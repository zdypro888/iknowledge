package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func envWith(overrides map[string]string) []string {
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if _, replaced := overrides[key]; !replaced {
			env = append(env, item)
		}
	}
	for key, value := range overrides {
		env = append(env, key+"="+value)
	}
	return env
}

const fakeFrozenSourceGo = `#!/bin/sh
if [ -n "${FAKE_GO_LOG:-}" ]; then printf '%s\n' "$*" >> "$FAKE_GO_LOG"; fi
if [ "$1" = "list" ]; then
  case "$*" in
    *'{{.Dir}}'*) printf '%s\n' "$FAKE_MODULE_DIR" ;;
    *) printf '%s\n' "$FAKE_RESOLVED_VERSION" ;;
  esac
  exit "${FAKE_LIST_EXIT:-0}"
fi
if [ "$1" = "mod" ] && [ "$2" = "download" ]; then exit "${FAKE_DOWNLOAD_EXIT:-0}"; fi
if [ "$1" != "install" ]; then exit 2; fi
printf '%s\n' '#!/bin/sh' 'printf "iknowledge '"$FAKE_BINARY_VERSION"' go1.test\n"' > "$GOBIN/iknowledge"
chmod +x "$GOBIN/iknowledge"
`

func prepareFakeFrozenModule(t *testing.T, fakeBin, moduleDir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(moduleDir, "skills", "kb-bootstrap"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(moduleDir, "go.mod"), []byte("module github.com/zdypro888/iknowledge\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(moduleDir, "skills", "kb-bootstrap", "SKILL.md"), []byte("FROZEN-MODULE-SKILL\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(fakeBin, "go"), fakeFrozenSourceGo)
}

// 同时覆盖两个脚本回归：IKNOWLEDGE_FORCE_SOURCE 必须在任何 Release 请求前
// 生效；curl | sh 的 $0=sh 绝不能从恶意 cwd 复制 skills/kb-bootstrap。
func TestInstallScriptForceSourceAndPipeIsolation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("依赖 POSIX sh；Windows 路径由交叉编译与 sh -n 护栏覆盖")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh unavailable")
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	installScript, err := os.ReadFile(filepath.Join(repoRoot, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}

	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	installDir := filepath.Join(tmp, "bin")
	fakeBin := filepath.Join(tmp, "fake-bin")
	moduleDir := filepath.Join(tmp, "module-cache")
	maliciousCWD := filepath.Join(tmp, "malicious-repo")
	for _, dir := range []string{home, installDir, fakeBin, filepath.Join(maliciousCWD, "skills", "kb-bootstrap")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(maliciousCWD, "skills", "kb-bootstrap", "SKILL.md"),
		[]byte("MALICIOUS-CWD-SKILL\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	goLog := filepath.Join(tmp, "go.log")
	curlLog := filepath.Join(tmp, "curl.log")
	prepareFakeFrozenModule(t, fakeBin, moduleDir)
	if err := os.WriteFile(curlLog, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(fakeBin, "curl"), `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_CURL_LOG"
out=''
while [ "$#" -gt 0 ]; do
  if [ "$1" = '-o' ]; then shift; out="$1"; fi
  shift
done
[ -n "$out" ] || exit 3
printf '%s\n' 'REMOTE-TRUSTED-SKILL' > "$out"
`)

	cmd := exec.Command(sh, "-s")
	cmd.Dir = maliciousCWD
	cmd.Stdin = bytes.NewReader(installScript) // 模拟 curl ... | sh
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	cmd.Env = envWith(map[string]string{
		"HOME":                      home,
		"PATH":                      installDir + string(os.PathListSeparator) + fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"IKNOWLEDGE_BIN":            installDir,
		"IKNOWLEDGE_FORCE_SOURCE":   "1",
		"CODEX_HOME":                filepath.Join(home, "codex-not-installed"),
		"FAKE_GO_LOG":               goLog,
		"FAKE_CURL_LOG":             curlLog,
		"FAKE_MODULE_DIR":           moduleDir,
		"FAKE_RESOLVED_VERSION":     "v0.3.1",
		"FAKE_BINARY_VERSION":       "v0.3.1",
		"IKNOWLEDGE_TEST_UNRELATED": "kept",
	})
	if err := cmd.Run(); err != nil {
		t.Fatalf("install.sh: %v\n%s", err, output.String())
	}

	goCalls, err := os.ReadFile(goLog)
	if err != nil || !strings.Contains(string(goCalls), "install github.com/zdypro888/iknowledge/cmd/iknowledge@v0.3.1") {
		t.Fatalf("FORCE_SOURCE 未走 go install: %q err=%v\n%s", goCalls, err, output.String())
	}
	curlCalls, err := os.ReadFile(curlLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(curlCalls), "/releases/") || strings.Contains(string(curlCalls), "api.github.com") {
		t.Fatalf("FORCE_SOURCE 仍请求了 Release:\n%s", curlCalls)
	}
	skill, err := os.ReadFile(filepath.Join(home, ".claude", "skills", "kb-bootstrap", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(skill) != "FROZEN-MODULE-SKILL\n" {
		t.Fatalf("curl|sh 从 cwd 注入了 skill: %q", skill)
	}
}

func TestInstallScriptFailsClosedOnOldPATHShadow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("依赖 POSIX sh")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh unavailable")
	}
	repoRoot, _ := filepath.Abs(filepath.Join("..", ".."))
	installScript, err := os.ReadFile(filepath.Join(repoRoot, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	home, installDir := filepath.Join(tmp, "home"), filepath.Join(tmp, "new-bin")
	fakeBin, oldBin := filepath.Join(tmp, "fake-bin"), filepath.Join(tmp, "old-bin")
	moduleDir := filepath.Join(tmp, "module-cache")
	for _, dir := range []string{home, installDir, fakeBin, oldBin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	const oldTarget = "#!/bin/sh\nprintf 'old controlled target\\n'\n"
	writeExecutable(t, filepath.Join(installDir, "iknowledge"), oldTarget)
	oldSkillPath := filepath.Join(home, ".claude", "skills", "kb-bootstrap", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(oldSkillPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldSkillPath, []byte("OLD-SKILL\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(oldBin, "iknowledge"), "#!/bin/sh\nprintf 'old vulnerable build\\n'\n")
	prepareFakeFrozenModule(t, fakeBin, moduleDir)
	writeExecutable(t, filepath.Join(fakeBin, "curl"), `#!/bin/sh
out=''
while [ "$#" -gt 0 ]; do
  if [ "$1" = '-o' ]; then shift; out="$1"; fi
  shift
done
[ -n "$out" ] || exit 3
printf '%s\n' 'REMOTE-SKILL' > "$out"
`)
	cmd := exec.Command(sh, "-s")
	cmd.Stdin = bytes.NewReader(installScript)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	cmd.Env = envWith(map[string]string{
		"HOME": home, "IKNOWLEDGE_BIN": installDir, "IKNOWLEDGE_FORCE_SOURCE": "1",
		"CODEX_HOME":      filepath.Join(home, "no-codex"),
		"PATH":            oldBin + string(os.PathListSeparator) + installDir + string(os.PathListSeparator) + fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"FAKE_MODULE_DIR": moduleDir, "FAKE_RESOLVED_VERSION": "v0.3.1", "FAKE_BINARY_VERSION": "v0.3.1",
	})
	if err := cmd.Run(); err == nil {
		t.Fatalf("旧 PATH shadow 必须令安装 fail closed:\n%s", output.String())
	}
	if !strings.Contains(output.String(), "PATH 中的 iknowledge") {
		t.Fatalf("未给出 shadow 诊断:\n%s", output.String())
	}
	if got, err := os.ReadFile(filepath.Join(installDir, "iknowledge")); err != nil || string(got) != oldTarget {
		t.Fatalf("PATH preflight failure replaced controlled target: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(oldSkillPath); err != nil || string(got) != "OLD-SKILL\n" {
		t.Fatalf("PATH preflight failure replaced skill: %q err=%v", got, err)
	}
}

func TestInstallScriptStagesSkillsBeforeReplacingOldBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("依赖 POSIX sh")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh unavailable")
	}
	repoRoot, _ := filepath.Abs(filepath.Join("..", ".."))
	installScript, err := os.ReadFile(filepath.Join(repoRoot, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	home, installDir, fakeBin := filepath.Join(tmp, "home"), filepath.Join(tmp, "bin"), filepath.Join(tmp, "fake-bin")
	moduleDir := filepath.Join(tmp, "module-cache")
	for _, dir := range []string{home, installDir, fakeBin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(installDir, "iknowledge")
	const oldBinary = "#!/bin/sh\nprintf 'old-install-must-survive\\n'\n"
	writeExecutable(t, target, oldBinary)
	prepareFakeFrozenModule(t, fakeBin, moduleDir)
	if err := os.MkdirAll(filepath.Join(home, ".claude", "skills", "kb-bootstrap", "SKILL.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(fakeBin, "curl"), "#!/bin/sh\nexit 22\n")

	cmd := exec.Command(sh, "-s")
	cmd.Stdin = bytes.NewReader(installScript)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	cmd.Env = envWith(map[string]string{
		"HOME": home, "IKNOWLEDGE_BIN": installDir, "IKNOWLEDGE_FORCE_SOURCE": "1",
		"CODEX_HOME":      filepath.Join(home, "no-codex"),
		"PATH":            installDir + string(os.PathListSeparator) + fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"FAKE_MODULE_DIR": moduleDir, "FAKE_RESOLVED_VERSION": "v0.3.1", "FAKE_BINARY_VERSION": "v0.3.1",
	})
	if err := cmd.Run(); err == nil {
		t.Fatalf("skill staging failure must fail installation:\n%s", output.String())
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != oldBinary {
		t.Fatalf("old binary changed before skills staged: %q\n%s", got, output.String())
	}
	if !strings.Contains(output.String(), "skill 暂存失败；主二进制尚未替换") {
		t.Fatalf("missing transactional staging diagnosis:\n%s", output.String())
	}
}

func TestInstallScriptAggregatesAliasStopFailuresBeforeCommit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("依赖 POSIX ps/uid 语义")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh unavailable")
	}
	uidBytes, err := exec.Command("id", "-u").Output()
	if err != nil {
		t.Skip("id -u unavailable")
	}
	repoRoot, _ := filepath.Abs(filepath.Join("..", ".."))
	installScript, err := os.ReadFile(filepath.Join(repoRoot, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	installDir := filepath.Join(home, "custom-bin")
	fakeBin := filepath.Join(tmp, "fake-bin")
	moduleDir := filepath.Join(tmp, "module-cache")
	aliases := []string{filepath.Join(home, ".local", "bin", "iknowledge"), filepath.Join(home, "go", "bin", "iknowledge")}
	for _, dir := range []string{home, installDir, fakeBin, filepath.Dir(aliases[0]), filepath.Dir(aliases[1])} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	const oldBinary = "#!/bin/sh\nprintf 'iknowledge v0.3.1 go1.test\\n'\n"
	target := filepath.Join(installDir, "iknowledge")
	for _, path := range append([]string{target}, aliases...) {
		writeExecutable(t, path, oldBinary)
	}
	prepareFakeFrozenModule(t, fakeBin, moduleDir)
	writeExecutable(t, filepath.Join(fakeBin, "curl"), `#!/bin/sh
out=''
while [ "$#" -gt 0 ]; do
  if [ "$1" = '-o' ]; then shift; out="$1"; fi
  shift
done
[ -n "$out" ] || exit 3
printf '%s\n' 'REMOTE-SKILL' > "$out"
`)
	writeExecutable(t, filepath.Join(fakeBin, "ps"), `#!/bin/sh
if [ "$1" = '-axo' ]; then
  printf '%s %s %s serve --repo demo\n' "$FAKE_UID" 424242 "$STUCK_PATH"
  exit 0
fi
if [ "$1" = '-p' ]; then
  printf '%s %s serve --repo demo\n' "$FAKE_UID" "$STUCK_PATH"
  exit 0
fi
exit 2
`)
	writeExecutable(t, filepath.Join(fakeBin, "sleep"), "#!/bin/sh\nexit 0\n")

	cmd := exec.Command(sh, "-s")
	cmd.Stdin = bytes.NewReader(installScript)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	cmd.Env = envWith(map[string]string{
		"HOME": home, "IKNOWLEDGE_BIN": installDir, "IKNOWLEDGE_FORCE_SOURCE": "1",
		"CODEX_HOME": filepath.Join(home, "no-codex"),
		"PATH":       installDir + string(os.PathListSeparator) + fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"FAKE_UID":   strings.TrimSpace(string(uidBytes)), "STUCK_PATH": aliases[0],
		"FAKE_MODULE_DIR": moduleDir, "FAKE_RESOLVED_VERSION": "v0.3.1", "FAKE_BINARY_VERSION": "v0.3.1",
	})
	if err := cmd.Run(); err == nil {
		t.Fatalf("stuck controlled alias must fail before commit:\n%s", output.String())
	}
	for _, path := range append([]string{target}, aliases...) {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != oldBinary {
			t.Fatalf("pre-commit stop failure changed %s: %q err=%v\n%s", path, got, err, output.String())
		}
	}
}

func TestInstallScriptSourceResolutionFailureIsFailClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("依赖 POSIX sh")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh unavailable")
	}
	repoRoot, _ := filepath.Abs(filepath.Join("..", ".."))
	installScript, err := os.ReadFile(filepath.Join(repoRoot, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	home, installDir, fakeBin := filepath.Join(tmp, "home"), filepath.Join(tmp, "bin"), filepath.Join(tmp, "fake-bin")
	for _, dir := range []string{home, installDir, fakeBin, filepath.Join(home, ".claude", "skills", "kb-bootstrap")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(installDir, "iknowledge")
	const oldBinary = "#!/bin/sh\nprintf 'iknowledge v0.4.0 go1.test\\n'\n"
	writeExecutable(t, target, oldBinary)
	skill := filepath.Join(home, ".claude", "skills", "kb-bootstrap", "SKILL.md")
	if err := os.WriteFile(skill, []byte("OLD-SKILL\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	goLog := filepath.Join(tmp, "go.log")
	writeExecutable(t, filepath.Join(fakeBin, "go"), `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_GO_LOG"
exit 9
`)
	cmd := exec.Command(sh, "-s")
	cmd.Stdin = bytes.NewReader(installScript)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	cmd.Env = envWith(map[string]string{
		"HOME": home, "IKNOWLEDGE_BIN": installDir, "IKNOWLEDGE_FORCE_SOURCE": "1",
		"CODEX_HOME": filepath.Join(home, "no-codex"), "FAKE_GO_LOG": goLog,
		"PATH": installDir + string(os.PathListSeparator) + fakeBin + string(os.PathListSeparator) + "/usr/bin:/bin",
	})
	if err := cmd.Run(); err == nil {
		t.Fatalf("unresolved latest must fail closed:\n%s", output.String())
	}
	goCalls, _ := os.ReadFile(goLog)
	if strings.Contains(string(goCalls), "install ") || strings.Contains(output.String(), "skill 将取 main") {
		t.Fatalf("resolver failure crossed immutable boundary: calls=%q\n%s", goCalls, output.String())
	}
	if got, _ := os.ReadFile(target); string(got) != oldBinary {
		t.Fatalf("resolver failure replaced binary: %q", got)
	}
	if got, _ := os.ReadFile(skill); string(got) != "OLD-SKILL\n" {
		t.Fatalf("resolver failure replaced skill: %q", got)
	}
}

func TestInstallScriptRejectsAutomaticReleaseDowngrade(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("依赖 POSIX sh")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh unavailable")
	}
	repoRoot, _ := filepath.Abs(filepath.Join("..", ".."))
	installScript, err := os.ReadFile(filepath.Join(repoRoot, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	home, installDir, fakeBin := filepath.Join(tmp, "home"), filepath.Join(tmp, "bin"), filepath.Join(tmp, "fake-bin")
	for _, dir := range []string{home, installDir, fakeBin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(installDir, "iknowledge")
	const newer = "#!/bin/sh\nprintf 'iknowledge v9.1.0 go1.test\\n'\n"
	writeExecutable(t, target, newer)
	curlLog := filepath.Join(tmp, "curl.log")
	writeExecutable(t, filepath.Join(fakeBin, "curl"), `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_CURL_LOG"
printf '%s\n' '{"tag_name":"v9.0.0"}'
`)
	cmd := exec.Command(sh, "-s")
	cmd.Stdin = bytes.NewReader(installScript)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	cmd.Env = envWith(map[string]string{
		"HOME": home, "IKNOWLEDGE_BIN": installDir, "CODEX_HOME": filepath.Join(home, "no-codex"),
		"FAKE_CURL_LOG": curlLog,
		"PATH":          installDir + string(os.PathListSeparator) + fakeBin + string(os.PathListSeparator) + "/usr/bin:/bin",
	})
	if err := cmd.Run(); err == nil {
		t.Fatalf("automatic downgrade must fail:\n%s", output.String())
	}
	if !strings.Contains(output.String(), "拒绝自动降级") {
		t.Fatalf("missing downgrade diagnosis:\n%s", output.String())
	}
	calls, _ := os.ReadFile(curlLog)
	if strings.Contains(string(calls), "/releases/download/") {
		t.Fatalf("downgrade downloaded assets before refusal: %s", calls)
	}
	if got, _ := os.ReadFile(target); string(got) != newer {
		t.Fatalf("downgrade changed installed binary: %q", got)
	}
}

func TestInstallScriptRejectsAutomaticSourceFallbackDowngrade(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("依赖 POSIX sh")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh unavailable")
	}
	repoRoot, _ := filepath.Abs(filepath.Join("..", ".."))
	installScript, _ := os.ReadFile(filepath.Join(repoRoot, "install.sh"))
	tmp := t.TempDir()
	home, installDir, fakeBin, moduleDir := filepath.Join(tmp, "home"), filepath.Join(tmp, "bin"), filepath.Join(tmp, "fake-bin"), filepath.Join(tmp, "module")
	for _, dir := range []string{home, installDir, fakeBin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(installDir, "iknowledge")
	const newer = "#!/bin/sh\nprintf 'iknowledge v9.1.0 go1.test\\n'\n"
	writeExecutable(t, target, newer)
	prepareFakeFrozenModule(t, fakeBin, moduleDir)
	writeExecutable(t, filepath.Join(fakeBin, "curl"), "#!/bin/sh\nexit 22\n")
	goLog := filepath.Join(tmp, "go.log")
	cmd := exec.Command(sh, "-s")
	cmd.Stdin = bytes.NewReader(installScript)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	cmd.Env = envWith(map[string]string{
		"HOME": home, "IKNOWLEDGE_BIN": installDir, "CODEX_HOME": filepath.Join(home, "no-codex"),
		"FAKE_GO_LOG": goLog, "FAKE_MODULE_DIR": moduleDir, "FAKE_RESOLVED_VERSION": "v9.0.0", "FAKE_BINARY_VERSION": "v9.0.0",
		"PATH": installDir + string(os.PathListSeparator) + fakeBin + string(os.PathListSeparator) + "/usr/bin:/bin",
	})
	if err := cmd.Run(); err == nil {
		t.Fatalf("default source fallback downgrade must fail:\n%s", output.String())
	}
	goCalls, _ := os.ReadFile(goLog)
	if strings.Contains(string(goCalls), "install ") || !strings.Contains(output.String(), "拒绝自动降级") {
		t.Fatalf("source fallback crossed downgrade guard: calls=%q\n%s", goCalls, output.String())
	}
	if got, _ := os.ReadFile(target); string(got) != newer {
		t.Fatalf("source fallback downgrade changed binary: %q", got)
	}
}

func TestInstallUninstallScriptGuards(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	install, err := os.ReadFile(filepath.Join(repoRoot, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	uninstall, err := os.ReadFile(filepath.Join(repoRoot, "uninstall.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`bin_name="iknowledge.exe"`,
		`缺 sha256sums.txt，拒绝安装未校验 Release 资产`,
		`找不到 sha256sum/shasum，拒绝安装未校验 Release 资产`,
		`install.sh|*/install.sh)`,
		`if ! cmp -s "$resolved_bin" "$BIN"`,
		`PATH 中的 iknowledge`,
		`members="$(tar tzf`,
		`mktemp "$install_dir/.iknowledge-install.XXXXXX"`,
		`mktemp "$dst/.SKILL.md.XXXXXX"`,
		`stage_deployment_aliases()`,
		`stop_manifest_serves()`,
		`if [ ! -s "$stage_skill" ]`,
		`BINARY_EXPECTED_VERSION="$latest_tag"`,
		`version_output="$("$BINARY_SOURCE" version)"`,
		`target_will_resolve_first()`,
		`checksums 中缺 ${skill_asset}，拒绝安装`,
		`is_formal_semver()`,
		`readlink "/proc/$executable_pid/exe"`,
		`lsof_command=/usr/sbin/lsof`,
		`go mod download "github.com/${REPO}@${resolved_ref}"`,
		`go list -m -f '{{.Dir}}'`,
		`SOURCE_DOWNGRADE_EXPLICIT`,
	} {
		if !bytes.Contains(install, []byte(want)) {
			t.Errorf("install.sh 缺安全护栏 %q", want)
		}
	}
	if bytes.Contains(install, []byte(`SKILL_REF="main"`)) || bytes.Contains(install, []byte(`go install "github.com/${REPO}/cmd/iknowledge@latest"`)) {
		t.Error("source fallback 不得混用 main 或未冻结的 @latest")
	}
	forceAt, binaryAt := bytes.Index(install, []byte(`if [ -n "${IKNOWLEDGE_FORCE_SOURCE:-}" ]`)), bytes.Index(install, []byte(`elif install_binary`))
	if forceAt < 0 || binaryAt < 0 || forceAt > binaryAt {
		t.Errorf("IKNOWLEDGE_FORCE_SOURCE 必须先于 install_binary 分支: force=%d binary=%d", forceAt, binaryAt)
	}
	stageAt, commitAt := bytes.Index(install, []byte(`CLAUDE_SKILL_STAGE="$(stage_skill`)), bytes.Index(install, []byte(`install_staged_binary "$BINARY_SOURCE"`))
	if stageAt < 0 || commitAt < 0 || stageAt > commitAt {
		t.Errorf("skill 必须在 binary 替换前暂存: stage=%d commit=%d", stageAt, commitAt)
	}
	for _, want := range []string{
		`installed_bin="$install_dir/$bin_name"`,
		`cmp -s "$candidate" "$installed_bin"`,
		`printf 'target\t%s\n' "$installed_bin"`,
		`ps -axo uid=,pid=,args=`,
		`symlink_points_to_installed_bin()`,
		`if ! stop_manifest_serves; then`,
		`rm -f "$uninstall_candidate"`,
		`taskkill.exe /F /IM iknowledge.exe`,
		`状态目录必须是绝对路径`,
		`if [ -L "$state_dir" ]`,
		`if [ -L "$state_dir/repos" ]`,
		`if [ -L "$repo_state" ]`,
		`rm -f "$repo_state/auth-token" "$repo_state/local-identity" "$repo_state/scout-trust-v1" "$repo_state/semantic-config-v1.json"`,
		`保留待恢复事务状态`,
		`readlink "/proc/$uninstall_executable_pid/exe"`,
		`uninstall_lsof_command=/usr/sbin/lsof`,
	} {
		if !bytes.Contains(uninstall, []byte(want)) {
			t.Errorf("uninstall.sh 缺卸载路径 %q", want)
		}
	}
}

func TestInstallScriptAcceptsPhysicalPATHForSymlinkInstallDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("depends on POSIX symlinks")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh unavailable")
	}
	repoRoot, _ := filepath.Abs(filepath.Join("..", ".."))
	installScript, err := os.ReadFile(filepath.Join(repoRoot, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	realInstall := filepath.Join(home, "real-bin")
	linkInstall := filepath.Join(home, "linked-bin")
	fakeBin := filepath.Join(tmp, "fake-bin")
	moduleDir := filepath.Join(tmp, "module-cache")
	for _, dir := range []string{home, realInstall, fakeBin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(realInstall, linkInstall); err != nil {
		t.Fatal(err)
	}
	prepareFakeFrozenModule(t, fakeBin, moduleDir)
	writeExecutable(t, filepath.Join(fakeBin, "curl"), `#!/bin/sh
out=''
while [ "$#" -gt 0 ]; do
  if [ "$1" = '-o' ]; then shift; out="$1"; fi
  shift
done
[ -n "$out" ] || exit 3
printf '%s\n' 'REMOTE-SKILL' > "$out"
`)
	cmd := exec.Command(sh, "-s")
	cmd.Stdin = bytes.NewReader(installScript)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	cmd.Env = envWith(map[string]string{
		"HOME": home, "IKNOWLEDGE_BIN": linkInstall, "IKNOWLEDGE_FORCE_SOURCE": "1",
		"CODEX_HOME":      filepath.Join(home, "no-codex"),
		"PATH":            linkInstall + string(os.PathListSeparator) + fakeBin + string(os.PathListSeparator) + "/usr/bin:/bin",
		"FAKE_MODULE_DIR": moduleDir, "FAKE_RESOLVED_VERSION": "v0.3.1", "FAKE_BINARY_VERSION": "v0.3.1",
	})
	if err := cmd.Run(); err != nil {
		t.Fatalf("symlink install dir rejected: %v\n%s", err, output.String())
	}
	if _, err := os.Stat(filepath.Join(realInstall, "iknowledge")); err != nil {
		t.Fatalf("physical target missing: %v\n%s", err, output.String())
	}
}

func TestUninstallScriptRecognizesBareServeByExecutableIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("依赖 POSIX sh")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh unavailable")
	}
	uidOut, err := exec.Command("id", "-u").Output()
	if err != nil {
		t.Skip("id -u unavailable")
	}
	repoRoot, _ := filepath.Abs(filepath.Join("..", ".."))
	uninstallScript, _ := os.ReadFile(filepath.Join(repoRoot, "uninstall.sh"))
	tmp := t.TempDir()
	home, installDir, fakeBin := filepath.Join(tmp, "home"), filepath.Join(tmp, "controlled-bin"), filepath.Join(tmp, "fake-bin")
	skillDir := filepath.Join(home, ".claude", "skills", "kb-bootstrap")
	for _, dir := range []string{installDir, fakeBin, skillDir, filepath.Join(tmp, "foreign-bin")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(installDir, "iknowledge")
	foreign := filepath.Join(tmp, "foreign-bin", "iknowledge")
	writeExecutable(t, target, "#!/bin/sh\nexit 0\n")
	writeExecutable(t, foreign, "#!/bin/sh\nexit 0\n")
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("REMOVE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	counter := filepath.Join(tmp, "ps-count")
	if err := os.WriteFile(counter, []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(fakeBin, "uname"), "#!/bin/sh\nprintf 'Linux\\n'\n")
	writeExecutable(t, filepath.Join(fakeBin, "ps"), `#!/bin/sh
count=$(sed -n '1p' "$PS_COUNT")
if [ "$1" = '-axo' ]; then
  if [ "$count" -lt 2 ]; then printf '%s %s iknowledge serve --repo controlled\n' "$FAKE_UID" "$CONTROLLED_PID"; fi
  printf '%s %s iknowledge serve --repo foreign\n' "$FAKE_UID" "$FOREIGN_PID"
  exit 0
fi
if [ "$1" = '-p' ]; then
  if [ "$2" = "$CONTROLLED_PID" ]; then
    count=$((count + 1)); printf '%s\n' "$count" > "$PS_COUNT"
    printf '%s iknowledge serve --repo controlled\n' "$FAKE_UID"
    exit 0
  fi
  if [ "$2" = "$FOREIGN_PID" ]; then
    printf '%s iknowledge serve --repo foreign\n' "$FAKE_UID"
    exit 0
  fi
fi
exit 2
`)
	writeExecutable(t, filepath.Join(fakeBin, "readlink"), `#!/bin/sh
case "$1" in
  "/proc/$CONTROLLED_PID/exe") printf '%s\n' "$CONTROLLED_PATH" ;;
  "/proc/$FOREIGN_PID/exe") printf '%s\n' "$FOREIGN_PATH" ;;
  *) exit 1 ;;
esac
`)
	writeExecutable(t, filepath.Join(fakeBin, "sleep"), "#!/bin/sh\nexit 0\n")
	cmd := exec.Command(sh, "-s")
	cmd.Stdin = bytes.NewReader(uninstallScript)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	cmd.Env = envWith(map[string]string{
		"HOME": home, "IKNOWLEDGE_BIN": installDir, "CODEX_HOME": filepath.Join(home, "no-codex"),
		"IKNOWLEDGE_STATE_HOME": filepath.Join(home, "state"), "FAKE_UID": strings.TrimSpace(string(uidOut)),
		"CONTROLLED_PID": "99999999", "FOREIGN_PID": "99999998", "CONTROLLED_PATH": target, "FOREIGN_PATH": foreign, "PS_COUNT": counter,
		"PATH": fakeBin + string(os.PathListSeparator) + "/usr/bin:/bin",
	})
	if err := cmd.Run(); err != nil {
		t.Fatalf("bare controlled serve identity was not handled: %v\n%s", err, output.String())
	}
	if !strings.Contains(output.String(), "99999999") || strings.Contains(output.String(), "停止由受控安装路径启动的 serve: 99999998") {
		t.Fatalf("identity selection mixed controlled and foreign installs:\n%s", output.String())
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("controlled target survived uninstall: %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("foreign install was changed: %v", err)
	}
}

func TestUninstallScriptFailsClosedWhenExecutableIdentityUnavailable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("depends on POSIX process table")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh unavailable")
	}
	uidOut, err := exec.Command("id", "-u").Output()
	if err != nil {
		t.Skip("id -u unavailable")
	}
	repoRoot, _ := filepath.Abs(filepath.Join("..", ".."))
	uninstallScript, err := os.ReadFile(filepath.Join(repoRoot, "uninstall.sh"))
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	home, installDir, fakeBin := filepath.Join(tmp, "home"), filepath.Join(tmp, "bin"), filepath.Join(tmp, "fake-bin")
	for _, dir := range []string{home, installDir, fakeBin, filepath.Join(home, ".claude", "skills", "kb-bootstrap")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(installDir, "iknowledge")
	writeExecutable(t, target, "#!/bin/sh\nprintf 'old\\n'\n")
	skill := filepath.Join(home, ".claude", "skills", "kb-bootstrap", "SKILL.md")
	if err := os.WriteFile(skill, []byte("KEEP\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(fakeBin, "uname"), "#!/bin/sh\nprintf 'Linux\\n'\n")
	writeExecutable(t, filepath.Join(fakeBin, "ps"), `#!/bin/sh
if [ "$1" = '-axo' ]; then printf '%s %s iknowledge serve --repo demo\n' "$FAKE_UID" 424242; exit 0; fi
if [ "$1" = '-p' ]; then printf '%s iknowledge serve --repo demo\n' "$FAKE_UID"; exit 0; fi
exit 2
`)
	writeExecutable(t, filepath.Join(fakeBin, "readlink"), "#!/bin/sh\nexit 2\n")
	cmd := exec.Command(sh, "-s")
	cmd.Stdin = bytes.NewReader(uninstallScript)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	cmd.Env = envWith(map[string]string{
		"HOME": home, "IKNOWLEDGE_BIN": installDir, "CODEX_HOME": filepath.Join(home, "no-codex"),
		"IKNOWLEDGE_STATE_HOME": filepath.Join(home, "state"),
		"FAKE_UID":              strings.TrimSpace(string(uidOut)),
		"PATH":                  fakeBin + string(os.PathListSeparator) + "/usr/bin:/bin",
	})
	if err := cmd.Run(); err == nil {
		t.Fatalf("unprovable executable identity must fail closed:\n%s", output.String())
	}
	if !strings.Contains(output.String(), "无法证明其 executable identity") {
		t.Fatalf("missing identity diagnosis:\n%s", output.String())
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("binary changed before process preflight: %v", err)
	}
	if got, err := os.ReadFile(skill); err != nil || string(got) != "KEEP\n" {
		t.Fatalf("skill changed before process preflight: %q err=%v", got, err)
	}
}

func TestUninstallScriptIgnoresOtherUIDAndRemovesRelativeAlias(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("depends on POSIX UID and symlink semantics")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh unavailable")
	}
	uidOut, err := exec.Command("id", "-u").Output()
	if err != nil {
		t.Skip("id -u unavailable")
	}
	repoRoot, _ := filepath.Abs(filepath.Join("..", ".."))
	uninstallScript, err := os.ReadFile(filepath.Join(repoRoot, "uninstall.sh"))
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	installDir := filepath.Join(home, "custom-bin")
	aliasDir := filepath.Join(home, ".local", "bin")
	fakeBin := filepath.Join(tmp, "fake-bin")
	for _, dir := range []string{installDir, aliasDir, fakeBin, filepath.Join(home, ".claude", "skills", "kb-bootstrap")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(installDir, "iknowledge")
	alias := filepath.Join(aliasDir, "iknowledge")
	writeExecutable(t, target, "#!/bin/sh\nprintf 'old\\n'\n")
	if err := os.Symlink(filepath.Join("..", "..", "custom-bin", "iknowledge"), alias); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "skills", "kb-bootstrap", "SKILL.md"), []byte("REMOVE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(fakeBin, "ps"), `#!/bin/sh
if [ "$1" = '-axo' ]; then
  printf '%s %s %s serve --repo foreign\n' "$OTHER_UID" 424242 "$TARGET_PATH"
  exit 0
fi
exit 1
`)
	writeExecutable(t, filepath.Join(fakeBin, "sleep"), "#!/bin/sh\nexit 0\n")
	otherUID := strings.TrimSpace(string(uidOut)) + "1"
	cmd := exec.Command(sh, "-s")
	cmd.Stdin = bytes.NewReader(uninstallScript)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	cmd.Env = envWith(map[string]string{
		"HOME": home, "IKNOWLEDGE_BIN": installDir, "CODEX_HOME": filepath.Join(home, "no-codex"),
		"IKNOWLEDGE_STATE_HOME": filepath.Join(home, "state"), "OTHER_UID": otherUID, "TARGET_PATH": target,
		"PATH": fakeBin + string(os.PathListSeparator) + "/usr/bin:/bin",
	})
	if err := cmd.Run(); err != nil {
		t.Fatalf("foreign UID or relative alias broke uninstall: %v\n%s", err, output.String())
	}
	for _, path := range []string{target, alias, filepath.Join(home, ".claude", "skills", "kb-bootstrap")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("controlled path survived uninstall: %s err=%v\n%s", path, err, output.String())
		}
	}
}

func TestUninstallScriptPostDeleteRespawnPreservesSkills(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("depends on POSIX process table")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh unavailable")
	}
	uidOut, err := exec.Command("id", "-u").Output()
	if err != nil {
		t.Skip("id -u unavailable")
	}
	repoRoot, _ := filepath.Abs(filepath.Join("..", ".."))
	uninstallScript, err := os.ReadFile(filepath.Join(repoRoot, "uninstall.sh"))
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	home, installDir, fakeBin := filepath.Join(tmp, "home"), filepath.Join(tmp, "bin"), filepath.Join(tmp, "fake-bin")
	skillDir := filepath.Join(home, ".claude", "skills", "kb-bootstrap")
	for _, dir := range []string{installDir, fakeBin, skillDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(installDir, "iknowledge")
	writeExecutable(t, target, "#!/bin/sh\nprintf 'old\\n'\n")
	skill := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(skill, []byte("KEEP-AFTER-RESPAWN\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(fakeBin, "ps"), `#!/bin/sh
if [ "$1" = '-axo' ]; then
  if [ ! -e "$STUCK_PATH" ]; then printf '%s %s %s serve --repo respawn\n' "$FAKE_UID" 424242 "$STUCK_PATH"; fi
  exit 0
fi
if [ "$1" = '-p' ]; then
  printf '%s %s serve --repo respawn\n' "$FAKE_UID" "$STUCK_PATH"
  exit 0
fi
exit 2
`)
	writeExecutable(t, filepath.Join(fakeBin, "sleep"), "#!/bin/sh\nexit 0\n")
	cmd := exec.Command(sh, "-s")
	cmd.Stdin = bytes.NewReader(uninstallScript)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	cmd.Env = envWith(map[string]string{
		"HOME": home, "IKNOWLEDGE_BIN": installDir, "CODEX_HOME": filepath.Join(home, "no-codex"),
		"IKNOWLEDGE_STATE_HOME": filepath.Join(home, "state"), "FAKE_UID": strings.TrimSpace(string(uidOut)), "STUCK_PATH": target,
		"PATH": fakeBin + string(os.PathListSeparator) + "/usr/bin:/bin",
	})
	if err := cmd.Run(); err == nil {
		t.Fatalf("post-delete KeepAlive respawn must fail loudly:\n%s", output.String())
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("test did not reach post-delete sweep: err=%v\n%s", err, output.String())
	}
	if got, err := os.ReadFile(skill); err != nil || string(got) != "KEEP-AFTER-RESPAWN\n" {
		t.Fatalf("skill removed before post-delete convergence: %q err=%v\n%s", got, err, output.String())
	}
}

// release workflow 在仓库内写 dist/。若它未被 git 忽略，Go 在构建输出创建后
// 会把 vcs.modified 烙进正式二进制，令干净 tag 被误报为 +dirty。
func TestReleaseArtifactsDoNotDirtyVersionMetadata(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	ignore, err := os.ReadFile(filepath.Join(repoRoot, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, line := range strings.Split(string(ignore), "\n") {
		if strings.TrimSpace(line) == "/dist/" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("release 输出 dist/ 必须被 git 忽略，否则正式二进制会误报 +dirty")
	}
}
