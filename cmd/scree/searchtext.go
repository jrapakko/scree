package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/jrapakko/scree/internal/chunk"
	"github.com/jrapakko/scree/internal/index"
	"github.com/jrapakko/scree/internal/mcpserver"
	"github.com/jrapakko/scree/internal/store"
	"github.com/jrapakko/scree/internal/textsafe"
)

// Limits on the text view: matching lines shown per hit, characters per
// shown line, how much of a long line is kept before a match past the cut,
// the bytes of one line matched against the query words, the bytes of a
// hit's lines listed under its location, the leading spaces kept on an
// excerpt line, and the longest run of spaces kept inside one.
const (
	maxHitLines   = 10
	maxLineChars  = 160
	matchLead     = 40
	maxMatchBytes = 4096
	maxHitBytes   = chunk.MaxChars
	maxLeadSpace  = 4
	maxSpaceRun   = 3
)

// stdoutTerminal reports whether stdout is a terminal; tests replace it.
var stdoutTerminal = func() bool { return term.IsTerminal(int(os.Stdout.Fd())) }

// linksOn reports whether the text view writes OSC 8 file links: only on a
// terminal, and not with NO_COLOR set or TERM=dumb.
func linksOn() bool {
	return stdoutTerminal() && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
}

// writeText writes hits as the text view: for each query word (split and
// trimmed as keyword search splits the query), the hits whose text, path
// or heading holds it as keyword search matches it, each hit under the
// first such word only; then the hits with no word, and the stale ones
// (whose current lines cannot be read), under "other hits:". Each hit is
// an excerpt indented two spaces (its heading line, then its current lines
// that hold the word), a tab-indented ">path:start-end" location, and its
// current lines in full, numbered after two tabs and ">>". With links the
// location is an OSC 8 link to its file.
func writeText(ctx context.Context, w io.Writer, ix *index.Indexer, query string, hits []store.Hit, note string, links bool) error {
	if len(hits) == 0 {
		_, err := fmt.Fprintln(w, mcpserver.FormatHits(nil, note))
		return err
	}
	var words []string
	for _, word := range index.KeywordWords(query) {
		if !slices.Contains(words, word) {
			words = append(words, word)
		}
	}
	lines := ix.Lines(hits)
	// texts holds each distinct text matched once: each hit's whole text,
	// path and heading (whole[i]), then each distinct (path, line), cut
	// to a window and made safe (row[i][j] for hits[i]'s line j).
	var texts []string
	seenText := map[string]int{}
	add := func(s string) int {
		n, ok := seenText[s]
		if !ok {
			n = len(texts)
			seenText[s] = n
			texts = append(texts, s)
		}
		return n
	}
	whole := make([][3]int, len(hits))
	for i, h := range hits {
		whole[i] = [3]int{add(strings.Join(strings.Fields(safe(h.Text)), " ")), add(safe(h.Path)), add(safe(h.Heading))}
	}
	type lineKey struct {
		path string
		line int
	}
	seen := map[lineKey]int{}
	row := make([][]int, len(hits))
	for i, h := range hits {
		chunk := strings.Split(h.Text, "\n")
		for j, l := range lines[i] {
			key := lineKey{h.Path, h.Line + j}
			n, ok := seen[key]
			if !ok {
				piece := ""
				if j < len(chunk) {
					piece = chunk[j]
				}
				n = len(texts)
				seen[key] = n
				texts = append(texts, lineText(l, piece))
			}
			row[i] = append(row[i], n)
		}
	}
	matches, err := matchTexts(ctx, words, texts)
	if err != nil {
		// Show the hits under no word rather than nothing.
		matches = make([][]store.TextMatch, len(words))
		note = strings.TrimSpace("word matching failed: " + err.Error() + "\n" + note)
	}
	at := make([]map[int]int, len(words)) // text row -> match offset
	for wi := range words {
		at[wi] = map[int]int{}
		for _, m := range matches[wi] {
			at[wi][m.Row] = m.At
		}
	}
	group := make([]int, len(hits)) // index into words, or -1
	for i := range hits {
		group[i] = -1
		if lines[i] == nil {
			continue
		}
		for wi := range words {
			if slices.ContainsFunc(whole[i][:], func(n int) bool { _, ok := at[wi][n]; return ok }) {
				group[i] = wi
				break
			}
		}
	}

	var b strings.Builder
	entry := func(i int) {
		h := hits[i]
		hl, hj := headLine(h, lines[i])
		if hl != "" {
			b.WriteString("  " + hl + "\n")
		}
		if g := group[i]; g >= 0 {
			var shown []string
			for j, n := range row[i] {
				if a, ok := at[g][n]; ok && j != hj {
					lead := textLine(lines[i][j])
					lead = lead[:len(lead)-len(strings.TrimLeft(lead, " "))]
					shown = append(shown, "  "+lead+cutLine(texts[n], a))
				}
			}
			for _, l := range shown[:min(len(shown), maxHitLines)] {
				b.WriteString(l + "\n")
			}
			if n := len(shown) - maxHitLines; n > 0 {
				fmt.Fprintf(&b, "  … %d more matching lines\n", n)
			}
		}
		if lines[i] == nil {
			b.WriteString("\t>" + safe(h.Path) + " (stale)\n")
			return
		}
		l := safe(h.Path) + ":" + strconv.Itoa(h.Line)
		if end := h.Line + len(lines[i]) - 1; end > h.Line {
			l += "-" + strconv.Itoa(end)
		}
		if links {
			l = osc8(l, filepath.Join(ix.Root(), filepath.FromSlash(h.Path)))
		}
		b.WriteString("\t>" + l + "\n")
		writeLines(&b, lines[i], h.Line)
	}
	section := func(head string, g int) {
		first := true
		for i := range hits {
			if group[i] != g {
				continue
			}
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			if first {
				b.WriteString(head + ":\n")
				first = false
			}
			entry(i)
		}
	}
	for wi, word := range words {
		section(safe(word), wi)
	}
	section("other hits", -1)
	if note != "" {
		b.WriteString(safe(note) + "\n")
	}
	_, err = io.WriteString(w, b.String())
	return err
}

// matchTexts is store.MatchTexts; tests replace it.
var matchTexts = store.MatchTexts

// headLine returns the excerpt line for h's heading: its first non-blank
// line when that is a markdown heading (with that line's index in lines),
// else the last heading in its trail (with -1), made safe by textLine and
// cut to maxLineChars; "" for none.
func headLine(h store.Hit, lines []string) (string, int) {
	if lines == nil {
		lines = strings.Split(h.Text, "\n")
	}
	for j, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if t := strings.TrimLeft(l, " "); len(l)-len(t) <= 3 && strings.HasPrefix(t, "#") {
			return cutLine(strings.TrimLeft(textLine(l), " "), 0), j
		}
		break
	}
	trail := strings.Split(h.Heading, " > ")
	return cutLine(strings.TrimLeft(textLine(trail[len(trail)-1]), " "), 0), -1
}

// writeLines writes lines, numbered from first and right-aligned, each
// after two tabs and ">>" and made safe; a blank line is its number alone.
// Past maxHitBytes of text the rest is cut, with a count of lines left.
func writeLines(b *strings.Builder, lines []string, first int) {
	width := len(strconv.Itoa(first + len(lines) - 1))
	budget := maxHitBytes
	for j, l := range lines {
		if budget <= 0 {
			fmt.Fprintf(b, "\t\t>>… %d more lines\n", len(lines)-j)
			return
		}
		if len(l) > budget {
			end := budget
			for end > 0 && !utf8.RuneStart(l[end]) {
				end--
			}
			l = l[:end] + "…"
		}
		budget -= len(l)
		fmt.Fprintf(b, "\t\t>>%*d", width, first+j)
		if l = strings.TrimRightFunc(safe(l), unicode.IsSpace); l != "" {
			b.WriteString("  " + l)
		}
		b.WriteString("\n")
	}
}

// lineText returns a note line as the text view matches and shows it: made
// safe with each run of whitespace one space, and a line over maxMatchBytes
// cut to a window of that size starting where piece (the hit's chunk text
// for that line) does, or at the start, with "…" at each cut.
func lineText(l, piece string) string {
	if len(l) > maxMatchBytes {
		start := 0
		if p := strings.TrimSpace(piece); p != "" {
			start = max(strings.Index(l, p[:min(len(p), 64)]), 0)
		}
		for start > 0 && !utf8.RuneStart(l[start]) {
			start--
		}
		end := min(start+maxMatchBytes, len(l))
		for end < len(l) && !utf8.RuneStart(l[end]) {
			end--
		}
		w := l[start:end]
		if start > 0 {
			w = "… " + w
		}
		if end < len(l) {
			w += " …"
		}
		l = w
	}
	return strings.Join(strings.Fields(safe(l)), " ")
}

// textLine makes one note line safe to show in the excerpt: controls
// (textsafe.Strip) and format characters (Cf) dropped, each tab four
// spaces and any other whitespace one, its leading spaces cut to
// maxLeadSpace, any longer run of spaces inside it made one, and trailing
// space trimmed. So no text line starts with a tab or reaches the depth of
// a location line, and none can be padded to wrap a forged one onto its
// own terminal row.
func textLine(l string) string {
	l = strings.TrimRightFunc(strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r):
			return ' '
		case unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, strings.ReplaceAll(textsafe.Strip(l), "\t", "    ")), unicode.IsSpace)
	body := strings.TrimLeft(l, " ")
	if body == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString(strings.Repeat(" ", min(len(l)-len(body), maxLeadSpace)))
	run := 0
	for _, r := range body {
		if r == ' ' {
			run++
			continue
		}
		if run > maxSpaceRun {
			run = 1
		}
		b.WriteString(strings.Repeat(" ", run))
		run = 0
		b.WriteRune(r)
	}
	return b.String()
}

// safe makes note-derived text fit for one line on a terminal: control
// characters (textsafe.Strip) and format characters (Cf, which holds the
// bidi controls, zero-width characters and tag characters) dropped, tabs
// and newlines made spaces.
func safe(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n':
			return ' '
		case unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, textsafe.Strip(s))
}

// osc8 wraps label in an OSC 8 hyperlink to the file abs. The URI is
// percent-encoded, so it holds no control character.
func osc8(label, abs string) string {
	uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}).String()
	return "\x1b]8;;" + uri + "\x1b\\" + label + "\x1b]8;;\x1b\\"
}

// cutLine shortens s to maxLineChars characters, marking each cut with "…".
// When the match at byte at would fall near or past the end, the kept part
// starts matchLead characters before it.
func cutLine(s string, at int) string {
	r := []rune(s)
	if len(r) <= maxLineChars {
		return s
	}
	start, prefix := 0, ""
	if a := utf8.RuneCountInString(s[:min(max(at, 0), len(s))]); a > maxLineChars-matchLead {
		start, prefix = a-matchLead, "…"
	}
	rest := r[start:]
	if budget := maxLineChars - utf8.RuneCountInString(prefix); len(rest) > budget {
		return prefix + string(rest[:budget-1]) + "…"
	}
	return prefix + string(rest)
}
