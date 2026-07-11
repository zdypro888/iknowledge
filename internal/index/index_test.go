package index

import (
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/zdypro888/iknowledge/internal/model"
)

func TestTokenize(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"checkLockout", []string{"checklockout", "check", "lockout"}},
		{"HTTPServer start", []string{"httpserver", "http", "server", "start"}},
		{"登录锁定", []string{"登录", "录锁", "锁定"}},
		{"锁", []string{"锁"}},
		{"user_name", []string{"user", "name"}}, // 下划线是切分点(impl §8:ASCII 按非字母数字切)
		{"登录 lock 15分钟", []string{"登录", "lock", "15", "分钟"}},
		{"", nil},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := Tokenize(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Tokenize(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestSplitIdent(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"checkLockout", []string{"check", "lockout"}},
		{"HTTPServer", []string{"http", "server"}},
		{"AuthService.SignIn", []string{"auth", "service", "sign", "in"}},
		{"max_retries", []string{"max", "retries"}},
		{"init~2", []string{"init", "2"}},
		{"_", nil},
	}
	for _, tt := range tests {
		if got := SplitIdent(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("SplitIdent(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func fixtureIndex(t *testing.T) *Index {
	t.Helper()
	since := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	shards := map[string][]model.Node{
		"tree/internal/auth/login.go.yaml": {
			{ID: "internal/auth/login.go", Level: model.LevelFile, Status: model.StatusFresh, Since: since},
			{
				ID: "internal/auth/login.go#Authenticate", Level: model.LevelFunction,
				Status: model.StatusFresh, Since: since,
				Lineage:  []string{"internal/auth/login.go#Login"},
				Keywords: []string{"登录", "锁定"},
				Entries: []model.Entry{
					{ID: "e_00000001", Kind: model.KindPitfall, Text: "不要在调用方预先加密", Confidence: model.ConfidenceInferred},
					{ID: "e_00000002", Kind: model.KindUsage, Text: "旧条目", Confidence: model.ConfidenceInferred, SupersededBy: "e_00000003"},
					{ID: "e_00000003", Kind: model.KindUsage, Text: "一律经 SignIn 调用", Confidence: model.ConfidenceInferred},
				},
			},
			{
				ID: "internal/auth/login.go#checkLockout", Level: model.LevelFunction,
				Status: model.StatusFresh, Since: since,
				Entries: []model.Entry{{
					ID: "e_00000010", Kind: model.KindContract, Text: "计数器 15 分钟滑动窗",
					Confidence: model.ConfidenceInferred,
					BasedOn:    []string{"internal/auth/login.go#Login#e_00000002"}, // 旧节点 ID + 被取代条目:双重归一
				}},
			},
		},
	}
	changes := []model.Change{
		{ID: "chg_a", Nodes: []string{"internal/auth/login.go#Login"}, At: since.Add(-time.Hour), What: "旧名下的历史", Why: "y"},
		{ID: "chg_b", Nodes: []string{"internal/auth/login.go#Authenticate"}, At: since.Add(time.Hour), What: "新名下的历史", Why: "y"},
		{ID: "chg_pre", Nodes: []string{"internal/auth/login.go#checkLockout"}, At: since.Add(-2 * time.Hour), What: "前任的历史", Why: "y"},
	}
	flows := []model.Flow{{
		ID: "flow:user-login", Title: "用户登录", Since: since,
		// 引用旧 ID:验证反向链接经 lineage 归一化。
		Steps: []model.FlowStep{{Node: "internal/auth/login.go#Login", Note: "核心验证"}},
	}}
	return Build(shards, changes, flows)
}

func TestResolveNodeID(t *testing.T) {
	ix := fixtureIndex(t)
	if got := ix.ResolveNodeID("internal/auth/login.go#Authenticate"); got != "internal/auth/login.go#Authenticate" {
		t.Errorf("现任 ID 解析失败:%q", got)
	}
	if got := ix.ResolveNodeID("internal/auth/login.go#Login"); got != "internal/auth/login.go#Authenticate" {
		t.Errorf("血缘解析失败:%q", got)
	}
	if got := ix.ResolveNodeID("no/such.go#X"); got != "" {
		t.Errorf("不存在的 ID 应返回空:%q", got)
	}
}

func TestResolveEntryRef(t *testing.T) {
	ix := fixtureIndex(t)
	// 旧节点 ID + 被取代条目 → 现任节点 + 现任条目。
	got := ix.ResolveEntryRef("internal/auth/login.go#Login#e_00000002")
	want := "internal/auth/login.go#Authenticate#e_00000003"
	if got != want {
		t.Errorf("ResolveEntryRef = %q, want %q", got, want)
	}
}

func TestResolveEntryRefFindsEntryOnNonFirstSplitHeir(t *testing.T) {
	old := "svc.go#Old"
	first := "svc.go#AFirst"
	second := "svc.go#ZSecond"
	ix := Build(map[string][]model.Node{
		"tree/svc.go.yaml": {
			{ID: first, Lineage: []string{old}},
			{ID: second, Lineage: []string{old}, Entries: []model.Entry{
				{ID: "e_target", Confidence: model.ConfidenceInferred},
			}},
		},
	}, nil, nil)
	if got, want := ix.ResolveEntryRef(old+"#e_target"), second+"#e_target"; got != want {
		t.Fatalf("拆分条目应落到实际持有它的继承者: got %q want %q", got, want)
	}
}

func TestResolveEntryRefFollowsLineageWhenOldNodeIDWasReused(t *testing.T) {
	old := "svc.go#Old"
	heir := "svc.go#Renamed"
	ix := Build(map[string][]model.Node{
		"tree/current.yaml": {{ID: old, Entries: []model.Entry{{ID: "e_new"}}}},
		"tree/heir.yaml": {{ID: heir, Lineage: []string{old}, Entries: []model.Entry{{
			ID: "e_historical", Confidence: model.ConfidenceInferred,
		}}}},
	}, nil, nil)
	if got, want := ix.ResolveEntryRef(old+"#e_historical"), heir+"#e_historical"; got != want {
		t.Fatalf("旧 ID 复用后历史 entry 应沿 lineage: got %q want %q", got, want)
	}
	if got, want := ix.ResolveEntryRef(old+"#e_new"), old+"#e_new"; got != want {
		t.Fatalf("现任 exact entry 应优先: got %q want %q", got, want)
	}
}

func TestReusedNodeIDRoutesHistoricalJournalLandmineAndFlowToHeir(t *testing.T) {
	oldID := "svc.go#Old"
	heirID := "svc.go#Renamed"
	oldAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reusedAt := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	newAt := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	ix := Build(map[string][]model.Node{
		"tree/current.yaml": {{ID: oldID, Since: reusedAt}},
		"tree/heir.yaml":    {{ID: heirID, Since: oldAt, Lineage: []string{oldID}}},
	}, []model.Change{
		{ID: "chg_old", Nodes: []string{oldID}, At: oldAt, What: "old"},
		{ID: "chg_new", Nodes: []string{oldID}, At: newAt, What: "new"},
	}, []model.Flow{
		{ID: "flow:old", Since: oldAt, Steps: []model.FlowStep{{Node: oldID}}},
		{ID: "flow:new", Since: newAt, Steps: []model.FlowStep{{Node: oldID}}},
	})
	if got := ix.History(heirID); len(got) != 1 || got[0].ID != "chg_old" {
		t.Fatalf("继承者历史错位:%+v", got)
	}
	if got := ix.History(oldID); len(got) != 1 || got[0].ID != "chg_new" {
		t.Fatalf("复用节点历史被旧代污染:%+v", got)
	}
	if got := ix.LandmineScore(heirID); got != 1 {
		t.Fatalf("旧地雷应随历史到继承者:%d", got)
	}
	if got := ix.LandmineScore(oldID); got != 1 {
		t.Fatalf("复用节点只应计新代地雷:%d", got)
	}
	if got := ix.FlowsOf(heirID); !reflect.DeepEqual(got, []string{"flow:old"}) {
		t.Fatalf("旧 flow 应跟随 lineage:%v", got)
	}
	if got := ix.FlowsOf(oldID); !reflect.DeepEqual(got, []string{"flow:new"}) {
		t.Fatalf("新 flow 应指向复用节点:%v", got)
	}
}

func TestDependentsCycleDoesNotReturnRoot(t *testing.T) {
	ix := Build(nil, nil, nil)
	ix.basedOnRev["n#a"] = []string{"n#b"}
	ix.basedOnRev["n#b"] = []string{"n#a"}
	if got := ix.Dependents("n#a"); !reflect.DeepEqual(got, []string{"n#b"}) {
		t.Fatalf("依赖环不应把根自身作为 dependent:%v", got)
	}
}

func TestDisputedByDoesNotMutatePublishedIndex(t *testing.T) {
	ix := Build(nil, nil, nil)
	ix.disputesRev["n#e"] = []string{"z#e", "a#e"}
	wantInternal := append([]string(nil), ix.disputesRev["n#e"]...)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := ix.DisputedBy("n#e")
			if !reflect.DeepEqual(got, []string{"a#e", "z#e"}) {
				t.Errorf("DisputedBy = %v", got)
			}
		}()
	}
	wg.Wait()
	if !reflect.DeepEqual(ix.disputesRev["n#e"], wantInternal) {
		t.Fatalf("读方法修改了索引内部 slice: %v", ix.disputesRev["n#e"])
	}
}

func TestBuildIsolatesDuplicateNodeIDsAcrossShards(t *testing.T) {
	ix := Build(map[string][]model.Node{
		"tree/a.yaml": {{ID: "dup.go#F", Entries: []model.Entry{{ID: "e_a"}}}},
		"tree/b.yaml": {{ID: "dup.go#F", Entries: []model.Entry{{ID: "e_b"}}}},
	}, nil, nil)
	if ix.Node("dup.go#F") != nil {
		t.Fatal("跨分片重复 node ID 不应随机选一个进入索引")
	}
	if got := ix.DuplicateNodeIDs(); !reflect.DeepEqual(got, []string{"dup.go#F"}) {
		t.Fatalf("DuplicateNodeIDs = %v", got)
	}
}

func TestDependents(t *testing.T) {
	ix := fixtureIndex(t)
	// checkLockout 的条目 basedOn 指向(归一化后的)e_00000003。
	deps := ix.Dependents("internal/auth/login.go#Authenticate#e_00000003")
	if len(deps) != 1 || deps[0] != "internal/auth/login.go#checkLockout#e_00000010" {
		t.Errorf("Dependents = %v", deps)
	}
}

func TestHistorySinceAndLineage(t *testing.T) {
	ix := fixtureIndex(t)
	// Authenticate:血缘命中 chg_a(不受 Since 限制)+ 直接命中 chg_b。
	hist := ix.History("internal/auth/login.go#Authenticate")
	if len(hist) != 2 {
		t.Fatalf("history = %d 条, want 2(血缘穿透)", len(hist))
	}
	// checkLockout:chg_pre 在 Since 之前且是同 ID 直接命中 → 过滤(防前任历史错挂)。
	hist2 := ix.History("internal/auth/login.go#checkLockout")
	if len(hist2) != 0 {
		t.Errorf("Since 过滤失败:%+v", hist2)
	}
}

func TestSearch(t *testing.T) {
	ix := fixtureIndex(t)
	tests := []struct {
		query string
		want  string // 首位命中
	}{
		{"登录锁定", "internal/auth/login.go#Authenticate"},
		{"lockout", "internal/auth/login.go#checkLockout"}, // 标识符拆词
		{"预先加密", "internal/auth/login.go#Authenticate"},    // 条目文本
		{"login", "internal/auth/login.go#Authenticate"},   // ID 分段;function 优先于 file
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			hits := ix.Search(tt.query, 10)
			if len(hits) == 0 || hits[0].NodeID != tt.want {
				t.Errorf("Search(%q) = %+v, want 首位 %s", tt.query, hits, tt.want)
			}
		})
	}
	if hits := ix.Search("完全不存在的词汇组合xyz", 10); len(hits) != 0 {
		t.Errorf("零命中应为空:%+v", hits)
	}
	// 被取代条目的文本不入索引。
	if hits := ix.Search("旧条目", 10); len(hits) != 0 {
		t.Errorf("superseded 条目文本不应入索引:%+v", hits)
	}
}

func TestFlowsReverseLink(t *testing.T) {
	ix := fixtureIndex(t)
	// flow 引用的是旧 ID(lineage 归一后挂到现任节点)。
	flows := ix.FlowsOf("internal/auth/login.go#Authenticate")
	if len(flows) != 1 || flows[0] != "flow:user-login" {
		t.Errorf("FlowsOf = %v", flows)
	}
}

// R29 批次5:trigram 近似匹配——精确 token 不命中时,词形相近的能浮出。
func TestSearchTrigramFallback(t *testing.T) {
	ix := fixtureIndex(t)
	// "authentication" 与 "Authenticate" 共享大量 trigram(auth, uth, hen, ...),
	// 精确 token 不命中(authenticate ≠ authenticate——大小写经 Tokenize 归一后相同?),
	// 但即便精确也 miss,trigram 回退让它浮出。
	hits := ix.Search("authentication", 10)
	if len(hits) == 0 {
		t.Fatal("trigram 回退应让 authentication 命中 Authenticate,却零结果")
	}
	found := false
	for _, h := range hits {
		if h.NodeID == "internal/auth/login.go#Authenticate" {
			found = true
		}
	}
	if !found {
		t.Errorf("authentication 应近似命中 Authenticate,got %+v", hits)
	}
}

// trigrams 对中文不生效(防切坏 UTF-8)。
func TestTrigramsSkipNonASCII(t *testing.T) {
	if g := trigrams("登录"); g != nil {
		t.Errorf("中文不应走 trigram,got %v", g)
	}
	if g := trigrams("abc"); len(g) != 1 || g[0] != "abc" {
		t.Errorf("abc 的 trigram 应只有 abc,got %v", g)
	}
	if g := trigrams("loginLockout"); len(g) == 0 {
		t.Error("loginLockout 应产生 trigram")
	}
}
