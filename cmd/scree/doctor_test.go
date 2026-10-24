package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStdout runs fn and returns what it printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	defer func() { os.Stdout = old }()
	fn()
	w.Close()
	os.Stdout = old
	return <-done
}

func TestDoctorListsReincluded(t *testing.T) {
	home := tempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("SCREE_DB", "")
	t.Setenv("SCREE_NOTES", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	notes := filepath.Join(tempDir(t), "notes")
	files := map[string]string{
		".screeignore":      "# comment\njournal/\n!journal/public.md\n*.private.md\narchive/\n!archive/2024/\n",
		"journal/public.md": "x", "journal/day.md": "x",
		"archive/2024/a.md": "x", "archive/2024/s.private.md": "x", "archive/2023/b.md": "x",
		"plain.md": "x",
	}
	for rel, src := range files {
		p := filepath.Join(notes, filepath.FromSlash(rel))
		mkdir(t, filepath.Dir(p))
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := captureStdout(t, func() {
		runDoctor(context.Background(), []string{"-v", "-offline", "-notes", notes})
	})
	for _, want := range []string{
		`ok   .screeignore: 3 ignored, 2 re-included by "!"`,
		`  re-included: "archive/2024/a.md" (.screeignore line 6)`,
		`  re-included: "journal/public.md" (.screeignore line 3)`,
		`  ignored (.screeignore): "archive/2024/s.private.md"`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("doctor output lacks %q:\n%s", want, out)
		}
	}
}

// Doctor names the git-side files it refuses or cannot parse, never their
// content, even when a repo links them to a file holding a key.
func TestDoctorNeverPrintsGitignoreContent(t *testing.T) {
	const secretText = "OPENROUTER_API_KEY=sk-or-v1-FAKE0123456789 [bad\n"
	write := func(p, s string) {
		t.Helper()
		mkdir(t, filepath.Dir(p))
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	symlink := func(target, name string) {
		t.Helper()
		mkdir(t, filepath.Dir(name))
		if err := os.Symlink(target, name); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	cases := []struct {
		name  string
		setup func(repo, secret, home string) (root string)
		want  string // a line fragment naming the file
	}{
		{"nested .gitignore", func(repo, secret, _ string) string {
			symlink(secret, filepath.Join(repo, "sub", ".gitignore"))
			return repo
		}, `unreadable: read "sub/.gitignore": not a regular file (symlinks are not followed); folder skipped`},
		{"ancestor .gitignore", func(repo, secret, _ string) string {
			symlink(secret, filepath.Join(repo, "sub", ".gitignore"))
			return filepath.Join(repo, "sub", "inner")
		}, "sub/.gitignore: not a regular file (symlinks are not followed); folder skipped"},
		{"info/exclude", func(repo, secret, _ string) string {
			symlink(secret, filepath.Join(repo, ".git", "info", "exclude"))
			return repo
		}, "info/exclude: not a regular file (symlinks are not followed)"},
		{".screeignore", func(repo, secret, _ string) string {
			write(filepath.Join(repo, ".env"), secretText)
			symlink(".env", filepath.Join(repo, ".screeignore"))
			return repo
		}, "read .screeignore: not a regular file (symlinks are not followed)"},
		{"user excludesFile", func(repo, secret, home string) string {
			write(filepath.Join(home, ".gitconfig"), "[core]\n\texcludesFile = "+secret+"\n")
			return repo
		}, "secret.txt line 1: bad pattern"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := tempDir(t)
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
			t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
			for _, v := range []string{"SCREE_DB", "SCREE_NOTES", "OPENROUTER_API_KEY", "CLAUDE_PROJECT_DIR"} {
				t.Setenv(v, "")
			}
			tmp := tempDir(t)
			secret := filepath.Join(tmp, "secret.txt")
			write(secret, secretText)
			repo := filepath.Join(tmp, "repo")
			mkdir(t, filepath.Join(repo, ".git", "info"))
			write(filepath.Join(repo, "sub", "inner", "a.md"), "a")
			write(filepath.Join(repo, "b.md"), "b")
			root := c.setup(repo, secret, home)
			out := captureStdout(t, func() {
				runDoctor(context.Background(), []string{"-v", "-offline", "-notes", root})
			})
			if strings.Contains(out, "sk-or-v1-FAKE") || strings.Contains(out, "[bad") {
				t.Errorf("doctor printed file content:\n%s", out)
			}
			if !strings.Contains(out, c.want) {
				t.Errorf("doctor output lacks %q:\n%s", c.want, out)
			}
		})
	}
}

func TestDoctorSizeHiddenAndCap(t *testing.T) {
	serveEnv(t)
	t.Setenv("SCREE_FIRST_SYNC_MB", "1")
	notes := filepath.Join(tempDir(t), "notes")
	for rel, src := range map[string]string{
		".screeignore":                     "include-hidden .foo/\n",
		".github/PULL_REQUEST_TEMPLATE.md": "x",
		".foo/a.md":                        "x",
		"big1.md":                          strings.Repeat("word ", 120<<10),
		"big2.md":                          strings.Repeat("word ", 120<<10),
	} {
		p := filepath.Join(notes, filepath.FromSlash(rel))
		mkdir(t, filepath.Dir(p))
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := captureStdout(t, func() {
		runDoctor(context.Background(), []string{"-offline", "-notes", notes})
	})
	for _, want := range []string{
		"ok   files: 4 indexable",
		"ok   size: 1.2 MB of markdown, about 307k tokens to embed in full (under $0.01 with openai/text-embedding-3-small)",
		"warn over SCREE_FIRST_SYNC_MB: Claude Code sessions will not index this folder until you run scree reindex",
		"ok   hidden folders indexed: .github/ (+ .foo/ from .screeignore)",
		"outside the folder",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output lacks %q:\n%s", want, out)
		}
	}
	t.Setenv("SCREE_FIRST_SYNC_MB", "0.5")
	out = captureStdout(t, func() {
		runDoctor(context.Background(), []string{"-offline", "-notes", notes})
	})
	if !strings.Contains(out, `SCREE_FIRST_SYNC_MB="0.5": want a whole number of megabytes`) {
		t.Errorf("0.5 accepted:\n%s", out)
	}
}

func TestSizeLine(t *testing.T) {
	if got := sizeLine(10<<20, costModel); got != "size: 10.0 MB of markdown, about 2.6M tokens to embed in full (about $0.05 with openai/text-embedding-3-small)" {
		t.Errorf("sizeLine = %q", got)
	}
	if got := sizeLine(400, "other/model"); got != "size: 0.0 MB of markdown, about 100 tokens to embed in full" {
		t.Errorf("sizeLine = %q", got)
	}
}
