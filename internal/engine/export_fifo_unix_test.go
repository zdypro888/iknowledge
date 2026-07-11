//go:build unix

package engine

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestImportPreflightRejectsExistingFIFOWithoutBlocking(t *testing.T) {
	e, repo := initEngine(t, map[string]string{"a.go": "package a\n"})
	target := filepath.Join(repo, ".knowledge", "tree", "fifo.go.yaml")
	if err := syscall.Mkfifo(target, 0o600); err != nil {
		t.Fatal(err)
	}
	bundle := makeTestBundle(t, map[string]string{
		"tree/fifo.go.yaml": "schema: 1\nnodes: []\n",
	})
	done := make(chan error, 1)
	go func() {
		_, err := e.Import(bytes.NewReader(bundle), nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("既存 FIFO 必须在读取前拒绝")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("import 被既存 FIFO 阻塞")
	}
	info, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("FIFO 被导入改写: mode=%v", info.Mode())
	}
}
