package ignore

import (
	"slices"
	"strings"
	"testing"
)

type check struct {
	path  string
	isDir bool
	want  bool
}

func TestMatch(t *testing.T) {
	cases := []struct {
		name   string
		file   string
		checks []check
	}{
		{
			name: "spec example",
			file: "# journal stays local\njournal/\n**/private-*.md\n/inbox/scratch.md\n!journal/public.md\n",
			checks: []check{
				{"journal/x.md", false, true},
				{"journal/public.md", false, false},
				{"a/b/private-x.md", false, true},
				{"private-y.md", false, true},
				{"inbox/scratch.md", false, true},
				{"x/inbox/scratch.md", false, false},
				{"notes.md", false, false},
			},
		},
		{
			name: "comments and blank lines skipped",
			file: "# a.md\n\n   \n#b.md\r\n",
			checks: []check{
				{"a.md", false, false},
				{"b.md", false, false},
				{"# a.md", false, false},
			},
		},
		{
			name: "escaped hash is literal",
			file: `\#lit.md`,
			checks: []check{
				{"#lit.md", false, true},
				{"lit.md", false, false},
			},
		},
		{
			name: "escaped bang is literal",
			file: `\!bang.md`,
			checks: []check{
				{"!bang.md", false, true},
				{"bang.md", false, false},
			},
		},
		{
			name: "trailing spaces trimmed",
			file: "a.md   \r\nb.md\\ \n",
			checks: []check{
				{"a.md", false, true},
				{"b.md ", false, true},
				{"b.md", false, false},
			},
		},
		{
			name: "trailing slash matches folders only",
			file: "journal/",
			checks: []check{
				{"journal", false, false},
				{"journal", true, true},
				{"journal/x.md", false, true},
				{"a/journal/x.md", false, true},
			},
		},
		{
			name: "basename pattern matches at any depth",
			file: "*.md",
			checks: []check{
				{"a.md", false, true},
				{"x/y/a.md", false, true},
				{"a.txt", false, false},
			},
		},
		{
			name: "pattern with slash is anchored and star stays in segment",
			file: "a/*.md",
			checks: []check{
				{"a/b.md", false, true},
				{"x/a/b.md", false, false},
				{"a/b/c.md", false, false},
			},
		},
		{
			name: "double star",
			file: "journal/**\n**/deep/*.md\na/**/z.md",
			checks: []check{
				{"journal/x.md", false, true},
				{"journal/a/b/x.md", false, true},
				{"journalx.md", false, false},
				{"deep/a.md", false, true},
				{"p/q/deep/a.md", false, true},
				{"a/z.md", false, true},
				{"a/b/c/z.md", false, true},
				{"b/z.md", false, false},
			},
		},
		{
			name: "negated class",
			file: "[!a]x.md",
			checks: []check{
				{"bx.md", false, true},
				{"ax.md", false, false},
				{"sub/cx.md", false, true},
			},
		},
		{
			name: "last match wins",
			file: "!x.md\nx.md",
			checks: []check{
				{"x.md", false, true},
			},
		},
		{
			name: "negation after ignore re-includes",
			file: "*.md\n!keep.md",
			checks: []check{
				{"x.md", false, true},
				{"keep.md", false, false},
				{"a/keep.md", false, false},
			},
		},
		{
			name: "case sensitive",
			file: "Secret.md",
			checks: []check{
				{"Secret.md", false, true},
				{"secret.md", false, false},
			},
		},
		{
			name: "crlf line endings",
			file: "a.md\r\nb/\r\n",
			checks: []check{
				{"a.md", false, true},
				{"b/x.md", false, true},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := Parse([]byte(c.file))
			if errs := m.Errors(); len(errs) > 0 {
				t.Fatalf("unexpected errors: %v", errs)
			}
			for _, ch := range c.checks {
				if got := m.Match(ch.path, ch.isDir); got != ch.want {
					t.Errorf("Match(%q, %v) = %v, want %v", ch.path, ch.isDir, got, ch.want)
				}
			}
		})
	}
}

func TestBadPattern(t *testing.T) {
	m := Parse([]byte("ok.md\n[\n!\n"))
	errs := m.Errors()
	if len(errs) != 2 || !strings.HasPrefix(errs[0], "line 2: bad pattern") || !strings.HasPrefix(errs[1], "line 3:") {
		t.Fatalf("Errors() = %q", errs)
	}
	for _, p := range []string{"[", "[.md", "a/["} {
		if m.Match(p, false) {
			t.Errorf("bad pattern matched %q", p)
		}
	}
	if !m.Match("ok.md", false) {
		t.Error("good pattern next to a bad one stopped matching")
	}
}

func TestEmpty(t *testing.T) {
	var nilM *Matcher
	for _, m := range []*Matcher{Parse(nil), nilM} {
		if m.Match("a.md", false) || m.Prunable("a") || len(m.Errors()) != 0 {
			t.Error("empty matcher ignored something")
		}
	}
}

func TestPrunable(t *testing.T) {
	m := Parse([]byte("journal/\n"))
	if !m.Prunable("journal") {
		t.Error("journal not prunable")
	}
	if m.Prunable("other") {
		t.Error("other prunable")
	}
	m = Parse([]byte("journal/\n!elsewhere/keep.md\n"))
	if m.Prunable("journal") {
		t.Error("prunable despite a negation rule")
	}
	if !m.Match("journal", true) {
		t.Error("journal no longer matched")
	}
}

func TestFoldCaseMatching(t *testing.T) {
	for _, c := range []struct {
		pattern, path string
	}{
		{"Private/", "private/x.md"},
		{"private/", "Private/x.md"},
		{"*.MD", "a/b.md"},
		{"[A-C]x.md", "bx.md"},
	} {
		folded := ParseWith([]byte(c.pattern), Options{FoldCase: true})
		if !folded.Match(c.path, false) {
			t.Errorf("FoldCase: %q does not match %q", c.pattern, c.path)
		}
		if matched, _ := folded.MatchSelf(strings.TrimSuffix(c.path, "/x.md"), true); !matched && strings.HasSuffix(c.pattern, "/") {
			t.Errorf("FoldCase MatchSelf: %q does not match the folder of %q", c.pattern, c.path)
		}
		if Parse([]byte(c.pattern)).Match(c.path, false) {
			t.Errorf("exact: %q matches %q", c.pattern, c.path)
		}
	}
}

func TestNFCMatching(t *testing.T) {
	nfc, nfd := "café", "café"
	for _, c := range []struct{ pattern, path string }{
		{nfc + "/", nfd + "/x.md"},
		{nfd + "/", nfc + "/x.md"},
		{"*" + nfc + ".md", "a/" + nfd + ".md"},
	} {
		if !Parse([]byte(c.pattern)).Match(c.path, false) {
			t.Errorf("%q does not match %q", c.pattern, c.path)
		}
	}
}

// Errors name the line only. The pattern could be text from a file the
// user never meant to show, so it is never repeated.
func TestParseErrorsHaveNoText(t *testing.T) {
	data := "ok.md\n[secret-pattern\n  # fine\nOPENROUTER_API_KEY=sk-or-v1-FAKE [bad\n"
	for _, o := range []Options{{}, {FoldCase: true}} {
		errs := ParseWith([]byte(data), o).Errors()
		if want := []string{"line 2: bad pattern", "line 4: bad pattern"}; !slices.Equal(errs, want) {
			t.Errorf("Errors() = %q, want %q", errs, want)
		}
		for _, e := range errs {
			if strings.Contains(e, "secret") || strings.Contains(e, "sk-or") || strings.Contains(e, "[") {
				t.Errorf("error echoes the pattern: %q", e)
			}
		}
	}
}

func TestScreeignoreFileRuleBeatsFolderNegation(t *testing.T) {
	cases := []struct {
		file   string
		checks []check
	}{
		{"*.private.md\narchive/\n!archive/2024/\n", []check{
			{"archive/2024/salary.private.md", false, true},
			{"archive/2024/a.md", false, false},
			{"archive/2023/a.md", false, true},
			{"x.private.md", false, true},
		}},
		{"journal/\n!journal/public.md\n", []check{
			{"journal/public.md", false, false},
			{"journal/x.md", false, true},
		}},
		// A later file rule still wins over an earlier negation.
		{"archive/\n!archive/2024/\n*.private.md\n", []check{
			{"archive/2024/s.private.md", false, true},
			{"archive/2024/a.md", false, false},
		}},
		// A file-level "!" can re-include after a file rule.
		{"*.private.md\n!keep.private.md\n", []check{
			{"a/keep.private.md", false, false},
			{"a/b.private.md", false, true},
		}},
	}
	for _, c := range cases {
		m := Parse([]byte(c.file))
		for _, ch := range c.checks {
			if got := m.Match(ch.path, ch.isDir); got != ch.want {
				t.Errorf("%q: Match(%q) = %v, want %v", c.file, ch.path, got, ch.want)
			}
		}
	}
}

func TestExplainReincluded(t *testing.T) {
	m := Parse([]byte("# c\narchive/\n!archive/2024/\njournal/\n!journal/public.md\n!never-excluded.md\n"))
	for _, c := range []struct {
		path    string
		ignored bool
		by      int
	}{
		{"archive/2024/a.md", false, 3},
		{"archive/2023/a.md", true, 0},
		{"journal/public.md", false, 5},
		{"never-excluded.md", false, 0},
		{"other.md", false, 0},
	} {
		ignored, by := m.Explain(c.path, false)
		if ignored != c.ignored || by != c.by {
			t.Errorf("Explain(%q) = %v, %d, want %v, %d", c.path, ignored, by, c.ignored, c.by)
		}
	}
}
