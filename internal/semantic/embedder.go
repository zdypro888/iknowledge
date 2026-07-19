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
