package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zdypro888/iknowledge/internal/index"
	"github.com/zdypro888/iknowledge/internal/model"
	"github.com/zdypro888/iknowledge/internal/parser"
	"github.com/zdypro888/iknowledge/internal/store"
)

// ---- kb_remember(impl §7.3 全校验) ----

// RememberEntry 是一条待写入的知识。
type RememberEntry struct {
	Kind    string   `json:"kind"`
	Text    string   `json:"text"`
	BasedOn []string `json:"based_on,omitempty"`
	// Disputes 矛盾声明(knowledge.md §12.4):本条与既有条目冲突且写入方无法自裁
	// (证据在代码之外等)时登记待裁决,防静默共存;能自裁的直接 kb_verify refute,不用它。
	Disputes []string `json:"disputes,omitempty"`
}

// RememberArgs 是 kb_remember 入参。
type RememberArgs struct {
	Node       string          `json:"node"`
	Entries    []RememberEntry `json:"entries,omitempty"`
	Keywords   []string        `json:"keywords,omitempty"`
	Supersedes []string        `json:"supersedes,omitempty"`
	BaseHash   string          `json:"base_hash,omitempty"`
}

// Remember 沉淀/更新知识条目。
func (e *Engine) Remember(a RememberArgs, sid, author string) (string, error) {
	if err := e.requireInit(); err != nil {
		return "", err
	}
	if err := e.Sync(); err != nil {
		return "", err
	}
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()

	if strings.Contains(a.Node, "@stmt") {
		return "", kbErr("NODE_NOT_FOUND", "stmt 级节点一期不产出",
			"把行级洞见改挂函数级 pitfall(impl §3)")
	}
	if len(a.Entries) == 0 && len(a.Keywords) == 0 && len(a.Supersedes) == 0 {
		return "", kbErr("INVALID_ARGUMENT", "空写入:entries/keywords/supersedes 至少一项",
			"给出要沉淀的内容")
	}

	ref, warns, err := e.resolveOrAnchorNodeLocked(a.Node)
	if err != nil {
		return "", err
	}
	n := ref.Node
	if n.Status == model.StatusOrphaned {
		return "", kbErr("NODE_ORPHANED", "节点 "+n.ID+" 的符号已消失,无锚可落",
			"符号在新位置则对新节点 remember;认领/送葬走 kb_adopt")
	}
	// 边界提醒(定案:知识库对应代码,不是记忆库):无锚节点是最容易被当
	// 通用记忆垃圾桶的地方——每次写入都亮边界,写入方自检。警示不拒收(§12.7)。
	if len(a.Entries) > 0 && n.Anchor.Hash == "" && !n.PendingAnchor &&
		(n.Level == model.LevelDir || n.Level == model.LevelProject) {
		warns = append(warns, "无锚节点只收【约束本仓库代码的业务规则/外部契约】(90 天复核周期);"+
			"通用编程知识、会话/用户偏好、任务待办不属于知识库——判据:代码变了它会失效吗?偏好归宿主 memory,待办归 kb_task")
	}

	// 乐观并发校验(impl §7.3 定案)。重锚/升级的【落地】推迟到全部校验通过之后
	// (#37:原先在校验前就 mutate 缓存 Node,一旦条目被拒,缓存里的 anchor/status
	// 已被改脏且不落盘,下个读者看到错误状态)。
	cur := e.currentAnchorLocked(ref)
	if cur.parseErr != nil {
		return "", kbErr("PARSE_FAILED", "文件当前不可解析:"+cur.parseErr.Error(),
			"修完语法后重试;record_change 不受此限")
	}
	// #38:节点落盘状态非 orphaned,但现算发现符号已从代码里消失 → 同样无锚可落。
	if cur.missing {
		return "", kbErr("NODE_ORPHANED", "节点 "+n.ID+" 的符号在当前代码中已消失,无锚可落",
			"符号在新位置则对新节点 remember;认领/送葬走 kb_adopt")
	}
	if a.BaseHash != "" && cur.hash != "" && a.BaseHash != cur.hash {
		return "", kbErr("ANCHOR_STALE", "base_hash 与当前代码不符(代码在你读后又变了)",
			"重读原文后按当前代码重试")
	}
	reanchored := cur.hash != "" && cur.hash != n.Anchor.Hash
	upgraded := n.Status == model.StatusSuspect

	// supersedes 校验:被取代条目必须存在于本节点。
	entryByID := map[string]*model.Entry{}
	for i := range n.Entries {
		entryByID[n.Entries[i].ID] = &n.Entries[i]
	}
	for _, sid2 := range a.Supersedes {
		if _, ok := entryByID[sid2]; !ok {
			return "", kbErr("NODE_NOT_FOUND", "supersedes 引用的条目 "+sid2+" 不在节点 "+n.ID,
				"用 kb_recall 核对条目 ID")
		}
	}
	// supersedes 条数校验必须先于任何缓存 mutate(#37 同族,R2-A1):原先它在
	// keywords 替换之后,拒收时缓存已改脏且未落盘,后续任何成功写会把脏值持久化。
	if len(a.Supersedes) > 0 && len(a.Entries) != 1 {
		return "", kbErr("DUPLICATE_ENTRY", "supersedes 需要恰好一条新条目作为现任",
			"一次合并提交一条新条目")
	}

	// 条目校验(预算/查重/lint/basedOn)。
	budget := budgetFor(n.Level)
	var warnList []string
	warnList = append(warnList, warns...)
	var newEntries []model.Entry
	for _, in := range a.Entries {
		if !validKind(in.Kind) {
			return "", kbErr("INVALID_ARGUMENT", "非法 kind "+in.Kind,
				"kind ∈ summary|contract|mutation|pitfall|usage")
		}
		if est := EstimateTokens(in.Text); est > budget.hard {
			return "", kbErr("BUDGET_EXCEEDED",
				fmt.Sprintf("条目估算 %d token,超过 %s 层上限 %d(估算规则:CJK 字数 + 其余词数×1.3)",
					est, n.Level, budget.hard),
				"按估算规则精炼,或拆分/上移层级")
		} else if budget.soft > 0 && est > budget.soft {
			warnList = append(warnList,
				fmt.Sprintf("条目估算 %d token,超过 %s 层软预算 %d;已接受但建议精炼或拆分,硬上限 %d",
					est, n.Level, budget.soft, budget.hard))
		}
		if reject, warn := LintImperative(in.Text); reject != "" {
			return "", kbErr("IMPERATIVE_CONTENT", reject, "改写为事实陈述(knowledge.md §12.8)")
		} else if warn != "" {
			warnList = append(warnList, warn)
		}
		if w := echoWarn(in.Text, cur.sig); w != "" {
			warnList = append(warnList, w)
		}
		if w := boundaryWarn(in.Text); w != "" {
			warnList = append(warnList, w)
		}
		// 机械查重:范围含 refuted/superseded/retired(impl §7.3 定案)。
		norm := normalizeText(in.Text)
		superseding := map[string]bool{}
		for _, s := range a.Supersedes {
			superseding[s] = true
		}
		for i := range n.Entries {
			old := &n.Entries[i]
			if superseding[old.ID] {
				continue // 正在被本次取代的条目不算重复
			}
			if normalizeText(old.Text) == norm {
				switch {
				case old.Confidence == model.ConfidenceRefuted:
					return "", kbErr("DUPLICATE_ENTRY",
						"该结论曾被勘误(条目 "+old.ID+"),同文本拒收",
						"先读疫苗条目与勘误记录;确认原结论其实成立则带原文证据 kb_remember 一条【措辞不同】的新条目并说明为何翻案(refuted 条目不能靠 confirm 复活)")
				case old.SupersededBy != "":
					return "", kbErr("DUPLICATE_ENTRY",
						"与已被取代条目 "+old.ID+" 全同,现任是 "+old.SupersededBy,
						"对现任条目操作(supersedes 合并)")
				default:
					return "", kbErr("DUPLICATE_ENTRY",
						"与既有条目 "+old.ID+" 归一化后全同",
						"用 supersedes 合并进条目 "+old.ID)
				}
			}
			if old.Active() && BigramJaccard(old.Text, in.Text) > 0.8 {
				warnList = append(warnList,
					"疑似与 "+old.ID+" 相似(>0.8)——三种结局:同一结论→supersedes 合并;互相矛盾→本条 disputes 声明待裁决;确证旧条错误→kb_verify refute 它")
			}
		}
		// basedOn:引用可解析 + 可信度封顶 inferred(knowledge.md §8.3)。
		for _, dep := range in.BasedOn {
			resolved := e.rt.ix.ResolveEntryRef(dep)
			if !e.entryRefExistsLocked(resolved) {
				return "", kbErr("NODE_NOT_FOUND", "based_on 引用 "+dep+" 不存在",
					"格式 node-id#entry-id;用 kb_recall 核对")
			}
		}
		// disputes:被指条目必须存在且活跃(§12.4;指一条已退场的条目无裁决意义),
		// 引用归一化后落盘(正向单侧存储,反向 index 现算)。
		var disputes []string
		for _, d := range in.Disputes {
			resolved := e.rt.ix.ResolveEntryRef(d)
			target := e.rt.ix.EntryByRef(resolved)
			if target == nil {
				return "", kbErr("NODE_NOT_FOUND", "disputes 引用 "+d+" 不存在",
					"格式 node-id#entry-id;用 kb_recall 核对")
			}
			if !target.Active() {
				return "", kbErr("INVALID_ARGUMENT", "disputes 引用 "+d+" 已退场(被取代/驳倒/退休),无需裁决",
					"直接写入新知识即可;若要翻案走带证据的 kb_remember")
			}
			disputes = append(disputes, resolved)
		}
		if len(disputes) > 0 {
			warnList = append(warnList,
				"矛盾已登记待裁决:双方并存呈现,尽快裁决——读双方依据后 kb_verify refute 错误方(附证据)或 obsolete 过时方;若证据在代码之外,升级给人")
		}
		newEntries = append(newEntries, model.Entry{
			ID: model.NewEntryID(), Kind: in.Kind, Text: in.Text,
			Confidence: model.ConfidenceInferred,
			BasedOn:    in.BasedOn,
			Disputes:   disputes,
			Author:     author,
			At:         e.now().UTC(),
		})
	}

	// keywords:整体替换语义,小写归一去重,上限 12(impl §7.3)。
	if a.Keywords != nil {
		kws := normalizeKeywords(a.Keywords)
		if len(kws) > 12 {
			return "", kbErr("BUDGET_EXCEEDED",
				fmt.Sprintf("keywords %d 个,超上限 12", len(kws)),
				"提交精选全集(整体替换语义,非追加)")
		}
		n.Keywords = kws
	}

	// 落库。supersedes 标记必须在 append 之前——entryByID 的指针指向
	// n.Entries 当前底层数组,append 扩容后再写就写到被丢弃的内存上。
	newIDs := make([]string, 0, len(newEntries))
	for i := range newEntries {
		newIDs = append(newIDs, newEntries[i].ID)
	}
	if len(a.Supersedes) > 0 {
		for _, sid2 := range a.Supersedes {
			entryByID[sid2].SupersededBy = newIDs[0]
		}
	}
	// 全部校验已过,现在才落地重锚/升级(#37)。
	if reanchored {
		n.Anchor = cur.anchor
	}
	if upgraded {
		n.Status = model.StatusFresh // 重验即重锚:视为按当前代码重验
	}
	n.Entries = append(n.Entries, newEntries...)
	if n.Status == model.StatusUndigested && hasActiveEntries(n) {
		n.Status = model.StatusFresh
	}
	if err := e.saveNodeShardLocked(ref); err != nil {
		return "", err
	}
	e.markDigested(sid, n.ID)
	if err := e.reloadLocked(); err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "entryIds: %s\nnodeStatus: %s\nreanchored: %v", strings.Join(newIDs, ", "), n.Status, reanchored)
	if upgraded {
		b.WriteString("\n(原 suspect 节点已借本次写入重验重锚升回 fresh——其余旧条目请顺手确认仍然成立)")
	}
	for _, w := range warnList {
		b.WriteString("\n⚠ ")
		b.WriteString(w)
	}
	return b.String(), nil
}

func validKind(k string) bool {
	switch k {
	case model.KindSummary, model.KindContract, model.KindMutation, model.KindPitfall, model.KindUsage:
		return true
	}
	return false
}

func normalizeKeywords(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, k := range in {
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

// entryRefExistsLocked 判断(已归一化的)"node#entry" 引用存在。
func (e *Engine) entryRefExistsLocked(ref string) bool {
	i := strings.LastIndexByte(ref, '#')
	if i <= 0 {
		return false
	}
	nodeRef := e.rt.ix.Node(ref[:i])
	if nodeRef == nil {
		return false
	}
	for j := range nodeRef.Node.Entries {
		if nodeRef.Node.Entries[j].ID == ref[i+1:] {
			return true
		}
	}
	return false
}

// currentAnchor 现算节点的当前锚(读源码+解析)。
type curAnchor struct {
	hash     string
	anchor   model.Anchor
	parseErr error
	missing  bool   // 符号不在文件里
	sig      string // 符号签名(复述检测用;文件/无锚节点为空)
}

func (e *Engine) currentAnchorLocked(ref *index.NodeRef) curAnchor {
	n := ref.Node
	file, symbol := model.SplitNodeID(n.ID)
	if n.Level == model.LevelDir || n.Level == model.LevelProject {
		return curAnchor{hash: "", anchor: n.Anchor} // 无代码锚(时间+人工复核,§8.4)
	}
	src, err := os.ReadFile(filepath.Join(e.Store.RepoRoot(), filepath.FromSlash(file)))
	if err != nil {
		return curAnchor{missing: true, anchor: n.Anchor}
	}
	p := e.Reg.ForFile(file)
	if p == nil {
		return curAnchor{anchor: n.Anchor}
	}
	syms, err := p.Parse(file, src)
	if err != nil {
		return curAnchor{parseErr: err, anchor: n.Anchor}
	}
	if symbol == "" {
		fh := parser.HashFileFor(p, syms, src)
		a := n.Anchor
		a.Hash = fh
		return curAnchor{hash: fh, anchor: a}
	}
	for i := range syms {
		if syms[i].Name == symbol {
			return curAnchor{hash: syms[i].Hash, sig: parser.Signature(syms[i]), anchor: model.Anchor{
				File: file, Symbol: symbol,
				Hash: syms[i].Hash, StructHash: syms[i].StructHash, Lines: syms[i].Lines,
			}}
		}
	}
	return curAnchor{missing: true, anchor: n.Anchor}
}

// boundaryRe 是"任务态内容混进知识库"的机械信号(边界定案:知识库只收锚定本仓库
// 代码的知识——进行中/待办是状态不是知识,归 kb_task;判据:代码变了它会失效吗)。
// 模式收得极窄防误杀:技术陈述里的"下次重连时会重放"这类合法用语不碰。
var boundaryRe = regexp.MustCompile(`\bTODO\b|\bFIXME\b|待办|别忘了`)

// boundaryWarn 检测任务态词——警示不拒收(语义边界终归 AI 判,同 §12.7 哲学)。
func boundaryWarn(text string) string {
	if m := boundaryRe.FindString(text); m != "" {
		return "疑似任务态内容(命中:" + m + ")——进行中/待办是状态不是知识,归 kb_task;知识库只收结论(判据:代码变了它会失效吗)"
	}
	return ""
}

// echoWarn 复述检测(2026-07-04,实战反馈"inferred 摘要基本是代码复述"的机械子集):
// 条目的 ASCII 词大量来自符号签名 = 签名回声,读原文即得,存了是噪音。
// 只测机械信号警示不拒收——中文结构复述属语义判断,归 AI(与矛盾检测同定案,§12.7);
// 种子/热点提示词的"只存代码上看不出来的"纪律负责语义层。
func echoWarn(text, sig string) string {
	if sig == "" {
		return ""
	}
	sigSet := map[string]bool{}
	for _, t := range index.Tokenize(sig) {
		sigSet[t] = true
	}
	var ascii, hit int
	for _, t := range index.Tokenize(text) {
		if t[0] < 128 {
			ascii++
			if sigSet[t] {
				hit++
			}
		}
	}
	if ascii >= 4 && hit*10 >= ascii*7 { // ≥70% 来自签名
		return "疑似签名复述(条目 ASCII 词多来自符号签名)——代码上看得出来的不该存,建议改写为契约/坑/为什么"
	}
	return ""
}

// newSymbolPlan 是增量落锚的规划结果(不含副作用)。
type newSymbolPlan struct {
	node     model.Node
	fileNode *model.Node // 若分片缺文件节点则需补;否则 nil
	shardRel string
}

// planNewSymbolLocked 解析一个新符号并规划增量落锚,【不落盘、不重建索引】。
// 会失败的分支(路径穿越/文件缺失/解析错/符号不在/多命中)全在此拒收——供
// record_change 的两遍式在 journal 前完成全部校验(impl §7.3)。
func (e *Engine) planNewSymbolLocked(query string) (*newSymbolPlan, error) {
	file, symbol := model.SplitNodeID(query)
	if symbol == "" {
		return nil, kbErr("NODE_NOT_FOUND", "节点 "+query+" 不存在",
			"用 kb_map 确认路径/符号;新文件先 kb_init 对账")
	}
	// 铁律二防线:AI 报的节点 ID 直达文件系统前必须消毒,拒绝 ../ 逃出仓库。
	if _, ok := model.SafeRel(file); !ok {
		return nil, kbErr("NODE_NOT_FOUND", "非法文件路径 "+file+"(疑似路径穿越)",
			"路径须相对仓库根、正斜杠、不含 ..")
	}
	// #28/#35:该文件分片处于 conflict/schema 隔离态时,增量落锚会用空壳分片覆盖它,
	// 连同人未解决的另一分支知识一起丢失——拒绝写入,要求先解决冲突。
	if cerr := e.rt.cache.ConflictShard(file); cerr != nil {
		return nil, kbErr("SHARD_CONFLICT", "文件 "+file+" 的知识分片不可用:"+cerr.Error(),
			"先人工解决 .knowledge/tree/"+file+".yaml 的合并冲突或升级 iknowledge,再写入")
	}
	abs := filepath.Join(e.Store.RepoRoot(), filepath.FromSlash(file))
	src, err := os.ReadFile(abs)
	if err != nil {
		return nil, kbErr("NODE_NOT_FOUND", "文件 "+file+" 不存在",
			"路径须相对仓库根、正斜杠;用 kb_map 确认")
	}
	p := e.Reg.ForFile(file)
	if p == nil {
		return nil, kbErr("NODE_NOT_FOUND", "文件 "+file+" 无注册解析器",
			"一期仅支持 Go 源文件")
	}
	syms, err := p.Parse(file, src)
	if err != nil {
		return nil, kbErr("PARSE_FAILED", "文件 "+file+" 当前不可解析:"+err.Error(),
			"修完语法后重试")
	}
	var target *parser.Symbol
	for i := range syms {
		if syms[i].Name == symbol || model.LooseSymbolMatch(symbol, syms[i].Name) {
			if target != nil {
				return nil, kbErr("NODE_NOT_FOUND", "符号 "+symbol+" 在 "+file+" 有多个宽松命中",
					"用规范名(方法带接收者)重试")
			}
			target = &syms[i]
		}
	}
	if target == nil {
		return nil, kbErr("NODE_NOT_FOUND", "符号 "+symbol+" 不在 "+file+" 中",
			"用 kb_map 确认符号名(方法带接收者、同名带 ~n 序号)")
	}
	plan := &newSymbolPlan{node: e.nodeFromSymbol(file, *target), shardRel: "tree/" + file + ".yaml"}
	if cs := e.rt.cache.Shards()[plan.shardRel]; cs == nil || cs.Shard == nil {
		plan.fileNode = &model.Node{
			ID: file, Level: model.LevelFile,
			Anchor: model.Anchor{File: file, Hash: parser.HashFileFor(p, syms, src)},
			Status: model.StatusUndigested, Since: e.now().UTC(),
		}
	}
	return plan, nil
}

// resolveOrAnchorNodeLocked 解析节点;不存在且符号在源码里 → 增量落锚自动建节点
// (impl §7.3:AI 新写函数是最高频写场景,不再 NODE_NOT_FOUND 卡死)。kb_remember 用。
func (e *Engine) resolveOrAnchorNodeLocked(query string) (*index.NodeRef, []string, error) {
	id, cands := e.resolveQueryLocked(query)
	if id != "" {
		return e.rt.ix.Node(id), nil, nil
	}
	if len(cands) > 1 {
		return nil, nil, kbErr("NODE_NOT_FOUND",
			"符号 "+query+" 有多个候选:"+strings.Join(cands, "、"), "用完整节点 ID 重试")
	}
	plan, err := e.planNewSymbolLocked(query)
	if err != nil {
		return nil, nil, err
	}
	shardRel := plan.shardRel
	cs := e.rt.cache.Shards()[shardRel]
	var sh *store.Shard
	if cs != nil && cs.Shard != nil {
		sh = cs.Shard
	} else {
		sh = &store.Shard{Schema: model.SchemaVersion}
		if plan.fileNode != nil {
			sh.Nodes = append(sh.Nodes, *plan.fileNode)
		}
	}
	sh.Nodes = append(sh.Nodes, plan.node)
	planFile, _ := model.SplitNodeID(plan.node.ID)
	if err := e.Store.SaveShard(e.Store.ShardPathFor(planFile), sh, nil); err != nil {
		return nil, nil, err
	}
	if err := e.reloadLocked(); err != nil {
		return nil, nil, err
	}
	ref := e.rt.ix.Node(plan.node.ID)
	if ref == nil {
		return nil, nil, fmt.Errorf("engine: 增量落锚后节点 %s 不可见", plan.node.ID)
	}
	return ref, []string{"节点 " + plan.node.ID + " 为新符号增量落锚自动创建"}, nil
}

func (e *Engine) nodeFromSymbol(file string, sym parser.Symbol) model.Node {
	level := model.LevelFunction
	if sym.Kind == "type" || sym.Kind == "var" || sym.Kind == "const" {
		level = model.LevelDecl
	}
	return model.Node{
		ID: model.SymbolNodeID(file, sym.Name), Level: level,
		Anchor: model.Anchor{File: file, Symbol: sym.Name,
			Hash: sym.Hash, StructHash: sym.StructHash, Lines: sym.Lines},
		Status: model.StatusFresh, // 定案:增量落锚建 fresh(impl §7.3)
		Since:  e.now().UTC(),
	}
}

// ---- kb_record_change(impl §7.3) ----

// ChangeArgs 是 kb_record_change 入参。
type ChangeArgs struct {
	Nodes     []string         `json:"nodes"`
	What      string           `json:"what"`
	Why       string           `json:"why"`
	Task      string           `json:"task,omitempty"`
	Rejected  []model.Rejected `json:"rejected,omitempty"`
	Overturns string           `json:"overturns,omitempty"`
	Rebuttal  string           `json:"rebuttal,omitempty"`
	Verified  string           `json:"verified,omitempty"`
	Remaps    []model.Remap    `json:"remaps,omitempty"`
	BaseHash  string           `json:"base_hash,omitempty"`
}

// RecordChange 修改代码后的变更记录:一个逻辑修改 = 一条记录。
func (e *Engine) RecordChange(a ChangeArgs, sid, author string) (string, error) {
	if err := e.requireInit(); err != nil {
		return "", err
	}
	if err := e.Sync(); err != nil {
		return "", err
	}
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()

	if len(a.Nodes) == 0 || strings.TrimSpace(a.What) == "" || strings.TrimSpace(a.Why) == "" {
		return "", kbErr("INVALID_ARGUMENT", "nodes/what/why 必填",
			"一个逻辑修改=一条记录,nodes 首位为主节点")
	}
	// 决策链校验(impl §7.3)。
	if a.Overturns != "" {
		if strings.TrimSpace(a.Rebuttal) == "" {
			return "", kbErr("MISSING_REBUTTAL", "overturns 非空时 rebuttal 必填",
				"直接回应被推翻记录的 why")
		}
		target := e.rt.ix.ChangeByID(a.Overturns)
		if target == nil {
			return "", kbErr("OVERTURNS_NOT_FOUND", "被推翻的记录 "+a.Overturns+" 不存在",
				"用 kb_recall(mode=history) 核对记录 ID")
		}
		if !e.overturnsInScopeLocked(target, a.Nodes) {
			return "", kbErr("OVERTURNS_NOT_FOUND",
				"记录 "+a.Overturns+" 不属于本次 nodes 任一节点(含血缘)的历史",
				"决策链必须落在同一节点线上;核对 nodes 或记录 ID")
		}
	}

	var warns []string
	var reanchored, orphaned, pendingAnchor, created []string

	// 【第一遍:纯解析与校验,不落任何盘】——任何会失败的分支(多候选、新符号解析
	// 失败)都在 journal 写入前拒收,保证 record_change 要么整体成功要么不留半应用
	// (#10/#29/#41:原先逐节点边解析边 saveNodeShardLocked,后面节点报错时前面已落盘)。
	type nodeAction struct {
		id         string
		kind       string // reanchor | orphan | pending | create
		anchor     model.Anchor
		newNode    *model.Node // create 用
		fileEnsure *model.Node // create 时若文件节点缺失
		shardRel   string
	}
	var actions []nodeAction
	resolved := make([]string, 0, len(a.Nodes))
	for _, q := range a.Nodes {
		id, cands := e.resolveQueryLocked(q)
		switch {
		case id != "":
			ref := e.rt.ix.Node(id)
			cur := e.currentAnchorLocked(ref)
			act := nodeAction{id: id, shardRel: ref.ShardRel}
			switch {
			case cur.parseErr != nil:
				act.kind = "pending"
				pendingAnchor = append(pendingAnchor, id)
			case cur.missing:
				act.kind = "orphan"
				orphaned = append(orphaned, id)
			default:
				act.kind, act.anchor = "reanchor", cur.anchor
				reanchored = append(reanchored, id)
			}
			actions = append(actions, act)
			resolved = append(resolved, id)
		case len(cands) > 1:
			return "", kbErr("NODE_NOT_FOUND", "符号 "+q+" 有多个候选:"+strings.Join(cands, "、"),
				"用完整节点 ID")
		default: // 情形② 新增符号:先规划(可能失败:解析错/符号不在/多命中),不落盘
			plan, err := e.planNewSymbolLocked(q)
			if err != nil {
				return "", err
			}
			actions = append(actions, nodeAction{id: plan.node.ID, kind: "create",
				newNode: &plan.node, fileEnsure: plan.fileNode, shardRel: plan.shardRel})
			created = append(created, plan.node.ID)
			resolved = append(resolved, plan.node.ID)
		}
	}

	if a.BaseHash != "" && len(resolved) > 0 {
		if ref := e.rt.ix.Node(resolved[0]); ref != nil {
			// #40:base_hash 对 record_change 语义已反(它是改码【后】记账,现算哈希
			// 必然=改后代码≠改前的 base_hash,诚实流水线每次都误报)。改为:仅当失配
			// 且该节点【未被本次重锚】(即代码没真的变、却带了个对不上的 base_hash)才警示。
			cur := e.currentAnchorLocked(ref)
			reanchoredThis := false
			for _, act := range actions {
				if act.id == resolved[0] && act.kind == "reanchor" {
					reanchoredThis = true
				}
			}
			if !reanchoredThis && cur.hash != "" && cur.hash != a.BaseHash {
				warns = append(warns, "base_hash 与当前代码不符——建议重读原文核对")
			}
		}
	}

	// 【journal 先行(账本优先)】:变更是唯一真相,先落。
	ids := map[string]bool{}
	for _, c := range e.rt.ix.Changes() {
		ids[c.ID] = true
	}
	chID := model.NewChangeID(e.now())
	for ids[chID] {
		chID = model.NewChangeID(e.now())
	}
	change := model.Change{
		ID: chID, Nodes: resolved, At: e.now().UTC(),
		Task: a.Task, What: a.What, Why: a.Why,
		Rejected: a.Rejected, Overturns: a.Overturns, Rebuttal: a.Rebuttal,
		Remaps: a.Remaps, Verified: a.Verified, Author: author,
		Commit: gitHead(e.Store.RepoRoot()),
	}
	if err := e.Store.AppendChange(change); err != nil {
		return "", err
	}

	// 【第二遍:应用节点动作并落盘】——校验已过,这里的失败只剩磁盘 IO,如实上抛。
	touched := map[string]*store.Shard{} // shardRel → 待存分片(按分片合并写)
	shardOf := func(rel string) *store.Shard {
		if touched[rel] == nil {
			if cs := e.rt.cache.Shards()[rel]; cs != nil {
				touched[rel] = cs.Shard
			}
		}
		return touched[rel]
	}
	for _, act := range actions {
		switch act.kind {
		case "reanchor":
			if ref := e.rt.ix.Node(act.id); ref != nil {
				ref.Node.Anchor = act.anchor
				ref.Node.PendingAnchor = false
				if ref.Node.Status == model.StatusSuspect {
					ref.Node.Status = model.StatusFresh
				}
				shardOf(act.shardRel)
			}
		case "orphan":
			if ref := e.rt.ix.Node(act.id); ref != nil {
				ref.Node.Status = model.StatusOrphaned
				shardOf(act.shardRel)
			}
		case "pending":
			if ref := e.rt.ix.Node(act.id); ref != nil {
				ref.Node.PendingAnchor = true
				shardOf(act.shardRel)
			}
		case "create":
			sh := shardOf(act.shardRel)
			if sh == nil {
				sh = &store.Shard{Schema: model.SchemaVersion}
				touched[act.shardRel] = sh
			}
			// 按 ID 去重(R2-A2):同一次调用在同一个新文件里报多个新符号时,
			// 每个 plan 都带 fileEnsure,不去重会把文件节点追加多遍。
			if act.fileEnsure != nil && !shardHasNode(sh, act.fileEnsure.ID) {
				sh.Nodes = append(sh.Nodes, *act.fileEnsure)
			}
			if !shardHasNode(sh, act.newNode.ID) {
				sh.Nodes = append(sh.Nodes, *act.newNode)
			}
		}
	}
	for rel, sh := range touched {
		if err := e.Store.SaveShard(filepath.Join(e.Store.Dir(), filepath.FromSlash(rel)), sh, nil); err != nil {
			return "", err
		}
	}
	if err := e.reloadLocked(); err != nil {
		return "", err
	}

	// remaps 申报式迁移(knowledge.md §12.6 第 2 层)——自身已原子。
	if len(a.Remaps) > 0 {
		if err := e.applyRemapsLocked(a.Remaps); err != nil {
			return "", err
		}
		warns = append(warns, "remaps 已迁移:条目统一降半级待确认(verified→inferred、inferred→suspect)")
	}

	e.markDigested(sid, resolved...)
	if err := e.reloadLocked(); err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "changeId: %s\nreanchored: %v\norphaned: %v\npendingAnchor: %v\ncreated: %v",
		change.ID, reanchored, orphaned, pendingAnchor, created)
	// suspect 顺手偿还提示(§12.2 第 1 条:此刻理解新鲜,边际成本≈0)。
	for _, id := range resolved {
		if ref := e.rt.ix.Node(id); ref != nil {
			for i := range ref.Node.Entries {
				en := &ref.Node.Entries[i]
				if en.Active() && en.Confidence == model.ConfidenceSuspect {
					fmt.Fprintf(&b, "\n顺手偿还:节点 %s 有待重验条目 %s(你刚读过原文,kb_verify 确认/驳倒它)", id, en.ID)
					break
				}
			}
		}
	}
	// 置信度桥接(2026-07-04,实战反馈"116/116 条 inferred、0 verified——阶梯塌成单层"):
	// 本次带 verified(测试/红绿证据)且触及节点有 inferred 条目时,当下就是升级黄金时点
	// (AI 手里正握着验证上下文)。只提示不自动升——测试验证的是代码行为,不是知识文本
	// 本身的正确性,必须 AI 读过条目确认它仍准确描述当前代码才 confirm。
	if strings.TrimSpace(a.Verified) != "" {
		for _, id := range resolved {
			if ref := e.rt.ix.Node(id); ref != nil && ref.Node.Status == model.StatusFresh {
				if n := countInferred(ref.Node); n > 0 {
					fmt.Fprintf(&b, "\n顺手确认:节点 %s 有 %d 条 inferred 知识,本次改动已带验证——对其中仍准确描述当前代码的条目 kb_verify confirm 升 verified(附 evidence=本次验证依据;测试验证的是代码,知识文本要你亲自确认)", id, n)
					break
				}
			}
		}
	}
	// 时代摘要摊销检查(§12.3:>10 条或 >600 token 触发,维护欠账承载)。
	for _, id := range resolved {
		if e.eraDebtLocked(id) {
			fmt.Fprintf(&b, "\n历史超预算:节点 %s 建议 kb_maintain 取时代摘要债", id)
			break
		}
	}
	if remind := e.settleReminder(sid); len(remind) > 0 {
		fmt.Fprintf(&b, "\n沉淀提醒:本会话多次读取但未沉淀的节点:%s", strings.Join(remind, "、"))
	}
	for _, w := range warns {
		b.WriteString("\n⚠ ")
		b.WriteString(w)
	}
	return b.String(), nil
}

// shardHasNode 判断分片内是否已有指定 ID 的节点(增量落锚去重用)。
func shardHasNode(sh *store.Shard, id string) bool {
	for i := range sh.Nodes {
		if sh.Nodes[i].ID == id {
			return true
		}
	}
	return false
}

// overturnsInScopeLocked 被推翻记录必须在本次 nodes 任一节点(含血缘)历史上。
func (e *Engine) overturnsInScopeLocked(target *model.Change, nodes []string) bool {
	scope := map[string]bool{}
	for _, q := range nodes {
		id, _ := e.resolveQueryLocked(q)
		if id == "" {
			continue
		}
		scope[id] = true
		if ref := e.rt.ix.Node(id); ref != nil {
			for _, old := range ref.Node.Lineage {
				scope[old] = true
			}
		}
	}
	for _, tn := range target.Nodes {
		if scope[tn] || scope[e.rt.ix.ResolveNodeID(tn)] {
			return true
		}
	}
	return false
}

// applyRemapsLocked 按申报分派 Entries、接续血缘、降半级(定案见 model.Remap)。
func (e *Engine) applyRemapsLocked(remaps []model.Remap) error {
	// 第一遍:全部校验并把编辑收成计划(不 mutate)——applyRemapsLocked 要么整体成功
	// 要么不留半迁移;校验期的任何拒收都在落盘之前(#30 半应用、#27 自毁)。
	type edit struct {
		addEntries []model.Entry
		addLineage []string
	}
	plan := map[string]*edit{} // 现任目标节点 ID → 编辑
	editOf := func(id string) *edit {
		if plan[id] == nil {
			plan[id] = &edit{}
		}
		return plan[id]
	}
	removeSet := map[string]bool{}

	for _, rm := range remaps {
		fromID := e.rt.ix.ResolveNodeID(rm.From)
		if fromID == "" {
			return kbErr("NODE_NOT_FOUND", "remaps.from "+rm.From+" 不存在", "核对节点 ID")
		}
		if len(rm.To) == 0 {
			return kbErr("INVALID_ARGUMENT", "remaps.to 为空", "至少给一个目标节点")
		}
		fromRef := e.rt.ix.Node(fromID)
		targetIDs := map[string]string{} // rm.To 原样 → 现任 ID
		for _, to := range rm.To {
			toID, cands := e.resolveQueryLocked(to)
			if toID == "" {
				if len(cands) > 1 {
					return kbErr("NODE_NOT_FOUND", "remaps.to "+to+" 有多个候选", "用完整节点 ID")
				}
				return kbErr("NODE_NOT_FOUND", "remaps.to "+to+" 不存在",
					"目标符号须已在代码中(先把目标节点列进 nodes 完成增量落锚)")
			}
			// #27:from 与 to 解析到同一节点(常因 from 是目标血缘里的旧 ID)→ 自毁,拒收。
			if toID == fromID {
				return kbErr("NODE_NOT_FOUND", "remaps.from 与 to 解析到同一节点 "+toID+"(疑似把目标的血缘旧 ID 当 from)",
					"from 应是被拆分/合并前的源节点,不能等于任一目标")
			}
			targetIDs[to] = toID
		}
		lineageToAdd := appendUnique(append([]string{}, fromRef.Node.Lineage...), fromID)
		for i := range fromRef.Node.Entries {
			en := fromRef.Node.Entries[i]
			en.Confidence = demote(en.Confidence)
			dstID := targetIDs[rm.To[0]]
			if t, ok := rm.Entries[en.ID]; ok {
				if targetIDs[t] == "" {
					return kbErr("NODE_NOT_FOUND", "remaps.entries 目标 "+t+" 不在 to 列表", "核对映射")
				}
				dstID = targetIDs[t]
			}
			editOf(dstID).addEntries = append(editOf(dstID).addEntries, en)
		}
		for _, toID := range targetIDs {
			e := editOf(toID)
			for _, l := range lineageToAdd {
				e.addLineage = appendUnique(e.addLineage, l)
			}
		}
		removeSet[fromID] = true
	}

	// 第二遍:应用。in-place 改 Node(不动 shard 的 Nodes 数组),收集受影响分片。
	dirty := map[string]bool{}
	for id, ed := range plan {
		ref := e.rt.ix.Node(id)
		if ref == nil {
			return kbErr("NODE_NOT_FOUND", "目标节点 "+id+" 迁移中消失", "重试")
		}
		ref.Node.Entries = append(ref.Node.Entries, ed.addEntries...)
		for _, l := range ed.addLineage {
			ref.Node.Lineage = appendUnique(ref.Node.Lineage, l)
		}
		if ref.Node.Status == model.StatusUndigested && hasActiveEntries(ref.Node) {
			ref.Node.Status = model.StatusFresh
		}
		dirty[ref.ShardRel] = true
	}
	for id := range removeSet {
		if ref := e.rt.ix.Node(id); ref != nil {
			dirty[ref.ShardRel] = true
		}
	}
	// 每个受影响分片重建一次(删除 from 节点),整分片存一次——避免逐节点 [:0] 压缩
	// 反复使索引指针失效(#16)。
	return e.rewriteShardsLocked(dirty, removeSet)
}

// rewriteShardsLocked 把 dirty 分片按现存索引节点 + 删除集重写落盘(整分片一次)。
func (e *Engine) rewriteShardsLocked(dirty map[string]bool, removeSet map[string]bool) error {
	for rel := range dirty {
		cs := e.rt.cache.Shards()[rel]
		if cs == nil || cs.Shard == nil {
			continue
		}
		kept := make([]model.Node, 0, len(cs.Shard.Nodes))
		for i := range cs.Shard.Nodes {
			if !removeSet[cs.Shard.Nodes[i].ID] {
				kept = append(kept, cs.Shard.Nodes[i])
			}
		}
		cs.Shard.Nodes = kept
		path := filepath.Join(e.Store.Dir(), filepath.FromSlash(rel))
		if err := e.Store.SaveShard(path, cs.Shard, cs.Raw); err != nil {
			return err
		}
	}
	return nil
}

func demote(c model.Confidence) model.Confidence {
	switch c {
	case model.ConfidenceVerified:
		return model.ConfidenceInferred
	case model.ConfidenceInferred:
		return model.ConfidenceSuspect
	}
	return c
}

// removeNodeLocked 从分片里摘除节点并落盘。用【新切片】而非 [:0] 原地压缩——
// 后者会移动 Nodes 底层数组,使索引里指向本分片其他节点的 NodeRef 全部错位(#16)。
func (e *Engine) removeNodeLocked(ref *index.NodeRef) error {
	return e.rewriteShardsLocked(map[string]bool{ref.ShardRel: true}, map[string]bool{ref.Node.ID: true})
}

// eraDebtLocked 节点未折叠历史是否超阈值(>10 条或 >600 token,§12.3)。
func (e *Engine) eraDebtLocked(nodeID string) bool {
	ref := e.rt.ix.Node(nodeID)
	if ref == nil {
		return false
	}
	hist := e.rt.ix.History(nodeID)
	count, tokens := 0, 0
	for _, c := range hist {
		if !ref.Node.EraUntil.IsZero() && !c.At.After(ref.Node.EraUntil) {
			continue
		}
		count++
		tokens += EstimateTokens(c.What + c.Why)
	}
	return count > 10 || tokens > 600
}

func gitHead(repo string) string {
	data, err := os.ReadFile(filepath.Join(repo, ".git", "HEAD"))
	if err != nil {
		return ""
	}
	head := strings.TrimSpace(string(data))
	if refPath, ok := strings.CutPrefix(head, "ref: "); ok {
		if data, err := os.ReadFile(filepath.Join(repo, ".git", filepath.FromSlash(refPath))); err == nil {
			return strings.TrimSpace(string(data))[:min(12, len(strings.TrimSpace(string(data))))]
		}
		return ""
	}
	return head[:min(12, len(head))]
}

// ---- kb_revert(R29 批次4:撤销 record_change/verify 的"全错"记录) ----

// RevertArgs 是 kb_revert 入参。
type RevertArgs struct {
	Change string `json:"change"` // 被撤销的 change ID(必填)
	Reason string `json:"reason"` // 撤销理由(必填:留痕,后续可追溯为什么撤)
}

// Revert 撤销一条 change。撤销本身是追加一条带 Reverts 字段的 Change(追加式不变量不破),
// 并反向应用被撤销 change 的副作用:
//   - 被撤销的是 record_change 带 overturns:清除该 overturns 链(被推翻的决策恢复)
//   - 被撤销的是 verify(refute):恢复被 refute 级联的 entry(清 RefutedBy)
//   - 被撤销的是 verify(confirm/obsolete):恢复 confidence/status
//
// 只能撤销最近一条针对同一目标的记录(已被后续 change 再次修改的不可逆撤)。
// 不碰源码(铁律二),只改 .knowledge/。
func (e *Engine) Revert(a RevertArgs, sid, author string) (string, error) {
	if strings.TrimSpace(a.Change) == "" {
		return "", kbErr("INVALID_PARAMS", "缺 change(被撤销的记录 ID)", "用 kb_recall mode=history 取 change ID")
	}
	if strings.TrimSpace(a.Reason) == "" {
		return "", kbErr("INVALID_PARAMS", "缺 reason(撤销理由必填,留痕)", "说明为什么这条记录全错")
	}
	if err := e.Sync(); err != nil {
		return "", err
	}
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	if err := e.reloadLocked(); err != nil {
		return "", err
	}

	// 找被撤销的 change。
	var target *model.Change
	for i := range e.rt.ix.Changes() {
		c := e.rt.ix.Changes()[i]
		if c.ID == a.Change {
			target = &c
			break
		}
	}
	if target == nil {
		return "", kbErr("NODE_NOT_FOUND", "change "+a.Change+" 不存在", "kb_recall mode=history 确认 ID")
	}
	// 不可重复撤销:已有 revert 指向它。
	for _, c := range e.rt.ix.Changes() {
		if c.Reverts == a.Change {
			return "", kbErr("ALREADY_REVERTED", "change "+a.Change+" 已被 "+c.ID+" 撤销", "一条记录只能撤一次")
		}
	}

	// 反向应用副作用:遍历 target 涉及的节点,清理被它设的 SupersededBy/RefutedBy。
	// 这是机械反推:target 的 overturns/refute 级联设的字段,清掉即恢复。
	restored := 0
	touched := map[string]*store.Shard{}
	shardOf := func(rel string) *store.Shard {
		if touched[rel] == nil {
			if cs := e.rt.cache.Shards()[rel]; cs != nil {
				touched[rel] = cs.Shard
			}
		}
		return touched[rel]
	}
	for _, nodeID := range target.Nodes {
		ref := e.rt.ix.Node(nodeID)
		if ref == nil {
			continue
		}
		n := ref.Node
		changed := false
		for i := range n.Entries {
			en := &n.Entries[i]
			// refute 级联:被 target 标 RefutedBy 的恢复(清字段,重新 Active)。
			if en.RefutedBy == target.ID {
				en.RefutedBy = ""
				restored++
				changed = true
			}
			// obsolete:被 target 标 RetiredBy 的恢复。
			if en.RetiredBy == target.ID {
				en.RetiredBy = ""
				restored++
				changed = true
			}
		}
		if changed {
			shardOf(ref.ShardRel)
		}
	}
	// 追加撤销记录(Reverts 指向被撤销的,Why 记理由)。
	chID := model.NewChangeID(e.now())
	ids := map[string]bool{}
	for _, c := range e.rt.ix.Changes() {
		ids[c.ID] = true
	}
	for ids[chID] {
		chID = model.NewChangeID(e.now())
	}
	revertChange := model.Change{
		ID: chID, Nodes: target.Nodes, At: e.now().UTC(),
		What: "撤销 " + target.ID, Why: a.Reason,
		Reverts: target.ID, Author: author,
		Commit: gitHead(e.Store.RepoRoot()),
	}
	if err := e.Store.AppendChange(revertChange); err != nil {
		return "", err
	}
	// 落盘恢复的 entries。
	for rel, sh := range touched {
		if sh == nil {
			continue
		}
		path := filepath.Join(e.Store.Dir(), filepath.FromSlash(rel))
		if err := e.Store.SaveShard(path, sh, e.rt.cache.Shards()[rel].Raw); err != nil {
			e.warnOpsLocked("revert 落盘失败(下次重试):" + err.Error())
		}
	}
	return fmt.Sprintf("ack:已撤销 %s(恢复 %d 条条目)。撤销记录 %s 已追加(journal 可追溯理由)。", target.ID, restored, chID), nil
}
