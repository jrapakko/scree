---
title: CLI reference
nav_title: CLI
---

```
scree mcp [-docker]
scree run <command> [args]
scree key [-offline] [-replace] [-check]
scree watch
scree serve [-http addr [-allow-remote]]
scree search [-k n] [-mode auto|hybrid|semantic|keyword] [-format text|xml|json|grep] [-no-pager] "query"
scree reindex [-all] [path]
scree doctor [-v] [-offline]
scree indexes [-prune]
scree version
scree help
```

From a terminal, `scree search`, `reindex`, `doctor` and `watch` use the API key in the settings file when `OPENROUTER_API_KEY` is unset, by re-running themselves as `scree run` would (see [Configuration](configuration.md#direct-commands-and-the-settings-files-key)). `scree run <command>`, for example `scree run search "…"`, loads the file's settings for any command. `scree mcp` is what a repo's `.mcp.json` runs.

Flags use Go's `flag` package: `-flag` and `--flag` are the same, booleans take `-flag=false`, and **flags must come before positional arguments**. `scree search -k 3 "query"` works; `scree search "query" -k 3` is a usage error.

## Which folder

| Command | Folder |
| --- | --- |
| any, with `-notes <dir>` | `<dir>`, used as given |
| `serve` under Claude Code (`CLAUDE_PROJECT_DIR` set) | the git root of the working directory, or that folder itself outside git; `CLAUDE_PROJECT_DIR` must be in the same repo |
| `serve` outside Claude Code, without `-notes` | none: an error |
| other commands without `-notes` | the git root above the current directory, or the current directory outside git |

A folder found automatically (not given with `-notes`) is refused if it is your home directory, a folder above it, or a filesystem root. Home comes from the password database; `$HOME` is used only if that lookup fails. The git root is found only through a `.git` you own that is not a symlink, and the search stops at a sticky folder such as `/tmp` (see [Security](security.md)). `doctor` prints which rule chose the folder.

Under Claude Code, `serve` also refuses `-notes`, `-db` and `SCREE_DB`, and refuses a folder whose root `.mcp.json` names no `scree mcp` server. Without `CLAUDE_PROJECT_DIR` (a terminal, a container, another MCP client), `-notes` and `-db` work as for any command and the opt-in check does not apply.

## Flags for every command but `indexes` and `key`

| Flag | Default | Meaning |
| --- | --- | --- |
| `-notes <dir>` | see above | Folder to index |
| `-db <file>` | `$SCREE_DB`, else `$XDG_DATA_HOME/scree/indexes/<folder>-<hash>/index.db` | Index file. The index records its folder and refuses another one. |
| `-gitignore` | `true` | Honor `.gitignore`, `.git/info/exclude` and your global excludes file. `-gitignore=false` turns that layer off. |

## Commands

### `scree mcp [-docker]`

What a repo's `.mcp.json` runs. It loads the settings file from your passwd home, then replaces itself (`exec`) with its own binary running `scree serve`, in an environment of exactly `HOME` (your passwd home), `PATH=/usr/local/bin:/usr/bin:/bin`, `CLAUDE_PROJECT_DIR` and the settings from the file. Nothing else from the environment that launched it gets through.

- It takes no argument but `-docker`, since arguments could come from a repo's `.mcp.json`.
- It needs `CLAUDE_PROJECT_DIR`: it is for Claude Code. From a terminal, use `scree run serve -notes <dir>`.
- `-docker` runs `serve` in a container instead; see [Docker](docker.md#claude-code-with-docker).
- It refuses to run on Windows.

### `scree run <command> [args]`

Runs any other command, with its arguments, the way `scree mcp` runs `serve`: settings from the file, nothing else from the environment except `CLAUDE_PROJECT_DIR` when set. For the text view it also passes `NO_COLOR=1` when `NO_COLOR` is set or `TERM` is `dumb`, and your `TERM` when it is a plain terminal name such as `xterm-256color`, so the pager works. `scree run mcp` and `scree run run` are refused. It refuses to run on Windows. The systemd unit uses `scree run watch`.

### `scree key [-offline] [-replace] [-check]`

Saves your OpenRouter API key in the settings file, then makes one test call to the embeddings API with it. On a terminal it prompts without echoing; otherwise it reads the first line of stdin. It writes the file atomically with mode 600, sets the `OPENROUTER_API_KEY=` line (or appends one) and keeps every other line as it was. It never prints the key.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-replace` | `false` | Let a key read from stdin replace one the file already holds. A key typed at the prompt always replaces it. |
| `-offline` | `false` | Save the key without the test call. |
| `-check` | `false` | Save nothing; make only the test call with the saved key. |

```sh
scree key                          # prompt, save, test
scree key -replace < new-key.txt   # replace the saved key from a file
scree key -check                   # test the saved key
```

The test call runs in the same clean environment as `scree run`, so it uses the file's endpoint and proxy, and prints `ok   embeddings API returned N dimensions`. Ctrl-C at the prompt saves nothing and exits with status 130. If `OPENROUTER_API_KEY` is also set in your environment, it says that commands run directly use that one. It refuses a settings file that fails the [usual checks](configuration.md#what-scree-mcp-and-scree-run-accept-and-refuse) (a missing one is fine), a symlinked `~/.config` or `~/.config/scree`, a `~/.config/scree` not yours or writable by others, and a key that is empty, over 1 KB, or holds whitespace, quotes, a backslash or control characters. It doesn't run on Windows.

### `scree watch`

Indexes new and changed files, then watches the folder and keeps the index current until stopped. If another process holds the writer lock, it waits, retrying every 5 seconds, and takes over when that process exits. Meant for a folder no Claude Code session keeps current, under systemd or Docker. It is subject to the first-sync cap.

It exits non-zero if the folder or the index file is removed or replaced, so a supervisor restarts it.

### `scree serve`

The MCP server. Claude Code starts it through `scree mcp`; you rarely run it by hand. Logs go to stderr; stdout carries the protocol.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-http <addr>` | none (stdio) | Serve streamable HTTP on `addr`, for example `127.0.0.1:7077` |
| `-allow-remote` | `false` | Allow a non-loopback `-http` address. Requires `SCREE_HTTP_TOKEN`. |

The writer runs in the background, so the MCP handshake and searches never wait for the lock or the first sync. HTTP rules are on the [Security](security.md#http-mode) page.

### `scree search`

Prints the top hits, or `No matching documents.`. Takes exactly one query argument; quote it. On a terminal it shows a [text view](#the-text-view) grouped by query word; in a pipe it prints the same `<hit>` elements the MCP tool returns (see [Using with Claude Code](using-with-claude-code.md#search-output)).

| Flag | Default | Meaning |
| --- | --- | --- |
| `-k <n>` | `8` | Number of results, at least 1. Not capped on the CLI; the MCP tool caps at 50. |
| `-mode <m>` | `auto` | `auto` (semantic, plus keyword search when the query holds an identifier such as `FOO_BAR` or a name from the folder's code; keyword alone when there is no API key), `hybrid`, `semantic` or `keyword`; see [search modes](using-with-claude-code.md#search-modes). `keyword` makes no API call. |
| `-format <f>` | `text` on a terminal, else `xml` | `text` for the text view, `xml` for `<hit>` elements, `json` for one object per line, or `grep` for `path:line:text` lines |
| `-no-pager` | `false` | Print the text view directly instead of through a pager |

```sh
scree search "how do I rotate the backup keys"
scree search -mode keyword PURGE_OLD_RECORDS     # exact words, no API call
scree search -no-pager "ci seed"                  # text view, without the pager
scree search -format json "release checklist"
scree search -format grep "ci seed"              # path:line:text, for fzf or vim -q
```

`-format json` is the stable format for scripts and evaluations. Each line is `{"n","mode","path","start","end","heading","score","match","stale","also","also_more"}`: `mode` is the mode that ran (never `auto`), `start`, `end` and `score` are `null` when absent, `also` is a list of `{"path","start","end"}` (with `"stale":true` on a copy whose text is gone), and `also_more` appears only when more copies exist. The chunk text is left out.

`-format grep` prints `path:line:text` lines like `grep -n`: each line of a hit that holds a query word, or a semantic hit's first line, in rank order, each once, with control characters dropped. The mode note goes to stderr. Pick a hit with [fzf](https://github.com/junegunn/fzf) and open it in vim, or load all of them into vim's quickfix list:

```sh
sg() { scree search -format grep "$*" | fzf --delimiter : \
  --preview 'n={2}; case $n in ""|*[!0-9]*) exit;; esac; sed -n "$(( n>5 ? n-5 : 1 )),+30p" -- {1}' \
  --bind 'enter:become(n={2}; case $n in ""|*[!0-9]*) exit 1;; esac; vim "+$n" -- {1})'; }
vim --cmd 'set efm=%f:%l:%m' -q <(scree search -format grep "ci seed")
```

Paths are relative to the folder, so run these from its top. A folder you didn't write can choose its file names and text, so treat the fields as untrusted: check the line is a number and put `--` before the path, as above, rather than pasting `{1}` or `{2}` into shell arithmetic or a command line, and give `vim -q` the `%f:%l:%m` error format, since its default one also picks up `"file" line N:` inside note text. scree leaves out hits whose path holds `:` or a control character (with a count on stderr) and starts a path beginning with `-` or `+` with `./`.

An empty index returns no hits without calling the API. Without an API key, `auto` runs `keyword` and ends with `[mode: auto → keyword: no API key; run scree key, or set OPENROUTER_API_KEY]`, and `-mode semantic` or `hybrid` is an error.

#### The text view

The view is grouped by query word: a `word:` line, then each hit whose text, path or heading holds that word, as keyword search matches it. Each hit is listed once, under the first word it holds. Per hit it shows:

- an excerpt: the hit's heading and the lines that hold the word (at most 10, each cut at 160 characters);
- its location, `>path:start-end`, a clickable link to the file in terminals that support OSC 8 links;
- every line of that range from the current file, as numbered `>>N` lines (cut at 3,500 bytes).

Hits with no query word, usually found by the vector side alone, and stale hits come last, under `other hits:`. A stale hit has no numbered lines.

```
seed:
  ### 5.1 Files
  - `.forgejo/workflows/ci.yml`: build, test, and the seed from the run number
	>docs/plan.md:189-191
		>>189  ### 5.1 Files
		>>190
		>>191  - `.forgejo/workflows/ci.yml`: build, test, and the seed from the run number

other hits:
  ## Keys
	>docs/keys.md:3-4
		>>3  ## Keys
		>>4  Rotate them yearly.

  # Old notes
	>docs/old.md (stale)
[mode: auto → hybrid]
```

A word matches whole tokens, as keyword search does, including inside a path written in a note (`ci` matches `.forgejo/workflows/ci.yml`) and across a line break. Use `-mode keyword` for a grep-like view with no semantic hits. The numbered lines are read from the file through the same ignore rules as everything else, and control characters in note text are dropped, so a note can't add its own escapes.

**Colours.** The word headings, the matched words, the locations and the line numbers are coloured. `SCREE_COLORS` changes them (see [Configuration](configuration.md#the-text-view)). Set `NO_COLOR`, or `TERM=dumb`, to turn colours and links off.

**Pager.** On a terminal the view goes through a pager, so it starts at the top: `less -FRX` by default, which quits at once if the view fits on one screen, passes the colours and links, and leaves the text on screen when you quit. `SCREE_PAGER` picks another one. The view is printed directly with `-no-pager`, `SCREE_PAGER` set but empty, in a pipe, or when no pager can start. Quitting the pager early is not an error. `less` passes OSC 8 links with `-R` from version 566; with an older `less`, use `-no-pager`.

### `scree reindex [-all] [path]`

Without a path, syncs the whole folder: indexes new and changed files by content hash and removes rows for deleted or newly ignored ones. Prints `N files: N indexed, N removed, N failed, N deferred`. Without an API key, files are indexed for keyword search only, with no API call, and the count reads `N indexed (M keyword only: no API key)`; the first sync with a key embeds just those files.

With a path, relative to the folder or absolute inside it, re-indexes that one file and prints `re-indexed "<path>"` or `"<path>" is already up to date`. An ignored path is an error.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-all` | `false` | Drop the index and rebuild it from scratch. Needed after changing `SCREE_MODEL` or `SCREE_DIMENSIONS`, and once for an index built before the current chunk size limits. |

If another process holds the writer lock:

- plain `reindex` and `reindex <path>` print `pid N keeps this index current; nothing to do` and exit 0 (an ignored path is still refused first);
- `reindex -all` fails: `index is in use by pid N (a Claude Code session or scree watch); close it, then run reindex -all`.

The CLI is never subject to the first-sync cap: it is how you index a large folder on purpose.

### `scree doctor [-v] [-offline]`

Checks the setup and prints one line per check, marked `ok`, `warn` or `FAIL`:

- the folder and which rule chose it;
- files indexable, their total size, estimated tokens and, for the default model, the cost to embed them in full, with a warning when the folder is over `SCREE_FIRST_SYNC_MB` and not yet indexed;
- the root hidden folders that are indexed;
- how many files each ignore layer excludes (built-in, `.gitignore` and nested repositories, `.screeignore` with its `!` re-includes, front matter), plus bad ignore-file lines by number;
- the index file, its file and chunk counts (and how many files are keyword-only because they were indexed without a key), the keyword index, the last full sync;
- the [code vocabulary](using-with-claude-code.md#the-code-vocabulary): how many names were read from how many manifests and source files, and how many of them the notes also contain;
- whether a process holds the writer lock, and its pid;
- the configured model, and whether the index was built with another;
- whether `OPENROUTER_API_KEY` is set, and `(from <file>)` when it came from the settings file; without a key it reports `FAIL` and says search runs keyword mode only;
- whether zero data retention is requested;
- a test embedding call, unless `-offline`.

`doctor` never creates or writes the index.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-v` | `false` | Also run FTS5's integrity check on the keyword index, print up to 20 code-vocabulary names the notes also contain, and list every ignored path with its reason, unreadable paths, and every file a `!` rule re-included with the rule's line |
| `-offline` | `false` | Skip the test call to the embeddings API |

Sample of `doctor -v -offline` in a small repo with the key set and no index yet:

```
ok   folder ~/code/demo (from git root of the current directory)
ok   files: 3 indexable
ok   size: 0.0 MB of markdown, about 3k tokens to embed in full (under $0.01 with openai/text-embedding-3-small)
ok   hidden folders indexed: .github/
ok   built-in: 2 hidden, 1 not markdown, 0 outside the folder, 0 too large, 0 unsafe name
ok   .gitignore: 1 (0 nested repositories)
ok   .screeignore: 1 ignored, 0 re-included by "!"
ok   front matter: 1
  ignored (hidden): ".git/"
  ignored (hidden): ".obsidian/"
  ignored (not markdown): "main.go"
  ignored (.gitignore): "node_modules/"
  ignored (.screeignore): "journal/"
  ignored (front matter): "docs/draft.md"
ok   index ~/.local/share/scree/indexes/demo-b40ee07c8f619a6a/index.db: not created yet (the first serve, watch or reindex builds it)
ok   writer: none running (a Claude Code session or scree watch takes the role)
ok   model openai/text-embedding-3-small@512 (index empty)
ok   OPENROUTER_API_KEY set
ok   zero data retention requested (provider.zdr=true, data_collection=deny)
```

The real output prints absolute paths.

Once the index exists, the index lines look like this, with `-v`:

```
ok   index ~/.local/share/scree/indexes/demo-b40ee07c8f619a6a/index.db: 3 files, 11 chunks (vec1 …)
ok   keyword index: 11 chunks
ok   keyword index integrity check
ok   code vocabulary: 42 names from 9 manifests and source files, 3 also in the notes (names only; never embedded)
  vocabulary sample (names also in the notes): "pagefetcher" "psycopg" "usestate"
```

A name counts as in the notes when keyword search would match it: `pagefetcher` matches `PageFetcher`, and a dotted or slashed name such as `golang.org/x/sync` must match as a phrase. A name found only in code is never printed, since it may be text the extractor misread from a string.

### `scree indexes [-prune]`

Lists every index under `$XDG_DATA_HOME/scree/indexes/`, one line each: its folder, file and chunk counts, size on disk, last sync, model, `[missing]` when the folder is gone, `[in use by pid N]` when a writer holds it, and the index directory's name. It also points out an index left by the single-folder version (`~/.local/share/scree/index.db`).

Listing never writes: it reads each index over a read-only connection without upgrading it, and checks the lock without creating it. Each value it prints from an index's files is cut at 80 characters.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-prune` | `false` | Delete indexes whose folder no longer exists. An index a writer holds is kept. Only real directories directly inside the indexes folder are removed, and no symlink is followed out of it. |

`indexes` takes no `-notes`, `-db` or `-gitignore`.

### `scree version`, `scree help`

`version` (also `-version`, `--version`) prints `scree <version>`; builds from source print `scree dev`. `help` (also `-h`, `-help`, `--help`) prints the usage text.

## Exit status

| Status | When |
| --- | --- |
| 0 | Success; `reindex` while another process writes; `serve` or `watch` stopped by a signal; quitting the search pager early; `-h` on a command |
| 1 | Any error, printed as `scree: <message>`; `doctor` found a `FAIL`; `reindex` had failed or deferred files; `indexes -prune` could not remove an index; a second Ctrl-C or SIGTERM during shutdown |
| 2 | No command, an unknown command, or a flag that doesn't parse |
| 130 | Ctrl-C or SIGTERM at the `scree key` prompt; nothing is saved |

On the first SIGINT or SIGTERM, `serve` and `watch` shut down cleanly, waiting at most 5 seconds for the writer. A second signal exits at once.
