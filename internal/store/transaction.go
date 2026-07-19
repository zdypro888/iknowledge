package store

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// TruthTransaction 是跨多个 .knowledge 真相文件的崩溃恢复事务。
//
// prepared intent 保存在仓外的用户私有状态目录，完整记录每个目标文件的
// 原始字节（或原本不存在）。全部真相写成功后另写一个很小的 committed marker；
// marker 与 intent 分离，避免提交一个大 import 时为了改状态再重写数百 MiB 快照。
// 恢复规则只有两条：无 marker 的 prepared 全量回滚；有 marker 的事务保留真相、
// 只清理 WAL。intent 先删、marker 后删，因此清理中途崩溃留下 marker-only 也安全。
type TruthTransaction struct {
	store  *Store
	intent truthTransactionIntent
	done   bool
}

const (
	truthTransactionSchema     = 1
	truthTransactionIntentFile = "transaction-v1.json"
	truthTransactionCommitFile = "transaction-v1.commit"
	truthTransactionMaxFiles   = 10000
	truthTransactionMaxBytes   = 256 << 20
	// []byte 的 JSON base64 膨胀约 4/3；再给路径与结构开销留出有界余量。
	truthTransactionMaxJSONBytes = 350 << 20
)

type truthTransactionIntent struct {
	Schema  int                    `json:"schema"`
	State   string                 `json:"state"`
	ID      string                 `json:"id"`
	RepoKey string                 `json:"repo_key"`
	Files   []truthTransactionFile `json:"files"`
}

type truthTransactionFile struct {
	Rel    string `json:"rel"`
	Exists bool   `json:"exists"`
	Data   []byte `json:"data,omitempty"`
}

type truthTransactionCommit struct {
	Schema  int    `json:"schema"`
	State   string `json:"state"`
	ID      string `json:"id"`
	RepoKey string `json:"repo_key"`
}

// PrepareTruthTransaction 在任何真相写之前，持久化所有目标文件的 before-image。
// 同一仓库同时只允许一个 intent；调用方必须先经 RecoverTruthTransaction 清理
// 上次进程遗留状态，再开始新事务。
func (s *Store) PrepareTruthTransaction(rels []string) (*TruthTransaction, error) {
	intentPath, commitPath, repoKey, err := s.truthTransactionPaths()
	if err != nil {
		return nil, err
	}
	for _, path := range []string{intentPath, commitPath} {
		if _, err := os.Lstat(path); err == nil {
			return nil, fmt.Errorf("store: 已有未恢复的事务 WAL，拒绝覆盖: %s", path)
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("store: 检查事务 WAL: %w", err)
		}
	}

	ordered := append([]string(nil), rels...)
	sort.Strings(ordered)
	if len(ordered) == 0 || len(ordered) > truthTransactionMaxFiles {
		return nil, fmt.Errorf("store: 事务文件数必须在 1..%d，实际 %d", truthTransactionMaxFiles, len(ordered))
	}
	id, err := newTruthTransactionID()
	if err != nil {
		return nil, fmt.Errorf("store: 生成事务 ID: %w", err)
	}
	intent := truthTransactionIntent{
		Schema: truthTransactionSchema, State: "prepared", ID: id, RepoKey: repoKey,
		Files: make([]truthTransactionFile, 0, len(ordered)),
	}
	var total int64
	for i, rel := range ordered {
		if i > 0 && rel == ordered[i-1] {
			return nil, fmt.Errorf("store: 事务目标重复: %s", rel)
		}
		if err := validateTruthTransactionRel(rel); err != nil {
			return nil, err
		}
		data, err := s.ReadKnowledgeFile(rel)
		switch {
		case err == nil:
			total += int64(len(data))
			if total > truthTransactionMaxBytes {
				return nil, fmt.Errorf("store: 事务 before-image 超过 %d MiB 上限", truthTransactionMaxBytes>>20)
			}
			intent.Files = append(intent.Files, truthTransactionFile{Rel: rel, Exists: true, Data: data})
		case os.IsNotExist(err):
			intent.Files = append(intent.Files, truthTransactionFile{Rel: rel})
		default:
			return nil, fmt.Errorf("store: 读取事务 before-image %s: %w", rel, err)
		}
	}
	data, err := json.Marshal(intent)
	if err != nil {
		return nil, fmt.Errorf("store: 编码事务 intent: %w", err)
	}
	if len(data) > truthTransactionMaxJSONBytes {
		return nil, fmt.Errorf("store: 事务 intent 超过 %d MiB 上限", truthTransactionMaxJSONBytes>>20)
	}
	data = append(data, '\n')
	if err := writePrivateStateFile(intentPath, data); err != nil {
		return nil, fmt.Errorf("store: 持久化 prepared 事务 intent: %w", err)
	}
	return &TruthTransaction{store: s, intent: intent}, nil
}

// Commit 原子写 committed marker 后清理 WAL。committed=true 表示真相已经提交，
// 即使清理报错也绝不能再 Abort/重试业务操作；下次 reload 会继续幂等清理。
func (tx *TruthTransaction) Commit() (committed bool, err error) {
	if tx == nil || tx.store == nil {
		return false, fmt.Errorf("store: 空事务")
	}
	if tx.done {
		return false, fmt.Errorf("store: 事务已经结束")
	}
	intentPath, commitPath, repoKey, err := tx.store.truthTransactionPaths()
	if err != nil {
		return false, err
	}
	if tx.intent.RepoKey != repoKey {
		return false, fmt.Errorf("store: 事务仓库身份不匹配")
	}
	marker := truthTransactionCommit{
		Schema: truthTransactionSchema, State: "committed", ID: tx.intent.ID, RepoKey: repoKey,
	}
	data, err := json.Marshal(marker)
	if err != nil {
		return false, err
	}
	data = append(data, '\n')
	if writeErr := writePrivateStateFile(commitPath, data); writeErr != nil {
		// atomic rename 后的目录 fsync 可能单独报错。marker 若已完整可读，事务
		// 已越过不可回滚点；返回 committed=true，防调用方把已提交真相撤掉。
		got, readErr := readTruthTransactionCommit(commitPath, repoKey)
		if readErr != nil || got.ID != marker.ID {
			return false, fmt.Errorf("store: 持久化 committed marker: %w", writeErr)
		}
		tx.done = true
		cleanupErr := tx.store.cleanupCommittedTruthTransaction(intentPath, commitPath)
		return true, errors.Join(fmt.Errorf("store: committed marker 持久性报告失败（事务视为已提交）: %w", writeErr), cleanupErr)
	}
	tx.done = true
	return true, tx.store.cleanupCommittedTruthTransaction(intentPath, commitPath)
}

// Abort 将全部目标恢复到 Prepare 时的原始状态。恢复完成前始终保留 intent；
// 因此进程在回滚中再次崩溃，下次 reload 会继续幂等回滚。
func (tx *TruthTransaction) Abort() error {
	if tx == nil || tx.store == nil {
		return fmt.Errorf("store: 空事务")
	}
	if tx.done {
		return fmt.Errorf("store: 事务已经结束")
	}
	intentPath, commitPath, repoKey, err := tx.store.truthTransactionPaths()
	if err != nil {
		return err
	}
	if marker, markerErr := readTruthTransactionCommit(commitPath, repoKey); markerErr == nil {
		if marker.ID == tx.intent.ID {
			return fmt.Errorf("store: 事务已 committed，拒绝回滚")
		}
		return fmt.Errorf("store: committed marker 与当前事务不匹配")
	} else if !os.IsNotExist(markerErr) {
		return fmt.Errorf("store: 检查 committed marker: %w", markerErr)
	}
	if err := tx.store.restoreTruthTransaction(tx.intent); err != nil {
		return err
	}
	if err := removePrivateStateFile(intentPath); err != nil {
		return fmt.Errorf("store: 清理已回滚事务 intent: %w", err)
	}
	tx.done = true
	return nil
}

// RecoverTruthTransaction 在缓存加载之前恢复崩溃遗留事务。
func (s *Store) RecoverTruthTransaction() error {
	_, err := s.RecoverTruthTransactionWithStatus()
	return err
}

// RecoverTruthTransactionWithStatus 与 RecoverTruthTransaction 相同，并报告本次
// 是否发现/处理了 WAL。Engine 用该信号丢弃旧 mtime cache：恢复后的原始文件
// 可能恰好与半应用文件同 size/mtime，只靠增量对账不能证明缓存仍正确。
// 任何未知 schema、非白名单路径、容量越界、恢复或清理错误都 fail closed。
func (s *Store) RecoverTruthTransactionWithStatus() (recovered bool, err error) {
	recovered, err = s.recoverTruthTransactionWithStatusHeld()
	if !errors.Is(err, ErrWriterLockRequired) {
		return recovered, err
	}
	// 没有外层 owner 时尝试自己短持 writer lock。若 live serve/另一 CLI
	// 正在写，Acquire 会立即返回 ErrLocked，绝不会把活事务误当崩溃恢复。
	release, lockErr := s.AcquireWriterLock()
	if lockErr != nil {
		return false, lockErr
	}
	defer release()
	return s.recoverTruthTransactionWithStatusHeld()
}

func (s *Store) recoverTruthTransactionWithStatusHeld() (recovered bool, err error) {
	intentPath, commitPath, repoKey, err := s.truthTransactionPaths()
	if err != nil {
		return false, err
	}
	intentExists, err := privateStatePathExists(intentPath)
	if err != nil {
		return false, err
	}
	markerExists, err := privateStatePathExists(commitPath)
	if err != nil {
		return false, err
	}
	if !intentExists && !markerExists {
		return false, nil
	}
	if !s.writerLockHeldNow() {
		return false, ErrWriterLockRequired
	}
	intent, intentErr := readTruthTransactionIntent(intentPath, repoKey)
	marker, markerErr := readTruthTransactionCommit(commitPath, repoKey)
	intentMissing := os.IsNotExist(intentErr)
	markerMissing := os.IsNotExist(markerErr)
	if !intentMissing && intentErr != nil {
		return false, fmt.Errorf("store: 读取事务 intent: %w", intentErr)
	}
	if !markerMissing && markerErr != nil {
		return false, fmt.Errorf("store: 读取 committed marker: %w", markerErr)
	}
	switch {
	case intentMissing && markerMissing:
		return false, nil
	case intentMissing && !markerMissing:
		// committed 清理在 intent fsync 删除后崩溃；marker-only 只需续清。
		if err := removePrivateStateFile(commitPath); err != nil {
			return true, fmt.Errorf("store: 清理 marker-only 事务: %w", err)
		}
		return true, nil
	case !intentMissing && !markerMissing:
		if intent.ID != marker.ID || intent.RepoKey != marker.RepoKey {
			return true, fmt.Errorf("store: 事务 intent/marker 身份不匹配，拒绝猜测")
		}
		return true, s.cleanupCommittedTruthTransaction(intentPath, commitPath)
	default: // prepared intent，无 committed marker
		if err := s.restoreTruthTransaction(intent); err != nil {
			return true, fmt.Errorf("store: 恢复 prepared 事务: %w", err)
		}
		if err := removePrivateStateFile(intentPath); err != nil {
			return true, fmt.Errorf("store: 清理已恢复 intent: %w", err)
		}
		return true, nil
	}
}

func privateStatePathExists(path string) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, fmt.Errorf("本机事务状态必须是普通文件: %s", path)
	}
	return true, nil
}

func (s *Store) restoreTruthTransaction(intent truthTransactionIntent) error {
	var errs []error
	for i := len(intent.Files) - 1; i >= 0; i-- {
		file := intent.Files[i]
		if file.Exists {
			if err := s.WriteKnowledgeFile(file.Rel, file.Data); err != nil {
				errs = append(errs, fmt.Errorf("恢复 %s: %w", file.Rel, err))
			}
			continue
		}
		if err := s.RemoveKnowledgeFile(file.Rel); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("删除事务中新文件 %s: %w", file.Rel, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Store) cleanupCommittedTruthTransaction(intentPath, commitPath string) error {
	// 顺序不可交换：先删 marker 会留下“prepared”并在重启时误回滚已提交真相。
	if err := removePrivateStateFile(intentPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("store: 清理 committed intent: %w", err)
	}
	if err := removePrivateStateFile(commitPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("store: 清理 committed marker: %w", err)
	}
	return nil
}

func (s *Store) truthTransactionPaths() (intentPath, commitPath, repoKey string, err error) {
	intentPath, err = s.privateStatePath(truthTransactionIntentFile)
	if err != nil {
		return "", "", "", err
	}
	commitPath, err = s.privateStatePath(truthTransactionCommitFile)
	if err != nil {
		return "", "", "", err
	}
	dir, err := s.PrivateStateDir()
	if err != nil {
		return "", "", "", err
	}
	repoKey = filepath.Base(filepath.Clean(dir))
	if len(repoKey) != 64 {
		return "", "", "", fmt.Errorf("store: 私有状态仓库键异常")
	}
	if _, err := hex.DecodeString(repoKey); err != nil {
		return "", "", "", fmt.Errorf("store: 私有状态仓库键异常: %w", err)
	}
	return intentPath, commitPath, repoKey, nil
}

func readTruthTransactionIntent(path, repoKey string) (truthTransactionIntent, error) {
	var intent truthTransactionIntent
	data, err := readBoundedPrivateStateFile(path, truthTransactionMaxJSONBytes)
	if err != nil {
		return intent, err
	}
	if err := decodeStrictJSON(data, &intent); err != nil {
		return intent, fmt.Errorf("事务 intent JSON 非法: %w", err)
	}
	if err := validateTruthTransactionIntent(intent, repoKey); err != nil {
		return intent, err
	}
	return intent, nil
}

func readTruthTransactionCommit(path, repoKey string) (truthTransactionCommit, error) {
	var marker truthTransactionCommit
	data, err := readBoundedPrivateStateFile(path, 4096)
	if err != nil {
		return marker, err
	}
	if err := decodeStrictJSON(data, &marker); err != nil {
		return marker, fmt.Errorf("committed marker JSON 非法: %w", err)
	}
	if marker.Schema != truthTransactionSchema || marker.State != "committed" || marker.RepoKey != repoKey || !validTruthTransactionID(marker.ID) {
		return marker, fmt.Errorf("committed marker schema/state/id/repo_key 非法")
	}
	return marker, nil
}

func validateTruthTransactionIntent(intent truthTransactionIntent, repoKey string) error {
	if intent.Schema != truthTransactionSchema {
		return fmt.Errorf("事务 intent schema %d 未知", intent.Schema)
	}
	if intent.State != "prepared" || !validTruthTransactionID(intent.ID) || intent.RepoKey != repoKey {
		return fmt.Errorf("事务 intent state/id/repo_key 非法")
	}
	if len(intent.Files) == 0 || len(intent.Files) > truthTransactionMaxFiles {
		return fmt.Errorf("事务 intent 文件数越界")
	}
	var total int64
	prev := ""
	for i, file := range intent.Files {
		if err := validateTruthTransactionRel(file.Rel); err != nil {
			return err
		}
		if i > 0 && file.Rel <= prev {
			return fmt.Errorf("事务 intent 路径必须严格排序且不重复")
		}
		prev = file.Rel
		if !file.Exists && len(file.Data) != 0 {
			return fmt.Errorf("事务 intent 不存在文件却携带 data: %s", file.Rel)
		}
		total += int64(len(file.Data))
		if total > truthTransactionMaxBytes {
			return fmt.Errorf("事务 intent before-image 容量越界")
		}
	}
	return nil
}

func validateTruthTransactionRel(rel string) error {
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	if rel == "" || len(rel) > 4096 || !utf8.ValidString(rel) || rel != clean ||
		strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, "../") ||
		strings.Contains(rel, "\\") || strings.Contains(rel, ":") {
		return fmt.Errorf("store: 事务路径非法: %q", rel)
	}
	if rel == "project.yaml" || rel == "config.yaml" {
		return nil
	}
	if strings.HasPrefix(rel, "tree/") && strings.HasSuffix(rel, ".yaml") && len(rel) > len("tree/.yaml") {
		return nil
	}
	// flow/topic 的正式运行时布局是一对象一文件、目录顶层 YAML。
	for _, prefix := range []string{"flows/", "topics/"} {
		if strings.HasPrefix(rel, prefix) && strings.Count(rel, "/") == 1 && strings.HasSuffix(rel, ".yaml") {
			name := strings.TrimSuffix(strings.TrimPrefix(rel, prefix), ".yaml")
			if safeFlowName(name) {
				return nil
			}
		}
	}
	parts := strings.Split(rel, "/")
	if len(parts) != 2 || parts[1] == "" || parts[1] == ".yaml" {
		return fmt.Errorf("store: 事务路径不在白名单: %q", rel)
	}
	switch parts[0] {
	case "journal":
		if !strings.HasSuffix(parts[1], ".jsonl") {
			break
		}
		month := strings.TrimSuffix(parts[1], ".jsonl")
		if len(month) == 7 {
			if parsed, err := time.Parse("2006-01", month); err == nil && parsed.Format("2006-01") == month {
				return nil
			}
		}
	case "wip":
		if strings.HasSuffix(parts[1], ".yaml") {
			return nil
		}
	}
	return fmt.Errorf("store: 事务路径不在白名单: %q", rel)
}

func readBoundedPrivateStateFile(path string, max int) ([]byte, error) {
	if err := ensurePrivateStateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("本机事务状态必须是普通文件: %s", path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("本机事务状态权限必须是 0600: %s", path)
	}
	if info.Size() < 0 || info.Size() > int64(max) {
		return nil, fmt.Errorf("本机事务状态超过 %d 字节上限: %s", max, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > max {
		return nil, fmt.Errorf("本机事务状态超过 %d 字节上限: %s", max, path)
	}
	return data, nil
}

func removePrivateStateFile(path string) error {
	if err := ensurePrivateStateDir(filepath.Dir(path)); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("本机事务状态必须是普通文件: %s", path)
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return fsyncDir(filepath.Dir(path))
}

func decodeStrictJSON(data []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("存在额外 JSON 值")
		}
		return err
	}
	return nil
}

func newTruthTransactionID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func validTruthTransactionID(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 16
}
