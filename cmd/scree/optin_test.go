package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// optIn writes the .mcp.json `claude mcp add scree --scope project --
// scree mcp` writes into dir.
func optIn(t *testing.T, dir string) {
	t.Helper()
	src := `{"mcpServers": {"scree": {"type": "stdio", "command": "scree", "args": ["mcp"], "env": {}}}}`
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCheckOptIn(t *testing.T) {
	dir := tempDir(t)
	p := filepath.Join(dir, ".mcp.json")
	if err := checkOptIn(dir); !errors.Is(err, errNotOptedIn) || !strings.Contains(err.Error(), "claude mcp add scree --scope project -- scree mcp") {
		t.Errorf("no .mcp.json: %v", err)
	}
	for src, ok := range map[string]bool{
		`{"mcpServers": {"scree": {"command": "scree", "args": ["mcp"]}}}`:                true,
		`{"mcpServers": {"scree": {"command": "scree", "args": ["mcp", "-docker"]}}}`:     true,
		`{"mcpServers": {"x": {"command": "/home/u/.local/bin/scree", "args": ["mcp"]}}}`: true,
		`{"mcpServers": {"x": {"command": "scree"}}}`:                                     false,
		`{"mcpServers": {"x": {"command": "scree", "args": []}}}`:                         false,
		`{"mcpServers": {"x": {"command": "scree", "args": ["serve"]}}}`:                  false,
		`{"mcpServers": {"x": {"command": "scree", "args": ["-v", "mcp"]}}}`:              false,
		`{"mcpServers": {"x": {"command": "not-scree", "args": ["mcp"]}}}`:                false,
		`{"mcpServers": {"x": {"command": "scree-evil", "args": ["mcp"]}}}`:               false,
		`{"mcpServers": {"x": {"command": "node", "args": ["mcp"]}}}`:                     false,
		`{"mcpServers": {"scree": {"command": "scree-mcp"}}}`:                             true,
		`{"mcpServers": {"x": {"command": "/home/u/.local/bin/scree-mcp"}}}`:              true,
		`{"mcpServers": {"other": {"command": "node"}, "s": {"command": "scree-mcp"}}}`:   true,
		`{"mcpServers": {"x": {"command": "not-scree-mcp"}}}`:                             false,
		`{"mcpServers": {"x": {"command": "scree-mcp-evil"}}}`:                            false,
		`{"mcpServers": {"x": {"args": ["scree-mcp"]}}}`:                                  false,
		`{"other": {"command": "scree-mcp"}}`:                                             false,
		`{}`:                                                                              false,
	} {
		os.WriteFile(p, []byte(src), 0o644)
		if err := checkOptIn(dir); (err == nil) != ok {
			t.Errorf("%s: %v", src, err)
		}
	}
	os.WriteFile(p, []byte(`{"mcpServers": {"scree": {"command": "scree-mcp"`), 0o644)
	if err := checkOptIn(dir); err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("bad JSON: %v", err)
	}
	big := `{"mcpServers": {"scree": {"command": "scree-mcp"}}, "pad": "` + strings.Repeat("x", 70<<10) + `"}`
	os.WriteFile(p, []byte(big), 0o644)
	if err := checkOptIn(dir); err == nil || !strings.Contains(err.Error(), "64 KB") {
		t.Errorf("big: %v", err)
	}
	os.Remove(p)
	real := filepath.Join(tempDir(t), "real.json")
	os.WriteFile(real, []byte(`{"mcpServers": {"scree": {"command": "scree-mcp"}}}`), 0o644)
	if err := os.Symlink(real, p); err != nil {
		t.Skip("no symlinks")
	}
	if err := checkOptIn(dir); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("symlink: %v", err)
	}
}
