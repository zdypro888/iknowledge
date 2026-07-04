package model

import (
	"regexp"
	"testing"
	"time"
)

func TestNodeIDGrammar(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"file node", FileNodeID("internal/auth/login.go"), "internal/auth/login.go"},
		{"symbol node", SymbolNodeID("internal/auth/login.go", "Login"), "internal/auth/login.go#Login"},
		{"method node", SymbolNodeID("a.go", "AuthService.SignIn"), "a.go#AuthService.SignIn"},
		{"dir node", DirNodeID("internal/auth"), "internal/auth/"},
		{"dir node trailing slash idempotent", DirNodeID("internal/auth/"), "internal/auth/"},
		{"root dir is project", DirNodeID("."), "."},
		{"empty dir is project", DirNodeID(""), "."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %q, want %q", tt.got, tt.want)
			}
		})
	}
}

func TestSplitNodeID(t *testing.T) {
	tests := []struct {
		id         string
		wantFile   string
		wantSymbol string
	}{
		{"internal/auth/login.go#Login", "internal/auth/login.go", "Login"},
		{"internal/auth/login.go", "internal/auth/login.go", ""},
		{"internal/auth/", "internal/auth/", ""},
		{".", ".", ""},
		{"a.go#init~2", "a.go", "init~2"},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			f, s := SplitNodeID(tt.id)
			if f != tt.wantFile || s != tt.wantSymbol {
				t.Errorf("SplitNodeID(%q) = (%q, %q), want (%q, %q)", tt.id, f, s, tt.wantFile, tt.wantSymbol)
			}
		})
	}
}

func TestBaseSymbol(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"init", "init"},
		{"init~2", "init"},
		{"init~13", "init"},
		{"_~3", "_"},
		{"Login", "Login"},
		{"weird~name", "weird~name"}, // 非数字后缀不剥
		{"x~", "x~"},
	}
	for _, tt := range tests {
		if got := BaseSymbol(tt.in); got != tt.want {
			t.Errorf("BaseSymbol(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestLooseSymbolMatch(t *testing.T) {
	tests := []struct {
		got, want string
		match     bool
	}{
		{"AuthService.SignIn", "AuthService.SignIn", true},
		{"SignIn", "AuthService.SignIn", true},                // 忽略接收者
		{"(*AuthService).SignIn", "AuthService.SignIn", true}, // 忽略指针与括号
		{"*AuthService.SignIn", "AuthService.SignIn", true},
		{"Login", "AuthService.SignIn", false},
		{"SignIn", "SignIn", true},
		{"Service.SignIn", "AuthService.SignIn", false}, // 接收者不同不算命中
	}
	for _, tt := range tests {
		if got := LooseSymbolMatch(tt.got, tt.want); got != tt.match {
			t.Errorf("LooseSymbolMatch(%q, %q) = %v, want %v", tt.got, tt.want, got, tt.match)
		}
	}
}

func TestNewEntryID(t *testing.T) {
	re := regexp.MustCompile(`^e_[0-9a-f]{8}$`)
	seen := map[string]bool{}
	for range 100 {
		id := NewEntryID()
		if !re.MatchString(id) {
			t.Fatalf("NewEntryID() = %q, want match %v", id, re)
		}
		if seen[id] {
			t.Fatalf("NewEntryID() 重复:%q", id)
		}
		seen[id] = true
	}
}

func TestNewChangeID(t *testing.T) {
	at := time.Date(2026, 6, 20, 10, 32, 0, 0, time.UTC)
	re := regexp.MustCompile(`^chg_20260620T103200Z_[0-9a-f]{16}$`)
	a, b := NewChangeID(at), NewChangeID(at)
	if !re.MatchString(a) {
		t.Fatalf("NewChangeID() = %q, want match %v", a, re)
	}
	if a == b {
		t.Fatalf("同时刻两次 NewChangeID 相同:%q(随机段失效)", a)
	}
	// 非 UTC 输入必须归一到 UTC(多机多时区下 ID 的时间段才可比)。
	jst := time.FixedZone("JST", 9*3600)
	c := NewChangeID(time.Date(2026, 6, 20, 19, 32, 0, 0, jst))
	if !re.MatchString(c) {
		t.Fatalf("非 UTC 输入未归一:%q", c)
	}
}
