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
