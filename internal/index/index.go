// Package index 是内存索引(impl §8):倒排关键词、节点表、lineage/supersedes
// 归一化解析、basedOn 反向图、flow 反向链接、journal 按节点反查。
// 文件是真相,索引是缓存,随时可由 store 的内容重建。
package index

import (
	"slices"
	"sort"
	"strings"
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
	nodes       map[string]*NodeRef
	lineage     map[string][]string        // 旧节点 ID → 现任节点 ID(拆分可多个,#8)
	inverted    map[string]map[string]bool // token → 节点 ID 集合
	basedOnRev  map[string][]string        // 归一化 "node#entry" → 依赖它的 "node#entry"
	disputesRev map[string][]string        // 归一化 "node#entry" → 声明与它矛盾的 "node#entry"
	flowsByNode map[string][]string        // 节点 ID → flow IDs(反向链接,现算不落盘)
	journalBy   map[string][]ChangeRef     // 现任节点 ID → 变更引用
	changes     []model.Change
	flows       []model.Flow
}

// Build 全量构建索引。shards 是"分片相对路径 → 节点切片",由 engine 从 store
// 快照筛出健康分片传入(conflict/schema 隔离的不进索引)——index 因此不依赖
// store,维持 impl §2 依赖方向。切片共享缓存底层数组,NodeRef 指针指向缓存实体。
func Build(shards map[string][]model.Node, changes []model.Change, flows []model.Flow) *Index {
	ix := &Index{
		nodes:       map[string]*NodeRef{},
		lineage:     map[string][]string{},
		inverted:    map[string]map[string]bool{},
		basedOnRev:  map[string][]string{},
		disputesRev: map[string][]string{},
		flowsByNode: map[string][]string{},
		journalBy:   map[string][]ChangeRef{},
		changes:     changes,
		flows:       flows,
	}

	for rel, nodes := range shards {
		for i := range nodes {
			n := &nodes[i]
			// 铁律二防线:恶意/损坏分片里带 ../ 的节点 ID 直接丢弃——
			// 读路径由节点 ID 驱动文件读,不消毒会被穿越到仓库外(索引是唯一收口)。
			if !model.SafeNodeID(n.ID) {
				continue
			}
			ix.nodes[n.ID] = &NodeRef{ShardRel: rel, Node: n}
			for _, oldID := range n.Lineage {
				ix.lineage[oldID] = append(ix.lineage[oldID], n.ID)
			}
		}
	}
	for old := range ix.lineage {
		sort.Strings(ix.lineage[old]) // 确定性:map 迭代序不可信
	}

	// 倒排:节点 ID 分段 + 符号标识符拆词 + keywords + 活跃条目文本(impl §8)。
	for id, ref := range ix.nodes {
		n := ref.Node
		add := func(tokens []string) {
			for _, tok := range tokens {
				set := ix.inverted[tok]
				if set == nil {
					set = map[string]bool{}
					ix.inverted[tok] = set
				}
				set[id] = true
			}
		}
		file, symbol := model.SplitNodeID(id)
		for seg := range strings.SplitSeq(file, "/") {
			add(Tokenize(seg))
		}
		if symbol != "" {
			add(SplitIdent(symbol))
		}
		for _, kw := range n.Keywords {
			add(Tokenize(kw))
		}
		for i := range n.Entries {
			if n.Entries[i].Active() {
				add(Tokenize(n.Entries[i].Text))
			}
		}
	}

	// basedOn / disputes 反向图:引用永不改写,建图时归一化(impl §8)。
	for id, ref := range ix.nodes {
		for i := range ref.Node.Entries {
			e := &ref.Node.Entries[i]
			from := id + "#" + e.ID
			for _, dep := range e.BasedOn {
				ix.basedOnRev[ix.ResolveEntryRef(dep)] = append(ix.basedOnRev[ix.ResolveEntryRef(dep)], from)
			}
			for _, d := range e.Disputes {
				ix.disputesRev[ix.ResolveEntryRef(d)] = append(ix.disputesRev[ix.ResolveEntryRef(d)], from)
			}
		}
	}

	// flow 反向链接(现算,不落盘)。
	for _, f := range flows {
		if f.Deprecated {
			continue
		}
		for _, step := range f.Steps {
			nid := ix.ResolveNodeID(step.Node)
			if nid != "" {
				ix.flowsByNode[nid] = appendUnique(ix.flowsByNode[nid], f.ID)
			}
		}
	}

	// journal 反查:按现任 ID 与血缘归一(impl §3 Change.Nodes 注释)。
	// 拆分(一个旧 ID → 多个现任)时,该记录挂进【全部】继承者的历史(#8:
	// 否则 last-write-wins 只落到随机一个,另一个 N3 断档)。
	for ci := range changes {
		for _, nid := range changes[ci].Nodes {
			if _, ok := ix.nodes[nid]; ok {
				ix.journalBy[nid] = append(ix.journalBy[nid], ChangeRef{Idx: ci})
				continue
			}
			for _, cur := range ix.lineage[nid] {
				ix.journalBy[cur] = append(ix.journalBy[cur], ChangeRef{Idx: ci, ViaLineage: true})
			}
		}
	}
	return ix
}

// ---- 查询面 ----

// Node 取现任节点(仅精确 ID)。
func (ix *Index) Node(id string) *NodeRef { return ix.nodes[id] }

// Nodes 返回全部节点表(只读用)。
func (ix *Index) Nodes() map[string]*NodeRef { return ix.nodes }

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
	nodeID, entryID := splitEntryRef(ref)
	if nodeID == "" {
		return ref
	}
	cur := ix.ResolveNodeID(nodeID)
	if cur == "" {
		return ref
	}
	n := ix.nodes[cur].Node
	// 条目 supersedes 链解析(防环:步数上限)。
	byID := map[string]*model.Entry{}
	for i := range n.Entries {
		byID[n.Entries[i].ID] = &n.Entries[i]
	}
	eid := entryID
	for range 32 {
		e, ok := byID[eid]
		if !ok || e.SupersededBy == "" {
			break
		}
		eid = e.SupersededBy
	}
	return cur + "#" + eid
}

// DisputedBy 返回声明与该条目矛盾的条目引用(归一化;knowledge.md §12.4)。
func (ix *Index) DisputedBy(entryRef string) []string {
	out := ix.disputesRev[ix.ResolveEntryRef(entryRef)]
	sort.Strings(out)
	return out
}

// EntryByRef 按 "node-id#entry-id" 引用取条目(沿 lineage/supersedes 归一;查无 nil)。
func (ix *Index) EntryByRef(ref string) *model.Entry {
	nodeID, entryID := splitEntryRef(ix.ResolveEntryRef(ref))
	if nodeID == "" {
		return nil
	}
	nref := ix.nodes[nodeID]
	if nref == nil {
		return nil
	}
	for i := range nref.Node.Entries {
		if nref.Node.Entries[i].ID == entryID {
			return &nref.Node.Entries[i]
		}
	}
	return nil
}

// Dependents 沿 basedOn 反向图取直接+间接依赖者(级联污染回收,knowledge.md §12.5)。
func (ix *Index) Dependents(entryRef string) []string {
	seen := map[string]bool{}
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
	walk(entryRef)
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
func (ix *Index) Search(query string, limit int) []Hit {
	if limit <= 0 {
		limit = 10
	}
	tokens := Tokenize(query)
	scores := map[string]int{}
	for _, tok := range tokens {
		for id := range ix.inverted[tok] {
			scores[id]++
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
