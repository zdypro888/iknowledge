package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/zdypro888/iknowledge/internal/model"
)

// ---- 流程/主题节点(knowledge.md §6;flows/ 与 topics/ 目录) ----

// FlowPathFor 由 flow ID 得文件路径:"flow:user-login" → flows/user-login.yaml,
// "topic:error-handling" → topics/error-handling.yaml。非法 ID 返回空串。
func (s *Store) FlowPathFor(id string) string {
	kind, name, ok := strings.Cut(id, ":")
	if !ok || name == "" || strings.ContainsAny(name, "/\\") {
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

// LoadFlows 读全部流程/主题节点;不可解析的文件忽略并告警(impl §4)。
func (s *Store) LoadFlows() ([]model.Flow, []string, error) {
	var flows []model.Flow
	var warnings []string
	for _, sub := range []string{"flows", "topics"} {
		dir := filepath.Join(s.dir, sub)
		entries, err := os.ReadDir(dir)
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
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				return nil, nil, err
			}
			var fs model.FlowShard
			if err := yaml.Unmarshal(data, &fs); err != nil || fs.Flow.ID == "" {
				warnings = append(warnings, fmt.Sprintf("%s/%s 不可解析,已忽略", sub, e.Name()))
				continue
			}
			flows = append(flows, fs.Flow)
		}
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
	// 以磁盘现存内容为底合并未知字段。
	if old, err := os.ReadFile(path); err == nil {
		var raw yaml.Node
		if yaml.Unmarshal(old, &raw) == nil {
			src := &raw
			if src.Kind == yaml.DocumentNode && len(src.Content) == 1 {
				src = src.Content[0]
			}
			mergeUnknown(&fresh, src, flowSchema)
		}
	}
	data, err := yaml.Marshal(&fresh)
	if err != nil {
		return err
	}
	return atomicWrite(path, data)
}

// ---- 任务态 WIP(knowledge.md §7;wip/ 目录,git 排除) ----

func (s *Store) wipPath(owner string) string {
	safe := strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' {
			return '-'
		}
		return r
	}, owner)
	return filepath.Join(s.dir, "wip", safe+".yaml")
}

// LoadWIPs 读全部活跃任务态。
func (s *Store) LoadWIPs() ([]model.WIP, error) {
	dir := filepath.Join(s.dir, "wip")
	entries, err := os.ReadDir(dir)
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
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
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
	return atomicWrite(s.wipPath(w.Owner), data)
}

// ClearWIP 删除 owner 的台账(complete 归档后)。
func (s *Store) ClearWIP(owner string) error {
	if err := os.Remove(s.wipPath(owner)); err != nil && !os.IsNotExist(err) {
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
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	fh, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer fh.Close()
	if _, err := fh.Write(append(line, '\n')); err != nil {
		return err
	}
	return fh.Close()
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
	if err := os.MkdirAll(filepath.Dir(s.dismissedPath()), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(s.dismissedPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(id + "\n")
	return err
}

// LoadDismissedDebts 读已消解欠账 ID 集合。
func (s *Store) LoadDismissedDebts() (map[string]bool, error) {
	out := map[string]bool{}
	data, err := os.ReadFile(s.dismissedPath())
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
	Source string `json:"source,omitempty"`
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
	if os.MkdirAll(filepath.Dir(path), 0o755) != nil {
		return
	}
	fh, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer fh.Close()
	fh.Write(append(line, '\n'))
}

// LoadUsage 读全部使用日志(kb_status 汇总用)。
func (s *Store) LoadUsage() ([]UsageRecord, error) {
	dir := filepath.Join(s.dir, "local")
	entries, err := os.ReadDir(dir)
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
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
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
