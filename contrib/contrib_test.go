// Package contrib holds files that ship beside the binary. The test checks
// the MCP wrapper scripts with a fake scree and a fake docker.
package contrib

import (
	"os"
	"os/exec"
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
	for v in OPENROUTER_API_KEY SCREE_NOTES SCREE_MODEL SCREE_DIMENSIONS; do
		if eval "[ -n \"\${$v+x}\" ]"; then eval "printf 'env=%s=%s\n' $v \"\$$v\""; fi
	done
} > "$FAKE_OUT"
`

type env struct {
	dir, envFile, fakeBin, out string
}

func setup(t *testing.T, content string, mode os.FileMode) env {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	dir := t.TempDir()
	e := env{dir: dir, envFile: filepath.Join(dir, "env"), fakeBin: filepath.Join(dir, "fake"), out: filepath.Join(dir, "out")}
	if content != "" {
		if err := os.WriteFile(e.envFile, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(e.envFile, mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(e.fakeBin, []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	return e
}

// run executes a wrapper with a minimal environment and returns its combined
// output, the lines the fake recorded, and the exit error.
func (e env) run(t *testing.T, script string, args ...string) (string, []string, error) {
	t.Helper()
	path, err := filepath.Abs(script)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(path, args...)
	// Run from the temp dir, which tests may fill with planted commands.
	cmd.Dir = e.dir
	cmd.Env = []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + e.dir,
		"SCREE_ENV_FILE=" + e.envFile,
		"SCREE_BIN=" + e.fakeBin,
		"SCREE_DOCKER=" + e.fakeBin,
		"FAKE_OUT=" + e.out,
	}
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

const envFile = "# scree settings\nSCREE_NOTES=/home/u/notes\nOPENROUTER_API_KEY=" + secret + "\nSCREE_MODEL=openai/text-embedding-3-small\n"

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
			out, lines, err := e.run(t, s)
			if err == nil || lines != nil || !strings.Contains(out, "chmod 600 "+e.envFile) {
				t.Errorf("%s with mode %o: err %v, child ran %v, output %q", s, mode, err, lines != nil, out)
			}
		}
	}
}

func TestWrapperRefusesMissingEnvFile(t *testing.T) {
	e := setup(t, "", 0)
	for _, s := range scripts {
		out, lines, err := e.run(t, s)
		if err == nil || lines != nil || !strings.Contains(out, "not found") {
			t.Errorf("%s: err %v, child ran %v, output %q", s, err, lines != nil, out)
		}
	}
}

func TestScreeMCPPassesEnv(t *testing.T) {
	e := setup(t, envFile, 0o600)
	out, lines, err := e.run(t, "scree-mcp", "-http", "127.0.0.1:7077")
	if err != nil || out != "" {
		t.Fatalf("err %v, output %q", err, out)
	}
	want := []string{
		"arg=serve", "arg=-http", "arg=127.0.0.1:7077",
		"env=OPENROUTER_API_KEY=" + secret,
		"env=SCREE_NOTES=/home/u/notes",
		"env=SCREE_MODEL=openai/text-embedding-3-small",
	}
	if !slices.Equal(lines, want) {
		t.Errorf("child saw\n%q\nwant\n%q", lines, want)
	}
}

func TestScreeMCPDockerArgs(t *testing.T) {
	e := setup(t, envFile+"SCREE_ZDR=0\n", 0o600)
	out, lines, err := e.run(t, "scree-mcp-docker", "-extra")
	if err != nil || out != "" {
		t.Fatalf("err %v, output %q", err, out)
	}
	var args []string
	for _, l := range lines {
		if a, ok := strings.CutPrefix(l, "arg="); ok {
			args = append(args, a)
		}
	}
	want := []string{
		"run", "-i", "--rm",
		"-e", "OPENROUTER_API_KEY", "-e", "SCREE_MODEL", "-e", "SCREE_ZDR",
		"-v", "/home/u/notes:/notes:ro,z", "-v", "scree-data:/data",
		"scree:latest", "serve", "-extra",
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

func TestScreeMCPDockerNeedsNotes(t *testing.T) {
	e := setup(t, "OPENROUTER_API_KEY="+secret+"\n", 0o600)
	out, lines, err := e.run(t, "scree-mcp-docker")
	if err == nil || lines != nil || !strings.Contains(out, "SCREE_NOTES is not set") {
		t.Errorf("err %v, child ran %v, output %q", err, lines != nil, out)
	}
}

// The env file is read as data. A value with spaces, $(...) or backquotes
// must never run anything, even when the cwd holds a matching executable.
func TestEnvFileIsData(t *testing.T) {
	notes := "/Users/u/Library/Mobile Documents/iCloud~md~obsidian/Documents/Vault"
	content := "SCREE_NOTES=" + notes + "\n" +
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
		out, lines, err := e.run(t, s)
		if err != nil {
			t.Fatalf("%s: err %v, output %q", s, err, out)
		}
		for _, f := range []string{"pwned-subst", "pwned-tick", "pwned-planted"} {
			if _, err := os.Stat(filepath.Join(e.dir, f)); err == nil {
				t.Errorf("%s: %s ran", s, f)
			}
		}
		if !slices.Contains(lines, "env=SCREE_NOTES="+notes) ||
			!slices.Contains(lines, "env=SCREE_MODEL=$(touch pwned-subst)`touch pwned-tick`") {
			t.Errorf("%s: values not kept literally: %q", s, lines)
		}
	}
}

func TestEnvFileSyntax(t *testing.T) {
	content := "  # indented comment\n\n" +
		"export OPENROUTER_API_KEY=\"" + secret + "\"\r\n" +
		"SCREE_NOTES='/home/u/my notes'\n" +
		"SCREE_MODEL=\"it's fine\"\n" +
		"SCREE_DIMENSIONS=256" // no final newline
	e := setup(t, content, 0o600)
	_, lines, err := e.run(t, "scree-mcp")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"env=OPENROUTER_API_KEY=" + secret,
		"env=SCREE_NOTES=/home/u/my notes",
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
			out, lines, err := e.run(t, s)
			if err == nil || lines != nil || !strings.Contains(out, "line 5") {
				t.Errorf("%s with %q: err %v, child ran %v, output %q", s, line, err, lines != nil, out)
			}
		}
	}
}

func TestScreeMCPDockerNeedsAbsoluteNotes(t *testing.T) {
	e := setup(t, "SCREE_NOTES=notes\nOPENROUTER_API_KEY="+secret+"\n", 0o600)
	out, lines, err := e.run(t, "scree-mcp-docker")
	if err == nil || lines != nil || !strings.Contains(out, "absolute path") {
		t.Errorf("err %v, child ran %v, output %q", err, lines != nil, out)
	}
}

func TestScreeRunPassesArgs(t *testing.T) {
	e := setup(t, envFile, 0o600)
	out, lines, err := e.run(t, "scree-run", "search", "a b")
	if err != nil || out != "" {
		t.Fatalf("err %v, output %q", err, out)
	}
	if len(lines) < 2 || lines[0] != "arg=search" || lines[1] != "arg=a b" || !slices.Contains(lines, "env=SCREE_NOTES=/home/u/notes") {
		t.Errorf("child saw %q", lines)
	}
}
