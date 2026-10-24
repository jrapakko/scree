package policy

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
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
// sources. Fields below the blank line are guarded by Policy.mu, which is
// never held while a file is read.
type gitRepo struct {
	top     string   // the work tree top, symlinks resolved
	prefix  string   // the root relative to top: "" or "a/b/"
	common  string   // the git common dir, for info/exclude
	configs []string // the user's git config files, lowest precedence first
	home    string   // for "~/" in core.excludesFile; "" if unknown
	xdgIgn  string   // the default global excludes file; "" if unknown
	opts    ignore.Options

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

// findRepo finds the git work tree around the resolved root with
// FindGitTop. It returns nil when the root is not in one.
func findRepo(root string) (*gitRepo, error) {
	top, err := FindGitTop(root)
	if err != nil || top == "" {
		return nil, err
	}
	g, err := newRepo(root, top, filepath.Join(top, ".git"))
	if err != nil {
		return nil, fmt.Errorf("git work tree at %s: %w", top, err)
	}
	return g, nil
}

func newRepo(root, top, dotGit string) (*gitRepo, error) {
	fi, err := os.Lstat(dotGit)
	if err != nil {
		return nil, err
	}
	gitDir := dotGit
	switch {
	case fi.IsDir():
	case fi.Mode().IsRegular():
		data, _, err := readRepoFile(dotGit)
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
		if err := checkGitDir(dir, dotGit); err != nil {
			return nil, err
		}
		gitDir = dir
	default:
		return nil, errors.New(".git is not a folder or a file")
	}
	common := gitDir
	switch data, st, err := readRepoFile(filepath.Join(gitDir, "commondir")); {
	case err != nil:
		return nil, err
	case !st.missing:
		c := strings.TrimSpace(string(data))
		if !filepath.IsAbs(c) {
			c = filepath.Join(gitDir, c)
		}
		if err := checkGitDir(c, filepath.Join(gitDir, "commondir")); err != nil {
			return nil, err
		}
		common = c
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
	// The repository's own .git/config is never read: a planted one could
	// name any file as the excludes file. Only the user's config can.
	return g, nil
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
// reloading what changed. Files are stat'ed and read without holding
// Policy.mu, which is taken only to compare stamps and swap cached values, so
// a slow filesystem stalls only its own caller.
func (p *Policy) loadBase() ([]*ignore.Matcher, error) {
	g := p.git
	stamps := make([]fileStamp, len(g.configs))
	for i, c := range g.configs {
		st, err := statUserFile(c)
		if err != nil {
			p.mu.Lock()
			g.cfgStamps = nil
			p.mu.Unlock()
			return nil, err
		}
		stamps[i] = st
	}
	p.mu.Lock()
	changed := !slices.Equal(g.cfgStamps, stamps)
	excludes := g.excludes
	p.mu.Unlock()
	if changed {
		value, set := "", false
		for i, c := range g.configs {
			if stamps[i].missing {
				continue
			}
			data, _, err := readUserFile(c)
			if err != nil {
				p.mu.Lock()
				g.cfgStamps = nil
				p.mu.Unlock()
				return nil, err
			}
			if v, ok := excludesFile(data); ok {
				value, set = v, true
			}
		}
		switch {
		case !set || value == "":
			excludes = g.xdgIgn
		case strings.HasPrefix(value, "~/") && g.home != "":
			excludes = filepath.Join(g.home, value[2:])
		case !filepath.IsAbs(value):
			excludes = filepath.Join(g.top, value)
		default:
			excludes = value
		}
		p.mu.Lock()
		g.excludes, g.cfgStamps = excludes, stamps
		p.mu.Unlock()
	}
	global, err := p.loadExt(&g.global, excludes, true)
	if err != nil {
		return nil, err
	}
	exclude, err := p.loadExt(&g.exclude, filepath.Join(g.common, "info", "exclude"), false)
	if err != nil {
		return nil, err
	}
	return []*ignore.Matcher{global, exclude}, nil
}

// loadExt returns the rules in the file at name, cached in f, re-reading it
// when its stamp changed. An empty name or a missing file means no rules.
// user says whether the file is user-controlled (symlinks followed) or comes
// with the repo. Policy.mu guards f and is never held during I/O.
func (p *Policy) loadExt(f *extFile, name string, user bool) (*ignore.Matcher, error) {
	if name == "" {
		return nil, nil
	}
	stat, read := statRepoFile, readRepoFile
	if user {
		stat, read = statUserFile, readUserFile
	}
	st, err := stat(name)
	if err != nil {
		p.mu.Lock()
		*f = extFile{path: name}
		p.mu.Unlock()
		return nil, err
	}
	p.mu.Lock()
	if f.path == name && f.stamp == st {
		m := f.m
		p.mu.Unlock()
		return m, nil
	}
	p.mu.Unlock()
	var m *ignore.Matcher
	if !st.missing {
		data, rst, err := read(name)
		if err != nil {
			p.mu.Lock()
			*f = extFile{path: name}
			p.mu.Unlock()
			return nil, err
		}
		m, st = ignore.ParseWith(data, p.git.opts), rst
	}
	p.mu.Lock()
	*f = extFile{path: name, stamp: st, m: m}
	p.mu.Unlock()
	return m, nil
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
// dir, reloading it when its stamp changed. A .gitignore is never read
// through a symlink, and a file inside the root is read through the root
// handle. Like loadBase, it holds Policy.mu only around the cache.
func (p *Policy) dirMatcher(dir string) (*ignore.Matcher, error) {
	g := p.git
	rel, inside := g.rootRel(dir)
	name := g.fileName(dir)
	store := func(dr *dirRules) (*ignore.Matcher, error) {
		p.mu.Lock()
		g.dirs[dir] = dr
		p.mu.Unlock()
		return dr.m, dr.err
	}
	fail := func(err error) (*ignore.Matcher, error) {
		if inside {
			err = readErr(name, err)
		}
		return store(&dirRules{err: fmt.Errorf("%w; folder skipped", err)})
	}
	var st fileStamp
	var err error
	if inside {
		st, err = p.statRootRepoFile(path.Join(rel, GitIgnoreFileName))
	} else {
		st, err = statRepoFile(name)
	}
	if err != nil {
		return fail(err)
	}
	p.mu.Lock()
	if dr := g.dirs[dir]; dr != nil && dr.stamp == st {
		m, err := dr.m, dr.err
		p.mu.Unlock()
		return m, err
	}
	p.mu.Unlock()
	dr := &dirRules{stamp: st}
	if !st.missing {
		var data []byte
		if inside {
			data, dr.stamp, err = p.readRootRepoFile(path.Join(rel, GitIgnoreFileName))
		} else {
			data, dr.stamp, err = readRepoFile(name)
		}
		if err != nil {
			return fail(err)
		}
		dr.m = ignore.ParseWith(data, g.opts)
	}
	return store(dr)
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
	base, err := p.loadBase()
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
	m, err := v.p.dirMatcher(d)
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
