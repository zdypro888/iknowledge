package engine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestImportRestrictsBundleEntries(t *testing.T) {
	e, repo := initEngine(t, map[string]string{"a.go": "package a\n\nfunc F() {}\n"})
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	add := func(name, body string) {
		t.Helper()
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	add("tree/imported.go.yaml", "schema: 1\nnodes: []\n")
	add("journal/imported.jsonl", "{}\n")
	add("MANIFEST.json", "{}")
	add("local/token", "evil")
	add("wip/task.yaml", "evil")
	add("../escape", "evil")
	add("/abs", "evil")
	add("tree/../local/token", "evil")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	n, err := e.Import(bytes.NewReader(buf.Bytes()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("导入文件数=%d, want 2", n)
	}
	for _, rel := range []string{
		".knowledge/tree/imported.go.yaml",
		".knowledge/journal/imported.jsonl",
	} {
		if _, err := os.Stat(filepath.Join(repo, rel)); err != nil {
			t.Fatalf("合法条目未写入 %s: %v", rel, err)
		}
	}
	for _, rel := range []string{
		".knowledge/local/token",
		".knowledge/wip/task.yaml",
		"escape",
	} {
		if _, err := os.Stat(filepath.Join(repo, rel)); !os.IsNotExist(err) {
			t.Fatalf("非法条目不应写入 %s: %v", rel, err)
		}
	}
}

func TestImportDryRunAndBackupReport(t *testing.T) {
	e, repo := initEngine(t, map[string]string{"a.go": "package a\n\nfunc F() {}\n"})
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := "schema: 1\nnodes: []\n"
	if err := tw.WriteHeader(&tar.Header{Name: "tree/imported.go.yaml", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	rep, err := e.ImportWithOptions(bytes.NewReader(buf.Bytes()), ImportOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Imported != 1 || rep.Scanned != 1 {
		t.Fatalf("dry-run report=%+v", rep)
	}
	if _, err := os.Stat(filepath.Join(repo, ".knowledge/tree/imported.go.yaml")); !os.IsNotExist(err) {
		t.Fatalf("dry-run 不应写入文件: %v", err)
	}

	rep, err = e.ImportWithOptions(bytes.NewReader(buf.Bytes()), ImportOptions{Backup: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.BackupPath == "" {
		t.Fatalf("backup path empty: %+v", rep)
	}
	if _, err := os.Stat(filepath.Join(repo, rep.BackupPath)); err != nil {
		t.Fatalf("备份未写入: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".knowledge/tree/imported.go.yaml")); err != nil {
		t.Fatalf("导入文件未写入: %v", err)
	}
}
