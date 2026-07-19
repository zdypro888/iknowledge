package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zdypro888/iknowledge/internal/model"
	"github.com/zdypro888/iknowledge/internal/semantic"
	"github.com/zdypro888/iknowledge/internal/store"
	"github.com/zdypro888/iknowledge/internal/vector"
)

const (
	semanticOnlyTestQuery = "qzvplmno"
	semanticTargetMarker  = "orchidmarker"
	semanticOtherMarker   = "cobaltmarker"
)

type semanticHTTPTestProvider struct {
	server           *httptest.Server
	requests         atomic.Int64
	fail             atomic.Bool
	authSeen         atomic.Bool
	blockQuery       atomic.Bool
	blockRebuild     atomic.Bool
	driftBatchCanary atomic.Bool
	driftedCanaries  atomic.Int64
	queryEntered     chan struct{}
	queryRelease     chan struct{}
	rebuildEntered   chan struct{}
	rebuildRelease   chan struct{}
}

// observedDoneContext makes it deterministic that a goroutine reached a
// context-aware select. Embedding Background keeps the underlying Done nil,
// so observing this hook while the rebuild gate is occupied proves the caller
// is queued at that gate rather than merely not yet scheduled.
type observedDoneContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

func (c *observedDoneContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.Context.Done()
}

type blockingQueryEmbedder struct {
	calls   atomic.Int64
	entered chan struct{}
	release chan struct{}
	err     error
}

func (b *blockingQueryEmbedder) Fingerprint() string { return "blocking-query-v1" }
func (b *blockingQueryEmbedder) EmbedDocuments(context.Context, []string) ([][]float32, error) {
	return nil, nil
}
func (b *blockingQueryEmbedder) EmbedQuery(ctx context.Context, _ string) ([]float32, error) {
	return b.blockQuery(ctx)
}
func (b *blockingQueryEmbedder) EmbedQueryWithCanary(ctx context.Context, _, _ string) ([]float32, []float32, error) {
	values, err := b.blockQuery(ctx)
	if err != nil {
		return nil, nil, err
	}
	return values, []float32{0, 0, 1}, nil
}
func (b *blockingQueryEmbedder) EmbedDocumentsWithCanary(_ context.Context, documents []string, _ string) ([][]float32, []float32, error) {
	vectors := make([][]float32, len(documents))
	for i := range vectors {
		vectors[i] = []float32{1, 0, 0}
	}
	return vectors, []float32{0, 0, 1}, nil
}
func (b *blockingQueryEmbedder) blockQuery(ctx context.Context) ([]float32, error) {
	b.calls.Add(1)
	select {
	case b.entered <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
		if b.err != nil {
			return nil, b.err
		}
		return []float32{1, 0, 0}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

var _ semantic.CanaryEmbedder = (*blockingQueryEmbedder)(nil)

func TestSemanticQueryFailureBackoffCollapsesQueuedMisses(t *testing.T) {
	e := &Engine{}
	embedder := &blockingQueryEmbedder{
		entered: make(chan struct{}, 1), release: make(chan struct{}), err: errors.New("provider down"),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const callers = 12
	start := make(chan struct{})
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, err := e.cachedSemanticQuery(ctx, embedder, "same-failing-query")
			errs <- err
		}()
	}
	close(start)
	select {
	case <-embedder.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	close(embedder.release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err == nil {
			t.Fatal("queued query unexpectedly succeeded")
		}
	}
	if got := embedder.calls.Load(); got != 1 {
		t.Fatalf("queued failing-query provider calls=%d, want 1", got)
	}
}

func TestSemanticLoadRejectsStaleMetadataBeforeVectorPayload(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"x.go": "package sample\nfunc X() {}\n"})
	cfg := DefaultSemanticSettings()
	cfg.Enabled, cfg.Endpoint, cfg.Model, cfg.Dimensions = true, "https://embed.example/v1", "m", 3
	source := sha256.Sum256([]byte("current-source"))
	meta := semanticIndexMetadata{
		Schema: 1, Generation: strings.Repeat("a", 32),
		SettingsFingerprint: "stale-settings", EmbedderFingerprint: "stale-embedder",
		ProbeFingerprint: "probe", QueryProbeFingerprint: "query-probe", SourceFingerprint: hex.EncodeToString(source[:]),
		Dimensions: 3, Records: 1, BuiltAt: time.Now().UTC().Format(time.RFC3339),
	}
	metadata, checksum, err := marshalSemanticIndexMetadata(meta)
	if err != nil {
		t.Fatal(err)
	}
	var header [semanticWrapperSize]byte
	copy(header[:8], semanticIndexMagic[:])
	binary.LittleEndian.PutUint16(header[8:10], semanticIndexVersion)
	binary.LittleEndian.PutUint32(header[12:16], uint32(len(metadata)))
	copy(header[16:48], checksum[:])
	// 故意只放 32 个伪 footer，不含可解码 vector payload。若实现先解码
	// payload，错误会来自 vector；正确路径应先报告 metadata binding stale。
	malformed := append(header[:], metadata...)
	malformed = append(malformed, make([]byte, 32)...)
	if err := e.Store.WritePrivateKnowledgeFile(semanticIndexRel, malformed); err != nil {
		t.Fatal(err)
	}
	_, _, _, err = e.loadSemanticIndex(context.Background(), cfg, source)
	if err == nil || !strings.Contains(err.Error(), "provider 配置已变化") {
		t.Fatalf("stale metadata should win before payload decode, err=%v", err)
	}
}

func TestSemanticQueryCacheCollapsesConcurrentMisses(t *testing.T) {
	e := &Engine{}
	embedder := &blockingQueryEmbedder{entered: make(chan struct{}, 1), release: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const callers = 24
	start := make(chan struct{})
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			values, _, err := e.cachedSemanticQuery(ctx, embedder, "same-query")
			if err == nil && len(values) != 3 {
				err = fmt.Errorf("dimensions=%d", len(values))
			}
			errs <- err
		}()
	}
	close(start)
	select {
	case <-embedder.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	close(embedder.release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := embedder.calls.Load(); got != 1 {
		t.Fatalf("concurrent same-query provider calls=%d, want 1", got)
	}
}

func TestSearchSemanticSnapshotFiltersBeforeDistinctTopK(t *testing.T) {
	staleHash := sha256.Sum256([]byte("stale"))
	validAHash := sha256.Sum256([]byte("valid-a"))
	validBHash := sha256.Sum256([]byte("valid-b"))
	snapshot, err := vector.Build(2, []vector.Record{
		{ID: "stale-a", NodeID: "a.go#A", Kind: semanticLaneCurrent, SourceHash: staleHash, Vector: []float32{1, 0}},
		{ID: "valid-a", NodeID: "a.go#A", Kind: semanticLaneCurrent, SourceHash: validAHash, Vector: []float32{0.9, 0.1}},
		{ID: "valid-b", NodeID: "b.go#B", Kind: semanticLaneCurrent, SourceHash: validBHash, Vector: []float32{0.8, 0.2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := semanticSourceManifest{ready: true, records: map[string]semanticSourceRecord{
		"valid-a": {NodeID: "a.go#A", Kind: semanticLaneCurrent, SourceHash: validAHash},
		"valid-b": {NodeID: "b.go#B", Kind: semanticLaneCurrent, SourceHash: validBHash},
	}}
	e := &Engine{}
	byLane, err := e.searchSemanticSnapshot(context.Background(), snapshot, []float32{1, 0}, 1, manifest)
	if err != nil {
		t.Fatal(err)
	}
	current := byLane[semanticLaneCurrent]
	if len(current) != 1 || current[0].ID != "valid-a" {
		t.Fatalf("stale winner displaced valid backfill: %+v", current)
	}
}

func TestSemanticAuthorizedRebuildRejectsChangedConfigBeforeProvider(t *testing.T) {
	provider := newSemanticHTTPTestProvider(t)
	e, _ := initEngine(t, map[string]string{"vault.go": "package sample\n\nfunc Vault() {}\n"})
	cfg := DefaultSemanticSettings()
	cfg.Enabled, cfg.Endpoint, cfg.Model, cfg.Dimensions, cfg.TimeoutSec =
		true, provider.server.URL, "integration-embed", 3, 2
	cfg.RebuildPolicy = SemanticRebuildAILocal
	if err := SaveSemanticSettings(e.Store, cfg); err != nil {
		t.Fatal(err)
	}
	authorized := cfg
	cfg.RebuildPolicy = SemanticRebuildManual
	if err := SaveSemanticSettings(e.Store, cfg); err != nil {
		t.Fatal(err)
	}

	_, err := e.rebuildSemantic(context.Background(), &authorized)
	var kbe *KBError
	if !errors.As(err, &kbe) || kbe.Code != "SEMANTIC_AUTHORIZATION_CHANGED" {
		t.Fatalf("changed authorization err=%v", err)
	}
	if got := provider.requests.Load(); got != 0 {
		t.Fatalf("changed authorization contacted provider %d times", got)
	}
}

func TestSemanticConfigCannotChangeAcrossInFlightProviderRequest(t *testing.T) {
	provider := newSemanticHTTPTestProvider(t)
	e, _ := initEngine(t, map[string]string{"vault.go": "package sample\n\nfunc Vault() {}\n"})
	if _, err := e.Remember(RememberArgs{Node: "vault.go#Vault", Entries: []RememberEntry{{
		Kind: "summary", Text: semanticTargetMarker + " authorization boundary",
	}}}, "semantic-auth", "test"); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultSemanticSettings()
	cfg.Enabled, cfg.Endpoint, cfg.Model, cfg.Dimensions, cfg.TimeoutSec =
		true, provider.server.URL, "integration-embed", 3, 2
	if err := SaveSemanticSettings(e.Store, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RebuildSemantic(context.Background()); err != nil {
		t.Fatal(err)
	}

	provider.blockQuery.Store(true)
	type recallResult struct {
		out  string
		meta ReadMeta
		err  error
	}
	done := make(chan recallResult, 1)
	go func() {
		out, meta, err := e.RecallContext(context.Background(), RecallArgs{Query: semanticOnlyTestQuery}, "semantic-auth")
		done <- recallResult{out: out, meta: meta, err: err}
	}()
	select {
	case <-provider.queryEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("provider query did not start")
	}
	disabled := cfg
	disabled.Enabled = false
	if err := SaveSemanticSettings(e.Store, disabled); !errors.Is(err, store.ErrSemanticConfigLocked) {
		t.Fatalf("config changed while provider request was in flight: %v", err)
	}
	close(provider.queryRelease)
	result := <-done
	if result.err != nil || !result.meta.Hit || !strings.Contains(result.out, "vault.go#Vault") {
		t.Fatalf("authorized in-flight query result hit=%v err=%v out=%s", result.meta.Hit, result.err, result.out)
	}
	if err := SaveSemanticSettings(e.Store, disabled); err != nil {
		t.Fatalf("disable after provider request completed: %v", err)
	}
	requestsAfterDisable := provider.requests.Load()
	if _, _, err := e.RecallContext(context.Background(), RecallArgs{Query: semanticOnlyTestQuery + "-disabled"}, "semantic-auth"); err != nil {
		t.Fatal(err)
	}
	if got := provider.requests.Load(); got != requestsAfterDisable {
		t.Fatalf("disabled semantic contacted provider: before=%d after=%d", requestsAfterDisable, got)
	}
}

func TestSemanticRemoteCredentialIsBoundToOneOrigin(t *testing.T) {
	t.Setenv(SemanticAPIKeyEnv, "credential-must-not-cross-origin")
	t.Setenv(SemanticAPIOriginEnv, "https://provider-a.example.com:443/")
	cfg := DefaultSemanticSettings()
	cfg.Enabled, cfg.Endpoint, cfg.Model, cfg.Dimensions =
		true, "https://provider-a.example.com/v1", "remote-embed", 3
	if _, err := newSemanticEmbedder(cfg); err != nil {
		t.Fatalf("matching credential origin rejected: %v", err)
	}

	otherRepoCfg := cfg
	otherRepoCfg.Endpoint = "https://provider-b.example.com/v1"
	if _, err := newSemanticEmbedder(otherRepoCfg); err == nil {
		t.Fatal("a process credential was reusable at another repo's remote origin")
	} else if strings.Contains(err.Error(), "credential-must-not-cross-origin") ||
		strings.Contains(err.Error(), "https://provider-a.example.com") {
		t.Fatalf("credential binding error leaked secret/raw audience: %v", err)
	}

	t.Setenv(SemanticAPIOriginEnv, "")
	if _, err := newSemanticEmbedder(cfg); err == nil || !strings.Contains(err.Error(), SemanticAPIOriginEnv) {
		t.Fatalf("remote key without audience binding error=%v", err)
	}

	// Loopback never sends the ambient remote key, so a missing/malformed
	// remote audience must not make local Ollama unusable.
	t.Setenv(SemanticAPIOriginEnv, "not-an-origin")
	localCfg := cfg
	localCfg.Endpoint = "http://127.0.0.1:11434/v1"
	if _, err := newSemanticEmbedder(localCfg); err != nil {
		t.Fatalf("loopback embedder consulted remote credential binding: %v", err)
	}
}

func TestSemanticBatchRechecksAuthorizationAfterProviderQueue(t *testing.T) {
	provider := newSemanticHTTPTestProvider(t)
	e, _ := initEngine(t, map[string]string{"vault.go": "package sample\n\nfunc Vault() {}\n"})
	cfg := DefaultSemanticSettings()
	cfg.Enabled, cfg.Endpoint, cfg.Model, cfg.Dimensions, cfg.TimeoutSec =
		true, provider.server.URL, "integration-embed", 3, 2
	if err := SaveSemanticSettings(e.Store, cfg); err != nil {
		t.Fatal(err)
	}
	embedder, err := newSemanticEmbedder(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// Occupy the one-slot provider gate without taking the config read lock.
	// The real rebuild batch queues behind it with the old authorization.
	holder := &blockingQueryEmbedder{entered: make(chan struct{}, 1), release: make(chan struct{})}
	holderDone := make(chan error, 1)
	go func() {
		_, _, err := e.cachedSemanticQuery(context.Background(), holder, "hold-provider-gate")
		holderDone <- err
	}()
	select {
	case <-holder.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("provider gate holder did not start")
	}
	batchDone := make(chan error, 1)
	go func() {
		_, _, _, err := e.embedSemanticDocumentsDualCanary(context.Background(), embedder, nil,
			func() error { return e.validateSemanticProviderSettings(cfg) })
		batchDone <- err
	}()

	disabled := cfg
	disabled.Enabled = false
	if err := SaveSemanticSettings(e.Store, disabled); err != nil {
		t.Fatalf("disable while batch waits only for provider slot: %v", err)
	}
	close(holder.release)
	if err := <-holderDone; err != nil {
		t.Fatal(err)
	}
	if err := <-batchDone; err == nil || !strings.Contains(err.Error(), "配置已变化") {
		t.Fatalf("queued batch did not recheck authorization: %v", err)
	}
	if got := provider.requests.Load(); got != 0 {
		t.Fatalf("revoked queued batch contacted provider %d times", got)
	}
}

func TestSemanticSyncCoalescesConcurrentSessionsAndWaitIsCancelable(t *testing.T) {
	provider := newSemanticHTTPTestProvider(t)
	e, _ := initEngine(t, map[string]string{"vault.go": "package sample\n\nfunc Vault() {}\n"})
	if _, err := e.Remember(RememberArgs{Node: "vault.go#Vault", Entries: []RememberEntry{{
		Kind: "summary", Text: semanticTargetMarker + " coalesced rebuild",
	}}}, "semantic-sync-gate", "test"); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultSemanticSettings()
	cfg.Enabled, cfg.Endpoint, cfg.Model, cfg.Dimensions, cfg.TimeoutSec =
		true, provider.server.URL, "integration-embed", 3, 2
	cfg.RebuildPolicy = SemanticRebuildAILocal
	if err := SaveSemanticSettings(e.Store, cfg); err != nil {
		t.Fatal(err)
	}

	type syncResult struct {
		text string
		err  error
	}
	provider.blockRebuild.Store(true)
	ownerDone := make(chan syncResult, 1)
	go func() {
		text, err := e.SyncSemantic(context.Background())
		ownerDone <- syncResult{text: text, err: err}
	}()
	select {
	case <-provider.rebuildEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("owner sync did not reach the initial provider probe")
	}

	// A canceled session must leave the in-process rebuild queue immediately;
	// it must not wait for the owner's provider timeout or publication.
	cancelBase, cancel := context.WithCancel(context.Background())
	cancelObserved := &observedDoneContext{Context: cancelBase, observed: make(chan struct{})}
	canceledDone := make(chan error, 1)
	go func() {
		_, err := e.SyncSemantic(cancelObserved)
		canceledDone <- err
	}()
	select {
	case <-cancelObserved.observed:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled session did not reach context-aware rebuild wait")
	}
	cancel()
	select {
	case err := <-canceledDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled waiter error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiter remained blocked behind rebuild owner")
	}

	// A second live MCP session joins the same flight while no generation is yet
	// published. It receives the owner's report without another paid generation.
	waiterCtx := &observedDoneContext{Context: context.Background(), observed: make(chan struct{})}
	waiterDone := make(chan syncResult, 1)
	go func() {
		text, err := e.SyncSemantic(waiterCtx)
		waiterDone <- syncResult{text: text, err: err}
	}()
	select {
	case <-waiterCtx.observed:
	case <-time.After(5 * time.Second):
		t.Fatal("second session did not reach rebuild gate")
	}
	close(provider.rebuildRelease)

	owner := <-ownerDone
	if owner.err != nil || !strings.Contains(owner.text, "semantic 索引已重建") {
		t.Fatalf("owner result text=%q err=%v", owner.text, owner.err)
	}
	waiter := <-waiterDone
	if waiter.err != nil || !strings.Contains(waiter.text, "semantic 索引已重建") {
		t.Fatalf("waiter result text=%q err=%v", waiter.text, waiter.err)
	}
	if got := provider.requests.Load(); got != 2 {
		t.Fatalf("coalesced sync provider requests=%d, want one probe + one document batch", got)
	}

	// Explicit CLI/API rebuild is intentionally forceful even when ready.
	requestsBeforeForce := provider.requests.Load()
	if _, err := e.RebuildSemantic(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := provider.requests.Load() - requestsBeforeForce; got != 2 {
		t.Fatalf("forced rebuild requests=%d, want probe + document batch", got)
	}
}

func TestSemanticSyncLiveWaiterTakesOverCanceledOwner(t *testing.T) {
	provider := newSemanticHTTPTestProvider(t)
	e, _ := initEngine(t, map[string]string{"vault.go": "package sample\n\nfunc Vault() {}\n"})
	if _, err := e.Remember(RememberArgs{Node: "vault.go#Vault", Entries: []RememberEntry{{
		Kind: "summary", Text: semanticTargetMarker + " owner cancellation handoff",
	}}}, "semantic-sync-handoff", "test"); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultSemanticSettings()
	cfg.Enabled, cfg.Endpoint, cfg.Model, cfg.Dimensions, cfg.TimeoutSec =
		true, provider.server.URL, "integration-embed", 3, 2
	cfg.RebuildPolicy = SemanticRebuildAILocal
	if err := SaveSemanticSettings(e.Store, cfg); err != nil {
		t.Fatal(err)
	}

	provider.blockRebuild.Store(true)
	ownerCtx, cancelOwner := context.WithCancel(context.Background())
	ownerDone := make(chan error, 1)
	go func() {
		_, err := e.SyncSemantic(ownerCtx)
		ownerDone <- err
	}()
	select {
	case <-provider.rebuildEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("owner did not reach provider")
	}
	waiterCtx := &observedDoneContext{Context: context.Background(), observed: make(chan struct{})}
	waiterDone := make(chan error, 1)
	go func() {
		_, err := e.SyncSemantic(waiterCtx)
		waiterDone <- err
	}()
	select {
	case <-waiterCtx.observed:
	case <-time.After(5 * time.Second):
		t.Fatal("live waiter did not join owner flight")
	}
	cancelOwner()
	if err := <-ownerDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("owner cancellation=%v", err)
	}
	// The live waiter must elect itself and issue a replacement probe while the
	// provider remains blocked. Inheriting the canceled owner's result would
	// finish before this second entry.
	select {
	case <-provider.rebuildEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("live waiter did not take over canceled owner")
	}
	close(provider.rebuildRelease)
	if err := <-waiterDone; err != nil {
		t.Fatalf("takeover waiter failed: %v", err)
	}
	if got := provider.requests.Load(); got != 3 {
		t.Fatalf("handoff provider requests=%d, want canceled probe + replacement probe + batch", got)
	}
}

func TestSemanticSyncCoalescesConcurrentProviderFailure(t *testing.T) {
	provider := newSemanticHTTPTestProvider(t)
	e, _ := initEngine(t, map[string]string{"vault.go": "package sample\n\nfunc Vault() {}\n"})
	if _, err := e.Remember(RememberArgs{Node: "vault.go#Vault", Entries: []RememberEntry{{
		Kind: "summary", Text: semanticTargetMarker + " failed singleflight",
	}}}, "semantic-sync-failure", "test"); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultSemanticSettings()
	cfg.Enabled, cfg.Endpoint, cfg.Model, cfg.Dimensions, cfg.TimeoutSec =
		true, provider.server.URL, "integration-embed", 3, 2
	cfg.RebuildPolicy = SemanticRebuildAILocal
	if err := SaveSemanticSettings(e.Store, cfg); err != nil {
		t.Fatal(err)
	}

	provider.blockRebuild.Store(true)
	const callers = 8
	errs := make(chan error, callers)
	go func() {
		_, err := e.SyncSemantic(context.Background())
		errs <- err
	}()
	select {
	case <-provider.rebuildEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("owner sync did not reach provider")
	}
	observed := make([]chan struct{}, 0, callers-1)
	for range callers - 1 {
		ctx := &observedDoneContext{Context: context.Background(), observed: make(chan struct{})}
		observed = append(observed, ctx.observed)
		go func() {
			_, err := e.SyncSemantic(ctx)
			errs <- err
		}()
	}
	for _, entered := range observed {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("waiter did not join sync flight")
		}
	}
	provider.fail.Store(true)
	close(provider.rebuildRelease)
	for range callers {
		if err := <-errs; err == nil || !strings.Contains(err.Error(), "503") {
			t.Fatalf("shared provider failure=%v", err)
		}
	}
	if got := provider.requests.Load(); got != 1 {
		t.Fatalf("concurrent failed sync provider requests=%d, want 1", got)
	}
	// The short failure generation also collapses a near-simultaneous arrival
	// burst rather than charging another request per MCP session.
	if _, err := e.SyncSemantic(context.Background()); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("retained failure=%v", err)
	}
	if got := provider.requests.Load(); got != 1 {
		t.Fatalf("retained failure contacted provider: %d", got)
	}
}

func TestSemanticSyncDeadlineStopsProviderAndDoesNotPublish(t *testing.T) {
	provider := newSemanticHTTPTestProvider(t)
	e, _ := initEngine(t, map[string]string{"vault.go": "package sample\n\nfunc Vault() {}\n"})
	if _, err := e.Remember(RememberArgs{Node: "vault.go#Vault", Entries: []RememberEntry{{
		Kind: "summary", Text: semanticTargetMarker + " deadline cancellation",
	}}}, "semantic-sync-timeout", "test"); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultSemanticSettings()
	cfg.Enabled, cfg.Endpoint, cfg.Model, cfg.Dimensions, cfg.TimeoutSec =
		true, provider.server.URL, "integration-embed", 3, 2
	cfg.RebuildPolicy = SemanticRebuildAILocal
	if err := SaveSemanticSettings(e.Store, cfg); err != nil {
		t.Fatal(err)
	}

	provider.blockRebuild.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := e.SyncSemantic(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("slow MCP sync error=%v, want deadline exceeded", err)
	}
	if got := provider.requests.Load(); got != 1 {
		t.Fatalf("timed-out sync continued provider batches: requests=%d, want initial probe only", got)
	}
	if _, err := e.Store.OpenKnowledgeFileRead(semanticIndexRel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("timed-out sync published an index: %v", err)
	}
	if health := e.SemanticHealthSnapshot(); health.Status != SemanticHealthConfiguredNoIndex {
		t.Fatalf("timed-out sync health=%+v", health)
	}
}

func TestSemanticSyncOversizedSourceFailsBeforeProvider(t *testing.T) {
	provider := newSemanticHTTPTestProvider(t)
	e, _ := initEngine(t, map[string]string{"vault.go": "package sample\n\nfunc Vault() {}\n"})
	cfg := DefaultSemanticSettings()
	cfg.Enabled, cfg.Endpoint, cfg.Model, cfg.Dimensions, cfg.TimeoutSec =
		true, provider.server.URL, "integration-embed", 3, 2
	cfg.RebuildPolicy = SemanticRebuildAILocal
	if err := SaveSemanticSettings(e.Store, cfg); err != nil {
		t.Fatal(err)
	}

	// Persist enough valid knowledge to produce one near-3KiB card per entry.
	// This exercises the real source builder and proves the interactive limit is
	// checked before even the initial provider canary request.
	shardPath := e.Store.ShardPathFor("vault.go")
	shard, _, err := e.Store.LoadShard(shardPath)
	if err != nil {
		t.Fatal(err)
	}
	var target *model.Node
	for i := range shard.Nodes {
		if shard.Nodes[i].ID == "vault.go#Vault" {
			target = &shard.Nodes[i]
			break
		}
	}
	if target == nil {
		t.Fatal("missing vault function node")
	}
	target.Entries = make([]model.Entry, semanticMCPSyncMaxRecords+1)
	cardText := strings.Repeat("x", semanticCardRawTarget)
	for i := range target.Entries {
		target.Entries[i] = model.Entry{
			ID: fmt.Sprintf("e_%08x", i), Kind: model.KindSummary,
			Text: cardText, Confidence: model.ConfidenceVerified,
		}
	}
	if err := e.Store.SaveShard(shardPath, shard, nil); err != nil {
		t.Fatal(err)
	}

	_, err = e.SyncSemantic(context.Background())
	var kb *KBError
	if !errors.As(err, &kb) || kb.Code != "SEMANTIC_SYNC_TOO_LARGE" {
		t.Fatalf("oversized MCP sync error=%v", err)
	}
	if got := provider.requests.Load(); got != 0 {
		t.Fatalf("oversized MCP sync contacted provider %d times", got)
	}
	if _, err := e.Store.OpenKnowledgeFileRead(semanticIndexRel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oversized MCP sync published an index: %v", err)
	}
}

func newSemanticHTTPTestProvider(t *testing.T) *semanticHTTPTestProvider {
	t.Helper()
	provider := &semanticHTTPTestProvider{
		queryEntered:   make(chan struct{}, 1),
		queryRelease:   make(chan struct{}),
		rebuildEntered: make(chan struct{}, 1),
		rebuildRelease: make(chan struct{}),
	}
	provider.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provider.requests.Add(1)
		if r.Header.Get("Authorization") != "" {
			provider.authSeen.Store(true)
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/embeddings" {
			http.Error(w, "unexpected endpoint", http.StatusNotFound)
			return
		}
		if provider.fail.Load() {
			http.Error(w, "provider unavailable", http.StatusServiceUnavailable)
			return
		}
		var request struct {
			Model      string   `json:"model"`
			Input      []string `json:"input"`
			Dimensions int      `json:"dimensions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if request.Model != "integration-embed" || request.Dimensions != 3 || len(request.Input) == 0 {
			http.Error(w, "invalid embedding options", http.StatusBadRequest)
			return
		}
		blockingQuery := false
		for _, text := range request.Input {
			blockingQuery = blockingQuery || text == semanticOnlyTestQuery
		}
		// Rebuild requests end in two canaries: unmodified document mode,
		// followed by query mode. An initial probe has no leading documents;
		// a real batch does. Indexing rather than matching text also works when
		// both plain-profile canaries have identical input text.
		documentCanaryIndex := -1
		if len(request.Input) >= 2 && request.Input[len(request.Input)-2] == semanticProbeText {
			documentCanaryIndex = len(request.Input) - 2
		}
		documentBatch := documentCanaryIndex > 0
		initialRebuildProbe := len(request.Input) == 2 && request.Input[0] == semanticProbeText && request.Input[1] == semanticProbeText
		if provider.blockRebuild.Load() && initialRebuildProbe {
			select {
			case provider.rebuildEntered <- struct{}{}:
			default:
			}
			select {
			case <-provider.rebuildRelease:
			case <-r.Context().Done():
				return
			}
		}
		if provider.fail.Load() {
			http.Error(w, "provider unavailable", http.StatusServiceUnavailable)
			return
		}
		if provider.blockQuery.Load() && blockingQuery {
			select {
			case provider.queryEntered <- struct{}{}:
			default:
			}
			select {
			case <-provider.queryRelease:
			case <-r.Context().Done():
				return
			}
		}
		data := make([]map[string]any, len(request.Input))
		for i, text := range request.Input {
			values := []float64{0, 1, 0}
			switch {
			case text == semanticProbeText:
				values = []float64{0, 0, 1}
				if provider.driftBatchCanary.Load() && documentBatch && i == documentCanaryIndex {
					values = []float64{0, 1, 0}
					provider.driftedCanaries.Add(1)
				}
			case text == semanticOnlyTestQuery || strings.Contains(text, semanticTargetMarker):
				values = []float64{1, 0, 0}
			case strings.Contains(text, semanticOtherMarker):
				values = []float64{0, 1, 0}
			}
			data[i] = map[string]any{"index": i, "embedding": values}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(provider.server.Close)
	return provider
}

func semanticTestIndexIdentity(t *testing.T, e *Engine) semanticFileIdentity {
	t.Helper()
	f, err := e.Store.OpenKnowledgeFileRead(semanticIndexRel)
	if err != nil {
		t.Fatal(err)
	}
	identity, identityErr := readSemanticFileIdentity(context.Background(), f)
	closeErr := f.Close()
	if identityErr != nil || closeErr != nil {
		t.Fatal(errors.Join(identityErr, closeErr))
	}
	return identity
}

func TestSemanticRuntimeExplicitRebuildHybridAndFallback(t *testing.T) {
	provider := newSemanticHTTPTestProvider(t)
	t.Setenv(SemanticAPIKeyEnv, "must-not-be-sent-to-loopback")
	e, _ := initEngine(t, map[string]string{
		"vault.go": "package sample\n\nfunc Vault() {}\n",
		"queue.go": "package sample\n\nfunc Queue() {}\n",
	})
	if _, err := e.Remember(RememberArgs{
		Node:     "vault.go#Vault",
		Entries:  []RememberEntry{{Kind: "summary", Text: semanticTargetMarker + " 描述凭据保险库的访问约束"}},
		Keywords: []string{"lexicalfallback"},
	}, "semantic-test", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Remember(RememberArgs{
		Node:    "queue.go#Queue",
		Entries: []RememberEntry{{Kind: "summary", Text: semanticOtherMarker + " 描述后台队列的批处理约束"}},
	}, "semantic-test", "test"); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultSemanticSettings()
	cfg.Enabled = true
	cfg.Endpoint = provider.server.URL
	cfg.Model = "integration-embed"
	cfg.Dimensions = 3
	cfg.Revision = "integration-v1"
	cfg.MinScore = 0.5
	cfg.TimeoutSec = 2
	if err := SaveSemanticSettings(e.Store, cfg); err != nil {
		t.Fatal(err)
	}

	// Enabled does not imply an automatic rebuild. A missing cache degrades
	// locally and must not contact the provider.
	before, meta, err := e.RecallContext(context.Background(), RecallArgs{Query: semanticOnlyTestQuery}, "semantic-test")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Hit || !strings.Contains(before, "semantic 已降级") || provider.requests.Load() != 0 {
		t.Fatalf("pre-rebuild recall hit=%v requests=%d output:\n%s", meta.Hit, provider.requests.Load(), before)
	}

	report, err := e.RebuildSemantic(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Records != 2 || report.Dimensions != 3 {
		t.Fatalf("rebuild report=%+v", report)
	}
	requestsAfterBuild := provider.requests.Load()
	if requestsAfterBuild != 2 { // initial dual-canary probe plus document batch with both canaries
		t.Fatalf("rebuild requests=%d, want 2", requestsAfterBuild)
	}
	if provider.authSeen.Load() {
		t.Error("loopback embedding endpoint received Authorization despite local Ollama mode")
	}

	// Exact node resolution short-circuits before semanticCandidates.
	exact, exactMeta, err := e.RecallContext(context.Background(), RecallArgs{Query: "vault.go#Vault"}, "semantic-test")
	if err != nil || !exactMeta.Hit || !strings.Contains(exact, "vault.go#Vault") {
		t.Fatalf("exact recall hit=%v err=%v output:\n%s", exactMeta.Hit, err, exact)
	}
	if got := provider.requests.Load(); got != requestsAfterBuild {
		t.Fatalf("exact node recall contacted provider: before=%d after=%d", requestsAfterBuild, got)
	}

	// The synthetic query has no lexical overlap. Only the semantic vector can
	// discover Vault.
	semanticOnly, semanticMeta, err := e.RecallContext(context.Background(), RecallArgs{Query: semanticOnlyTestQuery}, "semantic-test")
	if err != nil || !semanticMeta.Hit {
		t.Fatalf("semantic-only recall hit=%v err=%v output:\n%s", semanticMeta.Hit, err, semanticOnly)
	}
	if !strings.Contains(semanticOnly, "vault.go#Vault") || !strings.Contains(semanticOnly, "semantic rank=") ||
		!strings.Contains(semanticOnly, "refs=vault.go#Vault#") ||
		strings.Contains(semanticOnly, "keyword rank=") {
		t.Fatalf("semantic-only recall did not stay semantic-only:\n%s", semanticOnly)
	}

	// Provider failure is advisory: lexical candidates still render.
	provider.fail.Store(true)
	requestsBeforeFailure := provider.requests.Load()
	fallback, fallbackMeta, err := e.RecallContext(context.Background(), RecallArgs{Query: "lexicalfallback"}, "semantic-test")
	if err != nil || !fallbackMeta.Hit {
		t.Fatalf("provider fallback hit=%v err=%v output:\n%s", fallbackMeta.Hit, err, fallback)
	}
	if !strings.Contains(fallback, "vault.go#Vault") || !strings.Contains(fallback, "semantic 已降级") {
		t.Fatalf("provider failure did not preserve lexical recall:\n%s", fallback)
	}
	if got := provider.requests.Load(); got != requestsBeforeFailure+1 {
		t.Fatalf("provider failure requests=%d, want %d", got, requestsBeforeFailure+1)
	}

	// Changing the summary source now keeps the old generation in partial mode:
	// only records whose ID and source hash still match may contribute, while
	// the newly added card waits for rebuild.
	provider.fail.Store(false)
	e.semantic.mu.Lock()
	e.semantic.failureUntil = time.Time{}
	e.semantic.lastError = ""
	e.semantic.mu.Unlock()
	if _, err := e.Remember(RememberArgs{
		Node:     "vault.go#Vault",
		Entries:  []RememberEntry{{Kind: "summary", Text: "新增摘要使 semantic source fingerprint 变化"}},
		Keywords: []string{"stalefallback"},
	}, "semantic-test", "test"); err != nil {
		t.Fatal(err)
	}
	requestsBeforeStale := provider.requests.Load()
	stale, staleMeta, err := e.RecallContext(context.Background(), RecallArgs{Query: "stalefallback"}, "semantic-test")
	if err != nil || !staleMeta.Hit {
		t.Fatalf("stale fallback hit=%v err=%v output:\n%s", staleMeta.Hit, err, stale)
	}
	if !strings.Contains(stale, "vault.go#Vault") || !strings.Contains(stale, "partial") ||
		!strings.Contains(stale, "semantic 已降级") {
		t.Fatalf("source-mismatched generation did not report partial mode:\n%s", stale)
	}
	if got := provider.requests.Load(); got != requestsBeforeStale+1 {
		t.Fatalf("partial generation requests=%d, want %d", got, requestsBeforeStale+1)
	}
	e.semantic.mu.Lock()
	partialSnapshot := e.semantic.snapshot
	e.semantic.mu.Unlock()
	health := e.SemanticHealthSnapshot()
	if health.Status != SemanticHealthPartial || !health.PayloadLoaded {
		t.Fatalf("status discarded loaded partial payload: %+v", health)
	}
	requestsBeforeRepeat := provider.requests.Load()
	if _, _, err := e.RecallContext(context.Background(), RecallArgs{Query: "stalefallback"}, "semantic-test"); err != nil {
		t.Fatal(err)
	}
	if got := provider.requests.Load(); got != requestsBeforeRepeat {
		t.Fatalf("status evicted partial query cache: before=%d after=%d", requestsBeforeRepeat, got)
	}
	e.semantic.mu.Lock()
	if e.semantic.snapshot != partialSnapshot {
		t.Fatal("status/recall re-decoded an unchanged partial generation")
	}
	e.semantic.mu.Unlock()
}

func TestSemanticInvalidOrDisabledConfigEvictsResidentStateOffline(t *testing.T) {
	provider := newSemanticHTTPTestProvider(t)
	e, _ := initEngine(t, map[string]string{"vault.go": "package sample\n\nfunc Vault() {}\n"})
	if _, err := e.Remember(RememberArgs{Node: "vault.go#Vault", Entries: []RememberEntry{{
		Kind: "summary", Text: semanticTargetMarker + " resident eviction",
	}}}, "semantic-evict", "test"); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultSemanticSettings()
	cfg.Enabled, cfg.Endpoint, cfg.Model, cfg.Dimensions, cfg.TimeoutSec =
		true, provider.server.URL, "integration-embed", 3, 2
	if err := SaveSemanticSettings(e.Store, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RebuildSemantic(context.Background()); err != nil {
		t.Fatal(err)
	}

	prime := func(t *testing.T) {
		t.Helper()
		if err := SaveSemanticSettings(e.Store, cfg); err != nil {
			t.Fatal(err)
		}
		out, meta, err := e.RecallContext(context.Background(), RecallArgs{Query: semanticOnlyTestQuery}, "semantic-evict")
		if err != nil || !meta.Hit || !strings.Contains(out, "vault.go#Vault") {
			t.Fatalf("prime resident state hit=%v err=%v output=%s", meta.Hit, err, out)
		}
		e.semantic.mu.Lock()
		resident := e.semantic.snapshot != nil && len(e.semantic.queryCache) > 0
		e.semantic.mu.Unlock()
		if !resident {
			t.Fatal("semantic snapshot/query cache were not resident after recall")
		}
	}
	assertEvictedOffline := func(t *testing.T, requestsBefore int64) {
		t.Helper()
		if got := provider.requests.Load(); got != requestsBefore {
			t.Fatalf("invalid/disabled state contacted provider: before=%d after=%d", requestsBefore, got)
		}
		e.semantic.mu.Lock()
		defer e.semantic.mu.Unlock()
		if e.semantic.snapshot != nil || len(e.semantic.queryCache) != 0 || e.semantic.loadedKey != "" {
			t.Fatalf("resident semantic state survived revocation: snapshot=%v cache=%d key=%q",
				e.semantic.snapshot != nil, len(e.semantic.queryCache), e.semantic.loadedKey)
		}
	}

	tests := []struct {
		name       string
		mutate     func(*testing.T)
		useHealth  bool
		wantHealth SemanticHealthStatus
	}{
		{
			name: "disabled-recall",
			mutate: func(t *testing.T) {
				disabled := cfg
				disabled.Enabled = false
				if err := SaveSemanticSettings(e.Store, disabled); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "disabled-health",
			mutate: func(t *testing.T) {
				disabled := cfg
				disabled.Enabled = false
				if err := SaveSemanticSettings(e.Store, disabled); err != nil {
					t.Fatal(err)
				}
			},
			useHealth: true, wantHealth: SemanticHealthDisabled,
		},
		{
			name: "missing-recall",
			mutate: func(t *testing.T) {
				if err := e.Store.RemoveSemanticConfig(); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "missing-health",
			mutate: func(t *testing.T) {
				if err := e.Store.RemoveSemanticConfig(); err != nil {
					t.Fatal(err)
				}
			},
			useHealth: true, wantHealth: SemanticHealthUnconfigured,
		},
		{
			name: "corrupt-recall",
			mutate: func(t *testing.T) {
				if err := e.Store.WriteSemanticConfig([]byte("{")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "corrupt-health",
			mutate: func(t *testing.T) {
				if err := e.Store.WriteSemanticConfig([]byte("{")); err != nil {
					t.Fatal(err)
				}
			},
			useHealth: true, wantHealth: SemanticHealthCorrupt,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			prime(t)
			test.mutate(t)
			requestsBefore := provider.requests.Load()
			if test.useHealth {
				if health := e.SemanticHealthSnapshot(); health.Status != test.wantHealth {
					t.Fatalf("health=%+v, want status=%s", health, test.wantHealth)
				}
			} else {
				if _, meta, err := e.RecallContext(context.Background(), RecallArgs{Query: semanticOnlyTestQuery}, "semantic-evict"); err != nil || meta.Hit {
					t.Fatalf("revoked recall hit=%v err=%v", meta.Hit, err)
				}
			}
			assertEvictedOffline(t, requestsBefore)
		})
	}
}

func TestSemanticRecallAdvisoryOnlyIsEvidenceHitWithoutCurrentPromotion(t *testing.T) {
	provider := newSemanticHTTPTestProvider(t)
	e, _ := initEngine(t, map[string]string{
		"risk.go":    "package sample\n\nfunc Risk() {}\n",
		"history.go": "package sample\n\nfunc History() {}\n",
	})
	if _, err := e.Remember(RememberArgs{Node: "risk.go#Risk", Entries: []RememberEntry{{
		Kind: "pitfall", Text: semanticTargetMarker + " active risk evidence",
	}}}, "semantic-advisory", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RecordChange(ChangeArgs{
		Nodes: []string{"history.go#History"}, What: semanticTargetMarker + " historical decision", Why: "audit trail",
	}, "semantic-advisory", "test"); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultSemanticSettings()
	cfg.Enabled, cfg.Endpoint, cfg.Model, cfg.Dimensions, cfg.TimeoutSec =
		true, provider.server.URL, "integration-embed", 3, 2
	cfg.MinScore = 0.5
	if err := SaveSemanticSettings(e.Store, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RebuildSemantic(context.Background()); err != nil {
		t.Fatal(err)
	}

	out, meta, err := e.RecallContext(context.Background(), RecallArgs{Query: semanticOnlyTestQuery}, "semantic-advisory")
	if err != nil || !meta.Hit {
		t.Fatalf("advisory evidence should remain a usage-log hit: hit=%v err=%v output=%s", meta.Hit, err, out)
	}
	for _, want := range []string{
		"当前答案候选｜无", "风险警示", "risk.go#Risk", "历史来路 [historical]", "history.go#History",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("advisory-only recall missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "semantic 混合命中") || strings.Contains(out, "RRF=") {
		t.Fatalf("risk/history were promoted into the current RRF section:\n%s", out)
	}
}

func TestSemanticRebuildSameBatchCanaryDriftPreservesPublishedGeneration(t *testing.T) {
	provider := newSemanticHTTPTestProvider(t)
	e, _ := initEngine(t, map[string]string{"vault.go": "package sample\n\nfunc Vault() {}\n"})
	if _, err := e.Remember(RememberArgs{Node: "vault.go#Vault", Entries: []RememberEntry{{
		Kind: "summary", Text: semanticTargetMarker + " canary 漂移保护",
	}}}, "semantic-canary", "test"); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultSemanticSettings()
	cfg.Enabled, cfg.Endpoint, cfg.Model, cfg.Dimensions, cfg.TimeoutSec =
		true, provider.server.URL, "integration-embed", 3, 2
	if err := SaveSemanticSettings(e.Store, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RebuildSemantic(context.Background()); err != nil {
		t.Fatal(err)
	}
	beforeIdentity := semanticTestIndexIdentity(t, e)
	e.semantic.mu.Lock()
	beforeGeneration := e.semantic.metadata.Generation
	beforeSnapshot := e.semantic.snapshot
	e.semantic.mu.Unlock()
	requestsBefore := provider.requests.Load()

	// The initial dual-canary probe still reports the original model. Only the
	// document-mode canary returned in the same request as this rebuild's data
	// batch drifts, so publication must stop before entering the atomic writer.
	provider.driftBatchCanary.Store(true)
	if _, err := e.RebuildSemantic(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "检测到实际模型漂移") {
		t.Fatalf("same-batch model drift error=%v", err)
	}
	if got := provider.driftedCanaries.Load(); got != 1 {
		t.Fatalf("drifted same-batch canaries=%d, want 1", got)
	}
	if got := provider.requests.Load() - requestsBefore; got != 2 {
		t.Fatalf("failed rebuild requests=%d, want initial probe + one dual-canary document batch", got)
	}
	afterIdentity := semanticTestIndexIdentity(t, e)
	if afterIdentity != beforeIdentity {
		t.Fatalf("failed rebuild replaced disk index:\nbefore=%+v\nafter=%+v", beforeIdentity, afterIdentity)
	}
	e.semantic.mu.Lock()
	afterGeneration := e.semantic.metadata.Generation
	afterSnapshot := e.semantic.snapshot
	e.semantic.mu.Unlock()
	if afterGeneration != beforeGeneration || afterSnapshot != beforeSnapshot {
		t.Fatalf("failed rebuild replaced runtime index: generation %q -> %q, snapshot changed=%v",
			beforeGeneration, afterGeneration, afterSnapshot != beforeSnapshot)
	}
}

func TestSemanticIndexWrapperRoundTripAndTamper(t *testing.T) {
	source := sha256.Sum256([]byte("source"))
	snapshot, err := vector.Build(3, []vector.Record{{
		ID: "summary:vault.go#Vault#e_test", NodeID: "vault.go#Vault", Kind: "summary",
		SourceHash: source, Vector: []float32{1, 2, 3},
	}})
	if err != nil {
		t.Fatal(err)
	}
	meta := semanticIndexMetadata{
		Schema: 1, Generation: strings.Repeat("a", 32), SettingsFingerprint: "v1:settings", EmbedderFingerprint: "embedder:test",
		ProbeFingerprint: "probe-test", QueryProbeFingerprint: "query-probe-test", SourceFingerprint: hex.EncodeToString(source[:]),
		Dimensions: 3, Records: 1, BuiltAt: time.Date(2026, 7, 19, 1, 2, 3, 0, time.UTC).Format(time.RFC3339),
	}
	var encoded bytes.Buffer
	if err := encodeSemanticIndex(&encoded, meta, snapshot); err != nil {
		t.Fatal(err)
	}
	decodedMeta, decodedSnapshot, err := decodeSemanticIndex(bytes.NewReader(encoded.Bytes()), vector.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if decodedMeta != meta || decodedSnapshot.Status() != snapshot.Status() {
		t.Fatalf("roundtrip meta=%+v status=%+v", decodedMeta, decodedSnapshot.Status())
	}

	t.Run("metadata checksum", func(t *testing.T) {
		tampered := bytes.Clone(encoded.Bytes())
		tampered[semanticWrapperSize] ^= 0x01
		if _, _, err := decodeSemanticIndex(bytes.NewReader(tampered), vector.DefaultLimits()); err == nil ||
			!strings.Contains(err.Error(), "metadata checksum") {
			t.Fatalf("tampered metadata error=%v", err)
		}
	})
	t.Run("vector checksum", func(t *testing.T) {
		tampered := bytes.Clone(encoded.Bytes())
		tampered[len(tampered)-1] ^= 0x01
		if _, _, err := decodeSemanticIndex(bytes.NewReader(tampered), vector.DefaultLimits()); err == nil {
			t.Fatal("tampered vector payload was accepted")
		}
	})
}

func TestSemanticVectorFingerprintUsesNormalizedDirection(t *testing.T) {
	base := semanticVectorFingerprint([]float32{1, 2, 3})
	if base == "" || semanticVectorFingerprint([]float32{2, 4, 6}) != base {
		t.Fatal("positive rescaling of one embedding direction changed canary fingerprint")
	}
	left := semanticVectorFingerprint([]float32{1e20, 2e20})
	right := semanticVectorFingerprint([]float32{2e20, 1e20})
	if left == "" || right == "" || left == right {
		t.Fatalf("large distinct directions collided: left=%q right=%q", left, right)
	}
	if got := semanticVectorFingerprint([]float32{0, 0}); got != "" {
		t.Fatalf("zero vector received a fingerprint: %q", got)
	}
}

func TestSemanticRuntimeReloadsExternalGenerationAndClear(t *testing.T) {
	provider := newSemanticHTTPTestProvider(t)
	e, _ := initEngine(t, map[string]string{"vault.go": "package sample\n\nfunc Vault() {}\n"})
	if _, err := e.Remember(RememberArgs{Node: "vault.go#Vault", Entries: []RememberEntry{{
		Kind: "summary", Text: semanticTargetMarker + " 外部 generation 检测",
	}}}, "semantic-external", "test"); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultSemanticSettings()
	cfg.Enabled, cfg.Endpoint, cfg.Model, cfg.Dimensions, cfg.TimeoutSec = true, provider.server.URL, "integration-embed", 3, 2
	if err := SaveSemanticSettings(e.Store, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RebuildSemantic(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.semantic.mu.Lock()
	firstGeneration := e.semantic.metadata.Generation
	e.semantic.mu.Unlock()

	// 模拟真实部署：独立 CLI Engine 原子替换同 settings+source 的文件。
	external := New(e.Store)
	if _, err := external.RebuildSemantic(context.Background()); err != nil {
		t.Fatal(err)
	}
	external.semantic.mu.Lock()
	externalGeneration := external.semantic.metadata.Generation
	external.semantic.mu.Unlock()
	if externalGeneration == firstGeneration {
		t.Fatal("独立 rebuild 未产生新 generation")
	}
	status, err := e.SemanticStatusText()
	if err != nil || !strings.Contains(status, "index: metadata-valid") {
		t.Fatalf("status=%q err=%v", status, err)
	}
	e.semantic.mu.Lock()
	loadedGeneration := e.semantic.metadata.Generation
	e.semantic.mu.Unlock()
	if loadedGeneration != externalGeneration {
		t.Fatalf("live engine 仍持旧 generation: got=%s want=%s", loadedGeneration, externalGeneration)
	}

	if err := external.ClearSemanticIndex(); err != nil {
		t.Fatal(err)
	}
	status, err = e.SemanticStatusText()
	if err != nil || !strings.Contains(status, "stale/unavailable") {
		t.Fatalf("external clear 后 status=%q err=%v", status, err)
	}
	e.semantic.mu.Lock()
	stillLoaded := e.semantic.snapshot != nil
	e.semantic.mu.Unlock()
	if stillLoaded {
		t.Fatal("external clear 后 live engine 仍持有 snapshot")
	}
}

func TestSemanticQueryDropsHitsWhenExternalGenerationChanges(t *testing.T) {
	provider := newSemanticHTTPTestProvider(t)
	e, _ := initEngine(t, map[string]string{"vault.go": "package sample\n\nfunc Vault() {}\n"})
	if _, err := e.Remember(RememberArgs{Node: "vault.go#Vault", Entries: []RememberEntry{{
		Kind: "summary", Text: semanticTargetMarker + " generation race",
	}}}, "semantic-generation", "test"); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultSemanticSettings()
	cfg.Enabled, cfg.Endpoint, cfg.Model, cfg.Dimensions, cfg.TimeoutSec = true, provider.server.URL, "integration-embed", 3, 2
	if err := SaveSemanticSettings(e.Store, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RebuildSemantic(context.Background()); err != nil {
		t.Fatal(err)
	}
	provider.blockQuery.Store(true)
	type recallResult struct {
		out  string
		meta ReadMeta
		err  error
	}
	result := make(chan recallResult, 1)
	go func() {
		out, meta, err := e.RecallContext(context.Background(), RecallArgs{Query: semanticOnlyTestQuery}, "semantic-generation")
		result <- recallResult{out: out, meta: meta, err: err}
	}()
	select {
	case <-provider.queryEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("query embedding did not start")
	}
	external := New(e.Store)
	if _, err := external.RebuildSemantic(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(provider.queryRelease)
	got := <-result
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.meta.Hit || !strings.Contains(got.out, "索引在查询期间被替换") {
		t.Fatalf("external generation should discard old hits: hit=%v output=%s", got.meta.Hit, got.out)
	}
}

func writeSemanticHealthIndex(t *testing.T, e *Engine, cfg SemanticSettings, builtAt string) semanticIndexMetadata {
	t.Helper()
	docs, manifest, err := e.semanticSourceSnapshot(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	records := make([]vector.Record, 0, len(docs))
	for i, doc := range docs {
		values := []float32{1, 0, 0}
		if i%2 == 1 {
			values = []float32{0, 1, 0}
		}
		records = append(records, vector.Record{
			ID: doc.RecordID, NodeID: doc.NodeID, Kind: doc.Kind,
			SourceHash: doc.SourceHash, Vector: values,
		})
	}
	snapshot, err := vector.Build(3, records)
	if err != nil {
		t.Fatal(err)
	}
	embedderFingerprint, err := semanticOfflineEmbedderFingerprint(cfg)
	if err != nil {
		t.Fatal(err)
	}
	meta := semanticIndexMetadata{
		Schema: 1, Generation: "0123456789abcdef0123456789abcdef",
		SettingsFingerprint:   SemanticSettingsFingerprint(cfg),
		EmbedderFingerprint:   embedderFingerprint,
		ProbeFingerprint:      "offline-test-probe",
		QueryProbeFingerprint: "offline-test-query-probe",
		SourceFingerprint:     hex.EncodeToString(manifest.fingerprint[:]),
		Dimensions:            3,
		Records:               len(records),
		BuiltAt:               builtAt,
	}
	var encoded bytes.Buffer
	if err := encodeSemanticIndex(&encoded, meta, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.WritePrivateKnowledgeFile(semanticIndexRel, encoded.Bytes()); err != nil {
		t.Fatal(err)
	}
	return meta
}

func semanticHealthTestEngine(t *testing.T, endpoint string) (*Engine, SemanticSettings) {
	t.Helper()
	e, _ := initEngine(t, map[string]string{"vault.go": "package sample\n\nfunc Vault() {}\n"})
	if _, err := e.Remember(RememberArgs{Node: "vault.go#Vault", Entries: []RememberEntry{{
		Kind: "summary", Text: "凭据保险库使用不可变快照",
	}}}, "semantic-health", "test"); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultSemanticSettings()
	cfg.Enabled = true
	cfg.Endpoint = endpoint
	cfg.Model = "health-embed"
	cfg.Dimensions = 3
	cfg.TimeoutSec = 1
	return e, cfg
}

func TestSemanticHealthSnapshotStableStatesStayOffline(t *testing.T) {
	provider := newSemanticHTTPTestProvider(t)
	builtAt := "2026-07-19T12:34:56Z"

	t.Run("unconfigured", func(t *testing.T) {
		e, _ := initEngine(t, map[string]string{"plain.go": "package sample\n"})
		health := e.SemanticHealthSnapshot()
		if health.Status != SemanticHealthUnconfigured || health.Provider != "unchecked" || health.NextAction == "" {
			t.Fatalf("health=%+v", health)
		}
	})

	t.Run("disabled", func(t *testing.T) {
		e, cfg := semanticHealthTestEngine(t, provider.server.URL)
		cfg.Enabled = false
		if err := SaveSemanticSettings(e.Store, cfg); err != nil {
			t.Fatal(err)
		}
		health := e.SemanticHealthSnapshot()
		if health.Status != SemanticHealthDisabled || health.Model != cfg.Model || health.Profile == "" || health.Policy != "manual" {
			t.Fatalf("health=%+v", health)
		}
	})

	t.Run("configured-no-index-and-ai-policy", func(t *testing.T) {
		e, cfg := semanticHealthTestEngine(t, provider.server.URL)
		cfg.RebuildPolicy = SemanticRebuildAILocal
		if err := SaveSemanticSettings(e.Store, cfg); err != nil {
			t.Fatal(err)
		}
		health := e.SemanticHealthSnapshot()
		if health.Status != SemanticHealthConfiguredNoIndex || health.NextAction != "kb_semantic action=sync" || health.Policy != "ai-local" {
			t.Fatalf("health=%+v", health)
		}
	})

	t.Run("ready-metadata", func(t *testing.T) {
		e, cfg := semanticHealthTestEngine(t, provider.server.URL)
		if err := SaveSemanticSettings(e.Store, cfg); err != nil {
			t.Fatal(err)
		}
		meta := writeSemanticHealthIndex(t, e, cfg, builtAt)
		health := e.SemanticHealthSnapshot()
		if health.Status != SemanticHealthReady || health.Provider != "unchecked" || health.NextAction != "none" ||
			health.Model != cfg.Model || health.Profile == "" || health.ConfiguredDimensions != 3 || health.IndexDimensions != 3 ||
			health.Records != meta.Records || health.BuiltAt != builtAt || health.PayloadLoaded {
			t.Fatalf("health=%+v", health)
		}
		status, err := e.Status()
		if err != nil || !strings.Contains(status, "semantic: ready | provider: unchecked | next_action: none") ||
			!strings.Contains(status, "model=health-embed") || !strings.Contains(status, "built_at="+builtAt) {
			t.Fatalf("kb_status=%q err=%v", status, err)
		}
	})

	t.Run("ready-auto-dimensions-remain-distinct", func(t *testing.T) {
		e, cfg := semanticHealthTestEngine(t, provider.server.URL)
		cfg.Dimensions = 0
		if err := SaveSemanticSettings(e.Store, cfg); err != nil {
			t.Fatal(err)
		}
		writeSemanticHealthIndex(t, e, cfg, builtAt)
		health := e.SemanticHealthSnapshot()
		if health.Status != SemanticHealthReady || health.ConfiguredDimensions != 0 || health.IndexDimensions != 3 {
			t.Fatalf("auto/index dimensions conflated: %+v", health)
		}
		status, err := e.SemanticStatusText()
		if err != nil || !strings.Contains(status, "dimensions: 0(auto=0)") || !strings.Contains(status, "index_dimensions: 3") {
			t.Fatalf("auto dimensions status=%q err=%v", status, err)
		}
	})

	t.Run("stale-source", func(t *testing.T) {
		e, cfg := semanticHealthTestEngine(t, provider.server.URL)
		if err := SaveSemanticSettings(e.Store, cfg); err != nil {
			t.Fatal(err)
		}
		writeSemanticHealthIndex(t, e, cfg, builtAt)
		if _, err := e.Remember(RememberArgs{Node: "vault.go#Vault", Entries: []RememberEntry{{
			Kind: "contract", Text: "调用方必须传入已校验的租户标识",
		}}}, "semantic-health", "test"); err != nil {
			t.Fatal(err)
		}
		health := e.SemanticHealthSnapshot()
		if health.Status != SemanticHealthPartial || !strings.Contains(health.NextAction, "semantic rebuild") ||
			!strings.Contains(health.Detail, "source hash") {
			t.Fatalf("health=%+v", health)
		}
	})

	t.Run("stale-provider", func(t *testing.T) {
		e, cfg := semanticHealthTestEngine(t, provider.server.URL)
		if err := SaveSemanticSettings(e.Store, cfg); err != nil {
			t.Fatal(err)
		}
		writeSemanticHealthIndex(t, e, cfg, builtAt)
		cfg.Model = "health-embed-v2"
		if err := SaveSemanticSettings(e.Store, cfg); err != nil {
			t.Fatal(err)
		}
		health := e.SemanticHealthSnapshot()
		if health.Status != SemanticHealthStaleProvider {
			t.Fatalf("health=%+v", health)
		}
	})

	t.Run("corrupt", func(t *testing.T) {
		e, cfg := semanticHealthTestEngine(t, provider.server.URL)
		if err := SaveSemanticSettings(e.Store, cfg); err != nil {
			t.Fatal(err)
		}
		if err := e.Store.WritePrivateKnowledgeFile(semanticIndexRel, []byte("broken")); err != nil {
			t.Fatal(err)
		}
		health := e.SemanticHealthSnapshot()
		if health.Status != SemanticHealthCorrupt {
			t.Fatalf("health=%+v", health)
		}
	})

	if got := provider.requests.Load(); got != 0 {
		t.Fatalf("semantic health/status contacted provider %d times", got)
	}
}

func TestSemanticLegacyMetadataIsRebuildableStaleNotCorrupt(t *testing.T) {
	provider := newSemanticHTTPTestProvider(t)
	e, cfg := semanticHealthTestEngine(t, provider.server.URL)
	if err := SaveSemanticSettings(e.Store, cfg); err != nil {
		t.Fatal(err)
	}
	writeSemanticHealthIndex(t, e, cfg, "2026-07-19T12:34:56Z")
	data, err := e.Store.ReadKnowledgeFile(semanticIndexRel)
	if err != nil {
		t.Fatal(err)
	}
	meta, snapshot, err := decodeSemanticIndex(bytes.NewReader(data), semanticVectorLimits(cfg))
	if err != nil {
		t.Fatal(err)
	}
	meta.QueryProbeFingerprint = "" // Round 37 wrapper, before query-mode canaries.
	var legacy bytes.Buffer
	if err := encodeSemanticIndex(&legacy, meta, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.WritePrivateKnowledgeFile(semanticIndexRel, legacy.Bytes()); err != nil {
		t.Fatal(err)
	}
	health := e.SemanticHealthSnapshot()
	if health.Status != SemanticHealthStaleProvider || strings.Contains(health.Detail, "corrupt") ||
		!strings.Contains(health.NextAction, "semantic rebuild") {
		t.Fatalf("legacy metadata health=%+v", health)
	}
	if got := provider.requests.Load(); got != 0 {
		t.Fatalf("legacy metadata health contacted provider %d times", got)
	}
}

func TestSemanticHealthRemembersPayloadCorruptionProvedByRecall(t *testing.T) {
	provider := newSemanticHTTPTestProvider(t)
	e, _ := initEngine(t, map[string]string{"vault.go": "package sample\n\nfunc Vault() {}\n"})
	if _, err := e.Remember(RememberArgs{Node: "vault.go#Vault", Entries: []RememberEntry{{
		Kind: "summary", Text: semanticTargetMarker + " payload corruption",
	}}}, "semantic-corrupt", "test"); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultSemanticSettings()
	cfg.Enabled, cfg.Endpoint, cfg.Model, cfg.Dimensions, cfg.TimeoutSec =
		true, provider.server.URL, "integration-embed", 3, 2
	if err := SaveSemanticSettings(e.Store, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RebuildSemantic(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := e.Store.ReadKnowledgeFile(semanticIndexRel)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0x01
	if err := e.Store.WritePrivateKnowledgeFile(semanticIndexRel, data); err != nil {
		t.Fatal(err)
	}
	requestsBefore := provider.requests.Load()
	out, meta, err := e.RecallContext(context.Background(), RecallArgs{Query: semanticOnlyTestQuery}, "semantic-corrupt")
	if err != nil || meta.Hit || !strings.Contains(out, "semantic 已降级") ||
		!strings.Contains(out, "checksum") {
		t.Fatalf("corrupt recall hit=%v err=%v output=%s", meta.Hit, err, out)
	}
	if got := provider.requests.Load(); got != requestsBefore {
		t.Fatalf("corrupt payload contacted provider: before=%d after=%d", requestsBefore, got)
	}
	health := e.SemanticHealthSnapshot()
	if health.Status != SemanticHealthCorrupt || !strings.Contains(health.Detail, "recall 已验证") ||
		!strings.Contains(health.NextAction, "semantic rebuild") {
		t.Fatalf("health forgot proved payload corruption: %+v", health)
	}
	if _, err := e.Remember(RememberArgs{Node: "vault.go#Vault", Entries: []RememberEntry{{
		Kind: "contract", Text: "knowledge changed after corruption proof",
	}}}, "semantic-corrupt-change", "test"); err != nil {
		t.Fatal(err)
	}
	if health = e.SemanticHealthSnapshot(); health.Status != SemanticHealthCorrupt {
		t.Fatalf("source change hid corruption of unchanged disk generation: %+v", health)
	}
}

func TestSemanticSyncValidatesReadyPayloadAndRepairsCorruption(t *testing.T) {
	provider := newSemanticHTTPTestProvider(t)
	e, _ := initEngine(t, map[string]string{"vault.go": "package sample\n\nfunc Vault() {}\n"})
	if _, err := e.Remember(RememberArgs{Node: "vault.go#Vault", Entries: []RememberEntry{{
		Kind: "summary", Text: semanticTargetMarker + " sync payload validation",
	}}}, "semantic-sync-corrupt", "test"); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultSemanticSettings()
	cfg.Enabled, cfg.Endpoint, cfg.Model, cfg.Dimensions, cfg.TimeoutSec =
		true, provider.server.URL, "integration-embed", 3, 2
	cfg.RebuildPolicy = SemanticRebuildAILocal
	if err := SaveSemanticSettings(e.Store, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RebuildSemantic(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := e.Store.ReadKnowledgeFile(semanticIndexRel)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0x01
	if err := e.Store.WritePrivateKnowledgeFile(semanticIndexRel, data); err != nil {
		t.Fatal(err)
	}
	before := provider.requests.Load()
	text, err := e.SyncSemantic(context.Background())
	if err != nil || !strings.Contains(text, "semantic 索引已重建") {
		t.Fatalf("sync did not repair metadata-valid corrupt payload: text=%q err=%v", text, err)
	}
	if got := provider.requests.Load() - before; got != 2 {
		t.Fatalf("repair provider requests=%d, want probe + document batch", got)
	}
	if health := e.SemanticHealthSnapshot(); health.Status != SemanticHealthReady || !health.PayloadLoaded {
		t.Fatalf("repaired health=%+v", health)
	}
}

func TestSemanticHealthSnapshotDoesNotReadRemoteAPIKey(t *testing.T) {
	t.Setenv(SemanticAPIKeyEnv, "invalid\nheader")
	e, cfg := semanticHealthTestEngine(t, "https://embedding.example.test/v1")
	if err := SaveSemanticSettings(e.Store, cfg); err != nil {
		t.Fatal(err)
	}
	writeSemanticHealthIndex(t, e, cfg, "2026-07-19T12:34:56Z")
	if health := e.SemanticHealthSnapshot(); health.Status != SemanticHealthReady {
		t.Fatalf("status 读取或校验了 API key: %+v", health)
	}
}
