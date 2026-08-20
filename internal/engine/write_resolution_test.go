package engine

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/zdypro888/iknowledge/internal/model"
)

const writeResolutionReceiverSource = "package sample\n\n" +
	"type Worker struct{}\n\n" +
	"func (w *Worker) Run() {}\n"

func TestRecordChangeCanonicalizesUnambiguousNodeSyntax(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"a.go": writeResolutionReceiverSource})

	out, err := e.RecordChange(ChangeArgs{
		Nodes: []string{".\\a.go#(*Worker).Run()"},
		What:  "调整 worker 执行路径",
		Why:   "统一入口",
	}, "session", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.go#Worker.Run") || !strings.Contains(out, "规范化") {
		t.Fatalf("回执没有呈现规范 ID 与规范化:\n%s", out)
	}
	history := e.rt.ix.History("a.go#Worker.Run")
	if len(history) != 1 || !reflect.DeepEqual(history[0].Nodes, []string{"a.go#Worker.Run"}) {
		t.Fatalf("journal nodes = %v, want canonical receiver node", history)
	}
	if e.rt.ix.Node("a.go#(*Worker).Run()") != nil {
		t.Fatal("call-site spelling must not become a truth node")
	}
}

func TestRecordChangeTargetedSourceRecovery(t *testing.T) {
	t.Run("exact source symbol wins before loose methods", func(t *testing.T) {
		e, repo := initEngine(t, map[string]string{"a.go": "package sample\n"})
		writeFiles(t, repo, map[string]string{"a.go": "package sample\n\n" +
			"type A struct{}\nfunc (A) Run() {}\n" +
			"type B struct{}\nfunc (B) Run() {}\n" +
			"func Run() {}\n"})

		out, err := e.RecordChange(ChangeArgs{
			Nodes: []string{"./a.go#Run()"},
			What:  "新增顶层 Run",
			Why:   "提供统一入口",
		}, "session", "codex")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "created: [a.go#Run]") {
			t.Fatalf("没有精确创建顶层符号:\n%s", out)
		}
		if e.rt.ix.Node("a.go#Run") == nil {
			t.Fatal("targeted source recovery did not add exact symbol")
		}
		if e.rt.ix.Node("a.go#A.Run") != nil || e.rt.ix.Node("a.go#B.Run") != nil {
			t.Fatal("targeted recovery unexpectedly reconciled unrelated symbols")
		}
	})

	t.Run("explicit new file may create file node", func(t *testing.T) {
		e, repo := initEngine(t, map[string]string{"a.go": "package sample\n"})
		writeFiles(t, repo, map[string]string{
			"new.go": "package sample\n\nfunc Added() {}\n",
		})

		out, err := e.RecordChange(ChangeArgs{
			Nodes: []string{"./new.go"},
			What:  "新增实现文件",
			Why:   "拆分职责",
		}, "session", "codex")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "created: [new.go]") {
			t.Fatalf("显式文件节点没有增量落锚:\n%s", out)
		}
		ref := e.rt.ix.Node("new.go")
		if ref == nil || ref.Node.Level != model.LevelFile || ref.Node.Anchor.Hash == "" {
			t.Fatalf("new.go file node = %#v", ref)
		}
	})

	t.Run("old shard missing its file node is repaired narrowly", func(t *testing.T) {
		e, _ := initEngine(t, map[string]string{"a.go": writeResolutionReceiverSource})
		path := e.Store.ShardPathFor("a.go")
		shard, raw, err := e.Store.LoadShard(path)
		if err != nil {
			t.Fatal(err)
		}
		var kept []model.Node
		for _, node := range shard.Nodes {
			if node.ID != "a.go" {
				kept = append(kept, node)
			}
		}
		shard.Nodes = kept
		if err := e.Store.SaveShard(path, shard, raw); err != nil {
			t.Fatal(err)
		}
		if err := e.EnsureRuntime(); err != nil {
			t.Fatal(err)
		}
		if e.rt.ix.Node("a.go") != nil {
			t.Fatal("test setup failed: file node still indexed")
		}

		if _, err := e.RecordChange(ChangeArgs{
			Nodes: []string{"a.go"},
			What:  "补记文件级调整",
			Why:   "旧库缺失文件骨架",
		}, "session", "codex"); err != nil {
			t.Fatal(err)
		}
		if ref := e.rt.ix.Node("a.go"); ref == nil || ref.Node.Level != model.LevelFile {
			t.Fatalf("missing file skeleton was not recovered: %#v", ref)
		}
		if e.rt.ix.Node("a.go#Worker.Run") == nil {
			t.Fatal("narrow recovery damaged existing symbol nodes")
		}
	})

	t.Run("unrelated quarantined shard cannot crash recovery", func(t *testing.T) {
		e, repo := initEngine(t, map[string]string{"a.go": "package sample\n"})
		if err := e.Store.WriteKnowledgeFile("tree/bad.go.yaml", []byte("schema: 1\nnodes: [\n")); err != nil {
			t.Fatal(err)
		}
		if err := e.Sync(); err != nil {
			t.Fatal(err)
		}
		writeFiles(t, repo, map[string]string{"new.go": "package sample\n\nfunc Added() {}\n"})
		if _, err := e.RecordChange(ChangeArgs{
			Nodes: []string{"new.go#Added"}, What: "增加隔离分片旁的新符号", Why: "验证新节点恢复",
		}, "session", "codex"); err != nil {
			t.Fatalf("无关隔离分片不应阻断新节点: %v", err)
		}
		if e.rt.ix.Node("new.go#Added") == nil {
			t.Fatal("新节点未落锚")
		}
	})
}

func TestRecordChangeRejectsWrongOrAmbiguousAnchorsWithCandidates(t *testing.T) {
	t.Run("typo is never silently attached to file", func(t *testing.T) {
		e, _ := initEngine(t, map[string]string{"payments.go": "package sample\n\n" +
			"func ProcessPayment() {}\nfunc ProcessRefund() {}\n"})
		before := len(e.rt.ix.Changes())
		_, err := e.RecordChange(ChangeArgs{
			Nodes: []string{"payments.go#ProcesPayment"},
			What:  "修改支付",
			Why:   "修复问题",
		}, "session", "codex")
		var kbe *KBError
		if !errors.As(err, &kbe) || kbe.Code != "NODE_NOT_FOUND" {
			t.Fatalf("error = %v, want NODE_NOT_FOUND", err)
		}
		if !strings.Contains(kbe.Hint, "payments.go#ProcessPayment") ||
			!strings.Contains(kbe.Hint, "不会自动降级挂到文件") {
			t.Fatalf("error does not provide safe corrective candidate: %#v", kbe)
		}
		if len(e.rt.ix.Changes()) != before || e.rt.ix.Node("payments.go#ProcesPayment") != nil {
			t.Fatal("rejected typo mutated truth")
		}
	})

	t.Run("split lineage requires an explicit current heir", func(t *testing.T) {
		e, _ := initEngine(t, map[string]string{"a.go": "package sample\n\n" +
			"func Left() {}\nfunc Right() {}\n"})
		path := e.Store.ShardPathFor("a.go")
		shard, raw, err := e.Store.LoadShard(path)
		if err != nil {
			t.Fatal(err)
		}
		for i := range shard.Nodes {
			switch shard.Nodes[i].ID {
			case "a.go#Left", "a.go#Right":
				shard.Nodes[i].Lineage = []string{"a.go#Old"}
			}
		}
		if err := e.Store.SaveShard(path, shard, raw); err != nil {
			t.Fatal(err)
		}
		if err := e.EnsureRuntime(); err != nil {
			t.Fatal(err)
		}

		_, err = e.RecordChange(ChangeArgs{
			Nodes: []string{"a.go#Old"},
			What:  "修改拆分后的逻辑",
			Why:   "修复问题",
		}, "session", "codex")
		var kbe *KBError
		if !errors.As(err, &kbe) || kbe.Code != "NODE_NOT_FOUND" {
			t.Fatalf("error = %v, want split ambiguity", err)
		}
		if !strings.Contains(kbe.Hint, "a.go#Left") || !strings.Contains(kbe.Hint, "a.go#Right") {
			t.Fatalf("split candidates missing: %#v", kbe)
		}
		if len(e.rt.ix.History("a.go#Left")) != 0 || len(e.rt.ix.History("a.go#Right")) != 0 {
			t.Fatal("ambiguous split write silently chose an heir")
		}
	})
}

func TestRecordChangeRemapRejectsSplitLineageOnBothSides(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"a.go": "package sample\n\n" +
		"func Source() {}\nfunc Left() {}\nfunc Right() {}\n"})
	path := e.Store.ShardPathFor("a.go")
	shard, raw, err := e.Store.LoadShard(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range shard.Nodes {
		if shard.Nodes[i].ID == "a.go#Left" || shard.Nodes[i].ID == "a.go#Right" {
			shard.Nodes[i].Lineage = []string{"a.go#Old"}
		}
	}
	if err := e.Store.SaveShard(path, shard, raw); err != nil {
		t.Fatal(err)
	}
	if err := e.Sync(); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		from string
		to   string
	}{
		{name: "from", from: "a.go#Old", to: "a.go#Right"},
		{name: "to", from: "a.go#Source", to: "a.go#Old"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(e.rt.ix.Changes())
			_, err := e.RecordChange(ChangeArgs{
				Nodes: []string{"a.go#Source"}, What: "尝试歧义迁移", Why: "验证严格写解析",
				Remaps: []model.Remap{{From: tc.from, To: []string{tc.to}}},
			}, "session", "codex")
			var kbe *KBError
			if !errors.As(err, &kbe) || kbe.Code != "NODE_NOT_FOUND" ||
				!strings.Contains(kbe.Hint, "a.go#Left") || !strings.Contains(kbe.Hint, "a.go#Right") {
				t.Fatalf("split remap error=%#v", err)
			}
			if len(e.rt.ix.Changes()) != before {
				t.Fatal("拒绝的 split remap 产生了 journal 写入")
			}
		})
	}
}

func TestRecordChangePersistsExecutedCanonicalRemap(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"a.go": "package sample\n\n" +
		"func Old() {}\nfunc NewA() {}\nfunc NewB() {}\n"})
	if _, err := e.Remember(RememberArgs{
		Node: "a.go#Old",
		Entries: []RememberEntry{{
			Kind: "contract", Text: "旧入口契约",
		}},
	}, "session", "codex"); err != nil {
		t.Fatal(err)
	}
	old := e.rt.ix.Node("a.go#Old")
	if old == nil || len(old.Node.Entries) != 1 {
		t.Fatalf("source entries = %#v", old)
	}
	entryID := old.Node.Entries[0].ID

	if _, err := e.RecordChange(ChangeArgs{
		Nodes: []string{"a.go#NewA", "a.go#NewB"},
		What:  "拆分旧入口",
		Why:   "隔离职责",
		Remaps: []model.Remap{{
			From: ".\\a.go#Old()",
			To:   []string{"./a.go#NewA()", ".\\a.go#NewB()"},
			Entries: map[string]string{
				entryID: ".\\a.go#NewB()",
			},
		}},
	}, "session", "codex"); err != nil {
		t.Fatal(err)
	}

	changes := e.rt.ix.Changes()
	if len(changes) == 0 {
		t.Fatal("record_change did not append journal truth")
	}
	got := changes[len(changes)-1]
	want := []model.Remap{{
		From: "a.go#Old",
		To:   []string{"a.go#NewA", "a.go#NewB"},
		Entries: map[string]string{
			entryID: "a.go#NewB",
		},
	}}
	if !reflect.DeepEqual(got.Remaps, want) {
		t.Fatalf("journal remaps = %#v, want executed canonical remaps %#v", got.Remaps, want)
	}
	if err := validateImportedChange(got); err != nil {
		t.Fatalf("locally generated journal truth is not bundle-importable: %v", err)
	}
	if ref := e.rt.ix.Node("a.go#NewB"); ref == nil || len(ref.Node.Entries) != 1 || ref.Node.Entries[0].ID != entryID {
		t.Fatalf("canonical entry destination did not match execution: %#v", ref)
	}
}

func TestExplicitEmptySymbolNeverFallsBackToFileNode(t *testing.T) {
	assertInvalid := func(t *testing.T, err error) {
		t.Helper()
		var kbe *KBError
		if !errors.As(err, &kbe) || kbe.Code != "INVALID_ARGUMENT" || !strings.Contains(kbe.Msg, "符号名为空") {
			t.Fatalf("error = %#v, want explicit empty-symbol rejection", err)
		}
	}
	for _, query := range []string{"a.go#", "./a.go#()"} {
		t.Run(strings.ReplaceAll(query, "/", "_"), func(t *testing.T) {
			t.Run("remember", func(t *testing.T) {
				e, _ := initEngine(t, map[string]string{"a.go": "package sample\n\nfunc Existing() {}\n"})
				before := len(e.rt.ix.Node("a.go").Node.Entries)
				_, err := e.Remember(RememberArgs{Node: query, Entries: []RememberEntry{{
					Kind: "contract", Text: "不得落到文件",
				}}}, "session", "codex")
				assertInvalid(t, err)
				if got := len(e.rt.ix.Node("a.go").Node.Entries); got != before {
					t.Fatalf("file entries changed: before=%d after=%d", before, got)
				}
			})

			t.Run("record_change", func(t *testing.T) {
				e, _ := initEngine(t, map[string]string{"a.go": "package sample\n\nfunc Existing() {}\n"})
				before := len(e.rt.ix.Changes())
				_, err := e.RecordChange(ChangeArgs{
					Nodes: []string{query}, What: "错误空符号", Why: "验证严格写入",
				}, "session", "codex")
				assertInvalid(t, err)
				if got := len(e.rt.ix.Changes()); got != before {
					t.Fatalf("rejected change appended journal: before=%d after=%d", before, got)
				}
			})

			t.Run("task_touching", func(t *testing.T) {
				e, _ := initEngine(t, map[string]string{"a.go": "package sample\n\nfunc Existing() {}\n"})
				_, err := e.Task(TaskArgs{Action: "start", WIP: model.WIP{
					Task: "错误空符号任务", Touching: []string{query},
				}}, "session", "codex")
				assertInvalid(t, err)
				wips, loadErr := e.Store.LoadWIPs()
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				if len(wips) != 0 {
					t.Fatalf("rejected touching persisted WIP: %#v", wips)
				}
			})
		})
	}
}

func TestTaskTouchingNormalizationAndSafeArchiveFallback(t *testing.T) {
	t.Run("known receiver is stored canonically", func(t *testing.T) {
		e, _ := initEngine(t, map[string]string{"a.go": writeResolutionReceiverSource})
		out, err := e.Task(TaskArgs{Action: "start", WIP: model.WIP{
			Task:     "调整 worker",
			Touching: []string{"./a.go#(*Worker).Run()"},
		}}, "session", "codex")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "touching") || !strings.Contains(out, "a.go#Worker.Run") {
			t.Fatalf("normalization warning missing:\n%s", out)
		}
		wips, err := e.Store.LoadWIPs()
		if err != nil {
			t.Fatal(err)
		}
		if len(wips) != 1 || !reflect.DeepEqual(wips[0].Touching, []string{"a.go#Worker.Run"}) {
			t.Fatalf("stored touching = %#v", wips)
		}
	})

	t.Run("source-confirmed new symbol explicitly falls back to its file", func(t *testing.T) {
		e, repo := initEngine(t, map[string]string{"a.go": "package sample\n\nfunc Existing() {}\n"})
		start, err := e.Task(TaskArgs{Action: "start", WIP: model.WIP{
			Task:     "增加 Future",
			Touching: []string{"a.go#Future()"},
		}}, "session", "codex")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(start, "显式计划目标保留") {
			t.Fatalf("future target warning missing:\n%s", start)
		}
		writeFiles(t, repo, map[string]string{
			"a.go": "package sample\n\nfunc Existing() {}\nfunc Future() {}\n",
		})

		out, err := e.Task(TaskArgs{Action: "complete"}, "session", "codex")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "显式降级到其文件节点 a.go") {
			t.Fatalf("archive fallback was not explicit:\n%s", out)
		}
		changes := e.rt.ix.Changes()
		if len(changes) == 0 || !reflect.DeepEqual(changes[len(changes)-1].Nodes, []string{"a.go"}) {
			t.Fatalf("archived nodes = %#v, want containing file", changes)
		}
	})

	t.Run("unconfirmed typo never falls back to a similar file symbol", func(t *testing.T) {
		e, _ := initEngine(t, map[string]string{"a.go": "package sample\n\nfunc Future() {}\n"})
		if _, err := e.Task(TaskArgs{Action: "start", WIP: model.WIP{
			Task:     "修改 future",
			Touching: []string{"a.go#Futuer"},
		}}, "session", "codex"); err != nil {
			t.Fatal(err)
		}
		out, err := e.Task(TaskArgs{Action: "complete"}, "session", "codex")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "未归档到其他源码节点") ||
			!strings.Contains(out, "显式落到项目节点") {
			t.Fatalf("unsafe fallback was not disclosed:\n%s", out)
		}
		changes := e.rt.ix.Changes()
		if len(changes) == 0 || !reflect.DeepEqual(changes[len(changes)-1].Nodes, []string{model.ProjectNodeID}) {
			t.Fatalf("typo archive nodes = %#v", changes)
		}
	})
}
