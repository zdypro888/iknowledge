package store

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLocalAuthRejectsForgedServerWithoutLeakingRoot(t *testing.T) {
	t.Setenv(stateHomeEnv, t.TempDir())
	s, err := Open(t.TempDir())
	if err != nil {
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
	fixed := strings.Repeat("b", 64)
	var captured strings.Builder
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured.WriteString(r.Header.Get("Authorization"))
		captured.Write(body)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case LocalAuthChallengePath:
			_ = json.NewEncoder(w).Encode(map[string]any{"challenge": fixed})
		case LocalAuthSessionPath:
			var req localSessionRequest
			_ = json.Unmarshal(body, &req)
			// 反射刚收到的 client proof；域分离后不能冒充 server proof。
			_ = json.NewEncoder(w).Encode(map[string]any{
				"session": fixed, "expires_at": time.Now().Add(time.Hour).Unix(), "proof": req.Proof,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fake.Close)

	if _, err := s.AcquireLocalAuthSession(context.Background(), fake.URL, "/mcp/main", fake.Client()); err == nil ||
		!strings.Contains(err.Error(), "server proof") {
		t.Fatalf("伪服务不应通过双向认证: %v", err)
	}
	if strings.Contains(captured.String(), root) || strings.Contains(captured.String(), identity) || strings.Contains(captured.String(), "Bearer ") {
		t.Fatalf("握手向未知 listener 泄露了长期密钥/Bearer: %q", captured.String())
	}
}

func TestLocalAuthRefusesRedirectWithProof(t *testing.T) {
	t.Setenv(stateHomeEnv, t.TempDir())
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureAuthToken(); err != nil {
		t.Fatal(err)
	}
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("HMAC 请求不应跟随重定向到达 target")
	}))
	t.Cleanup(redirectTarget.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)
	if _, err := s.AcquireLocalAuthSession(context.Background(), redirector.URL, "/mcp/main", redirector.Client()); err == nil ||
		!strings.Contains(fmt.Sprint(err), "HTTP 307") {
		t.Fatalf("本机 auth 必须拒绝 redirect: %v", err)
	}
}

func TestLocalAuthBaseRequiresLoopbackDestination(t *testing.T) {
	for _, base := range []string{"http://0.0.0.0:18000", "http://[::]:18000", "http://192.0.2.1:18000"} {
		if _, err := validateLocalAuthBase(base); err == nil {
			t.Errorf("非回环目标必须拒绝:%s", base)
		}
	}
	for _, base := range []string{"http://127.0.0.1:18000", "http://[::1]:18000", "http://localhost:18000"} {
		if _, err := validateLocalAuthBase(base); err != nil {
			t.Errorf("合法回环目标被拒:%s: %v", base, err)
		}
	}
}
