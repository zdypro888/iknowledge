package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/zdypro888/iknowledge/internal/model"
)

// ---- kb_task(impl §7.3;knowledge.md §7 任务态层) ----

// TaskArgs 是 kb_task 入参。
type TaskArgs struct {
	Action string    `json:"action"` // start | update | complete | abandon | get
	WIP    model.WIP `json:"wip"`
	// Owner/Reason 只用于代为收口另一会话明确存在且已过期的 WIP。
	// 普通会话操作不接受 AI 自报 owner，仍由 sid+author 唯一确定。
	Owner  string `json:"owner,omitempty"`
	Reason string `json:"reason,omitempty" redact:"true"`
}

const crossSessionWIPStaleAfter = 7 * 24 * time.Hour

// Task 任务态读写:与知识层严格分离,归档时压缩成变更记录。
func (e *Engine) Task(a TaskArgs, sid, author string) (out string, err error) {
	return e.TaskContext(context.Background(), a, sid, author)
}

// TaskContext adds a historical decision advisory to task start while keeping
// the original Task API for non-MCP callers. The advisory never blocks: an
// unavailable provider never prevents WIP creation, but request cancellation
// still stops the operation before it mutates task state.
func (e *Engine) TaskContext(ctx context.Context, a TaskArgs, sid, author string) (out string, err error) {
	redaction := RedactSecrets(&a)
	// complete 还会把磁盘里的旧 WIP 压缩进 Change；它可能来自尚未实行
	// 写入脱敏的旧版本，因此最终 redaction report 要到归档对象复扫后再取值。
	defer func() { appendRedactionNotice(&out, &err, redaction) }()
	if ctx == nil {
		return "", fmt.Errorf("kb_task: nil context")
	}
	if err := e.requireInit(); err != nil {
		return "", err
	}
	a.Owner = strings.TrimSpace(a.Owner)
	a.Reason = strings.TrimSpace(a.Reason)
	switch a.Action {
	case "start":
		if strings.TrimSpace(a.WIP.Task) == "" {
			return "", kbErr("INVALID_ARGUMENT", "start 需要 wip.task", "给任务一句话描述")
		}
	case "update", "complete", "abandon", "get":
	default:
		return "", kbErr("INVALID_ARGUMENT", "非法 action "+a.Action, "action ∈ start|update|complete|abandon|get")
	}
	owner := taskOwner(sid, author)
	if a.Owner != "" && a.Action != "complete" && a.Action != "abandon" {
		return "", kbErr("INVALID_ARGUMENT", "owner 仅可用于 complete/abandon 代为收口 stale WIP", "普通操作省略 owner；先 kb_task get 获取精确 owner")
	}
	// owner 是一个明确的越权目标参数，而不是请求方可以用来自报身份
	// 的字段。即使它恰好等于服务端推导的当前 owner，也必须走 stale
	// + reason 闸门；当前会话的普通收口必须完全省略 owner。
	explicitStaleClosure := a.Owner != ""
	if !explicitStaleClosure && a.Reason != "" {
		return "", kbErr("INVALID_ARGUMENT", "reason 仅用于显式 owner 的 stale WIP 收口", "当前会话 complete/abandon 省略 owner 和 reason")
	}
	if explicitStaleClosure {
		if a.Reason == "" {
			return "", kbErr("INVALID_ARGUMENT", "代为收口 stale WIP 必须提供 reason", "写明 commit/部署/验收证据，或说明任务为何已取消/被取代")
		}
		// 拒绝路径必须零写入：在 SyncContext（可能恢复崩溃事务或对账）前，
		// 先从磁盘只读确认 owner 精确存在且已满 7 天。通过后仍会在写锁内
		// 二次校验，封住并发 update/complete 的 TOCTOU 窗口。
		wips, loadErr := e.Store.LoadWIPs()
		if loadErr != nil {
			return "", loadErr
		}
		if err := validateCrossSessionWIP(wipByOwner(wips, a.Owner), a.Owner, e.now()); err != nil {
			return "", err
		}
	}
	if err := e.SyncContext(ctx); err != nil {
		return "", err
	}
	var touchingWarnings []string
	if (a.Action == "start" || a.Action == "update") && a.WIP.Touching != nil {
		if err := e.rt.mu.RLockContext(ctx); err != nil {
			return "", err
		}
		normalized, warns, normalizeErr := e.normalizeTaskTouchingLocked(a.WIP.Touching)
		e.rt.mu.RUnlock()
		if normalizeErr != nil {
			return "", normalizeErr
		}
		a.WIP.Touching = normalized
		touchingWarnings = warns
	}
	decisionWarning := ""
	if a.Action == "start" {
		decisionWarning = e.semanticDecisionAdvisory(ctx, taskDecisionQuery(a.WIP), a.WIP.Touching)
		if err := ctx.Err(); err != nil {
			return "", err
		}
	}
	if err := e.rt.mu.LockContext(ctx); err != nil {
		return "", err
	}
	defer e.rt.mu.Unlock()
	// Cancellation can arrive while waiting for the truth-state writer lock.
	// Recheck after acquisition so a disconnected MCP request cannot mutate WIP
	// merely because it had passed the earlier provider-boundary check.
	if err := ctx.Err(); err != nil {
		return "", err
	}
	switch a.Action {
	case "start":
		w := a.WIP
		w.Owner = owner // owner = 会话唯一键(服务端定,防冒名/同客户端多会话覆盖)
		w.Updated = e.now().UTC()
		if err := e.Store.SaveWIP(w); err != nil {
			return "", err
		}
		if err := e.reloadLocked(); err != nil {
			return "", err
		}
		out := "wip 已建立(owner " + owner + ")。touching 声明的节点在他人 recall/map 时会自动附带此台账。" + decisionWarning
		return appendTaskTouchingWarnings(out, touchingWarnings), nil

	case "update":
		cur := e.wipForSessionLocked(owner, author)
		if cur == nil {
			return "", kbErr("NODE_NOT_FOUND", "没有你的活跃 wip", "先 kb_task start")
		}
		var sessionBefore *model.WIP
		if existing := e.wipOfLocked(owner); existing != nil {
			copy := *existing
			sessionBefore = &copy
		}
		merged := mergeWIP(*cur, a.WIP)
		merged.Owner = owner
		merged.Updated = e.now().UTC()
		if err := e.Store.SaveWIP(merged); err != nil {
			reloadErr := e.reloadLocked()
			return "", errors.Join(err, reloadErr)
		}
		// 升级前按 author 命名的 WIP 迁到 session 唯一键。即使上一次迁移
		// 半途留下了 session+legacy 两份，也在本次成功更新时收敛。
		if legacy := e.wipOfLocked(author); author != owner && legacy != nil {
			legacyCopy := *legacy
			if err := e.Store.ClearWIP(author); err != nil {
				legacyRestoreErr := e.Store.SaveWIP(legacyCopy)
				var sessionRollbackErr error
				if sessionBefore != nil {
					sessionRollbackErr = e.Store.SaveWIP(*sessionBefore)
				} else {
					sessionRollbackErr = e.Store.ClearWIP(owner)
				}
				reloadErr := e.reloadLocked()
				return "", errors.Join(fmt.Errorf("迁移 legacy WIP: %w", err), legacyRestoreErr, sessionRollbackErr, reloadErr)
			}
		}
		if err := e.reloadLocked(); err != nil {
			return "", err
		}
		return appendTaskTouchingWarnings("wip 已更新", touchingWarnings), nil

	case "complete":
		cur := e.wipForSessionLocked(owner, author)
		if explicitStaleClosure {
			cur = e.wipOfLocked(a.Owner) // 精确 owner；禁止 legacy/session 猜测。
			if err := validateCrossSessionWIP(cur, a.Owner, e.now()); err != nil {
				return "", err
			}
		} else if cur == nil {
			return "", kbErr("NODE_NOT_FOUND", "没有你的活跃 wip", "先 kb_task start")
		}
		// 归档为变更记录(§7 生命周期第 3 条:半成品状态绝不留在知识层)。
		nodes, archiveWarnings := e.resolveTaskArchiveNodesLocked(cur.Touching)
		// 解析 touching 可能需要读取/解析多个源文件。取锁后的早期
		// 取消检查不能覆盖这个窗口；在生成 ID/WAL/首个写入前再次 fail closed。
		if err := ctx.Err(); err != nil {
			return "", err
		}
		chID := e.freshChangeIDLocked()
		what := "完成任务:" + cur.Task
		if len(cur.Done) > 0 {
			what += "(" + strings.Join(cur.Done, ";") + ")"
		}
		why := cur.Intent
		if why == "" {
			why = "任务态归档(kb_task complete)"
		}
		change := model.Change{
			ID: chID, Nodes: nodes, At: e.now().UTC(),
			Task: cur.Task, What: what, Why: why, Author: author, EffectsVersion: 1,
		}
		if explicitStaleClosure {
			// 代为 complete 必须把“为什么有资格宣称完成”持久化；只把理由
			// 留在易失的 MCP 调用参数里会让 journal 失去可审计性。
			change.Verified = "跨会话 stale WIP 收口依据(owner=" + cur.Owner +
				", closed_by=" + owner + "): " + a.Reason
		}
		// a 在入口处只覆盖请求内容。旧 WIP 的 Task/Intent/Done/Owner
		// 从磁盘读入，必须在它们进入 Git 跟踪的 journal 前对完整 Change
		// 再扫一次，防止仓外 WIP 里的历史凭据被提升到团队真相。
		archivedRedaction := RedactSecrets(&change)
		redaction.Count += archivedRedaction.Count
		for _, kind := range archivedRedaction.Kinds {
			redaction.Kinds = appendUnique(redaction.Kinds, kind)
		}
		sort.Strings(redaction.Kinds)
		// WIP 删除与 journal 追加是一个崩溃事务；prepared intent 在 ClearWIP
		// 前同时收录两者，进程在任一写后退出都会于下次 reload 全量恢复。
		tx, err := e.prepareTruthTransactionLocked(map[string]bool{
			e.Store.WIPRelFor(cur.Owner):  true,
			e.Store.JournalRelFor(change): true,
		})
		if err != nil {
			return "", err
		}
		defer e.guardTruthTransactionPanicLocked(tx)
		rollback := func(cause error) (string, error) {
			return "", e.rollbackTruthTransactionLocked(tx, cause)
		}
		if err := e.Store.ClearWIP(cur.Owner); err != nil {
			return rollback(fmt.Errorf("task complete 清除 WIP: %w", err))
		}
		if err := e.Store.AppendChange(change); err != nil {
			committed := false
			if changes, _, loadErr := e.Store.LoadJournal(); loadErr == nil {
				for _, c := range changes {
					if c.ID == change.ID {
						committed = true
						break
					}
				}
			}
			if !committed {
				return rollback(fmt.Errorf("task complete 追加 journal: %w", err))
			}
		}
		committed, commitErr := e.commitTruthTransactionLocked(tx)
		if !committed {
			return rollback(fmt.Errorf("task complete 写 committed marker: %w", commitErr))
		}
		if commitErr != nil {
			return "", fmt.Errorf("task complete 已提交但 WAL 清理/重载失败(不要重试): %w", commitErr)
		}
		out := "已归档为变更记录 " + chID + ",wip 清空。"
		if explicitStaleClosure {
			out = "已代为收口 stale wip owner " + cur.Owner + "；" + out
		}
		// 任务尾偿还(§12.2 第 3 条,≤3 条,仅本任务读过的)+ 沉淀提醒(§9.3)。
		if debts := e.sessionSuspectsLocked(sid); len(debts) > 0 {
			out += "\n任务尾偿还(≤3 条,本任务读过且仍待重验):" + strings.Join(debts, "、") + "(kb_verify)"
		}
		if remind := e.settleReminder(sid); len(remind) > 0 {
			out += "\n沉淀提醒:多次读取未沉淀的节点:" + strings.Join(remind, "、") + "(kb_remember)"
		}
		if md := e.computeDebtsLocked(); len(md) > 0 {
			n := min(2, len(md))
			out += fmt.Sprintf("\n顺手维护(≤2 条,§12.7):%d 条欠账,kb_maintain next 取用", n)
		}
		return appendTaskTouchingWarnings(out, archiveWarnings), nil

	case "abandon":
		cur := e.wipForSessionLocked(owner, author)
		if explicitStaleClosure {
			cur = e.wipOfLocked(a.Owner) // 精确 owner；禁止 legacy/session 猜测。
			if err := validateCrossSessionWIP(cur, a.Owner, e.now()); err != nil {
				return "", err
			}
		} else if cur == nil {
			return "", kbErr("NODE_NOT_FOUND", "没有你的活跃 wip", "无需 abandon；用 kb_task get 查看其他会话台账")
		}
		// 取消的任务不是已完成的代码事实，因此明确删除纯本地
		// WIP，绝不伪造 journal 变更记录。它是用户/代理显式动作，
		// 而非 TTL 自动清理，所以不会误删仍在进行的长任务。
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := e.Store.ClearWIP(cur.Owner); err != nil {
			return "", err
		}
		if err := e.reloadLocked(); err != nil {
			return "", err
		}
		if explicitStaleClosure {
			return "stale wip owner " + cur.Owner + " 已代为显式放弃并清空；未写入知识层或 journal。", nil
		}
		return "wip 已显式放弃并清空；未写入知识层或 journal。", nil

	case "get":
		if len(e.rt.wips) == 0 {
			return "无活跃 wip。", nil
		}
		var b strings.Builder
		for _, w := range e.rt.wips {
			fmt.Fprintf(&b, "[%s] %s(更新 %s)\n  intent: %s\n  plan: %v\n  done: %v\n  todo: %v\n  touching: %v\n",
				w.Owner, w.Task, w.Updated.Format("01-02 15:04"), w.Intent, w.Plan, w.Done, w.Todo, w.Touching)
		}
		return strings.TrimRight(b.String(), "\n"), nil
	}
	return "", kbErr("INVALID_ARGUMENT", "非法 action "+a.Action, "action ∈ start|update|complete|abandon|get")
}

func wipByOwner(wips []model.WIP, owner string) *model.WIP {
	for i := range wips {
		if wips[i].Owner == owner {
			return &wips[i]
		}
	}
	return nil
}

// validateCrossSessionWIP 是代收口的 fail-closed 闸门。Updated=zero 只说明
// 年龄未知，不足以证明 stale；未来时间同样按 recent 拒绝。原 owner 自己的
// complete/abandon 不走本闸门，保持原语义。
func validateCrossSessionWIP(w *model.WIP, owner string, now time.Time) error {
	if w == nil {
		return kbErr("NODE_NOT_FOUND", "指定 owner 的 wip 不存在: "+owner, "先 kb_task get 获取精确 owner，不得猜测")
	}
	if w.Updated.IsZero() {
		return kbErr("INVALID_ARGUMENT", "目标 wip 的更新时间未知，无法证明已 stale 7 天: "+owner, "由原 owner 自行 complete/abandon")
	}
	age := now.Sub(w.Updated)
	if age < crossSessionWIPStaleAfter {
		return kbErr("INVALID_ARGUMENT",
			fmt.Sprintf("目标 wip 尚未 stale 7 天: %s(updated %s)", owner, w.Updated.UTC().Format(time.RFC3339)),
			"不得代收口活跃任务；由原 owner 操作或等待满 7 天")
	}
	return nil
}

func appendTaskTouchingWarnings(out string, warnings []string) string {
	for _, warning := range warnings {
		out += "\n⚠ " + warning
	}
	return out
}

func taskDecisionQuery(w model.WIP) string {
	parts := []string{w.Task, w.Intent}
	parts = append(parts, w.Plan...)
	parts = append(parts, w.Todo...)
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

const (
	semanticAdvisoryMaxNeighborScope = 100
	semanticAdvisoryMaxExactWarnings = 20
	semanticAdvisoryMaxOtherWarnings = 6
)

// semanticDecisionAdvisory compares a proposed task with risk/history cards.
// Similarity is discovery only: it never blocks, never automatically overturns
// a decision, and always returns truth-graph references for exact review.
func (e *Engine) semanticDecisionAdvisory(ctx context.Context, query string, touching []string) string {
	if strings.TrimSpace(query) == "" {
		return ""
	}
	candidates, semanticWarning := e.semanticCandidates(ctx, query)
	statusNote := ""
	if semanticWarning != "" {
		statusNote = "\n⚠ 历史决策提醒状态: " + semanticWarning + "；任务不会因此阻断，本次提醒可能不完整。"
	}
	if err := e.rt.mu.RLockContext(ctx); err != nil {
		// TaskContext checks ctx immediately after this advisory helper returns and
		// propagates cancellation before any WIP mutation.
		return ""
	}
	defer e.rt.mu.RUnlock()
	// Exact lexical risk is a deterministic, zero-model safety net. Semantic
	// similarity broadens discovery, but disabling or losing the embedding
	// provider must not disable warnings for an explicitly named pitfall.
	lexicalRisk := e.rt.ix.SearchRisk(query, 100)
	var risk, history []semanticEvidence
	manifest, manifestErr := e.semanticManifestLocked()
	if len(candidates.risk)+len(candidates.history) > 0 {
		if manifestErr != nil {
			if statusNote == "" {
				statusNote = "\n⚠ 历史决策提醒状态: " + manifestErr.Error() + "；任务不会因此阻断，本次提醒可能不完整。"
			}
		} else {
			// Preserve the complete configured Top-K until touching/one-hop priority is
			// known. Truncating each lane first can hide a directly touched risk behind
			// several slightly more similar unrelated nodes.
			risk = e.semanticEvidenceLocked(manifest, candidates.risk, semanticLaneRisk, 0)
			history = e.semanticEvidenceLocked(manifest, candidates.history, semanticLaneHistory, 0)
		}
	}
	risk = e.mergeLexicalRiskEvidenceLocked(risk, lexicalRisk, 0)

	scope := map[string]bool{}
	exactScope := map[string]bool{}
	var roots []string
	for _, input := range touching {
		// A historical symbol may have split into several current heirs. Every
		// heir is directly in scope; choosing only ResolveNodeID's first result can
		// hide a warning attached to the second branch under the output cap.
		for _, id := range e.rt.ix.ResolveNodeIDs(input) {
			if id == "" || scope[id] {
				continue
			}
			scope[id] = true
			exactScope[id] = true
			roots = append(roots, id)
		}
	}
	neighbors := e.structuralNeighborsLocked(roots, semanticAdvisoryMaxNeighborScope+1)
	if len(neighbors) > semanticAdvisoryMaxNeighborScope {
		neighbors = neighbors[:semanticAdvisoryMaxNeighborScope]
		statusNote += "\n⚠ 结构一跳范围超过 100 个节点；已检查优先级最高的 100 个，touching 精确节点仍全部检查，其余结构告警可能不完整。"
	}
	for _, neighbor := range neighbors {
		scope[neighbor.id] = true
	}
	// Top-K is a discovery budget, never a safety boundary. Supplement every
	// typed risk/history card attached to touching or one-hop nodes from the
	// current manifest, then independently inspect exact truth for live risks.
	// This also covers an old symbol that split into multiple heirs even when a
	// lower-scoring heir fell outside semantic Top-K or lexical limit 100.
	if manifestErr == nil {
		risk = e.mergeScopedManifestEvidenceLocked(risk, manifest, scope, semanticLaneRisk)
		history = e.mergeScopedManifestEvidenceLocked(history, manifest, scope, semanticLaneHistory)
	}
	risk = e.mergeScopedTruthRiskEvidenceLocked(risk, scope, exactScope)
	if len(risk)+len(history) == 0 {
		return statusNote
	}
	type warning struct {
		semanticEvidence
		proximity int // 2=touching exact, 1=structural one-hop, 0=semantic/lexical only
	}
	ordered := make([]warning, 0, len(risk)+len(history))
	for _, evidence := range append(append([]semanticEvidence(nil), risk...), history...) {
		proximity := 0
		if exactScope[evidence.nodeID] {
			proximity = 2
		} else if scope[evidence.nodeID] {
			proximity = 1
		}
		ordered = append(ordered, warning{semanticEvidence: evidence, proximity: proximity})
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].proximity != ordered[j].proximity {
			return ordered[i].proximity > ordered[j].proximity
		}
		if ordered[i].lane != ordered[j].lane {
			return ordered[i].lane == semanticLaneRisk
		}
		if left, right := evidenceBestRank(ordered[i].semanticEvidence), evidenceBestRank(ordered[j].semanticEvidence); left != right {
			return left < right
		}
		if ordered[i].score != ordered[j].score {
			return ordered[i].score > ordered[j].score
		}
		return ordered[i].nodeID < ordered[j].nodeID
	})
	selected := make([]warning, 0, semanticAdvisoryMaxExactWarnings+semanticAdvisoryMaxOtherWarnings)
	exactN, otherN, omittedExact, omittedOther := 0, 0, 0, 0
	omittedExactNodes := map[string]bool{}
	for _, item := range ordered {
		if item.proximity == 2 {
			if exactN >= semanticAdvisoryMaxExactWarnings {
				omittedExact++
				omittedExactNodes[item.nodeID] = true
				continue
			}
			exactN++
			selected = append(selected, item)
			continue
		}
		if otherN >= semanticAdvisoryMaxOtherWarnings {
			omittedOther++
			continue
		}
		otherN++
		selected = append(selected, item)
	}
	ordered = selected
	var b strings.Builder
	b.WriteString("\n⚠ 历史决策提醒（仅供参考，不阻断；相似不等于裁决）:")
	for _, item := range ordered {
		label := "历史"
		if item.lane == semanticLaneRisk {
			label = "风险"
		}
		proximity := "语义相关"
		switch item.proximity {
		case 2:
			proximity = "touching精确"
		case 1:
			proximity = "结构一跳"
		}
		fmt.Fprintf(&b, "\n- [%s·%s] %s", label, proximity, item.nodeID)
		if item.lexicalRank > 0 {
			fmt.Fprintf(&b, " keyword-risk rank=%d score=%d", item.lexicalRank, item.lexicalScore)
		}
		if item.rank > 0 {
			fmt.Fprintf(&b, " cosine=%.3f", item.score)
		}
		if len(item.facets) > 0 {
			fmt.Fprintf(&b, " facets=%s", strings.Join(item.facets, ","))
		}
		if len(item.references) > 0 {
			fmt.Fprintf(&b, " refs=%s", strings.Join(item.references, ","))
		}
	}
	if omittedExact > 0 {
		omittedIDs := make([]string, 0, len(omittedExactNodes))
		for nodeID := range omittedExactNodes {
			omittedIDs = append(omittedIDs, nodeID)
		}
		sort.Strings(omittedIDs)
		fmt.Fprintf(&b, "\n- ⚠ 另有 %d 条 touching 精确风险/历史因输出预算未逐条展开；这不是安全通过。被省略的当前节点 ID: %s；请对这些完整 ID 逐一 kb_recall(mode=history)。", omittedExact, strings.Join(omittedIDs, "、"))
	}
	if omittedOther > 0 {
		fmt.Fprintf(&b, "\n- 另有 %d 条结构/语义线索因输出预算未展开。", omittedOther)
	}
	b.WriteString("\n动手前用 kb_recall(node,mode=history) 核对引用和当前源码；历史卡片不得当成当前结论。")
	b.WriteString(statusNote)
	return b.String()
}

func (e *Engine) mergeScopedManifestEvidenceLocked(in []semanticEvidence, manifest semanticSourceManifest, scope map[string]bool, lane string) []semanticEvidence {
	out := append([]semanticEvidence(nil), in...)
	byNode := make(map[string]int, len(out))
	for i := range out {
		if out[i].lane == lane {
			byNode[out[i].nodeID] = i
		}
	}
	recordIDs := make([]string, 0, len(manifest.records))
	for recordID := range manifest.records {
		recordIDs = append(recordIDs, recordID)
	}
	sort.Strings(recordIDs)
	for _, recordID := range recordIDs {
		record := manifest.records[recordID]
		if record.Kind != lane || !scope[record.NodeID] || e.rt.ix.Node(record.NodeID) == nil {
			continue
		}
		idx, ok := byNode[record.NodeID]
		if !ok {
			idx = len(out)
			byNode[record.NodeID] = idx
			out = append(out, semanticEvidence{nodeID: record.NodeID, lane: lane, recordID: recordID})
		}
		for _, facet := range record.Facets {
			out[idx].facets = appendUnique(out[idx].facets, facet)
		}
		for _, reference := range record.References {
			out[idx].references = appendUnique(out[idx].references, reference)
		}
	}
	return out
}

// mergeScopedTruthRiskEvidenceLocked is the model-free core of the decision
// advisory. Directly touched/adjacent code with a live pitfall, suspect state,
// pending anchor, orphan, or open dispute always emits a warning, even if the
// task wording shares no lexical token and no embedding provider is configured.
func (e *Engine) mergeScopedTruthRiskEvidenceLocked(in []semanticEvidence, scope, exactScope map[string]bool) []semanticEvidence {
	out := append([]semanticEvidence(nil), in...)
	byNode := make(map[string]int, len(out))
	for i := range out {
		byNode[out[i].nodeID] = i
	}
	nodeIDs := make([]string, 0, len(scope))
	for nodeID := range scope {
		nodeIDs = append(nodeIDs, nodeID)
	}
	sort.Strings(nodeIDs)
	inactive, inactiveErr := inactiveChanges(e.rt.ix.Changes())
	for _, nodeID := range nodeIDs {
		ref := e.rt.ix.Node(nodeID)
		if ref == nil || ref.Node == nil {
			continue
		}
		n := ref.Node
		facets := []string{"structural_one_hop"}
		if exactScope[nodeID] {
			facets[0] = "touching_exact"
		}
		var references []string
		hasRisk := false
		switch n.Status {
		case model.StatusSuspect:
			hasRisk = true
			facets = appendUnique(facets, "node_suspect")
		case model.StatusOrphaned:
			hasRisk = true
			facets = appendUnique(facets, "node_orphaned")
		}
		if n.PendingAnchor {
			hasRisk = true
			facets = appendUnique(facets, "pending_anchor")
		}
		for i := range n.Entries {
			entry := &n.Entries[i]
			if !entry.Active() {
				continue
			}
			entryRef := nodeID + "#" + entry.ID
			if entry.Kind == model.KindPitfall {
				hasRisk = true
				facets = appendUnique(facets, "pitfall")
				references = appendUnique(references, entryRef)
			}
			if entry.Confidence == model.ConfidenceSuspect {
				hasRisk = true
				facets = appendUnique(facets, "suspect_confidence")
				references = appendUnique(references, entryRef)
			}
			for _, targetRef := range entry.Disputes {
				resolved := e.rt.ix.ResolveEntryRef(targetRef)
				if target := e.rt.ix.EntryByRef(resolved); target != nil && target.Active() {
					hasRisk = true
					facets = appendUnique(facets, "open_dispute")
					references = appendUnique(references, entryRef)
					references = appendUnique(references, resolved)
				}
			}
			for _, sourceRef := range e.rt.ix.DisputedBy(entryRef) {
				if source := e.rt.ix.EntryByRef(sourceRef); source != nil && source.Active() {
					hasRisk = true
					facets = appendUnique(facets, "open_dispute")
					references = appendUnique(references, entryRef)
					references = appendUnique(references, sourceRef)
				}
			}
		}
		for _, change := range e.rt.ix.History(nodeID) {
			if len(change.Rejected) == 0 || (inactiveErr == nil && inactive[change.ID]) {
				continue
			}
			hasRisk = true
			facets = appendUnique(facets, "rejected_active")
			references = appendUnique(references, change.ID)
			if inactiveErr != nil {
				facets = appendUnique(facets, "decision_graph_ambiguous")
			}
		}
		for _, flowID := range e.rt.ix.FlowsOf(nodeID) {
			flow := e.rt.ix.Flow(flowID)
			if flow == nil || flow.Deprecated {
				continue
			}
			if strings.TrimSpace(flow.Troubleshoot) != "" {
				hasRisk = true
				facets = appendUnique(facets, "flow_troubleshoot")
				references = appendUnique(references, flowID)
			}
			if n.Status == model.StatusOrphaned || n.Status == model.StatusSuspect || n.PendingAnchor {
				hasRisk = true
				facets = appendUnique(facets, "stale_flow")
				references = appendUnique(references, flowID)
			}
		}
		if !hasRisk {
			continue
		}
		idx, ok := byNode[nodeID]
		if !ok {
			idx = len(out)
			byNode[nodeID] = idx
			out = append(out, semanticEvidence{nodeID: nodeID, lane: semanticLaneRisk})
		}
		for _, facet := range facets {
			out[idx].facets = appendUnique(out[idx].facets, facet)
		}
		for _, reference := range references {
			out[idx].references = appendUnique(out[idx].references, reference)
		}
	}
	return out
}

func (e *Engine) wipOfLocked(owner string) *model.WIP {
	for i := range e.rt.wips {
		if e.rt.wips[i].Owner == owner {
			return &e.rt.wips[i]
		}
	}
	return nil
}

// taskOwner 用 MCP session 区分同一客户端的并行任务。clientInfo.name 只表示
// "codex"/"claude-code",不是会话 ID;直接当 owner 会让后一个 start 覆盖前一个。
// 匿名/旧客户端没有 sid 时退化为 author。author 仍保留在前缀,展示可溯源。
func taskOwner(sid, author string) string {
	if strings.TrimSpace(sid) == "" {
		return author
	}
	return author + "@" + sid
}

// wipForSessionLocked 优先取会话唯一台账;legacyOwner 兼容升级前按 author
// 命名的本地 WIP,首次 update 后会迁到新键。
func (e *Engine) wipForSessionLocked(owner, legacyOwner string) *model.WIP {
	if w := e.wipOfLocked(owner); w != nil {
		return w
	}
	if legacyOwner != owner {
		return e.wipOfLocked(legacyOwner)
	}
	return nil
}

// mergeWIP 提供了字段就替换(update 语义)。
func mergeWIP(cur, in model.WIP) model.WIP {
	if in.Task != "" {
		cur.Task = in.Task
	}
	if in.Intent != "" {
		cur.Intent = in.Intent
	}
	if in.Plan != nil {
		cur.Plan = in.Plan
	}
	if in.Done != nil {
		cur.Done = in.Done
	}
	if in.Todo != nil {
		cur.Todo = in.Todo
	}
	if in.Touching != nil {
		cur.Touching = in.Touching
	}
	return cur
}

// sessionSuspectsLocked 本会话读取过且仍有待重验条目/状态的节点(≤3)。
func (e *Engine) sessionSuspectsLocked(sid string) []string {
	reads := e.ledgerSnapshot(sid)
	if reads == nil {
		return nil
	}
	var out []string
	for id := range reads {
		ref := e.rt.ix.Node(id)
		if ref == nil {
			continue
		}
		bad := ref.Node.Status == model.StatusSuspect
		for i := range ref.Node.Entries {
			if ref.Node.Entries[i].Active() && ref.Node.Entries[i].Confidence == model.ConfidenceSuspect {
				bad = true
			}
		}
		if bad {
			out = append(out, id)
			if len(out) >= 3 {
				break
			}
		}
	}
	return out
}

// ---- kb_flow(impl §7.3;knowledge.md §6 横向维度) ----

// FlowArgs 是 kb_flow 入参。
type FlowArgs struct {
	Action string     `json:"action"` // get | create | update | deprecate
	Flow   model.Flow `json:"flow"`
}

// Flow 流程/主题节点 CRUD;steps 引用的树节点必须存在,反向链接由 index 现算。
func (e *Engine) Flow(a FlowArgs, sid, author string) (out string, err error) {
	redaction := RedactSecrets(&a)
	defer appendRedactionNotice(&out, &err, redaction)
	if err := e.requireInit(); err != nil {
		return "", err
	}
	if err := e.Sync(); err != nil {
		return "", err
	}
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()

	// get 先处理(#5:原先无读动作,update 只能盲写)——ID 空则列全部,否则取详情。
	if a.Action == "get" {
		if strings.TrimSpace(a.Flow.ID) == "" {
			return framed(e.listFlowsLocked()), nil
		}
		if out, ok := e.flowView(a.Flow.ID); ok {
			return framed(out), nil
		}
		return "", kbErr("NODE_NOT_FOUND", a.Flow.ID+" 不存在", "kb_flow get(空 id)列全部")
	}

	if a.Flow.ID == "" || e.Store.FlowPathFor(a.Flow.ID) == "" {
		return "", kbErr("INVALID_ARGUMENT", "非法 flow ID "+a.Flow.ID,
			"格式 flow:name 或 topic:name(name 不含斜杠)")
	}
	existing := e.rt.ix.Flow(a.Flow.ID)

	switch a.Action {
	case "create", "update":
		if a.Action == "create" && existing != nil {
			return "", kbErr("DUPLICATE_ENTRY", a.Flow.ID+" 已存在", "用 update")
		}
		if a.Action == "update" && existing == nil {
			return "", kbErr("NODE_NOT_FOUND", a.Flow.ID+" 不存在", "用 create")
		}
		if a.Flow.Title == "" {
			return "", kbErr("INVALID_ARGUMENT", "flow.title 必填", "给流程一个标题")
		}
		// steps 引用的树节点必须存在(impl §7.3 kb_flow)。
		for _, st := range a.Flow.Steps {
			if e.rt.ix.ResolveNodeID(st.Node) == "" {
				return "", kbErr("NODE_NOT_FOUND", "step 引用的节点 "+st.Node+" 不存在",
					"用 kb_map 核对;节点 ID 相对仓库根")
			}
		}
		// lint 与预算同样适用于流程文本(横向层不是投毒逃逸口)。
		// steps[].note 同受检(R2-E2:它与 title/conventions 一样渲染进 recall/inject)。
		texts := append([]string{a.Flow.Title, a.Flow.Troubleshoot}, a.Flow.Conventions...)
		for _, st := range a.Flow.Steps {
			texts = append(texts, st.Note)
		}
		for _, text := range texts {
			if reject, _ := LintImperative(text); reject != "" {
				return "", kbErr("IMPERATIVE_CONTENT", reject, "改写为事实陈述(knowledge.md §12.8)")
			}
		}
		f := a.Flow
		f.Author = author
		if existing != nil {
			f.Since = existing.Since
			f.Deprecated = existing.Deprecated
			stampFlowStepGenerations(&f, existing, e.now().UTC())
		} else {
			f.Since = e.now().UTC()
			stampFlowStepGenerations(&f, nil, f.Since)
		}
		if err := e.Store.SaveFlow(f); err != nil {
			return "", err
		}
		if err := e.reloadLocked(); err != nil {
			return "", err
		}
		return a.Flow.ID + " 已保存(" + fmt.Sprint(len(a.Flow.Steps)) + " 步;树节点的反向链接由索引现算)", nil

	case "deprecate":
		if existing == nil {
			return "", kbErr("NODE_NOT_FOUND", a.Flow.ID+" 不存在", "kb_recall mode=flow 核对")
		}
		f := *existing
		f.Deprecated = true
		if err := e.Store.SaveFlow(f); err != nil {
			return "", err
		}
		if err := e.reloadLocked(); err != nil {
			return "", err
		}
		return a.Flow.ID + " 已废弃(文件保留可溯,退出注入与反向链接)", nil
	}
	return "", kbErr("INVALID_ARGUMENT", "非法 action "+a.Action, "action ∈ get|create|update|deprecate")
}

func stampFlowStepGenerations(next *model.Flow, previous *model.Flow, now time.Time) {
	byNode := map[string][]time.Time{}
	if previous != nil {
		for _, step := range previous.Steps {
			since := step.Since
			if since.IsZero() {
				since = previous.Since
			}
			byNode[step.Node] = append(byNode[step.Node], since)
		}
	}
	for i := range next.Steps {
		queue := byNode[next.Steps[i].Node]
		if len(queue) > 0 {
			next.Steps[i].Since = queue[0]
			byNode[next.Steps[i].Node] = queue[1:]
		} else {
			next.Steps[i].Since = now
		}
	}
}
