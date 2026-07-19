// Package index 是内存索引(impl §8):倒排关键词、节点表、lineage/supersedes
// 归一化解析、basedOn 反向图、flow 反向链接、journal 按节点反查。
// 文件是真相,索引是缓存,随时可由 store 的内容重建。
package index

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/zdypro888/iknowledge/internal/model"
)

// NodeRef 指向缓存分片里的一个节点。
type NodeRef struct {
	ShardRel string // 相对 .knowledge 的分片路径(tree/….yaml / project.yaml)
	Node     *model.Node
}

// ChangeRef 是 journal 反查的一条:ViaLineage 标记经血缘命中
// (Since 过滤只作用于同 ID 直接命中,血缘命中不受限,impl §7.3)。
type ChangeRef struct {
	Idx        int // changes 切片下标(at 升序)
	ViaLineage bool
}

// Index 由一次快照(shards+journal+flows)构建;只读,重载后整体重建。
type Index struct {
	nodes           map[string]*NodeRef
	lineage         map[string][]string        // 旧节点 ID → 现任节点 ID(拆分可多个,#8)
	inverted        map[string]map[string]bool // token → 节点 ID 集合
	trigram         map[string]map[string]bool // 三字母组 → 节点 ID 集合(R29 批次5:近似命中回退)
	invertedCurrent map[string]map[string]bool // 当前事实/导航 token → 节点 ID
	trigramCurrent  map[string]map[string]bool // 当前事实/导航近似索引
	invertedRisk    map[string]map[string]bool // 风险文本 token → 节点 ID
	trigramRisk     map[string]map[string]bool // 风险文本近似索引
	fileNodes       map[string][]string        // 文件路径 → 该文件的所有节点 ID(R29 批次3:文件域查询免扫全表)
	basedOnRev      map[string][]string        // 归一化 "node#entry" → 依赖它的 "node#entry"
	disputesRev     map[string][]string        // 归一化 "node#entry" → 声明与它矛盾的 "node#entry"
	flowsByNode     map[string][]string        // 节点 ID → flow IDs(反向链接,现算不落盘)
	journalBy       map[string][]ChangeRef     // 现任节点 ID → 变更引用
	landmine        map[string]int             // 轮30-C:节点 ID → 雷区分(变更频次 + 推翻次数×2 + refute 数)
	duplicateIDs    []string                   // 跨分片重复 node ID:隔离,避免 map 遍历 last-write-wins
	changes         []model.Change
	flows           []model.Flow
}

// Build 全量构建索引。shards 是"分片相对路径 → 节点切片",由 engine 从 store
// 快照筛出健康分片传入(conflict/schema 隔离的不进索引)——index 因此不依赖
// store,维持 impl §2 依赖方向。切片共享缓存底层数组,NodeRef 指针指向缓存实体。
func Build(shards map[string][]model.Node, changes []model.Change, flows []model.Flow) *Index {
	ix := &Index{
		nodes:           map[string]*NodeRef{},
		lineage:         map[string][]string{},
		inverted:        map[string]map[string]bool{},
		trigram:         map[string]map[string]bool{},
		invertedCurrent: map[string]map[string]bool{},
		trigramCurrent:  map[string]map[string]bool{},
		invertedRisk:    map[string]map[string]bool{},
		trigramRisk:     map[string]map[string]bool{},
		fileNodes:       map[string][]string{},
		basedOnRev:      map[string][]string{},
		disputesRev:     map[string][]string{},
		flowsByNode:     map[string][]string{},
		journalBy:       map[string][]ChangeRef{},
		landmine:        map[string]int{},
		changes:         changes,
		flows:           flows,
	}

	// 先计数再建表:跨分片重复 ID 不能由 map 迭代顺序随机决定胜者。
	// 全部副本都隔离,由 status 告警人工修复。
	counts := map[string]int{}
	for _, nodes := range shards {
		for i := range nodes {
			if model.SafeNodeID(nodes[i].ID) {
				counts[nodes[i].ID]++
			}
		}
	}
	for id, n := range counts {
		if n > 1 {
			ix.duplicateIDs = append(ix.duplicateIDs, id)
		}
	}
	sort.Strings(ix.duplicateIDs)

	rels := make([]string, 0, len(shards))
	for rel := range shards {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		nodes := shards[rel]
		for i := range nodes {
			n := &nodes[i]
			// 铁律二防线:恶意/损坏分片里带 ../ 的节点 ID 直接丢弃——
			// 读路径由节点 ID 驱动文件读,不消毒会被穿越到仓库外(索引是唯一收口)。
			if !model.SafeNodeID(n.ID) || counts[n.ID] != 1 {
				continue
			}
			ix.nodes[n.ID] = &NodeRef{ShardRel: rel, Node: n}
			// R29 批次3:file→nodes 索引(文件域查询免扫全表:Inject/LooseMatch/missProtocol)。
			if file, _ := model.SplitNodeID(n.ID); file != "" {
				ix.fileNodes[file] = append(ix.fileNodes[file], n.ID)
			}
			for _, oldID := range n.Lineage {
				ix.lineage[oldID] = append(ix.lineage[oldID], n.ID)
			}
		}
	}
	for old := range ix.lineage {
		sort.Strings(ix.lineage[old]) // 确定性:map 迭代序不可信
	}
	entryResolver := ix.NewEntryResolver()
	resolveEntryRef := func(ref string) string {
		resolved, _, err := entryResolver.ResolveContext(context.Background(), ref, 0)
		if err != nil {
			return ref
		}
		return resolved
	}

	// 活跃且双方都未退场的 dispute 是“待裁决”。先算出双方 entry ref，
	// 再建倒排：否则被指方的文本会被当成“当前结论”。
	openDisputeEntries := map[string]bool{}
	for id, ref := range ix.nodes {
		for i := range ref.Node.Entries {
			entry := &ref.Node.Entries[i]
			if !entry.Active() {
				continue
			}
			from := id + "#" + entry.ID
			for _, target := range entry.Disputes {
				resolved, targetEntry, _ := entryResolver.ResolveContext(context.Background(), target, 0)
				if targetEntry == nil || !targetEntry.Active() {
					continue
				}
				openDisputeEntries[from] = true
				openDisputeEntries[resolved] = true
			}
		}
	}

	// 倒排分三份：Search 保留全量旧语义；SearchCurrent 只提供当前事实
	// 和节点导航；SearchRisk 只提供 pitfall/suspect/open-dispute 风险文本。
	// R29 批次5:同时建 trigram 索引(三字母组),供精确 token 命中不足时近似回退。
	for id, ref := range ix.nodes {
		n := ref.Node
		addCurrent := func(tokens []string) {
			addSearchTokens(id, tokens, ix.inverted, ix.trigram)
			addSearchTokens(id, tokens, ix.invertedCurrent, ix.trigramCurrent)
		}
		addRisk := func(tokens []string) {
			addSearchTokens(id, tokens, ix.inverted, ix.trigram)
			addSearchTokens(id, tokens, ix.invertedRisk, ix.trigramRisk)
		}
		file, symbol := model.SplitNodeID(id)
		for seg := range strings.SplitSeq(file, "/") {
			addCurrent(Tokenize(seg))
		}
		if symbol != "" {
			addCurrent(SplitIdent(symbol))
		}
		for _, kw := range n.Keywords {
			addCurrent(Tokenize(kw))
		}
		for i := range n.Entries {
			entry := &n.Entries[i]
			if !entry.Active() {
				continue
			}
			// Orphan knowledge is preserved for adoption/history, but there is no
			// current code anchor against which its body can be treated as either a
			// fact or an actionable live risk. Keep only node identity navigation.
			if n.Status == model.StatusOrphaned {
				continue
			}
			tokens := Tokenize(entry.Text)
			if entry.Kind == model.KindPitfall || n.Status == model.StatusSuspect ||
				n.PendingAnchor || entry.Confidence == model.ConfidenceSuspect || openDisputeEntries[id+"#"+entry.ID] {
				addRisk(tokens)
			} else {
				addCurrent(tokens)
			}
		}
	}

	// basedOn / disputes 反向图:引用永不改写,建图时归一化(impl §8)。
	for id, ref := range ix.nodes {
		for i := range ref.Node.Entries {
			e := &ref.Node.Entries[i]
			from := id + "#" + e.ID
			for _, dep := range e.BasedOn {
				resolved := resolveEntryRef(dep)
				ix.basedOnRev[resolved] = append(ix.basedOnRev[resolved], from)
			}
			for _, d := range e.Disputes {
				resolved := resolveEntryRef(d)
				ix.disputesRev[resolved] = append(ix.disputesRev[resolved], from)
			}
		}
	}

	// flow 反向链接(现算,不落盘)。
	for _, f := range flows {
		if f.Deprecated {
			continue
		}
		for _, step := range f.Steps {
			at := step.Since
			if at.IsZero() {
				at = f.Since
			}
			for _, nid := range ix.resolveNodeIDsAt(step.Node, at) {
				ix.flowsByNode[nid] = appendUnique(ix.flowsByNode[nid], f.ID)
			}
		}
	}

	// journal 反查:按现任 ID 与血缘归一(impl §3 Change.Nodes 注释)。
	// 拆分(一个旧 ID → 多个现任)时,该记录挂进【全部】继承者的历史(#8:
	// 否则 last-write-wins 只落到随机一个,另一个 N3 断档)。
	for ci := range changes {
		c := &changes[ci]
		// 轮30-C 雷区分:每次 record_change +1,推翻(overturns/reverts)额外 +2。
		overturnBonus := 0
		if c.Overturns != "" || c.Reverts != "" {
			overturnBonus = 2
		}
		seenTargets := map[string]bool{}
		for _, historicalID := range c.Nodes {
			for _, cur := range ix.resolveNodeIDsAt(historicalID, c.At) {
				if seenTargets[cur] {
					continue
				}
				seenTargets[cur] = true
				viaLineage := cur != historicalID
				ix.journalBy[cur] = append(ix.journalBy[cur], ChangeRef{Idx: ci, ViaLineage: viaLineage})
				ix.landmine[cur] += 1 + overturnBonus
			}
		}
	}
	// entry-level refute(refuted 条目)也算地雷信号:+1 per refuted entry。
	for id, ref := range ix.nodes {
		for i := range ref.Node.Entries {
			if ref.Node.Entries[i].RefutedBy != "" {
				ix.landmine[id]++
			}
		}
	}
	return ix
}

// resolveNodeIDsAt 在“旧 ID 被无关新节点复用”时用制品时间分代。普通查询的
// ResolveNodeIDs 仍代表现在；journal/flow 是历史制品，必须把新节点诞生前的
// 引用送往 lineage 继承者，不能先被 exact ID 截走。
func (ix *Index) resolveNodeIDsAt(id string, at time.Time) []string {
	resolved, _ := ix.ResolveNodeIDsAtContext(context.Background(), id, at, 0)
	return resolved
}

// ---- 查询面 ----

// Node 取现任节点(仅精确 ID)。
func (ix *Index) Node(id string) *NodeRef { return ix.nodes[id] }

// FileNodes 返回某文件的所有节点 ID(R29 批次3:文件域查询免扫全表)。
func (ix *Index) FileNodes(file string) []string { return ix.fileNodes[file] }

// LandmineScore 返回节点的雷区分(轮30-C):变更频次 + 推翻×2 + refute 数。
// 0 = 无地雷信号;≥3 = 雷区(反复改过/推翻过,AI 进该区要警告)。
func (ix *Index) LandmineScore(nodeID string) int { return ix.landmine[nodeID] }

// Nodes 返回全部节点表(只读用)。
func (ix *Index) Nodes() map[string]*NodeRef { return ix.nodes }

// DuplicateNodeIDs 返回被跨分片重复定义并隔离的节点 ID。
func (ix *Index) DuplicateNodeIDs() []string {
	return append([]string(nil), ix.duplicateIDs...)
}

// ResolveNodeID 把(可能是旧的)节点 ID 归一到现任 ID;查无返回 ""。
// 旧 ID 因拆分对应多个现任时返回字典序首个(确定性);需要全部用 ResolveNodeIDs。
func (ix *Index) ResolveNodeID(id string) string {
	if _, ok := ix.nodes[id]; ok {
		return id
	}
	if curs := ix.lineage[id]; len(curs) > 0 {
		return curs[0]
	}
	return ""
}

// ResolveNodeIDs 返回旧 ID 对应的全部现任节点(直接命中返回自身;拆分返回多个)。
func (ix *Index) ResolveNodeIDs(id string) []string {
	if _, ok := ix.nodes[id]; ok {
		return []string{id}
	}
	return ix.lineage[id]
}

// ResolveNodeIDsAt 按历史制品时间解析节点归属。与普通 ResolveNodeIDs 不同，
// 它会在旧 ID 后来被无关新节点复用时把旧 change/flow 送往 lineage heirs；
// 拆分时返回全部现任 heirs。返回值是副本，可在只读锁外安全排序。
func (ix *Index) ResolveNodeIDsAt(id string, at time.Time) []string {
	resolved, _ := ix.ResolveNodeIDsAtContext(context.Background(), id, at, 0)
	return resolved
}

// ResolveNodeIDsAtContext 是供有资源边界的派生索引构造使用的历史归属解析。
// maxCandidates > 0 时在复制 lineage 前后都保证返回候选不超过该值；<=0 仅供
// 既有同步调用保留无限制语义。超过上限必须显式失败，不能截断并静默漏掉 split heir。
func (ix *Index) ResolveNodeIDsAtContext(ctx context.Context, id string, at time.Time, maxCandidates int) ([]string, error) {
	if ctx == nil {
		return nil, fmt.Errorf("index: resolve node IDs: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	first, rest := ix.resolvedNodeIDsAtParts(id, at)
	return copyResolvedNodeIDsContext(ctx, first, rest, maxCandidates)
}

// CountNodeIDsAtContext performs the same temporal selection as
// ResolveNodeIDsAtContext without allocating the result slice. It is intended
// for a global shape preflight that must authorize fan-out before DTO maps and
// slices are created.
func (ix *Index) CountNodeIDsAtContext(ctx context.Context, id string, at time.Time, maxCandidates int) (int, error) {
	if ctx == nil {
		return 0, fmt.Errorf("index: count node IDs: nil context")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	first, rest := ix.resolvedNodeIDsAtParts(id, at)
	total := len(rest)
	if first != "" {
		total++
	}
	if maxCandidates > 0 && total > maxCandidates {
		return 0, fmt.Errorf("index: resolved node candidates %d exceed limit %d", total, maxCandidates)
	}
	for i := range rest {
		if i&63 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
	}
	return total, ctx.Err()
}

func (ix *Index) resolvedNodeIDsAtParts(id string, at time.Time) (string, []string) {
	exact := ix.nodes[id]
	heirs := ix.lineage[id]
	switch {
	case exact != nil && len(heirs) > 0 && exact.Node.Since.IsZero():
		return id, heirs
	case exact != nil && len(heirs) > 0 &&
		(at.IsZero() || (!exact.Node.Since.IsZero() && at.Before(exact.Node.Since))):
		return "", heirs
	case exact != nil && (exact.Node.Since.IsZero() || at.IsZero() || !at.Before(exact.Node.Since)):
		return id, nil
	case exact != nil:
		// 无 lineage 可承接的前任历史宁可不污染无关的新 exact 节点。
		return "", nil
	default:
		return "", heirs
	}
}

func copyResolvedNodeIDsContext(ctx context.Context, first string, rest []string, maxCandidates int) ([]string, error) {
	total := len(rest)
	if first != "" {
		total++
	}
	if maxCandidates > 0 && total > maxCandidates {
		return nil, fmt.Errorf("index: resolved node candidates %d exceed limit %d", total, maxCandidates)
	}
	if total == 0 {
		return nil, ctx.Err()
	}
	out := make([]string, 0, total)
	if first != "" {
		out = append(out, first)
	}
	for i, id := range rest {
		if i&63 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		out = append(out, id)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// LooseMatch 宽松匹配(impl §3 定案):AI 报名精确失败后,忽略接收者/指针做
// 归一匹配;唯一命中返回其 ID,多命中返回候选列表。
func (ix *Index) LooseMatch(file, symbol string) (string, []string) {
	var hits []string
	for id := range ix.nodes {
		f, s := model.SplitNodeID(id)
		if s == "" || (file != "" && f != file) {
			continue
		}
		if model.LooseSymbolMatch(symbol, s) {
			hits = append(hits, id)
		}
	}
	sort.Strings(hits)
	if len(hits) == 1 {
		return hits[0], nil
	}
	return "", hits
}

// ResolveEntryRef 归一化 "node-id#entry-id" 引用:节点沿 lineage、条目沿 supersedes 链。
func (ix *Index) ResolveEntryRef(ref string) string {
	resolved, _, err := ix.ResolveEntryContext(context.Background(), ref, 0)
	if err != nil {
		return ref
	}
	return resolved
}

// EntryResolver owns a request-local, lazily populated entry lookup. Reusing
// one resolver across many disputes scans each candidate node's Entries at most
// once; the Index and returned Entry pointers remain immutable.
type EntryResolver struct {
	ix     *Index
	byNode map[string]map[string]*model.Entry
}

// NewEntryResolver creates a request-local resolver. Callers should construct
// it only after their global source-shape budget has been authorized.
func (ix *Index) NewEntryResolver() *EntryResolver { return &EntryResolver{ix: ix} }

// ResolveEntryContext 一次完成 node lineage、entry supersedes 与最终条目查找，
// 避免调用方先 ResolveEntryRef、再 EntryByRef 时重复整条解析。maxCandidates > 0
// 时限制 exact node + unique lineage heirs 的总数；超过上限显式失败而不截断。
// 返回的 Entry 指向发布后只读的 Index 快照，只能在该快照生命周期内读取。
func (ix *Index) ResolveEntryContext(ctx context.Context, ref string, maxCandidates int) (string, *model.Entry, error) {
	return ix.NewEntryResolver().ResolveContext(ctx, ref, maxCandidates)
}

// CheckEntryResolutionShapeContext validates the raw exact+lineage fan-out
// without allocating a candidate set or scanning Entries. Duplicate lineage
// declarations intentionally still count here: preflight is conservative and
// malformed repetition must not bypass the source-build fan-out bound.
func (ix *Index) CheckEntryResolutionShapeContext(ctx context.Context, ref string, maxCandidates int) error {
	if ctx == nil {
		return fmt.Errorf("index: check entry resolution: nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	nodeID, _ := splitEntryRef(ref)
	if nodeID == "" {
		return nil
	}
	total := len(ix.lineage[nodeID])
	if ix.nodes[nodeID] != nil {
		total++
	}
	if maxCandidates > 0 && total > maxCandidates {
		return fmt.Errorf("index: entry candidate declarations %d exceed limit %d", total, maxCandidates)
	}
	for i := range ix.lineage[nodeID] {
		if i&63 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

// ResolveContext resolves through node lineage and entry supersedes while
// reusing this resolver's per-node entry lookup.
func (r *EntryResolver) ResolveContext(ctx context.Context, ref string, maxCandidates int) (string, *model.Entry, error) {
	if ctx == nil {
		return "", nil, fmt.Errorf("index: resolve entry: nil context")
	}
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	if r == nil || r.ix == nil {
		return "", nil, fmt.Errorf("index: resolve entry: nil resolver")
	}
	ix := r.ix
	nodeID, entryID := splitEntryRef(ref)
	if nodeID == "" {
		return ref, nil, nil
	}
	// Node ID 可能在迁移后被一个无关新符号复用。节点级解析此时应指向现任
	// （ResolveNodeIDs 的既有语义），但稳定 entry 引用仍可能属于 lineage 继承者。
	// 因此 entry 解析要同时查“现任同名节点 + 旧 ID 的全部继承者”。
	lineage := ix.lineage[nodeID]
	capacity := len(lineage) + 1
	if maxCandidates > 0 && capacity > maxCandidates {
		capacity = maxCandidates
	}
	candidates := make([]string, 0, capacity)
	seen := make(map[string]struct{}, capacity)
	appendCandidate := func(candidate string) error {
		if candidate == "" {
			return nil
		}
		if _, duplicate := seen[candidate]; duplicate {
			return nil
		}
		if maxCandidates > 0 && len(candidates) >= maxCandidates {
			return fmt.Errorf("index: entry candidates exceed limit %d", maxCandidates)
		}
		seen[candidate] = struct{}{}
		candidates = append(candidates, candidate)
		return nil
	}
	if ix.nodes[nodeID] != nil {
		if err := appendCandidate(nodeID); err != nil {
			return "", nil, err
		}
	}
	for i, heir := range lineage {
		if i&63 == 0 {
			if err := ctx.Err(); err != nil {
				return "", nil, err
			}
		}
		if err := appendCandidate(heir); err != nil {
			return "", nil, err
		}
	}
	if len(candidates) == 0 {
		return ref, nil, nil
	}
	// 拆分时一个旧 node ID 可有多个继承者,条目只会被 remap 到其中一个。
	// 必须先按 entry ID 在全部继承者中定位,不能先用 ResolveNodeID 固定选
	// 字典序首个——否则 old#entry 会被归一到不含该条目的错误节点。
	bestNode, bestEntryID := "", ""
	var bestEntry *model.Entry
	for i, cur := range candidates {
		if i&63 == 0 {
			if err := ctx.Err(); err != nil {
				return "", nil, err
			}
		}
		eid, entry, ok, err := r.resolveEntryInNodeContext(ctx, cur, entryID)
		if err != nil {
			return "", nil, err
		}
		if !ok {
			continue
		}
		// 真正存在于现任同名节点的 exact entry 优先；无需继续扫描 heirs。
		if cur == nodeID && ix.nodes[nodeID] != nil {
			return cur + "#" + eid, entry, nil
		}
		if bestNode == "" || cur < bestNode {
			bestNode, bestEntryID, bestEntry = cur, eid, entry
		}
	}
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	if bestNode != "" {
		return bestNode + "#" + bestEntryID, bestEntry, nil
	}
	// 保持原来的可诊断退化:节点能归一但条目不存在时,返回首个现任节点
	// 加原 entry ID,由上层给出精确的“条目不在节点”错误。
	return candidates[0] + "#" + entryID, nil, nil
}

// resolveEntryInNodeContext 在单个现任节点内解析 supersedes 链。
func (r *EntryResolver) resolveEntryInNodeContext(ctx context.Context, nodeID, entryID string) (string, *model.Entry, bool, error) {
	byID, err := r.entriesByNodeContext(ctx, nodeID)
	if err != nil {
		return "", nil, false, err
	}
	eid := entryID
	if byID[eid] == nil {
		return "", nil, false, nil
	}
	for range 32 {
		if err := ctx.Err(); err != nil {
			return "", nil, false, err
		}
		e, ok := byID[eid]
		if !ok || e.SupersededBy == "" {
			break
		}
		eid = e.SupersededBy
	}
	return eid, byID[eid], true, nil
}

func (r *EntryResolver) entriesByNodeContext(ctx context.Context, nodeID string) (map[string]*model.Entry, error) {
	if r.byNode != nil {
		if byID, ok := r.byNode[nodeID]; ok {
			return byID, nil
		}
	}
	nref := r.ix.nodes[nodeID]
	if nref == nil {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	byID := make(map[string]*model.Entry, len(nref.Node.Entries))
	for i := range nref.Node.Entries {
		if i&63 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		byID[nref.Node.Entries[i].ID] = &nref.Node.Entries[i]
	}
	if r.byNode == nil {
		r.byNode = make(map[string]map[string]*model.Entry)
	}
	r.byNode[nodeID] = byID
	return byID, ctx.Err()
}

// DisputedBy 返回声明与该条目矛盾的条目引用(归一化;knowledge.md §12.4)。
func (ix *Index) DisputedBy(entryRef string) []string {
	// disputesRev 是发布后只读的索引快照;排序前必须复制,否则多个持
	// rt.mu.RLock 的 Recall 会并发改同一底层 slice。
	out := append([]string(nil), ix.disputesRev[ix.ResolveEntryRef(entryRef)]...)
	sort.Strings(out)
	return out
}

// EntryByRef 按 "node-id#entry-id" 引用取条目(沿 lineage/supersedes 归一;查无 nil)。
func (ix *Index) EntryByRef(ref string) *model.Entry {
	_, entry, err := ix.ResolveEntryContext(context.Background(), ref, 0)
	if err != nil {
		return nil
	}
	return entry
}

// Dependents 沿 basedOn 反向图取直接+间接依赖者(级联污染回收,knowledge.md §12.5)。
func (ix *Index) Dependents(entryRef string) []string {
	root := ix.ResolveEntryRef(entryRef)
	seen := map[string]bool{root: true}
	var out []string
	var walk func(ref string)
	walk = func(ref string) {
		for _, dep := range ix.basedOnRev[ix.ResolveEntryRef(ref)] {
			if seen[dep] {
				continue
			}
			seen[dep] = true
			out = append(out, dep)
			walk(dep)
		}
	}
	walk(root)
	sort.Strings(out)
	return out
}

// FlowsOf 返回引用该节点的流程 ID(反向链接)。
func (ix *Index) FlowsOf(nodeID string) []string { return ix.flowsByNode[nodeID] }

// Flows 返回全部流程/主题节点。
func (ix *Index) Flows() []model.Flow { return ix.flows }

// Flow 按 ID 取流程。
func (ix *Index) Flow(id string) *model.Flow {
	for i := range ix.flows {
		if ix.flows[i].ID == id {
			return &ix.flows[i]
		}
	}
	return nil
}

// History 取节点的变更引用(at 升序;含血缘穿透与 Since 过滤,impl §7.3)。
func (ix *Index) History(nodeID string) []model.Change {
	ref := ix.nodes[nodeID]
	var out []model.Change
	for _, cr := range ix.journalBy[nodeID] {
		c := ix.changes[cr.Idx]
		// 同 ID 直接命中须 at ≥ 节点 Since——防旧名被无关新函数复用后错继承前任历史;
		// 血缘命中不受 Since 限制(前任的历史正是要继承的)。
		if !cr.ViaLineage && ref != nil && c.At.Before(ref.Node.Since) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// Changes 返回全部变更(at 升序)。
func (ix *Index) Changes() []model.Change { return ix.changes }

// ChangeByID 按 ID 找变更。
func (ix *Index) ChangeByID(id string) *model.Change {
	for i := range ix.changes {
		if ix.changes[i].ID == id {
			return &ix.changes[i]
		}
	}
	return nil
}

// Hit 是一次检索命中。
type Hit struct {
	NodeID string
	Score  int
}

// Search 关键词倒排检索(impl §8 排序:命中 token 数降序 → 层级 → ID 字典序,前 10)。
// R29 批次5:精确 token 命中 < 3 时,回退 trigram 近似匹配(auth↔authentication、
// loginLockout↔login lockout)。trigram 分数与精确分加权合并。
func (ix *Index) Search(query string, limit int) []Hit {
	return ix.searchTokenMaps(query, limit, ix.inverted, ix.trigram)
}

// SearchCurrent 只检索当前事实文本和节点 ID/路径/符号/keywords 导航。
// 风险条目不会因为词面命中而被提升为当前结论。
func (ix *Index) SearchCurrent(query string, limit int) []Hit {
	return ix.searchTokenMaps(query, limit, ix.invertedCurrent, ix.trigramCurrent)
}

// SearchRisk 只检索 pitfall、suspect 节点/条目、活跃未裁决 dispute 双方的文本。
func (ix *Index) SearchRisk(query string, limit int) []Hit {
	return ix.searchTokenMaps(query, limit, ix.invertedRisk, ix.trigramRisk)
}

func (ix *Index) searchTokenMaps(query string, limit int, inverted, trigram map[string]map[string]bool) []Hit {
	if limit <= 0 {
		limit = 10
	}
	tokens := Tokenize(query)
	scores := map[string]int{}
	exactHits := 0
	for _, tok := range tokens {
		for id := range inverted[tok] {
			scores[id] += 2 // 精确命中权重 2
			exactHits++
		}
	}
	// 精确命中不足 → trigram 回退。
	if exactHits < 3 {
		for _, tok := range tokens {
			if len(tok) < 3 {
				continue
			}
			for _, tg := range trigrams(tok) {
				for id := range trigram[tg] {
					scores[id]++ // trigram 权重 1
				}
			}
		}
	}
	hits := make([]Hit, 0, len(scores))
	for id, sc := range scores {
		hits = append(hits, Hit{NodeID: id, Score: sc})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		li, lj := levelRank(ix.nodes[hits[i].NodeID].Node.Level), levelRank(ix.nodes[hits[j].NodeID].Node.Level)
		if li != lj {
			return li < lj
		}
		return hits[i].NodeID < hits[j].NodeID
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

func addSearchTokens(id string, tokens []string, inverted, trigram map[string]map[string]bool) {
	for _, tok := range tokens {
		set := inverted[tok]
		if set == nil {
			set = map[string]bool{}
			inverted[tok] = set
		}
		set[id] = true
		// trigram:对长度≥3 的 token 取所有三字母组(小写归一)。
		if len(tok) < 3 {
			continue
		}
		for _, tg := range trigrams(tok) {
			tgSet := trigram[tg]
			if tgSet == nil {
				tgSet = map[string]bool{}
				trigram[tg] = tgSet
			}
			tgSet[id] = true
		}
	}
}

// trigrams 取小写归一化的所有三字母组(loginLockout → [log, ogi, gin, ...])。
// 仅对纯 ASCII 字母数字 token 生效——中文等多字节字符的"trigram"会切坏 UTF-8 且语义不通。
// 长度 < 3 返回 nil。
func trigrams(s string) []string {
	if len(s) < 3 {
		return nil
	}
	s = strings.ToLower(s)
	// 非 ASCII 直接跳过(中文等不走 trigram 机制)。
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return nil
		}
	}
	out := make([]string, 0, len(s)-2)
	for i := 0; i+3 <= len(s); i++ {
		out = append(out, s[i:i+3])
	}
	return out
}

func levelRank(level string) int {
	switch level {
	case model.LevelFunction, model.LevelDecl:
		return 0
	case model.LevelFile:
		return 1
	case model.LevelStmt:
		return 2
	case model.LevelDir:
		return 3
	default:
		return 4
	}
}

// ---- 分词(impl §8 定案) ----

// Tokenize:ASCII 按非字母数字切 + 全部转小写;CJK 连续串按 bigram;
// ASCII 词再做标识符拆分(驼峰/下划线)后同时入索引。
func Tokenize(text string) []string {
	var tokens []string
	var ascii []rune
	var cjk []rune
	flushASCII := func() {
		if len(ascii) == 0 {
			return
		}
		word := strings.ToLower(string(ascii))
		tokens = append(tokens, word)
		for _, part := range SplitIdent(string(ascii)) {
			if part != word {
				tokens = append(tokens, part)
			}
		}
		ascii = ascii[:0]
	}
	flushCJK := func() {
		if len(cjk) == 0 {
			return
		}
		if len(cjk) == 1 {
			tokens = append(tokens, string(cjk))
		}
		for i := 0; i+1 < len(cjk); i++ {
			tokens = append(tokens, string(cjk[i:i+2]))
		}
		cjk = cjk[:0]
	}
	for _, r := range text {
		switch {
		case r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			flushCJK()
			ascii = append(ascii, r)
		case unicode.Is(unicode.Han, r):
			flushASCII()
			cjk = append(cjk, r)
		default:
			flushASCII()
			flushCJK()
		}
	}
	flushASCII()
	flushCJK()
	return dedup(tokens)
}

// SplitIdent 标识符按驼峰与下划线拆词(impl §8:checkLockout → check/lockout),
// 全小写。空库期(Keywords/Entries 全空)缓解词汇鸿沟的唯一手段。
func SplitIdent(ident string) []string {
	var parts []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			parts = append(parts, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	runes := []rune(ident)
	for i, r := range runes {
		switch {
		case r == '_' || r == '.' || r == '~':
			flush()
		case unicode.IsUpper(r):
			// 驼峰边界:小写→大写,或 大写序列后跟小写(HTTPServer → http/server)。
			if i > 0 && (unicode.IsLower(runes[i-1]) ||
				(unicode.IsUpper(runes[i-1]) && i+1 < len(runes) && unicode.IsLower(runes[i+1]))) {
				flush()
			}
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return dedup(parts)
}

func dedup(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func appendUnique(list []string, v string) []string {
	if slices.Contains(list, v) {
		return list
	}
	return append(list, v)
}

func splitEntryRef(ref string) (nodeID, entryID string) {
	i := strings.LastIndexByte(ref, '#')
	if i < 0 {
		return "", ""
	}
	// 节点 ID 自身可能含 '#'(file#Symbol),条目引用是 node-id#entry-id,
	// entry ID 形如 e_xxxx——取最后一个 '#' 右侧为 entry。
	return ref[:i], ref[i+1:]
}
