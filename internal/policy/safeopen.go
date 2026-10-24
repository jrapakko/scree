package policy

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

// errNoFollow refuses a repo-controlled ignore or git file that is a
// symlink, FIFO, device or folder. The message names no content.
var errNoFollow = errors.New("not a regular file (symlinks are not followed)")

// Two classes of file feed the ignore rules. Repo-controlled files
// (.gitignore, .git when it is a file, commondir, info/exclude, .screeignore)
// come with the folder being indexed, so a symlink there could make scree
// parse, and report on, any file the user can read: they are never followed
// through a symlink. User-controlled files (the user's git config and the
// global excludes file it names) are followed, as git does, since dotfile
// managers link them. Both are opened without blocking, must be regular
// files, and are capped at maxGitFileBytes.

// statRepoFile stamps a repo-controlled file outside the root without
// following a symlink. A missing file gives a missing stamp.
func statRepoFile(name string) (fileStamp, error) {
	fi, err := os.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return fileStamp{ok: true, missing: true}, nil
	}
	if err != nil {
		return fileStamp{}, fmt.Errorf("read %s: %w", name, stripPath(err))
	}
	if !fi.Mode().IsRegular() {
		return fileStamp{}, fmt.Errorf("read %s: %w", name, errNoFollow)
	}
	return stampOf(fi), nil
}

// readRepoFile reads a repo-controlled file outside the root: never through
// a symlink, only a regular file, never blocking. A missing file gives a
// missing stamp and no data.
func readRepoFile(name string) ([]byte, fileStamp, error) {
	lfi, err := os.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fileStamp{ok: true, missing: true}, nil
	}
	if err != nil {
		return nil, fileStamp{}, fmt.Errorf("read %s: %w", name, stripPath(err))
	}
	if !lfi.Mode().IsRegular() {
		return nil, fileStamp{}, fmt.Errorf("read %s: %w", name, errNoFollow)
	}
	f, err := os.OpenFile(name, os.O_RDONLY|repoOpenFlags, 0)
	if err != nil {
		return nil, fileStamp{}, fmt.Errorf("read %s: %w", name, noFollowErr(err))
	}
	defer f.Close()
	data, st, err := readOpened(f, lfi)
	if err != nil {
		return nil, fileStamp{}, fmt.Errorf("read %s: %w", name, err)
	}
	return data, st, nil
}

// statUserFile stamps a user-controlled file, following symlinks. A missing
// file gives a missing stamp; a dangling symlink is an error.
func statUserFile(name string) (fileStamp, error) {
	fi, err := os.Stat(name)
	if errors.Is(err, fs.ErrNotExist) {
		if _, lerr := os.Lstat(name); errors.Is(lerr, fs.ErrNotExist) {
			return fileStamp{ok: true, missing: true}, nil
		}
		return fileStamp{}, fmt.Errorf("read %s: dangling symlink", name)
	}
	if err != nil {
		return fileStamp{}, fmt.Errorf("read %s: %w", name, stripPath(err))
	}
	if !fi.Mode().IsRegular() {
		return fileStamp{}, fmt.Errorf("read %s: %w", name, ErrNotRegular)
	}
	return stampOf(fi), nil
}

// readUserFile reads a file named by the user's own config: symlinks are
// followed (dotfile managers), but it must be a regular file, and opening it
// never blocks. A missing file gives a missing stamp and no data.
func readUserFile(name string) ([]byte, fileStamp, error) {
	st, err := statUserFile(name)
	if err != nil || st.missing {
		return nil, st, err
	}
	f, err := os.OpenFile(name, os.O_RDONLY|userOpenFlags, 0)
	if err != nil {
		return nil, fileStamp{}, fmt.Errorf("read %s: %w", name, stripPath(err))
	}
	defer f.Close()
	data, st, err := readOpened(f, nil)
	if err != nil {
		return nil, fileStamp{}, fmt.Errorf("read %s: %w", name, err)
	}
	return data, st, nil
}

// statRootRepoFile is statRepoFile for a file under the root.
func (p *Policy) statRootRepoFile(rel string) (fileStamp, error) {
	fi, err := p.r.Lstat(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return fileStamp{ok: true, missing: true}, nil
	}
	if err != nil {
		return fileStamp{}, stripPath(err)
	}
	if !fi.Mode().IsRegular() {
		return fileStamp{}, errNoFollow
	}
	return stampOf(fi), nil
}

// readRootRepoFile is readRepoFile for a file under the root, read through
// the root handle. os.Root follows a symlink in the last component even with
// O_NOFOLLOW, so the Lstat and the SameFile check on the opened file are
// what refuse one, including one swapped in after the Lstat.
func (p *Policy) readRootRepoFile(rel string) ([]byte, fileStamp, error) {
	lfi, err := p.r.Lstat(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fileStamp{ok: true, missing: true}, nil
	}
	if err != nil {
		return nil, fileStamp{}, stripPath(err)
	}
	if !lfi.Mode().IsRegular() {
		return nil, fileStamp{}, errNoFollow
	}
	f, err := p.r.OpenFile(rel, os.O_RDONLY|repoOpenFlags, 0)
	if err != nil {
		return nil, fileStamp{}, noFollowErr(err)
	}
	defer f.Close()
	return readOpened(f, lfi)
}

// readOpened checks that f is a regular file (and, with lfi set, the file
// Lstat saw) and reads at most maxGitFileBytes of it.
func readOpened(f *os.File, lfi fs.FileInfo) ([]byte, fileStamp, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, fileStamp{}, stripPath(err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fileStamp{}, ErrNotRegular
	}
	if lfi != nil && !os.SameFile(lfi, fi) {
		return nil, fileStamp{}, errNoFollow
	}
	data, err := io.ReadAll(io.LimitReader(f, maxGitFileBytes+1))
	if err != nil {
		return nil, fileStamp{}, stripPath(err)
	}
	if len(data) > maxGitFileBytes {
		return nil, fileStamp{}, fmt.Errorf("larger than %d bytes", maxGitFileBytes)
	}
	return data, stampOf(fi), nil
}

func stampOf(fi fs.FileInfo) fileStamp {
	return fileStamp{ok: true, mod: fi.ModTime().UnixNano(), size: fi.Size()}
}
