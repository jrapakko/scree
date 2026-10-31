# Plan: human view for `scree search` (`-format text`)

From the architect, 2026-10-30. User's ask: a CLI human view grouped by keyword, with
tab-indented `>` lines and per-hit file links. No colour (formatting is the point), no
local search page.

## Decisions

- **Flag:** `-format text|xml|json`. Default changes from `"xml"` to `""`: empty means
  text when stdout is a terminal, xml otherwise. Explicit `-format text` into a pipe
  prints plain text, no links.
- **Mode stays `auto`** for every format (a keyword-only default for text would make
  `scree search q` and `scree search q | less` return different hits). `-mode keyword`
  gives the pure grep-like view.
- **Limits:** 10 lines per word, then `\t>… N more lines`. 160 characters per line, cut
  with `…`; if the match lies past the cut, the window starts ~40 characters before the
  match with a leading `…`. Words come from the keyword builder (≤32), deduplicated by
  exact string.
- **Indent:** a literal tab, then `>`.
- **Libraries:** `golang.org/x/term` for `IsTerminal` (take `@latest` if online, else
  v0.21.0 from the module cache; keep x/sys at v0.48.0). OSC 8 written by hand.
- **No colour.**

## Output layout

```
ci:
	>docs/release.md:41: see .forgejo/workflows/ci.yml for the job
	>ci/README.md:3-9 (in path)
	>docs/ops.md:20-31 (in heading)
	>… 4 more lines
seed:
	>docs/db.md:7: seed the database with fixtures
other hits:
	>docs/keys.md:3-9 Setup > Keys
	>docs/old.md (stale)
[mode: auto → hybrid]
```

- Per word: every current line of every hit containing the word (hit rank order, then
  line order) as `path:line: text`. A hit whose lines lack the word but whose path or
  heading holds it gets one `path:start-end (in path)` / `(in heading)` entry.
- `other hits:` — hits with no entry under any word (usually semantic-only) as
  `path:start-end heading`; stale hits, and hits whose file changed or was refused
  between Search and the line read, as `path (stale)`.
- Links: when on, wrap the `path:line` / `path:start-end` label as
  `ESC ]8;; file:///abs ESC \ label ESC ]8;; ESC \`, URI from
  `(&url.URL{Scheme: "file", Path: abs}).String()`. No line fragment.
- No hits: `No matching documents.` (as FormatHits). `[mode: auto → …]` note as for xml.
- Sanitising: every note-derived string (line text, path label, heading, the word) goes
  through `textsafe.Strip`, then drop `unicode.Bidi_Control` runes; tabs in line text
  become a space.

## Steps

1. **`internal/store`**
   - `store.go`: `const ftsTokenize = "unicode61 remove_diacritics 2 tokenchars '_-'"`;
     build `ftsSchema` from it (identical SQL, no migration).
   - New `match.go`:
     ```go
     // TextMatch is a text that matched a word, and the byte offset of the
     // match's first token in it.
     type TextMatch struct{ Row, At int }
     // MatchTexts reports, for each word, which texts match it as keyword
     // search would: each word is quoted as phrase() quotes it and matched in
     // a private in-memory FTS5 table using the keyword index's tokenizer.
     // texts must hold no \x01 or \x02 (callers pass textsafe.Strip output).
     func MatchTexts(ctx context.Context, words, texts []string) ([][]TextMatch, error)
     ```
     `driver.Open("file::memory:", fts5.Register)`, `SetMaxOpenConns(1)`;
     `CREATE VIRTUAL TABLE t USING fts5(text, tokenize="…")`; insert all texts in one
     transaction (rowid i+1); per word
     `SELECT rowid, highlight(t, 0, char(1), char(2)) FROM t WHERE t MATCH ? ORDER BY rowid`
     with `phrase(word)` from `vocab.go`; `At` = index of `\x01`. Close when done.
2. **`internal/index/index.go`**
   - Split `ftsQuery` into `KeywordWords(q string) []string` (current split, control
     strip, trim, 32-word/64-byte limits) plus quoting/joining; behaviour unchanged.
   - Add:
     ```go
     // Lines returns the current text of each hit's line range, read through
     // the policy once per file. A stale hit, a file now refused or
     // unreadable, or a range that no longer holds the hit's text
     // (collapse(range) does not contain collapse(h.Text)) gives nil.
     func (ix *Indexer) Lines(hits []store.Hit) [][]string
     ```
3. **`cmd/scree/searchtext.go`** (new)
   - `var stdoutTerminal = func() bool { return term.IsTerminal(int(os.Stdout.Fd())) }`
   - `linksOn()`: `stdoutTerminal() && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"`.
   - `writeText(ctx, w, ix, query, hits, note, links)`: words from
     `index.KeywordWords` (dedup), lines from `ix.Lines`, texts = all lines, then each
     path, then each heading (Strip'd) with an index map; one `store.MatchTexts` call;
     render. Helpers `osc8`, `cutLine`, `safe`. Constants `maxWordLines = 10`,
     `maxLineChars = 160`. Abs path `filepath.Join(ix.Root(), filepath.FromSlash(h.Path))`.
4. **`cmd/scree/launch.go`**: in `runRun` only (not `runMCP`), pass a normalised
   `NO_COLOR=1` to the child when NO_COLOR is non-empty or TERM is `dumb`. Never pass the
   raw values or TERM.
5. **`cmd/scree/main.go`**: flag `fs.String("format", "", "text (the default on a
   terminal), xml (the default otherwise) or json")`; resolve `""` via
   `stdoutTerminal()`; validate (`format must be text, xml or json`); `text` branch calls
   `writeText(..., linksOn())`. Update the usage strings to `[-format text|xml|json]`.
6. **`go.mod`/`go.sum`**: add `golang.org/x/term`, `go mod tidy`.
7. **Docs**
   - SPEC.md CLI line → `[-format text|xml|json]`; paragraph after the `-format json` one:
     "`-format text`, the default when stdout is a terminal (xml otherwise), groups the
     results by query word, split and trimmed as for keyword search: a `word:` line, then
     one tab-indented `>path:line: text` line per current source line of a hit that holds
     the word as keyword search matches it (same tokenizer; at most 10 per word, each cut
     at 160 characters around the match), or `>path:a-b (in path)` / `(in heading)` when
     only those hold it. Hits with no word, and stale hits, follow under `other hits:`.
     Lines are read from the current file through the same rules; control and bidi
     characters are dropped. On a terminal with `NO_COLOR` unset each location is an
     OSC 8 link to the file."
   - Replace the "Human view" open item with "Resolved: the human view is `scree
     search`'s text format (see CLI), with per-hit file links; no local search page."
   - README: command-block line for `-format text`; in "Results", a short paragraph and
     the example layout, noting a word can match inside a path written in a note (`ci` in
     `.forgejo/workflows/ci.yml`) and that `scree run search` honours `NO_COLOR`.

## Tests

- `internal/store/match_test.go` `TestMatchTexts`: `ci` matches
  `see .forgejo/workflows/ci.yml` (At at `ci`) not `circle`; `Café` ~ `cafe`/`CAFÉ`;
  `purge_old_records` ~ `PURGE_OLD_RECORDS`, `purge` does not; `ci.yml` as phrase;
  `R7-Q2` one token; `NEAR`, `col:x`, `"` plain text; `/` → no matches, no error.
- `internal/store/fts_test.go`: agreement — every chunk `Keyword` returns for a word has
  a line, path or heading matched by `MatchTexts`.
- `internal/index`: `KeywordWords` equals what `ftsQuery` quoted before; `Lines` for
  unchanged file, relocated file, text gone (nil), file now ignored (nil), stale hit (nil).
- `cmd/scree/search_test.go` (searchFixture, `-mode keyword -format text`):
  `purge_old_records ops` → exactly
  `purge_old_records:\n\t>ops.md:3: run PURGE_OLD_RECORDS nightly via the PageFetcher\nops:\n\t>ops.md:1: # Ops\n`;
  `md` → `\t>ops.md:1-3 (in path)`; no `-format` in a pipe → xml
  (`TestCLISearchDefaultsToAuto` keeps passing); semantic stub, unrelated query →
  `other hits:`; stale → `(stale)`; >10 lines → `>… N more lines`; `-format yaml` refused;
  hostile note line (`\x1b]8;;http://evil\x1b\\x\x1b]8;;\x1b\\`, BEL, U+202E) in a file
  named with space, `%`, `#`, raw ESC (skip if refused), with `stdoutTerminal` forced
  true: every `\x1b` belongs to our OSC 8 pairs (count = 4 × labels), URI holds `%1B`,
  `%20`, `%25`, no BEL, no U+202E; `NO_COLOR=1` or `TERM=dumb` → no `\x1b`.
- `cmd/scree/launch_test.go` `TestRunCleanEnvAndArgs`: NO_COLOR set → child env gains
  exactly `NO_COLOR=1`; unset → unchanged; TERM never passed; `runMCP` env unchanged.
- Commands, one at a time: `CGO_ENABLED=0 go build ./cmd/scree`, `go vet ./...`,
  `go test ./internal/store ./internal/index ./cmd/scree`, then `go test ./...`.

## Risks

- Default changes on a TTY (xml → text); machine consumers unaffected. Release note.
- `highlight()` offsets rely on Strip'd input (no `\x01`/`\x02`).
- `Lines` re-reads each hit's file; the containment check turns mid-search edits into
  `(stale)`.
- In `auto`, stopwords (`how:`, `the:`) form groups, capped at 10 lines.
- Display width not measured; CJK lines may wrap.
- `file://` links open the desktop's `.md` handler; no line jump.
- `also` copies not shown in text (still in xml/json).
