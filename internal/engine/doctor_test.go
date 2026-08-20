package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zdypro888/iknowledge/internal/model"
)

func TestParserHealthAndDoctor(t *testing.T) {
	e, _ := initEngine(t, map[string]string{
		"a.go": "package a\n\nfunc F() {}\n",
		"b.ts": "export function G() { return 1 }\n",
	})
	ph, err := e.ParserHealth()
	if err != nil {
		t.Fatal(err)
	}
	if ph.Files != 2 || ph.Symbols == 0 {
		t.Fatalf("parser health 不完整:%+v", ph)
	}
	if ph.ByLang["go"].Files != 1 || ph.ByLang["typescript"].Files != 1 {
		t.Fatalf("语言统计不对:%+v", ph.ByLang)
	}
	dr, err := e.Doctor()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dr.Text(), "parser: files=2") {
		t.Fatalf("doctor 未包含 parser 仪表盘:\n%s", dr.Text())
	}
}

func TestDoctorDetectsOuterKnowledgeIgnore(t *testing.T) {
	e, repo := initEngine(t, map[string]string{"a.go": "package a\n\nfunc F() {}\n"})
	initGitForDoctorTest(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".knowledge/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := e.Doctor()
	if err != nil {
		t.Fatal(err)
	}
	if rep.TruthGit.State != "ignored" || rep.TruthGit.protected() {
		t.Fatalf("outer ignore 未被识别: %+v", rep.TruthGit)
	}
	if !strings.Contains(rep.Text(), "知识正本被 Git ignore") {
		t.Fatalf("doctor 未暴露正本丢失风险:\n%s", rep.Text())
	}
}

func TestDoctorRecognizesTrackedKnowledgeTruth(t *testing.T) {
	e, repo := initEngine(t, map[string]string{"a.go": "package a\n\nfunc F() {}\n"})
	initGitForDoctorTest(t, repo)
	files, err := durableKnowledgeFiles(e.Store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"-C", repo, "add", "--"}
	for _, file := range files {
		rel, err := filepath.Rel(repo, file)
		if err != nil {
			t.Fatal(err)
		}
		args = append(args, rel)
	}
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		t.Fatalf("git add knowledge truth: %v: %s", err, out)
	}
	rep, err := e.Doctor()
	if err != nil {
		t.Fatal(err)
	}
	if !rep.TruthGit.protected() {
		t.Fatalf("已跟踪正本应健康: %+v", rep.TruthGit)
	}
}

func initGitForDoctorTest(t *testing.T, repo string) {
	t.Helper()
	if out, err := exec.Command("git", "-C", repo, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
}

func TestSessionGateWarnsUndigestedRepeatedReads(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"a.go": "package a\n\nfunc F() {}\n"})
	sid := "session-gate"
	for range 2 {
		if _, _, err := e.Recall(RecallArgs{Query: "a.go#F"}, sid); err != nil {
			t.Fatal(err)
		}
	}
	out, err := e.Session(sid, "gate")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "多次读取但未沉淀") {
		t.Fatalf("gate 未提醒沉淀:\n%s", out)
	}
}

func TestSessionGateRequiresActiveWIPClosure(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"a.go": "package a\n\nfunc F() {}\n"})
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	e.now = func() time.Time { return now }
	if err := e.Store.SaveWIP(model.WIP{
		Owner: "codex@session-wip", Task: "旧任务", Updated: now.Add(-8 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	out, err := e.Session("session-wip", "gate")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "过期 WIP") || !strings.Contains(out, "complete") || !strings.Contains(out, "abandon") {
		t.Fatalf("gate 未要求收口过期 WIP:\n%s", out)
	}
}
