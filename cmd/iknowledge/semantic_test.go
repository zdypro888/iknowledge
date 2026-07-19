package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zdypro888/iknowledge/internal/engine"
	"github.com/zdypro888/iknowledge/internal/store"
)

func semanticCLIRepo(t *testing.T) (string, *store.Store) {
	t.Helper()
	t.Setenv("IKNOWLEDGE_STATE_HOME", t.TempDir())
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.New(s).Init(engine.InitOptions{}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return repo, s
}

func runSemanticForTest(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := runSemantic(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestSemanticConfigurePersistsWithoutCallingProvider(t *testing.T) {
	repo, s := semanticCLIRepo(t)
	secret := "sk-cli-must-not-leak"
	t.Setenv(engine.SemanticAPIKeyEnv, secret)
	code, out, errOut := runSemanticForTest(t,
		"configure", "--repo", repo,
		"--endpoint", "http://127.0.0.1:1/v1",
		"--model", "test-embedding",
		"--dimensions", "768",
		"--revision", "local-r1",
		"--top-k", "7",
		"--min-score", "0.42",
		"--max-vector-mib", "32",
		"--timeout", "5",
	)
	if code != 0 {
		t.Fatalf("configure code=%d stderr=%q", code, errOut)
	}
	if !strings.Contains(out, "未调用 embedding 服务") || !strings.Contains(out, engine.SemanticAPIKeyEnv) {
		t.Fatalf("configure 未明示费用/密钥边界: %q", out)
	}
	if strings.Contains(out, secret) || strings.Contains(errOut, secret) {
		t.Fatal("CLI 回执泄露 API key")
	}
	cfg, err := engine.LoadSemanticSettings(s)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled || cfg.Endpoint != "http://127.0.0.1:1/v1" || cfg.Model != "test-embedding" ||
		cfg.Dimensions != 768 || cfg.Revision != "local-r1" || cfg.TopK != 7 ||
		cfg.MinScore != 0.42 || cfg.MaxVectorMiB != 32 || cfg.TimeoutSec != 5 {
		t.Fatalf("configure 落盘不完整: %+v", cfg)
	}
	if _, err := s.ReadKnowledgeFile("local/vector.idx"); !os.IsNotExist(err) {
		t.Fatalf("configure 不得自动重建索引: %v", err)
	}
}

func TestSemanticConfigureUpdatesOnlyVisitedFlags(t *testing.T) {
	repo, s := semanticCLIRepo(t)
	code, _, errOut := runSemanticForTest(t,
		"configure", "--repo", repo,
		"--endpoint", "https://embedding.example.com/v1",
		"--model", "model-a", "--dimensions", "1024", "--revision", "r1",
	)
	if code != 0 {
		t.Fatalf("initial configure code=%d stderr=%q", code, errOut)
	}
	code, _, errOut = runSemanticForTest(t,
		"configure", "--repo", repo, "--model", "model-b", "--revision", "",
	)
	if code != 0 {
		t.Fatalf("update configure code=%d stderr=%q", code, errOut)
	}
	cfg, err := engine.LoadSemanticSettings(s)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Endpoint != "https://embedding.example.com/v1" || cfg.Model != "model-b" || cfg.Dimensions != 1024 || cfg.Revision != "" {
		t.Fatalf("未指定字段未被保留: %+v", cfg)
	}
}

func TestSemanticEnableDisableAndStatusStayOffline(t *testing.T) {
	repo, s := semanticCLIRepo(t)
	code, _, errOut := runSemanticForTest(t,
		"configure", "--repo", repo,
		"--endpoint", "http://127.0.0.1:1/v1", "--model", "offline-model",
	)
	if code != 0 {
		t.Fatalf("configure code=%d stderr=%q", code, errOut)
	}
	code, out, errOut := runSemanticForTest(t, "disable", "--repo", repo)
	if code != 0 || !strings.Contains(out, "已禁用") {
		t.Fatalf("disable code=%d out=%q stderr=%q", code, out, errOut)
	}
	cfg, err := engine.LoadSemanticSettings(s)
	if err != nil || cfg.Enabled {
		t.Fatalf("disable 未落盘: enabled=%v err=%v", cfg.Enabled, err)
	}
	code, out, errOut = runSemanticForTest(t, "status", "--repo", repo)
	if code != 0 || !strings.Contains(out, "semantic: disabled") {
		t.Fatalf("disabled status code=%d out=%q stderr=%q", code, out, errOut)
	}
	code, out, errOut = runSemanticForTest(t, "enable", "--repo", repo)
	if code != 0 || !strings.Contains(out, "已启用") {
		t.Fatalf("enable code=%d out=%q stderr=%q", code, out, errOut)
	}
	cfg, err = engine.LoadSemanticSettings(s)
	if err != nil || !cfg.Enabled {
		t.Fatalf("enable 未落盘: enabled=%v err=%v", cfg.Enabled, err)
	}
	// endpoint 故意指向无服务端口。status 只读本地配置/索引，仍应快速成功。
	code, out, errOut = runSemanticForTest(t, "status", "--repo", repo)
	if code != 0 || !strings.Contains(out, "semantic: enabled") || !strings.Contains(out, "index: stale/unavailable") {
		t.Fatalf("enabled status code=%d out=%q stderr=%q", code, out, errOut)
	}
}

func TestSemanticEnableRequiresConfigurationAndRebuildRequiresOptIn(t *testing.T) {
	repo, _ := semanticCLIRepo(t)
	code, _, errOut := runSemanticForTest(t, "enable", "--repo", repo)
	if code != 1 || !strings.Contains(errOut, "尚未配置") {
		t.Fatalf("unconfigured enable code=%d stderr=%q", code, errOut)
	}
	var out, rebuildErr bytes.Buffer
	code = runSemanticRebuild(context.Background(), []string{"--repo", repo}, &out, &rebuildErr)
	if code != 1 || !strings.Contains(rebuildErr.String(), "semantic 未启用") {
		t.Fatalf("disabled rebuild code=%d out=%q stderr=%q", code, out.String(), rebuildErr.String())
	}
}

func TestSemanticClearKeepsProviderConfiguration(t *testing.T) {
	repo, s := semanticCLIRepo(t)
	code, _, errOut := runSemanticForTest(t,
		"configure", "--repo", repo,
		"--endpoint", "http://127.0.0.1:1/v1", "--model", "offline-model",
	)
	if code != 0 {
		t.Fatalf("configure code=%d stderr=%q", code, errOut)
	}
	if err := s.WritePrivateKnowledgeFile("local/vector.idx", []byte("derived")); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runSemanticForTest(t, "clear", "--repo", repo)
	if code != 0 || !strings.Contains(out, "provider 配置保留") {
		t.Fatalf("clear code=%d out=%q stderr=%q", code, out, errOut)
	}
	if _, err := s.ReadKnowledgeFile("local/vector.idx"); !os.IsNotExist(err) {
		t.Fatalf("clear 未删除派生索引: %v", err)
	}
	cfg, err := engine.LoadSemanticSettings(s)
	if err != nil || !cfg.Enabled || cfg.Model != "offline-model" {
		t.Fatalf("clear 误删 provider 配置: cfg=%+v err=%v", cfg, err)
	}
	// 清理派生缓存必须幂等。
	code, _, errOut = runSemanticForTest(t, "clear", "--repo", repo)
	if code != 0 {
		t.Fatalf("second clear code=%d stderr=%q", code, errOut)
	}
}

func TestSemanticCommandUsageAndRepositoryBoundary(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want int
	}{
		{nil, 2},
		{[]string{"unknown"}, 2},
		{[]string{"help"}, 0},
		{[]string{"status", "unexpected"}, 2},
	} {
		code, _, _ := runSemanticForTest(t, tc.args...)
		if code != tc.want {
			t.Errorf("runSemantic(%v)=%d, want %d", tc.args, code, tc.want)
		}
	}
	uninitialized := t.TempDir()
	code, _, errOut := runSemanticForTest(t, "status", "--repo", uninitialized)
	if code != 1 || !strings.Contains(errOut, "库未初始化") {
		t.Fatalf("uninitialized status code=%d stderr=%q", code, errOut)
	}
}
