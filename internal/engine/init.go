// Package engine 承载业务规则(impl §2):锚定校验、suspect 降级、迁移、对账。
// M1.1 实现骨架秒建与幂等对账(impl §6);读路径与写路径随 M1.2/M1.3 扩展。
package engine

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/zdypro888/iknowledge/internal/model"
	"github.com/zdypro888/iknowledge/internal/parser"
	"github.com/zdypro888/iknowledge/internal/store"
)

// Engine 组合存储与解析器,执行业务规则。
type Engine struct {
	Store *store.Store
	Reg   *parser.Registry
	// now 可注入以便测试;缺省 time.Now。
	now func() time.Time
	// scoutAddr 是 serve 实际监听地址(自派侦察兵回连用,SetScoutAddr 注入;
	// 空则回退 config 端口)。启动期一次性写入,不加锁。
	scoutAddr string
	// rt 是 serve 期的内存态(缓存/索引/台账/作业),见 runtime.go。
	rt runtime
	// afterImportTruthWrite 仅供崩溃恢复测试在某次真相 rename 后模拟进程猝死。
	// 生产构造永为 nil；普通 error 不走此钩子，仍在当次调用内主动 rollback。
	afterImportTruthWrite func(string) error
}

// New 建引擎。多语言注册(2026-07-04):专职解析器(Go/Python)先注册,
// config.yaml 的 extensions 白名单再挂通用文件级插件(已占用的扩展名忽略,
// 专职插件永远优先);config 读取尽力而为——没有 config 时纯 Go 行为不变。
func New(s *store.Store) *Engine {
	cfg, _ := s.LoadConfig()
	return &Engine{Store: s, Reg: registryForConfig(cfg), now: time.Now}
}

func registryForConfig(cfg *store.Config) *parser.Registry {
	reg := parser.NewRegistry()
	if cfg != nil && len(cfg.Extensions) > 0 {
		var free []string
		for _, ext := range cfg.Extensions {
			ext = strings.TrimSpace(ext)
			if ext == "" {
				continue
			}
			if !strings.HasPrefix(ext, ".") {
				ext = "." + ext
			}
			if reg.ForFile("x"+ext) == nil {
				free = append(free, ext)
			}
		}
		if len(free) > 0 {
			reg.Register(parser.NewGeneric(free))
		}
	}
	return reg
}

// InitOptions 对应 kb_init / CLI init 的入参(impl §7.3)。
type InitOptions struct {
	Force bool // 对丢失/受损分片强制重写(仍不动已有 Entries)
	// ReanchorAll 是 mass-suspect 的唯一批量出口(impl §6 第 7 步):
	// 人工确认全局性变更为预期后,全库按当前代码重锚,suspect 一律升回 fresh。
	ReanchorAll bool
}

// InitReport 是 init/对账的返回报告(impl §6)。
type InitReport struct {
	Created     int      // 本次新建节点数
	Migrated    int      // StructHash 精确迁移数
	Suspected   int      // 本次降级 suspect 数
	Orphaned    int      // 本次标孤儿数
	ParseFailed int      // 语法错误跳过的文件数
	Files       int      // 扫描的源文件数
	MassSuspect bool     // suspect 激增(>50% 节点)告警
	Warnings    []string // 大小写碰撞、冲突分片等
}

// Text 渲染文本报告(kb_init 返回体)。
func (r *InitReport) Text() string {
	var b strings.Builder
	if r.MassSuspect {
		b.WriteString("⚠ suspect 激增(>50% 节点):疑似全局性变更(批量格式化/哈希规则升级),")
		b.WriteString("请人工确认后运行 `init --reanchor-all`,勿逐条偿还。\n")
	}
	fmt.Fprintf(&b, "created=%d migrated=%d suspected=%d orphaned=%d parseFailed=%d files=%d",
		r.Created, r.Migrated, r.Suspected, r.Orphaned, r.ParseFailed, r.Files)
	for _, w := range r.Warnings {
		b.WriteString("\n⚠ ")
		b.WriteString(w)
	}
	return b.String()
}

// 对账的内部工作集。
type reconcile struct {
	e      *Engine
	report *InitReport
	now    time.Time

	// 旧世界:从 tree 文件分片加载(不含 _dir/project)。
	oldShards map[string]*loadedShard // key: 源文件相对路径
	oldByID   map[string]*model.Node
	oldStruct map[string]int // StructHash → 旧库计数(唯一性判据)

	// 新世界:本次扫描。
	files     []string
	symsByRel map[string][]parser.Symbol
	// fileHashByRel 解析期缓存的文件级锚定哈希(HashFileFor:插件自定义优先,
	// 缺省符号级联;src 不驻留,故在解析现场算)。
	fileHashByRel map[string]string
	newByID       map[string]symRef
	newStruct     map[string][]string // StructHash → 未被 ID 命中的新符号 ID 列表

	// 结果:每个源文件的最终节点集(nil 值表示该分片应删除)。
	out      map[string][]model.Node
	skipRel  map[string]bool // conflict/too-new 分片对应的文件:整体跳过
	migrated map[string]bool // 已被迁移占用的新符号 ID
}

// loadedShard 只保留解码结果;回写时 SaveShard(raw=nil) 会重读磁盘做未知字段合并。
type loadedShard struct {
	shard *store.Shard
}

type symRef struct {
	rel string
	sym parser.Symbol
}

// Init 骨架秒建 + 幂等对账(impl §6 全步骤)。
// 持 rt.mu 全程(#17/#32:serve 期 kb_init 与并发 recall/remember 共享 Store 与缓存,
// 不加锁则对账写分片与读路径读分片竞态)。Init 直接改 .knowledge 而不经 rt 缓存;
// 无需显式作废缓存——所有写盘都改 mtime/size,下次 Sync 的目录对账自然重载(R2 复核)。
func (e *Engine) Init(opts InitOptions) (*InitReport, error) {
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	return e.initLocked(opts)
}

func (e *Engine) initLocked(opts InitOptions) (*InitReport, error) {
	s := e.Store
	if !e.rt.truthTxActive {
		recovered, err := s.RecoverTruthTransactionWithStatus()
		if err != nil {
			return nil, fmt.Errorf("init 前恢复未完成事务: %w", err)
		}
		if recovered {
			e.rt.cache = nil
		}
	}
	if err := s.EnsureLayout(); err != nil {
		return nil, err
	}
	cfg, err := s.EnsureConfig()
	if err != nil {
		return nil, err
	}
	e.Reg = registryForConfig(cfg)
	if err := s.EnsureGitFiles(); err != nil { // impl §6 第 6 步
		return nil, err
	}

	rep := &InitReport{}
	rc := &reconcile{
		e: e, report: rep, now: e.now().UTC(),
		oldShards:     map[string]*loadedShard{},
		oldByID:       map[string]*model.Node{},
		oldStruct:     map[string]int{},
		symsByRel:     map[string][]parser.Symbol{},
		fileHashByRel: map[string]string{},
		newByID:       map[string]symRef{},
		newStruct:     map[string][]string{},
		out:           map[string][]model.Node{},
		skipRel:       map[string]bool{},
		migrated:      map[string]bool{},
	}

	if err := rc.scanSources(cfg); err != nil {
		return nil, err
	}
	if err := rc.loadOldShards(); err != nil {
		return nil, err
	}
	rc.matchAndMigrate(opts)
	if err := rc.writeShards(opts); err != nil {
		return nil, err
	}
	if err := rc.ensureDirAndProjectNodes(); err != nil {
		return nil, err
	}

	// impl §6 第 7 步:suspect 激增检测。
	total := 0
	for _, nodes := range rc.out {
		total += len(nodes)
	}
	if total > 0 && rep.Suspected*2 > total {
		rep.MassSuspect = true
	}
	return rep, nil
}

// scanSources 枚举 + 解析全部源文件,检测大小写碰撞(impl §3)。
func (rc *reconcile) scanSources(cfg *store.Config) error {
	files, err := listSourceFiles(rc.e.Store.RepoRoot(), rc.e.Reg, cfg)
	if err != nil {
		return err
	}
	lowerSeen := map[string]string{}
	for _, rel := range files {
		// 大小写碰撞:大小写不敏感文件系统上两个分片会互相覆盖 → 告警并跳过后者。
		if prev, ok := lowerSeen[strings.ToLower(rel)]; ok {
			rc.report.Warnings = append(rc.report.Warnings,
				fmt.Sprintf("路径大小写碰撞:%s 与 %s,跳过后者", prev, rel))
			continue
		}
		lowerSeen[strings.ToLower(rel)] = rel

		src, err := safeRepoRead(rc.e.Store.RepoRoot(), rel)
		if err != nil {
			return fmt.Errorf("engine: 读 %s: %w", rel, err)
		}
		if parser.IsGenerated(src) {
			continue
		}
		rc.report.Files++
		p := rc.e.Reg.ForFile(rel)
		syms, err := p.Parse(rel, src)
		if err != nil {
			// 解析失败三态之 init(impl §5 定案):跳过并计入报告,不动其分片。
			rc.report.ParseFailed++
			rc.skipRel[rel] = true
			continue
		}
		rc.files = append(rc.files, rel)
		rc.symsByRel[rel] = syms
		rc.fileHashByRel[rel] = parser.HashFileFor(p, syms, src)
		for _, sym := range syms {
			rc.newByID[model.SymbolNodeID(rel, sym.Name)] = symRef{rel: rel, sym: sym}
		}
	}
	return nil
}

// 符号级大小写"碰撞"不检测(M1.1 验收修正):碰撞检测的理由是大小写不敏感
// 文件系统上分片【文件】互相覆盖,只对路径成立;符号是分片内容而非文件名,
// DefaultPrompts 与 defaultPrompts 是合法的不同 Go 符号(aibridge 验收时撞出)。

// loadOldShards 载入现存 tree 文件分片;conflict/too-new 的分片整体跳过其文件。
func (rc *reconcile) loadOldShards() error {
	s := rc.e.Store
	return s.WalkTreeShards(func(path string) error {
		rel := s.SrcRelOfShard(path)
		if rel == "" {
			return nil // _dir.yaml 另行处理
		}
		sh, _, err := s.LoadShard(path)
		if err != nil {
			rc.skipRel[rel] = true
			rc.report.Warnings = append(rc.report.Warnings,
				fmt.Sprintf("分片不可用,已跳过:%s(%v)", rel, err))
			return nil
		}
		rc.oldShards[rel] = &loadedShard{shard: sh}
		for i := range sh.Nodes {
			n := &sh.Nodes[i]
			rc.oldByID[n.ID] = n
			if n.Anchor.StructHash != "" {
				rc.oldStruct[n.Anchor.StructHash]++
			}
		}
		return nil
	})
}

// matchAndMigrate 是对账核心:ID 匹配 → StructHash 双向唯一迁移 → 孤儿/清除 → 新建。
func (rc *reconcile) matchAndMigrate(opts InitOptions) {
	// 迁移候选(新侧):未被旧 ID 命中的新符号,按 StructHash 建索引。
	for id, ref := range rc.newByID {
		if rc.skipRel[ref.rel] {
			continue
		}
		if _, exists := rc.oldByID[id]; !exists {
			rc.newStruct[ref.sym.StructHash] = append(rc.newStruct[ref.sym.StructHash], id)
		}
	}

	// 先为每个现存文件铺出最终节点集(file 节点 + 符号节点,继承旧状态)。
	for _, rel := range rc.files {
		if rc.skipRel[rel] {
			continue
		}
		rc.out[rel] = rc.buildFileNodes(rel, opts)
	}

	// 处理旧世界中消失的节点:迁移 / 孤儿 / 清除。
	oldIDs := make([]string, 0, len(rc.oldByID))
	for id := range rc.oldByID {
		oldIDs = append(oldIDs, id)
	}
	sort.Strings(oldIDs) // 确定性
	for _, id := range oldIDs {
		old := rc.oldByID[id]
		file, symbol := model.SplitNodeID(id)
		if rc.skipRel[file] {
			continue // 分片不可用或源文件解析失败:整体不动
		}
		if _, stillThere := rc.newByID[id]; stillThere && rc.fileIndexed(file) {
			continue // ID 仍在,已由 buildFileNodes 处理
		}
		if symbol == "" && rc.fileIndexed(file) {
			continue // 文件节点且文件还在:已处理
		}

		// 符号消失:尝试 StructHash 精确迁移(impl §6 第 5 步,双向唯一 + 目标无 Entries)。
		if symbol != "" && rc.tryMigrate(old) {
			continue
		}
		// 迁移不成:有知识 → 孤儿保留(宁可人工认领,不可错挂);
		// 无知识(undigested)→ 清除(无知识可保,留着只堆积空孤儿;M1.1 实现定案,impl §6)。
		if len(old.Entries) > 0 {
			orphan := *old
			orphan.Status = model.StatusOrphaned
			rc.out[file] = append(rc.out[file], orphan)
			rc.report.Orphaned++
		}
	}

	// out 里每个文件的节点排序:file 节点在首,符号按源码顺序(新建/继承时已按序),
	// 孤儿追加在尾部——保持分片 diff 稳定。
}

func (rc *reconcile) fileIndexed(rel string) bool {
	_, ok := rc.symsByRel[rel]
	return ok
}

// buildFileNodes 为一个源文件产出最终节点集:file 节点 + 全部符号节点。
func (rc *reconcile) buildFileNodes(rel string, opts InitOptions) []model.Node {
	syms := rc.symsByRel[rel]
	nodes := make([]model.Node, 0, len(syms)+1)

	fileAnchor := model.Anchor{File: rel, Hash: rc.fileHashByRel[rel]}
	nodes = append(nodes, rc.reconcileNode(model.FileNodeID(rel), model.LevelFile, fileAnchor, opts))

	for _, sym := range syms {
		level := model.LevelFunction
		if sym.Kind == "type" || sym.Kind == "var" || sym.Kind == "const" {
			level = model.LevelDecl
		}
		anchor := model.Anchor{
			File: rel, Symbol: sym.Name,
			Hash: sym.Hash, StructHash: sym.StructHash, DocStructHash: sym.DocStructHash, Lines: sym.Lines,
		}
		nodes = append(nodes, rc.reconcileNode(model.SymbolNodeID(rel, sym.Name), level, anchor, opts))
	}
	return nodes
}

// reconcileNode 单节点对账(impl §6 第 4 步 + 第 7 步 reanchor):
//   - 无旧节点 → 新建 undigested;
//   - 旧节点哈希一致 → 锚随行号/结构哈希刷新,状态不动(orphaned 回归按知识有无定 fresh/undigested);
//   - 失配:有 Entries → 降 suspect 且【保留旧锚】(锚记录知识锚定时的代码,
//     "重验即重锚"才允许更新——否则代码回退无从检测);undigested 无知识可腐 → 仅重锚;
//   - ReanchorAll → 全部按当前代码重锚,suspect 升回 fresh(Entries 不动)。
func (rc *reconcile) reconcileNode(id, level string, anchor model.Anchor, opts InitOptions) model.Node {
	old, exists := rc.oldByID[id]
	if !exists {
		rc.report.Created++
		return model.Node{ID: id, Level: level, Anchor: anchor, Status: model.StatusUndigested, Since: rc.now}
	}

	n := *old
	n.Level = level
	switch {
	case opts.ReanchorAll:
		n.Anchor = anchor
		if n.Status == model.StatusSuspect || n.Status == model.StatusOrphaned {
			n.Status = statusForKnowledge(&n)
		}
	case old.Anchor.Hash == anchor.Hash:
		n.Anchor = anchor // 哈希一致:行号/StructHash 顺手刷新
		if n.Status == model.StatusOrphaned || n.Status == model.StatusSuspect {
			// 代码与锚重新一致(切回分支 / A→B→A 回退):知识锚定的代码原样在场,恢复。
			n.Status = statusForKnowledge(&n)
		}
	case old.Status == model.StatusUndigested:
		n.Anchor = anchor // 无知识可腐:仅重锚,保持 undigested
	default:
		if n.Status != model.StatusSuspect {
			rc.report.Suspected++
		}
		n.Status = model.StatusSuspect
		n.Anchor.Lines = anchor.Lines // 展示性字段可刷新;哈希保留旧值
	}
	return n
}

func statusForKnowledge(n *model.Node) model.Status {
	if len(n.Entries) > 0 {
		return model.StatusFresh
	}
	return model.StatusUndigested
}

// tryMigrate 尝试 StructHash 精确迁移(impl §6 第 5 步):
// 旧 StructHash 在旧库唯一 && 新扫描恰一个未占用符号命中 && 目标无既有 Entries。
// 多对一/一对多/目标已占用(孪生函数体)→ 不迁,交给孤儿逻辑。
func (rc *reconcile) tryMigrate(old *model.Node) bool {
	sh := old.Anchor.StructHash
	if sh == "" || rc.oldStruct[sh] != 1 {
		return false
	}
	candidates := rc.newStruct[sh]
	if len(candidates) != 1 {
		return false
	}
	targetID := candidates[0]
	if rc.migrated[targetID] {
		return false
	}
	ref := rc.newByID[targetID]

	// 在 out 里找到目标节点(buildFileNodes 已建为 undigested)替换为迁移结果。
	nodes := rc.out[ref.rel]
	for i := range nodes {
		if nodes[i].ID != targetID {
			continue
		}
		if len(nodes[i].Entries) > 0 { // 目标已占用:不迁
			return false
		}
		migrated := *old
		migrated.ID = targetID
		migrated.Level = nodes[i].Level
		newAnchor := nodes[i].Anchor
		docGuardOK := old.Anchor.DocStructHash != "" && newAnchor.DocStructHash != "" &&
			old.Anchor.DocStructHash == newAnchor.DocStructHash
		if docGuardOK || len(old.Entries) == 0 {
			migrated.Anchor = newAnchor // 纯改名/搬家已被 doc 护栏证明，可直接重锚
		} else {
			// StructHash 只证明“代码骨架像同一实体”，不能证明迁移时没顺手改
			// 契约 doc。保留旧 Hash 作为知识基线，同时把位置/迁移哈希更新到新实体；
			// 下一次 init 仍会失配，直到显式 verify/reanchor，不能自动洗回 fresh。
			migrated.Anchor.File = newAnchor.File
			migrated.Anchor.Symbol = newAnchor.Symbol
			migrated.Anchor.StructHash = newAnchor.StructHash
			migrated.Anchor.DocStructHash = newAnchor.DocStructHash
			migrated.Anchor.Lines = newAnchor.Lines
			migrated.Status = model.StatusSuspect
			rc.report.Suspected++
			rc.report.Warnings = append(rc.report.Warnings,
				"迁移已保留但需重验:"+old.ID+" → "+targetID+"(doc 护栏缺失或失配)")
		}
		// 血缘:全链 flat 集合,旧 ID 追加、去重(knowledge.md §12.6)。
		migrated.Lineage = appendUnique(append([]string{}, old.Lineage...), old.ID)
		if docGuardOK && (migrated.Status == model.StatusOrphaned || migrated.Status == model.StatusSuspect) {
			// 结构等同的代码在新位置找到了:孤儿回归;suspect 语义按旧锚已不可判,
			// DocStructHash 又证明契约说明不变,故按知识有无回归。
			migrated.Status = statusForKnowledge(&migrated)
		}
		rc.out[ref.rel][i] = migrated
		rc.migrated[targetID] = true
		rc.report.Created-- // buildFileNodes 曾计作新建,冲正
		rc.report.Migrated++
		return true
	}
	return false
}

func appendUnique(list []string, v string) []string {
	if slices.Contains(list, v) {
		return list
	}
	return append(list, v)
}

// writeShards 落盘:现存文件的分片重写;消失文件的分片按"仅剩孤儿"重写或删除。
func (rc *reconcile) writeShards(opts InitOptions) error {
	s := rc.e.Store

	// 现存文件。
	for _, rel := range rc.files {
		if rc.skipRel[rel] {
			continue
		}
		nodes := rc.out[rel]
		if !opts.Force && !rc.shardChanged(rel, nodes) {
			continue // 幂等:内容无变不重写,保持 mtime 稳定
		}
		sh := &store.Shard{Schema: model.SchemaVersion, Nodes: nodes}
		if err := s.SaveShard(s.ShardPathFor(rel), sh, nil); err != nil {
			return err
		}
	}

	// 旧分片对应的源文件已消失(或被排除):仅剩孤儿则保留孤儿,否则删分片。
	for rel := range rc.oldShards {
		if rc.fileIndexed(rel) || rc.skipRel[rel] {
			continue
		}
		orphans := rc.out[rel] // matchAndMigrate 只会往消失文件追加孤儿
		path := s.ShardPathFor(rel)
		if len(orphans) == 0 {
			shardRel, relErr := filepath.Rel(s.Dir(), path)
			if relErr != nil {
				return relErr
			}
			if err := s.RemoveKnowledgeFile(filepath.ToSlash(shardRel)); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("engine: 删空分片 %s: %w", rel, err)
			}
			continue
		}
		sh := &store.Shard{Schema: model.SchemaVersion, Nodes: orphans}
		if err := s.SaveShard(path, sh, nil); err != nil {
			return err
		}
	}
	return nil
}

// shardChanged 判断分片内容是否需要重写(与磁盘现状对比)。
func (rc *reconcile) shardChanged(rel string, nodes []model.Node) bool {
	old, ok := rc.oldShards[rel]
	if !ok {
		return true
	}
	if old.shard.Schema != model.SchemaVersion || len(old.shard.Nodes) != len(nodes) {
		return true
	}
	for i := range nodes {
		if !nodeEqual(&old.shard.Nodes[i], &nodes[i]) {
			return true
		}
	}
	return false
}

func nodeEqual(a, b *model.Node) bool {
	if a.ID != b.ID || a.Level != b.Level || a.Status != b.Status || !a.Since.Equal(b.Since) ||
		a.Anchor != b.Anchor ||
		len(a.Entries) != len(b.Entries) || len(a.Keywords) != len(b.Keywords) || len(a.Lineage) != len(b.Lineage) {
		return false
	}
	for i := range a.Entries {
		if a.Entries[i].ID != b.Entries[i].ID || a.Entries[i].Text != b.Entries[i].Text ||
			a.Entries[i].Confidence != b.Entries[i].Confidence {
			return false
		}
	}
	for i := range a.Keywords {
		if a.Keywords[i] != b.Keywords[i] {
			return false
		}
	}
	for i := range a.Lineage {
		if a.Lineage[i] != b.Lineage[i] {
			return false
		}
	}
	return true
}

// ensureDirAndProjectNodes 逐目录生成 _dir.yaml 与 project.yaml 壳(impl §6 第 3 步),
// 并回收死目录壳(R2-A6):目录下已无任何源文件时,与文件分片同规则——
// 无知识删壳,有知识转孤儿(否则壳节点永久残留索引与统计)。
// 仅目录节点壳,无摘要;文件清单属 auto,读取时现算(不落盘,与 impl §3 定案一致)。
func (rc *reconcile) ensureDirAndProjectNodes() error {
	s := rc.e.Store

	// 目录存活以【全部列出的源文件】为准(含解析失败/大小写碰撞跳过的 skipRel 文件,
	// 保守:只要目录里还有源文件就不回收)。
	dirs := map[string]bool{}
	addDirs := func(rel string) {
		for d := path.Dir(rel); d != "." && d != "/"; d = path.Dir(d) {
			dirs[d] = true
		}
	}
	for _, rel := range rc.files {
		addDirs(rel)
	}
	for rel := range rc.skipRel {
		addDirs(rel)
	}
	if err := rc.reapDeadDirShards(dirs); err != nil {
		return err
	}
	for d := range dirs {
		p := s.DirShardPathFor(d)
		if _, err := os.Stat(p); err == nil {
			continue // 已存在:目录节点可能带知识,不动
		}
		sh := &store.Shard{Schema: model.SchemaVersion, Nodes: []model.Node{{
			ID: model.DirNodeID(d), Level: model.LevelDir,
			Anchor: model.Anchor{File: d + "/"},
			Status: model.StatusUndigested, Since: rc.now,
		}}}
		if err := s.SaveShard(p, sh, nil); err != nil {
			return err
		}
	}

	return rc.ensureProjectNode()
}

// reapDeadDirShards 回收死目录壳(R2-A6)。liveDirs 是仍含源文件的目录集合。
// conflict/schema 隔离分片不动(与文件分片同哲学:坏分片人工解决,不覆盖不删除)。
func (rc *reconcile) reapDeadDirShards(liveDirs map[string]bool) error {
	s := rc.e.Store
	treeRoot := filepath.Join(s.Dir(), "tree")
	return s.WalkTreeShards(func(p string) error {
		if filepath.Base(p) != "_dir.yaml" {
			return nil
		}
		rel, err := filepath.Rel(treeRoot, filepath.Dir(p))
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			return nil
		}
		if liveDirs[filepath.ToSlash(rel)] {
			return nil
		}
		sh, _, lerr := s.LoadShard(p)
		if lerr != nil {
			return nil
		}
		keep, changed := false, false
		for i := range sh.Nodes {
			if len(sh.Nodes[i].Entries) == 0 {
				continue
			}
			keep = true
			if sh.Nodes[i].Status != model.StatusOrphaned {
				sh.Nodes[i].Status = model.StatusOrphaned
				rc.report.Orphaned++
				changed = true
			}
		}
		if !keep {
			shardRel, relErr := filepath.Rel(s.Dir(), p)
			if relErr != nil {
				return relErr
			}
			if err := s.RemoveKnowledgeFile(filepath.ToSlash(shardRel)); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("engine: 删死目录壳 %s: %w", rel, err)
			}
			return nil
		}
		if !changed {
			return nil // 幂等:内容无变不重写
		}
		return s.SaveShard(p, sh, nil)
	})
}

func (rc *reconcile) ensureProjectNode() error {
	s := rc.e.Store
	pp := s.ProjectShardPath()
	if _, err := os.Stat(pp); os.IsNotExist(err) {
		sh := &store.Shard{Schema: model.SchemaVersion, Nodes: []model.Node{{
			ID: model.ProjectNodeID, Level: model.LevelProject,
			Anchor: model.Anchor{File: "."},
			Status: model.StatusUndigested, Since: rc.now,
		}}}
		if err := s.SaveShard(pp, sh, nil); err != nil {
			return err
		}
	}
	return nil
}
