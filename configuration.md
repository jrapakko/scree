---
title: Configuration
---

scree reads its settings from environment variables. Keep them in the settings file, `~/.config/scree/env` (see [Getting started](getting-started.md#the-settings-file)); `scree mcp` and `scree run` load it, and it is their only source of configuration.

## Settings

| Variable | Default | Meaning |
| --- | --- | --- |
| `OPENROUTER_API_KEY` | none | API key sent as `Authorization: Bearer` to the embeddings endpoint. Needed for indexing and for every search mode but `keyword`. |
| `SCREE_MODEL` | `openai/text-embedding-3-small` | Embedding model. |
| `SCREE_DIMENSIONS` | `512` | Embedding size requested from the API; `0` uses the model's default. Must be an integer. |
| `SCREE_EMBED_URL` | `https://openrouter.ai/api/v1/embeddings` | Any OpenAI-compatible embeddings endpoint. |
| `SCREE_ZDR` | `1` | `1` asks OpenRouter to route only to endpoints with zero data retention and `data_collection: deny`. `0` sends no `provider` field, which endpoints other than OpenRouter may need. Accepts `1`/`0`, `true`/`false`, `t`/`f`. |
| `SCREE_MAX_FILE_BYTES` | `1048576` (1 MiB) | Files larger than this are skipped and refused by `read_doc`. A positive integer. |
| `SCREE_FIRST_SYNC_MB` | `50` | A Claude Code session or `scree watch` won't start the first index of a folder with more markdown than this many MB; `0` means no limit. A whole number. `scree reindex` is never capped. |
| `SCREE_DB` | one index per folder, `$XDG_DATA_HOME/scree/indexes/<folder>-<hash>/index.db` | Index file, same as `-db`. Refused by `serve` under Claude Code. |
| `SCREE_HTTP_TOKEN` | none | Bearer token for `serve -http`. Required with `-allow-remote`; when set, enforced on loopback too. At least 16 characters. |

`<folder>` in the index path is the folder's name with characters outside `[A-Za-z0-9._-]` replaced by `_`, cut to 40 bytes, with a leading `.` replaced by `_`; `<hash>` is the first 16 hex digits of the SHA-256 of the folder's resolved path. `$XDG_DATA_HOME` defaults to `~/.local/share`.

An invalid `SCREE_DIMENSIONS`, `SCREE_MAX_FILE_BYTES`, `SCREE_FIRST_SYNC_MB` or `SCREE_ZDR` stops the command with an error naming the variable.

### Changing the model

The index records the model and dimensions it was built with, as `<model>@<dimensions>`, and refuses to mix vectors: with another `SCREE_MODEL` or `SCREE_DIMENSIONS`, search, `watch`, `serve` and plain `reindex` fail with a model mismatch. Rebuild with `scree run reindex -all`. `doctor` shows both models.

The default, `openai/text-embedding-3-small` at 512 dimensions, was chosen over `perplexity/pplx-embed-v1-0.6b` on a retrieval comparison recorded in the SPEC.

### Other OpenAI-compatible endpoints

Set `SCREE_EMBED_URL`, `SCREE_MODEL` and, since other servers may reject OpenRouter's `provider` field, `SCREE_ZDR=0`. The key still goes in `OPENROUTER_API_KEY`. `doctor` warns when ZDR is on and the URL's host isn't `openrouter.ai`. Testing local and LAN model servers is an [exploration](roadmap.md#explorations).

### Removed

`SCREE_NOTES` was the folder setting of the single-folder version. It is no longer read; a run with it set logs a warning, and `doctor` prints one. The folder now comes from `-notes` or the git root. See [Migrating](getting-started.md#from-the-single-folder-version).

## Variables scree reads but you don't set in the file

| Variable | Use |
| --- | --- |
| `CLAUDE_PROJECT_DIR` | Set by Claude Code in the server's environment. Its presence tells `serve` it runs under Claude Code, and it must be in the same repo as the working directory. `scree mcp` and `scree run` pass it through; the settings file can't set it. |
| `HOME` | Set by `scree mcp` and `scree run` to your passwd home for the commands they run. |
| `XDG_DATA_HOME` | Base of the index location (default `~/.local/share`) for a command run directly. `scree mcp` and `scree run` don't pass it, so under them the index lives under `~/.local/share`. The image sets it to `/data`. |
| `XDG_CONFIG_HOME` | Location of your git config and global excludes file (default `~/.config`). Never the location of the settings file. |

## What `scree mcp` and `scree run` accept and refuse

They share one loader, built into the binary.

**The home directory** comes from the password database, never from `$HOME`: on Linux and other non-macOS Unix, `/etc/passwd`, then `/usr/bin/getent passwd <uid>` (for an LDAP or sssd login), run by absolute path in an empty environment; on macOS, the system user database. If neither knows you, they refuse to start rather than trust `$HOME`.

**The file** is `<home>/.config/scree/env`, and it must:

- exist, else `<file> not found; create it with OPENROUTER_API_KEY, then: chmod 600 <file>`;
- be a regular file, not a symlink, else `<file> is a symlink or not a regular file; not reading it`;
- be owned by you, else `<file> is not owned by you`;
- give group and others no permissions at all, else `<file> is open to group or others (mode 644); fix with: chmod 600 <file>`;
- be at most 64 KB.

**Each line** must be blank, a `#` comment, or `NAME=value` with an optional `export ` prefix. Accepted names: `SCREE_` followed by a capital letter, digit or `_` and then any letters, digits or `_`, `OPENROUTER_API_KEY`, `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY` in either case, and `DOCKER_HOST` and `DOCKER_CONTEXT`. Anything else is an error: `<file> line N: NAME is not a scree setting` or `<file> line N: expected NAME=value`. Matching single or double quotes around a value are removed; nothing is expanded. A later line overrides an earlier one. Errors name the line, never its value.

**The environment of what they run** is built from nothing: `HOME` (the passwd home), `PATH=/usr/local/bin:/usr/bin:/bin`, `CLAUDE_PROJECT_DIR` if set, and the file's settings. `DOCKER_HOST` and `DOCKER_CONTEXT` go only to the docker CLI under `scree mcp -docker`, never to scree. An inherited `SCREE_*`, key or proxy variable, `XDG_DATA_HOME`, `XDG_CONFIG_HOME`, `DOCKER_HOST`, or the Go runtime's `GODEBUG`, `GOMAXPROCS`, `GOGC` and `GOTRACEBACK` do not get through.

**Arguments:** `scree mcp` refuses every argument but `-docker`. `scree run` passes its arguments to the command it runs.

**The binary:** they re-execute their own binary (symlinks resolved), never one found on `PATH`. `scree mcp -docker` runs only `docker` or `podman`, found in `/usr/local/bin`, `/usr/bin`, `/bin` or `/opt/homebrew/bin`.

**Windows:** both refuse to run: `scree mcp and scree run are not supported on Windows; set the variables in the environment and run scree serve -notes <dir> or another command directly`.

### Docker settings

These are read only by `scree mcp -docker` (see [Docker](docker.md)):

| Variable | Default | Accepted values |
| --- | --- | --- |
| `SCREE_DOCKER` | `docker` | `docker` or `podman` |
| `SCREE_IMAGE` | `scree:latest` | an image already present locally (never pulled), such as `ghcr.io/jrapakko/scree:<version>` you pulled yourself |
| `SCREE_DATA_VOLUME` | `scree-data` | a volume name: starts with a letter or digit, then letters, digits, `_`, `.`, `-`; never a path |
| `SCREE_MOUNT_OPTS` | `ro` | `ro`, `ro,z` or `ro,Z` |
| `DOCKER_HOST`, `DOCKER_CONTEXT` | unset | passed to the docker CLI, if your engine needs them |

The container gets only `OPENROUTER_API_KEY`, `SCREE_MODEL`, `SCREE_DIMENSIONS`, `SCREE_ZDR`, `SCREE_FIRST_SYNC_MB`, `SCREE_MAX_FILE_BYTES`, `SCREE_EMBED_URL` and `SCREE_HTTP_TOKEN`, by name and only when set. Proxy variables and `SCREE_DB` are not passed to the container.

### systemd

`contrib/scree-watch.service` runs `scree run watch -notes %h/notes`, so it reads the same file through the same loader, with the same checks. It does not use systemd's `EnvironmentFile`.
