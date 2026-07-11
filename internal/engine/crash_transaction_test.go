package engine

import (
	"os"
	"testing"
	"time"

	"github.com/zdypro888/iknowledge/internal/model"
)

func TestReloadRecoversPreparedMultiShardTransactionBeforeCacheLoad(t *testing.T) {
	e, _ := initEngine(t, map[string]string{
		"a.go": "package a\nfunc A() {}\n",
		"b.go": "package a\nfunc B() {}\n",
	})
	rels := []string{"tree/a.go.yaml", "tree/b.go.yaml", "journal/2026-07.jsonl"}
	original := map[string][]byte{}
	for _, rel := range rels[:2] {
		data, err := e.Store.ReadKnowledgeFile(rel)
		if err != nil {
			t.Fatal(err)
		}
		original[rel] = data
	}
	if _, err := e.Store.PrepareTruthTransaction(rels); err != nil {
		t.Fatal(err)
	}
	// 第一、第二分片已经替换，但 journal 尚未出现时进程退出。
	if err := e.Store.WriteKnowledgeFile(rels[0], []byte("broken-a\n")); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.WriteKnowledgeFile(rels[1], []byte("broken-b\n")); err != nil {
		t.Fatal(err)
	}

	restarted := New(e.Store)
	if err := restarted.EnsureRuntime(); err != nil {
		t.Fatalf("EnsureRuntime must recover before parsing cache: %v", err)
	}
	for _, rel := range rels[:2] {
		got, err := e.Store.ReadKnowledgeFile(rel)
		if err != nil || string(got) != string(original[rel]) {
			t.Fatalf("%s not restored before cache load: %v", rel, err)
		}
	}
	if _, err := e.Store.ReadKnowledgeFile(rels[2]); !os.IsNotExist(err) {
		t.Fatalf("new journal should remain absent: %v", err)
	}
}

func TestReloadRecoversPreparedTransactionIncludingWrittenJournal(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"a.go": "package a\nfunc A() {}\n"})
	shardRel := "tree/a.go.yaml"
	change := model.Change{
		ID: "chg_20260704T120000Z_aaaaaaaaaaaaaaaa", Nodes: []string{"a.go#A"},
		At: time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC), What: "crash", Why: "test",
	}
	journalRel := e.Store.JournalRelFor(change)
	before, err := e.Store.ReadKnowledgeFile(shardRel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Store.PrepareTruthTransaction([]string{shardRel, journalRel}); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.WriteKnowledgeFile(shardRel, []byte("broken-after-shard\n")); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.AppendChange(change); err != nil {
		t.Fatal(err)
	}

	restarted := New(e.Store)
	if err := restarted.EnsureRuntime(); err != nil {
		t.Fatal(err)
	}
	got, err := e.Store.ReadKnowledgeFile(shardRel)
	if err != nil || string(got) != string(before) {
		t.Fatalf("shard not restored: %v", err)
	}
	if _, err := e.Store.ReadKnowledgeFile(journalRel); !os.IsNotExist(err) {
		t.Fatalf("journal written before crash was not rolled back: %v", err)
	}
	if got := restarted.rt.ix.ChangeByID(change.ID); got != nil {
		t.Fatalf("rolled-back journal leaked into index: %+v", got)
	}
}

func TestReloadRecoversTaskCompleteWIPDeletion(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"a.go": "package a\nfunc A() {}\n"})
	wip := model.WIP{Owner: "codex@s-crash", Task: "crash-safe task", Intent: "verify WAL"}
	if err := e.Store.SaveWIP(wip); err != nil {
		t.Fatal(err)
	}
	change := model.Change{
		ID: "chg_20260704T120000Z_bbbbbbbbbbbbbbbb", Nodes: []string{model.ProjectNodeID},
		At: time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC), What: "complete", Why: "test",
	}
	wipRel := e.Store.WIPRelFor(wip.Owner)
	journalRel := e.Store.JournalRelFor(change)
	if _, err := e.Store.PrepareTruthTransaction([]string{wipRel, journalRel}); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.ClearWIP(wip.Owner); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.AppendChange(change); err != nil {
		t.Fatal(err)
	}

	restarted := New(e.Store)
	if err := restarted.EnsureRuntime(); err != nil {
		t.Fatal(err)
	}
	if got := restarted.wipOfLocked(wip.Owner); got == nil || got.Task != wip.Task {
		t.Fatalf("prepared task complete did not restore WIP: %+v", got)
	}
	if _, err := e.Store.ReadKnowledgeFile(journalRel); !os.IsNotExist(err) {
		t.Fatalf("prepared task journal survived recovery: %v", err)
	}
}

func TestTransactionCacheRebuildPreservesSessionLedger(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"a.go": "package a\nfunc A() {}\n"})
	sid := "session-ledger"
	e.recordRead(sid, "a.go#A", "sha256:before")
	if _, err := e.RecordChange(ChangeArgs{
		Nodes: []string{"a.go#A"}, What: "记录事务缓存重建", Why: "验证会话台账不被清空",
	}, sid, "codex"); err != nil {
		t.Fatal(err)
	}
	reads := e.ledgerSnapshot(sid)
	if got, ok := reads["a.go#A"]; !ok || !got.Digested {
		t.Fatalf("transaction reload lost or failed to digest session ledger: %+v", reads)
	}
}
