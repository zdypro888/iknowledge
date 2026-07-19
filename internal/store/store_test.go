package store

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zdypro888/iknowledge/internal/model"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.EnsureLayout(); err != nil {
		t.Fatalf("EnsureLayout: %v", err)
	}
	return s
}

func sampleShard() *Shard {
	return &Shard{
		Schema: model.SchemaVersion,
		Nodes: []model.Node{
			{
				ID: "internal/auth/login.go", Level: model.LevelFile,
				Anchor: model.Anchor{File: "internal/auth/login.go", Hash: "sha256:f11e"},
				Status: model.StatusUndigested,
				Since:  time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC),
			},
			{
				ID: "internal/auth/login.go#Login", Level: model.LevelFunction,
				Anchor: model.Anchor{
					File: "internal/auth/login.go", Symbol: "Login",
					Hash: "sha256:ab12", StructHash: "sha256:ef56", Lines: [2]int{40, 140},
				},
				Status: model.StatusFresh,
				Since:  time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC),
				Entries: []model.Entry{{
					ID: "e_11223344", Kind: model.KindPitfall,
					Text: "不要在调用方预先加密", Confidence: model.ConfidenceInferred,
				}},
				Keywords: []string{"登录", "锁定"},
			},
		},
	}
}

func TestShardRoundTrip(t *testing.T) {
	s := newStore(t)
	path := s.ShardPathFor("internal/auth/login.go")
	if err := s.SaveShard(path, sampleShard(), nil); err != nil {
		t.Fatalf("SaveShard: %v", err)
	}
	got, raw, err := s.LoadShard(path)
	if err != nil {
		t.Fatalf("LoadShard: %v", err)
	}
	if raw == nil {
		t.Fatal("raw 不应为 nil")
	}
	if got.Schema != model.SchemaVersion || len(got.Nodes) != 2 {
		t.Fatalf("往返丢内容:%+v", got)
	}
	n := got.Nodes[1]
	if n.ID != "internal/auth/login.go#Login" || n.Anchor.StructHash != "sha256:ef56" ||
		len(n.Entries) != 1 || n.Entries[0].Text != "不要在调用方预先加密" ||
		n.Anchor.Lines != [2]int{40, 140} {
		t.Errorf("节点字段往返不完整:%+v", n)
	}
}

func TestPrivateKnowledgeStreamIsAtomic(t *testing.T) {
	s := newStore(t)
	const rel = "local/vector.idx"
	if err := s.WritePrivateKnowledgeFile(rel, []byte("old")); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("encode failed")
	err := s.WritePrivateKnowledgeFileStream(rel, func(w io.Writer) error {
		_, _ = w.Write([]byte("partial-new"))
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("stream error=%v, want wrapped encode error", err)
	}
	if got, err := s.ReadKnowledgeFile(rel); err != nil || string(got) != "old" {
		t.Fatalf("失败写入破坏旧文件: %q/%v", got, err)
	}
	payload := strings.Repeat("vector", 1<<15)
	if err := s.WritePrivateKnowledgeFileStream(rel, func(w io.Writer) error {
		_, err := io.WriteString(w, payload)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f, err := s.OpenKnowledgeFileRead(rel)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(f)
	closeErr := f.Close()
	if err != nil || closeErr != nil || string(got) != payload {
		t.Fatalf("stream roundtrip bytes=%d read=%v close=%v", len(got), err, closeErr)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(s.Dir(), filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("vector cache mode=%o, want 600", info.Mode().Perm())
		}
	}
}

func TestSrcRelOfShardAllowsDotDotPrefixName(t *testing.T) {
	s := newStore(t)
	path := s.ShardPathFor("..cache/a.go")
	if path == "" {
		t.Fatal("ShardPathFor should allow a repo-local path segment that merely starts with '..'")
	}
	if err := s.SaveShard(path, &Shard{Schema: model.SchemaVersion, Nodes: []model.Node{{
		ID: "..cache/a.go", Level: model.LevelFile,
		Anchor: model.Anchor{File: "..cache/a.go"},
		Status: model.StatusUndigested,
		Since:  time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC),
	}}}, nil); err != nil {
		t.Fatalf("SaveShard: %v", err)
	}
	if got := s.SrcRelOfShard(path); got != "..cache/a.go" {
		t.Fatalf("SrcRelOfShard = %q, want ..cache/a.go", got)
	}
}

// TestShardUnknownFieldPreserved 未知字段往返保留(impl §4 定案):
// 新版本二进制在分片各层写入的未知字段,旧二进制改写分片后必须原样带回。
func TestShardUnknownFieldPreserved(t *testing.T) {
	s := newStore(t)
	path := s.ShardPathFor("a.go")

	// 模拟"新版本"写的分片:顶层、节点层、条目层各有一个本版本不认识的字段。
	future := `schema: 1
future_top: 顶层未知
nodes:
  - id: a.go
    level: file
    anchor:
      file: a.go
      hash: sha256:aa
      future_anchor: 锚层未知
    status: undigested
    since: 2026-07-04T00:00:00Z
  - id: a.go#F
    level: function
    anchor:
      file: a.go
      symbol: F
      hash: sha256:bb
    status: fresh
    since: 2026-07-04T00:00:00Z
    future_node: 节点层未知
    entries:
      - id: e_00000001
        kind: usage
        text: 原文本
        confidence: inferred
        future_entry: 条目层未知
`
	if err := s.atomicWrite(path, []byte(future)); err != nil {
		t.Fatalf("写 fixture: %v", err)
	}

	sh, raw, err := s.LoadShard(path)
	if err != nil {
		t.Fatalf("LoadShard: %v", err)
	}
	// 旧二进制的正常操作:改状态、改条目文本、加节点,然后回写。
	sh.Nodes[1].Status = model.StatusSuspect
	sh.Nodes[1].Entries[0].Text = "新文本"
	sh.Nodes = append(sh.Nodes, model.Node{
		ID: "a.go#G", Level: model.LevelFunction,
		Anchor: model.Anchor{File: "a.go", Symbol: "G", Hash: "sha256:cc"},
		Status: model.StatusUndigested, Since: time.Now().UTC(),
	})
	if err := s.SaveShard(path, sh, raw); err != nil {
		t.Fatalf("SaveShard: %v", err)
	}

	data, _ := os.ReadFile(path)
	text := string(data)
	for _, want := range []string{"future_top: 顶层未知", "future_anchor: 锚层未知", "future_node: 节点层未知", "future_entry: 条目层未知"} {
		if !strings.Contains(text, want) {
			t.Errorf("未知字段被丢:%q\n%s", want, text)
		}
	}
	for _, want := range []string{"status: suspect", "text: 新文本", "a.go#G"} {
		if !strings.Contains(text, want) {
			t.Errorf("新值未写入:%q", want)
		}
	}
	if strings.Contains(text, "原文本") {
		t.Errorf("旧值残留(未知字段合并覆盖了新值)")
	}
}

// TestShardDeletedNodeNotResurrected 引擎有意删除的节点不因未知字段合并复活。
func TestShardDeletedNodeNotResurrected(t *testing.T) {
	s := newStore(t)
	path := s.ShardPathFor("a.go")
	sh := sampleShard()
	if err := s.SaveShard(path, sh, nil); err != nil {
		t.Fatalf("SaveShard: %v", err)
	}
	sh2, raw, err := s.LoadShard(path)
	if err != nil {
		t.Fatalf("LoadShard: %v", err)
	}
	sh2.Nodes = sh2.Nodes[:1] // 删掉 #Login
	if err := s.SaveShard(path, sh2, raw); err != nil {
		t.Fatalf("SaveShard: %v", err)
	}
	got, _, err := s.LoadShard(path)
	if err != nil {
		t.Fatalf("LoadShard: %v", err)
	}
	if len(got.Nodes) != 1 {
		t.Errorf("被删节点复活了:%+v", got.Nodes)
	}
}

func TestShardUnknownFieldsDoNotCrossChangedStableIDs(t *testing.T) {
	s := newStore(t)
	path := s.ShardPathFor("a.go")
	fixture := `schema: 1
nodes:
  - id: a.go#Old
    level: function
    anchor:
      file: a.go
      symbol: Old
    status: fresh
    since: 2026-07-04T00:00:00Z
    future_node: belongs-to-old
`
	if err := s.atomicWrite(path, []byte(fixture)); err != nil {
		t.Fatal(err)
	}
	_, raw, err := s.LoadShard(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := &Shard{Schema: model.SchemaVersion, Nodes: []model.Node{{
		ID: "a.go#New", Level: model.LevelFunction,
		Anchor: model.Anchor{File: "a.go", Symbol: "New"},
		Status: model.StatusFresh, Since: time.Now().UTC(),
	}}}
	if err := s.SaveShard(path, replacement, raw); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "belongs-to-old") {
		t.Fatalf("稳定 id 改变后不应把旧节点未知字段挂到新节点:\n%s", data)
	}
}

func TestShardOverdeepUnknownFieldFailsClosedWithoutPanic(t *testing.T) {
	s := newStore(t)
	path := s.ShardPathFor("deep.go")
	var nested strings.Builder
	nested.WriteString("future_deep:\n")
	for i := 0; i < maxMergeDepth+2; i++ {
		nested.WriteString(strings.Repeat("  ", i+1) + "x:\n")
	}
	nested.WriteString(strings.Repeat("  ", maxMergeDepth+3) + "leaf: value\n")
	fixture := "schema: 1\nnodes: []\n" + nested.String()
	if err := s.atomicWrite(path, []byte(fixture)); err != nil {
		t.Fatal(err)
	}
	sh, raw, err := s.LoadShard(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveShard(path, sh, raw); err == nil {
		t.Fatal("过深未知字段必须只读拒写")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != fixture {
		t.Fatal("拒写时原文件不应改变")
	}
}

func TestShardConflictIsolated(t *testing.T) {
	s := newStore(t)
	path := s.ShardPathFor("a.go")
	conflict := "schema: 1\n<<<<<<< HEAD\nnodes: []\n=======\nnodes:\n  - id: a\n>>>>>>> other\n"
	if err := s.atomicWrite(path, []byte(conflict)); err != nil {
		t.Fatalf("写 fixture: %v", err)
	}
	_, _, err := s.LoadShard(path)
	if !errors.Is(err, ErrShardConflict) {
		t.Errorf("冲突分片应报 ErrShardConflict,got %v", err)
	}
}

func TestShardSchemaTooNew(t *testing.T) {
	s := newStore(t)
	path := s.ShardPathFor("a.go")
	if err := s.atomicWrite(path, []byte("schema: 99\nnodes: []\n")); err != nil {
		t.Fatalf("写 fixture: %v", err)
	}
	_, _, err := s.LoadShard(path)
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Errorf("更高版本分片应报 ErrSchemaTooNew,got %v", err)
	}
}

// TestAtomicWriteCleanup 崩溃残留的 temp 文件匹配 *.tmp(gitignore 兜住),
// 且成功写入后目录里没有残留。
func TestAtomicWrite(t *testing.T) {
	s := newStore(t)
	path := s.ShardPathFor("a.go")
	if err := s.SaveShard(path, sampleShard(), nil); err != nil {
		t.Fatalf("SaveShard: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("成功写入后残留 temp:%s", e.Name())
		}
	}
}

func TestJournalAppendAndMonthlyRolling(t *testing.T) {
	s := newStore(t)
	c1 := model.Change{ID: "chg_20260620T103200Z_aaaaaaaaaaaaaaaa", Nodes: []string{"a.go#F"},
		At: time.Date(2026, 6, 20, 10, 32, 0, 0, time.UTC), What: "w", Why: "y"}
	c2 := model.Change{ID: "chg_20260704T000100Z_bbbbbbbbbbbbbbbb", Nodes: []string{"a.go#F"},
		At: time.Date(2026, 7, 4, 0, 1, 0, 0, time.UTC), What: "w2", Why: "y2"}
	for _, c := range []model.Change{c1, c2} {
		if err := s.AppendChange(c); err != nil {
			t.Fatalf("AppendChange: %v", err)
		}
	}
	for _, f := range []string{"2026-06.jsonl", "2026-07.jsonl"} {
		if _, err := os.Stat(filepath.Join(s.Dir(), "journal", f)); err != nil {
			t.Errorf("按月分片缺失 %s: %v", f, err)
		}
	}
	got, stats, err := s.LoadJournal()
	if err != nil {
		t.Fatalf("LoadJournal: %v", err)
	}
	if len(got) != 2 || stats.BadLines != 0 {
		t.Fatalf("LoadJournal = %d 条(bad=%d), want 2", len(got), stats.BadLines)
	}
	if got[0].ID != c1.ID || got[1].ID != c2.ID {
		t.Errorf("排序错:%v", []string{got[0].ID, got[1].ID})
	}
}

// TestJournalReadContract 读端契约三 fixture(impl §10):乱序/重复行/坏行。
func TestJournalReadContract(t *testing.T) {
	s := newStore(t)
	line := func(id, at string) string {
		return `{"id":"` + id + `","nodes":["a.go#F"],"at":"` + at + `","what":"w","why":"y"}`
	}
	// union 合并产物:乱序、整行重复、坏行(冲突残留/半行)、同 ID 异内容。
	content := strings.Join([]string{
		line("chg_20260703T000000Z_cccccccccccccccc", "2026-07-03T00:00:00Z"),
		line("chg_20260701T000000Z_aaaaaaaaaaaaaaaa", "2026-07-01T00:00:00Z"), // 乱序
		line("chg_20260703T000000Z_cccccccccccccccc", "2026-07-03T00:00:00Z"), // 整行重复
		"<<<<<<< HEAD",       // 冲突残留
		`{"id":"chg_2026070`, // 断电半行
		`{"not":"a change"}`, // 缺 id
		`{"id":"chg_20260702T000000Z_dddddddddddddddd","nodes":["a.go#F"],"at":"2026-07-02T00:00:00Z","what":"版本A","why":"y"}`,
		`{"id":"chg_20260702T000000Z_dddddddddddddddd","nodes":["a.go#F"],"at":"2026-07-02T00:00:00Z","what":"版本B","why":"y"}`, // 同 ID 异内容
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(s.Dir(), "journal", "2026-07.jsonl"), []byte(content), 0o644); err != nil {
		t.Fatalf("写 fixture: %v", err)
	}

	got, stats, err := s.LoadJournal()
	if err != nil {
		t.Fatalf("LoadJournal: %v", err)
	}
	if stats.BadLines != 3 {
		t.Errorf("BadLines = %d, want 3", stats.BadLines)
	}
	if stats.DupDropped != 1 {
		t.Errorf("DupDropped = %d, want 1", stats.DupDropped)
	}
	if len(stats.ConflictIDs) != 1 || stats.ConflictIDs[0] != "chg_20260702T000000Z_dddddddddddddddd" {
		t.Errorf("ConflictIDs = %v", stats.ConflictIDs)
	}
	// 同 ID 双份保留 + 其余 2 条 = 4;且按 at 升序。
	if len(got) != 4 {
		t.Fatalf("len = %d, want 4(同 ID 双份保留)", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].At.Before(got[i-1].At) {
			t.Errorf("未按 at 排序:%v 在 %v 后", got[i].At, got[i-1].At)
		}
	}
}

func TestWriterLock(t *testing.T) {
	s := newStore(t)
	release, err := s.AcquireWriterLock()
	if err != nil {
		t.Fatalf("首个锁应成功: %v", err)
	}
	// 同进程再拿:flock 对同一 fd 才可重入,这里是新 fd,应被挡。
	if _, err := s.AcquireWriterLock(); !errors.Is(err, ErrLocked) {
		t.Errorf("第二个锁应报 ErrLocked,got %v", err)
	}
	release()
	release2, err := s.AcquireWriterLock()
	if err != nil {
		t.Errorf("释放后应可再取: %v", err)
	} else {
		release2()
	}
}

func TestSemanticLockIsIndependentAndExclusive(t *testing.T) {
	s := newStore(t)
	releaseWriter, err := s.AcquireWriterLock()
	if err != nil {
		t.Fatal(err)
	}
	defer releaseWriter()
	releaseSemantic, err := s.AcquireSemanticLock()
	if err != nil {
		t.Fatalf("serve writer lock 不应阻止 semantic lock: %v", err)
	}
	other, err := Open(s.RepoRoot())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.AcquireSemanticLock(); !errors.Is(err, ErrSemanticLocked) {
		t.Fatalf("第二个 semantic lock=%v, want ErrSemanticLocked", err)
	}
	releaseSemantic()
	releaseAgain, err := other.AcquireSemanticLock()
	if err != nil {
		t.Fatalf("释放后 semantic lock: %v", err)
	}
	releaseAgain()
}

func TestCheckedPrivateStreamKeepsOldFileWhenValidationFails(t *testing.T) {
	s := newStore(t)
	if err := s.WritePrivateKnowledgeFile("local/derived.bin", []byte("old")); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("generation changed")
	err := s.WritePrivateKnowledgeFileStreamChecked("local/derived.bin", func(w io.Writer) error {
		_, err := io.WriteString(w, "new")
		return err
	}, func() error { return wantErr })
	if !errors.Is(err, wantErr) {
		t.Fatalf("checked write error=%v", err)
	}
	got, err := s.ReadKnowledgeFile("local/derived.bin")
	if err != nil || string(got) != "old" {
		t.Fatalf("old generation not preserved: %q err=%v", got, err)
	}
}

func TestEnsureGitFiles(t *testing.T) {
	s := newStore(t)
	if err := s.EnsureGitFiles(); err != nil {
		t.Fatalf("EnsureGitFiles: %v", err)
	}
	if !s.GitFilesOK() {
		t.Fatal("生成后 GitFilesOK 应为 true")
	}
	// 用户删了 union 行 → 幂等补齐(否则第一次分支合并 journal 就出冲突标记)。
	ga := filepath.Join(s.Dir(), ".gitattributes")
	if err := os.WriteFile(ga, []byte("# 用户自己的行\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if s.GitFilesOK() {
		t.Fatal("缺行时 GitFilesOK 应为 false")
	}
	if err := s.EnsureGitFiles(); err != nil {
		t.Fatalf("EnsureGitFiles 补齐: %v", err)
	}
	data, _ := os.ReadFile(ga)
	if !strings.Contains(string(data), "# 用户自己的行") || !strings.Contains(string(data), "merge=union") {
		t.Errorf("补齐应保留用户行并加回缺行:\n%s", data)
	}
}

func TestConfig(t *testing.T) {
	s := newStore(t)
	cfg, err := s.EnsureConfig()
	if err != nil {
		t.Fatalf("EnsureConfig: %v", err)
	}
	want := DerivePort(s.RepoRoot())
	if cfg.Port != want || want < 18000 || want >= 20000 {
		t.Errorf("Port = %d, want %d(区间 [18000,20000))", cfg.Port, want)
	}
	// 用户手改后 EnsureConfig 不覆盖。
	data, _ := os.ReadFile(filepath.Join(s.Dir(), "config.yaml"))
	custom := strings.Replace(string(data), "port:", "port: 19999 #", 1)
	if err := os.WriteFile(filepath.Join(s.Dir(), "config.yaml"), []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg2, err := s.EnsureConfig()
	if err != nil {
		t.Fatalf("EnsureConfig(已存在): %v", err)
	}
	if cfg2.Port != 19999 {
		t.Errorf("用户改的端口被覆盖:%d", cfg2.Port)
	}
}

func TestLoadConfigFailsClosedOnInvalidRuntimeValues(t *testing.T) {
	for _, fixture := range []string{
		"schema: 99\nport: 18000\n",
		"schema: 1\nport: 0\n",
		"schema: 1\nport: 18000\ninclude: ['[bad']\n",
		"schema: 1\nport: 18000\nexclude: ['../escape']\n",
		"schema: 1\nport: 18000\nscout: surprise\n",
		"schema: 1\nport: 18000\nextensions: ['../go']\n",
	} {
		s := newStore(t)
		if err := s.WriteKnowledgeFile("config.yaml", []byte(fixture)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.LoadConfig(); err == nil {
			t.Fatalf("非法 config 必须 fail closed:\n%s", fixture)
		}
	}
}

// TestCacheReload 惰性重载(impl §10):目录清单对账捕捉文件增删 +
// journal 切分支 fixture(同名月份文件内容整体替换后 history 随之切换)。
func TestCacheReload(t *testing.T) {
	s := newStore(t)
	c := NewCache(s)

	pathA := s.ShardPathFor("a.go")
	if err := s.SaveShard(pathA, sampleShard(), nil); err != nil {
		t.Fatal(err)
	}
	branch1 := `{"id":"chg_20260701T000000Z_aaaaaaaaaaaaaaaa","nodes":["a.go#F"],"at":"2026-07-01T00:00:00Z","what":"分支一的决策","why":"y"}` + "\n"
	jpath := filepath.Join(s.Dir(), "journal", "2026-07.jsonl")
	if err := os.WriteFile(jpath, []byte(branch1), 0o644); err != nil {
		t.Fatal(err)
	}

	rep, err := c.Refresh()
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if len(rep.Added) != 2 { // 分片 + journal
		t.Errorf("首次 Refresh Added = %v", rep.Added)
	}
	if changes, _ := c.Journal(); len(changes) != 1 || changes[0].What != "分支一的决策" {
		t.Fatalf("journal 初始加载失败:%+v", changes)
	}

	// 模拟 git 切分支:分片被删、新分片出现、journal 同名文件内容整体替换。
	if err := os.Remove(pathA); err != nil {
		t.Fatal(err)
	}
	pathB := s.ShardPathFor("b.go")
	shB := &Shard{Schema: 1, Nodes: []model.Node{{
		ID: "b.go", Level: model.LevelFile,
		Anchor: model.Anchor{File: "b.go"}, Status: model.StatusUndigested, Since: time.Now().UTC(),
	}}}
	if err := s.SaveShard(pathB, shB, nil); err != nil {
		t.Fatal(err)
	}
	branch2 := `{"id":"chg_20260702T000000Z_bbbbbbbbbbbbbbbb","nodes":["b.go"],"at":"2026-07-02T00:00:00Z","what":"分支二的决策","why":"y"}` + "\n"
	// checkout 会给同名文件新 mtime;为防同秒 mtime 盲区,内容长度也不同(size 兜底)。
	if err := os.WriteFile(jpath, []byte(branch2), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := c.Refresh(); err != nil {
		t.Fatalf("Refresh 2: %v", err)
	}
	if _, ok := c.Shards()["tree/a.go.yaml"]; ok {
		t.Error("被删分片仍在缓存(跨分支幽灵知识)")
	}
	if _, ok := c.Shards()["tree/b.go.yaml"]; !ok {
		t.Error("新增分片未加载")
	}
	changes, _ := c.Journal()
	if len(changes) != 1 || changes[0].What != "分支二的决策" {
		t.Errorf("journal 未随分支切换(幽灵决策链):%+v", changes)
	}
}

func TestSrcRelOfShard(t *testing.T) {
	s := newStore(t)
	tests := []struct {
		path string
		want string
	}{
		{s.ShardPathFor("internal/auth/login.go"), "internal/auth/login.go"},
		{s.DirShardPathFor("internal/auth"), ""},
		{s.ProjectShardPath(), ""},
	}
	for _, tt := range tests {
		if got := s.SrcRelOfShard(tt.path); got != tt.want {
			t.Errorf("SrcRelOfShard(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}
