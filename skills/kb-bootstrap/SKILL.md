---
name: kb-bootstrap
description: 一句话初始化/接入 iknowledge 代码知识库(MCP)。当用户说"初始化(当前)项目知识库"、"给这个项目装知识库"、"接入 iknowledge / knowledge MCP"、"启动知识库服务"、"知识库还没装"时使用。覆盖:安装二进制、init 骨架、写入 .mcp.json/CLAUDE.md/hooks 三件套、启动 serve、验证连通。
---

# iknowledge 知识库一句话接入

目标仓库 = 当前工作目录的仓库根(用户另有指定则从其说法取)。

**分工铁律**:iknowledge 二进制对源码只读、只写 `.knowledge/`;三件套配置文件由**你(agent)**代写——写任何一处之前先读现有文件做**合并**,绝不整文件覆盖用户已有配置。

## 步骤(幂等,重复执行安全)

1. **确保二进制**:`iknowledge version`;命令不存在则试 `$(go env GOPATH)/bin/iknowledge`(后续命令同理带全路径);再不行 `go install github.com/zdypro888/iknowledge/cmd/iknowledge@latest`;没有 Go 环境就停下告诉用户先装 Go。
2. **建骨架**:`iknowledge init --repo <root>`(纯 AST,零 LLM;大仓十几秒,幂等)。
3. **拿三件套**:`iknowledge setup --repo <root>`,读输出(含端口与各片段)。
4. **写三处(读旧→合并→写回)**:
   - `<root>/.mcp.json`:把 `knowledge` 条目合并进 `mcpServers`(文件不存在则新建;已有 `knowledge` 键则仅更新 url);
   - `<root>/CLAUDE.md`:追加 setup 输出的纪律段(文件里已含"本仓库配有 knowledge MCP"则跳过);
   - `<root>/.claude/settings.json`:把 hooks 片段合并进 `hooks.PostToolUse`(已有相同 `iknowledge hook` command 的条目则跳过)。
5. **起服务**:先探活 `curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:<端口>/inject?file=probe"`——返回任何 HTTP 状态码(400/404 也算)说明已在跑,跳过;连接失败则后台启动 `iknowledge serve --repo <root>`(用 run_in_background,不要挂在前台)。
6. **验证**:对仓库里任意一个真实 `.go` 文件 `curl "http://127.0.0.1:<端口>/inject?file=<相对路径>"`,返回 200(有骨架节点)即通。
7. **收尾告知用户**(务必说清):
   - MCP 工具与 hook 注入在**下一个 Claude Code 会话**生效(启动时加载),请重启会话;
   - `.knowledge/` 建议随 git 提交(知识随代码走、团队共享);
   - 服务是常驻进程,机器重启后说一句"启动知识库服务"即可(本 skill 第 5 步单独可用)。

## 注意

- `init` 满屏 `undigested` 是设计使然(骨架先行,知识靠干活自然沉淀),不要试图"全库消化"。
- 多仓库各有独立端口(`18000 + hash(路径) % 2000`),互不冲突。
- 任何步骤失败都如实报告,不要静默跳过;第 4 步写文件前必须先读原文件。
