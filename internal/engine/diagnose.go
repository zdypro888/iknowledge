package engine

import (
	"fmt"
	"sort"
	"strings"

	"github.com/zdypro888/iknowledge/internal/model"
)

// 轮30-B:kb_diagnose(症状→位置反向定位)。
//
// 与 kb_recall(位置→内容)相反:AI 输入症状/报错("支付回调偶发超时"),系统返回
// 最可能的位置 + 相关 pitfall + 排障流程 + 历史否决方案。让 AI 不必靠猜文件名 + grep 定位问题。
//
// 复用已有数据(零新存储):
//   - ix.Search 已索引 pitfall entry 文本(index.go Build tokenizes all active entries),
//     症状查询天然命中描述问题的 pitfall;
//   - keywordOverlap(investigate.go)匹配 Flow.Troubleshoot 字段;
//   - ix.History 取命中节点的 rejected 方案(动手前必读的负知识)。
type DiagnoseArgs struct {
	Symptom string `json:"symptom"`           // 症状/报错描述(必填)
	Limit   int    `json:"limit,omitempty"`   // 返回位置上限(缺省 8)
}

// Diagnose 按症状反向定位代码位置 + 附 pitfall/排障/否决方案上下文。
func (e *Engine) Diagnose(a DiagnoseArgs, sid string) (string, ReadMeta, error) {
	if err := e.requireInit(); err != nil {
		return "", ReadMeta{}, err
	}
	if strings.TrimSpace(a.Symptom) == "" {
		return "", ReadMeta{}, kbErr("INVALID_PARAMS", "缺 symptom(症状/报错描述)", "描述你遇到的问题现象,如'支付回调偶发超时'")
	}
	if err := e.Sync(); err != nil {
		return "", ReadMeta{}, err
	}
	e.rt.mu.RLock()
	defer e.rt.mu.RUnlock()

	if a.Limit <= 0 {
		a.Limit = 8
	}
	var b strings.Builder
	fmt.Fprintf(&b, "诊断:%s\n", strings.TrimSpace(a.Symptom))

	// ① 症状 → pitfall/entry 命中(ix.Search 已索引所有活跃条目文本,含 pitfall)。
	hits := e.rt.ix.Search(a.Symptom, a.Limit)
	type located struct {
		nodeID   string
		score    int
		pitfalls []string // 命中的 pitfall 文本
		other    int      // 命中的非 pitfall 条目数
	}
	var locs []located
	for _, h := range hits {
		ref := e.rt.ix.Node(h.NodeID)
		if ref == nil || !hasActiveEntries(ref.Node) {
			continue
		}
		// 放松 fresh 限制:diagnose 该看 suspect 的地雷(libraryFindingsLocked 只取 fresh,
		// 这里也收 suspect——问题常潜伏在被标 suspect 的区域)。
		if ref.Node.Status == model.StatusOrphaned {
			continue
		}
		l := located{nodeID: h.NodeID, score: h.Score}
		for i := range ref.Node.Entries {
			en := &ref.Node.Entries[i]
			if !en.Active() {
				continue
			}
			// pitfall 优先:症状最可能匹配描述问题的 pitfall。
			if en.Kind == model.KindPitfall {
				l.pitfalls = append(l.pitfalls, en.Text)
			} else {
				l.other++
			}
		}
		// 只要有 pitfall 或足够相关(other>0 且 score 高)就收。
		if len(l.pitfalls) > 0 || l.other > 0 {
			locs = append(locs, l)
		}
	}
	// pitfall 命中优先排序,然后按 score。
	sort.Slice(locs, func(i, j int) bool {
		if len(locs[i].pitfalls) != len(locs[j].pitfalls) {
			return len(locs[i].pitfalls) > len(locs[j].pitfalls)
		}
		return locs[i].score > locs[j].score
	})

	if len(locs) == 0 {
		b.WriteString("库内无匹配症状的知识。\n")
		b.WriteString("—— 若你已知大致区域,kb_recall 查具体节点;若完全无头绪,kb_investigate 派侦察兵深挖。\n")
		return strings.TrimRight(b.String(), "\n"), ReadMeta{Hit: false}, nil
	}

	b.WriteString("最可能位置(按症状相关度,pitfall 优先):\n")
	// 收集所有命中节点,后面用于 rejected 上下文。
	hitNodeIDs := make([]string, 0, len(locs))
	for i, l := range locs {
		hitNodeIDs = append(hitNodeIDs, l.nodeID)
		fmt.Fprintf(&b, "  %d. %s", i+1, l.nodeID)
		if ref := e.rt.ix.Node(l.nodeID); ref != nil {
			fmt.Fprintf(&b, " [%s]", ref.Node.Status)
		}
		for _, p := range l.pitfalls {
			fmt.Fprintf(&b, "\n     ⚠ pitfall: %s", shortText(p, 80))
		}
		b.WriteString("\n")
	}

	// ② 相关排障流程(Flow.Troubleshoot 关键词匹配)。
	var flows []string
	for i := range e.rt.flows {
		f := &e.rt.flows[i]
		if f.Deprecated {
			continue
		}
		text := f.Title + " " + f.Troubleshoot + " " + strings.Join(f.Conventions, " ")
		if keywordOverlap(a.Symptom, text) {
			flows = append(flows, f.ID)
		}
	}
	if len(flows) > 0 {
		fmt.Fprintf(&b, "相关排障流程: %s(recall mode=flow 看完整流程)\n", strings.Join(flows, "、"))
	}

	// ③ 历史否决方案(动手前必读的负知识):命中节点的 history 里的 rejected。
	rejectedShown := 0
	for _, nid := range hitNodeIDs {
		if rejectedShown >= 5 {
			break
		}
		for _, c := range e.rt.ix.History(nid) {
			for _, rj := range c.Rejected {
				fmt.Fprintf(&b, "  ✗ 曾否决「%s」(理由:%s)—— 动手前确认你的方案不是它的翻版\n",
					shortText(rj.Option, 50), shortText(rj.Reason, 50))
				rejectedShown++
				if rejectedShown >= 5 {
					break
				}
			}
			if rejectedShown >= 5 {
				break
			}
		}
	}
	b.WriteString("—— 确认位置后 kb_recall <node-id> 取该节点全量知识; symptom 不够准时换个描述重试。\n")
	return strings.TrimRight(b.String(), "\n"), ReadMeta{Hit: true}, nil
}
