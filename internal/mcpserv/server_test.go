package mcpserv

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zdypro888/iknowledge/internal/engine"
	"github.com/zdypro888/iknowledge/internal/store"
)

const testSrc = `package auth

// Login 登录入口。
func Login(user, pass string) error { return nil }
`

func newTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
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
	ts := httptest.NewServer(New(e).Handler())
	t.Cleanup(ts.Close)
	return ts, repo
}

type rpcOut struct {
	Result map[string]any `json:"result"`
	Error  *rpcError      `json:"error"`
}

// call 发一个 JSON-RPC 请求,返回解析结果与 HTTP 响应。
func call(t *testing.T, url, sid, method string, params any) (*rpcOut, *http.Response) {
	t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out rpcOut
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		_ = resp.Body.Close()
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return &out, resp
}

func toolCall(t *testing.T, url, sid, name string, args any) (string, bool) {
	t.Helper()
	out, _ := call(t, url, sid, "tools/call", map[string]any{"name": name, "arguments": args})
	if out.Error != nil {
		t.Fatalf("tools/call %s 协议错误: %+v", name, out.Error)
	}
	content := out.Result["content"].([]any)[0].(map[string]any)["text"].(string)
	isErr, _ := out.Result["isError"].(bool)
	return content, isErr
}

func initialize(t *testing.T, url string) string {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"clientInfo":{"name":"claude-code"}}}`
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	sid := resp.Header.Get("Mcp-Session-Id")
	if sid == "" {
		_ = resp.Body.Close()
		t.Fatal("initialize 未返回 Mcp-Session-Id 头")
	}
	var out rpcOut
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		_ = resp.Body.Close()
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if out.Result["protocolVersion"] != "2025-06-18" {
		t.Fatalf("protocolVersion 错:%+v", out.Result)
	}
	instructions, ok := out.Result["instructions"].(string)
	if !ok || instructions == "" {
		t.Error("initialize 缺 instructions")
	} else {
		for _, required := range []string{
			"每个会话先 kb_status", "provider=unchecked", "next_action=kb_semantic action=sync",
			"policy=ai-local/ai-remote", "本会话最多同步一次", "绝不替用户配置、下载或切换模型",
		} {
			if !strings.Contains(instructions, required) {
				t.Errorf("initialize instructions 缺 %q: %s", required, instructions)
			}
		}
	}
	if _, ok := out.Result["repoRoot"]; !ok {
		t.Error("initialize 缺 repoRoot(连错仓库防护)")
	}
	serverInfo, ok := out.Result["serverInfo"].(map[string]any)
	if !ok || serverInfo["version"] != serverVersion() {
		t.Errorf("serverInfo.version 未取构建元数据:%+v", out.Result["serverInfo"])
	}
	return sid
}

func TestProtocolBasics(t *testing.T) {
	ts, _ := newTestServer(t)
	main := ts.URL + "/mcp/main"
	sid := initialize(t, main)

	t.Run("ping", func(t *testing.T) {
		out, _ := call(t, main, sid, "ping", nil)
		if out.Error != nil {
			t.Fatalf("ping: %+v", out.Error)
		}
	})
	t.Run("通知返回202", func(t *testing.T) {
		resp, err := http.Post(main, "application/json",
			strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
		if err != nil {
			t.Fatal(err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusAccepted {
			t.Errorf("通知应 202,got %d", resp.StatusCode)
		}
	})
	t.Run("未知会话404", func(t *testing.T) {
		req, _ := http.NewRequest("POST", main, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		req.Header.Set("Mcp-Session-Id", "deadbeef00000000")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("未知会话应 404(客户端据此自动重连),got %d", resp.StatusCode)
		}
	})
	t.Run("匿名连接可用", func(t *testing.T) {
		out, _ := call(t, main, "", "tools/list", nil)
		if out.Error != nil {
			t.Fatalf("匿名 tools/list: %+v", out.Error)
		}
	})
	t.Run("308重定向", func(t *testing.T) {
		client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}}
		resp, err := client.Post(ts.URL+"/mcp?repo=x", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusPermanentRedirect {
			t.Errorf("/mcp 应 308,got %d", resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/mcp/main") {
			t.Errorf("Location = %s", loc)
		}
	})
	t.Run("未知方法-32601", func(t *testing.T) {
		out, _ := call(t, main, sid, "resources/list", nil)
		if out.Error == nil || out.Error.Code != -32601 {
			t.Errorf("未知方法应 -32601:%+v", out.Error)
		}
	})
}

func TestInvalidJSONRPCGetsProtocolError(t *testing.T) {
	ts, _ := newTestServer(t)
	endpoint := ts.URL + "/mcp/main"
	cases := []struct {
		name string
		body string
		code int
	}{
		{"syntax", "{broken", -32700},
		{"empty-object", `{}`, -32600},
		{"null", `null`, -32600},
		{"array", `[]`, -32600},
		{"wrong-version", `{"jsonrpc":"1.0","id":1,"method":"ping"}`, -32600},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Post(endpoint, "application/json", strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			var out rpcOut
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				_ = resp.Body.Close()
				t.Fatal(err)
			}
			if err := resp.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if out.Error == nil || out.Error.Code != tc.code {
				t.Fatalf("协议错误=%+v,want %d", out.Error, tc.code)
			}
		})
	}

	resp, err := http.Post(endpoint, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":null,"method":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		_ = resp.Body.Close()
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if id, ok := raw["id"]; !ok || string(id) != "null" {
		t.Fatalf("id:null 请求未返回响应:%s", id)
	}
}

func TestRPCBodyLimitRejectsInsteadOfTruncating(t *testing.T) {
	ts, _ := newTestServer(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"ping"}` + strings.Repeat(" ", maxRPCBodyBytes)
	resp, err := http.Post(ts.URL+"/mcp/main", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限请求状态=%d,want 413", resp.StatusCode)
	}
}

func TestExpiredSessionIsReapedOnAccess(t *testing.T) {
	srv := New(nil)
	srv.sessions["expired"] = &session{author: "old", lastSeen: time.Now().Add(-sessionTTL - time.Minute)}
	if srv.sessionExists("expired") {
		t.Fatal("过期 session 不应继续有效")
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.sessions) != 0 {
		t.Fatalf("过期 session 未机会性回收:%v", srv.sessions)
	}
}

func TestWrongRepoGuard(t *testing.T) {
	ts, _ := newTestServer(t)
	out, _ := call(t, ts.URL+"/mcp/main?repo=/some/other/repo", "", "ping", nil)
	if out.Error == nil || !strings.Contains(out.Error.Message, "WRONG_REPO") {
		t.Errorf("连错仓库应硬错误:%+v", out.Error)
	}
}

func TestToolVisibilityByEndpoint(t *testing.T) {
	ts, _ := newTestServer(t)
	list := func(url string) map[string]bool {
		out, _ := call(t, url, "", "tools/list", nil)
		names := map[string]bool{}
		for _, tl := range out.Result["tools"].([]any) {
			names[tl.(map[string]any)["name"].(string)] = true
		}
		return names
	}
	mainTools := list(ts.URL + "/mcp/main")
	if len(mainTools) != 17 { // 16 个存量工具 + 授权内的 kb_semantic
		t.Errorf("main 端点应 17 个工具,got %d: %v", len(mainTools), mainTools)
	}
	if !mainTools["kb_semantic"] {
		t.Error("main 端点应可见 kb_semantic")
	}
	scoutTools := list(ts.URL + "/mcp/scout/job_x")
	if len(scoutTools) != 7 {
		t.Errorf("scout 端点应仅 7 个受限工具,got %d: %v", len(scoutTools), scoutTools)
	}
	for _, banned := range []string{"kb_investigate", "kb_record_change", "kb_init", "kb_adopt", "kb_verify", "kb_revert", "kb_maintain", "kb_status", "kb_semantic", "kb_session"} {
		if scoutTools[banned] {
			t.Errorf("scout 端点不应可见 %s(防套娃/侦察兵不改码)", banned)
		}
	}
	for _, allowed := range []string{"kb_map", "kb_recall", "kb_remember", "kb_task", "kb_submit_findings"} {
		if !scoutTools[allowed] {
			t.Errorf("scout 端点应可见 %s", allowed)
		}
	}
	// scout 端点调 main 专属工具 → -32601，semantic sync 不得借侦察端点绕过授权。
	for _, name := range []string{"kb_record_change", "kb_semantic"} {
		out, _ := call(t, ts.URL+"/mcp/scout/job_x", "", "tools/call",
			map[string]any{"name": name, "arguments": map[string]any{"action": "sync"}})
		if out.Error == nil || out.Error.Code != -32601 {
			t.Errorf("scout 越权调 %s 应 -32601:%+v", name, out.Error)
		}
	}
}

// TestFullAgentLoop 模拟一个 agent 的完整纪律循环(e2e,协议层)。
func TestFullAgentLoop(t *testing.T) {
	ts, repo := newTestServer(t)
	main := ts.URL + "/mcp/main"
	sid := initialize(t, main)

	// ① kb_status / kb_map 导航。
	text, isErr := toolCall(t, main, sid, "kb_status", map[string]any{})
	if isErr || !strings.Contains(text, "节点:") ||
		!strings.Contains(text, "semantic: unconfigured | provider: unchecked | next_action:") {
		t.Fatalf("status: %s", text)
	}
	text, isErr = toolCall(t, main, sid, "kb_map", map[string]any{})
	if isErr || !strings.Contains(text, "internal/auth/ ") {
		t.Fatalf("map 默认两级应见目录: %s", text)
	}
	text, isErr = toolCall(t, main, sid, "kb_map", map[string]any{"path": "internal/auth", "depth": 2})
	if isErr || !strings.Contains(text, "internal/auth/login.go") || !strings.Contains(text, "#Login") {
		t.Fatalf("map 下钻应见文件与符号: %s", text)
	}

	// ② recall 空手 → miss 协议。
	text, isErr = toolCall(t, main, sid, "kb_recall", map[string]any{"query": "限流阈值"})
	if isErr || !strings.Contains(text, "回填义务") {
		t.Fatalf("miss 协议: %s", text)
	}

	// ③ remember 沉淀 + 回填 keywords。
	text, isErr = toolCall(t, main, sid, "kb_remember", map[string]any{
		"node":     "internal/auth/login.go#Login",
		"entries":  []map[string]any{{"kind": "pitfall", "text": "pass 传明文,不要在调用方加密"}},
		"keywords": []string{"限流阈值", "登录"},
	})
	if isErr {
		t.Fatalf("remember: %s", text)
	}

	// ④ 现在关键词能命中了(索引生长闭环)。
	text, isErr = toolCall(t, main, sid, "kb_recall", map[string]any{"query": "限流阈值"})
	if isErr || !strings.Contains(text, "Login") {
		t.Fatalf("回填后应命中: %s", text)
	}

	// ⑤ 改代码 → record_change 记账(一个逻辑修改一条)。
	if err := os.WriteFile(filepath.Join(repo, "internal/auth/login.go"),
		[]byte(strings.Replace(testSrc, "return nil", "return validate(user)", 1)+"\nfunc validate(u string) error { return nil }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	text, isErr = toolCall(t, main, sid, "kb_record_change", map[string]any{
		"nodes": []string{"internal/auth/login.go#Login", "internal/auth/login.go#validate"},
		"what":  "抽出 validate", "why": "复用校验",
		"rejected": []map[string]any{{"option": "内联校验", "reason": "三处重复"}},
	})
	if isErr || !strings.Contains(text, "changeId: chg_") {
		t.Fatalf("record_change: %s", text)
	}

	// ⑥ history 可见决策链。
	text, isErr = toolCall(t, main, sid, "kb_recall", map[string]any{
		"query": "internal/auth/login.go#Login", "mode": "history"})
	if isErr || !strings.Contains(text, "✗ 否决过: 内联校验") {
		t.Fatalf("history: %s", text)
	}

	// ⑦ 业务错误走 isError(KB_ERR 约定)。
	// 宽松:no/such.go#X 走关键词分支可能 miss 而非错(结果不断言,仅覆盖路径)——
	// 断言用下面一个必然 KB_ERR 的调用。
	_, _ = toolCall(t, main, sid, "kb_recall", map[string]any{"query": "no/such.go#X", "mode": "usage"})
	text, isErr = toolCall(t, main, sid, "kb_verify", map[string]any{
		"entry": "internal/auth/login.go#Login#e_00000000", "verdict": "refute"})
	if !isErr || !strings.Contains(text, "KB_ERR:") {
		t.Fatalf("业务拒绝应 isError+KB_ERR: %s", text)
	}

	// ⑧ 使用日志落盘(impl §7.6)。
	data, err := os.ReadFile(filepath.Join(repo, ".knowledge", "local",
		"usage-"+monthNow()+".jsonl"))
	if err != nil {
		t.Fatalf("使用日志未落盘: %v", err)
	}
	log := string(data)
	for _, want := range []string{`"tool":"kb_recall"`, `"tool":"kb_record_change"`, `"hit":true`} {
		if !strings.Contains(log, want) {
			t.Errorf("使用日志缺 %s:\n%s", want, log)
		}
	}

	// ⑨ GET /inject 注入端点。
	resp, err := http.Get(ts.URL + "/inject?file=internal/auth/login.go&session=" + sid)
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
	if resp.StatusCode != 200 || !strings.Contains(string(body), "不要在调用方加密") {
		t.Fatalf("inject: %d %s", resp.StatusCode, body)
	}
}

func TestKBStatusToolDescribesSemanticNextAction(t *testing.T) {
	def, ok := allTools["kb_status"].(map[string]any)
	if !ok {
		t.Fatalf("kb_status definition=%T", allTools["kb_status"])
	}
	description, _ := def["description"].(string)
	for _, required := range []string{
		"semantic", "每会话先读", "provider=unchecked", "next_action", "kb_semantic action=sync",
		"policy=ai-local/ai-remote", "不要重复配置或同步",
	} {
		if !strings.Contains(description, required) {
			t.Fatalf("kb_status description missing %q: %s", required, description)
		}
	}
}

func TestKBSemanticToolSchemaAndDiscipline(t *testing.T) {
	def, ok := allTools["kb_semantic"].(map[string]any)
	if !ok {
		t.Fatalf("kb_semantic definition=%T", allTools["kb_semantic"])
	}
	description, _ := def["description"].(string)
	for _, required := range []string{
		"next_action", "kb_semantic action=sync", "ai-local/ai-remote", "每会话最多 sync 一次",
		"ready/none", "绝不修改 endpoint/model/profile/policy", "下载或切换模型",
	} {
		if !strings.Contains(description, required) {
			t.Fatalf("kb_semantic description missing %q: %s", required, description)
		}
	}
	schema, ok := def["inputSchema"].(map[string]any)
	if !ok {
		t.Fatalf("kb_semantic inputSchema=%T", def["inputSchema"])
	}
	required, _ := schema["required"].([]string)
	if len(required) != 1 || required[0] != "action" {
		t.Fatalf("kb_semantic required=%v", required)
	}
	properties, _ := schema["properties"].(map[string]any)
	action, _ := properties["action"].(map[string]any)
	enum, _ := action["enum"].([]string)
	if len(enum) != 2 || enum[0] != "status" || enum[1] != "sync" {
		t.Fatalf("kb_semantic action enum=%v", enum)
	}
}

func TestKBFlowSchemaAllowsActionSpecificShapes(t *testing.T) {
	def, ok := allTools["kb_flow"].(map[string]any)
	if !ok {
		t.Fatalf("kb_flow definition=%T", allTools["kb_flow"])
	}
	schema, _ := def["inputSchema"].(map[string]any)
	required, _ := schema["required"].([]string)
	if len(required) != 1 || required[0] != "action" {
		t.Fatalf("kb_flow top-level required=%v; get must allow action-only", required)
	}
	properties, _ := schema["properties"].(map[string]any)
	flow, _ := properties["flow"].(map[string]any)
	if nested, exists := flow["required"]; exists {
		t.Fatalf("kb_flow nested required=%v; engine validates id/title by action", nested)
	}
}

func TestSemanticAutomationDisciplinePrompt(t *testing.T) {
	for name, prompt := range map[string]string{
		"initialize": engine.InitializeInstructions,
		"repository": engine.DisciplinePrompt,
	} {
		t.Run(name, func(t *testing.T) {
			for _, required := range []string{
				"每个会话先 kb_status", "provider=unchecked", "kb_semantic action=sync",
				"policy=ai-local/ai-remote", "最多", "一次", "绝不替用户配置、下载或切换模型",
			} {
				if !strings.Contains(prompt, required) {
					t.Fatalf("%s prompt missing %q:\n%s", name, required, prompt)
				}
			}
		})
	}
}

func newMCPSemanticProvider(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	requests := &atomic.Int64{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var input struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || len(input.Input) == 0 {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		data := make([]map[string]any, len(input.Input))
		for i := range input.Input {
			data[i] = map[string]any{"index": i, "embedding": []float64{1, 0, 0}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(provider.Close)
	return provider, requests
}

func TestKBSemanticStatusInputAndManualAuthorization(t *testing.T) {
	t.Setenv("IKNOWLEDGE_STATE_HOME", t.TempDir())
	ts, repo := newTestServer(t)
	main := ts.URL + "/mcp/main"
	sid := initialize(t, main)

	text, isErr := toolCall(t, main, sid, "kb_semantic", map[string]any{"action": "status"})
	if isErr || !strings.Contains(text, "status: unconfigured") || !strings.Contains(text, "provider: unchecked") {
		t.Fatalf("unconfigured semantic status isErr=%v:\n%s", isErr, text)
	}
	for name, args := range map[string]map[string]any{
		"missing-action": {},
		"unknown-action": {"action": "configure"},
	} {
		t.Run(name, func(t *testing.T) {
			text, isErr := toolCall(t, main, sid, "kb_semantic", args)
			if !isErr || !strings.Contains(text, "KB_ERR:INVALID_ARGUMENT") || !strings.Contains(text, "status|sync") {
				t.Fatalf("invalid input isErr=%v text=%s", isErr, text)
			}
		})
	}

	s, err := store.Open(repo)
	if err != nil {
		t.Fatal(err)
	}
	cfg := engine.DefaultSemanticSettings()
	cfg.Enabled = true
	cfg.Endpoint = "http://127.0.0.1:1/v1"
	cfg.Model = "manual-model"
	cfg.Dimensions = 3
	cfg.RebuildPolicy = engine.SemanticRebuildManual
	if err := engine.SaveSemanticSettings(s, cfg); err != nil {
		t.Fatal(err)
	}
	text, isErr = toolCall(t, main, sid, "kb_semantic", map[string]any{"action": "sync"})
	if !isErr || !strings.Contains(text, "KB_ERR:SEMANTIC_SYNC_NOT_AUTHORIZED") || !strings.Contains(text, "rebuild_policy=manual") {
		t.Fatalf("manual sync isErr=%v:\n%s", isErr, text)
	}
}

func TestKBSemanticAuthorizedSyncIsProviderIdempotent(t *testing.T) {
	t.Setenv("IKNOWLEDGE_STATE_HOME", t.TempDir())
	provider, requests := newMCPSemanticProvider(t)
	ts, repo := newTestServer(t)
	main := ts.URL + "/mcp/main"
	sid := initialize(t, main)

	s, err := store.Open(repo)
	if err != nil {
		t.Fatal(err)
	}
	cfg := engine.DefaultSemanticSettings()
	cfg.Enabled = true
	cfg.Endpoint = provider.URL
	cfg.Model = "mcp-test-model"
	cfg.Dimensions = 3
	cfg.TimeoutSec = 2
	cfg.RebuildPolicy = engine.SemanticRebuildAILocal
	if err := engine.SaveSemanticSettings(s, cfg); err != nil {
		t.Fatal(err)
	}

	text, isErr := toolCall(t, main, sid, "kb_remember", map[string]any{
		"node": "internal/auth/login.go#Login",
		"entries": []map[string]any{{
			"kind": "summary", "text": "登录入口使用服务端锁定策略",
		}},
	})
	if isErr {
		t.Fatalf("remember before semantic sync: %s", text)
	}
	text, isErr = toolCall(t, main, sid, "kb_status", map[string]any{})
	if isErr || !strings.Contains(text, "semantic: configured-no-index") ||
		!strings.Contains(text, "next_action: kb_semantic action=sync") || !strings.Contains(text, "policy=ai-local") {
		t.Fatalf("pre-sync status isErr=%v:\n%s", isErr, text)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("kb_status contacted provider %d times", got)
	}

	text, isErr = toolCall(t, main, sid, "kb_semantic", map[string]any{"action": "sync"})
	if isErr || !strings.Contains(text, "semantic 索引已重建") {
		t.Fatalf("authorized sync isErr=%v:\n%s", isErr, text)
	}
	afterSync := requests.Load()
	if afterSync == 0 {
		t.Fatal("authorized sync did not contact configured provider")
	}
	text, isErr = toolCall(t, main, sid, "kb_status", map[string]any{})
	if isErr || !strings.Contains(text, "semantic: ready") || !strings.Contains(text, "next_action: none") {
		t.Fatalf("post-sync status isErr=%v:\n%s", isErr, text)
	}
	if got := requests.Load(); got != afterSync {
		t.Fatalf("post-sync kb_status contacted provider: before=%d after=%d", afterSync, got)
	}

	// “每会话最多同步一次”是服务端不变量，不只依赖提示词纪律。
	// 误调第二次必须零 provider 请求而非再次付费/占用本机模型。
	text, isErr = toolCall(t, main, sid, "kb_semantic", map[string]any{"action": "sync"})
	if !isErr || !strings.Contains(text, "KB_ERR:SEMANTIC_SYNC_ALREADY_ATTEMPTED") {
		t.Fatalf("duplicate sync isErr=%v:\n%s", isErr, text)
	}
	if got := requests.Load(); got != afterSync {
		t.Fatalf("duplicate ready sync contacted provider: before=%d after=%d", afterSync, got)
	}
}

func TestKBSemanticFailedSyncCannotRetryProviderInSameSession(t *testing.T) {
	t.Setenv("IKNOWLEDGE_STATE_HOME", t.TempDir())
	requests := &atomic.Int64{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "temporary provider failure", http.StatusServiceUnavailable)
	}))
	t.Cleanup(provider.Close)
	ts, repo := newTestServer(t)
	main := ts.URL + "/mcp/main"
	sid := initialize(t, main)

	s, err := store.Open(repo)
	if err != nil {
		t.Fatal(err)
	}
	cfg := engine.DefaultSemanticSettings()
	cfg.Enabled = true
	cfg.Endpoint = provider.URL
	cfg.Model = "mcp-failing-model"
	cfg.Dimensions = 3
	cfg.TimeoutSec = 2
	cfg.RebuildPolicy = engine.SemanticRebuildAILocal
	if err := engine.SaveSemanticSettings(s, cfg); err != nil {
		t.Fatal(err)
	}

	out, _ := call(t, main, sid, "tools/call", map[string]any{
		"name": "kb_semantic", "arguments": map[string]any{"action": "sync"},
	})
	if out.Error == nil {
		t.Fatalf("first failing sync unexpectedly succeeded: %+v", out.Result)
	}
	afterFirst := requests.Load()
	if afterFirst == 0 {
		t.Fatalf("first sync did not reach provider: %+v", out.Error)
	}

	text, isErr := toolCall(t, main, sid, "kb_semantic", map[string]any{"action": "sync"})
	if !isErr || !strings.Contains(text, "KB_ERR:SEMANTIC_SYNC_ALREADY_ATTEMPTED") {
		t.Fatalf("second sync isErr=%v:\n%s", isErr, text)
	}
	if got := requests.Load(); got != afterFirst {
		t.Fatalf("second sync retried provider: before=%d after=%d", afterFirst, got)
	}
}

func TestAuthorFromClientInfo(t *testing.T) {
	ts, repo := newTestServer(t)
	main := ts.URL + "/mcp/main"
	sid := initialize(t, main) // clientInfo.name = claude-code

	if _, isErr := toolCall(t, main, sid, "kb_remember", map[string]any{
		"node":    "internal/auth/login.go#Login",
		"entries": []map[string]any{{"kind": "summary", "text": "登录入口"}},
	}); isErr {
		t.Fatal("remember 失败")
	}
	data, err := os.ReadFile(filepath.Join(repo, ".knowledge", "tree", "internal", "auth", "login.go.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "author: claude-code") {
		t.Errorf("author 应由 clientInfo 推导(不接受 AI 自报):\n%s", data)
	}
}

func TestInvalidJSON(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Post(ts.URL+"/mcp/main", "application/json", strings.NewReader("{not json"))
	if err != nil {
		t.Fatal(err)
	}
	var out rpcOut
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		_ = resp.Body.Close()
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if out.Error == nil || out.Error.Code != -32700 {
		t.Errorf("坏 JSON 应 -32700:%+v", out.Error)
	}
}

func TestScoutSubmitViaMainEndpoint(t *testing.T) {
	// 轮 22:委派模式下侦察兵连 main 端点交卷——验证 main 可调 kb_submit_findings。
	ts, _ := newTestServer(t)
	main := ts.URL + "/mcp/main"
	sid := initialize(t, main)

	text, isErr := toolCall(t, main, sid, "kb_investigate", map[string]any{"question": "登录偶尔失败,定位原因"})
	if isErr {
		t.Fatalf("investigate: %s", text)
	}
	jobID := ""
	for line := range strings.SplitSeq(text, "\n") {
		if i := strings.Index(line, "job_"); i >= 0 {
			jobID = line[i : i+12]
			break
		}
	}
	if jobID == "" {
		t.Fatalf("简报里没有 job id: %s", text)
	}
	text, isErr = toolCall(t, main, sid, "kb_submit_findings", map[string]any{
		"job": jobID, "conclusion": "锁定计数无时间窗", "locations": []string{"internal/auth/login.go#Login"}})
	if isErr {
		t.Fatalf("main 端点交卷失败(委派模式回程断裂,轮 23 blocker 复发): %s", text)
	}
	_ = fmt.Sprint()
}
