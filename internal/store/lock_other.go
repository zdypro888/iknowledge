//go:build !unix

package store

import "errors"

// ErrLocked 表示写者锁被占(impl §4 写者互斥定案)。
var ErrLocked = errors.New("serve 运行中,请改用 kb_init 或先停 serve")

// AcquireWriterLock 在非 unix 平台不可用:一期仅支持 macOS/Linux,
// Windows(os.Rename 覆盖语义、flock)排四期(impl §1 平台定案)。
func (s *Store) AcquireWriterLock() (release func(), err error) {
	return nil, errors.New("store: 本平台暂不支持写者锁(一期仅 macOS/Linux,impl §1)")
}
