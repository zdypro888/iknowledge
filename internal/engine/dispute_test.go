package engine

import (
	"strings"
	"testing"
)

// 矛盾裁决机械层(knowledge.md §12.4):登记 → 双向呈现 + 派债 → 一方退场自动解除。
func TestDisputeLifecycle(t *testing.T) {
	e, _ := newRepo(t, map[string]string{
		"auth/auth.go": "package auth\n\nfunc Login() {}\n",
	})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	sid := "sid-dp"
	node := "auth/auth.go#Login"

	out, err := e.Remember(RememberArgs{
		Node:    node,
		Entries: []RememberEntry{{Kind: "contract", Text: "pass 参数传明文,函数内部做哈希"}},
	}, sid, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	firstID := strings.TrimSpace(strings.TrimPrefix(strings.SplitN(out, "\n", 2)[0], "entryIds:"))

	// 声明矛盾:引用不存在 → 拒收。
	if _, err := e.Remember(RememberArgs{
		Node:    node,
		Entries: []RememberEntry{{Kind: "contract", Text: "pass 参数传的是已哈希值", Disputes: []string{node + "#e_deadbeef"}}},
	}, sid, "claude-code"); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("无效 disputes 引用应拒收,err=%v", err)
	}

	// 正常登记。
	out, err = e.Remember(RememberArgs{
		Node:    node,
		Entries: []RememberEntry{{Kind: "contract", Text: "pass 参数传的是已哈希值,函数不再处理", Disputes: []string{node + "#" + firstID}}},
	}, sid, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "矛盾已登记待裁决") {
		t.Errorf("登记提示缺失:%s", out)
	}

	// recall:双向标注。
	view, _, err := e.Recall(RecallArgs{Query: node}, sid)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(view, "矛盾待裁决") || !strings.Contains(view, "被 "+node+"#") {
		t.Errorf("矛盾双向呈现缺失:\n%s", view)
	}

	// 债:dispute-open 出现。
	e.rt.mu.Lock()
	debts := e.computeDebtsLocked()
	e.rt.mu.Unlock()
	found := false
	for _, d := range debts {
		if d.Kind == "dispute-open" {
			found = true
		}
	}
	if !found {
		t.Fatalf("缺 dispute-open 债:%+v", debts)
	}

	// 裁决:refute 第一条(附证据)→ 债与标注自动解除。
	if _, err := e.Verify(VerifyArgs{
		Entry: node + "#" + firstID, Verdict: "refute",
		Evidence: "auth.go 第 3 行:Login 直接比较传入值与存储哈希,未做哈希运算",
	}, sid, "claude-code"); err != nil {
		t.Fatal(err)
	}
	e.rt.mu.Lock()
	debts = e.computeDebtsLocked()
	e.rt.mu.Unlock()
	for _, d := range debts {
		if d.Kind == "dispute-open" {
			t.Errorf("裁决后 dispute-open 债应消失:%+v", d)
		}
	}
	view, _, err = e.Recall(RecallArgs{Query: node}, sid)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(view, "矛盾待裁决") {
		t.Errorf("裁决后不应再标矛盾:\n%s", view)
	}

	// 对已退场条目声明矛盾 → 拒收。
	if _, err := e.Remember(RememberArgs{
		Node:    node,
		Entries: []RememberEntry{{Kind: "pitfall", Text: "又一条声明", Disputes: []string{node + "#" + firstID}}},
	}, sid, "claude-code"); err == nil || !strings.Contains(err.Error(), "已退场") {
		t.Fatalf("对退场条目的 disputes 应拒收,err=%v", err)
	}
}
