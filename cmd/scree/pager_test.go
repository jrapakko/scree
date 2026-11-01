//go:build unix

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeExec writes an executable shell script named name in dir.
func fakeExec(t *testing.T, dir, name, body string) string {
	t.Helper()
	mkdir(t, dir)
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestPagerArgv: SCREE_PAGER names the pager by absolute path or by a
// name in pagerDirs, never through PATH, with its arguments split on
// whitespace; set but empty, there is none; unset, less from pagerDirs
// with -FRX, but only with TERM set.
func TestPagerArgv(t *testing.T) {
	dir := tempDir(t)
	bin := filepath.Join(dir, "bin")
	fakeExec(t, bin, "less", "exit 0")
	fakeExec(t, bin, "mypager", "exit 0")
	if err := os.WriteFile(filepath.Join(bin, "notexec"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(dir, "planted")
	fakeExec(t, planted, "onpath", "exit 0")
	fakeExec(t, planted, "less", "exit 0")
	t.Setenv("PATH", planted)
	t.Setenv("TERM", "xterm")
	replace(t, &pagerDirs, []string{filepath.Join(dir, "nonexistent"), bin})

	unset := func() {
		t.Setenv("SCREE_PAGER", "")
		os.Unsetenv("SCREE_PAGER")
	}
	unset()
	if got := pagerArgv(); !slices.Equal(got, []string{filepath.Join(bin, "less"), "-FRX"}) {
		t.Errorf("default: %q", got)
	}
	t.Setenv("TERM", "")
	if got := pagerArgv(); got != nil {
		t.Errorf("default without TERM: %q", got)
	}
	t.Setenv("TERM", "xterm")
	replace(t, &pagerDirs, []string{planted + "-none"})
	unset()
	if got := pagerArgv(); got != nil {
		t.Errorf("no less in the fixed folders, one on PATH: %q", got)
	}
	pagerDirs = []string{filepath.Join(dir, "nonexistent"), bin}

	for v, want := range map[string][]string{
		"":                        nil,
		"  \t ":                   nil,
		"mypager -x  --y":         {filepath.Join(bin, "mypager"), "-x", "--y"},
		"less":                    {filepath.Join(bin, "less")},
		"/opt/x/pg -R 'a b'":      {"/opt/x/pg", "-R", "'a", "b'"},
		"onpath":                  nil,
		"notexec":                 nil,
		"bin/mypager":             nil,
		"../bin/mypager":          nil,
		"$(touch pwned); mypager": nil,
	} {
		t.Setenv("SCREE_PAGER", v)
		var got []string
		stderr := captureStderr(t, func() { got = pagerArgv() })
		if !slices.Equal(got, want) {
			t.Errorf("SCREE_PAGER=%q: %q, want %q", v, got, want)
		}
		if warned := strings.Contains(stderr, "scree: warning: SCREE_PAGER: "); warned != (want == nil && strings.TrimSpace(v) != "") {
			t.Errorf("SCREE_PAGER=%q: stderr %q", v, stderr)
		}
	}
}

// TestSearchPager: the text view on a terminal goes through the pager,
// which writes to the real stdout; not with -no-pager, into a pipe, or
// for other formats. A pager that cannot start leaves the view printed
// directly, and one that quits at once is not an error.
func TestSearchPager(t *testing.T) {
	serveEnv(t)
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm")
	notes, db := searchFixture(t)
	dir := tempDir(t)
	got := filepath.Join(dir, "paged")
	pager := fakeExec(t, filepath.Join(dir, "bin"), "pager", `printf '[paged %s]' "$1"; cat > "$2"`)
	t.Setenv("SCREE_PAGER", pager+" -FRX "+got)
	args := []string{"-notes", notes, "-db", db, "-mode", "keyword", "ops"}
	paged := func(args ...string) (stdout, pagerIn string) {
		t.Helper()
		os.Remove(got)
		stdout = searchOut(t, true, args...)
		b, _ := os.ReadFile(got)
		return stdout, string(b)
	}

	out, in := paged(args...)
	if out != "[paged -FRX]" || !strings.HasPrefix(in, "\x1b[1mops:\x1b[m\n  # \x1b[1;33mOps\x1b[m\n\t>\x1b]8;;file://") {
		t.Errorf("paged: stdout %q, pager got %q", out, in)
	}
	out, in = paged(append([]string{"-no-pager"}, args...)...)
	if in != "" || !strings.HasPrefix(out, "\x1b[1mops:") {
		t.Errorf("-no-pager: stdout %q, pager got %q", out, in)
	}
	for _, format := range []string{"xml", "json"} {
		if out, in = paged(append([]string{"-format", format}, args...)...); in != "" || out == "" || strings.HasPrefix(out, "[paged") {
			t.Errorf("-format %s: stdout %q, pager got %q", format, out, in)
		}
	}
	os.Remove(got)
	if out = searchOut(t, false, append([]string{"-format", "text"}, args...)...); !strings.HasPrefix(out, "ops:\n") {
		t.Errorf("pipe: %q", out)
	}
	if _, err := os.Stat(got); err == nil {
		t.Error("pager ran into a pipe")
	}

	t.Setenv("SCREE_PAGER", filepath.Join(dir, "missing")+" -R")
	var stderr string
	stderr = captureStderr(t, func() { out = searchOut(t, true, args...) })
	if !strings.HasPrefix(out, "\x1b[1mops:") || !strings.Contains(stderr, "scree: warning: SCREE_PAGER: ") {
		t.Errorf("missing pager: stdout %q, stderr %q", out, stderr)
	}

	for _, quit := range []string{fakeExec(t, filepath.Join(dir, "bin"), "quit", "exit 3"), "/usr/bin/true"} {
		if _, err := os.Stat(quit); err != nil {
			continue
		}
		t.Setenv("SCREE_PAGER", quit)
		if out = searchOut(t, true, args...); out != "" {
			t.Errorf("%s: %q", quit, out)
		}
	}
}

// TestPagerQuitEarly: writing more than a pipe holds to a pager that has
// quit fails, and ending the pager does not report it.
func TestPagerQuitEarly(t *testing.T) {
	t.Setenv("SCREE_PAGER", fakeExec(t, tempDir(t), "quit", "exit 0"))
	w, done := startPager()
	if w == os.Stdout {
		t.Fatal("no pager started")
	}
	var err error
	chunk := []byte(strings.Repeat("x", 64<<10))
	for range 64 {
		if _, err = w.Write(chunk); err != nil {
			break
		}
	}
	if err == nil {
		t.Fatal("4 MB written to a pager that quit")
	}
	if err := done(err); err != nil {
		t.Errorf("done: %v", err)
	}
}

// TestRunCarriesPagerAndColors: scree run and the settings-file key
// re-exec pass SCREE_PAGER (empty too) and SCREE_COLORS from the settings
// file, and not from the caller's environment.
func TestRunCarriesPagerAndColors(t *testing.T) {
	settings := testSettings + "SCREE_PAGER=\nSCREE_COLORS='match=7:lineno='\n"
	t.Setenv("SCREE_PAGER", "/tmp/evil-pager")
	t.Setenv("SCREE_COLORS", "match=31")
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm")
	want := []string{"SCREE_COLORS=match=7:lineno=", "SCREE_PAGER="}

	l := launchSetup(t, settings, 0o600)
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	if err := runRun([]string{"search", "q"}); err != nil {
		t.Fatal(err)
	}
	for _, v := range want {
		if !slices.Contains(l.env, v) {
			t.Errorf("scree run: child env lacks %q: %q", v, l.env)
		}
	}
	if slices.ContainsFunc(l.env, func(e string) bool { return strings.Contains(e, "evil") || e == "SCREE_COLORS=match=31" }) {
		t.Errorf("scree run: caller's value in child env: %q", l.env)
	}

	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv(keyFromVar, "")
	r := recordExec(t)
	if err := useSettingsKey("search", []string{"q"}); err != nil || !r.called {
		t.Fatalf("key re-exec: %v, exec %v", err, r.called)
	}
	for _, v := range want {
		if !slices.Contains(r.env, v) {
			t.Errorf("key re-exec: child env lacks %q: %q", v, r.env)
		}
	}
}

// TestPagerCharset: the pager runs with LESSCHARSET=utf-8 added to the
// environment, unless LESSCHARSET is already set (empty too).
func TestPagerCharset(t *testing.T) {
	dir := tempDir(t)
	got := filepath.Join(dir, "charset")
	pager := fakeExec(t, filepath.Join(dir, "bin"), "pager", `cat > /dev/null; printf '%s:%s' "${LESSCHARSET+set}" "$LESSCHARSET" > "$1"`)
	t.Setenv("SCREE_PAGER", pager+" "+got)
	for _, tc := range []struct {
		set       bool
		val, want string
	}{
		{false, "", "set:utf-8"},
		{true, "latin1", "set:latin1"},
		{true, "", "set:"},
	} {
		t.Setenv("LESSCHARSET", tc.val)
		if !tc.set {
			os.Unsetenv("LESSCHARSET")
		}
		w, done := startPager()
		if w == os.Stdout {
			t.Fatal("no pager started")
		}
		done(nil)
		if b, _ := os.ReadFile(got); string(b) != tc.want {
			t.Errorf("LESSCHARSET set %v %q: pager saw %q, want %q", tc.set, tc.val, b, tc.want)
		}
	}
}
