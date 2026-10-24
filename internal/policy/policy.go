// Package policy decides which files under the notes folder scree may read.
// Every note read goes through a Policy, so indexing, search, read_note and
// reindex all apply the same built-in rules, .screeignore patterns and
// front-matter opt-outs.
package policy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/jrapakko/scree/internal/ignore"
)

// DefaultMaxBytes is the largest note scree reads unless configured otherwise.
const DefaultMaxBytes int64 = 1 << 20

// IgnoreFileName is the ignore file read from the root of the notes folder.
const IgnoreFileName = ".screeignore"

// maxIgnoreBytes caps the size of .screeignore.
const maxIgnoreBytes = 256 << 10

// Reason says why a path may not be read.
type Reason int

// Reasons, in the order they are checked.
const (
	Allowed Reason = iota
	Invalid
	UnsafeName
	Hidden
	NotMarkdown
	OutsideRoot
	TooLarge
	IgnoreFile
	FrontMatter
)

// String describes the reason in a few words.
func (r Reason) String() string {
	switch r {
	case Allowed:
		return "allowed"
	case Invalid:
		return "invalid path"
	case UnsafeName:
		return "unsafe name"
	case Hidden:
		return "hidden"
	case NotMarkdown:
		return "not markdown"
	case OutsideRoot:
		return "outside notes folder"
	case TooLarge:
		return "too large"
	case IgnoreFile:
		return ".screeignore"
	case FrontMatter:
		return "front matter"
	}
	return fmt.Sprintf("Reason(%d)", int(r))
}

// Layer names the ignore layer the reason belongs to: "built-in",
// ".screeignore" or "front matter". It is empty for Allowed.
func (r Reason) Layer() string {
	switch r {
	case Allowed:
		return ""
	case IgnoreFile:
		return ".screeignore"
	case FrontMatter:
		return "front matter"
	}
	return "built-in"
}

// ErrNotRegular means a path names a FIFO, device or other non-regular
// file. Such files are never opened.
var ErrNotRegular = errors.New("not a regular file")

// readErr wraps a read failure with the quoted store key. It drops the
// absolute path a *fs.PathError carries, so messages show only rel.
func readErr(rel string, err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	return fmt.Errorf("read %q: %w", rel, err)
}

// ErrIgnored matches every *IgnoredError with errors.Is.
var ErrIgnored = errors.New("ignored")

// IgnoredError reports a path the policy refuses to read.
type IgnoredError struct {
	Path   string
	Reason Reason
}

func (e *IgnoredError) Error() string { return fmt.Sprintf("%q is ignored (%s)", e.Path, e.Reason) }

// Is makes errors.Is(err, ErrIgnored) true.
func (e *IgnoredError) Is(target error) bool { return target == ErrIgnored }

func ignored(rel string, r Reason) error { return &IgnoredError{Path: rel, Reason: r} }

type fileStamp struct {
	ok      bool // a stamp was taken
	missing bool
	mod     int64 // modification time in nanoseconds
	size    int64
}

// Policy reads notes under one root folder. It is safe for concurrent use.
type Policy struct {
	given string // the configured root, made absolute
	root  string // the root with symlinks resolved
	r     *os.Root
	max   int64

	mu      sync.Mutex
	m       *ignore.Matcher
	stamp   fileStamp
	loadErr error
}

// Open opens the notes folder at root. Notes larger than maxBytes are
// refused; maxBytes <= 0 means DefaultMaxBytes.
func Open(root string, maxBytes int64) (*Policy, error) {
	given, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("notes folder: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(given)
	if err != nil {
		return nil, fmt.Errorf("notes folder: %w", err)
	}
	r, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, fmt.Errorf("notes folder: %w", err)
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	return &Policy{given: given, root: resolved, r: r, max: maxBytes}, nil
}

// Close releases the root folder handle.
func (p *Policy) Close() error { return p.r.Close() }

// Root returns the notes folder with symlinks resolved.
func (p *Policy) Root() string { return p.root }

// Clean checks a slash-separated store key and returns it in canonical form.
// It refuses absolute paths, paths that are not already clean, paths that
// climb out of the root, and names with control characters or invalid UTF-8.
// Refusals are *IgnoredError with reason Invalid or UnsafeName.
func (p *Policy) Clean(rel string) (string, error) {
	slash := filepath.ToSlash(rel)
	clean := path.Clean("/" + slash)[1:]
	if clean == "" || clean != strings.TrimPrefix(slash, "./") {
		return "", ignored(rel, Invalid)
	}
	if unsafeName(clean) {
		return "", ignored(clean, UnsafeName)
	}
	return clean, nil
}

// Rel converts an absolute path under the root to a slash-separated store
// key. It accepts paths under either the resolved or the configured root.
func (p *Policy) Rel(abs string) (string, error) {
	for _, root := range []string{p.root, p.given} {
		if rel, ok := relUnder(root, abs); ok {
			return rel, nil
		}
	}
	return "", fmt.Errorf("%q is outside %s", abs, p.given)
}

func relUnder(root, abs string) (string, bool) {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return rel, true
}

// CheckName applies the rules that need no file access: a valid, safe name,
// no hidden segment, a markdown extension, and the most recently loaded
// .screeignore patterns. It returns Allowed when none of them refuse rel. A
// file that passes may still be refused by Read.
func (p *Policy) CheckName(rel string) Reason {
	clean, err := p.Clean(rel)
	if err != nil {
		var ie *IgnoredError
		errors.As(err, &ie)
		return ie.Reason
	}
	if r := nameReason(clean); r != Allowed {
		return r
	}
	p.mu.Lock()
	m := p.m
	p.mu.Unlock()
	if m.Match(clean, false) {
		return IgnoreFile
	}
	return Allowed
}

// Read returns the content of the note rel if the policy allows it. Refusals
// are *IgnoredError; a missing file gives an error wrapping fs.ErrNotExist.
// Ignored files are never opened.
func (p *Policy) Read(rel string) ([]byte, error) {
	clean, err := p.Clean(rel)
	if err != nil {
		return nil, err
	}
	if r := nameReason(clean); r != Allowed {
		return nil, ignored(clean, r)
	}
	m, err := p.matcher()
	if err != nil {
		return nil, err
	}
	return p.read(clean, true, m)
}

// read runs the checks after the name rules on rel. With resolve false, rel
// is known to contain no symlinks.
func (p *Policy) read(rel string, resolve bool, m *ignore.Matcher) ([]byte, error) {
	realRel := rel
	if resolve {
		real, err := filepath.EvalSymlinks(filepath.Join(p.root, filepath.FromSlash(rel)))
		if err != nil {
			return nil, readErr(rel, err)
		}
		r, ok := relUnder(p.root, real)
		if !ok {
			return nil, ignored(rel, OutsideRoot)
		}
		if realRel = r; realRel != rel {
			if unsafeName(realRel) {
				return nil, ignored(rel, UnsafeName)
			}
			if r := nameReason(realRel); r != Allowed {
				return nil, ignored(rel, r)
			}
		}
	}
	fi, err := p.r.Stat(realRel)
	if err != nil {
		return nil, readErr(rel, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, readErr(rel, ErrNotRegular)
	}
	if fi.Size() > p.max {
		return nil, ignored(rel, TooLarge)
	}
	if m.Match(rel, false) || m.Match(realRel, false) {
		return nil, ignored(rel, IgnoreFile)
	}

	f, err := p.r.Open(realRel)
	if err != nil {
		return nil, readErr(rel, err)
	}
	defer f.Close()
	if fi, err = f.Stat(); err != nil {
		return nil, readErr(rel, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, readErr(rel, ErrNotRegular)
	}
	src, err := io.ReadAll(io.LimitReader(f, p.max+1))
	if err != nil {
		return nil, readErr(rel, err)
	}
	if int64(len(src)) > p.max {
		return nil, ignored(rel, TooLarge)
	}
	if frontMatterIgnore(src) {
		return nil, ignored(rel, FrontMatter)
	}
	return src, nil
}

// Walk calls fn for every file under the root. src is set for files the
// policy allows; refusals pass an *IgnoredError, and read failures pass the
// error. Hidden folders and folders .screeignore fully excludes are reported
// once, as "dir/", and not descended; symlinked folders are never descended.
// Walk stops when fn returns an error, and fails before visiting anything if
// .screeignore exists but cannot be read.
func (p *Policy) Walk(ctx context.Context, fn func(rel string, src []byte, err error) error) error {
	m, err := p.matcher()
	if err != nil {
		return err
	}
	return filepath.WalkDir(p.root, func(abs string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if abs == p.root {
			return err
		}
		rel, ok := relUnder(p.root, abs)
		if !ok {
			return nil
		}
		if err != nil {
			if d != nil && d.IsDir() {
				rel += "/"
			}
			return fn(rel, nil, readErr(rel, err))
		}
		if d.IsDir() {
			switch {
			case strings.HasPrefix(d.Name(), "."):
				err = fn(rel+"/", nil, ignored(rel+"/", Hidden))
			case m.Prunable(rel):
				err = fn(rel+"/", nil, ignored(rel+"/", IgnoreFile))
			default:
				return nil
			}
			if err != nil {
				return err
			}
			return filepath.SkipDir
		}
		if rel == IgnoreFileName {
			return nil
		}
		if _, err := p.Clean(rel); err != nil {
			return fn(rel, nil, err)
		}
		resolve := d.Type()&fs.ModeSymlink != 0
		if resolve {
			if real, err := filepath.EvalSymlinks(abs); err == nil {
				if fi, err := os.Stat(real); err == nil && fi.IsDir() {
					if _, ok := relUnder(p.root, real); !ok {
						return fn(rel+"/", nil, ignored(rel+"/", OutsideRoot))
					}
					return nil
				}
			}
		}
		if r := nameReason(rel); r != Allowed {
			return fn(rel, nil, ignored(rel, r))
		}
		src, err := p.read(rel, resolve, m)
		return fn(rel, src, err)
	})
}

// Summary counts what a Scan found.
type Summary struct {
	Allowed      int
	Ignored      map[Reason][]string
	IgnoreErrors []string // bad .screeignore patterns
	ReadErrors   []string // files or folders that could not be read, with the reason
}

// Scan walks the root and counts allowed and ignored files by reason. Files
// that fail to read for other reasons are listed in ReadErrors.
func (p *Policy) Scan(ctx context.Context) (Summary, error) {
	s := Summary{Ignored: map[Reason][]string{}}
	err := p.Walk(ctx, func(rel string, _ []byte, err error) error {
		var ie *IgnoredError
		switch {
		case err == nil:
			s.Allowed++
		case errors.As(err, &ie):
			s.Ignored[ie.Reason] = append(s.Ignored[ie.Reason], rel)
		default:
			s.ReadErrors = append(s.ReadErrors, err.Error())
		}
		return nil
	})
	p.mu.Lock()
	s.IgnoreErrors = p.m.Errors()
	p.mu.Unlock()
	return s, err
}

// CheckRoot fails if the configured notes folder no longer refers to the
// folder the policy opened.
func (p *Policy) CheckRoot() error {
	have, err := p.r.Stat(".")
	if err != nil {
		return fmt.Errorf("notes folder %s: %w", p.given, err)
	}
	now, err := os.Stat(p.given)
	if err != nil || !os.SameFile(have, now) {
		return fmt.Errorf("notes folder %s removed or moved", p.given)
	}
	return nil
}

// matcher returns the .screeignore rules, reloading them when the file's
// modification time or size changed. A missing file means no rules; any
// other problem is returned so callers fail closed.
func (p *Policy) matcher() (*ignore.Matcher, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, err := p.statIgnore()
	if err != nil {
		p.m, p.stamp, p.loadErr = nil, fileStamp{}, nil
		return nil, err
	}
	if p.stamp == st {
		return p.m, p.loadErr
	}
	p.m, p.loadErr = nil, nil
	if st.missing {
		p.m = ignore.Parse(nil)
	} else {
		data, err := p.readIgnore()
		if err != nil {
			p.loadErr = err
		} else {
			p.m = ignore.Parse(data)
		}
	}
	p.stamp = st
	return p.m, p.loadErr
}

func (p *Policy) statIgnore() (fileStamp, error) {
	fi, err := p.r.Stat(IgnoreFileName)
	if errors.Is(err, fs.ErrNotExist) {
		if _, lerr := p.r.Lstat(IgnoreFileName); errors.Is(lerr, fs.ErrNotExist) {
			return fileStamp{ok: true, missing: true}, nil
		}
		return fileStamp{}, fmt.Errorf("read %s: dangling symlink: %w", IgnoreFileName, err)
	}
	if err != nil {
		return fileStamp{}, fmt.Errorf("read %s: %w", IgnoreFileName, err)
	}
	return fileStamp{ok: true, mod: fi.ModTime().UnixNano(), size: fi.Size()}, nil
}

func (p *Policy) readIgnore() ([]byte, error) {
	f, err := p.r.Open(IgnoreFileName)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", IgnoreFileName, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", IgnoreFileName, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("read %s: not a regular file", IgnoreFileName)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxIgnoreBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", IgnoreFileName, err)
	}
	if len(data) > maxIgnoreBytes {
		return nil, fmt.Errorf("read %s: larger than %d bytes", IgnoreFileName, maxIgnoreBytes)
	}
	return data, nil
}

// nameReason applies the built-in name rules to a clean, safe path.
func nameReason(rel string) Reason {
	for _, part := range strings.Split(rel, "/") {
		if strings.HasPrefix(part, ".") {
			return Hidden
		}
	}
	ext := strings.ToLower(path.Ext(rel))
	if ext != ".md" && ext != ".markdown" {
		return NotMarkdown
	}
	return Allowed
}

// unsafeName reports names with invalid UTF-8 or control characters (C0,
// DEL and C1), which could corrupt terminal or tool output.
func unsafeName(s string) bool {
	if !utf8.ValidString(s) {
		return true
	}
	for _, r := range s {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return true
		}
	}
	return false
}
