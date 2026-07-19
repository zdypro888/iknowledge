//go:build unix

package store

import "os"

// fsyncDir 使 rename 产生的目录项更新持久(POSIX:fsync 父目录;macOS/Linux 均支持)。
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}
