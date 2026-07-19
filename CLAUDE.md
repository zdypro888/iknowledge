# iknowledge 开发约定

AI 代码知识库(MCP 服务)。**两份核心设计正本与一份 semantic 专项契约共同构成现行规范**,实现与其冲突时要么修文档(留痕)要么修实现,不许静默偏离:

- [knowledge.md](knowledge.md) — 概念设计全案(五维模型、自愈机制、两条铁律、五篇推演)
- [knowledge-impl.md](knowledge-impl.md) — 第一期工程方案(数据模型、存储、MCP API 全量规范、里程碑)——**核心实现照这份**,已含推演五的全部定案
- [vecdb.md](vecdb.md) — 可选 semantic/vector preview 的专项实施契约；能力已实现但默认禁用，离线算法回归基线已交付，真实 embedding 模型质量晋级门尚未完成

## 铁律(违反即返工)

- **零重依赖**:第一期仅允许 `gopkg.in/yaml.v3`;禁止引入 MCP SDK/JSON-RPC 框架/tokenizer——JSON-RPC 2.0 手写(风格参照 aibridge 的 `internal/bridge/mcp.go`)
- **工具对源码只读**:仓库内容只写 `.knowledge/`(设计铁律二),永不改源码;仓库外写入仅限按 canonical repo path 分仓的用户私有运行态(auth/local identity/scout trust/semantic provider 配置/崩溃 WAL)、显式 `export -o` 制品与 install/uninstall 用户级部署,除此之外都是 bug
- 包依赖方向:`mcpserv → engine → {store, index, parser, model, pty, semantic, vector}`;`model`/`pty`/`semantic`/`vector` 不依赖其他内部包
- 表驱动测试;所有 YAML 写入走 temp + fsync + `os.Rename` + 目录 fsync 原子写(轮 25);分片读写必须保留未知字段(yaml.Node 往返)
- 三种哈希语义不许混用:`Hash` 只管腐烂检测,`StructHash` 只找迁移候选,`DocStructHash` 决定候选能否保持 fresh(impl §5/§6)
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

**轮 29(2026-07-08,全面优化——安全/并发/性能/功能七批次)**:用户命题"找优化点并全做,做到完美"。七批落地,每批三关(build/vet/-race)全绿才进下一批:

①**安全债清盘**:P0 命令注入修复(scout_command 经 sh -c 是供应链 RCE 向量,改零依赖切词+元字符拒绝+KEY=VAL 前缀);rand 错误不再吞(session/job ID 是凭证,低熵源 fail closed);常数时间比较修(len 预检泄露 token 长度,否定 ConstantTimeCompare);HTTP 超时硬化(ReadTimeout/IdleTimeout/MaxHeaderBytes);非 loopback 绑定强制 --allow-insecure-bind(把"一条 flag 之差网络裸奔"变不可能);错误码语义(kbInvalid NODE_NOT_FOUND→INVALID_PARAMS)。

②**RWMutex 完整改造**(核心面并发化):rt.mu Mutex→RWMutex,读路径(Recall/Map/Inject/Status)改 RLock,多会话读不再串行。三项读时写外移使读路径变纯读:reconcileOnReadLocked 状态对账搬进 reconcileAllLocked(reloadLocked 末尾);ensureCallGraphLocked 走 cgMu 独立锁;sessionLedger 走 sessionMu 独立锁。死锁修复(staleAlert 去重入 RLock)。并发压测(10 goroutine×60 读 + 读写混合)-race 全过。

③**性能优化**:git trail 60s 缓存(读锁内稳态零子进程);Map 覆盖率 O(D×N)→单遍预计算 O(N);config + 源文件列表 60s TTL 共享(LoadConfig 错误不再吞,进 kb_status);file→nodes 索引(Inject 文件域查询免扫全表)。

④**新功能四项**:导出/导入(iknowledge export/import,.kbundle tar.gz,跨仓 --remap 路径重映射);kb_revert(撤销全错的 record_change/verify,model.Change.Reverts 追加式不变量);kb_session(本会话读写统计,查 usage log 零存储);注入排序(verified+近期确认活过 token 预算)+ 跨节点去重(cross-dup debt,补 dup-entries 同节点盲区)。工具数 13→15。

⑤**trigram 模糊搜索**:index 建三字母组索引,精确 token 命中<3 时回退(authentication↔Authenticate 近似命中)。仅 ASCII token(中文不切)。

⑥**JS/TS 解析器**(多语言 T1):纯 Go 轻量词法(零运行时依赖,Node.js 不自带 AST 解析库),.ts/.tsx/.js/.jsx/.mjs/.cjs/.mts/.cts。近似解析(宁缺不要错),格式/注释免疫哈希 + 改名免疫 StructHash。恒注册(纯 Go 无需探测)。

⑦**工程化/健壮性**:atomicWrite symlink 防线注释;fsyncdir build tag !unix→windows(+unknown 报错版);Python helper `-I -S` + 最小环境/非仓库 cwd;mergeUnknown/cloneNode 深度限(防恶意嵌套栈溢出);只读端点 limit 钳制;session 后台回收 goroutine + cap 10000。

全轮零重依赖守住(go.mod 仍只 yaml.v3)、工具不碰源码、包依赖方向不变。新增 ~1500 行 Go(含测试),总测试 149→175+。

**轮 29-续(2026-07-08,功能扩展——多语言覆盖 + 健康度 + 许可证定案)**:用户命题"还需要什么功能",做了三件——①**Rust(.rs)/Java(.java)解析器**:纯 Go 轻量词法(同 TS 范式,零运行时依赖),多语言符号级覆盖 Go/Python/JS-TS/Rust/Java 五语言;②**kb_status 知识健康度仪表盘**:置信度分布/平均年龄/suspect 积压/近 30 天活动,零新存储运行时聚合;③**许可证定案 MIT**(LICENSE + 双语 README 声明),消除发布前唯一待办。留痕:README 双语 FAQ 五语言声明、CLAUDE.md 待办①许可证划掉。

**轮 30(2026-07-08,定位升级——从"记忆"到"防重复犯错 + 协作定位")**:用户命题"不只是 memory,要避免重复犯错,要能配合 AI 定位问题/代码"。审出三个真实盲区,全用已有数据重新聚合(零新存储/零新依赖),三个机制落地:

①**写入时方案防撞(轮30-A)**:kb_remember 提新方案时,自动比对历史 rejected(bigram>0.8 命中)。分级:带 disputes(已主动声明矛盾)→温和提醒;没带→强警告(含 change ID/被否方案原文/理由)。不阻断写入(知识导航源码拍板)。解决"换个 AI 会话又提曾被否决的方案"。

②**kb_diagnose 症状→位置反向定位(轮30-B)**:AI 输入症状/报错,系统返回最可能位置 + 相关 pitfall + 排障流程 + 历史否决方案。与 kb_recall(位置→内容)相反。复用 ix.Search(已索引 pitfall 文本)+ keywordOverlap(Flow.Troubleshoot)+ ix.History(rejected 上下文)。pitfall 优先排序 + 放松 fresh 限制(diagnose 该看 suspect 地雷)。工具数 16。

③**雷区标记 + Inject 强警告 + kb_status 雷区 TOP5(轮30-C)**:index.Build 预计算 landmine 分(变更频次 + 推翻×2 + refute 数)。Inject 对 landmine≥3 的文件强警告"动手前必读否决理由"。kb_status 加雷区 TOP5(与热点 TOP5 语义不同:热点=常改该消化,雷区=易错该警惕)。

回归:防撞(相似强警告/disputes 软化/不相似不误报)+ diagnose(pitfall 命中/rejected 上下文/无命中引导)+ 雷区(Inject 警告/Status TOP5/无信号不误报)共 9 测试通过。build/vet/-race 全套绿。零新依赖(go.mod 仍只 yaml.v3)。

**轮 31(2026-07-09,运维与收尾闭环)**:把"下一步缺什么"做成可执行闭环而非口头建议。①新增 `iknowledge doctor` 仓库/部署自检:初始化、config、parser health、维护欠账、活跃 wip、PATH/常见部署软链、误留 `iknowledge serve` 进程提示;②导入升级为报告化 `ImportWithOptions`,CLI 支持 `import --dry-run --backup --max-entry-mb`,导入前备份写入 `.knowledge/local/import-backups/`,并限制单条 bundle 大小;③`kb_session` 增加 `gate` 质量门,任务收尾提示失败调用、读而未沉淀、多次读取未沉淀、suspect 使用风险与 record_change 义务;④`kb_recall`/`kb_diagnose` 输出命中解释(score/归一化来源/pitfall 命中数),降低 AI 误把弱相关当确定结论的风险;⑤`iknowledge maintain --plan` 输出按债种分组的维护路线。顺手修 import tar slip 后续问题:bundle 只允许 tree/journal/flows/topics/config,排除 local/wip/MANIFEST/路径逃逸,写入统一走 store 原子写。回归覆盖 dry-run/backup、parser health/doctor、session gate、导入路径限制。仍零新依赖,不改服务模式。

**轮 32(2026-07-09,地基验证 + 智能升级——"完善智能"三件)**:用户命题"按最完美方法做,尽量完善智能"。①**端到端工作流集成测试**:4 个测试模拟 AI 真实工作序列(record→recall→防撞→revert / diagnose 定位 / 雷区累积 / 跨会话 stale),验证轮29~30 所有功能协同正确(此前 181 测试全是单工具单点)。发现 stale 测试构造要点:Go parser 哈希 gofmt 免疫且行内注释不参与,须改实际语句才触发失配;②**变更影响面分析**:record_change 回执末尾用 callgraph calledByOf 报告波及调用方——改一个函数立刻知道"被 N 处调用,其中带契约知识的 M 处可能被破坏,建议 kb_recall 复核"。从"记忆"到"主动协作"的关键一跃;③**知识缺口发现**:kb_status 加知识缺口 TOP5(被依赖 calledBy≥2 却零活跃知识的节点),主动提示"这块每次用都得从零读代码,该沉淀契约/坑了"。三批次 build/vet/-race 全绿,新增 ~470 行(含测试),总测试 181→193。

**轮 33(2026-07-09,预编译发布)**:release workflow 在 `v*` tag 构建 darwin/linux/windows × amd64/arm64、生成 sha256sums 并创建 GitHub Release;install.sh 优先预编译、无匹配资产回退 go install,双宿主 skill 同步安装。`v0.2.0` 为首个标签;随后主线补 Windows arm64 与 installer 静态加固。

**轮 34(2026-07-11,先 pull 最新后全仓审核并修复)**:安全/事务/解析/并发四镜头清盘。①self scout 授权、auth 根 token、本机 listener identity 全迁仓外用户私有态;stdio/hook/scout 无论 Bearer 开关均先做 loopback 双向 HMAC,仓内临时配置只含 scope 短 session;源码与 `.knowledge` 根以下 symlink/非普通文件统一拒绝。②record_change/verify/revert/adopt/task-complete/import 统一用仓外 prepared/committed WAL,恢复只允许 writer-lock owner,panic 同进程回滚;结构化 Entry/Node effects 使 revert 可证明、旧记录 fail closed。③bundle 必须唯一 manifest/单 gzip,限制单条/总量/header/staging,拒尾随、非普通、不可便携与运行时不可见路径;默认不覆盖异内容,显式 `--force`;remap 覆盖 project/tree/flow/journal/config 并做最终引用/月分片/effects 校验。④Go 哈希/调用图按 `(dir,package)` 隔离,Python `-I -S`+PEP263,TS regex/class initializer,Java/Rust 边界修正;DocStructHash 护栏。⑤session ledger/callgraph 不可变快照,重复 Node ID 全隔离,历史/FlowStep 按 Since 路由代际,拆分 entry 找真实继承者,WIP 按 session。⑥安装/卸载/发布跨平台与 checksum/原子性加固。README/两份正本同步;交付门仍为 build/vet/全量 race + shell 语法/脚本动态回归 + Windows 双架构交叉构建。未发布/未打新 tag。

**轮 35(2026-07-18,参考 ProjectMem 后的选择性吸收)**:只吸收能强化“代码知识库”边界的机制,不搬全局跨项目记忆、watcher/dashboard、把 commit 前缀自动当决策或虚构金额 ROI。①所有语义写入口与 bundle 导入默认秘密脱敏(厂商 token/JWT/Bearer/私钥/URL 凭证/常见 credential 赋值),命中只回执类型/数量,原文不落盘;②新增 `iknowledge precheck` 暂存区预检,把源码增改删/重命名映射到历史否决、suspect/orphan/pending、未决矛盾、雷区/pitfall,记账只认相对 HEAD 新增且节点精确覆盖源码的 journal;缺省告警,`--strict` 才阻断;③新增 `iknowledge brief --budget` 一屏 Markdown(WIP/风险/近期决策/维护债),严格预算且始终保留防投毒数据框;④usage 只记录真实 precheck 次数/告警/阻断;⑤CLI/MCP 统一使用 `internal/buildinfo` 发布版本。仍零新依赖、MCP 工具数不变、只写既定边界。

**轮 36(2026-07-19,分支合并收口 + 向量检索定案留档)**:把轮35能力合并到轮34之后的事务、bundle、listener identity 与解析加固主线,冲突处理以主线安全边界为底座并补齐回归:bundle 在 remap 后/写盘前脱敏且 dry-run 同报告;JSON-RPC 真 4MiB 上限、parse/invalid request 与显式 `id:null` 语义修正;stdio 同步转发 `id:null`;session 改机会性回收+容量上限,移除无生命周期后台 goroutine;自派侦察兵兼容不读 PTY 而直接交卷的非 TUI 命令。新增 [`vecdb.md`](vecdb.md) 定义当时尚未实现的可选语义检索层:默认纯 Go Flat 派生索引、仅摘要、混合召回、Zvec 只作规模增长后的可选后端;不得把设计文档误报为现有能力。

**轮 37(2026-07-19,semantic/vector preview 落地；历史状态，资源/类型卡边界已由轮 38 及后续加固取代)**:按 vecdb 契约交付默认禁用的可选语义召回。新增 standard-library-only `internal/semantic`(自有 Embedder、OpenAI-compatible/Ollama HTTP、安全 URL/redirect/secret/context/资源边界)与 `internal/vector`(连续 float32 Flat snapshot、稳定 Top-K、有界 checksummed codec)，不引入 Eino/eino-ext 或第三方 MCP；Eino 仅参考 nbco 的 provider 抽象。provider 设置只由 `iknowledge semantic configure|enable|disable|status|rebuild|clear` 写 canonical-repo 仓外 0600 私有态，loopback Ollama 不读取远端 API key，远程只允许 HTTPS，configure/status 不联网，只有显式 rebuild 批量发送脱敏 active summary/era_summary。`kb_recall` 保持精确节点零 provider 短路，普通意图以 lexical+semantic 分池 RRF、当前 source hash 复核和结构扩展返回；missing/stale/corrupt/超限/provider 失败一律提示并降级。派生索引 `.knowledge/local/vector.idx` 用独立锁、不可变 generation 与原子流式写，硬界 top_k 100、dimensions 4096、vector 512MiB、HTTP 30s；本段记录的“多仓 coordinator 未交付/summary-only”只代表当轮中间态，当前能力以最后一轮进度与 `vecdb.md` 为准。

**轮 38(2026-07-19,语义亮点升级——证据分道 + 历史决策提醒)**:在仍默认禁用、零新依赖的边界内把 preview 从“摘要相似搜索”升级为证据感知召回。①仓外配置新增具体化 query profile(`auto` 保存时按 Qwen3→`qwen3-code-v1`,否则 `plain`)与 `manual|ai-local|ai-remote` 持久 sync policy;`kb_status` 纯本地报告 semantic health/next_action,新增 `kb_semantic status|sync`,工具数 16→17；只有用户事先选 ai policy 才允许 AI 每会话按需同步一次,server 并发安全地硬限首次 sync 尝试（失败也耗额度、重复零 provider）,绝不代配/下载/换模型。②从 truth graph 生成 `current/risk/history` 三类脱敏卡(活跃契约/用法、pitfall/dispute/仍生效否决、决策史/era/推翻来路/flow),一次 Flat 扫描为三 lane 各做 NodeID distinct Top-K;current 才与 lexical RRF,risk/history 只作 advisory。③source 集合变化不再整库盲目 stale,改为 `partial`:逐条验证 record/node/lane/source hash,只复用未变旧卡;provider/model/profile 变化仍硬拒绝。④OpenAI-compatible rebuild 每批把 document/query 双模式 canary 放进同一请求,query miss 也与 query-canary 同请求;canary/provider 构建失败在原子 rename 前保留上一 generation。⑤`kb_task start` 加“历史决策提醒”,用 risk/history 提示换说法的坑与否决,明确只是辅助、仅供参考、不阻断、不裁决。⑥新增离线 `cmd/kbsemeval` + `eval/semantic/v1/qrels.jsonl`,严格回归三 lane Recall/MRR/precision、distinct-node、`expected_hits` 精确顺序与 ranking violations=0；advisory-only 由 engine 测试守住。当前仅 4 个手工预计算向量 case,不是模型质量声明,100+ 独立中英真实模型 qrels 晋级门仍待完成。仍不引 Eino/eino-ext、不需要第三方 MCP、不自动安装本机模型、不把 preview 宣称为默认。

**轮 38-续(2026-07-19,对抗加固与进程资源闭环)**:把“亮点”从功能演示补成可守的运行时契约。①Flat 三 lane distinct-node 改成过滤前置、每 lane 只留 `top_k` 的有界 heap+位置表，空间 `O(lanes×top_k)`；typed source 在主锁内只收集受 item/字段/总字节上限约束的不可变 DTO，时态 change/flow 按 At/Since 解析全部 heirs。②MCP sync 用可取消 rebuild gate 合并并发代，8min/3000 卡/100 batch 硬界，超限零 provider；status 对必超限源直接指向 CLI，CLI force rebuild 不受会话闸门。③remote key 必须以 `IKNOWLEDGE_EMBEDDING_API_ORIGIN` 绑定唯一 origin；配置每个 provider batch 前在跨进程共享锁内重验。④canary 改为 L2 归一化方向 fingerprint(v2 进入 settings fingerprint)，明确只检测常见漂移、不是 attestation；未知 provider JSON 深度限 64。⑤semantic 临时文件改专用前缀并在独立锁内限域清理；disabled/missing/corrupt/clear 统一逐出 resident/query cache。⑥单进程多仓在任何 listener 前预检 enabled `max_vector_mib` 合计≤1GiB，运行时 hot enable/load/rebuild 仍经共享 coordinator 动态 reserve/release；provider/Flat gate 全 daemon 分别为 1/2，搜索 resident lease 阻止换代与仍被引用旧矩阵叠加。⑦离线 qrels 已接入 `go test ./...` 强制门。真实模型质量门仍未过，preview 继续默认 disabled。

**轮 38-收口(2026-07-19,确定性风险防线与发布闭环)**:最终审计补齐 runtime/truth/release 边界。①关键词索引拆 current/risk，pitfall、suspect、open dispute 双方正文不再因精确词命中串入当前答案；lexical-risk 使任务历史决策提醒在未启用模型时仍能精确提示，semantic 只扩展同义发现。②MCP sync singleflight 在 owner 取消时由 live waiter 接管，provider 失败仍短时合并；metadata-ready 的 sync 先校验 payload，corrupt 不再耗掉一次机会却不修复，坏文件身份跨 source 变化持续记忆。③status 分开 configured dimensions(含 0=auto)与 index dimensions；orphan flow 降 risk，长卡保留首尾理由，overturn/revert 拒绝自身/未来/环，split lineage 的全部 heirs 进入 touching 提醒，输出预算外仍显式交付被省略 current heir IDs。④多仓共享完整 rebuild/source gate；source manifest 用独立 384MiB 累计额度+192MiB 构造授权防止多仓缓存无界增长，同仓等待与 DTO/card 预处理均可取消；shutdown 超时后不再无界等待 resident lock；rename 后错误明确报告 committed-but-stale/耐久性不确定。⑤install 在提交前按新 target 的真实 PATH 顺序校验 shadow，物理化 symlink 目录；uninstall 固化受控清单后按当前 UID 精确停服，删除后两次复扫 KeepAlive，收敛前不删 skill。⑥Release 先用 draft 上传全部资产，binary 与 bootstrap skill 同作 checksum，完整后才公开；remote key/origin 的实际宿主进程继承边界写入文档。真实模型质量门仍未过，preview 继续默认 disabled。
