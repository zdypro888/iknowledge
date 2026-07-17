package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zdypro888/iknowledge/internal/engine"
)

func TestPrecheckCLIWarnOnlyAndStrict(t *testing.T) {
	repo := setupGitRepo(t) // fixture 已在暂存区,尚无 journal 记账。
	initRepo(t, repo, engine.InitOptions{})

	var out bytes.Buffer
	if code := runPrecheck([]string{"--repo", repo}, &out); code != 0 {
		t.Fatalf("缺省仅告警应退出 0,实得 %d\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "unaccounted-change") || !strings.Contains(out.String(), "默认仅告警") {
		t.Fatalf("缺省报告不完整:\n%s", out.String())
	}
	out.Reset()
	if code := runPrecheck([]string{"--repo", repo, "--strict"}, &out); code != 1 {
		t.Fatalf("strict 有阻断项应退出 1,实得 %d\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "[BLOCK]") {
		t.Fatalf("strict 报告缺阻断项:\n%s", out.String())
	}
}

func TestGitPrecheckFilesWorkingIncludesUntracked(t *testing.T) {
	repo := setupGitRepo(t)
	name := "new untracked.go"
	if err := os.WriteFile(filepath.Join(repo, name), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := gitPrecheckFiles(repo, true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, file := range files {
		if filepath.ToSlash(file) == name {
			found = true
		}
	}
	if !found {
		t.Fatalf("--working 未包含未跟踪文件 %q: %v", name, files)
	}
}

func TestGitPrecheckFilesIncludesStagedDeletion(t *testing.T) {
	repo := setupGitRepo(t)
	gitTestCommand(t, repo, "config", "user.email", "iknowledge-test@example.invalid")
	gitTestCommand(t, repo, "config", "user.name", "iknowledge test")
	gitTestCommand(t, repo, "commit", "-q", "-m", "baseline")
	if err := os.Remove(filepath.Join(repo, "internal", "auth", "login.go")); err != nil {
		t.Fatal(err)
	}
	gitTestCommand(t, repo, "add", "-u")
	files, err := gitPrecheckFiles(repo, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(files, "internal/auth/login.go") {
		t.Fatalf("暂存删除未进入 precheck:%v", files)
	}
	deleted, err := gitPrecheckDeletedFiles(repo, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(deleted, "internal/auth/login.go") {
		t.Fatalf("暂存删除状态未传给 engine:%v", deleted)
	}
}

func TestPrecheckCLIRejectsUnrelatedJournal(t *testing.T) {
	repo := setupGitRepo(t) // fixture 源码都在 index 中。
	initRepo(t, repo, engine.InitOptions{})
	journal := filepath.Join(repo, ".knowledge", "journal", "2099-01.jsonl")
	if err := os.MkdirAll(filepath.Dir(journal), 0o755); err != nil {
		t.Fatal(err)
	}
	// 只给 main.go 记账,不能让 internal/auth/login.go 一并假通过。
	if err := os.WriteFile(journal, []byte(`{"id":"chg_20990101T000000Z_0000000000000001","nodes":["main.go#run"],"at":"2099-01-01T00:00:00Z","what":"main 调整","why":"测试无关记录"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTestCommand(t, repo, "add", ".knowledge/journal/2099-01.jsonl")

	var out bytes.Buffer
	if code := runPrecheck([]string{"--repo", repo}, &out); code != 0 {
		t.Fatalf("缺省仍应只告警,实得 %d\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "internal/auth/login.go [unaccounted-change]") {
		t.Fatalf("无关 journal 造成假通过:\n%s", out.String())
	}
}

func TestGitPrecheckJournalNodesReadsIndexAdditions(t *testing.T) {
	repo := setupGitRepo(t)
	initRepo(t, repo, engine.InitOptions{})
	journal := filepath.Join(repo, ".knowledge", "journal", "2099-02.jsonl")
	if err := os.MkdirAll(filepath.Dir(journal), 0o755); err != nil {
		t.Fatal(err)
	}
	data := `{"id":"chg_fake","nodes":["other.go#Other"],"what":"畸形伪记录","why":"缺真实 ID/时间"}` + "\n" +
		`{"id":"chg_20990201T000000Z_0000000000000002","nodes":["internal/auth/login.go#Login"],"at":"2099-02-01T00:00:00Z","what":"登录调整","why":"测试新增记录"}` + "\n"
	if err := os.WriteFile(journal, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTestCommand(t, repo, "add", ".knowledge/journal/2099-02.jsonl")
	files, err := gitPrecheckFiles(repo, false)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := gitPrecheckJournalNodes(repo, false, files)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0] != "internal/auth/login.go#Login" {
		t.Fatalf("新增 journal nodes=%v", nodes)
	}
}

func TestPrecheckCLIAcceptsMatchingRecordChange(t *testing.T) {
	repo := setupGitRepo(t)
	e, _ := initRepo(t, repo, engine.InitOptions{})
	gitTestCommand(t, repo, "config", "user.email", "iknowledge-test@example.invalid")
	gitTestCommand(t, repo, "config", "user.name", "iknowledge test")
	gitTestCommand(t, repo, "add", ".")
	gitTestCommand(t, repo, "commit", "-q", "-m", "baseline")

	login := filepath.Join(repo, "internal", "auth", "login.go")
	data, err := os.ReadFile(login)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(login, []byte(strings.Replace(string(data), "return errEmpty", "return nil", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RecordChange(engine.ChangeArgs{
		Nodes: []string{"internal/auth/login.go#Login"},
		What:  "统一空用户返回路径",
		Why:   "验证 precheck 对应节点记账",
	}, "precheck-e2e", "tester"); err != nil {
		t.Fatal(err)
	}
	gitTestCommand(t, repo, "add", ".")

	var out bytes.Buffer
	if code := runPrecheck([]string{"--repo", repo}, &out); code != 0 {
		t.Fatalf("对应记录缺省应通过告警模式,实得 %d\n%s", code, out.String())
	}
	if strings.Contains(out.String(), "unaccounted-change") {
		t.Fatalf("对应 record_change 未覆盖源码:\n%s", out.String())
	}
}

func TestPrecheckCLIAcceptsAccountedDeletion(t *testing.T) {
	repo := setupGitRepo(t)
	e, _ := initRepo(t, repo, engine.InitOptions{})
	gitTestCommand(t, repo, "config", "user.email", "iknowledge-test@example.invalid")
	gitTestCommand(t, repo, "config", "user.name", "iknowledge test")
	gitTestCommand(t, repo, "add", ".")
	gitTestCommand(t, repo, "commit", "-q", "-m", "baseline")

	if err := os.Remove(filepath.Join(repo, "internal", "auth", "login.go")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RecordChange(engine.ChangeArgs{
		Nodes: []string{"internal/auth/login.go#Login"},
		What:  "删除旧登录入口",
		Why:   "调用方已迁移",
	}, "precheck-delete", "tester"); err != nil {
		t.Fatal(err)
	}
	gitTestCommand(t, repo, "add", "-A")

	var out bytes.Buffer
	if code := runPrecheck([]string{"--repo", repo, "--strict"}, &out); code != 0 {
		t.Fatalf("已记账删除不应永久阻断 strict,实得 %d\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "deleted-source") || strings.Contains(out.String(), "unaccounted-change") {
		t.Fatalf("删除预检结果不完整:\n%s", out.String())
	}
}

func TestGitPrecheckJournalNodesIgnoresEditedHeadRecord(t *testing.T) {
	repo := setupGitRepo(t)
	initRepo(t, repo, engine.InitOptions{})
	journal := filepath.Join(repo, ".knowledge", "journal", "2099-03.jsonl")
	if err := os.MkdirAll(filepath.Dir(journal), 0o755); err != nil {
		t.Fatal(err)
	}
	base := `{"id":"chg_20990301T000000Z_0000000000000003","nodes":["internal/auth/login.go#Login"],"at":"2099-03-01T00:00:00Z","what":"旧记录","why":"已在 HEAD"}` + "\n"
	if err := os.WriteFile(journal, []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTestCommand(t, repo, "config", "user.email", "iknowledge-test@example.invalid")
	gitTestCommand(t, repo, "config", "user.name", "iknowledge test")
	gitTestCommand(t, repo, "add", ".")
	gitTestCommand(t, repo, "commit", "-q", "-m", "baseline with journal")

	edited := strings.Replace(base, "旧记录", "篡改旧记录", 1)
	if err := os.WriteFile(journal, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTestCommand(t, repo, "add", ".knowledge/journal/2099-03.jsonl")
	files, err := gitPrecheckFiles(repo, false)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := gitPrecheckJournalNodes(repo, false, files)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 0 {
		t.Fatalf("改写既有 ID 不能冒充追加记账:%v", nodes)
	}
}

func gitTestCommand(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, out)
	}
}

func TestShellQuote(t *testing.T) {
	got := shellQuote("/tmp/a b/it's")
	if got != `'/tmp/a b/it'"'"'s'` {
		t.Fatalf("shellQuote=%q", got)
	}
}

func TestBriefCLI(t *testing.T) {
	repo := setupGitRepo(t)
	initRepo(t, repo, engine.InitOptions{})
	var out bytes.Buffer
	if code := runBrief([]string{"--repo", repo, "--budget", "300"}, &out); code != 0 {
		t.Fatalf("brief 退出码 %d", code)
	}
	if !strings.Contains(out.String(), "# iknowledge briefing") || !strings.Contains(out.String(), "源码永远优先") {
		t.Fatalf("brief 输出不完整:\n%s", out.String())
	}
}
