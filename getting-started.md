---
title: Getting started
---

## Requirements

- Go 1.26 to build from source (`go.mod` says `go 1.26.10`). No CGO and no C toolchain: the SQLite driver and vec1 run as WebAssembly inside the Go binary.
- An OpenRouter API key, from <https://openrouter.ai/settings/keys>. Any OpenAI-compatible embeddings endpoint also works through `SCREE_EMBED_URL` (see [Configuration](configuration.md)).
- Claude Code, for the MCP server. The CLI works without it.
- Linux, macOS or Windows. The shell wrappers need a POSIX `sh`; they are tested under bash, dash and busybox ash.

## Build or install

### From source

From a clone of the repo, on the `per-repo` branch:

```sh
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/scree ./cmd/scree
mkdir -p ~/.local/bin
install -m 755 bin/scree contrib/scree-mcp contrib/scree-run ~/.local/bin/
```

`scree` must sit in the same folder as the wrappers: they run the `scree` next to them, never one found through `PATH`. Claude Code must find `scree-mcp` on its `PATH`; if `~/.local/bin` isn't on it, add it in your shell profile.

### Release binaries and image (Planned)

No version has been tagged yet. The release workflow (`.github/workflows/release.yml`) runs on a `v*` tag and will publish:

- A **draft** GitHub release built by GoReleaser, with archives for Linux, macOS and Windows on amd64 and arm64, named `scree_<version>_<os>_<arch>.tar.gz` (`.zip` on Windows), plus `checksums.txt`. Each archive holds the binary, `LICENSE`, `README.md`, `docs/SPEC.md`, the four files in `contrib/` and `THIRD_PARTY_LICENSES/`. The workflow runs `go test` before building.
- A multi-arch (linux/amd64, linux/arm64) image at `ghcr.io/jrapakko/scree`, tagged `<version>` and `<major>.<minor>`. See [Docker](docker.md).

The release is created as a draft and published by hand, so a tag alone does not make it public.

## The settings file

Every wrapper, and the systemd unit, reads one file: `~/.config/scree/env` (or `$XDG_CONFIG_HOME/scree/env`). It holds the API key and any optional settings. Create it without the key touching your screen or shell history:

```sh
mkdir -p ~/.config/scree
printf 'OpenRouter API key: '; stty -echo; IFS= read -r key; stty echo; echo
( umask 077; printf "OPENROUTER_API_KEY='%s'\n" "$key" > ~/.config/scree/env )
chmod 600 ~/.config/scree/env
unset key
```

Rules:

- **Mode 600, owned by you.** The wrappers refuse to start if the file gives group or others any permission at all, or is owned by someone else.
- **One `NAME=value` per line.** Blank lines and `#` comments are skipped, and a leading `export ` is allowed. Wrap values in single or double quotes so a value with spaces stays one value; systemd's `EnvironmentFile=` reads the same quotes. A value can't contain the quote character that wraps it.
- **Read as data, never run as shell.** Nothing in a value is expanded: `$HOME`, `$(…)` and backquotes stay literal.
- **Only scree's variables:** `SCREE_*`, `OPENROUTER_API_KEY` and the proxy variables (`HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY` and their lowercase forms). Any other name stops the wrapper with the line number.
- It holds settings, not the folder. A leftover `SCREE_NOTES` line from the single-folder version is ignored, and `doctor` warns about it.

Don't `export` the key in a shell profile. Use `scree-run <command>` to run the CLI with the file's settings. The full list of settings is on the [Configuration](configuration.md) page.

## Check a repo before indexing it

From anywhere inside the repo:

```sh
cd ~/code/myrepo
scree-run doctor -v
```

`doctor` finds the folder (the git root above the current directory), reports how many files each ignore layer excludes, and with `-v` lists every ignored path with its reason. It also checks the index, the writer lock, the model, the key and, unless you pass `-offline`, makes one test call to the embeddings API. If a private file shows up as indexable, add a `.screeignore` or `.gitignore` entry before going further. See [Ignoring files](ignoring-files.md).

**New (step 6-1):** `doctor` also prints the total size of indexable markdown and an estimate of the tokens and cost to embed it in full.

## Opt the repo in

From the repo's root:

```sh
claude mcp add scree --scope project -- scree-mcp
```

This writes `.mcp.json` at the repo root with a server named `scree` whose command is `scree-mcp`, with no arguments, no key and no paths. Commit it; it works for everyone who installs the wrapper. Claude Code asks each person to approve a project-scoped server the first time.

A repo without this entry is never indexed. Don't register `scree-mcp` at user scope: it would index the repo of every project you open. If you registered scree at user scope with an earlier version, remove that with `claude mcp remove scree --scope user`.

## The first index

Start Claude Code in the repo. The wrapper starts `scree serve`, which indexes the git root above `CLAUDE_PROJECT_DIR` (the project folder Claude Code sets in the server's environment). The first sync runs in the background, so the session connects at once; until it finishes, search results end with an `[index incomplete: …]` line.

You can also build the index from the terminal first, which shows progress and failures:

```sh
cd ~/code/myrepo
scree-run reindex
```

**New (step 6-1):** a Claude Code session does not start the first index of a folder with more than `SCREE_FIRST_SYNC_MB` (default 50) of markdown. Search then says so, and `scree-run reindex` in the folder indexes it anyway. The CLI is never capped.

## A notes folder that isn't a repo

A folder outside any git repo works from the CLI with `-notes`:

```sh
scree-run reindex -notes ~/wiki
scree-run search -notes ~/wiki "how do we rotate the signing keys"
scree-run watch -notes ~/wiki      # keep it current; or use the systemd unit
```

The systemd user unit in `contrib/scree-watch.service` runs `scree watch -notes %h/notes` with the settings file as its `EnvironmentFile`. Edit `-notes` in `ExecStart` for another folder:

```sh
mkdir -p ~/.config/systemd/user
cp contrib/scree-watch.service ~/.config/systemd/user/
systemctl --user daemon-reload && systemctl --user enable --now scree-watch
journalctl --user -u scree-watch -f
```

To search such a folder from Claude Code, run the same `claude mcp add scree --scope project -- scree-mcp` inside it and start Claude Code there. It isn't a git repo, so the folder is the project folder itself.

### Why `-notes` isn't allowed under Claude Code

When `CLAUDE_PROJECT_DIR` is set, `scree serve` refuses `-notes`, `-db` and `SCREE_DB`, and the wrappers refuse all arguments. A repo's committed `.mcp.json` decides how the server is started, so if it could pass `-notes` or `-db`, a cloned repo could point your session, and your API key, at any folder on your disk, or at another folder's index. Under Claude Code, the folder and index follow from the repo alone. See [Security](security.md).

## Next

- [Using with Claude Code](using-with-claude-code.md)
- [CLI reference](cli.md)
- [Troubleshooting](troubleshooting.md)
