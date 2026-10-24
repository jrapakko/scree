//go:build windows

package lock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockOffsetHigh puts the locked byte at offset 2^62, past any pid text, so
// readers are never blocked by the mandatory range lock.
const lockOffsetHigh = 1 << 30

func tryLock(f *os.File) (held bool, err error) {
	ol := windows.Overlapped{OffsetHigh: lockOffsetHigh}
	err = windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return true, nil
	}
	return false, err
}

func unlock(f *os.File) {
	ol := windows.Overlapped{OffsetHigh: lockOffsetHigh}
	windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ol)
}

// noFollow is 0: Windows has no O_NOFOLLOW; notSymlink checks first.
const noFollow = 0
