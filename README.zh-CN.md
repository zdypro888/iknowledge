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

装机脚本优先安装预编译资产(免 Go 工具链)：二进制和 `kb-bootstrap-SKILL.md` 都来自同一个已完整发布的不可变 tag，两者都必须通过该 Release 的 `sha256sums.txt`；资产未上传齐时 Release 保持 draft，缺记录、缺校验工具或不匹配一律拒收。没有可用的已验证资产才回退 `go install`，并尽力把 `@latest` 冻结为具体 module version（也可用 `IKNOWLEDGE_SOURCE_REF=vX.Y.Z`）。提交前会检查 PATH shadow、可控别名、版本自报和两处 skill 暂存；若 `~/.local/bin` 不在 PATH 且无法安全创建 `/usr/local/bin` 入口，会要求你先加 PATH 后重跑。所有 pre-commit 失败保留旧二进制和旧 skill；极少数替换后别名/停服失败会明确报错，保留同版 skill，修复精确路径后重跑即收敛。skill 装进 Claude Code(`~/.claude/skills/`)，检测到 Codex 时同步装进 `~/.codex/skills/`；发布覆盖 macOS/Linux/Windows 的 amd64/arm64。自定义目录用 `IKNOWLEDGE_BIN`，强制源码安装用 `IKNOWLEDGE_FORCE_SOURCE=1`。Unix 升级只会向当前 UID、且以受控精确路径启动的旧 `serve` 优雅发送 `TERM`，替换后复扫 KeepAlive 重启，绝不按进程名广杀；Windows 若 `.exe` 被占用，只关闭该绝对路径后在 Git Bash/MSYS2 重跑。

之后在任何项目里,对 **Claude Code 或 Codex** 说:**"初始化当前项目知识库"**——AI 自己建骨架、代写全部接入配置(Claude Code 三件套 + Codex 项目级 `.codex/config.toml`/`AGENTS.md`)并验证连通(两侧均已实测)。重启会话后 kb_* 工具与 hook 注入就位;服务由 stdio 桥按需自动拉起,机器重启后也不用管。

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
| `.mcp.json` | MCP stdio 桥(`command: iknowledge stdio`) | agent 看见 17 个 kb_* 工具;桥按需自动拉起后台 serve,零服务管理(必装) |
| `CLAUDE.md` | 纪律提示词 | AI 干活的规矩:读前查库、改后记账、悟到就沉淀(必装) |
| `.claude/settings.json` | hook 片段 | AI 每 Read/Edit 一个文件,该文件的知识+过时警报自动进上下文(推荐) |
| `<repo>/.codex/config.toml` + 仓库 `AGENTS.md` | Codex MCP + 纪律 | 项目级 Codex 接入(可选);只有明确要让所有 Codex 项目都启动本仓 MCP 时才写 `~/.codex/config.toml` |
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
    <string>/Users/你/.local/bin/iknowledge</string>
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
ExecStart=%h/.local/bin/iknowledge serve --repo /path/to/repoA --repo /path/to/repoB
Restart=on-failure

[Install]
WantedBy=default.target
```

</details>

以上路径对应安装器默认值；若设置了 `IKNOWLEDGE_BIN`，launchd/systemd 也必须使用同一个绝对路径，升级器才能精确识别受管二进制。

## 日常怎么用(简易使用)

**装完基本不用管。** AI 在仓库里干活时:hook 把所触文件的知识自动喂给它(改完文件还会当场提醒记账);纪律提示词让它定位先 `kb_recall`、改完 `kb_record_change` 记账、读懂难点顺手 `kb_remember` 沉淀——**知识库靠真实工作自己长大**(首触即建,消化成本搭任务便车)。

你只需要偶尔:

```bash
iknowledge status --repo .     # 看覆盖率/新鲜度/维护欠账 + 热点待消化清单(git 改动频率 × 被调中心度)
iknowledge doctor --repo . --deploy   # 另查 Git 正本、WIP 年龄、进程数/RSS/年龄与 daemon 构建
iknowledge maintain --repo . --plan   # 只读打印维护路线(清账让 AI 走 kb_maintain)
iknowledge brief --repo . --budget 1200   # 新会话一屏简报:WIP/风险/近期决策/维护债
iknowledge precheck --repo .          # 用已知风险与变更账本检查暂存源码
iknowledge semantic status --repo .   # 可选语义/向量预览状态(缺省禁用)
iknowledge import --repo . -i backup.kbundle --dry-run --backup   # bundle 迁移前先预演并备份
# 已有非 journal 文件内容不同时,核对报告后才显式加 --force;所有 hard cap 仍不可绕过
git add .knowledge && git commit   # 知识随代码提交,团队共享、跟分支走
iknowledge init --repo . --reanchor-all   # 全局性改动(如全仓 gofmt)后批量重锚
```

“知识随代码提交”是可检查的不变量,不是口号。持久正本(`project.yaml`、`config.yaml`、`tree/`、`journal/`、`flows/`、`topics/`)必须真的进入 Git 索引；只有 `local/`、`wip/` 与临时文件应被忽略。`doctor` 会报告 `tracked / partial / untracked / ignored / no-git`,也能发现外层 `.gitignore` 整体吞掉 `.knowledge/`。非 Git 聚合根仍可本机使用,但若不为知识根建立独立 Git 仓库,就没有随分支/团队共享的耐久保证。

刚 init 完满屏 `undigested`(未消化)是**设计使然**:骨架先行,知识空洞诚实标注,AI 碰到会明说"仅有骨架,请读原文"并自动附上该文件的近期提交线索(来时路),绝不编造。想加速热区,`kb_status` 会按"git 近期改动 × 跨文件被调中心度"给出热点待消化清单,让 AI 照单做一轮种子消化(读热点文件 + `kb_remember` 沉淀)即可。

服务的启动你不用管:stdio 桥按需自动拉起后台 serve。内部客户端无论是否启用业务 Bearer,都先用双向 HMAC 验证当前 loopback listener,只发送按 scope 绑定的短期 session,长期本机身份绝不发给未知端口。daemon 启动时固定记录 executable SHA-256、revision 与启动时间；新构建的 bridge 会先认证旧 daemon、请求优雅排空、等 listener 释放后自动拉起当前构建。首次遇到没有 runtime endpoint 的历史版本会 fail closed,只需人工停旧 serve 一次。`doctor --deploy` 会显示 bridge/daemon 数量、RSS、年龄与构建身份。若此前启用了 Bearer,仓外私有 token 同时保持该模式,桥会继续拉起 `serve --auth`。即便一切不可用,AI 也照常读码,hook 静默无操作。

## 17 个工具速查

| 类 | 工具 | 一句话 |
|---|---|---|
| 查 | `kb_map` | 金字塔导航:什么在哪、覆盖率 |
| 查 | `kb_recall` | 按关键词/节点查知识、历史、调用关系与接口↔实现(方法集匹配);命中后沿调用图/流程/实现关系自动带出结构相邻节点;骨架/可疑节点自动附来时路 |
| 查 | `kb_diagnose` | 症状/报错 → 最可能位置、pitfall、排障流程与历史否决方案 |
| 记 | `kb_remember` | 沉淀经验(usage/pitfall/contract/summary…);支持矛盾声明(disputes)待裁决 |
| 记 | `kb_record_change` | 变更记账:改了什么/为什么/否决了什么；保守规范化节点语法、原子落锚源码已确认的新文件/符号，歧义时返回有界规范候选而不静默猜节点 |
| 记 | `kb_verify` | confirm 升级置信(须附验证依据并留痕)/ refute 勘误(级联降级派生知识)/ obsolete 退休 |
| 记 | `kb_revert` | 事务化、追加式撤销一条全错的 record_change / verify;结构化前后状态保证崩溃恢复与重试安全 |
| 记 | `kb_adopt` | 孤儿知识认领(符号迁移)或送葬(归档) |
| 态 | `kb_task` | 会话隔离的任务态 start/update/complete/abandon；核实 stale≥7 天的断开会话可按精确 owner+reason 安全代收口，收尾自动提醒偿还与沉淀 |
| 态 | `kb_flow` | 跨文件流程/主题节点(登录流程、支付链路…) |
| 态 | `kb_session` | 当前会话摘要与收尾质量门,提示缺沉淀/缺记账风险 |
| 派 | `kb_investigate` | 派一次性侦察兵满库定位,只带结论回来,主上下文不脏 |
| 派 | `kb_submit_findings` | 侦察兵回报出口 |
| 维 | `kb_status` | 库健康；semantic 正常态静默，仅在确需同步时追加 `semantic_action` |
| 维 | `kb_semantic` | 用户询问/诊断时查看离线详细状态，或按持久授权执行一次安全增量 sync；同会话重复调用成功跳过且零 provider，不再制造可重试错误 |
| 维 | `kb_maintain` | 领取维护欠账(落后摘要、疑似重复、待重验、矛盾待裁决…);`patrol` 取跨节点矛盾巡检简报 |
| 维 | `kb_init` | 库内自助初始化/对账(等价 CLI init) |

## 卸载(与安装同样省事)

```bash
# 项目级:在项目里对 AI 说一句(停服务、删 .knowledge/、清全部接入配置痕迹;会先跟你确认)
「卸载当前项目知识库」

# 机器级:移除二进制(含 IKNOWLEDGE_BIN 自定义目录)与两处 skill,
# 只安全停止由该安装绝对路径启动的 serve
curl -fsSL https://raw.githubusercontent.com/zdypro888/iknowledge/main/uninstall.sh | sh
```

先逐项目说"卸载",最后跑机器级脚本(顺序反了也行,脚本结尾会打印手动清理清单)。机器卸载会清本机凭据/信任/semantic provider 设置,但检测到 prepared/committed WAL 时必须保留它待恢复。`.knowledge/` 若已随 git 提交,删除前想清楚——那是团队共享资产。

## 常见问题

- **它会不会变成 AI 的杂物记忆库?** 不会——**知识库对应代码,不是记忆库**。判据一问:"代码变了它会失效吗(或它解释这个仓库的代码为什么长这样)?"三不进:通用编程知识(任何仓库都成立的话)、会话/用户偏好(归 AI 宿主自己的 memory)、任务待办(归 kb_task,git 排除用完即弃)。纪律、工具描述、写入警示三层把关:任务态词(TODO/待办)触发警示,无锚节点(project/目录)每次写入都亮边界提醒。
- **只支持 Go 吗?** Go 提供符号级解析 + 全仓调用图/接口匹配。**Python** 用 `-I -S` 隔离的本机 AST 与严格 PEP 263 解码提供符号/语义哈希(无调用图)。**JavaScript/TypeScript**(.ts/.tsx/.js/.jsx/.mjs/.cjs/.mts/.cts)、**Rust**、**Java** 是内置轻量符号词法。其他语言可经 `extensions` 以文件粒度入库,仍有账本/经验/注入/腐烂检测,但无符号下钻与调用关系。
- **要不要先"全库分析"?** 不用。init 只建结构骨架(AST,免费);语义知识按需生长——批量消化又贵又浅还立刻开始腐烂,详见设计文档"冷启动:允许空洞的塔"。
- **知识错了怎么办?** AI 读原文发现冲突时,按纪律以原文为准并 `kb_verify refute` 勘误;基于错误知识推导出的条目会被级联降为 suspect。两条知识互相矛盾且当场断不了对错时,可登记 disputes 待裁决,双方并存呈现、都标"裁决前别信"。升级 verified 与勘误义务对称:confirm 也必须附验证依据并留确认记录,没验证过的结论洗不成可信知识。分居不同节点的矛盾,用 `kb_maintain patrol` 按关键词簇聚成一张简报跨节点并读裁决。
- **代码改了知识会不会过时?** 会,而且系统知道:锚定哈希检测腐烂;自身名免疫的结构哈希寻找改名/挪动候选;doc 敏感迁移护栏阻止“改名时顺手改了契约”被静默判新鲜。无法证明安全的迁移会保留知识与血缘,但降为 `suspect` 等重验;同会话内重读变更节点会收到过时警报,suspect 进入维护欠账队列。
- **有 `.knowledge/` 就等于已经备份了吗?** 不等于。随分支/团队共享只在持久正本真的被 Git 跟踪时成立。运行 `iknowledge doctor --repo .` 可发现部分/全部未跟踪、外层 ignore 吞目录或知识根根本不在 Git 工作树；本机运行态仍应保持不跟踪。
- **语义/向量检索会上传仓库,或需要另一个 MCP 服务吗?** 可选 **preview 已实现且缺省禁用**。本机 Ollama 与远程 HTTPS OpenAI-compatible endpoint 都由 iknowledge 内部 HTTP provider 直连,不是另一个 MCP 服务。纯 Go Flat 派生索引只含脱敏的 `current / risk / history` 知识卡,绝不含源码切块；只有 current 与 lexical 做 RRF,risk/history 只是历史决策提醒,不阻断、不裁决。provider、索引或资源异常时完整回退关键词/结构检索。

  配置按 canonical repo 写入仓外用户私有态,不会进入 Git。`manual` 是默认重建策略；`ai-local`/`ai-remote` 只授权 AI 按 status 指引每会话尝试同步一次。同一会话并发或后续重复 sync 会确定性成功返回 `skipped/already-attempted`,不接触 provider,也不再诱导 agent 重试；首次 claim 后,该会话的 `kb_status` 同时抑制 `semantic_action`。有完整校验且向量空间一致的旧索引时，获胜的 MCP sync 会按完整 record identity 复用未变向量，只 embedding 新增/变化知识；删除不产生文档 embedding。3000 条交互上限按这部分 delta 计算，因此 casino 这类总记录超过 3000、日常只改少量知识的仓库仍可自动同步。没有可安全复用的旧代时，MCP 把当前全部卡视为 pending；只要未超过上限，同一次 sync 就能退回完整重建。只有安全评估后的 pending 超过 3000，或完整性/canary 检查使这次 MCP 尝试不能安全继续（例如大代际发生漂移）时，才转为 CLI 完整重建；任何情况都不混用向量空间。日常 `kb_status` 不复述模型、记录数或 provider 探测状态；启用后确需人工处理时只给精简 `semantic_attention`。需要诊断时显式调用 `kb_semantic status`。

  远程 key 只读 `IKNOWLEDGE_EMBEDDING_API_KEY`,并须由 `IKNOWLEDGE_EMBEDDING_API_ORIGIN` 绑定唯一目标。iknowledge 不自动安装 Ollama、下载模型或静默换模型。最短本机用法如下:

  ```bash
  ollama pull qwen3-embedding:0.6b
  iknowledge semantic configure --repo . --endpoint http://127.0.0.1:11434/v1 --model qwen3-embedding:0.6b --dimensions 0 --query-profile auto --rebuild-policy manual
  iknowledge semantic rebuild --repo .
  ```

  若要授权 AI 在需要时同步本机索引,把 policy 改为 `ai-local`;远程同步必须另行明确选择 `ai-remote`,且 endpoint 必须是非回环 HTTPS。完整实现、安全、资源与后端升级契约见 [knowledge-impl.md §8.1](knowledge-impl.md#81-可选语义检索-preview已实现默认禁用);确定性 fixture 与真实模型晋级协议见 [eval/semantic/README.md](eval/semantic/README.md)。当前 4 个手工 case 只守算法回归,不代表真实模型质量,所以 preview 仍保持显式启用。
- **安全模型?** 缺省监听 `127.0.0.1`,Origin 校验挡浏览器 DNS rebinding。stdio/hook/scout 即使业务 Bearer 关闭也会做仅回环可用的双向 HMAC listener 身份校验;共享机器再用 `serve --auth`,让业务端点额外要求根 Bearer 或 scope 短 session。长期密钥/scout 信任按 canonical repo 分仓写用户私有配置态(Unix 文件 0600),旧仓内 token 只触发安全轮换、绝不复用。`.knowledge` 写入与源码读取都拒绝根以下 symlink,git tracked symlink 不能引流到仓外。显式非回环明文 HTTP 仍不提供传输保密。
- **没有子代理能力的宿主怎么用侦查?** `kb_investigate` 缺省是委派模式。宿主没有子代理时,设 `scout: self`,核对命令后运行 `iknowledge trust-scout --repo .`。授权在仓外用户私有态,绑定精确模式/命令且配置一变即失效;仓库内 executable 一律拒绝。仓内临时 MCP 配置只含短期 HMAC session,不含根密钥。之后服务端用 PTY 拉起受信侦察兵并等协议交卷。仅 macOS/Linux。
- **自定义子代理(审计 agent 等)没有 kb_* 工具怎么查库?** 用只读腿:`curl "http://127.0.0.1:<端口>/recall?q=<词>"`(`/map`、`/status` 同理)——有 shell 就能查,零 MCP 配置,输出与工具一致;侦查简报也会自动附上这条降级路径。只读:记账与沉淀仍由主 AI 收尾。
- **Codex 能用吗?** 能,已实测(codex-cli 0.142,含桌面 App):`iknowledge setup` 的第 ④ 段贴进已信任仓库的 `.codex/config.toml`(stdio 形态含 `command = "iknowledge"`、绝对 `--repo` 与 `cwd`;http 直连备选也会打印),纪律段贴进 `AGENTS.md`。不要默认写 `~/.codex/config.toml`；只有明确想让每个无关 Codex 项目也启动这个仓库的 knowledge MCP 时才用全局段。差异两点:Codex 对 MCP 工具调用会弹一次审批(交互界面点允许;headless `exec` 需 `--dangerously-bypass-approvals-and-sandbox`);无 hook 注入机制,靠纪律主动查询。

## 状态

第一期已全量交付并持续加固:现为 17 个 MCP 工具 + `/mcp/main`、`/mcp/scout` 双端点 + `GET /inject` 与只读腿(`/recall` `/map` `/status`)+ `iknowledge hook/setup/maintain/doctor/brief/precheck/semantic` 套件。最新一轮运维加固加入 Git 正本诊断、保留 generation 的请求同步、确定性的变更记账节点恢复、Codex 项目级配置、同会话 semantic 幂等保护与认证后的构建感知 daemon 换代。2026-07-11 对抗审计集中加固了可崩溃恢复的多文件事务、严格/便携 bundle、解析边界与语义哈希、代际索引、并发快照、源码/存储 symlink 边界、listener 身份、自派侦查信任和跨平台校验安装;2026-07-18 又加入语义写入与 bundle 导入默认秘密脱敏、预算化新会话简报、暂存区风险/记账预检。2026-07-04 补齐原二/三/四期计划:全仓调用图与结构扩展检索、热点待消化清单、矛盾裁决登记、非代码知识复核提醒、`--auth` 鉴权、单进程多仓库、Windows 支持、PTY 自派侦查备模式。**客户端双实测通过**(Claude Code + Codex,含 instructions 语义)。**M1.4 A/B 验收达标**:10 个固定定位任务,接知识库(种子覆盖 19%)vs 裸 grep 同模型双跑——中位 token 省 41%(59% ≤ 60% 阈值)、8/10 任务更省、用时更短;协议、工装(`cmd/kbeval`)与两轮全量数据在 [eval/m14/](eval/m14/)。

可选语义/向量检索 preview 已交付,但仍需每仓显式开启且缺省禁用。确定性离线算法基线(`cmd/kbsemeval`、[eval/semantic/](eval/semantic/))已守住 lane 隔离、distinct-node 排名与精确稳定的获胜记录顺序;“只提醒不裁决”由 engine 测试另行守住。基线使用手工预计算向量,**不代表真实模型质量**。100+ 条独立中英 qrels 的真实模型 vs lexical 晋级评测仍未完成,因此词法/结构检索仍是基线与回退路径。

- [`knowledge.md`](knowledge.md) — 概念设计全案(20 轮设计讨论的收敛:五个维度、自愈机制、经济学、安全、四篇推演)
- [`knowledge-impl.md`](knowledge-impl.md) — 现行工程实现规范(包结构、数据模型、存储、MCP API、semantic preview、里程碑)
- [`eval/semantic/README.md`](eval/semantic/README.md) — semantic 检索算法回归 fixture 与真实模型质量晋级协议

## 许可证

[MIT](LICENSE)——商用与非商用、修改、再分发均自由。唯一依赖 `gopkg.in/yaml.v3`(同为 MIT/Apache-2.0)。
