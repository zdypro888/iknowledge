package store

import (
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/zdypro888/iknowledge/internal/model"
)

// Shard 是一个分片文件的内容:{schema: 1, nodes: [...]}(impl §4)。
type Shard struct {
	Schema int          `yaml:"schema"`
	Nodes  []model.Node `yaml:"nodes"`
}

// 分片读取的两类业务错误(impl §4 合并冲突容错、§3 版本演进)。
var (
	// ErrShardConflict 分片含未解决的合并冲突(或整体不可解析),隔离为 conflict 状态。
	ErrShardConflict = errors.New("分片有未解决的合并冲突或不可解析,请人工解决")
	// ErrSchemaTooNew 分片 schema 版本高于本二进制,按文件只读隔离(KB_ERR:SCHEMA_TOO_NEW)。
	ErrSchemaTooNew = errors.New("分片 schema 版本高于本二进制,请升级 iknowledge")
)

// LoadShard 读一个分片。返回的 raw 是原始 yaml 文档树,供 SaveShard 做未知字段回写;
// 调用方不持有 raw 时传 nil 保存(未知字段以磁盘现存内容为准重新读取)。
func (s *Store) LoadShard(path string) (*Shard, *yaml.Node, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	return decodeShard(path, data)
}

func decodeShard(path string, data []byte) (*Shard, *yaml.Node, error) {
	var raw yaml.Node
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, nil, fmt.Errorf("%w(%s: %v)", ErrShardConflict, path, err)
	}
	var sh Shard
	if err := raw.Decode(&sh); err != nil {
		return nil, nil, fmt.Errorf("%w(%s: %v)", ErrShardConflict, path, err)
	}
	if sh.Schema > model.SchemaVersion {
		return nil, nil, fmt.Errorf("%w(%s: schema %d > %d)", ErrSchemaTooNew, path, sh.Schema, model.SchemaVersion)
	}
	return &sh, &raw, nil
}

// SaveShard 原子写分片,未知字段往返保留(impl §4 定案,防旧二进制静默丢
// 同事用新版本写入的字段):以 raw(或磁盘现存内容)为底,把结构体状态编码后
// 与底上的未知字段合并——结构体已知字段以新值为准,底上多出的字段原样带回。
func (s *Store) SaveShard(path string, sh *Shard, raw *yaml.Node) error {
	if raw == nil {
		if data, err := os.ReadFile(path); err == nil {
			if _, r, err := decodeShard(path, data); err == nil {
				raw = r
			}
			// 现存文件坏(冲突态)时不做保留合并——调用方本就不该写 conflict 分片,
			// 此处按纯新内容写出属于显式覆盖。
		}
	}

	var fresh yaml.Node
	if err := fresh.Encode(sh); err != nil {
		return fmt.Errorf("store: 编码分片: %w", err)
	}
	if raw != nil {
		src := raw
		if src.Kind == yaml.DocumentNode && len(src.Content) == 1 {
			src = src.Content[0]
		}
		mergeUnknown(&fresh, src, shardSchema)
	}

	data, err := yaml.Marshal(&fresh)
	if err != nil {
		return fmt.Errorf("store: 序列化分片: %w", err)
	}
	return atomicWrite(path, data)
}

// mergeUnknown 把 src(磁盘旧文档)上的【未知字段】原样带回 dst(新编码):
//   - 两边同为 mapping:src 独有且【不在本层已知字段集】的 key 追加进 dst
//     ——已知字段缺席 = 引擎有意清零(omitempty),不得复活(如 pending_anchor 清除);
//     两边都有且同为 mapping/sequence 的按 schema 递归;标量以 dst(新值)为准;
//   - 两边同为 sequence 且元素是带 "id" 的 mapping(nodes/entries):按 id 配对递归;
//     src 独有的 id 不带回(dst 里没有 = 引擎有意删除);
//   - 其他情形一律以 dst 为准。
//
// 已知字段集由 model 结构体的 yaml tag 反射派生(schema.go),模型加字段自动跟进。
// mergeUnknown 把 src 的未知字段合进 dst(已知字段由引擎管,不在此)。
// R29-E7.4:depth 限制(上限 100)防恶意/损坏 shard 的极端嵌套导致栈溢出。
const maxMergeDepth = 100

func mergeUnknown(dst, src *yaml.Node, sc *mergeSchema) {
	mergeUnknownDepth(dst, src, sc, 0)
}

func mergeUnknownDepth(dst, src *yaml.Node, sc *mergeSchema, depth int) {
	if depth > maxMergeDepth || dst == nil || src == nil || dst.Kind != src.Kind {
		return
	}
	switch dst.Kind {
	case yaml.MappingNode:
		dstKeys := map[string]*yaml.Node{}
		for i := 0; i+1 < len(dst.Content); i += 2 {
			dstKeys[dst.Content[i].Value] = dst.Content[i+1]
		}
		for i := 0; i+1 < len(src.Content); i += 2 {
			k, v := src.Content[i], src.Content[i+1]
			if dv, ok := dstKeys[k.Value]; ok {
				mergeUnknownDepth(dv, v, sc.child(k.Value), depth+1)
				continue
			}
			if sc != nil && sc.known[k.Value] {
				continue // 已知字段被引擎清零,尊重删除
			}
			dst.Content = append(dst.Content, cloneNodeDepth(k, 0), cloneNodeDepth(v, 0))
		}
	case yaml.SequenceNode:
		srcByID := map[string]*yaml.Node{}
		for _, item := range src.Content {
			if id := mapScalar(item, "id"); id != "" {
				srcByID[id] = item
			}
		}
		if len(srcByID) == 0 {
			return
		}
		for _, item := range dst.Content {
			if id := mapScalar(item, "id"); id != "" {
				if sv, ok := srcByID[id]; ok {
					mergeUnknownDepth(item, sv, sc, depth+1) // sequence 的 schema 即元素 schema
				}
			}
		}
	}
}

// mapScalar 取 mapping 节点里某 key 的标量值;不是 mapping 或无该 key 返回 ""。
func mapScalar(n *yaml.Node, key string) string {
	if n == nil || n.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key && n.Content[i+1].Kind == yaml.ScalarNode {
			return n.Content[i+1].Value
		}
	}
	return ""
}

// cloneNode 深拷贝 yaml 节点(R29-E7.4:depth 限制防极端嵌套栈溢出)。
func cloneNode(n *yaml.Node) *yaml.Node { return cloneNodeDepth(n, 0) }

func cloneNodeDepth(n *yaml.Node, depth int) *yaml.Node {
	if n == nil || depth > maxMergeDepth {
		return nil
	}
	c := *n
	c.Content = make([]*yaml.Node, len(n.Content))
	for i, child := range n.Content {
		c.Content[i] = cloneNodeDepth(child, depth+1)
	}
	return &c
}
