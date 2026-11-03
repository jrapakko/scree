---
title: Claude Desktop (one-click bundle)
nav_title: Claude Desktop
---

**Untested.** The bundle has not been tested in any MCP client on any platform: not in Claude Desktop, and not on macOS or Windows at all. It doesn't target Linux. Only the server command it runs was exercised, on Linux with a Linux build. Expect rough edges, and please [report](https://github.com/jrapakko/scree/issues) what you find.

Releases after 0.3.6 include `scree_<version>.mcpb`, an [MCPB](https://github.com/modelcontextprotocol/mcpb) bundle for Claude Desktop on macOS and Windows. It holds a macOS universal binary and a Windows amd64 binary (Windows on ARM runs it under emulation). For Claude Code, use `scree mcp` from a repo's `.mcp.json` instead (see [Getting started](getting-started.md#turn-scree-on-for-the-repo)); on Linux, use the native binary or the [image](docker.md).

## Installing

Download `scree_<version>.mcpb` from the [GitHub releases page](https://github.com/jrapakko/scree/releases) and open it in Claude Desktop. That installs it and asks for two settings:

- **Notes folder** (required): the one folder scree indexes and searches. Choosing it is the opt-in, so the `.mcp.json` check doesn't apply. Pick a notes folder or a repo: the server refuses your home folder, a folder above it, or a drive root, and doesn't start.
- **OpenRouter API key** (optional): without it, scree indexes and searches by keyword only. With it, search also works by meaning, and the notes and each query are sent to OpenRouter to be embedded. Spaces or a newline pasted around it are trimmed. If OpenRouter rejects the key, scree keeps working by keyword only and its search results say the key was rejected.

## What runs

Claude Desktop runs `server/scree serve -notes <folder>` from the bundle (`server/scree.exe` on Windows), with `OPENROUTER_API_KEY` set to the key or empty. An empty key is the same as none.

- **The settings file is ignored.** The bundle never goes through `scree mcp` and doesn't read [the settings file](configuration.md), so `SCREE_MODEL`, `SCREE_DIMENSIONS` and `SCREE_EMBED_URL` there don't apply.
- **One model per index.** A folder already indexed with another model is refused.
- **Large folders wait for you.** A folder with more than 50 MB of markdown isn't indexed on its own; search results say so, and Claude is told to ask you before running the `reindex` tool with no path, which indexes it anyway. See [`reindex`](using-with-claude-code.md#reindex).
- **The index is shared with the CLI.** It goes where the CLI puts it, so the CLI can search the same folder. Keep the two at the same version, since one may upgrade the index past what the other reads.

Without a key, `auto` search ends with `[mode: auto → keyword: no API key; add an OpenRouter API key in the extension's settings, set OPENROUTER_API_KEY, or start through scree mcp]` (on Windows without the `scree mcp` part).

## Unsigned binaries

The binaries aren't signed or notarized.

- **macOS:** Gatekeeper may refuse to run `server/scree` from a downloaded bundle until you allow it under System Settings → Privacy & Security. The unsigned binary also leaves the `DYLD_*` caveat on the [Security](security.md#trusting-a-repo) page.
- **Windows:** SmartScreen may warn in the same way.

## Checking the download

Each release also has `scree_<version>.mcpb.sha256`, in `sha256sum` format:

```sh
sha256sum -c scree_<version>.mcpb.sha256       # Linux
shasum -a 256 -c scree_<version>.mcpb.sha256   # macOS
```

The [MCP Registry](https://registry.modelcontextprotocol.io/) entry `io.github.jrapakko/scree` lists the bundle and records the same sha256. Whether a client checks it on install is untested.
