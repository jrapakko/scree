package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jrapakko/scree/internal/lock"
	"github.com/jrapakko/scree/internal/policy"
)

// gitRoot is policy.FindGitTop with errors read as "no work tree", for
// tests that only need the answer.
func gitRoot(dir string) string {
	top, _ := policy.FindGitTop(dir)
	return top
}

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
	fakeHome(t, home)
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
	fakeHome(t, tempDir(t))
	repo := repoTree(t)
	serveIn(t, filepath.Join(repo, "sub"))
	root, src, err := resolveFolder("", true)
	if err != nil || root != repo || src != claudeCode {
		t.Fatalf("resolveFolder = %q, %s, %v; want %q", root, src, err, repo)
	}
	// Without git, the project dir itself.
	plain := filepath.Join(tempDir(t), "plain")
	mkdir(t, plain)
	serveIn(t, plain)
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
	fakeHome(t, tempDir(t))
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
	fakeHome(t, tempDir(t))
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
	fakeHome(t, home)
	mkdir(t, filepath.Join(home, ".git")) // a dotfiles repo
	notes := filepath.Join(home, "notes")
	mkdir(t, notes)
	t.Chdir(notes)
	_, _, err := resolveFolder("", false)
	if err == nil || !strings.Contains(err.Error(), "home directory") || !strings.Contains(err.Error(), "pass -notes "+home) {
		t.Fatalf("CLI: err = %v", err)
	}
	serveIn(t, notes)
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
	fakeHome(t, tempDir(t))
	serveIn(t, "/")
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
	fakeHome(t, home)
	for _, dir := range []string{home, filepath.Join(tmp, "home"), tmp, "/"} {
		if dir == "/" && gitRoot("/") != "" {
			continue
		}
		serveIn(t, dir)
		if _, _, err := resolveFolder("", true); err == nil || !strings.Contains(err.Error(), "a folder above it") {
			t.Errorf("%s: err = %v", dir, err)
		}
		// An explicit -notes on the command line stays unguarded.
		if root, _, err := resolveFolder(dir, false); err != nil || root != dir {
			t.Errorf("-notes %s: %q, %v", dir, root, err)
		}
	}
	serveIn(t, repo)
	if root, _, err := resolveFolder("", true); err != nil || root != repo {
		t.Errorf("repo: %q, %v", root, err)
	}

	// A symlinked HOME is compared resolved.
	link := filepath.Join(tmp, "homelink")
	if err := os.Symlink(home, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	fakeHome(t, link)
	serveIn(t, filepath.Join(tmp, "home"))
	if _, _, err := resolveFolder("", true); err == nil {
		t.Error("symlinked HOME: the folder above it was not refused")
	}
	serveIn(t, repo)
	if root, _, err := resolveFolder("", true); err != nil || root != repo {
		t.Errorf("symlinked HOME, repo: %q, %v", root, err)
	}
}

func TestResolveNoLiftAcrossSticky(t *testing.T) {
	fakeHome(t, tempDir(t))
	s := filepath.Join(tempDir(t), "s")
	p := filepath.Join(s, "p")
	mkdir(t, filepath.Join(s, ".git"))
	mkdir(t, p)
	if err := os.Chmod(s, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(s); err != nil || fi.Mode()&os.ModeSticky == 0 {
		t.Skip("no sticky bit on this filesystem")
	}
	serveIn(t, p)
	if root, _, err := resolveFolder("", true); err != nil || root != p {
		t.Errorf("resolveFolder = %q, %v; want %q", root, err, p)
	}
}

func TestResolveRefusesSymlinkGit(t *testing.T) {
	fakeHome(t, tempDir(t))
	base := tempDir(t)
	mkdir(t, filepath.Join(base, "real.git"))
	repo := filepath.Join(base, "repo")
	mkdir(t, filepath.Join(repo, "sub"))
	if err := os.Symlink(filepath.Join(base, "real.git"), filepath.Join(repo, ".git")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	serveIn(t, filepath.Join(repo, "sub"))
	if _, _, err := resolveFolder("", true); !errors.Is(err, policy.ErrUntrustedGit) {
		t.Errorf("resolveFolder = %v", err)
	}
}

// fakeHome makes dir the home directory for the home guard (instead of the
// password database) and for $HOME.
func fakeHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	old := lookupHome
	lookupHome = func() string { return dir }
	t.Cleanup(func() { lookupHome = old })
}

// serveIn sets CLAUDE_PROJECT_DIR to dir and runs in it, as Claude Code
// starts serve.
func serveIn(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("CLAUDE_PROJECT_DIR", dir)
	t.Chdir(dir)
}

// TestServeFolderFromWorkingDirectory: under Claude Code the folder is the
// working directory's git root; a CLAUDE_PROJECT_DIR forged by a project's
// .mcp.json to another repo or to home is refused, not followed.
func TestServeFolderFromWorkingDirectory(t *testing.T) {
	home := tempDir(t)
	fakeHome(t, home)
	repo := repoTree(t)
	other := repoTree(t)
	t.Chdir(filepath.Join(repo, "sub"))
	t.Setenv("CLAUDE_PROJECT_DIR", repo)
	if root, src, err := resolveFolder("", true); err != nil || root != repo || src != claudeCode {
		t.Fatalf("resolveFolder = %q, %s, %v", root, src, err)
	}
	for _, forged := range []string{other, filepath.Join(other, "sub"), home, "/"} {
		t.Setenv("CLAUDE_PROJECT_DIR", forged)
		if root, _, err := resolveFolder("", true); err == nil || !strings.Contains(err.Error(), "not in the repo of the working directory") {
			t.Errorf("CLAUDE_PROJECT_DIR=%s: %q, %v", forged, root, err)
		}
	}
}

// TestServeFolderWorktree: in a worktree of the project's repo, serve
// indexes the worktree.
func TestServeFolderWorktree(t *testing.T) {
	fakeHome(t, tempDir(t))
	repo := repoTree(t)
	mkdir(t, filepath.Join(repo, ".git", "worktrees", "wt"))
	if err := os.WriteFile(filepath.Join(repo, ".git", "worktrees", "wt", "commondir"), []byte("../..\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(repo, ".claude", "worktrees", "wt")
	mkdir(t, wt)
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+filepath.Join(repo, ".git", "worktrees", "wt")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(wt)
	t.Setenv("CLAUDE_PROJECT_DIR", repo)
	if root, _, err := resolveFolder("", true); err != nil || root != wt {
		t.Fatalf("worktree: %q, %v; want %q", root, err, wt)
	}
	// A worktree of another repository is not the project.
	other := repoTree(t)
	t.Setenv("CLAUDE_PROJECT_DIR", other)
	if _, _, err := resolveFolder("", true); err == nil {
		t.Error("worktree of another repo accepted")
	}
}

// TestHomeGuardIgnoresHOME: the guard takes home from the password
// database, so a bogus HOME does not let serve index the real home.
func TestHomeGuardIgnoresHOME(t *testing.T) {
	real := lookupHome()
	if real == "" {
		t.Skip("no home directory")
	}
	if h, err := filepath.EvalSymlinks(real); err == nil {
		real = h
	}
	t.Setenv("HOME", tempDir(t))
	if !isHomeOrAbove(real) || !isHomeOrAbove(filepath.Dir(real)) {
		t.Errorf("home %s not guarded with a bogus HOME", real)
	}
}

func TestHomeGuardAdviceUnderClaude(t *testing.T) {
	home := tempDir(t)
	fakeHome(t, home)
	serveIn(t, home)
	_, _, err := resolveFolder("", true)
	if err == nil || strings.Contains(err.Error(), "-notes") || !strings.Contains(err.Error(), "does not index it from Claude Code") {
		t.Errorf("err = %v", err)
	}
}

// TestServeRequiresOptIn: serve under Claude Code refuses a repo whose
// .mcp.json does not name scree mcp; the CLI is unaffected.
func TestServeRequiresOptIn(t *testing.T) {
	serveEnv(t)
	repo := repoTree(t)
	serveIn(t, repo)
	cfg := config{serve: true}
	if _, err := open(&cfg, true); !errors.Is(err, errNotOptedIn) {
		t.Fatalf("serve without opt-in: %v", err)
	}
	optIn(t, repo)
	cfg = config{serve: true}
	ix, err := open(&cfg, true)
	if err != nil {
		t.Fatalf("serve after opt-in: %v", err)
	}
	closeIndexer(ix)
	os.Remove(filepath.Join(repo, ".mcp.json"))
	cfg = config{}
	ix, err = open(&cfg, true)
	if err != nil {
		t.Fatalf("CLI: %v", err)
	}
	closeIndexer(ix)
}

// TestIndexNeverThroughSymlink: a symlinked index directory, indexes
// folder, index.db or writer.lock is refused, and nothing is written where
// the link points.
func TestIndexNeverThroughSymlink(t *testing.T) {
	notes := filepath.Join(tempDir(t), "notes")
	mkdir(t, notes)
	target := tempDir(t)
	cases := map[string]func(data, dir string){
		"index dir": func(data, dir string) {
			mkdir(t, filepath.Dir(dir))
			os.Symlink(target, dir)
		},
		"indexes folder": func(data, dir string) {
			mkdir(t, filepath.Join(data, "scree"))
			os.Symlink(target, filepath.Join(data, "scree", "indexes"))
		},
		"index.db": func(data, dir string) {
			mkdir(t, dir)
			os.Symlink(filepath.Join(target, "x.db"), filepath.Join(dir, "index.db"))
		},
		"index.db-wal": func(data, dir string) {
			mkdir(t, dir)
			os.Symlink(filepath.Join(target, "x.db-wal"), filepath.Join(dir, "index.db-wal"))
		},
	}
	for name, plant := range cases {
		t.Run(name, func(t *testing.T) {
			data := serveEnv(t)
			root, _ := filepath.EvalSymlinks(notes)
			plant(data, indexDir(root))
			cfg := config{notes: notes}
			if ix, err := open(&cfg, true); err == nil {
				closeIndexer(ix)
				t.Fatal("opened an index through a symlink")
			}
			if ents, _ := os.ReadDir(target); len(ents) != 0 {
				t.Errorf("wrote into the link target: %v", ents)
			}
		})
	}
	t.Run("writer.lock", func(t *testing.T) {
		serveEnv(t)
		root, _ := filepath.EvalSymlinks(notes)
		dir := indexDir(root)
		mkdir(t, dir)
		victim := filepath.Join(target, "victim")
		os.WriteFile(victim, []byte("keep"), 0o600)
		os.Symlink(victim, filepath.Join(dir, "writer.lock"))
		if _, _, err := lock.TryLock(filepath.Join(dir, "writer.lock")); !errors.Is(err, lock.ErrSymlink) {
			t.Errorf("TryLock = %v", err)
		}
		if _, _, err := lock.Probe(filepath.Join(dir, "writer.lock")); !errors.Is(err, lock.ErrSymlink) {
			t.Errorf("Probe = %v", err)
		}
		if b, _ := os.ReadFile(victim); string(b) != "keep" {
			t.Errorf("victim changed: %q", b)
		}
	})
}
