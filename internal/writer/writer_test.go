package writer

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

// gate blocks Embed after the first n calls until release is closed.
type gate struct {
	constEmbedder
	after   int32
	calls   atomic.Int32
	release chan struct{}
}

func (g *gate) Embed(ctx context.Context, in []string) ([][]float32, error) {
	if g.calls.Add(1) > g.after {
		select {
		case <-g.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return g.constEmbedder.Embed(ctx, in)
}

type env struct {
	dir, root, db, lockPath string
}

func newEnv(t *testing.T, files ...string) env {
	t.Helper()
	dir := t.TempDir()
	e := env{dir: dir, root: filepath.Join(dir, "notes"), db: filepath.Join(dir, "idx", "index.db")}
	e.lockPath = filepath.Join(dir, "idx", "writer.lock")
	os.MkdirAll(e.root, 0o755)
	os.MkdirAll(filepath.Dir(e.db), 0o700)
	for _, f := range files {
		os.WriteFile(filepath.Join(e.root, f), []byte("# "+f+"\ntext\n"), 0o644)
	}
	return e
}

// writer opens its own policy and store, as a separate process would, and
// returns the writer, its Run cancel func, and a channel with Run's result.
func (e env) writer(t *testing.T, emb interface {
	Embed(context.Context, []string) ([][]float32, error)
	Model() string
}) (*Writer, context.CancelFunc, chan error) {
	t.Helper()
	p, err := policy.Open(e.root, 0)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(e.db)
	if err != nil {
		t.Fatal(err)
	}
	ix := &index.Indexer{Policy: p, Store: s, Embedder: emb, Log: log.New(io.Discard, "", 0)}
	w := &Writer{Indexer: ix, LockPath: e.lockPath, Retry: 200 * time.Millisecond, Log: log.New(io.Discard, "", 0)}
	w.Attach()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
		s.Close()
		p.Close()
	})
	return w, cancel, done
}

func waitPhase(t *testing.T, w *Writer, want Phase, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if w.Status().Phase == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("phase = %s, want %s", w.Status().Phase, want)
}

func TestOneWriter(t *testing.T) {
	e := newEnv(t, "a.md", "b.md")
	a, _, _ := e.writer(t, constEmbedder{})
	waitPhase(t, a, Watching, 5*time.Second)
	b, _, _ := e.writer(t, constEmbedder{})
	waitPhase(t, b, Waiting, 5*time.Second)
	time.Sleep(500 * time.Millisecond) // a few retries
	if st := b.Status(); st.Phase != Waiting || st.HolderPID != os.Getpid() || b.Holding() {
		t.Fatalf("second writer: %+v, holding %v", st, b.Holding())
	}
	if !a.Holding() {
		t.Fatal("first writer does not hold the lock")
	}
	if n := b.Note(context.Background()); n != "" {
		t.Errorf("waiter note after a full sync = %q", n)
	}
	if paths, _ := a.Indexer.Store.Paths(context.Background()); len(paths) != 2 {
		t.Errorf("paths = %v", paths)
	}
}

func TestTakeover(t *testing.T) {
	e := newEnv(t, "a.md")
	a, cancelA, doneA := e.writer(t, constEmbedder{})
	waitPhase(t, a, Watching, 5*time.Second)
	b, _, _ := e.writer(t, constEmbedder{})
	waitPhase(t, b, Waiting, 5*time.Second)

	cancelA()
	if err := <-doneA; err != nil {
		t.Fatal(err)
	}
	doneA <- nil // for the cleanup
	// Edited while neither process watches: the takeover sync picks it up.
	os.WriteFile(filepath.Join(e.root, "new.md"), []byte("# New\nx\n"), 0o644)
	start := time.Now()
	waitPhase(t, b, Watching, 5*time.Second)
	if d := time.Since(start); d > 2*b.Retry+2*time.Second {
		t.Errorf("takeover took %s", d)
	}
	paths, _ := b.Indexer.Store.Paths(context.Background())
	slices.Sort(paths)
	if !slices.Equal(paths, []string{"a.md", "new.md"}) {
		t.Errorf("paths = %v", paths)
	}
}

func TestNoteDuringFirstSync(t *testing.T) {
	e := newEnv(t, "a.md", "b.md", "c.md", "d.md")
	g := &gate{after: 2, release: make(chan struct{})}
	a, _, _ := e.writer(t, g)
	waitPhase(t, a, Syncing, 5*time.Second)
	deadline := time.Now().Add(5 * time.Second)
	for a.Status().Indexed < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	ctx := context.Background()
	if n := a.Note(ctx); n != "[index incomplete: first sync in progress, 2 notes indexed so far]" {
		t.Errorf("holder note = %q", n)
	}
	b, _, _ := e.writer(t, constEmbedder{})
	waitPhase(t, b, Waiting, 5*time.Second)
	if n := b.Note(ctx); !strings.HasPrefix(n, "[index incomplete: another scree process (pid ") {
		t.Errorf("waiter note = %q", n)
	}
	close(g.release)
	waitPhase(t, a, Watching, 5*time.Second)
	if n := a.Note(ctx); n != "" {
		t.Errorf("note after sync = %q", n)
	}
	if n := b.Note(ctx); n != "" {
		t.Errorf("waiter note after sync = %q", n)
	}
}

func TestStaleQueuedToHolder(t *testing.T) {
	e := newEnv(t, "a.md", "b.md")
	a, _, _ := e.writer(t, constEmbedder{})
	waitPhase(t, a, Watching, 5*time.Second)
	// Remove a file so quickly that only the stale path, not a debounced
	// event, can be what removes it first.
	os.Remove(filepath.Join(e.root, "b.md"))
	a.Stale([]string{"b.md"})
	ctx := context.Background()
	deadline := time.Now().Add(5 * time.Second)
	for {
		paths, _ := a.Indexer.Store.Paths(ctx)
		if slices.Equal(paths, []string{"a.md"}) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("paths = %v", paths)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestStoppedNote(t *testing.T) {
	e := newEnv(t, "a.md")
	os.WriteFile(filepath.Join(e.root, ".screeignore"), nil, 0o644)
	os.Chmod(filepath.Join(e.root, ".screeignore"), 0)
	t.Cleanup(func() { os.Chmod(filepath.Join(e.root, ".screeignore"), 0o644) })
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("mode 000 is not enforced")
	}
	a, _, done := e.writer(t, constEmbedder{})
	err := <-done
	done <- err
	if err == nil || a.Status().Phase != Stopped || a.Holding() {
		t.Fatalf("Run = %v, status %+v", err, a.Status())
	}
	if n := a.Note(context.Background()); !strings.HasPrefix(n, "[index not updating: ") {
		t.Errorf("note = %q", n)
	}
}
