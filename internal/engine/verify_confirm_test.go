package engine

import (
	"strings"
	"testing"
)

// confirm 证据强制(2026-07-05 三人成虎堵漏):升级必须附依据并进 journal;
// verified 复确认只刷时间锚不要证据。
func TestConfirmEvidenceRequired(t *testing.T) {
	e, _ := newRepo(t, map[string]string{
		"a.go": "package a\n\nfunc F() int { return 1 }\n",
	})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	sid := "s-cf"
	if _, err := e.Remember(RememberArgs{
		Node:    "a.go#F",
		Entries: []RememberEntry{{Kind: "contract", Text: "返回值恒为正,调用方以 0 作哨兵会误判"}},
	}, sid, "claude-code"); err != nil {
		t.Fatal(err)
	}
	view, _, err := e.Recall(RecallArgs{Query: "a.go#F"}, sid)
	if err != nil {
		t.Fatal(err)
	}
	eid := extractEntryID(t, view, "返回值恒为正")
	ref := "a.go#F#" + eid

	// ① 无证据升级 → 拒收。
	_, err = e.Verify(VerifyArgs{Entry: ref, Verdict: "confirm"}, sid, "claude-code")
	kbCode(t, err, "EVIDENCE_REQUIRED")

	// ② 带证据升级 → verified + journal 留确认记录(凭什么确认可溯)。
	res, err := e.Verify(VerifyArgs{Entry: ref, Verdict: "confirm",
		Evidence: "go test -run TestF 绿,断言返回值 >0"}, sid, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "verified") || !strings.Contains(res, "确认记录") {
		t.Errorf("升级回执应含 verified 与确认记录号:%s", res)
	}
	found := false
	for _, c := range e.rt.ix.Changes() {
		if strings.Contains(c.What, "确认:条目 "+eid) && strings.Contains(c.Why, "go test -run TestF") {
			found = true
		}
	}
	if !found {
		t.Error("journal 应有确认记录(验证依据留痕)")
	}

	// ③ verified 复确认 → 不要证据,纯时间锚刷新。
	res, err = e.Verify(VerifyArgs{Entry: ref, Verdict: "confirm"}, sid, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "复确认") {
		t.Errorf("verified 复确认应只刷时间锚:%s", res)
	}
}
