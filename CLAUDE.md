# iknowledge 开发约定

AI 代码知识库(MCP 服务)。**两份设计文档是唯一真相源**,实现与其冲突时要么修文档(留痕)要么修实现,不许静默偏离:

- [knowledge.md](knowledge.md) — 概念设计全案(五维模型、自愈机制、两条铁律、五篇推演)
- [knowledge-impl.md](knowledge-impl.md) — 第一期工程方案(数据模型、存储、MCP API 全量规范、里程碑)——**写代码照这份**,已含推演五的全部定案

## 铁律(违反即返工)

- **零重依赖**:第一期仅允许 `gopkg.in/yaml.v3`;禁止引入 MCP SDK/JSON-RPC 框架/tokenizer——JSON-RPC 2.0 手写(风格参照 aibridge 的 `internal/bridge/mcp.go`)
- **工具对源码只读**:唯一写入 `.knowledge/`(设计铁律二);任何往 `.knowledge/` 之外写文件的代码都是 bug
- 包依赖方向:`mcpserv → engine → {store, index, parser, model}`;`model` 不依赖任何内部包
- 表驱动测试;所有 YAML 写入走 temp + fsync + `os.Rename` + 目录 fsync 原子写(轮 25);分片读写必须保留未知字段(yaml.Node 往返)
- 双哈希语义不许混用:`Hash` 只管腐烂检测,`StructHash` 只管迁移匹配(impl §5)
- **测试 MCP:协议级手段优先**(curl / httptest,便宜无额度消耗);真实客户端联测**禁用 `claude -p`**(独立限流池,撞账号限额),用 **PTY 驱动交互式 claude**(aibridge `internal/agent` 模式,用户已授权)

## 命令

```bash
go build ./...
go vet ./...
go test ./...
```

一期 lint 只用 `go vet`,**不引入 golangci-lint**(与零重依赖哲学一致、减小 CI 面;复评点 M1.4)。CI(.github/workflows/ci.yml)与本节保持一致。

## 当前进度

设计定稿(2026-07-04 推演五修订 + 轮 23/24,见 knowledge.md 附录 A)。**全量实现已交付并经对抗审查修复**(轮 24):13 个 MCP 工具 + /mcp/main 与 /mcp/scout 双端点 + GET /inject + 会话台账/过时警报/任务态/侦查委派/维护欠账/时代摘要/使用日志(~1 万行 Go,含测试;engine 70%/index 81%/mcpserv 71% 覆盖)。57-agent 对抗审查确认的 42 处真问题全部修复并加回归测试(路径穿越、conflict 分片数据丢失、迁移自毁、record_change 原子性、init 加锁、lint 误杀、错误码语义等)。测试全绿,curl 协议级自测全过。轮 25(2026-07-04 第三方全仓审计,go-audit):P0=0、P1=3 全修,共修 19 处(P1:remember 拒收路径污染 keywords 缓存、record_change 同文件双新符号重复节点、MCP 规范 Origin 校验缺失)+ 审后追加 8 项(atomicWrite/journal fsync 耐久、serve SIGTERM 优雅停机、version 子命令、断开 index→store 依赖边使代码符合 impl §2 声明、framed() 伪造框架标记消毒、ledgerTTL 更名去歧义、非回环监听裸奔警示、字符串拼接清理);台账字段(what/why/rejected/wip)定案不过指令 lint、靠数据框隔离(impl §7.3/kb_task 已留痕)。轮 25 补交付**接入套件**:`iknowledge setup`(打印 .mcp.json/纪律段/hook 三件套,只打印不代写)+ `iknowledge hook`(PostToolUse hook 桥,失败一律静默退出 0;挂接点 PreToolUse→PostToolUse 已在 impl §7.1 勘误留痕;§9"唯一注入腿"更新为三条腿)+ README 重写(傻瓜部署/简易使用/速查/FAQ)。casino(48.6 万行/1522 文件)实测:init 877 文件→15493 节点 13.3s、幂等 0.98s、kb_status 812ms、kb_map ~105ms;**hook 注入 PTY 闭环通过**(Claude Code 读文件后,分片里的预埋知识原文出现在其回答中)。claude CLI 真实客户端已实测通过(PTY 交互式:kb_status/kb_recall ok、Mcp-Session-Id 回带、recall 命中预埋知识)。遗留:①Codex 客户端同法实测(用户接入时);②M1.4 的 10 任务 A/B 验收协议(需真实 agent 会话);③PTY 自派备模式与 git 历史挖掘维持延后(轮 22/设计留白)。原 M1.1–M1.4 里程碑保留作验收清单用。
