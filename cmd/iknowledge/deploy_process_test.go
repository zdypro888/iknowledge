package main

import (
	"reflect"
	"testing"
)

func TestParseIKnowledgeProcesses(t *testing.T) {
	input := ` 10 1 01-02:03:04 20480 iknowledge /usr/local/bin/iknowledge serve --repo /work/a
 11 9 03:04 15360 iknowledge iknowledge stdio --repo /work/a
 12 9 00:05 100 other other command
malformed`
	got := parseIKnowledgeProcesses(input)
	if len(got) != 2 || got[0].Kind != "serve" || got[0].RSSKiB != 20480 ||
		got[1].Kind != "stdio" || got[1].Elapsed != "03:04" {
		t.Fatalf("parsed=%+v", got)
	}
	if elapsedSeconds("01-02:03:04") != 93784 || elapsedSeconds("03:04") != 184 {
		t.Fatalf("elapsed parser day=%d minute=%d", elapsedSeconds("01-02:03:04"), elapsedSeconds("03:04"))
	}
}

func TestParseIKnowledgeProcessesRequiresExactExecutableAndSubcommand(t *testing.T) {
	input := ` 20 1 00:01 100 zsh /bin/zsh -c iknowledge stdio --repo /work/a
 21 1 00:02 101 code /usr/bin/code /tmp/iknowledge serve notes.txt
 22 1 00:03 102 go /usr/local/go/bin/go test -run Test_iknowledge_serve
 23 1 00:04 103 iknowledge-helper /tmp/iknowledge-helper stdio
 24 1 00:05 104 iknowledge /usr/local/bin/iknowledge status --note iknowledge serve
 25 1 00:06 105 iknowledge /usr/local/bin/iknowledge --repo /work/a serve
 26 1 00:07 106 iknowledge /usr/local/bin/iknowledge serve --repo /work/a
 27 1 00:08 107 iknowledge iknowledge stdio --repo /work/a`

	got := parseIKnowledgeProcesses(input)
	want := []iknowledgeProcess{
		{PID: 26, PPID: 1, Elapsed: "00:07", RSSKiB: 106, Kind: "serve", Command: "/usr/local/bin/iknowledge serve --repo /work/a"},
		{PID: 27, PPID: 1, Elapsed: "00:08", RSSKiB: 107, Kind: "stdio", Command: "iknowledge stdio --repo /work/a"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parsed processes mismatch\n got: %+v\nwant: %+v", got, want)
	}
}
