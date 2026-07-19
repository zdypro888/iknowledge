# iknowledge 可选语义检索实施契约

> 状态：可选 preview 已实现，默认禁用；离线**算法回归基线**已交付，真实 embedding 模型质量晋级门仍未完成，不是默认检索路径。
>
> 日期：2026-07-19。
>
> 本文记录已实现边界与后续晋级门。YAML/journal 仍是知识正本，现有精确、关键词、trigram 与调用图检索仍是默认路径和故障回退。

## 1. 决策摘要

iknowledge 需要的是一个受控的“第三候选发现通道”，不是一套通用代码 RAG，也不是一个独立向量数据库服务。

定案如下：

1. 默认 `disabled`，不加载 provider、不发网络请求，现有行为不变；
2. 项目内实现纯 Go、精确 Flat 向量索引，只索引从知识正本确定性生成的 `current / risk / history` 类型卡片，不切源码；
3. 用户可显式选择本机 Ollama 或远程 HTTPS OpenAI-compatible embedding；两者都是 iknowledge 内部直连的 HTTP provider，**不是第三方 MCP server**；
4. provider 的 endpoint/model/dimensions/revision/enabled 与有界检索/资源参数全部保存在按 canonical repo 隔离的**仓外用户私有状态**；`.knowledge/config.yaml` 不承载这些字段；
5. API key 只从固定环境变量 `IKNOWLEDGE_EMBEDDING_API_KEY` 读取，命令行、仓内文件、缓存和日志都不保存 key；远程 key 非空时还必须用 `IKNOWLEDGE_EMBEDDING_API_ORIGIN` 绑定唯一规范 origin；
6. 查询顺序为“精确短路 → lexical 与三条 semantic lane 候选 → current 与 lexical 做 RRF → risk/history 独立告警 → 当前 record ID/node ID/kind/source hash 复核 → 既有结构邻居一跳扩展与来源解释”；三条 lane 各做 distinct-node Top-K，一个节点的多张卡不会挤掉其他节点；
7. provider/model/profile 不匹配仍是硬 stale；只有知识 source fingerprint 变化时进入 `partial`，逐条安全复用 source hash 未变的旧卡片，新改、删除或已变化卡片一律丢弃；
8. rebuild 的 OpenAI-compatible provider 在同一次 HTTP batch 中携带 document/query 双模式 canary；普通 query cache miss 也把 query 与 query-mode canary 放在同一请求，用于发现常见的意外模型漂移；它不是远端 attestation，强模型身份仍依赖受信 endpoint 与 immutable revision。这类 canary/provider 失败发生在原子写前，保留上一代；
9. `kb_status` 纯本地报告 semantic 健康；只有用户预先通过 CLI 选择 `ai-local` 或 `ai-remote`，AI 才能按 `next_action` 调一次 `kb_semantic action=sync`。`manual` 永远不授权 MCP 重建；
10. Eino/eino-ext 只作接口与 provider 实现参考，当前不是依赖；Zvec 只在真实规模越过门槛后作为可选 adapter。

换句话说：在不改变 iknowledge 单二进制、零重依赖和“知识导航，原文定论”边界的前提下，补上有限的语义召回能力。

## 2. 目标与非目标

### 2.1 目标

- 找回措辞不同但确实相关的设计摘要、约束、历史背景和坑点；
- 保留符号、路径、关键词和调用关系的确定性优势；血缘、流程和历史仍走既有精确下钻路径；
- provider 失败、缓存损坏或资源不足时，`kb_recall` 仍能走完整 lexical/structural 路径；
- 不改变知识正本，不把向量或 provider 配置带进 Git/bundle；
- 默认构建继续 `CGO_ENABLED=0`，六个平台仍是一份二进制；
- provider、模型与向量后端可以替换，但不同 fingerprint 的向量绝不混用。

### 2.2 非目标

- 不切分或上传源码全文，不做通用源码问答 RAG；
- 不让语义相似代替调用图、流程、精确节点解析或源文件核验；
- 不把向量候选误写成一层已实现的逐条 lineage/supersedes/confidence/dispute 裁决；精确节点下钻仍由既有快照/历史逻辑呈现当前状态和负知识；
- 当前不把 semantic 候选直接接入 `kb_diagnose`；风险卡已服务 `kb_recall` 与任务开始时的历史决策提醒，diagnose 若接入仍需自己的行为评测；
- 首版不实现 HNSW、分布式索引、多租户服务或百万级数据；
- 不内置 embedding 模型，也不要求 Docker、Python、SQLite、Faiss 或常驻向量数据库；
- 当前不依赖 Eino/eino-ext、chromem-go 或 Zvec。
- 不让“历史决策提醒”自动拒绝任务、推翻决定或修改知识；它只给出带精确引用的辅助提醒，源码与人工/AI 复核仍负责裁决。

## 3. 用户模式与安全授权

### 3.1 三种模式

| 模式 | 行为 | 网络边界 |
|---|---|---|
| `disabled`（默认） | 私有状态不存在或 `enabled=false`；不构造 Embedder，不加载/查询向量快照 | 零 embedding 网络请求 |
| `ollama` | `enabled=true` 且 endpoint 为 loopback；由 iknowledge 直连 Ollama 的 OpenAI-compatible embedding API | 允许 loopback `http` |
| `remote` | `enabled=true` 且 endpoint 非 loopback；由 iknowledge 直连 OpenAI-compatible `/v1/embeddings` | 只允许 `https` |

三种模式由 `enabled` 与规范化 endpoint 推导，不需要再存一个可漂移的 `mode` 字段。它们都是 iknowledge 自身的 provider 模式。AI 客户端仍只连接一个 `knowledge` MCP；用户无需安装、暴露或信任任何第三方向量 MCP。

### 3.2 配置只能来自仓外私有状态

已实现命令：

```text
iknowledge semantic configure --repo <repo> \
  --endpoint http://127.0.0.1:11434/v1 --model qwen3-embedding:0.6b \
  --dimensions 0 --query-profile auto --rebuild-policy manual

iknowledge semantic configure --repo <repo> \
  --endpoint https://api.example.com --model <model> \
  --dimensions <n> --revision <immutable-model-revision> \
  --query-profile auto --rebuild-policy ai-remote

iknowledge semantic enable  --repo <repo>
iknowledge semantic disable --repo <repo>
iknowledge semantic status  --repo <repo>
iknowledge semantic rebuild --repo <repo>
iknowledge semantic clear   --repo <repo>
```

`configure` 只接受非 secret 的 provider 元数据,保存后同时 enable,但**不联网、不自动重建**;
`enable` 只用于重新启用既有配置。配置按 canonical repo 写一次后跨 MCP 会话、serve/CLI
重启持续生效,不需要每次调用重填；仓库移动或另一份 clone 的 canonical path 不同,会进入新的
安全分区并回到未配置态。iknowledge 不自动安装 Ollama、下载模型、猜 endpoint 或运行中换模型。

`query-profile auto` 不是持久化的模糊状态：CLI 在保存时把包含 `qwen3-embedding` 的模型名
具体化为 `qwen3-code-v1`,其他模型具体化为 `plain`;配置文件只保存具体 profile。改变 model
而未显式指定 profile 会重新执行 auto,防止 Qwen instruction 残留到别的模型。`qwen3-code-v1`
只给 query 加固定 code-retrieval instruction,documents 保持原文模式；profile 进入 settings/
embedder fingerprint,改变后旧索引硬 stale。

`rebuild_policy` 是“谁可发起显式同步”的持久授权,不是后台任务：

| policy | 行为 |
|---|---|
| `manual`（默认） | 只有用户运行 `iknowledge semantic rebuild`；MCP `sync` 拒绝 |
| `ai-local` | 只允许 loopback endpoint；`kb_status` 需要同步时给出 `kb_semantic action=sync` |
| `ai-remote` | 只允许非 loopback HTTPS；用户明确授权后同样允许 MCP 按需同步 |

AI 每会话先看 `kb_status`;只有 `next_action` 精确要求 sync 且 policy 为 `ai-local/ai-remote`
时,本会话最多调用一次 `kb_semantic {action:"sync"}`。这不只靠提示词：server 在同一
`Mcp-Session-Id` 下并发安全地只受理**第一次 sync 尝试**；无论成功、未获 policy 授权还是
provider 失败,后续调用都在接触 provider 前返回 `SEMANTIC_SYNC_ALREADY_ATTEMPTED`。用户手动
运行 CLI `semantic rebuild` 不受该会话闸门影响；新 MCP 会话仍须重新先看 status。ready/none/
manual/disabled/unconfigured 都不得调用。`kb_semantic status`、`kb_status` 与 CLI `semantic status`
只读本地状态及有界 wrapper metadata；serve 启动同样不构造或探测 provider。它们都不读 API
key、不联网。CLI `semantic rebuild` 或获授权的 MCP sync 才批量发送脱敏类型卡；有效索引的
普通 recall 只发送脱敏查询。`disable` 阻止新请求；`clear` 只删除可重建索引并保留 provider 配置。

配置写与 provider 请求还共享独立的跨进程读写锁：query 或 rebuild 的**单个 HTTP batch**只在
请求边界持共享锁，`configure/enable/disable` 原子写配置时取排他锁。若恰有在途请求，CLI 会
明确报 busy 而不会假装禁用成功；重试成功后，已排队请求及重建的下一批都会重新核对完整
enabled/endpoint/model/profile/policy 并在 HTTP 前终止。长 rebuild 不会把授权锁钉住整代，
因此 disable 可在批次之间生效。

本机默认推荐 `qwen3-embedding:0.6b`（实际输出 1024 维）；配置仍建议 `--dimensions 0`
让首次 rebuild 从 provider 响应确定并固化维度,避免手填错误。模型由用户自行安装,例如
`ollama pull qwen3-embedding:0.6b`;iknowledge 不下载、不启动、不自动部署 Ollama 或模型。

若本机的 nbco 已通过 Ollama 部署 `bge-m3`,可在 iknowledge 中填写同一个 loopback endpoint
与 model 名复用**模型服务**,无需再拉另一份模型。复用不等于共享数据库:nbco 与 iknowledge
各自维护独立的文档、fingerprint 和向量索引;iknowledge 的索引仍只在当前 canonical repo 的
`.knowledge/local/vector.idx`,不会读写 nbco 的 Qdrant/索引。

实际状态文件为：

```text
<user-config>/iknowledge/state/repos/<sha256(canonical-repo)>/semantic-config-v1.json
```

它复用现有 private-state 的 canonical-path 分仓、拒绝 symlink、目录 `0700`、文件 `0600` 与安全原子写。文件只保存：

```text
schema, enabled, endpoint, model, dimensions, revision,
query_profile, rebuild_policy,
top_k, min_score, max_vector_mib, timeout_seconds
```

后四项只是有硬上限的本机检索/资源参数，不参与外发目的授权：默认分别为 `20`、`0.35`、
`512`、`30`。它们与 provider 元数据一样只能由 semantic CLI 写仓外状态，仓内不能抬高。

settings/embedder fingerprint 覆盖 normalized endpoint、model、dimensions、revision、
具体 query profile 与卡片/预处理/排序协议版本,不包含 API key;任一向量语义变化都会让旧索引
硬 stale。revision 可留空,但显式填写模型 snapshot/tag 更利于审计。运行时另存 document/query
两种固定 canary 的向量 fingerprint,检测常见的“同名模型在服务端意外漂移”；不匹配时拒用
旧索引并要求 rebuild，避免无意把不同向量空间拼进一个 generation。canary 不能证明恶意远端
的模型身份：endpoint 可按输入路由或伪装固定 canary；需要强身份时必须使用受信服务与 immutable revision。

API key 固定只读 `IKNOWLEDGE_EMBEDDING_API_KEY`；不支持 `api_key` 或可由仓库选择的 `api_key_env` 字段。远程 key 非空时还必须由非秘密环境变量 `IKNOWLEDGE_EMBEDDING_API_ORIGIN` 绑定一个规范 `scheme://host[:port]`，与当前 endpoint origin 不同就拒绝在 HTTP 前；这避免多仓 daemon 把进程级凭据误送到另一服务。Ollama 模式不读取这两个变量。

这里的“环境变量”严格指**实际发出 provider HTTP 请求的进程环境**：用户手动运行 CLI
`semantic rebuild` 时是当前 shell；MCP `kb_semantic sync` 和普通 semantic recall 时是长驻
`iknowledge serve`，它只继承启动自己的 stdio/桌面 AI 宿主或 launchd/systemd service 环境。
在另一个终端执行 `export` 不会修改已运行进程，GUI 启动的桌面 App 通常也不会继承之后加入
shell 的变量。remote 部署必须把 key/origin 放进实际宿主的受保护 service/desktop 环境并重启
宿主或 serve；不得把 key 写进 `.knowledge`、MCP 参数、仓库脚本或版本控制。若不便为桌面宿主
注入 secret，保持 `manual`，在含变量的终端显式运行 CLI rebuild 是边界最清楚的路径。

关键安全不变量：

- `.knowledge/config.yaml`、YAML/journal、bundle、MCP 参数与仓内脚本都不能写 provider 配置或把 `enabled` 改为 true；
- `init`、`import`、`setup`、`serve` 启动和读取旧 `vector.idx` 都不能隐式 enable；
- 在仓外私有状态未启用时，仓内任何内容单独都不能触发 embedding 外发；
- 启用后，仓内内容也不能改变目的 endpoint、模型、revision、请求类型或凭据来源，只能进入已授权且先脱敏的“类型卡/查询”载荷；
- canonical repo 路径变化会落到新的私有状态分区，默认重新回到 disabled；
- 不再需要单独的 trust marker：仓外 `configure` + `enable` 状态本身就是本机用户授权，仓内没有可被信任的对应配置。

### 3.3 HTTP 与数据外发边界

- 远程发送前复用现有 secret redaction；hash/fingerprint 也以脱敏后的文本为准；
- 请求 context 已贯穿 `RecallContext`/`TaskContext`/`SyncSemantic`、Embedder、`Snapshot.SearchDistinctNodesByKindsFiltered` 与 vector codec；`semantic rebuild` CLI 使用信号 context 响应 SIGINT/SIGTERM。这是已接通边界的协作式取消，不声称任意阻塞 I/O 或所有 server shutdown 路径都能立即中断；
- endpoint 只允许规范 origin/base URL，拒绝 userinfo、fragment 和歧义路径；client 拼接固定 embedding path；
- `ollama` 只允许解析后仍为 loopback 的地址；`remote` 默认必须是 HTTPS；
- 禁止自动跨 origin redirect，避免凭据或正文被转发；
- 错误、状态与日志只保留 provider/mode/model、HTTP 状态和经过清洗的原因，不记录 Authorization、API key、原始请求/响应 body 或未脱敏文本；
- provider 返回值必须逐项校验数量、维度、有限数、非零向量和响应上限，任一批次不合法则整代失败，不能发布半个索引。

## 4. 为什么自有 Embedder 足够，Eino 只作参考

[Eino](https://github.com/cloudwego/eino) 抽象了 ChatModel、Embedding、Retriever、Indexer、Tool，并提供 Chain/Graph/Workflow、agent、middleware 等编排能力；[eino-ext](https://github.com/cloudwego/eino-ext) 提供多种模型/provider 集成。它适合 `nbco`：后者本身是多模型、agent/session、tool/MCP、技能、摘要和多语义源的完整 AI 编排应用，复用 Eino 能让这些共同生命周期与 provider 适配得到摊销。

iknowledge 的边界窄得多：它只需要“批量文本 → 定长向量”这一条能力，然后在本地做 Flat 搜索。为此引入整套 agent/RAG 组件体系会扩大依赖、配置面、升级面与供应链，而不会改善知识正本、状态校验或混合排序这些项目特有逻辑。

因此定案是：

- 参考 Eino 的 context、批处理、provider 抽象和错误处理方式；
- 在 `internal/semantic` 定义项目自己的薄 `Embedder` 与 provider；
- OpenAI-compatible 与 Ollama provider 都用标准库 `net/http` 实现；
- `internal/vector` 只实现 Flat index/codec/immutable snapshot；
- `internal/semantic` 与 `internal/vector` 都不依赖 Eino/eino-ext，也不依赖 `engine`、`store`、`index` 或 `model`；
- 若未来另有产品已整体采用 Eino，可在那个产品侧写 adapter，不反向污染 iknowledge 根模块。

核心接口边界（`Embedder` 属于 `internal/semantic`，Record/Snapshot/codec 属于 `internal/vector`）：

```go
type Embedder interface {
	Fingerprint() string // provider/预处理的稳定安全指纹，不含凭据
	EmbedQuery(ctx context.Context, query string) ([]float32, error)
	EmbedDocuments(ctx context.Context, documents []string) ([][]float32, error)
}

// OpenAI-compatible 实现还提供 CanaryEmbedder / DualModeCanaryEmbedder：
// query+query-canary 一次请求；rebuild documents+document-canary+
// query-canary 一次请求。基础 Embedder 保留给 fake/自定义 adapter。

// vector.Build/DecodeWithLimits 一次产生完整 *vector.Snapshot；
// SearchDistinctNodesByKindsFiltered(ctx, query, limit,
//   []string{"current","risk","history"}, currentManifestFilter)
// 先淘汰失效 record，再用一次 Flat 扫描返回三条独立、节点去重的 Top-K；
// 不暴露可变 record/vector slice。
```

查询与文档分开是刻意契约：Qwen3 一类模型会为 query 加 instruction，不能在 adapter 内静默混用。`engine` 负责读取仓外设置、从当前知识 model 生成类型卡、调用 `semantic.Embedder`、做 lane/RRF 编排并校验 manifest record hash/节点存在性；`semantic` 只处理已脱敏的 text→vector HTTP 边界，`vector` 只处理向量、不可变快照、distinct-node Top-K 与格式校验。当前只做整代 `Build/Replace`，不暴露可被并发读到半状态的 `Upsert/Delete`。

## 5. 真实索引记录：三类证据卡

当前不是把 `summary` 换个名字塞进向量库，而是从健康 truth/index snapshot 确定性生成三类
typed knowledge cards。它们都指回当前可精确查询的节点、条目、change 或 flow 引用：

| lane | 进入来源 | 呈现语义 |
|---|---|---|
| `current` | 非 orphan 节点的普通活跃 Entry（不满足 risk 条件）、引用 fresh 节点的有效 flow title/step note/conventions | 可进入“当前答案候选”，与 lexical 做 RRF；候选标题只显示节点结构/状态，条目正文须精确下钻 |
| `risk` | pitfall、suspect/pending-anchor 节点或置信、open dispute、仍生效 change 的 rejected、flow troubleshoot，以及指向 orphan/suspect/pending 节点的 stale flow | 独立“风险警示”，只告警，不混成当前结论 |
| `history` | `EraSummary`、全部 change 决策史、已失效 change 的 rejected（含 overturn/revert 来时路） | 独立“历史来路 [historical]”，只作考古线索 |

`superseded/refuted/retired` Entry 不进入新快照；orphaned 节点的 Entry/EraSummary 也不得当作活体知识，
但既有 change/history 仍可考古，指向它的 flow 会保留为 `stale_flow` 风险线索而非当前流程。Entry 的 facet、confidence、
dispute，change ID/rejected 原因和 flow 引用会写进卡片正文或 manifest 的 facets/references；决策卡的 What/Why/Task/Rebuttal/Verified 与 rejected Option/Reason 各自有字段级首尾预算，一个超长字段不能把“原因”等其他结构字段挤掉。
让命中能回到精确 truth graph 复核；它们不是向量层自动裁决出的标签。

同一节点/lane 的材料先稳定排序、精确去重，再按约 3KiB 原始目标切成多张卡；最终 provider
文档仍受 4KiB 上限。记录形态为：

```text
record_id    card:<lane>:<node-id>:<zero-padded chunk-index>
node_id      当前节点 ID
kind         current | risk | history
source_hash  SHA-256(document-preprocess-version + redacted final text)
facets       entry kind / rejected / flow / decision_history ...（manifest 内）
references   node#entry、change ID 或 flow ID（manifest 内）
vector       L2-normalized float32[]
```

规则：

- 不索引原始源码、findings、WIP、会话台账、使用日志或模板空文；外发的是现有知识正本聚合出的脱敏卡片，不是代码 RAG chunk；
- 记录只能从 engine 已隔离 schema-too-new、分片冲突、重复 Node ID 等坏数据后的健康 snapshot 构造，不能重新遍历原始 shard 猜“谁是真”；
- 查询对 `current/risk/history` 一次 Flat 扫描，但每条 lane 独立按 NodeID distinct Top-K；当前 manifest 过滤发生在节点竞争与 Top-K **之前**，失效高分卡不能挤掉仍有效的低分回填。同一节点即使有多张高分卡，在同一 lane 也只占一个名额，同分按 record ID 稳定选择；
- change/flow 证据按其发生时刻解析 NodeID，再沿 lineage 映射到全部当前继承者；不能把旧 ID 后来复用的无关新符号误当历史主体，也不能在一次 split 后只保留字典序第一个继承者；
- current 才参与 lexical RRF；risk/history 始终分栏呈现为 advisory。cosine 只负责发现，不是 confidence，也不能把 rejected/historical 自动判成当前方案；
- 历史决策提醒不把 Top-K 当直接风险的完整性边界：对 `touching` 全部当前 heirs 做 typed risk/history manifest 补齐并直接检查 truth 中的 pitfall/suspect/pending/orphan/open-dispute；结构一跳按确定性优先级最多检查 100 个节点。即使无模型、无词面命中或第二个 split heir 落在 Top-K 外也不得漏掉 touching 直接风险；一跳超限必须显式提示不完整；
- 每个 hit 都用 `record_id` 对照**当前 immutable source manifest**，同时验证 node ID、lane/source hash 与当前节点存在；缓存不保存卡片正文，也不直接成为答案；
- 需要条目状态、dispute 双向关系或完整决策史时，必须按 facets/references/node ID 精确下钻；向量层不替代既有 lineage/supersedes/confidence 裁决。

实际送入 provider 的文档采用有版本的确定性预处理，包含 canonical node ID、lane 与脱敏
卡片正文；`source_hash` 覆盖这份**最终文本**。源集合指纹对全部记录的
`(record_id, node_id, lane, source_hash)` 按字节序排序、长度前缀编码后取 SHA-256。
map 顺序、平台路径分隔符和浮动时间不会让相同知识漂移。普通知识内容/卡片集合变化只更新
source fingerprint，旧代中逐条 hash 未变的记录可按 §6 的 partial 规则安全复用；卡片 schema
或预处理协议版本变化同时进入 settings fingerprint，属于硬 stale，不能跨协议复用。

## 6. 不可变 generation 与并发

语义层已使用独立 runtime state,provider 网络与索引构建不持有主 `rt.mu`：

1. 健康 tree/project、journal、flow 或 reconcile 结果变化时递增 source version；按 version 缓存 records map + fingerprint 组成的 immutable source manifest；
2. 稳态 recall 命中同代 manifest 时 O(1) 取得它，不再逐次全库脱敏、截断与哈希；
3. generation 变化后先持可取消的 `rt.mu` 读锁执行零分配全局 shape preflight，再在同一快照内完成有界的 dispute/时态 lineage 解析与 immutable 轻量 DTO 抓取；决策生效图、卡片格式化、脱敏、截断、排序与哈希在主锁外完成，只有 version 未变时才整体发布 manifest。同仓/跨仓等待用 channel gate，锁等待、preflight、DTO 与卡片预处理都定期检查 context；
4. 每仓 `rebuildMu` 串行完整重建；Embedder、Flat build 与文件写入在主锁外进行，不发布半代；
5. 发布前再次核对 source 与仓外 settings fingerprint；任一变化就丢弃晚到结果；
6. 持久 metadata 只存 schema、generation、settings/embedder、document probe、query probe、source fingerprints、dimensions、records 与 built_at，不单列 raw model/revision；
7. OpenAI-compatible rebuild 的初始探测与每个文档 batch 都在**同一 HTTP 请求**携带 document/query 两种 canary；每批最多 30 张文档卡 + 2 个 canary。任一批 canary 与初始 fingerprint 不同，整次重建在原子写前失败，磁盘上一代绝不替换；进程额度容得下旧+新时内存旧代也原样保留，满额时会在 provider 前只逐出本仓可重建内存以守住硬上限，失败后仍可从未动过的磁盘旧代重载；
8. 普通 query cache miss 同样把 query 与 query-mode canary 放在一个请求，cache 同时保存 query vector 与 observed canary fingerprint；并发同 query miss 通过 daemon 进程共享 provider gate + 二次查 cache 折叠，单仓 CLI 则使用同样容量的本地 gate；
9. 内存 `loadedKey=settings+current-source+resident-limit` 一次发布完整 immutable snapshot；Flat 搜索只在取得短期 resident lease 后使用指针，换代/逐出等待租约结束，再解码新矩阵，避免新旧 snapshot 无界叠加。provider 请求前后、租约内搜索前后都核对配置、磁盘 generation 与 current manifest。`partial` 且驻留代仍合法时不会无故逐出或重复解码；观测到 disabled、配置缺失或 corrupt 时立即只清驻留 snapshot/query cache 并归还进程额度，不删除磁盘文件；
10. `serve --repo ...` 在任何 writer lock/listener 前预检 enabled 仓库的 `max_vector_mib` 合计；运行中 hot enable/config/load 仍必须向同一进程 coordinator 动态 reserve。provider 请求全进程最多 1 个、Flat 扫描全进程最多 2 个。重建的新矩阵与已发布旧矩阵分别计费；若 1GiB 已满，则在 provider 前只逐出本仓可重建内存、保留磁盘旧代后再申请，任何路径都不能以换代为由突破总额。source manifest 不混入 vector 额度，但受同一 coordinator 的独立 384MiB 累计上限约束：单次构造保守预留 192MiB，发布后按 map/string/slice 驻留估算促销；disabled/clear/no-index/shutdown 归还，容量不足显式降级而不无界累积。

`mcpserv` 将请求 context 传到 `RecallContext`/`kb_task`/`kb_semantic sync`，并贯穿 Embedder、`Snapshot.SearchDistinctNodesByKindsFiltered` 与 vector codec；CLI rebuild 通过 signal context 响应 SIGINT/SIGTERM。这些是已接通的取消边界，不扩大为“所有 server shutdown 或阻塞 I/O 均可立即取消”。

严格一致性：

- snapshot 只有在 settings/embedder fingerprints、dimensions 和 record metadata 合法时才可载入；provider/model/profile 不匹配是硬 stale，绝不跨向量空间复用；
- source 集合变化进入 `partial`，不是“旧向量可能还差不多”：每条 hit 必须在当前 manifest 中找到相同 record ID/node ID/lane/source hash 才可使用；变化、删除、新增但未入旧代的卡片自然淘汰/等待同步。若当前 source 本身损坏或无法构造，则仍 fail closed；
- `kb_status`/`semantic status` 纯本地给出 `unconfigured / disabled / configured-no-index / ready / partial / stale-source / stale-provider / corrupt`、model/profile/policy/dimensions/records/built_at/next_action。`provider=unchecked` 只表示未联网；冷态不加载大矩阵。ai-* policy 会在本地构造同一份有界 source manifest：若超过 MCP 的 3000 卡上限，`next_action` 直接指向 CLI rebuild，绝不诱导 AI 消耗一次注定失败的会话 sync；
- 启动/首次查询以 fingerprints 验证持久缓存；missing/hard-stale/corrupt/oversize 都不能阻塞主服务。重建可由用户显式 `semantic rebuild`，或在用户已选 ai policy 时由 `kb_semantic sync` 发起；未启用时绝不构造 provider；
- provider 超时、429/5xx、坏向量、取消，以及文件原子 rename **之前**的构建/写入失败会保留磁盘上一代；容量允许时也保留同一内存指针，满额重建为守总预算可能已逐出本仓 resident，但下一次 recall 可有界重载旧文件。rename 已成功后的目录 fsync 或 post-commit 校验失败不承诺回滚，后续仍依靠 binding/checksum 拒绝坏代，并支持 `semantic clear`/`rebuild` 恢复；
- `semantic status` 纯本地显示 enabled、配置路径、model/profile/policy/dimensions 与本地索引状态，不回显 endpoint，绝不探测 provider 或显示 key。冷进程只校验固定 wrapper 和 `≤64 KiB` metadata/binding，并显示 `metadata-valid (payload validation deferred to recall)`；向量 payload 的有界 decode 与完整 checksum 校验延迟到首次需要 semantic 的 recall。

## 7. 默认 Flat 后端与持久化

Flat 后端逐条做精确余弦 Top-K：写入时 L2 归一化，查询时点积。普通 `Search` 用固定容量小根堆；召回用 `SearchDistinctNodesByKindsFiltered` 在一次矩阵扫描中先按当前 manifest 淘汰失效 record，再为每条 lane 维护最多 `top_k` 个节点的 retained heap 与位置表。空间是 `O(lanes × top_k)`，不会随 unique NodeID 数增长；被逐出的节点后遇更强卡片仍可重新入堆，同节点 winner 更新与同分 record ID 顺序保持稳定。这样不会为三条 lane 扫三遍最高 512MiB 矩阵，也不会让一个节点的多卡挤满结果。所有 dimensions、NaN、Inf、零向量、整数乘法和 slice 长度都在分配前校验；当前不做 SIMD/HNSW。

派生缓存路径：

```text
.knowledge/local/vector.idx
```

当前二进制格式是一层 semantic wrapper 包一个 vector codec payload：

```text
wrapper: magic/version/flags + metadata length + metadata checksum
metadata JSON: schema + generation + settings/embedder/probe/source fingerprints
               + query_probe fingerprint
               + dimensions + records + built_at
vector payload: codec header + record table(id/node_id/kind/source_hash)
                + normalized float32 matrix + payload checksum
```

持久 metadata 不存 raw endpoint/model/revision；这些设置只通过 settings/embedder fingerprint 绑定。文件不保存类型卡正文或 API key。必须复用 Store 的 root confinement 与安全原子写，Unix 权限固定 `0600`；先写专用 `.vector.idx-*.tmp`、完整校验和 fsync，再原子替换。rebuild/clear 在 semantic 跨进程锁内只回收该专用前缀的普通文件，遇 symlink/目录 fail closed，不误删其他 `.tmp`。rename 前失败保留旧代；rename 后的目录 fsync/post-commit 失败不做回滚承诺。recall 载入时先验证固定头与长度，再做 checked multiplication 和有界 payload decode，checksum 失败整文件拒绝，不做部分恢复。冷态 `semantic status` 只验 wrapper metadata，不触发这次 payload decode/checksum。缓存永不进入 bundle；`semantic clear`/`rebuild` 始终是受支持的恢复方式。

默认硬上限（可在实现基准后向下收紧，不能由仓内配置抬高）：

| 项目 | 默认/硬上限 |
|---|---|
| configured/actual dimensions | `0(auto)..4096` / `1..4096` |
| records/repo | `100,000` |
| source shape items / generation | `250,000`（节点、entry、journal node/ref、dispute、flow step/convention 等输入合计） |
| source 普通字段 / Node ID / reference | `1 MiB` / `4032 bytes` / `8065 bytes` |
| 单个历史引用的 lineage 候选 | `4096`；超限显式失败，不静默漏 heir |
| source DTO/body / output metadata | 各 `64 MiB` |
| 单条待格式化内容 / 单张原始知识卡 | 各 `1 MiB` |
| rebuild provider 文档 | 加前缀并脱敏后截断为 `4 KiB` |
| provider 通用单条文本接口 | 硬上限 `16 KiB` |
| rebuild batch | 每请求最多 30 张文档卡 + document/query 双 canary（总输入 32）；provider 正文合计硬上限 `512 KiB` |
| 单次 HTTP request/response | `1 MiB` / `16 MiB` |
| source/rebuild 在途代/daemon | 1；从有界 DTO 构造到原子提交全程串行且可取消 |
| cached/building source budget / daemon | `384 MiB`；构造代预留 `192 MiB`，稳态 manifest 按保守驻留估算记账 |
| provider 在途请求/daemon | 1；query/probe 取得 slot 后二次查缓存（单仓 CLI 同为 1） |
| Flat search 在途扫描/daemon | 2；其余请求等待或随 context 取消（单仓 CLI 同为 2） |
| provider request timeout | 默认 `30s`，可配 `1..30s` |
| 一次 rebuild CLI 总时限 | `30min` |
| 一次 MCP sync 总时限/源卡 | `8min` / `3000`（最多 100 个文档 batch；超限零 provider） |
| semantic candidate top_k | 默认 20，硬上限 100 |
| semantic min_score | 默认 `0.35`，范围 `0..1` |
| vector payload / repo | 默认 `512 MiB`，可配 `16..512 MiB` |
| vector record metadata / semantic wrapper metadata | `64 MiB` / `64 KiB`（两层独立上限） |
| loaded/rebuilding vector budget / daemon | `1024 MiB`；启动预检 + 运行时 reserve/release，换代也计入 |

preview 对文档、批次、响应、record、dimensions 和 vector payload 设置有界上限，超限时该 semantic 通道报资源边界并回退 lexical/structural；这是降低资源风险的工程边界，不作“绝不会 OOM”的绝对承诺。preview 不会在启动时主动为所有仓重建。多 repo 已有进程级 coordinator：steady load、hot enable、外部 generation 替换和本进程 rebuild 都共享 1GiB 逻辑矩阵预算，typed source manifest 则共享独立 384MiB 累计预算；disabled/clear/no-index/shutdown 归还对应额度。它们不是 LRU：容量不足会给出可操作错误；重建只有在需要时才先逐出**本仓**可重建内存，磁盘 generation 不删。Go runtime、估算偏差、provider response 等额外开销仍存在，因此这些是逻辑授权上限，不是 RSS/OOM 保证。

## 8. 三通道召回与历史决策提醒

普通 `kb_recall` 的固定顺序如下：

1. **确定性短路**：node ID、精确 symbol/path 及显式 flow/history 路由先走原有解析；足以回答时不调用 Embedder；
2. **一次 semantic 扫描、三条 lane**：普通意图查询运行 lexical/trigram，同时一次 Flat 扫描产出 current/risk/history 三组 distinct-node Top-K；任一 semantic/provider 失败不取消 lexical/structural；
3. **当前答案融合**：只有 current lane 与 lexical 按固定 RRF 融合，不直接相加 lexical score 与 cosine；最终按稳定 node ID 解平；
4. **风险与历史隔离**：risk 显示为“风险警示”，history 显示为“历史来路 [historical]”；二者不进入 current RRF，即使 cosine 更高也不能自动成为当前答案；
5. **逐条当前有效性校验**：每个 semantic hit 对照当前 immutable manifest 的 record ID/node ID/lane/source hash 与节点存在性；partial 状态下也只放行完全一致的旧卡；
6. **展示与结构相邻**：current 候选行展示当前 node status/facets/refs；risk/history 证据行也展示 refs 供精确复核。融合后附有界一跳邻居，包含调用/被调、接口/实现和同流程步骤；
7. **来源可解释**：输出显式列出 keyword rank/score、semantic rank/cosine 和 RRF；cosine 不是 confidence，风险/历史必须沿 refs 精确复核；
8. **裁决边界**：semantic 层不自动执行 lineage/supersedes/confidence/dispute 裁决，不按相似度 refute/obsolete 条目，也不修改任务或代码。

“历史决策提醒”已接到 `kb_task start`：它把 task/intent/plan/todo 聚成脱敏 query。独立
`lexical-risk` 倒排让精确 pitfall/dispute 风险在 semantic 禁用时仍可告警且绝不进入 current；
启用 semantic 后再用 risk/history 卡发现同义改写，并把 touching 节点的直接风险优先显示。全部 touching current heirs 会精确检查；结构一跳按稳定优先级最多检查 100 个节点，超限时明确提示其余结构提醒可能不完整。输出明确标注“仅供参考，不阻断；
相似不等于裁决”，附 node/facets/refs/cosine，帮助 AI 在动手前发现换一种说法的历史否决、
pitfall、dispute 或旧方案。provider/索引不可用时 WIP 仍正常建立，但返回显式“本次提醒可能
不完整”的状态提示；只有调用方取消才在写任务态前传播 cancellation。回执最多展开 20 条 touching 精确提醒与 6 条其他提醒；touching 超出输出预算时必须列出每个被省略的 **current heir ID**，而不是只给一个无法下钻的数量。这项提醒永远不拒绝任务、
不替用户作决定、不把历史自动改写为当前结论。

`kb_diagnose` 仍走已有 pitfall/flow/rejected 反查，没有直接继承这条 semantic candidate
pipeline；它若接入需独立行为评测。`kb_investigate` 等消费者同理，不能因 recall/任务告警已
接入就自动继承。

## 9. 现成方案评估

### chromem-go

优点：纯 Go、运行时零第三方依赖、内嵌、Flat 精确搜索，官方 README 的 2020 年中端笔记本基准约为 2.5 万条 10ms、10 万条 40ms。

问题：项目仍标为 beta，持久化和 provider API 会把本项目绑定到外部格式/生命周期，MPL-2.0 也增加依赖治理成本。

结论：最接近需求，可参考其 Flat 与 benchmark，不形成默认依赖。

参考：[chromem-go](https://github.com/philippgille/chromem-go)

### Eino / eino-ext

优点：组件与 agent 编排完整，provider 生态适合 nbco 一类完整 AI runtime。

问题：它不是一个“只加向量扫描”的存储后端；iknowledge 用不到大部分抽象。

结论：只参考 Embedder/provider 边界，自有标准库 HTTP 实现更符合本项目。

参考：[Eino](https://github.com/cloudwego/eino)、[eino-ext](https://github.com/cloudwego/eino-ext)

### Zvec

优点：Flat、HNSW、IVF、稀疏/稠密、过滤和混合查询能力完整。

问题：Go SDK 通过 cgo 包装 C API，需要原生库与额外发布矩阵。

结论：越过 §10 门槛后才考虑独立 adapter/module 或 build flavor，绝不进入默认根依赖。

参考：[Zvec](https://github.com/alibaba/zvec)、[Zvec Go SDK](https://github.com/zvec-ai/zvec-go)

### Bleve

Bleve 的全文检索成熟，但当前向量路径需要修改版 Faiss 动态库。本项目已有 lexical 索引，单为向量能力引入它仍不能保持默认无原生库。

参考：[Bleve](https://github.com/blevesearch/bleve)

### LanceDB Go

LanceDB 功能完整，但官方 API 页面将 Go 列为 community-contributed SDK；当前 Go 仓库要求下载 native headers/libraries 并设置 CGO flags，列出的 Windows 预编译目标只有 amd64，没有 arm64。

结论：部署复杂度与默认六平台、`CGO_ENABLED=0` 发布目标不匹配。

参考：[LanceDB SDK 支持表](https://docs.lancedb.com/api-reference)、[lancedb-go](https://github.com/lancedb/lancedb-go)

### sqlite-vec

sqlite-vec 官方 Go 文档同时给出 CGO binding 和基于 `ncruces/go-sqlite3` WASM 的 CGO-free 路径，所以“Go 只能走 cgo”并不准确。但后一路仍引入 SQLite/WASM 依赖栈，项目及文档目前明确处于 alpha/work-in-progress；iknowledge 也不需要 SQL 事务来保存可重建向量。

结论：不采用，但理由是依赖与成熟度/需求不匹配，而不是声称不存在 CGO-free 方案。

参考：[sqlite-vec Go bindings](https://alexgarcia.xyz/sqlite-vec/go.html)

## 10. 升级门槛

满足下列任一**真实、连续、可复现**信号才评估 HNSW/Zvec：

- 有效类型卡记录稳定超过 10 万；
- Flat 扫描（不含 query embedding）在目标最低规格机器 P95 持续超过 50ms；
- 受 §7 内存上限约束后，常用仓库无法保持可用 snapshot；
- 出现明确的高并发、复杂元数据过滤或百万级数据需求。

升级后仍必须保持同一 Embedder、record ID/source hash、generation 与 fallback 契约；YAML/journal 仍是正本；默认纯 Go Flat 仍可用。Zvec adapter 单独隔离，不得让没启用语义搜索的用户下载原生库。

## 11. Preview 交付状态与评测门槛

1. **核心契约（已交付）**：canonical-repo 仓外配置、query profile/policy、typed cards、Flat/codec、三 lane distinct-node Top-K、格式/限额/竞态测试；
2. **provider（已交付）**：标准库 Ollama/OpenAI-compatible HTTP，请求 context、脱敏、redirect/坏响应降级，以及 rebuild 双模式同批 canary 和 query+canary；
3. **MCP/Recall（已交付）**：`kb_status` 健康与 `kb_semantic status|sync`、精确短路、current+lexical RRF、risk/history advisory、partial source-hash 复用、结构邻居与来源解释；
4. **历史决策提醒（已交付，辅助能力）**：`kb_task start` 对 risk/history 做相似线索发现，仅供参考、不阻断、不裁决；
5. **离线算法回归基线（已交付）**：`cmd/kbsemeval` 读取版本化 `eval/semantic/v1/qrels.jsonl`，完全离线使用预计算、人工标注的小向量 fixture，验证三 lane Recall@K/MRR、lane precision、distinct-node rate，以及 `expected_hits` 指定的逐 lane 精确获胜记录与稳定顺序。严格默认门要求三 lane 都有 qrels、各 lane Recall@K=1、lane precision=1、distinct-node rate=1 且 `ranking_violations=0`：

```bash
go run ./cmd/kbsemeval --input eval/semantic/v1/qrels.jsonl
```

这条 PASS 只证明**检索算法/通道隔离没有回归**，不是某个真实 embedding 模型已经准确，
更不是默认开启依据。checked-in fixture 只有 4 个手工 case；真实模型质量晋级仍待从真实 recall
miss 固化独立的中英 query/qrels，绝不能从模型输出反向调 qrels。

最低离线评测集为 100 条固定 query，覆盖中文/英文、精确 symbol/path、关键词、同义改写、跨节点语义、负知识/历史、dispute/suspect 和 no-answer 控制组。以当前 lexical+structural 为基线，首版同时满足：

- 精确 symbol/path/history 控制组首条结果与状态标记 100% 不退化；
- 同义改写子集的 Recall@10 至少提升 **10 个绝对百分点**；
- 全集 nDCG@10 与 MRR@10 不低于基线；
- no-answer 误收率不高于基线，retired/refuted 被召回为有效答案的次数为 0；
- provider 缺失、超时、429/5xx、畸形/超限响应时，recall 降级且 lexical/structural 仍可用；调用方/CLI 显式取消则应传播 cancellation，不伪装成普通 miss；
- race、截断/坏 checksum、source/provider 在重建中变化、删除缓存重建测试全部通过；多 repo 总常驻另作晋级资源评测；
- Darwin/Linux/Windows × amd64/arm64 的 `CGO_ENABLED=0` 构建继续通过；
- 在 1 万、2.5 万、10 万记录上记录构建时间、文件/常驻大小、P50/P95；25k Flat scan 的目标 P95 ≤ 50ms。

现有离线算法 qrels 已让“向量排序/分 lane/distinct/稳定获胜记录”可回归；“相似只发现证据、
不越权裁决”则由 engine 的 recall 渲染与任务决策提醒测试守住，不伪装成向量指标。两者都
尚未满足上述 100 条真实模型与 lexical 基线对照，所以仍只算 preview，不算模型质量晋级完成。
通过这些门槛也**不会**把 local 或 remote 设为默认；enabled 永远是每仓本机用户的显式选择。

## 12. 最终决策

已交付 preview 采用项目内 `internal/vector` 的纯 Go Flat snapshot，以及 `internal/semantic` 的自有薄 Embedder + 标准库 OpenAI-compatible/Ollama HTTP；Eino、chromem-go 只作参考，Zvec 保留为规模增长后的隔离 adapter。

向量只补“候选发现”，不改变真相来源。当前实现把 current 与 lexical 做 RRF，把 risk/history
隔离为可解释 advisory，并用 source hash 在 partial 代安全复用未变化卡片；任务开始时的历史
决策提醒也只作辅助。用户未通过仓外 CLI 显式 configure/enable 时，仓库内容永远不能开启外发；
批量类型卡只在用户 CLI rebuild 或其预先授权的 MCP sync 中发送。任何 provider/模型硬不一致
都拒用 semantic，普通故障回到已经可用的 lexical/structural 路径。离线算法基线已交付，真实
模型质量晋级仍未完成，因此 preview 继续默认禁用。
