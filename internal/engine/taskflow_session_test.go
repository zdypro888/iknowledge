package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	gort "runtime"
	"sort"
	"strings"
	"sync"
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

func TestDecisionAdvisoryLockWaitHonorsContext(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"a.go": "package p\n\nfunc A() {}\n"})
	e.rt.mu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	out := e.semanticDecisionAdvisory(ctx, "change A", []string{"a.go#A"})
	elapsed := time.Since(started)
	e.rt.mu.Unlock()
	if out != "" || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("advisory canceled output=%q context=%v", out, ctx.Err())
	}
	if elapsed > time.Second {
		t.Fatalf("advisory lock wait ignored context for %v", elapsed)
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

func TestTaskAbandonClearsOnlyCurrentWIPWithoutJournal(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"a.go": "package p\n\nfunc A() {}\n"})
	for _, sid := range []string{"sid-cancel", "sid-keep"} {
		if _, err := e.Task(TaskArgs{Action: "start", WIP: model.WIP{
			Task: "task " + sid, Touching: []string{"a.go#A"},
		}}, sid, "codex"); err != nil {
			t.Fatal(err)
		}
	}
	before := len(e.rt.ix.Changes())
	out, err := e.Task(TaskArgs{Action: "abandon"}, "sid-cancel", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "未写入知识层或 journal") {
		t.Fatalf("abandon 回执不明确: %s", out)
	}
	if len(e.rt.ix.Changes()) != before {
		t.Fatal("abandon 伪造了变更历史")
	}
	wips, err := e.Store.LoadWIPs()
	if err != nil {
		t.Fatal(err)
	}
	if len(wips) != 1 || wips[0].Owner != "codex@sid-keep" {
		t.Fatalf("abandon 清理范围错误: %+v", wips)
	}
}

func TestTaskCrossSessionStaleCompletePersistsReasonAndClearsAtomically(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"a.go": "package p\n\nfunc A() {}\n"})
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	e.now = func() time.Time { return now }
	const target = "codex@stale-session"
	if err := e.Store.SaveWIP(model.WIP{
		Owner: target, Task: "已由提交完成的任务", Intent: "修复 A", Done: []string{"commit abc123"},
		Touching: []string{"a.go#A"}, Updated: now.Add(-8 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	reason := "commit abc123 已部署且回归 TestA 通过"
	out, err := e.Task(TaskArgs{Action: "complete", Owner: target, Reason: reason}, "new-session", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, target) {
		t.Fatalf("跨会话 complete 回执未标目标 owner: %s", out)
	}
	wips, err := e.Store.LoadWIPs()
	if err != nil {
		t.Fatal(err)
	}
	if len(wips) != 0 {
		t.Fatalf("跨会话 complete 未清空目标 WIP: %+v", wips)
	}
	changes, _, err := e.Store.LoadJournal()
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 {
		t.Fatalf("journal changes=%d, want 1: %+v", len(changes), changes)
	}
	if got := changes[0].Verified; !strings.Contains(got, target) || !strings.Contains(got, reason) ||
		!strings.Contains(got, "closed_by=codex@new-session") {
		t.Fatalf("跨会话收口依据未持久化: %q", got)
	}
}

func TestTaskCompleteRedactsLegacyWIPBeforeJournal(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"a.go": "package p\n\nfunc A() {}\n"})
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	e.now = func() time.Time { return now }
	secret := "sk-abcdefghijklmnopqrstuvwxyz"
	target := "legacy@" + secret
	// 直接写 store 模拟尚未实行入口脱敏的旧版 WIP。
	if err := e.Store.SaveWIP(model.WIP{
		Owner: target, Task: "任务 " + secret, Intent: "意图 " + secret,
		Done: []string{"已完成 " + secret}, Touching: []string{"a.go#A"},
		Updated: now.Add(-8 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	out, err := e.Task(TaskArgs{
		Action: "complete", Owner: target, Reason: "commit abc123 已验证",
	}, "closer", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "写入前已脱敏") {
		t.Fatalf("回执缺旧 WIP 脱敏说明: %s", out)
	}
	changes, _, err := e.Store.LoadJournal()
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 {
		t.Fatalf("journal changes=%d, want 1", len(changes))
	}
	change := changes[0]
	persisted := change.Task + "\n" + change.What + "\n" + change.Why + "\n" + change.Verified
	if strings.Contains(persisted, secret) {
		t.Fatalf("旧 WIP 凭据泄入 journal: %s", persisted)
	}
	if !strings.Contains(persisted, "[REDACTED:openai-key]") {
		t.Fatalf("journal 缺脱敏标记: %s", persisted)
	}
}

func TestTaskCrossSessionStaleAbandonClearsOnlyTargetWithoutJournal(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"a.go": "package p\n\nfunc A() {}\n"})
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	e.now = func() time.Time { return now }
	for _, w := range []model.WIP{
		{Owner: "codex@stale", Task: "已取消", Updated: now.Add(-9 * 24 * time.Hour)},
		{Owner: "codex@keep", Task: "保留", Updated: now.Add(-10 * 24 * time.Hour)},
	} {
		if err := e.Store.SaveWIP(w); err != nil {
			t.Fatal(err)
		}
	}
	out, err := e.Task(TaskArgs{
		Action: "abandon", Owner: "codex@stale", Reason: "产品决策已明确取消并由新流程取代",
	}, "auditor", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "未写入知识层或 journal") {
		t.Fatalf("跨会话 abandon 回执不明确: %s", out)
	}
	wips, err := e.Store.LoadWIPs()
	if err != nil {
		t.Fatal(err)
	}
	if len(wips) != 1 || wips[0].Owner != "codex@keep" {
		t.Fatalf("跨会话 abandon 清理范围错误: %+v", wips)
	}
	changes, _, err := e.Store.LoadJournal()
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("跨会话 abandon 伪造了 journal: %+v", changes)
	}
}

func TestTaskCrossSessionClosureRejectionsAreZeroWrite(t *testing.T) {
	e, repo := initEngine(t, map[string]string{"a.go": "package p\n\nfunc A() {}\n"})
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	e.now = func() time.Time { return now }
	for _, w := range []model.WIP{
		{Owner: "codex@old", Task: "旧任务", Updated: now.Add(-8 * 24 * time.Hour)},
		{Owner: "codex@recent", Task: "仍活跃", Updated: now.Add(-6 * 24 * time.Hour)},
		{Owner: "codex@auditor", Task: "当前会话仍活跃", Updated: now.Add(-6 * 24 * time.Hour)},
		{Owner: "codex@unknown-age", Task: "年龄未知"},
	} {
		if err := e.Store.SaveWIP(w); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name string
		args TaskArgs
	}{
		{name: "other action", args: TaskArgs{Action: "update", Owner: "codex@old", Reason: "不允许"}},
		{name: "empty reason", args: TaskArgs{Action: "complete", Owner: "codex@old", Reason: "  "}},
		{name: "missing target", args: TaskArgs{Action: "abandon", Owner: "codex@missing", Reason: "不存在"}},
		{name: "recent target", args: TaskArgs{Action: "complete", Owner: "codex@recent", Reason: "太早"}},
		{name: "explicit derived owner still needs stale reason", args: TaskArgs{Action: "complete", Owner: "codex@auditor"}},
		{name: "unknown age", args: TaskArgs{Action: "abandon", Owner: "codex@unknown-age", Reason: "年龄不明"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := snapshotTaskTruth(t, repo)
			if _, err := e.Task(tc.args, "auditor", "codex"); err == nil {
				t.Fatalf("%s 应拒绝", tc.name)
			}
			after := snapshotTaskTruth(t, repo)
			if after != before {
				t.Fatalf("拒绝路径写了 WIP/journal\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

func TestTaskConcurrentCrossSessionCompleteHasSingleWinner(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"a.go": "package p\n\nfunc A() {}\n"})
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	e.now = func() time.Time { return now }
	const target = "codex@stale-race"
	if err := e.Store.SaveWIP(model.WIP{
		Owner: target, Task: "并发收口", Touching: []string{"a.go#A"}, Updated: now.Add(-8 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	const callers = 6
	start := make(chan struct{})
	results := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := e.Task(TaskArgs{
				Action: "complete", Owner: target, Reason: "并发审计确认 commit abc 已完成",
			}, "auditor", "codex")
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("并发代收口成功数=%d, want 1", successes)
	}
	changes, _, err := e.Store.LoadJournal()
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 {
		t.Fatalf("并发代收口 journal=%d, want 1", len(changes))
	}
}

func snapshotTaskTruth(t *testing.T, repo string) string {
	t.Helper()
	var records []string
	for _, subdir := range []string{"wip", "journal"} {
		root := filepath.Join(repo, ".knowledge", subdir)
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(filepath.Join(repo, ".knowledge"), path)
			if err != nil {
				return err
			}
			records = append(records, filepath.ToSlash(rel)+"\x00"+string(data))
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	sort.Strings(records)
	return strings.Join(records, "\n")
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
