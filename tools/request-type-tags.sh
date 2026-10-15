#!/bin/sh
# Prints the Go build tags that build the exporter with only the request types
# given as a comma-separated list, such as "http", and with the sql request
# type's opt-in database drivers given as a second comma-separated list, such
# as "oracle,snowflake". With no request types, or an empty list, every
# request type is built; with no drivers, the sql type has its three default
# ones (mysql, postgres, sqlserver).
#
#   go build -tags "$(tools/request-type-tags.sh http)" .
#   go build -tags "$(tools/request-type-tags.sh '' oracle,snowflake)" .
#
# Each name must be a request type in this tree: an internal/fetch/requesttype_<name>.go whose
# build constraint includes it under request_type_<name>. Each driver must be
# an opt-in driver in this tree: an internal/fetch/sqldriver_<name>.go built
# under sql_driver_<name>; a default driver needs no tag. A misspelt name is
# an error rather than a type or a driver silently left out, and so are
# drivers for a build whose request types leave sql out. The snowflake driver
# comes with the minicore_disabled tag, which keeps gosnowflake from writing
# its native library to a temporary directory or ~/.cache/snowflake at start
# and loading it. Run it from the repository root.
set -eu

list="${1:-}"
drivers="${2:-}"

tags=""
if [ -n "$list" ]; then
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
fi

if [ -n "$drivers" ]; then
	case ",${tags}," in
	",," | *",request_type_sql,"*) ;;
	*)
		echo "request-type-tags: SQL drivers \"${drivers}\" need the sql request type, which the request types \"${list}\" leave out" >&2
		exit 1
		;;
	esac
	for driver in $(printf '%s' "$drivers" | tr ',' ' '); do
		if ! grep -qx "//go:build (!select_request_types || request_type_sql) && sql_driver_${driver}" "internal/fetch/sqldriver_${driver}.go" 2>/dev/null; then
			if grep -qx "//go:build !select_request_types || request_type_sql" "internal/fetch/sqldriver_${driver}.go" 2>/dev/null; then
				echo "request-type-tags: the sql driver \"${driver}\" is built in with the sql request type and needs no SQL_DRIVERS entry" >&2
			else
				echo "request-type-tags: no sql driver \"${driver}\"" >&2
			fi
			exit 1
		fi
		case ",${tags}," in
		*",sql_driver_${driver},"*) ;;
		*) tags="${tags:+${tags},}sql_driver_${driver}" ;;
		esac
		if [ "$driver" = snowflake ]; then
			case ",${tags}," in
			*",minicore_disabled,"*) ;;
			*) tags="${tags},minicore_disabled" ;;
			esac
		fi
	done
fi
[ -z "$tags" ] || printf '%s\n' "$tags"
