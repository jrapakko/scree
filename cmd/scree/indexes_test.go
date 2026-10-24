package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/jrapakko/scree/internal/lock"
	"github.com/jrapakko/scree/internal/store"
)

// indexesEnv points XDG_DATA_HOME at a temp dir and returns it.
func indexesEnv(t *testing.T) string {
	t.Helper()
	data := tempDir(t)
	t.Setenv("XDG_DATA_HOME", data)
	return data
}

// makeIndex builds an index for a new folder named name the way claim
// does (meta root, root file), with one indexed file, and returns the
// folder and the index directory.
func makeIndex(t *testing.T, name string, synced bool) (root, dir string) {
	t.Helper()
	root = filepath.Join(tempDir(t), name)
	mkdir(t, root)
	dir = indexDir(root)
	s, err := store.Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CheckRoot(root); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckModel("m1"); err != nil {
		t.Fatal(err)
	}
	if synced {
		s.SetMeta("synced_at", "2026-10-24T08:00:00Z")
	}
	chunks := []store.Chunk{{Path: "a.md", Text: "x"}, {Path: "a.md", Text: "y"}}
	if err := s.ReplaceFile(context.Background(), "a.md", "h", 1, nil, chunks, [][]float32{{1, 0}, {0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := writeRootFile(dir, root); err != nil {
		t.Fatal(err)
	}
	return root, dir
}

func runIndexesOut(t *testing.T, prune bool) (string, error) {
	t.Helper()
	var b strings.Builder
	err := indexes(&b, prune)
	return b.String(), err
}

// lineFor returns the listing line for root.
func lineFor(t *testing.T, out, root string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, quote(root)+"  ") {
			return l
		}
	}
	t.Fatalf("no line for %s in:\n%s", root, out)
	return ""
}

func TestIndexesList(t *testing.T) {
	indexesEnv(t)
	live, liveDir := makeIndex(t, "live", true)
	gone, _ := makeIndex(t, "gone", false)
	os.RemoveAll(gone)
	out, err := runIndexesOut(t, false)
	if err != nil {
		t.Fatal(err)
	}
	l := lineFor(t, out, live)
	for _, want := range []string{"1 files, 2 chunks", "last sync 2026-10-24T08:00:00Z", "model m1", filepath.Base(liveDir)} {
		if !strings.Contains(l, want) {
			t.Errorf("live line lacks %q: %s", want, l)
		}
	}
	if strings.Contains(l, "[missing]") || strings.Contains(l, "[in use") {
		t.Errorf("live line: %s", l)
	}
	if g := lineFor(t, out, gone); !strings.Contains(g, "[missing]") || !strings.Contains(g, "last sync never") {
		t.Errorf("gone line: %s", g)
	}
}

func TestIndexesShowsWriter(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("no file locking on this OS")
	}
	indexesEnv(t)
	root, dir := makeIndex(t, "held", false)
	l, _, err := lock.TryLock(filepath.Join(dir, "writer.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	out, _ := runIndexesOut(t, false)
	if line := lineFor(t, out, root); !strings.Contains(line, "[in use by pid ") {
		t.Errorf("held line: %s", line)
	}
}

// snapshot lists every path under dir with its size and mtime.
func snapshot(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil {
			out = append(out, fmt.Sprintf("%s %v %d", p, fi.ModTime(), fi.Size()))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// TestIndexesNeverWrites: listing creates no file (no lock file, no -wal
// or -shm) and does not migrate an older index.
func TestIndexesNeverWrites(t *testing.T) {
	data := indexesEnv(t)
	_, dir := makeIndex(t, "old", false)
	db := filepath.Join(dir, "index.db")
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	s.SetMeta("schema", "2")
	s.Close()
	before := snapshot(t, data)
	if _, err := runIndexesOut(t, false); err != nil {
		t.Fatal(err)
	}
	after := snapshot(t, data)
	if strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Errorf("listing changed files:\nbefore %q\nafter  %q", before, after)
	}
	if sum, err := store.Peek(db); err != nil || sum.Schema != "2" {
		t.Errorf("schema after listing = %q, %v", sum.Schema, err)
	}
}

func TestIndexesPrune(t *testing.T) {
	indexesEnv(t)
	_, liveDir := makeIndex(t, "live", false)
	gone, goneDir := makeIndex(t, "gone", false)
	busy, busyDir := makeIndex(t, "busy", false)
	os.RemoveAll(gone)
	os.RemoveAll(busy)
	l, _, err := lock.TryLock(filepath.Join(busyDir, "writer.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	locking := runtime.GOOS == "windows" || runtime.GOOS == "linux" || runtime.GOOS == "darwin"

	out, err := runIndexesOut(t, true)
	if err != nil {
		t.Errorf("prune: %v", err)
	}
	if _, err := os.Stat(goneDir); !os.IsNotExist(err) {
		t.Errorf("missing, unlocked index kept: %v", err)
	}
	if !strings.Contains(out, "removed "+quote(goneDir)) {
		t.Errorf("removal not printed:\n%s", out)
	}
	if _, err := os.Stat(liveDir); err != nil {
		t.Errorf("live index removed: %v", err)
	}
	if locking {
		if _, err := os.Stat(filepath.Join(busyDir, "index.db")); err != nil {
			t.Errorf("held index removed: %v", err)
		}
		if !strings.Contains(out, "kept "+quote(busyDir)+": in use by pid") {
			t.Errorf("held index not reported:\n%s", out)
		}
	}
}

func TestIndexesLegacy(t *testing.T) {
	data := indexesEnv(t)
	out, err := runIndexesOut(t, false)
	if err != nil || !strings.Contains(out, "no indexes in") || strings.Contains(out, "legacy") {
		t.Fatalf("empty: %q, %v", out, err)
	}
	legacy := filepath.Join(data, "scree", "index.db")
	mkdir(t, filepath.Dir(legacy))
	if err := os.WriteFile(legacy, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out, _ = runIndexesOut(t, true)
	if !strings.Contains(out, "legacy index "+quote(legacy)+" (from the single-folder version); delete it, or pass -db "+quote(legacy)) {
		t.Errorf("legacy line missing:\n%s", out)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Errorf("prune touched the legacy index: %v", err)
	}
}

// TestPruneIgnoresSymlink: a symlink in indexes/ that points at an index
// directory elsewhere is neither listed, followed nor removed, and a
// symlink inside a pruned index is removed without touching its target.
func TestPruneIgnoresSymlink(t *testing.T) {
	data := indexesEnv(t)
	base := filepath.Join(data, "scree", "indexes")
	outside := filepath.Join(tempDir(t), "elsewhere")
	mkdir(t, outside)
	writeRootFile(outside, filepath.Join(tempDir(t), "no-such-folder"))
	keep := filepath.Join(outside, "keep.txt")
	os.WriteFile(keep, []byte("keep"), 0o600)
	mkdir(t, base)
	if err := os.Symlink(outside, filepath.Join(base, "link-0123456789abcdef")); err != nil {
		t.Skip("no symlinks:", err)
	}
	gone, goneDir := makeIndex(t, "gone", false)
	os.RemoveAll(gone)
	target := filepath.Join(tempDir(t), "target")
	mkdir(t, target)
	os.WriteFile(filepath.Join(target, "precious"), []byte("x"), 0o600)
	os.Symlink(target, filepath.Join(goneDir, "inner"))

	out, err := runIndexesOut(t, true)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, `skipped "link-0123456789abcdef": not a directory`) {
		t.Errorf("symlink not skipped:\n%s", out)
	}
	if _, err := os.Lstat(filepath.Join(base, "link-0123456789abcdef")); err != nil {
		t.Errorf("symlink removed: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("symlink target touched: %v", err)
	}
	if _, err := os.Stat(goneDir); !os.IsNotExist(err) {
		t.Errorf("gone index kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "precious")); err != nil {
		t.Errorf("inner symlink followed: %v", err)
	}
}

// TestPruneRefusesOutsideBase: pruneIndex refuses a name that is not a
// plain directory directly inside the indexes folder.
func TestPruneRefusesOutsideBase(t *testing.T) {
	data := indexesEnv(t)
	base := filepath.Join(data, "scree", "indexes")
	mkdir(t, base)
	victim := filepath.Join(tempDir(t), "victim")
	mkdir(t, victim)
	gone := filepath.Join(tempDir(t), "gone")
	for _, name := range []string{"..", ".", "../victim", filepath.Join("a", "..", "..", "victim")} {
		if removed, err := pruneIndex(base, name, gone); removed || err == nil {
			t.Errorf("pruneIndex(%q) = %v, %v", name, removed, err)
		}
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("victim removed: %v", err)
	}
	if _, err := os.Stat(base); err != nil {
		t.Errorf("base removed: %v", err)
	}
}

func TestIndexesCapsMeta(t *testing.T) {
	indexesEnv(t)
	_, dir := makeIndex(t, "long", false)
	s, err := store.Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	s.SetMeta("model", strings.Repeat("m", 500))
	s.SetMeta("synced_at", strings.Repeat("é", 500))
	s.Close()
	out, _ := runIndexesOut(t, false)
	if strings.Contains(out, strings.Repeat("m", 81)) || strings.Contains(out, strings.Repeat("é", 81)) ||
		!strings.Contains(out, strings.Repeat("m", 79)+"…") {
		t.Errorf("meta not capped:\n%s", out)
	}
}
