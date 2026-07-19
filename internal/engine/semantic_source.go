package engine

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/zdypro888/iknowledge/internal/model"
)

const (
	semanticMaxDocumentRunes = 6000
	// 4KiB 保证最坏 6x JSON 转义下，32 条重建批次仍低于 provider 的
	// 1MiB request hard cap；知识卡片正常远小于该值。
	semanticMaxDocumentBytes    = 4 << 10
	semanticMaxRawDocumentBytes = 1 << 20
	semanticMaxSourceRecords    = 100_000
	// 主真相锁内只允许收集有界的 immutable string headers/轻量 DTO。
	// records 是最终卡片数，items 还覆盖节点、entry、journal node/ref、
	// dispute、flow step/convention 等构造输入，防止“卡片很少但笛卡尔展开很大”。
	semanticMaxSourceItems      = 250_000
	semanticMaxSourceFieldBytes = 1 << 20
	// NodeID is copied into vector NodeID and the derived RecordID. Keeping it
	// below the vector layer's 4KiB per-string limit makes both fields valid and
	// prevents one identifier from being multiplied into an enormous preflight
	// allocation. References may contain nodeID#entryID and are bounded
	// separately before they can be copied into card metadata.
	semanticMaxSourceNodeIDBytes    = 4032
	semanticMaxSourceReferenceBytes = (semanticMaxSourceNodeIDBytes * 2) + 1
	// 重建会持有最终脱敏文档直到批量发送；独立限制文本工作集，避免
	// “向量 payload 合法”却因 10 万条超长知识耗尽内存。
	semanticMaxSourceBytes         = 64 << 20
	semanticMaxSourceMetadataBytes = 64 << 20
	semanticMaxRenderedItemBytes   = 1 << 20
	// A single historical reference must never expand through an unbounded split
	// lineage before the global item budget can observe it. Thousands of heirs are
	// already a corrupt/pathological truth graph; fail explicitly rather than omit
	// heirs or repeat the expansion for every dispute/change/flow reference.
	semanticMaxSourceResolutionCandidates = 4096
	// 同一节点/通道可能有很多条知识。先切成小卡片再向量化，查询时按
	// NodeID distinct，既保留全部知识，又不让一个节点挤满 Top-K。
	semanticCardRawTarget = 3 << 10
	// Structured decision cards compact each field independently before the
	// final card chunker runs. Their sum stays below semanticCardRawTarget so a
	// long "what" cannot erase the "why"/rebuttal, and a long rejected option
	// cannot erase its reason.
	semanticDecisionWhatBytes     = 640
	semanticDecisionWhyBytes      = 640
	semanticDecisionTaskBytes     = 320
	semanticDecisionRebuttalBytes = 480
	semanticDecisionVerifiedBytes = 320
	semanticRejectedOptionBytes   = 1300
	semanticRejectedReasonBytes   = 1300
)

const (
	semanticLaneCurrent     = "current"
	semanticLaneRisk        = "risk"
	semanticLaneHistory     = "history"
	semanticStaleFlowMarker = "[stale-flow state=%s] 该步骤引用的代码节点待复核，不能视为当前有效流程"
)

const (
	semanticDocumentPreprocessVersion = "iknowledge-semantic-document-v5"
	semanticSourcePreprocessVersion   = "iknowledge-semantic-source-v5"
)

type semanticDocument struct {
	RecordID   string
	NodeID     string
	Kind       string // current | risk | history
	Text       string
	SourceHash [32]byte
	Facets     []string
	References []string
}

type semanticRawDocument struct {
	RecordID   string
	NodeID     string
	Kind       string
	Raw        string
	Facets     []string
	References []string
}

type semanticSourceRecord struct {
	NodeID     string
	Kind       string
	SourceHash [32]byte
	Facets     []string
	References []string
}

// semanticSourceManifest 是随健康 truth generation 发布的不可变视图。
// records 发布后只读；变更时整体替换，查询可在 rt.mu 外安全持有旧 map，最终
// merge 仍会在当前 version 下再次验证。
type semanticSourceManifest struct {
	version     uint64
	fingerprint [32]byte
	records     map[string]semanticSourceRecord
	errText     string
	ready       bool
}

type semanticCardItem struct {
	facet     string
	reference string
	text      string
}

// semanticSourceInput 是主真相锁内复制出的有界、不可变轻量快照。这里不保留
// index/model 指针或 slice alias；string 在 Go 中不可变，只复制 header 即可。
// 所有排序、inactive graph、格式化、分卡、脱敏与 hash 都在 rt.mu 外完成。
type semanticSourceInput struct {
	entries  []semanticEntryInput
	eras     []semanticEraInput
	changes  []model.Change // 只填语义构造所需字段，所有 slice 均深拷贝
	flows    []semanticFlowInput
	disputes []semanticDisputeInput
}

type semanticEntryInput struct {
	nodeID     string
	entryRef   string
	kind       string
	confidence string
	text       string
	baseRisk   bool
}

type semanticEraInput struct {
	nodeID string
	text   string
}

type semanticDisputeInput struct {
	sourceRef string
	targetRef string
}

type semanticFlowInput struct {
	id           string
	title        string
	conventions  []string
	troubleshoot string
	steps        []semanticFlowStepInput
}

type semanticFlowStepInput struct {
	nodeID      string
	note        string
	staleReason string
}

type semanticSourceBudget struct {
	items int
	bytes int
	ctx   context.Context
}

func (b *semanticSourceBudget) addItem(kind string) error {
	if b.items&63 == 0 && b.ctx != nil {
		if err := b.ctx.Err(); err != nil {
			return err
		}
	}
	if b.items >= semanticMaxSourceItems {
		return fmt.Errorf("semantic source %s 数量超过硬上限 %d", kind, semanticMaxSourceItems)
	}
	b.items++
	return nil
}

func (b *semanticSourceBudget) addItems(kind string, count int) error {
	if count < 0 {
		return fmt.Errorf("semantic source %s 数量非法", kind)
	}
	if b.ctx != nil {
		if err := b.ctx.Err(); err != nil {
			return err
		}
	}
	if count > semanticMaxSourceItems-b.items {
		return fmt.Errorf("semantic source %s 数量超过硬上限 %d", kind, semanticMaxSourceItems)
	}
	b.items += count
	return nil
}

func (b *semanticSourceBudget) addString(kind, value string) error {
	return b.addBoundedString(kind, value, semanticMaxSourceFieldBytes)
}

func (b *semanticSourceBudget) addIdentifier(kind, value string) error {
	return b.addBoundedString(kind, value, semanticMaxSourceNodeIDBytes)
}

func (b *semanticSourceBudget) addReference(kind, value string) error {
	return b.addBoundedString(kind, value, semanticMaxSourceReferenceBytes)
}

func (b *semanticSourceBudget) addBoundedString(kind, value string, limit int) error {
	if b.items&63 == 0 && b.ctx != nil {
		if err := b.ctx.Err(); err != nil {
			return err
		}
	}
	if len(value) > limit {
		return fmt.Errorf("semantic source %s 单字段 %d bytes 超过硬上限 %d bytes", kind, len(value), limit)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("semantic source %s 不是合法 UTF-8", kind)
	}
	if len(value) > semanticMaxSourceBytes-b.bytes {
		return fmt.Errorf("semantic source DTO 文本超过硬上限 %d MiB", semanticMaxSourceBytes>>20)
	}
	b.bytes += len(value)
	return nil
}

func semanticEntryRef(nodeID, entryID string) (string, error) {
	if err := validateSemanticEntryRefShape(nodeID, entryID); err != nil {
		return "", err
	}
	return nodeID + "#" + entryID, nil
}

func validateSemanticEntryRefShape(nodeID, entryID string) error {
	if len(entryID) > semanticMaxSourceReferenceBytes-1 ||
		len(nodeID) > semanticMaxSourceReferenceBytes-1-len(entryID) {
		return fmt.Errorf("semantic source entry ref 单字段超过硬上限 %d bytes", semanticMaxSourceReferenceBytes)
	}
	return nil
}

// semanticSourceShapePreflightLockedContext performs a successful-path
// allocation-free walk of the complete source shape before any DTO slice/map
// or per-node entry lookup is created. It validates all count/byte/field limits
// and every possible lineage fan-out conservatively; the construction pass
// below repeats exact accounting while materializing only the authorized view.
// Caller holds rt.mu.RLock, so ix and flows cannot change between the two passes.
func (e *Engine) semanticSourceShapePreflightLockedContext(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("semantic source preflight: nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ix := e.rt.ix
	if ix == nil {
		return nil
	}
	budget := semanticSourceBudget{ctx: ctx}
	for nodeID, ref := range ix.Nodes() {
		if err := budget.addItem("node/entry/flow 输入"); err != nil {
			return err
		}
		if err := budget.addIdentifier("node ID", nodeID); err != nil {
			return err
		}
		if ref == nil || ref.Node == nil {
			continue
		}
		n := ref.Node
		for i := range n.Entries {
			if err := budget.addItem("node/entry/flow 输入"); err != nil {
				return err
			}
			entry := &n.Entries[i]
			// Resolver lookup includes inactive predecessors in supersedes chains,
			// so every ID must be shape-safe even when only active entries emit cards.
			if err := budget.addIdentifier("entry ID", entry.ID); err != nil {
				return err
			}
			if !entry.Active() {
				continue
			}
			if err := validateSemanticEntryRefShape(nodeID, entry.ID); err != nil {
				return err
			}
			for _, targetRef := range entry.Disputes {
				if err := budget.addItem("node/entry/flow 输入"); err != nil {
					return err
				}
				if err := budget.addReference("dispute ref", targetRef); err != nil {
					return err
				}
				// Check only raw fan-out here: no candidate map and no Entries scan.
				if err := ix.CheckEntryResolutionShapeContext(ctx, targetRef, semanticMaxSourceResolutionCandidates); err != nil {
					return fmt.Errorf("semantic source dispute ref preflight: %w", err)
				}
			}
			if n.Status == model.StatusOrphaned {
				continue
			}
			if err := budget.addString("entry text", entry.Text); err != nil {
				return err
			}
			if strings.TrimSpace(entry.Text) == "" {
				continue
			}
			if err := budget.addString("entry kind", entry.Kind); err != nil {
				return err
			}
			if err := budget.addString("entry confidence", string(entry.Confidence)); err != nil {
				return err
			}
		}
		if n.Status != model.StatusOrphaned {
			if err := budget.addString("era summary", n.EraSummary); err != nil {
				return err
			}
			if strings.TrimSpace(n.EraSummary) != "" {
				if err := budget.addItem("node/entry/flow 输入"); err != nil {
					return err
				}
			}
		}
	}

	for _, change := range ix.Changes() {
		if err := budget.addItem("node/entry/flow 输入"); err != nil {
			return err
		}
		if err := budget.addIdentifier("change ID", change.ID); err != nil {
			return err
		}
		for _, value := range []string{change.Task, change.What, change.Why, change.Rebuttal, change.Verified} {
			if err := budget.addString("change field", value); err != nil {
				return err
			}
		}
		for _, value := range []string{change.Overturns, change.Reverts} {
			if err := budget.addIdentifier("change reference", value); err != nil {
				return err
			}
		}
		for _, historicalID := range change.Nodes {
			if err := budget.addItem("node/entry/flow 输入"); err != nil {
				return err
			}
			if err := budget.addIdentifier("change node ref", historicalID); err != nil {
				return err
			}
			count, err := ix.CountNodeIDsAtContext(ctx, historicalID, change.At, semanticMaxSourceResolutionCandidates)
			if err != nil {
				return fmt.Errorf("semantic source change lineage preflight: %w", err)
			}
			// No dedupe map exists yet; charging every declaration is a safe
			// upper bound on the materialized unique-node set.
			if err := budget.addItems("resolved change nodes", count); err != nil {
				return err
			}
		}
		for _, item := range change.Rejected {
			if err := budget.addItem("node/entry/flow 输入"); err != nil {
				return err
			}
			if err := budget.addString("rejected option", item.Option); err != nil {
				return err
			}
			if err := budget.addString("rejected reason", item.Reason); err != nil {
				return err
			}
		}
	}

	for _, flow := range e.rt.flows {
		if err := budget.addItem("node/entry/flow 输入"); err != nil {
			return err
		}
		if flow.Deprecated {
			continue
		}
		if err := budget.addIdentifier("flow ID", flow.ID); err != nil {
			return err
		}
		if err := budget.addString("flow title", flow.Title); err != nil {
			return err
		}
		if err := budget.addString("flow troubleshoot", flow.Troubleshoot); err != nil {
			return err
		}
		for _, convention := range flow.Conventions {
			if err := budget.addItem("node/entry/flow 输入"); err != nil {
				return err
			}
			if err := budget.addString("flow convention", convention); err != nil {
				return err
			}
		}
		for _, step := range flow.Steps {
			if err := budget.addItem("node/entry/flow 输入"); err != nil {
				return err
			}
			if err := budget.addIdentifier("flow step node", step.Node); err != nil {
				return err
			}
			if err := budget.addString("flow step note", step.Note); err != nil {
				return err
			}
			at := step.Since
			if at.IsZero() {
				at = flow.Since
			}
			count, err := ix.CountNodeIDsAtContext(ctx, step.Node, at, semanticMaxSourceResolutionCandidates)
			if err != nil {
				return fmt.Errorf("semantic source flow lineage preflight: %w", err)
			}
			if err := budget.addItems("resolved flow nodes", count); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

// semanticSourceInputLocked 只在 rt.mu.RLock 下收集有硬 count/byte/field
// 上限的 DTO；绝不在锁内格式化、聚合、分卡、排序或脱敏。
func (e *Engine) semanticSourceInputLocked() (semanticSourceInput, error) {
	return e.semanticSourceInputLockedContext(context.Background())
}

func (e *Engine) semanticSourceInputLockedContext(ctx context.Context) (semanticSourceInput, error) {
	var out semanticSourceInput
	if ctx == nil {
		return out, fmt.Errorf("semantic source input: nil context")
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if e.rt.ix == nil {
		return out, nil
	}
	if err := e.semanticSourceShapePreflightLockedContext(ctx); err != nil {
		return out, err
	}
	ix := e.rt.ix
	entryResolver := ix.NewEntryResolver()
	budget := semanticSourceBudget{ctx: ctx}
	for nodeID, ref := range ix.Nodes() {
		if err := budget.addItem("node/entry/flow 输入"); err != nil {
			return semanticSourceInput{}, err
		}
		if err := budget.addIdentifier("node ID", nodeID); err != nil {
			return semanticSourceInput{}, err
		}
		if ref == nil || ref.Node == nil {
			continue
		}
		n := ref.Node
		for i := range n.Entries {
			if err := budget.addItem("node/entry/flow 输入"); err != nil {
				return semanticSourceInput{}, err
			}
			entry := &n.Entries[i]
			if !entry.Active() {
				continue
			}
			if err := budget.addIdentifier("entry ID", entry.ID); err != nil {
				return semanticSourceInput{}, err
			}
			entryRef, err := semanticEntryRef(nodeID, entry.ID)
			if err != nil {
				return semanticSourceInput{}, err
			}
			for _, targetRef := range entry.Disputes {
				if err := budget.addItem("node/entry/flow 输入"); err != nil {
					return semanticSourceInput{}, err
				}
				if err := budget.addReference("dispute ref", targetRef); err != nil {
					return semanticSourceInput{}, err
				}
				resolved, target, err := entryResolver.ResolveContext(ctx, targetRef, semanticMaxSourceResolutionCandidates)
				if err != nil {
					return semanticSourceInput{}, fmt.Errorf("semantic source dispute ref resolution: %w", err)
				}
				if target == nil || !target.Active() {
					continue
				}
				if err := budget.addReference("resolved dispute ref", entryRef); err != nil {
					return semanticSourceInput{}, err
				}
				if err := budget.addReference("resolved dispute ref", resolved); err != nil {
					return semanticSourceInput{}, err
				}
				out.disputes = append(out.disputes, semanticDisputeInput{sourceRef: entryRef, targetRef: resolved})
			}
			if n.Status == model.StatusOrphaned {
				continue
			}
			if err := budget.addString("entry text", entry.Text); err != nil {
				return semanticSourceInput{}, err
			}
			if strings.TrimSpace(entry.Text) == "" {
				continue
			}
			if err := budget.addString("entry kind", entry.Kind); err != nil {
				return semanticSourceInput{}, err
			}
			confidence := string(entry.Confidence)
			if err := budget.addString("entry confidence", confidence); err != nil {
				return semanticSourceInput{}, err
			}
			if err := budget.addReference("entry ref", entryRef); err != nil {
				return semanticSourceInput{}, err
			}
			out.entries = append(out.entries, semanticEntryInput{
				nodeID: nodeID, entryRef: entryRef, kind: entry.Kind, confidence: confidence, text: entry.Text,
				baseRisk: entry.Kind == model.KindPitfall || n.Status == model.StatusSuspect || n.PendingAnchor || entry.Confidence == model.ConfidenceSuspect,
			})
		}
		if n.Status != model.StatusOrphaned {
			if err := budget.addString("era summary", n.EraSummary); err != nil {
				return semanticSourceInput{}, err
			}
			if strings.TrimSpace(n.EraSummary) != "" {
				if err := budget.addItem("node/entry/flow 输入"); err != nil {
					return semanticSourceInput{}, err
				}
				out.eras = append(out.eras, semanticEraInput{nodeID: nodeID, text: n.EraSummary})
			}
		}
	}

	for _, change := range ix.Changes() {
		if err := budget.addItem("node/entry/flow 输入"); err != nil {
			return semanticSourceInput{}, err
		}
		if err := budget.addIdentifier("change ID", change.ID); err != nil {
			return semanticSourceInput{}, err
		}
		for _, field := range []struct{ kind, value string }{
			{"change task", change.Task}, {"change what", change.What}, {"change why", change.Why},
			{"change rebuttal", change.Rebuttal}, {"change verified", change.Verified},
		} {
			if err := budget.addString(field.kind, field.value); err != nil {
				return semanticSourceInput{}, err
			}
		}
		for _, field := range []struct{ kind, value string }{
			{"change overturns", change.Overturns}, {"change reverts", change.Reverts},
		} {
			if err := budget.addIdentifier(field.kind, field.value); err != nil {
				return semanticSourceInput{}, err
			}
		}
		seenNodes := make(map[string]bool)
		var nodes []string
		for _, historicalID := range change.Nodes {
			if err := budget.addItem("node/entry/flow 输入"); err != nil {
				return semanticSourceInput{}, err
			}
			if err := budget.addIdentifier("change node ref", historicalID); err != nil {
				return semanticSourceInput{}, err
			}
			resolvedIDs, err := ix.ResolveNodeIDsAtContext(ctx, historicalID, change.At, semanticMaxSourceResolutionCandidates)
			if err != nil {
				return semanticSourceInput{}, fmt.Errorf("semantic source change lineage resolution: %w", err)
			}
			for _, resolved := range resolvedIDs {
				if seenNodes[resolved] {
					continue
				}
				if err := budget.addItem("node/entry/flow 输入"); err != nil {
					return semanticSourceInput{}, err
				}
				if err := budget.addIdentifier("resolved change node", resolved); err != nil {
					return semanticSourceInput{}, err
				}
				seenNodes[resolved] = true
				nodes = append(nodes, resolved)
			}
		}
		var rejected []model.Rejected
		for _, item := range change.Rejected {
			if err := budget.addItem("node/entry/flow 输入"); err != nil {
				return semanticSourceInput{}, err
			}
			if err := budget.addString("rejected option", item.Option); err != nil {
				return semanticSourceInput{}, err
			}
			if err := budget.addString("rejected reason", item.Reason); err != nil {
				return semanticSourceInput{}, err
			}
			rejected = append(rejected, item)
		}
		out.changes = append(out.changes, model.Change{
			ID: change.ID, Nodes: nodes, At: change.At, Task: change.Task, What: change.What, Why: change.Why,
			Rejected: rejected, Overturns: change.Overturns, Rebuttal: change.Rebuttal,
			Reverts: change.Reverts, Verified: change.Verified,
		})
	}

	for _, flow := range e.rt.flows {
		if err := budget.addItem("node/entry/flow 输入"); err != nil {
			return semanticSourceInput{}, err
		}
		if flow.Deprecated {
			continue
		}
		if err := budget.addIdentifier("flow ID", flow.ID); err != nil {
			return semanticSourceInput{}, err
		}
		for _, field := range []struct{ kind, value string }{
			{"flow title", flow.Title}, {"flow troubleshoot", flow.Troubleshoot},
		} {
			if err := budget.addString(field.kind, field.value); err != nil {
				return semanticSourceInput{}, err
			}
		}
		var conventions []string
		for _, convention := range flow.Conventions {
			if err := budget.addItem("node/entry/flow 输入"); err != nil {
				return semanticSourceInput{}, err
			}
			if err := budget.addString("flow convention", convention); err != nil {
				return semanticSourceInput{}, err
			}
			conventions = append(conventions, convention)
		}
		input := semanticFlowInput{
			id: flow.ID, title: flow.Title, conventions: conventions, troubleshoot: flow.Troubleshoot,
		}
		for _, step := range flow.Steps {
			if err := budget.addItem("node/entry/flow 输入"); err != nil {
				return semanticSourceInput{}, err
			}
			if err := budget.addIdentifier("flow step node", step.Node); err != nil {
				return semanticSourceInput{}, err
			}
			if err := budget.addString("flow step note", step.Note); err != nil {
				return semanticSourceInput{}, err
			}
			at := step.Since
			if at.IsZero() {
				at = flow.Since
			}
			resolvedIDs, err := ix.ResolveNodeIDsAtContext(ctx, step.Node, at, semanticMaxSourceResolutionCandidates)
			if err != nil {
				return semanticSourceInput{}, fmt.Errorf("semantic source flow lineage resolution: %w", err)
			}
			for _, nodeID := range resolvedIDs {
				if err := budget.addItem("node/entry/flow 输入"); err != nil {
					return semanticSourceInput{}, err
				}
				if err := budget.addIdentifier("resolved flow node", nodeID); err != nil {
					return semanticSourceInput{}, err
				}
				ref := ix.Node(nodeID)
				staleReason := ""
				if ref != nil && ref.Node != nil {
					switch {
					case ref.Node.Status == model.StatusOrphaned:
						staleReason = "orphaned"
					case ref.Node.Status == model.StatusSuspect:
						staleReason = "suspect"
					case ref.Node.PendingAnchor:
						staleReason = "pending-anchor"
					}
				}
				input.steps = append(input.steps, semanticFlowStepInput{
					nodeID: nodeID, note: step.Note, staleReason: staleReason,
				})
			}
		}
		if len(input.steps) > 0 {
			out.flows = append(out.flows, input)
		}
	}
	return out, nil
}

type semanticRenderBudget struct {
	items         int
	bodyBytes     uint64
	metadataBytes uint64
	ctx           context.Context
}

func (b *semanticRenderBudget) add(size int, nodeID, lane, facet, reference string) error {
	if b.items&63 == 0 && b.ctx != nil {
		if err := b.ctx.Err(); err != nil {
			return err
		}
	}
	if b.items >= semanticMaxSourceItems {
		return fmt.Errorf("semantic source card items 超过硬上限 %d", semanticMaxSourceItems)
	}
	b.items++
	if size < 0 || size > semanticMaxRenderedItemBytes {
		return fmt.Errorf("semantic source 单条待格式化正文超过硬上限 %d MiB", semanticMaxRenderedItemBytes>>20)
	}
	if uint64(size) > uint64(semanticMaxSourceBytes)-b.bodyBytes {
		return fmt.Errorf("semantic source 待格式化正文超过硬上限 %d MiB", semanticMaxSourceBytes>>20)
	}
	b.bodyBytes += uint64(size)
	// Conservative per-item expansion. Every emitted chunk owns at least one
	// non-empty item; charging a full record/map/slice allowance for every item
	// therefore strictly dominates chunk metadata even when facet/reference sets
	// are unioned. The second NodeID copy covers its occurrence in RecordID.
	metadata := uint64(512 + 2*len(nodeID) + len(lane) + len(facet) + len(reference) + 32)
	if metadata > uint64(semanticMaxSourceMetadataBytes)-b.metadataBytes {
		return fmt.Errorf("semantic source 输出元数据超过硬上限 %d MiB", semanticMaxSourceMetadataBytes>>20)
	}
	b.metadataBytes += metadata
	return nil
}

func semanticDecisionCard(change model.Change, state string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[decision-history state=%s change=%s]\n改动: %s\n原因: %s",
		state, shortChangeID(change.ID),
		compactSemanticTextBytes(strings.TrimSpace(change.What), semanticDecisionWhatBytes),
		compactSemanticTextBytes(strings.TrimSpace(change.Why), semanticDecisionWhyBytes))
	if task := strings.TrimSpace(change.Task); task != "" {
		b.WriteString("\n任务: ")
		b.WriteString(compactSemanticTextBytes(task, semanticDecisionTaskBytes))
	}
	if change.Overturns != "" {
		b.WriteString("\n推翻: ")
		b.WriteString(change.Overturns)
		b.WriteString("\n反驳: ")
		b.WriteString(compactSemanticTextBytes(strings.TrimSpace(change.Rebuttal), semanticDecisionRebuttalBytes))
	}
	if change.Reverts != "" {
		b.WriteString("\n撤销: ")
		b.WriteString(change.Reverts)
	}
	if verified := strings.TrimSpace(change.Verified); verified != "" {
		b.WriteString("\n验证: ")
		b.WriteString(compactSemanticTextBytes(verified, semanticDecisionVerifiedBytes))
	}
	return b.String()
}

func semanticRejectedCard(changeID, label string, rejected model.Rejected) string {
	return fmt.Sprintf("[%s change=%s] 否决方案: %s\n原因: %s", label, shortChangeID(changeID),
		compactSemanticTextBytes(strings.TrimSpace(rejected.Option), semanticRejectedOptionBytes),
		compactSemanticTextBytes(strings.TrimSpace(rejected.Reason), semanticRejectedReasonBytes))
}

// buildSemanticRawDocuments 在 rt.mu 外完成所有可能较重的聚合/格式化。先用
// DTO 计算展开后的 item/byte 上限，通过后才创建大字符串和 cards map。
func buildSemanticRawDocuments(input semanticSourceInput) ([]semanticRawDocument, error) {
	return buildSemanticRawDocumentsContext(context.Background(), input)
}

func buildSemanticRawDocumentsContext(ctx context.Context, input semanticSourceInput) ([]semanticRawDocument, error) {
	if ctx == nil {
		return nil, fmt.Errorf("semantic raw documents: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	openSets := make(map[string]map[string]bool)
	for i, dispute := range input.disputes {
		if i&63 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		for _, pair := range [][2]string{{dispute.sourceRef, dispute.targetRef}, {dispute.targetRef, dispute.sourceRef}} {
			set := openSets[pair[0]]
			if set == nil {
				set = make(map[string]bool)
				openSets[pair[0]] = set
			}
			set[pair[1]] = true
		}
	}
	open := make(map[string][]string, len(openSets))
	for entryRef, set := range openSets {
		values, err := sortedSemanticSetContext(ctx, set)
		if err != nil {
			return nil, err
		}
		open[entryRef] = values
	}
	inactive, err := inactiveChangesContext(ctx, input.changes)
	if err != nil {
		return nil, err
	}
	for i := range input.changes {
		if err := contextSortStrings(ctx, input.changes[i].Nodes); err != nil {
			return nil, err
		}
	}
	if err := contextHeapSort(ctx, input.flows, func(a, b semanticFlowInput) bool { return a.id < b.id }); err != nil {
		return nil, err
	}

	var renderBudget = semanticRenderBudget{ctx: ctx}
	for _, entry := range input.entries {
		size := len("[ confidence=] ") + len(entry.kind) + len(entry.confidence) + len(entry.text)
		if refs := open[entry.entryRef]; len(refs) > 0 {
			size += len("\n[open-dispute] ")
			for i, ref := range refs {
				size += len(ref)
				if i > 0 {
					size += len("、")
				}
			}
		}
		if err := renderBudget.add(size, entry.nodeID, semanticLaneHistory, entry.kind, entry.entryRef); err != nil {
			return nil, err
		}
	}
	for _, era := range input.eras {
		if err := renderBudget.add(len("[era-summary historical] ")+len(era.text), era.nodeID, semanticLaneHistory, "era_summary", era.nodeID); err != nil {
			return nil, err
		}
	}
	for changeIndex, change := range input.changes {
		if changeIndex&63 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		// Keep the pre-compaction Cartesian budget conservative: a single huge
		// journal field fanned to many lineage heirs is rejected before repeated
		// formatting, even though the emitted card later compacts each field.
		decisionSize := len("[decision-history state= change=]\n改动: \n原因: ") + len("active-lineage") + len(shortChangeID(change.ID)) + len(change.What) + len(change.Why)
		if inactive[change.ID] {
			decisionSize += len("overturned-or-reverted") - len("active-lineage")
		}
		if strings.TrimSpace(change.Task) != "" {
			decisionSize += len("\n任务: ") + len(change.Task)
		}
		if change.Overturns != "" {
			decisionSize += len("\n推翻: \n反驳: ") + len(change.Overturns) + len(change.Rebuttal)
		}
		if change.Reverts != "" {
			decisionSize += len("\n撤销: ") + len(change.Reverts)
		}
		if strings.TrimSpace(change.Verified) != "" {
			decisionSize += len("\n验证: ") + len(change.Verified)
		}
		for _, nodeID := range change.Nodes {
			if err := renderBudget.add(decisionSize, nodeID, semanticLaneHistory, "decision_history", change.ID); err != nil {
				return nil, err
			}
			label := "rejected-active"
			if inactive[change.ID] {
				label = "rejected-historical"
			}
			for _, rejected := range change.Rejected {
				size := len("[ change=] 否决方案: \n原因: ") + len(label) + len(shortChangeID(change.ID)) + len(rejected.Option) + len(rejected.Reason)
				if err := renderBudget.add(size, nodeID, semanticLaneHistory, "rejected", change.ID); err != nil {
					return nil, err
				}
			}
		}
	}
	for flowIndex, flow := range input.flows {
		if flowIndex&63 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		for _, step := range flow.steps {
			parts, size := 0, 0
			if flow.title != "" {
				parts++
				size += len("流程: ") + len(flow.title)
			}
			if step.note != "" {
				parts++
				size += len("步骤: ") + len(step.note)
			}
			for i, convention := range flow.conventions {
				if i&63 == 0 {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
				}
				parts++
				size += len("约定: ") + len(convention)
				if size > semanticMaxRenderedItemBytes {
					return nil, fmt.Errorf("semantic source 单条待格式化正文超过硬上限 %d MiB", semanticMaxRenderedItemBytes>>20)
				}
			}
			if step.staleReason != "" {
				parts++
				size += len(fmt.Sprintf(semanticStaleFlowMarker, step.staleReason))
			}
			if parts > 0 {
				size += parts - 1
				facet := "flow"
				if step.staleReason != "" {
					facet = "stale_flow"
				}
				if err := renderBudget.add(size, step.nodeID, semanticLaneHistory, facet, flow.id); err != nil {
					return nil, err
				}
			}
			if flow.troubleshoot != "" {
				if err := renderBudget.add(len("[flow-troubleshoot] ")+len(flow.troubleshoot), step.nodeID, semanticLaneRisk, "flow_troubleshoot", flow.id); err != nil {
					return nil, err
				}
			}
		}
	}

	cards := make(map[string]map[string][]semanticCardItem)
	formattedItems := 0
	add := func(nodeID, lane, facet, reference, text string) error {
		formattedItems++
		if formattedItems&63 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		text = strings.TrimSpace(text)
		if nodeID == "" || text == "" {
			return nil
		}
		byLane := cards[nodeID]
		if byLane == nil {
			byLane = make(map[string][]semanticCardItem)
			cards[nodeID] = byLane
		}
		byLane[lane] = append(byLane[lane], semanticCardItem{facet: facet, reference: reference, text: text})
		return nil
	}
	for _, entry := range input.entries {
		lane := semanticLaneCurrent
		refs := open[entry.entryRef]
		if entry.baseRisk || len(refs) > 0 {
			lane = semanticLaneRisk
		}
		line := fmt.Sprintf("[%s confidence=%s] %s", entry.kind, entry.confidence, entry.text)
		if len(refs) > 0 {
			line += "\n[open-dispute] " + strings.Join(refs, "、")
		}
		if err := add(entry.nodeID, lane, entry.kind, entry.entryRef, line); err != nil {
			return nil, err
		}
	}
	for _, era := range input.eras {
		if err := add(era.nodeID, semanticLaneHistory, "era_summary", era.nodeID, "[era-summary historical] "+era.text); err != nil {
			return nil, err
		}
	}
	for changeIndex, change := range input.changes {
		if changeIndex&63 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		lane := semanticLaneRisk
		decisionState := "active-lineage"
		if inactive[change.ID] {
			lane = semanticLaneHistory
			decisionState = "overturned-or-reverted"
		}
		for _, nodeID := range change.Nodes {
			if err := add(nodeID, semanticLaneHistory, "decision_history", change.ID, semanticDecisionCard(change, decisionState)); err != nil {
				return nil, err
			}
			for _, rejected := range change.Rejected {
				label := "rejected-active"
				if lane == semanticLaneHistory {
					label = "rejected-historical"
				}
				if err := add(nodeID, lane, "rejected", change.ID, semanticRejectedCard(change.ID, label, rejected)); err != nil {
					return nil, err
				}
			}
		}
	}
	for flowIndex, flow := range input.flows {
		if flowIndex&63 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		for _, step := range flow.steps {
			var current []string
			if flow.title != "" {
				current = append(current, "流程: "+flow.title)
			}
			if step.note != "" {
				current = append(current, "步骤: "+step.note)
			}
			for conventionIndex, convention := range flow.conventions {
				if conventionIndex&63 == 0 {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
				}
				current = append(current, "约定: "+convention)
			}
			lane, facet := semanticLaneCurrent, "flow"
			if step.staleReason != "" {
				lane, facet = semanticLaneRisk, "stale_flow"
				current = append(current, fmt.Sprintf(semanticStaleFlowMarker, step.staleReason))
			}
			if err := add(step.nodeID, lane, facet, flow.id, strings.Join(current, "\n")); err != nil {
				return nil, err
			}
			if flow.troubleshoot != "" {
				if err := add(step.nodeID, semanticLaneRisk, "flow_troubleshoot", flow.id, "[flow-troubleshoot] "+flow.troubleshoot); err != nil {
					return nil, err
				}
			}
		}
	}

	rawDocs := make([]semanticRawDocument, 0)
	totalRawBytes := 0
	nodeIDs, err := sortedSemanticCardNodesContext(ctx, cards)
	if err != nil {
		return nil, err
	}
	for _, nodeID := range nodeIDs {
		for _, lane := range []string{semanticLaneCurrent, semanticLaneRisk, semanticLaneHistory} {
			documents, err := renderSemanticCardChunksContext(ctx, nodeID, lane, cards[nodeID][lane])
			if err != nil {
				return nil, err
			}
			for _, doc := range documents {
				if len(rawDocs)&63 == 0 {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
				}
				if len(rawDocs) >= semanticMaxSourceRecords {
					return nil, fmt.Errorf("semantic source records 超过硬上限 %d", semanticMaxSourceRecords)
				}
				if len(doc.Raw) > semanticMaxRawDocumentBytes {
					return nil, fmt.Errorf("semantic 原始知识卡片 %s 超过硬上限 %d MiB", doc.RecordID, semanticMaxRawDocumentBytes>>20)
				}
				if len(doc.Raw) > semanticMaxSourceBytes-totalRawBytes {
					return nil, fmt.Errorf("semantic 原始知识卡片正文超过硬上限 %d MiB", semanticMaxSourceBytes>>20)
				}
				totalRawBytes += len(doc.Raw)
				rawDocs = append(rawDocs, doc)
			}
		}
	}
	return rawDocs, nil
}

type semanticSourceLease struct {
	manifestOnce     sync.Once
	documentsOnce    sync.Once
	releaseManifest  func()
	releaseDocuments func()
}

func (l *semanticSourceLease) ReleaseManifest() {
	if l == nil {
		return
	}
	l.manifestOnce.Do(func() {
		if l.releaseManifest != nil {
			l.releaseManifest()
		}
	})
}

func (l *semanticSourceLease) ReleaseDocuments() {
	if l == nil {
		return
	}
	l.documentsOnce.Do(func() {
		if l.releaseDocuments != nil {
			l.releaseDocuments()
		}
	})
}

func (l *semanticSourceLease) Release() {
	if l == nil {
		return
	}
	// Never retain a generation barrier merely because the larger documents
	// allocation is still being retired.
	l.ReleaseManifest()
	l.ReleaseDocuments()
}

func (e *Engine) semanticSourceMetadata(ctx context.Context) ([32]byte, int, error) {
	_, manifest, lease, err := e.semanticSourceSnapshotLease(ctx, false)
	if err != nil {
		if lease != nil {
			lease.Release()
		}
		return [32]byte{}, 0, err
	}
	fingerprint, records := manifest.fingerprint, len(manifest.records)
	manifest = semanticSourceManifest{}
	lease.Release()
	return fingerprint, records, nil
}

// semanticSourceDocuments returns provider documents with only their explicit
// process-budget lease. It deliberately releases the manifest generation
// barrier before returning, so a long provider rebuild cannot block truth Sync.
func (e *Engine) semanticSourceDocuments(ctx context.Context) ([]semanticDocument, [32]byte, *semanticSourceLease, error) {
	docs, manifest, lease, err := e.semanticSourceSnapshotLease(ctx, true)
	if err != nil {
		if lease != nil {
			lease.Release()
		}
		return nil, [32]byte{}, nil, err
	}
	fingerprint := manifest.fingerprint
	manifest = semanticSourceManifest{}
	lease.ReleaseManifest()
	return docs, fingerprint, lease, nil
}

// semanticSourceSnapshotLease 在主锁外完成确定性预处理，并仅在
// source version 未变时发布 manifest。返回的 release 保证 records map 所在代
// 在调用者使用期间仍被 daemon 预算记账；调用者必须恰好调用一次。
func (e *Engine) semanticSourceSnapshotLease(ctx context.Context, includeText bool) ([]semanticDocument, semanticSourceManifest, *semanticSourceLease, error) {
	if ctx == nil {
		return nil, semanticSourceManifest{}, nil, fmt.Errorf("semantic source: nil context")
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, semanticSourceManifest{}, nil, err
		}
		// Lock order is rt.mu -> sourceResidentMu -> coordinator.mu. A returned
		// read lease never reacquires rt.mu, so truth reload can safely wait for
		// old readers before retiring their generation and charge.
		if err := e.rt.mu.RLockContext(ctx); err != nil {
			return nil, semanticSourceManifest{}, nil, err
		}
		if err := e.semantic.sourceResidentMu.RLockContext(ctx); err != nil {
			e.rt.mu.RUnlock()
			return nil, semanticSourceManifest{}, nil, err
		}
		version := e.rt.semanticSourceVersion
		cached := e.rt.semanticManifest
		if cached.ready && cached.version == version {
			if cached.errText != "" {
				e.semantic.sourceResidentMu.RUnlock()
				e.rt.mu.RUnlock()
				return nil, cached, nil, fmt.Errorf("%s", cached.errText)
			}
			if !includeText {
				e.rt.mu.RUnlock()
				return nil, cached, &semanticSourceLease{releaseManifest: e.semantic.sourceResidentMu.RUnlock}, nil
			}
		}
		e.semantic.sourceResidentMu.RUnlock()
		e.rt.mu.RUnlock()
		releaseSource, err := e.acquireSemanticSourceBuild(ctx)
		if err != nil {
			return nil, semanticSourceManifest{}, nil, err
		}
		// Read process only after the gate has been selected and acquired. If the
		// local gate won, coordinator attachment now observes sourceGate != nil and
		// is rejected; if attachment won, this is the coordinator matching that
		// gate. The earlier snapshot admitted an unaccounted manifest publication.
		e.semantic.mu.Lock()
		process := e.semantic.process
		closing := e.semantic.closing
		e.semantic.mu.Unlock()
		if closing {
			releaseSource()
			return nil, semanticSourceManifest{}, nil, fmt.Errorf("semantic daemon 正在关闭")
		}
		// The daemon-wide gate is distinct from rebuild/provider/search gates:
		// many hot-enabled repositories may all need their first <=64MiB source
		// manifest before vector reservation. Serialize that peak without making a
		// rebuild deadlock itself, and re-read cache/version after the cancellable
		// wait so queued status calls coalesce instead of rebuilding the same source.
		if err := e.rt.mu.RLockContext(ctx); err != nil {
			releaseSource()
			return nil, semanticSourceManifest{}, nil, err
		}
		if err := e.semantic.sourceResidentMu.RLockContext(ctx); err != nil {
			e.rt.mu.RUnlock()
			releaseSource()
			return nil, semanticSourceManifest{}, nil, err
		}
		version = e.rt.semanticSourceVersion
		cached = e.rt.semanticManifest
		cachedCurrent := cached.ready && cached.version == version
		if cachedCurrent && cached.errText != "" {
			e.semantic.sourceResidentMu.RUnlock()
			e.rt.mu.RUnlock()
			releaseSource()
			return nil, cached, nil, fmt.Errorf("%s", cached.errText)
		}
		if cachedCurrent && !includeText {
			e.rt.mu.RUnlock()
			releaseSource()
			return nil, cached, &semanticSourceLease{releaseManifest: e.semantic.sourceResidentMu.RUnlock}, nil
		}
		e.semantic.sourceResidentMu.RUnlock()
		reservedSource := false
		if process != nil {
			reserveErr := process.reserveSourceTransient(e)
			if reserveErr != nil {
				e.rt.mu.RUnlock()
				releaseSource()
				return nil, semanticSourceManifest{}, nil, reserveErr
			}
			reservedSource = true
		}
		input, sourceErr := e.semanticSourceInputLockedContext(ctx)
		e.rt.mu.RUnlock()
		var rawDocs []semanticRawDocument
		if sourceErr == nil {
			rawDocs, sourceErr = buildSemanticRawDocumentsContext(ctx, input)
		}
		input = semanticSourceInput{}

		docs, manifest, buildErr := buildSemanticSource(ctx, version, rawDocs, includeText, sourceErr)
		rawDocs = nil
		var manifestBytes, documentBytes uint64
		if buildErr == nil {
			manifestBytes, buildErr = semanticManifestResidentBytesContext(ctx, manifest)
			if buildErr == nil && includeText {
				documentBytes, buildErr = semanticDocumentsResidentBytesContext(ctx, docs)
				if buildErr == nil && (manifestBytes > uint64(semanticSourceBuildMaxMiB)<<20 ||
					documentBytes > (uint64(semanticSourceBuildMaxMiB)<<20)-manifestBytes) {
					buildErr = fmt.Errorf("semantic source manifest+documents 驻留估算超过构造授权 %dMiB", semanticSourceBuildMaxMiB)
				}
			}
			if buildErr != nil {
				manifest = semanticSourceManifest{version: version, ready: true, errText: buildErr.Error()}
				docs = nil
			}
		}
		if errors.Is(buildErr, context.Canceled) || errors.Is(buildErr, context.DeadlineExceeded) {
			docs = nil
			manifest = semanticSourceManifest{}
			if reservedSource {
				process.releaseSourceTransient(e)
			}
			releaseSource()
			return nil, semanticSourceManifest{}, nil, buildErr
		}
		// Shutdown can begin while formatting runs outside all locks. Fence the
		// publication immediately before acquiring truth locks so a completed
		// shutdown cannot be followed by a new manifest/charge from this build.
		e.semantic.mu.Lock()
		closing = e.semantic.closing
		e.semantic.mu.Unlock()
		if closing {
			docs = nil
			manifest = semanticSourceManifest{}
			if reservedSource {
				process.releaseSourceTransient(e)
			}
			releaseSource()
			return nil, semanticSourceManifest{}, nil, fmt.Errorf("semantic daemon 正在关闭")
		}
		if err := e.rt.mu.LockContext(ctx); err != nil {
			docs = nil
			manifest = semanticSourceManifest{}
			if reservedSource {
				process.releaseSourceTransient(e)
			}
			releaseSource()
			return nil, semanticSourceManifest{}, nil, err
		}
		if e.rt.semanticSourceVersion != version {
			e.rt.mu.Unlock()
			docs = nil
			manifest = semanticSourceManifest{}
			if reservedSource {
				process.releaseSourceTransient(e)
			}
			releaseSource()
			continue
		}
		if err := e.semantic.sourceResidentMu.LockContext(ctx); err != nil {
			e.rt.mu.Unlock()
			docs = nil
			manifest = semanticSourceManifest{}
			if reservedSource {
				process.releaseSourceTransient(e)
			}
			releaseSource()
			return nil, semanticSourceManifest{}, nil, err
		}
		if buildErr != nil {
			if manifest.ready {
				e.rt.semanticManifest = manifest
			}
			if reservedSource {
				process.releaseSourceTransient(e)
				process.releaseSourceResident(e)
			}
		} else {
			if reservedSource {
				if promoteErr := process.promoteSourceTransient(e, manifestBytes, documentBytes); promoteErr != nil {
					docs = nil
					manifest = semanticSourceManifest{}
					process.releaseSourceTransient(e)
					process.releaseSourceResident(e)
					e.rt.semanticManifest = semanticSourceManifest{}
					e.semantic.sourceResidentMu.Unlock()
					e.rt.mu.Unlock()
					releaseSource()
					return nil, semanticSourceManifest{}, nil, promoteErr
				}
			}
			e.rt.semanticManifest = manifest
		}
		e.semantic.sourceResidentMu.Unlock()
		e.rt.mu.Unlock()
		releaseSource()
		if buildErr != nil {
			return nil, manifest, nil, buildErr
		}
		documentsPublished := reservedSource && documentBytes != 0

		// Acquire a lease on the actually published cache, not the local map. An
		// invalidate/rebuild may win the small post-publication gap; in that case
		// discard docs and retry against the new truth generation.
		if err := e.rt.mu.RLockContext(ctx); err != nil {
			docs = nil
			manifest = semanticSourceManifest{}
			if documentsPublished {
				process.releaseSourceDocuments(e)
			}
			return nil, semanticSourceManifest{}, nil, err
		}
		if err := e.semantic.sourceResidentMu.RLockContext(ctx); err != nil {
			e.rt.mu.RUnlock()
			docs = nil
			manifest = semanticSourceManifest{}
			if documentsPublished {
				process.releaseSourceDocuments(e)
			}
			return nil, semanticSourceManifest{}, nil, err
		}
		published := e.rt.semanticManifest
		if e.rt.semanticSourceVersion == version && published.ready && published.errText == "" &&
			published.version == version && published.fingerprint == manifest.fingerprint {
			e.rt.mu.RUnlock()
			lease := &semanticSourceLease{releaseManifest: e.semantic.sourceResidentMu.RUnlock}
			if documentsPublished {
				lease.releaseDocuments = func() { process.releaseSourceDocuments(e) }
			}
			return docs, published, lease, nil
		}
		published = semanticSourceManifest{}
		manifest = semanticSourceManifest{}
		docs = nil
		e.semantic.sourceResidentMu.RUnlock()
		e.rt.mu.RUnlock()
		if documentsPublished {
			process.releaseSourceDocuments(e)
		}
		if err := ctx.Err(); err != nil {
			return nil, semanticSourceManifest{}, nil, err
		}
	}
}

// semanticManifestResidentBytes conservatively accounts for retained map,
// struct, slice-header and string storage. The build reservation is larger
// because raw DTOs and provider text can coexist transiently; promotion keeps
// only this estimated immutable manifest footprint in the daemon-wide ledger.
func semanticManifestResidentBytesContext(ctx context.Context, manifest semanticSourceManifest) (uint64, error) {
	if ctx == nil {
		return 0, fmt.Errorf("semantic manifest estimate: nil context")
	}
	bytes := uint64(4 << 10)
	i := 0
	for recordID, record := range manifest.records {
		if i&63 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		i++
		bytes += 192 + uint64(len(recordID)+len(record.NodeID)+len(record.Kind))
		for _, value := range record.Facets {
			bytes += 16 + uint64(len(value))
		}
		for _, value := range record.References {
			bytes += 16 + uint64(len(value))
		}
	}
	// Leave 25% headroom for allocator size classes and map buckets.
	bytes += bytes / 4
	limit := uint64(semanticSourceBuildMaxMiB) << 20
	if bytes > limit {
		return 0, fmt.Errorf("semantic source manifest 驻留估算 %.1fMiB 超过单次硬上限 %dMiB",
			float64(bytes)/(1<<20), semanticSourceBuildMaxMiB)
	}
	return bytes, nil
}

func semanticDocumentsResidentBytesContext(ctx context.Context, docs []semanticDocument) (uint64, error) {
	if ctx == nil {
		return 0, fmt.Errorf("semantic documents estimate: nil context")
	}
	bytes := uint64(4 << 10)
	for i, doc := range docs {
		if i&63 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		bytes += 160 + uint64(len(doc.RecordID)+len(doc.NodeID)+len(doc.Kind)+len(doc.Text))
		for _, value := range doc.Facets {
			bytes += 16 + uint64(len(value))
		}
		for _, value := range doc.References {
			bytes += 16 + uint64(len(value))
		}
	}
	bytes += bytes / 4
	limit := uint64(semanticSourceBuildMaxMiB) << 20
	if bytes > limit {
		return 0, fmt.Errorf("semantic source documents 驻留估算 %.1fMiB 超过单次硬上限 %dMiB",
			float64(bytes)/(1<<20), semanticSourceBuildMaxMiB)
	}
	return bytes, nil
}

func buildSemanticSource(ctx context.Context, version uint64, rawDocs []semanticRawDocument, includeText bool, sourceErr error) ([]semanticDocument, semanticSourceManifest, error) {
	manifest := semanticSourceManifest{version: version, ready: true}
	if sourceErr != nil {
		manifest.errText = sourceErr.Error()
		return nil, manifest, sourceErr
	}
	docs := make([]semanticDocument, 0, len(rawDocs))
	totalTextBytes := 0
	for i := range rawDocs {
		if i&63 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, semanticSourceManifest{}, err
			}
		}
		doc := makeSemanticDocumentFromRaw(rawDocs[i])
		rawDocs[i] = semanticRawDocument{}
		if len(doc.Text) > semanticMaxSourceBytes-totalTextBytes {
			err := fmt.Errorf("semantic 脱敏知识卡片正文超过硬上限 %d MiB", semanticMaxSourceBytes>>20)
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
	if err := contextHeapSort(ctx, docs, func(a, b semanticDocument) bool { return a.RecordID < b.RecordID }); err != nil {
		return nil, semanticSourceManifest{}, err
	}
	h := sha256.New()
	_, _ = h.Write([]byte(semanticSourcePreprocessVersion + "\x00"))
	manifest.records = make(map[string]semanticSourceRecord, len(docs))
	for i, doc := range docs {
		if i&63 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, semanticSourceManifest{}, err
			}
		}
		writeSemanticHashField(h, doc.RecordID)
		writeSemanticHashField(h, doc.NodeID)
		writeSemanticHashField(h, doc.Kind)
		_, _ = h.Write(doc.SourceHash[:])
		manifest.records[doc.RecordID] = semanticSourceRecord{
			NodeID: doc.NodeID, Kind: doc.Kind, SourceHash: doc.SourceHash,
			Facets: doc.Facets, References: doc.References,
		}
	}
	copy(manifest.fingerprint[:], h.Sum(nil))
	if !includeText {
		docs = nil
	}
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
	return makeSemanticDocumentFromRaw(semanticRawDocument{
		RecordID: recordID, NodeID: nodeID, Kind: kind, Raw: raw,
	})
}

func makeSemanticDocumentFromRaw(raw semanticRawDocument) semanticDocument {
	clean := compactSemanticText(raw.Raw, semanticMaxDocumentRunes)
	text := "代码知识节点: " + raw.NodeID + "\n知识通道: " + raw.Kind + "\n知识卡片:\n" + clean
	// 对最终 provider 文本整体脱敏；node ID/文件名同样可能含 token、URL
	// credential 等模式，不能只处理知识正文。
	text, _ = RedactText(text)
	text = compactSemanticTextBytes(text, semanticMaxDocumentBytes)
	return semanticDocument{
		RecordID: raw.RecordID, NodeID: raw.NodeID, Kind: raw.Kind, Text: text,
		SourceHash: sha256.Sum256([]byte(semanticDocumentPreprocessVersion + "\x00" + text)),
		Facets:     append([]string(nil), raw.Facets...), References: append([]string(nil), raw.References...),
	}
}

func renderSemanticCardChunksContext(ctx context.Context, nodeID, lane string, items []semanticCardItem) ([]semanticRawDocument, error) {
	if ctx == nil {
		return nil, fmt.Errorf("semantic card render: nil context")
	}
	if len(items) == 0 {
		return nil, ctx.Err()
	}
	if err := contextHeapSort(ctx, items, func(a, b semanticCardItem) bool {
		if a.facet != b.facet {
			return a.facet < b.facet
		}
		if a.reference != b.reference {
			return a.reference < b.reference
		}
		return a.text < b.text
	}); err != nil {
		return nil, err
	}
	// 精确去重：同一 flow step 或重复 journal node 声明不能浪费卡片预算。
	unique := items[:0]
	for i, item := range items {
		if i&63 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if len(unique) > 0 {
			last := unique[len(unique)-1]
			if item.facet == last.facet && item.reference == last.reference && item.text == last.text {
				continue
			}
		}
		unique = append(unique, item)
	}
	var out []semanticRawDocument
	var lines []string
	facets, refs := map[string]bool{}, map[string]bool{}
	bytes := 0
	flush := func() error {
		if len(lines) == 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		index := len(out)
		facetValues, err := sortedSemanticSetContext(ctx, facets)
		if err != nil {
			return err
		}
		refValues, err := sortedSemanticSetContext(ctx, refs)
		if err != nil {
			return err
		}
		out = append(out, semanticRawDocument{
			RecordID: fmt.Sprintf("card:%s:%s:%04d", lane, nodeID, index),
			NodeID:   nodeID, Kind: lane, Raw: strings.Join(lines, "\n"),
			Facets: facetValues, References: refValues,
		})
		lines, facets, refs, bytes = nil, map[string]bool{}, map[string]bool{}, 0
		return nil
	}
	for i, item := range unique {
		if i&63 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		line := compactSemanticTextBytes(strings.TrimSpace(item.text), semanticCardRawTarget)
		if len(lines) > 0 && bytes+1+len(line) > semanticCardRawTarget {
			if err := flush(); err != nil {
				return nil, err
			}
		}
		lines = append(lines, line)
		bytes += len(line) + 1
		facets[item.facet] = true
		if item.reference != "" {
			refs[item.reference] = true
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return out, ctx.Err()
}

func sortedSemanticCardNodesContext(ctx context.Context, cards map[string]map[string][]semanticCardItem) ([]string, error) {
	ids := make([]string, 0, len(cards))
	for id := range cards {
		if len(ids)&63 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		ids = append(ids, id)
	}
	if err := contextSortStrings(ctx, ids); err != nil {
		return nil, err
	}
	return ids, nil
}

func sortedSemanticSetContext(ctx context.Context, values map[string]bool) ([]string, error) {
	out := make([]string, 0, len(values))
	for value := range values {
		if len(out)&63 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if value != "" {
			out = append(out, value)
		}
	}
	if err := contextSortStrings(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// semanticFlowsFingerprint 让未进入 store.Cache 的 flows 外部编辑也能使
// semantic manifest 换代；只哈希知识文本，不保存或输出正文。
func semanticFlowsFingerprint(flows []model.Flow) [32]byte {
	h := sha256.New()
	_, _ = h.Write([]byte("iknowledge-semantic-flows-v1\x00"))
	// Store.LoadFlows publishes an ID-sorted immutable slice. Hash it directly:
	// copying the complete shape here ran before the source-build reservation and
	// multiplied memory across repositories without changing determinism.
	for _, flow := range flows {
		writeSemanticHashField(h, flow.ID)
		writeSemanticHashField(h, flow.Title)
		writeSemanticHashField(h, flow.Troubleshoot)
		writeSemanticHashField(h, semanticFingerprintTime(flow.Since))
		if flow.Deprecated {
			writeSemanticHashField(h, "deprecated")
		}
		for _, convention := range flow.Conventions {
			writeSemanticHashField(h, convention)
		}
		for _, step := range flow.Steps {
			writeSemanticHashField(h, step.Node)
			writeSemanticHashField(h, step.Note)
			writeSemanticHashField(h, semanticFingerprintTime(step.Since))
		}
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

func semanticFingerprintTime(value time.Time) string {
	if value.IsZero() {
		return "<zero>"
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func compactSemanticTextBytes(text string, maxBytes int) string {
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "\uFFFD")
	}
	if maxBytes <= 0 || len(text) <= maxBytes {
		return text
	}
	const marker = "\n[…知识卡片过长，保留首尾…]\n"
	if maxBytes <= len(marker)+1 {
		end := maxBytes
		for end > 0 && !utf8.ValidString(text[:end]) {
			end--
		}
		return strings.TrimSpace(text[:end])
	}
	budget := maxBytes - len(marker)
	// The tail carries reasons, rebuttals and rejected-alternative rationale,
	// so reserve half the bounded payload for it instead of silently retaining
	// only an option/change prefix.
	headBytes := budget / 2
	tailBytes := budget - headBytes
	for headBytes > 0 && !utf8.ValidString(text[:headBytes]) {
		headBytes--
	}
	tailStart := len(text) - tailBytes
	for tailStart < len(text) && !utf8.ValidString(text[tailStart:]) {
		tailStart++
	}
	return strings.TrimSpace(text[:headBytes]) + marker + strings.TrimSpace(text[tailStart:])
}

func compactSemanticText(text string, maxRunes int) string {
	text = strings.TrimSpace(text)
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "\uFFFD")
	}
	runeCount := utf8.RuneCountInString(text)
	if maxRunes <= 0 || runeCount <= maxRunes {
		return text
	}
	const marker = "\n[…知识卡片过长，保留首尾…]\n"
	budget := maxRunes - utf8.RuneCountInString(marker)
	if budget < 2 {
		return text[:semanticByteOffsetAfterRunes(text, maxRunes)]
	}
	tail := budget / 4
	head := budget - tail
	headEnd := semanticByteOffsetAfterRunes(text, head)
	tailStart := len(text)
	for range tail {
		_, size := utf8.DecodeLastRuneInString(text[:tailStart])
		tailStart -= size
	}
	return strings.TrimSpace(text[:headEnd]) + marker + strings.TrimSpace(text[tailStart:])
}

func semanticByteOffsetAfterRunes(text string, count int) int {
	offset := 0
	for range count {
		_, size := utf8.DecodeRuneInString(text[offset:])
		offset += size
	}
	return offset
}
