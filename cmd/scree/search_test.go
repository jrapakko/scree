package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jrapakko/scree/internal/store"
)

// searchFixture builds an index of one note without the API and returns
// the folder and the index file.
func searchFixture(t *testing.T) (notes, db string) {
	t.Helper()
	notes = filepath.Join(tempDir(t), "notes")
	mkdir(t, notes)
	src := "# Ops\n\nrun PURGE_OLD_RECORDS nightly via the PageFetcher\n"
	if err := os.WriteFile(filepath.Join(notes, "ops.md"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	db = filepath.Join(tempDir(t), "index.db")
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	root, _ := filepath.EvalSymlinks(notes)
	if err := s.CheckRoot(root); err != nil {
		t.Fatal(err)
	}
	chunks := []store.Chunk{{Path: "ops.md", Heading: "Ops", Line: 1, EndLine: 3, Text: "# Ops\n\nrun PURGE_OLD_RECORDS nightly via the PageFetcher"}}
	if err := s.ReplaceFile(context.Background(), "ops.md", "h", 1, nil, chunks, [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateVocab(context.Background(), []store.VocabFile{{Path: "app.py", Hash: "h", Names: []string{"codeonlyhelper", "pagefetcher"}}}, nil); err != nil {
		t.Fatal(err)
	}
	return notes, db
}

// TestCLISearchKeywordJSON: scree search -mode keyword -format json needs
// no API key and prints one object per hit with what an eval scorer needs;
// doctor reports the keyword index.
func TestCLISearchKeywordJSON(t *testing.T) {
	serveEnv(t)
	notes, db := searchFixture(t)
	var runErr error
	out := captureStdout(t, func() {
		runErr = runSearch(context.Background(), []string{"-notes", notes, "-db", db, "-mode", "keyword", "-format", "json", "purge_old_records"})
	})
	if runErr != nil {
		t.Fatal(runErr)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 {
		t.Fatalf("output %q", out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{"n": 1.0, "mode": "keyword", "path": "ops.md", "start": 1.0, "end": 3.0, "heading": "Ops", "score": nil, "match": "keyword", "stale": false} {
		if v, ok := got[k]; !ok || v != want {
			t.Errorf("%s = %v (present %v), want %v", k, v, ok, want)
		}
	}

	if err := runSearch(context.Background(), []string{"-notes", notes, "-db", db, "-mode", "nope", "x"}); err == nil || !strings.Contains(err.Error(), "mode must be") {
		t.Errorf("bad mode: %v", err)
	}
	if err := runSearch(context.Background(), []string{"-notes", notes, "-db", db, "-format", "yaml", "x"}); err == nil {
		t.Error("bad format accepted")
	}

	out = captureStdout(t, func() {
		runDoctor(context.Background(), []string{"-v", "-offline", "-notes", notes, "-db", db})
	})
	for _, want := range []string{"ok   keyword index: 1 chunks", "ok   keyword index integrity check",
		"ok   code vocabulary: 2 names from 1 manifests and source files, 1 also in the notes",
		`  vocabulary sample (names also in the notes): "pagefetcher"` + "\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor lacks %q:\n%s", want, out)
		}
	}
	// A name found only in code is never printed.
	if strings.Contains(out, "codeonlyhelper") {
		t.Errorf("doctor prints a code-only name:\n%s", out)
	}
}

// TestCLISearchDefaultsToAuto: with no -mode, a plain query runs semantic
// and an identifier query hybrid, as the JSON mode field and the XML
// note say. A name from the code vocabulary gives hybrid, whether or not
// the notes hold it.
func TestCLISearchDefaultsToAuto(t *testing.T) {
	serveEnv(t)
	notes, db := searchFixture(t)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"index":0,"embedding":[1,0]}]}`)
	}))
	defer api.Close()
	t.Setenv("SCREE_EMBED_URL", api.URL)
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	t.Setenv("SCREE_ZDR", "0")
	for q, want := range map[string]string{"nightly run": "semantic", "purge_old_records": "hybrid", "does the PageFetcher purge?": "hybrid",
		"does the codeonlyhelper purge?": "hybrid"} {
		var runErr error
		out := captureStdout(t, func() {
			runErr = runSearch(context.Background(), []string{"-notes", notes, "-db", db, "-format", "json", q})
		})
		var got struct{ Mode, Match string }
		if runErr != nil || json.Unmarshal([]byte(out), &got) != nil || got.Mode != want {
			t.Errorf("%q: mode %q (%v, %q), want %s", q, got.Mode, runErr, out, want)
		}
		out = captureStdout(t, func() {
			runErr = runSearch(context.Background(), []string{"-notes", notes, "-db", db, q})
		})
		if runErr != nil || !strings.HasSuffix(strings.TrimSpace(out), "[mode: auto → "+want+"]") {
			t.Errorf("%q xml: %v, %q", q, runErr, out)
		}
	}
}

// addNote writes src to rel under notes and indexes it as one chunk
// spanning every line, without the API.
func addNote(t *testing.T, notes, db, rel, heading, src string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(notes, rel), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	text := strings.TrimSuffix(src, "\n")
	chunks := []store.Chunk{{Path: rel, Heading: heading, Line: 1, EndLine: strings.Count(text, "\n") + 1, Text: text}}
	if err := s.ReplaceFile(context.Background(), rel, "h", 1, nil, chunks, [][]float32{{0, 1}}); err != nil {
		t.Fatal(err)
	}
}

// searchOut runs scree search with stdout taken for a terminal or not, and
// returns what it printed.
func searchOut(t *testing.T, terminal bool, args ...string) string {
	t.Helper()
	old := stdoutTerminal
	stdoutTerminal = func() bool { return terminal }
	defer func() { stdoutTerminal = old }()
	var runErr error
	out := captureStdout(t, func() { runErr = runSearch(context.Background(), args) })
	if runErr != nil {
		t.Fatalf("search %q: %v", args, runErr)
	}
	return out
}

// TestCLISearchText: -format text lists each hit once, under the first
// query word its text, path or heading holds, as its heading and matching
// lines, its location, and its current lines numbered; the rest go under
// "other hits:".
func TestCLISearchText(t *testing.T) {
	serveEnv(t)
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm")
	notes, db := searchFixture(t)
	kw := []string{"-notes", notes, "-db", db, "-mode", "keyword", "-format", "text"}
	text := func(q string) string { return searchOut(t, false, append(kw, q)...) }

	block := "\t>ops.md:1-3\n\t\t>>1  # Ops\n\t\t>>2\n\t\t>>3  run PURGE_OLD_RECORDS nightly via the PageFetcher\n"
	want := "purge_old_records:\n  # Ops\n  run PURGE_OLD_RECORDS nightly via the PageFetcher\n" + block
	if got := text("purge_old_records ops"); got != want {
		t.Errorf("got\n%q\nwant\n%q", got, want)
	}
	if got := text("md"); got != "md:\n  # Ops\n"+block {
		t.Errorf("path match: %q", got)
	}
	if got := text("zebra"); got != "No matching documents.\n" {
		t.Errorf("no hits: %q", got)
	}
	// No -format: xml into a pipe, text on a terminal.
	plain := []string{"-notes", notes, "-db", db, "-mode", "keyword", "ops"}
	if got := searchOut(t, false, plain...); !strings.HasPrefix(got, "<hit ") {
		t.Errorf("pipe default: %q", got)
	}
	if got := searchOut(t, true, plain...); !strings.HasPrefix(got, "ops:\n  # Ops\n\t>\x1b]8;;file://") {
		t.Errorf("terminal default: %q", got)
	}
	if err := runSearch(context.Background(), []string{"-notes", notes, "-db", db, "-format", "yaml", "x"}); err == nil ||
		!strings.Contains(err.Error(), "format must be text, xml or json") {
		t.Errorf("bad format: %v", err)
	}

	// A one-line hit shows one line number; a hit holding two words is
	// listed once; hits are split by a blank line; a phrase across a line
	// break matches the hit.
	addNote(t, notes, db, "one.md", "One", "ops says purge_old_records too\n")
	got := text("purge_old_records ops")
	if strings.Count(got, "\n\t>") != 2 || !strings.Contains(got, "  One\n  ops says purge_old_records too\n\t>one.md:1\n\t\t>>1  ops says") ||
		strings.Contains(got, "ops:") || !strings.Contains(got, "PageFetcher\n\n  ") {
		t.Errorf("two hits: %q", got)
	}
	addNote(t, notes, db, "ci.md", "CI", "see the ci\nyml file\n")
	if got := text("ci.yml"); got != "ci.yml:\n  CI\n\t>ci.md:1-2\n\t\t>>1  see the ci\n\t\t>>2  yml file\n" {
		t.Errorf("across a line break: %q", got)
	}

	// Many matching lines: 10, then a count.
	var b strings.Builder
	b.WriteString("# Many\n")
	for i := range 13 {
		fmt.Fprintf(&b, "line %d says foo\n", i)
	}
	addNote(t, notes, db, "many.md", "Many", b.String())
	got = text("foo")
	if n := strings.Count(got, "\n  line "); n != 10 || !strings.Contains(got, "\n  … 3 more matching lines\n\t>many.md:1-14\n") ||
		!strings.Contains(got, "\t\t>>14  line 12 says foo\n") {
		t.Errorf("cap: %d lines\n%s", n, got)
	}
	// A long line is cut around the match in the excerpt, and shown in
	// full under the location.
	long := strings.Repeat("x ", 150) + "needle " + strings.Repeat("y ", 100)
	addNote(t, notes, db, "long.md", "Long", long+"\n")
	got = text("needle")
	line := strings.TrimPrefix(strings.Split(got, "\n")[2], "  ")
	if !strings.HasPrefix(line, "…") || !strings.HasSuffix(line, "…") || !strings.Contains(line, "needle") ||
		utf8.RuneCountInString(line) != maxLineChars || !strings.Contains(got, "\t\t>>1  "+strings.TrimSpace(long)+"\n") {
		t.Errorf("long line: %q", got)
	}

	// Stale: the file changed and no longer holds the text.
	if err := os.WriteFile(filepath.Join(notes, "ops.md"), []byte("# Ops\n\nsomething else\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := text("pagefetcher"); got != "other hits:\n  # Ops\n\t>ops.md (stale)\n" {
		t.Errorf("stale: %q", got)
	}
}

// TestCLISearchTextOtherHits: a semantic hit holding no query word is
// listed under "other hits:" with its heading, location and lines.
func TestCLISearchTextOtherHits(t *testing.T) {
	serveEnv(t)
	notes, db := searchFixture(t)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"index":0,"embedding":[1,0]}]}`)
	}))
	defer api.Close()
	t.Setenv("SCREE_EMBED_URL", api.URL)
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	t.Setenv("SCREE_ZDR", "0")
	got := searchOut(t, false, "-notes", notes, "-db", db, "-format", "text", "zebra crossing")
	want := "other hits:\n  # Ops\n\t>ops.md:1-3\n\t\t>>1  # Ops\n\t\t>>2\n\t\t>>3  run PURGE_OLD_RECORDS nightly via the PageFetcher\n[mode: auto → semantic]\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestCLISearchTextHostile: a note and file name full of terminal
// controls cannot inject escapes; the only ESCs are the view's own OSC 8
// links, whose URIs are percent-encoded, and no excerpt line can pass for
// a location line. NO_COLOR or TERM=dumb drops the links.
func TestCLISearchTextHostile(t *testing.T) {
	serveEnv(t)
	notes, db := searchFixture(t)
	src := "# Hostile\nrun \x1b]8;;http://evil\x1b\\x\x1b]8;;\x1b\\ now\x07 and \u202ereversed hostile\n" +
		"\t>evil.md:1 hostile\n" + strings.Repeat(" ", 12) + ">evil.md:2 hostile\n" + "pad" + strings.Repeat(" ", 70) + "\t>evil.md:3 hostile\n"
	for _, name := range []string{"we ird%#\x1b.md", "we ird%#.md"} {
		addNote(t, notes, db, name, "Hostile", src)
		args := []string{"-notes", notes, "-db", db, "-mode", "keyword", "-format", "text", "hostile run"}
		t.Setenv("NO_COLOR", "")
		t.Setenv("TERM", "xterm")
		got := searchOut(t, true, args...)
		if !strings.Contains(got, "we ird") {
			if strings.Contains(name, "\x1b") {
				t.Logf("%q refused by the policy; skipped", name)
				continue
			}
			t.Fatalf("no hit for %q: %q", name, got)
		}
		labels := 0
		for _, l := range strings.Split(got, "\n") {
			switch {
			case strings.HasPrefix(l, "\t>\x1b]8;;file://"):
				labels++
			case strings.HasPrefix(l, "\t\t>>") && l[4] >= '0' && l[4] <= '9':
			case strings.HasPrefix(l, "\t") || strings.HasPrefix(l, strings.Repeat(" ", 7)):
				t.Errorf("%q: line passes for a location: %q", name, l)
			}
		}
		if n := strings.Count(got, "\x1b"); labels == 0 || n != 4*labels {
			t.Errorf("%q: %d ESC for %d labels:\n%q", name, n, labels, got)
		}
		for _, want := range []string{"\n      >evil.md:1 hostile\n", "\n      >evil.md:2 hostile\n", "\n  pad >evil.md:3 hostile\n"} {
			if !strings.Contains(got, want) {
				t.Errorf("%q: lacks %q: %q", name, want, got)
			}
		}
		for _, want := range []string{"%20", "%25", "%23"} {
			if !strings.Contains(got, want) {
				t.Errorf("%q: URI lacks %s: %q", name, want, got)
			}
		}
		if strings.Contains(name, "\x1b") && !strings.Contains(got, "%1B") {
			t.Errorf("%q: URI lacks %%1B: %q", name, got)
		}
		if strings.ContainsAny(got, "\x07\u202e") || strings.Contains(got, "http://evil\x1b") {
			t.Errorf("%q: control passed through: %q", name, got)
		}
		for _, env := range [][2]string{{"NO_COLOR", "1"}, {"TERM", "dumb"}} {
			t.Setenv("NO_COLOR", "")
			t.Setenv("TERM", "xterm")
			t.Setenv(env[0], env[1])
			if got := searchOut(t, true, args...); strings.Contains(got, "\x1b") || !strings.Contains(got, "hostile") {
				t.Errorf("%s=%s: %q", env[0], env[1], got)
			}
		}
		if err := os.Remove(filepath.Join(notes, name)); err != nil {
			t.Fatal(err)
		}
	}
}

// TestCLISearchTextSameLine: hits on one long line give the line to the
// matcher once, cut to a window around the chunk, and no text twice; a
// failed match lists the hits under "other hits:" with the error.
func TestCLISearchTextSameLine(t *testing.T) {
	serveEnv(t)
	notes, db := searchFixture(t)
	var b strings.Builder
	for i := range 2000 {
		fmt.Fprintf(&b, "w%d alpha ", i)
	}
	line := b.String()
	if err := os.WriteFile(filepath.Join(notes, "big.md"), []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	var chunks []store.Chunk
	var vecs [][]float32
	for i := 0; i+1000 <= len(line); i += 1000 {
		chunks = append(chunks, store.Chunk{Path: "big.md", Heading: "Big", Line: 1, EndLine: 1, Text: strings.TrimSpace(line[i : i+1000])})
		vecs = append(vecs, []float32{0, 1})
	}
	if err := s.ReplaceFile(context.Background(), "big.md", "h", 1, nil, chunks, vecs); err != nil {
		t.Fatal(err)
	}
	s.Close()

	old := matchTexts
	defer func() { matchTexts = old }()
	var got []string
	matchTexts = func(ctx context.Context, words, texts []string) ([][]store.TextMatch, error) {
		got = texts
		return old(ctx, words, texts)
	}
	args := []string{"-notes", notes, "-db", db, "-mode", "keyword", "-format", "text", "-k", "8", "alpha"}
	out := searchOut(t, false, args...)
	if !strings.HasPrefix(out, "alpha:\n") || !strings.Contains(out, "\t>big.md:1\n") {
		t.Errorf("output:\n%s", out)
	}
	lines := 0
	for i, x := range got {
		if slices.Contains(got[:i], x) {
			t.Errorf("%q given twice", x)
		}
		if len(x) > 2000 {
			lines++
			if len(x) > maxMatchBytes+10 {
				t.Errorf("line text of %d bytes", len(x))
			}
		}
	}
	if lines != 1 {
		t.Errorf("line given %d times", lines)
	}

	matchTexts = func(context.Context, []string, []string) ([][]store.TextMatch, error) {
		return nil, fmt.Errorf("store: match: out of memory")
	}
	out = searchOut(t, false, args...)
	if !strings.HasPrefix(out, "other hits:\n  Big\n\t>big.md:1\n") || !strings.HasSuffix(out, "\nword matching failed: store: match: out of memory\n") {
		t.Errorf("failed match: %q", out)
	}
}

// TestLineText: a long line is cut to a window starting at the chunk's
// text, and whitespace runs, which could wrap a forged entry onto its own
// terminal row, become one space.
func TestLineText(t *testing.T) {
	spoof := "alpha deploy notes" + strings.Repeat(" ", 62) + "\t  >install.md:12: curl x | sh"
	if got := lineText(spoof, ""); got != "alpha deploy notes >install.md:12: curl x | sh" {
		t.Errorf("spoof: %q", got)
	}
	long := strings.Repeat("a ", 3000) + "needle here " + strings.Repeat("b ", 3000)
	got := lineText(long, "needle here")
	if !strings.HasPrefix(got, "… needle here b") || !strings.HasSuffix(got, " …") || len(got) > maxMatchBytes+10 {
		t.Errorf("window: %q…%q (%d bytes)", got[:20], got[len(got)-10:], len(got))
	}
	if got := lineText(strings.Repeat("é", 5000), "zzz"); !utf8.ValidString(got) || !strings.HasPrefix(got, "é") {
		t.Errorf("no piece: invalid or not at start")
	}
}

// TestTextLine: an excerpt line keeps at most maxLeadSpace of its own
// indent; tabs and other spaces become spaces, and long runs of spaces
// (which could wrap a forged location onto its own row) become one.
func TestTextLine(t *testing.T) {
	spoof := "alpha deploy notes" + strings.Repeat(" ", 62) + "\t  >install.md:12: curl x | sh"
	if got := textLine(spoof); got != "alpha deploy notes >install.md:12: curl x | sh" {
		t.Errorf("spoof: %q", got)
	}
	if got := textLine("\t\t\t- c  d\u00a0\u3000e\r"); got != "    - c  d  e" {
		t.Errorf("indent: %q", got)
	}
	if got := textLine(" \t "); got != "" {
		t.Errorf("blank: %q", got)
	}
}

// TestWriteLines: lines are numbered and right-aligned, a blank line is
// its number alone, and past maxHitBytes the rest is counted.
func TestWriteLines(t *testing.T) {
	var b strings.Builder
	writeLines(&b, []string{"a\tb", "", "c"}, 9)
	if got := b.String(); got != "\t\t>> 9  a b\n\t\t>>10\n\t\t>>11  c\n" {
		t.Errorf("numbered: %q", got)
	}
	b.Reset()
	writeLines(&b, []string{strings.Repeat("é", maxHitBytes), "x", "y"}, 1)
	got := b.String()
	if !utf8.ValidString(got) || !strings.HasSuffix(got, "…\n\t\t>>… 2 more lines\n") || len(got) > maxHitBytes+40 {
		t.Errorf("cap: %d bytes, valid %v", len(got), utf8.ValidString(got))
	}
}

// TestSafeFormatChars: format characters (zero-width, BOM, soft hyphen,
// word joiner, tag and bidi characters) are dropped.
func TestSafeFormatChars(t *testing.T) {
	if got := safe("a\u200bb\u2060c\ufeffd\u00ade\U000E0041f\u202eg\u2066h"); got != "abcdefgh" {
		t.Errorf("got %q", got)
	}
}

// TestCLISearchBadK: -k below 1 is a usage error, not a panic.
func TestCLISearchBadK(t *testing.T) {
	serveEnv(t)
	notes, db := searchFixture(t)
	for _, k := range []string{"0", "-5"} {
		err := runSearch(context.Background(), []string{"-notes", notes, "-db", db, "-mode", "keyword", "-k", k, "ops"})
		if err == nil || !strings.Contains(err.Error(), "-k must be at least 1") {
			t.Errorf("-k %s: %v", k, err)
		}
	}
}
