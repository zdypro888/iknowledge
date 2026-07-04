package engine

import (
	"os/exec"
	"strings"
	"testing"
)

// 侦查简报的"来时路"(git 历史挖掘的机械落地):线索文件的近期提交进简报。
func TestInvestigateGitTrail(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("无 git")
	}
	e, repo := newRepo(t, map[string]string{
		"pay/charge.go": "package pay\n\n// Charge 扣款入口。\nfunc Charge() {}\n",
	})
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@t"}, {"config", "user.name", "t"},
		{"add", "."},
		{"commit", "-q", "-m", "引入 charge 扣款模块防重复扣费"},
	} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}

	out, err := e.Investigate(InvestigateArgs{Question: "charge 扣款在哪里"}, "sid-tr", "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "侦查简报") {
		t.Fatalf("应开 job 出简报:\n%s", out)
	}
	if !strings.Contains(out, "来时路") || !strings.Contains(out, "引入 charge 扣款模块防重复扣费") {
		t.Errorf("简报缺来时路/提交线索:\n%s", out)
	}
}
