//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// detachProc 让子进程脱离本会话(自成会话首领):stdio 桥随客户端退出时,
// 拉起的 serve 不被连坐,留给 hook/只读腿/后续会话复用。
func detachProc(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}
