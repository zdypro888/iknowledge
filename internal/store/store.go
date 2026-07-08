// Package store 实现 .knowledge/ 的文件存储(impl §4):分片读写(未知字段往返保留)、
// journal 追加与读端契约、原子写、写者互斥锁、配置。文件是唯一真相,本包不含业务规则。
//
// 铁律二:本包唯一写入 .knowledge/,任何往其外写文件的代码都是 bug。
package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zdypro888/iknowledge/internal/model"
)

// KnowledgeDir 是知识库目录名。
const KnowledgeDir = ".knowledge"

// Store 是一个仓库的 .knowledge/ 存取器。
type Store struct {
	repo string // 仓库根(绝对路径)
	dir  string // <repo>/.knowledge
}

// Open 打开(不创建)仓库的存取器。
func Open(repoRoot string) (*Store, error) {
	abs, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("store: 解析仓库路径: %w", err)
	}
	return &Store{repo: abs, dir: filepath.Join(abs, KnowledgeDir)}, nil
}

// RepoRoot 返回仓库根绝对路径。
func (s *Store) RepoRoot() string { return s.repo }

// Dir 返回 .knowledge 绝对路径。
func (s *Store) Dir() string { return s.dir }

// Initialized 判断库是否已初始化(存在 .knowledge/project.yaml)。
func (s *Store) Initialized() bool {
	_, err := os.Stat(s.ProjectShardPath())
	return err == nil
}

// EnsureLayout 幂等创建目录布局(knowledge.md §11.4):
// tree/、journal/、wip/、local/、flows/、topics/(后三类一期只建目录不实现逻辑)。
func (s *Store) EnsureLayout() error {
	for _, d := range []string{"", "tree", "journal", "wip", "local", "flows", "topics"} {
		if err := os.MkdirAll(filepath.Join(s.dir, d), 0o755); err != nil {
			return fmt.Errorf("store: 建目录 %s: %w", d, err)
		}
	}
	return nil
}

// ---- 路径规则(impl §4) ----

// ShardPathFor 返回源文件的知识分片路径:tree/<源文件相对路径>.yaml。
// 路径不安全(含 ..、绝对路径等,铁律二)时返回 ""——调用方须拒写。
func (s *Store) ShardPathFor(srcRel string) string {
	safe, ok := model.SafeRel(srcRel)
	if !ok {
		return ""
	}
	return filepath.Join(s.dir, "tree", filepath.FromSlash(safe)+".yaml")
}

// DirShardPathFor 返回目录节点分片路径:tree/<目录>/_dir.yaml。不安全返回 ""。
func (s *Store) DirShardPathFor(dirRel string) string {
	safe, ok := model.SafeRel(strings.TrimSuffix(dirRel, "/"))
	if !ok {
		return ""
	}
	return filepath.Join(s.dir, "tree", filepath.FromSlash(safe), "_dir.yaml")
}

// ProjectShardPath 返回项目节点分片路径。
func (s *Store) ProjectShardPath() string {
	return filepath.Join(s.dir, "project.yaml")
}

// SrcRelOfShard 由分片路径反推源文件相对路径(正斜杠);非 tree 分片返回 ""。
func (s *Store) SrcRelOfShard(shardPath string) string {
	rel, err := filepath.Rel(filepath.Join(s.dir, "tree"), shardPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	rel = filepath.ToSlash(rel)
	if base := filepath.Base(rel); base == "_dir.yaml" {
		return ""
	}
	return strings.TrimSuffix(rel, ".yaml")
}

// WalkTreeShards 遍历全部 tree 分片(含 _dir.yaml),回调收绝对路径。
func (s *Store) WalkTreeShards(fn func(path string) error) error {
	root := filepath.Join(s.dir, "tree")
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil
	}
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return nil
		}
		return fn(path)
	})
}

// ---- git 配套文件(impl §6 第 6 步;由 init 幂等生成,kb_status 校验在位) ----

var gitattributesLines = []string{"journal/*.jsonl merge=union"}
var gitignoreLines = []string{"local/", "wip/", "*.tmp"}

// EnsureGitFiles 幂等生成/补齐 .knowledge/.gitattributes 与 .knowledge/.gitignore。
func (s *Store) EnsureGitFiles() error {
	if err := ensureLines(filepath.Join(s.dir, ".gitattributes"), gitattributesLines); err != nil {
		return err
	}
	return ensureLines(filepath.Join(s.dir, ".gitignore"), gitignoreLines)
}

// GitFilesOK 校验两文件在位且行齐全(kb_status 用;用户手删后 union 会静默失效)。
func (s *Store) GitFilesOK() bool {
	return linesPresent(filepath.Join(s.dir, ".gitattributes"), gitattributesLines) &&
		linesPresent(filepath.Join(s.dir, ".gitignore"), gitignoreLines)
}

func ensureLines(path string, want []string) error {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("store: 读 %s: %w", filepath.Base(path), err)
	}
	have := map[string]bool{}
	for line := range strings.SplitSeq(string(existing), "\n") {
		have[strings.TrimSpace(line)] = true
	}
	var missing []string
	for _, w := range want {
		if !have[w] {
			missing = append(missing, w)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	content := string(existing)
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += strings.Join(missing, "\n") + "\n"
	return atomicWrite(path, []byte(content))
}

func linesPresent(path string, want []string) bool {
	existing, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	have := map[string]bool{}
	for line := range strings.SplitSeq(string(existing), "\n") {
		have[strings.TrimSpace(line)] = true
	}
	for _, w := range want {
		if !have[w] {
			return false
		}
	}
	return true
}

// atomicWrite 所有写入的唯一出口:同目录 temp + fsync + os.Rename + 目录 fsync
// (impl §4 定案)。temp 名匹配 .gitignore 的 *.tmp,崩溃残留不会进 git。
// fsync 缺席时 rename 可能先于数据落盘持久化,掉电会留下空文件/半文件——
// 分片是知识唯一真相,不接受;写频率是 agent 工具调用级,毫秒级 fsync 可承受。
func atomicWrite(path string, data []byte) error {
	if path == "" {
		return fmt.Errorf("store: 拒绝写空路径(疑似不安全的节点 ID/文件路径,铁律二)")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("store: 建目录: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "*.tmp")
	if err != nil {
		return fmt.Errorf("store: 建临时文件: %w", err)
	}
	// R29-E7.1 安全注释:os.CreateTemp 用 O_EXCL 防覆盖,但会跟随已存在的 symlink。
	// 防线是 EnsureLayout 创建 .knowledge/ 的 0755 权限——同机其他用户无可写权限,
	// 无法在 .knowledge/tree/ 内植入恶意 symlink。若将来放松目录权限(如共享组 0775),
	// 必须改用 O_NOFOLLOW 打开临时文件,否则有 symlink 攻击面。
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("store: 写临时文件: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("store: chmod: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("store: fsync 临时文件: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("store: 关临时文件: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("store: rename: %w", err)
	}
	// rename 产生的目录项更新要持久化——平台差异见 fsyncdir_*.go
	//(unix:fsync 父目录;windows:NTFS 日志兜底,句柄语义拿不到,留痕降级)。
	if err := fsyncDir(dir); err != nil {
		return fmt.Errorf("store: fsync 目录: %w", err)
	}
	return nil
}
