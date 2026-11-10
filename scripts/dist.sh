#!/bin/sh
# Copyright 2026 Furkan Anıl Öztaş
# SPDX-License-Identifier: Apache-2.0

set -eu
umask 022
export GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 GOWORK=off
name=ahenk-darwin-amd64

# Stage beside the final archive so a failed build cannot replace it.
test ! -L dist
test ! -L "dist/$name.tar.gz"
mkdir -p dist
work=$(mktemp -d "dist/.$name.XXXXXXXX")
trap 'rm -rf "$work"' EXIT
trap 'exit 1' HUP INT TERM
release="$work/$name"
mkdir -p "$release"
cp LICENSE "$release/LICENSE"
cp docs/third-party-licenses.md "$release/THIRD_PARTY.md"

go build -mod=readonly -trimpath -buildvcs=false -o "$release/ahenk" ./cmd/ahenk

goroot=$(go env GOROOT)
format='{{if .Standard}}go|{{.Dir}}{{else}}{{with .Module}}{{if not .Main}}{{if .Replace}}replacement|{{else}}{{.Path}}@{{.Version}}|{{.Dir}}|{{end}}{{end}}{{end}}{{.Dir}}{{end}}'
go list -mod=readonly -deps -f "$format" ./cmd/ahenk > "$work/packages"
sort -u "$work/packages" > "$work/unique-packages"

# Preserve root and package-level legal files, including Go's bundled code.
while IFS='|' read -r component base directory; do
    case "$component" in
        replacement) printf 'Review replaced dependencies before packaging.\n' >&2; exit 1 ;;
        go) directory=$base; base=$goroot ;;
        *@*) ;;
        *) continue ;;
    esac
    if [ ! -f "$base/LICENSE" ]; then
        printf 'Missing upstream LICENSE for %s.\n' "$component" >&2
        exit 1
    fi
    while :; do
        case "$directory" in
            "$base"|"$base"/*) ;;
            *) printf 'Unexpected package directory for %s.\n' "$component" >&2; exit 1 ;;
        esac
        destination="$release/licenses/$component${directory#"$base"}"
        for notice in "$directory"/LICENSE* "$directory"/LICENCE* "$directory"/NOTICE* "$directory"/COPYING* "$directory"/PATENTS*; do
            if [ -f "$notice" ] && [ ! -f "$destination/${notice##*/}" ]; then
                mkdir -p "$destination"
                cp "$notice" "$destination/"
            fi
        done
        [ "$directory" != "$base" ] || break
        directory=${directory%/*}
    done
done < "$work/unique-packages"

# Normalize archive metadata so equivalent inputs produce identical bytes.
find "$release" -type f -exec chmod 0644 {} +
chmod 0755 "$release/ahenk"
TZ=UTC find "$release" -exec touch -t 202601010000 {} +
(
    cd "$work"
    find "$name" -print > files
    LC_ALL=C sort files > sorted-files
    COPYFILE_DISABLE=1 tar -cf archive.tar --format ustar --uid 0 --gid 0 \
        --uname root --gname wheel --no-recursion -T sorted-files
)
gzip -n -c "$work/archive.tar" > "$work/archive.tar.gz"
mv -f "$work/archive.tar.gz" "dist/$name.tar.gz"
printf '%s\n' "dist/$name.tar.gz"
