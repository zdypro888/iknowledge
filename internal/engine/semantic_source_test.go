package engine

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zdypro888/iknowledge/internal/index"
	"github.com/zdypro888/iknowledge/internal/model"
)

type cancelAfterErrContext struct {
	calls int
	after int
}

func (c *cancelAfterErrContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *cancelAfterErrContext) Done() <-chan struct{}       { return nil }
func (c *cancelAfterErrContext) Value(any) any               { return nil }
func (c *cancelAfterErrContext) Err() error {
	c.calls++
	if c.calls >= c.after {
		return context.Canceled
	}
	return nil
}

func TestSemanticDocumentsBuildTypedRedactedKnowledgeCards(t *testing.T) {
	e, _ := newRepo(t, map[string]string{
		"auth.go": "package auth\n\nfunc Login() {}\n",
	})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	node := "auth.go#Login"
	if _, err := e.Remember(RememberArgs{Node: node, Entries: []RememberEntry{
		{Kind: "summary", Text: "连续登录失败会触发锁定，token=sk-abcdefghijklmnopqrstuvwxyz123456"},
		{Kind: "pitfall", Text: "不要绕过锁定检查"},
	}}, "s", "test"); err != nil {
		t.Fatal(err)
	}
	docs, manifest, err := e.semanticSourceSnapshot(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := manifest.fingerprint
	if len(docs) != 2 {
		t.Fatalf("documents=%d, want current+risk cards: %+v", len(docs), docs)
	}
	byLane := make(map[string]semanticDocument, len(docs))
	for _, doc := range docs {
		byLane[doc.Kind] = doc
		if doc.NodeID != node || !strings.HasPrefix(doc.RecordID, "card:"+doc.Kind+":"+node+":") {
			t.Fatalf("unexpected typed card: %+v", doc)
		}
	}
	current, currentOK := byLane[semanticLaneCurrent]
	risk, riskOK := byLane[semanticLaneRisk]
	if !currentOK || !riskOK || !strings.Contains(current.Text, "[summary") ||
		!strings.Contains(risk.Text, "[pitfall") {
		t.Fatalf("typed lanes missing: current=%+v risk=%+v", current, risk)
	}
	if strings.Contains(current.Text, "sk-abcdefghijklmnopqrstuvwxyz123456") ||
		!strings.Contains(current.Text, "[REDACTED:openai-key]") {
		t.Fatalf("embedding 文本未脱敏: %q", current.Text)
	}
	if fingerprint == ([32]byte{}) || current.SourceHash == ([32]byte{}) || risk.SourceHash == ([32]byte{}) {
		t.Fatal("fingerprint/source hash 不得为空")
	}
	// 这三个 golden 固定 v5 typed source 的 record ordering/card
	// formatting/redaction/hash 语义，防止后续加固悄悄漂移召回。
	if got, want := hex.EncodeToString(fingerprint[:]), "e678d91a7b5e0a66a0d498ba961908e42fcefd53946a9607f751c803774aedf6"; got != want {
		t.Fatalf("semantic source fingerprint drift: got=%s want=%s", got, want)
	}
	if got, want := hex.EncodeToString(current.SourceHash[:]), "f6ed1c59ab6254df9242e9fff35cbdbbd77bb9d01139bb08c36830c93defa289"; got != want {
		t.Fatalf("current source hash drift: got=%s want=%s", got, want)
	}
	if got, want := hex.EncodeToString(risk.SourceHash[:]), "ae92db40c8a136d6ca7c7a9e9360d7cc199a07afbf4a0cc004848b2bb7d516f1"; got != want {
		t.Fatalf("risk source hash drift: got=%s want=%s", got, want)
	}

	// 新风险必须只改变 risk 卡片；current source hash 保持稳定，使旧索引在
	// partial 模式仍可安全复用未变化的 current 证据。
	if _, err := e.Remember(RememberArgs{Node: node, Entries: []RememberEntry{{
		Kind: "pitfall", Text: "另一个排障坑",
	}}}, "s", "test"); err != nil {
		t.Fatal(err)
	}
	after, manifestAfter, err := e.semanticSourceSnapshot(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	fingerprintAfter := manifestAfter.fingerprint
	if len(after) != 2 || fingerprintAfter == fingerprint {
		t.Fatalf("风险变更未换代: docs=%d before=%x after=%x", len(after), fingerprint, fingerprintAfter)
	}
	for _, doc := range after {
		if doc.Kind == semanticLaneCurrent && doc.SourceHash != current.SourceHash {
			t.Fatalf("risk-only 变更污染 current source hash: before=%x after=%x", current.SourceHash, doc.SourceHash)
		}
		if doc.Kind == semanticLaneRisk && doc.SourceHash == risk.SourceHash {
			t.Fatalf("risk card source hash 未变化: %x", doc.SourceHash)
		}
	}
}

func TestSemanticSourceFailsClosedOnBoundedDTOInputs(t *testing.T) {
	newEngine := func(shards map[string][]model.Node, changes []model.Change, flows []model.Flow) *Engine {
		e := &Engine{}
		e.rt.ix = index.Build(shards, changes, flows)
		e.rt.flows = flows
		e.rt.semanticSourceVersion = 1
		return e
	}
	baseNode := model.Node{ID: "worker.go#Run", Level: model.LevelFunction, Status: model.StatusFresh}

	t.Run("oversized-field", func(t *testing.T) {
		node := baseNode
		node.EraSummary = strings.Repeat("x", semanticMaxSourceFieldBytes+1)
		e := newEngine(map[string][]model.Node{"tree/worker.yaml": {node}}, nil, nil)
		if _, _, err := e.semanticSourceSnapshot(context.Background(), true); err == nil || !strings.Contains(err.Error(), "单字段") {
			t.Fatalf("oversized field error=%v", err)
		}
	})

	t.Run("invalid-utf8", func(t *testing.T) {
		node := baseNode
		node.EraSummary = string([]byte{0xff})
		e := newEngine(map[string][]model.Node{"tree/worker.yaml": {node}}, nil, nil)
		if _, _, err := e.semanticSourceSnapshot(context.Background(), true); err == nil || !strings.Contains(err.Error(), "UTF-8") {
			t.Fatalf("invalid UTF-8 error=%v", err)
		}
	})

	t.Run("oversized-node-id-before-render", func(t *testing.T) {
		node := baseNode
		node.ID = strings.Repeat("n", semanticMaxSourceNodeIDBytes+1)
		e := newEngine(map[string][]model.Node{"tree/worker.yaml": {node}}, nil, nil)
		if _, _, err := e.semanticSourceSnapshot(context.Background(), true); err == nil ||
			!strings.Contains(err.Error(), "node ID") || !strings.Contains(err.Error(), "4032") {
			t.Fatalf("oversized node ID error=%v", err)
		}
	})

	t.Run("too-many-input-headers", func(t *testing.T) {
		flow := model.Flow{
			ID: "flow:huge", Steps: []model.FlowStep{{Node: baseNode.ID}},
			Conventions: make([]string, semanticMaxSourceItems),
		}
		e := newEngine(map[string][]model.Node{"tree/worker.yaml": {baseNode}}, nil, []model.Flow{flow})
		if _, _, err := e.semanticSourceSnapshot(context.Background(), true); err == nil || !strings.Contains(err.Error(), "数量超过硬上限") {
			t.Fatalf("too many inputs error=%v", err)
		}
	})

	t.Run("cartesian-expansion-preflight", func(t *testing.T) {
		const nodesN = 700
		nodes := make([]model.Node, 0, nodesN)
		refs := make([]string, 0, nodesN)
		for i := range nodesN {
			id := "worker.go#Run" + string(rune('A'+i%26)) + strings.Repeat("x", i/26)
			nodes = append(nodes, model.Node{ID: id, Level: model.LevelFunction, Status: model.StatusFresh})
			refs = append(refs, id)
		}
		change := model.Change{ID: "chg_large", Nodes: refs, What: strings.Repeat("w", 100_000), Why: "test"}
		e := newEngine(map[string][]model.Node{"tree/workers.yaml": nodes}, []model.Change{change}, nil)
		if _, _, err := e.semanticSourceSnapshot(context.Background(), true); err == nil || !strings.Contains(err.Error(), "待格式化正文") {
			t.Fatalf("cartesian preflight error=%v", err)
		}
	})
}

func TestSemanticRenderMetadataBudgetPrecedesCardAllocation(t *testing.T) {
	budget := semanticRenderBudget{ctx: context.Background()}
	nodeID := strings.Repeat("n", semanticMaxSourceNodeIDBytes)
	var err error
	for i := 0; i < semanticMaxSourceItems; i++ {
		err = budget.add(1, nodeID, semanticLaneHistory, "rejected", "change")
		if err != nil {
			break
		}
	}
	if err == nil || !strings.Contains(err.Error(), "输出元数据") {
		t.Fatalf("expanded metadata bypassed preflight: %v", err)
	}
	if budget.bodyBytes >= uint64(semanticMaxSourceBytes) {
		t.Fatalf("metadata test unexpectedly exhausted body budget first: %d", budget.bodyBytes)
	}
}

func TestSemanticSourceCancellationCoversGraphsRenderAndReservation(t *testing.T) {
	t.Run("inactive graph", func(t *testing.T) {
		changes := make([]model.Change, 10_000)
		for i := range changes {
			changes[i].ID = "change-" + strings.Repeat("x", i%31) + string(rune(i+1))
		}
		ctx := &cancelAfterErrContext{after: 12}
		if _, err := inactiveChangesContext(ctx, changes); !errors.Is(err, context.Canceled) {
			t.Fatalf("inactive graph cancellation=%v", err)
		}
	})

	t.Run("single node card render", func(t *testing.T) {
		items := make([]semanticCardItem, 20_000)
		for i := range items {
			items[i] = semanticCardItem{facet: "f", reference: string(rune(i + 1)), text: "body"}
		}
		ctx := &cancelAfterErrContext{after: 8}
		if _, err := renderSemanticCardChunksContext(ctx, "node", semanticLaneCurrent, items); !errors.Is(err, context.Canceled) {
			t.Fatalf("card render cancellation=%v", err)
		}
	})

	t.Run("source reservation", func(t *testing.T) {
		e, _ := initEngine(t, map[string]string{"worker.go": "package worker\n\nfunc Run() {}\n"})
		coordinator := NewSemanticProcessCoordinator(1024)
		if err := e.SetSemanticProcessCoordinator(coordinator); err != nil {
			t.Fatal(err)
		}
		ctx := &cancelAfterErrContext{after: 10}
		if _, _, _, err := e.semanticSourceSnapshotLease(ctx, false); !errors.Is(err, context.Canceled) {
			t.Fatalf("source cancellation=%v", err)
		}
		if got := coordinator.sourceReservedBytes(e); got != 0 {
			t.Fatalf("canceled source leaked reservation=%d", got)
		}
		e.rt.mu.RLock()
		ready := e.rt.semanticManifest.ready
		e.rt.mu.RUnlock()
		if ready {
			t.Fatal("canceled source published a manifest")
		}
	})
}

func TestSemanticSourceShapePreflightAllocatesNothing(t *testing.T) {
	e := &Engine{}
	e.rt.ix = index.Build(map[string][]model.Node{"tree/worker.yaml": {{
		ID: "worker.go#Run", Status: model.StatusFresh, Entries: []model.Entry{{
			ID: "e_current", Kind: model.KindContract, Text: "bounded", Confidence: model.ConfidenceInferred,
		}},
	}}}, nil, nil)
	if allocs := testing.AllocsPerRun(100, func() {
		if err := e.semanticSourceShapePreflightLockedContext(context.Background()); err != nil {
			panic(err)
		}
	}); allocs != 0 {
		t.Fatalf("shape preflight allocated %.2f objects/run", allocs)
	}
}

func TestSemanticSourcePreflightBoundsLineageBeforeConstruction(t *testing.T) {
	const old = "legacy.go#Run"
	nodes := make([]model.Node, semanticMaxSourceResolutionCandidates+1)
	for i := range nodes {
		nodes[i] = model.Node{ID: fmt.Sprintf("worker_%04d.go#Run", i), Lineage: []string{old}}
	}

	t.Run("change", func(t *testing.T) {
		changes := []model.Change{{ID: "chg_fanout", Nodes: []string{old}, What: "split", Why: "test"}}
		e := &Engine{}
		e.rt.ix = index.Build(map[string][]model.Node{"tree/workers.yaml": nodes}, changes, nil)
		_, err := e.semanticSourceInputLockedContext(context.Background())
		if err == nil || !strings.Contains(err.Error(), "change lineage preflight") || !strings.Contains(err.Error(), "4096") {
			t.Fatalf("unbounded change lineage was not rejected in preflight: %v", err)
		}
	})

	t.Run("dispute", func(t *testing.T) {
		withSource := append([]model.Node(nil), nodes...)
		withSource = append(withSource, model.Node{
			ID: "caller.go#Call", Status: model.StatusFresh,
			Entries: []model.Entry{{
				ID: "e_source", Kind: model.KindContract, Text: "source", Confidence: model.ConfidenceInferred,
				Disputes: []string{old + "#e_target"},
			}},
		})
		e := &Engine{}
		e.rt.ix = index.Build(map[string][]model.Node{"tree/workers.yaml": withSource}, nil, nil)
		_, err := e.semanticSourceInputLockedContext(context.Background())
		if err == nil || !strings.Contains(err.Error(), "dispute ref preflight") || !strings.Contains(err.Error(), "4096") {
			t.Fatalf("unbounded dispute lineage was not rejected in preflight: %v", err)
		}
	})
}

func TestSemanticSourceCannotRebuildAfterShutdown(t *testing.T) {
	e := &Engine{}
	e.rt.ix = index.Build(map[string][]model.Node{"tree/worker.yaml": {{ID: "worker.go#Run"}}}, nil, nil)
	coordinator := NewSemanticProcessCoordinator(1024)
	if err := e.SetSemanticProcessCoordinator(coordinator); err != nil {
		t.Fatal(err)
	}
	e.ReleaseSemanticProcessResources()
	if _, _, _, err := e.semanticSourceSnapshotLease(context.Background(), false); err == nil || !strings.Contains(err.Error(), "关闭") {
		t.Fatalf("post-shutdown source rebuild was not rejected: %v", err)
	}
	if got := coordinator.sourceReservedBytes(e); got != 0 {
		t.Fatalf("post-shutdown source leaked reservation=%d", got)
	}
}

func TestSemanticSourceUsesTemporalNodeGenerationForChangeAndFlow(t *testing.T) {
	oldAt := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	reusedAt := oldAt.Add(24 * time.Hour)
	oldID := "legacy.go#Run"
	heirID := "worker.go#Run"
	shards := map[string][]model.Node{
		"tree/reused.yaml": {{ID: oldID, Level: model.LevelFunction, Status: model.StatusFresh, Since: reusedAt}},
		"tree/heir.yaml":   {{ID: heirID, Level: model.LevelFunction, Status: model.StatusFresh, Since: oldAt, Lineage: []string{oldID}}},
	}
	changes := []model.Change{
		{ID: "chg_old", At: oldAt, Nodes: []string{oldID}, What: "旧代决策", Why: "旧实现"},
		{ID: "chg_new", At: reusedAt, Nodes: []string{oldID}, What: "新代决策", Why: "新实现"},
	}
	flows := []model.Flow{
		{ID: "flow:old", Title: "旧代流程", Since: oldAt, Steps: []model.FlowStep{{Node: oldID}}},
		{ID: "flow:step-old", Title: "步骤时代优先", Since: reusedAt, Steps: []model.FlowStep{{Node: oldID, Since: oldAt}}},
		{ID: "flow:new", Title: "新代流程", Since: reusedAt, Steps: []model.FlowStep{{Node: oldID}}},
	}
	e := &Engine{}
	e.rt.ix = index.Build(shards, changes, flows)
	e.rt.flows = flows
	e.rt.semanticSourceVersion = 1
	docs, _, err := e.semanticSourceSnapshot(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	texts := map[string]map[string]string{}
	for _, doc := range docs {
		if texts[doc.NodeID] == nil {
			texts[doc.NodeID] = map[string]string{}
		}
		texts[doc.NodeID][doc.Kind] += doc.Text
	}
	if got := texts[heirID][semanticLaneHistory]; !strings.Contains(got, "旧代决策") || strings.Contains(got, "新代决策") {
		t.Fatalf("lineage heir history generation mismatch: %q", got)
	}
	if got := texts[oldID][semanticLaneHistory]; !strings.Contains(got, "新代决策") || strings.Contains(got, "旧代决策") {
		t.Fatalf("reused exact history generation mismatch: %q", got)
	}
	if got := texts[heirID][semanticLaneCurrent]; !strings.Contains(got, "旧代流程") || !strings.Contains(got, "步骤时代优先") || strings.Contains(got, "新代流程") {
		t.Fatalf("lineage heir flow generation mismatch: %q", got)
	}
	if got := texts[oldID][semanticLaneCurrent]; !strings.Contains(got, "新代流程") || strings.Contains(got, "旧代流程") || strings.Contains(got, "步骤时代优先") {
		t.Fatalf("reused exact flow generation mismatch: %q", got)
	}
}

func TestSemanticSourceFansHistoricalChangeAndFlowToAllSplitHeirs(t *testing.T) {
	at := time.Date(2025, 2, 3, 4, 5, 6, 0, time.UTC)
	oldID := "legacy.go#Run"
	heirs := []string{"worker_a.go#Run", "worker_b.go#Run"}
	shards := map[string][]model.Node{
		"tree/a.yaml": {{ID: heirs[0], Level: model.LevelFunction, Status: model.StatusFresh, Since: at, Lineage: []string{oldID}}},
		"tree/b.yaml": {{ID: heirs[1], Level: model.LevelFunction, Status: model.StatusFresh, Since: at, Lineage: []string{oldID}}},
	}
	changes := []model.Change{{ID: "chg_split", At: at, Nodes: []string{oldID}, What: "拆分前决策", Why: "两边都继承"}}
	flows := []model.Flow{{ID: "flow:split", Title: "拆分前流程", Since: at, Steps: []model.FlowStep{{Node: oldID}}}}
	e := &Engine{}
	e.rt.ix = index.Build(shards, changes, flows)
	e.rt.flows = flows
	e.rt.semanticSourceVersion = 1
	docs, _, err := e.semanticSourceSnapshot(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	texts := map[string]string{}
	for _, doc := range docs {
		texts[doc.NodeID] += doc.Text
	}
	for _, heir := range heirs {
		if !strings.Contains(texts[heir], "拆分前决策") || !strings.Contains(texts[heir], "拆分前流程") {
			t.Fatalf("split heir %s missing inherited evidence: %q", heir, texts[heir])
		}
	}
}

func TestSemanticFlowsFingerprintIncludesGenerationTimes(t *testing.T) {
	at := time.Date(2025, 3, 4, 5, 6, 7, 123, time.FixedZone("offset", 8*60*60))
	base := []model.Flow{{ID: "flow:generation", Since: at, Steps: []model.FlowStep{{Node: "worker.go#Run"}}}}
	flowChanged := append([]model.Flow(nil), base...)
	flowChanged[0].Since = at.Add(time.Nanosecond)
	if semanticFlowsFingerprint(base) == semanticFlowsFingerprint(flowChanged) {
		t.Fatal("flow.Since change must invalidate semantic source generation")
	}
	stepChanged := append([]model.Flow(nil), base...)
	stepChanged[0].Steps = append([]model.FlowStep(nil), base[0].Steps...)
	stepChanged[0].Steps[0].Since = at
	if semanticFlowsFingerprint(base) == semanticFlowsFingerprint(stepChanged) {
		t.Fatal("step.Since change must invalidate semantic source generation")
	}
	sameInstant := append([]model.Flow(nil), base...)
	sameInstant[0].Since = at.UTC()
	if semanticFlowsFingerprint(base) != semanticFlowsFingerprint(sameInstant) {
		t.Fatal("fingerprint must canonicalize equal instants to UTC")
	}
}

func TestSemanticDocumentRedactsFinalNodePrefix(t *testing.T) {
	secret := "sk-abcdefghijklmnopqrstuvwxyz123456"
	doc := makeSemanticDocument("summary:x#e", secret+".go#Load", "summary", "安全摘要")
	if strings.Contains(doc.Text, secret) || !strings.Contains(doc.Text, "[REDACTED:openai-key]") {
		t.Fatalf("最终 embedding 文本的 node ID 未脱敏: %q", doc.Text)
	}
	if len(doc.Text) > semanticMaxDocumentBytes {
		t.Fatalf("embedding 文本=%d bytes, 超过 %d", len(doc.Text), semanticMaxDocumentBytes)
	}
}

func TestCompactSemanticTextKeepsHeadAndTail(t *testing.T) {
	input := "HEAD-" + strings.Repeat("中", 100) + "-TAIL"
	got := compactSemanticText(input, 40)
	if len([]rune(got)) > 40 || !strings.HasPrefix(got, "HEAD-") || !strings.HasSuffix(got, "-TAIL") {
		t.Fatalf("compact=%q runes=%d", got, len([]rune(got)))
	}
}

func TestSemanticTypedCardsTrackFlowsAndDecisionState(t *testing.T) {
	e, _ := newRepo(t, map[string]string{
		"worker.go": "package worker\n\nfunc Run() {}\n",
	})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	node := "worker.go#Run"
	if _, err := e.Flow(FlowArgs{Action: "create", Flow: model.Flow{
		ID: "flow:jobs", Title: "异步任务执行", Conventions: []string{"先持久化再投递"},
		Troubleshoot: "重复消费时核对幂等键", Steps: []model.FlowStep{{Node: node, Note: "执行任务"}},
	}}, "semantic-source", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RecordChange(ChangeArgs{
		Nodes: []string{node}, What: "采用持久队列", Why: "进程重启不能丢任务",
		Rejected: []model.Rejected{{Option: "仅使用内存队列", Reason: "重启会丢任务"}},
	}, "semantic-source", "test"); err != nil {
		t.Fatal(err)
	}

	docs, _, err := e.semanticSourceSnapshot(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	byLane := map[string]semanticDocument{}
	for _, doc := range docs {
		byLane[doc.Kind] = doc
	}
	if !strings.Contains(byLane[semanticLaneCurrent].Text, "先持久化再投递") ||
		!strings.Contains(byLane[semanticLaneRisk].Text, "仅使用内存队列") ||
		!strings.Contains(byLane[semanticLaneRisk].Text, "重复消费") {
		t.Fatalf("flow/rejected cards missing: %+v", byLane)
	}
	if !containsString(byLane[semanticLaneCurrent].Facets, "flow") ||
		!containsString(byLane[semanticLaneRisk].Facets, "rejected") ||
		!containsString(byLane[semanticLaneRisk].References, "flow:jobs") {
		t.Fatalf("facets/references missing: current=%+v risk=%+v", byLane[semanticLaneCurrent], byLane[semanticLaneRisk])
	}

	e.rt.mu.RLock()
	changes := e.rt.ix.Changes()
	e.rt.mu.RUnlock()
	if len(changes) == 0 {
		t.Fatal("record change missing")
	}
	previous := changes[len(changes)-1].ID
	if _, err := e.RecordChange(ChangeArgs{
		Nodes: []string{node}, What: "改用磁盘队列", Why: "恢复时间目标收紧",
		Overturns: previous, Rebuttal: "新实现已满足磁盘吞吐要求",
	}, "semantic-source", "test"); err != nil {
		t.Fatal(err)
	}
	docs, _, err = e.semanticSourceSnapshot(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	byLane = map[string]semanticDocument{}
	for _, doc := range docs {
		byLane[doc.Kind] = doc
	}
	if strings.Contains(byLane[semanticLaneRisk].Text, "仅使用内存队列") ||
		!strings.Contains(byLane[semanticLaneHistory].Text, "仅使用内存队列") ||
		!strings.Contains(byLane[semanticLaneHistory].Text, "改用磁盘队列") {
		t.Fatalf("overturned decision did not move to history: risk=%q history=%q",
			byLane[semanticLaneRisk].Text, byLane[semanticLaneHistory].Text)
	}
}

func TestSemanticOpenDisputeIsSymmetricAndLeavesRiskAfterResolution(t *testing.T) {
	e, _ := newRepo(t, map[string]string{"auth.go": "package auth\n\nfunc Login() {}\n"})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	node := "auth.go#Login"
	out, err := e.Remember(RememberArgs{Node: node, Entries: []RememberEntry{{
		Kind: "contract", Text: "调用方传入明文密码",
	}}}, "semantic-dispute", "test")
	if err != nil {
		t.Fatal(err)
	}
	firstID := strings.TrimSpace(strings.TrimPrefix(strings.SplitN(out, "\n", 2)[0], "entryIds:"))
	if _, err := e.Remember(RememberArgs{Node: node, Entries: []RememberEntry{{
		Kind: "contract", Text: "调用方传入已哈希密码", Disputes: []string{node + "#" + firstID},
	}}}, "semantic-dispute", "test"); err != nil {
		t.Fatal(err)
	}
	docs, _, err := e.semanticSourceSnapshot(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	var riskText string
	for _, doc := range docs {
		if doc.Kind == semanticLaneRisk {
			riskText += doc.Text
		}
	}
	if !strings.Contains(riskText, "调用方传入明文密码") ||
		!strings.Contains(riskText, "调用方传入已哈希密码") ||
		strings.Count(riskText, "[open-dispute]") != 2 {
		t.Fatalf("both active dispute sides must be risk evidence: %q", riskText)
	}
	if _, err := e.Verify(VerifyArgs{
		Entry: node + "#" + firstID, Verdict: "refute",
		Evidence: "auth.go 第 3 行 Login 直接使用调用方提供的哈希值",
	}, "semantic-dispute", "test"); err != nil {
		t.Fatal(err)
	}
	docs, _, err = e.semanticSourceSnapshot(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	var currentText string
	riskText = ""
	for _, doc := range docs {
		switch doc.Kind {
		case semanticLaneCurrent:
			currentText += doc.Text
		case semanticLaneRisk:
			riskText += doc.Text
		}
	}
	if !strings.Contains(currentText, "调用方传入已哈希密码") || strings.Contains(riskText, "调用方传入已哈希密码") {
		t.Fatalf("resolved surviving contract did not return to current: current=%q risk=%q", currentText, riskText)
	}
}

func TestInactiveChangesHandlesArbitraryRevertChains(t *testing.T) {
	tests := []struct {
		name     string
		changes  []model.Change
		inactive []string
	}{
		{name: "overturn", changes: []model.Change{{ID: "a"}, {ID: "b", Overturns: "a"}}, inactive: []string{"a"}},
		{name: "revert-overturn-restores-target", changes: []model.Change{
			{ID: "a"}, {ID: "b", Overturns: "a"}, {ID: "r", Reverts: "b"},
		}, inactive: []string{"b"}},
		{name: "revert-of-revert-reapplies-overturn", changes: []model.Change{
			{ID: "a"}, {ID: "b", Overturns: "a"}, {ID: "r", Reverts: "b"}, {ID: "rr", Reverts: "r"},
		}, inactive: []string{"a", "r"}},
		{name: "direct-revert-parity", changes: []model.Change{
			{ID: "a"}, {ID: "r", Reverts: "a"}, {ID: "rr", Reverts: "r"}, {ID: "rrr", Reverts: "rr"},
		}, inactive: []string{"a", "rr"}},
		{name: "overturn-of-overturn-keeps-effects", changes: []model.Change{
			{ID: "a"}, {ID: "b", Overturns: "a"}, {ID: "c", Overturns: "b"},
		}, inactive: []string{"a", "b"}},
		{name: "revert-latest-overturn-restores-b-not-a", changes: []model.Change{
			{ID: "a"}, {ID: "b", Overturns: "a"}, {ID: "c", Overturns: "b"}, {ID: "r", Reverts: "c"},
		}, inactive: []string{"a", "c"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inactive, err := inactiveChanges(test.changes)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]bool{}
			for _, id := range test.inactive {
				want[id] = true
			}
			for _, change := range test.changes {
				if inactive[change.ID] != want[change.ID] {
					t.Fatalf("inactive=%v, want=%v", inactive, want)
				}
			}
		})
	}
}

func TestInactiveChangesRejectsAmbiguousGraphs(t *testing.T) {
	for name, changes := range map[string][]model.Change{
		"duplicate":        {{ID: "a"}, {ID: "a"}},
		"missing-revert":   {{ID: "r", Reverts: "missing"}},
		"missing-overturn": {{ID: "o", Overturns: "missing"}},
		"inactive-missing-overturn": {
			{ID: "o", Overturns: "missing"}, {ID: "r", Reverts: "o"},
		},
		"cycle":           {{ID: "a", Reverts: "b"}, {ID: "b", Reverts: "a"}},
		"self-overturn":   {{ID: "a", Overturns: "a"}},
		"future-overturn": {{ID: "a", Overturns: "b"}, {ID: "b"}},
		"future-revert":   {{ID: "a", Reverts: "b"}, {ID: "b"}},
		"mixed-cycle-same-time": {
			{ID: "a", At: time.Unix(1, 0), Reverts: "b"},
			{ID: "b", At: time.Unix(1, 0), Overturns: "a"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if inactive, err := inactiveChanges(changes); err == nil {
				t.Fatalf("ambiguous graph accepted: %v", inactive)
			}
		})
	}
}

func TestCompactSemanticTextBytesPreservesReasonTailAndUTF8(t *testing.T) {
	input := "选项: " + strings.Repeat("前缀内容", 80) + "\n原因: tail-reason-必须保留"
	got := compactSemanticTextBytes(input, 160)
	if len(got) > 160 || !utf8.ValidString(got) {
		t.Fatalf("bounded UTF-8 compaction failed: bytes=%d valid=%v", len(got), utf8.ValidString(got))
	}
	if !strings.Contains(got, "选项") || !strings.Contains(got, "tail-reason-必须保留") ||
		!strings.Contains(got, "保留首尾") {
		t.Fatalf("head/tail rationale lost: %q", got)
	}
}

func TestSemanticDecisionCardsCompactFieldsWithoutErasingRationale(t *testing.T) {
	at := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	change := model.Change{
		ID:    "chg_structured_compaction",
		Nodes: []string{"worker.go#Run"},
		At:    at.Add(2 * time.Second),
		What:  "what-head " + strings.Repeat("W", 4000) + " what-tail",
		Why:   "why-head " + strings.Repeat("Y", 4000) + " why-tail",
		Task:  "task-head " + strings.Repeat("T", 2000) + " task-tail",
		Verified: "verified-head " + strings.Repeat("V", 2000) +
			" verified-tail",
		Overturns: "chg_base",
		Rebuttal:  "rebuttal-head " + strings.Repeat("B", 3000) + " rebuttal-tail",
		Reverts:   "chg_reverted",
		Rejected: []model.Rejected{{
			Option: "option-head " + strings.Repeat("O", 5000) + " option-tail",
			Reason: "reason-head " + strings.Repeat("R", 5000) + " reason-tail",
		}},
	}
	raw, err := buildSemanticRawDocuments(semanticSourceInput{changes: []model.Change{
		{ID: "chg_base", Nodes: []string{"worker.go#Run"}, At: at, What: "base", Why: "base reason"},
		{ID: "chg_reverted", Nodes: []string{"worker.go#Run"}, At: at.Add(time.Second), What: "reverted", Why: "old reason"},
		change,
	}})
	if err != nil {
		t.Fatal(err)
	}
	var risk, history string
	for _, doc := range raw {
		if len(doc.Raw) > semanticCardRawTarget {
			t.Fatalf("raw card exceeded chunk target: %d", len(doc.Raw))
		}
		switch doc.Kind {
		case semanticLaneRisk:
			risk += doc.Raw
		case semanticLaneHistory:
			history += doc.Raw
		}
	}
	for _, want := range []string{
		"改动: what-head", "what-tail", "原因: why-head", "why-tail", "任务: task-head", "task-tail",
		"推翻: chg_base", "反驳: rebuttal-head", "rebuttal-tail", "撤销: chg_reverted",
		"验证: verified-head", "verified-tail",
	} {
		if !strings.Contains(history, want) {
			t.Fatalf("structured decision field %q lost:\n%s", want, history)
		}
	}
	for _, want := range []string{"否决方案: option-head", "option-tail", "原因: reason-head", "reason-tail"} {
		if !strings.Contains(risk, want) {
			t.Fatalf("structured rejected field %q lost:\n%s", want, risk)
		}
	}
}

func TestSemanticStaleFlowsAreRiskNotCurrent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  model.Status
		pending bool
		state   string
	}{
		{name: "orphaned", status: model.StatusOrphaned, state: "orphaned"},
		{name: "suspect", status: model.StatusSuspect, state: "suspect"},
		{name: "pending-anchor", status: model.StatusFresh, pending: true, state: "pending-anchor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const nodeID = "old.go#Removed"
			flows := []model.Flow{{ID: "deploy", Title: "部署流程", Steps: []model.FlowStep{{
				Node: nodeID, Note: "调用待复核入口",
			}}}}
			e := &Engine{}
			e.rt.ix = index.Build(map[string][]model.Node{"tree/old.yaml": {{
				ID: nodeID, Level: model.LevelFunction, Status: tc.status, PendingAnchor: tc.pending,
			}}}, nil, flows)
			e.rt.flows = flows
			input, err := e.semanticSourceInputLocked()
			if err != nil {
				t.Fatal(err)
			}
			raw, err := buildSemanticRawDocuments(input)
			if err != nil {
				t.Fatal(err)
			}
			var current, risk string
			for _, doc := range raw {
				switch doc.Kind {
				case semanticLaneCurrent:
					current += doc.Raw
				case semanticLaneRisk:
					risk += doc.Raw
				}
			}
			if strings.Contains(current, "部署流程") || !strings.Contains(risk, "部署流程") ||
				!strings.Contains(risk, "stale-flow state="+tc.state) {
				t.Fatalf("%s flow lane mismatch: current=%q risk=%q", tc.state, current, risk)
			}
		})
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
