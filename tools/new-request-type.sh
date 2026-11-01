#!/bin/sh
# Adds a request type to the tree: the files of the type, with a Requester
# that compiles and answers "not implemented" until its logic is written,
# and the wiring that names every type, so that a new type is this command
# and then its logic, its documentation and its tests.
#
#   sh tools/new-request-type.sh <name>                  # a type of every build
#   sh tools/new-request-type.sh <name> opt-in           # an opt-in type
#   sh tools/new-request-type.sh <name> default SSHExec  # its Go name given
#   sh tools/new-request-type.sh <name> default "" an    # its article given
#   sh tools/new-request-type.sh --article <name> [<GoName>]
#
# make new-request-type NAME=<name> [OPT_IN=1] [GO_NAME=<GoName>]
# [ARTICLE=a|an] runs it. --article prints the article the script would
# write before the name, "a" or "an", and changes nothing.
# The name is lowercase letters and digits, starting with a letter, and no
# type of the tree already. The Go name is how the type's identifiers spell
# it, RequestType<GoName> and <GoName>Config: the name with its first letter
# in capitals when it is not given, and otherwise the name with the letters
# of an initialism in capitals, as Go writes them (SSHExec for sshexec), the
# same letters in the same order. The article is the one the name is read
# with, "an ntp collector", "a dns collector": an initialism, a Go name with
# two capitals or more at its start or a name without a vowel, is read
# letter by letter, "an" before a letter whose name starts with a vowel
# sound (f, h, l, m, n, r, s, x as well as a, e, i, o), and a word by its
# first letter, "an" before a, e, i, o and u; ARTICLE, the fourth argument,
# says it when that reading is wrong, as "a uuid" read as a word would be.
# It writes
# internal/fetch/requesttype_<name>.go, its tests in internal/fetch and
# internal/exporter, its table of schema rows in test/repository and
# docs/<NAME>.md; adds the type to the lists of
# internal/fetch/requesttype.go, the guard of requesttype_none.go, the loops
# and the opt-in lists of the Makefile and .github/workflows/ci.yml, the
# lists the documentation marks <!-- request-types -->, the indexes of the
# README and docs/CONFIGURATION.md, spec § 35's list of pages, the copies in
# the documentation of the sorted list of a default build's types, the tables of
# the repository tests that name every type and the pages whose static
# targets they read; runs make schemas; and prints what is left to write by
# hand. Each place it writes that a person has to finish says TODO(<name>).
#
# It is POSIX sh with POSIX sed and awk, as macOS has them, and needs go,
# gofmt and make. Run it from the repository root.
set -eu

fail() {
	echo "new-request-type: $*" >&2
	exit 1
}

# article NAME TITLE is the article NAME is written with: "a" or "an".
# TITLE is its Go name, whose capitals say whether it is an initialism.
article() {
	lead=$(printf '%s' "$2" | sed 's/[^A-Z0-9].*//')
	case "$1" in
	*[aeiouy]*) letters="" ;;
	*) letters=1 ;;
	esac
	[ "${#lead}" -lt 2 ] || letters=1
	if [ -n "$letters" ]; then
		case "$1" in [aefhilmnorsx]*) echo an ;; *) echo a ;; esac
	else
		case "$1" in [aeiou]*) echo an ;; *) echo a ;; esac
	fi
}

if [ "${1:-}" = "--article" ]; then
	[ -n "${2:-}" ] || fail "usage: sh tools/new-request-type.sh --article <name> [<GoName>]"
	t="${3:-}"
	[ -n "$t" ] || t="$2"
	article "$2" "$t"
	exit 0
fi

name="${1:-}"
mode="${2:-}"
goName="${3:-}"
articleGiven="${4:-}"

case "$mode" in
"" | default) optin="" ;;
opt-in | 1) optin=1 ;;
*) fail "the second argument is \"opt-in\" or nothing, not \"$mode\"" ;;
esac

case "$name" in
"") fail "usage: sh tools/new-request-type.sh <name> [opt-in|default] [<GoName>] [a|an]" ;;
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
case "$articleGiven" in
"") a=$(article "$name" "$Title") ;;
a | an) a="$articleGiven" ;;
*) fail "the article is \"a\" or \"an\", not \"$articleGiven\"" ;;
esac
A=$(printf '%s' "$a" | sed 's/^a/A/')
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
# and @NAME@, and its article in place of @a@, and of @A@ at the start of a
# sentence or of a word of an identifier.
fill() {
	sed -e "s/@name@/${name}/g" -e "s/@Title@/${Title}/g" -e "s/@NAME@/${NAME}/g" -e "s/@a@/${a}/g" -e "s/@A@/${A}/g"
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

// @Title@Config is @a@ @name@ collector's own request keys, those no other type
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
		// TargetHint names what a target of the type is and shows its
		// forms, as the collectors page and the debug page ask for one, or
		// NoTarget says it takes none; AnswerHint says what a response of
		// it pasted on the debug page is. Both are required. TODO(@name@)
		TargetHint: func(*model.Collector) (string, string) { return "Server", "host:port" },
		AnswerHint: "Paste the JSON document the exporter makes of the answer, as the Response of a run shows it",
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
		// named in logs, "http" unset; JSONAnswer, Decoder, Rows and
		// FileExtensions, how its answer is decoded; Families, its own
		// self-metric families; SchemaRule, its required keys in the
		// configuration schema; Templates, the keys that take a
		// placeholder; CheckFiles, NamedFiles and ReadFiles, the files it
		// reads; Follow, CloseConnections, Connections, ConnectionLimit and
		// ConnectionPools, the connections it keeps. TODO(@name@): set those
		// the type has.
	})
}

// errNotImplemented@Title@ is what a scrape of @a@ @name@ collector fails with
// until the type is written.
var errNotImplemented@Title@ = errors.New("the @name@ request type is not implemented yet")

// @name@Requester is the @name@ request type's Requester.
type @name@Requester struct{}

// Validate checks @a@ @name@ collector's request and fills in its defaults.
// TODO(@name@): the type's required keys, defaults and rules between keys,
// each refusal naming the collector and the key; until then every request
// is accepted, and its scrape fails.
func (@name@Requester) Validate(*model.Collector) error { return nil }

// Fetch performs one request of @a@ @name@ collector. TODO(@name@): reach the
// target through the target policy, within ctx's deadline, and hand the
// answer back as an HTTPResponse, held to the response limit.
func (@name@Requester) Fetch(_ context.Context, _ string, _ *model.Collector, _ RequestOverrides, _ http.Header) (*HTTPResponse, error) {
	return nil, errNotImplemented@Title@
}

// CheckTarget checks @a@ @name@ collector's target. TODO(@name@): what a
// target is, refused before anything is sent when it is not one; until
// then any target is taken.
func (@name@Requester) CheckTarget(*model.Collector, string, bool) error { return nil }

// Label is the url label of @a@ @name@ request. TODO(@name@): the target as
// the verbose self-metrics label it, without credentials.
func (@name@Requester) Label(target string, _ *model.Collector, _ RequestOverrides) (string, error) {
	return target, nil
}

// Method is the http_method label of @a@ @name@ request. TODO(@name@): what a
// scrape does, in capitals, as tlscert's HANDSHAKE.
func (@name@Requester) Method(*model.Collector, RequestOverrides) string { return "" }

// Display renders @a@ @name@ target for logs and labels. TODO(@name@): without
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
func Test@A@@Title@CollectorIsNotImplementedYet(t *testing.T) {
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
func TestAProbeOf@A@@Title@CollectorFailsUntilTheTypeIsWritten(t *testing.T) {
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
// @name@SchemaKeys are the keys of @a@ @name@ collector that a schema holds to
// a rule: allowed values, a pattern or a length. TODO(@name@): a row for
// each, as schemakeys_tcprows_test.go has, built: true for a key of an
// opt-in type, which the committed schemas, the default build's, leave out.
func @name@SchemaKeys() []schemaKey {
	return nil
}
EOF
	if [ -z "$optin" ]; then
		fill <<'EOF'

// The keys of @a@ @name@ collector get one verdict from the schema and the
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

// The keys of @a@ @name@ collector get one verdict from the schema and the
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

The tag is `request_type_@name@`. A configuration with @a@ `@name@` collector
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

A [static target](STATIC-TARGETS.md) of @a@ `@name@` collector may set
`timeout` under its `request`. TODO(@name@): what a static target of the
type is, and every other key its `request` may set, each named in this
paragraph, as a test of the pages reads the type's TargetFields here.

## Limits

TODO(@name@): what bounds a scrape: its deadline, the size of the answer,
and anything of the type's own.

## Errors and self-metrics

TODO(@name@): what fails a scrape, how each failure reads, and the
self-metrics of the type's collectors. Until the type is written, every
scrape fails: `the @name@ request type is not implemented yet`.

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
# this one as the list writes a type, and the default build's list names it
# when it is in a default build. A table of the types lists those of the
# default build first and the opt-in ones, marked so, after them, each group
# in the order the types came: a row of the default build goes after the
# last row of one, before the opt-in rows, and an opt-in row last.
for page in README.md docs/*.md charts/*/README.md; do
	[ -f "$page" ] || continue
	grep -q '^<!-- request-types: ' "$page" || continue
	rewrite "$page" awk -v name="$name" -v NAME="$NAME" -v optin="$optin" -v defaults="$defaultCommas" '
		function flush(    i, last, row, cells, n, line) {
			last = 0
			for (i = 1; i <= count; i++) if (lines[i] ~ /^\| \[`[a-z0-9]+`\]/ && (optin != "" || lines[i] !~ /^\| \[`[a-z0-9]+`\]\([^)]*\), opt-in \|/)) last = i
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
				printf format ", TODO(%s): say here what this list says of each other type, and move it to its place among them.\n", name, name
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

# The README's index groups the pages by what they read, so the type's line
# goes after the last of the types', for a person to move beside those it
# is like. docs/CONFIGURATION.md's related pages, as spec § 35's list, name
# the types of the default build and then the opt-in ones: the line goes
# after the last of its group.
if [ -n "$optin" ]; then
	readmeLine="- [TODO(${name}): the page's title](docs/${NAME}.md) — the \`${name}\` request type, in a build that asks for it: TODO(${name}): what it reads, and this line moved beside the types it is like."
	relatedLine="- [${NAME}.md](${NAME}.md) — the opt-in \`${name}\` request type."
	relatedGroup='^- \[[A-Z0-9]+\.md\]\([A-Z0-9]+\.md\) — the opt-in `[a-z0-9]+` request type'
else
	readmeLine="- [TODO(${name}): the page's title](docs/${NAME}.md) — the \`${name}\` request type: TODO(${name}): what it reads, and this line moved beside the types it is like."
	relatedLine="- [${NAME}.md](${NAME}.md) — the \`${name}\` request type."
	relatedGroup='^- \[[A-Z0-9]+\.md\]\([A-Z0-9]+\.md\) — the `[a-z0-9]+` request type'
fi
add_after_last README.md '^- \[.*\]\(docs/[A-Z0-9]+\.md\) — the `[a-z0-9]+` request type' "$readmeLine"
grep -E -q -e "$relatedGroup" docs/CONFIGURATION.md || relatedGroup='^- \[[A-Z0-9]+\.md\]\([A-Z0-9]+\.md\) — the .*`[a-z0-9]+` request type'
add_after_last docs/CONFIGURATION.md "$relatedGroup" "$relatedLine"

# Spec § 35's list of the request types' pages: a type of the default build
# before ", and the opt-in" in the lines before the end of the list, an
# opt-in type last; when the list is written otherwise, at its end with a
# TODO.
rewrite docs/SPECIFICATION-EXPORTER.md awk -v name="$name" -v NAME="$NAME" -v optin="$optin" '
	{ lines[NR] = $0 }
	END {
		end = ", with `localfile` and `grpc` above"
		item = "`" name "` in `docs/" NAME ".md`"
		for (n = 1; n <= NR; n++) if (index(lines[n], end) > 0) break
		placed = 0
		if (n <= NR && optin == "") {
			for (k = n; k >= 1 && k > n - 8; k--) if ((i = index(lines[k], ", and the opt-in `")) > 0) {
				lines[k] = substr(lines[k], 1, i - 1) ", " item substr(lines[k], i)
				placed = 1
				break
			}
		} else if (n <= NR) {
			i = index(lines[n], end)
			head = substr(lines[n], 1, i - 1)
			if (match(head, / and `[a-z0-9]+` in `docs\/[A-Z0-9]+\.md`$/)) {
				lines[n] = substr(head, 1, RSTART - 1) ", " substr(head, RSTART + 5) " and " item substr(lines[n], i)
				placed = 1
			}
		}
		if (n <= NR && !placed) {
			i = index(lines[n], end)
			lines[n] = substr(lines[n], 1, i - 1) ", " item " (TODO(" name "): in its place in this list)" substr(lines[n], i)
		}
		for (k = 1; k <= NR; k++) print lines[k]
	}
'

# The lists the exporter writes of the types of a default build, sorted, as
# its message for a type the build left out gives them ("it was built with:
# dns, graphite, ...") and a schema's error for a type written as a number
# ("write one of dns, graphite, ..."): a type of the default build joins
# each copy in the documentation.
if [ -z "$optin" ]; then
	oldDefaultWords=$(printf '%s\n' "$defaultTypes" | grep -v -x -e "$name" | paste -s -d , - | sed 's/,/, /g')
	newDefaultWords=$(printf '%s\n' "$defaultTypes" | paste -s -d , - | sed 's/,/, /g')
	for page in README.md docs/*.md charts/*/README.md; do
		[ -f "$page" ] || continue
		grep -q -F -e "$oldDefaultWords" "$page" || continue
		rewrite "$page" awk -v old="$oldDefaultWords" -v new="$newDefaultWords" '
			function replace(s, from, to,    i, out) {
				out = ""
				while ((i = index(s, from)) > 0) { out = out substr(s, 1, i - 1) to; s = substr(s, i + length(from)) }
				return out s
			}
			{ print replace($0, old, new) }
		'
	done
fi

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

The ${name} request type is in the tree$([ -n "$optin" ] && printf ', opt-in'), written "${a} ${name} collector"
(ARTICLE=a or ARTICLE=an says otherwise). What is left by hand, each place
marked TODO(${name}) (grep -rn 'TODO(${name})' .), which
TestNoScaffoldPlaceholderIsLeftInTheTree fails on until it is gone:

  1. The logic: internal/fetch/requesttype_${name}.go, its keys in ${Title}Config
     with their KeyRules, Fields, Overrides, TargetFields, the Requester's
     methods and the hooks the type has; then make schemas again.
  2. Its tests, in place of the not-implemented ones in
     internal/fetch/requesttype_${name}_test.go and
     internal/exporter/requesttype_${name}_test.go, against a stand-in of what
     it reads, and a row of ${name}SchemaKeys (test/repository/schemakeys_${name}*)
     for each key the schema holds to allowed values, a pattern or a length.
  3. Its page, docs/${NAME}.md, the TODOs of the lists the documentation
     marks <!-- request-types -->, each moved to its place in its list, and
     the README's index line, moved beside the types it is like.
  4. The README's tagline and sources paragraph, the chart README's opening
     and, for a type of the default build, Chart.yaml's description, naming
     the type in the words readmeSourceNames and chartSourceNames give.
  5. The prose that lists every type, which the script cannot write: each
     place docs/DEVELOPMENT.md lists under "Prose that names the types".
  6. Spec: § 5.1's rules of the type (a subsection of its own, as the other
     types have) and § 34's bullets for its tests.
  7. Examples under examples/, and the chart README's section of the type, if
     it is deployed with anything of its own.
  8. go test ./... and make ci.
EOF
