// Package model 是知识库的纯数据层(impl §3 定稿)。
// 铁律:本包不依赖任何内部包(impl §2 依赖方向)。
package model

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// SchemaVersion 是分片文件的 schema 版本,按【文件】记(每个分片自带版本),首版为 1。
// journal 行【不带】版本:JSONL 无处放文件头(union 合并会把头撕坏/重复),
// journal 行只做增量字段演化、永不破坏性改版(impl §3)。
//
// 版本演进规则(定案):加字段不升号(读写往返保留未知字段);破坏性改动才升号,
// 新版二进制向后读 N-1 版,迁移只在该文件本就要被写入时顺带就地升级;
// 读到更高版本 => 该文件只读隔离并报 KB_ERR:SCHEMA_TOO_NEW(粒度=单文件)。
const SchemaVersion = 1

// Status 是节点状态(impl §3 定稿:去掉 stale/refuted——stale 无产生路径,
// refuted 是条目级概念,见 Confidence)。
type Status string

const (
	StatusFresh      Status = "fresh"
	StatusSuspect    Status = "suspect"    // 锚点哈希失配,待重验
	StatusOrphaned   Status = "orphaned"   // 锚定符号已消失,待认领/送葬(kb_adopt 二期)
	StatusUndigested Status = "undigested" // 仅有骨架,knowledge 为空(允许空洞的塔)
)

// Confidence 是条目可信度分级(knowledge.md §8.3)。
type Confidence string

const (
	ConfidenceDerived  Confidence = "derived"  // 机器从 AST/LSP 推导,恒真
	ConfidenceVerified Confidence = "verified" // 有验证依据(测试通过/人工确认)
	ConfidenceInferred Confidence = "inferred" // AI 读码推断,未经独立验证
	ConfidenceSuspect  Confidence = "suspect"  // 待重验(锚失配或依据被勘误级联)
	ConfidenceRefuted  Confidence = "refuted"  // 已被证据驳倒,作废但保留
)

// Level 是树节点层级(impl §3 定稿)。flow/topic 是独立的横向对象,不共用树节点 level。
const (
	LevelProject  = "project"
	LevelDir      = "dir"
	LevelFile     = "file"
	LevelFunction = "function"
	LevelDecl     = "decl" // type/var/const,与 function 同层
	LevelStmt     = "stmt" // 一期不产出,枚举保留稳 schema
)

// Entry.Kind 的合法值(knowledge.md §8.1 knowledge 块的五类)。
const (
	KindSummary  = "summary"
	KindContract = "contract"
	KindMutation = "mutation"
	KindPitfall  = "pitfall"
	KindUsage    = "usage"
)

// Anchor 把节点钉在源码上(impl §3)。
type Anchor struct {
	File       string `yaml:"file"`                  // repo 相对路径,一律正斜杠
	Symbol     string `yaml:"symbol,omitempty"`      // 符号规范名(文法见 impl §3);文件/目录节点为空
	Hash       string `yaml:"hash,omitempty"`        // 锚定/腐烂检测用,gofmt 免疫(impl §5 双哈希)
	StructHash string `yaml:"struct_hash,omitempty"` // 结构哈希:改名/注释免疫,仅供迁移匹配(impl §6)
	Lines      [2]int `yaml:"lines,omitempty,flow"`  // 仅展示用,不作锚定依据
}

// Entry 是一条经验知识(impl §3)。
type Entry struct {
	// ID = "e_" + 8 hex(crypto/rand)。与内容无关的稳定 ID——内容短哈希会在
	// 条目被编辑/合并时漂移,basedOn/RefutedBy 引用全部悬空(推演五 #24)。
	ID         string     `yaml:"id"`
	Kind       string     `yaml:"kind"` // summary|contract|mutation|pitfall|usage
	Text       string     `yaml:"text"`
	Confidence Confidence `yaml:"confidence"`
	// At 写入时间(全量实现补定):维护欠账"摘要落后于其下变更"的判定依据,
	// 也是来源审计的一部分。旧分片缺此字段视为零值,判定时按保守处理。
	At      time.Time `yaml:"at,omitempty"`
	BasedOn []string  `yaml:"based_on,omitempty"` // 其他条目 ID("node-id#entry-id")
	// Author 来源可溯(knowledge.md §12.8 第 4 条):服务端从 clientInfo 推导,不接受 AI 自报。
	Author    string `yaml:"author,omitempty"`
	RefutedBy string `yaml:"refuted_by,omitempty"` // 勘误 change ID
	// SupersededBy 更新/合并链:被新条目取代,保留但退出注入;引用沿链解析——
	// 与 lineage/overturns 同构,"旧 ID 永不复用、新旧留链"贯穿三种对象。
	SupersededBy string `yaml:"superseded_by,omitempty"`
	// RetiredBy 体面退休(kb_verify:obsolete 的 change ID):没错但不再适用,
	// 归档退出注入、不触发级联(它没错,衍生结论未必失效)。
	RetiredBy string `yaml:"retired_by,omitempty"`
}

// Active 判断条目是否仍参与注入/查询呈现(未被取代、未被驳倒、未退休)。
func (e *Entry) Active() bool {
	return e.SupersededBy == "" && e.RetiredBy == "" && e.Confidence != ConfidenceRefuted
}

// Node 是空间金字塔的一格(impl §3)。
// auto 部分(签名/调用关系)与 coverage 均不落盘,读取时现算。
type Node struct {
	ID     string `yaml:"id"`    // 文法见 impl §3;目录节点 "internal/auth/";项目节点 "."
	Level  string `yaml:"level"` // project|dir|file|function|decl|stmt
	Anchor Anchor `yaml:"anchor"`
	Status Status `yaml:"status"`
	// Since 节点创建时间。history 联查按它过滤同名前任的记录(impl §7.3 recall)。
	Since   time.Time `yaml:"since"`
	Entries []Entry   `yaml:"entries,omitempty"`
	// Keywords 上限 12,小写归一去重;写入是整体替换语义(impl §7.3)。
	Keywords []string `yaml:"keywords,omitempty"`
	// Lineage 血缘:此节点历史上的旧 ID,全链 flat 集合(非一跳),查询按集合去重,
	// 天然免疫 A→B→A 环(knowledge.md §12.6)。
	Lineage []string `yaml:"lineage,omitempty"`
	// EraSummary 时代摘要(knowledge.md §12.3):早于 EraUntil 的历史在呈现层
	// 折叠为这段摘要(由 AI 经 kb_maintain 写入,负知识须逐条保留在摘要文本里);
	// 原始记录永在 journal 可溯源,永不改写。
	EraSummary string    `yaml:"era_summary,omitempty"`
	EraUntil   time.Time `yaml:"era_until,omitempty"`
	// PendingAnchor 待补锚(impl §7.3 record_change 第四情形):文件当时不可解析,
	// 记录照收、锚保持旧值;该文件下次成功解析时(任何读写路径经过)自动补锚。
	PendingAnchor bool `yaml:"pending_anchor,omitempty"`
}

// Rejected 是一条负知识:否决了什么、为什么(knowledge.md §5.1)。
type Rejected struct {
	Option string `yaml:"option" json:"option"`
	Reason string `yaml:"reason" json:"reason"`
}

// Change 是 journal 的一行变更记录(impl §3)。
// Change.Remaps(重构申报,二期)按演进规则届时加字段,不升号。
type Change struct {
	// ID = "chg_" + UTC 时间戳(YYYYMMDDTHHMMSSZ)+ "_" + 16 hex(crypto/rand 64bit)。
	// 唯一性不依赖时钟(多机多分支合并、时钟回拨下短随机段必撞车,
	// 而 overturns/RefutedBy/lineage 全拿它当外键);追加前查内存 ID 集合,撞则重生成。
	ID string `json:"id"`
	// Nodes 一个逻辑修改 = 一条记录。首个为主节点(承载 what/why),其余为波及节点。
	Nodes     []string   `json:"nodes"`
	At        time.Time  `json:"at"`
	Commit    string     `json:"commit,omitempty"`
	Task      string     `json:"task,omitempty"`
	What      string     `json:"what"`
	Why       string     `json:"why"`
	Rejected  []Rejected `json:"rejected,omitempty"`
	Overturns string     `json:"overturns,omitempty"` // 决策链:被推翻的 change ID
	Rebuttal  string     `json:"rebuttal,omitempty"`  // Overturns 非空时必填(engine 校验)
	// Remaps 重构申报(knowledge.md §12.6 第 2 层):拆分/合并机器猜不了,谁重构谁申报。
	Remaps   []Remap `json:"remaps,omitempty"`
	Verified string  `json:"verified,omitempty"`
	Author   string  `json:"author,omitempty"`
}

// Remap 是一条节点映射申报。分派粒度(定案,销 knowledge.md §16.14):
// 默认 From 的全部 Entries 归 To[0];Entries 字段可逐条指定归属(entryID → 目标节点 ID)。
// 迁移后条目可信度统一降半级待确认(verified→inferred、inferred→suspect)。
type Remap struct {
	From    string            `json:"from"`
	To      []string          `json:"to"`
	Entries map[string]string `json:"entries,omitempty"`
}

// NewEntryID 生成 "e_" + 8 hex 的稳定随机条目 ID。
func NewEntryID() string {
	return "e_" + randHex(4)
}

// NewChangeID 生成 "chg_<UTC 时间戳>_<16 hex>" 的变更 ID。
// 调用方(store)负责追加前查重、撞则重生成。
func NewChangeID(at time.Time) string {
	return "chg_" + at.UTC().Format("20060102T150405Z") + "_" + randHex(8)
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败意味着系统熵源坏了,任何回退都会破坏 ID 唯一性承诺。
		panic("model: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}
