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
	"time"

	"github.com/jrapakko/scree/internal/writer"
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
	for _, v := range []string{"SCREE_DB", "SCREE_NOTES", "SCREE_EMBED_URL", "OPENROUTER_API_KEY", "SCREE_HTTP_TOKEN", "CLAUDE_PROJECT_DIR", "SCREE_COLORS"} {
		t.Setenv(v, "")
	}
	// Set and empty: no pager, so tests on a fake terminal read stdout.
	t.Setenv("SCREE_PAGER", "")
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
	if _, err := os.Stat(filepath.Join(testIndexDir(t, notes), "index.db")); err != nil {
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

func TestShutdownTimeout(t *testing.T) {
	serveEnv(t)
	notes := filepath.Join(tempDir(t), "notes")
	mkdir(t, notes)
	release := make(chan struct{})
	oldRun, oldTimeout := runWriter, ShutdownTimeout
	runWriter = func(context.Context, *writer.Writer) error { <-release; return nil } // ignores ctx
	ShutdownTimeout = 200 * time.Millisecond
	t.Cleanup(func() { close(release); runWriter, ShutdownTimeout = oldRun, oldTimeout })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	runServe(ctx, []string{"-notes", notes})
	if d := time.Since(start); d < ShutdownTimeout || d > ShutdownTimeout+2*time.Second {
		t.Errorf("serve returned after %s, want about %s", d, ShutdownTimeout)
	}
	start = time.Now()
	runWatch(ctx, []string{"-notes", notes})
	if d := time.Since(start); d < ShutdownTimeout || d > ShutdownTimeout+2*time.Second {
		t.Errorf("watch returned after %s, want about %s", d, ShutdownTimeout)
	}
}

// TestServeRefusesHomeNotes: an explicit -notes that is the home folder, a
// folder above it, or a filesystem root is refused before anything is
// written, unless -allow-home; a folder inside home is served.
func TestServeRefusesHomeNotes(t *testing.T) {
	data := serveEnv(t)
	home := filepath.Join(tempDir(t), "home", "u")
	notes := filepath.Join(home, "notes")
	mkdir(t, notes)
	fakeHome(t, home)
	t.Setenv("XDG_DATA_HOME", data)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	root := filepath.VolumeName(home) + string(filepath.Separator)
	dirs := []string{home, filepath.Dir(home), root, home + string(filepath.Separator) + "."}
	// A symlink to home is refused too (where symlinks can be made).
	if link := filepath.Join(tempDir(t), "link"); os.Symlink(home, link) == nil {
		dirs = append(dirs, link)
	}
	for _, dir := range dirs {
		err := runServe(ctx, []string{"-notes", dir})
		if err == nil || !strings.Contains(err.Error(), "pick a notes folder or a repo instead, not your home folder") {
			t.Errorf("serve -notes %s = %v", dir, err)
		}
		if _, err := os.Stat(data); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("serve -notes %s created %s before refusing", dir, data)
		}
	}
	for _, args := range [][]string{{"-notes", notes}, {"-allow-home", "-notes", home}} {
		if err := runServe(ctx, args); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("serve %q = %v", args, err)
		}
	}
	if _, err := os.Stat(filepath.Join(testIndexDir(t, home), "index.db")); err != nil {
		t.Errorf("serve -allow-home made no index: %v", err)
	}
}
