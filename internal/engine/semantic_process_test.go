package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zdypro888/iknowledge/internal/model"
	"github.com/zdypro888/iknowledge/internal/vector"
)

func TestSemanticProcessCoordinatorBoundsCumulativeSourceManifests(t *testing.T) {
	coordinator := NewSemanticProcessCoordinator(1024)
	buildBytes := uint64(semanticSourceBuildMaxMiB) << 20
	coordinator.maxSourceBytes = buildBytes + (1 << 20)
	e1, e2, e3 := &Engine{}, &Engine{}, &Engine{}
	for _, e := range []*Engine{e1, e2, e3} {
		if err := e.SetSemanticProcessCoordinator(coordinator); err != nil {
			t.Fatal(err)
		}
	}
	if err := coordinator.reserveSourceTransient(e1); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.promoteSourceTransient(e1, 1<<20, 0); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.reserveSourceTransient(e2); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.promoteSourceTransient(e2, 1<<20, 0); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.reserveSourceTransient(e3); err == nil || !strings.Contains(err.Error(), "source 进程预算不足") {
		t.Fatalf("cumulative source cache bypassed cap: %v", err)
	}
	coordinator.releaseSourceResident(e1)
	if err := coordinator.reserveSourceTransient(e3); err != nil {
		t.Fatalf("released source capacity was not reusable: %v", err)
	}
	coordinator.releaseSourceTransient(e3)
}

func TestSemanticSourceSnapshotsEnforceCumulativeDaemonBudget(t *testing.T) {
	coordinator := NewSemanticProcessCoordinator(1024)
	engines := make([]*Engine, 3)
	for i := range engines {
		e, _ := initEngine(t, map[string]string{
			"worker.go": "package worker\n\nfunc Run() {}\n",
		})
		if err := e.SetSemanticProcessCoordinator(coordinator); err != nil {
			t.Fatal(err)
		}
		engines[i] = e
	}
	if _, _, err := engines[0].semanticSourceSnapshot(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	first := coordinator.sourceReservedBytes(engines[0])
	if first == 0 {
		t.Fatal("first manifest was not charged")
	}
	coordinator.mu.Lock()
	coordinator.maxSourceBytes = (uint64(semanticSourceBuildMaxMiB) << 20) + first
	coordinator.mu.Unlock()
	if _, _, err := engines[1].semanticSourceSnapshot(context.Background(), false); err != nil {
		t.Fatalf("second manifest should fit exactly beside build reservation: %v", err)
	}
	if _, _, err := engines[2].semanticSourceSnapshot(context.Background(), false); err == nil || !strings.Contains(err.Error(), "source 进程预算不足") {
		t.Fatalf("third repository bypassed cumulative source budget: %v", err)
	}
	engines[0].evictSemanticSourceState()
	if _, _, err := engines[2].semanticSourceSnapshot(context.Background(), false); err != nil {
		t.Fatalf("released manifest capacity was not reusable: %v", err)
	}
}

func TestSemanticSourceGateWaitIsCancellableAndDisabledReleasesCache(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"worker.go": "package worker\n\nfunc Run() {}\n"})
	coordinator := NewSemanticProcessCoordinator(1024)
	if err := e.SetSemanticProcessCoordinator(coordinator); err != nil {
		t.Fatal(err)
	}
	gate := e.semanticSourceGate()
	gate <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := e.semanticSourceSnapshot(ctx, false)
	<-gate
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("queued source build ignored cancellation: %v", err)
	}
	if _, _, err := e.semanticSourceSnapshot(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if got := coordinator.sourceReservedBytes(e); got == 0 {
		t.Fatal("published source manifest did not retain a bounded reservation")
	}
	if _, warning := e.semanticCandidates(context.Background(), "worker"); warning != "" {
		t.Fatalf("disabled semantic candidate warning=%q", warning)
	}
	if got := coordinator.sourceReservedBytes(e); got != 0 {
		t.Fatalf("disabled semantic retained source reservation=%d", got)
	}
	e.rt.mu.RLock()
	ready := e.rt.semanticManifest.ready
	e.rt.mu.RUnlock()
	if ready {
		t.Fatal("disabled semantic retained source manifest")
	}
}

func TestSemanticSourceGenerationLeasePinsManifestAndCharge(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"worker.go": "package worker\n\nfunc Run() {}\n"})
	coordinator := NewSemanticProcessCoordinator(1024)
	if err := e.SetSemanticProcessCoordinator(coordinator); err != nil {
		t.Fatal(err)
	}
	_, manifest, lease, err := e.semanticSourceSnapshotLease(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !manifest.ready || coordinator.sourceReservedBytes(e) == 0 {
		t.Fatal("published manifest was not charged")
	}
	done := make(chan struct{})
	go func() {
		e.evictSemanticSourceState()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("eviction passed an active source generation lease")
	case <-time.After(50 * time.Millisecond):
	}
	if coordinator.sourceReservedBytes(e) == 0 {
		t.Fatal("active generation was uncharged before its lease ended")
	}
	lease.Release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("eviction remained blocked after source lease release")
	}
	if got := coordinator.sourceReservedBytes(e); got != 0 {
		t.Fatalf("evicted generation retained source charge=%d", got)
	}
}

func TestSemanticDocumentLeaseSurvivesTruthInvalidation(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"worker.go": "package worker\n\nfunc Run() {}\n"})
	if _, err := e.Remember(RememberArgs{Node: "worker.go#Run", Entries: []RememberEntry{{
		Kind: model.KindSummary, Text: "worker 执行任务",
	}}}, "semantic-doc-lease", "test"); err != nil {
		t.Fatal(err)
	}
	coordinator := NewSemanticProcessCoordinator(1024)
	if err := e.SetSemanticProcessCoordinator(coordinator); err != nil {
		t.Fatal(err)
	}
	docs, _, lease, err := e.semanticSourceDocuments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) == 0 {
		t.Fatal("expected source documents")
	}
	coordinator.mu.Lock()
	residentBefore := coordinator.sourceResident[e]
	documentsBefore := coordinator.sourceDocuments[e]
	coordinator.mu.Unlock()
	if residentBefore == 0 || documentsBefore == 0 {
		t.Fatalf("missing resident/documents charge: resident=%d documents=%d", residentBefore, documentsBefore)
	}
	e.evictSemanticSourceState()
	coordinator.mu.Lock()
	residentAfter := coordinator.sourceResident[e]
	documentsAfter := coordinator.sourceDocuments[e]
	coordinator.mu.Unlock()
	if residentAfter != 0 || documentsAfter != documentsBefore {
		t.Fatalf("truth invalidation retired wrong source ownership: resident=%d documents=%d", residentAfter, documentsAfter)
	}
	docs = nil
	lease.ReleaseDocuments()
	if got := coordinator.sourceReservedBytes(e); got != 0 {
		t.Fatalf("documents release leaked source charge=%d", got)
	}
}

func TestSemanticCoordinatorCannotAttachAfterSourceWork(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"worker.go": "package worker\n\nfunc Run() {}\n"})
	if _, _, err := e.semanticSourceSnapshot(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if err := e.SetSemanticProcessCoordinator(NewSemanticProcessCoordinator(1024)); err == nil {
		t.Fatal("coordinator attached after an unaccounted source generation")
	}
}

func TestSemanticConfiguredNoIndexRecallReleasesSourceCharge(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"worker.go": "package worker\n\nfunc Run() {}\n"})
	coordinator := NewSemanticProcessCoordinator(1024)
	if err := e.SetSemanticProcessCoordinator(coordinator); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultSemanticSettings()
	cfg.Enabled = true
	cfg.Endpoint = "http://127.0.0.1:11434/v1"
	cfg.Model = "local-test"
	if err := SaveSemanticSettings(e.Store, cfg); err != nil {
		t.Fatal(err)
	}
	_, warning := e.semanticCandidates(context.Background(), "worker")
	if !strings.Contains(warning, "semantic 索引不存在") {
		t.Fatalf("configured-no-index warning=%q", warning)
	}
	if got := coordinator.sourceReservedBytes(e); got != 0 {
		t.Fatalf("configured-no-index recall retained source charge=%d", got)
	}
}

func TestSemanticStatusContextCancelsQueuedSourceBuild(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"worker.go": "package worker\n\nfunc Run() {}\n"})
	coordinator := NewSemanticProcessCoordinator(1024)
	if err := e.SetSemanticProcessCoordinator(coordinator); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultSemanticSettings()
	cfg.Enabled = true
	cfg.Endpoint = "http://127.0.0.1:11434/v1"
	cfg.Model = "local-test"
	cfg.RebuildPolicy = SemanticRebuildAILocal
	if err := SaveSemanticSettings(e.Store, cfg); err != nil {
		t.Fatal(err)
	}
	gate := e.semanticSourceGate()
	gate <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := e.StatusContext(ctx)
		done <- err
	}()
	time.Sleep(25 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("status cancellation=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("status ignored request cancellation while waiting for source gate")
	}
	<-gate
	if got := coordinator.sourceReservedBytes(e); got != 0 {
		t.Fatalf("canceled status leaked source reservation=%d", got)
	}
}

func TestSemanticProcessCoordinatorSharesProviderAndSearchGates(t *testing.T) {
	coordinator := NewSemanticProcessCoordinator(SemanticProcessResidentMaxMiB)
	e1, e2 := &Engine{}, &Engine{}
	if err := e1.SetSemanticProcessCoordinator(coordinator); err != nil {
		t.Fatal(err)
	}
	if err := e2.SetSemanticProcessCoordinator(coordinator); err != nil {
		t.Fatal(err)
	}
	if e1.semanticGate() != e2.semanticGate() || cap(e1.semanticGate()) != semanticProviderConcurrency {
		t.Fatal("multi-repo engines did not share the bounded provider gate")
	}
	if e1.semanticSearchGate() != e2.semanticSearchGate() || cap(e1.semanticSearchGate()) != semanticSearchConcurrency {
		t.Fatal("multi-repo engines did not share the bounded Flat-search gate")
	}
	if e1.semanticRebuildGate() != e2.semanticRebuildGate() || cap(e1.semanticRebuildGate()) != 1 {
		t.Fatal("multi-repo engines did not serialize bounded source/rebuild work")
	}
	if e1.semanticSourceGate() != e2.semanticSourceGate() || cap(e1.semanticSourceGate()) != 1 {
		t.Fatal("multi-repo engines did not serialize source-manifest peak memory")
	}
	if e1.semanticSourceGate() == e1.semanticRebuildGate() {
		t.Fatal("source and rebuild gates must be distinct to avoid self-deadlock")
	}
}

func TestSemanticStatusReleasesReservationBeforeResidentUnlock(t *testing.T) {
	e, cfg := semanticHealthTestEngine(t, "http://127.0.0.1:11434/v1")
	if err := SaveSemanticSettings(e.Store, cfg); err != nil {
		t.Fatal(err)
	}
	coordinator := NewSemanticProcessCoordinator(1024)
	if err := e.SetSemanticProcessCoordinator(coordinator); err != nil {
		t.Fatal(err)
	}
	meta := writeSemanticHealthIndex(t, e, cfg, "2026-07-19T12:34:56Z")
	snapshot, err := vector.Build(3, []vector.Record{{
		ID: "old", NodeID: "vault.go#Vault", Kind: semanticLaneCurrent,
		Vector: []float32{1, 0, 0},
	}})
	if err != nil {
		t.Fatal(err)
	}
	e.semantic.mu.Lock()
	e.semantic.loadedKey = "different-generation"
	e.semantic.snapshot = snapshot
	e.semantic.metadata = meta
	e.semantic.mu.Unlock()
	if err := coordinator.reserveResident(e, uint64(cfg.MaxVectorMiB)<<20); err != nil {
		t.Fatal(err)
	}

	// Force status to block exactly inside releaseResident. Correct code still
	// owns residentMu there; the old buggy ordering unlocked residentMu first,
	// allowing a concurrent reload to reserve/publish before status erased that
	// fresh reservation.
	coordinator.mu.Lock()
	healthDone := make(chan SemanticHealth, 1)
	go func() { healthDone <- e.SemanticHealthSnapshot() }()
	deadline := time.After(5 * time.Second)
	for {
		e.semantic.mu.Lock()
		cleared := e.semantic.snapshot == nil
		e.semantic.mu.Unlock()
		if cleared {
			break
		}
		select {
		case <-deadline:
			coordinator.mu.Unlock()
			t.Fatal("status did not reach resident release")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	residentAcquired := make(chan struct{})
	go func() {
		e.semantic.residentMu.Lock()
		close(residentAcquired)
		e.semantic.residentMu.Unlock()
	}()
	select {
	case <-residentAcquired:
		coordinator.mu.Unlock()
		t.Fatal("residentMu was exposed before coordinator reservation release")
	case <-time.After(50 * time.Millisecond):
	}
	coordinator.mu.Unlock()
	select {
	case health := <-healthDone:
		if health.Status != SemanticHealthReady {
			t.Fatalf("health=%+v", health)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("status did not finish after coordinator release")
	}
	select {
	case <-residentAcquired:
	case <-time.After(5 * time.Second):
		t.Fatal("resident waiter remained blocked")
	}
}

func TestSemanticProcessCoordinatorRejectsHotEnableOverBudget(t *testing.T) {
	coordinator := NewSemanticProcessCoordinator(1024)
	e1, e2, hot := &Engine{}, &Engine{}, &Engine{}
	for _, e := range []*Engine{e1, e2, hot} {
		if err := e.SetSemanticProcessCoordinator(coordinator); err != nil {
			t.Fatal(err)
		}
	}
	const half = uint64(512 << 20)
	if err := coordinator.reserveResident(e1, half); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.reserveResident(e2, half); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.reserveResident(hot, half); err == nil || !strings.Contains(err.Error(), "进程驻留预算不足") {
		t.Fatalf("hot enable/load bypassed process cap: %v", err)
	}
	coordinator.releaseResident(e1)
	if err := coordinator.reserveResident(hot, half); err != nil {
		t.Fatalf("released capacity was not reusable: %v", err)
	}
}

func TestSemanticBuildReservationPreservesOrSafelyEvictsOldGeneration(t *testing.T) {
	const half = uint64(512 << 20)
	t.Run("capacity preserves failed generation", func(t *testing.T) {
		coordinator := NewSemanticProcessCoordinator(1024)
		e := &Engine{}
		if err := e.SetSemanticProcessCoordinator(coordinator); err != nil {
			t.Fatal(err)
		}
		old := &vector.Snapshot{}
		e.semantic.snapshot = old
		if err := coordinator.reserveResident(e, half); err != nil {
			t.Fatal(err)
		}
		cfg := DefaultSemanticSettings()
		cfg.MaxVectorMiB = 512
		if err := e.beginSemanticBuild(cfg); err != nil {
			t.Fatal(err)
		}
		if e.semantic.snapshot != old || coordinator.reservedBytes(e) != 2*half {
			t.Fatal("old generation was not separately budgeted during rebuild")
		}
		e.abortSemanticBuild()
		if e.semantic.snapshot != old || coordinator.reservedBytes(e) != half {
			t.Fatal("failed rebuild did not preserve the published generation")
		}
	})

	t.Run("full process evicts cache before provider", func(t *testing.T) {
		coordinator := NewSemanticProcessCoordinator(1024)
		e, other := &Engine{}, &Engine{}
		if err := e.SetSemanticProcessCoordinator(coordinator); err != nil {
			t.Fatal(err)
		}
		if err := other.SetSemanticProcessCoordinator(coordinator); err != nil {
			t.Fatal(err)
		}
		e.semantic.snapshot = &vector.Snapshot{}
		if err := coordinator.reserveResident(e, half); err != nil {
			t.Fatal(err)
		}
		if err := coordinator.reserveResident(other, half); err != nil {
			t.Fatal(err)
		}
		cfg := DefaultSemanticSettings()
		cfg.MaxVectorMiB = 512
		if err := e.beginSemanticBuild(cfg); err != nil {
			t.Fatal(err)
		}
		if e.semantic.snapshot != nil || coordinator.reservedBytes(e) != half {
			t.Fatal("full process kept old+new matrices beyond the hard cap")
		}
		e.abortSemanticBuild()
		if coordinator.reservedBytes(e) != 0 {
			t.Fatal("failed fallback rebuild leaked its transient reservation")
		}
	})
}

func TestSemanticIncrementalReservationPromotesWithoutDoubleCounting(t *testing.T) {
	const (
		oldResident = uint64(12 << 20)
		newMatrix   = uint64(13 << 20)
		oldDecoded  = uint64(12 << 20)
		otherRepo   = uint64(512 << 20)
	)
	coordinator := NewSemanticProcessCoordinator(1024)
	e, other := &Engine{}, &Engine{}
	if err := e.SetSemanticProcessCoordinator(coordinator); err != nil {
		t.Fatal(err)
	}
	if err := other.SetSemanticProcessCoordinator(coordinator); err != nil {
		t.Fatal(err)
	}
	e.semantic.snapshot = &vector.Snapshot{}
	if err := coordinator.reserveResident(e, oldResident); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.reserveResident(other, otherRepo); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultSemanticSettings()
	cfg.MaxVectorMiB = 512
	if err := e.beginSemanticBuildReservationContext(context.Background(), cfg, newMatrix, oldDecoded); err != nil {
		t.Fatal(err)
	}
	if e.semantic.snapshot == nil {
		t.Fatal("exact incremental reservation unnecessarily evicted the old resident")
	}
	if got, want := coordinator.reservedBytes(e), oldResident+newMatrix+oldDecoded; got != want {
		t.Fatalf("incremental reservation=%d, want %d", got, want)
	}
	if err := coordinator.promoteTransient(e, newMatrix+1); err == nil {
		t.Fatal("promotion larger than new-matrix authorization succeeded")
	}
	if got, want := coordinator.reservedBytes(e), oldResident+newMatrix+oldDecoded; got != want {
		t.Fatalf("failed promotion changed reservation=%d, want %d", got, want)
	}
	if err := coordinator.promoteTransient(e, newMatrix); err != nil {
		t.Fatal(err)
	}
	if got := coordinator.reservedBytes(e); got != newMatrix {
		t.Fatalf("promoted reservation=%d, want %d", got, newMatrix)
	}
	e.semantic.mu.Lock()
	e.semantic.building = false
	e.semantic.mu.Unlock()
}

func TestSemanticNextActionNeverSuggestsImpossibleInteractiveSync(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"x.go": "package sample\nfunc X() {}\n"})
	if got := e.semanticRebuildNextAction(string(SemanticRebuildAILocal), semanticMCPSyncMaxRecords); got != "kb_semantic action=sync" {
		t.Fatalf("within-limit action=%q", got)
	}
	got := e.semanticRebuildNextAction(string(SemanticRebuildAIRemote), semanticMCPSyncMaxRecords+1)
	if !strings.HasPrefix(got, "iknowledge semantic rebuild --repo ") {
		t.Fatalf("oversized source still suggested MCP sync: %q", got)
	}
	if detail := semanticSyncLimitDetail(semanticMCPSyncMaxRecords + 1); !strings.Contains(detail, "唯一 sync 尝试") {
		t.Fatalf("oversized detail=%q", detail)
	}
	old := semanticIndexInspection{meta: semanticIndexMetadata{Records: 100, Dimensions: 3}, vectorBytes: 100 * 3 * 4}
	cfg := DefaultSemanticSettings()
	if got := e.semanticIncrementalNextAction(string(SemanticRebuildAILocal), cfg, old, 4000); !strings.HasPrefix(got, "iknowledge semantic rebuild --repo ") {
		t.Fatalf("known minimum delta over limit still suggested MCP sync: %q", got)
	}
}

func TestSemanticHealthSyncTooLargeClassificationIsStructured(t *testing.T) {
	cli := "iknowledge semantic rebuild --repo /tmp/repo"
	for _, test := range []struct {
		name    string
		health  SemanticHealth
		records int
		want    bool
	}{
		{name: "ai local corrupt", health: SemanticHealth{Status: SemanticHealthCorrupt, Policy: string(SemanticRebuildAILocal), NextAction: cli}, records: semanticMCPSyncMaxRecords + 1, want: true},
		{name: "ai remote stale provider", health: SemanticHealth{Status: SemanticHealthStaleProvider, Policy: string(SemanticRebuildAIRemote), NextAction: cli}, records: semanticMCPSyncMaxRecords + 1, want: true},
		{name: "exact boundary", health: SemanticHealth{Policy: string(SemanticRebuildAILocal), NextAction: cli}, records: semanticMCPSyncMaxRecords},
		{name: "retry unstable generation", health: SemanticHealth{Policy: string(SemanticRebuildAILocal), NextAction: "retry kb_status"}, records: semanticMCPSyncMaxRecords + 1},
		{name: "actionable delta", health: SemanticHealth{Policy: string(SemanticRebuildAILocal), NextAction: "kb_semantic action=sync"}, records: semanticMCPSyncMaxRecords + 1},
		{name: "manual policy", health: SemanticHealth{Policy: string(SemanticRebuildManual), NextAction: cli}, records: semanticMCPSyncMaxRecords + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := semanticHealthSyncTooLarge(test.health, test.records); got != test.want {
				t.Fatalf("classification=%v, want %v: %+v records=%d", got, test.want, test.health, test.records)
			}
		})
	}
}
