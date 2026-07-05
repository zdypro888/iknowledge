package engine

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zdypro888/iknowledge/internal/parser"
)

// countingParser 记录 Parse 调用次数,内容含 BAD 即报解析失败。
// 模拟子进程型解析器(Python)——每次 Parse 都有真实成本。
type countingParser struct{ calls *int }

func (countingParser) Language() string     { return "cnt" }
func (countingParser) Extensions() []string { return []string{".cnt"} }
func (c countingParser) Parse(path string, src []byte) ([]parser.Symbol, error) {
	*c.calls++
	if bytes.Contains(src, []byte("BAD")) {
		return nil, fmt.Errorf("bad content")
	}
	return nil, nil
}

// parseFailed 指纹缓存(多语言加固):TTL 过期重扫时,指纹没变的文件不重解析
// (子进程解析器稳态零成本);文件真变了计数照常刷新。
func TestParseFailedFingerprintCache(t *testing.T) {
	e, repo := newRepo(t, map[string]string{
		"a.cnt": "ok\n",
		"b.cnt": "BAD\n",
	})
	calls := 0
	e.Reg.Register(countingParser{calls: &calls})
	base := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

	if n := e.parseFailedCached(); n != 1 {
		t.Fatalf("首扫 parseFailed=%d,想要 1", n)
	}
	if calls != 2 {
		t.Fatalf("首扫应解析 2 个文件,实际 %d", calls)
	}

	// TTL 内:纯缓存命中,不扫不解析。
	if n := e.parseFailedCached(); n != 1 || calls != 2 {
		t.Fatalf("TTL 内应零成本(n=%d calls=%d)", n, calls)
	}

	// TTL 过期 + 文件没变:重扫只 stat,不重解析(指纹缓存的意义所在)。
	e.now = func() time.Time { return base.Add(2 * time.Minute) }
	if n := e.parseFailedCached(); n != 1 {
		t.Fatalf("过期重扫 parseFailed=%d,想要 1", n)
	}
	if calls != 2 {
		t.Fatalf("文件没变不应重解析,实际新增 %d 次", calls-2)
	}

	// 文件修好(尺寸变了):只重解析这一个,计数刷新为 0。
	if err := os.WriteFile(filepath.Join(repo, "b.cnt"), []byte("okok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.now = func() time.Time { return base.Add(4 * time.Minute) }
	if n := e.parseFailedCached(); n != 0 {
		t.Fatalf("修好后 parseFailed=%d,想要 0", n)
	}
	if calls != 3 {
		t.Fatalf("只应重解析改动的 1 个文件(总 calls=%d,想要 3)", calls)
	}
}
