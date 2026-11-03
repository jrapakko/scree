#!/bin/sh
# Builds the MCPB bundle for Claude Desktop: contrib/mcpb/manifest.json
# with the version set, the macOS (universal) binary as server/scree, the
# Windows binary as server/scree.exe, LICENSE and THIRD_PARTY_LICENSES/.
# The zip is reproducible: fixed order, no extra attributes, every
# timestamp SOURCE_DATE_EPOCH (default: the last commit's time). Needs GNU
# touch, Info-ZIP zip and jq.
#
#   scripts/mcpb.sh <version> <darwin-bin> <windows-exe> <out.mcpb>
#
# Set MCPB_NO_LICENSES=1 to build without THIRD_PARTY_LICENSES/ (local
# tests only; releases must ship it).
set -eu
if [ $# -ne 4 ]; then
	echo "usage: scripts/mcpb.sh <version> <darwin-bin> <windows-exe> <out.mcpb>" >&2
	exit 2
fi
version=$1 darwin=$2 windows=$3 out=$4
case $version in
*[!0-9A-Za-z.-]*) version_ok=false ;; # also refuses a newline, which grep would split on
*) version_ok=true ;;
esac
if ! $version_ok || ! printf '%s\n' "$version" | grep -Eqx '[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?'; then
	echo "mcpb: not a semver version: $version" >&2
	exit 1
fi
# Nothing goes in through a symlink, which could pull in files from
# elsewhere. Only the last path component is checked: a symlinked parent
# folder (a dist/ that is a link, say) is not refused, so the inputs are
# trusted to come from this checkout's own build.
for f in "$darwin" "$windows" contrib/mcpb/manifest.json LICENSE; do
	if [ -L "$f" ]; then
		echo "mcpb: $f is a symlink" >&2
		exit 1
	fi
	if [ ! -f "$f" ]; then
		echo "mcpb: missing $f (run from the repo root)" >&2
		exit 1
	fi
done
if [ -L THIRD_PARTY_LICENSES ]; then
	echo "mcpb: THIRD_PARTY_LICENSES is a symlink" >&2
	exit 1
fi
if [ -d THIRD_PARTY_LICENSES ] && [ -n "$(find THIRD_PARTY_LICENSES -type l)" ]; then
	echo "mcpb: THIRD_PARTY_LICENSES holds a symlink" >&2
	exit 1
fi
if [ ! -d THIRD_PARTY_LICENSES ] && [ "${MCPB_NO_LICENSES:-}" != 1 ]; then
	echo "mcpb: missing THIRD_PARTY_LICENSES/ (scripts/third-party-licenses.sh makes it)" >&2
	exit 1
fi
epoch=${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct)}
case $epoch in
'' | *[!0-9]*)
	echo "mcpb: SOURCE_DATE_EPOCH is not a number: $epoch" >&2
	exit 1
	;;
esac
case $out in
/*) ;;
*) out=$PWD/$out ;;
esac

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
stage=$tmp/stage
mkdir -p "$stage/server"
jq --arg v "$version" '.version = $v' contrib/mcpb/manifest.json >"$stage/manifest.json"
cp "$darwin" "$stage/server/scree"
cp "$windows" "$stage/server/scree.exe"
cp LICENSE "$stage/LICENSE"
if [ -d THIRD_PARTY_LICENSES ]; then
	cp -R THIRD_PARTY_LICENSES "$stage/THIRD_PARTY_LICENSES"
fi
find "$stage" -type d -exec chmod 755 {} +
find "$stage" -type f -exec chmod 644 {} +
chmod 755 "$stage/server/scree" "$stage/server/scree.exe"
find "$stage" -exec touch -h -d "@$epoch" {} +
# Zip stores local time; UTC keeps it the same on every machine.
(cd "$stage" && find . -type f | LC_ALL=C sort | TZ=UTC zip -X -9 -D -q -@ "$tmp/out.mcpb")
mv "$tmp/out.mcpb" "$out"
sha256sum "$out"
