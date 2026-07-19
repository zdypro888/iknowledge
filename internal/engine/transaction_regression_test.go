package engine

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/zdypro888/iknowledge/internal/model"
)

func TestRememberNewSymbolValidationHasNoAnchorSideEffect(t *testing.T) {
	e, repo := initEngine(t, map[string]string{"a.go": "package a\n\nfunc Existing() {}\n"})
	writeFiles(t, repo, map[string]string{"a.go": "package a\n\nfunc Existing() {}\nfunc NewSymbol() {}\n"})
	if _, err := e.Remember(RememberArgs{Node: "a.go#NewSymbol", Entries: []RememberEntry{{
		Kind: "not-a-kind", Text: "这条输入必须被拒绝",
	}}}, "s", "codex"); err == nil {
		t.Fatal("非法 entry 应拒绝")
	}
	if ref := e.rt.ix.Node("a.go#NewSymbol"); ref != nil {
		t.Fatalf("校验失败不应提前增量落锚:%+v", ref.Node)
	}
	sh, _, err := e.Store.LoadShard(e.Store.ShardPathFor("a.go"))
	if err != nil {
		t.Fatal(err)
	}
	if shardHasNode(sh, "a.go#NewSymbol") {
		t.Fatal("校验失败把新符号写进了分片")
	}
	if _, err := e.Remember(RememberArgs{Node: "a.go#NewSymbol", Entries: []RememberEntry{{
		Kind: model.KindContract, Text: "NewSymbol 返回前保持调用方约束",
	}}}, "s", "codex"); err != nil {
		t.Fatalf("失败请求不应妨碍随后合法落锚:%v", err)
	}
}

func TestAdoptBuryRecordsReversibleNodeEffect(t *testing.T) {
	e, repo := initEngine(t, map[string]string{"a.go": "package a\n\nfunc Old() {}\n"})
	if _, err := e.Remember(RememberArgs{Node: "a.go#Old", Entries: []RememberEntry{{
		Kind: model.KindContract, Text: "Old 保留历史兼容约束",
	}}}, "s", "codex"); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, repo, map[string]string{"a.go": "package a\n"})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := e.Sync(); err != nil {
		t.Fatal(err)
	}
	if ref := e.rt.ix.Node("a.go#Old"); ref == nil || ref.Node.Status != model.StatusOrphaned {
		if ref == nil {
			t.Fatal("前置孤儿节点消失")
		}
		t.Fatalf("前置孤儿状态异常:%s", ref.Node.Status)
	}
	out, err := e.Adopt(AdoptArgs{Orphan: "a.go#Old", Action: "bury", Reason: "功能确认删除"}, "s", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if e.rt.ix.Node("a.go#Old") != nil {
		t.Fatal("bury 后节点仍存在")
	}
	parts := strings.Split(out, "记录 ")
	if len(parts) != 2 {
		t.Fatalf("无法从输出取 bury change ID:%s", out)
	}
	changeID := strings.TrimSuffix(parts[1], ",知识快照已入 journal 可溯)")
	change := e.rt.ix.ChangeByID(changeID)
	if change == nil || len(change.NodeEffects) != 1 || change.NodeEffects[0].Before == nil || change.NodeEffects[0].After != nil {
		t.Fatalf("bury 未记录可逆 NodeEffect:%+v", change)
	}
	if _, err := e.Revert(RevertArgs{Change: changeID, Reason: "删除判断有误"}, "s", "codex"); err != nil {
		t.Fatal(err)
	}
	ref := e.rt.ix.Node("a.go#Old")
	if ref == nil || len(ref.Node.Entries) != 1 || ref.Node.Status != model.StatusOrphaned {
		t.Fatalf("revert 未恢复被送葬节点:%+v", ref)
	}
}

func TestRecordChangeRemapValidatedBeforeWritesAndReversible(t *testing.T) {
	e, repo := initEngine(t, map[string]string{"a.go": `package a

func Login() {}
`})
	if _, err := e.Remember(RememberArgs{Node: "a.go#Login", Entries: []RememberEntry{{
		Kind: "contract", Text: "Login 是旧入口",
	}}}, "s", "codex"); err != nil {
		t.Fatal(err)
	}
	// 模拟未来版本写入的未知 entry 字段；remap 与 revert 都必须带着它走。
	e.rt.mu.Lock()
	loginRef := e.rt.ix.Node("a.go#Login")
	raw := cloneYAMLNode(e.rt.cache.Shards()[loginRef.ShardRel].Raw)
	rawEntry := yamlMappingValue(yamlObjectByID(yamlMappingValue(raw, "nodes"), "a.go#Login"), "entries").Content[0]
	rawEntry.Content = append(rawEntry.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "future_entry"},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "keep-me"})
	rawData, err := yaml.Marshal(raw)
	if err != nil {
		e.rt.mu.Unlock()
		t.Fatal(err)
	}
	if err := e.Store.WriteKnowledgeFile(loginRef.ShardRel, rawData); err != nil {
		e.rt.mu.Unlock()
		t.Fatal(err)
	}
	if err := e.reloadLocked(); err != nil {
		e.rt.mu.Unlock()
		t.Fatal(err)
	}
	e.rt.mu.Unlock()
	writeFiles(t, repo, map[string]string{"a.go": `package a

func Login() {}
func NewLogin() {}
`})
	beforeChanges := len(e.rt.ix.Changes())
	_, err = e.RecordChange(ChangeArgs{
		Nodes: []string{"a.go#NewLogin"}, What: "拆入口", Why: "测试",
		Remaps: []model.Remap{{From: "a.go#Login", To: []string{"a.go#NewLogin"},
			Entries: map[string]string{"e_missing": "a.go#NewLogin"}}},
	}, "s", "codex")
	if err == nil {
		t.Fatal("非法 remaps.entries 应在任何写入前拒绝")
	}
	if got := len(e.rt.ix.Changes()); got != beforeChanges {
		t.Fatalf("非法 remap 追加了 journal: before=%d after=%d", beforeChanges, got)
	}
	if e.rt.ix.Node("a.go#NewLogin") != nil {
		t.Fatal("非法 remap 提前创建了目标节点")
	}
	if ref := e.rt.ix.Node("a.go#Login"); ref == nil || len(ref.Node.Entries) != 1 {
		t.Fatalf("非法 remap 改坏源节点:%+v", ref)
	}

	out, err := e.RecordChange(ChangeArgs{
		Nodes: []string{"a.go#NewLogin", "a.go#NewLogin"}, What: "拆入口", Why: "测试",
		Remaps: []model.Remap{{From: "a.go#Login", To: []string{"a.go#NewLogin"}}},
	}, "s", "codex")
	if err != nil {
		t.Fatal(err)
	}
	changeID := strings.TrimSpace(strings.Split(strings.SplitAfter(out, "changeId: ")[1], "\n")[0])
	if e.rt.ix.Node("a.go#Login") != nil {
		t.Fatal("合法 remap 后源节点应移除")
	}
	if ref := e.rt.ix.Node("a.go#NewLogin"); ref == nil || len(ref.Node.Entries) != 1 {
		t.Fatalf("同次新节点未接收 remap entries:%+v", ref)
	}
	remappedRaw, err := e.Store.ReadKnowledgeFile("tree/a.go.yaml")
	if err != nil || !strings.Contains(string(remappedRaw), "future_entry: keep-me") {
		t.Fatalf("remap 丢失未知 entry 字段:err=%v\n%s", err, remappedRaw)
	}
	change := e.rt.ix.ChangeByID(changeID)
	if change == nil || len(change.NodeEffects) == 0 {
		t.Fatalf("record_change 未记录可逆 NodeEffects:%+v", change)
	}
	seenNodes := map[string]int{}
	for _, id := range change.Nodes {
		seenNodes[id]++
	}
	if seenNodes["a.go#Login"] != 1 || seenNodes["a.go#NewLogin"] != 1 {
		t.Fatalf("remap 参与节点未去重纳入 change.Nodes/history:%v", change.Nodes)
	}
	if _, err := e.Revert(RevertArgs{Change: changeID, Reason: "拆分申报错误"}, "s", "codex"); err != nil {
		t.Fatal(err)
	}
	if ref := e.rt.ix.Node("a.go#Login"); ref == nil || len(ref.Node.Entries) != 1 || ref.Node.Entries[0].Confidence != model.ConfidenceInferred {
		t.Fatalf("revert 未恢复 remap 源节点及原置信度:%+v", ref)
	}
	if e.rt.ix.Node("a.go#NewLogin") != nil {
		t.Fatal("revert 未撤销本次增量创建的目标节点")
	}
	revertedRaw, err := e.Store.ReadKnowledgeFile("tree/a.go.yaml")
	if err != nil || !strings.Contains(string(revertedRaw), "future_entry: keep-me") {
		t.Fatalf("revert 丢失未知 entry 字段:err=%v\n%s", err, revertedRaw)
	}
}

func TestRevertVerifyRestoresExactStatesAndCascade(t *testing.T) {
	e, _ := initEngine(t, map[string]string{
		"a/a.go": "package a\n\nfunc F() {}\n",
		"b/b.go": "package b\n\nfunc G() {}\n",
	})
	if _, err := e.Remember(RememberArgs{Node: "a/a.go#F", Entries: []RememberEntry{{Kind: "contract", Text: "F 保证事务原子"}}}, "s", "codex"); err != nil {
		t.Fatal(err)
	}
	root := e.rt.ix.Node("a/a.go#F").Node.Entries[0]
	rootRef := "a/a.go#F#" + root.ID
	if _, err := e.Remember(RememberArgs{Node: "b/b.go#G", Entries: []RememberEntry{{
		Kind: "contract", Text: "G 依赖 F 的原子保证", BasedOn: []string{rootRef},
	}}}, "s", "codex"); err != nil {
		t.Fatal(err)
	}
	dep := e.rt.ix.Node("b/b.go#G").Node.Entries[0]
	depRef := "b/b.go#G#" + dep.ID
	if _, err := e.Verify(VerifyArgs{Entry: depRef, Verdict: "confirm", Evidence: "go test -run TestAtomic 通过"}, "s", "codex"); err != nil {
		t.Fatal(err)
	}

	if _, err := e.Verify(VerifyArgs{Entry: rootRef, Verdict: "refute", Evidence: "a/a.go 原文没有事务边界"}, "s", "codex"); err != nil {
		t.Fatal(err)
	}
	refuteID := latestChangeID(t, e, "勘误:条目 "+root.ID)
	if got := e.rt.ix.Node("b/b.go#G").Node.Entries[0].Confidence; got != model.ConfidenceSuspect {
		t.Fatalf("前置:级联未降 suspect:%s", got)
	}
	if _, err := e.Revert(RevertArgs{Change: refuteID, Reason: "勘误证据看错文件"}, "s", "codex"); err != nil {
		t.Fatal(err)
	}
	rootAfter := e.rt.ix.Node("a/a.go#F").Node.Entries[0]
	depAfter := e.rt.ix.Node("b/b.go#G").Node.Entries[0]
	if rootAfter.Confidence != model.ConfidenceInferred || rootAfter.RefutedBy != "" {
		t.Errorf("refute revert 未恢复 root before:%+v", rootAfter)
	}
	if depAfter.Confidence != model.ConfidenceVerified {
		t.Errorf("refute revert 未恢复级联前 verified:%+v", depAfter)
	}

	if _, err := e.Verify(VerifyArgs{Entry: rootRef, Verdict: "confirm", Evidence: "go test -run TestF 通过"}, "s", "codex"); err != nil {
		t.Fatal(err)
	}
	confirmID := latestChangeID(t, e, "确认:条目 "+root.ID)
	if _, err := e.Revert(RevertArgs{Change: confirmID, Reason: "验证用例无效"}, "s", "codex"); err != nil {
		t.Fatal(err)
	}
	if got := e.rt.ix.Node("a/a.go#F").Node.Entries[0].Confidence; got != model.ConfidenceInferred {
		t.Errorf("confirm revert=%s want inferred", got)
	}

	if _, err := e.Verify(VerifyArgs{Entry: rootRef, Verdict: "obsolete", Reason: "功能已下线"}, "s", "codex"); err != nil {
		t.Fatal(err)
	}
	obsoleteID := latestChangeID(t, e, "退休:条目 "+root.ID)
	if _, err := e.Revert(RevertArgs{Change: obsoleteID, Reason: "功能重新上线"}, "s", "codex"); err != nil {
		t.Fatal(err)
	}
	if got := e.rt.ix.Node("a/a.go#F").Node.Entries[0].RetiredBy; got != "" {
		t.Errorf("obsolete revert 未清 RetiredBy:%s", got)
	}
}

func TestRevertRejectsUnprovableLegacyHalfApplication(t *testing.T) {
	e, _, rootRef, depRef := setupVerifyFailureRepo(t)
	e.rt.mu.Lock()
	root := e.entryByExactRefLocked(rootRef)
	dep := e.entryByExactRefLocked(depRef)
	targetID := model.NewChangeID(time.Now().Add(-time.Minute))
	root.Confidence = model.ConfidenceRefuted
	root.RefutedBy = "" // 旧 revert 只清了 marker，却漏恢复 confidence
	dep.Confidence = model.ConfidenceSuspect
	rootNode := e.rt.ix.Node("a/a.go#F")
	depNode := e.rt.ix.Node("b/b.go#G")
	if err := e.saveNodeShardLocked(rootNode); err != nil {
		e.rt.mu.Unlock()
		t.Fatal(err)
	}
	if err := e.saveNodeShardLocked(depNode); err != nil {
		e.rt.mu.Unlock()
		t.Fatal(err)
	}
	target := model.Change{
		ID: targetID, Nodes: []string{"a/a.go#F"}, At: time.Now().Add(-time.Minute),
		What: "勘误:条目 " + root.ID + " 被驳倒(legacy)", Why: "legacy evidence",
	}
	oldRevert := model.Change{
		ID: model.NewChangeID(time.Now()), Nodes: target.Nodes, At: time.Now(),
		What: "撤销 " + targetID, Why: "旧实现", Reverts: targetID,
	}
	if err := e.Store.AppendChange(target); err != nil {
		e.rt.mu.Unlock()
		t.Fatal(err)
	}
	if err := e.Store.AppendChange(oldRevert); err != nil {
		e.rt.mu.Unlock()
		t.Fatal(err)
	}
	if err := e.reloadLocked(); err != nil {
		e.rt.mu.Unlock()
		t.Fatal(err)
	}
	e.rt.mu.Unlock()

	_, err := e.Revert(RevertArgs{Change: targetID, Reason: "补完旧半应用"}, "s", "codex")
	if err == nil || !strings.Contains(err.Error(), "REVERT_UNPROVABLE") {
		t.Fatalf("无法证明旧 confidence 时必须 fail closed:%v", err)
	}
	root = e.entryByExactRefLocked(rootRef)
	dep = e.entryByExactRefLocked(depRef)
	if root.Confidence != model.ConfidenceRefuted || root.RefutedBy != "" || dep.Confidence != model.ConfidenceSuspect {
		t.Fatalf("fail closed 不应猜测改盘:root=%+v dep=%+v", root, dep)
	}
}

func TestRevertOfRevertRequiresExplicitLatestAuditStep(t *testing.T) {
	e, _, rootRef, _ := setupVerifyFailureRepo(t)
	if _, err := e.Verify(VerifyArgs{Entry: rootRef, Verdict: "confirm", Evidence: "精确测试通过"}, "s", "codex"); err != nil {
		t.Fatal(err)
	}
	confirmID := latestChangeID(t, e, "确认:条目 ")
	if _, err := e.Revert(RevertArgs{Change: confirmID, Reason: "测试证据无效"}, "s", "codex"); err != nil {
		t.Fatal(err)
	}
	firstRevert := latestChangeID(t, e, "撤销 "+confirmID)
	if _, err := e.Revert(RevertArgs{Change: firstRevert, Reason: "撤销判断也错误"}, "s", "codex"); err != nil {
		t.Fatal(err)
	}
	beforeChanges := len(e.rt.ix.Changes())
	if _, err := e.Revert(RevertArgs{Change: confirmID, Reason: "不能沿用首个撤销"}, "s", "codex"); err == nil || !strings.Contains(err.Error(), "REVERT_CONFLICT") {
		t.Fatalf("结构化撤销后状态再变化应要求显式撤销最新记录:%v", err)
	}
	if got := len(e.rt.ix.Changes()); got != beforeChanges {
		t.Fatalf("冲突路径不应追加/沿用 journal:%d→%d", beforeChanges, got)
	}
}

func TestRevertRejectsConflictedOrEffectlessLegacyChange(t *testing.T) {
	e, _ := initEngine(t, map[string]string{"a.go": "package a\n"})
	at := time.Now().UTC()
	conflictID := model.NewChangeID(at)
	for _, what := range []string{"first", "second"} {
		if err := e.Store.AppendChange(model.Change{ID: conflictID, Nodes: []string{"."}, At: at, What: what, Why: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	legacyID := model.NewChangeID(at.Add(time.Second))
	if err := e.Store.AppendChange(model.Change{ID: legacyID, Nodes: []string{"."}, At: at.Add(time.Second), What: "legacy", Why: "unknown"}); err != nil {
		t.Fatal(err)
	}
	if err := e.Sync(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Revert(RevertArgs{Change: conflictID, Reason: "ambiguous"}, "s", "codex"); err == nil || !strings.Contains(err.Error(), "JOURNAL_CONFLICT") {
		t.Fatalf("同 ID 异内容必须 fail closed:%v", err)
	}
	if _, err := e.Revert(RevertArgs{Change: legacyID, Reason: "unknown effects"}, "s", "codex"); err == nil || !strings.Contains(err.Error(), "REVERT_UNPROVABLE") {
		t.Fatalf("旧空 effects 不得当空操作成功:%v", err)
	}
}

func TestVerifyRollbackOnShardOrJournalFailure(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("Windows chmod 不提供稳定的只读故障注入")
	}
	t.Run("second shard save", func(t *testing.T) {
		e, repo, rootRef, _ := setupVerifyFailureRepo(t)
		beforeChanges := len(e.rt.ix.Changes())
		blockedDir := filepath.Join(repo, ".knowledge/tree/b")
		if err := os.Chmod(blockedDir, 0o500); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(blockedDir, 0o755)
		_, err := e.Verify(VerifyArgs{Entry: rootRef, Verdict: "refute", Evidence: "原文反例"}, "s", "codex")
		if err == nil {
			t.Fatal("第二分片不可写时 Verify 应失败")
		}
		if got := len(e.rt.ix.Changes()); got != beforeChanges {
			t.Fatalf("SaveShard 失败仍追加 journal:%d→%d", beforeChanges, got)
		}
		assertVerifyFailureStateUnchanged(t, e)
	})

	t.Run("journal append", func(t *testing.T) {
		e, repo, rootRef, _ := setupVerifyFailureRepo(t)
		beforeChanges := len(e.rt.ix.Changes())
		journalDir := filepath.Join(repo, ".knowledge/journal")
		if err := os.Chmod(journalDir, 0o500); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(journalDir, 0o755)
		_, err := e.Verify(VerifyArgs{Entry: rootRef, Verdict: "refute", Evidence: "原文反例"}, "s", "codex")
		if err == nil {
			t.Fatal("journal 不可写时 Verify 应失败")
		}
		if got := len(e.rt.ix.Changes()); got != beforeChanges {
			t.Fatalf("AppendChange 失败仍留下 journal:%d→%d", beforeChanges, got)
		}
		assertVerifyFailureStateUnchanged(t, e)
	})
}

func TestRevertRollbackIsRetryableAfterShardOrJournalFailure(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("Windows chmod 不提供稳定的只读故障注入")
	}
	for _, failAt := range []string{"shard", "journal"} {
		t.Run(failAt, func(t *testing.T) {
			e, repo, rootRef, _ := setupVerifyFailureRepo(t)
			if _, err := e.Verify(VerifyArgs{Entry: rootRef, Verdict: "refute", Evidence: "原文反例"}, "s", "codex"); err != nil {
				t.Fatal(err)
			}
			rootID := e.rt.ix.Node("a/a.go#F").Node.Entries[0].ID
			refuteID := latestChangeID(t, e, "勘误:条目 "+rootID)
			var blocked string
			if failAt == "shard" {
				blocked = filepath.Join(repo, ".knowledge/tree/b")
			} else {
				blocked = filepath.Join(repo, ".knowledge/journal")
			}
			blockedMode := os.FileMode(0o500)
			restoreMode := os.FileMode(0o755)
			if err := os.Chmod(blocked, blockedMode); err != nil {
				t.Fatal(err)
			}
			_, err := e.Revert(RevertArgs{Change: refuteID, Reason: "故障注入"}, "s", "codex")
			if err == nil {
				t.Fatalf("%s 不可写时 Revert 应失败", failAt)
			}
			if err := os.Chmod(blocked, restoreMode); err != nil {
				t.Fatal(err)
			}
			// 失败不得留下 Reverts journal，也不得半恢复 root/cascade。
			for _, c := range e.rt.ix.Changes() {
				if c.Reverts == refuteID {
					t.Fatalf("失败形成不可重试 ALREADY_REVERTED:%+v", c)
				}
			}
			root := e.rt.ix.Node("a/a.go#F").Node.Entries[0]
			dep := e.rt.ix.Node("b/b.go#G").Node.Entries[0]
			if root.Confidence != model.ConfidenceRefuted || root.RefutedBy != refuteID || dep.Confidence != model.ConfidenceSuspect {
				t.Fatalf("Revert 失败留下半恢复:root=%+v dep=%+v", root, dep)
			}
			if _, err := e.Revert(RevertArgs{Change: refuteID, Reason: "故障解除后重试"}, "s", "codex"); err != nil {
				t.Fatalf("失败后应可重试:%v", err)
			}
		})
	}
}

func setupVerifyFailureRepo(t *testing.T) (*Engine, string, string, string) {
	t.Helper()
	e, repo := initEngine(t, map[string]string{
		"a/a.go": "package a\n\nfunc F() {}\n",
		"b/b.go": "package b\n\nfunc G() {}\n",
	})
	if _, err := e.Remember(RememberArgs{Node: "a/a.go#F", Entries: []RememberEntry{{Kind: "contract", Text: "F 是原子操作"}}}, "s", "codex"); err != nil {
		t.Fatal(err)
	}
	root := e.rt.ix.Node("a/a.go#F").Node.Entries[0]
	rootRef := "a/a.go#F#" + root.ID
	if _, err := e.Remember(RememberArgs{Node: "b/b.go#G", Entries: []RememberEntry{{Kind: "contract", Text: "G 依赖 F", BasedOn: []string{rootRef}}}}, "s", "codex"); err != nil {
		t.Fatal(err)
	}
	dep := e.rt.ix.Node("b/b.go#G").Node.Entries[0]
	return e, repo, rootRef, "b/b.go#G#" + dep.ID
}

func assertVerifyFailureStateUnchanged(t *testing.T, e *Engine) {
	t.Helper()
	root := e.rt.ix.Node("a/a.go#F").Node.Entries[0]
	dep := e.rt.ix.Node("b/b.go#G").Node.Entries[0]
	if root.Confidence != model.ConfidenceInferred || root.RefutedBy != "" || dep.Confidence != model.ConfidenceInferred {
		t.Fatalf("Verify 失败未回滚分片:root=%+v dep=%+v", root, dep)
	}
}

func latestChangeID(t *testing.T, e *Engine, whatPrefix string) string {
	t.Helper()
	changes := e.rt.ix.Changes()
	for i := len(changes) - 1; i >= 0; i-- {
		if strings.HasPrefix(changes[i].What, whatPrefix) {
			return changes[i].ID
		}
	}
	t.Fatalf("找不到 change what prefix %q", whatPrefix)
	return ""
}
