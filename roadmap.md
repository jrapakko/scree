---
title: Roadmap
---

## Done

Everything below is on `main`. `v0.1.0` was released on 2026-10-24; `v0.2.0` adds code-vocabulary routing.

- **Per-repo indexes.** One index per folder under `$XDG_DATA_HOME/scree/indexes/`, chosen from `-notes`, or under Claude Code from the git root of the session's working directory, with `CLAUDE_PROJECT_DIR` as a cross-check. Each index records its folder and model and refuses another.
- **Enforced opt-in.** Under Claude Code, `serve` indexes only a repo whose own committed `.mcp.json` names `scree mcp`.
- **Launchers in the binary.** `scree mcp`, `scree run <command>` and `scree mcp -docker` replace the old shell wrappers. The settings file always comes from the passwd home, is read as data, and is the only configuration they pass on.
- **Ignoring files.** Built-in rules with a root hidden-folder allowlist for forge, CI and agent folders, git's ignore rules (nested `.gitignore`, `.git/info/exclude`, global excludes), `.screeignore` with `!` re-includes and `include-hidden`, and `scree: ignore` front matter. Watches are pruned to match.
- **Safe reads.** Canonical on-disk names with NFC and case folding, symlinks only inside the folder, FIFO-safe opens, trust only your own `.git`, no symlinked git files, nothing echoed, fail closed on a bad `.screeignore`, the home guard.
- **One writer per index.** A `writer.lock` per index, background first sync in `serve`, takeover by readers, a bounded shutdown, and resync after API outages.
- **Tools.** `search_docs(query, k, mode?)`, `read_doc(path, start?, end?)` returning `<doc>`, and `reindex(path?)`, with server instructions that name the project folder and mark file text as data.
- **Search modes.** An FTS5 keyword index beside the vectors; `hybrid` merges both by reciprocal rank fusion, `keyword` makes no API call, and `auto`, the default, runs `hybrid` for identifier-like queries and `semantic` otherwise. `scree search -mode` and `-format json`.
- **Code-vocabulary routing.** `auto` also runs `hybrid` when a query word is a name from the repo's code: dependency names from manifests and mixed-case names from imports and declarations in Python, JavaScript, TypeScript and Go, collected locally with a stoplist and never embedded or sent. `doctor` reports the vocabulary's size and how many names the notes also contain. See [the code vocabulary](using-with-claude-code.md#the-code-vocabulary).
- **Line ranges that stay current.** Hits carry `lines="a-b"`; for a changed file, search finds the chunk's current range, or marks the hit `stale="true"`.
- **Duplicate folding.** Identical chunks in several files come back as one hit with `also` and `also_more`.
- **Cost controls.** `SCREE_FIRST_SYNC_MB` (default 50) caps the first index a session or `watch` starts on its own; `doctor` prints the indexable size and an upper-bound cost.
- **`scree indexes [-prune]`.** Lists every index with its folder, size, last sync, model and writer, points out a single-folder leftover, and deletes indexes whose folder is gone.
- **Migration.** Indexes from earlier builds are upgraded in place; see [Migrating](getting-started.md#migrating).
- **Packaging.** MIT license, CI, a GoReleaser and GHCR release workflow, and an 18 MB distroless image.

## Open items

- **Human view.** Decide between plain files in an editor and a small local search page over the same index.
- **MCP registry.** The license, CI, release workflow, OCI label and the first tagged release (`v0.1.0`) are done. Still to do: build an MCPB bundle, write `server.json` for `io.github.jrapakko/scree`, and publish with `mcp-publisher`.
- **Base image pinning.** The Dockerfile's `golang` and distroless images are referenced by tag. Pin them by digest once something like Dependabot keeps the digests current.
- **Docker and worktrees.** `scree mcp -docker` mounts only the work tree, so a worktree whose git dir lives in the main repository outside the mount can't be read in the container, and it requires `CLAUDE_PROJECT_DIR` to lead to the same work tree, not just the same repository as native `serve` allows.
- **Docker on SELinux.** The only way to make the read-only mount readable is `SCREE_MOUNT_OPTS=ro,z` (or `ro,Z`), which permanently relabels every file in the folder on the host. A label-free option, such as `--security-opt label=disable` for that one container, is not offered yet. The container also gets no proxy variables; only the docker CLI does.
- **Launcher portability.** On Linux and other non-macOS Unix, the home directory comes from `/etc/passwd`, then `/usr/bin/getent`; a user neither finds fails closed. On macOS it comes from the system user database, and a build with the `osusergo` tag, which would read `$HOME`, refuses to compile. Not yet tested on a real macOS host with directory-service users, or on BSD. macOS release binaries are not signed with the hardened runtime, so `DYLD_INSERT_LIBRARIES` from a repo's `.mcp.json` still reaches the first process there; signing would close that. Windows is refused.
- **Tracked-but-ignored files.** Read `.git/index` so that tracked files matching a `.gitignore` pattern are indexed, as git considers them. Today they are skipped.
- **`[include]` in git config.** `core.excludesFile` is read only from the standard config files.
- **Several folders per session.** Index `--add-dir` folders only if each has opted in (for example with its own `.mcp.json` entry), with a `root` attribute on hits.
- **Roots without `CLAUDE_PROJECT_DIR`.** For other MCP clients, consider roots through the newer protocol's input requests instead of `-notes`.

Resolved: the embedding model (`openai/text-embedding-3-small` at 512 dimensions, which beat `perplexity/pplx-embed-v1-0.6b` in a retrieval comparison), a CGO-free SQLite driver with a vector extension (vec1 instead of `sqlite-vec`), `auto` as the default search mode, and identifier routing for names with no `_` or digit: a vocabulary collected from code, measured on two repos with code and docs, gave a modest gain on questions naming code and sent no plain question to `hybrid`. Rare-term frequency was considered and dropped, since library names are frequent in a repo's own docs.

## Explorations

Ideas to try before committing to them.

- **Language-server vocabulary.** `workspace/symbol` from a language server as a fuller code vocabulary, at the cost of a process per language. Check whether Claude Code's own LSP support makes it unnecessary.
- **Keyword tokenizer.** Whether `.` and `/` should join tokens, for file names and paths, or a trigram side index for partial identifiers, measured on a set of identifier queries.
- **CI workflow YAML.** Index workflow files in the allowlisted forge and CI folders, one chunk per job, with a heading trail like `ci.yml > jobs.test`. Questions: which files count (GitHub, Gitea and Forgejo `workflows/`, `.gitlab-ci.yml` and its includes, `.circleci/config.yml`, `.buildkite/pipeline.yml`); a YAML parser or a line-based split, given that chunk text must be the original lines with their line numbers; resistance to hostile YAML such as alias expansion and deep nesting; GitLab's shape, where top-level keys are jobs except reserved ones; and whether retrieval for CI questions actually improves.
- **Local and LAN embedding models.** Test scree against an OpenAI-compatible server on the same machine (Ollama, llama.cpp) and on another machine on the LAN, through `SCREE_EMBED_URL` with `SCREE_ZDR=0`. Compare retrieval with the current default, and measure the model server's memory. A LAN server should only be reached over TLS or a VPN such as WireGuard, since embedding requests carry file text in the clear.
