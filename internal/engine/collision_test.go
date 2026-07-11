package engine

import (
	"strings"
	"testing"

	"github.com/zdypro888/iknowledge/internal/model"
)

// 轮30-A 方案防撞:kb_remember 自动比对历史 rejected,命中分级提醒。
func TestRememberRejectedCollisionWarns(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"internal/auth/login.go": authSrc})

	// 先 record_change 一条带 rejected 的变更(否决了"用 sync.Map 缓存")。
	_, err := e.RecordChange(ChangeArgs{
		Nodes: []string{"internal/auth/login.go#Login"},
		What:  "登录限流改造", Why: "防止暴力破解",
		Rejected: []model.Rejected{{Option: "用 sync.Map 做登录尝试计数缓存", Reason: "尺寸不可预估,长跑内存泄漏"}},
	}, "s1", "codex")
	if err != nil {
		t.Fatal(err)
	}

	// 再 remember 一条与 rejected 方案相似的 entry(不带 disputes)→ 应强警告。
	// 文本与 rejected Option 高度相似(bigram>0.8):几乎是同一句话。
	out, err := e.Remember(RememberArgs{
		Node:    "internal/auth/login.go#Login",
		Entries: []RememberEntry{{Kind: "summary", Text: "用 sync.Map 做登录尝试计数缓存"}},
	}, "s2", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "曾被否决") {
		t.Errorf("相似方案应触发强警告,out:\n%s", out)
	}
}

func TestRememberRejectedCollisionDisputesSoftensWarning(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"internal/auth/login.go": authSrc})

	// 先记一条 rejected。
	if _, err := e.RecordChange(ChangeArgs{
		Nodes: []string{"internal/auth/login.go#Login"},
		What:  "限流", Why: "防爆破",
		Rejected: []model.Rejected{{Option: "用 sync.Map 做计数缓存", Reason: "内存泄漏风险"}},
	}, "s1", "codex"); err != nil {
		t.Fatal(err)
	}

	// 找到被否决方案对应的现有 entry(supersedes 目标——先 remember 一条作为 disputes 指向)。
	// 这里直接测:带 disputes 的相似 entry 应是温和提醒(含"已声明 disputes"),不是强警告。
	// disputes 需指向一个存在的 entry,先 remember 一条基准。
	_, err := e.Remember(RememberArgs{
		Node:    "internal/auth/login.go#Login",
		Entries: []RememberEntry{{Kind: "summary", Text: "基准条目"}},
	}, "s0", "codex")
	if err != nil {
		t.Fatal(err)
	}
	// 取基准条目 ID。
	ref := e.rt.ix.Node("internal/auth/login.go#Login")
	if ref == nil {
		t.Fatal("节点不存在")
	}
	var baseEntryID string
	for i := range ref.Node.Entries {
		if ref.Node.Entries[i].Active() {
			baseEntryID = ref.Node.Entries[i].ID
			break
		}
	}

	out, err := e.Remember(RememberArgs{
		Node: "internal/auth/login.go#Login",
		Entries: []RememberEntry{{
			Kind: "summary", Text: "用 sync.Map 做计数缓存",
			Disputes: []string{"internal/auth/login.go#Login#" + baseEntryID},
		}},
	}, "s2", "codex")
	if err != nil {
		t.Fatal(err)
	}
	// 带 disputes:应是温和提醒(含"注意"和"已声明"),不是"⚠ 此方案曾被否决"。
	if strings.Contains(out, "⚠ 此方案曾被否决") {
		t.Errorf("带 disputes 的相似方案不该强警告,out:\n%s", out)
	}
}

// 不相似的方案不该误报。
func TestRememberRejectedNoFalsePositive(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"internal/auth/login.go": authSrc})
	if _, err := e.RecordChange(ChangeArgs{
		Nodes: []string{"internal/auth/login.go#Login"},
		What:  "限流", Why: "防爆破",
		Rejected: []model.Rejected{{Option: "用 Redis 做分布式锁", Reason: "过度设计"}},
	}, "s1", "codex"); err != nil {
		t.Fatal(err)
	}
	out, err := e.Remember(RememberArgs{
		Node:    "internal/auth/login.go#Login",
		Entries: []RememberEntry{{Kind: "summary", Text: "登录函数验证用户名密码后返回 nil"}},
	}, "s2", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "否决") {
		t.Errorf("不相似方案不该触发防撞警告,out:\n%s", out)
	}
}
