// Package ignore matches slash-separated paths against gitignore-style
// patterns, as used by a notes folder's .screeignore file.
//
// It differs from git in one way: a "!" rule can re-include a file inside an
// excluded folder, because each rule is checked against the file and every
// ancestor folder and the last matching rule wins.
package ignore

import (
	"fmt"
	"path"
	"strings"
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
}

// Parse reads ignore patterns, one per line. Bad patterns are recorded in
// Errors and never match. A nil or empty input gives a matcher that ignores
// nothing.
func Parse(data []byte) *Matcher {
	m := &Matcher{}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		r, ok, err := parseLine(line)
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
	segs := strings.Split(rel, "/")
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
		if matchSegs(r.segs, segs[:n]) {
			return true
		}
	}
	return false
}

func matchSegs(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for k := 0; k <= len(name); k++ {
				if matchSegs(pat[1:], name[k:]) {
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
