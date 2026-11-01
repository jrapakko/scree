package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestLessCharsetPTY: less -FRX on a terminal, with no LANG or LC_* (as
// under scree run), shows UTF-8 as is once LESSCHARSET=utf-8 is set, and
// as <E2>-style escapes without it.
func TestLessCharsetPTY(t *testing.T) {
	less := findExec([]string{"/usr/bin", "/bin"}, "less")
	if less == "" {
		t.Skip("no less")
	}
	in := filepath.Join(tempDir(t), "view")
	if err := os.WriteFile(in, []byte("[mode: auto → semantic] cut…\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	show := func(env ...string) string {
		t.Helper()
		master, slave := openPTY(t)
		f, err := os.Open(in)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		cmd := exec.Command(less, "-FRX")
		cmd.Env = append([]string{"TERM=xterm"}, env...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = f, slave, slave
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 1}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			cmd.Process.Kill()
			t.Fatal("less did not quit")
		}
		slave.Close()
		var out bytes.Buffer
		b := make([]byte, 4096)
		for {
			n, err := master.Read(b)
			out.Write(b[:n])
			if err != nil {
				break
			}
		}
		return out.String()
	}
	if out := show("LESSCHARSET=utf-8"); !strings.Contains(out, "auto → semantic") || !strings.Contains(out, "cut…") {
		t.Errorf("with LESSCHARSET=utf-8: %q", out)
	}
	if out := show(); !strings.Contains(out, "<E2><86><92>") {
		t.Errorf("without LESSCHARSET (the bug this guards): %q", out)
	}
}
