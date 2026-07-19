package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zdypro888/iknowledge/internal/engine"
	"github.com/zdypro888/iknowledge/internal/mcpserv"
	"github.com/zdypro888/iknowledge/internal/store"
)

type alwaysErrorReader struct{}

func (alwaysErrorReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

type alwaysErrorWriter struct{}

func (alwaysErrorWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

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

func TestServeUpUsesMutualLocalSession(t *testing.T) {
	repo := setupGitRepo(t)
	e, _ := initRepo(t, repo, engine.InitOptions{})
	root, err := e.Store.EnsureAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	srv := mcpserv.New(e)
	srv.AuthToken = root
	identity, err := e.Store.EnsureLocalIdentity()
	if err != nil {
		t.Fatal(err)
	}
	srv.LocalIdentity = identity
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	session, ok, err := serveUp(e.Store, ts.URL, true)
	if err != nil || !ok || session == "" {
		t.Fatalf("双向握手未识别真实 auth serve: session=%q ok=%v err=%v", session, ok, err)
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp/main",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Authorization", "IKnowledgeSession "+session)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("短期 session 未获放行: %d", resp.StatusCode)
	}
	// 业务 Bearer 关闭时也必须证明端口身份，不能退化为 /status 200 探测。
	unauthSrv := mcpserv.New(e)
	unauthSrv.LocalIdentity = identity
	unauthTS := httptest.NewServer(unauthSrv.Handler())
	t.Cleanup(unauthTS.Close)
	if session, ok, err := serveUp(e.Store, unauthTS.URL, false); err != nil || !ok || session == "" {
		t.Fatalf("无 Bearer 模式仍应完成本机身份握手: session=%q ok=%v err=%v", session, ok, err)
	}

	var captured strings.Builder
	fixed := strings.Repeat("d", 64)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured.WriteString(r.Header.Get("Authorization"))
		captured.Write(body)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/challenge") {
			_ = json.NewEncoder(w).Encode(map[string]any{"challenge": fixed})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"session": fixed, "expires_at": time.Now().Add(time.Hour).Unix(), "proof": fixed,
		})
	}))
	t.Cleanup(fake.Close)
	if _, ok, err := serveUp(e.Store, fake.URL, false); ok || err == nil {
		t.Fatalf("伪 listener 不得通过 server proof: ok=%v err=%v", ok, err)
	}
	if strings.Contains(captured.String(), root) || strings.Contains(captured.String(), identity) || strings.Contains(captured.String(), "Bearer ") {
		t.Fatalf("stdio 探测泄露长期密钥: %q", captured.String())
	}
}

func TestProxyStdioRejectsMalformedInputAndInvalidJSONResponse(t *testing.T) {
	t.Setenv("IKNOWLEDGE_STATE_HOME", t.TempDir())
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":null,"result":{}}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "not-json")
	}))
	t.Cleanup(ts.Close)
	input := strings.Join([]string{
		`{"jsonrpc":"2.0",`,
		`{"jsonrpc":"2.0","id":null,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	if code := proxyStdio(strings.NewReader(input), &out, ts.URL, s, ts.URL, "", time.Second); code != 0 {
		t.Fatalf("proxy code=%d out=%s", code, out.String())
	}
	if requests != 2 {
		t.Fatalf("畸形 JSON 不应转发，id:null 应作为请求转发，requests=%d", requests)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], `"code":-32700`) ||
		!strings.Contains(lines[1], `"id":null`) || !strings.Contains(lines[1], `"result":{}`) ||
		!strings.Contains(lines[2], `"code":-32000`) {
		t.Fatalf("错误响应不完整:\n%s", out.String())
	}
	for i, line := range lines {
		if !json.Valid([]byte(line)) || !strings.Contains(line, `"jsonrpc":"2.0"`) {
			t.Fatalf("line %d 不是完整 JSON-RPC error: %s", i, line)
		}
	}
}

func TestReadBoundedDetectsOverflowAndReadError(t *testing.T) {
	if data, tooLarge, err := readBounded(strings.NewReader("1234"), 3); err != nil || !tooLarge || data != nil {
		t.Fatalf("overflow: data=%q tooLarge=%v err=%v", data, tooLarge, err)
	}
	if _, _, err := readBounded(alwaysErrorReader{}, 3); err == nil || !strings.Contains(err.Error(), "read failed") {
		t.Fatalf("read error 未传播: %v", err)
	}
}

func TestProxyStdioPropagatesStdoutFailure(t *testing.T) {
	t.Setenv("IKNOWLEDGE_STATE_HOME", t.TempDir())
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if code := proxyStdio(strings.NewReader("not-json\n"), alwaysErrorWriter{}, "http://127.0.0.1:1", s,
		"http://127.0.0.1:1", "", time.Second); code != 1 {
		t.Fatalf("stdout 写失败应退出 1, got %d", code)
	}
}
