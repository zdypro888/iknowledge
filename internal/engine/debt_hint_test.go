package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 就地欠账提示:被注入文件的节点挂着债 → hook 注入附提示;无债文件不附。
func TestInjectDebtHint(t *testing.T) {
	e, repo := initEngine(t, map[string]string{
		"a/a.go": "package a\n\nfunc F() int { return 1 }\n",
		"b/b.go": "package b\n\nfunc G() int { return 1 }\n",
	})
	sid := "s-dh"
	for _, node := range []string{"a/a.go#F", "b/b.go#G"} {
		if _, err := e.Remember(RememberArgs{
			Node:    node,
			Entries: []RememberEntry{{Kind: "summary", Text: "返回常量,调用方依赖非零语义 " + node}},
		}, sid, "claude-code"); err != nil {
			t.Fatal(err)
		}
	}
	// a.go 改代码不记账 → init 对账降 suspect → suspect-reverify 债挂上。
	if err := os.WriteFile(filepath.Join(repo, "a", "a.go"),
		[]byte("package a\n\nfunc F() int { return 2 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}

	out, err := e.Inject("a/a.go", sid, "Read")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "维护欠账") || !strings.Contains(out, "scope=a/a.go") {
		t.Errorf("有债文件的注入应附就地欠账提示:\n%s", out)
	}
	out, err = e.Inject("b/b.go", sid, "Read")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "维护欠账") {
		t.Errorf("无债文件不应附欠账提示:\n%s", out)
	}
}

func TestMaintainScopeTreatsFileAsBoundary(t *testing.T) {
	e, repo := initEngine(t, map[string]string{
		"a/a.go":     "package a\n\nfunc F() int { return 1 }\n",
		"a/a.go2.go": "package a\n\nfunc H() int { return 1 }\n",
	})
	sid := "s-scope"
	if _, err := e.Remember(RememberArgs{
		Node:    "a/a.go2.go#H",
		Entries: []RememberEntry{{Kind: "summary", Text: "返回常量,调用方依赖非零语义 H"}},
	}, sid, "claude-code"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "a", "a.go2.go"),
		[]byte("package a\n\nfunc H() int { return 2 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}

	out, err := e.Maintain(MaintainArgs{Action: "next", Scope: "a/a.go"}, sid, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "a/a.go2.go") || !strings.Contains(out, "范围 a/a.go 内无欠账") {
		t.Fatalf("文件 scope 不应匹配同名前缀文件:\n%s", out)
	}
	out, err = e.Maintain(MaintainArgs{Action: "next", Scope: "a/a.go2.go"}, sid, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "节点: a/a.go2.go#H") {
		t.Fatalf("精确文件 scope 应匹配本文件符号债:\n%s", out)
	}
}

// 查重警告的三种结局指引(disputes 的自然发现点)。
func TestDupWarnThreeWay(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"a/a.go": "package a\n\nfunc F() int { return 1 }\n"})
	sid := "s-dw"
	if _, err := e.Remember(RememberArgs{
		Node:    "a/a.go#F",
		Entries: []RememberEntry{{Kind: "contract", Text: "返回值恒为正数,零与负数都视为内部错误码,调用方必须先判符号再消费数值,否则会把错误码当业务量使用"}},
	}, sid, "claude-code"); err != nil {
		t.Fatal(err)
	}
	out, err := e.Remember(RememberArgs{
		Node:    "a/a.go#F",
		Entries: []RememberEntry{{Kind: "contract", Text: "返回值恒为正数,零与负数均视为内部错误码,调用方必须先判符号再消费数值,否则会把错误码当业务量使用"}},
	}, sid, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"supersedes 合并", "disputes 声明待裁决", "refute"} {
		if !strings.Contains(out, want) {
			t.Errorf("相似警告缺三种结局之 %q:\n%s", want, out)
		}
	}
}
