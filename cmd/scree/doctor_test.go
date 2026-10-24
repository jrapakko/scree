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
		`  re-included: "archive/2024/a.md" (.screeignore line 6: !archive/2024/)`,
		`  re-included: "journal/public.md" (.screeignore line 3: !journal/public.md)`,
		`  ignored (.screeignore): "archive/2024/s.private.md"`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("doctor output lacks %q:\n%s", want, out)
		}
	}
}
