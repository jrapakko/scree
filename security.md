---
title: Security
---

scree reads files on your disk, sends some of their text to a remote API with your key, and hands text to a language model. This page says what it defends against, how, and what it leaves to you.

## Threat model

| Threat | Example |
| --- | --- |
| **A cloned repo you don't control** | Its committed `.mcp.json` (including its `env`), `.gitignore`, `.git` contents, symlinks and markdown are all chosen by someone else. |
| **A shared machine** | Other local users can create files in `/tmp`, own a `.git` above your folder, or try to read your settings and index. |
| **Prompt injection** | A markdown file contains text written to look like instructions to the model, or like a forged search result. |
| **Data leaving the machine** | Private files sent to the embedding API, or kept by a provider. |

## Trusting a repo

**Approving scree in a repo's `.mcp.json` means trusting that repo.** Claude Code applies the repo's `env`, including `PATH`, when it starts the server, so a repo you approve decides which program `scree` resolves to, and that program runs as you and can read your settings file. Everything below holds only once the real `scree` runs. Approve scree only in repos whose `.mcp.json` you trust, as you would any project server.

To take that choice back for yourself, add a local-scope entry with an absolute path. It replaces the project's entry of the same name, `env` included, for you only:

```sh
claude mcp add scree --scope local -- "$HOME/.local/bin/scree" mcp
```

On macOS a Go binary still links the system library dynamically, so `DYLD_*` variables from a repo's `env` can reach the first `scree mcp` process. Release binaries are not yet signed with the hardened runtime, which would close that; see the [roadmap](roadmap.md#open-items).

## Opt-in is enforced

Under Claude Code, `serve` refuses a folder whose root `.mcp.json` names no `scree mcp` server: a `command` of `scree`, or a path ending in `/scree`, with `mcp` as its first argument (the legacy `scree-mcp` is also accepted). The file must be a regular file, not a symlink, under 64 KB and valid JSON; it is never echoed. So a repo is indexed only if its own committed `.mcp.json` opts in, and a user-scope registration alone indexes nothing. Claude Code asks you to approve a project server once.

The first-sync cap (`SCREE_FIRST_SYNC_MB`, default 50) bounds what an approved repo can cost before you look at it.

## Configuration comes only from your settings file

`scree mcp` and `scree run` are built into the static binary. It is CGO-free; on Linux there is no dynamic loader and no shell, so attacks that act on a shell or loader before its first line, such as `LD_PRELOAD`, bash's `SHELLOPTS` with `PS4`, or exported `BASH_FUNC_*` functions, do nothing to it.

- **The settings file always comes from your passwd home.** The home directory is taken from the password database (on Linux `/etc/passwd`, then `getent passwd` by absolute path in an empty environment; on macOS the system user database), never from `$HOME`, so a repo can't swap in another settings file. If the lookup fails, they refuse to start.
- **The file must be yours.** A regular file, not a symlink, owned by you, with no permission bits for group or others, at most 64 KB.
- **The file is data.** It is parsed line by line as `NAME=value`, never sourced, so nothing in it is expanded or run. Only scree's own names, the proxy variables, and `DOCKER_HOST` and `DOCKER_CONTEXT` (for the docker CLI only) are accepted.
- **Inherited environment and arguments are ignored.** `scree mcp` refuses every argument but `-docker`, and `exec`s its own binary as `scree serve` in an environment of exactly `HOME`, `PATH=/usr/local/bin:/usr/bin:/bin`, `CLAUDE_PROJECT_DIR` and the file's settings. Not an inherited `SCREE_*`, key or proxy variable, `XDG_DATA_HOME`, `XDG_CONFIG_HOME`, `DOCKER_HOST`, or the Go runtime's `GODEBUG` and `GOMAXPROCS` gets through. A repo can't redirect your key to another endpoint through a proxy or `SCREE_EMBED_URL`, move the index, or point scree at another folder.
- **The binary is fixed.** `scree mcp` re-executes its own binary, never one found on `PATH`. `scree mcp -docker` runs only `docker` or `podman`, from a fixed list of folders.

## The folder follows from the working directory

Under Claude Code, `serve` indexes the git root of its working directory, which Claude Code sets and a `.mcp.json` cannot. `CLAUDE_PROJECT_DIR`, which a `.mcp.json` can override, is only a cross-check: `serve` refuses to start unless it is in the same repository. `serve` refuses `-notes`, `-db` and `SCREE_DB` there, so the folder and index can't be chosen by the repo's config.

## Git files from a repo are untrusted

- **Trust only your own `.git`.** A work tree is used only if its `.git` is owned by you and is not a symlink; a linked worktree's git dir and common dir must be folders you own. A `.git` owned by someone else is an error, not a silent skip, so a folder is never indexed without the ignore rules its owner expects. This is the same class of issue as git's CVE-2022-24765.
- **Stop at sticky folders.** scree never looks for a `.git` in or above a sticky folder such as `/tmp`, where anyone could plant one.
- **No symlinked git files.** `.gitignore`, `.git/info/exclude`, a `.git` file and `.screeignore` are never followed through a symlink and must be regular files. Git ignore files are read up to 256 KB.
- **No repo `.git/config`.** The global excludes file is taken only from your own git config. A repo's config could otherwise name any file on your disk.
- **Nothing echoed.** Messages about these files name the file and line number, never their content, so a crafted ignore file can't use scree to print another file.
- **Fail closed.** An unreadable `.gitignore` excludes its folder; an unreadable, symlinked or unparsable `.screeignore` stops indexing until fixed.

## One policy for every read

Indexing, search, `read_doc` and `reindex` go through the same policy, checked against the name as stored on disk:

- symlinks are followed only inside the folder, with the rules checked on the target too; symlinked folders are never descended;
- non-regular files are skipped without being opened, and files are opened without blocking, so a FIFO can't hang a session;
- a file is used only if what was opened is the regular file the policy checked, so swapping in a symlink mid-sync doesn't work;
- files over `SCREE_MAX_FILE_BYTES` (1 MiB) and names with control characters are refused.

## The home guard

A folder found automatically (from the working directory, lifted to its git root) is refused if it is your home directory, a folder above it, or a filesystem root. A dotfiles repo at `~` would otherwise make every non-repo folder under it lift to `~` and upload your whole home directory. Home comes from the password database, so a `HOME` set by a repo can't move it; `$HOME` is used only if that lookup fails. `scree mcp -docker` applies the same rule to what it mounts. Only an explicit `-notes` on the command line can index such a folder.

## Model-facing output: data, not instructions

- Each search result is a `<hit>` element and `read_doc` returns a `<doc>` element. Any `<hit`, `</hit`, `<doc` or `</doc` inside file text, in any letter case, has its `<` replaced by `&lt;`, so a file can't close its element and forge a result.
- Attribute values, including `also` lists, are quoted, and control characters are stripped from text, attributes and error messages. Tool errors are collapsed to one line.
- The server's instructions tell Claude Code that text inside `<hit>` and `<doc>` is file content, possibly copied from elsewhere, that agent folders such as `.claude/` and `.cursor/` hold instructions written for other models, and that all of it is data, never instructions.
- Hits are capped at 4000 bytes each and 64 KB per response; `read_doc` at 256 KB.
- A search query is never FTS5 syntax: each word becomes a quoted term.
- Log lines, which can contain file names and API messages, are made terminal-safe. `scree indexes` cuts each value it prints from an index to 80 characters.

## Zero data retention

By default every embedding request asks OpenRouter for endpoints with zero data retention and `data_collection: deny`. A model with no such endpoint fails with an error naming the model, rather than silently using one that keeps data. `SCREE_ZDR=0` turns this off, for other endpoints. Search queries are embedded the same way; `keyword` mode sends nothing.

## HTTP mode

Stdio through `scree mcp` is the recommended setup. `scree serve -http addr`:

- listens on loopback only (`127.0.0.1`, `::1`, `localhost`); an empty host, `0.0.0.0` and `::` count as remote;
- needs `-allow-remote` **and** `SCREE_HTTP_TOKEN` of at least 16 characters for any other address;
- with a token set, requires `Authorization: Bearer <token>` on every request, on loopback too, compared in constant time;
- rejects cross-origin browser requests before the token check;
- has a 10 s header read timeout, 120 s idle timeout and 64 KB header limit, and no read or write timeout, since a session's event stream is long-lived.

Keep the token in the settings file, not on a command line, for example `printf 'SCREE_HTTP_TOKEN=%s\n' "$(openssl rand -hex 32)" >> ~/.config/scree/env`. A Claude Code HTTP registration stores the header, token included, in `~/.claude.json`.

## Index files

The index directory is created with mode 0700 and its files 0600. The index file, its `-wal` and `-shm` files and `writer.lock` are never opened through a symlink. The index stores chunk text in plain form so search can return it. `scree indexes -prune` removes only real directories directly inside the indexes folder and never follows a symlink out of it.

## Docker and SELinux

`scree mcp -docker` never pulls an image (`--pull=never`), since the container receives your key; mounts the work tree read-only; refuses your home directory, a folder above it, `/` and paths containing `:`; accepts only a volume name for the data volume; and passes settings by name so the key never appears on a command line. The docker CLI gets only `HOME`, a fixed `PATH`, the settings, and `DOCKER_HOST` or `DOCKER_CONTEXT` from the settings file. The image runs as an unprivileged user.

On SELinux hosts, the container can read the bind-mounted folder only with `:z`, which **permanently relabels every file in that folder on the host** to `container_file_t`, even for a read-only mount. That is a trade-off you choose with `SCREE_MOUNT_OPTS=ro,z`: mount a repo or a notes folder, never your home directory, or use the native binary, which relabels nothing. See [Docker](docker.md).

## What scree does not protect against

- **A repo you approved.** Its `.mcp.json` chooses which program runs as `scree` (see [Trusting a repo](#trusting-a-repo)), and its markdown is embedded with your key, within the first-sync cap.
- **The provider seeing your text.** Chunks and queries are sent to the embedding API over HTTPS. Zero data retention is a routing request honoured by OpenRouter and its providers, not something scree can verify.
- **Files you didn't exclude.** Anything the rules allow is embedded. Run `doctor -v` before turning a repo on, and use `.screeignore` or `scree: ignore`.
- **Prompt injection, fully.** Neutralizing tags and labelling text as data reduce the risk; a model can still be swayed by text it reads.
- **Your own user account.** Any process running as you can read the settings file, the key and the index.
- **Plain HTTP off loopback.** `-allow-remote` serves HTTP without TLS; put it behind TLS or a VPN.
- **Platform gaps.** On Windows, `scree mcp` and `scree run` refuse to run and there are no ownership checks. On macOS, `DYLD_*` variables can reach the first process. File locking may not hold on network filesystems. Under Docker, your global git excludes file isn't visible, so the container may index files native scree ignores.

## Reporting a problem

Open an issue at [github.com/jrapakko/scree/issues](https://github.com/jrapakko/scree/issues). There is no security email. For a vulnerability, describe the class of problem and the affected component; leave out a working exploit, and the maintainer will follow up.
