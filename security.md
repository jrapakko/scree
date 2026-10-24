---
title: Security
---

scree reads files on your disk, sends some of their text to a remote API with your key, and hands text to a language model. This page says what it defends against, how, and what it leaves to you.

## Threat model

| Threat | Example |
| --- | --- |
| **A cloned repo you don't control** | Its committed `.mcp.json`, `.gitignore`, `.git` contents, symlinks and markdown are all chosen by someone else. |
| **A shared machine** | Other local users can create files in `/tmp`, own a `.git` above your folder, or try to read your settings and index. |
| **Prompt injection** | A markdown file contains text written to look like instructions to the model, or like a forged search result. |
| **Data leaving the machine** | Private files sent to the embedding API, or kept by a provider. |

## Configuration comes only from your settings file

- **The settings file is the only config source for the wrappers.** `scree-mcp`, `scree-run` and `scree-mcp-docker` load `~/.config/scree/env` and nothing else. They refuse to start if it is missing, not owned by you, or has any permission bits for group or others.
- **Inherited environment and arguments are ignored.** A repo's committed `.mcp.json` can launch `scree-mcp` with its own environment and arguments. So before reading the file, the wrappers reset `PATH`, unset every inherited `SCREE_*`, `OPENROUTER_API_KEY` and proxy variable, and refuse all arguments (`scree-mcp`, `scree-mcp-docker`). A repo can't redirect your key to another endpoint through a proxy or `SCREE_EMBED_URL`, swap in another binary, or point scree at another folder.
- **The file is data.** It is parsed line by line as `NAME=value`, never sourced, so nothing in it is expanded or run, and only scree's own names and the proxy variables are accepted.
- **The binary is fixed.** `scree-mcp` runs the `scree` in its own folder, never one found on `PATH`. `scree-mcp-docker` runs only `docker` or `podman`.
- **The folder follows from the repo.** Under Claude Code, `serve` refuses `-notes`, `-db` and `SCREE_DB`, and indexes the git root above `CLAUDE_PROJECT_DIR`.
- **Opt-in per repo.** Only a repo whose `.mcp.json` names `scree-mcp` is indexed, and Claude Code asks you to approve a project server once. **New (step 6-1):** the first-sync cap (`SCREE_FIRST_SYNC_MB`, default 50) bounds what an approved repo can cost before you look at it.

## Git files from a repo are untrusted

- **Trust only your own `.git`.** A work tree is used only if its `.git` is owned by you and is not a symlink; a linked worktree's git dir and common dir must be folders you own. A `.git` owned by someone else is an error, not a silent skip, so a folder is never indexed without the ignore rules its owner expects. This is the same class of issue as git's CVE-2022-24765.
- **Stop at sticky folders.** scree never looks for a `.git` in or above a sticky folder such as `/tmp`, where anyone could plant one.
- **No symlinked git files.** `.gitignore`, `.git/info/exclude`, a `.git` file and `.screeignore` are never followed through a symlink and must be regular files. Git ignore files are read up to 256 KB.
- **No repo `.git/config`.** The global excludes file is taken only from your own git config. A repo's config could otherwise name any file on your disk.
- **Nothing echoed.** Messages about these files name the file and line number, never their content, so a crafted ignore file can't use scree to print another file.
- **Fail closed.** An unreadable `.gitignore` excludes its folder; an unreadable or symlinked `.screeignore` stops indexing until fixed.

## One policy for every read

Indexing, search, `read_doc` and `reindex` go through the same policy, checked against the name as stored on disk:

- symlinks are followed only inside the folder, with the rules checked on the target too; symlinked folders are never descended;
- non-regular files are skipped without being opened, and files are opened without blocking, so a FIFO can't hang a session;
- a file is used only if what was opened is the regular file the policy checked, so swapping in a symlink mid-sync doesn't work;
- files over `SCREE_MAX_FILE_BYTES` (1 MiB) and names with control characters are refused.

## The home guard

A folder found automatically (from `CLAUDE_PROJECT_DIR` or the current directory, lifted to its git root) is refused if it is your home directory, a folder above it, or a filesystem root. A dotfiles repo at `~` would otherwise make every non-repo folder under it lift to `~` and upload your whole home directory. The Docker wrapper applies the same rule to what it mounts. Only an explicit `-notes` on the command line can index such a folder.

## Model-facing output: data, not instructions

- Each search result is a `<hit>` element and `read_doc` returns a `<doc>` element. Any `<hit`, `</hit`, `<doc` or `</doc` inside file text, in any letter case, has its `<` replaced by `&lt;`, so a file can't close its element and forge a result.
- Attribute values are quoted, and control characters are stripped from text, attributes and error messages. Tool errors are collapsed to one line.
- The server's instructions tell Claude Code that text inside `<hit>` and `<doc>` is file content, possibly copied from elsewhere, and is data, never instructions.
- Hits are capped at 4000 bytes each and 64 KB per response; `read_doc` at 256 KB.
- Log lines, which can contain file names and API messages, are made terminal-safe.

## Zero data retention

By default every embedding request asks OpenRouter for endpoints with zero data retention and `data_collection: deny`. A model with no such endpoint fails with an error naming the model, rather than silently using one that keeps data. `SCREE_ZDR=0` turns this off, for other endpoints. Search queries are embedded the same way.

## HTTP mode

Stdio through the wrapper is the recommended setup. `scree serve -http addr`:

- listens on loopback only (`127.0.0.1`, `::1`, `localhost`); an empty host, `0.0.0.0` and `::` count as remote;
- needs `-allow-remote` **and** `SCREE_HTTP_TOKEN` of at least 16 characters for any other address;
- with a token set, requires `Authorization: Bearer <token>` on every request, on loopback too, compared in constant time;
- rejects cross-origin browser requests before the token check;
- has a 10 s header read timeout, 120 s idle timeout and 64 KB header limit.

Keep the token in the settings file, not on a command line, for example `printf 'SCREE_HTTP_TOKEN=%s\n' "$(openssl rand -hex 32)" >> ~/.config/scree/env`. A Claude Code HTTP registration stores the header, token included, in `~/.claude.json`.

## Index files

The index directory is created with mode 0700 and its files 0600. The index stores chunk text in plain form so search can return it.

## Docker and SELinux

The Docker wrapper never pulls an image (`--pull=never`), since the container receives your key; mounts the repo read-only; refuses your home directory, a folder above it, `/` and paths containing `:`; accepts only a volume name for the data volume; and passes variables by name so the key never appears on a command line. The image runs as an unprivileged user.

On SELinux hosts, the container can read the bind-mounted folder only with `:z`, which **permanently relabels every file in that folder on the host** to `container_file_t`, even for a read-only mount. That is a trade-off you choose with `SCREE_MOUNT_OPTS=ro,z`: mount a repo or a notes folder, never your home directory, or use the native binary, which relabels nothing. See [Docker](docker.md).

## What scree does not protect against

- **The provider seeing your text.** Chunks and queries are sent to the embedding API over HTTPS. Zero data retention is a routing request honoured by OpenRouter and its providers, not something scree can verify.
- **Files you didn't exclude.** Anything the rules allow is embedded. Run `doctor -v` before opting a repo in, and use `.screeignore` or `scree: ignore`.
- **Prompt injection, fully.** Neutralizing tags and labelling text as data reduce the risk; a model can still be swayed by text it reads.
- **Your own user account.** Any process running as you can read the settings file, the key and the index.
- **A repo you approved.** Once you approve its `.mcp.json`, its markdown is embedded with your key, within the first-sync cap.
- **Plain HTTP off loopback.** `-allow-remote` serves HTTP without TLS; put it behind TLS or a VPN.
- **The settings file's location.** The wrappers choose the file from `SCREE_ENV_FILE` if it is set in their environment, before clearing other variables. The chosen file still has to be yours, mode 600, and hold only scree settings.
- **Platform gaps.** Windows has no ownership checks. File locking may not hold on network filesystems. Under Docker, your global git excludes file isn't visible, so the container may index files native scree ignores.

## Reporting a problem

Open an issue at [github.com/jrapakko/scree/issues](https://github.com/jrapakko/scree/issues). There is no security email. For a vulnerability, describe the class of problem and the affected component; leave out a working exploit, and the maintainer will follow up.
