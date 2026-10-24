package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tempDir returns a fresh temp dir with symlinks resolved (macOS: /var ->
// /private/var), so it compares equal to resolved results.
func tempDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func mkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

// repoTree makes <tmp>/repo/.git and <tmp>/repo/sub and returns the repo.
func repoTree(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(tempDir(t), "repo")
	mkdir(t, filepath.Join(repo, ".git"))
	mkdir(t, filepath.Join(repo, "sub"))
	return repo
}

func TestResolveExplicit(t *testing.T) {
	home := tempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_PROJECT_DIR", repoTree(t))
	for _, serve := range []bool{false, true} {
		root, src, err := resolveFolder(home, serve)
		if err != nil || root != home || src != flagNotes {
			t.Errorf("serve=%v: resolveFolder(home) = %q, %s, %v", serve, root, src, err)
		}
	}
	// A relative -notes is made absolute, not lifted.
	repo := repoTree(t)
	t.Chdir(repo)
	if root, _, err := resolveFolder("sub", false); err != nil || root != filepath.Join(repo, "sub") {
		t.Errorf("resolveFolder(sub) = %q, %v", root, err)
	}
}

func TestResolveProjectDirLifted(t *testing.T) {
	t.Setenv("HOME", tempDir(t))
	repo := repoTree(t)
	t.Setenv("CLAUDE_PROJECT_DIR", filepath.Join(repo, "sub"))
	root, src, err := resolveFolder("", true)
	if err != nil || root != repo || src != projectDir {
		t.Fatalf("resolveFolder = %q, %s, %v; want %q", root, src, err, repo)
	}
	// Without git, the project dir itself.
	plain := filepath.Join(tempDir(t), "plain")
	mkdir(t, plain)
	t.Setenv("CLAUDE_PROJECT_DIR", plain)
	if top := gitRoot(plain); top != "" {
		t.Skipf("the temp dir is inside a git work tree at %s", top)
	}
	if root, _, err := resolveFolder("", true); err != nil || root != plain {
		t.Fatalf("resolveFolder = %q, %v; want %q", root, err, plain)
	}
}

func TestResolveServeWithoutProjectDir(t *testing.T) {
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	_, _, err := resolveFolder("", true)
	if err == nil || !strings.Contains(err.Error(), "CLAUDE_PROJECT_DIR") || !strings.Contains(err.Error(), "-notes") {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveCwdGitRoot(t *testing.T) {
	t.Setenv("HOME", tempDir(t))
	t.Setenv("CLAUDE_PROJECT_DIR", "/nonexistent")
	repo := repoTree(t)
	deep := filepath.Join(repo, "sub", "deeper")
	mkdir(t, deep)
	t.Chdir(deep)
	root, src, err := resolveFolder("", false)
	if err != nil || root != repo || src != gitRootOfCwd {
		t.Fatalf("resolveFolder = %q, %s, %v; want %q", root, src, err, repo)
	}
}

func TestResolveCwdNoGit(t *testing.T) {
	t.Setenv("HOME", tempDir(t))
	dir := filepath.Join(tempDir(t), "notes")
	mkdir(t, dir)
	if top := gitRoot(dir); top != "" {
		t.Skipf("the temp dir is inside a git work tree at %s", top)
	}
	t.Chdir(dir)
	root, src, err := resolveFolder("", false)
	if err != nil || root != dir || src != cwd {
		t.Fatalf("resolveFolder = %q, %s, %v; want %q", root, src, err, dir)
	}
}

func TestResolveHomeGuard(t *testing.T) {
	home := tempDir(t)
	t.Setenv("HOME", home)
	mkdir(t, filepath.Join(home, ".git")) // a dotfiles repo
	notes := filepath.Join(home, "notes")
	mkdir(t, notes)
	t.Chdir(notes)
	_, _, err := resolveFolder("", false)
	if err == nil || !strings.Contains(err.Error(), "home directory") || !strings.Contains(err.Error(), "pass -notes "+home) {
		t.Fatalf("CLI: err = %v", err)
	}
	t.Setenv("CLAUDE_PROJECT_DIR", notes)
	if _, _, err := resolveFolder("", true); err == nil {
		t.Fatal("serve: no home-guard error")
	}
	if root, _, err := resolveFolder(notes, false); err != nil || root != notes {
		t.Fatalf("with -notes: %q, %v", root, err)
	}
	if root, _, err := resolveFolder(home, false); err != nil || root != home {
		t.Fatalf("with -notes $HOME: %q, %v", root, err)
	}
}

func TestResolveFilesystemRoot(t *testing.T) {
	t.Setenv("HOME", tempDir(t))
	t.Setenv("CLAUDE_PROJECT_DIR", "/")
	if gitRoot("/") != "" {
		t.Skip("/ is a git work tree")
	}
	if _, _, err := resolveFolder("", true); err == nil || !strings.Contains(err.Error(), "filesystem root") {
		t.Fatalf("err = %v", err)
	}
}

func TestIndexDirStable(t *testing.T) {
	data := tempDir(t)
	t.Setenv("XDG_DATA_HOME", data)
	a, b := indexDir("/home/u/repo"), indexDir("/home/u/other/repo")
	if a != indexDir("/home/u/repo") {
		t.Error("same root, different dirs")
	}
	if a == b {
		t.Error("two roots share a dir")
	}
	for _, d := range []string{a, b} {
		if filepath.Dir(d) != filepath.Join(data, "scree", "indexes") {
			t.Errorf("%s is not under $XDG_DATA_HOME/scree/indexes", d)
		}
		if !strings.HasPrefix(filepath.Base(d), "repo-") || len(filepath.Base(d)) != len("repo-")+16 {
			t.Errorf("base %q", filepath.Base(d))
		}
	}
	for in, want := range map[string]string{
		"my notes":              "my_notes",
		".dotfiles":             "_dotfiles",
		"ünï\x1b:c":             "_n___c",
		strings.Repeat("x", 50): strings.Repeat("x", 40),
		"a.b_c-D9":              "a.b_c-D9",
	} {
		if got := safeBase(in); got != want {
			t.Errorf("safeBase(%q) = %q, want %q", in, got, want)
		}
	}
	if base := filepath.Base(indexDir("/x/my notes")); !strings.HasPrefix(base, "my_notes-") {
		t.Errorf("unsafe base not sanitized: %q", base)
	}
}

func TestWriteRootFile(t *testing.T) {
	dir := tempDir(t)
	if err := writeRootFile(dir, "/a"); err != nil {
		t.Fatal(err)
	}
	if err := writeRootFile(dir, "/b"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "root"))
	if err != nil || string(b) != "/a\n" {
		t.Fatalf("root file = %q, %v", b, err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "root")); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
}

func TestResolveHomeAncestors(t *testing.T) {
	tmp := tempDir(t)
	home := filepath.Join(tmp, "home", "u")
	repo := filepath.Join(home, "code", "r")
	mkdir(t, repo)
	t.Setenv("HOME", home)
	for _, dir := range []string{home, filepath.Join(tmp, "home"), tmp, "/"} {
		if dir == "/" && gitRoot("/") != "" {
			continue
		}
		t.Setenv("CLAUDE_PROJECT_DIR", dir)
		if _, _, err := resolveFolder("", true); err == nil || !strings.Contains(err.Error(), "a folder above it") {
			t.Errorf("%s: err = %v", dir, err)
		}
		// An explicit -notes on the command line stays unguarded.
		if root, _, err := resolveFolder(dir, false); err != nil || root != dir {
			t.Errorf("-notes %s: %q, %v", dir, root, err)
		}
	}
	t.Setenv("CLAUDE_PROJECT_DIR", repo)
	if root, _, err := resolveFolder("", true); err != nil || root != repo {
		t.Errorf("repo: %q, %v", root, err)
	}

	// A symlinked HOME is compared resolved.
	link := filepath.Join(tmp, "homelink")
	if err := os.Symlink(home, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("HOME", link)
	t.Setenv("CLAUDE_PROJECT_DIR", filepath.Join(tmp, "home"))
	if _, _, err := resolveFolder("", true); err == nil {
		t.Error("symlinked HOME: the folder above it was not refused")
	}
	t.Setenv("CLAUDE_PROJECT_DIR", repo)
	if root, _, err := resolveFolder("", true); err != nil || root != repo {
		t.Errorf("symlinked HOME, repo: %q, %v", root, err)
	}
}
