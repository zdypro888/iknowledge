//go:build !darwin && !linux

package pty

import (
	"errors"
	"os"
	"os/exec"
)

// Start 在不支持的平台明确报错(impl §7.5:自派备模式仅 macOS/Linux;
// Windows ConPTY 是另一个量级的适配,委派主模式不受影响)。
func Start(*exec.Cmd) (*os.File, error) {
	return nil, errors.New("pty: 本平台不支持自派备模式(仅 macOS/Linux;委派主模式不受影响)")
}

// KillGroup 退化为杀单进程。
func KillGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		cmd.Process.Kill()
	}
}
