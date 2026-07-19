package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zdypro888/iknowledge/internal/engine"
	"github.com/zdypro888/iknowledge/internal/store"
)

func semanticBudgetTestRepos(t *testing.T, count int) []string {
	t.Helper()
	repos := make([]string, count)
	for i := range repos {
		repos[i] = setupGitRepo(t)
		initRepo(t, repos[i], engine.InitOptions{})
	}
	// setupGitRepo 隔离单仓测试 state；多仓预算必须在同一个进程 state root
	// 下写入所有 canonical repo 配置。
	t.Setenv("IKNOWLEDGE_STATE_HOME", t.TempDir())
	return repos
}

func saveSemanticBudgetConfig(t *testing.T, repos []string, enabled bool) {
	t.Helper()
	for _, repo := range repos {
		s, err := store.Open(repo)
		if err != nil {
			t.Fatal(err)
		}
		cfg := engine.DefaultSemanticSettings()
		cfg.Enabled = enabled
		if enabled {
			cfg.Endpoint = "http://127.0.0.1:11434/v1"
			cfg.Model = "serve-budget-test"
		}
		cfg.MaxVectorMiB = 512
		if err := engine.SaveSemanticSettings(s, cfg); err != nil {
			t.Fatal(err)
		}
	}
}

func TestServePreflightSemanticResidentBudget(t *testing.T) {
	t.Run("enabled-at-cap", func(t *testing.T) {
		repos := semanticBudgetTestRepos(t, 2)
		saveSemanticBudgetConfig(t, repos, true)
		stores, err := preflightServeRepos(repos)
		if err != nil || len(stores) != len(repos) {
			t.Fatalf("enabled at cap stores=%d err=%v", len(stores), err)
		}
	})

	t.Run("disabled-does-not-count", func(t *testing.T) {
		repos := semanticBudgetTestRepos(t, 3)
		saveSemanticBudgetConfig(t, repos, false) // 每仓 max=512，但 enabled=false。
		stores, err := preflightServeRepos(repos)
		if err != nil || len(stores) != len(repos) {
			t.Fatalf("disabled semantic stores=%d err=%v", len(stores), err)
		}
	})
}

func TestServeRejectsSemanticResidentBudgetBeforeListening(t *testing.T) {
	repos := semanticBudgetTestRepos(t, 3)
	ports := make([]int, len(repos))
	for i, repo := range repos {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ports[i] = ln.Addr().(*net.TCPAddr).Port
		if err := ln.Close(); err != nil {
			t.Fatal(err)
		}
		cfg := fmt.Sprintf("schema: 1\nport: %d\n", ports[i])
		if err := os.WriteFile(filepath.Join(repo, ".knowledge", "config.yaml"), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	saveSemanticBudgetConfig(t, repos, true) // 3×512MiB > 1024MiB。
	if code := runServe([]string{"--repo", repos[0], "--repo", repos[1], "--repo", repos[2]}); code != 1 {
		t.Fatalf("over-budget serve code=%d, want 1", code)
	}
	// runServe 已返回且每个计划端口都仍可立即绑定，证明预算拒绝发生在
	// 创建任何 listener 之前，而不是先半启动前仓再在后仓失败。
	for _, port := range ports {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			t.Fatalf("port %d was touched before budget rejection: %v", port, err)
		}
		if err := ln.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestServePreflightReportsCorruptSemanticConfigWithRepo(t *testing.T) {
	repo := setupGitRepo(t)
	e, _ := initRepo(t, repo, engine.InitOptions{})
	if err := e.Store.WriteSemanticConfig([]byte("{")); err != nil {
		t.Fatal(err)
	}
	if _, err := preflightServeRepos([]string{repo}); err == nil ||
		!strings.Contains(err.Error(), "semantic 配置损坏") || !strings.Contains(err.Error(), e.Store.RepoRoot()) {
		t.Fatalf("corrupt semantic config error=%v", err)
	}
}
