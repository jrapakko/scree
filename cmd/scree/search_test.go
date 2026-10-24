package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jrapakko/scree/internal/store"
)

// TestCLISearchKeywordJSON: scree search -mode keyword -format json needs
// no API key and prints one object per hit with what an eval scorer needs;
// doctor reports the keyword index.
func TestCLISearchKeywordJSON(t *testing.T) {
	serveEnv(t)
	notes := filepath.Join(tempDir(t), "notes")
	mkdir(t, notes)
	src := "# Ops\n\nrun DELETE_STALE_INTAKE nightly\n"
	if err := os.WriteFile(filepath.Join(notes, "ops.md"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(tempDir(t), "index.db")
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := filepath.EvalSymlinks(notes)
	if err := s.CheckRoot(root); err != nil {
		t.Fatal(err)
	}
	chunks := []store.Chunk{{Path: "ops.md", Heading: "Ops", Line: 1, EndLine: 3, Text: "# Ops\n\nrun DELETE_STALE_INTAKE nightly"}}
	if err := s.ReplaceFile(context.Background(), "ops.md", "h", 1, nil, chunks, [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	var runErr error
	out := captureStdout(t, func() {
		runErr = runSearch(context.Background(), []string{"-notes", notes, "-db", db, "-mode", "keyword", "-format", "json", "delete_stale_intake"})
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
	for k, want := range map[string]any{"n": 1.0, "path": "ops.md", "start": 1.0, "end": 3.0, "heading": "Ops", "score": nil, "match": "keyword", "stale": false} {
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
	for _, want := range []string{"ok   keyword index: 1 chunks", "ok   keyword index integrity check"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor lacks %q:\n%s", want, out)
		}
	}
}
