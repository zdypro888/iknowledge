package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zdypro888/iknowledge/internal/model"
	"gopkg.in/yaml.v3"
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

func TestFlowPathRejectsCrossPlatformUnsafeNames(t *testing.T) {
	s := newStoreT(t)
	for _, id := range []string{"flow:a:b", "flow:CON", "flow:con.txt", "topic:LPT9", "flow:tail.", "flow:tail ",
		"flow:bad\x00name", "flow:bad\nname", "flow:a*", `flow:a?`, `flow:a|b`, "flow:e\u0301"} {
		if got := s.FlowPathFor(id); got != "" {
			t.Errorf("跨平台不安全 ID %q 不应映射路径:%s", id, got)
		}
	}
	if got := s.FlowPathFor("flow:登录-v2"); got == "" {
		t.Fatal("普通 Unicode flow 名应允许")
	}
}

func TestLoadFlowsIsolatesIDFilenameMismatch(t *testing.T) {
	s := newStoreT(t)
	wrong := "schema: 1\nflow:\n  id: flow:b\n  title: shadow\n"
	right := "schema: 1\nflow:\n  id: flow:b\n  title: canonical\n"
	if err := s.atomicWrite(filepath.Join(s.Dir(), "flows", "a.yaml"), []byte(wrong)); err != nil {
		t.Fatal(err)
	}
	if err := s.atomicWrite(filepath.Join(s.Dir(), "flows", "b.yaml"), []byte(right)); err != nil {
		t.Fatal(err)
	}
	flows, warns, err := s.LoadFlows()
	if err != nil {
		t.Fatal(err)
	}
	if len(flows) != 1 || flows[0].ID != "flow:b" || flows[0].Title != "canonical" {
		t.Fatalf("影子 flow 未隔离:%+v", flows)
	}
	if len(warns) == 0 || !strings.Contains(strings.Join(warns, "\n"), "期望 flow:a") {
		t.Fatalf("缺 ID/文件错位告警:%v", warns)
	}
}

func TestFlowStepUnknownFieldPreserved(t *testing.T) {
	s := newStoreT(t)
	path := s.FlowPathFor("flow:future")
	fixture := `schema: 1
flow:
  id: flow:future
  title: 未来流程
  steps:
    - node: a.go#A
      note: 入口
      future_step: 必须保留
  since: 2026-07-04T00:00:00Z
`
	if err := s.atomicWrite(path, []byte(fixture)); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveFlow(model.Flow{ID: "flow:future", Title: "已更新",
		Steps: []model.FlowStep{{Node: "a.go#A", Note: "新入口"}}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "future_step: 必须保留") {
		t.Fatalf("FlowStep 未知字段丢失:\n%s", data)
	}
}

func TestFlowStepUnknownFieldsPreserveOccurrenceAndPosition(t *testing.T) {
	s := newStoreT(t)
	path := s.FlowPathFor("flow:repeat")
	fixture := `schema: 1
flow:
  id: flow:repeat
  title: 重复节点
  steps:
    - node: a.go#A
      note: 第一次
      future_step: first
    - node: a.go#A
      note: 第二次
      future_step: second
`
	if err := s.atomicWrite(path, []byte(fixture)); err != nil {
		t.Fatal(err)
	}
	// 第一项同时改 node，须按位置保留 first；第二项仍同 node，须取第二个
	// occurrence，不能让两个步骤都吸收旧序列最后一项。
	if err := s.SaveFlow(model.Flow{ID: "flow:repeat", Title: "更新", Steps: []model.FlowStep{
		{Node: "b.go#B", Note: "第一次改目标"},
		{Node: "a.go#A", Note: "第二次"},
	}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw yaml.Node
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	doc, _ := yamlDocumentMapForTest(&raw)
	steps := yamlMapValueForTest(yamlMapValueForTest(doc, "flow"), "steps")
	if steps == nil || len(steps.Content) != 2 {
		t.Fatalf("steps 异常:\n%s", data)
	}
	if got := mapScalar(steps.Content[0], "future_step"); got != "first" {
		t.Fatalf("改 node 后位置字段丢失/错配: first=%q\n%s", got, data)
	}
	if got := mapScalar(steps.Content[1], "future_step"); got != "second" {
		t.Fatalf("重复 node occurrence 错配: second=%q\n%s", got, data)
	}
}

func yamlDocumentMapForTest(root *yaml.Node) (*yaml.Node, error) {
	if root.Kind == yaml.DocumentNode && len(root.Content) == 1 {
		return root.Content[0], nil
	}
	return root, nil
}

func yamlMapValueForTest(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func TestFlowSchemaTooNewIsReadOnly(t *testing.T) {
	s := newStoreT(t)
	path := s.FlowPathFor("flow:newer")
	fixture := `schema: 99
flow:
  id: flow:newer
  title: 新版流程
  since: 2026-07-04T00:00:00Z
`
	if err := s.atomicWrite(path, []byte(fixture)); err != nil {
		t.Fatal(err)
	}
	flows, warns, err := s.LoadFlows()
	if err != nil {
		t.Fatal(err)
	}
	if len(flows) != 0 || len(warns) != 1 {
		t.Fatalf("高 schema 应按文件隔离: flows=%v warns=%v", flows, warns)
	}
	err = s.SaveFlow(model.Flow{ID: "flow:newer", Title: "旧版覆盖"})
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("高 schema SaveFlow 应拒绝, got %v", err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "schema: 99") {
		t.Fatalf("高 schema 文件被改写:\n%s", data)
	}
}

func TestFlowOverdeepUnknownFieldFailsClosedWithoutPanic(t *testing.T) {
	s := newStoreT(t)
	path := s.FlowPathFor("flow:deep")
	var nested strings.Builder
	nested.WriteString("  future_deep:\n")
	for i := 0; i < maxMergeDepth+2; i++ {
		nested.WriteString(strings.Repeat("  ", i+2) + "x:\n")
	}
	nested.WriteString(strings.Repeat("  ", maxMergeDepth+4) + "leaf: value\n")
	fixture := "schema: 1\nflow:\n  id: flow:deep\n  title: deep\n" + nested.String()
	if err := s.atomicWrite(path, []byte(fixture)); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveFlow(model.Flow{ID: "flow:deep", Title: "updated"}); err == nil {
		t.Fatal("过深 flow 未知字段必须只读拒写")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != fixture {
		t.Fatal("拒写时原 flow 不应改变")
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

func TestLoadUsageContextHonorsCancellationWithoutLogs(t *testing.T) {
	s := newStoreT(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	recs, err := s.LoadUsageContext(ctx)
	if !errors.Is(err, context.Canceled) || recs != nil {
		t.Fatalf("LoadUsageContext canceled = %#v, %v; want nil, context.Canceled", recs, err)
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
	s := newStoreT(t)
	if err := s.atomicWrite("", []byte("x")); err == nil {
		t.Error("空路径应拒绝(铁律二防线)")
	}
	// 目标父路径是文件而非目录:MkdirAll 必败。
	blocker := filepath.Join(s.Dir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.atomicWrite(filepath.Join(blocker, "sub", "f.yaml"), []byte("x")); err == nil {
		t.Error("父路径为文件应报错")
	}
}
