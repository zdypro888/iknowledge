# iknowledge

**A code knowledge base for AI agents — the project's decision & experience archive.**

给 AI 配一个"项目笔记本":AI 写代码是金鱼记忆——项目一大就记不住什么在哪、函数怎么用,每次从头读代码,读懂了又忘,还老忘"上次为什么这么改",把改好的又改回去。iknowledge 把 AI 付出理解成本得到的结论沉淀下来,锚定在代码结构上,随代码演化而失效与更新。

## 它记什么(越往下越值钱)

1. **地图**——什么在哪(项目→文件夹→文件→函数的金字塔骨架,机器自动生成);
2. **经验**——代码上看不出来的门道("这函数别直接调"、"密码传明文进去,里面会自己加密");
3. **账本**——每次改动的"为什么"和"当时否决了什么方案"(防 AI 反复横跳的关键,git 不记这个,全世界没人帮你记)。

两条铁律:**知识导航、原文定论**(知识库永不替代读代码);**工具永不改码**(对源码只读,唯一写入 `.knowledge/`,改代码永远是主力 AI)。

## 30 秒装好(傻瓜部署)

```bash
# 1. 安装(需 Go;或 git clone 后 go build ./cmd/iknowledge)
go install github.com/zdypro888/iknowledge/cmd/iknowledge@latest
iknowledge version    # 验证

# 2. 初始化你的仓库(纯 AST 骨架秒建,零 LLM 成本;48 万行仓库实测约 13 秒)
iknowledge init --repo /path/to/your/repo

# 3. 打印接入三件套,按提示各贴一处(iknowledge 只打印、不代写你的文件)
iknowledge setup --repo /path/to/your/repo

# 4. 启动服务(常驻;重启机器后再跑一次即可)
iknowledge serve --repo /path/to/your/repo
```

`setup` 打印的三件套,各贴进目标仓库的三个文件:

| 贴到哪 | 是什么 | 作用 |
|---|---|---|
| `.mcp.json` | MCP 服务地址 | Claude Code / Codex 等 agent 看见 13 个 kb_* 工具(必装) |
| `CLAUDE.md` | 纪律提示词 | AI 干活的规矩:读前查库、改后记账、悟到就沉淀(必装) |
| `.claude/settings.json` | hook 片段 | AI 每 Read/Edit 一个文件,该文件的知识+过时警报自动进上下文(推荐) |

多仓库共存没问题:每个仓库端口独立(`18000 + hash(路径) % 2000`),各自 `serve` 互不干扰。

## 日常怎么用(简易使用)

**装完基本不用管。** AI 在仓库里干活时:hook 把所触文件的知识自动喂给它;纪律提示词让它定位先 `kb_recall`、改完 `kb_record_change` 记账、读懂难点顺手 `kb_remember` 沉淀——**知识库靠真实工作自己长大**(首触即建,消化成本搭任务便车)。

你只需要偶尔:

```bash
iknowledge status --repo .     # 看覆盖率/新鲜度/维护欠账
git add .knowledge && git commit   # 知识随代码提交,团队共享、跟分支走
iknowledge init --repo . --reanchor-all   # 全局性改动(如全仓 gofmt)后批量重锚
```

刚 init 完满屏 `undigested`(未消化)是**设计使然**:骨架先行,知识空洞诚实标注,AI 碰到会明说"仅有骨架,请读原文",绝不编造。想加速热区,可以让 AI 做一轮种子消化(读热点文件 + `kb_remember` 沉淀)。

服务没启动也不会坏事:MCP 工具不可用时 AI 照常读码干活,hook 静默无操作,任务尾提醒你 `iknowledge serve`。

## 13 个工具速查

| 类 | 工具 | 一句话 |
|---|---|---|
| 查 | `kb_map` | 金字塔导航:什么在哪、覆盖率 |
| 查 | `kb_recall` | 按关键词/节点查知识、历史、来时路 |
| 记 | `kb_remember` | 沉淀经验(usage/pitfall/contract/summary…) |
| 记 | `kb_record_change` | 变更记账:改了什么/为什么/否决了什么(一个逻辑修改一条) |
| 记 | `kb_verify` | confirm 升级置信 / refute 勘误(级联降级派生知识)/ obsolete 退休 |
| 记 | `kb_adopt` | 孤儿知识认领(符号迁移)或送葬(归档) |
| 态 | `kb_task` | 任务态 start/update/complete,收尾自动提醒偿还与沉淀 |
| 态 | `kb_flow` | 跨文件流程/主题节点(登录流程、支付链路…) |
| 派 | `kb_investigate` | 派一次性侦察兵满库定位,只带结论回来,主上下文不脏 |
| 派 | `kb_submit_findings` | 侦察兵回报出口 |
| 维 | `kb_status` | 库健康:覆盖率/suspect/孤儿/欠账 |
| 维 | `kb_maintain` | 领取维护欠账(落后摘要、疑似重复…) |
| 维 | `kb_init` | 库内自助初始化/对账(等价 CLI init) |

## 常见问题

- **要不要先"全库分析"?** 不用。init 只建结构骨架(AST,免费);语义知识按需生长——批量消化又贵又浅还立刻开始腐烂,详见设计文档"冷启动:允许空洞的塔"。
- **知识错了怎么办?** AI 读原文发现冲突时,按纪律以原文为准并 `kb_verify refute` 勘误;基于错误知识推导出的条目会被级联降为 suspect。
- **代码改了知识会不会过时?** 会,而且系统知道:双哈希锚定检测腐烂,改名/挪动自动迁移,失配标 `suspect` 等重验;同会话内重读到已变更节点会收到过时警报。
- **安全模型?** 默认仅监听 `127.0.0.1`,无鉴权(本地信任模型);带 Origin 校验挡浏览器 DNS rebinding;监听非回环地址会打裸奔警告——别在不可信网络那么干。工具对源码只读。
- **Codex 能用吗?** 走同一个 HTTP MCP 端点;hook 自动注入目前是 Claude Code 形态(PostToolUse),Codex 侧靠纪律提示词与 MCP instructions。

## 状态

第一期已全量交付:13 个 MCP 工具 + `/mcp/main`、`/mcp/scout` 双端点 + `GET /inject` + `iknowledge hook/setup` 接入套件,经多轮对抗审查与第三方全仓审计修复(最近一轮 2026-07-04),Claude Code 真实客户端连接已实测通过。遗留:Codex 客户端实测、M1.4 的 10 任务 A/B 验收协议。

- [`knowledge.md`](knowledge.md) — 概念设计全案(20 轮设计讨论的收敛:五个维度、自愈机制、经济学、安全、四篇推演)
- [`knowledge-impl.md`](knowledge-impl.md) — 第一期工程方案(包结构、数据模型、存储、MCP API 全量规范、里程碑)
