package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/zdypro888/iknowledge/internal/engine"
	"github.com/zdypro888/iknowledge/internal/semantic"
	"github.com/zdypro888/iknowledge/internal/store"
)

const semanticUsage = `用法:
  iknowledge semantic configure --repo <path> --endpoint <url> --model <name>
      [--dimensions 0] [--revision <id>] [--query-profile auto|plain|qwen3-code-v1]
      [--rebuild-policy manual|ai-local|ai-remote]
      [--top-k 20] [--min-score 0.35]
      [--max-vector-mib 512] [--timeout 30]
  iknowledge semantic enable  --repo <path>
  iknowledge semantic disable --repo <path>
  iknowledge semantic status  --repo <path>
  iknowledge semantic rebuild --repo <path>
  iknowledge semantic clear   --repo <path>

configure 只保存并启用本机配置，不会调用 embedding 服务或自动重建索引。
rebuild 是唯一会显式调用 embedding 服务的子命令。
远程 API key 如需仅从 IKNOWLEDGE_EMBEDDING_API_KEY 读取；非空时还须用
IKNOWLEDGE_EMBEDDING_API_ORIGIN 绑定唯一 scheme://host[:port]。
`

const semanticRebuildTimeout = 30 * time.Minute

func runSemantic(args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(errOut, semanticUsage)
		return 2
	}
	switch args[0] {
	case "configure":
		return runSemanticConfigure(args[1:], out, errOut)
	case "enable":
		return runSemanticToggle(args[1:], true, out, errOut)
	case "disable":
		return runSemanticToggle(args[1:], false, out, errOut)
	case "status":
		return runSemanticStatus(args[1:], out, errOut)
	case "rebuild":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		ctx, cancel := context.WithTimeout(ctx, semanticRebuildTimeout)
		defer cancel()
		return runSemanticRebuild(ctx, args[1:], out, errOut)
	case "clear":
		return runSemanticClear(args[1:], out, errOut)
	case "help", "-h", "--help":
		if len(args) != 1 {
			fmt.Fprintln(errOut, "错误: semantic help 不接受 positional 参数")
			return 2
		}
		fmt.Fprint(out, semanticUsage)
		return 0
	default:
		fmt.Fprintf(errOut, "错误: 未知 semantic 子命令 %q\n%s", args[0], semanticUsage)
		return 2
	}
}

func runSemanticConfigure(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("semantic configure", flag.ContinueOnError)
	fs.SetOutput(errOut)
	repo := fs.String("repo", ".", "仓库路径")
	endpoint := fs.String("endpoint", "", "OpenAI-compatible embedding endpoint")
	model := fs.String("model", "", "embedding 模型名")
	dimensions := fs.Int("dimensions", 0, "输出维度;0 表示使用模型缺省值")
	revision := fs.String("revision", "", "用户维护的模型修订标识(换模型时更新)")
	queryProfile := fs.String("query-profile", "auto", "query 预处理: auto|plain|qwen3-code-v1")
	rebuildPolicy := fs.String("rebuild-policy", "", "MCP 显式同步授权: manual|ai-local|ai-remote")
	topK := fs.Int("top-k", 0, "语义候选数")
	minScore := fs.Float64("min-score", 0, "最低余弦相似度")
	maxVectorMiB := fs.Int("max-vector-mib", 0, "连续向量 payload 上限(MiB)")
	timeoutSec := fs.Int("timeout", 0, "单次 embedding HTTP 超时(秒)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if semanticRejectUnexpectedArgs(fs, errOut) {
		return 2
	}

	s, err := openSemanticStore(*repo)
	if err != nil {
		fmt.Fprintln(errOut, "错误:", err)
		return 1
	}
	cfg, err := engine.LoadSemanticSettings(s)
	if err != nil {
		fmt.Fprintln(errOut, "错误:", err)
		return 1
	}
	seen := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if seen["endpoint"] {
		cfg.Endpoint = *endpoint
	}
	if seen["model"] {
		cfg.Model = *model
	}
	if seen["dimensions"] {
		cfg.Dimensions = *dimensions
	}
	if seen["revision"] {
		cfg.Revision = *revision
	}
	if seen["query-profile"] || seen["model"] {
		// Changing the model without an explicit profile deliberately re-runs
		// auto selection. This prevents a Qwen-only instruction from silently
		// surviving a switch to an unrelated model (and vice versa).
		requested := *queryProfile
		if !seen["query-profile"] {
			requested = "auto"
		}
		cfg.QueryProfile, err = resolveSemanticQueryProfile(requested, cfg.Model)
		if err != nil {
			fmt.Fprintln(errOut, "错误: semantic 配置无效:", err)
			return 1
		}
	}
	if seen["rebuild-policy"] {
		cfg.RebuildPolicy, err = parseSemanticRebuildPolicy(*rebuildPolicy)
		if err != nil {
			fmt.Fprintln(errOut, "错误: semantic 配置无效:", err)
			return 1
		}
	}
	if seen["top-k"] {
		cfg.TopK = *topK
	}
	if seen["min-score"] {
		cfg.MinScore = *minScore
	}
	if seen["max-vector-mib"] {
		cfg.MaxVectorMiB = *maxVectorMiB
	}
	if seen["timeout"] {
		cfg.TimeoutSec = *timeoutSec
	}
	cfg.Enabled = true
	if err := engine.SaveSemanticSettings(s, cfg); err != nil {
		fmt.Fprintln(errOut, "错误: semantic 配置无效:", err)
		return 1
	}

	// 重新读回获取 normalize 后的真实值，回执不显示用户输入的假状态。
	cfg, err = engine.LoadSemanticSettings(s)
	if err != nil {
		fmt.Fprintln(errOut, "错误: semantic 配置回读失败:", err)
		return 1
	}
	fmt.Fprintf(out, "已保存并启用 semantic 本机配置(未重建索引，未调用 embedding 服务)。\nendpoint: %s\nmodel: %s\nquery_profile: %s\nrebuild_policy: %s\ndimensions: %d(auto=0)\n", cfg.Endpoint, cfg.Model, cfg.QueryProfile, cfg.RebuildPolicy, cfg.Dimensions)
	fmt.Fprintf(out, "远程 API key 如需仅从 %s 读取，不会写入仓库或配置文件；非空时须用 %s 绑定唯一 provider origin。\n",
		engine.SemanticAPIKeyEnv, engine.SemanticAPIOriginEnv)
	fmt.Fprintf(out, "确认服务/模型后显式执行: iknowledge semantic rebuild --repo %s\n", s.RepoRoot())
	return 0
}

func resolveSemanticQueryProfile(requested, model string) (semantic.QueryProfile, error) {
	switch strings.ToLower(strings.TrimSpace(requested)) {
	case "auto":
		if strings.Contains(strings.ToLower(strings.TrimSpace(model)), "qwen3-embedding") {
			return semantic.QueryProfileQwen3CodeV1, nil
		}
		return semantic.QueryProfilePlain, nil
	case string(semantic.QueryProfilePlain):
		return semantic.QueryProfilePlain, nil
	case string(semantic.QueryProfileQwen3CodeV1):
		return semantic.QueryProfileQwen3CodeV1, nil
	default:
		return "", fmt.Errorf("query-profile=%q 不受支持；可选 auto、plain、qwen3-code-v1", requested)
	}
}

func parseSemanticRebuildPolicy(raw string) (engine.SemanticRebuildPolicy, error) {
	policy := engine.SemanticRebuildPolicy(strings.ToLower(strings.TrimSpace(raw)))
	switch policy {
	case engine.SemanticRebuildManual, engine.SemanticRebuildAILocal, engine.SemanticRebuildAIRemote:
		return policy, nil
	default:
		return "", fmt.Errorf("rebuild-policy=%q 不受支持；可选 manual、ai-local、ai-remote", raw)
	}
}

func runSemanticToggle(args []string, enabled bool, out, errOut io.Writer) int {
	name := "disable"
	if enabled {
		name = "enable"
	}
	fs := flag.NewFlagSet("semantic "+name, flag.ContinueOnError)
	fs.SetOutput(errOut)
	repo := fs.String("repo", ".", "仓库路径")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if semanticRejectUnexpectedArgs(fs, errOut) {
		return 2
	}
	s, err := openSemanticStore(*repo)
	if err != nil {
		fmt.Fprintln(errOut, "错误:", err)
		return 1
	}
	cfg, err := engine.LoadSemanticSettings(s)
	if err != nil {
		fmt.Fprintln(errOut, "错误:", err)
		return 1
	}
	cfg.Enabled = enabled
	if err := engine.SaveSemanticSettings(s, cfg); err != nil {
		if enabled && cfg.Endpoint == "" && cfg.Model == "" {
			fmt.Fprintln(errOut, "错误: semantic 尚未配置；先运行 iknowledge semantic configure --endpoint <url> --model <name>")
		} else {
			fmt.Fprintln(errOut, "错误:", err)
		}
		return 1
	}
	state := "禁用"
	if enabled {
		state = "启用"
	}
	fmt.Fprintf(out, "semantic 已%s；未调用 embedding 服务。\n", state)
	return 0
}

func runSemanticStatus(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("semantic status", flag.ContinueOnError)
	fs.SetOutput(errOut)
	repo := fs.String("repo", ".", "仓库路径")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if semanticRejectUnexpectedArgs(fs, errOut) {
		return 2
	}
	s, err := openSemanticStore(*repo)
	if err != nil {
		fmt.Fprintln(errOut, "错误:", err)
		return 1
	}
	text, err := engine.New(s).SemanticStatusText()
	if err != nil {
		fmt.Fprintln(errOut, "错误:", err)
		return 1
	}
	fmt.Fprintln(out, text)
	return 0
}

func runSemanticRebuild(ctx context.Context, args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("semantic rebuild", flag.ContinueOnError)
	fs.SetOutput(errOut)
	repo := fs.String("repo", ".", "仓库路径")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if semanticRejectUnexpectedArgs(fs, errOut) {
		return 2
	}
	if ctx == nil {
		fmt.Fprintln(errOut, "错误: semantic rebuild 需要有效 context")
		return 1
	}
	s, err := openSemanticStore(*repo)
	if err != nil {
		fmt.Fprintln(errOut, "错误:", err)
		return 1
	}
	report, err := engine.New(s).RebuildSemantic(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			fmt.Fprintln(errOut, "semantic 重建已取消:", err)
		} else {
			fmt.Fprintln(errOut, "错误:", err)
		}
		return 1
	}
	fmt.Fprintln(out, report.Text())
	return 0
}

func runSemanticClear(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("semantic clear", flag.ContinueOnError)
	fs.SetOutput(errOut)
	repo := fs.String("repo", ".", "仓库路径")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if semanticRejectUnexpectedArgs(fs, errOut) {
		return 2
	}
	s, err := openSemanticStore(*repo)
	if err != nil {
		fmt.Fprintln(errOut, "错误:", err)
		return 1
	}
	if err := engine.New(s).ClearSemanticIndex(); err != nil {
		fmt.Fprintln(errOut, "错误:", err)
		return 1
	}
	fmt.Fprintln(out, "semantic 磁盘派生索引已清理；provider 配置保留。运行中的 serve 会在下次 status/recall 时检测删除并立即淘汰旧快照；需禁止后续 provider 查询请另执行 semantic disable。")
	return 0
}

func openSemanticStore(repo string) (*store.Store, error) {
	s, err := store.Open(repo)
	if err != nil {
		return nil, err
	}
	if !s.Initialized() {
		return nil, fmt.Errorf("库未初始化，先跑 iknowledge init --repo %s", s.RepoRoot())
	}
	return s, nil
}

func semanticRejectUnexpectedArgs(fs *flag.FlagSet, errOut io.Writer) bool {
	if fs.NArg() == 0 {
		return false
	}
	fmt.Fprintf(errOut, "错误: %s 不接受 positional 参数: %s\n", fs.Name(), strings.Join(fs.Args(), " "))
	return true
}
