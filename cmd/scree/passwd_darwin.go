package main

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
)

// passwdHome returns the current user's home directory from the user
// database, never $HOME. On macOS os/user calls getpwuid_r in libSystem
// even when built with CGO_ENABLED=0 (so Open Directory users resolve);
// only the osusergo build tag would make it parse /etc/passwd and fall
// back to $HOME, and passwd_osusergo_darwin.go refuses to build with it.
func passwdHome() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("cannot look up your home directory in the user database: %w", err)
	}
	if u.Uid != strconv.Itoa(os.Getuid()) || !filepath.IsAbs(u.HomeDir) {
		return "", fmt.Errorf("the user database has no usable home directory for uid %d", os.Getuid())
	}
	return filepath.Clean(u.HomeDir), nil
}
