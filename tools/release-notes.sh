#!/bin/sh
# Prints the description of a GitHub Release: the body of CHANGELOG.md's
# section for the release, `## exporter X.Y.Z` or `## chart X.Y.Z`, without
# its heading. The release workflows pass it to gh release with --notes-file.
#
#   sh tools/release-notes.sh exporter 1.8.0
#   sh tools/release-notes.sh chart 1.8.0
#
# A third argument names another changelog, for the tests. A release whose
# section is missing, or does not begin with `### What changed for you` and a
# line under it, fails here, before anything is published: a Release whose
# description does not tell a user what to do on upgrading is worse than one
# that waits for the entry. Run it from the repository root.
set -eu

if [ "$#" -lt 2 ] || [ "$#" -gt 3 ]; then
	echo "usage: release-notes.sh exporter|chart MAJOR.MINOR.PATCH [CHANGELOG]" >&2
	exit 2
fi
component="$1"
version="$2"
changelog="${3:-CHANGELOG.md}"

case "$component" in
exporter | chart) ;;
*)
	echo "release-notes: \"$component\" is neither exporter nor chart" >&2
	exit 2
	;;
esac
if ! printf '%s\n' "$version" | grep -Eqx '[0-9]+\.[0-9]+\.[0-9]+'; then
	echo "release-notes: \"$version\" is not a MAJOR.MINOR.PATCH version" >&2
	exit 2
fi
if [ ! -r "$changelog" ]; then
	echo "release-notes: cannot read $changelog" >&2
	exit 1
fi

heading="## $component $version"
status=0
notes="$(awk -v heading="$heading" '
	$0 == heading { found = 1; inside = 1; next }
	inside && /^## / { inside = 0 }
	inside { lines[++n] = $0 }
	END {
		if (!found) exit 3
		first = 1
		while (first <= n && lines[first] ~ /^[ \t]*$/) first++
		last = n
		while (last >= first && lines[last] ~ /^[ \t]*$/) last--
		# The first subsection is What changed for you, with a line of
		# text before the next subsection or the end of the section.
		opened = 0
		said = 0
		for (i = first; i <= last; i++) {
			if (lines[i] ~ /^### /) {
				if (opened) break
				if (lines[i] != "### What changed for you") exit 4
				opened = 1
			} else if (opened && lines[i] !~ /^[ \t]*$/) {
				said = 1
			}
		}
		if (!said) exit 4
		for (i = first; i <= last; i++) print lines[i]
	}
' "$changelog")" || status=$?

case "$status" in
0) ;;
3)
	echo "release-notes: $changelog has no \"$heading\" section; move the release's lines from \"## Unreleased\" into one (docs/RELEASING.md)" >&2
	exit 1
	;;
4)
	echo "release-notes: \"$heading\" in $changelog does not begin with \"### What changed for you\" and a line under it; write what upgrading changes for a user, or \"Nothing; you can upgrade without changes.\"" >&2
	exit 1
	;;
*)
	echo "release-notes: reading $changelog failed" >&2
	exit 1
	;;
esac
printf '%s\n' "$notes"
