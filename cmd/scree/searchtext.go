package main

import (
	"context"
	"errors"
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

// palette holds the SGR codes the text view colours with: the word
// headings, the matched words, the location label and the line numbers.
// An empty code leaves that part plain; the zero palette colours nothing.
type palette struct{ word, match, link, lineno string }

// defaultPalette is the palette on a terminal before SCREE_COLORS.
var defaultPalette = palette{word: "1", match: "1;33", link: "36", lineno: "2"}

// parseColors returns defaultPalette with the settings in s, which is
// SCREE_COLORS: key=code items split by ":", the keys word, match, link
// and lineno, each code digits and ";" only (so no other escape can be
// set), "" for none. Errors name the key, never the code.
func parseColors(s string) (palette, error) {
	p := defaultPalette
	for _, item := range strings.Split(s, ":") {
		if item == "" {
			continue
		}
		key, code, ok := strings.Cut(item, "=")
		var dst *string
		switch key {
		case "word":
			dst = &p.word
		case "match":
			dst = &p.match
		case "link":
			dst = &p.link
		case "lineno":
			dst = &p.lineno
		default:
			if isIdent(key) {
				return palette{}, fmt.Errorf("SCREE_COLORS: unknown key %s (want word, match, link or lineno)", key)
			}
			return palette{}, errors.New("SCREE_COLORS: expected key=code items split by ':', keys word, match, link or lineno")
		}
		if !ok {
			return palette{}, fmt.Errorf("SCREE_COLORS: %s has no =code", key)
		}
		if strings.Trim(code, "0123456789;") != "" {
			return palette{}, fmt.Errorf("SCREE_COLORS: %s: a code holds only digits and ';', such as 1;33", key)
		}
		*dst = code
	}
	return p, nil
}

// paint wraps s in the SGR code, unless code or s is empty.
func paint(code, s string) string {
	if code == "" || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[m"
}

// paintSpans returns s[lo:hi] with each part of it inside spans (byte
// ranges of s, in any order, possibly overlapping) painted in code.
func paintSpans(s string, spans [][2]int, lo, hi int, code string) string {
	if code == "" || len(spans) == 0 {
		return s[lo:hi]
	}
	spans = slices.Clone(spans)
	slices.SortFunc(spans, func(a, b [2]int) int { return a[0] - b[0] })
	var b strings.Builder
	at := lo
	for _, sp := range spans {
		start, end := max(sp[0], at), min(sp[1], hi)
		if start >= end {
			continue
		}
		b.WriteString(s[at:start])
		b.WriteString(paint(code, s[start:end]))
		at = end
	}
	b.WriteString(s[at:hi])
	return b.String()
}

// writeText writes hits as the text view: for each query word (split and
// trimmed as keyword search splits the query), the hits whose text, path
// or heading holds it as keyword search matches it, each hit under the
// first such word only; then the hits with no word, and the stale ones
// (whose current lines cannot be read), under "other hits:". Each hit is
// an excerpt indented two spaces (its heading line, then its current lines
// that hold the word), a tab-indented ">path:start-end" location, and its
// current lines in full, numbered after two tabs and ">>". With links the
// location is an OSC 8 link to its file. pal colours the headings, the
// query words wherever they match in the excerpt and numbered lines, the
// location label and the line numbers.
func writeText(ctx context.Context, w io.Writer, ix *index.Indexer, query string, hits []store.Hit, note string, links bool, pal palette) error {
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
	// to a window and made safe (row[i][j] for hits[i]'s line j), and, to
	// colour matches, each heading and numbered line as shown.
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
				n = add(lineText(l, piece))
				seen[key] = n
			}
			row[i] = append(row[i], n)
		}
	}
	head := make([]string, len(hits))
	headAt := make([]int, len(hits)) // index in lines[i] of the heading line, or -1
	shown := make([][]string, len(hits))
	more := make([]int, len(hits))
	for i, h := range hits {
		head[i], headAt[i] = headLine(h, lines[i])
		shown[i], more[i] = shownLines(lines[i])
		if pal.match != "" {
			add(head[i])
			for _, l := range shown[i] {
				add(l)
			}
		}
	}
	matches, err := matchTexts(ctx, words, texts)
	if err != nil {
		// Show the hits under no word rather than nothing.
		matches = make([][]store.TextMatch, len(words))
		note = strings.TrimSpace("word matching failed: " + err.Error() + "\n" + note)
	}
	at := make([]map[int]int, len(words)) // text row -> match offset
	spans := map[int][][2]int{}           // text row -> every word's matches
	for wi := range words {
		at[wi] = map[int]int{}
		for _, m := range matches[wi] {
			at[wi][m.Row] = m.At
			spans[m.Row] = append(spans[m.Row], m.Spans...)
		}
	}
	// spansOf returns where the query words match s, a text given to the
	// matcher.
	spansOf := func(s string) [][2]int {
		if n, ok := seenText[s]; ok {
			return spans[n]
		}
		return nil
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
		hj := headAt[i]
		if head[i] != "" {
			b.WriteString("  " + cutLine(head[i], 0, spansOf(head[i]), pal.match) + "\n")
		}
		if g := group[i]; g >= 0 {
			var shown []string
			for j, n := range row[i] {
				if a, ok := at[g][n]; ok && j != hj {
					lead := textLine(lines[i][j])
					lead = lead[:len(lead)-len(strings.TrimLeft(lead, " "))]
					shown = append(shown, "  "+lead+cutLine(texts[n], a, spansOf(texts[n]), pal.match))
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
			b.WriteString("\t>" + paint(pal.link, safe(h.Path)) + " (stale)\n")
			return
		}
		l := safe(h.Path) + ":" + strconv.Itoa(h.Line)
		if end := h.Line + len(lines[i]) - 1; end > h.Line {
			l += "-" + strconv.Itoa(end)
		}
		l = paint(pal.link, l)
		if links {
			l = osc8(l, filepath.Join(ix.Root(), filepath.FromSlash(h.Path)))
		}
		b.WriteString("\t>" + l + "\n")
		writeLines(&b, shown[i], more[i], len(lines[i]), h.Line, pal.lineno, func(s string) string {
			return paintSpans(s, spansOf(s), 0, len(s), pal.match)
		})
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
				b.WriteString(paint(pal.word, head+":") + "\n")
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
// else the last heading in its trail (with -1), made safe by textLine;
// "" for none. The caller cuts it to maxLineChars.
func headLine(h store.Hit, lines []string) (string, int) {
	if lines == nil {
		lines = strings.Split(h.Text, "\n")
	}
	for j, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if t := strings.TrimLeft(l, " "); len(l)-len(t) <= 3 && strings.HasPrefix(t, "#") {
			return strings.TrimLeft(textLine(l), " "), j
		}
		break
	}
	trail := strings.Split(h.Heading, " > ")
	return strings.TrimLeft(textLine(trail[len(trail)-1]), " "), -1
}

// shownLines returns lines as writeLines shows them, made safe with
// trailing space trimmed; past maxHitBytes of text the rest is cut, and
// more is the count of lines left out.
func shownLines(lines []string) (shown []string, more int) {
	budget := maxHitBytes
	for j, l := range lines {
		if budget <= 0 {
			return shown, len(lines) - j
		}
		if len(l) > budget {
			end := budget
			for end > 0 && !utf8.RuneStart(l[end]) {
				end--
			}
			l = l[:end] + "…"
		}
		budget -= len(l)
		shown = append(shown, strings.TrimRightFunc(safe(l), unicode.IsSpace))
	}
	return shown, 0
}

// writeLines writes shown (from shownLines of n lines), numbered from
// first and right-aligned, each after two tabs and ">>", the ">>" and
// number painted in code and the text passed through mark; a blank line
// is its number alone. Then a count of the more lines left out.
func writeLines(b *strings.Builder, shown []string, more, n, first int, code string, mark func(string) string) {
	width := len(strconv.Itoa(first + n - 1))
	for j, l := range shown {
		b.WriteString("\t\t" + paint(code, fmt.Sprintf(">>%*d", width, first+j)))
		if l != "" {
			b.WriteString("  " + mark(l))
		}
		b.WriteString("\n")
	}
	if more > 0 {
		fmt.Fprintf(b, "\t\t>>… %d more lines\n", more)
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
// starts matchLead characters before it. The parts of s inside spans are
// painted in code.
func cutLine(s string, at int, spans [][2]int, code string) string {
	start, end := cutBounds(s, at)
	out := paintSpans(s, spans, start, end, code)
	if start > 0 {
		out = "…" + out
	}
	if end < len(s) {
		out += "…"
	}
	return out
}

// cutBounds returns the byte range of s that cutLine keeps.
func cutBounds(s string, at int) (start, end int) {
	r := []rune(s)
	if len(r) <= maxLineChars {
		return 0, len(s)
	}
	first, budget := 0, maxLineChars
	if a := utf8.RuneCountInString(s[:min(max(at, 0), len(s))]); a > maxLineChars-matchLead {
		first, budget = a-matchLead, maxLineChars-1
	}
	last := len(r)
	if last-first > budget {
		last = first + budget - 1
	}
	return len(string(r[:first])), len(string(r[:last]))
}
