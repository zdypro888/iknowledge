package main

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zdypro888/iknowledge/internal/engine"
	"github.com/zdypro888/iknowledge/internal/mcpserv"
)

// TestStdioBridge:stdio 桥全链路(serve 已在线的路径)——initialize 捕获会话头、
// 后续请求回带、tools/call 正常应答、通知不产生回包。
// 自动拉起路径无法在 go test 内验(os.Executable 是测试二进制),由装机冒烟覆盖。
func TestStdioBridge(t *testing.T) {
	repo := setupGitRepo(t)
	e, _ := initRepo(t, repo, engine.InitOptions{})
	if _, err := e.Store.EnsureConfig(); err != nil {
		t.Fatal(err)
	}
	startServe(t, repo, e) // 写 config 端口并起真实 mcpserv

	stdin := strings.Join([]string{
		`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"clientInfo":{"name":"stdio-test"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"kb_status","arguments":{}}}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	if code := runStdio([]string{"--repo", repo}, strings.NewReader(stdin), &out); code != 0 {
		t.Fatalf("stdio 退出码 %d", code)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("应恰好 2 个响应(通知无回包),实得 %d:\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[0], `"repoRoot"`) || !strings.Contains(lines[0], "instructions") {
		t.Errorf("initialize 响应不完整:%s", lines[0])
	}
	// kb_status 走会话头回带后的正常路径(WRONG_SESSION 会在这里露馅)。
	if !strings.Contains(lines[1], "覆盖率") {
		t.Errorf("kb_status 响应不完整:%s", lines[1])
	}
}

func TestStdioBridgeForwardsParseError(t *testing.T) {
	repo := setupGitRepo(t)
	e, _ := initRepo(t, repo, engine.InitOptions{})
	if _, err := e.Store.EnsureConfig(); err != nil {
		t.Fatal(err)
	}
	startServe(t, repo, e)

	var out bytes.Buffer
	if code := runStdio([]string{"--repo", repo}, strings.NewReader("{broken\n"), &out); code != 0 {
		t.Fatalf("stdio 退出码 %d", code)
	}
	if !strings.Contains(out.String(), `"code":-32700`) || !strings.Contains(out.String(), `"id":null`) {
		t.Fatalf("畸形 JSON 的 parse-error 被吞掉:%s", out.String())
	}
}

func TestStdioBridgeForwardsInvalidRequest(t *testing.T) {
	repo := setupGitRepo(t)
	e, _ := initRepo(t, repo, engine.InitOptions{})
	if _, err := e.Store.EnsureConfig(); err != nil {
		t.Fatal(err)
	}
	startServe(t, repo, e)

	var out bytes.Buffer
	if code := runStdio([]string{"--repo", repo}, strings.NewReader("{}\n"), &out); code != 0 {
		t.Fatalf("stdio 退出码 %d", code)
	}
	if !strings.Contains(out.String(), `"code":-32600`) || !strings.Contains(out.String(), `"id":null`) {
		t.Fatalf("非法 request 的协议错误被吞掉:%s", out.String())
	}
}

// TestStdioBridgeAuth 回归:token 文件存在时,stdio 探活和 MCP 转发都必须携带 token。
// 旧实现会把已启用鉴权的健康服务误判为离线,等待 8 秒后失败。
func TestStdioBridgeAuth(t *testing.T) {
	repo := setupGitRepo(t)
	e, _ := initRepo(t, repo, engine.InitOptions{})
	token, err := e.Store.EnsureAuthToken()
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
	msrv.AuthToken = token
	srv := &http.Server{Handler: msrv.Handler()}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	stdin := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"clientInfo":{"name":"stdio-auth-test"}}}` + "\n"
	var out bytes.Buffer
	if code := runStdio([]string{"--repo", repo}, strings.NewReader(stdin), &out); code != 0 {
		t.Fatalf("stdio 退出码 %d", code)
	}
	if !strings.Contains(out.String(), `"repoRoot"`) {
		t.Fatalf("鉴权下 initialize 失败:%s", out.String())
	}
}

func TestServeCommandArgs(t *testing.T) {
	if got, want := serveCommandArgs("/tmp/repo", ""), []string{"serve", "--repo", "/tmp/repo"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("无 token 参数 = %#v, want %#v", got, want)
	}
	if got, want := serveCommandArgs("/tmp/repo", "secret"), []string{"serve", "--repo", "/tmp/repo", "--auth"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("有 token 参数 = %#v, want %#v", got, want)
	}
}

// TestServeUpAvoidsExpensiveStatus 回归:完整 /status 在大仓会超过 800ms,
// 就绪探针必须走不触发状态统计的 MCP method gate。
func TestServeUpAvoidsExpensiveStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/mcp/main":
			w.WriteHeader(http.StatusMethodNotAllowed)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()
	if !serveUp(ts.URL, "") {
		t.Fatal("应通过轻量 /mcp/main method gate 判定服务就绪")
	}
}

func TestServeUpDoesNotLeakTokenToUnauthenticatedPort(t *testing.T) {
	token := strings.Repeat("a", 64)
	gotAuthorization := ""
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	if serveUp(ts.URL, token) {
		t.Fatal("token 在位时不得把无鉴权端口认作目标服务")
	}
	if gotAuthorization != "" {
		t.Fatalf("Bearer 泄露给未验证端口:%q", gotAuthorization)
	}

	real := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(mcpserv.AuthFingerprintHeader, mcpserv.AuthFingerprint(token))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer real.Close()
	redirectAuth := ""
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectAuth = r.Header.Get("Authorization")
		http.Redirect(w, r, real.URL+"/mcp/main", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()
	up := serveUp(redirector.URL, token)
	if up || redirectAuth != "" {
		t.Fatalf("重定向端口绕过持钥证明绑定:up=%v auth=%q", up, redirectAuth)
	}
}

func TestServeUpRejectsReplayedFingerprintWithoutLeakingToken(t *testing.T) {
	token := strings.Repeat("b", 64)
	gotAuthorization := ""
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		if r.Header.Get("Authorization") != "Bearer "+token {
			// 静态指纹可在真服务在线时公开读取;伪监听者重放它也不能替代 nonce-HMAC。
			w.Header().Set(mcpserv.AuthFingerprintHeader, mcpserv.AuthFingerprint(token))
			w.Header().Set("WWW-Authenticate", `Bearer realm="iknowledge"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	if serveUp(ts.URL, token) {
		t.Fatal("只重放静态指纹的 challenge 不得被信任")
	}
	if gotAuthorization != "" {
		t.Fatalf("token 泄露给只会伪造 Bearer challenge 的监听者:%q", gotAuthorization)
	}
}
