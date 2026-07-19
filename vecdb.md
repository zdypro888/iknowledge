# iknowledge 向量检索设想

> 状态：设计定案，尚未实现。
>
> 日期：2026-07-19。
>
> 本文只定义可选语义检索层；现有 YAML/journal、关键词索引与调用图仍是系统正本和默认路径。

## 1. 结论

iknowledge 有必要提供语义检索，但当前没有必要引入一个通用、独立运行的向量数据库。

默认方案定为：

1. 在项目内实现一个轻量、纯 Go 的 Flat 向量索引；
2. 仅索引知识摘要，不索引源码；
3. 索引只存放在 `.knowledge/local/`，是可删除、可重建的派生缓存；
4. 搜索采用“精确/关键词 + 结构关系 + 向量语义”的混合召回；
5. embedding 明确选择后才启用，默认关闭，不静默上传知识；
6. 当真实规模和基准证明 Flat 不够时，再提供 Zvec 等可选高性能后端，不能让它破坏默认单二进制发布。

换句话说：需要的是向量检索能力，而不是为了使用向量数据库而引入向量数据库。

## 2. 为什么现在不直接接 Zvec

Zvec 的能力完整，支持 Flat、HNSW、IVF、稀疏向量、全文检索、过滤和混合查询，适合大规模数据。但它的 Go SDK 通过 cgo 包装 C API，需要平台原生库和动态库处理。

这与 iknowledge 当前约束冲突：

- 发布矩阵是 Darwin/Linux/Windows × amd64/arm64；
- 默认使用 `CGO_ENABLED=0` 交叉编译；
- 安装体验是一份二进制，不要求编译器、Python、Docker 或常驻数据库；
- 当前知识规模通常是数千至数万节点，而不是百万级向量。

因此 Zvec 可以成为未来的可选加速后端，但不应成为默认依赖。

参考：

- [Zvec](https://github.com/alibaba/zvec)
- [Zvec Go SDK](https://github.com/zvec-ai/zvec-go)

## 3. 目标与非目标

### 3.1 目标

- 用户用自然语言描述问题时，能找回措辞不同但语义相关的知识摘要；
- 跨节点、跨目录全局召回相关设计、约束、坑点和历史背景；
- 保留精确标识符、路径、关键词和调用关系的确定性优势；
- 不改变知识正本，不把向量索引提交到 Git；
- 支持远程 embedding 和本地 embedding，用户明确选择；
- 保持默认构建纯 Go、无 cgo、无外部服务；
- 后端和 embedding 模型可以替换，不把数据格式绑死在某个厂商上。

### 3.2 非目标

- 不对整个源码仓库切块做通用代码 RAG；
- 不用语义相似代替调用图、流程关系和精确节点查询；
- 不让向量结果绕过 suspect、dispute、retired 等可信度状态；
- 不在首版实现百万级 ANN、分布式索引或多租户服务；
- 不把 embedding 模型强塞进 iknowledge 二进制；
- 不保证不同模型生成的向量可以混用。

## 4. 为什么 Flat 索引足够作为默认实现

Flat 索引会逐条计算余弦相似度，返回精确 Top-K。它没有 HNSW 的建图、调参、召回损失和持久化复杂度。

这与 iknowledge 的数据特点匹配：

- 向量只覆盖摘要，数量远小于源码 chunk 数量；
- 已有大型仓库实测约 1.5 万知识节点；
- 搜索主要由人或 agent 交互触发，不是高 QPS 在线推荐系统；
- 索引损坏可以由正本重建，不需要数据库级事务恢复；
- embedding 计算或网络请求通常比数万条向量扫描更慢。

作为量级参考，chromem-go 官方基准在一台 2020 年中端笔记本 CPU 上，2.5 万条向量精确查询约 10ms，10 万条约 40ms。这个结果不能直接当作本项目性能承诺，但足以说明应先测量，再决定是否引入 ANN。

参考：[chromem-go](https://github.com/philippgille/chromem-go)

## 5. 总体架构

```text
用户查询
   │
   ├─ 精确节点 / 路径 / 关键词召回 ─┐
   ├─ 向量语义召回（可选）──────────┼─ 候选融合与可信度校正 ─ 结构扩展 ─ 结果
   └─ 现有调用图 / 流程关系 ────────┘

知识正本（YAML/journal）
   │
   └─ 摘要快照 ─ Embedder ─ VectorIndex ─ `.knowledge/local/vector.idx`
```

建议先稳定两个内部接口：

```go
type Embedder interface {
	ModelID() string
	Dimensions() int
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

type VectorIndex interface {
	Rebuild(records []VectorRecord) error
	Upsert(records []VectorRecord) error
	Delete(ids []string) error
	Search(query []float32, limit int) ([]VectorHit, error)
	Status() VectorIndexStatus
}
```

调用方只能依赖接口，不能直接依赖 chromem-go、Zvec 或其他后端类型。

## 6. 索引什么

首版只索引摘要型内容：

- `Node.Summary`；
- 已存在的时代摘要；
- 后续若要覆盖详细 entry，先由维护流程生成稳定摘要，再进入索引。

明确不索引：

- 原始源码全文；
- secret 脱敏前文本；
- 已 retired 的知识；
- 空摘要或仅包含模板文字的内容；
- findings、会话台账等短期本地状态。

每条向量记录至少包含：

```text
record_id      稳定记录 ID
node_id        对应知识节点
kind           summary / era_summary
source_hash    摘要源内容哈希
vector         归一化后的 float32 向量
```

命中向量记录后，必须回到当前知识索引按 `node_id` 取数据并重新检查状态。向量缓存中的旧文本不能直接作为答案。

## 7. 默认 Flat 后端

默认后端放在项目内部，保持纯 Go：

- 向量在写入时做 L2 归一化；
- 查询时使用点积得到余弦相似度；
- 使用固定容量 Top-K 小根堆，避免全量排序；
- 数据量足够大时按 CPU 数分片扫描，但小数据量避免并行开销；
- 所有维度、NaN、Inf、零向量在写入边界校验；
- 相同模型和维度的向量才能进入同一索引；
- 结果分数只用于同一模型内排序，不宣称跨模型可比。

首版不实现 SIMD 和 HNSW。只有基准证明它们能改善真实端到端延迟时才增加复杂度。

## 8. 持久化格式

建议使用一个带版本头的二进制文件：

```text
.knowledge/local/vector.idx
```

文件头至少包含：

- magic；
- schema version；
- embedding provider/model ID；
- dimensions；
- normalization 标记；
- 源摘要集合指纹；
- record count；
- payload checksum。

行为约束：

- 模型、维度、格式版本或源指纹不匹配时自动判 stale；
- checksum 错误时放弃旧索引并重建，不尝试带病读取；
- 重建写临时文件，校验并 fsync 后原子替换；
- 缓存文件永不进入 bundle，也不提交 Git；
- 没有可用 Embedder 时保留关键词/结构搜索，不让主功能失败；
- 删除 `.knowledge/local/vector.idx` 必须是安全、受支持的恢复方式。

## 9. Embedding 提供方

向量数据库不负责理解文本，真正决定检索质量的是 embedding 模型。因此存储层和模型层必须解耦。

建议支持：

1. OpenAI-compatible HTTP 接口；
2. 本地 Ollama embedding 接口；
3. 测试用 deterministic fake embedder；
4. 后续按真实需求添加其他 provider。

安全约束：

- semantic search 默认关闭；
- 远程 provider 必须由用户显式配置；
- API key 只从环境变量或受保护的本地凭据读取，绝不写入知识库；
- 发往远程 provider 的内容必须先经过现有秘密脱敏；
- 状态命令应明确显示 provider、model、索引是否 stale，但不显示凭据；
- provider 不可用时返回清晰的降级原因，关键词搜索继续工作。

配置形态仅作示意，最终应沿用项目现有配置风格：

```yaml
semantic_search:
  enabled: true
  provider: ollama
  endpoint: http://127.0.0.1:11434
  model: nomic-embed-text
  top_k: 20
```

## 10. 混合检索与排序

纯向量搜索会把“语义相似但逻辑无关”的内容排到前面，因此不能单独决定最终结果。

建议规则：

1. 精确 node ID、符号名和路径命中拥有最高优先级；
2. 关键词/trigram 和向量检索分别产生候选及各自排名；
3. 使用 RRF 一类对分数尺度不敏感的方法融合排名；
4. 对候选执行现有调用边、流程关系的一跳结构扩展；
5. stale、suspect、dispute-open 等状态继续按现有规则提示或降权；
6. retired 内容不能因向量相似而复活；
7. 返回结果标注命中来源：`exact`、`lexical`、`semantic`、`structural`；
8. 向量低置信命中不得盖过历史否决、雷区和明确冲突。

融合参数必须通过固定评测集标定，不能凭主观感觉不断改权重。

## 11. 现成方案评估

### chromem-go

优点：纯 Go、零第三方依赖、内嵌、支持可选持久化及多种 embedding provider，接入最简单。

问题：仍标记为 beta；当前只做 Flat 精确搜索；持久化以 gob 和逐文档文件为主；许可证是 MPL-2.0；最近正式 tag 较旧。

结论：适合作为实现参考或快速原型，不作为默认长期依赖。项目内置的薄 Flat 层能保留相同核心能力，同时减少许可证、API 漂移和存储格式绑定。

### Zvec

优点：索引类型和混合检索能力最完整，适合十万至百万级数据和更严格性能要求。

问题：Go SDK 需要 cgo、C 编译器或预编译原生库；平台发布、动态库定位和交叉编译复杂。

结论：未来可提供独立 build flavor 或外部后端，不能进入默认纯 Go 二进制。

### Bleve

优点：Go 全文检索成熟，支持 BM25、CJK、向量和混合排序。

问题：当前向量搜索依赖 Faiss 动态库，因此不能解决无 cgo 发布问题。

结论：本项目已有关键词索引，单为向量能力引入 Bleve 得不偿失。

参考：[Bleve](https://github.com/blevesearch/bleve)

### LanceDB Go

优点：向量、过滤、全文及持久化能力完整。

问题：Go SDK 需要下载 Rust 原生库并设置 CGO，Windows arm64 等目标也受限制。

结论：部署复杂度与 Zvec 同类，没有更方便。

参考：[LanceDB Go](https://github.com/lancedb/lancedb-go)

### sqlite-vec 类方案

主流 SQLite 向量扩展通常包含原生扩展和 cgo；少数 pure-Go 实现仍较新，ANN 部分处于实验阶段，而且会引入完整 SQLite 依赖树。

结论：单文件数据库的表面便利不足以抵消成熟度和依赖成本，暂不采用。

## 12. 后端升级门槛

满足下列任一真实信号，才考虑 HNSW/Zvec：

- 有效摘要向量稳定超过 10 万；
- Flat 搜索在目标机器上的 P95 持续超过 50ms；
- 向量扫描的 CPU 或内存已成为可观测瓶颈；
- 有高并发、复杂元数据过滤或百万级数据的明确用户需求。

升级时仍保持：

- YAML/journal 是正本；
- 相同 `Embedder` 和 `VectorIndex` 接口；
- 默认纯 Go Flat 后端继续可用；
- 高性能后端是显式选择，而不是安装时的隐式负担。

## 13. 验收条件

实现向量检索时，至少同时完成以下验证：

- deterministic fake embedder 的单元测试；
- 模型/维度不匹配、坏 checksum、截断文件的重建测试；
- Upsert/Delete/Rebuild/Search 的竞态测试；
- 六个平台保持 `CGO_ENABLED=0` 构建通过；
- 无 provider、provider 超时和 provider 返回坏向量时正确降级；
- secret 不进入远程请求日志、索引元数据和错误文本；
- 固定中文/英文查询集对比 lexical-only 与 hybrid 的 Recall@K；
- 精确符号、路径、历史否决不被语义结果挤掉；
- 1 万、2.5 万、10 万摘要的构建时间、索引大小、P50/P95 延迟基准；
- 删除派生索引后能从知识正本完整重建。

没有检索质量评测，只测“能返回相似向量”，不算功能完成。

## 14. 最终决策

iknowledge 的默认向量能力采用项目内置、纯 Go、精确 Flat 索引；embedding 与索引后端通过接口解耦；向量只增强摘要召回，并与现有关键词及结构检索融合。

chromem-go 是最接近需求的现成方案，但当前不值得形成长期依赖。Zvec 保留为规模增长后的可选加速器。是否升级由真实规模和端到端基准决定，而不是由“向量数据库功能更多”决定。
