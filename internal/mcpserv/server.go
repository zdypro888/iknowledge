// Package mcpserv 是手写 JSON-RPC 2.0 的 MCP HTTP server(impl §7,风格参照
// aibridge internal/bridge/mcp.go)。零框架依赖(CLAUDE.md 铁律)。
package mcpserv

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zdypro888/iknowledge/internal/buildinfo"
	"github.com/zdypro888/iknowledge/internal/engine"
	"github.com/zdypro888/iknowledge/internal/store"
)

const protocolVersion = "2025-06-18"
const maxRPCBodyBytes = 4 << 20

// serverVersion 与 CLI 版本共用构建元数据，避免两个入口报告不同版本。
func serverVersion() string { return buildinfo.Read().Version }

// Server 是一个仓库的 MCP 服务。
type Server struct {
	E *engine.Engine
	// AuthToken 非空即启用 Bearer 鉴权(impl §1 --auth,2026-07-04 自四期提前):
	// 全部端点(含 /inject)要求 Authorization: Bearer <token>。Handler() 前设置。
	AuthToken string
	// LocalIdentity 始终用于内部 loopback 的双向 HMAC 身份证明；它不决定
	// 业务端点是否要求鉴权，因而可在 AuthToken 为空时防端口冒充。
	LocalIdentity string

	mu       sync.Mutex
	sessions map[string]*session

	authMu         sync.Mutex
	authChallenges map[string]localAuthChallenge
	authSessions   map[string]localAuthSession
}

type session struct {
	author                string
	lastSeen              time.Time
	semanticSyncAttempted bool
}

type localAuthChallenge struct {
	client  string
	scope   string
	expires time.Time
}

type localAuthSession struct {
	scope   string
	expires time.Time
}

const (
	localChallengeTTL = 30 * time.Second
	localSessionTTL   = time.Hour // 覆盖最长 32m self scout；stdio 到期会自动重握手。
	maxChallenges     = 1024
	maxAuthSessions   = 4096
)

// sessionTTL 是 MCP 会话的空闲上限(R2-D2:原先 sessions 只增不清,长跑 serve
// 内存无界增长;engine 侧台账早有 2h TTL,这里不对称)。过期会话按规范返回 404,
// 客户端自动重新 initialize——与服务端重启后的自愈路径完全相同。
const sessionTTL = 24 * time.Hour

// New 建服务。
func New(e *engine.Engine) *Server {
	return &Server{
		E: e, sessions: map[string]*session{},
		authChallenges: map[string]localAuthChallenge{}, authSessions: map[string]localAuthSession{},
	}
}

// Handler 返回完整路由(impl §7.1):
//
//	POST /mcp/main            主 AI(及委派模式侦察兵)
//	POST /mcp/scout/<job-id>  备模式侦查 agent(工具集受限)
//	     /mcp                 308 → /mcp/main(原示例写 /mcp,照抄连不上——M1.2 验收入口)
//	GET  /inject              hook 注入端点(非 MCP)
func (s *Server) Handler() http.Handler {
	protected := http.NewServeMux()
	protected.HandleFunc("/mcp/main", func(w http.ResponseWriter, r *http.Request) {
		s.serveRPC(w, r, "main")
	})
	protected.HandleFunc("/mcp/scout/", func(w http.ResponseWriter, r *http.Request) {
		s.serveRPC(w, r, "scout")
	})
	protected.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		target := "/mcp/main"
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusPermanentRedirect)
	})
	protected.HandleFunc("/inject", s.serveInject)
	// 子代理只读腿(2026-07-04,实战反馈:受限工具集的审计/侦查子代理没有 kb_* 工具,
	// 主 AI 手工转录知识进 brief 必有损耗——但子代理有 shell,curl 即可查库):
	// 纯只读,记账/沉淀仍归有 MCP 的主 AI;同受 auth/origin 门;usage 照记。
	protected.HandleFunc("/recall", s.serveRecall)
	protected.HandleFunc("/map", s.serveMap)
	protected.HandleFunc("/status", s.serveStatus)

	// 本机 challenge/session 端点必须在根 Bearer guard 之外，否则客户端为了
	// 认证 server 又得先把根密钥发给未知 listener，正是此协议要消除的漏洞。
	root := http.NewServeMux()
	root.HandleFunc(store.LocalAuthChallengePath, s.serveLocalAuthChallenge)
	root.HandleFunc(store.LocalAuthSessionPath, s.serveLocalAuthSession)
	root.Handle("/", s.authGuard(protected))
	return originGuard(root)
}

// authGuard Bearer 鉴权(AuthToken 非空时):常数时间比较防时序侧信道。
// 401 带 WWW-Authenticate(RFC 6750);鉴权先于 Origin 校验(未认证请求最先挡)。
func (s *Server) authGuard(next http.Handler) http.Handler {
	if s.AuthToken == "" {
		return next
	}
	wantHash := sha256.Sum256([]byte("Bearer " + s.AuthToken))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		gotHash := sha256.Sum256(got)
		rootOK := subtle.ConstantTimeCompare(gotHash[:], wantHash[:]) == 1
		sessionOK := false
		if !rootOK {
			prefix := store.LocalSessionAuthScheme + " "
			header := string(got)
			if strings.HasPrefix(header, prefix) {
				sessionOK = s.localAuthSessionValid(strings.TrimPrefix(header, prefix), r.URL.Path, time.Now())
			}
		}
		if !rootOK && !sessionOK {
			w.Header().Set("WWW-Authenticate", `Bearer realm="iknowledge"`)
			http.Error(w, "unauthorized: 需要显式 HTTP Bearer 或经本机 HMAC 握手取得的短期 session", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) serveLocalAuthChallenge(w http.ResponseWriter, r *http.Request) {
	if s.LocalIdentity == "" {
		http.NotFound(w, r)
		return
	}
	if !localAuthRequestIsLoopback(r) {
		http.Error(w, "local auth is loopback-only", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var input struct {
		Client string `json:"client"`
		Scope  string `json:"scope"`
	}
	if !decodeLocalAuthJSON(w, r, &input) {
		return
	}
	if !store.ValidLocalAuthValue(input.Client) || !validLocalAuthScope(input.Scope) {
		http.Error(w, "invalid local auth request", http.StatusBadRequest)
		return
	}
	challenge, err := secureHex32()
	if err != nil {
		http.Error(w, "entropy unavailable", http.StatusInternalServerError)
		return
	}
	now := time.Now()
	s.authMu.Lock()
	s.pruneLocalAuthLocked(now)
	if len(s.authChallenges) >= maxChallenges {
		s.authMu.Unlock()
		http.Error(w, "too many auth challenges", http.StatusTooManyRequests)
		return
	}
	s.authChallenges[challenge] = localAuthChallenge{client: input.Client, scope: input.Scope, expires: now.Add(localChallengeTTL)}
	s.authMu.Unlock()
	writeLocalAuthJSON(w, map[string]string{"challenge": challenge})
}

func (s *Server) serveLocalAuthSession(w http.ResponseWriter, r *http.Request) {
	if s.LocalIdentity == "" {
		http.NotFound(w, r)
		return
	}
	if !localAuthRequestIsLoopback(r) {
		http.Error(w, "local auth is loopback-only", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var input struct {
		Client    string `json:"client"`
		Challenge string `json:"challenge"`
		Scope     string `json:"scope"`
		Proof     string `json:"proof"`
	}
	if !decodeLocalAuthJSON(w, r, &input) {
		return
	}
	if !store.ValidLocalAuthValue(input.Client) || !store.ValidLocalAuthValue(input.Challenge) ||
		!store.ValidLocalAuthValue(input.Proof) || !validLocalAuthScope(input.Scope) {
		http.Error(w, "invalid local auth proof", http.StatusUnauthorized)
		return
	}
	now := time.Now()
	s.authMu.Lock()
	s.pruneLocalAuthLocked(now)
	challenge, ok := s.authChallenges[input.Challenge]
	delete(s.authChallenges, input.Challenge) // 成败都一次性消费，防重放/在线猜测。
	s.authMu.Unlock()
	if !ok || challenge.client != input.Client || challenge.scope != input.Scope || !challenge.expires.After(now) {
		http.Error(w, "invalid local auth proof", http.StatusUnauthorized)
		return
	}
	want, err := store.LocalAuthClientProof(s.LocalIdentity, input.Client, input.Challenge, input.Scope)
	if err != nil || !store.EqualLocalAuthProof(input.Proof, want) {
		http.Error(w, "invalid local auth proof", http.StatusUnauthorized)
		return
	}
	session, err := secureHex32()
	if err != nil {
		http.Error(w, "entropy unavailable", http.StatusInternalServerError)
		return
	}
	expiresAt := now.Add(localSessionTTL).Truncate(time.Second)
	serverProof, err := store.LocalAuthServerProof(s.LocalIdentity, input.Client, input.Challenge, input.Scope, session, expiresAt.Unix())
	if err != nil {
		http.Error(w, "local auth failure", http.StatusInternalServerError)
		return
	}
	s.authMu.Lock()
	s.pruneLocalAuthLocked(now)
	if len(s.authSessions) >= maxAuthSessions {
		s.authMu.Unlock()
		http.Error(w, "too many auth sessions", http.StatusServiceUnavailable)
		return
	}
	s.authSessions[session] = localAuthSession{scope: input.Scope, expires: expiresAt}
	s.authMu.Unlock()
	writeLocalAuthJSON(w, struct {
		Session   string `json:"session"`
		ExpiresAt int64  `json:"expires_at"`
		Proof     string `json:"proof"`
	}{Session: session, ExpiresAt: expiresAt.Unix(), Proof: serverProof})
}

func localAuthRequestIsLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func decodeLocalAuthJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		http.Error(w, "invalid local auth request", http.StatusBadRequest)
		return false
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		http.Error(w, "invalid local auth request", http.StatusBadRequest)
		return false
	}
	return true
}

func writeLocalAuthJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(value)
}

func secureHex32() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (s *Server) pruneLocalAuthLocked(now time.Time) {
	for challenge, state := range s.authChallenges {
		if !state.expires.After(now) {
			delete(s.authChallenges, challenge)
		}
	}
	for session, state := range s.authSessions {
		if !state.expires.After(now) {
			delete(s.authSessions, session)
		}
	}
}

func (s *Server) localAuthSessionValid(session, requestPath string, now time.Time) bool {
	if !store.ValidLocalAuthValue(session) {
		return false
	}
	s.authMu.Lock()
	defer s.authMu.Unlock()
	s.pruneLocalAuthLocked(now)
	state, ok := s.authSessions[session]
	return ok && state.expires.After(now) && state.scope == requestPath
}

func validLocalAuthScope(scope string) bool {
	if len(scope) > 512 {
		return false
	}
	switch scope {
	case "/mcp/main", "/inject", "/recall", "/map", "/status":
		return true
	}
	return strings.HasPrefix(scope, "/mcp/scout/") && len(strings.TrimPrefix(scope, "/mcp/scout/")) > 0
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

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRPCBodyBytes+1))
	if err != nil {
		http.Error(w, "read request body failed", http.StatusBadRequest)
		return
	}
	if len(body) > maxRPCBodyBytes {
		http.Error(w, "request body exceeds 4 MiB", http.StatusRequestEntityTooLarge)
		return
	}
	var req rpcRequest
	if !json.Valid(body) {
		writeRPCError(w, nil, -32700, "parse error")
		return
	}
	if err := json.Unmarshal(body, &req); err != nil || req.JSONRPC != "2.0" || strings.TrimSpace(req.Method) == "" {
		writeRPCError(w, req.ID, -32600, "invalid request")
		return
	}

	// 会话识别(impl §7.1):未知/失效的 Mcp-Session-Id → HTTP 404
	// (规范行为,客户端据此自动重新 initialize——服务端重启后存量会话自愈)。
	sid := r.Header.Get("Mcp-Session-Id")
	if req.Method != "initialize" && sid != "" && !s.sessionExists(sid) {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}

	// 通知只有“完全没有 id 字段”的合法请求。显式 id:null 仍必须返回响应。
	if len(req.ID) == 0 {
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
		s.handleToolCall(r.Context(), w, req, role, sid)
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
		"serverInfo":      map[string]any{"name": "knowledge", "version": serverVersion()},
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

// claimSemanticSync 把“每个 MCP 会话最多一次 semantic sync”从提示词纪律
// 提升为服务端不变量。即使第一次因 provider 瞬时故障失败，也不允许 AI 在同一
// 会话里自动重试并反复产生费用；用户仍可显式通过 CLI 重建，或在新会话先重新
// 查看 kb_status 后再按持久策略决定。
func (s *Server) claimSemanticSync(sid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sid]
	if !ok {
		return &engine.KBError{Code: "SESSION_NOT_FOUND", Msg: "MCP 会话不存在", Hint: "重新 initialize 后先调用 kb_status"}
	}
	if sess.semanticSyncAttempted {
		return &engine.KBError{
			Code: "SEMANTIC_SYNC_ALREADY_ATTEMPTED",
			Msg:  "本 MCP 会话已经尝试过一次 semantic sync",
			Hint: "不要自动重试；查看 kb_status。需要人工重试时使用 CLI semantic rebuild",
		}
	}
	sess.semanticSyncAttempted = true
	return nil
}

// handleToolCall 分发 + 使用日志(impl §7.6)。
func (s *Server) handleToolCall(ctx context.Context, w http.ResponseWriter, req rpcRequest, role, sid string) {
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
	text, meta, err := s.dispatch(ctx, p.Name, p.Arguments, sid)
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
func (s *Server) dispatch(ctx context.Context, name string, args json.RawMessage, sid string) (string, engine.ReadMeta, error) {
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
		text, err := s.E.StatusContext(ctx)
		return text, meta, err

	case "kb_semantic":
		var a struct {
			Action string `json:"action"`
		}
		if err := un(&a); err != nil {
			return "", meta, kbInvalid(err)
		}
		switch a.Action {
		case "status":
			text, err := s.E.SemanticStatusTextContext(ctx)
			return text, meta, err
		case "sync":
			if err := s.claimSemanticSync(sid); err != nil {
				return "", meta, err
			}
			text, err := s.E.SyncSemantic(ctx)
			return text, meta, err
		default:
			return "", meta, &engine.KBError{Code: "INVALID_ARGUMENT", Msg: "非法 semantic action " + a.Action, Hint: "action ∈ status|sync"}
		}

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
		text, m, err := s.E.RecallContext(ctx, engine.RecallArgs{Query: a.Query, Mode: a.Mode, Limit: a.Limit, Before: a.Before}, sid)
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
		text, err := s.E.TaskContext(ctx, a, sid, author)
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
	if limit > 100 {
		limit = 100 // 上限与 Engine 一致；缺失/非法/<=0 保持 0，由 Engine 使用默认 5
	}
	started := time.Now()
	out, meta, err := s.E.RecallContext(r.Context(), engine.RecallArgs{
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
	out, err := s.E.StatusContext(r.Context())
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
	_, _ = io.WriteString(w, out) // response 已提交，无可恢复路径
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
	_, _ = io.WriteString(w, text) // response 已提交，无可恢复路径
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
