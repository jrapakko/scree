// Package chunk splits markdown into heading-scoped pieces.
package chunk

import (
	"strings"
	"unicode/utf8"
)

const (
	// TargetChars is the size at which a chunk is closed at the next blank
	// line outside a code fence.
	TargetChars = 2500
	// MaxChars is the hard upper bound on a chunk's text in bytes. A chunk is
	// closed before a line that would push it over, even inside a code fence,
	// and longer lines are cut into pieces.
	MaxChars = 3500
	// MaxHeadingChars caps each heading's text in bytes, so the heading trail
	// stays short. Longer headings are cut and end in "…".
	MaxHeadingChars = 120
)

// Chunk is one piece of a note.
type Chunk struct {
	// Heading is the trail of headings above the chunk, joined with " > ".
	// Empty for text before the first heading.
	Heading string
	// Line and EndLine are the 1-based numbers of the first and last
	// non-blank source lines whose text is in Text. A piece cut from an
	// overlong line has Line == EndLine, the line it came from.
	Line, EndLine int
	Text          string
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
		out  []Chunk
		buf  []string
		nums []int // the source line number of each buf entry
		size int
	)
	flush := func() {
		text := strings.TrimSpace(strings.Join(buf, "\n"))
		if text != "" && !isOnlyHeading(text) {
			// The range is taken from the line numbers kept with buf, never
			// by counting newlines in the trimmed text.
			first, last := 0, 0
			for i, ln := range buf {
				if strings.TrimSpace(ln) != "" {
					if first == 0 {
						first = nums[i]
					}
					last = nums[i]
				}
			}
			out = append(out, Chunk{Heading: s.heading, Line: first, EndLine: last, Text: text})
		}
		buf, nums, size = nil, nil, 0
	}
	// Paragraph boundaries inside fences are not split points.
	fence := ""
	for i, ln := range s.lines {
		lineNo := s.line + i
		if fence != "" {
			if isFenceClose(ln, fence) {
				fence = ""
			}
		} else if f := fenceOpen(ln); f != "" {
			fence = f
		} else if strings.TrimSpace(ln) == "" && size >= TargetChars {
			flush()
			continue
		}
		if len(ln) > MaxChars {
			// Cut an overlong line into pieces of its own; the last piece
			// starts the next chunk.
			if size > 0 {
				flush()
			}
			for len(ln) > MaxChars {
				cut := runeCut(ln, MaxChars)
				if piece := ln[:cut]; strings.TrimSpace(piece) != "" {
					out = append(out, Chunk{Heading: s.heading, Line: lineNo, EndLine: lineNo, Text: piece})
				}
				ln = ln[cut:]
			}
		}
		if size > 0 && size+len(ln)+1 > MaxChars {
			flush()
		}
		buf = append(buf, ln)
		nums = append(nums, lineNo)
		size += len(ln) + 1
	}
	flush()
	return out
}

// runeCut returns the largest index <= n that starts a rune in s, so s[:cut]
// does not end mid-rune. It falls back to n for input with no rune starts.
func runeCut(s string, n int) int {
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if cut == 0 {
		return n
	}
	return cut
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
	if len(rest) > MaxHeadingChars {
		rest = rest[:runeCut(rest, MaxHeadingChars)] + "…"
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
