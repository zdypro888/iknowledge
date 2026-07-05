package parser

import (
	"reflect"
	"testing"
)

func TestFileCallsExtraction(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		pkg     string
		imports map[string]string
		decls   []string
		calls   map[string][]CallRef
	}{
		{
			name: "直呼与接收者自调",
			src: `package a

type S struct{}

func (s *S) Run() { s.step(); helper() }

func (s *S) step() {}

func helper() {}
`,
			pkg:     "a",
			imports: map[string]string{},
			decls:   []string{"S", "S.Run", "S.step", "helper"},
			calls: map[string][]CallRef{
				"S.Run": {{Name: "S.step"}, {Name: "helper"}},
			},
		},
		{
			name: "限定引用:缺省名与别名;dot/blank 不入",
			src: `package a

import (
	"fmt"
	alias "example.com/m/pkg"
	. "example.com/m/dot"
	_ "example.com/m/blank"
)

func f() { fmt.Println(); alias.Do() }
`,
			pkg:     "a",
			imports: map[string]string{"fmt": "fmt", "alias": "example.com/m/pkg"},
			decls:   []string{"f"},
			calls: map[string][]CallRef{
				"f": {{Qual: "fmt", Name: "Println"}, {Qual: "alias", Name: "Do"}},
			},
		},
		{
			name: "var 初始化调用:等长按位归属,不等长整组归属",
			src: `package a

var x, y = f(), g()

var a, b = h()

func f() int { return 0 }
func g() int { return 0 }
func h() (int, int) { return 0, 0 }
`,
			pkg:     "a",
			imports: map[string]string{},
			decls:   []string{"x", "y", "a", "b", "f", "g", "h"},
			calls: map[string][]CallRef{
				"x": {{Name: "f"}},
				"y": {{Name: "g"}},
				"a": {{Name: "h"}},
				"b": {{Name: "h"}},
			},
		},
		{
			name: "~n 消歧与 Parse 一致(双 init)",
			src: `package a

func init() { f() }

func init() { g() }

func f() {}
func g() {}
`,
			pkg:     "a",
			imports: map[string]string{},
			decls:   []string{"init", "init~2", "f", "g"},
			calls: map[string][]CallRef{
				"init":   {{Name: "f"}},
				"init~2": {{Name: "g"}},
			},
		},
		{
			name: "链式选择器不解析;同方去重;非接收者限定保留基名",
			src: `package a

type T struct{}

func (t T) M() {}

func f() {
	var t T
	t.M()
	t.M()
	a.b.C()
}

var a struct{ b interface{ C() } }
`,
			pkg:     "a",
			imports: map[string]string{},
			decls:   []string{"T", "T.M", "f", "a"},
			calls: map[string][]CallRef{
				"f": {{Qual: "t", Name: "M"}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc, err := Golang{}.FileCalls("a.go", []byte(tc.src))
			if err != nil {
				t.Fatal(err)
			}
			if fc.Package != tc.pkg {
				t.Errorf("Package = %q, want %q", fc.Package, tc.pkg)
			}
			if !reflect.DeepEqual(fc.Imports, tc.imports) {
				t.Errorf("Imports = %v, want %v", fc.Imports, tc.imports)
			}
			if !reflect.DeepEqual(fc.Decls, tc.decls) {
				t.Errorf("Decls = %v, want %v", fc.Decls, tc.decls)
			}
			// calls 只比对期望里出现的键之外还要求无多余键。
			if len(fc.Calls) != len(tc.calls) {
				t.Errorf("Calls 键集 = %v, want %v", fc.Calls, tc.calls)
			}
			for caller, want := range tc.calls {
				if got := fc.Calls[caller]; !reflect.DeepEqual(got, want) {
					t.Errorf("Calls[%s] = %v, want %v", caller, got, want)
				}
			}
		})
	}
}

func TestInterfaceExtraction(t *testing.T) {
	src := `package a

import "io"

type Plain interface {
	One(x int)
	Two() error
}

type WithEmbed interface {
	Plain
	io.Reader
	Three()
}

type Constrained interface {
	~int | ~string
}

type NotIface struct{}
`
	fc, err := Golang{}.FileCalls("a.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.Interfaces) != 3 {
		t.Fatalf("接口数 = %d, want 3:%+v", len(fc.Interfaces), fc.Interfaces)
	}
	byName := map[string]IfaceDecl{}
	for _, d := range fc.Interfaces {
		byName[d.Name] = d
	}
	if p := byName["Plain"]; !reflect.DeepEqual(p.Methods, []string{"One", "Two"}) || len(p.Embeds) != 0 {
		t.Errorf("Plain = %+v", p)
	}
	we := byName["WithEmbed"]
	if !reflect.DeepEqual(we.Methods, []string{"Three"}) ||
		!reflect.DeepEqual(we.Embeds, []CallRef{{Name: "Plain"}, {Qual: "io", Name: "Reader"}}) {
		t.Errorf("WithEmbed = %+v", we)
	}
	// 约束元素 → 哨兵(engine 弃)。
	c := byName["Constrained"]
	if len(c.Embeds) == 0 || c.Embeds[0].Name != "!unresolvable" {
		t.Errorf("约束接口应标哨兵:%+v", c)
	}
}

func TestFileCallsDeclsMatchParse(t *testing.T) {
	// 规范名一致性是 node ID 拼接的前提:两条提取路径必须产出同一名单。
	src := `package a

import "fmt"

type Stack[T any] struct{}

func (s *Stack[T]) Push(v T) { s.grow(); fmt.Println(v) }

func (s *Stack[T]) grow() {}

var _ = f()
var _ = g()

func f() int { return 0 }
func g() int { return 0 }

const c = 1
`
	syms, err := Golang{}.Parse("a.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	var parsed []string
	for _, s := range syms {
		parsed = append(parsed, s.Name)
	}
	fc, err := Golang{}.FileCalls("a.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fc.Decls, parsed) {
		t.Errorf("FileCalls.Decls = %v\nParse 名单     = %v", fc.Decls, parsed)
	}
}
