//go:build unix

package main

import "syscall"

// The index and its -wal and -shm files hold note text. The SQLite VFS
// creates the side files with 0666 masked by the umask, so tighten the umask
// for everything scree creates.
func init() { syscall.Umask(0o077) }
