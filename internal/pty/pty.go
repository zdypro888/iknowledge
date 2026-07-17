//go:build darwin || linux

// Package pty 提供最小 PTY 原语(impl §7.5 自派备模式,2026-07-04):
// 交互式 CLI(claude 等)拒绝在无 TTY 下运行,自派侦察兵需要一个伪终端壳。
// 不做终端仿真——交卷信号走 kb_submit_findings(协议级),输出只旁路进日志;
// 因此不需要 aibridge 的 vt10x/creack/pty 三方依赖(铁律一:零重依赖),
// 手写 openpt/grantpt/unlockpt 三步(约 40 行/平台,做法同 creack/pty)。
package pty

import (
	"os"
	"os/exec"
	"syscall"
)

// Start 在新 PTY 下启动 cmd,返回主端(读=进程输出,写=进程键盘输入)。
// 子进程成为新会话首领并以 PTY 为控制终端。
func Start(cmd *exec.Cmd) (*os.File, error) {
	ptmx, slaveName, err := open()
	if err != nil {
		return nil, err
	}
	slave, err := os.OpenFile(slaveName, os.O_RDWR, 0)
	if err != nil {
		_ = ptmx.Close()
		return nil, err
	}
	defer func() { _ = slave.Close() }()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	cmd.SysProcAttr.Setctty = true // Ctty 缺省 0 = 子进程的 stdin(即 slave)
	if err := cmd.Start(); err != nil {
		_ = ptmx.Close()
		return nil, err
	}
	return ptmx, nil
}

// KillGroup 杀掉 cmd 所在的整个进程组(Setsid 后子进程自成组;
// shell 包装的命令只杀 shell 会留孤儿)。
func KillGroup(cmd *exec.Cmd) {
	// Cmd.Wait 会并发写 ProcessState,这里不能读取它。Process 与 Pid 在 Start
	// 成功后保持不变；进程组已经消失时 kill(2) 返回 ESRCH,按清理语义忽略。
	process := cmd.Process
	if process == nil {
		return
	}
	_ = syscall.Kill(-process.Pid, syscall.SIGKILL) // KillGroup is deliberately best-effort
}

// ioctl 是平台文件里三步握手的公共出口。
func ioctl(fd uintptr, req uintptr, arg uintptr) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg)
	if errno != 0 {
		return errno
	}
	return nil
}
