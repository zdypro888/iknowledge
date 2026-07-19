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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zdypro888/iknowledge/internal/vector"
)

const (
	semanticOnlyTestQuery = "qzvplmno"
	semanticTargetMarker  = "orchidmarker"
	semanticOtherMarker   = "cobaltmarker"
)

type semanticHTTPTestProvider struct {
	server       *httptest.Server
	requests     atomic.Int64
	fail         atomic.Bool
	authSeen     atomic.Bool
	blockQuery   atomic.Bool
	queryEntered chan struct{}
	queryRelease chan struct{}
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
			_, err := e.cachedSemanticQuery(ctx, embedder, "same-failing-query")
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
		ProbeFingerprint: "probe", SourceFingerprint: hex.EncodeToString(source[:]),
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
			values, err := e.cachedSemanticQuery(ctx, embedder, "same-query")
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

func newSemanticHTTPTestProvider(t *testing.T) *semanticHTTPTestProvider {
	t.Helper()
	provider := &semanticHTTPTestProvider{queryEntered: make(chan struct{}, 1), queryRelease: make(chan struct{})}
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
		if provider.blockQuery.Load() && len(request.Input) == 1 && request.Input[0] == semanticOnlyTestQuery {
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
	if requestsAfterBuild != 2 { // one model probe plus one document batch
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

	// Changing the summary source invalidates the published generation before
	// any new provider call; lexical recall remains available.
	provider.fail.Store(false)
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
	if !strings.Contains(stale, "vault.go#Vault") || !strings.Contains(stale, "stale") ||
		!strings.Contains(stale, "semantic 已降级") {
		t.Fatalf("stale generation did not degrade to lexical:\n%s", stale)
	}
	if got := provider.requests.Load(); got != requestsBeforeStale {
		t.Fatalf("stale generation contacted provider: before=%d after=%d", requestsBeforeStale, got)
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
		ProbeFingerprint: "probe-test", SourceFingerprint: hex.EncodeToString(source[:]),
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
