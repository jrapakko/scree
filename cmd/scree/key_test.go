//go:build unix

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jrapakko/scree/internal/index"
)

// keyHome makes a fake passwd home, with the settings file holding content
// at mode unless content is "", and returns the settings file's path.
func keyHome(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	home := tempDir(t)
	file := settingsPath(home)
	if content != "" {
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(file, mode); err != nil {
			t.Fatal(err)
		}
	}
	replace(t, &settingsHome, func() (string, error) { return home, nil })
	return file
}

// keyAPI is a fake embeddings API that records each Authorization header.
type keyAPI struct {
	*httptest.Server
	mu    sync.Mutex
	auths []string
}

func newKeyAPI(t *testing.T, status int, body string) *keyAPI {
	t.Helper()
	a := &keyAPI{}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.auths = append(a.auths, r.Header.Get("Authorization"))
		a.mu.Unlock()
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(a.Close)
	return a
}

func (a *keyAPI) calls() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.auths...)
}

const embedOK = `{"data":[{"index":0,"embedding":[1,0]}]}`

// searchJSON runs scree search -format json and returns the mode it ran.
func searchJSON(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var runErr error
	out := captureStdout(t, func() {
		runErr = runSearch(context.Background(), append(append([]string{}, args...), "-format", "json", "nightly run"))
	})
	var got struct{ Mode string }
	if runErr == nil {
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("output %q: %v", out, err)
		}
	}
	return got.Mode, runErr
}

// execRec records what useSettingsKey or scree key would have exec'd.
type execRec struct {
	called    bool
	argv, env []string
}

func recordExec(t *testing.T) *execRec {
	t.Helper()
	r := &execRec{}
	replace(t, &selfExecutable, func() (string, error) { return testExe, nil })
	replace(t, &execProgram, func(argv0 string, argv, env []string) error {
		if r.called {
			t.Fatal("exec called twice")
		}
		r.called, r.argv, r.env = true, slices.Clone(argv), slices.Clone(env)
		return nil
	})
	return r
}

// TestDirectCommandsReexecWithSettingsKey: with OPENROUTER_API_KEY unset
// and a key in the settings file, search, reindex, doctor and watch
// re-execute as scree run would, with an environment from the file alone
// plus the marker, so no proxy, CA, GODEBUG or endpoint from the caller's
// environment applies to the key.
func TestDirectCommandsReexecWithSettingsKey(t *testing.T) {
	serveEnv(t)
	file := keyHome(t, "# scree\nexport OPENROUTER_API_KEY='"+testSecret+"'\nSCREE_EMBED_URL=http://127.0.0.1:1/v1\n"+
		"HTTPS_PROXY=http://proxy.internal:3128\nSCREE_KEY_FROM=/elsewhere\n", 0o600)
	home := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	for k, v := range map[string]string{
		"HTTPS_PROXY": "http://127.0.0.1:18801", "https_proxy": "http://127.0.0.1:18801", "SSL_CERT_FILE": "/tmp/ca.pem",
		"SSL_CERT_DIR": "/tmp/ca", "GODEBUG": "http2debug=2", "SCREE_EMBED_URL": "https://attacker.example/v1",
		"SCREE_DB": "/tmp/evil.db", "LD_PRELOAD": "/tmp/evil.so", "TERM": "dumb",
	} {
		t.Setenv(k, v)
	}
	want := []string{"HOME=" + home, "PATH=/usr/local/bin:/usr/bin:/bin", "NO_COLOR=1", "TERM=dumb", "SCREE_KEY_FROM=" + file,
		"HTTPS_PROXY=http://proxy.internal:3128", "OPENROUTER_API_KEY=" + testSecret, "SCREE_EMBED_URL=http://127.0.0.1:1/v1"}
	for _, cmd := range keyCommands {
		r := recordExec(t)
		if err := useSettingsKey(cmd, []string{"-notes", "x y", "q"}); err != nil {
			t.Fatal(err)
		}
		if !r.called || !slices.Equal(r.argv, []string{testExe, cmd, "-notes", "x y", "q"}) {
			t.Errorf("%s: exec %v %q", cmd, r.called, r.argv)
		}
		if !slices.Equal(r.env, want) {
			t.Errorf("%s: child env\n%q\nwant\n%q", cmd, r.env, want)
		}
	}

	// A plain TERM is passed; one that could name a path or hold shell
	// syntax is dropped; TERMINFO never goes through.
	t.Setenv("TERMINFO", "/tmp/evil-terminfo")
	for term, want := range map[string]string{"xterm-256color": "TERM=xterm-256color", "xterm;rm": "", "../evil": "", "/tmp/evil": ""} {
		t.Setenv("TERM", term)
		r := recordExec(t)
		if err := useSettingsKey("search", nil); err != nil || !r.called {
			t.Fatalf("TERM=%q: %v, exec %v", term, err, r.called)
		}
		got := slices.IndexFunc(r.env, func(e string) bool { return strings.HasPrefix(e, "TERM") })
		if want == "" && got >= 0 || want != "" && (got < 0 || r.env[got] != want) {
			t.Errorf("TERM=%q: child env %q", term, r.env)
		}
	}

	// Other commands, a key in the environment, or the marker (a child
	// already re-executed) run here.
	for _, cmd := range []string{"serve", "mcp", "run", "key", "indexes", "version", "help", "-h"} {
		r := recordExec(t)
		if err := useSettingsKey(cmd, nil); err != nil || r.called {
			t.Errorf("%s: %v, exec %v", cmd, err, r.called)
		}
	}
	for k, v := range map[string]string{"OPENROUTER_API_KEY": "env-key", keyFromVar: file} {
		t.Run(k, func(t *testing.T) {
			t.Setenv(k, v)
			r := recordExec(t)
			if err := useSettingsKey("search", nil); err != nil || r.called {
				t.Errorf("%v, exec %v", err, r.called)
			}
		})
	}

	// A file with no key, or none at all: no exec, so a child cannot loop.
	for _, content := range []string{"SCREE_MODEL=x\nOPENROUTER_API_KEY=\n", ""} {
		keyHome(t, content, 0o600)
		r := recordExec(t)
		var err error
		stderr := captureStderr(t, func() { err = useSettingsKey("search", nil) })
		if err != nil || r.called || stderr != "" {
			t.Errorf("%q: %v, exec %v, stderr %q", content, err, r.called, stderr)
		}
	}
}

// TestSettingsKeyChild: the re-executed child uses the key from its
// environment, and doctor says which file it came from.
func TestSettingsKeyChild(t *testing.T) {
	serveEnv(t)
	notes, db := searchFixture(t)
	api := newKeyAPI(t, 200, embedOK)
	file := keyHome(t, "", 0)
	t.Setenv("SCREE_ZDR", "0")
	t.Setenv("SCREE_EMBED_URL", api.URL)
	t.Setenv("OPENROUTER_API_KEY", testSecret)
	t.Setenv(keyFromVar, file)
	r := recordExec(t)
	if err := useSettingsKey("search", nil); err != nil || r.called {
		t.Fatalf("child re-executed: %v, %v", err, r.called)
	}
	mode, err := searchJSON(t, "-notes", notes, "-db", db)
	if err != nil || mode != "semantic" {
		t.Fatalf("mode %q, %v", mode, err)
	}
	if got := api.calls(); len(got) != 1 || got[0] != "Bearer "+testSecret {
		t.Errorf("API calls %q", got)
	}
	out := captureStdout(t, func() {
		runDoctor(context.Background(), []string{"-offline", "-notes", notes, "-db", db})
	})
	if !strings.Contains(out, "ok   OPENROUTER_API_KEY set (from "+file+")") || strings.Contains(out, testSecret) {
		t.Errorf("doctor:\n%s", out)
	}
	t.Setenv(keyFromVar, "")
	out = captureStdout(t, func() {
		runDoctor(context.Background(), []string{"-offline", "-notes", notes, "-db", db})
	})
	if !strings.Contains(out, "ok   OPENROUTER_API_KEY set\n") {
		t.Errorf("doctor without the marker:\n%s", out)
	}
}

// TestSettingsKeyFileRefused: a settings file that fails run's checks gives
// one warning naming the problem, and the command runs without a key.
func TestSettingsKeyFileRefused(t *testing.T) {
	serveEnv(t)
	notes, db := searchFixture(t)
	keyword := []string{"-notes", notes, "-db", db, "-mode", "keyword"}
	check := func(t *testing.T, want string) {
		t.Helper()
		r := recordExec(t)
		var err error
		stderr := captureStderr(t, func() { err = useSettingsKey("search", keyword) })
		if err != nil || r.called || strings.Count(stderr, "\n") != 1 || !strings.HasPrefix(stderr, "scree: warning: ") ||
			!strings.Contains(stderr, want) || !strings.HasSuffix(stderr, "; running without an API key\n") ||
			strings.Contains(stderr, testSecret) {
			t.Errorf("err %v, exec %v, stderr %q (want %q)", err, r.called, stderr, want)
		}
		if mode, err := searchJSON(t, keyword...); err != nil || mode != "keyword" {
			t.Errorf("keyword search: %q, %v", mode, err)
		}
		if mode, err := searchJSON(t, keyword[:4]...); err != nil || mode != "keyword" {
			t.Errorf("auto search: %q, %v", mode, err)
		}
	}

	file := keyHome(t, testSettings, 0o644)
	check(t, file+" is open to group or others")

	file = keyHome(t, "", 0)
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(filepath.Dir(file), "real")
	os.WriteFile(real, []byte(testSettings), 0o600)
	if err := os.Symlink(real, file); err != nil {
		t.Fatal(err)
	}
	check(t, "symlink")

	keyHome(t, testSettings, 0o600)
	t.Run("not ours", func(t *testing.T) {
		replace(t, &launchUID, func() int { return os.Getuid() + 1 })
		check(t, "is not owned by you")
	})

	keyHome(t, testSettings+"NOT_OURS=1\n", 0o600)
	check(t, "line 4: NOT_OURS is not a scree setting")

	keyHome(t, testSettings+"#"+strings.Repeat("x", 70<<10)+"\n", 0o600)
	check(t, "64 KB")
}

// TestServeIgnoresSettingsKey: serve keeps to its environment (under
// Claude Code, scree mcp has applied the settings file already), so it
// neither reads the key nor refuses a bad file, and without a key it says
// to use the extension's settings, set the variable or start through
// scree mcp, never scree key.
func TestServeIgnoresSettingsKey(t *testing.T) {
	serveEnv(t)
	notes, db := searchFixture(t)
	for _, mode := range []os.FileMode{0o600, 0o644} {
		keyHome(t, testSettings, mode)
		r := recordExec(t)
		if err := useSettingsKey("serve", []string{"-notes", notes}); err != nil || r.called {
			t.Errorf("mode %o: %v, exec %v", mode, err, r.called)
		}
		cfg := config{serve: true, notes: notes, db: db}
		envConfig(&cfg)
		ix, err := open(&cfg, true)
		if err != nil {
			t.Fatalf("mode %o: %v", mode, err)
		}
		if cfg.apiKey != "" || !ix.NoKey() {
			t.Errorf("mode %o: serve has a key", mode)
		}
		_, err = ix.Search(context.Background(), "nightly", 3, index.Semantic)
		if err == nil || !errors.Is(err, index.ErrNoKey) ||
			!strings.Contains(err.Error(), "no API key: add an OpenRouter API key in the extension's settings, set OPENROUTER_API_KEY, or start through scree mcp;") ||
			strings.Contains(err.Error(), "scree key") {
			t.Errorf("mode %o: %v", mode, err)
		}
		closeIndexer(ix)
	}
}

// TestAutoWithoutKey: with no key anywhere, auto runs keyword and says so,
// and an explicit semantic or hybrid search is an error naming scree key.
func TestAutoWithoutKey(t *testing.T) {
	serveEnv(t)
	notes, db := searchFixture(t)
	api := newKeyAPI(t, 200, embedOK)
	t.Setenv("SCREE_EMBED_URL", api.URL)
	args := []string{"-notes", notes, "-db", db}
	if mode, err := searchJSON(t, args...); err != nil || mode != "keyword" {
		t.Errorf("json: mode %q, %v", mode, err)
	}
	const note = "[mode: auto → keyword: no API key; run scree key, or set OPENROUTER_API_KEY]"
	for _, format := range []string{"xml", "text"} {
		out := searchOut(t, false, append(args, "-format", format, "purge nightly")...)
		if !strings.Contains(out, "ops.md") || !strings.HasSuffix(strings.TrimSpace(out), note) {
			t.Errorf("%s: %q", format, out)
		}
	}
	for _, mode := range []string{"semantic", "hybrid"} {
		err := runSearch(context.Background(), append(args, "-mode", mode, "nightly"))
		if err == nil || !strings.Contains(err.Error(), "no API key: run scree key, or set OPENROUTER_API_KEY;") {
			t.Errorf("%s: %v", mode, err)
		}
	}
	if got := api.calls(); len(got) != 0 {
		t.Errorf("API called without a key: %q", got)
	}
	out := captureStdout(t, func() {
		runDoctor(context.Background(), []string{"-offline", "-notes", notes, "-db", db})
	})
	if !strings.Contains(out, "FAIL OPENROUTER_API_KEY not set (run scree key, or set OPENROUTER_API_KEY)") {
		t.Errorf("doctor:\n%s", out)
	}
}

// keyRun runs scree key with stdin holding input (or, with terminal, the
// terminal typing it), and returns stdout, stderr and the log together.
func keyRun(t *testing.T, terminal bool, input string, args ...string) (string, error) {
	t.Helper()
	prompted := false
	replace(t, &stdinTerminal, func() bool { return terminal })
	replace(t, &readPassword, func() ([]byte, error) {
		prompted = true
		return []byte(strings.TrimSuffix(input, "\n")), nil
	})
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		if !terminal {
			io.WriteString(w, input)
		}
		w.Close()
	}()
	oldIn := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldIn; r.Close() }()

	er, ew, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldErr := os.Stderr
	os.Stderr = ew
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	errOut := make(chan string)
	go func() {
		b, _ := io.ReadAll(er)
		errOut <- string(b)
	}()
	var runErr error
	out := captureStdout(t, func() { runErr = runKey(context.Background(), args) })
	os.Stderr = oldErr
	log.SetOutput(os.Stderr)
	ew.Close()
	stderr := <-errOut
	if terminal && prompted && !strings.Contains(stderr, "OpenRouter API key: ") {
		t.Errorf("no prompt on stderr: %q", stderr)
	}
	all := out + stderr + logBuf.String()
	if runErr != nil {
		all += runErr.Error()
	}
	if k := strings.TrimSpace(input); k != "" && strings.Contains(all, k) {
		t.Errorf("scree key showed the key: %q", all)
	}
	return out + stderr, runErr
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestKeyWritesSettings: scree key creates the folder (700) and the file
// (600) with the key in single quotes, and tests it against the API.
func TestKeyWritesSettings(t *testing.T) {
	serveEnv(t)
	t.Setenv("SCREE_ZDR", "0")
	file := keyHome(t, "", 0)
	api := newKeyAPI(t, 200, embedOK)
	out, err := keyRun(t, true, testSecret, "-offline")
	if err != nil || !strings.Contains(out, "saved OPENROUTER_API_KEY in "+file) {
		t.Fatalf("%q, %v", out, err)
	}
	if got := readFile(t, file); got != "OPENROUTER_API_KEY='"+testSecret+"'\n" {
		t.Errorf("file %q", got)
	}
	for p, want := range map[string]os.FileMode{file: 0o600, filepath.Dir(file): 0o700} {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s: %v, %v", p, fi.Mode(), err)
		}
	}
	if entries, _ := os.ReadDir(filepath.Dir(file)); len(entries) != 1 {
		t.Errorf("left behind: %v", entries)
	}

	// From stdin, with the test call, which runs in a re-executed scree
	// key -check with the file's settings and the marker.
	os.WriteFile(file, []byte("SCREE_EMBED_URL="+api.URL+"\nSCREE_ZDR=0\n"), 0o600)
	t.Setenv("SCREE_EMBED_URL", "https://attacker.example/v1")
	t.Setenv("TERM", "xterm")
	r := recordExec(t)
	out, err = keyRun(t, false, testSecret+"\r\n")
	if err != nil || !strings.Contains(out, "saved OPENROUTER_API_KEY in "+file) {
		t.Fatalf("%q, %v", out, err)
	}
	home := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	wantEnv := []string{"HOME=" + home, "PATH=/usr/local/bin:/usr/bin:/bin", "TERM=xterm", "SCREE_KEY_FROM=" + file,
		"OPENROUTER_API_KEY=" + testSecret, "SCREE_EMBED_URL=" + api.URL, "SCREE_ZDR=0"}
	if !r.called || !slices.Equal(r.argv, []string{testExe, "key", "-check"}) || !slices.Equal(r.env, wantEnv) {
		t.Fatalf("exec %v %q\nenv %q\nwant %q", r.called, r.argv, r.env, wantEnv)
	}
	if got := api.calls(); len(got) != 0 {
		t.Errorf("the parent called the API: %q", got)
	}
	// The child, given that environment, makes the call.
	for _, kv := range r.env {
		k, v, _ := strings.Cut(kv, "=")
		if k != "HOME" && k != "PATH" {
			t.Setenv(k, v)
		}
	}
	out, err = keyRun(t, false, "", "-check")
	if err != nil || !strings.Contains(out, "ok   embeddings API returned 2 dimensions") {
		t.Fatalf("%q, %v", out, err)
	}
	if got := api.calls(); len(got) != 1 || got[0] != "Bearer "+testSecret {
		t.Errorf("API calls %q", got)
	}

	// -check without the marker re-executes too, and needs a key in the file.
	t.Setenv(keyFromVar, "")
	t.Setenv("OPENROUTER_API_KEY", "")
	r = recordExec(t)
	if _, err := keyRun(t, false, "", "-check"); err != nil || !r.called || !slices.Contains(r.env, "SCREE_KEY_FROM="+file) {
		t.Errorf("-check: %v, exec %v %q", err, r.called, r.env)
	}
	os.WriteFile(file, []byte("SCREE_ZDR=0\n"), 0o600)
	r = recordExec(t)
	if _, err := keyRun(t, false, "", "-check"); err == nil || !strings.Contains(err.Error(), "holds no OPENROUTER_API_KEY") || r.called {
		t.Errorf("-check without a key: %v, exec %v", err, r.called)
	}
}

// TestKeyTestCallFails: a failed test call in the child is an error naming
// the file, never showing the key, even when the API echoes it.
func TestKeyTestCallFails(t *testing.T) {
	serveEnv(t)
	t.Setenv("SCREE_ZDR", "0")
	api := newKeyAPI(t, 401, `{"error":{"message":"bad key `+testSecret+`"}}`)
	file := keyHome(t, "", 0)
	t.Setenv("SCREE_EMBED_URL", api.URL)
	t.Setenv("OPENROUTER_API_KEY", testSecret)
	t.Setenv(keyFromVar, file)
	out, err := keyRun(t, false, "", "-check")
	if err == nil || !strings.Contains(err.Error(), "the test call with the key in "+file+" failed") ||
		strings.Contains(err.Error(), testSecret) || strings.Contains(out, testSecret) {
		t.Errorf("err %v, out %q", err, out)
	}
}

// TestKeyUpdatesSettings: an existing key line is replaced in place, with
// its indent, export and line ending; every other line is kept byte for
// byte; a file without one gets one appended.
func TestKeyUpdatesSettings(t *testing.T) {
	serveEnv(t)
	for _, c := range []struct{ before, after string }{
		{"# mine\r\n  export OPENROUTER_API_KEY=\"old\"\r\nSCREE_ZDR=1\n\nOPENROUTER_API_KEY=older",
			"# mine\r\n  export OPENROUTER_API_KEY='new-key'\r\nSCREE_ZDR=1\n\nOPENROUTER_API_KEY='new-key'"},
		{"# OPENROUTER_API_KEY=commented\nSCREE_MODEL=x", "# OPENROUTER_API_KEY=commented\nSCREE_MODEL=x\nOPENROUTER_API_KEY='new-key'\n"},
		{"SCREE_MODEL=x\n", "SCREE_MODEL=x\nOPENROUTER_API_KEY='new-key'\n"},
	} {
		file := keyHome(t, c.before, 0o600)
		if _, err := keyRun(t, false, "new-key\n", "-offline", "-replace"); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, file); got != c.after {
			t.Errorf("%q: got %q, want %q", c.before, got, c.after)
		}
		if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o600 {
			t.Errorf("mode %v", fi.Mode())
		}
		if _, s, err := readSettings(file); err != nil || s["OPENROUTER_API_KEY"] != "new-key" {
			t.Errorf("reads back as %v", err)
		}
	}
}

// TestKeyRefuses: scree key leaves alone a settings file run would refuse,
// before asking for the key, and refuses a bad key without writing.
func TestKeyRefuses(t *testing.T) {
	serveEnv(t)
	wantRefused := func(t *testing.T, file, before, input, want string) {
		t.Helper()
		_, err := keyRun(t, true, input, "-offline")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err %v, want %q", err, want)
		}
		b, rerr := os.ReadFile(file)
		if before == "" && rerr == nil || before != "" && string(b) != before {
			t.Errorf("file now %q (%v)", b, rerr)
		}
	}

	file := keyHome(t, testSettings, 0o640)
	wantRefused(t, file, testSettings, "new-key", "open to group or others")

	file = keyHome(t, "", 0)
	os.MkdirAll(filepath.Dir(file), 0o700)
	real := filepath.Join(filepath.Dir(file), "real")
	os.WriteFile(real, []byte(testSettings), 0o600)
	os.Symlink(real, file)
	wantRefused(t, real, testSettings, "new-key", "symlink")
	if fi, err := os.Lstat(file); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("symlink replaced: %v", err)
	}

	file = keyHome(t, testSettings, 0o600)
	t.Run("not ours", func(t *testing.T) {
		replace(t, &launchUID, func() int { return os.Getuid() + 1 })
		wantRefused(t, file, testSettings, "new-key", "is not owned by you")
	})

	file = keyHome(t, "SCREE_MODEL=x\nNOT_OURS=1\n", 0o600)
	wantRefused(t, file, "SCREE_MODEL=x\nNOT_OURS=1\n", "new-key", "line 2: NOT_OURS is not a scree setting")

	file = keyHome(t, "", 0)
	os.MkdirAll(filepath.Dir(file), 0o700)
	os.Chmod(filepath.Dir(file), 0o770)
	wantRefused(t, file, "", "new-key", "writable by group or others")

	for input, want := range map[string]string{
		"":                                 "no key given",
		"sk-or key":                        "whitespace",
		"sk-or\tkey":                       "whitespace",
		"sk-or\x1bkey":                     "control",
		"sk-or​key":                        "invisible",
		"sk-or'key":                        "quote",
		strings.Repeat("k", maxKeyBytes+1): "longer than",
	} {
		file := keyHome(t, "", 0)
		wantRefused(t, file, "", input, want)
		file = keyHome(t, "", 0)
		_, err := keyRun(t, false, input+"\n", "-offline")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("stdin %q: %v", input, err)
		}
		if _, err := os.Stat(file); err == nil {
			t.Errorf("stdin %q: file written", input)
		}
	}

	if _, err := keyRun(t, false, "", "extra"); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("argument: %v", err)
	}
}

// TestKeyReplaceFromStdin: from stdin, a key already in the file is
// replaced only with -replace; at the terminal prompt it is replaced.
func TestKeyReplaceFromStdin(t *testing.T) {
	serveEnv(t)
	before := "SCREE_MODEL=x\nOPENROUTER_API_KEY='old-key'\n"
	file := keyHome(t, before, 0o600)
	_, err := keyRun(t, false, "new-key\n", "-offline")
	if err == nil || !strings.Contains(err.Error(), "already holds an API key") || !strings.Contains(err.Error(), "-replace") ||
		strings.Contains(err.Error(), "old-key") {
		t.Errorf("err %v", err)
	}
	if got := readFile(t, file); got != before {
		t.Errorf("file now %q", got)
	}
	if _, err := keyRun(t, false, "new-key\n", "-offline", "-replace"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, file); got != "SCREE_MODEL=x\nOPENROUTER_API_KEY='new-key'\n" {
		t.Errorf("-replace: file %q", got)
	}
	if _, err := keyRun(t, true, "term-key", "-offline"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, file); got != "SCREE_MODEL=x\nOPENROUTER_API_KEY='term-key'\n" {
		t.Errorf("terminal: file %q", got)
	}
	// A file with an empty key line, or none, takes a key from stdin.
	file = keyHome(t, "OPENROUTER_API_KEY=\n", 0o600)
	if _, err := keyRun(t, false, "new-key\n", "-offline"); err != nil || readFile(t, file) != "OPENROUTER_API_KEY='new-key'\n" {
		t.Errorf("empty key line: %v", err)
	}
}

// TestKeyRefusesOversizedFile: scree key never writes a file that the
// 64 KB limit would then refuse.
func TestKeyRefusesOversizedFile(t *testing.T) {
	serveEnv(t)
	var b strings.Builder
	for b.Len() < maxSettingsFile-1000 {
		b.WriteString("# " + strings.Repeat("c", 77) + "\n")
	}
	before := b.String()
	file := keyHome(t, before, 0o600)
	_, err := keyRun(t, false, strings.Repeat("k", maxKeyBytes)+"\n", "-offline")
	if err == nil || !strings.Contains(err.Error(), "larger than 64 KB") {
		t.Errorf("err %v", err)
	}
	if got := readFile(t, file); got != before {
		t.Errorf("file changed: %d bytes", len(got))
	}
	if entries, _ := os.ReadDir(filepath.Dir(file)); len(entries) != 1 {
		t.Errorf("left behind: %v", entries)
	}
	// A short key still fits.
	if _, err := keyRun(t, false, "short-key\n", "-offline"); err != nil {
		t.Errorf("short key: %v", err)
	}
}

// TestKeyRefusesSymlinkedFolders: scree key writes nothing through a
// symlinked ~/.config/scree or ~/.config.
func TestKeyRefusesSymlinkedFolders(t *testing.T) {
	serveEnv(t)
	for _, link := range []string{"scree", "config"} {
		t.Run(link, func(t *testing.T) {
			file := keyHome(t, "", 0)
			dir := filepath.Dir(file)
			linked := dir
			if link == "config" {
				linked = filepath.Dir(dir)
			} else {
				mkdir(t, filepath.Dir(dir))
			}
			target := filepath.Join(tempDir(t), "dotfiles")
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, linked); err != nil {
				t.Fatal(err)
			}
			_, err := keyRun(t, false, "new-key\n", "-offline")
			if err == nil || !strings.Contains(err.Error(), linked+" is a symlink") {
				t.Errorf("err %v", err)
			}
			if entries, _ := os.ReadDir(target); len(entries) != 0 {
				t.Errorf("wrote through the symlink: %v", entries)
			}
		})
	}
}

// TestUnusableEnvKeyUsesSettingsKey: an OPENROUTER_API_KEY that envKey
// reads as no key (blank, an unfilled placeholder, invalid) does not stop a
// direct command from using the settings file's key; a usable one does.
func TestUnusableEnvKeyUsesSettingsKey(t *testing.T) {
	serveEnv(t)
	keyHome(t, testSettings, 0o600)
	for in, reexec := range map[string]bool{
		" \n":                               true,
		"${user_config.openrouter_api_key}": true,
		"bad\x01key":                        true,
		"\n" + testSecret + "\n":            false,
	} {
		t.Setenv("OPENROUTER_API_KEY", in)
		r := recordExec(t)
		var err error
		msg := captureStderr(t, func() { err = useSettingsKey("search", []string{"q"}) })
		if err != nil || r.called != reexec {
			t.Errorf("%q: %v, exec %v, want %v", in, err, r.called, reexec)
		}
		said := strings.Contains(msg, "; using the key from ")
		if said != (reexec && strings.TrimSpace(in) != "") {
			t.Errorf("%q: stderr %q", in, msg)
		}
		if strings.Contains(msg, "bad") || strings.Contains(msg, "user_config") || strings.Contains(msg, testSecret) {
			t.Errorf("%q: stderr shows the value: %q", in, msg)
		}
	}
}

// TestDoctorInvalidKey: a set but unusable OPENROUTER_API_KEY is reported
// as such, not as "not set", and doctor never prints the value.
func TestDoctorInvalidKey(t *testing.T) {
	serveEnv(t)
	notes, db := searchFixture(t)
	for in, want := range map[string]string{
		"bad\x01secretvalue":                "FAIL OPENROUTER_API_KEY is set but not a valid key: the key contains whitespace, control or invisible characters (run scree key, or set OPENROUTER_API_KEY)",
		"${user_config.openrouter_api_key}": "FAIL OPENROUTER_API_KEY is set but is an unfilled placeholder (run scree key, or set OPENROUTER_API_KEY)",
	} {
		t.Setenv("OPENROUTER_API_KEY", in)
		var out string
		captureStderr(t, func() {
			out = captureStdout(t, func() {
				runDoctor(context.Background(), []string{"-offline", "-notes", notes, "-db", db})
			})
		})
		if !strings.Contains(out, want) || strings.Contains(out, "not set") {
			t.Errorf("%q: doctor:\n%s", in, out)
		}
		if strings.Contains(out, "secretvalue") || strings.Contains(out, "user_config") {
			t.Errorf("%q: doctor shows the value:\n%s", in, out)
		}
	}
}
