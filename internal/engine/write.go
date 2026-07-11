package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

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

	rememberPlan, warns, err := e.planRememberNodeLocked(a.Node)
	if err != nil {
		return "", err
	}
	ref := rememberPlan.ref
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
		// 轮30-A 方案防撞:新方案 vs 历史 rejected(bigram>0.8 命中)。
		// 分级:带了 disputes(已主动声明矛盾关系)→ 温和提醒;没带 → 强警告。
		// 不阻断写入(知识导航源码拍板,系统提醒 AI+人判断)。
		hasDisputes := len(in.Disputes) > 0
		for _, c := range e.rt.ix.Changes() {
			for _, rj := range c.Rejected {
				if BigramJaccard(rj.Option, in.Text) > 0.8 {
					if hasDisputes {
						warnList = append(warnList,
							"注意:此方案与历史否决方案相似(change "+c.ID[:min(12, len(c.ID))]+" 否决过「"+shortText(rj.Option, 40)+"」,理由:"+shortText(rj.Reason, 40)+")。你已声明 disputes,请确保确实不同")
					} else {
						warnList = append(warnList,
							"⚠ 此方案曾被否决!change "+c.ID[:min(12, len(c.ID))]+" 否决过「"+shortText(rj.Option, 40)+"」,理由:"+shortText(rj.Reason, 40)+"。若确信这次不同:用 disputes 字段声明与历史的关系,或带 based_on 原文证据")
					}
					break // 一个 change 只报一次
				}
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
	if err := e.commitRememberNodeLocked(rememberPlan); err != nil {
		// SaveShard 可能在 rename 已完成、目录 fsync 才报错；无论磁盘最终是
		// before 还是 after，都重载以免本次工作副本污染后续请求。
		reloadErr := e.reloadLocked()
		return "", errors.Join(err, reloadErr)
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
	src, err := safeRepoRead(e.Store.RepoRoot(), file)
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
				Hash: syms[i].Hash, StructHash: syms[i].StructHash, DocStructHash: syms[i].DocStructHash, Lines: syms[i].Lines,
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
	src, err := safeRepoRead(e.Store.RepoRoot(), file)
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
	node := e.nodeFromSymbol(file, *target)
	// 索引会隔离跨分片重复 ID。隔离态不能被当成“新符号”重新创建或覆盖，
	// 否则一次 remember/record_change 会悄悄改写其中一个副本。
	for _, cs := range e.rt.cache.Shards() {
		if cs != nil && shardHasNode(cs.Shard, node.ID) {
			return nil, kbErr("SHARD_CONFLICT", "节点 "+node.ID+" 被重复分片隔离",
				"先修复 kb_status 报出的重复 Node ID，再重试")
		}
	}
	plan := &newSymbolPlan{node: node, shardRel: "tree/" + file + ".yaml"}
	if cs := e.rt.cache.Shards()[plan.shardRel]; cs == nil || cs.Shard == nil {
		plan.fileNode = &model.Node{
			ID: file, Level: model.LevelFile,
			Anchor: model.Anchor{File: file, Hash: parser.HashFileFor(p, syms, src)},
			Status: model.StatusUndigested, Since: e.now().UTC(),
		}
	}
	return plan, nil
}

type rememberNodePlan struct {
	ref        *index.NodeRef
	fileEnsure *model.Node
}

// planRememberNodeLocked 只构造独立工作副本，不改缓存、不落盘。这样新符号的
// 增量落锚也要等 entry/keyword/supersedes 全部校验通过后才与知识一起提交。
func (e *Engine) planRememberNodeLocked(query string) (*rememberNodePlan, []string, error) {
	id, cands := e.resolveQueryLocked(query)
	if id != "" {
		original := e.rt.ix.Node(id)
		return &rememberNodePlan{ref: &index.NodeRef{
			ShardRel: original.ShardRel,
			Node:     cloneModelNode(original.Node),
		}}, nil, nil
	}
	if len(cands) > 1 {
		return nil, nil, kbErr("NODE_NOT_FOUND",
			"符号 "+query+" 有多个候选:"+strings.Join(cands, "、"), "用完整节点 ID 重试")
	}
	plan, err := e.planNewSymbolLocked(query)
	if err != nil {
		return nil, nil, err
	}
	return &rememberNodePlan{
		ref:        &index.NodeRef{ShardRel: plan.shardRel, Node: cloneModelNode(&plan.node)},
		fileEnsure: cloneModelNode(plan.fileNode),
	}, []string{"节点 " + plan.node.ID + " 为新符号增量落锚自动创建"}, nil
}

func (e *Engine) commitRememberNodeLocked(plan *rememberNodePlan) error {
	if plan == nil || plan.ref == nil || plan.ref.Node == nil {
		return fmt.Errorf("engine: remember 提交计划为空")
	}
	rel := plan.ref.ShardRel
	cs := e.rt.cache.Shards()[rel]
	var sh *store.Shard
	var raw *yaml.Node
	if cs != nil && cs.Shard != nil {
		sh = cloneEngineShard(cs.Shard)
		raw = cs.Raw
	} else {
		sh = &store.Shard{Schema: model.SchemaVersion}
	}
	if plan.fileEnsure != nil && !shardHasNode(sh, plan.fileEnsure.ID) {
		sh.Nodes = append(sh.Nodes, *cloneModelNode(plan.fileEnsure))
	}
	if existing := nodeInShard(sh, plan.ref.Node.ID); existing != nil {
		*existing = *cloneModelNode(plan.ref.Node)
	} else {
		sh.Nodes = append(sh.Nodes, *cloneModelNode(plan.ref.Node))
	}
	return e.Store.SaveShard(filepath.Join(e.Store.Dir(), filepath.FromSlash(rel)), sh, raw)
}

func (e *Engine) nodeFromSymbol(file string, sym parser.Symbol) model.Node {
	level := model.LevelFunction
	if sym.Kind == "type" || sym.Kind == "var" || sym.Kind == "const" {
		level = model.LevelDecl
	}
	return model.Node{
		ID: model.SymbolNodeID(file, sym.Name), Level: level,
		Anchor: model.Anchor{File: file, Symbol: sym.Name,
			Hash: sym.Hash, StructHash: sym.StructHash, DocStructHash: sym.DocStructHash, Lines: sym.Lines},
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
	// adoptOrphan 仅供 kb_adopt claim 复用 RecordChange 的事务管线；外部协议
	// 不暴露。锁内再次确认源仍为 orphaned，消除预检与提交之间的竞态。
	adoptOrphan string
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
	if a.adoptOrphan != "" {
		ref := e.rt.ix.Node(a.adoptOrphan)
		if ref == nil || ref.Node.Status != model.StatusOrphaned {
			return "", kbErr("NODE_NOT_FOUND", a.adoptOrphan+" 不是可认领的 orphaned 节点",
				"用 kb_status 核对孤儿列表")
		}
	}

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
		if e.changeIDConflictedLocked(a.Overturns) {
			return "", kbErr("JOURNAL_CONFLICT", "change "+a.Overturns+" 存在同 ID 异内容冲突",
				"先人工裁决 journal 冲突，不能选择随机副本 overturn")
		}
		target := e.rt.ix.ChangeByID(a.Overturns)
		if target == nil {
			return "", kbErr("OVERTURNS_NOT_FOUND", "被推翻的记录 "+a.Overturns+" 不存在",
				"用 kb_recall(mode=history) 核对记录 ID")
		}
		if target.EffectsVersion > 1 {
			return "", kbErr("SCHEMA_TOO_NEW", "change "+a.Overturns+" 的 effects_version 高于当前引擎",
				"升级 iknowledge 后再 overturn")
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

	// remap 也是本次 record_change 的校验面，必须在 journal 和任何分片写入
	// 前完成全量规划。目标可以是本次 nodes 刚规划、尚未落盘的新符号。
	plannedNodes := map[string]*model.Node{}
	for i := range actions {
		if actions[i].kind == "create" && actions[i].newNode != nil {
			plannedNodes[actions[i].newNode.ID] = actions[i].newNode
		}
	}
	remapPlan, err := e.planRemapsLocked(a.Remaps, plannedNodes)
	if err != nil {
		return "", err
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

	// 先为所有可能改动的分片和节点取 before 快照。RecordChange 的 journal
	// 改为提交标记：分片全部成功后才 append；任一 Save/reload/append 失败均按
	// 原始字节回滚。NodeEffects 同时让后续 kb_revert 可逆转重锚/create/remap。
	affectedNodes := map[string]bool{}
	touchedRels := map[string]bool{}
	plannedShard := map[string]string{}
	for _, act := range actions {
		affectedNodes[act.id] = true
		touchedRels[act.shardRel] = true
		plannedShard[act.id] = act.shardRel
		if act.fileEnsure != nil {
			affectedNodes[act.fileEnsure.ID] = true
			plannedShard[act.fileEnsure.ID] = act.shardRel
		}
	}
	if remapPlan != nil {
		for id := range remapPlan.removeSet {
			affectedNodes[id] = true
			if ref := e.rt.ix.Node(id); ref != nil {
				touchedRels[ref.ShardRel] = true
			}
		}
		for id := range remapPlan.edits {
			affectedNodes[id] = true
			if ref := e.rt.ix.Node(id); ref != nil {
				touchedRels[ref.ShardRel] = true
			} else if rel := plannedShard[id]; rel != "" {
				touchedRels[rel] = true
			}
		}
	}
	beforeNodes := map[string]*model.Node{}
	beforeShards := map[string]string{}
	beforeRawNodes := map[string]string{}
	for id := range affectedNodes {
		if ref := e.rt.ix.Node(id); ref != nil {
			beforeNodes[id] = cloneModelNode(ref.Node)
			beforeShards[id] = ref.ShardRel
			beforeRawNodes[id] = e.rawNodeYAMLLocked(id)
		}
	}
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
		Commit: gitHead(e.Store.RepoRoot()), EffectsVersion: 1,
	}
	// prepared WAL 必须在首个分片写之前包含全部目标，包括最后才追加的月
	// journal；否则进程在 append 后、commit marker 前退出会留下幽灵记录。
	touchedRels[e.Store.JournalRelFor(change)] = true
	tx, err := e.prepareTruthTransactionLocked(touchedRels)
	if err != nil {
		return "", err
	}
	defer e.guardTruthTransactionPanicLocked(tx)
	rollback := func(cause error) (string, error) {
		return "", e.rollbackTruthTransactionLocked(tx, cause)
	}

	// 应用节点动作到分片克隆，不提前污染当前索引快照。
	touched := map[string]*store.Shard{} // shardRel → 待存分片(按分片合并写)
	shardOf := func(rel string) *store.Shard {
		if touched[rel] == nil {
			if cs := e.rt.cache.Shards()[rel]; cs != nil {
				touched[rel] = cloneEngineShard(cs.Shard)
			}
		}
		return touched[rel]
	}
	for _, act := range actions {
		sh := shardOf(act.shardRel)
		switch act.kind {
		case "reanchor":
			if n := nodeInShard(sh, act.id); n != nil {
				n.Anchor = act.anchor
				n.PendingAnchor = false
				if n.Status == model.StatusSuspect {
					n.Status = model.StatusFresh
				}
			}
		case "orphan":
			if n := nodeInShard(sh, act.id); n != nil {
				n.Status = model.StatusOrphaned
			}
		case "pending":
			if n := nodeInShard(sh, act.id); n != nil {
				n.PendingAnchor = true
			}
		case "create":
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
	touchedOrder := make([]string, 0, len(touched))
	for rel := range touched {
		touchedOrder = append(touchedOrder, rel)
	}
	sort.Strings(touchedOrder)
	for _, rel := range touchedOrder {
		var err error
		if cs := e.rt.cache.Shards()[rel]; cs != nil {
			err = e.Store.SaveShard(filepath.Join(e.Store.Dir(), filepath.FromSlash(rel)), touched[rel], cs.Raw)
		} else {
			err = e.Store.SaveShard(filepath.Join(e.Store.Dir(), filepath.FromSlash(rel)), touched[rel], nil)
		}
		if err != nil {
			return rollback(fmt.Errorf("record_change 保存 %s: %w", rel, err))
		}
	}
	if err := e.reloadLocked(); err != nil {
		return rollback(err)
	}

	// 此处只执行上面已经全量校验过的 remap 计划；剩余错误只可能来自 IO。
	if remapPlan != nil {
		if err := e.applyRemapPlanLocked(remapPlan); err != nil {
			return rollback(err)
		}
		warns = append(warns, "remaps 已迁移:条目统一降半级待确认(verified→inferred、inferred→suspect)")
	}
	if err := e.reloadLocked(); err != nil {
		return rollback(err)
	}

	affectedIDs := make([]string, 0, len(affectedNodes))
	for id := range affectedNodes {
		affectedIDs = append(affectedIDs, id)
	}
	sort.Strings(affectedIDs)
	for _, id := range affectedIDs {
		var after *model.Node
		afterShard := ""
		afterRaw := ""
		if ref := e.rt.ix.Node(id); ref != nil {
			after = cloneModelNode(ref.Node)
			afterShard = ref.ShardRel
			afterRaw = e.rawNodeYAMLLocked(id)
		}
		before := beforeNodes[id]
		if reflect.DeepEqual(before, after) && beforeShards[id] == afterShard {
			continue
		}
		change.NodeEffects = append(change.NodeEffects, model.NodeEffect{
			Node: id, BeforeShard: beforeShards[id], AfterShard: afterShard,
			Before: before, After: after, BeforeRaw: beforeRawNodes[id], AfterRaw: afterRaw,
		})
	}
	uniqueNodes := make([]string, 0, len(change.Nodes)+len(change.NodeEffects))
	for _, id := range change.Nodes {
		uniqueNodes = appendUnique(uniqueNodes, id)
	}
	for _, effect := range change.NodeEffects {
		uniqueNodes = appendUnique(uniqueNodes, effect.Node)
	}
	change.Nodes = uniqueNodes
	if err := e.Store.AppendChange(change); err != nil {
		committed := false
		if changes, _, loadErr := e.Store.LoadJournal(); loadErr == nil {
			for _, c := range changes {
				if c.ID == change.ID {
					committed = true
					break
				}
			}
		}
		if !committed {
			return rollback(fmt.Errorf("record_change 追加 journal: %w", err))
		}
	}
	committed, commitErr := e.commitTruthTransactionLocked(tx)
	if !committed {
		return rollback(fmt.Errorf("record_change 写 committed marker: %w", commitErr))
	}
	if commitErr != nil {
		return "", fmt.Errorf("record_change 已提交但 WAL 清理/重载失败(不要重试同一变更): %w", commitErr)
	}
	e.markDigested(sid, resolved...)

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
	// 轮31 批次2 变更影响面分析:对本次改动的主节点,用 callgraph 查谁调它,
	// 标注带活跃知识的调用方(它们的契约/pitfall 可能被本次改动破坏)。
	// 这是"智能协作"的关键:AI 改一个函数,系统立刻告诉它波及面 + 哪里要复核。
	if cg := e.ensureCallGraphLocked(); cg != nil {
		impactShown := 0
		for _, id := range resolved {
			if impactShown >= 3 {
				break // 限额:最多报 3 个节点的影响面(防长清单刷屏)
			}
			callers := cg.calledByOf(id)
			if len(callers) == 0 {
				continue
			}
			// 只列出带活跃知识的调用方:这些契约/pitfall 可能被本次改动破坏。
			var withKnowledge []string
			for _, caller := range callers {
				if ref := e.rt.ix.Node(caller); ref != nil && hasActiveEntries(ref.Node) {
					withKnowledge = append(withKnowledge, caller)
				}
			}
			if len(withKnowledge) == 0 {
				continue // 调用方都没知识,不值得报(纯机械依赖,callgraph 已在 recall 展示)
			}
			fmt.Fprintf(&b, "\n变更影响:你改了 %s,它被 %d 处调用", id, len(callers))
			if len(withKnowledge) > 0 {
				fmt.Fprintf(&b, ",其中 %d 处带知识(契约/坑可能被破坏,建议复核):\n", len(withKnowledge))
				for _, c := range withKnowledge {
					fmt.Fprintf(&b, "  ⚠ %s —— kb_recall %s 看它的契约是否仍成立\n", c, c)
				}
			}
			impactShown++
		}
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

func nodeInShard(sh *store.Shard, id string) *model.Node {
	if sh == nil {
		return nil
	}
	for i := range sh.Nodes {
		if sh.Nodes[i].ID == id {
			return &sh.Nodes[i]
		}
	}
	return nil
}

func cloneYAMLNode(in *yaml.Node) *yaml.Node {
	if in == nil {
		return nil
	}
	out := *in
	out.Content = make([]*yaml.Node, len(in.Content))
	for i := range in.Content {
		out.Content[i] = cloneYAMLNode(in.Content[i])
	}
	return &out
}

func yamlMappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.DocumentNode && len(node.Content) == 1 {
		node = node.Content[0]
	}
	if node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func yamlObjectByID(seq *yaml.Node, id string) *yaml.Node {
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return nil
	}
	for _, item := range seq.Content {
		if idNode := yamlMappingValue(item, "id"); idNode != nil && idNode.Value == id {
			return item
		}
	}
	return nil
}

func (e *Engine) rawEntryNodeLocked(nodeID, entryID string) *yaml.Node {
	ref := e.rt.ix.Node(nodeID)
	if ref == nil {
		return nil
	}
	cs := e.rt.cache.Shards()[ref.ShardRel]
	if cs == nil || cs.Raw == nil {
		return nil
	}
	node := yamlObjectByID(yamlMappingValue(cs.Raw, "nodes"), nodeID)
	entry := yamlObjectByID(yamlMappingValue(node, "entries"), entryID)
	return cloneYAMLNode(entry)
}

func (e *Engine) rawNodeYAMLLocked(nodeID string) string {
	ref := e.rt.ix.Node(nodeID)
	if ref == nil {
		return ""
	}
	cs := e.rt.cache.Shards()[ref.ShardRel]
	if cs == nil || cs.Raw == nil {
		return ""
	}
	node := yamlObjectByID(yamlMappingValue(cs.Raw, "nodes"), nodeID)
	if node == nil {
		return ""
	}
	data, err := yaml.Marshal(node)
	if err != nil {
		return ""
	}
	return string(data)
}

func injectRawEntry(raw *yaml.Node, nodeID, entryID string, entry *yaml.Node) {
	if raw == nil || entry == nil {
		return
	}
	node := yamlObjectByID(yamlMappingValue(raw, "nodes"), nodeID)
	if node == nil {
		return
	}
	entries := yamlMappingValue(node, "entries")
	if entries == nil {
		entries = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		node.Content = append(node.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "entries"}, entries)
	}
	if yamlObjectByID(entries, entryID) == nil {
		entries.Content = append(entries.Content, cloneYAMLNode(entry))
	}
}

func injectRawNode(raw *yaml.Node, rawNode string) {
	if raw == nil || strings.TrimSpace(rawNode) == "" {
		return
	}
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(rawNode), &node); err != nil {
		return
	}
	n := &node
	if n.Kind == yaml.DocumentNode && len(n.Content) == 1 {
		n = n.Content[0]
	}
	idNode := yamlMappingValue(n, "id")
	nodes := yamlMappingValue(raw, "nodes")
	if idNode == nil || nodes == nil || nodes.Kind != yaml.SequenceNode || yamlObjectByID(nodes, idNode.Value) != nil {
		return
	}
	nodes.Content = append(nodes.Content, cloneYAMLNode(n))
}

type knowledgeFileSnapshot struct {
	exists bool
	data   []byte
}

func (e *Engine) snapshotKnowledgeFiles(rels map[string]bool) (map[string]knowledgeFileSnapshot, error) {
	out := make(map[string]knowledgeFileSnapshot, len(rels))
	for rel := range rels {
		data, err := e.Store.ReadKnowledgeFile(rel)
		switch {
		case err == nil:
			out[rel] = knowledgeFileSnapshot{exists: true, data: data}
		case os.IsNotExist(err):
			out[rel] = knowledgeFileSnapshot{}
		default:
			return nil, fmt.Errorf("读取事务快照 %s: %w", rel, err)
		}
	}
	return out, nil
}

func (e *Engine) rollbackKnowledgeSnapshots(snapshots map[string]knowledgeFileSnapshot) error {
	rels := make([]string, 0, len(snapshots))
	for rel := range snapshots {
		rels = append(rels, rel)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(rels)))
	var errs []error
	for _, rel := range rels {
		snapshot := snapshots[rel]
		if snapshot.exists {
			if err := e.Store.WriteKnowledgeFile(rel, snapshot.data); err != nil {
				errs = append(errs, fmt.Errorf("回滚 %s: %w", rel, err))
			}
			continue
		}
		if err := e.Store.RemoveKnowledgeFile(rel); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("删除事务中新文件 %s: %w", rel, err))
		}
	}
	return errors.Join(errs...)
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

type remapEdit struct {
	addEntries []model.Entry
	addLineage []string
}

type remapApplyPlan struct {
	edits     map[string]*remapEdit
	removeSet map[string]bool
	rawMoves  []remapRawMove
}

type remapRawMove struct {
	targetNode string
	entryID    string
	rawEntry   *yaml.Node
}

// planRemapsLocked 把 remaps 解析为完整、确定的执行计划，但不修改内存或磁盘。
// plannedNodes 是同一次 RecordChange 即将创建的节点，普通调用传 nil。
func (e *Engine) planRemapsLocked(remaps []model.Remap, plannedNodes map[string]*model.Node) (*remapApplyPlan, error) {
	if len(remaps) == 0 {
		return nil, nil
	}
	type resolvedRemap struct {
		fromID   string
		fromNode *model.Node
		to       []string
		toByName map[string]string
		raw      model.Remap
	}
	resolveTarget := func(q string) (string, []string) {
		if plannedNodes[q] != nil {
			return q, nil
		}
		if id, cands := e.resolveQueryLocked(q); id != "" || len(cands) > 0 {
			return id, cands
		}
		qFile, qSym := model.SplitNodeID(q)
		var hits []string
		for id := range plannedNodes {
			file, sym := model.SplitNodeID(id)
			if qSym != "" && (qFile == "" || qFile == file) && model.LooseSymbolMatch(qSym, sym) {
				hits = append(hits, id)
			}
		}
		sort.Strings(hits)
		if len(hits) == 1 {
			return hits[0], nil
		}
		return "", hits
	}

	resolved := make([]resolvedRemap, 0, len(remaps))
	sources := map[string]bool{}
	for _, rm := range remaps {
		fromID := e.rt.ix.ResolveNodeID(rm.From)
		fromRef := e.rt.ix.Node(fromID)
		if fromID == "" || fromRef == nil {
			return nil, kbErr("NODE_NOT_FOUND", "remaps.from "+rm.From+" 不存在", "核对节点 ID")
		}
		if sources[fromID] {
			return nil, kbErr("INVALID_ARGUMENT", "remaps.from "+rm.From+" 重复申报", "每个源节点只能映射一次")
		}
		sources[fromID] = true
		if len(rm.To) == 0 {
			return nil, kbErr("INVALID_ARGUMENT", "remaps.to 为空", "至少给一个目标节点")
		}
		rr := resolvedRemap{fromID: fromID, fromNode: fromRef.Node, raw: rm, toByName: map[string]string{}}
		seenTo := map[string]bool{}
		for _, to := range rm.To {
			toID, cands := resolveTarget(to)
			if toID == "" {
				if len(cands) > 1 {
					return nil, kbErr("NODE_NOT_FOUND", "remaps.to "+to+" 有多个候选", "用完整节点 ID")
				}
				return nil, kbErr("NODE_NOT_FOUND", "remaps.to "+to+" 不存在",
					"目标符号须已在代码中或列进本次 nodes 完成增量落锚")
			}
			if toID == fromID {
				return nil, kbErr("NODE_NOT_FOUND", "remaps.from 与 to 解析到同一节点 "+toID+"(疑似把目标的血缘旧 ID 当 from)",
					"from 应是被拆分/合并前的源节点,不能等于任一目标")
			}
			if seenTo[toID] {
				return nil, kbErr("INVALID_ARGUMENT", "remaps.to 重复指向 "+toID, "目标节点去重后重试")
			}
			seenTo[toID] = true
			rr.to = append(rr.to, toID)
			rr.toByName[to] = toID
			rr.toByName[toID] = toID
		}
		resolved = append(resolved, rr)
	}
	for _, rr := range resolved {
		for _, toID := range rr.to {
			if sources[toID] {
				return nil, kbErr("INVALID_ARGUMENT", "节点 "+toID+" 同时作为 remap 源和目标", "拆成两次明确的迁移")
			}
		}
	}

	plan := &remapApplyPlan{edits: map[string]*remapEdit{}, removeSet: map[string]bool{}}
	editOf := func(id string) *remapEdit {
		if plan.edits[id] == nil {
			plan.edits[id] = &remapEdit{}
		}
		return plan.edits[id]
	}
	entryIDs := map[string]map[string]bool{}
	for _, rr := range resolved {
		for _, toID := range rr.to {
			if entryIDs[toID] != nil {
				continue
			}
			entryIDs[toID] = map[string]bool{}
			n := plannedNodes[toID]
			if n == nil {
				if ref := e.rt.ix.Node(toID); ref != nil {
					n = ref.Node
				}
			}
			if n != nil {
				for i := range n.Entries {
					entryIDs[toID][n.Entries[i].ID] = true
				}
			}
		}
	}
	for _, rr := range resolved {
		knownEntries := map[string]bool{}
		for i := range rr.fromNode.Entries {
			knownEntries[rr.fromNode.Entries[i].ID] = true
		}
		for entryID, dst := range rr.raw.Entries {
			if !knownEntries[entryID] {
				return nil, kbErr("INVALID_ARGUMENT", "remaps.entries 引用了源节点中不存在的条目 "+entryID, "核对 entry ID")
			}
			if rr.toByName[dst] == "" {
				return nil, kbErr("NODE_NOT_FOUND", "remaps.entries 目标 "+dst+" 不在 to 列表", "核对映射")
			}
		}
		lineage := appendUnique(append([]string{}, rr.fromNode.Lineage...), rr.fromID)
		for i := range rr.fromNode.Entries {
			en := rr.fromNode.Entries[i]
			en.Confidence = demote(en.Confidence)
			dstID := rr.to[0]
			if dst, ok := rr.raw.Entries[en.ID]; ok {
				dstID = rr.toByName[dst]
			}
			if entryIDs[dstID][en.ID] {
				return nil, kbErr("INVALID_ARGUMENT", "remap 后目标 "+dstID+" 出现重复条目 ID "+en.ID, "先合并/清理冲突条目")
			}
			entryIDs[dstID][en.ID] = true
			editOf(dstID).addEntries = append(editOf(dstID).addEntries, en)
			if raw := e.rawEntryNodeLocked(rr.fromID, en.ID); raw != nil {
				plan.rawMoves = append(plan.rawMoves, remapRawMove{targetNode: dstID, entryID: en.ID, rawEntry: raw})
			}
		}
		for _, toID := range rr.to {
			for _, old := range lineage {
				editOf(toID).addLineage = appendUnique(editOf(toID).addLineage, old)
			}
		}
		plan.removeSet[rr.fromID] = true
	}
	return plan, nil
}

// applyRemapsLocked 供 adopt 等已有调用使用：先纯规划，再执行。
func (e *Engine) applyRemapsLocked(remaps []model.Remap) error {
	plan, err := e.planRemapsLocked(remaps, nil)
	if err != nil || plan == nil {
		return err
	}
	return e.applyRemapPlanLocked(plan)
}

func (e *Engine) applyRemapPlanLocked(plan *remapApplyPlan) error {
	dirty := map[string]bool{}
	rawOverrides := map[string]*yaml.Node{}
	for id, ed := range plan.edits {
		ref := e.rt.ix.Node(id)
		if ref == nil {
			return kbErr("NODE_NOT_FOUND", "目标节点 "+id+" 迁移中消失", "重试")
		}
		ref.Node.Entries = append(ref.Node.Entries, ed.addEntries...)
		for _, old := range ed.addLineage {
			ref.Node.Lineage = appendUnique(ref.Node.Lineage, old)
		}
		if ref.Node.Status == model.StatusUndigested && hasActiveEntries(ref.Node) {
			ref.Node.Status = model.StatusFresh
		}
		dirty[ref.ShardRel] = true
	}
	for id := range plan.removeSet {
		if ref := e.rt.ix.Node(id); ref != nil {
			dirty[ref.ShardRel] = true
		}
	}
	for _, move := range plan.rawMoves {
		ref := e.rt.ix.Node(move.targetNode)
		if ref == nil || move.rawEntry == nil {
			continue
		}
		if rawOverrides[ref.ShardRel] == nil {
			if cs := e.rt.cache.Shards()[ref.ShardRel]; cs != nil {
				rawOverrides[ref.ShardRel] = cloneYAMLNode(cs.Raw)
			}
		}
		injectRawEntry(rawOverrides[ref.ShardRel], move.targetNode, move.entryID, move.rawEntry)
	}
	return e.rewriteShardsLockedWithRaw(dirty, plan.removeSet, rawOverrides)
}

// rewriteShardsLocked 把 dirty 分片按现存索引节点 + 删除集重写落盘(整分片一次)。
func (e *Engine) rewriteShardsLocked(dirty map[string]bool, removeSet map[string]bool) error {
	return e.rewriteShardsLockedWithRaw(dirty, removeSet, nil)
}

func (e *Engine) rewriteShardsLockedWithRaw(dirty map[string]bool, removeSet map[string]bool, rawOverrides map[string]*yaml.Node) error {
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
		raw := cs.Raw
		if rawOverrides[rel] != nil {
			raw = rawOverrides[rel]
		}
		if err := e.Store.SaveShard(path, cs.Shard, raw); err != nil {
			return err
		}
	}
	return nil
}

// shortText 截断文本到 maxRunes 个 rune(防切坏 UTF-8),超长加省略号。轮30-A 防撞提醒用。
func shortText(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "…"
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
	data, err := safeRepoRead(repo, ".git/HEAD")
	if err != nil {
		return ""
	}
	head := strings.TrimSpace(string(data))
	if refPath, ok := strings.CutPrefix(head, "ref: "); ok {
		if data, err := safeRepoRead(repo, ".git/"+filepath.ToSlash(refPath)); err == nil {
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

func (e *Engine) changeIDConflictedLocked(id string) bool {
	if e.rt.cache == nil {
		return false
	}
	_, stats := e.rt.cache.Journal()
	for _, conflicted := range stats.ConflictIDs {
		if conflicted == id {
			return true
		}
	}
	return false
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
	if e.changeIDConflictedLocked(a.Change) {
		return "", kbErr("JOURNAL_CONFLICT", "change "+a.Change+" 存在同 ID 异内容冲突",
			"先人工裁决 journal 冲突，不能选择随机副本 revert")
	}
	target := e.rt.ix.ChangeByID(a.Change)
	if target == nil {
		return "", kbErr("NODE_NOT_FOUND", "change "+a.Change+" 不存在", "kb_recall mode=history 确认 ID")
	}
	if target.EffectsVersion > 1 {
		return "", kbErr("SCHEMA_TOO_NEW", "change "+a.Change+" 的 effects_version 高于当前引擎",
			"升级 iknowledge 后再 revert")
	}
	// 已有 revert 通常表示操作完成；但旧实现 journal 先行且吞 SaveShard 错误，
	// 可能留下“已撤销”记录却没恢复分片。下面会按 before/after 状态判定：完整
	// 恢复才报 ALREADY_REVERTED，半应用则补完而不再追加第二条 journal。
	var existingRevert *model.Change
	for _, c := range e.rt.ix.Changes() {
		if c.Reverts == a.Change {
			cc := c
			existingRevert = &cc
			break
		}
	}

	effects := append([]model.EntryEffect(nil), target.Effects...)
	nodeEffects := append([]model.NodeEffect(nil), target.NodeEffects...)
	legacy := target.EffectsVersion == 0 && len(effects) == 0 && len(nodeEffects) == 0
	if legacy {
		effects = e.legacyEffectsLocked(target)
		if len(effects) == 0 {
			return "", kbErr("REVERT_UNPROVABLE", "旧 change "+target.ID+" 缺少结构化 effects，无法证明安全逆操作",
				"人工检查 journal/git 后用新 change 明确修正；不要猜测旧状态")
		}
	}

	// 先验证全部 effect，再在分片克隆上反向应用。当前状态只允许等于 target.After
	//（待恢复）或 target.Before（上次崩溃已恢复）；任何第三种状态说明有后续编辑，
	// 必须拒绝，不能把新事实覆盖掉。
	touched := map[string]*store.Shard{}
	rawOverrides := map[string]*yaml.Node{}
	originals := map[string][]byte{}
	needRestore, alreadyRestored := 0, 0
	seenEffects := map[string]bool{}

	// record_change 的节点级副作用（重锚/create/remap）走 NodeEffects。
	// 先对当前最终态做严格比较，再在分片克隆上删 After、放回 Before。
	for _, effect := range nodeEffects {
		if effect.Node == "" || seenEffects["node:"+effect.Node] {
			return "", kbErr("REVERT_CONFLICT", "change "+target.ID+" 的 node_effects 损坏或重复:"+effect.Node, "人工检查 journal")
		}
		seenEffects["node:"+effect.Node] = true
		current, currentRel, duplicate := e.findCachedNode(effect.Node)
		if duplicate {
			return "", kbErr("REVERT_CONFLICT", "Node ID "+effect.Node+" 在多个分片重复", "先修复重复节点")
		}
		matchesAfter := reflect.DeepEqual(current, effect.After) && currentRel == effect.AfterShard
		matchesBefore := reflect.DeepEqual(current, effect.Before) && currentRel == effect.BeforeShard
		if effect.After == nil {
			matchesAfter = current == nil
		}
		if effect.Before == nil {
			matchesBefore = current == nil
		}
		switch {
		case matchesAfter:
			needRestore++
		case matchesBefore:
			alreadyRestored++
		default:
			return "", kbErr("REVERT_CONFLICT", "节点 "+effect.Node+" 已被后续 change 修改", "先撤销后续变更；当前节点不再等于目标 change 的 after")
		}
		for _, rel := range []string{effect.AfterShard, effect.BeforeShard} {
			if rel == "" || touched[rel] != nil {
				continue
			}
			cs := e.rt.cache.Shards()[rel]
			if cs == nil || cs.Shard == nil {
				return "", kbErr("REVERT_CONFLICT", "node effect 分片不可用:"+rel, "先修复分片冲突")
			}
			touched[rel] = cloneEngineShard(cs.Shard)
			rawOverrides[rel] = cloneYAMLNode(cs.Raw)
			data, err := e.Store.ReadKnowledgeFile(rel)
			if err != nil {
				return "", err
			}
			originals[rel] = data
		}
	}
	for _, effect := range nodeEffects {
		if effect.After != nil {
			sh := touched[effect.AfterShard]
			kept := make([]model.Node, 0, len(sh.Nodes))
			for i := range sh.Nodes {
				if sh.Nodes[i].ID != effect.Node {
					kept = append(kept, sh.Nodes[i])
				}
			}
			sh.Nodes = kept
		}
		if effect.Before != nil {
			sh := touched[effect.BeforeShard]
			if n := nodeInShard(sh, effect.Node); n != nil {
				*n = *cloneModelNode(effect.Before)
			} else {
				sh.Nodes = append(sh.Nodes, *cloneModelNode(effect.Before))
			}
			injectRawNode(rawOverrides[effect.BeforeShard], effect.BeforeRaw)
		}
	}
	for _, effect := range effects {
		if effect.Entry == "" || seenEffects[effect.Entry] {
			return "", kbErr("REVERT_CONFLICT", "change "+target.ID+" 的 effects 损坏或重复:"+effect.Entry, "人工检查 journal")
		}
		seenEffects[effect.Entry] = true
		i := strings.LastIndexByte(effect.Entry, '#')
		if i <= 0 || i == len(effect.Entry)-1 {
			return "", kbErr("REVERT_CONFLICT", "非法 effect entry "+effect.Entry, "人工检查 journal")
		}
		nodeID, entryID := effect.Entry[:i], effect.Entry[i+1:]
		ref := e.rt.ix.Node(nodeID)
		if ref == nil {
			return "", kbErr("REVERT_CONFLICT", "effect 节点已不存在:"+nodeID, "它可能已被后续 remap；先撤销后续变更")
		}
		current := e.entryByExactRefLocked(effect.Entry)
		if current == nil {
			return "", kbErr("REVERT_CONFLICT", "effect 条目已不存在:"+effect.Entry, "先撤销后续编辑")
		}
		state := entryState(current)
		switch state {
		case effect.After:
			needRestore++
		case effect.Before:
			alreadyRestored++
		default:
			if legacy && legacyStateBetween(state, effect.Before, effect.After) {
				needRestore++ // 兼容旧实现只清 marker、没恢复 confidence 的半应用
			} else {
				return "", kbErr("REVERT_CONFLICT", "条目 "+effect.Entry+" 已被后续 change 修改", "先撤销后续变更；当前状态不再等于目标 change 的 after")
			}
		}
		sh := touched[ref.ShardRel]
		if sh == nil {
			cs := e.rt.cache.Shards()[ref.ShardRel]
			if cs == nil || cs.Shard == nil {
				return "", kbErr("REVERT_CONFLICT", "effect 分片不可用:"+ref.ShardRel, "先修复分片冲突")
			}
			sh = cloneEngineShard(cs.Shard)
			touched[ref.ShardRel] = sh
			rawOverrides[ref.ShardRel] = cloneYAMLNode(cs.Raw)
			data, err := e.Store.ReadKnowledgeFile(ref.ShardRel)
			if err != nil {
				return "", err
			}
			originals[ref.ShardRel] = data
		}
		if en := entryInShard(sh, nodeID, entryID); en != nil {
			applyEntryState(en, effect.Before)
		} else {
			return "", kbErr("REVERT_CONFLICT", "克隆分片中找不到 effect 条目 "+effect.Entry, "重试")
		}
	}
	if existingRevert != nil && needRestore == 0 {
		return "", kbErr("ALREADY_REVERTED", "change "+a.Change+" 已被 "+existingRevert.ID+" 撤销", "一条记录只能撤一次")
	}
	if existingRevert != nil && !legacy {
		return "", kbErr("REVERT_CONFLICT", "change "+a.Change+" 已有结构化撤销 "+existingRevert.ID+"，当前状态后来又发生变化",
			"若是撤销的撤销，请显式 revert 后一条记录；不能沿用旧撤销记录改盘")
	}

	// 先原子替换各个分片，任一失败立即把此前成功项恢复原字节；只有分片全部
	// 成功后才追加 Reverts journal，避免形成 ALREADY_REVERTED 不可重试状态。
	rels := make([]string, 0, len(touched))
	for rel := range touched {
		rels = append(rels, rel)
	}
	sort.Strings(rels)

	// 先构造最终 journal 记录，才能在首个分片写前把目标月份也纳入 WAL。
	chID := model.NewChangeID(e.now())
	ids := map[string]bool{}
	for _, c := range e.rt.ix.Changes() {
		ids[c.ID] = true
	}
	for ids[chID] {
		chID = model.NewChangeID(e.now())
	}
	var revertChange *model.Change
	if existingRevert != nil {
		chID = existingRevert.ID
	} else {
		reverseEffects := make([]model.EntryEffect, 0, len(effects))
		for _, effect := range effects {
			reverseEffects = append(reverseEffects, model.EntryEffect{Entry: effect.Entry, Before: effect.After, After: effect.Before})
		}
		nodes := append([]string(nil), target.Nodes...)
		for _, effect := range effects {
			if i := strings.LastIndexByte(effect.Entry, '#'); i > 0 {
				nodes = appendUnique(nodes, effect.Entry[:i])
			}
		}
		reverseNodeEffects := make([]model.NodeEffect, 0, len(nodeEffects))
		for _, effect := range nodeEffects {
			reverseNodeEffects = append(reverseNodeEffects, model.NodeEffect{
				Node: effect.Node, BeforeShard: effect.AfterShard, AfterShard: effect.BeforeShard,
				Before: cloneModelNode(effect.After), After: cloneModelNode(effect.Before),
				BeforeRaw: effect.AfterRaw, AfterRaw: effect.BeforeRaw,
			})
			nodes = appendUnique(nodes, effect.Node)
		}
		change := model.Change{
			ID: chID, Nodes: nodes, At: e.now().UTC(),
			What: "撤销 " + target.ID, Why: a.Reason,
			Reverts: target.ID, EffectsVersion: 1, Effects: reverseEffects, NodeEffects: reverseNodeEffects, Author: author,
			Commit: gitHead(e.Store.RepoRoot()),
		}
		revertChange = &change
	}
	txRels := make(map[string]bool, len(rels)+1)
	for _, rel := range rels {
		txRels[rel] = true
	}
	if revertChange != nil {
		txRels[e.Store.JournalRelFor(*revertChange)] = true
	}
	tx, err := e.prepareTruthTransactionLocked(txRels)
	if err != nil {
		return "", err
	}
	defer e.guardTruthTransactionPanicLocked(tx)
	rollback := func(cause error) (string, error) {
		return "", e.rollbackTruthTransactionLocked(tx, cause)
	}

	var saved []string
	for _, rel := range rels {
		cs := e.rt.cache.Shards()[rel]
		path := filepath.Join(e.Store.Dir(), filepath.FromSlash(rel))
		saved = append(saved, rel) // atomic rename 后 dir fsync 仍可能报错，attempted 也须回滚
		raw := rawOverrides[rel]
		if raw == nil {
			raw = cs.Raw
		}
		if err := e.Store.SaveShard(path, touched[rel], raw); err != nil {
			rbErr := e.restoreShardBytes(saved, originals)
			return rollback(errors.Join(fmt.Errorf("revert 保存 %s: %w", rel, err), rbErr))
		}
	}

	// 追加撤销记录(Reverts 指向被撤销的,Effects 反向描述本次恢复)。
	if revertChange != nil {
		if err := e.Store.AppendChange(*revertChange); err != nil {
			// fsync/Close 可能在字节已追加后报错；若 journal 已能读到同一 ID，
			// 视为提交成功。否则恢复全部分片，让调用方可安全重试。
			committed := false
			if changes, _, loadErr := e.Store.LoadJournal(); loadErr == nil {
				for _, c := range changes {
					if c.ID == chID && c.Reverts == target.ID {
						committed = true
						break
					}
				}
			}
			if !committed {
				rbErr := e.restoreShardBytes(saved, originals)
				return rollback(errors.Join(fmt.Errorf("revert 追加 journal: %w", err), rbErr))
			}
		}
	}
	committed, commitErr := e.commitTruthTransactionLocked(tx)
	if !committed {
		return rollback(fmt.Errorf("revert 写 committed marker: %w", commitErr))
	}
	if commitErr != nil {
		return "", fmt.Errorf("revert 已提交但 WAL 清理/重载失败(不要重试同一 change): %w", commitErr)
	}
	compat := ""
	if legacy {
		compat = "（旧 journal 无 effects，已按保守规则恢复）"
	}
	if existingRevert != nil {
		return fmt.Sprintf("ack:已补完旧撤销 %s(恢复 %d 项状态)，沿用撤销记录 %s%s。", target.ID, needRestore, chID, compat), nil
	}
	return fmt.Sprintf("ack:已撤销 %s(恢复 %d 项状态，%d 项已处于 before)。撤销记录 %s 已追加%s。", target.ID, needRestore, alreadyRestored, chID, compat), nil
}

func legacyStateBetween(current, before, after model.EntryState) bool {
	return (current.Confidence == before.Confidence || current.Confidence == after.Confidence) &&
		(current.ConfirmedAt == before.ConfirmedAt || current.ConfirmedAt == after.ConfirmedAt) &&
		(current.RefutedBy == before.RefutedBy || current.RefutedBy == after.RefutedBy) &&
		(current.RetiredBy == before.RetiredBy || current.RetiredBy == after.RetiredBy) &&
		(current.SupersededBy == before.SupersededBy || current.SupersededBy == after.SupersededBy)
}

func cloneEngineShard(in *store.Shard) *store.Shard {
	out := &store.Shard{Schema: in.Schema, Nodes: make([]model.Node, len(in.Nodes))}
	for i := range in.Nodes {
		out.Nodes[i] = *cloneModelNode(&in.Nodes[i])
	}
	return out
}

func cloneModelNode(in *model.Node) *model.Node {
	if in == nil {
		return nil
	}
	out := *in
	out.Keywords = append([]string(nil), in.Keywords...)
	out.Lineage = append([]string(nil), in.Lineage...)
	out.Entries = make([]model.Entry, len(in.Entries))
	for i := range in.Entries {
		out.Entries[i] = in.Entries[i]
		out.Entries[i].BasedOn = append([]string(nil), in.Entries[i].BasedOn...)
		out.Entries[i].Disputes = append([]string(nil), in.Entries[i].Disputes...)
	}
	return &out
}

func entryInShard(sh *store.Shard, nodeID, entryID string) *model.Entry {
	for i := range sh.Nodes {
		if sh.Nodes[i].ID != nodeID {
			continue
		}
		for j := range sh.Nodes[i].Entries {
			if sh.Nodes[i].Entries[j].ID == entryID {
				return &sh.Nodes[i].Entries[j]
			}
		}
	}
	return nil
}

func (e *Engine) findCachedNode(id string) (*model.Node, string, bool) {
	var found *model.Node
	var rel string
	for shardRel, cs := range e.rt.cache.Shards() {
		if cs == nil || cs.Shard == nil {
			continue
		}
		for i := range cs.Shard.Nodes {
			if cs.Shard.Nodes[i].ID != id {
				continue
			}
			if found != nil {
				return nil, "", true
			}
			found = cloneModelNode(&cs.Shard.Nodes[i])
			rel = shardRel
		}
	}
	return found, rel, false
}

func (e *Engine) restoreShardBytes(saved []string, originals map[string][]byte) error {
	var errs []error
	for i := len(saved) - 1; i >= 0; i-- {
		rel := saved[i]
		if err := e.Store.WriteKnowledgeFile(rel, originals[rel]); err != nil {
			errs = append(errs, fmt.Errorf("回滚 %s: %w", rel, err))
		}
	}
	return errors.Join(errs...)
}

// legacyEffectsLocked 只恢复能从稳定 marker 精确证明 before 的旧副作用。
// RetiredBy=target.ID 的 before 必然只是清该 marker；旧 confirm/refute 的原
// confidence 无法从当前状态反推，尤其级联前可能本来就是 suspect，必须拒绝猜测。
func (e *Engine) legacyEffectsLocked(target *model.Change) []model.EntryEffect {
	var effects []model.EntryEffect
	for nodeID, ref := range e.rt.ix.Nodes() {
		for i := range ref.Node.Entries {
			en := &ref.Node.Entries[i]
			if en.RetiredBy == target.ID {
				after := entryState(en)
				before := after
				before.RetiredBy = ""
				effects = append(effects, model.EntryEffect{Entry: nodeID + "#" + en.ID, Before: before, After: after})
			}
		}
	}
	return effects
}
