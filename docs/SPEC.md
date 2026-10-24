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
| Embeddings | API through OpenRouter | No local model. Candidates: pplx-embed-v1-0.6b (~$0.004/M tokens), text-embedding-3-small (~$0.02/M). Test both on real notes. |
| MCP | Official Go MCP SDK | stdio for Claude Code; optional HTTP. |
| Chunking | By heading | Each chunk keeps its file path and heading trail. |
| Running it | Native background service | launchd or a systemd user unit. A Dockerfile can ship for reproducibility. |

Library choices are starting points. Check each project's current docs before committing. One writer plus a separate reader process under WAL was tested with vec1 and held up.

## How indexing works

1. On startup, compare every file's content hash to the index and queue anything new or changed.
2. The watcher queues a path on each file event, ignoring editor swap and temp files.
3. Wait one to two seconds after the last event for that path, since editors fire several per save.
4. Hash the file. If the hash matches the index, stop.
5. Split by heading, embed the chunks, then delete the file's old rows and insert the new ones.
6. On delete or rename, remove the old path's rows.

One process writes to the index. Files are indexed one at a time, so a large re-index cannot spike memory.

## Ignoring files

An ignored file is never read for indexing, never sent to the embedding API, never returned by `search_notes`, and refused by `read_note` and `reindex(path)`. Ignoring is the way to keep private notes from leaving the machine.

Three layers, checked in this order:

1. **Built-in rules, always on:**
   - Hidden files and folders, meaning any path segment starting with `.` (`.git`, `.obsidian`, `.trash`). The one exception is `.screeignore` itself, which is watched but never indexed.
   - Anything that isn't `.md` or `.markdown`.
   - Symlinks whose target resolves outside the notes folder. A symlink inside it is followed like a normal file.
   - Files larger than 1 MB, which are usually exports or logs rather than notes. `SCREE_MAX_FILE_BYTES` changes the limit.
2. **`.screeignore`** at the root of the notes folder, using gitignore syntax: one pattern per line, `#` comments, `!` to re-include, a leading `/` anchors to the root, a trailing `/` matches folders only, and `**` matches any depth. Only the root file is read, not nested ones. Example:

   ```
   # journal stays local
   journal/
   **/private-*.md
   /inbox/scratch.md
   !journal/public.md
   ```
3. **Front matter opt-out** for a single note, without touching the ignore file:

   ```
   ---
   scree: ignore
   ---
   ```

   The file has to be read to see this, but nothing is chunked or embedded. If the note is already indexed, it is removed.

When the rules change, the index follows:

- Editing `.screeignore` triggers a sync. Newly ignored files are removed from the index, and newly included ones are indexed.
- Adding `scree: ignore` to a note removes it on the next save. Removing the line re-indexes it.
- `.gitignore` is not read. Notes folders often ignore files in git for reasons that have nothing to do with search.

`scree doctor` reports how many files each layer ignored. `scree doctor -v` lists the ignored paths, so a pattern can be checked before it matters.

## Tools and commands

MCP tools:

```
search_notes(query, k)
read_note(path)
reindex(path?)
```

Search returns the top chunks with file path and heading, so Claude can open the full note only when it needs to.

CLI:

```
scree watch
scree serve
scree search "query"
scree reindex --all
scree doctor
```

`reindex --all` rebuilds from scratch after a model or chunking change.

## Resource budget

Target roughly 20–50 MB of memory while idle. Hence no local embedding model, no in-memory vector store, no Docker Desktop VM. Reducing the embedding to 512 dimensions, if the model supports it, shrinks disk and memory further.

Cost is negligible: a 10 MB folder of notes is about 2.5M tokens, a fraction of a cent on the cheapest models, and only changed files are re-embedded.

## Name

`scree` for the repo, module path, and binary. Unrelated `scree` exists on PyPI (ragged-tensor library); a Go module path doesn't conflict. Check npm, GitHub, and pkg.go.dev before publishing.

## Open items

- Pick an embedding model by running two or three on real notes.
- Decide how the human view works: plain files in an editor, or a small local search page over the same index.
- Resolved: the SQLite driver plus vector extension builds without CGO, using vec1 instead of `sqlite-vec` (see Go stack).
