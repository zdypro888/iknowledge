package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zdypro888/iknowledge/internal/buildinfo"
	"time"

	"github.com/zdypro888/iknowledge/internal/engine"
)

func TestCLIRejectsUnexpectedPositionalArgs(t *testing.T) {
	for _, args := range [][]string{
		{"serve", "unexpected", "--auth"},
		{"import", "unexpected", "--dry-run"},
		{"export", "unexpected", "-o", "x"},
		{"init", "unexpected", "--force"},
		{"status", "unexpected", "--prompt"},
		{"doctor", "unexpected", "--strict"},
		{"maintain", "unexpected", "--plan"},
		{"setup", "unexpected"},
		{"trust-scout", "unexpected"},
		{"stdio", "unexpected"},
		{"version", "unexpected"},
	} {
		if code := run(args); code != 2 {
			t.Errorf("run(%v)=%d, want usage exit 2", args, code)
		}
	}
	var out bytes.Buffer
	if code := runHook([]string{"unexpected", "--repo", "x"}, strings.NewReader(`{}`), &out); code != 0 || out.Len() != 0 {
		t.Fatalf("hook 非法 positional 也必须静默 exit 0:code=%d out=%q", code, out.String())
	}
}

func TestRunExportAtomicAndRejectsKnowledgeOutput(t *testing.T) {
	repo := setupGitRepo(t)
	e, _ := initRepo(t, repo, engine.InitOptions{})
	_ = e
	outDir := t.TempDir()
	out := filepath.Join(outDir, "backup.kbundle")
	if err := os.WriteFile(out, []byte("OLD"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := runExport([]string{"--repo", repo, "-o", out}); code != 0 {
		t.Fatalf("runExport success code=%d", code)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(data, []byte("OLD")) || len(data) < 32 {
		t.Fatalf("导出未原子替换完整 bundle:len=%d", len(data))
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(out)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("导出 bundle 默认权限=%o, want 600", info.Mode().Perm())
		}
	}
	if leftovers, _ := filepath.Glob(filepath.Join(outDir, ".backup.kbundle-*.tmp")); len(leftovers) != 0 {
		t.Fatalf("成功导出遗留临时文件:%v", leftovers)
	}

	inside := filepath.Join(repo, ".knowledge", "forbidden.kbundle")
	if code := runExport([]string{"--repo", repo, "-o", inside}); code == 0 {
		t.Fatal("export 输出在源 .knowledge 内必须拒绝")
	}
	if _, err := os.Stat(inside); !os.IsNotExist(err) {
		t.Fatalf("被拒输出仍落盘:%v", err)
	}

	if runtime.GOOS != "windows" {
		secret := filepath.Join(t.TempDir(), "secret.yaml")
		if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(repo, ".knowledge", "flows", "bad.yaml")
		if err := os.Symlink(secret, link); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(out, []byte("PRESERVE"), 0o644); err != nil {
			t.Fatal(err)
		}
		if code := runExport([]string{"--repo", repo, "-o", out}); code == 0 {
			t.Fatal("源库 symlink 应使导出失败")
		}
		after, err := os.ReadFile(out)
		if err != nil || string(after) != "PRESERVE" {
			t.Fatalf("导出失败未保留旧输出:data=%q err=%v", after, err)
		}
		if leftovers, _ := filepath.Glob(filepath.Join(outDir, ".backup.kbundle-*.tmp")); len(leftovers) != 0 {
			t.Fatalf("失败导出遗留临时文件:%v", leftovers)
		}
	}
}

func TestRunImportRejectsCaseFoldOverwriteBlackBox(t *testing.T) {
	repo := setupGitRepo(t)
	// 单独的大写源文件产生 tree/Case.go.yaml。
	caseSource := filepath.Join(repo, "Case.go")
	if err := os.WriteFile(caseSource, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	initRepo(t, repo, engine.InitOptions{})
	target := filepath.Join(repo, ".knowledge", "tree", "Case.go.yaml")
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	bundle := makeCLITestBundle(t, map[string]string{"tree/case.go.yaml": "schema: 1\nnodes: []\n"})
	input := filepath.Join(t.TempDir(), "case.kbundle")
	if err := os.WriteFile(input, bundle, 0o644); err != nil {
		t.Fatal(err)
	}
	if code := runImport([]string{"--repo", repo, "-i", input, "--force"}); code == 0 {
		t.Fatal("CLI --force 不得绕过大小写便携路径冲突")
	}
	after, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("大小写冲突导入覆盖了原文件:err=%v", err)
	}
	entries, err := os.ReadDir(filepath.Join(repo, ".knowledge", "tree"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == "case.go.yaml" {
			t.Fatal("大小写冲突导入产生了新文件")
		}
	}
}

func TestVersionPrefersReleaseInjectedBuildVersion(t *testing.T) {
	old := buildinfo.Version
	buildinfo.Version = "v9.8.7-test"
	t.Cleanup(func() { buildinfo.Version = old })
	got := versionText()
	if !strings.Contains(got, "iknowledge v9.8.7-test") || strings.Contains(got, "(devel)") {
		t.Fatalf("versionText=%q", got)
	}
}

func makeCLITestBundle(t *testing.T, files map[string]string) []byte {
	t.Helper()
	filesWithManifest := make(map[string]string, len(files)+1)
	for name, body := range files {
		filesWithManifest[name] = body
	}
	filesWithManifest["MANIFEST.json"] = `{"schema":1,"exported_at":"2026-07-11T00:00:00Z","repo":"/source/repo"}`
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range filesWithManifest {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)), ModTime: time.Unix(0, 0)}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
