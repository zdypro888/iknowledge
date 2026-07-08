package engine

import (
	"strings"
	"testing"
)

// 轮31 批次2:变更影响面分析——record_change 时用 callgraph 报告波及调用方/契约。
func TestRecordChangeImpactAnalysis(t *testing.T) {
	// 构造有调用关系的代码:HandleLogin 调 Login,Login 调 verify。
	e, _ := initEngine(t, map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.26\n",
		"internal/auth/login.go": `package auth

func Login(user, pass string) error {
	if user == "" {
		return errEmpty
	}
	return verify(user, pass)
}

func verify(user, pass string) error { return nil }

var errEmpty error
`,
		"internal/api/handler.go": `package api

import "example.com/m/internal/auth"

func HandleLogin(user, pass string) error {
	return auth.Login(user, pass)
}
`,
	})

	// 给调用方 HandleLogin 沉淀一条契约知识(它依赖 Login 的行为)。
	if _, err := e.Remember(RememberArgs{
		Node:    "internal/api/handler.go#HandleLogin",
		Entries: []RememberEntry{{Kind: "contract", Text: "HandleLogin 依赖 Login 返回 nil 表示成功"}},
	}, "s", "claude-code"); err != nil {
		t.Fatal(err)
	}

	// 现在 record_change 改 Login → 回执应报告"变更影响:被 HandleLogin 调用,它带知识"。
	out, err := e.RecordChange(ChangeArgs{
		Nodes: []string{"internal/auth/login.go#Login"},
		What:  "改 Login 的验证逻辑", Why: "加锁定检查",
	}, "s", "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "变更影响") {
		t.Errorf("record_change 改被调用的节点应报告变更影响面,out:\n%s", out)
	}
	if !strings.Contains(out, "HandleLogin") {
		t.Errorf("变更影响应列出带知识的调用方 HandleLogin,out:\n%s", out)
	}
	if !strings.Contains(out, "复核") {
		t.Errorf("应提示复核调用方契约,out:\n%s", out)
	}
}

// 调用方没知识时不报(避免噪音)。
func TestRecordChangeImpactNoKnowledgeNoNoise(t *testing.T) {
	e, _ := initEngine(t, map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.26\n",
		"internal/auth/login.go": `package auth

func Login(user, pass string) error {
	return verify(user, pass)
}

func verify(user, pass string) error { return nil }
`,
	})
	// 不给任何调用方沉淀知识。
	out, err := e.RecordChange(ChangeArgs{
		Nodes: []string{"internal/auth/login.go#verify"},
		What:  "改 verify", Why: "y",
	}, "s", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "变更影响") {
		t.Errorf("调用方无知识时不该报变更影响(纯机械依赖,recall 已展示),out:\n%s", out)
	}
}
