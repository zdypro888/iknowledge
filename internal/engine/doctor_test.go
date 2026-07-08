package engine

import (
	"strings"
	"testing"
)

func TestParserHealthAndDoctor(t *testing.T) {
	e, _ := initEngine(t, map[string]string{
		"a.go": "package a\n\nfunc F() {}\n",
		"b.ts": "export function G() { return 1 }\n",
	})
	ph, err := e.ParserHealth()
	if err != nil {
		t.Fatal(err)
	}
	if ph.Files != 2 || ph.Symbols == 0 {
		t.Fatalf("parser health 不完整:%+v", ph)
	}
	if ph.ByLang["go"].Files != 1 || ph.ByLang["typescript"].Files != 1 {
		t.Fatalf("语言统计不对:%+v", ph.ByLang)
	}
	dr, err := e.Doctor()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dr.Text(), "parser: files=2") {
		t.Fatalf("doctor 未包含 parser 仪表盘:\n%s", dr.Text())
	}
}

func TestSessionGateWarnsUndigestedRepeatedReads(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"a.go": "package a\n\nfunc F() {}\n"})
	sid := "session-gate"
	for range 2 {
		if _, _, err := e.Recall(RecallArgs{Query: "a.go#F"}, sid); err != nil {
			t.Fatal(err)
		}
	}
	out, err := e.Session(sid, "gate")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "多次读取但未沉淀") {
		t.Fatalf("gate 未提醒沉淀:\n%s", out)
	}
}
