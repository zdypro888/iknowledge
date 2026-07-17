package engine

import (
	"fmt"
	"sort"
	"strings"

	"github.com/zdypro888/iknowledge/internal/model"
)

const (
	defaultBriefBudget = 1200
	minBriefBudget     = 300
	maxBriefBudget     = 4000
)

// Brief 生成一个可直接贴进新会话的 Markdown 项目简报。它只聚合现有事实,
// 不生成 ROI、节省金额等无法从仓库验证的指标。
func (e *Engine) Brief(tokenBudget int) (string, error) {
	if err := e.requireInit(); err != nil {
		return "", err
	}
	if err := e.Sync(); err != nil {
		return "", err
	}
	if tokenBudget <= 0 {
		tokenBudget = defaultBriefBudget
	}
	tokenBudget = max(minBriefBudget, min(tokenBudget, maxBriefBudget))

	e.rt.mu.RLock()
	defer e.rt.mu.RUnlock()
	debts := e.computeDebtsLocked()
	changes := append([]model.Change(nil), e.rt.ix.Changes()...)
	inactive := inactiveChanges(changes)

	var b strings.Builder
	b.WriteString("# iknowledge briefing\n\n")
	repo := strings.ReplaceAll(briefOneLine(e.Store.RepoRoot(), 160), "`", "'")
	fmt.Fprintf(&b, "> 仓库: `%s` · 预算: ≤%d estimated tokens · 源码永远优先于知识库。\n", repo, tokenBudget)

	b.WriteString("\n## 当前任务\n")
	if len(e.rt.wips) == 0 {
		b.WriteString("- 无活跃 WIP。\n")
	} else {
		for i, w := range e.rt.wips {
			if i >= 3 {
				fmt.Fprintf(&b, "- 另有 %d 个活跃 WIP。\n", len(e.rt.wips)-i)
				break
			}
			fmt.Fprintf(&b, "- **%s** (%s)", briefOneLine(w.Task, 100), briefOneLine(w.Owner, 80))
			if w.Intent != "" {
				fmt.Fprintf(&b, " — %s", briefOneLine(w.Intent, 100))
			}
			if len(w.Todo) > 0 || len(w.Touching) > 0 {
				fmt.Fprintf(&b, " _(todo %d, touching %d)_", len(w.Todo), len(w.Touching))
			}
			b.WriteByte('\n')
		}
	}

	type scoredNode struct {
		id    string
		score int
	}
	var stateRisks []string
	var landmines []scoredNode
	var disputes []string
	disputeSeen := map[string]bool{}
	ids := make([]string, 0, len(e.rt.ix.Nodes()))
	for id := range e.rt.ix.Nodes() {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		n := e.rt.ix.Node(id).Node
		var states []string
		if n.Status == model.StatusSuspect || n.Status == model.StatusOrphaned {
			states = append(states, string(n.Status))
		}
		if n.PendingAnchor {
			states = append(states, "pending-anchor")
		}
		if len(states) > 0 && len(stateRisks) < 4 {
			stateRisks = append(stateRisks, id+" ("+strings.Join(states, ", ")+")")
		}
		if score := e.rt.ix.LandmineScore(id); score >= 3 {
			landmines = append(landmines, scoredNode{id: id, score: score})
		}
		for i := range n.Entries {
			en := &n.Entries[i]
			if !en.Active() {
				continue
			}
			from := id + "#" + en.ID
			for _, targetRef := range en.Disputes {
				if target := e.rt.ix.EntryByRef(targetRef); target != nil && target.Active() && len(disputes) < 3 {
					addPrecheckDispute(&disputes, disputeSeen, from, e.rt.ix.ResolveEntryRef(targetRef))
				}
			}
		}
	}
	sort.Slice(landmines, func(i, j int) bool {
		if landmines[i].score != landmines[j].score {
			return landmines[i].score > landmines[j].score
		}
		return landmines[i].id < landmines[j].id
	})

	sort.Slice(changes, func(i, j int) bool { return changes[i].At.After(changes[j].At) })
	var rejected []string
	for _, c := range changes {
		if inactive[c.ID] {
			continue
		}
		for _, rj := range c.Rejected {
			if len(rejected) >= 3 {
				break
			}
			rejected = append(rejected, fmt.Sprintf("%s: 「%s」— %s", shortChangeID(c.ID), briefOneLine(rj.Option, 80), briefOneLine(rj.Reason, 90)))
		}
		if len(rejected) >= 3 {
			break
		}
	}

	b.WriteString("\n## 先看风险\n")
	if len(stateRisks)+len(landmines)+len(disputes)+len(rejected) == 0 {
		b.WriteString("- 未发现 suspect/orphan/pending、雷区、未决矛盾或生效中的否决项。\n")
	}
	for _, risk := range stateRisks {
		fmt.Fprintf(&b, "- **状态债:** `%s`\n", briefCode(risk, 220))
	}
	for i, lm := range landmines {
		if i >= 3 {
			break
		}
		fmt.Fprintf(&b, "- **雷区:** `%s`，分数 %d（反复改动/推翻/勘误信号）。\n", briefCode(lm.id, 180), lm.score)
	}
	for _, dispute := range disputes {
		fmt.Fprintf(&b, "- **待裁决:** `%s`\n", briefCode(dispute, 240))
	}
	for _, item := range rejected {
		fmt.Fprintf(&b, "- **否决过:** %s\n", item)
	}

	b.WriteString("\n## 最近决策\n")
	shown := 0
	for _, c := range changes {
		if inactive[c.ID] || shown >= 3 {
			continue
		}
		fmt.Fprintf(&b, "- `%s` %s — %s", briefCode(shortChangeID(c.ID), 80), briefOneLine(c.What, 110), briefOneLine(c.Why, 120))
		if c.Verified != "" {
			fmt.Fprintf(&b, " _(验证: %s)_", briefOneLine(c.Verified, 80))
		}
		b.WriteByte('\n')
		shown++
	}
	if shown == 0 {
		b.WriteString("- 暂无变更记录。\n")
	}

	b.WriteString("\n## 维护面\n")
	if len(debts) == 0 {
		b.WriteString("- 无维护欠账。\n")
	} else {
		byKind := map[string]int{}
		for _, debt := range debts {
			byKind[debt.Kind]++
		}
		var kinds []string
		for kind := range byKind {
			kinds = append(kinds, kind)
		}
		sort.Strings(kinds)
		var counts []string
		for _, kind := range kinds {
			counts = append(counts, fmt.Sprintf("%s=%d", kind, byKind[kind]))
		}
		fmt.Fprintf(&b, "- 共 %d 条: %s。\n", len(debts), strings.Join(counts, ", "))
		for i, debt := range debts {
			if i >= 3 {
				break
			}
			fmt.Fprintf(&b, "- 下一项 `%s` [%s] `%s`: %s\n", briefCode(debt.ID, 100), briefOneLine(debt.Kind, 80), briefCode(debt.Node, 180), briefOneLine(debt.Desc, 120))
		}
	}
	b.WriteString("\n> 开工顺序: 先读相关源码与 `kb_recall`，处理风险，再改码；每个逻辑改动以 `kb_record_change` 收尾。\n")

	clean, redaction := RedactText(b.String())
	if redaction.Count > 0 {
		clean += fmt.Sprintf("\n> 安全: 简报输出已脱敏 %d 处历史秘密。\n", redaction.Count)
	}
	return truncateBrief(clean, tokenBudget), nil
}

func briefOneLine(s string, limit int) string {
	return shortText(strings.Join(strings.Fields(s), " "), limit)
}

func briefCode(s string, limit int) string {
	return strings.ReplaceAll(briefOneLine(s, limit), "`", "'")
}

func truncateBrief(s string, budget int) string {
	// brief 会被直接贴进新 AI 会话,必须复用全项目的数据框防投毒契约。
	// 截断时头尾均保留;正文若伪造框架标记,与 framed() 同样先消毒。
	body := sanitizeFrameBody(strings.TrimRight(s, "\n"))
	full := frameHeader + body + frameFooter
	if EstimateTokens(full) <= budget {
		return full
	}
	var kept []string
	used := 0
	tail := "\n> ……简报已按预算截断；用更大的 `--budget` 查看后续。" + frameFooter
	reserve := EstimateTokens(frameHeader) + EstimateTokens(tail) + 2
	for _, line := range strings.Split(body, "\n") {
		cost := EstimateTokens(line) + 1
		if used+cost > budget-reserve {
			break
		}
		kept = append(kept, line)
		used += cost
	}
	return frameHeader + strings.Join(kept, "\n") + tail
}
