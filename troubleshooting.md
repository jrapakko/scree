---
title: Troubleshooting
---

Start with `scree-run doctor -v` in the folder. Messages below are quoted from the code; `<file>` is the settings file, usually `~/.config/scree/env`. Under Claude Code, server errors appear in the MCP server status (`/mcp`) and its logs.

## Wrapper refusals

These come from `scree-mcp`, `scree-run` or `scree-mcp-docker` before scree starts.

| Message | Fix |
| --- | --- |
| `<file> not found; create it with OPENROUTER_API_KEY, then: chmod 600 <file>` | Create the file as in [Getting started](getting-started.md#the-settings-file). |
| `<file> is open to group or others (mode 644); fix with: chmod 600 <file>` | `chmod 600 <file>`. Any group or other bit is refused. |
| `cannot read the mode of <file>` | `stat` failed; check the path and that it is a regular file. |
| `<file> is not owned by you` | The file belongs to another user. Recreate it as yourself. |
| `takes no arguments; put settings in <file>` | `scree-mcp` was given arguments, maybe from a `.mcp.json`. Register it with none: `claude mcp add scree --scope project -- scree-mcp`. |
| `<file> line N: NAME is not a scree setting` | Only `SCREE_*`, `OPENROUTER_API_KEY` and proxy variables are allowed. Remove the line. |
| `<file> line N: expected NAME=value` | Fix the line's syntax. |
| `CLAUDE_PROJECT_DIR is not set; run it from Claude Code` (Docker wrapper) | It only runs as a Claude Code server. Use `docker run` from a terminal. |
| `refusing to mount <dir> (your home directory, a folder above it, or /)` (Docker wrapper) | Start Claude Code in a repo, not in `~`. |
| `SCREE_MOUNT_OPTS must be ro, ro,z or ro,Z`, `SCREE_DOCKER must be docker or podman`, `SCREE_DATA_VOLUME must be a volume name, not a path` | Fix the value in the settings file. |

If Claude Code can't start the server at all, check that `scree-mcp` is on Claude Code's `PATH` and that `scree` is in the same folder.

## Choosing the folder

**`scree: no folder: run under Claude Code (CLAUDE_PROJECT_DIR) or pass -notes <dir>`**
`serve` was started outside Claude Code without `-notes`. From a terminal, pass `-notes <dir>`.

**`… is your home directory, a folder above it, or a filesystem root; pass -notes … to index it anyway`**
The folder was derived from the current directory or `CLAUDE_PROJECT_DIR` and landed on `~` or above, often because of a dotfiles repo with its `.git` at `~`. Run from inside a real repo, or pass `-notes` on the command line.

**`serve: -notes, -db and SCREE_DB are for the command line; under Claude Code (CLAUDE_PROJECT_DIR is set) scree indexes the repo it runs in`**
Something passed `-notes` or `-db` to `serve`, or set `SCREE_DB`, inside a Claude Code session. Remove it; see [why](getting-started.md#why--notes-isnt-allowed-under-claude-code). If you see this from a terminal, unset `CLAUDE_PROJECT_DIR`.

<a id="untrusted-git"></a>
**`git work tree at <dir> is owned by another user (or is a symlink); not indexing it. To index a folder below it anyway, pass -notes <folder> -gitignore=false on the command line`**
A `.git` above the folder belongs to another user or is a symlink, so its ignore rules can't be trusted. If you trust the folder, index it from the CLI with `-notes <folder> -gitignore=false`; git's ignore rules are then not applied, so check `doctor -v`. A message starting `git dir … named by …` means the same for a linked worktree's git dir.

## The index

**`index root mismatch: index <file> was built for <dir>, not <dir>; pass a different -db or delete it`**
`-db` or `SCREE_DB` points at an index built for another folder. Use the default location (no `-db`), another file, or delete that index.

**Model mismatch**

```
index was built with a different embedding model; run `scree reindex --all` (index: <old>, configured: <new>)
```

`SCREE_MODEL` or `SCREE_DIMENSIONS` changed. Run `scree-run reindex -all` in the folder. `doctor` shows both.

**`index is in use by pid N (a Claude Code session or scree watch); close it, then run reindex -all`**
Close the sessions in that repo, or stop `scree watch`, then retry.

**`pid N keeps this index current; nothing to do`**
Not an error: another process is the writer and picks up edits itself.

**`index file was removed or replaced`**
The index was deleted or swapped while running. `scree watch` exits so its supervisor restarts it; in a Claude Code session, restart the session.

**`N notes failed and N were deferred; run reindex again`**
Some files hit API errors (failed) or were skipped while the API was paused (deferred). The log above it names each file. Retry once the API is reachable.

## Search notes

**`[index incomplete: first sync in progress, N notes indexed so far]`**
The first index is still being built; results may miss files. Wait, or run `scree-run reindex` in a terminal to watch progress.

**`[index incomplete: another scree process (pid N) is building it]`**
Another session or `scree watch` is building the index. Nothing to do.

**`[index not updating: …]`**
This session's writer stopped with the error shown; search still works on what is indexed. Fix the cause and restart the session.

**`[not indexed: this folder has X MB of markdown, over SCREE_FIRST_SYNC_MB=M; run "scree reindex" in it to index it anyway]`** (**new, step 6-1**)
Check `doctor -v`, exclude build output or vendored docs with `.screeignore`, then `scree-run reindex` to index it on purpose, or raise `SCREE_FIRST_SYNC_MB`.

**Search returns nothing for a file you expect**
Check `doctor -v` for the file's reason. An ignored, oversized or opted-out file never appears.

## The embeddings API

**`embed: no API key (set OPENROUTER_API_KEY)`**
The key isn't in the environment. Add it to the settings file and use `scree-run`, not bare `scree`.

**`embed: model <model> has no endpoint that meets zero data retention; choose another model or set SCREE_ZDR=0 (API: …)`**
OpenRouter has no zero-retention endpoint for that model. Pick another `SCREE_MODEL`, or accept retention with `SCREE_ZDR=0`. With ZDR off, the same error says the model has no endpoint allowed by the account's data policy: check your OpenRouter privacy settings.

**`embed: 401 Unauthorized: …`**
The key is wrong or revoked. Create a new one and update the settings file.

**`embed: 402 Payment Required: …`**
The OpenRouter account is out of credit. Add credit.

**`embed: embedding API unavailable: …`**
A timeout, network error, 429 or 5xx. Indexing pauses (30 s, doubling to 5 min) and then resyncs on its own. With a proxy, set `HTTPS_PROXY` in the settings file.

**`SCREE_EMBED_URL is not OpenRouter; set SCREE_ZDR=0 if it rejects the provider field`** (doctor warning)
Another endpoint may reject OpenRouter's `provider` field. Set `SCREE_ZDR=0` for it.

## Settings values

| Message | Fix |
| --- | --- |
| `SCREE_MAX_FILE_BYTES="…": want a positive number of bytes` | Use a plain integer such as `2097152`. |
| `SCREE_ZDR="…": want 1 or 0` | Use `1` or `0`. |
| `SCREE_DIMENSIONS: …` | Use an integer; `0` means the model's default. |
| `SCREE_NOTES is no longer read; remove it from the settings file (the folder is now -notes or the git root)` | Delete the line. |

## HTTP mode

| Message | Fix |
| --- | --- |
| `refusing to listen on <addr>: not a loopback address; pass -allow-remote and set SCREE_HTTP_TOKEN` | Bind `127.0.0.1`, or pass both. |
| `refusing to listen on <addr>: -allow-remote needs SCREE_HTTP_TOKEN` | Add a token to the settings file. |
| `SCREE_HTTP_TOKEN is too short: use at least 16 characters` | `openssl rand -hex 32` makes one. |
| HTTP `401 unauthorized` | The client must send `Authorization: Bearer <token>`. |

## Docker and SELinux

**`open /notes: permission denied`, or permission denied on the repo path**
On an SELinux host the mount needs `:z` (`docker run -v …:ro,z`, or `SCREE_MOUNT_OPTS=ro,z` for the Docker wrapper). Read the [relabel warning](docker.md#selinux-z-is-required-and-it-relabels) first.

**The Docker wrapper fails to find the image**
It runs with `--pull=never`. Build it (`docker build -t scree .`) or pull it yourself.

**Edits aren't picked up in a container on macOS**
Docker Desktop doesn't pass file events through bind mounts. Use the native binary.
