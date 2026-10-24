---
title: Configuration
---

scree reads its settings from environment variables. Keep them in the settings file, `~/.config/scree/env` (see [Getting started](getting-started.md#the-settings-file)); the wrappers load it, and it is their only source of configuration.

## Settings

| Variable | Default | Meaning |
| --- | --- | --- |
| `OPENROUTER_API_KEY` | none | API key sent as `Authorization: Bearer` to the embeddings endpoint. Needed for indexing and search. |
| `SCREE_MODEL` | `openai/text-embedding-3-small` | Embedding model. |
| `SCREE_DIMENSIONS` | `512` | Embedding size requested from the API; `0` uses the model's default. Must be an integer. |
| `SCREE_EMBED_URL` | `https://openrouter.ai/api/v1/embeddings` | Any OpenAI-compatible embeddings endpoint. |
| `SCREE_ZDR` | `1` | `1` asks OpenRouter to route only to endpoints with zero data retention and `data_collection: deny`. `0` sends no `provider` field, which endpoints other than OpenRouter may need. Accepts `1`/`0`, `true`/`false`, `t`/`f`. |
| `SCREE_MAX_FILE_BYTES` | `1048576` (1 MiB) | Files larger than this are skipped and refused by `read_doc`. A positive integer. |
| `SCREE_DB` | one index per folder, `$XDG_DATA_HOME/scree/indexes/<folder>-<hash>/index.db` | Index file, same as `-db`. Refused by `serve` under Claude Code. |
| `SCREE_HTTP_TOKEN` | none | Bearer token for `serve -http`. Required with `-allow-remote`; when set, enforced on loopback too. At least 16 characters. |
| `SCREE_FIRST_SYNC_MB` | `50` | **New (step 6-1).** A Claude Code session or `scree watch` won't start the first index of a folder with more markdown than this many MB; `0` means no limit. An integer. `scree reindex` is never capped. |

`<folder>` in the index path is the folder's name with characters outside `[A-Za-z0-9._-]` replaced by `_` and cut to 40 bytes; `<hash>` is the first 16 hex digits of the SHA-256 of the folder's resolved path. `$XDG_DATA_HOME` defaults to `~/.local/share`.

An invalid `SCREE_DIMENSIONS`, `SCREE_MAX_FILE_BYTES` or `SCREE_ZDR` stops the command with an error naming the variable.

### Changing the model

The index records the model and dimensions it was built with, as `<model>@<dimensions>`, and refuses to mix vectors: with another `SCREE_MODEL` or `SCREE_DIMENSIONS`, search, `watch`, `serve` and plain `reindex` fail with a model mismatch. Rebuild with `scree-run reindex -all`. `doctor` shows both models.

The default, `openai/text-embedding-3-small` at 512 dimensions, was chosen over `perplexity/pplx-embed-v1-0.6b` on a retrieval comparison recorded in the SPEC.

### Other OpenAI-compatible endpoints

Set `SCREE_EMBED_URL`, `SCREE_MODEL` and, since other servers may reject OpenRouter's `provider` field, `SCREE_ZDR=0`. The key still goes in `OPENROUTER_API_KEY`. `doctor` warns when ZDR is on and the URL's host isn't `openrouter.ai`. Testing local and LAN model servers is an [open item](roadmap.md#open-items).

### Removed

`SCREE_NOTES` was the folder setting of the single-folder version. It is no longer read; a run with it set logs a warning, and `doctor` prints one. The folder now comes from `-notes` or the git root.

## Variables scree reads but you don't set in the file

| Variable | Use |
| --- | --- |
| `CLAUDE_PROJECT_DIR` | Set by Claude Code in the server's environment. `serve` indexes the git root above it. The wrappers pass it through; the settings file can't set it. |
| `XDG_DATA_HOME` | Base of the index location (default `~/.local/share`). The image sets it to `/data`. |
| `XDG_CONFIG_HOME` | Location of the settings file (default `~/.config`) and of your git config and global excludes file. |
| `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY` | Go's standard proxy settings for the API call. They may go in the settings file. |

## What the wrappers accept and refuse

`scree-mcp`, `scree-run` and `scree-mcp-docker` share the same loader.

**Before reading the file,** each wrapper resets `PATH` to `/usr/local/bin:/usr/bin:/bin:/opt/homebrew/bin` and unsets every inherited `SCREE_*` variable, `OPENROUTER_API_KEY` and the six proxy variables. Nothing set in the environment that launched the wrapper, such as a repo's `.mcp.json`, reaches scree. The file's location comes from `SCREE_ENV_FILE` if set, else `${XDG_CONFIG_HOME:-$HOME/.config}/scree/env`.

**The file must:**

- exist, else `<wrapper>: <file> not found; create it with OPENROUTER_API_KEY, then: chmod 600 <file>`;
- give group and others no permissions at all, else `… is open to group or others (mode 644); fix with: chmod 600 <file>`;
- be owned by you, else `<wrapper>: <file> is not owned by you`.

**Each line** must be blank, a `#` comment, or `NAME=value` with an optional `export ` prefix. Accepted names: anything starting with `SCREE_`, `OPENROUTER_API_KEY`, and `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY` in either case. Anything else stops the wrapper with `line N: NAME is not a scree setting` or `line N: expected NAME=value`. Matching single or double quotes around a value are removed; nothing is expanded.

**Arguments:** `scree-mcp` and `scree-mcp-docker` refuse any argument (`takes no arguments; put settings in <file>`). `scree-run` passes its arguments to `scree`.

**The binary:** `scree-mcp` and `scree-run` run the `scree` in their own folder. `scree-mcp-docker` runs `docker` or `podman` only, found next to it or on the reset `PATH`.

### Docker wrapper settings

These are read only by `scree-mcp-docker` (see [Docker](docker.md)):

| Variable | Default | Accepted values |
| --- | --- | --- |
| `SCREE_DOCKER` | `docker` | `docker` or `podman` |
| `SCREE_IMAGE` | `scree:latest` | an image already present locally (never pulled) |
| `SCREE_DATA_VOLUME` | `scree-data` | a volume name: starts with a letter or digit, then letters, digits, `_`, `.`, `-`; never a path |
| `SCREE_MOUNT_OPTS` | `ro` | `ro`, `ro,z` or `ro,Z` |

The Docker wrapper passes only `OPENROUTER_API_KEY`, `SCREE_MODEL`, `SCREE_DIMENSIONS`, `SCREE_ZDR`, `SCREE_MAX_FILE_BYTES`, `SCREE_EMBED_URL` and `SCREE_HTTP_TOKEN` into the container, by name and only when set. Proxy variables and `SCREE_DB` are not passed. The step 6-1 plan adds `SCREE_FIRST_SYNC_MB` to this list.

### systemd

`contrib/scree-watch.service` loads the same file with `EnvironmentFile=%h/.config/scree/env`. systemd parses it itself, so the wrappers' name filter does not apply there.
