// Command scree is local semantic search over markdown notes, served to
// Claude Code over MCP.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jrapakko/scree/internal/embed"
	"github.com/jrapakko/scree/internal/index"
	"github.com/jrapakko/scree/internal/mcpserver"
	"github.com/jrapakko/scree/internal/store"
	"github.com/jrapakko/scree/internal/watch"
)

var version = "dev"

const usage = `scree: semantic search over markdown notes

Usage:
  scree watch              index changed notes, then keep the index current
  scree serve [-http addr] run the MCP server (stdio unless -http is given)
  scree search [-k n] "query"
  scree reindex [-all] [path]
  scree doctor             check configuration and the index

Every command takes -notes and -db. Environment:
  SCREE_NOTES         notes folder (required unless -notes is given)
  SCREE_DB            index file (default $XDG_DATA_HOME/scree/index.db)
  SCREE_MODEL         embedding model (default openai/text-embedding-3-small)
  SCREE_DIMENSIONS    embedding size, 0 for the model default (default 512)
  SCREE_EMBED_URL     embeddings endpoint (default OpenRouter)
  OPENROUTER_API_KEY  API key for the endpoint
`

type config struct {
	notes, db, model, url, apiKey string
	dims                          int
}

func main() {
	log.SetFlags(log.LstdFlags)
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "watch":
		err = runWatch(ctx, args)
	case "serve":
		err = runServe(ctx, args)
	case "search":
		err = runSearch(ctx, args)
	case "reindex":
		err = runReindex(ctx, args)
	case "doctor":
		err = runDoctor(ctx, args)
	case "version", "-version", "--version":
		fmt.Println("scree", version)
	case "help", "-h", "-help", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "scree: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "scree:", err)
		os.Exit(1)
	}
}

func flags(name string, cfg *config) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.StringVar(&cfg.notes, "notes", os.Getenv("SCREE_NOTES"), "notes folder")
	fs.StringVar(&cfg.db, "db", envOr("SCREE_DB", defaultDB()), "index file")
	cfg.model = envOr("SCREE_MODEL", "openai/text-embedding-3-small")
	cfg.url = envOr("SCREE_EMBED_URL", "https://openrouter.ai/api/v1/embeddings")
	cfg.apiKey = os.Getenv("OPENROUTER_API_KEY")
	cfg.dims = 512
	if v := os.Getenv("SCREE_DIMENSIONS"); v != "" {
		if _, err := fmt.Sscan(v, &cfg.dims); err != nil {
			log.Fatalf("SCREE_DIMENSIONS: %v", err)
		}
	}
	return fs
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func defaultDB() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "scree", "index.db")
}

// open builds an indexer from cfg. checkModel guards against mixing vectors
// from different models.
func open(cfg *config, checkModel bool) (*index.Indexer, error) {
	if cfg.notes == "" {
		return nil, errors.New("no notes folder: set SCREE_NOTES or pass -notes")
	}
	root, err := filepath.Abs(cfg.notes)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("notes folder %s is not a directory", root)
	}
	s, err := store.Open(cfg.db)
	if err != nil {
		return nil, err
	}
	e := embed.New(cfg.url, cfg.apiKey, cfg.model, cfg.dims)
	if checkModel {
		if err := s.CheckModel(e.Model()); err != nil {
			s.Close()
			return nil, err
		}
	}
	return &index.Indexer{Root: root, Store: s, Embedder: e, Log: log.Default()}, nil
}

func runWatch(ctx context.Context, args []string) error {
	var cfg config
	flags("watch", &cfg).Parse(args)
	ix, err := open(&cfg, true)
	if err != nil {
		return err
	}
	defer ix.Store.Close()
	log.Printf("syncing %s", ix.Root)
	st, err := ix.Sync(ctx)
	if err != nil {
		return err
	}
	log.Printf("sync done: %d notes, %d indexed, %d removed, %d failed", st.Scanned, st.Indexed, st.Removed, st.Failed)
	log.Printf("watching %s", ix.Root)
	return watch.Run(ctx, ix)
}

func runServe(ctx context.Context, args []string) error {
	var cfg config
	fs := flags("serve", &cfg)
	addr := fs.String("http", "", "serve streamable HTTP on this address instead of stdio, e.g. 127.0.0.1:7077")
	fs.Parse(args)
	// stdout belongs to the MCP protocol.
	log.SetOutput(os.Stderr)
	ix, err := open(&cfg, true)
	if err != nil {
		return err
	}
	defer ix.Store.Close()
	srv := mcpserver.New(ix, version)
	if *addr == "" {
		return srv.Run(ctx, &mcp.StdioTransport{})
	}
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	hs := &http.Server{Addr: *addr, Handler: h}
	go func() {
		<-ctx.Done()
		hs.Close()
	}()
	log.Printf("serving MCP on http://%s", *addr)
	if err := hs.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func runSearch(ctx context.Context, args []string) error {
	var cfg config
	fs := flags("search", &cfg)
	k := fs.Int("k", 8, "number of results")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New(`usage: scree search [-k n] "query"`)
	}
	ix, err := open(&cfg, true)
	if err != nil {
		return err
	}
	defer ix.Store.Close()
	hits, err := ix.Search(ctx, fs.Arg(0), *k)
	if err != nil {
		return err
	}
	fmt.Println(mcpserver.FormatHits(hits))
	return nil
}

func runReindex(ctx context.Context, args []string) error {
	var cfg config
	fs := flags("reindex", &cfg)
	all := fs.Bool("all", false, "drop the index and rebuild it from scratch")
	fs.Parse(args)
	ix, err := open(&cfg, !*all)
	if err != nil {
		return err
	}
	defer ix.Store.Close()
	if *all {
		if err := ix.Store.Reset(ctx); err != nil {
			return err
		}
		if err := ix.Store.CheckModel(ix.Embedder.Model()); err != nil {
			return err
		}
	}
	if fs.NArg() == 1 {
		_, err := ix.IndexFile(ctx, fs.Arg(0))
		return err
	}
	st, err := ix.Sync(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("%d notes: %d indexed, %d removed, %d failed\n", st.Scanned, st.Indexed, st.Removed, st.Failed)
	if st.Failed > 0 {
		return fmt.Errorf("%d notes failed to index", st.Failed)
	}
	return nil
}

func runDoctor(ctx context.Context, args []string) error {
	var cfg config
	fs := flags("doctor", &cfg)
	offline := fs.Bool("offline", false, "skip the test call to the embeddings API")
	fs.Parse(args)

	ok := true
	report := func(good bool, format string, a ...any) {
		mark := "ok  "
		if !good {
			mark, ok = "FAIL", false
		}
		fmt.Printf("%s %s\n", mark, fmt.Sprintf(format, a...))
	}

	ix, err := open(&cfg, false)
	if err != nil {
		report(false, "%v", err)
		return errors.New("doctor found problems")
	}
	defer ix.Store.Close()
	report(true, "notes folder %s", ix.Root)

	st, err := ix.Store.Stats(ctx)
	report(err == nil, "index %s: %d notes, %d chunks (vec1 %s)", cfg.db, st.Files, st.Chunks, st.Vec1Info)

	have, _ := ix.Store.Model()
	want := ix.Embedder.Model()
	switch {
	case have == "":
		report(true, "model %s (index empty)", want)
	case have == want:
		report(true, "model %s", want)
	default:
		report(false, "index built with %s but configured for %s; run `scree reindex --all`", have, want)
	}

	report(cfg.apiKey != "", "OPENROUTER_API_KEY %s", map[bool]string{true: "set", false: "not set"}[cfg.apiKey != ""])
	if !*offline && cfg.apiKey != "" {
		vecs, err := ix.Embedder.Embed(ctx, []string{"scree doctor"})
		if err != nil {
			report(false, "embeddings API: %v", err)
		} else {
			report(true, "embeddings API returned %d dimensions", len(vecs[0]))
		}
	}
	if !ok {
		return errors.New("doctor found problems")
	}
	return nil
}
