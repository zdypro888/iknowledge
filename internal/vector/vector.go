// Package vector implements the optional, derived semantic-search index.
//
// A Snapshot owns one immutable, row-major float32 matrix. Callers never get a
// reference to that matrix, so searches may run concurrently without locks.
package vector

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"unicode/utf8"
)

var (
	// ErrInvalidInput reports malformed records, vectors, queries, or limits.
	ErrInvalidInput = errors.New("vector: invalid input")
	// ErrLimitExceeded reports an otherwise well-formed input that exceeds a
	// configured resource bound.
	ErrLimitExceeded = errors.New("vector: resource limit exceeded")
)

const (
	defaultMaxRecords       = uint64(100_000)
	defaultMaxDimensions    = uint32(4_096)
	defaultMaxVectorBytes   = uint64(512 << 20)
	defaultMaxMetadataBytes = uint64(64 << 20)
	defaultMaxStringBytes   = uint32(4 << 10)
	maxSearchKinds          = 64
)

// Limits bounds both Snapshot construction and binary decoding. Every field
// must be non-zero. Use DefaultLimits as the safe starting point.
type Limits struct {
	MaxRecords       uint64
	MaxDimensions    uint32
	MaxVectorBytes   uint64
	MaxMetadataBytes uint64
	MaxStringBytes   uint32
}

// DefaultLimits returns conservative limits for a derived per-repository
// cache. In particular, limits are expressed in bytes rather than record count
// alone because Flat index cost is proportional to records * dimensions.
func DefaultLimits() Limits {
	return Limits{
		MaxRecords:       defaultMaxRecords,
		MaxDimensions:    defaultMaxDimensions,
		MaxVectorBytes:   defaultMaxVectorBytes,
		MaxMetadataBytes: defaultMaxMetadataBytes,
		MaxStringBytes:   defaultMaxStringBytes,
	}
}

// Record is one source record supplied while building a Snapshot. Build copies
// and normalizes Vector; callers retain ownership of every input slice.
type Record struct {
	ID         string
	NodeID     string
	Kind       string
	SourceHash [32]byte
	Vector     []float32
}

// Hit is an immutable search result. It intentionally does not expose the
// stored vector.
type Hit struct {
	ID         string
	NodeID     string
	Kind       string
	SourceHash [32]byte
	Score      float32
}

// RecordMetadata is the immutable subset of a stored record exposed to search
// filters. It deliberately omits the vector so a filter cannot retain or
// mutate Snapshot matrix storage.
type RecordMetadata struct {
	ID         string
	NodeID     string
	Kind       string
	SourceHash [32]byte
}

// RecordFilter decides whether a stored record may compete for its node's
// winning slot and the final Top-K. Returning true keeps the record. Filters
// are called synchronously in snapshot order and should be cheap.
type RecordFilter func(RecordMetadata) bool

// Status describes the logical payload owned by a Snapshot. VectorBytes is the
// exact contiguous matrix size; MetadataBytes is its encoded size and excludes
// Go object and allocator overhead.
type Status struct {
	Records       int
	Dimensions    int
	VectorBytes   uint64
	MetadataBytes uint64
	Normalized    bool
}

type metadata struct {
	id         string
	nodeID     string
	kind       string
	sourceHash [32]byte
}

// Snapshot is an immutable Flat index. vectors is a single contiguous,
// row-major matrix; the vector for record i begins at i*dimensions.
type Snapshot struct {
	dimensions    int
	records       []metadata
	vectors       []float32
	metadataBytes uint64
}

// BuildPlan is the immutable result of Preflight. It owns copied metadata but
// no vectors. A plan may be reused to create more than one Builder.
type BuildPlan struct {
	dimensions    int
	records       []metadata
	elements      int
	metadataBytes uint64
	valid         bool
}

// Builder fills one preallocated, contiguous matrix in plan order. It is
// intentionally single-writer; publish the immutable Snapshot returned by
// Finish for concurrent searches.
type Builder struct {
	plan     *BuildPlan
	vectors  []float32
	appended int
	finished bool
}

// Build validates records with DefaultLimits and returns an immutable Snapshot.
func Build(dimensions int, records []Record) (*Snapshot, error) {
	return BuildWithLimitsContext(context.Background(), dimensions, records, DefaultLimits())
}

// BuildWithLimits is the context-free compatibility wrapper around
// BuildWithLimitsContext.
func BuildWithLimits(dimensions int, records []Record, limits Limits) (*Snapshot, error) {
	return BuildWithLimitsContext(context.Background(), dimensions, records, limits)
}

// BuildWithLimitsContext validates all metadata and resource requirements
// before allocating the matrix, then copies and normalizes each input vector.
// It never keeps caller-owned slices.
func BuildWithLimitsContext(ctx context.Context, dimensions int, records []Record, limits Limits) (*Snapshot, error) {
	plan, err := Preflight(ctx, dimensions, records, limits)
	if err != nil {
		return nil, err
	}
	builder, err := NewBuilder(ctx, plan)
	if err != nil {
		return nil, err
	}
	if err := builder.appendRecords(ctx, records); err != nil {
		return nil, err
	}
	return builder.Finish(ctx)
}

// Preflight validates dimensions, resource bounds, metadata, and duplicate
// record IDs without reading or retaining Record.Vector. Only after every
// record passes does it allocate and copy the compact metadata plan.
func Preflight(ctx context.Context, dimensions int, records []Record, limits Limits) (*BuildPlan, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil context", ErrInvalidInput)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateLimits(limits); err != nil {
		return nil, err
	}
	if dimensions <= 0 || uint64(dimensions) > uint64(limits.MaxDimensions) {
		if dimensions > 0 {
			return nil, fmt.Errorf("%w: dimensions %d exceed maximum %d", ErrLimitExceeded, dimensions, limits.MaxDimensions)
		}
		return nil, fmt.Errorf("%w: dimensions must be positive", ErrInvalidInput)
	}
	count := uint64(len(records))
	if count > limits.MaxRecords {
		return nil, fmt.Errorf("%w: records %d exceed maximum %d", ErrLimitExceeded, count, limits.MaxRecords)
	}
	elements, _, err := checkedVectorSize(count, uint32(dimensions), limits)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{}, len(records))
	var metadataBytes uint64
	for i, record := range records {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if err := validateRecordMetadata(record.ID, record.NodeID, record.Kind, limits); err != nil {
			return nil, fmt.Errorf("record %d: %w", i, err)
		}
		if _, exists := seen[record.ID]; exists {
			return nil, fmt.Errorf("%w: duplicate record ID %q", ErrInvalidInput, record.ID)
		}
		seen[record.ID] = struct{}{}
		recordMetadataBytes := encodedMetadataSize(record.ID, record.NodeID, record.Kind)
		metadataBytes, err = checkedAdd(metadataBytes, recordMetadataBytes)
		if err != nil || metadataBytes > limits.MaxMetadataBytes {
			return nil, fmt.Errorf("%w: metadata exceeds maximum %d bytes", ErrLimitExceeded, limits.MaxMetadataBytes)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	metas := make([]metadata, len(records))
	for i, record := range records {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		metas[i] = metadata{
			id:         record.ID,
			nodeID:     record.NodeID,
			kind:       record.Kind,
			sourceHash: record.SourceHash,
		}
	}

	return &BuildPlan{
		dimensions:    dimensions,
		records:       metas,
		elements:      elements,
		metadataBytes: metadataBytes,
		valid:         true,
	}, nil
}

// NewBuilder allocates exactly one matrix for a validated plan.
func NewBuilder(ctx context.Context, plan *BuildPlan) (*Builder, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil context", ErrInvalidInput)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if plan == nil || !plan.valid || plan.dimensions <= 0 || plan.elements < 0 ||
		plan.elements != len(plan.records)*plan.dimensions {
		return nil, fmt.Errorf("%w: invalid build plan", ErrInvalidInput)
	}
	vectors := make([]float32, plan.elements)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &Builder{plan: plan, vectors: vectors}, nil
}

// Append copies and normalizes a provider batch into the next matrix rows. The
// batch corresponds positionally to the next records in the BuildPlan. A failed
// or canceled batch does not advance the builder and may be retried.
func (b *Builder) Append(ctx context.Context, vectors [][]float32) error {
	if err := b.ready(ctx); err != nil {
		return err
	}
	if len(vectors) > len(b.plan.records)-b.appended {
		return fmt.Errorf("%w: append has %d vectors, only %d records remain", ErrInvalidInput, len(vectors), len(b.plan.records)-b.appended)
	}
	startRecord := b.appended
	for i, vector := range vectors {
		if i&63 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if err := b.writeVector(ctx, startRecord+i, vector); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	b.appended += len(vectors)
	return nil
}

func (b *Builder) appendRecords(ctx context.Context, records []Record) error {
	if err := b.ready(ctx); err != nil {
		return err
	}
	if len(records) != len(b.plan.records) {
		return fmt.Errorf("%w: got %d records for %d-record plan", ErrInvalidInput, len(records), len(b.plan.records))
	}
	for i, record := range records {
		if i&63 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if err := b.writeVector(ctx, i, record.Vector); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	b.appended = len(records)
	return nil
}

func (b *Builder) writeVector(ctx context.Context, record int, vector []float32) error {
	if len(vector) != b.plan.dimensions {
		return fmt.Errorf("%w: record %q has dimension %d, want %d", ErrInvalidInput, b.plan.records[record].id, len(vector), b.plan.dimensions)
	}
	start := record * b.plan.dimensions
	if err := normalizeVectorContext(ctx, b.vectors[start:start+b.plan.dimensions], vector); err != nil {
		return fmt.Errorf("record %q: %w", b.plan.records[record].id, err)
	}
	return nil
}

func (b *Builder) ready(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil context", ErrInvalidInput)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if b == nil || b.plan == nil || !b.plan.valid || b.finished {
		return fmt.Errorf("%w: builder is nil, invalid, or already finished", ErrInvalidInput)
	}
	return nil
}

// Finish publishes the Snapshot only after every planned row was appended. A
// Builder can finish once; the returned Snapshot takes ownership of its matrix.
func (b *Builder) Finish(ctx context.Context) (*Snapshot, error) {
	if err := b.ready(ctx); err != nil {
		return nil, err
	}
	if b.appended != len(b.plan.records) {
		return nil, fmt.Errorf("%w: appended %d of %d vectors", ErrInvalidInput, b.appended, len(b.plan.records))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	snapshot := &Snapshot{
		dimensions:    b.plan.dimensions,
		records:       b.plan.records,
		vectors:       b.vectors,
		metadataBytes: b.plan.metadataBytes,
	}
	b.finished = true
	b.vectors = nil
	return snapshot, nil
}

// Status returns a value copy and is safe to call concurrently with Search.
func (s *Snapshot) Status() Status {
	if s == nil {
		return Status{}
	}
	return Status{
		Records:       len(s.records),
		Dimensions:    s.dimensions,
		VectorBytes:   uint64(len(s.vectors)) * 4,
		MetadataBytes: s.metadataBytes,
		Normalized:    true,
	}
}

// Search performs an exact cosine search. The query is validated and
// normalized without mutation. Results are sorted by descending score, then by
// ascending record ID, making ties deterministic regardless of input order.
func (s *Snapshot) Search(ctx context.Context, query []float32, limit int) ([]Hit, error) {
	if s == nil {
		return nil, fmt.Errorf("%w: nil snapshot", ErrInvalidInput)
	}
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil context", ErrInvalidInput)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, fmt.Errorf("%w: limit must be positive", ErrInvalidInput)
	}
	if len(query) != s.dimensions {
		return nil, fmt.Errorf("%w: query has dimension %d, want %d", ErrInvalidInput, len(query), s.dimensions)
	}
	normalized := make([]float32, s.dimensions)
	if err := normalizeVectorContext(ctx, normalized, query); err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	if len(s.records) == 0 {
		return []Hit{}, nil
	}
	if limit > len(s.records) {
		limit = len(s.records)
	}

	best := make(hitHeap, 0, limit)
	for i, record := range s.records {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		start := i * s.dimensions
		var score64 float64
		for j, value := range s.vectors[start : start+s.dimensions] {
			score64 += float64(value) * float64(normalized[j])
		}
		score := float32(score64)
		if score > 1 {
			score = 1
		} else if score < -1 {
			score = -1
		}
		candidate := Hit{
			ID:         record.id,
			NodeID:     record.nodeID,
			Kind:       record.kind,
			SourceHash: record.sourceHash,
			Score:      score,
		}
		if len(best) < limit {
			heap.Push(&best, candidate)
			continue
		}
		if hitBetter(candidate, best[0]) {
			best[0] = candidate
			heap.Fix(&best, 0)
		}
	}

	result := []Hit(best)
	sort.Slice(result, func(i, j int) bool { return hitBetter(result[i], result[j]) })
	return result, nil
}

// SearchDistinctNodes performs the same exact cosine scan as Search, but
// returns at most one record per node. When several records for a node have
// the same score, the record with the lexicographically smaller ID wins.
// Results are sorted by descending score, then by ascending winning record ID.
func (s *Snapshot) SearchDistinctNodes(ctx context.Context, query []float32, limit int) ([]Hit, error) {
	groups, err := s.searchDistinctNodeGroups(ctx, query, limit, nil, true, nil)
	if err != nil {
		return nil, err
	}
	return groups[""], nil
}

// SearchDistinctNodesByKind is SearchDistinctNodes restricted to records whose
// Kind exactly equals kind. The filter is applied before records compete for a
// node's winning slot.
func (s *Snapshot) SearchDistinctNodesByKind(ctx context.Context, query []float32, limit int, kind string) ([]Hit, error) {
	groups, err := s.SearchDistinctNodesByKindsFiltered(ctx, query, limit, []string{kind}, nil)
	if err != nil {
		return nil, err
	}
	return groups[kind], nil
}

// SearchDistinctNodesByKinds performs one exact matrix scan for all requested
// kinds. Each result independently applies the same per-node winner and stable
// Top-K rules as SearchDistinctNodesByKind. Every requested kind is present in
// the returned map, including kinds with no matching records.
func (s *Snapshot) SearchDistinctNodesByKinds(ctx context.Context, query []float32, limit int, kinds []string) (map[string][]Hit, error) {
	return s.SearchDistinctNodesByKindsFiltered(ctx, query, limit, kinds, nil)
}

// SearchDistinctNodesByKindsFiltered is SearchDistinctNodesByKinds with a
// metadata filter applied before dot-product work, per-node winner selection,
// and Top-K competition. This ordering lets callers exclude stale records
// without allowing them to displace a valid record from the same or another
// node. A nil filter keeps every record. The filter is called only for records
// whose Kind was requested.
func (s *Snapshot) SearchDistinctNodesByKindsFiltered(ctx context.Context, query []float32, limit int, kinds []string, filter RecordFilter) (map[string][]Hit, error) {
	return s.searchDistinctNodeGroups(ctx, query, limit, kinds, false, filter)
}

func (s *Snapshot) searchDistinctNodeGroups(ctx context.Context, query []float32, limit int, kinds []string, allKinds bool, filter RecordFilter) (map[string][]Hit, error) {
	if s == nil {
		return nil, fmt.Errorf("%w: nil snapshot", ErrInvalidInput)
	}
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil context", ErrInvalidInput)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, fmt.Errorf("%w: limit must be positive", ErrInvalidInput)
	}
	groupOrder := []string{""}
	requestedKinds := make(map[string]struct{}, 1)
	if allKinds {
		requestedKinds[""] = struct{}{}
	} else {
		if len(kinds) == 0 {
			return nil, fmt.Errorf("%w: kinds must not be empty", ErrInvalidInput)
		}
		if len(kinds) > maxSearchKinds {
			return nil, fmt.Errorf("%w: requested kinds %d exceed maximum %d", ErrLimitExceeded, len(kinds), maxSearchKinds)
		}
		groupOrder = make([]string, 0, len(kinds))
		requestedKinds = make(map[string]struct{}, len(kinds))
		for i, kind := range kinds {
			if i&255 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			if kind == "" || !utf8.ValidString(kind) {
				return nil, fmt.Errorf("%w: kind at index %d must be non-empty valid UTF-8", ErrInvalidInput, i)
			}
			if len(kind) > int(defaultMaxStringBytes) {
				return nil, fmt.Errorf("%w: kind at index %d exceeds %d bytes", ErrLimitExceeded, i, defaultMaxStringBytes)
			}
			if _, duplicate := requestedKinds[kind]; duplicate {
				return nil, fmt.Errorf("%w: duplicate kind %q", ErrInvalidInput, kind)
			}
			requestedKinds[kind] = struct{}{}
			groupOrder = append(groupOrder, kind)
		}
	}
	if len(query) != s.dimensions {
		return nil, fmt.Errorf("%w: query has dimension %d, want %d", ErrInvalidInput, len(query), s.dimensions)
	}
	normalized := make([]float32, s.dimensions)
	if err := normalizeVectorContext(ctx, normalized, query); err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	effectiveLimit := limit
	if effectiveLimit > len(s.records) {
		effectiveLimit = len(s.records)
	}
	bestByKind := make(map[string]*distinctTopK, len(groupOrder))
	for _, kind := range groupOrder {
		bestByKind[kind] = newDistinctTopK(effectiveLimit)
	}

	for i, record := range s.records {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		groupKey := ""
		if !allKinds {
			if _, requested := requestedKinds[record.kind]; !requested {
				continue
			}
			groupKey = record.kind
		}
		if filter != nil {
			keep := filter(RecordMetadata{
				ID:         record.id,
				NodeID:     record.nodeID,
				Kind:       record.kind,
				SourceHash: record.sourceHash,
			})
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if !keep {
				continue
			}
		}
		start := i * s.dimensions
		var score64 float64
		for j, value := range s.vectors[start : start+s.dimensions] {
			score64 += float64(value) * float64(normalized[j])
		}
		score := float32(score64)
		if score > 1 {
			score = 1
		} else if score < -1 {
			score = -1
		}
		candidate := Hit{
			ID:         record.id,
			NodeID:     record.nodeID,
			Kind:       record.kind,
			SourceHash: record.sourceHash,
			Score:      score,
		}
		bestByKind[groupKey].consider(candidate)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	results := make(map[string][]Hit, len(groupOrder))
	for _, kind := range groupOrder {
		results[kind] = bestByKind[kind].sorted()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// distinctTopK maintains the exact Top-K node winners for every processed
// prefix. Nodes outside the retained K need no state: once a winner is evicted,
// K other node winners are already better and can only improve. A later record
// for that node is considered normally and can re-enter only if it is itself
// better than the current worst retained winner.
type distinctTopK struct {
	limit int
	heap  distinctHitHeap
}

func newDistinctTopK(limit int) *distinctTopK {
	initialCapacity := limit
	if initialCapacity > 16 {
		initialCapacity = 16
	}
	return &distinctTopK{
		limit: limit,
		heap: distinctHitHeap{
			hits:      make([]Hit, 0, initialCapacity),
			positions: make(map[string]int, initialCapacity),
		},
	}
}

func (d *distinctTopK) consider(candidate Hit) {
	if d.limit == 0 {
		return
	}
	if position, retained := d.heap.positions[candidate.NodeID]; retained {
		if hitBetter(candidate, d.heap.hits[position]) {
			d.heap.hits[position] = candidate
			heap.Fix(&d.heap, position)
		}
		return
	}
	if d.heap.Len() < d.limit {
		heap.Push(&d.heap, candidate)
		return
	}
	if !hitBetter(candidate, d.heap.hits[0]) {
		return
	}
	delete(d.heap.positions, d.heap.hits[0].NodeID)
	d.heap.hits[0] = candidate
	d.heap.positions[candidate.NodeID] = 0
	heap.Fix(&d.heap, 0)
}

func (d *distinctTopK) sorted() []Hit {
	if d == nil || d.heap.Len() == 0 {
		return []Hit{}
	}
	result := append([]Hit(nil), d.heap.hits...)
	sort.Slice(result, func(i, j int) bool { return hitBetter(result[i], result[j]) })
	return result
}

func validateLimits(limits Limits) error {
	if limits.MaxRecords == 0 || limits.MaxDimensions == 0 || limits.MaxVectorBytes == 0 ||
		limits.MaxMetadataBytes == 0 || limits.MaxStringBytes == 0 {
		return fmt.Errorf("%w: every limit must be non-zero", ErrInvalidInput)
	}
	return nil
}

func validateRecordMetadata(id, nodeID, kind string, limits Limits) error {
	fields := [...]struct {
		name  string
		value string
	}{
		{name: "record ID", value: id},
		{name: "node ID", value: nodeID},
		{name: "kind", value: kind},
	}
	for _, field := range fields {
		name, value := field.name, field.value
		if value == "" {
			return fmt.Errorf("%w: %s must not be empty", ErrInvalidInput, name)
		}
		if !utf8.ValidString(value) {
			return fmt.Errorf("%w: %s is not valid UTF-8", ErrInvalidInput, name)
		}
		if uint64(len(value)) > uint64(limits.MaxStringBytes) {
			return fmt.Errorf("%w: %s length %d exceeds maximum %d", ErrLimitExceeded, name, len(value), limits.MaxStringBytes)
		}
	}
	return nil
}

func normalizeVectorContext(ctx context.Context, dst, src []float32) error {
	var normSquared float64
	for i, value := range src {
		if i&8191 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return fmt.Errorf("%w: vector component %d is not finite", ErrInvalidInput, i)
		}
		normSquared += float64(value) * float64(value)
	}
	if normSquared == 0 {
		return fmt.Errorf("%w: zero vector", ErrInvalidInput)
	}
	norm := math.Sqrt(normSquared)
	for i, value := range src {
		if i&8191 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		dst[i] = float32(float64(value) / norm)
	}
	return nil
}

func validateNormalizedVectorContext(ctx context.Context, vector []float32) error {
	var normSquared float64
	for i, value := range vector {
		if i&8191 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return fmt.Errorf("%w: vector component %d is not finite", ErrInvalidInput, i)
		}
		normSquared += float64(value) * float64(value)
	}
	if normSquared == 0 {
		return fmt.Errorf("%w: zero vector", ErrInvalidInput)
	}
	if math.Abs(normSquared-1) > 1e-4 {
		return fmt.Errorf("%w: vector is not L2-normalized (squared norm %.8f)", ErrInvalidInput, normSquared)
	}
	return nil
}

func checkedVectorSize(count uint64, dimensions uint32, limits Limits) (int, uint64, error) {
	if dimensions == 0 {
		return 0, 0, fmt.Errorf("%w: dimensions must be positive", ErrInvalidInput)
	}
	if count > ^uint64(0)/uint64(dimensions) {
		return 0, 0, fmt.Errorf("%w: vector element count overflows", ErrLimitExceeded)
	}
	elements := count * uint64(dimensions)
	if elements > ^uint64(0)/4 {
		return 0, 0, fmt.Errorf("%w: vector byte count overflows", ErrLimitExceeded)
	}
	vectorBytes := elements * 4
	if vectorBytes > limits.MaxVectorBytes {
		return 0, 0, fmt.Errorf("%w: vectors require %d bytes, maximum is %d", ErrLimitExceeded, vectorBytes, limits.MaxVectorBytes)
	}
	maxInt := uint64(^uint(0) >> 1)
	if uint64(dimensions) > maxInt {
		return 0, 0, fmt.Errorf("%w: dimensions do not fit this platform", ErrLimitExceeded)
	}
	if elements > maxInt {
		return 0, 0, fmt.Errorf("%w: vector element count does not fit this platform", ErrLimitExceeded)
	}
	return int(elements), vectorBytes, nil
}

func encodedMetadataSize(id, nodeID, kind string) uint64 {
	// Three uint32 length prefixes plus the SHA-256 source hash.
	return 3*4 + 32 + uint64(len(id)+len(nodeID)+len(kind))
}

func checkedAdd(a, b uint64) (uint64, error) {
	if a > ^uint64(0)-b {
		return 0, fmt.Errorf("%w: byte count overflows", ErrLimitExceeded)
	}
	return a + b, nil
}

func hitBetter(a, b Hit) bool {
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	return a.ID < b.ID
}

// hitHeap is ordered with the worst retained hit at its root.
type hitHeap []Hit

func (h hitHeap) Len() int { return len(h) }
func (h hitHeap) Less(i, j int) bool {
	if h[i].Score != h[j].Score {
		return h[i].Score < h[j].Score
	}
	return h[i].ID > h[j].ID
}
func (h hitHeap) Swap(i, j int)   { h[i], h[j] = h[j], h[i] }
func (h *hitHeap) Push(value any) { *h = append(*h, value.(Hit)) }
func (h *hitHeap) Pop() any {
	old := *h
	last := len(old) - 1
	value := old[last]
	*h = old[:last]
	return value
}

// distinctHitHeap is ordered with the worst retained node winner at its root.
// positions is updated by every heap swap, allowing a later record to update
// a retained node winner in O(log limit) without a map for discarded nodes.
type distinctHitHeap struct {
	hits      []Hit
	positions map[string]int
}

func (h *distinctHitHeap) Len() int { return len(h.hits) }
func (h *distinctHitHeap) Less(i, j int) bool {
	if h.hits[i].Score != h.hits[j].Score {
		return h.hits[i].Score < h.hits[j].Score
	}
	return h.hits[i].ID > h.hits[j].ID
}
func (h *distinctHitHeap) Swap(i, j int) {
	h.hits[i], h.hits[j] = h.hits[j], h.hits[i]
	h.positions[h.hits[i].NodeID] = i
	h.positions[h.hits[j].NodeID] = j
}
func (h *distinctHitHeap) Push(value any) {
	hit := value.(Hit)
	h.positions[hit.NodeID] = len(h.hits)
	h.hits = append(h.hits, hit)
}
func (h *distinctHitHeap) Pop() any {
	last := len(h.hits) - 1
	hit := h.hits[last]
	delete(h.positions, hit.NodeID)
	h.hits[last] = Hit{}
	h.hits = h.hits[:last]
	return hit
}
