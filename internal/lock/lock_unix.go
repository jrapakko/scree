//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package lock

import (
	"errors"
	"os"
	"syscall"
)

func tryLock(f *os.File) (held bool, err error) {
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true, nil
	}
	return false, err
}

func unlock(f *os.File) { syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }

// noFollow makes an open fail on a symlink.
const noFollow = syscall.O_NOFOLLOW
