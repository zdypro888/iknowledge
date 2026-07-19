# iknowledge

**中文** | [English](README.md)

**A code knowledge base for AI agents — the project's decision & experience archive.**

给 AI 配一个"项目笔记本":AI 写代码是金鱼记忆——项目一大就记不住什么在哪、函数怎么用,每次从头读代码,读懂了又忘,还老忘"上次为什么这么改",把改好的又改回去。iknowledge 把 AI 付出理解成本得到的结论沉淀下来,锚定在代码结构上,随代码演化而失效与更新。

## 它记什么(越往下越值钱)

1. **地图**——什么在哪(项目→文件夹→文件→函数的金字塔骨架,机器自动生成);
2. **经验**——代码上看不出来的门道("这函数别直接调"、"密码传明文进去,里面会自己加密");
3. **账本**——每次改动的"为什么"和"当时否决了什么方案"(防 AI 反复横跳的关键,git 不记这个,全世界没人帮你记)。

两条铁律:**知识导航、原文定论**(知识库永不替代读代码);**工具永不改码**。仓库内容只写 `.knowledge/`;仓库外仅允许按 canonical repo 分仓的用户私有运行态(鉴权/本机身份/scout 信任/semantic provider 设置/崩溃 WAL)、用户显式指定的 export 制品与 install/uninstall 部署。改代码永远是主力 AI。

## 最省事:一条命令装机,之后对 AI 说一句

```bash
curl -fsSL https://raw.githubusercontent.com/zdypro888/iknowledge/main/install.sh | sh
```

装机脚本优先安装**带校验和的预编译二进制**(免 Go 工具链;缺 checksums、缺校验工具或校验不符一律拒收),没有可用的已验证资产才回退 `go install`;同时把 [`kb-bootstrap`](skills/kb-bootstrap/SKILL.md) 技能装进 Claude Code(`~/.claude/skills/`),检测到 Codex 时同步装进 `~/.codex/skills/`。发布流水线覆盖 macOS、Linux、Windows 的 amd64/arm64。自定义二进制目录用 `IKNOWLEDGE_BIN`,明确要求源码安装用 `IKNOWLEDGE_FORCE_SOURCE=1`。

之后在任何项目里,对 **Claude Code 或 Codex** 说:**"初始化当前项目知识库"**——AI 自己建骨架、代写全部接入配置(Claude Code 三件套 + Codex 的 config.toml/AGENTS.md)并验证连通(两侧均已实测)。重启会话后 kb_* 工具与 hook 注入就位;服务由 stdio 桥按需自动拉起,机器重启后也不用管。

> AI 代写配置不违反铁律:iknowledge 二进制永不改源码或接入配置;少量仓外私有运行态用于避免密钥/WAL 被误提交。不想用 skill 就走下面的手动路线。

## 30 秒装好(傻瓜部署)

```bash
# 1. 安装(需 Go;或 git clone 后 go build ./cmd/iknowledge)
go install github.com/zdypro888/iknowledge/cmd/iknowledge@latest
iknowledge version    # 验证

# 2. 初始化你的仓库(纯 AST 骨架秒建,零 LLM 成本;48 万行仓库实测约 13 秒)
iknowledge init --repo /path/to/your/repo

# 3. 打印接入指南,按需粘贴各段(iknowledge 只打印、不代写你的文件)
iknowledge setup --repo /path/to/your/repo

# (无需手动启动服务:.mcp.json 用 stdio 形态时,AI 会话会自动带起后台 serve)
```

`setup` 会打印五段标注明确的接入配置:

| 贴到哪 | 是什么 | 作用 |
|---|---|---|
| `.mcp.json` | MCP stdio 桥(`command: iknowledge stdio`) | agent 看见 16 个 kb_* 工具;桥按需自动拉起后台 serve,零服务管理(必装) |
| `CLAUDE.md` | 纪律提示词 | AI 干活的规矩:读前查库、改后记账、悟到就沉淀(必装) |
| `.claude/settings.json` | hook 片段 | AI 每 Read/Edit 一个文件,该文件的知识+过时警报自动进上下文(推荐) |
| `~/.codex/config.toml` + 仓库 `AGENTS.md` | Codex MCP + 纪律 | 使用 Codex 宿主时接入(可选) |
| `.git/hooks/pre-commit` | `iknowledge precheck --repo .` | 提交前呈现历史否决、腐烂知识、矛盾与漏记账;缺省只告警,自行加 `--strict` 才阻断(可选) |

多仓库共存没问题:每个仓库端口独立(`18000 + hash(路径) % 2000`);一个进程可以同时服务多个仓库(`iknowledge serve --repo A --repo B`,每仓仍用自己的端口,客户端配置不用改)。

<details>
<summary><b>手动常驻/开机自启(可选)</b>——stdio 桥已自动管理服务,仅远程或显式共享场景需要</summary>

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

**装完基本不用管。** AI 在仓库里干活时:hook 把所触文件的知识自动喂给它(改完文件还会当场提醒记账);纪律提示词让它定位先 `kb_recall`、改完 `kb_record_change` 记账、读懂难点顺手 `kb_remember` 沉淀——**知识库靠真实工作自己长大**(首触即建,消化成本搭任务便车)。

你只需要偶尔:

```bash
iknowledge status --repo .     # 看覆盖率/新鲜度/维护欠账 + 热点待消化清单(git 改动频率 × 被调中心度)
iknowledge doctor --repo . --deploy   # 初始化/配置/parser/部署自检;会提示误留的 serve 进程
iknowledge maintain --repo . --plan   # 只读打印维护路线(清账让 AI 走 kb_maintain)
iknowledge brief --repo . --budget 1200   # 新会话一屏简报:WIP/风险/近期决策/维护债
iknowledge precheck --repo .          # 用已知风险与变更账本检查暂存源码
iknowledge semantic status --repo .   # 可选语义/向量预览状态(缺省禁用)
iknowledge import --repo . -i backup.kbundle --dry-run --backup   # bundle 迁移前先预演并备份
# 已有非 journal 文件内容不同时,核对报告后才显式加 --force;所有 hard cap 仍不可绕过
git add .knowledge && git commit   # 知识随代码提交,团队共享、跟分支走
iknowledge init --repo . --reanchor-all   # 全局性改动(如全仓 gofmt)后批量重锚
```

刚 init 完满屏 `undigested`(未消化)是**设计使然**:骨架先行,知识空洞诚实标注,AI 碰到会明说"仅有骨架,请读原文"并自动附上该文件的近期提交线索(来时路),绝不编造。想加速热区,`kb_status` 会按"git 近期改动 × 跨文件被调中心度"给出热点待消化清单,让 AI 照单做一轮种子消化(读热点文件 + `kb_remember` 沉淀)即可。

服务的启动你不用管:stdio 桥按需自动拉起后台 serve。内部客户端无论是否启用业务 Bearer,都先用双向 HMAC 验证当前 loopback listener,只发送按 scope 绑定的短期 session,长期本机身份绝不发给未知端口。若此前启用了 Bearer,仓外私有 token 同时保持该模式,桥会继续拉起 `serve --auth`。即便一切不可用,AI 也照常读码,hook 静默无操作。

## 16 个工具速查

| 类 | 工具 | 一句话 |
|---|---|---|
| 查 | `kb_map` | 金字塔导航:什么在哪、覆盖率 |
| 查 | `kb_recall` | 按关键词/节点查知识、历史、调用关系与接口↔实现(方法集匹配);命中后沿调用图/流程/实现关系自动带出结构相邻节点;骨架/可疑节点自动附来时路 |
| 查 | `kb_diagnose` | 症状/报错 → 最可能位置、pitfall、排障流程与历史否决方案 |
| 记 | `kb_remember` | 沉淀经验(usage/pitfall/contract/summary…);支持矛盾声明(disputes)待裁决 |
| 记 | `kb_record_change` | 变更记账:改了什么/为什么/否决了什么(一个逻辑修改一条) |
| 记 | `kb_verify` | confirm 升级置信(须附验证依据并留痕)/ refute 勘误(级联降级派生知识)/ obsolete 退休 |
| 记 | `kb_revert` | 事务化、追加式撤销一条全错的 record_change / verify;结构化前后状态保证崩溃恢复与重试安全 |
| 记 | `kb_adopt` | 孤儿知识认领(符号迁移)或送葬(归档) |
| 态 | `kb_task` | 会话隔离的任务态 start/update/complete,收尾自动提醒偿还与沉淀 |
| 态 | `kb_flow` | 跨文件流程/主题节点(登录流程、支付链路…) |
| 态 | `kb_session` | 当前会话摘要与收尾质量门,提示缺沉淀/缺记账风险 |
| 派 | `kb_investigate` | 派一次性侦察兵满库定位,只带结论回来,主上下文不脏 |
| 派 | `kb_submit_findings` | 侦察兵回报出口 |
| 维 | `kb_status` | 库健康:覆盖率/suspect/孤儿/欠账/热点待消化 |
| 维 | `kb_maintain` | 领取维护欠账(落后摘要、疑似重复、待重验、矛盾待裁决…);`patrol` 取跨节点矛盾巡检简报 |
| 维 | `kb_init` | 库内自助初始化/对账(等价 CLI init) |

## 卸载(与安装同样省事)

```bash
# 项目级:在项目里对 AI 说一句(停服务、删 .knowledge/、清全部接入配置痕迹;会先跟你确认)
「卸载当前项目知识库」

# 机器级:移除二进制(含 IKNOWLEDGE_BIN 自定义目录)与两处 skill,并停掉所有 serve
curl -fsSL https://raw.githubusercontent.com/zdypro888/iknowledge/main/uninstall.sh | sh
```

先逐项目说"卸载",最后跑机器级脚本(顺序反了也行,脚本结尾会打印手动清理清单)。机器卸载会清本机凭据/信任/semantic provider 设置,但检测到 prepared/committed WAL 时必须保留它待恢复。`.knowledge/` 若已随 git 提交,删除前想清楚——那是团队共享资产。

## 常见问题

- **它会不会变成 AI 的杂物记忆库?** 不会——**知识库对应代码,不是记忆库**。判据一问:"代码变了它会失效吗(或它解释这个仓库的代码为什么长这样)?"三不进:通用编程知识(任何仓库都成立的话)、会话/用户偏好(归 AI 宿主自己的 memory)、任务待办(归 kb_task,git 排除用完即弃)。纪律、工具描述、写入警示三层把关:任务态词(TODO/待办)触发警示,无锚节点(project/目录)每次写入都亮边界提醒。
- **只支持 Go 吗?** Go 提供符号级解析 + 全仓调用图/接口匹配。**Python** 用 `-I -S` 隔离的本机 AST 与严格 PEP 263 解码提供符号/语义哈希(无调用图)。**JavaScript/TypeScript**(.ts/.tsx/.js/.jsx/.mjs/.cjs/.mts/.cts)、**Rust**、**Java** 是内置轻量符号词法。其他语言可经 `extensions` 以文件粒度入库,仍有账本/经验/注入/腐烂检测,但无符号下钻与调用关系。
- **要不要先"全库分析"?** 不用。init 只建结构骨架(AST,免费);语义知识按需生长——批量消化又贵又浅还立刻开始腐烂,详见设计文档"冷启动:允许空洞的塔"。
- **知识错了怎么办?** AI 读原文发现冲突时,按纪律以原文为准并 `kb_verify refute` 勘误;基于错误知识推导出的条目会被级联降为 suspect。两条知识互相矛盾且当场断不了对错时,可登记 disputes 待裁决,双方并存呈现、都标"裁决前别信"。升级 verified 与勘误义务对称:confirm 也必须附验证依据并留确认记录,没验证过的结论洗不成可信知识。分居不同节点的矛盾,用 `kb_maintain patrol` 按关键词簇聚成一张简报跨节点并读裁决。
- **代码改了知识会不会过时?** 会,而且系统知道:锚定哈希检测腐烂;自身名免疫的结构哈希寻找改名/挪动候选;doc 敏感迁移护栏阻止“改名时顺手改了契约”被静默判新鲜。无法证明安全的迁移会保留知识与血缘,但降为 `suspect` 等重验;同会话内重读变更节点会收到过时警报,suspect 进入维护欠账队列。
- **语义/向量检索会上传仓库,或需要另一个 MCP 服务吗?** 可选 **preview 已实现且缺省禁用**。命令套件为 `iknowledge semantic <configure|enable|disable|status|rebuild|clear>`:每仓显式 `configure` 后再运行 `rebuild` 才会批量生成索引;configure/enable/disable/status/clear 均不联系 provider,只有 rebuild 会批量发送脱敏摘要。语义索引过期或 provider 不可用时,自动回退到既有词法/结构检索。本机 Ollama 和远程 OpenAI-compatible endpoint 都是 iknowledge 内部直连的 HTTP provider,不是第三方 MCP。索引只包含脱敏后的有效知识摘要/era 摘要,绝不包含源码切块。endpoint/model/dimensions/revision/enabled 及有界排序/资源参数只存在按 canonical repo 分仓的用户私有状态中;受跟踪配置、bundle 和 MCP 参数都不能开启或重定向对外流量。远程 API key 只从固定环境变量 `IKNOWLEDGE_EMBEDDING_API_KEY` 读取。Eino 只是实现参考,不是依赖;iknowledge 使用自有的标准库 HTTP Embedder provider 与本地 Flat 快照。不需要向量功能时保持禁用即可,详见 [`vecdb.md`](vecdb.md)。

  最短本机用法如下(需你自行安装并运行 Ollama/模型;iknowledge 绝不自动安装、启动或部署它们)。`qwen3-embedding:0.6b` 实际输出 1024 维,`--dimensions 0` 会安全自动探测:

  ```bash
  ollama pull qwen3-embedding:0.6b
  iknowledge semantic configure --repo . --endpoint http://127.0.0.1:11434/v1 --model qwen3-embedding:0.6b --dimensions 0
  iknowledge semantic rebuild --repo .
  ```

  若 nbco 已经通过本机 Ollama 部署 `bge-m3`,iknowledge 可填相同 endpoint/model 复用该模型服务;两边的文档、fingerprint 与向量索引完全独立,iknowledge 不会读写 nbco 的 Qdrant 或索引。
- **安全模型?** 缺省监听 `127.0.0.1`,Origin 校验挡浏览器 DNS rebinding。stdio/hook/scout 即使业务 Bearer 关闭也会做仅回环可用的双向 HMAC listener 身份校验;共享机器再用 `serve --auth`,让业务端点额外要求根 Bearer 或 scope 短 session。长期密钥/scout 信任按 canonical repo 分仓写用户私有配置态(Unix 文件 0600),旧仓内 token 只触发安全轮换、绝不复用。`.knowledge` 写入与源码读取都拒绝根以下 symlink,git tracked symlink 不能引流到仓外。显式非回环明文 HTTP 仍不提供传输保密。
- **没有子代理能力的宿主怎么用侦查?** `kb_investigate` 缺省是委派模式。宿主没有子代理时,设 `scout: self`,核对命令后运行 `iknowledge trust-scout --repo .`。授权在仓外用户私有态,绑定精确模式/命令且配置一变即失效;仓库内 executable 一律拒绝。仓内临时 MCP 配置只含短期 HMAC session,不含根密钥。之后服务端用 PTY 拉起受信侦察兵并等协议交卷。仅 macOS/Linux。
- **自定义子代理(审计 agent 等)没有 kb_* 工具怎么查库?** 用只读腿:`curl "http://127.0.0.1:<端口>/recall?q=<词>"`(`/map`、`/status` 同理)——有 shell 就能查,零 MCP 配置,输出与工具一致;侦查简报也会自动附上这条降级路径。只读:记账与沉淀仍由主 AI 收尾。
- **Codex 能用吗?** 能,已实测(codex-cli 0.142,含桌面 App):`iknowledge setup` 的第 ④ 段贴进 `~/.codex/config.toml`(stdio 形态 `command = "iknowledge"`;http 直连备选也在输出里),纪律段贴进仓库 `AGENTS.md`。差异两点:Codex 对 MCP 工具调用会弹一次审批(交互界面点允许;headless `exec` 需 `--dangerously-bypass-approvals-and-sandbox`);无 hook 注入机制,靠纪律主动查询。

## 状态

第一期已全量交付并持续加固:现为 16 个 MCP 工具 + `/mcp/main`、`/mcp/scout` 双端点 + `GET /inject` 与只读腿(`/recall` `/map` `/status`)+ `iknowledge hook/setup/maintain/doctor/brief/precheck/semantic` 套件。2026-07-11 对抗审计集中加固了可崩溃恢复的多文件事务、严格/便携 bundle、解析边界与语义哈希、代际索引、并发快照、源码/存储 symlink 边界、listener 身份、自派侦查信任和跨平台校验安装;2026-07-18 又加入语义写入与 bundle 导入默认秘密脱敏、预算化新会话简报、暂存区风险/记账预检。2026-07-04 补齐原二/三/四期计划:全仓调用图与结构扩展检索、热点待消化清单、矛盾裁决登记、非代码知识复核提醒、`--auth` 鉴权、单进程多仓库、Windows 支持、PTY 自派侦查备模式。**客户端双实测通过**(Claude Code + Codex,含 instructions 语义)。**M1.4 A/B 验收达标**:10 个固定定位任务,接知识库(种子覆盖 19%)vs 裸 grep 同模型双跑——中位 token 省 41%(59% ≤ 60% 阈值)、8/10 任务更省、用时更短;协议、工装(`cmd/kbeval`)与两轮全量数据在 [eval/m14/](eval/m14/)。

可选语义/向量检索 preview 已交付,但仍需每仓显式开启且缺省禁用。质量晋级 benchmark 尚未完成,因此词法/结构检索仍是基线与回退路径。

- [`knowledge.md`](knowledge.md) — 概念设计全案(20 轮设计讨论的收敛:五个维度、自愈机制、经济学、安全、四篇推演)
- [`knowledge-impl.md`](knowledge-impl.md) — 第一期工程方案(包结构、数据模型、存储、MCP API 全量规范、里程碑)
- [`vecdb.md`](vecdb.md) — 已实现的可选语义/向量检索 preview 及其晋级门槛

## 许可证

[MIT](LICENSE)——商用与非商用、修改、再分发均自由。唯一依赖 `gopkg.in/yaml.v3`(同为 MIT/Apache-2.0)。
