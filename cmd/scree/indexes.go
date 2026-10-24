package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jrapakko/scree/internal/lock"
	"github.com/jrapakko/scree/internal/store"
	"github.com/jrapakko/scree/internal/textsafe"
)

// indexesBase is the folder holding one directory per indexed folder.
func indexesBase() string { return filepath.Join(dataHome(), "scree", "indexes") }

// indexInfo is what `scree indexes` learned about one index directory.
type indexInfo struct {
	name    string // directory name under indexesBase
	root    string // the indexed folder, from the root file or meta; "" if unknown
	sum     store.Summary
	peekErr error
	size    int64 // bytes of the regular files in the directory
	missing bool  // root no longer exists
	rootErr error // root could not be checked (other than not existing)
	held    bool
	pid     int
	lockErr error
}

// runIndexes lists every index under indexesBase and, with -prune, removes
// those whose folder is gone. Listing only reads: each index is read with
// store.Peek, which never runs init or migrates, and the writer lock is
// checked with lock.Probe, which never creates or writes the lock file.
func runIndexes(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("indexes", flag.ExitOnError)
	prune := fs.Bool("prune", false, "delete indexes whose folder no longer exists")
	fs.Parse(args)
	if fs.NArg() != 0 {
		return errors.New("usage: scree indexes [-prune]")
	}
	return indexes(os.Stdout, *prune)
}

func indexes(w io.Writer, prune bool) error {
	base := indexesBase()
	if legacy := filepath.Join(dataHome(), "scree", "index.db"); fileExists(legacy) {
		fmt.Fprintf(w, "legacy index %s (from the single-folder version); delete it, or pass -db %s to keep using it\n",
			quote(legacy), quote(legacy))
	}
	entries, err := os.ReadDir(base)
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(w, "no indexes in %s\n", quote(base))
		return nil
	}
	if err != nil {
		return fmt.Errorf("indexes: %w", err)
	}
	var infos []indexInfo
	for _, e := range entries {
		fi, err := os.Lstat(filepath.Join(base, e.Name()))
		if err != nil {
			fmt.Fprintf(w, "skipped %s: %v\n", quote(e.Name()), err)
			continue
		}
		if !fi.IsDir() {
			// A symlink or file is never followed, listed as an index or
			// removed.
			fmt.Fprintf(w, "skipped %s: not a directory\n", quote(e.Name()))
			continue
		}
		infos = append(infos, inspect(base, e.Name()))
	}
	if len(infos) == 0 {
		fmt.Fprintf(w, "no indexes in %s\n", quote(base))
	}
	for _, in := range infos {
		fmt.Fprintln(w, in.line())
	}
	if !prune {
		return nil
	}
	failed := 0
	for _, in := range infos {
		if !in.missing {
			continue
		}
		dir := filepath.Join(base, in.name)
		switch removed, err := pruneIndex(base, in.name, in.root); {
		case errors.Is(err, lock.ErrHeld):
			// A writer still runs on it; not an error, it can go later.
			fmt.Fprintf(w, "kept %s: %v\n", quote(dir), textsafe.Strip(err.Error()))
		case err != nil:
			failed++
			fmt.Fprintf(w, "not removed %s: %v\n", quote(dir), textsafe.Strip(err.Error()))
		case removed:
			fmt.Fprintf(w, "removed %s (folder %s is gone)\n", quote(dir), quote(in.root))
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d indexes could not be removed", failed)
	}
	return nil
}

// inspect reads one index directory without writing to it.
func inspect(base, name string) indexInfo {
	dir := filepath.Join(base, name)
	in := indexInfo{name: name}
	in.root = readRootFile(dir)
	db := filepath.Join(dir, "index.db")
	if fileExists(db) {
		in.sum, in.peekErr = store.Peek(db)
		if in.root == "" {
			in.root = in.sum.Root
		}
	} else {
		in.peekErr = errors.New("no index.db")
	}
	if ents, err := os.ReadDir(dir); err == nil {
		for _, e := range ents {
			if fi, err := os.Lstat(filepath.Join(dir, e.Name())); err == nil && fi.Mode().IsRegular() {
				in.size += fi.Size()
			}
		}
	}
	if in.root != "" {
		_, err := os.Stat(in.root)
		in.missing = errors.Is(err, fs.ErrNotExist)
		if err != nil && !in.missing {
			in.rootErr = err
		}
	}
	in.held, in.pid, in.lockErr = lock.Probe(filepath.Join(dir, "writer.lock"))
	return in
}

// line renders one index for the listing.
func (in indexInfo) line() string {
	var b strings.Builder
	if in.root != "" {
		b.WriteString(quote(capRunes(in.root)))
	} else {
		b.WriteString("(folder unknown)")
	}
	if in.peekErr != nil {
		fmt.Fprintf(&b, "  unreadable: %s", capRunes(textsafe.Strip(in.peekErr.Error())))
	} else {
		fmt.Fprintf(&b, "  %d files, %d chunks", in.sum.Files, in.sum.Chunks)
	}
	fmt.Fprintf(&b, "  %s", humanBytes(in.size))
	synced := in.sum.SyncedAt
	if synced == "" {
		synced = "never"
	}
	fmt.Fprintf(&b, "  last sync %s", capRunes(textsafe.Strip(synced)))
	if in.sum.Model != "" {
		fmt.Fprintf(&b, "  model %s", capRunes(textsafe.Strip(in.sum.Model)))
	}
	if in.missing {
		b.WriteString("  [missing]")
	}
	if in.rootErr != nil {
		b.WriteString("  [folder unreadable]")
	}
	switch {
	case in.lockErr != nil:
		b.WriteString("  [lock unreadable]")
	case in.held && in.pid > 0:
		fmt.Fprintf(&b, "  [in use by pid %d]", in.pid)
	case in.held:
		b.WriteString("  [in use]")
	}
	fmt.Fprintf(&b, "  (%s)", quote(in.name))
	return b.String()
}

// pruneIndex removes the index directory name under base if its folder
// root is still gone and no writer holds it. It works on the resolved base
// and refuses a directory that is a symlink or resolves anywhere other
// than directly inside it; os.RemoveAll removes symlinks inside it without
// following them. The writer lock is held while the contents are removed,
// so no writer can start on it meanwhile, then released, and the lock file
// and directory removed last (Windows cannot delete an open file).
func pruneIndex(base, name, root string) (bool, error) {
	if root == "" {
		return false, errors.New("folder unknown")
	}
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return false, err
	}
	dir := filepath.Join(realBase, name)
	if filepath.Dir(dir) != realBase || name == "." || name == ".." {
		return false, errors.New("not directly inside the indexes folder")
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return false, err
	}
	if !fi.IsDir() {
		return false, errors.New("not a directory")
	}
	if resolved, err := filepath.EvalSymlinks(dir); err != nil || resolved != dir {
		return false, fmt.Errorf("resolves outside the indexes folder (%s)", quote(resolved))
	}
	if _, err := os.Stat(root); !errors.Is(err, fs.ErrNotExist) {
		return false, errors.New("the folder exists again")
	}
	lockFile := filepath.Join(dir, "writer.lock")
	l, pid, err := lock.TryLock(lockFile)
	if errors.Is(err, lock.ErrHeld) {
		return false, fmt.Errorf("in use by pid %d (%w)", pid, lock.ErrHeld)
	}
	if err != nil {
		return false, err
	}
	// The directory must still be the one checked above, not one swapped in
	// meanwhile (or a symlink).
	if now, err := os.Lstat(dir); err != nil || !os.SameFile(fi, now) {
		l.Release()
		return false, errors.New("changed while pruning")
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		l.Release()
		return false, err
	}
	for _, e := range ents {
		if e.Name() == "writer.lock" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			l.Release()
			return false, err
		}
	}
	l.Release()
	if err := os.Remove(lockFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	return true, os.Remove(dir)
}

// readRootFile returns the first line of dir/root, or "" if it is missing,
// not a regular file, or unreadable. At most 4 KB is read.
func readRootFile(dir string) string {
	p := filepath.Join(dir, "root")
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	line, _ := bufio.NewReader(io.LimitReader(f, 4096)).ReadString('\n')
	return strings.TrimRight(line, "\r\n")
}

func fileExists(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode().IsRegular()
}

// quote quotes a path for terminal output, with control characters removed.
func quote(s string) string { return strconv.Quote(textsafe.Strip(s)) }

// maxMetaRunes caps each value `scree indexes` prints from an index's root
// file or meta table, which anything that wrote the index could fill.
const maxMetaRunes = 80

// capRunes cuts s to maxMetaRunes runes, marking a cut with "…".
func capRunes(s string) string {
	r := []rune(s)
	if len(r) <= maxMetaRunes {
		return s
	}
	return string(r[:maxMetaRunes-1]) + "…"
}

// humanBytes formats n as B, KB or MB.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
