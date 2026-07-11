package engine

import (
	"bytes"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/zdypro888/iknowledge/internal/model"
	"github.com/zdypro888/iknowledge/internal/parser"
)

// callGraph 是全仓调用图(impl §5 修订:AST 近似,无类型检查)。
// auto 派生值,不落盘(impl §3):serve 期驻内存,按文件 mtime+size 指纹增量重取,
// 任一文件变更才整体重连边(连边纯内存,毫秒级;提取才是大头)。
//
// 归位规则(近似的三条边界在 parser.CallRef 留痕):
//  1. 无限定引用(直呼/接收者自调)→ 同包符号表;
//  2. 限定引用且限定名是 import → 仅模块内包(go.mod module 前缀)归位,库外丢弃;
//  3. 限定引用且非 import(局部变量上的方法调用)→ 同包唯一方法基名启发。
//
// 同名歧义(如 build tag 双版本 lock_unix/lock_other 的同名方法):
// 调用方自己文件里声明的优先,否则包内唯一才归位,歧义丢边——宁缺毋错。
type callGraph struct {
	module   string // go.mod module 路径;"" = 非模块仓库(仅同包归位)
	modMtime int64
	modSize  int64

	files   map[string]*fileCallsEntry // rel → 提取结果 + 指纹
	edges   map[string][]string        // node ID → 被它调用的 node ID(升序)
	reverse map[string][]string        // node ID → 调用它的 node ID(升序)

	// 接口→实现关系(方法集匹配,2026-07-04):双向,均为 node ID 升序。
	implsOf  map[string][]string // 接口类型节点 → 实现类型节点
	ifacesOf map[string][]string // 类型节点 → 它实现的仓内接口节点
}

type fileCallsEntry struct {
	mtimeNS int64
	size    int64
	fc      *parser.FileCalls
}

// packageKey 区分同一目录内合法并存的 production package 与 external-test
// package（例如 package auth 与 package auth_test）。Go 的包身份不只是目录：
// 若只按目录建声明表，两边同名 helper 会互相制造歧义并把两边的真调用边都丢掉。
type packageKey struct {
	dir string
	pkg string
}

// ensureCallGraphLocked 惰性构建/增量刷新调用图。
// R29 批次2:走 cgMu 独立锁(不再要求持 rt.mu)——它是派生值,只读文件系统+自身
// files map,不依赖 rt.mu 保护的 cache/ix;读路径可并发刷新它而不互斥。
// 图是尽力而为的派生值:清单/解析失败返回 nil,调用方按"无图"降级,不阻断读路径。
func (e *Engine) ensureCallGraphLocked() *callGraph {
	e.rt.cgMu.Lock()
	defer e.rt.cgMu.Unlock()
	repo := e.Store.RepoRoot()
	// R29 批次3:callgraph 不走 cachedSourceFiles——它的增量语义依赖实时文件清单
	// 检测增删(cachedSourceFiles 的 60s TTL 会让删除的文件滞留,破坏增量)。config 用缓存。
	cfg := e.cachedConfig()
	rels, err := listSourceFiles(repo, e.Reg, cfg)
	if err != nil {
		return nil
	}

	// 已发布给读者的 callGraph 视为不可变快照。刷新必须在副本上进行:
	// 调用方拿到返回值后不再持 cgMu,若这里原地改 edges/reverse/files,
	// 另一个并发 Recall/Map 会与刷新形成 data race。
	cg := cloneCallGraph(e.rt.cg)
	changed := cg.refreshModule(repo)

	alive := make(map[string]bool, len(rels))
	for _, rel := range rels {
		alive[rel] = true
		st, err := safeRepoFileInfo(repo, rel)
		if err != nil {
			// listSourceFiles 与这里的读取之间仍可能发生替换；安全 stat
			// 一旦拒绝该路径，必须同时清掉旧提取结果，不能留下陈旧调用边。
			if _, ok := cg.files[rel]; ok {
				delete(cg.files, rel)
				changed = true
			}
			continue
		}
		if prev := cg.files[rel]; prev != nil && prev.mtimeNS == st.ModTime().UnixNano() && prev.size == st.Size() {
			continue
		}
		entry := &fileCallsEntry{mtimeNS: st.ModTime().UnixNano(), size: st.Size()}
		if ex, ok := e.Reg.ForFile(rel).(parser.CallExtractor); ok {
			src, err := safeRepoRead(repo, rel)
			if err == nil && !parser.IsGenerated(src) {
				if fc, err := ex.FileCalls(rel, src); err == nil {
					entry.fc = fc
				}
			}
		}
		// 提取失败/生成代码/无提取能力:留空条目占位——指纹避免每次重读重试。
		cg.files[rel] = entry
		changed = true
	}
	for rel := range cg.files {
		if !alive[rel] {
			delete(cg.files, rel)
			changed = true
		}
	}

	if changed || cg.edges == nil {
		cg.resolve()
	}
	e.rt.cg = cg
	return cg
}

// cloneCallGraph 建可变的下一版。fileCallsEntry 与边切片在构建后均只读,
// 未发生变化时可共享;files map 会在增量刷新中增删,必须复制。
func cloneCallGraph(src *callGraph) *callGraph {
	if src == nil {
		return &callGraph{files: map[string]*fileCallsEntry{}}
	}
	dst := &callGraph{
		module: src.module, modMtime: src.modMtime, modSize: src.modSize,
		files: make(map[string]*fileCallsEntry, len(src.files)),
		edges: src.edges, reverse: src.reverse,
		implsOf: src.implsOf, ifacesOf: src.ifacesOf,
	}
	for rel, entry := range src.files {
		dst.files[rel] = entry
	}
	return dst
}

// refreshModule 按 go.mod 指纹重读模块路径;变更返回 true。
func (cg *callGraph) refreshModule(repo string) bool {
	st, err := safeRepoFileInfo(repo, "go.mod")
	if err != nil {
		if cg.module == "" && cg.modMtime == 0 {
			return false
		}
		cg.module, cg.modMtime, cg.modSize = "", 0, 0
		return true
	}
	if st.ModTime().UnixNano() == cg.modMtime && st.Size() == cg.modSize {
		return false
	}
	cg.modMtime, cg.modSize = st.ModTime().UnixNano(), st.Size()
	data, err := safeRepoRead(repo, "go.mod")
	if err != nil {
		// 安全读取失败（含 stat/open/read 间被替换）时不能继续沿用旧 module，
		// 也不能缓存本次指纹，否则同一文件恢复可读后将永不重试。
		cg.module, cg.modMtime, cg.modSize = "", 0, 0
		return true
	}
	mod := ""
	for line := range bytes.Lines(data) {
		if rest, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("module")); ok {
			mod = strings.TrimSpace(strings.Trim(strings.TrimSpace(string(rest)), `"`))
			break
		}
	}
	if mod != cg.module {
		cg.module = mod
	}
	return true
}

// resolve 由提取结果连边(纯内存)。
func (cg *callGraph) resolve() {
	// 包符号表:(目录, package 声明名) → 符号名 → 声明它的文件集。
	// 同目录的 package p / package p_test 必须物理隔离；同一 package 内 build
	// tag 双版本仍会形成多声明，继续走既有“调用方文件优先，否则丢边”规则。
	pkgDecls := map[packageKey]map[string][]string{}
	// 方法基名启发表:(目录, package) → 基名 → 规范名集合。
	pkgMethodBase := map[packageKey]map[string][]string{}
	// import 只可落到目标目录唯一的可导入 package。external-test package 只有
	// *_test.go 文件，不可被普通 import；多个非测试 package 则按歧义丢边。
	hasNonTestFile := map[packageKey]bool{}
	for rel, entry := range cg.files {
		if entry.fc == nil {
			continue
		}
		key := packageKey{dir: path.Dir(rel), pkg: entry.fc.Package}
		if !strings.HasSuffix(rel, "_test.go") {
			hasNonTestFile[key] = true
		}
		decls := pkgDecls[key]
		if decls == nil {
			decls = map[string][]string{}
			pkgDecls[key] = decls
		}
		mb := pkgMethodBase[key]
		if mb == nil {
			mb = map[string][]string{}
			pkgMethodBase[key] = mb
		}
		for _, name := range entry.fc.Decls {
			decls[name] = append(decls[name], rel)
			if t, m, ok := strings.Cut(name, "."); ok && t != "" && m != "" {
				if !slices.Contains(mb[m], name) {
					mb[m] = append(mb[m], name)
				}
			}
		}
	}

	// lookup 归位一个 package 内的符号名:调用方文件优先,否则包内唯一。
	lookup := func(key packageKey, name, callerRel string) string {
		sites := pkgDecls[key][name]
		switch {
		case len(sites) == 0:
			return ""
		case len(sites) == 1:
			return sites[0] + "#" + name
		}
		for _, s := range sites {
			if s == callerRel {
				return s + "#" + name
			}
		}
		return "" // 跨文件歧义(build tag 双版本等):宁缺毋错
	}
	importKey := func(dir string) (packageKey, bool) {
		var found packageKey
		count := 0
		for key := range pkgDecls {
			if key.dir == dir && hasNonTestFile[key] {
				found = key
				count++
			}
		}
		return found, count == 1
	}

	// 接口→实现(2026-07-04,codegraph 启发的方法集匹配;AST 近似,无类型检查):
	// 接口注册 → 仓内内嵌展开 → 全仓匹配。宁缺毋错三闸:含不可归位内嵌(仓外/约束
	// 元素)整个接口弃;≥2 方法才严格匹配,单方法接口仅唯一实现者才认;同包同名
	// 接口多文件声明(build tag)弃。已知低估留痕:结构体内嵌带来的方法提升 AST
	// 看不见,漏配不误配。
	ifaceMethodTargets := cg.resolveInterfaces(pkgDecls, lookup, importKey)

	edges := map[string][]string{}
	reverse := map[string][]string{}
	addEdge := func(from, to string) {
		if to == "" || to == from || slices.Contains(edges[from], to) {
			return
		}
		edges[from] = append(edges[from], to)
		reverse[to] = append(reverse[to], from)
	}

	for rel, entry := range cg.files {
		if entry.fc == nil {
			continue
		}
		key := packageKey{dir: path.Dir(rel), pkg: entry.fc.Package}
		for caller, refs := range entry.fc.Calls {
			from := rel + "#" + caller
			for _, ref := range refs {
				switch ref.Qual {
				case "":
					addEdge(from, lookup(key, ref.Name, rel))
				default:
					if imp, ok := entry.fc.Imports[ref.Qual]; ok {
						if tdir := cg.moduleDir(imp); tdir != "" {
							if targetKey, ok := importKey(tdir); ok {
								addEdge(from, lookup(targetKey, ref.Name, rel))
							}
						}
						continue // import 命中但库外/未归位:丢弃,不落启发
					}
					// 非 import 限定名:局部变量方法调用。① 同包唯一基名启发;
					// ② 接口分发兜底(codegraph 启发):该方法名属于仓内接口的
					// 方法集 → 连到全部实现(扇出 ≤5,过则视为过度歧义丢弃)。
					if cands := pkgMethodBase[key][ref.Name]; len(cands) == 1 {
						addEdge(from, lookup(key, cands[0], rel))
					} else if ts := ifaceMethodTargets[ref.Name]; len(ts) >= 1 && len(ts) <= 5 {
						for _, t := range ts {
							addEdge(from, t)
						}
					}
				}
			}
		}
	}
	for _, m := range []map[string][]string{edges, reverse} {
		for k := range m {
			sort.Strings(m[k])
		}
	}
	cg.edges, cg.reverse = edges, reverse
}

// resolveInterfaces 建接口→实现关系,返回"接口方法名 → 实现方法节点"分发表。
// 输入沿用 resolve 的包符号表与归位闭包;结果写 cg.implsOf/ifacesOf。
func (cg *callGraph) resolveInterfaces(
	pkgDecls map[packageKey]map[string][]string,
	lookup func(key packageKey, name, callerRel string) string,
	importKey func(dir string) (packageKey, bool),
) map[string][]string {
	type iface struct {
		key          packageKey
		name, nodeID string
		methods      map[string]bool
		embeds       []parser.CallRef
		imports      map[string]string
		dead         bool
	}

	// ① 注册:同 package 同名多文件声明(build tag)→ 弃；同目录 p/p_test
	// 是两个 package，不得互相杀死。
	byKey := map[string]*iface{} // dir + "\x00" + package + "\x00" + name
	for rel, entry := range cg.files {
		if entry.fc == nil {
			continue
		}
		pkgKey := packageKey{dir: path.Dir(rel), pkg: entry.fc.Package}
		for _, d := range entry.fc.Interfaces {
			key := pkgKey.dir + "\x00" + pkgKey.pkg + "\x00" + d.Name
			if prev, ok := byKey[key]; ok {
				prev.dead = true
				continue
			}
			it := &iface{key: pkgKey, name: d.Name, nodeID: rel + "#" + d.Name,
				methods: map[string]bool{}, embeds: d.Embeds, imports: entry.fc.Imports}
			for _, m := range d.Methods {
				it.methods[m] = true
			}
			byKey[key] = it
		}
	}

	// ② 内嵌展开(不动点,深度上限 5):仓内内嵌并入方法集;不可归位(仓外/
	// 约束元素/找不到)→ 整个接口弃。
	for range 5 {
		progressed := false
		for _, it := range byKey {
			if it.dead || len(it.embeds) == 0 {
				continue
			}
			var rest []parser.CallRef
			for _, em := range it.embeds {
				targetKey := packageKey{}
				targetOK := false
				switch {
				case em.Name == "!unresolvable":
					it.dead = true
				case em.Qual == "":
					targetKey, targetOK = it.key, true
				default:
					if imp, ok := it.imports[em.Qual]; ok {
						if tdir := cg.moduleDir(imp); tdir != "" {
							targetKey, targetOK = importKey(tdir)
						}
					}
				}
				if it.dead {
					break
				}
				target := byKey[targetKey.dir+"\x00"+targetKey.pkg+"\x00"+em.Name]
				switch {
				case !targetOK || target == nil || target.dead:
					it.dead = true
				case len(target.embeds) > 0:
					rest = append(rest, em) // 目标自身未展开完,下一轮再并
				default:
					for m := range target.methods {
						it.methods[m] = true
					}
					progressed = true
				}
			}
			if !it.dead {
				if len(rest) < len(it.embeds) {
					progressed = true
				}
				it.embeds = rest
			}
		}
		if !progressed {
			break
		}
	}

	// ③ 类型方法集(dir → 类型名 → 方法基名集;来源是 "T.M" 形态的声明)。
	typeMethods := map[packageKey]map[string]map[string]bool{}
	for key, decls := range pkgDecls {
		for name := range decls {
			t, m, ok := strings.Cut(name, ".")
			if !ok || t == "" || m == "" {
				continue
			}
			tm := typeMethods[key]
			if tm == nil {
				tm = map[string]map[string]bool{}
				typeMethods[key] = tm
			}
			if tm[t] == nil {
				tm[t] = map[string]bool{}
			}
			tm[t][m] = true
		}
	}

	// ④ 全仓匹配 + 分发表。
	implsOf := map[string][]string{}
	ifacesOf := map[string][]string{}
	targets := map[string]map[string]bool{} // 方法名 → 实现方法节点集
	for _, it := range byKey {
		if it.dead || len(it.embeds) > 0 || len(it.methods) == 0 {
			continue
		}
		var impls []string // 实现类型节点
		type implTarget struct {
			key packageKey
			typ string
		}
		var implTargets []implTarget
		for key, tm := range typeMethods {
			for t, ms := range tm {
				if key == it.key && t == it.name {
					continue // 接口自身
				}
				ok := true
				for m := range it.methods {
					if !ms[m] {
						ok = false
						break
					}
				}
				if !ok {
					continue
				}
				if tn := lookup(key, t, ""); tn != "" {
					impls = append(impls, tn)
					implTargets = append(implTargets, implTarget{key: key, typ: t})
				}
			}
		}
		// 宁缺闸:≥2 方法严格匹配全收;单方法接口仅唯一实现者才认。
		if len(impls) == 0 || (len(it.methods) == 1 && len(impls) != 1) {
			continue
		}
		implsOf[it.nodeID] = append(implsOf[it.nodeID], impls...)
		for i, tn := range impls {
			ifacesOf[tn] = append(ifacesOf[tn], it.nodeID)
			for m := range it.methods {
				if mn := lookup(implTargets[i].key, implTargets[i].typ+"."+m, ""); mn != "" {
					if targets[m] == nil {
						targets[m] = map[string]bool{}
					}
					targets[m][mn] = true
				}
			}
		}
	}
	for _, m := range []map[string][]string{implsOf, ifacesOf} {
		for k := range m {
			sort.Strings(m[k])
			m[k] = slices.Compact(m[k])
		}
	}
	out := map[string][]string{}
	for m, set := range targets {
		for t := range set {
			out[m] = append(out[m], t)
		}
		sort.Strings(out[m])
	}
	cg.implsOf, cg.ifacesOf = implsOf, ifacesOf
	return out
}

// implementationsOf / interfacesOf 返回接口↔实现关系(node ID 升序;无则 nil)。
func (cg *callGraph) implementationsOf(nodeID string) []string { return cg.implsOf[nodeID] }
func (cg *callGraph) interfacesOf(nodeID string) []string      { return cg.ifacesOf[nodeID] }

// moduleDir 把 import 路径映射到仓库内目录;库外返回 ""。
func (cg *callGraph) moduleDir(imp string) string {
	if cg.module == "" {
		return ""
	}
	if imp == cg.module {
		return "."
	}
	if rest, ok := strings.CutPrefix(imp, cg.module+"/"); ok {
		return rest
	}
	return ""
}

// callsOf / calledByOf 返回节点的出边/入边(node ID,升序;无图或无边返回 nil)。
func (cg *callGraph) callsOf(nodeID string) []string    { return cg.edges[nodeID] }
func (cg *callGraph) calledByOf(nodeID string) []string { return cg.reverse[nodeID] }

// fileCentrality 统计每文件的跨文件被调入边数(热区排序的中心度因子,
// knowledge.md §12.1)。同文件内部调用不计——helper 密集的文件会虚高。
func (cg *callGraph) fileCentrality() map[string]int {
	out := map[string]int{}
	for to, froms := range cg.reverse {
		tf, _ := model.SplitNodeID(to)
		for _, from := range froms {
			if ff, _ := model.SplitNodeID(from); ff != tf {
				out[tf]++
			}
		}
	}
	return out
}

// neighbor 是结构扩展的一个候选(检索三级递进第 2 级,knowledge.md §10.2)。
type neighbor struct {
	id    string
	via   string // 首个发现途径(展示用)
	score int    // 被多个命中引用 +1/次;有活知识 +2(带知识的邻居才最值得带出)
}

// structuralNeighborsLocked 对命中集做一跳扩展:调用边(双向)+ 同流程步骤。
// 只返回索引里存在、且不在命中集里的节点;按 score 降序、ID 升序,上限 limit。
// 前提:已持 rt.mu。
func (e *Engine) structuralNeighborsLocked(hitIDs []string, limit int) []neighbor {
	inHits := map[string]bool{}
	for _, id := range hitIDs {
		inHits[id] = true
	}
	cand := map[string]*neighbor{}
	add := func(id, via string) {
		if id == "" || inHits[id] {
			return
		}
		ref := e.rt.ix.Node(id)
		if ref == nil {
			return
		}
		nb := cand[id]
		if nb == nil {
			nb = &neighbor{id: id, via: via}
			if hasActiveEntries(ref.Node) {
				nb.score += 2
			}
			cand[id] = nb
		}
		nb.score++
	}

	cg := e.ensureCallGraphLocked()
	for _, hid := range hitIDs {
		if cg != nil {
			for _, to := range cg.callsOf(hid) {
				add(to, "被 "+hid+" 调用")
			}
			for _, from := range cg.calledByOf(hid) {
				add(from, "调用 "+hid)
			}
			for _, t := range cg.implementationsOf(hid) {
				add(t, "实现 "+hid)
			}
			for _, i := range cg.interfacesOf(hid) {
				add(i, hid+" 所实现的接口")
			}
		}
		for _, fid := range e.rt.ix.FlowsOf(hid) {
			f := e.rt.ix.Flow(fid)
			if f == nil || f.Deprecated {
				continue
			}
			for _, st := range f.Steps {
				add(e.rt.ix.ResolveNodeID(st.Node), "同流程 "+fid)
			}
		}
	}

	out := make([]neighbor, 0, len(cand))
	for _, nb := range cand {
		out = append(out, *nb)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].id < out[j].id
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// displayEdges 把边列表转为展示形态:同文件目标显示裸符号名,跨文件显示完整
// node ID(可直接作 kb_recall 入参);超过 limit 截断并以计数收尾(限额哲学 §12.2)。
func displayEdges(edgeIDs []string, selfFile string, limit int) []string {
	if len(edgeIDs) == 0 {
		return nil
	}
	var same, cross []string
	for _, id := range edgeIDs {
		f, s := model.SplitNodeID(id)
		if f == selfFile && s != "" {
			same = append(same, s)
		} else {
			cross = append(cross, id)
		}
	}
	out := append(same, cross...)
	if len(out) > limit {
		total := len(out)
		out = out[:limit]
		out = append(out, fmt.Sprintf("……(共 %d 处)", total))
	}
	return out
}
