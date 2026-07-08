package engine

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

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

	// 解析失败文件:全库 parse 扫描,60s TTL 缓存(kb_status 最大单项成本,
	// casino 实测数百毫秒)。#21:锁外做——只读 Store/Reg 不碰 rt 状态。
	parseFailed := e.parseFailedCached()
	// 热区频率因子同样锁外跑 git(knowledge.md §12.1);60s TTL 缓存——
	// git log 在大仓库百毫秒级,频繁 kb_status 不该每次付。
	gitCounts := e.gitCountsCached()

	// R29 批次2:Status 改用读锁——ensureCallGraphLocked 走 cgMu 独立锁,
	// computeDebtsLocked/节点遍历全纯读。parseFailed/gitCounts 已锁外算。
	e.rt.mu.RLock()
	defer e.rt.mu.RUnlock()

	total, digested, suspect, orphaned, pending := 0, 0, 0, 0, 0
	conflicts := 0
	var orphanIDs []string
	type fileDigest struct{ done, total int }
	perFile := map[string]*fileDigest{} // 符号节点按文件聚合(热点消化比)
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
			if n.Level == model.LevelFunction || n.Level == model.LevelDecl {
				file, _ := model.SplitNodeID(n.ID)
				fd := perFile[file]
				if fd == nil {
					fd = &fileDigest{}
					perFile[file] = fd
				}
				fd.total++
				if hasActiveEntries(n) {
					fd.done++
				}
			}
		}
	}
	sort.Strings(orphanIDs)
	_, jstats := e.rt.cache.Journal()

	var b strings.Builder
	fmt.Fprintf(&b, "repoRoot: %s\nschema: %d\n", e.Store.RepoRoot(), model.SchemaVersion)
	// 覆盖率一位小数:68/15598 取整成 "0%" 会把正常的按需消化误读成没干活。
	fmt.Fprintf(&b, "节点: %d(已消化 %d,覆盖率 %.1f%%)\n", total, digested, pct(digested, total))
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

	// 热点待消化(knowledge.md §12.1:热度 = git 近期改动频率 × 跨文件被调中心度,
	// +1 平滑——非 git 仓库/新文件退化为单因子仍可排序)。消化优先级的机械输出,
	// 消化本身仍由 AI 会话做(kb_recall 读原文 → kb_remember 沉淀)。
	if cg := e.ensureCallGraphLocked(); cg != nil && len(perFile) > 0 {
		centrality := cg.fileCentrality()
		type hotspot struct {
			file        string
			heat        int
			chg, ctr    int
			done, total int
		}
		var hot []hotspot
		for file, fd := range perFile {
			if fd.done >= fd.total {
				continue // 全消化的文件不是"待消化"热点
			}
			chg, ctr := gitCounts[file], centrality[file]
			hot = append(hot, hotspot{file, (1 + chg) * (1 + ctr), chg, ctr, fd.done, fd.total})
		}
		sort.Slice(hot, func(i, j int) bool {
			if hot[i].heat != hot[j].heat {
				return hot[i].heat > hot[j].heat
			}
			return hot[i].file < hot[j].file
		})
		if len(hot) > 10 {
			hot = hot[:10] // TOP10:对齐 M1.4 种子协议("消化 10 个热点")
		}
		if len(hot) > 0 && hot[0].heat > 1 { // 双因子全零(冷库无 git)时不值得占版面
			b.WriteString("热点待消化(90 天改动 × 跨文件被调;读原文后 kb_remember 只存【代码上看不出来的】——契约/坑/为什么;热点改动频繁,优先沉淀跨改动仍成立的契约/不变量,实现细节易腐):\n")
			for _, h := range hot {
				fmt.Fprintf(&b, "  - %s 热度 %d(改 %d 次 × 被调 %d)消化 %d/%d\n",
					h.file, h.heat, h.chg, h.ctr, h.done, h.total)
			}
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
	// R29 批次3:config.yaml 解析失败不再静默吞——kb_status 显式提示。
	if cerr := e.configError(); cerr != nil {
		fmt.Fprintf(&b, "⚠ config.yaml 解析失败(用空配置运行,includes/excludes/extensions 可能失效):%v\n", cerr)
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
	// R29 批次2:Map 是纯读(不调 reconcile/ledger/callgraph),用读锁,多并发不互斥。
	e.rt.mu.RLock()
	defer e.rt.mu.RUnlock()

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

	// R29 批次3:覆盖率单遍预计算。原先 coverage(prefix) 每目录遍历全索引 O(D×N),
	// 几千节点仓库 kb_map 主成本。现一遍遍历所有节点,每个符号节点累加进它的各祖先目录。
	type covEntry struct{ dig, tot int }
	cov := map[string]*covEntry{} // dir → {dig, tot}
	covOf := func(d string) *covEntry {
		c := cov[d]
		if c == nil {
			c = &covEntry{}
			cov[d] = c
		}
		return c
	}
	for id, ref := range e.rt.ix.Nodes() {
		if ref.Node.Level == model.LevelDir || ref.Node.Level == model.LevelProject {
			continue
		}
		file, _ := model.SplitNodeID(id)
		// fileDir 取文件所在目录(internal/auth/login.go → internal/auth)。
		dir := path.Dir(file)
		if dir == "." || dir == "/" || dir == "" {
			continue // 文件在根下,没有目录壳(极少)
		}
		c := covOf(dir)
		c.tot++
		if hasActiveEntries(ref.Node) {
			c.dig++
		}
		// 上溯祖先目录也累加(父目录覆盖率含子树)。
		for parent := path.Dir(dir); parent != "." && parent != "/" && parent != ""; parent = path.Dir(parent) {
			pc := covOf(parent)
			pc.tot++
			if hasActiveEntries(ref.Node) {
				pc.dig++
			}
		}
	}
	coverage := func(prefix string) (dig, tot int) {
		if c := cov[prefix]; c != nil {
			return c.dig, c.tot
		}
		return 0, 0
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
	// R29 批次2:Recall 改用读锁——reconcileOnReadLocked 状态对账已搬进
	// reconcileAllLocked(reloadLocked 内,写锁),recordRead 走 sessionMu 独立锁,
	// recallNodeLocked 其余全是纯读。多会话 recall 可并发。
	e.rt.mu.RLock()
	defer e.rt.mu.RUnlock()

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
		// 结构扩展一跳(检索三级递进第 2 级,knowledge.md §10.2):
		// 沿调用边与流程引用带出关键词没匹配上、但结构相连的节点。
		if nbs := e.structuralNeighborsLocked(ids, 5); len(nbs) > 0 {
			b.WriteString("结构相邻(一跳,关键词未命中但与命中节点相连):\n")
			for _, nb := range nbs {
				fmt.Fprintf(&b, "- %s %s(%s)\n", nb.id, nodeLine(e.rt.ix.Node(nb.id).Node), nb.via)
			}
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

// SessionSummary 聚合"本会话学到了什么"(R29 批次4):按 session 过滤 usage log,
// 统计写工具调用次数。零存储成本(usage log 已有 session 字段)。
func (e *Engine) SessionSummary(sid string) (string, error) {
	if sid == "" {
		return "无会话 ID(匿名连接),无摘要。", nil
	}
	recs, err := e.Store.LoadUsage()
	if err != nil {
		return "", err
	}
	writeTools := map[string]bool{
		"kb_remember": true, "kb_record_change": true, "kb_verify": true,
		"kb_revert": true, "kb_adopt": true, "kb_flow": true, "kb_maintain": true,
	}
	var reads, writes int
	writeByTool := map[string]int{}
	for _, r := range recs {
		if r.Session != sid {
			continue
		}
		if writeTools[r.Tool] {
			writes++
			if r.OK {
				writeByTool[r.Tool]++
			}
		} else {
			reads++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "会话 %s 摘要:\n", sid)
	fmt.Fprintf(&b, "  读取 %d 次(recall/map/inject),写入 %d 次。\n", reads, writes)
	if writes == 0 {
		b.WriteString("  本会话未沉淀任何知识。\n")
	} else {
		b.WriteString("  写入明细(成功次数):\n")
		for _, tool := range []string{"kb_remember", "kb_record_change", "kb_verify", "kb_revert", "kb_adopt", "kb_flow", "kb_maintain"} {
			if n := writeByTool[tool]; n > 0 {
				fmt.Fprintf(&b, "    %s: %d\n", tool, n)
			}
		}
		b.WriteString("  任务结束时:确认已记账的变更(kb_record_change),沉淀难懂的知识(kb_remember)。\n")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// rankEntry 给活跃 entry 一个排序键:verified 优先 > inferred > suspect;
// 同级按 ConfirmedAt/At 倒序(最近确认的先存活过 token 预算)。R29 批次4 注入排序。
func rankEntry(e *model.Entry) (confidenceRank int, recent time.Time) {
	switch e.Confidence {
	case model.ConfidenceVerified:
		confidenceRank = 0
	case model.ConfidenceInferred:
		confidenceRank = 1
	case model.ConfidenceSuspect:
		confidenceRank = 2
	default: // derived 等
		confidenceRank = 3
	}
	recent = e.ConfirmedAt
	if e.At.After(recent) {
		recent = e.At
	}
	return
}

// activeEntriesSorted 返回按重要性排序的活跃 entry 指针切片(不改原存储顺序)。
// R29 批次4:recall/inject 渲染前排序,让 verified + 近期确认的活过 token 预算截断。
func activeEntriesSorted(n *model.Node) []*model.Entry {
	var out []*model.Entry
	for i := range n.Entries {
		if n.Entries[i].Active() {
			out = append(out, &n.Entries[i])
		}
	}
	sort.Slice(out, func(i, j int) bool {
		ci, ri := rankEntry(out[i])
		cj, rj := rankEntry(out[j])
		if ci != cj {
			return ci < cj
		}
		return ri.After(rj) // 近期在前
	})
	return out
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
	// 来时路(2026-07-04,实战反馈"冷启动价值低"的机械解):骨架/可疑节点的 recall
	// 自动附该文件近期提交——零 LLM 成本的考古线索,"为什么长这样"不再空手。
	// 单文件 git log 毫秒级,与本函数已有的单文件 parse(reconcileOnRead)同量级,
	// 不违 #21(那是针对全库扫描不进锁)。
	if file, _ := model.SplitNodeID(nodeID); file != "" && !strings.HasSuffix(file, "/") &&
		nodeID != model.ProjectNodeID && (!hasActiveEntries(n) || n.Status == model.StatusSuspect || auto.stale) {
		// R29 批次3:用缓存 git trail(60s TTL),消除读锁内稳态子进程。
		if trail := e.cachedGitTrail(file); trail != "" {
			b.WriteString("来时路(近期提交——「为什么长这样」的档案,深挖 git show/blame):\n")
			b.WriteString(trail)
		}
	}

	fmt.Fprintf(&b, "节点: %s(%s,%s)\n", nodeID, n.Level, n.Status)
	if auto.signature != "" {
		fmt.Fprintf(&b, "签名: %s\n", auto.signature)
	}
	if len(auto.calls) > 0 {
		fmt.Fprintf(&b, "调用: %s\n", strings.Join(auto.calls, ", "))
	}
	if len(auto.calledBy) > 0 {
		fmt.Fprintf(&b, "被调用: %s\n", strings.Join(auto.calledBy, ", "))
	}
	if len(auto.impls) > 0 {
		fmt.Fprintf(&b, "实现者(方法集匹配): %s\n", strings.Join(auto.impls, ", "))
	}
	if len(auto.ifaces) > 0 {
		fmt.Fprintf(&b, "实现接口: %s\n", strings.Join(auto.ifaces, ", "))
	}
	if len(n.Keywords) > 0 {
		fmt.Fprintf(&b, "keywords: %s\n", strings.Join(n.Keywords, ", "))
	}
	if flows := e.rt.ix.FlowsOf(nodeID); len(flows) > 0 {
		fmt.Fprintf(&b, "所属流程: %s(recall mode=flow 查看)\n", strings.Join(flows, ", "))
	}

	// 条目(superseded/refuted/retired 不出现,impl §7.3)。
	// R29 批次4:按重要性排序——verified + 近期确认的在前,活过 token 预算截断。
	anchorless := n.Anchor.Hash == "" && !n.PendingAnchor
	for _, en := range activeEntriesSorted(n) {
		fmt.Fprintf(&b, "[%s|%s] %s(id %s", en.Kind, en.Confidence, en.Text, en.ID)
		if en.Author != "" {
			fmt.Fprintf(&b, ",author %s", en.Author)
		}
		b.WriteString(")\n")
		if en.Confidence == model.ConfidenceSuspect {
			b.WriteString("  ↳ 待重验:其依据已被勘误或锚已失配,重验前勿信(kb_verify)\n")
		}
		// 矛盾待裁决的双向呈现(§12.4:并存呈现防静默覆盖;一方退场自动解除)。
		for _, d := range en.Disputes {
			if t := e.rt.ix.EntryByRef(d); t != nil && t.Active() {
				fmt.Fprintf(&b, "  ↳ ⚠ 与 %s 矛盾待裁决:两者必有一错,裁决前都别信(kb_verify refute 错误方/obsolete 过时方)\n", d)
			}
		}
		for _, from := range e.rt.ix.DisputedBy(nodeID + "#" + en.ID) {
			if t := e.rt.ix.EntryByRef(from); t != nil && t.Active() {
				fmt.Fprintf(&b, "  ↳ ⚠ 被 %s 声明矛盾待裁决:两者必有一错,裁决前都别信\n", from)
			}
		}
		// 非代码知识的时间锚标注(§8.4):无代码锚不会自动失效,超期要诚实提示。
		if anchorless {
			last := en.At
			if en.ConfirmedAt.After(last) {
				last = en.ConfirmedAt
			}
			if e.now().Sub(last) > reviewOverdueAfter {
				if last.IsZero() {
					b.WriteString("  ↳ 无确认时间记录(旧数据),可能过期:核实后 kb_verify confirm 建立时间锚\n")
				} else {
					fmt.Fprintf(&b, "  ↳ 上次确认 %s(超 %d 天),可能过期:核实后 kb_verify confirm 刷新\n",
						last.Format("2006-01-02"), int(e.now().Sub(last).Hours()/24))
				}
			}
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
	impls     []string // 接口节点:实现者(方法集匹配)
	ifaces    []string // 类型节点:所实现的仓内接口
	stale     bool
}

// reconcileOnReadLocked 现算命中节点的展示派生数据(curHash/signature/calls)并据
// 已对账的 status 决定 stale 横幅。
//
// R29 批次2 状态对账外移:原先这里还做 suspect 降级/anchor 补全/落盘(读时写)——
// 现在那些状态变更搬进了 reconcileAllLocked(reloadLocked 调,写锁内),读路径变纯读。
// 这里仍读源文件算 curHash/signature(auto 展示数据本就要读源码,增量成本≈0),
// 并比对已落盘的 status 决定要不要报 stale。
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
		fh := parser.HashFileFor(p, syms, src)
		auto.curHash = fh
		// #42:哈希失配 + 有活跃知识 + fresh/suspect → 报 stale。
		// 状态已是 reconcileAllLocked 预算的(失配的已降 suspect),这里只据 status 报横幅。
		if n.Anchor.Hash != "" && fh != n.Anchor.Hash && hasActiveEntries(n) &&
			(n.Status == model.StatusFresh || n.Status == model.StatusSuspect) {
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
		// 符号不在原位 = 失配的一种(reconcileAllLocked 已降 suspect);这里只报横幅。
		if hasActiveEntries(n) && n.Status == model.StatusSuspect {
			auto.stale = true
		}
		return auto
	}
	auto.curHash = cur.Hash
	auto.signature = parser.Signature(*cur)
	// 调用关系走全仓调用图(impl §5 修订;auto 派生现算不落盘)。
	// 展示上限 12(限额哲学):同文件裸名在前,跨文件完整 node ID 可直接再 recall。
	if cg := e.ensureCallGraphLocked(); cg != nil {
		nodeID := file + "#" + symbol
		auto.calls = displayEdges(cg.callsOf(nodeID), file, 12)
		auto.calledBy = displayEdges(cg.calledByOf(nodeID), file, 12)
		// 接口↔实现(方法集匹配,codegraph 启发):接口节点列实现者,类型节点列所实现接口。
		auto.impls = displayEdges(cg.implementationsOf(nodeID), file, 12)
		auto.ifaces = displayEdges(cg.interfacesOf(nodeID), file, 12)
	}
	// #42:哈希失配就报 stale,不论当前是 fresh 还是【已 suspect 仍失配】。
	// 状态变更已在 reconcileAllLocked 预算(失配→suspect),这里只据现状报横幅。
	if n.Anchor.Hash != "" && cur.Hash != n.Anchor.Hash && (n.Status == model.StatusFresh || n.Status == model.StatusSuspect) && hasActiveEntries(n) {
		auto.stale = true
	}
	return auto
}

// reconcileAllLocked 是读路径状态对账的预算版(R29 批次2 从 reconcileOnReadLocked 外移):
// 遍历所有有活跃知识、状态 fresh/suspect 或 pending_anchor 的节点,比对源码哈希——
// 失配降 suspect、回到锚定恢复 fresh、pending_anchor 补全——批量落盘。
// 在 reloadLocked 末尾调(写锁内),使读路径不必再做这些读时写。
//
// 成本控制:只对"有活跃知识 且 (fresh|suspect|pending)"的节点做(纯骨架节点跳过),
// 且每文件 parse 结果用 rcParse 缓存(同文件多符号共享,避免重复解析)。
func (e *Engine) reconcileAllLocked() {
	repo := e.Store.RepoRoot()
	type parsed struct {
		syms []parser.Symbol
		src  []byte
		p    parser.Parser
		ok   bool
	}
	fileCache := map[string]*parsed{}
	dirty := map[string]bool{} // 有状态变更的分片,需落盘

	for _, ref := range e.rt.ix.Nodes() {
		n := ref.Node
		if !hasActiveEntries(n) && !n.PendingAnchor {
			continue
		}
		if n.Status != model.StatusFresh && n.Status != model.StatusSuspect && !n.PendingAnchor {
			continue
		}
		file, symbol := model.SplitNodeID(n.ID)
		if file == "" || strings.HasSuffix(file, "/") {
			continue
		}
		pp, ok := fileCache[file]
		if !ok {
			pp = &parsed{}
			fileCache[file] = pp
			src, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(file)))
			if err != nil {
				continue
			}
			p := e.Reg.ForFile(file)
			if p == nil {
				continue
			}
			syms, err := p.Parse(file, src)
			if err != nil {
				continue // 不可解析:锚保持不降级(PARSE 三态)
			}
			pp.syms, pp.src, pp.p, pp.ok = syms, src, p, true
		}
		if !pp.ok {
			continue
		}
		var curHash string
		if symbol == "" {
			curHash = parser.HashFileFor(pp.p, pp.syms, pp.src)
		} else {
			for i := range pp.syms {
				if pp.syms[i].Name == symbol {
					// pending_anchor 补全需要完整 Symbol(StructHash/Lines)。
					if n.PendingAnchor {
						n.Anchor.Hash = pp.syms[i].Hash
						n.Anchor.StructHash = pp.syms[i].StructHash
						n.Anchor.Lines = pp.syms[i].Lines
						n.PendingAnchor = false
						dirty[ref.ShardRel] = true
					}
					curHash = pp.syms[i].Hash
					break
				}
			}
			if curHash == "" {
				// 符号不在原位:有活跃知识则降 suspect。
				if hasActiveEntries(n) && n.Status == model.StatusFresh {
					n.Status = model.StatusSuspect
					dirty[ref.ShardRel] = true
				}
				continue
			}
		}
		switch {
		case n.Anchor.Hash != "" && curHash != n.Anchor.Hash &&
			(n.Status == model.StatusFresh || n.Status == model.StatusSuspect) && hasActiveEntries(n):
			n.Status = model.StatusSuspect
			dirty[ref.ShardRel] = true
		case n.Status == model.StatusSuspect && curHash == n.Anchor.Hash:
			n.Status = model.StatusFresh
			dirty[ref.ShardRel] = true
		}
	}
	for shardRel := range dirty {
		ref := &index.NodeRef{ShardRel: shardRel}
		if err := e.saveNodeShardLocked(ref); err != nil {
			e.warnOpsLocked("reconcileAll 落盘失败(下次重试):" + err.Error())
		}
	}
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
// tool 是触发 hook 的宿主工具名(Read/Edit/Write/…,可空):写事件在注入尾部追加
// 记账提醒——"改完的当下"是记账遵守率的黄金时点(2026-07-04,纪律依赖的机械解)。
func (e *Engine) Inject(file, sid, tool string) (string, error) {
	if err := e.requireInit(); err != nil {
		return "", err
	}
	if err := e.Sync(); err != nil {
		return "", err
	}
	// R29 批次2:Inject 改用读锁——ledger 走 sessionMu,staleAlert/computeDebts/
	// wipAttachment 全纯读。hook 注入高频(每次 Read/Edit),读锁让并发不互斥。
	e.rt.mu.RLock()
	defer e.rt.mu.RUnlock()

	file = strings.Trim(filepath.ToSlash(file), "/")
	fileRef := e.rt.ix.Node(file)
	if fileRef == nil {
		return "", kbErr("NODE_NOT_FOUND", "文件 "+file+" 无节点", "路径须相对仓库根;kb_map 可确认")
	}

	var parts []string
	var nodeIDs []string

	// 过时警报置顶(§9.2)。R29 批次3:用 fileNodes 索引免扫全表。
	for _, id := range e.rt.ix.FileNodes(file) {
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

	// 就地欠账提示(2026-07-04,实战反馈"债在积累没人清"):AI 正在动这个文件,
	// 理解新鲜、人在现场——顺手清账成本最低的时点。只报本文件的债,不刷全库。
	debtCount := 0
	for _, d := range e.computeDebtsLocked() {
		if f, _ := model.SplitNodeID(d.Node); f == file {
			debtCount++
		}
	}
	if debtCount > 0 {
		parts = append(parts, fmt.Sprintf("⚙ 本文件有 %d 条维护欠账(kb_maintain next scope=%s 顺手清一条)", debtCount, file))
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
		// R29 批次4:按重要性排序注入(verified 优先活过 token 预算)。
		for _, en := range activeEntriesSorted(ref.Node) {
			fmt.Fprintf(&b, "  [%s|%s] %s\n", en.Kind, en.Confidence, en.Text)
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
	// 写事件的记账提醒(改完的当下最有效;放预算裁剪之前的尾部,预算不裁尾部提醒?
	// ——提醒必须存活,单独追加在裁剪之后)。
	writeEvent := tool == "Edit" || tool == "Write" || tool == "MultiEdit" || tool == "NotebookEdit"

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
	// 记账提醒追加在预算裁剪之后:提醒必须存活,不参与折叠。
	if writeEvent {
		out += "\n✍ 你刚修改了本文件:该逻辑修改单元收尾时必须 kb_record_change(改了什么/为什么/否决了什么;一次重构=一条,nodes 列全)。"
	}
	return framed(out), nil
}
