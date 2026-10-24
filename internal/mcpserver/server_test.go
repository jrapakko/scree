package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jrapakko/scree/internal/index"
	"github.com/jrapakko/scree/internal/store"
)

type lenEmbedder struct{}

func (lenEmbedder) Embed(_ context.Context, in []string) ([][]float32, error) {
	out := make([][]float32, len(in))
	for i, s := range in {
		out[i] = []float32{1, float32(strings.Count(s, "zebra"))}
	}
	return out, nil
}
func (lenEmbedder) Model() string { return "len" }

func connect(t *testing.T) (*mcp.ClientSession, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	root := filepath.Join(dir, "notes")
	os.MkdirAll(root, 0o755)
	os.WriteFile(filepath.Join(root, "z.md"), []byte("# Zoo\nzebra zebra\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "secret.md"), []byte("secret"), 0o644)
	os.Symlink(filepath.Join(dir, "secret.md"), filepath.Join(root, "link.md"))

	s, err := store.Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ix := &index.Indexer{Root: root, Store: s, Embedder: lenEmbedder{}}

	ct, st := mcp.NewInMemoryTransports()
	if _, err := New(ix, "test").Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, root
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res.Content[0].(*mcp.TextContent).Text, res.IsError
}

func TestTools(t *testing.T) {
	cs, _ := connect(t)

	if text, _ := call(t, cs, "search_notes", map[string]any{"query": "zebra"}); text != "No matching notes." {
		t.Errorf("search before index = %q", text)
	}
	if text, isErr := call(t, cs, "reindex", nil); isErr || !strings.Contains(text, "1 indexed") {
		t.Errorf("reindex = %q", text)
	}
	text, _ := call(t, cs, "search_notes", map[string]any{"query": "zebra", "k": 3})
	if !strings.HasPrefix(text, "## z.md:1 · Zoo") {
		t.Errorf("search = %q", text)
	}
	if text, isErr := call(t, cs, "read_note", map[string]any{"path": "z.md"}); isErr || !strings.Contains(text, "zebra zebra") {
		t.Errorf("read_note = %q", text)
	}
	for _, bad := range []string{"../secret.md", "link.md", "/etc/passwd", "z.txt"} {
		if text, isErr := call(t, cs, "read_note", map[string]any{"path": bad}); !isErr {
			t.Errorf("read_note(%q) allowed: %q", bad, text)
		}
	}
}
