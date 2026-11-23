#!/bin/sh
# Makes third_party/gojq the gojq release named, with gojq.patch applied:
# the steps of third_party/gojq/PATCH.md, which says why there is a copy.
#
#   sh tools/gojq-copy.sh v0.12.21
#
# It needs the network, for the release, and git, to apply the patch. What it
# leaves is for a commit after the tests of internal/expr and
# internal/transform have run: the copy's sources, UPSTREAM.sha256 with the
# release's checksums, gojq.patch against the release, and go.mod and go.sum
# requiring it. A patch that no longer applies stops it, with the copy as the
# release has it, for the patch to be made again by hand.
set -eu

version=${1:?usage: sh tools/gojq-copy.sh <version>, such as v0.12.21}
copy=third_party/gojq
cd "$(dirname "$0")/.."

# sum256 prints a file's SHA-256, with whichever of the two tools is there.
sum256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}

released=$(go mod download -json "github.com/itchyny/gojq@$version" | sed -n 's/^[[:space:]]*"Dir": "\(.*\)",\{0,1\}$/\1/p')
[ -d "$released" ] || { echo "gojq $version was not downloaded" >&2; exit 1; }

# The copy's own files stay; gojq's go, and the release's come.
for file in "$copy"/*.go "$copy"/LICENSE; do
	case "$(basename "$file")" in maxvalue.go) ;; *) rm -f "$file" ;; esac
done
{
	echo "# gojq $version: the SHA-256 of each of its Go sources and its licence as upstream released them,"
	echo "# which the copy here is held to with gojq.patch taken out (PATCH.md). builtin.go is the one"
	echo "# as gofmt writes it, since the repository's formatting check takes no other."
} > "$copy/UPSTREAM.sha256"
for file in "$released"/LICENSE "$released"/*.go; do
	name=$(basename "$file")
	case "$name" in *_test.go) continue ;; esac
	cp "$file" "$copy/$name"
	chmod u+w "$copy/$name"
	[ "$name" = builtin.go ] && gofmt -w "$copy/$name"
	echo "$(sum256 "$copy/$name")  $name" >> "$copy/UPSTREAM.sha256"
done

# The patch, and the patch again as it is against this release.
before=$(mktemp -d)
trap 'rm -rf "$before"' EXIT
patched=$(sed -n 's/^+++ b\///p' "$copy/gojq.patch")
for name in $patched; do cp "$copy/$name" "$before/$name"; done
git apply --directory="$copy" "$copy/gojq.patch"
: > "$copy/gojq.patch"
for name in $patched; do
	diff -u --label "a/$name" --label "b/$name" "$before/$name" "$copy/$name" >> "$copy/gojq.patch" || true
done

go mod edit -require="github.com/itchyny/gojq@$version"
go mod tidy
echo "third_party/gojq is gojq $version with gojq.patch applied."
echo "Check that third_party/gojq/go.mod requires what the release's go.mod does of the modules the sources import,"
echo "then run: go test ./internal/expr/ ./internal/transform/ ./test/repository/"
