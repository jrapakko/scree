package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jrapakko/scree/internal/embed"
	"github.com/jrapakko/scree/internal/index"
	"github.com/jrapakko/scree/internal/mcpserver"
	"github.com/jrapakko/scree/internal/policy"
	"github.com/jrapakko/scree/internal/store"
)

// mcpbManifest is the part of contrib/mcpb/manifest.json the tests check.
type mcpbManifest struct {
	Version string `json:"version"`
	Server  struct {
		Type       string `json:"type"`
		EntryPoint string `json:"entry_point"`
		MCPConfig  struct {
			Command           string            `json:"command"`
			Args              []string          `json:"args"`
			Env               map[string]string `json:"env"`
			PlatformOverrides map[string]struct {
				Command string            `json:"command"`
				Args    []string          `json:"args"`
				Env     map[string]string `json:"env"`
			} `json:"platform_overrides"`
		} `json:"mcp_config"`
	} `json:"server"`
	Tools []struct {
		Name string `json:"name"`
	} `json:"tools"`
	Compatibility struct {
		Platforms []string `json:"platforms"`
	} `json:"compatibility"`
	UserConfig map[string]struct {
		Type      string `json:"type"`
		Required  bool   `json:"required"`
		Sensitive bool   `json:"sensitive"`
		Default   *any   `json:"default"`
	} `json:"user_config"`
}

func readMCPBManifest(t *testing.T) mcpbManifest {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "contrib", "mcpb", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m mcpbManifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestMCPBCommand: the bundle runs the binary it ships as serve with an
// explicit -notes, and passes nothing else from the environment but the key.
func TestMCPBCommand(t *testing.T) {
	m := readMCPBManifest(t)
	c := m.Server.MCPConfig
	if m.Server.Type != "binary" || m.Server.EntryPoint != "server/scree" || c.Command != "${__dirname}/server/scree" {
		t.Errorf("server = %q %q, command %q", m.Server.Type, m.Server.EntryPoint, c.Command)
	}
	win, ok := c.PlatformOverrides["win32"]
	if !ok || win.Command != "${__dirname}/server/scree.exe" || win.Args != nil || win.Env != nil || len(c.PlatformOverrides) != 1 {
		t.Errorf("platform_overrides = %+v", c.PlatformOverrides)
	}
	if !slices.Equal(m.Compatibility.Platforms, []string{"darwin", "win32"}) {
		t.Errorf("platforms = %q", m.Compatibility.Platforms)
	}
	if len(c.Env) != 1 || c.Env["OPENROUTER_API_KEY"] != "${user_config.openrouter_api_key}" {
		t.Errorf("env = %q, want only OPENROUTER_API_KEY", c.Env)
	}
	if len(c.Args) == 0 || c.Args[0] != "serve" {
		t.Fatalf("args = %q, want serve first", c.Args)
	}
	// The rest parse as serve's flags, with -notes the chosen folder and
	// nothing left over.
	notes := "/picked folder"
	args := make([]string, len(c.Args)-1)
	for i, a := range c.Args[1:] {
		args[i] = strings.ReplaceAll(a, "${user_config.notes_dir}", notes)
	}
	var cfg config
	fs := flags("serve", &cfg)
	fs.Init("serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || cfg.notes != notes {
		t.Errorf("serve %q: err %v, notes %q, left %q", args, err, cfg.notes, fs.Args())
	}
}

// TestMCPBUserConfig: every ${user_config.X} the command uses is declared,
// and is required or has a default, since a client leaves an unset optional
// field as the literal placeholder.
func TestMCPBUserConfig(t *testing.T) {
	m := readMCPBManifest(t)
	c := m.Server.MCPConfig
	all := append([]string{c.Command}, c.Args...)
	for _, v := range c.Env {
		all = append(all, v)
	}
	re := regexp.MustCompile(`\$\{user_config\.([^}]*)\}`)
	used := map[string]bool{}
	for _, s := range all {
		for _, sub := range re.FindAllStringSubmatch(s, -1) {
			used[sub[1]] = true
		}
	}
	for name := range used {
		u, ok := m.UserConfig[name]
		if !ok {
			t.Errorf("user_config.%s is used but not declared", name)
			continue
		}
		if !u.Required && u.Default == nil {
			t.Errorf("user_config.%s is optional with no default", name)
		}
	}
	for name := range m.UserConfig {
		if !used[name] {
			t.Errorf("user_config.%s is declared but not used", name)
		}
	}
	if d := m.UserConfig["notes_dir"]; d.Type != "directory" || !d.Required || d.Default != nil {
		t.Errorf("notes_dir = %+v, want a required directory with no default", d)
	}
	k := m.UserConfig["openrouter_api_key"]
	if k.Type != "string" || !k.Sensitive || k.Required || k.Default == nil || *k.Default != "" {
		t.Errorf("openrouter_api_key = %+v, want sensitive, optional, default empty", k)
	}
}

// TestEnvKey: OPENROUTER_API_KEY as a client such as Claude Desktop may
// pass it. The bundle's default, an empty value, is no key, as if it were
// unset (serve never looks for the settings file's key); surrounding
// whitespace is trimmed; an unfilled ${user_config...} placeholder or a
// value scree key would refuse is no key, logged without the value.
func TestEnvKey(t *testing.T) {
	const key = "sk-or-v1-0123456789abcdef"
	for in, want := range map[string]string{
		"":                                    "",
		" \t\n":                               "",
		key:                                   key,
		"\n " + key + "\r\n":                  key,
		"${user_config.openrouter_api_key}":   "",
		" ${user_config.openrouter_api_key} ": "",
		"sk-or\x01" + key:                     "",
		"sk-or " + key:                        "",
		"sk-or\u200b" + key:                   "",
		`"` + key + `"`:                       "",
	} {
		t.Setenv("OPENROUTER_API_KEY", in)
		var logged bytes.Buffer
		log.SetOutput(&logged)
		var cfg config
		envConfig(&cfg)
		log.SetOutput(os.Stderr)
		if cfg.apiKey != want {
			t.Errorf("%q: key %q, want %q", in, cfg.apiKey, want)
		}
		if embed.New(cfg.url, cfg.apiKey, cfg.model, cfg.dims).HasKey() != (want != "") {
			t.Errorf("%q: HasKey wrong", in)
		}
		msg := logged.String()
		if strings.TrimSpace(in) != "" && want == "" && !strings.Contains(msg, "running without an API key") {
			t.Errorf("%q: logged %q", in, msg)
		}
		if (want != "" || strings.TrimSpace(in) == "") && msg != "" {
			t.Errorf("%q: logged %q for a usable or empty key", in, msg)
		}
		if strings.Contains(msg, key) || strings.Contains(msg, "sk-or") {
			t.Errorf("%q: the log shows the value: %q", in, msg)
		}
	}
	if slices.Contains(keyCommands, "serve") {
		t.Error("serve would look for the settings file's key")
	}
}

// TestMCPBTools: the manifest lists exactly the tools the server serves.
func TestMCPBTools(t *testing.T) {
	m := readMCPBManifest(t)
	var listed []string
	for _, tl := range m.Tools {
		listed = append(listed, tl.Name)
	}
	slices.Sort(listed)

	dir := tempDir(t)
	root := filepath.Join(dir, "notes")
	mkdir(t, root)
	s, err := store.Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	p, err := policy.Open(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	ix := &index.Indexer{Policy: p, Store: s, Embedder: embed.New("", "", "m", 0), Log: log.New(io.Discard, "", 0)}
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	if _, err := mcpserver.New(ix, nil, "test").Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var served []string
	for _, tl := range res.Tools {
		served = append(served, tl.Name)
	}
	slices.Sort(served)
	if !slices.Equal(listed, served) {
		t.Errorf("manifest tools = %q, server tools = %q", listed, served)
	}
}

// TestServeKeyHint: serve's hint names the extension's settings outside
// Claude Code (Claude Desktop), and under it says to run scree key, since
// the server already runs through scree mcp.
func TestServeKeyHint(t *testing.T) {
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	if h := noKeyHint(true); !strings.HasPrefix(h, "add an OpenRouter API key in the extension's settings, ") {
		t.Errorf("outside Claude Code: %q", h)
	}
	t.Setenv("CLAUDE_PROJECT_DIR", "/some/repo")
	want := "run scree key, or set OPENROUTER_API_KEY"
	if runtime.GOOS == "windows" {
		want = "set OPENROUTER_API_KEY"
	}
	if h := noKeyHint(true); h != want {
		t.Errorf("under Claude Code: %q, want %q", h, want)
	}
	if h := noKeyHint(false); strings.Contains(h, "extension") {
		t.Errorf("CLI: %q", h)
	}
}
