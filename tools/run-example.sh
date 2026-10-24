#!/bin/sh
# What `make run` uses to run a shipped example: the example names, the files
# of one, the opt-in request types it needs, and the run itself. Everything is
# read from the tree, so an example added under examples/ can be run without
# anyone listing it.
#
#   sh tools/run-example.sh list                       # name and files, one example a line
#   sh tools/run-example.sh files open-meteo           # its configuration, then its static target file, if it has one
#   sh tools/run-example.sh opt-in-types tcp redis     # tcp,redis: the example's opt-in types and those given
#   sh tools/run-example.sh run open-meteo 127.0.0.1 9115 --log.level=debug
#   sh tools/run-example.sh check-root-binaries prometheus-universal-exporter pue-allocator
#
# An example is a directory under examples/, named by the directory, whose
# configuration is the YAML file with a top-level collectors or
# collector_files key and whose static target file, if any, the one with a
# top-level targets key (the test of the example tree tells them apart the
# same way); or a configuration of its own directly in examples/,
# config.<name>.<anything>.yaml, named by <name>: examples/config.ecb.xml-test.yaml
# is ecb. A name that two examples would share is an error, which a test
# keeps from being shipped.
#
# The opt-in request types an example needs are those of the tree,
# internal/fetch/requesttype_<name>.go built under request_type_<name> alone,
# that a YAML file of the example names on a line `type: <name>`: no decoder,
# transform or metric type shares a name with a request type, so the line is
# a request's. Run it from the repository root.
set -eu

app=prometheus-universal-exporter

# examples prints "name<TAB>configuration<TAB>static target file" for every
# example, the last field empty for one without static targets.
examples() {
	for dir in examples/*/; do
		[ -d "$dir" ] || continue
		dir=${dir%/}
		config=""
		targets=""
		for file in "$dir"/*.yaml "$dir"/*.yml; do
			[ -f "$file" ] || continue
			if grep -Eq '^(collectors|collector_files):' "$file"; then
				if [ -n "$config" ]; then
					echo "run-example: $dir has two configurations, $config and $file" >&2
					exit 1
				fi
				config=$file
			elif grep -Eq '^targets:' "$file"; then
				if [ -n "$targets" ]; then
					echo "run-example: $dir has two static target files, $targets and $file" >&2
					exit 1
				fi
				targets=$file
			fi
		done
		[ -n "$config" ] || continue
		printf '%s\t%s\t%s\n' "${dir#examples/}" "$config" "$targets"
	done
	for file in examples/config.*.yaml examples/config.*.yml; do
		[ -f "$file" ] || continue
		name=${file#examples/config.}
		name=${name%%.*}
		printf '%s\t%s\t\n' "$name" "$file"
	done
}

# example prints the line of the example named $1, failing with the list of
# names for a name no example has.
example() {
	all=$(examples)
	duplicate=$(printf '%s\n' "$all" | cut -f1 | sort | uniq -d)
	if [ -n "$duplicate" ]; then
		echo "run-example: more than one example is named $duplicate" >&2
		exit 1
	fi
	line=$(printf '%s\n' "$all" | awk -F '\t' -v name="$1" '$1 == name')
	if [ -z "$line" ]; then
		echo "run-example: no example \"$1\"; the examples are:" >&2
		printf '%s\n' "$all" | cut -f1 | sort | sed 's/^/  /' >&2
		exit 1
	fi
	printf '%s\n' "$line"
}

case "${1:-}" in
list)
	examples | sort | awk -F '\t' '{ printf "  %-16s %s%s\n", $1, $2, ($3 == "" ? "" : " " $3) }'
	;;
files)
	line=$(example "${2:-}")
	printf '%s\n' "$line" | cut -f2
	targets=$(printf '%s\n' "$line" | cut -f3)
	[ -z "$targets" ] || printf '%s\n' "$targets"
	;;
opt-in-types)
	line=$(example "${2:-}")
	config=$(printf '%s\n' "$line" | cut -f2)
	# A directory example's collector files are YAML beside it or below.
	case "$config" in
	examples/*/*) files=$(find "$(dirname "$config")" -name '*.yaml' -o -name '*.yml') ;;
	*) files=$config ;;
	esac
	types=$(printf '%s' "${3:-}" | tr ',' ' ')
	for source in internal/fetch/requesttype_*.go; do
		name=${source#internal/fetch/requesttype_}
		name=${name%.go}
		case "$name" in *_test) continue ;; esac
		grep -qx "//go:build request_type_${name}" "$source" || continue
		# shellcheck disable=SC2086 # the file names have no spaces
		if grep -Eq "^[[:space:]]+type:[[:space:]]*[\"']?${name}[\"']?[[:space:]]*(#.*)?\$" $files; then
			types="$types $name"
		fi
	done
	out=""
	for type in $types; do
		case ",$out," in
		*",$type,"*) ;;
		*) out="${out:+$out,}$type" ;;
		esac
	done
	printf '%s\n' "$out"
	;;
run)
	if [ $# -lt 4 ]; then
		echo "usage: sh tools/run-example.sh run NAME HOST PORT [FLAGS...]" >&2
		exit 2
	fi
	line=$(example "$2")
	host=$3
	port=$4
	shift 4
	config=$(printf '%s\n' "$line" | cut -f2)
	targets=$(printf '%s\n' "$line" | cut -f3)
	echo "Open http://$host:$port/ for the landing page and its collectors."
	if [ -n "$targets" ]; then
		echo "Open http://$host:$port/static-targets/status for the static targets' last scrapes."
		set -- "--static-targets-file=$targets" "$@"
	fi
	set -- "--config.file=$config" "--web.listen-address=$host:$port" "$@"
	echo "bin/$app $*"
	exec "bin/$app" "$@"
	;;
check-root-binaries)
	# `go build` in the repository root leaves the binary there, where it
	# stays, ignored by git, long after make builds a newer one into bin/.
	shift
	for binary in "$@"; do
		if [ -e "$binary" ]; then
			echo "warning: ./$binary is an old binary from a go build in the repository root; make builds bin/$binary, so run that one, or delete ./$binary" >&2
		fi
	done
	;;
*)
	echo "usage: sh tools/run-example.sh list | files NAME | opt-in-types NAME [TYPES] | run NAME HOST PORT [FLAGS...] | check-root-binaries BINARY..." >&2
	exit 2
	;;
esac
