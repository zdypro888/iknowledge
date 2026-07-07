package engine

import (
	"strings"
	"testing"
)

// 跨节点矛盾巡检(2026-07-05):同关键词簇并读简报——机器聚类,AI 裁决。
func TestPatrolCrossNodeClusters(t *testing.T) {
	e, _ := newRepo(t, map[string]string{
		"a/auth.go": "package a\n\nfunc Login() {}\n",
		"b/gate.go": "package b\n\nfunc Check() {}\n",
	})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	sid := "s-pt"

	// 空库:无簇可巡。
	brief, err := e.PatrolBrief("")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(brief, "无跨节点同关键词簇") {
		t.Errorf("空库应报无可巡检:%s", brief)
	}

	// 两个节点共享关键词,知识实际互相矛盾(经典跨节点盲区场景)。
	for node, text := range map[string]string{
		"a/auth.go#Login": "限流后返回 429,调用方不得自行重试",
		"b/gate.go#Check": "网关层对 429 自动重试 3 次,上游无需处理",
	} {
		if _, err := e.Remember(RememberArgs{
			Node:     node,
			Entries:  []RememberEntry{{Kind: "contract", Text: text}},
			Keywords: []string{"ratelimit", "throttle"},
		}, sid, "claude-code"); err != nil {
			t.Fatal(err)
		}
	}

	brief, err = e.PatrolBrief("")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"a/auth.go#Login", "b/gate.go#Check", "巡检纪律", "refute"} {
		if !strings.Contains(brief, want) {
			t.Errorf("简报缺 %q:\n%s", want, brief)
		}
	}
	// 两个关键词圈的是同一节点集 → 去重后只巡一簇。
	if n := strings.Count(brief, "簇「"); n != 1 {
		t.Errorf("同节点集应去重为 1 簇,实得 %d:\n%s", n, brief)
	}

	// scope 过滤:b/ 范围内只剩单节点,不成簇。
	brief, err = e.PatrolBrief("b/")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(brief, "无跨节点同关键词簇") {
		t.Errorf("scope 内单节点不应成簇:%s", brief)
	}

	// MCP 面:kb_maintain action=patrol 走同一条路。
	out, err := e.Maintain(MaintainArgs{Action: "patrol"}, sid, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "簇「") {
		t.Errorf("maintain patrol 应返回简报:%s", out)
	}
}
