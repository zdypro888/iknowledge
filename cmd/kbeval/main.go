// kbeval——M1.4 A/B 验收 harness(impl §11;评测工装,不属产品面,不受产品铁律约束,
// 但同样零第三方依赖、复用 internal/pty)。
//
// 协议(impl §11 定案):种子消化 10 热点 → 固定 10 定位任务(5×N1 + 5×N2)A/B 双跑
// (A=接知识库,B=纯 grep 同模型)→ 判据:A 组中位 token ≤ B 组 60% 且中位轮数不增。
//
// token 采集口径(实测定案,二改):官方 OTEL 遥测 console 导出
// (claude_code.token.usage 累计计数器,混在 PTY 流里,ANSI 剥离后取末次导出)。
// 一改的"转录 JSONL 解析"被否:2.1.201 转录惰性缓冲,短会话连优雅退出都不落盘。
// 回合启动检测 = 提交点后出现 spinner/响应符号(字节流速会被输入框回显假触发);
// 回合结束检测 = 输出流速归零 60s(忙 TUI 每秒重绘,闲 TUI 近零输出)。
// 轮数口径 = claude_code.api_request 事件数(代理循环迭代;logs 导出缺席时 0=不可得)。
// 驱动方式:PTY 交互式 claude(禁 claude -p:独立限流池,CLAUDE.md 铁律)。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zdypro888/iknowledge/internal/pty"
	"gopkg.in/yaml.v3"
)

const usage = `kbeval——iknowledge M1.4 A/B 验收 harness

用法:
  kbeval seed   --repo <目标仓库> [--timeout 900]                 种子消化会话(照 kb_status 热点清单)
  kbeval run    --repo <目标仓库> --tasks <tasks.yaml> --out <目录> [--only n1-1] [--mode both|a|b] [--timeout 480]
  kbeval report --out <目录>                                      汇总结果并判定通过与否

前置:目标仓库已 init 且 serve 运行中(A 组/种子需要);B 组不接知识库。
`

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "seed":
		return runSeed(args[1:])
	case "run":
		return runTasks(args[1:])
	case "report":
		return runReport(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "未知子命令 %q\n%s", args[0], usage)
		return 2
	}
}

// ---- 任务与结果 ----

type task struct {
	ID       string `yaml:"id"`
	Kind     string `yaml:"kind"` // N1 | N2
	Question string `yaml:"question"`
	Answer   string `yaml:"answer"` // 人工判卷用,不喂给模型
}

type taskFile struct {
	Repo  string `yaml:"repo"` // 任务针对的仓库(防拿错清单)
	Tasks []task `yaml:"tasks"`
}

type usageSum struct {
	Input      int `json:"input_tokens"`
	Output     int `json:"output_tokens"`
	CacheWrite int `json:"cache_creation_input_tokens"`
	CacheRead  int `json:"cache_read_input_tokens"`
}

func (u usageSum) total() int { return u.Input + u.Output + u.CacheWrite + u.CacheRead }

type result struct {
	Task       string   `json:"task"`
	Kind       string   `json:"kind"`
	Mode       string   `json:"mode"` // a | b
	Model      string   `json:"model,omitempty"`
	Usage      usageSum `json:"usage"`
	Total      int      `json:"total_tokens"`
	CostUSD    float64  `json:"cost_usd,omitempty"` // claude_code.cost.usage(遥测末次导出)
	Turns      int      `json:"turns"`
	DurationS  int      `json:"duration_seconds"`
	Transcript string   `json:"transcript"`
	StartedAt  string   `json:"started_at"`
	TimedOut   bool     `json:"timed_out,omitempty"`
}

// ---- seed ----

const seedPrompt = `请为本仓库的知识库做一轮种子消化(M1.4 种子步骤):
1. 调 kb_status,取"热点待消化"清单;
2. 对每个热点文件:先 git log -n 5 -- <文件> 看来时路(为什么长这样),再精读原文与关键被调方;
3. 沉淀标准=【代码上看不出来的】:契约/前置条件、坑与易错、来自提交历史的"为什么"。
   禁止复述代码结构("X 函数做了 Y"这种 recall 自带签名与调用关系,存了是噪音)。
   半衰期意识:热点文件改动频繁,优先沉淀跨改动仍成立的契约/不变量,少存实现细节(易腐)。
   每文件 kb_remember:1 条 summary(一句话职责,面向导航)+ ≥1 条 contract/pitfall/usage 洞见;
   顺手把你用过的检索词回填 keywords;
4. 全部完成后再调一次 kb_status,报告覆盖率变化,然后结束。
只读与沉淀,不修改任何源码。`

func runSeed(args []string) int {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	repo := fs.String("repo", "", "目标仓库")
	timeout := fs.Int("timeout", 900, "会话上限秒")
	model := fs.String("model", "", "claude --model 参数(缺省继承用户默认)")
	if err := fs.Parse(args); err != nil || *repo == "" {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	cfgPath, err := mcpConfigA(*repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	defer os.Remove(cfgPath)
	res, err := driveSession(*repo, cfgPath, seedPrompt, *model, time.Duration(*timeout)*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	fmt.Printf("种子消化完成:tokens=%d(in %d/out %d/cacheW %d/cacheR %d)轮=%d 用时=%ds 转录=%s\n",
		res.Total, res.Usage.Input, res.Usage.Output, res.Usage.CacheWrite, res.Usage.CacheRead,
		res.Turns, res.DurationS, res.Transcript)
	return 0
}

// ---- run ----

func runTasks(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	repo := fs.String("repo", "", "目标仓库")
	tasksPath := fs.String("tasks", "", "任务清单 yaml")
	outDir := fs.String("out", "", "结果目录")
	only := fs.String("only", "", "只跑指定任务 ID(逗号分隔)")
	mode := fs.String("mode", "both", "a|b|both")
	timeout := fs.Int("timeout", 480, "单会话上限秒")
	model := fs.String("model", "", "claude --model 参数(A/B 同模型,缺省继承用户默认)")
	if err := fs.Parse(args); err != nil || *repo == "" || *tasksPath == "" || *outDir == "" {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	data, err := os.ReadFile(*tasksPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	var tf taskFile
	if err := yaml.Unmarshal(data, &tf); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	onlySet := map[string]bool{}
	for id := range strings.SplitSeq(*only, ",") {
		if id = strings.TrimSpace(id); id != "" {
			onlySet[id] = true
		}
	}

	var cfgA string
	if *mode != "b" {
		cfgA, err = mcpConfigA(*repo)
		if err != nil {
			fmt.Fprintln(os.Stderr, "错误(A 组需要 serve 运行中):", err)
			return 1
		}
		defer os.Remove(cfgA)
	}
	cfgB, err := mcpConfigEmpty()
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	defer os.Remove(cfgB)

	for _, t := range tf.Tasks {
		if len(onlySet) > 0 && !onlySet[t.ID] {
			continue
		}
		prompt := t.Question + "\n\n(这是一个定位任务:给出结论——文件路径与关键符号,以及一句话依据;不要修改任何代码;回答完即结束。)"
		for _, m := range []string{"a", "b"} {
			if *mode != "both" && *mode != m {
				continue
			}
			outPath := filepath.Join(*outDir, t.ID+"-"+m+".json")
			if _, err := os.Stat(outPath); err == nil {
				fmt.Printf("跳过 %s(结果已存在)\n", outPath)
				continue
			}
			cfg := cfgA
			if m == "b" {
				cfg = cfgB
			}
			fmt.Printf("▶ %s 组 %s:%s\n", strings.ToUpper(m), t.ID, firstLine(t.Question))
			res, err := driveSession(*repo, cfg, prompt, *model, time.Duration(*timeout)*time.Second)
			if err != nil {
				// 单会话失败不中断全程:落 .err.txt 继续;重跑 run 自动补缺
				//(跳过判据只认 .json)。
				fmt.Fprintln(os.Stderr, "  失败(继续下一个):", err)
				os.WriteFile(filepath.Join(*outDir, t.ID+"-"+m+".err.txt"), []byte(err.Error()), 0o644)
				continue
			}
			res.Task, res.Kind, res.Mode = t.ID, t.Kind, m
			blob, _ := json.MarshalIndent(res, "", "  ")
			if err := os.WriteFile(outPath, blob, 0o644); err != nil {
				fmt.Fprintln(os.Stderr, "错误:", err)
				return 1
			}
			fmt.Printf("  tokens=%d 轮=%d 用时=%ds%s\n", res.Total, res.Turns, res.DurationS,
				map[bool]string{true: "(超时截断)", false: ""}[res.TimedOut])
		}
	}
	return 0
}

// ---- report ----

func runReport(args []string) int {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	outDir := fs.String("out", "", "结果目录")
	if err := fs.Parse(args); err != nil || *outDir == "" {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	entries, err := os.ReadDir(*outDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	byMode := map[string][]result{}
	for _, en := range entries {
		if !strings.HasSuffix(en.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(*outDir, en.Name()))
		if err != nil {
			continue
		}
		var r result
		if json.Unmarshal(data, &r) == nil && r.Mode != "" {
			byMode[r.Mode] = append(byMode[r.Mode], r)
		}
	}
	a, b := byMode["a"], byMode["b"]
	fmt.Printf("结果:A 组 %d 条,B 组 %d 条\n", len(a), len(b))
	fmt.Println("任务\t组\ttokens\t轮\t用时s")
	for _, rs := range [][]result{a, b} {
		sort.Slice(rs, func(i, j int) bool { return rs[i].Task < rs[j].Task })
		for _, r := range rs {
			fmt.Printf("%s\t%s\t%d\t%d\t%d\n", r.Task, strings.ToUpper(r.Mode), r.Total, r.Turns, r.DurationS)
		}
	}
	if len(a) == 0 || len(b) == 0 {
		fmt.Println("(A/B 任一组为空,不判定)")
		return 0
	}
	ma, mb := medianBy(a, func(r result) int { return r.Total }), medianBy(b, func(r result) int { return r.Total })
	ta, tb := medianBy(a, func(r result) int { return r.Turns }), medianBy(b, func(r result) int { return r.Turns })
	da, db := medianBy(a, func(r result) int { return r.DurationS }), medianBy(b, func(r result) int { return r.DurationS })
	fmt.Printf("\n中位 tokens:A=%d B=%d(A/B=%.0f%%,阈值 ≤60%%)\n中位用时:A=%ds B=%ds\n", ma, mb, pct(ma, mb), da, db)
	turnsUsable := ta > 0 || tb > 0
	if turnsUsable {
		fmt.Printf("中位轮数:A=%d B=%d(要求 A ≤ B)\n", ta, tb)
	} else {
		fmt.Println("中位轮数:遥测不可得(本版 Claude Code 无 api_request 事件)——判定仅按 token,用时作参考")
	}
	pass := float64(ma) <= 0.6*float64(mb) && (!turnsUsable || ta <= tb)
	if pass {
		fmt.Println("判定:PASS——M1.4 达标(数据落盘于结果目录,回写文档销案)")
	} else {
		fmt.Println("判定:FAIL——按 impl §11,用数据裁决二期 go/no-go(检查种子质量/任务难度分布后可复测)")
	}
	return 0
}

func medianBy(rs []result, f func(result) int) int {
	vals := make([]int, len(rs))
	for i, r := range rs {
		vals[i] = f(r)
	}
	sort.Ints(vals)
	return vals[len(vals)/2]
}

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) * 100 / float64(b)
}

// ---- 会话驱动 ----

// driveSession 在 repo 目录下 PTY 启动交互式 claude,喂 prompt,
// 靠转录静默判定完成,返回计量。
//
// 实测行为定案(2026-07-04 排障):转录 jsonl 在【回合推进/结束时】才创建
// (活跃会话可 90s+ 无文件);会话是否已提交只能看 PTY 输出流速——欢迎屏是
// 一次性几 KB,干活的 TUI 每秒都在重绘 spinner。故:重投判据 = 最近窗口
// 输出字节数低于阈值且尚无转录;绝不向活跃会话重投(会排队成第二条用户消息,
// 污染计量)。
func driveSession(repo, mcpCfg, prompt, model string, timeout time.Duration) (*result, error) {
	absRepo, err := filepath.Abs(repo)
	if err != nil {
		return nil, err
	}
	tdir, err := transcriptDir(absRepo)
	if err != nil {
		return nil, err
	}
	before, _ := listSet(tdir)

	claudeCmd := fmt.Sprintf(`claude --strict-mcp-config --mcp-config %q --dangerously-skip-permissions`, mcpCfg)
	if model != "" {
		claudeCmd += fmt.Sprintf(" --model %q", model)
	}
	cmd := exec.Command("sh", "-c", claudeCmd)
	cmd.Dir = absRepo
	// 计量走官方 OTEL 遥测(console 导出混进 PTY 流,ANSI 剥离后解析):
	// 2.1.201 的会话转录 jsonl 惰性缓冲,短会话连优雅退出都不落盘,不可依赖。
	cmd.Env = append(os.Environ(), "TERM=xterm-256color",
		"CLAUDE_CODE_ENABLE_TELEMETRY=1",
		"OTEL_METRICS_EXPORTER=console",
		"OTEL_LOGS_EXPORTER=console",
		"OTEL_METRIC_EXPORT_INTERVAL=5000",
		"OTEL_LOGS_EXPORT_INTERVAL=5000")
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}
	defer ptmx.Close()
	defer pty.KillGroup(cmd)
	buf := newRingBuf(6 << 20) // 全量输出环形缓冲:遥测块 + 活动符号都从这里判
	go func() {
		b := make([]byte, 32<<10)
		for {
			n, err := ptmx.Read(b)
			buf.write(b[:n])
			if err != nil {
				return
			}
		}
	}()

	start := time.Now()
	// TUI 启动等待(信任/更新提示等由 --dangerously-skip-permissions 与已信任目录消化;
	// 插件多的环境启动慢,宁多等)。
	time.Sleep(10 * time.Second)
	// 多行提示词必须括号粘贴包裹:裸 \n 会被 TUI 当回车键逐行提交(实测排障定案);
	// 停 2s 再单发回车(粘贴突发去抖,aibridge 同法)。
	submit := func() {
		ptmx.Write([]byte(bracketedPaste(prompt)))
		time.Sleep(2 * time.Second)
		ptmx.Write([]byte("\r"))
	}
	mark := buf.len()
	submit()

	// 阶段一:确认回合启动——提交点之后出现 spinner/响应符号(✻✶✳✽⏺ 只在
	// 回合运行/回答时出现,欢迎屏没有;字节流速会被输入框回显假触发,弃用)。
	deadline := time.Now().Add(timeout)
	resends := 0
	started := false
	nextResend := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) && !started {
		time.Sleep(2 * time.Second)
		if buf.containsAfter(mark, "✻", "✶", "✳", "✽", "⏺") {
			started = true
			break
		}
		if time.Now().After(nextResend) && resends < 2 {
			resends++
			nextResend = time.Now().Add(25 * time.Second)
			ptmx.Write([]byte("\r")) // 收掉可能的弹窗
			time.Sleep(3 * time.Second)
			mark = buf.len()
			submit()
		}
	}
	if !started {
		return nil, fmt.Errorf("超时未见回合启动(重投 %d 次;屏幕尾部:%s)", resends, buf.tailClean(300))
	}

	// 阶段二:等回合结束。完成信号 = 输出流速归零(60s 窗口增量 < 512B;
	// 忙 TUI 每秒重绘 spinner,KB 级/30s;闲 TUI 近零输出,实测 <4KB/30s)。
	timedOut := true
	lastLen := buf.len()
	quietSince := time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		cur := buf.len()
		if cur != lastLen {
			if cur-lastLen > 512 { // 容忍零星心跳重绘
				quietSince = time.Now()
			}
			lastLen = cur
			continue
		}
		if time.Since(quietSince) >= 60*time.Second {
			timedOut = false
			break
		}
	}

	// 阶段三:优雅退出——OTEL console 导出主要在进程退出时 flush(实测:周期导出
	// 在 TUI 下不可靠),硬杀=计量丢失,必须把退出确认走完。
	// 会话若开过后台任务,/exit 会弹"Exit anyway?"确认框(默认项即退出)——补回车确认。
	ptmx.Write([]byte("/exit"))
	time.Sleep(1 * time.Second)
	ptmx.Write([]byte("\r"))
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	for i := 0; i < 3; i++ { // 最多补 3 次确认回车,每次等 8s
		select {
		case <-exited:
			i = 3
		case <-time.After(8 * time.Second):
			ptmx.Write([]byte("\r"))
		}
	}
	select {
	case <-exited:
	case <-time.After(8 * time.Second):
		pty.KillGroup(cmd) // 实在不退才硬杀(计量此时大概率已丢,错误信息带现场)
	}
	time.Sleep(2 * time.Second) // 终态导出落进缓冲

	res := &result{StartedAt: start.UTC().Format(time.RFC3339),
		DurationS: int(time.Since(start).Seconds()), TimedOut: timedOut}
	clean := stripANSI(buf.bytes())
	res.Model, res.Usage, res.Turns = tallyTelemetry(clean)
	res.Total = res.Usage.total()
	res.CostUSD = tallyCost(clean)
	// 转录若真落盘了就记路径(补充审计;计量不依赖它)。
	if now, err := listSet(tdir); err == nil {
		for f := range now {
			if !before[f] && strings.HasSuffix(f, ".jsonl") {
				res.Transcript = filepath.Join(tdir, f)
			}
		}
	}
	if res.Total == 0 {
		// 全量剥净缓冲落盘,便于事后排障(遥测缺失的会话钱已花,现场必须留)。
		dump := filepath.Join(os.TempDir(), fmt.Sprintf("kbeval-debug-%d.txt", start.Unix()))
		os.WriteFile(dump, clean, 0o644)
		return nil, fmt.Errorf("遥测未捕获 token 计数(缓冲 %d 字节;剥净现场 %s;屏尾:%s)",
			buf.len(), dump, buf.tailClean(300))
	}
	return res, nil
}

// ---- 输出缓冲与遥测解析 ----

// ringBuf 有界输出缓冲:超限丢最旧(遥测计数器是累计值且周期重导,尾部永远够用)。
type ringBuf struct {
	mu   sync.Mutex
	data []byte
	cap  int
	off  int64 // 已丢弃的字节数(len() 返回逻辑总长,活动检测用)
}

func newRingBuf(capBytes int) *ringBuf { return &ringBuf{cap: capBytes} }

func (r *ringBuf) write(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.data = append(r.data, p...)
	if over := len(r.data) - r.cap; over > 0 {
		r.data = r.data[over:]
		r.off += int64(over)
	}
}

func (r *ringBuf) len() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.off + int64(len(r.data))
}

func (r *ringBuf) bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]byte, len(r.data))
	copy(out, r.data)
	return out
}

// containsAfter 检查逻辑位置 mark 之后的输出是否含任一标记。
func (r *ringBuf) containsAfter(mark int64, marks ...string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	from := max(mark-r.off, 0)
	if from >= int64(len(r.data)) {
		return false
	}
	seg := string(r.data[from:])
	for _, m := range marks {
		if strings.Contains(seg, m) {
			return true
		}
	}
	return false
}

// tailClean 返回剥 ANSI 后的尾部 n 字符(错误报告用)。
func (r *ringBuf) tailClean(n int) string {
	s := string(stripANSI(r.bytes()))
	if len(s) > n {
		s = s[len(s)-n:]
	}
	return strings.ReplaceAll(s, "\n", " ")
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]|\x1b\][^\x07]*\x07|\x1b[()][A-Z0-9]|\x1b\[[<>][a-zA-Z0-9]*`)

func stripANSI(b []byte) []byte { return ansiRe.ReplaceAll(b, nil) }

var (
	// OTEL console 导出是 util.inspect 风格(非严格 JSON);COUNTER 是累计值,
	// 取【最后一个】token.usage 块内的 (type, value) 对,按 type 跨模型求和。
	tokenTypeRe = regexp.MustCompile(`type:\s*['"](input|output|cacheRead|cacheCreation)['"]`)
	valueRe     = regexp.MustCompile(`value:\s*([0-9]+)`)
	modelRe     = regexp.MustCompile(`model:\s*['"]([a-z0-9.-]+)['"]`)
)

// tallyTelemetry 从剥净的输出里解析 token 计量与轮数。
// 轮数口径:claude_code.api_request 事件数(代理循环迭代);logs 导出缺席时为 0
// (报告侧对 0 诚实标注"不可得",不参与判定)。
func tallyTelemetry(clean []byte) (model string, u usageSum, turns int) {
	s := string(clean)
	// 全部去重模型(主模型 + Claude Code 的 haiku 后台辅助都会出现;成本都算数)。
	seen := map[string]bool{}
	var models []string
	for _, m := range modelRe.FindAllStringSubmatch(s, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			models = append(models, m[1])
		}
	}
	sort.Strings(models)
	model = strings.Join(models, ",")
	idx := strings.LastIndex(s, `"claude_code.token.usage"`)
	if idx < 0 {
		return model, u, strings.Count(s, "claude_code.api_request")
	}
	block := s[idx:]
	// 块尾:下一个 descriptor(其他指标)或文本尽头。
	if end := strings.Index(block[1:], "descriptor"); end > 0 {
		block = block[:end+1]
	}
	types := tokenTypeRe.FindAllStringSubmatchIndex(block, -1)
	for i, t := range types {
		typ := block[t[2]:t[3]]
		// 该 type 之后、下一 type 之前的第一个 value。
		segEnd := len(block)
		if i+1 < len(types) {
			segEnd = types[i+1][0]
		}
		if v := valueRe.FindStringSubmatch(block[t[1]:segEnd]); v != nil {
			n := atoiSafe(v[1])
			switch typ {
			case "input":
				u.Input += n
			case "output":
				u.Output += n
			case "cacheCreation":
				u.CacheWrite += n
			case "cacheRead":
				u.CacheRead += n
			}
		}
	}
	return model, u, strings.Count(s, "claude_code.api_request")
}

// tallyCost 解析 claude_code.cost.usage(USD,累计计数器,末次导出跨模型求和;
// codegraph benchmark 有美元维度,对比更立体——顺手捡的采集项)。
func tallyCost(clean []byte) float64 {
	s := string(clean)
	idx := strings.LastIndex(s, `"claude_code.cost.usage"`)
	if idx < 0 {
		return 0
	}
	block := s[idx:]
	if end := strings.Index(block[1:], "descriptor"); end > 0 {
		block = block[:end+1]
	}
	total := 0.0
	for _, m := range costValueRe.FindAllStringSubmatch(block, -1) {
		var v float64
		fmt.Sscanf(m[1], "%f", &v)
		total += v
	}
	return total
}

var costValueRe = regexp.MustCompile(`value:\s*([0-9]+(?:\.[0-9]+)?)`)

func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

// transcriptDir 定位 Claude Code 的会话转录目录(路径 slug:'/' 与 '.' → '-')。
func transcriptDir(absRepo string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	slug := strings.NewReplacer("/", "-", ".", "-").Replace(absRepo)
	dir := filepath.Join(home, ".claude", "projects", slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func listSet(dir string) (map[string]bool, error) {
	out := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out, err
	}
	for _, e := range entries {
		out[e.Name()] = true
	}
	return out, nil
}

// ---- MCP 配置 ----

// mcpConfigA 生成 A 组(接知识库)的临时 MCP 配置;校验 serve 可达。
func mcpConfigA(repo string) (string, error) {
	absRepo, err := filepath.Abs(repo)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(absRepo, ".knowledge", "config.yaml"))
	if err != nil {
		return "", fmt.Errorf("读 config.yaml(仓库未 init?):%w", err)
	}
	var cfg struct {
		Port int `yaml:"port"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil || cfg.Port == 0 {
		return "", fmt.Errorf("config.yaml 缺 port")
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/mcp/main?repo=%s", cfg.Port, absRepo)
	entry := map[string]any{"type": "http", "url": url}
	if tok, err := os.ReadFile(filepath.Join(absRepo, ".knowledge", "local", "token")); err == nil && len(tok) > 0 {
		entry["headers"] = map[string]string{"Authorization": "Bearer " + strings.TrimSpace(string(tok))}
	}
	blob, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"knowledge": entry}})
	f, err := os.CreateTemp("", "kbeval-mcp-a-*.json")
	if err != nil {
		return "", err
	}
	f.Write(blob)
	f.Close()
	return f.Name(), nil
}

func mcpConfigEmpty() (string, error) {
	f, err := os.CreateTemp("", "kbeval-mcp-b-*.json")
	if err != nil {
		return "", err
	}
	f.WriteString(`{"mcpServers":{}}`)
	f.Close()
	return f.Name(), nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// bracketedPaste 把文本包成终端括号粘贴序列——TUI 视作单次粘贴,内嵌换行保持字面。
func bracketedPaste(s string) string { return "\x1b[200~" + s + "\x1b[201~" }
