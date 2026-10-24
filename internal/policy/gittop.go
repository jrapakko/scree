package policy

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ownerOf and myUID are how trust checks learn who owns a file and who we
// are. Tests replace them. A result of -1 means unknown, and skips the
// ownership check (Windows and other non-unix systems).
var (
	ownerOf = fileOwner
	myUID   = currentUID
)

// ErrUntrustedGit means a .git, or the git dir it names, is a symlink, not a
// folder or file, or owned by another user.
var ErrUntrustedGit = errors.New("owned by another user (or is a symlink)")

// FindGitTop walks up from the resolved folder dir to the nearest folder
// holding a .git entry, and returns "" when there is none. It stops, without
// looking further, at a folder with the sticky bit set, such as /tmp: a .git
// in or above a shared folder could have been planted by anyone. A .git that
// is a symlink, neither a folder nor a regular file, or owned by another
// user is an error, not a skip, so a folder is never indexed without its
// repo's ignore rules.
func FindGitTop(dir string) (string, error) {
	for d := dir; ; {
		fi, err := os.Stat(d)
		if err != nil {
			return "", fmt.Errorf("folder %s: %w", d, stripPath(err))
		}
		if fi.Mode()&fs.ModeSticky != 0 {
			return "", nil
		}
		gfi, err := os.Lstat(filepath.Join(d, ".git"))
		switch {
		case err == nil:
			if !trusted(gfi, true) {
				return "", untrustedTop(d)
			}
			return d, nil
		case !errors.Is(err, fs.ErrNotExist):
			return "", fmt.Errorf("git work tree at %s: %w", d, stripPath(err))
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", nil
		}
		d = parent
	}
}

// CommonDir returns the git common dir of the work tree whose top is top:
// its .git folder, or for a worktree the repository's, as named by the .git
// file and commondir under the same trust rules as the git layer. Two work
// trees of one repository have the same common dir.
func CommonDir(top string) (string, error) {
	g, err := newRepo(top, top, filepath.Join(top, ".git"))
	if err != nil {
		return "", err
	}
	c := filepath.Clean(g.common)
	if r, err := filepath.EvalSymlinks(c); err == nil {
		c = r
	}
	return c, nil
}

func untrustedTop(top string) error {
	return fmt.Errorf("git work tree at %s is %w; not indexing it. "+
		"To index a folder below it anyway, pass -notes <folder> -gitignore=false on the command line", top, ErrUntrustedGit)
}

// trusted reports whether fi, from Lstat, is not a symlink, is a folder (or,
// with fileOK, a regular file), and is owned by the current user when
// ownership is known.
func trusted(fi fs.FileInfo, fileOK bool) bool {
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		return false
	case fi.IsDir():
	case fileOK && fi.Mode().IsRegular():
	default:
		return false
	}
	if uid, own := myUID(), ownerOf(fi); uid >= 0 && own >= 0 && uid != own {
		return false
	}
	return true
}

// checkGitDir applies the trust rule to a git dir or common dir named by a
// .git file or commondir: an existing folder, not a symlink, owned by the
// current user. It has no sticky-folder check, since worktree storage
// commonly lives elsewhere.
func checkGitDir(dir, namedBy string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("the git dir named by %s: %w", namedBy, stripPath(err))
	}
	if !trusted(fi, false) {
		return fmt.Errorf("the git dir named by %s is not a folder, is a symlink, or is owned by another user", namedBy)
	}
	return nil
}
