package engine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zdypro888/iknowledge/internal/semantic"
	"github.com/zdypro888/iknowledge/internal/vector"
)

const (
	semanticIndexRel       = "local/vector.idx"
	semanticIndexVersion   = uint16(1)
	semanticWrapperSize    = 48
	semanticMaxMetadata    = 64 << 10
	semanticEmbedBatchSize = 32
	semanticQueryCacheTTL  = 2 * time.Minute
	semanticQueryCacheCap  = 256
	semanticFailureBackoff = 3 * time.Second
	// MCP sync must finish comfortably before the ordinary HTTP server's
	// 10-minute write budget. CLI force rebuild has its own 30-minute context
	// and is intentionally not constrained by these interactive limits.
	semanticMCPSyncTimeout    = 8 * time.Minute
	semanticMCPSyncMaxRecords = 3000 // 100 provider batches of 30 documents.
	// Preview 默认只允许一个在途 provider 请求：serve 的所有仓共享，单仓
	// CLI 使用同容量本地 gate。拿到 slot 后 query/probe 会二次查 cache，因此
	// 相同 cache miss 不会排队放大费用。
	semanticProviderConcurrency = 1
	// Query vectors may be cached, but a Flat scan can still touch up to the
	// configured 512MiB matrix. Bound concurrent scans independently from the
	// provider gate so cache hits cannot amplify CPU/memory bandwidth without
	// limit; serve shares this gate across repositories.
	semanticSearchConcurrency   = 2
	semanticProviderMaxResponse = 16 << 20
	semanticProbeText           = "iknowledge semantic model probe v1: code decisions pitfalls architecture"
)

var semanticIndexMagic = [8]byte{'I', 'K', 'S', 'E', 'M', 'I', 'D', 'X'}

type semanticIndexMetadata struct {
	Schema              int    `json:"schema"`
	Generation          string `json:"generation"`
	SettingsFingerprint string `json:"settings_fingerprint"`
	EmbedderFingerprint string `json:"embedder_fingerprint"`
	// ProbeFingerprint is the document-mode canary observed throughout rebuild.
	ProbeFingerprint      string `json:"probe_fingerprint"`
	QueryProbeFingerprint string `json:"query_probe_fingerprint"`
	SourceFingerprint     string `json:"source_fingerprint"`
	Dimensions            int    `json:"dimensions"`
	Records               int    `json:"records"`
	BuiltAt               string `json:"built_at"`
}

type semanticQueryCacheEntry struct {
	vector            []float32
	canaryFingerprint string
	expiresAt         time.Time
}

type semanticSyncFlight struct {
	done     chan struct{}
	result   string
	err      error
	finished bool
	// retryable means the owner, rather than the shared operation, was
	// cancelled. Live waiters must elect a new owner instead of inheriting one
	// unrelated MCP session's cancellation.
	retryable   bool
	retainUntil time.Time
}

type semanticFileIdentity struct {
	Size             int64
	HeaderChecksum   [32]byte
	MetadataChecksum [32]byte
	VectorChecksum   [32]byte
}

// semanticRuntime 与主知识 runtime 分锁。snapshot 一经发布便不可变；Flat
// 搜索持短期 resident lease，provider 网络不持有该锁或主 rt.mu。
type semanticRuntime struct {
	mu sync.Mutex
	// residentMu is the lifetime barrier for immutable snapshots. Reload,
	// rebuild publication and eviction take it, and a Flat scan holds the same
	// short exclusive lease. Therefore a replacement never decodes a new matrix
	// while an old matrix can still be referenced by a search; provider I/O is
	// deliberately outside the lease.
	residentMu contextMutex
	// sourceResidentMu is the generation lifetime barrier for immutable source
	// manifests. Production callers hold a read lease while using the records
	// map; publication/invalidation take the write side before replacing the map
	// and its daemon-wide accounting.
	sourceResidentMu contextRWMutex
	loadedKey        string
	loadedAt         time.Time
	loadedFile       semanticFileIdentity
	snapshot         *vector.Snapshot
	metadata         semanticIndexMetadata
	loadErr          string
	// Payload corruption belongs to the immutable file generation, not to a
	// settings+source cache key. Knowledge may change while the same broken file
	// remains on disk; status must keep reporting that proven corruption.
	corruptFile semanticFileIdentity
	corruptErr  string
	lastError   string

	failureUntil time.Time

	queryCache   map[string]semanticQueryCacheEntry
	syncMu       sync.Mutex
	syncFlight   *semanticSyncFlight
	rebuildGate  chan struct{}
	sourceGate   chan struct{}
	providerGate chan struct{}
	searchGate   chan struct{}
	process      *SemanticProcessCoordinator
	building     bool
	closing      bool
}

// SemanticRebuildReport 是显式重建结果。重建不会由 serve/recall 自动触发。
type SemanticRebuildReport struct {
	Records       int
	Dimensions    int
	VectorBytes   uint64
	MetadataBytes uint64
	Model         string
	Endpoint      string
	Fingerprint   string
}

// SyncSemantic is the only MCP-authorized semantic mutation. It never changes
// endpoint/model/profile or consent; it only rebuilds the derived index when
// the user has previously selected an ai-* policy through the local CLI.
func (e *Engine) SyncSemantic(ctx context.Context) (string, error) {
	if ctx == nil {
		return "", fmt.Errorf("semantic sync: nil context")
	}
	for {
		e.semantic.syncMu.Lock()
		if flight := e.semantic.syncFlight; flight != nil {
			if flight.finished {
				if time.Now().Before(flight.retainUntil) {
					result, err := flight.result, flight.err
					e.semantic.syncMu.Unlock()
					return result, err
				}
				e.semantic.syncFlight = nil
			} else {
				e.semantic.syncMu.Unlock()
				select {
				case <-flight.done:
					if flight.retryable && ctx.Err() == nil {
						continue
					}
					return flight.result, flight.err
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}
		}
		flight := &semanticSyncFlight{done: make(chan struct{})}
		e.semantic.syncFlight = flight
		e.semantic.syncMu.Unlock()

		result, err := e.syncSemanticOwner(ctx)
		e.semantic.syncMu.Lock()
		flight.result, flight.err, flight.finished = result, err, true
		flight.retryable = ctx.Err() != nil &&
			(errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
		if err != nil && !flight.retryable {
			// Collapse both already-waiting sessions and a short arrival burst after
			// a paid/provider failure. Owner cancellation remains immediately
			// retryable by another live session.
			flight.retainUntil = time.Now().Add(semanticFailureBackoff)
		} else if e.semantic.syncFlight == flight {
			e.semantic.syncFlight = nil
		}
		close(flight.done)
		e.semantic.syncMu.Unlock()
		return result, err
	}
}

func (e *Engine) syncSemanticOwner(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, semanticMCPSyncTimeout)
	defer cancel()
	releaseRebuild, err := e.acquireSemanticRebuild(ctx)
	if err != nil {
		return "", err
	}
	defer releaseRebuild()
	// The gate is acquired before reading authorization or health. Concurrent
	// MCP sessions therefore observe the generation published by the owner and
	// do not queue a second paid rebuild based on the same stale no-index view.
	cfg, err := LoadSemanticSettings(e.Store)
	if err != nil {
		_ = e.evictSemanticResidentStateContext(ctx, semanticIndexMetadata{})
		_ = e.evictSemanticSourceStateContext(ctx)
		return "", err
	}
	if !cfg.Enabled {
		_ = e.evictSemanticResidentStateContext(ctx, semanticIndexMetadata{})
		_ = e.evictSemanticSourceStateContext(ctx)
		return "", kbErr("SEMANTIC_DISABLED", "semantic 尚未启用", "由用户运行 iknowledge semantic configure/enable")
	}
	if cfg.RebuildPolicy == SemanticRebuildManual {
		return "", kbErr("SEMANTIC_SYNC_NOT_AUTHORIZED", "semantic rebuild_policy=manual，AI 未获同步授权",
			"用户可手动运行 iknowledge semantic rebuild，或经 CLI 显式选择 ai-local/ai-remote")
	}
	health, err := e.SemanticHealthSnapshotContext(ctx)
	if err != nil {
		return "", err
	}
	if health.Status == SemanticHealthReady {
		// Serialize the final no-op decision with external rebuild/clear. Merely
		// checking health/identity and returning leaves a TOCTOU where `semantic
		// clear` can remove the file after the check while this MCP session consumes
		// its one attempt as a false success.
		releaseSemantic, lockErr := e.Store.AcquireSemanticLock()
		if lockErr != nil {
			return "", fmt.Errorf("semantic sync final validation: %w", lockErr)
		}
		current, settingsErr := LoadSemanticSettings(e.Store)
		if settingsErr != nil {
			releaseSemantic()
			return "", settingsErr
		}
		if current != cfg {
			releaseSemantic()
			return "", semanticAuthorizationChangedError()
		}
		// Metadata-only ready is intentionally cheap for status, but sync is the
		// user's explicit repair opportunity. Validate the local payload even when
		// this process previously loaded it: ensureSemanticSnapshot rechecks the
		// immutable on-disk identity under the cross-process mutation lock.
		if err := e.SyncContext(ctx); err != nil {
			releaseSemantic()
			return "", err
		}
		sourceFingerprint, _, err := e.semanticSourceMetadata(ctx)
		if err != nil {
			releaseSemantic()
			return "", err
		}
		if _, _, loadErr := e.ensureSemanticSnapshot(ctx, cfg, sourceFingerprint); loadErr != nil {
			if err := ctx.Err(); err != nil {
				releaseSemantic()
				return "", err
			}
			verified, healthErr := e.SemanticHealthSnapshotContext(ctx)
			releaseSemantic()
			if healthErr != nil {
				return "", healthErr
			}
			if verified.Status == SemanticHealthReady {
				// Resource/closing failures leave the same metadata-ready generation
				// intact and are not repaired by contacting a provider. Every other
				// local state is rebuildable: notably, an external clear may race
				// between the first health read and payload open, and must not consume
				// this session's one authorized sync attempt as a false failure.
				return "", loadErr
			}
			health = verified
		} else {
			health, err = e.SemanticHealthSnapshotContext(ctx)
			releaseSemantic()
			if err != nil {
				return "", err
			}
		}
	}
	if health.Status == SemanticHealthReady && health.PayloadLoaded {
		return "semantic 索引已经 ready，无需调用 provider。", nil
	}
	report, err := e.rebuildSemanticHeld(ctx, &cfg)
	if err != nil {
		return "", err
	}
	return report.Text(), nil
}

func (r SemanticRebuildReport) Text() string {
	return fmt.Sprintf("semantic 索引已重建: records=%d dimensions=%d vector=%.1fMiB metadata=%.1fKiB\nmodel=%s\nendpoint=%s\nfingerprint=%s",
		r.Records, r.Dimensions, float64(r.VectorBytes)/(1<<20), float64(r.MetadataBytes)/(1<<10),
		r.Model, r.Endpoint, r.Fingerprint)
}

func newSemanticEmbedder(cfg SemanticSettings) (semantic.Embedder, error) {
	apiKey := ""
	if !semanticEndpointLoopback(cfg.Endpoint) {
		apiKey = os.Getenv(SemanticAPIKeyEnv)
		if apiKey != "" {
			endpointOrigin, err := semanticEndpointOrigin(cfg.Endpoint)
			if err != nil {
				return nil, err
			}
			credentialOrigin, err := canonicalSemanticCredentialOrigin(os.Getenv(SemanticAPIOriginEnv))
			if err != nil {
				return nil, fmt.Errorf("semantic 远程凭据 audience 未授权: %w", err)
			}
			if credentialOrigin != endpointOrigin {
				return nil, fmt.Errorf("%s 与 semantic endpoint origin 不匹配，拒绝发送远程凭据", SemanticAPIOriginEnv)
			}
		}
	}
	return semantic.NewOpenAICompatible(semantic.OpenAIConfig{
		BaseURL: cfg.Endpoint, Model: cfg.Model, Revision: cfg.Revision,
		APIKey: apiKey, Dimensions: cfg.Dimensions, QueryProfile: cfg.QueryProfile,
		Timeout:      time.Duration(cfg.TimeoutSec) * time.Second,
		MaxBatchSize: semanticEmbedBatchSize, MaxResponseBytes: semanticProviderMaxResponse,
	})
}

func (e *Engine) semanticGate() chan struct{} {
	e.semantic.mu.Lock()
	defer e.semantic.mu.Unlock()
	if e.semantic.process != nil {
		return e.semantic.process.providerGate
	}
	if e.semantic.providerGate == nil {
		e.semantic.providerGate = make(chan struct{}, semanticProviderConcurrency)
	}
	return e.semantic.providerGate
}

func (e *Engine) semanticSearchGate() chan struct{} {
	e.semantic.mu.Lock()
	defer e.semantic.mu.Unlock()
	if e.semantic.process != nil {
		return e.semantic.process.searchGate
	}
	if e.semantic.searchGate == nil {
		e.semantic.searchGate = make(chan struct{}, semanticSearchConcurrency)
	}
	return e.semantic.searchGate
}

func (e *Engine) semanticRebuildGate() chan struct{} {
	e.semantic.mu.Lock()
	defer e.semantic.mu.Unlock()
	if e.semantic.process != nil {
		return e.semantic.process.rebuildGate
	}
	if e.semantic.rebuildGate == nil {
		e.semantic.rebuildGate = make(chan struct{}, 1)
	}
	return e.semantic.rebuildGate
}

func (e *Engine) semanticSourceGate() chan struct{} {
	e.semantic.mu.Lock()
	defer e.semantic.mu.Unlock()
	if e.semantic.process != nil {
		return e.semantic.process.sourceGate
	}
	if e.semantic.sourceGate == nil {
		e.semantic.sourceGate = make(chan struct{}, 1)
	}
	return e.semantic.sourceGate
}

func (e *Engine) acquireSemanticSourceBuild(ctx context.Context) (func(), error) {
	if ctx == nil {
		return nil, fmt.Errorf("semantic source build: nil context")
	}
	gate := e.semanticSourceGate()
	select {
	case gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-gate
			return nil, err
		}
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// acquireSemanticRebuild serializes full generations without an
// uninterruptible sync.Mutex wait. The cross-process semantic lock remains the
// publication authority; this gate only coalesces/waits within one Engine.
func (e *Engine) acquireSemanticRebuild(ctx context.Context) (func(), error) {
	if ctx == nil {
		return nil, fmt.Errorf("semantic rebuild: nil context")
	}
	gate := e.semanticRebuildGate()
	select {
	case gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-gate
			return nil, err
		}
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (e *Engine) embedSemanticDocumentsDualCanary(ctx context.Context, embedder semantic.Embedder, texts []string, authorize func() error) ([][]float32, []float32, []float32, error) {
	if len(texts) > semanticEmbedBatchSize-2 {
		return nil, nil, nil, fmt.Errorf("semantic dual-canary batch documents=%d，最大 %d", len(texts), semanticEmbedBatchSize-2)
	}
	gate := e.semanticGate()
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		return nil, nil, nil, ctx.Err()
	}
	// Authorization is checked after waiting for the provider slot and directly
	// before the request. Hold the shared config lock only across this batch:
	// configure/disable may succeed between batches, and the next batch then
	// aborts before RoundTrip instead of keeping authorization pinned for a long
	// full-generation rebuild.
	if authorize != nil {
		releaseAuthorization, err := e.Store.AcquireSemanticConfigReadLock()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("semantic provider authorization: %w", err)
		}
		defer releaseAuthorization()
		if err := authorize(); err != nil {
			return nil, nil, nil, err
		}
	}
	if dual, ok := embedder.(semantic.DualModeCanaryEmbedder); ok {
		return dual.EmbedDocumentsWithDualCanary(ctx, texts, semanticProbeText, semanticProbeText)
	}
	return nil, nil, nil, fmt.Errorf("semantic embedder 不支持原子 document/query 双模式 canary，无法执行同请求漂移检测，拒绝构建")
}

func embedSemanticQueryCanary(ctx context.Context, embedder semantic.Embedder, query string) ([]float32, []float32, error) {
	if canaryEmbedder, ok := embedder.(semantic.CanaryEmbedder); ok {
		return canaryEmbedder.EmbedQueryWithCanary(ctx, query, semanticProbeText)
	}
	return nil, nil, fmt.Errorf("semantic embedder 不支持同请求 query canary，无法执行常见漂移检测，拒绝查询")
}

// RebuildSemantic 显式生成完整不可变 generation。它只读取知识正本，
// embedding 与文件写入均在主 runtime 锁外；提交前重新核对源 fingerprint。
func (e *Engine) RebuildSemantic(ctx context.Context) (SemanticRebuildReport, error) {
	return e.rebuildSemantic(ctx, nil)
}

// rebuildSemantic optionally binds the whole generation to the exact settings
// that SyncSemantic already authorized. CLI rebuild passes nil and binds to the
// settings read after acquiring the same cross-process semantic lock.
func (e *Engine) rebuildSemantic(ctx context.Context, authorized *SemanticSettings) (SemanticRebuildReport, error) {
	if ctx == nil {
		return SemanticRebuildReport{}, fmt.Errorf("semantic rebuild: nil context")
	}
	if err := e.requireInit(); err != nil {
		return SemanticRebuildReport{}, err
	}
	releaseRebuild, err := e.acquireSemanticRebuild(ctx)
	if err != nil {
		return SemanticRebuildReport{}, err
	}
	defer releaseRebuild()
	return e.rebuildSemanticHeld(ctx, authorized)
}

// rebuildSemanticHeld performs one forced generation while the caller owns
// acquireSemanticRebuild. SyncSemantic rechecks ready before entering here;
// RebuildSemantic intentionally always enters so the CLI remains a force
// rebuild operation.
func (e *Engine) rebuildSemanticHeld(ctx context.Context, authorized *SemanticSettings) (SemanticRebuildReport, error) {
	if err := ctx.Err(); err != nil {
		return SemanticRebuildReport{}, err
	}
	releaseSemantic, err := e.Store.AcquireSemanticLock()
	if err != nil {
		return SemanticRebuildReport{}, fmt.Errorf("semantic rebuild: %w", err)
	}
	defer releaseSemantic()
	if err := e.Store.CleanupSemanticIndexTemps(); err != nil {
		return SemanticRebuildReport{}, fmt.Errorf("semantic rebuild: 清理崩溃残留: %w", err)
	}
	cfg, err := LoadSemanticSettings(e.Store)
	if err != nil {
		return SemanticRebuildReport{}, err
	}
	if authorized != nil && cfg != *authorized {
		return SemanticRebuildReport{}, semanticAuthorizationChangedError()
	}
	if !cfg.Enabled {
		return SemanticRebuildReport{}, fmt.Errorf("semantic 未启用；先运行 iknowledge semantic configure")
	}
	if err := e.SyncContext(ctx); err != nil {
		return SemanticRebuildReport{}, err
	}
	docs, sourceFingerprint, sourceLease, sourceErr := e.semanticSourceDocuments(ctx)
	if sourceErr != nil {
		return SemanticRebuildReport{}, sourceErr
	}
	defer func() {
		docs = nil
		sourceLease.ReleaseDocuments()
	}()
	if authorized != nil && len(docs) > semanticMCPSyncMaxRecords {
		return SemanticRebuildReport{}, kbErr("SEMANTIC_SYNC_TOO_LARGE",
			fmt.Sprintf("semantic source 有 %d 条卡片，超过 MCP 同步上限 %d", len(docs), semanticMCPSyncMaxRecords),
			"请由用户运行 iknowledge semantic rebuild --repo "+e.Store.RepoRoot())
	}
	// Reserve the daemon-wide worst-case matrix budget before constructing a
	// credential-bearing client or sending even the probe. beginSemanticBuild
	// waits for active Flat scans and counts old+new separately; at a full cap it
	// may drop only this repository's rebuildable resident cache. The old file
	// remains atomic and reloadable if this rebuild later fails.
	if err := e.beginSemanticBuildContext(ctx, cfg); err != nil {
		return SemanticRebuildReport{}, err
	}
	buildActive := true
	defer func() {
		if buildActive {
			e.abortSemanticBuild()
		}
	}()
	// Size authorization is evaluated before constructing a credential-bearing
	// remote client or sending the initial canary probe. Oversized interactive
	// work therefore fails with exactly zero provider requests.
	embedder, err := newSemanticEmbedder(cfg)
	if err != nil {
		return SemanticRebuildReport{}, err
	}
	authorizeRequest := func() error { return e.validateSemanticProviderSettings(cfg) }

	_, documentProbe, queryProbe, err := e.embedSemanticDocumentsDualCanary(ctx, embedder, nil, authorizeRequest)
	if err != nil {
		return SemanticRebuildReport{}, fmt.Errorf("semantic 模型探测失败: %w", err)
	}
	if err := validateSemanticVector(documentProbe, cfg.Dimensions); err != nil {
		return SemanticRebuildReport{}, fmt.Errorf("semantic 模型探测: %w", err)
	}
	dimensions := len(documentProbe)
	if err := validateSemanticVector(queryProbe, dimensions); err != nil {
		return SemanticRebuildReport{}, fmt.Errorf("semantic query 模型探测: %w", err)
	}
	probeFingerprint := semanticVectorFingerprint(documentProbe)
	queryProbeFingerprint := semanticVectorFingerprint(queryProbe)

	// 先对 dimensions、metadata、record count 和最终矩阵字节数做完整预检。
	// 除固定单条 probe 外，任何批量/付费文档请求都必须发生在预检之后。
	records := make([]vector.Record, len(docs))
	for i, doc := range docs {
		records[i] = vector.Record{
			ID: doc.RecordID, NodeID: doc.NodeID, Kind: doc.Kind, SourceHash: doc.SourceHash,
		}
	}
	plan, err := vector.Preflight(ctx, dimensions, records, semanticVectorLimits(cfg))
	if err != nil {
		return SemanticRebuildReport{}, err
	}
	builder, err := vector.NewBuilder(ctx, plan)
	if err != nil {
		return SemanticRebuildReport{}, err
	}
	// Builder 已复制紧凑 metadata，尽早释放预检输入。
	records = nil
	const documentBatchSize = semanticEmbedBatchSize - 2 // reserve document + query mode canaries
	for start := 0; start < len(docs); start += documentBatchSize {
		end := min(start+documentBatchSize, len(docs))
		texts := make([]string, end-start)
		for i := range texts {
			texts[i] = docs[start+i].Text
		}
		vectors, batchProbe, batchQueryProbe, err := e.embedSemanticDocumentsDualCanary(ctx, embedder, texts, authorizeRequest)
		if err != nil {
			return SemanticRebuildReport{}, fmt.Errorf("semantic embedding 批次 %d..%d: %w", start, end, err)
		}
		if len(vectors) != len(texts) {
			return SemanticRebuildReport{}, fmt.Errorf("semantic embedding 批次返回 %d 条，期望 %d", len(vectors), len(texts))
		}
		if err := validateSemanticVector(batchProbe, dimensions); err != nil {
			return SemanticRebuildReport{}, fmt.Errorf("semantic embedding 批次 %d..%d canary: %w", start, end, err)
		}
		if observed := semanticVectorFingerprint(batchProbe); observed != probeFingerprint {
			return SemanticRebuildReport{}, fmt.Errorf("semantic embedding 批次 %d..%d 检测到实际模型漂移，已放弃混合向量 generation", start, end)
		}
		if err := validateSemanticVector(batchQueryProbe, dimensions); err != nil {
			return SemanticRebuildReport{}, fmt.Errorf("semantic embedding 批次 %d..%d query canary: %w", start, end, err)
		}
		if observed := semanticVectorFingerprint(batchQueryProbe); observed != queryProbeFingerprint {
			return SemanticRebuildReport{}, fmt.Errorf("semantic embedding 批次 %d..%d 检测到查询模型漂移，已放弃混合向量 generation", start, end)
		}
		for i, values := range vectors {
			if err := validateSemanticVector(values, dimensions); err != nil {
				return SemanticRebuildReport{}, fmt.Errorf("semantic record %s: %w", docs[start+i].RecordID, err)
			}
		}
		if err := builder.Append(ctx, vectors); err != nil {
			return SemanticRebuildReport{}, fmt.Errorf("semantic embedding 批次 %d..%d 写入矩阵: %w", start, end, err)
		}
	}
	snapshot, err := builder.Finish(ctx)
	if err != nil {
		return SemanticRebuildReport{}, err
	}
	docs = nil
	sourceLease.ReleaseDocuments()
	// embedding 期间若知识发生变化，晚到任务不得发布旧 generation。
	if err := e.validateSemanticRebuildCurrent(ctx, cfg, sourceFingerprint); err != nil {
		return SemanticRebuildReport{}, err
	}
	status := snapshot.Status()
	generation, err := newSemanticGeneration()
	if err != nil {
		return SemanticRebuildReport{}, err
	}
	meta := semanticIndexMetadata{
		Schema: 1, Generation: generation, SettingsFingerprint: SemanticSettingsFingerprint(cfg),
		EmbedderFingerprint: embedder.Fingerprint(), ProbeFingerprint: probeFingerprint,
		QueryProbeFingerprint: queryProbeFingerprint,
		SourceFingerprint:     hex.EncodeToString(sourceFingerprint[:]),
		Dimensions:            dimensions, Records: status.Records, BuiltAt: time.Now().UTC().Format(time.RFC3339),
	}
	_, expectedMetadataChecksum, err := marshalSemanticIndexMetadata(meta)
	if err != nil {
		return SemanticRebuildReport{}, err
	}
	writeErr := e.Store.WriteSemanticIndexStreamChecked(func(w io.Writer) error {
		return encodeSemanticIndexContext(ctx, w, meta, snapshot)
	}, func() error {
		// 大文件编码/fsync 期间也可能变代；在 rename 旧索引之前再校验，
		// 失败会丢弃 temp 并完整保留上一代文件。
		return e.validateSemanticRebuildCurrent(ctx, cfg, sourceFingerprint)
	})
	if writeErr != nil {
		// fsyncDir runs after rename. Reconcile the actual commit point so an
		// error never falsely promises that the previous disk generation survived.
		checkCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if inspection, inspectErr := e.inspectSemanticIndexMetadata(checkCtx, cfg); inspectErr == nil &&
			inspection.identity.MetadataChecksum == expectedMetadataChecksum {
			return SemanticRebuildReport{}, fmt.Errorf("semantic 新 generation 已原子替换磁盘文件，但提交后耐久性确认失败；索引将按本地健康检查处理: %w", writeErr)
		}
		return SemanticRebuildReport{}, writeErr
	}
	if err := e.validateSemanticRebuildCurrent(ctx, cfg, sourceFingerprint); err != nil {
		return SemanticRebuildReport{}, fmt.Errorf("semantic 新 generation 已提交，但提交后知识/配置已变化；该 generation 会被标记 stale，不能报告为本次重建成功: %w", err)
	}
	f, err := e.Store.OpenKnowledgeFileRead(semanticIndexRel)
	if err != nil {
		return SemanticRebuildReport{}, err
	}
	fileIdentity, identityErr := readSemanticFileIdentity(ctx, f)
	closeErr := f.Close()
	if identityErr != nil || closeErr != nil {
		return SemanticRebuildReport{}, errors.Join(identityErr, closeErr)
	}
	if fileIdentity.MetadataChecksum != expectedMetadataChecksum {
		return SemanticRebuildReport{}, fmt.Errorf("semantic 索引刚写入即被另一重建代替；保留磁盘胜者并放弃本进程旧快照")
	}

	key := semanticLoadedKey(cfg, sourceFingerprint)
	if err := e.publishSemanticBuild(key, fileIdentity, snapshot, meta); err != nil {
		return SemanticRebuildReport{}, err
	}
	buildActive = false
	return SemanticRebuildReport{
		Records: status.Records, Dimensions: status.Dimensions,
		VectorBytes: status.VectorBytes, MetadataBytes: status.MetadataBytes,
		Model: cfg.Model, Endpoint: cfg.Endpoint, Fingerprint: meta.SettingsFingerprint,
	}, nil
}

func (e *Engine) beginSemanticBuild(cfg SemanticSettings) error {
	return e.beginSemanticBuildContext(context.Background(), cfg)
}

func (e *Engine) beginSemanticBuildContext(ctx context.Context, cfg SemanticSettings) error {
	if err := e.semantic.residentMu.LockContext(ctx); err != nil {
		return err
	}
	defer e.semantic.residentMu.Unlock()
	e.semantic.mu.Lock()
	defer e.semantic.mu.Unlock()
	if e.semantic.building {
		return fmt.Errorf("semantic rebuild 已在进行")
	}
	if e.semantic.closing {
		return fmt.Errorf("semantic daemon 正在关闭")
	}
	if e.semantic.process != nil {
		bytes := uint64(cfg.MaxVectorMiB) << 20
		if err := e.semantic.process.reserveTransient(e, bytes); err != nil {
			// At the 1GiB steady-state ceiling, keeping old+new may not fit.
			// No scan can retain the old pointer under residentMu, so release only
			// this repository's resident cache (the atomic disk generation stays)
			// and retry before contacting the provider. A failed rebuild then
			// degrades to a later bounded reload rather than exceeding the cap.
			if e.semantic.snapshot == nil {
				return err
			}
			e.semantic.loadedKey, e.semantic.loadErr = "", ""
			e.semantic.loadedAt, e.semantic.loadedFile = time.Time{}, semanticFileIdentity{}
			e.semantic.snapshot, e.semantic.metadata = nil, semanticIndexMetadata{}
			e.semantic.queryCache = nil
			e.semantic.process.releaseResident(e)
			if retryErr := e.semantic.process.reserveTransient(e, bytes); retryErr != nil {
				return retryErr
			}
		}
	}
	// The transient reservation counts the new builder separately from any
	// published snapshot. Unless the full-cap fallback above was required, keep
	// the old immutable generation usable through atomic publication. Failures
	// before rename leave its disk generation untouched; post-commit validation
	// errors are reported explicitly as committed-but-stale.
	e.semantic.building = true
	return nil
}

func (e *Engine) abortSemanticBuild() {
	e.semantic.residentMu.Lock()
	defer e.semantic.residentMu.Unlock()
	e.semantic.mu.Lock()
	process := e.semantic.process
	e.semantic.building = false
	e.semantic.mu.Unlock()
	process.releaseTransient(e)
}

func (e *Engine) publishSemanticBuild(key string, identity semanticFileIdentity, snapshot *vector.Snapshot, meta semanticIndexMetadata) error {
	e.semantic.residentMu.Lock()
	defer e.semantic.residentMu.Unlock()
	e.semantic.mu.Lock()
	defer e.semantic.mu.Unlock()
	if e.semantic.closing {
		return fmt.Errorf("semantic daemon 已关闭，拒绝发布重建结果")
	}
	if !e.semantic.building {
		return fmt.Errorf("semantic rebuild publication lost its resident reservation")
	}
	e.semantic.loadedKey, e.semantic.loadedAt, e.semantic.loadedFile = key, time.Now(), identity
	e.semantic.snapshot, e.semantic.metadata = snapshot, meta
	e.semantic.loadErr, e.semantic.lastError = "", ""
	e.semantic.corruptFile, e.semantic.corruptErr = semanticFileIdentity{}, ""
	// 同名 provider 的实际模型可能已重建/漂移，旧 query vector 不得跨代复用。
	e.semantic.queryCache = nil
	e.semantic.failureUntil = time.Time{}
	e.semantic.process.promoteTransient(e)
	e.semantic.building = false
	return nil
}

func semanticAuthorizationChangedError() *KBError {
	return kbErr("SEMANTIC_AUTHORIZATION_CHANGED",
		"semantic 配置在 MCP 授权后发生变化，已在 provider 请求前终止",
		"重新查看 kb_status；不要在本会话自动重试")
}

func (e *Engine) validateSemanticRebuildCurrent(ctx context.Context, cfg SemanticSettings, source [32]byte) error {
	if err := e.SyncContext(ctx); err != nil {
		return err
	}
	fingerprint, _, err := e.semanticSourceMetadata(ctx)
	if err != nil {
		return err
	}
	if fingerprint != source {
		return fmt.Errorf("semantic 重建期间知识摘要发生变化，已放弃旧结果；请重试")
	}
	currentCfg, err := LoadSemanticSettings(e.Store)
	if err != nil {
		return err
	}
	if currentCfg != cfg {
		return fmt.Errorf("semantic 重建期间 provider 配置发生变化，已放弃旧结果；请重试")
	}
	return nil
}

// validateSemanticProviderSettings binds an outbound request to the exact
// canonical-repository configuration that authorized it. Callers perform this
// check while holding Store.AcquireSemanticConfigReadLock; supported config
// writes take its exclusive counterpart, so enable/policy/endpoint/model
// changes cannot successfully interleave at the provider boundary.
func (e *Engine) validateSemanticProviderSettings(expected SemanticSettings) error {
	current, err := LoadSemanticSettings(e.Store)
	if err != nil {
		return err
	}
	if current != expected {
		return fmt.Errorf("semantic 配置已变化，已在 provider 请求前终止")
	}
	if !current.Enabled {
		return fmt.Errorf("semantic 已禁用，已在 provider 请求前终止")
	}
	return nil
}

func semanticVectorLimits(cfg SemanticSettings) vector.Limits {
	limits := vector.DefaultLimits()
	limits.MaxVectorBytes = uint64(cfg.MaxVectorMiB) << 20
	return limits
}

func validateSemanticVector(values []float32, expected int) error {
	if len(values) == 0 {
		return fmt.Errorf("向量为空")
	}
	if expected > 0 && len(values) != expected {
		return fmt.Errorf("维度=%d，期望 %d", len(values), expected)
	}
	if len(values) > int(vector.DefaultLimits().MaxDimensions) {
		return fmt.Errorf("维度=%d 超过上限", len(values))
	}
	var norm float64
	for i, value := range values {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return fmt.Errorf("第 %d 维不是有限数", i)
		}
		norm += float64(value) * float64(value)
	}
	if norm == 0 {
		return fmt.Errorf("零向量")
	}
	return nil
}

func semanticVectorFingerprint(values []float32) string {
	var normSquared float64
	for _, value := range values {
		component := float64(value)
		if math.IsNaN(component) || math.IsInf(component, 0) {
			return ""
		}
		normSquared += component * component
	}
	if normSquared == 0 || math.IsNaN(normSquared) || math.IsInf(normSquared, 0) {
		return ""
	}
	norm := math.Sqrt(normSquared)
	h := sha256.New()
	_, _ = h.Write([]byte("iknowledge-semantic-normalized-canary-v2\x00"))
	var dimensions [8]byte
	binary.LittleEndian.PutUint64(dimensions[:], uint64(len(values)))
	_, _ = h.Write(dimensions[:])
	var encoded [4]byte
	for _, value := range values {
		// Match vector.Builder/Search: normalize in float64, then store as
		// float32. Quantization is now bounded to [-10000,10000], so large
		// provider magnitudes cannot overflow int32 or alias another direction;
		// positive rescaling of the same direction remains stable.
		normalized := float32(float64(value) / norm)
		quantized := int32(math.Round(float64(normalized) * 10_000))
		binary.LittleEndian.PutUint32(encoded[:], uint32(quantized))
		_, _ = h.Write(encoded[:])
	}
	return hex.EncodeToString(h.Sum(nil)[:12])
}

func semanticLoadedKey(cfg SemanticSettings, source [32]byte) string {
	// max_vector_mib 是 runtime resident policy，不改变向量空间/持久 metadata，
	// 但必须进入 load key；用户收紧上限后 live serve 不能继续持有旧大快照。
	return SemanticSettingsFingerprint(cfg) + ":" + hex.EncodeToString(source[:]) + fmt.Sprintf(":max=%d", cfg.MaxVectorMiB)
}

func (e *Engine) ensureSemanticSnapshot(ctx context.Context, cfg SemanticSettings, source [32]byte) (*vector.Snapshot, semanticIndexMetadata, error) {
	if err := e.semantic.residentMu.LockContext(ctx); err != nil {
		return nil, semanticIndexMetadata{}, err
	}
	defer e.semantic.residentMu.Unlock()
	return e.ensureSemanticSnapshotLocked(ctx, cfg, source)
}

// ensureSemanticSnapshotLocked requires residentMu. It may decode
// or replace the matrix and therefore must never run concurrently with a
// semanticSnapshotLease.
func (e *Engine) ensureSemanticSnapshotLocked(ctx context.Context, cfg SemanticSettings, source [32]byte) (*vector.Snapshot, semanticIndexMetadata, error) {
	if ctx == nil {
		return nil, semanticIndexMetadata{}, fmt.Errorf("semantic snapshot: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, semanticIndexMetadata{}, err
	}
	key := semanticLoadedKey(cfg, source)
	now := time.Now()
	e.semantic.mu.Lock()
	defer e.semantic.mu.Unlock()
	if e.semantic.closing {
		return nil, semanticIndexMetadata{}, fmt.Errorf("semantic daemon 正在关闭")
	}
	process := e.semantic.process
	fileChanged := false
	if e.semantic.loadedKey == key {
		if e.semantic.snapshot != nil {
			// `semantic rebuild/clear` 常由另一个 CLI 进程执行；用 wrapper metadata
			// checksum + vector checksum 比较真实文件 generation，不能只看“存在”。
			f, openErr := e.Store.OpenKnowledgeFileRead(semanticIndexRel)
			if openErr == nil {
				identity, identityErr := readSemanticFileIdentity(ctx, f)
				closeErr := f.Close()
				openErr = errors.Join(identityErr, closeErr)
				if openErr == nil && identity == e.semantic.loadedFile {
					return e.semantic.snapshot, e.semantic.metadata, nil
				}
			}
			if openErr != nil && os.IsNotExist(openErr) {
				openErr = fmt.Errorf("semantic 索引不存在；运行 iknowledge semantic rebuild")
			}
			// 某个调用者取消只结束它自己的请求，不能破坏其他请求仍可使用的
			// 共享 immutable snapshot，也不能把取消写成全局 loadErr。
			if errors.Is(openErr, context.Canceled) || errors.Is(openErr, context.DeadlineExceeded) {
				return nil, semanticIndexMetadata{}, openErr
			}
			// identity 变化时继续走完整有界 decode；打开/identity 失败则按
			// 普通 load error 降级，绝不继续使用内存旧代。
			e.semantic.snapshot, e.semantic.metadata = nil, semanticIndexMetadata{}
			e.semantic.loadedFile = semanticFileIdentity{}
			if openErr != nil {
				e.semantic.loadErr, e.semantic.lastError = openErr.Error(), openErr.Error()
				process.releaseResident(e)
				return nil, semanticIndexMetadata{}, openErr
			}
			fileChanged = true
		}
		// 外部 `semantic rebuild` 可能刚以原子 rename 写好同一 key 的文件。
		// 短暂负缓存防止每次 recall 都解码，随后自动重试，无需重启 serve。
		if !fileChanged && now.Sub(e.semantic.loadedAt) < semanticFailureBackoff {
			if e.semantic.loadErr == "" {
				return nil, semanticIndexMetadata{}, fmt.Errorf("semantic 索引暂不可用")
			}
			return nil, semanticIndexMetadata{}, errors.New(e.semantic.loadErr)
		}
	}
	if e.semantic.building {
		return nil, semanticIndexMetadata{}, fmt.Errorf("semantic 索引正在重建；本次已降级 lexical，请稍后重试")
	}
	if process != nil {
		if err := process.reserveResident(e, uint64(cfg.MaxVectorMiB)<<20); err != nil {
			e.semantic.loadedKey, e.semantic.loadErr = "", ""
			e.semantic.loadedAt, e.semantic.loadedFile = time.Time{}, semanticFileIdentity{}
			e.semantic.snapshot, e.semantic.metadata = nil, semanticIndexMetadata{}
			e.semantic.queryCache = nil
			process.releaseResident(e)
			e.semantic.lastError = err.Error()
			return nil, semanticIndexMetadata{}, err
		}
	}
	// residentMu excludes every active scan. Drop a different/externally
	// replaced generation before allocating the new matrix, so logical resident
	// memory never contains two snapshots for one Engine.
	e.semantic.snapshot, e.semantic.metadata = nil, semanticIndexMetadata{}
	e.semantic.loadedFile = semanticFileIdentity{}

	snapshot, meta, identity, err := e.loadSemanticIndex(ctx, cfg, source)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		process.releaseResident(e)
		return nil, semanticIndexMetadata{}, err
	}
	e.semantic.loadedKey, e.semantic.loadedAt = key, now
	e.semantic.loadedFile = identity
	e.semantic.snapshot, e.semantic.metadata = snapshot, meta
	if err != nil {
		e.semantic.snapshot = nil
		e.semantic.loadedFile = semanticFileIdentity{}
		e.semantic.loadErr, e.semantic.lastError = err.Error(), err.Error()
		if identity != (semanticFileIdentity{}) {
			e.semantic.corruptFile, e.semantic.corruptErr = identity, err.Error()
		}
		process.releaseResident(e)
		return nil, semanticIndexMetadata{}, err
	}
	e.semantic.loadErr = ""
	e.semantic.corruptFile, e.semantic.corruptErr = semanticFileIdentity{}, ""
	// 磁盘 generation 可能由外部 CLI 针对同名、已漂移模型重建；旧 probe
	// 与 query cache 都属于上一代，下一次 recall 必须重新探测/嵌入。
	e.semantic.queryCache = nil
	return snapshot, meta, nil
}

type semanticSnapshotLease struct {
	engine   *Engine
	snapshot *vector.Snapshot
	meta     semanticIndexMetadata
	identity semanticFileIdentity
	released bool
}

// acquireSemanticSnapshotLease keeps residentMu until release.
// The provider call happens before this short lease; only identity checks,
// Flat search and current-source filtering are inside it. Exclusive ownership
// closes the otherwise unavoidable RWMutex downgrade gap in which another
// goroutine could decode a new matrix while this caller still held the old
// pointer. Same-repository Flat scans are therefore serialized; daemon-wide
// searchGate still permits two scans when they belong to different engines.
func (e *Engine) acquireSemanticSnapshotLease(ctx context.Context, cfg SemanticSettings, source [32]byte) (*semanticSnapshotLease, error) {
	if err := e.semantic.residentMu.LockContext(ctx); err != nil {
		return nil, err
	}
	snapshot, meta, err := e.ensureSemanticSnapshotLocked(ctx, cfg, source)
	if err != nil {
		e.semantic.residentMu.Unlock()
		return nil, err
	}
	e.semantic.mu.Lock()
	identity := e.semantic.loadedFile
	valid := !e.semantic.building && snapshot != nil && snapshot == e.semantic.snapshot && meta == e.semantic.metadata
	e.semantic.mu.Unlock()
	if !valid {
		e.semantic.residentMu.Unlock()
		return nil, fmt.Errorf("semantic resident generation changed before lease")
	}
	return &semanticSnapshotLease{engine: e, snapshot: snapshot, meta: meta, identity: identity}, nil
}

func (l *semanticSnapshotLease) release() {
	if l == nil || l.released {
		return
	}
	l.released = true
	l.engine.semantic.residentMu.Unlock()
}

func (l *semanticSnapshotLease) validateFile(ctx context.Context) error {
	if l == nil || l.engine == nil {
		return fmt.Errorf("semantic snapshot lease missing")
	}
	f, err := l.engine.Store.OpenKnowledgeFileRead(semanticIndexRel)
	if err != nil {
		return err
	}
	identity, identityErr := readSemanticFileIdentity(ctx, f)
	closeErr := f.Close()
	if err := errors.Join(identityErr, closeErr); err != nil {
		return err
	}
	if identity != l.identity {
		return fmt.Errorf("semantic 索引在查询期间被替换，已丢弃旧候选；下次查询使用新 generation")
	}
	return nil
}

func (e *Engine) loadSemanticIndex(ctx context.Context, cfg SemanticSettings, source [32]byte) (*vector.Snapshot, semanticIndexMetadata, semanticFileIdentity, error) {
	f, err := e.Store.OpenKnowledgeFileRead(semanticIndexRel)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, semanticIndexMetadata{}, semanticFileIdentity{}, fmt.Errorf("semantic 索引不存在；运行 iknowledge semantic rebuild")
		}
		return nil, semanticIndexMetadata{}, semanticFileIdentity{}, err
	}
	defer f.Close()
	identity, err := readSemanticFileIdentity(ctx, f)
	if err != nil {
		return nil, semanticIndexMetadata{}, semanticFileIdentity{}, err
	}
	maxFile := semanticMaxIndexFileSize(cfg)
	if identity.Size < semanticWrapperSize || identity.Size > maxFile {
		return nil, semanticIndexMetadata{}, semanticFileIdentity{}, fmt.Errorf("semantic 索引大小 %d 超出允许范围", identity.Size)
	}
	// Wrapper metadata 已足以判断 settings/embedder/source stale；必须先绑定
	// 校验，再为仍有效的 generation 分配/扫描最高 512MiB vector payload。
	meta, err := decodeSemanticIndexMetadataContext(ctx, f)
	if err != nil {
		return nil, semanticIndexMetadata{}, identity, err
	}
	// Source drift does not make unchanged records unsafe: every hit is later
	// rebound to the current manifest by record ID + source hash. Provider/model
	// drift, however, changes the vector space and remains a hard rejection.
	if err := validateSemanticIndexProviderBinding(meta, cfg); err != nil {
		return nil, semanticIndexMetadata{}, identity, err
	}
	snapshot, err := vector.DecodeWithLimitsContext(ctx, f, semanticVectorLimits(cfg))
	if err != nil {
		return nil, semanticIndexMetadata{}, identity, err
	}
	switch {
	case meta.Dimensions != snapshot.Status().Dimensions || meta.Records != snapshot.Status().Records:
		return nil, semanticIndexMetadata{}, identity, fmt.Errorf("semantic 索引 metadata 与 payload 不一致")
	}
	return snapshot, meta, identity, nil
}

func validateSemanticIndexBinding(meta semanticIndexMetadata, cfg SemanticSettings, source [32]byte) error {
	if err := validateSemanticIndexProviderBinding(meta, cfg); err != nil {
		return err
	}
	if meta.SourceFingerprint != hex.EncodeToString(source[:]) {
		return fmt.Errorf("知识摘要已变化，semantic 索引 stale；请 rebuild")
	}
	return nil
}

func validateSemanticIndexProviderBinding(meta semanticIndexMetadata, cfg SemanticSettings) error {
	embedder, err := newSemanticEmbedder(cfg)
	if err != nil {
		return err
	}
	switch {
	case meta.SettingsFingerprint != SemanticSettingsFingerprint(cfg):
		return fmt.Errorf("semantic provider 配置已变化，索引 stale；请 rebuild")
	case meta.EmbedderFingerprint != embedder.Fingerprint():
		return fmt.Errorf("semantic embedder 已变化，索引 stale；请 rebuild")
	case meta.QueryProbeFingerprint == "":
		// Round 37 indexes predate query-mode canaries. Their wrapper remains
		// structurally readable so upgrades can classify them as stale/rebuildable
		// rather than corrupt, but they are never queryable by the new runtime.
		return fmt.Errorf("semantic 索引缺少 query canary，属于旧代；请 rebuild")
	case cfg.Dimensions > 0 && meta.Dimensions != cfg.Dimensions:
		return fmt.Errorf("semantic 索引维度=%d，配置要求 %d", meta.Dimensions, cfg.Dimensions)
	}
	return nil
}

func semanticMaxIndexFileSize(cfg SemanticSettings) int64 {
	limits := vector.DefaultLimits()
	return (int64(cfg.MaxVectorMiB) << 20) + int64(limits.MaxMetadataBytes) +
		semanticMaxMetadata + semanticWrapperSize + int64(vector.EncodedFixedOverhead)
}

type semanticCandidateSet struct {
	current []vector.Hit
	risk    []vector.Hit
	history []vector.Hit
}

func (s semanticCandidateSet) len() int { return len(s.current) + len(s.risk) + len(s.history) }

// semanticCandidates returns three evidence lanes from one Flat scan. Any
// provider/cache error remains advisory and must not break lexical/structural
// retrieval. Source-stale snapshots are allowed in partial mode; record ID +
// source hash binding below makes unchanged cards safe and drops every changed,
// deleted, or newly missing card.
func (e *Engine) semanticCandidates(ctx context.Context, query string) (semanticCandidateSet, string) {
	cfg, err := LoadSemanticSettings(e.Store)
	if err != nil {
		// A malformed/unreadable private configuration is no longer an
		// authorization to retain a resident semantic generation. Keep the
		// rebuildable file on disk, but release the matrix and cached queries.
		_ = e.evictSemanticResidentStateContext(ctx, semanticIndexMetadata{})
		_ = e.evictSemanticSourceStateContext(ctx)
		return semanticCandidateSet{}, err.Error()
	}
	if !cfg.Enabled {
		// Missing configuration is folded into the same safe disabled value by
		// LoadSemanticSettings. In both cases disabled means no resident/query
		// semantic state, not merely "skip the next provider request".
		_ = e.evictSemanticResidentStateContext(ctx, semanticIndexMetadata{})
		_ = e.evictSemanticSourceStateContext(ctx)
		return semanticCandidateSet{}, ""
	}
	query, _ = RedactText(strings.TrimSpace(query))
	if query == "" {
		return semanticCandidateSet{}, ""
	}
	sourceFingerprint, _, sourceErr := e.semanticSourceMetadata(ctx)
	if sourceErr != nil {
		return semanticCandidateSet{}, sourceErr.Error()
	}
	_, initialMeta, err := e.ensureSemanticSnapshot(ctx, cfg, sourceFingerprint)
	if err != nil {
		if strings.Contains(err.Error(), "semantic 索引不存在") {
			_ = e.evictSemanticSourceStateContext(ctx)
		}
		return semanticCandidateSet{}, err.Error()
	}
	partial := initialMeta.SourceFingerprint != hex.EncodeToString(sourceFingerprint[:])
	embedder, err := newSemanticEmbedder(cfg)
	if err != nil {
		return semanticCandidateSet{}, err.Error()
	}
	queryVector, queryProbeFingerprint, err := e.cachedSemanticQueryForSettings(ctx, embedder, query, cfg)
	if err != nil {
		e.setSemanticError(err)
		return semanticCandidateSet{}, err.Error()
	}
	releaseAuthorization, err := e.Store.AcquireSemanticConfigReadLock()
	if err != nil {
		return semanticCandidateSet{}, fmt.Errorf("semantic query final authorization: %w", err).Error()
	}
	defer releaseAuthorization()
	if err := e.validateSemanticProviderSettings(cfg); err != nil {
		// The provider request (or a cache hit) raced a successful config
		// change. Do not leave the generation authorized by the old settings
		// resident after observing that revocation.
		_ = e.evictSemanticResidentStateContext(ctx, semanticIndexMetadata{})
		return semanticCandidateSet{}, err.Error()
	}
	lease, err := e.acquireSemanticSnapshotLease(ctx, cfg, sourceFingerprint)
	if err != nil {
		return semanticCandidateSet{}, err.Error()
	}
	defer lease.release()
	meta := lease.meta
	if meta.Generation != initialMeta.Generation {
		return semanticCandidateSet{}, "semantic 索引在查询期间被替换，已丢弃旧候选；下次查询使用新 generation"
	}
	partial = meta.SourceFingerprint != hex.EncodeToString(sourceFingerprint[:])
	if queryProbeFingerprint != meta.QueryProbeFingerprint {
		err := fmt.Errorf("embedding 服务实际模型已变化，semantic 索引 stale；请 rebuild")
		e.setSemanticError(err)
		return semanticCandidateSet{}, err.Error()
	}
	if len(queryVector) != meta.Dimensions {
		err := fmt.Errorf("semantic 查询维度=%d，索引维度=%d", len(queryVector), meta.Dimensions)
		e.setSemanticError(err)
		return semanticCandidateSet{}, err.Error()
	}
	if err := lease.validateFile(ctx); err != nil {
		return semanticCandidateSet{}, err.Error()
	}
	// Lock order for combined operations is vector resident lease -> rt.mu ->
	// source generation lease. Clear/shutdown use the same order. Sync after the
	// provider first, then pin only the manifest used by the Flat filter.
	if err := e.SyncContext(ctx); err != nil {
		return semanticCandidateSet{}, err.Error()
	}
	_, scanManifest, scanSourceLease, sourceErr := e.semanticSourceSnapshotLease(ctx, false)
	if sourceErr != nil {
		return semanticCandidateSet{}, sourceErr.Error()
	}
	scanFingerprint := scanManifest.fingerprint
	partial = meta.SourceFingerprint != hex.EncodeToString(scanFingerprint[:])
	byLane, err := e.searchSemanticSnapshot(ctx, lease.snapshot, queryVector, cfg.TopK, scanManifest)
	scanManifest = semanticSourceManifest{}
	scanSourceLease.Release()
	if err != nil {
		e.setSemanticError(err)
		return semanticCandidateSet{}, err.Error()
	}
	if err := lease.validateFile(ctx); err != nil {
		return semanticCandidateSet{}, err.Error()
	}

	// Flat scan 后再次 Sync，捕捉扫描期间的进程内写入以及外部
	// checkout/编辑；退掉上一代 source lease 后再 Sync，避免锁序反转。
	if err := e.SyncContext(ctx); err != nil {
		return semanticCandidateSet{}, err.Error()
	}
	_, currentManifest, currentSourceLease, sourceErr := e.semanticSourceSnapshotLease(ctx, false)
	if sourceErr != nil {
		return semanticCandidateSet{}, sourceErr.Error()
	}
	defer func() {
		currentManifest = semanticSourceManifest{}
		currentSourceLease.Release()
	}()
	partial = meta.SourceFingerprint != hex.EncodeToString(currentManifest.fingerprint[:])
	if currentManifest.fingerprint != scanFingerprint {
		partial = true
		// Source changes may invalidate a former node winner or an entire stale
		// Top-K prefix. Re-run the same immutable matrix locally with the latest
		// manifest filtering *before* node competition so unchanged lower-ranked
		// cards backfill correctly without another provider request.
		byLane, err = e.searchSemanticSnapshot(ctx, lease.snapshot, queryVector, cfg.TopK, currentManifest)
		if err != nil {
			e.setSemanticError(err)
			return semanticCandidateSet{}, err.Error()
		}
	}
	if err := lease.validateFile(ctx); err != nil {
		return semanticCandidateSet{}, err.Error()
	}
	filter := func(hits []vector.Hit, lane string) []vector.Hit {
		filtered := make([]vector.Hit, 0, len(hits))
		for _, hit := range hits {
			record, ok := currentManifest.records[hit.ID]
			if float64(hit.Score) < cfg.MinScore || !ok || record.NodeID != hit.NodeID ||
				record.Kind != lane || hit.Kind != lane || record.SourceHash != hit.SourceHash {
				continue
			}
			filtered = append(filtered, hit)
		}
		return filtered
	}
	result := semanticCandidateSet{
		current: filter(byLane[semanticLaneCurrent], semanticLaneCurrent),
		risk:    filter(byLane[semanticLaneRisk], semanticLaneRisk),
		history: filter(byLane[semanticLaneHistory], semanticLaneHistory),
	}
	e.semantic.mu.Lock()
	e.semantic.lastError = ""
	e.semantic.mu.Unlock()
	if partial {
		return result, "semantic 索引为 partial：仅使用与当前 source hash 一致的旧卡片；新改知识等待 sync/rebuild"
	}
	return result, ""
}

func (e *Engine) searchSemanticSnapshot(ctx context.Context, snapshot *vector.Snapshot, query []float32, limit int, manifest semanticSourceManifest) (map[string][]vector.Hit, error) {
	gate := e.semanticSearchGate()
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return snapshot.SearchDistinctNodesByKindsFiltered(ctx, query, limit,
		[]string{semanticLaneCurrent, semanticLaneRisk, semanticLaneHistory},
		func(record vector.RecordMetadata) bool {
			current, ok := manifest.records[record.ID]
			return ok && current.NodeID == record.NodeID && current.Kind == record.Kind &&
				current.SourceHash == record.SourceHash
		})
}

func (e *Engine) cachedSemanticQuery(ctx context.Context, embedder semantic.Embedder, query string) ([]float32, string, error) {
	return e.cachedSemanticQueryBound(ctx, embedder, query, nil)
}

func (e *Engine) cachedSemanticQueryForSettings(ctx context.Context, embedder semantic.Embedder, query string, cfg SemanticSettings) ([]float32, string, error) {
	return e.cachedSemanticQueryBound(ctx, embedder, query, &cfg)
}

func (e *Engine) cachedSemanticQueryBound(ctx context.Context, embedder semantic.Embedder, query string, expected *SemanticSettings) ([]float32, string, error) {
	key := embedder.Fingerprint() + "\x00" + query
	now := time.Now()
	e.semantic.mu.Lock()
	if entry, ok := e.semantic.queryCache[key]; ok && now.Before(entry.expiresAt) {
		vectorCopy := append([]float32(nil), entry.vector...)
		e.semantic.mu.Unlock()
		return vectorCopy, entry.canaryFingerprint, nil
	}
	e.semantic.mu.Unlock()
	gate := e.semanticGate()
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
	// 与 probe 同理：拿到有界 provider slot 后再看一次 cache。相同 query
	// 的排队请求复用先完成者结果，不会串行放大费用。
	now = time.Now()
	e.semantic.mu.Lock()
	if now.Before(e.semantic.failureUntil) && e.semantic.lastError != "" {
		err := errors.New(e.semantic.lastError)
		e.semantic.mu.Unlock()
		return nil, "", err
	}
	if entry, ok := e.semantic.queryCache[key]; ok && now.Before(entry.expiresAt) {
		vectorCopy := append([]float32(nil), entry.vector...)
		e.semantic.mu.Unlock()
		return vectorCopy, entry.canaryFingerprint, nil
	}
	e.semantic.mu.Unlock()
	var releaseSemantic func()
	if expected != nil {
		if e.Store == nil {
			return nil, "", fmt.Errorf("semantic query: missing store for provider authorization")
		}
		var lockErr error
		releaseSemantic, lockErr = e.Store.AcquireSemanticConfigReadLock()
		if lockErr != nil {
			return nil, "", fmt.Errorf("semantic query provider authorization: %w", lockErr)
		}
		defer releaseSemantic()
		if err := e.validateSemanticProviderSettings(*expected); err != nil {
			return nil, "", err
		}
	}
	values, canary, err := embedSemanticQueryCanary(ctx, embedder, query)
	if err != nil {
		e.setSemanticError(err)
		return nil, "", err
	}
	if err := validateSemanticVector(values, 0); err != nil {
		e.setSemanticError(err)
		return nil, "", err
	}
	if err := validateSemanticVector(canary, len(values)); err != nil {
		e.setSemanticError(err)
		return nil, "", err
	}
	canaryFingerprint := semanticVectorFingerprint(canary)
	e.semantic.mu.Lock()
	if e.semantic.queryCache == nil {
		e.semantic.queryCache = make(map[string]semanticQueryCacheEntry)
	}
	if len(e.semantic.queryCache) >= semanticQueryCacheCap {
		keys := make([]string, 0, len(e.semantic.queryCache))
		for existing, entry := range e.semantic.queryCache {
			if now.After(entry.expiresAt) {
				delete(e.semantic.queryCache, existing)
			} else {
				keys = append(keys, existing)
			}
		}
		if len(e.semantic.queryCache) >= semanticQueryCacheCap && len(keys) > 0 {
			sort.Strings(keys)
			delete(e.semantic.queryCache, keys[0]) // 确定性有界退化，无后台 goroutine。
		}
	}
	e.semantic.queryCache[key] = semanticQueryCacheEntry{
		vector: append([]float32(nil), values...), canaryFingerprint: canaryFingerprint,
		expiresAt: now.Add(semanticQueryCacheTTL),
	}
	e.semantic.mu.Unlock()
	return values, canaryFingerprint, nil
}

func (e *Engine) setSemanticError(err error) {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	e.semantic.mu.Lock()
	e.semantic.lastError = err.Error()
	e.semantic.failureUntil = time.Now().Add(semanticFailureBackoff)
	e.semantic.mu.Unlock()
}

// ClearSemanticIndex 删除可重建的本机派生缓存，不改变 provider 配置或 enabled。
func (e *Engine) ClearSemanticIndex() error {
	releaseSemantic, err := e.Store.AcquireSemanticLock()
	if err != nil {
		return fmt.Errorf("semantic clear: %w", err)
	}
	defer releaseSemantic()
	if err := e.Store.CleanupSemanticIndexTemps(); err != nil {
		return fmt.Errorf("semantic clear: 清理崩溃残留: %w", err)
	}
	if err := e.Store.RemoveKnowledgeFile(semanticIndexRel); err != nil && !os.IsNotExist(err) {
		return err
	}
	e.semantic.residentMu.Lock()
	defer e.semantic.residentMu.Unlock()
	e.semantic.mu.Lock()
	process := e.semantic.process
	e.semantic.loadedKey, e.semantic.loadErr = "", ""
	e.semantic.loadedAt, e.semantic.loadedFile = time.Time{}, semanticFileIdentity{}
	e.semantic.snapshot, e.semantic.metadata = nil, semanticIndexMetadata{}
	e.semantic.corruptFile, e.semantic.corruptErr = semanticFileIdentity{}, ""
	e.semantic.queryCache = nil
	e.semantic.failureUntil, e.semantic.lastError = time.Time{}, ""
	e.semantic.mu.Unlock()
	process.releaseResident(e)
	e.evictSemanticSourceState()
	return nil
}

// SemanticHealthStatus 是 MCP/CLI 可依赖的稳定本地状态集。不得把
// provider 暂时不可达塞进这些状态，因为健康检查刻意不联网。
type SemanticHealthStatus string

const (
	SemanticHealthDisabled          SemanticHealthStatus = "disabled"
	SemanticHealthUnconfigured      SemanticHealthStatus = "unconfigured"
	SemanticHealthConfiguredNoIndex SemanticHealthStatus = "configured-no-index"
	SemanticHealthReady             SemanticHealthStatus = "ready"
	SemanticHealthPartial           SemanticHealthStatus = "partial"
	SemanticHealthStaleSource       SemanticHealthStatus = "stale-source"
	SemanticHealthStaleProvider     SemanticHealthStatus = "stale-provider"
	SemanticHealthCorrupt           SemanticHealthStatus = "corrupt"
)

// SemanticHealth 是 semantic 派生索引的结构化健康快照。Provider 当前固定
// 为 unchecked：status 绝不联网、不读 API key，也不解码最高 512MiB
// 的 vector payload。ready 只代表本地 wrapper/metadata/绑定有效；payload
// 的 checksum 与解码仍延迟到 recall。
type SemanticHealth struct {
	Status     SemanticHealthStatus
	Enabled    bool
	Configured bool
	Model      string
	Profile    string
	Policy     string
	// ConfiguredDimensions preserves the user's 0=auto choice. IndexDimensions
	// is populated only when a local generation exists; conflating the two made
	// status falsely claim that an auto configuration was pinned to the observed
	// provider dimension.
	ConfiguredDimensions int
	IndexDimensions      int
	Records              int
	BuiltAt              string
	Provider             string
	NextAction           string
	Detail               string
	LastError            string
	PayloadLoaded        bool
}

func semanticHealthValue(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func (h SemanticHealth) compactText() string {
	return fmt.Sprintf("semantic: %s | provider: %s | next_action: %s\nsemantic 配置: model=%s | profile=%s | policy=%s | dimensions=%d(auto=0) | index_dimensions=%d | records=%d | built_at=%s",
		h.Status, semanticHealthValue(h.Provider), semanticHealthValue(h.NextAction),
		semanticHealthValue(h.Model), semanticHealthValue(h.Profile), semanticHealthValue(h.Policy),
		h.ConfiguredDimensions, h.IndexDimensions, h.Records, semanticHealthValue(h.BuiltAt))
}

func (e *Engine) semanticRebuildNextAction(policy string, sourceRecords int) string {
	switch policy {
	case "ai-local", "ai-remote":
		if sourceRecords > semanticMCPSyncMaxRecords {
			return "iknowledge semantic rebuild --repo " + e.Store.RepoRoot()
		}
		return "kb_semantic action=sync"
	default:
		return "iknowledge semantic rebuild --repo " + e.Store.RepoRoot()
	}
}

func semanticSyncLimitDetail(sourceRecords int) string {
	if sourceRecords <= semanticMCPSyncMaxRecords {
		return ""
	}
	return fmt.Sprintf("source 有 %d 条卡片，超过 MCP 同步上限 %d；为避免消耗本会话唯一 sync 尝试，请由用户运行 CLI rebuild",
		sourceRecords, semanticMCPSyncMaxRecords)
}

// semanticOfflineEmbedderFingerprint 只根据静态配置计算向量空间指纹。
// APIKey 故意为空；构造客户端不会发送请求。
func semanticOfflineEmbedderFingerprint(cfg SemanticSettings) (string, error) {
	embedder, err := semantic.NewOpenAICompatible(semantic.OpenAIConfig{
		BaseURL: cfg.Endpoint, Model: cfg.Model, Revision: cfg.Revision,
		QueryProfile: cfg.QueryProfile, Dimensions: cfg.Dimensions,
	})
	if err != nil {
		return "", err
	}
	return embedder.Fingerprint(), nil
}

// SemanticHealthSnapshot 返回纯本地健康快照。除仓外 config 与 vector.idx
// wrapper/<=64KiB metadata 外，ai-* policy 还会构造有界 source manifest，
// 用来避免建议一个注定超限的 MCP sync；绝不读 API key、探测 provider 或
// 解码 vector payload。
func (e *Engine) SemanticHealthSnapshot() (health SemanticHealth) {
	return e.semanticHealthSnapshotContext(context.Background())
}

func (e *Engine) SemanticHealthSnapshotContext(ctx context.Context) (SemanticHealth, error) {
	if ctx == nil {
		return SemanticHealth{}, fmt.Errorf("semantic health: nil context")
	}
	if err := ctx.Err(); err != nil {
		return SemanticHealth{}, err
	}
	health := e.semanticHealthSnapshotContext(ctx)
	if err := ctx.Err(); err != nil {
		return health, err
	}
	return health, nil
}

func (e *Engine) semanticHealthSnapshotContext(ctx context.Context) (health SemanticHealth) {
	health = SemanticHealth{Status: SemanticHealthUnconfigured, Provider: "unchecked", Policy: "manual"}
	defer func() {
		e.semantic.mu.Lock()
		health.LastError = e.semantic.lastError
		e.semantic.mu.Unlock()
	}()

	configPath, err := e.Store.SemanticConfigFile()
	if err != nil {
		_ = e.evictSemanticResidentStateContext(ctx, semanticIndexMetadata{})
		_ = e.evictSemanticSourceStateContext(ctx)
		health.Status, health.NextAction, health.Detail = SemanticHealthCorrupt, "repair semantic local state", err.Error()
		return health
	}
	// LoadSemanticSettings 把不存在折叠为安全的 disabled 默认值；先用
	// Store 的同一有界/普通文件读取区分“从未配置”与“显式禁用”。
	if _, err := e.Store.LoadSemanticConfig(); err != nil {
		_ = e.evictSemanticResidentStateContext(ctx, semanticIndexMetadata{})
		_ = e.evictSemanticSourceStateContext(ctx)
		if os.IsNotExist(err) {
			health.NextAction = "iknowledge semantic configure --repo " + e.Store.RepoRoot()
			return health
		}
		health.Status, health.NextAction, health.Detail = SemanticHealthCorrupt, "repair semantic config", err.Error()
		return health
	}
	health.Configured = true
	cfg, err := LoadSemanticSettings(e.Store)
	if err != nil {
		_ = e.evictSemanticResidentStateContext(ctx, semanticIndexMetadata{})
		_ = e.evictSemanticSourceStateContext(ctx)
		health.Status, health.NextAction, health.Detail = SemanticHealthCorrupt, "repair semantic config", err.Error()
		return health
	}
	health.Enabled = cfg.Enabled
	health.Model = cfg.Model
	health.Profile = string(cfg.QueryProfile)
	health.Policy = string(cfg.RebuildPolicy)
	health.ConfiguredDimensions = cfg.Dimensions
	_ = configPath // 路径仅由 SemanticStatusText 展示，MCP 保持简洁。
	if !cfg.Enabled {
		_ = e.evictSemanticResidentStateContext(ctx, semanticIndexMetadata{})
		_ = e.evictSemanticSourceStateContext(ctx)
		health.Status = SemanticHealthDisabled
		if cfg.Endpoint == "" || cfg.Model == "" {
			health.NextAction = "iknowledge semantic configure --repo " + e.Store.RepoRoot()
		} else {
			health.NextAction = "iknowledge semantic enable --repo " + e.Store.RepoRoot()
		}
		return health
	}
	if !e.Store.Initialized() {
		_ = e.evictSemanticSourceStateContext(ctx)
		health.Status, health.NextAction = SemanticHealthConfiguredNoIndex, "iknowledge init --repo "+e.Store.RepoRoot()
		health.Detail = "知识库尚未初始化"
		return health
	}

	// An ai-* next_action must be executable. Build the same bounded local
	// manifest up front so status never tells an AI to spend its one session
	// sync attempt on a source that the interactive path must reject (>3000).
	// This remains offline: no API key, provider request or vector decode.
	var sourceFingerprint [32]byte
	sourceRecords := -1
	sourceReady := false
	loadSource := func() error {
		if sourceReady {
			return nil
		}
		if err := e.SyncContext(ctx); err != nil {
			return err
		}
		fingerprint, records, err := e.semanticSourceMetadata(ctx)
		if err != nil {
			return err
		}
		sourceFingerprint = fingerprint
		sourceRecords = records
		sourceReady = true
		return nil
	}
	if cfg.RebuildPolicy == SemanticRebuildAILocal || cfg.RebuildPolicy == SemanticRebuildAIRemote {
		if err := loadSource(); err != nil {
			if ctx.Err() != nil {
				return health
			}
			_ = e.invalidateSemanticSnapshotForStatusContext(ctx, semanticIndexMetadata{})
			health.Status, health.NextAction, health.Detail = SemanticHealthStaleSource, "repair knowledge source; then rebuild", err.Error()
			return health
		}
	}

	inspection, err := e.inspectSemanticIndexMetadata(ctx, cfg)
	if err != nil {
		if ctx.Err() != nil {
			return health
		}
		_ = e.invalidateSemanticSnapshotForStatusContext(ctx, semanticIndexMetadata{})
		if os.IsNotExist(err) {
			health.Status = SemanticHealthConfiguredNoIndex
			health.NextAction = e.semanticRebuildNextAction(health.Policy, sourceRecords)
			health.Detail = "semantic 索引不存在"
			if detail := semanticSyncLimitDetail(sourceRecords); detail != "" {
				health.Detail += "；" + detail
			}
			_ = e.evictSemanticSourceStateContext(ctx)
			return health
		}
		health.Status, health.NextAction, health.Detail = SemanticHealthCorrupt, e.semanticRebuildNextAction(health.Policy, sourceRecords), err.Error()
		return health
	}
	health.IndexDimensions = inspection.meta.Dimensions
	health.Records = inspection.meta.Records
	health.BuiltAt = inspection.meta.BuiltAt

	wantEmbedder, err := semanticOfflineEmbedderFingerprint(cfg)
	if err != nil {
		_ = e.invalidateSemanticSnapshotForStatusContext(ctx, inspection.meta)
		health.Status, health.NextAction, health.Detail = SemanticHealthCorrupt, "repair semantic config", err.Error()
		return health
	}
	if inspection.meta.SettingsFingerprint != SemanticSettingsFingerprint(cfg) ||
		inspection.meta.EmbedderFingerprint != wantEmbedder ||
		inspection.meta.QueryProbeFingerprint == "" ||
		(cfg.Dimensions > 0 && inspection.meta.Dimensions != cfg.Dimensions) {
		_ = e.invalidateSemanticSnapshotForStatusContext(ctx, inspection.meta)
		health.Status = SemanticHealthStaleProvider
		health.NextAction = e.semanticRebuildNextAction(health.Policy, sourceRecords)
		health.Detail = "provider/model/profile 配置与索引指纹不一致"
		return health
	}
	if err := loadSource(); err != nil {
		if ctx.Err() != nil {
			return health
		}
		_ = e.invalidateSemanticSnapshotForStatusContext(ctx, inspection.meta)
		health.Status, health.NextAction, health.Detail = SemanticHealthStaleSource, "repair knowledge source; then rebuild", err.Error()
		return health
	}
	if inspection.meta.SourceFingerprint != hex.EncodeToString(sourceFingerprint[:]) {
		key := semanticLoadedKey(cfg, sourceFingerprint)
		e.semantic.mu.Lock()
		knownCorrupt := e.semantic.corruptFile == inspection.identity && e.semantic.corruptErr != ""
		payloadLoaded := e.semantic.loadedKey == key && e.semantic.snapshot != nil &&
			e.semantic.loadedFile == inspection.identity && e.semantic.metadata == inspection.meta
		loadErr := e.semantic.corruptErr
		e.semantic.mu.Unlock()
		if knownCorrupt {
			health.Status = SemanticHealthCorrupt
			health.NextAction = e.semanticRebuildNextAction(health.Policy, sourceRecords)
			health.Detail = "此前 recall 已验证 vector payload 不可用: " + loadErr
			return health
		}
		if !payloadLoaded {
			// A different file/config generation may still be resident. Drop only
			// that mismatch; do not evict an already validated partial snapshot or
			// its query cache on every kb_status call.
			_ = e.invalidateSemanticSnapshotForStatusContext(ctx, inspection.meta)
		}
		health.PayloadLoaded = payloadLoaded
		health.Status = SemanticHealthPartial
		health.NextAction = e.semanticRebuildNextAction(health.Policy, sourceRecords)
		health.Detail = "知识已变化；仅使用 record source hash 仍匹配的旧向量，新改知识等待同步"
		if detail := semanticSyncLimitDetail(sourceRecords); detail != "" {
			health.Detail += "；" + detail
		}
		return health
	}

	key := semanticLoadedKey(cfg, sourceFingerprint)
	if err := e.semantic.residentMu.LockContext(ctx); err != nil {
		return health
	}
	e.semantic.mu.Lock()
	// A prior recall may already have paid for the full payload checksum/decode
	// and proved this exact settings+source generation bad. Reuse that local
	// evidence: status itself remains metadata-only, but must not erase a known
	// corruption and misleadingly return ready.
	if e.semantic.corruptFile == inspection.identity && e.semantic.corruptErr != "" {
		health.Status = SemanticHealthCorrupt
		health.NextAction = e.semanticRebuildNextAction(health.Policy, sourceRecords)
		health.Detail = "此前 recall 已验证 vector payload 不可用: " + e.semantic.corruptErr
		e.semantic.mu.Unlock()
		e.semantic.residentMu.Unlock()
		return health
	}
	health.PayloadLoaded = e.semantic.loadedKey == key && e.semantic.snapshot != nil &&
		e.semantic.loadedFile == inspection.identity
	if !health.PayloadLoaded {
		// 外部 rebuild 已替换 generation：淘汰旧矩阵，仅留轻量
		// metadata。真正 payload checksum/decode 延迟到 recall。
		e.semantic.loadedKey, e.semantic.loadErr = "", ""
		e.semantic.loadedAt, e.semantic.loadedFile = time.Time{}, semanticFileIdentity{}
		e.semantic.snapshot, e.semantic.metadata = nil, inspection.meta
		e.semantic.queryCache = nil
		e.semantic.failureUntil, e.semantic.lastError = time.Time{}, ""
	}
	process := e.semantic.process
	e.semantic.mu.Unlock()
	if !health.PayloadLoaded {
		process.releaseResident(e)
	}
	e.semantic.residentMu.Unlock()
	health.Status, health.NextAction = SemanticHealthReady, "none"
	if health.PayloadLoaded {
		health.Detail = "payload loaded"
	} else {
		health.Detail = "metadata valid; payload validation deferred to recall"
	}
	return health
}

// evictSemanticResidentState only releases rebuildable in-memory state. It
// deliberately leaves vector.idx and provider settings untouched, retains the
// last diagnostic error, and never performs provider or payload I/O.
func (e *Engine) evictSemanticResidentState(meta semanticIndexMetadata) {
	_ = e.evictSemanticResidentStateContext(context.Background(), meta)
}

func (e *Engine) evictSemanticResidentStateContext(ctx context.Context, meta semanticIndexMetadata) error {
	if err := e.semantic.residentMu.LockContext(ctx); err != nil {
		return err
	}
	defer e.semantic.residentMu.Unlock()
	e.semantic.mu.Lock()
	process := e.semantic.process
	e.semantic.loadedKey, e.semantic.loadErr = "", ""
	e.semantic.loadedAt, e.semantic.loadedFile = time.Time{}, semanticFileIdentity{}
	e.semantic.snapshot, e.semantic.metadata = nil, meta
	e.semantic.queryCache = nil
	e.semantic.failureUntil = time.Time{}
	e.semantic.mu.Unlock()
	process.releaseResident(e)
	return nil
}

// evictSemanticSourceState drops only the rebuildable typed-card manifest.
// Bumping the source version prevents an already-running constructor from
// publishing after disable/clear; its transient reservation is released when
// it observes the version mismatch.
func (e *Engine) evictSemanticSourceState() {
	_ = e.evictSemanticSourceStateContext(context.Background())
}

func (e *Engine) evictSemanticSourceStateContext(ctx context.Context) error {
	e.semantic.mu.Lock()
	process := e.semantic.process
	e.semantic.mu.Unlock()
	if err := e.rt.mu.LockContext(ctx); err != nil {
		return err
	}
	if err := e.semantic.sourceResidentMu.LockContext(ctx); err != nil {
		e.rt.mu.Unlock()
		return err
	}
	e.rt.semanticSourceVersion++
	if e.rt.semanticSourceVersion == 0 {
		e.rt.semanticSourceVersion = 1
	}
	e.rt.semanticManifest = semanticSourceManifest{}
	process.releaseSourceResident(e)
	e.semantic.sourceResidentMu.Unlock()
	e.rt.mu.Unlock()
	return nil
}

// invalidateSemanticSnapshotForStatus 在 status 发现外部 clear/corruption/stale
// 后淘汰本进程旧代，但不解码新 payload。
func (e *Engine) invalidateSemanticSnapshotForStatus(meta semanticIndexMetadata) {
	_ = e.invalidateSemanticSnapshotForStatusContext(context.Background(), meta)
}

func (e *Engine) invalidateSemanticSnapshotForStatusContext(ctx context.Context, meta semanticIndexMetadata) error {
	return e.evictSemanticResidentStateContext(ctx, meta)
}

// SemanticStatusText 保留 CLI 的人类可读输出，底层与 MCP 共用同一份
// 结构化、纯本地健康快照。
func (e *Engine) SemanticStatusText() (string, error) {
	return e.SemanticStatusTextContext(context.Background())
}

func (e *Engine) SemanticStatusTextContext(ctx context.Context) (string, error) {
	health, err := e.SemanticHealthSnapshotContext(ctx)
	if err != nil {
		return "", err
	}
	path, _ := e.Store.SemanticConfigFile()
	state := "disabled"
	if health.Enabled {
		state = "enabled"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "semantic: %s\nstatus: %s\nconfig: %s\nprovider: %s\nnext_action: %s\n",
		state, health.Status, path, health.Provider, health.NextAction)
	if health.Configured {
		fmt.Fprintf(&b, "model: %s\nquery_profile: %s\npolicy: %s\ndimensions: %d(auto=0)\n",
			health.Model, health.Profile, health.Policy, health.ConfiguredDimensions)
	}
	switch health.Status {
	case SemanticHealthReady:
		indexState := "metadata-valid (payload validation deferred to recall)"
		if health.PayloadLoaded {
			indexState = "ready"
		}
		fmt.Fprintf(&b, "index: %s\nrecords: %d\nindex_dimensions: %d\nbuilt_at: %s\n",
			indexState, health.Records, health.IndexDimensions, health.BuiltAt)
	case SemanticHealthPartial:
		fmt.Fprintf(&b, "index: partial (%s)\nrecords: %d\nindex_dimensions: %d\nbuilt_at: %s\n",
			health.Detail, health.Records, health.IndexDimensions, health.BuiltAt)
	case SemanticHealthConfiguredNoIndex, SemanticHealthStaleSource, SemanticHealthStaleProvider, SemanticHealthCorrupt:
		fmt.Fprintf(&b, "index: stale/unavailable (%s)\n", health.Detail)
	}
	if health.LastError != "" {
		fmt.Fprintf(&b, "last_error: %s\n", health.LastError)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

type semanticIndexInspection struct {
	meta        semanticIndexMetadata
	identity    semanticFileIdentity
	vectorBytes uint64
}

func (e *Engine) inspectSemanticIndexMetadata(ctx context.Context, cfg SemanticSettings) (semanticIndexInspection, error) {
	f, err := e.Store.OpenKnowledgeFileRead(semanticIndexRel)
	if err != nil {
		return semanticIndexInspection{}, err
	}
	defer f.Close()
	identity, err := readSemanticFileIdentity(ctx, f)
	if err != nil {
		return semanticIndexInspection{}, err
	}
	maxFile := semanticMaxIndexFileSize(cfg)
	if identity.Size < semanticWrapperSize || identity.Size > maxFile {
		return semanticIndexInspection{}, fmt.Errorf("semantic 索引大小 %d 超出允许范围", identity.Size)
	}
	meta, err := decodeSemanticIndexMetadataContext(ctx, f)
	if err != nil {
		return semanticIndexInspection{}, err
	}
	limits := semanticVectorLimits(cfg)
	records, dimensions := uint64(meta.Records), uint64(meta.Dimensions)
	if records > limits.MaxRecords || dimensions > uint64(limits.MaxDimensions) ||
		(dimensions > 0 && records > ^uint64(0)/(dimensions*4)) {
		return semanticIndexInspection{}, fmt.Errorf("semantic metadata 资源声明越界")
	}
	vectorBytes := records * dimensions * 4
	if vectorBytes > limits.MaxVectorBytes {
		return semanticIndexInspection{}, fmt.Errorf("semantic vector payload 声明 %d 超过上限 %d", vectorBytes, limits.MaxVectorBytes)
	}
	return semanticIndexInspection{meta: meta, identity: identity, vectorBytes: vectorBytes}, nil
}

func (e *Engine) inspectSemanticIndex(ctx context.Context, cfg SemanticSettings, source [32]byte) (semanticIndexInspection, error) {
	inspection, err := e.inspectSemanticIndexMetadata(ctx, cfg)
	if err != nil {
		if os.IsNotExist(err) {
			return semanticIndexInspection{}, fmt.Errorf("semantic 索引不存在；运行 iknowledge semantic rebuild")
		}
		return semanticIndexInspection{}, err
	}
	if err := validateSemanticIndexBinding(inspection.meta, cfg, source); err != nil {
		return semanticIndexInspection{}, err
	}
	return inspection, nil
}

func newSemanticGeneration() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("semantic generation entropy: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func marshalSemanticIndexMetadata(meta semanticIndexMetadata) ([]byte, [32]byte, error) {
	metadata, err := json.Marshal(meta)
	if err != nil {
		return nil, [32]byte{}, err
	}
	if len(metadata) == 0 || len(metadata) > semanticMaxMetadata {
		return nil, [32]byte{}, fmt.Errorf("semantic metadata 大小=%d 越界", len(metadata))
	}
	return metadata, sha256.Sum256(metadata), nil
}

// readSemanticFileIdentity 只读取固定 wrapper、≤64KiB metadata 与末尾 vector
// checksum，足以识别外部 CLI 的原子 generation 替换，而无需每次 recall 扫描
// 整个向量 payload。
func readSemanticFileIdentity(ctx context.Context, f *os.File) (semanticFileIdentity, error) {
	if ctx == nil {
		return semanticFileIdentity{}, fmt.Errorf("semantic identity: nil context")
	}
	if err := ctx.Err(); err != nil {
		return semanticFileIdentity{}, err
	}
	info, err := f.Stat()
	if err != nil {
		return semanticFileIdentity{}, err
	}
	if info.Size() < semanticWrapperSize+32 {
		return semanticFileIdentity{}, fmt.Errorf("semantic 索引过短: %d", info.Size())
	}
	var header [semanticWrapperSize]byte
	if _, err := f.ReadAt(header[:], 0); err != nil {
		return semanticFileIdentity{}, fmt.Errorf("semantic identity header: %w", err)
	}
	if subtle.ConstantTimeCompare(header[:8], semanticIndexMagic[:]) != 1 ||
		binary.LittleEndian.Uint16(header[8:10]) != semanticIndexVersion ||
		binary.LittleEndian.Uint16(header[10:12]) != 0 {
		return semanticFileIdentity{}, fmt.Errorf("semantic identity header 非法")
	}
	metadataLen := binary.LittleEndian.Uint32(header[12:16])
	if metadataLen == 0 || metadataLen > semanticMaxMetadata ||
		int64(semanticWrapperSize)+int64(metadataLen)+32 > info.Size() {
		return semanticFileIdentity{}, fmt.Errorf("semantic identity metadata 长度=%d 非法", metadataLen)
	}
	metadata := make([]byte, int(metadataLen))
	if _, err := f.ReadAt(metadata, semanticWrapperSize); err != nil {
		return semanticFileIdentity{}, fmt.Errorf("semantic identity metadata: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return semanticFileIdentity{}, err
	}
	metadataChecksum := sha256.Sum256(metadata)
	if subtle.ConstantTimeCompare(header[16:48], metadataChecksum[:]) != 1 {
		return semanticFileIdentity{}, fmt.Errorf("semantic metadata checksum 不匹配")
	}
	var vectorChecksum [32]byte
	if _, err := f.ReadAt(vectorChecksum[:], info.Size()-int64(len(vectorChecksum))); err != nil {
		return semanticFileIdentity{}, fmt.Errorf("semantic identity vector checksum: %w", err)
	}
	return semanticFileIdentity{
		Size: info.Size(), HeaderChecksum: sha256.Sum256(header[:]),
		MetadataChecksum: metadataChecksum, VectorChecksum: vectorChecksum,
	}, nil
}

func encodeSemanticIndex(w io.Writer, meta semanticIndexMetadata, snapshot *vector.Snapshot) error {
	return encodeSemanticIndexContext(context.Background(), w, meta, snapshot)
}

func encodeSemanticIndexContext(ctx context.Context, w io.Writer, meta semanticIndexMetadata, snapshot *vector.Snapshot) error {
	if ctx == nil {
		return fmt.Errorf("semantic encode: nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	metadata, checksum, err := marshalSemanticIndexMetadata(meta)
	if err != nil {
		return err
	}
	var header [semanticWrapperSize]byte
	copy(header[:8], semanticIndexMagic[:])
	binary.LittleEndian.PutUint16(header[8:10], semanticIndexVersion)
	binary.LittleEndian.PutUint16(header[10:12], 0)
	binary.LittleEndian.PutUint32(header[12:16], uint32(len(metadata)))
	copy(header[16:48], checksum[:])
	if err := writeSemanticAll(w, header[:]); err != nil {
		return err
	}
	if err := writeSemanticAll(w, metadata); err != nil {
		return err
	}
	return vector.EncodeContext(ctx, w, snapshot)
}

func decodeSemanticIndex(r io.Reader, limits vector.Limits) (semanticIndexMetadata, *vector.Snapshot, error) {
	return decodeSemanticIndexContext(context.Background(), r, limits)
}

func decodeSemanticIndexContext(ctx context.Context, r io.Reader, limits vector.Limits) (semanticIndexMetadata, *vector.Snapshot, error) {
	meta, err := decodeSemanticIndexMetadataContext(ctx, r)
	if err != nil {
		return semanticIndexMetadata{}, nil, err
	}
	snapshot, err := vector.DecodeWithLimitsContext(ctx, r, limits)
	if err != nil {
		return semanticIndexMetadata{}, nil, err
	}
	return meta, snapshot, nil
}

func decodeSemanticIndexMetadataContext(ctx context.Context, r io.Reader) (semanticIndexMetadata, error) {
	if ctx == nil {
		return semanticIndexMetadata{}, fmt.Errorf("semantic decode: nil context")
	}
	if err := ctx.Err(); err != nil {
		return semanticIndexMetadata{}, err
	}
	var header [semanticWrapperSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return semanticIndexMetadata{}, fmt.Errorf("semantic index header: %w", err)
	}
	if subtle.ConstantTimeCompare(header[:8], semanticIndexMagic[:]) != 1 {
		return semanticIndexMetadata{}, fmt.Errorf("semantic index magic 非法")
	}
	if version := binary.LittleEndian.Uint16(header[8:10]); version != semanticIndexVersion {
		return semanticIndexMetadata{}, fmt.Errorf("semantic index version=%d 不支持", version)
	}
	if binary.LittleEndian.Uint16(header[10:12]) != 0 {
		return semanticIndexMetadata{}, fmt.Errorf("semantic index flags 非法")
	}
	metadataLen := binary.LittleEndian.Uint32(header[12:16])
	if metadataLen == 0 || metadataLen > semanticMaxMetadata {
		return semanticIndexMetadata{}, fmt.Errorf("semantic metadata 长度=%d 越界", metadataLen)
	}
	metadata := make([]byte, int(metadataLen))
	if _, err := io.ReadFull(r, metadata); err != nil {
		return semanticIndexMetadata{}, fmt.Errorf("semantic metadata: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return semanticIndexMetadata{}, err
	}
	actual := sha256.Sum256(metadata)
	if subtle.ConstantTimeCompare(header[16:48], actual[:]) != 1 {
		return semanticIndexMetadata{}, fmt.Errorf("semantic metadata checksum 不匹配")
	}
	var meta semanticIndexMetadata
	dec := json.NewDecoder(strings.NewReader(string(metadata)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&meta); err != nil {
		return semanticIndexMetadata{}, fmt.Errorf("semantic metadata JSON: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return semanticIndexMetadata{}, fmt.Errorf("semantic metadata 有尾随 JSON")
	}
	if err := validateSemanticIndexMetadata(meta); err != nil {
		return semanticIndexMetadata{}, err
	}
	return meta, nil
}

func validateSemanticIndexMetadata(meta semanticIndexMetadata) error {
	if meta.Schema != 1 || meta.Dimensions < 1 || meta.Dimensions > 4096 || meta.Records < 0 ||
		len(meta.Generation) != 32 ||
		len(meta.SettingsFingerprint) == 0 || len(meta.SettingsFingerprint) > 256 ||
		len(meta.EmbedderFingerprint) == 0 || len(meta.EmbedderFingerprint) > 256 ||
		len(meta.ProbeFingerprint) == 0 || len(meta.ProbeFingerprint) > 128 ||
		len(meta.QueryProbeFingerprint) > 128 ||
		len(meta.SourceFingerprint) != 64 || len(meta.BuiltAt) == 0 || len(meta.BuiltAt) > 64 {
		return fmt.Errorf("semantic metadata 字段非法")
	}
	if _, err := hex.DecodeString(meta.Generation); err != nil {
		return fmt.Errorf("semantic generation 非十六进制")
	}
	if _, err := hex.DecodeString(meta.SourceFingerprint); err != nil {
		return fmt.Errorf("semantic source fingerprint 非十六进制")
	}
	if _, err := time.Parse(time.RFC3339, meta.BuiltAt); err != nil {
		return fmt.Errorf("semantic built_at 非法")
	}
	return nil
}

func writeSemanticAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
