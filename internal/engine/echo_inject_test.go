package engine

import (
	"strings"
	"testing"
)

// 复述检测:条目 ASCII 词大量来自符号签名 → 警示不拒收;正常洞见不误伤。
func TestEchoWarn(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"internal/auth/login.go": authSrc})
	sid := "s-echo"

	out, err := e.Remember(RememberArgs{
		Node:    "internal/auth/login.go#Login",
		Entries: []RememberEntry{{Kind: "summary", Text: "Login takes user pass string returns error"}},
	}, sid, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "疑似签名复述") {
		t.Errorf("签名回声应警示:%s", out)
	}

	out, err = e.Remember(RememberArgs{
		Node:    "internal/auth/login.go#checkLockout",
		Entries: []RememberEntry{{Kind: "pitfall", Text: "锁定窗口依赖外部时钟,本地时间漂移会导致误锁,排障先查 NTP"}},
	}, sid, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "疑似签名复述") {
		t.Errorf("正常洞见被误伤:%s", out)
	}
}

// hook 写事件的记账提醒:Edit 附提醒,Read 不附。
func TestInjectWriteReminder(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"internal/auth/login.go": authSrc})
	sid := "s-inj"
	if _, err := e.Remember(RememberArgs{
		Node:    "internal/auth/login.go#Login",
		Entries: []RememberEntry{{Kind: "summary", Text: "登录入口,锁定检查在内部"}},
	}, sid, "claude-code"); err != nil {
		t.Fatal(err)
	}

	out, err := e.Inject("internal/auth/login.go", sid, "Edit")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "必须 kb_record_change") {
		t.Errorf("写事件应附记账提醒:\n%s", out)
	}
	out, err = e.Inject("internal/auth/login.go", sid, "Read")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "你刚修改了本文件") {
		t.Errorf("读事件不应附记账提醒:\n%s", out)
	}
}
