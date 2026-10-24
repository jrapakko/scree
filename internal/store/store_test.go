package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "sub", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestReplaceSearchRemove(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	if hits, err := s.Search(ctx, []float32{1, 0, 0}, 5); err != nil || hits != nil {
		t.Fatalf("empty search = %v, %v", hits, err)
	}

	err := s.ReplaceFile(ctx, "a.md", "h1", 1,
		[]Chunk{{Heading: "A", Line: 1, Text: "alpha"}, {Heading: "B", Line: 5, Text: "beta"}},
		[][]float32{{1, 0, 0}, {0, 1, 0}})
	if err != nil {
		t.Fatal(err)
	}
	err = s.ReplaceFile(ctx, "b.md", "h2", 1,
		[]Chunk{{Heading: "C", Line: 1, Text: "gamma"}},
		[][]float32{{0.9, 0.1, 0}})
	if err != nil {
		t.Fatal(err)
	}

	hits, err := s.Search(ctx, []float32{1, 0, 0}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].Text != "alpha" || hits[1].Text != "gamma" {
		t.Fatalf("hits = %+v", hits)
	}

	// Replacing a file drops its old chunks.
	err = s.ReplaceFile(ctx, "a.md", "h3", 2,
		[]Chunk{{Heading: "A2", Line: 1, Text: "alpha2"}}, [][]float32{{0, 0, 1}})
	if err != nil {
		t.Fatal(err)
	}
	if h, _ := s.FileHash(ctx, "a.md"); h != "h3" {
		t.Errorf("hash = %q", h)
	}
	st, _ := s.Stats(ctx)
	if st.Files != 2 || st.Chunks != 2 {
		t.Errorf("stats = %+v", st)
	}

	if err := s.RemoveFile(ctx, "b.md"); err != nil {
		t.Fatal(err)
	}
	hits, _ = s.Search(ctx, []float32{1, 0, 0}, 5)
	if len(hits) != 1 || hits[0].Path != "a.md" {
		t.Fatalf("after remove hits = %+v", hits)
	}
}

func TestCheckModelAndReset(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if err := s.CheckModel("m1"); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckModel("m1"); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckModel("m2"); !errors.Is(err, ErrModelMismatch) {
		t.Fatalf("err = %v", err)
	}
	s.ReplaceFile(ctx, "a.md", "h", 1, []Chunk{{Text: "x"}}, [][]float32{{1, 2}})
	if err := s.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.Model(); m != "" {
		t.Errorf("model after reset = %q", m)
	}
	// A different dimension works after reset.
	if err := s.ReplaceFile(ctx, "a.md", "h", 1, []Chunk{{Text: "x"}}, [][]float32{{1, 2, 3}}); err != nil {
		t.Fatal(err)
	}
}
