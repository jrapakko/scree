package policy

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// caseInsensitive reports whether names in dir are compared without case,
// by creating a probe file and looking it up in upper case.
func caseInsensitive(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "case-probe")
	write(t, probe, "x")
	defer os.Remove(probe)
	a, err := os.Lstat(probe)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Lstat(filepath.Join(dir, "CASE-PROBE"))
	return err == nil && os.SameFile(a, b)
}

func TestCanonicalExactAndMissing(t *testing.T) {
	isolateGit(t)
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	write(t, filepath.Join(root, "Docs", "a.md"), "a")
	link(t, "Docs", filepath.Join(root, "l"))
	p := open(t, root, 0)
	for in, want := range map[string]string{
		"Docs/a.md":       "Docs/a.md",
		"./Docs/a.md":     "Docs/a.md",
		"Docs/missing.md": "Docs/missing.md",
		"gone/x/y.md":     "gone/x/y.md",
		"l/a.md":          "l/a.md",
	} {
		if got, err := p.Canonical(in); err != nil || got != want {
			t.Errorf("Canonical(%q) = %q, %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"../x.md", "a\x1b.md"} {
		if _, err := p.Canonical(bad); !errors.Is(err, ErrIgnored) {
			t.Errorf("Canonical(%q) = %v, want a refusal", bad, err)
		}
	}
	link(t, "../outside", filepath.Join(root, "out"))
	if got, err := p.Canonical("out/secret.md"); err != nil || got != "out/secret.md" {
		t.Errorf("Canonical through an outside link = %q, %v", got, err)
	}
	_, err := p.Read("out/secret.md")
	wantReason(t, err, OutsideRoot)
}

// TestCanonicalCase runs where the temp dir is case-insensitive: macOS and
// Windows by default, or a casefold folder on Linux (see the step 4b notes).
func TestCanonicalCase(t *testing.T) {
	isolateGit(t)
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	if !caseInsensitive(t, root) {
		t.Skip("the temp dir is case-sensitive")
	}
	write(t, filepath.Join(root, ".screeignore"), "private/\n")
	write(t, filepath.Join(root, "private", "tax.md"), "secret")
	write(t, filepath.Join(root, "Notes", "a.md"), "a")
	write(t, filepath.Join(root, ".GITHUB", "x.md"), "x")
	p := open(t, root, 0)
	if !p.FoldsCase() {
		t.Fatal("the probe did not detect a case-insensitive folder")
	}
	for _, rel := range []string{"PRIVATE/tax.md", "Private/TAX.md", "private/tax.md"} {
		src, err := p.Read(rel)
		if src != nil {
			t.Fatalf("Read(%q) returned the content", rel)
		}
		wantReason(t, err, IgnoreFile)
		var ie *IgnoredError
		errors.As(err, &ie)
		if ie.Path != "private/tax.md" {
			t.Errorf("Read(%q) refused as %q, want the on-disk name", rel, ie.Path)
		}
		if r := p.CheckName(rel); r != IgnoreFile {
			t.Errorf("CheckName(%q) = %s", rel, r)
		}
	}
	if got, err := p.Canonical("notes/A.MD"); err != nil || got != "Notes/a.md" {
		t.Errorf("Canonical = %q, %v", got, err)
	}
	// The allowlist compares the on-disk name exactly.
	_, err := p.Read(".github/x.md")
	wantReason(t, err, Hidden)
}

func TestForcedFoldCase(t *testing.T) {
	top := repoFixture(t)
	write(t, filepath.Join(top, ".screeignore"), "PRIVATE/\n")
	write(t, filepath.Join(top, ".gitignore"), "Build/\n*.TMP.md\n")
	write(t, filepath.Join(top, "private", "a.md"), "x")
	write(t, filepath.Join(top, "build", "b.md"), "x")
	write(t, filepath.Join(top, "c.tmp.md"), "x")
	write(t, filepath.Join(top, "ok.md"), "x")
	p := open(t, top, 0)
	if p.FoldsCase() {
		t.Skip("the temp dir is case-insensitive; TestCanonicalCase covers it")
	}
	// Exact matching: none of the differently cased patterns apply.
	if s := scan(t, p); s.Allowed != 4 {
		t.Fatalf("exact: Scan = %+v", s)
	}
	p2, err := OpenOptions(top, Options{GitIgnore: true})
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	p2.opts.FoldCase = true
	p2.git.opts = p2.opts
	s := scan(t, p2)
	want := map[Reason][]string{Hidden: {".git/"}, GitIgnore: {"build/", "c.tmp.md"}, IgnoreFile: {"private/"}}
	if s.Allowed != 1 || !reflect.DeepEqual(s.Ignored, want) {
		t.Errorf("folded: Scan = %+v", s)
	}
}

func TestCanonicalUnicode(t *testing.T) {
	isolateGit(t)
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	nfc, nfd := "café.md", "café.md"
	if err := os.WriteFile(filepath.Join(root, nfd), []byte("x"), 0o644); err != nil {
		t.Skipf("cannot create an NFD name: %v", err)
	}
	p := open(t, root, 0)
	got, err := p.Canonical(nfc)
	if err != nil {
		t.Fatal(err)
	}
	if _, serr := os.Lstat(filepath.Join(root, nfc)); serr == nil {
		// The filesystem treats the two as one name (APFS, casefold ext4).
		if got != nfd {
			t.Errorf("Canonical(NFC) = %q, want the on-disk NFD name", got)
		}
		if _, err := p.Read(nfc); err != nil {
			t.Errorf("Read(NFC) = %v", err)
		}
		return
	}
	if got != nfc {
		t.Errorf("Canonical(NFC) = %q", got)
	}
	if _, err := p.Read(nfc); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Read(NFC) = %v, want no such file", err)
	}
}

func TestNFCPatternMatchesNFDName(t *testing.T) {
	top := repoFixture(t)
	nfcDir, nfdDir := "café", "café"
	write(t, filepath.Join(top, ".screeignore"), nfcDir+"/\n")
	write(t, filepath.Join(top, ".gitignore"), "résumé.md\n")
	if err := os.MkdirAll(filepath.Join(top, nfdDir), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(top, nfdDir, "x.md"), "x")
	write(t, filepath.Join(top, "résumé.md"), "x")
	p := open(t, top, 0)
	got := walkAll(t, p)
	want := map[string]string{".git/": "hidden", nfdDir + "/": ".screeignore", "résumé.md": ".gitignore"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Walk = %q\nwant %q", got, want)
	}
}

func TestHiddenAllowlistExactCase(t *testing.T) {
	isolateGit(t)
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	write(t, filepath.Join(root, ".GITHUB", "x.md"), "x")
	write(t, filepath.Join(root, ".Claude", "a.md"), "x")
	p := open(t, root, 0)
	for _, rel := range []string{".GITHUB/x.md", ".Claude/a.md"} {
		_, err := p.Read(rel)
		wantReason(t, err, Hidden)
	}
	want := map[string]string{".GITHUB/": "hidden", ".Claude/": "hidden"}
	if got := walkAll(t, p); !reflect.DeepEqual(got, want) {
		t.Errorf("Walk = %v", got)
	}
}

func TestCanonicalKeepsUnsafeCheck(t *testing.T) {
	// A safe spelling cannot map to an on-disk name with control characters
	// on any filesystem scree knows (they have no case and no other
	// normalization), so Canonical's final check is a backstop. Both checks
	// refuse an unsafe name.
	isolateGit(t)
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	p := open(t, root, 0)
	for _, name := range []string{"a\x1b.md", "d/\u009b.md"} {
		_, err := p.Canonical(name)
		wantReason(t, err, UnsafeName)
		if r := p.CheckName(name); r != UnsafeName {
			t.Errorf("CheckName(%q) = %s", name, r)
		}
	}
}

// withHook sets a TestHooks field for the rest of the test.
func withHook(t *testing.T, h *func(string), fn func(string)) {
	t.Helper()
	*h = fn
	t.Cleanup(func() { *h = nil })
}

func TestReadRaceSwapToSymlink(t *testing.T) {
	isolateGit(t)
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	write(t, filepath.Join(root, "zzz.md"), "plain")
	write(t, filepath.Join(root, ".env"), "OPENROUTER_API_KEY=sk-secret")
	p := open(t, root, 0)
	swapped := false
	withHook(t, &TestHooks.BeforeOpen, func(rel string) {
		if rel != "zzz.md" || swapped {
			return
		}
		swapped = true
		os.Remove(filepath.Join(root, "zzz.md"))
		if err := os.Symlink(".env", filepath.Join(root, "zzz.md")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	})
	check := func(what string, src []byte, err error) {
		t.Helper()
		if strings.Contains(string(src), "sk-secret") {
			t.Fatalf("%s: read .env through the swapped link", what)
		}
		if !errors.Is(err, ErrChanged) {
			t.Fatalf("%s: err = %v, want ErrChanged", what, err)
		}
	}
	src, err := p.Read("zzz.md")
	check("Read", src, err)

	// The same swap during a walk.
	os.Remove(filepath.Join(root, "zzz.md"))
	write(t, filepath.Join(root, "zzz.md"), "plain")
	swapped = false
	err = p.Walk(context.Background(), func(rel string, src []byte, err error) error {
		if rel == "zzz.md" {
			check("Walk", src, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestReadRaceSwapFolder(t *testing.T) {
	isolateGit(t)
	dir := fixture(t)
	root := filepath.Join(dir, "notes")
	write(t, filepath.Join(root, "a", "x.md"), "plain")
	write(t, filepath.Join(root, ".secret", "x.md"), "hidden secret")
	p := open(t, root, 0)
	swapped := false
	withHook(t, &TestHooks.BeforeLstat, func(rel string) {
		if rel != "a/x.md" || swapped {
			return
		}
		swapped = true
		if err := os.RemoveAll(filepath.Join(root, "a")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(".secret", filepath.Join(root, "a")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	})
	for _, how := range []string{"Read", "Walk"} {
		swapped = false
		os.Remove(filepath.Join(root, "a"))
		write(t, filepath.Join(root, "a", "x.md"), "plain")
		var src []byte
		var err error
		if how == "Read" {
			src, err = p.Read("a/x.md")
		} else {
			werr := p.Walk(context.Background(), func(rel string, s []byte, e error) error {
				if rel == "a/x.md" {
					src, err = s, e
				}
				return nil
			})
			if werr != nil {
				t.Fatal(werr)
			}
		}
		if strings.Contains(string(src), "secret") {
			t.Fatalf("%s: read the hidden folder through the swapped link", how)
		}
		if !errors.Is(err, ErrChanged) {
			t.Fatalf("%s: err = %v, want ErrChanged", how, err)
		}
	}
}
