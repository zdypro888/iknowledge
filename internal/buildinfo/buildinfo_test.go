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
