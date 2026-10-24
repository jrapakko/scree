# scree

Local semantic search over your markdown notes, served to Claude Code over MCP.

Scree splits notes by heading, embeds the pieces through an API, and stores them in a SQLite file on disk. Claude Code asks for the few chunks that match a question instead of loading whole files. The markdown stays the source of truth; the index can be deleted and rebuilt at any time.

See [docs/SPEC.md](docs/SPEC.md) for the design.

## Build

Requires Go 1.26. No CGO.

```sh
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/scree ./cmd/scree
```

## Configure

```sh
export SCREE_NOTES=~/notes
export OPENROUTER_API_KEY=sk-or-...    # https://openrouter.ai/settings/keys
```

| Variable | Default |
| --- | --- |
| `SCREE_NOTES` | none, required (or `-notes`) |
| `SCREE_DB` | `$XDG_DATA_HOME/scree/index.db` (or `-db`) |
| `SCREE_MODEL` | `openai/text-embedding-3-small` |
| `SCREE_DIMENSIONS` | `512`; `0` uses the model's default size |
| `SCREE_EMBED_URL` | `https://openrouter.ai/api/v1/embeddings` |

Changing the model or dimensions requires `scree reindex -all`. The index records which model built it and refuses to mix them.

## Use

```sh
scree doctor             # check config, index, and the API
scree reindex            # index new and changed notes
scree search "how do I rotate the backup keys"
scree watch              # keep the index current (run as a service)
scree serve              # MCP over stdio
scree serve -http 127.0.0.1:7077
scree reindex -all       # rebuild from scratch
```

### Claude Code

```sh
claude mcp add scree --scope user \
  -e SCREE_NOTES="$HOME/notes" -e OPENROUTER_API_KEY="$OPENROUTER_API_KEY" \
  -- "$PWD/bin/scree" serve
```

Tools: `search_notes(query, k)`, `read_note(path)`, `reindex(path?)`.

### Background service (systemd)

```sh
mkdir -p ~/.local/bin ~/.config/systemd/user ~/.config/scree
cp bin/scree ~/.local/bin/
cp contrib/scree-watch.service ~/.config/systemd/user/
printf 'SCREE_NOTES=%s\nOPENROUTER_API_KEY=%s\n' "$HOME/notes" "sk-or-..." > ~/.config/scree/env
chmod 600 ~/.config/scree/env
systemctl --user daemon-reload && systemctl --user enable --now scree-watch
journalctl --user -u scree-watch -f
```

## How it works

- `scree watch` is the one writer. It syncs on startup by content hash, then watches with fsnotify, waits 1.5 s after the last event for a path, and re-embeds only files whose hash changed.
- `scree serve` reads the same file. SQLite's WAL mode lets it search while the watcher writes. Its `reindex` tool also writes, and SQLite serializes it with the watcher.
- Vectors live in [vec1](https://sqlite.org/vec1), SQLite's vector extension, with a flat (exact) cosine index. It is compiled to WebAssembly by [ncruces/go-sqlite3](https://github.com/ncruces/go-sqlite3), which is what keeps the binary CGO-free.
- Hidden files and folders (`.obsidian`, `.git`) and non-markdown files are ignored. `read_note` refuses paths and symlinks that lead outside the notes folder.

Idle memory is about 13 MB per process.
