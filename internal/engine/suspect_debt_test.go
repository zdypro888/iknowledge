package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// suspect 进欠账队列(§12.2 偿还机制队列化):产生 → 领账 → confirm 重验 → 消账;
// 超 20 个聚合为一条 mass 债(批量出口指向 reanchor_all)。
func TestSuspectReverifyDebt(t *testing.T) {
	e, repo := newRepo(t, map[string]string{
		"a/a.go": "package a\n\nfunc F() int { return 1 }\n",
	})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	sid := "sid-sd"
	if _, err := e.Remember(RememberArgs{
		Node:    "a/a.go#F",
		Entries: []RememberEntry{{Kind: "summary", Text: "常量一,调用方依赖返回值"}},
	}, sid, "claude-code"); err != nil {
		t.Fatal(err)
	}

	// 改代码不记账 → init 对账降 suspect → 欠账出现。
	if err := os.WriteFile(filepath.Join(repo, "a", "a.go"),
		[]byte("package a\n\nfunc F() int { return 2 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	debts, err := e.Debts()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range debts {
		if d.Kind == "suspect-reverify" && d.Node == "a/a.go#F" {
			found = true
			if !strings.Contains(d.Hint, "kb_verify confirm") {
				t.Errorf("hint 缺重验指引:%s", d.Hint)
			}
		}
	}
	if !found {
		t.Fatalf("suspect 应进欠账队列:%+v", debts)
	}

	// confirm 重验重锚 → 债自动消失(成因消除)。
	if _, err := e.Verify(VerifyArgs{Entry: "a/a.go#F", Verdict: "confirm"}, sid, "claude-code"); err != nil {
		t.Fatal(err)
	}
	debts, _ = e.Debts()
	for _, d := range debts {
		if d.Kind == "suspect-reverify" {
			t.Errorf("重验后 suspect 债应消失:%+v", d)
		}
	}
}

func TestSuspectMassAggregates(t *testing.T) {
	files := map[string]string{}
	for i := range 21 {
		files[fmt.Sprintf("m/f%02d.go", i)] = fmt.Sprintf("package m\n\nfunc F%02d() int { return 1 }\n", i)
	}
	e, repo := newRepo(t, files)
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	sid := "sid-sm"
	for i := range 21 {
		if _, err := e.Remember(RememberArgs{
			Node:    fmt.Sprintf("m/f%02d.go#F%02d", i, i),
			Entries: []RememberEntry{{Kind: "summary", Text: fmt.Sprintf("第 %d 号函数,返回常量", i)}},
		}, sid, "claude-code"); err != nil {
			t.Fatal(err)
		}
	}
	// 全体改动(模拟全局性变更)→ 21 个 suspect → 聚合为一条 mass 债。
	for i := range 21 {
		p := filepath.Join(repo, "m", fmt.Sprintf("f%02d.go", i))
		if err := os.WriteFile(p, fmt.Appendf(nil, "package m\n\nfunc F%02d() int { return 9 }\n", i), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	debts, err := e.Debts()
	if err != nil {
		t.Fatal(err)
	}
	mass, perNode := 0, 0
	for _, d := range debts {
		if d.Kind != "suspect-reverify" {
			continue
		}
		if d.Node == "." {
			mass++
			if !strings.Contains(d.Desc, "21 个节点") || !strings.Contains(d.Hint, "reanchor_all") {
				t.Errorf("mass 债内容不对:%+v", d)
			}
		} else {
			perNode++
		}
	}
	if mass != 1 || perNode != 0 {
		t.Errorf("应聚合为 1 条 mass 债(实得 mass=%d perNode=%d)", mass, perNode)
	}
}
