// Package contrib holds files that ship beside the binary. The test checks
// the MCP wrapper scripts with a fake scree and a fake docker.
package contrib

import (
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const secret = "sk-or-v1-test-secret-0123456789"

// fake is a stand-in for scree or docker. It records its arguments and the
// variables the wrapper should pass, one per line, in $FAKE_OUT, and prints
// nothing.
const fake = `#!/bin/sh
{
	for a in "$@"; do printf 'arg=%s\n' "$a"; done
	for v in OPENROUTER_API_KEY SCREE_DB SCREE_MODEL SCREE_DIMENSIONS SCREE_EMBED_URL CLAUDE_PROJECT_DIR; do
		if eval "[ -n \"\${$v+x}\" ]"; then eval "printf 'env=%s=%s\n' $v \"\$$v\""; fi
	done
} > "$FAKE_OUT"
`

type env struct {
	dir, bin, envFile, out string
	project                string // CLAUDE_PROJECT_DIR for run: <dir>/proj, with no .git above it
}

// setup makes a temp dir holding the env file and, in bin/, copies of the
// wrapper scripts next to fakes named scree and docker. The wrappers only run
// programs from their own folder or the system PATH, so this is how the
// tests reach the fakes.
func setup(t *testing.T, content string, mode os.FileMode) env {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	dir := t.TempDir()
	e := env{dir: dir, bin: filepath.Join(dir, "bin"), envFile: filepath.Join(dir, ".config", "scree", "env"), out: filepath.Join(dir, "out"),
		project: filepath.Join(dir, "proj")}
	if err := os.Mkdir(e.project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(e.envFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if content != "" {
		if err := os.WriteFile(e.envFile, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(e.envFile, mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(e.bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, s := range scripts {
		b, err := os.ReadFile(s)
		if err != nil {
			t.Fatal(err)
		}
		// The copies take the home directory from $TEST_PW_HOME instead of
		// the password database, so they never read the real env file.
		block, err := passwdBlock(string(b))
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		_ = block
		b = []byte(strings.Replace(string(b), "eval \"pwhome=~$user\"\n", "pwhome=$TEST_PW_HOME\n", 1))
		if err := os.WriteFile(filepath.Join(e.bin, s), b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"scree", "docker"} {
		if err := os.WriteFile(filepath.Join(e.bin, name), []byte(fake), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

// run executes a wrapper with a minimal environment plus extra, and returns
// its combined output, the lines the fake recorded, and the exit error. It
// runs in the CLAUDE_PROJECT_DIR it passes, as Claude Code does, when that
// is an existing folder, else in the temp dir; a "CWD=dir" entry in extra
// sets the working directory instead and is not passed on.
func (e env) run(t *testing.T, script string, extra []string, args ...string) (string, []string, error) {
	t.Helper()
	cmd := exec.Command(filepath.Join(e.bin, script), args...)
	env := []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + e.dir,
		"TEST_PW_HOME=" + e.dir,
		"FAKE_OUT=" + e.out,
		"CLAUDE_PROJECT_DIR=" + e.project,
	}
	cwd := ""
	for _, x := range extra {
		if d, ok := strings.CutPrefix(x, "CWD="); ok {
			cwd = d
			continue
		}
		env = append(env, x)
	}
	if cwd == "" {
		cwd = e.dir
		for _, x := range env {
			if d, ok := strings.CutPrefix(x, "CLAUDE_PROJECT_DIR="); ok {
				if fi, err := os.Stat(d); err == nil && fi.IsDir() && filepath.IsAbs(d) {
					cwd = d
				} else {
					cwd = e.dir
				}
			}
		}
	}
	cmd.Dir = cwd
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if strings.Contains(string(out), secret) {
		t.Errorf("%s printed the key: %q", script, out)
	}
	var lines []string
	if b, rerr := os.ReadFile(e.out); rerr == nil {
		lines = strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	return string(out), lines, err
}

const envFile = "# scree settings\nOPENROUTER_API_KEY=" + secret + "\nSCREE_MODEL=openai/text-embedding-3-small\n"

var scripts = []string{"scree-mcp", "scree-mcp-docker", "scree-run"}

func TestScriptsExecutable(t *testing.T) {
	for _, s := range scripts {
		fi, err := os.Stat(s)
		if err != nil || fi.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s: %v, mode %v", s, err, fi.Mode())
		}
	}
}

func TestWrapperRefusesOpenEnvFile(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o640, 0o602} {
		e := setup(t, envFile, mode)
		for _, s := range scripts {
			out, lines, err := e.run(t, s, nil)
			if err == nil || lines != nil || !strings.Contains(out, "chmod 600 "+e.envFile) {
				t.Errorf("%s with mode %o: err %v, child ran %v, output %q", s, mode, err, lines != nil, out)
			}
		}
	}
}

func TestWrapperRefusesMissingEnvFile(t *testing.T) {
	e := setup(t, "", 0)
	for _, s := range scripts {
		out, lines, err := e.run(t, s, nil)
		if err == nil || lines != nil || !strings.Contains(out, "not found; create it with OPENROUTER_API_KEY, then") ||
			strings.Contains(out, "SCREE_NOTES") {
			t.Errorf("%s: err %v, child ran %v, output %q", s, err, lines != nil, out)
		}
	}
}

func TestScreeMCPPassesEnv(t *testing.T) {
	e := setup(t, envFile, 0o600)
	out, lines, err := e.run(t, "scree-mcp", nil)
	if err != nil || out != "" {
		t.Fatalf("err %v, output %q", err, out)
	}
	want := []string{
		"arg=serve",
		"env=OPENROUTER_API_KEY=" + secret,
		"env=SCREE_MODEL=openai/text-embedding-3-small",
		"env=CLAUDE_PROJECT_DIR=" + e.project,
	}
	if !slices.Equal(lines, want) {
		t.Errorf("child saw\n%q\nwant\n%q", lines, want)
	}
}

// dockerArgs runs scree-mcp-docker with CLAUDE_PROJECT_DIR=project and
// returns the arguments docker got.
func dockerArgs(t *testing.T, e env, project string, extra ...string) ([]string, []string) {
	t.Helper()
	// Later entries override earlier ones in exec.Cmd.Env.
	out, lines, err := e.run(t, "scree-mcp-docker", append([]string{"CLAUDE_PROJECT_DIR=" + project}, extra...))
	if err != nil || out != "" {
		t.Fatalf("err %v, output %q", err, out)
	}
	var args []string
	for _, l := range lines {
		if a, ok := strings.CutPrefix(l, "arg="); ok {
			args = append(args, a)
		}
	}
	return args, lines
}

func TestScreeMCPDockerArgs(t *testing.T) {
	e := setup(t, envFile+"SCREE_ZDR=0\nSCREE_FIRST_SYNC_MB=5\n", 0o600)
	repo := filepath.Join(e.dir, "repo")
	for _, d := range []string{".git", "sub"} {
		if err := os.MkdirAll(filepath.Join(repo, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	args, lines := dockerArgs(t, e, filepath.Join(repo, "sub"))
	want := []string{
		"run", "-i", "--rm", "--pull=never",
		"-e", "OPENROUTER_API_KEY", "-e", "SCREE_MODEL", "-e", "SCREE_ZDR", "-e", "SCREE_FIRST_SYNC_MB",
		"-v", repo + ":" + repo + ":ro", "-v", "scree-data:/data",
		"-w", repo, "-e", "CLAUDE_PROJECT_DIR=" + filepath.Join(repo, "sub"),
		"scree:latest", "serve",
	}
	if !slices.Equal(args, want) {
		t.Errorf("docker args\n%q\nwant\n%q", args, want)
	}
	// The key travels in the environment, never in argv.
	if !slices.Contains(lines, "env=OPENROUTER_API_KEY="+secret) {
		t.Errorf("key did not reach docker's environment: %q", lines)
	}
	for _, a := range args {
		if strings.Contains(a, secret) {
			t.Errorf("key in argv: %q", a)
		}
	}
}

func TestScreeMCPDockerNoGit(t *testing.T) {
	e := setup(t, envFile, 0o600)
	args, _ := dockerArgs(t, e, e.project)
	if !slices.Contains(args, e.project+":"+e.project+":ro") || !slices.Contains(args, "-w") ||
		args[slices.Index(args, "-w")+1] != e.project || slices.Contains(args, "-notes") {
		t.Errorf("docker args %q", args)
	}
}

func TestScreeMCPDockerMountOpts(t *testing.T) {
	e := setup(t, envFile+"SCREE_MOUNT_OPTS=ro,z\n", 0o600)
	args, _ := dockerArgs(t, e, e.project)
	if !slices.Contains(args, e.project+":"+e.project+":ro,z") {
		t.Errorf("docker args %q", args)
	}
}

func TestScreeMCPDockerNeedsProjectDir(t *testing.T) {
	e := setup(t, "OPENROUTER_API_KEY="+secret+"\n", 0o600)
	out, lines, err := e.run(t, "scree-mcp-docker", []string{"CLAUDE_PROJECT_DIR="})
	if err == nil || lines != nil || !strings.Contains(out, "CLAUDE_PROJECT_DIR is not set") {
		t.Errorf("err %v, child ran %v, output %q", err, lines != nil, out)
	}
}

func TestScreeMCPDockerRefusesColon(t *testing.T) {
	e := setup(t, envFile, 0o600)
	dir := filepath.Join(e.dir, "a:b")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	out, lines, err := e.run(t, "scree-mcp-docker", []string{"CLAUDE_PROJECT_DIR=" + dir})
	if err == nil || lines != nil || !strings.Contains(out, "contains ':'") {
		t.Errorf("err %v, child ran %v, output %q", err, lines != nil, out)
	}
}

func TestScreeMCPDockerRefusesHome(t *testing.T) {
	e := setup(t, envFile, 0o600)
	// HOME is e.dir; a dotfiles repo there lifts every project to it.
	if err := os.Mkdir(filepath.Join(e.dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{e.dir, e.project} {
		out, lines, err := e.run(t, "scree-mcp-docker", []string{"CLAUDE_PROJECT_DIR=" + dir})
		if err == nil || lines != nil || !strings.Contains(out, "refusing to mount") {
			t.Errorf("%s: err %v, child ran %v, output %q", dir, err, lines != nil, out)
		}
	}
}

// The env file is read as data. A value with spaces, $(...) or backquotes
// must never run anything, even when the cwd holds a matching executable.
func TestEnvFileIsData(t *testing.T) {
	notes := "/Users/u/Library/Mobile Documents/iCloud~md~obsidian/Documents/Vault"
	content := "SCREE_DB=" + notes + "\n" +
		"OPENROUTER_API_KEY=" + secret + "\n" +
		"SCREE_MODEL=$(touch pwned-subst)`touch pwned-tick`\n"
	for _, s := range scripts {
		e := setup(t, content, 0o600)
		planted := filepath.Join(e.dir, "Documents", "iCloud~md~obsidian", "Documents", "Vault")
		if err := os.MkdirAll(filepath.Dir(planted), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(planted, []byte("#!/bin/sh\ntouch \"$HOME/pwned-planted\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		out, lines, err := e.run(t, s, nil)
		if err != nil {
			t.Fatalf("%s: err %v, output %q", s, err, out)
		}
		for _, f := range []string{"pwned-subst", "pwned-tick", "pwned-planted"} {
			for _, d := range []string{e.dir, e.project} {
				if _, err := os.Stat(filepath.Join(d, f)); err == nil {
					t.Errorf("%s: %s ran", s, f)
				}
			}
		}
		if !slices.Contains(lines, "env=SCREE_DB="+notes) ||
			!slices.Contains(lines, "env=SCREE_MODEL=$(touch pwned-subst)`touch pwned-tick`") {
			t.Errorf("%s: values not kept literally: %q", s, lines)
		}
	}
}

func TestEnvFileSyntax(t *testing.T) {
	content := "  # indented comment\n\n" +
		"export OPENROUTER_API_KEY=\"" + secret + "\"\r\n" +
		"SCREE_DB='/home/u/my notes'\n" +
		"SCREE_MODEL=\"it's fine\"\n" +
		"SCREE_DIMENSIONS=256" // no final newline
	e := setup(t, content, 0o600)
	_, lines, err := e.run(t, "scree-mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"env=OPENROUTER_API_KEY=" + secret,
		"env=SCREE_DB=/home/u/my notes",
		"env=SCREE_MODEL=it's fine",
		"env=SCREE_DIMENSIONS=256",
	} {
		if !slices.Contains(lines, want) {
			t.Errorf("missing %q in %q", want, lines)
		}
	}
}

func TestEnvFileRefusesOtherNames(t *testing.T) {
	for _, line := range []string{"PATH=/tmp", "LD_PRELOAD=/tmp/x.so", "BASH_ENV=/tmp/x", "not a setting", "SCREE-X=1"} {
		e := setup(t, envFile+line+"\n", 0o600)
		for _, s := range scripts {
			out, lines, err := e.run(t, s, nil)
			if err == nil || lines != nil || !strings.Contains(out, "line 4") {
				t.Errorf("%s with %q: err %v, child ran %v, output %q", s, line, err, lines != nil, out)
			}
		}
	}
}

func TestScreeMCPDockerNeedsAbsoluteProjectDir(t *testing.T) {
	e := setup(t, "OPENROUTER_API_KEY="+secret+"\n", 0o600)
	out, lines, err := e.run(t, "scree-mcp-docker", []string{"CLAUDE_PROJECT_DIR=proj"})
	if err == nil || lines != nil || !strings.Contains(out, "absolute path") {
		t.Errorf("err %v, child ran %v, output %q", err, lines != nil, out)
	}
}

func TestScreeRunPassesArgs(t *testing.T) {
	e := setup(t, envFile, 0o600)
	out, lines, err := e.run(t, "scree-run", nil, "search", "a b")
	if err != nil || out != "" {
		t.Fatalf("err %v, output %q", err, out)
	}
	if len(lines) < 2 || lines[0] != "arg=search" || lines[1] != "arg=a b" || !slices.Contains(lines, "env=SCREE_MODEL=openai/text-embedding-3-small") {
		t.Errorf("child saw %q", lines)
	}
}

// CLAUDE_PROJECT_DIR comes from Claude Code's environment. The env file must
// not be able to set it and so redirect the mount.
func TestDockerPassesProjectDirNotFromFile(t *testing.T) {
	e := setup(t, envFile+"CLAUDE_PROJECT_DIR=/etc\n", 0o600)
	for _, s := range scripts {
		out, lines, err := e.run(t, s, nil)
		if err == nil || lines != nil || !strings.Contains(out, "CLAUDE_PROJECT_DIR is not a scree setting") {
			t.Errorf("%s: err %v, child ran %v, output %q", s, err, lines != nil, out)
		}
	}
}

// A project's .mcp.json can launch a wrapper with its own environment. None
// of it may reach scree: not the endpoint, not the key, not PATH.
func TestWrapperIgnoresInheritedEnv(t *testing.T) {
	for _, s := range scripts {
		e := setup(t, envFile, 0o600)
		planted := filepath.Join(e.dir, "planted")
		if err := os.Mkdir(planted, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"scree", "docker"} {
			os.WriteFile(filepath.Join(planted, name), []byte("#!/bin/sh\ntouch \"$HOME/pwned-path\"\n"), 0o755)
		}
		extra := []string{
			"PATH=" + planted + ":/usr/bin:/bin",
			"SCREE_EMBED_URL=http://attacker.example/v1/embeddings",
			"OPENROUTER_API_KEY=attacker-key",
			"SCREE_BIN=" + filepath.Join(planted, "scree"),
			"SCREE_DOCKER=" + filepath.Join(planted, "docker"),
			"SCREE_DATA_VOLUME=" + e.dir,
			"SCREE_NOTES=/",
			"SCREE_MOUNT_OPTS=rw",
			"HTTPS_PROXY=http://attacker.example:3128",
		}
		args := []string{"search", "x"}
		if s != "scree-run" {
			args = nil
		}
		out, lines, err := e.run(t, s, extra, args...)
		if err != nil {
			t.Fatalf("%s: err %v, output %q", s, err, out)
		}
		if _, err := os.Stat(filepath.Join(e.dir, "pwned-path")); err == nil {
			t.Errorf("%s ran a binary from the inherited PATH", s)
		}
		for _, l := range lines {
			if strings.Contains(l, "attacker") || strings.Contains(l, e.dir+":/data") {
				t.Errorf("%s passed inherited setting %q", s, l)
			}
		}
		if !slices.Contains(lines, "env=OPENROUTER_API_KEY="+secret) {
			t.Errorf("%s: key from the env file missing: %q", s, lines)
		}
		if !slices.Contains(lines, "env=SCREE_MODEL=openai/text-embedding-3-small") {
			t.Errorf("%s: SCREE_MODEL from the env file missing: %q", s, lines)
		}
	}
}

func TestMCPWrappersRefuseArgs(t *testing.T) {
	e := setup(t, envFile, 0o600)
	for _, s := range []string{"scree-mcp", "scree-mcp-docker"} {
		out, lines, err := e.run(t, s, nil, "-notes", "/")
		if err == nil || lines != nil || !strings.Contains(out, "takes no arguments") {
			t.Errorf("%s: err %v, child ran %v, output %q", s, err, lines != nil, out)
		}
	}
}

func TestScreeMCPDockerSettingsValidated(t *testing.T) {
	cases := map[string]string{
		"SCREE_DOCKER=/tmp/evil\n":      "must be docker or podman",
		"SCREE_DATA_VOLUME=/home/u\n":   "volume name, not a path",
		"SCREE_DATA_VOLUME=../x\n":      "volume name, not a path",
		"SCREE_DATA_VOLUME='-v /:/x'\n": "volume name, not a path",
	}
	for line, want := range cases {
		e := setup(t, envFile+line, 0o600)
		out, lines, err := e.run(t, "scree-mcp-docker", nil)
		if err == nil || lines != nil || !strings.Contains(out, want) {
			t.Errorf("%q: err %v, child ran %v, output %q", line, err, lines != nil, out)
		}
	}
}

// mountOf returns the host side of the folder mount in docker's arguments.
func mountOf(t *testing.T, args []string) string {
	t.Helper()
	for i, a := range args {
		if a == "-v" && i+1 < len(args) && !strings.HasSuffix(args[i+1], ":/data") {
			return strings.SplitN(args[i+1], ":", 2)[0]
		}
	}
	t.Fatalf("no folder mount in %q", args)
	return ""
}

func TestDockerResolvesProjectDir(t *testing.T) {
	e := setup(t, envFile, 0o600)
	real := filepath.Join(e.dir, "real")
	link := filepath.Join(e.dir, "link")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	args, _ := dockerArgs(t, e, link)
	if got := mountOf(t, args); got != want {
		t.Errorf("mounted %q, want %q", got, want)
	}
	if i := slices.Index(args, "-w"); i < 0 || args[i+1] != want || !slices.Contains(args, "CLAUDE_PROJECT_DIR="+want) {
		t.Errorf("workdir or project dir not resolved: %q", args)
	}
}

// TestDockerFollowsWorkingDirectory: the mount is the git root of the
// working directory; a CLAUDE_PROJECT_DIR in another repo, or at home, is
// refused rather than followed.
func TestDockerFollowsWorkingDirectory(t *testing.T) {
	e := setup(t, envFile, 0o600)
	repo := filepath.Join(e.dir, "repo")
	other := filepath.Join(e.dir, "other")
	for _, d := range []string{filepath.Join(repo, ".git"), filepath.Join(repo, "sub"), filepath.Join(other, ".git")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	args, _ := dockerArgs(t, e, repo, "CWD="+filepath.Join(repo, "sub"))
	if got := mountOf(t, args); got != repo {
		t.Errorf("mounted %q, want %q", got, repo)
	}
	for _, pd := range []string{other, e.dir, "/"} {
		os.Remove(e.out)
		out, lines, err := e.run(t, "scree-mcp-docker", []string{"CLAUDE_PROJECT_DIR=" + pd, "CWD=" + repo})
		if err == nil || lines != nil || !strings.Contains(out, "is not in the repo of the working directory") {
			t.Errorf("forged CLAUDE_PROJECT_DIR %s: err %v, child ran %v, output %q", pd, err, lines != nil, out)
		}
	}
}

// TestNoProgramFromInheritedPath: a planted id (or any program) earlier in
// an inherited PATH, absolute or relative, never runs.
func TestNoProgramFromInheritedPath(t *testing.T) {
	for _, s := range scripts {
		e := setup(t, envFile, 0o600)
		marker := filepath.Join(e.dir, "pwned-id")
		evil := "#!/bin/sh\ntouch " + marker + "\necho root\n"
		for _, d := range []string{filepath.Join(e.dir, "planted"), filepath.Join(e.project, ".bin")} {
			os.MkdirAll(d, 0o755)
			for _, name := range []string{"id", "env", "sed", "stat", "dirname", "ls", "pwd", "scree", "docker"} {
				os.WriteFile(filepath.Join(d, name), []byte(evil), 0o755)
			}
		}
		args := []string{"search", "x"}
		if s != "scree-run" {
			args = nil
		}
		out, lines, err := e.run(t, s, []string{"PATH=.bin:" + filepath.Join(e.dir, "planted") + ":/usr/bin:/bin"}, args...)
		if _, serr := os.Stat(marker); serr == nil {
			t.Errorf("%s ran a program from the inherited PATH (output %q)", s, out)
		}
		if err != nil || !slices.Contains(lines, "env=OPENROUTER_API_KEY="+secret) {
			t.Errorf("%s: err %v, output %q, lines %q", s, err, out, lines)
		}
	}
}

// TestWrappersHandOffPasswdHome: the child gets HOME from the password
// database and no XDG_DATA_HOME or XDG_CONFIG_HOME from the environment.
func TestWrappersHandOffPasswdHome(t *testing.T) {
	e := setup(t, envFile, 0o600)
	fake := "#!/bin/sh\nprintf 'HOME=%s\\nXDG_DATA_HOME=%s\\nXDG_CONFIG_HOME=%s\\n' \"$HOME\" \"${XDG_DATA_HOME-unset}\" \"${XDG_CONFIG_HOME-unset}\" > \"$FAKE_OUT\"\n"
	for _, name := range []string{"scree", "docker"} {
		if err := os.WriteFile(filepath.Join(e.bin, name), []byte(fake), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range scripts {
		os.Remove(e.out)
		_, lines, err := e.run(t, s, []string{"HOME=/nonexistent", "XDG_DATA_HOME=/tmp/evil", "XDG_CONFIG_HOME=/tmp/evil"})
		want := []string{"HOME=" + e.dir, "XDG_DATA_HOME=unset", "XDG_CONFIG_HOME=unset"}
		if err != nil || !slices.Equal(lines, want) {
			t.Errorf("%s: err %v, child saw %q, want %q", s, err, lines, want)
		}
	}
}

func TestDockerRefusesMissingProjectDir(t *testing.T) {
	e := setup(t, envFile, 0o600)
	out, lines, err := e.run(t, "scree-mcp-docker", []string{"CLAUDE_PROJECT_DIR=" + filepath.Join(e.dir, "gone")})
	if err == nil || lines != nil || !strings.Contains(out, "cannot open CLAUDE_PROJECT_DIR") {
		t.Errorf("err %v, child ran %v, output %q", err, lines != nil, out)
	}
}

func TestDockerRefusesHomeAncestors(t *testing.T) {
	e := setup(t, envFile, 0o600)
	home := filepath.Join(e.dir, "home", "u")
	if err := os.MkdirAll(filepath.Join(home, "code", "r"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The env file lives in the home taken from the password database.
	if err := os.MkdirAll(filepath.Join(home, ".config", "scree"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config", "scree", "env"), []byte(envFile), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{home, filepath.Join(e.dir, "home"), e.dir, "/"} {
		out, lines, err := e.run(t, "scree-mcp-docker", []string{"TEST_PW_HOME=" + home, "CLAUDE_PROJECT_DIR=" + dir})
		if err == nil || lines != nil || !strings.Contains(out, "refusing to mount") {
			t.Errorf("%s: err %v, child ran %v, output %q", dir, err, lines != nil, out)
		}
	}
	// A symlinked home is compared resolved.
	link := filepath.Join(e.dir, "homelink")
	if err := os.Symlink(home, link); err != nil {
		t.Fatal(err)
	}
	out, lines, err := e.run(t, "scree-mcp-docker", []string{"TEST_PW_HOME=" + link, "CLAUDE_PROJECT_DIR=" + filepath.Join(e.dir, "home")})
	if err == nil || lines != nil || !strings.Contains(out, "refusing to mount") {
		t.Errorf("symlinked home: err %v, child ran %v, output %q", err, lines != nil, out)
	}
	args, _ := dockerArgs(t, e, filepath.Join(home, "code", "r"), "TEST_PW_HOME="+link)
	if got := mountOf(t, args); got != filepath.Join(home, "code", "r") {
		t.Errorf("mounted %q", got)
	}
}

func TestDockerMountOptsReadOnly(t *testing.T) {
	for _, opts := range []string{"rw", "ro,rw", "z", "ro,z,rw", "ro z", ""} {
		e := setup(t, envFile+"SCREE_MOUNT_OPTS='"+opts+"'\n", 0o600)
		out, lines, err := e.run(t, "scree-mcp-docker", nil)
		if opts == "" {
			// Empty means the default, ro.
			if err != nil || !slices.Contains(lines, "arg="+e.project+":"+e.project+":ro") {
				t.Errorf("empty: err %v, output %q, lines %q", err, out, lines)
			}
			continue
		}
		if err == nil || lines != nil || !strings.Contains(out, "SCREE_MOUNT_OPTS must be") {
			t.Errorf("%q: err %v, child ran %v, output %q", opts, err, lines != nil, out)
		}
	}
	for _, opts := range []string{"ro", "ro,z", "ro,Z"} {
		e := setup(t, envFile+"SCREE_MOUNT_OPTS="+opts+"\n", 0o600)
		args, _ := dockerArgs(t, e, e.project)
		if !slices.Contains(args, e.project+":"+e.project+":"+opts) {
			t.Errorf("%q: docker args %q", opts, args)
		}
	}
}

func TestDockerNoLiftThroughSticky(t *testing.T) {
	e := setup(t, envFile, 0o600)
	sticky := filepath.Join(e.dir, "sticky")
	p := filepath.Join(sticky, "p")
	for _, d := range []string{filepath.Join(sticky, ".git"), p} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(sticky, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	args, _ := dockerArgs(t, e, p)
	if got := mountOf(t, args); got != p {
		t.Errorf("mounted %q, want %q", got, p)
	}
	// A .git below the sticky folder still lifts.
	if err := os.Mkdir(filepath.Join(p, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(p, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	args, _ = dockerArgs(t, e, sub)
	if got := mountOf(t, args); got != p {
		t.Errorf("mounted %q, want %q", got, p)
	}
}

func TestDockerNoLiftThroughSymlinkGit(t *testing.T) {
	e := setup(t, envFile, 0o600)
	repo := filepath.Join(e.dir, "repo")
	sub := filepath.Join(repo, "sub")
	gitdir := filepath.Join(e.dir, "elsewhere.git")
	for _, d := range []string{sub, gitdir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(gitdir, filepath.Join(repo, ".git")); err != nil {
		t.Fatal(err)
	}
	args, _ := dockerArgs(t, e, sub)
	if got := mountOf(t, args); got != sub {
		t.Errorf("mounted %q, want %q", got, sub)
	}
}

func TestDockerNoLiftToForeignGit(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root to chown .git to another user")
	}
	e := setup(t, envFile, 0o600)
	repo := filepath.Join(e.dir, "repo")
	sub := filepath.Join(repo, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(filepath.Join(repo, ".git"), 65534, 65534); err != nil {
		t.Fatal(err)
	}
	args, _ := dockerArgs(t, e, sub)
	if got := mountOf(t, args); got != sub {
		t.Errorf("mounted %q, want %q", got, sub)
	}
}

func TestWrapperRefusesForeignEnvFile(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root to chown the env file to another user")
	}
	e := setup(t, envFile, 0o600)
	if err := os.Chown(e.envFile, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	for _, s := range scripts {
		out, lines, err := e.run(t, s, nil)
		if err == nil || lines != nil || !strings.Contains(out, s+": "+e.envFile+" is not owned by you") {
			t.Errorf("%s: err %v, child ran %v, output %q", s, err, lines != nil, out)
		}
	}
}

const (
	blockBegin = "# BEGIN passwd home"
	blockEnd   = "# END passwd home\n"
)

// passwdBlock returns the part of a wrapper that finds the home directory
// in the password database, from its BEGIN line through its END line.
func passwdBlock(script string) (string, error) {
	i := strings.Index(script, blockBegin)
	j := strings.Index(script, blockEnd)
	if i < 0 || j < i {
		return "", errors.New("no passwd home block")
	}
	return script[i : j+len(blockEnd)], nil
}

// TestPasswdHome runs each wrapper's lookup block alone: it finds the
// home directory in the password database, whatever HOME and
// XDG_CONFIG_HOME say.
func TestPasswdHome(t *testing.T) {
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		t.Skip("no current user:", err)
	}
	for _, s := range scripts {
		b, err := os.ReadFile(s)
		if err != nil {
			t.Fatal(err)
		}
		block, err := passwdBlock(string(b))
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		cmd := exec.Command("/bin/sh", "-c", "set -eu; prog=test\n"+block+`printf '%s' "$pwhome"`)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), "XDG_CONFIG_HOME=" + t.TempDir(), "SCREE_ENV_FILE=/tmp/x"}
		out, err := cmd.CombinedOutput()
		if err != nil || string(out) != u.HomeDir {
			t.Errorf("%s: home %q, %v; want %q", s, out, err, u.HomeDir)
		}
	}
}

// TestEnvFileNotFromEnvironment: SCREE_ENV_FILE, XDG_CONFIG_HOME and HOME
// from a project's .mcp.json do not choose the settings file.
func TestEnvFileNotFromEnvironment(t *testing.T) {
	e := setup(t, envFile, 0o600)
	other := filepath.Join(e.dir, "other")
	planted := filepath.Join(other, "scree", "env")
	os.MkdirAll(filepath.Dir(planted), 0o700)
	if err := os.WriteFile(planted, []byte("OPENROUTER_API_KEY=planted-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, s := range scripts {
		os.Remove(e.out)
		_, lines, err := e.run(t, s, []string{"SCREE_ENV_FILE=" + planted, "XDG_CONFIG_HOME=" + other, "HOME=" + filepath.Join(other, "..")})
		if err != nil || !slices.Contains(lines, "env=OPENROUTER_API_KEY="+secret) {
			t.Errorf("%s: err %v, lines %q", s, err, lines)
		}
	}
}
