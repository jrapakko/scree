package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jrapakko/scree/internal/textsafe"
)

// pagerDirs are the only folders a pager named without a path is taken
// from; PATH is never searched.
var pagerDirs = []string{"/usr/local/bin", "/usr/bin", "/bin"}

// pagerArgv returns the pager for the text view on a terminal: SCREE_PAGER
// split on whitespace and run directly, never through a shell, its first
// word an absolute path or a name found in pagerDirs; or, when SCREE_PAGER
// is unset and TERM is set (less cannot drive a terminal it does not
// know), less from pagerDirs with -FRX. nil means no pager: SCREE_PAGER
// set but empty, or no pager found, which for SCREE_PAGER gives a warning
// on stderr.
func pagerArgv() []string {
	v, set := os.LookupEnv("SCREE_PAGER")
	if !set {
		if os.Getenv("TERM") == "" {
			return nil
		}
		if p := findExec(pagerDirs, "less"); p != "" {
			return []string{p, "-FRX"}
		}
		return nil
	}
	argv := strings.Fields(v)
	if len(argv) == 0 {
		return nil
	}
	switch name := argv[0]; {
	case filepath.IsAbs(name):
	case !strings.ContainsRune(name, '/') && !strings.ContainsRune(name, filepath.Separator):
		if argv[0] = findExec(pagerDirs, name); argv[0] == "" {
			pagerWarn("%s not found in %s", name, strings.Join(pagerDirs, ", "))
			return nil
		}
	default:
		pagerWarn("%s is neither an absolute path nor a name", name)
		return nil
	}
	return argv
}

// pagerWarn writes a warning about SCREE_PAGER on stderr.
func pagerWarn(format string, a ...any) {
	fmt.Fprintln(os.Stderr, textsafe.Strip("scree: warning: SCREE_PAGER: "+fmt.Sprintf(format, a...)+"; printing directly"))
}

// startPager starts pagerArgv's pager on stdout and returns the writer for
// the text view and a function that ends the pager: it closes the pager's
// input and waits for it to exit, and returns err, the error from writing
// the view, unless that came from the pager, whose input fails only once
// it has quit (q before the end). The pager's exit status is not reported.
// With no pager, or one that cannot start, it returns os.Stdout.
func startPager() (io.Writer, func(err error) error) {
	direct := func(err error) error { return err }
	argv := pagerArgv()
	if argv == nil {
		return os.Stdout, direct
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = pagerEnv()
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	in, err := cmd.StdinPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		if _, set := os.LookupEnv("SCREE_PAGER"); set {
			pagerWarn("%v", err)
		}
		return os.Stdout, direct
	}
	return in, func(error) error {
		in.Close()
		cmd.Wait()
		return nil
	}
}

// pagerEnv returns the pager's environment: the current one plus
// LESSCHARSET=utf-8 unless LESSCHARSET is set, since the text view is
// always UTF-8 and scree run and the settings-file key re-exec pass no
// LANG or LC_*, without which less shows non-ASCII bytes as <E2>.
func pagerEnv() []string {
	env := os.Environ()
	if _, set := os.LookupEnv("LESSCHARSET"); !set {
		env = append(env, "LESSCHARSET=utf-8")
	}
	return env
}

// findExec returns the path of the executable regular file name in the
// first of dirs that holds one, or "".
func findExec(dirs []string, name string) string {
	for _, d := range dirs {
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			return p
		}
	}
	return ""
}
