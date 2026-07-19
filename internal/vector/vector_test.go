package vector

import (
	"context"
	"errors"
	"math"
	"reflect"
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
