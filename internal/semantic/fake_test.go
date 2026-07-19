package semantic

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"testing"
)

var _ Embedder = (*DeterministicFake)(nil)

func TestDeterministicFakeSeparatesModes(t *testing.T) {
	fake, err := NewDeterministicFake(8)
	if err != nil {
		t.Fatal(err)
	}
	query, err := fake.EmbedQuery(context.Background(), "same text")
	if err != nil {
		t.Fatal(err)
	}
	documents, err := fake.EmbedDocuments(context.Background(), []string{"same text", "other"})
	if err != nil {
		t.Fatal(err)
	}
	if len(query) != 8 || len(documents) != 2 || len(documents[0]) != 8 {
		t.Fatalf("unexpected dimensions: query=%d documents=%v", len(query), documents)
	}
	if reflect.DeepEqual(query, documents[0]) {
		t.Fatal("query and document modes returned the same vector")
	}
	again, err := fake.EmbedQuery(context.Background(), "same text")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(query, again) {
		t.Fatalf("fake is not deterministic: %v != %v", query, again)
	}
	var norm float64
	for _, value := range query {
		norm += float64(value) * float64(value)
	}
	if math.Abs(math.Sqrt(norm)-1) > 1e-5 {
		t.Fatalf("query vector norm = %f", math.Sqrt(norm))
	}
}

func TestDeterministicFakeDoesNotAliasResults(t *testing.T) {
	fake, err := NewDeterministicFake(4)
	if err != nil {
		t.Fatal(err)
	}
	first, err := fake.EmbedQuery(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	want := append([]float32(nil), first...)
	first[0] = 42
	second, err := fake.EmbedQuery(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(second, want) {
		t.Fatalf("mutating one result changed another: got %v want %v", second, want)
	}
}

func TestDeterministicFakeFingerprint(t *testing.T) {
	first, _ := NewDeterministicFake(4)
	second, _ := NewDeterministicFake(4)
	different, _ := NewDeterministicFake(5)
	if first.Fingerprint() == "" || first.Fingerprint() != second.Fingerprint() {
		t.Fatalf("fingerprint is not stable: %q %q", first.Fingerprint(), second.Fingerprint())
	}
	if first.Fingerprint() == different.Fingerprint() {
		t.Fatal("dimension change did not change fingerprint")
	}
}

func TestFingerprintEncodingIsUnambiguous(t *testing.T) {
	first := fingerprint("schema", "a\x00b", "c")
	second := fingerprint("schema", "a", "b\x00c")
	if first == second {
		t.Fatalf("length-distinct inputs collided: %q", first)
	}
}

func TestDeterministicFakeRejectsInvalidDimensions(t *testing.T) {
	for _, dimensions := range []int{-1, 0, maxFakeDimensions + 1} {
		t.Run(fmt.Sprint(dimensions), func(t *testing.T) {
			if _, err := NewDeterministicFake(dimensions); err == nil {
				t.Fatalf("NewDeterministicFake(%d) succeeded", dimensions)
			}
		})
	}
}

func TestDeterministicFakeHonorsContext(t *testing.T) {
	fake, err := NewDeterministicFake(4)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fake.EmbedQuery(ctx, "x"); err != context.Canceled {
		t.Fatalf("EmbedQuery error = %v", err)
	}
	if _, err := fake.EmbedDocuments(ctx, nil); err != context.Canceled {
		t.Fatalf("EmbedDocuments error = %v", err)
	}
	if _, err := fake.EmbedQuery(nil, "x"); err == nil {
		t.Fatal("nil context succeeded")
	}
}
