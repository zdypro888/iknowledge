package main

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zdypro888/iknowledge/internal/engine"
	"github.com/zdypro888/iknowledge/internal/mcpserv"
)

// startServe 在随机端口起真实 MCP 服务,并把端口写进 .knowledge/config.yaml
// (hook 桥从 config 读端口,测试必须让两者一致)。
func startServe(t *testing.T, repo string, e *engine.Engine) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	cfg := fmt.Sprintf("schema: 1\nport: %d\n", port)
	if err := os.WriteFile(filepath.Join(repo, ".knowledge", "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mcpserv.New(e).Handler()}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return port
}

// TestHookBridge:hook 桥全链路——stdin 事件 → /inject → additionalContext。
func TestHookBridge(t *testing.T) {
	repo := setupGitRepo(t)
	e, _ := initRepo(t, repo, engine.InitOptions{})
	// 预埋一条知识,注入文本才有可辨识内容。
	if _, err := e.Remember(engine.RememberArgs{
		Node:    "internal/auth/login.go#Login",
		Entries: []engine.RememberEntry{{Kind: "usage", Text: "pass 传明文,内部做校验,调用方不要预加密"}},
	}, "seed-session", "tester"); err != nil {
		t.Fatal(err)
	}
	startServe(t, repo, e)

	absLogin := filepath.Join(repo, "internal", "auth", "login.go")
	event := func(file string) string {
		return fmt.Sprintf(`{"session_id":"hooktest","cwd":%q,"hook_event_name":"PostToolUse",`+
			`"tool_name":"Read","tool_input":{"file_path":%q}}`, repo, file)
	}

	tests := []struct {
		name     string
		args     []string
		stdin    string
		contains []string // 全部须出现;空切片表示输出必须为空
	}{
		{"命中:显式 --repo + 绝对路径", []string{"--repo", repo}, event(absLogin),
			[]string{"additionalContext", "PostToolUse", "调用方不要预加密"}},
		{"命中:靠 cwd 向上找仓库", nil, event(absLogin),
			[]string{"additionalContext", "调用方不要预加密"}},
		{"静默:文件无节点(排除段)", []string{"--repo", repo}, event(filepath.Join(repo, "vendor", "dep", "dep.go")), nil},
		{"静默:事件缺 file_path", []string{"--repo", repo}, `{"session_id":"x","tool_input":{}}`, nil},
		{"静默:stdin 不是 JSON", []string{"--repo", repo}, "not-json", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			if code := runHook(tt.args, strings.NewReader(tt.stdin), &out); code != 0 {
				t.Fatalf("hook 必须永远退出 0(增强不阻塞),实得 %d", code)
			}
			if len(tt.contains) == 0 {
				if out.Len() != 0 {
					t.Errorf("应静默无输出,实得:%s", out.String())
				}
				return
			}
			for _, want := range tt.contains {
				if !strings.Contains(out.String(), want) {
					t.Errorf("输出缺 %q:\n%s", want, out.String())
				}
			}
		})
	}
}

// TestHookBridgeAuth:serve --auth 时 hook 桥自动携带 token(闭环)。
func TestHookBridgeAuth(t *testing.T) {
	repo := setupGitRepo(t)
	e, _ := initRepo(t, repo, engine.InitOptions{})
	if _, err := e.Remember(engine.RememberArgs{
		Node:    "internal/auth/login.go#Login",
		Entries: []engine.RememberEntry{{Kind: "usage", Text: "pass 传明文,内部做校验"}},
	}, "seed", "tester"); err != nil {
		t.Fatal(err)
	}
	tok, err := e.Store.EnsureAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := os.WriteFile(filepath.Join(repo, ".knowledge", "config.yaml"),
		fmt.Appendf(nil, "schema: 1\nport: %d\n", port), 0o644); err != nil {
		t.Fatal(err)
	}
	msrv := mcpserv.New(e)
	msrv.AuthToken = tok
	srv := &http.Server{Handler: msrv.Handler()}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	stdin := fmt.Sprintf(`{"session_id":"x","cwd":%q,"tool_input":{"file_path":%q}}`,
		repo, filepath.Join(repo, "internal", "auth", "login.go"))
	var out bytes.Buffer
	if code := runHook([]string{"--repo", repo}, strings.NewReader(stdin), &out); code != 0 {
		t.Fatalf("hook 退出码 %d", code)
	}
	if !strings.Contains(out.String(), "pass 传明文") {
		t.Errorf("鉴权下 hook 注入失败(应自动带 token):%s", out.String())
	}
}

// TestHookServeDown:serve 未启动时 hook 必须静默退出 0(纪律第 0 条)。
func TestHookServeDown(t *testing.T) {
	repo := setupGitRepo(t)
	initRepo(t, repo, engine.InitOptions{})
	// 占个端口拿到"当前必然无人监听"的端口号,再关掉写进 config。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	if err := os.WriteFile(filepath.Join(repo, ".knowledge", "config.yaml"),
		fmt.Appendf(nil, "schema: 1\nport: %d\n", port), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	stdin := fmt.Sprintf(`{"session_id":"x","cwd":%q,"tool_input":{"file_path":%q}}`,
		repo, filepath.Join(repo, "main.go"))
	if code := runHook([]string{"--repo", repo}, strings.NewReader(stdin), &out); code != 0 || out.Len() != 0 {
		t.Errorf("serve 未启动应静默退出 0,实得 code=%d out=%s", code, out.String())
	}
}

// TestMaintainCLI:maintain 只读打印欠账清单(空库:无欠账;不加写者锁,serve 期可用)。
func TestMaintainCLI(t *testing.T) {
	repo := setupGitRepo(t)
	initRepo(t, repo, engine.InitOptions{})
	var out bytes.Buffer
	if code := runMaintain([]string{"--repo", repo}, &out); code != 0 {
		t.Fatalf("maintain 退出码 %d", code)
	}
	if !strings.Contains(out.String(), "无维护欠账") {
		t.Errorf("空库应报无欠账,实得:%s", out.String())
	}
}

// TestSetupPrints:setup 打印三件套且只打印(不写 .knowledge/ 之外任何文件)。
func TestSetupPrints(t *testing.T) {
	repo := setupGitRepo(t)
	e, _ := initRepo(t, repo, engine.InitOptions{})
	if _, err := e.Store.EnsureConfig(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := runSetup([]string{"--repo", repo}, &out); code != 0 {
		t.Fatalf("setup 退出码 %d", code)
	}
	for _, want := range []string{
		"mcpServers", "/mcp/main?repo=",
		"本仓库配有 knowledge MCP", // 纪律段(engine.DisciplinePrompt 首行)
		"PostToolUse", "iknowledge hook --repo",
		"[mcp_servers.knowledge]", "AGENTS.md", // Codex 段(config.toml + 纪律载体)
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("setup 输出缺 %q", want)
		}
	}
	// 只打印不代写:目标仓库不得出现 .mcp.json / .claude / CLAUDE.md。
	for _, f := range []string{".mcp.json", ".claude", "CLAUDE.md"} {
		if _, err := os.Stat(filepath.Join(repo, f)); !os.IsNotExist(err) {
			t.Errorf("setup 不得代写 %s(铁律二)", f)
		}
	}
}

func TestSetupPrintsCodexAuthHeaders(t *testing.T) {
	repo := setupGitRepo(t)
	e, _ := initRepo(t, repo, engine.InitOptions{})
	if _, err := e.Store.EnsureConfig(); err != nil {
		t.Fatal(err)
	}
	tok, err := e.Store.EnsureAuthToken()
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if code := runSetup([]string{"--repo", repo}, &out); code != 0 {
		t.Fatalf("setup 退出码 %d", code)
	}
	got := out.String()
	for _, want := range []string{
		`"headers": { "Authorization": "Bearer ` + tok,
		"[mcp_servers.knowledge.http_headers]",
		`Authorization = "Bearer ` + tok + `"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("auth setup 输出缺 %q\n%s", want, got)
		}
	}
}
