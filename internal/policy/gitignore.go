package policy

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/jrapakko/scree/internal/ignore"
)

// GitIgnoreFileName is the per-folder ignore file of a git work tree.
const GitIgnoreFileName = ".gitignore"

// maxGitFileBytes caps .gitignore, info/exclude, the global excludes file and
// git config files.
const maxGitFileBytes = 256 << 10

// matchAll stands in for a .gitignore that cannot be read: it excludes
// everything in its folder.
var matchAll = ignore.Parse([]byte("*\n"))

// gitRepo is the git work tree around the root, and the cached ignore
// sources. Fields below the blank line are guarded by Policy.mu.
type gitRepo struct {
	top     string   // the work tree top, symlinks resolved
	prefix  string   // the root relative to top: "" or "a/b/"
	common  string   // the git common dir, for info/exclude and config
	configs []string // git config files, lowest precedence first
	home    string   // for "~/" in core.excludesFile; "" if unknown
	xdgIgn  string   // the default global excludes file; "" if unknown

	cfgStamps []fileStamp
	excludes  string // the global excludes file in effect
	global    extFile
	exclude   extFile
	dirs      map[string]*dirRules // by top-relative folder
}

// extFile is an ignore source outside the root, read by absolute path.
type extFile struct {
	path  string
	stamp fileStamp
	m     *ignore.Matcher
}

// dirRules is one folder's cached .gitignore.
type dirRules struct {
	stamp fileStamp
	m     *ignore.Matcher // nil when there is no .gitignore
	err   error           // the file exists but cannot be used; the folder is excluded
}

// findRepo walks up from the resolved root looking for an entry named
// ".git". It returns nil when the root is not in a work tree.
func findRepo(root string) (*gitRepo, error) {
	for d := root; ; {
		dotGit := filepath.Join(d, ".git")
		_, err := os.Lstat(dotGit)
		if err == nil {
			g, err := newRepo(root, d, dotGit)
			if err != nil {
				return nil, fmt.Errorf("git work tree at %s: %w", d, err)
			}
			return g, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("git work tree at %s: %w", d, err)
		}
		parent := filepath.Dir(d)
		if parent == d {
			return nil, nil
		}
		d = parent
	}
}

func newRepo(root, top, dotGit string) (*gitRepo, error) {
	fi, err := os.Stat(dotGit)
	if err != nil {
		return nil, err
	}
	gitDir := dotGit
	switch {
	case fi.IsDir():
	case fi.Mode().IsRegular():
		data, err := readCapped(dotGit)
		if err != nil {
			return nil, err
		}
		line, _, _ := strings.Cut(string(data), "\n")
		dir, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
		if dir = strings.TrimSpace(dir); !ok || dir == "" {
			return nil, errors.New(`.git file has no "gitdir:" line`)
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(top, dir)
		}
		gitDir = dir
	default:
		return nil, errors.New(".git is not a folder or a file")
	}
	common := gitDir
	switch data, err := readCapped(filepath.Join(gitDir, "commondir")); {
	case err == nil:
		c := strings.TrimSpace(string(data))
		if !filepath.IsAbs(c) {
			c = filepath.Join(gitDir, c)
		}
		common = c
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}

	prefix := ""
	if rel, ok := relUnder(top, root); ok && rel != "." {
		prefix = rel + "/"
	}
	g := &gitRepo{top: top, prefix: prefix, common: common, dirs: map[string]*dirRules{}}
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if home, err := os.UserHomeDir(); err == nil {
		g.home = home
		if xdg == "" {
			xdg = filepath.Join(home, ".config")
		}
	}
	if xdg != "" {
		g.configs = append(g.configs, filepath.Join(xdg, "git", "config"))
		g.xdgIgn = filepath.Join(xdg, "git", "ignore")
	}
	if g.home != "" {
		g.configs = append(g.configs, filepath.Join(g.home, ".gitconfig"))
	}
	g.configs = append(g.configs, filepath.Join(common, "config"))
	return g, nil
}

// statFile stamps a file outside the root. A missing file gives a missing
// stamp; a dangling symlink is an error.
func statFile(name string) (fileStamp, error) {
	fi, err := os.Stat(name)
	if errors.Is(err, fs.ErrNotExist) {
		if _, lerr := os.Lstat(name); errors.Is(lerr, fs.ErrNotExist) {
			return fileStamp{ok: true, missing: true}, nil
		}
		return fileStamp{}, fmt.Errorf("read %s: dangling symlink: %w", name, err)
	}
	if err != nil {
		return fileStamp{}, fmt.Errorf("read %s: %w", name, err)
	}
	return fileStamp{ok: true, mod: fi.ModTime().UnixNano(), size: fi.Size()}, nil
}

// readCapped reads a regular file of at most maxGitFileBytes.
func readCapped(name string) ([]byte, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readCappedFile(f, name)
}

func readCappedFile(f fs.File, name string) ([]byte, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: %w", name, ErrNotRegular)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxGitFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxGitFileBytes {
		return nil, fmt.Errorf("%s: larger than %d bytes", name, maxGitFileBytes)
	}
	return data, nil
}

// excludesFile reads core.excludesFile from git config text. ok is false when
// the key is not set. [include] and [includeIf] are not followed.
func excludesFile(data []byte) (value string, ok bool) {
	section := ""
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(nil, maxGitFileBytes+1)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			end := strings.IndexByte(line, ']')
			if end < 0 {
				continue
			}
			name, _, _ := strings.Cut(strings.TrimSpace(line[1:end]), " ")
			section = strings.ToLower(name)
			line = strings.TrimSpace(line[end+1:])
			if line == "" {
				continue
			}
		}
		if section != "core" {
			continue
		}
		key, val, _ := strings.Cut(line, "=")
		if !strings.EqualFold(strings.TrimSpace(key), "excludesfile") {
			continue
		}
		value, ok = configValue(val), true
	}
	return value, ok
}

// configValue decodes a git config value: double quotes group, backslash
// escapes, and an unquoted "#" or ";" starts a comment.
func configValue(s string) string {
	var b strings.Builder
	quoted := false
	s = strings.TrimSpace(s)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			i++
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			default:
				b.WriteByte(s[i])
			}
		case c == '"':
			quoted = !quoted
		case !quoted && (c == '#' || c == ';'):
			return strings.TrimSpace(b.String())
		default:
			b.WriteByte(c)
		}
	}
	return strings.TrimSpace(b.String())
}

// loadBase returns the global excludes file and info/exclude, in that order,
// reloading what changed. The caller holds Policy.mu.
func (g *gitRepo) loadBase() ([]*ignore.Matcher, error) {
	stamps := make([]fileStamp, len(g.configs))
	changed := len(g.cfgStamps) != len(stamps)
	for i, c := range g.configs {
		st, err := statFile(c)
		if err != nil {
			g.cfgStamps = nil
			return nil, err
		}
		stamps[i] = st
		changed = changed || g.cfgStamps[i] != st
	}
	if changed {
		excludes, set := "", false
		for i, c := range g.configs {
			if stamps[i].missing {
				continue
			}
			data, err := readCapped(c)
			if err != nil {
				g.cfgStamps = nil
				return nil, fmt.Errorf("read %s: %w", c, stripPath(err))
			}
			if v, ok := excludesFile(data); ok {
				excludes, set = v, true
			}
		}
		switch {
		case !set || excludes == "":
			excludes = g.xdgIgn
		case strings.HasPrefix(excludes, "~/") && g.home != "":
			excludes = filepath.Join(g.home, excludes[2:])
		case !filepath.IsAbs(excludes):
			excludes = filepath.Join(g.top, excludes)
		}
		g.excludes, g.cfgStamps = excludes, stamps
	}
	global, err := g.global.load(g.excludes)
	if err != nil {
		return nil, err
	}
	exclude, err := g.exclude.load(filepath.Join(g.common, "info", "exclude"))
	if err != nil {
		return nil, err
	}
	return []*ignore.Matcher{global, exclude}, nil
}

// load returns the rules in the file at name, re-reading it when its stamp
// changed. An empty name or a missing file means no rules.
func (f *extFile) load(name string) (*ignore.Matcher, error) {
	if name == "" {
		return nil, nil
	}
	if f.path != name {
		*f = extFile{path: name}
	}
	st, err := statFile(name)
	if err != nil {
		f.stamp = fileStamp{}
		return nil, err
	}
	if st == f.stamp {
		return f.m, nil
	}
	f.m = nil
	if !st.missing {
		data, err := readCapped(name)
		if err != nil {
			f.stamp = fileStamp{}
			return nil, fmt.Errorf("read %s: %w", name, stripPath(err))
		}
		f.m = ignore.Parse(data)
	}
	f.stamp = st
	return f.m, nil
}

// rootRel maps a top-relative folder to the root. inside is false for the
// folders between the top and the root.
func (g *gitRepo) rootRel(dir string) (rel string, inside bool) {
	switch {
	case g.prefix == "":
		return dir, true
	case dir == strings.TrimSuffix(g.prefix, "/"):
		return "", true
	case strings.HasPrefix(dir, g.prefix):
		return dir[len(g.prefix):], true
	}
	return "", false
}

// fileName names the .gitignore in the top-relative folder dir for messages:
// root-relative inside the root, absolute above it.
func (g *gitRepo) fileName(dir string) string {
	if rel, inside := g.rootRel(dir); inside {
		return path.Join(rel, GitIgnoreFileName)
	}
	return filepath.Join(g.top, filepath.FromSlash(dir), GitIgnoreFileName)
}

// dirMatcher returns the rules of the .gitignore in the top-relative folder
// dir, reloading it when its stamp changed. A file inside the root is read
// through the root handle, so a symlink leading outside it fails. The caller
// holds Policy.mu.
func (p *Policy) dirMatcher(dir string) (*ignore.Matcher, error) {
	g := p.git
	dr := g.dirs[dir]
	if dr == nil {
		dr = &dirRules{}
		g.dirs[dir] = dr
	}
	rel, inside := g.rootRel(dir)
	name := g.fileName(dir)
	fail := func(err error) (*ignore.Matcher, error) {
		dr.stamp, dr.m, dr.err = fileStamp{}, nil, fmt.Errorf("%w; folder skipped", readErr(name, err))
		return nil, dr.err
	}
	var st fileStamp
	var err error
	if inside {
		st, err = p.statInRoot(path.Join(rel, GitIgnoreFileName))
	} else {
		st, err = statFile(name)
	}
	if err != nil {
		return fail(err)
	}
	if st == dr.stamp {
		return dr.m, dr.err
	}
	dr.m, dr.err = nil, nil
	if !st.missing {
		var data []byte
		if inside {
			data, err = p.readInRoot(path.Join(rel, GitIgnoreFileName))
		} else {
			data, err = readCapped(name)
		}
		if err != nil {
			return fail(err)
		}
		dr.m = ignore.Parse(data)
	}
	dr.stamp = st
	return dr.m, nil
}

// statInRoot stamps a file under the root.
func (p *Policy) statInRoot(rel string) (fileStamp, error) {
	fi, err := p.r.Stat(rel)
	if errors.Is(err, fs.ErrNotExist) {
		if _, lerr := p.r.Lstat(rel); errors.Is(lerr, fs.ErrNotExist) {
			return fileStamp{ok: true, missing: true}, nil
		}
		return fileStamp{}, fmt.Errorf("dangling symlink: %w", err)
	}
	if err != nil {
		return fileStamp{}, err
	}
	if !fi.Mode().IsRegular() {
		return fileStamp{}, ErrNotRegular
	}
	return fileStamp{ok: true, mod: fi.ModTime().UnixNano(), size: fi.Size()}, nil
}

func (p *Policy) readInRoot(rel string) ([]byte, error) {
	f, err := p.r.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readCappedFile(f, rel)
}

// stripPath drops the path a *fs.PathError carries.
func stripPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// gitView is a snapshot of the git rules for one Read, CheckName or Walk.
// Each .gitignore is checked at most once per view. It is not safe for
// concurrent use.
type gitView struct {
	p       *Policy
	g       *gitRepo
	rules   ignore.GitRules
	memo    map[string]*ignore.Matcher
	errs    map[string]error // folders whose .gitignore cannot be used
	patErrs []string         // bad patterns, with the file name
}

// gitView loads the git rules. It is nil when the git layer is off or the
// root is not in a work tree. An unusable global excludes file, info/exclude,
// config file, or .gitignore in the root or a folder above it is an error, so
// callers fail closed.
func (p *Policy) gitView() (*gitView, error) {
	if p.git == nil {
		return nil, nil
	}
	p.mu.Lock()
	base, err := p.git.loadBase()
	p.mu.Unlock()
	if err != nil {
		return nil, err
	}
	v := &gitView{p: p, g: p.git, memo: map[string]*ignore.Matcher{}, errs: map[string]error{}}
	v.rules.Base = base
	v.rules.Dir = v.dir
	for i, name := range []string{p.git.global.path, filepath.Join(p.git.common, "info", "exclude")} {
		for _, e := range base[i].Errors() {
			v.patErrs = append(v.patErrs, name+" "+e)
		}
	}
	dirs := []string{""}
	if pr := strings.TrimSuffix(p.git.prefix, "/"); pr != "" {
		segs := strings.Split(pr, "/")
		for i := 1; i <= len(segs); i++ {
			dirs = append(dirs, strings.Join(segs[:i], "/"))
		}
	}
	for _, d := range dirs {
		v.dir(d)
		if err := v.errs[d]; err != nil {
			return nil, err
		}
	}
	return v, nil
}

// dir is GitRules.Dir. A .gitignore that cannot be used excludes its folder.
func (v *gitView) dir(d string) *ignore.Matcher {
	if m, ok := v.memo[d]; ok {
		return m
	}
	v.p.mu.Lock()
	m, err := v.p.dirMatcher(d)
	v.p.mu.Unlock()
	if err != nil {
		v.errs[d] = err
		m = matchAll
	} else {
		for _, e := range m.Errors() {
			v.patErrs = append(v.patErrs, v.g.fileName(d)+" "+e)
		}
	}
	v.memo[d] = m
	return m
}

// dirReason applies the git layer to the folder rel under the root, before
// anything in it is read: NestedRepo when it holds a .git entry, GitIgnore
// when git excludes it or its own .gitignore cannot be used (then cause says
// why).
func (v *gitView) dirReason(rel string) (r Reason, cause error) {
	if _, err := v.p.r.Lstat(rel + "/.git"); err == nil {
		return NestedRepo, nil
	}
	t := v.g.prefix + rel
	if v.rules.IgnoredDir(t) {
		return GitIgnore, nil
	}
	v.dir(t)
	if err := v.errs[t]; err != nil {
		return GitIgnore, err
	}
	return Allowed, nil
}

// fileReason applies the git layer to the file rel under the root. With
// nested set, it also refuses a file inside a nested repository.
func (v *gitView) fileReason(rel string, nested bool) Reason {
	if nested {
		segs := strings.Split(rel, "/")
		for i := 1; i < len(segs); i++ {
			if _, err := v.p.r.Lstat(strings.Join(segs[:i], "/") + "/.git"); err == nil {
				return NestedRepo
			}
		}
	}
	if v.rules.Ignored(v.g.prefix+rel, false) {
		return GitIgnore
	}
	return Allowed
}
