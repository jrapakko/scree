// Command scree is local semantic search over the markdown in your repos and
// notes, served to Claude Code over MCP.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
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
	"github.com/jrapakko/scree/internal/lock"
	"github.com/jrapakko/scree/internal/mcpserver"
	"github.com/jrapakko/scree/internal/policy"
	"github.com/jrapakko/scree/internal/store"
	"github.com/jrapakko/scree/internal/textsafe"
	"github.com/jrapakko/scree/internal/writer"
)

var version = "dev"

const usage = `scree: semantic search over the markdown in a folder

Usage:
  scree mcp [-docker]      run serve for Claude Code (what .mcp.json names),
                           with settings from ~/.config/scree/env and nothing
                           else from the environment; -docker runs it in a
                           container
  scree run <command> [args]
                           run another command the same way, from a terminal
  scree watch              index changed files, then keep the index current
  scree serve [-http addr [-allow-remote]]
                           run the MCP server (stdio unless -http is given;
                           -http is loopback only unless -allow-remote)
  scree search [-k n] [-mode auto|hybrid|semantic|keyword] [-format xml|json] "query"
                           auto (default) is semantic, plus keyword search
                           when the query holds an identifier such as FOO_BAR
                           or a name from the folder's code;
                           keyword makes no API call; -format json prints one
                           object per hit, the stable format for evals
  scree reindex [-all] [path]
  scree doctor [-v] [-offline]
                           check configuration, the index and ignore rules
  scree indexes [-prune]   list every per-folder index with its folder, size
                           and last sync; -prune deletes those whose folder
                           is gone

Every command but indexes takes -notes, -db and -gitignore. The folder is
-notes if given, otherwise the git root of the current directory, or the
current directory outside git. serve needs -notes outside Claude Code. Under
Claude Code (CLAUDE_PROJECT_DIR set), serve refuses -notes, -db and SCREE_DB,
requires CLAUDE_PROJECT_DIR to be in the same repo as its working directory,
and indexes only a repo whose .mcp.json names scree mcp. ~ in
~/.config/scree/env is the home directory in the password database, not
$HOME. Environment:
  SCREE_DB            index file (default $XDG_DATA_HOME/scree/indexes/<folder>-<hash>/index.db)
  SCREE_MODEL         embedding model (default openai/text-embedding-3-small)
  SCREE_DIMENSIONS    embedding size, 0 for the model default (default 512)
  SCREE_EMBED_URL     embeddings endpoint (default OpenRouter)
  SCREE_MAX_FILE_BYTES  largest file to index, in bytes (default 1048576)
  SCREE_FIRST_SYNC_MB a Claude Code session or watch does not start the
                      first index of a folder with more markdown than this
                      (default 50; 0 for no limit); reindex always does
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
	firstSyncMax                  int64 // SCREE_FIRST_SYNC_MB in bytes; 0 means no cap
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
	go exitOnSecondSignal(ctx, stop)

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
	case "indexes":
		err = runIndexes(ctx, args)
	case "mcp":
		err = runMCP(args)
	case "run":
		err = runRun(args)
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

// exitOnSecondSignal waits for the first SIGINT or SIGTERM, which cancels
// ctx and starts a clean shutdown, then exits at once on a second one.
func exitOnSecondSignal(ctx context.Context, stop context.CancelFunc) {
	<-ctx.Done()
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	stop()
	<-c
	fmt.Fprintln(os.Stderr, "scree: second signal; exiting now")
	os.Exit(1)
}

// ShutdownTimeout bounds how long serve and watch wait for the writer to
// stop after the client goes away or a signal arrives. A writer stuck in a
// file read or an API call then cannot keep the process alive.
var ShutdownTimeout = 5 * time.Second

// runWriter runs the writer role; tests replace it.
var runWriter = func(ctx context.Context, w *writer.Writer) error { return w.Run(ctx) }

// startWriter runs w in the background. stop cancels it and waits at most
// ShutdownTimeout; it reports whether the writer stopped, and only then may
// the index be closed.
func startWriter(ctx context.Context, w *writer.Writer, onErr func(error)) (done <-chan error, stop func() bool) {
	wctx, cancel := context.WithCancel(ctx)
	ch := make(chan error, 1)
	run := runWriter
	go func() {
		err := run(wctx, w)
		if err != nil && onErr != nil {
			onErr(err)
		}
		ch <- err
		close(ch)
	}()
	return ch, func() bool {
		cancel()
		select {
		case <-ch:
			return true
		case <-time.After(ShutdownTimeout):
			log.Printf("writer did not stop within %s; exiting", ShutdownTimeout)
			return false
		}
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

// defaultFirstSyncMB is the default SCREE_FIRST_SYNC_MB.
const defaultFirstSyncMB = 50

// firstSyncMax reads SCREE_FIRST_SYNC_MB, in whole megabytes, and returns it
// in bytes. 0 means no cap.
func firstSyncMax() (int64, error) {
	v := os.Getenv("SCREE_FIRST_SYNC_MB")
	if v == "" {
		return defaultFirstSyncMB << 20, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 || n > 1<<30 {
		return 0, fmt.Errorf("SCREE_FIRST_SYNC_MB=%q: want a whole number of megabytes, 0 for no limit", v)
	}
	return n << 20, nil
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

// open builds an indexer from cfg. It only reads the index: a recorded folder
// or model that differs is an error (checkModel guards against mixing
// vectors from different models), and recording them is left to the
// process that holds the writer lock (claim). For doctor, a missing index is
// not created, and the Indexer's Store is nil. Release it with closeIndexer.
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
	if cfg.firstSyncMax, err = firstSyncMax(); err != nil {
		return nil, err
	}
	root, src, err := resolveFolder(cfg.notes, cfg.serve)
	if err != nil {
		return nil, err
	}
	cfg.source = src
	if cfg.serve && src == claudeCode {
		if err := checkOptIn(root); err != nil {
			return nil, err
		}
	}
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
		if err := checkIndexDirs(filepath.Dir(cfg.db)); err != nil {
			p.Close()
			return nil, err
		}
	}
	e := embed.New(cfg.url, cfg.apiKey, cfg.model, cfg.dims)
	e.ZDR = cfg.zdr
	// Queries fail fast: someone is waiting on the answer.
	q := *e
	q.Timeout, q.MaxAttempts = 10*time.Second, 2
	ix := &index.Indexer{Policy: p, Embedder: e, QueryEmbedder: &q, Log: log.Default()}
	if cfg.doctor {
		if _, err := os.Stat(cfg.db); errors.Is(err, fs.ErrNotExist) {
			return ix, nil
		}
	}
	s, err := store.Open(cfg.db)
	if err != nil {
		p.Close()
		return nil, err
	}
	ix.Store = s
	err = s.MatchRoot(p.Root())
	if err == nil && checkModel {
		err = s.MatchModel(e.Model())
	}
	if err != nil {
		closeIndexer(ix)
		return nil, err
	}
	return ix, nil
}

// lockPath is the writer lock beside the index file.
func lockPath(cfg *config) string { return filepath.Join(filepath.Dir(cfg.db), "writer.lock") }

// claim does the writes only the lock holder may do: it records the folder
// (in the index and, for the default location, in the root file beside it)
// and, with model set, the embedding model.
func claim(cfg *config, ix *index.Indexer, model bool) error {
	if err := ix.Store.CheckRoot(ix.Root()); err != nil {
		return err
	}
	if !cfg.dbExplicit {
		if err := writeRootFile(filepath.Dir(cfg.db), ix.Root()); err != nil {
			return fmt.Errorf("index %s: %w", cfg.db, err)
		}
	}
	if model {
		return ix.Store.CheckModel(ix.Embedder.Model())
	}
	return nil
}

// newWriter builds the writer role for serve and watch.
func newWriter(cfg *config, ix *index.Indexer) *writer.Writer {
	w := &writer.Writer{
		Indexer:      ix,
		LockPath:     lockPath(cfg),
		FirstSyncMax: cfg.firstSyncMax,
		OnAcquire:    func(context.Context) error { return claim(cfg, ix, true) },
	}
	w.Attach()
	return w
}

// notesWarning is shown when the environment still sets SCREE_NOTES.
const notesWarning = "SCREE_NOTES is no longer read; remove it from the settings file (the folder is now -notes or the git root)"

// sizeLine describes bytes of markdown and the cost to embed it in full, at
// about four bytes per token. A price is given only for costModel.
func sizeLine(bytes int64, model string) string {
	tokens := float64(bytes) / 4
	var t string
	switch {
	case tokens >= 1e6:
		t = fmt.Sprintf("%.1fM", tokens/1e6)
	case tokens >= 1e3:
		t = fmt.Sprintf("%.0fk", tokens/1e3)
	default:
		t = fmt.Sprintf("%.0f", tokens)
	}
	line := fmt.Sprintf("size: %.1f MB of markdown, about %s tokens to embed in full", float64(bytes)/(1<<20), t)
	if model == costModel {
		cost := tokens / 1e6 * costPerMTokens
		price := fmt.Sprintf("about $%.2f", cost)
		if cost < 0.01 {
			price = "under $0.01"
		}
		line += fmt.Sprintf(" (%s with %s)", price, costModel)
	}
	return line
}

// indexEmpty reports whether the index holds no files yet (or does not
// exist).
func indexEmpty(ctx context.Context, ix *index.Indexer) bool {
	if ix.Store == nil {
		return true
	}
	st, err := ix.Store.Stats(ctx)
	return err == nil && st.Files == 0
}

func closeIndexer(ix *index.Indexer) {
	if ix.Store != nil {
		ix.Store.Close()
	}
	ix.Policy.Close()
}

func runWatch(ctx context.Context, args []string) error {
	var cfg config
	flags("watch", &cfg).Parse(args)
	ix, err := open(&cfg, true)
	if err != nil {
		return err
	}
	// Waits for the lock if another process holds it, then syncs and
	// watches. An error exits non-zero, so systemd or Docker restarts it.
	done, stop := startWriter(ctx, newWriter(&cfg, ix), nil)
	select {
	case err := <-done:
		closeIndexer(ix)
		return err
	case <-ctx.Done():
	}
	if stop() {
		closeIndexer(ix)
	}
	return nil
}

func runServe(ctx context.Context, args []string) error {
	cfg := config{serve: true}
	fs := flags("serve", &cfg)
	addr := fs.String("http", "", "serve streamable HTTP on this address instead of stdio, e.g. 127.0.0.1:7077")
	allowRemote := fs.Bool("allow-remote", false, "allow -http on a non-loopback address (needs SCREE_HTTP_TOKEN)")
	fs.Parse(args)
	// stdout belongs to the MCP protocol.
	log.SetOutput(textsafe.Writer(os.Stderr))
	if err := checkServeFolder(fs); err != nil {
		return err
	}
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
	log.Printf("serving %s (index %s)", ix.Root(), cfg.db)
	// The writer role runs in the background, so the MCP handshake and
	// searches never wait for the lock or the first sync. On the way out it
	// gets ShutdownTimeout to stop; the index is closed only if it did, and
	// otherwise the process exits with it still running.
	w := newWriter(&cfg, ix)
	_, stopWriter := startWriter(ctx, w, func(err error) {
		log.Printf("writer: %v; serving what is indexed", err)
	})
	defer func() {
		if stopWriter() {
			closeIndexer(ix)
		}
	}()
	srv := mcpserver.New(ix, w, version)
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

// errServeFolder refuses -notes, -db and SCREE_DB under Claude Code.
var errServeFolder = errors.New("serve: -notes, -db and SCREE_DB are for the command line; " +
	"under Claude Code (CLAUDE_PROJECT_DIR is set) scree indexes the repo it runs in")

// checkServeFolder enforces that a Claude Code session's folder and index
// follow from the repo alone: with CLAUDE_PROJECT_DIR set, serve refuses
// -notes, -db and SCREE_DB, whoever passed them. Without it (a container, a
// terminal, another client) they work as on the command line.
func checkServeFolder(fs *flag.FlagSet) error {
	if os.Getenv("CLAUDE_PROJECT_DIR") == "" {
		return nil
	}
	refused := os.Getenv("SCREE_DB") != ""
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "notes" || f.Name == "db" {
			refused = true
		}
	})
	if refused {
		return errServeFolder
	}
	return nil
}

func runSearch(ctx context.Context, args []string) error {
	var cfg config
	fs := flags("search", &cfg)
	k := fs.Int("k", 8, "number of results")
	modeFlag := fs.String("mode", "auto", "auto (semantic, plus keyword for identifier-like queries or names from the folder's code), hybrid, semantic or keyword (no API call)")
	format := fs.String("format", "xml", "xml, or json for one object per hit")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New(`usage: scree search [-k n] [-mode auto|hybrid|semantic|keyword] [-format xml|json] "query"`)
	}
	mode, err := index.ParseMode(*modeFlag)
	if err != nil {
		return err
	}
	if *format != "xml" && *format != "json" {
		return fmt.Errorf("format must be xml or json, not %q", *format)
	}
	ix, err := open(&cfg, true)
	if err != nil {
		return err
	}
	defer closeIndexer(ix)
	used := ix.Resolve(ctx, mode, fs.Arg(0))
	hits, err := ix.Search(ctx, fs.Arg(0), *k, used)
	if err != nil {
		return err
	}
	if *format == "json" {
		return mcpserver.WriteJSON(os.Stdout, hits, used)
	}
	note := ""
	if mode == index.Auto {
		note = mcpserver.ModeNote(used)
	}
	fmt.Println(mcpserver.FormatHits(hits, note))
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
	l, pid, err := lock.TryLock(lockPath(&cfg))
	if errors.Is(err, lock.ErrHeld) {
		if *all {
			return fmt.Errorf("index is in use by pid %d (a Claude Code session or scree watch); close it, then run reindex -all", pid)
		}
		if fs.NArg() == 1 {
			// Refuse an ignored path as the MCP tool does, before saying
			// another process has it covered.
			rel, err := argRel(ix, fs.Arg(0))
			if err != nil {
				return err
			}
			if _, err := ix.Policy.Read(rel); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		fmt.Printf("pid %d keeps this index current; nothing to do\n", pid)
		return nil
	}
	if err != nil {
		return err
	}
	defer l.Release()
	if err := claim(&cfg, ix, !*all); err != nil {
		return err
	}
	if *all {
		if err := ix.Store.Reset(ctx); err != nil {
			return err
		}
		if err := claim(&cfg, ix, true); err != nil {
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
	if err := writer.MarkSynced(ix.Store); err != nil {
		return err
	}
	fmt.Printf("%d files: %d indexed, %d removed, %d failed, %d deferred\n",
		st.Scanned, st.Indexed, st.Removed, st.Failed, st.Deferred)
	if st.Failed+st.Deferred > 0 {
		return fmt.Errorf("%d files failed and %d were deferred; run reindex again", st.Failed, st.Deferred)
	}
	return nil
}

// reindexOne indexes one note, given relative to the notes folder or as an
// absolute path under it. A refusal is an error.
func reindexOne(ctx context.Context, ix *index.Indexer, arg string) error {
	rel, err := argRel(ix, arg)
	if err != nil {
		return err
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

// argRel maps a path argument, relative to the folder or absolute under it,
// to its canonical store key.
func argRel(ix *index.Indexer, arg string) (string, error) {
	rel := filepath.ToSlash(arg)
	if filepath.IsAbs(arg) {
		var err error
		if rel, err = ix.Rel(arg); err != nil {
			return "", err
		}
	}
	return ix.Policy.Canonical(rel)
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
	doctorScan(ctx, ix, &cfg, *verbose, report, warn)

	// doctor only reads: it never creates the index or records anything in
	// it; the process holding the writer lock does that.
	want := ix.Embedder.Model()
	have := ""
	if ix.Store == nil {
		report(true, "index %s: not created yet (the first serve, watch or reindex builds it)", cfg.db)
	} else {
		st, err := ix.Store.Stats(ctx)
		report(err == nil, "index %s: %d files, %d chunks (vec1 %s)", cfg.db, st.Files, st.Chunks, st.Vec1Info)
		doctorKeyword(ctx, ix.Store, st.Chunks, *verbose, report)
		doctorVocab(ctx, ix.Store, *verbose, report)
		have, _ = ix.Store.Model()
		synced, _ := ix.Store.Meta(writer.SyncedAtKey)
		if synced == "" {
			synced = "never"
		}
		report(true, "last full sync: %s", synced)
	}
	switch held, pid, err := lock.Probe(lockPath(&cfg)); {
	case err != nil:
		warn("writer lock: %v", err)
	case held:
		report(true, "writer: pid %d keeps this index current", pid)
	default:
		report(true, "writer: none running (a Claude Code session or scree watch takes the role)")
	}
	switch {
	case have == "":
		report(true, "model %s (index empty)", want)
	case have == want:
		report(true, "model %s", want)
	default:
		report(false, "index built with %s but configured for %s; run `scree reindex -all`", have, want)
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

// doctorKeyword reports the keyword index: its row count must equal chunks,
// and with verbose, FTS5's integrity check (which scans everything) must
// pass.
func doctorKeyword(ctx context.Context, s *store.Store, chunks int, verbose bool, report func(bool, string, ...any)) {
	n, err := s.KeywordRows(ctx)
	switch {
	case err != nil:
		report(false, "keyword index: %v", err)
		return
	case n != chunks:
		report(false, "keyword index: %d rows for %d chunks; run `scree reindex -all`", n, chunks)
		return
	}
	report(true, "keyword index: %d chunks", n)
	if verbose {
		if err := s.CheckKeywordIndex(ctx); err != nil {
			report(false, "keyword index integrity check: %v; run `scree reindex -all`", err)
		} else {
			report(true, "keyword index integrity check")
		}
	}
}

// doctorVocab reports the code vocabulary auto mode routes on, and with
// verbose, up to 20 of its names.
func doctorVocab(ctx context.Context, s *store.Store, verbose bool, report func(bool, string, ...any)) {
	names, files, some, err := s.VocabStats(ctx, 20)
	if err != nil {
		report(false, "code vocabulary: %v", err)
		return
	}
	report(true, "code vocabulary: %d names from %d manifests and source files (names only; never embedded)", names, files)
	if verbose && len(some) > 0 {
		q := make([]string, len(some))
		for i, n := range some {
			q[i] = strconv.Quote(n)
		}
		fmt.Printf("  vocabulary sample: %s\n", strings.Join(q, " "))
	}
}

// costModel is the model doctor prices, at costPerMTokens dollars per
// million tokens.
const (
	costModel      = "openai/text-embedding-3-small"
	costPerMTokens = 0.02
)

// doctorScan reports what the ignore layers do to the folder, its size and
// the cost to embed it.
func doctorScan(ctx context.Context, ix *index.Indexer, cfg *config, verbose bool, report func(bool, string, ...any), warn func(string, ...any)) {
	gitignore := cfg.gitignore
	sum, err := ix.Policy.Scan(ctx)
	if err != nil {
		report(false, "files: %v (nothing will be indexed)", err)
		return
	}
	n := func(r policy.Reason) int { return len(sum.Ignored[r]) }
	report(true, "files: %d indexable", sum.Allowed)
	report(true, "%s", sizeLine(sum.Bytes, cfg.model))
	if cfg.firstSyncMax > 0 && sum.Bytes > cfg.firstSyncMax && indexEmpty(ctx, ix) {
		warn("over SCREE_FIRST_SYNC_MB: Claude Code sessions will not index this folder until you run scree reindex")
	}
	defaults, extra := ix.Policy.HiddenFolders()
	hidden := "none"
	if len(defaults) > 0 {
		hidden = strings.Join(defaults, "/ ") + "/"
	}
	if len(extra) > 0 {
		more := "(+ " + strings.Join(extra, "/ ") + "/ from " + policy.IgnoreFileName + ")"
		if hidden == "none" {
			hidden = more
		} else {
			hidden += " " + more
		}
	}
	report(true, "hidden folders indexed: %s", hidden)
	report(true, "built-in: %d hidden, %d not markdown, %d outside the folder, %d too large, %d unsafe name",
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
	report(true, "%s: %d ignored, %d re-included by \"!\"", policy.IgnoreFile.Layer(), n(policy.IgnoreFile), len(sum.Reincluded))
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
	for _, e := range sum.Reincluded {
		fmt.Printf("  re-included: %s\n", e)
	}
}
