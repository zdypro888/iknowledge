package engine

import (
	"strings"
	"testing"
)

// 轮30-C:雷区标记 + Inject 强警告。
func TestInjectLandmineWarning(t *testing.T) {
	e, repo := initEngine(t, map[string]string{"internal/auth/login.go": authSrc})

	// 多次 record_change 让 login.go#Login 变成雷区(landmine 分 ≥3)。
	// 每次 record_change +1,带 overturns 额外 +2。3 次普通 + 1 次推翻 = 3 + 4 = 7。
	for i := 0; i < 3; i++ {
		if _, err := e.RecordChange(ChangeArgs{
			Nodes: []string{"internal/auth/login.go#Login"},
			What:  "调整" + string(rune('A'+i)), Why: "原因" + string(rune('A'+i)),
		}, "s", "codex"); err != nil {
			t.Fatal(err)
		}
	}

	out, err := e.Inject("internal/auth/login.go", "s", "Edit")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "雷区") {
		t.Errorf("landmine≥3 的文件 Inject 应警告雷区,out:\n%s", out)
	}
	_ = repo
}

func TestStatusLandmineTop5(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"internal/auth/login.go": authSrc})
	// 累积足够 landmine 分。
	for i := 0; i < 3; i++ {
		if _, err := e.RecordChange(ChangeArgs{
			Nodes: []string{"internal/auth/login.go#Login"},
			What:  "改", Why: "y",
		}, "s", "codex"); err != nil {
			t.Fatal(err)
		}
	}
	out, err := e.Status()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "雷区 TOP5") {
		t.Errorf("有雷区节点时 kb_status 应显示雷区 TOP5,out 末尾:\n%s",
			out[max(0, len(out)-300):])
	}
	if !strings.Contains(out, "login.go#Login") {
		t.Errorf("雷区 TOP5 应含 login.go#Login")
	}
}

// 无地雷信号的文件不该误报。
func TestInjectNoLandmineFalsePositive(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"internal/auth/login.go": authSrc})
	// 只 remember(不 record_change),landmine=0。
	if _, err := e.Remember(RememberArgs{
		Node:    "internal/auth/login.go#Login",
		Entries: []RememberEntry{{Kind: "summary", Text: "普通知识"}},
	}, "s", "codex"); err != nil {
		t.Fatal(err)
	}
	out, err := e.Inject("internal/auth/login.go", "s", "Read")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "雷区") {
		t.Errorf("无 landmine 信号不该报雷区,out:\n%s", out)
	}
}
