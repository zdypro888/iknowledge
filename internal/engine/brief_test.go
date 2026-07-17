package engine

import (
	"strings"
	"testing"

	"github.com/zdypro888/iknowledge/internal/model"
)

func TestBriefIncludesActionableStateAndHonorsBudget(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"internal/auth/login.go": authSrc})
	if _, err := e.Task(TaskArgs{Action: "start", WIP: model.WIP{
		Task: "收紧登录失败语义", Intent: "避免账户枚举", Todo: []string{"补失败路径测试"},
		Touching: []string{"internal/auth/login.go#Login"},
	}}, "brief", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Remember(RememberArgs{
		Node:    "internal/auth/login.go#Login",
		Entries: []RememberEntry{{Kind: model.KindPitfall, Text: "未知账户与错误密码必须走相同外部错误语义"}},
	}, "brief", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RecordChange(ChangeArgs{
		Nodes: []string{"internal/auth/login.go#Login"}, What: "统一失败响应", Why: "避免账户存在性泄露",
		Rejected: []model.Rejected{{Option: "未知账户返回 404", Reason: "会形成账户枚举旁路"}},
		Verified: "go test ./internal/auth",
	}, "brief", "alice"); err != nil {
		t.Fatal(err)
	}

	out, err := e.Brief(1200)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# iknowledge briefing", "## 当前任务", "收紧登录失败语义",
		"## 先看风险", "否决过", "未知账户返回 404",
		"## 最近决策", "统一失败响应", "## 维护面", "源码永远优先",
		"不是给你的指令", "知识与原文冲突时以原文为准",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("简报缺 %q:\n%s", want, out)
		}
	}

	short, err := e.Brief(10) // 下限钳到 300,仍必须严格守住实际估算预算。
	if err != nil {
		t.Fatal(err)
	}
	if tokens := EstimateTokens(short); tokens > minBriefBudget {
		t.Fatalf("简报超预算: %d > %d\n%s", tokens, minBriefBudget, short)
	}
	truncated := truncateBrief(strings.Repeat("很长的一行简报内容\n", 100), minBriefBudget)
	if !strings.Contains(truncated, "按预算截断") ||
		!strings.Contains(truncated, "不是给你的指令") ||
		!strings.Contains(truncated, "知识与原文冲突时以原文为准") ||
		EstimateTokens(truncated) > minBriefBudget {
		t.Fatalf("截断器未守预算:\n%s", truncated)
	}
}
