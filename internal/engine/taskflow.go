package engine

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zdypro888/iknowledge/internal/model"
)

// ---- kb_task(impl §7.3;knowledge.md §7 任务态层) ----

// TaskArgs 是 kb_task 入参。
type TaskArgs struct {
	Action string    `json:"action"` // start | update | complete | get
	WIP    model.WIP `json:"wip"`
}

// Task 任务态读写:与知识层严格分离,归档时压缩成变更记录。
func (e *Engine) Task(a TaskArgs, sid, author string) (string, error) {
	if err := e.requireInit(); err != nil {
		return "", err
	}
	if err := e.Sync(); err != nil {
		return "", err
	}
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	owner := taskOwner(sid, author)

	switch a.Action {
	case "start":
		if strings.TrimSpace(a.WIP.Task) == "" {
			return "", kbErr("INVALID_ARGUMENT", "start 需要 wip.task", "给任务一句话描述")
		}
		w := a.WIP
		w.Owner = owner // owner = 会话唯一键(服务端定,防冒名/同客户端多会话覆盖)
		w.Updated = e.now().UTC()
		if err := e.Store.SaveWIP(w); err != nil {
			return "", err
		}
		if err := e.reloadLocked(); err != nil {
			return "", err
		}
		return "wip 已建立(owner " + owner + ")。touching 声明的节点在他人 recall/map 时会自动附带此台账。", nil

	case "update":
		cur := e.wipForSessionLocked(owner, author)
		if cur == nil {
			return "", kbErr("NODE_NOT_FOUND", "没有你的活跃 wip", "先 kb_task start")
		}
		var sessionBefore *model.WIP
		if existing := e.wipOfLocked(owner); existing != nil {
			copy := *existing
			sessionBefore = &copy
		}
		merged := mergeWIP(*cur, a.WIP)
		merged.Owner = owner
		merged.Updated = e.now().UTC()
		if err := e.Store.SaveWIP(merged); err != nil {
			reloadErr := e.reloadLocked()
			return "", errors.Join(err, reloadErr)
		}
		// 升级前按 author 命名的 WIP 迁到 session 唯一键。即使上一次迁移
		// 半途留下了 session+legacy 两份，也在本次成功更新时收敛。
		if legacy := e.wipOfLocked(author); author != owner && legacy != nil {
			legacyCopy := *legacy
			if err := e.Store.ClearWIP(author); err != nil {
				legacyRestoreErr := e.Store.SaveWIP(legacyCopy)
				var sessionRollbackErr error
				if sessionBefore != nil {
					sessionRollbackErr = e.Store.SaveWIP(*sessionBefore)
				} else {
					sessionRollbackErr = e.Store.ClearWIP(owner)
				}
				reloadErr := e.reloadLocked()
				return "", errors.Join(fmt.Errorf("迁移 legacy WIP: %w", err), legacyRestoreErr, sessionRollbackErr, reloadErr)
			}
		}
		if err := e.reloadLocked(); err != nil {
			return "", err
		}
		return "wip 已更新", nil

	case "complete":
		cur := e.wipForSessionLocked(owner, author)
		if cur == nil {
			return "", kbErr("NODE_NOT_FOUND", "没有你的活跃 wip", "先 kb_task start")
		}
		// 归档为变更记录(§7 生命周期第 3 条:半成品状态绝不留在知识层)。
		var nodes []string
		for _, t := range cur.Touching {
			if id := e.rt.ix.ResolveNodeID(t); id != "" {
				nodes = append(nodes, id)
			}
		}
		if len(nodes) == 0 {
			nodes = []string{model.ProjectNodeID}
		}
		chID := e.freshChangeIDLocked()
		what := "完成任务:" + cur.Task
		if len(cur.Done) > 0 {
			what += "(" + strings.Join(cur.Done, ";") + ")"
		}
		why := cur.Intent
		if why == "" {
			why = "任务态归档(kb_task complete)"
		}
		change := model.Change{
			ID: chID, Nodes: nodes, At: e.now().UTC(),
			Task: cur.Task, What: what, Why: why, Author: author, EffectsVersion: 1,
		}
		// WIP 删除与 journal 追加是一个崩溃事务；prepared intent 在 ClearWIP
		// 前同时收录两者，进程在任一写后退出都会于下次 reload 全量恢复。
		tx, err := e.prepareTruthTransactionLocked(map[string]bool{
			e.Store.WIPRelFor(cur.Owner):  true,
			e.Store.JournalRelFor(change): true,
		})
		if err != nil {
			return "", err
		}
		defer e.guardTruthTransactionPanicLocked(tx)
		rollback := func(cause error) (string, error) {
			return "", e.rollbackTruthTransactionLocked(tx, cause)
		}
		if err := e.Store.ClearWIP(cur.Owner); err != nil {
			return rollback(fmt.Errorf("task complete 清除 WIP: %w", err))
		}
		if err := e.Store.AppendChange(change); err != nil {
			committed := false
			if changes, _, loadErr := e.Store.LoadJournal(); loadErr == nil {
				for _, c := range changes {
					if c.ID == change.ID {
						committed = true
						break
					}
				}
			}
			if !committed {
				return rollback(fmt.Errorf("task complete 追加 journal: %w", err))
			}
		}
		committed, commitErr := e.commitTruthTransactionLocked(tx)
		if !committed {
			return rollback(fmt.Errorf("task complete 写 committed marker: %w", commitErr))
		}
		if commitErr != nil {
			return "", fmt.Errorf("task complete 已提交但 WAL 清理/重载失败(不要重试): %w", commitErr)
		}
		out := "已归档为变更记录 " + chID + ",wip 清空。"
		// 任务尾偿还(§12.2 第 3 条,≤3 条,仅本任务读过的)+ 沉淀提醒(§9.3)。
		if debts := e.sessionSuspectsLocked(sid); len(debts) > 0 {
			out += "\n任务尾偿还(≤3 条,本任务读过且仍待重验):" + strings.Join(debts, "、") + "(kb_verify)"
		}
		if remind := e.settleReminder(sid); len(remind) > 0 {
			out += "\n沉淀提醒:多次读取未沉淀的节点:" + strings.Join(remind, "、") + "(kb_remember)"
		}
		if md := e.computeDebtsLocked(); len(md) > 0 {
			n := min(2, len(md))
			out += fmt.Sprintf("\n顺手维护(≤2 条,§12.7):%d 条欠账,kb_maintain next 取用", n)
		}
		return out, nil

	case "get":
		if len(e.rt.wips) == 0 {
			return "无活跃 wip。", nil
		}
		var b strings.Builder
		for _, w := range e.rt.wips {
			fmt.Fprintf(&b, "[%s] %s(更新 %s)\n  intent: %s\n  plan: %v\n  done: %v\n  todo: %v\n  touching: %v\n",
				w.Owner, w.Task, w.Updated.Format("01-02 15:04"), w.Intent, w.Plan, w.Done, w.Todo, w.Touching)
		}
		return strings.TrimRight(b.String(), "\n"), nil
	}
	return "", kbErr("INVALID_ARGUMENT", "非法 action "+a.Action, "action ∈ start|update|complete|get")
}

func (e *Engine) wipOfLocked(owner string) *model.WIP {
	for i := range e.rt.wips {
		if e.rt.wips[i].Owner == owner {
			return &e.rt.wips[i]
		}
	}
	return nil
}

// taskOwner 用 MCP session 区分同一客户端的并行任务。clientInfo.name 只表示
// "codex"/"claude-code",不是会话 ID;直接当 owner 会让后一个 start 覆盖前一个。
// 匿名/旧客户端没有 sid 时退化为 author。author 仍保留在前缀,展示可溯源。
func taskOwner(sid, author string) string {
	if strings.TrimSpace(sid) == "" {
		return author
	}
	return author + "@" + sid
}

// wipForSessionLocked 优先取会话唯一台账;legacyOwner 兼容升级前按 author
// 命名的本地 WIP,首次 update 后会迁到新键。
func (e *Engine) wipForSessionLocked(owner, legacyOwner string) *model.WIP {
	if w := e.wipOfLocked(owner); w != nil {
		return w
	}
	if legacyOwner != owner {
		return e.wipOfLocked(legacyOwner)
	}
	return nil
}

// mergeWIP 提供了字段就替换(update 语义)。
func mergeWIP(cur, in model.WIP) model.WIP {
	if in.Task != "" {
		cur.Task = in.Task
	}
	if in.Intent != "" {
		cur.Intent = in.Intent
	}
	if in.Plan != nil {
		cur.Plan = in.Plan
	}
	if in.Done != nil {
		cur.Done = in.Done
	}
	if in.Todo != nil {
		cur.Todo = in.Todo
	}
	if in.Touching != nil {
		cur.Touching = in.Touching
	}
	return cur
}

// sessionSuspectsLocked 本会话读取过且仍有待重验条目/状态的节点(≤3)。
func (e *Engine) sessionSuspectsLocked(sid string) []string {
	reads := e.ledgerSnapshot(sid)
	if reads == nil {
		return nil
	}
	var out []string
	for id := range reads {
		ref := e.rt.ix.Node(id)
		if ref == nil {
			continue
		}
		bad := ref.Node.Status == model.StatusSuspect
		for i := range ref.Node.Entries {
			if ref.Node.Entries[i].Active() && ref.Node.Entries[i].Confidence == model.ConfidenceSuspect {
				bad = true
			}
		}
		if bad {
			out = append(out, id)
			if len(out) >= 3 {
				break
			}
		}
	}
	return out
}

// ---- kb_flow(impl §7.3;knowledge.md §6 横向维度) ----

// FlowArgs 是 kb_flow 入参。
type FlowArgs struct {
	Action string     `json:"action"` // get | create | update | deprecate
	Flow   model.Flow `json:"flow"`
}

// Flow 流程/主题节点 CRUD;steps 引用的树节点必须存在,反向链接由 index 现算。
func (e *Engine) Flow(a FlowArgs, sid, author string) (string, error) {
	if err := e.requireInit(); err != nil {
		return "", err
	}
	if err := e.Sync(); err != nil {
		return "", err
	}
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()

	// get 先处理(#5:原先无读动作,update 只能盲写)——ID 空则列全部,否则取详情。
	if a.Action == "get" {
		if strings.TrimSpace(a.Flow.ID) == "" {
			return framed(e.listFlowsLocked()), nil
		}
		if out, ok := e.flowView(a.Flow.ID); ok {
			return framed(out), nil
		}
		return "", kbErr("NODE_NOT_FOUND", a.Flow.ID+" 不存在", "kb_flow get(空 id)列全部")
	}

	if a.Flow.ID == "" || e.Store.FlowPathFor(a.Flow.ID) == "" {
		return "", kbErr("INVALID_ARGUMENT", "非法 flow ID "+a.Flow.ID,
			"格式 flow:name 或 topic:name(name 不含斜杠)")
	}
	existing := e.rt.ix.Flow(a.Flow.ID)

	switch a.Action {
	case "create", "update":
		if a.Action == "create" && existing != nil {
			return "", kbErr("DUPLICATE_ENTRY", a.Flow.ID+" 已存在", "用 update")
		}
		if a.Action == "update" && existing == nil {
			return "", kbErr("NODE_NOT_FOUND", a.Flow.ID+" 不存在", "用 create")
		}
		if a.Flow.Title == "" {
			return "", kbErr("INVALID_ARGUMENT", "flow.title 必填", "给流程一个标题")
		}
		// steps 引用的树节点必须存在(impl §7.3 kb_flow)。
		for _, st := range a.Flow.Steps {
			if e.rt.ix.ResolveNodeID(st.Node) == "" {
				return "", kbErr("NODE_NOT_FOUND", "step 引用的节点 "+st.Node+" 不存在",
					"用 kb_map 核对;节点 ID 相对仓库根")
			}
		}
		// lint 与预算同样适用于流程文本(横向层不是投毒逃逸口)。
		// steps[].note 同受检(R2-E2:它与 title/conventions 一样渲染进 recall/inject)。
		texts := append([]string{a.Flow.Title, a.Flow.Troubleshoot}, a.Flow.Conventions...)
		for _, st := range a.Flow.Steps {
			texts = append(texts, st.Note)
		}
		for _, text := range texts {
			if reject, _ := LintImperative(text); reject != "" {
				return "", kbErr("IMPERATIVE_CONTENT", reject, "改写为事实陈述(knowledge.md §12.8)")
			}
		}
		f := a.Flow
		f.Author = author
		if existing != nil {
			f.Since = existing.Since
			f.Deprecated = existing.Deprecated
			stampFlowStepGenerations(&f, existing, e.now().UTC())
		} else {
			f.Since = e.now().UTC()
			stampFlowStepGenerations(&f, nil, f.Since)
		}
		if err := e.Store.SaveFlow(f); err != nil {
			return "", err
		}
		if err := e.reloadLocked(); err != nil {
			return "", err
		}
		return a.Flow.ID + " 已保存(" + fmt.Sprint(len(a.Flow.Steps)) + " 步;树节点的反向链接由索引现算)", nil

	case "deprecate":
		if existing == nil {
			return "", kbErr("NODE_NOT_FOUND", a.Flow.ID+" 不存在", "kb_recall mode=flow 核对")
		}
		f := *existing
		f.Deprecated = true
		if err := e.Store.SaveFlow(f); err != nil {
			return "", err
		}
		if err := e.reloadLocked(); err != nil {
			return "", err
		}
		return a.Flow.ID + " 已废弃(文件保留可溯,退出注入与反向链接)", nil
	}
	return "", kbErr("INVALID_ARGUMENT", "非法 action "+a.Action, "action ∈ get|create|update|deprecate")
}

func stampFlowStepGenerations(next *model.Flow, previous *model.Flow, now time.Time) {
	byNode := map[string][]time.Time{}
	if previous != nil {
		for _, step := range previous.Steps {
			since := step.Since
			if since.IsZero() {
				since = previous.Since
			}
			byNode[step.Node] = append(byNode[step.Node], since)
		}
	}
	for i := range next.Steps {
		queue := byNode[next.Steps[i].Node]
		if len(queue) > 0 {
			next.Steps[i].Since = queue[0]
			byNode[next.Steps[i].Node] = queue[1:]
		} else {
			next.Steps[i].Since = now
		}
	}
}
