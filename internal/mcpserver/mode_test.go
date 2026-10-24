package mcpserver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jrapakko/scree/internal/embed"
	"github.com/jrapakko/scree/internal/index"
	"github.com/jrapakko/scree/internal/store"
)

// queryDownEmbedder embeds documents but fails on queries (inputs with no
// newline), like an API that went down after indexing.
type queryDownEmbedder struct{ lenEmbedder }

func (q queryDownEmbedder) Embed(ctx context.Context, in []string) ([][]float32, error) {
	if len(in) == 1 && !strings.Contains(in[0], "\n") {
		return nil, fmt.Errorf("%w: 503", embed.ErrUnavailable)
	}
	return q.lenEmbedder.Embed(ctx, in)
}

func TestSearchModeArgument(t *testing.T) {
	cs, _, _, _ := connectWith(t, queryDownEmbedder{}, nil)
	if _, isErr := call(t, cs, "reindex", nil); isErr {
		t.Fatal("reindex failed")
	}
	text, isErr := call(t, cs, "search_docs", map[string]any{"query": "zebra", "mode": "keyword"})
	if isErr || !strings.HasPrefix(text, `<hit n="1" path="z.md" lines="1-2" heading="Zoo" match="keyword">`) {
		t.Errorf("keyword = %q", text)
	}
	text, isErr = call(t, cs, "search_docs", map[string]any{"query": "zebra"})
	if !isErr || !strings.Contains(text, `mode="keyword" still works`) {
		t.Errorf("hybrid with the API down = %q, %v", text, isErr)
	}
	if text, isErr := call(t, cs, "search_docs", map[string]any{"query": "zebra", "mode": "fuzzy"}); !isErr || !strings.Contains(text, "mode must be") {
		t.Errorf("bad mode = %q, %v", text, isErr)
	}
}

func TestFormatHitsMatch(t *testing.T) {
	kw := hit("a.md", 1, "A", "x")
	kw.EndLine, kw.Match = 2, store.MatchKeyword
	both := hit("b.md", 3, "B", "y")
	both.EndLine, both.Match, both.Distance = 4, store.MatchBoth, 0.25
	got := FormatHits([]store.Hit{kw, both}, "")
	want := "<hit n=\"1\" path=\"a.md\" lines=\"1-2\" heading=\"A\" match=\"keyword\">\nx\n</hit>\n" +
		"<hit n=\"2\" path=\"b.md\" lines=\"3-4\" heading=\"B\" score=\"0.750\" match=\"both\">\ny\n</hit>"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestWriteJSON(t *testing.T) {
	sem := hit("docs/x.md", 12, "Setup > Keys", "text")
	sem.EndLine, sem.Match, sem.Distance = 18, store.MatchBoth, 0.0876
	sem.Also = []store.Copy{{Path: "print/x.md", Line: 12, EndLine: 18}, {Path: "old.md", Line: 3}}
	sem.AlsoMore = 2
	kw := hit("a<b>.md", 0, "H\x1b[31m", "t")
	kw.Match, kw.Stale = store.MatchKeyword, true
	var b strings.Builder
	if err := WriteJSON(&b, []store.Hit{sem, kw}, index.Hybrid); err != nil {
		t.Fatal(err)
	}
	want := `{"n":1,"mode":"hybrid","path":"docs/x.md","start":12,"end":18,"heading":"Setup > Keys","score":0.912,"match":"both","stale":false,"also":[{"path":"print/x.md","start":12,"end":18},{"path":"old.md","start":3,"end":null}],"also_more":2}` + "\n" +
		`{"n":2,"mode":"hybrid","path":"a<b>.md","start":null,"end":null,"heading":"H[31m","score":null,"match":"keyword","stale":true,"also":[]}` + "\n"
	if b.String() != want {
		t.Errorf("got\n%s\nwant\n%s", b.String(), want)
	}
}

// TestSearchDefaultsToAuto: with no mode, search_docs runs auto and says
// which mode it chose; an explicit mode adds no note. A name declared in
// the folder's code routes to hybrid after reindex reads it.
func TestSearchDefaultsToAuto(t *testing.T) {
	cs, root, _ := connect(t)
	if err := os.WriteFile(filepath.Join(root, "zoo.go"), []byte("package zoo\n\ntype ZebraKeeper struct{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, isErr := call(t, cs, "reindex", nil); isErr {
		t.Fatal("reindex failed")
	}
	for _, c := range []struct {
		args map[string]any
		want string // the suffix after the last hit
	}{
		{map[string]any{"query": "zebra"}, "</hit>\n[mode: auto → semantic]"},
		{map[string]any{"query": "zebra_2 zebra"}, "</hit>\n[mode: auto → hybrid]"},
		{map[string]any{"query": "who feeds the ZebraKeeper zebra"}, "</hit>\n[mode: auto → hybrid]"},
		{map[string]any{"query": "zebra", "mode": "auto"}, "</hit>\n[mode: auto → semantic]"},
		{map[string]any{"query": "zebra", "mode": "hybrid"}, "</hit>"},
		{map[string]any{"query": "zebra", "mode": "semantic"}, "</hit>"},
	} {
		text, isErr := call(t, cs, "search_docs", c.args)
		if isErr || !strings.HasSuffix(text, c.want) {
			t.Errorf("%v = %q, want suffix %q", c.args, text, c.want)
		}
	}
}

func TestAlsoStale(t *testing.T) {
	h := hit("a.md", 1, "A", "x")
	h.EndLine = 2
	h.Also = []store.Copy{{Path: "b.md", Line: 3, EndLine: 4}, {Path: "c.md", Stale: true}}
	if got := FormatHits([]store.Hit{h}, ""); !strings.Contains(got, `also="b.md:3-4, c.md:stale"`) {
		t.Errorf("got %s", got)
	}
	var b strings.Builder
	WriteJSON(&b, []store.Hit{h}, index.Semantic)
	if !strings.Contains(b.String(), `{"path":"c.md","start":null,"end":null,"stale":true}`) {
		t.Errorf("json %s", b.String())
	}
}
