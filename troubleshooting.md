---
title: Troubleshooting
---

Start with `scree run doctor -v` in the folder. Messages below are quoted from the code; the CLI prints each error as `scree: <message>`. `<file>` is the settings file, `~/.config/scree/env` under your passwd home. Under Claude Code, server errors appear in the MCP server status (`/mcp`) and its logs.

## `scree mcp` and `scree run` refusals

These come before `scree serve` or the command starts.

| Message | Fix |
| --- | --- |
| `<file> not found; create it with OPENROUTER_API_KEY, then: chmod 600 <file>` | Create the file as in [Getting started](getting-started.md#the-settings-file). It is always under the home directory in the password database, whatever `$HOME` says. |
| `<file> is a symlink or not a regular file; not reading it` | Replace the symlink with the real file. |
| `<file> is not owned by you` | The file belongs to another user. Recreate it as yourself. |
| `<file> is open to group or others (mode 644); fix with: chmod 600 <file>` | `chmod 600 <file>`. Any group or other bit is refused. |
| `<file> is larger than 64 KB` | The file holds settings only; trim it. |
| `<file> line N: NAME is not a scree setting` | Only `SCREE_*`, `OPENROUTER_API_KEY`, the proxy variables, `DOCKER_HOST` and `DOCKER_CONTEXT` are allowed. Remove the line. |
| `<file> line N: expected NAME=value`, `<file> line N: not a scree setting name` | Fix the line's syntax. |
| `cannot find the home directory of uid N in /etc/passwd or with /usr/bin/getent passwd` | Neither source knows your user, so scree won't guess from `$HOME`. Check `getent passwd $(id -u)`. |
| `mcp takes no arguments but -docker; put settings in ~/.config/scree/env (arguments could come from a repo's .mcp.json)` | Register it with no other arguments: `claude mcp add scree --scope project -- scree mcp`. |
| `mcp: CLAUDE_PROJECT_DIR is not set; scree mcp is for Claude Code (from a terminal, run scree run serve -notes <dir>)` | `scree mcp` only runs as a Claude Code server. From a terminal, use `scree run`. |
| `run: cannot run mcp` | `scree run` runs the other commands. |
| `scree mcp and scree run are not supported on Windows; …` | Set the variables in the environment and run `scree serve -notes <dir>` or another command directly. |

If Claude Code can't start the server at all, check that `scree` is on Claude Code's `PATH`, or register an [absolute path](using-with-claude-code.md#a-plain-scree-or-an-absolute-path). An old `.mcp.json` that names `scree-mcp` needs re-registering; see [Migrating](getting-started.md#from-the-scree-mcp-shell-wrappers).

## Choosing the folder

**`` this repo has not turned scree on: <dir>/.mcp.json must list scree mcp; to turn scree on, run `claude mcp add scree --scope project -- scree mcp` in <dir> and commit .mcp.json ``**
Under Claude Code, `serve` indexes only a folder whose own root `.mcp.json` names `scree mcp`. This is what a user-scope registration gets in every other repo. Run the command it suggests in a repo you want indexed, or remove the user-scope entry with `claude mcp remove scree --scope user`.

**`<dir>/.mcp.json is a symlink or not a regular file; not indexing`**, **`… is larger than 64 KB; not indexing`**, **`… is not valid JSON; not indexing`**
The opt-in check reads only a plain, small, valid `.mcp.json` at the repo root.

**`CLAUDE_PROJECT_DIR <dir> is not in the repo of the working directory <dir>; not indexing`**
`serve` indexes the git root of its working directory, and `CLAUDE_PROJECT_DIR` must be in the same repository. Something, often a repo's `.mcp.json` `env`, set `CLAUDE_PROJECT_DIR` to another place. Remove that setting.

**`no folder: run under Claude Code (CLAUDE_PROJECT_DIR) or pass -notes <dir>`**
`serve` was started outside Claude Code without `-notes`. From a terminal, pass `-notes <dir>`.

**`<dir> is your home directory, a folder above it, or a filesystem root; pass -notes <dir> to index it anyway`**
The folder was derived from the current directory and landed on `~` or above, often because of a dotfiles repo with its `.git` at `~`. Run from inside a real repo, or pass `-notes` on the command line. Under Claude Code the message ends `scree does not index it from Claude Code` instead: start the session in a repo.

**`serve: -notes, -db and SCREE_DB are for the command line; under Claude Code (CLAUDE_PROJECT_DIR is set) scree indexes the repo it runs in`**
Something passed `-notes` or `-db` to `serve`, or set `SCREE_DB`, inside a Claude Code session. Remove it; see [why](getting-started.md#why--notes-isnt-allowed-under-claude-code). If you see this from a terminal, unset `CLAUDE_PROJECT_DIR`.

<a id="untrusted-git"></a>
**`git work tree at <dir> is owned by another user (or is a symlink); not indexing it. To index a folder below it anyway, pass -notes <folder> -gitignore=false on the command line`**
A `.git` above the folder belongs to another user or is a symlink, so its ignore rules can't be trusted. If you trust the folder, index it from the CLI with `-notes <folder> -gitignore=false`; git's ignore rules are then not applied, so check `doctor -v`. A message starting `the git dir named by …` means the same for a linked worktree's git dir.

**`index folder <dir> is a symlink or not a folder; not using it`**
An index folder, or the `scree` or `indexes` folder above it, is a symlink. Replace it with a real folder.

## The index

**`index root mismatch: index <file> was built for <dir>, not <dir>; pass a different -db or delete it`**
`-db` or `SCREE_DB` points at an index built for another folder. Use the default location (no `-db`), another file, or delete that index.

**Model mismatch**

```
index was built with a different embedding model; run `scree reindex -all` (index: <old>, configured: <new>)
```

`SCREE_MODEL` or `SCREE_DIMENSIONS` changed. Run `scree run reindex -all` in the folder. `doctor` shows both.

**`index is in use by pid N (a Claude Code session or scree watch); close it, then run reindex -all`**
Close the sessions in that repo, or stop `scree watch`, then retry.

**`pid N keeps this index current; nothing to do`**
Not an error: another process is the writer and picks up edits itself.

**`index file was removed or replaced: <file>`**, **`notes folder <dir> was removed or moved`**
The index or the folder was deleted or swapped while running. `scree watch` exits so its supervisor restarts it; in a Claude Code session, restart the session.

**`N files failed and N were deferred; run reindex again`**
Some files hit API errors (failed) or were skipped while the API was paused (deferred). The log above it names each file. Retry once the API is reachable.

**`` keyword index: N rows for M chunks; run `scree reindex -all` ``** (doctor)
The keyword index is out of step with the chunks. Rebuild it. `doctor -v` also runs FTS5's integrity check.

**Old indexes left behind**
`scree indexes` lists every index with `[missing]` for one whose folder is gone; `scree indexes -prune` deletes those. It also reports `legacy index "<file>" (from the single-folder version); delete it, or pass -db "<file>" to keep using it`. During `-prune`, `kept <dir>: in use by pid N …` means a writer still holds that index; it can go later.

## Search notes

**`[index incomplete: first sync in progress, N files indexed so far]`**
The first index is still being built; results may miss files. Wait, or run `scree run reindex` in a terminal to watch progress.

**`[index incomplete: another scree process (pid N) is building it]`**
Another session or `scree watch` is building the index. Nothing to do.

**`[index not updating: …]`**
This session's writer stopped with the error shown; search still works on what is indexed. Fix the cause and restart the session.

**`[not indexed: this folder has X MB of markdown, over SCREE_FIRST_SYNC_MB=M; run "scree reindex" in it to index it anyway]`**
Check `doctor -v`, exclude build output or vendored docs with `.screeignore`, then `scree run reindex` to index it on purpose, or raise `SCREE_FIRST_SYNC_MB`.

**`bad line in .screeignore: …; nothing is indexed until it is fixed (scree doctor lists the lines)`**
Fix the lines `doctor` names by number. Indexing resumes once the file parses.

**`mode must be auto, hybrid, semantic or keyword, not "…"`**, **`format must be xml or json, not "…"`**
A typo in `-mode`, `mode` or `-format`.

**Search returns nothing for a file you expect**
Check `doctor -v` for the file's reason. An ignored, oversized or opted-out file never appears. For an exact name, try `-mode keyword`; for a name with no `_` or digit, `auto` runs only the vector search, so pass `-mode hybrid`.

**A hit says `stale="true"`**
The file changed and the chunk's text is no longer in it. The writer re-indexes the file within seconds; search again.

## The embeddings API

When an embedding call fails during a search, the error ends with `; mode="keyword" still works without the embedding API`.

**`embed: no API key (set OPENROUTER_API_KEY)`**
The key isn't in the environment. Add it to the settings file and use `scree run`, not bare `scree`.

**`embed: model <model> has no endpoint that meets zero data retention; choose another model or set SCREE_ZDR=0 (API: …)`**
OpenRouter has no zero-retention endpoint for that model. Pick another `SCREE_MODEL`, or accept retention with `SCREE_ZDR=0`. With ZDR off, the same error says the model has no endpoint that `is allowed by the account's data policy`: check your OpenRouter privacy settings.

**`embed: 401 Unauthorized: …`**
The key is wrong or revoked. Create a new one and update the settings file.

**`embed: 402 Payment Required: …`**
The OpenRouter account is out of credit. Add credit.

**`embed: embedding API unavailable: …`**
A timeout (`no response within 10s`), network error, 429 or 5xx. Indexing pauses (30 s, doubling to 5 min) and then resyncs on its own. With a proxy, set `HTTPS_PROXY` in the settings file.

**`SCREE_EMBED_URL is not OpenRouter; set SCREE_ZDR=0 if it rejects the provider field`** (doctor warning)
Another endpoint may reject OpenRouter's `provider` field. Set `SCREE_ZDR=0` for it.

## Settings values

| Message | Fix |
| --- | --- |
| `SCREE_MAX_FILE_BYTES="…": want a positive number of bytes` | Use a plain integer such as `2097152`. |
| `SCREE_FIRST_SYNC_MB="…": want a whole number of megabytes, 0 for no limit` | Use an integer such as `100`. |
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

| Message | Fix |
| --- | --- |
| `mcp -docker: refusing to mount <dir> (your home directory, a folder above it, or /)` | Start Claude Code in a repo, not in `~`. |
| `mcp -docker: CLAUDE_PROJECT_DIR <dir> is not in the repo of the working directory <dir>; refusing` | The Docker variant mounts one work tree, so `CLAUDE_PROJECT_DIR` must be in that same work tree. |
| `mcp -docker: cannot mount <dir>: the path contains ':'` | Move or rename the repo. |
| `mcp -docker: docker not found in /usr/local/bin, /usr/bin, /bin, /opt/homebrew/bin` | Install the engine in one of those folders; `PATH` is never searched. |
| `mcp -docker: SCREE_MOUNT_OPTS must be ro, ro,z or ro,Z`, `mcp -docker: SCREE_DOCKER must be docker or podman`, `mcp -docker: SCREE_DATA_VOLUME must be a volume name, not a path`, `mcp -docker: SCREE_IMAGE must be an image name` | Fix the value in the settings file. |

**`open /notes: permission denied`, or permission denied on the repo path**
On an SELinux host the mount needs `:z` (`docker run -v …:ro,z`, or `SCREE_MOUNT_OPTS=ro,z` for `scree mcp -docker`). Read the [relabel warning](docker.md#selinux-z-is-required-and-it-relabels) first.

**`scree mcp -docker` fails to find the image**
It runs with `--pull=never`. Build it (`docker build -t scree .`) or pull it yourself, and set `SCREE_IMAGE` if its name isn't `scree:latest`.

**The docker CLI can't reach the engine**
It gets no environment from Claude Code. Set `DOCKER_HOST` or `DOCKER_CONTEXT` in the settings file.

**Edits aren't picked up in a container on macOS**
Docker Desktop doesn't pass file events through bind mounts. Use the native binary.
