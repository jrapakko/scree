// Package chunk splits markdown into heading-scoped pieces.
package chunk

import (
	"strings"
)

// MaxChars is the soft upper bound on a chunk's text. Sections longer than
// this are split at blank lines.
const MaxChars = 2000

// Chunk is one piece of a note.
type Chunk struct {
	// Heading is the trail of headings above the chunk, joined with " > ".
	// Empty for text before the first heading.
	Heading string
	// Line is the 1-based line in the source file where the chunk starts.
	Line int
	Text string
}

type section struct {
	heading string
	line    int
	lines   []string
}

// Split breaks markdown source into chunks, one or more per heading section.
// YAML front matter is skipped, and headings inside fenced code blocks are
// treated as text.
func Split(src string) []Chunk {
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	start := frontMatterEnd(lines)

	var (
		trail    []string // heading text per level, index 0 = level 1
		sections []section
		cur      = section{line: start + 1}
		fence    string
	)
	for i := start; i < len(lines); i++ {
		ln := lines[i]
		if fence != "" {
			if isFenceClose(ln, fence) {
				fence = ""
			}
			cur.lines = append(cur.lines, ln)
			continue
		}
		if f := fenceOpen(ln); f != "" {
			fence = f
			cur.lines = append(cur.lines, ln)
			continue
		}
		if level, text, ok := atxHeading(ln); ok {
			sections = append(sections, cur)
			if len(trail) >= level {
				trail = trail[:level-1]
			}
			for len(trail) < level-1 {
				trail = append(trail, "")
			}
			trail = append(trail, text)
			cur = section{heading: joinTrail(trail), line: i + 1, lines: []string{ln}}
			continue
		}
		cur.lines = append(cur.lines, ln)
	}
	sections = append(sections, cur)

	var out []Chunk
	for _, s := range sections {
		out = append(out, splitSection(s)...)
	}
	return out
}

func splitSection(s section) []Chunk {
	var (
		out   []Chunk
		buf   []string
		size  int
		first = s.line
	)
	flush := func(next int) {
		text := strings.TrimSpace(strings.Join(buf, "\n"))
		if text != "" && !isOnlyHeading(text) {
			out = append(out, Chunk{Heading: s.heading, Line: first, Text: text})
		}
		buf, size, first = nil, 0, next
	}
	// Paragraph boundaries inside fences are not split points.
	fence := ""
	for i, ln := range s.lines {
		if fence != "" {
			if isFenceClose(ln, fence) {
				fence = ""
			}
		} else if f := fenceOpen(ln); f != "" {
			fence = f
		} else if strings.TrimSpace(ln) == "" && size >= MaxChars {
			flush(s.line + i + 1)
			continue
		}
		buf = append(buf, ln)
		size += len(ln) + 1
	}
	flush(0)
	return out
}

func isOnlyHeading(text string) bool {
	if strings.Contains(text, "\n") {
		return false
	}
	_, _, ok := atxHeading(text)
	return ok
}

func joinTrail(trail []string) string {
	var parts []string
	for _, t := range trail {
		if t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, " > ")
}

// atxHeading parses "# Title" style headings, allowing up to three spaces of
// indentation and optional closing #s.
func atxHeading(ln string) (level int, text string, ok bool) {
	t := strings.TrimLeft(ln, " ")
	if len(ln)-len(t) > 3 {
		return 0, "", false
	}
	for level < len(t) && level < 7 && t[level] == '#' {
		level++
	}
	if level == 0 || level > 6 {
		return 0, "", false
	}
	rest := t[level:]
	if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
		return 0, "", false
	}
	rest = strings.TrimSpace(rest)
	if trimmed := strings.TrimRight(rest, "#"); trimmed != rest && (trimmed == "" || strings.HasSuffix(trimmed, " ")) {
		rest = strings.TrimSpace(trimmed)
	}
	return level, rest, true
}

func fenceOpen(ln string) string {
	t := strings.TrimLeft(ln, " ")
	if len(ln)-len(t) > 3 {
		return ""
	}
	for _, c := range []byte{'`', '~'} {
		n := 0
		for n < len(t) && t[n] == c {
			n++
		}
		if n >= 3 {
			return t[:n]
		}
	}
	return ""
}

func isFenceClose(ln, fence string) bool {
	t := strings.TrimSpace(ln)
	return strings.HasPrefix(t, fence) && strings.Trim(t, fence[:1]) == ""
}

// frontMatterEnd returns the index of the first line after YAML front matter,
// or 0 if there is none.
func frontMatterEnd(lines []string) int {
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return 0
	}
	for i := 1; i < len(lines); i++ {
		if t := strings.TrimSpace(lines[i]); t == "---" || t == "..." {
			return i + 1
		}
	}
	return 0
}
