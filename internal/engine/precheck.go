package engine

import (
	"fmt"
	"sort"
	"strings"

	"github.com/zdypro888/iknowledge/internal/model"
	"github.com/zdypro888/iknowledge/internal/parser"
)

// PrecheckWarning 是源码进入提交前需要人或 AI 处理的一项事实信号。
// Severity=block 表示 strict 模式应阻止提交;默认模式只告警,避免突然改变现有工作流。
type PrecheckWarning struct {
	File     string   `json:"file,omitempty"`
	Severity string   `json:"severity"`
	Kind     string   `json:"kind"`
	Message  string   `json:"message"`
	Details  []string `json:"details,omitempty"`
}

type PrecheckReport struct {
	Files    []string          `json:"files"`
	Warnings []PrecheckWarning `json:"warnings"`
}

func (r PrecheckReport) Blocking() int {
	n := 0
	for _, w := range r.Warnings {
		if w.Severity == "block" {
			n++
		}
	}
	return n
}

func (r PrecheckReport) Text() string {
	blocks, warns := r.Blocking(), 0
	for _, w := range r.Warnings {
		if w.Severity == "warn" {
			warns++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "precheck: 源码 %d 个 | 阻断项 %d | 提醒 %d", len(r.Files), blocks, warns)
	if len(r.Warnings) == 0 {
		b.WriteString("\n✓ 未发现已知风险。")
		return framed(b.String())
	}
	for _, w := range r.Warnings {
		label := "WARN"
		if w.Severity == "block" {
			label = "BLOCK"
		}
		where := w.File
		if where == "" {
			where = "(本次提交)"
		}
		fmt.Fprintf(&b, "\n[%s] %s [%s] %s", label, where, w.Kind, w.Message)
		for _, detail := range w.Details {
			fmt.Fprintf(&b, "\n  - %s", detail)
		}
	}
	b.WriteString("\n默认仅告警;CI/团队门禁可加 --strict。")
	return framed(b.String())
}

// Precheck 把待提交源码映射到知识节点,在提交前主动呈现已知否决方案、腐烂状态、
// 未决矛盾和雷区。accountedNodes 只包含本次 Git 变更中新追加 journal 记录的 nodes;
// deletedFiles 来自同一 Git 视图。节点必须能经当前索引反查到对应源码文件,无关
// journal 不能替别的源码改动冒充已记账。
func (e *Engine) Precheck(files, accountedNodes, deletedFiles []string) (PrecheckReport, error) {
	var report PrecheckReport
	if err := e.requireInit(); err != nil {
		return report, err
	}
	if err := e.Sync(); err != nil {
		return report, err
	}
	cfg := e.cachedConfig()
	if err := e.configError(); err != nil {
		return report, err
	}
	e.rt.mu.RLock()
	defer e.rt.mu.RUnlock()
	accountedFiles := e.precheckAccountedFilesLocked(accountedNodes)
	deleted := map[string]bool{}
	for _, input := range deletedFiles {
		if rel, ok := model.SafeRel(strings.TrimSpace(model.ToSlash(input))); ok {
			deleted[rel] = true
		}
	}

	seenFile := map[string]bool{}
	for _, input := range files {
		rel, ok := model.SafeRel(strings.TrimSpace(model.ToSlash(input)))
		if !ok || seenFile[rel] || parser.ExcludedPath(rel) || e.Reg.ForFile(rel) == nil || !cfgAllows(cfg, rel) {
			continue
		}
		seenFile[rel] = true
		report.Files = append(report.Files, rel)
		nodeIDs := append([]string(nil), e.rt.ix.FileNodes(rel)...)
		sort.Strings(nodeIDs)
		if len(nodeIDs) == 0 {
			report.Warnings = append(report.Warnings, PrecheckWarning{
				File: rel, Severity: "block", Kind: "unindexed-source",
				Message: "源码尚无知识节点;先运行 iknowledge init --repo . 对账骨架。",
			})
			continue
		}
		if !accountedFiles[rel] {
			report.Warnings = append(report.Warnings, PrecheckWarning{
				File: rel, Severity: "block", Kind: "unaccounted-change",
				Message: "本次新增 journal 记录未覆盖该源码;完成 kb_record_change 后把对应记录一并纳入提交。",
			})
		}
		e.precheckFileLocked(&report, rel, nodeIDs, deleted[rel])
	}
	sort.Strings(report.Files)
	sort.SliceStable(report.Warnings, func(i, j int) bool {
		a, b := report.Warnings[i], report.Warnings[j]
		if a.Severity != b.Severity {
			return a.Severity == "block"
		}
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Kind < b.Kind
	})
	return report, nil
}

// precheckAccountedFilesLocked 把变更记录声明的节点还原成真实源码文件。
// 只认 Anchor.File 精确命中:project/dir 级泛化记录不能一条覆盖整仓,否则 strict
// 门禁会被一个笼统节点永久绕过。前提:已持 rt.mu 读锁或写锁。
func (e *Engine) precheckAccountedFilesLocked(nodes []string) map[string]bool {
	out := map[string]bool{}
	for _, id := range nodes {
		ref := e.rt.ix.Node(strings.TrimSpace(id))
		if ref == nil {
			continue
		}
		file, ok := model.SafeRel(model.ToSlash(ref.Node.Anchor.File))
		if !ok || parser.ExcludedPath(file) || e.Reg.ForFile(file) == nil {
			continue
		}
		out[file] = true
	}
	return out
}

func (e *Engine) precheckFileLocked(report *PrecheckReport, file string, nodeIDs []string, deleted bool) {
	var stale, deletedOrphans, landmines, pitfalls, disputes, rejected []string
	disputeSeen := map[string]bool{}
	history := map[string]model.Change{}
	for _, id := range nodeIDs {
		ref := e.rt.ix.Node(id)
		if ref == nil {
			continue
		}
		n := ref.Node
		state := ""
		switch n.Status {
		case model.StatusSuspect:
			state = "suspect(锚与源码失配,知识待重验)"
		case model.StatusOrphaned:
			if deleted {
				deletedOrphans = append(deletedOrphans, id+": orphaned(源码已删除,后续认领或送葬知识)")
			} else {
				state = "orphaned(符号已消失,待认领/送葬)"
			}
		}
		if n.PendingAnchor {
			if state != "" {
				state += ", "
			}
			state += "pending-anchor(待补锚)"
		}
		if state != "" {
			stale = append(stale, id+": "+state)
		}
		if score := e.rt.ix.LandmineScore(id); score >= 3 {
			landmines = append(landmines, fmt.Sprintf("%s: 雷区分 %d", id, score))
		}
		for i := range n.Entries {
			en := &n.Entries[i]
			if !en.Active() {
				continue
			}
			entryRef := id + "#" + en.ID
			if en.Kind == model.KindPitfall && len(pitfalls) < 3 {
				pitfalls = append(pitfalls, id+": "+precheckKnowledgeText(en.Text, 100))
			}
			for _, targetRef := range en.Disputes {
				if target := e.rt.ix.EntryByRef(targetRef); target != nil && target.Active() {
					addPrecheckDispute(&disputes, disputeSeen, entryRef, e.rt.ix.ResolveEntryRef(targetRef))
				}
			}
			for _, sourceRef := range e.rt.ix.DisputedBy(entryRef) {
				if source := e.rt.ix.EntryByRef(sourceRef); source != nil && source.Active() {
					addPrecheckDispute(&disputes, disputeSeen, sourceRef, entryRef)
				}
			}
		}
		for _, c := range e.rt.ix.History(id) {
			history[c.ID] = c
		}
	}

	// 追加账本的撤销/推翻语义按时间重放,只呈现当前仍生效决策里的负知识。
	inactive := inactiveChanges(e.rt.ix.Changes())
	var changes []model.Change
	for _, c := range history {
		changes = append(changes, c)
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].At.After(changes[j].At) })
	for _, c := range changes {
		if inactive[c.ID] {
			continue
		}
		for _, rj := range c.Rejected {
			if len(rejected) >= 5 {
				break
			}
			rejected = append(rejected, fmt.Sprintf("%s: 否决「%s」—— %s", shortChangeID(c.ID), precheckKnowledgeText(rj.Option, 90), precheckKnowledgeText(rj.Reason, 90)))
		}
	}

	if len(stale) > 0 {
		report.Warnings = append(report.Warnings, PrecheckWarning{File: file, Severity: "block", Kind: "stale-knowledge", Message: "触及节点存在不可直接信任的状态。", Details: stale})
	}
	if deleted {
		report.Warnings = append(report.Warnings, PrecheckWarning{File: file, Severity: "warn", Kind: "deleted-source", Message: "源码已删除;记账后仍需用 kb_adopt claim/bury 处置遗留知识节点。", Details: deletedOrphans})
	}
	if len(disputes) > 0 {
		report.Warnings = append(report.Warnings, PrecheckWarning{File: file, Severity: "block", Kind: "open-dispute", Message: "触及节点仍有知识矛盾待裁决。", Details: disputes})
	}
	if len(rejected) > 0 {
		// 历史否决是必须阅读的上下文,但其存在本身无法被本次提交“修掉”。若列为
		// block,任何带负知识的文件都会永久过不了 --strict,所以这里只 warn。
		report.Warnings = append(report.Warnings, PrecheckWarning{File: file, Severity: "warn", Kind: "rejected-alternative", Message: "历史记录包含仍生效的否决方案;确认本次没有重走旧路。", Details: rejected})
	}
	if len(landmines) > 0 {
		report.Warnings = append(report.Warnings, PrecheckWarning{File: file, Severity: "warn", Kind: "landmine", Message: "该区域反复改动/推翻/勘误,提交前应复核影响面。", Details: landmines})
	}
	if len(pitfalls) > 0 {
		report.Warnings = append(report.Warnings, PrecheckWarning{File: file, Severity: "warn", Kind: "pitfall", Message: "提交前复核这些活跃坑点。", Details: pitfalls})
	}
}

func addPrecheckDispute(out *[]string, seen map[string]bool, a, b string) {
	if b < a {
		a, b = b, a
	}
	key := a + "\x00" + b
	if seen[key] {
		return
	}
	seen[key] = true
	*out = append(*out, a+" ↔ "+b)
}

func inactiveChanges(changes []model.Change) map[string]bool {
	byID := map[string]model.Change{}
	inactive := map[string]bool{}
	for _, c := range changes {
		byID[c.ID] = c
	}
	for _, c := range changes {
		if c.Overturns != "" {
			inactive[c.Overturns] = true
		}
		if c.Reverts != "" {
			inactive[c.Reverts] = true
			if target, ok := byID[c.Reverts]; ok && target.Overturns != "" {
				delete(inactive, target.Overturns) // 撤销一次推翻会恢复被它推翻的决策。
			}
		}
	}
	return inactive
}

func shortChangeID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}

func precheckKnowledgeText(s string, limit int) string {
	clean, _ := RedactText(s)
	return shortText(strings.Join(strings.Fields(clean), " "), limit)
}
