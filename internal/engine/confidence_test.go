package engine

import (
	"strings"
	"testing"

	"github.com/zdypro888/iknowledge/internal/model"
)

// 置信度桥接(实战反馈"116/116 inferred、0 verified"):带 verified 的 record_change
// 触及有 inferred 知识的节点 → 即时提示;存量走 confidence-lag 债;confirm 后消。
func TestConfidenceBridge(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"a/a.go": "package a\n\nfunc F() int { return 1 }\n"})
	sid := "s-cf"
	if _, err := e.Remember(RememberArgs{
		Node:    "a/a.go#F",
		Entries: []RememberEntry{{Kind: "contract", Text: "返回值调用方依赖为正,负数视为错误码"}},
	}, sid, "claude-code"); err != nil {
		t.Fatal(err)
	}

	// ① 不带 verified 的记账:不提示确认。
	out, err := e.RecordChange(ChangeArgs{
		Nodes: []string{"a/a.go#F"}, What: "微调", Why: "x",
	}, sid, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "顺手确认") {
		t.Errorf("无 verified 不应提示确认:%s", out)
	}

	// ② 带 verified 的记账:即时提示升级。
	out, err = e.RecordChange(ChangeArgs{
		Nodes: []string{"a/a.go#F"}, What: "改判定", Why: "撞库",
		Verified: "go test ./a -run TestF 通过",
	}, sid, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "顺手确认") || !strings.Contains(out, "kb_verify confirm") {
		t.Errorf("带 verified 应提示升级:%s", out)
	}

	// ③ 存量债:节点有 inferred + 历史有 verified → confidence-lag 债。
	debts, err := e.Debts()
	if err != nil {
		t.Fatal(err)
	}
	var lag *Debt
	for i := range debts {
		if debts[i].Kind == "confidence-lag" && debts[i].Node == "a/a.go#F" {
			lag = &debts[i]
		}
	}
	if lag == nil {
		t.Fatalf("应有 confidence-lag 债:%+v", debts)
	}

	// ④ confirm 升级 → 债消失(不再有活跃 inferred 条目)。
	// 拿条目 ID。
	view, _, err := e.Recall(RecallArgs{Query: "a/a.go#F"}, sid)
	if err != nil {
		t.Fatal(err)
	}
	eid := extractEntryID(t, view, "返回值调用方依赖")
	if _, err := e.Verify(VerifyArgs{Entry: "a/a.go#F#" + eid, Verdict: "confirm"}, sid, "claude-code"); err != nil {
		t.Fatal(err)
	}
	debts, _ = e.Debts()
	for _, d := range debts {
		if d.Kind == "confidence-lag" {
			t.Errorf("confirm 升 verified 后债应消失:%+v", d)
		}
	}
}

// 无 verified 历史的节点不产生 confidence-lag 债(避免全库刷屏)。
func TestConfidenceLagNeedsVerifiedHistory(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"a/a.go": "package a\n\nfunc F() int { return 1 }\n"})
	sid := "s-cf2"
	if _, err := e.Remember(RememberArgs{
		Node:    "a/a.go#F",
		Entries: []RememberEntry{{Kind: "summary", Text: "返回常量一"}},
	}, sid, "claude-code"); err != nil {
		t.Fatal(err)
	}
	// 记账但不带 verified。
	if _, err := e.RecordChange(ChangeArgs{Nodes: []string{"a/a.go#F"}, What: "x", Why: "y"}, sid, "claude-code"); err != nil {
		t.Fatal(err)
	}
	debts, _ := e.Debts()
	for _, d := range debts {
		if d.Kind == "confidence-lag" {
			t.Errorf("无 verified 历史不应产生 confidence-lag 债:%+v", d)
		}
	}
	_ = model.ConfidenceInferred
}
