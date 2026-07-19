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
	root, err := s.LoadAuthToken()
	if err != nil {
		return "", err
	}
	authEnabled := root != ""
	root = "" // 根密钥只用作模式位，绝不传给未知端口。
	if session, ok, probeErr := serveUp(s, base, authEnabled); ok {
		return session, nil
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
	cmdArgs := []string{"serve", "--repo", s.RepoRoot()}
	// token 文件是持久化的本机 auth 模式标记：机器重启后 stdio 自动拉起
	// serve 时必须继续传 --auth，不能把旧 token 仅当请求头后静默降级为裸服务。
	if authEnabled {
		cmdArgs = append(cmdArgs, "--auth")
	}
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
		if session, ok, probeErr := serveUp(s, base, authEnabled); ok {
			return session, nil
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
	enc := json.NewEncoder(out)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		id, hasID, rpcCode, rpcMessage := parseBridgeRequest(line)
		if rpcCode != 0 {
			if err := encodeBridgeError(enc, nil, rpcCode, rpcMessage); err != nil {
				fmt.Fprintln(os.Stderr, "stdio 写错误:", err)
				return 1
			}
			continue
		}

		// 每条业务请求前都重新证明“此刻占用端口的 listener”持有本仓库身份。
		// 这样后台 serve 在 stdio 长会话中退出、端口随后被接管时，请求正文不会
		// 先泄给新 listener。生产路径的 ensureServe 始终传入非空初始 session；
		// 空值仅保留给不涉及本机服务的桥解析单元测试。
		if authSession != "" {
			fresh, verifyErr := s.AcquireLocalAuthSession(context.Background(), base, "/mcp/main", client)
			if verifyErr != nil {
				if hasID {
					if writeErr := encodeBridgeError(enc, id, -32000, "本机身份验证失败:"+verifyErr.Error()); writeErr != nil {
						fmt.Fprintln(os.Stderr, "stdio 写错误:", writeErr)
						return 1
					}
				}
				continue
			}
			authSession = fresh.Token
		}

		resp, err := proxyHTTPRequest(client, endpoint, line, sid, authSession)
		// 短期 session 到期后重握手并把同一 JSON-RPC 请求安全重试一次。
		// 401 发生在 handler 进入业务前，不会造成写工具重复执行。
		if err == nil && resp.StatusCode == http.StatusUnauthorized {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			fresh, refreshErr := s.AcquireLocalAuthSession(context.Background(), base, "/mcp/main",
				&http.Client{Timeout: 3 * time.Second})
			if refreshErr == nil {
				authSession = fresh.Token
				resp, err = proxyHTTPRequest(client, endpoint, line, sid, authSession)
			} else {
				err = fmt.Errorf("本机身份重新验证失败: %w", refreshErr)
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
		if v := resp.Header.Get("Mcp-Session-Id"); v != "" {
			sid = v
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

func parseBridgeRequest(line []byte) (id json.RawMessage, hasID bool, code int, message string) {
	if !json.Valid(line) {
		return nil, false, -32700, "parse error"
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(line, &object); err != nil || object == nil {
		return nil, false, -32600, "invalid request"
	}
	var version, method string
	if err := json.Unmarshal(object["jsonrpc"], &version); err != nil || version != "2.0" {
		return nil, false, -32600, "invalid request: jsonrpc must be 2.0"
	}
	if err := json.Unmarshal(object["method"], &method); err != nil || strings.TrimSpace(method) == "" {
		return nil, false, -32600, "invalid request: method required"
	}
	id, hasID = object["id"]
	if !hasID {
		return nil, false, 0, "" // 合法 notification。
	}
	trimmed := bytes.TrimSpace(id)
	var value any
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return nil, false, -32600, "invalid request id"
	}
	switch value.(type) {
	case nil:
		return id, true, 0, "" // JSON-RPC 不推荐 null ID，但它仍是请求而非通知。
	case string, json.Number:
		return id, true, 0, ""
	default:
		return nil, false, -32600, "invalid request id"
	}
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
