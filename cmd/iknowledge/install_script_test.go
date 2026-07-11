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
	writeExecutable(t, filepath.Join(fakeBin, "go"), `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_GO_LOG"
if [ "$1" != "install" ]; then exit 2; fi
printf '%s\n' '#!/bin/sh' 'exit 0' > "$GOBIN/iknowledge"
chmod +x "$GOBIN/iknowledge"
`)
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
		"IKNOWLEDGE_TEST_UNRELATED": "kept",
	})
	if err := cmd.Run(); err != nil {
		t.Fatalf("install.sh: %v\n%s", err, output.String())
	}

	goCalls, err := os.ReadFile(goLog)
	if err != nil || !strings.Contains(string(goCalls), "install github.com/zdypro888/iknowledge/cmd/iknowledge@latest") {
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
	if string(skill) != "REMOTE-TRUSTED-SKILL\n" {
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
	for _, dir := range []string{home, installDir, fakeBin, oldBin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable(t, filepath.Join(oldBin, "iknowledge"), "#!/bin/sh\nprintf 'old vulnerable build\\n'\n")
	writeExecutable(t, filepath.Join(fakeBin, "go"), `#!/bin/sh
printf '%s\n' '#!/bin/sh' 'exit 0' > "$GOBIN/iknowledge"
chmod +x "$GOBIN/iknowledge"
`)
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
		"CODEX_HOME": filepath.Join(home, "no-codex"),
		"PATH":       oldBin + string(os.PathListSeparator) + installDir + string(os.PathListSeparator) + fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
	})
	if err := cmd.Run(); err == nil {
		t.Fatalf("旧 PATH shadow 必须令安装 fail closed:\n%s", output.String())
	}
	if !strings.Contains(output.String(), "PATH 中的 iknowledge") {
		t.Fatalf("未给出 shadow 诊断:\n%s", output.String())
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
		`缺 sha256sums.txt，拒绝安装未校验二进制`,
		`找不到 sha256sum/shasum，拒绝安装未校验二进制`,
		`install.sh|*/install.sh)`,
		`if ! cmp -s "$resolved_bin" "$BIN"`,
		`PATH 中的 iknowledge`,
		`members="$(tar tzf`,
		`mktemp "$install_dir/.iknowledge-install.XXXXXX"`,
		`mktemp "$dst/.SKILL.md.XXXXXX"`,
	} {
		if !bytes.Contains(install, []byte(want)) {
			t.Errorf("install.sh 缺安全护栏 %q", want)
		}
	}
	forceAt, binaryAt := bytes.Index(install, []byte(`if [ -n "${IKNOWLEDGE_FORCE_SOURCE:-}" ]`)), bytes.Index(install, []byte(`elif install_binary`))
	if forceAt < 0 || binaryAt < 0 || forceAt > binaryAt {
		t.Errorf("IKNOWLEDGE_FORCE_SOURCE 必须先于 install_binary 分支: force=%d binary=%d", forceAt, binaryAt)
	}
	for _, want := range []string{
		`installed_bin="$install_dir/$bin_name"`,
		`cmp -s "$candidate" "$installed_bin"`,
		`rm -f "$installed_bin"`,
		`taskkill.exe /F /IM iknowledge.exe`,
		`状态目录必须是绝对路径`,
		`rm -f "$repo_state/auth-token" "$repo_state/local-identity" "$repo_state/scout-trust-v1"`,
		`保留待恢复事务状态`,
	} {
		if !bytes.Contains(uninstall, []byte(want)) {
			t.Errorf("uninstall.sh 缺卸载路径 %q", want)
		}
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
