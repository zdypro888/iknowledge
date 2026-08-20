package mcpserv

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestClaimSemanticSyncConcurrentSingleWinner(t *testing.T) {
	s := New(nil)
	s.sessions["same-session"] = &session{lastSeen: time.Now()}

	const callers = 64
	start := make(chan struct{})
	var wg sync.WaitGroup
	var claimed atomic.Int64
	errs := make(chan error, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			won, err := s.claimSemanticSync("same-session")
			if err != nil {
				errs <- err
				return
			}
			if won {
				claimed.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("claimSemanticSync: %v", err)
	}
	if got := claimed.Load(); got != 1 {
		t.Fatalf("concurrent claims=%d, want exactly one provider-authorized winner", got)
	}
	if !s.semanticSyncAttempted("same-session") {
		t.Fatal("winning claim did not persist attempted state")
	}
}

func TestSuppressSemanticActionPreservesOtherStatus(t *testing.T) {
	input := "repoRoot: /repo\nsemantic_action: kb_semantic action=sync | policy=ai-local\n节点: 3\n"
	want := "repoRoot: /repo\n节点: 3\n"
	if got := suppressSemanticAction(input); got != want {
		t.Fatalf("suppressSemanticAction:\n got %q\nwant %q", got, want)
	}
}
