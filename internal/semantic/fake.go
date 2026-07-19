package semantic

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
)

const maxFakeDimensions = 4_096

// DeterministicFake is a dependency-free embedder for tests. Query and
// document vectors intentionally occupy different deterministic streams so a
// test can catch accidental use of the wrong embedding mode.
type DeterministicFake struct {
	dimensions  int
	fingerprint string
}

// NewDeterministicFake returns a fake with a stable algorithm and fingerprint.
func NewDeterministicFake(dimensions int) (*DeterministicFake, error) {
	if dimensions <= 0 || dimensions > maxFakeDimensions {
		return nil, fmt.Errorf("semantic: fake dimensions must be between 1 and %d", maxFakeDimensions)
	}
	return &DeterministicFake{
		dimensions:  dimensions,
		fingerprint: fingerprint("deterministic-fake-v1", fmt.Sprint(dimensions)),
	}, nil
}

func (f *DeterministicFake) Fingerprint() string { return f.fingerprint }

func (f *DeterministicFake) EmbedQuery(ctx context.Context, query string) ([]float32, error) {
	return f.embed(ctx, "query", query)
}

func (f *DeterministicFake) EmbedDocuments(ctx context.Context, documents []string) ([][]float32, error) {
	if ctx == nil {
		return nil, fmt.Errorf("semantic: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(documents) == 0 {
		return nil, nil
	}
	out := make([][]float32, len(documents))
	for i, document := range documents {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		vector, err := f.embed(ctx, "document", document)
		if err != nil {
			return nil, err
		}
		out[i] = vector
	}
	return out, nil
}

func (f *DeterministicFake) embed(ctx context.Context, mode, text string) ([]float32, error) {
	if ctx == nil {
		return nil, fmt.Errorf("semantic: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	seed := sha256.Sum256([]byte("iknowledge-semantic-fake-v1\x00" + mode + "\x00" + text))
	vector := make([]float32, f.dimensions)
	var norm float64
	var counter uint64
	for offset := 0; offset < len(vector); {
		if offset&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		var blockInput [40]byte
		copy(blockInput[:32], seed[:])
		binary.LittleEndian.PutUint64(blockInput[32:], counter)
		block := sha256.Sum256(blockInput[:])
		counter++
		for i := 0; i+1 < len(block) && offset < len(vector); i += 2 {
			value := float32(int16(binary.LittleEndian.Uint16(block[i:i+2]))) / 32768
			vector[offset] = value
			norm += float64(value) * float64(value)
			offset++
		}
	}
	if norm == 0 {
		vector[0] = 1
		return vector, nil
	}
	scale := float32(1 / math.Sqrt(norm))
	for i := range vector {
		vector[i] *= scale
	}
	return vector, nil
}
