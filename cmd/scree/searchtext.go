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

	"github.com/jrapakko/scree/internal/index"
	"github.com/jrapakko/scree/internal/mcpserver"
	"github.com/jrapakko/scree/internal/store"
	"github.com/jrapakko/scree/internal/textsafe"
)

// Limits on the text view: lines listed per word, characters per line, how
// much of a long line is kept before a match past the cut, and the bytes of
// one line matched against the query words.
const (
	maxWordLines  = 10
	maxLineChars  = 160
	matchLead     = 40
	maxMatchBytes = 4096
)

// stdoutTerminal reports whether stdout is a terminal; tests replace it.
var stdoutTerminal = func() bool { return term.IsTerminal(int(os.Stdout.Fd())) }

// linksOn reports whether the text view writes OSC 8 file links: only on a
// terminal, and not with NO_COLOR set or TERM=dumb.
func linksOn() bool {
	return stdoutTerminal() && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
}

// writeText writes hits as the text view: for each query word (split and
// trimmed as keyword search splits the query), each current line of a hit
// that holds the word as keyword search matches it, or one entry for a hit
// whose path or heading holds it; then the hits with no word, and the stale
// ones, under "other hits:". With links, each location is an OSC 8 link to
// its file.
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
	// texts holds each distinct (path, line) once, cut to a window and
	// made safe, then each hit's path, then its heading; row[i][j] is the
	// text of hits[i]'s line j, and path and head index into texts (-1 for
	// a hit with no lines).
	var texts []string
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
	path, head := make([]int, len(hits)), make([]int, len(hits))
	for i, h := range hits {
		path[i], head[i] = -1, -1
		if lines[i] != nil {
			path[i] = len(texts)
			texts = append(texts, safe(h.Path))
		}
	}
	for i, h := range hits {
		if lines[i] != nil {
			head[i] = len(texts)
			texts = append(texts, safe(h.Heading))
		}
	}
	matches, err := matchTexts(ctx, words, texts)
	if err != nil {
		// Show the hits without their lines rather than nothing.
		matches = make([][]store.TextMatch, len(words))
		note = strings.TrimSpace("line matching failed: " + err.Error() + "\n" + note)
	}

	label := func(i int, loc string) string {
		l := safe(hits[i].Path) + ":" + loc
		if links {
			l = osc8(l, filepath.Join(ix.Root(), filepath.FromSlash(hits[i].Path)))
		}
		return l
	}
	span := func(i int) string {
		return strconv.Itoa(hits[i].Line) + "-" + strconv.Itoa(hits[i].Line+len(lines[i])-1)
	}
	var b strings.Builder
	listed := make([]bool, len(hits))
	for wi, word := range words {
		at := map[int]int{}
		shown := map[int]bool{} // lines listed for this word
		for _, m := range matches[wi] {
			at[m.Row] = m.At
		}
		var entries []string
		for i, h := range hits {
			if lines[i] == nil {
				continue
			}
			found := false
			for j, n := range row[i] {
				a, ok := at[n]
				if !ok {
					continue
				}
				found = true
				if !shown[n] {
					shown[n] = true
					entries = append(entries, label(i, strconv.Itoa(h.Line+j))+": "+cutLine(texts[n], a))
				}
			}
			if _, ok := at[path[i]]; !found && ok {
				found = true
				entries = append(entries, label(i, span(i))+" (in path)")
			} else if _, ok := at[head[i]]; !found && ok {
				found = true
				entries = append(entries, label(i, span(i))+" (in heading)")
			}
			listed[i] = listed[i] || found
		}
		if len(entries) == 0 {
			continue
		}
		b.WriteString(safe(word) + ":\n")
		for _, e := range entries[:min(len(entries), maxWordLines)] {
			b.WriteString("\t>" + e + "\n")
		}
		if n := len(entries) - maxWordLines; n > 0 {
			fmt.Fprintf(&b, "\t>… %d more lines\n", n)
		}
	}
	var other []string
	for i, h := range hits {
		switch {
		case lines[i] == nil:
			other = append(other, safe(h.Path)+" (stale)")
		case !listed[i]:
			e := label(i, span(i))
			if hd := safe(h.Heading); hd != "" {
				e += " " + hd
			}
			other = append(other, e)
		}
	}
	if len(other) > 0 {
		b.WriteString("other hits:\n")
		for _, e := range other {
			b.WriteString("\t>" + e + "\n")
		}
	}
	if note != "" {
		b.WriteString(safe(note) + "\n")
	}
	_, err = io.WriteString(w, b.String())
	return err
}

// matchTexts is store.MatchTexts; tests replace it.
var matchTexts = store.MatchTexts

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
