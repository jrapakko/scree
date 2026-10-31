//go:build unix

package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const testSecret = "sk-or-v1-test-secret-0123456789"

const testSettings = "# scree settings\nOPENROUTER_API_KEY=" + testSecret + "\nSCREE_MODEL=openai/text-embedding-3-small\n"

const testExe = "/opt/scree/bin/scree"

// launched records what scree mcp or scree run would have exec'd.
type launched struct {
	called      bool
	argv0       string
	argv, env   []string
	home, file  string // the fake passwd home and its settings file
	proj        string // <home>/proj, a folder outside any repo
	engineDir   string // the only folder docker and podman are found in
	plantedPATH string // a folder with docker, podman and scree, on the inherited PATH
}

// launchSetup makes a fake passwd home holding the settings file (unless
// content is ""), with mode, replaces the passwd lookup, this binary's path
// and exec, and clears the variables the tests set.
func launchSetup(t *testing.T, content string, mode os.FileMode) *launched {
	t.Helper()
	home := tempDir(t)
	l := &launched{home: home, file: filepath.Join(home, ".config", "scree", "env"), proj: filepath.Join(home, "proj"),
		engineDir: filepath.Join(home, "engines"), plantedPATH: filepath.Join(home, "planted")}
	mkdir(t, filepath.Dir(l.file))
	mkdir(t, l.proj)
	for _, d := range []string{l.engineDir, l.plantedPATH} {
		mkdir(t, d)
		for _, n := range []string{"docker", "podman", "scree"} {
			if err := os.WriteFile(filepath.Join(d, n), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	if content != "" {
		if err := os.WriteFile(l.file, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(l.file, mode); err != nil {
			t.Fatal(err)
		}
	}
	replace(t, &settingsHome, func() (string, error) { return home, nil })
	replace(t, &selfExecutable, func() (string, error) { return testExe, nil })
	replace(t, &engineDirs, []string{filepath.Join(home, "nonexistent"), l.engineDir})
	replace(t, &execProgram, func(argv0 string, argv, env []string) error {
		if l.called {
			t.Fatal("exec called twice")
		}
		l.called, l.argv0, l.argv, l.env = true, argv0, slices.Clone(argv), slices.Clone(env)
		return nil
	})
	t.Setenv("CLAUDE_PROJECT_DIR", l.proj)
	t.Chdir(l.proj)
	return l
}

func replace[T any](t *testing.T, p *T, v T) {
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// wantRefused checks that err mentions want and nothing was exec'd.
func (l *launched) wantRefused(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) || l.called {
		t.Errorf("err %v (want %q), exec called %v", err, want, l.called)
	}
	if err != nil && strings.Contains(err.Error(), testSecret) {
		t.Errorf("error shows the key: %v", err)
	}
	l.called = false
}

// hostile is an environment a repo's .mcp.json could set: none of it may
// reach the re-executed scree or docker.
func hostile(l *launched) map[string]string {
	return map[string]string{
		"LD_PRELOAD":            "/tmp/evil.so",
		"LD_LIBRARY_PATH":       "/tmp",
		"DYLD_INSERT_LIBRARIES": "/tmp/evil.dylib",
		"SHELLOPTS":             "xtrace",
		"PS4":                   "$(touch /tmp/pwned)",
		"BASH_ENV":              "/tmp/evil.sh",
		"ENV":                   "/tmp/evil.sh",
		"BASH_FUNC_echo%%":      "() { touch /tmp/pwned; }",
		"GODEBUG":               "panicnil=1",
		"GOTRACEBACK":           "crash",
		"GOMAXPROCS":            "1",
		"GOGC":                  "off",
		"PATH":                  l.plantedPATH + ":/usr/bin:/bin",
		"HOME":                  "/nonexistent",
		"XDG_DATA_HOME":         "/tmp/evil",
		"XDG_CONFIG_HOME":       "/tmp/evil",
		"SCREE_EMBED_URL":       "http://attacker.example/v1/embeddings",
		"SCREE_DB":              "/tmp/evil.db",
		"SCREE_ENV_FILE":        "/tmp/evil-env",
		"SCREE_DOCKER":          filepath.Join(l.plantedPATH, "docker"),
		"SCREE_DATA_VOLUME":     "/",
		"SCREE_MOUNT_OPTS":      "rw",
		"SCREE_IMAGE":           "attacker/image",
		"OPENROUTER_API_KEY":    "attacker-key",
		"DOCKER_HOST":           "tcp://attacker.example:2375",
		"DOCKER_CONTEXT":        "attacker",
		"HTTPS_PROXY":           "http://attacker.example:3128",
		"http_proxy":            "http://attacker.example:3128",
		"NO_PROXY":              "*",
	}
}

// TestMCPCleanEnv: scree mcp re-executes this binary as `scree serve` with
// exactly HOME (the passwd home), a fixed PATH, CLAUDE_PROJECT_DIR and the
// settings from the file, whatever the inherited environment holds.
func TestMCPCleanEnv(t *testing.T) {
	l := launchSetup(t, testSettings+"export HTTPS_PROXY='http://proxy.internal:3128'\nDOCKER_HOST=unix:///run/docker.sock\n", 0o600)
	for k, v := range hostile(l) {
		t.Setenv(k, v)
	}
	// Only scree run passes NO_COLOR on.
	t.Setenv("NO_COLOR", "1")
	t.Setenv("TERM", "dumb")
	if err := runMCP(nil); err != nil {
		t.Fatal(err)
	}
	if !l.called || l.argv0 != testExe || !slices.Equal(l.argv, []string{testExe, "serve"}) {
		t.Errorf("exec %q %q", l.argv0, l.argv)
	}
	want := []string{
		"HOME=" + l.home,
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"CLAUDE_PROJECT_DIR=" + l.proj,
		"HTTPS_PROXY=http://proxy.internal:3128",
		"OPENROUTER_API_KEY=" + testSecret,
		"SCREE_MODEL=openai/text-embedding-3-small",
	}
	if !slices.Equal(l.env, want) {
		t.Errorf("child env\n%q\nwant\n%q", l.env, want)
	}
}

// TestRunCleanEnvAndArgs: scree run passes its arguments through, with the
// same clean environment, plus NO_COLOR=1 when NO_COLOR is set or TERM is
// dumb; TERM itself never.
func TestRunCleanEnvAndArgs(t *testing.T) {
	l := launchSetup(t, testSettings, 0o600)
	for k, v := range hostile(l) {
		t.Setenv(k, v)
	}
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	if err := runRun([]string{"search", "a b", "-k", "3"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(l.argv, []string{testExe, "search", "a b", "-k", "3"}) {
		t.Errorf("argv %q", l.argv)
	}
	want := []string{"HOME=" + l.home, "PATH=/usr/local/bin:/usr/bin:/bin", "OPENROUTER_API_KEY=" + testSecret, "SCREE_MODEL=openai/text-embedding-3-small"}
	if !slices.Equal(l.env, want) {
		t.Errorf("child env\n%q\nwant\n%q", l.env, want)
	}
	withNoColor := slices.Insert(slices.Clone(want), 2, "NO_COLOR=1")
	for _, env := range [][2]string{{"NO_COLOR", "always; rm -rf"}, {"TERM", "dumb"}} {
		t.Setenv("NO_COLOR", "")
		t.Setenv("TERM", "xterm-256color")
		t.Setenv(env[0], env[1])
		l.called = false
		if err := runRun([]string{"search", "a b", "-k", "3"}); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(l.env, withNoColor) {
			t.Errorf("%s=%s: child env\n%q\nwant\n%q", env[0], env[1], l.env, withNoColor)
		}
	}
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	// Under Claude Code, CLAUDE_PROJECT_DIR goes through, so serve's checks apply.
	t.Setenv("CLAUDE_PROJECT_DIR", l.proj)
	l.called = false
	if err := runRun([]string{"doctor", "-v"}); err != nil || !slices.Contains(l.env, "CLAUDE_PROJECT_DIR="+l.proj) {
		t.Errorf("err %v, env %q", err, l.env)
	}
}

func TestRunRefuses(t *testing.T) {
	l := launchSetup(t, testSettings, 0o600)
	l.wantRefused(t, runRun(nil), "usage: scree run")
	l.wantRefused(t, runRun([]string{"mcp"}), "cannot run mcp")
	l.wantRefused(t, runRun([]string{"run", "doctor"}), "cannot run run")
}

// TestMCPRefusesArgs: arguments could come from a repo's .mcp.json.
func TestMCPRefusesArgs(t *testing.T) {
	l := launchSetup(t, testSettings, 0o600)
	for _, args := range [][]string{{"-notes", "/"}, {"serve"}, {"-docker", "-notes", "/"}, {"--docker"}, {"-docker=true"}, {""}, {"-db", "/tmp/x"}} {
		l.wantRefused(t, runMCP(args), "takes no arguments but -docker")
	}
}

func TestMCPNeedsProjectDir(t *testing.T) {
	l := launchSetup(t, testSettings, 0o600)
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	l.wantRefused(t, runMCP(nil), "CLAUDE_PROJECT_DIR is not set")
	l.wantRefused(t, runMCP([]string{"-docker"}), "CLAUDE_PROJECT_DIR is not set")
}

func TestSettingsFileRefusals(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o640, 0o602, 0o610, 0o601} {
		l := launchSetup(t, testSettings, mode)
		l.wantRefused(t, runMCP(nil), "chmod 600 "+l.file)
		l.wantRefused(t, runRun([]string{"doctor"}), "open to group or others")
	}
	l := launchSetup(t, "", 0)
	l.wantRefused(t, runMCP(nil), l.file+" not found; create it with OPENROUTER_API_KEY, then: chmod 600")

	t.Run("not owned by us", func(t *testing.T) {
		l := launchSetup(t, testSettings, 0o600)
		replace(t, &launchUID, func() int { return os.Getuid() + 1 })
		l.wantRefused(t, runMCP(nil), l.file+" is not owned by you")
		l.wantRefused(t, runRun([]string{"doctor"}), "is not owned by you")
	})

	// A symlink, even to a good file of ours.
	l = launchSetup(t, "", 0)
	real := filepath.Join(l.home, "real-env")
	if err := os.WriteFile(real, []byte(testSettings), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, l.file); err != nil {
		t.Fatal(err)
	}
	l.wantRefused(t, runMCP(nil), "symlink")

	// A folder.
	l = launchSetup(t, "", 0)
	mkdir(t, l.file)
	l.wantRefused(t, runMCP(nil), "not a regular file")

	// Too large.
	l = launchSetup(t, testSettings+"#"+strings.Repeat("x", 70<<10)+"\n", 0o600)
	l.wantRefused(t, runMCP(nil), "64 KB")
}

// TestSettingsNotFromEnvironment: the settings file is the one under the
// passwd home, whatever HOME, XDG_CONFIG_HOME or SCREE_ENV_FILE say.
func TestSettingsNotFromEnvironment(t *testing.T) {
	l := launchSetup(t, testSettings, 0o600)
	other := tempDir(t)
	planted := filepath.Join(other, ".config", "scree", "env")
	mkdir(t, filepath.Dir(planted))
	os.WriteFile(planted, []byte("OPENROUTER_API_KEY=planted-key\n"), 0o600)
	t.Setenv("HOME", other)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(other, ".config"))
	t.Setenv("SCREE_ENV_FILE", planted)
	if err := runMCP(nil); err != nil || !slices.Contains(l.env, "OPENROUTER_API_KEY="+testSecret) {
		t.Errorf("err %v, env %q", err, l.env)
	}
}

func TestParseSettingsSyntax(t *testing.T) {
	content := "  # indented comment\n\n\t\n" +
		"export OPENROUTER_API_KEY=\"" + testSecret + "\"\r\n" +
		"SCREE_DB='/home/u/my notes'\n" +
		"SCREE_MODEL=\"it's fine\"\n" +
		"\tSCREE_ZDR=0\n" +
		"SCREE_EMBED_URL=http://a=b\n" +
		"SCREE_HTTP_TOKEN=\"\n" +
		"SCREE_IMAGE='x\"\n" +
		"SCREE_DATA_VOLUME=\n" +
		"DOCKER_CONTEXT=desktop-linux\n" +
		"no_proxy=localhost\n" +
		"SCREE_DIMENSIONS=128\n" +
		"SCREE_DIMENSIONS=256" // no final newline; the later line wins
	got, err := parseSettings("env", []byte(content))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"OPENROUTER_API_KEY": testSecret,
		"SCREE_DB":           "/home/u/my notes",
		"SCREE_MODEL":        "it's fine",
		"SCREE_ZDR":          "0",
		"SCREE_EMBED_URL":    "http://a=b",
		"SCREE_HTTP_TOKEN":   `"`,
		"SCREE_IMAGE":        `'x"`,
		"SCREE_DATA_VOLUME":  "",
		"DOCKER_CONTEXT":     "desktop-linux",
		"no_proxy":           "localhost",
		"SCREE_DIMENSIONS":   "256",
	}
	if len(got) != len(want) {
		t.Errorf("got %q, want %q", got, want)
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			t.Errorf("%s = %q, want %q", k, g, v)
		}
	}
}

// TestSettingsAreData: nothing in a value is expanded or run.
func TestSettingsAreData(t *testing.T) {
	l := launchSetup(t, "SCREE_DB=/Users/u/Library/Mobile Documents/iCloud~md~obsidian/Vault\n"+
		"OPENROUTER_API_KEY="+testSecret+"\n"+
		"SCREE_MODEL=$(touch pwned-subst)`touch pwned-tick` ${HOME}\n", 0o600)
	if err := runRun([]string{"doctor"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"SCREE_DB=/Users/u/Library/Mobile Documents/iCloud~md~obsidian/Vault",
		"SCREE_MODEL=$(touch pwned-subst)`touch pwned-tick` ${HOME}",
	} {
		if !slices.Contains(l.env, want) {
			t.Errorf("missing %q in %q", want, l.env)
		}
	}
}

func TestParseSettingsRefusesOtherNames(t *testing.T) {
	for line, want := range map[string]string{
		"PATH=/tmp":               "line 4: PATH is not a scree setting",
		"LD_PRELOAD=/tmp/x.so":    "line 4: LD_PRELOAD is not a scree setting",
		"BASH_ENV=/tmp/x":         "line 4: BASH_ENV is not a scree setting",
		"GODEBUG=x=1":             "line 4: GODEBUG is not a scree setting",
		"HOME=/tmp":               "line 4: HOME is not a scree setting",
		"CLAUDE_PROJECT_DIR=/etc": "line 4: CLAUDE_PROJECT_DIR is not a scree setting",
		"SCREE_=1":                "line 4: SCREE_ is not a scree setting",
		"SCREE_x=1":               "line 4: SCREE_x is not a scree setting",
		"not a setting":           "line 4: expected NAME=value",
		"SCREE-X=1":               "line 4: not a scree setting name",
		"export  SCREE_ZDR=1":     "line 4: not a scree setting name",
		"=value":                  "line 4: not a scree setting name",
		"SCREE_ZDR=1\x00x":        "line 4: contains a NUL byte",
	} {
		_, err := parseSettings("env", []byte(testSettings+line+"\n"))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", line, err, want)
		}
	}
	// A key pasted without a name is never echoed.
	_, err := parseSettings("env", []byte(testSecret+"\n"))
	if err == nil || strings.Contains(err.Error(), testSecret) {
		t.Errorf("bare key: %v", err)
	}
	if !isSetting("SCREE_A1_b") || !isSetting("SCREE_1") || !isSetting("SCREE__X") || isSetting("SCREE_A-B") {
		t.Error("isSetting")
	}
}

// TestLaunchUnsupportedMessage: on Windows loadSettings returns
// errLaunchUnsupported before reading anything; it must say so.
func TestLaunchUnsupportedMessage(t *testing.T) {
	if !strings.Contains(errLaunchUnsupported.Error(), "not supported on Windows") {
		t.Error(errLaunchUnsupported)
	}
}

// dockerRun runs scree mcp -docker in cwd with CLAUDE_PROJECT_DIR=pd and
// returns the argv it would exec.
func dockerRun(t *testing.T, l *launched, cwd, pd string) []string {
	t.Helper()
	t.Chdir(cwd)
	t.Setenv("CLAUDE_PROJECT_DIR", pd)
	l.called = false
	if err := runMCP([]string{"-docker"}); err != nil {
		t.Fatalf("cwd %s, pd %s: %v", cwd, pd, err)
	}
	return l.argv
}

func (l *launched) dockerRefused(t *testing.T, cwd, pd, want string) {
	t.Helper()
	t.Chdir(cwd)
	t.Setenv("CLAUDE_PROJECT_DIR", pd)
	l.called = false
	l.wantRefused(t, runMCP([]string{"-docker"}), want)
}

// TestDockerArgv: the exact docker command line and environment. Settings
// travel by name, never as values in argv.
func TestDockerArgv(t *testing.T) {
	l := launchSetup(t, testSettings+"SCREE_ZDR=0\nSCREE_FIRST_SYNC_MB=5\nSCREE_DB=/x\nHTTPS_PROXY=http://proxy.internal:3128\nDOCKER_HOST=unix:///run/user/1000/docker.sock\n", 0o600)
	for k, v := range hostile(l) {
		t.Setenv(k, v)
	}
	repo := filepath.Join(l.home, "repo")
	mkdir(t, filepath.Join(repo, ".git"))
	mkdir(t, filepath.Join(repo, "sub"))
	argv := dockerRun(t, l, filepath.Join(repo, "sub"), filepath.Join(repo, "sub"))
	docker := filepath.Join(l.engineDir, "docker")
	want := []string{
		docker, "run", "-i", "--rm", "--pull=never",
		"-e", "OPENROUTER_API_KEY", "-e", "SCREE_MODEL", "-e", "SCREE_ZDR", "-e", "SCREE_FIRST_SYNC_MB",
		"-v", repo + ":" + repo + ":ro", "-v", "scree-data:/data",
		"-w", repo, "-e", "CLAUDE_PROJECT_DIR=" + filepath.Join(repo, "sub"),
		"scree:latest", "serve",
	}
	if l.argv0 != docker || !slices.Equal(argv, want) {
		t.Errorf("docker %s argv\n%q\nwant\n%q", l.argv0, argv, want)
	}
	for _, a := range argv {
		if strings.Contains(a, testSecret) {
			t.Errorf("key in argv: %q", a)
		}
	}
	wantEnv := []string{
		"HOME=" + l.home,
		"PATH=" + filepath.Join(l.home, "nonexistent") + ":" + l.engineDir,
		"DOCKER_HOST=unix:///run/user/1000/docker.sock",
		"HTTPS_PROXY=http://proxy.internal:3128",
		"OPENROUTER_API_KEY=" + testSecret,
		"SCREE_DB=/x",
		"SCREE_FIRST_SYNC_MB=5",
		"SCREE_MODEL=openai/text-embedding-3-small",
		"SCREE_ZDR=0",
	}
	if !slices.Equal(l.env, wantEnv) {
		t.Errorf("docker env\n%q\nwant\n%q", l.env, wantEnv)
	}
}

func TestDockerDefaultEngineDirs(t *testing.T) {
	want := []string{"/usr/local/bin", "/usr/bin", "/bin", "/opt/homebrew/bin"}
	if !slices.Equal(engineDirs, want) {
		t.Errorf("engineDirs %q", engineDirs)
	}
}

func TestDockerSettings(t *testing.T) {
	l := launchSetup(t, testSettings+"SCREE_DOCKER=podman\nSCREE_IMAGE=ghcr.io/jrapakko/scree:0.1.0\nSCREE_DATA_VOLUME=my_vol.1-x\nSCREE_MOUNT_OPTS=ro,Z\n", 0o600)
	argv := dockerRun(t, l, l.proj, l.proj)
	if argv[0] != filepath.Join(l.engineDir, "podman") ||
		!slices.Contains(argv, l.proj+":"+l.proj+":ro,Z") || !slices.Contains(argv, "my_vol.1-x:/data") ||
		argv[len(argv)-2] != "ghcr.io/jrapakko/scree:0.1.0" || slices.Contains(argv, "-notes") {
		t.Errorf("argv %q", argv)
	}
	for _, opts := range []string{"ro", "ro,z", "ro,Z", ""} {
		l := launchSetup(t, testSettings+"SCREE_MOUNT_OPTS='"+opts+"'\n", 0o600)
		argv := dockerRun(t, l, l.proj, l.proj)
		if !slices.Contains(argv, l.proj+":"+l.proj+":"+cmpOrRO(opts)) {
			t.Errorf("%q: argv %q", opts, argv)
		}
	}
	for line, want := range map[string]string{
		"SCREE_MOUNT_OPTS=rw":            "SCREE_MOUNT_OPTS must be",
		"SCREE_MOUNT_OPTS=ro,rw":         "SCREE_MOUNT_OPTS must be",
		"SCREE_MOUNT_OPTS=z":             "SCREE_MOUNT_OPTS must be",
		"SCREE_MOUNT_OPTS='ro z'":        "SCREE_MOUNT_OPTS must be",
		"SCREE_DOCKER=/tmp/evil":         "must be docker or podman",
		"SCREE_DOCKER=scree":             "must be docker or podman",
		"SCREE_DATA_VOLUME=/home/u":      "volume name, not a path",
		"SCREE_DATA_VOLUME=../x":         "volume name, not a path",
		"SCREE_DATA_VOLUME=.x":           "volume name, not a path",
		"SCREE_DATA_VOLUME='-v /:/x'":    "volume name, not a path",
		"SCREE_IMAGE=--privileged":       "SCREE_IMAGE must be an image name",
		"SCREE_IMAGE='scree serve -x'":   "SCREE_IMAGE must be an image name",
		"DOCKER_HOST=unix:///x\nSCREE_=": "line 5",
	} {
		l := launchSetup(t, testSettings+line+"\n", 0o600)
		l.dockerRefused(t, l.proj, l.proj, want)
	}
}

func cmpOrRO(s string) string {
	if s == "" {
		return "ro"
	}
	return s
}

// TestDockerEngineNotFromPATH: docker and podman come only from the fixed
// folders, never from the inherited PATH.
func TestDockerEngineNotFromPATH(t *testing.T) {
	l := launchSetup(t, testSettings, 0o600)
	t.Setenv("PATH", l.plantedPATH)
	replace(t, &engineDirs, []string{filepath.Join(l.home, "nonexistent")})
	l.dockerRefused(t, l.proj, l.proj, "docker not found in")
	// Not executable is not found either.
	replace(t, &engineDirs, []string{l.engineDir})
	os.Chmod(filepath.Join(l.engineDir, "docker"), 0o644)
	l.dockerRefused(t, l.proj, l.proj, "docker not found in")
}

func TestDockerProjectDir(t *testing.T) {
	l := launchSetup(t, testSettings, 0o600)
	l.dockerRefused(t, l.proj, "proj", "absolute path")
	l.dockerRefused(t, l.proj, filepath.Join(l.home, "gone"), "cannot open CLAUDE_PROJECT_DIR")
	f := filepath.Join(l.home, "file")
	os.WriteFile(f, nil, 0o600)
	l.dockerRefused(t, l.proj, f, "cannot open CLAUDE_PROJECT_DIR")

	// A symlinked CLAUDE_PROJECT_DIR is resolved, and so is the mount.
	real := filepath.Join(l.home, "real")
	link := filepath.Join(l.home, "link")
	mkdir(t, real)
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	argv := dockerRun(t, l, link, link)
	if !slices.Contains(argv, real+":"+real+":ro") || argv[slices.Index(argv, "-w")+1] != real || !slices.Contains(argv, "CLAUDE_PROJECT_DIR="+real) {
		t.Errorf("argv %q", argv)
	}
}

// TestDockerFollowsWorkingDirectory: the mount is the git root of the
// working directory; a CLAUDE_PROJECT_DIR in another repo, or at home, is
// refused rather than followed.
func TestDockerFollowsWorkingDirectory(t *testing.T) {
	l := launchSetup(t, testSettings, 0o600)
	repo := filepath.Join(l.home, "repo")
	other := filepath.Join(l.home, "other")
	for _, d := range []string{filepath.Join(repo, ".git"), filepath.Join(repo, "sub"), filepath.Join(other, ".git")} {
		mkdir(t, d)
	}
	argv := dockerRun(t, l, filepath.Join(repo, "sub"), repo)
	if !slices.Contains(argv, repo+":"+repo+":ro") || !slices.Contains(argv, "CLAUDE_PROJECT_DIR="+repo) {
		t.Errorf("argv %q", argv)
	}
	for _, pd := range []string{other, l.home, "/"} {
		l.dockerRefused(t, repo, pd, "is not in the repo of the working directory")
	}
}

func TestDockerRefusesColon(t *testing.T) {
	l := launchSetup(t, testSettings, 0o600)
	dir := filepath.Join(l.home, "a:b")
	mkdir(t, dir)
	l.dockerRefused(t, dir, dir, "contains ':'")
}

func TestDockerRefusesHome(t *testing.T) {
	l := launchSetup(t, testSettings, 0o600)
	// A dotfiles repo at home lifts every project to it.
	mkdir(t, filepath.Join(l.home, ".git"))
	for _, d := range []string{l.home, l.proj} {
		l.dockerRefused(t, d, d, "refusing to mount")
	}
}

func TestDockerRefusesHomeAncestors(t *testing.T) {
	l := launchSetup(t, "", 0)
	home := filepath.Join(l.home, "home", "u")
	mkdir(t, filepath.Join(home, "code", "r"))
	mkdir(t, filepath.Join(home, ".config", "scree"))
	if err := os.WriteFile(settingsPath(home), []byte(testSettings), 0o600); err != nil {
		t.Fatal(err)
	}
	replace(t, &settingsHome, func() (string, error) { return home, nil })
	for _, d := range []string{home, filepath.Join(l.home, "home"), l.home, "/"} {
		l.dockerRefused(t, d, d, "refusing to mount")
	}
	// A symlinked home is compared resolved.
	link := filepath.Join(l.home, "homelink")
	if err := os.Symlink(home, link); err != nil {
		t.Fatal(err)
	}
	replace(t, &settingsHome, func() (string, error) { return link, nil })
	l.dockerRefused(t, filepath.Join(l.home, "home"), filepath.Join(l.home, "home"), "refusing to mount")
	r := filepath.Join(home, "code", "r")
	if argv := dockerRun(t, l, r, r); !slices.Contains(argv, r+":"+r+":ro") {
		t.Errorf("argv %q", argv)
	}
}

func TestDockerNoLiftThroughSticky(t *testing.T) {
	l := launchSetup(t, testSettings, 0o600)
	sticky := filepath.Join(l.home, "sticky")
	p := filepath.Join(sticky, "p")
	mkdir(t, filepath.Join(sticky, ".git"))
	mkdir(t, p)
	if err := os.Chmod(sticky, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if argv := dockerRun(t, l, p, p); !slices.Contains(argv, p+":"+p+":ro") {
		t.Errorf("argv %q", argv)
	}
	// A .git below the sticky folder still lifts.
	mkdir(t, filepath.Join(p, ".git"))
	sub := filepath.Join(p, "sub")
	mkdir(t, sub)
	if argv := dockerRun(t, l, sub, sub); !slices.Contains(argv, p+":"+p+":ro") {
		t.Errorf("argv %q", argv)
	}
}

// TestDockerRefusesSymlinkGit: a .git that is a symlink is untrusted, and
// under FindGitTop's rules that refuses the folder rather than mounting it
// without its ignore rules.
func TestDockerRefusesSymlinkGit(t *testing.T) {
	l := launchSetup(t, testSettings, 0o600)
	repo := filepath.Join(l.home, "repo")
	sub := filepath.Join(repo, "sub")
	gitdir := filepath.Join(l.home, "elsewhere.git")
	mkdir(t, sub)
	mkdir(t, gitdir)
	if err := os.Symlink(gitdir, filepath.Join(repo, ".git")); err != nil {
		t.Fatal(err)
	}
	l.dockerRefused(t, sub, sub, "owned by another user (or is a symlink)")
}

func TestDockerNoLiftToForeignGit(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root to chown .git to another user")
	}
	l := launchSetup(t, testSettings, 0o600)
	repo := filepath.Join(l.home, "repo")
	sub := filepath.Join(repo, "sub")
	mkdir(t, sub)
	mkdir(t, filepath.Join(repo, ".git"))
	if err := os.Chown(filepath.Join(repo, ".git"), 65534, 65534); err != nil {
		t.Fatal(err)
	}
	l.dockerRefused(t, sub, sub, "owned by another user")
}

// TestExecErrorReported: a failed exec is an error, not a silent return.
func TestExecErrorReported(t *testing.T) {
	l := launchSetup(t, testSettings, 0o600)
	replace(t, &execProgram, func(string, []string, []string) error { return errors.New("boom") })
	l.wantRefused(t, runMCP(nil), "exec "+testExe+": boom")
}
