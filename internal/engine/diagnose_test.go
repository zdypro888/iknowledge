package engine

import (
	"strings"
	"testing"

	"github.com/zdypro888/iknowledge/internal/model"
)

// 轮30-B:kb_diagnose 症状→位置反向定位。
func TestDiagnoseHitsPitfall(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"internal/auth/login.go": authSrc})
	// 预置一条 pitfall:描述"回调超时"问题。
	if _, err := e.Remember(RememberArgs{
		Node:    "internal/auth/login.go#Login",
		Entries: []RememberEntry{{Kind: "pitfall", Text: "登录回调偶发超时来自网关重试风暴堆积"}},
	}, "s", "codex"); err != nil {
		t.Fatal(err)
	}

	out, meta, err := e.Diagnose(DiagnoseArgs{Symptom: "回调超时"}, "s")
	if err != nil {
		t.Fatal(err)
	}
	if !meta.Hit {
		t.Errorf("应命中(have pitfall 描述回调超时),meta.Hit=false")
	}
	if !strings.Contains(out, "login.go#Login") {
		t.Errorf("应定位到 login.go#Login,out:\n%s", out)
	}
	if !strings.Contains(out, "pitfall") {
		t.Errorf("应展示 pitfall 文本,out:\n%s", out)
	}
}

func TestDiagnoseIncludesRejectedContext(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"internal/auth/login.go": authSrc})
	// record_change 带 rejected(曾否决的方案)。
	if _, err := e.RecordChange(ChangeArgs{
		Nodes:   []string{"internal/auth/login.go#Login"},
		What:    "限流改造", Why: "防爆破",
		Rejected: []model.Rejected{{Option: "用内存队列缓冲重试", Reason: "重启丢消息"}},
	}, "s", "codex"); err != nil {
		t.Fatal(err)
	}
	// 再记一条 pitfall 让 diagnose 命中该节点。
	if _, err := e.Remember(RememberArgs{
		Node:    "internal/auth/login.go#Login",
		Entries: []RememberEntry{{Kind: "pitfall", Text: "登录限流相关超时"}},
	}, "s", "codex"); err != nil {
		t.Fatal(err)
	}

	out, _, err := e.Diagnose(DiagnoseArgs{Symptom: "登录超时"}, "s")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "曾否决") {
		t.Errorf("diagnose 命中节点应附历史否决方案,out:\n%s", out)
	}
}

func TestDiagnoseNoHitReturnsGuidance(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"internal/auth/login.go": authSrc})
	out, meta, err := e.Diagnose(DiagnoseArgs{Symptom: "完全不相关的量子计算问题"}, "s")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Hit {
		t.Error("无匹配时 meta.Hit 应为 false")
	}
	if !strings.Contains(out, "kb_investigate") {
		t.Errorf("无命中应提示用 investigate 深挖,out:\n%s", out)
	}
}
