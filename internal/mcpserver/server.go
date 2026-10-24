// Package mcpserver exposes the index to MCP clients such as Claude Code.
package mcpserver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jrapakko/scree/internal/index"
	"github.com/jrapakko/scree/internal/store"
)

const (
	defaultK = 8
	maxK     = 50
	// maxNoteBytes caps read_note so one huge file cannot flood the context.
	maxNoteBytes = 256 << 10
)

type searchArgs struct {
	Query string `json:"query" jsonschema:"what to look for, in natural language"`
	K     int    `json:"k,omitempty" jsonschema:"number of chunks to return (default 8, max 50)"`
}

type readArgs struct {
	Path string `json:"path" jsonschema:"note path relative to the notes folder, as returned by search_notes"`
}

type reindexArgs struct {
	Path string `json:"path,omitempty" jsonschema:"one note to re-index; omit to sync every changed note"`
}

// New returns an MCP server backed by ix.
func New(ix *index.Indexer, version string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "scree", Version: version}, &mcp.ServerOptions{
		Instructions: "scree searches the user's markdown notes by meaning. Use search_notes first; " +
			"results are short chunks with a path and heading. Call read_note only when a chunk " +
			"is not enough.",
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "search_notes",
		Description: "Semantic search over the user's markdown notes. Returns the most relevant chunks with file path, heading trail, and line number.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a searchArgs) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(a.Query) == "" {
			return errResult("query is required"), nil, nil
		}
		k := a.K
		if k <= 0 {
			k = defaultK
		}
		k = min(k, maxK)
		hits, err := ix.Search(ctx, a.Query, k)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return textResult(FormatHits(hits)), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "read_note",
		Description: "Read a whole note by the path search_notes returned.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a readArgs) (*mcp.CallToolResult, any, error) {
		text, err := readNote(ix, a.Path)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return textResult(text), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "reindex",
		Description: "Re-index one note, or with no path, every note that changed since it was last indexed.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a reindexArgs) (*mcp.CallToolResult, any, error) {
		if a.Path != "" {
			if !index.IsNote(a.Path) {
				return errResult("not a markdown note: " + a.Path), nil, nil
			}
			changed, err := ix.IndexFile(ctx, a.Path)
			if err != nil {
				return errResult(err.Error()), nil, nil
			}
			if !changed {
				return textResult(a.Path + " is already up to date."), nil, nil
			}
			return textResult("Re-indexed " + a.Path + "."), nil, nil
		}
		st, err := ix.Sync(ctx)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return textResult(fmt.Sprintf("Scanned %d notes: %d indexed, %d removed, %d failed.",
			st.Scanned, st.Indexed, st.Removed, st.Failed)), nil, nil
	})

	return s
}

// FormatHits renders search results for a model or a terminal.
func FormatHits(hits []store.Hit) string {
	if len(hits) == 0 {
		return "No matching notes."
	}
	var b strings.Builder
	for i, h := range hits {
		if i > 0 {
			b.WriteString("\n\n")
		}
		heading := h.Heading
		if heading == "" {
			heading = "(top)"
		}
		fmt.Fprintf(&b, "## %s:%d · %s · score %.3f\n\n%s", h.Path, h.Line, heading, 1-h.Distance, h.Text)
	}
	return b.String()
}

func readNote(ix *index.Indexer, rel string) (string, error) {
	if !index.IsNote(rel) {
		return "", fmt.Errorf("not a markdown note: %s", rel)
	}
	abs, err := ix.Abs(rel)
	if err != nil {
		return "", err
	}
	// Refuse symlinks that lead out of the notes folder.
	root, err := filepath.EvalSymlinks(ix.Root)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("no such note: %s", rel)
	}
	if r, err := filepath.Rel(root, real); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid note path %q", rel)
	}
	f, err := os.Open(real)
	if err != nil {
		return "", err
	}
	defer f.Close()
	buf := make([]byte, maxNoteBytes+1)
	n, _ := f.Read(buf)
	for n < len(buf) {
		m, err := f.Read(buf[n:])
		n += m
		if err != nil {
			break
		}
	}
	if n > maxNoteBytes {
		return string(buf[:maxNoteBytes]) + "\n\n[truncated at 256 KB]", nil
	}
	return string(buf[:n]), nil
}

func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func errResult(s string) *mcp.CallToolResult {
	r := textResult(s)
	r.IsError = true
	return r
}
