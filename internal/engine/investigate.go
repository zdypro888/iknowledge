package engine

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/zdypro888/iknowledge/internal/model"
)

// kb_investigate(knowledge.md §10.4,轮 22 定案:委派为主、自派为备)。
// 委派主模式:服务端零 AI 进程管理——只维护 job 表与简报模板,秒回。
// 自派备模式(PTY)按轮 22 排期后移:委派模式实测足用则不做。

// InvestigateArgs 入参。
type InvestigateArgs struct {
	Question string `json:"question"`
	Scope    string `json:"scope,omitempty"`
}

// Investigate ① 先查库,命中新鲜流程/排障知识直接返回不派兵;
// ② 未命中开 job(同 repo 最多 1 个活跃,TTL 30 分钟)。
// 委派模式(主):秒回侦查简报;自派备模式(config scout=self):PTY 驱动侦察兵
// 进程执行简报,阻塞等交卷(impl §7.5)。简报附"来时路"(线索文件的近期
// git 提交)——git 子进程在锁外跑(#21 同族)。
func (e *Engine) Investigate(a InvestigateArgs, sid, author string) (string, error) {
	if err := e.requireInit(); err != nil {
		return "", err
	}
	if err := e.Sync(); err != nil {
		return "", err
	}
	// R29 批次3:用缓存 config(60s TTL,错误不再吞——configError() 进 kb_status)。
	cfg := e.cachedConfig()
	selfMode := cfg != nil && cfg.Scout == "self"

	out, job, clueFiles, err := e.investigateLocked(a, selfMode)
	if err != nil || job == nil {
		return out, err // 库内命中(不派兵)或错误
	}
	if trail := gitTrail(e.Store.RepoRoot(), clueFiles); trail != "" {
		out = strings.Replace(out, briefDisciplineMark,
			"来时路(线索文件的近期提交——「为什么长这样」的档案入口,深挖用 git show/blame):\n"+trail+briefDisciplineMark, 1)
	}
	if !selfMode {
		return out, nil
	}
	return e.selfDispatch(job, out, cfg)
}

// investigateLocked 是 Investigate 的持锁段。返回值三态:
// (库内命中文本, nil, nil, nil)不派兵;(简报, job, 线索文件, nil)已开 job;错误。
func (e *Engine) investigateLocked(a InvestigateArgs, selfMode bool) (string, *scoutJob, []string, error) {
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()

	if strings.TrimSpace(a.Question) == "" {
		return "", nil, nil, kbErr("INVALID_ARGUMENT", "question 必填", "描述要定位的问题")
	}

	// ① 先查库:流程/排障命中且新鲜 → 直接返回 findings,不派兵。
	if out, hit := e.libraryFindingsLocked(a.Question); hit {
		return framed(out), nil, nil, nil
	}

	// ② 递归护栏:同 repo 最多 1 个活跃 job(SCOUT_BUSY 同时挡住侦察兵套娃)。
	nowT := e.now()
	if e.rt.job != nil && !e.rt.job.expired(nowT) {
		return "", nil, nil, kbErr("SCOUT_BUSY",
			"已有活跃侦查 job "+e.rt.job.ID+"(问题:"+firstLine(e.rt.job.Question)+")",
			"等它交卷(kb_submit_findings)或 TTL 过期后重试")
	}
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		// job ID 是 kb_submit_findings 的凭证(R29-S1.2),低熵源不可用则 fail closed。
		return "", nil, nil, kbErr("INTERNAL", "熵源不可用,重试", "稍后重试 kb_investigate")
	}
	job := &scoutJob{
		ID: "job_" + hex.EncodeToString(b), Question: a.Question, Scope: a.Scope,
		Started: nowT, TTL: 30 * time.Minute,
		done: make(chan string, 1),
	}
	e.rt.job = job

	brief, clueFiles := e.briefingLocked(job, selfMode)
	return brief, job, clueFiles, nil
}

// libraryFindingsLocked 库内直接命中的判定:流程(标题/排障/约定)关键词命中,
// 或倒排检索命中带活跃知识且 fresh 的节点。
func (e *Engine) libraryFindingsLocked(question string) (string, bool) {
	var b strings.Builder
	hit := false

	// 流程命中。
	for i := range e.rt.flows {
		f := &e.rt.flows[i]
		if f.Deprecated {
			continue
		}
		text := f.Title + " " + f.Troubleshoot + " " + strings.Join(f.Conventions, " ")
		if keywordOverlap(question, text) {
			out, _ := e.flowView(f.ID)
			b.WriteString(out)
			hit = true
		}
	}
	// 节点命中(fresh 且有活跃知识)。
	var locations []string
	for _, h := range e.rt.ix.Search(question, 5) {
		ref := e.rt.ix.Node(h.NodeID)
		if ref.Node.Status == model.StatusFresh && hasActiveEntries(ref.Node) && h.Score >= 2 {
			locations = append(locations, h.NodeID)
		}
	}
	if len(locations) > 0 {
		fmt.Fprintf(&b, "库内已有相关知识节点:%s(kb_recall 取全量)\n", strings.Join(locations, "、"))
		hit = true
	}
	if !hit {
		return "", false
	}
	b.WriteString("—— 以上为库内命中,未派兵;不足以定位时再次调用 kb_investigate 我会开 job 出简报。")
	return b.String(), true
}

// keywordOverlap 问题与文本的粗粒度关键词重叠(≥2 个共同 token)。
func keywordOverlap(q, text string) bool {
	qt := map[string]bool{}
	for _, t := range tokenizeForOverlap(q) {
		qt[t] = true
	}
	n := 0
	for _, t := range tokenizeForOverlap(text) {
		if qt[t] {
			n++
			if n >= 2 {
				return true
			}
		}
	}
	return false
}

func tokenizeForOverlap(s string) []string {
	// 复用 index 分词太重,这里 bigram 集合即可。
	set := bigramSet(s)
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}

// briefDisciplineMark 是简报里"来时路"段的插入锚(Investigate 锁外拼装 git 线索用)。
const briefDisciplineMark = "侦查纪律(子代理执行):"

// briefingLocked 侦查简报(impl §7.5 模板):问题+scope+库内线索+侦查纪律+收尾义务。
// selfMode 变体:侦察兵进程自己就是执行者(无"转交子代理"的置顶指令,收尾不用带回
// ——交卷经 kb_submit_findings 已由服务端回程给主 AI)。
// 第二返回值是线索节点的去重文件清单(≤3,来时路的 git 查询对象)。
func (e *Engine) briefingLocked(job *scoutJob, selfMode bool) (string, []string) {
	var clues strings.Builder
	var clueFiles []string
	seenFile := map[string]bool{}
	for _, h := range e.rt.ix.Search(job.Question, 5) {
		ref := e.rt.ix.Node(h.NodeID)
		fmt.Fprintf(&clues, "  - %s %s\n", h.NodeID, nodeLine(ref.Node))
		if file, _ := model.SplitNodeID(h.NodeID); file != "" && file != model.ProjectNodeID &&
			!strings.HasSuffix(file, "/") && !seenFile[file] && len(clueFiles) < 3 {
			seenFile[file] = true
			clueFiles = append(clueFiles, file)
		}
	}
	if clues.Len() == 0 {
		clues.WriteString("  (库内无线索——空库期,你的蒸馏就是第一批知识)\n")
	}
	scope := job.Scope
	if scope == "" {
		scope = "(全仓)"
	}
	head := "【侦查简报 " + job.ID + "】置顶指令:把本简报【原样】交给一个子代理(如 Claude Code 的 Task 工具)执行,不要自己执行——保护你自己的上下文。"
	tail := "收尾交卷,然后把同样内容完整写进你的最终答复带回主 AI。"
	if selfMode {
		head = "【侦查简报 " + job.ID + "】你就是侦察兵,直接执行本简报。"
		tail = "收尾交卷(交卷即回程,主 AI 在等待中)。"
	}
	// 简报的降级门(与纪律段首句同哲学):受限子代理可能没有 kb_* 工具,
	// 上面的工具指令对它是死指令——附只读腿 + 代沉淀/代交卷条款。
	degrade := ""
	if base := e.scoutBase(); base != "" {
		degrade = fmt.Sprintf(`
子代理若无 kb_* 工具(受限工具集):查库用只读腿 curl "http://%s/recall?q=<词>"(/map、/status 同理);蒸馏与 conclusion/locations/plan/risks 完整写进最终答复,由主 AI 代 kb_remember 与 kb_submit_findings。`, base)
	}
	return fmt.Sprintf(head+`

问题: %s
范围: %s
库内线索:
%s
`+briefDisciplineMark+`
1. 你是一次性侦察兵:脏读随便 grep/试错,你的上下文用完即焚,价值全靠蒸馏落库;
2. 蒸馏义务:定位过程中悟到的流程/坑/关键词,满足沉淀阈值就 kb_remember(尤其把你用过的检索词回填进目标节点 keywords);进行中状态写 kb_task;
3. 禁止调用 kb_investigate(防套娃)与 kb_record_change(侦察兵不改码,铁律二);
4. 必须以 kb_submit_findings(job=%s, conclusion/locations/plan/risks)`+tail+degrade+`
(job TTL 30 分钟,过期作废)`, job.Question, scope, clues.String(), job.ID), clueFiles
}

// FindingsArgs 是 kb_submit_findings 入参。
type FindingsArgs struct {
	Job        string   `json:"job"`
	Conclusion string   `json:"conclusion"`
	Locations  []string `json:"locations,omitempty"`
	Plan       string   `json:"plan,omitempty"`
	Risks      string   `json:"risks,omitempty"`
}

// SubmitFindings 侦察兵交卷:findings 落库存档 + 销 job(impl §7.3)。
// 委派模式下不路由——子代理自己的返回值就是回程通道。
func (e *Engine) SubmitFindings(a FindingsArgs, sid, author string) (string, error) {
	if err := e.requireInit(); err != nil {
		return "", err
	}
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()

	if e.rt.job == nil || e.rt.job.expired(e.now()) || e.rt.job.ID != a.Job {
		return "", kbErr("JOB_NOT_FOUND",
			"job "+a.Job+" 不存在或已过期(防误调/迟到乱入)",
			"主 AI 重新 kb_investigate 开新 job")
	}
	if strings.TrimSpace(a.Conclusion) == "" {
		return "", kbErr("EVIDENCE_REQUIRED", "conclusion 必填", "给出定位结论")
	}
	f := model.Findings{
		Job: a.Job, Question: e.rt.job.Question,
		Conclusion: a.Conclusion, Locations: a.Locations,
		Plan: a.Plan, Risks: a.Risks,
		At: e.now().UTC(), Author: author,
	}
	if err := e.Store.AppendFindings(f); err != nil {
		return "", err
	}
	// 自派备模式:给阻塞中的 Investigate 投递格式化 findings(缓冲 1,不阻塞)。
	if e.rt.job.done != nil {
		select {
		case e.rt.job.done <- formatFindings(f):
		default:
		}
	}
	e.rt.job = nil
	return "ack:findings 已存档、job 已销。请把 conclusion/locations/plan/risks 完整写进你的最终答复,带回主 AI。", nil
}

// formatFindings 把交卷内容格式化为主 AI 可读文本(自派回程)。
func formatFindings(f model.Findings) string {
	var b strings.Builder
	fmt.Fprintf(&b, "【侦察兵交卷 %s】\n结论: %s\n", f.Job, f.Conclusion)
	if len(f.Locations) > 0 {
		fmt.Fprintf(&b, "位置: %s\n", strings.Join(f.Locations, "、"))
	}
	if f.Plan != "" {
		fmt.Fprintf(&b, "改动方案: %s\n", f.Plan)
	}
	if f.Risks != "" {
		fmt.Fprintf(&b, "风险: %s\n", f.Risks)
	}
	return strings.TrimRight(b.String(), "\n")
}
