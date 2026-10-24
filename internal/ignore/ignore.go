// Package ignore matches slash-separated paths against gitignore-style
// patterns, as used by a notes folder's .screeignore file.
//
// Matcher.Match differs from git in one way: a "!" rule can re-include a file
// inside an excluded folder, because each rule is checked against the file and
// every ancestor folder and the last matching rule wins. GitRules applies the
// same patterns the way git does.
//
// Patterns and paths are compared in Unicode NFC, so a pattern typed in one
// normalization matches a name stored in another. With Options.FoldCase they
// are also compared case-insensitively, for case-insensitive filesystems.
package ignore

import (
	"fmt"
	"path"
	"strings"

	"golang.org/x/text/unicode/norm"
)

type rule struct {
	segs    []string // pattern split on "/"; "**" matches any number of segments
	neg     bool     // a "!" rule re-includes what it matches
	dirOnly bool     // a trailing "/" matches folders only
}

// Matcher holds the parsed rules of one ignore file.
type Matcher struct {
	rules  []rule
	errs   []string
	hasNeg bool
	fold   bool
}

// Options configures ParseWith.
type Options struct {
	// FoldCase compares patterns and paths case-insensitively, as on a
	// case-insensitive filesystem.
	FoldCase bool
}

// Parse reads ignore patterns, one per line, comparing case exactly. Bad
// patterns are recorded in Errors and never match. A nil or empty input
// gives a matcher that ignores nothing.
func Parse(data []byte) *Matcher { return ParseWith(data, Options{}) }

// ParseWith is Parse with options.
func ParseWith(data []byte, o Options) *Matcher {
	m := &Matcher{fold: o.FoldCase}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		r, ok, err := parseLine(m.key(line))
		if err != nil {
			m.errs = append(m.errs, fmt.Sprintf("line %d: bad pattern %q", i+1, line))
			continue
		}
		if !ok {
			continue
		}
		m.rules = append(m.rules, r)
		m.hasNeg = m.hasNeg || r.neg
	}
	return m
}

// parseLine turns one line into a rule. ok is false for blank lines and
// comments.
func parseLine(line string) (r rule, ok bool, err error) {
	line = trimTrailingSpaces(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return r, false, nil
	}
	switch {
	case strings.HasPrefix(line, `\#`), strings.HasPrefix(line, `\!`):
		line = line[1:]
	case strings.HasPrefix(line, "!"):
		r.neg = true
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") {
		r.dirOnly = true
		line = strings.TrimSuffix(line, "/")
	}
	if strings.Contains(line, "/") {
		line = strings.TrimPrefix(line, "/")
		r.segs = strings.Split(line, "/")
	} else {
		r.segs = []string{"**", line}
	}
	for i, s := range r.segs {
		if s == "" {
			return r, false, path.ErrBadPattern
		}
		if s == "**" {
			continue
		}
		s = translateClass(s)
		if _, err := path.Match(s, ""); err != nil {
			return r, false, err
		}
		r.segs[i] = s
	}
	return r, true, nil
}

// trimTrailingSpaces drops trailing spaces unless they are escaped with a
// backslash.
func trimTrailingSpaces(s string) string {
	for strings.HasSuffix(s, " ") {
		t := strings.TrimSuffix(s, " ")
		// An odd number of backslashes before the space escapes it.
		n := len(t) - len(strings.TrimRight(t, `\`))
		if n%2 == 1 {
			return s
		}
		s = t
	}
	return s
}

// translateClass rewrites gitignore's negated class "[!...]" to the "[^...]"
// form that path.Match understands.
func translateClass(s string) string {
	if !strings.Contains(s, "[!") {
		return s
	}
	b := []byte(s)
	inClass := false
	for i := 0; i < len(b); i++ {
		switch {
		case b[i] == '\\':
			i++
		case !inClass && b[i] == '[':
			inClass = true
			if i+1 < len(b) && b[i+1] == '!' {
				b[i+1] = '^'
			}
			i++ // the first char of a class is never its end
		case inClass && b[i] == ']':
			inClass = false
		}
	}
	return string(b)
}

// Match reports whether the slash-separated path rel is ignored. isDir says
// whether rel is a folder, for patterns that end in "/". A rule applies to
// rel if it matches rel itself or any folder above it; the last rule that
// applies wins.
func (m *Matcher) Match(rel string, isDir bool) bool {
	if m == nil || len(m.rules) == 0 {
		return false
	}
	segs := strings.Split(m.key(rel), "/")
	for i := len(m.rules) - 1; i >= 0; i-- {
		r := m.rules[i]
		if r.applies(segs, isDir) {
			return !r.neg
		}
	}
	return false
}

func (r rule) applies(segs []string, isDir bool) bool {
	for n := 1; n <= len(segs); n++ {
		dir := n < len(segs) || isDir
		if r.dirOnly && !dir {
			continue
		}
		if matchSegs(r.segs, segs[:n], false) {
			return true
		}
	}
	return false
}

// matchSegs matches pattern segments against path segments. With git set, a
// trailing "**" matches one or more segments, as in git, so "a/**" matches
// what is inside a but not a itself; otherwise it also matches none.
func matchSegs(pat, name []string, git bool) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			if git && len(pat) == 1 {
				return len(name) > 0
			}
			for k := 0; k <= len(name); k++ {
				if matchSegs(pat[1:], name[k:], git) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], name[0]); !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

// MatchSelf applies the rules to the slash-separated path rel itself, not to
// the folders above it, with git's meaning of a trailing "**". matched reports
// whether any rule matched; negated reports whether the last one that did was
// a "!" rule. This is the per-file step of git's matching; see GitRules.
func (m *Matcher) MatchSelf(rel string, isDir bool) (matched, negated bool) {
	return m.matchSelf(strings.Split(rel, "/"), isDir)
}

func (m *Matcher) matchSelf(segs []string, isDir bool) (matched, negated bool) {
	if m == nil || len(m.rules) == 0 {
		return false, false
	}
	segs = m.keySegs(segs)
	for i := len(m.rules) - 1; i >= 0; i-- {
		r := m.rules[i]
		if r.dirOnly && !isDir {
			continue
		}
		if matchSegs(r.segs, segs, true) {
			return true, r.neg
		}
	}
	return false, false
}

// key puts s in the form rules are compared in: NFC, and lower case when
// folding. It returns s itself when nothing changes.
func (m *Matcher) key(s string) string {
	s = norm.NFC.String(s)
	if m.fold {
		s = strings.ToLower(s)
	}
	return s
}

// keySegs applies key to each segment, copying segs only if one changes.
func (m *Matcher) keySegs(segs []string) []string {
	var out []string
	for i, s := range segs {
		k := m.key(s)
		if k != s && out == nil {
			out = append(make([]string, 0, len(segs)), segs[:i]...)
		}
		if out != nil {
			out = append(out, k)
		}
	}
	if out == nil {
		return segs
	}
	return out
}

// Prunable reports whether everything under the folder dirRel is ignored, so
// a walk can skip it. It is false whenever any "!" rule exists, since that
// rule might re-include something inside.
func (m *Matcher) Prunable(dirRel string) bool {
	return m != nil && !m.hasNeg && m.Match(dirRel, true)
}

// Errors lists the patterns that could not be parsed, as "line N: ..." text.
func (m *Matcher) Errors() []string {
	if m == nil {
		return nil
	}
	return m.errs
}
