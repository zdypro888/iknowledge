package main

import (
	"errors"
	"testing"

	"github.com/zdypro888/iknowledge/internal/engine"
	"github.com/zdypro888/iknowledge/internal/store"
)

func TestServeAuthTokenPersistsMode(t *testing.T) {
	t.Setenv("IKNOWLEDGE_STATE_HOME", t.TempDir())
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	token, err := serveAuthToken(s, true)
	if err != nil || token == "" {
		t.Fatalf("首次 --auth: token=%q err=%v", token, err)
	}
	restarted, err := serveAuthToken(s, false)
	if err != nil {
		t.Fatal(err)
	}
	if restarted != token {
		t.Fatalf("手工重启漏 --auth 时发生降级: got %q want %q", restarted, token)
	}
}

func TestAcquireRecoveredViewHoldsWriterLockForWholeSnapshot(t *testing.T) {
	repo := setupGitRepo(t)
	initRepo(t, repo, engine.InitOptions{})
	s, err := store.Open(repo)
	if err != nil {
		t.Fatal(err)
	}
	releaseView, err := acquireRecoveredView(s)
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.Open(repo)
	if err != nil {
		releaseView()
		t.Fatal(err)
	}
	if _, err := other.AcquireWriterLock(); !errors.Is(err, store.ErrLocked) {
		releaseView()
		t.Fatalf("一致视图期间 writer 应被挡:%v", err)
	}
	releaseView()
	releaseWriter, err := other.AcquireWriterLock()
	if err != nil {
		t.Fatalf("视图释放后 writer 仍被挡:%v", err)
	}
	releaseWriter()
}
