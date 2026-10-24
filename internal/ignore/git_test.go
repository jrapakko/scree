package ignore

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRules builds GitRules from .gitignore contents keyed by top-relative
// folder ("" is the top) and base files, lowest precedence first.
func gitRules(t *testing.T, dirs map[string]string, base ...string) *GitRules {
	t.Helper()
	ms := map[string]*Matcher{}
	for d, src := range dirs {
		m := Parse([]byte(src))
		if errs := m.Errors(); len(errs) > 0 {
			t.Fatalf("%s/.gitignore: %v", d, errs)
		}
		ms[d] = m
	}
	g := &GitRules{Dir: func(dir string) *Matcher { return ms[dir] }}
	for _, src := range base {
		g.Base = append(g.Base, Parse([]byte(src)))
	}
	return g
}

func checkGit(t *testing.T, g *GitRules, checks []check) {
	t.Helper()
	for _, c := range checks {
		if got := g.Ignored(c.path, c.isDir); got != c.want {
			t.Errorf("Ignored(%q, %v) = %v, want %v", c.path, c.isDir, got, c.want)
		}
	}
}

func TestMatchSelf(t *testing.T) {
	m := Parse([]byte("build/\n*.md\n!keep.md\na/**\n"))
	cases := []struct {
		rel            string
		isDir          bool
		matched, neg   bool
		ancestorsMatch bool // what Match says, for contrast
	}{
		{"build", true, true, false, true},
		{"build", false, false, false, false},
		{"build/x.txt", false, false, false, true}, // no ancestor loop
		{"x.md", false, true, false, true},
		{"keep.md", false, true, true, false},
		{"a", true, false, false, true}, // a/** is inside a only
		{"a/b", true, true, false, true},
	}
	for _, c := range cases {
		matched, neg := m.MatchSelf(c.rel, c.isDir)
		if matched != c.matched || neg != c.neg {
			t.Errorf("MatchSelf(%q, %v) = %v, %v, want %v, %v", c.rel, c.isDir, matched, neg, c.matched, c.neg)
		}
		if got := m.Match(c.rel, c.isDir); got != c.ancestorsMatch {
			t.Errorf("Match(%q, %v) = %v, want %v", c.rel, c.isDir, got, c.ancestorsMatch)
		}
	}
	var nilM *Matcher
	if matched, _ := nilM.MatchSelf("a", false); matched {
		t.Error("nil matcher matched")
	}
}

func TestGitNoReincludeInsideExcludedDir(t *testing.T) {
	src := "build/\n!build/keep.md\n"
	checkGit(t, gitRules(t, map[string]string{"": src}), []check{
		{"build", true, true},
		{"build/keep.md", false, true},
		{"build/x.md", false, true},
		{"other.md", false, false},
	})
	// The .screeignore matcher re-includes it; that is the contrast.
	if Parse([]byte(src)).Match("build/keep.md", false) {
		t.Error(".screeignore semantics changed: build/keep.md no longer re-included")
	}
}

func TestGitReincludeFile(t *testing.T) {
	checkGit(t, gitRules(t, map[string]string{"": "*.md\n!README.md\n"}), []check{
		{"README.md", false, false},
		{"sub/README.md", false, false},
		{"x.md", false, true},
		{"sub/x.md", false, true},
	})
}

func TestGitReincludeInsideDoubleStar(t *testing.T) {
	// a/** excludes what is inside a, not a itself, so a/keep.md can come back.
	checkGit(t, gitRules(t, map[string]string{"": "a/**\n!a/keep.md\n"}), []check{
		{"a", true, false},
		{"a/keep.md", false, false},
		{"a/x.md", false, true},
		{"a/b", true, true},
		{"a/b/keep.md", false, true},
	})
}

func TestGitNestedPrecedence(t *testing.T) {
	g := gitRules(t, map[string]string{
		"":     "docs/*.md\n",
		"docs": "!a.md\n",
	})
	checkGit(t, g, []check{
		{"docs/a.md", false, false},
		{"docs/b.md", false, true},
	})
}

func TestGitNestedAnchoring(t *testing.T) {
	checkGit(t, gitRules(t, map[string]string{"sub": "/x.md\n"}), []check{
		{"sub/x.md", false, true},
		{"sub/deep/x.md", false, false},
		{"x.md", false, false},
	})
	checkGit(t, gitRules(t, map[string]string{"sub": "x.md\n"}), []check{
		{"sub/x.md", false, true},
		{"sub/deep/x.md", false, true},
		{"x.md", false, false},
	})
}

func TestGitBaseOrder(t *testing.T) {
	global, exclude := "*.md\n", "!a.md\n"
	checkGit(t, gitRules(t, nil, global, exclude), []check{
		{"a.md", false, false},
		{"b.md", false, true},
		{"sub/a.md", false, false},
	})
	// A .gitignore rule overrides both.
	g := gitRules(t, map[string]string{"": "a.md\n", "sub": "!b.md\n"}, global, exclude)
	checkGit(t, g, []check{
		{"a.md", false, true},
		{"sub/a.md", false, true},
		{"sub/b.md", false, false},
		{"c.md", false, true},
	})
}

func TestGitDirOnly(t *testing.T) {
	checkGit(t, gitRules(t, map[string]string{"": "node_modules/\n"}), []check{
		{"node_modules", true, true},
		{"node_modules/x/README.md", false, true},
		{"a/node_modules", true, true},
		{"node_modules", false, false},
		{"a/node_modules", false, false},
	})
	g := gitRules(t, map[string]string{"": "node_modules/\n"})
	if !g.IgnoredDir("node_modules") || g.IgnoredDir("src") {
		t.Error("IgnoredDir disagrees with Ignored(rel, true)")
	}
}

func TestGitEmpty(t *testing.T) {
	var nilG *GitRules
	for _, g := range []*GitRules{nilG, {}, {Dir: func(string) *Matcher { return nil }}} {
		if g.Ignored("a.md", false) || g.IgnoredDir("a") || g.Ignored("", true) {
			t.Error("empty rules ignored something")
		}
	}
}

// gitFixture is the tree TestGitAgainstGit builds: ignore files by path, and
// the files and folders to compare. Folders end in "/".
var gitFixture = struct {
	ignores map[string]string // path relative to the work tree -> content
	paths   []string
}{
	ignores: map[string]string{
		".gitignore": "# comment line\n" +
			"*.log\n" +
			"build/\n" +
			"!build/keep.md\n" +
			"/top-only.md\n" +
			"docs/*.tmp.md\n" +
			"\\#hash.md\n" +
			"trail.md   \n" +
			"gen/**\n" +
			"!gen/keep.md\n" +
			"[!x]y.md\n" +
			"a/**/z.md\n" +
			"**/cache/\n" +
			"vendor\n" +
			"!notes.md\n",
		"docs/.gitignore": "!*.tmp.md\n" +
			"/local.md\n" +
			"draft-*.md\n" +
			"sub/\n" +
			"!b.priv.md\n",
		"docs/api/.gitignore": "!draft-api.md\n" +
			"*.bak.md\n" +
			"q?.md\n",
		".git/info/exclude": "secret*.md\n" +
			"!secret-ok.md\n" +
			"!a.priv.md\n",
		"xdg/git/ignore": "*.priv.md\n" +
			"notes.md\n" +
			"secret-ok.md\n",
	},
	paths: []string{
		"a.md", "x.log", "sub/y.log", "sub/",
		"build/", "build/keep.md", "build/x.md",
		"top-only.md", "sub/top-only.md",
		"docs/", "docs/a.tmp.md", "docs/b.md", "tmp.md/", "x.tmp.md",
		"#hash.md", "hash.md", "trail.md", "trail.md.x",
		"gen/", "gen/keep.md", "gen/x.md", "gen/d/", "gen/d/keep.md",
		"ay.md", "xy.md", "sub/by.md",
		"a/z.md", "a/b/c/z.md", "b/z.md",
		"cache/", "cache/x.md", "p/cache/x.md", "p/cache.md",
		"vendor/x.md", "p/vendor",
		"docs/local.md", "docs/sub2/local.md", "local.md",
		"docs/draft-1.md", "docs/api/draft-api.md", "docs/api/draft-2.md",
		"docs/api/x.bak.md", "docs/api/q1.md", "docs/api/q12.md",
		"docs/sub/", "docs/sub/x.md",
		"secret.md", "secret-ok.md", "docs/secret2.md",
		"a.priv.md", "b.priv.md", "docs/b.priv.md", "docs/api/c.priv.md",
		"notes.md", "docs/notes.md",
	},
}

// TestGitAgainstGit compares GitRules with git check-ignore on a fixed tree.
// It is the correctness backstop for the native matcher, so it fails rather
// than skips in CI when git is missing.
func TestGitAgainstGit(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("git is not on PATH, and CI is set: this test must run in CI (%v)", err)
		}
		t.Skipf("git is not on PATH: %v", err)
	}
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	xdg := filepath.Join(tmp, "xdg")
	repo := filepath.Join(tmp, "repo")
	for _, d := range []string{home, xdg, repo} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	env := []string{"HOME=" + home, "XDG_CONFIG_HOME=" + xdg, "GIT_CONFIG_NOSYSTEM=1"}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GIT_") || k == "HOME" || k == "XDG_CONFIG_HOME" {
			continue
		}
		env = append(env, kv)
	}
	git := func(args ...string) error {
		cmd := exec.Command(gitBin, append([]string{"-C", repo}, args...)...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil && len(out) > 0 {
			return errors.Join(err, errors.New(strings.TrimSpace(string(out))))
		}
		return err
	}
	if err := git("init", "-q"); err != nil {
		t.Fatalf("git init: %v", err)
	}

	write := func(name, content string) {
		t.Helper()
		p := filepath.Join(repo, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range gitFixture.paths {
		if d, ok := strings.CutSuffix(p, "/"); ok {
			if err := os.MkdirAll(filepath.Join(repo, filepath.FromSlash(d)), 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		write(p, "x\n")
	}
	dirs := map[string]string{}
	var global, exclude string
	for name, content := range gitFixture.ignores {
		switch {
		case name == "xdg/git/ignore":
			p := filepath.Join(xdg, "git", "ignore")
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			global = content
		case name == ".git/info/exclude":
			write(name, content)
			exclude = content
		default:
			write(name, content)
			dirs[strings.TrimSuffix(strings.TrimSuffix(name, ".gitignore"), "/")] = content
		}
	}
	g := gitRules(t, dirs, global, exclude)

	var ignored, kept int
	for _, p := range gitFixture.paths {
		rel, isDir := strings.CutSuffix(p, "/")
		var want bool
		switch err := git("check-ignore", "--no-index", "-q", "--", rel); {
		case err == nil:
			want = true
		case exitCode(err) == 1:
			want = false
		default:
			t.Fatalf("git check-ignore %q: %v", rel, err)
		}
		if want {
			ignored++
		} else {
			kept++
		}
		if got := g.Ignored(rel, isDir); got != want {
			t.Errorf("Ignored(%q, %v) = %v, git check-ignore says %v", rel, isDir, got, want)
		}
	}
	if ignored < 10 || kept < 10 {
		t.Errorf("fixture is lopsided: %d ignored, %d kept", ignored, kept)
	}
}

func exitCode(err error) int {
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return ee.ExitCode()
	}
	return -1
}
