package mcpserv

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zdypro888/iknowledge/internal/engine"
	"github.com/zdypro888/iknowledge/internal/store"
)

// 子代理只读腿(GET /recall /map /status):输出与工具一致、只收 GET、参数校验。
func TestReadOnlyEndpoints(t *testing.T) {
	ts, repo := newTestServer(t)

	get := func(path string) (int, string) {
		t.Helper()
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			_ = resp.Body.Close()
			t.Fatal(err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatal(err)
		}
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
	// HTTP 只读腿与 MCP/Engine 同样缺省 limit=5；只有显式超限才钳到 100。
	s, err := store.Open(repo)
	if err != nil {
		t.Fatal(err)
	}
	e := engine.New(s)
	for i := 0; i < 7; i++ {
		name := fmt.Sprintf("limit%d.go", i)
		if err := os.WriteFile(filepath.Join(repo, name), []byte(fmt.Sprintf("package sample\nfunc Limit%d() {}\n", i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.Init(engine.InitOptions{}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 7; i++ {
		node := fmt.Sprintf("limit%d.go#Limit%d", i, i)
		if _, err := e.Remember(engine.RememberArgs{Node: node, Keywords: []string{"readonlylimitmarker"}}, "readonly-limit", "test"); err != nil {
			t.Fatal(err)
		}
	}
	if code, body := get("/recall?q=readonlylimitmarker"); code != 200 || !strings.Contains(body, "关键词命中 5 个节点") {
		t.Errorf("/recall 默认 limit 应为 5 = %d %q", code, body)
	}
	if code, _ := get("/recall"); code != http.StatusBadRequest {
		t.Errorf("缺 q 应 400,got %d", code)
	}
	// 只收 GET。
	resp, err := http.Post(ts.URL+"/recall?q=x", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /recall 应 405,got %d", resp.StatusCode)
	}
}
