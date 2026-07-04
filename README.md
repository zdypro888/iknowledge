# iknowledge

**A code knowledge base for AI agents — the project's decision & experience archive.**

给 AI 配一个"项目笔记本":AI 写代码是金鱼记忆——项目一大就记不住什么在哪、函数怎么用,每次从头读代码,读懂了又忘,还老忘"上次为什么这么改",把改好的又改回去。iknowledge 把 AI 付出理解成本得到的结论沉淀下来,锚定在代码结构上,随代码演化而失效与更新。

## 它记什么(越往下越值钱)

1. **地图**——什么在哪(项目→文件夹→文件→函数的金字塔骨架,机器自动生成);
2. **经验**——代码上看不出来的门道("这函数别直接调"、"密码传明文进去,里面会自己加密");
3. **账本**——每次改动的"为什么"和"当时否决了什么方案"(防 AI 反复横跳的关键,git 不记这个,全世界没人帮你记)。

两条铁律:**知识导航、原文定论**(知识库永不替代读代码);**工具永不改码**(对源码只读,唯一写入 `.knowledge/`,改代码永远是主力 AI)。

## 最省事:一条命令装机,之后对 AI 说一句

```bash
curl -fsSL https://raw.githubusercontent.com/zdypro888/iknowledge/main/install.sh | sh
```

装机脚本做三件事:`go install` 二进制、把 [`kb-bootstrap`](skills/kb-bootstrap/SKILL.md) 技能装进 Claude Code(`~/.claude/skills/`)、检测到 Codex 时同步装进 `~/.codex/skills/`。

之后在任何项目里,对 **Claude Code 或 Codex** 说:**"初始化当前项目知识库"**——AI 自己建骨架、代写全部接入配置(Claude Code 三件套 + Codex 的 config.toml/AGENTS.md)、拉起服务、验证连通(两侧均已实测)。重启会话后 kb_* 工具与 hook 注入就位;机器重启后说一句"启动知识库服务"即可。

> AI 代写配置不违反铁律:铁律约束的是 iknowledge 二进制(只写 `.knowledge/`),配置粘贴在设计上本就是"由用户/主 AI 完成"。不想用 skill 就走下面的手动路线。

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

多仓库共存没问题:每个仓库端口独立(`18000 + hash(路径) % 2000`);一个进程可以同时服务多个仓库(`iknowledge serve --repo A --repo B`,每仓仍用自己的端口,客户端配置不用改)。

<details>
<summary><b>开机自启(可选)</b>——不想每次重启后手动 serve 的话</summary>

macOS(launchd):存为 `~/Library/LaunchAgents/com.iknowledge.serve.plist` 后 `launchctl load` 它:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.iknowledge.serve</string>
  <key>ProgramArguments</key><array>
    <string>/Users/你/go/bin/iknowledge</string>
    <string>serve</string>
    <string>--repo</string><string>/path/to/repoA</string>
    <string>--repo</string><string>/path/to/repoB</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
</dict></plist>
```

Linux(systemd 用户单元):存为 `~/.config/systemd/user/iknowledge.service` 后 `systemctl --user enable --now iknowledge`:

```ini
[Unit]
Description=iknowledge knowledge MCP

[Service]
ExecStart=%h/go/bin/iknowledge serve --repo /path/to/repoA --repo /path/to/repoB
Restart=on-failure

[Install]
WantedBy=default.target
```

</details>

## 日常怎么用(简易使用)

**装完基本不用管。** AI 在仓库里干活时:hook 把所触文件的知识自动喂给它;纪律提示词让它定位先 `kb_recall`、改完 `kb_record_change` 记账、读懂难点顺手 `kb_remember` 沉淀——**知识库靠真实工作自己长大**(首触即建,消化成本搭任务便车)。

你只需要偶尔:

```bash
iknowledge status --repo .     # 看覆盖率/新鲜度/维护欠账 + 热点待消化清单(git 改动频率 × 被调中心度)
iknowledge maintain --repo .   # 只读打印维护欠账(清账让 AI 走 kb_maintain)
git add .knowledge && git commit   # 知识随代码提交,团队共享、跟分支走
iknowledge init --repo . --reanchor-all   # 全局性改动(如全仓 gofmt)后批量重锚
```

刚 init 完满屏 `undigested`(未消化)是**设计使然**:骨架先行,知识空洞诚实标注,AI 碰到会明说"仅有骨架,请读原文",绝不编造。想加速热区,`kb_status` 会按"git 近期改动 × 跨文件被调中心度"给出热点待消化清单,让 AI 照单做一轮种子消化(读热点文件 + `kb_remember` 沉淀)即可。

服务没启动也不会坏事:MCP 工具不可用时 AI 照常读码干活,hook 静默无操作,任务尾提醒你 `iknowledge serve`。

## 13 个工具速查

| 类 | 工具 | 一句话 |
|---|---|---|
| 查 | `kb_map` | 金字塔导航:什么在哪、覆盖率 |
| 查 | `kb_recall` | 按关键词/节点查知识、历史、调用关系;命中后沿调用图/流程自动带出结构相邻节点 |
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

## 卸载(与安装同样省事)

```bash
# 项目级:在项目里对 AI 说一句(停服务、删 .knowledge/、清全部接入配置痕迹;会先跟你确认)
「卸载当前项目知识库」

# 机器级:移除二进制与两处 skill,并停掉所有运行中的 serve
curl -fsSL https://raw.githubusercontent.com/zdypro888/iknowledge/main/uninstall.sh | sh
```

先逐项目说"卸载",最后跑机器级脚本(顺序反了也行,脚本结尾会打印手动清理清单)。`.knowledge/` 若已随 git 提交,删除前想清楚——那是团队共享的知识资产。

## 常见问题

- **要不要先"全库分析"?** 不用。init 只建结构骨架(AST,免费);语义知识按需生长——批量消化又贵又浅还立刻开始腐烂,详见设计文档"冷启动:允许空洞的塔"。
- **知识错了怎么办?** AI 读原文发现冲突时,按纪律以原文为准并 `kb_verify refute` 勘误;基于错误知识推导出的条目会被级联降为 suspect。
- **代码改了知识会不会过时?** 会,而且系统知道:双哈希锚定检测腐烂,改名/挪动自动迁移,失配标 `suspect` 等重验;同会话内重读到已变更节点会收到过时警报。
- **安全模型?** 默认仅监听 `127.0.0.1`,无鉴权(本地信任模型);带 Origin 校验挡浏览器 DNS rebinding;监听非回环地址会打警告。共享多用户机器用 `serve --auth`:token 生成在 `.knowledge/local/token`(0600),全端点要求 `Authorization: Bearer`,`setup` 会打印带 headers 的接入片段(含密钥,勿提交 git)。工具对源码只读。
- **没有子代理能力的宿主怎么用侦查?** `kb_investigate` 缺省是委派模式(简报交给宿主子代理跑)。宿主没有子代理时,在 `.knowledge/config.yaml` 加 `scout: self`——服务端自己用 PTY 拉起一个侦察兵进程(缺省 `claude`,`scout_command` 可换)执行简报、阻塞等交卷,主 AI 一次调用直接拿到结论。仅 macOS/Linux。
- **自定义子代理(审计 agent 等)没有 kb_* 工具怎么查库?** 用只读腿:`curl "http://127.0.0.1:<端口>/recall?q=<词>"`(`/map`、`/status` 同理)——有 shell 就能查,零 MCP 配置,输出与工具一致。只读:记账与沉淀仍由主 AI 收尾。
- **Codex 能用吗?** 能,已实测(codex-cli 0.142,含桌面 App):`iknowledge setup` 的第 ④ 段贴进 `~/.codex/config.toml`(`[mcp_servers.knowledge] url = …`,rmcp 走 HTTP 直连),纪律段贴进仓库 `AGENTS.md`。差异两点:Codex 对 MCP 工具调用会弹一次审批(交互界面点允许;headless `exec` 需 `--dangerously-bypass-approvals-and-sandbox`);无 hook 注入机制,靠纪律主动查询。

## 状态

第一期已全量交付并持续加固:13 个 MCP 工具 + `/mcp/main`、`/mcp/scout` 双端点 + `GET /inject` + `iknowledge hook/setup/maintain` 套件,经多轮对抗审查与第三方全仓审计修复。2026-07-04 补齐原二/三/四期计划:全仓调用图与结构扩展检索、热点待消化清单、矛盾裁决登记、非代码知识复核提醒、`--auth` 鉴权、单进程多仓库、Windows 支持(CI 三平台真机全绿)、PTY 自派侦查备模式。**客户端双实测通过**(Claude Code + Codex,含 instructions 语义)。**M1.4 A/B 验收达标**:10 个固定定位任务,接知识库(种子覆盖 19%)vs 裸 grep 同模型双跑——中位 token 省 41%(59% ≤ 60% 阈值)、8/10 任务更省、用时更短;协议、工装(`cmd/kbeval`)与两轮全量数据在 [eval/m14/](eval/m14/)。

- [`knowledge.md`](knowledge.md) — 概念设计全案(20 轮设计讨论的收敛:五个维度、自愈机制、经济学、安全、四篇推演)
- [`knowledge-impl.md`](knowledge-impl.md) — 第一期工程方案(包结构、数据模型、存储、MCP API 全量规范、里程碑)
