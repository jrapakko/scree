# scree

Local semantic search over the markdown in your repos and notes, served to Claude Code over MCP.

Scree splits markdown by heading, embeds the pieces through an API, and stores them in a SQLite file on disk, one index per folder. Claude Code asks for the few chunks that match a question instead of loading whole files. Each hit says which lines it came from, so Claude can read just those. The markdown stays the source of truth; an index can be deleted and rebuilt at any time.

See [docs/SPEC.md](docs/SPEC.md) for the design.

## Quick start

You need Go 1.26 and an OpenRouter API key from https://openrouter.ai/settings/keys. The commands run from the root of this repo unless a step says otherwise.

1. **Build** a static binary (no CGO):

   ```sh
   CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/scree ./cmd/scree
   ```

2. **Write the settings file.** This prompts for the key without echoing it, so it never lands in your shell history or on screen.

   ```sh
   mkdir -p ~/.config/scree
   printf 'OpenRouter API key: '; stty -echo; IFS= read -r key; stty echo; echo
   ( umask 077; printf "OPENROUTER_API_KEY='%s'\n" "$key" > ~/.config/scree/env )
   chmod 600 ~/.config/scree/env
   unset key
   ```

3. **Install** the binary and the wrappers in one folder:

   ```sh
   mkdir -p ~/.local/bin
   install -m 755 bin/scree contrib/scree-mcp contrib/scree-run ~/.local/bin/
   ```

   Claude Code must find `scree-mcp` on its `PATH`. If `~/.local/bin` isn't on it, add it in your shell profile.

4. **Check a repo before indexing it.** `scree-run` loads the settings file and runs a scree command. From inside the repo:

   ```sh
   cd ~/code/myrepo
   scree-run doctor -v
   ```

   `doctor -v` lists every file that will be ignored and estimates the cost of indexing the rest. Add a [`.screeignore`](#ignoring-files) or `.gitignore` entry before indexing if anything private shows up as indexable.

5. **Turn scree on for the repo.** From the repo's root:

   ```sh
   claude mcp add scree --scope project -- scree-mcp
   ```

   This writes `.mcp.json` with `"command": "scree-mcp"`, with no key and no paths, so commit it. Claude Code asks each person to approve a project server once. Every session in the repo then keeps the index current; the first session builds it in the background. A repo without this entry is never indexed. Don't register `scree-mcp` at user scope: every project you open would then be indexed.

6. **Optional: a notes folder outside any repo.** See [A notes folder](#a-notes-folder).

## Build

Requires Go 1.26. No CGO. Tagged releases also publish prebuilt binaries for Linux, macOS and Windows (amd64 and arm64) on the GitHub releases page, and a `ghcr.io/jrapakko/scree` image.

```sh
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/scree ./cmd/scree
```

## Configure

Settings come from environment variables. The [settings file](#settings-file) is the one place to keep them, including the API key. Don't `export` the key in a shell profile.

| Variable | Default |
| --- | --- |
| `SCREE_DB` | `$XDG_DATA_HOME/scree/indexes/<folder>-<hash>/index.db`, one per folder (or `-db`) |
| `SCREE_MODEL` | `openai/text-embedding-3-small` |
| `SCREE_DIMENSIONS` | `512`; `0` uses the model's default size |
| `SCREE_EMBED_URL` | `https://openrouter.ai/api/v1/embeddings` |
| `SCREE_MAX_FILE_BYTES` | `1048576`; larger files are skipped |
| `SCREE_FIRST_SYNC_MB` | `50`; a Claude Code session or `watch` won't start the first index of a folder with more markdown than this (`0` = no limit); `scree reindex` always does |
| `SCREE_ZDR` | `1`; set `0` for endpoints other than OpenRouter |
| `SCREE_HTTP_TOKEN` | none; required with `serve -allow-remote`, and enforced on loopback if set |

`$XDG_DATA_HOME` defaults to `~/.local/share`.

**Which folder.** `-notes <dir>` names it on the command line. Without it, commands use the git root of the current directory, or the current directory outside git. Under Claude Code, `serve` indexes the git root above `CLAUDE_PROJECT_DIR` (the repo Claude Code runs in) and refuses `-notes`, `-db` and `SCREE_DB`. A folder found automatically is refused if it is your home directory, a folder above it, or a filesystem root; pass `-notes` to index it anyway. Each index records its folder and refuses to be used for another one.

Changing the model or dimensions requires `scree reindex -all`. The index records which model built it and refuses to mix them.

### Privacy

Every embedding request asks OpenRouter to route only to endpoints with zero data retention and `data_collection: deny`. A model with no such endpoint fails with a clear error naming the model, rather than falling back to one that keeps data. `SCREE_ZDR=0` turns this off, which other OpenAI-compatible endpoints may need. Only chunk text, with its path and heading, is sent to the API; keyword search runs locally.

### Ignoring files

An ignored file is never read for indexing, never sent to the embedding API, never returned by search, and refused by `read_doc` and `reindex`. Four layers decide:

- **Built-in:** hidden files and folders, except the root `.claude/` (not `.claude/worktrees/`), `.github/`, `.gitlab/`, `.gitea/`, `.forgejo/`, `.cursor/`, `.codex/`, `.gemini/`, `.windsurf/`, `.circleci/` and `.buildkite/`; anything that isn't `.md` or `.markdown`; symlinks that lead outside the folder; files over `SCREE_MAX_FILE_BYTES`; non-regular files such as FIFOs; and names with control characters.
- **`.gitignore`:** in a git repo, everything git ignores (nested `.gitignore` files, `.git/info/exclude`, your global excludes file). Ignored folders such as `node_modules/` are never read, and nested repositories are skipped. A tracked file that matches a pattern is still ignored. `-gitignore=false` turns this layer off.
- **`.screeignore`** at the root of the indexed folder, in gitignore syntax.
- **Front matter:** `scree: ignore` in a file's front matter.

```
# .screeignore
journal/
**/private-*.md
/inbox/scratch.md
!journal/public.md
```

`include-hidden .name/` in `.screeignore` indexes another root hidden folder, and a pattern like `/.cursor/` drops one of the defaults. A `!` rule re-includes files inside an excluded folder, but never a file that a rule matched by name, so `*.private.md` stays ignored even under `!archive/2024/`; `doctor -v` lists what `!` re-included. If `.screeignore` can't be read, or is a symlink, nothing is indexed until it is fixed.

```
---
scree: ignore
---
```

Run `scree doctor -v` to see what each layer ignores before relying on a pattern. The full rules are in [docs/SPEC.md](docs/SPEC.md#ignoring-files).

## Use

```sh
scree doctor             # check config, index, and the API
scree doctor -v          # also list every ignored file, and check the keyword index
scree reindex            # index new and changed files
scree reindex docs/a.md  # re-index one file
scree reindex -all       # rebuild from scratch
scree search "how do I rotate the backup keys"
scree search -mode keyword DELETE_STALE_INTAKE   # exact words, no API call
scree search -format json "…"                    # one JSON object per hit
scree watch              # keep the index current (run as a service)
scree serve              # MCP over stdio
scree serve -http 127.0.0.1:7077   # MCP over HTTP, loopback only
scree indexes            # list per-folder indexes; -prune deletes ones whose folder is gone
```

Every command but `indexes` takes `-notes <dir>`, `-db <file>` and `-gitignore=false`. From a terminal, use `scree-run <command>` so the settings file is loaded, for example `scree-run search "…"`.

### Search modes

`scree search -mode` and the `search_docs` tool's `mode` take:

- **`auto`** (the default): `semantic`, unless the query has an identifier-like word, a word with a letter plus `_` or a digit, such as `DELETE_STALE_INTAKE`, `psycopg_pool`, `D1-Q4` or `v2`; then `hybrid`. `ID`, `API`, `2026` and `well-known` don't count.
- **`hybrid`**: a vector search and an SQLite FTS5 keyword search over chunk text, heading and path, merged by reciprocal rank fusion, so exact names rank first.
- **`semantic`**: vectors only.
- **`keyword`**: FTS5 only. It makes no API call, so it works without a key or while the API is down. A failed embedding call says so.

On a private set of 16 plain-language and 7 identifier queries over real notes, `auto` kept plain-question file MRR at 0.89 while raising identifier MRR from 0.46 to 0.80. Always-on `hybrid` raised identifiers as much but cost plain questions, which is why it is not the default.

The query is never passed to FTS5 as syntax: each word becomes a quoted term. Words keep `_` and `-`, so `psycopg_pool` is one term, and `pool` alone finds it only through the vector side.

### Results

Each hit is a `<hit>` element:

```
<hit n="1" path="docs/setup.md" lines="12-19" heading="Setup > Keys" score="0.912" match="both">
chunk text
</hit>
```

- `lines` is the range of source lines the chunk came from, so Claude can read exactly those (`read_doc(path, start, end)`). If the file changed since it was indexed, search finds the chunk in the current file, exactly or ignoring whitespace changes, and reports the current range. If the text is gone, the hit has `stale="true"` and no `lines`. Changed files are queued for re-indexing.
- `match` says which search found the hit: `semantic`, `keyword` or `both`. `score` is the cosine similarity and is missing for keyword-only hits.
- Identical chunks in several files, such as committed build copies, come back as one hit. The shown copy has the shortest path, and `also="docs/print/x.md:12-18, …"` lists up to three others, with `also_more="N"` for the rest. Excluding build folders with `.screeignore` is still better: it saves embedding cost.
- In `auto` mode the output ends with a line such as `[mode: auto → hybrid]`.

`-format json` prints one object per line, the stable format for scripts and evals: `{"n","mode","path","start","end","heading","score","match","stale","also"}`, where `mode` is the mode that ran, `start`, `end` and `score` are `null` when absent, and `also` is a list of `{"path","start","end"}`.

### Settings file

Claude Code, the wrapper scripts and the systemd unit all read the same file, `~/.config/scree/env`, so the API key lives in one place with mode 600. Step 2 of the [Quick start](#quick-start) creates it. It holds the key and optional settings, not the folder.

The wrappers always read `~/.config/scree/env`, with `~` taken from the password database (your login's home directory), not from `$HOME` or `$XDG_CONFIG_HOME`, so a repo's `.mcp.json` can't choose which settings file is loaded. They refuse to start if the file is missing, not owned by you, or readable by group or others; `chmod 600` fixes that.

Write one `NAME=value` per line, and wrap values in single quotes, as step 2 does, so a path with spaces stays one value. systemd reads the same quotes. The wrappers read the file as data and never run it as shell, so nothing in a value is expanded. They accept only scree's own variables (`SCREE_*`, `OPENROUTER_API_KEY`) and the proxy variables. Other variables from the Configure table can go in the same file. A value can't itself contain the quote character that wraps it.

### Claude Code

`contrib/scree-mcp` loads the settings file and runs `scree serve`. Claude Code stores only the wrapper's name, so the key stays out of `~/.claude.json`. Install it next to `scree` ([Quick start](#quick-start) step 3), then register it in each repo, from the repo's root:

```sh
claude mcp add scree --scope project -- scree-mcp
```

Commit the resulting `.mcp.json`. The wrapper passes Claude Code's `CLAUDE_PROJECT_DIR` through, and `serve` indexes the git root above it. Several sessions in one repo share one index: one writes it, the others read, and one takes over when the writer exits. A repo larger than `SCREE_FIRST_SYNC_MB` is not indexed until you run `scree-run reindex` in it once; search says so.

The tools are `search_docs(query, k, mode?)`, `read_doc(path, start?, end?)` and `reindex(path?)`. Each hit carries its line range, so Claude can read just those lines, and duplicates across copied folders are folded into one hit (`also=`). Still exclude build output with `.screeignore`.

The settings file is the wrapper's only source of configuration. A repo's committed `.mcp.json` can start `scree-mcp` with its own environment and arguments, so the wrapper resets `PATH`, ignores every inherited `SCREE_*`, `OPENROUTER_API_KEY` and proxy variable, finds the settings file from the password database, runs only the `scree` installed next to it, and refuses arguments. A repo can't redirect your key to another endpoint, swap in another binary or settings file, or point scree at another folder.

Stdio through the wrapper is the recommended setup. `serve -http` listens on loopback only. A non-loopback address needs `-allow-remote` and a `SCREE_HTTP_TOKEN` of at least 16 characters, and every request must then send `Authorization: Bearer <token>`. Keep the token in the settings file rather than on a command line:

```sh
printf 'SCREE_HTTP_TOKEN=%s\n' "$(openssl rand -hex 32)" >> ~/.config/scree/env
scree-run serve -notes ~/notes -http 0.0.0.0:7077 -allow-remote
```

Run this from a terminal, not from inside a Claude Code session, where `CLAUDE_PROJECT_DIR` is set and `serve` refuses `-notes`.

A client then needs the header. If you add it with `claude mcp add --transport http ... --header "Authorization: Bearer $SCREE_HTTP_TOKEN"`, expanding the variable at the shell keeps the token out of your shell history, but Claude Code still stores the expanded header in `~/.claude.json`.

### A notes folder

A folder outside any repo, such as `~/notes`, is used from the command line:

```sh
scree-run reindex -notes ~/notes
scree-run search -notes ~/notes "backup password"
```

The [systemd unit](#background-service-systemd) runs `scree watch -notes ~/notes` to keep it current. To search it from Claude Code, run `claude mcp add scree --scope project -- scree-mcp` in `~/notes` and start Claude Code there; it isn't a git repo, so the folder is `~/notes` itself. A Claude Code session elsewhere can't reach it, since under Claude Code `serve` only indexes the folder the session runs in.

### Background service (systemd)

```sh
# needs ~/.config/scree/env from "Settings file" above
mkdir -p ~/.local/bin ~/.config/systemd/user
cp bin/scree ~/.local/bin/
cp contrib/scree-watch.service ~/.config/systemd/user/
systemctl --user daemon-reload && systemctl --user enable --now scree-watch
journalctl --user -u scree-watch -f
```

The unit indexes `~/notes`. Edit `-notes` in `ExecStart` for another folder. It waits for the writer lock if a Claude Code session in that folder already holds it.

### Docker or Podman

The image is about 18 MB. It contains only the static binary and runs as an unprivileged user. A folder mounts read-only, and the indexes live in a volume at `/data`. On Linux, file-change events reach the container through the bind mount. On macOS, Docker Desktop doesn't pass them through, so `watch` misses edits there. Use the native binary or a systemd unit day to day.

**SELinux hosts (Fedora, RHEL, CentOS Stream) need `:z` on every bind mount.** Without it, the container can't read the mounted folder and scree fails with `open /notes: permission denied`. The examples below include `:z`, and the Claude Code wrapper needs `SCREE_MOUNT_OPTS=ro,z` in the settings file. `:z` **permanently relabels every file in the folder on the host** (to `container_file_t`), even when the mount is read-only. Mount a notes folder or a repo, never your home directory. On systems without SELinux, drop `:z`. If you'd rather not relabel anything, use the native binary instead.

```sh
docker build -t scree .            # or: podman build -t scree .
```

With `docker run`:

```sh
# background indexer
docker run -d --name scree-watch --restart unless-stopped \
  -e OPENROUTER_API_KEY \
  -v "$HOME/notes:/notes:ro,z" -v scree-data:/data \
  scree watch -notes /notes

# one-off commands against the same index
docker run --rm -e OPENROUTER_API_KEY -v "$HOME/notes:/notes:ro,z" -v scree-data:/data scree doctor -notes /notes
docker run --rm -e OPENROUTER_API_KEY -v "$HOME/notes:/notes:ro,z" -v scree-data:/data scree search -notes /notes "backup password"
```

`-e OPENROUTER_API_KEY` with no value passes the variable through from your shell, so the key never appears in the command line.

With compose, which reads `OPENROUTER_API_KEY` from your shell or from `.env` next to `compose.yaml`:

```sh
SCREE_NOTES_DIR=$HOME/notes docker compose up -d     # starts watch
docker compose logs -f watch
```

`SCREE_NOTES_DIR` is only compose's name for the host folder mounted at `/notes`. `compose.yaml` passes through `SCREE_MODEL`, `SCREE_DIMENSIONS`, `SCREE_MAX_FILE_BYTES`, `SCREE_FIRST_SYNC_MB`, `SCREE_ZDR` and `SCREE_EMBED_URL` too, and `SCREE_HTTP_TOKEN` to `serve`. The `serve` service sits behind the `mcp` profile, so `up` doesn't start it.

For Claude Code, the MCP server runs one container per session over stdio. `contrib/scree-mcp-docker` loads the settings file and runs `docker run -i --rm --pull=never` (a missing image is never pulled from a registry, since the container gets the key) with the project folder mounted read-only **at its own path** (so each repo gets its own index in the `scree-data` volume), the `scree-data` volume, and only the variables that are set, passed by name so the key never appears on the command line. It mounts the git root above `CLAUDE_PROJECT_DIR`, so `.gitignore` applies; your global git excludes file is not visible inside the container. A repo's committed `.mcp.json` names `scree-mcp`, so install the Docker wrapper under that name:

```sh
install -m 755 contrib/scree-mcp-docker ~/.local/bin/scree-mcp
```

`SCREE_DOCKER` (`docker` or `podman` only), `SCREE_IMAGE` (default `scree:latest`; a qualified `ghcr.io/jrapakko/scree:<version>` you pulled yourself also works) and `SCREE_DATA_VOLUME` (a volume name, not a path) in the settings file change the engine, image and volume. `SCREE_MOUNT_OPTS` (default `ro`) sets the mount options and accepts only `ro`, `ro,z` or `ro,Z`; on SELinux hosts set it to `ro,z`, as explained above. The wrapper never mounts your home directory (from the password database), a folder above it, or `/`, refuses paths containing `:`, and lifts to the git root only through a `.git` you own that is not a symlink, never in or above a sticky folder such as `/tmp`. Like `scree-mcp`, the Docker wrapper ignores inherited settings and takes no arguments.

Inside a container, `serve -http` has to bind a non-loopback address to be reachable through a published port, so it needs `-allow-remote` and `SCREE_HTTP_TOKEN`. Publish the port on the host's loopback only, for example `-p 127.0.0.1:7077:7077`.

The `watch` and `serve` containers share the `scree-data` volume. SQLite's WAL mode lets `serve` read while `watch` writes, as with native processes.

## How it works

- Each folder has its own index under `$XDG_DATA_HOME/scree/indexes/`. Whichever `scree serve` (one per Claude Code session) or `scree watch` holds the index's lock file is its one writer: it syncs on startup by content hash, then watches with fsnotify, waits 1.5 s after the last event for a path, and re-embeds only files whose hash changed. Other processes on the same index only read, and retry the lock every 5 s, so one takes over when the writer exits.
- The first sync runs in the background, so Claude Code connects at once; until it finishes, search results say the index is incomplete. SQLite's WAL mode lets readers search while the writer writes.
- Search results are `<hit>` elements with a path, line range, heading and match type. Chunks are at most 3500 characters, a hit's text is capped at 4000, and one response at 64 KB. Search combines vectors with an SQLite FTS5 keyword index when the query has an identifier, so exact names rank first. `read_doc` returns the file, or a range of its lines, in a `<doc>` element, capped at 256 KB. The server tells the client to treat both as data, not instructions.
- Every hit is re-checked against the current ignore rules before it is returned, so a newly ignored or deleted file drops out at once.
- Search waits at most 10 s for the API (2 attempts). Indexing waits up to 30 s per attempt (3 attempts). After an API outage, indexing pauses for 30 s, doubling up to 5 min, and then resyncs, so files changed during the outage are picked up without another edit.
- On shutdown, `serve` and `watch` wait at most 5 s for the writer, and a second Ctrl-C or SIGTERM exits at once.
- `scree watch` exits with an error if the index file or the folder disappears or is replaced, so systemd or Docker restarts it. In `serve`, the writer stops and searches report it. Watches are re-added when a folder is renamed or an ignore file changes. The index directory is created 0700 and its files are 0600.
- Vectors live in [vec1](https://sqlite.org/vec1), SQLite's vector extension, with a flat (exact) cosine index, and keywords in SQLite's FTS5, both compiled to WebAssembly by [ncruces/go-sqlite3](https://github.com/ncruces/go-sqlite3), which is what keeps the binary CGO-free.
- scree only trusts a `.git` you own that is not a symlink, never looks for one in or above a sticky folder such as `/tmp`, never follows a symlink for git's ignore files (`.gitignore`, `.git/info/exclude`, a `.git` file) or `.screeignore`, never reads a repository's `.git/config`, and never prints the content of any of them: messages name the file and line only. A `.git` owned by someone else is an error; to index a folder below it anyway, pass `-notes <folder> -gitignore=false`.
- One policy decides what may be read, and indexing, search, `read_doc` and `reindex` all go through it. Symlinks are followed only inside the indexed folder, and the rules are checked on their target too. Symlinked folders are not descended.

Idle memory is about 13–15 MB per process for a small index, and about 20 MB with an index of 1000 files (4000 chunks). Indexing costs about $0.0025 per MB of markdown with the default model; `doctor`'s estimate is an upper bound.

## Migrating from the single-folder version

The single-folder version read `SCREE_NOTES` and kept one index at `~/.local/share/scree/index.db`. Now each folder has its own index, and the folder comes from the repo or `-notes`.

1. Remove the `SCREE_NOTES=` line from `~/.config/scree/env`. It is ignored, and `doctor` warns while it is there.
2. Remove a user-scope registration, which would now index every project you open: `claude mcp remove scree --scope user`. Register scree per repo instead ([Quick start](#quick-start) step 5), and use the notes folder [from the command line](#a-notes-folder).
3. Rebuild the notes index in its new place with `scree-run reindex -notes ~/notes`, then delete `~/.local/share/scree/index.db`. Or keep it with `-db ~/.local/share/scree/index.db`, which adopts it for that folder. `scree indexes` points out the old file.
4. Update the systemd unit: `cp contrib/scree-watch.service ~/.config/systemd/user/ && systemctl --user daemon-reload && systemctl --user restart scree-watch`.
5. Docker: the image no longer sets `SCREE_NOTES` or `SCREE_DB`; pass `-notes /notes`. The old `/data/index.db` in the `scree-data` volume is unused; reindex.

An index from an earlier per-folder build is upgraded in place when it is opened: line ranges are filled in on the next sync without re-embedding unchanged files, and the duplicate hashes and keyword index are built from the stored chunks. An index built before the current chunk size limits needs `scree reindex -all` once.

## License

MIT; see [LICENSE](LICENSE). Release archives and the container image include the licenses of the bundled dependencies (`THIRD_PARTY_LICENSES/`, or `/usr/share/doc/scree` in the image).
