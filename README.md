# iknowledge

**A code knowledge base for AI agents — the project's decision & experience archive.**

给 AI 配一个"项目笔记本":AI 写代码是金鱼记忆——项目一大就记不住什么在哪、函数怎么用,每次从头读代码,读懂了又忘,还老忘"上次为什么这么改",把改好的又改回去。iknowledge 把 AI 付出理解成本得到的结论沉淀下来,锚定在代码结构上,随代码演化而失效与更新。

## 它记什么(越往下越值钱)

1. **地图**——什么在哪(项目→文件夹→文件→函数的金字塔摘要,机器自动生成);
2. **经验**——代码上看不出来的门道("这函数别直接调"、"密码传明文进去,里面会自己加密");
3. **账本**——每次改动的"为什么"和"当时否决了什么方案"(防 AI 反复横跳的关键,git 不记这个,全世界没人帮你记)。

## 怎么用

以 MCP 服务的形式提供,任何支持 MCP 的 agent(Claude Code、Codex 等)都能接入:

```bash
iknowledge init  --repo /path/to/repo    # 骨架秒建(纯 AST,零 LLM 成本)
iknowledge serve --repo /path/to/repo    # 启动 MCP 服务
```

AI 通过三类动作与它交互:**查**(kb_map / kb_recall 定位与回忆)、**记**(kb_remember 沉淀经验、kb_record_change 记账)、**派侦察兵**(kb_investigate:一次性分身去满仓库翻代码定位问题,只把结论带回来,主力 AI 上下文始终干净)。

两条铁律:**知识导航、原文定论**(知识库永不替代读代码);**工具永不改码**(对源码只读,改代码永远是主力 AI)。

## 状态

第一期已全量交付:13 个 MCP 工具 + `/mcp/main`、`/mcp/scout` 双端点 + `GET /inject`,经多轮对抗审查与第三方全仓审计修复(最近一轮 2026-07-04),Claude Code 真实客户端连接已实测通过。遗留:Codex 客户端实测、M1.4 的 10 任务 A/B 验收协议。

- [`knowledge.md`](knowledge.md) — 概念设计全案(20 轮设计讨论的收敛:五个维度、自愈机制、经济学、安全、四篇推演)
- [`knowledge-impl.md`](knowledge-impl.md) — 第一期工程方案(包结构、数据模型、存储、MCP API 全量规范、里程碑)
