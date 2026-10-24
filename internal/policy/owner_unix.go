//go:build unix

package policy

import (
	"io/fs"
	"os"
	"syscall"
)

// fileOwner returns the uid that owns fi, or -1 if it is unknown.
func fileOwner(fi fs.FileInfo) int {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid)
	}
	return -1
}

func currentUID() int { return os.Getuid() }
