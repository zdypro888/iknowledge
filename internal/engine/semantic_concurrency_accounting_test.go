package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zdypro888/iknowledge/internal/vector"
)

func TestSyncContextCancellationWhileWaitingForRuntimeWriter(t *testing.T) {
	e, _ := initEngine(t, map[string]string{
		"worker.go": "package worker\n\nfunc Run() {}\n",
	})

	e.rt.mu.Lock()
	locked := true
	defer func() {
		if locked {
			e.rt.mu.Unlock()
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.SyncContext(ctx) }()

	// Keep the writer lock unavailable long enough for SyncContext to enter its
	// cancellable wait. It must return without requiring the unrelated owner to
	// release the lock first.
	time.Sleep(25 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("SyncContext cancellation=%v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SyncContext remained blocked on rt.mu after cancellation")
	}

	e.rt.mu.Unlock()
	locked = false
}

func TestSemanticResidentWaitsHonorCancellation(t *testing.T) {
	t.Run("ensure snapshot", func(t *testing.T) {
		e := &Engine{}
		e.semantic.residentMu.Lock()
		locked := true
		defer func() {
			if locked {
				e.semantic.residentMu.Unlock()
			}
		}()

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, _, err := e.ensureSemanticSnapshot(ctx, DefaultSemanticSettings(), [32]byte{})
			done <- err
		}()
		time.Sleep(25 * time.Millisecond)
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("ensureSemanticSnapshot cancellation=%v, want context.Canceled", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("ensureSemanticSnapshot remained blocked on resident lease after cancellation")
		}

		e.semantic.residentMu.Unlock()
		locked = false
	})

	t.Run("health snapshot", func(t *testing.T) {
		e, cfg := semanticHealthTestEngine(t, "http://127.0.0.1:11434/v1")
		if err := SaveSemanticSettings(e.Store, cfg); err != nil {
			t.Fatal(err)
		}
		writeSemanticHealthIndex(t, e, cfg, "2026-07-19T12:34:56Z")

		e.semantic.residentMu.Lock()
		locked := true
		defer func() {
			if locked {
				e.semantic.residentMu.Unlock()
			}
		}()

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := e.SemanticHealthSnapshotContext(ctx)
			done <- err
		}()
		// The fixture is metadata-ready and performs no provider I/O. This delay
		// lets it reach the resident-generation barrier deterministically.
		time.Sleep(100 * time.Millisecond)
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("SemanticHealthSnapshotContext cancellation=%v, want context.Canceled", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("SemanticHealthSnapshotContext remained blocked on resident lease after cancellation")
		}

		e.semantic.residentMu.Unlock()
		locked = false
	})
}

func TestSemanticPartialHealthReleasesSourceLeaseBeforeResidentInvalidation(t *testing.T) {
	e, cfg := semanticHealthTestEngine(t, "http://127.0.0.1:11434/v1")
	if err := SaveSemanticSettings(e.Store, cfg); err != nil {
		t.Fatal(err)
	}
	writeSemanticHealthIndex(t, e, cfg, "2026-07-20T12:34:56Z")
	if _, err := e.Remember(RememberArgs{Node: "vault.go#Vault", Entries: []RememberEntry{{
		Kind: "summary", Text: "new source generation for lock-order regression",
	}}}, "semantic-lock-order", "test"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.semanticSourceSnapshot(context.Background(), false); err != nil {
		t.Fatal(err)
	}

	// Force partial-health invalidation to wait at residentMu. While it waits,
	// the source-generation write lock must remain obtainable: holding a source
	// read lease here would invert clear's resident -> source order and deadlock.
	e.semantic.residentMu.Lock()
	residentLocked := true
	defer func() {
		if residentLocked {
			e.semantic.residentMu.Unlock()
		}
	}()
	waitObserved := make(chan struct{})
	waitCtx := &observedDoneContext{Context: context.Background(), observed: waitObserved}
	type healthResult struct {
		health SemanticHealth
		err    error
	}
	healthDone := make(chan healthResult, 1)
	go func() {
		health, err := e.SemanticHealthSnapshotContext(waitCtx)
		healthDone <- healthResult{health: health, err: err}
	}()
	// With the current source manifest prebuilt, all prior context-aware locks
	// are uncontended TryLock paths. The first observed Done() select is therefore
	// the deliberate wait on residentMu below, making this a deterministic
	// assertion rather than a scheduler-dependent sleep.
	select {
	case <-waitObserved:
	case <-time.After(5 * time.Second):
		t.Fatal("partial health did not reach resident invalidation")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.semantic.sourceResidentMu.LockContext(ctx); err != nil {
		t.Fatalf("partial status retained source lease while waiting for resident invalidation: %v", err)
	}
	e.semantic.sourceResidentMu.Unlock()
	e.semantic.residentMu.Unlock()
	residentLocked = false

	select {
	case result := <-healthDone:
		if result.err != nil || result.health.Status != SemanticHealthPartial {
			t.Fatalf("health=%+v err=%v, want partial", result.health, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("partial health remained blocked after resident invalidation resumed")
	}
}

func TestSyncSemanticDisabledReleasesSemanticProcessCharges(t *testing.T) {
	e, _ := initEngine(t, map[string]string{
		"worker.go": "package worker\n\nfunc Run() {}\n",
	})
	coordinator := NewSemanticProcessCoordinator(SemanticProcessResidentMaxMiB)
	if err := e.SetSemanticProcessCoordinator(coordinator); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.semanticSourceSnapshot(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if got := coordinator.sourceReservedBytes(e); got == 0 {
		t.Fatal("source fixture did not retain a process charge")
	}

	const vectorCharge = uint64(16 << 20)
	if err := coordinator.reserveResident(e, vectorCharge); err != nil {
		t.Fatal(err)
	}
	e.semantic.mu.Lock()
	e.semantic.snapshot = &vector.Snapshot{}
	e.semantic.loadedKey = "disabled-fixture"
	e.semantic.mu.Unlock()

	cfg := DefaultSemanticSettings()
	cfg.Endpoint = "http://127.0.0.1:11434/v1"
	cfg.Model = "disabled-test"
	cfg.Enabled = false
	if err := SaveSemanticSettings(e.Store, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := e.syncSemanticOwner(context.Background()); err == nil {
		t.Fatal("syncSemanticOwner accepted disabled semantic settings")
	}
	if got := coordinator.sourceReservedBytes(e); got != 0 {
		t.Fatalf("disabled sync retained source charge=%d", got)
	}
	if got := coordinator.reservedBytes(e); got != 0 {
		t.Fatalf("disabled sync retained vector charge=%d", got)
	}
	e.rt.mu.RLock()
	manifestReady := e.rt.semanticManifest.ready
	e.rt.mu.RUnlock()
	if manifestReady {
		t.Fatal("disabled sync retained source manifest")
	}
}

func TestReloadWaitsForSourceGenerationLeaseBeforeRetiringCharge(t *testing.T) {
	e, _ := initEngine(t, map[string]string{
		"worker.go": "package worker\n\nfunc Run() {}\n",
	})
	coordinator := NewSemanticProcessCoordinator(SemanticProcessResidentMaxMiB)
	if err := e.SetSemanticProcessCoordinator(coordinator); err != nil {
		t.Fatal(err)
	}
	_, manifest, lease, err := e.semanticSourceSnapshotLease(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !manifest.ready || coordinator.sourceReservedBytes(e) == 0 {
		t.Fatal("source fixture was not published and charged")
	}
	versionBefore := manifest.version
	manifest = semanticSourceManifest{}

	// Force the next coherent reload to observe a flow-generation change without
	// modifying repository files. reloadLockedContext must then wait on the active
	// source read lease before clearing the map and retiring its resident charge.
	e.rt.mu.Lock()
	e.rt.semanticFlowsHashReady = true
	e.rt.semanticFlowsHash = [32]byte{0xff}
	e.rt.mu.Unlock()

	done := make(chan error, 1)
	go func() { done <- e.SyncContext(context.Background()) }()
	select {
	case err := <-done:
		lease.Release()
		t.Fatalf("reload passed active source generation lease: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if got := coordinator.sourceReservedBytes(e); got == 0 {
		lease.Release()
		t.Fatal("reload retired source charge before active lease ended")
	}

	lease.Release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reload remained blocked after source generation lease release")
	}
	if got := coordinator.sourceReservedBytes(e); got != 0 {
		t.Fatalf("reload retained source charge=%d", got)
	}
	e.rt.mu.RLock()
	ready := e.rt.semanticManifest.ready
	versionAfter := e.rt.semanticSourceVersion
	e.rt.mu.RUnlock()
	if ready {
		t.Fatal("reload retained source manifest after generation change")
	}
	if versionAfter == versionBefore {
		t.Fatalf("reload did not advance source generation: before=%d after=%d", versionBefore, versionAfter)
	}
}
