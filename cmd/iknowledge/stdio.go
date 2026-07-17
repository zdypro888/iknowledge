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
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/zdypro888/iknowledge/internal/mcpserv"
	"github.com/zdypro888/iknowledge/internal/store"
)

func runStdio(args []string, in io.Reader, out io.Writer) int {
	fs := flag.NewFlagSet("stdio", flag.ContinueOnError)
	repo := fs.String("repo", ".", "仓库路径")
	if err := fs.Parse(args); err != nil {
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
	cfg, err := s.EnsureConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", cfg.Port)
	token, err := s.LoadAuthToken()
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:读取鉴权 token:", err)
		return 1
	}
	if err := ensureServe(s, base, token); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	return proxyStdio(in, out, base+"/mcp/main?repo="+url.QueryEscape(s.RepoRoot()), token)
}

// ensureServe 确认后台 serve 在线;不在则以脱会话方式拉起并等端口就绪。
// 并发拉起无害:写者锁单例,输家进程自退,赢家端口很快可达。
func ensureServe(s *store.Store, base, token string) error {
	if serveUp(base, token) {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logPath := filepath.Join(s.Dir(), "local", "serve.log")
	logF, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, serveCommandArgs(s.RepoRoot(), token)...)
	cmd.Stdout, cmd.Stderr = logF, logF
	detachProc(cmd) // 脱离会话:stdio 桥随客户端退出,serve 留给 hook/后续会话
	if err := cmd.Start(); err != nil {
		_ = logF.Close()
		return fmt.Errorf("拉起 serve: %w", err)
	}
	if err := logF.Close(); err != nil {
		return fmt.Errorf("关闭 serve 日志句柄: %w", err)
	}
	go func() { _ = cmd.Wait() }() // 回收僵尸(serve 常驻,正常情况下不返回)
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if serveUp(base, token) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("serve 未在 8s 内就绪(日志 %s)", logPath)
}

func serveCommandArgs(repo, token string) []string {
	args := []string{"serve", "--repo", repo}
	if token != "" {
		args = append(args, "--auth")
	}
	return args
}

func serveUp(base, token string) bool {
	c := &http.Client{
		Timeout: 800 * time.Millisecond,
		// 探活身份绑定的是这个端口;跟随重定向会让伪服务借真实服务的持钥证明过关,
		// 随后在第二跳收到 Bearer。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	request := func(auth, challenge string) (*http.Response, error) {
		req, err := http.NewRequest(http.MethodGet, base+"/status", nil)
		if err != nil {
			return nil, err
		}
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		if challenge != "" {
			req.Header.Set(mcpserv.AuthChallengeHeader, challenge)
		}
		return c.Do(req)
	}
	// 第一跳绝不携带秘密。nonce-HMAC 持钥证明用于排除无鉴权/不同 token 的实例;
	// 主动同机代理可转发探测,不属于明文 loopback+Bearer 的可解边界。
	challenge := ""
	if token != "" {
		var err error
		challenge, err = mcpserv.NewAuthChallenge()
		if err != nil {
			return false
		}
	}
	probe, err := request("", challenge)
	if err != nil {
		return false
	}
	if !discardAndClose(probe) {
		return false
	}
	if token == "" {
		return probe.StatusCode == http.StatusOK
	}
	want := mcpserv.AuthFingerprint(token)
	if probe.StatusCode != http.StatusUnauthorized {
		return false
	}
	// 普通 401 或可重放的静态指纹都不足以证明监听者就是本仓服务。随机挑战的
	// HMAC 应答证明当前监听者持有 token;主动实时转发仍属于 OS 隔离边界。
	if probe.Header.Get(mcpserv.AuthFingerprintHeader) != want ||
		!mcpserv.VerifyAuthProof(token, challenge, probe.Header.Get(mcpserv.AuthProofHeader)) {
		return false
	}
	resp, err := request(token, "")
	if err != nil {
		return false
	}
	ok := resp.StatusCode == http.StatusOK && resp.Header.Get(mcpserv.AuthFingerprintHeader) == want
	return discardAndClose(resp) && ok
}

func discardAndClose(resp *http.Response) bool {
	_, copyErr := io.Copy(io.Discard, resp.Body)
	closeErr := resp.Body.Close()
	return copyErr == nil && closeErr == nil
}

// proxyStdio 逐行转发 JSON-RPC:stdin → POST endpoint → stdout。
// Mcp-Session-Id 从 initialize 响应头捕获、后续请求回带(会话台账/过时警报依赖它)。
func proxyStdio(in io.Reader, out io.Writer, endpoint, token string) int {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 16<<20)            // MCP 消息可含大结果,上限 16MiB
	client := &http.Client{Timeout: 10 * time.Minute} // 自派侦查会阻塞分钟级
	sid := ""
	enc := json.NewEncoder(out)
	encodeError := func(id json.RawMessage, message string) bool {
		err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id,
			"error": map[string]any{"code": -32000, "message": message}})
		if err != nil {
			fmt.Fprintln(os.Stderr, "stdio 写错误响应失败:", err)
			return false
		}
		return true
	}
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		// 只需 id 判"请求 vs 通知"(通知无响应体可回)。
		var probe struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
		}
		probeErr := json.Unmarshal(line, &probe)
		// 语法/结构错误都不是通知,服务端的 -32700/-32600 必须转回客户端。通知
		// 只有一个定义:合法 JSON-RPC 请求对象且完全缺少 id;显式 null 仍需响应。
		hasID := probeErr != nil || probe.JSONRPC != "2.0" || strings.TrimSpace(probe.Method) == "" || len(probe.ID) > 0

		req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(line))
		if err != nil {
			if hasID && !encodeError(probe.ID, "构造 serve 请求失败:"+err.Error()) {
				return 1
			}
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		if sid != "" {
			req.Header.Set("Mcp-Session-Id", sid)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			if hasID && !encodeError(probe.ID, "serve 不可达:"+err.Error()) {
				return 1
			}
			continue
		}
		if v := resp.Header.Get("Mcp-Session-Id"); v != "" {
			sid = v
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		closeErr := resp.Body.Close()
		if readErr != nil || closeErr != nil {
			transportErr := readErr
			if transportErr == nil {
				transportErr = closeErr
			}
			if hasID && !encodeError(probe.ID, "读取 serve 响应失败:"+transportErr.Error()) {
				return 1
			}
			continue
		}
		if !hasID {
			continue // 通知:服务端 202/空体,无可回
		}
		if len(bytes.TrimSpace(body)) == 0 || resp.StatusCode >= 400 && !json.Valid(body) {
			if !encodeError(probe.ID, fmt.Sprintf("serve HTTP %d", resp.StatusCode)) {
				return 1
			}
			continue
		}
		n, err := out.Write(body)
		if err == nil && n != len(body) {
			err = io.ErrShortWrite
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "stdio 写响应失败:", err)
			return 1
		}
		if body[len(body)-1] != '\n' {
			if _, err := io.WriteString(out, "\n"); err != nil {
				fmt.Fprintln(os.Stderr, "stdio 写响应换行失败:", err)
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
