package main

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
)

// scree mcp and scree run load the settings file as data and re-execute
// scree (or docker) with an environment built from nothing but that file.
// They replace the old shell wrappers: scree is a static binary with no
// shell and, on Linux, no dynamic loader, so nothing a repo's .mcp.json puts
// in the environment (LD_PRELOAD, SHELLOPTS and PS4, BASH_FUNC_*) acts on
// it before main. The Go runtime does read GODEBUG, GOTRACEBACK, GOMAXPROCS
// and GOGC before main, which is why the real work happens in the re-exec'd
// process, whose environment holds none of them.

// cleanPath is PATH for the re-executed scree.
const cleanPath = "/usr/local/bin:/usr/bin:/bin"

// maxSettingsFile bounds the settings file.
const maxSettingsFile = 64 << 10

// Hooks that tests replace.
var (
	// settingsHome returns the home directory the settings file lives in:
	// the password database's, never $HOME.
	settingsHome = passwdHome
	// launchUID is the uid that must own the settings file.
	launchUID = os.Getuid
	// execProgram replaces the process.
	execProgram = syscall.Exec
	// selfExecutable is this binary, symlinks resolved.
	selfExecutable = func() (string, error) {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		return filepath.EvalSymlinks(exe)
	}
	// engineDirs are the only folders docker or podman is taken from,
	// and, joined, the docker child's PATH.
	engineDirs = []string{"/usr/local/bin", "/usr/bin", "/bin", "/opt/homebrew/bin"}
)

// errLaunchUnsupported: Windows has no exec(2) to replace the process with
// one that has a clean environment, and no owner or mode bits for the
// settings file.
var errLaunchUnsupported = errors.New("scree mcp and scree run are not supported on Windows; " +
	"set the variables in the environment and run scree serve -notes <dir> or another command directly")

// settingsPath is the settings file under home.
func settingsPath(home string) string {
	return filepath.Join(home, ".config", "scree", "env")
}

// dockerOnly settings reach the docker or podman CLI, never scree.
var dockerOnly = []string{"DOCKER_HOST", "DOCKER_CONTEXT"}

// isSetting reports whether name may appear in the settings file: SCREE_*
// (SCREE_ then [A-Z0-9_], then [A-Za-z0-9_]*), OPENROUTER_API_KEY, the proxy
// variables, DOCKER_HOST and DOCKER_CONTEXT.
func isSetting(name string) bool {
	switch name {
	case "OPENROUTER_API_KEY", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy":
		return true
	}
	if slices.Contains(dockerOnly, name) {
		return true
	}
	rest, ok := strings.CutPrefix(name, "SCREE_")
	if !ok || rest == "" {
		return false
	}
	if c := rest[0]; !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
		return false
	}
	return isIdent(rest)
}

// isIdent reports whether s is a non-empty run of [A-Za-z0-9_].
func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

// parseSettings reads the settings file as data, never as shell: each line
// is NAME=value, with an optional "export " and optional matching quotes
// around the value. Leading whitespace and a trailing CR are dropped, and
// blank lines and lines starting with # are skipped. Nothing in a value is
// expanded or run. Only names isSetting accepts are allowed; a later line
// overrides an earlier one. Errors name the line, never its value.
func parseSettings(path string, data []byte) (map[string]string, error) {
	s := map[string]string{}
	for i, line := range strings.Split(string(data), "\n") {
		n := i + 1
		line = strings.TrimSuffix(line, "\r")
		line = strings.TrimLeft(line, " \t\v\f\r")
		if line == "" || line[0] == '#' {
			continue
		}
		if strings.ContainsRune(line, 0) {
			return nil, fmt.Errorf("%s line %d: contains a NUL byte", path, n)
		}
		line = strings.TrimPrefix(line, "export ")
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s line %d: expected NAME=value", path, n)
		}
		if !isSetting(name) {
			if isIdent(name) {
				return nil, fmt.Errorf("%s line %d: %s is not a scree setting", path, n, name)
			}
			return nil, fmt.Errorf("%s line %d: not a scree setting name", path, n)
		}
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		s[name] = value
	}
	return s, nil
}

// readSettingsFile reads path, which must be a regular file (not a
// symlink), owned by launchUID, with no group or other permission bits, and
// at most maxSettingsFile bytes.
func readSettingsFile(path string) ([]byte, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s not found; create it with OPENROUTER_API_KEY, then: chmod 600 %s", path, path)
	}
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is a symlink or not a regular file; not reading it", path)
	}
	if err := checkSettingsOwner(fi); err != nil {
		return nil, fmt.Errorf("%s %w", path, err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("%s is open to group or others (mode %o); fix with: chmod 600 %s", path, perm, path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|noFollow, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if now, err := f.Stat(); err != nil || !os.SameFile(fi, now) {
		return nil, fmt.Errorf("%s changed while being read; not reading it", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSettingsFile+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSettingsFile {
		return nil, fmt.Errorf("%s is larger than 64 KB", path)
	}
	return data, nil
}

// loadSettings finds the passwd home and reads the settings file in it.
func loadSettings() (home string, s map[string]string, err error) {
	if runtime.GOOS == "windows" {
		return "", nil, errLaunchUnsupported
	}
	if home, err = settingsHome(); err != nil {
		return "", nil, err
	}
	p := settingsPath(home)
	data, err := readSettingsFile(p)
	if err != nil {
		return "", nil, err
	}
	if s, err = parseSettings(p, data); err != nil {
		return "", nil, err
	}
	return home, s, nil
}

// childEnv is HOME, PATH, extra, then the settings sorted by name, without
// the docker-only ones unless withDocker.
func childEnv(home, path string, s map[string]string, withDocker bool, extra ...string) []string {
	env := append([]string{"HOME=" + home, "PATH=" + path}, extra...)
	names := make([]string, 0, len(s))
	for n := range s {
		if withDocker || !slices.Contains(dockerOnly, n) {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	for _, n := range names {
		env = append(env, n+"="+s[n])
	}
	return env
}

// runMCP is `scree mcp [-docker]`, what a repo's .mcp.json runs. It takes
// no other arguments, since those could come from the repo, and runs
// `scree serve` (or, with -docker, `docker run ... serve`) with the
// settings file as its only configuration.
func runMCP(args []string) error {
	docker := false
	switch {
	case len(args) == 0:
	case len(args) == 1 && args[0] == "-docker":
		docker = true
	default:
		return errors.New("mcp takes no arguments but -docker; put settings in ~/.config/scree/env " +
			"(arguments could come from a repo's .mcp.json)")
	}
	home, s, err := loadSettings()
	if err != nil {
		return err
	}
	pd := os.Getenv("CLAUDE_PROJECT_DIR")
	if pd == "" {
		return errors.New("mcp: CLAUDE_PROJECT_DIR is not set; scree mcp is for Claude Code " +
			"(from a terminal, run scree run serve -notes <dir>)")
	}
	if docker {
		return execDocker(home, s, pd)
	}
	return reexec(home, s, pd, false, "serve")
}

// runRun is `scree run <command> [args]`: any other command, with its
// arguments, run the way scree mcp runs serve. NO_COLOR set or TERM=dumb
// reaches the child as NO_COLOR=1, so search's text view leaves out links;
// neither raw value is passed.
func runRun(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: scree run <command> [args]")
	}
	if args[0] == "mcp" || args[0] == "run" {
		return fmt.Errorf("run: cannot run %s", args[0])
	}
	home, s, err := loadSettings()
	if err != nil {
		return err
	}
	noColor := os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb"
	return reexec(home, s, os.Getenv("CLAUDE_PROJECT_DIR"), noColor, args...)
}

// reexec replaces the process with this binary running args, in an
// environment of HOME (the passwd home), cleanPath, CLAUDE_PROJECT_DIR when
// set, NO_COLOR=1 with noColor, and the settings.
func reexec(home string, s map[string]string, pd string, noColor bool, args ...string) error {
	exe, err := selfExecutable()
	if err != nil {
		return fmt.Errorf("cannot find the scree binary: %w", err)
	}
	var extra []string
	if pd != "" {
		extra = append(extra, "CLAUDE_PROJECT_DIR="+pd)
	}
	if noColor {
		extra = append(extra, "NO_COLOR=1")
	}
	env := childEnv(home, cleanPath, s, false, extra...)
	if err := execProgram(exe, append([]string{exe}, args...), env); err != nil {
		return fmt.Errorf("exec %s: %w", exe, err)
	}
	return nil
}

// dockerPassed are the settings handed to the container, by name and only
// when the settings file sets them, so unset ones keep the image defaults
// and no value appears in argv.
var dockerPassed = []string{
	"OPENROUTER_API_KEY", "SCREE_MODEL", "SCREE_DIMENSIONS", "SCREE_ZDR",
	"SCREE_FIRST_SYNC_MB", "SCREE_MAX_FILE_BYTES", "SCREE_EMBED_URL", "SCREE_HTTP_TOKEN",
}

// execDocker replaces the process with `docker run -i --rm --pull=never`
// (or podman) running serve on the git root of the working directory,
// mounted read-only at its own host path, so each repo gets its own index
// in the data volume.
func execDocker(home string, s map[string]string, pd string) error {
	argv, err := dockerArgv(home, s, pd)
	if err != nil {
		return err
	}
	env := childEnv(home, strings.Join(engineDirs, string(os.PathListSeparator)), s, true)
	if err := execProgram(argv[0], argv, env); err != nil {
		return fmt.Errorf("exec %s: %w", argv[0], err)
	}
	return nil
}

// dockerArgv builds the engine's argv, argv[0] being its absolute path.
func dockerArgv(home string, s map[string]string, pd string) ([]string, error) {
	if !filepath.IsAbs(pd) {
		return nil, errors.New("mcp -docker: CLAUDE_PROJECT_DIR must be an absolute path")
	}
	pdRes, err := filepath.EvalSymlinks(pd)
	if err == nil {
		var fi os.FileInfo
		if fi, err = os.Stat(pdRes); err == nil && !fi.IsDir() {
			err = errors.New("not a folder")
		}
	}
	if err != nil {
		return nil, fmt.Errorf("mcp -docker: cannot open CLAUDE_PROJECT_DIR %s", pd)
	}
	wd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("mcp -docker: cannot resolve the working directory: %w", err)
	}
	// The folder is the git root of the working directory, which Claude Code
	// sets to the project; a .mcp.json can set the environment but not the
	// working directory. CLAUDE_PROJECT_DIR must lead to the same folder:
	// only that work tree is mounted.
	dir, _, err := liftGitRoot(wd)
	if err != nil {
		return nil, fmt.Errorf("mcp -docker: %w", err)
	}
	pdRoot, _, err := liftGitRoot(pdRes)
	if err != nil {
		return nil, fmt.Errorf("mcp -docker: CLAUDE_PROJECT_DIR: %w", err)
	}
	if pdRoot != dir {
		return nil, fmt.Errorf("mcp -docker: CLAUDE_PROJECT_DIR %s is not in the repo of the working directory %s; refusing", pdRes, dir)
	}
	if strings.Contains(dir, ":") {
		return nil, fmt.Errorf("mcp -docker: cannot mount %s: the path contains ':'", dir)
	}
	if homeOrAbove(dir, home) {
		return nil, fmt.Errorf("mcp -docker: refusing to mount %s (your home directory, a folder above it, or /)", dir)
	}
	opts := cmp.Or(s["SCREE_MOUNT_OPTS"], "ro")
	if opts != "ro" && opts != "ro,z" && opts != "ro,Z" {
		return nil, errors.New("mcp -docker: SCREE_MOUNT_OPTS must be ro, ro,z or ro,Z")
	}
	vol := cmp.Or(s["SCREE_DATA_VOLUME"], "scree-data")
	if !validVolume(vol) {
		return nil, errors.New("mcp -docker: SCREE_DATA_VOLUME must be a volume name, not a path")
	}
	image := cmp.Or(s["SCREE_IMAGE"], "scree:latest")
	if strings.HasPrefix(image, "-") || strings.ContainsFunc(image, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return nil, errors.New("mcp -docker: SCREE_IMAGE must be an image name")
	}
	engine := cmp.Or(s["SCREE_DOCKER"], "docker")
	if engine != "docker" && engine != "podman" {
		return nil, errors.New("mcp -docker: SCREE_DOCKER must be docker or podman")
	}
	bin, err := findEngine(engine)
	if err != nil {
		return nil, err
	}
	// --pull=never: a missing local image must never be pulled from a
	// registry, since the container receives the API key.
	argv := []string{bin, "run", "-i", "--rm", "--pull=never"}
	for _, v := range dockerPassed {
		if _, ok := s[v]; ok {
			argv = append(argv, "-e", v)
		}
	}
	// serve runs in the mounted folder, where it applies the same
	// working-directory rule and the opt-in check.
	return append(argv,
		"-v", dir+":"+dir+":"+opts, "-v", vol+":/data",
		"-w", dir, "-e", "CLAUDE_PROJECT_DIR="+pdRes,
		image, "serve"), nil
}

// validVolume reports whether v is a volume name: [A-Za-z0-9] then
// [A-Za-z0-9_.-]*.
func validVolume(v string) bool {
	if v == "" {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		alnum := c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
		if !alnum && (i == 0 || c != '_' && c != '.' && c != '-') {
			return false
		}
	}
	return true
}

// findEngine returns the absolute path of docker or podman in engineDirs,
// never through PATH.
func findEngine(name string) (string, error) {
	for _, d := range engineDirs {
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("mcp -docker: %s not found in %s", name, strings.Join(engineDirs, ", "))
}
