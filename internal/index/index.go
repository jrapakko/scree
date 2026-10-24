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
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/jrapakko/scree/internal/chunk"
	"github.com/jrapakko/scree/internal/embed"
	"github.com/jrapakko/scree/internal/store"
)

// Indexer embeds notes under Root into Store.
type Indexer struct {
	Root     string
	Store    *store.Store
	Embedder embed.Embedder
	Log      *log.Logger
}

// IsNote reports whether a relative path should be indexed.
func IsNote(rel string) bool {
	for _, part := range strings.Split(rel, "/") {
		if strings.HasPrefix(part, ".") {
			return false
		}
	}
	ext := strings.ToLower(path.Ext(rel))
	return ext == ".md" || ext == ".markdown"
}

// Rel converts an absolute path under Root to the slash-separated form used
// as a key in the store. It fails for paths outside Root.
func (ix *Indexer) Rel(abs string) (string, error) {
	rel, err := filepath.Rel(ix.Root, abs)
	if err != nil {
		return "", err
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("%s is outside %s", abs, ix.Root)
	}
	return rel, nil
}

// Abs resolves a store key to a file path, refusing anything that would
// escape Root.
func (ix *Indexer) Abs(rel string) (string, error) {
	clean := path.Clean("/" + filepath.ToSlash(rel))[1:]
	if clean == "" || clean != strings.TrimPrefix(filepath.ToSlash(rel), "./") {
		return "", fmt.Errorf("invalid note path %q", rel)
	}
	return filepath.Join(ix.Root, filepath.FromSlash(clean)), nil
}

// IndexFile embeds rel if its content changed since it was last indexed. It
// removes rel from the index if the file no longer exists.
func (ix *Indexer) IndexFile(ctx context.Context, rel string) (changed bool, err error) {
	abs, err := ix.Abs(rel)
	if err != nil {
		return false, err
	}
	src, err := os.ReadFile(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return ix.remove(ctx, rel)
	}
	if err != nil {
		return false, err
	}
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
		if vecs, err = ix.Embedder.Embed(ctx, inputs); err != nil {
			return false, fmt.Errorf("%s: %w", rel, err)
		}
	}
	if err := ix.Store.ReplaceFile(ctx, rel, hash, time.Now().Unix(), chunks, vecs); err != nil {
		return false, fmt.Errorf("%s: %w", rel, err)
	}
	ix.logf("indexed %s (%d chunks)", rel, len(chunks))
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
	ix.logf("removed %s", rel)
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

// SyncStats reports what a Sync did.
type SyncStats struct {
	Scanned, Indexed, Removed, Failed int
}

// Sync indexes every new or changed note under Root and drops notes that no
// longer exist. Files are processed one at a time. A failure on one file is
// logged and does not stop the rest.
func (ix *Indexer) Sync(ctx context.Context) (SyncStats, error) {
	var st SyncStats
	seen := map[string]bool{}
	err := filepath.WalkDir(ix.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			ix.logf("walk: %v", err)
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if p == ix.Root {
			return nil
		}
		if d.IsDir() && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		rel, err := ix.Rel(p)
		if err != nil || d.IsDir() || !d.Type().IsRegular() || !IsNote(rel) {
			return nil
		}
		st.Scanned++
		seen[rel] = true
		changed, err := ix.IndexFile(ctx, rel)
		switch {
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
		if !seen[p] {
			if _, err := ix.remove(ctx, p); err != nil {
				return st, err
			}
			st.Removed++
		}
	}
	return st, nil
}

// Search embeds query and returns the k nearest chunks.
func (ix *Indexer) Search(ctx context.Context, query string, k int) ([]store.Hit, error) {
	vecs, err := ix.Embedder.Embed(ctx, []string{query})
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
