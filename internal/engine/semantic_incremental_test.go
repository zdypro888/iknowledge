package engine

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zdypro888/iknowledge/internal/model"
	"github.com/zdypro888/iknowledge/internal/vector"
)

func setSemanticScaleEntries(t *testing.T, e *Engine, count int, lastVariant string) {
	t.Helper()
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
	target.Entries = make([]model.Entry, count)
	// Keep every two entries above the card target so each entry remains a
	// distinct vector record, while avoiding the doubled source/marshal cost of
	// filling every entry to the full production card limit (important under
	// the race detector).
	padding := strings.Repeat("x", semanticCardRawTarget/2+64)
	for i := range target.Entries {
		text := fmt.Sprintf("%s stable-%08x %s", semanticTargetMarker, i, padding)
		if i == count-1 && lastVariant != "" {
			text = fmt.Sprintf("%s %s-%08x %s", semanticTargetMarker, lastVariant, i, padding)
		}
		target.Entries[i] = model.Entry{
			ID: fmt.Sprintf("e_%08x", i), Kind: model.KindSummary,
			Text: text, Confidence: model.ConfidenceVerified,
		}
	}
	if err := e.Store.SaveShard(shardPath, shard, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.SyncContext(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func mutateAllSemanticScaleEntries(t *testing.T, e *Engine) {
	t.Helper()
	shardPath := e.Store.ShardPathFor("vault.go")
	shard, _, err := e.Store.LoadShard(shardPath)
	if err != nil {
		t.Fatal(err)
	}
	for i := range shard.Nodes {
		if shard.Nodes[i].ID != "vault.go#Vault" {
			continue
		}
		for j := range shard.Nodes[i].Entries {
			shard.Nodes[i].Entries[j].Text = "changed " + shard.Nodes[i].Entries[j].Text
		}
		if err := e.Store.SaveShard(shardPath, shard, nil); err != nil {
			t.Fatal(err)
		}
		if err := e.SyncContext(context.Background()); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatal("missing vault function node")
}

func writeCurrentSemanticFixtureIndex(t *testing.T, e *Engine, cfg SemanticSettings, probeVector []float32) int {
	t.Helper()
	docs, manifest, err := e.semanticSourceSnapshot(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	records := make([]vector.Record, len(docs))
	for i, doc := range docs {
		records[i] = vector.Record{
			ID: doc.RecordID, NodeID: doc.NodeID, Kind: doc.Kind,
			SourceHash: doc.SourceHash, Vector: []float32{0, 1, 0},
		}
	}
	snapshot, err := vector.Build(3, records)
	if err != nil {
		t.Fatal(err)
	}
	embedderFingerprint, err := semanticOfflineEmbedderFingerprint(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if probeVector == nil {
		probeVector = []float32{0, 0, 1}
	}
	probeFingerprint := semanticVectorFingerprint(probeVector)
	meta := semanticIndexMetadata{
		Schema: 1, Generation: "0123456789abcdef0123456789abcdef",
		SettingsFingerprint:   SemanticSettingsFingerprint(cfg),
		EmbedderFingerprint:   embedderFingerprint,
		ProbeFingerprint:      probeFingerprint,
		QueryProbeFingerprint: probeFingerprint,
		SourceFingerprint:     hex.EncodeToString(manifest.fingerprint[:]),
		Dimensions:            3,
		Records:               len(records),
		BuiltAt:               time.Now().UTC().Format(time.RFC3339),
	}
	var encoded bytes.Buffer
	if err := encodeSemanticIndex(&encoded, meta, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.WritePrivateKnowledgeFile(semanticIndexRel, encoded.Bytes()); err != nil {
		t.Fatal(err)
	}
	return len(records)
}

func assertIncrementalSemanticRecall(t *testing.T, e *Engine) {
	t.Helper()
	candidates, warning := e.semanticCandidates(context.Background(), "baseline vector reuse query")
	if warning != "" {
		t.Fatalf("semantic recall warning=%q", warning)
	}
	if len(candidates.current) == 0 || candidates.current[0].NodeID != "vault.go#Vault" {
		t.Fatalf("semantic recall candidates=%+v", candidates)
	}
}

func TestSemanticIncrementalSyncOverTotalLimitEmbedsOnlyDelta(t *testing.T) {
	provider := newSemanticHTTPTestProvider(t)
	e, _ := initEngine(t, map[string]string{"vault.go": "package sample\n\nfunc Vault() {}\n"})
	cfg := DefaultSemanticSettings()
	cfg.Enabled = true
	cfg.Endpoint = provider.server.URL
	cfg.Model = "integration-embed"
	cfg.Dimensions = 3
	cfg.TimeoutSec = 2
	cfg.RebuildPolicy = SemanticRebuildAILocal
	if err := SaveSemanticSettings(e.Store, cfg); err != nil {
		t.Fatal(err)
	}

	baseEntries := semanticMCPSyncMaxRecords + 1
	setSemanticScaleEntries(t, e, baseEntries, "")
	oldRecords := writeCurrentSemanticFixtureIndex(t, e, cfg, nil)
	if oldRecords <= semanticMCPSyncMaxRecords {
		t.Fatalf("fixture records=%d, want >%d", oldRecords, semanticMCPSyncMaxRecords)
	}

	setSemanticScaleEntries(t, e, baseEntries+1, "added")
	preSync := e.SemanticHealthSnapshot()
	if preSync.Status != SemanticHealthPartial || preSync.NextAction != "kb_semantic action=sync" {
		t.Fatalf("large partial source should offer incremental MCP sync: %+v", preSync)
	}
	provider.requests.Store(0)
	provider.documentInputs.Store(0)
	text, err := e.SyncSemantic(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "reused=") || !strings.Contains(text, "embedded=") {
		t.Fatalf("incremental report missing accounting: %s", text)
	}
	if got := provider.documentInputs.Load(); got <= 0 || got > 4 {
		t.Fatalf("add embedded documents=%d, want small delta", got)
	}
	addedHealth := e.SemanticHealthSnapshot()
	if addedHealth.Status != SemanticHealthReady || addedHealth.Records <= semanticMCPSyncMaxRecords {
		t.Fatalf("health after add=%+v", addedHealth)
	}
	assertIncrementalSemanticRecall(t, e)

	setSemanticScaleEntries(t, e, baseEntries+1, "modified")
	provider.requests.Store(0)
	provider.documentInputs.Store(0)
	if _, err := e.SyncSemantic(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := provider.documentInputs.Load(); got <= 0 || got > 4 {
		t.Fatalf("modify embedded documents=%d, want small delta", got)
	}
	assertIncrementalSemanticRecall(t, e)

	setSemanticScaleEntries(t, e, baseEntries, "")
	provider.requests.Store(0)
	provider.documentInputs.Store(0)
	text, err = e.SyncSemantic(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := provider.documentInputs.Load(); got != 0 {
		t.Fatalf("delete-only sync embedded %d documents, want 0", got)
	}
	if !strings.Contains(text, "embedded=0") {
		t.Fatalf("delete-only report=%s", text)
	}
	if got := e.SemanticHealthSnapshot(); got.Status != SemanticHealthReady || got.Records != oldRecords {
		t.Fatalf("health after delete=%+v oldRecords=%d", got, oldRecords)
	}
	assertIncrementalSemanticRecall(t, e)

	// A model can drift after the initial probe but in the same request as the
	// changed document batch. The old generation must remain published and the
	// deterministic block must be remembered so later MCP sessions do not keep
	// paying for the same doomed canary.
	setSemanticScaleEntries(t, e, baseEntries, "batch-drift")
	if got := e.SemanticHealthSnapshot(); got.Status != SemanticHealthPartial || got.NextAction != "kb_semantic action=sync" {
		t.Fatalf("offline status should offer candidate incremental sync: %+v", got)
	}
	oldIdentity := semanticTestIndexIdentity(t, e)
	provider.driftBatchCanary.Store(true)
	provider.requests.Store(0)
	provider.documentInputs.Store(0)
	_, err = e.SyncSemantic(context.Background())
	if err == nil || !strings.Contains(err.Error(), "漂移") {
		t.Fatalf("same-batch canary drift error=%v", err)
	}
	if got := provider.requests.Load(); got != 2 {
		t.Fatalf("canary drift provider requests=%d, want initial probe + one document batch", got)
	}
	if got := provider.documentInputs.Load(); got <= 0 || got > 4 {
		t.Fatalf("canary drift sent documents=%d, want only the small delta", got)
	}
	if got := semanticTestIndexIdentity(t, e); got != oldIdentity {
		t.Fatalf("canary drift replaced old generation: before=%+v after=%+v", oldIdentity, got)
	}
	if got := e.SemanticHealthSnapshot(); got.NextAction == "kb_semantic action=sync" || !strings.Contains(got.Detail, "canary") {
		t.Fatalf("known canary drift remained actionable: %+v", got)
	}
	var kb *KBError
	if _, err := e.syncSemanticOwner(context.Background()); !errors.As(err, &kb) || kb.Code != "SEMANTIC_SYNC_TOO_LARGE" {
		t.Fatalf("direct sync bypassed cached canary block: %v", err)
	}
	if got := provider.requests.Load(); got != 2 {
		t.Fatalf("blocked direct sync repeated provider request: %d", got)
	}

	// Changing the source invalidates the canary assessment key. A fresh full
	// local scan must compute the exact oversized delta without any provider I/O,
	// and the server-side action gate must reject even a direct sync call.
	provider.driftBatchCanary.Store(false)
	mutateAllSemanticScaleEntries(t, e)
	provider.requests.Store(0)
	provider.documentInputs.Store(0)
	health := e.SemanticHealthSnapshot()
	if health.Status != SemanticHealthPartial || health.NextAction == "kb_semantic action=sync" ||
		!strings.Contains(health.Detail, "精确增量") {
		t.Fatalf("known oversized delta remained actionable: %+v", health)
	}
	if got := semanticTestIndexIdentity(t, e); got != oldIdentity {
		t.Fatalf("local oversized assessment replaced old generation: before=%+v after=%+v", oldIdentity, got)
	}
	kb = nil
	if _, err := e.syncSemanticOwner(context.Background()); !errors.As(err, &kb) || kb.Code != "SEMANTIC_SYNC_TOO_LARGE" {
		t.Fatalf("direct sync bypassed oversized-delta block: %v", err)
	}
	if got := provider.requests.Load(); got != 0 {
		t.Fatalf("oversized delta contacted provider %d times", got)
	}

	// A large stale-provider generation has no reusable vector space, so all
	// source cards are pending. The hard action gate must retain the established
	// TOO_LARGE machine contract across this early health branch as well.
	staleCfg := cfg
	staleCfg.Model = "integration-embed-changed"
	if err := SaveSemanticSettings(e.Store, staleCfg); err != nil {
		t.Fatal(err)
	}
	kb = nil
	if _, err := e.syncSemanticOwner(context.Background()); !errors.As(err, &kb) || kb.Code != "SEMANTIC_SYNC_TOO_LARGE" {
		t.Fatalf("large stale-provider sync error=%v", err)
	}
	if got := provider.requests.Load(); got != 0 {
		t.Fatalf("large stale-provider sync contacted provider %d times", got)
	}
}
