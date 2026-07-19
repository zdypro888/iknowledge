package engine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zdypro888/iknowledge/internal/semantic"
	"github.com/zdypro888/iknowledge/internal/vector"
)

const (
	semanticIndexRel       = "local/vector.idx"
	semanticIndexVersion   = uint16(1)
	semanticWrapperSize    = 48
	semanticMaxMetadata    = 64 << 10
	semanticEmbedBatchSize = 32
	semanticProbeTTL       = 10 * time.Minute
	semanticQueryCacheTTL  = 2 * time.Minute
	semanticQueryCacheCap  = 256
	semanticFailureBackoff = 3 * time.Second
	// Preview 默认每仓只允许一个在途 provider 请求。拿到 slot 后 query/probe
	// 会二次查 cache，因此相同 cache miss 不会排队放大费用；不同查询宁可
	// 串行，也不让远端费用或本机模型内存随并发请求增长。
	semanticProviderConcurrency = 1
	semanticProviderMaxResponse = 16 << 20
	semanticProbeText           = "iknowledge semantic model probe v1: code decisions pitfalls architecture"
)

var semanticIndexMagic = [8]byte{'I', 'K', 'S', 'E', 'M', 'I', 'D', 'X'}

type semanticIndexMetadata struct {
	Schema              int    `json:"schema"`
	Generation          string `json:"generation"`
	SettingsFingerprint string `json:"settings_fingerprint"`
	EmbedderFingerprint string `json:"embedder_fingerprint"`
	ProbeFingerprint    string `json:"probe_fingerprint"`
	SourceFingerprint   string `json:"source_fingerprint"`
	Dimensions          int    `json:"dimensions"`
	Records             int    `json:"records"`
	BuiltAt             string `json:"built_at"`
}

type semanticQueryCacheEntry struct {
	vector    []float32
	expiresAt time.Time
}

type semanticFileIdentity struct {
	Size             int64
	HeaderChecksum   [32]byte
	MetadataChecksum [32]byte
	VectorChecksum   [32]byte
}

// semanticRuntime 与主知识 runtime 分锁。snapshot 一经发布便不可变；搜索
// 复制指针后无锁运行。网络请求同样永不持有此锁。
type semanticRuntime struct {
	mu        sync.Mutex
	rebuildMu sync.Mutex
	// sourceMu 串行首次/变更后的语义源 manifest 构造。构造过程只在抓取
	// immutable string headers 时短持 rt.mu，脱敏与哈希均在主锁外。
	sourceMu sync.Mutex

	loadedKey  string
	loadedAt   time.Time
	loadedFile semanticFileIdentity
	snapshot   *vector.Snapshot
	metadata   semanticIndexMetadata
	loadErr    string
	lastError  string

	probeKey         string
	probeFingerprint string
	probeAt          time.Time
	failureUntil     time.Time

	queryCache   map[string]semanticQueryCacheEntry
	providerGate chan struct{}
}

// SemanticRebuildReport 是显式重建结果。重建不会由 serve/recall 自动触发。
type SemanticRebuildReport struct {
	Records       int
	Dimensions    int
	VectorBytes   uint64
	MetadataBytes uint64
	Model         string
	Endpoint      string
	Fingerprint   string
}

func (r SemanticRebuildReport) Text() string {
	return fmt.Sprintf("semantic 索引已重建: records=%d dimensions=%d vector=%.1fMiB metadata=%.1fKiB\nmodel=%s\nendpoint=%s\nfingerprint=%s",
		r.Records, r.Dimensions, float64(r.VectorBytes)/(1<<20), float64(r.MetadataBytes)/(1<<10),
		r.Model, r.Endpoint, r.Fingerprint)
}

func newSemanticEmbedder(cfg SemanticSettings) (semantic.Embedder, error) {
	apiKey := ""
	if !semanticEndpointLoopback(cfg.Endpoint) {
		apiKey = os.Getenv(SemanticAPIKeyEnv)
	}
	return semantic.NewOpenAICompatible(semantic.OpenAIConfig{
		BaseURL: cfg.Endpoint, Model: cfg.Model, Revision: cfg.Revision,
		APIKey: apiKey, Dimensions: cfg.Dimensions,
		Timeout:      time.Duration(cfg.TimeoutSec) * time.Second,
		MaxBatchSize: semanticEmbedBatchSize, MaxResponseBytes: semanticProviderMaxResponse,
	})
}

func (e *Engine) semanticGate() chan struct{} {
	e.semantic.mu.Lock()
	defer e.semantic.mu.Unlock()
	if e.semantic.providerGate == nil {
		e.semantic.providerGate = make(chan struct{}, semanticProviderConcurrency)
	}
	return e.semantic.providerGate
}

func (e *Engine) embedSemanticDocuments(ctx context.Context, embedder semantic.Embedder, texts []string) ([][]float32, error) {
	gate := e.semanticGate()
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return embedder.EmbedDocuments(ctx, texts)
}

// RebuildSemantic 显式生成完整不可变 generation。它只读取知识正本，
// embedding 与文件写入均在主 runtime 锁外；提交前重新核对源 fingerprint。
func (e *Engine) RebuildSemantic(ctx context.Context) (SemanticRebuildReport, error) {
	if ctx == nil {
		return SemanticRebuildReport{}, fmt.Errorf("semantic rebuild: nil context")
	}
	if err := e.requireInit(); err != nil {
		return SemanticRebuildReport{}, err
	}
	e.semantic.rebuildMu.Lock()
	defer e.semantic.rebuildMu.Unlock()
	if err := ctx.Err(); err != nil {
		return SemanticRebuildReport{}, err
	}
	releaseSemantic, err := e.Store.AcquireSemanticLock()
	if err != nil {
		return SemanticRebuildReport{}, fmt.Errorf("semantic rebuild: %w", err)
	}
	defer releaseSemantic()
	cfg, err := LoadSemanticSettings(e.Store)
	if err != nil {
		return SemanticRebuildReport{}, err
	}
	if !cfg.Enabled {
		return SemanticRebuildReport{}, fmt.Errorf("semantic 未启用；先运行 iknowledge semantic configure")
	}
	embedder, err := newSemanticEmbedder(cfg)
	if err != nil {
		return SemanticRebuildReport{}, err
	}
	if err := e.Sync(); err != nil {
		return SemanticRebuildReport{}, err
	}
	docs, sourceManifest, sourceErr := e.semanticSourceSnapshot(ctx, true)
	if sourceErr != nil {
		return SemanticRebuildReport{}, sourceErr
	}
	sourceFingerprint := sourceManifest.fingerprint

	probe, err := e.embedSemanticDocuments(ctx, embedder, []string{semanticProbeText})
	if err != nil {
		return SemanticRebuildReport{}, fmt.Errorf("semantic 模型探测失败: %w", err)
	}
	if len(probe) != 1 {
		return SemanticRebuildReport{}, fmt.Errorf("semantic 模型探测返回 %d 条，期望 1", len(probe))
	}
	if err := validateSemanticVector(probe[0], cfg.Dimensions); err != nil {
		return SemanticRebuildReport{}, fmt.Errorf("semantic 模型探测: %w", err)
	}
	dimensions := len(probe[0])
	probeFingerprint := semanticVectorFingerprint(probe[0])

	// 先对 dimensions、metadata、record count 和最终矩阵字节数做完整预检。
	// 除固定单条 probe 外，任何批量/付费文档请求都必须发生在预检之后。
	records := make([]vector.Record, len(docs))
	for i, doc := range docs {
		records[i] = vector.Record{
			ID: doc.RecordID, NodeID: doc.NodeID, Kind: doc.Kind, SourceHash: doc.SourceHash,
		}
	}
	plan, err := vector.Preflight(ctx, dimensions, records, semanticVectorLimits(cfg))
	if err != nil {
		return SemanticRebuildReport{}, err
	}
	builder, err := vector.NewBuilder(ctx, plan)
	if err != nil {
		return SemanticRebuildReport{}, err
	}
	// Builder 已复制紧凑 metadata，尽早释放预检输入。
	records = nil
	for start := 0; start < len(docs); start += semanticEmbedBatchSize {
		end := min(start+semanticEmbedBatchSize, len(docs))
		texts := make([]string, end-start)
		for i := range texts {
			texts[i] = docs[start+i].Text
		}
		vectors, err := e.embedSemanticDocuments(ctx, embedder, texts)
		if err != nil {
			return SemanticRebuildReport{}, fmt.Errorf("semantic embedding 批次 %d..%d: %w", start, end, err)
		}
		if len(vectors) != len(texts) {
			return SemanticRebuildReport{}, fmt.Errorf("semantic embedding 批次返回 %d 条，期望 %d", len(vectors), len(texts))
		}
		for i, values := range vectors {
			if err := validateSemanticVector(values, dimensions); err != nil {
				return SemanticRebuildReport{}, fmt.Errorf("semantic record %s: %w", docs[start+i].RecordID, err)
			}
		}
		if err := builder.Append(ctx, vectors); err != nil {
			return SemanticRebuildReport{}, fmt.Errorf("semantic embedding 批次 %d..%d 写入矩阵: %w", start, end, err)
		}
	}
	snapshot, err := builder.Finish(ctx)
	if err != nil {
		return SemanticRebuildReport{}, err
	}
	docs = nil

	// embedding 期间若知识发生变化，晚到任务不得发布旧 generation。
	if err := e.validateSemanticRebuildCurrent(ctx, cfg, sourceFingerprint); err != nil {
		return SemanticRebuildReport{}, err
	}
	status := snapshot.Status()
	generation, err := newSemanticGeneration()
	if err != nil {
		return SemanticRebuildReport{}, err
	}
	meta := semanticIndexMetadata{
		Schema: 1, Generation: generation, SettingsFingerprint: SemanticSettingsFingerprint(cfg),
		EmbedderFingerprint: embedder.Fingerprint(), ProbeFingerprint: probeFingerprint,
		SourceFingerprint: hex.EncodeToString(sourceFingerprint[:]),
		Dimensions:        dimensions, Records: status.Records, BuiltAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := e.Store.WritePrivateKnowledgeFileStreamChecked(semanticIndexRel, func(w io.Writer) error {
		return encodeSemanticIndexContext(ctx, w, meta, snapshot)
	}, func() error {
		// 大文件编码/fsync 期间也可能变代；在 rename 旧索引之前再校验，
		// 失败会丢弃 temp 并完整保留上一代文件。
		return e.validateSemanticRebuildCurrent(ctx, cfg, sourceFingerprint)
	}); err != nil {
		return SemanticRebuildReport{}, err
	}
	if err := e.validateSemanticRebuildCurrent(ctx, cfg, sourceFingerprint); err != nil {
		return SemanticRebuildReport{}, err
	}
	f, err := e.Store.OpenKnowledgeFileRead(semanticIndexRel)
	if err != nil {
		return SemanticRebuildReport{}, err
	}
	fileIdentity, identityErr := readSemanticFileIdentity(ctx, f)
	closeErr := f.Close()
	if identityErr != nil || closeErr != nil {
		return SemanticRebuildReport{}, errors.Join(identityErr, closeErr)
	}
	_, expectedMetadataChecksum, err := marshalSemanticIndexMetadata(meta)
	if err != nil {
		return SemanticRebuildReport{}, err
	}
	if fileIdentity.MetadataChecksum != expectedMetadataChecksum {
		return SemanticRebuildReport{}, fmt.Errorf("semantic 索引刚写入即被另一重建代替；保留磁盘胜者并放弃本进程旧快照")
	}

	key := semanticLoadedKey(cfg, sourceFingerprint)
	e.semantic.mu.Lock()
	e.semantic.loadedKey, e.semantic.loadedAt, e.semantic.loadedFile = key, time.Now(), fileIdentity
	e.semantic.snapshot, e.semantic.metadata = snapshot, meta
	e.semantic.loadErr, e.semantic.lastError = "", ""
	e.semantic.probeKey, e.semantic.probeFingerprint, e.semantic.probeAt =
		embedder.Fingerprint(), probeFingerprint, time.Now()
	e.semantic.queryCache = nil // 同名 provider 的实际模型可能已重建/漂移，旧 query vector 不得跨代复用。
	e.semantic.failureUntil = time.Time{}
	e.semantic.mu.Unlock()
	return SemanticRebuildReport{
		Records: status.Records, Dimensions: status.Dimensions,
		VectorBytes: status.VectorBytes, MetadataBytes: status.MetadataBytes,
		Model: cfg.Model, Endpoint: cfg.Endpoint, Fingerprint: meta.SettingsFingerprint,
	}, nil
}

func (e *Engine) validateSemanticRebuildCurrent(ctx context.Context, cfg SemanticSettings, source [32]byte) error {
	if err := e.Sync(); err != nil {
		return err
	}
	_, manifest, err := e.semanticSourceSnapshot(ctx, false)
	if err != nil {
		return err
	}
	if manifest.fingerprint != source {
		return fmt.Errorf("semantic 重建期间知识摘要发生变化，已放弃旧结果；请重试")
	}
	currentCfg, err := LoadSemanticSettings(e.Store)
	if err != nil {
		return err
	}
	if currentCfg != cfg {
		return fmt.Errorf("semantic 重建期间 provider 配置发生变化，已放弃旧结果；请重试")
	}
	return nil
}

func semanticVectorLimits(cfg SemanticSettings) vector.Limits {
	limits := vector.DefaultLimits()
	limits.MaxVectorBytes = uint64(cfg.MaxVectorMiB) << 20
	return limits
}

func validateSemanticVector(values []float32, expected int) error {
	if len(values) == 0 {
		return fmt.Errorf("向量为空")
	}
	if expected > 0 && len(values) != expected {
		return fmt.Errorf("维度=%d，期望 %d", len(values), expected)
	}
	if len(values) > int(vector.DefaultLimits().MaxDimensions) {
		return fmt.Errorf("维度=%d 超过上限", len(values))
	}
	var norm float64
	for i, value := range values {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return fmt.Errorf("第 %d 维不是有限数", i)
		}
		norm += float64(value) * float64(value)
	}
	if norm == 0 {
		return fmt.Errorf("零向量")
	}
	return nil
}

func semanticVectorFingerprint(values []float32) string {
	h := sha256.New()
	var encoded [4]byte
	for _, value := range values {
		quantized := int32(math.Round(float64(value) * 10_000))
		binary.LittleEndian.PutUint32(encoded[:], uint32(quantized))
		_, _ = h.Write(encoded[:])
	}
	return hex.EncodeToString(h.Sum(nil)[:12])
}

func semanticLoadedKey(cfg SemanticSettings, source [32]byte) string {
	// max_vector_mib 是 runtime resident policy，不改变向量空间/持久 metadata，
	// 但必须进入 load key；用户收紧上限后 live serve 不能继续持有旧大快照。
	return SemanticSettingsFingerprint(cfg) + ":" + hex.EncodeToString(source[:]) + fmt.Sprintf(":max=%d", cfg.MaxVectorMiB)
}

func (e *Engine) ensureSemanticSnapshot(ctx context.Context, cfg SemanticSettings, source [32]byte) (*vector.Snapshot, semanticIndexMetadata, error) {
	if ctx == nil {
		return nil, semanticIndexMetadata{}, fmt.Errorf("semantic snapshot: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, semanticIndexMetadata{}, err
	}
	key := semanticLoadedKey(cfg, source)
	now := time.Now()
	e.semantic.mu.Lock()
	defer e.semantic.mu.Unlock()
	fileChanged := false
	if e.semantic.loadedKey == key {
		if e.semantic.snapshot != nil {
			// `semantic rebuild/clear` 常由另一个 CLI 进程执行；用 wrapper metadata
			// checksum + vector checksum 比较真实文件 generation，不能只看“存在”。
			f, openErr := e.Store.OpenKnowledgeFileRead(semanticIndexRel)
			if openErr == nil {
				identity, identityErr := readSemanticFileIdentity(ctx, f)
				closeErr := f.Close()
				openErr = errors.Join(identityErr, closeErr)
				if openErr == nil && identity == e.semantic.loadedFile {
					return e.semantic.snapshot, e.semantic.metadata, nil
				}
			}
			if openErr != nil && os.IsNotExist(openErr) {
				openErr = fmt.Errorf("semantic 索引不存在；运行 iknowledge semantic rebuild")
			}
			// 某个调用者取消只结束它自己的请求，不能破坏其他请求仍可使用的
			// 共享 immutable snapshot，也不能把取消写成全局 loadErr。
			if errors.Is(openErr, context.Canceled) || errors.Is(openErr, context.DeadlineExceeded) {
				return nil, semanticIndexMetadata{}, openErr
			}
			// identity 变化时继续走完整有界 decode；打开/identity 失败则按
			// 普通 load error 降级，绝不继续使用内存旧代。
			e.semantic.snapshot, e.semantic.metadata = nil, semanticIndexMetadata{}
			e.semantic.loadedFile = semanticFileIdentity{}
			if openErr != nil {
				e.semantic.loadErr, e.semantic.lastError = openErr.Error(), openErr.Error()
				return nil, semanticIndexMetadata{}, openErr
			}
			fileChanged = true
		}
		// 外部 `semantic rebuild` 可能刚以原子 rename 写好同一 key 的文件。
		// 短暂负缓存防止每次 recall 都解码，随后自动重试，无需重启 serve。
		if !fileChanged && now.Sub(e.semantic.loadedAt) < semanticFailureBackoff {
			if e.semantic.loadErr == "" {
				return nil, semanticIndexMetadata{}, fmt.Errorf("semantic 索引暂不可用")
			}
			return nil, semanticIndexMetadata{}, errors.New(e.semantic.loadErr)
		}
	}

	snapshot, meta, identity, err := e.loadSemanticIndex(ctx, cfg, source)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil, semanticIndexMetadata{}, err
	}
	e.semantic.loadedKey, e.semantic.loadedAt = key, now
	e.semantic.loadedFile = identity
	e.semantic.snapshot, e.semantic.metadata = snapshot, meta
	if err != nil {
		e.semantic.snapshot = nil
		e.semantic.loadedFile = semanticFileIdentity{}
		e.semantic.loadErr, e.semantic.lastError = err.Error(), err.Error()
		return nil, semanticIndexMetadata{}, err
	}
	e.semantic.loadErr = ""
	// 磁盘 generation 可能由外部 CLI 针对同名、已漂移模型重建；旧 probe
	// 与 query cache 都属于上一代，下一次 recall 必须重新探测/嵌入。
	e.semantic.probeKey, e.semantic.probeFingerprint, e.semantic.probeAt = "", "", time.Time{}
	e.semantic.queryCache = nil
	return snapshot, meta, nil
}

func (e *Engine) loadSemanticIndex(ctx context.Context, cfg SemanticSettings, source [32]byte) (*vector.Snapshot, semanticIndexMetadata, semanticFileIdentity, error) {
	f, err := e.Store.OpenKnowledgeFileRead(semanticIndexRel)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, semanticIndexMetadata{}, semanticFileIdentity{}, fmt.Errorf("semantic 索引不存在；运行 iknowledge semantic rebuild")
		}
		return nil, semanticIndexMetadata{}, semanticFileIdentity{}, err
	}
	defer f.Close()
	identity, err := readSemanticFileIdentity(ctx, f)
	if err != nil {
		return nil, semanticIndexMetadata{}, semanticFileIdentity{}, err
	}
	maxFile := semanticMaxIndexFileSize(cfg)
	if identity.Size < semanticWrapperSize || identity.Size > maxFile {
		return nil, semanticIndexMetadata{}, semanticFileIdentity{}, fmt.Errorf("semantic 索引大小 %d 超出允许范围", identity.Size)
	}
	// Wrapper metadata 已足以判断 settings/embedder/source stale；必须先绑定
	// 校验，再为仍有效的 generation 分配/扫描最高 512MiB vector payload。
	meta, err := decodeSemanticIndexMetadataContext(ctx, f)
	if err != nil {
		return nil, semanticIndexMetadata{}, semanticFileIdentity{}, err
	}
	if err := validateSemanticIndexBinding(meta, cfg, source); err != nil {
		return nil, semanticIndexMetadata{}, semanticFileIdentity{}, err
	}
	snapshot, err := vector.DecodeWithLimitsContext(ctx, f, semanticVectorLimits(cfg))
	if err != nil {
		return nil, semanticIndexMetadata{}, semanticFileIdentity{}, err
	}
	switch {
	case meta.Dimensions != snapshot.Status().Dimensions || meta.Records != snapshot.Status().Records:
		return nil, semanticIndexMetadata{}, semanticFileIdentity{}, fmt.Errorf("semantic 索引 metadata 与 payload 不一致")
	}
	return snapshot, meta, identity, nil
}

func validateSemanticIndexBinding(meta semanticIndexMetadata, cfg SemanticSettings, source [32]byte) error {
	embedder, err := newSemanticEmbedder(cfg)
	if err != nil {
		return err
	}
	wantSource := hex.EncodeToString(source[:])
	switch {
	case meta.SettingsFingerprint != SemanticSettingsFingerprint(cfg):
		return fmt.Errorf("semantic provider 配置已变化，索引 stale；请 rebuild")
	case meta.EmbedderFingerprint != embedder.Fingerprint():
		return fmt.Errorf("semantic embedder 已变化，索引 stale；请 rebuild")
	case meta.SourceFingerprint != wantSource:
		return fmt.Errorf("知识摘要已变化，semantic 索引 stale；请 rebuild")
	case cfg.Dimensions > 0 && meta.Dimensions != cfg.Dimensions:
		return fmt.Errorf("semantic 索引维度=%d，配置要求 %d", meta.Dimensions, cfg.Dimensions)
	}
	return nil
}

func semanticMaxIndexFileSize(cfg SemanticSettings) int64 {
	limits := vector.DefaultLimits()
	return (int64(cfg.MaxVectorMiB) << 20) + int64(limits.MaxMetadataBytes) +
		semanticMaxMetadata + semanticWrapperSize + int64(vector.EncodedFixedOverhead)
}

// semanticCandidates 返回摘要级候选。任何 provider/缓存错误都由调用方降级，
// 不得让关键词与结构检索失败。
func (e *Engine) semanticCandidates(ctx context.Context, query string) ([]vector.Hit, string) {
	cfg, err := LoadSemanticSettings(e.Store)
	if err != nil {
		return nil, err.Error()
	}
	if !cfg.Enabled {
		return nil, ""
	}
	query, _ = RedactText(strings.TrimSpace(query))
	if query == "" {
		return nil, ""
	}
	_, sourceManifest, sourceErr := e.semanticSourceSnapshot(ctx, false)
	if sourceErr != nil {
		return nil, sourceErr.Error()
	}
	sourceFingerprint := sourceManifest.fingerprint
	snapshot, meta, err := e.ensureSemanticSnapshot(ctx, cfg, sourceFingerprint)
	if err != nil {
		return nil, err.Error()
	}
	embedder, err := newSemanticEmbedder(cfg)
	if err != nil {
		return nil, err.Error()
	}
	probeFingerprint, err := e.currentSemanticProbe(ctx, embedder)
	if err != nil {
		return nil, err.Error()
	}
	if probeFingerprint != meta.ProbeFingerprint {
		err := fmt.Errorf("embedding 服务实际模型已变化，semantic 索引 stale；请 rebuild")
		e.setSemanticError(err)
		return nil, err.Error()
	}

	queryVector, err := e.cachedSemanticQuery(ctx, embedder, query)
	if err != nil {
		e.setSemanticError(err)
		return nil, err.Error()
	}
	if len(queryVector) != meta.Dimensions {
		err := fmt.Errorf("semantic 查询维度=%d，索引维度=%d", len(queryVector), meta.Dimensions)
		e.setSemanticError(err)
		return nil, err.Error()
	}
	latestSnapshot, latestMeta, err := e.ensureSemanticSnapshot(ctx, cfg, sourceFingerprint)
	if err != nil {
		return nil, err.Error()
	}
	if latestMeta.Generation != meta.Generation {
		return nil, "semantic 索引在查询期间被替换，已丢弃旧候选；下次查询使用新 generation"
	}
	snapshot = latestSnapshot
	hits, err := snapshot.Search(ctx, queryVector, cfg.TopK)
	if err != nil {
		e.setSemanticError(err)
		return nil, err.Error()
	}
	_, afterSearchMeta, err := e.ensureSemanticSnapshot(ctx, cfg, sourceFingerprint)
	if err != nil {
		return nil, err.Error()
	}
	if afterSearchMeta.Generation != meta.Generation {
		return nil, "semantic 索引在搜索期间被替换，已丢弃旧候选；下次查询使用新 generation"
	}

	// provider 调用后再次 Sync，捕捉进程内写入以及外部 checkout/编辑；manifest
	// 只在 tree/project generation 变化时重建，稳态 recall 不再扫描全库。
	if err := e.Sync(); err != nil {
		return nil, err.Error()
	}
	_, currentManifest, sourceErr := e.semanticSourceSnapshot(ctx, false)
	if sourceErr != nil {
		return nil, sourceErr.Error()
	}
	if currentManifest.fingerprint != sourceFingerprint {
		return nil, "知识在 semantic 查询期间发生变化，已丢弃旧候选"
	}
	filtered := make([]vector.Hit, 0, len(hits))
	for _, hit := range hits {
		record, ok := currentManifest.records[hit.ID]
		if float64(hit.Score) < cfg.MinScore || !ok || record.NodeID != hit.NodeID || record.SourceHash != hit.SourceHash {
			continue
		}
		filtered = append(filtered, hit)
	}
	e.semantic.mu.Lock()
	e.semantic.lastError = ""
	e.semantic.mu.Unlock()
	return filtered, ""
}

func (e *Engine) currentSemanticProbe(ctx context.Context, embedder semantic.Embedder) (string, error) {
	now := time.Now()
	e.semantic.mu.Lock()
	if now.Before(e.semantic.failureUntil) && e.semantic.lastError != "" {
		err := errors.New(e.semantic.lastError)
		e.semantic.mu.Unlock()
		return "", err
	}
	if e.semantic.probeKey == embedder.Fingerprint() && now.Sub(e.semantic.probeAt) < semanticProbeTTL {
		value := e.semantic.probeFingerprint
		e.semantic.mu.Unlock()
		return value, nil
	}
	e.semantic.mu.Unlock()
	gate := e.semanticGate()
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	// 排队期间另一请求可能已完成 probe；二次检查封住 thundering herd，
	// 避免同一模型的并发 cache miss 逐个产生付费请求。
	now = time.Now()
	e.semantic.mu.Lock()
	if now.Before(e.semantic.failureUntil) && e.semantic.lastError != "" {
		err := errors.New(e.semantic.lastError)
		e.semantic.mu.Unlock()
		return "", err
	}
	if e.semantic.probeKey == embedder.Fingerprint() && now.Sub(e.semantic.probeAt) < semanticProbeTTL {
		value := e.semantic.probeFingerprint
		e.semantic.mu.Unlock()
		return value, nil
	}
	e.semantic.mu.Unlock()
	vectors, err := embedder.EmbedDocuments(ctx, []string{semanticProbeText})
	if err != nil || len(vectors) != 1 {
		if err == nil {
			err = fmt.Errorf("semantic 模型探测返回 %d 条，期望 1", len(vectors))
		}
		e.setSemanticError(err)
		return "", err
	}
	if err := validateSemanticVector(vectors[0], 0); err != nil {
		e.setSemanticError(err)
		return "", err
	}
	fingerprint := semanticVectorFingerprint(vectors[0])
	e.semantic.mu.Lock()
	e.semantic.probeKey, e.semantic.probeFingerprint, e.semantic.probeAt =
		embedder.Fingerprint(), fingerprint, now
	e.semantic.failureUntil, e.semantic.lastError = time.Time{}, ""
	e.semantic.mu.Unlock()
	return fingerprint, nil
}

func (e *Engine) cachedSemanticQuery(ctx context.Context, embedder semantic.Embedder, query string) ([]float32, error) {
	key := embedder.Fingerprint() + "\x00" + query
	now := time.Now()
	e.semantic.mu.Lock()
	if entry, ok := e.semantic.queryCache[key]; ok && now.Before(entry.expiresAt) {
		vectorCopy := append([]float32(nil), entry.vector...)
		e.semantic.mu.Unlock()
		return vectorCopy, nil
	}
	e.semantic.mu.Unlock()
	gate := e.semanticGate()
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// 与 probe 同理：拿到有界 provider slot 后再看一次 cache。相同 query
	// 的排队请求复用先完成者结果，不会串行放大费用。
	now = time.Now()
	e.semantic.mu.Lock()
	if now.Before(e.semantic.failureUntil) && e.semantic.lastError != "" {
		err := errors.New(e.semantic.lastError)
		e.semantic.mu.Unlock()
		return nil, err
	}
	if entry, ok := e.semantic.queryCache[key]; ok && now.Before(entry.expiresAt) {
		vectorCopy := append([]float32(nil), entry.vector...)
		e.semantic.mu.Unlock()
		return vectorCopy, nil
	}
	e.semantic.mu.Unlock()
	values, err := embedder.EmbedQuery(ctx, query)
	if err != nil {
		e.setSemanticError(err)
		return nil, err
	}
	if err := validateSemanticVector(values, 0); err != nil {
		e.setSemanticError(err)
		return nil, err
	}
	e.semantic.mu.Lock()
	if e.semantic.queryCache == nil {
		e.semantic.queryCache = make(map[string]semanticQueryCacheEntry)
	}
	if len(e.semantic.queryCache) >= semanticQueryCacheCap {
		keys := make([]string, 0, len(e.semantic.queryCache))
		for existing, entry := range e.semantic.queryCache {
			if now.After(entry.expiresAt) {
				delete(e.semantic.queryCache, existing)
			} else {
				keys = append(keys, existing)
			}
		}
		if len(e.semantic.queryCache) >= semanticQueryCacheCap && len(keys) > 0 {
			sort.Strings(keys)
			delete(e.semantic.queryCache, keys[0]) // 确定性有界退化，无后台 goroutine。
		}
	}
	e.semantic.queryCache[key] = semanticQueryCacheEntry{
		vector: append([]float32(nil), values...), expiresAt: now.Add(semanticQueryCacheTTL),
	}
	e.semantic.mu.Unlock()
	return values, nil
}

func (e *Engine) setSemanticError(err error) {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	e.semantic.mu.Lock()
	e.semantic.lastError = err.Error()
	e.semantic.failureUntil = time.Now().Add(semanticFailureBackoff)
	e.semantic.mu.Unlock()
}

// ClearSemanticIndex 删除可重建的本机派生缓存，不改变 provider 配置或 enabled。
func (e *Engine) ClearSemanticIndex() error {
	releaseSemantic, err := e.Store.AcquireSemanticLock()
	if err != nil {
		return fmt.Errorf("semantic clear: %w", err)
	}
	defer releaseSemantic()
	if err := e.Store.RemoveKnowledgeFile(semanticIndexRel); err != nil && !os.IsNotExist(err) {
		return err
	}
	e.semantic.mu.Lock()
	e.semantic.loadedKey, e.semantic.loadErr = "", ""
	e.semantic.loadedAt, e.semantic.loadedFile = time.Time{}, semanticFileIdentity{}
	e.semantic.snapshot, e.semantic.metadata = nil, semanticIndexMetadata{}
	e.semantic.probeKey, e.semantic.probeFingerprint, e.semantic.probeAt = "", "", time.Time{}
	e.semantic.queryCache = nil
	e.semantic.failureUntil, e.semantic.lastError = time.Time{}, ""
	e.semantic.mu.Unlock()
	return nil
}

// SemanticStatusText 返回纯本地状态，不探测 provider、不产生网络请求或费用。
func (e *Engine) SemanticStatusText() (string, error) {
	cfg, err := LoadSemanticSettings(e.Store)
	if err != nil {
		return "", err
	}
	path, _ := e.Store.SemanticConfigFile()
	var b strings.Builder
	fmt.Fprintf(&b, "semantic: %s\nconfig: %s\n", map[bool]string{true: "enabled", false: "disabled"}[cfg.Enabled], path)
	if cfg.Endpoint != "" {
		fmt.Fprintf(&b, "endpoint: %s\nmodel: %s\ndimensions: %d(auto=0)\n", cfg.Endpoint, cfg.Model, cfg.Dimensions)
	}
	if !cfg.Enabled || !e.Store.Initialized() {
		return strings.TrimRight(b.String(), "\n"), nil
	}
	if err := e.Sync(); err != nil {
		return "", err
	}
	_, sourceManifest, sourceErr := e.semanticSourceSnapshot(context.Background(), false)
	if sourceErr != nil {
		fmt.Fprintf(&b, "index: stale/unavailable (%v)\n", sourceErr)
		return strings.TrimRight(b.String(), "\n"), nil
	}
	inspection, loadErr := e.inspectSemanticIndex(context.Background(), cfg, sourceManifest.fingerprint)
	if loadErr != nil {
		e.semantic.mu.Lock()
		// status 发现外部 clear/corruption 后立即淘汰本进程旧代，但绝不为
		// 展示状态解码并常驻最大 512MiB vector payload。
		e.semantic.loadedKey, e.semantic.loadErr = "", ""
		e.semantic.loadedAt, e.semantic.loadedFile = time.Time{}, semanticFileIdentity{}
		e.semantic.snapshot, e.semantic.metadata = nil, semanticIndexMetadata{}
		e.semantic.probeKey, e.semantic.probeFingerprint, e.semantic.probeAt = "", "", time.Time{}
		e.semantic.queryCache = nil
		e.semantic.failureUntil = time.Time{}
		e.semantic.mu.Unlock()
		fmt.Fprintf(&b, "index: stale/unavailable (%v)\n", loadErr)
	} else {
		key := semanticLoadedKey(cfg, sourceManifest.fingerprint)
		e.semantic.mu.Lock()
		loaded := e.semantic.loadedKey == key && e.semantic.snapshot != nil && e.semantic.loadedFile == inspection.identity
		var status vector.Status
		if loaded {
			status = e.semantic.snapshot.Status()
		} else {
			// 外部 generation 已替换：记录轻量 metadata 并淘汰旧矩阵；真正
			// payload checksum/decode 延迟到下一次需要 semantic 的 recall。
			e.semantic.loadedKey, e.semantic.loadedAt = "", time.Time{}
			e.semantic.loadedFile = semanticFileIdentity{}
			e.semantic.snapshot, e.semantic.metadata = nil, inspection.meta
			e.semantic.probeKey, e.semantic.probeFingerprint, e.semantic.probeAt = "", "", time.Time{}
			e.semantic.queryCache = nil
			e.semantic.failureUntil, e.semantic.lastError = time.Time{}, ""
		}
		e.semantic.mu.Unlock()
		if loaded {
			fmt.Fprintf(&b, "index: ready\nrecords: %d\nindex_dimensions: %d\nvector_bytes: %d\nbuilt_at: %s\n",
				status.Records, status.Dimensions, status.VectorBytes, inspection.meta.BuiltAt)
		} else {
			fmt.Fprintf(&b, "index: metadata-valid (payload validation deferred to recall)\nrecords: %d\nindex_dimensions: %d\nvector_bytes: %d\nbuilt_at: %s\n",
				inspection.meta.Records, inspection.meta.Dimensions, inspection.vectorBytes, inspection.meta.BuiltAt)
		}
	}
	e.semantic.mu.Lock()
	lastErr := e.semantic.lastError
	e.semantic.mu.Unlock()
	if lastErr != "" {
		fmt.Fprintf(&b, "last_error: %s\n", lastErr)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

type semanticIndexInspection struct {
	meta        semanticIndexMetadata
	identity    semanticFileIdentity
	vectorBytes uint64
}

func (e *Engine) inspectSemanticIndex(ctx context.Context, cfg SemanticSettings, source [32]byte) (semanticIndexInspection, error) {
	f, err := e.Store.OpenKnowledgeFileRead(semanticIndexRel)
	if err != nil {
		if os.IsNotExist(err) {
			return semanticIndexInspection{}, fmt.Errorf("semantic 索引不存在；运行 iknowledge semantic rebuild")
		}
		return semanticIndexInspection{}, err
	}
	defer f.Close()
	identity, err := readSemanticFileIdentity(ctx, f)
	if err != nil {
		return semanticIndexInspection{}, err
	}
	maxFile := semanticMaxIndexFileSize(cfg)
	if identity.Size < semanticWrapperSize || identity.Size > maxFile {
		return semanticIndexInspection{}, fmt.Errorf("semantic 索引大小 %d 超出允许范围", identity.Size)
	}
	meta, err := decodeSemanticIndexMetadataContext(ctx, f)
	if err != nil {
		return semanticIndexInspection{}, err
	}
	if err := validateSemanticIndexBinding(meta, cfg, source); err != nil {
		return semanticIndexInspection{}, err
	}
	limits := semanticVectorLimits(cfg)
	records, dimensions := uint64(meta.Records), uint64(meta.Dimensions)
	if records > limits.MaxRecords || dimensions > uint64(limits.MaxDimensions) ||
		(dimensions > 0 && records > ^uint64(0)/(dimensions*4)) {
		return semanticIndexInspection{}, fmt.Errorf("semantic metadata 资源声明越界")
	}
	vectorBytes := records * dimensions * 4
	if vectorBytes > limits.MaxVectorBytes {
		return semanticIndexInspection{}, fmt.Errorf("semantic vector payload 声明 %d 超过上限 %d", vectorBytes, limits.MaxVectorBytes)
	}
	return semanticIndexInspection{meta: meta, identity: identity, vectorBytes: vectorBytes}, nil
}

func newSemanticGeneration() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("semantic generation entropy: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func marshalSemanticIndexMetadata(meta semanticIndexMetadata) ([]byte, [32]byte, error) {
	metadata, err := json.Marshal(meta)
	if err != nil {
		return nil, [32]byte{}, err
	}
	if len(metadata) == 0 || len(metadata) > semanticMaxMetadata {
		return nil, [32]byte{}, fmt.Errorf("semantic metadata 大小=%d 越界", len(metadata))
	}
	return metadata, sha256.Sum256(metadata), nil
}

// readSemanticFileIdentity 只读取固定 wrapper、≤64KiB metadata 与末尾 vector
// checksum，足以识别外部 CLI 的原子 generation 替换，而无需每次 recall 扫描
// 整个向量 payload。
func readSemanticFileIdentity(ctx context.Context, f *os.File) (semanticFileIdentity, error) {
	if ctx == nil {
		return semanticFileIdentity{}, fmt.Errorf("semantic identity: nil context")
	}
	if err := ctx.Err(); err != nil {
		return semanticFileIdentity{}, err
	}
	info, err := f.Stat()
	if err != nil {
		return semanticFileIdentity{}, err
	}
	if info.Size() < semanticWrapperSize+32 {
		return semanticFileIdentity{}, fmt.Errorf("semantic 索引过短: %d", info.Size())
	}
	var header [semanticWrapperSize]byte
	if _, err := f.ReadAt(header[:], 0); err != nil {
		return semanticFileIdentity{}, fmt.Errorf("semantic identity header: %w", err)
	}
	if subtle.ConstantTimeCompare(header[:8], semanticIndexMagic[:]) != 1 ||
		binary.LittleEndian.Uint16(header[8:10]) != semanticIndexVersion ||
		binary.LittleEndian.Uint16(header[10:12]) != 0 {
		return semanticFileIdentity{}, fmt.Errorf("semantic identity header 非法")
	}
	metadataLen := binary.LittleEndian.Uint32(header[12:16])
	if metadataLen == 0 || metadataLen > semanticMaxMetadata ||
		int64(semanticWrapperSize)+int64(metadataLen)+32 > info.Size() {
		return semanticFileIdentity{}, fmt.Errorf("semantic identity metadata 长度=%d 非法", metadataLen)
	}
	metadata := make([]byte, int(metadataLen))
	if _, err := f.ReadAt(metadata, semanticWrapperSize); err != nil {
		return semanticFileIdentity{}, fmt.Errorf("semantic identity metadata: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return semanticFileIdentity{}, err
	}
	metadataChecksum := sha256.Sum256(metadata)
	if subtle.ConstantTimeCompare(header[16:48], metadataChecksum[:]) != 1 {
		return semanticFileIdentity{}, fmt.Errorf("semantic metadata checksum 不匹配")
	}
	var vectorChecksum [32]byte
	if _, err := f.ReadAt(vectorChecksum[:], info.Size()-int64(len(vectorChecksum))); err != nil {
		return semanticFileIdentity{}, fmt.Errorf("semantic identity vector checksum: %w", err)
	}
	return semanticFileIdentity{
		Size: info.Size(), HeaderChecksum: sha256.Sum256(header[:]),
		MetadataChecksum: metadataChecksum, VectorChecksum: vectorChecksum,
	}, nil
}

func encodeSemanticIndex(w io.Writer, meta semanticIndexMetadata, snapshot *vector.Snapshot) error {
	return encodeSemanticIndexContext(context.Background(), w, meta, snapshot)
}

func encodeSemanticIndexContext(ctx context.Context, w io.Writer, meta semanticIndexMetadata, snapshot *vector.Snapshot) error {
	if ctx == nil {
		return fmt.Errorf("semantic encode: nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	metadata, checksum, err := marshalSemanticIndexMetadata(meta)
	if err != nil {
		return err
	}
	var header [semanticWrapperSize]byte
	copy(header[:8], semanticIndexMagic[:])
	binary.LittleEndian.PutUint16(header[8:10], semanticIndexVersion)
	binary.LittleEndian.PutUint16(header[10:12], 0)
	binary.LittleEndian.PutUint32(header[12:16], uint32(len(metadata)))
	copy(header[16:48], checksum[:])
	if err := writeSemanticAll(w, header[:]); err != nil {
		return err
	}
	if err := writeSemanticAll(w, metadata); err != nil {
		return err
	}
	return vector.EncodeContext(ctx, w, snapshot)
}

func decodeSemanticIndex(r io.Reader, limits vector.Limits) (semanticIndexMetadata, *vector.Snapshot, error) {
	return decodeSemanticIndexContext(context.Background(), r, limits)
}

func decodeSemanticIndexContext(ctx context.Context, r io.Reader, limits vector.Limits) (semanticIndexMetadata, *vector.Snapshot, error) {
	meta, err := decodeSemanticIndexMetadataContext(ctx, r)
	if err != nil {
		return semanticIndexMetadata{}, nil, err
	}
	snapshot, err := vector.DecodeWithLimitsContext(ctx, r, limits)
	if err != nil {
		return semanticIndexMetadata{}, nil, err
	}
	return meta, snapshot, nil
}

func decodeSemanticIndexMetadataContext(ctx context.Context, r io.Reader) (semanticIndexMetadata, error) {
	if ctx == nil {
		return semanticIndexMetadata{}, fmt.Errorf("semantic decode: nil context")
	}
	if err := ctx.Err(); err != nil {
		return semanticIndexMetadata{}, err
	}
	var header [semanticWrapperSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return semanticIndexMetadata{}, fmt.Errorf("semantic index header: %w", err)
	}
	if subtle.ConstantTimeCompare(header[:8], semanticIndexMagic[:]) != 1 {
		return semanticIndexMetadata{}, fmt.Errorf("semantic index magic 非法")
	}
	if version := binary.LittleEndian.Uint16(header[8:10]); version != semanticIndexVersion {
		return semanticIndexMetadata{}, fmt.Errorf("semantic index version=%d 不支持", version)
	}
	if binary.LittleEndian.Uint16(header[10:12]) != 0 {
		return semanticIndexMetadata{}, fmt.Errorf("semantic index flags 非法")
	}
	metadataLen := binary.LittleEndian.Uint32(header[12:16])
	if metadataLen == 0 || metadataLen > semanticMaxMetadata {
		return semanticIndexMetadata{}, fmt.Errorf("semantic metadata 长度=%d 越界", metadataLen)
	}
	metadata := make([]byte, int(metadataLen))
	if _, err := io.ReadFull(r, metadata); err != nil {
		return semanticIndexMetadata{}, fmt.Errorf("semantic metadata: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return semanticIndexMetadata{}, err
	}
	actual := sha256.Sum256(metadata)
	if subtle.ConstantTimeCompare(header[16:48], actual[:]) != 1 {
		return semanticIndexMetadata{}, fmt.Errorf("semantic metadata checksum 不匹配")
	}
	var meta semanticIndexMetadata
	dec := json.NewDecoder(strings.NewReader(string(metadata)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&meta); err != nil {
		return semanticIndexMetadata{}, fmt.Errorf("semantic metadata JSON: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return semanticIndexMetadata{}, fmt.Errorf("semantic metadata 有尾随 JSON")
	}
	if err := validateSemanticIndexMetadata(meta); err != nil {
		return semanticIndexMetadata{}, err
	}
	return meta, nil
}

func validateSemanticIndexMetadata(meta semanticIndexMetadata) error {
	if meta.Schema != 1 || meta.Dimensions < 1 || meta.Dimensions > 4096 || meta.Records < 0 ||
		len(meta.Generation) != 32 ||
		len(meta.SettingsFingerprint) == 0 || len(meta.SettingsFingerprint) > 256 ||
		len(meta.EmbedderFingerprint) == 0 || len(meta.EmbedderFingerprint) > 256 ||
		len(meta.ProbeFingerprint) == 0 || len(meta.ProbeFingerprint) > 128 ||
		len(meta.SourceFingerprint) != 64 || len(meta.BuiltAt) == 0 || len(meta.BuiltAt) > 64 {
		return fmt.Errorf("semantic metadata 字段非法")
	}
	if _, err := hex.DecodeString(meta.Generation); err != nil {
		return fmt.Errorf("semantic generation 非十六进制")
	}
	if _, err := hex.DecodeString(meta.SourceFingerprint); err != nil {
		return fmt.Errorf("semantic source fingerprint 非十六进制")
	}
	if _, err := time.Parse(time.RFC3339, meta.BuiltAt); err != nil {
		return fmt.Errorf("semantic built_at 非法")
	}
	return nil
}

func writeSemanticAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
