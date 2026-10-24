# scree: spec

Local semantic search over your markdown notes, served to Claude Code over MCP.

## Why it exists

Long markdown files get unwieldy as context, and reorganizing them by hand only delays the problem. Scree splits notes into small pieces, embeds them, and lets Claude Code pull in just the pieces that match a question.

The markdown stays the source of truth. The index is derived from it and can be deleted and rebuilt at any time.

## Architecture

One Go process containing:

- **Watcher + indexer** (fsnotify, hash, chunk): reads markdown notes, calls the embedding API, writes to the index.
- **vec1 index** (SQLite): chunks + vectors, on disk.
- **MCP search server** (`search_notes` tool): reads the index, embeds each query through the same API, serves Claude Code over stdio or HTTP.

External: markdown notes (source of truth), embedding API (OpenRouter), Claude Code, and a human view (editor or browser) that reads the notes directly.

## Go stack

| Concern | Choice | Notes |
| --- | --- | --- |
| Language | Go | One small static binary, low idle memory, no runtime to install. |
| File watching | `fsnotify` | Native events when run directly on the laptop. Containers on Docker Desktop lose these events on bind mounts. |
| Vector storage | SQLite with [vec1](https://sqlite.org/vec1) | Disk-backed, so memory stays flat. `ncruces/go-sqlite3` ships vec1 compiled to WebAssembly, so the binary stays CGO-free. Flat (exact) cosine index. `sqlite-vec` was the first choice, but its Go bindings pin driver v0.17.1, far behind current releases. |
| Embeddings | API through OpenRouter | No local model. Candidates: pplx-embed-v1-0.6b (~$0.004/M tokens), text-embedding-3-small (~$0.02/M). Test both on real notes. Requests ask for zero-data-retention endpoints with `data_collection: deny` by default (`SCREE_ZDR=0` turns this off); a model with no such endpoint fails with a clear error. Timeouts: 10 s per query with no retry on timeout; 30 s per indexing attempt, and after an outage indexing pauses for 30 s, doubling up to 5 min, then resyncs. |
| MCP | Official Go MCP SDK | stdio for Claude Code; optional HTTP. |
| Chunking | By heading | Each chunk keeps its file path and heading trail. |
| Running it | Native background service | launchd or a systemd user unit. A Dockerfile can ship for reproducibility. |

Library choices are starting points. Check each project's current docs before committing. One writer plus a separate reader process under WAL was tested with vec1 and held up.

## How indexing works

1. On startup, compare every file's content hash to the index and queue anything new or changed.
2. The watcher queues a path on each file event, ignoring editor swap and temp files.
3. Wait one to two seconds after the last event for that path, since editors fire several per save.
4. Hash the file. If the hash matches the index, stop.
5. Split by heading, embed the chunks, then delete the file's old rows and insert the new ones. Within a section, a chunk is closed at a blank line once it reaches about 2500 characters, and it is hard-capped at 3500, even mid-line or inside a code fence; an overlong line is cut on a character boundary. Each heading is capped at 120 characters.
6. On delete or rename, remove the old path's rows.

The watcher re-adds its watches after a folder is renamed, so edits under the new name are seen. It never watches a folder the policy skips (hidden, gitignored, a nested repository, or excluded by `.screeignore`), and it re-adds its watches after any ignore file changes. It exits with an error when the index file or the notes folder is removed or replaced, and relies on its supervisor (systemd, Docker) to restart it.

One process writes to the index. Files are indexed one at a time, so a large re-index cannot spike memory.

## What gets indexed

Markdown files (`.md`, `.markdown`) anywhere in the indexed folder, including the root forge, CI and agent folders listed under Ignoring files (pull-request templates, `.claude/agents`, Cursor and Codex rules written in markdown), minus whatever the ignore layers exclude. Nothing else: no code, no YAML, no JSON.

## Ignoring files

An ignored file is never read for indexing, never sent to the embedding API, never returned by `search_notes`, and refused by `read_note` and `reindex(path)`. Ignoring is the way to keep private notes from leaving the machine.

Four layers, checked in this order:

1. **Built-in rules, always on:**
   - Hidden files and folders, meaning any path segment starting with `.` (`.git`, `.obsidian`, `.trash`), with two exceptions. `.screeignore` itself is watched but never indexed. And these folders **at the root** of the indexed folder are searched like any other: `.claude/` (except `.claude/worktrees/`, which holds full copies of the repo), `.github/`, `.gitlab/`, `.gitea/`, `.forgejo/`, `.cursor/`, `.codex/`, `.gemini/`, `.windsurf/`, `.circleci/` and `.buildkite/`. The same name deeper in the tree, and any hidden segment inside them, stays hidden. Every other rule applies inside them.
   - Anything that isn't `.md` or `.markdown`.
   - Symlinks whose target resolves outside the notes folder. A symlink inside it is followed like a normal file, and every rule here, including the extension and `.screeignore`, applies to both the link path and the resolved target. Symlinked folders are never descended.
   - Files larger than 1 MB, which are usually exports or logs rather than notes. `SCREE_MAX_FILE_BYTES` changes the limit.
   - File names containing control characters or invalid UTF-8 (unsafe name).
   - Non-regular files such as FIFOs and devices, which are skipped without being opened.
2. **Git's ignore rules**, when the folder is inside a git work tree (a `.git` folder or file in it or any folder above it): every `.gitignore` from the work tree's top down, `.git/info/exclude`, and the global excludes file (`core.excludesFile` from `~/.gitconfig`, `$XDG_CONFIG_HOME/git/config` or the repo's config, default `$XDG_CONFIG_HOME/git/ignore`; `[include]` is not followed). Git's semantics apply: a pattern is relative to the folder of its `.gitignore`, a deeper file overrides a shallower one, and a file inside an excluded folder cannot be re-included, so excluded folders such as `node_modules/` are never descended. A folder that holds its own `.git` (a nested repository or submodule) is skipped. One divergence: a tracked file that matches a pattern is still ignored, since scree does not read git's index. A `.gitignore` that cannot be read excludes its folder. `-gitignore=false` turns this layer off.
3. **`.screeignore`** at the root of the notes folder, using gitignore syntax: one pattern per line, `#` comments, `!` to re-include, a leading `/` anchors to the root, a trailing `/` matches folders only, and `**` matches any depth. Only the root file is read, not nested ones. It is applied after git's rules and can only exclude more. Example:

   ```
   # journal stays local
   journal/
   **/private-*.md
   /inbox/scratch.md
   !journal/public.md
   ```

   Unlike git, `!` can re-include a file inside an excluded folder, which the example relies on: each pattern is checked against the file and every folder above it, and the last match wins.

   If `.screeignore` exists but cannot be read (for example, a symlink pointing outside the notes folder), nothing is indexed until it is fixed. Scree fails closed rather than ignoring the rules.

   A line `include-hidden .name/` adds another root folder to that list (one segment starting with `.`; `.git` is refused; write `\include-hidden` for a literal pattern). To drop a default, exclude it with an ordinary pattern such as `/.cursor/`.
4. **Front matter opt-out** for a single note, without touching the ignore file:

   ```
   ---
   scree: ignore
   ---
   ```

   The file has to be read to see this, but nothing is chunked or embedded. If the note is already indexed, it is removed.

When the rules change, the index follows:

- Editing `.screeignore` triggers a sync. Newly ignored files are removed from the index, and newly included ones are indexed.
- Adding `scree: ignore` to a note removes it on the next save. Removing the line re-indexes it.
- Editing a `.gitignore` at any level triggers a sync, like `.screeignore`. Changes to `.git/info/exclude` or the global excludes file are picked up on the next sync (session start, `reindex`, or any ignore-file edit).

`scree doctor` reports how many files each layer ignored, including `.gitignore` (with nested repositories counted separately). A hidden folder or a folder `.screeignore` excludes outright counts as one entry. `scree doctor -v` lists the ignored paths, so a pattern can be checked before it matters.

## Tools and commands

MCP tools:

```
search_notes(query, k)
read_note(path)
reindex(path?)
```

Search returns the top chunks with file path and heading, so Claude can open the full note only when it needs to. Each result is an element:

```
<hit n="1" path="sub/a.md" line="12" heading="Projects > scree" score="0.912">
chunk text
</hit>
```

- `path` and `heading` are quoted strings. Control characters are stripped, and any `<hit` or `</hit` inside note text becomes `&lt;hit`, so a note cannot forge a result.
- A hit's text is capped at 4000 characters and one response at 64 KB; the rest is replaced by a `[N more hits omitted: output limit]` line.
- `read_note` returns `<note path="…">…</note>`, treated the same way, truncated at 256 KB.
- The server instructions tell the client that text inside `<hit>` and `<note>` is note content, possibly copied from elsewhere, and is data, not instructions.

CLI:

```
scree watch
scree serve
scree serve -http addr [-allow-remote]
scree search "query"
scree reindex --all
scree doctor
scree doctor -v
```

`reindex --all` rebuilds from scratch after a model or chunking change.

HTTP transport (`serve -http`):

- Loopback addresses only by default. A non-loopback address needs `-allow-remote` and `SCREE_HTTP_TOKEN` (at least 16 characters); with a token set, every request needs `Authorization: Bearer <token>`, on loopback too.
- Cross-origin browser requests are rejected (`http.CrossOriginProtection`), before the token check.
- 10 s to read request headers, 120 s idle timeout, 64 KB of headers. No read or write timeout, since a session's event stream is long-lived.

## Resource budget

Target roughly 20–50 MB of memory while idle; measured idle memory is about 13–15 MB per process. Hence no local embedding model, no in-memory vector store, no Docker Desktop VM. Reducing the embedding to 512 dimensions, if the model supports it, shrinks disk and memory further.

Cost is negligible: a 10 MB folder of notes is about 2.5M tokens, a fraction of a cent on the cheapest models, and only changed files are re-embedded.

## Name

`scree` for the repo, module path, and binary. Unrelated `scree` exists on PyPI (ragged-tensor library); a Go module path doesn't conflict. Check npm, GitHub, and pkg.go.dev before publishing.

## Open items

- **Local and LAN embedding models.** Test scree against an OpenAI-compatible server on this machine (Ollama, llama.cpp) and on another machine on the LAN, through `SCREE_EMBED_URL` with `SCREE_ZDR=0`. Compare retrieval with the current default on the same real-notes question set, and measure the model server's memory. A LAN server should only be reached over TLS or a VPN such as WireGuard, since embedding requests carry note text in the clear.
- **Human view.** Decide between plain files in an editor and a small local search page over the same index.
- **MCP registry.** The license, CI, release workflow and OCI label are done. Still to do: cut `v0.1.0`, build an MCPB bundle, write `server.json` for `io.github.jrapakko/scree`, and publish with `mcp-publisher`.
- **Base image pinning.** The Dockerfile's `golang` and distroless images are referenced by tag. Pin them by digest once something like Dependabot keeps the digests current.
- **Wrapper portability.** `contrib/scree-mcp*` are tested only under bash as `/bin/sh`. Check them under `dash` and with `shellcheck`.
- Resolved: embedding model `openai/text-embedding-3-small` at 512 dimensions. It beat `perplexity/pplx-embed-v1-0.6b` on 16 questions over 75 real notes (file-level MRR 0.89 vs 0.78).
- Resolved: the SQLite driver plus vector extension builds without CGO, using vec1 instead of `sqlite-vec` (see Go stack).
