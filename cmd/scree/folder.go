package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// folderSource says where the indexed folder came from.
type folderSource int

const (
	flagNotes    folderSource = iota // -notes
	projectDir                       // CLAUDE_PROJECT_DIR, lifted to its git root
	gitRootOfCwd                     // the git root above the current directory
	cwd                              // the current directory, outside git
)

func (s folderSource) String() string {
	switch s {
	case flagNotes:
		return "-notes"
	case projectDir:
		return "CLAUDE_PROJECT_DIR"
	case gitRootOfCwd:
		return "git root of the current directory"
	case cwd:
		return "current directory"
	}
	return fmt.Sprintf("folderSource(%d)", int(s))
}

// resolveFolder picks the folder to index. serve passes useProjectDir=true.
// explicit is the -notes value ("" if not given) and is used as is. A folder
// found any other way is lifted to its git root, and refused if it is the
// home directory or a filesystem root.
func resolveFolder(explicit string, useProjectDir bool) (root string, src folderSource, err error) {
	if explicit != "" {
		root, err := filepath.Abs(explicit)
		return root, flagNotes, err
	}
	var base string
	if useProjectDir {
		base = os.Getenv("CLAUDE_PROJECT_DIR")
		if base == "" {
			return "", 0, errors.New("no folder: run under Claude Code (CLAUDE_PROJECT_DIR) or pass -notes <dir>")
		}
		src = projectDir
	} else {
		if base, err = os.Getwd(); err != nil {
			return "", 0, err
		}
		src = cwd
	}
	if base, err = filepath.Abs(base); err != nil {
		return "", 0, err
	}
	if base, err = filepath.EvalSymlinks(base); err != nil {
		return "", 0, fmt.Errorf("folder: %w", err)
	}
	root = base
	if top := gitRoot(base); top != "" {
		root = top
		if src == cwd {
			src = gitRootOfCwd
		}
	}
	if isHomeOrFSRoot(root) {
		return "", 0, fmt.Errorf("%s is your home directory (or a filesystem root); pass -notes %s to index it anyway", root, root)
	}
	return root, src, nil
}

// isHomeOrFSRoot reports whether the resolved folder dir is the resolved home
// directory or a filesystem root.
func isHomeOrFSRoot(dir string) bool {
	if filepath.Dir(dir) == dir {
		return true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	if h, err := filepath.EvalSymlinks(home); err == nil {
		home = h
	}
	return dir == home
}

// gitRoot walks up from dir to the nearest folder containing a ".git"
// entry; "" if none.
func gitRoot(dir string) string {
	for {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// dataHome returns $XDG_DATA_HOME, or ~/.local/share when it is unset.
func dataHome() string {
	if base := os.Getenv("XDG_DATA_HOME"); base != "" {
		return base
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share")
}

// indexDir returns $XDG_DATA_HOME/scree/indexes/<base>-<hash16> for a
// resolved root. hash16 is the first 16 hex digits of sha256(root); base is
// the folder's name made safe, so stale entries are recognisable.
func indexDir(resolvedRoot string) string {
	sum := sha256.Sum256([]byte(resolvedRoot))
	return filepath.Join(dataHome(), "scree", "indexes", safeBase(filepath.Base(resolvedRoot))+"-"+hex.EncodeToString(sum[:])[:16])
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
