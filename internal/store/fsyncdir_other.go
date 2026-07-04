//go:build !unix

package store

// fsyncDir 非 unix 平台为空操作(impl §1 Windows 修订留痕):Windows 上对目录句柄
// FlushFileBuffers 需要 GENERIC_WRITE 打开目录,os.Open 拿不到;NTFS 以元数据日志
// 保障 rename 的目录项一致性,MoveFileEx 替换语义(Go os.Rename 内置)本身原子。
// 掉电窗口内可能丢"最新一次 rename 可见性"但不产生半文件——与内容 fsync(仍在)
// 相比是可接受的降级,不是静默省略。
func fsyncDir(string) error { return nil }
