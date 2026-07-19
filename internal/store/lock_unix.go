//go:build unix

package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// ErrLocked 表示写者锁被占(impl §4 写者互斥定案)。
var ErrLocked = errors.New("serve 运行中,请改用 kb_init 或先停 serve")

// AcquireWriterLock 对 .knowledge/local/.lock 取 flock 排他锁(非阻塞)。
// serve 启动时取并持有;CLI init 取不到即报错;第二个 serve 同样被挡。
// 人工直接编辑分片不受锁管(惰性重载兜住)。
func (s *Store) AcquireWriterLock() (release func(), err error) {
	path := filepath.Join(s.dir, "local", ".lock")
	f, err := s.openKnowledgeFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("store: 开锁文件: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("store: flock: %w", err)
	}
	s.setWriterLockHeld(true)
	var once sync.Once
	return func() {
		once.Do(func() {
			s.setWriterLockHeld(false)
			syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			f.Close()
		})
	}, nil
}
