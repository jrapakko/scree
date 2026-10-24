package watch

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jrapakko/scree/internal/index"
	"github.com/jrapakko/scree/internal/policy"
	"github.com/jrapakko/scree/internal/store"
)

// countEmbedder is constEmbedder that counts the files it embedded.
type countEmbedder struct {
	constEmbedder
	n atomic.Int64
}

func (c *countEmbedder) Embed(ctx context.Context, in []string) ([][]float32, error) {
	c.n.Add(1)
	return c.constEmbedder.Embed(ctx, in)
}

// BenchmarkWatch2000In20000 indexes 20,000 notes in one folder, then
// rewrites 2,000 of them and times until the watcher has re-embedded all of
// them. Before the Canonical fast path and per-batch folder listings, each
// event listed the 20,000-entry folder several times and a batch took about
// 160 s; the target is under 10 s on Linux. Run it with -benchtime=1x.
func BenchmarkWatch2000In20000(b *testing.B) {
	const total, changed = 20000, 2000
	dir := b.TempDir()
	root := filepath.Join(dir, "notes")
	if err := os.MkdirAll(root, 0o755); err != nil {
		b.Fatal(err)
	}
	for i := range total {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("n%05d.md", i)), []byte(fmt.Sprintf("# note %d\n", i)), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	s, err := store.Open(filepath.Join(dir, "index.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	p, err := policy.Open(root, 0)
	if err != nil {
		b.Fatal(err)
	}
	defer p.Close()
	emb := &countEmbedder{}
	ix := &index.Indexer{Policy: p, Store: s, Embedder: emb, Log: log.New(os.Stderr, "", 0)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := ix.Sync(ctx); err != nil {
		b.Fatal(err)
	}
	ix.Log.SetOutput(discard{})
	done := make(chan error, 1)
	go func() { done <- Run(ctx, ix) }()
	time.Sleep(500 * time.Millisecond)

	b.ResetTimer()
	for it := 0; it < b.N; it++ {
		before := emb.n.Load()
		start := time.Now()
		for i := range changed {
			name := filepath.Join(root, fmt.Sprintf("n%05d.md", i*(total/changed)))
			if err := os.WriteFile(name, []byte(fmt.Sprintf("# note %d\nrevision %d\n", i, it)), 0o644); err != nil {
				b.Fatal(err)
			}
		}
		for emb.n.Load()-before < changed {
			if time.Since(start) > 5*time.Minute {
				b.Fatalf("only %d of %d re-embedded after 5 min", emb.n.Load()-before, changed)
			}
			time.Sleep(20 * time.Millisecond)
		}
		b.ReportMetric(time.Since(start).Seconds(), "s/batch")
	}
	b.StopTimer()
	cancel()
	if err := <-done; err != nil {
		b.Error(err)
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
