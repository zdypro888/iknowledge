package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zdypro888/iknowledge/internal/model"
)

// JournalStats 是读端契约(impl §4 定案)的诊断信息。
type JournalStats struct {
	BadLines    int      // 无法解析的行(冲突残留/断电半行),跳过并计数进 kb_status
	DupDropped  int      // 整行相同静默去重的行数
	ConflictIDs []string // ID 相同但内容不同:告警并双份保留供人裁决
}

// journalPath 返回某时刻对应的月份文件路径(按 UTC 月分片,append-only)。
func (s *Store) journalPath(c model.Change) string {
	return filepath.Join(s.dir, "journal", c.At.UTC().Format("2006-01")+".jsonl")
}

// AppendChange 往对应月份文件追加一行(O_APPEND;ID 查重是 engine 的职责)。
// 落盘带 fsync:journal 与分片同属真相数据(local/ 下的 usage/findings 可再生,不 fsync)。
func (s *Store) AppendChange(c model.Change) error {
	line, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("store: 编码 change: %w", err)
	}
	path := s.journalPath(c)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("store: 建 journal 目录: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("store: 开 journal: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("store: 追加 journal: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("store: fsync journal: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("store: 关闭 journal: %w", err)
	}
	return nil
}

// LoadJournal 加载全部变更记录并执行读端契约(impl §4 定案):
// union 合并的产物乱序与重复是常态——按 at 全排序(文件行序不可信)、
// 整行相同静默去重、ID 相同内容不同告警双份保留、坏行跳过计数。
func (s *Store) LoadJournal() ([]model.Change, JournalStats, error) {
	var (
		changes []model.Change
		stats   JournalStats
		seen    = map[string]bool{}   // 整行文本 → 去重
		byID    = map[string]string{} // ID → 首见行文本(检测同 ID 异内容)
		flagged = map[string]bool{}   // 已计入 ConflictIDs 的 ID
	)
	dir := filepath.Join(s.dir, "journal")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, stats, nil
	}
	if err != nil {
		return nil, stats, fmt.Errorf("store: 读 journal 目录: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, stats, fmt.Errorf("store: 读 %s: %w", e.Name(), err)
		}
		for line := range strings.SplitSeq(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if seen[line] {
				stats.DupDropped++
				continue
			}
			// 坏行也进 seen:union 重复是常态,同一坏行只计一次 BadLines
			// (R2-A7:原先坏行不去重,统计口径随重复次数虚增)。
			seen[line] = true
			var c model.Change
			if err := json.Unmarshal([]byte(line), &c); err != nil || c.ID == "" {
				stats.BadLines++
				continue
			}
			if prev, ok := byID[c.ID]; ok && prev != line && !flagged[c.ID] {
				stats.ConflictIDs = append(stats.ConflictIDs, c.ID)
				flagged[c.ID] = true
			}
			byID[c.ID] = line
			changes = append(changes, c)
		}
	}
	// "近 N 条"一律指 at 降序;此处升序全排,读端自行取尾。at 相同按 ID 定序保证确定性。
	sort.SliceStable(changes, func(i, j int) bool {
		if !changes[i].At.Equal(changes[j].At) {
			return changes[i].At.Before(changes[j].At)
		}
		return changes[i].ID < changes[j].ID
	})
	return changes, stats, nil
}
