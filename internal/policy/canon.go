package policy

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"runtime"
	"slices"
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
	return p.NewCanon().Canonical(rel)
}

// Canon canonicalizes many paths in one batch, such as one drain of the
// watcher's queue, listing each folder at most once (again only when a name
// that exists is missing from the listing). It is not safe for concurrent
// use; tool calls use the uncached Policy.Canonical.
type Canon struct {
	p     *Policy
	lists map[string][]string // folder -> entry names
}

// NewCanon starts a batch.
func (p *Policy) NewCanon() *Canon { return &Canon{p: p, lists: map[string][]string{}} }

// Canonical is Policy.Canonical with this batch's folder listings.
func (c *Canon) Canonical(rel string) (string, error) {
	p := c.p
	clean, err := p.Clean(rel)
	if err != nil {
		return "", err
	}
	if p.fastCanonical(clean) {
		return clean, nil
	}
	segs := strings.Split(clean, "/")
	out := make([]string, 0, len(segs))
	for i, seg := range segs {
		parent := "."
		if len(out) > 0 {
			parent = strings.Join(out, "/")
		}
		name, err := c.onDisk(parent, seg)
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

// CheckName is Policy.CheckName with this batch's listings.
func (c *Canon) CheckName(rel string) Reason {
	clean, err := c.Canonical(rel)
	if err != nil {
		var ie *IgnoredError
		if errors.As(err, &ie) {
			return ie.Reason
		}
		return Invalid
	}
	return c.p.checkName(clean)
}

// ReadStat is Policy.ReadStat with this batch's listings.
func (c *Canon) ReadStat(rel string) ([]byte, fs.FileInfo, error) {
	clean, err := c.Canonical(rel)
	if err != nil {
		return nil, nil, err
	}
	return c.p.readStat(clean)
}

// fastCanonical reports whether clean is already the on-disk spelling
// without listing any folder. It applies only on Linux with case folding
// off, where filesystems neither fold case nor normalize Unicode, and when
// clean exists. A per-folder casefold directory (ext4 chattr +F) below a
// case-sensitive root would still accept another spelling, so the leaf is
// also looked up with one letter's case swapped: if that names the same
// file, the slow path decides.
func (p *Policy) fastCanonical(clean string) bool {
	if p.opts.FoldCase || runtime.GOOS != "linux" {
		return false
	}
	fi, err := p.r.Lstat(clean)
	if err != nil {
		return false
	}
	dir, leaf := path.Split(clean)
	if swapped := swapOne(leaf); swapped != leaf {
		if sfi, err := p.r.Lstat(dir + swapped); err == nil && os.SameFile(fi, sfi) {
			return false
		}
	}
	return true
}

// swapOne swaps the case of the first letter in s that has another case.
func swapOne(s string) string {
	for i, r := range s {
		if w := swapCase(string(r)); w != string(r) {
			return s[:i] + w + s[i+len(string(r)):]
		}
	}
	return s
}

// errNoEntry means a segment could not be looked up at all.
var errNoEntry = errors.New("no entry")

// onDisk returns the name under which seg is stored in the folder parent.
// Once seg exists, failing to list parent is an error: the caller's spelling
// is never trusted.
func (c *Canon) onDisk(parent, seg string) (string, error) {
	p := c.p
	fi, err := p.r.Lstat(path.Join(parent, seg))
	if err != nil {
		return "", errNoEntry
	}
	names, cached := c.lists[parent]
	for attempt := 0; attempt < 2; attempt++ {
		if !cached || attempt == 1 {
			if names, err = p.listDir(parent); err != nil {
				return "", err
			}
			c.lists[parent] = names
		}
		if slices.Contains(names, seg) {
			return seg, nil
		}
		for _, n := range names {
			if efi, err := p.r.Lstat(path.Join(parent, n)); err == nil && os.SameFile(efi, fi) {
				return n, nil
			}
		}
		if !cached {
			break
		}
	}
	return "", ignored(seg, Invalid)
}

// listDir returns the entry names of the folder parent under the root.
func (p *Policy) listDir(parent string) ([]string, error) {
	if TestHooks.ReadDir != nil {
		TestHooks.ReadDir(parent)
	}
	f, err := p.r.Open(parent)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Readdirnames(-1)
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
