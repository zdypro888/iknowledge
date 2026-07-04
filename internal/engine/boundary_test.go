package engine

import (
	"strings"
	"testing"
)

// 边界定案:知识库对应代码,不是记忆库——任务态词警示 + 无锚节点边界提醒(都不拒收)。
func TestKnowledgeBoundary(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"a/a.go": "package a\n\nfunc F() int { return 1 }\n"})
	sid := "s-bd"

	// ① 任务态词 → 警示不拒收。
	out, err := e.Remember(RememberArgs{
		Node:    "a/a.go#F",
		Entries: []RememberEntry{{Kind: "pitfall", Text: "TODO 回头把这里的重试逻辑补上"}},
	}, sid, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "疑似任务态内容") || !strings.Contains(out, "kb_task") {
		t.Errorf("任务态词应警示:%s", out)
	}

	// ② 合法技术陈述含"下次"不误杀。
	out, err = e.Remember(RememberArgs{
		Node:    "a/a.go#F",
		Entries: []RememberEntry{{Kind: "usage", Text: "断线后下次重连会全量重放快照,调用方无需自行补偿"}},
	}, sid, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "疑似任务态内容") {
		t.Errorf("合法技术陈述被误杀:%s", out)
	}

	// ③ 无锚节点(dir)写入 → 边界提醒(判据 + 三去处)。
	out, err = e.Remember(RememberArgs{
		Node:    "a/",
		Entries: []RememberEntry{{Kind: "contract", Text: "本目录对外接口冻结,新增字段须走 v2 前缀(平台合规要求)"}},
	}, sid, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "无锚节点只收") || !strings.Contains(out, "代码变了它会失效吗") {
		t.Errorf("无锚写入应亮边界提醒:%s", out)
	}

	// ④ 有锚节点写入不带无锚提醒(不刷屏)。
	out, err = e.Remember(RememberArgs{
		Node:    "a/a.go#F",
		Entries: []RememberEntry{{Kind: "contract", Text: "返回值语义:正数为量,非正数为错误码,调用方先判符号"}},
	}, sid, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "无锚节点只收") {
		t.Errorf("有锚写入不应带无锚提醒:%s", out)
	}
}
