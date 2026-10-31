package store

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// TestMatchTexts: words match texts as keyword search matches chunks, and
// At points at the match.
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
	}
	cases := []struct {
		word string
		want []TextMatch
	}{
		{"ci", []TextMatch{{0, strings.Index(texts[0], "ci.yml")}}},
		{"cafe", []TextMatch{{2, 0}, {3, 0}}},
		{"Café", []TextMatch{{2, 0}, {3, 0}}},
		{"purge_old_records", []TextMatch{{4, 4}}},
		{"purge", []TextMatch{{5, 0}}},
		{"ci.yml", []TextMatch{{0, strings.Index(texts[0], "ci.yml")}}},
		{"R7-Q2", []TextMatch{{6, 5}}},
		{"r7", []TextMatch{{6, strings.LastIndex(texts[6], "R7")}}},
		{"NEAR", []TextMatch{{7, 0}}},
		{"col:x", []TextMatch{{7, strings.Index(texts[7], "col")}}},
		{`"quoted"`, []TextMatch{{7, strings.Index(texts[7], "quoted")}}},
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
		if !slices.Equal(got[i], c.want) {
			t.Errorf("%q: %v, want %v", c.word, got[i], c.want)
		}
	}
	if got, err := MatchTexts(context.Background(), []string{"x"}, nil); err != nil || len(got) != 1 || got[0] != nil {
		t.Errorf("no texts: %v, %v", got, err)
	}
}
