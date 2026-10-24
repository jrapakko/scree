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

	"github.com/fsnotify/fsnotify"

	"github.com/jrapakko/scree/internal/embed"
	"github.com/jrapakko/scree/internal/index"
	"github.com/jrapakko/scree/internal/policy"
	"github.com/jrapakko/scree/internal/store"
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
	dir := tempDir(t)
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
	dir := tempDir(t)
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

// startRepo is start over a git work tree: dir/notes holds a .git folder and
// .gitignore with the given content. HOME is isolated so the user's git
// config never applies. It returns the folder, the indexed paths, and the
// folders being watched, relative to the root.
func startRepo(t *testing.T, gitignore string, prepare func(root string)) (string, func() []string, func() []string) {
	t.Helper()
	home := tempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	var watcher atomic.Pointer[fsnotify.Watcher]
	onWatcher = func(w *fsnotify.Watcher) { watcher.Store(w) }
	t.Cleanup(func() { onWatcher = nil })
	_, root, paths := start(t, func(_, root string) {
		os.MkdirAll(filepath.Join(root, ".git"), 0o755)
		if gitignore != "" {
			os.WriteFile(filepath.Join(root, ".gitignore"), []byte(gitignore), 0o644)
		}
		if prepare != nil {
			prepare(root)
		}
	})
	watched := func() []string {
		var got []string
		for _, p := range watcher.Load().WatchList() {
			rel, err := filepath.Rel(root, p)
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, filepath.ToSlash(rel))
		}
		slices.Sort(got)
		return got
	}
	return root, paths, watched
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// canary writes a note and waits until it is indexed, which proves events
// written before it have been seen.
func canary(t *testing.T, root string, paths func() []string, name string) {
	t.Helper()
	writeFile(t, filepath.Join(root, name), "# C\nx\n")
	waitFor(t, name+" indexed", func() bool { return slices.Contains(paths(), name) })
	time.Sleep(Debounce + 500*time.Millisecond)
}

func TestWatchSkipsGitignoredDirs(t *testing.T) {
	_, _, watched := startRepo(t, "node_modules/\n", func(root string) {
		for _, d := range []string{"node_modules/a/b", "docs/sub", "vendor/lib/.git", "vendor/lib/x", ".claude/agents", ".claude/worktrees/w", ".github", ".obsidian"} {
			os.MkdirAll(filepath.Join(root, d), 0o755)
		}
	})
	want := []string{".", ".claude", ".claude/agents", ".github", "docs", "docs/sub", "vendor"}
	if got := watched(); !slices.Equal(got, want) {
		t.Fatalf("watched = %v\nwant %v", got, want)
	}
}

func TestWatchCreatedIgnoredDir(t *testing.T) {
	root, paths, watched := startRepo(t, "node_modules/\n", nil)
	writeFile(t, filepath.Join(root, "node_modules", "a", "b.md"), "# B\nx\n")
	canary(t, root, paths, "canary.md")
	if got := paths(); !slices.Equal(got, []string{"canary.md"}) {
		t.Errorf("paths = %v", got)
	}
	if got := watched(); !slices.Equal(got, []string{"."}) {
		t.Errorf("watched = %v", got)
	}
}

func TestWatchGitignoreEdit(t *testing.T) {
	root, paths, _ := startRepo(t, "", func(root string) {
		writeFile(t, filepath.Join(root, "docs", "x.md"), "# X\nx\n")
		writeFile(t, filepath.Join(root, "a.md"), "# A\na\n")
	})
	if got := paths(); !slices.Equal(got, []string{"a.md", "docs/x.md"}) {
		t.Fatalf("initial paths = %v", got)
	}
	ign := filepath.Join(root, ".gitignore")
	writeFile(t, ign, "docs/\n")
	waitFor(t, "docs/x.md removed", func() bool { return slices.Equal(paths(), []string{"a.md"}) })
	writeFile(t, ign, "# nothing\n")
	waitFor(t, "docs/x.md back", func() bool { return slices.Equal(paths(), []string{"a.md", "docs/x.md"}) })
}

func TestWatchNestedGitignoreEdit(t *testing.T) {
	root, paths, _ := startRepo(t, "", func(root string) {
		writeFile(t, filepath.Join(root, "docs", "x.md"), "# X\nx\n")
		writeFile(t, filepath.Join(root, "docs", "y.md"), "# Y\ny\n")
	})
	ign := filepath.Join(root, "docs", ".gitignore")
	writeFile(t, ign, "x.md\n")
	waitFor(t, "docs/x.md removed", func() bool { return slices.Equal(paths(), []string{"docs/y.md"}) })
	if err := os.Remove(ign); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "docs/x.md back", func() bool { return slices.Equal(paths(), []string{"docs/x.md", "docs/y.md"}) })
}

func TestWatchUnignoredDirGetsWatched(t *testing.T) {
	root, paths, watched := startRepo(t, "gen/\n", func(root string) {
		writeFile(t, filepath.Join(root, "gen", "old.md"), "# O\no\n")
	})
	if got := paths(); len(got) != 0 {
		t.Fatalf("initial paths = %v", got)
	}
	writeFile(t, filepath.Join(root, ".gitignore"), "# gen is back\n")
	waitFor(t, "gen/old.md indexed", func() bool { return slices.Equal(paths(), []string{"gen/old.md"}) })
	if got := watched(); !slices.Contains(got, "gen") {
		t.Fatalf("watched = %v", got)
	}
	writeFile(t, filepath.Join(root, "gen", "new.md"), "# N\nn\n")
	waitFor(t, "gen/new.md indexed", func() bool { return slices.Equal(paths(), []string{"gen/new.md", "gen/old.md"}) })

	// Ignoring it again drops the watch.
	writeFile(t, filepath.Join(root, ".gitignore"), "gen/\n")
	waitFor(t, "gen removed", func() bool { return len(paths()) == 0 })
	if got := watched(); slices.Contains(got, "gen") {
		t.Errorf("watched = %v", got)
	}
}

func TestWatchAllowlistedHiddenDir(t *testing.T) {
	root, paths, watched := startRepo(t, "", nil)
	writeFile(t, filepath.Join(root, ".github", "ISSUE_TEMPLATE", "bug.md"), "# Bug\nx\n")
	waitFor(t, "bug.md indexed", func() bool {
		return slices.Equal(paths(), []string{".github/ISSUE_TEMPLATE/bug.md"})
	})
	if got := watched(); !slices.Contains(got, ".github/ISSUE_TEMPLATE") {
		t.Errorf("watched = %v", got)
	}
	writeFile(t, filepath.Join(root, ".github", "ISSUE_TEMPLATE", "feature.md"), "# F\nx\n")
	waitFor(t, "feature.md indexed", func() bool { return len(paths()) == 2 })
}

func TestWatchWorktreesIgnored(t *testing.T) {
	root, paths, watched := startRepo(t, "", func(root string) {
		os.MkdirAll(filepath.Join(root, ".claude", "agents"), 0o755)
	})
	writeFile(t, filepath.Join(root, ".claude", "worktrees", "w", "README.md"), "# W\nx\n")
	writeFile(t, filepath.Join(root, ".claude", "agents", "a.md"), "# A\nx\n")
	canary(t, root, paths, "canary.md")
	if got := paths(); !slices.Equal(got, []string{".claude/agents/a.md", "canary.md"}) {
		t.Errorf("paths = %v", got)
	}
	for _, p := range watched() {
		if strings.Contains(p, "worktrees") {
			t.Errorf("watching %s", p)
		}
	}
}

func TestWatchIncludeHiddenEdit(t *testing.T) {
	root, paths, _ := startRepo(t, "", func(root string) {
		writeFile(t, filepath.Join(root, ".foo", "a.md"), "# A\nx\n")
	})
	if got := paths(); len(got) != 0 {
		t.Fatalf("initial paths = %v", got)
	}
	writeFile(t, filepath.Join(root, ".screeignore"), "include-hidden .foo/\n")
	waitFor(t, ".foo/a.md indexed", func() bool { return slices.Equal(paths(), []string{".foo/a.md"}) })
	writeFile(t, filepath.Join(root, ".foo", "b.md"), "# B\nx\n")
	waitFor(t, ".foo/b.md indexed", func() bool { return slices.Equal(paths(), []string{".foo/a.md", ".foo/b.md"}) })
}

// gateEmbedder blocks every Embed while hold is set, until release.
type gateEmbedder struct {
	constEmbedder
	hold    atomic.Bool
	waiting atomic.Int32
	open    chan struct{}
}

func newGate() *gateEmbedder { return &gateEmbedder{open: make(chan struct{})} }

func (g *gateEmbedder) Embed(ctx context.Context, in []string) ([][]float32, error) {
	if g.hold.Load() {
		g.waiting.Add(1)
		select {
		case <-g.open:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return g.constEmbedder.Embed(ctx, in)
}

func (g *gateEmbedder) release() {
	g.hold.Store(false)
	close(g.open)
}

func TestOverflowTriggersSync(t *testing.T) {
	errInject = make(chan error, 1)
	var suppress atomic.Bool
	suppressEvents = suppress.Load
	t.Cleanup(func() { errInject, suppressEvents = nil, nil })
	_, root, paths := start(t, func(_, root string) {
		os.WriteFile(filepath.Join(root, "a.md"), []byte("# A\nx\n"), 0o644)
		os.WriteFile(filepath.Join(root, "b.md"), []byte("# B\nx\n"), 0o644)
	})
	if got := paths(); len(got) != 2 {
		t.Fatalf("initial paths = %v", got)
	}
	// The kernel "loses" these events.
	suppress.Store(true)
	os.Remove(filepath.Join(root, "a.md"))
	os.WriteFile(filepath.Join(root, "c.md"), []byte("# C\nx\n"), 0o644)
	time.Sleep(Debounce + 500*time.Millisecond)
	if got := paths(); len(got) != 2 || got[0] != "a.md" {
		t.Fatalf("events were not suppressed: %v", got)
	}
	suppress.Store(false)
	errInject <- fsnotify.ErrEventOverflow
	waitFor(t, "resync after overflow", func() bool { return slices.Equal(paths(), []string{"b.md", "c.md"}) })
}

func TestEventsReadWhileEmbedding(t *testing.T) {
	var events atomic.Int64
	onEvent = func() { events.Add(1) }
	t.Cleanup(func() { onEvent = nil })
	g := newGate()
	_, root, paths := startWith(t, nil, func(ix *index.Indexer) { ix.Embedder = g })
	g.hold.Store(true)
	os.WriteFile(filepath.Join(root, "first.md"), []byte("# F\nx\n"), 0o644)
	waitFor(t, "worker blocked in Embed", func() bool { return g.waiting.Load() > 0 })
	before := events.Load()
	for i := range 500 {
		p := filepath.Join(root, fmt.Sprintf("n%03d.md", i))
		os.WriteFile(p, []byte("# N\nx\n"), 0o644)
		if i%2 == 1 {
			os.Remove(p)
		}
	}
	// The event goroutine keeps reading while the worker is stuck.
	waitFor(t, "events read during the embed", func() bool { return events.Load() >= before+500 })
	g.release()
	var want []string
	want = append(want, "first.md")
	for i := 0; i < 500; i += 2 {
		want = append(want, fmt.Sprintf("n%03d.md", i))
	}
	slices.Sort(want)
	deadline := time.Now().Add(30 * time.Second)
	for !slices.Equal(paths(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("index has %d paths, want %d", len(paths()), len(want))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestIgnoreEditDuringSlowEmbed(t *testing.T) {
	g := newGate()
	_, root, paths := startWith(t, func(_, root string) {
		os.MkdirAll(filepath.Join(root, "journal"), 0o755)
		os.WriteFile(filepath.Join(root, "journal", "day.md"), []byte("# D\nx\n"), 0o644)
	}, func(ix *index.Indexer) { ix.Embedder = g })
	if got := paths(); !slices.Equal(got, []string{"journal/day.md"}) {
		t.Fatalf("initial paths = %v", got)
	}
	g.hold.Store(true)
	os.WriteFile(filepath.Join(root, "slow.md"), []byte("# S\nx\n"), 0o644)
	waitFor(t, "worker blocked in Embed", func() bool { return g.waiting.Load() > 0 })
	os.WriteFile(filepath.Join(root, ".screeignore"), []byte("journal/\n"), 0o644)
	time.Sleep(Debounce + 500*time.Millisecond)
	g.release()
	waitFor(t, "journal removed and slow.md indexed", func() bool { return slices.Equal(paths(), []string{"slow.md"}) })
}
