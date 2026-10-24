package watch

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jrapakko/scree/internal/index"
	"github.com/jrapakko/scree/internal/store"
)

type constEmbedder struct{}

func (constEmbedder) Embed(_ context.Context, in []string) ([][]float32, error) {
	out := make([][]float32, len(in))
	for i := range in {
		out[i] = []float32{1, float32(len(in[i]))}
	}
	return out, nil
}
func (constEmbedder) Model() string { return "const" }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(Debounce + 5*time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestWatchIndexesAndRemoves(t *testing.T) {
	if testing.Short() {
		t.Skip("uses real file events and timers")
	}
	dir := t.TempDir()
	root := filepath.Join(dir, "notes")
	os.MkdirAll(root, 0o755)
	s, err := store.Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ix := &index.Indexer{Root: root, Store: s, Embedder: constEmbedder{}, Log: log.New(os.Stderr, "", 0)}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, ix) }()
	time.Sleep(200 * time.Millisecond)

	paths := func() []string { p, _ := s.Paths(ctx); return p }

	os.WriteFile(filepath.Join(root, "a.md"), []byte("# A\nhello\n"), 0o644)
	waitFor(t, "a.md indexed", func() bool { return len(paths()) == 1 })

	// A folder moved in with a note inside.
	staging := filepath.Join(dir, "staging")
	os.MkdirAll(staging, 0o755)
	os.WriteFile(filepath.Join(staging, "b.md"), []byte("# B\nworld\n"), 0o644)
	if err := os.Rename(staging, filepath.Join(root, "sub")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "sub/b.md indexed", func() bool { return len(paths()) == 2 })

	// The folder removed.
	os.RemoveAll(filepath.Join(root, "sub"))
	waitFor(t, "sub removed", func() bool { p := paths(); return len(p) == 1 && p[0] == "a.md" })

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
