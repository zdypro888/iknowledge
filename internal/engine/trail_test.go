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

	// 骨架节点的 recall 也自动附来时路(冷启动价值:零 LLM 成本的考古线索)。
	view, meta, err := e.Recall(RecallArgs{Query: "pay/charge.go#Charge"}, "sid-tr")
	if err != nil || !meta.Hit {
		t.Fatalf("recall: %v hit=%v", err, meta.Hit)
	}
	if !strings.Contains(view, "此节点未消化") || !strings.Contains(view, "来时路") ||
		!strings.Contains(view, "引入 charge 扣款模块防重复扣费") {
		t.Errorf("骨架 recall 缺来时路:\n%s", view)
	}
	// 消化后(fresh 且非 stale)不再附——知识在场,考古线索退位给 history。
	if _, err := e.Remember(RememberArgs{
		Node:    "pay/charge.go#Charge",
		Entries: []RememberEntry{{Kind: "summary", Text: "扣款入口,幂等由调用方保证"}},
	}, "sid-tr", "claude-code"); err != nil {
		t.Fatal(err)
	}
	view, _, err = e.Recall(RecallArgs{Query: "pay/charge.go#Charge"}, "sid-tr")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(view, "来时路") {
		t.Errorf("已消化 fresh 节点不应附来时路:\n%s", view)
	}
}
