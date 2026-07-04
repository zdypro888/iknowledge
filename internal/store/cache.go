package store

import (
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/zdypro888/iknowledge/internal/model"
)

// Cache 是内存态的 .knowledge 视图,配惰性重载(impl §4 修订):
// 不引入 fsnotify;每次请求前 Refresh——
// ① 递归目录清单对账捕捉文件新增与删除(git 切分支的主要形态);
// ② 已缓存文件按 mtime+size 快查内容变更,变了才重读(size 兜住同秒两次写入)。
// ①② 对 tree 分片与 journal 月份文件同等适用:checkout 后 journal 常是同名但内容不同,
// 必须经 ② 重读,否则 history 会持续返回另一分支的幽灵决策链。
type Cache struct {
	s *Store

	states  map[string]fileState    // 相对 .knowledge 的正斜杠路径 → 文件状态
	shards  map[string]*CachedShard // 同键;仅 tree 分片与 project.yaml
	journal []model.Change
	jstats  JournalStats
}

type fileState struct {
	mtime int64 // UnixNano
	size  int64
}

// CachedShard 是一个分片的缓存态;Err 非空表示 conflict/schema 隔离(内容不可用)。
type CachedShard struct {
	Shard *Shard
	Raw   *yaml.Node
	Err   error
}

// NewCache 建缓存(空,首次 Refresh 全量加载)。
func NewCache(s *Store) *Cache {
	return &Cache{s: s, states: map[string]fileState{}, shards: map[string]*CachedShard{}}
}

// RefreshReport 汇总一次对账的变化。
type RefreshReport struct {
	Added, Changed, Removed []string
}

// Refresh 执行惰性重载对账。任何 journal 文件变化都触发 journal 全量重读
// (万级行毫秒级,读端契约本就要全排序)。
func (c *Cache) Refresh() (RefreshReport, error) {
	var rep RefreshReport
	current := map[string]fileState{}

	err := filepath.WalkDir(c.s.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			// local/ 与 wip/ 是本地态,不进知识视图。
			if name := d.Name(); name == "local" || name == "wip" {
				return filepath.SkipDir
			}
			return nil
		}
		rel := c.rel(path)
		if !cacheScope(rel) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil // 对账瞬间被删,当作不存在,下轮再见
		}
		current[rel] = fileState{mtime: info.ModTime().UnixNano(), size: info.Size()}
		return nil
	})
	if err != nil {
		return rep, err
	}

	journalDirty := false
	for rel, st := range current {
		old, ok := c.states[rel]
		switch {
		case !ok:
			rep.Added = append(rep.Added, rel)
		case old != st:
			rep.Changed = append(rep.Changed, rel)
		default:
			continue
		}
		if strings.HasPrefix(rel, "journal/") {
			journalDirty = true
		} else {
			c.loadShard(rel)
		}
	}
	for rel := range c.states {
		if _, ok := current[rel]; ok {
			continue
		}
		rep.Removed = append(rep.Removed, rel)
		if strings.HasPrefix(rel, "journal/") {
			journalDirty = true
		} else {
			delete(c.shards, rel)
		}
	}
	// journal 重载先于提交 states(#19:原先无条件先 c.states=current,
	// LoadJournal 失败时 states 已更新,下轮对账不再认为 journal 脏 → 永不重试,
	// 卡在旧 journal 上)。只有重载成功才提交,失败保留旧 states 令下轮重试。
	if journalDirty {
		changes, stats, err := c.s.LoadJournal()
		if err != nil {
			return rep, err
		}
		c.journal, c.jstats = changes, stats
	}
	c.states = current
	return rep, nil
}

// Shards 返回缓存的分片视图(键:相对 .knowledge 的正斜杠路径)。
func (c *Cache) Shards() map[string]*CachedShard { return c.shards }

// ConflictShard 返回某源文件对应分片的隔离错误(conflict/schema-too-new);
// nil 表示分片正常或不存在。写路径据此拒绝覆盖(#28/#35:否则空壳分片
// 会盖掉带冲突标记的分片,连同人未解决的另一分支知识一起丢失)。
func (c *Cache) ConflictShard(srcRel string) error {
	if cs := c.shards["tree/"+srcRel+".yaml"]; cs != nil {
		return cs.Err
	}
	return nil
}

// Journal 返回缓存的变更记录(at 升序)与读端诊断。
func (c *Cache) Journal() ([]model.Change, JournalStats) { return c.journal, c.jstats }

func (c *Cache) rel(path string) string {
	rel, err := filepath.Rel(c.s.dir, path)
	if err != nil {
		return ""
	}
	return filepath.ToSlash(rel)
}

// cacheScope 界定重载范围:tree 分片、project.yaml、journal 月份文件。
// config.yaml 是启动配置,不热载;flows/topics 一期读到未知文件忽略(impl §4)。
func cacheScope(rel string) bool {
	switch {
	case rel == "project.yaml":
		return true
	case strings.HasPrefix(rel, "tree/") && strings.HasSuffix(rel, ".yaml"):
		return true
	case strings.HasPrefix(rel, "journal/") && strings.HasSuffix(rel, ".jsonl"):
		return true
	}
	return false
}

func (c *Cache) loadShard(rel string) {
	sh, raw, err := c.s.LoadShard(filepath.Join(c.s.dir, filepath.FromSlash(rel)))
	if err != nil {
		// conflict/schema 隔离态也进缓存——recall 要如实呈现"该分片不可用"。
		c.shards[rel] = &CachedShard{Err: err}
		return
	}
	c.shards[rel] = &CachedShard{Shard: sh, Raw: raw}
}
