package policy

import (
	"errors"
	"os"
	"path"
	"runtime"
	"strings"
	"unicode"
)

// Canonical checks rel like Clean and rewrites each segment to its name as
// stored on disk, so a caller's spelling (another case on a case-insensitive
// filesystem, or another Unicode normalization) is never what the rules are
// matched against. Segments from the first one that cannot be looked up
// (missing, or behind a symlink leading outside the root) are kept as given;
// reading the result then reports the precise error. A name that exists
// under a spelling no entry of its folder has is refused with Invalid, and a
// result with control characters with UnsafeName.
func (p *Policy) Canonical(rel string) (string, error) {
	clean, err := p.Clean(rel)
	if err != nil {
		return "", err
	}
	segs := strings.Split(clean, "/")
	out := make([]string, 0, len(segs))
	for i, seg := range segs {
		parent := "."
		if len(out) > 0 {
			parent = strings.Join(out, "/")
		}
		name, err := p.onDisk(parent, seg)
		if errors.Is(err, errNoEntry) {
			out = append(out, segs[i:]...)
			break
		}
		if err != nil {
			var ie *IgnoredError
			if errors.As(err, &ie) {
				return "", ignored(clean, ie.Reason)
			}
			return "", readErr(clean, err)
		}
		out = append(out, name)
	}
	canon := strings.Join(out, "/")
	if unsafeName(canon) {
		return "", ignored(canon, UnsafeName)
	}
	return canon, nil
}

// errNoEntry means a segment could not be looked up at all.
var errNoEntry = errors.New("no entry")

// onDisk returns the name under which seg is stored in the folder parent.
// Once seg exists, failing to list parent is an error: the caller's spelling
// is never trusted.
func (p *Policy) onDisk(parent, seg string) (string, error) {
	fi, err := p.r.Lstat(path.Join(parent, seg))
	if err != nil {
		return "", errNoEntry
	}
	f, err := p.r.Open(parent)
	if err != nil {
		return "", err
	}
	entries, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.Name() == seg {
			return seg, nil
		}
	}
	for _, e := range entries {
		if efi, err := p.r.Lstat(path.Join(parent, e.Name())); err == nil && os.SameFile(efi, fi) {
			return e.Name(), nil
		}
	}
	return "", ignored(seg, Invalid)
}

// probeFold reports whether the filesystem under r compares names
// case-insensitively. It looks up the first root entry with a letter in
// another case. With no such entry, it assumes the platform default:
// insensitive on macOS and Windows, sensitive elsewhere.
func probeFold(r *os.Root) bool {
	if f, err := r.Open("."); err == nil {
		entries, _ := f.ReadDir(256)
		f.Close()
		for _, e := range entries {
			swapped := swapCase(e.Name())
			if swapped == e.Name() {
				continue
			}
			a, err := r.Lstat(e.Name())
			if err != nil {
				continue
			}
			b, err := r.Lstat(swapped)
			return err == nil && os.SameFile(a, b)
		}
	}
	return runtime.GOOS == "darwin" || runtime.GOOS == "windows"
}

func swapCase(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case unicode.IsUpper(r):
			return unicode.ToLower(r)
		case unicode.IsLower(r):
			return unicode.ToUpper(r)
		}
		return r
	}, s)
}

// FoldsCase reports whether the folder is on a case-insensitive filesystem,
// so ignore patterns match case-insensitively.
func (p *Policy) FoldsCase() bool { return p.opts.FoldCase }
