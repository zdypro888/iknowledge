package mcpserv

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// 子代理只读腿(GET /recall /map /status):输出与工具一致、只收 GET、参数校验。
func TestReadOnlyEndpoints(t *testing.T) {
	ts, _ := newTestServer(t)

	get := func(path string) (int, string) {
		t.Helper()
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	if code, body := get("/status"); code != 200 || !strings.Contains(body, "repoRoot:") {
		t.Errorf("/status = %d %q", code, body)
	}
	if code, body := get("/map"); code != 200 || !strings.Contains(body, "internal/") {
		t.Errorf("/map = %d %q", code, body)
	}
	if code, body := get("/recall?q=internal/auth/login.go%23Login"); code != 200 || !strings.Contains(body, "节点: internal/auth/login.go#Login") {
		t.Errorf("/recall 精确命中 = %d %q", code, body)
	}
	if code, _ := get("/recall"); code != http.StatusBadRequest {
		t.Errorf("缺 q 应 400,got %d", code)
	}
	// 只收 GET。
	resp, err := http.Post(ts.URL+"/recall?q=x", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /recall 应 405,got %d", resp.StatusCode)
	}
}
