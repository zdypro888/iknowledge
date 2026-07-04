// 接入套件(impl §7.1/§9,轮 25):setup 打印三件套、hook 做宿主 hook 桥。
// 两者都不往 .knowledge/ 之外写任何文件(铁律二)——setup 只打印,hook 只读+HTTP。
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/zdypro888/iknowledge/internal/engine"
	"github.com/zdypro888/iknowledge/internal/store"
)

// mcpJSONSnippet 是 .mcp.json 接入片段(init/setup 共用)。
func mcpJSONSnippet(root string, port int) string {
	return fmt.Sprintf(`{ "mcpServers": { "knowledge": { "type": "http",
  "url": "http://127.0.0.1:%d/mcp/main?repo=%s" } } }`, port, url.QueryEscape(root))
}

// hooksJSONSnippet 是 Claude Code hooks 接入片段(.claude/settings.json)。
// PostToolUse 而非设计初稿的 PreToolUse:现版 Claude Code 只有 PostToolUse 的
// hookSpecificOutput.additionalContext 能把文本注入上下文(impl §7.1 轮 25 勘误)。
func hooksJSONSnippet(root string) string {
	return fmt.Sprintf(`{ "hooks": { "PostToolUse": [ {
  "matcher": "Read|Edit|Write|MultiEdit",
  "hooks": [ { "type": "command", "command": "iknowledge hook --repo %s" } ] } ] } }`, root)
}

// codexTOMLSnippet 是 Codex 接入片段(~/.codex/config.toml;CLI 与桌面 App 共用)。
// 实测(impl §7.1 轮 25):codex-cli 0.142 的 rmcp 客户端走 streamable HTTP 直连
// /mcp/main,服务端不开 SSE 也兼容,Mcp-Session-Id 正常回带(台账/过时警报有效)。
func codexTOMLSnippet(root string, port int) string {
	return fmt.Sprintf(`[mcp_servers.knowledge]
url = "http://127.0.0.1:%d/mcp/main?repo=%s"`, port, url.QueryEscape(root))
}

// runSetup 打印接入三件套:.mcp.json、CLAUDE.md 纪律段、hooks 片段。
func runSetup(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
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
	cfg, err := s.LoadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	if cfg == nil {
		fmt.Fprintln(os.Stderr, "错误: 缺 .knowledge/config.yaml,先跑 iknowledge init --repo "+s.RepoRoot())
		return 1
	}
	root := s.RepoRoot()
	fmt.Fprintf(out, `接入三件套(iknowledge 只打印不代写,铁律二;贴完运行 iknowledge serve --repo %s):

① MCP 服务(必装)——贴进 %s/.mcp.json:
%s

② 纪律提示词(必装)——贴进 %s/CLAUDE.md(或 codex 等 agent 的指令文件):
%s

③ hook 自动注入(推荐)——贴进 %s/.claude/settings.json(已有 hooks 则合并):
%s
   效果:AI 每次 Read/Edit/Write 一个文件,该文件的知识+过时警报自动进上下文;
   serve 未启动时 hook 静默无操作,不影响任何工具调用。

④ Codex 接入(可选)——把下面段落合并进 ~/.codex/config.toml(CLI 与桌面 App 共用):
%s
   纪律提示词贴进 %s/AGENTS.md(内容同②)。Codex 无 hook 注入机制,靠纪律主动查询。
   注意:Codex 对 MCP 工具调用会弹审批,交互界面点允许即可;headless exec 需
   --dangerously-bypass-approvals-and-sandbox。多仓库共存时把条目名 knowledge
   改成不重复的名字(如 knowledge-<项目名>)。

验证:iknowledge serve --repo %s 启动后,
  curl "http://127.0.0.1:%d/inject?file=<某个 .go 文件路径>"
`, root, root, mcpJSONSnippet(root, cfg.Port), root, engine.DisciplinePrompt,
		root, hooksJSONSnippet(root), codexTOMLSnippet(root, cfg.Port), root, root, cfg.Port)
	return 0
}

// hookInput 是宿主 hook 喂给 stdin 的事件 JSON(只解码所需字段)。
type hookInput struct {
	SessionID string `json:"session_id"`
	CWD       string `json:"cwd"`
	ToolInput struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
	} `json:"tool_input"`
}

// runHook 是 Claude Code PostToolUse hook 桥:stdin 读事件 → GET /inject →
// additionalContext JSON。注入是增强不是依赖(纪律第 0 条),因此任何失败
// (无事件/无仓库/serve 未启动/文件无节点)都静默退出 0,绝不阻塞宿主工具调用。
func runHook(args []string, in io.Reader, out io.Writer) int {
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // hook 的 stderr 会被宿主当告警展示,解析错也保持静默
	repo := fs.String("repo", "", "仓库路径(缺省:从事件 cwd/文件路径向上找 .knowledge)")
	if err := fs.Parse(args); err != nil {
		return 0
	}
	var hi hookInput
	if err := json.NewDecoder(io.LimitReader(in, 1<<20)).Decode(&hi); err != nil {
		return 0
	}
	file := hi.ToolInput.FilePath
	if file == "" {
		file = hi.ToolInput.NotebookPath
	}
	if file == "" {
		return 0
	}
	root := *repo
	if root == "" {
		root = findKnowledgeRoot(hi.CWD)
	}
	if root == "" && filepath.IsAbs(file) {
		root = findKnowledgeRoot(filepath.Dir(file))
	}
	if root == "" {
		return 0
	}
	s, err := store.Open(root)
	if err != nil {
		return 0
	}
	cfg, err := s.LoadConfig()
	if err != nil || cfg == nil {
		return 0
	}
	// 本机回环,1s 足够;超时静默——宁可少注入一次,不能卡住宿主。
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/inject?file=%s&session=%s",
		cfg.Port, url.QueryEscape(file), url.QueryEscape(hi.SessionID)))
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil || len(bytes.TrimSpace(body)) == 0 {
		return 0
	}
	_ = json.NewEncoder(out).Encode(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "PostToolUse",
			"additionalContext": string(body),
		},
	})
	return 0
}

// findKnowledgeRoot 从 start 向上找最近的含 .knowledge/ 目录的仓库根;没有返回空。
func findKnowledgeRoot(start string) string {
	dir := start
	if dir == "" {
		dir, _ = os.Getwd()
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	dir = abs
	for {
		if fi, err := os.Stat(filepath.Join(dir, ".knowledge")); err == nil && fi.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
