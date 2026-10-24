# scree

Local semantic search over your markdown notes, served to Claude Code over MCP.

Scree splits notes by heading, embeds the pieces through an API, and stores them in a SQLite file on disk. Claude Code asks for the few chunks that match a question instead of loading whole files. The markdown stays the source of truth; the index can be deleted and rebuilt at any time.

See [docs/SPEC.md](docs/SPEC.md) for the design.

## Quick start

You need Go 1.26 and an OpenRouter API key from https://openrouter.ai/settings/keys. The commands run from the repo root.

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

3. **Install** the binary and the Claude Code wrapper:

   ```sh
   mkdir -p ~/.local/bin
   install -m 755 bin/scree contrib/scree-mcp contrib/scree-run ~/.local/bin/
   ```

4. **Check and build the index.** `scree-run` loads the settings file and runs a scree command. Change `~/notes` if your notes are elsewhere:

   ```sh
   scree-run doctor -v -notes ~/notes && scree-run reindex -notes ~/notes
   ```

   `doctor -v` lists every file that will be ignored. Add a [`.screeignore`](#ignoring-files) before indexing if anything private shows up as indexable.

5. **Connect Claude Code.** Only the wrapper's path is stored, so the key stays out of `~/.claude.json`:

   ```sh
   claude mcp add scree --scope user -- ~/.local/bin/scree-mcp -notes ~/notes
   ```

6. **Optional: keep the index current** with the [systemd unit](#background-service-systemd), which runs `scree watch` in the background.

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
| `SCREE_MAX_FILE_BYTES` | `1048576`; larger notes are skipped |
| `SCREE_ZDR` | `1`; set `0` for endpoints other than OpenRouter |
| `SCREE_HTTP_TOKEN` | none; required with `serve -allow-remote`, and enforced on loopback if set |

The folder comes from `-notes`. Without it, `serve` uses the project directory Claude Code passes in `CLAUDE_PROJECT_DIR`, and other commands use the git root of the current directory (or the current directory outside git). A folder found this way is refused if it is your home directory.

Changing the model or dimensions requires `scree reindex -all`. The index records which model built it and refuses to mix them.

### Privacy

Every embedding request asks OpenRouter to route only to endpoints with zero data retention and `data_collection: deny`. A model with no such endpoint fails with a clear error naming the model, rather than falling back to one that keeps data. `SCREE_ZDR=0` turns this off, which other OpenAI-compatible endpoints may need.

### Ignoring files

An ignored file is never read for indexing, never sent to the embedding API, and refused by `read_note` and `reindex`. Four layers decide:

- **Built-in:** hidden files and folders, except the root `.claude/` (not `.claude/worktrees/`), `.github/`, `.gitlab/`, `.gitea/`, `.forgejo/`, `.cursor/`, `.codex/`, `.gemini/`, `.windsurf/`, `.circleci/` and `.buildkite/`; anything that isn't `.md` or `.markdown`; symlinks that lead outside the folder; files over `SCREE_MAX_FILE_BYTES`; and names with control characters.
- **`.gitignore`:** in a git repo, everything git ignores (nested `.gitignore` files, `.git/info/exclude`, your global excludes file). Ignored folders such as `node_modules/` are never read. `-gitignore=false` turns this off.
- **`.screeignore`** at the root of the notes folder, in gitignore syntax.
- **Front matter:** `scree: ignore` in a note's front matter.

```
# .screeignore
journal/
**/private-*.md
/inbox/scratch.md
!journal/public.md
```

`include-hidden .name/` in `.screeignore` indexes another root hidden folder, and a pattern like `/.cursor/` drops one of the defaults. A `!` rule re-includes files inside an excluded folder, but never a file that a rule matched by name, so `*.private.md` stays ignored even under `!archive/2024/`; `doctor -v` lists what `!` re-included.

```
---
scree: ignore
---
```

Run `scree doctor -v` to see what each layer ignores before relying on a pattern. The full rules are in [docs/SPEC.md](docs/SPEC.md#ignoring-files).

## Use

```sh
scree doctor             # check config, index, and the API
scree doctor -v          # also list every ignored file and why
scree reindex            # index new and changed notes
scree search "how do I rotate the backup keys"
scree watch              # keep the index current (run as a service)
scree serve              # MCP over stdio
scree serve -http 127.0.0.1:7077   # MCP over HTTP, loopback only
scree reindex -all       # rebuild from scratch
```

After upgrading from an earlier version, run `scree reindex -all` once so existing notes are re-chunked with the current size limits.

### Settings file

Claude Code, the wrapper scripts and the systemd unit all read the same file, `~/.config/scree/env`, so the API key lives in one place with mode 600. Step 2 of the [Quick start](#quick-start) creates it. It holds the key and optional settings, not the folder. A leftover `SCREE_NOTES` line is ignored, and `doctor` warns about it. For the CLI, `scree-run <command>` loads it, for example `scree-run search "…"`.

Write one `NAME=value` per line, and wrap values in single quotes, as step 2 does, so a path with spaces stays one value. systemd reads the same quotes. The wrapper scripts read the file as data and never run it as shell, so nothing in a value is expanded. They accept only scree's own variables (`SCREE_*`, `OPENROUTER_API_KEY`) and the proxy variables, and they refuse to start if the file is readable by group or others; `chmod 600` fixes that. Other variables from the Configure table can go in the same file. A value can't itself contain the quote character that wraps it.

### Claude Code

`contrib/scree-mcp` loads the settings file and runs `scree serve`. Claude Code stores only the wrapper's path, so the key stays out of `~/.claude.json`:

```sh
mkdir -p ~/.local/bin
install -m 755 bin/scree contrib/scree-mcp ~/.local/bin/
claude mcp add scree --scope user -- ~/.local/bin/scree-mcp -notes ~/notes
```

The wrapper refuses to start if the settings file is missing, not owned by you, or readable by group or others. Install `scree` in the same folder as the wrapper: it runs that binary and nothing found through `PATH`.

The settings file is the wrapper's only source of configuration. A repo's committed `.mcp.json` can start `scree-mcp` with its own environment and arguments, so the wrapper resets `PATH`, ignores every inherited `SCREE_*`, `OPENROUTER_API_KEY` and proxy variable, and refuses arguments. A repo can't redirect your key to another endpoint, swap in another binary, or point scree at another folder.

Tools: `search_notes(query, k)`, `read_note(path)`, `reindex(path?)`.

Stdio through the wrapper is the recommended setup. `serve -http` listens on loopback only. A non-loopback address needs `-allow-remote` and a `SCREE_HTTP_TOKEN` of at least 16 characters, and every request must then send `Authorization: Bearer <token>`. Keep the token in the settings file rather than on a command line:

```sh
printf 'SCREE_HTTP_TOKEN=%s\n' "$(openssl rand -hex 32)" >> ~/.config/scree/env
scree-run serve -notes ~/notes -http 0.0.0.0:7077 -allow-remote
```

A client then needs the header. If you add it with `claude mcp add --transport http ... --header "Authorization: Bearer $SCREE_HTTP_TOKEN"`, expanding the variable at the shell keeps the token out of your shell history, but Claude Code still stores the expanded header in `~/.claude.json`.

### Background service (systemd)

```sh
# needs ~/.config/scree/env from "Settings file" above
mkdir -p ~/.local/bin ~/.config/systemd/user
cp bin/scree ~/.local/bin/
cp contrib/scree-watch.service ~/.config/systemd/user/
systemctl --user daemon-reload && systemctl --user enable --now scree-watch
journalctl --user -u scree-watch -f
```

The unit indexes `~/notes`. Edit `-notes` in `ExecStart` for another folder.

### Docker or Podman

The image is about 18 MB. It contains only the static binary and runs as an unprivileged user. Notes mount read-only at `/notes`, and the index lives in a volume at `/data`. On Linux, file-change events reach the container through the bind mount. On macOS, Docker Desktop doesn't pass them through, so `watch` misses edits there. Use the native binary or a systemd unit day to day.

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

`compose.yaml` passes through `SCREE_MODEL`, `SCREE_DIMENSIONS`, `SCREE_MAX_FILE_BYTES`, `SCREE_ZDR` and `SCREE_EMBED_URL` too, and `SCREE_HTTP_TOKEN` to `serve`. The `serve` service sits behind the `mcp` profile, so `up` doesn't start it.

For Claude Code, the MCP server runs one container per session over stdio. `contrib/scree-mcp-docker` loads the settings file and runs `docker run -i --rm --pull=never` (a missing image is never pulled from a registry, since the container gets the key) with the project folder mounted read-only **at its own path** (so each repo gets its own index in the `scree-data` volume), the `scree-data` volume, and only the variables that are set, passed by name so the key never appears on the command line. It mounts the git root above `CLAUDE_PROJECT_DIR`, so `.gitignore` applies; your global git excludes file is not visible inside the container. A repo's committed `.mcp.json` names `scree-mcp`, so install the Docker wrapper under that name:

```sh
install -m 755 contrib/scree-mcp-docker ~/.local/bin/scree-mcp
```

`SCREE_DOCKER` (`docker` or `podman` only), `SCREE_IMAGE` and `SCREE_DATA_VOLUME` (a volume name, not a path) in the settings file change the engine, image and volume. `SCREE_MOUNT_OPTS` (default `ro`) sets the mount options; on SELinux hosts set it to `ro,z`, as explained below. Like `scree-mcp`, the Docker wrapper ignores inherited settings and takes no arguments.

Inside a container, `serve -http` has to bind a non-loopback address to be reachable through a published port, so it needs `-allow-remote` and `SCREE_HTTP_TOKEN`. Publish the port on the host's loopback only, for example `-p 127.0.0.1:7077:7077`.

The `watch` and `serve` containers share the `scree-data` volume. SQLite's WAL mode lets `serve` read while `watch` writes, as with native processes.

## How it works

- Each folder has its own index. Whichever `scree serve` (one per Claude Code session) or `scree watch` holds the index's lock file is its one writer: it syncs on startup by content hash, then watches with fsnotify, waits 1.5 s after the last event for a path, and re-embeds only files whose hash changed. Other sessions in the same repo only read, and take over when the writer exits.
- The first sync runs in the background, so Claude Code connects at once; until it finishes, search results say the index is incomplete. SQLite's WAL mode lets readers search while the writer writes.
- Search waits at most 10 s for the API (2 attempts). Indexing waits up to 30 s per attempt (3 attempts). After an API outage, indexing pauses for 30 s, doubling up to 5 min, and then resyncs, so notes changed during the outage are picked up without another edit.
- `scree watch` exits with an error if the index file or the folder disappears or is replaced, so systemd or Docker restarts it. In `serve`, the writer stops and searches report it. Watches are re-added when a folder is renamed or an ignore file changes. The index directory is created 0700 and its files are 0600.
- Search results are `<hit>` elements of at most 3500 characters each, and one response is at most 64 KB. `read_note` returns the note in a `<note>` element. The server tells the client to treat both as data, not instructions.
- Vectors live in [vec1](https://sqlite.org/vec1), SQLite's vector extension, with a flat (exact) cosine index. It is compiled to WebAssembly by [ncruces/go-sqlite3](https://github.com/ncruces/go-sqlite3), which is what keeps the binary CGO-free.
- One policy decides what may be read, and indexing, search, `read_note` and `reindex` all go through it. Symlinks are followed only inside the notes folder, and the rules are checked on their target too. Symlinked folders are not descended.

Idle memory is about 13–15 MB per process.

## License

MIT; see [LICENSE](LICENSE). Release archives and the container image include the licenses of the bundled dependencies (`THIRD_PARTY_LICENSES/`, or `/usr/share/doc/scree` in the image).
