package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zdypro888/iknowledge/internal/model"
)

func TestSemanticDecisionAdvisoryPrioritizesTouchingBeforeTruncation(t *testing.T) {
	const query = "priority-query"
	scores := make([]float64, 25)
	for i := range len(scores) - 1 {
		scores[i] = 0.99 - float64(i)*0.015
	}
	scores[len(scores)-1] = 0.20 // deliberately outside configured Top-K=20
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Model != "priority-model" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		data := make([]map[string]any, len(request.Input))
		for inputIndex, text := range request.Input {
			vector := []float64{0, 1}
			switch {
			case text == query:
				vector = []float64{1, 0}
			case text == semanticProbeText:
				vector = []float64{0, 1}
			default:
				for marker, score := range scores {
					if strings.Contains(text, fmt.Sprintf("priority-risk-%d", marker)) {
						vector = []float64{score, math.Sqrt(1 - score*score)}
						break
					}
				}
			}
			data[inputIndex] = map[string]any{"index": inputIndex, "embedding": vector}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(provider.Close)

	files := map[string]string{}
	for i := range scores {
		files[fmt.Sprintf("n%d.go", i)] = fmt.Sprintf("package priority\n\nfunc N%d() {}\n", i)
	}
	e, _ := initEngine(t, files)
	for i := range scores {
		node := fmt.Sprintf("n%d.go#N%d", i, i)
		if _, err := e.Remember(RememberArgs{Node: node, Entries: []RememberEntry{{
			Kind: "pitfall", Text: fmt.Sprintf("priority-risk-%d", i),
		}}}, "advisory-priority", "test"); err != nil {
			t.Fatal(err)
		}
	}
	cfg := DefaultSemanticSettings()
	cfg.Enabled, cfg.Endpoint, cfg.Model, cfg.Dimensions, cfg.TimeoutSec =
		true, provider.URL, "priority-model", 2, 2
	cfg.MinScore = 0.1
	if err := SaveSemanticSettings(e.Store, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RebuildSemantic(context.Background()); err != nil {
		t.Fatal(err)
	}

	out, err := e.TaskContext(context.Background(), TaskArgs{Action: "start", WIP: model.WIP{
		Task: query, Touching: []string{"n24.go#N24"},
	}}, "advisory-priority", "test")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[风险·touching精确] n24.go#N24") {
		t.Fatalf("lower-similarity touching risk was truncated before priority ordering:\n%s", out)
	}
}

func TestSemanticDecisionAdvisoryKeepsExactRiskWithoutEmbeddingModel(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"worker.go": "package worker\n\nfunc Run() {}\n"})
	const marker = "deterministic-advisory-marker"
	if _, err := e.Remember(RememberArgs{Node: "worker.go#Run", Entries: []RememberEntry{{
		Kind: model.KindPitfall, Text: marker + " 必须复用幂等键",
	}}}, "advisory-lexical", "test"); err != nil {
		t.Fatal(err)
	}
	out, err := e.TaskContext(context.Background(), TaskArgs{Action: "start", WIP: model.WIP{
		Task: marker, Touching: []string{"worker.go#Run"},
	}}, "advisory-lexical", "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"历史决策提醒", "keyword-risk", "worker.go#Run", "仅供参考，不阻断"} {
		if !strings.Contains(out, want) {
			t.Fatalf("model-free advisory missing %q:\n%s", want, out)
		}
	}
}

func TestSemanticDecisionAdvisoryChecksEverySplitHeirWithoutQueryMatch(t *testing.T) {
	files := make(map[string]string)
	var nodeIDs []string
	for i := range 24 { // exceeds both the old global cap and the exact-detail budget
		file, symbol := fmt.Sprintf("heir_%d.go", i), fmt.Sprintf("Run%d", i)
		files[file] = fmt.Sprintf("package worker\n\nfunc %s() {}\n", symbol)
		nodeIDs = append(nodeIDs, file+"#"+symbol)
	}
	e, _ := initEngine(t, files)
	for _, nodeID := range nodeIDs {
		if _, err := e.Remember(RememberArgs{Node: nodeID, Entries: []RememberEntry{{
			Kind: model.KindPitfall, Text: "risk text intentionally unrelated to task wording " + nodeID,
		}}}, "advisory-split", "test"); err != nil {
			t.Fatal(err)
		}
		file, _ := model.SplitNodeID(nodeID)
		path := e.Store.ShardPathFor(file)
		shard, raw, err := e.Store.LoadShard(path)
		if err != nil {
			t.Fatal(err)
		}
		for i := range shard.Nodes {
			if shard.Nodes[i].ID == nodeID {
				shard.Nodes[i].Lineage = append(shard.Nodes[i].Lineage, "legacy.go#Run")
			}
		}
		if err := e.Store.SaveShard(path, shard, raw); err != nil {
			t.Fatal(err)
		}
	}
	out, err := e.TaskContext(context.Background(), TaskArgs{Action: "start", WIP: model.WIP{
		Task: "rename output variable", Touching: []string{"legacy.go#Run"},
	}}, "advisory-split", "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, heir := range nodeIDs {
		if !strings.Contains(out, heir) {
			t.Fatalf("split heir %s was neither expanded nor named in the omission handoff:\n%s", heir, out)
		}
	}
	if got := strings.Count(out, "[风险·touching精确]"); got != semanticAdvisoryMaxExactWarnings {
		t.Fatalf("expanded exact warnings=%d, want %d:\n%s", got, semanticAdvisoryMaxExactWarnings, out)
	}
	if !strings.Contains(out, "被省略的当前节点 ID:") || !strings.Contains(out, "完整 ID 逐一 kb_recall") {
		t.Fatalf("exact omission did not provide actionable current heir IDs:\n%s", out)
	}
}

func TestSemanticDecisionAdvisoryChecksRejectedAndFlowWithoutEmbedding(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"worker.go": "package worker\n\nfunc Run() {}\n"})
	const nodeID = "worker.go#Run"
	if _, err := e.RecordChange(ChangeArgs{
		Nodes: []string{nodeID}, What: "use durable queue", Why: "survive restarts",
		Rejected: []model.Rejected{{Option: "ephemeral queue", Reason: "loses work on restart"}},
	}, "advisory-no-model", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Flow(FlowArgs{Action: "create", Flow: model.Flow{
		ID: "flow:worker", Title: "worker path", Steps: []model.FlowStep{{Node: nodeID}},
		Troubleshoot: "drain the durable queue before retrying",
	}}, "advisory-no-model", "test"); err != nil {
		t.Fatal(err)
	}
	out, err := e.TaskContext(context.Background(), TaskArgs{Action: "start", WIP: model.WIP{
		Task: "rename a local variable", Touching: []string{nodeID},
	}}, "advisory-no-model", "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[风险·touching精确] " + nodeID,
		"rejected_active", "flow_troubleshoot", "refs=", "flow:worker",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("model-free typed truth warning missing %q:\n%s", want, out)
		}
	}
}

func TestSemanticDecisionAdvisoryWarnsWithoutBlockingTaskStart(t *testing.T) {
	provider := newSemanticHTTPTestProvider(t)
	e, _ := initEngine(t, map[string]string{
		"worker.go": "package worker\n\nfunc Run() {}\n",
	})
	node := "worker.go#Run"
	if _, err := e.Remember(RememberArgs{Node: node, Entries: []RememberEntry{{
		Kind: "pitfall", Text: semanticTargetMarker + " 重试前必须复用幂等键",
	}}}, "advisory", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RecordChange(ChangeArgs{
		Nodes: []string{node}, What: "采用事务发件箱", Why: "保证投递一致性",
		Rejected: []model.Rejected{{Option: semanticTargetMarker + " 仅内存重试", Reason: "重启后丢失"}},
	}, "advisory", "test"); err != nil {
		t.Fatal(err)
	}
	e.rt.mu.RLock()
	changes := e.rt.ix.Changes()
	e.rt.mu.RUnlock()
	if len(changes) == 0 {
		t.Fatal("missing decision history")
	}
	if _, err := e.RecordChange(ChangeArgs{
		Nodes: []string{node}, What: "推翻旧投递边界", Why: "基础设施已升级",
		Overturns: changes[len(changes)-1].ID, Rebuttal: "新队列已提供持久化保证",
	}, "advisory", "test"); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultSemanticSettings()
	cfg.Enabled, cfg.Endpoint, cfg.Model, cfg.Dimensions, cfg.TimeoutSec =
		true, provider.server.URL, "integration-embed", 3, 2
	if err := SaveSemanticSettings(e.Store, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RebuildSemantic(context.Background()); err != nil {
		t.Fatal(err)
	}

	requestsBefore := provider.requests.Load()
	out, err := e.TaskContext(context.Background(), TaskArgs{Action: "start", WIP: model.WIP{
		Task: semanticOnlyTestQuery, Touching: []string{node},
	}}, "advisory-session", "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"历史决策提醒", "仅供参考，不阻断", "相似不等于裁决",
		"[风险·touching精确]", "[历史·touching精确]", "refs=",
		"历史卡片不得当成当前结论",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("advisory output missing %q:\n%s", want, out)
		}
	}
	if got := provider.requests.Load(); got != requestsBefore+1 {
		t.Fatalf("task advisory provider requests=%d, want %d", got, requestsBefore+1)
	}
	got, err := e.Task(TaskArgs{Action: "get"}, "advisory-session", "test")
	if err != nil || !strings.Contains(got, semanticOnlyTestQuery) {
		t.Fatalf("warning blocked task persistence: output=%q err=%v", got, err)
	}

	// Provider failure is not the same as “checked and found no risk”. It must
	// remain advisory and persist the task, while making the incomplete check
	// visible instead of silently implying safety.
	provider.fail.Store(true)
	requestsBefore = provider.requests.Load()
	failedQuery := semanticOnlyTestQuery + "-provider-failure"
	out, err = e.TaskContext(context.Background(), TaskArgs{Action: "start", WIP: model.WIP{
		Task: failedQuery, Touching: []string{node},
	}}, "advisory-provider-failure", "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"历史决策提醒状态", "本次提醒可能不完整", "任务不会因此阻断"} {
		if !strings.Contains(out, want) {
			t.Fatalf("provider-failure output missing %q:\n%s", want, out)
		}
	}
	if got := provider.requests.Load(); got != requestsBefore+1 {
		t.Fatalf("provider-failure requests=%d, want %d", got, requestsBefore+1)
	}
	got, err = e.Task(TaskArgs{Action: "get"}, "advisory-provider-failure", "test")
	if err != nil || !strings.Contains(got, failedQuery) {
		t.Fatalf("provider failure blocked task persistence: output=%q err=%v", got, err)
	}
	provider.fail.Store(false)
	e.semantic.mu.Lock()
	e.semantic.failureUntil = time.Time{}
	e.semantic.lastError = ""
	e.semantic.mu.Unlock()

	// Invalid input is rejected before any optional provider call, even if its
	// plan contains a semantically searchable phrase.
	requestsBefore = provider.requests.Load()
	_, err = e.TaskContext(context.Background(), TaskArgs{Action: "start", WIP: model.WIP{
		Plan: []string{semanticOnlyTestQuery},
	}}, "invalid-advisory", "test")
	if err == nil || provider.requests.Load() != requestsBefore {
		t.Fatalf("invalid start err=%v requests before=%d after=%d", err, requestsBefore, provider.requests.Load())
	}
}
