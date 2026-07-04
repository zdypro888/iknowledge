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
// ② 未命中开 job(同 repo 最多 1 个活跃,TTL 30 分钟)秒回侦查简报。
func (e *Engine) Investigate(a InvestigateArgs, sid, author string) (string, error) {
	if err := e.requireInit(); err != nil {
		return "", err
	}
	if err := e.Sync(); err != nil {
		return "", err
	}
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()

	if strings.TrimSpace(a.Question) == "" {
		return "", kbErr("INVALID_ARGUMENT", "question 必填", "描述要定位的问题")
	}

	// ① 先查库:流程/排障命中且新鲜 → 直接返回 findings,不派兵。
	if out, hit := e.libraryFindingsLocked(a.Question); hit {
		return framed(out), nil
	}

	// ② 递归护栏:同 repo 最多 1 个活跃 job(SCOUT_BUSY 同时挡住侦察兵套娃)。
	nowT := e.now()
	if e.rt.job != nil && !e.rt.job.expired(nowT) {
		return "", kbErr("SCOUT_BUSY",
			"已有活跃侦查 job "+e.rt.job.ID+"(问题:"+firstLine(e.rt.job.Question)+")",
			"等它交卷(kb_submit_findings)或 TTL 过期后重试")
	}
	b := make([]byte, 4)
	rand.Read(b)
	job := &scoutJob{
		ID: "job_" + hex.EncodeToString(b), Question: a.Question, Scope: a.Scope,
		Started: nowT, TTL: 30 * time.Minute,
	}
	e.rt.job = job

	return e.briefingLocked(job), nil
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

// briefingLocked 侦查简报(impl §7.5 模板):问题+scope+库内线索+侦查纪律+收尾义务。
func (e *Engine) briefingLocked(job *scoutJob) string {
	var clues strings.Builder
	for _, h := range e.rt.ix.Search(job.Question, 5) {
		ref := e.rt.ix.Node(h.NodeID)
		fmt.Fprintf(&clues, "  - %s %s\n", h.NodeID, nodeLine(ref.Node))
	}
	if clues.Len() == 0 {
		clues.WriteString("  (库内无线索——空库期,你的蒸馏就是第一批知识)\n")
	}
	scope := job.Scope
	if scope == "" {
		scope = "(全仓)"
	}
	return fmt.Sprintf(`【侦查简报 %s】置顶指令:把本简报【原样】交给一个子代理(如 Claude Code 的 Task 工具)执行,不要自己执行——保护你自己的上下文。

问题: %s
范围: %s
库内线索:
%s
侦查纪律(子代理执行):
1. 你是一次性侦察兵:脏读随便 grep/试错,你的上下文用完即焚,价值全靠蒸馏落库;
2. 蒸馏义务:定位过程中悟到的流程/坑/关键词,满足沉淀阈值就 kb_remember(尤其把你用过的检索词回填进目标节点 keywords);进行中状态写 kb_task;
3. 禁止调用 kb_investigate(防套娃)与 kb_record_change(侦察兵不改码,铁律二);
4. 必须以 kb_submit_findings(job=%s, conclusion/locations/plan/risks)收尾交卷,然后把同样内容完整写进你的最终答复带回主 AI。
(job TTL 30 分钟,过期作废)`, job.ID, job.Question, scope, clues.String(), job.ID)
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
	e.rt.job = nil
	return "ack:findings 已存档、job 已销。请把 conclusion/locations/plan/risks 完整写进你的最终答复,带回主 AI。", nil
}
