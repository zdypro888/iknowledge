package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zdypro888/iknowledge/internal/model"
)

func TestRedactTextPatterns(t *testing.T) {
	privateKey := "-----BEGIN PRIVATE KEY-----\nabc123\n-----END PRIVATE KEY-----"
	input := strings.Join([]string{
		"OpenAI sk-abcdefghijklmnopqrstuvwxyz123456",
		"GitHub github_pat_abcdefghijklmnopqrstuvwxyz123456",
		"AWS AKIAABCDEFGHIJKLMNOP",
		"Authorization: Bearer abcdefghijklmnopqrstuvwxyz.123456",
		"database postgres://alice:very-secret-password@db.local/app",
		"api_key=abcdefghijklmnop",
		`password: "abcdefghijklmnop"`,
		`{"client_secret":"abcdefghijklmnop"}`,
		privateKey,
	}, "\n")

	got, report := RedactText(input)
	if report.Count != 9 {
		t.Fatalf("脱敏数=%d kinds=%v\n%s", report.Count, report.Kinds, got)
	}
	for _, secret := range []string{
		"sk-abcdefghijklmnopqrstuvwxyz123456",
		"github_pat_abcdefghijklmnopqrstuvwxyz123456",
		"AKIAABCDEFGHIJKLMNOP",
		"abcdefghijklmnopqrstuvwxyz.123456",
		"very-secret-password",
		"abcdefghijklmnop",
		"abc123",
	} {
		if strings.Contains(got, secret) {
			t.Errorf("秘密仍在脱敏结果中:%q\n%s", secret, got)
		}
	}
	if !strings.Contains(got, `password: "[REDACTED:credential]"`) ||
		!strings.Contains(got, `{"client_secret":"[REDACTED:credential]"}`) {
		t.Fatalf("脱敏破坏了结构化引号:\n%s", got)
	}
}

func TestRedactSecretsOnlyTaggedFields(t *testing.T) {
	type payload struct {
		Text string `redact:"true"`
		ID   string
	}
	secret := "sk-abcdefghijklmnopqrstuvwxyz123456"
	p := payload{Text: "凭证 " + secret, ID: secret}
	report := RedactSecrets(&p)
	if report.Count != 1 || strings.Contains(p.Text, secret) {
		t.Fatalf("tagged 字段未脱敏: report=%+v payload=%+v", report, p)
	}
	if p.ID != secret {
		t.Fatalf("未标记结构字段被改写:%q", p.ID)
	}
}

func TestRememberRedactsBeforePersistence(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"internal/auth/login.go": authSrc})
	secret := "sk-abcdefghijklmnopqrstuvwxyz123456"
	out, err := e.Remember(RememberArgs{
		Node: "internal/auth/login.go#Login",
		Entries: []RememberEntry{{
			Kind: "pitfall",
			Text: "测试日志曾包含凭证 " + secret + ",输出前必须过滤",
		}},
	}, "redact-test", "tester")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "写入前已脱敏 1 处") {
		t.Fatalf("响应未告知脱敏:%s", out)
	}
	node := loadNodes(t, e, "internal/auth/login.go")["internal/auth/login.go#Login"]
	if len(node.Entries) != 1 {
		t.Fatalf("entries=%d", len(node.Entries))
	}
	if strings.Contains(node.Entries[0].Text, secret) || !strings.Contains(node.Entries[0].Text, "[REDACTED:openai-key]") {
		t.Fatalf("落盘文本未安全脱敏:%s", node.Entries[0].Text)
	}
}

func TestAllSemanticArgumentTypesExposeRedactableFields(t *testing.T) {
	secret := "sk-abcdefghijklmnopqrstuvwxyz123456"
	cases := []struct {
		name  string
		value any
	}{
		{"remember", &RememberArgs{Entries: []RememberEntry{{Text: secret}}, Keywords: []string{secret}}},
		{"record-change", &ChangeArgs{What: secret, Why: secret, Task: secret, Rebuttal: secret, Verified: secret,
			Rejected: []model.Rejected{{Option: secret, Reason: secret}}}},
		{"task", &TaskArgs{WIP: model.WIP{Task: secret, Intent: secret, Plan: []string{secret}, Done: []string{secret}, Todo: []string{secret}}}},
		{"flow", &FlowArgs{Flow: model.Flow{Title: secret, Steps: []model.FlowStep{{Note: secret}}, Conventions: []string{secret}, Troubleshoot: secret}}},
		{"verify", &VerifyArgs{Evidence: secret, Reason: secret}},
		{"adopt", &AdoptArgs{Reason: secret}},
		{"investigate", &InvestigateArgs{Question: secret}},
		{"findings", &FindingsArgs{Conclusion: secret, Plan: secret, Risks: secret}},
		{"maintain", &MaintainArgs{EraSummary: secret}},
		{"revert", &RevertArgs{Reason: secret}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := RedactSecrets(tc.value)
			data, err := json.Marshal(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			if report.Count == 0 || strings.Contains(string(data), secret) {
				t.Fatalf("语义入参未完整暴露脱敏字段:report=%+v payload=%s", report, data)
			}
		})
	}
}
