---
title: How it works
---

## Architecture

One Go binary. Under Claude Code, each session runs its own `scree serve` process, started through `scree mcp` by the repo's committed `.mcp.json`. Each process has three parts:

- **Writer** (fsnotify, hash, chunk): reads the folder's markdown, calls the embedding API, writes the index. Only the process holding the index's `writer.lock` runs it.
- **Index** (SQLite): chunks with their line ranges and text hashes, vectors in vec1, and an FTS5 keyword index, in one file on disk.
- **MCP server** (`search_docs`, `read_doc`, `reindex`): reads the index, embeds each query through the same API when the search mode needs it, serves Claude Code over stdio, or HTTP if asked.

```
                 Claude Code session A          Claude Code session B
                         | stdio                        | stdio
                         v                              v
                 +----------------+             +----------------+
                 |   scree mcp    |             |   scree mcp    |
                 |  (settings     |             |                |
                 |   file only,   |             |                |
                 |   clean env)   |             |                |
                 +-------+--------+             +-------+--------+
                         | exec                         | exec
                         v                              v
  +------------------------------------+   +------------------------------+
  | scree serve  (holds writer.lock)   |   | scree serve  (reader)        |
  |                                    |   |                              |
  |  MCP server --- search ---+        |   |  MCP server --- search --+   |
  |                           |        |   |                          |   |
  |  writer: watch -> hash -> chunk    |   |  retries the lock every  |   |
  |          -> embed -> write         |   |  5 s; takes over when A  |   |
  +-----+----------------+----+--------+   |  exits                   |   |
        |                |    |            +--------------------------+---+
        | fsnotify       |    |  read/write (WAL)          read       |
        v                |    v                                       v
  repo markdown          |   ~/.local/share/scree/indexes/<folder>-<hash>/
  (source of truth)      |      index.db   writer.lock   root
                         |
                         v  HTTPS: chunk text, path, heading; queries
                 embeddings API (OpenRouter, zero data retention requested)
```

`scree mcp` is part of the same static binary. It reads the settings file from your passwd home, then `exec`s its own binary as `scree serve` with an environment built only from that file (see [Configuration](configuration.md#what-scree-mcp-and-scree-run-accept-and-refuse)). The Go runtime reads a few variables such as `GODEBUG` before `main` runs, which is why the real work happens in the replacement process.

The folder is the git root of the working directory Claude Code starts the server in: the project, or the active worktree. A `.mcp.json` has no working-directory field, but its `env` is applied last and can override anything, `CLAUDE_PROJECT_DIR` and `PATH` included, so `CLAUDE_PROJECT_DIR` is only a cross-check: `serve` refuses to start unless it is in the same repository. `serve` also refuses a folder whose own root `.mcp.json` names no `scree mcp` server (see [Security](security.md#opt-in-is-enforced)).

MCP roots are not used: they are deprecated as of protocol version 2026-07-28, they include `--add-dir` folders that have not opted in, and the Go SDK refuses server-initiated `roots/list` on that version. Outside Claude Code, `-notes <dir>` names the folder, and the CLI defaults to the git root of the current directory.

The Go stack: `fsnotify` for file events, the official Go MCP SDK, and `ncruces/go-sqlite3`, which ships SQLite, vec1 and FTS5 compiled to WebAssembly so the binary needs no CGO.

## Chunking

Each file is split by ATX heading (`#` to `######`), after skipping YAML front matter. Headings inside fenced code blocks are text.

- Each chunk keeps the heading trail above it, such as `API > Authentication > Tokens`. Each heading is capped at 120 characters.
- Within a section, a chunk is closed at a blank line outside a code fence once it reaches about 2500 characters, and hard-capped at 3500, even mid-line or inside a code fence. An overlong line is cut on a character boundary.
- A chunk that is only a heading is dropped.
- Each chunk records its first and last non-blank source line and a hash of its text. The hash is how identical chunks in different files are found.

What is embedded is the path, the heading trail and the chunk text together (`docs/api.md\nAPI > Authentication\n\n<text>`), so the vector carries context the text alone may lack.

## Embedding and zero data retention

Chunks go to the embeddings endpoint in batches of 64. With `SCREE_ZDR=1` (the default), every request carries `"provider": {"zdr": true, "data_collection": "deny"}`, which asks OpenRouter to route only to endpoints with zero data retention that don't collect data. If the model has no such endpoint, the call fails with an error naming the model; it never falls back to one that keeps data. Keyword search runs locally and sends nothing.

| | Timeout per attempt | Attempts |
| --- | --- | --- |
| Search query | 10 s | 2 |
| Indexing | 30 s | 3 |

Network errors, 429 and 5xx are retried; a timed-out attempt is not. After an outage, indexing pauses for 30 s, doubling up to 5 minutes, and then resyncs, so files changed during the outage are picked up without another edit. Error messages never contain the API key: it is scrubbed before any message is cut or printed.

## Storage

One SQLite file per folder, in WAL mode, with a `meta` table (model, folder, last sync), a `files` table (path, content hash, mtime, size), a `chunks` table (text, heading, line range, text hash), a vec1 virtual table holding one vector per chunk, and an external-content FTS5 table over chunk text, heading and path, kept in sync by triggers in the same transaction. vec1 uses a flat, exact cosine index. At 512 dimensions a vector is 2 KB. The keyword index adds roughly half the size of the chunk text on disk, since it does not store the text twice.

FTS5 uses the `unicode61` tokenizer with diacritics removed and `_` and `-` as token characters, so identifiers such as `SCREE_FIRST_SYNC_MB` stay one token.

Index location: `$XDG_DATA_HOME/scree/indexes/<folder>-<hash>/`, holding `index.db`, `writer.lock` and `root`, a one-line file with the folder's resolved path that `scree indexes` reads. `<hash>` is the first 16 hex digits of SHA-256 of that path. The index directory is created with mode 0700 and the files 0600. The index file, its `-wal` and `-shm` files and `writer.lock` are never opened through a symlink, and an index folder, or the `scree` or `indexes` folder above it, that is a symlink is refused.

The index records the folder and the model. Opening it for another folder (through `-db` or `SCREE_DB`) is an error, since a sync would delete the other folder's rows. An index from the single-folder version with no recorded folder is adopted by the first folder that opens it.

An index from an earlier version is upgraded in place when it is opened: line ranges are filled in on the next sync without re-embedding unchanged files, and text hashes and the keyword index are built from the stored chunks (about a second per 100,000 chunks). The first process to open it does this inside one transaction; others wait.

## The writer lock

One process writes each index: whichever `scree serve` or `scree watch` holds `writer.lock` (flock on Linux, macOS and the BSDs; `LockFileEx` on Windows; on other systems there is no locking and a warning is logged). The others only read and retry every 5 s. When the writer exits, the next one to get the lock syncs first. CLI `reindex` takes the lock too, and `reindex -all` refuses to run while another process holds it.

The lock relies on the filesystem's locking, so on NFS and some network filesystems it may not hold. SQLite's own locking still keeps the file consistent; the cost would be duplicate embedding calls, not corruption.

## Background sync

When the writer starts (the first session in a repo, a takeover, or `scree watch`), it compares every file's SHA-256 with the index, embeds only new and changed files, and removes rows for deleted or newly ignored ones. Files are indexed one at a time, so a large sync doesn't spike memory. Each file's old rows are replaced in one transaction.

In `serve` this runs in the background: the MCP handshake and searches never wait, and search output says the index is incomplete until the first sync finishes. For a new, empty index with more than `SCREE_FIRST_SYNC_MB` (default 50) of markdown, a session or `watch` skips the first sync and search says so; `scree reindex` indexes it on purpose.

## The watcher

After the sync, the writer watches the folder with fsnotify:

- **Event loop and worker.** File events are read on their own goroutine, which never waits for the embedding API. The loop debounces them into a set of dirty paths; a worker indexes them. Editor swap and temp files are ignored.
- **Debounce.** A path is indexed 1.5 s after its last event, since editors fire several events per save.
- **Pruning.** It never watches a folder the policy skips: hidden, gitignored, a nested repository, or excluded by `.screeignore`. So `node_modules/` costs no watches.
- **Re-adding.** Watches are re-added after a folder is renamed and after any ignore file changes.
- **Overflow.** If the kernel's event queue overflows anyway, the writer resyncs and rebuilds its watches.
- **Health.** Every 10 s it checks that the folder and the index file are still the ones it started with. If either was removed or replaced, `scree watch` exits with an error, so systemd or Docker restarts it; in `serve`, the writer stops, releases the lock, and search reports `[index not updating: …]` while it keeps serving what is indexed.

Files are opened without blocking, so a FIFO is never waited on, and a file is used only after checking that what was opened is the regular file the policy checked. On shutdown, `serve` and `watch` wait at most 5 s for the writer; a second signal exits at once.

## Search

The mode decides which sides run (see [search modes](using-with-claude-code.md#search-modes)); `auto` picks `hybrid` for a query with an identifier-like word and `semantic` otherwise.

1. **Fetch.** Each side fetches up to `min(max(4k, 40), 200)` chunks, over-fetching because some hits are dropped and duplicates folded. The vector side embeds the query (10 s timeout, 2 attempts) and ranks chunks by cosine distance. The keyword side runs an FTS5 query over chunk text, heading and path, ranked by BM25 with the text weighted twice as much as heading or path. The query is turned into quoted terms joined with `OR`: at most 32 words of at most 64 bytes, with control characters stripped and punctuation trimmed from their ends.
2. **Merge.** In `hybrid`, the two lists are merged by reciprocal rank fusion: a chunk scores `Σ 1/(60 + rank)` over the lists it appears in, with equal weights. A chunk found by both is marked `match="both"`.
3. **Re-check.** Each hit's file is re-checked against the current rules, with a stat, plus a full read when the file's mtime or size differs from what was indexed. Refused, deleted or opted-out files are dropped, and the writer is told to re-check them.
4. **Relocate.** For a file that changed, the chunk is looked up in the current text, exactly and then ignoring whitespace, nearest the stored line, and the hit gets the current line range, or `stale="true"` if the text is gone. Each file is read at most once per search, and changed files are queued for re-indexing.
5. **Fold.** Chunks with the same text hash in several files become one hit, shown at the copy with the fewest path segments, then the shortest path, then the first in byte order, with up to three others in `also` and the rest counted in `also_more`. Only copies the current rules allow are listed.
6. The first `k` survivors are returned. An empty index returns nothing without calling the API.

## Resource use

Measured idle memory is about 13–15 MB per process for a small index and about 20 MB with an index of 1000 files (4000 chunks), against a target of 20–50 MB. Adding the keyword index did not change it, since both indexes are on disk. That budget is why there is no local embedding model, no in-memory vector store and no Docker Desktop VM: vectors stay on disk, and a reader process holds no file watches. After a first sync, the writer hands its heap back to the OS.

## Cost

Indexing costs about $0.0025 per MB of markdown with the default model, `openai/text-embedding-3-small` (listed at about $0.02 per million tokens), so a 10 MB folder costs about 2.5 cents to embed in full. It is paid once: after that only changed files are re-embedded, and `reindex -all` pays it again. A search embeds one short query, except in `keyword` mode, which makes no API call.

`doctor` prints an estimate for a folder, computed at four bytes per token and $0.02 per million tokens, about $0.005 per MB. That is deliberately an upper bound.
