---
title: Ignoring files
---

An ignored file is never read for indexing, never sent to the embedding API, never returned by search, and refused by `read_doc` and `reindex <path>`. Ignoring is how you keep a private file from leaving the machine. Check what is ignored with `scree-run doctor -v` before you rely on a rule.

## What gets indexed

Files ending in `.md`, `.markdown` or `.mdc` (in any letter case), anywhere in the folder, minus what the four layers below exclude. Nothing else: no code, YAML or JSON.

The layers are checked in this order, and the first that matches wins:

1. built-in rules;
2. git's ignore rules;
3. `.screeignore`;
4. `scree: ignore` front matter.

## 1. Built-in rules

Always on:

- **Hidden paths:** any path segment starting with `.`, such as `.git/`, `.obsidian/`, `.trash/` or `.env.md`. Exceptions are listed below.
- **Not markdown:** any other extension.
- **Symlinks** whose target resolves outside the folder. A symlink inside the folder is followed, and every rule is checked against both the link and its target. Symlinked folders are never descended.
- **Size:** files over `SCREE_MAX_FILE_BYTES` (1 MiB by default), which are usually exports or logs.
- **Unsafe names:** names with control characters or invalid UTF-8.
- **Non-regular files** such as FIFOs and devices, skipped without being opened.
- `.screeignore` itself is never indexed.

### Root hidden folders that are indexed

These folders are searched like any other **when they sit at the root** of the indexed folder: `.claude/`, `.github/`, `.gitlab/`, `.gitea/`, `.forgejo/`, `.cursor/`, `.codex/`, `.gemini/`, `.windsurf/`, `.circleci/` and `.buildkite/`. That covers pull-request templates, `.claude/agents/*.md` and agent rules written in markdown.

- Only the first path segment counts, compared case-sensitively: `docs/.github/` stays hidden, and so does `.GITHUB/`.
- A hidden segment inside them stays hidden: `.github/.drafts/x.md` is ignored.
- `.claude/worktrees/` is always hidden, since Claude Code keeps full copies of the repo there. No directive re-includes it.
- Every other rule still applies inside them, including the extension, so `.github/workflows/ci.yml` is not indexed.

To index another root hidden folder, add `include-hidden .name/` to `.screeignore`. To drop a default, exclude it with an ordinary pattern such as `/.cursor/`.

## 2. Git's ignore rules

When the folder is inside a git work tree, scree applies everything git would ignore, using its own Go matcher (it never runs `git`):

- every `.gitignore` from the work tree's top down to the file, each pattern relative to the folder of its `.gitignore`, a deeper file overriding a shallower one;
- `.git/info/exclude` (in a linked worktree, the one in the common git dir);
- your global excludes file: `core.excludesFile` from `~/.gitconfig` or `$XDG_CONFIG_HOME/git/config`, defaulting to `$XDG_CONFIG_HOME/git/ignore`. `[include]` sections are not followed.

Git's semantics apply. A file inside an excluded folder can't be re-included, so a folder like `node_modules/` is never descended at all. A folder holding its own `.git`, a nested repository or submodule, is skipped and counted separately by `doctor`.

Differences and limits:

- **Tracked files are not special.** A tracked file that matches a pattern is still ignored, since scree does not read git's index. This is the one known divergence from git.
- **The repository's `.git/config` is never read.** Only your own config can name an excludes file.
- **No symlinks, regular files only.** `.gitignore`, `.git/info/exclude` and a `.git` file are never followed through a symlink. Each git ignore file is read up to 256 KB.
- **A `.gitignore` that can't be read excludes its folder.**
- **Trust:** the work tree is used only if its `.git` is owned by you and is not a symlink. For a linked worktree (a `.git` file), the git dir and common dir it names must be folders you own, not symlinks. scree never looks for a `.git` in or above a sticky folder such as `/tmp`. A `.git` owned by someone else is an error, not a silent skip; see [Troubleshooting](troubleshooting.md#untrusted-git). Windows has no ownership check.
- Messages about these files name the file and line number, never their content.

`-gitignore=false` turns this layer off. Under Docker, your global excludes file is not visible in the container (see [Docker](docker.md)).

## 3. `.screeignore`

One file, at the root of the indexed folder. Nested `.screeignore` files are not read. It uses gitignore syntax: one pattern per line, `#` comments, `!` to re-include, a leading `/` anchors to the root, a trailing `/` matches folders only, and `**` matches any depth. It runs after git's rules and can only exclude more.

```
# .screeignore
drafts/
**/private-*.md
/docs/scratch.md
!drafts/published.md
include-hidden .changeset/
```

### How `!` differs from git

In git, nothing inside an excluded folder can be re-included. In `.screeignore` it can: in the example, `drafts/` is excluded but `drafts/published.md` is indexed.

A `!` rule that matches only a folder above the file undoes folder exclusions, but never a rule that matched the file itself:

```
*.private.md
archive/
!archive/2024/
```

Here `archive/2024/notes.md` is indexed again, but `archive/2024/plan.private.md` stays ignored, because `*.private.md` matched the file by name. `doctor -v` lists every file a `!` rule re-included, with the rule's line number.

### Directives and errors

- `include-hidden .name/` adds a root hidden folder to the allowlist. The name is one segment starting with `.`, ending with `/`, with no wildcards; `.git` is refused. Write `\include-hidden` for a literal pattern that starts with those words.
- `doctor` reports bad lines by number only.
- **Fail closed:** if `.screeignore` exists but can't be read, is a symlink, or isn't a regular file, nothing is indexed until it is fixed.

## 4. Front matter

Opt one file out without touching an ignore file:

```
---
scree: ignore
---
```

The front matter must start on the first line with `---` and end with `---` or `...`. The key may be quoted (`"scree"` or `'scree'`) and followed by spaces before the colon; the value `ignore` matches in any case, optionally quoted, optionally followed by a `# comment`. The file has to be read to see this line, but nothing in it is chunked or embedded, and an already indexed copy is removed.

## Names on disk

Every rule is checked against the name as stored on disk, not the spelling a caller passed: `read_doc`, `reindex` and the watcher first map a path to its on-disk name. Names and patterns are compared in Unicode NFC, and case-insensitively where the filesystem is (macOS and Windows by default). The root hidden-folder allowlist is always case-sensitive.

## When rules change

- Editing `.screeignore` or any `.gitignore` triggers a sync: newly ignored files are removed from the index, newly allowed ones indexed.
- Adding `scree: ignore` removes the file on its next save; removing the line re-indexes it.
- Changes to `.git/info/exclude` or the global excludes file are picked up on the next sync: session start, `reindex`, or any ignore-file edit.
- Search re-checks each hit against the current rules, so a newly ignored file drops out of results at once.

## Worked examples

**A docs site with build output committed.** `site/` is generated from `docs/`:

```
# .screeignore
/site/
```

Without this, every section is indexed twice and costs twice.

**A monorepo where only docs matter.** Keep the top-level READMEs, drop package changelogs:

```
# .screeignore
**/CHANGELOG.md
/vendor/
```

`node_modules/` is already excluded if `.gitignore` lists it.

**Private notes inside a repo.** A `notes/` folder that should stay local, except one shared file:

```
# .screeignore
/notes/
!/notes/onboarding.md
```

**Check before indexing:**

```sh
scree-run doctor -v | grep -E 're-included|ignored \(\.screeignore\)'
```

**A dotfiles repo at `~` with `*` in its `.gitignore`.** scree refuses a folder found automatically if it is your home directory, so this repo is never indexed by accident. To index a folder below it without git's rules, use `-notes <folder> -gitignore=false` from the CLI.
