package index

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jrapakko/scree/internal/policy"
	"github.com/jrapakko/scree/internal/store"
	"github.com/jrapakko/scree/internal/vocab"
)

// setupRepo is setup over a git work tree, with a recording embedder.
func setupRepo(t *testing.T) (*Indexer, *recordingEmbedder) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "repo")
	if err := os.MkdirAll(filepath.Join(root, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	p, err := policy.Open(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	re := &recordingEmbedder{}
	return &Indexer{Policy: p, Store: s, Embedder: re}, re
}

// codeFixture is a small repo with code, manifests and docs, and code that
// must contribute nothing: gitignored, vendored, hidden, .screeignored and
// too large.
var codeFixture = map[string]string{
	"README.md":                     "# Demo\nThe PageFetcher fetches pages. See apple.\n",
	"docs/guide.md":                 "# Guide\nUse psycopg for the database. banana\n",
	"requirements.txt":              "psycopg[binary]>=3\n",
	"web/package.json":              `{"dependencies": {"zustand": "^4"}}`,
	"go.mod":                        "module example.com/demo\n\nrequire golang.org/x/sync v0.7.0\n",
	"app/engine.py":                 "class PageFetcher:\n    def fetchPage(self):\n        secretLocal = 1\n",
	"web/src/hooks.ts":              "export function useCartTotal() { return 0 }\n",
	"internal/x/x.go":               "package x\n\ntype FileHash [32]byte\n",
	"build/gen.go":                  "package gen\n\nfunc GeneratedThing() {}\n",
	"vendor/lib/lib.go":             "package lib\n\nfunc VendoredThing() {}\n",
	"web/node_modules/pkg/index.js": "export function nodeModuleThing() {}\n",
	".hidden/h.py":                  "def hiddenThing(): pass\n",
	"private/p.py":                  "def privateThing(): pass\n",
	".gitignore":                    "build/\n",
	".screeignore":                  "private/\n",
}

func writeFixture(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, src := range files {
		write(t, root, rel, src)
	}
}

func vocabNames(t *testing.T, ix *Indexer) []string {
	t.Helper()
	n, _, names, err := ix.Store.VocabStats(context.Background(), 1000)
	if err != nil || n != len(names) {
		t.Fatalf("VocabStats = %d, %q, %v", n, names, err)
	}
	return names
}

func TestSyncBuildsVocab(t *testing.T) {
	ctx := context.Background()
	ix, re := setupRepo(t)
	writeFixture(t, ix.Root(), codeFixture)
	big := "package big\n\nfunc HugeThing() {}\n" + strings.Repeat("// pad\n", 200000)
	write(t, ix.Root(), "big/big.go", big)
	st, err := ix.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Scanned != 2 || st.Indexed != 2 {
		t.Errorf("notes: %+v", st)
	}
	if st.Sources != 6 || st.SourcesChanged != 6 {
		t.Errorf("sources: %+v", st)
	}
	want := []string{"fetchpage", "filehash", "golang.org/x/sync", "pagefetcher", "psycopg", "usecarttotal", "zustand"}
	if got := vocabNames(t, ix); !slices.Equal(got, want) {
		t.Errorf("vocab = %q, want %q", got, want)
	}

	// No code reaches the embedder: every input is a note chunk, and none
	// holds a line of any source file or manifest.
	for _, in := range re.inputs {
		if !strings.HasPrefix(in, "README.md\n") && !strings.HasPrefix(in, "docs/guide.md\n") {
			t.Errorf("embedded a non-note input: %q", in)
		}
		for rel, src := range codeFixture {
			if strings.HasSuffix(rel, ".md") {
				continue
			}
			for _, line := range strings.Split(src, "\n") {
				if len(strings.TrimSpace(line)) > 3 && strings.Contains(in, line) {
					t.Errorf("embedder input holds a line of %s: %q", rel, line)
				}
			}
		}
	}
	// Code is not searchable or readable through the policy.
	if _, err := ix.Policy.Read("app/engine.py"); err == nil {
		t.Error("policy reads source files")
	}
	if paths := paths(t, ix); !slices.Equal(paths, []string{"README.md", "docs/guide.md"}) {
		t.Errorf("indexed %v", paths)
	}
}

func TestSyncSkipsUnchangedSources(t *testing.T) {
	ctx := context.Background()
	ix, _ := setupRepo(t)
	writeFixture(t, ix.Root(), codeFixture)
	if _, err := ix.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := ix.Sync(ctx)
	if err != nil || st.Sources != 6 || st.SourcesChanged != 0 {
		t.Fatalf("second sync = %+v, %v", st, err)
	}
	write(t, ix.Root(), "internal/x/x.go", "package x\n\ntype ChunkStore struct{}\n")
	os.Remove(filepath.Join(ix.Root(), "web", "package.json"))
	st, err = ix.Sync(ctx)
	if err != nil || st.Sources != 5 || st.SourcesChanged != 1 {
		t.Fatalf("third sync = %+v, %v", st, err)
	}
	got := vocabNames(t, ix)
	if !slices.Contains(got, "chunkstore") || slices.Contains(got, "filehash") || slices.Contains(got, "zustand") {
		t.Errorf("vocab after edit = %q", got)
	}
	// Ignoring a file drops its names.
	write(t, ix.Root(), ".screeignore", "private/\napp/\n")
	if _, err := ix.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := vocabNames(t, ix); slices.Contains(got, "pagefetcher") {
		t.Errorf("ignored file still contributes: %q", got)
	}
}

func TestResolveVocab(t *testing.T) {
	ctx := context.Background()
	ix, _ := setupRepo(t)
	writeFixture(t, ix.Root(), codeFixture)
	if _, err := ix.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		mode  Mode
		query string
		want  Mode
	}{
		{Auto, "how does the PageFetcher retry?", Hybrid},
		{Auto, "when does PageFetcher's retry give up", Hybrid},
		{Auto, "which version of psycopg do we pin", Hybrid},
		{"", "is `zustand` used for state", Hybrid},
		{Auto, "upgrade golang.org/x/sync", Hybrid},
		{Auto, "how do I sync the index", Semantic},
		{Auto, "run the new build", Semantic},
		{Auto, "sync run new", Semantic},
		{Auto, "how do backups work", Semantic},
		{Auto, "secretLocal", Semantic},       // a local, not a declaration
		{Auto, "GeneratedThing", Semantic},    // gitignored
		{Auto, "VendoredThing", Semantic},     // vendored
		{Auto, "nodeModuleThing", Semantic},   // node_modules
		{Auto, "hiddenThing", Semantic},       // hidden folder
		{Auto, "privateThing", Semantic},      // .screeignore
		{Auto, "psycopg_pool sizing", Hybrid}, // HasIdentifier still applies
		{Semantic, "PageFetcher", Semantic},
		{Keyword, "PageFetcher", Keyword},
	} {
		if got := ix.Resolve(ctx, c.mode, c.query); got != c.want {
			t.Errorf("Resolve(%q, %q) = %q, want %q", c.mode, c.query, got, c.want)
		}
	}
	// Search resolves auto the same way: a vocabulary name runs keyword
	// search too.
	var kw []string
	TestHooks.Keyword = func(m string) { kw = append(kw, m) }
	t.Cleanup(func() { TestHooks.Keyword = nil })
	if _, err := ix.Search(ctx, "psycopg setup", 5, Auto); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.Search(ctx, "database setup", 5, Auto); err != nil {
		t.Fatal(err)
	}
	if len(kw) != 1 || !strings.Contains(kw[0], `"psycopg"`) {
		t.Errorf("keyword queries = %q", kw)
	}
}

func TestQueryWords(t *testing.T) {
	got := queryWords(`How does "PageFetcher" work? (psycopg, PageFetcher's) ... ChunkStore’s`)
	want := []string{"how", "does", "pagefetcher", "work", "psycopg", "chunkstore"}
	if !slices.Equal(got, want) {
		t.Errorf("queryWords = %q, want %q", got, want)
	}
}

// TestSyncSymlinkClass: a symlink is read only as its own class. A note
// linked to code is neither embedded nor indexed, a source file linked to a
// note adds no names, and a source file linked to another in the folder is
// read like one.
func TestSyncSymlinkClass(t *testing.T) {
	ctx := context.Background()
	ix, re := setupRepo(t)
	root := ix.Root()
	write(t, root, "README.md", "# Demo\nPlain words.\n")
	write(t, root, "secret.py", "API_TOKEN = 'leakedValue'\nclass SecretKeeper: pass\n")
	write(t, root, "doc.md", "# Doc\ndef markdownTrick(): pass\n")
	write(t, root, ".claude/hooks/hook.py", "class HiddenHook: pass\n")
	write(t, root, "app/engine.py", "class PageFetcher: pass\n")
	symlink(t, "secret.py", filepath.Join(root, "notes.md"))
	symlink(t, "doc.md", filepath.Join(root, "trick.py"))
	symlink(t, "app/engine.py", filepath.Join(root, "inner.py"))
	symlink(t, ".claude/hooks/hook.py", filepath.Join(root, "hooklink.py"))
	if _, err := ix.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	for _, in := range re.inputs {
		if strings.Contains(in, "leakedValue") || strings.Contains(in, "SecretKeeper") {
			t.Errorf("embedded code through a link: %q", in)
		}
	}
	if got := paths(t, ix); !slices.Equal(got, []string{"README.md", "doc.md"}) {
		t.Errorf("indexed %v", got)
	}
	files, err := ix.Store.VocabFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files["inner.py"]; !ok {
		t.Errorf("in-folder .py link not read: %v", files)
	}
	for _, p := range []string{"trick.py", "hooklink.py", "notes.md", ".claude/hooks/hook.py"} {
		if _, ok := files[p]; ok {
			t.Errorf("%s read for the vocabulary", p)
		}
	}
	got := vocabNames(t, ix)
	if want := []string{"pagefetcher", "secretkeeper"}; !slices.Equal(got, want) {
		t.Errorf("vocab = %q, want %q", got, want)
	}
	// Defence in depth: nothing but markdown reaches the embedder.
	n := len(re.inputs)
	if _, err := ix.indexSrc(ctx, "secret.py", []byte("class SecretKeeper: pass\n"), nil); err == nil || len(re.inputs) != n {
		t.Errorf("indexSrc(secret.py) = %v, %d inputs", err, len(re.inputs)-n)
	}
}

// TestSyncHiddenSources: the hidden-folder allowlist is for markdown; code
// under any hidden folder contributes nothing.
func TestSyncHiddenSources(t *testing.T) {
	ctx := context.Background()
	ix, _ := setupRepo(t)
	writeFixture(t, ix.Root(), map[string]string{
		".claude/agents/a.md":       "# Agent\n",
		".claude/hooks/hook.py":     "class HookRunner: pass\n",
		".github/x.py":              "class CiHelper: pass\n",
		".github/package.json":      `{"dependencies": {"zustand": "1"}}`,
		".github/workflows/ci.md":   "# CI\n",
		"app/engine.py":             "class PageFetcher: pass\n",
		"app/.cache/stale.py":       "class StaleThing: pass\n",
		".claude/worktrees/w/a.py":  "class WorktreeThing: pass\n",
		".claude/worktrees/w/b.md":  "# copy\n",
		".claude/skills/s/SKILL.md": "# Skill\n",
	})
	st, err := ix.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Sources != 1 {
		t.Errorf("sources: %+v", st)
	}
	if got := vocabNames(t, ix); !slices.Equal(got, []string{"pagefetcher"}) {
		t.Errorf("vocab = %q", got)
	}
	if got := paths(t, ix); !slices.Equal(got, []string{".claude/agents/a.md", ".claude/skills/s/SKILL.md", ".github/workflows/ci.md"}) {
		t.Errorf("indexed %v", got)
	}
}

// TestSyncSourceCap: files past the cap are never opened, and Sources
// counts at most the cap.
func TestSyncSourceCap(t *testing.T) {
	ctx := context.Background()
	ix, _ := setupRepo(t)
	defer func(n int) { sourceCap = n }(sourceCap)
	sourceCap = 3
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		write(t, ix.Root(), "src/"+n+".py", "class "+n+"Thing: pass\n")
	}
	write(t, ix.Root(), "z.md", "# Z\n")
	var opened []string
	policy.TestHooks.BeforeOpen = func(rel string) { opened = append(opened, rel) }
	defer func() { policy.TestHooks.BeforeOpen = nil }()
	st, err := ix.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"src/a.py", "src/b.py", "src/c.py", "z.md"}; !slices.Equal(opened, want) {
		t.Errorf("opened %q, want %q", opened, want)
	}
	if st.Sources != 3 || st.SourcesChanged != 3 || st.Indexed != 1 {
		t.Errorf("stats: %+v", st)
	}
	if got := vocabNames(t, ix); !slices.Equal(got, []string{"athing", "bthing", "cthing"}) {
		t.Errorf("vocab = %q", got)
	}
}

// TestSyncNameCap: names past the total cap are dropped, and the file cut
// short is read again until there is room.
func TestSyncNameCap(t *testing.T) {
	ctx := context.Background()
	ix, _ := setupRepo(t)
	defer func(n int) { nameCap = n }(nameCap)
	nameCap = 3
	write(t, ix.Root(), "a.py", "class OneThing: pass\nclass TwoThing: pass\n")
	write(t, ix.Root(), "b.py", "class RedThing: pass\nclass BlueThing: pass\n")
	if _, err := ix.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := vocabNames(t, ix); !slices.Equal(got, []string{"bluething", "onething", "twothing"}) {
		t.Errorf("capped vocab = %q", got)
	}
	nameCap = 10
	st, err := ix.Sync(ctx)
	if err != nil || st.SourcesChanged != 1 {
		t.Fatalf("sync = %+v, %v", st, err)
	}
	if got := vocabNames(t, ix); len(got) != 4 {
		t.Errorf("vocab with room = %q", got)
	}
	if st, err := ix.Sync(ctx); err != nil || st.SourcesChanged != 0 {
		t.Errorf("third sync = %+v, %v", st, err)
	}
}

// TestSyncNameCapSteady: a file cut short is not read again, and the
// vocabulary not rebuilt, while it and the room it gets stay the same; it
// is read again when the room shrinks or the file changes.
func TestSyncNameCapSteady(t *testing.T) {
	ctx := context.Background()
	ix, _ := setupRepo(t)
	defer func(n int) { nameCap = n }(nameCap)
	nameCap = 3
	write(t, ix.Root(), "a.py", "class OneThing: pass\nclass TwoThing: pass\n")
	write(t, ix.Root(), "b.py", "class RedThing: pass\nclass BlueThing: pass\n")
	write(t, ix.Root(), "c.py", "class GreenThing: pass\n")
	if st, err := ix.Sync(ctx); err != nil || st.SourcesChanged != 3 {
		t.Fatalf("first sync = %+v, %v", st, err)
	}
	for range 2 {
		if st, err := ix.Sync(ctx); err != nil || st.SourcesChanged != 0 {
			t.Fatalf("steady sync = %+v, %v", st, err)
		}
	}
	if got := vocabNames(t, ix); !slices.Equal(got, []string{"bluething", "onething", "twothing"}) {
		t.Errorf("capped vocab = %q", got)
	}
	// Less room for b.py: cut further.
	write(t, ix.Root(), "a.py", "class OneThing: pass\nclass TwoThing: pass\nclass SixThing: pass\n")
	if st, err := ix.Sync(ctx); err != nil || st.SourcesChanged != 2 {
		t.Fatalf("shrunk sync = %+v, %v", st, err)
	}
	if got := vocabNames(t, ix); !slices.Equal(got, []string{"onething", "sixthing", "twothing"}) {
		t.Errorf("shrunk vocab = %q", got)
	}
	if st, err := ix.Sync(ctx); err != nil || st.SourcesChanged != 0 {
		t.Fatalf("steady sync = %+v, %v", st, err)
	}
	// A changed file that is cut is read again.
	write(t, ix.Root(), "b.py", "class RedThing: pass\n")
	if st, err := ix.Sync(ctx); err != nil || st.SourcesChanged != 1 {
		t.Fatalf("changed sync = %+v, %v", st, err)
	}
}

// TestSyncSourceCapFailed: a file that fails to read does not count
// against the source cap.
func TestSyncSourceCapFailed(t *testing.T) {
	ctx := context.Background()
	ix, _ := setupRepo(t)
	defer func(n int) { sourceCap = n }(sourceCap)
	sourceCap = 2
	for _, n := range []string{"a", "b", "c"} {
		write(t, ix.Root(), "src/"+n+".py", "class "+n+"Thing: pass\n")
	}
	if err := os.Chmod(filepath.Join(ix.Root(), "src", "a.py"), 0); err != nil {
		t.Fatal(err)
	}
	if f, err := os.Open(filepath.Join(ix.Root(), "src", "a.py")); err == nil {
		f.Close()
		t.Skip("running with permission to read any file")
	}
	st, err := ix.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Sources != 2 {
		t.Errorf("stats: %+v", st)
	}
	if got := vocabNames(t, ix); !slices.Equal(got, []string{"bthing", "cthing"}) {
		t.Errorf("vocab = %q", got)
	}
}

// TestSyncVocabVersion: names read by another extractor version are all
// read again.
func TestSyncVocabVersion(t *testing.T) {
	ctx := context.Background()
	ix, _ := setupRepo(t)
	writeFixture(t, ix.Root(), codeFixture)
	if _, err := ix.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if v, _ := ix.Store.Meta(store.VocabVersionKey); v != vocab.Version {
		t.Fatalf("vocab version = %q", v)
	}
	// Stand in for names an older extractor read: same hash, other names.
	src, _ := os.ReadFile(filepath.Join(ix.Root(), "app", "engine.py"))
	sum := sha256.Sum256(src)
	old := store.VocabFile{Path: "app/engine.py", Hash: hex.EncodeToString(sum[:]), Names: []string{"oldname"}}
	if err := ix.Store.UpdateVocab(ctx, []store.VocabFile{old}, nil); err != nil {
		t.Fatal(err)
	}
	if st, err := ix.Sync(ctx); err != nil || st.SourcesChanged != 0 || !slices.Contains(vocabNames(t, ix), "oldname") {
		t.Fatalf("same version: %+v, %v", st, err)
	}
	if err := ix.Store.SetMeta(store.VocabVersionKey, "0"); err != nil {
		t.Fatal(err)
	}
	st, err := ix.Sync(ctx)
	if err != nil || st.SourcesChanged != st.Sources || st.Sources != 6 {
		t.Fatalf("new version: %+v, %v", st, err)
	}
	if got := vocabNames(t, ix); slices.Contains(got, "oldname") || !slices.Contains(got, "pagefetcher") {
		t.Errorf("vocab = %q", got)
	}
	if v, _ := ix.Store.Meta(store.VocabVersionKey); v != vocab.Version {
		t.Errorf("vocab version = %q", v)
	}
}

// TestSyncVocabReadErrors: a transient read failure keeps a file's names;
// a file that is gone or unreadable loses them.
func TestSyncVocabReadErrors(t *testing.T) {
	ctx := context.Background()
	ix, _ := setupRepo(t)
	root := ix.Root()
	writeFixture(t, root, map[string]string{
		"a/changing.py": "class ChangingThing: pass\n",
		"b/dangling.py": "class DanglingThing: pass\n",
		"c/locked.py":   "class LockedThing: pass\n",
	})
	if _, err := ix.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	// The file is replaced between the checks and the open: ErrChanged.
	policy.TestHooks.BeforeOpen = func(rel string) {
		if rel == "a/changing.py" {
			p := filepath.Join(root, "a", "changing.py")
			os.Remove(p)
			os.WriteFile(p, []byte("class ChangingThing: pass\n"), 0o644)
		}
	}
	defer func() { policy.TestHooks.BeforeOpen = nil }()
	os.Remove(filepath.Join(root, "b", "dangling.py"))
	symlink(t, "missing.py", filepath.Join(root, "b", "dangling.py"))
	locked := filepath.Join(root, "c", "locked.py")
	os.Chmod(locked, 0)
	defer os.Chmod(locked, 0o644)
	if _, err := os.ReadFile(locked); err == nil {
		os.Remove(locked) // running as root: gone stands in for unreadable
	}
	if _, err := ix.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := vocabNames(t, ix); !slices.Equal(got, []string{"changingthing"}) {
		t.Errorf("vocab = %q", got)
	}
}
