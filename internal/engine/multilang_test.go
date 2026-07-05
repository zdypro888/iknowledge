package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zdypro888/iknowledge/internal/parser"
)

// T0 通用文件级覆盖:extensions 白名单 → 文件节点 + 内容哈希腐烂检测 + 账本可用。
func TestGenericExtensionCoverage(t *testing.T) {
	e, repo := newRepo(t, map[string]string{
		".knowledge/config.yaml": "schema: 1\nport: 18999\nextensions: [\".proto\", \"sql\"]\n",
		"api/v1.proto":           "syntax = \"proto3\";\nmessage Ping { int32 seq = 1; }\n",
		"db/schema.sql":          "CREATE TABLE users (id INT PRIMARY KEY);\n",
		"main.go":                "package main\n\nfunc main() {}\n",
	})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	sid := "s-ml"

	// 两种扩展名(带点/不带点)都建了文件节点;Go 专职插件不受影响。
	for _, q := range []string{"api/v1.proto", "db/schema.sql", "main.go#main"} {
		out, meta, err := e.Recall(RecallArgs{Query: q}, sid)
		if err != nil || !meta.Hit {
			t.Fatalf("%s 应命中:%v %v\n%s", q, err, meta.Hit, out)
		}
	}

	// 账本与经验挂 proto 文件节点。
	if _, err := e.Remember(RememberArgs{
		Node:    "api/v1.proto",
		Entries: []RememberEntry{{Kind: "contract", Text: "seq 从 1 起单调递增,0 保留作探活"}},
	}, sid, "claude-code"); err != nil {
		t.Fatal(err)
	}

	// 内容哈希腐烂检测:改 proto 不记账 → init 对账降 suspect。
	if err := os.WriteFile(filepath.Join(repo, "api", "v1.proto"),
		[]byte("syntax = \"proto3\";\nmessage Ping { int32 seq = 1; bool ack = 2; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	out, _, err := e.Recall(RecallArgs{Query: "api/v1.proto"}, sid)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "suspect") {
		t.Errorf("通用文件改动应触发腐烂检测:\n%s", out)
	}
}

// T1 Python 端到端:符号级节点 + 沉淀 + 改名迁移(StructHash 全语言通用机制)。
func TestPythonEndToEnd(t *testing.T) {
	if !parser.PythonAvailable() {
		t.Skip("无 python3")
	}
	e, repo := newRepo(t, map[string]string{
		"svc/app.py": "class Worker:\n    def run(self, n):\n        return n * 2\n\ndef helper(x):\n    return x + 1\n",
	})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	sid := "s-py"

	out, meta, err := e.Recall(RecallArgs{Query: "svc/app.py#Worker.run"}, sid)
	if err != nil || !meta.Hit {
		t.Fatalf("Python 方法节点应命中:%v %v\n%s", err, meta.Hit, out)
	}
	if _, err := e.Remember(RememberArgs{
		Node:    "svc/app.py#Worker.run",
		Entries: []RememberEntry{{Kind: "pitfall", Text: "n 为负时结果语义未定义,调用方须先钳制"}},
	}, sid, "claude-code"); err != nil {
		t.Fatal(err)
	}

	// 改名 → StructHash 迁移,知识随符号走(全语言通用的自愈机制在 Python 上成立)。
	if err := os.WriteFile(filepath.Join(repo, "svc", "app.py"),
		[]byte("class Worker:\n    def execute(self, n):\n        return n * 2\n\ndef helper(x):\n    return x + 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := e.Init(InitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Migrated != 1 {
		t.Fatalf("改名应触发 StructHash 迁移(migrated=%d)", rep.Migrated)
	}
	out, meta, err = e.Recall(RecallArgs{Query: "svc/app.py#Worker.execute"}, sid)
	if err != nil || !meta.Hit || !strings.Contains(out, "调用方须先钳制") {
		t.Errorf("迁移后知识应随新符号可见:\n%s", out)
	}
}
