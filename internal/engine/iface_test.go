package engine

import (
	"reflect"
	"strings"
	"testing"
)

// 接口→实现(codegraph 启发的方法集匹配)全场景。
func TestInterfaceImplements(t *testing.T) {
	files := map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.26\n",
		// 双方法接口 + 两个实现(跨包)+ 一个只实现一半的类型。
		"spec/spec.go": `package spec

type Strategy interface {
	Observe(v int)
	Converged() bool
}
`,
		"impl/a.go": `package impl

type Alpha struct{}

func (a *Alpha) Observe(v int) {}
func (a *Alpha) Converged() bool { return true }

type Half struct{}

func (h Half) Observe(v int) {}
`,
		"impl/b.go": `package impl

type Beta struct{}

func (b Beta) Observe(v int) {}
func (b Beta) Converged() bool { return false }
`,
		// 接口分发调用点:局部变量 s 的静态类型无从得知,分发兜底连到实现。
		"run/run.go": `package run

func Drive(observe func()) {
	var s any
	_ = s
}

func Loop(s interface{ Fake() }) {}

func Pump(x int) {
	var s stubStrategy
	s.Observe(x)
	_ = s.Converged()
}

type stubStrategy = int
`,
		// 单方法接口:两个实现 → 不认;另一个单方法接口唯一实现 → 认。
		"one/one.go": `package one

type Multi interface{ Common() }

type M1 struct{}
func (M1) Common() {}

type M2 struct{}
func (M2) Common() {}

type Solo interface{ OnlyOne() }

type S1 struct{}
func (S1) OnlyOne() {}
`,
		// 内嵌展开:Extended 内嵌同包 Solo 风格接口 + 自有方法;External 内嵌仓外 → 弃。
		"emb/emb.go": `package emb

type Base interface {
	First()
	Second()
}

type Extended interface {
	Base
	Third()
}

type Ext struct{}
func (Ext) First() {}
func (Ext) Second() {}
func (Ext) Third() {}

type External interface {
	error
	Fourth()
}

type F struct{}
func (F) Error() string { return "" }
func (F) Fourth() {}
`,
	}
	e, _ := cgRepo(t, files)
	e.rt.mu.Lock()
	cg := e.rt.cg
	e.rt.mu.Unlock()

	// 双方法接口 → 两个完整实现,半实现不入。
	wantImpls := []string{"impl/a.go#Alpha", "impl/b.go#Beta"}
	if got := cg.implementationsOf("spec/spec.go#Strategy"); !reflect.DeepEqual(got, wantImpls) {
		t.Errorf("Strategy 实现者 = %v, want %v", got, wantImpls)
	}
	if got := cg.interfacesOf("impl/a.go#Alpha"); !reflect.DeepEqual(got, []string{"spec/spec.go#Strategy"}) {
		t.Errorf("Alpha 所实现接口 = %v", got)
	}

	// 分发边:Pump 里 s.Observe/s.Converged 无法归位 → 连到全部实现的对应方法。
	calls := cg.callsOf("run/run.go#Pump")
	for _, want := range []string{"impl/a.go#Alpha.Observe", "impl/b.go#Beta.Observe",
		"impl/a.go#Alpha.Converged", "impl/b.go#Beta.Converged"} {
		found := false
		for _, c := range calls {
			if c == want {
				found = true
			}
		}
		if !found {
			t.Errorf("Pump 分发边缺 %s(实得 %v)", want, calls)
		}
	}
	// calledBy 反向可见(修盲区的主证据)。
	if got := cg.calledByOf("impl/b.go#Beta.Observe"); len(got) == 0 {
		t.Error("Beta.Observe 的 calledBy 仍为空——接口分发未修复盲区")
	}

	// 单方法闸:Multi 两实现 → 不认;Solo 唯一实现 → 认。
	if got := cg.implementationsOf("one/one.go#Multi"); got != nil {
		t.Errorf("单方法双实现应不认,got %v", got)
	}
	if got := cg.implementationsOf("one/one.go#Solo"); !reflect.DeepEqual(got, []string{"one/one.go#S1"}) {
		t.Errorf("单方法唯一实现应认,got %v", got)
	}

	// 内嵌展开:Extended = Base(First/Second)+ Third → Ext 完整实现。
	if got := cg.implementationsOf("emb/emb.go#Extended"); !reflect.DeepEqual(got, []string{"emb/emb.go#Ext"}) {
		t.Errorf("内嵌展开后 Extended 实现者 = %v", got)
	}
	// 仓外内嵌(error)→ 整个接口弃。
	if got := cg.implementationsOf("emb/emb.go#External"); got != nil {
		t.Errorf("仓外内嵌应弃,got %v", got)
	}
}

// recall 展示与结构扩展含接口关系。
func TestInterfaceInRecallAndExpansion(t *testing.T) {
	e, _ := cgRepo(t, map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.26\n",
		"s/s.go": `package s

type Sink interface {
	Write(p []byte)
	Flush() error
}

type FileSink struct{}
func (f *FileSink) Write(p []byte) {}
func (f *FileSink) Flush() error { return nil }
`,
	})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	sid := "s-if"
	view, _, err := e.Recall(RecallArgs{Query: "s/s.go#Sink"}, sid)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(view, "实现者(方法集匹配): FileSink") {
		t.Errorf("接口节点应列实现者:\n%s", view)
	}
	view, _, err = e.Recall(RecallArgs{Query: "s/s.go#FileSink"}, sid)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(view, "实现接口: Sink") {
		t.Errorf("类型节点应列所实现接口:\n%s", view)
	}

	// 结构扩展:命中接口(带知识)→ 实现者浮出。
	if _, err := e.Remember(RememberArgs{
		Node:     "s/s.go#Sink",
		Entries:  []RememberEntry{{Kind: "contract", Text: "Flush 幂等,重复调用不重复落盘"}},
		Keywords: []string{"落盘"},
	}, sid, "claude-code"); err != nil {
		t.Fatal(err)
	}
	out, meta, err := e.Recall(RecallArgs{Query: "落盘"}, sid)
	if err != nil || !meta.Hit {
		t.Fatalf("关键词应命中:%v %v", err, meta.Hit)
	}
	if !strings.Contains(out, "结构相邻") || !strings.Contains(out, "s/s.go#FileSink") {
		t.Errorf("结构扩展应带出实现者:\n%s", out)
	}
}
