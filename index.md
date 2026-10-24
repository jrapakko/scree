---
title: scree
nav_title: Overview
---

scree is a Go MCP server that gives Claude Code semantic search over the markdown in a repository: READMEs, design docs, ADRs, agent instructions and notes, but not code.

It splits each markdown file by heading, embeds the pieces through OpenRouter with zero data retention requested, and stores chunks and vectors in a SQLite file on your machine, using SQLite's [vec1](https://sqlite.org/vec1) vector extension. Claude Code talks to it over stdio. The markdown stays the source of truth: the index can be deleted and rebuilt at any time.

Source: [github.com/jrapakko/scree](https://github.com/jrapakko/scree). License: MIT.

## Why

The point is relevance and context, not speed. Long markdown files are unwieldy as context, and an agent that greps or reads whole files either misses the section that answers a question in different words, or pulls in thousands of lines to find it. scree returns the few sections that match the question by meaning, each with its path, heading trail and line range, so Claude reads only those lines.

## Two-minute overview

1. **Build and install** one static binary (`scree`, no CGO) and two shell wrappers (`scree-mcp`, `scree-run`) into the same folder, for example `~/.local/bin`.
2. **Write the settings file** `~/.config/scree/env` with your OpenRouter API key, mode 600. It is the only place scree's wrappers take configuration from.
3. **Check a repo:** `scree-run doctor -v` in it lists every file that would be indexed or ignored, and why.
4. **Opt the repo in:** `claude mcp add scree --scope project -- scree-mcp` writes a `.mcp.json` with no key and no paths. Commit it. A repo without this entry is never indexed.
5. **Use it.** Each Claude Code session in the repo starts `scree serve`. The first one builds the index in the background; later ones only read it and take over writing when the writer exits. Claude calls `search_docs`, gets `<hit>` elements with line ranges, and reads just those lines.

Each folder gets its own index under `$XDG_DATA_HOME/scree/indexes/`. Idle memory is about 13–19 MB per process. Indexing costs about $0.0025 per MB of markdown, once; only changed files are re-embedded, and a search embeds one short query.

## Status

This site documents the per-repo version on the `per-repo` branch. Labels used on these pages:

- **New (step 6-1):** part of the change being implemented now: the tool names `search_docs` and `read_doc`, the `<doc>` element, `lines="a-b"` on hits, ranged `read_doc`, the first-sync cap and the cost estimate in `doctor`. Builds of the branch from before 6-1 lands still expose `search_notes` and `read_note`.
- **Planned:** designed but not implemented: current line ranges and duplicate collapse (6-2), hybrid keyword and vector search (6b), and `scree indexes` (7). See the [roadmap](roadmap.md).

Everything else is implemented on the branch.

## Pages

- [Getting started](getting-started.md): install, the settings file, opting a repo in, the first index.
- [Using with Claude Code](using-with-claude-code.md): the tools, their output, and several sessions in one repo.
- [CLI reference](cli.md): every command and flag, and exit codes.
- [Configuration](configuration.md): environment variables, the settings file, what the wrappers accept.
- [Ignoring files](ignoring-files.md): what gets indexed, `.gitignore`, `.screeignore`, front matter.
- [How it works](how-it-works.md): architecture, chunking, storage, the writer lock, the watcher, resource use and cost.
- [Security](security.md): the threat model and the defences.
- [Docker](docker.md): the image, compose, and the Claude Code Docker wrapper.
- [Troubleshooting](troubleshooting.md): error messages and fixes.
- [Roadmap](roadmap.md): planned steps and open items.
