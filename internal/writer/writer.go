// Package writer gives one process at a time the write role for an index.
// The holder of the index's lock file syncs the folder, then watches it;
// every other process only reads and retries the lock, taking over when the
// holder exits.
package writer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"sync"
	"time"

	"github.com/jrapakko/scree/internal/index"
	"github.com/jrapakko/scree/internal/lock"
	"github.com/jrapakko/scree/internal/textsafe"
	"github.com/jrapakko/scree/internal/watch"
)

// DefaultRetry is how often a process without the lock tries to take it.
const DefaultRetry = 5 * time.Second

// SyncedAtKey is the meta key the writer sets when a full sync finishes.
const SyncedAtKey = "synced_at"

// Phase is what a Writer is doing.
type Phase int

const (
	Starting Phase = iota // Run has not tried the lock yet
	Waiting               // another process holds the lock
	Syncing               // holding the lock, first sync running
	Watching              // holding the lock, watching for changes
	Skipped               // holding nothing: the first sync was not started (step 6's cap)
	Stopped               // the writer failed; see Status.Err
)

func (p Phase) String() string {
	switch p {
	case Starting:
		return "starting"
	case Waiting:
		return "waiting"
	case Syncing:
		return "syncing"
	case Watching:
		return "watching"
	case Skipped:
		return "skipped"
	case Stopped:
		return "stopped"
	}
	return fmt.Sprintf("Phase(%d)", int(p))
}

// Status is a snapshot of a Writer.
type Status struct {
	Phase     Phase
	HolderPID int   // when Waiting
	Indexed   int   // during Syncing: files indexed so far
	Err       error // when Stopped
}

// Writer runs the write role for one index.
type Writer struct {
	Indexer  *index.Indexer
	LockPath string
	Retry    time.Duration // 0 means DefaultRetry
	Log      *log.Logger   // nil means log.Default()
	// OnAcquire, if set, runs after the lock is taken and before the first
	// sync, for writes only the holder may do (recording the folder and
	// model). An error stops the writer.
	OnAcquire func(context.Context) error

	mu      sync.Mutex
	status  Status
	holding bool
	synced  bool // meta synced_at is known to be set
	stale   chan []string
}

// Run takes the write role whenever it can, until ctx is done: it tries the
// lock, and while another process holds it, waits Retry and tries again.
// Holding the lock, it syncs, records synced_at, and watches. The lock is
// released when Run returns. It returns nil when ctx is done, and the error
// when the sync or the watcher fails.
func (w *Writer) Run(ctx context.Context) error {
	retry := w.Retry
	if retry <= 0 {
		retry = DefaultRetry
	}
	lastPID := -1
	for {
		l, pid, err := lock.TryLock(w.LockPath)
		if errors.Is(err, lock.ErrHeld) {
			w.set(Status{Phase: Waiting, HolderPID: pid})
			if pid != lastPID {
				w.logf("pid %d writes this index; waiting to take over", pid)
				lastPID = pid
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(retry):
			}
			continue
		}
		if err != nil {
			w.stop(err)
			return err
		}
		if lastPID >= 0 {
			w.logf("took over the index from pid %d", lastPID)
		}
		err = w.hold(ctx)
		w.mu.Lock()
		w.holding = false
		w.mu.Unlock()
		l.Release()
		if ctx.Err() != nil {
			return nil
		}
		w.stop(err)
		return err
	}
}

func (w *Writer) hold(ctx context.Context) error {
	ix := w.Indexer
	w.mu.Lock()
	w.holding = true
	w.status = Status{Phase: Syncing}
	if w.stale == nil {
		w.stale = make(chan []string, 16)
	}
	stale := w.stale
	w.mu.Unlock()
	if w.OnAcquire != nil {
		if err := w.OnAcquire(ctx); err != nil {
			return err
		}
	}
	w.logf("syncing %s", ix.Root())
	st, err := ix.Sync(ctx)
	if err != nil {
		return err
	}
	w.logf("sync done: %d notes, %d indexed, %d removed, %d failed, %d deferred",
		st.Scanned, st.Indexed, st.Removed, st.Failed, st.Deferred)
	// A first sync allocates much more than watching does; hand the heap
	// back now rather than holding it for the life of the session.
	debug.FreeOSMemory()
	if err := ix.Store.SetMeta(SyncedAtKey, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	w.mu.Lock()
	w.synced = true
	w.status = Status{Phase: Watching}
	w.mu.Unlock()
	w.logf("watching %s", ix.Root())
	return watch.RunOptions(ctx, ix, watch.Options{Stale: stale})
}

// Progress records Sync progress for Note, for Indexer.Progress.
func (w *Writer) Progress(st index.SyncStats) {
	w.mu.Lock()
	if w.status.Phase == Syncing {
		w.status.Indexed = st.Indexed
	}
	w.mu.Unlock()
}

// Attach sets the Indexer's OnStale and Progress hooks to this writer. Call
// it before the Indexer is shared between goroutines.
func (w *Writer) Attach() {
	w.Indexer.OnStale = w.Stale
	w.Indexer.Progress = w.Progress
}

// Stale queues store keys for the writer to re-check, for Indexer.OnStale.
// It does nothing unless this process holds the lock and is watching, and
// never blocks; a full queue drops them, and the next sync catches up.
func (w *Writer) Stale(paths []string) {
	w.mu.Lock()
	ch, ok := w.stale, w.holding && w.status.Phase == Watching
	w.mu.Unlock()
	if !ok || ch == nil {
		return
	}
	select {
	case ch <- paths:
	default:
	}
}

// Status returns what the writer is doing.
func (w *Writer) Status() Status {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status
}

// Holding reports whether this process holds the write role.
func (w *Writer) Holding() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.holding
}

// Note returns a one-line note for search output, or "" when the index is
// complete and being kept current.
func (w *Writer) Note(ctx context.Context) string {
	st := w.Status()
	if st.Phase == Stopped {
		return fmt.Sprintf("[index not updating: %s]", textsafe.Strip(st.Err.Error()))
	}
	if st.Phase != Syncing && st.Phase != Waiting {
		return ""
	}
	if !w.syncedOnce(ctx) {
		if st.Phase == Syncing {
			return fmt.Sprintf("[index incomplete: first sync in progress, %d notes indexed so far]", st.Indexed)
		}
		return fmt.Sprintf("[index incomplete: another scree process (pid %d) is building it]", st.HolderPID)
	}
	return ""
}

// syncedOnce reports whether a full sync of the index has ever finished.
func (w *Writer) syncedOnce(ctx context.Context) bool {
	w.mu.Lock()
	known := w.synced
	w.mu.Unlock()
	if known {
		return true
	}
	v, err := w.Indexer.Store.Meta(SyncedAtKey)
	if err != nil || v == "" {
		return false
	}
	w.mu.Lock()
	w.synced = true
	w.mu.Unlock()
	return true
}

func (w *Writer) set(st Status) {
	w.mu.Lock()
	w.status = st
	w.mu.Unlock()
}

func (w *Writer) stop(err error) {
	if err == nil {
		err = errors.New("writer stopped")
	}
	w.set(Status{Phase: Stopped, Err: err})
	w.logf("writer stopped: %v", err)
}

func (w *Writer) logf(format string, args ...any) {
	l := w.Log
	if l == nil {
		l = log.Default()
	}
	l.Printf(format, args...)
}
