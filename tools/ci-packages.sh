#!/bin/sh
# Prints the packages whose tests one side of CI runs, one import path a line:
# "allocator", the target allocator (cmd/pue-allocator and internal/allocator
# with what is under it), or "exporter", every other package of the module but
# test/repository, the repository's own checks, which CI tests on their own.
# A second argument gives the build tags the packages are listed with, so a
# package a build with those tags compiles nothing of is left out rather than
# named to go test, which would fail on it.
#
#   go test $(sh tools/ci-packages.sh allocator)
#   go test -tags "$tags" $(sh tools/ci-packages.sh exporter "$tags")
#
# A new package lands on the exporter's side unless it is under the
# allocator's directories. Selecting no package is an error, not an empty
# list, which go test would read as the package in the current directory.
# Run it from the repository root.
set -eu

side="${1:-}"
tags="${2:-}"

module="$(go list -m)"
allocator="^${module}/(cmd/pue-allocator|internal/allocator)(/.*)?\$"
repository="^${module}/test/repository\$"
packages="$(go list -tags "$tags" ./...)"

case "$side" in
allocator)
	selected="$(printf '%s\n' "$packages" | grep -E "$allocator" || true)"
	;;
exporter)
	selected="$(printf '%s\n' "$packages" | grep -v -E -e "$allocator" -e "$repository" || true)"
	;;
*)
	echo "ci-packages: the side must be allocator or exporter, not \"${side}\"" >&2
	exit 2
	;;
esac

if [ -z "$selected" ]; then
	echo "ci-packages: no package is on the ${side} side" >&2
	exit 1
fi
printf '%s\n' "$selected"
