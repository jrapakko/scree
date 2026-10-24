package chunk

import (
	"strings"
	"testing"
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
