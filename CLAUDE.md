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
go test -race ./...
```

一期 lint 只用 `go vet`,**不引入 golangci-lint**(与零重依赖哲学一致、减小 CI 面;复评点 M1.4)。CI(.github/workflows/ci.yml)与本节保持一致:ubuntu + macos + windows 矩阵跑同三条命令(flock/LockFileEx/rename/目录 fsync 平台敏感,三平台护栏;-race 因并发面是核心:rt.mu/flock/会话台账)。

## 当前进度

设计定稿(2026-07-04 推演五修订 + 轮 23/24,见 knowledge.md 附录 A)。**全量实现已交付并经对抗审查修复**(轮 24):13 个 MCP 工具 + /mcp/main 与 /mcp/scout 双端点 + GET /inject + 会话台账/过时警报/任务态/侦查委派/维护欠账/时代摘要/使用日志(~1 万行 Go,含测试;engine 70%/index 81%/mcpserv 71% 覆盖)。57-agent 对抗审查确认的 42 处真问题全部修复并加回归测试(路径穿越、conflict 分片数据丢失、迁移自毁、record_change 原子性、init 加锁、lint 误杀、错误码语义等)。测试全绿,curl 协议级自测全过。轮 25(2026-07-04 第三方全仓审计,go-audit):P0=0、P1=3 全修,共修 19 处(P1:remember 拒收路径污染 keywords 缓存、record_change 同文件双新符号重复节点、MCP 规范 Origin 校验缺失)+ 审后追加 8 项(atomicWrite/journal fsync 耐久、serve SIGTERM 优雅停机、version 子命令、断开 index→store 依赖边使代码符合 impl §2 声明、framed() 伪造框架标记消毒、ledgerTTL 更名去歧义、非回环监听裸奔警示、字符串拼接清理);台账字段(what/why/rejected/wip)定案不过指令 lint、靠数据框隔离(impl §7.3/kb_task 已留痕)。轮 25 补交付**接入套件**:`iknowledge setup`(打印 .mcp.json/纪律段/hook 三件套,只打印不代写)+ `iknowledge hook`(PostToolUse hook 桥,失败一律静默退出 0;挂接点 PreToolUse→PostToolUse 已在 impl §7.1 勘误留痕;§9"唯一注入腿"更新为三条腿)+ README 重写(傻瓜部署/简易使用/速查/FAQ)。casino(48.6 万行/1522 文件)实测:init 877 文件→15493 节点 13.3s、幂等 0.98s、kb_status 812ms、kb_map ~105ms;**hook 注入 PTY 闭环通过**(Claude Code 读文件后,分片里的预埋知识原文出现在其回答中)。claude CLI 真实客户端已实测通过(PTY 交互式:kb_status/kb_recall ok、Mcp-Session-Id 回带、recall 命中预埋知识)。

**轮 26(2026-07-04,设计留白清盘)**:把两份文档里可做的延后项全部落地并逐项留痕(impl §1/§2/§5/§7.3/§7.5/§8;knowledge.md §15 各期标注)——①全仓调用图(parser CallExtractor + engine 归位三规则,AST 近似宁缺毋错;casino 886 文件首建 263ms/增量 21ms/1.7 万边);②结构扩展检索一跳(命中集沿调用边+流程扩展);③热区排序(kb_status 热点待消化 TOP5,热度=(1+git 90 天改动)×(1+跨文件被调));④非代码知识时间锚(Entry.ConfirmedAt + review-overdue 债 + 无锚节点级 confirm 批量刷新);⑤矛盾裁决机械层(Entry.Disputes 单侧存储/index 反向图/双向呈现/dispute-open 债);⑥maintain CLI 子命令(只读);⑦--auth Bearer 鉴权(token 0600、常数时间比较、setup/hook 自动携带);⑧Windows 支持(LockFileEx via kernel32 LazyDLL、目录 fsync 留痕降级、CI 三平台矩阵 + -race);⑨单守护多 repo(一进程多监听,客户端配置零改动,双仓实测通过);⑩来时路(侦查简报附线索文件近 3 条提交,git 锁外);⑪PTY 自派备模式(internal/pty 手写原语零三方依赖,交卷走协议不解析终端,config scout=self,协议级假侦察兵 E2E 通过)。测试 8 包全绿(-race),新增 20+ 测试。**维持不做**(等信号,过度工程红线):tree-sitter 多语言、跨仓库、embedding、SQLite 派生缓存、TLS、stmt 节点、摘要自动重算。**M1.4 已达标(2026-07-04 两轮)**:cmd/kbeval 工装(PTY 驱动 + OTEL 遥测计量;实测定案——括号粘贴、spinner 符号确认启动、转录惰性缓冲弃用改遥测、退出需补确认回车否则终态导出丢失)+ eval/m14 固定 10 任务。第一轮种子 6% 覆盖:中位 A/B=69% 判 FAIL(不移动球门);第二轮加深种子至 19%(热点 TOP10 按热度自然入列):**中位 A=349,985 vs B=593,607(59%≤60%)判 PASS**,A 赢 8/10、用时 149s vs 161s;两轮数据并列落盘 eval/m14/(败绩不删)。**问题清单清盘(同日)**:CI 三平台真机全绿(windows-latest 实跑 LockFileEx/原子写/-race);scout 自派信任弹窗兜底(活动符号检测+重投,E2E 回归过);热点 TOP10 + git 计数 60s 缓存;多 repo serve 自动化 e2e;store 直测补至 71.5%;**Codex 端到端实测通过**(App 内嵌 codex-cli 0.142.5:kb_status/kb_recall/instructions 语义三项全绿,§7.1 兼容矩阵补齐)。遗留:①许可证选型(MIT/Apache-2.0,用户拍板);②真 claude 驱动的自派实测(链路已由假兵 E2E + 兜底验证,等真实使用场景);③阈值标定(token 系数/bigram/复核 90 天,等真实语料)。原 M1.1–M1.4 里程碑保留作验收清单用。

**轮 27(2026-07-04~05)**:①**边界定案**(用户定调:知识库对应代码,不是记忆库)——知识判据"代码变了它会失效吗?"+ 三不进(通用编程知识/会话偏好/任务待办),纪律段+机械警示(TODO 拦截)+无锚提醒三层落地;②**codegraph 对照借鉴**(只捡便宜不搬架构):接口→实现边(方法集匹配 AST 近似,宁缺三闸;修 calledBy 已知盲区,casino 15.6k 节点首建 245ms/增量 71ms)+ kbeval 成本维度(cost.usage USD);明确**不借**:SQLite/watcher/tree-sitter(护城河在经验/账本层);③**多语言两档**(核心零改动,全部成本在插件面):T0 通用文件级(config `extensions` 白名单缺省关,FileHasher 能力接口)+ T1 Python 自托管解析器(python3 ast 助手脚本,不可用则不注册;改名 StructHash 迁移实测通过);④**Go 稳定性加固**(用户关切"不能加功能反而难用"):python 子进程双超时护栏(探测 5s/解析 20s,坏 python 不许卡 serve)、parseFailed 逐文件指纹缓存(没变的文件绝不重解析)、探测成本核账后**不做懒加载**(复杂度本身是风险)、Go 哈希路径零变化实证(存量库幂等 migrated=0 suspected=0)。

**轮 28(2026-07-07,用户命题"冲突测得到吗/库本身错了呢")**:自查定位——同节点冲突已有三层(写入 bigram 查重逼三选一、disputes 矛盾单、dup-entries 债),错误知识五件套(五级置信/refute 证据强制/based_on 级联/疫苗/腐烂+时间锚)成立;审出真窟窿 **confirm 免证据升级**(写的 AI 没验证、confirm 的 AI 也没验证,库里却挂 verified——三人成虎)。落地两件:①**confirm 证据对称**(inferred/suspect→verified 强制 evidence + journal 确认记录,与 refute 同规格;verified 复确认/derived/节点级重锚豁免;置信桥提示与 MCP schema 同步);②**kb_maintain patrol 跨节点矛盾巡检**(关键词簇聚类跨 ≥2 节点活跃知识,同节点集去重、预算封顶 5 簇/30 条溢出明示,纯只读不开 job,refute/disputes 即留痕;CLI `maintain -patrol [-scope]` 同源;补冲突检测的跨节点盲区——机器聚类,AI 裁决)。留痕:knowledge.md §12.4/§12.5、impl §7.3(kb_verify/kb_maintain)、双语 README。
