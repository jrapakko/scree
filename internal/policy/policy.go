// Package policy decides which files under the notes folder scree may read.
// Every note read goes through a Policy, so indexing, search, read_doc and
// reindex all apply the same built-in rules, git's ignore rules, .screeignore
// patterns and front-matter opt-outs.
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
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/jrapakko/scree/internal/ignore"
)

// DefaultMaxBytes is the largest note scree reads unless configured otherwise.
const DefaultMaxBytes int64 = 1 << 20

// IgnoreFileName is the ignore file read from the root of the notes folder.
const IgnoreFileName = ".screeignore"

// DefaultHiddenAllow lists the hidden folders that are searched like any
// other when they sit at the root: forge, CI and agent folders. A line
// "include-hidden .name/" in .screeignore adds to it.
var DefaultHiddenAllow = []string{
	".claude", ".github", ".gitlab", ".gitea", ".forgejo", ".cursor",
	".codex", ".gemini", ".windsurf", ".circleci", ".buildkite",
}

// includeHidden is the .screeignore directive that adds to the allowlist.
const includeHidden = "include-hidden"

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
	GitIgnore
	NestedRepo
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
		return "outside the folder"
	case TooLarge:
		return "too large"
	case GitIgnore:
		return ".gitignore"
	case NestedRepo:
		return "nested repository"
	case IgnoreFile:
		return ".screeignore"
	case FrontMatter:
		return "front matter"
	}
	return fmt.Sprintf("Reason(%d)", int(r))
}

// Layer names the ignore layer the reason belongs to: "built-in",
// ".gitignore", ".screeignore" or "front matter". It is empty for Allowed.
func (r Reason) Layer() string {
	switch r {
	case Allowed:
		return ""
	case GitIgnore, NestedRepo:
		return ".gitignore"
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

// ErrChanged means a file was replaced, for example by a symlink, between
// the policy's checks and the read. It counts as a read failure, so the
// file's existing rows are kept and the next event or sync retries it.
var ErrChanged = errors.New("file changed while reading")

// TestHooks lets tests act between read's steps. They are nil outside tests.
var TestHooks struct {
	BeforeLstat, BeforeOpen func(rel string)
	ReadDir                 func(parent string) // Canonical lists a folder
}

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
	// Cause is set when a read failure led to the refusal, such as a
	// folder skipped because its .gitignore cannot be read.
	Cause error
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
	git   *gitRepo       // nil when the git layer is off or the root is not in a work tree
	opts  ignore.Options // how patterns match: NFC always, case folded on case-insensitive filesystems

	mu      sync.Mutex
	m       *ignore.Matcher
	allow   []string // root hidden folders that are searched
	dirErrs []string // bad include-hidden directives
	stamp   fileStamp
	loadErr error
}

// Options configures OpenOptions.
type Options struct {
	MaxBytes  int64 // notes larger than this are refused; <= 0 means DefaultMaxBytes
	GitIgnore bool  // honor .gitignore, .git/info/exclude and core.excludesFile when the root is in a git work tree
}

// Open opens the notes folder at root with the git layer on. Notes larger
// than maxBytes are refused; maxBytes <= 0 means DefaultMaxBytes.
func Open(root string, maxBytes int64) (*Policy, error) {
	return OpenOptions(root, Options{MaxBytes: maxBytes, GitIgnore: true})
}

// OpenOptions opens the notes folder at root. With o.GitIgnore set, it looks
// for a git work tree at or above the root; a .git file that cannot be
// parsed is an error.
func OpenOptions(root string, o Options) (*Policy, error) {
	maxBytes := o.MaxBytes
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
	p := &Policy{given: given, root: resolved, r: r, max: maxBytes, allow: DefaultHiddenAllow}
	p.opts.FoldCase = probeFold(r)
	if o.GitIgnore {
		if p.git, err = findRepo(resolved); err != nil {
			r.Close()
			return nil, err
		}
		if p.git != nil {
			p.git.opts = p.opts
		}
	}
	return p, nil
}

// HiddenFolders returns the root hidden folders that are searched and exist
// as folders in the root: the defaults, and extra for those added by
// "include-hidden" in .screeignore, as loaded by the last Scan, Walk or Read.
func (p *Policy) HiddenFolders() (defaults, extra []string) {
	p.mu.Lock()
	allow := p.allow
	p.mu.Unlock()
	for _, name := range allow {
		if fi, err := p.r.Lstat(name); err != nil || !fi.IsDir() {
			continue
		}
		if slices.Contains(DefaultHiddenAllow, name) {
			defaults = append(defaults, name)
		} else if !slices.Contains(extra, name) {
			extra = append(extra, name)
		}
	}
	return defaults, extra
}

// Close releases the root folder handle.
func (p *Policy) Close() error { return p.r.Close() }

// GitTop returns the top of the git work tree the root is in, or "" when it
// is not in one or the git layer is off.
func (p *Policy) GitTop() string {
	if p.git == nil {
		return ""
	}
	return p.git.top
}

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
	// The path may reach the root through another symlink, such as macOS's
	// /var -> /private/var. Resolve the deepest existing directory above the
	// file, never the file itself, so a symlinked note keeps its own name;
	// Read still applies every rule to the result.
	dir, rest := filepath.Dir(abs), filepath.Base(abs)
	for {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			if rel, ok := relUnder(p.root, filepath.Join(real, rest)); ok {
				return rel, nil
			}
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir, rest = parent, filepath.Join(filepath.Base(dir), rest)
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

// CheckName applies the rules that need no file content: a valid, safe name,
// no hidden segment outside the allowed root folders, a markdown extension,
// git's ignore rules, and the most recently loaded .screeignore patterns. It
// stats at most one .gitignore per folder above rel. It returns Allowed when
// none of them refuse rel. A file that passes may still be refused by Read,
// which also fails when the git rules cannot be loaded.
func (p *Policy) CheckName(rel string) Reason {
	return p.NewCanon().CheckName(rel)
}

// checkName is CheckName on a canonical path.
func (p *Policy) checkName(clean string) Reason {
	if r := p.nameReason(clean); r != Allowed {
		return r
	}
	if v, err := p.gitView(); err == nil && v != nil {
		if r := v.fileReason(clean, true); r != Allowed {
			return r
		}
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
	src, _, err := p.ReadStat(rel)
	return src, err
}

// ReadStat is Read that also returns the opened file's FileInfo, which
// matches the content returned.
func (p *Policy) ReadStat(rel string) ([]byte, fs.FileInfo, error) {
	return p.NewCanon().ReadStat(rel)
}

// readStat is ReadStat on a canonical path.
func (p *Policy) readStat(clean string) ([]byte, fs.FileInfo, error) {
	rs, err := p.load()
	if err != nil {
		return nil, nil, err
	}
	if r := p.nameReason(clean); r != Allowed {
		return nil, nil, ignored(clean, r)
	}
	return p.read(clean, true, rs)
}

// Check applies every rule Read applies except those that need the content
// (front matter), without opening the file. It returns the FileInfo of the
// file that would be read. rel is a store key, already in on-disk form.
func (p *Policy) Check(rel string) (fs.FileInfo, error) {
	clean, err := p.Clean(rel)
	if err != nil {
		return nil, err
	}
	rs, err := p.load()
	if err != nil {
		return nil, err
	}
	if r := p.nameReason(clean); r != Allowed {
		return nil, ignored(clean, r)
	}
	_, fi, err := p.check(clean, true, rs)
	return fi, err
}

// rules are the loaded ignore rules for one operation.
type rules struct {
	m *ignore.Matcher
	g *gitView // nil when the git layer does not apply
}

// load loads .screeignore and the git rules. Either failing is an error, so
// callers fail closed.
func (p *Policy) load() (rules, error) {
	m, err := p.matcher()
	if err != nil {
		return rules{}, err
	}
	g, err := p.gitView()
	if err != nil {
		return rules{}, err
	}
	return rules{m: m, g: g}, nil
}

// read runs the checks after the name rules on rel. With resolve false, rel
// is known to contain no symlinks and to sit in no nested repository.
func (p *Policy) read(rel string, resolve bool, rs rules) ([]byte, fs.FileInfo, error) {
	realRel, lfi, err := p.check(rel, resolve, rs)
	if err != nil {
		return nil, nil, err
	}
	if TestHooks.BeforeOpen != nil {
		TestHooks.BeforeOpen(realRel)
	}
	// Never wait on a FIFO swapped in after the checks; the checks after
	// opening refuse anything but the regular file Lstat saw.
	f, err := p.r.OpenFile(realRel, os.O_RDONLY|userOpenFlags, 0)
	if err != nil {
		return nil, nil, readErr(rel, err)
	}
	defer f.Close()
	ffi, err := f.Stat()
	if err != nil {
		return nil, nil, readErr(rel, err)
	}
	// What was opened must be the file the rules were checked on, reached
	// through the same folders: os.Root follows in-root symlinks in folder
	// names, so a folder swapped for a link to a hidden one would otherwise
	// pass. A swap and swap back between these checks remains possible; it
	// needs a local process that can already read the files.
	if !os.SameFile(lfi, ffi) {
		return nil, nil, readErr(rel, ErrChanged)
	}
	if again, err := p.resolve(realRel); err != nil || again != realRel {
		return nil, nil, readErr(rel, ErrChanged)
	}
	if now, err := p.r.Lstat(realRel); err != nil || !os.SameFile(now, ffi) {
		return nil, nil, readErr(rel, ErrChanged)
	}
	src, err := io.ReadAll(io.LimitReader(f, p.max+1))
	if err != nil {
		return nil, nil, readErr(rel, err)
	}
	if int64(len(src)) > p.max {
		return nil, nil, ignored(rel, TooLarge)
	}
	if frontMatterIgnore(src) {
		return nil, nil, ignored(rel, FrontMatter)
	}
	return src, ffi, nil
}

// check runs every rule of read that needs no content, and returns the path
// relative to the root that would be opened and its Lstat.
func (p *Policy) check(rel string, resolve bool, rs rules) (string, fs.FileInfo, error) {
	m := rs.m
	realRel := rel
	if resolve {
		r, err := p.resolve(rel)
		if err != nil {
			return "", nil, err
		}
		if realRel = r; realRel != rel {
			if unsafeName(realRel) {
				return "", nil, ignored(rel, UnsafeName)
			}
			if r := p.nameReason(realRel); r != Allowed {
				return "", nil, ignored(rel, r)
			}
		}
	}
	if TestHooks.BeforeLstat != nil {
		TestHooks.BeforeLstat(realRel)
	}
	lfi, err := p.r.Lstat(realRel)
	if err != nil {
		return "", nil, readErr(rel, err)
	}
	if lfi.Mode()&fs.ModeSymlink != 0 {
		if !resolve {
			// It became a symlink after Walk listed it.
			return p.check(rel, true, rs)
		}
		return "", nil, readErr(rel, ErrChanged)
	}
	if !lfi.Mode().IsRegular() {
		return "", nil, readErr(rel, ErrNotRegular)
	}
	if lfi.Size() > p.max {
		return "", nil, ignored(rel, TooLarge)
	}
	if rs.g != nil {
		for _, x := range []string{rel, realRel} {
			if r := rs.g.fileReason(x, resolve); r != Allowed {
				return "", nil, ignored(rel, r)
			}
		}
	}
	if m.Match(rel, false) || m.Match(realRel, false) {
		return "", nil, ignored(rel, IgnoreFile)
	}
	return realRel, lfi, nil
}

// resolve follows every symlink in rel and returns the result relative to
// the root. A target outside the root is refused with OutsideRoot.
func (p *Policy) resolve(rel string) (string, error) {
	real, err := filepath.EvalSymlinks(filepath.Join(p.root, filepath.FromSlash(rel)))
	if err != nil {
		return "", readErr(rel, err)
	}
	r, ok := relUnder(p.root, real)
	if !ok {
		return "", ignored(rel, OutsideRoot)
	}
	return r, nil
}

// Walk calls fn for every file under the root. src is set for files the
// policy allows; refusals pass an *IgnoredError, and read failures pass the
// error. Hidden folders, nested repositories, and folders git or .screeignore
// fully excludes are reported once, as "dir/", and not descended or listed;
// symlinked folders are never descended. .gitignore files and the root
// .screeignore are skipped without a report. Walk stops when fn returns an
// error, and fails before visiting anything if .screeignore or the git rules
// that apply to the root cannot be read.
func (p *Policy) Walk(ctx context.Context, fn func(rel string, src []byte, err error) error) error {
	_, err := p.walk(ctx, true, func(rel string, src []byte, _ fs.FileInfo, err error) error { return fn(rel, src, err) })
	return err
}

// WalkStat is Walk that also passes the FileInfo of each allowed file as it
// was opened, matching src.
func (p *Policy) WalkStat(ctx context.Context, fn func(rel string, src []byte, fi fs.FileInfo, err error) error) error {
	_, err := p.walk(ctx, true, fn)
	return err
}

// Estimate counts the files a Walk would allow and their total size,
// without opening any of them: every name, git, .screeignore and size rule
// applies, but front matter is not checked, so it is an upper bound.
func (p *Policy) Estimate(ctx context.Context) (files int, bytes int64, err error) {
	_, err = p.walk(ctx, false, func(rel string, _ []byte, fi fs.FileInfo, err error) error {
		if err == nil && fi != nil {
			files++
			bytes += fi.Size()
		}
		return nil
	})
	return files, bytes, err
}

// walk is Walk. With read false, files are only checked (Lstat, never
// opened), and fn gets a nil src.
func (p *Policy) walk(ctx context.Context, read bool, fn func(rel string, src []byte, fi fs.FileInfo, err error) error) (rules, error) {
	rs, err := p.load()
	if err != nil {
		return rs, err
	}
	return rs, filepath.WalkDir(p.root, func(abs string, d fs.DirEntry, err error) error {
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
			return fn(rel, nil, nil, readErr(rel, err))
		}
		if d.IsDir() {
			r, cause := p.dirReason(rel, rs)
			if r == Allowed {
				return nil
			}
			if err := fn(rel+"/", nil, nil, &IgnoredError{Path: rel + "/", Reason: r, Cause: cause}); err != nil {
				return err
			}
			return filepath.SkipDir
		}
		if rel == IgnoreFileName || d.Name() == GitIgnoreFileName {
			return nil
		}
		if _, err := p.Clean(rel); err != nil {
			return fn(rel, nil, nil, err)
		}
		resolve := d.Type()&fs.ModeSymlink != 0
		if resolve {
			if real, err := filepath.EvalSymlinks(abs); err == nil {
				if fi, err := os.Stat(real); err == nil && fi.IsDir() {
					if _, ok := relUnder(p.root, real); !ok {
						return fn(rel+"/", nil, nil, ignored(rel+"/", OutsideRoot))
					}
					return nil
				}
			}
		}
		if r := p.nameReason(rel); r != Allowed {
			return fn(rel, nil, nil, ignored(rel, r))
		}
		if !read {
			_, fi, err := p.check(rel, resolve, rs)
			return fn(rel, nil, fi, err)
		}
		src, fi, err := p.read(rel, resolve, rs)
		return fn(rel, src, fi, err)
	})
}

// dirReason decides whether a walk skips the folder rel, in this order:
// Hidden, NestedRepo, GitIgnore (cause is set when its .gitignore cannot be
// read), IgnoreFile. The git checks run before anything in the folder is
// listed or read.
func (p *Policy) dirReason(rel string, rs rules) (Reason, error) {
	p.mu.Lock()
	allow := p.allow
	p.mu.Unlock()
	if hiddenReason(strings.Split(rel, "/"), true, allow) != Allowed {
		return Hidden, nil
	}
	if rs.g != nil {
		if r, cause := rs.g.dirReason(rel); r != Allowed {
			return r, cause
		}
	}
	if rs.m.Prunable(rel) {
		return IgnoreFile, nil
	}
	return Allowed, nil
}

// SkipDir reports whether the folder rel (a clean store key) is skipped, as
// Walk decides it: Allowed, Hidden, NestedRepo, GitIgnore or IgnoreFile. When
// the ignore rules cannot be loaded it applies the built-in rule only; Read
// still refuses every file then.
func (p *Policy) SkipDir(rel string) Reason {
	rs, err := p.load()
	if err != nil {
		rs = rules{}
	}
	r, _ := p.dirReason(rel, rs)
	return r
}

// ErrBadIgnoreFile means .screeignore has a line scree cannot parse. Nothing
// is indexed until it is fixed, since the line may have been meant to keep
// files private.
var ErrBadIgnoreFile = errors.New("bad line in " + IgnoreFileName)

// CheckIgnoreFile reloads .screeignore if it changed and returns an error
// wrapping ErrBadIgnoreFile that names each bad line by number (never its
// text). A missing file is fine; an unreadable one is reported by the
// read path, which refuses everything.
func (p *Policy) CheckIgnoreFile() error {
	if _, err := p.matcher(); err != nil {
		return nil
	}
	p.mu.Lock()
	errs := append(slices.Clone(p.dirErrs), p.m.Errors()...)
	p.mu.Unlock()
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s; nothing is indexed until it is fixed (scree doctor lists the lines)",
		ErrBadIgnoreFile, strings.Join(errs, ", "))
}

// Summary counts what a Scan found.
type Summary struct {
	Allowed      int
	Bytes        int64 // total size of the allowed files
	Ignored      map[Reason][]string
	IgnoreErrors []string // bad patterns and directives, each prefixed with its file name
	ReadErrors   []string // files or folders that could not be read, with the reason
	// Reincluded lists allowed files that an earlier .screeignore rule
	// excluded and a "!" rule re-included, as `"path" (.screeignore line N)`.
	// The rule's text is never shown: messages carry no file content.
	Reincluded []string
}

// Scan walks the root and counts allowed and ignored files by reason. Files
// that fail to read for other reasons are listed in ReadErrors.
func (p *Policy) Scan(ctx context.Context) (Summary, error) {
	s := Summary{Ignored: map[Reason][]string{}}
	var allowed []string
	rs, err := p.walk(ctx, true, func(rel string, src []byte, _ fs.FileInfo, err error) error {
		var ie *IgnoredError
		switch {
		case err == nil:
			allowed = append(allowed, rel)
			s.Allowed++
			s.Bytes += int64(len(src))
		case errors.As(err, &ie):
			s.Ignored[ie.Reason] = append(s.Ignored[ie.Reason], rel)
			if ie.Cause != nil {
				s.ReadErrors = append(s.ReadErrors, ie.Cause.Error())
			}
		default:
			s.ReadErrors = append(s.ReadErrors, err.Error())
		}
		return nil
	})
	for _, rel := range allowed {
		if _, n := rs.m.Explain(rel, false); n > 0 {
			s.Reincluded = append(s.Reincluded, fmt.Sprintf("%s (%s line %d)", strconv.Quote(rel), IgnoreFileName, n))
		}
	}
	p.mu.Lock()
	for _, e := range append(slices.Clone(p.dirErrs), p.m.Errors()...) {
		s.IgnoreErrors = append(s.IgnoreErrors, IgnoreFileName+" "+e)
	}
	p.mu.Unlock()
	if rs.g != nil {
		s.IgnoreErrors = append(s.IgnoreErrors, rs.g.patErrs...)
	}
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
// other problem is returned so callers fail closed. The file is stat'ed and
// read without holding p.mu, which only guards the cached result.
func (p *Policy) matcher() (*ignore.Matcher, error) {
	st, err := p.statIgnore()
	if err != nil {
		p.mu.Lock()
		p.m, p.stamp, p.loadErr = nil, fileStamp{}, nil
		p.allow, p.dirErrs = DefaultHiddenAllow, nil
		p.mu.Unlock()
		return nil, err
	}
	p.mu.Lock()
	if p.stamp == st {
		m, err := p.m, p.loadErr
		p.mu.Unlock()
		return m, err
	}
	p.mu.Unlock()
	var (
		m       *ignore.Matcher
		loadErr error
		allow   = DefaultHiddenAllow
		dirErrs []string
	)
	if st.missing {
		m = ignore.ParseWith(nil, p.opts)
	} else {
		data, rst, err := p.readIgnore()
		switch {
		case err != nil:
			loadErr = err
		case rst.missing:
			m = ignore.ParseWith(nil, p.opts)
		default:
			var extra []string
			var rest []byte
			extra, rest, dirErrs = parseDirectives(data)
			m = ignore.ParseWith(rest, p.opts)
			allow = append(slices.Clone(DefaultHiddenAllow), extra...)
			st = rst
		}
	}
	p.mu.Lock()
	p.m, p.loadErr, p.allow, p.dirErrs, p.stamp = m, loadErr, allow, dirErrs, st
	p.mu.Unlock()
	return m, loadErr
}

func (p *Policy) statIgnore() (fileStamp, error) {
	st, err := p.statRootRepoFile(IgnoreFileName)
	if err != nil {
		return fileStamp{}, fmt.Errorf("read %s: %w", IgnoreFileName, err)
	}
	return st, nil
}

// readIgnore reads .screeignore, which comes with the folder, so it is never
// read through a symlink.
func (p *Policy) readIgnore() ([]byte, fileStamp, error) {
	data, st, err := p.readRootRepoFile(IgnoreFileName)
	if err != nil {
		return nil, fileStamp{}, fmt.Errorf("read %s: %w", IgnoreFileName, err)
	}
	return data, st, nil
}

// parseDirectives takes the "include-hidden .name/" lines out of .screeignore
// text. Each becomes an empty line in rest, so pattern line numbers stay
// right. A bad directive is reported as "line N: bad include-hidden
// directive", without its text, and allows nothing.
// "\include-hidden" stays a pattern.
func parseDirectives(data []byte) (allow []string, rest []byte, errs []string) {
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		t := strings.TrimRight(strings.TrimSuffix(line, "\r"), " \t")
		arg, ok := strings.CutPrefix(t, includeHidden)
		if !ok || (arg != "" && arg[0] != ' ' && arg[0] != '\t') {
			continue
		}
		lines[i] = ""
		arg = strings.TrimSpace(arg)
		name, slash := strings.CutSuffix(arg, "/")
		if !slash || !strings.HasPrefix(name, ".") || name == "." || name == ".." || name == ".git" ||
			strings.ContainsAny(name, "/*?[]\\") || unsafeName(name) {
			errs = append(errs, fmt.Sprintf("line %d: bad %s directive", i+1, includeHidden))
			continue
		}
		allow = append(allow, name)
	}
	return allow, []byte(strings.Join(lines, "\n")), errs
}

// nameReason applies the built-in name rules to a clean, safe path, using the
// most recently loaded hidden-folder allowlist.
func (p *Policy) nameReason(rel string) Reason {
	p.mu.Lock()
	allow := p.allow
	p.mu.Unlock()
	if r := hiddenReason(strings.Split(rel, "/"), false, allow); r != Allowed {
		return r
	}
	switch strings.ToLower(path.Ext(rel)) {
	case ".md", ".markdown", ".mdc":
		return Allowed
	}
	return NotMarkdown
}

// hiddenReason returns Hidden when a segment of the path segs starts with
// ".", except a first segment in allow that names a folder (isDir says
// whether the last segment is one). .claude/worktrees is always hidden: it
// holds full copies of the repo.
func hiddenReason(segs []string, isDir bool, allow []string) Reason {
	if len(segs) >= 2 && segs[0] == ".claude" && segs[1] == "worktrees" {
		return Hidden
	}
	for i, s := range segs {
		if !strings.HasPrefix(s, ".") {
			continue
		}
		if i == 0 && (isDir || len(segs) > 1) && slices.Contains(allow, s) {
			continue
		}
		return Hidden
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
