package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jrapakko/scree/internal/policy"
)

// folderSource says where the indexed folder came from.
type folderSource int

const (
	flagNotes    folderSource = iota // -notes
	claudeCode                       // serve under Claude Code: the working directory, lifted to its git root
	gitRootOfCwd                     // the git root above the current directory
	cwd                              // the current directory, outside git
)

func (s folderSource) String() string {
	switch s {
	case flagNotes:
		return "-notes"
	case claudeCode:
		return "the working directory under Claude Code"
	case gitRootOfCwd:
		return "git root of the current directory"
	case cwd:
		return "current directory"
	}
	return fmt.Sprintf("folderSource(%d)", int(s))
}

// resolveFolder picks the folder to index. serve passes useProjectDir=true.
// explicit is the -notes value ("" if not given) and is used as is. A folder
// found any other way is lifted to its git root (gitRoot), and refused if it
// is the home directory, a folder above it, or a filesystem root.
//
// For serve under Claude Code the folder is the process's working
// directory, which Claude Code sets to the project (or the active
// worktree), lifted to its git root. A project's .mcp.json can set any
// environment variable, CLAUDE_PROJECT_DIR included, but not the working
// directory, so CLAUDE_PROJECT_DIR is only a cross-check: it must lead to
// the same folder, or to another work tree of the same repository.
func resolveFolder(explicit string, useProjectDir bool) (root string, src folderSource, err error) {
	if explicit != "" {
		root, err := filepath.Abs(explicit)
		return root, flagNotes, err
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", 0, err
	}
	src = cwd
	if useProjectDir {
		pd := os.Getenv("CLAUDE_PROJECT_DIR")
		if pd == "" {
			return "", 0, errors.New("no folder: run under Claude Code (CLAUDE_PROJECT_DIR) or pass -notes <dir>")
		}
		src = claudeCode
		root, _, err = liftGitRoot(wd)
		if err != nil {
			return "", 0, err
		}
		pdRoot, _, err := liftGitRoot(pd)
		if err != nil {
			return "", 0, fmt.Errorf("CLAUDE_PROJECT_DIR: %w", err)
		}
		if pdRoot != root && !sameRepo(root, pdRoot) {
			return "", 0, fmt.Errorf("CLAUDE_PROJECT_DIR %s is not in the repo of the working directory %s; not indexing", pdRoot, root)
		}
	} else {
		var top bool
		if root, top, err = liftGitRoot(wd); err != nil {
			return "", 0, err
		}
		if top {
			src = gitRootOfCwd
		}
	}
	if isHomeOrAbove(root) {
		if src == claudeCode {
			return "", 0, fmt.Errorf("%s is your home directory, a folder above it, or a filesystem root; scree does not index it from Claude Code", root)
		}
		return "", 0, fmt.Errorf("%s is your home directory, a folder above it, or a filesystem root; pass -notes %s to index it anyway", root, root)
	}
	return root, src, nil
}

// liftGitRoot resolves dir and lifts it to its git work tree's top
// (policy.FindGitTop: never in or above a sticky folder, and only through a
// .git the user owns that is not a symlink). top reports whether it was
// lifted to a work tree.
func liftGitRoot(dir string) (root string, top bool, err error) {
	if dir, err = filepath.Abs(dir); err != nil {
		return "", false, err
	}
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		return "", false, fmt.Errorf("folder: %w", err)
	}
	t, err := policy.FindGitTop(dir)
	if err != nil {
		return "", false, err
	}
	if t != "" {
		return t, true, nil
	}
	return dir, false, nil
}

// sameRepo reports whether the work trees a and b share a repository (one
// is a worktree of the other, or both are worktrees of one repo).
func sameRepo(a, b string) bool {
	ca, err := policy.CommonDir(a)
	if err != nil {
		return false
	}
	cb, err := policy.CommonDir(b)
	return err == nil && ca == cb
}

// isHomeOrAbove reports whether the resolved folder dir is a filesystem (or
// volume) root, the home directory, or a folder above it. Home is the
// password database's entry for the current user, so a HOME set by a
// project's .mcp.json cannot move it; $HOME is used only when that lookup
// fails. It is compared with its symlinks resolved.
func isHomeOrAbove(dir string) bool { return homeOrAbove(dir, lookupHome()) }

// homeOrAbove is isHomeOrAbove with the home directory given; "" means
// unknown, and only filesystem roots are refused. Besides comparing paths,
// it compares files: dir is refused if it is the same directory
// (os.SameFile) as home or any folder above home, so another path to the
// same place (a bind mount, a differently cased path on a case-insensitive
// disk, a macOS firmlink such as /System/Volumes/Data/Users/x) is refused
// too.
func homeOrAbove(dir, home string) bool {
	if filepath.Dir(dir) == dir {
		return true
	}
	if home == "" {
		return false
	}
	var err error
	if h, err := filepath.EvalSymlinks(home); err == nil {
		home = h
	}
	if home, err = filepath.Abs(home); err != nil {
		return false
	}
	if _, under := relUnder(dir, home); under {
		return true
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return false
	}
	for a := home; ; a = filepath.Dir(a) {
		if ai, err := os.Stat(a); err == nil && os.SameFile(fi, ai) {
			return true
		}
		if filepath.Dir(a) == a {
			return false
		}
	}
}

// lookupHome returns the current user's home directory from the password
// database (passwdHome), or $HOME when that lookup fails. Tests replace it.
var lookupHome = func() string {
	if home, err := passwdHome(); err == nil {
		return home
	}
	home, _ := os.UserHomeDir()
	return home
}

// relUnder reports whether path is dir or inside it.
func relUnder(dir, path string) (string, bool) {
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// dataHome returns $XDG_DATA_HOME, or ~/.local/share when it is unset. A
// relative $XDG_DATA_HOME is ignored, as the XDG spec says. With neither it
// nor an absolute home directory known, it fails, rather than putting the
// indexes under the working directory, which may be the indexed folder.
func dataHome() (string, error) {
	if base := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(base) {
		return base, nil
	}
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return "", errors.New("no folder for the indexes: set HOME (or XDG_DATA_HOME) to an absolute path, or pass -db")
	}
	return filepath.Join(home, ".local", "share"), nil
}

// indexDir returns $XDG_DATA_HOME/scree/indexes/<base>-<hash16> for a
// resolved root. hash16 is the first 16 hex digits of sha256(root); base is
// the folder's name made safe, so stale entries are recognisable. It fails
// as dataHome does.
func indexDir(resolvedRoot string) (string, error) {
	data, err := dataHome()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(resolvedRoot))
	return filepath.Join(data, "scree", "indexes", safeBase(filepath.Base(resolvedRoot))+"-"+hex.EncodeToString(sum[:])[:16]), nil
}

// checkIndexDirs refuses an index directory, or the scree or indexes folder
// above it under the data directory, that exists as a symlink or as
// anything but a folder, so the index and its lock are never created
// somewhere a link points. The data directory itself may be a symlink.
func checkIndexDirs(dir string) error {
	indexes := filepath.Dir(dir)
	for _, d := range []string{filepath.Dir(indexes), indexes, dir} {
		fi, err := os.Lstat(d)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("index folder: %w", err)
		}
		if !fi.IsDir() {
			return fmt.Errorf("index folder %s is a symlink or not a folder; not using it", d)
		}
	}
	return nil
}

// safeBase replaces every rune outside [A-Za-z0-9._-] with "_", cuts the
// result to 40 bytes, and replaces a leading "." with "_".
func safeBase(name string) string {
	b := []byte(strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			return r
		}
		return '_'
	}, name))
	if len(b) > 40 {
		b = b[:40]
	}
	if len(b) > 0 && b[0] == '.' {
		b[0] = '_'
	}
	return string(b)
}

// writeRootFile records the indexed folder in <dir>/root, so `scree indexes`
// can map a hashed directory back to it. An existing file is left alone.
func writeRootFile(dir, root string) error {
	f, err := os.OpenFile(filepath.Join(dir, "root"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := f.WriteString(root + "\n"); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
