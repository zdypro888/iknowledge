package engine

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"

	"github.com/zdypro888/iknowledge/internal/model"
)

const (
	semanticMaxDocumentRunes = 6000
	// 4KiB 保证最坏 6x JSON 转义下，32 条重建批次仍低于 provider 的
	// 1MiB request hard cap；知识摘要正常远小于该值。
	semanticMaxDocumentBytes    = 4 << 10
	semanticMaxRawDocumentBytes = 1 << 20
	semanticMaxSourceRecords    = 100_000
	// 重建会持有最终脱敏文档直到批量发送；独立限制文本工作集，避免
	// “向量 payload 合法”却因 10 万条超长摘要耗尽内存。
	semanticMaxSourceBytes = 64 << 20
)

const (
	semanticDocumentPreprocessVersion = "iknowledge-semantic-document-v2"
	semanticSourcePreprocessVersion   = "iknowledge-semantic-source-v2"
)

type semanticDocument struct {
	RecordID   string
	NodeID     string
	Kind       string
	Text       string
	SourceHash [32]byte
}

type semanticRawDocument struct {
	RecordID string
	NodeID   string
	Kind     string
	Raw      string
}

type semanticSourceRecord struct {
	NodeID     string
	SourceHash [32]byte
}

// semanticSourceManifest 是随健康 tree/project generation 发布的不可变视图。
// records 发布后只读；变更时整体替换，查询可在 rt.mu 外安全持有旧 map，最终
// merge 仍会在当前 version 下再次验证。
type semanticSourceManifest struct {
	version     uint64
	fingerprint [32]byte
	records     map[string]semanticSourceRecord
	errText     string
	ready       bool
}

// semanticRawDocumentsLocked 只从当前健康 index 抓取不可变 string headers。
// 前提：调用方持有 rt.mu；不做正则脱敏、rune 截断或哈希等重活。
func (e *Engine) semanticRawDocumentsLocked() ([]semanticRawDocument, error) {
	if e.rt.ix == nil {
		return nil, nil
	}
	ids := make([]string, 0, len(e.rt.ix.Nodes()))
	for id := range e.rt.ix.Nodes() {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	rawDocs := make([]semanticRawDocument, 0)
	totalRawBytes := 0
	appendDocument := func(doc semanticRawDocument) error {
		if len(rawDocs) >= semanticMaxSourceRecords {
			return fmt.Errorf("semantic source records 超过硬上限 %d", semanticMaxSourceRecords)
		}
		if len(doc.Raw) > semanticMaxRawDocumentBytes {
			return fmt.Errorf("semantic 原始摘要 %s 超过硬上限 %d MiB", doc.RecordID, semanticMaxRawDocumentBytes>>20)
		}
		if len(doc.Raw) > semanticMaxSourceBytes-totalRawBytes {
			return fmt.Errorf("semantic 原始摘要正文超过硬上限 %d MiB", semanticMaxSourceBytes>>20)
		}
		totalRawBytes += len(doc.Raw)
		rawDocs = append(rawDocs, doc)
		return nil
	}
	for _, nodeID := range ids {
		ref := e.rt.ix.Node(nodeID)
		if ref == nil || ref.Node == nil || ref.Node.Status == model.StatusOrphaned {
			continue
		}
		n := ref.Node
		for i := range n.Entries {
			entry := &n.Entries[i]
			if !entry.Active() || entry.Kind != model.KindSummary || strings.TrimSpace(entry.Text) == "" {
				continue
			}
			recordID := "summary:" + nodeID + "#" + entry.ID
			if err := appendDocument(semanticRawDocument{RecordID: recordID, NodeID: nodeID, Kind: model.KindSummary, Raw: entry.Text}); err != nil {
				return nil, err
			}
		}
		if strings.TrimSpace(n.EraSummary) != "" {
			if err := appendDocument(semanticRawDocument{RecordID: "era:" + nodeID, NodeID: nodeID, Kind: "era_summary", Raw: n.EraSummary}); err != nil {
				return nil, err
			}
		}
	}
	return rawDocs, nil
}

// semanticSourceSnapshot 在主锁外完成确定性预处理，并仅在 source version 未变
// 时发布 manifest。includeText 只供显式 rebuild；普通 recall 命中缓存后为 O(1)。
func (e *Engine) semanticSourceSnapshot(ctx context.Context, includeText bool) ([]semanticDocument, semanticSourceManifest, error) {
	if ctx == nil {
		return nil, semanticSourceManifest{}, fmt.Errorf("semantic source: nil context")
	}
	e.semantic.sourceMu.Lock()
	defer e.semantic.sourceMu.Unlock()
	for {
		if err := ctx.Err(); err != nil {
			return nil, semanticSourceManifest{}, err
		}
		e.rt.mu.RLock()
		version := e.rt.semanticSourceVersion
		cached := e.rt.semanticManifest
		if cached.ready && cached.version == version {
			if cached.errText != "" {
				e.rt.mu.RUnlock()
				return nil, cached, fmt.Errorf("%s", cached.errText)
			}
			if !includeText {
				e.rt.mu.RUnlock()
				return nil, cached, nil
			}
		}
		rawDocs, rawErr := e.semanticRawDocumentsLocked()
		e.rt.mu.RUnlock()

		docs, manifest, buildErr := buildSemanticSource(ctx, version, rawDocs, includeText, rawErr)
		e.rt.mu.Lock()
		if e.rt.semanticSourceVersion != version {
			e.rt.mu.Unlock()
			continue
		}
		e.rt.semanticManifest = manifest
		e.rt.mu.Unlock()
		if buildErr != nil {
			return nil, manifest, buildErr
		}
		return docs, manifest, nil
	}
}

func buildSemanticSource(ctx context.Context, version uint64, rawDocs []semanticRawDocument, includeText bool, sourceErr error) ([]semanticDocument, semanticSourceManifest, error) {
	manifest := semanticSourceManifest{version: version, ready: true}
	if sourceErr != nil {
		manifest.errText = sourceErr.Error()
		return nil, manifest, sourceErr
	}
	docs := make([]semanticDocument, 0, len(rawDocs))
	totalTextBytes := 0
	for i, raw := range rawDocs {
		if i&63 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, semanticSourceManifest{}, err
			}
		}
		doc := makeSemanticDocument(raw.RecordID, raw.NodeID, raw.Kind, raw.Raw)
		if len(doc.Text) > semanticMaxSourceBytes-totalTextBytes {
			err := fmt.Errorf("semantic 脱敏摘要正文超过硬上限 %d MiB", semanticMaxSourceBytes>>20)
			manifest.errText = err.Error()
			return nil, manifest, err
		}
		totalTextBytes += len(doc.Text)
		if !includeText {
			doc.Text = ""
		}
		docs = append(docs, doc)
	}
	// 再按 record ID 排序，让未来数据源扩展也不改变 fingerprint 语义。
	sort.Slice(docs, func(i, j int) bool { return docs[i].RecordID < docs[j].RecordID })
	h := sha256.New()
	_, _ = h.Write([]byte(semanticSourcePreprocessVersion + "\x00"))
	manifest.records = make(map[string]semanticSourceRecord, len(docs))
	for _, doc := range docs {
		writeSemanticHashField(h, doc.RecordID)
		writeSemanticHashField(h, doc.NodeID)
		writeSemanticHashField(h, doc.Kind)
		_, _ = h.Write(doc.SourceHash[:])
		manifest.records[doc.RecordID] = semanticSourceRecord{NodeID: doc.NodeID, SourceHash: doc.SourceHash}
	}
	copy(manifest.fingerprint[:], h.Sum(nil))
	return docs, manifest, nil
}

func writeSemanticHashField(h interface{ Write([]byte) (int, error) }, value string) {
	var size [8]byte
	binary.LittleEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = h.Write(size[:])
	_, _ = h.Write([]byte(value))
}

// semanticManifestLocked 只返回当前 source generation 的已发布 manifest。
// 前提：调用方持有 rt.mu；不能在这里触发全库构造。
func (e *Engine) semanticManifestLocked() (semanticSourceManifest, error) {
	manifest := e.rt.semanticManifest
	if !manifest.ready || manifest.version != e.rt.semanticSourceVersion {
		return semanticSourceManifest{}, fmt.Errorf("semantic source generation 已变化")
	}
	if manifest.errText != "" {
		return manifest, fmt.Errorf("%s", manifest.errText)
	}
	return manifest, nil
}

func makeSemanticDocument(recordID, nodeID, kind, raw string) semanticDocument {
	clean := compactSemanticText(raw, semanticMaxDocumentRunes)
	text := "代码知识节点: " + nodeID + "\n类型: " + kind + "\n摘要: " + clean
	// 对最终 provider 文本整体脱敏；node ID/文件名同样可能含 token、URL
	// credential 等模式，不能只处理 summary 正文。
	text, _ = RedactText(text)
	text = compactSemanticTextBytes(text, semanticMaxDocumentBytes)
	return semanticDocument{
		RecordID: recordID, NodeID: nodeID, Kind: kind, Text: text,
		SourceHash: sha256.Sum256([]byte(semanticDocumentPreprocessVersion + "\x00" + text)),
	}
}

func compactSemanticTextBytes(text string, maxBytes int) string {
	if maxBytes <= 0 || len(text) <= maxBytes {
		return text
	}
	runes := []rune(text)
	// 二分找到 UTF-8 编码不越界的最长 rune 前缀，避免切断多字节字符。
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		if len(string(runes[:mid])) <= maxBytes {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return strings.TrimSpace(string(runes[:lo]))
}

func compactSemanticText(text string, maxRunes int) string {
	text = strings.TrimSpace(text)
	runes := []rune(text)
	if maxRunes <= 0 || len(runes) <= maxRunes {
		return text
	}
	const marker = "\n[…摘要过长，保留首尾…]\n"
	budget := maxRunes - len([]rune(marker))
	if budget < 2 {
		return string(runes[:maxRunes])
	}
	tail := budget / 4
	head := budget - tail
	return strings.TrimSpace(string(runes[:head])) + marker + strings.TrimSpace(string(runes[len(runes)-tail:]))
}
