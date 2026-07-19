package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/zdypro888/iknowledge/internal/model"
)

// 维护欠账队列(knowledge.md §12.7):服务端只做检测与记账,AI 管语言。
// 定案(全量实现):队列是**现算派生值**,不落盘——欠账由成因(摘要落后/历史超预算/
// 疑似重复)现场推导,成因消除欠账自动消失,不存在队列本身腐烂的问题。

// Debt 是一条维护欠账。
type Debt struct {
	ID   string // 稳定推导:kind+node 的短哈希(同一成因两次 next 拿到同一 ID)
	Kind string // era-compress | summary-stale | dup-entries | review-overdue
	Node string
	Desc string
	Hint string
}

// reviewOverdueAfter 是非代码知识的复核周期(knowledge.md §8.4:无代码哈希锚,
// 失效检测靠"时间 + 人工复核")。90 天是经验值,与 §12.3 阈值同哲学:待实测调参。
const reviewOverdueAfter = 90 * 24 * time.Hour

func debtID(kind, node string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + node))
	return "d_" + hex.EncodeToString(sum[:4])
}

// countInferred 数节点里活跃的 inferred 条目(置信度桥接判定,write/maintain 共用)。
func countInferred(n *model.Node) int {
	c := 0
	for i := range n.Entries {
		if e := &n.Entries[i]; e.Active() && e.Confidence == model.ConfidenceInferred {
			c++
		}
	}
	return c
}

// historyHasVerified 判断变更历史里有无带验证依据的记录(测试/红绿证据)。
func historyHasVerified(hist []model.Change) bool {
	for i := range hist {
		if strings.TrimSpace(hist[i].Verified) != "" {
			return true
		}
	}
	return false
}

// maintainContextHeapSort keeps debt derivation cancellable while avoiding an
// additional O(n) scratch buffer. Every comparison/extraction path crosses a
// context checkpoint at least once per 64 operations.
func maintainContextHeapSort[T any](ctx context.Context, values []T, less func(a, b T) bool) error {
	if ctx == nil {
		return fmt.Errorf("maintain sort: nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	checks := 0
	checkpoint := func() error {
		checks++
		if checks&63 == 0 {
			return ctx.Err()
		}
		return nil
	}
	checkedLess := func(a, b T) (bool, error) {
		if err := checkpoint(); err != nil {
			return false, err
		}
		return less(a, b), nil
	}
	var sift func(root, end int) error
	sift = func(root, end int) error {
		for {
			child := root*2 + 1
			if child >= end {
				return nil
			}
			if child+1 < end {
				rightGreater, err := checkedLess(values[child], values[child+1])
				if err != nil {
					return err
				}
				if rightGreater {
					child++
				}
			}
			rootLess, err := checkedLess(values[root], values[child])
			if err != nil {
				return err
			}
			if !rootLess {
				return nil
			}
			values[root], values[child] = values[child], values[root]
			root = child
		}
	}
	for root := len(values)/2 - 1; root >= 0; root-- {
		if err := sift(root, len(values)); err != nil {
			return err
		}
	}
	for end := len(values) - 1; end > 0; end-- {
		if err := checkpoint(); err != nil {
			return err
		}
		values[0], values[end] = values[end], values[0]
		if err := sift(0, end); err != nil {
			return err
		}
	}
	return ctx.Err()
}

type maintainDupEntry struct {
	nodeID string
	entry  *model.Entry
	grams  map[string]bool
}

// maintainBigramSimilarContext compares cached bigram sets. Jaccard cannot
// exceed min(size)/max(size), so the length bound avoids intersections that
// cannot possibly cross the strict 0.8 threshold.
func maintainBigramSimilarContext(a, b map[string]bool, checkpoint func() error) (bool, error) {
	if len(a) == 0 || len(b) == 0 {
		return false, nil
	}
	smaller, larger := a, b
	if len(smaller) > len(larger) {
		smaller, larger = larger, smaller
	}
	if len(smaller)*5 <= len(larger)*4 {
		return false, nil
	}
	intersection := 0
	for gram := range smaller {
		if err := checkpoint(); err != nil {
			return false, err
		}
		if larger[gram] {
			intersection++
		}
	}
	// intersection / union > 4/5, rearranged to avoid floating point.
	return intersection*9 > (len(a)+len(b))*4, nil
}

// computeDebtsLocked 现算全部欠账(排除已消解的,#11)。前提:已持锁。
func (e *Engine) computeDebtsLocked() []Debt {
	debts, _ := e.computeDebtsLockedContext(context.Background())
	return debts
}

// computeDebtsLockedContext is the cancellable debt derivation path. All
// intermediate slices remain local and are discarded on cancellation, so a
// caller never observes a partially derived queue. 前提:已持锁。
func (e *Engine) computeDebtsLockedContext(ctx context.Context) ([]Debt, error) {
	if ctx == nil {
		return nil, fmt.Errorf("compute debts: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dismissed, _ := e.Store.LoadDismissedDebts()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var debts []Debt
	checks := 0
	checkpoint := func() error {
		checks++
		if checks&63 == 0 {
			return ctx.Err()
		}
		return nil
	}
	nodes := e.rt.ix.Nodes()
	var allDupEntries []maintainDupEntry

	// ⑥ suspect 待重验(§12.2 偿还机制的队列化,2026-07-04:实战反馈"发现不等于修复,
	// 欠账终究要有人还"——suspect 原先只在读到时提醒,不进欠账队列,冷区的 suspect
	// 可以烂很久没人管)。超 20 个聚合为一条(mass-suspect 是全局性事件,逐条派账
	// 刷屏无意义,批量出口是 kb_init reanchor_all,impl §6)。
	var suspects []string
	for id, ref := range nodes {
		if err := checkpoint(); err != nil {
			return nil, err
		}
		if ref.Node.Status == model.StatusSuspect {
			suspects = append(suspects, id)
		}
	}
	if err := maintainContextHeapSort(ctx, suspects, func(a, b string) bool { return a < b }); err != nil {
		return nil, err
	}
	if len(suspects) > 20 {
		debts = append(debts, Debt{
			ID: debtID("suspect-mass", "."), Kind: "suspect-reverify", Node: ".",
			Desc: fmt.Sprintf("%d 个节点处于 suspect(疑似全局性变更)", len(suspects)),
			Hint: "人工确认变更为预期(如全库格式化/大重构)后用 kb_init reanchor_all=true 批量重锚;否则逐个 kb_recall 读原文 + kb_verify confirm/refute",
		})
	} else {
		for _, id := range suspects {
			if err := checkpoint(); err != nil {
				return nil, err
			}
			debts = append(debts, Debt{
				ID: debtID("suspect", id), Kind: "suspect-reverify", Node: id,
				Desc: "节点 suspect:代码在知识写入后已变更且无对应变更记录",
				Hint: "kb_recall 读原文核对:知识仍成立则 kb_verify confirm(整节点 ID,重验即重锚);不成立则 refute(附证据)/obsolete;若是你改的,先补 kb_record_change",
			})
		}
	}

	for id, ref := range nodes {
		if err := checkpoint(); err != nil {
			return nil, err
		}
		n := ref.Node
		hist := e.rt.ix.History(id)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// ① 历史超预算 → 时代摘要债(§12.3:未折叠 >10 条或 >600 token)。
		historyCount, historyTokens := 0, 0
		for i := range hist {
			if err := checkpoint(); err != nil {
				return nil, err
			}
			change := &hist[i]
			if !n.EraUntil.IsZero() && !change.At.After(n.EraUntil) {
				continue
			}
			historyCount++
			historyTokens += EstimateTokens(change.What + change.Why)
		}
		if historyCount > 10 || historyTokens > 600 {
			debts = append(debts, Debt{
				ID: debtID("era", id), Kind: "era-compress", Node: id,
				Desc: "未折叠历史超预算(>10 条或 >600 token)",
				Hint: "读该节点 history 全量,把'近 5 条'之外的记录压缩成时代摘要——负知识(否决项)必须逐条保留在摘要文本里;然后 kb_maintain complete 提交 era_summary",
			})
		}
		// ② 文件摘要落后:file 节点的 summary 早于其下最近变更(§12.7)。
		if n.Level == model.LevelFile {
			var summaryAt = n.Since
			hasSummary := false
			for i := range n.Entries {
				if err := checkpoint(); err != nil {
					return nil, err
				}
				en := &n.Entries[i]
				if en.Active() && en.Kind == model.KindSummary {
					hasSummary = true
					if en.At.After(summaryAt) {
						summaryAt = en.At
					}
				}
			}
			if hasSummary {
				late := 0
				// The index already publishes file→nodes, so file summaries scan
				// only their own symbols instead of multiplying files×all nodes.
				for _, childID := range e.rt.ix.FileNodes(n.ID) {
					if err := checkpoint(); err != nil {
						return nil, err
					}
					f, sym := model.SplitNodeID(childID)
					if f != n.ID || sym == "" {
						continue
					}
					childHist := e.rt.ix.History(childID)
					if err := ctx.Err(); err != nil {
						return nil, err
					}
					for i := range childHist {
						if err := checkpoint(); err != nil {
							return nil, err
						}
						if childHist[i].At.After(summaryAt) {
							late++
						}
					}
				}
				if late > 0 {
					debts = append(debts, Debt{
						ID: debtID("summary", n.ID), Kind: "summary-stale", Node: n.ID,
						Desc: fmt.Sprintf("文件摘要落后于其下 %d 次变更", late),
						Hint: "重读该文件的函数级知识与近期变更,kb_remember 一条新 summary 并 supersedes 旧摘要",
					})
				}
			}
		}
		// ⑦ 置信度滞后(2026-07-04,实战反馈"阶梯塌成单层"):节点 fresh、有 inferred
		// 条目、且历史里有带 verified 的变更(测试/红绿证据)——代码有验证背书、知识仍
		// 匹配代码,却没人 confirm 升级。补账通道(即时提示在 record_change 回执,此处
		// 捞存量:种子期写入、后来才被验证过的知识)。
		if n.Anchor.Hash != "" && n.Status == model.StatusFresh {
			inf := 0
			for i := range n.Entries {
				if err := checkpoint(); err != nil {
					return nil, err
				}
				en := &n.Entries[i]
				if en.Active() && en.Confidence == model.ConfidenceInferred {
					inf++
				}
			}
			hasVerified := false
			if inf > 0 {
				for i := range hist {
					if err := checkpoint(); err != nil {
						return nil, err
					}
					if strings.TrimSpace(hist[i].Verified) != "" {
						hasVerified = true
						break
					}
				}
			}
			if inf > 0 && hasVerified {
				debts = append(debts, Debt{
					ID: debtID("conflag", id), Kind: "confidence-lag", Node: id,
					Desc: fmt.Sprintf("%d 条 inferred 知识,但该节点有测试验证过的变更记录", inf),
					Hint: "读该节点知识与其 history(kb_recall mode=history 看 verified 依据):对仍准确描述当前代码的条目 kb_verify confirm 升 verified;不准的 refute。测试验证的是代码行为,知识文本的准确性要你确认",
				})
			}
		}
		// ④ 非代码知识超期未复核(§8.4:无锚知识的时间锚)。零值时间按超期处理
		//(旧分片缺字段,保守:报一次,confirm 后有了时间锚就不再复报)。
		if n.Anchor.Hash == "" && !n.PendingAnchor {
			overdue := 0
			var oldest string
			for i := range n.Entries {
				if err := checkpoint(); err != nil {
					return nil, err
				}
				en := &n.Entries[i]
				if !en.Active() {
					continue
				}
				last := en.At
				if en.ConfirmedAt.After(last) {
					last = en.ConfirmedAt
				}
				if e.now().Sub(last) > reviewOverdueAfter {
					overdue++
					if oldest == "" {
						oldest = en.ID
					}
				}
			}
			if overdue > 0 {
				debts = append(debts, Debt{
					ID: debtID("review", id), Kind: "review-overdue", Node: id,
					Desc: fmt.Sprintf("%d 条非代码知识超过 %d 天未复核(如 %s)", overdue, int(reviewOverdueAfter.Hours()/24), oldest),
					Hint: "无代码锚的知识不会因代码变更自动失效,只能定期人工核实:仍成立则 kb_verify confirm(条目引用或整节点 ID)刷新确认时间;不再成立则 refute(附证据)/obsolete",
				})
			}
		}
		// ⑤ 矛盾待裁决(§12.4):双方均活跃的 disputes 声明。语义矛盾服务端测不出
		//(§12.7 定案不变),这里只派"AI 已声明、尚未裁决"的账——成因(任一方退场)
		// 消除即自动消失,与其余债种同型。
		for i := range n.Entries {
			if err := checkpoint(); err != nil {
				return nil, err
			}
			en := &n.Entries[i]
			if !en.Active() {
				continue
			}
			for _, d := range en.Disputes {
				if err := checkpoint(); err != nil {
					return nil, err
				}
				_, t, resolveErr := e.rt.ix.ResolveEntryContext(ctx, d, 0)
				if resolveErr != nil {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
					continue
				}
				if t != nil && t.Active() {
					from := id + "#" + en.ID
					debts = append(debts, Debt{
						ID: debtID("dispute:"+from+":"+d, id), Kind: "dispute-open", Node: id,
						Desc: "条目 " + en.ID + " 与 " + d + " 矛盾待裁决",
						Hint: "读双方文本与依据(原文优先):kb_verify refute 错误方(附证据)或 obsolete 过时方;证据在代码之外则升级给人;实非矛盾则 dismiss",
					})
				}
			}
		}
		// ③ 疑似重复条目(bigram>0.8 的活跃对)。
		var actives []maintainDupEntry
		for i := range n.Entries {
			if err := checkpoint(); err != nil {
				return nil, err
			}
			if n.Entries[i].Active() {
				entry := &n.Entries[i]
				dupEntry := maintainDupEntry{nodeID: id, entry: entry, grams: bigramSet(entry.Text)}
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				actives = append(actives, dupEntry)
				allDupEntries = append(allDupEntries, dupEntry)
			}
		}
		for i := 0; i < len(actives); i++ {
			if err := checkpoint(); err != nil {
				return nil, err
			}
			for j := i + 1; j < len(actives); j++ {
				if err := checkpoint(); err != nil {
					return nil, err
				}
				similar, err := maintainBigramSimilarContext(actives[i].grams, actives[j].grams, checkpoint)
				if err != nil {
					return nil, err
				}
				if similar {
					debts = append(debts, Debt{
						ID: debtID("dup:"+actives[i].entry.ID+":"+actives[j].entry.ID, id), Kind: "dup-entries", Node: id,
						Desc: "条目 " + actives[i].entry.ID + " 与 " + actives[j].entry.ID + " 疑似重复(相似度>0.8)",
						Hint: "读两条内容,kb_remember 一条合并文本并 supersedes 两者(语义判定归你,服务端只报机械信号)",
					})
				}
			}
		}
	}
	// ⑤ 跨节点疑似重复(R29 批次4):dup-entries 只比同节点内;不同节点写了语义
	// 相同的条目(如 login.go#Login 和 auth_handler.go#PostLogin 各写一条"密码明文传")
	// 永远不被标重复,双双注入撑爆 context。这里跨节点两两 bigram,scope 限界,
	// 上限 5 条。bigram 集合复用同节点检测已经构建的缓存。
	crossDups, err := crossNodeDupCandidatesPreparedContext(ctx, allDupEntries)
	if err != nil {
		return nil, err
	}
	debts = append(debts, crossDups...)
	if len(dismissed) > 0 {
		kept := debts[:0]
		for _, d := range debts {
			if err := checkpoint(); err != nil {
				return nil, err
			}
			if !dismissed[d.ID] {
				kept = append(kept, d)
			}
		}
		debts = kept
	}
	if err := maintainContextHeapSort(ctx, debts, func(a, b Debt) bool { return a.ID < b.ID }); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return debts, nil
}

// Debts 现算全部维护欠账(CLI `iknowledge maintain` 的只读视图;
// MCP 侧取用/销账仍走 kb_maintain,knowledge.md §12.7)。
func (e *Engine) Debts() ([]Debt, error) {
	if err := e.requireInit(); err != nil {
		return nil, err
	}
	if err := e.Sync(); err != nil {
		return nil, err
	}
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	return e.computeDebtsLocked(), nil
}

// MaintainArgs 是 kb_maintain 入参。
type MaintainArgs struct {
	Action     string `json:"action"` // next | complete | dismiss
	ID         string `json:"id,omitempty"`
	Scope      string `json:"scope,omitempty"`                     // 路径前缀:只取本任务相关的债
	EraSummary string `json:"era_summary,omitempty" redact:"true"` // era-compress 完成时提交
}

func debtInScope(node, scope string) bool {
	scope = strings.Trim(strings.TrimSpace(model.ToSlash(scope)), "/")
	if scope == "" {
		return true
	}
	if strings.Contains(scope, "#") {
		return node == scope
	}
	file, _ := model.SplitNodeID(node)
	file = strings.TrimSuffix(file, "/")
	if scope == model.ProjectNodeID {
		return file == model.ProjectNodeID
	}
	return file == scope || strings.HasPrefix(file, scope+"/")
}

// Maintain 维护欠账:next 取一条最高优先级欠账;complete 销账(era 债携带摘要落库)。
func (e *Engine) Maintain(a MaintainArgs, sid, author string) (out string, err error) {
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

	debts := e.computeDebtsLocked()

	switch a.Action {
	case "next":
		for _, d := range debts {
			if !debtInScope(d.Node, a.Scope) {
				continue
			}
			return fmt.Sprintf("欠账 %s(%s)\n节点: %s\n成因: %s\n操作: %s\n完成后 kb_maintain complete id=%s",
				d.ID, d.Kind, d.Node, d.Desc, d.Hint, d.ID), nil
		}
		if a.Scope != "" {
			return "范围 " + a.Scope + " 内无欠账。", nil
		}
		return "无维护欠账。", nil

	case "complete":
		if a.ID == "" {
			return "", kbErr("INVALID_ARGUMENT", "complete 需要 id", "先 kb_maintain next 取欠账")
		}
		var target *Debt
		for i := range debts {
			if debts[i].ID == a.ID {
				target = &debts[i]
			}
		}
		if target == nil {
			// 欠账现算:成因已消除即视为销账成功(例如 supersedes 合并后 dup 债自动消失)。
			return "ack:欠账 " + a.ID + " 的成因已消除(或不存在),视为已销。", nil
		}
		if target.Kind == "era-compress" {
			if strings.TrimSpace(a.EraSummary) == "" {
				return "", kbErr("EVIDENCE_REQUIRED",
					"era-compress 债的销账必须携带 era_summary(时代摘要文本,负知识逐条保留)",
					"读全量 history 后提交摘要")
			}
			ref := e.rt.ix.Node(target.Node)
			hist := e.rt.ix.History(target.Node)
			if ref == nil || len(hist) == 0 {
				return "", kbErr("NODE_NOT_FOUND", "节点或历史不存在", "kb_status 核对")
			}
			// 折叠点:保留最近 5 条,其余并入时代摘要(§12.3)。
			if len(hist) <= 5 {
				return "ack:未折叠历史已不足 5 条,无需压缩。", nil
			}
			cut := hist[len(hist)-6] // 第 6 新的记录及更早折叠
			n := ref.Node
			n.EraSummary = strings.TrimSpace(a.EraSummary)
			n.EraUntil = cut.At
			if err := e.saveNodeShardLocked(ref); err != nil {
				return "", err
			}
			if err := e.reloadLocked(); err != nil {
				return "", err
			}
			return fmt.Sprintf("ack:时代摘要已落库,折叠至 %s(原始记录仍在 journal 可溯)",
				cut.At.Format("2006-01-02")), nil
		}
		// summary-stale / dup-entries:成因仍在,complete 无效——用 kb_remember 消因,
		// 或 dismiss(#11:dup-entries 是 bigram 启发式,AI 判定实为不同则消解不复现)。
		hint := target.Hint
		if target.Kind == "dup-entries" {
			hint += ";若判定两条实为不同,用 action=dismiss 消解(不再复报)"
		}
		return "", kbErr("EVIDENCE_REQUIRED", "欠账 "+a.ID+" 的成因仍在("+target.Desc+")", hint)

	case "dismiss":
		// 消解假阳性欠账(#11):记进 local,现算时排除,不再复报。
		if a.ID == "" {
			return "", kbErr("INVALID_ARGUMENT", "dismiss 需要 id", "先 kb_maintain next 取欠账")
		}
		if err := e.Store.DismissDebt(a.ID); err != nil {
			return "", err
		}
		return "ack:欠账 " + a.ID + " 已消解,后续不再复报(误判可删 .knowledge/local/dismissed-debts.txt 对应行)。", nil

	case "patrol":
		// 跨节点矛盾巡检(2026-07-05):纯只读简报,不开 job 不记状态(patrol.go)。
		return e.patrolBriefLocked(a.Scope), nil
	}
	return "", kbErr("INVALID_ARGUMENT", "非法 action "+a.Action, "action ∈ next|complete|dismiss|patrol")
}

// crossNodeDupCandidates 扫描跨节点疑似重复条目(R29 批次4)。旧调用保留
// background wrapper；上下文版本供可取消读路径使用。前提:已持 rt.mu。
func (e *Engine) crossNodeDupCandidates() []Debt {
	debts, _ := e.crossNodeDupCandidatesContext(context.Background())
	return debts
}

func (e *Engine) crossNodeDupCandidatesContext(ctx context.Context) ([]Debt, error) {
	if ctx == nil {
		return nil, fmt.Errorf("cross-node duplicate scan: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	checks := 0
	checkpoint := func() error {
		checks++
		if checks&63 == 0 {
			return ctx.Err()
		}
		return nil
	}
	var all []maintainDupEntry
	for id, ref := range e.rt.ix.Nodes() {
		if err := checkpoint(); err != nil {
			return nil, err
		}
		for i := range ref.Node.Entries {
			if err := checkpoint(); err != nil {
				return nil, err
			}
			if ref.Node.Entries[i].Active() {
				entry := &ref.Node.Entries[i]
				all = append(all, maintainDupEntry{nodeID: id, entry: entry, grams: bigramSet(entry.Text)})
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
		}
	}
	return crossNodeDupCandidatesPreparedContext(ctx, all)
}

// crossNodeDupCandidatesPreparedContext uses a rare-gram inverted index rather
// than comparing every pair. A Jaccard score >0.8 means fewer than 20% of
// either set can be absent from the other, so probing the rarest ceil(20%) of
// each entry's grams guarantees that every qualifying pair shares a probed
// gram. Cached sets make each active entry pay tokenization only once.
func crossNodeDupCandidatesPreparedContext(ctx context.Context, all []maintainDupEntry) ([]Debt, error) {
	if ctx == nil {
		return nil, fmt.Errorf("cross-node duplicate candidates: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	checks := 0
	checkpoint := func() error {
		checks++
		if checks&63 == 0 {
			return ctx.Err()
		}
		return nil
	}
	if err := maintainContextHeapSort(ctx, all, func(a, b maintainDupEntry) bool {
		if a.nodeID != b.nodeID {
			return a.nodeID < b.nodeID
		}
		return a.entry.ID < b.entry.ID
	}); err != nil {
		return nil, err
	}

	inverted := make(map[string][]int)
	for i := range all {
		if err := checkpoint(); err != nil {
			return nil, err
		}
		for gram := range all[i].grams {
			if err := checkpoint(); err != nil {
				return nil, err
			}
			inverted[gram] = append(inverted[gram], i)
		}
	}
	type gramRank struct {
		gram  string
		count int
	}
	var debts []Debt
	for i := 0; i < len(all) && len(debts) < 5; i++ {
		if err := checkpoint(); err != nil {
			return nil, err
		}
		if len(all[i].grams) == 0 {
			continue
		}
		ranks := make([]gramRank, 0, len(all[i].grams))
		for gram := range all[i].grams {
			if err := checkpoint(); err != nil {
				return nil, err
			}
			ranks = append(ranks, gramRank{gram: gram, count: len(inverted[gram])})
		}
		if err := maintainContextHeapSort(ctx, ranks, func(a, b gramRank) bool {
			if a.count != b.count {
				return a.count < b.count
			}
			return a.gram < b.gram
		}); err != nil {
			return nil, err
		}
		probeCount := (len(ranks) + 4) / 5
		candidateSet := make(map[int]bool)
		for p := 0; p < probeCount; p++ {
			if err := checkpoint(); err != nil {
				return nil, err
			}
			for _, j := range inverted[ranks[p].gram] {
				if err := checkpoint(); err != nil {
					return nil, err
				}
				if j <= i || all[i].nodeID == all[j].nodeID {
					continue
				}
				smaller, larger := len(all[i].grams), len(all[j].grams)
				if smaller > larger {
					smaller, larger = larger, smaller
				}
				if smaller*5 <= larger*4 {
					continue
				}
				candidateSet[j] = true
			}
		}
		candidates := make([]int, 0, len(candidateSet))
		for j := range candidateSet {
			if err := checkpoint(); err != nil {
				return nil, err
			}
			candidates = append(candidates, j)
		}
		if err := maintainContextHeapSort(ctx, candidates, func(a, b int) bool { return a < b }); err != nil {
			return nil, err
		}
		for _, j := range candidates {
			if err := checkpoint(); err != nil {
				return nil, err
			}
			similar, err := maintainBigramSimilarContext(all[i].grams, all[j].grams, checkpoint)
			if err != nil {
				return nil, err
			}
			if similar {
				fromRef := all[i].nodeID + "#" + all[i].entry.ID
				toRef := all[j].nodeID + "#" + all[j].entry.ID
				debts = append(debts, Debt{
					ID: debtID("xdup:"+fromRef+":"+toRef, all[i].nodeID), Kind: "cross-dup", Node: all[i].nodeID,
					Desc: fromRef + " 与 " + toRef + " 跨节点疑似重复(相似度>0.8)",
					Hint: "读两条内容:若确为重复,用 kb_remember 合并文本 + supersedes,或提炼为 kb_flow 共享约定;非重复则 dismiss",
				})
				if len(debts) == 5 {
					break
				}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return debts, nil
}
