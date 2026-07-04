package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/zdypro888/iknowledge/internal/model"
)

// 维护欠账队列(knowledge.md §12.7):服务端只做检测与记账,AI 管语言。
// 定案(全量实现):队列是**现算派生值**,不落盘——欠账由成因(摘要落后/历史超预算/
// 疑似重复)现场推导,成因消除欠账自动消失,不存在队列本身腐烂的问题。

// Debt 是一条维护欠账。
type Debt struct {
	ID   string // 稳定推导:kind+node 的短哈希(同一成因两次 next 拿到同一 ID)
	Kind string // era-compress | summary-stale | dup-entries
	Node string
	Desc string
	Hint string
}

func debtID(kind, node string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + node))
	return "d_" + hex.EncodeToString(sum[:4])
}

// computeDebtsLocked 现算全部欠账(排除已消解的,#11)。前提:已持锁。
func (e *Engine) computeDebtsLocked() []Debt {
	dismissed, _ := e.Store.LoadDismissedDebts()
	var debts []Debt
	for id, ref := range e.rt.ix.Nodes() {
		n := ref.Node
		// ① 历史超预算 → 时代摘要债(§12.3:未折叠 >10 条或 >600 token)。
		if e.eraDebtLocked(id) {
			debts = append(debts, Debt{
				ID: debtID("era", id), Kind: "era-compress", Node: id,
				Desc: "未折叠历史超预算(>10 条或 >600 token)",
				Hint: "读该节点 history 全量,把'近 5 条'之外的记录压缩成时代摘要——负知识(否决项)必须逐条保留在摘要文本里;然后 kb_maintain complete 提交 era_summary",
			})
		}
		// ② 文件摘要落后:file 节点的 summary 早于其下最近变更(§12.7)。
		if n.Level == model.LevelFile && hasActiveEntries(n) {
			var summaryAt = n.Since
			hasSummary := false
			for i := range n.Entries {
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
				for childID := range e.rt.ix.Nodes() {
					f, sym := model.SplitNodeID(childID)
					if f != n.ID || sym == "" {
						continue
					}
					for _, c := range e.rt.ix.History(childID) {
						if c.At.After(summaryAt) {
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
		// ③ 疑似重复条目(bigram>0.8 的活跃对)。
		var actives []*model.Entry
		for i := range n.Entries {
			if n.Entries[i].Active() {
				actives = append(actives, &n.Entries[i])
			}
		}
		for i := 0; i < len(actives); i++ {
			for j := i + 1; j < len(actives); j++ {
				if BigramJaccard(actives[i].Text, actives[j].Text) > 0.8 {
					debts = append(debts, Debt{
						ID: debtID("dup:"+actives[i].ID+":"+actives[j].ID, id), Kind: "dup-entries", Node: id,
						Desc: "条目 " + actives[i].ID + " 与 " + actives[j].ID + " 疑似重复(相似度>0.8)",
						Hint: "读两条内容,kb_remember 一条合并文本并 supersedes 两者(语义判定归你,服务端只报机械信号)",
					})
				}
			}
		}
	}
	if len(dismissed) > 0 {
		kept := debts[:0]
		for _, d := range debts {
			if !dismissed[d.ID] {
				kept = append(kept, d)
			}
		}
		debts = kept
	}
	sort.Slice(debts, func(i, j int) bool { return debts[i].ID < debts[j].ID })
	return debts
}

// MaintainArgs 是 kb_maintain 入参。
type MaintainArgs struct {
	Action     string `json:"action"` // next | complete | dismiss
	ID         string `json:"id,omitempty"`
	Scope      string `json:"scope,omitempty"`       // 路径前缀:只取本任务相关的债
	EraSummary string `json:"era_summary,omitempty"` // era-compress 完成时提交
}

// Maintain 维护欠账:next 取一条最高优先级欠账;complete 销账(era 债携带摘要落库)。
func (e *Engine) Maintain(a MaintainArgs, sid, author string) (string, error) {
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
			if a.Scope != "" && !strings.HasPrefix(d.Node, strings.TrimSuffix(a.Scope, "/")) {
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
	}
	return "", kbErr("INVALID_ARGUMENT", "非法 action "+a.Action, "action ∈ next|complete|dismiss")
}
