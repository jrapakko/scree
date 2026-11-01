package store

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// TestMatchTexts: words match texts as keyword search matches chunks, At
// points at the match, and Spans covers each place the word matches.
func TestMatchTexts(t *testing.T) {
	texts := []string{
		"see .forgejo/workflows/ci.yml for the job", // 0
		"circle the wagons",                         // 1
		"Café au lait",                              // 2
		"CAFÉ AU LAIT",                              // 3
		"run PURGE_OLD_RECORDS nightly",             // 4
		"purge everything",                          // 5
		"ship R7-Q2 then R7",                        // 6
		`NEAR the col:x "quoted" text`,              // 7
		"Café, then café again",                     // 8
	}
	// m is a match of the text at row on each of words, found in order.
	m := func(row int, words ...string) TextMatch {
		tm := TextMatch{Row: row}
		from := 0
		for i, w := range words {
			at := from + strings.Index(texts[row][from:], w)
			if i == 0 {
				tm.At = at
			}
			tm.Spans = append(tm.Spans, [2]int{at, at + len(w)})
			from = at + len(w)
		}
		return tm
	}
	cases := []struct {
		word string
		want []TextMatch
	}{
		{"ci", []TextMatch{m(0, "ci")}},
		{"cafe", []TextMatch{m(2, "Café"), m(3, "CAFÉ"), m(8, "Café", "café")}},
		{"Café", []TextMatch{m(2, "Café"), m(3, "CAFÉ"), m(8, "Café", "café")}},
		{"purge_old_records", []TextMatch{m(4, "PURGE_OLD_RECORDS")}},
		{"purge", []TextMatch{m(5, "purge")}},
		{"ci.yml", []TextMatch{m(0, "ci.yml")}},
		{"R7-Q2", []TextMatch{m(6, "R7-Q2")}},
		{"r7", []TextMatch{{Row: 6, At: strings.LastIndex(texts[6], "R7"), Spans: [][2]int{{16, 18}}}}},
		{"NEAR", []TextMatch{m(7, "NEAR")}},
		{"col:x", []TextMatch{m(7, "col:x")}},
		{`"quoted"`, []TextMatch{m(7, "quoted")}},
		{"/", nil},
		{"nothing", nil},
	}
	words := make([]string, len(cases))
	for i, c := range cases {
		words[i] = c.word
	}
	got, err := MatchTexts(context.Background(), words, texts)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(words) {
		t.Fatalf("%d results for %d words", len(got), len(words))
	}
	for i, c := range cases {
		if !slices.EqualFunc(got[i], c.want, func(a, b TextMatch) bool {
			return a.Row == b.Row && a.At == b.At && slices.Equal(a.Spans, b.Spans)
		}) {
			t.Errorf("%q: %v, want %v", c.word, got[i], c.want)
		}
	}
	if got, err := MatchTexts(context.Background(), []string{"x"}, nil); err != nil || len(got) != 1 || got[0] != nil {
		t.Errorf("no texts: %v, %v", got, err)
	}
}
