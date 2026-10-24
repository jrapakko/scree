package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jrapakko/scree/internal/store"
)

// searchFixture builds an index of one note without the API and returns
// the folder and the index file.
func searchFixture(t *testing.T) (notes, db string) {
	t.Helper()
	notes = filepath.Join(tempDir(t), "notes")
	mkdir(t, notes)
	src := "# Ops\n\nrun PURGE_OLD_RECORDS nightly\n"
	if err := os.WriteFile(filepath.Join(notes, "ops.md"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	db = filepath.Join(tempDir(t), "index.db")
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	root, _ := filepath.EvalSymlinks(notes)
	if err := s.CheckRoot(root); err != nil {
		t.Fatal(err)
	}
	chunks := []store.Chunk{{Path: "ops.md", Heading: "Ops", Line: 1, EndLine: 3, Text: "# Ops\n\nrun PURGE_OLD_RECORDS nightly"}}
	if err := s.ReplaceFile(context.Background(), "ops.md", "h", 1, nil, chunks, [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateVocab(context.Background(), []store.VocabFile{{Path: "app.py", Hash: "h", Names: []string{"pagefetcher"}}}, nil); err != nil {
		t.Fatal(err)
	}
	return notes, db
}

// TestCLISearchKeywordJSON: scree search -mode keyword -format json needs
// no API key and prints one object per hit with what an eval scorer needs;
// doctor reports the keyword index.
func TestCLISearchKeywordJSON(t *testing.T) {
	serveEnv(t)
	notes, db := searchFixture(t)
	var runErr error
	out := captureStdout(t, func() {
		runErr = runSearch(context.Background(), []string{"-notes", notes, "-db", db, "-mode", "keyword", "-format", "json", "purge_old_records"})
	})
	if runErr != nil {
		t.Fatal(runErr)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 {
		t.Fatalf("output %q", out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{"n": 1.0, "mode": "keyword", "path": "ops.md", "start": 1.0, "end": 3.0, "heading": "Ops", "score": nil, "match": "keyword", "stale": false} {
		if v, ok := got[k]; !ok || v != want {
			t.Errorf("%s = %v (present %v), want %v", k, v, ok, want)
		}
	}

	if err := runSearch(context.Background(), []string{"-notes", notes, "-db", db, "-mode", "nope", "x"}); err == nil || !strings.Contains(err.Error(), "mode must be") {
		t.Errorf("bad mode: %v", err)
	}
	if err := runSearch(context.Background(), []string{"-notes", notes, "-db", db, "-format", "yaml", "x"}); err == nil {
		t.Error("bad format accepted")
	}

	out = captureStdout(t, func() {
		runDoctor(context.Background(), []string{"-v", "-offline", "-notes", notes, "-db", db})
	})
	for _, want := range []string{"ok   keyword index: 1 chunks", "ok   keyword index integrity check",
		"ok   code vocabulary: 1 names from 1 manifests and source files", `  vocabulary sample: "pagefetcher"`} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor lacks %q:\n%s", want, out)
		}
	}
}

// TestCLISearchDefaultsToAuto: with no -mode, a plain query runs semantic
// and an identifier query hybrid, as the JSON mode field and the XML
// note say. A name from the code vocabulary also gives hybrid.
func TestCLISearchDefaultsToAuto(t *testing.T) {
	serveEnv(t)
	notes, db := searchFixture(t)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"index":0,"embedding":[1,0]}]}`)
	}))
	defer api.Close()
	t.Setenv("SCREE_EMBED_URL", api.URL)
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	t.Setenv("SCREE_ZDR", "0")
	for q, want := range map[string]string{"nightly run": "semantic", "purge_old_records": "hybrid", "does the PageFetcher purge?": "hybrid"} {
		var runErr error
		out := captureStdout(t, func() {
			runErr = runSearch(context.Background(), []string{"-notes", notes, "-db", db, "-format", "json", q})
		})
		var got struct{ Mode, Match string }
		if runErr != nil || json.Unmarshal([]byte(out), &got) != nil || got.Mode != want {
			t.Errorf("%q: mode %q (%v, %q), want %s", q, got.Mode, runErr, out, want)
		}
		out = captureStdout(t, func() {
			runErr = runSearch(context.Background(), []string{"-notes", notes, "-db", db, q})
		})
		if runErr != nil || !strings.HasSuffix(strings.TrimSpace(out), "[mode: auto → "+want+"]") {
			t.Errorf("%q xml: %v, %q", q, runErr, out)
		}
	}
}
