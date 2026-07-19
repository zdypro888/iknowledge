package engine

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// cgRepo 建一个带 go.mod 的临时仓库并返回已持锁构建好的调用图。
func cgRepo(t *testing.T, files map[string]string) (*Engine, *callGraph) {
	t.Helper()
	e, _ := newRepo(t, files)
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	cg := e.ensureCallGraphLocked()
	if cg == nil {
		t.Fatal("ensureCallGraphLocked() = nil")
	}
	return e, cg
}

func TestCallGraphCrossPackage(t *testing.T) {
	files := map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.26\n",
		"a/a.go": `package a

import "example.com/m/b"

func A() { b.B(); helperA() }

func helperA() {}
`,
		"b/b.go": `package b

func B() { sub() }

func sub() {}
`,
		// 同包跨文件:b2.go 调 b.go 的 sub。
		"b/b2.go": `package b

func B2() { sub() }
`,
	}
	_, cg := cgRepo(t, files)

	cases := []struct {
		node  string
		calls []string
	}{
		{"a/a.go#A", []string{"a/a.go#helperA", "b/b.go#B"}},
		{"b/b.go#B", []string{"b/b.go#sub"}},
		{"b/b2.go#B2", []string{"b/b.go#sub"}},
	}
	for _, tc := range cases {
		if got := cg.callsOf(tc.node); !reflect.DeepEqual(got, tc.calls) {
			t.Errorf("callsOf(%s) = %v, want %v", tc.node, got, tc.calls)
		}
	}
	if got := cg.calledByOf("b/b.go#sub"); !reflect.DeepEqual(got, []string{"b/b.go#B", "b/b2.go#B2"}) {
		t.Errorf("calledByOf(sub) = %v", got)
	}
	if got := cg.calledByOf("b/b.go#B"); !reflect.DeepEqual(got, []string{"a/a.go#A"}) {
		t.Errorf("calledByOf(B) = %v", got)
	}
}

func TestCallGraphSeparatesExternalTestPackage(t *testing.T) {
	files := map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.26\n",
		"pkg/prod.go": `package pkg

func helper() {}
func Run() {}
`,
		"pkg/use.go": `package pkg

func Use() { helper() }
`,
		"pkg/external_test.go": `package pkg_test

func helper() {}
func Run() {}
`,
		"pkg/external_use_test.go": `package pkg_test

func TestUse() { helper() }
`,
		"caller/caller.go": `package caller

import "example.com/m/pkg"

func Call() { pkg.Run() }
`,
	}
	_, cg := cgRepo(t, files)

	tests := []struct {
		from string
		want []string
	}{
		{"pkg/use.go#Use", []string{"pkg/prod.go#helper"}},
		{"pkg/external_use_test.go#TestUse", []string{"pkg/external_test.go#helper"}},
		// import 只能落到目标目录的 production package，不能被同目录
		// external-test package 的同名声明制造歧义。
		{"caller/caller.go#Call", []string{"pkg/prod.go#Run"}},
	}
	for _, tt := range tests {
		if got := cg.callsOf(tt.from); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("callsOf(%s) = %v, want %v", tt.from, got, tt.want)
		}
	}
}

func TestCallGraphRejectsSourceAndModuleSymlinks(t *testing.T) {
	e, repo := newRepo(t, map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.26\n",
		"a/a.go": `package a

func Run() { helper() }
func helper() {}
`,
	})

	cg := e.ensureCallGraphLocked()
	if cg == nil {
		t.Fatal("初始调用图 = nil")
	}
	if cg.module != "example.com/m" {
		t.Fatalf("初始调用图 module = %q", cg.module)
	}
	if got := cg.callsOf("a/a.go#Run"); !reflect.DeepEqual(got, []string{"a/a.go#helper"}) {
		t.Fatalf("初始 callsOf(Run) = %v", got)
	}

	outside := t.TempDir()
	outsideSource := filepath.Join(outside, "outside.go")
	if err := os.WriteFile(outsideSource, []byte("package a\nfunc OutsideSecret() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outsideModule := filepath.Join(outside, "go.mod")
	if err := os.WriteFile(outsideModule, []byte("module outside.example/secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	sourcePath := filepath.Join(repo, "a", "a.go")
	if err := os.Remove(sourcePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideSource, sourcePath); err != nil {
		t.Skipf("当前平台不能创建 symlink: %v", err)
	}
	modulePath := filepath.Join(repo, "go.mod")
	if err := os.Remove(modulePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideModule, modulePath); err != nil {
		t.Skipf("当前平台不能创建 symlink: %v", err)
	}

	cg = e.ensureCallGraphLocked()
	if cg == nil {
		t.Fatal("symlink 被拒绝后调用图不应整体失效")
	}
	if cg.module != "" {
		t.Errorf("callgraph 跟随了仓外 go.mod symlink: module = %q", cg.module)
	}
	if _, ok := cg.files["a/a.go"]; ok {
		t.Error("callgraph 保留或读取了仓外源码 symlink")
	}
	if got := cg.callsOf("a/a.go#Run"); got != nil {
		t.Errorf("仓外源码 symlink 的旧调用边未清除: %v", got)
	}
}

func TestCallGraphAmbiguityAndHeuristic(t *testing.T) {
	files := map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.26\n",
		// build tag 双版本:同名符号声明在两个文件——跨文件歧义必须丢边;
		// 声明文件内部自引用可精确归位。
		"s/lock_a.go": `//go:build unix

package s

func Lock() { onA() }

func onA() {}
`,
		"s/lock_b.go": `//go:build !unix

package s

func Lock() { onB() }

func onB() {}
`,
		"s/use.go": `package s

func Use() { Lock() }
`,
		// 方法基名启发:局部变量方法调用,包内唯一基名归位;多类型同名方法丢边。
		"h/h.go": `package h

type A struct{}
func (a A) Unique() {}
func (a A) Dup() {}

type B struct{}
func (b B) Dup() {}

func f() {
	var x A
	x.Unique()
	x.Dup()
}
`,
		// 库外调用不产生边。
		"x/x.go": `package x

import "fmt"

func X() { fmt.Println() }
`,
	}
	_, cg := cgRepo(t, files)

	if got := cg.callsOf("s/use.go#Use"); got != nil {
		t.Errorf("跨文件歧义应丢边,callsOf(Use) = %v", got)
	}
	if got := cg.callsOf("s/lock_a.go#Lock"); !reflect.DeepEqual(got, []string{"s/lock_a.go#onA"}) {
		t.Errorf("声明文件内自引用应归位,got %v", got)
	}
	if got := cg.callsOf("h/h.go#f"); !reflect.DeepEqual(got, []string{"h/h.go#A.Unique"}) {
		t.Errorf("唯一基名启发应仅归位 Unique,got %v", got)
	}
	if got := cg.callsOf("x/x.go#X"); got != nil {
		t.Errorf("库外调用不应产生边,got %v", got)
	}
}

func TestCallGraphNoModule(t *testing.T) {
	// 无 go.mod:限定引用无从归位,同包直呼仍可用。
	files := map[string]string{
		"a/a.go": `package a

func A() { helper() }

func helper() {}
`,
	}
	_, cg := cgRepo(t, files)
	if cg.module != "" {
		t.Errorf("module = %q, want 空", cg.module)
	}
	if got := cg.callsOf("a/a.go#A"); !reflect.DeepEqual(got, []string{"a/a.go#helper"}) {
		t.Errorf("callsOf(A) = %v", got)
	}
}

func TestCallGraphIncremental(t *testing.T) {
	files := map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.26\n",
		"a/a.go": `package a

func A() { f() }

func f() {}
func g() {}
`,
	}
	e, cg := cgRepo(t, files)
	if got := cg.callsOf("a/a.go#A"); !reflect.DeepEqual(got, []string{"a/a.go#f"}) {
		t.Fatalf("初始 callsOf(A) = %v", got)
	}

	// 改写文件(mtime 前推保证指纹变化,精度粗的文件系统不靠 sleep)。
	p := filepath.Join(e.Store.RepoRoot(), "a", "a.go")
	if err := os.WriteFile(p, []byte(`package a

func A() { g() }

func f() {}
func g() {}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatal(err)
	}

	e.rt.mu.Lock()
	cg2 := e.ensureCallGraphLocked()
	e.rt.mu.Unlock()
	if cg2 == cg {
		t.Fatal("刷新应发布新 callGraph 快照,不能原地修改已交给读者的图")
	}
	if got := cg.callsOf("a/a.go#A"); !reflect.DeepEqual(got, []string{"a/a.go#f"}) {
		t.Fatalf("旧调用图快照被刷新原地改写: %v", got)
	}
	if got := cg2.callsOf("a/a.go#A"); !reflect.DeepEqual(got, []string{"a/a.go#g"}) {
		t.Errorf("增量刷新后 callsOf(A) = %v, want [a/a.go#g]", got)
	}

	// 删除文件:节点与边消失。
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	e.rt.mu.Lock()
	cg3 := e.ensureCallGraphLocked()
	e.rt.mu.Unlock()
	if got := cg3.callsOf("a/a.go#A"); got != nil {
		t.Errorf("删除后 callsOf(A) = %v, want nil", got)
	}
}

func TestRecallStructuralExpansion(t *testing.T) {
	// 关键词只命中 pay.go#Charge;Verify 与它有调用边且带知识,应经一跳浮出。
	e, _ := newRepo(t, map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.26\n",
		"pay/pay.go": `package pay

// Charge 扣款入口。
func Charge() { verify() }

func verify() {}
`,
	})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	sid := "sid-cg"
	if _, err := e.Remember(RememberArgs{
		Node:     "pay/pay.go#Charge",
		Entries:  []RememberEntry{{Kind: "summary", Text: "扣款入口"}},
		Keywords: []string{"扣款"},
	}, sid, "claude-code"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Remember(RememberArgs{
		Node:    "pay/pay.go#verify",
		Entries: []RememberEntry{{Kind: "pitfall", Text: "校验依赖外部时钟,本地跑会假失败"}},
	}, sid, "claude-code"); err != nil {
		t.Fatal(err)
	}

	out, meta, err := e.Recall(RecallArgs{Query: "扣款"}, sid)
	if err != nil {
		t.Fatal(err)
	}
	if !meta.Hit {
		t.Fatalf("关键词应命中:%s", out)
	}
	if !strings.Contains(out, "结构相邻") || !strings.Contains(out, "pay/pay.go#verify") {
		t.Errorf("结构扩展未带出 verify:\n%s", out)
	}
	if !strings.Contains(out, "被 pay/pay.go#Charge 调用") {
		t.Errorf("扩展途径标注缺失:\n%s", out)
	}
}

func TestStatusHotspots(t *testing.T) {
	// 无 git 仓库:热度退化为纯中心度。core.go#Do 被两处跨文件调用 → 热点;
	// 全消化文件不出现在热点里。
	e, _ := newRepo(t, map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.26\n",
		"core/core.go": `package core

func Do() {}
`,
		"a/a.go": `package a

import "example.com/m/core"

func A() { core.Do() }
`,
		"b/b.go": `package b

import "example.com/m/core"

func B() { core.Do() }
`,
	})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	out, err := e.Status()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "热点待消化") || !strings.Contains(out, "core/core.go 热度 3(改 0 次 × 被调 2)消化 0/1") {
		t.Errorf("热点清单缺失或计算错:\n%s", out)
	}

	// 消化 core.go#Do 后它退出热点(a/b 仍在但热度=1 不占版面或列出)。
	if _, err := e.Remember(RememberArgs{
		Node:    "core/core.go#Do",
		Entries: []RememberEntry{{Kind: "summary", Text: "核心入口"}},
	}, "sid-h", "claude-code"); err != nil {
		t.Fatal(err)
	}
	out, err = e.Status()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "core/core.go 热度") {
		t.Errorf("全消化文件仍在热点里:\n%s", out)
	}
}

func TestDisplayEdges(t *testing.T) {
	edges := []string{
		"a/a.go#one", "a/a.go#two",
		"b/b.go#Cross",
	}
	got := displayEdges(edges, "a/a.go", 12)
	want := []string{"one", "two", "b/b.go#Cross"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("displayEdges = %v, want %v", got, want)
	}

	var many []string
	for i := range 20 {
		many = append(many, "c/c.go#F"+string(rune('a'+i)))
	}
	got = displayEdges(many, "a/a.go", 12)
	if len(got) != 13 || got[12] != "……(共 20 处)" {
		t.Errorf("截断展示 = %d 项,尾 = %q", len(got), got[len(got)-1])
	}
	if displayEdges(nil, "a/a.go", 12) != nil {
		t.Error("空边应返回 nil")
	}
}
