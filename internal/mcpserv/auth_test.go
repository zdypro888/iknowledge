package mcpserv

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zdypro888/iknowledge/internal/engine"
	"github.com/zdypro888/iknowledge/internal/store"
)

// Bearer 鉴权(impl §1 --auth):无/错令牌全端点 401,正确令牌放行;关闭时不设防。
func TestAuthGuard(t *testing.T) {
	t.Setenv("IKNOWLEDGE_STATE_HOME", t.TempDir())
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "internal/auth"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "internal/auth/login.go"), []byte(testSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(repo)
	if err != nil {
		t.Fatal(err)
	}
	e := engine.New(s)
	if _, err := e.Init(engine.InitOptions{}); err != nil {
		t.Fatal(err)
	}
	tok, err := s.EnsureAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(e)
	srv.AuthToken = tok
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	initBody := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"clientInfo":{"name":"claude-code"}}}`
	cases := []struct {
		name   string
		path   string
		method string
		header string
		want   int
	}{
		{"main 无令牌", "/mcp/main", "POST", "", http.StatusUnauthorized},
		{"main 错令牌", "/mcp/main", "POST", "Bearer wrong", http.StatusUnauthorized},
		{"main 正确令牌", "/mcp/main", "POST", "Bearer " + tok, http.StatusOK},
		{"inject 无令牌", "/inject?file=internal/auth/login.go", "GET", "", http.StatusUnauthorized},
		{"inject 正确令牌", "/inject?file=internal/auth/login.go", "GET", "Bearer " + tok, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body io.Reader
			if tc.method == "POST" {
				body = strings.NewReader(initBody)
			}
			req, err := http.NewRequest(tc.method, ts.URL+tc.path, body)
			if err != nil {
				t.Fatal(err)
			}
			if tc.method == "POST" {
				req.Header.Set("Content-Type", "application/json")
			}
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("状态码 = %d, want %d", resp.StatusCode, tc.want)
			}
			if tc.want == http.StatusUnauthorized && resp.Header.Get("WWW-Authenticate") == "" {
				t.Error("401 应带 WWW-Authenticate 头(RFC 6750)")
			}
		})
	}
}

func TestLocalAuthHandshakeSessionAndChallengeReplay(t *testing.T) {
	t.Setenv("IKNOWLEDGE_STATE_HOME", t.TempDir())
	repo := t.TempDir()
	s, err := store.Open(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	root, err := s.EnsureAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	identity, err := s.EnsureLocalIdentity()
	if err != nil {
		t.Fatal(err)
	}
	e := engine.New(s)
	if _, err := e.Init(engine.InitOptions{}); err != nil {
		t.Fatal(err)
	}
	srv := New(e)
	srv.AuthToken = root
	srv.LocalIdentity = identity
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	session, err := s.AcquireLocalAuthSession(context.Background(), ts.URL, "/status", ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	if !session.ExpiresAt.After(time.Now()) || !store.ValidLocalAuthValue(session.Token) {
		t.Fatalf("短期 session 异常: %+v", session)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/status", nil)
	req.Header.Set("Authorization", store.LocalSessionAuthorization(session.Token))
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("短期 session 未被 auth middleware 接受: %d", resp.StatusCode)
	}
	wrongScope, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp/main", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	wrongScope.Header.Set("Authorization", store.LocalSessionAuthorization(session.Token))
	wrongResp, err := ts.Client().Do(wrongScope)
	if err != nil {
		t.Fatal(err)
	}
	wrongResp.Body.Close()
	if wrongResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("scope=/status 的 session 越权访问 main: %d", wrongResp.StatusCode)
	}

	// 手工完成一次握手后重放同一个 challenge/proof；服务端必须在第一次尝试时
	// 已消费 challenge，第二次无论 proof 是否正确都返回 401。
	clientNonce := strings.Repeat("c", 64)
	challengeBody, _ := json.Marshal(map[string]string{"client": clientNonce, "scope": "/status"})
	challengeResp, err := ts.Client().Post(ts.URL+store.LocalAuthChallengePath, "application/json", bytes.NewReader(challengeBody))
	if err != nil {
		t.Fatal(err)
	}
	var challenge struct {
		Challenge string `json:"challenge"`
	}
	if err := json.NewDecoder(challengeResp.Body).Decode(&challenge); err != nil {
		t.Fatal(err)
	}
	challengeResp.Body.Close()
	proof, err := store.LocalAuthClientProof(identity, clientNonce, challenge.Challenge, "/status")
	if err != nil {
		t.Fatal(err)
	}
	sessionBody, _ := json.Marshal(map[string]string{
		"client": clientNonce, "challenge": challenge.Challenge, "scope": "/status", "proof": proof,
	})
	for i, want := range []int{http.StatusOK, http.StatusUnauthorized} {
		got, err := ts.Client().Post(ts.URL+store.LocalAuthSessionPath, "application/json", bytes.NewReader(sessionBody))
		if err != nil {
			t.Fatal(err)
		}
		got.Body.Close()
		if got.StatusCode != want {
			t.Fatalf("challenge attempt %d status=%d want=%d", i+1, got.StatusCode, want)
		}
	}
}

func TestLocalAuthEndpointsRejectNonLoopbackSource(t *testing.T) {
	srv := New(nil)
	srv.LocalIdentity = strings.Repeat("a", 64)
	body := strings.NewReader(`{"client":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","scope":"/status"}`)
	req := httptest.NewRequest(http.MethodPost, store.LocalAuthChallengePath, body)
	req.RemoteAddr = "192.0.2.10:4321"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("非回环 challenge status=%d, want 403", rec.Code)
	}
	srv.authMu.Lock()
	defer srv.authMu.Unlock()
	if len(srv.authChallenges) != 0 {
		t.Fatalf("非回环请求不应分配 challenge: %d", len(srv.authChallenges))
	}
}

// 令牌生成幂等且 0600(同机其他用户不可读)。
func TestEnsureAuthTokenIdempotent(t *testing.T) {
	t.Setenv("IKNOWLEDGE_STATE_HOME", t.TempDir())
	repo := t.TempDir()
	s, err := store.Open(repo)
	if err != nil {
		t.Fatal(err)
	}
	t1, err := s.EnsureAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(t1) != 64 {
		t.Errorf("token 长度 = %d, want 64(32 字节 hex)", len(t1))
	}
	t2, err := s.EnsureAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	if t1 != t2 {
		t.Error("EnsureAuthToken 不幂等")
	}
	tokenPath, err := s.AuthTokenFile()
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	// Windows 无 POSIX 权限位(0600 映射到 ACL 语义不保真),只在 unix 断言。
	if runtime.GOOS != "windows" {
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("token 权限 = %o, want 600", perm)
		}
	}
	got, err := s.LoadAuthToken()
	if err != nil || got != t1 {
		t.Errorf("LoadAuthToken = %q/%v, want %q", got, err, t1)
	}
}
