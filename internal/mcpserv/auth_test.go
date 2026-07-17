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
			challenge := strings.Repeat("c", 64)
			req.Header.Set(AuthChallengeHeader, challenge)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = resp.Body.Close() })
			if resp.StatusCode != tc.want {
				t.Errorf("状态码 = %d, want %d", resp.StatusCode, tc.want)
			}
			if tc.want == http.StatusUnauthorized && resp.Header.Get("WWW-Authenticate") == "" {
				t.Error("401 应带 WWW-Authenticate 头(RFC 6750)")
			}
			if got := resp.Header.Get(AuthFingerprintHeader); got != AuthFingerprint(tok) {
				t.Errorf("auth 服务指纹=%q,want %q", got, AuthFingerprint(tok))
			}
			if got := resp.Header.Get(AuthProofHeader); !VerifyAuthProof(tok, challenge, got) {
				t.Errorf("auth 服务 HMAC 证明无效:%q", got)
			}
		})
	}
}

func TestAuthProofIsNonceBound(t *testing.T) {
	token := strings.Repeat("d", 64)
	a := strings.Repeat("a", 64)
	b := strings.Repeat("b", 64)
	proof := AuthProof(token, a)
	if !VerifyAuthProof(token, a, proof) {
		t.Fatal("正确 challenge 的证明未通过")
	}
	if VerifyAuthProof(token, b, proof) || VerifyAuthProof(strings.Repeat("e", 64), a, proof) {
		t.Fatal("证明不得跨 challenge 或 token 重放")
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
		if err := os.Chmod(filepath.Join(repo, ".knowledge", "local", "token"), 0o644); err != nil {
			t.Fatal(err)
		}
		if tok, err := s.LoadAuthToken(); err == nil || tok != "" {
			t.Fatalf("权限过宽的 token 必须 fail closed,got %q/%v", tok, err)
		}
		if _, err := s.EnsureAuthToken(); err != nil {
			t.Fatal(err)
		}
		fi, err = os.Stat(filepath.Join(repo, ".knowledge", "local", "token"))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("既有 token 权限未收紧:mode=%v", fi.Mode().Perm())
		}
	}
	got, err := s.LoadAuthToken()
	if err != nil || got != t1 {
		t.Errorf("LoadAuthToken = %q/%v, want %q", got, err, t1)
	}
}

func TestLoadAuthTokenFailsClosedOnCorruption(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"empty", "\n"},
		{"short", "deadbeef\n"},
		{"non-hex", strings.Repeat("z", 64) + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			s, err := store.Open(repo)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(repo, ".knowledge", "local", "token")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if tok, err := s.LoadAuthToken(); err == nil || tok != "" {
				t.Fatalf("损坏 token 必须 fail closed,got %q/%v", tok, err)
			}
			if tok, err := s.EnsureAuthToken(); err == nil || tok != "" {
				t.Fatalf("损坏 token 不得被静默覆盖,got %q/%v", tok, err)
			}
		})
	}
}
