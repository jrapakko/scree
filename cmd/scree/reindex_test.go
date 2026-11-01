package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jrapakko/scree/internal/lock"
	"github.com/jrapakko/scree/internal/policy"
	"github.com/jrapakko/scree/internal/writer"
)

func TestCLIReindexMarksSynced(t *testing.T) {
	serveEnv(t)
	notes := filepath.Join(tempDir(t), "notes")
	mkdir(t, notes)
	captureStdout(t, func() {
		if err := runReindex(context.Background(), []string{"-notes", notes}); err != nil {
			t.Fatalf("reindex: %v", err)
		}
	})
	var cfg config
	flags("search", &cfg).Parse([]string{"-notes", notes})
	ix, err := open(&cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer closeIndexer(ix)
	w := &writer.Writer{Indexer: ix}
	if note := w.Note(context.Background()); note != "" {
		t.Errorf("reader note after CLI reindex = %q", note)
	}
	out := captureStdout(t, func() {
		runDoctor(context.Background(), []string{"-offline", "-notes", notes})
	})
	if !strings.Contains(out, "ok   last full sync: 20") {
		t.Errorf("doctor does not show the sync:\n%s", out)
	}

	// A path-only reindex does not mark a full sync.
	other := filepath.Join(tempDir(t), "other")
	mkdir(t, other)
	if err := os.WriteFile(filepath.Join(other, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	captureStdout(t, func() { runReindex(context.Background(), []string{"-notes", other, "a.txt"}) })
	out = captureStdout(t, func() {
		runDoctor(context.Background(), []string{"-offline", "-notes", other})
	})
	if !strings.Contains(out, "ok   last full sync: never") {
		t.Errorf("path reindex marked a full sync:\n%s", out)
	}
}

func TestCLIReindexIgnoredPathWhileLocked(t *testing.T) {
	serveEnv(t)
	notes := filepath.Join(tempDir(t), "notes")
	mkdir(t, filepath.Join(notes, "private"))
	for name, src := range map[string]string{".screeignore": "private/\n", "private/x.md": "x", "ok.md": "ok"} {
		if err := os.WriteFile(filepath.Join(notes, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	db := filepath.Join(tempDir(t), "index.db")
	l, _, err := lock.TryLock(filepath.Join(filepath.Dir(db), "writer.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	err = runReindex(context.Background(), []string{"-notes", notes, "-db", db, "private/x.md"})
	var ie *policy.IgnoredError
	if !errors.As(err, &ie) || ie.Reason != policy.IgnoreFile {
		t.Fatalf("reindex private/x.md = %v, want the .screeignore refusal", err)
	}
	out := captureStdout(t, func() {
		if err := runReindex(context.Background(), []string{"-notes", notes, "-db", db, "ok.md"}); err != nil {
			t.Errorf("reindex ok.md = %v", err)
		}
		if err := runReindex(context.Background(), []string{"-notes", notes, "-db", db, "gone.md"}); err != nil {
			t.Errorf("reindex gone.md = %v", err)
		}
	})
	if strings.Count(out, "keeps this index current") != 2 {
		t.Errorf("output %q", out)
	}
}

// TestCLIReindexWithoutKey: with OPENROUTER_API_KEY empty and no settings
// file, reindex indexes for keyword search and says so, keyword and auto
// search find the text, and doctor counts the files. With a key, the next
// reindex embeds them (one API call) and semantic search finds them.
func TestCLIReindexWithoutKey(t *testing.T) {
	serveEnv(t)
	notes := filepath.Join(tempDir(t), "notes")
	mkdir(t, notes)
	if err := os.WriteFile(filepath.Join(notes, "ops.md"), []byte("# Ops\nrotate the backup keys weekly\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var err error
	out := captureStdout(t, func() { err = runReindex(ctx, []string{"-notes", notes}) })
	if err != nil || !strings.Contains(out, "1 files: 1 indexed (1 keyword only: no API key), 0 removed, 0 failed, 0 deferred") {
		t.Fatalf("reindex without a key = %v:\n%s", err, out)
	}
	search := func(mode string) string {
		t.Helper()
		var err error
		out := captureStdout(t, func() {
			err = runSearch(ctx, []string{"-notes", notes, "-mode", mode, "-format", "xml", "backup keys"})
		})
		if err != nil {
			t.Fatalf("search -mode %s: %v", mode, err)
		}
		return out
	}
	for _, mode := range []string{"keyword", "auto"} {
		if out := search(mode); !strings.Contains(out, `path="ops.md"`) || strings.Contains(out, "keyword search only") {
			t.Errorf("search -mode %s:\n%s", mode, out)
		}
	}
	out = captureStdout(t, func() { runDoctor(ctx, []string{"-offline", "-notes", notes}) })
	if !strings.Contains(out, "1 files, 1 chunks (1 files keyword-only: no API key; run scree key, or set OPENROUTER_API_KEY) (vec1") {
		t.Errorf("doctor without a key:\n%s", out)
	}

	var mu sync.Mutex
	apiCalls := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		apiCalls++
		mu.Unlock()
		io.WriteString(w, `{"data":[{"index":0,"embedding":[1,0]}]}`)
	}))
	defer api.Close()
	calls := func() int {
		mu.Lock()
		defer mu.Unlock()
		return apiCalls
	}
	t.Setenv("OPENROUTER_API_KEY", "dummy")
	t.Setenv("SCREE_EMBED_URL", api.URL)
	t.Setenv("SCREE_ZDR", "0")
	// A key, but the files not embedded yet: semantic search says why it
	// misses them, and doctor how to embed them.
	if out := search("semantic"); !strings.Contains(out, "[1 files indexed for keyword search only; scree reindex embeds them]") {
		t.Errorf("semantic before embedding:\n%s", out)
	}
	out = captureStdout(t, func() { runDoctor(ctx, []string{"-offline", "-notes", notes}) })
	if !strings.Contains(out, "(1 files keyword-only: indexed without an API key; run scree reindex)") {
		t.Errorf("doctor with a key:\n%s", out)
	}
	before := calls()
	out = captureStdout(t, func() { err = runReindex(ctx, []string{"-notes", notes}) })
	if err != nil || !strings.Contains(out, "1 files: 1 indexed, 0 removed") || calls() != before+1 {
		t.Fatalf("reindex with a key = %v, API calls %d -> %d:\n%s", err, before, calls(), out)
	}
	if out := search("semantic"); !strings.Contains(out, `path="ops.md"`) || !strings.Contains(out, `match="semantic"`) ||
		strings.Contains(out, "keyword search only") {
		t.Errorf("semantic after embedding:\n%s", out)
	}
	out = captureStdout(t, func() { err = runReindex(ctx, []string{"-notes", notes}) })
	if err != nil || !strings.Contains(out, "1 files: 0 indexed") {
		t.Errorf("reindex again = %v:\n%s", err, out)
	}
}
