---
title: CLI reference
---

```
scree watch
scree serve [-http addr [-allow-remote]]
scree search [-k n] "query"
scree reindex [-all] [path]
scree doctor [-v] [-offline]
scree version
scree help
```

Run commands through `scree-run`, for example `scree-run search "…"`, so they get the API key and settings from the settings file. `scree-run` passes its arguments through; `scree-mcp` and `scree-mcp-docker` take none.

Flags use Go's `flag` package: `-flag` and `--flag` are the same, booleans take `-flag=false`, and **flags must come before positional arguments**. `scree search -k 3 "query"` works; `scree search "query" -k 3` is a usage error.

## Which folder

| Command | Folder |
| --- | --- |
| any, with `-notes <dir>` | `<dir>`, used as given |
| `serve` without `-notes` | the git root above `$CLAUDE_PROJECT_DIR`, or that folder itself outside git; an error if it is unset |
| other commands without `-notes` | the git root above the current directory, or the current directory outside git |

A folder found automatically (not given with `-notes`) is refused if it is your home directory, a folder above it, or a filesystem root. The git root is found only through a `.git` you own that is not a symlink, and the search stops at a sticky folder such as `/tmp` (see [Security](security.md)). `doctor` prints which rule chose the folder.

Under Claude Code (`CLAUDE_PROJECT_DIR` set), `serve` refuses `-notes`, `-db` and `SCREE_DB`. Without it (a terminal, a container, another MCP client), they work as for any command.

## Flags for every command

| Flag | Default | Meaning |
| --- | --- | --- |
| `-notes <dir>` | see above | Folder to index |
| `-db <file>` | `$SCREE_DB`, else `$XDG_DATA_HOME/scree/indexes/<folder>-<hash>/index.db` | Index file. The index records its folder and refuses another one. |
| `-gitignore` | `true` | Honor `.gitignore`, `.git/info/exclude` and your global excludes file. `-gitignore=false` turns that layer off. |

## Commands

### `scree watch`

Indexes new and changed files, then watches the folder and keeps the index current until stopped. If another process holds the writer lock, it waits, retrying every 5 seconds, and takes over when that process exits. Meant for a folder no Claude Code session keeps current, under systemd or Docker.

It exits non-zero if the folder or the index file is removed or replaced, so a supervisor restarts it.

### `scree serve`

The MCP server. Claude Code starts it through `scree-mcp`; you rarely run it by hand. Logs go to stderr; stdout carries the protocol.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-http <addr>` | none (stdio) | Serve streamable HTTP on `addr`, for example `127.0.0.1:7077` |
| `-allow-remote` | `false` | Allow a non-loopback `-http` address. Requires `SCREE_HTTP_TOKEN`. |

The writer runs in the background, so the MCP handshake and searches never wait for the lock or the first sync. HTTP rules are on the [Security](security.md#http-mode) page.

### `scree search [-k n] "query"`

Prints the top hits in the same `<hit>` format the MCP tool returns (see [Using with Claude Code](using-with-claude-code.md#search-output)), or `No matching notes.`. Takes exactly one query argument; quote it.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-k <n>` | `8` | Number of results. Not capped on the CLI; the MCP tool caps at 50. |

An empty index returns no hits without calling the API.

**Planned (step 6b):** `-mode hybrid|semantic|keyword` (keyword makes no API call) and `-format xml|json` (one JSON object per hit).

### `scree reindex [-all] [path]`

Without a path, syncs the whole folder: indexes new and changed files by content hash and removes rows for deleted or newly ignored ones. Prints `N notes: N indexed, N removed, N failed, N deferred`.

With a path, relative to the folder or absolute inside it, re-indexes that one file and prints `re-indexed "<path>"` or `"<path>" is already up to date`. An ignored path is an error.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-all` | `false` | Drop the index and rebuild it from scratch. Needed after changing `SCREE_MODEL` or `SCREE_DIMENSIONS`, and once after upgrading from an earlier version. |

If another process holds the writer lock:

- plain `reindex` and `reindex <path>` print `pid N keeps this index current; nothing to do` and exit 0 (an ignored path is still refused first);
- `reindex -all` fails: `index is in use by pid N (a Claude Code session or scree watch); close it, then run reindex -all`.

The CLI is never subject to the first-sync cap (**new in step 6-1**): it is how you index a large folder on purpose.

### `scree doctor [-v] [-offline]`

Checks the setup and prints one line per check, marked `ok`, `warn` or `FAIL`:

- the folder and which rule chose it;
- files indexable, and how many each ignore layer excludes (built-in, `.gitignore` and nested repositories, `.screeignore` with its `!` re-includes, front matter), plus bad ignore-file lines by number;
- the index file, its file and chunk counts, the last full sync;
- whether a process holds the writer lock, and its pid;
- the configured model, and whether the index was built with another;
- whether `OPENROUTER_API_KEY` is set, and whether zero data retention is requested;
- a test embedding call, unless `-offline`.

`doctor` never creates or writes the index.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-v` | `false` | Also list every ignored path with its reason, unreadable paths, and every file a `!` rule re-included with the rule's line |
| `-offline` | `false` | Skip the test call to the embeddings API |

**New (step 6-1):** a size line with the indexable megabytes, estimated tokens (bytes ÷ 4) and, for the default model, the cost to embed it all, plus a warning when the folder is over `SCREE_FIRST_SYNC_MB` and not yet indexed.

Sample of `doctor -v -offline` in a small repo with the key set, from the branch before step 6-1 (6-1 renames `notes:` to `files:` and "outside notes folder" to "outside the folder"):

```
ok   folder ~/code/demo (from git root of the current directory)
ok   notes: 3 indexable
ok   built-in: 2 hidden, 1 not markdown, 0 outside notes folder, 0 too large, 0 unsafe name
ok   .gitignore: 1 (0 nested repositories)
ok   .screeignore: 1 ignored, 0 re-included by "!"
ok   front matter: 1
  ignored (hidden): ".git/"
  ignored (hidden): ".obsidian/"
  ignored (not markdown): "main.go"
  ignored (.gitignore): "node_modules/"
  ignored (.screeignore): "journal/"
  ignored (front matter): "docs/draft.md"
ok   index ~/.local/share/scree/indexes/demo-b40ee07c8f619a6a/index.db: not created yet (the first serve, watch or reindex builds it)
ok   writer: none running (a Claude Code session or scree watch takes the role)
ok   model openai/text-embedding-3-small@512 (index empty)
ok   OPENROUTER_API_KEY set
ok   zero data retention requested (provider.zdr=true, data_collection=deny)
```

### `scree version`, `scree help`

`version` (also `-version`, `--version`) prints `scree <version>`; builds from source print `scree dev`. `help` (also `-h`, `-help`, `--help`) prints the usage text.

### `scree indexes [-prune]` (Planned, step 7)

Will list every index under `$XDG_DATA_HOME/scree/indexes/` with its folder, file count, size, last sync and whether it is missing or in use; `-prune` will delete indexes whose folder no longer exists and that no process holds. See the [roadmap](roadmap.md).

## Exit status

| Status | When |
| --- | --- |
| 0 | Success; `reindex` while another process writes; `serve` or `watch` stopped by a signal; `-h` on a command |
| 1 | Any error, printed as `scree: <message>`; `doctor` found a `FAIL`; `reindex` had failed or deferred files; a second Ctrl-C or SIGTERM during shutdown |
| 2 | No command, an unknown command, or a flag that doesn't parse |

On the first SIGINT or SIGTERM, `serve` and `watch` shut down cleanly, waiting at most 5 seconds for the writer. A second signal exits at once.
