package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// maxMCPJSON bounds the .mcp.json the opt-in check reads.
const maxMCPJSON = 64 << 10

// errNotOptedIn means the folder's .mcp.json does not name scree mcp.
var errNotOptedIn = errors.New("this repo has not turned scree on")

// checkOptIn refuses a folder whose root .mcp.json has no MCP server whose
// command is scree (or a path ending in /scree) with "mcp" as its first
// argument, or the legacy scree-mcp wrapper (or a path ending in
// /scree-mcp). serve applies it
// under Claude Code, so a session indexes only a repo that opted in through
// its own committed .mcp.json; a user-scope registration alone indexes
// nothing. The file must be a regular file, not a symlink, under 64 KB, and
// valid JSON; it is never echoed.
func checkOptIn(root string) error {
	p := filepath.Join(root, ".mcp.json")
	notOptedIn := func() error {
		return fmt.Errorf("%w: %s must list scree mcp; to turn scree on, run `claude mcp add scree --scope project -- scree mcp` in %s and commit .mcp.json",
			errNotOptedIn, p, root)
	}
	fi, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return notOptedIn()
	}
	if err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is a symlink or not a regular file; not indexing", p)
	}
	if fi.Size() > maxMCPJSON {
		return fmt.Errorf("%s is larger than 64 KB; not indexing", p)
	}
	f, err := os.OpenFile(p, os.O_RDONLY|noFollow, 0)
	if err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	defer f.Close()
	if now, err := f.Stat(); err != nil || !os.SameFile(fi, now) {
		return fmt.Errorf("%s changed while being read; not indexing", p)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxMCPJSON+1))
	if err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	if len(data) > maxMCPJSON {
		return fmt.Errorf("%s is larger than 64 KB; not indexing", p)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("%s is not valid JSON; not indexing", p)
	}
	for _, s := range cfg.MCPServers {
		if isCommand(s.Command, "scree") && len(s.Args) > 0 && s.Args[0] == "mcp" || isCommand(s.Command, "scree-mcp") {
			return nil
		}
	}
	return notOptedIn()
}

// isCommand reports whether command is name or a path ending in /name.
func isCommand(command, name string) bool {
	return command == name || strings.HasSuffix(command, "/"+name)
}
