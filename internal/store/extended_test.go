package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zdypro888/iknowledge/internal/model"
)

// extended.go 的直测(原先只靠 engine/mcpserv 集成测试间接覆盖):
// flows/WIP/usage/dismissed/findings 的读写往返与边界。

func newStoreT(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestFlowRoundTrip(t *testing.T) {
	s := newStoreT(t)
	f := model.Flow{
		ID: "flow:login", Title: "登录流程",
		Steps: []model.FlowStep{{Node: "a/a.go#Login", Note: "入口"}},
		Since: time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC),
	}
	if err := s.SaveFlow(f); err != nil {
		t.Fatal(err)
	}
	flows, warns, err := s.LoadFlows()
	if err != nil || len(warns) != 0 {
		t.Fatalf("LoadFlows: %v %v", err, warns)
	}
	if len(flows) != 1 || flows[0].ID != "flow:login" || flows[0].Steps[0].Note != "入口" {
		t.Errorf("flow 往返不完整:%+v", flows)
	}
	// 非法 ID 返回空路径(路径穿越防线:含分隔符即拒)。
	if p := s.FlowPathFor("flow:../evil"); p != "" {
		t.Errorf("穿越型 flow ID 应返回空路径,got %q", p)
	}
	if p := s.FlowPathFor(`flow:..\evil`); p != "" {
		t.Errorf("反斜杠穿越应返回空路径,got %q", p)
	}
}

func TestWIPRoundTrip(t *testing.T) {
	s := newStoreT(t)
	w := model.WIP{Owner: "sess-1", Task: "修支付", Todo: []string{"a", "b"},
		Touching: []string{"pay/pay.go#Charge"}, Updated: time.Now().UTC()}
	if err := s.SaveWIP(w); err != nil {
		t.Fatal(err)
	}
	wips, err := s.LoadWIPs()
	if err != nil || len(wips) != 1 || wips[0].Task != "修支付" {
		t.Fatalf("WIP 往返:%v %+v", err, wips)
	}
	if err := s.ClearWIP("sess-1"); err != nil {
		t.Fatal(err)
	}
	if wips, _ = s.LoadWIPs(); len(wips) != 0 {
		t.Errorf("ClearWIP 后仍有 %d 条", len(wips))
	}
}

func TestUsageAppendLoad(t *testing.T) {
	s := newStoreT(t)
	s.AppendUsage("2026-07", UsageRecord{Tool: "kb_recall", Hit: true, OK: true})
	s.AppendUsage("2026-07", UsageRecord{Tool: "kb_remember", OK: true})
	s.AppendUsage("2026-08", UsageRecord{Tool: "kb_recall", OK: true})
	recs, err := s.LoadUsage()
	if err != nil || len(recs) != 3 {
		t.Fatalf("LoadUsage = %d 条, err %v", len(recs), err)
	}
}

func TestDismissedDebts(t *testing.T) {
	s := newStoreT(t)
	if err := s.DismissDebt("d_abc"); err != nil {
		t.Fatal(err)
	}
	if err := s.DismissDebt("d_def"); err != nil {
		t.Fatal(err)
	}
	m, err := s.LoadDismissedDebts()
	if err != nil || !m["d_abc"] || !m["d_def"] {
		t.Fatalf("dismissed 往返:%v %v", m, err)
	}
}

func TestAppendFindings(t *testing.T) {
	s := newStoreT(t)
	if err := s.AppendFindings(model.Findings{Job: "job_1", Conclusion: "在 a.go", At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	// findings 落在 local/ 下的月份 JSONL;文件存在且含结论即可(读端无 API,审计用)。
	matches, _ := filepath.Glob(filepath.Join(s.Dir(), "local", "findings-*.jsonl"))
	if len(matches) != 1 {
		t.Fatalf("findings 文件数 = %d", len(matches))
	}
	data, _ := os.ReadFile(matches[0])
	if !strings.Contains(string(data), "在 a.go") {
		t.Errorf("findings 内容缺失:%s", data)
	}
}

// atomicWrite 的错误路径(#难测分支的可测子集)。
func TestAtomicWriteErrors(t *testing.T) {
	if err := atomicWrite("", []byte("x")); err == nil {
		t.Error("空路径应拒绝(铁律二防线)")
	}
	// 目标父路径是文件而非目录:MkdirAll 必败。
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(filepath.Join(blocker, "sub", "f.yaml"), []byte("x")); err == nil {
		t.Error("父路径为文件应报错")
	}
}
