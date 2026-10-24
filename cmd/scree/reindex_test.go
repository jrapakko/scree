package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
