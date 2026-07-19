package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	gort "runtime"
	"strings"
	"testing"
	"time"

	"github.com/zdypro888/iknowledge/internal/model"
)

func TestTaskContextCancellationNeverCreatesWIP(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"a.go": "package p\n\nfunc A() {}\n"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.TaskContext(ctx, TaskArgs{Action: "start", WIP: model.WIP{Task: "不得落盘"}}, "canceled", "codex"); !errors.Is(err, context.Canceled) {
		t.Fatalf("TaskContext error=%v, want context.Canceled", err)
	}
	wips, err := e.Store.LoadWIPs()
	if err != nil {
		t.Fatal(err)
	}
	if len(wips) != 0 {
		t.Fatalf("canceled task mutated WIP state: %+v", wips)
	}
}

func TestDecisionFirewallLockWaitHonorsContext(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"a.go": "package p\n\nfunc A() {}\n"})
	e.rt.mu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	out := e.semanticDecisionFirewall(ctx, "change A", []string{"a.go#A"})
	elapsed := time.Since(started)
	e.rt.mu.Unlock()
	if out != "" || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("firewall canceled output=%q context=%v", out, ctx.Err())
	}
	if elapsed > time.Second {
		t.Fatalf("firewall lock wait ignored context for %v", elapsed)
	}
}

func TestTaskWIPIsolatedBySession(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"a.go": "package p\n\nfunc A() {}\n"})
	for _, tc := range []struct{ sid, task string }{{"sid-one", "任务一"}, {"sid-two", "任务二"}} {
		if _, err := e.Task(TaskArgs{Action: "start", WIP: model.WIP{Task: tc.task}}, tc.sid, "codex"); err != nil {
			t.Fatalf("start %s: %v", tc.sid, err)
		}
	}
	wips, err := e.Store.LoadWIPs()
	if err != nil {
		t.Fatal(err)
	}
	if len(wips) != 2 {
		t.Fatalf("同 author 的两个 session 应有独立 WIP, got %+v", wips)
	}
	for _, want := range []string{"codex@sid-one", "codex@sid-two"} {
		found := false
		for _, w := range wips {
			found = found || w.Owner == want
		}
		if !found {
			t.Errorf("缺 owner %s: %+v", want, wips)
		}
	}

	if _, err := e.Task(TaskArgs{Action: "complete"}, "sid-one", "codex"); err != nil {
		t.Fatal(err)
	}
	out, err := e.Task(TaskArgs{Action: "get"}, "sid-two", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "任务二") || strings.Contains(out, "任务一") {
		t.Fatalf("完成 sid-one 不应清除 sid-two: %s", out)
	}
}

func TestTaskCompleteRestoresWIPWhenJournalAppendFails(t *testing.T) {
	if gort.GOOS == "windows" {
		t.Skip("chmod 故障注入不适用于 Windows")
	}
	e, repo := initEngine(t, map[string]string{"a.go": "package p\n"})
	if _, err := e.Task(TaskArgs{Action: "start", WIP: model.WIP{Task: "不可丢的任务", Intent: "验证归档事务"}}, "sid", "codex"); err != nil {
		t.Fatal(err)
	}
	journalDir := filepath.Join(repo, ".knowledge", "journal")
	if err := os.Chmod(journalDir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(journalDir, 0o755)
	if _, err := e.Task(TaskArgs{Action: "complete"}, "sid", "codex"); err == nil {
		t.Fatal("journal 不可写时 complete 应失败")
	}
	if err := os.Chmod(journalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	wips, err := e.Store.LoadWIPs()
	if err != nil {
		t.Fatal(err)
	}
	if len(wips) != 1 || wips[0].Task != "不可丢的任务" {
		t.Fatalf("journal 失败后 WIP 未恢复:%+v", wips)
	}
	changes, _, err := e.Store.LoadJournal()
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("失败归档不应留下 journal:%+v", changes)
	}
}

func TestTaskUpdateMigratesLegacyOwnerWithoutDuplicate(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"a.go": "package p\n"})
	if err := e.Store.SaveWIP(model.WIP{Owner: "codex", Task: "legacy task", Todo: []string{"old"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Task(TaskArgs{Action: "update", WIP: model.WIP{Todo: []string{"new"}}}, "sid", "codex"); err != nil {
		t.Fatal(err)
	}
	wips, err := e.Store.LoadWIPs()
	if err != nil {
		t.Fatal(err)
	}
	if len(wips) != 1 || wips[0].Owner != "codex@sid" || len(wips[0].Todo) != 1 || wips[0].Todo[0] != "new" {
		t.Fatalf("legacy WIP 迁移未收敛:%+v", wips)
	}
}
