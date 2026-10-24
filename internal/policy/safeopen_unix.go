//go:build unix

package policy

import (
	"errors"
	"syscall"
)

const (
	// repoOpenFlags never follows a final symlink and never waits on a FIFO.
	repoOpenFlags = syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	// userOpenFlags follows symlinks but never waits on a FIFO.
	userOpenFlags = syscall.O_NONBLOCK
)

// noFollowErr maps ELOOP, which O_NOFOLLOW gives for a symlink swapped in
// after the Lstat, to errNoFollow.
func noFollowErr(err error) error {
	if errors.Is(err, syscall.ELOOP) {
		return errNoFollow
	}
	return stripPath(err)
}
