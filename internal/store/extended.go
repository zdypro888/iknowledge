package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"

	"github.com/zdypro888/iknowledge/internal/model"
)

// ---- 流程/主题节点(knowledge.md §6;flows/ 与 topics/ 目录) ----

// FlowPathFor 由 flow ID 得文件路径:"flow:user-login" → flows/user-login.yaml,
// "topic:error-handling" → topics/error-handling.yaml。非法 ID 返回空串。
func (s *Store) FlowPathFor(id string) string {
	kind, name, ok := strings.Cut(id, ":")
	if !ok || !safeFlowName(name) {
		return ""
	}
	switch kind {
	case "flow":
		return filepath.Join(s.dir, "flows", name+".yaml")
	case "topic":
		return filepath.Join(s.dir, "topics", name+".yaml")
	}
	return ""
}

func safeFlowName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 240 ||
		strings.ContainsAny(name, `/\:<>"|?*`) || strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Mc, r) || unicode.Is(unicode.Me, r) {
			return false
		}
	}
	base := name
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	upper := strings.ToUpper(base)
	if upper == "CON" || upper == "PRN" || upper == "AUX" || upper == "NUL" {
		return false
	}
	if len(upper) == 4 && (strings.HasPrefix(upper, "COM") || strings.HasPrefix(upper, "LPT")) &&
		upper[3] >= '1' && upper[3] <= '9' {
		return false
	}
	return true
}

// LoadFlows 读全部流程/主题节点;不可解析的文件忽略并告警(impl §4)。
func (s *Store) LoadFlows() ([]model.Flow, []string, error) {
	type loadedFlow struct {
		flow   model.Flow
		origin string
	}
	var loaded []loadedFlow
	var warnings []string
	for _, sub := range []string{"flows", "topics"} {
		dir := filepath.Join(s.dir, sub)
		entries, err := s.readKnowledgeDir(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("store: 读 %s: %w", sub, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
				continue
			}
			name := strings.TrimSuffix(e.Name(), ".yaml")
			if !safeFlowName(name) {
				warnings = append(warnings, fmt.Sprintf("%s/%s 文件名跨平台不安全,已只读隔离", sub, e.Name()))
				continue
			}
			data, err := s.readKnowledgeFile(filepath.Join(dir, e.Name()))
			if err != nil {
				return nil, nil, err
			}
			path := filepath.Join(dir, e.Name())
			fs, _, err := decodeFlowShard(path, data)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("%s/%s %v,已只读隔离", sub, e.Name(), err))
				continue
			}
			prefix := "flow"
			if sub == "topics" {
				prefix = "topic"
			}
			expected := prefix + ":" + name
			if fs.Flow.ID != expected {
				warnings = append(warnings, fmt.Sprintf("%s/%s 声明 %s，期望 %s，已只读隔离", sub, e.Name(), fs.Flow.ID, expected))
				continue
			}
			loaded = append(loaded, loadedFlow{flow: fs.Flow, origin: sub + "/" + e.Name()})
		}
	}
	counts := map[string]int{}
	for _, item := range loaded {
		counts[item.flow.ID]++
	}
	var flows []model.Flow
	for _, item := range loaded {
		if counts[item.flow.ID] > 1 {
			warnings = append(warnings, fmt.Sprintf("%s 重复声明 Flow ID %s，全部隔离", item.origin, item.flow.ID))
			continue
		}
		flows = append(flows, item.flow)
	}
	sort.Slice(flows, func(i, j int) bool { return flows[i].ID < flows[j].ID })
	return flows, warnings, nil
}

// flowSchema 是 flow 文件的已知字段树(未知字段往返保留用)。
var flowSchema = buildSchema(reflect.TypeFor[model.FlowShard]())

// SaveFlow 原子写一个流程/主题节点,未知字段往返保留(#34:与 tree 分片同哲学,
// 防旧二进制重写 flow 时把同事用新版本写入的字段静默删掉)。
func (s *Store) SaveFlow(f model.Flow) error {
	path := s.FlowPathFor(f.ID)
	if path == "" {
		return fmt.Errorf("store: 非法 flow ID %q(应为 flow:name 或 topic:name)", f.ID)
	}
	var fresh yaml.Node
	if err := fresh.Encode(model.FlowShard{Schema: model.SchemaVersion, Flow: f}); err != nil {
		return err
	}
	// 以磁盘现存内容为底合并未知字段。更高 schema 或损坏文件必须拒写,
	// 不能由旧二进制降回 schema 1、静默抹掉新字段。
	var raw *yaml.Node
	if old, err := s.readKnowledgeFile(path); err == nil {
		_, raw, err = decodeFlowShard(path, old)
		if err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if raw != nil {
		src := raw
		if src.Kind == yaml.DocumentNode && len(src.Content) == 1 {
			src = src.Content[0]
		}
		if err := mergeUnknown(&fresh, src, flowSchema); err != nil {
			return err
		}
	}
	data, err := yaml.Marshal(&fresh)
	if err != nil {
		return err
	}
	return s.atomicWrite(path, data)
}

func decodeFlowShard(path string, data []byte) (model.FlowShard, *yaml.Node, error) {
	var raw yaml.Node
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return model.FlowShard{}, nil, fmt.Errorf("flow 不可解析(%s): %w", path, err)
	}
	var fs model.FlowShard
	if err := raw.Decode(&fs); err != nil || fs.Flow.ID == "" {
		if err == nil {
			err = fmt.Errorf("缺 flow.id")
		}
		return model.FlowShard{}, nil, fmt.Errorf("flow 不可解析(%s): %w", path, err)
	}
	if fs.Schema > model.SchemaVersion {
		return model.FlowShard{}, nil, fmt.Errorf("%w(%s: schema %d > %d)",
			ErrSchemaTooNew, path, fs.Schema, model.SchemaVersion)
	}
	return fs, &raw, nil
}

// ---- 任务态 WIP(knowledge.md §7;wip/ 目录,git 排除) ----

func (s *Store) wipPath(owner string) string {
	return filepath.Join(s.dir, filepath.FromSlash(s.WIPRelFor(owner)))
}

// WIPRelFor 返回 owner 台账的 .knowledge 正斜杠相对路径，供 task complete
// 把 WIP 删除与 journal 追加放进同一个崩溃可恢复事务。
func (s *Store) WIPRelFor(owner string) string {
	safe := strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' {
			return '-'
		}
		return r
	}, owner)
	return "wip/" + safe + ".yaml"
}

// LoadWIPs 读全部活跃任务态。
func (s *Store) LoadWIPs() ([]model.WIP, error) {
	dir := filepath.Join(s.dir, "wip")
	entries, err := s.readKnowledgeDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var wips []model.WIP
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		data, err := s.readKnowledgeFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var w model.WIP
		if err := yaml.Unmarshal(data, &w); err != nil || w.Task == "" {
			continue // 坏台账不阻塞;task 为空视为已清
		}
		wips = append(wips, w)
	}
	sort.Slice(wips, func(i, j int) bool { return wips[i].Owner < wips[j].Owner })
	return wips, nil
}

// SaveWIP 写一份任务态;ClearWIP 归档后清空。
func (s *Store) SaveWIP(w model.WIP) error {
	data, err := yaml.Marshal(&w)
	if err != nil {
		return err
	}
	return s.atomicWrite(s.wipPath(w.Owner), data)
}

// ClearWIP 删除 owner 的台账(complete 归档后)。
func (s *Store) ClearWIP(owner string) error {
	if err := s.removeKnowledgeFile(s.wipPath(owner)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ---- 侦查 findings 存档(local 本地态,不进 git) ----

// AppendFindings 追加一条侦查交卷存档。
func (s *Store) AppendFindings(f model.Findings) error {
	line, err := json.Marshal(f)
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, "local", "findings-"+f.At.UTC().Format("2006-01")+".jsonl")
	return s.appendKnowledgeFile(path, append(line, '\n'), 0o644, false)
}

// ---- 维护欠账消解记录(local 本地态,不进 git) ----
// dup-entries 是 bigram 启发式,会有假阳性;AI 判定两条实为不同后消解,
// 记进 local 使其不再复现(#11:否则现算欠账每次都重报,无法清除)。

func (s *Store) dismissedPath() string {
	return filepath.Join(s.dir, "local", "dismissed-debts.txt")
}

// DismissDebt 记一条已消解的欠账 ID(去重追加)。
func (s *Store) DismissDebt(id string) error {
	seen, _ := s.LoadDismissedDebts()
	if seen[id] {
		return nil
	}
	return s.appendKnowledgeFile(s.dismissedPath(), []byte(id+"\n"), 0o644, false)
}

// LoadDismissedDebts 读已消解欠账 ID 集合。
func (s *Store) LoadDismissedDebts() (map[string]bool, error) {
	out := map[string]bool{}
	data, err := s.readKnowledgeFile(s.dismissedPath())
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out[line] = true
		}
	}
	return out, nil
}

// ---- 使用日志(impl §7.6,local 本地态) ----

// UsageRecord 是一次 tools/call 的日志行。
type UsageRecord struct {
	At      string `json:"at"`
	Session string `json:"session,omitempty"`
	Tool    string `json:"tool"`
	// Source 调用来源:空=MCP 工具调用;"http"=子代理只读腿(统计口径区分,
	// 数据裁决时能看出只读腿的实际使用量)。
	Source    string `json:"source,omitempty"`
	OK        bool   `json:"ok"`
	ErrCode   string `json:"errCode,omitempty"`
	Hit       bool   `json:"hit,omitempty"`       // recall/map 是否命中
	HitStatus string `json:"hitStatus,omitempty"` // 命中节点状态(undigested 命中率数据源)
	Stale     bool   `json:"stale,omitempty"`     // recall 读取时对账发现失配(未记账变更事件)
	MS        int64  `json:"ms"`
}

// AppendUsage 追加一行使用日志(尽力而为:日志失败不影响业务)。
func (s *Store) AppendUsage(month string, rec UsageRecord) {
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	path := filepath.Join(s.dir, "local", "usage-"+month+".jsonl")
	_ = s.appendKnowledgeFile(path, append(line, '\n'), 0o644, false)
}

// LoadUsage 读全部使用日志(kb_status 汇总用)。
func (s *Store) LoadUsage() ([]UsageRecord, error) {
	dir := filepath.Join(s.dir, "local")
	entries, err := s.readKnowledgeDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var recs []UsageRecord
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "usage-") || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		data, err := s.readKnowledgeFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		for line := range strings.SplitSeq(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var r UsageRecord
			if json.Unmarshal([]byte(line), &r) == nil {
				recs = append(recs, r)
			}
		}
	}
	return recs, nil
}
