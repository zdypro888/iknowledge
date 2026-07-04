package engine

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zdypro888/iknowledge/internal/index"
	"github.com/zdypro888/iknowledge/internal/model"
	"github.com/zdypro888/iknowledge/internal/parser"
)

// KBError 是业务拒绝(impl §7.4):工具结果 isError:true,
// 文本格式 KB_ERR:<CODE>: <说明> | <怎么办>,便于 AI 自纠。
type KBError struct {
	Code string
	Msg  string
	Hint string
}

func (e *KBError) Error() string {
	return "KB_ERR:" + e.Code + ": " + e.Msg + " | " + e.Hint
}

func kbErr(code, msg, hint string) *KBError { return &KBError{Code: code, Msg: msg, Hint: hint} }

// ReadMeta 供使用日志(impl §7.6)。
type ReadMeta struct {
	Hit       bool
	HitStatus string
	Stale     bool
}

// ---- kb_status ----

// Status 组装库状态(impl §7.3)。
func (e *Engine) Status() (string, error) {
	if !e.Store.Initialized() {
		return "库未初始化。先调 kb_init(或 CLI:iknowledge init --repo " + e.Store.RepoRoot() + ")。", nil
	}
	if err := e.Sync(); err != nil {
		return "", err
	}

	// 解析失败文件:现扫(git 子进程 + 全库 parse)。#21:放在 rt.mu 之外做——
	// 只读 Store/Reg 不碰 rt 状态,持锁跑 git 会阻塞所有并发请求数百毫秒。
	parseFailed := 0
	cfg, _ := e.Store.LoadConfig()
	if files, err := listSourceFiles(e.Store.RepoRoot(), e.Reg, cfg); err == nil {
		for _, rel := range files {
			src, err := os.ReadFile(filepath.Join(e.Store.RepoRoot(), filepath.FromSlash(rel)))
			if err != nil || parser.IsGenerated(src) {
				continue
			}
			if _, err := e.Reg.ForFile(rel).Parse(rel, src); err != nil {
				parseFailed++
			}
		}
	}

	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()

	total, digested, suspect, orphaned, pending := 0, 0, 0, 0, 0
	conflicts := 0
	var orphanIDs []string
	for _, cs := range e.rt.cache.Shards() {
		if cs.Err != nil {
			conflicts++
			continue
		}
		for i := range cs.Shard.Nodes {
			n := &cs.Shard.Nodes[i]
			total++
			if hasActiveEntries(n) {
				digested++
			}
			switch n.Status {
			case model.StatusSuspect:
				suspect++
			case model.StatusOrphaned:
				orphaned++
				orphanIDs = append(orphanIDs, n.ID)
			}
			if n.PendingAnchor {
				pending++
			}
		}
	}
	sort.Strings(orphanIDs)
	_, jstats := e.rt.cache.Journal()

	var b strings.Builder
	fmt.Fprintf(&b, "repoRoot: %s\nschema: %d\n", e.Store.RepoRoot(), model.SchemaVersion)
	fmt.Fprintf(&b, "节点: %d(已消化 %d,覆盖率 %.0f%%)\n", total, digested, pct(digested, total))
	fmt.Fprintf(&b, "suspect: %d | 孤儿: %d | 待补锚: %d | 冲突分片: %d | 解析失败文件: %d | journal 坏行: %d\n",
		suspect, orphaned, pending, conflicts, parseFailed, jstats.BadLines)
	if len(orphanIDs) > 0 { // 供 kb_adopt 认领/送葬(#2:hint 指向这里)
		shown := orphanIDs
		if len(shown) > 20 {
			shown = shown[:20]
		}
		fmt.Fprintf(&b, "孤儿节点(kb_adopt claim/bury): %s\n", strings.Join(shown, "、"))
	}
	if len(jstats.ConflictIDs) > 0 {
		fmt.Fprintf(&b, "⚠ journal 同 ID 异内容(双份保留待人裁决): %v\n", jstats.ConflictIDs)
	}
	if e.Store.GitFilesOK() {
		b.WriteString(".gitattributes/.gitignore: 在位\n")
	} else {
		b.WriteString("⚠ .gitattributes/.gitignore 缺失或缺行——union 合并会静默失效,请重跑 kb_init\n")
	}

	// 使用日志汇总(impl §7.6:数据裁决的采集底)。
	if recs, err := e.Store.LoadUsage(); err == nil && len(recs) > 0 {
		var recalls, hits, staleN, changes, remembers, undigestedHits int
		for _, r := range recs {
			switch r.Tool {
			case "kb_recall":
				recalls++
				if r.Hit {
					hits++
				}
				if r.HitStatus == string(model.StatusUndigested) {
					undigestedHits++
				}
				if r.Stale {
					staleN++
				}
			case "kb_record_change":
				if r.OK {
					changes++
				}
			case "kb_remember":
				if r.OK {
					remembers++
				}
			}
		}
		fmt.Fprintf(&b, "使用日志: recall %d 次(命中率 %.0f%%,undigested 命中 %d,空手 %d)| remember %d | record_change %d | 读取时对账发现未记账变更 %d\n",
			recalls, pct(hits, recalls), undigestedHits, recalls-hits, remembers, changes, staleN)
		if staleN > 0 && changes*10 < staleN*10 { // 展示记账遵守率信号
			fmt.Fprintf(&b, "⚠ 记账遵守率信号:未记账变更事件 %d vs 记账 %d\n", staleN, changes)
		}
	}

	// 活跃任务态。
	if len(e.rt.wips) > 0 {
		b.WriteString("活跃 wip:\n")
		for _, w := range e.rt.wips {
			fmt.Fprintf(&b, "  - [%s] %s(todo %d 项,touching %v)\n", w.Owner, w.Task, len(w.Todo), w.Touching)
		}
	}
	// 维护欠账。
	debts := e.computeDebtsLocked()
	fmt.Fprintf(&b, "维护欠账: %d 条(kb_maintain 取用)\n", len(debts))
	for _, w := range e.rt.warns {
		fmt.Fprintf(&b, "⚠ %s\n", w)
	}
	for _, w := range e.rt.opsWarns {
		fmt.Fprintf(&b, "⚠ %s\n", w)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) * 100 / float64(b)
}

func hasActiveEntries(n *model.Node) bool {
	for i := range n.Entries {
		if n.Entries[i].Active() {
			return true
		}
	}
	return false
}

// ---- kb_map ----

// Map 金字塔分支摘要视图(impl §7.3)。
func (e *Engine) Map(pathArg string, depth int, sid string) (string, ReadMeta, error) {
	if err := e.requireInit(); err != nil {
		return "", ReadMeta{}, err
	}
	if err := e.Sync(); err != nil {
		return "", ReadMeta{}, err
	}
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()

	if depth <= 0 {
		depth = 2
	}
	root := strings.Trim(strings.TrimPrefix(pathArg, "./"), "/")

	// 收集视图内节点,按树层组织。
	type dirInfo struct {
		files map[string]bool
		dirs  map[string]bool
	}
	dirs := map[string]*dirInfo{} // dir 路径("" = 根)
	ensureDir := func(d string) *dirInfo {
		di, ok := dirs[d]
		if !ok {
			di = &dirInfo{files: map[string]bool{}, dirs: map[string]bool{}}
			dirs[d] = di
		}
		return di
	}
	fileNodes := map[string]*index.NodeRef{}
	symNodes := map[string][]*index.NodeRef{}
	var visibleIDs []string

	for id, ref := range e.rt.ix.Nodes() {
		file, symbol := model.SplitNodeID(id)
		if ref.Node.Level == model.LevelDir || ref.Node.Level == model.LevelProject {
			continue // 目录/项目节点由结构推导呈现
		}
		if root != "" && file != root && !strings.HasPrefix(file, root+"/") {
			continue
		}
		if symbol == "" {
			fileNodes[file] = ref
		} else {
			symNodes[file] = append(symNodes[file], ref)
		}
		d := path.Dir(file)
		if d == "." {
			d = ""
		}
		ensureDir(d).files[file] = true
		// 向上挂目录链。
		for d != "" {
			parent := path.Dir(d)
			if parent == "." {
				parent = ""
			}
			ensureDir(parent).dirs[d] = true
			d = parent
		}
	}
	if len(fileNodes) == 0 {
		return "", ReadMeta{}, kbErr("NODE_NOT_FOUND",
			"路径 "+pathArg+" 下没有任何节点", "用 kb_map 不带 path 看全景,或检查路径拼写(相对仓库根,正斜杠)")
	}

	coverage := func(prefix string) (dig, tot int) {
		for id, ref := range e.rt.ix.Nodes() {
			file, _ := model.SplitNodeID(id)
			if prefix != "" && file != prefix && !strings.HasPrefix(file, prefix+"/") {
				continue
			}
			if ref.Node.Level == model.LevelDir || ref.Node.Level == model.LevelProject {
				continue
			}
			tot++
			if hasActiveEntries(ref.Node) {
				dig++
			}
		}
		return
	}

	var b strings.Builder
	if root == "" {
		if pn := e.rt.ix.Node(model.ProjectNodeID); pn != nil {
			fmt.Fprintf(&b, ". %s\n", nodeLine(pn.Node))
		}
	}
	var walk func(dir string, level int)
	walk = func(dir string, level int) {
		if level > depth {
			return
		}
		di := dirs[dir]
		if di == nil {
			return
		}
		indent := strings.Repeat("  ", level)
		var subdirs []string
		for d := range di.dirs {
			subdirs = append(subdirs, d)
		}
		sort.Strings(subdirs)
		for _, d := range subdirs {
			dig, tot := coverage(d)
			line := d + "/"
			if ref := e.rt.ix.Node(model.DirNodeID(d)); ref != nil {
				line += " " + nodeLine(ref.Node)
			}
			fmt.Fprintf(&b, "%s%s [coverage %d/%d]\n", indent, line, dig, tot)
			walk(d, level+1)
		}
		var files []string
		for f := range di.files {
			files = append(files, f)
		}
		sort.Strings(files)
		for _, f := range files {
			ref := fileNodes[f]
			if ref == nil {
				continue
			}
			fmt.Fprintf(&b, "%s%s %s\n", indent, f, nodeLine(ref.Node))
			visibleIDs = append(visibleIDs, f)
			if level+1 > depth {
				continue
			}
			syms := symNodes[f]
			sort.Slice(syms, func(i, j int) bool { return syms[i].Node.ID < syms[j].Node.ID })
			for _, sref := range syms {
				_, symbol := model.SplitNodeID(sref.Node.ID)
				fmt.Fprintf(&b, "%s  #%s %s\n", indent, symbol, nodeLine(sref.Node))
				visibleIDs = append(visibleIDs, sref.Node.ID)
			}
		}
	}
	startDir := ""
	if root != "" {
		if _, ok := dirs[root]; ok {
			startDir = root
			dig, tot := coverage(root)
			fmt.Fprintf(&b, "%s/ [coverage %d/%d]\n", root, dig, tot)
		} else {
			startDir = path.Dir(root)
			if startDir == "." {
				startDir = ""
			}
		}
	}
	walk(startDir, 1)

	out := b.String()
	// 预算裁剪:超 2000 token 截断并提示下钻(impl §7.3)。
	if EstimateTokens(out) > 2000 {
		lines := strings.Split(out, "\n")
		var kept []string
		used := 0
		for _, l := range lines {
			used += EstimateTokens(l) + 1
			if used > 1900 {
				break
			}
			kept = append(kept, l)
		}
		out = strings.Join(kept, "\n") + "\n……(已截断,带 path 参数下钻查看)"
	}
	out += e.wipAttachment(visibleIDs)
	return framed(out), ReadMeta{Hit: true}, nil
}

// nodeLine 一行摘要:summary(或 [undigested])+ status 标记(impl §7.3 kb_map)。
func nodeLine(n *model.Node) string {
	var parts []string
	if s := firstSummary(n); s != "" {
		parts = append(parts, s)
	} else if !hasActiveEntries(n) {
		parts = append(parts, "[undigested]")
	}
	switch n.Status {
	case model.StatusSuspect:
		parts = append(parts, "[suspect 待重验]")
	case model.StatusOrphaned:
		parts = append(parts, "[orphaned 待认领]")
	}
	if n.PendingAnchor {
		parts = append(parts, "[待补锚]")
	}
	return strings.Join(parts, " ")
}

func firstSummary(n *model.Node) string {
	for i := range n.Entries {
		if n.Entries[i].Active() && n.Entries[i].Kind == model.KindSummary {
			return n.Entries[i].Text
		}
	}
	return ""
}

// wipAttachment 触碰 wip.touching 节点时自动附带台账(knowledge.md §7 规则 2)。
func (e *Engine) wipAttachment(nodeIDs []string) string {
	if len(e.rt.wips) == 0 || len(nodeIDs) == 0 {
		return ""
	}
	inView := map[string]bool{}
	for _, id := range nodeIDs {
		inView[id] = true
	}
	var b strings.Builder
	for _, w := range e.rt.wips {
		for _, t := range w.Touching {
			if inView[t] || inView[e.rt.ix.ResolveNodeID(t)] {
				fmt.Fprintf(&b, "\n⚠ 进行中任务(勿重复/勿冲突,半成品不是 bug):[%s] %s\n  done: %v\n  todo: %v\n  touching: %v",
					w.Owner, w.Task, w.Done, w.Todo, w.Touching)
				break
			}
		}
	}
	return b.String()
}

func (e *Engine) requireInit() *KBError {
	if !e.Store.Initialized() {
		return kbErr("NOT_INITIALIZED", "库未初始化", "先调 kb_init")
	}
	return nil
}

// ---- kb_recall ----

// RecallArgs 是 kb_recall 入参(impl §7.3)。
type RecallArgs struct {
	Query  string
	Mode   string // usage | history | flow
	Limit  int
	Before string // history 翻页
}

// Recall 查知识(impl §7.3 全行为)。
func (e *Engine) Recall(a RecallArgs, sid string) (string, ReadMeta, error) {
	if err := e.requireInit(); err != nil {
		return "", ReadMeta{}, err
	}
	if err := e.Sync(); err != nil {
		return "", ReadMeta{}, err
	}
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()

	if a.Mode == "" {
		a.Mode = "usage"
	}
	if a.Limit <= 0 {
		a.Limit = 5
	}

	// flow 模式且 query 直指流程 ID/标题;空 query 列出所有流程(#7)。
	if a.Mode == "flow" {
		if out, ok := e.flowView(a.Query); ok {
			return framed(out), ReadMeta{Hit: true}, nil
		}
		if strings.TrimSpace(a.Query) == "" {
			return framed(e.listFlowsLocked()), ReadMeta{Hit: len(e.rt.flows) > 0}, nil
		}
	}

	nodeID, candidates := e.resolveQueryLocked(a.Query)
	if nodeID == "" && len(candidates) > 1 {
		return "", ReadMeta{}, kbErr("NODE_NOT_FOUND",
			"符号 "+a.Query+" 有多个候选:"+strings.Join(candidates, "、"),
			"按候选列表用完整节点 ID 重查")
	}
	// #36:query 指向的文件分片处于 conflict/schema 隔离态时,它对索引贡献零节点、
	// 会被当 miss——但知识不是没有,是暂不可用,必须如实呈现而非静默丢。
	if nodeID == "" {
		if qf, _, _ := strings.Cut(strings.TrimSpace(a.Query), "#"); qf != "" {
			if cerr := e.rt.cache.ConflictShard(qf); cerr != nil {
				return framed("该文件的知识分片有未解决的合并冲突或版本不兼容,知识暂不可用,请人工解决:" +
					cerr.Error()), ReadMeta{Hit: false}, nil
			}
		}
	}
	if nodeID != "" {
		return e.recallNodeLocked(nodeID, a, sid)
	}

	// 关键词倒排。
	hits := e.rt.ix.Search(a.Query, a.Limit)
	if len(hits) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "关键词命中 %d 个节点:\n", len(hits))
		var ids []string
		for _, h := range hits {
			ref := e.rt.ix.Node(h.NodeID)
			fmt.Fprintf(&b, "- %s %s\n", h.NodeID, nodeLine(ref.Node))
			ids = append(ids, h.NodeID)
		}
		b.WriteString("(用节点 ID 精确重查取快照/历史)")
		b.WriteString(e.wipAttachment(ids))
		return framed(b.String()), ReadMeta{Hit: true}, nil
	}

	// miss 协议(impl §7.3 定案):零命中 → 符号模糊 + 最相关分支 map 摘要 + 回填义务。
	return e.missProtocolLocked(a.Query), ReadMeta{Hit: false}, nil
}

// resolveQueryLocked:节点 ID 精确 → lineage → 宽松归一(impl §3 文法)。
func (e *Engine) resolveQueryLocked(query string) (string, []string) {
	q := strings.TrimSpace(query)
	if id := e.rt.ix.ResolveNodeID(q); id != "" {
		return id, nil
	}
	if file, symbol, ok := strings.Cut(q, "#"); ok {
		id, cands := e.rt.ix.LooseMatch(file, symbol)
		return id, cands
	}
	// 纯符号名(无 #):全库宽松匹配,唯一命中才算。
	if !strings.ContainsAny(q, " \t/") && q != "" {
		id, cands := e.rt.ix.LooseMatch("", q)
		if id != "" || len(cands) > 1 {
			return id, cands
		}
	}
	return "", nil
}

// recallNodeLocked 命中节点:读取时对账 → 台账/警报 → 按模式渲染。
func (e *Engine) recallNodeLocked(nodeID string, a RecallArgs, sid string) (string, ReadMeta, error) {
	ref := e.rt.ix.Node(nodeID)
	n := ref.Node
	meta := ReadMeta{Hit: true, HitStatus: string(n.Status)}

	// 读取时锚点对账(一期,impl §7.3;auto 现算本就要读源文件,增量成本≈0)。
	staleBanner := ""
	auto := e.reconcileOnReadLocked(ref)
	if auto.stale {
		meta.Stale = true
		meta.HitStatus = string(model.StatusSuspect)
		staleBanner = "⚠ 该代码在知识写入后已变更且无对应变更记录——以下知识可能过时;若是你改的,请补 kb_record_change。\n"
	}

	alert := e.recordRead(sid, nodeID, auto.curHash)

	var b strings.Builder
	if alert != "" {
		b.WriteString(alert)
		b.WriteString("\n")
	}
	b.WriteString(staleBanner)

	switch n.Status {
	case model.StatusOrphaned:
		b.WriteString("⚠ 该节点为 orphaned:锚定符号已消失(可能被改名/删除)。历史与知识保留如下;认领/送葬用 kb_adopt。\n")
	}
	if !hasActiveEntries(n) && n.Status != model.StatusOrphaned {
		b.WriteString("此节点未消化,仅有骨架,请读原文。\n")
	}

	fmt.Fprintf(&b, "节点: %s(%s,%s)\n", nodeID, n.Level, n.Status)
	if auto.signature != "" {
		fmt.Fprintf(&b, "签名: %s\n", auto.signature)
	}
	if len(auto.calls) > 0 {
		fmt.Fprintf(&b, "调用(同文件): %s\n", strings.Join(auto.calls, ", "))
	}
	if len(auto.calledBy) > 0 {
		fmt.Fprintf(&b, "被调用(同文件): %s\n", strings.Join(auto.calledBy, ", "))
	}
	if len(n.Keywords) > 0 {
		fmt.Fprintf(&b, "keywords: %s\n", strings.Join(n.Keywords, ", "))
	}
	if flows := e.rt.ix.FlowsOf(nodeID); len(flows) > 0 {
		fmt.Fprintf(&b, "所属流程: %s(recall mode=flow 查看)\n", strings.Join(flows, ", "))
	}

	// 条目(superseded/refuted/retired 不出现,impl §7.3)。
	for i := range n.Entries {
		en := &n.Entries[i]
		if !en.Active() {
			continue
		}
		fmt.Fprintf(&b, "[%s|%s] %s(id %s", en.Kind, en.Confidence, en.Text, en.ID)
		if en.Author != "" {
			fmt.Fprintf(&b, ",author %s", en.Author)
		}
		b.WriteString(")\n")
		if en.Confidence == model.ConfidenceSuspect {
			b.WriteString("  ↳ 待重验:其依据已被勘误或锚已失配,重验前勿信(kb_verify)\n")
		}
	}

	if a.Mode == "history" {
		e.renderHistoryLocked(&b, nodeID, a.Before)
	}
	if a.Mode == "flow" {
		if flows := e.rt.ix.FlowsOf(nodeID); len(flows) == 0 {
			b.WriteString("该节点不属于任何流程。\n")
		} else {
			for _, fid := range flows {
				out, _ := e.flowView(fid)
				b.WriteString(out)
				b.WriteString("\n")
			}
		}
	}

	fmt.Fprintf(&b, "锚 hash: %s(写入时作 base_hash 可做乐观校验)", auto.curHash)
	b.WriteString(e.wipAttachment(append([]string{nodeID}, n.Lineage...)))
	// #26:读路径也要预算裁剪——写时预算只限单条,但一个节点的多条知识 + 长历史
	// 累加可爆读预算(注入/map 有裁剪,recall 原先没有)。上限 2500 token(单节点视图)。
	return framed(truncateToBudget(b.String(), 2500)), meta, nil
}

// truncateToBudget 按行截断到 token 预算内,尾部提示下钻(读路径防溢出)。
func truncateToBudget(s string, budget int) string {
	if EstimateTokens(s) <= budget {
		return s
	}
	lines := strings.Split(s, "\n")
	var kept []string
	used := 0
	for _, l := range lines {
		used += EstimateTokens(l) + 1
		if used > budget-40 {
			break
		}
		kept = append(kept, l)
	}
	return strings.Join(kept, "\n") + "\n……(内容超读预算已截断:用 mode=history + before 翻页,或对单条 kb_recall 取全量)"
}

// renderHistoryLocked 近 3 条全量 + 更早条数 + 时代摘要 + before 翻页(impl §7.3)。
func (e *Engine) renderHistoryLocked(b *strings.Builder, nodeID, before string) {
	n := e.rt.ix.Node(nodeID).Node
	hist := e.rt.ix.History(nodeID) // at 升序
	// 时代摘要折叠。
	var visible []model.Change
	folded := 0
	for _, c := range hist {
		if !n.EraUntil.IsZero() && !c.At.After(n.EraUntil) {
			folded++
			continue
		}
		visible = append(visible, c)
	}
	// before 翻页:取该记录之前的更早历史。
	if before != "" {
		cut := -1
		for i, c := range visible {
			if c.ID == before {
				cut = i
				break
			}
		}
		if cut >= 0 {
			visible = visible[:cut]
		}
	}
	b.WriteString("—— 来时路(近 3 条,at 降序)——\n")
	if n.EraSummary != "" {
		fmt.Fprintf(b, "[时代摘要,覆盖 %d 条早期记录] %s\n", folded, n.EraSummary)
	}
	if len(visible) == 0 {
		b.WriteString("(无未折叠历史)\n")
		return
	}
	start := max(len(visible)-3, 0)
	shown := visible[start:]
	for i := len(shown) - 1; i >= 0; i-- {
		c := shown[i]
		fmt.Fprintf(b, "%s %s\n  why: %s\n", c.At.Format("2006-01-02"), c.What, c.Why)
		for _, rj := range c.Rejected {
			fmt.Fprintf(b, "  ✗ 否决过: %s —— %s\n", rj.Option, rj.Reason)
		}
		if c.Overturns != "" {
			fmt.Fprintf(b, "  ↩ 推翻 %s;rebuttal: %s\n", c.Overturns, c.Rebuttal)
		}
		if c.Verified != "" {
			fmt.Fprintf(b, "  ✓ 验证: %s\n", c.Verified)
		}
		fmt.Fprintf(b, "  (id %s", c.ID)
		if c.Author != "" {
			fmt.Fprintf(b, ",author %s", c.Author)
		}
		b.WriteString(")\n")
	}
	if start > 0 {
		fmt.Fprintf(b, "……还有 %d 条更早记录(before=%q 翻页)\n", start, shown[0].ID)
	}
}

// listFlowsLocked 列出全部活跃流程/主题(mode=flow 空 query,或 kb_flow get)。
func (e *Engine) listFlowsLocked() string {
	var active []model.Flow
	for _, f := range e.rt.flows {
		if !f.Deprecated {
			active = append(active, f)
		}
	}
	if len(active) == 0 {
		return "库内暂无流程/主题节点(kb_flow create 建立)。"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "流程/主题节点(%d 个,mode=flow query=<id> 看详情):\n", len(active))
	for _, f := range active {
		fmt.Fprintf(&b, "- %s:%s(%d 步", f.ID, f.Title, len(f.Steps))
		if f.Troubleshoot != "" {
			b.WriteString(";含排障入口")
		}
		b.WriteString(")\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// flowView 渲染流程视图;query 可为 flow ID 或标题关键词。空 query 不匹配任何流程
// (#7:strings.Contains(title, "") 恒真,原先空 query 会返回第一个流程当"命中")。
func (e *Engine) flowView(query string) (string, bool) {
	query = strings.TrimSpace(query)
	if query == "" {
		return "", false
	}
	var f *model.Flow
	if strings.HasPrefix(query, "flow:") || strings.HasPrefix(query, "topic:") {
		f = e.rt.ix.Flow(query)
	}
	if f == nil {
		for i := range e.rt.flows {
			fl := &e.rt.flows[i]
			if !fl.Deprecated && (strings.Contains(fl.Title, query) || strings.Contains(query, fl.Title)) {
				f = fl
				break
			}
		}
	}
	if f == nil {
		return "", false
	}
	var b strings.Builder
	fmt.Fprintf(&b, "流程 %s:%s\n", f.ID, f.Title)
	needsReview := false
	for i, st := range f.Steps {
		cur := e.rt.ix.ResolveNodeID(st.Node)
		mark := ""
		if cur == "" {
			mark = " [引用节点已消失]"
			needsReview = true
		} else if e.rt.ix.Node(cur).Node.Status == model.StatusSuspect {
			mark = " [suspect]"
			needsReview = true
		}
		fmt.Fprintf(&b, "  %d. %s%s", i+1, st.Node, mark)
		if st.Note != "" {
			fmt.Fprintf(&b, " —— %s", st.Note)
		}
		b.WriteString("\n")
	}
	for _, c := range f.Conventions {
		fmt.Fprintf(&b, "  约定: %s\n", c)
	}
	if f.Troubleshoot != "" {
		fmt.Fprintf(&b, "  排障入口: %s\n", f.Troubleshoot)
	}
	if needsReview {
		b.WriteString("  ⚠ 流程引用的节点有 suspect/消失,流程待复核(kb_flow update)\n")
	}
	return b.String(), true
}

// missProtocolLocked 空手协议:把每次空手变成索引生长的机会(impl §7.3 定案)。
func (e *Engine) missProtocolLocked(query string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "关键词 %q 零命中。\n", query)
	// 符号名模糊匹配(包含式)。
	var fuzzy []string
	ql := strings.ToLower(query)
	for id := range e.rt.ix.Nodes() {
		_, symbol := model.SplitNodeID(id)
		if symbol != "" && strings.Contains(strings.ToLower(symbol), ql) {
			fuzzy = append(fuzzy, id)
			if len(fuzzy) >= 5 {
				break
			}
		}
	}
	if len(fuzzy) > 0 {
		sort.Strings(fuzzy)
		fmt.Fprintf(&b, "符号名模糊匹配: %s\n", strings.Join(fuzzy, "、"))
	}
	// 最相关分支的 map 摘要:取根一层。
	b.WriteString("库结构速览(kb_map 可下钻):\n")
	dirs := map[string]bool{}
	for id, ref := range e.rt.ix.Nodes() {
		if ref.Node.Level != model.LevelFile {
			continue
		}
		d := path.Dir(id)
		if d == "." {
			d = "(根)"
		}
		dirs[d] = true
	}
	var ds []string
	for d := range dirs {
		ds = append(ds, d)
	}
	sort.Strings(ds)
	if len(ds) > 12 {
		ds = ds[:12]
	}
	fmt.Fprintf(&b, "  %s\n", strings.Join(ds, ", "))
	b.WriteString("回填义务:若你随后用 grep/阅读定位到了目标,请把本次查询词 kb_remember 进该节点的 keywords——空手不回填,下次还空手。")
	return framed(b.String())
}

// ---- 读取时对账(impl §7.3)与 auto 现算 ----

type autoInfo struct {
	curHash   string
	signature string
	calls     []string
	calledBy  []string
	stale     bool
}

// reconcileOnReadLocked 重算命中节点的源码哈希:失配 → 即时降 suspect 并落盘;
// pending_anchor 且文件可解析 → 自动补锚。返回 auto 现算结果。
func (e *Engine) reconcileOnReadLocked(ref *index.NodeRef) autoInfo {
	n := ref.Node
	file, symbol := model.SplitNodeID(n.ID)
	if n.Level == model.LevelDir || n.Level == model.LevelProject {
		return autoInfo{curHash: n.Anchor.Hash}
	}
	src, err := os.ReadFile(filepath.Join(e.Store.RepoRoot(), filepath.FromSlash(file)))
	if err != nil {
		// 源文件读不到(已删):orphaned 语义由对账处理;这里如实返回旧锚。
		return autoInfo{curHash: n.Anchor.Hash}
	}
	p := e.Reg.ForFile(file)
	if p == nil {
		return autoInfo{curHash: n.Anchor.Hash}
	}
	syms, err := p.Parse(file, src)
	if err != nil {
		return autoInfo{curHash: n.Anchor.Hash} // 不可解析:锚保持,不降级(PARSE 三态)
	}

	var auto autoInfo
	auto.curHash = n.Anchor.Hash
	var cur *parser.Symbol
	if symbol == "" {
		fh := parser.FileHash(syms)
		auto.curHash = fh
		// #42(R2-A3 补齐文件节点分支):已 suspect 且仍失配同样要报 stale,
		// 否则第二次读取横幅消失,AI 以为没问题。
		if n.Anchor.Hash != "" && fh != n.Anchor.Hash && hasActiveEntries(n) &&
			(n.Status == model.StatusFresh || n.Status == model.StatusSuspect) {
			e.downgradeLocked(ref)
			auto.stale = true
		}
		return auto
	}
	for i := range syms {
		if syms[i].Name == symbol {
			cur = &syms[i]
			break
		}
	}
	if cur == nil {
		// 符号不在原位 = 失配的一种:有知识则降 suspect(对账/迁移由 init 兜)。
		if hasActiveEntries(n) && n.Status == model.StatusFresh {
			e.downgradeLocked(ref)
			auto.stale = true
		}
		return auto
	}
	auto.curHash = cur.Hash
	auto.signature = parser.Signature(*cur)
	if calls, err := parser.SameFileCalls(file, src); err == nil {
		auto.calls = calls[symbol]
		for caller, callees := range calls {
			for _, cee := range callees {
				if cee == symbol {
					auto.calledBy = append(auto.calledBy, caller)
				}
			}
		}
		sort.Strings(auto.calledBy)
	}

	// 读路径落盘尽力而为:失败不阻断读取(内存态已更新、返回信息正确),记警下次重试。
	bestEffortSave := func() {
		if err := e.saveNodeShardLocked(ref); err != nil {
			e.warnOpsLocked("读取时对账落盘失败(下次重试):" + err.Error())
		}
	}
	switch {
	case n.PendingAnchor:
		// 待补锚:文件重新可解析,自动补锚(impl §7.3 第四情形收尾)。
		n.Anchor.Hash, n.Anchor.StructHash, n.Anchor.Lines = cur.Hash, cur.StructHash, cur.Lines
		n.PendingAnchor = false
		bestEffortSave()
	case n.Anchor.Hash != "" && cur.Hash != n.Anchor.Hash && (n.Status == model.StatusFresh || n.Status == model.StatusSuspect) && hasActiveEntries(n):
		// #42:哈希失配就报 stale,不论当前是 fresh 还是【已 suspect 仍失配】——
		// 否则第二次读一个已 suspect 且代码又变的节点,横幅消失、AI 以为没问题。
		e.downgradeLocked(ref)
		auto.stale = true
	case n.Status == model.StatusSuspect && cur.Hash == n.Anchor.Hash:
		// 代码回到锚定状态:恢复(与 init 对账同规则)。
		n.Status = model.StatusFresh
		bestEffortSave()
	}
	return auto
}

// downgradeLocked 降 suspect 落盘(保留旧锚,重验即重锚的基准)。
// 读路径上的降级:落盘失败不阻断本次读取(内存已降级,返回给用户的信息正确),
// 但把错误记进告警,下次写路径会重试落盘。
func (e *Engine) downgradeLocked(ref *index.NodeRef) {
	ref.Node.Status = model.StatusSuspect
	if err := e.saveNodeShardLocked(ref); err != nil {
		e.warnOpsLocked("降级落盘失败(下次写路径重试):" + err.Error())
	}
}

// saveNodeShardLocked 把 ref 所在分片写回(未知字段由 store 合并保留)。
// 返回错误——写路径调用方必须上抬(#18/#31:原先 _ = 吞掉,落盘失败静默丢数据)。
func (e *Engine) saveNodeShardLocked(ref *index.NodeRef) error {
	cs := e.rt.cache.Shards()[ref.ShardRel]
	if cs == nil || cs.Shard == nil {
		return nil
	}
	path := filepath.Join(e.Store.Dir(), filepath.FromSlash(ref.ShardRel))
	return e.Store.SaveShard(path, cs.Shard, cs.Raw)
}

// ---- GET /inject(hook 注入端点,impl §7.1) ----

// Inject 组装一个文件的注入文本(knowledge.md §9.2 预算规则,≤1500 token)。
func (e *Engine) Inject(file, sid string) (string, error) {
	if err := e.requireInit(); err != nil {
		return "", err
	}
	if err := e.Sync(); err != nil {
		return "", err
	}
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()

	file = strings.Trim(filepath.ToSlash(file), "/")
	fileRef := e.rt.ix.Node(file)
	if fileRef == nil {
		return "", kbErr("NODE_NOT_FOUND", "文件 "+file+" 无节点", "路径须相对仓库根;kb_map 可确认")
	}

	var parts []string
	var nodeIDs []string

	// 过时警报置顶(§9.2)。
	for id := range e.rt.ix.Nodes() {
		f, _ := model.SplitNodeID(id)
		if f != file {
			continue
		}
		nodeIDs = append(nodeIDs, id)
		if l := e.ledger(sid); l != nil {
			if prev, ok := l.reads[id]; ok {
				cur := e.rt.ix.Node(id).Node.Anchor.Hash
				if prev.Hash != "" && cur != "" && prev.Hash != cur {
					parts = append(parts, e.staleAlert(id, prev))
				}
			}
		}
	}
	sort.Strings(nodeIDs)

	// 任务态。
	if w := e.wipAttachment(nodeIDs); w != "" {
		parts = append(parts, strings.TrimSpace(w))
	}

	// 本文件节点全量(快照 + 负知识全量)。
	var b strings.Builder
	fmt.Fprintf(&b, "文件 %s %s\n", file, nodeLine(fileRef.Node))
	for _, id := range nodeIDs {
		ref := e.rt.ix.Node(id)
		if id == file || !hasActiveEntries(ref.Node) {
			continue
		}
		_, symbol := model.SplitNodeID(id)
		fmt.Fprintf(&b, "#%s(%s)\n", symbol, ref.Node.Status)
		for i := range ref.Node.Entries {
			en := &ref.Node.Entries[i]
			if en.Active() {
				fmt.Fprintf(&b, "  [%s|%s] %s\n", en.Kind, en.Confidence, en.Text)
			}
		}
		if flows := e.rt.ix.FlowsOf(id); len(flows) > 0 {
			fmt.Fprintf(&b, "  流程: %s\n", strings.Join(flows, ", "))
		}
	}
	// 近 3 条历史 + 负知识(rejected 全量,§9.2:负知识永远优先于摘要)。
	var recent []model.Change
	for _, id := range nodeIDs {
		recent = append(recent, e.rt.ix.History(id)...)
	}
	sort.Slice(recent, func(i, j int) bool { return recent[i].At.Before(recent[j].At) })
	if len(recent) > 0 {
		b.WriteString("近期变更:\n")
		start := max(len(recent)-3, 0)
		for i := len(recent) - 1; i >= start; i-- {
			c := recent[i]
			fmt.Fprintf(&b, "  %s %s(why: %s)\n", c.At.Format("01-02"), c.What, c.Why)
			for _, rj := range c.Rejected {
				fmt.Fprintf(&b, "    ✗ 否决过: %s —— %s\n", rj.Option, rj.Reason)
			}
		}
		if start > 0 {
			fmt.Fprintf(&b, "  (还有 %d 条,kb_recall mode=history)\n", start)
		}
	}
	// 祖先链 summary 一行。
	for d := path.Dir(file); d != "."; d = path.Dir(d) {
		if ref := e.rt.ix.Node(model.DirNodeID(d)); ref != nil {
			if s := firstSummary(ref.Node); s != "" {
				fmt.Fprintf(&b, "祖先 %s/: %s\n", d, s)
			}
		}
	}
	if pref := e.rt.ix.Node(model.ProjectNodeID); pref != nil {
		if s := firstSummary(pref.Node); s != "" {
			fmt.Fprintf(&b, "项目: %s\n", s)
		}
	}
	parts = append(parts, strings.TrimRight(b.String(), "\n"))
	out := strings.Join(parts, "\n")

	// 预算 ≤1500 token,超出折叠(§9.2)。
	if EstimateTokens(out) > 1500 {
		lines := strings.Split(out, "\n")
		var kept []string
		used := 0
		for _, l := range lines {
			used += EstimateTokens(l) + 1
			if used > 1400 {
				break
			}
			kept = append(kept, l)
		}
		out = strings.Join(kept, "\n") + "\n……(超预算折叠,kb_recall 可下钻)"
	}
	return framed(out), nil
}
