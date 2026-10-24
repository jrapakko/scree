package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// serveEnv isolates HOME, XDG_* and the scree variables, and returns the data
// home, under which no index may appear when serve refuses.
func serveEnv(t *testing.T) string {
	t.Helper()
	home := tempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	data := filepath.Join(home, "data")
	t.Setenv("XDG_DATA_HOME", data)
	for _, v := range []string{"SCREE_DB", "SCREE_NOTES", "SCREE_EMBED_URL", "OPENROUTER_API_KEY", "SCREE_HTTP_TOKEN", "CLAUDE_PROJECT_DIR"} {
		t.Setenv(v, "")
	}
	return data
}

func wantServeRefused(t *testing.T, data string, args ...string) {
	t.Helper()
	err := runServe(context.Background(), args)
	if !errors.Is(err, errServeFolder) || !strings.Contains(err.Error(), "-notes, -db and SCREE_DB are for the command line") {
		t.Fatalf("runServe(%q) = %v", args, err)
	}
	if _, err := os.Stat(data); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("serve created %s before refusing: %v", data, err)
	}
}

func TestServeRefusesNotesUnderClaude(t *testing.T) {
	data := serveEnv(t)
	repo := repoTree(t)
	t.Setenv("CLAUDE_PROJECT_DIR", repo)
	wantServeRefused(t, data, "-notes", filepath.Join(repo, "sub"))
	wantServeRefused(t, data, "-notes", repo)
	wantServeRefused(t, data, "-gitignore=false", "-notes", "")
}

func TestServeRefusesDBUnderClaude(t *testing.T) {
	data := serveEnv(t)
	repo := repoTree(t)
	t.Setenv("CLAUDE_PROJECT_DIR", repo)
	wantServeRefused(t, data, "-db", filepath.Join(repo, "index.db"))
}

func TestServeRefusesSCREE_DBUnderClaude(t *testing.T) {
	data := serveEnv(t)
	repo := repoTree(t)
	t.Setenv("CLAUDE_PROJECT_DIR", repo)
	t.Setenv("SCREE_DB", filepath.Join(repo, "index.db"))
	wantServeRefused(t, data)
	if _, err := os.Stat(filepath.Join(repo, "index.db")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("index created: %v", err)
	}
}

func TestServeNotesWithoutClaude(t *testing.T) {
	data := serveEnv(t)
	notes := filepath.Join(tempDir(t), "notes")
	mkdir(t, notes)
	// A cancelled context makes serve return as soon as it is set up.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runServe(ctx, []string{"-notes", notes}); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("runServe = %v", err)
	}
	if _, err := os.Stat(filepath.Join(indexDir(notes), "index.db")); err != nil {
		t.Errorf("no index for -notes under %s: %v", data, err)
	}
}

func TestCLIUnaffected(t *testing.T) {
	serveEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vec := make([]float32, 512)
		vec[0] = 1
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"index": 0, "embedding": vec}}})
	}))
	defer srv.Close()
	t.Setenv("SCREE_EMBED_URL", srv.URL)
	t.Setenv("OPENROUTER_API_KEY", "test")
	t.Setenv("CLAUDE_PROJECT_DIR", repoTree(t))
	notes := filepath.Join(tempDir(t), "notes")
	mkdir(t, notes)
	db := filepath.Join(tempDir(t), "x.db")
	out := captureStdout(t, func() {
		if err := runSearch(context.Background(), []string{"-notes", notes, "-db", db, "query"}); err != nil {
			t.Errorf("search -notes -db: %v", err)
		}
	})
	if !strings.Contains(out, "No matching") {
		t.Errorf("search output %q", out)
	}
	if _, err := os.Stat(db); err != nil {
		t.Errorf("search did not use -db: %v", err)
	}
}
