//go:build unix

package main

import (
	"errors"
	"io/fs"
	"syscall"
)

// checkSettingsOwner refuses a settings file not owned by launchUID.
func checkSettingsOwner(fi fs.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("has no known owner")
	}
	if int64(st.Uid) != int64(launchUID()) {
		return errors.New("is not owned by you")
	}
	return nil
}
