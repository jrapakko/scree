//go:build unix

package policy

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestFIFORefusedWithoutBlocking(t *testing.T) {
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	if err := syscall.Mkfifo(filepath.Join(root, "pipe.md"), 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	link(t, "pipe.md", filepath.Join(root, "pipelink.md"))
	p := open(t, root, 0)

	done := make(chan error, 1)
	go func() {
		var errs []error
		for _, rel := range []string{"pipe.md", "pipelink.md"} {
			if _, err := p.Read(rel); err == nil || errors.Is(err, ErrIgnored) {
				errs = append(errs, errors.New(rel+": want a read error"))
			}
		}
		n := 0
		err := p.Walk(context.Background(), func(rel string, src []byte, err error) error {
			if err == nil {
				errs = append(errs, errors.New(rel+": walk allowed a FIFO"))
			}
			n++
			return nil
		})
		if err != nil || n != 2 {
			errs = append(errs, errors.New("walk did not report both entries"))
		}
		done <- errors.Join(errs...)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading a FIFO blocked")
	}
}

func TestScanReadErrors(t *testing.T) {
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	if err := syscall.Mkfifo(filepath.Join(root, "pipe.md"), 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	write(t, filepath.Join(root, "ok.md"), "ok")
	p := open(t, root, 0)
	s, err := p.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Allowed != 1 || len(s.ReadErrors) != 1 || s.ReadErrors[0] != `read "pipe.md": not a regular file` {
		t.Fatalf("Scan = %+v", s)
	}
	_, err = p.Read("pipe.md")
	if !errors.Is(err, ErrNotRegular) {
		t.Fatalf("Read(pipe.md) = %v", err)
	}
}

func TestReadErrorHidesAbsolutePath(t *testing.T) {
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	_, err := open(t, root, 0).Read("missing.md")
	if err == nil || strings.Contains(err.Error(), dir) {
		t.Fatalf("err = %v", err)
	}
}
