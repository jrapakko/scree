// Package index keeps the store in step with a folder of markdown notes.
package index

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jrapakko/scree/internal/chunk"
	"github.com/jrapakko/scree/internal/embed"
	"github.com/jrapakko/scree/internal/policy"
	"github.com/jrapakko/scree/internal/store"
	"github.com/jrapakko/scree/internal/vocab"
)

// Indexer embeds the notes its Policy allows into Store.
type Indexer struct {
	Policy   *policy.Policy
	Store    *store.Store
	Embedder embed.Embedder
	// QueryEmbedder embeds search queries; nil means Embedder.
	QueryEmbedder embed.Embedder
	Log           *log.Logger
	// MinBackoff is the first pause after the embedding API is unavailable;
	// each further failure doubles it, up to MaxBackoff. 0 means 30 s.
	MinBackoff time.Duration
	// OnStale, when set, receives the paths whose hits Search dropped (the
	// policy now refuses them, or they are gone) or found changed since they
	// were indexed, so the process that writes the index can re-index them.
	// Search never re-embeds itself. Nil means leave them; the writer's
	// watcher or next sync catches up.
	OnStale func(paths []string)
	// Progress, when set, is called after each file Sync handles, with the
	// counts so far.
	Progress func(SyncStats)

	// writeMu serializes IndexFile, Sync and RemovePrefix, which can be
	// called from the watcher and from the MCP reindex tool.
	writeMu sync.Mutex

	mu         sync.Mutex
	pauseUntil time.Time
	backoff    time.Duration
}

// MaxBackoff caps the pause after repeated API outages.
const MaxBackoff = 5 * time.Minute

// ErrPaused is returned instead of calling the API while indexing is paused
// after an outage.
var ErrPaused = fmt.Errorf("%w: paused after a recent failure", embed.ErrUnavailable)

// PausedUntil returns when indexing may call the API again, or the zero time
// if it is not paused.
func (ix *Indexer) PausedUntil() time.Time {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.pauseUntil
}

// embed calls the API unless paused, and updates the pause from the result.
func (ix *Indexer) embed(ctx context.Context, inputs []string) ([][]float32, error) {
	ix.mu.Lock()
	paused := time.Now().Before(ix.pauseUntil)
	ix.mu.Unlock()
	if paused {
		return nil, ErrPaused
	}
	vecs, err := ix.Embedder.Embed(ctx, inputs)
	ix.mu.Lock()
	defer ix.mu.Unlock()
	switch {
	case err == nil:
		ix.backoff, ix.pauseUntil = 0, time.Time{}
	case errors.Is(err, embed.ErrUnavailable):
		if ix.backoff == 0 {
			ix.backoff = ix.MinBackoff
			if ix.backoff <= 0 {
				ix.backoff = 30 * time.Second
			}
		} else {
			ix.backoff *= 2
		}
		ix.backoff = min(ix.backoff, MaxBackoff)
		ix.pauseUntil = time.Now().Add(ix.backoff)
	}
	return vecs, err
}

// Root returns the notes folder, with symlinks resolved.
func (ix *Indexer) Root() string { return ix.Policy.Root() }

// Rel converts an absolute path under the notes folder to the
// slash-separated form used as a key in the store. It fails for paths
// outside the folder.
func (ix *Indexer) Rel(abs string) (string, error) { return ix.Policy.Rel(abs) }

// IndexFile embeds rel if its content changed since it was last indexed. It
// removes rel from the index if the file no longer exists. If the policy
// refuses rel, any rows for it are removed and the *policy.IgnoredError is
// returned, with changed reporting whether rows were removed. rel is first
// mapped to its on-disk name (Policy.Canonical), which is the store key.
func (ix *Indexer) IndexFile(ctx context.Context, rel string) (changed bool, err error) {
	return ix.IndexFileCanon(ctx, ix.Policy.NewCanon(), rel)
}

// IndexFileCanon is IndexFile with the folder listings of the batch c, for
// the watcher, which handles many paths at a time.
func (ix *Indexer) IndexFileCanon(ctx context.Context, c *policy.Canon, rel string) (changed bool, err error) {
	ix.writeMu.Lock()
	defer ix.writeMu.Unlock()
	if err := ix.Policy.CheckIgnoreFile(); err != nil {
		return false, err
	}
	clean, err := c.Canonical(rel)
	if err == nil {
		var src []byte
		var fi fs.FileInfo
		src, fi, err = c.ReadStat(clean)
		switch {
		case err == nil:
			return ix.indexSrc(ctx, clean, src, fi)
		case errors.Is(err, fs.ErrNotExist):
			return ix.remove(ctx, clean)
		}
	}
	var ie *policy.IgnoredError
	if errors.As(err, &ie) && ie.Reason != policy.Invalid {
		removed, rerr := ix.remove(ctx, ie.Path)
		if rerr != nil {
			return removed, rerr
		}
		return removed, err
	}
	return false, err
}

// indexSrc stores src as the content of rel, embedding it only if its hash
// changed. fi is the file as opened; its mtime and size are recorded so
// search can tell when the file changed since.
func (ix *Indexer) indexSrc(ctx context.Context, rel string, src []byte, fi fs.FileInfo) (bool, error) {
	if !policy.IsMarkdown(rel) {
		return false, fmt.Errorf("%q: not markdown; never embedded", rel)
	}
	sum := sha256.Sum256(src)
	hash := hex.EncodeToString(sum[:])
	st := &store.FileStat{Mtime: fi.ModTime().UnixNano(), Size: fi.Size()}
	have, haveSt, err := ix.Store.FileInfo(ctx, rel)
	if err != nil {
		return false, err
	}
	if have == hash {
		if err := ix.backfillLines(ctx, rel, src); err != nil {
			return false, err
		}
		if haveSt == nil || *haveSt != *st {
			return false, ix.Store.SetFileStat(ctx, rel, *st)
		}
		return false, nil
	}

	pieces := chunk.Split(string(src))
	chunks := make([]store.Chunk, len(pieces))
	inputs := make([]string, len(pieces))
	for i, p := range pieces {
		chunks[i] = store.Chunk{Path: rel, Heading: p.Heading, Line: p.Line, EndLine: p.EndLine, Text: p.Text}
		// The path and heading give the model context the text alone may lack.
		inputs[i] = rel + "\n" + p.Heading + "\n\n" + p.Text
	}
	var vecs [][]float32
	if len(inputs) > 0 {
		if vecs, err = ix.embed(ctx, inputs); err != nil {
			return false, fmt.Errorf("%q: %w", rel, err)
		}
	}
	if err := ix.Store.ReplaceFile(ctx, rel, hash, time.Now().Unix(), st, chunks, vecs); err != nil {
		return false, fmt.Errorf("%q: %w", rel, err)
	}
	ix.logf("indexed %q (%d chunks)", rel, len(chunks))
	return true, nil
}

// backfillLines records line ranges for an unchanged file indexed before
// they were stored, without re-embedding: it re-splits src and, if the
// chunks are exactly the stored ones by order, writes their ranges. If the
// chunking differs (an older chunker), the rows are left as they are until
// the next real edit re-embeds the file.
func (ix *Indexer) backfillLines(ctx context.Context, rel string, src []byte) error {
	missing, err := ix.Store.MissingLines(ctx, rel)
	if err != nil || !missing {
		return err
	}
	texts, err := ix.Store.ChunkTexts(ctx, rel)
	if err != nil {
		return err
	}
	pieces := chunk.Split(string(src))
	if len(pieces) != len(texts) {
		return nil
	}
	ranges := make([]store.LineRange, len(pieces))
	for i, p := range pieces {
		if p.Text != texts[i] {
			return nil
		}
		ranges[i] = store.LineRange{Line: p.Line, EndLine: p.EndLine}
	}
	return ix.Store.SetLines(ctx, rel, ranges)
}

func (ix *Indexer) remove(ctx context.Context, rel string) (bool, error) {
	have, err := ix.Store.FileHash(ctx, rel)
	if err != nil || have == "" {
		return false, err
	}
	if err := ix.Store.RemoveFile(ctx, rel); err != nil {
		return false, err
	}
	ix.logf("removed %q", rel)
	return true, nil
}

// RemovePrefix drops every indexed file under the directory rel, for when a
// folder is deleted or moved away.
func (ix *Indexer) RemovePrefix(ctx context.Context, rel string) error {
	ix.writeMu.Lock()
	defer ix.writeMu.Unlock()
	paths, err := ix.Store.Paths(ctx)
	if err != nil {
		return err
	}
	for _, p := range paths {
		if strings.HasPrefix(p, rel+"/") {
			if _, err := ix.remove(ctx, p); err != nil {
				return err
			}
		}
	}
	return nil
}

// SyncStats reports what a Sync did. Deferred counts files left for later
// because the embedding API was unavailable; their old rows are kept. The
// counts are of notes; Sources counts the manifests and source files read
// for the code vocabulary, and SourcesChanged those whose names were
// re-read because they are new or changed.
type SyncStats struct {
	Scanned, Indexed, Removed, Failed, Deferred int
	Sources, SourcesChanged                     int
}

// MaxSourceFiles caps the manifests and source files one Sync reads for the
// code vocabulary, in walk order; files past it are not opened and
// contribute nothing. A file that fails to read does not count.
const MaxSourceFiles = 20000

// cutMark prefixes the stored hash of a file whose names were cut short
// for vocab.MaxNames. Sync keeps its names while the file is unchanged and
// the room left for it is the same as when it was cut, and reads it again
// when either changes, so a capped vocabulary is not rebuilt every sync.
const cutMark = "cut:"

// sourceCap and nameCap are MaxSourceFiles and vocab.MaxNames; tests lower
// them.
var sourceCap, nameCap = MaxSourceFiles, vocab.MaxNames

// Sync indexes every new or changed note the policy allows and drops notes
// that no longer exist or are now ignored. Files are processed one at a
// time. A failure on one file is logged and does not stop the rest, and its
// existing rows are kept. Sync fails, indexing nothing, if .screeignore
// cannot be read or has a bad line.
//
// The same walk rebuilds the code vocabulary (see package vocab) from the
// manifests and source files the policy allows outside hidden folders. A
// file whose hash is unchanged is not parsed again, unless the names were
// read by another vocab.Version. Only names are kept, at most
// vocab.MaxNames in all; code is never embedded or sent anywhere.
func (ix *Indexer) Sync(ctx context.Context) (SyncStats, error) {
	ix.writeMu.Lock()
	defer ix.writeMu.Unlock()
	if err := ix.Policy.CheckIgnoreFile(); err != nil {
		return SyncStats{}, err
	}
	var (
		st   SyncStats
		seen = map[string]bool{}
		keep []string // folders that could not be read: keep their rows

		codeSeen = map[string]bool{}
		put      []store.VocabFile
		opened   int  // source files read, at most sourceCap
		capped   bool // a source file was refused for the cap
		names    int  // names kept so far, at most nameCap
		full     bool // a file's names were cut for nameCap
	)
	if v, err := ix.Store.Meta(store.VocabVersionKey); err != nil {
		return st, err
	} else if v != vocab.Version {
		if err := ix.Store.ClearVocab(ctx, vocab.Version); err != nil {
			return st, err
		}
	}
	logFull := func() {
		if !full {
			full = true
			ix.logf("vocabulary: over %d names; keeping the first", nameCap)
		}
	}
	codeHave, err := ix.Store.VocabFiles(ctx)
	if err != nil {
		return st, err
	}
	// The walk asks before opening each file, so files past the cap are
	// never opened.
	isSource := func(rel string) bool {
		if !vocab.IsSource(rel) {
			return false
		}
		if opened < sourceCap {
			return true
		}
		if !capped {
			capped = true
			ix.logf("vocabulary: over %d source files; not reading the rest", sourceCap)
		}
		return false
	}
	err = ix.Policy.WalkStatWith(ctx, isSource, func(rel string, src []byte, fi fs.FileInfo, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, policy.ErrIgnored) {
			return nil
		}
		if vocab.IsSource(rel) {
			if err != nil {
				// A transient failure keeps the stored names; a file that is
				// gone, unreadable or not a regular file loses them.
				if !permanent(err) {
					codeSeen[rel] = true
					names += codeHave[rel].Names
				}
				return nil
			}
			opened++
			codeSeen[rel] = true
			st.Sources++
			sum := sha256.Sum256(src)
			hash := hex.EncodeToString(sum[:])
			have, ok := codeHave[rel]
			room := max(nameCap-names, 0)
			switch {
			case ok && have.Hash == hash && have.Names <= room:
				names += have.Names
				return nil
			case ok && have.Hash == cutMark+hash && have.Names == room:
				// Cut short before, and the room is the same: the same names.
				logFull()
				names += have.Names
				return nil
			}
			found := vocab.Extract(rel, src)
			if len(found) > room {
				// Cut short: marked, so it is read again only once the room
				// it gets changes.
				found, hash = found[:room], cutMark+hash
				logFull()
			}
			names += len(found)
			put = append(put, store.VocabFile{Path: rel, Hash: hash, Names: found})
			st.SourcesChanged++
			return nil
		}
		// Only markdown is chunked and embedded, whatever the walk passed.
		if !policy.IsMarkdown(rel) && !strings.HasSuffix(rel, "/") {
			return nil
		}
		st.Scanned++
		seen[rel] = true
		if err != nil {
			if strings.HasSuffix(rel, "/") {
				keep = append(keep, rel)
			}
			st.Failed++
			ix.logf("error: %v", err)
			return nil
		}
		changed, err := ix.indexSrc(ctx, rel, src, fi)
		switch {
		case errors.Is(err, embed.ErrUnavailable):
			if st.Deferred == 0 {
				ix.logf("error: %v; deferring files until the API is back", err)
			}
			st.Deferred++
		case err != nil:
			st.Failed++
			ix.logf("error: %v", err)
		case changed:
			st.Indexed++
		}
		if ix.Progress != nil {
			ix.Progress(st)
		}
		return nil
	})
	if err != nil {
		return st, err
	}
	kept := func(p string) bool {
		return slices.ContainsFunc(keep, func(dir string) bool { return strings.HasPrefix(p, dir) })
	}
	paths, err := ix.Store.Paths(ctx)
	if err != nil {
		return st, err
	}
	for _, p := range paths {
		if seen[p] || kept(p) {
			continue
		}
		if _, err := ix.remove(ctx, p); err != nil {
			return st, err
		}
		st.Removed++
	}
	var del []string
	for p := range codeHave {
		if !codeSeen[p] && !kept(p) {
			del = append(del, p)
		}
	}
	if err := ix.Store.UpdateVocab(ctx, put, del); err != nil {
		return st, err
	}
	if len(put)+len(del) > 0 {
		ix.logf("vocabulary: %d source files read, %d removed", len(put), len(del))
	}
	return st, nil
}

// permanent reports whether a read error will recur: the file is gone,
// unreadable or not a regular file.
func permanent(err error) bool {
	return errors.Is(err, policy.ErrNotRegular) || errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission)
}

// Mode selects how Search finds chunks.
type Mode string

const (
	// Auto, the default, is Hybrid when the query holds an identifier-like
	// word (see HasIdentifier) or a word from the code vocabulary (see
	// Indexer.Resolve), and Semantic otherwise.
	Auto Mode = "auto"
	// Hybrid merges vector and keyword search by reciprocal rank fusion.
	Hybrid Mode = "hybrid"
	// Semantic is vector search only.
	Semantic Mode = "semantic"
	// Keyword is FTS5 search only; it makes no embedding call.
	Keyword Mode = "keyword"
)

// ParseMode returns the Mode named s; "" is Auto.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(strings.ToLower(strings.TrimSpace(s))); m {
	case "":
		return Auto, nil
	case Auto, Hybrid, Semantic, Keyword:
		return m, nil
	}
	return "", fmt.Errorf("mode must be auto, hybrid, semantic or keyword, not %q", s)
}

// Resolve returns the mode a search for query runs in, without the code
// vocabulary: for Auto (or ""), Hybrid if HasIdentifier(query) and Semantic
// otherwise; any other mode as it is. Indexer.Resolve also consults the
// vocabulary.
func Resolve(mode Mode, query string) Mode {
	if mode != "" && mode != Auto {
		return mode
	}
	if HasIdentifier(query) {
		return Hybrid
	}
	return Semantic
}

// Resolve returns the mode a search for query runs in: for Auto (or ""),
// Hybrid if HasIdentifier(query) or a word of the query, trimmed as
// HasIdentifier trims it and lowercased, is in the index's code vocabulary,
// and Semantic otherwise; any other mode as it is. The notes need not hold
// the word: hybrid's keyword side matches the query's other words too, so
// a question naming code gains even when the docs don't name it. A name the
// extractor misread from a string can at worst send a question to Hybrid.
// A failed lookup counts as no match.
func (ix *Indexer) Resolve(ctx context.Context, mode Mode, query string) Mode {
	if m := Resolve(mode, query); m != Semantic || (mode != "" && mode != Auto) {
		return m
	}
	if ix.Store != nil {
		if ok, err := ix.Store.InVocab(ctx, queryWords(query)); err == nil && ok {
			return Hybrid
		}
	}
	return Semantic
}

// maxVocabWords caps the query words looked up in the vocabulary: the
// first 32 distinct ones.
const maxVocabWords = 32

// queryWords returns the distinct words of query as the vocabulary stores
// names: trimmed as HasIdentifier trims them, lowercased, without a
// trailing possessive 's (PageFetcher's), at most vocab.MaxNameBytes each.
func queryWords(query string) []string {
	var out []string
	for _, w := range strings.Fields(query) {
		w = strings.ToLower(strings.Trim(w, "\"'`()[]{}<>.,;:?!"))
		for _, s := range []string{"'s", "\u2019s"} {
			w = strings.TrimSuffix(w, s)
		}
		if w == "" || len(w) > vocab.MaxNameBytes || slices.Contains(out, w) {
			continue
		}
		out = append(out, w)
		if len(out) == maxVocabWords {
			break
		}
	}
	return out
}

// HasIdentifier reports whether query holds an identifier-like word, which
// makes auto mode add keyword search. The query is split on whitespace and
// each word trimmed of surrounding quotes, brackets, backticks and
// .,;:?!; a word qualifies if it has at least one ASCII letter and at least
// one "_" or ASCII digit: PURGE_OLD_RECORDS, psycopg_pool, R7-Q2, v2
// qualify; ID, API, 2026, well-known, psycopg do not.
func HasIdentifier(query string) bool {
	for _, w := range strings.Fields(query) {
		w = strings.Trim(w, "\"'`()[]{}<>.,;:?!")
		letter, mark := false, false
		for i := 0; i < len(w); i++ {
			switch c := w[i]; {
			case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z':
				letter = true
			case c == '_', '0' <= c && c <= '9':
				mark = true
			}
		}
		if letter && mark {
			return true
		}
	}
	return false
}

// Fusion weights for the two sides of a hybrid search, and the rank offset
// of reciprocal rank fusion.
const (
	wVec    = 1.0
	wFTS    = 1.0
	rrfBase = 60
)

// keywordHint is added to a failed embedding call's error.
const keywordHint = `; mode="keyword" still works without the embedding API`

// Search returns the k chunks that best match query, by mode (see
// Resolve for Auto): vector search (which embeds query), keyword search
// over chunk text, heading and path (no API call), or both merged by
// reciprocal rank fusion. Each hit's
// Match says which side found it. An empty index returns no hits without
// calling the API.
//
// Every hit is re-checked against the current rules, and a hit from a file
// that changed since it was indexed gets its current line range, found in
// the file read for that check (each file is read at most once per search);
// if its text is no longer there, the hit is kept with Stale set. Chunks
// with identical text in several files are folded into one hit, with the
// other copies in Also. Paths dropped or changed are passed to OnStale.
func (ix *Indexer) Search(ctx context.Context, query string, k int, mode Mode) ([]store.Hit, error) {
	if k < 1 {
		return nil, fmt.Errorf("index: search: k must be at least 1, not %d", k)
	}
	mode = ix.Resolve(ctx, mode, query)
	if n, _ := ix.Store.ChunkCount(ctx); n == 0 {
		return nil, nil
	}
	// Over-fetch, since refused hits are dropped and duplicates folded.
	fetch := min(max(4*k, 40), 200)
	var vec, kw []store.Hit
	if mode != Keyword {
		e := ix.QueryEmbedder
		if e == nil {
			e = ix.Embedder
		}
		vecs, err := e.Embed(ctx, []string{query})
		if err != nil {
			return nil, fmt.Errorf("%w%s", err, keywordHint)
		}
		if vec, err = ix.Store.Search(ctx, vecs[0], fetch); err != nil {
			return nil, err
		}
	}
	if mode != Semantic {
		if q, ok := ftsQuery(query); ok {
			if TestHooks.Keyword != nil {
				TestHooks.Keyword(q)
			}
			var err error
			if kw, err = ix.Store.Keyword(ctx, q, fetch); err != nil {
				return nil, err
			}
		}
	}
	var hits []store.Hit
	switch mode {
	case Semantic:
		hits = vec
	case Keyword:
		hits = kw
	default:
		hits = fuse(vec, kw)
	}
	files := map[string]*fileView{}
	var requeue []string
	out := hits[:0]
	for _, h := range hits {
		fv, seen := files[h.Path]
		if !seen {
			fv = ix.check(h)
			files[h.Path] = fv
			if !fv.ok || fv.changed {
				requeue = append(requeue, h.Path)
			}
		}
		if !fv.ok {
			continue
		}
		if fv.changed {
			fv.locate(&h)
		}
		out = append(out, h)
	}
	out, err := ix.foldDuplicates(ctx, out, files, &requeue)
	if err != nil {
		return nil, err
	}
	if len(out) > k {
		out = out[:k]
	}
	if len(requeue) > 0 && ix.OnStale != nil {
		ix.OnStale(requeue)
	}
	return out, nil
}

// Lines returns the current text of each hit's line range, read through
// the policy once per file. A stale hit, a file now refused or
// unreadable, or a range that no longer holds the hit's text
// (collapse(range) does not contain collapse(h.Text)) gives nil. A hit
// with no recorded end line is taken to span as many lines as its text.
func (ix *Indexer) Lines(hits []store.Hit) [][]string {
	out := make([][]string, len(hits))
	files := map[string][]string{} // nil: refused or unreadable
	for i, h := range hits {
		if h.Stale || h.Line < 1 {
			continue
		}
		lines, seen := files[h.Path]
		if !seen {
			if src, err := ix.Policy.Read(h.Path); err == nil {
				lines = strings.Split(strings.ReplaceAll(string(src), "\r\n", "\n"), "\n")
			}
			files[h.Path] = lines
		}
		end := h.EndLine
		if end == 0 {
			end = h.Line + strings.Count(h.Text, "\n")
		}
		if lines == nil || end < h.Line || end > len(lines) {
			continue
		}
		r := lines[h.Line-1 : end]
		if !strings.Contains(collapse(strings.Join(r, "\n")), collapse(h.Text)) {
			continue
		}
		out[i] = slices.Clone(r)
	}
	return out
}

// fuse merges vector and keyword hits by reciprocal rank fusion: a chunk
// scores the sum of w/(rrfBase+rank) over the lists it is in (rank is
// 1-based). Ties go to the better single rank, then the lower chunk id. A
// chunk in both lists keeps its vector distance and has Match MatchBoth.
func fuse(vec, kw []store.Hit) []store.Hit {
	type entry struct {
		hit   store.Hit
		score float64
		best  int
	}
	byID := map[int64]*entry{}
	var all []*entry
	add := func(hits []store.Hit, w float64) {
		for i, h := range hits {
			rank := i + 1
			e, ok := byID[h.ID]
			if !ok {
				e = &entry{hit: h, best: rank}
				byID[h.ID] = e
				all = append(all, e)
			} else {
				e.hit.Match = store.MatchBoth
				e.best = min(e.best, rank)
			}
			e.score += w / float64(rrfBase+rank)
		}
	}
	add(vec, wVec)
	add(kw, wFTS)
	slices.SortStableFunc(all, func(a, b *entry) int {
		switch {
		case a.score != b.score:
			if a.score > b.score {
				return -1
			}
			return 1
		case a.best != b.best:
			return a.best - b.best
		}
		return cmp.Compare(a.hit.ID, b.hit.ID)
	})
	out := make([]store.Hit, len(all))
	for i, e := range all {
		out[i] = e.hit
	}
	return out
}

// Limits on the keyword query built from user text.
const (
	maxFTSTerms     = 32
	maxFTSTermBytes = 64
)

// KeywordWords returns the words keyword search looks for in user text: it
// splits on whitespace, strips control characters and trims
// .,;:!?()[]{}"' from each end of a word (not _ or -), and keeps at most
// maxFTSTerms words of at most maxFTSTermBytes. Duplicates are kept.
func KeywordWords(q string) []string {
	var words []string
	for _, f := range strings.Fields(q) {
		f = strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, f)
		f = strings.Trim(f, `.,;:!?()[]{}"'`)
		if f == "" {
			continue
		}
		if len(f) > maxFTSTermBytes {
			f = f[:runeCut(f, maxFTSTermBytes)]
		}
		words = append(words, f)
		if len(words) == maxFTSTerms {
			break
		}
	}
	return words
}

// ftsQuery turns user text into an FTS5 query that is never parsed as FTS
// syntax: it writes each of KeywordWords(q) as a quoted string with inner
// quotes doubled, joined with OR. Inside quotes, AND, NEAR, *, ^ and col:
// are plain text. ok is false when no word is left, and keyword search is
// skipped.
func ftsQuery(q string) (string, bool) {
	words := KeywordWords(q)
	terms := make([]string, len(words))
	for i, w := range words {
		terms[i] = `"` + strings.ReplaceAll(w, `"`, `""`) + `"`
	}
	return strings.Join(terms, " OR "), len(terms) > 0
}

// runeCut returns the largest index <= n that does not split a rune in s.
func runeCut(s string, n int) int {
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return n
}

// TestHooks lets tests observe Search. Nil outside tests.
var TestHooks struct {
	SearchRead func(path string)  // Search reads a changed file
	Keyword    func(match string) // Search runs a keyword query
}

// fileView is what Search learned about one hit file.
type fileView struct {
	ok      bool   // the policy still allows it
	changed bool   // its mtime or size differs from what was indexed
	cur     string // when changed: its content, LF-normalized
	norm    *normText
}

// check re-checks a hit's file against the current rules: a stat-only
// Check, and a full Read (for front matter, and to locate the hit) when the
// file's mtime or size differs from what was indexed. Any refusal or error
// drops the file's hits.
func (ix *Indexer) check(h store.Hit) *fileView {
	fi, err := ix.Policy.Check(h.Path)
	if err != nil {
		return &fileView{}
	}
	if h.Stat != nil && h.Stat.Mtime == fi.ModTime().UnixNano() && h.Stat.Size == fi.Size() {
		return &fileView{ok: true}
	}
	if TestHooks.SearchRead != nil {
		TestHooks.SearchRead(h.Path)
	}
	src, err := ix.Policy.Read(h.Path)
	if err != nil {
		return &fileView{}
	}
	return &fileView{ok: true, changed: true, cur: strings.ReplaceAll(string(src), "\r\n", "\n")}
}

// maxOccurrences caps how many matches of a chunk Search considers in one
// file.
const maxOccurrences = 16

// locate sets h's line range to where its text is in the current file:
// first an exact match, then one ignoring whitespace differences, each time
// the occurrence nearest the stored line. Not found, h is marked Stale and
// loses its range.
func (fv *fileView) locate(h *store.Hit) {
	cur := fv.cur
	if i, ok := nearest(cur, h.Text, h.Line, func(i int) int { return lineAt(cur, i) }); ok {
		h.Line, h.EndLine = lineAt(cur, i), lineAt(cur, i+len(h.Text)-1)
		return
	}
	if t := collapse(h.Text); t != "" {
		if fv.norm == nil {
			fv.norm = normalize(cur)
		}
		n := fv.norm
		if i, ok := nearest(n.text, t, h.Line, func(i int) int { return lineAt(cur, n.orig[i]) }); ok {
			h.Line, h.EndLine = lineAt(cur, n.orig[i]), lineAt(cur, n.orig[i+len(t)-1])
			return
		}
	}
	h.Line, h.EndLine, h.Stale = 0, 0, true
}

// nearest returns the index in s of the match of t, among the first
// maxOccurrences, whose line (from lineOf) is closest to want.
func nearest(s, t string, want int, lineOf func(i int) int) (int, bool) {
	if t == "" {
		return 0, false
	}
	best, bestDist := -1, 0
	for from, seen := 0, 0; seen < maxOccurrences && from <= len(s); seen++ {
		i := strings.Index(s[from:], t)
		if i < 0 {
			break
		}
		i += from
		if d := abs(lineOf(i) - want); best < 0 || d < bestDist {
			best, bestDist = i, d
		}
		from = i + 1
	}
	return best, best >= 0
}

// normText is a file's text with every run of whitespace collapsed to one
// space; orig maps each byte of text to its offset in the file.
type normText struct {
	text string
	orig []int
}

// normalize builds the collapsed form of s, once per file per search.
func normalize(s string) *normText {
	var b strings.Builder
	orig := make([]int, 0, len(s))
	space := false
	for i, r := range s {
		if unicode.IsSpace(r) {
			space = b.Len() > 0
			continue
		}
		if space {
			b.WriteByte(' ')
			orig = append(orig, i-1) // the byte before r is whitespace
			space = false
		}
		n := b.Len()
		b.WriteRune(r)
		for range b.Len() - n {
			orig = append(orig, i) // every byte of r maps to its start
		}
	}
	return &normText{text: b.String(), orig: orig}
}

// collapse is normalize without the offsets.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// maxCopies caps the copies of one chunk Search looks up and checks;
// maxAlso caps how many it lists.
const (
	maxCopies = 64
	maxAlso   = 3
)

// foldDuplicates folds hits with the same text hash into one, keeping the
// group's best rank. The hit shown is the copy with the fewest path
// segments, then the shortest path, then the first in byte order. The other
// copies are looked up in the store, not only among the fetched hits, and
// each must pass the current rules; up to maxAlso go in Also, the count of
// the rest in AlsoMore. A listed copy whose file changed since it was
// indexed is located in the current file like a hit (files caches what
// Search learned, so each file is still read at most once) and gets its
// current range, or Stale; its path is added to requeue. A hit with no text
// hash is its own group.
func (ix *Indexer) foldDuplicates(ctx context.Context, hits []store.Hit, files map[string]*fileView, requeue *[]string) ([]store.Hit, error) {
	var order []string
	groups := map[string][]int{}
	for i, h := range hits {
		key := h.TextHash
		if key == "" {
			key = fmt.Sprintf("\x00%d", i)
		}
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], i)
	}
	out := make([]store.Hit, 0, len(order))
	for _, key := range order {
		members := groups[key]
		best := members[0]
		for _, m := range members[1:] {
			if primaryLess(hits[m].Path, hits[best].Path) {
				best = m
			}
		}
		h := hits[best]
		if h.TextHash != "" {
			copies, err := ix.Store.Copies(ctx, h.TextHash, h.ID, maxCopies)
			if err != nil {
				return nil, err
			}
			allowed := map[string]bool{}
			var also []store.Copy
			for _, c := range copies {
				if len(also) < maxAlso {
					// Listed: full check, and the current range.
					fv, seen := files[c.Path]
					if !seen {
						fv = ix.check(store.Hit{Chunk: store.Chunk{Path: c.Path}, Stat: c.Stat})
						files[c.Path] = fv
						if !fv.ok || fv.changed {
							*requeue = append(*requeue, c.Path)
						}
					}
					if !fv.ok {
						continue
					}
					cp := c.Copy
					if fv.changed {
						tmp := store.Hit{Chunk: store.Chunk{Text: h.Text, Line: c.Line}}
						fv.locate(&tmp)
						cp.Line, cp.EndLine, cp.Stale = tmp.Line, tmp.EndLine, tmp.Stale
					}
					also = append(also, cp)
					continue
				}
				// Only counted: a stat-only check is enough.
				ok, seen := allowed[c.Path]
				if !seen {
					if fv, inFiles := files[c.Path]; inFiles {
						ok = fv.ok
					} else {
						_, err := ix.Policy.Check(c.Path)
						ok = err == nil
					}
					allowed[c.Path] = ok
				}
				if ok {
					h.AlsoMore++
				}
			}
			h.Also = also
		}
		out = append(out, h)
	}
	return out, nil
}

// primaryLess orders duplicate copies: fewer path segments, then a shorter
// path, then byte order.
func primaryLess(a, b string) bool {
	if sa, sb := strings.Count(a, "/"), strings.Count(b, "/"); sa != sb {
		return sa < sb
	}
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// lineAt returns the 1-based line of byte offset i in s.
func lineAt(s string, i int) int { return 1 + strings.Count(s[:i], "\n") }

func (ix *Indexer) logf(format string, args ...any) {
	if ix.Log != nil {
		ix.Log.Printf(format, args...)
	}
}
