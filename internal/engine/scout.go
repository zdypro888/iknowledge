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

	cmd := scoutCommand(cfg.ScoutCommand, cfgPath)
	cmd.Dir = e.Store.RepoRoot()
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return "", kbErr("SCOUT_TIMEOUT", "侦察兵进程启动失败:"+err.Error(),
			"检查 config 的 scout_command;或改回委派模式(scout: delegate)")
	}
	defer ptmx.Close()
	defer pty.KillGroup(cmd)

	// 输出旁路进日志(有界),同时扫 spinner/响应符号做"回合已启动"信号
	//(kbeval 排障定案:欢迎屏无这些符号;首启信任弹窗会吞输入,须检测 + 重投)。
	logPath := filepath.Join(e.Store.Dir(), "local", "scout-"+job.ID+".log")
	activity := make(chan struct{}, 1)
	go drainToLog(ptmx, logPath, 1<<20, activity)
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	// 喂简报:括号粘贴包裹(多行文本裸 \n 会被 TUI 当回车逐行提交,kbeval 排障定案)
	// + 两步输入(正文、停顿、单发回车,对抗 paste 突发去抖,aibridge driver.go 同法)。
	time.Sleep(6 * time.Second) // TUI 启动等待
	submit := func() {
		if _, err := ptmx.Write([]byte("\x1b[200~" + brief + "\x1b[201~")); err == nil {
			time.Sleep(2 * time.Second)
			ptmx.Write([]byte("\r"))
		}
	}
	submit()

	// 启动确认:25s 无活动符号 → 单发回车收弹窗 + 重投(上限 2 次)。
	// 同时监听交卷/退程——非 TUI 侦察兵(自定义 scout_command)可能不画 spinner 直接交卷。
	started := false
	for resends := 0; !started; {
		select {
		case <-activity:
			started = true
		case findings := <-job.done:
			pty.KillGroup(cmd)
			return framed(findings + "\n(自派侦察兵已交卷;蒸馏物已落库,日志:" +
				filepath.ToSlash(filepath.Join(".knowledge", "local", "scout-"+job.ID+".log")) + ")"), nil
		case err := <-exited:
			select { // 退程宽限:交卷 HTTP 可能竞态在退程前一瞬
			case findings := <-job.done:
				return framed(findings + "\n(自派侦察兵已交卷)"), nil
			case <-time.After(2 * time.Second):
			}
			return "", kbErr("SCOUT_TIMEOUT",
				fmt.Sprintf("侦察兵进程启动即退出(%v)", err),
				"看日志 .knowledge/local/scout-"+job.ID+".log;检查 scout_command")
		case <-time.After(25 * time.Second):
			if resends == 2 {
				pty.KillGroup(cmd)
				return "", kbErr("SCOUT_TIMEOUT",
					"侦察兵未能启动回合(重投 2 次无响应;新目录首启的信任确认可能需要人工跑一次交互式 claude)",
					"在该仓库手动跑一次 claude 完成目录信任后重试;或看日志 .knowledge/local/scout-"+job.ID+".log")
			}
			resends++
			ptmx.Write([]byte("\r"))
			time.Sleep(3 * time.Second)
			submit()
		}
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

func scoutCommand(configured, cfgPath string) *exec.Cmd {
	if strings.TrimSpace(configured) == "" {
		return exec.Command("claude", "--mcp-config", cfgPath, "--strict-mcp-config", "--allowedTools", "mcp__knowledge__*")
	}
	command := strings.ReplaceAll(configured, "{mcp}", cfgPath)
	return exec.Command("sh", "-c", command)
}

// scoutActivityMarks 是"回合已启动"的屏幕信号:spinner/响应符只在回合运行时出现,
// 欢迎屏没有(kbeval 排障定案;字节流速会被输入框回显假触发,不可用)。
var scoutActivityMarks = []string{"✻", "✶", "✳", "✽", "⏺"}

// drainToLog 把 PTY 输出汇进日志文件,上限 cap 字节(超限丢弃,防失控进程刷盘);
// 首次扫到活动符号时向 activity 发一次信号(非阻塞,通道可为 nil)。
func drainToLog(r interface{ Read([]byte) (int, error) }, path string, capBytes int64, activity chan<- struct{}) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	signaled := false
	var written int64
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if !signaled && activity != nil {
				chunk := string(buf[:n])
				for _, m := range scoutActivityMarks {
					if strings.Contains(chunk, m) {
						signaled = true
						select {
						case activity <- struct{}{}:
						default:
						}
						break
					}
				}
			}
			if written < capBytes {
				m := int64(n)
				if written+m > capBytes {
					m = capBytes - written
				}
				f.Write(buf[:m])
				written += m
			}
		}
		if err != nil {
			return
		}
	}
}
