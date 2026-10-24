//go:build unix

package policy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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

// skipIfRoot skips tests that rely on permission bits being enforced.
func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode 000 is not enforced")
	}
}

// unreadable makes p mode 000 for the rest of the test.
func unreadable(t *testing.T, p string) {
	t.Helper()
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(p, 0o755) })
}

func TestGitignorePrunesWithoutDescending(t *testing.T) {
	skipIfRoot(t)
	top := repoFixture(t)
	write(t, filepath.Join(top, ".gitignore"), "node_modules/\n")
	write(t, filepath.Join(top, "node_modules", "x", "README.md"), "x")
	write(t, filepath.Join(top, "a.md"), "a")
	unreadable(t, filepath.Join(top, "node_modules", "x"))
	p := open(t, top, 0)
	got := walkAll(t, p)
	want := map[string]string{"a.md": "", ".git/": "hidden", "node_modules/": ".gitignore"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Walk = %v\nwant %v", got, want)
	}
	if s := scan(t, p); len(s.ReadErrors) != 0 {
		t.Errorf("ReadErrors = %q", s.ReadErrors)
	}
}

func TestGitignoreUnreadableSkipsFolder(t *testing.T) {
	skipIfRoot(t)
	top := repoFixture(t)
	write(t, filepath.Join(top, "docs", ".gitignore"), "nothing\n")
	write(t, filepath.Join(top, "docs", "a.md"), "a")
	write(t, filepath.Join(top, "ok.md"), "ok")
	write(t, filepath.Join(top, "other", "b.md"), "b")
	unreadable(t, filepath.Join(top, "docs", ".gitignore"))
	p := open(t, top, 0)
	s := scan(t, p)
	if s.Allowed != 2 || !reflect.DeepEqual(s.Ignored[GitIgnore], []string{"docs/"}) {
		t.Errorf("Scan = %+v", s)
	}
	if want := []string{`read "docs/.gitignore": permission denied; folder skipped`}; !reflect.DeepEqual(s.ReadErrors, want) {
		t.Errorf("ReadErrors = %q, want %q", s.ReadErrors, want)
	}
	_, err := p.Read("docs/a.md")
	wantReason(t, err, GitIgnore)
	if r := p.CheckName("docs/a.md"); r != GitIgnore {
		t.Errorf("CheckName = %s", r)
	}
}

func TestGitignoreRootUnreadableFailsClosed(t *testing.T) {
	skipIfRoot(t)
	top := repoFixture(t)
	write(t, filepath.Join(top, ".gitignore"), "nothing\n")
	write(t, filepath.Join(top, "a.md"), "a")
	unreadable(t, filepath.Join(top, ".gitignore"))
	p := open(t, top, 0)
	if _, err := p.Read("a.md"); err == nil || errors.Is(err, ErrIgnored) {
		t.Errorf("Read = %v, want a read error", err)
	}
	called := false
	err := p.Walk(context.Background(), func(string, []byte, error) error { called = true; return nil })
	if err == nil || called {
		t.Errorf("Walk err = %v, called = %v", err, called)
	}
}

func TestGitExcludesUnreadableFailsClosed(t *testing.T) {
	skipIfRoot(t)
	top := repoFixture(t)
	exclude := filepath.Join(top, ".git", "info", "exclude")
	write(t, exclude, "x.md\n")
	write(t, filepath.Join(top, "a.md"), "a")
	unreadable(t, exclude)
	p := open(t, top, 0)
	if _, err := p.Read("a.md"); err == nil || errors.Is(err, ErrIgnored) {
		t.Errorf("Read = %v, want a read error", err)
	}
	if _, err := p.Scan(context.Background()); err == nil {
		t.Error("Scan succeeded")
	}
}

func TestWorktreesPrunedUnlisted(t *testing.T) {
	skipIfRoot(t)
	isolateGit(t)
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	write(t, filepath.Join(root, ".claude", "worktrees", "w", "README.md"), "x")
	write(t, filepath.Join(root, ".claude", "agents", "a.md"), "x")
	unreadable(t, filepath.Join(root, ".claude", "worktrees"))
	p := open(t, root, 0)
	want := map[string]string{".claude/worktrees/": "hidden", ".claude/agents/a.md": ""}
	if got := walkAll(t, p); !reflect.DeepEqual(got, want) {
		t.Errorf("Walk = %v\nwant %v", got, want)
	}
	if s := scan(t, p); len(s.ReadErrors) != 0 {
		t.Errorf("ReadErrors = %q", s.ReadErrors)
	}
}

// The repository's .git/config is never read, so it cannot name an excludes
// file. Both it and the file it names are FIFOs here: opening either would
// block, or with O_NONBLOCK fail as not a regular file.
func TestRepoConfigExcludesFileIgnored(t *testing.T) {
	top := repoFixture(t)
	fifo := filepath.Join(t.TempDir(), "x")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	write(t, filepath.Join(top, "a.md"), "a")
	cfg := filepath.Join(top, ".git", "config")
	for _, step := range []string{"regular config naming a FIFO", "FIFO config"} {
		os.Remove(cfg)
		if step == "FIFO config" {
			if err := syscall.Mkfifo(cfg, 0o644); err != nil {
				t.Fatal(err)
			}
		} else {
			write(t, cfg, "[core]\n\texcludesFile = "+fifo+"\n")
		}
		done := make(chan error, 1)
		go func() {
			p, err := Open(top, 0)
			if err != nil {
				done <- err
				return
			}
			defer p.Close()
			s, err := p.Scan(context.Background())
			if err == nil && (s.Allowed != 1 || len(s.ReadErrors) != 0) {
				err = errors.New("scan saw the config")
			}
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s: %v", step, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: opening the repo config blocked", step)
		}
	}
}
