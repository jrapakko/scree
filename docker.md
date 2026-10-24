---
title: Docker
---

**The native binary is recommended.** It is one static file with no runtime, it sees your global git excludes file, it gets file events on every OS, and it relabels nothing on SELinux hosts. Use the image when you want isolation or a reproducible build.

## The image

Built from the repo's `Dockerfile`: a Go build stage, then `gcr.io/distroless/static-debian12:nonroot`. It holds only the static binary, CA certificates and the licenses (`/usr/share/doc/scree`), runs as uid 65532, and is about 18 MB.

- Entry point `scree`; default command `watch -notes /notes`.
- `XDG_DATA_HOME=/data`, a volume, so indexes live under `/data/scree/indexes/`.

```sh
docker build -t scree .            # or: podman build -t scree .
```

Tagged releases push `ghcr.io/jrapakko/scree:<version>` and `:<major>.<minor>` for linux/amd64 and linux/arm64. No version is tagged yet.

File events reach the container through a bind mount on Linux. Docker Desktop on macOS doesn't pass them through, so `watch` misses edits there.

## SELinux: `:z` is required, and it relabels

On SELinux hosts (Fedora, RHEL, CentOS Stream), every bind mount needs `:z`. Without it the container can't read the folder and scree fails with a permission error such as `open /notes: permission denied`.

`:z` **permanently relabels every file in the folder on the host** to `container_file_t`, even when the mount is read-only. Mount a repo or a notes folder, never your home directory. On systems without SELinux, drop `:z`. If you'd rather not relabel anything, use the native binary.

## `docker run`

```sh
# background indexer for one folder
docker run -d --name scree-watch --restart unless-stopped \
  -e OPENROUTER_API_KEY \
  -v "$HOME/wiki:/notes:ro,z" -v scree-data:/data \
  scree watch -notes /notes

# one-off commands against the same index
docker run --rm -e OPENROUTER_API_KEY -v "$HOME/wiki:/notes:ro,z" -v scree-data:/data scree doctor -notes /notes
docker run --rm -e OPENROUTER_API_KEY -v "$HOME/wiki:/notes:ro,z" -v scree-data:/data scree search -notes /notes "release checklist"
```

`-e OPENROUTER_API_KEY` with no value passes the variable through from your shell, so the key never appears on the command line.

## Compose

`compose.yaml` has two services sharing the `scree-data` volume. It reads `OPENROUTER_API_KEY` from your shell or from `.env` next to `compose.yaml`, and the host folder from `SCREE_NOTES_DIR`, which is only compose's name for the folder mounted at `/notes`:

```sh
SCREE_NOTES_DIR=$HOME/wiki docker compose up -d     # starts watch
docker compose logs -f watch
```

- `watch` runs `watch -notes /notes`, restarts unless stopped, and mounts the folder `ro,z`.
- `serve` runs `serve -notes /notes` with stdin open, behind the `mcp` profile, so `up` doesn't start it.
- Both pass `SCREE_MODEL`, `SCREE_DIMENSIONS`, `SCREE_MAX_FILE_BYTES`, `SCREE_FIRST_SYNC_MB`, `SCREE_ZDR` and `SCREE_EMBED_URL`, with the same defaults as the binary; `serve` also passes `SCREE_HTTP_TOKEN`.

The two containers share the index through the volume, with one writer at a time as for native processes. SQLite's WAL mode lets `serve` read while `watch` writes.

## Claude Code with Docker

`scree mcp -docker` runs one container per Claude Code session over stdio. The repo's committed `.mcp.json` names `scree mcp`, which everyone shares. To use Docker yourself, add a local-scope entry, which takes precedence over the project's for you:

```sh
claude mcp add scree --scope local -- "$HOME/.local/bin/scree" mcp -docker
```

The native `scree` binary is still needed on the host: it is the launcher. It loads the settings file like `scree mcp`, then runs:

```
docker run -i --rm --pull=never \
  -e OPENROUTER_API_KEY [-e SCREE_MODEL …] \
  -v <repo>:<repo>:<SCREE_MOUNT_OPTS> -v scree-data:/data \
  -w <repo> -e CLAUDE_PROJECT_DIR=<project dir> \
  scree:latest serve
```

- **`--pull=never`:** a missing image is never pulled from a registry, since the container receives your key. Build or pull the image yourself first.
- **The repo mounts at its own path,** read-only, so each repo hashes to its own index in the shared volume. `<repo>` is the git root of the working directory, found only through a `.git` you own that is not a symlink, never in or above a sticky folder, so `.gitignore` and `.git/info/exclude` are visible.
- **`CLAUDE_PROJECT_DIR` must lead to the same work tree,** not just the same repository as native `serve` allows; otherwise it refuses to start.
- **`serve` runs in the mounted folder** (`-w`), where it applies the same folder rule and the `.mcp.json` opt-in check as native `serve`.
- **Refused mounts:** your home directory (from the password database), a folder above it, `/`, a relative `CLAUDE_PROJECT_DIR`, and paths containing `:`.
- **Settings by name only:** `OPENROUTER_API_KEY`, `SCREE_MODEL`, `SCREE_DIMENSIONS`, `SCREE_ZDR`, `SCREE_FIRST_SYNC_MB`, `SCREE_MAX_FILE_BYTES`, `SCREE_EMBED_URL` and `SCREE_HTTP_TOKEN`, each only if the settings file sets it. Proxy variables are not passed to the container.
- **The docker CLI's environment** is `HOME`, a `PATH` of `/usr/local/bin:/usr/bin:/bin:/opt/homebrew/bin`, the settings, and `DOCKER_HOST` or `DOCKER_CONTEXT` if the settings file sets them. Nothing comes from Claude Code's or the repo's environment, so set those two in the settings file if your engine needs them.
- **Your global git excludes file is not visible in the container,** so it may index files that native scree ignores. Put such patterns in the repo's `.screeignore`.
- **A worktree whose git dir lies outside the mounted folder** can't be read in the container.
- Like `scree mcp`, it ignores inherited settings and takes no other arguments.

Settings for it, in the settings file:

| Variable | Default | Accepted |
| --- | --- | --- |
| `SCREE_DOCKER` | `docker` | `docker` or `podman`, found in the folders above, never through `PATH` |
| `SCREE_IMAGE` | `scree:latest` | any local image, such as a `ghcr.io/jrapakko/scree:<version>` you pulled yourself |
| `SCREE_DATA_VOLUME` | `scree-data` | a volume name, not a path |
| `SCREE_MOUNT_OPTS` | `ro` | `ro`, `ro,z`, `ro,Z`; use `ro,z` on SELinux hosts |
| `DOCKER_HOST`, `DOCKER_CONTEXT` | unset | passed to the docker CLI only |

## HTTP inside a container

Inside a container, `serve -http` has to bind a non-loopback address to be reachable through a published port, so it needs `-allow-remote` and `SCREE_HTTP_TOKEN`. Publish the port on the host's loopback only, for example `-p 127.0.0.1:7077:7077`.
