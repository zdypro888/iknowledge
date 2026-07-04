package engine

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultScoutCommandDoesNotUseShell(t *testing.T) {
	cfgPath := "/tmp/repo with spaces/.knowledge/local/scout-mcp-job.json"
	cmd := scoutCommand("", cfgPath)
	if filepath.Base(cmd.Path) != "claude" {
		t.Fatalf("default scout command path = %q, want claude", cmd.Path)
	}
	got := strings.Join(cmd.Args, "\x00")
	for _, want := range []string{"--mcp-config", cfgPath, "--strict-mcp-config", "--allowedTools", "mcp__knowledge__*"} {
		if !strings.Contains(got, want) {
			t.Fatalf("default scout args missing %q: %#v", want, cmd.Args)
		}
	}
	for _, arg := range cmd.Args {
		if strings.Contains(arg, " ") && arg != cfgPath {
			t.Fatalf("unexpected shell-like joined arg %q in %#v", arg, cmd.Args)
		}
	}
}

func TestCustomScoutCommandUsesShellTemplate(t *testing.T) {
	cmd := scoutCommand("GO_SCOUT_HELPER=1 helper -- {mcp}", "/tmp/cfg.json")
	if filepath.Base(cmd.Path) != "sh" || len(cmd.Args) != 3 || cmd.Args[1] != "-c" {
		t.Fatalf("custom scout command should use sh -c, got path=%q args=%#v", cmd.Path, cmd.Args)
	}
	if !strings.Contains(cmd.Args[2], "/tmp/cfg.json") {
		t.Fatalf("custom scout command did not substitute config path: %#v", cmd.Args)
	}
}
