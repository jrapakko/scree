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

// Test hooks; nil outside tests.
var (
	// onWatcher receives the watcher Run creates.
	onWatcher func(*fsnotify.Watcher)
	// errInject delivers errors to Run as if from the watcher.
	errInject chan error
	// suppressEvents, when it returns true, makes Run drop every file event,
	// standing in for events the kernel lost.
	suppressEvents func() bool
	// onEvent is called for each file event Run reads.
	onEvent func()
)

// Options configures RunOptions.
type Options struct {
	// Stale delivers store keys to re-check, such as paths a search found
	// refused or missing. They are handled like file events.
	Stale <-chan []string
}

// Run watches the notes folder until ctx is done. It does not do an initial
// sync; call ix.Sync first. It returns an error if the notes folder or the
// index file is removed or replaced, so a supervisor can restart it.
func Run(ctx context.Context, ix *index.Indexer) error {
	return RunOptions(ctx, ix, Options{})
}

// RunOptions is Run with options.
//
// File events are read on one goroutine, which debounces them into a set of
// dirty paths and never waits on the store or the embedding API. A single
// worker goroutine does every write: it handles dirty paths one at a time and
// runs a full Sync when an ignore file changed, when the kernel's event queue
// overflowed (then it also rebuilds the watches), or when an API outage ends.
func RunOptions(ctx context.Context, ix *index.Indexer, o Options) error {
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

	ctx, cancel := context.WithCancel(ctx)
	q := &workQueue{dirty: map[string]struct{}{}, wake: make(chan struct{}, 1)}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		worker(ctx, w, ix, q)
	}()
	defer func() {
		// Stop the worker and wait, so nothing writes after Run returns.
		cancel()
		wg.Wait()
	}()

	var (
		mu      sync.Mutex
		pending = map[string]*time.Timer{}
	)
	defer func() {
		mu.Lock()
		for _, t := range pending {
			t.Stop()
		}
		mu.Unlock()
	}()
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
			q.add(abs)
		})
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
		case paths := <-o.Stale:
			for _, rel := range paths {
				q.add(filepath.Join(root, filepath.FromSlash(rel)))
			}
		case err := <-errInject:
			watchError(ix, q, err)
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			watchError(ix, q, err)
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			if onEvent != nil {
				onEvent()
			}
			if suppressEvents != nil && suppressEvents() {
				continue
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
		}
	}
}

// watchError handles an error from the watcher. An event queue overflow
// means events were lost, so the worker resyncs and rebuilds the watches.
func watchError(ix *index.Indexer, q *workQueue, err error) {
	if errors.Is(err, fsnotify.ErrEventOverflow) {
		ix.Log.Printf("watch: event queue overflowed; resyncing")
		q.requestSync(true)
		return
	}
	ix.Log.Printf("watch: %v", err)
}

// workQueue hands work from the event goroutine to the worker.
type workQueue struct {
	mu       sync.Mutex
	dirty    map[string]struct{} // absolute paths to handle
	needSync bool
	rebuild  bool          // rebuild the watches after the sync
	wake     chan struct{} // buffer 1: there is work
}

func (q *workQueue) add(abs string) {
	q.mu.Lock()
	q.dirty[abs] = struct{}{}
	q.mu.Unlock()
	q.signal()
}

func (q *workQueue) requestSync(rebuild bool) {
	q.mu.Lock()
	q.needSync = true
	q.rebuild = q.rebuild || rebuild
	q.mu.Unlock()
	q.signal()
}

func (q *workQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// next takes one piece of work: a sync (which clears every dirty path, since
// it covers them) or one dirty path. ok is false when there is none.
func (q *workQueue) next() (doSync, rebuild bool, abs string, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.needSync {
		rebuild = q.rebuild
		q.needSync, q.rebuild = false, false
		clear(q.dirty)
		return true, rebuild, "", true
	}
	for p := range q.dirty {
		delete(q.dirty, p)
		return false, false, p, true
	}
	return false, false, "", false
}

// worker is the only goroutine that writes to the index while Run runs.
func worker(ctx context.Context, w *fsnotify.Watcher, ix *index.Indexer, q *workQueue) {
	// resync runs a Sync once an API pause ends, which picks up every note
	// deferred during the outage because its stored hash is stale.
	var (
		resync  *time.Timer
		resyncC <-chan time.Time
	)
	armResync := func() {
		if resyncC != nil {
			return
		}
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
	for {
		select {
		case <-ctx.Done():
			return
		case <-resyncC:
			resyncC = nil
			q.requestSync(false)
		case <-q.wake:
		}
		// One batch of folder listings per drain, renewed after a sync.
		canon := ix.Policy.NewCanon()
		for ctx.Err() == nil {
			doSync, rebuild, abs, ok := q.next()
			if !ok {
				break
			}
			if !doSync {
				if rel, err := ix.Rel(abs); err == nil && isIgnoreFile(rel) {
					// The rules changed: watch newly included folders and
					// drop newly ignored ones, then sync, which also picks up
					// anything created while the watches were changing.
					if err := syncWatches(w, ix); err != nil {
						ix.Log.Printf("watch: %v", err)
					}
					if runSync(ctx, ix, rel+" changed") {
						armResync()
					}
					canon = ix.Policy.NewCanon()
					continue
				}
				if handle(ctx, ix, canon, abs) {
					armResync()
				}
				continue
			}
			what := "resync"
			if rebuild {
				what = "resync after event overflow"
			}
			deferred := runSync(ctx, ix, what)
			canon = ix.Policy.NewCanon()
			if rebuild {
				if err := syncWatches(w, ix); err != nil {
					ix.Log.Printf("watch: %v", err)
				}
			}
			if deferred {
				armResync()
			}
		}
	}
}

// runSync runs a Sync and logs it. It reports whether notes were deferred
// because the embedding API is unavailable.
func runSync(ctx context.Context, ix *index.Indexer, what string) (deferred bool) {
	st, err := ix.Sync(ctx)
	if err != nil {
		if ctx.Err() == nil {
			ix.Log.Printf("error: %v", err)
		}
		return errors.Is(err, embed.ErrUnavailable)
	}
	ix.Log.Printf("%s: %d files, %d indexed, %d removed, %d failed, %d deferred",
		what, st.Scanned, st.Indexed, st.Removed, st.Failed, st.Deferred)
	return st.Deferred > 0
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
func handle(ctx context.Context, ix *index.Indexer, canon *policy.Canon, abs string) (deferred bool) {
	rel, err := ix.Rel(abs)
	if err != nil {
		return false
	}
	// Match and store the on-disk name only. A missing path keeps its
	// spelling, which is how it was stored.
	if c, err := canon.Canonical(rel); err == nil {
		rel = c
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
	case canon.CheckName(rel) != policy.Allowed:
		// Not a note; IndexFile only drops any stale rows.
		ix.IndexFileCanon(ctx, canon, rel)
		return false
	}
	_, err = ix.IndexFileCanon(ctx, canon, rel)
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
