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

	hits, err := ix.Search(ctx, "cherry", 1)
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
	hits, _ = ix.Search(ctx, "date", 5)
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
	hits, err := ix.Search(context.Background(), "apple", 3)
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
	if _, err := ix.Search(context.Background(), "apple", 1); err != nil || q.calls != 1 || fe.calls != calls {
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
