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
	"time"

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
	if err := ensureServe(s, base); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	token, _ := s.LoadAuthToken()
	return proxyStdio(in, out, base+"/mcp/main?repo="+url.QueryEscape(s.RepoRoot()), token)
}

// ensureServe 确认后台 serve 在线;不在则以脱会话方式拉起并等端口就绪。
// 并发拉起无害:写者锁单例,输家进程自退,赢家端口很快可达。
func ensureServe(s *store.Store, base string) error {
	if serveUp(base) {
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
	defer logF.Close()
	cmd := exec.Command(exe, "serve", "--repo", s.RepoRoot())
	cmd.Stdout, cmd.Stderr = logF, logF
	detachProc(cmd) // 脱离会话:stdio 桥随客户端退出,serve 留给 hook/后续会话
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("拉起 serve: %w", err)
	}
	go cmd.Wait() // 回收僵尸(serve 常驻,正常情况下不返回)
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if serveUp(base) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("serve 未在 8s 内就绪(日志 %s)", logPath)
}

func serveUp(base string) bool {
	c := &http.Client{Timeout: 800 * time.Millisecond}
	resp, err := c.Get(base + "/status")
	if err != nil {
		return false
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// proxyStdio 逐行转发 JSON-RPC:stdin → POST endpoint → stdout。
// Mcp-Session-Id 从 initialize 响应头捕获、后续请求回带(会话台账/过时警报依赖它)。
func proxyStdio(in io.Reader, out io.Writer, endpoint, token string) int {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 16<<20) // MCP 消息可含大结果,上限 16MiB
	client := &http.Client{Timeout: 10 * time.Minute} // 自派侦查会阻塞分钟级
	sid := ""
	enc := json.NewEncoder(out)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		// 只需 id 判"请求 vs 通知"(通知无响应体可回)。
		var probe struct {
			ID json.RawMessage `json:"id"`
		}
		json.Unmarshal(line, &probe)
		hasID := len(probe.ID) > 0 && string(probe.ID) != "null"

		req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(line))
		if err != nil {
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
			if hasID {
				enc.Encode(map[string]any{"jsonrpc": "2.0", "id": probe.ID,
					"error": map[string]any{"code": -32000, "message": "serve 不可达:" + err.Error()}})
			}
			continue
		}
		if v := resp.Header.Get("Mcp-Session-Id"); v != "" {
			sid = v
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if !hasID {
			continue // 通知:服务端 202/空体,无可回
		}
		if len(bytes.TrimSpace(body)) == 0 || resp.StatusCode >= 400 && !json.Valid(body) {
			enc.Encode(map[string]any{"jsonrpc": "2.0", "id": probe.ID,
				"error": map[string]any{"code": -32000, "message": fmt.Sprintf("serve HTTP %d", resp.StatusCode)}})
			continue
		}
		out.Write(body)
		if body[len(body)-1] != '\n' {
			io.WriteString(out, "\n")
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "stdio 读取错误:", err)
		return 1
	}
	return 0 // stdin EOF = 客户端会话结束,桥退场(serve 留守)
}
