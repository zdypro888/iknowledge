package engine

import "context"

// semanticSourceSnapshot is intentionally test-only. Production code must
// choose metadata, documents, or a generation view explicitly so ownership
// cannot be discarded while a shallow manifest map is still in use.
func (e *Engine) semanticSourceSnapshot(ctx context.Context, includeText bool) ([]semanticDocument, semanticSourceManifest, error) {
	docs, manifest, lease, err := e.semanticSourceSnapshotLease(ctx, includeText)
	if lease != nil {
		lease.Release()
	}
	return docs, manifest, err
}
