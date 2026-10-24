//go:build !unix

package main

import (
	"errors"
	"os/user"
)

// passwdHome returns the profile directory Windows records for the current
// user (GetUserProfileDirectory, not %USERPROFILE%). Only the home guard
// uses it there; scree mcp and scree run refuse to run on Windows.
func passwdHome() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	if u.HomeDir == "" {
		return "", errors.New("no home directory for the current user")
	}
	return u.HomeDir, nil
}
