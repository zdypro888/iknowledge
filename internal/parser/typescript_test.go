package parser

import (
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

func TestTypeScriptHashStable(t *testing.T) {
	src1 := []byte(`function add(a, b) { return a + b; }`)
	src2 := []byte(`// 注释变更
function add(a, b) {
  return a + b;
}`)
	s1, _ := TypeScript{}.Parse("a.ts", src1)
	s2, _ := TypeScript{}.Parse("a.ts", src2)
	if len(s1) != 1 || len(s2) != 1 {
		t.Fatalf("应各提取 1 个函数,got %d / %d", len(s1), len(s2))
	}
	if s1[0].Hash != s2[0].Hash {
		t.Errorf("格式/注释变更后哈希应稳定(归一化免疫):%q vs %q", s1[0].Hash, s2[0].Hash)
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
