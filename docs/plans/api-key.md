# Plan: easier API key for direct CLI use

Agreed with the user, 2026-10-30. Not started. Runs after the human-view change
(docs/plans/human-view.md) clears red-team, since both touch cmd/scree/main.go and
launch.go.

Problem: direct commands (`scree search`, `scree doctor`, …) read only the
`OPENROUTER_API_KEY` environment variable; only `scree run`, `scree mcp` and the systemd
unit read `~/.config/scree/env`. So a key saved during setup does not work for a plain
`scree search`, which errors in `auto` unless `-mode keyword` is given.

1. **Direct commands read the settings file** when `OPENROUTER_API_KEY` is unset, from
   `<passwd home>/.config/scree/env` with the same checks and parser `scree run` uses
   (regular file, not a symlink, owned by the user, no group/other bits; data only,
   allowed names only). The environment variable wins when set. `serve` under Claude
   Code keeps today's behaviour (it is started by `scree mcp` with the settings already
   applied).
2. **`scree key`**: prompt for the key with no echo (`golang.org/x/term` ReadPassword,
   already a dependency), write or update `OPENROUTER_API_KEY` in the settings file with
   mode 600 while keeping its other lines, create `~/.config/scree` if needed, then make
   one test embedding call and report the result. Replaces the README's `stty` snippet.
3. **`auto` falls back to keyword without a key**: search runs keyword mode and ends with
   `[mode: auto → keyword: no API key; run scree key]`. Explicit `-mode semantic` or
   `-mode hybrid` without a key still errors.

Update SPEC.md (Architecture launcher paragraph, Tools and commands, CLI) and README
(Quick start step 2, Settings file).
