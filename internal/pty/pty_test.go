//go:build darwin || linux

package pty

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// PTY 冒烟:进程在 PTY 下运行(isatty 为真)且输出可从主端读回。
func TestStartEcho(t *testing.T) {
	cmd := exec.Command("sh", "-c", "if [ -t 0 ]; then echo TTY-YES; else echo TTY-NO; fi")
	ptmx, err := Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ptmx.Close() }()

	out := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		var all []byte
		for {
			n, err := ptmx.Read(buf)
			all = append(all, buf[:n]...)
			if err != nil || strings.Contains(string(all), "TTY-") {
				out <- string(all)
				return
			}
		}
	}()
	select {
	case s := <-out:
		if !strings.Contains(s, "TTY-YES") {
			t.Errorf("进程未获得 TTY:%q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("读 PTY 输出超时")
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

// 键盘输入回程:写主端 → 子进程 stdin。
func TestStdinRoundTrip(t *testing.T) {
	cmd := exec.Command("sh", "-c", "read line; echo GOT-$line")
	ptmx, err := Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ptmx.Close() }()
	if _, err := ptmx.Write([]byte("hello\r")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var all []byte
	buf := make([]byte, 4096)
	for time.Now().Before(deadline) {
		n, err := ptmx.Read(buf)
		all = append(all, buf[:n]...)
		if strings.Contains(string(all), "GOT-hello") {
			if err := cmd.Wait(); err != nil {
				t.Fatal(err)
			}
			return
		}
		if err != nil {
			break
		}
	}
	t.Fatalf("未读到回显:%q", string(all))
}
