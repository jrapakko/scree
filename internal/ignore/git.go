package ignore

import "strings"

// GitRules holds the patterns that apply in one git work tree.
type GitRules struct {
	// Base holds core.excludesFile, then .git/info/exclude; later entries
	// take precedence. They are matched against the top-relative path.
	Base []*Matcher
	// Dir returns the matcher for the .gitignore in the top-relative folder
	// dir ("" is the top), or nil if there is none. A nil Dir means no
	// .gitignore files.
	Dir func(dir string) *Matcher
}

// Ignored reports whether the top-relative path rel is excluded the way git
// decides it: each ancestor folder is checked first, and an excluded folder
// excludes everything under it (no re-include). Within one path, the deepest
// .gitignore with a matching rule decides, then info/exclude, then the global
// file. isDir says whether rel is a folder, for patterns that end in "/".
func (g *GitRules) Ignored(rel string, isDir bool) bool {
	if g == nil || rel == "" {
		return false
	}
	segs := strings.Split(rel, "/")
	for n := 1; n <= len(segs); n++ {
		if g.decide(segs[:n], n < len(segs) || isDir) {
			return true
		}
	}
	return false
}

// IgnoredDir is Ignored(rel, true); a true result means a walk may skip rel.
func (g *GitRules) IgnoredDir(rel string) bool {
	return g.Ignored(rel, true)
}

// decide applies the rules to the path p itself, ignoring its ancestors.
func (g *GitRules) decide(p []string, isDir bool) bool {
	if g.Dir != nil {
		for k := len(p) - 1; k >= 0; k-- {
			m := g.Dir(strings.Join(p[:k], "/"))
			if m == nil {
				continue
			}
			if matched, neg := m.matchSelf(p[k:], isDir); matched {
				return !neg
			}
		}
	}
	for i := len(g.Base) - 1; i >= 0; i-- {
		if matched, neg := g.Base[i].matchSelf(p, isDir); matched {
			return !neg
		}
	}
	return false
}
