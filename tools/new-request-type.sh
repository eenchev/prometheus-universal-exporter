#!/bin/sh
# Adds a request type to the tree: the files of the type, with a Requester
# that compiles and answers "not implemented" until its logic is written,
# and the wiring that names every type, so that a new type is this command
# and then its logic, its documentation and its tests.
#
#   sh tools/new-request-type.sh <name>                  # a type of every build
#   sh tools/new-request-type.sh <name> opt-in           # an opt-in type
#   sh tools/new-request-type.sh <name> default SSHExec  # its Go name given
#
# make new-request-type NAME=<name> [OPT_IN=1] [GO_NAME=<GoName>] runs it.
# The name is lowercase letters and digits, starting with a letter, and no
# type of the tree already. The Go name is how the type's identifiers spell
# it, RequestType<GoName> and <GoName>Config: the name with its first letter
# in capitals when it is not given, and otherwise the name with the letters
# of an initialism in capitals, as Go writes them (SSHExec for sshexec), the
# same letters in the same order. It writes
# internal/fetch/requesttype_<name>.go, its tests in internal/fetch and
# internal/exporter, its table of schema rows in test/repository and
# docs/<NAME>.md; adds the type to the lists of
# internal/fetch/requesttype.go, the guard of requesttype_none.go, the loops
# and the opt-in lists of the Makefile and .github/workflows/ci.yml, the
# lists the documentation marks <!-- request-types -->, the indexes of the
# README and docs/CONFIGURATION.md, spec § 35's list of pages, the tables of
# the repository tests that name every type and the pages whose static
# targets they read; runs make schemas; and prints what is left to write by
# hand. Each place it writes that a person has to finish says TODO(<name>).
#
# It is POSIX sh with POSIX sed and awk, as macOS has them, and needs go,
# gofmt and make. Run it from the repository root.
set -eu

name="${1:-}"
mode="${2:-}"
goName="${3:-}"

fail() {
	echo "new-request-type: $*" >&2
	exit 1
}

case "$mode" in
"" | default) optin="" ;;
opt-in | 1) optin=1 ;;
*) fail "the second argument is \"opt-in\" or nothing, not \"$mode\"" ;;
esac

case "$name" in
"") fail "usage: sh tools/new-request-type.sh <name> [opt-in]" ;;
*[!a-z0-9]*) fail "\"$name\" is not a request type's name: lowercase letters and digits only" ;;
[!a-z]*) fail "\"$name\" is not a request type's name: it starts with a letter" ;;
none) fail "\"none\" is the name of the guard of a build without types, internal/fetch/requesttype_none.go" ;;
esac

[ -f internal/fetch/requesttype.go ] && [ -f tools/request-type-tags.sh ] || fail "run it from the repository root"
[ ! -e "internal/fetch/requesttype_${name}.go" ] || fail "the request type \"$name\" is in the tree already: internal/fetch/requesttype_${name}.go"

first=$(printf '%s' "$name" | cut -c1 | tr 'a-z' 'A-Z')
rest=$(printf '%s' "$name" | cut -c2-)
Title="${first}${rest}"
if [ -n "$goName" ]; then
	case "$goName" in
	[A-Z]*) ;;
	*) fail "the Go name \"$goName\" does not start with a capital letter, as an exported identifier does" ;;
	esac
	[ "$(printf '%s' "$goName" | tr 'A-Z' 'a-z')" = "$name" ] || fail "the Go name \"$goName\" is not \"$name\" with some of its letters in capitals"
	Title="$goName"
fi
NAME=$(printf '%s' "$name" | tr 'a-z' 'A-Z')
for file in "docs/${NAME}.md" "internal/fetch/requesttype_${name}_test.go" "internal/exporter/requesttype_${name}_test.go" \
	"test/repository/schemakeys_${name}_test.go" "test/repository/schemakeys_${name}rows_test.go"; do
	[ ! -e "$file" ] || fail "$file exists already"
done
! grep -q "RequestType${Title} " internal/fetch/requesttype.go || fail "internal/fetch/requesttype.go has a RequestType${Title} already"

# The places the script edits are found by what they hold; each is checked
# before anything is written, so a tree that has moved one is left as it was.
need() {
	grep -q -e "$2" "$1" 2>/dev/null || fail "$1 has no line matching \"$2\", where the type is added; the file has moved, so add the type there by hand"
}
need internal/fetch/requesttype.go '^var knownRequestTypes = \[\]string{.*}$'
need internal/fetch/requesttype.go '^var optInRequestTypes = \[\]string{.*}$'
need internal/fetch/requesttype.go '^// The request types in the source tree\.$'
need internal/fetch/requesttype_none.go '^//go:build select_request_types'
need Makefile 'for type in [a-z0-9 ]*; do'
need Makefile "^	tags=\"\$\$(sh tools/request-type-tags.sh '' '' [a-z0-9,]*)\""
need .github/workflows/ci.yml '^  request-types:$'
need .github/workflows/ci.yml '^          args: --build-tags request_type_'
need README.md '^- \[.*\](docs/[A-Z0-9]*\.md) — the `[a-z0-9]*` request type'
need docs/CONFIGURATION.md '^- \[[A-Z0-9]*\.md\]([A-Z0-9]*\.md) — the .*`[a-z0-9]*` request type'
need docs/SPECIFICATION-EXPORTER.md ', with `localfile` and `grpc` above'
need test/repository/readmesources_test.go '^var readmeSourceNames = '
need test/repository/requesttypefacts_test.go '^var chartSourceNames = '
need test/repository/schema_fullbuild_test.go '^//go:build !select_request_types'
need internal/fetch/optinkeys_test.go '^//go:build '
need internal/fetch/optinkeys_check_test.go '^//go:build '
need internal/fetch/unusedparams_test.go '^var unusedRefusals = '
need test/repository/schemakeys_fullbuild_test.go 'slices.Concat(httpSchemaKeys(),.*)) {$'
need test/repository/docfacts_test.go '^func TestEveryRequestTypePageNamesTheKeysAStaticTargetMaySet('

# The opt-in types as the Makefile builds them, before this one joins.
oldOptIn=$(sed -n "s/^	tags=\"\$\$(sh tools\/request-type-tags.sh '' '' \([a-z0-9,]*\)).*/\1/p" Makefile | sed -n 1p)
oldOptInTags=$(sh tools/request-type-tags.sh '' '' "$(printf '%s' "$oldOptIn" | tr ',' '\n' | sort | paste -s -d , -)")

# rewrite FILE COMMAND... replaces FILE by what COMMAND writes reading it,
# keeping the file's mode.
rewrite() {
	file=$1
	shift
	tmp="${file}.new-request-type.$$"
	"$@" <"$file" >"$tmp" || {
		rm -f "$tmp"
		fail "rewriting $file failed"
	}
	cat "$tmp" >"$file"
	rm -f "$tmp"
}

# fill writes a template with the type's names in place of @name@, @Title@
# and @NAME@.
fill() {
	sed -e "s/@name@/${name}/g" -e "s/@Title@/${Title}/g" -e "s/@NAME@/${NAME}/g"
}

if [ -n "$optin" ]; then
	constraint="//go:build request_type_${name}"
else
	constraint="//go:build !select_request_types || request_type_${name}"
fi

# --- internal/fetch/requesttype_<name>.go -----------------------------------

{
	printf '%s\n\n' "$constraint"
	fill <<'EOF'
package fetch

import (
	"context"
	"errors"
	"net/http"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The @name@ request type reads TODO(@name@): say what the type reads, what
// a collector writes, what its target is, what a scrape sends and what the
// answer handed to the decoder is, and why the type does what it does, as
// the comment opening each other requesttype_<name>.go does.
//
//	request:
//	  type: @name@

// @Title@Config is a @name@ collector's own request keys, those no other type
// takes, each a field with its yaml key, which a collector's request holds
// in Own (requestkeys.go); a key several types take is one of
// model.RequestConfig's common part, and goes in Fields instead.
// TODO(@name@): the keys, and, once there are any, an accessor reading them
// from a collector, as tcpRequest reads TCPConfig's.
type @Title@Config struct{}

func init() {
	registerRequestType(&RequestType{
		Name: RequestType@Title@,
		// Fields are the keys of model.RequestConfig's common part the
		// type takes, such as tls, max_response_bytes, allowed_targets and
		// denied_targets. TODO(@name@)
		Fields:   []string{},
		Config:   @Title@Config{},
		KeyRules: @name@KeyRules,
		// Overrides are the probe parameters a probe of the type may set,
		// and TargetFields the keys a static target's request may set: the
		// timeout and the param_<name> parameters, which every type takes,
		// until the type has more. TODO(@name@)
		Overrides:    []string{"timeout", PathParamPrefix},
		TargetFields: []string{"timeout"},
		Requester:    @name@Requester{},
		// UnusedParams words the refusal of a probe parameter nothing
		// uses, naming where a placeholder can stand: the label values
		// alone until a key of the type takes one, which joins them here
		// and in Templates. TODO(@name@)
		UnusedParams: func(c *model.Collector, _ RequestOverrides, unused string) error {
			return unusedParamsIn(c, unused, "its label values")
		},
		// The optional hooks and the settings of what the rest of the
		// exporter knows of the type are left at their zero values, which
		// say the type has none of them (RequestType): Stage, the stage
		// named in logs, "http" unset; TargetHint and NoTarget, what the
		// collectors page says of a target; JSONAnswer, Decoder, Rows and
		// FileExtensions, how its answer is decoded; Families, its own
		// self-metric families; SchemaRule, its required keys in the
		// configuration schema; Templates, the keys that take a
		// placeholder; CheckFiles, NamedFiles and ReadFiles, the files it
		// reads; Follow, CloseConnections, Connections, ConnectionLimit and
		// ConnectionPools, the connections it keeps. TODO(@name@): set those
		// the type has.
	})
}

// errNotImplemented@Title@ is what a scrape of a @name@ collector fails with
// until the type is written.
var errNotImplemented@Title@ = errors.New("the @name@ request type is not implemented yet")

// @name@Requester is the @name@ request type's Requester.
type @name@Requester struct{}

// Validate checks a @name@ collector's request and fills in its defaults.
// TODO(@name@): the type's required keys, defaults and rules between keys,
// each refusal naming the collector and the key; until then every request
// is accepted, and its scrape fails.
func (@name@Requester) Validate(*model.Collector) error { return nil }

// Fetch performs one request of a @name@ collector. TODO(@name@): reach the
// target through the target policy, within ctx's deadline, and hand the
// answer back as an HTTPResponse, held to the response limit.
func (@name@Requester) Fetch(_ context.Context, _ string, _ *model.Collector, _ RequestOverrides, _ http.Header) (*HTTPResponse, error) {
	return nil, errNotImplemented@Title@
}

// CheckTarget checks a @name@ collector's target. TODO(@name@): what a
// target is, refused before anything is sent when it is not one; until
// then any target is taken.
func (@name@Requester) CheckTarget(*model.Collector, string, bool) error { return nil }

// Label is the url label of a @name@ request. TODO(@name@): the target as
// the verbose self-metrics label it, without credentials.
func (@name@Requester) Label(target string, _ *model.Collector, _ RequestOverrides) (string, error) {
	return target, nil
}

// Method is the http_method label of a @name@ request. TODO(@name@): what a
// scrape does, in capitals, as tlscert's HANDSHAKE.
func (@name@Requester) Method(*model.Collector, RequestOverrides) string { return "" }

// Display renders a @name@ target for logs and labels. TODO(@name@): without
// credentials.
func (@name@Requester) Display(target string) string { return target }

// @name@KeyRules is what the configuration schema says of the type's own
// keys beyond their Go types, by the path of each under the request block
// (RequestType.KeyRules). TODO(@name@): each key's description, and its
// pattern, bounds or allowed values.
func @name@KeyRules() map[string]map[string]any {
	return map[string]map[string]any{}
}
EOF
} >"internal/fetch/requesttype_${name}.go"

# --- internal/fetch/requesttype_<name>_test.go ------------------------------

{
	printf '%s\n\n' "$constraint"
	fill <<'EOF'
package fetch

import (
	"context"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// TODO(@name@): the type's tests, against a stand-in of what it reads, and a
// bullet for each in spec § 34. This one holds until Fetch is written, and
// goes then.

// Until the @name@ type is written, a collector of it validates, and its
// scrape fails, saying the type is not implemented yet.
func TestA@Title@CollectorIsNotImplementedYet(t *testing.T) {
	c := &model.Collector{Name: "@name@", Request: model.RequestConfig{Type: RequestType@Title@}}
	if err := ValidateRequest(c); err != nil {
		t.Fatal(err)
	}
	_, err := FetchCollector(context.Background(), "target", c, RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), errNotImplemented@Title@.Error()) {
		t.Fatalf("err=%v, want %v", err, errNotImplemented@Title@)
	}
}
EOF
	if [ -n "$optin" ]; then
		fill <<'EOF'

// The keys the opt-in @name@ type takes that no type of a default build does
// are listed (optInRequestKeys, optInTargetKeys).
func TestThe@Title@TypesOptInKeysAreListed(t *testing.T) {
	checkOptInRequestKeys(t, RequestType@Title@)
}
EOF
	fi
} >"internal/fetch/requesttype_${name}_test.go"

# --- internal/exporter/requesttype_<name>_test.go ---------------------------

{
	printf '%s\n\n' "$constraint"
	fill <<'EOF'
package exporter

import (
	"net/http"
	"strings"
	"testing"
)

// TODO(@name@): the type's tests through the exporter, as the other types'
// requesttype_<name>_test.go here have: a probe of a collector, its label
// values filled from the probe's parameters, and a python transform reading
// its answer. This one holds until the type's Fetch is written, and goes
// then.

// Until the @name@ type is written, a probe of a collector of it fails,
// saying so, whatever reads its answer, a python transform included.
func TestAProbeOfA@Title@CollectorFailsUntilTheTypeIsWritten(t *testing.T) {
	requirePython(t)
	server := labelServer(t, `collectors:
  - name: demo
    request:
      type: @name@
    transform:
      type: python
      script: |
        metric(name="demo", type="gauge", value=1)
    metrics: []
`)
	recorder := probeOnce(t, server, "/probe?collector=demo&target=demo", nil)
	if body := recorder.Body.String(); recorder.Code == http.StatusOK || !strings.Contains(body, "not implemented yet") {
		t.Errorf("%d %v, want the scrape to fail as not implemented", recorder.Code, seriesLines(body))
	}
}
EOF
} >"internal/exporter/requesttype_${name}_test.go"

# --- test/repository/schemakeys_<name>[rows]_test.go ------------------------

# The rows of the keys the schemas hold to a rule, compiled in the default
# build, which checks that every such key has one, and their check, in a
# build with the type: one file for a type of the default build, two for an
# opt-in one, as tcp's.
if [ -n "$optin" ]; then
	rows="test/repository/schemakeys_${name}rows_test.go"
	rowsConstraint="//go:build !select_request_types || request_type_${name}"
else
	rows="test/repository/schemakeys_${name}_test.go"
	rowsConstraint="$constraint"
fi
{
	printf '%s\n\n' "$rowsConstraint"
	printf 'package repository\n\n'
	[ -n "$optin" ] || printf 'import "testing"\n\n'
	fill <<'EOF'
// @name@SchemaKeys are the keys of a @name@ collector that a schema holds to
// a rule: allowed values, a pattern or a length. TODO(@name@): a row for
// each, as schemakeys_tcprows_test.go has, built: true for a key of an
// opt-in type, which the committed schemas, the default build's, leave out.
func @name@SchemaKeys() []schemaKey {
	return nil
}
EOF
	if [ -z "$optin" ]; then
		fill <<'EOF'

// The keys of a @name@ collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOn@Title@KeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, @name@SchemaKeys())
}
EOF
	fi
} >"$rows"
if [ -n "$optin" ]; then
	{
		printf '%s\n\n' "$constraint"
		fill <<'EOF'
package repository

import "testing"

// The keys of a @name@ collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOn@Title@KeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, @name@SchemaKeys())
}
EOF
	} >"test/repository/schemakeys_${name}_test.go"
fi

# --- docs/<NAME>.md ---------------------------------------------------------

{
	fill <<'EOF'
# The @name@ request type

TODO(@name@): the page's title, what the @name@ request type reads and what
for, in a paragraph or two, as the other request types' pages open.

EOF
	if [ -n "$optin" ]; then
		fill <<'EOF'
The type is opt-in: the default build and the published image leave it out,
so a build carries it only when asked
([Building with @name@](#building-with-@name@)).

## Building with @name@

`OPT_IN_REQUEST_TYPES` names the opt-in types a build adds to the types it
carries, comma-separated:

```sh
make build OPT_IN_REQUEST_TYPES=@name@              # bin/prometheus-universal-exporter
docker build --build-arg OPT_IN_REQUEST_TYPES=@name@ -t exporter:@name@ .
go build -tags "$(sh tools/request-type-tags.sh '' '' @name@)" .
```

The tag is `request_type_@name@`. A configuration with a `@name@` collector
is refused by a build without the type, at startup, on a reload and by
`--dry-run`, saying how to build one that has it.

EOF
	fi
	fill <<'EOF'
## A collector

TODO(@name@): a collector that works, as a `yaml` block, and what each key
does.

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `@name@`. |
| TODO(@name@) | | |

## The target

TODO(@name@): what a probe's target is, how it is checked, and how it is
shown in logs and labels.

## Static targets

A [static target](STATIC-TARGETS.md) of a `@name@` collector may set
`timeout` under its `request`. TODO(@name@): what a static target of the
type is, and every other key its `request` may set, each named in this
paragraph, as a test of the pages reads the type's TargetFields here.

## Limits

TODO(@name@): what bounds a scrape: its deadline, the size of the answer,
and anything of the type's own.

## Errors and self-metrics

Until the type is written, every scrape of a `@name@` collector fails: `the
@name@ request type is not implemented yet`. TODO(@name@): what fails a scrape, how each failure reads,
and the self-metrics of the type's collectors.

## Limitations

TODO(@name@): what the type does not do, and why.

## Troubleshooting

TODO(@name@): the failures a reader is likely to meet, and what to do about
each.
EOF
} >"docs/${NAME}.md"

# --- the lists of the types -------------------------------------------------

# The types of the tree, this one among them, and the opt-in ones, sorted.
types=$(for f in internal/fetch/requesttype_*.go; do
	case "$f" in *_test.go | */requesttype_none.go) continue ;; esac
	f=${f##*/requesttype_}
	printf '%s\n' "${f%.go}"
done | sort)
optInTypes=$(for t in $types; do
	if [ "$(sed -n 1p "internal/fetch/requesttype_${t}.go")" = "//go:build request_type_${t}" ]; then
		printf '%s\n' "$t"
	fi
done)
defaultTypes=$(for t in $types; do
	case " $(printf '%s' "$optInTypes" | tr '\n' ' ') " in *" $t "*) ;; *) printf '%s\n' "$t" ;; esac
done)
typeWords=$(printf '%s' "$types" | tr '\n' ' ')
defaultCommas=$(printf '%s' "$defaultTypes" | paste -s -d , -)
optInCommas=$(printf '%s' "$optInTypes" | paste -s -d , -)
newOptIn="$oldOptIn"
[ -z "$optin" ] || newOptIn="${oldOptIn},${name}"
newOptInTags=$(sh tools/request-type-tags.sh '' '' "$optInCommas")

# internal/fetch/requesttype.go: the constant, knownRequestTypes and, of an
# opt-in type, optInRequestTypes; gofmt aligns the constants.
rewrite internal/fetch/requesttype.go awk -v name="$name" -v Title="$Title" -v optin="$optin" '
	prev ~ /^\/\/ The request types in the source tree\.$/ && $0 == "const (" { inConst = 1 }
	inConst && $0 == ")" { printf "\tRequestType%s = \"%s\"\n", Title, name; inConst = 0 }
	/^var knownRequestTypes = \[\]string\{.*\}$/ { sub(/\}$/, ", RequestType" Title "}") }
	optin != "" && /^var optInRequestTypes = \[\]string\{.*\}$/ { sub(/\}$/, ", RequestType" Title "}") }
	{ print; prev = $0 }
'
gofmt -w internal/fetch/requesttype.go

# The guard: a build with select_request_types and no type's tag.
guard="//go:build select_request_types"
for t in $types; do guard="${guard} && !request_type_${t}"; done
rewrite internal/fetch/requesttype_none.go awk -v guard="$guard" 'NR == 1 { print guard; next } { print }'

# The constraints that name every opt-in type.
if [ -n "$optin" ]; then
	any=""
	for t in $optInTypes; do any="${any:+${any} || }request_type_${t}"; done
	rewrite internal/fetch/optinkeys_check_test.go awk -v line="//go:build ${any}" 'NR == 1 { print line; next } { print }'
	rewrite internal/fetch/optinkeys_test.go awk -v line="//go:build !select_request_types || ${any}" 'NR == 1 { print line; next } { print }'
	none=""
	for t in $optInTypes; do none="${none} && !request_type_${t}"; done
	rewrite test/repository/schema_fullbuild_test.go awk -v none="$none" '
		NR == 1 {
			n = split($0, terms, " && ")
			line = terms[1] none
			for (i = 2; i <= n; i++) if (terms[i] !~ /^!request_type_/) line = line " && " terms[i]
			print line
			next
		}
		{ print }
	'
fi

# The Makefile: the loops over every type, and the builds of every opt-in
# type.
rewrite Makefile awk -v words="${typeWords% }" -v old="'' '' ${oldOptIn})" -v new="'' '' ${newOptIn})" '
	function replace(s, from, to,    i, out) {
		out = ""
		while ((i = index(s, from)) > 0) { out = out substr(s, 1, i - 1) to; s = substr(s, i + length(from)) }
		return out s
	}
	{ sub(/for type in [a-z0-9 ]+; do/, "for type in " words "; do"); print replace($0, old, new) }
'

# ci.yml: the request-types matrix, the builds of every opt-in type, and the
# lint of their build.
rewrite .github/workflows/ci.yml awk -v name="$name" -v old="'' '' ${oldOptIn})" -v new="'' '' ${newOptIn})" \
	-v oldTags="          args: --build-tags ${oldOptInTags}" -v newTags="          args: --build-tags ${newOptInTags}" '
	function replace(s, from, to,    i, out) {
		out = ""
		while ((i = index(s, from)) > 0) { out = out substr(s, 1, i - 1) to; s = substr(s, i + length(from)) }
		return out s
	}
	$0 == "  request-types:" { inJob = 1 }
	inJob && $0 == "        type:" { inList = 1; print; next }
	inList && /^          - [a-z0-9]+$/ {
		item = substr($0, 13)
		if (!done && (item "") > (name "")) { print "          - " name; done = 1 }
		print
		next
	}
	inList { if (!done) print "          - " name; done = 1; inList = 0; inJob = 0 }
	$0 == oldTags { print newTags; next }
	{ print replace($0, old, new) }
'

# The documentation's marked lists: a list of every type gets a TODO naming
# this one as the list writes a type (a row, in a table of the types), and
# the default build's list names it when it is in a default build.
for page in README.md docs/*.md charts/*/README.md; do
	[ -f "$page" ] || continue
	grep -q '^<!-- request-types: ' "$page" || continue
	rewrite "$page" awk -v name="$name" -v NAME="$NAME" -v optin="$optin" -v defaults="$defaultCommas" '
		function flush(    i, last, row, cells, n, line) {
			last = 0
			for (i = 1; i <= count; i++) if (lines[i] ~ /^\| \[`[a-z0-9]+`\]/) last = i
			for (i = 1; i <= count; i++) {
				line = lines[i]
				if (kind == "default" && optin == "") gsub(/request_types="[^"]*"/, "request_types=\"" defaults "\"", line)
				print line
				if (kind == "all" && i == last) {
					n = gsub(/\|/, "|", line)
					row = "| [`" name "`](" NAME ".md#static-targets)" (optin != "" ? ", opt-in" : "") " |"
					for (cells = 2; cells < n; cells++) row = row " TODO(" name ") |"
					print row
				}
			}
			if (kind == "all" && last == 0) {
				print ""
				printf format ", TODO(%s): say here what the list says of each other type.\n", name, name
			}
			count = 0
		}
		/^<!-- request-types: (all|default)( "[^"]*")? -->$/ {
			print
			kind = ($0 ~ /^<!-- request-types: all/) ? "all" : "default"
			format = "`%s`"
			if (match($0, /"[^"]*"/)) format = substr($0, RSTART + 1, RLENGTH - 2)
			gsub(/%/, "%%", format); sub(/%%s/, "%s", format)
			inList = 1
			next
		}
		inList && $0 == "<!-- /request-types -->" { flush(); inList = 0; print; next }
		inList { lines[++count] = $0; next }
		{ print }
	'
done

# add_after_last FILE PATTERN LINE adds LINE after the last line of FILE that
# matches PATTERN.
add_after_last() {
	file=$1
	tmp="${file}.new-request-type.$$"
	# The pattern goes through the environment: awk would read the
	# backslashes of a -v value as escapes.
	PATTERN="$2" LINE="$3" awk 'NR == FNR { if ($0 ~ ENVIRON["PATTERN"]) last = FNR; next } { print } FNR == last { print ENVIRON["LINE"] }' "$file" "$file" >"$tmp" || {
		rm -f "$tmp"
		fail "rewriting $file failed"
	}
	cat "$tmp" >"$file"
	rm -f "$tmp"
}

if [ -n "$optin" ]; then
	readmeLine="- [The ${name} request type](docs/${NAME}.md) — the \`${name}\` request type, in a build that asks for it: TODO(${name})."
	relatedLine="- [${NAME}.md](${NAME}.md) — the opt-in \`${name}\` request type."
else
	readmeLine="- [The ${name} request type](docs/${NAME}.md) — the \`${name}\` request type: TODO(${name})."
	relatedLine="- [${NAME}.md](${NAME}.md) — the \`${name}\` request type."
fi
add_after_last README.md '^- \[.*\]\(docs/[A-Z0-9]+\.md\) — the `[a-z0-9]+` request type' "$readmeLine"
add_after_last docs/CONFIGURATION.md '^- \[[A-Z0-9]+\.md\]\([A-Z0-9]+\.md\) — the .*`[a-z0-9]+` request type' "$relatedLine"

# Spec § 35's list of the request types' pages.
rewrite docs/SPECIFICATION-EXPORTER.md awk -v add=", \`${name}\` in \`docs/${NAME}.md\` (TODO(${name}): in its place in this list), with \`localfile\` and \`grpc\` above" '
	!done && (i = index($0, ", with `localfile` and `grpc` above")) > 0 {
		$0 = substr($0, 1, i - 1) add substr($0, i + length(", with `localfile` and `grpc` above"))
		done = 1
	}
	{ print }
'

# The repository tests' tables of how the README and the chart name each
# type, whose words a person chooses.
if [ -n "$optin" ]; then description='""'; else description="\"TODO(${name})\""; fi
for table in "test/repository/readmesources_test.go readmeSourceNames {\"TODO(${name})\", \"TODO(${name})\"}" \
	"test/repository/requesttypefacts_test.go chartSourceNames {\"TODO(${name})\", ${description}}"; do
	set -- $table
	file=$1
	var=$2
	shift 2
	rewrite "$file" awk -v var="$var" -v entry="	\"${name}\": $*," '
		$0 ~ "^var " var " = " { inVar = 1 }
		inVar && $0 == "}" { print entry; inVar = 0 }
		{ print }
	'
	gofmt -w "$file"
done

# The pages whose Static targets section names the keys a target of the type
# may set.
rewrite test/repository/docfacts_test.go awk -v entry="		{\"docs/${NAME}.md\", \"internal/fetch/requesttype_${name}.go\"}," '
	/^func TestEveryRequestTypePageNamesTheKeysAStaticTargetMaySet\(/ { inTest = 1 }
	inTest && $0 == "	} {" { print entry; inTest = 0 }
	{ print }
'

# The tables of the schemas' constrained keys, every type's.
rewrite test/repository/schemakeys_fullbuild_test.go awk -v name="$name" '
	/slices\.Concat\(httpSchemaKeys\(\),/ { sub(/\)\) \{$/, "), " name "SchemaKeys()) {") }
	{ print }
'

# The refusal of an unused probe parameter the fetch tests expect of each
# type: the label values alone, as the registration words it.
rewrite internal/fetch/unusedparams_test.go awk -v entry="	RequestType${Title}: \`probe parameters param_tenat are not used by collector \"places\": no placeholder in its label values names them\`," '
	/^var unusedRefusals = / { inVar = 1 }
	inVar && $0 == "}" { print entry; inVar = 0 }
	{ print }
'
gofmt -w internal/fetch/unusedparams_test.go

# The committed schemas name the types of a default build, and the
# catalogue of own keys every type's.
make schemas

cat <<EOF

The ${name} request type is in the tree$([ -n "$optin" ] && printf ', opt-in'). What is left by hand, each place marked
TODO(${name}) (grep -rn 'TODO(${name})' .):

  1. The logic: internal/fetch/requesttype_${name}.go, its keys in ${Title}Config
     with their KeyRules, Fields, Overrides, TargetFields, the Requester's
     methods and the hooks the type has; then make schemas again.
  2. Its tests, in place of the not-implemented ones in
     internal/fetch/requesttype_${name}_test.go and
     internal/exporter/requesttype_${name}_test.go, against a stand-in of what
     it reads, and a row of ${name}SchemaKeys (test/repository/schemakeys_${name}*)
     for each key the schema holds to allowed values, a pattern or a length.
  3. Its page, docs/${NAME}.md, and the TODOs of the lists the documentation
     marks <!-- request-types -->, the README's index, docs/CONFIGURATION.md's
     related pages and spec § 35.
  4. The README's tagline and sources paragraph, the chart README's opening
     and, for a type of the default build, Chart.yaml's description, naming
     the type in the words readmeSourceNames and chartSourceNames give.
  5. The prose that lists every type: the comment on the request types in
     internal/fetch/requesttype.go, docs/DEVELOPMENT.md's code layout, and the
     sentences of which types take a key (go test ./test/repository/ finds
     those that fall behind).
  6. Spec: § 5.1's rules of the type (a subsection of its own, as the other
     types have) and § 34's bullets for its tests.
  7. Examples under examples/, and the chart README's section of the type, if
     it is deployed with anything of its own.
  8. go test ./... and make ci.
EOF
