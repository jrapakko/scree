# scree: spec

Local semantic search over the markdown in your repos and notes, served to Claude Code over MCP.

## Why it exists

Long markdown files get unwieldy as context, and reorganizing them by hand only delays the problem. Scree splits notes into small pieces, embeds them, and lets Claude Code pull in just the pieces that match a question. In a code repo, the same applies to READMEs, design docs and ADRs.

The markdown stays the source of truth. The index is derived from it and can be deleted and rebuilt at any time.

## Architecture

One Go process per Claude Code session (`scree serve`), started through `scree mcp` by the repo's committed `.mcp.json` (`claude mcp add scree --scope project -- scree mcp`). The folder is the git root of the working directory Claude Code starts the server in (the project, or the active worktree). `.mcp.json` has no working-directory field, but its `env` is applied last and overrides everything, `CLAUDE_PROJECT_DIR` and `PATH` included, so `CLAUDE_PROJECT_DIR` is only a cross-check: `serve` refuses to start if it is not in the same repository (the same work tree, or another work tree with the same git common dir). `serve` also refuses a folder whose root `.mcp.json` (a regular file, not a symlink, under 64 KB, valid JSON) has no server whose `command` is `scree` or ends in `/scree` with `mcp` as its first argument (or whose `command` is the legacy wrapper name `scree-mcp`, or ends in `/scree-mcp`), so a repo is indexed only if its own committed `.mcp.json` opts in. Under Claude Code, `serve` refuses `-notes`, `-db` and `SCREE_DB`; those are command-line options. Outside Claude Code, `serve` has no default folder and needs `-notes`; the opt-in check does not apply to an explicit `-notes`. A folder outside any repo, such as `~/notes`, is searched from the CLI, or from a Claude Code session started in that folder. MCP roots are not used: they are deprecated as of protocol 2026-07-28, they include `--add-dir` folders that have not opted in, and the Go SDK refuses server-initiated `roots/list` on that version. Outside Claude Code, `-notes <dir>` names the folder; the CLI defaults to the git root of the current directory. Each folder has its own index under `$XDG_DATA_HOME/scree/indexes/`. Each process contains:

- **Writer** (fsnotify, hash, chunk): reads the folder's markdown, calls the embedding API, writes to the index. In each process that runs it (`serve` or `watch`), it first takes the index's lock file; only the holder writes.
- **Index** (SQLite): chunks with their line ranges and text hashes, vectors in vec1, and an FTS5 keyword index, on disk.
- **MCP search server** (`search_docs`, `read_doc`, `reindex`): reads the index, embeds each query through the same API when the search mode needs it, serves Claude Code over stdio or HTTP.

The processes that serve one folder share its index: one writes, the rest read.

`scree mcp` is what `.mcp.json` runs, and `scree mcp -docker` its container variant; `scree run <command>` does the same for a command typed in a terminal. They are part of the static binary (CGO-free; on Linux there is no dynamic loader and no shell), so the class of attacks that act on a shell or loader before its first line (`LD_PRELOAD`, bash's `SHELLOPTS` with `PS4`, exported `BASH_FUNC_*` functions) does not apply. They take the home directory from the password database (on Linux `/etc/passwd`, then `getent passwd <uid>` by absolute path in an empty environment; on macOS `getpwuid_r` in libSystem, which `os/user` uses even without cgo; never `$HOME`), read only `<home>/.config/scree/env`, which must be a regular file (not a symlink) owned by the user with no group or other permission bits, and parse it as data: `NAME=value` lines with an optional `export ` and optional matching quotes, accepting only `SCREE_*`, `OPENROUTER_API_KEY`, the proxy variables, and `DOCKER_HOST` and `DOCKER_CONTEXT`, which only the container variant passes on, to the docker CLI. `scree mcp` refuses every argument but `-docker`. The Go runtime reads `GODEBUG`, `GOTRACEBACK`, `GOMAXPROCS` and `GOGC` from the environment before `main`, so the real work happens in a replacement process: `scree mcp` `exec`s its own binary (`os.Executable`, symlinks resolved) as `scree serve` with an environment of exactly `HOME` (the passwd home), `PATH=/usr/local/bin:/usr/bin:/bin`, `CLAUDE_PROJECT_DIR` and the file's settings. With `-docker` it `exec`s `docker` or `podman` (`SCREE_DOCKER`), found by absolute path in `/usr/local/bin`, `/usr/bin`, `/bin` or `/opt/homebrew/bin` and never through the inherited `PATH`, with `HOME`, a `PATH` of those folders and the settings, and passes settings to the container by name (`-e NAME`), never as values in argv. Once the real `scree` runs, the repo's environment cannot choose the binary that serves, the settings file, the endpoint, the key, the index location or the folder. On Windows, which has no `exec`, `scree mcp` and `scree run` refuse to run; set the variables yourself and run `scree serve -notes` or another command directly.

**Trust.** Approving scree in a repo's `.mcp.json` means trusting that repo. Claude Code applies the repo's `env`, including `PATH`, when it starts the server, so the repo controls which program `scree` resolves to, and that program runs as you. The guarantees above hold only for the real `scree`. A user's own local-scope entry with an absolute path (`claude mcp add scree --scope local -- "$HOME/.local/bin/scree" mcp`) replaces the project's entry, `env` included, for that user. On macOS a Go binary still links libSystem dynamically, so `DYLD_*` variables from the repo's `env` reach the first `scree mcp` process unless the binary is signed with the hardened runtime (see Open items).

External: the markdown files (source of truth), embedding API (OpenRouter), Claude Code, and a human view (editor or browser) that reads the files directly.

## Go stack

| Concern | Choice | Notes |
| --- | --- | --- |
| Language | Go | One small static binary, low idle memory, no runtime to install. |
| File watching | `fsnotify` | Native events when run directly on the laptop. Containers on Docker Desktop lose these events on bind mounts. |
| Vector storage | SQLite with [vec1](https://sqlite.org/vec1) | Disk-backed, so memory stays flat. `ncruces/go-sqlite3` ships vec1 compiled to WebAssembly, so the binary stays CGO-free. Flat (exact) cosine index. `sqlite-vec` was the first choice, but its Go bindings pin driver v0.17.1, far behind current releases. Keyword search uses the FTS5 extension from the same driver (unicode61 tokenizer, with `_` and `-` as token characters so identifiers stay whole). |
| Embeddings | API through OpenRouter | No local model. Default `openai/text-embedding-3-small` (~$0.02/M tokens) at 512 dimensions, chosen over pplx-embed-v1-0.6b on real notes (see Open items). Requests ask for zero-data-retention endpoints with `data_collection: deny` by default (`SCREE_ZDR=0` turns this off); a model with no such endpoint fails with a clear error. Timeouts: 10 s per query with no retry on timeout; 30 s per indexing attempt, and after an outage indexing pauses for 30 s, doubling up to 5 min, then resyncs. |
| MCP | Official Go MCP SDK | stdio for Claude Code; optional HTTP. |
| Chunking | By heading | Each chunk keeps its file path and heading trail, and records its first and last source line and a hash of its text. |
| Running it | Per session, plus an optional service | `scree serve` per Claude Code session keeps its own folder current. A systemd user unit running `scree watch` covers a folder outside any session. A Dockerfile ships for reproducibility. |

Library choices are starting points. Check each project's current docs before committing. One writer plus several reader processes under WAL was tested with vec1 and held up.

## How indexing works

Which folder and which index:

- `-notes <dir>` names the folder. Without it, the CLI and `serve` under Claude Code use the working directory, lifted to the git root above it (the nearest folder with a `.git` you own; see Ignoring files). Outside git, that folder itself is used. `serve` also lifts `CLAUDE_PROJECT_DIR` the same way and refuses to start unless it lands in the same repository, and refuses a folder that has not opted in (see Architecture). `serve` with neither `CLAUDE_PROJECT_DIR` nor `-notes` exits with an error.
- A folder found this way, not given with `-notes`, is refused if it is your home directory, a folder above it, or a filesystem root, so a dotfiles repo at `~` cannot pull in everything below it. Home comes from the password database, with `$HOME` only as a fallback. Under Claude Code the error does not suggest `-notes`, which `serve` refuses there.
- The index is `$XDG_DATA_HOME/scree/indexes/<base>-<hash16>/index.db`, where `hash16` is the first 16 hex digits of the SHA-256 of the resolved folder path and `<base>` is the folder's name made safe. A `root` file beside it holds the folder's path, and `writer.lock` sits next to it. `-db` or `SCREE_DB` names another file. The index file, its `-wal` and `-shm` files and `writer.lock` must be regular files, never symlinks (`writer.lock` is opened with `O_NOFOLLOW` where the OS has it), and an index folder, or the `scree` or `indexes` folder above it, that is a symlink is refused.
- The index records its folder and refuses to be used for a different one, since syncing it against another folder would delete that folder's rows. An index from the single-folder version, which recorded none, is adopted by the first folder that opens it.

Then:

1. When the writer starts (the first session in a repo, a takeover, or `scree watch`), compare every file's content hash to the index and queue anything new or changed. For a new index larger than `SCREE_FIRST_SYNC_MB`, a session or `watch` skips this until `scree reindex` is run, and search says so.
2. The watcher queues a path on each file event, ignoring editor swap and temp files.
3. Wait 1.5 s after the last event for that path, since editors fire several per save.
4. Hash the file. If the hash matches the index, stop.
5. Split by heading, embed the chunks, then delete the file's old rows and insert the new ones, with each chunk's line range and text hash. The FTS5 index is maintained by triggers in the same transaction. Within a section, a chunk is closed at a blank line once it reaches about 2500 characters, and it is hard-capped at 3500, even mid-line or inside a code fence; an overlong line is cut on a character boundary. Each heading is capped at 120 characters. A chunk's line range is its first and last non-blank source line.
6. On delete or rename, remove the old path's rows.

An index from an earlier version is upgraded in place when it is opened: line ranges are filled in on the next sync without re-embedding when the file is unchanged, text hashes are computed from stored chunks, and the keyword index is built from stored chunks (about a second per 100,000 chunks). The first process to open it does this inside one transaction; others wait.

The watcher re-adds its watches after a folder is renamed, so edits under the new name are seen. It never watches a folder the policy skips (hidden, gitignored, a nested repository, or excluded by `.screeignore`), and it re-adds its watches after any ignore file changes. It exits with an error when the index file or the indexed folder is removed or replaced. `scree watch` then exits, so its supervisor (systemd, Docker) restarts it. In `serve`, the writer stops, releases the lock and reports the error in search results, while searches continue on what is indexed. File events are read on their own goroutine and never wait for the embedding API; if the kernel's event queue still overflows, the writer resyncs.

One process writes to each index: whichever `scree serve` or `scree watch` holds `writer.lock` next to it (flock on Linux, macOS and BSD; `LockFileEx` on Windows). The others only read, and they retry the lock every 5 s, so when the writing session ends another takes over and syncs first. `serve` starts this in the background: the MCP handshake and searches are never blocked, and while the first sync of a new index runs, search results end with a note saying the index is incomplete. Files are indexed one at a time, so a large re-index cannot spike memory. `reindex -all` refuses to run while another process holds the lock.

A file is opened only after checking that what was opened is the regular file the policy checked; a file swapped for a symlink during a sync is skipped and retried.

Shutdown waits at most 5 s for the writer, so a stuck file read cannot keep `serve` alive after Claude Code closes it. A second signal exits at once. Files are opened without blocking, so a FIFO is never waited on, and no lock is held while an ignore file is read.

## What gets indexed

Markdown files (`.md`, `.markdown`) anywhere in the indexed folder, including the root forge, CI and agent folders listed under Ignoring files (pull-request templates, `.claude/agents`, Cursor and Codex rules written in markdown), minus whatever the ignore layers exclude. Nothing else: no code, no YAML, no JSON.

## Ignoring files

An ignored file is never read for indexing, never sent to the embedding API, never returned by `search_docs`, and refused by `read_doc` and `reindex(path)`. Ignoring is the way to keep private files from leaving the machine.

Four layers, checked in this order:

1. **Built-in rules, always on:**
   - Hidden files and folders, meaning any path segment starting with `.` (`.git`, `.obsidian`, `.trash`), with two exceptions. `.screeignore` itself is watched but never indexed. And these folders **at the root** of the indexed folder are searched like any other: `.claude/` (except `.claude/worktrees/`, which holds full copies of the repo), `.github/`, `.gitlab/`, `.gitea/`, `.forgejo/`, `.cursor/`, `.codex/`, `.gemini/`, `.windsurf/`, `.circleci/` and `.buildkite/`. The same name deeper in the tree, and any hidden segment inside them, stays hidden. Every other rule applies inside them.
   - Anything that isn't `.md` or `.markdown`.
   - Symlinks whose target resolves outside the indexed folder. A symlink inside it is followed like a normal file, and every rule here, including the extension and `.screeignore`, applies to both the link path and the resolved target. Symlinked folders are never descended.
   - Files larger than 1 MB, which are usually exports or logs rather than notes. `SCREE_MAX_FILE_BYTES` changes the limit.
   - File names containing control characters or invalid UTF-8 (unsafe name).
   - Non-regular files such as FIFOs and devices, which are skipped without being opened; `doctor` reports them as unreadable.
2. **Git's ignore rules**, when the folder is inside a git work tree (a `.git` folder or file in it or any folder above it): every `.gitignore` from the work tree's top down, `.git/info/exclude`, and the global excludes file (`core.excludesFile` from `~/.gitconfig` or `$XDG_CONFIG_HOME/git/config`, default `$XDG_CONFIG_HOME/git/ignore`; `[include]` is not followed). Git's semantics apply: a pattern is relative to the folder of its `.gitignore`, a deeper file overrides a shallower one, and a file inside an excluded folder cannot be re-included, so excluded folders such as `node_modules/` are never descended. A folder that holds its own `.git` (a nested repository or submodule) is skipped. One divergence: a tracked file that matches a pattern is still ignored, since scree does not read git's index. A `.gitignore` that cannot be read excludes its folder. `-gitignore=false` turns this layer off. Git's files are read the way git reads them: a `.gitignore`, `.git/info/exclude` and the `.git` file are never followed through a symlink and must be regular files. The global excludes file is taken only from your own git config (`~/.gitconfig` or `$XDG_CONFIG_HOME/git/config`), never from a repository's `.git/config`. A work tree is trusted only if its `.git` is owned by you and not a symlink, and the git dir or common dir a `.git` file names must be a folder you own, not a symlink. scree never looks for a `.git` in or above a shared sticky folder such as `/tmp`. A `.git` owned by someone else is an error, not a skip; `-notes <folder> -gitignore=false` on the command line indexes a folder below it without git's rules. Windows has no ownership check. Messages about these files name the file and line, never its content.
3. **`.screeignore`** at the root of the indexed folder, using gitignore syntax: one pattern per line, `#` comments, `!` to re-include, a leading `/` anchors to the root, a trailing `/` matches folders only, and `**` matches any depth. Only the root file is read, not nested ones. It is applied after git's rules and can only exclude more. Example:

   ```
   # journal stays local
   journal/
   **/private-*.md
   /inbox/scratch.md
   !journal/public.md
   ```

   Unlike git, `!` can re-include a file inside an excluded folder, which the example relies on. A `!` rule that matches only a folder above the file undoes folder exclusions but never a rule that matches the file itself, so `*.private.md` stays ignored under `!archive/2024/`. `doctor -v` lists every file a `!` rule re-included, with the rule's line number. `doctor` reports bad lines by number only.

   If `.screeignore` exists but cannot be read, is a symlink or not a regular file, or has a line scree cannot parse (a bad pattern or `include-hidden` directive), nothing is indexed until it is fixed: sync, `reindex` and the watcher stop indexing with an error naming the lines by number (in the log, and in search output when it happens at a session's start), and resume once the file is fixed. Scree fails closed rather than ignoring the rules.

   A line `include-hidden .name/` adds another root folder to that list (one segment starting with `.`; `.git` is refused; write `\include-hidden` for a literal pattern). To drop a default, exclude it with an ordinary pattern such as `/.cursor/`.
4. **Front matter opt-out** for a single file, without touching the ignore file:

   ```
   ---
   scree: ignore
   ---
   ```

   The key may be quoted and followed by spaces before the colon; the value `ignore` is matched in any case. The file has to be read to see this, but nothing is chunked or embedded. If the file is already indexed, it is removed.

Every rule is checked against the file's name as stored on disk, never the spelling a caller passed. `read_doc`, `reindex` and the watcher first map a path to its on-disk name. Names and patterns are compared in Unicode NFC, and case-insensitively when the filesystem is (macOS and Windows by default). Root hidden folders in the allowlist must match exactly, so `.GITHUB/` stays hidden.

When the rules change, the index follows:

- Editing `.screeignore` triggers a sync. Newly ignored files are removed from the index, and newly included ones are indexed.
- Adding `scree: ignore` to a file removes it on the next save. Removing the line re-indexes it.
- Editing a `.gitignore` at any level triggers a sync, like `.screeignore`. Changes to `.git/info/exclude` or the global excludes file are picked up on the next sync (session start, `reindex`, or any ignore-file edit).

`scree doctor` reports how many files each layer ignored, including `.gitignore` (with nested repositories counted separately). A hidden folder or a folder `.screeignore` excludes outright counts as one entry. `scree doctor -v` lists the ignored paths, so a pattern can be checked before it matters.

## Tools and commands

MCP tools:

```
search_docs(query, k, mode?)        mode: auto (default) | hybrid | semantic | keyword
read_doc(path, start?, end?)
reindex(path?)
```

Search returns the top chunks with file path, line range and heading, so Claude can read just those lines, or the whole file only when it needs to. Each result is an element:

```
<hit n="1" path="sub/a.md" lines="12-19" heading="Projects > scree" score="0.912" match="both">
chunk text
</hit>
```

- `k` defaults to 8, at most 50.
- **Modes.** `semantic` is a vector search. `keyword` is an SQLite FTS5 search over chunk text, heading and path, ranked by BM25 with text weighted twice as much as heading or path; it needs no embedding call and works while the API is down. `hybrid` runs both, each fetching up to `min(max(4k, 40), 200)` chunks, and merges them by reciprocal rank fusion (`Σ 1/(60 + rank)`, equal weights). `auto`, the default, runs `hybrid` when the query has an identifier-like word (after trimming quotes, brackets, backticks and `.,;:?!`: at least one ASCII letter and at least one `_` or ASCII digit, as in `PURGE_OLD_RECORDS`, `R7-Q2` or `v2`; not `ID`, `API`, `2026` or `well-known`) and `semantic` otherwise, and its output ends with `[mode: auto → hybrid]` or `[mode: auto → semantic]`. A failed embedding call returns an error that says `mode="keyword"` still works.
- **Why auto.** On a private set of 16 plain-language and 7 identifier queries over real notes, `auto` kept plain-question file MRR at 0.89 while raising identifier MRR from 0.46 to 0.80. Always-on `hybrid` matched the identifier gain but dropped plain questions (0.76 at equal weights, 0.83 with the keyword side at half weight).
- **The query is never FTS5 syntax.** It is split on whitespace; each word has control characters stripped and `.,;:!?()[]{}"'` trimmed from its ends, at most 32 words of at most 64 bytes are kept, and each becomes a quoted string with inner quotes doubled, joined with `OR`. Operators such as `AND`, `NEAR`, `*`, `^` and `col:` are plain text inside quotes.
- `lines` is the range of source lines the chunk came from, first and last non-blank line, so a client can read exactly that range (`read_doc(path, start, end)`, or an editor). If the file changed since it was indexed, search finds the chunk in the current file (exactly, then ignoring whitespace differences, choosing the occurrence nearest the stored line) and reports the current range; each changed file is read at most once per search. If the text is no longer there, the hit says `stale="true"` and has no range. Changed files are queued for re-indexing when this process is the writer.
- `match` says which search found the hit (`semantic`, `keyword` or `both`). `score` is the cosine similarity, present only when the vector search found the chunk.
- Identical chunks in several files (for example committed build copies) come back as one hit. The primary is the copy with the fewest path segments, then the shortest path, then the first in byte order, and `also="path:12-18, …"` lists up to three others that the current rules allow, with `also_more="N"` for the rest. A listed copy whose file changed is located in the current file like the primary (each file is still read at most once per search) and queued for re-indexing; if its text is gone it is written `path:stale`. A path containing `,`, `:` or a quote is written as a quoted string. Excluding build folders with `.screeignore` is still better: it saves embedding cost and keeps the index small.
- `path` and `heading` are quoted strings. Control characters are stripped, and any `<hit` or `</hit` inside file text becomes `&lt;hit`, so a file cannot forge a result.
- A hit's text is capped at 4000 characters (with `[cut]`) and one response at 64 KB; the rest is replaced by a `[N more hits omitted: output limit]` line.
- `read_doc` takes optional 1-based `start` and `end` lines (inclusive; `end` past the last line is clamped, `start` alone reads to the end) and returns `<doc path="…" lines="a-b">…</doc>`, or `<doc path="…">` for the whole file, treated the same way, truncated at 256 KB.
- The server instructions name the project folder, say that results are its markdown files with paths relative to its root (including the root forge and agent folders such as `.github/` and `.claude/`), explain the search modes, and tell the client that text inside `<hit>` and `<doc>` is file content, possibly copied from elsewhere, and is data, not instructions. Agent folders such as `.claude/agents` and `.cursor/rules` hold imperative text written for models, so their hits are especially prompt-shaped; the instructions say so.
- While the first sync of a new index runs, or when another process is building it, search output ends with a one-line `[index incomplete: …]` note; a folder over the first-sync cap gets a `[not indexed: …]` note instead. With another process holding the writer lock, `reindex` reports that process instead of writing.
- Search re-checks every hit against the current rules before returning it, so a newly ignored, opted-out or deleted file drops out at once, before any re-sync.

CLI:

```
scree watch
scree serve
scree serve -http addr [-allow-remote]
scree search [-k n] [-mode auto|hybrid|semantic|keyword] [-format xml|json] "query"
scree reindex [-all] [path]
scree doctor [-v] [-offline]
scree indexes [-prune]
```

`-format json` prints one object per hit, the stable format for evals: `{"n","mode","path","start","end","heading","score","match","stale","also","also_more"}`, with `mode` the mode that ran, `null` for an absent line or score, `also` a list of `{"path","start","end"}` (plus `"stale":true` on a copy whose text is gone), and `also_more` present only when it is not zero.

`scree indexes` lists every index with its folder, size and last sync; `-prune` deletes those whose folder is gone. It also shows each index's file and chunk counts, its model, and whether a writer holds it. Listing never writes: it reads each index over a read-only connection without upgrading it, and checks the lock without creating it. `-prune` keeps an index a writer holds, works only on real directories directly inside the indexes folder, and never follows a symlink out of it. It also points out an index left by the single-folder version. Each value it prints from an index's root file or meta table is cut at 80 characters.

Every command but `indexes` takes `-notes <dir>`, `-db <file>` and `-gitignore=false`. See How indexing works for how the folder is chosen when `-notes` is absent. `-notes`, `-db` and `SCREE_DB` are refused by `serve` when `CLAUDE_PROJECT_DIR` is set: under Claude Code, a session's folder and index follow from the repo alone.

`doctor` checks the configuration, the folder and what each ignore layer drops, the size and an upper-bound cost estimate, the index and its keyword index (`-v` also runs FTS5's integrity check and lists every ignored path), the writer, the model and, unless `-offline`, one test call to the API.

`reindex -all` rebuilds from scratch after a model or chunking change, and refuses to run while another process holds the writer lock.

HTTP transport (`serve -http`):

- Loopback addresses only by default. A non-loopback address needs `-allow-remote` and `SCREE_HTTP_TOKEN` (at least 16 characters); with a token set, every request needs `Authorization: Bearer <token>`, on loopback too.
- Cross-origin browser requests are rejected (`http.CrossOriginProtection`), before the token check.
- 10 s to read request headers, 120 s idle timeout, 64 KB of headers. No read or write timeout, since a session's event stream is long-lived.

## Resource budget

Target roughly 20–50 MB of memory while idle. Measured idle memory is about 13–15 MB per process for a small index and about 20 MB with 1000 files (4000 chunks); adding the keyword index did not change it, since both indexes are on disk. Hence no local embedding model, no in-memory vector store, no Docker Desktop VM. Reducing the embedding to 512 dimensions, if the model supports it, shrinks disk and memory further.

The keyword index (external-content FTS5, so the text is not stored twice) added about half the size of the chunk text on disk in a 1000-file measurement (+16% on the whole index); line ranges and text hashes add about 30 bytes per chunk.

Cost: indexing measured about $0.0025 per MB of markdown with `openai/text-embedding-3-small`, so a 10 MB folder costs about 2.5 cents to embed in full, and only changed files are re-embedded. `scree doctor` prints an estimate at four bytes per token, an upper bound of about 5 cents for 10 MB. A Claude Code session will not start the first index of a folder with more than `SCREE_FIRST_SYNC_MB` (default 50) of markdown; `scree reindex` in the folder does it explicitly. Each `serve` process stays within the budget. Only the writer holds file watches. A search embeds one query, except in `keyword` mode.

## Name

`scree` for the repo, module path, and binary. Unrelated `scree` exists on PyPI (ragged-tensor library); a Go module path doesn't conflict. Check npm, GitHub, and pkg.go.dev before publishing.

## Open items

- **Human view.** Decide between plain files in an editor and a small local search page over the same index.
- **MCP registry.** The license, CI, release workflow and OCI label are done. Still to do: cut `v0.1.0`, build an MCPB bundle, write `server.json` for `io.github.jrapakko/scree`, and publish with `mcp-publisher`.
- **Base image pinning.** The Dockerfile's `golang` and distroless images are referenced by tag. Pin them by digest once something like Dependabot keeps the digests current.
- **Docker and worktrees.** `scree mcp -docker` mounts only the work tree, so a worktree whose git dir lives in the main repository outside the mount cannot be read in the container, and it requires `CLAUDE_PROJECT_DIR` to lift to the same work tree, not just the same repository as native `serve` allows.
- **Docker on SELinux.** The only way to make the read-only mount readable is `SCREE_MOUNT_OPTS=ro,z` (or `ro,Z`), which permanently relabels every file in the folder on the host. A label-free option, such as `--security-opt label=disable` for that one container, is not offered yet. The container also gets no proxy variables; only the docker CLI does.
- **Launcher portability.** `scree mcp` and `scree run` replace the old `contrib/scree-*` shell wrappers. On Linux and other non-macOS Unix the home directory comes from `/etc/passwd`, then `/usr/bin/getent` (Go's `os/user` without cgo would fall back to `$HOME` for a user missing from `/etc/passwd`, so it is not used there); a user neither finds fails closed. On macOS, `os/user` calls libSystem's `getpwuid_r`; a build with the `osusergo` tag, which would read `$HOME`, refuses to compile. Not yet tested on a real macOS host with Open Directory users, or on BSD. macOS release binaries are not signed with the hardened runtime, so `DYLD_INSERT_LIBRARIES` from a repo's `.mcp.json` still reaches the first process there; signing would close that. Windows is refused.
- **Tracked-but-ignored files.** Read `.git/index` so that tracked files matching a `.gitignore` pattern are indexed, as git considers them. Today they are skipped.
- **`[include]` in git config.** `core.excludesFile` is read only from the three standard config files.
- **Several folders per session.** Index `--add-dir` folders only if each has opted in (for example its own `.mcp.json` entry), with a `root` attribute on hits.
- **Roots without `CLAUDE_PROJECT_DIR`.** For other clients, consider roots via MRTR input requests (protocol 2026-07-28) instead of `-notes`.
- Resolved: embedding model `openai/text-embedding-3-small` at 512 dimensions. It beat `perplexity/pplx-embed-v1-0.6b` on 16 questions over 75 real notes (file-level MRR 0.89 vs 0.78).
- Resolved: the SQLite driver plus vector extension builds without CGO, using vec1 instead of `sqlite-vec` (see Go stack).
- Resolved: the default search mode is `auto` (see Tools and commands).

## Explorations

Ideas to try before committing to them. Each one lists the questions an exploration has to answer.

- **Identifier routing.** The `auto` rule misses names with no `_` or digit, such as `psycopg` or a `CamelCase` type. Options, in order of preference:
  - **(c) A code-derived vocabulary, the primary option.** Collect names locally from dependency manifests (`requirements.txt`, `pyproject.toml`, `go.mod`, `package.json`, `Cargo.toml`), import statements and declarations (`def`, `class`, `func`, `type`, `const`, `export`). The same ignore policy applies, and the vocabulary is never embedded or sent to the API. A query word found in it counts as an identifier. Membership is the signal, not frequency.
  - **(b) A language server** (`workspace/symbol`): the fuller version of (c), at the cost of a process per language. Check whether Claude Code's own LSP support makes it unnecessary.
  - Rare FTS-vocabulary terms were considered and dropped: library names like `psycopg` are frequent in a repo's docs, so a frequency signal misses exactly them.
  - Questions: BM25 also discounts frequent terms, so does routing a frequent name to `hybrid` improve results at all, or is semantic search already enough? Measure on a real repo with both code and docs, since the notes-only eval sets can't. Does reading code (names only) change how scree should describe its scope?
- **Keyword tokenizer.** Whether `.` and `/` should join tokens (file names, paths), or a trigram side index for partial identifiers; measure with the identifier eval (`internal/index/testdata/eval/identifiers.tsv` for this repo's docs).
- **CI workflow YAML.** Index workflow files in the allowlisted forge and CI folders, one chunk per job, with a heading trail like `ci.yml > jobs.test`. Questions:
  - Which files count: GitHub, Gitea and Forgejo `workflows/`, `.gitlab-ci.yml` (and its includes?), `.circleci/config.yml`, `.buildkite/pipeline.yml`?
  - A YAML parser or a line-based split, given that chunk text must be the original lines with their line numbers?
  - Resistance to hostile YAML, such as alias expansion and deep nesting?
  - How to handle GitLab's shape, where top-level keys are jobs except reserved ones (`stages`, `variables`, `include`, `default`, …)?
  - Does retrieval actually improve for CI questions, measured the way the embedding models were compared?
- **Local and LAN embedding models.** Test scree against an OpenAI-compatible server on this machine (Ollama, llama.cpp) and on another machine on the LAN, through `SCREE_EMBED_URL` with `SCREE_ZDR=0`. Compare retrieval with the current default on the same real-notes question set, and measure the model server's memory. A LAN server should only be reached over TLS or a VPN such as WireGuard, since embedding requests carry note text in the clear.
