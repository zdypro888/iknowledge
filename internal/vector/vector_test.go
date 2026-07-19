package vector

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

func TestBuildNormalizesCopiesAndReportsStatus(t *testing.T) {
	if got := (*Snapshot)(nil).Status(); got != (Status{}) {
		t.Fatalf("nil Status() = %#v, want zero value", got)
	}
	records := []Record{
		{ID: "b", NodeID: "node-b", Kind: "summary", SourceHash: [32]byte{1}, Vector: []float32{3, 4}},
		{ID: "a", NodeID: "node-a", Kind: "era_summary", SourceHash: [32]byte{2}, Vector: []float32{6, 8}},
	}
	snapshot, err := Build(2, records)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := snapshot.Status(), (Status{
		Records:       2,
		Dimensions:    2,
		VectorBytes:   16,
		MetadataBytes: encodedMetadataSize("b", "node-b", "summary") + encodedMetadataSize("a", "node-a", "era_summary"),
		Normalized:    true,
	}); got != want {
		t.Fatalf("Status() = %#v, want %#v", got, want)
	}
	if len(snapshot.vectors) != 4 {
		t.Fatalf("contiguous vector length = %d, want 4", len(snapshot.vectors))
	}
	for i := 0; i < len(snapshot.records); i++ {
		start := i * snapshot.dimensions
		var normSquared float64
		for _, value := range snapshot.vectors[start : start+snapshot.dimensions] {
			normSquared += float64(value) * float64(value)
		}
		if math.Abs(normSquared-1) > 1e-6 {
			t.Fatalf("record %d squared norm = %g, want 1", i, normSquared)
		}
	}

	// Build owns a defensive copy. Mutating both the input vector and the Record
	// slice after return cannot affect the immutable Snapshot.
	records[0].Vector[0] = -100
	records[0].ID = "changed"
	hits, err := snapshot.Search(context.Background(), []float32{3, 4}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{hits[0].ID, hits[1].ID}; !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("stable tied IDs = %v, want [a b]", got)
	}
	if hits[0].SourceHash != ([32]byte{2}) {
		t.Fatalf("source hash = %x, want copied value", hits[0].SourceHash)
	}
}

func TestBuildValidation(t *testing.T) {
	valid := Record{ID: "id", NodeID: "node", Kind: "summary", Vector: []float32{1, 2}}
	invalidUTF8 := string([]byte{0xff})

	tests := []struct {
		name       string
		dimensions int
		records    []Record
		limits     Limits
		want       error
	}{
		{name: "zero dimensions", dimensions: 0, limits: DefaultLimits(), want: ErrInvalidInput},
		{name: "negative dimensions", dimensions: -1, limits: DefaultLimits(), want: ErrInvalidInput},
		{name: "dimension limit", dimensions: 3, limits: withLimits(func(l *Limits) { l.MaxDimensions = 2 }), want: ErrLimitExceeded},
		{name: "record limit", dimensions: 2, records: []Record{valid, {ID: "id-2", NodeID: "node", Kind: "summary", Vector: []float32{1, 2}}}, limits: withLimits(func(l *Limits) { l.MaxRecords = 1 }), want: ErrLimitExceeded},
		{name: "vector bytes", dimensions: 2, records: []Record{valid}, limits: withLimits(func(l *Limits) { l.MaxVectorBytes = 7 }), want: ErrLimitExceeded},
		{name: "metadata bytes", dimensions: 2, records: []Record{valid}, limits: withLimits(func(l *Limits) { l.MaxMetadataBytes = 1 }), want: ErrLimitExceeded},
		{name: "string bytes", dimensions: 2, records: []Record{valid}, limits: withLimits(func(l *Limits) { l.MaxStringBytes = 1 }), want: ErrLimitExceeded},
		{name: "empty ID", dimensions: 2, records: []Record{{NodeID: "node", Kind: "summary", Vector: []float32{1, 2}}}, limits: DefaultLimits(), want: ErrInvalidInput},
		{name: "empty node", dimensions: 2, records: []Record{{ID: "id", Kind: "summary", Vector: []float32{1, 2}}}, limits: DefaultLimits(), want: ErrInvalidInput},
		{name: "empty kind", dimensions: 2, records: []Record{{ID: "id", NodeID: "node", Vector: []float32{1, 2}}}, limits: DefaultLimits(), want: ErrInvalidInput},
		{name: "invalid UTF-8", dimensions: 2, records: []Record{{ID: invalidUTF8, NodeID: "node", Kind: "summary", Vector: []float32{1, 2}}}, limits: DefaultLimits(), want: ErrInvalidInput},
		{name: "wrong dimension", dimensions: 2, records: []Record{{ID: "id", NodeID: "node", Kind: "summary", Vector: []float32{1}}}, limits: DefaultLimits(), want: ErrInvalidInput},
		{name: "NaN", dimensions: 2, records: []Record{{ID: "id", NodeID: "node", Kind: "summary", Vector: []float32{float32(math.NaN()), 1}}}, limits: DefaultLimits(), want: ErrInvalidInput},
		{name: "infinity", dimensions: 2, records: []Record{{ID: "id", NodeID: "node", Kind: "summary", Vector: []float32{float32(math.Inf(1)), 1}}}, limits: DefaultLimits(), want: ErrInvalidInput},
		{name: "zero vector", dimensions: 2, records: []Record{{ID: "id", NodeID: "node", Kind: "summary", Vector: []float32{0, 0}}}, limits: DefaultLimits(), want: ErrInvalidInput},
		{name: "duplicate ID", dimensions: 2, records: []Record{valid, valid}, limits: DefaultLimits(), want: ErrInvalidInput},
		{name: "invalid limits", dimensions: 2, records: []Record{valid}, limits: Limits{}, want: ErrInvalidInput},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := BuildWithLimits(test.dimensions, test.records, test.limits)
			if !errors.Is(err, test.want) {
				t.Fatalf("BuildWithLimits() error = %v, want errors.Is(%v)", err, test.want)
			}
		})
	}
}

func TestPreflightRejectsBeforeVectorWork(t *testing.T) {
	badVector := []float32{float32(math.NaN()), 0}

	// Capacity is decided from count and dimensions, before any vector is read
	// or the contiguous matrix is allocated.
	limits := withLimits(func(l *Limits) { l.MaxVectorBytes = 7 })
	_, err := BuildWithLimits(2, []Record{{ID: "id", NodeID: "node", Kind: "summary", Vector: badVector}}, limits)
	if !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("capacity error = %v, want ErrLimitExceeded before NaN validation", err)
	}

	// All metadata and duplicate IDs are likewise rejected before vector
	// validation. This is the same Preflight path used by BuildWithLimits.
	_, err = Build(2, []Record{
		{ID: "duplicate", NodeID: "one", Kind: "summary", Vector: badVector},
		{ID: "duplicate", NodeID: "two", Kind: "summary", Vector: []float32{0, 0}},
	})
	if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "duplicate record ID") {
		t.Fatalf("metadata-first error = %v, want duplicate ID", err)
	}

	_, err = Preflight(context.Background(), 2, []Record{{
		ID: "", NodeID: "node", Kind: "summary", Vector: badVector,
	}}, DefaultLimits())
	if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "record ID") {
		t.Fatalf("Preflight error = %v, want record ID validation", err)
	}
}

func TestBuilderBatchesEqualBuild(t *testing.T) {
	metadataOnly := []Record{
		{ID: "c", NodeID: "node-c", Kind: "summary", SourceHash: [32]byte{3}},
		{ID: "a", NodeID: "node-a", Kind: "era_summary", SourceHash: [32]byte{1}},
		{ID: "b", NodeID: "node-b", Kind: "summary", SourceHash: [32]byte{2}},
	}
	plan, err := Preflight(context.Background(), 3, metadataOnly, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	// Preflight owns metadata and does not retain caller records.
	metadataOnly[0].ID = "mutated"

	builder, err := NewBuilder(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := builder.Append(context.Background(), [][]float32{{3, 4, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := builder.Append(context.Background(), [][]float32{{0, 2, 0}, {0, 0, -5}}); err != nil {
		t.Fatal(err)
	}
	got, err := builder.Finish(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	want, err := Build(3, []Record{
		{ID: "c", NodeID: "node-c", Kind: "summary", SourceHash: [32]byte{3}, Vector: []float32{3, 4, 0}},
		{ID: "a", NodeID: "node-a", Kind: "era_summary", SourceHash: [32]byte{1}, Vector: []float32{0, 2, 0}},
		{ID: "b", NodeID: "node-b", Kind: "summary", SourceHash: [32]byte{2}, Vector: []float32{0, 0, -5}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status() != want.Status() || !reflect.DeepEqual(got.records, want.records) || !reflect.DeepEqual(got.vectors, want.vectors) {
		t.Fatalf("batched build differs:\n got=%#v %#v\nwant=%#v %#v", got.records, got.vectors, want.records, want.vectors)
	}
	if len(got.vectors) != len(got.records)*got.dimensions {
		t.Fatalf("matrix is not contiguous: %d floats for %d x %d", len(got.vectors), len(got.records), got.dimensions)
	}
	if _, err := builder.Finish(context.Background()); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("second Finish error = %v, want ErrInvalidInput", err)
	}
	if err := builder.Append(context.Background(), [][]float32{{1, 0, 0}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Append after Finish error = %v, want ErrInvalidInput", err)
	}
}

func TestBuilderFailureDoesNotAdvanceAndCanRetry(t *testing.T) {
	plan, err := Preflight(context.Background(), 2, []Record{
		{ID: "a", NodeID: "a", Kind: "summary"},
		{ID: "b", NodeID: "b", Kind: "summary"},
	}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	builder, err := NewBuilder(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := builder.Append(context.Background(), [][]float32{{1, 0}, {0}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad batch error = %v, want ErrInvalidInput", err)
	}
	if builder.appended != 0 {
		t.Fatalf("failed batch advanced to %d", builder.appended)
	}
	if _, err := builder.Finish(context.Background()); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("incomplete Finish error = %v, want ErrInvalidInput", err)
	}
	if err := builder.Append(context.Background(), [][]float32{{-1, 0}, {0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := builder.Append(context.Background(), [][]float32{{1, 0}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("overflow Append error = %v, want ErrInvalidInput", err)
	}
	snapshot, err := builder.Finish(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.vectors[0] != -1 || snapshot.vectors[1] != 0 {
		t.Fatalf("retry did not overwrite failed batch row: %v", snapshot.vectors[:2])
	}
	if _, err := NewBuilder(context.Background(), &BuildPlan{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("zero plan error = %v, want ErrInvalidInput", err)
	}
}

func TestBuildPipelineContextCancellation(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	records := []Record{{ID: "a", NodeID: "a", Kind: "summary", Vector: []float32{1, 0}}}

	if _, err := Preflight(canceled, 2, records, DefaultLimits()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Preflight error = %v, want context.Canceled", err)
	}
	if _, err := Preflight(nil, 2, records, DefaultLimits()); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Preflight nil context error = %v, want ErrInvalidInput", err)
	}
	plan, err := Preflight(context.Background(), 2, records, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewBuilder(canceled, plan); !errors.Is(err, context.Canceled) {
		t.Fatalf("NewBuilder error = %v, want context.Canceled", err)
	}
	builder, err := NewBuilder(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := builder.Append(canceled, [][]float32{{1, 0}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Append error = %v, want context.Canceled", err)
	}
	if builder.appended != 0 {
		t.Fatalf("canceled Append advanced to %d", builder.appended)
	}
	if err := builder.Append(context.Background(), [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	if _, err := builder.Finish(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("Finish error = %v, want context.Canceled", err)
	}
	if _, err := builder.Finish(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildWithLimitsContext(canceled, 2, records, DefaultLimits()); !errors.Is(err, context.Canceled) {
		t.Fatalf("BuildWithLimitsContext error = %v, want context.Canceled", err)
	}
}

func TestSearchStableTopKAndValidation(t *testing.T) {
	snapshot, err := Build(2, []Record{
		{ID: "b", NodeID: "nb", Kind: "summary", Vector: []float32{1, 0}},
		{ID: "c", NodeID: "nc", Kind: "summary", Vector: []float32{1, 1}},
		{ID: "a", NodeID: "na", Kind: "summary", Vector: []float32{2, 0}},
		{ID: "d", NodeID: "nd", Kind: "summary", Vector: []float32{-1, 0}},
	})
	if err != nil {
		t.Fatal(err)
	}
	query := []float32{10, 0}
	hits, err := snapshot.Search(context.Background(), query, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{hits[0].ID, hits[1].ID}; !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("top two = %v, want [a b]", got)
	}
	if query[0] != 10 || query[1] != 0 {
		t.Fatalf("Search mutated query: %v", query)
	}
	all, err := snapshot.Search(context.Background(), query, 99)
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{all[0].ID, all[1].ID, all[2].ID, all[3].ID}; !reflect.DeepEqual(got, []string{"a", "b", "c", "d"}) {
		t.Fatalf("all hits = %v, want [a b c d]", got)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name  string
		index *Snapshot
		ctx   context.Context
		query []float32
		limit int
		want  error
	}{
		{name: "nil snapshot", ctx: context.Background(), query: query, limit: 1, want: ErrInvalidInput},
		{name: "nil context", index: snapshot, query: query, limit: 1, want: ErrInvalidInput},
		{name: "canceled", index: snapshot, ctx: canceled, query: query, limit: 1, want: context.Canceled},
		{name: "bad limit", index: snapshot, ctx: context.Background(), query: query, limit: 0, want: ErrInvalidInput},
		{name: "bad dimension", index: snapshot, ctx: context.Background(), query: []float32{1}, limit: 1, want: ErrInvalidInput},
		{name: "zero query", index: snapshot, ctx: context.Background(), query: []float32{0, 0}, limit: 1, want: ErrInvalidInput},
		{name: "NaN query", index: snapshot, ctx: context.Background(), query: []float32{float32(math.NaN()), 0}, limit: 1, want: ErrInvalidInput},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := test.index.Search(test.ctx, test.query, test.limit)
			if !errors.Is(err, test.want) {
				t.Fatalf("Search() error = %v, want errors.Is(%v)", err, test.want)
			}
		})
	}
}

func TestSearchDistinctNodes(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name      string
		records   []Record
		ctx       context.Context
		query     []float32
		limit     int
		wantIDs   []string
		wantNodes []string
		wantErr   error
	}{
		{
			name: "multiple records cannot crowd out other nodes",
			records: []Record{
				{ID: "node-a-best", NodeID: "node-a", Kind: "summary", Vector: []float32{1, 0}},
				{ID: "node-a-second", NodeID: "node-a", Kind: "era_summary", Vector: []float32{10, 1}},
				{ID: "node-a-third", NodeID: "node-a", Kind: "era_summary", Vector: []float32{10, 2}},
				{ID: "node-b", NodeID: "node-b", Kind: "summary", Vector: []float32{4, 3}},
				{ID: "node-c", NodeID: "node-c", Kind: "summary", Vector: []float32{3, 4}},
			},
			ctx:       context.Background(),
			query:     []float32{1, 0},
			limit:     3,
			wantIDs:   []string{"node-a-best", "node-b", "node-c"},
			wantNodes: []string{"node-a", "node-b", "node-c"},
		},
		{
			name: "ties choose stable record ID",
			records: []Record{
				{ID: "z", NodeID: "same-node", Kind: "summary", Vector: []float32{1, 0}},
				{ID: "a", NodeID: "same-node", Kind: "era_summary", Vector: []float32{1, 0}},
				{ID: "b", NodeID: "other-node", Kind: "summary", Vector: []float32{1, 0}},
				{ID: "c", NodeID: "third-node", Kind: "summary", Vector: []float32{0, 1}},
			},
			ctx:       context.Background(),
			query:     []float32{1, 0},
			limit:     2,
			wantIDs:   []string{"a", "b"},
			wantNodes: []string{"same-node", "other-node"},
		},
		{
			name: "limit above distinct count returns all nodes",
			records: []Record{
				{ID: "a", NodeID: "node-a", Kind: "summary", Vector: []float32{1, 0}},
				{ID: "a-era", NodeID: "node-a", Kind: "era_summary", Vector: []float32{0, 1}},
				{ID: "b", NodeID: "node-b", Kind: "summary", Vector: []float32{0, 1}},
			},
			ctx:       context.Background(),
			query:     []float32{1, 0},
			limit:     99,
			wantIDs:   []string{"a", "b"},
			wantNodes: []string{"node-a", "node-b"},
		},
		{
			name: "canceled",
			records: []Record{
				{ID: "a", NodeID: "node-a", Kind: "summary", Vector: []float32{1, 0}},
			},
			ctx:     canceled,
			query:   []float32{1, 0},
			limit:   1,
			wantErr: context.Canceled,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot, err := Build(2, test.records)
			if err != nil {
				t.Fatal(err)
			}
			got, err := snapshot.SearchDistinctNodes(test.ctx, test.query, test.limit)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("SearchDistinctNodes() error = %v, want errors.Is(%v)", err, test.wantErr)
			}
			if test.wantErr != nil {
				return
			}
			gotIDs := make([]string, len(got))
			gotNodes := make([]string, len(got))
			for i, hit := range got {
				gotIDs[i] = hit.ID
				gotNodes[i] = hit.NodeID
			}
			if !reflect.DeepEqual(gotIDs, test.wantIDs) {
				t.Fatalf("IDs = %v, want %v", gotIDs, test.wantIDs)
			}
			if !reflect.DeepEqual(gotNodes, test.wantNodes) {
				t.Fatalf("NodeIDs = %v, want %v", gotNodes, test.wantNodes)
			}
		})
	}
}

func TestSearchDistinctNodesValidation(t *testing.T) {
	snapshot, err := Build(2, []Record{{
		ID: "a", NodeID: "node-a", Kind: "summary", Vector: []float32{1, 0},
	}})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		index *Snapshot
		ctx   context.Context
		query []float32
		limit int
		want  error
	}{
		{name: "nil snapshot", ctx: context.Background(), query: []float32{1, 0}, limit: 1, want: ErrInvalidInput},
		{name: "nil context", index: snapshot, query: []float32{1, 0}, limit: 1, want: ErrInvalidInput},
		{name: "bad limit", index: snapshot, ctx: context.Background(), query: []float32{1, 0}, limit: 0, want: ErrInvalidInput},
		{name: "bad dimension", index: snapshot, ctx: context.Background(), query: []float32{1}, limit: 1, want: ErrInvalidInput},
		{name: "zero query", index: snapshot, ctx: context.Background(), query: []float32{0, 0}, limit: 1, want: ErrInvalidInput},
		{name: "NaN query", index: snapshot, ctx: context.Background(), query: []float32{float32(math.NaN()), 0}, limit: 1, want: ErrInvalidInput},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := test.index.SearchDistinctNodes(test.ctx, test.query, test.limit)
			if !errors.Is(err, test.want) {
				t.Fatalf("SearchDistinctNodes() error = %v, want errors.Is(%v)", err, test.want)
			}
		})
	}
}

func TestSearchDistinctNodesByKind(t *testing.T) {
	snapshot, err := Build(2, []Record{
		{ID: "current-z", NodeID: "current-same", Kind: "current", Vector: []float32{1, 0}},
		{ID: "current-a", NodeID: "current-same", Kind: "current", Vector: []float32{1, 0}},
		{ID: "current-b", NodeID: "current-other", Kind: "current", Vector: []float32{4, 3}},
		{ID: "risk-a", NodeID: "risk-a", Kind: "risk", Vector: []float32{3, 4}},
		{ID: "risk-b", NodeID: "risk-b", Kind: "risk", Vector: []float32{0, 1}},
		{ID: "history-a", NodeID: "history-a", Kind: "history", Vector: []float32{-1, 0}},
		{ID: "history-b", NodeID: "history-b", Kind: "history", Vector: []float32{1, 0}},
	})
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name    string
		ctx     context.Context
		kind    string
		limit   int
		wantIDs []string
		wantErr error
	}{
		{name: "current", ctx: context.Background(), kind: "current", limit: 3, wantIDs: []string{"current-a", "current-b"}},
		{name: "risk", ctx: context.Background(), kind: "risk", limit: 2, wantIDs: []string{"risk-a", "risk-b"}},
		{name: "history", ctx: context.Background(), kind: "history", limit: 1, wantIDs: []string{"history-b"}},
		{name: "empty kind", ctx: context.Background(), limit: 1, wantErr: ErrInvalidInput},
		{name: "canceled", ctx: canceled, kind: "current", limit: 1, wantErr: context.Canceled},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := snapshot.SearchDistinctNodesByKind(test.ctx, []float32{1, 0}, test.limit, test.kind)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("SearchDistinctNodesByKind() error = %v, want errors.Is(%v)", err, test.wantErr)
			}
			if test.wantErr != nil {
				return
			}
			gotIDs := make([]string, len(got))
			for i, hit := range got {
				gotIDs[i] = hit.ID
				if hit.Kind != test.kind {
					t.Fatalf("hit %q kind = %q, want %q", hit.ID, hit.Kind, test.kind)
				}
			}
			if !reflect.DeepEqual(gotIDs, test.wantIDs) {
				t.Fatalf("IDs = %v, want %v", gotIDs, test.wantIDs)
			}
		})
	}
}

func TestSearchDistinctNodesByKinds(t *testing.T) {
	snapshot, err := Build(2, []Record{
		{ID: "current-z", NodeID: "current-same", Kind: "current", Vector: []float32{1, 0}},
		{ID: "current-a", NodeID: "current-same", Kind: "current", Vector: []float32{1, 0}},
		{ID: "current-b", NodeID: "current-other", Kind: "current", Vector: []float32{4, 3}},
		{ID: "risk-a", NodeID: "risk-a", Kind: "risk", Vector: []float32{3, 4}},
		{ID: "risk-b", NodeID: "risk-b", Kind: "risk", Vector: []float32{0, 1}},
		{ID: "history-a", NodeID: "history-a", Kind: "history", Vector: []float32{-1, 0}},
		{ID: "history-b", NodeID: "history-b", Kind: "history", Vector: []float32{1, 0}},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := snapshot.SearchDistinctNodesByKinds(
		context.Background(), []float32{1, 0}, 2, []string{"current", "risk", "history"},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"current": {"current-a", "current-b"},
		"risk":    {"risk-a", "risk-b"},
		"history": {"history-b", "history-a"},
	}
	if len(got) != len(want) {
		t.Fatalf("result groups = %d, want %d", len(got), len(want))
	}
	for kind, wantIDs := range want {
		hits, exists := got[kind]
		if !exists {
			t.Fatalf("result is missing kind %q", kind)
		}
		gotIDs := make([]string, len(hits))
		for i, hit := range hits {
			gotIDs[i] = hit.ID
			if hit.Kind != kind {
				t.Fatalf("hit %q kind = %q, want %q", hit.ID, hit.Kind, kind)
			}
		}
		if !reflect.DeepEqual(gotIDs, wantIDs) {
			t.Fatalf("kind %q IDs = %v, want %v", kind, gotIDs, wantIDs)
		}
	}

	empty, err := snapshot.SearchDistinctNodesByKinds(
		context.Background(), []float32{1, 0}, 2, []string{"missing"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if hits, exists := empty["missing"]; !exists || hits == nil || len(hits) != 0 {
		t.Fatalf("missing kind result = %#v, exists=%v; want non-nil empty slice", hits, exists)
	}
}

func TestSearchDistinctNodesByKindsFilteredBackfillsBeforeCompetition(t *testing.T) {
	staleSameNodeHash := [32]byte{1}
	freshSameNodeHash := [32]byte{2}
	staleOtherNodeHash := [32]byte{3}
	freshHash := [32]byte{4}
	snapshot, err := Build(2, []Record{
		{ID: "same-stale-best", NodeID: "same", Kind: "current", SourceHash: staleSameNodeHash, Vector: []float32{1, 0}},
		{ID: "same-fresh-second", NodeID: "same", Kind: "current", SourceHash: freshSameNodeHash, Vector: []float32{4, 3}},
		{ID: "other-stale-best", NodeID: "other-stale", Kind: "current", SourceHash: staleOtherNodeHash, Vector: []float32{1, 0}},
		{ID: "fresh-b", NodeID: "fresh-b", Kind: "current", SourceHash: freshHash, Vector: []float32{3, 4}},
		{ID: "fresh-c", NodeID: "fresh-c", Kind: "current", SourceHash: freshHash, Vector: []float32{0, 1}},
		{ID: "unrequested", NodeID: "history", Kind: "history", SourceHash: freshHash, Vector: []float32{1, 0}},
	})
	if err != nil {
		t.Fatal(err)
	}

	var seen []RecordMetadata
	got, err := snapshot.SearchDistinctNodesByKindsFiltered(
		context.Background(),
		[]float32{1, 0},
		3,
		[]string{"current"},
		func(record RecordMetadata) bool {
			seen = append(seen, record)
			return record.SourceHash != staleSameNodeHash && record.SourceHash != staleOtherNodeHash
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	wantIDs := []string{"same-fresh-second", "fresh-b", "fresh-c"}
	gotIDs := make([]string, len(got["current"]))
	for i, hit := range got["current"] {
		gotIDs[i] = hit.ID
	}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("filtered IDs = %v, want %v", gotIDs, wantIDs)
	}
	if len(seen) != 5 {
		t.Fatalf("filter calls = %d, want 5 requested-kind records", len(seen))
	}
	if seen[0] != (RecordMetadata{
		ID:         "same-stale-best",
		NodeID:     "same",
		Kind:       "current",
		SourceHash: staleSameNodeHash,
	}) {
		t.Fatalf("first filter metadata = %#v, want complete immutable metadata", seen[0])
	}
	for _, record := range seen {
		if record.Kind != "current" || record.ID == "unrequested" {
			t.Fatalf("filter saw unrequested record: %#v", record)
		}
	}
}

func TestSearchDistinctNodesByKindsFilteredStableTiesAndContext(t *testing.T) {
	snapshot, err := Build(2, []Record{
		{ID: "z", NodeID: "same", Kind: "current", Vector: []float32{1, 0}},
		{ID: "a", NodeID: "same", Kind: "current", Vector: []float32{1, 0}},
		{ID: "b", NodeID: "other", Kind: "current", Vector: []float32{1, 0}},
		{ID: "c", NodeID: "third", Kind: "current", Vector: []float32{1, 0}},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := snapshot.SearchDistinctNodesByKindsFiltered(
		context.Background(), []float32{1, 0}, 2, []string{"current"}, func(RecordMetadata) bool { return true },
	)
	if err != nil {
		t.Fatal(err)
	}
	if gotIDs := []string{got["current"][0].ID, got["current"][1].ID}; !reflect.DeepEqual(gotIDs, []string{"a", "b"}) {
		t.Fatalf("stable filtered ties = %v, want [a b]", gotIDs)
	}

	ctx, cancel := context.WithCancel(context.Background())
	filterCalls := 0
	_, err = snapshot.SearchDistinctNodesByKindsFiltered(
		ctx, []float32{1, 0}, 2, []string{"current"}, func(RecordMetadata) bool {
			filterCalls++
			cancel()
			return true
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled-in-filter error = %v, want context.Canceled", err)
	}
	if filterCalls != 1 {
		t.Fatalf("filter calls after cancellation = %d, want 1", filterCalls)
	}
}

func TestSearchDistinctNodesByKindsManyUniqueNodesMatchesRecordTopK(t *testing.T) {
	const recordCount = 10_000
	records := make([]Record, recordCount)
	for i := range records {
		records[i] = Record{
			ID:     fmt.Sprintf("record-%05d", i),
			NodeID: fmt.Sprintf("node-%05d", i),
			Kind:   "current",
			Vector: []float32{float32(i%257 - 128), float32((i*31)%263 - 131)},
		}
		if records[i].Vector[0] == 0 && records[i].Vector[1] == 0 {
			records[i].Vector[1] = 1
		}
	}
	snapshot, err := Build(2, records)
	if err != nil {
		t.Fatal(err)
	}
	query := []float32{7, -3}
	want, err := snapshot.Search(context.Background(), query, 17)
	if err != nil {
		t.Fatal(err)
	}
	got, err := snapshot.SearchDistinctNodesByKinds(context.Background(), query, 17, []string{"current"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got["current"], want) {
		t.Fatalf("distinct Top-K over %d unique nodes differs from record Top-K:\n got=%#v\nwant=%#v", recordCount, got["current"], want)
	}
}

func TestSearchDistinctNodesByKindsFilteredMatchesFullSort(t *testing.T) {
	const recordCount = 5_000
	kinds := []string{"current", "risk", "history"}
	records := make([]Record, recordCount)
	for i := range records {
		records[i] = Record{
			ID:         fmt.Sprintf("record-%05d", i),
			NodeID:     fmt.Sprintf("node-%03d", (i*37)%701),
			Kind:       kinds[i%len(kinds)],
			SourceHash: [32]byte{byte(i % 7)},
			Vector:     []float32{float32(i%193 - 96), float32((i*17)%211 - 105)},
		}
		if records[i].Vector[0] == 0 && records[i].Vector[1] == 0 {
			records[i].Vector[1] = 1
		}
	}
	snapshot, err := Build(2, records)
	if err != nil {
		t.Fatal(err)
	}
	query := []float32{-2, 9}
	keep := func(record RecordMetadata) bool {
		return record.SourceHash[0] != 0 && record.SourceHash[0] != 3
	}
	got, err := snapshot.SearchDistinctNodesByKindsFiltered(context.Background(), query, 23, kinds, keep)
	if err != nil {
		t.Fatal(err)
	}

	all, err := snapshot.Search(context.Background(), query, recordCount)
	if err != nil {
		t.Fatal(err)
	}
	want := make(map[string][]Hit, len(kinds))
	seenNodes := make(map[string]map[string]struct{}, len(kinds))
	for _, kind := range kinds {
		want[kind] = []Hit{}
		seenNodes[kind] = make(map[string]struct{})
	}
	for _, hit := range all {
		if len(want[hit.Kind]) >= 23 || !keep(RecordMetadata{
			ID: hit.ID, NodeID: hit.NodeID, Kind: hit.Kind, SourceHash: hit.SourceHash,
		}) {
			continue
		}
		if _, duplicate := seenNodes[hit.Kind][hit.NodeID]; duplicate {
			continue
		}
		seenNodes[hit.Kind][hit.NodeID] = struct{}{}
		want[hit.Kind] = append(want[hit.Kind], hit)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bounded filtered multi-kind results differ from full-sort reference:\n got=%#v\nwant=%#v", got, want)
	}
}

func TestSearchDistinctNodesEvictedNodeCanReenterWithStrongerRecord(t *testing.T) {
	snapshot, err := Build(2, []Record{
		// node-a enters first, then node-c evicts it after node-b fills the heap.
		{ID: "node-a-old", NodeID: "node-a", Kind: "current", Vector: []float32{3, 4}},
		{ID: "node-b", NodeID: "node-b", Kind: "current", Vector: []float32{4, 3}},
		{ID: "node-c", NodeID: "node-c", Kind: "current", Vector: []float32{1, 1}},
		// A later, stronger record must let the previously evicted node re-enter.
		{ID: "node-a-new", NodeID: "node-a", Kind: "current", Vector: []float32{10, 1}},
	})
	if err != nil {
		t.Fatal(err)
	}

	hits, err := snapshot.SearchDistinctNodesByKind(context.Background(), []float32{1, 0}, 2, "current")
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{hits[0].ID, hits[1].ID}; !reflect.DeepEqual(got, []string{"node-a-new", "node-b"}) {
		t.Fatalf("re-entered Top-K IDs = %v, want [node-a-new node-b]", got)
	}
}

func TestSearchDistinctNodesRetainedWinnerUpdatesAfterHeapSwap(t *testing.T) {
	snapshot, err := Build(2, []Record{
		{ID: "node-a", NodeID: "node-a", Kind: "current", Vector: []float32{1, 10}},
		{ID: "node-b", NodeID: "node-b", Kind: "current", Vector: []float32{1, 5}},
		{ID: "node-c", NodeID: "node-c", Kind: "current", Vector: []float32{3, 10}},
		// Replacing node-a at the root with node-d forces heap.Fix to swap
		// node-d away from position zero and update the positions index.
		{ID: "node-d-old", NodeID: "node-d", Kind: "current", Vector: []float32{2, 5}},
		// The same retained node then improves at its post-swap position.
		{ID: "node-d-new", NodeID: "node-d", Kind: "current", Vector: []float32{10, 1}},
	})
	if err != nil {
		t.Fatal(err)
	}

	hits, err := snapshot.SearchDistinctNodesByKind(context.Background(), []float32{1, 0}, 3, "current")
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(hits))
	for i, hit := range hits {
		got[i] = hit.ID
	}
	if !reflect.DeepEqual(got, []string{"node-d-new", "node-c", "node-b"}) {
		t.Fatalf("post-swap winner update IDs = %v, want [node-d-new node-c node-b]", got)
	}
}

func TestSearchDistinctNodesRandomizedOracleAcrossPermutations(t *testing.T) {
	const (
		seed        = int64(2026071901)
		recordCount = 1_200
		dimensions  = 7
		limit       = 19
	)
	kinds := []string{"current", "risk", "history"}
	rng := rand.New(rand.NewSource(seed))
	records := make([]Record, recordCount)
	for i := range records {
		vector := make([]float32, dimensions)
		nonZero := false
		for j := range vector {
			vector[j] = float32(rng.Intn(13) - 6)
			nonZero = nonZero || vector[j] != 0
		}
		if !nonZero {
			vector[i%dimensions] = 1
		}
		records[i] = Record{
			ID:         fmt.Sprintf("record-%04d", i),
			NodeID:     fmt.Sprintf("node-%02d", rng.Intn(83)),
			Kind:       kinds[rng.Intn(len(kinds))],
			SourceHash: [32]byte{byte(rng.Intn(11))},
			Vector:     vector,
		}
	}
	query := []float32{3, -1, 4, -2, 5, -3, 6}
	keep := func(record RecordMetadata) bool {
		return record.SourceHash[0]%4 != 0
	}

	orders := make([][]Record, 0, 8)
	orders = append(orders, append([]Record(nil), records...))
	reversed := append([]Record(nil), records...)
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	orders = append(orders, reversed)
	for range 6 {
		permutation := rng.Perm(len(records))
		permuted := make([]Record, len(records))
		for i, original := range permutation {
			permuted[i] = records[original]
		}
		orders = append(orders, permuted)
	}

	var canonical map[string][]Hit
	for orderIndex, order := range orders {
		snapshot, err := Build(dimensions, order)
		if err != nil {
			t.Fatalf("order %d Build() error = %v", orderIndex, err)
		}
		want := fullMapDistinctNodeReference(t, snapshot, query, limit, kinds, keep)
		got, err := snapshot.SearchDistinctNodesByKindsFiltered(context.Background(), query, limit, kinds, keep)
		if err != nil {
			t.Fatalf("order %d filtered search error = %v", orderIndex, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("seed %d order %d differs from full-map oracle:\n got=%#v\nwant=%#v", seed, orderIndex, got, want)
		}
		if orderIndex == 0 {
			canonical = got
		} else if !reflect.DeepEqual(got, canonical) {
			t.Fatalf("seed %d order %d is not permutation invariant:\n got=%#v\ncanonical=%#v", seed, orderIndex, got, canonical)
		}
	}
}

func fullMapDistinctNodeReference(t *testing.T, snapshot *Snapshot, query []float32, limit int, kinds []string, keep RecordFilter) map[string][]Hit {
	t.Helper()
	normalized := make([]float32, len(query))
	if err := normalizeVectorContext(context.Background(), normalized, query); err != nil {
		t.Fatal(err)
	}
	requested := make(map[string]struct{}, len(kinds))
	bestByKind := make(map[string]map[string]Hit, len(kinds))
	for _, kind := range kinds {
		requested[kind] = struct{}{}
		bestByKind[kind] = make(map[string]Hit)
	}
	for i, record := range snapshot.records {
		if _, ok := requested[record.kind]; !ok {
			continue
		}
		metadata := RecordMetadata{
			ID: record.id, NodeID: record.nodeID, Kind: record.kind, SourceHash: record.sourceHash,
		}
		if keep != nil && !keep(metadata) {
			continue
		}
		start := i * snapshot.dimensions
		var score64 float64
		for j, value := range snapshot.vectors[start : start+snapshot.dimensions] {
			score64 += float64(value) * float64(normalized[j])
		}
		score := float32(score64)
		if score > 1 {
			score = 1
		} else if score < -1 {
			score = -1
		}
		candidate := Hit{
			ID: record.id, NodeID: record.nodeID, Kind: record.kind, SourceHash: record.sourceHash, Score: score,
		}
		if current, exists := bestByKind[record.kind][record.nodeID]; !exists || hitBetter(candidate, current) {
			bestByKind[record.kind][record.nodeID] = candidate
		}
	}

	result := make(map[string][]Hit, len(kinds))
	for _, kind := range kinds {
		hits := make([]Hit, 0, len(bestByKind[kind]))
		for _, hit := range bestByKind[kind] {
			hits = append(hits, hit)
		}
		sort.Slice(hits, func(i, j int) bool { return hitBetter(hits[i], hits[j]) })
		if len(hits) > limit {
			hits = hits[:limit]
		}
		if hits == nil {
			hits = []Hit{}
		}
		result[kind] = hits
	}
	return result
}

func TestSearchDistinctNodesByKindsValidation(t *testing.T) {
	snapshot, err := Build(2, []Record{{
		ID: "current", NodeID: "node", Kind: "current", Vector: []float32{1, 0},
	}})
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name    string
		ctx     context.Context
		kinds   []string
		wantErr error
	}{
		{name: "nil kinds", ctx: context.Background(), wantErr: ErrInvalidInput},
		{name: "empty kinds", ctx: context.Background(), kinds: []string{}, wantErr: ErrInvalidInput},
		{name: "empty kind", ctx: context.Background(), kinds: []string{"current", ""}, wantErr: ErrInvalidInput},
		{name: "duplicate kind", ctx: context.Background(), kinds: []string{"current", "current"}, wantErr: ErrInvalidInput},
		{name: "invalid utf8 kind", ctx: context.Background(), kinds: []string{string([]byte{0xff})}, wantErr: ErrInvalidInput},
		{name: "oversized kind", ctx: context.Background(), kinds: []string{strings.Repeat("k", int(defaultMaxStringBytes)+1)}, wantErr: ErrLimitExceeded},
		{name: "too many kinds", ctx: context.Background(), kinds: func() []string {
			out := make([]string, maxSearchKinds+1)
			for i := range out {
				out[i] = string(rune('a' + i))
			}
			return out
		}(), wantErr: ErrLimitExceeded},
		{name: "canceled", ctx: canceled, kinds: []string{"current"}, wantErr: context.Canceled},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := snapshot.SearchDistinctNodesByKinds(test.ctx, []float32{1, 0}, 1, test.kinds)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("SearchDistinctNodesByKinds() error = %v, want errors.Is(%v)", err, test.wantErr)
			}
		})
	}
}

func TestEmptySnapshotAndConcurrentSearch(t *testing.T) {
	empty, err := Build(3, nil)
	if err != nil {
		t.Fatal(err)
	}
	hits, err := empty.Search(context.Background(), []float32{1, 0, 0}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if hits == nil || len(hits) != 0 {
		t.Fatalf("empty hits = %#v, want non-nil empty slice", hits)
	}

	records := make([]Record, 100)
	for i := range records {
		records[i] = Record{
			ID:     string(rune(0x1000 + i)),
			NodeID: "node",
			Kind:   "summary",
			Vector: []float32{float32(i + 1), 1, -1},
		}
	}
	snapshot, err := Build(3, records)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				hits, err := snapshot.Search(context.Background(), []float32{1, 1, 0}, 7)
				if err != nil {
					t.Errorf("Search() error = %v", err)
					return
				}
				if len(hits) != 7 {
					t.Errorf("Search() returned %d hits, want 7", len(hits))
					return
				}
			}
		}()
	}
	wg.Wait()
}

func withLimits(change func(*Limits)) Limits {
	limits := DefaultLimits()
	change(&limits)
	return limits
}
