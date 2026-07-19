package engine

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zdypro888/iknowledge/internal/index"
	"github.com/zdypro888/iknowledge/internal/model"
	"github.com/zdypro888/iknowledge/internal/vector"
)

func TestSemanticRecallMergeKeepsRiskAndHistoryOutsideCurrentRRF(t *testing.T) {
	e, _ := initEngine(t, map[string]string{
		"a.go":       "package sample\n\nfunc A() {}\n",
		"b.go":       "package sample\n\nfunc B() {}\n",
		"risk.go":    "package sample\n\nfunc Risk() {}\n",
		"history.go": "package sample\n\nfunc History() {}\n",
	})
	currentHash := sha256.Sum256([]byte("current"))
	riskHash := sha256.Sum256([]byte("risk"))
	historyHash := sha256.Sum256([]byte("history"))
	const (
		currentID = "card:current:b.go#B:0000"
		riskID    = "card:risk:risk.go#Risk:0000"
		historyID = "card:history:history.go#History:0000"
	)

	e.rt.mu.Lock()
	e.rt.semanticManifest = semanticSourceManifest{
		version: e.rt.semanticSourceVersion,
		ready:   true,
		records: map[string]semanticSourceRecord{
			currentID: {NodeID: "b.go#B", Kind: semanticLaneCurrent, SourceHash: currentHash, Facets: []string{"summary"}},
			riskID:    {NodeID: "risk.go#Risk", Kind: semanticLaneRisk, SourceHash: riskHash, Facets: []string{"pitfall"}},
			historyID: {NodeID: "history.go#History", Kind: semanticLaneHistory, SourceHash: historyHash, Facets: []string{"decision_history"}},
		},
	}
	hits := semanticCandidateSet{
		current: []vector.Hit{{ID: currentID, NodeID: "b.go#B", Kind: semanticLaneCurrent, SourceHash: currentHash, Score: 0.40}},
		risk:    []vector.Hit{{ID: riskID, NodeID: "risk.go#Risk", Kind: semanticLaneRisk, SourceHash: riskHash, Score: 0.999}},
		history: []vector.Hit{{ID: historyID, NodeID: "history.go#History", Kind: semanticLaneHistory, SourceHash: historyHash, Score: 0.998}},
	}
	merged, warning := e.mergeRecallCandidatesLocked(
		[]index.Hit{{NodeID: "a.go#A", Score: 1}}, nil, hits, 10,
	)
	out := e.renderRecallCandidatesLocked(merged, "")
	e.rt.mu.Unlock()

	if warning != "" {
		t.Fatalf("merge warning=%q", warning)
	}
	if len(merged.current) != 2 || len(merged.risk) != 1 || len(merged.history) != 1 {
		t.Fatalf("lane sizes current=%d risk=%d history=%d", len(merged.current), len(merged.risk), len(merged.history))
	}
	for _, candidate := range merged.current {
		if candidate.nodeID == "risk.go#Risk" || candidate.nodeID == "history.go#History" ||
			(candidate.semanticKind != "" && candidate.semanticKind != semanticLaneCurrent) {
			t.Fatalf("advisory entered current/RRF despite higher cosine: %+v", candidate)
		}
	}
	riskSection := strings.Index(out, "风险警示")
	if riskSection < 0 {
		t.Fatalf("risk section missing:\n%s", out)
	}
	currentSection := out[:riskSection]
	if strings.Contains(currentSection, "risk.go#Risk") || strings.Contains(currentSection, "history.go#History") {
		t.Fatalf("advisory rendered as current answer:\n%s", out)
	}
	for _, want := range []string{"a.go#A", "b.go#B", "risk.go#Risk", "history.go#History"} {
		if !strings.Contains(out, want) {
			t.Fatalf("render missing %q:\n%s", want, out)
		}
	}
}

func TestSemanticRecallMergeCountsAdvisoryOnlyAsEvidenceHit(t *testing.T) {
	// ReadMeta.Hit feeds usage logs: returning precise risk/history evidence is
	// useful knowledge even though it must never be represented as a current
	// answer. This contract keeps the evidence hit while render says current=0.
	result := recallMerge{risk: []semanticEvidence{{nodeID: "risk.go#Risk", lane: semanticLaneRisk}}}
	if !result.hit() {
		t.Fatal("advisory-only knowledge was incorrectly recorded as an empty recall")
	}
	if len(result.current) != 0 {
		t.Fatal("advisory-only result unexpectedly has a current candidate")
	}
}

func TestRecallCandidateHeaderNeverLeaksSuspectSummaryAsCurrent(t *testing.T) {
	e, _ := initEngine(t, map[string]string{
		"worker.go": "package worker\n\nfunc Run() {}\n",
	})
	const unsafeSummary = "unsafe-summary-must-require-exact-down-drill"
	e.rt.mu.Lock()
	ref := e.rt.ix.Node("worker.go#Run")
	if ref == nil {
		e.rt.mu.Unlock()
		t.Fatal("missing worker node")
	}
	ref.Node.Status = model.StatusFresh
	ref.Node.Entries = append(ref.Node.Entries, model.Entry{
		ID: "e_suspect", Kind: model.KindSummary, Text: unsafeSummary, Confidence: model.ConfidenceSuspect,
	})
	out := e.renderRecallCandidatesLocked(recallMerge{current: []recallCandidate{{
		nodeID: "worker.go#Run", lexicalRank: 1, lexicalScore: 2,
	}}}, "")
	e.rt.mu.Unlock()
	if strings.Contains(out, unsafeSummary) {
		t.Fatalf("candidate header leaked suspect summary as current:\n%s", out)
	}
	for _, want := range []string{"worker.go#Run", "[level=function]", "[status=fresh]", "用节点 ID 精确重查"} {
		if !strings.Contains(out, want) {
			t.Fatalf("safe candidate rendering missing %q:\n%s", want, out)
		}
	}
}

func TestRecallExactPitfallTokenIsRiskOnly(t *testing.T) {
	e, _ := initEngine(t, map[string]string{
		"risk.go": "package sample\n\nfunc Risk() {}\n",
	})
	const marker = "toxoplasmosis"
	if _, err := e.Remember(RememberArgs{
		Node:    "risk.go#Risk",
		Entries: []RememberEntry{{Kind: model.KindPitfall, Text: marker}},
	}, "risk-lane", "codex"); err != nil {
		t.Fatalf("remember pitfall: %v", err)
	}

	out, meta, err := e.RecallContext(context.Background(), RecallArgs{Query: marker, Limit: 5}, "risk-lane")
	if err != nil {
		t.Fatalf("recall pitfall: %v", err)
	}
	if !meta.Hit {
		t.Fatalf("risk-only evidence should count as a recall hit:\n%s", out)
	}
	riskAt := strings.Index(out, "风险警示")
	if riskAt < 0 {
		t.Fatalf("risk section missing:\n%s", out)
	}
	if !strings.Contains(out, "当前答案候选｜无") {
		t.Fatalf("risk-only lexical match was promoted into current answer:\n%s", out)
	}
	if strings.Contains(out[:riskAt], "risk.go#Risk") {
		t.Fatalf("risk node rendered before risk advisory section:\n%s", out)
	}
	for _, want := range []string{"risk.go#Risk", "keyword-risk rank=1", "facets=lexical-risk", "关键词/相似度只负责发现"} {
		if !strings.Contains(out[riskAt:], want) {
			t.Fatalf("risk rendering missing %q:\n%s", want, out)
		}
	}
}

func TestSyncRebuildsLexicalAndSemanticLanesAfterReconcile(t *testing.T) {
	e, repo := initEngine(t, map[string]string{
		"worker.go": "package worker\n\nfunc Run() int { return 1 }\n",
	})
	const marker = "xylophoniczebra"
	if _, err := e.Remember(RememberArgs{Node: "worker.go#Run", Entries: []RememberEntry{{
		Kind: model.KindUsage, Text: marker + " is safe only for the anchored implementation",
	}}}, "reconcile-lanes", "test"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.semanticSourceSnapshot(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	e.rt.mu.RLock()
	versionBefore := e.rt.semanticSourceVersion
	e.rt.mu.RUnlock()
	if err := os.WriteFile(filepath.Join(repo, "worker.go"), []byte("package worker\n\nfunc Run() int { return 2 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, _, err := e.RecallContext(context.Background(), RecallArgs{Query: marker, Limit: 5}, "reconcile-lanes")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "当前答案候选｜无") || !strings.Contains(out, "风险警示") {
		t.Fatalf("first post-change recall used pre-reconcile current lane:\n%s", out)
	}
	e.rt.mu.RLock()
	versionAfter := e.rt.semanticSourceVersion
	e.rt.mu.RUnlock()
	if versionAfter <= versionBefore {
		t.Fatalf("reconcile did not invalidate semantic generation: before=%d after=%d", versionBefore, versionAfter)
	}
	docs, _, err := e.semanticSourceSnapshot(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range docs {
		if strings.Contains(doc.Text, marker) && doc.Kind != semanticLaneRisk {
			t.Fatalf("reconciled knowledge remained in %s lane: %s", doc.Kind, doc.Text)
		}
	}
}
