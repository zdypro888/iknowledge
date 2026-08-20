// stdio 桥(2026-07-04 定案修订,impl §1 生命周期契约):
// MCP 生态惯例是客户端按需拉起 stdio 进程、随会话生死——不该让用户管理常驻服务。
// 但本设计的 hook 注入/多客户端共享/单一写入口/子代理只读腿又需要同一个 HTTP 实例。
// 两全:`iknowledge stdio` 由客户端以 stdio 形式拉起,它按需自动拉起后台 serve
// (不在才起,flock 天然单例;脱会话存活,后续会话/hook/只读腿复用),
// 然后做 stdio(newline-delimited JSON-RPC)↔ HTTP 的透明桥。
// 用户视角:零服务管理——第一个 AI 会话自动把一切带起来。
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/zdypro888/iknowledge/internal/buildinfo"
	"github.com/zdypro888/iknowledge/internal/engine"
	"github.com/zdypro888/iknowledge/internal/store"
)

func runStdio(args []string, in io.Reader, out io.Writer) int {
	fs := flag.NewFlagSet("stdio", flag.ContinueOnError)
	repo := fs.String("repo", ".", "仓库路径")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "错误: stdio 不接受位置参数:", strings.Join(fs.Args(), " "))
		return 2
	}
	s, err := store.Open(*repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	if !s.Initialized() {
		fmt.Fprintln(os.Stderr, "错误: 库未初始化,先跑 iknowledge init --repo "+s.RepoRoot())
		return 1
	}
	writerBusy := false
	if err := recoverTruthBeforeRead(s); err != nil {
		if errors.Is(err, store.ErrLocked) {
			writerBusy = true
		} else {
			fmt.Fprintln(os.Stderr, "错误: 恢复未完成事务:", err)
			return 1
		}
	}
	var cfg *store.Config
	if writerBusy {
		cfg, err = s.LoadConfig() // live serve 的启动配置只读；绝不在锁外 Ensure/写入。
	} else {
		cfg, err = s.EnsureConfig()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", cfg.Port)
	if writerBusy {
		authSession, ok, probeErr := serveUp(s, base, false)
		if !ok {
			fmt.Fprintln(os.Stderr, "错误: 仓库 writer 正忙且端口上没有通过身份校验的 serve:", probeErr)
			return 1
		}
		current, retiredRepos, generationErr := ensureCurrentServeGeneration(s, base)
		if generationErr != nil {
			fmt.Fprintln(os.Stderr, "错误:", generationErr)
			return 1
		}
		if !current {
			// 已认证旧 daemon 已完成优雅退场。ensureCurrentServeGeneration
			// 不只等 listener 关闭，还确认 writer lock 已释放（或另一条
			// bridge 已拉起当前构建）。直接走 ensureServe 可同时覆盖
			// “我来拉起”和“并发 bridge 已拉起”两种竞态；此处若再
			// 单独非阻塞取锁，反而会把后者误报 ErrLocked。
			authSession, err = ensureServeWithRepos(s, base, retiredRepos)
			if err != nil {
				fmt.Fprintln(os.Stderr, "错误:", err)
				return 1
			}
		}
		return proxyStdio(in, out, base+"/mcp/main?repo="+url.QueryEscape(s.RepoRoot()), s, base, authSession,
			engine.RequestWriteTimeout(cfg)+time.Minute)
	}
	authSession, err := ensureServe(s, base)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	return proxyStdio(in, out, base+"/mcp/main?repo="+url.QueryEscape(s.RepoRoot()), s, base, authSession,
		engine.RequestWriteTimeout(cfg)+time.Minute)
}

// ensureServe 确认后台 serve 在线;不在则以脱会话方式拉起并等端口就绪。
// 并发拉起无害:写者锁单例,输家进程自退,赢家端口很快可达。
func ensureServe(s *store.Store, base string) (string, error) {
	return ensureServeWithRepos(s, base, nil)
}

func ensureServeWithRepos(s *store.Store, base string, restartRepos []string) (string, error) {
	if _, err := s.LoadAuthToken(); err != nil { // validate persisted per-repo auth state only
		return "", err
	}
	if session, ok, probeErr := serveUp(s, base, false); ok {
		current, retiredRepos, generationErr := ensureCurrentServeGeneration(s, base)
		if generationErr != nil {
			return "", generationErr
		}
		if current {
			return session, nil
		}
		if len(retiredRepos) > 0 {
			restartRepos = retiredRepos
		}
	} else if probeErr != nil && !localServeUnavailable(probeErr) {
		return "", fmt.Errorf("端口已被无关、旧版或身份不匹配的进程占用: %w", probeErr)
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	logPath := filepath.Join(s.Dir(), "local", "serve.log")
	logF, err := s.OpenKnowledgeLog("local/serve.log", 0o644)
	if err != nil {
		return "", err
	}
	defer func() { _ = logF.Close() }()
	validatedRepos, err := validatedRuntimeRepos(s.RepoRoot(), restartRepos)
	if err != nil {
		return "", err
	}
	cmdArgs := []string{"serve"}
	for _, repo := range validatedRepos {
		cmdArgs = append(cmdArgs, "--repo", repo)
	}
	// 不传全局 --auth：runServe 会对每个 repo 分别 LoadAuthToken，
	// 因而精确保留多仓组内“A 已启用、B 未启用”的混合模式。
	// 统一传 --auth 会给原本裸的 B 新建 token，使它的旧 hook 立即 401。
	cmd := exec.Command(exe, cmdArgs...)
	cmd.Stdout, cmd.Stderr = logF, logF
	detachProc(cmd) // 脱离会话:stdio 桥随客户端退出,serve 留给 hook/后续会话
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("拉起 serve: %w", err)
	}
	go func() { _ = cmd.Wait() }() // 回收僵尸(serve 常驻,正常情况下不返回)
	deadline := time.Now().Add(8 * time.Second)
	var lastProbe error
	for time.Now().Before(deadline) {
		if session, ok, probeErr := serveUp(s, base, false); ok {
			current, _, generationErr := ensureCurrentServeGeneration(s, base)
			if generationErr != nil {
				return "", generationErr
			}
			if current {
				return session, nil
			}
		} else if probeErr != nil {
			if !localServeUnavailable(probeErr) {
				return "", fmt.Errorf("serve 端口身份校验失败(日志 %s): %w", logPath, probeErr)
			}
			lastProbe = probeErr
		}
		time.Sleep(200 * time.Millisecond)
	}
	if lastProbe != nil {
		return "", fmt.Errorf("serve 未在 8s 内通过本机身份校验(日志 %s): %w", logPath, lastProbe)
	}
	return "", fmt.Errorf("serve 未在 8s 内就绪(日志 %s)", logPath)
}

// localServeUnavailable 只把 TCP dial 阶段的“无人监听/不可达”当作可自动拉起。
// 已建立连接后的 404、畸形 JSON、HMAC 不匹配或读超时都说明端口上已有进程，
// 此时再启动一个注定抢不到端口的 serve 并等待 8 秒只会掩盖真正原因。
func localServeUnavailable(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

// serveUp 在 auth 模式下只做 challenge/HMAC/session 握手。根 token 从不作为
// Bearer 探测未知 listener；伪服务拿到 client proof 也无法伪造 server proof。
func serveUp(s *store.Store, base string, _ bool) (session string, ok bool, err error) {
	client := &http.Client{Timeout: 800 * time.Millisecond}
	got, err := s.AcquireLocalAuthSession(context.Background(), base, "/mcp/main", client)
	if err != nil {
		return "", false, err
	}
	return got.Token, true, nil
}

type serveRuntimeStatus struct {
	Schema    int                       `json:"schema"`
	RepoRoot  string                    `json:"repo_root"`
	RepoRoots []string                  `json:"repo_roots"`
	Build     buildinfo.RuntimeIdentity `json:"build"`
	StartedAt string                    `json:"started_at"`
	Sessions  int                       `json:"sessions"`
}

var errRuntimeEndpointMissing = errors.New("daemon runtime endpoint missing")

// ensureCurrentServeGeneration verifies the authenticated listener's captured
// executable identity. A new bridge can therefore replace a daemon left alive
// by an older installation instead of silently serving stale code forever.
// false,nil means an incompatible daemon accepted the authenticated graceful
// shutdown request and has completely released its listener.
func ensureCurrentServeGeneration(s *store.Store, base string) (bool, []string, error) {
	status, err := probeServeRuntime(s, base)
	if err != nil {
		if errors.Is(err, errRuntimeEndpointMissing) {
			return false, nil, fmt.Errorf("运行中的 iknowledge daemon 是不支持安全换代的旧版本；请先用 doctor --deploy 确认后优雅停止旧 serve，再重新连接: %w", err)
		}
		return false, nil, fmt.Errorf("读取 daemon 构建身份: %w", err)
	}
	if !sameRepoPath(status.RepoRoot, s.RepoRoot()) {
		return false, nil, fmt.Errorf("daemon runtime repo 不匹配: got %s want %s", status.RepoRoot, s.RepoRoot())
	}
	repos, err := validatedRuntimeRepos(s.RepoRoot(), status.RepoRoots)
	if err != nil {
		return false, nil, fmt.Errorf("daemon runtime repo 组无效: %w", err)
	}
	current := buildinfo.Runtime()
	if buildinfo.SameRuntime(status.Build, current) {
		return true, repos, nil
	}
	if err := preflightRuntimeRepoGroup(repos); err != nil {
		return false, nil, fmt.Errorf("旧 daemon 保持运行；换代前 repo 组校验失败: %w", err)
	}
	fmt.Fprintf(os.Stderr, "检测到旧 daemon 构建(%s/%s)，正在由当前构建(%s/%s)优雅换代…\n",
		status.Build.Version, shortBuildDigest(status.Build), current.Version, shortBuildDigest(current))
	if err := requestServeShutdown(s, base); err != nil {
		return false, nil, fmt.Errorf("请求旧 daemon 优雅换代: %w", err)
	}
	if err := waitServeRetirement(s, base, 12*time.Second); err != nil {
		return false, nil, err
	}
	return false, repos, nil
}

func probeServeRuntime(s *store.Store, base string) (serveRuntimeStatus, error) {
	var status serveRuntimeStatus
	client := &http.Client{
		Timeout:       3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	session, err := s.AcquireLocalAuthSession(context.Background(), base, "/runtime", client)
	if err != nil {
		if errors.Is(err, store.ErrLocalAuthScopeUnsupported) {
			// Runtime rollout was introduced after the local-auth protocol. Only
			// after proving the listener with the old and new daemons' common
			// /mcp/main scope may a challenge-level rejection of /runtime mean a
			// trusted legacy daemon rather than an arbitrary process on the port.
			if _, ok, proofErr := serveUp(s, base, false); ok {
				return status, errRuntimeEndpointMissing
			} else if proofErr != nil {
				return status, fmt.Errorf("runtime scope 被拒且兼容身份验证失败: %w", proofErr)
			}
			return status, errors.New("runtime scope 被拒且兼容身份验证未通过")
		}
		return status, err
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/runtime", nil)
	if err != nil {
		return status, err
	}
	req.Header.Set("Authorization", store.LocalSessionAuthScheme+" "+session.Token)
	resp, err := client.Do(req)
	if err != nil {
		return status, err
	}
	body, tooLarge, readErr := readBounded(resp.Body, 64<<10)
	closeErr := resp.Body.Close()
	if readErr == nil {
		readErr = closeErr
	}
	if readErr != nil {
		return status, readErr
	}
	if tooLarge {
		return status, errors.New("runtime 响应超过 64KiB")
	}
	if resp.StatusCode == http.StatusNotFound {
		return status, errRuntimeEndpointMissing
	}
	if resp.StatusCode != http.StatusOK {
		return status, fmt.Errorf("runtime HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, &status); err != nil {
		return status, fmt.Errorf("runtime JSON: %w", err)
	}
	if status.Schema != 1 || strings.TrimSpace(status.Build.Version) == "" ||
		(status.Build.ExecutableSHA256 == "" && status.Build.Revision == "") {
		return status, errors.New("runtime 构建身份不完整")
	}
	return status, nil
}

func requestServeShutdown(s *store.Store, base string) error {
	client := &http.Client{
		Timeout:       3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	session, err := s.AcquireLocalAuthSession(context.Background(), base, "/runtime/shutdown", client)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, base+"/runtime/shutdown", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", store.LocalSessionAuthScheme+" "+session.Token)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	closeErr := resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("shutdown HTTP %d", resp.StatusCode)
	}
	return closeErr
}

// waitServeRetirement waits for the old process, not merely its HTTP listener.
// http.Server.Shutdown closes listeners before it finishes draining requests;
// runServe keeps the repository writer lock until all draining and semantic
// cleanup completes. Starting the replacement in that gap creates a loser that
// exits on ErrLocked and leaves no daemon behind. A concurrent bridge may also
// legitimately win the replacement race, in which case observing a matching
// authenticated runtime is equivalent to retirement being complete.
func waitServeRetirement(s *store.Store, base string, timeout time.Duration) error {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return fmt.Errorf("非法 serve 地址 %q", base)
	}
	deadline := time.Now().Add(timeout)
	var lastProbe error
	for time.Now().Before(deadline) {
		release, lockErr := s.AcquireWriterLock()
		if lockErr == nil {
			release()
			return nil
		}
		if !errors.Is(lockErr, store.ErrLocked) {
			return fmt.Errorf("检查旧 daemon writer lock: %w", lockErr)
		}
		status, probeErr := probeServeRuntime(s, base)
		if probeErr == nil && buildinfo.SameRuntime(status.Build, buildinfo.Runtime()) &&
			sameRepoPath(status.RepoRoot, s.RepoRoot()) {
			return nil
		}
		lastProbe = probeErr
		time.Sleep(100 * time.Millisecond)
	}
	if lastProbe != nil {
		return fmt.Errorf("旧 daemon 未在 %s 内释放 %s 的 writer lock(最后探测: %v)", timeout, u.Host, lastProbe)
	}
	return fmt.Errorf("旧 daemon 未在 %s 内释放 %s 的 writer lock", timeout, u.Host)
}

func sameRepoPath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if ar, err := filepath.EvalSymlinks(a); err == nil {
		a = ar
	}
	if br, err := filepath.EvalSymlinks(b); err == nil {
		b = br
	}
	return a == b
}

const maxRuntimeRepoGroup = 64

// validatedRuntimeRepos treats the authenticated daemon's process-wide repo
// list as restart metadata, not as an unchecked command line. Every root is
// canonicalized through Store.Open, duplicates are removed, the endpoint's
// current repository must be present, and an absurd group is rejected before
// the old daemon is asked to stop. Daemons from the first single-repo runtime
// schema omitted repo_roots; that representation safely falls back to current.
func validatedRuntimeRepos(current string, roots []string) ([]string, error) {
	if len(roots) == 0 {
		return []string{current}, nil
	}
	if len(roots) > maxRuntimeRepoGroup {
		return nil, fmt.Errorf("repo 数 %d 超过上限 %d", len(roots), maxRuntimeRepoGroup)
	}
	out := make([]string, 0, len(roots))
	seen := make(map[string]bool, len(roots))
	containsCurrent := false
	for _, root := range roots {
		if strings.TrimSpace(root) == "" {
			return nil, errors.New("repo 组含空路径")
		}
		st, err := store.Open(root)
		if err != nil {
			return nil, fmt.Errorf("打开 repo %q: %w", root, err)
		}
		canonical := st.RepoRoot()
		if seen[canonical] {
			continue
		}
		seen[canonical] = true
		out = append(out, canonical)
		if sameRepoPath(canonical, current) {
			containsCurrent = true
		}
	}
	if !containsCurrent {
		return nil, fmt.Errorf("进程 repo 组不包含当前 endpoint %s", current)
	}
	return out, nil
}

func preflightRuntimeRepoGroup(roots []string) error {
	stores, err := preflightServeRepos(roots)
	if err != nil {
		return err
	}
	ports := make(map[int]string, len(stores))
	for _, st := range stores {
		if _, err := st.LoadAuthToken(); err != nil {
			return fmt.Errorf("读取 %s 持久鉴权状态: %w", st.RepoRoot(), err)
		}
		if _, err := st.EnsureLocalIdentity(); err != nil {
			return fmt.Errorf("验证 %s 本机身份状态: %w", st.RepoRoot(), err)
		}
		cfg, err := st.LoadConfig()
		if err != nil {
			return fmt.Errorf("读取 %s 配置: %w", st.RepoRoot(), err)
		}
		if cfg == nil {
			return fmt.Errorf("读取 %s 配置: config 不存在", st.RepoRoot())
		}
		if previous := ports[cfg.Port]; previous != "" {
			return fmt.Errorf("repo %s 与 %s 共用端口 %d", previous, st.RepoRoot(), cfg.Port)
		}
		ports[cfg.Port] = st.RepoRoot()
	}
	return nil
}

func shortBuildDigest(identity buildinfo.RuntimeIdentity) string {
	value := identity.ExecutableSHA256
	if value == "" {
		value = identity.Revision
	}
	if len(value) > 12 {
		return value[:12]
	}
	return value
}

// proxyStdio 逐行转发 JSON-RPC:stdin → POST endpoint → stdout。
// Mcp-Session-Id 从 initialize 响应头捕获、后续请求回带(会话台账/过时警报依赖它)。
const maxProxyResponseBytes int64 = 64 << 20

func proxyStdio(in io.Reader, out io.Writer, endpoint string, s *store.Store, base, authSession string, requestTimeout time.Duration) int {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 16<<20) // MCP 消息可含大结果,上限 16MiB
	client := &http.Client{
		Timeout: requestTimeout, // 覆盖最长自派侦查 + 一拍传输余量
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // session/业务请求都不跨 origin 重定向。
		},
	}
	sid := ""
	var initializeRequest []byte
	var initializeID json.RawMessage
	enc := json.NewEncoder(out)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		id, hasID, method, rpcCode, rpcMessage := parseBridgeRequest(line)
		if rpcCode != 0 {
			if err := encodeBridgeError(enc, nil, rpcCode, rpcMessage); err != nil {
				fmt.Fprintln(os.Stderr, "stdio 写错误:", err)
				return 1
			}
			continue
		}

		// 每次 HTTP 尝试(含恢复重放)前都重新证明“此刻占用端口的 listener”
		// 持有本仓库身份。生产路径始终传入非空初始 session；空值仅保留给
		// 不涉及本机服务的桥解析单元测试。
		verifyListener := func() error {
			if authSession == "" {
				return nil
			}
			fresh, verifyErr := s.AcquireLocalAuthSession(context.Background(), base, "/mcp/main", client)
			if verifyErr != nil {
				return verifyErr
			}
			authSession = fresh.Token
			return nil
		}

		var resp *http.Response
		var err error
		authRetried, sessionRetried := false, false
	requestAttempts:
		for {
			if verifyErr := verifyListener(); verifyErr != nil {
				err = fmt.Errorf("本机身份验证失败: %w", verifyErr)
				break
			}
			resp, err = proxyHTTPRequest(client, endpoint, line, sid, authSession)
			if err != nil {
				break
			}
			switch {
			case resp.StatusCode == http.StatusUnauthorized && !authRetried:
				// 401 在 handler 前产生；直接关闭响应，下一拍重新双向认证后安全重放。
				_ = resp.Body.Close()
				authRetried = true
				continue
			case resp.StatusCode == http.StatusNotFound && sid != "" && method != "initialize" && !sessionRetried:
				// 未知 Mcp-Session-Id 的 404 同样在 handler 前产生。隐藏 initialize
				// 自身也必须紧邻一次身份验证；成功后下一拍还会在业务重放前再验一次。
				_ = resp.Body.Close()
				if verifyErr := verifyListener(); verifyErr != nil {
					err = fmt.Errorf("MCP session 恢复前身份验证失败: %w", verifyErr)
					break requestAttempts
				}
				freshSID, refreshErr := reinitializeProxySession(client, endpoint, initializeRequest, initializeID, authSession)
				if refreshErr != nil {
					err = fmt.Errorf("MCP session 重新 initialize 失败: %w", refreshErr)
					break requestAttempts
				}
				sid = freshSID
				sessionRetried = true
				continue
			default:
				break requestAttempts
			}
		}
		if err != nil {
			if hasID {
				if writeErr := encodeBridgeError(enc, id, -32000, "serve 不可达:"+err.Error()); writeErr != nil {
					fmt.Fprintln(os.Stderr, "stdio 写错误:", writeErr)
					return 1
				}
			}
			continue
		}
		body, tooLarge, readErr := readBounded(resp.Body, maxProxyResponseBytes)
		closeErr := resp.Body.Close()
		if readErr == nil {
			readErr = closeErr
		}
		if readErr != nil || tooLarge {
			if hasID {
				message := "serve 响应读取失败"
				if tooLarge {
					message = fmt.Sprintf("serve 响应超过 %d 字节上限", maxProxyResponseBytes)
				} else {
					message += ":" + readErr.Error()
				}
				if err := encodeBridgeError(enc, id, -32000, message); err != nil {
					fmt.Fprintln(os.Stderr, "stdio 写错误:", err)
					return 1
				}
			}
			continue
		}
		if !hasID {
			continue // 通知:服务端 202/空体,无可回
		}
		if len(bytes.TrimSpace(body)) == 0 || !json.Valid(body) {
			if err := encodeBridgeError(enc, id, -32000,
				fmt.Sprintf("serve HTTP %d 返回空或非法 JSON", resp.StatusCode)); err != nil {
				fmt.Fprintln(os.Stderr, "stdio 写错误:", err)
				return 1
			}
			continue
		}
		if method == "initialize" {
			freshSID, success, initErr := validateInitializeResponse(body, resp.Header.Get("Mcp-Session-Id"), id)
			if initErr == nil && success && (resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices) {
				initErr = fmt.Errorf("成功 envelope 使用 HTTP %d", resp.StatusCode)
			}
			if initErr != nil {
				if err := encodeBridgeError(enc, id, -32000, "serve initialize 响应非法: "+initErr.Error()); err != nil {
					fmt.Fprintln(os.Stderr, "stdio 写错误:", err)
					return 1
				}
				continue
			}
			if success {
				sid = freshSID
				initializeRequest = bytes.Clone(line)
				initializeID = bytes.Clone(id)
			}
		}
		n, writeErr := out.Write(body)
		if writeErr == nil && n != len(body) {
			writeErr = io.ErrShortWrite
		}
		if writeErr != nil {
			fmt.Fprintln(os.Stderr, "stdio 写错误:", writeErr)
			return 1
		}
		if body[len(body)-1] != '\n' {
			if _, err := io.WriteString(out, "\n"); err != nil {
				fmt.Fprintln(os.Stderr, "stdio 写错误:", err)
				return 1
			}
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "stdio 读取错误:", err)
		return 1
	}
	return 0 // stdin EOF = 客户端会话结束,桥退场(serve 留守)
}

func parseBridgeRequest(line []byte) (id json.RawMessage, hasID bool, method string, code int, message string) {
	if !json.Valid(line) {
		return nil, false, "", -32700, "parse error"
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(line, &object); err != nil || object == nil {
		return nil, false, "", -32600, "invalid request"
	}
	var version string
	if err := json.Unmarshal(object["jsonrpc"], &version); err != nil || version != "2.0" {
		return nil, false, "", -32600, "invalid request: jsonrpc must be 2.0"
	}
	if err := json.Unmarshal(object["method"], &method); err != nil || strings.TrimSpace(method) == "" {
		return nil, false, "", -32600, "invalid request: method required"
	}
	id, hasID = object["id"]
	if !hasID {
		return nil, false, method, 0, "" // 合法 notification。
	}
	trimmed := bytes.TrimSpace(id)
	var value any
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return nil, false, "", -32600, "invalid request id"
	}
	switch value.(type) {
	case nil:
		return id, true, method, 0, "" // JSON-RPC 不推荐 null ID，但它仍是请求而非通知。
	case string, json.Number:
		return id, true, method, 0, ""
	default:
		return nil, false, "", -32600, "invalid request id"
	}
}

func reinitializeProxySession(client *http.Client, endpoint string, initializeRequest []byte, initializeID json.RawMessage, authSession string) (string, error) {
	if len(initializeRequest) == 0 {
		return "", errors.New("客户端尚未成功 initialize")
	}
	resp, err := proxyHTTPRequest(client, endpoint, initializeRequest, "", authSession)
	if err != nil {
		return "", err
	}
	body, tooLarge, readErr := readBounded(resp.Body, maxProxyResponseBytes)
	closeErr := resp.Body.Close()
	if readErr == nil {
		readErr = closeErr
	}
	if readErr != nil {
		return "", readErr
	}
	if tooLarge {
		return "", fmt.Errorf("initialize 响应超过 %d 字节上限", maxProxyResponseBytes)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("serve HTTP %d", resp.StatusCode)
	}
	sid, success, validateErr := validateInitializeResponse(body, resp.Header.Get("Mcp-Session-Id"), initializeID)
	if validateErr != nil {
		return "", validateErr
	}
	if !success {
		return "", errors.New("serve 返回 JSON-RPC initialize error")
	}
	return sid, nil
}

func validateInitializeResponse(body []byte, headerSID string, expectedID json.RawMessage) (sid string, success bool, err error) {
	var envelope struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	if !json.Valid(body) || json.Unmarshal(body, &envelope) != nil || envelope.JSONRPC != "2.0" {
		return "", false, errors.New("不是合法 JSON-RPC 2.0 响应")
	}
	if !bytes.Equal(bytes.TrimSpace(envelope.ID), bytes.TrimSpace(expectedID)) {
		return "", false, errors.New("响应 id 与 initialize 请求不匹配")
	}
	result, rpcErr := bytes.TrimSpace(envelope.Result), bytes.TrimSpace(envelope.Error)
	hasResult := len(result) != 0 && !bytes.Equal(result, []byte("null"))
	hasError := len(rpcErr) != 0 && !bytes.Equal(rpcErr, []byte("null"))
	if hasResult && hasError {
		return "", false, errors.New("响应同时含 result 与 error")
	}
	if hasError {
		return "", false, nil // 合法 JSON-RPC 错误原样交还客户端；隐藏握手由调用方拒绝。
	}
	if !hasResult {
		return "", false, errors.New("成功响应缺少非空 result")
	}
	sid = strings.TrimSpace(headerSID)
	if sid == "" {
		return "", false, errors.New("成功响应未返回 Mcp-Session-Id")
	}
	if len(sid) > 256 {
		return "", false, errors.New("Mcp-Session-Id 超过 256 字节")
	}
	return sid, true, nil
}

func encodeBridgeError(enc *json.Encoder, id any, code int, message string) error {
	return enc.Encode(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": message},
	})
}

func readBounded(r io.Reader, max int64) (data []byte, tooLarge bool, err error) {
	data, err = io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > max {
		return nil, true, nil
	}
	return data, false, nil
}

func proxyHTTPRequest(client *http.Client, endpoint string, body []byte, sid, authSession string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	if authSession != "" {
		req.Header.Set("Authorization", store.LocalSessionAuthorization(authSession))
	}
	return client.Do(req)
}
