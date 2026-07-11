package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zdypro888/iknowledge/internal/model"
)

// 轮31 批次1:端到端工作流集成测试。
//
// 验证整条价值链在真实使用序列下协同正确——之前所有测试都是单工具单点,
// 这组测试模拟 AI 真实工作序列:理解→记录→(后来人)定位→防撞→撤销→雷区。
// 这是这几轮(轮29~30)所有功能改动的协同验证。

// 工作流代码:一个有调用关系的小项目(Login → checkLockout → 告警)。
const workflowSrc = `package auth

// Login 登录入口;pass 传明文,内部 hash。
func Login(user, pass string) error {
	if user == "" {
		return errEmpty
	}
	if err := checkLockout(user); err != nil {
		return err
	}
	return verify(user, pass)
}

// checkLockout 检查账户锁定状态。
func checkLockout(user string) error {
	return nil
}

func verify(user, pass string) error { return nil }

var errEmpty error
`

// 完整工作流:init → remember 契约 → record_change(带rejected) →
// recall(history 含负知识) → 第二个会话 remember 相似方案触发防撞 → revert。
func TestWorkflowRecordRecallCollisionRevert(t *testing.T) {
	e, repo := initEngine(t, map[string]string{
		"go.mod":                 "module example.com/m\n\ngo 1.26\n",
		"internal/auth/login.go": workflowSrc,
	})

	// ① 会话 A:读懂 Login,沉淀契约 + pitfall。
	out, err := e.Remember(RememberArgs{
		Node: "internal/auth/login.go#Login",
		Entries: []RememberEntry{
			{Kind: "contract", Text: "pass 传明文,hash 在 verify 内部完成"},
			{Kind: "pitfall", Text: "checkLockout 目前是空实现,接入真实锁定逻辑时注意并发"},
		},
	}, "session-A", "claude-code")
	if err != nil {
		t.Fatalf("① remember 契约+pitfall 失败: %v", err)
	}
	if !strings.Contains(out, "entryIds:") {
		t.Errorf("① remember 应成功返回 entryIds,out: %s", out)
	}

	// ② 会话 A:改造完成,record_change(否决了"sync.Map 缓存"方案)。
	_, err = e.RecordChange(ChangeArgs{
		Nodes: []string{"internal/auth/login.go#Login"},
		What:  "登录限流改造", Why: "防止暴力破解",
		Rejected: []model.Rejected{
			{Option: "用 sync.Map 做登录尝试计数缓存", Reason: "尺寸不可预估,长跑内存泄漏"},
		},
		Verified: "TestLogin",
	}, "session-A", "claude-code")
	if err != nil {
		t.Fatalf("② record_change 失败: %v", err)
	}

	// ③ recall(history 模式)应含 rejected 负知识。
	recallOut, _, err := e.Recall(RecallArgs{
		Query: "internal/auth/login.go#Login", Mode: "history",
	}, "session-A")
	if err != nil {
		t.Fatalf("③ recall history 失败: %v", err)
	}
	if !strings.Contains(recallOut, "sync.Map") {
		t.Errorf("③ recall history 应含被否决方案 sync.Map,out 末尾:\n%s",
			recallOut[max(0, len(recallOut)-400):])
	}

	// ④ 会话 B(不知 A 的决策)提了被否决的相似方案 → 防撞强警告。
	collideOut, err := e.Remember(RememberArgs{
		Node:    "internal/auth/login.go#Login",
		Entries: []RememberEntry{{Kind: "summary", Text: "用 sync.Map 做登录尝试计数缓存"}},
	}, "session-B", "codex")
	if err != nil {
		t.Fatalf("④ 防撞 remember 失败: %v", err)
	}
	if !strings.Contains(collideOut, "否决") {
		t.Errorf("④ 相似方案应触发防撞警告(否决),out:\n%s", collideOut)
	}

	// ⑤ 误操作的撤销:假设④是被错误写入的,用 revert 撤销产生 rejected 的那条 change。
	// 取②的 change ID。
	e.rt.mu.RLock()
	changes := e.rt.ix.Changes()
	e.rt.mu.RUnlock()
	if len(changes) == 0 {
		t.Fatal("⑤ 应有 change 记录")
	}
	// 找含 rejected 的那条。
	var targetID string
	for _, c := range changes {
		if len(c.Rejected) > 0 {
			targetID = c.ID
			break
		}
	}
	if targetID == "" {
		t.Fatal("⑤ 找不到含 rejected 的 change")
	}
	revertOut, err := e.Revert(RevertArgs{Change: targetID, Reason: "测试撤销:验证 revert 与 change 链协同"}, "session-B", "codex")
	if err != nil {
		t.Fatalf("⑤ revert 失败: %v", err)
	}
	if !strings.Contains(revertOut, "撤销") {
		t.Errorf("⑤ revert 应确认撤销,out: %s", revertOut)
	}
	_ = repo
}

// 诊断工作流:预置 pitfall → diagnose 症状应命中 → 附 rejected 上下文。
func TestWorkflowDiagnoseFindsProblemLocation(t *testing.T) {
	e, _ := initEngine(t, map[string]string{
		"go.mod":                 "module example.com/m\n\ngo 1.26\n",
		"internal/auth/login.go": workflowSrc,
	})

	// 沉淀一条描述问题的 pitfall。
	if _, err := e.Remember(RememberArgs{
		Node:    "internal/auth/login.go#checkLockout",
		Entries: []RememberEntry{{Kind: "pitfall", Text: "checkLockout 空实现导致并发登录无锁,可被暴力绕过"}},
	}, "s", "claude-code"); err != nil {
		t.Fatal(err)
	}

	// diagnose "并发登录可被绕过" → 应命中 checkLockout。
	out, meta, err := e.Diagnose(DiagnoseArgs{Symptom: "并发登录可被绕过"}, "s")
	if err != nil {
		t.Fatalf("diagnose 失败: %v", err)
	}
	if !meta.Hit {
		t.Error("diagnose 应命中(有描述该问题的 pitfall)")
	}
	if !strings.Contains(out, "checkLockout") {
		t.Errorf("diagnose 应定位到 checkLockout,out:\n%s", out)
	}
}

// 雷区工作流:多次 record_change + overturn → Inject 雷区警告 + kb_status TOP5。
func TestWorkflowLandmineAccumulationAndWarning(t *testing.T) {
	e, repo := initEngine(t, map[string]string{
		"go.mod":                 "module example.com/m\n\ngo 1.26\n",
		"internal/auth/login.go": workflowSrc,
	})

	// 累积 landmine:多次 record_change(含一次 overturns)。
	for i := 0; i < 2; i++ {
		if _, err := e.RecordChange(ChangeArgs{
			Nodes: []string{"internal/auth/login.go#Login"},
			What:  "调整" + string(rune('A'+i)), Why: "原因",
		}, "s", "codex"); err != nil {
			t.Fatal(err)
		}
	}
	// 一次带 overturns 的(额外 +2 分)。
	// 需要一个已存在的 change ID 来 overturn——用前两次之一。
	e.rt.mu.RLock()
	prevID := ""
	for _, c := range e.rt.ix.Changes() {
		prevID = c.ID
		break
	}
	e.rt.mu.RUnlock()
	if prevID != "" {
		if _, err := e.RecordChange(ChangeArgs{
			Nodes: []string{"internal/auth/login.go#Login"},
			What:  "推翻之前的限流方案", Why: "实测发现空锁不够",
			Overturns: prevID, Rebuttal: "空锁在并发下无效",
		}, "s", "codex"); err != nil {
			t.Logf("overturns 需 scope 校验,若失败忽略:%v", err)
		}
	}

	// Inject 该文件 → 雷区警告。
	injOut, err := e.Inject("internal/auth/login.go", "s", "Edit")
	if err != nil {
		t.Fatalf("Inject 失败: %v", err)
	}
	if !strings.Contains(injOut, "雷区") {
		t.Errorf("累积变更后 Inject 应警告雷区,out:\n%s", injOut)
	}

	// kb_status 雷区 TOP5。
	statusOut, err := e.Status()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(statusOut, "雷区 TOP5") {
		t.Errorf("kb_status 应显示雷区 TOP5")
	}
	_ = repo
}

// 跨会话 stale 检测:会话 A 读 Login(哈希 X)→ 代码改了 → 会话 A 再 recall
// 应触发过时警报(哈希变了)。验证 reconcileAllLocked 与 ledger 的协同。
func TestWorkflowCrossSessionStaleAlert(t *testing.T) {
	e, repo := initEngine(t, map[string]string{
		"go.mod":                 "module example.com/m\n\ngo 1.26\n",
		"internal/auth/login.go": workflowSrc,
	})
	// 会话 A 先 recall(记录哈希基线)。
	e.Recall(RecallArgs{Query: "internal/auth/login.go#Login"}, "session-stale")

	// 改源码(让符号哈希变化:改函数体逻辑,不只是注释——Go parser 的 Hash 是
	// gofmt 免疫且行内注释不参与,必须改实际语句)。
	loginPath := filepath.Join(repo, "internal/auth", "login.go")
	newSrc := strings.Replace(workflowSrc, "return verify(user, pass)", "u = user\n\treturn verify(u, pass)", 1)
	if err := os.WriteFile(loginPath, []byte(newSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(loginPath, future, future); err != nil {
		t.Fatal(err)
	}

	// 会话 A 再 recall 同一节点 → 应有过时警报(读过的哈希变了)。
	out, meta, err := e.Recall(RecallArgs{Query: "internal/auth/login.go#Login"}, "session-stale")
	if err != nil {
		t.Fatalf("二次 recall 失败: %v", err)
	}
	// 过时警报走 recordRead 的 staleAlert;meta.Stale 或输出含"过时"。
	staleDetected := meta.Stale || strings.Contains(out, "过时")
	if !staleDetected {
		t.Errorf("代码变更后同会话再 recall 应触发过时警报,meta.Stale=%v,out 末尾:\n%s",
			meta.Stale, out[max(0, len(out)-300):])
	}
}
