// iknowledge:AI 代码知识库(MCP 服务)。薄 main:CLI 解析与装配(impl §1/§2)。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/zdypro888/iknowledge/internal/engine"
	"github.com/zdypro888/iknowledge/internal/mcpserv"
	"github.com/zdypro888/iknowledge/internal/store"
)

const usage = `iknowledge——AI 代码知识库(MCP 服务)

用法:
  iknowledge init   --repo <path> [--force] [--reanchor-all]   骨架秒建/对账(纯 AST,零 LLM)
  iknowledge serve  --repo <path> [--addr host:port]           启动 MCP 服务
  iknowledge status --repo <path> [--prompt]                   库状态;--prompt 打印纪律提示词
  iknowledge setup  --repo <path>                              打印接入三件套(.mcp.json/纪律段/hook),只打印不代写
  iknowledge hook   [--repo <path>]                            宿主 hook 桥(Claude Code PostToolUse):注入所触文件的知识
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
	case "status":
		return runStatus(args[1:])
	case "setup":
		return runSetup(args[1:], os.Stdout)
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
	repo := fs.String("repo", ".", "仓库路径")
	addr := fs.String("addr", "", "监听地址(缺省 127.0.0.1:<config 端口>)")
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
	// 写者互斥(impl §4):serve 启动时取 flock 并持有;第二个 serve/并行 CLI init 被挡。
	release, err := s.AcquireWriterLock()
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	defer release()

	listen := *addr
	if listen == "" {
		cfg, err := s.EnsureConfig()
		if err != nil {
			fmt.Fprintln(os.Stderr, "错误:", err)
			return 1
		}
		listen = fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	}
	e := engine.New(s)
	if err := e.EnsureRuntime(); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	srv := mcpserv.New(e)
	// 端口被占(哈希撞车):启动即报错并提示改 config,不静默换端口(impl §1)。
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: 监听 %s 失败(%v)——端口被占时改 .knowledge/config.yaml 的 port 或用 --addr\n", listen, err)
		return 1
	}
	// 无鉴权服务的信任模型是"仅回环"(impl §1);监听非回环时 Origin 校验挡不住
	// 直连的非浏览器客户端,必须显式警示而不是默默裸奔。
	if host, _, err := net.SplitHostPort(listen); err == nil {
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			fmt.Fprintln(os.Stderr, "警告: 监听非回环地址——服务无鉴权,Origin 校验不构成认证,任何能连通该端口的主机都可读写知识库;仅限可信隔离网络使用。")
		}
	}
	fmt.Printf("knowledge MCP 已启动:http://%s/mcp/main?repo=%s\nhook 注入端点:http://%s/inject?file=<path>\n",
		listen, url.QueryEscape(s.RepoRoot()), listen)
	// ReadHeaderTimeout:防半开连接无限占用(R2-D3);业务无流式/长轮询,10s 富余。
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	// SIGINT/SIGTERM 优雅停机:等在途工具调用落完盘再退(记账是多步文件写,
	// 硬杀有原子写兜底不会损坏,但会截断正在进行的记账链)。stop() 后信号恢复
	// 默认处置:停机卡住时再来一次 Ctrl+C/SIGTERM 即强杀。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() { errCh <- hs.Serve(ln) }()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "错误:", err)
			return 1
		}
	case <-ctx.Done():
		stop()
		fmt.Println("收到退出信号,优雅停机中(等待在途请求,上限 10s)…")
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := hs.Shutdown(sctx); err != nil {
			fmt.Fprintln(os.Stderr, "错误: 优雅停机未在时限内完成:", err)
			return 1
		}
	}
	return 0
}

// runVersion 版本自报(运维排障:确认在跑哪个构建)。全部取构建元数据,
// 不维护手写版本常量——发布忘更新的散落硬编码比没有版本号更误导。
func runVersion() int {
	version, revision, dirty := "(devel)", "", false
	if bi, ok := debug.ReadBuildInfo(); ok {
		if bi.Main.Version != "" {
			version = bi.Main.Version
		}
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				revision = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
	}
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
然后运行:iknowledge serve --repo %s
完整接入三件套(含 CLAUDE.md 纪律段与 hook 自动注入):iknowledge setup --repo %s
`, s.RepoRoot(), mcpJSONSnippet(s.RepoRoot(), cfg.Port), s.RepoRoot(), s.RepoRoot())
	}
	return 0
}
