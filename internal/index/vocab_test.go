package index

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jrapakko/scree/internal/policy"
	"github.com/jrapakko/scree/internal/store"
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
	got := queryWords(`How does "PageFetcher" work? (psycopg, PageFetcher) ...`)
	want := []string{"how", "does", "pagefetcher", "work", "psycopg"}
	if !slices.Equal(got, want) {
		t.Errorf("queryWords = %q, want %q", got, want)
	}
}
