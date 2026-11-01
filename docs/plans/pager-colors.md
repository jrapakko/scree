# Plan: page the text view and colour the matches

Agreed with the user, 2026-10-31. Runs after docs/plans/api-key.md, since both touch
cmd/scree.

User's ask: keep the full `>>` range, but show results in a more-style pager so they start
at the top, and colour the match text, with the colours configurable.

1. **Pager.** When `-format text` writes to a terminal, pipe it through a pager:
   `SCREE_PAGER` if set (from the environment or the settings file), else `less` found by
   absolute path (`/usr/bin/less`, `/bin/less`, `/usr/local/bin/less`, never `PATH`) with
   `-FRX` (quit if one screen, pass colour and OSC 8 links, leave the text on screen).
   No pager when stdout is not a terminal, with `-no-pager`, `SCREE_PAGER=` set empty, or
   when no pager is found. `SCREE_PAGER` is split on spaces and run directly, never
   through a shell. Check the installed `less` passes OSC 8 links with `-R`; if not, say
   so in the docs. A pager that exits early (`q`) must not make scree report an error
   (EPIPE).
2. **Colours.** On a terminal with `NO_COLOR` unset and `TERM` not `dumb`, colour the
   matched word in the excerpt and in the `>>` lines, and give the link line and line
   numbers their own colours. `SCREE_COLORS` sets them as SGR codes, for example
   `match=1;33:link=36:lineno=2:word=1` (unknown keys or bad codes are an error naming
   the key; `match=` turns one off). Only digits and `;` are accepted in a code, so
   config can't inject other escapes.
3. `scree run` passes `SCREE_PAGER` and `SCREE_COLORS` like other `SCREE_*` settings
   (they are already allowed in the settings file); check that holds.
4. Docs: SPEC (`-format text` paragraph, settings), README (settings table, Results).
5. Tests: colour codes only around matches and only on a TTY; `NO_COLOR` gives none;
   `SCREE_COLORS` parsing and rejection; the pager is chosen by absolute path and skipped
   in pipes; early pager exit is not an error.
