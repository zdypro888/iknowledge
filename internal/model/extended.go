package model

import "time"

// 本文件是全量实现(附录 A 轮 24)时按"加字段不升号"演进规则补定的结构:
// WIP 任务态、Flow 流程/主题节点、Entry 退休标记、Node 时代摘要与待补锚标记。

// WIP 是任务态层的一份台账(knowledge.md §7):与知识层严格分离,
// 存 .knowledge/wip/<owner>.yaml,git 排除,求即时不求持久。
type WIP struct {
	Task     string    `yaml:"task"`               // 任务一句话(含 issue 引用)
	Intent   string    `yaml:"intent,omitempty"`   // 意图
	Plan     []string  `yaml:"plan,omitempty"`     // 计划步骤
	Done     []string  `yaml:"done,omitempty"`     // 已完成
	Todo     []string  `yaml:"todo,omitempty"`     // 待办
	Touching []string  `yaml:"touching,omitempty"` // 声明"正在动谁"(节点 ID 列表)
	Owner    string    `yaml:"owner"`              // 谁的任务(author 同源:clientInfo 推导)
	Updated  time.Time `yaml:"updated"`
}

// FlowStep 是流程节点的一步,引用树节点而不复制其内容(knowledge.md §6)。
type FlowStep struct {
	Node  string    `yaml:"node"`            // 树节点 ID
	Note  string    `yaml:"note,omitempty"`  // 该步说明("入口"/"核心验证"…)
	Since time.Time `yaml:"since,omitempty"` // 此引用代际；防旧 ID 被复用后反链跳到无关新节点
}

// Flow 是横向流程/主题节点(knowledge.md §6):
// 流程 id 形如 "flow:user-login",主题 id 形如 "topic:error-handling"。
// 树节点的反向链接(被哪些流程引用)不落盘,由 index 现算——与 auto 同哲学,防腐。
type Flow struct {
	ID           string     `yaml:"id"`
	Title        string     `yaml:"title"`
	Steps        []FlowStep `yaml:"steps,omitempty"`        // 主题节点可为空
	Conventions  []string   `yaml:"conventions,omitempty"`  // 全局约定/横切关注点
	Troubleshoot string     `yaml:"troubleshoot,omitempty"` // 排障入口说明
	Deprecated   bool       `yaml:"deprecated,omitempty"`
	Since        time.Time  `yaml:"since"`
	Author       string     `yaml:"author,omitempty"`
	// 失效联动(knowledge.md §6):引用的树节点 suspect 时流程连带"待复核"——现算,不落盘。
}

// FlowShard 是 flows/topics 目录下一个文件的内容。
type FlowShard struct {
	Schema int  `yaml:"schema"`
	Flow   Flow `yaml:"flow"`
}

// Findings 是侦查交卷的结构化结果(knowledge.md §10.4):
// 存档进 .knowledge/local/findings-YYYY-MM.jsonl(本地态,不进 git——
// 蒸馏物已经 kb_remember/kb_task 落库,findings 本身是会话产物)。
type Findings struct {
	Job        string    `json:"job"`
	Question   string    `json:"question"`
	Conclusion string    `json:"conclusion"`
	Locations  []string  `json:"locations,omitempty"` // 节点 ID 指针
	Plan       string    `json:"plan,omitempty"`
	Risks      string    `json:"risks,omitempty"`
	At         time.Time `json:"at"`
	Author     string    `json:"author,omitempty"`
}
