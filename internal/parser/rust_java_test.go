package parser

import "testing"

// ---- Rust 解析器回归(R29-续)----

func TestRustParsesFunctions(t *testing.T) {
	src := []byte(`pub fn login(user: &str, pass: &str) -> Result<(), Error> {
    if user.is_empty() {
        return Err(Error::Empty);
    }
    check_auth(user, pass)
}

fn check_auth(u: &str, p: &str) -> bool {
    p == "secret"
}
`)
	syms, err := Rust{}.Parse("auth.rs", src)
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
	if !names["check_auth"] {
		t.Errorf("应提取 check_auth 函数,got %v", names)
	}
}

func TestRustParsesImplMethods(t *testing.T) {
	src := []byte(`struct UserService {
    db: Database,
}

impl UserService {
    pub fn find_user(&self, id: &str) -> Option<User> {
        self.db.query(id)
    }

    fn validate(&self, u: &User) -> bool {
        u.active
    }
}

impl<T: Clone> Repository<T> for UserService {
    fn save(&self, item: T) {
        // ...
    }
}
`)
	syms, _ := Rust{}.Parse("svc.rs", src)
	names := map[string]string{}
	for _, s := range syms {
		names[s.Name] = s.Kind
	}
	if names["UserService"] != "type" {
		t.Errorf("应提取 UserService 为 type(struct),got %v", names)
	}
	// impl 块内方法应有 Class.method 规范名
	if names["UserService.find_user"] != "method" {
		t.Errorf("应提取 UserService.find_user 方法,got %v", names)
	}
}

func TestRustHashStable(t *testing.T) {
	src1 := []byte(`fn add(a: i32, b: i32) -> i32 { a + b }`)
	src2 := []byte(`// 注释变更
fn add(a: i32, b: i32) -> i32 {
    a + b
}`)
	s1, _ := Rust{}.Parse("a.rs", src1)
	s2, _ := Rust{}.Parse("a.rs", src2)
	if len(s1) != 1 || len(s2) != 1 {
		t.Fatalf("应各提取 1 个函数")
	}
	if s1[0].Hash != s2[0].Hash {
		t.Errorf("格式/注释变更后哈希应稳定:%q vs %q", s1[0].Hash, s2[0].Hash)
	}
}

func TestRustStructHashRenameImmune(t *testing.T) {
	s1, _ := Rust{}.Parse("a.rs", []byte(`fn compute(x: i32) -> i32 { x * 2 }`))
	s2, _ := Rust{}.Parse("a.rs", []byte(`fn calculate(x: i32) -> i32 { x * 2 }`))
	if len(s1) != 1 || len(s2) != 1 {
		t.Fatal("应各提取 1 个函数")
	}
	if s1[0].StructHash != s2[0].StructHash {
		t.Errorf("改名后 StructHash 应稳定:%q vs %q", s1[0].StructHash, s2[0].StructHash)
	}
	if s1[0].Hash == s2[0].Hash {
		t.Error("改名后 Hash 应变")
	}
}

// ---- Java 解析器回归 ----

func TestJavaParsesClassAndMethods(t *testing.T) {
	src := []byte(`package com.example;

import java.util.List;

public class UserService {
    private Database db;

    public UserService(Database db) {
        this.db = db;
    }

    public User findUser(String id) throws NotFoundException {
        return db.query(id);
    }

    private boolean validate(User u) {
        return u.isActive();
    }

    static String formatName(User u) {
        return u.getName();
    }
}
`)
	syms, _ := Java{}.Parse("Svc.java", src)
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
	if names["UserService.validate"] != "method" {
		t.Errorf("应提取 validate 为 method,got %v", names)
	}
}

func TestJavaHashStable(t *testing.T) {
	src1 := []byte(`int add(int a, int b) { return a + b; }`)
	src2 := []byte(`// 注释变更
int add(int a, int b) {
    return a + b;
}`)
	// 加 class 包裹(Java 方法需在类内)
	wrap := func(b []byte) []byte {
		return []byte("class C {\n" + string(b) + "\n}")
	}
	s1, _ := Java{}.Parse("a.java", wrap(src1))
	s2, _ := Java{}.Parse("a.java", wrap(src2))
	if len(s1) == 0 || len(s2) == 0 {
		t.Fatal("应提取到方法")
	}
	// 找 add 方法
	var m1, m2 *Symbol
	for i := range s1 {
		if s1[i].Kind == "method" {
			m1 = &s1[i]
		}
	}
	for i := range s2 {
		if s2[i].Kind == "method" {
			m2 = &s2[i]
		}
	}
	if m1 == nil || m2 == nil {
		t.Fatal("应提取到 method")
	}
	if m1.Hash != m2.Hash {
		t.Errorf("格式/注释变更后哈希应稳定:%q vs %q", m1.Hash, m2.Hash)
	}
}

func TestRustJavaRegistryRegistered(t *testing.T) {
	r := NewRegistry()
	if r.ForFile("test.rs") == nil {
		t.Error(".rs 应注册 Rust 解析器")
	}
	if r.ForFile("Test.java") == nil {
		t.Error(".java 应注册 Java 解析器")
	}
}
