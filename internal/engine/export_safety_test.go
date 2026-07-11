package engine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/zdypro888/iknowledge/internal/model"
	"github.com/zdypro888/iknowledge/internal/store"
)

func TestImportProjectRemapAndFinalValidation(t *testing.T) {
	e, repo := initEngine(t, map[string]string{"target.go": "package target\n"})
	project := `schema: 1
nodes:
  - id: .
    level: project
    anchor: {file: .}
    status: fresh
    since: 2026-01-01T00:00:00Z
    entries:
      - id: e_project
        kind: contract
        text: project contract
        confidence: inferred
        based_on: [old/a.go#F#e_base]
`
	tree := `schema: 1
nodes:
  - id: old/a.go#F
    level: function
    anchor: {file: old/a.go, symbol: F}
    status: fresh
    since: 2026-01-01T00:00:00Z
    entries:
      - {id: e_base, kind: contract, text: base, confidence: inferred}
`
	rep, err := e.ImportWithOptions(bytes.NewReader(makeTestBundle(t, map[string]string{
		"project.yaml":       project,
		"tree/old/a.go.yaml": tree,
	})), ImportOptions{PathRemap: map[string]string{"old": "new"}, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Imported != 2 {
		t.Fatalf("report=%+v", rep)
	}
	projectData, err := os.ReadFile(filepath.Join(repo, ".knowledge/project.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(projectData), "new/a.go#F#e_base") {
		t.Fatalf("project entry reference 未语义 remap:\n%s", projectData)
	}
	if _, err := os.Stat(filepath.Join(repo, ".knowledge/tree/new/a.go.yaml")); err != nil {
		t.Fatalf("tree output 未 remap:%v", err)
	}

	badProject := strings.Replace(project, "id: .", "id: wrong.go", 1)
	if _, err := e.ImportWithOptions(bytes.NewReader(makeTestBundle(t, map[string]string{"project.yaml": badProject})), ImportOptions{Force: true}); err == nil {
		t.Fatal("project.yaml 必须只包含项目节点")
	}
}

func TestExportKeepsNestedSourceDirectoriesNamedLocalAndWIP(t *testing.T) {
	e, _ := initEngine(t, map[string]string{
		"internal/local/a.go": "package local\n\nfunc A() {}\n",
		"pkg/wip/b.go":        "package wip\n\nfunc B() {}\n",
	})
	var bundle bytes.Buffer
	if err := e.Export(&bundle); err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(bundle.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	seen := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		seen[hdr.Name] = true
	}
	for _, name := range []string{"tree/internal/local/a.go.yaml", "tree/pkg/wip/b.go.yaml"} {
		if !seen[name] {
			t.Errorf("export 静默漏掉合法嵌套分片:%s", name)
		}
	}
}

func TestImportOverwriteRequiresForceAndReportsReplacement(t *testing.T) {
	e, repo := initEngine(t, map[string]string{"a.go": "package a\n\nfunc F() {}\n"})
	rel := "tree/a.go.yaml"
	path := filepath.Join(repo, ".knowledge", filepath.FromSlash(rel))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sameSemantic := append([]byte("# formatting-only difference\n"), before...)
	rep, err := e.ImportWithOptions(bytes.NewReader(makeTestBundle(t, map[string]string{rel: string(sameSemantic)})), ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Imported != 0 || rep.Skipped != 1 || rep.Entries[0].Action != "skip" {
		t.Fatalf("语义相同应幂等跳过:%+v", rep)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("语义相同跳过时不得重写原字节")
	}

	var shard store.Shard
	if err := yaml.Unmarshal(before, &shard); err != nil {
		t.Fatal(err)
	}
	shard.Nodes[0].Status = model.StatusSuspect
	different, err := yaml.Marshal(&shard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.ImportWithOptions(bytes.NewReader(makeTestBundle(t, map[string]string{rel: string(different)})), ImportOptions{}); err == nil {
		t.Fatal("默认不得覆盖语义不同的非 journal 文件")
	}
	after, _ = os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("未 --force 的冲突导入改写了原文件")
	}
	rep, err = e.ImportWithOptions(bytes.NewReader(makeTestBundle(t, map[string]string{rel: string(different)})), ImportOptions{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Imported != 1 || rep.Entries[0].Action != "replace" || !strings.Contains(rep.Text(), "强制替换") {
		t.Fatalf("force 替换未在报告中明示:%+v\n%s", rep, rep.Text())
	}
}

func TestImportPortablePathPreflight(t *testing.T) {
	validTree := "schema: 1\nnodes: []\n"
	tests := []struct {
		name  string
		files map[string]string
	}{
		{"case-fold collision", map[string]string{"tree/Foo.go.yaml": validTree, "tree/foo.go.yaml": validTree}},
		{"windows reserved", map[string]string{"tree/CON.yaml": validTree}},
		{"windows invalid star", map[string]string{"tree/a*.go.yaml": validTree}},
		{"windows invalid question", map[string]string{"tree/a?.go.yaml": validTree}},
		{"trailing dot", map[string]string{"tree/bad./x.yaml": validTree}},
		{"control", map[string]string{"tree/bad\x01/x.yaml": validTree}},
		{"combining mark", map[string]string{"tree/e\u0301/x.yaml": validTree}},
	}
	for _, name := range []string{"flows/team/nested.yaml", "flows/legacy.jsonl", "topics/domain/nested.yaml", "topics/legacy.jsonl"} {
		t.Run("runtime-invisible-"+strings.ReplaceAll(name, "/", "-"), func(t *testing.T) {
			e, _ := initEngine(t, map[string]string{"a.go": "package a\n"})
			if _, err := e.Import(bytes.NewReader(makeTestBundle(t, map[string]string{name: "{}\n"})), nil); err == nil {
				t.Fatal("运行时不可见的 flow/topic 制品必须拒绝")
			}
		})
	}
	for _, name := range []string{"journal/nested/2026-01.jsonl", "journal/not-a-month.jsonl"} {
		if _, ok := importableBundleEntry(name); ok {
			t.Errorf("运行时不可见/非法 journal 路径被白名单接受:%s", name)
		}
	}
	if clean, ok := importableBundleEntry("journal/2026-01.jsonl"); !ok || clean != "journal/2026-01.jsonl" {
		t.Fatalf("合法 journal 路径被拒:%q/%v", clean, ok)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, _ := initEngine(t, map[string]string{"a.go": "package a\n"})
			if _, err := e.Import(bytes.NewReader(makeTestBundle(t, tt.files)), nil); err == nil {
				t.Fatal("不可便携路径必须在写盘前拒绝")
			}
		})
	}

	t.Run("existing case-fold overwrite", func(t *testing.T) {
		e, _ := initEngine(t, map[string]string{"Case.go": "package c\n"})
		if _, err := e.ImportWithOptions(bytes.NewReader(makeTestBundle(t, map[string]string{"tree/case.go.yaml": validTree})), ImportOptions{Force: true}); err == nil {
			t.Fatal("--force 也不得绕过与现有路径的 EqualFold 冲突")
		}
	})
}

func TestPortableFoldKeyMatchesUnicodeEqualFold(t *testing.T) {
	for _, pair := range [][2]string{{"tree/K.yaml", "tree/K.yaml"}, {"tree/Σ.yaml", "tree/ς.yaml"}, {"tree/Foo", "tree/fOO"}} {
		if !strings.EqualFold(pair[0], pair[1]) {
			t.Fatalf("测试前置非 EqualFold:%q/%q", pair[0], pair[1])
		}
		if portableFoldKey(pair[0]) != portableFoldKey(pair[1]) {
			t.Errorf("fold key 与 EqualFold 不一致:%q/%q", pair[0], pair[1])
		}
	}
}

func TestImportArchiveLimitsAndStrictEntryTypes(t *testing.T) {
	e, repo := initEngine(t, map[string]string{"a.go": "package a\n"})
	valid := validManifestJSON(t)

	t.Run("header limit", func(t *testing.T) {
		bundle := makeRawBundle(t, []rawTarEntry{
			{name: "MANIFEST.json", body: valid},
			{name: "tree/new.go.yaml", body: []byte("schema: 1\nnodes: []\n")},
		}, nil)
		_, err := e.ImportWithOptions(bytes.NewReader(bundle), ImportOptions{MaxHeaders: 1, MaxEntryBytes: 1024, MaxTotalBytes: 4096})
		if err == nil {
			t.Fatal("header 总数超限必须整包失败")
		}
	})

	t.Run("declared total", func(t *testing.T) {
		bundle := makeRawBundle(t, []rawTarEntry{
			{name: "MANIFEST.json", body: valid},
			{name: "tree/new.go.yaml", body: bytes.Repeat([]byte(" "), 80)},
		}, nil)
		_, err := e.ImportWithOptions(bytes.NewReader(bundle), ImportOptions{MaxEntryBytes: 128, MaxTotalBytes: int64(len(valid) + 40)})
		if err == nil {
			t.Fatal("声明解压总量超限必须整包失败")
		}
	})

	t.Run("pax metadata decompression", func(t *testing.T) {
		bundle := makePAXMetadataBundle(t, valid, 128<<10)
		_, err := e.ImportWithOptions(bytes.NewReader(bundle), ImportOptions{MaxEntryBytes: 1024, MaxTotalBytes: 4096, MaxHeaders: 2})
		if err == nil {
			t.Fatal("PAX metadata 也必须计入解压 tar stream hard cap")
		}
	})

	for _, typ := range []byte{tar.TypeDir, tar.TypeSymlink} {
		t.Run("non-regular-"+string(rune(typ)), func(t *testing.T) {
			bundle := makeRawBundle(t, []rawTarEntry{
				{name: "MANIFEST.json", body: valid},
				{name: "tree/new.go.yaml", typ: typ},
			}, nil)
			if _, err := e.Import(bytes.NewReader(bundle), nil); err == nil {
				t.Fatal("非普通 tar 条目必须整包失败")
			}
		})
	}
	if _, err := os.Stat(filepath.Join(repo, ".knowledge/tree/new.go.yaml")); !os.IsNotExist(err) {
		t.Fatalf("失败 bundle 不得部分落盘:%v", err)
	}
}

func TestImportStrictSingleGzipMemberAndTarTail(t *testing.T) {
	baseEntries := []rawTarEntry{
		{name: "MANIFEST.json", body: validManifestJSON(t)},
		{name: "tree/new.go.yaml", body: []byte("schema: 1\nnodes: []\n")},
	}
	e, _ := initEngine(t, map[string]string{"a.go": "package a\n"})
	member := makeRawBundle(t, baseEntries, nil)
	if _, err := e.Import(bytes.NewReader(append(append([]byte(nil), member...), member...)), nil); err == nil {
		t.Fatal("第二个 gzip member 必须拒绝")
	}
	if _, err := e.Import(bytes.NewReader(append(append([]byte(nil), member...), []byte("compressed-junk")...)), nil); err == nil {
		t.Fatal("gzip 压缩尾随必须拒绝")
	}
	if _, err := e.Import(bytes.NewReader(makeRawBundle(t, baseEntries, []byte("hidden"))), nil); err == nil {
		t.Fatal("tar EOF 后的非零解压数据必须拒绝")
	}
	// tar 允许零块后继续零填充；这不是隐藏数据。
	if _, err := e.Import(bytes.NewReader(makeRawBundle(t, baseEntries, make([]byte, 1536))), nil); err != nil {
		t.Fatalf("合法零填充被误拒:%v", err)
	}
}

func TestImportPanicRollsBackInProcessAndClearsActiveState(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"a.go": "package a\n"})
	bundle := makeTestBundle(t, map[string]string{
		"tree/crash-a.go.yaml": "schema: 1\nnodes: []\n",
		"tree/crash-b.go.yaml": "schema: 1\nnodes: []\n",
	})
	crashed := false
	e.afterImportTruthWrite = func(string) error {
		panic("simulated process crash after truth rename")
	}
	func() {
		defer func() {
			if recover() != nil {
				crashed = true
			}
		}()
		_, _ = e.Import(bytes.NewReader(bundle), nil)
	}()
	if !crashed {
		t.Fatal("崩溃注入未触发")
	}
	for _, rel := range []string{"tree/crash-a.go.yaml", "tree/crash-b.go.yaml"} {
		if _, err := e.Store.ReadKnowledgeFile(rel); !os.IsNotExist(err) {
			t.Fatalf("同进程 panic 回滚后 %s 不应存在:%v", rel, err)
		}
	}
	e.afterImportTruthWrite = nil
	if _, err := e.Import(bytes.NewReader(makeTestBundle(t, map[string]string{
		"tree/after-panic.go.yaml": "schema: 1\nnodes: []\n",
	})), nil); err != nil {
		t.Fatalf("panic 后 active/WAL 未清，下一事务失败:%v", err)
	}
	if err := e.Store.RecoverTruthTransaction(); err != nil {
		t.Fatalf("panic rollback 应清理 WAL:%v", err)
	}
}

func TestImportOrdinaryMidWriteFailureRollsBackAndClearsWAL(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"a.go": "package a\n"})
	bundle := makeTestBundle(t, map[string]string{
		"tree/fail-a.go.yaml": "schema: 1\nnodes: []\n",
		"tree/fail-b.go.yaml": "schema: 1\nnodes: []\n",
	})
	calls := 0
	e.afterImportTruthWrite = func(string) error {
		calls++
		if calls == 1 {
			return os.ErrPermission
		}
		return nil
	}
	if _, err := e.Import(bytes.NewReader(bundle), nil); err == nil {
		t.Fatal("中途写失败必须上抬")
	}
	for _, rel := range []string{"tree/fail-a.go.yaml", "tree/fail-b.go.yaml"} {
		if _, err := e.Store.ReadKnowledgeFile(rel); !os.IsNotExist(err) {
			t.Fatalf("普通失败应回滚 %s:%v", rel, err)
		}
	}
	if err := e.Store.RecoverTruthTransaction(); err != nil {
		t.Fatalf("普通 rollback 应清理 WAL:%v", err)
	}
}

func TestImportCommittedWALPreservesTruthOnRestart(t *testing.T) {
	e, repo := initEngine(t, map[string]string{"a.go": "package a\n"})
	rel := "tree/committed.go.yaml"
	want := "schema: 1\nnodes: []\n"
	if _, err := e.Import(bytes.NewReader(makeTestBundle(t, map[string]string{rel: want})), nil); err != nil {
		t.Fatal(err)
	}
	fresh, err := store.Open(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.RecoverTruthTransaction(); err != nil {
		t.Fatalf("重启清理 committed WAL:%v", err)
	}
	data, err := fresh.ReadKnowledgeFile(rel)
	if err != nil || !strings.Contains(string(data), "nodes: []") {
		t.Fatalf("已提交 import 被重启恢复误回滚:data=%q err=%v", data, err)
	}
}

func TestImportRequiresUniqueValidManifest(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"a.go": "package a\n"})
	data := []byte("schema: 1\nnodes: []\n")
	for _, tc := range []struct {
		name    string
		entries []rawTarEntry
	}{
		{"missing", []rawTarEntry{{name: "tree/new.go.yaml", body: data}}},
		{"bad schema", []rawTarEntry{{name: "MANIFEST.json", body: []byte(`{"schema":2,"exported_at":"2026-07-11T00:00:00Z","repo":"/x"}`)}}},
		{"duplicate", []rawTarEntry{{name: "MANIFEST.json", body: validManifestJSON(t)}, {name: "MANIFEST.json", body: validManifestJSON(t)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := e.Import(bytes.NewReader(makeRawBundle(t, tc.entries, nil)), nil); err == nil {
				t.Fatal("非法 manifest 必须拒绝")
			}
		})
	}
}

func TestImportConfigGlobValidationAndRemap(t *testing.T) {
	e, repo := initEngine(t, map[string]string{"a.go": "package a\n"})
	valid := "schema: 1\nport: 18001\ninclude: [old/*.go]\nexclude: [old/generated/*.go]\n"
	if _, err := e.ImportWithOptions(bytes.NewReader(makeTestBundle(t, map[string]string{"config.yaml": valid})), ImportOptions{
		PathRemap: map[string]string{"old": "new"}, Force: true,
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(repo, ".knowledge/config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "old/") || !strings.Contains(string(data), "new/*.go") || !strings.Contains(string(data), "new/generated/*.go") {
		t.Fatalf("config glob 未随 remap 语义变换:\n%s", data)
	}

	for _, body := range []string{
		"schema: 1\nport: 18001\ninclude: ['[']\n",
		"schema: 1\nport: 18001\ninclude: ['**/*.go']\n",
	} {
		if _, err := e.ImportWithOptions(bytes.NewReader(makeTestBundle(t, map[string]string{"config.yaml": body})), ImportOptions{
			PathRemap: map[string]string{"old": "new"}, Force: true,
		}); err == nil {
			t.Fatalf("非法或无法安全变换的 glob 必须 fail closed:%s", body)
		}
	}
}

func TestImportRejectsMalformedNodeEntryAndJournalMonth(t *testing.T) {
	trees := []string{
		`schema: 1
nodes:
  - {id: x.go#F, level: function, anchor: {file: x.go, symbol: Wrong}, status: fresh, since: 2026-01-01T00:00:00Z}
`,
		`schema: 1
nodes:
  - id: x.go
    level: file
    anchor: {file: x.go}
    status: fresh
    since: 2026-01-01T00:00:00Z
    entries:
      - {id: e_dup, kind: contract, text: one, confidence: inferred}
      - {id: e_dup, kind: contract, text: two, confidence: inferred}
`,
		`schema: 1
nodes:
  - id: x.go
    level: file
    anchor: {file: x.go}
    status: fresh
    since: 2026-01-01T00:00:00Z
    entries:
      - {id: "bad#id", kind: contract, text: one, confidence: inferred}
`,
		`schema: 1
nodes:
  - id: x.go
    level: file
    anchor: {file: x.go}
    status: immortal
    since: 2026-01-01T00:00:00Z
`,
		`schema: 1
nodes:
  - id: x.go
    level: file
    anchor: {file: x.go}
    status: fresh
    since: 2026-01-01T00:00:00Z
    entries:
      - {id: e_bad_kind, kind: invented, text: one, confidence: inferred}
`,
		`schema: 1
nodes:
  - id: x.go
    level: file
    anchor: {file: x.go}
    status: fresh
    since: 2026-01-01T00:00:00Z
    entries:
      - {id: e_bad_confidence, kind: contract, text: one, confidence: REFUTED}
`,
	}
	for i, body := range trees {
		t.Run("tree-"+string(rune('a'+i)), func(t *testing.T) {
			e, _ := initEngine(t, map[string]string{"a.go": "package a\n"})
			if _, err := e.Import(bytes.NewReader(makeTestBundle(t, map[string]string{"tree/x.go.yaml": body})), nil); err == nil {
				t.Fatal("Anchor/Entry staging 不变量破坏必须拒绝")
			}
		})
	}
	e, _ := initEngine(t, map[string]string{"a.go": "package a\n"})
	wrongMonth := `{"id":"chg_month","nodes":["a.go"],"at":"2026-02-01T00:00:00Z","what":"x","why":"x"}` + "\n"
	if _, err := e.Import(bytes.NewReader(makeTestBundle(t, map[string]string{"journal/2026-01.jsonl": wrongMonth})), nil); err == nil {
		t.Fatal("journal change.At 必须归属文件名月份")
	}
	tooNew := `{"id":"chg_new_effects","nodes":["a.go"],"at":"2026-01-01T00:00:00Z","what":"x","why":"x","effects_version":2}` + "\n"
	if _, err := e.Import(bytes.NewReader(makeTestBundle(t, map[string]string{"journal/2026-01.jsonl": tooNew})), nil); err == nil {
		t.Fatal("journal effects_version 高于当前引擎必须拒绝")
	}
	badEffect := `{"id":"chg_bad_effect","nodes":["a.go"],"at":"2026-01-01T00:00:00Z","what":"x","why":"x","effects_version":1,"effects":[{"entry":"a.go#e_x","before":{"confidence":"inferred"},"after":{"confidence":"IMMORTAL"}}]}` + "\n"
	if _, err := e.Import(bytes.NewReader(makeTestBundle(t, map[string]string{"journal/2026-01.jsonl": badEffect})), nil); err == nil {
		t.Fatal("journal EntryEffect 非法 confidence 必须拒绝")
	}
}

type rawTarEntry struct {
	name string
	body []byte
	typ  byte
}

func validManifestJSON(t *testing.T) []byte {
	t.Helper()
	data, err := json.Marshal(bundleManifest{
		Schema: bundleManifestSchema, ExportedAt: time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC), Repo: "/source/repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func makeRawBundle(t *testing.T, entries []rawTarEntry, decompressedTail []byte) []byte {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, entry := range entries {
		typ := entry.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		size := int64(len(entry.body))
		if typ != tar.TypeReg {
			size = 0
		}
		if err := tw.WriteHeader(&tar.Header{Name: entry.name, Mode: 0o644, Size: size, Typeflag: typ}); err != nil {
			t.Fatal(err)
		}
		if size > 0 {
			if _, err := tw.Write(entry.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	raw.Write(decompressedTail)
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func makePAXMetadataBundle(t *testing.T, manifest []byte, metadataBytes int) []byte {
	t.Helper()
	pax := map[string]string{}
	for i, remaining := 0, metadataBytes; remaining > 0; i++ {
		n := 1024
		if remaining < n {
			n = remaining
		}
		pax[fmt.Sprintf("comment.%06d", i)] = strings.Repeat("x", n)
		remaining -= n
	}
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, header := range []*tar.Header{
		{Name: "MANIFEST.json", Mode: 0o644, Size: int64(len(manifest)), Typeflag: tar.TypeReg},
		{Name: "tree/pax.go.yaml", Mode: 0o644, Size: int64(len("schema: 1\nnodes: []\n")), Typeflag: tar.TypeReg,
			PAXRecords: pax},
	} {
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		body := manifest
		if header.Name != "MANIFEST.json" {
			body = []byte("schema: 1\nnodes: []\n")
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}
