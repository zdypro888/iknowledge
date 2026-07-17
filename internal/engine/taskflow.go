package engine

import (
	"fmt"
	"strings"

	"github.com/zdypro888/iknowledge/internal/model"
)

// ---- kb_task(impl §7.3;knowledge.md §7 任务态层) ----

// TaskArgs 是 kb_task 入参。
type TaskArgs struct {
	Action string    `json:"action"` // start | update | complete | get
	WIP    model.WIP `json:"wip"`
}

// Task 任务态读写:与知识层严格分离,归档时压缩成变更记录。
func (e *Engine) Task(a TaskArgs, sid, author string) (out string, err error) {
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

	switch a.Action {
	case "start":
		if strings.TrimSpace(a.WIP.Task) == "" {
			return "", kbErr("INVALID_ARGUMENT", "start 需要 wip.task", "给任务一句话描述")
		}
		w := a.WIP
		w.Owner = author // owner = 会话方(服务端定,防冒名)
		w.Updated = e.now().UTC()
		if err := e.Store.SaveWIP(w); err != nil {
			return "", err
		}
		if err := e.reloadLocked(); err != nil {
			return "", err
		}
		return "wip 已建立(owner " + author + ")。touching 声明的节点在他人 recall/map 时会自动附带此台账。", nil

	case "update":
		cur := e.wipOfLocked(author)
		if cur == nil {
			return "", kbErr("NODE_NOT_FOUND", "没有你的活跃 wip", "先 kb_task start")
		}
		merged := mergeWIP(*cur, a.WIP)
		merged.Owner = author
		merged.Updated = e.now().UTC()
		if err := e.Store.SaveWIP(merged); err != nil {
			return "", err
		}
		if err := e.reloadLocked(); err != nil {
			return "", err
		}
		return "wip 已更新", nil

	case "complete":
		cur := e.wipOfLocked(author)
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
			Task: cur.Task, What: what, Why: why, Author: author,
		}
		if err := e.Store.AppendChange(change); err != nil {
			return "", err
		}
		if err := e.Store.ClearWIP(author); err != nil {
			return "", err
		}
		if err := e.reloadLocked(); err != nil {
			return "", err
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
	l := e.ledger(sid)
	if l == nil {
		return nil
	}
	var out []string
	for id := range l.reads {
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
func (e *Engine) Flow(a FlowArgs, sid, author string) (out string, err error) {
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
		} else {
			f.Since = e.now().UTC()
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
