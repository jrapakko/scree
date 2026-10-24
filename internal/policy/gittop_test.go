package policy

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeOwner makes every file named name look owned by uid 4242, and the
// current user 1000, for the rest of the test.
func fakeOwner(t *testing.T, name string) {
	t.Helper()
	oldOwner, oldUID := ownerOf, myUID
	ownerOf = func(fi fs.FileInfo) int {
		if fi.Name() == name {
			return 4242
		}
		return 1000
	}
	myUID = func() int { return 1000 }
	t.Cleanup(func() { ownerOf, myUID = oldOwner, oldUID })
}

func TestNoLiftAcrossSticky(t *testing.T) {
	isolateGit(t)
	s := filepath.Join(t.TempDir(), "s")
	p := filepath.Join(s, "p")
	if err := os.MkdirAll(filepath.Join(s, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p, "a.md"), "a")
	if err := os.Chmod(s, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(s); err != nil || fi.Mode()&os.ModeSticky == 0 {
		t.Skip("no sticky bit on this filesystem")
	}
	top, err := FindGitTop(p)
	if err != nil || top != "" {
		t.Fatalf("FindGitTop = %q, %v; want no work tree", top, err)
	}
	pol := open(t, p, 0)
	if pol.GitTop() != "" {
		t.Errorf("GitTop = %q", pol.GitTop())
	}
	// A work tree below the sticky folder is still found.
	if err := os.Mkdir(filepath.Join(p, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if top, err := FindGitTop(filepath.Join(p)); err != nil || top != p {
		t.Errorf("below sticky: FindGitTop = %q, %v", top, err)
	}
}

func TestForeignGitRefused(t *testing.T) {
	top := repoFixture(t)
	write(t, filepath.Join(top, "sub", "a.md"), "a")
	fakeOwner(t, ".git")
	for _, dir := range []string{top, filepath.Join(top, "sub")} {
		if _, err := FindGitTop(dir); !errors.Is(err, ErrUntrustedGit) || !strings.Contains(err.Error(), "-gitignore=false") {
			t.Errorf("FindGitTop(%s) = %v", dir, err)
		}
		if _, err := Open(dir, 0); !errors.Is(err, ErrUntrustedGit) {
			t.Errorf("Open(%s) = %v", dir, err)
		}
	}
	// With the git layer off, the folder can still be indexed explicitly.
	p, err := OpenOptions(filepath.Join(top, "sub"), Options{})
	if err != nil {
		t.Fatalf("OpenOptions without git: %v", err)
	}
	p.Close()
}

func TestSymlinkDotGitRefused(t *testing.T) {
	isolateGit(t)
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "real.git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	top := filepath.Join(base, "repo")
	link(t, filepath.Join(base, "real.git"), filepath.Join(top, ".git"))
	if _, err := FindGitTop(top); !errors.Is(err, ErrUntrustedGit) {
		t.Errorf("FindGitTop = %v", err)
	}
	if _, err := Open(top, 0); !errors.Is(err, ErrUntrustedGit) {
		t.Errorf("Open = %v", err)
	}
}

func TestGitdirTargetOwner(t *testing.T) {
	isolateGit(t)
	base := t.TempDir()
	gitdir := filepath.Join(base, "real.git")
	common := filepath.Join(base, "common.git")
	for _, d := range []string{filepath.Join(gitdir, "info"), filepath.Join(common, "info")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	top := filepath.Join(base, "repo")
	write(t, filepath.Join(top, ".git"), "gitdir: "+gitdir+"\n")
	write(t, filepath.Join(top, "a.md"), "a")
	p := open(t, top, 0) // trusted
	p.Close()

	fakeOwner(t, "real.git")
	if _, err := Open(top, 0); err == nil || !strings.Contains(err.Error(), "owned by another user") {
		t.Errorf("foreign gitdir: Open = %v", err)
	}

	fakeOwner(t, "common.git")
	write(t, filepath.Join(gitdir, "commondir"), common+"\n")
	if _, err := Open(top, 0); err == nil || !strings.Contains(err.Error(), "owned by another user") {
		t.Errorf("foreign commondir: Open = %v", err)
	}

	// A gitdir that is a symlink is refused too.
	os.Remove(filepath.Join(gitdir, "commondir"))
	ln := filepath.Join(base, "ln.git")
	link(t, gitdir, ln)
	write(t, filepath.Join(top, ".git"), "gitdir: "+ln+"\n")
	if _, err := Open(top, 0); err == nil || !strings.Contains(err.Error(), "is a symlink") {
		t.Errorf("symlinked gitdir: Open = %v", err)
	}
}
