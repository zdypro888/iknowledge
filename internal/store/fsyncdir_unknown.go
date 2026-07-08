//go:build !unix && !windows

package store

import "fmt"

// fsyncDir 非 unix 非 windows 平台(R29-E7.2):不静默谎称耐久性,如实报错。
// 对齐 lock_other.go 的立场——CI 只跑 mac/linux/windows,其他平台明确不支持。
func fsyncDir(_ string) error {
	return fmt.Errorf("store: 目录 fsync 在此平台未实现(仅支持 unix/windows)")
}
