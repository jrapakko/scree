// Package index keeps the store in step with a folder of markdown notes.
package index

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jrapakko/scree/internal/chunk"
	"github.com/jrapakko/scree/internal/embed"
	"github.com/jrapakko/scree/internal/policy"
	"github.com/jrapakko/scree/internal/store"
)

// Indexer embeds the notes its Policy allows into Store.
type Indexer struct {
	Policy   *policy.Policy
	Store    *store.Store
	Embedder embed.Embedder
	// QueryEmbedder embeds search queries; nil means Embedder.
	QueryEmbedder embed.Embedder
	Log           *log.Logger
	// MinBackoff is the first pause after the embedding API is unavailable;
	// each further failure doubles it, up to MaxBackoff. 0 means 30 s.
	MinBackoff time.Duration

	mu         sync.Mutex
	pauseUntil time.Time
	backoff    time.Duration
}

// MaxBackoff caps the pause after repeated API outages.
const MaxBackoff = 5 * time.Minute

// ErrPaused is returned instead of calling the API while indexing is paused
// after an outage.
var ErrPaused = fmt.Errorf("%w: paused after a recent failure", embed.ErrUnavailable)

// PausedUntil returns when indexing may call the API again, or the zero time
// if it is not paused.
func (ix *Indexer) PausedUntil() time.Time {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.pauseUntil
}

// embed calls the API unless paused, and updates the pause from the result.
func (ix *Indexer) embed(ctx context.Context, inputs []string) ([][]float32, error) {
	ix.mu.Lock()
	paused := time.Now().Before(ix.pauseUntil)
	ix.mu.Unlock()
	if paused {
		return nil, ErrPaused
	}
	vecs, err := ix.Embedder.Embed(ctx, inputs)
	ix.mu.Lock()
	defer ix.mu.Unlock()
	switch {
	case err == nil:
		ix.backoff, ix.pauseUntil = 0, time.Time{}
	case errors.Is(err, embed.ErrUnavailable):
		if ix.backoff == 0 {
			ix.backoff = ix.MinBackoff
			if ix.backoff <= 0 {
				ix.backoff = 30 * time.Second
			}
		} else {
			ix.backoff *= 2
		}
		ix.backoff = min(ix.backoff, MaxBackoff)
		ix.pauseUntil = time.Now().Add(ix.backoff)
	}
	return vecs, err
}

// Root returns the notes folder, with symlinks resolved.
func (ix *Indexer) Root() string { return ix.Policy.Root() }

// Rel converts an absolute path under the notes folder to the
// slash-separated form used as a key in the store. It fails for paths
// outside the folder.
func (ix *Indexer) Rel(abs string) (string, error) { return ix.Policy.Rel(abs) }

// IndexFile embeds rel if its content changed since it was last indexed. It
// removes rel from the index if the file no longer exists. If the policy
// refuses rel, any rows for it are removed and the *policy.IgnoredError is
// returned, with changed reporting whether rows were removed.
func (ix *Indexer) IndexFile(ctx context.Context, rel string) (changed bool, err error) {
	clean, err := ix.Policy.Clean(rel)
	if err == nil {
		var src []byte
		src, err = ix.Policy.Read(clean)
		switch {
		case err == nil:
			return ix.indexSrc(ctx, clean, src)
		case errors.Is(err, fs.ErrNotExist):
			return ix.remove(ctx, clean)
		}
	}
	var ie *policy.IgnoredError
	if errors.As(err, &ie) && ie.Reason != policy.Invalid {
		removed, rerr := ix.remove(ctx, ie.Path)
		if rerr != nil {
			return removed, rerr
		}
		return removed, err
	}
	return false, err
}

// indexSrc stores src as the content of rel, embedding it only if its hash
// changed.
func (ix *Indexer) indexSrc(ctx context.Context, rel string, src []byte) (bool, error) {
	sum := sha256.Sum256(src)
	hash := hex.EncodeToString(sum[:])
	have, err := ix.Store.FileHash(ctx, rel)
	if err != nil || have == hash {
		return false, err
	}

	pieces := chunk.Split(string(src))
	chunks := make([]store.Chunk, len(pieces))
	inputs := make([]string, len(pieces))
	for i, p := range pieces {
		chunks[i] = store.Chunk{Path: rel, Heading: p.Heading, Line: p.Line, Text: p.Text}
		// The path and heading give the model context the text alone may lack.
		inputs[i] = rel + "\n" + p.Heading + "\n\n" + p.Text
	}
	var vecs [][]float32
	if len(inputs) > 0 {
		if vecs, err = ix.embed(ctx, inputs); err != nil {
			return false, fmt.Errorf("%q: %w", rel, err)
		}
	}
	if err := ix.Store.ReplaceFile(ctx, rel, hash, time.Now().Unix(), chunks, vecs); err != nil {
		return false, fmt.Errorf("%q: %w", rel, err)
	}
	ix.logf("indexed %q (%d chunks)", rel, len(chunks))
	return true, nil
}

func (ix *Indexer) remove(ctx context.Context, rel string) (bool, error) {
	have, err := ix.Store.FileHash(ctx, rel)
	if err != nil || have == "" {
		return false, err
	}
	if err := ix.Store.RemoveFile(ctx, rel); err != nil {
		return false, err
	}
	ix.logf("removed %q", rel)
	return true, nil
}

// RemovePrefix drops every indexed file under the directory rel, for when a
// folder is deleted or moved away.
func (ix *Indexer) RemovePrefix(ctx context.Context, rel string) error {
	paths, err := ix.Store.Paths(ctx)
	if err != nil {
		return err
	}
	for _, p := range paths {
		if strings.HasPrefix(p, rel+"/") {
			if _, err := ix.remove(ctx, p); err != nil {
				return err
			}
		}
	}
	return nil
}

// SyncStats reports what a Sync did. Deferred counts files left for later
// because the embedding API was unavailable; their old rows are kept.
type SyncStats struct {
	Scanned, Indexed, Removed, Failed, Deferred int
}

// Sync indexes every new or changed note the policy allows and drops notes
// that no longer exist or are now ignored. Files are processed one at a
// time. A failure on one file is logged and does not stop the rest, and its
// existing rows are kept. Sync fails if .screeignore cannot be read.
func (ix *Indexer) Sync(ctx context.Context) (SyncStats, error) {
	var (
		st   SyncStats
		seen = map[string]bool{}
		keep []string // folders that could not be read: keep their rows
	)
	err := ix.Policy.Walk(ctx, func(rel string, src []byte, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, policy.ErrIgnored) {
			return nil
		}
		st.Scanned++
		seen[rel] = true
		if err != nil {
			if strings.HasSuffix(rel, "/") {
				keep = append(keep, rel)
			}
			st.Failed++
			ix.logf("error: %v", err)
			return nil
		}
		changed, err := ix.indexSrc(ctx, rel, src)
		switch {
		case errors.Is(err, embed.ErrUnavailable):
			if st.Deferred == 0 {
				ix.logf("error: %v; deferring notes until the API is back", err)
			}
			st.Deferred++
		case err != nil:
			st.Failed++
			ix.logf("error: %v", err)
		case changed:
			st.Indexed++
		}
		return nil
	})
	if err != nil {
		return st, err
	}
	paths, err := ix.Store.Paths(ctx)
	if err != nil {
		return st, err
	}
	for _, p := range paths {
		if seen[p] || slices.ContainsFunc(keep, func(dir string) bool { return strings.HasPrefix(p, dir) }) {
			continue
		}
		if _, err := ix.remove(ctx, p); err != nil {
			return st, err
		}
		st.Removed++
	}
	return st, nil
}

// Search embeds query and returns the k nearest chunks. An empty index
// returns no hits without calling the API.
func (ix *Indexer) Search(ctx context.Context, query string, k int) ([]store.Hit, error) {
	if n, _ := ix.Store.ChunkCount(ctx); n == 0 {
		return nil, nil
	}
	e := ix.QueryEmbedder
	if e == nil {
		e = ix.Embedder
	}
	vecs, err := e.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	return ix.Store.Search(ctx, vecs[0], k)
}

func (ix *Indexer) logf(format string, args ...any) {
	if ix.Log != nil {
		ix.Log.Printf(format, args...)
	}
}
