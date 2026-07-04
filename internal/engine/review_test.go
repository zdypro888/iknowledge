package engine

import (
	"strings"
	"testing"
	"time"
)

// 非代码知识时间锚(knowledge.md §8.4):超期 → 债 + recall 标注;confirm 刷新 → 消。
func TestReviewOverdueLifecycle(t *testing.T) {
	e, _ := newRepo(t, map[string]string{
		"pay/pay.go": "package pay\n\nfunc Charge() {}\n",
	})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	sid := "sid-rv"
	// 挂在 dir 节点上的业务规则(无代码锚)。
	if _, err := e.Remember(RememberArgs{
		Node:    "pay/",
		Entries: []RememberEntry{{Kind: "contract", Text: "支付回调必须 5 秒内响应(网关约束)"}},
	}, sid, "claude-code"); err != nil {
		t.Fatal(err)
	}

	base := e.now()

	// 89 天:未超期,无债、无标注。
	e.now = func() time.Time { return base.Add(89 * 24 * time.Hour) }
	e.rt.mu.Lock()
	debts := e.computeDebtsLocked()
	e.rt.mu.Unlock()
	for _, d := range debts {
		if d.Kind == "review-overdue" {
			t.Fatalf("89 天不应有复核债:%+v", d)
		}
	}

	// 91 天:债出现,recall 有标注。
	e.now = func() time.Time { return base.Add(91 * 24 * time.Hour) }
	e.rt.mu.Lock()
	debts = e.computeDebtsLocked()
	e.rt.mu.Unlock()
	found := false
	for _, d := range debts {
		if d.Kind == "review-overdue" && d.Node == "pay/" {
			found = true
		}
	}
	if !found {
		t.Fatalf("91 天应有复核债:%+v", debts)
	}
	out, _, err := e.Recall(RecallArgs{Query: "pay/"}, sid)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "可能过期") {
		t.Errorf("recall 缺过期标注:\n%s", out)
	}

	// 节点级 confirm(无锚节点批量刷新时间锚)→ 债消失、标注消失。
	ack, err := e.Verify(VerifyArgs{Entry: "pay/", Verdict: "confirm"}, sid, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ack, "已刷新确认时间") {
		t.Errorf("节点级 confirm 应报批量刷新:%s", ack)
	}
	e.rt.mu.Lock()
	debts = e.computeDebtsLocked()
	e.rt.mu.Unlock()
	for _, d := range debts {
		if d.Kind == "review-overdue" {
			t.Errorf("confirm 后复核债应消失:%+v", d)
		}
	}
	out, _, err = e.Recall(RecallArgs{Query: "pay/"}, sid)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "可能过期") {
		t.Errorf("confirm 后不应再标过期:\n%s", out)
	}

	// 有锚节点(函数)不受时间锚约束:同样时间跨度不产生复核债。
	if _, err := e.Remember(RememberArgs{
		Node:    "pay/pay.go#Charge",
		Entries: []RememberEntry{{Kind: "summary", Text: "扣款入口"}},
	}, sid, "claude-code"); err != nil {
		t.Fatal(err)
	}
	e.now = func() time.Time { return base.Add(200 * 24 * time.Hour) }
	e.rt.mu.Lock()
	debts = e.computeDebtsLocked()
	e.rt.mu.Unlock()
	for _, d := range debts {
		if d.Kind == "review-overdue" && strings.Contains(d.Node, "#Charge") {
			t.Errorf("有锚节点不应产生复核债:%+v", d)
		}
	}
}
