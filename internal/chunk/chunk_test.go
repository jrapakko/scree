package chunk

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSplitHeadingTrail(t *testing.T) {
	src := `---
title: x
---
Intro text.

# Projects

Top level.

## scree

Search notes.

### Stack

Go.

## Other

Something else.
`
	got := Split(src)
	want := []struct {
		heading string
		line    int
		prefix  string
	}{
		{"", 4, "Intro text."},
		{"Projects", 6, "# Projects"},
		{"Projects > scree", 10, "## scree"},
		{"Projects > scree > Stack", 14, "### Stack"},
		{"Projects > Other", 18, "## Other"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d chunks, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Heading != w.heading || got[i].Line != w.line || !strings.HasPrefix(got[i].Text, w.prefix) {
			t.Errorf("chunk %d = %+v, want heading %q line %d prefix %q", i, got[i], w.heading, w.line, w.prefix)
		}
	}
}

func TestSplitIgnoresHeadingsInFences(t *testing.T) {
	src := "# A\n\n```sh\n# not a heading\n```\n\n# B\ntext\n"
	got := Split(src)
	if len(got) != 2 {
		t.Fatalf("got %d chunks: %+v", len(got), got)
	}
	if !strings.Contains(got[0].Text, "# not a heading") {
		t.Errorf("fenced line missing from first chunk: %q", got[0].Text)
	}
}

func TestSplitSkipsEmptySections(t *testing.T) {
	got := Split("# Empty\n\n## Also empty\n\n## Full\nbody\n")
	if len(got) != 1 || got[0].Heading != "Empty > Full" {
		t.Fatalf("got %+v", got)
	}
}

func TestSplitLongSection(t *testing.T) {
	para := strings.Repeat("word ", 100) // 500 chars
	var b strings.Builder
	b.WriteString("# Long\n\n")
	for i := 0; i < 10; i++ {
		b.WriteString(para + "\n\n")
	}
	got := Split(b.String())
	if len(got) < 2 {
		t.Fatalf("expected long section to split, got %d chunks", len(got))
	}
	for _, c := range got {
		if c.Heading != "Long" {
			t.Errorf("split chunk lost heading: %q", c.Heading)
		}
		if len(c.Text) > MaxChars+len(para)+2 {
			t.Errorf("chunk too long: %d", len(c.Text))
		}
	}
	if got[1].Line <= got[0].Line {
		t.Errorf("line numbers not increasing: %d, %d", got[0].Line, got[1].Line)
	}
}

func TestAtxHeading(t *testing.T) {
	cases := []struct {
		in    string
		level int
		text  string
		ok    bool
	}{
		{"# Title", 1, "Title", true},
		{"### Title ###", 3, "Title", true},
		{"#hashtag", 0, "", false},
		{"####### seven", 0, "", false},
		{"    # indented code", 0, "", false},
		{"## C#", 2, "C#", true},
	}
	for _, c := range cases {
		l, txt, ok := atxHeading(c.in)
		if l != c.level || txt != c.text || ok != c.ok {
			t.Errorf("atxHeading(%q) = %d %q %v", c.in, l, txt, ok)
		}
	}
}

func checkCap(t *testing.T, got []Chunk) {
	t.Helper()
	for i, c := range got {
		if len(c.Text) > MaxChars {
			t.Fatalf("chunk %d is %d bytes, over MaxChars", i, len(c.Text))
		}
	}
}

func TestSplitHardCapSingleLine(t *testing.T) {
	line := strings.Repeat("abcdefghij", 500_000) // 5 MB, no blank lines
	got := Split("# Big\n" + line)
	checkCap(t, got)
	// The heading-only chunk is dropped, so every chunk is a piece.
	var b strings.Builder
	for _, c := range got {
		b.WriteString(c.Text)
	}
	if b.String() != line {
		t.Fatalf("pieces do not restore the line (%d chunks)", len(got))
	}
	for _, c := range got {
		if c.Line != 2 || c.Heading != "Big" {
			t.Fatalf("piece has line %d heading %q, want 2 Big", c.Line, c.Heading)
		}
	}
}

func TestSplitHardCapUnclosedFence(t *testing.T) {
	var b strings.Builder
	b.WriteString("# Code\n```\n")
	for i := 0; i < 10_000; i++ {
		b.WriteString("x := 1\n\n")
	}
	got := Split(b.String())
	if len(got) < 10 {
		t.Fatalf("unclosed fence not split: %d chunks", len(got))
	}
	checkCap(t, got)
	for i := 1; i < len(got); i++ {
		if got[i].Line <= got[i-1].Line {
			t.Fatalf("line numbers not increasing at chunk %d: %d, %d", i, got[i-1].Line, got[i].Line)
		}
	}
}

func TestSplitRuneSafe(t *testing.T) {
	line := strings.Repeat("€", 3000) // 3 bytes each; MaxChars is not a multiple of 3
	got := Split(line + "\n\n" + strings.Repeat("語 ", 1500))
	checkCap(t, got)
	var b strings.Builder
	for _, c := range got {
		if !utf8.ValidString(c.Text) {
			t.Fatalf("chunk at line %d is not valid UTF-8", c.Line)
		}
		if c.Line == 1 {
			b.WriteString(c.Text)
		}
	}
	if b.String() != line {
		t.Fatal("pieces of the first line do not restore it")
	}
}

func TestSplitTargetChars(t *testing.T) {
	para := strings.Repeat("word ", 80) // 400 chars
	src := strings.Repeat(para+"\n\n", 8)
	got := Split(src)
	if len(got) != 2 {
		t.Fatalf("got %d chunks, want 2", len(got))
	}
	checkCap(t, got)
}

func TestSplitHeadingCapped(t *testing.T) {
	long := strings.Repeat("é", 100) // 200 bytes
	got := Split("# " + long + "\n## " + strings.Repeat("h", 300) + "\nbody\n")
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	parts := strings.Split(got[0].Heading, " > ")
	if len(parts) != 2 {
		t.Fatalf("heading = %q", got[0].Heading)
	}
	for _, p := range parts {
		if !strings.HasSuffix(p, "…") || len(p) > MaxHeadingChars+len("…") || !utf8.ValidString(p) {
			t.Errorf("heading part %q (%d bytes) not capped on a rune boundary", p, len(p))
		}
	}
	if _, text, _ := atxHeading("# short"); text != "short" {
		t.Errorf("short heading changed: %q", text)
	}
}
