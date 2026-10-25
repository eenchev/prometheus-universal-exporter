#!/bin/sh
# What `make run` uses to run a shipped example: the example names, the files
# of one, the opt-in request types it needs, and the run itself. Everything is
# read from the tree, so an example added under examples/ can be run without
# anyone listing it.
#
#   sh tools/run-example.sh list                       # name and files, one example a line
#   sh tools/run-example.sh files open-meteo           # its configuration, then its static target file, if it has one
#   sh tools/run-example.sh opt-in-types tcp redis     # tcp,redis: the example's opt-in types and those given
#   sh tools/run-example.sh check-host ::1                # fails for an empty HOST, before make run builds
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

# config_files prints the YAML files of the configuration $1: a directory
# example's collector files are beside it or below.
config_files() {
	case "$1" in
	examples/*/*) find "$(dirname "$1")" -name '*.yaml' -o -name '*.yml' ;;
	*) printf '%s\n' "$1" ;;
	esac
}

# probe_query prints the query of the first probe URL that the comment
# opening the configuration $1 gives as `curl '...://.../probe?QUERY'`, or
# nothing when the comment gives none or names a collector that no
# collector file of the example has, since a stale comment would send the
# reader to a probe that fails.
probe_query() {
	query=$(awk '!/^#/ { exit } match($0, /^#[[:space:]]+curl '\''[a-z]+:\/\/[^\/'\'']+\/probe\?[^'\'']+'\''/) { line = substr($0, RSTART, RLENGTH); sub(/^[^?]*\?/, "", line); sub(/'\''$/, "", line); print line; exit }' "$1")
	collector=$(printf '%s\n' "$query" | tr '&' '\n' | sed -n 's/^collector=//p' | head -n 1)
	[ -n "$collector" ] || return 0
	# shellcheck disable=SC2046 # the file names have no spaces
	if grep -Eq "^[[:space:]]*-?[[:space:]]*name:[[:space:]]*[\"']?${collector}[\"']?[[:space:]]*(#.*)?\$" $(config_files "$1"); then
		printf '%s\n' "$query"
	fi
}

# probe_target_is_local prints yes when the target of the probe query $1 is
# one a local run can reach as the comment gives it: a host of localhost,
# 127.0.0.1 or [::1], or a DNS name with a dot that is no placeholder. A
# comment of an exporter of something the reader runs names it by a made-up
# host, such as filebeat-host; the URL would only send them to a probe that
# fails. A placeholder is a name without a dot, one whose first label ends in
# -host, an IP address but the loopback ones (an address is of one network),
# or a name under example.com, example.net, example.org, or the top-level
# domains example, invalid, test, local, localhost, internal, lan, home, corp
# or svc (cluster.local is under local).
probe_target_is_local() {
	target=$(printf '%s\n' "$1" | tr '&' '\n' | sed -n 's/^target=//p' | head -n 1)
	case "$target" in *://*) target=${target#*://} ;; esac
	target=${target%%/*}
	target=${target##*@}
	case "$target" in
	\[*) target=${target%%]*}] ;;
	*) target=${target%%:*} ;;
	esac
	target=$(printf '%s' "$target" | tr 'A-Z' 'a-z')
	target=${target%.}
	case "$target" in
	localhost | 127.0.0.1 | '[::1]') echo yes; return 0 ;;
	*.*) ;;
	*) return 0 ;;
	esac
	case "$target" in
	*[!0-9.]*) ;;
	*) return 0 ;;
	esac
	first=${target%%.*}
	case "$first" in *-host) return 0 ;; esac
	case "$target" in
	example.com | *.example.com | example.net | *.example.net | example.org | *.example.org) return 0 ;;
	*.example | *.invalid | *.test | *.local | *.localhost | *.internal | *.lan | *.home | *.corp | *.svc) return 0 ;;
	esac
	echo yes
}

# listen_host prints the address part of --web.listen-address for HOST $1: a
# bare IPv6 address in brackets, as Go wants it ([::1]:8080, not ::1:8080),
# one already in brackets as it is, anything else as it is. An empty HOST is
# refused: the URLs printed would have no host, and the exporter would listen
# on every interface, which an example run should not do unasked.
listen_host() {
	case "$1" in
	'')
		echo "run-example: HOST is empty; set it to the address to listen on, such as HOST=127.0.0.1, HOST=::1, or HOST=0.0.0.0 for every interface" >&2
		exit 2
		;;
	\[*\]) printf '%s\n' "$1" ;;
	*:*) printf '[%s]\n' "$1" ;;
	*) printf '%s\n' "$1" ;;
	esac
}

# url_host prints the host of the URLs to open for the listen address host
# $1: a browser on the same machine cannot open 0.0.0.0 or [::], the
# addresses of every interface, so those are opened as localhost and [::1].
url_host() {
	case "$1" in
	0.0.0.0) echo localhost ;;
	'[::]') echo '[::1]' ;;
	*) printf '%s\n' "$1" ;;
	esac
}

case "${1:-}" in
check-host)
	listen_host "${2:-}" >/dev/null
	;;
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
	files=$(config_files "$config")
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
	listen=$(listen_host "$3")
	host=$(url_host "$listen")
	port=$4
	shift 4
	config=$(printf '%s\n' "$line" | cut -f2)
	targets=$(printf '%s\n' "$line" | cut -f3)
	if [ "$host" != "$listen" ]; then
		echo "The exporter listens on every interface, $listen:$port; on this machine it is http://$host:$port/."
	fi
	echo "Open http://$host:$port/ for the landing page and its collectors."
	# The status page is served for every example: with static targets it
	# leads with their last scrapes, and its recent probes fill as soon as
	# anything probes.
	if [ -n "$targets" ]; then
		echo "Open http://$host:$port/static-targets/status for the static targets' last scrapes and the recent probes."
		set -- "--static-targets-file=$targets" "$@"
	else
		echo "Open http://$host:$port/static-targets/status for the recent probes."
	fi
	query=$(probe_query "$config")
	if [ -n "$query" ] && [ -n "$(probe_target_is_local "$query")" ]; then
		echo "Open http://$host:$port/probe?$query for a probe."
	elif [ -n "$query" ]; then
		# The comment's target is a placeholder: the query with the
		# reader's own target in its place.
		hint=$(printf '%s\n' "$query" | tr '&' '\n' | sed 's/^target=.*/target=<your target>/' | paste -s -d '&' -)
		placeholder=$(printf '%s\n' "$query" | tr '&' '\n' | sed -n 's/^target=//p' | head -n 1)
		echo "Probe it with http://$host:$port/probe?$hint: the example's target, $placeholder, is a placeholder for yours."
	fi
	set -- "--config.file=$config" "--web.listen-address=$listen:$port" "$@"
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
	echo "usage: sh tools/run-example.sh check-host HOST | list | files NAME | opt-in-types NAME [TYPES] | run NAME HOST PORT [FLAGS...] | check-root-binaries BINARY..." >&2
	exit 2
	;;
esac
