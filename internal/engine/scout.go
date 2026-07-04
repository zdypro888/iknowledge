package engine

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/zdypro888/iknowledge/internal/pty"
	"github.com/zdypro888/iknowledge/internal/store"
)

// 自派备模式(knowledge.md §10.4 轮 22 备模式;impl §7.5;2026-07-04 实装):
// 服务端 PTY 驱动一个交互式 CLI 侦察兵执行简报,阻塞等 kb_submit_findings 回程。
// 面向无子代理宿主/需接口级隔离(侦察兵连 /mcp/scout/<job>,工具集受限)。
// 与 aibridge 方案的关键差异:交卷信号走协议(SubmitFindings→job.done),
// 不解析终端画面,故无需终端仿真库(铁律一:零重依赖,PTY 原语手写)。

// defaultScoutCommand 缺省侦察兵命令(config scout_command 可覆盖):
// 禁用 claude -p(独立限流池,CLAUDE.md 铁律),走交互式 + PTY;
// --strict-mcp-config 只用注入的 scout 配置;--allowedTools 免审批放行 kb 工具。
const defaultScoutCommand = `claude --mcp-config {mcp} --strict-mcp-config --allowedTools "mcp__knowledge__*"`

// SetScoutAddr 注入 serve 实际监听地址(自派侦察兵回连用;--addr 覆盖端口时
// config 端口不可信)。serve 启动期调用,先于任何请求,不加锁。
func (e *Engine) SetScoutAddr(addr string) { e.scoutAddr = addr }

// scoutBase 返回侦察兵回连的 host:port(缺省从 config 端口推导)。
func (e *Engine) scoutBase() string {
	if e.scoutAddr != "" {
		return e.scoutAddr
	}
	if cfg, _ := e.Store.LoadConfig(); cfg != nil {
		return fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	}
	return ""
}

// selfDispatch 驱动自派侦察兵:写 scout MCP 配置 → PTY 启动 → 喂简报 →
// 阻塞等交卷/超时/进程早退。调用时不持 rt.mu(SubmitFindings 要拿锁)。
func (e *Engine) selfDispatch(job *scoutJob, brief string, cfg *store.Config) (string, error) {
	base := e.scoutBase()
	if base == "" {
		return "", kbErr("SCOUT_TIMEOUT", "无法确定回连地址(缺 config 端口)", "检查 .knowledge/config.yaml")
	}

	// scout MCP 配置写 .knowledge/local/(铁律二边界内);用完即删,日志保留。
	scoutURL := fmt.Sprintf("http://%s/mcp/scout/%s?repo=%s", base, job.ID, url.QueryEscape(e.Store.RepoRoot()))
	mcpCfg := map[string]any{"mcpServers": map[string]any{"knowledge": mcpServerEntry(scoutURL, e.Store)}}
	cfgBytes, err := json.MarshalIndent(mcpCfg, "", "  ")
	if err != nil {
		return "", err
	}
	cfgPath := filepath.Join(e.Store.Dir(), "local", "scout-mcp-"+job.ID+".json")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(cfgPath, cfgBytes, 0o600); err != nil {
		return "", err
	}
	defer os.Remove(cfgPath)

	command := cfg.ScoutCommand
	if strings.TrimSpace(command) == "" {
		command = defaultScoutCommand
	}
	command = strings.ReplaceAll(command, "{mcp}", cfgPath)
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = e.Store.RepoRoot()
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return "", kbErr("SCOUT_TIMEOUT", "侦察兵进程启动失败:"+err.Error(),
			"检查 config 的 scout_command;或改回委派模式(scout: delegate)")
	}
	defer ptmx.Close()
	defer pty.KillGroup(cmd)

	// 输出旁路进日志(有界;不参与任何判定——完成信号走协议)。
	logPath := filepath.Join(e.Store.Dir(), "local", "scout-"+job.ID+".log")
	go drainToLog(ptmx, logPath, 1<<20)
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	// 喂简报:括号粘贴包裹(多行文本裸 \n 会被 TUI 当回车逐行提交,kbeval 排障定案)
	// + 两步输入(正文、停顿、单发回车,对抗 paste 突发去抖,aibridge driver.go 同法)。
	if _, err := ptmx.Write([]byte("\x1b[200~" + brief + "\x1b[201~")); err == nil {
		time.Sleep(2 * time.Second)
		ptmx.Write([]byte("\r"))
	}

	timeout := time.Duration(cfg.ScoutTimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	select {
	case findings := <-job.done:
		pty.KillGroup(cmd)
		return framed(findings + "\n(自派侦察兵已交卷;蒸馏物已落库,日志:" +
			filepath.ToSlash(filepath.Join(".knowledge", "local", "scout-"+job.ID+".log")) + ")"), nil
	case err := <-exited:
		// 进程退了还没交卷:再宽限一拍(交卷 HTTP 可能竞态在退程前一瞬)。
		select {
		case findings := <-job.done:
			return framed(findings + "\n(自派侦察兵已交卷)"), nil
		case <-time.After(2 * time.Second):
		}
		return "", kbErr("SCOUT_TIMEOUT",
			fmt.Sprintf("侦察兵进程退出未交卷(%v)", err),
			"看日志 .knowledge/local/scout-"+job.ID+".log;job 仍在(TTL 30 分钟),迟到交卷会落库,用 kb_status 查看")
	case <-time.After(timeout):
		pty.KillGroup(cmd)
		return "", kbErr("SCOUT_TIMEOUT",
			fmt.Sprintf("侦察兵超时未交卷(%s)", timeout),
			"用 kb_status 查看已落库蒸馏物;job 仍在(TTL 30 分钟),迟到交卷不白费")
	}
}

// mcpServerEntry 组装 scout 的 MCP server 配置;serve --auth 时带令牌头。
func mcpServerEntry(url string, s *store.Store) map[string]any {
	entry := map[string]any{"type": "http", "url": url}
	if tok, _ := s.LoadAuthToken(); tok != "" {
		entry["headers"] = map[string]string{"Authorization": "Bearer " + tok}
	}
	return entry
}

// drainToLog 把 PTY 输出汇进日志文件,上限 cap 字节(超限丢弃,防失控进程刷盘)。
func drainToLog(r interface{ Read([]byte) (int, error) }, path string, capBytes int64) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	var written int64
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 && written < capBytes {
			m := int64(n)
			if written+m > capBytes {
				m = capBytes - written
			}
			f.Write(buf[:m])
			written += m
		}
		if err != nil {
			return
		}
	}
}
