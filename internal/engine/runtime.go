package engine

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zdypro888/iknowledge/internal/index"
	"github.com/zdypro888/iknowledge/internal/model"
	"github.com/zdypro888/iknowledge/internal/store"
)

// runtime 是 serve 期的内存态:缓存+索引+会话台账+侦查作业表。
// 写操作服务端内部串行化(knowledge.md §10.5 单一写入口)——mu 全局互斥。
type runtime struct {
	mu    sync.Mutex
	cache *store.Cache
	ix    *index.Index
	wips  []model.WIP
	flows []model.Flow
	warns []string // flows 等加载告警(每次 reload 重算)
	// opsWarns 运行期落盘失败等运营告警(R2-C1):原先混在 warns 里,
	// 每次 Sync 被 LoadFlows 的结果整体覆盖,kb_status(入口先 Sync)永远看不到。
	// 有界去重,进程存续期保留。
	opsWarns []string

	sessions map[string]*sessionLedger
	job      *scoutJob // 同 repo 最多 1 个活跃 job(递归护栏)

	// cg 是全仓调用图(auto 派生值,不落盘;文件指纹增量,见 callgraph.go)。
	// 生命周期独立于 ix:reloadLocked 重建索引不清它,指纹自会对账。
	cg *callGraph
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
	if e.rt.cache == nil {
		e.rt.cache = store.NewCache(e.Store)
		e.rt.sessions = map[string]*sessionLedger{}
	}
	if _, err := e.rt.cache.Refresh(); err != nil {
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
	return nil
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

// ledger 取(或建)会话台账;sid 为空(匿名连接)返回 nil——台账类功能退化关闭。
func (e *Engine) ledger(sid string) *sessionLedger {
	if sid == "" {
		return nil
	}
	e.evictStaleSessionsLocked()
	l, ok := e.rt.sessions[sid]
	if !ok {
		l = &sessionLedger{reads: map[string]readRecord{}}
		e.rt.sessions[sid] = l
	}
	l.lastActive = e.now()
	return l
}

// evictStaleSessionsLocked 回收空闲超过 TTL 的会话台账。前提:已持锁。
func (e *Engine) evictStaleSessionsLocked() {
	now := e.now()
	for sid, l := range e.rt.sessions {
		if now.Sub(l.lastActive) > ledgerTTL {
			delete(e.rt.sessions, sid)
		}
	}
}

// recordRead 登记读取(台账);返回过时警报文本(若该会话读过旧版)。
func (e *Engine) recordRead(sid, nodeID, curHash string) string {
	l := e.ledger(sid)
	if l == nil {
		return ""
	}
	prev, seen := l.reads[nodeID]
	rec := readRecord{Hash: curHash, At: e.now(), Reads: prev.Reads + 1, Digested: prev.Digested}
	l.reads[nodeID] = rec
	if seen && prev.Hash != "" && curHash != "" && prev.Hash != curHash {
		return e.staleAlert(nodeID, prev)
	}
	return ""
}

// staleAlert 组装过时警报(knowledge.md §9.5):要具体——谁改的、改了什么、为什么。
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
func (e *Engine) markDigested(sid string, nodeIDs ...string) {
	l := e.ledger(sid)
	if l == nil {
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
func (e *Engine) settleReminder(sid string) []string {
	l := e.ledger(sid)
	if l == nil {
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
