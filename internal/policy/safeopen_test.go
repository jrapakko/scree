package policy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const fakeSecret = "OPENROUTER_API_KEY=sk-or-v1-FAKE0123456789 [bad\n"

// noSecret fails if s repeats any part of fakeSecret.
func noSecret(t *testing.T, what, s string) {
	t.Helper()
	if strings.Contains(s, "sk-or-v1-FAKE") || strings.Contains(s, "[bad") {
		t.Errorf("%s echoes file content: %q", what, s)
	}
}

func TestGitignoreSymlinkInRootRefused(t *testing.T) {
	top := repoFixture(t)
	write(t, filepath.Join(top, ".env"), fakeSecret)
	link(t, "../.env", filepath.Join(top, "docs", ".gitignore"))
	write(t, filepath.Join(top, "docs", "a.md"), "a")
	write(t, filepath.Join(top, "ok.md"), "ok")
	p := open(t, top, 0)
	s := scan(t, p)
	if s.Allowed != 1 || !reflect.DeepEqual(s.Ignored[GitIgnore], []string{"docs/"}) {
		t.Fatalf("Scan = %+v", s)
	}
	want := []string{`read "docs/.gitignore": not a regular file (symlinks are not followed); folder skipped`}
	if !reflect.DeepEqual(s.ReadErrors, want) {
		t.Errorf("ReadErrors = %q, want %q", s.ReadErrors, want)
	}
	if len(s.IgnoreErrors) != 0 {
		t.Errorf("the linked file was parsed: %q", s.IgnoreErrors)
	}
	_, err := p.Read("docs/a.md")
	wantReason(t, err, GitIgnore)

	// The top's own .gitignore as a link fails the whole walk, closed.
	link(t, ".env", filepath.Join(top, ".gitignore"))
	_, err = p.Scan(context.Background())
	if err == nil || !strings.Contains(err.Error(), "symlinks are not followed") {
		t.Fatalf("Scan err = %v", err)
	}
	noSecret(t, "Scan", err.Error())
}

func TestAncestorGitignoreSymlinkRefused(t *testing.T) {
	top := repoFixture(t)
	secret := filepath.Join(t.TempDir(), "secret.txt")
	write(t, secret, fakeSecret)
	link(t, secret, filepath.Join(top, "sub", ".gitignore"))
	root := filepath.Join(top, "sub", "inner")
	write(t, filepath.Join(root, "a.md"), "a")
	p := open(t, root, 0)
	_, err := p.Scan(context.Background())
	if err == nil || !strings.Contains(err.Error(), filepath.Join(top, "sub", ".gitignore")+": not a regular file (symlinks are not followed)") {
		t.Fatalf("Scan err = %v", err)
	}
	noSecret(t, "Scan", err.Error())
	if _, err := p.Read("a.md"); err == nil {
		t.Error("Read succeeded")
	}
}

func TestInfoExcludeSymlinkRefused(t *testing.T) {
	top := repoFixture(t)
	secret := filepath.Join(t.TempDir(), "secret.txt")
	write(t, secret, fakeSecret)
	link(t, secret, filepath.Join(top, ".git", "info", "exclude"))
	write(t, filepath.Join(top, "a.md"), "a")
	p := open(t, top, 0)
	_, err := p.Scan(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exclude: not a regular file (symlinks are not followed)") {
		t.Fatalf("Scan err = %v", err)
	}
	noSecret(t, "Scan", err.Error())
}

func TestScreeignoreSymlinkRefused(t *testing.T) {
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	write(t, filepath.Join(root, "rules.txt"), "nothing.md\n")
	write(t, filepath.Join(root, "a.md"), "a")
	link(t, "rules.txt", filepath.Join(root, ".screeignore"))
	isolateGit(t)
	p := open(t, root, 0)
	for _, f := range []func() error{
		func() error { _, err := p.Read("a.md"); return err },
		func() error { _, err := p.Scan(context.Background()); return err },
	} {
		err := f()
		if err == nil || err.Error() != "read .screeignore: not a regular file (symlinks are not followed)" {
			t.Errorf("err = %v", err)
		}
	}
}

func TestGitFileSymlinkRefused(t *testing.T) {
	isolateGit(t)
	base := t.TempDir()
	top := filepath.Join(base, "repo")
	write(t, filepath.Join(base, "gitfile"), "gitdir: "+filepath.Join(base, "real.git")+"\n")
	if err := os.MkdirAll(filepath.Join(base, "real.git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	link(t, filepath.Join(base, "gitfile"), filepath.Join(top, ".git"))
	if _, err := Open(top, 0); !errors.Is(err, ErrUntrustedGit) {
		t.Fatalf("Open err = %v", err)
	}
}

func TestUserExcludesFileSymlinkFollowed(t *testing.T) {
	top := repoFixture(t)
	home := os.Getenv("HOME")
	write(t, filepath.Join(home, "dotfiles", "ignore"), "global.md\n")
	link(t, filepath.Join(home, "dotfiles", "ignore"), filepath.Join(home, ".config", "git", "ignore"))
	write(t, filepath.Join(top, "global.md"), "g")
	write(t, filepath.Join(top, "ok.md"), "ok")
	p := open(t, top, 0)
	if r := p.CheckName("global.md"); r != GitIgnore {
		t.Errorf("CheckName(global.md) = %s", r)
	}
	if r := p.CheckName("ok.md"); r != Allowed {
		t.Errorf("CheckName(ok.md) = %s", r)
	}
	// A linked ~/.gitconfig naming another linked file is followed too.
	write(t, filepath.Join(home, "dotfiles", "gitconfig"), "[core]\n\texcludesFile = ~/dotfiles/other\n")
	write(t, filepath.Join(home, "dotfiles", "other"), "ok.md\n")
	link(t, filepath.Join(home, "dotfiles", "gitconfig"), filepath.Join(home, ".gitconfig"))
	if r := p.CheckName("ok.md"); r != GitIgnore {
		t.Errorf("after linking .gitconfig: CheckName(ok.md) = %s", r)
	}
}

// A user-controlled excludes file may be read, but its content never
// appears in messages.
func TestUserExcludesFileContentNotEchoed(t *testing.T) {
	top := repoFixture(t)
	home := os.Getenv("HOME")
	secret := filepath.Join(t.TempDir(), "secret.txt")
	write(t, secret, fakeSecret)
	write(t, filepath.Join(home, ".gitconfig"), "[core]\n\texcludesFile = "+secret+"\n")
	write(t, filepath.Join(top, "a.md"), "a")
	p := open(t, top, 0)
	s := scan(t, p)
	if want := []string{secret + " line 1: bad pattern"}; !reflect.DeepEqual(s.IgnoreErrors, want) {
		t.Errorf("IgnoreErrors = %q, want %q", s.IgnoreErrors, want)
	}
	for _, e := range append(s.IgnoreErrors, s.ReadErrors...) {
		noSecret(t, "Scan", e)
	}
}
