package engine

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zdypro888/iknowledge/internal/index"
	"github.com/zdypro888/iknowledge/internal/model"
	"github.com/zdypro888/iknowledge/internal/parser"
	"github.com/zdypro888/iknowledge/internal/store"
)

// runtime 是 serve 期的内存态:缓存+索引+会话台账+侦查作业表。
//
// R29 批次2 并发改造:mu 由 sync.Mutex 升级为 sync.RWMutex——读路径(Map/Recall/
// Inject/Status)经改造后为纯读(reconcile/callgraph 搬进 reloadLocked 预计算,
// 会话台账与 job 走独立小锁),用 RLock 并发;写路径(remember/record_change/verify/
// adopt/task/flow/maintain/init)与 reloadLocked 用 Lock。
type runtime struct {
	mu    sync.RWMutex
	cache *store.Cache
	ix    *index.Index
	// semanticSourceVersion 只在健康 tree/project 视图变化时递增。
	// manifest 由 semantic_source.go 在主锁外构造，再按 version 原子发布；
	// recall 稳态只读取不可变 map/fingerprint，不再逐次全库脱敏扫描。
	semanticSourceVersion uint64
	semanticManifest      semanticSourceManifest
	// truthTxActive 只在 rt.mu 写锁下访问。事务中途的内部 reload 必须跳过
	// 崩溃恢复；新进程/下次请求则在加载缓存前恢复仓外 prepared WAL。
	truthTxActive bool
	wips          []model.WIP
	flows         []model.Flow
	warns         []string // flows 等加载告警(每次 reload 重算)
	// opsWarns 运行期落盘失败等运营告警(R2-C1):原先混在 warns 里,
	// 每次 Sync 被 LoadFlows 的结果整体覆盖,kb_status(入口先 Sync)永远看不到。
	// 有界去重,进程存续期保留。
	opsWarns []string

	// sessions 走独立小锁(R29 批次2):读路径的会话台账登记/过时警报不再吃大锁,
	// 多读并发不互斥。照 gitCountsMu/pfMu 同型范式。job 仅写路径(investigate)访问,
	// 仍归 rt.mu 保护,无需独立锁。
	sessionMu sync.Mutex
	sessions  map[string]*sessionLedger
	job       *scoutJob // 同 repo 最多 1 个活跃 job(递归护栏);归 rt.mu 保护

	// cg 是全仓调用图(auto 派生值,不落盘;文件指纹增量,见 callgraph.go)。
	// 生命周期独立于 ix:reloadLocked 重建索引不清它,指纹自会对账。
	// R29 批次2:cg 走独立锁 cgMu——它是派生值(只读文件系统+自身 files map,不依赖
	// rt.mu 保护的 cache/ix),读路径增量刷新它不必持 rt.mu 写锁,多读并发可同时建图。
	cgMu sync.Mutex
	cg   *callGraph

	// gitCounts 热区频率因子缓存(60s TTL;git log 大仓库百毫秒级)。
	// 用独立小锁:计算在 rt.mu 之外跑(#21 同族,git 子进程不占大锁)。
	gitCountsMu sync.Mutex
	gitCounts   map[string]int
	gitCountsAt time.Time

	// parseFailed 计数缓存(60s TTL;kb_status 的全库 parse 扫描是最大单项成本,
	// casino 实测数百毫秒——与 gitCounts 同型,同样锁外算)。
	// pfFiles 逐文件指纹缓存(多语言加固:Python 等子进程解析器每文件 ~40ms,
	// 混合仓 200 个 .py 的全量重扫 ≈8s——没变的文件绝不重解析,稳态成本归零)。
	pfMu          sync.Mutex
	parseFailedN  int
	parseFailedAt time.Time
	pfFiles       map[string]pfEntry

	// R29 批次3:config + 源文件列表缓存。原先每次 parseFailedCached / ensureCallGraph /
	// investigate / scout 都各自 LoadConfig(读盘+YAML 解析)且吞错误;listSourceFiles
	// 每次 git ls-files + os.Stat 每文件。单请求内多处调用重复付。现 60s TTL 共享,
	// LoadConfig 错误进 opsWarns 不再静默吞。
	cfgMu     sync.Mutex
	cfgCache  *store.Config
	cfgErr    error
	cfgAt     time.Time
	filesMu   sync.Mutex
	filesList []string
	filesErr  error
	filesAt   time.Time

	// R29 批次3:per-file git trail 缓存。recallNodeLocked 在读锁内调 gitTrail
	// (每文件一次 git log --follow 子进程),阻塞其他读。60s TTL 缓存消除稳态子进程。
	trailMu    sync.Mutex
	trailCache map[string]trailEntry
}

type trailEntry struct {
	text string
	at   time.Time
}

// pfEntry 是单文件的解析结果指纹。
type pfEntry struct {
	mtimeNS int64
	size    int64
	failed  bool
}

// parseFailedCached 返回解析失败文件数(60s TTL + 逐文件指纹增量:指纹没变的
// 文件复用上次结果,不重读不重解析——子进程解析器(Python)在稳态零成本)。
// 不持 rt.mu 调用。
func (e *Engine) parseFailedCached() int {
	e.rt.pfMu.Lock()
	if !e.rt.parseFailedAt.IsZero() && e.now().Sub(e.rt.parseFailedAt) < time.Minute {
		defer e.rt.pfMu.Unlock()
		return e.rt.parseFailedN
	}
	prev := e.rt.pfFiles
	e.rt.pfMu.Unlock()
	if prev == nil {
		prev = map[string]pfEntry{}
	}

	n := 0
	next := map[string]pfEntry{}
	// R29 批次3:用缓存源文件列表(60s TTL,cachedSourceFiles 内部用 cachedConfig)。
	files, err := e.cachedSourceFiles()
	if err == nil {
		for _, rel := range files {
			st, err := safeRepoFileInfo(e.Store.RepoRoot(), rel)
			if err != nil {
				continue
			}
			if pe, ok := prev[rel]; ok && pe.mtimeNS == st.ModTime().UnixNano() && pe.size == st.Size() {
				next[rel] = pe
				if pe.failed {
					n++
				}
				continue
			}
			src, err := safeRepoRead(e.Store.RepoRoot(), rel)
			if err != nil || parser.IsGenerated(src) {
				continue
			}
			_, perr := e.Reg.ForFile(rel).Parse(rel, src)
			pe := pfEntry{mtimeNS: st.ModTime().UnixNano(), size: st.Size(), failed: perr != nil}
			next[rel] = pe
			if pe.failed {
				n++
			}
		}
	}
	e.rt.pfMu.Lock()
	e.rt.parseFailedN, e.rt.parseFailedAt, e.rt.pfFiles = n, e.now(), next
	e.rt.pfMu.Unlock()
	return n
}

// gitCountsCached 返回近 90 天每文件改动计数(60s TTL)。不持 rt.mu 调用。
func (e *Engine) gitCountsCached() map[string]int {
	e.rt.gitCountsMu.Lock()
	if e.rt.gitCounts != nil && e.now().Sub(e.rt.gitCountsAt) < time.Minute {
		defer e.rt.gitCountsMu.Unlock()
		return e.rt.gitCounts
	}
	e.rt.gitCountsMu.Unlock()
	counts := gitChangeCounts(e.Store.RepoRoot(), "90.days")
	e.rt.gitCountsMu.Lock()
	e.rt.gitCounts, e.rt.gitCountsAt = counts, e.now()
	e.rt.gitCountsMu.Unlock()
	return counts
}

// cachedConfig 返回 config(60s TTL,缓存解析结果)。R29 批次3:原先 4 个调用点
// 各自 LoadConfig(读盘+YAML 解析)且 `_, _ :=` 吞错误——config.yaml 坏了静默退化
// 为空(includes/excludes/extensions 全失效)。现在错误保留进 cfgErr,
// kb_status 渲染时 configError() 取出显示。不持 rt.mu 调用。
func (e *Engine) cachedConfig() *store.Config {
	e.rt.cfgMu.Lock()
	if e.rt.cfgCache != nil && e.rt.cfgErr == nil && e.now().Sub(e.rt.cfgAt) < time.Minute {
		defer e.rt.cfgMu.Unlock()
		return e.rt.cfgCache
	}
	e.rt.cfgMu.Unlock()
	cfg, err := e.Store.LoadConfig()
	e.rt.cfgMu.Lock()
	e.rt.cfgCache, e.rt.cfgErr, e.rt.cfgAt = cfg, err, e.now()
	e.rt.cfgMu.Unlock()
	return cfg
}

// configError 返回上次 LoadConfig 的错误(若有),供 kb_status 显示。不持 rt.mu。
func (e *Engine) configError() error {
	e.rt.cfgMu.Lock()
	defer e.rt.cfgMu.Unlock()
	return e.rt.cfgErr
}

// cachedSourceFiles 返回源文件列表(60s TTL)。R29 批次3:parseFailedCached 与
// ensureCallGraphLocked 各自调 listSourceFiles(git ls-files + os.Stat 每文件),
// 单请求重复付。现共享缓存。不持 rt.mu 调用。
func (e *Engine) cachedSourceFiles() ([]string, error) {
	e.rt.filesMu.Lock()
	if e.rt.filesList != nil && e.rt.filesErr == nil && e.now().Sub(e.rt.filesAt) < time.Minute {
		defer e.rt.filesMu.Unlock()
		return e.rt.filesList, nil
	}
	e.rt.filesMu.Unlock()
	cfg := e.cachedConfig()
	files, err := listSourceFiles(e.Store.RepoRoot(), e.Reg, cfg)
	e.rt.filesMu.Lock()
	e.rt.filesList, e.rt.filesErr, e.rt.filesAt = files, err, e.now()
	e.rt.filesMu.Unlock()
	return files, err
}

// cachedGitTrail 返回单文件的 git 提交轨迹(60s TTL 缓存)。R29 批次3:recallNodeLocked
// 在读锁内调 gitTrail(每文件一次 git log --follow 子进程)会阻塞并发读;缓存消除稳态
// 子进程调用。不持 rt.mu 调用(用独立 trailMu),但调用方通常在 RLock 内——子进程在
// RLock 下仍会阻塞同 RLock 的其他 reader(RWMutex 的 RLock 之间也不并发子进程 I/O,
// 但不互斥内存操作);缓存让首次外的调用零子进程。
func (e *Engine) cachedGitTrail(file string) string {
	e.rt.trailMu.Lock()
	if e.rt.trailCache != nil {
		if ent, ok := e.rt.trailCache[file]; ok && e.now().Sub(ent.at) < time.Minute {
			defer e.rt.trailMu.Unlock()
			return ent.text
		}
	}
	e.rt.trailMu.Unlock()
	text := gitTrail(e.Store.RepoRoot(), []string{file})
	e.rt.trailMu.Lock()
	if e.rt.trailCache == nil {
		e.rt.trailCache = map[string]trailEntry{}
	}
	e.rt.trailCache[file] = trailEntry{text: text, at: e.now()}
	e.rt.trailMu.Unlock()
	return text
}

// sessionLedger 是一个会话的读取台账(knowledge.md §9.3):
// recall/读取过哪些节点、当时锚点哈希、时间——过时警报与沉淀提醒的判定依据。
type sessionLedger struct {
	reads      map[string]readRecord // 节点 ID → 最近一次读取
	lastActive time.Time
}

type readRecord struct {
	Hash string
	At   time.Time
	// Digested 该会话是否已对此节点 remember 过(沉淀提醒用:高成本读取未沉淀)。
	Digested bool
	// Reads 读取次数(多次下钻≈理解成本高)。
	Reads int
}

// scoutJob 是一个活跃侦查作业(knowledge.md §10.4 委派主模式)。
type scoutJob struct {
	ID       string
	Question string
	Scope    string
	Started  time.Time
	TTL      time.Duration
	// done 自派备模式的交卷信号(缓冲 1):SubmitFindings 投递格式化 findings,
	// 阻塞中的 Investigate 收货返回。委派模式下无人收货,缓冲写不阻塞。
	done chan string
}

func (j *scoutJob) expired(now time.Time) bool {
	return now.Sub(j.Started) > j.TTL
}

// EnsureRuntime 懒初始化运行时(serve 与写工具共用)。
func (e *Engine) EnsureRuntime() error {
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	return e.reloadLocked()
}

// Sync 每次 MCP 请求前的惰性重载(impl §4):目录清单+mtime+size 对账,
// 变了才重建索引。加锁调用。
func (e *Engine) Sync() error {
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	return e.reloadLocked()
}

// reloadLocked 前提:已持锁。
func (e *Engine) reloadLocked() error {
	if !e.rt.truthTxActive {
		recovered, err := e.Store.RecoverTruthTransactionWithStatus()
		if recovered {
			e.rt.cache = nil // 不能拿半应用文件的 mtime/size 快照解释已恢复原字节
			cfg, cfgErr := e.Store.LoadConfig()
			if cfgErr != nil {
				return fmt.Errorf("恢复事务后重读 config: %w", cfgErr)
			}
			e.Reg = registryForConfig(cfg)
		}
		if err != nil {
			return fmt.Errorf("恢复崩溃事务: %w", err)
		}
	}
	newCache := e.rt.cache == nil
	if newCache {
		e.rt.cache = store.NewCache(e.Store)
		e.rt.sessionMu.Lock()
		if e.rt.sessions == nil {
			e.rt.sessions = map[string]*sessionLedger{}
		}
		e.rt.sessionMu.Unlock()
	}
	refresh, err := e.rt.cache.Refresh()
	if err != nil {
		return err
	}
	// flows/wip 体量小,每次直读(不进 mtime 缓存,switch 分支后天然新鲜)。
	flows, warns, err := e.Store.LoadFlows()
	if err != nil {
		return err
	}
	wips, err := e.Store.LoadWIPs()
	if err != nil {
		return err
	}
	e.rt.flows, e.rt.warns, e.rt.wips = flows, warns, wips
	// flow/wip 变化无 mtime 追踪,索引重建成本毫秒级,始终重建保正确。
	changes, _ := e.rt.cache.Journal()
	// index 不依赖 store(impl §2 依赖方向):把快照筛成"rel → 节点切片"再交 Build,
	// conflict/schema 隔离的分片(cs.Err)不进索引;切片头拷贝共享底层数组,
	// 索引里的节点指针仍指向缓存实体,写路径经索引改节点再存盘的语义不变。
	healthy := make(map[string][]model.Node, len(e.rt.cache.Shards()))
	for rel, cs := range e.rt.cache.Shards() {
		if cs.Err != nil || cs.Shard == nil {
			continue
		}
		healthy[rel] = cs.Shard.Nodes
	}
	e.rt.ix = index.Build(healthy, changes, flows)
	for _, id := range e.rt.ix.DuplicateNodeIDs() {
		e.rt.warns = append(e.rt.warns, "重复 node ID 已隔离:"+id+"(检查多个 tree 分片/import remap)")
	}
	// R29 批次2:读路径状态对账预算(从 reconcileOnReadLocked 外移)。写锁内做,
	// 使读路径变纯读——失配降 suspect、pending_anchor 补全、回到锚定恢复 fresh。
	// 只对有活跃知识且 fresh/suspect/pending 的节点做,成本受限。
	e.reconcileAllLocked()
	if newCache || refreshChangesSemanticSource(refresh) {
		e.rt.semanticSourceVersion++
		if e.rt.semanticSourceVersion == 0 { // 理论上的 uint64 回绕也不能复用旧 manifest。
			e.rt.semanticSourceVersion = 1
		}
		e.rt.semanticManifest = semanticSourceManifest{}
	}
	return nil
}

func refreshChangesSemanticSource(rep store.RefreshReport) bool {
	for _, paths := range [][]string{rep.Added, rep.Changed, rep.Removed} {
		for _, rel := range paths {
			if rel == "project.yaml" || strings.HasPrefix(rel, "tree/") {
				return true
			}
		}
	}
	return false
}

// UsageRecord 是使用日志行(store.UsageRecord 的别名,R2-B5):
// 类型与写入都经 engine 转发,维持 impl §2 依赖方向 mcpserv → engine
// (mcpserv 原先为它直接 import store,是声明外的依赖边)。
type UsageRecord = store.UsageRecord

// LogUsage 追加一行使用日志(尽力而为:日志失败不影响业务,store 侧静默)。
func (e *Engine) LogUsage(month string, rec UsageRecord) { e.Store.AppendUsage(month, rec) }

// warnOpsLocked 记一条运营告警:去重 + 上限 20(防重复失败刷屏/无界增长)。前提:已持锁。
func (e *Engine) warnOpsLocked(msg string) {
	if slices.Contains(e.rt.opsWarns, msg) || len(e.rt.opsWarns) >= 20 {
		return
	}
	e.rt.opsWarns = append(e.rt.opsWarns, msg)
}

// ledgerTTL 是会话台账的空闲上限;超过则回收(#20:防长跑 serve 内存无界增长)。
// 命名区别于 mcpserv 的 sessionTTL(MCP 协议会话 24h):台账丢了只损失过时警报
// 的比对基线,代价低,回收更激进。
const ledgerTTL = 2 * time.Hour

// ledgerSnapshot 取(或建)会话台账的只读快照;sid 为空(匿名连接)返回 nil。
// sessionLedger 内含 map,绝不能把裸指针带出 sessionMu——Inject 与 Recall 都持
// rt.mu.RLock,同一会话可以并发进入;锁外遍历 reads 会与 recordRead 写 map 竞态。
func (e *Engine) ledgerSnapshot(sid string) map[string]readRecord {
	if sid == "" {
		return nil
	}
	e.rt.sessionMu.Lock()
	defer e.rt.sessionMu.Unlock()
	e.evictStaleSessionsLocked()
	l, ok := e.rt.sessions[sid]
	if !ok {
		l = &sessionLedger{reads: map[string]readRecord{}}
		e.rt.sessions[sid] = l
	}
	l.lastActive = e.now()
	out := make(map[string]readRecord, len(l.reads))
	for id, rec := range l.reads {
		out[id] = rec
	}
	return out
}

// evictStaleSessionsLocked 回收空闲超过 TTL 的会话台账。前提:已持 sessionMu。
func (e *Engine) evictStaleSessionsLocked() {
	now := e.now()
	for sid, l := range e.rt.sessions {
		if now.Sub(l.lastActive) > ledgerTTL {
			delete(e.rt.sessions, sid)
		}
	}
}

// recordRead 登记读取(台账);返回过时警报文本(若该会话读过旧版)。
// R29 批次2:reads 走 sessionMu;staleAlert 由调用方的 rt.mu 锁保护(不在本函数取)。
// sid 为空(匿名)直接返回空——不建台账。
func (e *Engine) recordRead(sid, nodeID, curHash string) string {
	if sid == "" {
		return ""
	}
	e.rt.sessionMu.Lock()
	e.evictStaleSessionsLocked()
	l, ok := e.rt.sessions[sid]
	if !ok {
		l = &sessionLedger{reads: map[string]readRecord{}}
		e.rt.sessions[sid] = l
	}
	l.lastActive = e.now()
	prev, seen := l.reads[nodeID]
	rec := readRecord{Hash: curHash, At: e.now(), Reads: prev.Reads + 1, Digested: prev.Digested}
	l.reads[nodeID] = rec
	e.rt.sessionMu.Unlock()
	if seen && prev.Hash != "" && curHash != "" && prev.Hash != curHash {
		return e.staleAlert(nodeID, prev)
	}
	return ""
}

// staleAlert 组装过时警报(knowledge.md §9.5):要具体——谁改的、改了什么、为什么。
// R29 批次2:前提——调用方已持 rt.mu 的读锁或写锁(ix.History 读索引快照)。
// recordRead 在 recallNodeLocked(Recall 的 RLock/Lock 区间)内调本函数,锁已持有,
// 这里不再自取(Go RWMutex 不支持重入,重复取会死锁)。
func (e *Engine) staleAlert(nodeID string, prev readRecord) string {
	var b strings.Builder
	fmt.Fprintf(&b, "⚠ 过时警报:你在本会话早前读过 %s(当时哈希 %s),它之后已被修改。", nodeID, shortHash(prev.Hash))
	hist := e.rt.ix.History(nodeID)
	for i := len(hist) - 1; i >= 0; i-- {
		if hist[i].At.After(prev.At) {
			c := hist[i]
			fmt.Fprintf(&b, "最近变更:「%s」(原因:%s", c.What, c.Why)
			if c.Author != "" {
				fmt.Fprintf(&b, ";作者:%s", c.Author)
			}
			b.WriteString(")。")
			break
		}
	}
	b.WriteString("**你记忆中的版本已过时,禁止凭旧记忆修改,必须重读原文。**")
	return b.String()
}

func shortHash(h string) string {
	const p = "sha256:"
	if len(h) > len(p)+8 {
		return h[len(p) : len(p)+8]
	}
	return h
}

// markDigested 会话对节点完成过沉淀(remember/record_change),沉淀提醒不再点名它。
// R29 批次2:走 sessionMu。
func (e *Engine) markDigested(sid string, nodeIDs ...string) {
	e.rt.sessionMu.Lock()
	defer e.rt.sessionMu.Unlock()
	l, ok := e.rt.sessions[sid]
	if !ok {
		return
	}
	for _, id := range nodeIDs {
		rec := l.reads[id]
		rec.Digested = true
		l.reads[id] = rec
	}
}

// settleReminder 沉淀提醒(knowledge.md §9.3 第 2 用途):台账里多次下钻却没
// 对应 remember 的节点,提醒补沉淀——对抗"读过即弃"。上限 3 条(§12.2 同限额哲学)。
// R29 批次2:走 sessionMu。
func (e *Engine) settleReminder(sid string) []string {
	e.rt.sessionMu.Lock()
	defer e.rt.sessionMu.Unlock()
	l, ok := e.rt.sessions[sid]
	if !ok {
		return nil
	}
	var out []string
	for id, rec := range l.reads {
		if rec.Reads >= 2 && !rec.Digested {
			out = append(out, id)
			if len(out) >= 3 {
				break
			}
		}
	}
	return out
}
