package engine

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/zdypro888/iknowledge/internal/model"
	"github.com/zdypro888/iknowledge/internal/store"
)

func TestInitRejectsSourceSymlinkOutsideRepository(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("Windows symlink 需要额外权限")
	}
	e, repo := newRepo(t, nil)
	outside := filepath.Join(t.TempDir(), "private.go")
	secret := `package private
const APISecret = "TOP_SECRET_MUST_NOT_BE_INDEXED"
`
	if err := os.WriteFile(outside, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "leak.go")); err != nil {
		t.Fatal(err)
	}
	rep, err := e.Init(InitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Files != 0 {
		t.Fatalf("仓外 symlink 不应进入源码清单:%+v", rep)
	}
	if _, err := safeRepoRead(repo, "leak.go"); err == nil {
		t.Fatal("统一源码读取必须拒绝最终 symlink")
	}
	if _, err := os.Stat(e.Store.ShardPathFor("leak.go")); !os.IsNotExist(err) {
		t.Fatalf("仓外源码不应生成知识分片:%v", err)
	}
}

func TestInitRecoversPreparedConfigBeforeBuildingRegistry(t *testing.T) {
	e, _ := newRepo(t, map[string]string{"a.go": "package a\n"})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	before, err := e.Store.ReadKnowledgeFile("config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Store.PrepareTruthTransaction([]string{"config.yaml"}); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.WriteKnowledgeFile("config.yaml", []byte("schema: 1\nport: 18001\nextensions: [.foo]\n")); err != nil {
		t.Fatal(err)
	}
	fresh := New(e.Store) // 构造时故意观察到半事务 .foo 配置。
	if fresh.Reg.ForFile("x.foo") == nil {
		t.Fatal("测试前置无效:registry 未观察到半事务 config")
	}
	if _, err := fresh.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	after, err := fresh.Store.ReadKnowledgeFile("config.yaml")
	if err != nil || string(after) != string(before) {
		t.Fatalf("init 未先恢复 config before-image:after=%q err=%v", after, err)
	}
	if fresh.Reg.ForFile("x.foo") != nil {
		t.Fatal("init 恢复后仍沿用半事务 parser registry")
	}
}

// newRepo 建一个临时仓库(无 git,走 WalkDir 回退;git 路径由 e2e 覆盖)。
func newRepo(t *testing.T, files map[string]string) (*Engine, string) {
	t.Helper()
	t.Setenv("IKNOWLEDGE_STATE_HOME", t.TempDir())
	repo := t.TempDir()
	writeFiles(t, repo, files)
	s, err := store.Open(repo)
	if err != nil {
		t.Fatal(err)
	}
	e := New(s)
	e.now = func() time.Time { return time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC) }
	return e, repo
}

func writeFiles(t *testing.T, repo string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func loadNodes(t *testing.T, e *Engine, srcRel string) map[string]model.Node {
	t.Helper()
	sh, _, err := e.Store.LoadShard(e.Store.ShardPathFor(srcRel))
	if err != nil {
		t.Fatalf("LoadShard(%s): %v", srcRel, err)
	}
	out := map[string]model.Node{}
	for _, n := range sh.Nodes {
		out[n.ID] = n
	}
	return out
}

const loginSrc = `package auth

// Login 登录入口;pass 传明文。
func Login(user, pass string) error {
	if user == "" {
		return nil
	}
	return check(user, pass)
}

func check(u, p string) error { return nil }

var maxRetries = 3

type Session struct{ ID string }
`

func TestInitCreatesSkeleton(t *testing.T) {
	e, _ := newRepo(t, map[string]string{
		"internal/auth/login.go": loginSrc,
		"main.go":                "package main\n\nfunc main() {}\n",
	})
	rep, err := e.Init(InitOptions{})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if rep.Files != 2 || rep.ParseFailed != 0 {
		t.Errorf("Files=%d ParseFailed=%d, want 2/0", rep.Files, rep.ParseFailed)
	}
	// login.go 分片:file + Login + check + maxRetries + Session = 5 节点。
	nodes := loadNodes(t, e, "internal/auth/login.go")
	wantIDs := []string{
		"internal/auth/login.go",
		"internal/auth/login.go#Login",
		"internal/auth/login.go#check",
		"internal/auth/login.go#maxRetries",
		"internal/auth/login.go#Session",
	}
	for _, id := range wantIDs {
		n, ok := nodes[id]
		if !ok {
			t.Fatalf("缺节点 %s(有:%v)", id, keys(nodes))
		}
		if n.Status != model.StatusUndigested {
			t.Errorf("%s 初始状态 = %s, want undigested", id, n.Status)
		}
	}
	if nodes["internal/auth/login.go#maxRetries"].Level != model.LevelDecl {
		t.Errorf("var 节点应为 decl 层")
	}
	if nodes["internal/auth/login.go#Login"].Level != model.LevelFunction {
		t.Errorf("函数节点应为 function 层")
	}
	// 目录节点与项目节点。
	for _, p := range []string{
		e.Store.DirShardPathFor("internal/auth"),
		e.Store.DirShardPathFor("internal"),
		e.Store.ProjectShardPath(),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("缺分片 %s", p)
		}
	}
	// git 配套文件。
	if !e.Store.GitFilesOK() {
		t.Error(".gitattributes/.gitignore 未生成")
	}
	// 端口配置。
	cfg, err := e.Store.LoadConfig()
	if err != nil || cfg == nil || cfg.Port < 18000 {
		t.Errorf("config.yaml 未生成或端口非法:%+v, %v", cfg, err)
	}
}

func TestInitIdempotent(t *testing.T) {
	e, _ := newRepo(t, map[string]string{"a.go": loginSrc})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	path := e.Store.ShardPathFor("a.go")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	st1, _ := os.Stat(path)

	rep, err := e.Init(InitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Created != 0 || rep.Suspected != 0 || rep.Migrated != 0 || rep.Orphaned != 0 {
		t.Errorf("重复 init 应零变化:%+v", rep)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("重复 init 改变了分片内容")
	}
	st2, _ := os.Stat(path)
	if !st1.ModTime().Equal(st2.ModTime()) {
		t.Error("重复 init 重写了未变化的分片(mtime 变化,幂等性破坏)")
	}
}

// digest 模拟 AI 消化:给节点挂一条知识(M1.3 前直接操作分片)。
func digest(t *testing.T, e *Engine, srcRel, nodeID, text string) {
	t.Helper()
	path := e.Store.ShardPathFor(srcRel)
	sh, raw, err := e.Store.LoadShard(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range sh.Nodes {
		if sh.Nodes[i].ID == nodeID {
			sh.Nodes[i].Entries = append(sh.Nodes[i].Entries, model.Entry{
				ID: model.NewEntryID(), Kind: model.KindPitfall,
				Text: text, Confidence: model.ConfidenceInferred,
			})
			sh.Nodes[i].Status = model.StatusFresh
			if err := e.Store.SaveShard(path, sh, raw); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatalf("digest: 节点 %s 不存在", nodeID)
}

// TestRenameMigration 改名 → StructHash 双向唯一命中 → 自动迁移,知识无损(附录 E)。
func TestRenameMigration(t *testing.T) {
	e, repo := newRepo(t, map[string]string{"auth/login.go": loginSrc})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	digest(t, e, "auth/login.go", "auth/login.go#Login", "不要在调用方预先加密")

	// 改名 Login → Authenticate(doc comment 里的名字也会跟着改,现实场景)。
	renamed := strings.ReplaceAll(loginSrc, "Login", "Authenticate")
	writeFiles(t, repo, map[string]string{"auth/login.go": renamed})

	rep, err := e.Init(InitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Migrated != 1 || rep.Orphaned != 0 {
		t.Fatalf("Migrated=%d Orphaned=%d, want 1/0(报告:%s)", rep.Migrated, rep.Orphaned, rep.Text())
	}
	nodes := loadNodes(t, e, "auth/login.go")
	n, ok := nodes["auth/login.go#Authenticate"]
	if !ok {
		t.Fatalf("迁移目标节点不存在:%v", keys(nodes))
	}
	if len(n.Entries) != 1 || n.Entries[0].Text != "不要在调用方预先加密" {
		t.Errorf("Entries 未随迁移带走:%+v", n.Entries)
	}
	if len(n.Lineage) != 1 || n.Lineage[0] != "auth/login.go#Login" {
		t.Errorf("血缘未接续:%v", n.Lineage)
	}
	if n.Status != model.StatusFresh {
		t.Errorf("迁移后状态 = %s, want fresh", n.Status)
	}
	if _, exists := nodes["auth/login.go#Login"]; exists {
		t.Error("旧 ID 节点应随迁移消失")
	}
}

func TestRenameWithContractDocChangeMigratesAsSuspect(t *testing.T) {
	e, repo := newRepo(t, map[string]string{"auth/login.go": loginSrc})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	digest(t, e, "auth/login.go", "auth/login.go#Login", "调用方必须传明文")
	old := loadNodes(t, e, "auth/login.go")["auth/login.go#Login"]

	changed := strings.ReplaceAll(loginSrc, "Login", "Authenticate")
	changed = strings.Replace(changed, "pass 传明文", "pass 传预哈希值", 1)
	writeFiles(t, repo, map[string]string{"auth/login.go": changed})
	rep, err := e.Init(InitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Migrated != 1 || rep.Suspected != 1 {
		t.Fatalf("改名+契约变更应保留迁移但待重验: %s", rep.Text())
	}
	n := loadNodes(t, e, "auth/login.go")["auth/login.go#Authenticate"]
	if n.Status != model.StatusSuspect || len(n.Entries) != 1 {
		t.Fatalf("迁移知识应保留并降 suspect: %+v", n)
	}
	if n.Anchor.Hash != old.Anchor.Hash {
		t.Fatal("suspect 迁移必须保留旧 Hash 基线，不能提前重锚")
	}
	if n.Anchor.DocStructHash == "" || n.Anchor.DocStructHash == old.Anchor.DocStructHash {
		t.Fatal("目标 DocStructHash 应记录新契约且与旧护栏失配")
	}
	// 再跑 init 不能因 ID 已稳定就把 suspect 洗回 fresh。
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	if again := loadNodes(t, e, "auth/login.go")["auth/login.go#Authenticate"]; again.Status != model.StatusSuspect || again.Anchor.Hash != old.Anchor.Hash {
		t.Fatalf("二次 init 错误洗白迁移: %+v", again)
	}
}

func TestLegacyRenameWithoutDocGuardIsConservativelySuspect(t *testing.T) {
	e, repo := newRepo(t, map[string]string{"auth/login.go": loginSrc})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	digest(t, e, "auth/login.go", "auth/login.go#Login", "旧库知识")
	path := e.Store.ShardPathFor("auth/login.go")
	sh, raw, err := e.Store.LoadShard(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range sh.Nodes {
		if sh.Nodes[i].ID == "auth/login.go#Login" {
			sh.Nodes[i].Anchor.DocStructHash = "" // 模拟轮 34 前的存量分片
		}
	}
	if err := e.Store.SaveShard(path, sh, raw); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, repo, map[string]string{"auth/login.go": strings.ReplaceAll(loginSrc, "Login", "Authenticate")})
	rep, err := e.Init(InitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	n := loadNodes(t, e, "auth/login.go")["auth/login.go#Authenticate"]
	if rep.Migrated != 1 || n.Status != model.StatusSuspect || len(n.Entries) != 1 {
		t.Fatalf("缺迁移护栏的旧库只能保守迁移: report=%s node=%+v", rep.Text(), n)
	}
}

// TestMoveAcrossFiles 搬家(原样移动到别的文件)→ 精确迁移。
func TestMoveAcrossFiles(t *testing.T) {
	e, repo := newRepo(t, map[string]string{"a.go": loginSrc})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	digest(t, e, "a.go", "a.go#Login", "坑")

	// Login 挪去 b.go,a.go 剩其余符号。
	writeFiles(t, repo, map[string]string{
		"a.go": "package auth\n\nfunc check(u, p string) error { return nil }\n\nvar maxRetries = 3\n\ntype Session struct{ ID string }\n",
		"b.go": "package auth\n\n// Login 登录入口;pass 传明文。\nfunc Login(user, pass string) error {\n\tif user == \"\" {\n\t\treturn nil\n\t}\n\treturn check(user, pass)\n}\n",
	})
	rep, err := e.Init(InitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Migrated != 1 {
		t.Fatalf("Migrated = %d, want 1(%s)", rep.Migrated, rep.Text())
	}
	nodes := loadNodes(t, e, "b.go")
	n := nodes["b.go#Login"]
	if len(n.Entries) != 1 || n.Lineage[0] != "a.go#Login" {
		t.Errorf("搬家迁移失败:%+v", n)
	}
}

// TestTwinBodiesNotMigrated 孪生函数体(复制粘贴)→ 多重命中 → 不迁,标孤儿(impl §6)。
func TestTwinBodiesNotMigrated(t *testing.T) {
	body := "(x int) int {\n\treturn x * 2\n}\n"
	e, repo := newRepo(t, map[string]string{"a.go": "package p\n\nfunc Double" + body})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	digest(t, e, "a.go", "a.go#Double", "溢出未处理")

	// Double 消失,出现两个结构相同的孪生(占位后同构)。
	writeFiles(t, repo, map[string]string{
		"a.go": "package p\n\nfunc Twice" + body + "\nfunc Dup" + body,
	})
	rep, err := e.Init(InitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Migrated != 0 || rep.Orphaned != 1 {
		t.Fatalf("Migrated=%d Orphaned=%d, want 0/1——宁可人工认领,不可错挂", rep.Migrated, rep.Orphaned)
	}
	nodes := loadNodes(t, e, "a.go")
	orphan, ok := nodes["a.go#Double"]
	if !ok || orphan.Status != model.StatusOrphaned {
		t.Errorf("旧节点应为 orphaned 保留:%+v", nodes)
	}
	if len(orphan.Entries) != 1 {
		t.Errorf("孤儿的知识必须保留")
	}
}

// TestGofmtImmunity 全库 gofmt 重排 → 零 suspect(推演五 #5 的解药)。
func TestGofmtImmunity(t *testing.T) {
	messy := "package auth\n\n// Login 入口。\nfunc Login( user,pass string )error{\n    if user==\"\"{\n\treturn nil}\n  return nil\n}\n\nvar maxRetries=3\n"
	clean := "package auth\n\n// Login 入口。\nfunc Login(user, pass string) error {\n\tif user == \"\" {\n\t\treturn nil\n\t}\n\treturn nil\n}\n\nvar maxRetries = 3\n"
	e, repo := newRepo(t, map[string]string{"a.go": messy})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	digest(t, e, "a.go", "a.go#Login", "坑")

	writeFiles(t, repo, map[string]string{"a.go": clean})
	rep, err := e.Init(InitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Suspected != 0 {
		t.Fatalf("gofmt 后 Suspected = %d, want 0(锚定哈希未免疫格式化)", rep.Suspected)
	}
	if n := loadNodes(t, e, "a.go")["a.go#Login"]; n.Status != model.StatusFresh {
		t.Errorf("gofmt 后状态 = %s, want fresh", n.Status)
	}
}

// TestCodeChangeSuspect 有知识的节点代码变更 → suspect,且保留旧锚(重验即重锚前提)。
func TestCodeChangeSuspect(t *testing.T) {
	e, repo := newRepo(t, map[string]string{"a.go": loginSrc})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	digest(t, e, "a.go", "a.go#Login", "坑")
	oldHash := loadNodes(t, e, "a.go")["a.go#Login"].Anchor.Hash

	changed := strings.Replace(loginSrc, "return check(user, pass)", "return nil // 改动", 1)
	writeFiles(t, repo, map[string]string{"a.go": changed})
	rep, err := e.Init(InitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Suspected != 1 {
		t.Fatalf("Suspected = %d, want 1", rep.Suspected)
	}
	n := loadNodes(t, e, "a.go")["a.go#Login"]
	if n.Status != model.StatusSuspect {
		t.Fatalf("状态 = %s, want suspect", n.Status)
	}
	if n.Anchor.Hash != oldHash {
		t.Error("suspect 必须保留旧锚(否则代码回退无从检测、重验即重锚失去基准)")
	}
	if len(n.Entries) != 1 {
		t.Error("对账绝不动已有 Entries")
	}

	// 代码回退到锚定状态 → 自动回 fresh。
	writeFiles(t, repo, map[string]string{"a.go": loginSrc})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	if n := loadNodes(t, e, "a.go")["a.go#Login"]; n.Status != model.StatusFresh {
		t.Errorf("代码回退后状态 = %s, want fresh", n.Status)
	}
}

// TestUndigestedNoSuspect undigested 无知识可腐:代码变更仅重锚,不降 suspect。
func TestUndigestedNoSuspect(t *testing.T) {
	e, repo := newRepo(t, map[string]string{"a.go": loginSrc})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(loginSrc, "user == \"\"", "user == \"x\"", 1)
	writeFiles(t, repo, map[string]string{"a.go": changed})
	rep, err := e.Init(InitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Suspected != 0 {
		t.Errorf("undigested 不应降 suspect(Suspected=%d)", rep.Suspected)
	}
	n := loadNodes(t, e, "a.go")["a.go#Login"]
	if n.Status != model.StatusUndigested {
		t.Errorf("状态 = %s, want undigested", n.Status)
	}
}

// TestDeletedFile 源文件删除:有知识 → 孤儿保留;无知识 → 分片清除。
func TestDeletedFile(t *testing.T) {
	e, repo := newRepo(t, map[string]string{"a.go": loginSrc, "b.go": "package p\n\nfunc B() {}\n"})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	digest(t, e, "a.go", "a.go#Login", "坑")

	if err := os.Remove(filepath.Join(repo, "a.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repo, "b.go")); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, repo, map[string]string{"c.go": "package p\n\nfunc C() {}\n"})

	rep, err := e.Init(InitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Orphaned != 1 {
		t.Fatalf("Orphaned = %d, want 1(仅 Login 有知识;%s)", rep.Orphaned, rep.Text())
	}
	// a.go 分片保留(只剩孤儿);b.go 分片应删除。
	nodes := loadNodes(t, e, "a.go")
	if n, ok := nodes["a.go#Login"]; !ok || n.Status != model.StatusOrphaned || len(n.Entries) != 1 {
		t.Errorf("孤儿保留失败:%+v", nodes)
	}
	if _, err := os.Stat(e.Store.ShardPathFor("b.go")); !os.IsNotExist(err) {
		t.Error("无知识文件的分片应清除")
	}
}

// TestParseFailedUntouched 语法错误文件:跳过计数,分片保持原样(impl §5 解析失败三态)。
func TestParseFailedUntouched(t *testing.T) {
	e, repo := newRepo(t, map[string]string{"a.go": loginSrc})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	digest(t, e, "a.go", "a.go#Login", "坑")
	before, _ := os.ReadFile(e.Store.ShardPathFor("a.go"))

	writeFiles(t, repo, map[string]string{"a.go": "package auth\n\nfunc Broken( {\n"})
	rep, err := e.Init(InitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.ParseFailed != 1 {
		t.Fatalf("ParseFailed = %d, want 1", rep.ParseFailed)
	}
	after, _ := os.ReadFile(e.Store.ShardPathFor("a.go"))
	if string(before) != string(after) {
		t.Error("解析失败的文件分片被改动了")
	}
}

// TestExclusions vendor/testdata/生成代码不索引(M1.1 验收点)。
func TestExclusions(t *testing.T) {
	e, _ := newRepo(t, map[string]string{
		"a.go":              "package p\n\nfunc A() {}\n",
		"vendor/dep/x.go":   "package dep\n\nfunc X() {}\n",
		"pkg/testdata/f.go": "package f\n\nfunc F() {}\n",
		"gen.go":            "// Code generated by protoc-gen-go. DO NOT EDIT.\npackage p\n\nfunc G() {}\n",
	})
	rep, err := e.Init(InitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Files != 1 {
		t.Errorf("Files = %d, want 1(仅 a.go)", rep.Files)
	}
	for _, rel := range []string{"vendor/dep/x.go", "pkg/testdata/f.go", "gen.go"} {
		if _, err := os.Stat(e.Store.ShardPathFor(rel)); !os.IsNotExist(err) {
			t.Errorf("%s 不应有分片", rel)
		}
	}
}

// TestSymbolCaseNotCollision 仅大小写不同的符号是合法的不同 Go 符号,
// 各自建节点、无告警(M1.1 验收修正:aibridge 的 DefaultPrompts/defaultPrompts 曾被误杀)。
func TestSymbolCaseNotCollision(t *testing.T) {
	e, _ := newRepo(t, map[string]string{
		"a.go": "package p\n\nvar defaultPrompts = 1\n\nfunc DefaultPrompts() int { return defaultPrompts }\n",
	})
	rep, err := e.Init(InitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	nodes := loadNodes(t, e, "a.go")
	if _, ok := nodes["a.go#defaultPrompts"]; !ok {
		t.Error("缺 defaultPrompts 节点")
	}
	if _, ok := nodes["a.go#DefaultPrompts"]; !ok {
		t.Error("缺 DefaultPrompts 节点")
	}
	for _, w := range rep.Warnings {
		if strings.Contains(w, "符号大小写") {
			t.Errorf("不应告警:%s", w)
		}
	}
}

// TestConfigExclude config.yaml 的 exclude 覆盖生效。
func TestConfigExclude(t *testing.T) {
	e, repo := newRepo(t, map[string]string{
		"a.go":       "package p\n\nfunc A() {}\n",
		"gen/big.go": "package gen\n\nfunc B() {}\n",
	})
	if err := os.MkdirAll(filepath.Join(repo, ".knowledge"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".knowledge", "config.yaml"),
		[]byte("schema: 1\nport: 18500\nexclude:\n  - gen/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := e.Init(InitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Files != 1 {
		t.Errorf("Files = %d, want 1(gen/ 被 exclude)", rep.Files)
	}
}

// TestReanchorAll mass-suspect 的批量出口:全库重锚,suspect 升回 fresh,Entries 不动。
func TestReanchorAll(t *testing.T) {
	e, repo := newRepo(t, map[string]string{"a.go": loginSrc})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	digest(t, e, "a.go", "a.go#Login", "坑")

	// 语义级改动(非格式化)让节点降 suspect。
	changed := strings.Replace(loginSrc, "return check(user, pass)", "return check(pass, user)", 1)
	writeFiles(t, repo, map[string]string{"a.go": changed})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	if n := loadNodes(t, e, "a.go")["a.go#Login"]; n.Status != model.StatusSuspect {
		t.Fatalf("前置失败:未降 suspect")
	}

	rep, err := e.Init(InitOptions{ReanchorAll: true})
	if err != nil {
		t.Fatal(err)
	}
	n := loadNodes(t, e, "a.go")["a.go#Login"]
	if n.Status != model.StatusFresh {
		t.Errorf("reanchor 后状态 = %s, want fresh(%s)", n.Status, rep.Text())
	}
	if len(n.Entries) != 1 {
		t.Error("reanchor 不许动 Entries")
	}
	// 锚必须已更新到当前代码:再跑一次普通 init 应零 suspect。
	rep2, err := e.Init(InitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Suspected != 0 {
		t.Errorf("reanchor 后普通 init 又降 suspect,说明锚没真正更新")
	}
}

// TestMassSuspectBanner suspect 激增(>50%)触发报告置顶告警。
func TestMassSuspectBanner(t *testing.T) {
	e, repo := newRepo(t, map[string]string{"a.go": "package p\n\nfunc A() int { return 1 }\n\nfunc B() int { return 2 }\n"})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	digest(t, e, "a.go", "a.go#A", "知识A")
	digest(t, e, "a.go", "a.go#B", "知识B")

	// 两个函数体都语义变更 + 文件节点连坐 → 3/3 suspect。
	writeFiles(t, repo, map[string]string{"a.go": "package p\n\nfunc A() int { return 10 }\n\nfunc B() int { return 20 }\n"})
	rep, err := e.Init(InitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.MassSuspect {
		t.Errorf("应触发 mass-suspect 告警(Suspected=%d)", rep.Suspected)
	}
	if !strings.Contains(rep.Text(), "reanchor-all") {
		t.Errorf("告警文案应指向 --reanchor-all:%s", rep.Text())
	}
}

// TestUnknownFieldsSurviveInit 对账重写分片时,未知字段(新版本写入)保留。
func TestUnknownFieldsSurviveInit(t *testing.T) {
	e, repo := newRepo(t, map[string]string{"a.go": loginSrc})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	// 手工往分片里塞一个未来字段。
	path := e.Store.ShardPathFor("a.go")
	data, _ := os.ReadFile(path)
	patched := strings.Replace(string(data), "schema: 1", "schema: 1\nfuture_field: 未来", 1)
	if err := os.WriteFile(path, []byte(patched), 0o644); err != nil {
		t.Fatal(err)
	}
	// 代码变更迫使分片重写。
	writeFiles(t, repo, map[string]string{"a.go": loginSrc + "\nfunc Extra() {}\n"})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if !strings.Contains(string(after), "future_field: 未来") {
		t.Error("init 重写分片丢了未知字段")
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
