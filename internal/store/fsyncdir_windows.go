//go:build windows

package store

// fsyncDir Windows 降级为空操作(impl §1 留痕):Windows 上对目录句柄
// FlushFileBuffers 需要 GENERIC_WRITE 打开目录,os.Open 拿不到;NTFS 以元数据日志
// 保障 rename 的目录项一致性,MoveFileEx 替换语义(Go os.Rename 内置)本身原子。
// 掉电窗口内可能丢"最新一次 rename 可见性"但不产生半文件——与内容 fsync(仍在)
// 相比是可接受的降级,不是静默省略。
// R29-E7.2:build tag 从 !unix 收窄为 windows——非 unix 非 windows 平台(若有)
// 不该静默谎称耐久性达成,见 fsyncdir_unknown.go。
func fsyncDir(string) error { return nil }
