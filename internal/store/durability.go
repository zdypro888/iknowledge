package store

// SyncDir 持久化目录项更新。Unix 执行真实目录 fsync；Windows 按既有平台
// 契约由 fsyncDir 降级为 no-op（NTFS 元数据日志兜底）。
func SyncDir(dir string) error { return fsyncDir(dir) }
