// Package semantic provides embedding providers for the optional semantic
// search layer. It does not own configuration, credentials, or index storage.
package semantic

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
)

// Embedder deliberately separates queries from documents. Some embedding
// models apply different instructions to the two modes, so collapsing them at
// this boundary would make a later provider upgrade silently change ranking.
type Embedder interface {
	// Fingerprint is a stable, credential-free identity for the provider
	// configuration. A changed fingerprint invalidates derived vectors.
	Fingerprint() string
	EmbedQuery(ctx context.Context, query string) ([]float32, error)
	EmbedDocuments(ctx context.Context, documents []string) ([][]float32, error)
}

// CanaryEmbedder pairs an ordinary embedding result with an observed canary
// fingerprint in the same provider request. Engine uses this capability to
// detect common accidental drift between index generations or queries.
// A canary is not remote attestation: an adversarial endpoint can route inputs
// differently or spoof a fixed canary. Strong model identity still requires a
// trusted endpoint and immutable revision. Safety-sensitive engine paths fail
// closed when an implementation cannot provide this same-request check.
type CanaryEmbedder interface {
	Embedder
	EmbedQueryWithCanary(ctx context.Context, query, canary string) (queryVector, canaryVector []float32, err error)
	EmbedDocumentsWithCanary(ctx context.Context, documents []string, canary string) (documentVectors [][]float32, canaryVector []float32, err error)
}

// DualModeCanaryEmbedder checks a rebuild batch against both sides of an
// asymmetric retrieval contract in one provider request. This matters for
// instruction-aware models: a document-only canary cannot even detect common
// query-mode drift. It still does not prove a remote model's identity.
type DualModeCanaryEmbedder interface {
	CanaryEmbedder
	EmbedDocumentsWithDualCanary(ctx context.Context, documents []string, documentCanary, queryCanary string) (
		documentVectors [][]float32, documentCanaryVector, queryCanaryVector []float32, err error)
}

func fingerprint(namespace string, parts ...string) string {
	h := sha256.New()
	writeFingerprintPart(h, namespace)
	for _, part := range parts {
		writeFingerprintPart(h, part)
	}
	return namespace + ":" + hex.EncodeToString(h.Sum(nil))
}

type byteWriter interface {
	Write([]byte) (int, error)
}

func writeFingerprintPart(w byteWriter, part string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(part)))
	_, _ = w.Write(size[:])
	_, _ = w.Write([]byte(part))
}
