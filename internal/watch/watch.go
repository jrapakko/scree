// Package watch re-indexes notes as they change on disk.
package watch

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/jrapakko/scree/internal/index"
)

// Debounce is how long a path must be quiet before it is indexed. Editors
// fire several events per save.
const Debounce = 1500 * time.Millisecond

// Run watches ix.Root until ctx is done. It does not do an initial sync;
// call ix.Sync first.
func Run(ctx context.Context, ix *index.Indexer) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()

	if err := addTree(w, ix.Root); err != nil {
		return err
	}

	var (
		mu      sync.Mutex
		pending = map[string]*time.Timer{}
		ready   = make(chan string, 256)
	)
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
			}
		})
	}

	for {
		select {
		case <-ctx.Done():
			return nil
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
			name := filepath.Base(ev.Name)
			if strings.HasPrefix(name, ".") {
				continue
			}
			if ev.Has(fsnotify.Create) {
				if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
					// A new or moved-in folder: watch it and pick up its notes.
					addTree(w, ev.Name)
					queueTree(ev.Name, queue)
					continue
				}
			}
			queue(ev.Name)
		case abs := <-ready:
			// One path at a time, on this goroutine, so there is one writer.
			handle(ctx, ix, abs)
		}
	}
}

func handle(ctx context.Context, ix *index.Indexer, abs string) {
	rel, err := ix.Rel(abs)
	if err != nil {
		return
	}
	fi, err := os.Stat(abs)
	switch {
	case err == nil && fi.IsDir():
		return
	case err != nil:
		// Gone: it may have been a note or a whole folder.
		if err := ix.RemovePrefix(ctx, rel); err != nil {
			ix.Log.Printf("error: %v", err)
		}
		if !index.IsNote(rel) {
			return
		}
	case !index.IsNote(rel):
		return
	}
	if _, err := ix.IndexFile(ctx, rel); err != nil {
		ix.Log.Printf("error: %v", err)
	}
}

func addTree(w *fsnotify.Watcher, root string) error {
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
