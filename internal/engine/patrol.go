package engine

// patrol.go — 跨节点矛盾巡检(2026-07-05,冲突检测盲区补位)。
//
// 定位:同节点冲突已有三层兜底(写入 bigram 查重逼三选一、disputes 矛盾单、
// dup-entries 债);措辞不同或分居两个节点的语义冲突,机器判不了(语义判断是
// LLM-complete,零重依赖铁律下没有 embedding)。机器能做且只做的:把"最可能
// 谈论同一主题"的活跃知识按关键词簇聚到一张纸上,跨节点并读,让读的 AI 当裁判。
// 纯只读拼简报:不开 job、不加状态——裁决动作(refute/disputes)本身就是留痕。

import (
	"fmt"
	"sort"
	"strings"

	"github.com/zdypro888/iknowledge/internal/model"
)

// patrolCluster 是一个巡检簇:同关键词、跨 ≥2 节点的活跃知识。
type patrolCluster struct {
	keyword string
	nodeIDs []string
	entries int
}

// 简报预算(限额哲学,与 recall 同族):簇数与总条目数都封顶,
// 溢出明说不隐瞒(no silent caps)。
const (
	patrolMaxClusters       = 5
	patrolMaxEntries        = 30
	patrolMaxEntriesPerNode = 3
)

// PatrolBrief 锁外入口(CLI 只读用);MCP 走 Maintain(action=patrol)持锁调用。
func (e *Engine) PatrolBrief(scope string) (string, error) {
	if err := e.requireInit(); err != nil {
		return "", err
	}
	if err := e.Sync(); err != nil {
		return "", err
	}
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	return e.patrolBriefLocked(scope), nil
}

// patrolBriefLocked 拼跨节点巡检简报。聚类=节点级 Keywords(导航词,写入时
// 已小写归一);同一节点集只巡一次(先排序后去重,确定性输出)。
func (e *Engine) patrolBriefLocked(scope string) string {
	byKw := map[string][]string{}
	for id, ref := range e.rt.ix.Nodes() {
		if !debtInScope(id, scope) || !hasActiveEntries(ref.Node) {
			continue
		}
		for _, kw := range ref.Node.Keywords {
			byKw[kw] = append(byKw[kw], id)
		}
	}

	var clusters []patrolCluster
	for kw, ids := range byKw {
		if len(ids) < 2 {
			continue // 单节点没有"跨节点"矛盾可巡
		}
		sort.Strings(ids)
		n := 0
		for _, id := range ids {
			n += countActive(e.rt.ix.Node(id).Node)
		}
		clusters = append(clusters, patrolCluster{keyword: kw, nodeIDs: ids, entries: n})
	}
	if len(clusters) == 0 {
		if scope != "" {
			return "范围 " + scope + " 内无跨节点同关键词簇,无可巡检。"
		}
		return "无跨节点同关键词簇,无可巡检(节点回填 keywords 后簇才成形——检索词回填是纪律段既有义务)。"
	}

	// 排序(条目多者优先,同数按关键词字典序)后按节点集签名去重:
	// 两个关键词圈住同一批节点时只留最大簇,防同一批条目重复巡。
	sort.Slice(clusters, func(i, j int) bool {
		if clusters[i].entries != clusters[j].entries {
			return clusters[i].entries > clusters[j].entries
		}
		return clusters[i].keyword < clusters[j].keyword
	})
	seen := map[string]bool{}
	kept := clusters[:0]
	for _, c := range clusters {
		sig := strings.Join(c.nodeIDs, "\x00")
		if seen[sig] {
			continue
		}
		seen[sig] = true
		kept = append(kept, c)
	}
	clusters = kept

	var b strings.Builder
	b.WriteString("【矛盾巡检简报】跨节点同主题知识并读——机器只聚类不判语义,你是裁判:\n")
	shown, budget := 0, patrolMaxEntries
	for _, c := range clusters {
		if shown >= patrolMaxClusters || budget <= 0 {
			break
		}
		shown++
		fmt.Fprintf(&b, "\n簇「%s」(%d 节点 / %d 条活跃):\n", c.keyword, len(c.nodeIDs), c.entries)
		for _, id := range c.nodeIDs {
			node := e.rt.ix.Node(id).Node
			perNode := 0
			for i := range node.Entries {
				en := &node.Entries[i]
				if !en.Active() {
					continue
				}
				if perNode >= patrolMaxEntriesPerNode || budget <= 0 {
					fmt.Fprintf(&b, "  - %s …(还有条目未列,kb_recall 取全量)\n", id)
					break
				}
				fmt.Fprintf(&b, "  - %s#%s [%s/%s] %s\n", id, en.ID, en.Kind, en.Confidence, firstLine(en.Text))
				perNode++
				budget--
			}
		}
	}
	if dropped := len(clusters) - shown; dropped > 0 {
		fmt.Fprintf(&b, "\n(另有 %d 簇本轮未列——预算上限;处理完再巡一轮)\n", dropped)
	}
	b.WriteString(`
巡检纪律:
1. 逐簇并读条目与其锚定代码原文(知识与原文冲突时以原文为准);
2. 互相矛盾且能自裁 → kb_verify refute 错误方(附原文证据);证据在代码之外 → kb_remember 新结论并 disputes 登记待裁决;
3. 与代码不符的直接 refute;都成立就不动——禁止为巡检刷 confirm(verified 必须有真实验证依据);
4. 簇大时建议把本简报转交子代理执行,保护你的上下文。
(纯只读简报,无需交卷——refute/disputes 本身就是留痕)`)
	return framed(b.String())
}

// countActive 数节点的活跃条目数。
func countActive(n *model.Node) int {
	c := 0
	for i := range n.Entries {
		if n.Entries[i].Active() {
			c++
		}
	}
	return c
}
