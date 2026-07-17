package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zdypro888/iknowledge/internal/engine"
	"github.com/zdypro888/iknowledge/internal/mcpserv"
)

// TestSelfDispatchE2E:自派备模式全链路(impl §7.5)——config scout=self 时
// Investigate 经 PTY 启动侦察兵进程,侦察兵连 /mcp/scout/<job> 交卷,
// Investigate 阻塞收货返回。侦察兵是本测试二进制的自我重执行(协议级假兵,
// 不依赖 claude、零额度消耗;真 claude 只是换 scout_command,链路同一条)。
func TestSelfDispatchE2E(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("自派备模式仅 macOS/Linux(pty stub)")
	}
	repo := setupGitRepo(t)
	e, _ := initRepo(t, repo, engine.InitOptions{})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	bin, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`schema: 1
port: %d
scout: self
scout_command: 'GO_SCOUT_HELPER=1 %s -test.run=TestScoutHelperProcess -- {mcp}'
scout_timeout_seconds: 30
`, port, bin)
	if err := os.WriteFile(filepath.Join(repo, ".knowledge", "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mcpserv.New(e).Handler()}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	e.SetScoutAddr(fmt.Sprintf("127.0.0.1:%d", port))

	out, err := e.Investigate(engine.InvestigateArgs{Question: "支付回调偶发超时的原因在哪"}, "sid-self", "claude-code")
	if err != nil {
		t.Fatalf("自派 Investigate: %v", err)
	}
	for _, want := range []string{"侦察兵交卷", "回调超时来自网关重试风暴", "已交卷"} {
		if !strings.Contains(out, want) {
			t.Errorf("回程缺 %q:\n%s", want, out)
		}
	}
}

// TestScoutHelperProcess 是假侦察兵进程本体(仅当 GO_SCOUT_HELPER=1 时活动):
// 读 {mcp} 配置 → initialize(带 Mcp-Session-Id 回带)→ tools/call kb_submit_findings。
func TestScoutHelperProcess(t *testing.T) {
	if os.Getenv("GO_SCOUT_HELPER") != "1" {
		t.Skip("helper 进程专用")
	}
	var cfgPath string
	for i, a := range os.Args {
		if a == "--" && i+1 < len(os.Args) {
			cfgPath = os.Args[i+1]
		}
	}
	if cfgPath == "" {
		t.Fatal("缺 {mcp} 参数")
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		MCPServers map[string]struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	kn, ok := cfg.MCPServers["knowledge"]
	if !ok {
		t.Fatal("配置缺 knowledge 条目")
	}
	// job ID 从 URL 路径提取(/mcp/scout/<job>?repo=...)。
	jobPart := kn.URL[strings.LastIndex(kn.URL, "/")+1:]
	job, _, _ := strings.Cut(jobPart, "?")

	post := func(sid, body string) (*http.Response, error) {
		req, err := http.NewRequest(http.MethodPost, kn.URL, strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		for k, v := range kn.Headers {
			req.Header.Set(k, v)
		}
		if sid != "" {
			req.Header.Set("Mcp-Session-Id", sid)
		}
		return http.DefaultClient.Do(req)
	}
	resp, err := post("", `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"clientInfo":{"name":"scout-helper"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	sid := resp.Header.Get("Mcp-Session-Id")
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}

	call := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"kb_submit_findings","arguments":{"job":%q,"conclusion":"回调超时来自网关重试风暴","locations":["internal/auth/login.go#Login"],"plan":"给回调处理加幂等键","risks":"重试窗口内的重复入账"}}}`, job)
	resp, err = post(sid, call)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, resp.Body); err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "ack") {
		t.Fatalf("交卷失败:%s", buf.String())
	}
}
