package policy

import (
	"bytes"
	"strings"
)

// frontMatterIgnore reports whether a note opts out of indexing with a
// top-level `scree: ignore` line in its YAML front matter. The front matter
// must start on the first line with "---" and end with "---" or "...".
func frontMatterIgnore(src []byte) bool {
	src = bytes.TrimPrefix(src, []byte("\ufeff"))
	first := true
	ignore := false
	for len(src) > 0 {
		var line []byte
		if i := bytes.IndexByte(src, '\n'); i >= 0 {
			line, src = src[:i], src[i+1:]
		} else {
			line, src = src, nil
		}
		s := strings.TrimRight(string(line), " \t\r")
		if first {
			if s != "---" {
				return false
			}
			first = false
			continue
		}
		if s == "---" || s == "..." {
			return ignore
		}
		if isIgnoreLine(s) {
			ignore = true
		}
	}
	return false // unclosed front matter is just text
}

// isIgnoreLine matches `scree: ignore` with an optionally quoted value and an
// optional trailing comment, at the top level only.
func isIgnoreLine(s string) bool {
	rest, ok := strings.CutPrefix(s, "scree:")
	if !ok || rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
		return false
	}
	rest = strings.TrimLeft(rest, " \t")
	if i := strings.Index(rest, "#"); i >= 0 {
		// A YAML comment needs whitespace before the '#'.
		if i == 0 || (rest[i-1] != ' ' && rest[i-1] != '\t') {
			return false
		}
		rest = rest[:i]
	}
	switch strings.TrimRight(rest, " \t") {
	case "ignore", "'ignore'", `"ignore"`:
		return true
	}
	return false
}
