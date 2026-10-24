---
title: Getting started
---

## Requirements

- Go 1.26 to build from source (`go.mod` says `go 1.26.10`). No CGO and no C toolchain: the SQLite driver, vec1 and FTS5 run as WebAssembly inside the Go binary.
- An OpenRouter API key, from <https://openrouter.ai/settings/keys>. Any OpenAI-compatible embeddings endpoint also works through `SCREE_EMBED_URL` (see [Configuration](configuration.md)).
- Claude Code, for the MCP server. The CLI works without it.
- Linux or macOS for `scree mcp` and `scree run`. The other commands also run on Windows, but `scree mcp` and `scree run` refuse to run there; see [Windows](#windows).

## Build or install

### From source

From a clone of the repo:

```sh
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/scree ./cmd/scree
mkdir -p ~/.local/bin
install -m 755 bin/scree ~/.local/bin/
```

That one binary is all there is: `scree mcp` and `scree run` are subcommands, not separate programs. Claude Code must find `scree` on its `PATH`; if `~/.local/bin` isn't on it, add it in your shell profile, or register an [absolute path](using-with-claude-code.md#a-plain-scree-or-an-absolute-path).

### Release binaries and image

The release workflow (`.github/workflows/release.yml`) runs on a `v*` tag and publishes:

- A **draft** GitHub release built by GoReleaser, with archives for Linux, macOS and Windows on amd64 and arm64, named `scree_<version>_<os>_<arch>.tar.gz` (`.zip` on Windows), plus `checksums.txt`. Each archive holds the binary, `LICENSE`, `README.md`, `docs/SPEC.md`, `contrib/scree-watch.service` and `THIRD_PARTY_LICENSES/`. The workflow runs `go test` before building.
- A multi-arch (linux/amd64, linux/arm64) image at `ghcr.io/jrapakko/scree`, tagged `<version>` and `<major>.<minor>`. See [Docker](docker.md).

The release is created as a draft and published by hand, so a tag alone does not make the GitHub release public; the image is pushed when the tag is. Releases are listed on the [GitHub releases page](https://github.com/jrapakko/scree/releases); the latest is `v0.2.0`.

## The settings file

`scree mcp`, `scree run` and the systemd unit all read one file: `~/.config/scree/env`. It holds the API key and any optional settings. Create it without the key touching your screen or shell history:

```sh
mkdir -p ~/.config/scree
printf 'OpenRouter API key: '; stty -echo; IFS= read -r key; stty echo; echo
( umask 077; printf "OPENROUTER_API_KEY='%s'\n" "$key" > ~/.config/scree/env )
chmod 600 ~/.config/scree/env
unset key
```

Rules:

- **Always under your passwd home.** `~` is the home directory in the password database (your login's), not `$HOME` or `$XDG_CONFIG_HOME`, so nothing in a launching environment can move it.
- **A regular file, mode 600, owned by you.** `scree mcp` and `scree run` refuse a file that is missing, is a symlink, is owned by someone else, gives group or others any permission at all, or is larger than 64 KB.
- **One `NAME=value` per line.** Blank lines and `#` comments are skipped, and a leading `export ` is allowed. Wrap values in single or double quotes so a value with spaces stays one value. A value can't contain the quote character that wraps it.
- **Read as data, never run as shell.** Nothing in a value is expanded: `$HOME`, `$(…)` and backquotes stay literal.
- **Only scree's variables:** `SCREE_*`, `OPENROUTER_API_KEY`, the proxy variables (`HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY` and their lowercase forms), and `DOCKER_HOST` and `DOCKER_CONTEXT`, which only `scree mcp -docker` uses. Any other name is an error naming the line.
- It holds settings, not the folder. A leftover `SCREE_NOTES` line from the single-folder version is ignored, and `doctor` warns about it.

Don't `export` the key in a shell profile. Use `scree run <command>` to run the CLI with the file's settings. The full list of settings is on the [Configuration](configuration.md) page.

## Check a repo before indexing it

From anywhere inside the repo:

```sh
cd ~/code/myrepo
scree run doctor -v
```

`doctor` finds the folder (the git root above the current directory), reports how many files each ignore layer excludes, and with `-v` lists every ignored path with its reason. It prints the total size of indexable markdown with an estimate of the tokens and cost to embed it in full, and checks the index, the keyword index, the writer lock, the model, the key and, unless you pass `-offline`, makes one test call to the embeddings API. If a private file shows up as indexable, add a `.screeignore` or `.gitignore` entry before going further. See [Ignoring files](ignoring-files.md).

## Turn scree on for the repo

From the repo's root:

```sh
claude mcp add scree --scope project -- scree mcp
```

This writes `.mcp.json` at the repo root with a server named `scree` whose command is `scree` and whose arguments are `["mcp"]`, with no key and no paths. Commit it; it works for everyone who has `scree` on their `PATH`. Claude Code asks each person to approve a project-scoped server the first time.

**Approving it means trusting the repo.** Claude Code applies the `env` of a repo's `.mcp.json`, including `PATH`, before starting the server, so an approved repo decides which program `scree` is, and that program gets your settings file. Approve scree only in repos whose `.mcp.json` you trust. See [Security](security.md#trusting-a-repo), and [absolute paths](using-with-claude-code.md#a-plain-scree-or-an-absolute-path) to take that choice back for yourself.

Opt-in is enforced: under Claude Code, `serve` refuses a folder whose own root `.mcp.json` names no `scree mcp` server, so a repo without this entry is never indexed even if scree is registered somewhere else. Don't register scree at user scope anyway: it would start in every project you open and refuse each one.

## The first index

Start Claude Code in the repo. `scree mcp` loads the settings file and runs `scree serve`, which indexes the git root of the working directory Claude Code starts it in (the project, or the active worktree). The first sync runs in the background, so the session connects at once; until it finishes, search results end with an `[index incomplete: …]` line.

You can also build the index from the terminal first, which shows progress and failures:

```sh
cd ~/code/myrepo
scree run reindex
```

A Claude Code session (or `scree watch`) does not start the first index of a folder with more than `SCREE_FIRST_SYNC_MB` (default 50) of markdown. Search then says so, and `scree run reindex` in the folder indexes it anyway. The CLI `reindex` is never capped.

## A notes folder that isn't a repo

A folder outside any git repo works from the CLI with `-notes`:

```sh
scree run reindex -notes ~/wiki
scree run search -notes ~/wiki "how do we rotate the signing keys"
scree run watch -notes ~/wiki      # keep it current; or use the systemd unit
```

The systemd user unit in `contrib/scree-watch.service` runs `scree run watch -notes %h/notes`, so it reads the settings file the same way `scree mcp` does. Edit `-notes` in `ExecStart` for another folder:

```sh
mkdir -p ~/.config/systemd/user
cp contrib/scree-watch.service ~/.config/systemd/user/
systemctl --user daemon-reload && systemctl --user enable --now scree-watch
journalctl --user -u scree-watch -f
```

The unit expects `scree` at `~/.local/bin/scree`. It waits for the writer lock if a Claude Code session in that folder already holds it.

To search such a folder from Claude Code, run the same `claude mcp add scree --scope project -- scree mcp` inside it and start Claude Code there. It isn't a git repo, so the folder is the working directory itself. A session elsewhere can't reach it: under Claude Code, `serve` indexes only the folder the session runs in.

### Why `-notes` isn't allowed under Claude Code

When `CLAUDE_PROJECT_DIR` is set, `scree serve` refuses `-notes`, `-db` and `SCREE_DB`, and `scree mcp` refuses every argument but `-docker`. A repo's committed `.mcp.json` decides how the server is started, so if it could pass `-notes` or `-db`, a cloned repo could point your session, and your API key, at any folder on your disk, or at another folder's index. Under Claude Code, the folder and index follow from the working directory alone. See [Security](security.md).

## Windows

`scree mcp` and `scree run` need `exec` and Unix file ownership, which Windows lacks, so they refuse to run there. Set the variables in the environment yourself and run `scree serve -notes <dir>`, `scree search` or another command directly.

## Migrating

### From the `scree-mcp` shell wrappers

The shell wrappers `scree-mcp`, `scree-run` and `scree-mcp-docker` are gone; `scree mcp`, `scree run` and `scree mcp -docker` do their work inside the binary. The settings file is unchanged, except that it must not be a symlink and may now hold `DOCKER_HOST` and `DOCKER_CONTEXT`.

1. In each repo, re-register and commit the new `.mcp.json`:

   ```sh
   claude mcp remove scree --scope project
   claude mcp add scree --scope project -- scree mcp
   ```

   `serve` still accepts an old entry naming `scree-mcp` as an opt-in, but that entry only starts while a `scree-mcp` program exists, and none is installed any more.
2. Delete the old wrappers from wherever you installed them, for example `rm -f ~/.local/bin/scree-mcp ~/.local/bin/scree-run`. If you had installed the Docker wrapper under the name `scree-mcp`, remove it too and add the [local Docker entry](docker.md#claude-code-with-docker).
3. Use `scree run <command>` where you used the old run wrapper.
4. Update the systemd unit, which now runs `scree run watch` instead of reading the file through `EnvironmentFile`:

   ```sh
   cp contrib/scree-watch.service ~/.config/systemd/user/
   systemctl --user daemon-reload && systemctl --user restart scree-watch
   ```

### From the single-folder version

The single-folder version read `SCREE_NOTES` and kept one index at `~/.local/share/scree/index.db`. Now each folder has its own index, and the folder comes from the repo or `-notes`.

1. Remove the `SCREE_NOTES=` line from the settings file. It is ignored, and `doctor` warns while it is there.
2. Remove a user-scope registration: `claude mcp remove scree --scope user`. Register scree per repo instead ([above](#turn-scree-on-for-the-repo)), and use a notes folder [from the command line](#a-notes-folder-that-isnt-a-repo).
3. Rebuild a notes folder's index in its new place with `scree run reindex -notes <folder>`, then delete `~/.local/share/scree/index.db`. Or keep the old file with `-db ~/.local/share/scree/index.db`, which adopts it for that folder. `scree indexes` points out the old file.
4. Update the systemd unit as above.
5. Docker: the image no longer sets `SCREE_NOTES` or `SCREE_DB`; pass `-notes /notes`. The old `/data/index.db` in the `scree-data` volume is unused; reindex.

An index from an earlier per-folder build is upgraded in place when it is opened: line ranges are filled in on the next sync without re-embedding unchanged files, and the duplicate hashes and keyword index are built from the stored chunks. An index built before the current chunk size limits needs `scree run reindex -all` once.

## Next

- [Using with Claude Code](using-with-claude-code.md)
- [CLI reference](cli.md)
- [Troubleshooting](troubleshooting.md)
