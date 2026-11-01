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
// keyCommands, with OPENROUTER_API_KEY unset and not already re-executed,
// it replaces the process with `scree run <cmd> <args>` when the settings
// file sets a key. A missing file, a file with no key, or no home directory
// to find it in leaves the command to run as it is, without a key; so does
// a file that fails the checks, after a warning on stderr. It returns only
// when the command should run here, or when the exec failed.
func useSettingsKey(cmd string, args []string) error {
	if !slices.Contains(keyCommands, cmd) || runtime.GOOS == "windows" ||
		os.Getenv("OPENROUTER_API_KEY") != "" || os.Getenv(keyFromVar) != "" {
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
	return reexecWithKey(home, p, s, append([]string{cmd}, args...)...)
}

// reexecWithKey replaces the process with this binary running args, in
// the environment scree run gives it, marked with keyFromVar=p.
func reexecWithKey(home, p string, s map[string]string, args ...string) error {
	delete(s, keyFromVar)
	return reexec(home, s, append(runExtra(), keyFromVar+"="+p), args...)
}

// noKeyHint says how to supply the API key, for messages about its
// absence: serve takes it only from its environment, which scree mcp sets
// from the settings file, and Windows has no settings file.
func noKeyHint(serve bool) string {
	switch {
	case runtime.GOOS == "windows":
		return "set OPENROUTER_API_KEY"
	case serve:
		return "set OPENROUTER_API_KEY, or start through scree mcp"
	}
	return "run scree key, or set OPENROUTER_API_KEY"
}

// runKey is `scree key [-offline] [-replace] [-check]`: it reads the API
// key (prompted for without echo on a terminal, otherwise one line from
// stdin), saves it in the settings file, and unless -offline makes one test
// embedding call; -check makes only the call. The key is never printed.
// main calls it with no signal handling installed, so Ctrl-C at the prompt
// ends scree at once.
func runKey(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("key", flag.ExitOnError)
	offline := set.Bool("offline", false, "skip the test call to the embeddings API")
	replaceKey := set.Bool("replace", false, "replace a key already in the file when reading the new one from stdin")
	check := set.Bool("check", false, "only make the test call, with the key in the settings file")
	set.Parse(args)
	if set.NArg() != 0 {
		return errors.New("usage: scree key [-offline] [-replace] [-check] (the key is prompted for, or read from stdin)")
	}
	if runtime.GOOS == "windows" {
		return errKeyUnsupported
	}
	if *check && os.Getenv(keyFromVar) != "" {
		return testKey(ctx, os.Getenv(keyFromVar))
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
		return checkInChild(home, p, s)
	}
	terminal := stdinTerminal()
	if !terminal && s["OPENROUTER_API_KEY"] != "" && !*replaceKey {
		return fmt.Errorf("%s already holds an API key; nothing saved (to replace it from stdin, add -replace)", p)
	}
	key, err := readKey(terminal)
	if err != nil {
		return err
	}
	if err := saveKey(p, key); err != nil {
		return err
	}
	fmt.Printf("saved OPENROUTER_API_KEY in %s\n", p)
	if os.Getenv("OPENROUTER_API_KEY") != "" {
		fmt.Println("note: OPENROUTER_API_KEY is set in the environment, and commands run directly use that one")
	}
	if *offline {
		return nil
	}
	if _, s, err = readSettings(p); err != nil {
		return err
	}
	return checkInChild(home, p, s)
}

// checkInChild replaces the process with `scree key -check`, run with the
// settings file's key the way direct commands are (see useSettingsKey).
func checkInChild(home, p string, s map[string]string) error {
	if s == nil {
		return settingsMissing(p)
	}
	if s["OPENROUTER_API_KEY"] == "" {
		return fmt.Errorf("%s holds no OPENROUTER_API_KEY; run scree key", p)
	}
	return reexecWithKey(home, p, s, "key", "-check")
}

// testKey makes one test embedding call with the key and settings in the
// environment, which checkInChild built from the settings file p.
func testKey(ctx context.Context, p string) error {
	var cfg config
	envConfig(&cfg)
	if cfg.apiKey == "" {
		return fmt.Errorf("%s holds no OPENROUTER_API_KEY; run scree key", p)
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

// readKey reads the key: from the terminal without echo, after a prompt on
// stderr, or else the first line of stdin.
func readKey(terminal bool) (string, error) {
	var b []byte
	var err error
	if terminal {
		fmt.Fprint(os.Stderr, "OpenRouter API key: ")
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
	switch {
	case len(b) == 0:
		return errors.New("no key given; nothing saved")
	case len(b) > maxKeyBytes:
		return fmt.Errorf("the key is longer than %d bytes; nothing saved", maxKeyBytes)
	case !utf8.Valid(b):
		return errors.New("the key is not valid UTF-8; nothing saved")
	}
	for _, r := range string(b) {
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || !unicode.IsPrint(r) {
			return errors.New("the key contains whitespace, control or invisible characters; nothing saved")
		}
		if r == '\'' || r == '"' || r == '\\' {
			return errors.New("the key contains a quote or backslash; nothing saved")
		}
	}
	return nil
}

// saveKey writes key into the settings file at p, creating its folder
// (mode 700) if needed. An existing OPENROUTER_API_KEY line is replaced
// (keeping its indent and any "export "), or one is appended; every other
// line is kept byte for byte. The file is replaced atomically by a new one
// with mode 600, and only if it passes the checks scree run applies; the
// folder must be owned by the user and not writable by group or others.
func saveKey(p, key string) error {
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
	out := setKeyLine(data, key)
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

// setKeyLine returns data with every OPENROUTER_API_KEY line, as
// parseSettings reads them, set to key in single quotes, or with such a
// line appended when there is none.
func setKeyLine(data []byte, key string) []byte {
	const name = "OPENROUTER_API_KEY="
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
