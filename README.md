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

2. **Save the key** in the settings file. `scree key` prompts for it without echoing it, so it never lands in your shell history or on screen, writes `~/.config/scree/env` with mode 600, and makes one test call to the API:

   ```sh
   bin/scree key
   ```

   It keeps any other lines in the file and replaces an earlier key. `scree key < file` reads the key from the first line of a file instead, and replaces a saved key only with `-replace`; `-offline` skips the test call, and `-check` makes only the test call. Ctrl-C at the prompt saves nothing.

3. **Install** the binary:

   ```sh
   mkdir -p ~/.local/bin
   install -m 755 bin/scree ~/.local/bin/
   ```

   Claude Code must find `scree` on its `PATH`. If `~/.local/bin` isn't on it, add it in your shell profile.

4. **Check a repo before indexing it.** `scree run` loads the settings file and runs a scree command. From inside the repo:

   ```sh
   cd ~/code/myrepo
   scree run doctor -v
   ```

   `doctor -v` lists every file that will be ignored and estimates the cost of indexing the rest. Add a [`.screeignore`](#ignoring-files) or `.gitignore` entry before indexing if anything private shows up as indexable.

5. **Turn scree on for the repo.** From the repo's root:

   ```sh
   claude mcp add scree --scope project -- scree mcp
   ```

   This writes `.mcp.json` with `"command": "scree", "args": ["mcp"]`, with no key and no paths, so commit it. Claude Code asks each person to approve a project server once. Every session in the repo then keeps the index current; the first session builds it in the background. `serve` refuses a repo whose own `.mcp.json` doesn't name `scree mcp`, so a repo without this entry is never indexed, even if scree is also registered elsewhere. Don't register scree at user scope anyway.

   **Approving a project server means trusting the repo.** Claude Code applies the `env` of a repo's `.mcp.json`, including `PATH`, before it starts the server, so a repo you approve decides which program `scree` is, and that program gets your settings file. Approve scree only in repos whose `.mcp.json` you trust, as you would any project server. To take that choice back for yourself, see [absolute paths](#a-plain-scree-or-an-absolute-path).

6. **Optional: a notes folder outside any repo.** See [A notes folder](#a-notes-folder).

## Build

Requires Go 1.26. No CGO. Tagged releases also publish prebuilt binaries for Linux, macOS and Windows (amd64 and arm64) on the GitHub releases page, a `ghcr.io/jrapakko/scree` image, and a [Claude Desktop bundle](#claude-desktop-one-click-bundle), which `scripts/mcpb.sh` zips from the release's binaries.

```sh
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/scree ./cmd/scree
```

## Configure

Settings come from environment variables. The [settings file](#settings-file) is the one place to keep them, including the API key. Don't `export` the key in a shell profile. With `OPENROUTER_API_KEY` unset, a command run directly (`scree search`, `doctor`, `reindex`, `watch`) still uses the key in the settings file, by running as `scree run` would; see below.

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
| `SCREE_PAGER` | `less -FRX` from `/usr/local/bin`, `/usr/bin` or `/bin`; the pager for `scree search`'s text view on a terminal, run without a shell: an absolute path or a name in those folders (never looked up on `PATH`), then its arguments. Set it empty for none |
| `SCREE_COLORS` | `word=1:match=1;33:link=36:lineno=2`; the text view's colours as SGR codes (digits and `;` only) for the word headings, the matched words, the locations and the line numbers. `match=` turns one off |
| `OPENROUTER_API_KEY` | none; when unset and the [settings file](#settings-file) holds one (`scree key` writes it there), `search`, `reindex`, `doctor` and `watch` run as `scree run` would. Without a key (unset or empty), notes are indexed for keyword search only and `auto` search runs `keyword` |

`$XDG_DATA_HOME` defaults to `~/.local/share`; a relative value is ignored. With neither it nor an absolute home directory, scree refuses to open an index unless `-db` names one.

**Which folder.** `-notes <dir>` names it on the command line. Without it, CLI commands use the git root of the current directory, or the current directory outside git. Under Claude Code, `serve` indexes the git root of the working directory Claude Code starts it in (the project, or the active worktree), refuses `-notes`, `-db` and `SCREE_DB`, and requires `CLAUDE_PROJECT_DIR` to be in the same repo; a `.mcp.json` can set environment variables but not the working directory. It also refuses a folder whose root `.mcp.json` names no `scree mcp` server. Outside Claude Code, `serve` has no default folder: pass `-notes`. A folder found automatically is refused if it is your home directory (from the password database, falling back to `$HOME` only if that lookup fails), a folder above it, or a filesystem root; on the command line, pass `-notes` to index it anyway. `serve` refuses such a folder even as `-notes` (a client such as Claude Desktop passes the folder a user picked) unless `-allow-home` is given. The check compares the folders themselves, not just their paths, so another path to your home folder (a bind mount, different letter case on macOS, `/System/Volumes/Data/Users/…`) is refused too. It only compares the chosen folder with your home folder and the folders above it, so a folder that holds a mount of your home somewhere inside it (as some containers and devcontainers set up) is not caught. Each index records its folder and refuses to be used for another one.

Changing the model or dimensions requires `scree reindex -all`. The index records which model built it and refuses to mix them.

### Privacy

Every embedding request asks OpenRouter to route only to endpoints with zero data retention and `data_collection: deny`. A model with no such endpoint fails with a clear error naming the model, rather than falling back to one that keeps data. `SCREE_ZDR=0` turns this off, which other OpenAI-compatible endpoints may need. Only chunk text, with its path and heading, is sent to the API; keyword search runs locally.

### Ignoring files

An ignored file is never read for indexing, never sent to the embedding API, never returned by search, and refused by `read_doc` and `reindex`. Four layers decide:

- **Built-in:** hidden files and folders, except the root `.claude/` (not `.claude/worktrees/`), `.github/`, `.gitlab/`, `.gitea/`, `.forgejo/`, `.cursor/`, `.codex/`, `.gemini/`, `.windsurf/`, `.circleci/` and `.buildkite/`; anything that isn't `.md`, `.markdown` or `.mdc`; symlinks that lead outside the folder; files over `SCREE_MAX_FILE_BYTES`; non-regular files such as FIFOs, which are never opened and which `doctor` reports as unreadable; and names with control characters.
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

`include-hidden .name/` in `.screeignore` indexes another root hidden folder, and a pattern like `/.cursor/` drops one of the defaults. A `!` rule re-includes files inside an excluded folder, but never a file that a rule matched by name, so `*.private.md` stays ignored even under `!archive/2024/`; `doctor -v` lists what `!` re-included. If `.screeignore` can't be read, is a symlink, or has a line scree can't parse, nothing is indexed until it is fixed; `doctor` names the bad lines by number.

```
---
scree: ignore
---
```

Run `scree doctor -v` to see what each layer ignores before relying on a pattern. The full rules are in [docs/SPEC.md](docs/SPEC.md#ignoring-files).

## Use

```sh
scree key                # save the API key in the settings file
scree doctor             # check config, index, and the API
scree doctor -v          # also list every ignored file, and check the keyword index
scree reindex            # index new and changed files
scree reindex docs/a.md  # re-index one file
scree reindex -all       # rebuild from scratch
scree search "how do I rotate the backup keys"
scree search -mode keyword PURGE_OLD_RECORDS   # exact words, no API call
scree search -format text "ci seed"              # hits grouped by query word (the default on a terminal)
scree search -no-pager "ci seed"                 # the same, printed without the pager
scree search -format json "…"                    # one JSON object per hit
scree search -format grep "…"                    # path:line:text, for fzf or vim -q
scree watch              # keep the index current (run as a service)
scree serve -notes .     # MCP over stdio, from a terminal
scree serve -notes . -http 127.0.0.1:7077   # MCP over HTTP, loopback only
scree indexes            # list per-folder indexes; -prune deletes ones whose folder is gone
```

Every command but `indexes` and `key` takes `-notes <dir>`, `-db <file>` and `-gitignore=false`. `serve` needs `-notes` outside Claude Code. From a terminal, `search`, `reindex`, `doctor` and `watch` run as `scree run <command>` would whenever `OPENROUTER_API_KEY` is unset and the settings file holds a key; `scree run` loads the file in any case, for example `scree run search "…"`.

### Search modes

`scree search -mode` and the `search_docs` tool's `mode` take:

- **`auto`** (the default): `semantic`, unless the query has an identifier-like word, a word with a letter plus `_` or a digit, such as `PURGE_OLD_RECORDS`, `psycopg_pool`, `R7-Q2` or `v2`, or a word from the folder's code vocabulary (below); then `hybrid`. `ID`, `API`, `2026` and `well-known` don't count.
- **`hybrid`**: a vector search and an SQLite FTS5 keyword search over chunk text, heading and path, merged by reciprocal rank fusion, so exact names rank first.
- **`semantic`**: vectors only.
- **`keyword`**: FTS5 only. It makes no API call, so it works without a key or while the API is down. A failed embedding call says so.

With no API key, `auto` runs `keyword` and its output ends with `[mode: auto → keyword: no API key; run scree key, or set OPENROUTER_API_KEY]`; `-mode semantic` or `hybrid` is an error. `serve` never reads the settings file itself; under Claude Code it says the same (`scree mcp` passes the file's key), and elsewhere, as from Claude Desktop, `add an OpenRouter API key in the extension's settings, set OPENROUTER_API_KEY, or start through scree mcp`. On Windows it drops the `scree key` and `scree mcp` parts.

Without a key, indexing still works: each note is chunked and stored for keyword search, with no API call, and `reindex` reports those files as `indexed (N keyword only: no API key)`. Once a key is set, the next sync (`scree reindex`, or the writer starting) embeds exactly those files and no others. Until then, `semantic` and `hybrid` miss them and say so (`[N files indexed for keyword search only; scree reindex embeds them]`), and `scree doctor` counts them on its index line. An API error or outage with a key is not treated as no key: the file fails or waits for the API as before. A key the API rejects (401) is: from then on that process indexes for keyword search only, without asking the API again, `auto` runs `keyword` and says `the embedding API rejected the API key; replace it: <hint>`, and the next process with a working key embeds those files. A 403, which OpenRouter sends for an input its moderation flags, fails only that file, as other API errors do; the key is not taken as rejected.

Scree trims spaces and newlines around `OPENROUTER_API_KEY`. A value that is an unfilled `${...}` placeholder, or that `scree key` would refuse (inner whitespace, control characters, quotes), counts as no key, with one log line that never shows the value.

On a private set of 16 plain-language and 7 identifier queries over real notes, `auto` kept plain-question file MRR at 0.89 while raising identifier MRR from 0.46 to 0.80. Always-on `hybrid` raised identifiers as much but cost plain questions, which is why it is not the default.

The code vocabulary catches names with no `_` or digit, such as `psycopg` or `PageFetcher`. Each sync reads, under the same ignore rules as markdown but never inside a hidden folder (the `.claude/`-style allowlist is for markdown), the dependency names in `requirements*.txt`, `pyproject.toml`, `package.json`, `go.mod` and `Cargo.toml`, and the mixed-case names (`PageFetcher`, `useState`) in import statements and declarations of Python, JavaScript, TypeScript and Go files; vendored folders and unchanged files are skipped, and at most 20,000 files are opened per sync. Common words are kept out, even as package names: `golang.org/x/sync` adds only its full path, and a stoplist of about 350 words drops function words (`how`, `the`), package names that are ordinary words (`sync`, `requests`, `next`, `config`) and plain words written in mixed case (`setUp`, `fileName`); a word it misses can still send a plain question to `hybrid`. `PageFetcher's` in a query counts as `PageFetcher`. Only the lowercased names are kept, in the local index; code is never chunked, embedded or sent to the API. On two repos with code and docs, this raised results for questions naming code a little and sent no plain question to `hybrid`.

Routing uses the vocabulary alone, whether or not the notes contain the name: `hybrid`'s keyword side matches every word of the query, so a question naming code gains even when the docs rarely name it. The extractor is line-based heuristics, not a parser, so it can misread, say, text from a string as a name; such a misread can at worst send a question to `hybrid`, and is never printed or embedded. `scree doctor` reports how many names were read and how many the notes also contain (`pagefetcher` matches `PageFetcher`; a dotted or slashed name such as `golang.org/x/sync` must match as a phrase), and `-v` shows a sample of the latter only; a name found only in code is never printed.

The query is never passed to FTS5 as syntax: each word becomes a quoted term. Words keep `_` and `-`, so `psycopg_pool` is one term, and `pool` alone finds it only through the vector side.

### Results

On a terminal, `scree search` prints a text view grouped by query word. Each hit is listed once, under the first query word its text, path or heading holds, as keyword search matches it: its heading and the lines that hold the word (at most 10, each cut at 160 characters), then its location, then every line of that range from the current file, numbered (read through the same ignore rules, cut at 3,500 bytes). Hits with no query word (usually semantic-only) and stale hits come last; a stale hit has no numbered lines.

```
seed:
  ### 5.1 Files
  - `.forgejo/workflows/ci.yml`: build, test, and the seed from the run number
	>docs/plan.md:189-191
		>>189  ### 5.1 Files
		>>190
		>>191  - `.forgejo/workflows/ci.yml`: build, test, and the seed from the run number

other hits:
  ## Keys
	>docs/keys.md:3-4
		>>3  ## Keys
		>>4  Rotate them yearly.

  # Old notes
	>docs/old.md (stale)
[mode: auto → hybrid]
```

A word matches whole tokens, as keyword search does, including inside a path written in a note (`ci` matches `.forgejo/workflows/ci.yml`) and across a line break. Each location is a link to the file in terminals that support OSC 8 links. The query words are coloured wherever they match in the excerpt and the numbered lines, and the word headings, locations and line numbers have their own colours (`SCREE_COLORS`). Set `NO_COLOR` or `TERM=dumb` to turn links and colours off (`scree run search` honours it). Use `-mode keyword` for a grep-like view with no semantic hits.

On a terminal the text view goes through a pager, so it starts at the top: `less -FRX` (it quits at once if the view fits on one screen, passes the colours and links, and leaves the text on screen when you quit), or `SCREE_PAGER`. `less` 566 and later pass OSC 8 links with `-R`; older versions show them as raw escapes, so use `-no-pager` or `SCREE_PAGER=` there. `-no-pager`, `SCREE_PAGER=` (set but empty), a pipe, or a pager that cannot start prints directly. Quitting the pager early is not an error. The pager runs with `LESSCHARSET=utf-8` unless you set `LESSCHARSET`, so `less` shows characters such as `→` even without `LANG`. `scree run search`, and a direct `scree search` using the settings file's key, take `SCREE_PAGER` and `SCREE_COLORS` from the settings file only. They pass your `TERM` on only if it is a plain terminal name such as `xterm-256color`, and never `TERMINFO`, `TERMINFO_DIRS` or `TERMCAP`. Without a usable `TERM` the default `less` is not used, because `less` needs it.

Piped, or with `-format xml`, each hit is a `<hit>` element:

```
<hit n="1" path="docs/setup.md" lines="12-19" heading="Setup > Keys" score="0.912" match="both">
chunk text
</hit>
```

- `lines` is the range of source lines the chunk came from, so Claude can read exactly those (`read_doc(path, start, end)`). If the file changed since it was indexed, search finds the chunk in the current file, exactly or ignoring whitespace changes, and reports the current range. If the text is gone, the hit has `stale="true"` and no `lines`. Changed files are queued for re-indexing.
- `match` says which search found the hit: `semantic`, `keyword` or `both`. `score` is the cosine similarity and is missing for keyword-only hits.
- Identical chunks in several files, such as committed build copies, come back as one hit. The shown copy has the shortest path, and `also="docs/print/x.md:12-18, …"` lists up to three others, with `also_more="N"` for the rest. A listed copy whose file changed is relocated like the hit, or shown as `path:stale`. Excluding build folders with `.screeignore` is still better: it saves embedding cost.
- In `auto` mode the output ends with a line such as `[mode: auto → hybrid]`.

`-format json` prints one object per line, the stable format for scripts and evals: `{"n","mode","path","start","end","heading","score","match","stale","also","also_more"}`, where `mode` is the mode that ran, `start`, `end` and `score` are `null` when absent, `also` is a list of `{"path","start","end"}` (with `"stale":true` on a copy whose text is gone), and `also_more` appears only when more copies exist.

`-format grep` prints `path:line:text` lines like `grep -n`: each line of a hit that holds a query word, or a semantic hit's first line, in rank order, each once, with control characters dropped. Pick a hit with [fzf](https://github.com/junegunn/fzf) and open it in vim, or load all of them into vim's quickfix list:

```sh
sg() { scree search -format grep "$*" | fzf --delimiter : \
  --preview 'n={2}; case $n in ""|*[!0-9]*) exit;; esac; sed -n "$(( n>5 ? n-5 : 1 )),+30p" -- {1}' \
  --bind 'enter:become(n={2}; case $n in ""|*[!0-9]*) exit 1;; esac; vim "+$n" -- {1})'; }
vim --cmd 'set efm=%f:%l:%m' -q <(scree search -format grep "ci seed")
```

Paths are relative to the folder, so run these from its top. A folder you didn't write can choose its file names and text, so treat the fields as untrusted: check the line is a number and put `--` before the path, as above, rather than pasting `{1}` or `{2}` into shell arithmetic or a command line, and give `vim -q` the `%f:%l:%m` error format, since its default one also picks up `"file" line N:` inside note text. scree leaves out hits whose path holds `:` or a control character (with a count on stderr) and starts a path beginning with `-` or `+` with `./`.

### Settings file

`scree mcp` (for Claude Code), `scree run` (from a terminal) and the systemd unit all read the same file, `~/.config/scree/env`, so the API key lives in one place with mode 600. `scree key` (step 2 of the [Quick start](#quick-start)) creates it or updates the key in it. It holds the key and optional settings, not the folder.

A command run directly, without `scree run`, uses the environment. When `OPENROUTER_API_KEY` is unset (or blank, a placeholder or invalid, as above) and this file holds a key, `search`, `reindex`, `doctor` and `watch` instead re-run themselves exactly as `scree run` would: with an environment built from this file alone, so the key goes only to the file's `SCREE_EMBED_URL` (or the default endpoint), through the file's proxy if any, and no `SCREE_*`, proxy, `SSL_CERT_FILE` or `GODEBUG` variable from your shell applies. `doctor` then says the key came from this file. A missing file, or one with no key, means no key. A file that fails the checks below gives one warning naming the problem, and the command runs without a key (`auto` search runs `keyword`). `serve` never reads the file itself; under Claude Code, `scree mcp` has already applied it.

`~` is taken from the password database (your login's home directory), not from `$HOME` or `$XDG_CONFIG_HOME`. `scree mcp` and `scree run` refuse to start if the file is missing, is a symlink, is not owned by you, or is readable by group or others; `chmod 600` fixes the last. `scree key` refuses such a file too (a missing one is fine), a `~/.config/scree` folder that is a symlink, is not yours or is writable by group or others, a symlinked `~/.config`, and a key that would make the file larger than 64 KB. On Linux the home directory comes from `/etc/passwd`, or from `getent passwd` for an LDAP or sssd login; if neither knows you, they refuse to start rather than trust `$HOME`. They don't run on Windows: set the variables in the environment and run `scree serve -notes …` or another command directly.

Write one `NAME=value` per line, and wrap values in single quotes, as `scree key` does, so a path with spaces stays one value. An `export ` prefix is allowed. The file is read as data and never run as shell, so nothing in a value is expanded. Only scree's own variables (`SCREE_*`, `OPENROUTER_API_KEY`), the proxy variables, and `DOCKER_HOST` and `DOCKER_CONTEXT` (passed only to docker by [`scree mcp -docker`](#docker-or-podman)) are accepted; any other name is an error that names the line. Other variables from the Configure table can go in the same file. A value can't itself contain the quote character that wraps it.

### Claude Code

`scree mcp` loads the settings file and runs `scree serve`. Claude Code stores only the command, so the key stays out of `~/.claude.json`. Install `scree` ([Quick start](#quick-start) step 3), then register it in each repo, from the repo's root:

```sh
claude mcp add scree --scope project -- scree mcp
```

Commit the resulting `.mcp.json`. `serve` indexes the git root of the working directory Claude Code starts it in, checks that `CLAUDE_PROJECT_DIR` is in the same repo, and checks that the repo's `.mcp.json` names `scree mcp` (a `command` of `scree`, or a path ending in `/scree`, with `mcp` as its first argument). In a worktree, the worktree is indexed. Several sessions in one repo share one index: one writes it, the others read, and one takes over when the writer exits. A repo larger than `SCREE_FIRST_SYNC_MB` is not indexed until you run `scree run reindex` in it once; search says so.

The tools are `search_docs(query, k, mode?)`, `read_doc(path, start?, end?)` and `reindex(path?)`. Each hit carries its line range, so Claude can read just those lines, and duplicates across copied folders are folded into one hit (`also=`). Still exclude build output with `.screeignore`.

The settings file is the only configuration `scree mcp` uses. A repo's committed `.mcp.json` sets the environment it starts in, so it ignores that environment: it refuses every argument but `-docker`, finds the settings file from the password database, and replaces itself (`exec`) with its own binary running `scree serve` in an environment of exactly `HOME` (your passwd home), `PATH=/usr/local/bin:/usr/bin:/bin`, `CLAUDE_PROJECT_DIR` and the settings from the file. Nothing else gets through: not an inherited `SCREE_*`, key or proxy variable, `XDG_DATA_HOME`, `XDG_CONFIG_HOME`, `DOCKER_HOST`, or the Go runtime's `GODEBUG` and `GOMAXPROCS`. `scree` is a static binary with no shell, so `LD_PRELOAD`, bash's `SHELLOPTS` and `PS4`, and exported `BASH_FUNC_*` functions, which could act on the old shell wrappers before their first line, do nothing to it. Once the real `scree` runs, the repo's environment can't send your key to another endpoint, swap in another settings file, move the index, or point scree at another folder. It can still choose which program runs as `scree` in the first place, through `PATH`; see the trust note in the [Quick start](#quick-start). (On macOS, Go binaries link the system library dynamically, so `DYLD_*` variables can still reach that first process; see the open items in [docs/SPEC.md](docs/SPEC.md#open-items).)

#### A plain `scree` or an absolute path

The committed `.mcp.json` says plain `scree`, so it works for everyone whatever their install folder, and each person's `PATH` (or the repo's `env`) finds the binary. In a config only you use, register the absolute path instead, so no `PATH` decides which program gets your settings. A local-scope entry, kept in your `~/.claude.json` for this project, takes precedence over the project's entry of the same name, `env` included:

```sh
claude mcp add scree --scope local -- "$HOME/.local/bin/scree" mcp
```

The shell expands `$HOME` when you run this, so the stored command is your absolute path, such as `/home/you/.local/bin/scree`. The repo's committed `.mcp.json` must still name `scree mcp`; that is how the repo opts in.

Stdio through `scree mcp` is the recommended setup. `serve -http` listens on loopback only. A non-loopback address needs `-allow-remote` and a `SCREE_HTTP_TOKEN` of at least 16 characters, and every request must then send `Authorization: Bearer <token>`. Keep the token in the settings file rather than on a command line:

```sh
printf 'SCREE_HTTP_TOKEN=%s\n' "$(openssl rand -hex 32)" >> ~/.config/scree/env
scree run serve -notes ~/notes -http 0.0.0.0:7077 -allow-remote
```

Run this from a terminal, not from inside a Claude Code session, where `CLAUDE_PROJECT_DIR` is set and `serve` refuses `-notes`.

A client then needs the header. If you add it with `claude mcp add --transport http ... --header "Authorization: Bearer $SCREE_HTTP_TOKEN"`, expanding the variable at the shell keeps the token out of your shell history, but Claude Code still stores the expanded header in `~/.claude.json`.

### A notes folder

A folder outside any repo, such as `~/notes`, is used from the command line:

```sh
scree run reindex -notes ~/notes
scree run search -notes ~/notes "backup password"
```

The [systemd unit](#background-service-systemd) runs `scree run watch -notes ~/notes` to keep it current. To search it from Claude Code, run `claude mcp add scree --scope project -- scree mcp` in `~/notes` and start Claude Code there; it isn't a git repo, so the folder is `~/notes` itself. A Claude Code session elsewhere can't reach it, since under Claude Code `serve` only indexes the folder the session runs in.

### Claude Desktop (one-click bundle)

**Untested.** The bundle has not been tested in any MCP client on any platform: not in Claude Desktop, and not on macOS or Windows at all. It doesn't target Linux. Only the server command it runs was exercised, on Linux with a Linux build. Expect rough edges, and please report what you find.

Releases after 0.3.6 include `scree_<version>.mcpb`, an [MCPB](https://github.com/modelcontextprotocol/mcpb) bundle for Claude Desktop on macOS and Windows. It holds a macOS universal binary and a Windows amd64 binary (Windows on ARM runs it under emulation). Opening the file in Claude Desktop installs it and asks for two settings:

- **Notes folder** (required): the one folder scree indexes and searches. Choosing it is the opt-in, so the `.mcp.json` check doesn't apply. Pick a notes folder or a repo: the server refuses your home folder, a folder above it, or a drive root, and doesn't start.
- **OpenRouter API key** (optional): without it, scree indexes and searches by keyword only. With it, search also works by meaning, and the notes and each query are sent to OpenRouter to be embedded. Spaces or a newline pasted around it are trimmed. If OpenRouter rejects the key, scree keeps working by keyword only and its search results say the key was rejected.

Claude Desktop then runs `server/scree serve -notes <folder>` from the bundle, with `OPENROUTER_API_KEY` set to the key or empty. The bundle doesn't read the [settings file](#settings-file), so `SCREE_MODEL`, `SCREE_DIMENSIONS` and `SCREE_EMBED_URL` there don't apply. A folder already indexed with another model is refused. A folder with more than 50 MB of markdown isn't indexed on its own; search results say so, and Claude is told to ask you before running the `reindex` tool with no path, which indexes it anyway. The index goes where the CLI puts it, so the CLI can search the same folder; keep the two at the same version, since one may upgrade the index past what the other reads.

The binaries aren't signed or notarized. On macOS, Gatekeeper may refuse to run `server/scree` from a downloaded bundle until you allow it under System Settings → Privacy & Security, and the unsigned binary also leaves the `DYLD_*` caveat in [docs/SPEC.md](docs/SPEC.md). Windows SmartScreen may warn in the same way. Each release also has `scree_<version>.mcpb.sha256` (`sha256sum` format; check with `sha256sum -c`, or `shasum -a 256 -c` on macOS), and the registry entry `io.github.jrapakko/scree` records the same sha256. Whether a client checks it on install is untested.

### Background service (systemd)

```sh
# needs ~/.config/scree/env from "Settings file" above
mkdir -p ~/.local/bin ~/.config/systemd/user
cp bin/scree ~/.local/bin/
cp contrib/scree-watch.service ~/.config/systemd/user/
systemctl --user daemon-reload && systemctl --user enable --now scree-watch
journalctl --user -u scree-watch -f
```

The unit runs `scree run watch -notes ~/notes`, so it reads the settings file the same way `scree mcp` does (not through systemd's `EnvironmentFile`). Edit `-notes` in `ExecStart` for another folder. It waits for the writer lock if a Claude Code session in that folder already holds it.

### Docker or Podman

The image is about 18 MB. It contains only the static binary and runs as an unprivileged user. A folder mounts read-only, and the indexes live in a volume at `/data`. On Linux, file-change events reach the container through the bind mount. On macOS, Docker Desktop doesn't pass them through, so `watch` misses edits there. Use the native binary or a systemd unit day to day.

**SELinux hosts (Fedora, RHEL, CentOS Stream) need `:z` on every bind mount.** Without it, the container can't read the mounted folder and scree fails with `open /notes: permission denied`. The examples below include `:z`, and `scree mcp -docker` needs `SCREE_MOUNT_OPTS=ro,z` in the settings file. `:z` **permanently relabels every file in the folder on the host** (to `container_file_t`), even when the mount is read-only. Mount a notes folder or a repo, never your home directory. On systems without SELinux, drop `:z`. If you'd rather not relabel anything, use the native binary instead.

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

For Claude Code, the MCP server can run one container per session over stdio. `scree mcp -docker` loads the settings file and runs `docker run -i --rm --pull=never` (a missing image is never pulled from a registry, since the container gets the key) with the project folder mounted read-only **at its own path** (so each repo gets its own index in the `scree-data` volume), the `scree-data` volume, and only the settings that are set, passed by name (`-e NAME`) so the key never appears on the command line. It mounts the git root of its working directory, under the same rules as `serve` (only through a `.git` you own that is not a symlink, never in or above a sticky folder such as `/tmp`), so `.gitignore` applies, and refuses to start if `CLAUDE_PROJECT_DIR` is in another work tree. `serve` runs in the mounted folder (`-w`), where it applies the same folder rule and the `.mcp.json` opt-in check. Your global git excludes file is not visible inside the container, and a worktree whose git dir lies outside the mounted folder can't be read there.

The repo's committed `.mcp.json` names `scree mcp`, which everyone shares. To use Docker yourself, add a local-scope entry, which takes precedence over the project's for you:

```sh
claude mcp add scree --scope local -- "$HOME/.local/bin/scree" mcp -docker
```

The docker CLI gets an environment of `HOME`, a `PATH` of `/usr/local/bin:/usr/bin:/bin:/opt/homebrew/bin`, the settings, and `DOCKER_HOST` or `DOCKER_CONTEXT` if the settings file sets them; nothing from Claude Code's or the repo's environment, so set those two in the settings file if your engine needs them. `SCREE_DOCKER` (`docker` or `podman` only, found in those folders, never through `PATH`), `SCREE_IMAGE` (default `scree:latest`; a qualified `ghcr.io/jrapakko/scree:<version>` you pulled yourself also works) and `SCREE_DATA_VOLUME` (a volume name, not a path) in the settings file change the engine, image and volume. `SCREE_MOUNT_OPTS` (default `ro`) sets the mount options and accepts only `ro`, `ro,z` or `ro,Z`; on SELinux hosts set it to `ro,z`, as explained above. It never mounts your home directory (from the password database), a folder above it, or `/`, and refuses paths containing `:`. Like `scree mcp`, it ignores inherited settings and takes no other arguments.

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
- The index file, its `-wal` and `-shm` files and `writer.lock` are never opened through a symlink, and an index folder (or the `scree/indexes` folder above it) that is a symlink is refused.
- One policy decides what may be read, and indexing, search, `read_doc` and `reindex` all go through it. Symlinks are followed only inside the indexed folder, and the rules are checked on their target too. Symlinked folders are not descended.

Idle memory is about 13–15 MB per process for a small index, and about 20 MB with an index of 1000 files (4000 chunks). Indexing costs about $0.0025 per MB of markdown with the default model; `doctor`'s estimate is an upper bound.

## Migrating from the scree-mcp wrappers

The shell wrappers `scree-mcp`, `scree-run` and `scree-mcp-docker` are gone; `scree mcp`, `scree run` and `scree mcp -docker` do their work inside the binary. The settings file is unchanged, except that it must not be a symlink and may now hold `DOCKER_HOST` and `DOCKER_CONTEXT`.

1. In each repo, re-register and commit the new `.mcp.json`:

   ```sh
   claude mcp remove scree --scope project
   claude mcp add scree --scope project -- scree mcp
   ```

   `serve` still accepts an old entry naming `scree-mcp`, but that entry only works while a `scree-mcp` program exists, and none is installed any more.
2. Delete the old wrappers: `rm -f ~/.local/bin/scree-mcp ~/.local/bin/scree-run`. If you installed `scree-mcp-docker` as `scree-mcp`, remove it too and add the [local Docker entry](#docker-or-podman).
3. Use `scree run <command>` where you used `scree-run <command>`.
4. Update the systemd unit, which now runs `scree run watch` instead of reading the file through `EnvironmentFile`: `cp contrib/scree-watch.service ~/.config/systemd/user/ && systemctl --user daemon-reload && systemctl --user restart scree-watch`.

## Migrating from the single-folder version

The single-folder version read `SCREE_NOTES` and kept one index at `~/.local/share/scree/index.db`. Now each folder has its own index, and the folder comes from the repo or `-notes`.

1. Remove the `SCREE_NOTES=` line from `~/.config/scree/env`. It is ignored, and `doctor` warns while it is there.
2. Remove a user-scope registration: `claude mcp remove scree --scope user`. It would start scree in every project you open; `serve` then refuses each one whose `.mcp.json` doesn't name `scree mcp`. Register scree per repo instead ([Quick start](#quick-start) step 5), and use the notes folder [from the command line](#a-notes-folder).
3. Rebuild the notes index in its new place with `scree run reindex -notes ~/notes`, then delete `~/.local/share/scree/index.db`. Or keep it with `-db ~/.local/share/scree/index.db`, which adopts it for that folder. `scree indexes` points out the old file.
4. Update the systemd unit: `cp contrib/scree-watch.service ~/.config/systemd/user/ && systemctl --user daemon-reload && systemctl --user restart scree-watch`.
5. Docker: the image no longer sets `SCREE_NOTES` or `SCREE_DB`; pass `-notes /notes`. The old `/data/index.db` in the `scree-data` volume is unused; reindex.

An index from an earlier per-folder build is upgraded in place when it is opened: line ranges are filled in on the next sync without re-embedding unchanged files, and the duplicate hashes and keyword index are built from the stored chunks. An index built before the current chunk size limits needs `scree reindex -all` once.

## License

MIT; see [LICENSE](LICENSE). Release archives and the container image include the licenses of the bundled dependencies (`THIRD_PARTY_LICENSES/`, or `/usr/share/doc/scree` in the image).
