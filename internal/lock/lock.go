// Package lock takes the per-index writer lock, so only one process writes
// to an index at a time.
//
// The lock file holds the holder's pid on one line. On Unix the whole file is
// locked with flock; on Windows LockFileEx locks one byte far past the end of
// the file, so the pid stays readable by others. Elsewhere there is no lock
// and TryLock always succeeds.
package lock

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ErrHeld means another process holds the lock.
var ErrHeld = errors.New("held by another process")

// Lock is a held writer lock. Release it, or exit, to let another process
// take it.
type Lock struct{ f *os.File }

// TryLock opens path (0600, created if missing) and takes an exclusive,
// non-blocking lock. When another process holds it, it returns ErrHeld and
// the pid recorded in the file (0 if unknown). The lock is per open file, so
// a second TryLock in the same process also gets ErrHeld.
func TryLock(path string) (*Lock, int, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, 0, fmt.Errorf("lock: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return nil, 0, fmt.Errorf("lock: %w", err)
	}
	held, err := tryLock(f)
	if err != nil {
		f.Close()
		return nil, 0, fmt.Errorf("lock %s: %w", path, err)
	}
	if held {
		f.Close()
		return nil, HolderPID(path), ErrHeld
	}
	if err := writePID(f); err != nil {
		unlock(f)
		f.Close()
		return nil, 0, fmt.Errorf("lock %s: %w", path, err)
	}
	return &Lock{f: f}, os.Getpid(), nil
}

func writePID(f *os.File) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	_, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	return err
}

// Probe reports whether another process holds the lock at path, and its pid,
// without creating or writing the file. A missing file means not held.
func Probe(path string) (held bool, pid int, err error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, fmt.Errorf("lock: %w", err)
	}
	defer f.Close()
	held, err = tryLock(f)
	if err != nil {
		return false, 0, fmt.Errorf("lock %s: %w", path, err)
	}
	if held {
		return true, HolderPID(path), nil
	}
	unlock(f)
	return false, 0, nil
}

// HolderPID returns the pid recorded in the lock file at path, or 0.
func HolderPID(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// Release gives up the lock. The file stays, with the old pid in it.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	unlock(l.f)
	err := l.f.Close()
	l.f = nil
	return err
}
