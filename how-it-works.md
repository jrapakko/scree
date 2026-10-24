---
title: How it works
---

## Architecture

One Go binary. Under Claude Code, each session runs its own `scree serve` process, started by the repo's committed `.mcp.json`. Each process has three parts:

- **Writer** (fsnotify, hash, chunk): reads the folder's markdown, calls the embedding API, writes the index. Only the process holding the index's `writer.lock` runs it.
- **Index** (SQLite with vec1): chunks and vectors in one file on disk.
- **MCP server** (`search_docs`, `read_doc`, `reindex`): reads the index, embeds each query through the same API, serves Claude Code over stdio, or HTTP if asked.

```
                 Claude Code session A          Claude Code session B
                         | stdio                        | stdio
                         v                              v
                 +----------------+             +----------------+
                 |   scree-mcp    |             |   scree-mcp    |
                 |  (wrapper:     |             |                |
                 |   settings     |             |                |
                 |   file only)   |             |                |
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

The folder is the git root above `CLAUDE_PROJECT_DIR`, which Claude Code sets in the server's environment before the handshake. MCP roots are not used: they are deprecated as of protocol version 2026-07-28, they include `--add-dir` folders that have not opted in, and the Go SDK refuses server-initiated `roots/list` on that version. Outside Claude Code, `-notes <dir>` names the folder, and the CLI defaults to the git root of the current directory.

The Go stack: `fsnotify` for file events, the official Go MCP SDK (v1.8.0), and `ncruces/go-sqlite3`, which ships SQLite and vec1 compiled to WebAssembly so the binary needs no CGO.

## Chunking

Each file is split by ATX heading (`#` to `######`), after skipping YAML front matter. Headings inside fenced code blocks are text.

- Each chunk keeps the heading trail above it, such as `API > Authentication > Tokens`. Each heading is capped at 120 bytes.
- Within a section, a chunk is closed at a blank line outside a code fence once it reaches 2500 bytes, and hard-capped at 3500 bytes, even mid-fence. A line longer than 3500 bytes is cut on a character boundary into pieces of its own.
- A chunk that is only a heading is dropped.
- **New (step 6-1):** each chunk records its first and last non-blank source line. An unchanged file indexed before this is backfilled on the next sync without re-embedding, if re-splitting it gives exactly the stored chunks.

What is embedded is the path, the heading trail and the chunk text together (`docs/api.md\nAPI > Authentication\n\n<text>`), so the vector carries context the text alone may lack.

## Embedding and zero data retention

Chunks go to the embeddings endpoint in batches of 64. With `SCREE_ZDR=1` (the default), every request carries `"provider": {"zdr": true, "data_collection": "deny"}`, which asks OpenRouter to route only to endpoints with zero data retention that don't collect data. If the model has no such endpoint, the call fails with an error naming the model; it never falls back to one that keeps data.

| | Timeout per attempt | Attempts |
| --- | --- | --- |
| Search query | 10 s | 2 |
| Indexing | 30 s | 3 |

Network errors, 429 and 5xx are retried; a timed-out attempt is not. After an outage, indexing pauses for 30 s, doubling up to 5 minutes, and then resyncs, so files changed during the outage are picked up without another edit. Error messages never contain the API key: it is scrubbed before any message is cut or printed.

## Storage

One SQLite file per folder, in WAL mode, with a `meta` table (model, folder, last sync), a `files` table (path, content hash, mtime, size), a `chunks` table, and a vec1 virtual table holding one vector per chunk. vec1 uses a flat, exact cosine index. At 512 dimensions a vector is 2 KB.

Index location: `$XDG_DATA_HOME/scree/indexes/<folder>-<hash>/`, holding `index.db`, `writer.lock` and `root`, a one-line file with the folder's resolved path. `<hash>` is the first 16 hex digits of SHA-256 of that path. The index directory is created with mode 0700 and the files 0600.

The index records the folder and the model. Opening it for another folder (through `-db` or `SCREE_DB`, or a hash collision) is an error, since a sync would delete the other folder's rows. An index from the single-folder version with no recorded folder is adopted on first write.

## The writer lock

One process writes each index: whichever `scree serve` or `scree watch` holds `writer.lock` (flock on Linux, macOS and the BSDs; `LockFileEx` on Windows; on other systems there is no locking and a warning is logged). The others only read and retry every 5 s. When the writer exits, the next one to get the lock syncs first. CLI `reindex` takes the lock too, and `reindex -all` refuses to run while another process holds it.

The lock relies on the filesystem's locking, so on NFS and some network filesystems it may not hold. SQLite's own locking still keeps the file consistent; the cost would be duplicate embedding calls, not corruption.

## Background sync

When the writer starts (the first session in a repo, a takeover, or `scree watch`), it compares every file's SHA-256 with the index, embeds only new and changed files, and removes rows for deleted or newly ignored ones. Files are indexed one at a time, so a large sync doesn't spike memory. Each file's old rows are replaced in one transaction.

In `serve` this runs in the background: the MCP handshake and searches never wait, and search output says the index is incomplete until the first sync finishes. **New (step 6-1):** for a new, empty index with more than `SCREE_FIRST_SYNC_MB` (default 50) of markdown, the writer skips the first sync and releases the lock; `scree reindex` indexes it on purpose.

## The watcher

After the sync, the writer watches the folder with fsnotify:

- **Event loop and worker.** File events are read on their own goroutine, which never waits for the embedding API. The loop debounces them into a set of dirty paths; a worker indexes them.
- **Debounce.** A path is indexed 1.5 s after its last event, since editors fire several events per save.
- **Pruning.** It never watches a folder the policy skips: hidden, gitignored, a nested repository, or excluded by `.screeignore`. So `node_modules/` costs no watches.
- **Re-adding.** Watches are re-added after a folder is renamed and after any ignore file changes.
- **Overflow.** If the kernel's event queue overflows anyway, the writer resyncs and rebuilds its watches.
- **Health.** Every 10 s it checks that the folder and the index file are still the ones it started with. If either was removed or replaced, `scree watch` exits with an error, so systemd or Docker restarts it; in `serve`, the writer stops, releases the lock, and search reports `[index not updating: …]` while it keeps serving what is indexed.

Files are opened without blocking, so a FIFO is never waited on, and a file is used only after checking that what was opened is the regular file the policy checked. On shutdown, `serve` and `watch` wait at most 5 s for the writer; a second signal exits at once.

## Search

A query is embedded (10 s timeout, 2 attempts), and the store returns the nearest chunks by cosine distance, over-fetching up to twice `k` (at most 100). Each hit's file is then re-checked against the current rules, with a stat, plus a full read for front matter when the file's mtime or size differs from what was indexed. Refused, deleted or opted-out files are dropped from the results, and the writer is told to re-check them. The first `k` survivors are returned. An empty index returns nothing without calling the API.

**Planned:** current line ranges for changed files and duplicate collapse (6-2), and hybrid search, which merges this vector search with an SQLite FTS5 keyword search by reciprocal rank fusion (6b). See the [roadmap](roadmap.md).

## Resource use

Measured idle memory is about 13–19 MB per process, against a target of 20–50 MB. That budget is why there is no local embedding model, no in-memory vector store and no Docker Desktop VM: vectors stay on disk, and a reader process holds no file watches. After a first sync, the writer hands its heap back to the OS.

## Cost

Indexing costs about $0.0025 per MB of markdown with the default model, `openai/text-embedding-3-small` (listed at about $0.02 per million tokens). It is paid once: after that only changed files are re-embedded, and `reindex -all` pays it again. A search embeds one short query, which costs a negligible fraction of a cent.

**New (step 6-1):** `doctor` prints an estimate for a folder, computed as bytes ÷ 4 tokens at $0.02 per million, about $0.005 per MB. That is deliberately an upper bound.
