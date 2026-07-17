// iknowledge:AI 代码知识库(MCP 服务)。薄 main:CLI 解析与装配(impl §1/§2)。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/zdypro888/iknowledge/internal/buildinfo"
	"github.com/zdypro888/iknowledge/internal/engine"
	"github.com/zdypro888/iknowledge/internal/mcpserv"
	"github.com/zdypro888/iknowledge/internal/store"
)

const usage = `iknowledge——AI 代码知识库(MCP 服务)

用法:
  iknowledge init   --repo <path> [--force] [--reanchor-all]   骨架秒建/对账(纯 AST,零 LLM)
  iknowledge stdio  --repo <path>                              MCP stdio 桥(推荐接入形态:客户端拉起,自动带起后台 serve)
  iknowledge serve  --repo <path> [--repo <path2> …] [--auth]  启动 MCP 服务;--repo 可重复(单进程多仓库);--auth 启用 token 鉴权
                    [--addr host:port]                         (仅单仓库)覆盖监听地址
  iknowledge status --repo <path> [--prompt]                   库状态;--prompt 打印纪律提示词
  iknowledge doctor --repo <path> [--deploy] [--strict]        自检:初始化/配置/parser/维护欠账/PATH 部署
  iknowledge maintain --repo <path> [--plan]                   打印维护欠账清单/路线(只读;取用/销账走 MCP kb_maintain)
  iknowledge brief --repo <path> [--budget 1200]               一屏项目简报(WIP/风险/近期决策/维护债)
  iknowledge precheck --repo <path> [--working] [--strict]     提交前检查已知否决/腐烂/矛盾/记账;缺省仅告警
  iknowledge setup  --repo <path>                              打印 MCP/纪律/hook/pre-commit 接入片段,只打印不代写
  iknowledge hook   [--repo <path>]                            宿主 hook 桥(Claude Code PostToolUse):注入所触文件的知识
  iknowledge export  --repo <path> [-o file.kbundle]           导出知识为 .kbundle(tar.gz;备份/迁移;缺省输出 stdout)
  iknowledge import  --repo <path> -i file.kbundle [--dry-run] [--backup] [--remap from=to]  导入 .kbundle(跨仓迁移用 --remap 重映射路径前缀)
  iknowledge version                                           版本自报(排障:确认在跑哪个构建)
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "init":
		return runInit(args[1:])
	case "serve":
		return runServe(args[1:])
	case "stdio":
		return runStdio(args[1:], os.Stdin, os.Stdout)
	case "status":
		return runStatus(args[1:])
	case "doctor":
		return runDoctor(args[1:], os.Stdout)
	case "maintain":
		return runMaintain(args[1:], os.Stdout)
	case "brief":
		return runBrief(args[1:], os.Stdout)
	case "precheck":
		return runPrecheck(args[1:], os.Stdout)
	case "setup":
		return runSetup(args[1:], os.Stdout)
	case "export":
		return runExport(args[1:])
	case "import":
		return runImport(args[1:])
	case "hook":
		return runHook(args[1:], os.Stdin, os.Stdout)
	case "version", "-v", "--version":
		return runVersion()
	case "-h", "--help", "help":
		fmt.Print(usage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "未知子命令 %q\n%s", args[0], usage)
		return 2
	}
}

func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	var repos []string
	fs.Func("repo", "仓库路径(可重复:单进程服务多个仓库,impl §1 多 repo 修订)", func(v string) error {
		repos = append(repos, v)
		return nil
	})
	addr := fs.String("addr", "", "监听地址(缺省 127.0.0.1:<config 端口>;仅单仓库可用)")
	auth := fs.Bool("auth", false, "启用 Bearer 鉴权(每仓 token 生成于各自 .knowledge/local/token,0600;共享多用户机器用)")
	allowInsecure := fs.Bool("allow-insecure-bind", false, "确认监听非回环地址的风险(无 --auth 时裸奔于网络;仅限可信隔离网络)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if len(repos) == 0 {
		repos = []string{"."}
	}
	if *addr != "" && len(repos) > 1 {
		fmt.Fprintln(os.Stderr, "错误: --addr 与多 --repo 互斥(多仓库各用自己 config 的端口,客户端配置才不用改)")
		return 2
	}

	// 多 repo 单守护(impl §1 修订,原四期):一进程多监听——每仓保留自己的端口与
	// 写者锁,既有客户端配置(.mcp.json/hook 按仓库端口发现)零改动;消掉的只是
	// "每仓一个进程"的管理负担。
	type unit struct {
		s      *store.Store
		hs     *http.Server
		ln     net.Listener
		listen string
	}
	var units []unit
	cleanup := func() {
		for _, u := range units {
			if u.ln != nil {
				_ = u.ln.Close() // best-effort cleanup; startup errors are reported separately
			}
		}
	}
	seen := map[string]bool{}
	for _, repo := range repos {
		s, err := store.Open(repo)
		if err != nil {
			fmt.Fprintln(os.Stderr, "错误:", err)
			cleanup()
			return 1
		}
		if seen[s.RepoRoot()] {
			continue // 同仓库重复传参:幂等去重
		}
		seen[s.RepoRoot()] = true
		if !s.Initialized() {
			fmt.Fprintln(os.Stderr, "错误: 库未初始化,先跑 iknowledge init --repo "+s.RepoRoot())
			cleanup()
			return 1
		}
		// 写者互斥(impl §4):serve 启动时取 flock 并持有;第二个 serve/并行 CLI init 被挡。
		release, err := s.AcquireWriterLock()
		if err != nil {
			fmt.Fprintf(os.Stderr, "错误(%s): %v\n", s.RepoRoot(), err)
			cleanup()
			return 1
		}
		defer release()

		listen := *addr
		if listen == "" {
			cfg, err := s.EnsureConfig()
			if err != nil {
				fmt.Fprintln(os.Stderr, "错误:", err)
				cleanup()
				return 1
			}
			listen = fmt.Sprintf("127.0.0.1:%d", cfg.Port)
		}
		e := engine.New(s)
		if err := e.EnsureRuntime(); err != nil {
			fmt.Fprintf(os.Stderr, "错误(%s): %v\n", s.RepoRoot(), err)
			cleanup()
			return 1
		}
		srv := mcpserv.New(e)
		if *auth {
			tok, err := s.EnsureAuthToken()
			if err != nil {
				fmt.Fprintln(os.Stderr, "错误:", err)
				cleanup()
				return 1
			}
			srv.AuthToken = tok
			fmt.Printf("鉴权已启用(%s):token 在 .knowledge/local/token(0600);重跑 iknowledge setup 取带 headers 的接入片段。\n", s.RepoRoot())
		}
		// 端口被占(哈希撞车):启动即报错并提示改 config,不静默换端口(impl §1)。
		ln, err := net.Listen("tcp", listen)
		if err != nil {
			fmt.Fprintf(os.Stderr, "错误: 监听 %s 失败(%v)——端口被占时改 %s/.knowledge/config.yaml 的 port 或用 --addr\n", listen, err, s.RepoRoot())
			cleanup()
			return 1
		}
		e.SetScoutAddr(listen) // 自派侦察兵回连实际监听地址(--addr 覆盖时 config 端口不可信)
		// R29-S1.5:无鉴权服务的信任模型是"仅回环"(impl §1)。监听非回环时,Origin 校验
		// 挡不住直连的非浏览器客户端(curl/任何本机进程)——以前只 warn 就放行,现在强制:
		// 非 loopback 且无 --auth,必须显式 --allow-insecure-bind 才启动,否则拒绝(退出 2)。
		// 这把"一条 flag 之差就网络裸奔"的失误从可能变成不可能。
		if host, _, err := net.SplitHostPort(listen); err == nil {
			ip := net.ParseIP(host)
			nonLoopback := host != "localhost" && (ip == nil || !ip.IsLoopback())
			if nonLoopback {
				if !*auth && !*allowInsecure {
					fmt.Fprintln(os.Stderr, "错误: 监听非回环地址且无 --auth——任何能连通该端口的主机都可读写知识库。")
					fmt.Fprintln(os.Stderr, "若确为可信隔离网络,加 --allow-insecure-bind 显式确认此风险;否则用 --auth 或保持缺省回环监听。")
					cleanup()
					return 2
				}
				if *auth {
					fmt.Fprintln(os.Stderr, "警告: 监听非回环地址——token 鉴权已启用,但明文 HTTP 仍可被网络窃听(含 token 本身);仅限可信隔离网络使用。")
				} else {
					fmt.Fprintln(os.Stderr, "警告: 监听非回环地址且无鉴权(--allow-insecure-bind 已确认)——任何能连通该端口的主机都可读写知识库。")
				}
			}
		}
		fmt.Printf("knowledge MCP 已启动:http://%s/mcp/main?repo=%s\nhook 注入端点:http://%s/inject?file=<path>\n",
			listen, url.QueryEscape(s.RepoRoot()), listen)
		// R29-S1.4:HTTP 服务器超时硬化(防 slowloris / 慢客户端无限占用 goroutine)。
		// ReadHeaderTimeout 10s 防半开连接(R2-D3,原有);ReadTimeout 30s 限制请求体读取;
		// IdleTimeout 120s 回收 keep-alive 空闲连接。WriteTimeout 不在 Server 层设——
		// scout 自派路由(/mcp/scout/ 的 kb_investigate selfDispatch)可阻塞最长
		// ScoutTimeoutSec(缺省 300s)等侦察兵交卷,全局 WriteTimeout 会误杀。
		// 改在快端点(/inject /recall /map /status /mcp/main)用 srv.scoutExempt 区分:
		// 那些路由 handler 内自带短超时;scout 路由显式解除 WriteDeadline(见 server.go)。
		units = append(units, unit{s: s, ln: ln, listen: listen,
			hs: &http.Server{
				Handler:           srv.Handler(),
				ReadHeaderTimeout: 10 * time.Second,
				ReadTimeout:       30 * time.Second,
				IdleTimeout:       120 * time.Second,
				WriteTimeout:      0, // 见上:scout 长路由在 handler 层自管
				MaxHeaderBytes:    1 << 20,
			}})
	}

	// SIGINT/SIGTERM 优雅停机:等在途工具调用落完盘再退(记账是多步文件写,
	// 硬杀有原子写兜底不会损坏,但会截断正在进行的记账链)。stop() 后信号恢复
	// 默认处置:停机卡住时再来一次 Ctrl+C/SIGTERM 即强杀。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, len(units))
	for _, u := range units {
		go func() { errCh <- u.hs.Serve(u.ln) }()
	}
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "错误:", err)
			cleanup()
			return 1
		}
	case <-ctx.Done():
		stop()
		fmt.Println("收到退出信号,优雅停机中(等待在途请求,上限 10s)…")
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, u := range units {
			if err := u.hs.Shutdown(sctx); err != nil {
				fmt.Fprintln(os.Stderr, "错误: 优雅停机未在时限内完成:", err)
				return 1
			}
		}
	}
	return 0
}

// runVersion 版本自报(运维排障:确认在跑哪个构建)。全部取构建元数据,
// 不维护手写版本常量——发布忘更新的散落硬编码比没有版本号更误导。
func runVersion() int {
	info := buildinfo.Read()
	version, revision, dirty := info.Version, info.Revision, info.Dirty
	if len(revision) > 12 {
		revision = revision[:12]
	}
	out := "iknowledge " + version
	if revision != "" {
		out += " (" + revision
		if dirty {
			out += "+dirty"
		}
		out += ")"
	}
	fmt.Println(out + " " + runtime.Version())
	return 0
}

func runStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	repo := fs.String("repo", ".", "仓库路径")
	prompt := fs.Bool("prompt", false, "打印纪律提示词(粘贴进 CLAUDE.md / codex 指令)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *prompt {
		fmt.Println(engine.DisciplinePrompt)
		return 0
	}
	s, err := store.Open(*repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	text, err := engine.New(s).Status()
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	fmt.Println(text)
	return 0
}

func runDoctor(args []string, w io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	repo := fs.String("repo", ".", "仓库路径")
	deploy := fs.Bool("deploy", false, "检查当前 iknowledge 二进制与常见部署路径")
	strict := fs.Bool("strict", false, "发现警告时返回非 0")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	s, err := store.Open(*repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	rep, err := engine.New(s).Doctor()
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	if _, err := fmt.Fprintln(w, rep.Text()); err != nil {
		fmt.Fprintln(os.Stderr, "错误: 写 doctor 输出:", err)
		return 1
	}
	warnings := len(rep.Warnings)
	if *deploy {
		text, n := deployDoctorText()
		if text != "" {
			if _, err := fmt.Fprintln(w, text); err != nil {
				fmt.Fprintln(os.Stderr, "错误: 写 doctor 输出:", err)
				return 1
			}
		}
		warnings += n
	}
	if *strict && warnings > 0 {
		return 1
	}
	return 0
}

func deployDoctorText() (string, int) {
	var b strings.Builder
	warnings := 0
	b.WriteString("deploy:\n")
	if p, err := exec.LookPath("iknowledge"); err == nil {
		fmt.Fprintf(&b, "  PATH iknowledge: %s\n", p)
		if real, err := filepath.EvalSymlinks(p); err == nil && real != p {
			fmt.Fprintf(&b, "    -> %s\n", real)
		}
	} else {
		warnings++
		b.WriteString("  ⚠ PATH 中找不到 iknowledge\n")
	}
	home, _ := os.UserHomeDir()
	paths := []string{"/opt/homebrew/bin/iknowledge", "/usr/local/bin/iknowledge"}
	if home != "" {
		paths = append([]string{filepath.Join(home, "go/bin/iknowledge"), filepath.Join(home, ".local/bin/iknowledge")}, paths...)
	}
	for _, p := range paths {
		st, err := os.Lstat(p)
		if err != nil {
			continue
		}
		mode := "file"
		if st.Mode()&os.ModeSymlink != 0 {
			mode = "symlink"
		}
		fmt.Fprintf(&b, "  %s: %s", p, mode)
		if real, err := filepath.EvalSymlinks(p); err == nil && real != p {
			fmt.Fprintf(&b, " -> %s", real)
		}
		b.WriteString("\n")
	}
	if out, err := exec.Command("pgrep", "-fl", "iknowledge serve").Output(); err == nil && strings.TrimSpace(string(out)) != "" {
		warnings++
		b.WriteString("  ⚠ 检测到 iknowledge serve 进程(若客户端应自行拉起,不需要服务模式):\n")
		for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
			fmt.Fprintf(&b, "    %s\n", line)
		}
	}
	return strings.TrimRight(b.String(), "\n"), warnings
}

// runMaintain 打印维护欠账清单(knowledge.md §12.7 的 CLI 侧只读落地):
// 会话外看清欠了什么账;清账动作仍由 AI 会话经 kb_maintain 完成(需要语言能力)。
func runMaintain(args []string, w io.Writer) int {
	fs := flag.NewFlagSet("maintain", flag.ContinueOnError)
	repo := fs.String("repo", ".", "仓库路径")
	patrol := fs.Bool("patrol", false, "打印跨节点矛盾巡检简报(只读;裁决仍由 AI 会话经 kb_verify/kb_remember 完成)")
	plan := fs.Bool("plan", false, "按欠账类型打印维护路线")
	scope := fs.String("scope", "", "巡检范围(路径前缀,仅 -patrol 用)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	s, err := store.Open(*repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	if *patrol {
		brief, err := engine.New(s).PatrolBrief(*scope)
		if err != nil {
			fmt.Fprintln(os.Stderr, "错误:", err)
			return 1
		}
		if _, err := fmt.Fprintln(w, brief); err != nil {
			fmt.Fprintln(os.Stderr, "错误: 写 maintain 输出:", err)
			return 1
		}
		return 0
	}
	debts, err := engine.New(s).Debts()
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	if len(debts) == 0 {
		if _, err := fmt.Fprintln(w, "无维护欠账。"); err != nil {
			fmt.Fprintln(os.Stderr, "错误: 写 maintain 输出:", err)
			return 1
		}
		return 0
	}
	if *plan {
		if _, err := fmt.Fprintln(w, renderMaintainPlan(debts)); err != nil {
			fmt.Fprintln(os.Stderr, "错误: 写 maintain 输出:", err)
			return 1
		}
		return 0
	}
	if _, err := fmt.Fprintf(w, "维护欠账 %d 条(清账走 MCP:kb_maintain next → 处理 → complete/dismiss):\n", len(debts)); err != nil {
		fmt.Fprintln(os.Stderr, "错误: 写 maintain 输出:", err)
		return 1
	}
	for _, d := range debts {
		if _, err := fmt.Fprintf(w, "- %s [%s] %s\n  %s\n", d.ID, d.Kind, d.Node, d.Desc); err != nil {
			fmt.Fprintln(os.Stderr, "错误: 写 maintain 输出:", err)
			return 1
		}
	}
	return 0
}

func renderMaintainPlan(debts []engine.Debt) string {
	byKind := map[string]int{}
	for _, d := range debts {
		byKind[d.Kind]++
	}
	var kinds []string
	for k := range byKind {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	var b strings.Builder
	fmt.Fprintf(&b, "维护路线:共 %d 条欠账\n", len(debts))
	for _, k := range kinds {
		fmt.Fprintf(&b, "  - %s: %d\n", k, byKind[k])
	}
	b.WriteString("建议顺序:suspect-reverify → dispute-open/cross-dup/dup-entries → confidence-lag → summary-stale → era-compress/review-overdue\n")
	limit := 8
	if len(debts) < limit {
		limit = len(debts)
	}
	b.WriteString("下一批:\n")
	for i := 0; i < limit; i++ {
		d := debts[i]
		fmt.Fprintf(&b, "  %d. %s [%s] %s\n     %s\n", i+1, d.ID, d.Kind, d.Node, d.Desc)
	}
	return strings.TrimRight(b.String(), "\n")
}

// runExport(R29 批次4):导出 .knowledge/ 为 .kbundle(tar.gz)。
// 用法:iknowledge export --repo <path> -o backup.kbundle
func runExport(args []string) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	repo := fs.String("repo", ".", "仓库路径")
	out := fs.String("o", "", "输出文件(缺省 stdout)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	s, err := store.Open(*repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	if !s.Initialized() {
		fmt.Fprintln(os.Stderr, "错误: 库未初始化")
		return 1
	}
	e := engine.New(s)
	if err := e.EnsureRuntime(); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	var w io.Writer = os.Stdout
	var outFile *os.File
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			fmt.Fprintln(os.Stderr, "错误:", err)
			return 1
		}
		outFile = f
		w = f
	}
	if err := e.Export(w); err != nil {
		if outFile != nil {
			_ = outFile.Close()
		}
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	if outFile != nil {
		if err := outFile.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "错误: 关闭导出文件:", err)
			return 1
		}
	}
	fmt.Fprintf(os.Stderr, "导出完成 → %s\n", outDesc(*out))
	return 0
}

func outDesc(path string) string {
	if path == "" {
		return "stdout"
	}
	return path
}

// runImport(R29 批次4):从 .kbundle(tar.gz)导入知识到目标仓。
// 用法:iknowledge import --repo <path> -i backup.kbundle [--remap internal/auth/=pkg/auth/]
func runImport(args []string) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	repo := fs.String("repo", ".", "目标仓库路径")
	in := fs.String("i", "", "输入文件(缺省 stdin)")
	remapStr := fs.String("remap", "", "路径前缀重映射(跨仓迁移,格式 from=to,可重复用逗号或多次)")
	dryRun := fs.Bool("dry-run", false, "只解析 bundle 并打印将导入的文件,不写盘")
	backup := fs.Bool("backup", false, "导入前备份当前知识库到 .knowledge/local/import-backups/")
	maxEntryMB := fs.Int("max-entry-mb", 16, "单个 bundle 条目大小上限(MB)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	s, err := store.Open(*repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	remap := map[string]string{}
	if *remapStr != "" {
		for _, pair := range strings.Split(*remapStr, ",") {
			if from, to, ok := strings.Cut(pair, "="); ok {
				remap[strings.TrimSpace(from)] = strings.TrimSpace(to)
			}
		}
	}
	var r io.Reader = os.Stdin
	var inFile *os.File
	if *in != "" {
		f, err := os.Open(*in)
		if err != nil {
			fmt.Fprintln(os.Stderr, "错误:", err)
			return 1
		}
		inFile = f
		r = f
	}
	if !*dryRun {
		release, err := s.AcquireWriterLock()
		if err != nil {
			fmt.Fprintln(os.Stderr, "错误:", err)
			return 1
		}
		defer release()
	}
	e := engine.New(s)
	rep, err := e.ImportWithOptions(r, engine.ImportOptions{
		PathRemap:     remap,
		DryRun:        *dryRun,
		Backup:        *backup,
		MaxEntryBytes: int64(*maxEntryMB) << 20,
	})
	if inFile != nil {
		if closeErr := inFile.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("关闭导入文件:%w", closeErr)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, rep.Text())
	if *dryRun {
		fmt.Fprintln(os.Stderr, "dry-run 完成:未写入任何文件。")
	} else {
		fmt.Fprintf(os.Stderr, "导入完成:%d 个文件已写入 %s/.knowledge/(重启 serve 生效)\n", rep.Imported, s.RepoRoot())
	}
	return 0
}

func runInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	repo := fs.String("repo", ".", "仓库路径")
	force := fs.Bool("force", false, "对丢失/受损分片强制重写(仍不动已有 Entries)")
	reanchorAll := fs.Bool("reanchor-all", false,
		"mass-suspect 批量出口:确认全局性变更为预期后,全库按当前代码重锚,suspect 升回 fresh")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	s, err := store.Open(*repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	if err := s.EnsureLayout(); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	// 写者互斥(impl §4):serve 运行中时 CLI init 被挡,提示改用 kb_init。
	release, err := s.AcquireWriterLock()
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	defer release()

	rep, err := engine.New(s).Init(engine.InitOptions{Force: *force, ReanchorAll: *reanchorAll})
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	fmt.Println(rep.Text())

	// agent 接入片段(impl §1 修订:init 只打印,由用户/主 AI 自行粘贴——
	// 工具不写 .knowledge/ 之外的任何文件,铁律二)。
	if cfg, err := s.LoadConfig(); err == nil && cfg != nil {
		fmt.Printf(`
接入:把下面片段粘贴进 %s/.mcp.json(iknowledge 不代写):
%s
(stdio 桥会按需自动拉起后台 serve,无需手动启动)
完整接入三件套(含 CLAUDE.md 纪律段与 hook 自动注入):iknowledge setup --repo %s
`, s.RepoRoot(), mcpJSONSnippet(s.RepoRoot()), s.RepoRoot())
	}
	return 0
}
