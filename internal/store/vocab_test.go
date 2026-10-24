package store

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestVocab(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if ok, err := s.InVocabAndNotes(ctx, []string{"pagefetcher"}); err != nil || ok {
		t.Fatalf("empty InVocabAndNotes = %v, %v", ok, err)
	}
	if err := s.UpdateVocab(ctx, []VocabFile{
		{Path: "a.py", Hash: "1", Names: []string{"pagefetcher", "psycopg", "codeonlyname"}},
		{Path: "go.mod", Hash: "2", Names: []string{"psycopg", "golang.org/x/sync", "example.com/codeonly/mod"}},
		{Path: "empty.go", Hash: "3"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	// In the vocabulary but not yet in any note: nothing routes.
	if ok, err := s.InVocabAndNotes(ctx, []string{"pagefetcher"}); err != nil || ok {
		t.Fatalf("InVocabAndNotes without notes = %v, %v", ok, err)
	}
	if err := s.ReplaceFile(ctx, "n.md", "h", 1, nil, []Chunk{{Path: "n.md", Heading: "Psycopg", Line: 1, EndLine: 1,
		Text: "The PageFetcher uses golang.org/x/sync; example.com/codeonly is a site, and the mod file too."}}, [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	n, in, files, some, err := s.VocabStats(ctx, 2)
	if err != nil || n != 5 || in != 3 || files != 3 || !slices.Equal(some, []string{"golang.org/x/sync", "pagefetcher"}) {
		t.Fatalf("VocabStats = %d, %d, %d, %q, %v", n, in, files, some, err)
	}
	if _, _, _, some, _ := s.VocabStats(ctx, 10); !slices.Equal(some, []string{"golang.org/x/sync", "pagefetcher", "psycopg"}) {
		t.Errorf("VocabStats sample = %q", some)
	}
	for words, want := range map[string]bool{
		"how does pagefetcher": true, "sync": false, "golang.org/x/sync": true, "psycopg": true,
		// In the vocabulary, not in the notes; the second one's tokens are
		// there, but not as a phrase.
		"codeonlyname": false, "example.com/codeonly/mod": false,
		// In the notes, not in the vocabulary.
		"site uses": false,
	} {
		ok, err := s.InVocabAndNotes(ctx, strings.Fields(words))
		if err != nil || ok != want {
			t.Errorf("InVocabAndNotes(%q) = %v, %v", words, ok, err)
		}
	}
	if err := s.UpdateVocab(ctx, []VocabFile{{Path: "a.py", Hash: "4", Names: []string{"fetchpage"}}}, []string{"go.mod"}); err != nil {
		t.Fatal(err)
	}
	if h, err := s.VocabFiles(ctx); err != nil || len(h) != 2 || h["a.py"] != (VocabStored{"4", 1}) || h["empty.go"] != (VocabStored{"3", 0}) {
		t.Errorf("VocabFiles = %v, %v", h, err)
	}
	if err := s.ClearVocab(ctx, "9"); err != nil {
		t.Fatal(err)
	}
	if n, _, files, _, _ := s.VocabStats(ctx, 10); n != 0 || files != 0 {
		t.Errorf("after ClearVocab: %d names, %d files", n, files)
	}
	if v, _ := s.Meta(VocabVersionKey); v != "9" {
		t.Errorf("vocab version = %q", v)
	}
	if err := s.UpdateVocab(ctx, []VocabFile{{Path: "a.py", Hash: "4", Names: []string{"fetchpage"}}}, nil); err != nil {
		t.Fatal(err)
	}
	if some, err := s.VocabNames(ctx, 10); err != nil || !slices.Equal(some, []string{"fetchpage"}) {
		t.Errorf("after update = %q, %v", some, err)
	}
	if err := s.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _, files, _, _ := s.VocabStats(ctx, 10); n != 0 || files != 0 {
		t.Errorf("after Reset: %d names, %d files", n, files)
	}
}

// TestMigrateVocab: a schema 3 index (no vocabulary tables) is upgraded on
// open, keeping its chunks, with an empty vocabulary and the terms table.
func TestMigrateVocab(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceFile(ctx, "a.md", "h", 1, nil, []Chunk{{Path: "a.md", Line: 1, EndLine: 1, Text: "x"}}, [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`DROP TABLE vocab`, `DROP TABLE vocab_files`, `DROP TABLE chunks_terms`, `UPDATE meta SET value = '3' WHERE key = 'schema'`} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	if sum, err := Peek(path); err != nil || sum.Schema != "3" {
		t.Fatalf("Peek before = %+v, %v", sum, err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v, _ := s.Meta("schema"); v != "4" || SchemaVersion != "4" {
		t.Errorf("schema = %q", v)
	}
	if n, _, files, _, err := s.VocabStats(ctx, 1); err != nil || n != 0 || files != 0 {
		t.Errorf("VocabStats = %d, %d, %v", n, files, err)
	}
	if st, err := s.Stats(ctx); err != nil || st.Files != 1 || st.Chunks != 1 {
		t.Errorf("Stats = %+v, %v", st, err)
	}
	if err := s.UpdateVocab(ctx, []VocabFile{{Path: "a.go", Hash: "1", Names: []string{"filehash"}}}, nil); err != nil {
		t.Fatal(err)
	}
	if some, err := s.VocabNames(ctx, 10); err != nil || !slices.Equal(some, []string{"filehash"}) {
		t.Errorf("VocabNames after migration = %q, %v", some, err)
	}
}
