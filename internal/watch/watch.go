// Package watch re-indexes notes as they change on disk.
package watch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
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

// onWatcher, when set by a test, receives the watcher Run creates.
var onWatcher func(*fsnotify.Watcher)

// Run watches the notes folder until ctx is done. It does not do an initial
// sync; call ix.Sync first. It returns an error if the notes folder or the
// index file is removed or replaced, so a supervisor can restart it.
func Run(ctx context.Context, ix *index.Indexer) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()
	if onWatcher != nil {
		onWatcher(w)
	}

	root := ix.Root()
	if err := addTree(w, ix, root); err != nil {
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
			if dropEvent(ix, ev.Name) {
				continue
			}
			if ev.Has(fsnotify.Create) {
				// Lstat: a symlinked folder is never watched or descended.
				if fi, err := os.Lstat(ev.Name); err == nil && fi.IsDir() {
					// A new or moved-in folder: watch it and pick up its
					// notes, unless the policy skips it.
					addTree(w, ix, ev.Name)
					queueTree(ix, ev.Name, queue)
					continue
				}
			}
			queue(ev.Name)
		case abs := <-ready:
			// One path at a time, on this goroutine, so there is one writer.
			if handle(ctx, w, ix, abs) && resyncC == nil {
				armResync()
			}
		}
	}
}

// dropEvent reports whether an event for abs can be ignored without a
// look: a hidden path the policy skips both as a file and as a folder.
// .screeignore and every .gitignore always pass, and so does a path under an
// allowed root folder such as .github/. Other ignored paths pass too, so
// handle can drop stale rows.
func dropEvent(ix *index.Indexer, abs string) bool {
	rel, err := ix.Rel(abs)
	if err != nil || rel == "." {
		return false
	}
	if rel == policy.IgnoreFileName || path.Base(rel) == policy.GitIgnoreFileName {
		return false
	}
	if !strings.HasPrefix(rel, ".") && !strings.Contains(rel, "/.") {
		return false
	}
	return ix.Policy.CheckName(rel) == policy.Hidden && ix.Policy.SkipDir(rel) == policy.Hidden
}

// isIgnoreFile reports whether rel is .screeignore or any .gitignore.
func isIgnoreFile(rel string) bool {
	return rel == policy.IgnoreFileName || path.Base(rel) == policy.GitIgnoreFileName
}

// handle indexes or removes one changed path. It reports whether a note was
// deferred because the embedding API is unavailable.
func handle(ctx context.Context, w *fsnotify.Watcher, ix *index.Indexer, abs string) (deferred bool) {
	rel, err := ix.Rel(abs)
	if err != nil {
		return false
	}
	if isIgnoreFile(rel) {
		// The rules changed: watch newly included folders and drop newly
		// ignored ones, then sync, which also picks up anything created
		// while the watches were changing.
		if err := syncWatches(w, ix); err != nil {
			ix.Log.Printf("watch: %v", err)
		}
		st, err := ix.Sync(ctx)
		if err != nil {
			ix.Log.Printf("error: %v", err)
			return false
		}
		ix.Log.Printf("%s changed: %d notes, %d indexed, %d removed, %d failed, %d deferred",
			rel, st.Scanned, st.Indexed, st.Removed, st.Failed, st.Deferred)
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

// skipDir reports whether the folder abs is not watched: the policy skips
// it (hidden, gitignored, a nested repository, or excluded by .screeignore),
// or it is not under the root. The root itself is always watched.
func skipDir(ix *index.Indexer, abs string) bool {
	if abs == ix.Root() {
		return false
	}
	rel, err := ix.Rel(abs)
	switch {
	case err != nil:
		return true
	case rel == ".":
		return false
	}
	return ix.Policy.SkipDir(rel) != policy.Allowed
}

// walkDirs calls fn for every folder under top that is watched, without
// descending into the ones the policy skips.
func walkDirs(ix *index.Indexer, top string, fn func(string) error) error {
	return filepath.WalkDir(top, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if skipDir(ix, p) {
			return filepath.SkipDir
		}
		return fn(p)
	})
}

// addTree drops the watches on top and everything under it, then watches
// every folder there the policy allows.
func addTree(w *fsnotify.Watcher, ix *index.Indexer, top string) error {
	dropWatches(w, top)
	return walkDirs(ix, top, w.Add)
}

// syncWatches makes the watch list match the policy after an ignore file
// changed: it adds folders that are now allowed and removes the ones that
// are now skipped. Unlike addTree it never drops a watch that stays.
func syncWatches(w *fsnotify.Watcher, ix *index.Indexer) error {
	want := map[string]bool{}
	err := walkDirs(ix, ix.Root(), func(p string) error {
		want[p] = true
		return nil
	})
	if err != nil {
		return err
	}
	for _, p := range w.WatchList() {
		if !want[p] {
			w.Remove(p)
		}
	}
	have := map[string]bool{}
	for _, p := range w.WatchList() {
		have[p] = true
	}
	for p := range want {
		if !have[p] {
			if err := w.Add(p); err != nil {
				return err
			}
		}
	}
	return nil
}

// queueTree queues every file under top, in the folders the policy allows.
func queueTree(ix *index.Indexer, top string, queue func(string)) {
	filepath.WalkDir(top, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDir(ix, p) {
				return filepath.SkipDir
			}
			return nil
		}
		queue(p)
		return nil
	})
}
