package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func ioctl(fd uintptr, req uint, arg unsafe.Pointer) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(req), uintptr(arg)); e != 0 {
		return e
	}
	return nil
}

// openPTY returns a new pseudo-terminal's master and slave.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	var n uint32
	var unlock int32
	if err := ioctl(m.Fd(), syscall.TIOCGPTN, unsafe.Pointer(&n)); err != nil {
		t.Skipf("TIOCGPTN: %v", err)
	}
	if err := ioctl(m.Fd(), syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)); err != nil {
		t.Skipf("TIOCSPTLCK: %v", err)
	}
	s, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("opening the pty: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return m, s
}

func echoOn(t *testing.T, f *os.File) bool {
	t.Helper()
	var tio syscall.Termios
	if err := ioctl(f.Fd(), syscall.TCGETS, unsafe.Pointer(&tio)); err != nil {
		t.Fatal(err)
	}
	return tio.Lflag&syscall.ECHO != 0
}

// TestKeyPromptCtrlC: Ctrl-C at the scree key prompt exits at once,
// non-zero, with echo back on, and saves nothing.
func TestKeyPromptCtrlC(t *testing.T) {
	master, slave := openPTY(t)
	home := tempDir(t)
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "SCREE_TEST_MAIN_HOME="+home, "SCREE_TEST_MAIN_ARGS=key -offline", "OPENROUTER_API_KEY=")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	out := make(chan []byte, 16)
	go func() {
		for {
			b := make([]byte, 4096)
			n, err := master.Read(b)
			if n > 0 {
				out <- b[:n]
			}
			if err != nil {
				close(out)
				return
			}
		}
	}()
	var seen bytes.Buffer
	wait := func(want string) {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for !strings.Contains(seen.String(), want) {
			select {
			case b, ok := <-out:
				if !ok {
					t.Fatalf("output ended before %q: %q", want, seen.String())
				}
				seen.Write(b)
			case <-deadline:
				cmd.Process.Kill()
				t.Fatalf("no %q in %q", want, seen.String())
			}
		}
	}
	wait("OpenRouter API key: ")
	// ReadPassword turns echo off just after the prompt.
	for i := 0; echoOn(t, slave) && i < 100; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if echoOn(t, slave) {
		t.Fatal("echo still on at the prompt")
	}
	master.Write([]byte{3})
	select {
	case err := <-exited:
		ee, ok := err.(*exec.ExitError)
		if !ok || ee.ExitCode() != 130 {
			t.Errorf("exit: %v", err)
		}
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("still running after Ctrl-C: %q", seen.String())
	}
	wait("interrupted; nothing saved")
	if !echoOn(t, slave) {
		t.Error("echo left off")
	}
	if _, err := os.Stat(filepath.Join(home, ".config")); err == nil {
		t.Error("created ~/.config")
	}
}
