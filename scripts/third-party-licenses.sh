#!/bin/sh
# Copies the license and notice files of every module compiled into
# scree into a folder, so release archives and images can ship them.
#
#   scripts/third-party-licenses.sh [out-dir]   (default: THIRD_PARTY_LICENSES)
set -eu
out=${1:-THIRD_PARTY_LICENSES}
rm -rf "$out"
mkdir -p "$out"
missing=0
# The Go standard library and runtime are compiled in too.
mkdir -p "$out/go"
golicense=
for f in "$(go env GOROOT)/LICENSE" /usr/share/licenses/golang/LICENSE; do
	if [ -f "$f" ]; then golicense=$f; break; fi
done
if [ -n "$golicense" ]; then
	cp "$golicense" "$out/go/LICENSE"
	[ -f "$(dirname "$golicense")/PATENTS" ] && cp "$(dirname "$golicense")/PATENTS" "$out/go/"
else
	echo "no license file found for the Go standard library" >&2
	missing=1
fi
for mod in $(go list -deps -f '{{if and (not .Standard) .Module}}{{.Module.Path}}@{{.Module.Version}}{{end}}' ./cmd/scree | sort -u); do
	path=${mod%@*}
	[ "$path" = github.com/jrapakko/scree ] && continue
	dir=$(go list -m -f '{{.Dir}}' "$mod")
	found=0
	for f in "$dir"/LICENSE* "$dir"/LICENCE* "$dir"/COPYING* "$dir"/NOTICE*; do
		[ -f "$f" ] || continue
		mkdir -p "$out/$path"
		cp "$f" "$out/$path/"
		found=1
	done
	if [ "$found" = 0 ]; then
		echo "no license file found for $mod" >&2
		missing=1
	fi
done
exit "$missing"
