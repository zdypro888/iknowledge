package mcpserv

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zdypro888/iknowledge/internal/engine"
	"github.com/zdypro888/iknowledge/internal/store"
)

// Bearer 鉴权(impl §1 --auth):无/错令牌全端点 401,正确令牌放行;关闭时不设防。
func TestAuthGuard(t *testing.T) {
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

// 令牌生成幂等且 0600(同机其他用户不可读)。
func TestEnsureAuthTokenIdempotent(t *testing.T) {
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
	fi, err := os.Stat(filepath.Join(repo, ".knowledge", "local", "token"))
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
