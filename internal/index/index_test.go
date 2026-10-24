package index

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jrapakko/scree/internal/store"
)

// fakeEmbedder maps text to a vector of keyword counts, which is enough to
// check that search finds the right chunk.
type fakeEmbedder struct{ calls int }

var keywords = []string{"apple", "banana", "cherry", "date"}

func (f *fakeEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	f.calls++
	out := make([][]float32, len(inputs))
	for i, in := range inputs {
		v := make([]float32, len(keywords))
		for j, k := range keywords {
			v[j] = float32(strings.Count(strings.ToLower(in), k)) + 0.01
		}
		out[i] = v
	}
	return out, nil
}

func (f *fakeEmbedder) Model() string { return "fake" }

func setup(t *testing.T) (*Indexer, *fakeEmbedder) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "notes")
	os.MkdirAll(filepath.Join(root, "sub"), 0o755)
	os.MkdirAll(filepath.Join(root, ".obsidian"), 0o755)
	s, err := store.Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	fe := &fakeEmbedder{}
	return &Indexer{Root: root, Store: s, Embedder: fe}, fe
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSyncIndexesChangesAndRemovals(t *testing.T) {
	ctx := context.Background()
	ix, fe := setup(t)
	write(t, ix.Root, "fruit.md", "# Apples\napple apple\n\n# Bananas\nbanana\n")
	write(t, ix.Root, "sub/cherry.md", "# Cherry\ncherry cherry cherry\n")
	write(t, ix.Root, ".obsidian/hidden.md", "# Hidden\ndate\n")
	write(t, ix.Root, "notes.txt", "date")

	st, err := ix.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st != (SyncStats{Scanned: 2, Indexed: 2}) {
		t.Fatalf("first sync = %+v", st)
	}

	hits, err := ix.Search(ctx, "cherry", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Path != "sub/cherry.md" || hits[0].Heading != "Cherry" {
		t.Fatalf("hits = %+v", hits)
	}

	// Unchanged files are not re-embedded.
	calls := fe.calls
	if st, _ := ix.Sync(ctx); st.Indexed != 0 || fe.calls != calls {
		t.Fatalf("second sync = %+v, embed calls %d -> %d", st, calls, fe.calls)
	}

	write(t, ix.Root, "fruit.md", "# Dates\ndate date\n")
	os.Remove(filepath.Join(ix.Root, "sub/cherry.md"))
	st, _ = ix.Sync(ctx)
	if st.Indexed != 1 || st.Removed != 1 {
		t.Fatalf("third sync = %+v", st)
	}
	hits, _ = ix.Search(ctx, "date", 5)
	if len(hits) != 1 || hits[0].Heading != "Dates" {
		t.Fatalf("hits after change = %+v", hits)
	}
}

func TestIndexFileMissingRemoves(t *testing.T) {
	ctx := context.Background()
	ix, _ := setup(t)
	write(t, ix.Root, "sub/a.md", "# A\napple\n")
	write(t, ix.Root, "sub/b.md", "# B\nbanana\n")
	ix.Sync(ctx)

	os.Remove(filepath.Join(ix.Root, "sub/a.md"))
	if changed, err := ix.IndexFile(ctx, "sub/a.md"); err != nil || !changed {
		t.Fatalf("IndexFile on missing = %v, %v", changed, err)
	}
	if err := ix.RemovePrefix(ctx, "sub"); err != nil {
		t.Fatal(err)
	}
	if paths, _ := ix.Store.Paths(ctx); len(paths) != 0 {
		t.Fatalf("paths = %v", paths)
	}
}

func TestAbsRejectsEscapes(t *testing.T) {
	ix := &Indexer{Root: "/notes"}
	for _, bad := range []string{"../etc/passwd", "a/../../x.md", "/etc/passwd", "", "a/./b.md"} {
		if _, err := ix.Abs(bad); err == nil {
			t.Errorf("Abs(%q) succeeded", bad)
		}
	}
	if p, err := ix.Abs("sub/a.md"); err != nil || p != "/notes/sub/a.md" {
		t.Errorf("Abs(sub/a.md) = %q, %v", p, err)
	}
}
