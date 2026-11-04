//go:build unix

package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const testMCPSecret = "sk-or-v1-mcp-secret-9876543210"

// noMCPKey fails if SCREE_MCP_API_KEY or its value appears in any of strs.
func noMCPKey(t *testing.T, what string, strs []string) {
	t.Helper()
	for _, e := range strs {
		if strings.Contains(e, mcpKeyVar) || strings.Contains(e, testMCPSecret) {
			t.Errorf("%s holds the MCP key: %q", what, e)
		}
	}
}

// envValue returns NAME's value in env.
func envValue(env []string, name string) (string, bool) {
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, name+"="); ok {
			return v, true
		}
	}
	return "", false
}

// TestMCPUsesMCPKey: with a usable SCREE_MCP_API_KEY in the settings file,
// scree mcp gives serve that key as OPENROUTER_API_KEY, trimmed, and never
// passes SCREE_MCP_API_KEY on; without one it passes the file's
// OPENROUTER_API_KEY.
func TestMCPUsesMCPKey(t *testing.T) {
	for _, c := range []struct{ content, want string }{
		{testSettings + "SCREE_MCP_API_KEY='" + testMCPSecret + "'\n", testMCPSecret},
		{"SCREE_MCP_API_KEY=\" " + testMCPSecret + "\t\"\n", testMCPSecret},
		{testSettings, testSecret},
	} {
		l := launchSetup(t, c.content, 0o600)
		t.Setenv(mcpKeyVar, "env-mcp-key")
		var err error
		stderr := captureStderr(t, func() { err = runMCP(nil) })
		if err != nil || !l.called {
			t.Fatalf("%q: %v, exec %v", c.content, err, l.called)
		}
		if got, _ := envValue(l.env, "OPENROUTER_API_KEY"); got != c.want {
			t.Errorf("%q: child OPENROUTER_API_KEY %q, want %q", c.content, got, c.want)
		}
		if n := slices.IndexFunc(l.env, func(e string) bool { return strings.HasPrefix(e, "OPENROUTER_API_KEY=") }); n < 0 ||
			slices.ContainsFunc(l.env[n+1:], func(e string) bool { return strings.HasPrefix(e, "OPENROUTER_API_KEY=") }) {
			t.Errorf("%q: OPENROUTER_API_KEY not exactly once: %q", c.content, l.env)
		}
		noMCPKey(t, "child env", slices.DeleteFunc(slices.Clone(l.env), func(e string) bool { return e == "OPENROUTER_API_KEY="+testMCPSecret }))
		if stderr != "" {
			t.Errorf("%q: stderr %q", c.content, stderr)
		}
	}
}

// TestMCPUnusableMCPKey: an unusable SCREE_MCP_API_KEY gives one warning
// with the reason, never the value, and scree mcp falls back to the
// file's OPENROUTER_API_KEY.
func TestMCPUnusableMCPKey(t *testing.T) {
	empty := "SCREE_MCP_API_KEY is set but empty (the file's last SCREE_MCP_API_KEY line is the one used)"
	for value, reason := range map[string]string{
		"":      empty,
		"''":    empty,
		"'   '": empty,
		"'" + testMCPSecret + "'\nSCREE_MCP_API_KEY=": empty,
		"'${user_config.mcp_key}'":                    "SCREE_MCP_API_KEY is set but is an unfilled placeholder",
		"'bad secretvalue'":                           "SCREE_MCP_API_KEY is set but not a valid key: the key contains whitespace",
		"bad\x01secretvalue":                          "SCREE_MCP_API_KEY is set but not a valid key: the key contains whitespace, control",
		"bad\\secretvalue":                            "SCREE_MCP_API_KEY is set but not a valid key: the key contains a quote or backslash",
	} {
		l := launchSetup(t, testSettings+"SCREE_MCP_API_KEY="+value+"\n", 0o600)
		var err error
		stderr := captureStderr(t, func() { err = runMCP(nil) })
		if err != nil || !l.called {
			t.Fatalf("%q: %v, exec %v", value, err, l.called)
		}
		if got, _ := envValue(l.env, "OPENROUTER_API_KEY"); got != testSecret {
			t.Errorf("%q: child OPENROUTER_API_KEY %q", value, got)
		}
		noMCPKey(t, "child env", l.env)
		want := "scree: warning: " + reason
		if !strings.HasPrefix(stderr, want) || !strings.HasSuffix(stderr, " in "+l.file+"; using OPENROUTER_API_KEY\n") ||
			strings.Count(stderr, "\n") != 1 {
			t.Errorf("%q: stderr %q, want %q", value, stderr, want)
		}
		if strings.Contains(stderr, "secretvalue") || strings.Contains(stderr, "user_config") || strings.Contains(stderr, testSecret) {
			t.Errorf("%q: stderr shows a value: %q", value, stderr)
		}
	}
}

// TestRunNeverSeesMCPKey: scree run, and direct commands re-executed with
// the settings file's key, use OPENROUTER_API_KEY and never see the MCP
// key; a file with only the MCP key gives direct commands no key.
func TestRunNeverSeesMCPKey(t *testing.T) {
	l := launchSetup(t, testSettings+"SCREE_MCP_API_KEY='"+testMCPSecret+"'\n", 0o600)
	t.Setenv("OPENROUTER_API_KEY", "")
	if err := runRun([]string{"search", "q"}); err != nil || !l.called {
		t.Fatalf("%v, exec %v", err, l.called)
	}
	if got, _ := envValue(l.env, "OPENROUTER_API_KEY"); got != testSecret {
		t.Errorf("run: OPENROUTER_API_KEY %q", got)
	}
	noMCPKey(t, "run env", l.env)

	for _, cmd := range keyCommands {
		l.called = false
		if err := useSettingsKey(cmd, []string{"q"}); err != nil || !l.called {
			t.Fatalf("%s: %v, exec %v", cmd, err, l.called)
		}
		if got, _ := envValue(l.env, "OPENROUTER_API_KEY"); got != testSecret {
			t.Errorf("%s: OPENROUTER_API_KEY %q", cmd, got)
		}
		noMCPKey(t, cmd+" env", l.env)
	}

	l = launchSetup(t, "SCREE_MCP_API_KEY='"+testMCPSecret+"'\n", 0o600)
	t.Setenv("OPENROUTER_API_KEY", "")
	if err := useSettingsKey("search", []string{"q"}); err != nil || l.called {
		t.Errorf("only an MCP key: %v, exec %v", err, l.called)
	}
	if err := runRun([]string{"search", "q"}); err != nil || !l.called {
		t.Fatalf("run: %v, exec %v", err, l.called)
	}
	if _, ok := envValue(l.env, "OPENROUTER_API_KEY"); ok {
		t.Errorf("run with only an MCP key has a key: %q", l.env)
	}
	noMCPKey(t, "run env", l.env)
}

// TestDockerMCPKey: scree mcp -docker passes the key by name only, and the
// docker CLI's environment holds the MCP key as OPENROUTER_API_KEY, so
// that is what the container gets; SCREE_MCP_API_KEY never goes through.
func TestDockerMCPKey(t *testing.T) {
	for _, content := range []string{
		testSettings + "SCREE_MCP_API_KEY='" + testMCPSecret + "'\n",
		"SCREE_MCP_API_KEY='" + testMCPSecret + "'\n",
	} {
		l := launchSetup(t, content, 0o600)
		argv := dockerRun(t, l, l.proj, l.proj)
		if i := slices.Index(argv, "OPENROUTER_API_KEY"); i < 1 || argv[i-1] != "-e" {
			t.Errorf("%q: argv %q", content, argv)
		}
		for _, a := range argv {
			if strings.Contains(a, testSecret) || strings.Contains(a, testMCPSecret) || strings.Contains(a, mcpKeyVar) {
				t.Errorf("%q: argv holds %q", content, a)
			}
		}
		if got, _ := envValue(l.env, "OPENROUTER_API_KEY"); got != testMCPSecret {
			t.Errorf("%q: docker env OPENROUTER_API_KEY %q", content, got)
		}
		noMCPKey(t, "docker env", slices.DeleteFunc(slices.Clone(l.env), func(e string) bool { return e == "OPENROUTER_API_KEY="+testMCPSecret }))
	}
	// Unusable: a warning, and the file's OPENROUTER_API_KEY.
	l := launchSetup(t, testSettings+"SCREE_MCP_API_KEY='${x}'\n", 0o600)
	var argv []string
	stderr := captureStderr(t, func() { argv = dockerRun(t, l, l.proj, l.proj) })
	if got, _ := envValue(l.env, "OPENROUTER_API_KEY"); got != testSecret || !strings.Contains(stderr, "unfilled placeholder") {
		t.Errorf("unusable: env key %q, stderr %q", got, stderr)
	}
	noMCPKey(t, "docker argv", argv)
	noMCPKey(t, "docker env", l.env)
}

// TestKeyMCPWritesSettings: scree key -mcp writes the SCREE_MCP_API_KEY
// line and leaves OPENROUTER_API_KEY alone, and scree key leaves
// SCREE_MCP_API_KEY alone.
func TestKeyMCPWritesSettings(t *testing.T) {
	serveEnv(t)
	before := "# mine\nexport OPENROUTER_API_KEY='old-key'\nSCREE_ZDR=0\n"
	file := keyHome(t, before, 0o600)
	out, err := keyRun(t, true, "mcp-key", "-mcp", "-offline")
	if err != nil || !strings.Contains(out, "saved SCREE_MCP_API_KEY in "+file) {
		t.Fatalf("%q, %v", out, err)
	}
	if got := readFile(t, file); got != before+"SCREE_MCP_API_KEY='mcp-key'\n" {
		t.Errorf("file %q", got)
	}
	if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
	// From stdin, a saved MCP key is replaced only with -replace.
	if _, err := keyRun(t, false, "mcp-key-2\n", "-mcp", "-offline"); err == nil ||
		!strings.Contains(err.Error(), "already holds an API key (SCREE_MCP_API_KEY)") {
		t.Errorf("no -replace: %v", err)
	}
	if _, err := keyRun(t, false, "mcp-key-2\n", "-mcp", "-offline", "-replace"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, file); got != before+"SCREE_MCP_API_KEY='mcp-key-2'\n" {
		t.Errorf("-replace: file %q", got)
	}
	// scree key replaces OPENROUTER_API_KEY only.
	if _, err := keyRun(t, false, "new-key\n", "-offline", "-replace"); err != nil {
		t.Fatal(err)
	}
	want := "# mine\nexport OPENROUTER_API_KEY='new-key'\nSCREE_ZDR=0\nSCREE_MCP_API_KEY='mcp-key-2'\n"
	if got := readFile(t, file); got != want {
		t.Errorf("scree key: file %q, want %q", got, want)
	}
	// A new file with only the MCP key; a file with only OPENROUTER_API_KEY
	// takes the MCP key from stdin without -replace.
	file = keyHome(t, "", 0)
	if _, err := keyRun(t, false, "mcp-key\n", "-mcp", "-offline"); err != nil || readFile(t, file) != "SCREE_MCP_API_KEY='mcp-key'\n" {
		t.Errorf("new file: %v", err)
	}
	file = keyHome(t, "OPENROUTER_API_KEY='k'\n", 0o600)
	if _, err := keyRun(t, false, "mcp-key\n", "-mcp", "-offline"); err != nil ||
		readFile(t, file) != "OPENROUTER_API_KEY='k'\nSCREE_MCP_API_KEY='mcp-key'\n" {
		t.Errorf("beside OPENROUTER_API_KEY: %v", err)
	}
	// The same refusals as scree key.
	keyHome(t, "", 0)
	if _, err := keyRun(t, false, "bad key\n", "-mcp", "-offline"); err == nil || !strings.Contains(err.Error(), "whitespace") {
		t.Errorf("bad key: %v", err)
	}
	keyHome(t, testSettings, 0o640)
	if _, err := keyRun(t, true, "mcp-key", "-mcp", "-offline"); err == nil || !strings.Contains(err.Error(), "open to group or others") {
		t.Errorf("open file: %v", err)
	}
}

// TestKeyMCPCheck: scree key -mcp tests the MCP key, in a re-executed
// scree key -check -mcp that gets it as OPENROUTER_API_KEY and never under
// its own name.
func TestKeyMCPCheck(t *testing.T) {
	serveEnv(t)
	api := newKeyAPI(t, 200, embedOK)
	file := keyHome(t, "OPENROUTER_API_KEY='"+testSecret+"'\nSCREE_EMBED_URL="+api.URL+"\nSCREE_ZDR=0\n", 0o600)
	home := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("TERM", "")
	r := recordExec(t)
	if _, err := keyRun(t, false, testMCPSecret+"\n", "-mcp"); err != nil {
		t.Fatal(err)
	}
	wantEnv := []string{"HOME=" + home, "PATH=/usr/local/bin:/usr/bin:/bin", "SCREE_KEY_FROM=" + file,
		"OPENROUTER_API_KEY=" + testMCPSecret, "SCREE_EMBED_URL=" + api.URL, "SCREE_ZDR=0"}
	if !r.called || !slices.Equal(r.argv, []string{testExe, "key", "-child", "-check", "-mcp"}) || !slices.Equal(r.env, wantEnv) {
		t.Fatalf("exec %v %q\nenv %q\nwant %q", r.called, r.argv, r.env, wantEnv)
	}
	for _, kv := range r.env {
		k, v, _ := strings.Cut(kv, "=")
		if k != "HOME" && k != "PATH" {
			t.Setenv(k, v)
		}
	}
	out, err := keyRun(t, false, "", "-child", "-check", "-mcp")
	if err != nil || !strings.Contains(out, "ok   embeddings API returned 2 dimensions") {
		t.Fatalf("%q, %v", out, err)
	}
	if got := api.calls(); len(got) != 1 || got[0] != "Bearer "+testMCPSecret {
		t.Errorf("API calls %q", got)
	}

	// Plain -check still tests OPENROUTER_API_KEY, without the MCP key.
	t.Setenv(keyFromVar, "")
	t.Setenv("OPENROUTER_API_KEY", "")
	r = recordExec(t)
	if _, err := keyRun(t, false, "", "-check"); err != nil || !r.called || !slices.Contains(r.env, "OPENROUTER_API_KEY="+testSecret) {
		t.Errorf("-check: %v, exec %v %q", err, r.called, r.env)
	}
	noMCPKey(t, "-check env", r.env)

	// -check -mcp needs a usable MCP key in the file.
	for content, want := range map[string]string{
		"OPENROUTER_API_KEY='" + testSecret + "'\n":                     "holds no SCREE_MCP_API_KEY; run scree key -mcp",
		"OPENROUTER_API_KEY='" + testSecret + "'\nSCREE_MCP_API_KEY=\n": "SCREE_MCP_API_KEY is set but empty",
		"SCREE_MCP_API_KEY='${secretvalue}'\n":                          "SCREE_MCP_API_KEY is set but is an unfilled placeholder in ",
		"SCREE_MCP_API_KEY='bad secretvalue'\n":                         "SCREE_MCP_API_KEY is set but not a valid key",
	} {
		keyHome(t, content, 0o600)
		r := recordExec(t)
		_, err := keyRun(t, false, "", "-check", "-mcp")
		if err == nil || !strings.Contains(err.Error(), want) || r.called || strings.Contains(err.Error(), "secretvalue") {
			t.Errorf("%q: %v, exec %v", content, err, r.called)
		}
	}
}

// TestDoctorMCPKey: doctor says when scree mcp uses the separate MCP key,
// warns when it is unusable, and never prints either key.
func TestDoctorMCPKey(t *testing.T) {
	serveEnv(t)
	notes, db := searchFixture(t)
	t.Setenv("OPENROUTER_API_KEY", testSecret)
	doctor := func() string {
		var out string
		captureStderr(t, func() {
			out = captureStdout(t, func() {
				runDoctor(context.Background(), []string{"-offline", "-notes", notes, "-db", db})
			})
		})
		if strings.Contains(out, testSecret) || strings.Contains(out, testMCPSecret) || strings.Contains(out, "secretvalue") {
			t.Errorf("doctor shows a key:\n%s", out)
		}
		return out
	}
	file := keyHome(t, testSettings+"SCREE_MCP_API_KEY='"+testMCPSecret+"'\n", 0o600)
	out := doctor()
	if !strings.Contains(out, "ok   OPENROUTER_API_KEY set\n") ||
		!strings.Contains(out, "ok   scree mcp uses SCREE_MCP_API_KEY from "+file+"; direct commands and scree run use OPENROUTER_API_KEY\n") {
		t.Errorf("doctor:\n%s", out)
	}
	file = keyHome(t, testSettings+"SCREE_MCP_API_KEY='bad secretvalue'\n", 0o600)
	out = doctor()
	if !strings.Contains(out, "warn SCREE_MCP_API_KEY is set but not a valid key: the key contains whitespace, control or invisible characters in "+
		file+"; scree mcp uses OPENROUTER_API_KEY instead\n") {
		t.Errorf("doctor, unusable:\n%s", out)
	}
	for _, content := range []string{testSettings, ""} {
		keyHome(t, content, 0o600)
		if out := doctor(); strings.Contains(out, mcpKeyVar) {
			t.Errorf("%q: doctor:\n%s", content, out)
		}
	}
}

// TestKeyCheckIgnoresInheritedMarker: a SCREE_KEY_FROM and a key in the
// caller's environment do not make scree key -check (or -check -mcp) test
// that key; it re-executes with the file's key, and only the re-exec,
// marked by its first argument, makes the call.
func TestKeyCheckIgnoresInheritedMarker(t *testing.T) {
	serveEnv(t)
	api := newKeyAPI(t, 200, embedOK)
	keyHome(t, "OPENROUTER_API_KEY='"+testSecret+"'\nSCREE_MCP_API_KEY='"+testMCPSecret+"'\nSCREE_EMBED_URL="+api.URL+"\nSCREE_ZDR=0\n", 0o600)
	t.Setenv(keyFromVar, "none")
	t.Setenv("OPENROUTER_API_KEY", "sk-or-v1-caller-cli-key")
	t.Setenv("SCREE_EMBED_URL", api.URL)
	t.Setenv("SCREE_ZDR", "0")
	for _, c := range []struct {
		args []string
		key  string
	}{{[]string{"-check", "-mcp"}, testMCPSecret}, {[]string{"-check"}, testSecret}} {
		r := recordExec(t)
		if _, err := keyRun(t, false, "", c.args...); err != nil {
			t.Fatalf("%q: %v", c.args, err)
		}
		if got, _ := envValue(r.env, "OPENROUTER_API_KEY"); !r.called || got != c.key || r.argv[2] != "-child" {
			t.Errorf("%q: exec %v %q, key %q", c.args, r.called, r.argv, got)
		}
		if got := api.calls(); len(got) != 0 {
			t.Errorf("%q: tested the caller's key: %q", c.args, got)
		}
	}
	// -child is refused without -check or without the marker variable.
	if _, err := keyRun(t, false, "sk-or-v1-unused\n", "-child", "-offline"); err == nil || !strings.Contains(err.Error(), "-child is for scree's own re-exec") {
		t.Errorf("-child without -check: %v", err)
	}
	t.Setenv(keyFromVar, "")
	if _, err := keyRun(t, false, "", "-child", "-check"); err == nil || !strings.Contains(err.Error(), "-child is for scree's own re-exec") {
		t.Errorf("-child without the marker: %v", err)
	}
	if got := api.calls(); len(got) != 0 {
		t.Errorf("API called: %q", got)
	}
}

// TestKeyMCPChildNoKeyHint: the re-executed -check -mcp, given no key,
// names scree key -mcp.
func TestKeyMCPChildNoKeyHint(t *testing.T) {
	serveEnv(t)
	file := keyHome(t, "", 0)
	t.Setenv(keyFromVar, file)
	t.Setenv("OPENROUTER_API_KEY", "")
	_, err := keyRun(t, false, "", "-child", "-check", "-mcp")
	if err == nil || !strings.HasSuffix(err.Error(), "holds no SCREE_MCP_API_KEY; run scree key -mcp") {
		t.Errorf("-mcp: %v", err)
	}
	_, err = keyRun(t, false, "", "-child", "-check")
	if err == nil || !strings.HasSuffix(err.Error(), "holds no OPENROUTER_API_KEY; run scree key") {
		t.Errorf("plain: %v", err)
	}
}

// TestDoctorEmptyMCPKey: a SCREE_MCP_API_KEY line that is empty, alone or
// as the last of several, is a warning, not silence.
func TestDoctorEmptyMCPKey(t *testing.T) {
	serveEnv(t)
	notes, db := searchFixture(t)
	t.Setenv("OPENROUTER_API_KEY", testSecret)
	for _, line := range []string{"SCREE_MCP_API_KEY=\n", "SCREE_MCP_API_KEY='" + testMCPSecret + "'\nSCREE_MCP_API_KEY='  '\n"} {
		file := keyHome(t, testSettings+line, 0o600)
		var out string
		captureStderr(t, func() {
			out = captureStdout(t, func() {
				runDoctor(context.Background(), []string{"-offline", "-notes", notes, "-db", db})
			})
		})
		want := "warn SCREE_MCP_API_KEY is set but empty (the file's last SCREE_MCP_API_KEY line is the one used) in " + file +
			"; scree mcp uses OPENROUTER_API_KEY instead\n"
		if !strings.Contains(out, want) || strings.Contains(out, testMCPSecret) || strings.Contains(out, testSecret) {
			t.Errorf("%q: doctor:\n%s", line, out)
		}
	}
}
