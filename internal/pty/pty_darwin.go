package pty

import (
	"os"
	"unsafe"
)

// darwin 的 PTY 三步(posix_openpt/grantpt/unlockpt 的 ioctl 等价,值同 creack/pty):
const (
	tiocptygrant = 0x20007454 // grantpt
	tiocptyunlk  = 0x20007452 // unlockpt
	tiocptygname = 0x40807453 // ptsname → [128]byte
)

func open() (*os.File, string, error) {
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, "", err
	}
	if err := ioctl(ptmx.Fd(), tiocptygrant, 0); err != nil {
		_ = ptmx.Close()
		return nil, "", err
	}
	if err := ioctl(ptmx.Fd(), tiocptyunlk, 0); err != nil {
		_ = ptmx.Close()
		return nil, "", err
	}
	var name [128]byte
	if err := ioctl(ptmx.Fd(), tiocptygname, uintptr(unsafe.Pointer(&name[0]))); err != nil {
		_ = ptmx.Close()
		return nil, "", err
	}
	n := 0
	for n < len(name) && name[n] != 0 {
		n++
	}
	return ptmx, string(name[:n]), nil
}
