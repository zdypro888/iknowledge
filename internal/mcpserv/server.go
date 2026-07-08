// Package mcpserv 是手写 JSON-RPC 2.0 的 MCP HTTP server(impl §7,风格参照
// aibridge internal/bridge/mcp.go)。零框架依赖(CLAUDE.md 铁律)。
package mcpserv

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zdypro888/iknowledge/internal/engine"
)

const protocolVersion = "2025-06-18"

// Version 是 serverInfo.version。
const Version = "0.2.0"

// Server 是一个仓库的 MCP 服务。
type Server struct {
	E *engine.Engine
	// AuthToken 非空即启用 Bearer 鉴权(impl §1 --auth,2026-07-04 自四期提前):
	// 全部端点(含 /inject)要求 Authorization: Bearer <token>。Handler() 前设置。
	AuthToken string

	mu       sync.Mutex
	sessions map[string]*session
}

type session struct {
	author   string
	lastSeen time.Time
}

// sessionTTL 是 MCP 会话的空闲上限(R2-D2:原先 sessions 只增不清,长跑 serve
// 内存无界增长;engine 侧台账早有 2h TTL,这里不对称)。过期会话按规范返回 404,
// 客户端自动重新 initialize——与服务端重启后的自愈路径完全相同。
const sessionTTL = 24 * time.Hour

// New 建服务。
func New(e *engine.Engine) *Server {
	s := &Server{E: e, sessions: map[string]*session{}}
	// R29-E7.6:后台 goroutine 每 10min 回收过期 session(handleInitialize 原先只
	// 机会性回收——大量 initialize 但不再发请求的场景会积累过期会话)。cap 10000
	// 防恶意刷 initialize 耗内存(超限返 503)。
	go s.reapSessions()
	return s
}

func (s *Server) reapSessions() {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for range t.C {
		s.mu.Lock()
		for id, sess := range s.sessions {
			if time.Since(sess.lastSeen) > sessionTTL {
				delete(s.sessions, id)
			}
		}
		s.mu.Unlock()
	}
}

// Handler 返回完整路由(impl §7.1):
//
//	POST /mcp/main            主 AI(及委派模式侦察兵)
//	POST /mcp/scout/<job-id>  备模式侦查 agent(工具集受限)
//	     /mcp                 308 → /mcp/main(原示例写 /mcp,照抄连不上——M1.2 验收入口)
//	GET  /inject              hook 注入端点(非 MCP)
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp/main", func(w http.ResponseWriter, r *http.Request) {
		s.serveRPC(w, r, "main")
	})
	mux.HandleFunc("/mcp/scout/", func(w http.ResponseWriter, r *http.Request) {
		s.serveRPC(w, r, "scout")
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		target := "/mcp/main"
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusPermanentRedirect)
	})
	mux.HandleFunc("/inject", s.serveInject)
	// 子代理只读腿(2026-07-04,实战反馈:受限工具集的审计/侦查子代理没有 kb_* 工具,
	// 主 AI 手工转录知识进 brief 必有损耗——但子代理有 shell,curl 即可查库):
	// 纯只读,记账/沉淀仍归有 MCP 的主 AI;同受 auth/origin 门;usage 照记。
	mux.HandleFunc("/recall", s.serveRecall)
	mux.HandleFunc("/map", s.serveMap)
	mux.HandleFunc("/status", s.serveStatus)
	return s.authGuard(originGuard(mux))
}

// authGuard Bearer 鉴权(AuthToken 非空时):常数时间比较防时序侧信道。
// 401 带 WWW-Authenticate(RFC 6750);鉴权先于 Origin 校验(未认证请求最先挡)。
func (s *Server) authGuard(next http.Handler) http.Handler {
	if s.AuthToken == "" {
		return next
	}
	want := []byte("Bearer " + s.AuthToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		// R29-S1.3:删 len 预检——它泄露 token 长度,否定了 ConstantTimeCompare 的常数时间性。
		// subtle.ConstantTimeCompare 自身安全处理不等长(对较长输入恒定时间返回 0)。
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="iknowledge"`)
			http.Error(w, "unauthorized: serve 以 --auth 启动,需 Authorization: Bearer <.knowledge/local/token>", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// originGuard 是 MCP 2025-06-18 传输安全要求(R2-E1):
// "Servers MUST validate the Origin header … to prevent DNS rebinding attacks"。
// 恶意网页经 DNS 重绑定可让受害者浏览器直连 127.0.0.1:18xxx 调工具(读知识/投毒)。
// Origin 只有浏览器会带:存在且非本机来源即 403;curl/MCP 客户端无 Origin,不受影响。
func originGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" && !localOrigin(o) {
			http.Error(w, "forbidden origin (DNS rebinding protection)", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func localOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// ---- JSON-RPC 2.0(request/response 子集,不做 SSE 流) ----

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *Server) serveRPC(w http.ResponseWriter, r *http.Request, role string) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// 连错仓库防护(impl §1):URL 带 ?repo= 时校验。
	if repo := r.URL.Query().Get("repo"); repo != "" {
		want, got := filepath.Clean(s.E.Store.RepoRoot()), filepath.Clean(repo)
		if want != got {
			writeRPCError(w, nil, -32600,
				"KB_ERR:WRONG_REPO: 本服务属于 "+want+",请求指向 "+got+" | 检查 .mcp.json 指向与端口")
			return
		}
	}

	body, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPCError(w, nil, -32700, "parse error")
		return
	}

	// 会话识别(impl §7.1):未知/失效的 Mcp-Session-Id → HTTP 404
	// (规范行为,客户端据此自动重新 initialize——服务端重启后存量会话自愈)。
	sid := r.Header.Get("Mcp-Session-Id")
	if req.Method != "initialize" && sid != "" && !s.sessionExists(sid) {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}

	// 通知(无 id)→ 202 无体。
	if len(req.ID) == 0 || string(req.ID) == "null" {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	switch req.Method {
	case "initialize":
		s.handleInitialize(w, req)
	case "ping":
		writeRPCResult(w, req.ID, map[string]any{})
	case "tools/list":
		writeRPCResult(w, req.ID, map[string]any{"tools": toolDefs(role)})
	case "tools/call":
		s.handleToolCall(w, req, role, sid)
	default:
		writeRPCError(w, req.ID, -32601, "method not found: "+req.Method)
	}
}

func (s *Server) sessionExists(sid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sid]
	if !ok {
		return false
	}
	if time.Since(sess.lastSeen) > sessionTTL {
		delete(s.sessions, sid)
		return false
	}
	sess.lastSeen = time.Now()
	return true
}

func (s *Server) handleInitialize(w http.ResponseWriter, req rpcRequest) {
	var p struct {
		ClientInfo struct {
			Name string `json:"name"`
		} `json:"clientInfo"`
	}
	_ = json.Unmarshal(req.Params, &p) // R29-E7.8:畸形 initialize 仍成功(author=unknown),不阻断握手
	author := strings.TrimSpace(p.ClientInfo.Name)
	if author == "" {
		author = "unknown"
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// 低熵源不可用:不发可预测 ID(session fixation 面)。fail closed。
		writeRPCError(w, req.ID, -32603, "entropy unavailable, retry initialize")
		return
	}
	sid := hex.EncodeToString(b)
	s.mu.Lock()
	for id, sess := range s.sessions { // initialize 低频,顺手回收过期会话
		if time.Since(sess.lastSeen) > sessionTTL {
			delete(s.sessions, id)
		}
	}
	// R29-E7.6:cap 10000 防 initialize 风暴耗内存(后台 goroutine 也定期回收)。
	if len(s.sessions) >= 10000 {
		s.mu.Unlock()
		writeRPCError(w, req.ID, -32603, "too many sessions, retry later")
		return
	}
	s.sessions[sid] = &session{author: author, lastSeen: time.Now()}
	s.mu.Unlock()

	w.Header().Set("Mcp-Session-Id", sid)
	writeRPCResult(w, req.ID, map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": true}},
		"serverInfo":      map[string]any{"name": "knowledge", "version": Version},
		"repoRoot":        s.E.Store.RepoRoot(),
		"instructions":    engine.InitializeInstructions,
	})
}

func (s *Server) author(sid string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[sid]; ok {
		return sess.author
	}
	return "anonymous"
}

// handleToolCall 分发 + 使用日志(impl §7.6)。
func (s *Server) handleToolCall(w http.ResponseWriter, req rpcRequest, role, sid string) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		writeRPCError(w, req.ID, -32602, "invalid params")
		return
	}
	if !toolVisible(role, p.Name) {
		writeRPCError(w, req.ID, -32601, "unknown tool: "+p.Name)
		return
	}

	start := time.Now()
	text, meta, err := s.dispatch(p.Name, p.Arguments, sid)
	rec := engine.UsageRecord{
		At: time.Now().UTC().Format(time.RFC3339), Session: sid, Tool: p.Name,
		OK: err == nil, Hit: meta.Hit, HitStatus: meta.HitStatus, Stale: meta.Stale,
		MS: time.Since(start).Milliseconds(),
	}
	if err != nil {
		if kbe, ok := errors.AsType[*engine.KBError](err); ok {
			rec.ErrCode = kbe.Code
			s.E.LogUsage(monthNow(), rec)
			writeToolResult(w, req.ID, kbe.Error(), true)
			return
		}
		rec.ErrCode = "INTERNAL"
		s.E.LogUsage(monthNow(), rec)
		writeRPCError(w, req.ID, -32603, "internal error: "+err.Error())
		return
	}
	s.E.LogUsage(monthNow(), rec)
	writeToolResult(w, req.ID, text, false)
}

func monthNow() string { return time.Now().UTC().Format("2006-01") }

// dispatch 把工具名路由到 engine;author 由会话推导(不接受 AI 自报,impl §7.1)。
func (s *Server) dispatch(name string, args json.RawMessage, sid string) (string, engine.ReadMeta, error) {
	author := s.author(sid)
	var meta engine.ReadMeta
	un := func(v any) error {
		if len(args) == 0 {
			return nil
		}
		return json.Unmarshal(args, v)
	}
	switch name {
	case "kb_init":
		var a struct {
			Force       bool `json:"force"`
			ReanchorAll bool `json:"reanchor_all"`
		}
		if err := un(&a); err != nil {
			return "", meta, kbInvalid(err)
		}
		rep, err := s.E.Init(engine.InitOptions{Force: a.Force, ReanchorAll: a.ReanchorAll})
		if err != nil {
			return "", meta, err
		}
		return rep.Text(), meta, nil

	case "kb_status":
		text, err := s.E.Status()
		return text, meta, err

	case "kb_map":
		var a struct {
			Path  string `json:"path"`
			Depth int    `json:"depth"`
		}
		if err := un(&a); err != nil {
			return "", meta, kbInvalid(err)
		}
		text, m, err := s.E.Map(a.Path, a.Depth, sid)
		return text, m, err

	case "kb_recall":
		var a struct {
			Query  string `json:"query"`
			Mode   string `json:"mode"`
			Limit  int    `json:"limit"`
			Before string `json:"before"`
		}
		if err := un(&a); err != nil {
			return "", meta, kbInvalid(err)
		}
		text, m, err := s.E.Recall(engine.RecallArgs{Query: a.Query, Mode: a.Mode, Limit: a.Limit, Before: a.Before}, sid)
		return text, m, err

	case "kb_diagnose":
		var a engine.DiagnoseArgs
		if err := un(&a); err != nil {
			return "", meta, kbInvalid(err)
		}
		text, m, err := s.E.Diagnose(a, sid)
		return text, m, err

	case "kb_remember":
		var a engine.RememberArgs
		if err := un(&a); err != nil {
			return "", meta, kbInvalid(err)
		}
		text, err := s.E.Remember(a, sid, author)
		return text, meta, err

	case "kb_record_change":
		var a engine.ChangeArgs
		if err := un(&a); err != nil {
			return "", meta, kbInvalid(err)
		}
		text, err := s.E.RecordChange(a, sid, author)
		return text, meta, err

	case "kb_verify":
		var a engine.VerifyArgs
		if err := un(&a); err != nil {
			return "", meta, kbInvalid(err)
		}
		text, err := s.E.Verify(a, sid, author)
		return text, meta, err

	case "kb_adopt":
		var a engine.AdoptArgs
		if err := un(&a); err != nil {
			return "", meta, kbInvalid(err)
		}
		text, err := s.E.Adopt(a, sid, author)
		return text, meta, err

	case "kb_revert":
		var a engine.RevertArgs
		if err := un(&a); err != nil {
			return "", meta, kbInvalid(err)
		}
		text, err := s.E.Revert(a, sid, author)
		return text, meta, err

	case "kb_task":
		var a engine.TaskArgs
		if err := un(&a); err != nil {
			return "", meta, kbInvalid(err)
		}
		text, err := s.E.Task(a, sid, author)
		return text, meta, err

	case "kb_flow":
		var a engine.FlowArgs
		if err := un(&a); err != nil {
			return "", meta, kbInvalid(err)
		}
		text, err := s.E.Flow(a, sid, author)
		return text, meta, err

	case "kb_session":
		var a struct {
			Action string `json:"action"`
		}
		if err := un(&a); err != nil {
			return "", meta, kbInvalid(err)
		}
		text, err := s.E.Session(sid, a.Action)
		return text, meta, err

	case "kb_maintain":
		var a engine.MaintainArgs
		if err := un(&a); err != nil {
			return "", meta, kbInvalid(err)
		}
		text, err := s.E.Maintain(a, sid, author)
		return text, meta, err

	case "kb_investigate":
		var a engine.InvestigateArgs
		if err := un(&a); err != nil {
			return "", meta, kbInvalid(err)
		}
		text, err := s.E.Investigate(a, sid, author)
		return text, meta, err

	case "kb_submit_findings":
		var a engine.FindingsArgs
		if err := un(&a); err != nil {
			return "", meta, kbInvalid(err)
		}
		text, err := s.E.SubmitFindings(a, sid, author)
		return text, meta, err
	}
	return "", meta, &engine.KBError{Code: "NODE_NOT_FOUND", Msg: "未知工具 " + name, Hint: "tools/list 查看可用工具"}
}

// kbInvalid 包装入参解析失败。R29-S1.6:错误码修正——参数解析失败用 INVALID_PARAMS,
// 不是 NODE_NOT_FOUND(语义不符,AI 按 KB_ERR: 前缀自纠错会被误导)。
func kbInvalid(err error) *engine.KBError {
	return &engine.KBError{Code: "INVALID_PARAMS", Msg: "入参解析失败:" + err.Error(), Hint: "对照工具 inputSchema 修正参数"}
}

// serveInject 是 hook 注入端点(GET /inject?file=&session=,非 MCP,impl §7.1)。
// serveRecall GET /recall?q=<查询>[&mode=usage|history|flow][&limit=N][&session=<sid>]
// ——kb_recall 的纯 HTTP 只读腿(输出与工具一致,text/plain)。
func (s *Server) serveRecall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	if q.Get("q") == "" {
		http.Error(w, "missing ?q=(查询词或节点 ID)", http.StatusBadRequest)
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 200 // R29-E7.5:钳制,防 ?limit=999999999 拖累
	}
	started := time.Now()
	out, meta, err := s.E.Recall(engine.RecallArgs{
		Query: q.Get("q"), Mode: q.Get("mode"), Limit: limit, Before: q.Get("before"),
	}, q.Get("session"))
	s.logReadOnly("kb_recall", q.Get("session"), meta, err, started)
	writeReadOnly(w, out, err)
}

// serveMap GET /map[?path=<前缀>][&depth=N][&session=<sid>]。
func (s *Server) serveMap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	depth, _ := strconv.Atoi(q.Get("depth"))
	started := time.Now()
	out, meta, err := s.E.Map(q.Get("path"), depth, q.Get("session"))
	s.logReadOnly("kb_map", q.Get("session"), meta, err, started)
	writeReadOnly(w, out, err)
}

// serveStatus GET /status。
func (s *Server) serveStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	out, err := s.E.Status()
	writeReadOnly(w, out, err)
}

// logReadOnly 只读腿的使用日志(与工具调用同一口径,月度 JSONL)。
func (s *Server) logReadOnly(tool, sid string, meta engine.ReadMeta, err error, started time.Time) {
	rec := engine.UsageRecord{
		At: time.Now().UTC().Format(time.RFC3339), Session: sid, Tool: tool, Source: "http",
		OK: err == nil, Hit: meta.Hit, HitStatus: meta.HitStatus, Stale: meta.Stale,
		MS: time.Since(started).Milliseconds(),
	}
	if kbe, ok := errors.AsType[*engine.KBError](err); ok {
		rec.ErrCode = kbe.Code
	}
	s.E.LogUsage(monthNow(), rec)
}

func writeReadOnly(w http.ResponseWriter, out string, err error) {
	if err != nil {
		if kbe, ok := errors.AsType[*engine.KBError](err); ok {
			http.Error(w, kbe.Error(), http.StatusUnprocessableEntity)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, out)
}

func (s *Server) serveInject(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	file := r.URL.Query().Get("file")
	sid := r.URL.Query().Get("session")
	if file == "" {
		http.Error(w, "missing ?file=", http.StatusBadRequest)
		return
	}
	// 绝对路径转仓库相对(hook 里 curl 通常拿到绝对路径)。
	// 逃逸判定用 ".."/"../" 精确匹配(R2-A8):HasPrefix(rel, "..") 会把
	// 仓库内以 ".." 开头的合法目录名(如 "..cache/x.go")误判为逃逸。
	if filepath.IsAbs(file) {
		if rel, err := filepath.Rel(s.E.Store.RepoRoot(), file); err == nil &&
			rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			file = filepath.ToSlash(rel)
		}
	}
	text, err := s.E.Inject(file, sid, r.URL.Query().Get("tool"))
	if err != nil {
		if kbe, ok := errors.AsType[*engine.KBError](err); ok {
			http.Error(w, kbe.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, text)
}

func writeToolResult(w http.ResponseWriter, id json.RawMessage, text string, isErr bool) {
	result := map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
	}
	if isErr {
		result["isError"] = true
	}
	writeRPCResult(w, id, result)
}

func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	if id == nil {
		id = json.RawMessage("null")
	}
	_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}})
}
