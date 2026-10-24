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
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jrapakko/scree/internal/embed"
	"github.com/jrapakko/scree/internal/index"
	"github.com/jrapakko/scree/internal/mcpserver"
	"github.com/jrapakko/scree/internal/policy"
	"github.com/jrapakko/scree/internal/store"
	"github.com/jrapakko/scree/internal/textsafe"
	"github.com/jrapakko/scree/internal/watch"
)

var version = "dev"

const usage = `scree: semantic search over markdown notes

Usage:
  scree watch              index changed notes, then keep the index current
  scree serve [-http addr [-allow-remote]]
                           run the MCP server (stdio unless -http is given;
                           -http is loopback only unless -allow-remote)
  scree search [-k n] "query"
  scree reindex [-all] [path]
  scree doctor [-v] [-offline]
                           check configuration, the index and ignore rules

Every command takes -notes, -db and -gitignore. The folder is -notes if
given; for serve, else $CLAUDE_PROJECT_DIR; otherwise the git root of the
current directory, or the current directory outside git. Environment:
  SCREE_DB            index file (default $XDG_DATA_HOME/scree/indexes/<folder>-<hash>/index.db)
  SCREE_MODEL         embedding model (default openai/text-embedding-3-small)
  SCREE_DIMENSIONS    embedding size, 0 for the model default (default 512)
  SCREE_EMBED_URL     embeddings endpoint (default OpenRouter)
  SCREE_MAX_FILE_BYTES  largest note to index, in bytes (default 1048576)
  SCREE_ZDR           1 asks OpenRouter for zero-data-retention endpoints
                      only; 0 for other endpoints (default 1)
  OPENROUTER_API_KEY  API key for the endpoint
  SCREE_HTTP_TOKEN    bearer token for serve -http; required with
                      -allow-remote, enforced on loopback when set
`

type config struct {
	notes, db, model, url, apiKey string
	dims                          int
	maxBytes                      int64
	zdr, gitignore                bool
	serve                         bool         // the folder defaults to CLAUDE_PROJECT_DIR
	doctor                        bool         // doctor reports the SCREE_NOTES warning itself
	source                        folderSource // set by open
	dbExplicit                    bool         // -db or SCREE_DB was given; set by open
}

func main() {
	log.SetFlags(log.LstdFlags)
	// Log lines can carry note paths and API messages; keep them terminal-safe.
	log.SetOutput(textsafe.Writer(os.Stderr))
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
		fmt.Fprintln(os.Stderr, "scree:", textsafe.Strip(err.Error()))
		os.Exit(1)
	}
}

func flags(name string, cfg *config) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.StringVar(&cfg.notes, "notes", "", "folder to index (default: see usage)")
	fs.StringVar(&cfg.db, "db", os.Getenv("SCREE_DB"), "index file (default: one per folder under $XDG_DATA_HOME/scree/indexes)")
	fs.BoolVar(&cfg.gitignore, "gitignore", true, "honor .gitignore, .git/info/exclude and core.excludesFile")
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

// maxFileBytes reads SCREE_MAX_FILE_BYTES.
func maxFileBytes() (int64, error) {
	v := os.Getenv("SCREE_MAX_FILE_BYTES")
	if v == "" {
		return policy.DefaultMaxBytes, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("SCREE_MAX_FILE_BYTES=%q: want a positive number of bytes", v)
	}
	return n, nil
}

// zdrSetting reads SCREE_ZDR.
func zdrSetting() (bool, error) {
	v := os.Getenv("SCREE_ZDR")
	if v == "" {
		return true, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("SCREE_ZDR=%q: want 1 or 0", v)
	}
	return b, nil
}

// open builds an indexer from cfg. checkModel guards against mixing vectors
// from different models. Release it with closeIndexer.
func open(cfg *config, checkModel bool) (*index.Indexer, error) {
	if os.Getenv("SCREE_NOTES") != "" && !cfg.doctor {
		log.Print(notesWarning)
	}
	var err error
	if cfg.maxBytes, err = maxFileBytes(); err != nil {
		return nil, err
	}
	if cfg.zdr, err = zdrSetting(); err != nil {
		return nil, err
	}
	root, src, err := resolveFolder(cfg.notes, cfg.serve)
	if err != nil {
		return nil, err
	}
	cfg.source = src
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("folder %s is not a directory", root)
	}
	p, err := policy.OpenOptions(root, policy.Options{MaxBytes: cfg.maxBytes, GitIgnore: cfg.gitignore})
	if err != nil {
		return nil, err
	}
	cfg.dbExplicit = cfg.db != ""
	if !cfg.dbExplicit {
		cfg.db = filepath.Join(indexDir(p.Root()), "index.db")
	}
	s, err := store.Open(cfg.db)
	if err != nil {
		p.Close()
		return nil, err
	}
	if err := s.CheckRoot(p.Root()); err != nil {
		s.Close()
		p.Close()
		return nil, err
	}
	if !cfg.dbExplicit {
		if err := writeRootFile(filepath.Dir(cfg.db), p.Root()); err != nil {
			s.Close()
			p.Close()
			return nil, fmt.Errorf("index %s: %w", cfg.db, err)
		}
	}
	e := embed.New(cfg.url, cfg.apiKey, cfg.model, cfg.dims)
	e.ZDR = cfg.zdr
	// Queries fail fast: someone is waiting on the answer.
	q := *e
	q.Timeout, q.MaxAttempts = 10*time.Second, 2
	if checkModel {
		if err := s.CheckModel(e.Model()); err != nil {
			s.Close()
			p.Close()
			return nil, err
		}
	}
	return &index.Indexer{Policy: p, Store: s, Embedder: e, QueryEmbedder: &q, Log: log.Default()}, nil
}

// notesWarning is shown when the environment still sets SCREE_NOTES.
const notesWarning = "SCREE_NOTES is no longer read; remove it from the settings file (the folder is now -notes or the git root)"

func closeIndexer(ix *index.Indexer) {
	ix.Store.Close()
	ix.Policy.Close()
}

func runWatch(ctx context.Context, args []string) error {
	var cfg config
	flags("watch", &cfg).Parse(args)
	ix, err := open(&cfg, true)
	if err != nil {
		return err
	}
	defer closeIndexer(ix)
	log.Printf("syncing %s", ix.Root())
	st, err := ix.Sync(ctx)
	if err != nil {
		return err
	}
	log.Printf("sync done: %d notes, %d indexed, %d removed, %d failed, %d deferred",
		st.Scanned, st.Indexed, st.Removed, st.Failed, st.Deferred)
	log.Printf("watching %s", ix.Root())
	return watch.Run(ctx, ix)
}

func runServe(ctx context.Context, args []string) error {
	cfg := config{serve: true}
	fs := flags("serve", &cfg)
	addr := fs.String("http", "", "serve streamable HTTP on this address instead of stdio, e.g. 127.0.0.1:7077")
	allowRemote := fs.Bool("allow-remote", false, "allow -http on a non-loopback address (needs SCREE_HTTP_TOKEN)")
	fs.Parse(args)
	// stdout belongs to the MCP protocol.
	log.SetOutput(textsafe.Writer(os.Stderr))
	token := os.Getenv("SCREE_HTTP_TOKEN")
	if *addr != "" {
		if err := mcpserver.CheckListenAddr(*addr, *allowRemote, token); err != nil {
			return err
		}
	}
	ix, err := open(&cfg, true)
	if err != nil {
		return err
	}
	defer closeIndexer(ix)
	log.Printf("serving %s (index %s)", ix.Root(), cfg.db)
	srv := mcpserver.New(ix, version)
	if *addr == "" {
		return srv.Run(ctx, &mcp.StdioTransport{})
	}
	hs := mcpserver.NewHTTPServer(*addr, mcpserver.HTTPHandler(srv, token))
	go func() {
		<-ctx.Done()
		hs.Close()
	}()
	auth := "no token"
	if token != "" {
		auth = "bearer token required"
	}
	log.Printf("serving MCP on http://%s (%s)", *addr, auth)
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
	defer closeIndexer(ix)
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
	defer closeIndexer(ix)
	if *all {
		if err := ix.Store.Reset(ctx); err != nil {
			return err
		}
		if err := ix.Store.CheckModel(ix.Embedder.Model()); err != nil {
			return err
		}
		if err := ix.Store.CheckRoot(ix.Root()); err != nil {
			return err
		}
	}
	if fs.NArg() == 1 {
		return reindexOne(ctx, ix, fs.Arg(0))
	}
	st, err := ix.Sync(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("%d notes: %d indexed, %d removed, %d failed, %d deferred\n",
		st.Scanned, st.Indexed, st.Removed, st.Failed, st.Deferred)
	if st.Failed+st.Deferred > 0 {
		return fmt.Errorf("%d notes failed and %d were deferred; run reindex again", st.Failed, st.Deferred)
	}
	return nil
}

// reindexOne indexes one note, given relative to the notes folder or as an
// absolute path under it. A refusal is an error.
func reindexOne(ctx context.Context, ix *index.Indexer, arg string) error {
	rel := filepath.ToSlash(arg)
	if filepath.IsAbs(arg) {
		var err error
		if rel, err = ix.Rel(arg); err != nil {
			return err
		}
	}
	changed, err := ix.IndexFile(ctx, rel)
	if err != nil {
		return err
	}
	if changed {
		fmt.Printf("re-indexed %q\n", rel)
	} else {
		fmt.Printf("%q is already up to date\n", rel)
	}
	return nil
}

func runDoctor(ctx context.Context, args []string) error {
	cfg := config{doctor: true}
	fs := flags("doctor", &cfg)
	offline := fs.Bool("offline", false, "skip the test call to the embeddings API")
	verbose := fs.Bool("v", false, "list every ignored or unreadable path")
	fs.Parse(args)

	ok := true
	report := func(good bool, format string, a ...any) {
		mark := "ok  "
		if !good {
			mark, ok = "FAIL", false
		}
		fmt.Printf("%s %s\n", mark, fmt.Sprintf(format, a...))
	}
	warn := func(format string, a ...any) {
		fmt.Printf("warn %s\n", fmt.Sprintf(format, a...))
	}

	if os.Getenv("SCREE_NOTES") != "" {
		warn("%s", notesWarning)
	}
	ix, err := open(&cfg, false)
	if err != nil {
		report(false, "%v", err)
		return errors.New("doctor found problems")
	}
	defer closeIndexer(ix)
	report(true, "folder %s (from %s)", ix.Root(), cfg.source)
	doctorScan(ctx, ix, cfg.gitignore, *verbose, report, warn)

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
	if cfg.zdr {
		report(true, "zero data retention requested (provider.zdr=true, data_collection=deny)")
		if u, err := url.Parse(cfg.url); err != nil || u.Hostname() != "openrouter.ai" {
			warn("SCREE_EMBED_URL is not OpenRouter; set SCREE_ZDR=0 if it rejects the provider field")
		}
	} else {
		warn("zero data retention off (SCREE_ZDR=0)")
	}
	if !*offline && cfg.apiKey != "" {
		vecs, err := ix.Embedder.Embed(ctx, []string{"scree doctor"})
		switch {
		case errors.Is(err, embed.ErrNoZDREndpoint):
			report(false, "%v", err)
		case err != nil:
			report(false, "embeddings API: %v", err)
		default:
			report(true, "embeddings API returned %d dimensions", len(vecs[0]))
		}
	}
	if !ok {
		return errors.New("doctor found problems")
	}
	return nil
}

// doctorScan reports what the ignore layers do to the notes folder.
func doctorScan(ctx context.Context, ix *index.Indexer, gitignore, verbose bool, report func(bool, string, ...any), warn func(string, ...any)) {
	sum, err := ix.Policy.Scan(ctx)
	if err != nil {
		report(false, "notes: %v (nothing will be indexed)", err)
		return
	}
	n := func(r policy.Reason) int { return len(sum.Ignored[r]) }
	report(true, "notes: %d indexable", sum.Allowed)
	report(true, "built-in: %d hidden, %d not markdown, %d outside notes folder, %d too large, %d unsafe name",
		n(policy.Hidden), n(policy.NotMarkdown), n(policy.OutsideRoot), n(policy.TooLarge), n(policy.UnsafeName))
	switch {
	case ix.Policy.GitTop() != "":
		nested := "nested repositories"
		if n(policy.NestedRepo) == 1 {
			nested = "nested repository"
		}
		report(true, "%s: %d (%d %s)", policy.GitIgnore.Layer(), n(policy.GitIgnore), n(policy.NestedRepo), nested)
	case !gitignore:
		report(true, "%s: off (-gitignore=false)", policy.GitIgnore.Layer())
	default:
		report(true, "%s: not a git work tree", policy.GitIgnore.Layer())
	}
	report(true, "%s: %d", policy.IgnoreFile.Layer(), n(policy.IgnoreFile))
	report(true, "%s: %d", policy.FrontMatter.Layer(), n(policy.FrontMatter))
	for _, e := range sum.IgnoreErrors {
		// A bad .screeignore line is scree's own config; a bad pattern in a
		// .gitignore or git excludes file is only worth a warning.
		if strings.HasPrefix(e, policy.IgnoreFileName+" ") {
			report(false, "%s", e)
		} else {
			warn("%s", e)
		}
	}
	if len(sum.ReadErrors) > 0 {
		warn("unreadable: %d (doctor -v lists them)", len(sum.ReadErrors))
	}
	if !verbose {
		return
	}
	reasons := make([]policy.Reason, 0, len(sum.Ignored))
	for r := range sum.Ignored {
		reasons = append(reasons, r)
	}
	slices.Sort(reasons)
	for _, r := range reasons {
		for _, p := range sum.Ignored[r] {
			fmt.Printf("  ignored (%s): %s\n", r, strconv.Quote(p))
		}
	}
	for _, e := range sum.ReadErrors {
		fmt.Printf("  unreadable: %s\n", e)
	}
}
