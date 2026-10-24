package watch

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jrapakko/scree/internal/embed"
	"github.com/jrapakko/scree/internal/index"
	"github.com/jrapakko/scree/internal/policy"
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

// start builds an indexer over dir/notes, optionally runs prepare and a
// Sync, then runs the watcher until the test ends. It returns the notes
// folder and a function listing the indexed paths.
func start(t *testing.T, prepare func(dir, root string)) (string, string, func() []string) {
	t.Helper()
	return startWith(t, prepare, nil)
}

// startWith is start with a hook to adjust the indexer before the first
// Sync.
func startWith(t *testing.T, prepare func(dir, root string), configure func(*index.Indexer)) (string, string, func() []string) {
	t.Helper()
	if testing.Short() {
		t.Skip("uses real file events and timers")
	}
	dir := t.TempDir()
	root := filepath.Join(dir, "notes")
	os.MkdirAll(root, 0o755)
	if prepare != nil {
		prepare(dir, root)
	}
	s, err := store.Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := policy.Open(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	ix := &index.Indexer{Policy: p, Store: s, Embedder: constEmbedder{}, Log: log.New(os.Stderr, "", 0)}
	if configure != nil {
		configure(ix)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if prepare != nil {
		if _, err := ix.Sync(ctx); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- Run(ctx, ix) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
		p.Close()
		s.Close()
	})
	time.Sleep(200 * time.Millisecond)
	paths := func() []string {
		got, _ := s.Paths(context.Background())
		slices.Sort(got)
		return got
	}
	return dir, root, paths
}

func TestWatchIndexesAndRemoves(t *testing.T) {
	dir, root, paths := start(t, nil)

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
}

func TestWatchScreeignoreEditTriggersSync(t *testing.T) {
	_, root, paths := start(t, func(_, root string) {
		os.MkdirAll(filepath.Join(root, "journal"), 0o755)
		os.WriteFile(filepath.Join(root, "journal", "day.md"), []byte("# Day\nx\n"), 0o644)
		os.WriteFile(filepath.Join(root, "a.md"), []byte("# A\nx\n"), 0o644)
	})
	if got := paths(); len(got) != 2 {
		t.Fatalf("initial paths = %v", got)
	}
	ign := filepath.Join(root, ".screeignore")
	os.WriteFile(ign, []byte("journal/\n"), 0o644)
	waitFor(t, "journal removed", func() bool { return slices.Equal(paths(), []string{"a.md"}) })
	os.Remove(ign)
	waitFor(t, "journal re-added", func() bool { return len(paths()) == 2 })
}

func TestWatchIgnoresDirSymlinkOutside(t *testing.T) {
	dir, root, paths := start(t, nil)
	outside := filepath.Join(dir, "outside")
	os.MkdirAll(outside, 0o755)
	if err := os.Symlink(outside, filepath.Join(root, "d")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	os.WriteFile(filepath.Join(outside, "secret.md"), []byte("# Secret\nx\n"), 0o644)
	// A canary inside the folder proves the watcher is processing events.
	os.WriteFile(filepath.Join(root, "canary.md"), []byte("# C\nx\n"), 0o644)
	waitFor(t, "canary indexed", func() bool { return len(paths()) >= 1 })
	time.Sleep(Debounce + time.Second)
	if got := paths(); !slices.Equal(got, []string{"canary.md"}) {
		t.Fatalf("paths = %v", got)
	}
}

func TestWatchFrontMatterRemoves(t *testing.T) {
	_, root, paths := start(t, func(_, root string) {
		os.WriteFile(filepath.Join(root, "a.md"), []byte("# A\nx\n"), 0o644)
	})
	if got := paths(); len(got) != 1 {
		t.Fatalf("initial paths = %v", got)
	}
	os.WriteFile(filepath.Join(root, "a.md"), []byte("---\nscree: ignore\n---\n# A\nx\n"), 0o644)
	waitFor(t, "a.md removed", func() bool { return len(paths()) == 0 })
}

// flakyEmbedder fails with ErrUnavailable until up is set.
type flakyEmbedder struct {
	constEmbedder
	up    atomic.Bool
	calls atomic.Int32
}

func (f *flakyEmbedder) Embed(ctx context.Context, in []string) ([][]float32, error) {
	f.calls.Add(1)
	if !f.up.Load() {
		return nil, fmt.Errorf("embed: %w: 503", embed.ErrUnavailable)
	}
	return f.constEmbedder.Embed(ctx, in)
}

func TestWatchResyncsAfterOutage(t *testing.T) {
	fe := &flakyEmbedder{}
	_, root, paths := startWith(t, nil, func(ix *index.Indexer) {
		ix.Embedder = fe
		ix.MinBackoff = 300 * time.Millisecond
	})
	os.WriteFile(filepath.Join(root, "a.md"), []byte("# A\nx\n"), 0o644)
	waitFor(t, "first failed attempt", func() bool { return fe.calls.Load() >= 1 })
	if got := paths(); len(got) != 0 {
		t.Fatalf("indexed during outage: %v", got)
	}
	fe.up.Store(true)
	// No new file event: only the resync timer can index it now.
	waitFor(t, "a.md indexed by resync", func() bool { return slices.Equal(paths(), []string{"a.md"}) })
}

func TestWatchDirRename(t *testing.T) {
	ctx := context.Background()
	var s *store.Store
	_, root, paths := startWith(t, func(_, root string) {
		os.MkdirAll(filepath.Join(root, "a", "sub"), 0o755)
		os.WriteFile(filepath.Join(root, "a", "x.md"), []byte("# X\nx\n"), 0o644)
		os.WriteFile(filepath.Join(root, "a", "sub", "y.md"), []byte("# Y\ny\n"), 0o644)
	}, func(ix *index.Indexer) { s = ix.Store })
	if got := paths(); !slices.Equal(got, []string{"a/sub/y.md", "a/x.md"}) {
		t.Fatalf("initial paths = %v", got)
	}
	if err := os.Rename(filepath.Join(root, "a"), filepath.Join(root, "b")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "rename picked up", func() bool { return slices.Equal(paths(), []string{"b/sub/y.md", "b/x.md"}) })

	// Watches on the new names must report the new paths.
	before, _ := s.FileHash(ctx, "b/x.md")
	os.WriteFile(filepath.Join(root, "b", "x.md"), []byte("# X\nchanged\n"), 0o644)
	waitFor(t, "edit in renamed folder", func() bool { h, _ := s.FileHash(ctx, "b/x.md"); return h != before })
	os.WriteFile(filepath.Join(root, "b", "new.md"), []byte("# N\nn\n"), 0o644)
	os.WriteFile(filepath.Join(root, "b", "sub", "z.md"), []byte("# Z\nz\n"), 0o644)
	waitFor(t, "new files in renamed folder", func() bool {
		return slices.Equal(paths(), []string{"b/new.md", "b/sub/y.md", "b/sub/z.md", "b/x.md"})
	})
}

// runUntilExit starts Run with a short health interval, calls act, and
// returns the error Run exits with.
func runUntilExit(t *testing.T, act func(dir, root string)) error {
	t.Helper()
	if testing.Short() {
		t.Skip("uses real file events and timers")
	}
	old := HealthInterval
	HealthInterval = 100 * time.Millisecond
	t.Cleanup(func() { HealthInterval = old })
	dir := t.TempDir()
	root := filepath.Join(dir, "notes")
	os.MkdirAll(root, 0o755)
	s, err := store.Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, err := policy.Open(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ix := &index.Indexer{Policy: p, Store: s, Embedder: constEmbedder{}, Log: log.New(os.Stderr, "", 0)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, ix) }()
	time.Sleep(200 * time.Millisecond)
	act(dir, root)
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not exit")
		return nil
	}
}

func TestWatchExitsWhenIndexDeleted(t *testing.T) {
	err := runUntilExit(t, func(dir, _ string) { os.Remove(filepath.Join(dir, "index.db")) })
	if !errors.Is(err, store.ErrIndexGone) {
		t.Fatalf("Run = %v, want ErrIndexGone", err)
	}
}

func TestWatchExitsWhenRootMoved(t *testing.T) {
	err := runUntilExit(t, func(dir, root string) { os.Rename(root, filepath.Join(dir, "moved")) })
	if err == nil || !strings.Contains(err.Error(), "removed or moved") {
		t.Fatalf("Run = %v", err)
	}
}
