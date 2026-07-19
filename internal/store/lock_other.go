//go:build !unix && !windows

package store

import "errors"

// ErrLocked 表示写者锁被占(impl §4 写者互斥定案)。
var ErrLocked = errors.New("serve 运行中,请改用 kb_init 或先停 serve")

// AcquireWriterLock 在 unix/windows 之外的平台不可用(impl §1 平台定案:
// macOS/Linux 一期,Windows 2026-07-04 修订落地;其余平台无锁原语适配)。
func (s *Store) AcquireWriterLock() (release func(), err error) {
	return nil, errors.New("store: 本平台暂不支持写者锁(支持 macOS/Linux/Windows,impl §1)")
}

func (s *Store) AcquireSemanticLock() (release func(), err error) {
	return nil, errors.New("store: 本平台暂不支持 semantic 锁(支持 macOS/Linux/Windows)")
}

func (s *Store) AcquireSemanticConfigReadLock() (release func(), err error) {
	return nil, errors.New("store: 本平台暂不支持 semantic 配置锁(支持 macOS/Linux/Windows)")
}

func (s *Store) AcquireSemanticConfigWriteLock() (release func(), err error) {
	return nil, errors.New("store: 本平台暂不支持 semantic 配置锁(支持 macOS/Linux/Windows)")
}
