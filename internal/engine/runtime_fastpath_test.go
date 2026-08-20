package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zdypro888/iknowledge/internal/model"
	"github.com/zdypro888/iknowledge/internal/parser"
)

type failOnceGoParser struct {
	inner  parser.Parser
	failed bool
}

type cancelSecondGoParser struct {
	inner  parser.Parser
	cancel context.CancelFunc
	calls  int
}

type cancelOnErrContext struct {
	calls    int
	cancelAt int
}

func (c *cancelOnErrContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *cancelOnErrContext) Done() <-chan struct{}       { return nil }
func (c *cancelOnErrContext) Value(any) any               { return nil }
func (c *cancelOnErrContext) Err() error {
	c.calls++
	if c.calls >= c.cancelAt {
		return context.Canceled
	}
	return nil
}

func (p *cancelSecondGoParser) Language() string     { return p.inner.Language() }
func (p *cancelSecondGoParser) Extensions() []string { return p.inner.Extensions() }
func (p *cancelSecondGoParser) Parse(path string, src []byte) ([]parser.Symbol, error) {
	return p.inner.Parse(path, src)
}
func (p *cancelSecondGoParser) ParseContext(ctx context.Context, path string, src []byte) ([]parser.Symbol, error) {
	p.calls++
	if p.calls == 2 {
		p.cancel()
		return nil, context.Canceled
	}
	return p.inner.Parse(path, src)
}

func (p *failOnceGoParser) Language() string     { return p.inner.Language() }
func (p *failOnceGoParser) Extensions() []string { return p.inner.Extensions() }
func (p *failOnceGoParser) Parse(path string, src []byte) ([]parser.Symbol, error) {
	if !p.failed {
		p.failed = true
		return nil, errors.New("transient parser failure")
	}
	return p.inner.Parse(path, src)
}

func TestSyncFastPathSkipsIndexBuildAndSourceReconcile(t *testing.T) {
	e, _ := initEngine(t, map[string]string{
		"a.go": "package a\n\nfunc F() int { return 1 }\n",
	})
	if _, err := e.Remember(RememberArgs{
		Node: "a.go#F", Entries: []RememberEntry{{Kind: model.KindContract, Text: "F 返回当前仓库的稳定状态码"}},
	}, "session", "codex"); err != nil {
		t.Fatal(err)
	}
	if err := e.Sync(); err != nil { // 先收敛写路径的 cache/source 基线。
		t.Fatal(err)
	}

	wantIndex := e.rt.ix
	wantBuilds := e.rt.syncIndexBuilds
	wantReconciles := e.rt.syncReconcileRuns
	wantSemanticVersion := e.rt.semanticSourceVersion
	for range 5 {
		if err := e.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	if e.rt.ix != wantIndex {
		t.Fatal("无变化 Sync 替换了 index generation")
	}
	if got := e.rt.syncIndexBuilds; got != wantBuilds {
		t.Fatalf("无变化 Sync 仍在 Build: before=%d after=%d", wantBuilds, got)
	}
	if got := e.rt.syncReconcileRuns; got != wantReconciles {
		t.Fatalf("无变化 Sync 仍在解析源码对账: before=%d after=%d", wantReconciles, got)
	}
	if got := e.rt.semanticSourceVersion; got != wantSemanticVersion {
		t.Fatalf("无变化 Sync 误使 semantic source 换代: before=%d after=%d", wantSemanticVersion, got)
	}
}

func TestSyncFastPathInvalidationMatrix(t *testing.T) {
	e, repo := initEngine(t, map[string]string{
		"a.go": "package a\n\nfunc F() int { return 1 }\n",
	})
	if _, err := e.Remember(RememberArgs{
		Node: "a.go#F", Entries: []RememberEntry{{Kind: model.KindContract, Text: "F 是流程的入口状态码"}},
	}, "session", "codex"); err != nil {
		t.Fatal(err)
	}
	if err := e.Sync(); err != nil {
		t.Fatal(err)
	}

	t.Run("WIP only refreshes runtime snapshot", func(t *testing.T) {
		builds, reconciles := e.rt.syncIndexBuilds, e.rt.syncReconcileRuns
		semanticVersion := e.rt.semanticSourceVersion
		if err := e.Store.SaveWIP(model.WIP{
			Task: "检查快路径", Owner: "codex@external", Updated: time.Date(2026, 8, 20, 1, 2, 3, 0, time.UTC),
		}); err != nil {
			t.Fatal(err)
		}
		if err := e.Sync(); err != nil {
			t.Fatal(err)
		}
		if len(e.rt.wips) != 1 || e.rt.wips[0].Task != "检查快路径" {
			t.Fatalf("WIP 外部变更未加载:%+v", e.rt.wips)
		}
		if e.rt.syncIndexBuilds != builds || e.rt.syncReconcileRuns != reconciles {
			t.Fatalf("WIP 变更误触发重工: builds %d->%d reconciles %d->%d",
				builds, e.rt.syncIndexBuilds, reconciles, e.rt.syncReconcileRuns)
		}
		if e.rt.semanticSourceVersion != semanticVersion {
			t.Fatal("WIP 变更不应使 semantic source 换代")
		}
	})

	t.Run("flow rebuilds index but does not parse sources", func(t *testing.T) {
		builds, reconciles := e.rt.syncIndexBuilds, e.rt.syncReconcileRuns
		semanticVersion := e.rt.semanticSourceVersion
		if err := e.Store.SaveFlow(model.Flow{
			ID: "flow:fast-path", Title: "快路径流程", Since: time.Date(2026, 8, 20, 1, 2, 3, 0, time.UTC),
			Steps: []model.FlowStep{{Node: "a.go#F"}},
		}); err != nil {
			t.Fatal(err)
		}
		if err := e.Sync(); err != nil {
			t.Fatal(err)
		}
		if e.rt.ix.Flow("flow:fast-path") == nil {
			t.Fatal("flow 外部变更未进入索引")
		}
		if e.rt.syncIndexBuilds != builds+1 {
			t.Fatalf("flow 变更应且只应 Build 一次: %d -> %d", builds, e.rt.syncIndexBuilds)
		}
		if e.rt.syncReconcileRuns != reconciles {
			t.Fatalf("flow 变更不应重新解析源码: %d -> %d", reconciles, e.rt.syncReconcileRuns)
		}
		if e.rt.semanticSourceVersion != semanticVersion+1 {
			t.Fatal("flow 知识变更应使 semantic source 换代")
		}
	})

	t.Run("journal rebuilds index without source reconcile", func(t *testing.T) {
		builds, reconciles := e.rt.syncIndexBuilds, e.rt.syncReconcileRuns
		semanticVersion := e.rt.semanticSourceVersion
		if err := e.Store.AppendChange(model.Change{
			ID: "chg_20260820T010203Z_0123456789abcdef", Nodes: []string{"a.go#F"},
			At: time.Date(2026, 8, 20, 1, 2, 3, 0, time.UTC), What: "外部 journal 变更", Why: "验证重载",
		}); err != nil {
			t.Fatal(err)
		}
		if err := e.Sync(); err != nil {
			t.Fatal(err)
		}
		if e.rt.ix.ChangeByID("chg_20260820T010203Z_0123456789abcdef") == nil {
			t.Fatal("journal 外部变更未进入索引")
		}
		if e.rt.syncIndexBuilds != builds+1 || e.rt.syncReconcileRuns != reconciles {
			t.Fatalf("journal 应仅 Build 一次: builds %d->%d reconciles %d->%d",
				builds, e.rt.syncIndexBuilds, reconciles, e.rt.syncReconcileRuns)
		}
		if e.rt.semanticSourceVersion != semanticVersion+1 {
			t.Fatal("journal 变更应使 semantic source 换代")
		}
	})

	t.Run("same size and restored mtime source still reconciles", func(t *testing.T) {
		path := filepath.Join(repo, "a.go")
		before, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		builds, reconciles := e.rt.syncIndexBuilds, e.rt.syncReconcileRuns
		semanticVersion := e.rt.semanticSourceVersion
		changed := []byte("package a\n\nfunc F() int { return 2 }\n")
		if err := os.WriteFile(path, changed, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
			t.Fatal(err)
		}
		if err := e.Sync(); err != nil {
			t.Fatal(err)
		}
		ref := e.rt.ix.Node("a.go#F")
		if ref == nil || ref.Node.Status != model.StatusSuspect {
			t.Fatalf("同尺寸+恢复 mtime 的源码变更未降 suspect:%+v", ref)
		}
		if e.rt.syncReconcileRuns != reconciles+1 {
			t.Fatalf("源码变更应对账一次: %d -> %d", reconciles, e.rt.syncReconcileRuns)
		}
		if e.rt.syncIndexBuilds != builds+1 {
			t.Fatalf("状态迁移应只重建一次: %d -> %d", builds, e.rt.syncIndexBuilds)
		}
		if e.rt.semanticSourceVersion != semanticVersion+1 {
			t.Fatal("锚状态变更应使 semantic source 换代")
		}

		// reconcile 自己的 tree 原子写已经推进 cache 基线;下轮立即走快路径。
		builds, reconciles = e.rt.syncIndexBuilds, e.rt.syncReconcileRuns
		if err := e.Sync(); err != nil {
			t.Fatal(err)
		}
		if e.rt.syncIndexBuilds != builds || e.rt.syncReconcileRuns != reconciles {
			t.Fatal("对账后下一轮未收敛到快路径")
		}
	})
}

func TestConcurrentSyncFastPathPublishesNoExtraGenerations(t *testing.T) {
	e, _ := initEngine(t, map[string]string{
		"a.go": "package a\n\nfunc F() int { return 1 }\n",
	})
	if _, err := e.Remember(RememberArgs{
		Node: "a.go#F", Entries: []RememberEntry{{Kind: model.KindContract, Text: "F 是并发读的稳定契约"}},
	}, "session", "codex"); err != nil {
		t.Fatal(err)
	}
	if err := e.Sync(); err != nil {
		t.Fatal(err)
	}
	builds, reconciles := e.rt.syncIndexBuilds, e.rt.syncReconcileRuns

	errCh := make(chan error, 24)
	for range 24 {
		go func() { errCh <- e.Sync() }()
	}
	for range 24 {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}
	if e.rt.syncIndexBuilds != builds || e.rt.syncReconcileRuns != reconciles {
		t.Fatalf("并发快路径产生了额外 generation: builds %d->%d reconciles %d->%d",
			builds, e.rt.syncIndexBuilds, reconciles, e.rt.syncReconcileRuns)
	}
}

func TestSyncFastPathCannotBypassPreparedTransactionRecovery(t *testing.T) {
	e, _ := initEngine(t, map[string]string{
		"a.go": "package a\n\nfunc F() int { return 1 }\n",
	})
	if err := e.Sync(); err != nil {
		t.Fatal(err)
	}
	rel := "tree/a.go.yaml"
	before, err := e.Store.ReadKnowledgeFile(rel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Store.PrepareTruthTransaction([]string{rel}); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.WriteKnowledgeFile(rel, []byte("schema: 1\nnodes: []\n")); err != nil {
		t.Fatal(err)
	}
	builds := e.rt.syncIndexBuilds
	if err := e.Sync(); err != nil {
		t.Fatal(err)
	}
	after, err := e.Store.ReadKnowledgeFile(rel)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("Sync 快路径跳过了 prepared WAL before-image 恢复")
	}
	if e.rt.ix.Node("a.go#F") == nil {
		t.Fatal("事务恢复后索引仍是半应用世代")
	}
	if e.rt.syncIndexBuilds <= builds {
		t.Fatal("事务恢复后必须强制新 index generation")
	}
}

func TestSyncFastPathRetriesTransientParserFailure(t *testing.T) {
	e, repo := initEngine(t, map[string]string{
		"a.go": "package a\n\nfunc F() int { return 1 }\n",
	})
	if _, err := e.Remember(RememberArgs{
		Node: "a.go#F", Entries: []RememberEntry{{Kind: model.KindContract, Text: "F 返回已保存状态码"}},
	}, "session", "codex"); err != nil {
		t.Fatal(err)
	}
	if err := e.Sync(); err != nil {
		t.Fatal(err)
	}

	e.Reg.Register(&failOnceGoParser{inner: parser.Golang{}})
	if err := os.WriteFile(filepath.Join(repo, "a.go"), []byte("package a\n\nfunc F() int { return 2 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.Sync(); err != nil {
		t.Fatal(err)
	}
	if got := e.rt.ix.Node("a.go#F").Node.Status; got != model.StatusFresh {
		t.Fatalf("解析失败时应保守保留 fresh, got %s", got)
	}
	if e.rt.reconcileSourceReady {
		t.Fatal("解析失败不得发布已对账源码基线")
	}

	if err := e.Sync(); err != nil {
		t.Fatal(err)
	}
	if got := e.rt.ix.Node("a.go#F").Node.Status; got != model.StatusSuspect {
		t.Fatalf("下轮必须重试并发现腐烂, got %s", got)
	}
	if !e.rt.reconcileSourceReady {
		t.Fatal("成功重试后应发布对账基线")
	}
}

func TestSyncCancellationDiscardsSpeculativeReconcileMutations(t *testing.T) {
	e, repo := initEngine(t, map[string]string{
		"a.go": "package a\n\nfunc A() int { return 1 }\n",
		"b.go": "package a\n\nfunc B() int { return 1 }\n",
	})
	for _, node := range []string{"a.go#A", "b.go#B"} {
		if _, err := e.Remember(RememberArgs{
			Node: node, Entries: []RememberEntry{{Kind: model.KindContract, Text: node + " 保留稳定返回值"}},
		}, "session", "codex"); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "a.go"), []byte("package a\n\nfunc A() int { return 2 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "b.go"), []byte("package a\n\nfunc B() int { return 2 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	e.Reg.Register(&cancelSecondGoParser{inner: parser.Golang{}, cancel: cancel})
	if err := e.SyncContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("SyncContext error=%v, want canceled", err)
	}
	if e.rt.cache != nil || e.rt.ix != nil || e.rt.reconcileSourceReady {
		t.Fatal("取消后仍保留了可能半应用的派生快照")
	}
	for _, file := range []string{"a.go", "b.go"} {
		shard, _, err := e.Store.LoadShard(e.Store.ShardPathFor(file))
		if err != nil {
			t.Fatal(err)
		}
		for _, node := range shard.Nodes {
			if node.ID != file && node.Status != model.StatusFresh {
				t.Fatalf("取消请求把未提交状态写入 %s: %+v", file, node)
			}
		}
	}

	e.Reg.Register(parser.Golang{})
	if err := e.Sync(); err != nil {
		t.Fatal(err)
	}
	for _, node := range []string{"a.go#A", "b.go#B"} {
		if got := e.rt.ix.Node(node).Node.Status; got != model.StatusSuspect {
			t.Fatalf("重试后 %s status=%s, want suspect", node, got)
		}
	}
}

func TestSyncCancellationAfterRefreshDoesNotConsumeTruthChange(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"a.go": "package a\n\nfunc A() {}\n"})
	if _, err := e.Remember(RememberArgs{
		Node: "a.go#A", Entries: []RememberEntry{{Kind: model.KindContract, Text: "old contract"}},
	}, "session", "codex"); err != nil {
		t.Fatal(err)
	}
	if err := e.Sync(); err != nil {
		t.Fatal(err)
	}
	versionBefore := e.rt.semanticSourceVersion

	path := e.Store.ShardPathFor("a.go")
	shard, raw, err := e.Store.LoadShard(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range shard.Nodes {
		for j := range shard.Nodes[i].Entries {
			if shard.Nodes[i].Entries[j].Active() {
				shard.Nodes[i].Entries[j].Text = "new contract"
			}
		}
	}
	if err := e.Store.SaveShard(path, shard, raw); err != nil {
		t.Fatal(err)
	}

	// Call the locked transaction directly so Err call #1 is the entry check and
	// #2 is snapshotReconcileSources' first checkpoint: cache.Refresh and index
	// publication have happened, semantic invalidation has not.
	ctx := &cancelOnErrContext{cancelAt: 2}
	e.rt.mu.Lock()
	err = e.reloadLockedContext(ctx)
	e.rt.mu.Unlock()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("reload error=%v, want canceled", err)
	}
	if e.rt.cache != nil || e.rt.ix != nil || e.rt.reconcileSourceReady {
		t.Fatal("取消消费了 RefreshReport 却没有丢弃派生快照")
	}
	if e.rt.semanticSourceVersion != versionBefore {
		t.Fatal("失败事务不应发布半套 semantic generation")
	}
	if err := e.Sync(); err != nil {
		t.Fatal(err)
	}
	if e.rt.semanticSourceVersion <= versionBefore {
		t.Fatal("下轮没有重放 tree 变更并使 semantic source 换代")
	}
	ref := e.rt.ix.Node("a.go#A")
	if ref == nil || len(ref.Node.Entries) == 0 || ref.Node.Entries[0].Text != "new contract" {
		t.Fatalf("下轮未读到持久真相: %+v", ref)
	}
}
