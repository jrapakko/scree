---
title: Roadmap
---

Done on the `per-repo` branch: git-style ignores and the root hidden-folder allowlist, `.gitignore` support and watcher pruning, per-folder indexes chosen from `-notes` or `CLAUDE_PROJECT_DIR`, canonical on-disk names, front matter and search re-checks, the writer lock with background sync, and the security hardening of the wrappers and git-file handling.

## In progress: step 6-1

Marked **New** on these pages:

- Tools renamed to `search_docs` and `read_doc`; `read_doc` returns `<doc>` instead of `<note>`. `reindex` keeps its name.
- Server instructions name the project folder.
- Hits carry `lines="a-b"` (first and last non-blank source line) instead of `line="N"`; older index rows are backfilled without re-embedding.
- `read_doc(path, start?, end?)` reads a line range.
- `SCREE_FIRST_SYNC_MB` (default 50): a session won't start the first index of a larger folder.
- `doctor` prints the indexable size and an estimated token count and cost.

## Planned

### Step 6-2: current line ranges and duplicate collapse

- If a hit's file changed since it was indexed, search finds the chunk in the current file, first exactly, then ignoring whitespace differences, and reports the current line range. If the text is gone, the hit keeps its text, loses its range, and says `stale="true"`. Changed files are queued for the writer. Each changed file is read at most once per search.
- Identical chunks in several files, such as committed build copies, come back as one hit. The primary is the copy with the fewest path segments, then the shortest path. `also="path:a-b, …"` lists up to three others, and `also_more="N"` counts the rest. Excluding build folders with `.screeignore` is still better, since it saves embedding cost.

### Step 6b: hybrid search

- An SQLite FTS5 keyword index over chunk text, heading and path, kept in sync by triggers, using the `unicode61` tokenizer with `_` and `-` as token characters so identifiers such as `SCREE_FIRST_SYNC_MB` stay one token.
- Vector and keyword results merged by reciprocal rank fusion, so exact identifiers, file names and error strings rank first.
- `search_docs(query, k, mode?)` with `mode` = `hybrid` (default), `semantic` or `keyword`. Keyword mode makes no API call, so it works during an outage or without a key.
- Hits gain `match="semantic|keyword|both"`; `score` is present only when the vector side found the hit.
- User text is never passed to FTS as query syntax: each word becomes a quoted term.
- CLI: `scree search -mode …` and `-format json`, one JSON object per hit.
- Existing indexes are upgraded in place without re-embedding.

### Step 7: `scree indexes [-prune]`

Lists each index under `$XDG_DATA_HOME/scree/indexes/` with its folder, file count, size on disk, last sync, and `[missing]` or `[in use by pid N]`. It also points out a legacy single-folder `index.db`. `-prune` deletes indexes whose folder is gone and whose lock is free, without following symlinks.

### Step 8: final docs

README Quick start, a migration section for the single-folder version, and the SPEC's architecture and open items brought up to date.

## Open items

From the SPEC on the `per-repo` branch, plus the items its step 8 adds:

- **Local and LAN embedding models.** Test scree against an OpenAI-compatible server on the same machine (Ollama, llama.cpp) and on another machine on the LAN, through `SCREE_EMBED_URL` with `SCREE_ZDR=0`. Compare retrieval with the current default, and measure the model server's memory. A LAN server should only be reached over TLS or a VPN such as WireGuard, since requests carry file text in the clear.
- **Human view.** Plain files in an editor, or a small local search page over the same index.
- **MCP registry.** License, CI, release workflow and OCI label are done. Still to do: cut `v0.1.0`, build an MCPB bundle, write `server.json` for `io.github.jrapakko/scree`, and publish with `mcp-publisher`.
- **Base image pinning.** Pin the Dockerfile's `golang` and distroless images by digest once something keeps the digests current.
- **Wrapper portability.** `contrib/scree-*` are tested under bash, dash and busybox ash as `/bin/sh`, not yet on BSD or macOS `sh` or with `shellcheck`, including the git-root walk in `scree-mcp-docker`.
- **Tracked-but-ignored files.** Read `.git/index` so tracked files that match a `.gitignore` pattern are indexed, as git treats them. Today they are skipped.
- **`[include]` in git config.** `core.excludesFile` is read only from the standard config files.
- **Several folders per session.** Index `--add-dir` folders only if each has opted in, with a `root` attribute on hits.
- **Roots without `CLAUDE_PROJECT_DIR`.** For other MCP clients, consider roots through the newer protocol's input requests instead of `-notes`.

## Explorations

- **CI workflow YAML.** Only markdown is indexed today, including markdown in `.github/`. Whether to index CI definitions too.
- **Local and LAN embedding models.** As above: lower cost and no text leaving the network, at the price of memory and setup.
- **Keyword tokenizer** (after 6b). Whether `.` and `/` should join tokens for file names and paths, or a trigram side index for partial identifiers.
