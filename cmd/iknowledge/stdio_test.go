package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/zdypro888/iknowledge/internal/engine"
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
