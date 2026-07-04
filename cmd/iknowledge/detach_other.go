//go:build !unix

package main

import "os/exec"

// detachProc 非 unix 平台:Windows 下 Start 出的进程本就不随父进程退出连坐,
// 无需额外脱离(不设 CREATE_NEW_PROCESS_GROUP,保持零 syscall 面)。
func detachProc(*exec.Cmd) {}
