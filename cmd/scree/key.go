package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/jrapakko/scree/internal/embed"
	"github.com/jrapakko/scree/internal/textsafe"
)

// Direct commands (search, reindex, doctor, watch) with OPENROUTER_API_KEY
// unset use the key in the settings file, which scree key writes, by
// re-executing themselves as scree run would run them: with an environment
// built from nothing but that file. The key then goes only to the file's
// endpoint, through the file's proxy if any, and nothing in the caller's
// environment (a proxy, SSL_CERT_FILE, GODEBUG) applies to it.

// keyFromVar marks a process re-executed to use the settings file's key;
// its value is the file. Such a process never re-executes again.
const keyFromVar = "SCREE_KEY_FROM"

// mcpKeyVar is the settings file's optional key for scree mcp alone, so
// its use can be told apart from direct commands' (on OpenRouter, by
// key). scree mcp passes it to serve as OPENROUTER_API_KEY; it never
// reaches any child under its own name.
const mcpKeyVar = "SCREE_MCP_API_KEY"

// keyCommands are the commands that use the settings file's key.
var keyCommands = []string{"search", "reindex", "doctor", "watch"}

// Hooks that tests replace.
var (
	// stdinTerminal reports whether stdin is a terminal.
	stdinTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
	// readPassword reads a line from the terminal on stdin without echo.
	readPassword = readTerminal
)

// maxKeyBytes bounds the key scree key accepts.
const maxKeyBytes = 1024

// errKeyUnsupported: there is no settings file on Windows.
var errKeyUnsupported = errors.New("scree key is not supported on Windows; set OPENROUTER_API_KEY in the environment")

// readSettings reads and parses the settings file at p with the checks
// scree run applies. A file that does not exist gives nil data and
// settings and no error.
func readSettings(p string) ([]byte, map[string]string, error) {
	data, err := readSettingsFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	s, err := parseSettings(p, data)
	if err != nil {
		return nil, nil, err
	}
	return data, s, nil
}

// useSettingsKey runs before anything else in main. For a command in
// keyCommands, with no usable OPENROUTER_API_KEY (unset, blank, an unfilled
// placeholder or not a valid key; see envKey) and not already re-executed,
// it replaces the process with `scree run <cmd> <args>` when the settings
// file sets a key. A missing file, a file with no key, or no home directory
// to find it in leaves the command to run as it is, without a key; so does
// a file that fails the checks, after a warning on stderr. When the
// variable was set but unusable, the switch to the file's key is said on
// stderr, with the reason and never the value. It returns only
// when the command should run here, or when the exec failed.
func useSettingsKey(cmd string, args []string) error {
	k, problem := envKey()
	if !slices.Contains(keyCommands, cmd) || runtime.GOOS == "windows" ||
		k != "" || os.Getenv(keyFromVar) != "" {
		return nil
	}
	home, err := settingsHome()
	if err != nil {
		return nil
	}
	p := settingsPath(home)
	_, s, err := readSettings(p)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scree: warning: %s; running without an API key\n", textsafe.Strip(err.Error()))
		return nil
	}
	if s["OPENROUTER_API_KEY"] == "" {
		return nil
	}
	if problem != "" {
		// Say why the variable was passed over; problem never holds the value.
		fmt.Fprintf(os.Stderr, "scree: %s; using the key from %s\n", problem, textsafe.Strip(p))
	}
	return reexecWithKey(home, p, s, append([]string{cmd}, args...)...)
}

// reexecWithKey replaces the process with this binary running args, in
// the environment scree run gives it, marked with keyFromVar=p.
func reexecWithKey(home, p string, s map[string]string, args ...string) error {
	delete(s, keyFromVar)
	return reexec(home, s, append(runExtra(), keyFromVar+"="+p), args...)
}

// noKeyHint says how to supply the API key, for messages about its
// absence. serve takes it only from its environment, which a client sets:
// under Claude Code (CLAUDE_PROJECT_DIR set) scree mcp sets it from the
// settings file, which scree key writes; elsewhere, such as Claude
// Desktop, the extension's settings do. Windows has no settings file.
func noKeyHint(serve bool) string {
	const ext = "add an OpenRouter API key in the extension's settings, "
	claudeCode := os.Getenv("CLAUDE_PROJECT_DIR") != ""
	switch {
	case runtime.GOOS == "windows" && serve && !claudeCode:
		return ext + "or set OPENROUTER_API_KEY"
	case runtime.GOOS == "windows":
		return "set OPENROUTER_API_KEY"
	case serve && !claudeCode:
		return ext + "set OPENROUTER_API_KEY, or start through scree mcp"
	}
	return "run scree key, or set OPENROUTER_API_KEY"
}

// runKey is `scree key [-mcp] [-offline] [-replace] [-check]`: it reads
// the API key (prompted for without echo on a terminal, otherwise one line
// from stdin), saves it in the settings file, and unless -offline makes one
// test embedding call; -check makes only the call. With -mcp it does the
// same for SCREE_MCP_API_KEY, the key scree mcp uses instead of
// OPENROUTER_API_KEY. The key is never printed. main calls it with no
// signal handling installed, so Ctrl-C at the prompt ends scree at once.
func runKey(ctx context.Context, args []string) error {
	// childMarker, first, is checkInChild's re-exec; it is not a flag, so
	// usage never shows it.
	child := len(args) > 0 && args[0] == childMarker
	if child {
		args = args[1:]
	}
	set := flag.NewFlagSet("key", flag.ExitOnError)
	mcpKey := set.Bool("mcp", false, "save or check SCREE_MCP_API_KEY, the key scree mcp uses, instead of OPENROUTER_API_KEY")
	offline := set.Bool("offline", false, "skip the test call to the embeddings API")
	replaceKey := set.Bool("replace", false, "replace a key already in the file when reading the new one from stdin")
	check := set.Bool("check", false, "only make the test call, with the key in the settings file")
	set.Parse(args)
	if set.NArg() != 0 {
		return errors.New("usage: scree key [-mcp] [-offline] [-replace] [-check] (the key is prompted for, or read from stdin)")
	}
	if runtime.GOOS == "windows" {
		return errKeyUnsupported
	}
	name, prompt := "OPENROUTER_API_KEY", "OpenRouter API key: "
	if *mcpKey {
		name, prompt = mcpKeyVar, "OpenRouter API key for scree mcp: "
	}
	// Only checkInChild's re-exec tests the key in its environment; a
	// SCREE_KEY_FROM inherited from a caller is not that.
	if child {
		if !*check || os.Getenv(keyFromVar) == "" {
			return errors.New("key: " + childMarker + " is for scree's own re-exec of scree key -check")
		}
		return testKey(ctx, os.Getenv(keyFromVar), name)
	}
	home, err := settingsHome()
	if err != nil {
		return err
	}
	p := settingsPath(home)
	// Refuse a settings file that fails the checks before asking.
	_, s, err := readSettings(p)
	if err != nil {
		return err
	}
	if *check {
		return checkInChild(home, p, s, *mcpKey)
	}
	terminal := stdinTerminal()
	if !terminal && s[name] != "" && !*replaceKey {
		return fmt.Errorf("%s already holds an API key (%s); nothing saved (to replace it from stdin, add -replace)", p, name)
	}
	key, err := readKey(terminal, prompt)
	if err != nil {
		return err
	}
	if err := saveKey(p, name, key); err != nil {
		return err
	}
	fmt.Printf("saved %s in %s\n", name, p)
	if !*mcpKey && os.Getenv("OPENROUTER_API_KEY") != "" {
		fmt.Println("note: OPENROUTER_API_KEY is set in the environment, and commands run directly use that one")
	}
	if *offline {
		return nil
	}
	if _, s, err = readSettings(p); err != nil {
		return err
	}
	return checkInChild(home, p, s, *mcpKey)
}

// childMarker is the first argument of the scree key -check that
// checkInChild runs.
const childMarker = "-child"

// checkInChild replaces the process with `scree key -check`, run with the
// settings file's key the way direct commands are (see useSettingsKey).
// With mcp the key is SCREE_MCP_API_KEY, passed as OPENROUTER_API_KEY the
// way scree mcp passes it, and the child is `scree key -check -mcp`.
func checkInChild(home, p string, s map[string]string, mcp bool) error {
	if s == nil {
		return settingsMissing(p)
	}
	if !mcp {
		if s["OPENROUTER_API_KEY"] == "" {
			return fmt.Errorf("%s holds no OPENROUTER_API_KEY; run scree key", p)
		}
		return reexecWithKey(home, p, s, "key", childMarker, "-check")
	}
	k, problem := settingsMCPKey(s)
	switch {
	case problem != "":
		return fmt.Errorf("%s in %s; not testing it (replace it with scree key -mcp)", problem, p)
	case k == "":
		return fmt.Errorf("%s holds no %s; run scree key -mcp", p, mcpKeyVar)
	}
	s = maps.Clone(s)
	s["OPENROUTER_API_KEY"] = k
	return reexecWithKey(home, p, s, "key", childMarker, "-check", "-mcp")
}

// testKey makes one test embedding call with the key and settings in the
// environment, which checkInChild built from the settings file p; name is
// the file's variable the key came from, for messages.
func testKey(ctx context.Context, p, name string) error {
	var cfg config
	envConfig(&cfg)
	if cfg.apiKey == "" {
		hint := "scree key"
		if name == mcpKeyVar {
			hint = "scree key -mcp"
		}
		return fmt.Errorf("%s holds no %s; run %s", p, name, hint)
	}
	zdr, err := zdrSetting()
	if err != nil {
		return err
	}
	e := embed.New(cfg.url, cfg.apiKey, cfg.model, cfg.dims)
	e.ZDR = zdr
	e.Timeout, e.MaxAttempts = 10*time.Second, 2
	vecs, err := e.Embed(ctx, []string{"scree key"})
	if err != nil {
		return fmt.Errorf("the test call with the key in %s failed: %w", p, err)
	}
	fmt.Printf("ok   embeddings API returned %d dimensions\n", len(vecs[0]))
	return nil
}

// readTerminal reads a line from the terminal on stdin without echo. On
// SIGINT or SIGTERM it restores the terminal and exits at once, so nothing
// is saved; the default action would leave echo off.
func readTerminal() ([]byte, error) {
	fd := int(os.Stdin.Fd())
	old, err := term.GetState(fd)
	if err != nil {
		return nil, err
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-sig:
			term.Restore(fd, old)
			fmt.Fprintln(os.Stderr, "\nscree: interrupted; nothing saved")
			os.Exit(130)
		case <-done:
		}
	}()
	return term.ReadPassword(fd)
}

// readKey reads the key: from the terminal without echo, after prompt on
// stderr, or else the first line of stdin.
func readKey(terminal bool, prompt string) (string, error) {
	var b []byte
	var err error
	if terminal {
		fmt.Fprint(os.Stderr, prompt)
		b, err = readPassword()
		fmt.Fprintln(os.Stderr)
	} else {
		b, err = bufio.NewReader(io.LimitReader(os.Stdin, maxKeyBytes+2)).ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			err = nil
		}
	}
	if err != nil {
		return "", fmt.Errorf("reading the key: %w", err)
	}
	b = bytes.TrimSuffix(b, []byte("\n"))
	b = bytes.TrimSuffix(b, []byte("\r"))
	return string(b), checkKey(b)
}

// checkKey refuses an empty key, one over maxKeyBytes, and one holding
// whitespace, control or invisible characters, quotes or a backslash,
// which the settings file could not hold as written. Errors never show the
// key.
func checkKey(b []byte) error {
	if len(b) == 0 {
		return errors.New("no key given; nothing saved")
	}
	if p := keyProblem(b); p != "" {
		return errors.New(p + "; nothing saved")
	}
	return nil
}

// keyProblem says what is wrong with a non-empty key, by checkKey's rules,
// or "" if nothing is. It never shows the key.
func keyProblem(b []byte) string {
	switch {
	case len(b) > maxKeyBytes:
		return fmt.Sprintf("the key is longer than %d bytes", maxKeyBytes)
	case !utf8.Valid(b):
		return "the key is not valid UTF-8"
	}
	for _, r := range string(b) {
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || !unicode.IsPrint(r) {
			return "the key contains whitespace, control or invisible characters"
		}
		if r == '\'' || r == '"' || r == '\\' {
			return "the key contains a quote or backslash"
		}
	}
	return ""
}

// envKey returns OPENROUTER_API_KEY as scree uses it: surrounding
// whitespace trimmed (a key pasted with a newline works), and "" for a
// value that is still a client's unfilled placeholder (starts with "${") or
// that fails checkKey's rules, with problem saying why in words that never
// show the value. An unset or empty variable is "" with no problem.
func envKey() (key, problem string) {
	return usableKey("OPENROUTER_API_KEY", os.Getenv("OPENROUTER_API_KEY"))
}

// settingsMCPKey is the settings file's SCREE_MCP_API_KEY read as envKey
// reads OPENROUTER_API_KEY, except that a line setting it empty or blank
// is a problem too: its fallback to OPENROUTER_API_KEY would merge the
// two keys' costs unnoticed. As for every setting, the file's last line
// for it counts.
func settingsMCPKey(s map[string]string) (key, problem string) {
	v, ok := s[mcpKeyVar]
	if ok && strings.TrimSpace(v) == "" {
		return "", mcpKeyVar + " is set but empty (the file's last " + mcpKeyVar + " line is the one used)"
	}
	return usableKey(mcpKeyVar, v)
}

// usableKey is envKey's reading of the value v of the variable name.
func usableKey(name, v string) (key, problem string) {
	v = strings.TrimSpace(v)
	switch {
	case v == "":
		return "", ""
	case strings.HasPrefix(v, "${"):
		return "", name + " is set but is an unfilled placeholder"
	}
	if p := keyProblem([]byte(v)); p != "" {
		return "", name + " is set but not a valid key: " + p
	}
	return v, ""
}

// saveKey writes key into the settings file at p as the variable name
// (OPENROUTER_API_KEY or SCREE_MCP_API_KEY), creating its folder (mode 700)
// if needed. An existing line for name is replaced
// (keeping its indent and any "export "), or one is appended; every other
// line is kept byte for byte. The file is replaced atomically by a new one
// with mode 600, and only if it passes the checks scree run applies; the
// folder must be owned by the user and not writable by group or others.
func saveKey(p, name, key string) error {
	dir := filepath.Dir(p)
	// Neither folder may be a symlink, which could lead the key into a
	// dotfiles repo or a folder others can read; ~/.config is checked
	// before MkdirAll could create anything through it.
	if err := refuseSymlink(filepath.Dir(dir)); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := checkKeyDir(dir); err != nil {
		return err
	}
	data, _, err := readSettings(p)
	if err != nil {
		return err
	}
	out := setKeyLine(data, name, key)
	if len(out) > maxSettingsFile {
		return fmt.Errorf("%s would be larger than 64 KB with the key; nothing saved", p)
	}
	tmp, err := os.CreateTemp(dir, ".env-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(out)
	if err == nil {
		err = tmp.Chmod(0o600)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("writing %s: %w", p, err)
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return fmt.Errorf("writing %s: %w", p, err)
	}
	return nil
}

// refuseSymlink refuses p if it is a symlink; a p that does not exist
// passes.
func refuseSymlink(p string) error {
	fi, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; not writing the key through it", p)
	}
	return nil
}

// checkKeyDir refuses a settings folder that is a symlink or not a
// directory, is not owned by launchUID, or is writable by group or others,
// any of whom could replace the file.
func checkKeyDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; not writing the key through it", dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a folder", dir)
	}
	if err := checkSettingsOwner(fi); err != nil {
		return fmt.Errorf("%s %w", dir, err)
	}
	if perm := fi.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("%s is writable by group or others (mode %o); fix with: chmod 700 %s", dir, perm, dir)
	}
	return nil
}

// setKeyLine returns data with every line setting the variable name, as
// parseSettings reads them, set to key in single quotes, or with such a
// line appended when there is none.
func setKeyLine(data []byte, name, key string) []byte {
	name += "="
	line := name + "'" + key + "'"
	var b bytes.Buffer
	found := false
	for _, l := range strings.SplitAfter(string(data), "\n") {
		body := strings.TrimSuffix(strings.TrimSuffix(l, "\n"), "\r")
		rest := strings.TrimLeft(body, " \t\v\f\r")
		rest = strings.TrimPrefix(rest, "export ")
		if !strings.HasPrefix(rest, name) {
			b.WriteString(l)
			continue
		}
		b.WriteString(body[:len(body)-len(rest)] + line + l[len(body):])
		found = true
	}
	if !found {
		if b.Len() > 0 && !bytes.HasSuffix(b.Bytes(), []byte("\n")) {
			b.WriteByte('\n')
		}
		b.WriteString(line + "\n")
	}
	return b.Bytes()
}
