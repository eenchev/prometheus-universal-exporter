#!/bin/sh
# Prints the Go build tags that build the exporter with only the request types
# given as a comma-separated list, such as "http". With no list, or an empty
# one, it prints nothing, which builds every request type.
#
#   go build -tags "$(tools/request-type-tags.sh http)" .
#
# Each name must be a request type in this tree: an internal/fetch/requesttype_<name>.go whose
# build constraint includes it under request_type_<name>. A misspelt name is an
# error rather than a type silently left out. Run it from the repository root.
set -eu

list="${1:-}"
[ -n "$list" ] || exit 0

tags="select_request_types"
for type in $(printf '%s' "$list" | tr ',' ' '); do
	if ! grep -qx "//go:build !select_request_types || request_type_${type}" "internal/fetch/requesttype_${type}.go" 2>/dev/null; then
		echo "request-type-tags: no request type \"${type}\"" >&2
		exit 1
	fi
	case ",${tags}," in
	*",request_type_${type},"*) ;;
	*) tags="${tags},request_type_${type}" ;;
	esac
done
printf '%s\n' "$tags"
