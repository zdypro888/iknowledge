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
				if syms[i].Hash == "" || syms[i].StructHash == "" {
					t.Errorf("符号 %q 双哈希缺失", syms[i].Name)
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
			name: "搬家到别的文件(双哈希均不变)",
			srcA: "package p\n\n// Helper 工具。\nfunc Helper() int {\n\treturn 7\n}\n",
			srcB: "package other\n\nimport \"fmt\"\n\nfunc init() { fmt.Println() }\n\n// Helper 工具。\nfunc Helper() int {\n\treturn 7\n}\n",
			symA: "Helper", symB: "Helper",
			wantHashSame: true, wantStructSame: true,
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
			s1[i].Start != s2[i].Start || s1[i].End != s2[i].End {
			t.Errorf("符号[%d] 两次解析不一致:%+v vs %+v", i, s1[i], s2[i])
		}
	}
}

func TestFileHash(t *testing.T) {
	symsA := parseAll(t, "package p\n\nfunc A() {}\n\nfunc B() {}\n")
	// import 重排/文件头注释变化不影响符号 → 文件哈希不变。
	symsB := parseAll(t, "package p\n\nimport _ \"embed\"\n\nfunc A() {}\n\nfunc B() {}\n")
	if FileHash(symsA) != FileHash(symsB) {
		t.Errorf("import 变化不应影响文件哈希")
	}
	symsC := parseAll(t, "package p\n\nfunc A() { _ = 1 }\n\nfunc B() {}\n")
	if FileHash(symsA) == FileHash(symsC) {
		t.Errorf("函数体变化必须影响文件哈希")
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
