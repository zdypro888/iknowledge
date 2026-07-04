package engine

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
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
}

type fileCallsEntry struct {
	mtimeNS int64
	size    int64
	fc      *parser.FileCalls
}

// ensureCallGraphLocked 惰性构建/增量刷新调用图。前提:已持 rt.mu。
// 图是尽力而为的派生值:清单/解析失败返回 nil,调用方按"无图"降级,不阻断读路径。
func (e *Engine) ensureCallGraphLocked() *callGraph {
	repo := e.Store.RepoRoot()
	cfg, _ := e.Store.LoadConfig()
	rels, err := listSourceFiles(repo, e.Reg, cfg)
	if err != nil {
		return nil
	}

	cg := e.rt.cg
	if cg == nil {
		cg = &callGraph{files: map[string]*fileCallsEntry{}}
		e.rt.cg = cg
	}
	changed := cg.refreshModule(repo)

	alive := make(map[string]bool, len(rels))
	for _, rel := range rels {
		alive[rel] = true
		st, err := os.Stat(filepath.Join(repo, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		if prev := cg.files[rel]; prev != nil && prev.mtimeNS == st.ModTime().UnixNano() && prev.size == st.Size() {
			continue
		}
		entry := &fileCallsEntry{mtimeNS: st.ModTime().UnixNano(), size: st.Size()}
		if ex, ok := e.Reg.ForFile(rel).(parser.CallExtractor); ok {
			src, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(rel)))
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
	return cg
}

// refreshModule 按 go.mod 指纹重读模块路径;变更返回 true。
func (cg *callGraph) refreshModule(repo string) bool {
	st, err := os.Stat(filepath.Join(repo, "go.mod"))
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
	data, err := os.ReadFile(filepath.Join(repo, "go.mod"))
	if err != nil {
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
	// 包符号表:目录 → 符号名 → 声明它的文件集(build tag 双版本会多于一个)。
	type declSites map[string][]string // 名 → []fileRel
	pkgDecls := map[string]declSites{}
	// 方法基名启发表:目录 → 基名 → 规范名集合(去重后唯一才可用)。
	pkgMethodBase := map[string]map[string][]string{}
	for rel, entry := range cg.files {
		if entry.fc == nil {
			continue
		}
		dir := path.Dir(rel)
		decls := pkgDecls[dir]
		if decls == nil {
			decls = declSites{}
			pkgDecls[dir] = decls
		}
		mb := pkgMethodBase[dir]
		if mb == nil {
			mb = map[string][]string{}
			pkgMethodBase[dir] = mb
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

	// lookup 归位一个目录内的符号名:调用方文件优先,否则包内唯一。
	lookup := func(dir, name, callerRel string) string {
		sites := pkgDecls[dir][name]
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
		dir := path.Dir(rel)
		for caller, refs := range entry.fc.Calls {
			from := rel + "#" + caller
			for _, ref := range refs {
				switch ref.Qual {
				case "":
					addEdge(from, lookup(dir, ref.Name, rel))
				default:
					if imp, ok := entry.fc.Imports[ref.Qual]; ok {
						if tdir := cg.moduleDir(imp); tdir != "" {
							addEdge(from, lookup(tdir, ref.Name, rel))
						}
						continue // import 命中但库外/未归位:丢弃,不落启发
					}
					// 非 import 限定名:局部变量方法调用,同包唯一基名启发。
					if cands := pkgMethodBase[dir][ref.Name]; len(cands) == 1 {
						addEdge(from, lookup(dir, cands[0], rel))
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
