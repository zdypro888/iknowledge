package engine

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zdypro888/iknowledge/internal/index"
	"github.com/zdypro888/iknowledge/internal/model"
)

// debtCancelOnErrContext makes checkpoint tests deterministic without relying
// on scheduler timing: its Nth Err observation trips cancellation.
type debtCancelOnErrContext struct {
	cancelAt int64
	calls    atomic.Int64
}

func (*debtCancelOnErrContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (*debtCancelOnErrContext) Done() <-chan struct{}       { return nil }
func (*debtCancelOnErrContext) Value(any) any               { return nil }
func (c *debtCancelOnErrContext) Err() error {
	if c.calls.Add(1) >= c.cancelAt {
		return context.Canceled
	}
	return nil
}

func debtContextTestEngine(t *testing.T, nodes []model.Node, changes []model.Change) *Engine {
	t.Helper()
	e, _ := initEngine(t, map[string]string{
		"seed.go": "package seed\n\nfunc Seed() {}\n",
	})
	e.rt.mu.Lock()
	e.rt.ix = index.Build(map[string][]model.Node{"tree/context.yaml": nodes}, changes, nil)
	e.rt.mu.Unlock()
	return e
}

func TestComputeDebtsLockedContextCancellationReturnsNoPartialResult(t *testing.T) {
	t.Run("node scan", func(t *testing.T) {
		nodes := make([]model.Node, 130)
		for i := range nodes {
			nodes[i] = model.Node{
				ID:     fmt.Sprintf("pkg/f%03d.go#F%03d", i, i),
				Level:  model.LevelFunction,
				Status: model.StatusSuspect,
				Anchor: model.Anchor{Hash: "hash"},
			}
		}
		e := debtContextTestEngine(t, nodes, nil)
		ctx := &debtCancelOnErrContext{cancelAt: 3}
		e.rt.mu.Lock()
		debts, err := e.computeDebtsLockedContext(ctx)
		e.rt.mu.Unlock()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("node scan cancellation=%v, want context.Canceled", err)
		}
		if debts != nil {
			t.Fatalf("node scan returned partial debts: %+v", debts)
		}
	})

	t.Run("history scan", func(t *testing.T) {
		const nodeID = "pkg/history.go#History"
		node := model.Node{
			ID: nodeID, Level: model.LevelFunction, Status: model.StatusFresh,
			Anchor: model.Anchor{Hash: "hash"},
		}
		changes := make([]model.Change, 256)
		for i := range changes {
			changes[i] = model.Change{
				ID: fmt.Sprintf("chg_%03d", i), Nodes: []string{nodeID},
				At: time.Unix(int64(i+1), 0), What: "changed behavior", Why: "test",
			}
		}
		e := debtContextTestEngine(t, []model.Node{node}, changes)
		// Initial/store/suspect-sort checks consume five Err calls; the sixth
		// is reached from the shared 64-operation history checkpoint.
		ctx := &debtCancelOnErrContext{cancelAt: 6}
		e.rt.mu.Lock()
		debts, err := e.computeDebtsLockedContext(ctx)
		e.rt.mu.Unlock()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("history scan cancellation=%v, want context.Canceled (calls=%d)", err, ctx.calls.Load())
		}
		if debts != nil {
			t.Fatalf("history scan returned partial debts: %+v", debts)
		}
	})

	t.Run("entry scan", func(t *testing.T) {
		entries := make([]model.Entry, 256)
		for i := range entries {
			entries[i] = model.Entry{
				ID: fmt.Sprintf("e_%08x", i), Kind: model.KindSummary,
				Text: fmt.Sprintf("entry knowledge %03d", i), Confidence: model.ConfidenceInferred,
			}
		}
		node := model.Node{
			ID: "pkg/entries.go#Entries", Level: model.LevelFunction, Status: model.StatusFresh,
			Anchor: model.Anchor{Hash: "hash"}, Entries: entries,
		}
		e := debtContextTestEngine(t, []model.Node{node}, nil)
		ctx := &debtCancelOnErrContext{cancelAt: 6}
		e.rt.mu.Lock()
		debts, err := e.computeDebtsLockedContext(ctx)
		e.rt.mu.Unlock()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("entry scan cancellation=%v, want context.Canceled (calls=%d)", err, ctx.calls.Load())
		}
		if debts != nil {
			t.Fatalf("entry scan returned partial debts: %+v", debts)
		}
	})
}

func TestComputeDebtsLockedContextMatchesLegacyWrapper(t *testing.T) {
	entries := []model.Entry{
		{ID: "e_00000001", Kind: model.KindSummary, Text: "the exact same durable knowledge", Confidence: model.ConfidenceVerified},
		{ID: "e_00000002", Kind: model.KindSummary, Text: "the exact same durable knowledge", Confidence: model.ConfidenceVerified},
	}
	e := debtContextTestEngine(t, []model.Node{{
		ID: "pkg/wrapper.go#Wrapper", Level: model.LevelFunction, Status: model.StatusFresh,
		Anchor: model.Anchor{Hash: "hash"}, Entries: entries,
	}}, nil)
	e.rt.mu.Lock()
	want := e.computeDebtsLocked()
	got, err := e.computeDebtsLockedContext(context.Background())
	e.rt.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("context debts differ from legacy wrapper:\ncontext=%+v\nlegacy=%+v", got, want)
	}
}

func TestMaintainContextHeapSortCancellationAndOrdering(t *testing.T) {
	values := make([]int, 4096)
	for i := range values {
		values[i] = len(values) - i
	}
	want := append([]int(nil), values...)
	sort.Ints(want)
	if err := maintainContextHeapSort(context.Background(), values, func(a, b int) bool { return a < b }); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(values, want) {
		t.Fatal("context heap sort produced a different order")
	}

	ctx := &debtCancelOnErrContext{cancelAt: 2}
	values = make([]int, 4096)
	for i := range values {
		values[i] = len(values) - i
	}
	if err := maintainContextHeapSort(ctx, values, func(a, b int) bool { return a < b }); !errors.Is(err, context.Canceled) {
		t.Fatalf("sort cancellation=%v, want context.Canceled", err)
	}
	if got := ctx.calls.Load(); got != 2 {
		t.Fatalf("sort observed cancellation after %d context checks, want 2", got)
	}
}

func TestMaintainCachedBigramComparisonMatchesJaccard(t *testing.T) {
	pairs := [][2]string{
		{"the same durable knowledge", "the same durable knowledge"},
		{"abcdefghij", "abcdefghix"},
		{"short", "a completely unrelated and much longer sentence"},
		{"", ""},
		{"支付回调必须五秒内响应", "支付回调必须五秒内响应"},
	}
	for _, pair := range pairs {
		got, err := maintainBigramSimilarContext(bigramSet(pair[0]), bigramSet(pair[1]), func() error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		want := BigramJaccard(pair[0], pair[1]) > 0.8
		if got != want {
			t.Fatalf("cached comparison(%q, %q)=%v, want %v", pair[0], pair[1], got, want)
		}
	}
}

func TestCrossNodeDupCandidatesRareGramIndexDeterministicAndBounded(t *testing.T) {
	t.Run("high similarity pair is not missed", func(t *testing.T) {
		common := map[string]bool{}
		for i := range 9 {
			common[fmt.Sprintf("c%d", i)] = true
		}
		gramsA := map[string]bool{"unique-a": true}
		gramsB := map[string]bool{"unique-b": true}
		for gram := range common {
			gramsA[gram] = true
			gramsB[gram] = true
		}
		all := []maintainDupEntry{
			{nodeID: "z/z.go#Z", entry: &model.Entry{ID: "e_z"}, grams: gramsB},
			{nodeID: "a/a.go#A", entry: &model.Entry{ID: "e_a"}, grams: gramsA},
		}
		debts, err := crossNodeDupCandidatesPreparedContext(context.Background(), all)
		if err != nil {
			t.Fatal(err)
		}
		if len(debts) != 1 || debts[0].Kind != "cross-dup" {
			t.Fatalf("rare-gram index missed >0.8 pair: %+v", debts)
		}
	})

	t.Run("stable maximum five", func(t *testing.T) {
		makeEntries := func(reverse bool) []maintainDupEntry {
			out := make([]maintainDupEntry, 8)
			for i := range out {
				position := i
				if reverse {
					position = len(out) - 1 - i
				}
				entry := &model.Entry{ID: fmt.Sprintf("e_%02d", position)}
				out[i] = maintainDupEntry{
					nodeID: fmt.Sprintf("pkg/f%02d.go#F%02d", position, position),
					entry:  entry, grams: map[string]bool{"aa": true, "bb": true, "cc": true},
				}
			}
			return out
		}
		forward, err := crossNodeDupCandidatesPreparedContext(context.Background(), makeEntries(false))
		if err != nil {
			t.Fatal(err)
		}
		reversed, err := crossNodeDupCandidatesPreparedContext(context.Background(), makeEntries(true))
		if err != nil {
			t.Fatal(err)
		}
		if len(forward) != 5 {
			t.Fatalf("cross-node duplicate cap=%d, want 5", len(forward))
		}
		if !reflect.DeepEqual(forward, reversed) {
			t.Fatalf("candidate output depends on input order:\nforward=%+v\nreversed=%+v", forward, reversed)
		}
	})
}
