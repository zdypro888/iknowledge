package engine

import (
	"fmt"
	"sync"
	"time"
)

const (
	// SemanticProcessResidentMaxMiB is the hard logical resident budget used by
	// one multi-repository daemon. Each loaded or rebuilding repository reserves
	// its configured max_vector_mib, so hot enable/config changes cannot bypass
	// the startup preflight.
	SemanticProcessResidentMaxMiB = 1024
	// SemanticProcessSourceMaxMiB independently bounds cached typed-card
	// manifests plus the one in-progress source build. Vector authorization stays
	// at 1GiB; source preprocessing therefore cannot consume an unbounded amount
	// merely because a daemon serves many repositories.
	SemanticProcessSourceMaxMiB = 384
	semanticSourceBuildMaxMiB   = 192
)

// SemanticProcessCoordinator bounds semantic work shared by every Engine in
// one daemon. CLI processes intentionally do not install one: they operate on
// a single repository and remain bounded by that repository's vector limits.
//
// Reservations are conservative authorizations (max_vector_mib), not sampled
// heap usage. Resident and in-progress rebuild matrices are counted separately,
// while resident leases ensure external generation replacement cannot overlap
// an old matrix still used by a search.
type SemanticProcessCoordinator struct {
	mu                sync.Mutex
	maxResidentBytes  uint64
	usedResidentBytes uint64
	resident          map[*Engine]uint64
	transient         map[*Engine]uint64
	maxSourceBytes    uint64
	usedSourceBytes   uint64
	sourceResident    map[*Engine]uint64
	sourceTransient   map[*Engine]uint64
	sourceDocuments   map[*Engine]uint64
	rebuildGate       chan struct{}
	sourceGate        chan struct{}
	providerGate      chan struct{}
	searchGate        chan struct{}
}

// NewSemanticProcessCoordinator creates the shared semantic resource boundary
// for a daemon. Non-positive budgets fall back to the production hard limit.
func NewSemanticProcessCoordinator(maxResidentMiB int) *SemanticProcessCoordinator {
	if maxResidentMiB <= 0 {
		maxResidentMiB = SemanticProcessResidentMaxMiB
	}
	return &SemanticProcessCoordinator{
		maxResidentBytes: uint64(maxResidentMiB) << 20,
		resident:         make(map[*Engine]uint64),
		transient:        make(map[*Engine]uint64),
		maxSourceBytes:   uint64(SemanticProcessSourceMaxMiB) << 20,
		sourceResident:   make(map[*Engine]uint64),
		sourceTransient:  make(map[*Engine]uint64),
		sourceDocuments:  make(map[*Engine]uint64),
		rebuildGate:      make(chan struct{}, 1),
		sourceGate:       make(chan struct{}, 1),
		providerGate:     make(chan struct{}, semanticProviderConcurrency),
		searchGate:       make(chan struct{}, semanticSearchConcurrency),
	}
}

func (c *SemanticProcessCoordinator) reserveSourceTransient(owner *Engine) error {
	if c == nil || owner == nil {
		return nil
	}
	bytes := uint64(semanticSourceBuildMaxMiB) << 20
	c.mu.Lock()
	defer c.mu.Unlock()
	current := c.sourceTransient[owner]
	if current != 0 || c.sourceDocuments[owner] != 0 {
		return fmt.Errorf("semantic source 构造/文档 lease 已在进行")
	}
	other := c.usedSourceBytes - current
	if other > c.maxSourceBytes || bytes > c.maxSourceBytes-other {
		return fmt.Errorf("semantic source 进程预算不足: 已驻留 %.1fMiB，本次构造需 %.1fMiB，硬上限 %.1fMiB；请 clear/disable 其他仓库或拆分 daemon",
			float64(other)/(1<<20), float64(bytes)/(1<<20), float64(c.maxSourceBytes)/(1<<20))
	}
	c.sourceTransient[owner] = bytes
	c.usedSourceBytes = other + bytes
	return nil
}

func (c *SemanticProcessCoordinator) promoteSourceTransient(owner *Engine, residentBytes, documentBytes uint64) error {
	if c == nil || owner == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	transient := c.sourceTransient[owner]
	if transient == 0 {
		return fmt.Errorf("semantic source 发布丢失构造预算")
	}
	if c.sourceDocuments[owner] != 0 {
		return fmt.Errorf("semantic source 发布时仍有旧 documents lease")
	}
	if residentBytes > transient || documentBytes > transient-residentBytes {
		return fmt.Errorf("semantic source manifest+documents 驻留估算 %.1fMiB 超过构造授权 %.1fMiB",
			float64(residentBytes+documentBytes)/(1<<20), float64(transient)/(1<<20))
	}
	c.usedSourceBytes -= c.sourceResident[owner]
	c.usedSourceBytes -= transient
	delete(c.sourceTransient, owner)
	if residentBytes == 0 {
		delete(c.sourceResident, owner)
	} else {
		c.sourceResident[owner] = residentBytes
		c.usedSourceBytes += residentBytes
	}
	if documentBytes == 0 {
		delete(c.sourceDocuments, owner)
	} else {
		c.sourceDocuments[owner] = documentBytes
		c.usedSourceBytes += documentBytes
	}
	return nil
}

func (c *SemanticProcessCoordinator) releaseSourceResident(owner *Engine) {
	if c == nil || owner == nil {
		return
	}
	c.mu.Lock()
	c.usedSourceBytes -= c.sourceResident[owner]
	delete(c.sourceResident, owner)
	c.mu.Unlock()
}

func (c *SemanticProcessCoordinator) releaseSourceTransient(owner *Engine) {
	if c == nil || owner == nil {
		return
	}
	c.mu.Lock()
	c.usedSourceBytes -= c.sourceTransient[owner]
	delete(c.sourceTransient, owner)
	c.mu.Unlock()
}

func (c *SemanticProcessCoordinator) releaseSourceDocuments(owner *Engine) {
	if c == nil || owner == nil {
		return
	}
	c.mu.Lock()
	c.usedSourceBytes -= c.sourceDocuments[owner]
	delete(c.sourceDocuments, owner)
	c.mu.Unlock()
}

func (c *SemanticProcessCoordinator) sourceReservedBytes(owner *Engine) uint64 {
	if c == nil || owner == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sourceResident[owner] + c.sourceTransient[owner] + c.sourceDocuments[owner]
}

func (c *SemanticProcessCoordinator) reserveResident(owner *Engine, bytes uint64) error {
	if c == nil || owner == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	current := c.resident[owner]
	other := c.usedResidentBytes - current
	if other > c.maxResidentBytes || bytes > c.maxResidentBytes-other {
		return fmt.Errorf("semantic 进程驻留预算不足: 已授权 %.1fMiB，本仓请求 %.1fMiB，硬上限 %.1fMiB；请 clear/disable 其他仓库、降低 --max-vector-mib，或拆分 daemon",
			float64(other)/(1<<20), float64(bytes)/(1<<20), float64(c.maxResidentBytes)/(1<<20))
	}
	c.resident[owner] = bytes
	c.usedResidentBytes = other + bytes
	return nil
}

func (c *SemanticProcessCoordinator) reserveTransient(owner *Engine, bytes uint64) error {
	if c == nil || owner == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	current := c.transient[owner]
	other := c.usedResidentBytes - current
	if other > c.maxResidentBytes || bytes > c.maxResidentBytes-other {
		return fmt.Errorf("semantic 进程驻留预算不足: 已授权 %.1fMiB，本次重建另需 %.1fMiB，硬上限 %.1fMiB；请 clear/disable 其他仓库、降低 --max-vector-mib，或拆分 daemon",
			float64(other)/(1<<20), float64(bytes)/(1<<20), float64(c.maxResidentBytes)/(1<<20))
	}
	c.transient[owner] = bytes
	c.usedResidentBytes = other + bytes
	return nil
}

func (c *SemanticProcessCoordinator) promoteTransient(owner *Engine) {
	if c == nil || owner == nil {
		return
	}
	c.mu.Lock()
	oldResident := c.resident[owner]
	newResident := c.transient[owner]
	c.usedResidentBytes -= oldResident
	delete(c.transient, owner)
	if newResident == 0 {
		delete(c.resident, owner)
	} else {
		c.resident[owner] = newResident
	}
	c.mu.Unlock()
}

func (c *SemanticProcessCoordinator) releaseResident(owner *Engine) {
	if c == nil || owner == nil {
		return
	}
	c.mu.Lock()
	c.usedResidentBytes -= c.resident[owner]
	delete(c.resident, owner)
	c.mu.Unlock()
}

func (c *SemanticProcessCoordinator) releaseTransient(owner *Engine) {
	if c == nil || owner == nil {
		return
	}
	c.mu.Lock()
	c.usedResidentBytes -= c.transient[owner]
	delete(c.transient, owner)
	c.mu.Unlock()
}

func (c *SemanticProcessCoordinator) releaseAll(owner *Engine) {
	if c == nil || owner == nil {
		return
	}
	c.mu.Lock()
	c.usedResidentBytes -= c.resident[owner]
	c.usedResidentBytes -= c.transient[owner]
	c.usedSourceBytes -= c.sourceResident[owner]
	c.usedSourceBytes -= c.sourceTransient[owner]
	c.usedSourceBytes -= c.sourceDocuments[owner]
	delete(c.resident, owner)
	delete(c.transient, owner)
	delete(c.sourceResident, owner)
	delete(c.sourceTransient, owner)
	delete(c.sourceDocuments, owner)
	c.mu.Unlock()
}

func (c *SemanticProcessCoordinator) reservedBytes(owner *Engine) uint64 {
	if c == nil || owner == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.resident[owner] + c.transient[owner]
}

// SetSemanticProcessCoordinator attaches an Engine to its daemon-wide
// coordinator. It must run during startup, before semantic work begins.
func (e *Engine) SetSemanticProcessCoordinator(coordinator *SemanticProcessCoordinator) error {
	if e == nil || coordinator == nil {
		return fmt.Errorf("semantic process coordinator: nil")
	}
	e.semantic.residentMu.Lock()
	defer e.semantic.residentMu.Unlock()
	e.semantic.mu.Lock()
	defer e.semantic.mu.Unlock()
	e.rt.mu.RLock()
	sourceReady := e.rt.semanticManifest.ready
	e.rt.mu.RUnlock()
	if e.semantic.snapshot != nil || e.semantic.building || e.semantic.process != nil ||
		e.semantic.sourceGate != nil || sourceReady {
		return fmt.Errorf("semantic process coordinator 必须在首次 semantic 操作前设置")
	}
	e.semantic.process = coordinator
	return nil
}

// ReleaseSemanticProcessResources is an idempotent daemon-shutdown hook. The
// HTTP server first drains/cancels requests, so no semantic lease or rebuild is
// active when this is called.
func (e *Engine) ReleaseSemanticProcessResources() {
	if e == nil {
		return
	}
	e.semantic.residentMu.Lock()
	e.semantic.mu.Lock()
	process := e.semantic.process
	e.semantic.loadedKey, e.semantic.loadErr = "", ""
	e.semantic.loadedAt, e.semantic.loadedFile = time.Time{}, semanticFileIdentity{}
	e.semantic.snapshot, e.semantic.metadata = nil, semanticIndexMetadata{}
	e.semantic.corruptFile, e.semantic.corruptErr = semanticFileIdentity{}, ""
	e.semantic.queryCache = nil
	e.semantic.failureUntil = time.Time{}
	e.semantic.building = false
	e.semantic.closing = true
	e.semantic.mu.Unlock()
	e.semantic.residentMu.Unlock()
	e.rt.mu.Lock()
	e.semantic.sourceResidentMu.Lock()
	// Invalidate every constructor that captured the pre-shutdown truth
	// generation. The closing flag rejects new constructors; the version bump
	// prevents an in-flight one from publishing after releaseAll retired its
	// transient reservation.
	e.rt.semanticSourceVersion++
	if e.rt.semanticSourceVersion == 0 {
		e.rt.semanticSourceVersion = 1
	}
	e.rt.semanticManifest = semanticSourceManifest{}
	process.releaseAll(e)
	e.semantic.sourceResidentMu.Unlock()
	e.rt.mu.Unlock()
}
