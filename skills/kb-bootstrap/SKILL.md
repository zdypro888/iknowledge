---
name: kb-bootstrap
description: 一句话初始化/接入/卸载 iknowledge 代码知识库(MCP)。接入触发:用户说"初始化(当前)项目知识库"、"给这个项目装知识库"、"接入 iknowledge / knowledge MCP"、"启动知识库服务"、"知识库还没装"。卸载触发:"卸载当前项目知识库"、"移除/停用知识库"。覆盖:安装二进制、init 骨架、写入 Claude Code(.mcp.json/CLAUDE.md/hooks)与 Codex(config.toml/AGENTS.md)接入配置(stdio 桥自动拉起服务)、验证连通;以及对称的项目级卸载。
---

# iknowledge 知识库一句话接入与卸载(Claude Code 与 Codex 双宿主)

目标仓库 = 当前工作目录的仓库根(用户另有指定则从其说法取)。

**分工铁律**:iknowledge 二进制对源码只读、只写 `.knowledge/`;接入配置文件由**你(agent)**代写——写任何一处之前先读现有文件做**合并**,绝不整文件覆盖用户已有配置。

## 步骤(幂等,重复执行安全)

1. **确保二进制**:`iknowledge version`;命令不存在则试 `$(go env GOPATH)/bin/iknowledge`(后续命令同理带全路径);再不行 `go install github.com/zdypro888/iknowledge/cmd/iknowledge@latest`;没有 Go 环境就停下告诉用户先装 Go。
2. **建骨架**:`iknowledge init --repo <root>`(纯 AST,零 LLM;大仓十几秒,幂等)。
3. **拿接入片段**:`iknowledge setup --repo <root>`,读输出(含端口与各片段)。
4. **写 Claude Code 三件套(读旧→合并→写回)**:
   - `<root>/.mcp.json`:把 `knowledge` 条目合并进 `mcpServers`——**stdio 形态**
     `{"command":"iknowledge","args":["stdio","--repo","<root>"]}`(文件不存在则新建;
     已有 `knowledge` 键则整体替换为 stdio 形态,旧 http url 形态一并升级)。
     **command 可解析性**:MCP 客户端直接 spawn 该命令(GUI 客户端 PATH 常没有
     ~/go/bin)——若 `command -v iknowledge` 失败且 `/usr/local/bin/iknowledge`
     不存在,先试软链 `ln -sf <绝对路径> /usr/local/bin/iknowledge`,仍不行则
     `command` 字段直接写第 1 步解析出的**绝对路径**;
   - `<root>/CLAUDE.md`:追加 setup 输出的纪律段(文件里已含"本仓库配有 knowledge MCP"则跳过);
   - `<root>/.claude/settings.json`:把 hooks 片段合并进 `hooks.PostToolUse`(已有相同 `iknowledge hook` command 的条目则跳过)。
5. **写 Codex 接入(本机存在 Codex 时;判定:`${CODEX_HOME:-$HOME/.codex}` 目录存在)**:
   - `${CODEX_HOME:-$HOME/.codex}/config.toml`:追加 setup 输出 ④ 的 `[mcp_servers.knowledge]` 段(stdio 形态:`command = "iknowledge"`、`args = ["stdio","--repo","<root>"]`;已有同名段则整体替换升级;该文件已有其他 `[mcp_servers.*]` 条目时,几个仓库共存要把段名改成 `knowledge-<项目名>` 防撞名);
   - `<root>/AGENTS.md`:追加与 CLAUDE.md 相同的纪律段(已含则跳过)。Codex 无 hook 注入机制,不写 hooks。
6. **验证(顺便完成首次拉起)**:stdio 桥会按需自动拉起后台 serve,无需手动起服务。验证一条命令:
   `printf '%s\n' '{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"clientInfo":{"name":"probe"}}}' | iknowledge stdio --repo <root>`
   ——输出含 `"repoRoot"` 即通(此时后台 serve 已被带起,hook 注入立即可用;
   再对任意真实 `.go` 文件 `curl "http://127.0.0.1:<端口>/inject?file=<相对路径>"` 可复核 hook 腿)。
7. **收尾告知用户**(务必说清):
   - MCP 工具与 hook 在**下一个会话**生效(Claude Code / Codex 都在启动时加载配置),请重启会话;
   - Codex 对 MCP 工具调用会弹一次审批,交互界面点允许即可;
   - `.knowledge/` 建议随 git 提交(知识随代码走、团队共享);
   - 后台 serve 由 stdio 桥按需自动拉起——机器重启后什么都不用做,下一个 AI 会话自动带起一切;
     (手动模式仍在:`iknowledge serve --repo A [--repo B …]` 单进程可服务多仓库。)

## 卸载流程(用户说"卸载当前项目知识库"时;与接入严格对称)

**先确认再动手**:`.knowledge/` 是知识库本体,删了就没了(除非已随 git 提交)。除非用户已在指令里明确表示确认,否则先问一句"确认删除本项目知识库与全部接入配置?"再执行。

1. **停服务**:从 `<root>/.knowledge/config.yaml` 读端口,`lsof -ti :<端口> | xargs kill` (没在跑就跳过)。
2. **删知识库**:`rm -rf <root>/.knowledge`。
3. **清配置痕迹(只删我们写入的段,别的内容一律保留;文件因此变空可整删)**:
   - `<root>/.mcp.json`:删 `mcpServers.knowledge` 键(删后 `mcpServers` 为空且文件无其他内容则删文件);
   - `<root>/CLAUDE.md` 与 `<root>/AGENTS.md`:删"本仓库配有 knowledge MCP"起到最后一条编号规则止的整段;
   - `<root>/.claude/settings.json`:删 `hooks.PostToolUse` 里 command 含 `iknowledge hook` 的条目(数组因此为空则连同空壳一起收干净);
   - `${CODEX_HOME:-$HOME/.codex}/config.toml`:删 `[mcp_servers.knowledge]`(或本仓库对应的 `knowledge-<项目名>`)整段。
4. **汇报**:列出实际删除/修改的每个文件;提醒机器级卸载(二进制与 skill 本体)另有一条命令:
   `curl -fsSL https://raw.githubusercontent.com/zdypro888/iknowledge/main/uninstall.sh | sh`。

## 注意

- `init` 满屏 `undigested` 是设计使然(骨架先行,知识靠干活自然沉淀),不要试图"全库消化"。
- 多仓库各有独立端口(`18000 + hash(路径) % 2000`),互不冲突。
- 任何步骤失败都如实报告,不要静默跳过;写配置文件前必须先读原文件;卸载只清单里列的内容,绝不多删。
