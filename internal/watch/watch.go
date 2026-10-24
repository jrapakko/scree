// Package watch re-indexes notes as they change on disk.
package watch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/jrapakko/scree/internal/embed"
	"github.com/jrapakko/scree/internal/index"
	"github.com/jrapakko/scree/internal/policy"
)

// Debounce is how long a path must be quiet before it is indexed. Editors
// fire several events per save.
const Debounce = 1500 * time.Millisecond

// HealthInterval is how often Run checks that the notes folder and the index
// file are still the ones it started with. A variable so tests can shorten it.
var HealthInterval = 10 * time.Second

// Run watches the notes folder until ctx is done. It does not do an initial
// sync; call ix.Sync first. It returns an error if the notes folder or the
// index file is removed or replaced, so a supervisor can restart it.
func Run(ctx context.Context, ix *index.Indexer) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()

	root := ix.Root()
	ignoreFile := filepath.Join(root, policy.IgnoreFileName)
	if err := addTree(w, root); err != nil {
		return err
	}

	var (
		mu      sync.Mutex
		pending = map[string]*time.Timer{}
		ready   = make(chan string, 256)
		stopped = make(chan struct{})
	)
	defer close(stopped)
	queue := func(abs string) {
		mu.Lock()
		defer mu.Unlock()
		if t, ok := pending[abs]; ok {
			t.Reset(Debounce)
			return
		}
		pending[abs] = time.AfterFunc(Debounce, func() {
			mu.Lock()
			delete(pending, abs)
			mu.Unlock()
			select {
			case ready <- abs:
			case <-ctx.Done():
			case <-stopped:
			}
		})
	}

	// resync runs a Sync once an API pause ends, which picks up every note
	// deferred during the outage because its stored hash is stale.
	var (
		resync  *time.Timer
		resyncC <-chan time.Time
	)
	armResync := func() {
		d := time.Until(ix.PausedUntil()) + 100*time.Millisecond
		if resync == nil {
			resync = time.NewTimer(d)
		} else {
			resync.Reset(d)
		}
		resyncC = resync.C
	}
	defer func() {
		if resync != nil {
			resync.Stop()
		}
	}()
	if ix.PausedUntil().After(time.Now()) {
		armResync()
	}

	health := time.NewTicker(HealthInterval)
	defer health.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-health.C:
			if err := ix.Policy.CheckRoot(); err != nil {
				return err
			}
			if err := ix.Store.CheckFile(); err != nil {
				return err
			}
		case <-resyncC:
			resyncC = nil
			st, err := ix.Sync(ctx)
			if err != nil {
				ix.Log.Printf("error: %v", err)
			} else {
				ix.Log.Printf("resync after API outage: %d indexed, %d removed, %d failed, %d deferred",
					st.Indexed, st.Removed, st.Failed, st.Deferred)
			}
			if st.Deferred > 0 || errors.Is(err, embed.ErrUnavailable) {
				armResync()
			}
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			ix.Log.Printf("watch: %v", err)
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			if ev.Has(fsnotify.Chmod) && !ev.Has(fsnotify.Write) {
				continue
			}
			if ev.Has(fsnotify.Rename) || ev.Has(fsnotify.Remove) {
				if ev.Name == root {
					return fmt.Errorf("notes folder %s was removed or moved", root)
				}
				// The old name's watches are stale: a later Add of the new
				// name would otherwise reuse them, still carrying old paths.
				dropWatches(w, ev.Name)
			}
			name := filepath.Base(ev.Name)
			if strings.HasPrefix(name, ".") && ev.Name != ignoreFile {
				continue
			}
			if ev.Has(fsnotify.Create) {
				// Lstat: a symlinked folder is never watched or descended.
				if fi, err := os.Lstat(ev.Name); err == nil && fi.IsDir() {
					// A new or moved-in folder: watch it and pick up its notes.
					addTree(w, ev.Name)
					queueTree(ev.Name, queue)
					continue
				}
			}
			queue(ev.Name)
		case abs := <-ready:
			// One path at a time, on this goroutine, so there is one writer.
			if handle(ctx, ix, abs) && resyncC == nil {
				armResync()
			}
		}
	}
}

// handle indexes or removes one changed path. It reports whether a note was
// deferred because the embedding API is unavailable.
func handle(ctx context.Context, ix *index.Indexer, abs string) (deferred bool) {
	rel, err := ix.Rel(abs)
	if err != nil {
		return false
	}
	if rel == policy.IgnoreFileName {
		// The rules changed: drop newly ignored notes, add newly included ones.
		st, err := ix.Sync(ctx)
		if err != nil {
			ix.Log.Printf("error: %v", err)
			return false
		}
		ix.Log.Printf("%s changed: %d notes, %d indexed, %d removed, %d failed, %d deferred",
			policy.IgnoreFileName, st.Scanned, st.Indexed, st.Removed, st.Failed, st.Deferred)
		return st.Deferred > 0
	}
	fi, err := os.Lstat(abs)
	switch {
	case err != nil:
		// Gone: it may have been a note or a whole folder.
		if err := ix.RemovePrefix(ctx, rel); err != nil {
			ix.Log.Printf("error: %v", err)
		}
	case fi.IsDir():
		return false
	case ix.Policy.CheckName(rel) != policy.Allowed:
		// Not a note; IndexFile only drops any stale rows.
		ix.IndexFile(ctx, rel)
		return false
	}
	_, err = ix.IndexFile(ctx, rel)
	switch {
	case errors.Is(err, index.ErrPaused):
		// Already logged when the pause began.
		return true
	case errors.Is(err, embed.ErrUnavailable):
		ix.Log.Printf("error: %v; will retry when the API is back", err)
		return true
	case err != nil && !errors.Is(err, policy.ErrIgnored):
		ix.Log.Printf("error: %v", err)
	}
	return false
}

// dropWatches removes the watches on abs and every folder under it.
func dropWatches(w *fsnotify.Watcher, abs string) {
	for _, p := range w.WatchList() {
		if p == abs || strings.HasPrefix(p, abs+string(filepath.Separator)) {
			// The kernel may already have dropped it (ErrNonExistentWatch);
			// either way it is gone.
			w.Remove(p)
		}
	}
}

func addTree(w *fsnotify.Watcher, root string) error {
	dropWatches(w, root)
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if p != root && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		return w.Add(p)
	})
}

func queueTree(root string, queue func(string)) {
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			queue(p)
		}
		return nil
	})
}
