package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestFTS5Available: the driver's build has FTS5 with both the unicode61
// and trigram tokenizers, and unicode61 with "_" and "-" as token
// characters keeps identifiers whole.
func TestFTS5Available(t *testing.T) {
	s := open(t)
	for _, q := range []string{
		`CREATE VIRTUAL TABLE t_uni USING fts5(x, tokenize="unicode61 remove_diacritics 2 tokenchars '_-'")`,
		`CREATE VIRTUAL TABLE t_tri USING fts5(x, tokenize=trigram)`,
		`CREATE VIRTUAL TABLE t_vocab USING fts5vocab(t_uni, row)`,
		`INSERT INTO t_uni(x) VALUES('Run PURGE_OLD_RECORDS after R7-Q2 with psycopg_pool. Café')`,
		`INSERT INTO t_tri(x) VALUES('psycopg_pool')`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v (does the SQLite build lack FTS5 or this tokenizer?)", q, err)
		}
	}
	rows, err := s.db.Query(`SELECT term FROM t_vocab ORDER BY term`)
	if err != nil {
		t.Fatal(err)
	}
	var terms []string
	for rows.Next() {
		var term string
		rows.Scan(&term)
		terms = append(terms, term)
	}
	rows.Close()
	for _, want := range []string{"purge_old_records", "r7-q2", "psycopg_pool", "cafe"} {
		if !slices.Contains(terms, want) {
			t.Errorf("token %q missing; tokens %q", want, terms)
		}
	}
	for _, part := range []string{"delete", "stale", "d1", "pool"} {
		if slices.Contains(terms, part) {
			t.Errorf("identifier split: token %q in %q", part, terms)
		}
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM t_tri WHERE t_tri MATCH '"pool"'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("trigram match = %d, %v", n, err)
	}
}

// ftsMatches returns the paths of chunks the keyword index matches for the
// quoted term.
func ftsMatches(t *testing.T, s *Store, term string) []string {
	t.Helper()
	hits, err := s.Keyword(context.Background(), `"`+term+`"`, 50)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, h := range hits {
		out = append(out, h.Path)
	}
	slices.Sort(out)
	return out
}

func checkFTS(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	n, err := s.KeywordRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := s.ChunkCount(ctx); c != n {
		t.Errorf("keyword rows %d, chunks %d", n, c)
	}
	if err := s.CheckKeywordIndex(ctx); err != nil {
		t.Errorf("integrity-check: %v", err)
	}
}

func TestFTSSyncedByTriggers(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	checkFTS(t, s)
	if err := s.ReplaceFile(ctx, "a.md", "h", 1, nil,
		[]Chunk{{Heading: "Alpha", Text: "uses SCREE_DB"}, {Heading: "B", Text: "other words"}},
		[][]float32{{1, 0}, {0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceFile(ctx, "docs/ADR-0007.md", "h", 1, nil,
		[]Chunk{{Heading: "Decision", Text: "SCREE_DB too"}}, [][]float32{{1, 1}}); err != nil {
		t.Fatal(err)
	}
	checkFTS(t, s)
	if got := ftsMatches(t, s, "scree_db"); !slices.Equal(got, []string{"a.md", "docs/ADR-0007.md"}) {
		t.Errorf("scree_db = %v", got)
	}
	if got := ftsMatches(t, s, "adr-0007"); !slices.Equal(got, []string{"docs/ADR-0007.md"}) {
		t.Errorf("path term = %v", got)
	}
	if got := ftsMatches(t, s, "alpha"); !slices.Equal(got, []string{"a.md"}) {
		t.Errorf("heading term = %v", got)
	}

	// Replacing drops the old text.
	if err := s.ReplaceFile(ctx, "a.md", "h2", 1, nil, []Chunk{{Heading: "A", Text: "now different"}}, [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	checkFTS(t, s)
	if got := ftsMatches(t, s, "scree_db"); !slices.Equal(got, []string{"docs/ADR-0007.md"}) {
		t.Errorf("after replace = %v", got)
	}
	// SetLines touches no indexed column.
	if err := s.SetLines(ctx, "a.md", []LineRange{{Line: 2, EndLine: 3}}); err != nil {
		t.Fatal(err)
	}
	checkFTS(t, s)

	if err := s.RemoveFile(ctx, "docs/ADR-0007.md"); err != nil {
		t.Fatal(err)
	}
	checkFTS(t, s)
	if got := ftsMatches(t, s, "scree_db"); got != nil {
		t.Errorf("after remove = %v", got)
	}

	if err := s.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	checkFTS(t, s)
	if got := ftsMatches(t, s, "different"); got != nil {
		t.Errorf("after reset = %v", got)
	}
	if err := s.ReplaceFile(ctx, "c.md", "h", 1, nil, []Chunk{{Text: "fresh SCREE_DB"}}, [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	checkFTS(t, s)
	if got := ftsMatches(t, s, "scree_db"); !slices.Equal(got, []string{"c.md"}) {
		t.Errorf("after reset and insert = %v", got)
	}
}

// makeSchema2 turns s's index into one from before the keyword index.
func makeSchema2(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec(dropFTS + `UPDATE meta SET value = '2' WHERE key = 'schema'`); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateFTSFromExisting(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceFile(ctx, "a.md", "h", 1, nil, []Chunk{{Heading: "Env", Text: "set CLAUDE_PROJECT_DIR"}}, [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	makeSchema2(t, s)
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v, _ := s.Meta("schema"); v != SchemaVersion {
		t.Errorf("schema = %q", v)
	}
	checkFTS(t, s)
	if got := ftsMatches(t, s, "claude_project_dir"); !slices.Equal(got, []string{"a.md"}) {
		t.Errorf("migrated match = %v", got)
	}
}

// TestMigrateFTSTiming builds the keyword index for 100k stored chunks, as
// the upgrade of a large index does on open. Set SCREE_FTS_TIMING=1 to run.
func TestMigrateFTSTiming(t *testing.T) {
	if os.Getenv("SCREE_FTS_TIMING") == "" {
		t.Skip("set SCREE_FTS_TIMING=1")
	}
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	makeSchema2(t, s)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	words := strings.Fields("the backup keys rotate every quarter using SCREE_DB and R7-Q2 notes about psycopg_pool configuration with headings and paths that vary across a folder of markdown documents")
	const n = 100_000
	for i := range n {
		var b strings.Builder
		for j := range 120 { // about 800 bytes, a typical chunk
			b.WriteString(words[(i*7+j*13)%len(words)])
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "id%d", i)
		if _, err := tx.Exec(`INSERT INTO chunks(path, ord, heading, line, text, text_hash) VALUES(?, 0, ?, 1, ?, 'x')`,
			fmt.Sprintf("dir%d/file%d.md", i%100, i), fmt.Sprintf("Heading %d", i), b.String()); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	before, _ := os.Stat(path)
	start := time.Now()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	defer s.Close()
	s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	after, _ := os.Stat(path)
	t.Logf("FTS rebuild of %d chunks on open: %v; file %d -> %d bytes (+%.0f%%)",
		n, took, before.Size(), after.Size(), 100*float64(after.Size()-before.Size())/float64(before.Size()))
	if rows, _ := s.KeywordRows(context.Background()); rows != n {
		t.Errorf("keyword rows = %d", rows)
	}
}

// TestPeekNeverWrites: Peek reads an older index without migrating it,
// and fails on a missing file instead of creating it.
func TestPeekNeverWrites(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceFile(ctx, "a.md", "h", 1, nil, []Chunk{{Text: "x"}, {Text: "y"}}, [][]float32{{1, 0}, {0, 1}}); err != nil {
		t.Fatal(err)
	}
	s.CheckModel("m")
	s.CheckRoot("/r")
	s.SetMeta("synced_at", "2026-10-24T00:00:00Z")
	makeSchema2(t, s)
	s.Close()
	before, _ := os.ReadFile(path)
	for _, p := range []string{path + "-wal", path + "-shm"} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("setup: %s exists before Peek", filepath.Base(p))
		}
	}

	sum, err := Peek(path)
	if err != nil {
		t.Fatal(err)
	}
	want := Summary{Root: "/r", Model: "m", SyncedAt: "2026-10-24T00:00:00Z", Schema: "2", Files: 1, Chunks: 2}
	if sum != want {
		t.Errorf("Peek = %+v, want %+v", sum, want)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Error("Peek changed the index file")
	}
	for _, p := range []string{path + "-wal", path + "-shm"} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("Peek created %s", filepath.Base(p))
		}
	}
	missing := filepath.Join(t.TempDir(), "none.db")
	if _, err := Peek(missing); err == nil {
		t.Error("Peek of a missing file succeeded")
	}
	if _, err := os.Stat(missing); err == nil {
		t.Error("Peek created a missing index")
	}

	// With a writer's connection open, Peek reads through the WAL and sees
	// rows not yet checkpointed.
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.ReplaceFile(ctx, "b.md", "h", 1, nil, []Chunk{{Text: "z"}}, [][]float32{{1, 1}}); err != nil {
		t.Fatal(err)
	}
	if sum, err := Peek(path); err != nil || sum.Files != 2 || sum.Chunks != 3 || sum.Schema != SchemaVersion {
		t.Errorf("Peek while open = %+v, %v", sum, err)
	}
}

// TestMatchTextsAgreesWithKeyword: every chunk keyword search returns for
// a word has a line, its path or its heading matched by MatchTexts, so the
// text view can always say where the word is.
func TestMatchTextsAgreesWithKeyword(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	files := map[string][]Chunk{
		"ci/README.md": {{Heading: "Pipeline", Text: "the build\nruns on push"}},
		"docs/release.md": {
			{Heading: "Release", Text: "see .forgejo/workflows/ci.yml\nfor the job"},
			{Heading: "Café notes", Text: "run PURGE_OLD_RECORDS\nafter R7-Q2"},
		},
		"docs/db.md": {{Heading: "DB", Text: "seed the database\nwith fixtures; circle back"}},
	}
	for p, cs := range files {
		vecs := make([][]float32, len(cs))
		for i := range vecs {
			vecs[i] = []float32{1, 0}
		}
		if err := s.ReplaceFile(ctx, p, "h", 1, nil, cs, vecs); err != nil {
			t.Fatal(err)
		}
	}
	for _, w := range []string{"ci", "seed", "cafe", "purge_old_records", "r7-q2", "ci.yml", "md", "docs", "pipeline", "the", "circle"} {
		hits, err := s.Keyword(ctx, phrase(w), 50)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) == 0 {
			t.Errorf("%q: no keyword hits", w)
		}
		for _, h := range hits {
			texts := append(strings.Split(h.Text, "\n"), h.Path, h.Heading)
			m, err := MatchTexts(ctx, []string{w}, texts)
			if err != nil {
				t.Fatal(err)
			}
			if len(m[0]) == 0 {
				t.Errorf("%q: keyword hit %s %q, but MatchTexts finds nothing in %q", w, h.Path, h.Heading, texts)
			}
		}
	}
}
