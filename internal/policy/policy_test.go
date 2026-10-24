package policy

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// tempDir returns a fresh temp dir with symlinks resolved (macOS: /var ->
// /private/var), so it compares equal to the resolved paths the code under
// test returns.
func tempDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// fixture creates dir/notes and dir/outside, and returns dir.
func fixture(t *testing.T) string {
	t.Helper()
	dir := tempDir(t)
	for _, d := range []string{"notes", "outside"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(dir, "outside", "secret.md"), "outside secret")
	return dir
}

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func link(t *testing.T, target, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, name); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

func open(t *testing.T, root string, max int64) *Policy {
	t.Helper()
	p, err := Open(root, max)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

func wantReason(t *testing.T, err error, want Reason) {
	t.Helper()
	var ie *IgnoredError
	if !errors.As(err, &ie) {
		t.Fatalf("err = %v, want IgnoredError(%s)", err, want)
	}
	if ie.Reason != want {
		t.Fatalf("reason = %s, want %s (%v)", ie.Reason, want, err)
	}
	if !errors.Is(err, ErrIgnored) {
		t.Fatalf("errors.Is(%v, ErrIgnored) = false", err)
	}
}

func TestReadAllowed(t *testing.T) {
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	write(t, filepath.Join(root, "sub", "a.md"), "hello")
	p := open(t, root, 0)
	src, err := p.Read("sub/a.md")
	if err != nil || string(src) != "hello" {
		t.Fatalf("Read = %q, %v", src, err)
	}
	if _, err := p.Read("missing.md"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file: err = %v, want ErrNotExist", err)
	}
}

func TestReadRefusesOutsideSymlink(t *testing.T) {
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	link(t, filepath.Join(dir, "outside", "secret.md"), filepath.Join(root, "x.md"))
	link(t, "../outside", filepath.Join(root, "d"))
	p := open(t, root, 0)
	for _, rel := range []string{"x.md", "d/secret.md"} {
		src, err := p.Read(rel)
		if src != nil {
			t.Errorf("Read(%s) returned content", rel)
		}
		wantReason(t, err, OutsideRoot)
	}
}

func TestReadAllowsInRootSymlink(t *testing.T) {
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	write(t, filepath.Join(root, "a.md"), "target")
	link(t, "a.md", filepath.Join(root, "rel.md"))
	link(t, filepath.Join(root, "a.md"), filepath.Join(root, "abs.md"))
	link(t, "../a.md", filepath.Join(root, "sub", "up.md"))
	p := open(t, root, 0)
	for _, rel := range []string{"rel.md", "abs.md", "sub/up.md"} {
		src, err := p.Read(rel)
		if err != nil || string(src) != "target" {
			t.Errorf("Read(%s) = %q, %v", rel, src, err)
		}
	}
}

func TestReadChecksResolvedTarget(t *testing.T) {
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	write(t, filepath.Join(root, ".git", "config"), "[remote]")
	write(t, filepath.Join(root, "notes.txt"), "plain")
	write(t, filepath.Join(root, "journal", "secret.md"), "private")
	write(t, filepath.Join(root, ".screeignore"), "journal/\n")
	link(t, ".git/config", filepath.Join(root, "gitcfg.md"))
	link(t, "notes.txt", filepath.Join(root, "l.md"))
	link(t, "journal/secret.md", filepath.Join(root, "pub.md"))
	p := open(t, root, 0)
	for rel, want := range map[string]Reason{"gitcfg.md": Hidden, "l.md": NotMarkdown, "pub.md": IgnoreFile} {
		_, err := p.Read(rel)
		wantReason(t, err, want)
	}
}

func TestSizeCap(t *testing.T) {
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	write(t, filepath.Join(root, "ok.md"), strings.Repeat("x", 100))
	write(t, filepath.Join(root, "big.md"), strings.Repeat("x", 101))
	p := open(t, root, 100)
	if src, err := p.Read("ok.md"); err != nil || len(src) != 100 {
		t.Fatalf("Read(ok.md) = %d bytes, %v", len(src), err)
	}
	_, err := p.Read("big.md")
	wantReason(t, err, TooLarge)
}

func TestUnsafeName(t *testing.T) {
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	p := open(t, root, 0)
	created := 0
	for _, name := range []string{"a\nb.md", "a\x1b[31m.md", "\xff.md", "a\u009b.md", "a\x7f.md"} {
		// Some filesystems refuse some of these names (macOS APFS rejects
		// invalid UTF-8). The name checks below need no file, so they still
		// run; Scan can only count the files that exist.
		if os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644) == nil {
			created++
		}
		_, err := p.Read(name)
		wantReason(t, err, UnsafeName)
		if r := p.CheckName(name); r != UnsafeName {
			t.Errorf("CheckName(%q) = %s", name, r)
		}
	}
	s, err := p.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Ignored[UnsafeName]) != created || s.Allowed != 0 {
		t.Errorf("Scan = %+v, want %d unsafe names", s, created)
	}
}

func TestCleanRejects(t *testing.T) {
	p := &Policy{}
	for _, bad := range []string{"../etc/passwd", "a/../../x.md", "/etc/passwd", "", "a/./b.md", "a//b.md", "."} {
		_, err := p.Clean(bad)
		wantReason(t, err, Invalid)
	}
	for in, want := range map[string]string{"sub/a.md": "sub/a.md", "./a.md": "a.md"} {
		if got, err := p.Clean(in); err != nil || got != want {
			t.Errorf("Clean(%q) = %q, %v", in, got, err)
		}
	}
}

func TestCheckName(t *testing.T) {
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	write(t, filepath.Join(root, ".screeignore"), "private.md\n")
	p := open(t, root, 0)
	if _, err := p.Read("x.md"); !errors.Is(err, fs.ErrNotExist) { // loads the ignore file
		t.Fatal(err)
	}
	for rel, want := range map[string]Reason{
		"a.md": Allowed, "../a.md": Invalid, ".obsidian/a.md": Hidden,
		"a.txt": NotMarkdown, "x/private.md": IgnoreFile, ".screeignore": Hidden,
	} {
		if got := p.CheckName(rel); got != want {
			t.Errorf("CheckName(%q) = %s, want %s", rel, got, want)
		}
	}
}

func TestScreeignoreReload(t *testing.T) {
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	ign := filepath.Join(root, ".screeignore")
	write(t, filepath.Join(root, "a.md"), "a")
	write(t, ign, "a.md\n")
	p := open(t, root, 0)
	_, err := p.Read("a.md")
	wantReason(t, err, IgnoreFile)

	// Same size, later mtime: the stamp still changes.
	write(t, ign, "b.md\n")
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(ign, later, later); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Read("a.md"); err != nil {
		t.Fatalf("after edit: %v", err)
	}
	write(t, ign, "*.md\n")
	_, err = p.Read("a.md")
	wantReason(t, err, IgnoreFile)
	if err := os.Remove(ign); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Read("a.md"); err != nil {
		t.Fatalf("after remove: %v", err)
	}
}

func TestScreeignoreUnreadableFailsClosed(t *testing.T) {
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	write(t, filepath.Join(dir, "outside", "ign"), "nothing.md\n")
	write(t, filepath.Join(root, "a.md"), "a")
	link(t, "../outside/ign", filepath.Join(root, ".screeignore"))
	p := open(t, root, 0)
	src, err := p.Read("a.md")
	if err == nil || src != nil {
		t.Fatalf("Read = %q, %v; want error", src, err)
	}
	if errors.Is(err, ErrIgnored) {
		t.Fatalf("err = %v, want a read error", err)
	}
	called := false
	err = p.Walk(context.Background(), func(string, []byte, error) error { called = true; return nil })
	if err == nil || called {
		t.Fatalf("Walk err = %v, called = %v", err, called)
	}
	if _, err := p.Scan(context.Background()); err == nil {
		t.Fatal("Scan succeeded")
	}
}

func TestScreeignoreBadPatterns(t *testing.T) {
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	write(t, filepath.Join(root, ".screeignore"), "[\n")
	p := open(t, root, 0)
	s, err := p.Scan(context.Background())
	if err != nil || len(s.IgnoreErrors) != 1 {
		t.Fatalf("Scan = %+v, %v", s, err)
	}
}

func TestWalkReportsReasons(t *testing.T) {
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	write(t, filepath.Join(root, "ok.md"), "ok")
	write(t, filepath.Join(root, "sub", "ok2.md"), "ok2")
	write(t, filepath.Join(root, ".git", "config"), "x")
	write(t, filepath.Join(root, ".git", "HEAD"), "x")
	write(t, filepath.Join(root, ".hidden.md"), "x")
	write(t, filepath.Join(root, "notes.txt"), "x")
	write(t, filepath.Join(root, "big.md"), strings.Repeat("x", 200))
	write(t, filepath.Join(root, "journal", "a.md"), "x")
	write(t, filepath.Join(root, "journal", "deep", "b.md"), "x")
	write(t, filepath.Join(root, "private.md"), "x")
	write(t, filepath.Join(root, "fm.md"), "---\nscree: ignore\n---\nx")
	write(t, filepath.Join(root, "bad\x1bname.md"), "x")
	write(t, filepath.Join(root, ".screeignore"), "journal/\nprivate.md\n")
	link(t, "../outside/secret.md", filepath.Join(root, "out.md"))
	link(t, "../outside", filepath.Join(root, "outdir"))
	link(t, "sub", filepath.Join(root, "indir"))
	link(t, "ok.md", filepath.Join(root, "alias.md"))
	p := open(t, root, 100)

	allowed := map[string]string{}
	err := p.Walk(context.Background(), func(rel string, src []byte, err error) error {
		if err == nil {
			allowed[rel] = string(src)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	wantAllowed := map[string]string{"ok.md": "ok", "sub/ok2.md": "ok2", "alias.md": "ok"}
	if !reflect.DeepEqual(allowed, wantAllowed) {
		t.Errorf("allowed = %v, want %v", allowed, wantAllowed)
	}

	s, err := p.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[Reason][]string{
		UnsafeName:  {"bad\x1bname.md"},
		Hidden:      {".git/", ".hidden.md"},
		NotMarkdown: {"notes.txt"},
		OutsideRoot: {"out.md", "outdir/"},
		TooLarge:    {"big.md"},
		IgnoreFile:  {"journal/", "private.md"},
		FrontMatter: {"fm.md"},
	}
	for _, v := range s.Ignored {
		slices.Sort(v)
	}
	if s.Allowed != 3 || !reflect.DeepEqual(s.Ignored, want) {
		t.Errorf("Scan = %d allowed, %q\nwant 3, %q", s.Allowed, s.Ignored, want)
	}
}

func TestWalkNegationDescends(t *testing.T) {
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	write(t, filepath.Join(root, "journal", "x.md"), "x")
	write(t, filepath.Join(root, "journal", "public.md"), "pub")
	write(t, filepath.Join(root, ".screeignore"), "journal/\n!journal/public.md\n")
	p := open(t, root, 0)
	s, err := p.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Allowed != 1 || !reflect.DeepEqual(s.Ignored[IgnoreFile], []string{"journal/x.md"}) {
		t.Errorf("Scan = %+v", s)
	}
}

func TestRel(t *testing.T) {
	dir := fixture(t)
	real := filepath.Join(dir, "notes")
	given := filepath.Join(dir, "via")
	link(t, real, given)
	p := open(t, given, 0)
	// The temp dir itself may sit behind a symlink (macOS: /var -> /private/var).
	if want, _ := filepath.EvalSymlinks(real); p.Root() != want {
		t.Fatalf("Root() = %s, want %s", p.Root(), want)
	}
	for _, abs := range []string{filepath.Join(real, "a", "b.md"), filepath.Join(given, "a", "b.md")} {
		if rel, err := p.Rel(abs); err != nil || rel != "a/b.md" {
			t.Errorf("Rel(%s) = %q, %v", abs, rel, err)
		}
	}
	if _, err := p.Rel(filepath.Join(dir, "outside", "secret.md")); err == nil {
		t.Error("Rel accepted a path outside the root")
	}
	// A path that reaches the root through some other symlink, such as
	// macOS's /var -> /private/var, still maps to the same key.
	alt := filepath.Join(tempDir(t), "alt")
	link(t, dir, alt)
	if rel, err := p.Rel(filepath.Join(alt, "notes", "new", "c.md")); err != nil || rel != "new/c.md" {
		t.Errorf("Rel through another symlink = %q, %v", rel, err)
	}
	if _, err := p.Rel(filepath.Join(alt, "outside", "secret.md")); err == nil {
		t.Error("Rel accepted an outside path through another symlink")
	}
}

func TestCheckRoot(t *testing.T) {
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	p := open(t, root, 0)
	if err := p.CheckRoot(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, filepath.Join(dir, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := p.CheckRoot(); err == nil {
		t.Fatal("CheckRoot succeeded after the root was moved")
	}
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := p.CheckRoot(); err == nil {
		t.Fatal("CheckRoot succeeded after the root was replaced")
	}
}

func TestReasonStrings(t *testing.T) {
	if s := (&IgnoredError{Path: "a.md", Reason: TooLarge}).Error(); s != `"a.md" is ignored (too large)` {
		t.Errorf("Error() = %s", s)
	}
	for r, layer := range map[Reason]string{
		Hidden: "built-in", OutsideRoot: "built-in", GitIgnore: ".gitignore", NestedRepo: ".gitignore",
		IgnoreFile: ".screeignore", FrontMatter: "front matter",
	} {
		if r.Layer() != layer {
			t.Errorf("%s.Layer() = %s", r, r.Layer())
		}
	}
	if OutsideRoot.String() != "outside the folder" {
		t.Error(OutsideRoot.String())
	}
	if GitIgnore.String() != ".gitignore" || NestedRepo.String() != "nested repository" {
		t.Error(GitIgnore.String(), NestedRepo.String())
	}
	if !(TooLarge < GitIgnore && GitIgnore < NestedRepo && NestedRepo < IgnoreFile) {
		t.Error("git reasons are out of order")
	}
}

// TestWalkStatWith: the files extra accepts are walked under every other
// rule, a symlink to outside the folder included, while Read, Check and
// CheckName still refuse them.
func TestWalkStatWith(t *testing.T) {
	dir := fixture(t)
	notes := filepath.Join(dir, "notes")
	write(t, filepath.Join(dir, "outside", "out.go"), "package out")
	write(t, filepath.Join(notes, "a.go"), "package a")
	write(t, filepath.Join(notes, "n.md"), "# n")
	write(t, filepath.Join(notes, ".hidden", "h.go"), "package h")
	write(t, filepath.Join(notes, "skip", "s.go"), "package s")
	write(t, filepath.Join(notes, "a.txt"), "not accepted")
	write(t, filepath.Join(notes, IgnoreFileName), "skip/\n")
	link(t, filepath.Join(dir, "outside", "out.go"), filepath.Join(notes, "link.go"))
	p, err := Open(notes, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	var got []string
	err = p.WalkStatWith(context.Background(), func(rel string) bool { return strings.HasSuffix(rel, ".go") },
		func(rel string, src []byte, _ fs.FileInfo, err error) error {
			if err == nil {
				got = append(got, rel+"="+string(src))
			}
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a.go=package a", "n.md=# n"}; !slices.Equal(got, want) {
		t.Errorf("walked %q, want %q", got, want)
	}
	if _, err := p.Read("a.go"); !errors.Is(err, ErrIgnored) {
		t.Errorf("Read(a.go) = %v", err)
	}
	if _, err := p.Check("a.go"); !errors.Is(err, ErrIgnored) {
		t.Errorf("Check(a.go) = %v", err)
	}
	if r := p.CheckName("a.go"); r != NotMarkdown {
		t.Errorf("CheckName(a.go) = %v", r)
	}
}
