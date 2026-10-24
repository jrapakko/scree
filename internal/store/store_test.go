package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

	err := s.ReplaceFile(ctx, "a.md", "h1", 1, nil,
		[]Chunk{{Heading: "A", Line: 1, Text: "alpha"}, {Heading: "B", Line: 5, Text: "beta"}},
		[][]float32{{1, 0, 0}, {0, 1, 0}})
	if err != nil {
		t.Fatal(err)
	}
	err = s.ReplaceFile(ctx, "b.md", "h2", 1, nil,
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
	err = s.ReplaceFile(ctx, "a.md", "h3", 2, nil,
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
	s.ReplaceFile(ctx, "a.md", "h", 1, nil, []Chunk{{Text: "x"}}, [][]float32{{1, 2}})
	if err := s.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.Model(); m != "" {
		t.Errorf("model after reset = %q", m)
	}
	// A different dimension works after reset.
	if err := s.ReplaceFile(ctx, "a.md", "h", 1, nil, []Chunk{{Text: "x"}}, [][]float32{{1, 2, 3}}); err != nil {
		t.Fatal(err)
	}
}

func TestFilePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	path := filepath.Join(dir, "index.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	err = s.ReplaceFile(context.Background(), "a.md", "h", 1, nil, []Chunk{{Text: "x"}}, [][]float32{{1, 0}})
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v, %v", fi.Mode().Perm(), err)
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v", filepath.Base(p), fi.Mode().Perm())
		}
	}
}

func TestOpenLeavesExistingDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o755 {
		t.Errorf("existing dir mode changed to %v", fi.Mode().Perm())
	}
}

func TestCheckFile(t *testing.T) {
	for _, replace := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "index.db")
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.CheckFile(); err != nil {
			t.Fatal(err)
		}
		os.Remove(path)
		if replace {
			os.WriteFile(path, nil, 0o600)
		}
		if err := s.CheckFile(); !errors.Is(err, ErrIndexGone) {
			t.Errorf("replace=%v: CheckFile = %v", replace, err)
		}
		s.Close()
	}
}

func TestMeta(t *testing.T) {
	s := open(t)
	if v, err := s.Meta("k"); err != nil || v != "" {
		t.Fatalf("unset Meta = %q, %v", v, err)
	}
	for _, want := range []string{"one", "two"} {
		if err := s.SetMeta("k", want); err != nil {
			t.Fatal(err)
		}
		if v, err := s.Meta("k"); err != nil || v != want {
			t.Fatalf("Meta = %q, %v, want %q", v, err, want)
		}
	}
}

func TestCheckRoot(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if err := s.CheckRoot("/a"); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Meta("root"); v != "/a" {
		t.Fatalf("root = %q", v)
	}
	if err := s.CheckRoot("/a"); err != nil {
		t.Fatal(err)
	}
	err := s.CheckRoot("/b")
	if !errors.Is(err, ErrRootMismatch) || !strings.Contains(err.Error(), "built for /a, not /b") {
		t.Fatalf("err = %v", err)
	}

	// A legacy index has files but no recorded root: it is adopted.
	legacy := open(t)
	if err := legacy.ReplaceFile(ctx, "a.md", "h", 1, nil, []Chunk{{Text: "x"}}, [][]float32{{1, 2}}); err != nil {
		t.Fatal(err)
	}
	if err := legacy.CheckRoot("/notes"); err != nil {
		t.Fatal(err)
	}
	if err := legacy.CheckRoot("/other"); !errors.Is(err, ErrRootMismatch) {
		t.Fatalf("after adoption: err = %v", err)
	}
}

func TestStoreMigratesFileColumns(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceFile(ctx, "old.md", "h", 1, nil, []Chunk{{Path: "old.md", Text: "x"}}, [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	// Make it look like an index from before the columns existed.
	for _, col := range []string{"mtime", "size"} {
		if _, err := s.db.Exec(`ALTER TABLE files DROP COLUMN ` + col); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h, st, err := s.FileInfo(ctx, "old.md")
	if err != nil || h != "h" || st != nil {
		t.Fatalf("legacy row: %q, %v, %v", h, st, err)
	}
	want := FileStat{Mtime: 123, Size: 4}
	if err := s.ReplaceFile(ctx, "new.md", "h2", 1, &want, []Chunk{{Path: "new.md", Text: "y"}}, [][]float32{{0, 1}}); err != nil {
		t.Fatal(err)
	}
	hits, err := s.Search(ctx, []float32{0, 1}, 2)
	if err != nil || len(hits) != 2 {
		t.Fatalf("Search = %v, %v", hits, err)
	}
	for _, h := range hits {
		switch h.Path {
		case "new.md":
			if h.Stat == nil || *h.Stat != want {
				t.Errorf("new.md stat = %v", h.Stat)
			}
		case "old.md":
			if h.Stat != nil {
				t.Errorf("old.md stat = %v, want nil", h.Stat)
			}
		}
	}
	if err := s.SetFileStat(ctx, "old.md", want); err != nil {
		t.Fatal(err)
	}
	if _, st, _ := s.FileInfo(ctx, "old.md"); st == nil || *st != want {
		t.Errorf("after SetFileStat: %v", st)
	}
}

func TestConcurrentOpenFresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new", "index.db")
	const n = 8
	errs := make(chan error, n)
	stores := make(chan *Store, n)
	for range n {
		go func() {
			s, err := Open(path)
			if err == nil {
				err = s.CheckModel("m")
			}
			if err == nil {
				err = s.CheckRoot("/r")
			}
			errs <- err
			stores <- s
		}()
	}
	for range n {
		if err := <-errs; err != nil {
			t.Error(err)
		}
		if s := <-stores; s != nil {
			defer s.Close()
		}
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var tables int
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = 'vecs'`).Scan(&tables); err != nil || tables != 1 {
		t.Errorf("vecs tables = %d, %v", tables, err)
	}
	if m, _ := s.Model(); m != "m" {
		t.Errorf("model = %q", m)
	}
}

func TestCreateVecsWhenPresent(t *testing.T) {
	s := open(t)
	// Opening an existing index runs init again: the table is found, not
	// created twice, in either order of two openers.
	for range 2 {
		if err := s.init(); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := createVecs(ctx, conn); err != nil {
		t.Fatal(err)
	}
}

func TestMatchModelAndRootNeverWrite(t *testing.T) {
	s := open(t)
	if err := s.MatchModel("m"); err != nil {
		t.Fatal(err)
	}
	if err := s.MatchRoot("/r"); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.Model(); m != "" {
		t.Errorf("MatchModel recorded %q", m)
	}
	if r, _ := s.Meta("root"); r != "" {
		t.Errorf("MatchRoot recorded %q", r)
	}
	s.CheckModel("m")
	s.CheckRoot("/r")
	if err := s.MatchModel("other"); !errors.Is(err, ErrModelMismatch) {
		t.Errorf("MatchModel = %v", err)
	}
	if err := s.MatchRoot("/other"); !errors.Is(err, ErrRootMismatch) {
		t.Errorf("MatchRoot = %v", err)
	}
}
