package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

// noKeyEmbedder embeds documents but, like an embed.Client with no API
// key, reports it has none.
type noKeyEmbedder struct{ lenEmbedder }

func (noKeyEmbedder) HasKey() bool { return false }

// TestSearchNoKey: with no API key, auto runs keyword search and says why;
// a mode that embeds the query is an error. Both give the Indexer's
// KeyHint, here the default, which never suggests scree key.
func TestSearchNoKey(t *testing.T) {
	cs, _, _, _ := connectWith(t, noKeyEmbedder{}, nil)
	if _, isErr := call(t, cs, "reindex", nil); isErr {
		t.Fatal("reindex failed")
	}
	for _, args := range []map[string]any{{"query": "zebra"}, {"query": "zebra", "mode": "auto"}} {
		text, isErr := call(t, cs, "search_docs", args)
		if isErr || !strings.HasPrefix(text, `<hit n="1" path="z.md" lines="1-2" heading="Zoo" match="keyword">`) ||
			!strings.HasSuffix(text, "</hit>\n[mode: auto → keyword: no API key; set OPENROUTER_API_KEY]") {
			t.Errorf("%v = %q, %v", args, text, isErr)
		}
	}
	text, isErr := call(t, cs, "search_docs", map[string]any{"query": "zebra", "mode": "keyword"})
	if isErr || strings.Contains(text, "[mode:") {
		t.Errorf("keyword = %q, %v", text, isErr)
	}
	for _, mode := range []string{"semantic", "hybrid"} {
		text, isErr := call(t, cs, "search_docs", map[string]any{"query": "zebra", "mode": mode})
		if !isErr || !strings.Contains(text, "no API key: set OPENROUTER_API_KEY;") || strings.Contains(text, "scree key") {
			t.Errorf("%s = %q, %v", mode, text, isErr)
		}
	}
}

func TestModeNoteKeyHint(t *testing.T) {
	if got := ModeNote(index.Keyword, "no API key; set X, or start through Y"); got != "[mode: auto → keyword: no API key; set X, or start through Y]" {
		t.Errorf("keyword: %q", got)
	}
	if got := ModeNote(index.Semantic, "set X"); got != "[mode: auto → semantic]" {
		t.Errorf("semantic: %q", got)
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
// the folder's code routes to hybrid after reindex reads it, whether or not
// a note mentions it; a mixed-case word the code does not declare does not.
func TestSearchDefaultsToAuto(t *testing.T) {
	cs, root, _ := connect(t)
	if err := os.WriteFile(filepath.Join(root, "zoo.go"), []byte("package zoo\n\ntype ZebraKeeper struct{}\n\ntype ZebraGroomer struct{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "keepers.md"), []byte("# Keepers\nThe ZebraKeeper feeds them.\n"), 0o644); err != nil {
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
		{map[string]any{"query": "who feeds the ZebraGroomer zebra"}, "</hit>\n[mode: auto → hybrid]"},
		{map[string]any{"query": "who feeds the ZebraWasher zebra"}, "</hit>\n[mode: auto → semantic]"},
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

func TestKeywordOnlyNote(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ix := &index.Indexer{Store: s}
	if got := KeywordOnlyNote(ctx, ix, index.Semantic, "h"); got != "" {
		t.Errorf("no keyword-only files: %q", got)
	}
	if err := s.ReplaceFile(ctx, "a.md", "h", 1, nil, []store.Chunk{{Path: "a.md", Text: "x"}}, nil); err != nil {
		t.Fatal(err)
	}
	for _, m := range []index.Mode{index.Semantic, index.Hybrid} {
		if got := KeywordOnlyNote(ctx, ix, m, "reindex embeds them"); got != "[1 files indexed for keyword search only; reindex embeds them]" {
			t.Errorf("%s: %q", m, got)
		}
	}
	if got := KeywordOnlyNote(ctx, ix, index.Keyword, "h"); got != "" {
		t.Errorf("keyword: %q", got)
	}
}

// TestReindexWithoutKey: the reindex tool counts files indexed without a
// key as indexed, keyword only, not failed.
func TestReindexWithoutKey(t *testing.T) {
	cs, _, _, _ := connectWith(t, noKeyEmbedder{}, nil)
	text, isErr := call(t, cs, "reindex", nil)
	if isErr || !strings.Contains(text, "1 indexed (1 keyword only: no API key), 0 removed, 0 failed, 0 deferred") {
		t.Fatalf("reindex = %q, %v", text, isErr)
	}
}

// TestKeyRejectedAtIndexing: when the API answers 401, the files are
// indexed for keyword search only and the API is not asked again; auto
// search finds them by keyword and says the key was rejected, a mode that
// embeds the query is an error saying so, and the key never shows.
func TestKeyRejectedAtIndexing(t *testing.T) {
	const key = "sk-or-v1-rejected0123456789abcdef"
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, `{"error":{"message":"bad key `+r.Header.Get("Authorization")+`"}}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	cs, _, _, _ := connectWith(t, embed.New(srv.URL, key, "m", 0), nil)
	var out []string
	text, isErr := call(t, cs, "reindex", nil)
	out = append(out, text)
	if isErr || !strings.Contains(text, "1 indexed (1 keyword only: API key rejected)") {
		t.Errorf("reindex = %q, %v", text, isErr)
	}
	text, isErr = call(t, cs, "search_docs", map[string]any{"query": "zebra"})
	out = append(out, text)
	if isErr || !strings.HasPrefix(text, `<hit n="1" path="z.md" lines="1-2" heading="Zoo" match="keyword">`) ||
		!strings.HasSuffix(text, "[mode: auto → keyword: the embedding API rejected the API key; replace it: set OPENROUTER_API_KEY]") {
		t.Errorf("auto = %q, %v", text, isErr)
	}
	text, isErr = call(t, cs, "search_docs", map[string]any{"query": "zebra", "mode": "semantic"})
	out = append(out, text)
	if !isErr || !strings.Contains(text, "API key rejected") || !strings.Contains(text, `mode "keyword" works`) {
		t.Errorf("semantic = %q, %v", text, isErr)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("%d API calls, want 1", n)
	}
	for _, o := range out {
		if strings.Contains(o, key) || strings.Contains(o, key[len(key)-12:]) {
			t.Errorf("output shows the key: %q", o)
		}
	}
}

// rejectingEmbedder embeds until reject is set, then answers as the API
// does for a rejected key.
type rejectingEmbedder struct {
	lenEmbedder
	reject *atomic.Bool
}

func (r rejectingEmbedder) Embed(ctx context.Context, in []string) ([][]float32, error) {
	if r.reject.Load() {
		return nil, fmt.Errorf("embed: %w: 401 Unauthorized", embed.ErrKeyRejected)
	}
	return r.lenEmbedder.Embed(ctx, in)
}

// TestKeyRejectedAtSearch: a key the API rejects first on a query makes
// that auto search run keyword instead, and later ones too.
func TestKeyRejectedAtSearch(t *testing.T) {
	var reject atomic.Bool
	cs, _, _, _ := connectWith(t, rejectingEmbedder{reject: &reject}, nil)
	if text, isErr := call(t, cs, "reindex", nil); isErr || strings.Contains(text, "keyword only") {
		t.Fatalf("reindex = %q", text)
	}
	reject.Store(true)
	for range 2 {
		text, isErr := call(t, cs, "search_docs", map[string]any{"query": "zebra"})
		if isErr || !strings.Contains(text, `path="z.md"`) || !strings.HasSuffix(text, "[mode: auto → keyword: the embedding API rejected the API key; replace it: set OPENROUTER_API_KEY]") {
			t.Errorf("auto = %q, %v", text, isErr)
		}
	}
}

// TestForbiddenInputIsPerFile: a 403, which OpenRouter sends for an input
// its moderation flags, fails that file only: the other files are still
// embedded, the key is not taken as rejected, and auto search stays
// semantic with no note about the key.
func TestForbiddenInputIsPerFile(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct{ Input []string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for _, in := range req.Input {
			if strings.Contains(in, "flaggedword") {
				http.Error(w, `{"error":{"message":"input flagged by moderation"}}`, http.StatusForbidden)
				return
			}
		}
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		var out struct {
			Data []item `json:"data"`
		}
		for i, in := range req.Input {
			out.Data = append(out.Data, item{i, []float32{1, float32(len(in))}})
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()
	cs, root, s, _ := connectWith(t, embed.New(srv.URL, "sk-or-v1-working0123456789", "m", 0), nil)
	os.WriteFile(filepath.Join(root, "bad.md"), []byte("# Bad\nflaggedword\n"), 0o644)
	os.WriteFile(filepath.Join(root, "y.md"), []byte("# Yak\nyak yak\n"), 0o644)
	text, isErr := call(t, cs, "reindex", nil)
	if isErr || !strings.Contains(text, "2 indexed, 0 removed, 1 failed") || strings.Contains(text, "keyword only") {
		t.Errorf("reindex = %q, %v", text, isErr)
	}
	if n, err := s.KeywordOnlyFiles(context.Background()); err != nil || n != 0 {
		t.Errorf("keyword-only files = %d, %v", n, err)
	}
	text, isErr = call(t, cs, "search_docs", map[string]any{"query": "zebra"})
	if isErr || !strings.HasSuffix(text, "[mode: auto → semantic]") || strings.Contains(text, "rejected") {
		t.Errorf("auto = %q, %v", text, isErr)
	}
	// Another sync tries the flagged file again, and only it.
	before := calls.Load()
	if text, _ := call(t, cs, "reindex", nil); !strings.Contains(text, "0 indexed, 0 removed, 1 failed") {
		t.Errorf("second reindex = %q", text)
	}
	if n := calls.Load() - before; n != 1 {
		t.Errorf("second reindex made %d calls, want 1", n)
	}
}
