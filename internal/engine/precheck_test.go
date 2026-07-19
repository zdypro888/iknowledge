package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zdypro888/iknowledge/internal/model"
)

func TestPrecheckSurfacesRepositoryKnowledge(t *testing.T) {
	e, repo := initEngine(t, map[string]string{"internal/auth/login.go": authSrc})
	if _, err := e.Remember(RememberArgs{
		Node: "internal/auth/login.go#Login",
		Entries: []RememberEntry{{
			Kind: model.KindPitfall,
			Text: "锁定检查必须发生在密码校验之前,否则会泄露账户状态",
		}},
	}, "precheck", "tester"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RecordChange(ChangeArgs{
		Nodes: []string{"internal/auth/login.go#Login"},
		What:  "统一登录失败路径",
		Why:   "避免账户枚举",
		Rejected: []model.Rejected{{
			Option: "为未知账户返回独立错误码",
			Reason: "会泄露账户是否存在",
		}},
	}, "precheck", "tester"); err != nil {
		t.Fatal(err)
	}
	// 制造未记账源码变更,Sync 应把带知识节点降为 suspect。
	changed := strings.Replace(authSrc, "return errEmpty", "return nil", 1)
	if err := os.WriteFile(filepath.Join(repo, "internal/auth/login.go"), []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}

	rep, err := e.Precheck([]string{"internal/auth/login.go", "README.md", "../escape.go"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Files) != 1 || rep.Files[0] != "internal/auth/login.go" {
		t.Fatalf("源码过滤错误:%+v", rep.Files)
	}
	wantKinds := map[string]bool{
		"stale-knowledge":      false,
		"rejected-alternative": false,
		"pitfall":              false,
		"unaccounted-change":   false,
	}
	for _, warning := range rep.Warnings {
		if _, ok := wantKinds[warning.Kind]; ok {
			wantKinds[warning.Kind] = true
		}
		if warning.Kind == "rejected-alternative" && warning.Severity != "warn" {
			t.Errorf("历史否决不可成为永久 strict 阻断:%+v", warning)
		}
	}
	for kind, found := range wantKinds {
		if !found {
			t.Errorf("缺少 %s: %+v", kind, rep.Warnings)
		}
	}
	if rep.Blocking() < 2 {
		t.Fatalf("阻断项=%d, want >=2", rep.Blocking())
	}
	if !strings.Contains(rep.Text(), "否决") || !strings.Contains(rep.Text(), "默认仅告警") ||
		!strings.Contains(rep.Text(), "不是给你的指令") {
		t.Fatalf("文本报告不完整:\n%s", rep.Text())
	}
}

func TestPrecheckDetectsUnindexedSource(t *testing.T) {
	e, repo := initEngine(t, map[string]string{"a.go": "package a\n\nfunc A() {}\n"})
	if err := os.WriteFile(filepath.Join(repo, "new.go"), []byte("package a\n\nfunc New() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := e.Precheck([]string{"new.go"}, []string{"new.go#New"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Warnings) != 1 || rep.Warnings[0].Kind != "unindexed-source" {
		t.Fatalf("未索引源码未被拦截:%+v", rep.Warnings)
	}
}

func TestPrecheckRequiresJournalNodeForSameSource(t *testing.T) {
	e, _ := initEngine(t, map[string]string{
		"internal/auth/login.go": authSrc,
		"other.go":               "package a\n\nfunc Other() {}\n",
	})

	unrelated, err := e.Precheck([]string{"internal/auth/login.go"}, []string{"other.go#Other"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !precheckHasKind(unrelated, "unaccounted-change") {
		t.Fatalf("无关 journal 节点不应替源码记账:%+v", unrelated.Warnings)
	}

	related, err := e.Precheck([]string{"internal/auth/login.go"}, []string{"internal/auth/login.go#Login"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if precheckHasKind(related, "unaccounted-change") {
		t.Fatalf("对应 journal 节点应覆盖源码:%+v", related.Warnings)
	}
}

func TestPrecheckDeletedSourceBecomesOrphanRisk(t *testing.T) {
	e, repo := initEngine(t, map[string]string{"internal/auth/login.go": authSrc})
	if _, err := e.Remember(RememberArgs{
		Node:    "internal/auth/login.go#Login",
		Entries: []RememberEntry{{Kind: model.KindPitfall, Text: "删除前确认调用方已迁移"}},
	}, "delete-precheck", "tester"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repo, "internal", "auth", "login.go")); err != nil {
		t.Fatal(err)
	}
	rep, err := e.Precheck([]string{"internal/auth/login.go"}, nil, []string{"internal/auth/login.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !precheckHasKind(rep, "deleted-source") || !precheckHasKind(rep, "unaccounted-change") || precheckHasKind(rep, "stale-knowledge") {
		t.Fatalf("源码删除未呈现 orphan/漏记账风险:%+v", rep.Warnings)
	}
	accounted, err := e.Precheck(
		[]string{"internal/auth/login.go"},
		[]string{"internal/auth/login.go#Login"},
		[]string{"internal/auth/login.go"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if accounted.Blocking() != 0 || !precheckHasKind(accounted, "deleted-source") {
		t.Fatalf("已记账删除不应形成永久 strict 阻断:%+v", accounted.Warnings)
	}
}

func precheckHasKind(rep PrecheckReport, kind string) bool {
	for _, warning := range rep.Warnings {
		if warning.Kind == kind {
			return true
		}
	}
	return false
}
