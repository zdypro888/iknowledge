package pty

import (
	"fmt"
	"os"
	"unsafe"
)

// linux 的 PTY 三步(值同 creack/pty):
const (
	tiocgptn   = 0x80045430 // 取 pts 号
	tiocsptlck = 0x40045431 // 解锁
)

func open() (*os.File, string, error) {
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, "", err
	}
	var n uint32
	if err := ioctl(ptmx.Fd(), tiocgptn, uintptr(unsafe.Pointer(&n))); err != nil {
		ptmx.Close()
		return nil, "", err
	}
	var unlock int32 // 0 = 解锁
	if err := ioctl(ptmx.Fd(), tiocsptlck, uintptr(unsafe.Pointer(&unlock))); err != nil {
		ptmx.Close()
		return nil, "", err
	}
	return ptmx, fmt.Sprintf("/dev/pts/%d", n), nil
}
