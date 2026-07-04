//go:build windows

package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

// ErrLocked 表示写者锁被占(impl §4 写者互斥定案)。
var ErrLocked = errors.New("serve 运行中,请改用 kb_init 或先停 serve")

// kernel32 文件锁(impl §1 Windows 修订,原排四期):stdlib syscall 不导出
// LockFileEx,经 LazyDLL 直取——仍零第三方依赖(铁律一)。
var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

const (
	lockfileFailImmediately = 0x1 // LOCKFILE_FAIL_IMMEDIATELY
	lockfileExclusiveLock   = 0x2 // LOCKFILE_EXCLUSIVE_LOCK
	errorLockViolation      = syscall.Errno(33)
)

// AcquireWriterLock Windows 实现:LockFileEx 排他 + 立即失败,语义对齐 unix 侧
// flock(LOCK_EX|LOCK_NB)——进程退出/句柄关闭自动释放,无残锁问题。
func (s *Store) AcquireWriterLock() (release func(), err error) {
	path := filepath.Join(s.dir, "local", ".lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("store: 建 local 目录: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("store: 开锁文件: %w", err)
	}
	ol := new(syscall.Overlapped)
	r, _, errno := procLockFileEx.Call(f.Fd(),
		uintptr(lockfileExclusiveLock|lockfileFailImmediately),
		0, 1, 0, uintptr(unsafe.Pointer(ol)))
	if r == 0 {
		f.Close()
		if errors.Is(errno, errorLockViolation) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("store: LockFileEx: %w", errno)
	}
	return func() {
		procUnlockFileEx.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(ol)))
		f.Close()
	}, nil
}
