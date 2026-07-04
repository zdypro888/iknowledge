---
name: kb-bootstrap
description: 一句话初始化/接入 iknowledge 代码知识库(MCP)。当用户说"初始化(当前)项目知识库"、"给这个项目装知识库"、"接入 iknowledge / knowledge MCP"、"启动知识库服务"、"知识库还没装"时使用。覆盖:安装二进制、init 骨架、写入 Claude Code(.mcp.json/CLAUDE.md/hooks)与 Codex(config.toml/AGENTS.md)接入配置、启动 serve、验证连通。
---

# iknowledge 知识库一句话接入(Claude Code 与 Codex 双宿主)

目标仓库 = 当前工作目录的仓库根(用户另有指定则从其说法取)。

**分工铁律**:iknowledge 二进制对源码只读、只写 `.knowledge/`;接入配置文件由**你(agent)**代写——写任何一处之前先读现有文件做**合并**,绝不整文件覆盖用户已有配置。

## 步骤(幂等,重复执行安全)

1. **确保二进制**:`iknowledge version`;命令不存在则试 `$(go env GOPATH)/bin/iknowledge`(后续命令同理带全路径);再不行 `go install github.com/zdypro888/iknowledge/cmd/iknowledge@latest`;没有 Go 环境就停下告诉用户先装 Go。
2. **建骨架**:`iknowledge init --repo <root>`(纯 AST,零 LLM;大仓十几秒,幂等)。
3. **拿接入片段**:`iknowledge setup --repo <root>`,读输出(含端口与各片段)。
4. **写 Claude Code 三件套(读旧→合并→写回)**:
   - `<root>/.mcp.json`:把 `knowledge` 条目合并进 `mcpServers`(文件不存在则新建;已有 `knowledge` 键则仅更新 url);
   - `<root>/CLAUDE.md`:追加 setup 输出的纪律段(文件里已含"本仓库配有 knowledge MCP"则跳过);
   - `<root>/.claude/settings.json`:把 hooks 片段合并进 `hooks.PostToolUse`(已有相同 `iknowledge hook` command 的条目则跳过)。
5. **写 Codex 接入(本机存在 Codex 时;判定:`${CODEX_HOME:-$HOME/.codex}` 目录存在)**:
   - `${CODEX_HOME:-$HOME/.codex}/config.toml`:追加 setup 输出 ④ 的 `[mcp_servers.knowledge]` 段(已有同名段则仅核对 url;该文件已有其他 `[mcp_servers.*]` 条目时,几个仓库共存要把段名改成 `knowledge-<项目名>` 防撞名);
   - `<root>/AGENTS.md`:追加与 CLAUDE.md 相同的纪律段(已含则跳过)。Codex 无 hook 注入机制,不写 hooks。
6. **起服务**:先探活 `curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:<端口>/inject?file=probe"`——返回任何 HTTP 状态码(400/404 也算)说明已在跑,跳过;连接失败则以**脱离本会话也能存活**的方式后台启动:`nohup iknowledge serve --repo <root> >/dev/null 2>&1 &`(不要挂在前台,也不要用会随 agent 会话终止的方式)。
7. **验证**:对仓库里任意一个真实 `.go` 文件 `curl "http://127.0.0.1:<端口>/inject?file=<相对路径>"`,返回 200(有骨架节点)即通。
8. **收尾告知用户**(务必说清):
   - MCP 工具与 hook 在**下一个会话**生效(Claude Code / Codex 都在启动时加载配置),请重启会话;
   - Codex 对 MCP 工具调用会弹一次审批,交互界面点允许即可;
   - `.knowledge/` 建议随 git 提交(知识随代码走、团队共享);
   - 服务是常驻进程,机器重启后说一句"启动知识库服务"即可(本 skill 第 6 步单独可用)。

## 注意

- `init` 满屏 `undigested` 是设计使然(骨架先行,知识靠干活自然沉淀),不要试图"全库消化"。
- 多仓库各有独立端口(`18000 + hash(路径) % 2000`),互不冲突。
- 任何步骤失败都如实报告,不要静默跳过;写配置文件前必须先读原文件。
