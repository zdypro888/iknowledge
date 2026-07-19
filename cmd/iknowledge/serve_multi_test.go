//go:build !windows

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zdypro888/iknowledge/internal/engine"
	"github.com/zdypro888/iknowledge/internal/store"
)

func TestLoopbackDialAddrNormalizesWildcard(t *testing.T) {
	tests := map[string]string{
		"0.0.0.0:18000": "127.0.0.1:18000",
		":18000":        "127.0.0.1:18000",
		"[::]:18000":    "[::1]:18000",
		"127.0.0.1:9":   "127.0.0.1:9",
		"[::1]:9":       "[::1]:9",
	}
	for input, want := range tests {
		if got := loopbackDialAddr(input); got != want {
			t.Errorf("loopbackDialAddr(%q)=%q, want %q", input, got, want)
		}
	}
}

// TestServeMultiRepo:多 repo 单守护 e2e(impl §1 修订)——一个 runServe 进程
// 服务两个 enabled semantic 仓库，resident 上限合计恰为 1024MiB；
// 各端口各答各的 repoRoot，SIGINT 优雅停机退出 0。
func TestServeMultiRepo(t *testing.T) {
	repos := make([]string, 2)
	ports := make([]int, 2)
	for i := range repos {
		repo := setupGitRepo(t)
		initRepo(t, repo, engine.InitOptions{})
		// 抢个空闲端口写进 config(DerivePort 的哈希端口在测试机可能被占)。
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
		repos[i] = repo
	}
	// setupGitRepo 会为每次调用隔离 state home；多仓进程必须共享一个
	// process state root，故所有仓准备完后统一切换并写各自 canonical 配置。
	t.Setenv("IKNOWLEDGE_STATE_HOME", t.TempDir())
	for _, repo := range repos {
		s, err := store.Open(repo)
		if err != nil {
			t.Fatal(err)
		}
		semanticCfg := engine.DefaultSemanticSettings()
		semanticCfg.Enabled = true
		semanticCfg.Endpoint = "http://127.0.0.1:11434/v1"
		semanticCfg.Model = "serve-budget-test"
		semanticCfg.MaxVectorMiB = 512
		if err := engine.SaveSemanticSettings(s, semanticCfg); err != nil {
			t.Fatal(err)
		}
	}

	code := make(chan int, 1)
	go func() { code <- runServe([]string{"--repo", repos[0], "--repo", repos[1]}) }()

	// 两个端口都答出各自 repoRoot。
	for i, repo := range repos {
		var repoRoot string
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) && repoRoot == "" {
			resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/mcp/main", ports[i]),
				"application/json",
				strings.NewReader(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"clientInfo":{"name":"test"}}}`))
			if err != nil {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			var out struct {
				Result struct {
					RepoRoot string `json:"repoRoot"`
				} `json:"result"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				_ = resp.Body.Close()
				t.Fatal(err)
			}
			if err := resp.Body.Close(); err != nil {
				t.Fatal(err)
			}
			repoRoot = out.Result.RepoRoot
		}
		// macOS 的 TempDir 有 /private 前缀差异,按后缀比对。
		if repoRoot == "" || !strings.HasSuffix(repoRoot, strings.TrimPrefix(repo, "/private")) {
			t.Errorf("端口 %d 的 repoRoot = %q, want 后缀 %q", ports[i], repoRoot, repo)
		}
	}

	// SIGINT 优雅停机(runServe 的 NotifyContext 捕获,测试进程不受影响)。
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-code:
		if c != 0 {
			t.Errorf("优雅停机退出码 = %d, want 0", c)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("停机超时")
	}
}
