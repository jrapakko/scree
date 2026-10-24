//go:build unix

package main

import "syscall"

// noFollow makes an open fail on a symlink.
const noFollow = syscall.O_NOFOLLOW
