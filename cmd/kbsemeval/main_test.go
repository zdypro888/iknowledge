package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// TestCommittedSemanticBaseline makes the checked-in qrels an ordinary
// `go test ./...` release gate. The vectors are intentionally deterministic:
// this protects lane isolation, distinct-node winners, and stable ordering,
// while real-model quality remains a separate versioned benchmark.
func TestCommittedSemanticBaseline(t *testing.T) {
	input := filepath.Join("..", "..", "eval", "semantic", "v1", "qrels.jsonl")
	var stdout, stderr bytes.Buffer
	code := run([]string{"--input", input}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("strict semantic baseline exit=%d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "PASS: semantic retrieval regression baseline") {
		t.Fatalf("strict semantic baseline did not report PASS:\n%s", stdout.String())
	}
}
