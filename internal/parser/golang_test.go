package parser

import (
	"strings"
	"testing"
)

func parseAll(t *testing.T, src string) []Symbol {
	t.Helper()
	syms, err := Golang{}.Parse("test.go", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return syms
}

func findSym(t *testing.T, syms []Symbol, name string) Symbol {
	t.Helper()
	for _, s := range syms {
		if s.Name == name {
			return s
		}
	}
	names := make([]string, len(syms))
	for i, s := range syms {
		names[i] = s.Name
	}
	t.Fatalf("符号 %q 不存在;有:%v", name, names)
	return Symbol{}
}

// TestExtractSymbols 覆盖 impl §10:各类声明的符号边界与规范名。
func TestExtractSymbols(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []struct{ name, kind string } // 按源码顺序
	}{
		{
			name: "普通函数",
			src:  "package p\n\nfunc Login(user, pass string) error { return nil }\n",
			want: []struct{ name, kind string }{{"Login", "func"}},
		},
		{
			name: "指针接收者方法",
			src:  "package p\n\ntype AuthService struct{}\n\nfunc (s *AuthService) SignIn() {}\n",
			want: []struct{ name, kind string }{{"AuthService", "type"}, {"AuthService.SignIn", "method"}},
		},
		{
			name: "泛型接收者去类型参数",
			src:  "package p\n\ntype Stack[T any] struct{ v []T }\n\nfunc (s *Stack[T]) Push(v T) {}\n",
			want: []struct{ name, kind string }{{"Stack", "type"}, {"Stack.Push", "method"}},
		},
		{
			name: "多类型参数接收者",
			src:  "package p\n\ntype Cache[K comparable, V any] struct{}\n\nfunc (c Cache[K, V]) Get(k K) (V, bool) { var z V; return z, false }\n",
			want: []struct{ name, kind string }{{"Cache", "type"}, {"Cache.Get", "method"}},
		},
		{
			name: "泛型函数与多返回值",
			src:  "package p\n\nfunc Map[T, U any](in []T, f func(T) U) ([]U, error) { return nil, nil }\n",
			want: []struct{ name, kind string }{{"Map", "func"}},
		},
		{
			name: "同文件多 init 带序号",
			src:  "package p\n\nfunc init() {}\n\nfunc init() {}\n\nfunc init() {}\n",
			want: []struct{ name, kind string }{{"init", "func"}, {"init~2", "func"}, {"init~3", "func"}},
		},
		{
			name: "多下划线声明带序号",
			src:  "package p\n\nvar _ = 1\n\nvar _ = 2\n",
			want: []struct{ name, kind string }{{"_", "var"}, {"_~2", "var"}},
		},
		{
			name: "GenDecl 分组按 Spec 拆",
			src:  "package p\n\nvar (\n\ta = 1\n\tb = 2\n)\n",
			want: []struct{ name, kind string }{{"a", "var"}, {"b", "var"}},
		},
		{
			name: "var a, b int 按名拆",
			src:  "package p\n\nvar a, b int\n",
			want: []struct{ name, kind string }{{"a", "var"}, {"b", "var"}},
		},
		{
			name: "const iota 块",
			src:  "package p\n\nconst (\n\tA = iota\n\tB\n\tC\n)\n",
			want: []struct{ name, kind string }{{"A", "const"}, {"B", "const"}, {"C", "const"}},
		},
		{
			name: "type 分组",
			src:  "package p\n\ntype (\n\tFoo struct{}\n\tBar interface{}\n)\n",
			want: []struct{ name, kind string }{{"Foo", "type"}, {"Bar", "type"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			syms := parseAll(t, tt.src)
			if len(syms) != len(tt.want) {
				t.Fatalf("符号数 = %d, want %d(%+v)", len(syms), len(tt.want), syms)
			}
			for i, w := range tt.want {
				if syms[i].Name != w.name || syms[i].Kind != w.kind {
					t.Errorf("符号[%d] = (%q, %q), want (%q, %q)", i, syms[i].Name, syms[i].Kind, w.name, w.kind)
				}
				if syms[i].Hash == "" || syms[i].StructHash == "" || syms[i].DocStructHash == "" {
					t.Errorf("符号 %q 语义哈希缺失", syms[i].Name)
				}
			}
		})
	}
}

// TestUnitBoundaries 覆盖代码单元边界:doc comment 归属与块级 doc 继承。
func TestUnitBoundaries(t *testing.T) {
	t.Run("函数单元含 doc comment", func(t *testing.T) {
		src := "package p\n\n// Login 登录入口。\n// 第二行。\nfunc Login() {}\n"
		s := findSym(t, parseAll(t, src), "Login")
		if !strings.HasPrefix(string(s.Body), "// Login 登录入口。") {
			t.Errorf("Body 未含 doc comment:%q", s.Body)
		}
		if s.Lines[0] != 3 {
			t.Errorf("起始行 = %d, want 3(doc 首行)", s.Lines[0])
		}
	})
	t.Run("未分组声明单元为整个 GenDecl 含 doc", func(t *testing.T) {
		src := "package p\n\n// Foo 是示例。\ntype Foo struct{ X int }\n"
		s := findSym(t, parseAll(t, src), "Foo")
		if !strings.HasPrefix(string(s.Body), "// Foo 是示例。") || !strings.Contains(string(s.Body), "type Foo") {
			t.Errorf("Body = %q, 应含 doc 与 type 关键字", s.Body)
		}
	})
	t.Run("分组内 Spec 单元含自身 doc 不含块 doc", func(t *testing.T) {
		src := "package p\n\n// 块级说明。\nvar (\n\t// a 的说明。\n\ta = 1\n\tb = 2\n)\n"
		a := findSym(t, parseAll(t, src), "a")
		if !strings.HasPrefix(string(a.Body), "// a 的说明。") {
			t.Errorf("a.Body 应含自身 doc:%q", a.Body)
		}
		if strings.Contains(string(a.Body), "块级说明") {
			t.Errorf("a.Body 不应含块级 doc:%q", a.Body)
		}
		b := findSym(t, parseAll(t, src), "b")
		if got := strings.TrimSpace(string(b.Body)); got != "b = 2" {
			t.Errorf("b.Body = %q, want %q", got, "b = 2")
		}
	})
	t.Run("var a, b int 共享单元与 Hash", func(t *testing.T) {
		syms := parseAll(t, "package p\n\nvar a, b int\n")
		a, b := findSym(t, syms, "a"), findSym(t, syms, "b")
		if a.Start != b.Start || a.End != b.End || a.Hash != b.Hash {
			t.Errorf("a 与 b 应共享单元与 Hash:a=%+v b=%+v", a, b)
		}
		if a.StructHash == b.StructHash {
			t.Errorf("a 与 b 的 StructHash 应不同(各自占位)")
		}
	})
}

// TestHashMatrix 覆盖 impl §10 哈希行为矩阵。
func TestHashMatrix(t *testing.T) {
	tests := []struct {
		name           string
		srcA, srcB     string
		symA, symB     string
		wantHashSame   bool
		wantStructSame bool
	}{
		{
			name: "仅移动位置(双哈希均不变)",
			srcA: "package p\n\n// Login 入口。\nfunc Login() int {\n\tx := 1\n\n\treturn x\n}\n\nfunc Other() {}\n",
			srcB: "package p\n\nfunc Other() {}\n\nvar filler = 42\n\n// Login 入口。\nfunc Login() int {\n\tx := 1\n\n\treturn x\n}\n",
			symA: "Login", symB: "Login",
			wantHashSame: true, wantStructSame: true,
		},
		{
			name: "gofmt 格式化(双哈希均不变)",
			srcA: "package p\n\n// Login 入口。\nfunc Login( user,pass string )error{\n    if user==\"\"{\n\treturn nil}\n  return nil\n}\n",
			srcB: "package p\n\n// Login 入口。\nfunc Login(user, pass string) error {\n\tif user == \"\" {\n\t\treturn nil\n\t}\n\treturn nil\n}\n",
			symA: "Login", symB: "Login",
			wantHashSame: true, wantStructSame: true,
		},
		{
			name: "注释 reflow:doc 缩进与标记变化(双哈希均不变)",
			srcA: "package p\n\n//Login 入口。\nfunc Login() {}\n",
			srcB: "package p\n\n//   Login 入口。\nfunc Login() {}\n",
			symA: "Login", symB: "Login",
			wantHashSame: true, wantStructSame: true,
		},
		{
			name: "注释内容修改(Hash 变 / StructHash 不变)",
			srcA: "package p\n\n// Login 入口;pass 传明文。\nfunc Login() {}\n",
			srcB: "package p\n\n// Login 入口;pass 传密文。\nfunc Login() {}\n",
			symA: "Login", symB: "Login",
			wantHashSame: false, wantStructSame: true,
		},
		{
			name: "改名(Hash 变 / StructHash 不变)",
			srcA: "package p\n\n// Login 验证凭证。\nfunc Login(user, pass string) error {\n\treturn check(user, pass)\n}\nfunc check(u, p string) error { return nil }\n",
			srcB: "package p\n\n// Authenticate 验证凭证。\nfunc Authenticate(user, pass string) error {\n\treturn check(user, pass)\n}\nfunc check(u, p string) error { return nil }\n",
			symA: "Login", symB: "Authenticate",
			wantHashSame: false, wantStructSame: true,
		},
		{
			name: "改函数体(双哈希均变)",
			srcA: "package p\n\nfunc Login() error { return nil }\n",
			srcB: "package p\n\nfunc Login() error { return errTooMany }\n\nvar errTooMany error\n",
			symA: "Login", symB: "Login",
			wantHashSame: false, wantStructSame: false,
		},
		{
			name: "方法改名(Hash 变 / StructHash 不变)",
			srcA: "package p\n\ntype S struct{}\n\nfunc (s *S) SignIn() error { return nil }\n",
			srcB: "package p\n\ntype S struct{}\n\nfunc (s *S) Authenticate() error { return nil }\n",
			symA: "S.SignIn", symB: "S.Authenticate",
			wantHashSame: false, wantStructSame: true,
		},
		{
			name: "type 改名(Hash 变 / StructHash 不变)",
			srcA: "package p\n\n// Session 会话。\ntype Session struct{ ID string }\n",
			srcB: "package p\n\n// Token 会话。\ntype Token struct{ ID string }\n",
			symA: "Session", symB: "Token",
			wantHashSame: false, wantStructSame: true,
		},
		{
			name: "var 分组整理(双哈希均不变——分组不是语义变更)",
			srcA: "package p\n\n// cache 全局缓存。\nvar cache = map[string]int{}\n",
			srcB: "package p\n\nvar (\n\t// cache 全局缓存。\n\tcache = map[string]int{}\n)\n",
			symA: "cache", symB: "cache",
			wantHashSame: true, wantStructSame: true,
		},
		{
			name: "块级 doc 继承:块 doc 内容变(Hash 变 / StructHash 不变)",
			srcA: "package p\n\n// 一组限流参数。\nvar (\n\tlimit = 3\n)\n",
			srcB: "package p\n\n// 一组超时参数。\nvar (\n\tlimit = 3\n)\n",
			symA: "limit", symB: "limit",
			wantHashSame: false, wantStructSame: true,
		},
		{
			name: "自身 doc 优先于块 doc:自身 doc 变(Hash 变)",
			srcA: "package p\n\n// 块 doc。\nvar (\n\t// limit 是重试上限。\n\tlimit = 3\n)\n",
			srcB: "package p\n\n// 块 doc。\nvar (\n\t// limit 是锁定阈值。\n\tlimit = 3\n)\n",
			symA: "limit", symB: "limit",
			wantHashSame: false, wantStructSame: true,
		},
		{
			name: "行尾注释不参与哈希",
			srcA: "package p\n\nvar limit = 3 // 旧说明\n",
			srcB: "package p\n\nvar limit = 3 // 新说明\n",
			symA: "limit", symB: "limit",
			wantHashSame: true, wantStructSame: true,
		},
		{
			name: "const 与 var 同文本不同哈希(tok 前缀)",
			srcA: "package p\n\nconst limit = 3\n",
			srcB: "package p\n\nvar limit = 3\n",
			symA: "limit", symB: "limit",
			wantHashSame: false, wantStructSame: false,
		},
		{
			name: "搬到不同 package/import 上下文(双哈希均变)",
			srcA: "package p\n\n// Helper 工具。\nfunc Helper() int {\n\treturn 7\n}\n",
			srcB: "package other\n\nimport \"fmt\"\n\nfunc init() { fmt.Println() }\n\n// Helper 工具。\nfunc Helper() int {\n\treturn 7\n}\n",
			symA: "Helper", symB: "Helper",
			wantHashSame: false, wantStructSame: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := findSym(t, parseAll(t, tt.srcA), tt.symA)
			b := findSym(t, parseAll(t, tt.srcB), tt.symB)
			if got := a.Hash == b.Hash; got != tt.wantHashSame {
				t.Errorf("Hash 相同 = %v, want %v(a=%s b=%s)", got, tt.wantHashSame, a.Hash, b.Hash)
			}
			if got := a.StructHash == b.StructHash; got != tt.wantStructSame {
				t.Errorf("StructHash 相同 = %v, want %v(a=%s b=%s)", got, tt.wantStructSame, a.StructHash, b.StructHash)
			}
		})
	}
}

// TestHashDeterminism 同一源码两次解析,哈希与提取结果逐位一致。
func TestHashDeterminism(t *testing.T) {
	src := "package p\n\n// Login 入口。\nfunc Login() {}\n\nvar (\n\ta, b = 1, 2\n)\n\ntype Foo struct{ X int }\n"
	s1, s2 := parseAll(t, src), parseAll(t, src)
	if len(s1) != len(s2) {
		t.Fatalf("两次解析符号数不同:%d vs %d", len(s1), len(s2))
	}
	for i := range s1 {
		if s1[i].Name != s2[i].Name || s1[i].Hash != s2[i].Hash || s1[i].StructHash != s2[i].StructHash ||
			s1[i].DocStructHash != s2[i].DocStructHash ||
			s1[i].Start != s2[i].Start || s1[i].End != s2[i].End {
			t.Errorf("符号[%d] 两次解析不一致:%+v vs %+v", i, s1[i], s2[i])
		}
	}
}

func TestFileHash(t *testing.T) {
	symsA := parseAll(t, "package p\n\nfunc A() {}\n\nfunc B() {}\n")
	// import 是编译语义上下文，blank import 变化必须影响文件哈希。
	symsB := parseAll(t, "package p\n\nimport _ \"embed\"\n\nfunc A() {}\n\nfunc B() {}\n")
	if FileHash(symsA) == FileHash(symsB) {
		t.Errorf("blank import 变化必须影响文件哈希")
	}
	symsC := parseAll(t, "package p\n\nfunc A() { _ = 1 }\n\nfunc B() {}\n")
	if FileHash(symsA) == FileHash(symsC) {
		t.Errorf("函数体变化必须影响文件哈希")
	}
}

func TestGoZeroSymbolFileHashIncludesContext(t *testing.T) {
	p := Golang{}
	srcA := []byte("package p\n\nimport _ \"embed\"\n")
	srcB := []byte("package p\n\nimport _ \"net/http/pprof\"\n")
	symsA, err := p.Parse("a.go", srcA)
	if err != nil {
		t.Fatal(err)
	}
	symsB, err := p.Parse("b.go", srcB)
	if err != nil {
		t.Fatal(err)
	}
	if len(symsA) != 0 || len(symsB) != 0 {
		t.Fatalf("测试前提错误:应是零符号文件")
	}
	if HashFileFor(p, symsA, srcA) == HashFileFor(p, symsB, srcB) {
		t.Fatal("零符号 Go 文件的 import 变更也必须改变文件哈希")
	}
	if p.HashFile(srcA) == p.HashFile(srcB) {
		t.Fatal("Golang.HashFile 独立出口也必须覆盖零符号上下文")
	}
}

func TestGoFileNodeHashIncludesUnreferencedNamedImports(t *testing.T) {
	p := Golang{}
	srcA := []byte("package p\n\nfunc Run() {}\n")
	srcB := []byte("package p\n\nimport unused \"example.com/unused\"\n\nfunc Run() {}\n")
	symsA, err := p.Parse("a.go", srcA)
	if err != nil {
		t.Fatal(err)
	}
	symsB, err := p.Parse("b.go", srcB)
	if err != nil {
		t.Fatal(err)
	}
	if symsA[0].Hash != symsB[0].Hash || symsA[0].StructHash != symsB[0].StructHash {
		t.Fatal("未被引用的 named import 不得污染符号锚")
	}
	if HashFileFor(p, symsA, srcA) == HashFileFor(p, symsB, srcB) {
		t.Fatal("file 节点必须覆盖全量 import，包括未归位到符号的 named import")
	}
}

func TestGoSemanticFileContextHashes(t *testing.T) {
	parse := func(src string) Symbol {
		t.Helper()
		return findSym(t, parseAll(t, src), "Run")
	}
	base := `//go:build linux && amd64

package p

import (
	alias "example.com/a"
	_ "embed"
)

func Run() { alias.Call() }
`
	tests := []struct {
		name string
		src  string
		same bool
	}{
		{
			name: "import 重排免疫",
			src: `//go:build linux && amd64

package p

import (
	_ "embed"
	alias "example.com/a"
)

func Run() { alias.Call() }
`,
			same: true,
		},
		{
			name: "import 路径变更",
			src:  strings.Replace(base, "example.com/a", "example.com/b", 1),
		},
		{
			name: "import alias 变更",
			src:  strings.Replace(strings.Replace(base, "alias \"example.com/a\"", "other \"example.com/a\"", 1), "alias.Call", "other.Call", 1),
		},
		{
			name: "无关 named import 不污染符号",
			src:  strings.Replace(base, "\t_ \"embed\"\n", "\t_ \"embed\"\n\tunused \"example.com/unused\"\n", 1),
			same: true,
		},
		{
			name: "blank import 变更",
			src:  strings.Replace(base, "\t_ \"embed\"\n", "", 1),
		},
		{
			name: "build constraint 变更",
			src:  strings.Replace(base, "linux && amd64", "darwin && amd64", 1),
		},
		{
			name: "package 变更",
			src:  strings.Replace(base, "package p", "package q", 1),
		},
	}
	baseSym := parse(base)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parse(tt.src)
			if same := got.Hash == baseSym.Hash; same != tt.same {
				t.Errorf("Hash 相同=%v,want %v", same, tt.same)
			}
			if same := got.StructHash == baseSym.StructHash; same != tt.same {
				t.Errorf("StructHash 相同=%v,want %v", same, tt.same)
			}
		})
	}
}

func TestGoDefaultVersionedImportAliasContext(t *testing.T) {
	a := findSym(t, parseAll(t, "package p\n\nimport \"gopkg.in/yaml.v3\"\n\nfunc Run() { _ = yaml.Node{} }\n"), "Run")
	b := findSym(t, parseAll(t, "package p\n\nimport \"gopkg.in/yaml.v4\"\n\nfunc Run() { _ = yaml.Node{} }\n"), "Run")
	if a.Hash == b.Hash || a.StructHash == b.StructHash {
		t.Fatal("默认别名的版本化 import 路径变更必须进入符号上下文")
	}
}

func TestGoImplicitImportWithNonBasenamePackageIsConservative(t *testing.T) {
	// Go import 声明不记录真实 package name；这里故意让 selector 与路径末段
	// 不同，模拟 client-go 等现实依赖。basename 猜测会把两条 import 都判成
	// “未使用”，导致路径变化时符号三哈希完全相同。
	a := findSym(t, parseAll(t, `package p

import "example.com/client-go"

func Run() { kubernetes.New() }
`), "Run")
	b := findSym(t, parseAll(t, `package p

import "example.com/client-runtime"

func Run() { kubernetes.New() }
`), "Run")
	if a.Hash == b.Hash || a.StructHash == b.StructHash || a.DocStructHash == b.DocStructHash {
		t.Fatal("无法证明包名的隐式 import 必须保守进入全部符号上下文")
	}
}

func TestGoDocStructHashGuardsRenameMigration(t *testing.T) {
	old := findSym(t, parseAll(t, "package p\n\n// Login 验证密码。\nfunc Login() {}\n"), "Login")
	renamed := findSym(t, parseAll(t, "package p\n\n// Authenticate 验证密码。\nfunc Authenticate() {}\n"), "Authenticate")
	changedContract := findSym(t, parseAll(t, "package p\n\n// Authenticate 跳过密码验证。\nfunc Authenticate() {}\n"), "Authenticate")

	if old.StructHash != renamed.StructHash || old.DocStructHash != renamed.DocStructHash {
		t.Fatal("仅自身名及 doc 中同名标识符变更应可迁移")
	}
	if old.StructHash != changedContract.StructHash {
		t.Fatal("旧 StructHash 仍应保持 doc 免疫语义")
	}
	if old.DocStructHash == changedContract.DocStructHash {
		t.Fatal("改名同时修改 doc 契约必须被 DocStructHash 拦截")
	}
}

// TestParseError 语法错误文件必须报错(impl §5 解析失败三态的 parser 侧)。
func TestParseError(t *testing.T) {
	_, err := Golang{}.Parse("bad.go", []byte("package p\n\nfunc Broken( {\n"))
	if err == nil {
		t.Fatal("语法错误文件应返回 error")
	}
}

func TestIsGenerated(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want bool
	}{
		{"protoc 生成", "// Code generated by protoc-gen-go. DO NOT EDIT.\n\npackage pb\n", true},
		{"mockgen 生成", "// Code generated by MockGen. DO NOT EDIT.\r\npackage mocks\n", true},
		{"普通文件", "package p\n", false},
		{"第二行才有标记(定案:只看首行)", "package p\n// Code generated by x. DO NOT EDIT.\n", false},
		{"前缀相似但不匹配", "// Code generated manually\npackage p\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsGenerated([]byte(tt.src)); got != tt.want {
				t.Errorf("IsGenerated = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExcludedPath(t *testing.T) {
	tests := []struct {
		rel  string
		want bool
	}{
		{"vendor/foo/bar.go", true},
		{"internal/vendor/x.go", true},
		{"internal/auth/testdata/fix.go", true},
		{".knowledge/tree/a.go.yaml", true},
		{"internal/auth/login.go", false},
		{"vendored/x.go", false}, // 只匹配整段
	}
	for _, tt := range tests {
		if got := ExcludedPath(tt.rel); got != tt.want {
			t.Errorf("ExcludedPath(%q) = %v, want %v", tt.rel, got, tt.want)
		}
	}
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	if p := r.ForFile("internal/auth/login.go"); p == nil || p.Language() != "go" {
		t.Errorf("ForFile(.go) 应返回 go 插件")
	}
	if p := r.ForFile("README.md"); p != nil {
		t.Errorf("ForFile(.md) 应返回 nil")
	}
}
