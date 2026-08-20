package buildinfo

import "testing"

func TestInjectedVersionTakesPrecedence(t *testing.T) {
	old := Version
	Version = "v9.9.9-test"
	t.Cleanup(func() { Version = old })
	if got := Read().Version; got != Version {
		t.Fatalf("Read().Version=%q,want injected %q", got, Version)
	}
}

func TestSameRuntimePrefersExecutableDigest(t *testing.T) {
	a := RuntimeIdentity{Version: "same", Revision: "same", ExecutableSHA256: "aaa"}
	b := RuntimeIdentity{Version: "same", Revision: "same", ExecutableSHA256: "bbb"}
	if SameRuntime(a, b) {
		t.Fatal("different executable digests must not be treated as one daemon generation")
	}
	b.ExecutableSHA256 = "aaa"
	b.Version = "display-only-difference"
	if !SameRuntime(a, b) {
		t.Fatal("matching executable digests should identify the same generation")
	}
}
