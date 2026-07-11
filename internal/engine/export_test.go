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
	goruntime "runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/zdypro888/iknowledge/internal/model"
	"github.com/zdypro888/iknowledge/internal/store"
)

func TestImportRestrictsBundleEntries(t *testing.T) {
	for _, bad := range []string{"local/token", "wip/task.yaml", "../escape", "/abs", "tree/../local/token", "unknown.yaml"} {
		t.Run(strings.ReplaceAll(bad, "/", "_"), func(t *testing.T) {
			e, repo := initEngine(t, map[string]string{"a.go": "package a\n\nfunc F() {}\n"})
			bundle := makeTestBundle(t, map[string]string{
				"tree/imported.go.yaml": "schema: 1\nnodes: []\n",
				bad:                     "evil",
			})
			if _, err := e.Import(bytes.NewReader(bundle), nil); err == nil {
				t.Fatalf("非法条目 %q 必须使整包失败", bad)
			}
			if _, err := os.Stat(filepath.Join(repo, ".knowledge/tree/imported.go.yaml")); !os.IsNotExist(err) {
				t.Fatalf("整包失败不得部分导入:%v", err)
			}
		})
	}
}

func TestImportDryRunAndBackupReport(t *testing.T) {
	e, repo := initEngine(t, map[string]string{"a.go": "package a\n\nfunc F() {}\n"})
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	manifest, err := json.Marshal(bundleManifest{Schema: bundleManifestSchema, ExportedAt: time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC), Repo: "/source/repo"})
	if err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "MANIFEST.json", Mode: 0o644, Size: int64(len(manifest)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(manifest); err != nil {
		t.Fatal(err)
	}
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
	if rep.Imported != 1 || rep.Scanned != 2 { // 含必需的 MANIFEST.json
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
	rep2, err := e.ImportWithOptions(bytes.NewReader(buf.Bytes()), ImportOptions{Backup: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep2.BackupPath == rep.BackupPath {
		t.Fatalf("同秒两次 backup 不得互相覆盖:%s", rep.BackupPath)
	}
}

func TestExportPropagatesFinalFlushError(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"a.go": "package a\n\nfunc F() {}\n"})
	w := &failAfterWrite{allow: 1}
	if err := e.Export(w); err == nil {
		t.Fatal("tar/gzip 最终 flush 写失败必须由 Export 上抬")
	}
}

func TestExportRejectsKnowledgeSymlink(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("Windows symlink 需要额外权限")
	}
	e, repo := initEngine(t, map[string]string{"a.go": "package a\n\nfunc F() {}\n"})
	secret := filepath.Join(repo, "outside-secret.yaml")
	if err := os.WriteFile(secret, []byte("TOP_SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(repo, ".knowledge/tree/leak.yaml")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := e.Export(&out); err == nil {
		t.Fatal("Export 必须拒绝 .knowledge 内 symlink")
	}
	if bytes.Contains(out.Bytes(), []byte("TOP_SECRET")) {
		t.Fatal("Export 跟随 symlink 泄露了仓外内容")
	}
}

func TestExportImportRoundTripDoesNotConflictOnJSONFieldOrder(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"a.go": "package a\n\nfunc F() {}\n"})
	if _, err := e.RecordChange(ChangeArgs{Nodes: []string{"a.go#F"}, What: "round trip", Why: "test"}, "s", "codex"); err != nil {
		t.Fatal(err)
	}
	var bundle bytes.Buffer
	if err := e.Export(&bundle); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Import(bytes.NewReader(bundle.Bytes()), nil); err != nil {
		t.Fatalf("自己的 Export 重新 Import 不应因 JSON 字段顺序误报同 ID 冲突:%v", err)
	}
	changes, stats, err := e.Store.LoadJournal()
	if err != nil || len(changes) != 1 || len(stats.ConflictIDs) != 0 {
		t.Fatalf("round trip journal=%+v stats=%+v err=%v", changes, stats, err)
	}
}

type failAfterWrite struct {
	allow int
	calls int
}

func (w *failAfterWrite) Write(p []byte) (int, error) {
	w.calls++
	if w.calls > w.allow {
		return 0, io.ErrClosedPipe
	}
	return len(p), nil
}

func TestImportSemanticRemapPreservesFreeTextAndUnknownFields(t *testing.T) {
	e, repo := initEngine(t, map[string]string{"a.go": "package a\n\nfunc F() {}\n"})
	tree := `schema: 1
nodes:
  - id: old/a.go#F
    level: function
    anchor:
      file: old/a.go
      symbol: F
    status: fresh
    since: 2026-01-01T00:00:00Z
    lineage: [old/legacy.go#F]
    future_node: keep-node
    entries:
      - id: e_12345678
        kind: contract
        text: "自由文本 old/ 不得被路径 remap 改写"
        confidence: inferred
        based_on: [old/a.go#B#e_base]
        disputes: [old/a.go#P#e_peer]
        future_entry: keep-entry
  - id: old/a.go#B
    level: function
    anchor: {file: old/a.go, symbol: B}
    status: fresh
    since: 2026-01-01T00:00:00Z
    entries:
      - {id: e_base, kind: contract, text: base, confidence: inferred}
  - id: old/a.go#P
    level: function
    anchor: {file: old/a.go, symbol: P}
    status: fresh
    since: 2026-01-01T00:00:00Z
    entries:
      - {id: e_peer, kind: contract, text: peer, confidence: inferred}
`
	journal := `{"id":"chg_old","nodes":["old/a.go#F"],"at":"2026-01-01T00:00:00Z","what":"说明 old/ 是自由文本","why":"test","remaps":[{"from":"old/a.go#F","to":["old/b.go#B"],"entries":{"e_12345678":"old/b.go#B"},"future":"keep"}],"future_top":"keep"}` + "\n"
	flow := `schema: 1
flow:
  id: flow:test
  title: test
  steps:
    - node: old/a.go#F
      note: old/ 自由说明
  since: 2026-01-01T00:00:00Z
  future_flow: keep-flow
`
	bundle := makeTestBundle(t, map[string]string{
		"tree/old/a.go.yaml": tree, "journal/2026-01.jsonl": journal, "flows/test.yaml": flow,
	})
	_, err := e.ImportWithOptions(bytes.NewReader(bundle), ImportOptions{PathRemap: map[string]string{
		"old": "new", "new": "final", // 同一值只映射一次，不能 old→new→final
	}})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(repo, ".knowledge/tree/new/a.go.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".knowledge/tree/final/a.go.yaml")); !os.IsNotExist(err) {
		t.Fatalf("多映射发生级联: %v", err)
	}
	text := string(data)
	for _, want := range []string{"id: new/a.go#F", "file: new/a.go", "new/legacy.go#F", "new/a.go#B#e_base", "new/a.go#P#e_peer", "自由文本 old/", "future_node: keep-node", "future_entry: keep-entry"} {
		if !strings.Contains(text, want) {
			t.Errorf("tree remap/未知字段缺 %q:\n%s", want, text)
		}
	}
	journalData, err := os.ReadFile(filepath.Join(repo, ".knowledge/journal/2026-01.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var change map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(journalData), &change); err != nil {
		t.Fatal(err)
	}
	if got := change["what"]; got != "说明 old/ 是自由文本" {
		t.Errorf("journal 自由文本被改写:%v", got)
	}
	if nodes := change["nodes"].([]any); nodes[0] != "new/a.go#F" {
		t.Errorf("journal nodes 未映射:%v", nodes)
	}
	if change["future_top"] != "keep" {
		t.Errorf("journal 未知字段丢失:%v", change)
	}
	flowData, err := os.ReadFile(filepath.Join(repo, ".knowledge/flows/test.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(flowData), "node: new/a.go#F") || !strings.Contains(string(flowData), "note: old/ 自由说明") || !strings.Contains(string(flowData), "future_flow: keep-flow") {
		t.Errorf("flow remap/未知字段错误:\n%s", flowData)
	}
}

func TestImportStagingRejectsPathAndNodeConflictsWithoutWrites(t *testing.T) {
	tests := []struct {
		name   string
		files  map[string]string
		remap  map[string]string
		marker string
	}{
		{
			name: "output path collision",
			files: map[string]string{
				"tree/old/a.go.yaml": shardYAML(t, "old/a.go"),
				"tree/new/a.go.yaml": shardYAML(t, "new/a.go"),
			},
			remap: map[string]string{"old": "new"}, marker: ".knowledge/tree/new/a.go.yaml",
		},
		{
			name: "duplicate node id",
			files: map[string]string{
				"tree/dup/a.go.yaml": `schema: 1
nodes:
  - {id: dup/a.go, level: file, anchor: {file: dup/a.go}, status: fresh, since: 2026-01-01T00:00:00Z}
  - {id: dup/a.go, level: file, anchor: {file: dup/a.go}, status: fresh, since: 2026-01-01T00:00:00Z}
`,
			}, marker: ".knowledge/tree/dup/a.go.yaml",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, repo := initEngine(t, map[string]string{"a.go": "package a\n\nfunc F() {}\n"})
			bundle := makeTestBundle(t, tt.files)
			if _, err := e.ImportWithOptions(bytes.NewReader(bundle), ImportOptions{PathRemap: tt.remap}); err == nil {
				t.Fatal("冲突 staging 应拒绝")
			}
			if _, err := os.Stat(filepath.Join(repo, tt.marker)); !os.IsNotExist(err) {
				t.Fatalf("staging 失败不应落任何导入文件:%v", err)
			}
		})
	}
}

func TestImportMergesJournalAndRejectsChangeIDConflict(t *testing.T) {
	makeTarget := func(t *testing.T) (*Engine, string, string, []byte, string) {
		t.Helper()
		e, repo := initEngine(t, map[string]string{"a.go": "package a\n\nfunc F() {}\n"})
		if _, err := e.RecordChange(ChangeArgs{Nodes: []string{"a.go#F"}, What: "目标仓历史", Why: "保留"}, "s", "codex"); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(filepath.Join(repo, ".knowledge/journal"))
		if err != nil || len(entries) != 1 {
			t.Fatalf("journal files=%v err=%v", entries, err)
		}
		rel := "journal/" + entries[0].Name()
		data, err := os.ReadFile(filepath.Join(repo, ".knowledge", filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		changes, _, err := e.Store.LoadJournal()
		if err != nil || len(changes) != 1 {
			t.Fatalf("changes=%v err=%v", changes, err)
		}
		return e, repo, rel, data, changes[0].ID
	}

	t.Run("preserve target unique history", func(t *testing.T) {
		e, _, rel, _, _ := makeTarget(t)
		month := strings.TrimSuffix(filepath.Base(rel), ".jsonl")
		incoming := fmt.Sprintf(`{"id":"chg_bundle_unique","nodes":["a.go#F"],"at":%q,"what":"bundle history","why":"merge"}`+"\n", month+"-01T00:00:00Z")
		if _, err := e.Import(bytes.NewReader(makeTestBundle(t, map[string]string{rel: incoming})), nil); err != nil {
			t.Fatal(err)
		}
		changes, _, err := e.Store.LoadJournal()
		if err != nil {
			t.Fatal(err)
		}
		if len(changes) != 2 {
			t.Fatalf("同月 import 覆盖丢了目标历史:%+v", changes)
		}
	})

	t.Run("same id different content", func(t *testing.T) {
		e, repo, rel, before, id := makeTarget(t)
		month := strings.TrimSuffix(filepath.Base(rel), ".jsonl")
		incoming := fmt.Sprintf(`{"id":%q,"nodes":["a.go#F"],"at":%q,"what":"冲突内容","why":"bad"}`+"\n", id, month+"-01T00:00:00Z")
		if _, err := e.Import(bytes.NewReader(makeTestBundle(t, map[string]string{rel: incoming})), nil); err == nil {
			t.Fatal("同 change ID 不同内容必须拒绝")
		}
		after, err := os.ReadFile(filepath.Join(repo, ".knowledge", filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("冲突 staging 仍改写了目标 journal")
		}
	})
}

func TestImportRejectsFlowFileMismatchAndDanglingStep(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"id file mismatch", "schema: 1\nflow:\n  id: flow:other\n  title: x\n  since: 2026-01-01T00:00:00Z\n"},
		{"dangling step", "schema: 1\nflow:\n  id: flow:test\n  title: x\n  steps:\n    - node: missing/a.go#F\n  since: 2026-01-01T00:00:00Z\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, repo := initEngine(t, map[string]string{"a.go": "package a\n\nfunc F() {}\n"})
			if _, err := e.Import(bytes.NewReader(makeTestBundle(t, map[string]string{"flows/test.yaml": tt.body})), nil); err == nil {
				t.Fatal("非法 flow staging 应拒绝")
			}
			if _, err := os.Stat(filepath.Join(repo, ".knowledge/flows/test.yaml")); !os.IsNotExist(err) {
				t.Fatalf("非法 flow 不应落盘:%v", err)
			}
		})
	}
}

func TestImportValidatesGzipTrailerBeforeWrites(t *testing.T) {
	e, repo := initEngine(t, map[string]string{"a.go": "package a\n\nfunc F() {}\n"})
	bundle := makeTestBundle(t, map[string]string{"tree/imported.go.yaml": "schema: 1\nnodes: []\n"})
	bundle[len(bundle)-1] ^= 0xff // 破坏 gzip ISIZE/CRC trailer
	if _, err := e.Import(bytes.NewReader(bundle), nil); err == nil {
		t.Fatal("gzip trailer 损坏必须拒绝")
	}
	if _, err := os.Stat(filepath.Join(repo, ".knowledge/tree/imported.go.yaml")); !os.IsNotExist(err) {
		t.Fatalf("校验 trailer 前不应写盘:%v", err)
	}
}

func TestImportConfigValidationFailClosed(t *testing.T) {
	tests := []string{
		"schema: 2\nport: 18000\n",
		"schema: 1\nport: 0\n",
		"schema: 1\nport: 70000\n",
		"schema: 1\nport: 18000\nscout: shell\n",
		"schema: 1\nport: 18000\nscout_timeout_seconds: -1\n",
		"schema: [broken\n",
	}
	for i, body := range tests {
		t.Run(fmt.Sprintf("case-%d", i), func(t *testing.T) {
			e, repo := initEngine(t, map[string]string{"a.go": "package a\n\nfunc F() {}\n"})
			configPath := filepath.Join(repo, ".knowledge/config.yaml")
			before, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.Import(bytes.NewReader(makeTestBundle(t, map[string]string{"config.yaml": body})), nil); err == nil {
				t.Fatal("非法 config 应在 staging 拒绝")
			}
			after, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("非法 config 覆盖了目标配置")
			}
		})
	}
}

func TestImportRejectsOverwriteThatBreaksExistingReferences(t *testing.T) {
	e, repo := initEngine(t, map[string]string{
		"a/a.go": "package a\n\nfunc A() {}\n",
		"b/b.go": "package b\n\nfunc B() {}\n",
	})
	if _, err := e.Remember(RememberArgs{Node: "b/b.go#B", Entries: []RememberEntry{{Kind: "contract", Text: "B contract"}}}, "s", "codex"); err != nil {
		t.Fatal(err)
	}
	b := e.rt.ix.Node("b/b.go#B").Node.Entries[0]
	if _, err := e.Remember(RememberArgs{Node: "a/a.go#A", Entries: []RememberEntry{{
		Kind: "contract", Text: "A depends B", BasedOn: []string{"b/b.go#B#" + b.ID},
	}}}, "s", "codex"); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.SaveFlow(model.Flow{ID: "flow:ab", Title: "AB", Steps: []model.FlowStep{{Node: "b/b.go#B"}}}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(repo, ".knowledge/tree/b/b.go.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	bundle := makeTestBundle(t, map[string]string{"tree/b/b.go.yaml": "schema: 1\nnodes: []\n"})
	if _, err := e.Import(bytes.NewReader(bundle), nil); err == nil {
		t.Fatal("覆盖 tree 导致现有 based_on/flow 悬空时必须拒绝")
	}
	after, err := os.ReadFile(filepath.Join(repo, ".knowledge/tree/b/b.go.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("引用完整性校验失败后仍覆盖了 tree")
	}
}

func makeTestBundle(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if _, ok := files["MANIFEST.json"]; !ok {
		manifest, err := json.Marshal(bundleManifest{Schema: bundleManifestSchema, ExportedAt: time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC), Repo: "/source/repo"})
		if err != nil {
			t.Fatal(err)
		}
		copyFiles := make(map[string]string, len(files)+1)
		for name, body := range files {
			copyFiles[name] = body
		}
		copyFiles["MANIFEST.json"] = string(manifest)
		files = copyFiles
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		body := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
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

func shardYAML(t *testing.T, file string) string {
	t.Helper()
	data, err := yaml.Marshal(&store.Shard{Schema: model.SchemaVersion, Nodes: []model.Node{{
		ID: file, Level: model.LevelFile, Anchor: model.Anchor{File: file}, Status: model.StatusFresh,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
