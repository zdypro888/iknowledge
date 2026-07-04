package engine

import (
	"fmt"
	"strings"

	"github.com/zdypro888/iknowledge/internal/model"
)

// ---- kb_verify(impl §7.3):勘误与污染回收 ----

// VerifyArgs 是 kb_verify 入参。
type VerifyArgs struct {
	Entry    string `json:"entry"` // "node-id#entry-id"
	Verdict  string `json:"verdict"`
	Evidence string `json:"evidence,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// Verify confirm(升级)/ refute(勘误,须证据,级联回收)/ obsolete(体面退休)。
func (e *Engine) Verify(a VerifyArgs, sid, author string) (string, error) {
	if err := e.requireInit(); err != nil {
		return "", err
	}
	if err := e.Sync(); err != nil {
		return "", err
	}
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()

	// #3:node-level 重验——entry 传的是纯节点 ID(能直接解析到节点)且 verdict=confirm 时,
	// 视为"读过原文、知识仍成立",重验即重锚清 suspect(无需为清 suspect 硬写一条新知识)。
	if nid := e.rt.ix.ResolveNodeID(strings.TrimSpace(a.Entry)); nid != "" && a.Verdict == "confirm" {
		return e.reverifyNodeLocked(nid)
	}

	// 引用沿 supersedes 链解析(引用旧 ID 自动落到现任条目,impl §7.3)。
	ref := e.rt.ix.ResolveEntryRef(a.Entry)
	i := strings.LastIndexByte(ref, '#')
	if i <= 0 {
		return "", kbErr("NODE_NOT_FOUND", "条目引用格式错:"+a.Entry, "格式 node-id#entry-id;或传纯节点 ID + confirm 做节点级重验")
	}
	nodeID, entryID := ref[:i], ref[i+1:]
	nodeRef := e.rt.ix.Node(nodeID)
	if nodeRef == nil {
		return "", kbErr("NODE_NOT_FOUND", "节点 "+nodeID+" 不存在", "用 kb_recall 核对")
	}
	var entry *model.Entry
	for j := range nodeRef.Node.Entries {
		if nodeRef.Node.Entries[j].ID == entryID {
			entry = &nodeRef.Node.Entries[j]
		}
	}
	if entry == nil {
		return "", kbErr("NODE_NOT_FOUND", "条目 "+entryID+" 不在节点 "+nodeID, "用 kb_recall 核对")
	}

	switch a.Verdict {
	case "confirm":
		old := entry.Confidence
		if old == model.ConfidenceRefuted {
			return "", kbErr("EVIDENCE_REQUIRED",
				"条目已被勘误,confirm 翻案须走新条目",
				"带原文证据 kb_remember 一条新知识并在文中回应勘误")
		}
		entry.Confidence = model.ConfidenceVerified
		entry.ConfirmedAt = e.now().UTC() // 非代码知识的时间锚刷新(§8.4)
		if err := e.saveNodeShardLocked(nodeRef); err != nil {
			return "", err
		}
		if err := e.reloadLocked(); err != nil {
			return "", err
		}
		return fmt.Sprintf("newConfidence: verified(原 %s)", old), nil

	case "refute":
		// 勘误必须附原文证据,服务端无证据不接受(knowledge.md §12.5)。
		if strings.TrimSpace(a.Evidence) == "" {
			return "", kbErr("EVIDENCE_REQUIRED", "refute 必须附原文证据(引用具体代码行)",
				"附 evidence 后重试")
		}
		// 勘误进 journal:纠错是一等公民。
		chID := e.freshChangeIDLocked()
		change := model.Change{
			ID: chID, Nodes: []string{nodeID}, At: e.now().UTC(),
			What:   "勘误:条目 " + entryID + " 被驳倒(" + firstLine(entry.Text) + ")",
			Why:    "原文证据:" + a.Evidence,
			Author: author,
		}
		if err := e.Store.AppendChange(change); err != nil {
			return "", err
		}
		entry.Confidence = model.ConfidenceRefuted
		entry.RefutedBy = chID
		// 污染回收:沿 based_on 级联降级衍生条目(knowledge.md §12.5 第 2 条)。
		var cascaded []string
		for _, depRef := range e.rt.ix.Dependents(nodeID + "#" + entryID) {
			di := strings.LastIndexByte(depRef, '#')
			depNode := e.rt.ix.Node(depRef[:di])
			if depNode == nil {
				continue
			}
			for j := range depNode.Node.Entries {
				en := &depNode.Node.Entries[j]
				if en.ID == depRef[di+1:] && en.Active() && en.Confidence != model.ConfidenceSuspect {
					en.Confidence = model.ConfidenceSuspect
					cascaded = append(cascaded, depRef)
					if err := e.saveNodeShardLocked(depNode); err != nil {
						return "", err
					}
				}
			}
		}
		if err := e.saveNodeShardLocked(nodeRef); err != nil {
			return "", err
		}
		if err := e.reloadLocked(); err != nil {
			return "", err
		}
		var b strings.Builder
		fmt.Fprintf(&b, "newConfidence: refuted(勘误记录 %s)\ncascaded: %v", chID, cascaded)
		fmt.Fprintf(&b, "\n疫苗义务:请在节点 %s 补一条 pitfall——「本处易被误读为 X(实际是 Y)」,下一个 AI 大概率犯同样的读错(kb_remember)", nodeID)
		return b.String(), nil

	case "obsolete":
		// 体面退休:没错但不再适用,归档退出注入,不触发级联(impl §7.3)。
		if strings.TrimSpace(a.Reason) == "" {
			return "", kbErr("EVIDENCE_REQUIRED", "obsolete 须附 reason(功能下线/约定废止)", "附 reason 后重试")
		}
		chID := e.freshChangeIDLocked()
		change := model.Change{
			ID: chID, Nodes: []string{nodeID}, At: e.now().UTC(),
			What: "退休:条目 " + entryID + "(" + firstLine(entry.Text) + ")",
			Why:  a.Reason, Author: author,
		}
		if err := e.Store.AppendChange(change); err != nil {
			return "", err
		}
		entry.RetiredBy = chID
		if err := e.saveNodeShardLocked(nodeRef); err != nil {
			return "", err
		}
		if err := e.reloadLocked(); err != nil {
			return "", err
		}
		return "条目已退休(记录 " + chID + "),不触发级联", nil
	}
	return "", kbErr("INVALID_ARGUMENT", "非法 verdict "+a.Verdict, "verdict ∈ confirm|refute|obsolete")
}

// reverifyNodeLocked 节点级重验:读原文现算哈希,与当前锚一致则清 suspect(重验即重锚)。
func (e *Engine) reverifyNodeLocked(nodeID string) (string, error) {
	ref := e.rt.ix.Node(nodeID)
	if ref == nil {
		return "", kbErr("NODE_NOT_FOUND", "节点 "+nodeID+" 不存在", "用 kb_recall 核对")
	}
	// 无代码锚的节点(project/dir,§8.4):confirm = 批量刷新全部活跃条目的时间锚
	// (逐条 confirm 太摩擦;节点级语义即"我核实过,这些仍成立")。
	if ref.Node.Anchor.Hash == "" && !ref.Node.PendingAnchor {
		refreshed := 0
		now := e.now().UTC()
		for i := range ref.Node.Entries {
			if en := &ref.Node.Entries[i]; en.Active() {
				en.ConfirmedAt = now
				refreshed++
			}
		}
		if refreshed == 0 {
			return "节点 " + nodeID + " 无活跃条目,无需复核。", nil
		}
		if err := e.saveNodeShardLocked(ref); err != nil {
			return "", err
		}
		if err := e.reloadLocked(); err != nil {
			return "", err
		}
		return fmt.Sprintf("ack:节点 %s 的 %d 条非代码知识已刷新确认时间(不成立的请单独 refute/obsolete)。", nodeID, refreshed), nil
	}
	if ref.Node.Status != model.StatusSuspect {
		return "节点 " + nodeID + " 当前不是 suspect(状态 " + string(ref.Node.Status) + "),无需重验。", nil
	}
	cur := e.currentAnchorLocked(ref)
	if cur.parseErr != nil {
		return "", kbErr("PARSE_FAILED", "文件当前不可解析,无法重验", "修完语法后重试")
	}
	if cur.missing {
		return "", kbErr("NODE_ORPHANED", "符号已消失,无法重验", "认领/送葬走 kb_adopt")
	}
	// 重锚到当前代码 + 清 suspect(等价"我读过原文,现有知识对当前代码仍成立")。
	ref.Node.Anchor = cur.anchor
	ref.Node.Status = model.StatusFresh
	if err := e.saveNodeShardLocked(ref); err != nil {
		return "", err
	}
	if err := e.reloadLocked(); err != nil {
		return "", err
	}
	return "节点 " + nodeID + " 已重验重锚,suspect 解除(其条目请逐条确认仍成立,不成立的单独 refute)。", nil
}

// ---- kb_adopt(impl §7.3):孤儿处置 ----

// AdoptArgs 是 kb_adopt 入参。
type AdoptArgs struct {
	Orphan string `json:"orphan"`
	Action string `json:"action"` // claim | bury
	To     string `json:"to,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Adopt claim(认领,等价一次申报式迁移)/ bury(送葬,归档进 journal 可溯)。
func (e *Engine) Adopt(a AdoptArgs, sid, author string) (string, error) {
	if err := e.requireInit(); err != nil {
		return "", err
	}
	if err := e.Sync(); err != nil {
		return "", err
	}
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()

	ref := e.rt.ix.Node(a.Orphan)
	if ref == nil {
		return "", kbErr("NODE_NOT_FOUND", "孤儿节点 "+a.Orphan+" 不存在", "kb_status 列出孤儿")
	}
	if ref.Node.Status != model.StatusOrphaned {
		return "", kbErr("NODE_NOT_FOUND", a.Orphan+" 不是 orphaned 状态", "只有孤儿可 adopt")
	}

	switch a.Action {
	case "claim":
		if a.To == "" {
			return "", kbErr("NODE_NOT_FOUND", "claim 必须给 to(新节点 ID)", "附 to 后重试")
		}
		chID := e.freshChangeIDLocked()
		change := model.Change{
			ID: chID, Nodes: []string{a.To}, At: e.now().UTC(),
			What: "认领孤儿 " + a.Orphan + " → " + a.To,
			Why:  "kb_adopt claim(等价申报式迁移)", Author: author,
		}
		if err := e.Store.AppendChange(change); err != nil {
			return "", err
		}
		if err := e.applyRemapsLocked([]model.Remap{{From: a.Orphan, To: []string{a.To}}}); err != nil {
			return "", err
		}
		if err := e.reloadLocked(); err != nil {
			return "", err
		}
		return "migrated: " + a.Orphan + " → " + a.To + "(条目降半级待确认,血缘已接续,记录 " + chID + ")", nil

	case "bury":
		if strings.TrimSpace(a.Reason) == "" {
			return "", kbErr("EVIDENCE_REQUIRED", "bury 必须附 reason(为什么确认作废)", "附 reason 后重试")
		}
		// 归档:送葬原因 + 条目快照进 journal(可溯);节点从分片摘除
		// (git 历史 + journal 双保险,不留下永久孤儿噪音)。
		var snapshot []string
		for i := range ref.Node.Entries {
			snapshot = append(snapshot, "["+ref.Node.Entries[i].Kind+"] "+ref.Node.Entries[i].Text)
		}
		chID := e.freshChangeIDLocked()
		change := model.Change{
			ID: chID, Nodes: []string{a.Orphan}, At: e.now().UTC(),
			What: "送葬孤儿 " + a.Orphan + "(知识快照:" + strings.Join(snapshot, ";") + ")",
			Why:  a.Reason, Author: author,
		}
		if err := e.Store.AppendChange(change); err != nil {
			return "", err
		}
		if err := e.removeNodeLocked(ref); err != nil {
			return "", err
		}
		if err := e.reloadLocked(); err != nil {
			return "", err
		}
		return "buried: " + a.Orphan + "(记录 " + chID + ",知识快照已入 journal 可溯)", nil
	}
	return "", kbErr("INVALID_ARGUMENT", "非法 action "+a.Action, "action ∈ claim|bury")
}

func (e *Engine) freshChangeIDLocked() string {
	ids := map[string]bool{}
	for _, c := range e.rt.ix.Changes() {
		ids[c.ID] = true
	}
	id := model.NewChangeID(e.now())
	for ids[id] {
		id = model.NewChangeID(e.now())
	}
	return id
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len([]rune(s)) > 40 {
		return string([]rune(s)[:40]) + "…"
	}
	return s
}
