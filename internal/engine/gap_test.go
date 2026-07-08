package engine

import (
	"strings"
	"testing"
)

// 轮31 批次3:知识缺口发现——高被依赖零覆盖节点主动提示沉淀。
func TestStatusKnowledgeGapTop5(t *testing.T) {
	// verify 被 Login 和 CheckBoth 两处调用,但无任何知识 → 知识缺口。
	e, _ := initEngine(t, map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.26\n",
		"internal/auth/login.go": `package auth

func Login(user, pass string) error {
	return verify(user, pass)
}

func Logout(user string) error {
	return verify(user, "")
}

func verify(user, pass string) error { return nil }
`,
	})
	out, err := e.Status()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "知识缺口 TOP5") {
		t.Errorf("有高被依赖零覆盖节点时 kb_status 应报知识缺口,out 末尾:\n%s",
			out[max(0, len(out)-400):])
	}
	if !strings.Contains(out, "verify") {
		t.Errorf("知识缺口应含 verify(被 Login+Logout 调用却零覆盖)")
	}
}

// 有知识的节点不算缺口。
func TestStatusKnowledgeGapExcludesCovered(t *testing.T) {
	e, _ := initEngine(t, map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.26\n",
		"internal/auth/login.go": `package auth

func Login(user, pass string) error {
	return verify(user, pass)
}

func Logout(user string) error {
	return verify(user, "")
}

func verify(user, pass string) error { return nil }
`,
	})
	// 给 verify 沉淀知识 → 它不再是缺口。
	if _, err := e.Remember(RememberArgs{
		Node:    "internal/auth/login.go#verify",
		Entries: []RememberEntry{{Kind: "contract", Text: "verify 校验用户密码"}},
	}, "s", "codex"); err != nil {
		t.Fatal(err)
	}
	out, err := e.Status()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "知识缺口") {
		t.Errorf("verify 有知识后不该再算缺口,out:\n%s", out)
	}
}
