# iknowledge 可选语义检索实施契约

> 状态：可选 preview 已实现，默认禁用；离线质量晋级门尚待完成，不是默认检索路径。
>
> 日期：2026-07-19。
>
> 本文记录已实现边界与后续晋级门。YAML/journal 仍是知识正本，现有精确、关键词、trigram 与调用图检索仍是默认路径和故障回退。

## 1. 决策摘要

iknowledge 需要的是一个受控的“第三候选发现通道”，不是一套通用代码 RAG，也不是一个独立向量数据库服务。

定案如下：

1. 默认 `disabled`，不加载 provider、不发网络请求，现有行为不变；
2. 首版在项目内实现纯 Go、精确 Flat 向量索引，只索引知识摘要，不切源码；
3. 用户可显式选择本机 Ollama 或远程 HTTPS OpenAI-compatible embedding；两者都是 iknowledge 内部直连的 HTTP provider，**不是第三方 MCP server**；
4. provider 的 endpoint/model/dimensions/revision/enabled 与有界检索/资源参数全部保存在按 canonical repo 隔离的**仓外用户私有状态**；`.knowledge/config.yaml` 不承载这些字段；
5. API key 只从固定环境变量 `IKNOWLEDGE_EMBEDDING_API_KEY` 读取，命令行、仓内文件、缓存和日志都不保存 key；
6. 查询顺序为“精确短路 → lexical 与 semantic 候选 → RRF 融合 → 当前 record source hash/节点存在性校验 → 既有结构邻居一跳扩展与来源 rank 解释”；结构邻居包含调用/被调、接口/实现和同流程步骤；向量分数不能单独决定答案；
7. 向量缓存是由 settings/embedder/probe/source fingerprints 共同绑定的不可变派生 generation，任何不匹配都只会降级到现有检索，绝不使用旧向量凑结果；
8. Eino/eino-ext 只作接口与 provider 实现参考，当前不是依赖；Zvec 只在真实规模越过门槛后作为可选 adapter。

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
- 首版不接入 `kb_diagnose`：它当前直接检索 active pitfall/全部条目，仅索引 summary 会造成不对称召回；
- 首版不实现 HNSW、分布式索引、多租户服务或百万级数据；
- 不内置 embedding 模型，也不要求 Docker、Python、SQLite、Faiss 或常驻向量数据库；
- 当前不依赖 Eino/eino-ext、chromem-go 或 Zvec。

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
  --dimensions 0

iknowledge semantic configure --repo <repo> \
  --endpoint https://api.example.com --model <model> \
  --dimensions <n> --revision <immutable-model-revision>

iknowledge semantic enable  --repo <repo>
iknowledge semantic disable --repo <repo>
iknowledge semantic status  --repo <repo>
iknowledge semantic rebuild --repo <repo>
iknowledge semantic clear   --repo <repo>
```

`configure` 只接受非 secret 的 provider 元数据,保存后同时 enable,但**不联网、不自动重建**;
`enable` 只用于重新启用既有配置。用户随后显式执行 `semantic rebuild`,才向固定 provider
批量发送脱敏摘要;有效索引在 recall 时只发送脱敏查询。serve 启动与 `semantic status`
只读本地状态,不能探测 provider;stale 时提示 rebuild 并 lexical 降级。`disable` 阻止新请求;
`clear` 只删除可重建索引并保留 provider 配置。

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
top_k, min_score, max_vector_mib, timeout_seconds
```

后四项只是有硬上限的本机检索/资源参数，不参与外发目的授权：默认分别为 `20`、`0.35`、
`512`、`30`。它们与 provider 元数据一样只能由 semantic CLI 写仓外状态，仓内不能抬高。

settings/embedder fingerprint 覆盖 normalized endpoint、model、dimensions、revision 与
预处理/协议版本,不包含 API key;任一向量语义变化都会让旧索引 stale。revision 可留空,
但显式填写模型 snapshot/tag 更利于审计。运行时另以固定 probe 向量 fingerprint 检测“同名
模型在服务端漂移”,不匹配时拒用旧索引并要求 rebuild。

API key 固定只读 `IKNOWLEDGE_EMBEDDING_API_KEY`；不支持 `api_key` 或可由仓库选择的 `api_key_env` 字段。Ollama 模式不读取该变量。

关键安全不变量：

- `.knowledge/config.yaml`、YAML/journal、bundle、MCP 参数与仓内脚本都不能写 provider 配置或把 `enabled` 改为 true；
- `init`、`import`、`setup`、`serve` 启动和读取旧 `vector.idx` 都不能隐式 enable；
- 在仓外私有状态未启用时，仓内任何内容单独都不能触发 embedding 外发；
- 启用后，仓内内容也不能改变目的 endpoint、模型、revision、请求类型或凭据来源，只能进入已授权且先脱敏的“摘要/查询”载荷；
- canonical repo 路径变化会落到新的私有状态分区，默认重新回到 disabled；
- 不再需要单独的 trust marker：仓外 `configure` + `enable` 状态本身就是本机用户授权，仓内没有可被信任的对应配置。

### 3.3 HTTP 与数据外发边界

- 远程发送前复用现有 secret redaction；hash/fingerprint 也以脱敏后的文本为准；
- 请求 context 已贯穿 `RecallContext`、Embedder、`Snapshot.Search` 与 vector codec；`semantic rebuild` CLI 使用信号 context 响应 SIGINT/SIGTERM。这是已接通边界的协作式取消，不声称任意阻塞 I/O 或所有 server shutdown 路径都能立即中断；
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

// vector.Build/DecodeWithLimits 一次产生完整 *vector.Snapshot；
// (*Snapshot).Search(ctx, query, limit) 只读，不暴露可变 record/vector slice。
```

查询与文档分开是刻意契约：有些模型会为两类输入加不同 instruction，不能在 adapter 内静默混用。`engine` 负责读取仓外设置、从当前知识 model 生成 `vector.Record`、调用 `semantic.Embedder`、做 RRF 并校验 manifest record hash/节点存在性；`semantic` 只处理已脱敏的 text→vector HTTP 边界，`vector` 只处理向量、不可变快照与格式校验。首版只做整代 `Build/Replace`，不暴露可被并发读到半状态的 `Upsert/Delete`。

## 5. 真实索引记录

当前模型没有 `Node.Summary` 字段。可索引来源严格限定为健康 `index` 快照中的：

1. 每个 `Node.Entries` 中 `Active() && Kind == summary` 的条目；
2. 非空的 `Node.EraSummary`。

当前记录为：

```text
record_id    summary:<node-id>#<entry-id>  或 era:<node-id>
node_id      当前节点 ID
kind         summary | era_summary
source_hash  SHA-256(preprocess-version + canonical node/kind prefix + redacted text)
vector       L2-normalized float32[]
```

规则：

- `superseded`、`refuted`、`retired` 条目和 `orphaned` 节点不进入新快照；候选呈现时读取当前 `Node.Status`，但当前融合不按 status 降权；
- 不索引原始源码、contract/mutation/pitfall/usage 全文、findings、WIP、会话台账、使用日志或模板空文；
- 直接 pitfall 仍由现有 lexical 索引或精确节点下钻发现；混合候选列表本身不实施负知识优先排序；
- 记录只能从 engine 已隔离 schema-too-new、分片冲突、重复 Node ID 等坏数据后的健康 snapshot 构造，不能重新遍历原始 shard 猜“谁是真”；
- 混合时用 `record_id` 对照**当前 immutable source manifest**，只接受 node ID/source hash 一致且节点仍存在的 hit；缓存不保存摘要正文，也不直接成为答案；
- 当前候选融合不做逐条 lineage/supersedes/confidence/dispute 校验；需要条目与历史详情时，用候选列表提示的节点 ID 精确重查。

实际送入 provider 的文档必须采用有版本的确定性预处理（至少含 canonical node ID、kind
与脱敏摘要），`source_hash` 覆盖这份**最终文本**而不是仅覆盖原摘要。源集合指纹定义为：
对全部记录的 `(record_id, node_id, kind, source_hash)` 按字节序排序、长度前缀编码后取
SHA-256。这样 map 遍历顺序、平台路径分隔符和浮动时间不会让相同知识产生不同指纹；
预处理规则一变也会自然判 stale。

## 6. 不可变 generation 与并发

语义层已使用独立 runtime state,provider 网络与索引构建不持有主 `rt.mu`：

1. 健康 tree/project 视图变化时递增 source version；按 version 缓存 records map + fingerprint 组成的 immutable source manifest；
2. 稳态 recall 命中同代 manifest 时 O(1) 取得它，不再逐次全库脱敏、截断与哈希；
3. generation 变化后只在 `rt.mu` 内复制 immutable string headers，脱敏、截断、排序与哈希均在主锁外重建；只有 version 未变时才整体发布 manifest；
4. 每仓 `rebuildMu` 串行完整重建；Embedder、Flat build 与文件写入在主锁外进行，不发布半代；
5. 发布前再次核对 source 与仓外 settings fingerprint；任一变化就丢弃晚到结果；
6. 持久 metadata 只存 schema、generation、settings/embedder/probe/source fingerprints、dimensions、records 与 built_at，不单列 raw model/revision；
7. 内存 `loadedKey=settings+source` 一次替换完整 immutable snapshot；搜索持有旧 Go 指针也只读安全；provider 请求后再次核对 source fingerprint。

`mcpserv` 将请求 context 传到 `RecallContext`，并贯穿 Embedder、`Snapshot.Search` 与 vector codec；CLI rebuild 通过 signal context 响应 SIGINT/SIGTERM。这些是已接通的取消边界，不扩大为“所有 server shutdown 或阻塞 I/O 均可立即取消”。

严格一致性：

- snapshot 只有在 settings/embedder/probe/source fingerprint、dimensions 和 record metadata 全部匹配时才可查询；
- source 一变，旧 snapshot 立即从可用态转为 stale，重建完成前只走 lexical/structural；**不允许“旧向量可能还差不多”**；
- 启动/首次查询以 fingerprints 验证持久缓存；missing/stale/corrupt/oversize 都不能阻塞主服务。重建只由显式 `semantic rebuild` 发起，未启用时绝不构造 provider；
- provider 超时、429/5xx、坏向量、取消，以及文件原子 rename **之前**的构建/写入失败会保留上一代；rename 已成功后的目录 fsync 或 post-commit 校验失败不承诺回滚，后续仍依靠 binding/checksum 拒绝坏代，并支持 `semantic clear`/`rebuild` 恢复；
- `semantic status` 纯本地显示 enabled、配置路径、endpoint/model/dimensions 与本地索引状态，绝不探测 provider 或显示 key。冷进程只校验固定 wrapper 和 `≤64 KiB` metadata/binding，并显示 `metadata-valid (payload validation deferred to recall)`；向量 payload 的有界 decode 与完整 checksum 校验延迟到首次需要 semantic 的 recall。

## 7. 默认 Flat 后端与持久化

Flat 后端逐条做精确余弦 Top-K：写入时 L2 归一化，查询时点积，用固定容量小根堆避免全量排序。所有 dimensions、NaN、Inf、零向量、整数乘法和 slice 长度都在分配前校验；小数据量串行，大数据量是否并行由基准决定，首版不做 SIMD/HNSW。

派生缓存路径：

```text
.knowledge/local/vector.idx
```

当前二进制格式是一层 semantic wrapper 包一个 vector codec payload：

```text
wrapper: magic/version/flags + metadata length + metadata checksum
metadata JSON: schema + generation + settings/embedder/probe/source fingerprints
               + dimensions + records + built_at
vector payload: codec header + record table(id/node_id/kind/source_hash)
                + normalized float32 matrix + payload checksum
```

持久 metadata 不存 raw endpoint/model/revision；这些设置只通过 settings/embedder fingerprint 绑定。文件不保存摘要正文或 API key。必须复用 Store 的 root confinement 与安全原子写，Unix 权限固定 `0600`；先写临时文件、完整校验和 fsync，再原子替换。rename 前失败保留旧代；rename 后的目录 fsync/post-commit 失败不做回滚承诺。recall 载入时先验证固定头与长度，再做 checked multiplication 和有界 payload decode，checksum 失败整文件拒绝，不做部分恢复。冷态 `semantic status` 只验 wrapper metadata，不触发这次 payload decode/checksum。缓存永不进入 bundle；`semantic clear`/`rebuild` 始终是受支持的恢复方式。

默认硬上限（可在实现基准后向下收紧，不能由仓内配置抬高）：

| 项目 | 默认/硬上限 |
|---|---|
| configured/actual dimensions | `0(auto)..4096` / `1..4096` |
| records/repo | `100,000` |
| rebuild provider 文档 | 加前缀并脱敏后截断为 `4 KiB` |
| provider 通用单条文本接口 | 硬上限 `16 KiB` |
| rebuild batch | 32 条；provider 正文合计硬上限 `512 KiB` |
| 单次 HTTP request/response | `1 MiB` / `16 MiB` |
| provider 在途请求/仓 | 1；query/probe 取得 slot 后二次查缓存 |
| provider request timeout | 默认 `30s`，可配 `1..30s` |
| 一次 rebuild CLI 总时限 | `30min` |
| semantic candidate top_k | 默认 20，硬上限 100 |
| semantic min_score | 默认 `0.35`，范围 `0..1` |
| vector payload / repo | 默认 `512 MiB`，可配 `16..512 MiB` |

preview 对文档、批次、响应、record、dimensions 和 vector payload 设置有界上限，超限时该 semantic 通道报资源边界并回退 lexical/structural；这是降低资源风险的工程边界，不作“绝不会 OOM”的绝对承诺。preview 不会在启动时主动为所有仓重建。多 repo 的进程级全局 LRU/
resident coordinator 尚未进入 preview;部署多个大索引时应把各仓 `max_vector_mib` 配低于硬上限,
并把“进程总常驻预算”作为晋级前的资源评测项,不能在文档里假装已有全局 1GiB 闸门。

## 8. 混合检索契约

首版只接入 `kb_recall`。固定顺序如下：

1. **确定性短路**：node ID、精确 symbol/path 及现有显式 flow/history 路由先走原有解析；足以回答时不调用 Embedder；
2. **独立候选池**：普通意图查询分别运行现有 token/trigram 排名与 semantic Top-K；任一通道失败不取消另一通道；
3. **无量纲融合**：对去重后的 `node_id` 用固定参数的 RRF 融合，不直接相加 lexical score 与 cosine；候选池和最终数量都有上限，平分按稳定 node ID 排序；
4. **当前有效性校验**：semantic hit 以 record ID 对照当前 immutable source manifest，校验 node ID/source hash 且确认当前 index 仍存在该节点；
5. **展示与结构相邻**：候选行展示当前 node status，融合后调用既有 `structuralNeighborsLocked` 附上有界的一跳邻居，包含调用/被调、接口/实现和同流程步骤；
6. **来源可解释**：输出显式列出 keyword rank/score、semantic rank/cosine 和 RRF；cosine 不是 confidence；
7. **当前未实现的融合规则**：不做逐条 lineage/supersedes/confidence/dispute 校验、状态降权、负知识优先排序，也没有独立的 flow 向量通道/candidate pool；上一步的“同流程步骤”只是 RRF 之后复用既有结构邻居。需要条目详情时，按候选给出的节点 ID 精确下钻；现有快照/history 呈现仍会显示既有负知识和历史。

`kb_diagnose` 继续用 active pitfall/全条目的现有检索。只有后续为 pitfall 形成稳定、可追溯的摘要记录并通过独立评测，才允许接入语义通道；`kb_investigate` 等消费者也必须分别验收，不能因为 Recall 已接入就自动继承。

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

- 有效摘要记录稳定超过 10 万；
- Flat 扫描（不含 query embedding）在目标最低规格机器 P95 持续超过 50ms；
- 受 §7 内存上限约束后，常用仓库无法保持可用 snapshot；
- 出现明确的高并发、复杂元数据过滤或百万级数据需求。

升级后仍必须保持同一 Embedder、record ID/source hash、generation 与 fallback 契约；YAML/journal 仍是正本；默认纯 Go Flat 仍可用。Zvec adapter 单独隔离，不得让没启用语义搜索的用户下载原生库。

## 11. Preview 交付状态与评测门槛

1. **核心契约（已交付）**：仓外 configure/enable、fake Embedder、真实摘要 records、Flat/codec、格式/限额/竞态测试；
2. **provider（已交付）**：标准库 Ollama/OpenAI-compatible HTTP，请求 context 贯穿 Embedder，并含脱敏、redirect、坏响应与降级；
3. **Recall preview（已交付）**：仅接 `kb_recall`，默认 disabled，精确短路 + lexical/semantic RRF + 当前 manifest record hash/节点存在性校验 + 既有结构邻居一跳扩展 + 来源 rank 解释；
4. **观测/质量裁决（待完成）**：从真实 recall miss 固化中英 query/qrels，过门后才考虑其他消费者、默认策略或 ANN。

最低离线评测集为 100 条固定 query，覆盖中文/英文、精确 symbol/path、关键词、同义改写、跨节点语义、负知识/历史、dispute/suspect 和 no-answer 控制组。以当前 lexical+structural 为基线，首版同时满足：

- 精确 symbol/path/history 控制组首条结果与状态标记 100% 不退化；
- 同义改写子集的 Recall@10 至少提升 **10 个绝对百分点**；
- 全集 nDCG@10 与 MRR@10 不低于基线；
- no-answer 误收率不高于基线，retired/refuted 被召回为有效答案的次数为 0；
- provider 缺失、超时、429/5xx、畸形/超限响应时，recall 降级且 lexical/structural 仍可用；调用方/CLI 显式取消则应传播 cancellation，不伪装成普通 miss；
- race、截断/坏 checksum、source/provider 在重建中变化、删除缓存重建测试全部通过；多 repo 总常驻另作晋级资源评测；
- Darwin/Linux/Windows × amd64/arm64 的 `CGO_ENABLED=0` 构建继续通过；
- 在 1 万、2.5 万、10 万记录上记录构建时间、文件/常驻大小、P50/P95；25k Flat scan 的目标 P95 ≤ 50ms。

没有 qrels 与基线对照，只验证“能返回相似向量”，只算 preview,不算质量晋级完成。通过这些门槛也**不会**把 remote 设为默认；enabled 永远是每仓本机用户的显式选择。

## 12. 最终决策

已交付 preview 采用项目内 `internal/vector` 的纯 Go Flat snapshot，以及 `internal/semantic` 的自有薄 Embedder + 标准库 OpenAI-compatible/Ollama HTTP；Eino、chromem-go 只作参考，Zvec 保留为规模增长后的隔离 adapter。

向量只补“候选发现”，不改变真相来源。当前混合列表只做 RRF、record hash/节点存在性校验、status 展示、既有结构邻居一跳扩展和来源 rank 解释；负知识与历史仍由精确节点下钻呈现。用户未通过仓外 CLI 显式 configure/enable 时，仓库内容永远不能开启外发；批量摘要只在显式 rebuild 时发送。任何失败或不一致都回到已经可用的 lexical/structural 路径。
