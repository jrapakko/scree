---
title: scree
nav_title: Overview
---

scree is a Go MCP server that gives Claude Code search over the markdown in a repository: READMEs, design docs, ADRs, agent instructions and notes, but not code.

It splits each markdown file by heading, embeds the pieces through OpenRouter with zero data retention requested, and stores chunks, vectors and a keyword index in a SQLite file on your machine, using SQLite's [vec1](https://sqlite.org/vec1) vector extension and FTS5. Claude Code talks to it over stdio. The markdown stays the source of truth: the index can be deleted and rebuilt at any time.

Source: [github.com/jrapakko/scree](https://github.com/jrapakko/scree). License: MIT. These pages describe the `main` branch.

## Why

The point is relevance and context, not speed. Long markdown files are unwieldy as context, and an agent that greps or reads whole files either misses the section that answers a question in different words, or pulls in thousands of lines to find it. scree returns the few sections that match the question, by meaning and, for identifiers such as `PURGE_OLD_RECORDS` or names from the repo's code such as `PageFetcher`, by exact words too. Each hit carries its path, heading trail and current line range, so Claude reads only those lines.

## Two-minute overview

1. **Build and install** one static binary, `scree` (no CGO), somewhere on Claude Code's `PATH`, for example `~/.local/bin`.
2. **Write the settings file** `~/.config/scree/env` with your OpenRouter API key, mode 600. `scree mcp` and `scree run` take configuration from it and nothing else.
3. **Check a repo:** `scree run doctor -v` in it lists every file that would be indexed or ignored, and why, and estimates the cost.
4. **Turn scree on for the repo:** `claude mcp add scree --scope project -- scree mcp` writes a `.mcp.json` with no key and no paths. Commit it. A repo whose own `.mcp.json` doesn't name `scree mcp` is never indexed.
5. **Use it.** Each Claude Code session in the repo starts `scree serve` through `scree mcp`. The first one builds the index in the background; later ones only read it and take over writing when the writer exits. Claude calls `search_docs`, gets `<hit>` elements with line ranges, and reads just those lines.

Approving scree in a repo's `.mcp.json` means trusting that repo, as with any project server; see [Security](security.md#trusting-a-repo).

Each folder gets its own index under `$XDG_DATA_HOME/scree/indexes/`. Idle memory is about 13–15 MB per process for a small index and about 20 MB for 1000 files. Indexing costs about $0.0025 per MB of markdown, once; only changed files are re-embedded, and a search embeds at most one short query.

## Pages

- [Getting started](getting-started.md): install, the settings file, turning a repo on, the first index, migrating.
- [Using with Claude Code](using-with-claude-code.md): the tools, search modes, their output, and several sessions in one repo.
- [CLI reference](cli.md): every command and flag, and exit codes.
- [Configuration](configuration.md): environment variables, the settings file, what `scree mcp` and `scree run` accept.
- [Ignoring files](ignoring-files.md): what gets indexed, `.gitignore`, `.screeignore`, front matter.
- [How it works](how-it-works.md): architecture, chunking, storage, search, the writer lock, the watcher, resource use and cost.
- [Security](security.md): the threat model, the trust model and the defences.
- [Docker](docker.md): the image, compose, and `scree mcp -docker`.
- [Troubleshooting](troubleshooting.md): error messages and fixes.
- [Roadmap](roadmap.md): what is done, open items and explorations.
