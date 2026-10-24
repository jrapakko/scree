//go:build unix && !darwin

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Where passwdHome looks; tests replace them.
var (
	passwdFile = "/etc/passwd"
	getentPath = "/usr/bin/getent"
)

// errNoPasswdEntry means the passwd source has no line for the uid.
var errNoPasswdEntry = errors.New("no passwd entry")

// passwdHome returns the current user's home directory from the password
// database, never $HOME. os/user is not used here: built with
// CGO_ENABLED=0 it parses /etc/passwd and, when the user is not in it
// (LDAP, sssd), falls back to $HOME, which a repo's .mcp.json can set. So
// /etc/passwd is read directly, and a user missing from it is looked up
// with getent (by absolute path, in an empty environment, so that dynamic
// program inherits nothing). If both fail, it fails closed.
func passwdHome() (string, error) {
	uid := os.Getuid()
	home, err := homeFromPasswd(passwdFile, uid)
	if err == nil {
		return home, nil
	}
	if !errors.Is(err, errNoPasswdEntry) && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if home, gerr := homeFromGetent(uid); gerr == nil {
		return home, nil
	}
	return "", fmt.Errorf("cannot find the home directory of uid %d in %s or with %s passwd", uid, passwdFile, getentPath)
}

// homeFromPasswd returns the home field of the first line of a passwd file
// whose uid field is uid.
func homeFromPasswd(path string, uid int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		if home, ok := passwdLineHome(sc.Text(), uid); ok {
			return home, nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	return "", errNoPasswdEntry
}

// homeFromGetent asks getent, which goes through NSS.
func homeFromGetent(uid int) (string, error) {
	if !filepath.IsAbs(getentPath) {
		return "", errors.New("getent path is not absolute")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, getentPath, "passwd", strconv.Itoa(uid))
	cmd.Env = []string{} // empty, not inherited (nil would inherit)
	cmd.Dir = "/"
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	for _, l := range strings.Split(out.String(), "\n") {
		if home, ok := passwdLineHome(l, uid); ok {
			return home, nil
		}
	}
	return "", errNoPasswdEntry
}

// passwdLineHome parses name:pw:uid:gid:gecos:home:shell and returns the
// home when the uid field is uid and the home is absolute.
func passwdLineHome(line string, uid int) (string, bool) {
	f := strings.Split(strings.TrimSuffix(line, "\r"), ":")
	if len(f) != 7 || f[0] == "" || strings.HasPrefix(f[0], "#") || strings.HasPrefix(f[0], "+") || strings.HasPrefix(f[0], "-") {
		return "", false
	}
	if n, err := strconv.Atoi(f[2]); err != nil || n != uid || f[2] != strconv.Itoa(n) {
		return "", false
	}
	if !filepath.IsAbs(f[5]) {
		return "", false
	}
	return filepath.Clean(f[5]), true
}
