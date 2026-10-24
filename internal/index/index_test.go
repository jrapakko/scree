package index

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jrapakko/scree/internal/embed"
	"github.com/jrapakko/scree/internal/policy"
	"github.com/jrapakko/scree/internal/store"
)

// fakeEmbedder maps text to a vector of keyword counts, which is enough to
// check that search finds the right chunk.
type fakeEmbedder struct{ calls int }

var keywords = []string{"apple", "banana", "cherry", "date"}

func (f *fakeEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	f.calls++
	out := make([][]float32, len(inputs))
	for i, in := range inputs {
		v := make([]float32, len(keywords))
		for j, k := range keywords {
			v[j] = float32(strings.Count(strings.ToLower(in), k)) + 0.01
		}
		out[i] = v
	}
	return out, nil
}

func (f *fakeEmbedder) Model() string { return "fake" }

func setup(t *testing.T) (*Indexer, *fakeEmbedder) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "notes")
	os.MkdirAll(filepath.Join(root, "sub"), 0o755)
	os.MkdirAll(filepath.Join(root, ".obsidian"), 0o755)
	s, err := store.Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	p, err := policy.Open(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	fe := &fakeEmbedder{}
	return &Indexer{Policy: p, Store: s, Embedder: fe}, fe
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, filepath.FromSlash(rel))), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSyncIndexesChangesAndRemovals(t *testing.T) {
	ctx := context.Background()
	ix, fe := setup(t)
	write(t, ix.Root(), "fruit.md", "# Apples\napple apple\n\n# Bananas\nbanana\n")
	write(t, ix.Root(), "sub/cherry.md", "# Cherry\ncherry cherry cherry\n")
	write(t, ix.Root(), ".obsidian/hidden.md", "# Hidden\ndate\n")
	write(t, ix.Root(), "notes.txt", "date")

	st, err := ix.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st != (SyncStats{Scanned: 2, Indexed: 2}) {
		t.Fatalf("first sync = %+v", st)
	}

	hits, err := ix.Search(ctx, "cherry", 1, Hybrid)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Path != "sub/cherry.md" || hits[0].Heading != "Cherry" {
		t.Fatalf("hits = %+v", hits)
	}

	// Unchanged files are not re-embedded.
	calls := fe.calls
	if st, _ := ix.Sync(ctx); st.Indexed != 0 || fe.calls != calls {
		t.Fatalf("second sync = %+v, embed calls %d -> %d", st, calls, fe.calls)
	}

	write(t, ix.Root(), "fruit.md", "# Dates\ndate date\n")
	os.Remove(filepath.Join(ix.Root(), "sub/cherry.md"))
	st, _ = ix.Sync(ctx)
	if st.Indexed != 1 || st.Removed != 1 {
		t.Fatalf("third sync = %+v", st)
	}
	hits, _ = ix.Search(ctx, "date", 5, Hybrid)
	if len(hits) != 1 || hits[0].Heading != "Dates" {
		t.Fatalf("hits after change = %+v", hits)
	}
}

func TestIndexFileMissingRemoves(t *testing.T) {
	ctx := context.Background()
	ix, _ := setup(t)
	write(t, ix.Root(), "sub/a.md", "# A\napple\n")
	write(t, ix.Root(), "sub/b.md", "# B\nbanana\n")
	ix.Sync(ctx)

	os.Remove(filepath.Join(ix.Root(), "sub/a.md"))
	if changed, err := ix.IndexFile(ctx, "sub/a.md"); err != nil || !changed {
		t.Fatalf("IndexFile on missing = %v, %v", changed, err)
	}
	if err := ix.RemovePrefix(ctx, "sub"); err != nil {
		t.Fatal(err)
	}
	if paths, _ := ix.Store.Paths(ctx); len(paths) != 0 {
		t.Fatalf("paths = %v", paths)
	}
}

func symlink(t *testing.T, target, name string) {
	t.Helper()
	if err := os.Symlink(target, name); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

func paths(t *testing.T, ix *Indexer) []string {
	t.Helper()
	p, err := ix.Store.Paths(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(p)
	return p
}

func TestIndexFileRefusesOutsideSymlink(t *testing.T) {
	ctx := context.Background()
	ix, fe := setup(t)
	outside := filepath.Join(filepath.Dir(ix.Root()), "outside")
	write(t, outside, "secret.md", "# Secret\napple\n")
	symlink(t, filepath.Join(outside, "secret.md"), filepath.Join(ix.Root(), "x.md"))
	symlink(t, outside, filepath.Join(ix.Root(), "d"))
	for _, rel := range []string{"x.md", "d/secret.md"} {
		_, err := ix.IndexFile(ctx, rel)
		var ie *policy.IgnoredError
		if !errors.As(err, &ie) || ie.Reason != policy.OutsideRoot {
			t.Errorf("IndexFile(%s) = %v, want OutsideRoot", rel, err)
		}
	}
	if fe.calls != 0 || len(paths(t, ix)) != 0 {
		t.Fatalf("embed calls %d, paths %v", fe.calls, paths(t, ix))
	}
}

func TestSyncSkipsDirSymlinkOutside(t *testing.T) {
	ix, fe := setup(t)
	outside := filepath.Join(filepath.Dir(ix.Root()), "outside")
	write(t, outside, "secret.md", "# Secret\napple\n")
	symlink(t, outside, filepath.Join(ix.Root(), "d"))
	symlink(t, filepath.Join(outside, "secret.md"), filepath.Join(ix.Root(), "x.md"))
	write(t, ix.Root(), "ok.md", "# Ok\nbanana\n")
	st, err := ix.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st != (SyncStats{Scanned: 1, Indexed: 1}) || !slices.Equal(paths(t, ix), []string{"ok.md"}) || fe.calls != 1 {
		t.Fatalf("sync = %+v, paths %v, calls %d", st, paths(t, ix), fe.calls)
	}
}

func TestSyncFollowsInRootSymlink(t *testing.T) {
	ix, _ := setup(t)
	write(t, ix.Root(), "sub/a.md", "# A\napple\n")
	symlink(t, "sub/a.md", filepath.Join(ix.Root(), "alias.md"))
	symlink(t, "sub", filepath.Join(ix.Root(), "subdir")) // folder links are not descended
	if _, err := ix.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := paths(t, ix); !slices.Equal(got, []string{"alias.md", "sub/a.md"}) {
		t.Fatalf("paths = %v", got)
	}
}

func TestSyncScreeignore(t *testing.T) {
	ctx := context.Background()
	ix, _ := setup(t)
	write(t, ix.Root(), "journal/day.md", "# Day\napple\n")
	write(t, ix.Root(), "journal/public.md", "# Public\nbanana\n")
	write(t, ix.Root(), "keep.md", "# Keep\ncherry\n")
	if _, err := ix.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if len(paths(t, ix)) != 3 {
		t.Fatalf("paths = %v", paths(t, ix))
	}

	write(t, ix.Root(), ".screeignore", "journal/\n!journal/public.md\n")
	st, err := ix.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Removed != 1 || !slices.Equal(paths(t, ix), []string{"journal/public.md", "keep.md"}) {
		t.Fatalf("after ignoring: %+v, %v", st, paths(t, ix))
	}
	if _, err := ix.IndexFile(ctx, "journal/day.md"); !errors.Is(err, policy.ErrIgnored) {
		t.Fatalf("IndexFile(ignored) = %v", err)
	}

	write(t, ix.Root(), ".screeignore", "# nothing ignored any more\n")
	if st, err = ix.Sync(ctx); err != nil || st.Indexed != 1 || len(paths(t, ix)) != 3 {
		t.Fatalf("after un-ignoring: %+v, %v, %v", st, err, paths(t, ix))
	}
}

func TestSyncFailsOnUnreadableScreeignore(t *testing.T) {
	ix, fe := setup(t)
	outside := filepath.Join(filepath.Dir(ix.Root()), "outside")
	write(t, outside, "ign", "")
	write(t, ix.Root(), "a.md", "# A\napple\n")
	symlink(t, filepath.Join(outside, "ign"), filepath.Join(ix.Root(), ".screeignore"))
	if _, err := ix.Sync(context.Background()); err == nil || fe.calls != 0 {
		t.Fatalf("Sync err = %v, calls %d", err, fe.calls)
	}
}

func TestFrontMatterOptOutRemoves(t *testing.T) {
	ctx := context.Background()
	ix, _ := setup(t)
	write(t, ix.Root(), "a.md", "# A\napple\n")
	write(t, ix.Root(), "b.md", "# B\nbanana\n")
	ix.Sync(ctx)
	write(t, ix.Root(), "a.md", "---\nscree: ignore\n---\n# A\napple\n")
	changed, err := ix.IndexFile(ctx, "a.md")
	if !changed || !errors.Is(err, policy.ErrIgnored) {
		t.Fatalf("IndexFile = %v, %v", changed, err)
	}
	if got := paths(t, ix); !slices.Equal(got, []string{"b.md"}) {
		t.Fatalf("paths = %v", got)
	}
	write(t, ix.Root(), "b.md", "---\nscree: 'ignore'\n---\nbanana\n")
	if st, _ := ix.Sync(ctx); st.Removed != 1 || len(paths(t, ix)) != 0 {
		t.Fatalf("sync = %+v, paths %v", st, paths(t, ix))
	}
}

// recordingEmbedder remembers every input it was asked to embed.
type recordingEmbedder struct {
	fakeEmbedder
	inputs []string
}

func (r *recordingEmbedder) Embed(ctx context.Context, in []string) ([][]float32, error) {
	r.inputs = append(r.inputs, in...)
	return r.fakeEmbedder.Embed(ctx, in)
}

func TestSyncTooLargeSkipped(t *testing.T) {
	ix, _ := setup(t)
	p, err := policy.Open(ix.Root(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	re := &recordingEmbedder{}
	ix.Policy, ix.Embedder = p, re
	write(t, ix.Root(), "big.md", "# Big\n"+strings.Repeat("zzz ", 500))
	write(t, ix.Root(), "small.md", "# Small\napple\n")
	st, err := ix.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Indexed != 1 || !slices.Equal(paths(t, ix), []string{"small.md"}) {
		t.Fatalf("sync = %+v, paths %v", st, paths(t, ix))
	}
	for _, in := range re.inputs {
		if strings.Contains(in, "zzz") {
			t.Fatal("embedder saw the oversized note")
		}
	}
}

func TestIndexFileRejectsBadPath(t *testing.T) {
	ix, fe := setup(t)
	for _, bad := range []string{"../x.md", "/etc/passwd", ""} {
		_, err := ix.IndexFile(context.Background(), bad)
		var ie *policy.IgnoredError
		if !errors.As(err, &ie) || ie.Reason != policy.Invalid {
			t.Errorf("IndexFile(%q) = %v", bad, err)
		}
	}
	if fe.calls != 0 {
		t.Fatal("embedder called")
	}
}

// downEmbedder fails with ErrUnavailable while down is set.
type downEmbedder struct {
	fakeEmbedder
	down bool
}

func (d *downEmbedder) Embed(ctx context.Context, in []string) ([][]float32, error) {
	if d.down {
		d.calls++
		return nil, fmt.Errorf("embed: %w: 503", embed.ErrUnavailable)
	}
	return d.fakeEmbedder.Embed(ctx, in)
}

func TestSyncPausesAfterUnavailable(t *testing.T) {
	ix, _ := setup(t)
	de := &downEmbedder{down: true}
	ix.Embedder = de
	for i := range 5 {
		write(t, ix.Root(), fmt.Sprintf("n%d.md", i), "# N\napple\n")
	}
	st, err := ix.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if de.calls != 1 || st.Deferred != 5 || st.Failed != 0 {
		t.Fatalf("calls %d, stats %+v", de.calls, st)
	}
	if until := ix.PausedUntil(); time.Until(until) < 25*time.Second {
		t.Fatalf("PausedUntil = %v, want about 30 s ahead", until)
	}
	if _, err := ix.IndexFile(context.Background(), "n0.md"); !errors.Is(err, ErrPaused) || de.calls != 1 {
		t.Fatalf("IndexFile while paused = %v, calls %d", err, de.calls)
	}
}

func TestPauseExpires(t *testing.T) {
	ix, _ := setup(t)
	ix.MinBackoff = 50 * time.Millisecond
	de := &downEmbedder{down: true}
	ix.Embedder = de
	write(t, ix.Root(), "a.md", "# A\napple\n")
	if st, _ := ix.Sync(context.Background()); st.Deferred != 1 {
		t.Fatalf("first sync = %+v", st)
	}
	// A second failure doubles the pause.
	time.Sleep(60 * time.Millisecond)
	ix.Sync(context.Background())
	if ix.backoff != 100*time.Millisecond || de.calls != 2 {
		t.Fatalf("second pause = %s after %d calls, want 100ms after 2", ix.backoff, de.calls)
	}
	de.down = false
	time.Sleep(110 * time.Millisecond)
	st, err := ix.Sync(context.Background())
	if err != nil || st.Indexed != 1 || st.Deferred != 0 {
		t.Fatalf("after pause = %+v, %v", st, err)
	}
	if !ix.PausedUntil().IsZero() {
		t.Fatal("pause not reset after success")
	}
}

func TestSearchEmptyIndexSkipsEmbed(t *testing.T) {
	ix, fe := setup(t)
	hits, err := ix.Search(context.Background(), "apple", 3, Hybrid)
	if err != nil || hits != nil || fe.calls != 0 {
		t.Fatalf("hits %v, err %v, calls %d", hits, err, fe.calls)
	}
}

func TestSearchUsesQueryEmbedder(t *testing.T) {
	ix, fe := setup(t)
	write(t, ix.Root(), "a.md", "# A\napple\n")
	ix.Sync(context.Background())
	q := &fakeEmbedder{}
	ix.QueryEmbedder = q
	calls := fe.calls
	if _, err := ix.Search(context.Background(), "apple", 1, Hybrid); err != nil || q.calls != 1 || fe.calls != calls {
		t.Fatalf("err %v, query calls %d, index calls %d -> %d", err, q.calls, calls, fe.calls)
	}
}

// caseInsensitive reports whether names under dir are compared without case.
func caseInsensitive(t *testing.T, dir string) bool {
	t.Helper()
	write(t, dir, "case-probe", "x")
	defer os.Remove(filepath.Join(dir, "case-probe"))
	a, _ := os.Lstat(filepath.Join(dir, "case-probe"))
	b, err := os.Lstat(filepath.Join(dir, "CASE-PROBE"))
	return err == nil && os.SameFile(a, b)
}

func TestIndexFileCanonicalCase(t *testing.T) {
	ctx := context.Background()
	ix, _ := setup(t)
	if !caseInsensitive(t, ix.Root()) {
		t.Skip("the temp dir is case-sensitive")
	}
	write(t, ix.Root(), ".screeignore", "private/\n")
	write(t, ix.Root(), "private/tax.md", "# Tax\napple\n")
	write(t, ix.Root(), "Notes/A.md", "# A\napple\n")
	for _, rel := range []string{"Private/tax.md", "PRIVATE/TAX.md"} {
		_, err := ix.IndexFile(ctx, rel)
		var ie *policy.IgnoredError
		if !errors.As(err, &ie) || ie.Reason != policy.IgnoreFile {
			t.Errorf("IndexFile(%q) = %v, want a .screeignore refusal", rel, err)
		}
	}
	if changed, err := ix.IndexFile(ctx, "notes/a.MD"); err != nil || !changed {
		t.Fatalf("IndexFile(notes/a.MD) = %v, %v", changed, err)
	}
	if got := paths(t, ix); !slices.Equal(got, []string{"Notes/A.md"}) {
		t.Errorf("paths = %v, want only the on-disk name", got)
	}
}

// TestSyncRaceKeepsOldRows: a file swapped for a symlink while Sync reads it
// counts as failed, and its rows stay.
func TestSyncRaceKeepsOldRows(t *testing.T) {
	ctx := context.Background()
	ix, _ := setup(t)
	write(t, ix.Root(), "a.md", "# A\napple\n")
	write(t, ix.Root(), ".env", "secret")
	if _, err := ix.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	write(t, ix.Root(), "a.md", "# A\nbanana\n") // changed, so Sync reads it
	before, _ := ix.Store.FileHash(ctx, "a.md")
	policy.TestHooks.BeforeOpen = func(rel string) {
		if rel == "a.md" {
			os.Remove(filepath.Join(ix.Root(), "a.md"))
			os.Symlink(".env", filepath.Join(ix.Root(), "a.md"))
		}
	}
	defer func() { policy.TestHooks.BeforeOpen = nil }()
	st, err := ix.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Failed != 1 || st.Removed != 0 || st.Indexed != 0 {
		t.Errorf("Sync = %+v", st)
	}
	if after, _ := ix.Store.FileHash(ctx, "a.md"); after != before || after == "" {
		t.Errorf("rows changed: hash %q -> %q", before, after)
	}
}

// searchPaths runs Search and returns the distinct hit paths.
func searchPaths(t *testing.T, ix *Indexer, q string, k int) []string {
	t.Helper()
	hits, err := ix.Search(context.Background(), q, k, Hybrid)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, h := range hits {
		if !slices.Contains(got, h.Path) {
			got = append(got, h.Path)
		}
	}
	slices.Sort(got)
	return got
}

func indexed(t *testing.T, files map[string]string) *Indexer {
	t.Helper()
	ix, _ := setup(t)
	for rel, src := range files {
		write(t, ix.Root(), rel, src)
	}
	if _, err := ix.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	return ix
}

func TestSearchDropsNewlyIgnored(t *testing.T) {
	ix := indexed(t, map[string]string{"a.md": "# A\napple\n", "private/b.md": "# B\napple apple\n"})
	if got := searchPaths(t, ix, "apple", 8); !slices.Equal(got, []string{"a.md", "private/b.md"}) {
		t.Fatalf("before: %v", got)
	}
	var stale []string
	ix.OnStale = func(p []string) { stale = append(stale, p...) }
	write(t, ix.Root(), ".screeignore", "private/\n") // no sync
	if got := searchPaths(t, ix, "apple", 8); !slices.Equal(got, []string{"a.md"}) {
		t.Errorf("after: %v", got)
	}
	if !slices.Equal(stale, []string{"private/b.md"}) {
		t.Errorf("OnStale got %v", stale)
	}
}

func TestSearchDropsFrontMatterOptOut(t *testing.T) {
	ix := indexed(t, map[string]string{"a.md": "# A\napple\n", "b.md": "# B\napple\n"})
	write(t, ix.Root(), "b.md", "---\nscree: ignore\n---\n# B\napple\n") // no sync
	if got := searchPaths(t, ix, "apple", 8); !slices.Equal(got, []string{"a.md"}) {
		t.Errorf("paths = %v", got)
	}
}

func TestSearchDropsDeleted(t *testing.T) {
	ix := indexed(t, map[string]string{"a.md": "# A\napple\n", "b.md": "# B\napple\n"})
	if err := os.Remove(filepath.Join(ix.Root(), "b.md")); err != nil {
		t.Fatal(err)
	}
	if got := searchPaths(t, ix, "apple", 8); !slices.Equal(got, []string{"a.md"}) {
		t.Errorf("paths = %v", got)
	}
}

func TestSearchKeepsK(t *testing.T) {
	files := map[string]string{}
	for i := range 8 {
		dir := "ok"
		if i < 3 {
			dir = "private"
		}
		files[fmt.Sprintf("%s/n%d.md", dir, i)] = "# N\n" + strings.Repeat("apple ", i+1) + "\n"
	}
	ix := indexed(t, files)
	write(t, ix.Root(), ".screeignore", "private/\n")
	got := searchPaths(t, ix, "apple", 5)
	if len(got) != 5 {
		t.Fatalf("got %d hits: %v", len(got), got)
	}
	for _, p := range got {
		if strings.HasPrefix(p, "private/") {
			t.Errorf("refused hit %q returned", p)
		}
	}
}

func TestSearchSkipsReadWhenUnchanged(t *testing.T) {
	ix := indexed(t, map[string]string{"a.md": "# A\napple\n"})
	opened := 0
	policy.TestHooks.BeforeOpen = func(string) { opened++ }
	defer func() { policy.TestHooks.BeforeOpen = nil }()
	if got := searchPaths(t, ix, "apple", 8); !slices.Equal(got, []string{"a.md"}) {
		t.Fatalf("paths = %v", got)
	}
	if opened != 0 {
		t.Errorf("search opened the file %d times; the stat matched", opened)
	}
}

// legacyLines makes path's rows look like a schema 1 index: no end_line, and
// a stale start line.
func legacyLines(t *testing.T, ix *Indexer, path string, n int) {
	t.Helper()
	ranges := make([]store.LineRange, n)
	for i := range ranges {
		ranges[i] = store.LineRange{Line: 1}
	}
	if err := ix.Store.SetLines(context.Background(), path, ranges); err != nil {
		t.Fatal(err)
	}
}

func TestBackfillLinesWithoutEmbedding(t *testing.T) {
	ix, fe := setup(t)
	ctx := context.Background()
	src := "# Apples\n\n\napple one\napple two\n\n# Bananas\n\nbanana\n"
	write(t, ix.Root(), "a.md", src)
	if _, err := ix.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	legacyLines(t, ix, "a.md", 2)
	if missing, _ := ix.Store.MissingLines(ctx, "a.md"); !missing {
		t.Fatal("setup: lines not cleared")
	}
	fe.calls = 0
	st, err := ix.Sync(ctx)
	if err != nil || st.Indexed != 0 {
		t.Fatalf("Sync = %+v, %v", st, err)
	}
	if fe.calls != 0 {
		t.Errorf("backfill called the embedder %d times", fe.calls)
	}
	if missing, _ := ix.Store.MissingLines(ctx, "a.md"); missing {
		t.Error("end_line not filled")
	}
	hits, err := ix.Search(ctx, "banana", 2, Hybrid)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][2]int{}
	for _, h := range hits {
		got[h.Heading] = [2]int{h.Line, h.EndLine}
	}
	if got["Apples"] != [2]int{1, 5} || got["Bananas"] != [2]int{7, 9} {
		t.Errorf("ranges = %v", got)
	}
}

func TestBackfillSkipsWhenChunkingDiffers(t *testing.T) {
	ix, fe := setup(t)
	ctx := context.Background()
	src := "# Apples\napple\n"
	write(t, ix.Root(), "a.md", src)
	if _, err := ix.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	// Rows from an older chunker: same file hash, other chunk text.
	hash, stat, err := ix.Store.FileInfo(ctx, "a.md")
	if err != nil {
		t.Fatal(err)
	}
	old := []store.Chunk{{Path: "a.md", Heading: "Apples", Line: 1, Text: "apple, chunked differently"}}
	if err := ix.Store.ReplaceFile(ctx, "a.md", hash, 1, stat, old, [][]float32{{1, 0, 0, 0}}); err != nil {
		t.Fatal(err)
	}
	fe.calls = 0
	if _, err := ix.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if fe.calls != 0 {
		t.Errorf("embedder called %d times", fe.calls)
	}
	if missing, _ := ix.Store.MissingLines(ctx, "a.md"); !missing {
		t.Error("lines were written for chunks that do not match")
	}
	if texts, _ := ix.Store.ChunkTexts(ctx, "a.md"); len(texts) != 1 || texts[0] != old[0].Text {
		t.Errorf("rows changed: %q", texts)
	}
}

// searchOne runs Search and returns the hit from path, failing if none.
func searchOne(t *testing.T, ix *Indexer, q, path string) store.Hit {
	t.Helper()
	hits, err := ix.Search(context.Background(), q, 8, Hybrid)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.Path == path {
			return h
		}
	}
	t.Fatalf("no hit from %s in %+v", path, hits)
	return store.Hit{}
}

func TestSearchCurrentLinesExact(t *testing.T) {
	ix := indexed(t, map[string]string{"a.md": "# A\napple pie\n"})
	if h := searchOne(t, ix, "apple", "a.md"); h.Line != 1 || h.EndLine != 2 {
		t.Fatalf("before: %d-%d", h.Line, h.EndLine)
	}
	write(t, ix.Root(), "a.md", strings.Repeat("filler\n", 10)+"# A\napple pie\n") // no sync
	h := searchOne(t, ix, "apple", "a.md")
	if h.Line != 11 || h.EndLine != 12 || h.Stale {
		t.Errorf("after inserting 10 lines: %d-%d stale=%v", h.Line, h.EndLine, h.Stale)
	}
}

func TestSearchCurrentLinesWhitespace(t *testing.T) {
	ix := indexed(t, map[string]string{"a.md": "# A\napple pie and more\nsecond line\n"})
	write(t, ix.Root(), "a.md", "# A\n\n  apple   pie and\nmore second\n\tline\n")
	h := searchOne(t, ix, "apple", "a.md")
	if h.Line != 1 || h.EndLine != 5 || h.Stale {
		t.Errorf("re-wrapped: %d-%d stale=%v", h.Line, h.EndLine, h.Stale)
	}
}

func TestSearchStaleWhenRewritten(t *testing.T) {
	ix := indexed(t, map[string]string{"a.md": "# A\napple pie\n"})
	write(t, ix.Root(), "a.md", "# A\nsomething else entirely, apple\n")
	h := searchOne(t, ix, "apple", "a.md")
	if !h.Stale || h.Line != 0 || h.EndLine != 0 || h.Text != "# A\napple pie" {
		t.Errorf("rewritten: %+v", h)
	}
}

func TestSearchReadsChangedFileOnce(t *testing.T) {
	ix := indexed(t, map[string]string{"a.md": "# A\napple one\n# B\napple two\n# C\napple three\n"})
	write(t, ix.Root(), "a.md", "intro\n\n# A\napple one\n# B\napple two\n# C\napple three\n")
	reads := 0
	TestHooks.SearchRead = func(string) { reads++ }
	defer func() { TestHooks.SearchRead = nil }()
	hits, err := ix.Search(context.Background(), "apple", 8, Hybrid)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, h := range hits {
		if h.Path == "a.md" {
			n++
			if h.Stale || h.Line < 3 {
				t.Errorf("hit %+v not relocated", h)
			}
		}
	}
	if n != 3 || reads != 1 {
		t.Errorf("%d hits from a.md, %d reads; want 3 and 1", n, reads)
	}
}

func TestSearchNearestOccurrence(t *testing.T) {
	filler := strings.Repeat("filler\n", 20)
	ix := indexed(t, map[string]string{"a.md": filler + "# A\napple x\n"})
	if h := searchOne(t, ix, "apple", "a.md"); h.Line != 21 {
		t.Fatalf("before: %d", h.Line)
	}
	write(t, ix.Root(), "a.md", "# A\napple x\n"+filler+"# A\napple x\n")
	if h := searchOne(t, ix, "apple", "a.md"); h.Line != 23 || h.EndLine != 24 {
		t.Errorf("nearest copy: %d-%d, want 23-24", h.Line, h.EndLine)
	}
}

func TestSearchQueuesChangedForWriter(t *testing.T) {
	ix := indexed(t, map[string]string{"a.md": "# A\napple\n", "b.md": "# B\napple\n"})
	var queued []string
	ix.OnStale = func(p []string) { queued = append(queued, p...) }
	searchPaths(t, ix, "apple", 8)
	if len(queued) != 0 {
		t.Fatalf("unchanged files queued: %v", queued)
	}
	write(t, ix.Root(), "a.md", "# A\napple\nmore\n")
	searchPaths(t, ix, "apple", 8)
	if !slices.Equal(queued, []string{"a.md"}) {
		t.Errorf("queued %v, want [a.md]", queued)
	}
}

func dupFiles(paths ...string) map[string]string {
	files := map[string]string{}
	for _, p := range paths {
		files[p] = "# Setup\napple setup steps\n"
	}
	return files
}

func TestCollapseDuplicates(t *testing.T) {
	ix := indexed(t, dupFiles("docs/print/x.md", "docs/wiki-build/x.md", "docs/x.md"))
	hits, err := ix.Search(context.Background(), "apple", 8, Hybrid)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Path != "docs/x.md" {
		t.Fatalf("hits = %+v", hits)
	}
	want := []store.Copy{{Path: "docs/print/x.md", Line: 1, EndLine: 2}, {Path: "docs/wiki-build/x.md", Line: 1, EndLine: 2}}
	if !slices.Equal(hits[0].Also, want) || hits[0].AlsoMore != 0 {
		t.Errorf("also = %+v (+%d)", hits[0].Also, hits[0].AlsoMore)
	}
}

func TestCollapseCapsAlso(t *testing.T) {
	ix := indexed(t, dupFiles("a.md", "b/a.md", "c/a.md", "d/a.md", "e/a.md", "f/a.md"))
	hits, err := ix.Search(context.Background(), "apple", 8, Hybrid)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Path != "a.md" || len(hits[0].Also) != 3 || hits[0].AlsoMore != 2 {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestCollapseSkipsRefusedCopy(t *testing.T) {
	ix := indexed(t, dupFiles("docs/print/x.md", "docs/wiki-build/x.md", "docs/x.md"))
	write(t, ix.Root(), ".screeignore", "docs/print/\n") // no sync
	hits, err := ix.Search(context.Background(), "apple", 8, Hybrid)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || len(hits[0].Also) != 1 || hits[0].Also[0].Path != "docs/wiki-build/x.md" {
		t.Errorf("hits = %+v", hits)
	}
}

func TestCollapseKeepsK(t *testing.T) {
	files := dupFiles("d1/x.md", "d2/x.md", "d3/x.md", "d4/x.md", "d5/x.md")
	for i := range 10 {
		files[fmt.Sprintf("n%d.md", i)] = fmt.Sprintf("# N%d\n%s\n", i, strings.Repeat("apple ", i+1))
	}
	ix := indexed(t, files)
	hits, err := ix.Search(context.Background(), "apple", 8, Hybrid)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 8 {
		t.Errorf("got %d hits, want 8", len(hits))
	}
	seen := map[string]bool{}
	for _, h := range hits {
		if seen[h.TextHash] {
			t.Errorf("duplicate text in two hits: %s", h.Path)
		}
		seen[h.TextHash] = true
	}
}
