//go:build unix

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestServeExitsOnSIGTERMWithFIFO runs the real binary on a repo whose git
// files and one note are FIFOs, and checks that it exits promptly on SIGTERM
// and on stdin EOF.
func TestServeExitsOnSIGTERMWithFIFO(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain")
	}
	bin := filepath.Join(t.TempDir(), "scree")
	build := exec.Command(goBin, "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vec := make([]float32, 512)
		vec[0] = 1
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"index": 0, "embedding": vec}}})
	}))
	defer srv.Close()

	for _, mode := range []string{"SIGTERM", "stdin EOF"} {
		t.Run(mode, func(t *testing.T) {
			home := tempDir(t)
			repo := filepath.Join(tempDir(t), "repo")
			mkdir(t, filepath.Join(repo, ".git", "info"))
			mkdir(t, filepath.Join(repo, "docs"))
			for _, f := range []string{"docs/.gitignore", "pipe.md", "docs/pipe.md"} {
				if err := syscall.Mkfifo(filepath.Join(repo, f), 0o644); err != nil {
					t.Skipf("mkfifo: %v", err)
				}
			}
			if err := os.WriteFile(filepath.Join(repo, "a.md"), []byte("# a\nhello\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			optIn(t, repo)
			cmd := exec.Command(bin, "serve")
			cmd.Dir = repo
			cmd.Env = []string{
				"PATH=/usr/bin:/bin", "HOME=" + home,
				"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
				"XDG_DATA_HOME=" + filepath.Join(home, "data"),
				"CLAUDE_PROJECT_DIR=" + repo,
				"SCREE_EMBED_URL=" + srv.URL, "OPENROUTER_API_KEY=test", "SCREE_ZDR=0",
			}
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			logf := filepath.Join(home, "stderr")
			errOut, err := os.Create(logf)
			if err != nil {
				t.Fatal(err)
			}
			defer errOut.Close()
			cmd.Stderr = errOut
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			pid := cmd.Process.Pid
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			exited := false
			defer func() {
				if !exited {
					syscall.Kill(pid, syscall.SIGKILL)
					<-done
				}
			}()
			// Let it start the writer and reach the FIFOs.
			time.Sleep(time.Second)
			if mode == "SIGTERM" {
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			} else {
				stdin.Close()
			}
			start := time.Now()
			select {
			case <-done:
				exited = true
				b, _ := os.ReadFile(logf)
				t.Logf("exited after %s; stderr:\n%s", time.Since(start), b)
			case <-time.After(6 * time.Second):
				b, _ := os.ReadFile(logf)
				t.Fatalf("serve still running 6 s after %s; stderr:\n%s", mode, b)
			}
		})
	}
}
