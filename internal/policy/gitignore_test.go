package policy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// isolateGit points HOME and XDG_CONFIG_HOME at empty temp folders, so the
// user's git config and global excludes file never leak into a test. It
// returns the home folder.
func isolateGit(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return home
}

// repoFixture creates an empty git work tree (a .git folder with info/) in a
// temp dir and returns its top. HOME is isolated.
func repoFixture(t *testing.T) string {
	t.Helper()
	isolateGit(t)
	top := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(top, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	return top
}

// scan runs Scan and sorts each reason's paths.
func scan(t *testing.T, p *Policy) Summary {
	t.Helper()
	s, err := p.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range s.Ignored {
		slices.Sort(v)
	}
	return s
}

// walkAll returns every path Walk reports, with "" for allowed ones and the
// reason otherwise.
func walkAll(t *testing.T, p *Policy) map[string]string {
	t.Helper()
	got := map[string]string{}
	err := p.Walk(context.Background(), func(rel string, src []byte, err error) error {
		var ie *IgnoredError
		switch {
		case err == nil:
			got[rel] = ""
		case errors.As(err, &ie):
			got[rel] = ie.Reason.String()
		default:
			got[rel] = "error: " + err.Error()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestGitignoreNested(t *testing.T) {
	top := repoFixture(t)
	write(t, filepath.Join(top, "docs", ".gitignore"), "x.md\n")
	write(t, filepath.Join(top, "docs", "x.md"), "x")
	write(t, filepath.Join(top, "docs", "deep", "x.md"), "x")
	write(t, filepath.Join(top, "docs", "y.md"), "y")
	write(t, filepath.Join(top, "x.md"), "top")
	p := open(t, top, 0)
	if p.GitTop() != top {
		t.Fatalf("GitTop() = %q, want %q", p.GitTop(), top)
	}
	for _, rel := range []string{"docs/x.md", "docs/deep/x.md"} {
		_, err := p.Read(rel)
		wantReason(t, err, GitIgnore)
		if r := p.CheckName(rel); r != GitIgnore {
			t.Errorf("CheckName(%q) = %s", rel, r)
		}
	}
	for _, rel := range []string{"docs/y.md", "x.md"} {
		if _, err := p.Read(rel); err != nil {
			t.Errorf("Read(%q) = %v", rel, err)
		}
	}
	want := map[string]string{
		"x.md": "", "docs/y.md": "", "docs/x.md": ".gitignore", "docs/deep/x.md": ".gitignore", ".git/": "hidden",
	}
	if got := walkAll(t, p); !reflect.DeepEqual(got, want) {
		t.Errorf("Walk = %v\nwant %v", got, want)
	}
}

func TestGitignoreNoReinclude(t *testing.T) {
	top := repoFixture(t)
	write(t, filepath.Join(top, ".gitignore"), "build/\n!build/keep.md\n")
	write(t, filepath.Join(top, "build", "keep.md"), "k")
	p := open(t, top, 0)
	_, err := p.Read("build/keep.md")
	wantReason(t, err, GitIgnore)
	s := scan(t, p)
	if s.Allowed != 0 || !reflect.DeepEqual(s.Ignored[GitIgnore], []string{"build/"}) {
		t.Errorf("Scan = %+v", s)
	}
}

func TestGitignoreInfoExcludeAndGlobal(t *testing.T) {
	top := repoFixture(t)
	home := os.Getenv("HOME")
	write(t, filepath.Join(top, ".git", "info", "exclude"), "excluded.md\n")
	write(t, filepath.Join(home, ".gitconfig"), "[user]\n\tname = x\n[core]\n\texcludesFile = \"~/my ignore\" # comment\n")
	write(t, filepath.Join(home, "my ignore"), "global.md\n")
	write(t, filepath.Join(home, ".config", "git", "ignore"), "default.md\n")
	for _, f := range []string{"excluded.md", "global.md", "default.md", "ok.md"} {
		write(t, filepath.Join(top, f), "x")
	}
	p := open(t, top, 0)
	want := map[string]Reason{"excluded.md": GitIgnore, "global.md": GitIgnore, "default.md": Allowed, "ok.md": Allowed}
	for rel, r := range want {
		if got := p.CheckName(rel); got != r {
			t.Errorf("CheckName(%q) = %s, want %s", rel, got, r)
		}
	}

	// With the key unset, the default $XDG_CONFIG_HOME/git/ignore applies.
	write(t, filepath.Join(home, ".gitconfig"), "[user]\n\tname = x\n")
	later := time.Now().Add(2 * time.Second)
	os.Chtimes(filepath.Join(home, ".gitconfig"), later, later)
	want = map[string]Reason{"excluded.md": GitIgnore, "global.md": Allowed, "default.md": GitIgnore, "ok.md": Allowed}
	for rel, r := range want {
		if got := p.CheckName(rel); got != r {
			t.Errorf("after unsetting: CheckName(%q) = %s, want %s", rel, got, r)
		}
	}
	_, err := p.Read("default.md")
	wantReason(t, err, GitIgnore)
}

func TestGitignoreRepoConfigWins(t *testing.T) {
	top := repoFixture(t)
	home := os.Getenv("HOME")
	write(t, filepath.Join(home, ".config", "git", "config"), "[core]\nexcludesfile = /nonexistent\n")
	write(t, filepath.Join(home, ".gitconfig"), "[Core]\nExcludesFile = ~/a\n")
	write(t, filepath.Join(top, ".git", "config"), "[core]\n\texcludesfile = ~/b\n")
	write(t, filepath.Join(home, "a"), "a.md\n")
	write(t, filepath.Join(home, "b"), "b.md\n")
	p := open(t, top, 0)
	if p.CheckName("a.md") != Allowed || p.CheckName("b.md") != GitIgnore {
		t.Errorf("CheckName(a.md) = %s, CheckName(b.md) = %s", p.CheckName("a.md"), p.CheckName("b.md"))
	}
}

func TestExcludesFileParse(t *testing.T) {
	cases := []struct {
		cfg  string
		want string
		ok   bool
	}{
		{"[core]\n\texcludesfile = ~/x\n", "~/x", true},
		{"[core]\nexcludesFile=\"a b\" ; c\n", "a b", true},
		{"; c\n[core]\n# x\n  EXCLUDESFILE = p\\\"q\n", `p"q`, true},
		{"[user]\nexcludesfile = no\n", "", false},
		{"[core \"sub\"]\nexcludesfile = sub\n", "sub", true},
		{"[core]\nexcludesfile = 1\n[core]\nexcludesfile = 2\n", "2", true},
		{"[include]\npath = other\n", "", false},
	}
	for _, c := range cases {
		got, ok := excludesFile([]byte(c.cfg))
		if got != c.want || ok != c.ok {
			t.Errorf("excludesFile(%q) = %q, %v, want %q, %v", c.cfg, got, ok, c.want, c.ok)
		}
	}
}

func TestGitignoreWorktreeFile(t *testing.T) {
	isolateGit(t)
	dir := t.TempDir()
	common := filepath.Join(dir, "main", ".git")
	gitDir := filepath.Join(common, "worktrees", "wt")
	write(t, filepath.Join(common, "info", "exclude"), "from-common.md\n")
	write(t, filepath.Join(gitDir, "commondir"), "../..\n")
	top := filepath.Join(dir, "wt")
	rel, err := filepath.Rel(top, gitDir)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(top, ".git"), "gitdir: "+rel+"\n")
	write(t, filepath.Join(top, "from-common.md"), "x")
	write(t, filepath.Join(top, "ok.md"), "x")
	p := open(t, top, 0)
	_, err = p.Read("from-common.md")
	wantReason(t, err, GitIgnore)
	if _, err := p.Read("ok.md"); err != nil {
		t.Fatal(err)
	}
	// The root's own .git file is hidden like a .git folder.
	if r := p.CheckName(".git"); r != Hidden {
		t.Errorf("CheckName(.git) = %s", r)
	}
}

func TestGitFileUnparseableFailsOpen(t *testing.T) {
	isolateGit(t)
	top := t.TempDir()
	write(t, filepath.Join(top, ".git"), "not a gitdir line\n")
	if p, err := Open(top, 0); err == nil {
		p.Close()
		t.Fatal("Open succeeded with an unparseable .git file")
	} else if !strings.Contains(err.Error(), "git work tree at") {
		t.Fatalf("err = %v", err)
	}
	p, err := OpenOptions(top, Options{GitIgnore: false})
	if err != nil {
		t.Fatalf("with the git layer off: %v", err)
	}
	p.Close()
}

func TestGitignoreSubdirRoot(t *testing.T) {
	top := repoFixture(t)
	write(t, filepath.Join(top, ".gitignore"), "sub/private/\n/top.md\n")
	write(t, filepath.Join(top, "sub", "private", "a.md"), "x")
	write(t, filepath.Join(top, "sub", "top.md"), "x")
	write(t, filepath.Join(top, "sub", "ok.md"), "x")
	p := open(t, filepath.Join(top, "sub"), 0)
	if p.GitTop() != top {
		t.Fatalf("GitTop() = %q", p.GitTop())
	}
	_, err := p.Read("private/a.md")
	wantReason(t, err, GitIgnore)
	if _, err := p.Read("top.md"); err != nil {
		t.Errorf("/top.md is anchored at the top, not the root: %v", err)
	}
	s := scan(t, p)
	if s.Allowed != 2 || !reflect.DeepEqual(s.Ignored[GitIgnore], []string{"private/"}) {
		t.Errorf("Scan = %+v", s)
	}
	if r := p.SkipDir("private"); r != GitIgnore {
		t.Errorf("SkipDir(private) = %s", r)
	}
}

func TestGitignoreOff(t *testing.T) {
	top := repoFixture(t)
	write(t, filepath.Join(top, ".gitignore"), "*.md\n")
	write(t, filepath.Join(top, ".git", "info", "exclude"), "*.md\n")
	write(t, filepath.Join(top, "a.md"), "a")
	write(t, filepath.Join(top, "vendor", "lib", ".git", "HEAD"), "x")
	write(t, filepath.Join(top, "vendor", "lib", "b.md"), "b")
	p, err := OpenOptions(top, Options{GitIgnore: false})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if p.GitTop() != "" {
		t.Errorf("GitTop() = %q", p.GitTop())
	}
	s := scan(t, p)
	if s.Allowed != 2 || len(s.Ignored[GitIgnore])+len(s.Ignored[NestedRepo]) != 0 {
		t.Errorf("Scan = %+v", s)
	}
}

func TestNotAGitRepo(t *testing.T) {
	isolateGit(t)
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	write(t, filepath.Join(root, ".gitignore"), "*.md\n")
	write(t, filepath.Join(root, "a.md"), "a")
	p := open(t, root, 0)
	if p.GitTop() != "" {
		t.Skipf("the temp dir is inside a git work tree at %s", p.GitTop())
	}
	if _, err := p.Read("a.md"); err != nil {
		t.Fatal(err)
	}
	if got := walkAll(t, p); !reflect.DeepEqual(got, map[string]string{"a.md": ""}) {
		t.Errorf("Walk = %v", got)
	}
}

func TestNestedRepoPruned(t *testing.T) {
	top := repoFixture(t)
	write(t, filepath.Join(top, "vendor", "lib", ".git", "HEAD"), "x")
	write(t, filepath.Join(top, "vendor", "lib", "README.md"), "x")
	write(t, filepath.Join(top, "sub", "mod", ".git"), "gitdir: ../../.git/modules/mod\n")
	write(t, filepath.Join(top, "sub", "mod", "a.md"), "x")
	write(t, filepath.Join(top, "vendor", "ok.md"), "x")
	p := open(t, top, 0)
	s := scan(t, p)
	if s.Allowed != 1 || !reflect.DeepEqual(s.Ignored[NestedRepo], []string{"sub/mod/", "vendor/lib/"}) {
		t.Errorf("Scan = %+v", s)
	}
	_, err := p.Read("vendor/lib/README.md")
	wantReason(t, err, NestedRepo)
	if r := p.CheckName("sub/mod/a.md"); r != NestedRepo {
		t.Errorf("CheckName = %s", r)
	}
	if r := p.SkipDir("vendor/lib"); r != NestedRepo {
		t.Errorf("SkipDir = %s", r)
	}
}

func TestGitignoreReload(t *testing.T) {
	top := repoFixture(t)
	ign := filepath.Join(top, "docs", ".gitignore")
	write(t, ign, "a.md\n")
	write(t, filepath.Join(top, "docs", "a.md"), "a")
	p := open(t, top, 0)
	if r := p.CheckName("docs/a.md"); r != GitIgnore {
		t.Fatalf("CheckName = %s", r)
	}
	// Same size, later mtime: the stamp still changes.
	write(t, ign, "b.md\n")
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(ign, later, later); err != nil {
		t.Fatal(err)
	}
	if r := p.CheckName("docs/a.md"); r != Allowed {
		t.Fatalf("after edit: CheckName = %s", r)
	}
	if err := os.Remove(ign); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(top, ".gitignore"), "docs/\n")
	if r := p.CheckName("docs/a.md"); r != GitIgnore {
		t.Fatalf("after moving the rule up: CheckName = %s", r)
	}
	if r := p.SkipDir("docs"); r != GitIgnore {
		t.Fatalf("SkipDir = %s", r)
	}
}

func TestGitignoreChecksSymlinkTarget(t *testing.T) {
	top := repoFixture(t)
	write(t, filepath.Join(top, ".gitignore"), "ignored/\n")
	write(t, filepath.Join(top, "ignored", "b.md"), "b")
	link(t, "ignored/b.md", filepath.Join(top, "a.md"))
	p := open(t, top, 0)
	_, err := p.Read("a.md")
	wantReason(t, err, GitIgnore)
	if got := walkAll(t, p); got["a.md"] != ".gitignore" {
		t.Errorf("Walk = %v", got)
	}
}

func TestGitignoreSymlinkOutsideSkipsFolder(t *testing.T) {
	top := repoFixture(t)
	outside := filepath.Join(t.TempDir(), "ign")
	write(t, outside, "nothing\n")
	link(t, outside, filepath.Join(top, "docs", ".gitignore"))
	write(t, filepath.Join(top, "docs", "a.md"), "a")
	write(t, filepath.Join(top, "ok.md"), "ok")
	p := open(t, top, 0)
	s := scan(t, p)
	if s.Allowed != 1 || !reflect.DeepEqual(s.Ignored[GitIgnore], []string{"docs/"}) {
		t.Fatalf("Scan = %+v", s)
	}
	if len(s.ReadErrors) != 1 || !strings.HasPrefix(s.ReadErrors[0], `read "docs/.gitignore": `) ||
		!strings.HasSuffix(s.ReadErrors[0], "; folder skipped") {
		t.Errorf("ReadErrors = %q", s.ReadErrors)
	}
	_, err := p.Read("docs/a.md")
	wantReason(t, err, GitIgnore)
}

func TestGitignoreBadPatternNamed(t *testing.T) {
	top := repoFixture(t)
	write(t, filepath.Join(top, "docs", ".gitignore"), "ok.md\n\n[\n")
	write(t, filepath.Join(top, ".screeignore"), "[\n")
	write(t, filepath.Join(top, "docs", "a.md"), "a")
	p := open(t, top, 0)
	s := scan(t, p)
	want := []string{`.screeignore line 1: bad pattern "["`, `docs/.gitignore line 3: bad pattern "["`}
	if !reflect.DeepEqual(s.IgnoreErrors, want) {
		t.Errorf("IgnoreErrors = %q\nwant %q", s.IgnoreErrors, want)
	}
}

func TestScanBytes(t *testing.T) {
	isolateGit(t)
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	write(t, filepath.Join(root, "a.md"), "12345")
	write(t, filepath.Join(root, "b.md"), "123")
	write(t, filepath.Join(root, "c.txt"), "1234567")
	if s := scan(t, open(t, root, 0)); s.Bytes != 8 {
		t.Errorf("Bytes = %d, want 8", s.Bytes)
	}
}

func TestHiddenAllowlist(t *testing.T) {
	top := repoFixture(t)
	cases := map[string]Reason{
		".claude/agents/a.md":              Allowed,
		".github/PULL_REQUEST_TEMPLATE.md": Allowed,
		".gitlab/issue_templates/bug.md":   Allowed,
		".cursor/rules/x.md":               Allowed,
		".claude/worktrees/x/README.md":    Hidden,
		"sub/.claude/a.md":                 Hidden,
		"sub/.github/x.md":                 Hidden,
		".github/.hidden/a.md":             Hidden,
		".obsidian/a.md":                   Hidden,
		".git/x.md":                        Hidden,
		".github/workflows/ci.yml":         NotMarkdown,
	}
	for rel := range cases {
		write(t, filepath.Join(top, filepath.FromSlash(rel)), "x")
	}
	p := open(t, top, 0)
	for rel, want := range cases {
		if got := p.CheckName(rel); got != want {
			t.Errorf("CheckName(%q) = %s, want %s", rel, got, want)
		}
		_, err := p.Read(rel)
		if want == Allowed {
			if err != nil {
				t.Errorf("Read(%q) = %v", rel, err)
			}
			continue
		}
		var ie *IgnoredError
		if !errors.As(err, &ie) || ie.Reason != want {
			t.Errorf("Read(%q) = %v, want %s", rel, err, want)
		}
	}
	want := map[string]string{
		".claude/agents/a.md": "", ".github/PULL_REQUEST_TEMPLATE.md": "", ".gitlab/issue_templates/bug.md": "",
		".cursor/rules/x.md": "", ".claude/worktrees/": "hidden", "sub/.claude/": "hidden", "sub/.github/": "hidden",
		".github/.hidden/": "hidden", ".obsidian/": "hidden", ".git/": "hidden", ".github/workflows/ci.yml": "not markdown",
	}
	if got := walkAll(t, p); !reflect.DeepEqual(got, want) {
		t.Errorf("Walk = %v\nwant %v", got, want)
	}
	for rel, want := range map[string]Reason{".github": Allowed, ".claude": Allowed, ".claude/worktrees": Hidden, "sub/.github": Hidden, ".obsidian": Hidden} {
		if got := p.SkipDir(rel); got != want {
			t.Errorf("SkipDir(%q) = %s, want %s", rel, got, want)
		}
	}
}

func TestMdcIsMarkdown(t *testing.T) {
	isolateGit(t)
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	write(t, filepath.Join(root, ".cursor", "rules", "style.mdc"), "---\ndescription: x\n---\nrule")
	write(t, filepath.Join(root, "x.mdc"), "x")
	write(t, filepath.Join(root, "Y.MDC"), "x")
	p := open(t, root, 0)
	for _, rel := range []string{".cursor/rules/style.mdc", "x.mdc", "Y.MDC"} {
		if r := p.CheckName(rel); r != Allowed {
			t.Errorf("CheckName(%q) = %s", rel, r)
		}
		if _, err := p.Read(rel); err != nil {
			t.Errorf("Read(%q) = %v", rel, err)
		}
	}
	if s := scan(t, p); s.Allowed != 3 {
		t.Errorf("Scan = %+v", s)
	}
}

func TestAllowlistOnlyForDirs(t *testing.T) {
	isolateGit(t)
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	write(t, filepath.Join(root, ".github"), "a file, not a folder")
	p := open(t, root, 0)
	if r := p.CheckName(".github"); r != Hidden {
		t.Errorf("CheckName(.github) = %s", r)
	}
	if got := walkAll(t, p); !reflect.DeepEqual(got, map[string]string{".github": "hidden"}) {
		t.Errorf("Walk = %v", got)
	}
}

func TestAllowlistRulesStillApply(t *testing.T) {
	top := repoFixture(t)
	write(t, filepath.Join(top, ".gitignore"), ".claude/local/\n")
	write(t, filepath.Join(top, ".screeignore"), "/.github/drafts/\n")
	write(t, filepath.Join(top, ".claude", "local", "a.md"), "x")
	write(t, filepath.Join(top, ".github", "drafts", "a.md"), "x")
	write(t, filepath.Join(top, ".github", "fm.md"), "---\nscree: ignore\n---\nx")
	write(t, filepath.Join(top, ".github", "big.md"), strings.Repeat("x", 200))
	write(t, filepath.Join(top, ".github", "ok.md"), "ok")
	p := open(t, top, 100)
	for rel, want := range map[string]Reason{
		".claude/local/a.md": GitIgnore, ".github/drafts/a.md": IgnoreFile, ".github/fm.md": FrontMatter, ".github/big.md": TooLarge,
	} {
		_, err := p.Read(rel)
		wantReason(t, err, want)
	}
	want := map[string]string{
		".git/": "hidden", ".claude/local/": ".gitignore", ".github/drafts/": ".screeignore",
		".github/fm.md": "front matter", ".github/big.md": "too large", ".github/ok.md": "",
	}
	if got := walkAll(t, p); !reflect.DeepEqual(got, want) {
		t.Errorf("Walk = %v\nwant %v", got, want)
	}
}

func TestAllowlistSymlinkTarget(t *testing.T) {
	isolateGit(t)
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	write(t, filepath.Join(root, "sub", ".github", "x.md"), "x")
	link(t, "sub/.github/x.md", filepath.Join(root, "a.md"))
	p := open(t, root, 0)
	_, err := p.Read("a.md")
	wantReason(t, err, Hidden)
}

func TestIncludeHidden(t *testing.T) {
	isolateGit(t)
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	ign := filepath.Join(root, ".screeignore")
	write(t, ign, "include-hidden .foo/\n")
	write(t, filepath.Join(root, ".foo", "a.md"), "a")
	write(t, filepath.Join(root, ".foo", ".bar", "a.md"), "x")
	write(t, filepath.Join(root, "sub", ".foo", "a.md"), "x")
	write(t, filepath.Join(root, "include-hidden .foo", "a.md"), "a")
	p := open(t, root, 0)
	// The directive is not also a pattern.
	for _, rel := range []string{".foo/a.md", "include-hidden .foo/a.md"} {
		if _, err := p.Read(rel); err != nil {
			t.Fatalf("Read(%q) = %v", rel, err)
		}
	}
	for _, rel := range []string{".foo/.bar/a.md", "sub/.foo/a.md"} {
		_, err := p.Read(rel)
		wantReason(t, err, Hidden)
	}
	s := scan(t, p)
	if s.Allowed != 2 || len(s.IgnoreErrors) != 0 ||
		!reflect.DeepEqual(s.Ignored[Hidden], []string{".foo/.bar/", "sub/.foo/"}) {
		t.Errorf("Scan = %+v", s)
	}

	write(t, ign, "# no directive\n")
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(ign, later, later); err != nil {
		t.Fatal(err)
	}
	_, err := p.Read(".foo/a.md")
	wantReason(t, err, Hidden)
	if r := p.CheckName(".foo/a.md"); r != Hidden {
		t.Errorf("CheckName after reload = %s", r)
	}
}

func TestIncludeHiddenBad(t *testing.T) {
	isolateGit(t)
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	bad := []string{"foo/", ".a/b/", ".git/", ".*/", ".foo", "./", "../", ""}
	var lines []string
	for _, b := range bad {
		lines = append(lines, strings.TrimSpace("include-hidden "+b))
	}
	write(t, filepath.Join(root, ".screeignore"), strings.Join(lines, "\n")+"\n")
	for _, d := range []string{"foo", ".a", ".git", ".foo", ".x"} {
		write(t, filepath.Join(root, d, "a.md"), "x")
	}
	p := open(t, root, 0)
	s := scan(t, p)
	var want []string
	for i, b := range bad {
		want = append(want, ".screeignore line "+string(rune('1'+i))+`: bad include-hidden "`+b+`"`)
	}
	if !reflect.DeepEqual(s.IgnoreErrors, want) {
		t.Errorf("IgnoreErrors = %q\nwant %q", s.IgnoreErrors, want)
	}
	if s.Allowed != 1 { // foo/a.md, which was never hidden
		t.Errorf("Scan = %+v", s)
	}
}

func TestIncludeHiddenLiteralEscape(t *testing.T) {
	isolateGit(t)
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	write(t, filepath.Join(root, ".screeignore"), "\\include-hidden .foo/\n")
	write(t, filepath.Join(root, ".foo", "a.md"), "x")
	write(t, filepath.Join(root, "include-hidden .foo", "a.md"), "x")
	p := open(t, root, 0)
	_, err := p.Read(".foo/a.md")
	wantReason(t, err, Hidden)
	_, err = p.Read("include-hidden .foo/a.md")
	wantReason(t, err, IgnoreFile)
}

func TestIncludeHiddenCannotReopenWorktrees(t *testing.T) {
	isolateGit(t)
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	write(t, filepath.Join(root, ".screeignore"), "include-hidden .claude/\n!.claude/worktrees/\n!**/README.md\n")
	write(t, filepath.Join(root, ".claude", "worktrees", "w", "README.md"), "x")
	write(t, filepath.Join(root, ".claude", "agents", "a.md"), "x")
	p := open(t, root, 0)
	_, err := p.Read(".claude/worktrees/w/README.md")
	wantReason(t, err, Hidden)
	want := map[string]string{".claude/worktrees/": "hidden", ".claude/agents/a.md": ""}
	if got := walkAll(t, p); !reflect.DeepEqual(got, want) {
		t.Errorf("Walk = %v", got)
	}
}

func TestRemoveDefaultViaPattern(t *testing.T) {
	isolateGit(t)
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	write(t, filepath.Join(root, ".screeignore"), "/.cursor/\n")
	write(t, filepath.Join(root, ".cursor", "x.md"), "x")
	p := open(t, root, 0)
	_, err := p.Read(".cursor/x.md")
	wantReason(t, err, IgnoreFile)
	if got := walkAll(t, p); !reflect.DeepEqual(got, map[string]string{".cursor/": ".screeignore"}) {
		t.Errorf("Walk = %v", got)
	}
}

func TestPatternLineNumbersWithDirective(t *testing.T) {
	isolateGit(t)
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	write(t, filepath.Join(root, ".screeignore"), "include-hidden .foo/\nok.md\n[\n")
	p := open(t, root, 0)
	s := scan(t, p)
	if want := []string{`.screeignore line 3: bad pattern "["`}; !reflect.DeepEqual(s.IgnoreErrors, want) {
		t.Errorf("IgnoreErrors = %q, want %q", s.IgnoreErrors, want)
	}
}
