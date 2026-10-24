package index

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ncruces/go-sqlite3/driver"
	"github.com/ncruces/go-sqlite3/ext/fts5"

	"github.com/jrapakko/scree/internal/embed"
	"github.com/jrapakko/scree/internal/policy"
	"github.com/jrapakko/scree/internal/store"
)

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"": Auto, "auto": Auto, "hybrid": Hybrid, "Semantic": Semantic, " keyword ": Keyword} {
		if got, err := ParseMode(in); err != nil || got != want {
			t.Errorf("ParseMode(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ParseMode("fuzzy"); err == nil {
		t.Error("ParseMode(fuzzy) succeeded")
	}
}

func TestFTSQuery(t *testing.T) {
	for in, want := range map[string]string{
		"DELETE_STALE_INTAKE":      `"DELETE_STALE_INTAKE"`,
		"what is (D1-Q4)?":         `"what" OR "is" OR "D1-Q4"`,
		`foo" OR "bar`:             `"foo" OR "OR" OR "bar"`,
		`foo"OR"bar`:               `"foo""OR""bar"`,
		"col:text":                 `"col:text"`,
		"-x a* ^x":                 `"-x" OR "a*" OR "^x"`,
		"tab\there\x00z":           `"tab" OR "herez"`,
		"'quoted', [list]; {set}.": `"quoted" OR "list" OR "set"`,
	} {
		if got, ok := ftsQuery(in); !ok || got != want {
			t.Errorf("ftsQuery(%q) = %s, %v; want %s", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "   ", `"""`, "( )", "?!", "\x01"} {
		if got, ok := ftsQuery(in); ok {
			t.Errorf("ftsQuery(%q) = %s, want no terms", in, got)
		}
	}
	many := strings.Repeat("w ", 40)
	if got, _ := ftsQuery(many); strings.Count(got, `"w"`) != maxFTSTerms {
		t.Errorf("40 terms gave %d", strings.Count(got, `"w"`))
	}
	long := strings.Repeat("é", 40) // 80 bytes
	got, _ := ftsQuery(long)
	if term := strings.Trim(got, `"`); len(term) > maxFTSTermBytes || !strings.HasPrefix(long, term) {
		t.Errorf("long term = %q (%d bytes)", term, len(term))
	}
}

// TestFTSQueryEscaping runs FTS operators and quote tricks through search:
// none is a syntax error, and quotes match only literal text.
func TestFTSQueryEscaping(t *testing.T) {
	ix := indexed(t, map[string]string{
		"lit.md":  "# Lit\nthe phrase foo or bar is here\n",
		"foo.md":  "# Foo\nonly foo\n",
		"bar.md":  "# Bar\nonly bar\n",
		"near.md": "# Near\nNEAR AND NOT text\n",
	})
	queries := []string{
		`foo" OR "bar`, `foo"OR"bar`, `NEAR(a b)`, `col:text`, `text:foo`, `a*`, `^x`, `"""`, `-x`, `AND`, `OR`, `NOT`,
		`( )`, `"unbalanced`, `{text heading}: foo`, `foo + bar`, `*`, `""foo""`, `NEAR/2`, `'`, `\"`,
		strings.Repeat("foo ", 33) + "bar", strings.Repeat("x", 500),
	}
	for _, q := range queries {
		if _, err := ix.Search(context.Background(), q, 8, Keyword); err != nil {
			t.Errorf("Search(%q): %v", q, err)
		}
	}
	// One term with quotes inside is a phrase of literal words.
	if got := modePaths(t, ix, `foo"OR"bar`, Keyword); !slices.Equal(got, []string{"lit.md"}) {
		t.Errorf(`foo"OR"bar matched %v, want only the literal phrase`, got)
	}
	// Operators are words.
	if got := modePaths(t, ix, "NEAR", Keyword); !slices.Equal(got, []string{"near.md"}) {
		t.Errorf("NEAR matched %v", got)
	}
}

func modePaths(t *testing.T, ix *Indexer, q string, mode Mode) []string {
	t.Helper()
	hits, err := ix.Search(context.Background(), q, 50, mode)
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

// decoyEmbedder ranks the chunk mentioning "decoy" nearest to every query;
// other chunks get distinct vectors farther away.
type decoyEmbedder struct{ calls int }

func (d *decoyEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	d.calls++
	out := make([][]float32, len(inputs))
	for i, in := range inputs {
		switch {
		case !strings.Contains(in, "\n"): // a query
			out[i] = []float32{1, 0, 0}
		case strings.Contains(in, "decoy"):
			out[i] = []float32{1, 0.01, 0}
		default:
			h := fnv.New32a()
			h.Write([]byte(in))
			a := float64(h.Sum32()%1000) / 1000 * math.Pi / 2
			out[i] = []float32{0.3, float32(math.Cos(a)), float32(math.Sin(a))}
		}
	}
	return out, nil
}

func (d *decoyEmbedder) Model() string { return "decoy" }

func TestHybridIdentifierFirst(t *testing.T) {
	ix, _ := setup(t)
	ix.Embedder = &decoyEmbedder{}
	ids := []string{"DELETE_STALE_INTAKE", "D1-Q4", "S3-CI", "psycopg_pool"}
	write(t, ix.Root(), "decoy.md", "# Decoy\nthe decoy talks about stale intake, pools and quarters\n")
	for i := range 25 {
		write(t, ix.Root(), fmt.Sprintf("n%02d.md", i), fmt.Sprintf("# Note %d\nordinary text number %d about deleting stale things\n", i, i))
	}
	for i, id := range ids {
		write(t, ix.Root(), fmt.Sprintf("id%d.md", i), fmt.Sprintf("# Ops %d\nrun %s when needed\n", i, id))
	}
	if _, err := ix.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n, _ := ix.Store.ChunkCount(context.Background()); n != 30 {
		t.Fatalf("%d chunks, want 30", n)
	}
	for i, id := range ids {
		sem, err := ix.Search(context.Background(), id, 8, Semantic)
		if err != nil {
			t.Fatal(err)
		}
		if sem[0].Path != "decoy.md" {
			t.Fatalf("setup: semantic top for %s is %s, want the decoy", id, sem[0].Path)
		}
		hits, err := ix.Search(context.Background(), id, 8, Hybrid)
		if err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf("id%d.md", i); hits[0].Path != want || hits[0].Match != store.MatchBoth {
			t.Errorf("hybrid %s: top %s (%s), want %s (both)", id, hits[0].Path, hits[0].Match, want)
		}
	}
}

func TestHybridPlainWordsUnchanged(t *testing.T) {
	ix := indexed(t, map[string]string{
		"a.md": "# A\ncherry cherry cherry\n",
		"b.md": "# B\ncherry cherry apple\n",
		"c.md": "# C\ncherry banana date\n",
		"d.md": "# D\napple banana\n",
		"e.md": "# E\ndate date apple\n",
		"f.md": "# F\nbanana banana\n",
		"g.md": "# G\napple\n",
	})
	top3 := func(mode Mode) []string {
		hits, err := ix.Search(context.Background(), "cherry pie", 3, mode)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, h := range hits {
			out = append(out, h.Path)
		}
		return out
	}
	sem, hyb := top3(Semantic), top3(Hybrid)
	if !slices.Equal(sem, []string{"a.md", "b.md", "c.md"}) {
		t.Fatalf("setup: semantic top 3 = %v", sem)
	}
	if !slices.Equal(hyb, sem) {
		t.Errorf("hybrid top 3 = %v, semantic %v", hyb, sem)
	}
}

// failingEmbedder fails every call, like an API outage.
type failingEmbedder struct{ calls int }

func (f *failingEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	f.calls++
	return nil, fmt.Errorf("%w: 503", embed.ErrUnavailable)
}
func (f *failingEmbedder) Model() string { return "fake" }

func TestKeywordModeNoAPI(t *testing.T) {
	ix := indexed(t, map[string]string{"a.md": "# A\nset SCREE_DB first\n", "b.md": "# B\napple\n"})
	down := &failingEmbedder{}
	ix.QueryEmbedder = down
	hits, err := ix.Search(context.Background(), "SCREE_DB", 8, Keyword)
	if err != nil || len(hits) != 1 || hits[0].Path != "a.md" || hits[0].Match != store.MatchKeyword {
		t.Fatalf("keyword = %+v, %v", hits, err)
	}
	if down.calls != 0 {
		t.Errorf("keyword mode called the embedder %d times", down.calls)
	}
	for _, mode := range []Mode{Hybrid, Semantic} {
		_, err = ix.Search(context.Background(), "SCREE_DB", 8, mode)
		if !errors.Is(err, embed.ErrUnavailable) || !strings.Contains(err.Error(), `mode="keyword" still works`) {
			t.Errorf("%s with the API down: %v", mode, err)
		}
	}
}

func TestMatchAttribute(t *testing.T) {
	// The fake embedder only knows fruit words, so "zebra" is found by
	// keyword alone, apple by both, and banana (absent from the query's
	// words) by vector alone.
	ix := indexed(t, map[string]string{
		"a.md": "# A\napple zebra\n",
		"b.md": "# B\nbanana\n",
	})
	match := func(q string, mode Mode) map[string]store.Match {
		hits, err := ix.Search(context.Background(), q, 8, mode)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]store.Match{}
		for _, h := range hits {
			m[h.Path] = h.Match
		}
		return m
	}
	if got := match("apple", Hybrid); got["a.md"] != store.MatchBoth || got["b.md"] != store.MatchSemantic {
		t.Errorf("hybrid apple = %v", got)
	}
	if got := match("zebra", Keyword); got["a.md"] != store.MatchKeyword || len(got) != 1 {
		t.Errorf("keyword zebra = %v", got)
	}
	if got := match("zebra", Semantic); got["a.md"] != store.MatchSemantic {
		t.Errorf("semantic zebra = %v", got)
	}
}

func TestFuseOrder(t *testing.T) {
	h := func(id int64) store.Hit { return store.Hit{ID: id, Match: store.MatchSemantic} }
	k := func(id int64) store.Hit { return store.Hit{ID: id, Match: store.MatchKeyword} }
	got := fuse([]store.Hit{h(1), h(2), h(3)}, []store.Hit{k(4), k(3)})
	var ids []int64
	for _, x := range got {
		ids = append(ids, x.ID)
	}
	// 3 is in both lists; 1 and 4 tie on score with best rank 1 each, so
	// the lower id goes first; 2 is rank 2 in one list.
	if !slices.Equal(ids, []int64{3, 1, 4, 2}) {
		t.Errorf("fused order = %v", ids)
	}
	if got[0].Match != store.MatchBoth || got[2].Match != store.MatchKeyword {
		t.Errorf("matches = %v, %v", got[0].Match, got[2].Match)
	}
}

// TestKeywordModeRechecks: keyword hits go through the same policy
// re-check, current line ranges and duplicate folding as vector hits.
func TestKeywordModeRechecks(t *testing.T) {
	files := dupFiles("docs/print/x.md", "docs/x.md")
	files["a.md"] = "# A\nkiwi pie\n"
	files["gone.md"] = "# G\nkiwi tart\n"
	files["private/p.md"] = "# P\nkiwi secret\n"
	ix := indexed(t, files)
	write(t, ix.Root(), "a.md", strings.Repeat("filler\n", 10)+"# A\nkiwi pie\n")
	write(t, ix.Root(), "gone.md", "# G\nsomething else\n")
	write(t, ix.Root(), ".screeignore", "private/\n")
	var queued []string
	ix.OnStale = func(p []string) { queued = append(queued, p...) }
	hits, err := ix.Search(context.Background(), "kiwi setup", 8, Keyword)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]store.Hit{}
	for _, h := range hits {
		got[h.Path] = h
	}
	if _, ok := got["private/p.md"]; ok {
		t.Error("newly ignored file returned")
	}
	if h := got["a.md"]; h.Line != 11 || h.EndLine != 12 || h.Stale {
		t.Errorf("a.md: %d-%d stale=%v", h.Line, h.EndLine, h.Stale)
	}
	if h := got["gone.md"]; !h.Stale || h.Line != 0 {
		t.Errorf("gone.md: %+v", h)
	}
	if h, ok := got["docs/x.md"]; !ok || len(h.Also) != 1 || h.Also[0].Path != "docs/print/x.md" {
		t.Errorf("duplicates not folded: %+v", hits)
	}
	if _, ok := got["docs/print/x.md"]; ok {
		t.Error("copy returned as its own hit")
	}
	slices.Sort(queued)
	if !slices.Equal(queued, []string{"a.md", "gone.md", "private/p.md"}) {
		t.Errorf("queued %v", queued)
	}
}

// TestMigrateFTSFromIndex: an index from before the keyword index is
// upgraded on open from its stored chunks, with no embedding call, and
// keyword search finds an identifier in it.
func TestMigrateFTSFromIndex(t *testing.T) {
	ctx := context.Background()
	ix, fe := setup(t)
	write(t, ix.Root(), "a.md", "# Env\nexport CLAUDE_PROJECT_DIR=/x\n")
	if _, err := ix.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(filepath.Dir(ix.Root()), "index.db")
	ix.Store.Close()
	db, err := driver.Open("file:"+(&url.URL{Path: dbPath}).EscapedPath(), fts5.Register)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TRIGGER chunks_ai; DROP TRIGGER chunks_ad; DROP TRIGGER chunks_au; DROP TABLE chunks_fts;
		UPDATE meta SET value = '2' WHERE key = 'schema'`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ix.Store = s
	fe.calls = 0
	hits, err := ix.Search(ctx, "CLAUDE_PROJECT_DIR", 8, Keyword)
	if err != nil || len(hits) != 1 || hits[0].Path != "a.md" {
		t.Fatalf("keyword after migration = %+v, %v", hits, err)
	}
	if st, err := ix.Sync(ctx); err != nil || st.Indexed != 0 {
		t.Fatalf("Sync = %+v, %v", st, err)
	}
	if fe.calls != 0 {
		t.Errorf("embedder called %d times", fe.calls)
	}
	if v, _ := s.Meta("schema"); v != store.SchemaVersion {
		t.Errorf("schema = %q", v)
	}
}

// TestRepoDocsIdentifiers is an eval with no API: this repo's README.md
// and docs/SPEC.md are indexed with a fake embedder, and for each query in
// testdata/eval/identifiers.tsv, keyword mode must put a chunk of the
// expected file, under a heading containing the expected text, in the top
// 3. The mean reciprocal rank (over the top 10) is logged.
func TestRepoDocsIdentifiers(t *testing.T) {
	ix, _ := setup(t)
	for _, rel := range []string{"README.md", "docs/SPEC.md"} {
		src, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		write(t, ix.Root(), rel, string(src))
	}
	if _, err := ix.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join("testdata", "eval", "identifiers.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var rr float64
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cols := strings.Split(line, "\t")
		if len(cols) != 3 {
			t.Fatalf("bad row %q", line)
		}
		q, file, heading := cols[0], cols[1], cols[2]
		hits, err := ix.Search(context.Background(), q, 10, Keyword)
		if err != nil {
			t.Fatal(err)
		}
		rank := 0
		for i, h := range hits {
			if h.Path == file && strings.Contains(h.Heading, heading) {
				rank = i + 1
				break
			}
		}
		n++
		t.Logf("%-22s rank %d", q, rank)
		if rank > 0 {
			rr += 1 / float64(rank)
		}
		if rank == 0 || rank > 3 {
			var top []string
			for _, h := range hits[:min(3, len(hits))] {
				top = append(top, h.Path+" > "+h.Heading)
			}
			t.Errorf("%s: expected %s %q at rank %d; top 3 %q", q, file, heading, rank, top)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if n < 10 {
		t.Fatalf("only %d queries", n)
	}
	t.Logf("keyword identifier MRR over %d queries: %.3f", n, rr/float64(n))
}

func TestHasIdentifier(t *testing.T) {
	for q, want := range map[string]bool{
		"DELETE_STALE_INTAKE": true,
		"psycopg_pool":        true,
		"D1-Q4":               true,
		"S3-CI":               true,
		"RT2-13":              true,
		"v2":                  true,
		"what sets `IDLE_IN_TRANSACTION_TIMEOUT_MS`?": true,
		"is (D1-Q4) done?":                            true,
		`"psycopg_pool",`:                             true,
		"ID":                                          false,
		"ORM":                                         false,
		"API":                                         false,
		"2026":                                        false,
		"well-known":                                  false,
		"psycopg":                                     false,
		"how do I rotate the backup keys?":            false,
		"what changed in 2026, and why?":              false,
		"":                                            false,
		"_ __ 42":                                     false,
	} {
		if got := HasIdentifier(q); got != want {
			t.Errorf("HasIdentifier(%q) = %v, want %v", q, got, want)
		}
	}
}

func TestResolve(t *testing.T) {
	for _, c := range []struct {
		mode  Mode
		query string
		want  Mode
	}{
		{Auto, "how do backups work", Semantic},
		{"", "how do backups work", Semantic},
		{Auto, "what is D1-Q4", Hybrid},
		{"", "psycopg_pool sizing", Hybrid},
		{Hybrid, "how do backups work", Hybrid},
		{Semantic, "D1-Q4", Semantic},
		{Keyword, "how do backups work", Keyword},
	} {
		if got := Resolve(c.mode, c.query); got != c.want {
			t.Errorf("Resolve(%q, %q) = %q, want %q", c.mode, c.query, got, c.want)
		}
	}
}

// TestAutoMode: auto runs no keyword query for a plain question and gives
// semantic's results; for an identifier it gives hybrid's.
func TestAutoMode(t *testing.T) {
	ix, _ := setup(t)
	ix.Embedder = &decoyEmbedder{}
	write(t, ix.Root(), "decoy.md", "# Decoy\nthe decoy talks about rotation\n")
	write(t, ix.Root(), "id.md", "# Ops\nrun DELETE_STALE_INTAKE when needed\n")
	write(t, ix.Root(), "plain.md", "# Plain\nrotate the backup keys\n")
	if _, err := ix.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	keyword := 0
	TestHooks.Keyword = func(string) { keyword++ }
	defer func() { TestHooks.Keyword = nil }()
	paths := func(q string, mode Mode) []string {
		hits, err := ix.Search(context.Background(), q, 8, mode)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, h := range hits {
			out = append(out, h.Path+":"+string(h.Match))
		}
		return out
	}
	plain := "how do I rotate the backup keys"
	got := paths(plain, Auto)
	if keyword != 0 {
		t.Errorf("auto ran %d keyword queries for a plain question", keyword)
	}
	if want := paths(plain, Semantic); !slices.Equal(got, want) {
		t.Errorf("auto plain = %v, semantic %v", got, want)
	}
	id := "when does DELETE_STALE_INTAKE run"
	got = paths(id, Auto)
	if keyword != 1 {
		t.Errorf("auto ran %d keyword queries for an identifier", keyword)
	}
	if want := paths(id, Hybrid); !slices.Equal(got, want) || !strings.HasPrefix(got[0], "id.md:") {
		t.Errorf("auto identifier = %v, hybrid %v", got, want)
	}
	// Explicit modes are untouched by the rule.
	keyword = 0
	if got := paths(plain, Hybrid); keyword != 1 || len(got) == 0 {
		t.Errorf("hybrid plain: %d keyword queries, %v", keyword, got)
	}
	if got := paths(id, Keyword); len(got) != 1 || got[0] != "id.md:keyword" {
		t.Errorf("keyword = %v", got)
	}
}

func TestAutoKeepsKeywordHint(t *testing.T) {
	ix := indexed(t, map[string]string{"a.md": "# A\nset SCREE_DB first\n"})
	ix.QueryEmbedder = &failingEmbedder{}
	for _, q := range []string{"where is the database", "SCREE_DB"} {
		_, err := ix.Search(context.Background(), q, 8, Auto)
		if !errors.Is(err, embed.ErrUnavailable) || !strings.Contains(err.Error(), `mode="keyword" still works`) {
			t.Errorf("auto %q with the API down: %v", q, err)
		}
	}
}

// TestCollapseRelocatesCopies: a listed copy whose file changed gets its
// current range, or Stale when its text is gone, and is queued.
func TestCollapseRelocatesCopies(t *testing.T) {
	ix := indexed(t, dupFiles("docs/x.md", "docs/a/x.md", "docs/b/x.md"))
	write(t, ix.Root(), "docs/a/x.md", strings.Repeat("filler\n", 5)+"# Setup\napple setup steps\n")
	write(t, ix.Root(), "docs/b/x.md", "# Setup\nrewritten\n")
	var queued []string
	ix.OnStale = func(p []string) { queued = append(queued, p...) }
	hits, err := ix.Search(context.Background(), "apple", 8, Semantic)
	if err != nil || len(hits) != 1 || hits[0].Path != "docs/x.md" {
		t.Fatalf("hits = %+v, %v", hits, err)
	}
	want := []store.Copy{{Path: "docs/a/x.md", Line: 6, EndLine: 7}, {Path: "docs/b/x.md", Stale: true}}
	if !slices.Equal(hits[0].Also, want) {
		t.Errorf("also = %+v, want %+v", hits[0].Also, want)
	}
	slices.Sort(queued)
	if !slices.Equal(queued, []string{"docs/a/x.md", "docs/b/x.md"}) {
		t.Errorf("queued %v", queued)
	}
}

// TestBadIgnoreLineFailsClosed: a .screeignore line scree cannot parse
// stops indexing (Sync and IndexFile), so a broken rule never lets a
// private file be embedded; fixing it resumes.
func TestBadIgnoreLineFailsClosed(t *testing.T) {
	ix, fe := setup(t)
	write(t, ix.Root(), ".screeignore", "ok\n[secret\n")
	write(t, ix.Root(), "a.md", "# A\napple\n")
	if _, err := ix.Sync(context.Background()); !errors.Is(err, policy.ErrBadIgnoreFile) || !strings.Contains(err.Error(), "line 2: bad pattern") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("Sync = %v", err)
	}
	if _, err := ix.IndexFile(context.Background(), "a.md"); !errors.Is(err, policy.ErrBadIgnoreFile) {
		t.Errorf("IndexFile = %v", err)
	}
	if fe.calls != 0 {
		t.Errorf("embedded %d times", fe.calls)
	}
	write(t, ix.Root(), ".screeignore", "ok\n")
	if st, err := ix.Sync(context.Background()); err != nil || st.Indexed != 1 {
		t.Errorf("after fix: %+v, %v", st, err)
	}
}
