package parser

import (
	"fmt"
	"strings"
	"testing"
)

// R29 批次6:TypeScript/JavaScript 解析器回归。
func TestTypeScriptParsesFunctions(t *testing.T) {
	src := []byte(`import { x } from "y";

// login 登录入口
export function login(user, pass) {
  if (!user) return null;
  return checkAuth(user, pass);
}

function checkAuth(u, p) {
  return p === "secret";
}
`)
	syms, err := TypeScript{}.Parse("auth.ts", src)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, s := range syms {
		names[s.Name] = true
	}
	if !names["login"] {
		t.Errorf("应提取 login 函数,got %v", names)
	}
	if !names["checkAuth"] {
		t.Errorf("应提取 checkAuth 函数,got %v", names)
	}
}

func TestTypeScriptParsesClassAndMethods(t *testing.T) {
	src := []byte(`class UserService {
  private db: Database;

  constructor(db: Database) {
    this.db = db;
  }

  public findUser(id: string): User | null {
    return this.db.query(id);
  }

  async deleteUser(id: string): Promise<void> {
    await this.db.delete(id);
  }
}
`)
	syms, _ := TypeScript{}.Parse("svc.ts", src)
	names := map[string]string{}
	for _, s := range syms {
		names[s.Name] = s.Kind
	}
	if names["UserService"] != "type" {
		t.Errorf("应提取 UserService 为 type(class),got %v", names)
	}
	if names["UserService.findUser"] != "method" {
		t.Errorf("应提取 findUser 为 method,got %v", names)
	}
}

func TestTypeScriptClassHashExcludesMethodBodies(t *testing.T) {
	parse := func(src string) (Symbol, Symbol) {
		t.Helper()
		syms, err := TypeScript{}.Parse("svc.ts", []byte(src))
		if err != nil {
			t.Fatal(err)
		}
		var class, method *Symbol
		for i := range syms {
			switch syms[i].Name {
			case "Service":
				class = &syms[i]
			case "Service.run":
				method = &syms[i]
			}
		}
		if class == nil || method == nil {
			t.Fatalf("缺 class/method: %+v", syms)
		}
		return *class, *method
	}
	base := `class Service extends Base {
  field = 1;
  run(value: string): number { return 1; }
}`
	bodyChanged := `class Service extends Base {
  field = 1;
  run(value: string): number { return 2; }
}`
	fieldChanged := `class Service extends Base {
  field = 2;
  run(value: string): number { return 1; }
}`
	extendsChanged := `class Service extends OtherBase {
  field = 1;
  run(value: string): number { return 1; }
}`

	baseClass, baseMethod := parse(base)
	bodyClass, bodyMethod := parse(bodyChanged)
	if baseClass.Hash != bodyClass.Hash || baseClass.StructHash != bodyClass.StructHash {
		t.Fatal("方法实现变化不得连坐 class 双哈希")
	}
	if baseMethod.Hash == bodyMethod.Hash || baseMethod.StructHash == bodyMethod.StructHash {
		t.Fatal("方法自己的双哈希必须检测实现变化")
	}
	for _, changed := range []string{fieldChanged, extendsChanged} {
		got, _ := parse(changed)
		if baseClass.Hash == got.Hash || baseClass.StructHash == got.StructHash {
			t.Fatal("字段/extends 结构变化必须改变 class 双哈希")
		}
	}
}

func TestTypeScriptHashStable(t *testing.T) {
	src1 := []byte(`function add(a, b) { return a + b; }`)
	src2 := []byte(`/* 注释变更 */ function   add(a,    b) { /* body */ return   a   +   b; }`)
	s1, _ := TypeScript{}.Parse("a.ts", src1)
	s2, _ := TypeScript{}.Parse("a.ts", src2)
	if len(s1) != 1 || len(s2) != 1 {
		t.Fatalf("应各提取 1 个函数,got %d / %d", len(s1), len(s2))
	}
	if s1[0].Hash != s2[0].Hash {
		t.Errorf("横向格式/注释变更后哈希应稳定:%q vs %q", s1[0].Hash, s2[0].Hash)
	}
}

func TestTypeScriptOverloadAndDeclareDoNotStopFileScan(t *testing.T) {
	src := []byte(`declare function ambient(x: string): string;
function choose(x: string): string;
function choose(x: number): number;
function choose(x: unknown) { return x; }
function after() { return 1; }`)
	syms, err := TypeScript{}.Parse("overload.ts", src)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]int{}
	for _, sym := range syms {
		names[sym.Name]++
	}
	if names["ambient"] != 0 || names["choose"] != 1 || names["after"] != 1 || len(syms) != 2 {
		t.Fatalf("overload/declare 只应跳当前签名并继续提取实现: %+v", syms)
	}
}

func TestTypeScriptObjectLiteralMethodsAreNotTopLevelSymbols(t *testing.T) {
	src := []byte(`const obj = {
  fakeMethod() { return 1; },
  nested: { ghost() { return 2; } },
};
function real() { return 3; }`)
	syms, _ := TypeScript{}.Parse("object.ts", src)
	if len(syms) != 1 || syms[0].Name != "real" || syms[0].Kind != "func" {
		t.Fatalf("对象字面量方法不得冒充顶层/class 方法: %+v", syms)
	}
}

func TestTypeScriptRegexLiteralsDoNotCreateOrHideSymbols(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{
			name: "顶层 regex 里的关键字和不平衡花括号",
			src: `const pattern = /class Fake \{\}/;
const openBrace = /\{/;
function real() { return 1; }`,
			want: []string{"real"},
		},
		{
			name: "参数和正文 regex 的 slash/括号",
			src: `function slash(value = /[(){}]/) {
  const delimiter = /\//;
  return delimiter.test(value);
}`,
			want: []string{"slash"},
		},
		{
			name: "class 字段 regex 不伪造方法",
			src: `class Service {
  pattern = /function ghost\(\) \{\}/;
  real() { return /[{}]/.test("{}"); }
}`,
			want: []string{"Service", "Service.real"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			syms, err := TypeScript{}.Parse("regex.ts", []byte(tt.src))
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, len(syms))
			for i := range syms {
				got[i] = syms[i].Name
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("regex 内容破坏符号边界: got %v want %v", got, tt.want)
			}
		})
	}
}

func TestTypeScriptRegexDoesNotTruncateSemanticHash(t *testing.T) {
	parse := func(result int) Symbol {
		t.Helper()
		src := []byte(fmt.Sprintf(`function slash(value) { const delimiter = /\//; return delimiter.test(value) ? %d : 0; }`, result))
		syms, err := TypeScript{}.Parse("regex.ts", src)
		if err != nil || len(syms) != 1 {
			t.Fatalf("Parse = %+v, %v", syms, err)
		}
		return syms[0]
	}
	a, b := parse(1), parse(2)
	if a.Hash == b.Hash || a.StructHash == b.StructHash {
		t.Fatal("escaped slash regex 后的代码变更必须进入双哈希")
	}
}

func TestTypeScriptRegexAfterControlHeadDoesNotCloseBody(t *testing.T) {
	src := []byte(`function guarded(value) {
  if (value) /\}/.test(value);
  return 1;
}
function after() { return 2; }`)
	syms, err := TypeScript{}.Parse("regex.ts", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(syms) != 2 || syms[0].Name != "guarded" || syms[1].Name != "after" {
		t.Fatalf("控制头后的 regex 破坏声明边界: %+v", syms)
	}
	if !strings.Contains(string(syms[0].Body), "return 1") {
		t.Fatalf("regex 内 escaped brace 截断函数正文: %q", syms[0].Body)
	}
}

func TestTypeScriptClassFieldInitializersAreNotMethods(t *testing.T) {
	src := []byte(`class Service {
  handler = function fake() { return 1; };
  factory = { ghost() { return 2; } };
  Ctor = class Inner { nested() { return 3; } };
  arrow = () => { return 4; }
  real() { return 5; }
}`)
	syms, err := TypeScript{}.Parse("fields.ts", src)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, sym := range syms {
		names[sym.Name] = true
	}
	if !names["Service"] || !names["Service.real"] {
		t.Fatalf("真实 class/method 丢失: %+v", syms)
	}
	for _, fake := range []string{"Service.fake", "Service.ghost", "Service.Inner", "Service.nested"} {
		if names[fake] {
			t.Fatalf("字段 initializer 伪造 class method %s: %+v", fake, syms)
		}
	}
}

func TestTypeScriptStructHashRenameImmune(t *testing.T) {
	src1 := []byte(`function compute(x) { return x * 2; }`)
	src2 := []byte(`function calculate(x) { return x * 2; }`)
	s1, _ := TypeScript{}.Parse("a.ts", src1)
	s2, _ := TypeScript{}.Parse("a.ts", src2)
	if len(s1) != 1 || len(s2) != 1 {
		t.Fatal("应各提取 1 个函数")
	}
	// StructHash 改名免疫(仅用于迁移匹配);Hash 改名应变。
	if s1[0].StructHash != s2[0].StructHash {
		t.Errorf("改名后 StructHash 应稳定(迁移匹配用):%q vs %q", s1[0].StructHash, s2[0].StructHash)
	}
	if s1[0].Hash == s2[0].Hash {
		t.Error("改名后 Hash 应变(腐烂检测)")
	}
}

func TestTypeScriptCallableBodyStartsAfterFullSignature(t *testing.T) {
	tests := []struct {
		name string
		src  string
		sym  string
	}{
		{
			name: "解构参数与对象默认值",
			src:  `function choose({ id }: { id: string } = { id: "x" }) { return id; }`,
			sym:  "choose",
		},
		{
			name: "泛型约束与对象返回类型",
			src:  `function choose<T extends { id: string }>({ id }: T): { value: string } { return { value: id }; }`,
			sym:  "choose",
		},
		{
			name: "class 方法对象返回类型",
			src:  `class Picker { choose({ id } = { id: "x" }): { value: string } { return { value: id }; } }`,
			sym:  "Picker.choose",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			syms, err := TypeScript{}.Parse("a.ts", []byte(tt.src))
			if err != nil {
				t.Fatal(err)
			}
			var got *Symbol
			for i := range syms {
				if syms[i].Name == tt.sym {
					got = &syms[i]
					break
				}
			}
			if got == nil {
				t.Fatalf("未提取 %s: %+v", tt.sym, syms)
			}
			if !strings.Contains(string(got.Body), "return") {
				t.Fatalf("符号正文被参数/返回类型大括号截断: %q", got.Body)
			}
			changed := strings.Replace(tt.src, "return", "return /* semantic */ 1 +", 1)
			changedSyms, _ := TypeScript{}.Parse("a.ts", []byte(changed))
			var changedHash string
			for _, s := range changedSyms {
				if s.Name == tt.sym {
					changedHash = s.Hash
				}
			}
			if changedHash == "" || changedHash == got.Hash {
				t.Fatal("真实正文变更必须改变符号哈希")
			}
		})
	}
}

func TestTypeScriptAsyncFunctionCanonicalName(t *testing.T) {
	syms, _ := TypeScript{}.Parse("a.ts", []byte(`export default async function load<T>({ id }: T) { return id; }`))
	if len(syms) != 1 || syms[0].Name != "load" || syms[0].Kind != "func" {
		t.Fatalf("async function 规范名错误: %+v", syms)
	}
}

func TestTypeScriptClassScanSkipsCommentsAndStrings(t *testing.T) {
	src := []byte(`class Service {
  // fake() { return 1; }
  /* ghost(): void { } */
  label = "stringMethod() {}";
  template = ` + "`templateMethod() {}`" + `;
  real() { return 1; }
}`)
	syms, _ := TypeScript{}.Parse("a.ts", src)
	names := map[string]bool{}
	for _, s := range syms {
		names[s.Name] = true
	}
	if !names["Service.real"] || names["Service.fake"] || names["Service.ghost"] ||
		names["Service.stringMethod"] || names["Service.templateMethod"] {
		t.Fatalf("class 二次扫描误建符号: %v", names)
	}
}

func TestJavaScriptASIReturnNewlineChangesHash(t *testing.T) {
	oneLine, _ := TypeScript{}.Parse("a.js", []byte("function value() { return { ok: true }; }"))
	asi, _ := TypeScript{}.Parse("a.js", []byte("function value() { return\n{ ok: true }; }"))
	if len(oneLine) != 1 || len(asi) != 1 {
		t.Fatalf("应各提取一个函数: %d/%d", len(oneLine), len(asi))
	}
	if oneLine[0].Hash == asi[0].Hash {
		t.Fatal("return\\n{} 触发 ASI，不能与 return {} 得到同一哈希")
	}
}

func TestJavaScriptLineTerminatorsAreConservativelyHashSensitive(t *testing.T) {
	tests := []struct {
		name, sameLine, withNewline string
	}{
		{"break label", "break outer;", "break\nouter;"},
		{"continue label", "continue outer;", "continue\nouter;"},
		{"throw", "throw new Error();", "throw\nnew Error();"},
		{"yield", "yield item;", "yield\nitem;"},
		{"postfix boundary", "value ++other;", "value\n++other;"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wrap := func(stmt string) []byte {
				return []byte("function value() { " + stmt + " }")
			}
			a, _ := TypeScript{}.Parse("a.js", wrap(tt.sameLine))
			b, _ := TypeScript{}.Parse("a.js", wrap(tt.withNewline))
			if len(a) != 1 || len(b) != 1 {
				t.Fatalf("应各提取一个函数: %+v / %+v", a, b)
			}
			if a[0].Hash == b[0].Hash {
				t.Fatalf("LineTerminator 可能改变 JS 语义，哈希必须 fail closed: %q", tt.withNewline)
			}
			if (TypeScript{}).HashFile(wrap(tt.sameLine)) == (TypeScript{}).HashFile(wrap(tt.withNewline)) {
				t.Fatal("文件级哈希也不得吞掉 LineTerminator")
			}
		})
	}
}

func TestTypeScriptStringContentNotMatched(t *testing.T) {
	// 字符串里的 "function" 不该被当声明。
	src := []byte("const desc = \"this function does X\";\nconst tmpl = `function fake() {}`;\n")
	syms, _ := TypeScript{}.Parse("a.ts", src)
	for _, s := range syms {
		if s.Name == "fake" || s.Name == "does" {
			t.Errorf("字符串内容不该被当函数声明提取:%+v", s)
		}
	}
}

func TestTypeScriptRegistryRegistered(t *testing.T) {
	r := NewRegistry()
	for _, ext := range []string{".ts", ".tsx", ".js", ".jsx", ".mjs"} {
		if r.ForFile("test"+ext) == nil {
			t.Errorf("扩展名 %s 应注册 TypeScript 解析器", ext)
		}
	}
}
