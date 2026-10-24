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
	if ok, err := s.InVocab(ctx, []string{"pagefetcher"}); err != nil || ok {
		t.Fatalf("empty InVocab = %v, %v", ok, err)
	}
	if err := s.UpdateVocab(ctx, []VocabFile{
		{Path: "a.py", Hash: "1", Names: []string{"pagefetcher", "psycopg"}},
		{Path: "go.mod", Hash: "2", Names: []string{"psycopg", "golang.org/x/sync"}},
		{Path: "empty.go", Hash: "3"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	n, files, some, err := s.VocabStats(ctx, 2)
	if err != nil || n != 3 || files != 3 || !slices.Equal(some, []string{"golang.org/x/sync", "pagefetcher"}) {
		t.Fatalf("VocabStats = %d, %d, %q, %v", n, files, some, err)
	}
	for words, want := range map[string]bool{"how does pagefetcher": true, "sync": false, "golang.org/x/sync": true} {
		ok, err := s.InVocab(ctx, strings.Fields(words))
		if err != nil || ok != want {
			t.Errorf("InVocab(%q) = %v, %v", words, ok, err)
		}
	}
	if err := s.UpdateVocab(ctx, []VocabFile{{Path: "a.py", Hash: "4", Names: []string{"fetchpage"}}}, []string{"go.mod"}); err != nil {
		t.Fatal(err)
	}
	if h, err := s.VocabHashes(ctx); err != nil || len(h) != 2 || h["a.py"] != "4" || h["empty.go"] != "3" {
		t.Errorf("VocabHashes = %v, %v", h, err)
	}
	if _, _, some, _ := s.VocabStats(ctx, 10); !slices.Equal(some, []string{"fetchpage"}) {
		t.Errorf("after update = %q", some)
	}
	if err := s.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	if n, files, _, _ := s.VocabStats(ctx, 10); n != 0 || files != 0 {
		t.Errorf("after Reset: %d names, %d files", n, files)
	}
}

// TestMigrateVocab: a schema 3 index (no vocabulary tables) is upgraded on
// open, keeping its chunks, with an empty vocabulary.
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
	for _, q := range []string{`DROP TABLE vocab`, `DROP TABLE vocab_files`, `UPDATE meta SET value = '3' WHERE key = 'schema'`} {
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
	if n, files, _, err := s.VocabStats(ctx, 1); err != nil || n != 0 || files != 0 {
		t.Errorf("VocabStats = %d, %d, %v", n, files, err)
	}
	if st, err := s.Stats(ctx); err != nil || st.Files != 1 || st.Chunks != 1 {
		t.Errorf("Stats = %+v, %v", st, err)
	}
	if err := s.UpdateVocab(ctx, []VocabFile{{Path: "a.go", Hash: "1", Names: []string{"filehash"}}}, nil); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.InVocab(ctx, []string{"filehash"}); err != nil || !ok {
		t.Errorf("InVocab after migration = %v, %v", ok, err)
	}
}
