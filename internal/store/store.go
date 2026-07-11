// Package store 实现 .knowledge/ 的文件存储(impl §4):分片读写(未知字段往返保留)、
// journal 追加与读端契约、原子写、写者互斥锁、配置。文件是唯一真相,本包不含业务规则。
//
// 铁律二:本包永不写源码。仓库内容只写 .knowledge/；唯一仓外例外是按
// canonical repo 分仓的用户私有认证/信任/崩溃恢复状态（见 auth.go/transaction.go）。
package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/zdypro888/iknowledge/internal/model"
)

// KnowledgeDir 是知识库目录名。
const KnowledgeDir = ".knowledge"

// ErrSymlinkPath 表示 .knowledge 本身或目标路径的任一既存组件是符号链接。
// 知识库允许仓库根本身经 symlink 打开，但从 .knowledge 起一律不跟随链接：
// 否则恶意仓库可把 tree/local/journal 指到仓外，突破“只写 .knowledge”边界。
var ErrSymlinkPath = errors.New("knowledge 路径包含符号链接")

// Store 是一个仓库的 .knowledge/ 存取器。
type Store struct {
	repo       string // 仓库根(绝对路径)
	dir        string // <repo>/.knowledge
	writerMu   sync.Mutex
	writerHeld bool
}

// ErrWriterLockRequired 表示发现需要恢复的 WAL，但当前 Store 并不持有仓库
// writer lock。恢复会改写多个真相文件，绝不能与活跃 serve 事务并发。
var ErrWriterLockRequired = errors.New("恢复事务 WAL 前必须持有 writer lock")

func (s *Store) setWriterLockHeld(held bool) {
	s.writerMu.Lock()
	s.writerHeld = held
	s.writerMu.Unlock()
}

func (s *Store) writerLockHeldNow() bool {
	s.writerMu.Lock()
	defer s.writerMu.Unlock()
	return s.writerHeld
}

// Open 打开(不创建)仓库的存取器。
func Open(repoRoot string) (*Store, error) {
	abs, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("store: 解析仓库路径: %w", err)
	}
	s := &Store{repo: abs, dir: filepath.Join(abs, KnowledgeDir)}
	// 已存在的 .knowledge 必须是真目录，不能让 Open 后的首个读写就越界。
	if err := s.checkKnowledgePath(s.dir); err != nil {
		return nil, err
	}
	return s, nil
}

// RepoRoot 返回仓库根绝对路径。
func (s *Store) RepoRoot() string { return s.repo }

// Dir 返回 .knowledge 绝对路径。
func (s *Store) Dir() string { return s.dir }

// Initialized 判断库是否已初始化(存在 .knowledge/project.yaml)。
func (s *Store) Initialized() bool {
	if err := s.checkKnowledgePath(s.ProjectShardPath()); err != nil {
		return false
	}
	_, err := os.Stat(s.ProjectShardPath())
	return err == nil
}

// knowledgePath 把严格的正斜杠相对路径映射到 .knowledge 内。
func (s *Store) knowledgePath(rel string) (string, error) {
	rel = filepath.ToSlash(rel)
	cleanRel := filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	if rel == "" || rel != cleanRel || rel == "." || strings.HasPrefix(rel, "/") ||
		strings.HasPrefix(rel, "../") || strings.Contains(rel, "\\") || strings.Contains(rel, ":") {
		return "", fmt.Errorf("store: 非法 knowledge 路径 %q", rel)
	}
	target := filepath.Join(s.dir, filepath.FromSlash(rel))
	if err := s.checkKnowledgePath(target); err != nil {
		return "", err
	}
	return target, nil
}

// WriteKnowledgeFile 原子写 .knowledge/ 内的相对路径。用于导入等跨分片写入:
// 仍然复用 store 的 temp+fsync+rename 写入纪律,且拒绝路径逃逸。
func (s *Store) WriteKnowledgeFile(rel string, data []byte) error {
	target, err := s.knowledgePath(rel)
	if err != nil {
		return err
	}
	return s.atomicWrite(target, data)
}

// WritePrivateKnowledgeFile 与 WriteKnowledgeFile 相同，但目标权限固定 0600，
// 用于包含 token 的临时 MCP 配置等本机秘密。
func (s *Store) WritePrivateKnowledgeFile(rel string, data []byte) error {
	target, err := s.knowledgePath(rel)
	if err != nil {
		return err
	}
	return s.atomicWriteMode(target, data, 0o600)
}

// ReadKnowledgeFile 安全读取 .knowledge 内的相对路径；与写入口共用同一套
// 词法边界和 symlink 拒绝规则，供本地信任凭据等小文件使用。
func (s *Store) ReadKnowledgeFile(rel string) ([]byte, error) {
	target, err := s.knowledgePath(rel)
	if err != nil {
		return nil, err
	}
	return s.readKnowledgeFile(target)
}

// CreateKnowledgeFile 安全创建/截断 .knowledge 内文件，供需要流式写入的
// 有界本地日志使用。调用方负责 Close。
func (s *Store) CreateKnowledgeFile(rel string, perm os.FileMode) (*os.File, error) {
	target, err := s.knowledgePath(rel)
	if err != nil {
		return nil, err
	}
	return s.openKnowledgeFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
}

// OpenKnowledgeLog 安全打开 .knowledge 内的追加日志。调用方负责 Close。
func (s *Store) OpenKnowledgeLog(rel string, perm os.FileMode) (*os.File, error) {
	target, err := s.knowledgePath(rel)
	if err != nil {
		return nil, err
	}
	return s.openKnowledgeFile(target, os.O_CREATE|os.O_WRONLY|os.O_APPEND, perm)
}

// RemoveKnowledgeFile 安全删除 .knowledge 内文件；最终 symlink 也拒绝，
// 避免清理逻辑与其他写入口形成不同的边界语义。
func (s *Store) RemoveKnowledgeFile(rel string) error {
	target, err := s.knowledgePath(rel)
	if err != nil {
		return err
	}
	return s.removeKnowledgeFile(target)
}

// EnsureLayout 幂等创建目录布局(knowledge.md §11.4):
// tree/、journal/、wip/、local/、flows/、topics/(后三类一期只建目录不实现逻辑)。
func (s *Store) EnsureLayout() error {
	for _, d := range []string{"", "tree", "journal", "wip", "local", "flows", "topics"} {
		if err := s.ensureKnowledgeDir(filepath.Join(s.dir, d), 0o755); err != nil {
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
	if err := s.checkKnowledgePath(root); err != nil {
		return err
	}
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
	if err := s.ensureLines(filepath.Join(s.dir, ".gitattributes"), gitattributesLines); err != nil {
		return err
	}
	return s.ensureLines(filepath.Join(s.dir, ".gitignore"), gitignoreLines)
}

// GitFilesOK 校验两文件在位且行齐全(kb_status 用;用户手删后 union 会静默失效)。
func (s *Store) GitFilesOK() bool {
	return s.linesPresent(filepath.Join(s.dir, ".gitattributes"), gitattributesLines) &&
		s.linesPresent(filepath.Join(s.dir, ".gitignore"), gitignoreLines)
}

func (s *Store) ensureLines(path string, want []string) error {
	existing, err := s.readKnowledgeFile(path)
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
	return s.atomicWrite(path, []byte(content))
}

func (s *Store) linesPresent(path string, want []string) bool {
	existing, err := s.readKnowledgeFile(path)
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
func (s *Store) atomicWrite(path string, data []byte) error {
	return s.atomicWriteMode(path, data, 0o644)
}

func (s *Store) atomicWriteMode(path string, data []byte, perm os.FileMode) error {
	if path == "" {
		return fmt.Errorf("store: 拒绝写空路径(疑似不安全的节点 ID/文件路径,铁律二)")
	}
	dir := filepath.Dir(path)
	if err := s.ensureKnowledgeDir(dir, 0o755); err != nil {
		return fmt.Errorf("store: 建目录: %w", err)
	}
	if err := s.checkKnowledgePath(path); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "*.tmp")
	if err != nil {
		return fmt.Errorf("store: 建临时文件: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("store: 写临时文件: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
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
	// 写入期间再次校验父目录与目标；静态恶意链接在创建 temp 前已被挡，
	// 此处也避免正常并发操作把刚校验过的目标换成链接后继续提交。
	if err := s.checkKnowledgePath(dir); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := s.checkKnowledgePath(path); err != nil {
		os.Remove(tmpName)
		return err
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

// checkKnowledgePath 校验 path 词法位于 .knowledge 内，并拒绝从 .knowledge
// 开始的任一既存 symlink。缺失组件允许存在，由安全创建函数逐层落盘。
func (s *Store) checkKnowledgePath(path string) error {
	root := filepath.Clean(s.dir)
	target := filepath.Clean(path)
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("store: knowledge 路径逃逸 %q", path)
	}
	components := []string{root}
	if rel != "." {
		cur := root
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			cur = filepath.Join(cur, part)
			components = append(components, cur)
		}
	}
	for i, component := range components {
		info, err := os.Lstat(component)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("store: lstat %s: %w", component, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("store: %w: %s", ErrSymlinkPath, component)
		}
		if (i == 0 || i < len(components)-1) && !info.IsDir() {
			return fmt.Errorf("store: 路径组件不是目录: %s", component)
		}
	}
	return nil
}

// ensureKnowledgeDir 不用 MkdirAll（它会静默跟随 symlink），而是逐组件
// lstat + mkdir；创建后再校验类型，所有平台只依赖 os 标准库。
func (s *Store) ensureKnowledgeDir(dir string, perm os.FileMode) error {
	root := filepath.Clean(s.dir)
	target := filepath.Clean(dir)
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("store: knowledge 目录逃逸 %q", dir)
	}
	components := []string{root}
	if rel != "." {
		cur := root
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			cur = filepath.Join(cur, part)
			components = append(components, cur)
		}
	}
	for _, component := range components {
		info, err := os.Lstat(component)
		if os.IsNotExist(err) {
			if err := os.Mkdir(component, perm); err != nil && !os.IsExist(err) {
				return err
			}
			info, err = os.Lstat(component)
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("store: %w: %s", ErrSymlinkPath, component)
		}
		if !info.IsDir() {
			return fmt.Errorf("store: 路径组件不是目录: %s", component)
		}
	}
	return nil
}

func (s *Store) readKnowledgeFile(path string) ([]byte, error) {
	if err := s.checkKnowledgePath(path); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

func (s *Store) readKnowledgeDir(path string) ([]os.DirEntry, error) {
	if err := s.checkKnowledgePath(path); err != nil {
		return nil, err
	}
	return os.ReadDir(path)
}

// openKnowledgeFile 是追加/锁文件的统一安全出口。OpenFile 会跟随最终
// symlink，因此必须在打开前同时校验父链和最终文件。
func (s *Store) openKnowledgeFile(path string, flags int, perm os.FileMode) (*os.File, error) {
	if err := s.ensureKnowledgeDir(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := s.checkKnowledgePath(path); err != nil {
		return nil, err
	}
	return os.OpenFile(path, flags, perm)
}

func (s *Store) appendKnowledgeFile(path string, data []byte, perm os.FileMode, syncData bool) error {
	f, err := s.openKnowledgeFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	var syncErr error
	if syncData {
		syncErr = f.Sync()
	}
	closeErr := f.Close()
	var dirErr error
	if syncData {
		// 首条月 journal 可能刚创建；只同步文件数据并不保证目录项掉电后
		// 仍存在。即使 Sync/Close 已报错也尝试目录 fsync，让调用方可通过
		// 读回 change ID 判断 append 是否实际提交。
		dirErr = fsyncDir(filepath.Dir(path))
	}
	return errors.Join(syncErr, closeErr, dirErr)
}

func (s *Store) removeKnowledgeFile(path string) error {
	if err := s.checkKnowledgePath(path); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	// 删除 remap 源分片等真相文件时，目录项也必须持久；否则掉电可让已删
	// 节点复活。local/WIP 多一次 fsync 可接受，换取统一可靠语义。
	return fsyncDir(filepath.Dir(path))
}
