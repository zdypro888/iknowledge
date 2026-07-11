package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTransactionStore(t *testing.T) *Store {
	t.Helper()
	t.Setenv(stateHomeEnv, t.TempDir())
	return newStore(t)
}

func TestRecoverPreparedTruthTransactionRestoresAllFiles(t *testing.T) {
	s := newTransactionStore(t)
	if err := s.WriteKnowledgeFile("project.yaml", []byte("old-project\n")); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteKnowledgeFile("journal/2026-07.jsonl", []byte("old-journal\n")); err != nil {
		t.Fatal(err)
	}
	tx, err := s.PrepareTruthTransaction([]string{
		"tree/new.go.yaml", "project.yaml", "journal/2026-07.jsonl",
	})
	if err != nil {
		t.Fatalf("PrepareTruthTransaction: %v", err)
	}
	if tx == nil {
		t.Fatal("nil transaction")
	}
	if err := s.WriteKnowledgeFile("project.yaml", []byte("new-project\n")); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteKnowledgeFile("tree/new.go.yaml", []byte("new-shard\n")); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteKnowledgeFile("journal/2026-07.jsonl", []byte("old-journal\nnew-change\n")); err != nil {
		t.Fatal(err)
	}

	// 模拟进程在多个 truth write 后、committed marker 前退出。
	reopened, err := Open(s.RepoRoot())
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.RecoverTruthTransaction(); err != nil {
		t.Fatalf("RecoverTruthTransaction: %v", err)
	}
	for rel, want := range map[string]string{
		"project.yaml":          "old-project\n",
		"journal/2026-07.jsonl": "old-journal\n",
	} {
		got, err := reopened.ReadKnowledgeFile(rel)
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v; want %q", rel, got, err, want)
		}
	}
	if _, err := reopened.ReadKnowledgeFile("tree/new.go.yaml"); !os.IsNotExist(err) {
		t.Fatalf("new transaction file survived rollback: %v", err)
	}
}

func TestRecoveryCannotRaceActiveWriterTransaction(t *testing.T) {
	s := newTransactionStore(t)
	if err := s.WriteKnowledgeFile("project.yaml", []byte("before\n")); err != nil {
		t.Fatal(err)
	}
	release, err := s.AcquireWriterLock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareTruthTransaction([]string{"project.yaml"}); err != nil {
		release()
		t.Fatal(err)
	}
	if err := s.WriteKnowledgeFile("project.yaml", []byte("in-flight\n")); err != nil {
		release()
		t.Fatal(err)
	}

	reader, err := Open(s.RepoRoot())
	if err != nil {
		release()
		t.Fatal(err)
	}
	if err := reader.RecoverTruthTransaction(); !errors.Is(err, ErrLocked) {
		release()
		t.Fatalf("非 owner 不得恢复活事务: %v", err)
	}
	got, err := s.ReadKnowledgeFile("project.yaml")
	if err != nil || string(got) != "in-flight\n" {
		release()
		t.Fatalf("并发恢复改写了活事务:data=%q err=%v", got, err)
	}
	release()
	if err := reader.RecoverTruthTransaction(); err != nil {
		t.Fatal(err)
	}
	got, err = reader.ReadKnowledgeFile("project.yaml")
	if err != nil || string(got) != "before\n" {
		t.Fatalf("writer 退出后 prepared WAL 未恢复:data=%q err=%v", got, err)
	}
}

func TestRecoverCommittedTruthTransactionKeepsTruth(t *testing.T) {
	s := newTransactionStore(t)
	if err := s.WriteKnowledgeFile("project.yaml", []byte("old\n")); err != nil {
		t.Fatal(err)
	}
	tx, err := s.PrepareTruthTransaction([]string{"project.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareTruthTransaction([]string{"project.yaml"}); err == nil {
		t.Fatal("同一仓库的第二个 intent 应被拒绝")
	}
	if err := s.WriteKnowledgeFile("project.yaml", []byte("committed\n")); err != nil {
		t.Fatal(err)
	}
	_, commitPath, repoKey, err := s.truthTransactionPaths()
	if err != nil {
		t.Fatal(err)
	}
	marker, _ := json.Marshal(truthTransactionCommit{
		Schema: truthTransactionSchema, State: "committed", ID: tx.intent.ID, RepoKey: repoKey,
	})
	if err := writePrivateStateFile(commitPath, append(marker, '\n')); err != nil {
		t.Fatal(err)
	}

	// 模拟 marker 已持久化、WAL 尚未清理时退出。
	reopened, err := Open(s.RepoRoot())
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.RecoverTruthTransaction(); err != nil {
		t.Fatal(err)
	}
	got, err := reopened.ReadKnowledgeFile("project.yaml")
	if err != nil || string(got) != "committed\n" {
		t.Fatalf("committed truth = %q, %v", got, err)
	}
	intentPath, commitPath, _, _ := reopened.truthTransactionPaths()
	for _, path := range []string{intentPath, commitPath} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("WAL not cleaned: %s: %v", path, err)
		}
	}
}

func TestTruthTransactionCommitAPIKeepsTruthAndAllowsNextTransaction(t *testing.T) {
	s := newTransactionStore(t)
	if err := s.WriteKnowledgeFile("project.yaml", []byte("before\n")); err != nil {
		t.Fatal(err)
	}
	tx, err := s.PrepareTruthTransaction([]string{"project.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WriteKnowledgeFile("project.yaml", []byte("after\n")); err != nil {
		t.Fatal(err)
	}
	committed, err := tx.Commit()
	if err != nil || !committed {
		t.Fatalf("Commit = %v, %v", committed, err)
	}
	if err := s.RecoverTruthTransaction(); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadKnowledgeFile("project.yaml")
	if err != nil || string(got) != "after\n" {
		t.Fatalf("committed truth = %q, %v", got, err)
	}
	// 成功清理后同仓可开始下一事务；若旧 intent/marker 泄漏这里会拒绝。
	next, err := s.PrepareTruthTransaction([]string{"project.yaml"})
	if err != nil {
		t.Fatalf("next Prepare: %v", err)
	}
	if err := next.Abort(); err != nil {
		t.Fatal(err)
	}
}

func TestTruthTransactionAbortRestoresDeletedWIP(t *testing.T) {
	s := newTransactionStore(t)
	rel := s.WIPRelFor("codex@session")
	if err := s.WriteKnowledgeFile(rel, []byte("task: old\n")); err != nil {
		t.Fatal(err)
	}
	tx, err := s.PrepareTruthTransaction([]string{rel, "journal/2026-07.jsonl"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveKnowledgeFile(rel); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteKnowledgeFile("journal/2026-07.jsonl", []byte("change\n")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Abort(); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadKnowledgeFile(rel)
	if err != nil || string(got) != "task: old\n" {
		t.Fatalf("restored WIP = %q, %v", got, err)
	}
	if _, err := s.ReadKnowledgeFile("journal/2026-07.jsonl"); !os.IsNotExist(err) {
		t.Fatalf("new journal survived abort: %v", err)
	}
}

func TestTruthTransactionRejectsUnknownSchemaAndNonTruthPath(t *testing.T) {
	s := newTransactionStore(t)
	if _, err := s.PrepareTruthTransaction([]string{"local/findings.jsonl"}); err == nil ||
		!strings.Contains(err.Error(), "不在白名单") {
		t.Fatalf("non-truth path error = %v", err)
	}
	intentPath, _, repoKey, err := s.truthTransactionPaths()
	if err != nil {
		t.Fatal(err)
	}
	bad := `{"schema":99,"state":"prepared","id":"00000000000000000000000000000000","repo_key":"` + repoKey + `","files":[{"rel":"project.yaml","exists":false}]}`
	if err := writePrivateStateFile(intentPath, []byte(bad)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverTruthTransaction(); err == nil || !strings.Contains(err.Error(), "schema 99") {
		t.Fatalf("unknown schema error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), "project.yaml")); !os.IsNotExist(err) {
		t.Fatalf("unknown intent must not mutate truth: %v", err)
	}
}

func TestTruthTransactionFlowTopicPathsMatchRuntimeLayout(t *testing.T) {
	for _, rel := range []string{
		"flows/onboarding.yaml",
		"topics/errors.yaml",
	} {
		if err := validateTruthTransactionRel(rel); err != nil {
			t.Errorf("%s rejected: %v", rel, err)
		}
	}
	for _, rel := range []string{
		"flows/../config.yaml", "topics/x.txt", "wip/nested/task.yaml",
		"flows/team/onboarding.yaml", "flows/onboarding.jsonl",
		"topics/domain/errors.yaml", "topics/errors.jsonl",
	} {
		if err := validateTruthTransactionRel(rel); err == nil {
			t.Errorf("%s unexpectedly allowed", rel)
		}
	}
}
