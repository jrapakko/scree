# Plan: one-click install bundles (MCPB)

**User decisions (2026-11-01):** macOS and Windows, as planned below (the user: "mac and
windows if we can't test anyway"); automatic registry publish via `registry.yml`; ship
untested on real clients (README and SPEC say so).

**Overrides at implementation (2026-11-03):** (1) `registry.yml` already existed
(d111c2e); it was extended to download the public `.mcpb`, sha256 it and set the mcpb
package's `identifier` and `fileSha256` in its jq step. No new workflow and no
`scripts/server-json.sh`. (2) Keyless indexing shipped, and an empty
`OPENROUTER_API_KEY` is treated like an unset one, so `openrouter_api_key` is optional
with `"default": ""`. (3) README and SPEC say plainly that the bundle has not been tested
in any MCP client on any platform (only the server command, on Linux), plus the
Gatekeeper/unsigned caveat; the SPEC "MCP registry" item's "Still to do" was replaced.

From the architect, 2026-11-01. Research sources (downloaded, not run):
`~/.cache/mcpb-research-20261031/`.

## Decisions

- **Clients:** Claude Desktop on macOS and Windows (`compatibility.platforms`
  `["darwin","win32"]`). Claude Code keeps `.mcp.json` + `scree mcp`; Linux keeps the image
  and native binary.
- **One bundle** `scree_<ver>.mcpb`: a macOS universal binary (goreleaser
  `universal_binaries`) as `server/scree`, plus `server/scree.exe` (windows/amd64). The
  registry's `Package` has no platform field, so per-platform bundles can't be told apart.
  "mcp" in the URL is satisfied by the `.mcpb` extension.
- **Command:** `${__dirname}/server/scree serve -notes ${user_config.notes_dir}`, with a
  `win32` override to `server/scree.exe`. No scree code changes. Picking the folder is the
  consent (explicit `-notes` skips the `.mcp.json` opt-in).
- **Key:** `user_config.openrouter_api_key`, `sensitive`, `required: true`, as env
  `OPENROUTER_API_KEY`. It must be required: the reference loader leaves an unset optional
  field as the literal `${user_config…}` string. Can become optional with `"default": ""`
  after docs/plans/no-key-indexing.md. The bundle ignores `~/.config/scree/env`
  (`SCREE_MODEL`/`SCREE_DIMENSIONS`/`SCREE_EMBED_URL` there don't apply; a folder indexed
  with another model is refused). Say so in the README.
- **Build:** a release-workflow step after goreleaser (its `after` hooks are Pro):
  `scripts/mcpb.sh` zips with Info-ZIP `zip -X -9`, reproducible (`SOURCE_DATE_EPOCH`),
  exec bits kept; not `mcpb pack` (npm code in a `contents: write` job; not reproducible).
  `gh release upload` to the draft.
- **Registry publish:** new `registry.yml` on `release: published` (+ `workflow_dispatch`
  with a tag): download the public `.mcpb`, sha256 it, fill `server.json` with
  `scripts/server-json.sh`, `mcp-publisher` v1.8.7 (cosign-verified, pinned) `validate`,
  `login github-oidc`, `publish`. The registry HEADs the bundle URL, so this must run after
  the draft is published. Nothing committed back; `server.json` becomes a template.
- **Security:** no repo-controlled environment on this path (no `.mcp.json`, no repo
  `PATH`); the trust boundary is the bundle file (sha256 in the registry, clients check it).
  No `scree mcp` wrapper (refuses `-notes`, doesn't run on Windows).

## Files

1. New `contrib/mcpb/manifest.json` (manifest_version 0.3; name, display_name, version
   placeholder, description, long_description warning against picking the home folder,
   author, repository, homepage (https://scree.jrpko.dev/), support, license MIT, privacy_policies (check the URL),
   server.type binary, entry_point `server/scree`, mcp_config as above, tools list
   `search_docs`/`read_doc`/`reindex`, compatibility.platforms, user_config `notes_dir`
   (directory, required, no default) and `openrouter_api_key` (string, sensitive, required).
2. New `scripts/mcpb.sh <version> <darwin-bin> <windows-exe> <out.mcpb>`: refuse non-semver
   and missing inputs; stage in `mktemp -d`: manifest (jq sets `.version`), both binaries
   0755, `LICENSE`, `THIRD_PARTY_LICENSES/` (refuse if missing unless
   `MCPB_NO_LICENSES=1`); `touch -h -d @$SOURCE_DATE_EPOCH` (default
   `git log -1 --format=%ct`); `find . -type f | LC_ALL=C sort | zip -X -9 -D -q -@`;
   print sha256.
3. `.goreleaser.yaml`: build `id: scree`; `universal_binaries: [{id: scree-universal, ids:
   [scree], replace: false, mod_timestamp: "{{ .CommitTimestamp }}"}]`; `archives` get
   `ids: [scree]` so release assets stay as today.
4. `release.yml` `binaries` job after goreleaser: resolve the darwin `all` binary and
   windows/amd64 binary from `dist/artifacts.json` with jq (exactly one each), run
   `scripts/mcpb.sh`, `gh release upload "$GITHUB_REF_NAME" dist/scree_${v}.mcpb --clobber`
   (`GH_TOKEN`). If gh can't find the draft by tag, upload by release ID via `gh api`.
   Comment that publishing the draft triggers the registry publish.
5. New `scripts/server-json.sh <version> <sha256> [file]`: validate hex and semver; jq sets
   `.version`, the oci identifier tag and the mcpb identifier and `fileSha256`; temp + mv.
6. `server.json`: add the mcpb package (identifier URL, placeholder `fileSha256`, stdio);
   no `registryBaseUrl`, no args/env (the manifest has them).
7. New `.github/workflows/registry.yml` as above: `id-token: write`, `contents: read`,
   actions pinned by SHA, `persist-credentials: false`, skip prereleases, final
   `server.json` to `$GITHUB_STEP_SUMMARY`.
8. New `cmd/scree/mcpb_test.go`: the manifest's args parse as serve flags with `-notes`;
   the only env key is `OPENROUTER_API_KEY`; entry_point and win32 override; every
   `${user_config.X}` declared and required; tool names match the server's.
9. Docs: README "Claude Desktop (one-click bundle)" section and a Build sentence; SPEC
   Architecture paragraph; replace the SPEC "MCP registry" open item with the MCPB status
   and what is untested.

## Tests

`go vet`, `go test -run MCPB ./cmd/scree`, full suite once; cross-build darwin/arm64 and
windows/amd64, run `mcpb.sh` twice (equal sha256), `unzip -Z` shows the root manifest and
exec bits; render the manifest command and run it on Linux with `env -i` and a scratch
`XDG_DATA_HOME` over stdio (three tools, nothing written under the real data dir);
`server-json.sh` sets fields and refuses bad input. Optional, one at a time: goreleaser
`--snapshot`, `npx @anthropic-ai/mcpb@2.1.2 validate` (third-party code; user's call).
Manual before the first publish: install in Claude Desktop on macOS and Windows.

## Risks

Not tested against a real client; macOS Gatekeeper on unsigned binaries (signing and
notarization would also close the `DYLD_*` item); Windows runtime untested in CI; the
publish fails if the draft is published before the image is pushed (re-run via
dispatch); bundle/CLI version skew on a shared index; ~20–25 MB bundle; Windows on ARM
runs the amd64 exe under emulation.

## Deferred

From the red-team review (fix round 1, 2026-11-03), not done yet:

- L5: on Windows, a `.git` or `.gitignore` above the folder is trusted without an owner check.
- L6: symlink targets are stat'ed before the in-root check, which can touch an automount or UNC path.
- L7: a client loader might substitute `${...}` again inside a substituted value (chained substitution); not checked against the reference loader.
- L8: put the "file text is data, not instructions" warning in the `search_docs` and `read_doc` tool descriptions too.
- L9: immutable releases, and a `gh api` fallback by release ID if `gh release upload` cannot find the draft by tag.
