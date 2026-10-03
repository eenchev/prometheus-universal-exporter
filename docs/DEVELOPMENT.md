# Development

```sh
make fmt        # rewrite the whole tree with gofmt, tools/ included
make fmt-check  # fail if any source needs gofmt
make lint       # golangci-lint, same configuration and version as CI
make gopls-check  # gopls check, what VS Code shows, at CI's version
make hooks      # once per clone: a pre-commit hook runs make precommit
make precommit  # fmt-check, lint, gopls-check and vet: what the hook runs
make test       # go test ./..., then with -race, twice, in a random order
make vet
make build      # every request type; REQUEST_TYPES=http builds only those listed
make test-request-types  # go test once for each request type built on its own
make helm-test  # helm lint and the template scenarios CI renders, with the pinned helm
make vulncheck  # govulncheck, for reference; not part of make ci
make ci         # everything above, in CI order

make test-external  # opt-in; probes real third-party endpoints
```

## Code layout

The `main` package at the root holds only the command line (`main.go`) and the
`--dry-run` report (`check.go`), with the tests of the command line itself
(`cli_test.go`, `shutdown_test.go`). The tests that check the repository as a
whole — the documentation, the chart, the workflows, the Dockerfile, the
shipped examples and schemas, and the layering below — are in
`test/repository`, a package of tests only, which runs from the repository
root. Everything else is under `internal/`, in packages that each import only
the ones above them in this list:

| Package | What it holds |
| --- | --- |
| `internal/model` | The shared data types: the configuration as written, the static target file, and `MetricSet`, what a probe produces. |
| `internal/expr` | jq, regex, CSS and XPath compilation, with bounded caches. |
| `internal/fetch` | Request types — `http`, `localfile`, `graphite` and `grpc`, each in its own build-tagged `requesttype_<name>.go`, with `grpc`'s descriptors, connections and calls in `grpc*.go` behind its tag — probe parameters, path parameters, request templates and the HTTP transports. |
| `internal/decode` | Decoders for every response format, the Prometheus text parser, and charset conversion. |
| `internal/transform` | The transforms and metric rules, the Python worker pool, and the checks run on rules and scripts at load. |
| `internal/config` | Loading, validating and reloading the configuration, collector and target files, and their JSON Schemas. |
| `internal/exporter` | The HTTP server and the one pipeline probes and static targets share (`pipeline.go`): cache, shared probes, limits, self-metrics, readiness, static targets and OTLP export. |
| `internal/testutil` | Helpers shared by the tests of several packages; imported only by tests. |
| `internal/grpctest` | An in-process gRPC server for the `grpc` type's tests, with a queue service compiled from `.proto` sources at run time, reflection and TLS; imported only by tests, and behind the type's build tag. |

A package's tests live beside it, unless they need more than it can import: a
test that validates a whole configuration lives in `internal/config`, and one
that probes through a running `Server` lives in `internal/exporter`, even when
what it checks is a transform or a request type.

## Repeatable tests

Every test must pass however many times it runs and in whatever order, which
`make test` and CI check with `go test -race -count=2 -shuffle=on ./...`. A
failure there names the seed it used; run it again with `-shuffle=<seed>`.

State shared across tests is what breaks this. The Python worker pool is one
such thing: its counts — starts, runs, stops, idle workers — would carry over
from test to test. A test that runs Python calls `requirePython(t)`, which also
gives it a pool of its own and stops that pool's workers when it ends; a test
that uses the pool without an interpreter calls `usePythonPool(t)`. Tests
therefore must not use `t.Parallel`, which the swap assumes.

The Python tests run `python3` from `PATH`. Every workflow that runs the
suite — CI, the exporter release and the Dockerfile update — installs the
Python the image ships, the Dockerfile's `PYTHON_VERSION`, with the libraries
the image ships at the Dockerfile's versions (`lxml`, `PyYAML`,
`python-dateutil`; PyYAML is also what `tools/check-manifests.py` reads with),
and a test keeps each of them reading them. Run them with that version locally too: the sandbox depends on what the
standard library imports, and that changes between releases — from 3.12,
`zoneinfo` loads `sysconfig`, which imports the blocked `threading`, so a
sandbox change can pass on 3.11 and fail in the image.

Some tests use a Python module the image does not ship. The tests named
`TestTheStrictOpenMetricsParser…`, and the self-metrics tests that end in a
subtest `the strict reference parser`, give the exporter's OpenMetrics answers
to the strict parser of `prometheus_client`. The module is pinned in
`test/python/requirements.txt`, the one place for modules only the tests use:

```sh
python3 -m pip install -r test/python/requirements.txt
```

Where `python3` does not have the module those tests are skipped, saying what
is missing, so the suite runs on a machine without it. The workflows that run
the suite — CI, the release and the Dockerfile update — install the file and
set `STRICT_OPENMETRICS_PARSER=required` for the job, and with that variable
set, to anything, a missing module or a missing `python3` fails those tests
instead: a green run there has run them. Set it locally to check the same. The
rules that parser checks are also written out in Go beside the tests
(`strictOpenMetricsError`) and checked by tests that run everywhere; the module
confirms that they are the parser's. Dependabot proposes new versions of the
pinned module, and CI runs the suite on a change to the file.

## Tests of a build with only some request types

A build can carry only some request types
([Choosing request types at build time](CONFIGURATION.md#choosing-request-types-at-build-time)),
and such a build's tests have to pass as the default build's do.
`make test-request-types` runs the suite once for each type built on its own,
CI runs the same, and `make ci` includes it. One of them by hand:

```sh
go test -tags "$(sh tools/request-type-tags.sh localfile)" ./...
```

A test that needs a request type says so with a build constraint on its file,
the one the type's own code carries:

```go
//go:build !select_request_types || request_type_http
```

That is every test whose configuration names the type — `type: http`, or
`testutil.MinimalConfig` and `testutil.Collector`, which are http collectors —
that starts a stand-in for a target of the type, or that asserts what only a
build with the type does, such as a probe parameter of http's being refused
for a collector of another type. A test that needs two types names both,
`!select_request_types || (request_type_http && request_type_localfile)`, as
the tests that load `configs/config.example.yaml` do, and one that holds only
with every type, such as the comparison of the committed schemas with what the
code generates, carries `!select_request_types` alone. A test is never skipped
at run time for a type the build lacks: the constraint says what the test
needs where it can be read, and vetting a single-type build compiles exactly
the tests that build runs.

Tests that hold whatever the build carries stay in a file without a
constraint, so every build runs them. Where a file's tests are of both kinds,
those that need a type are in a file beside it named for the type —
`cache_test.go` and `cache_http_test.go`,
`requesttype_grpc_test.go` and `requesttype_grpc_http_test.go` — and a helper
lives where everything that calls it is compiled: `helpers_test.go` for every
build, `helpers_http_test.go` for a build with http,
`helpers_http_or_localfile_test.go` for a build with either. A helper compiled
into a build in which nothing calls it is what the linter reports as unused
when it is run with that build's tags
(`golangci-lint run --build-tags "$(sh tools/request-type-tags.sh grpc)"`).

## Test data

The files under `testdata/` are what the tests read, a directory per format
and `testdata/chart` for the chart's. A fixture is written in the shape its
source writes or documents, not captured from it, and the test that reads it
says what it stands for.

`testdata/csv` holds CSV in the shapes it comes in. The `csvfixtures` tests
read each file at three levels: the rows it decodes into
(`internal/decode/csvfixtures_test.go`); the series, the failures and the log
lines of a collector's rules over it (`internal/transform/csvfixtures_test.go`);
and whole probes, of an `http` collector at a stand-in that sends the file
with one `Content-Type` or another and of a `localfile` collector reading the
file or a directory of them
(`internal/exporter/csvfixtures_probe_test.go` and
`csvfixtures_localfile_test.go`), where every series of each answer is
asserted, in the text format and, where the two differ, OpenMetrics. A table
of 5,000 rows, for the limits, is generated by the test that reads it.

| File | What it stands for |
| --- | --- |
| `status.csv` | The specification's own example: a header and two rows. |
| `tickets-rfc4180.csv` | A helpdesk's ticket export as RFC 4180 writes it: CRLF line ends, and fields with commas, doubled quotes and line breaks in quotes. |
| `inventory-semicolon.csv` | A stock list as a spreadsheet saves it with a German or Bulgarian locale: semicolons between the fields, and numbers with a decimal comma in quotes. |
| `sensors.tsv` | A data logger's tab-separated readings, with empty fields in the middle of rows and at their end, and fields padded with spaces. |
| `queues-pipe.txt` | A query's result as `psql -A` prints it: fields separated by a pipe, and a footer counting the rows. |
| `accounts-colon.txt` | Accounts in the form of `/etc/passwd`: no header, fields separated by a colon. |
| `readings-noheader.csv` | What a data logger appends to its file: no header, a reading a line. |
| `cities-utf8-bom.csv` | A list of cities with headers and values in Cyrillic, accented Latin, Greek, CJK and emoji, as a spreadsheet's "CSV UTF-8" saves it, with a byte order mark. |
| `cities-utf16le-bom.csv` | The same list as a spreadsheet's "Unicode text": UTF-16, little-endian, with a byte order mark. |
| `cities-utf16be.csv` | The same list in UTF-16, big-endian, without a byte order mark. |
| `oblasti-utf8.csv` | The Bulgarian part of such a list, in UTF-8 without a byte order mark. |
| `oblasti-windows-1251.csv` | The same Bulgarian list as an older system writes it, in windows-1251. |
| `communes-iso-8859-1.csv` | The French part of such a list in ISO 8859-1. |
| `nodes-space-aligned.txt` | A cluster tool's table: columns aligned with spaces, numbers to the right, a note of several words in quotes. |
| `hosts-trailing-delimiter.csv` | An export that ends every line, the header's too, with the delimiter. |
| `jobs-short-rows.csv` | A scheduler's report whose writer stops a row at its last value: rows with fewer fields than the header. |
| `jobs-blank-lines.csv` | The same report with blank lines between the rows and after them, and one line of blanks. |
| `jobs-no-final-newline.csv` | The same report without a line end after its last row. |
| `usage-duplicate-columns.csv` | A capacity report whose header names two pairs of columns alike. |
| `usage-unnamed-column.csv` | A capacity report saved from a spreadsheet with an empty header cell above a column of values. |
| `numbers.csv` | The ways exports write a number, and what they write in place of one. |
| `backups-times.csv` | A backup tool's report, each column's time written another way. |
| `usgs-all-hour.csv` | The USGS earthquake feed's `all_hour.csv`, in its documented columns, for `examples/config.usgs.csv-test.yaml`. |
| `service-status.csv` | A fleet's status export: a row per service and host, its state in words, counters and gauges. |

Several of them are written in a way an editor would undo — CRLF line ends,
a byte order mark, UTF-16 and legacy encodings, blanks that end a line, no
final line end — and a test checks that each still is. A file added to
`testdata/csv` goes in this table and in the list of
`internal/decode/csvfixtures_test.go`, which fails until it is in both.

## The configuration schema

`configs/config.schema.json`, `configs/collector-file.schema.json` for
[collector files](CONFIGURATION.md#collector-files) and
`configs/static-targets.schema.json` for the
[static target file](STATIC-TARGETS.md) are generated from the
configuration structs. After adding or changing a key, regenerate all three, or
the test suite fails:

```sh
make schemas
```

The committed files are the schemas of a build with every request type, which
is what `make schemas` runs, and the test that compares them with what the
code generates runs in that build alone: the schema a single-type binary
prints lists the types it carries, and a test in every build holds it to that.

Allowed values, patterns and descriptions that a struct cannot express are added
by path, in `configSchemaRules` for the configuration and collector files and
in `targetsSchemaRules` for the target file, both in
`internal/config/configschema.go`.

When the exporter comes to refuse a value, give the schema the rule too where
a schema can tell, and add the value to
`test/repository/schemaloader_test.go`, which puts each document of its
tables through both the committed schema and `config.Load` and fails when
they disagree. What a schema cannot tell — a least duration, the range of a
size — goes in the key's description and in that file's table of what the
exporter alone refuses.

## Measuring speed

`internal/exporter/probe_bench_test.go` benchmarks a whole probe — the request
to a target on the same machine, the decode, the transform, validation and
the answer — for each transform over the same items, 100 and 5,000 of them:

```sh
go test -run '^$' -bench 'Probe' -benchtime 2s ./internal/exporter/
```

`jq_cached` is answered from the cache, so it measures writing an answer
alone, and `/gzip` writing it compressed. Measure a change to the pipeline
with these before and after, on a machine doing nothing else: `ns/op` moves
with whatever else runs, while `B/op` and `allocs/op` do not, and say most
about a change that saves allocation. A profile says where the time goes:

```sh
go test -run '^$' -bench 'Probe/prom/n=5000$' -benchtime 2s -cpuprofile cpu.out ./internal/exporter/
go tool pprof -top cpu.out
```

What was made fast stays fast by tests, not by the benchmarks: the decoders,
the transforms, the duplicate check and the body read each have a test that
bounds their allocations per series or per body, and a test that compares
them with the plainer code they replaced, kept beside the tests, over a
table and tens of thousands of generated inputs. The allocation bounds are
skipped under `-race`, which changes what is allocated; `make ci` runs the
tests without it as well.

## Tests that reach the internet

`make ci` never touches the network. The demo configurations under `examples/`
describe real services, though, and a stub replaying a captured response cannot
tell you when one of those services renames a field or a column: the local
tests go on passing while the shipped configuration quietly stops working.

`internal/exporter/external_e2e_test.go` closes that gap by probing the real endpoints, and it is
off unless asked for:

```sh
EXTERNAL_E2E=1 go test -run TestExternal -v ./...
# or
make test-external
```

Without `EXTERNAL_E2E` the tests skip with a message saying how to run them, so
`go test ./...` stays offline and deterministic. They are deliberately not in
CI: a red build caused by somebody else's afternoon outage teaches everyone to
ignore red builds. Run them when changing a demo configuration, and every so
often to catch a source that has moved on.

A failure here usually means the service changed or is unreachable rather than
that this code broke, and the assertions say so — they check that the probe
returned 200, that each metric the configuration declares is present, and that
the per-row or per-entry labels survived.

The suite also holds the status page demo's detailed tests,
`internal/exporter/grafanastatus_external_e2e_test.go`, which run its configuration against a
captured copy of status.grafana.com's summary. They need no network but are
opt-in like the rest.
A test file joins the suite by being listed in `test/repository/externalgate_test.go`; every test
in it must be named `TestExternal*`, so `make test-external` picks it up, and
start with `requireExternalE2E(t)`. `TestExternalSuiteTestsAreOptIn`, which
does run by default, fails when either is missing.

The default suite covers every example too, without the network. The tests in
`test/repository` that check the examples — against the schemas, that each
loads, that its scripts satisfy the contract, and that a static target file
is valid with the configuration in its directory — walk `examples/` at any
depth, so a new example, a file or a directory like `examples/open-meteo/`,
is covered without a test naming it. And nine examples run against a local
stand-in answering in their service's documented shape, with every series
asserted: Filebeat's (`internal/exporter/filebeat_example_test.go`),
Open-Meteo's (`internal/exporter/openmeteo_example_test.go`, which also checks
the query the collector sent and scrapes the example's static targets), and
the ECB's, the METAR one with its static targets, mempool.space's, the
Prometheus demo server's, Frankfurter's, with its pre-script, the USGS
earthquake feed's, with the warning its incomplete row is logged with, and
scrapethissite.com's countries page
(`ecb_example_test.go`, `metar_example_test.go`, `mempool_example_test.go`,
`promdemo_example_test.go`, `frankfurter_example_test.go`,
`usgs_example_test.go` and `scrapethissite_example_test.go` beside them, on
the stand-in of `example_standin_test.go` or, for the last, the plain site of
`htmlfixtures_test.go`). Their fixtures under `testdata/` are
written in the shape each service documents, not captured from it, so it is
the external suite that says whether a service still answers that way.

The pages under `testdata/html` are written the same way, each in the shape
of a page collectors are pointed at: a load balancer's statistics report,
an appliance's layout tables around its data tables, lists and dashboard
cards, markup no validator would pass, entities and typographic numbers,
what is in a page and is not its content, attribute names of JavaScript
frameworks, the forms a value takes, a hosted status page.
`internal/exporter/htmlfixtures_test.go` lists every file with what it stands
for, and a test fails on a file the list does not have and on one no test
reads. The `htmlfixtures_*_test.go` files beside it read each page with `css`
and with `xpath` collectors written as a configuration file holds them:
through the decoder and the transform directly, through `/probe` from a local
server sending the `Content-Type` a real one would, and from disk with the
`localfile` request type, naming every series and every log line. The files
under `testdata/html/charset` are one page in the encodings a target may
answer in — windows-1251, Shift_JIS, UTF-16 and the rest — and are not UTF-8
on purpose: an editor that saves them as UTF-8 breaks the tests that read
them.

Static analysis is configured in `.golangci.yml`, so a local `make lint` and the
CI run check exactly the same rules. Install the pinned version with `make
lint-install`; the Makefile and the CI workflow pin the same version, and a test
keeps them in step. `make lint` refuses to run with any other installed
release: a different release enables different checks, so an older one passes
locally what CI then fails (gosec's G705 taint check, for one, is newer than
v2.5). `make lint-install` builds the linter with `go install`, which needs a Go
at least as new as the one that release requires; Homebrew's `golangci-lint` or
the release binary from GitHub work as well, as long as the version matches.

What VS Code and other editors underline comes from gopls, the Go language
server, not from golangci-lint, and some of its analyzers exist only in gopls
(`writestring`, which flags `b.WriteString(a + b)`, for one). `make
gopls-check` runs `gopls check` over every Go file and fails on any finding,
and CI runs it too. Like the linter it is pinned (`GOPLS_VERSION`) and refuses
another installed version; `make gopls-install` installs it, into the same
`GOPATH/bin` the VS Code Go extension uses, so the editor then shows exactly
what CI checks. Let the extension auto-update gopls and the editor may show
findings of a newer release before CI has them; bump the pin to follow.

helm is pinned the same way: `HELM_VERSION` in the Makefile, which every
workflow that installs helm installs too, and a test keeps them equal. `make
helm-test` refuses another installed release, since helm releases word the
schema's errors and render details differently — a chart test that passes
with one can fail with another. `make helm-install` installs it with `go
install`, setting the version at link time as helm's release build does: built
from source without that, helm reports only its release line, `v4.3`, and `make
helm-test` would refuse the helm just installed. (Homebrew's `helm` works too
when its version matches.) To move to a newer helm, change `HELM_VERSION` and
the workflows' `version:` together.

`make helm-test` makes every chart check `ci.yml` makes: the same renders, the
same values that have to be rejected and the same lines looked for in what is
rendered. A test (`TestMakeHelmTestChecksWhatTheCIWorkflowChecks`) reads both
and fails when one has a case the other lacks, so add a chart case to both.

Both go beyond the plain renders: the Recreate strategy without a
`rollingUpdate`, a monitor's `port` and `namespaceSelector` on both monitor
types, the garbage collector's target (`goGC.percent` rendered as `GOGC`, and
no `GOGC` by default), a configuration file whose first line is indented, the
chart as a dependency of a parent chart, and the values that must be refused,
among them a monitor's `auth` without a `type`, a monitor's `port` given as a
number, a `scrapeTimeout` longer than its `interval`, an Ingress without the
Service, a mount at the configuration directory written with a trailing
slash, and a `goGC.percent` that is `off` with no Go memory limit, `0`, or
set beside a `GOGC` entry in `env`. The Go tests in `test/repository`
check the same behaviour in more detail, but are skipped without helm, so a
case worth keeping goes into these two lists as well. Write a case that looks
at what is rendered so that it keeps the render first
(`out="$(helm template ...)"`) and then reads `"$out"`: neither `make` nor the
workflow's shell fails a pipeline whose first command failed, so a check that
some text is absent would pass on a render that failed. Every such check in
the two lists is written that way; `tools/check-manifests.py`, which the
render is piped into, refuses a render with no manifests for the same reason.

A check also has to be able to fail, and the same reader says whether each
one can (`TestEveryHelmCheckOfMakeAndCIFailsWhenItShould`), naming the file,
the line and the recipe or step of one that cannot. What it holds a check to
is how the two files run their shell:

- make runs each recipe line, with the lines continued onto it, in a shell of
  its own started without `-e`, and the line's result is its last command's.
  So a line of more than one command starts with `set -eu`, and no line
  starts with make's `-`, which ignores a failure. The Makefile leaves make's
  way of running a recipe alone: `.IGNORE`, `.ONESHELL`, `.SHELLFLAGS`,
  `MAKEFLAGS`, a `PATH`, a `SHELL` other than sh or bash, and a second rule
  for `helm-test` are reported. GitHub starts a step's shell as `bash -e`, so
  a step needs no `set -e`; a `shell:` — the step's, the job's default or the
  workflow's — that is not bash or sh started with `-e`, `continue-on-error`
  on a step or on the job, a step's `if` other than the chart steps', any
  `if` on the job, and a `changes` step whose `chart` filter no longer holds
  `charts/**` are reported.
- A render that must succeed, or a line that must be found, is a command of
  its own: nothing follows it with `||`, `&&` or `&`, each of which takes its
  failure away, under `set -e` too. The one exception hands the failure to an
  `exit`, which ends the shell there: `check || exit 1`, or
  `check || { echo "..." >&2; exit 1; }`, on one line or on several.
- A render that must be refused, or a text that must be absent, is the one
  condition of an `if` whose `then` branch runs `exit 1`:
  `if helm template ... >/dev/null 2>&1; then echo "..." >&2; exit 1; fi`.
  Without the `exit 1` the check prints its message and passes.
- A check is made on every run of its block. It stands at the top of the
  block, in the body of a `for name in value ...; do` loop whose values are
  written out, or is the condition of an `if` that stands there, and nowhere
  else: there is no guard a check may stand behind. One in a branch of an
  `if` — `if false`, `if [ -n "$SKIP" ]`, the `then` of a refusal — after
  `&&` or `||`, or in a loop over a variable, a command or nothing is
  reported. So is an `exit`, of any status, that is not a check's own: an
  early `exit 0` ends the block before its checks.
- Nothing in a block changes what its commands are or how it ends: a
  function or an `alias` (either can stand in for `helm`, `grep`, `python3`
  or `exit`), `.` or `source`, a `PATH`, `hash`, `return`, `exec`, `break`,
  `continue`, `set +e`, a `set` option other than `-e`, `-u`, `-x`, `-v` and
  `pipefail`, and a `trap` are reported. The one trap allowed is the
  clean-up both lists have, `trap 'rm -rf "$tree"' EXIT`: an `rm` alone, on
  `EXIT`.
- helm is piped only into `grep -q` as a command of its own or into
  `tools/check-manifests.py`, both of which fail on a render of nothing; a
  kept render holds the one helm command, and is read as `echo "$out" |` or
  `printf '%s\n' "$out" |` into `grep -q` and a pattern, `python3 -c` or
  `tools/check-manifests.py`. Another way of looking at a render — a
  here-string, `[[ "$out" == *X* ]]`, a `grep` of a file, `grep -c`, `-F` or
  `-e` — is no check to the comparison, and is reported by name with the
  line to write instead: `echo "$out" | grep -q -- 'X'`.

The reader reports what it does not understand (`!`, `elif`, `while`, `case`,
a subshell, `{` anywhere but after a check's `||`) rather than passing it.
`TestAHelmCheckMadeToothlessIsNoticed` makes each such change — a `|| true`,
a dropped `exit 1` or `set -eu`, a leading `-`, `continue-on-error`, an early
`exit 0`, an `if false` around the checks, a `trap 'exit 0' EXIT`, a `helm`
function, an `if` on the job and the like — to a copy of the Makefile's or
the workflow's text in memory, and fails if the reader does not report it.
`TestAHarmlessEditOfAHelmCheckIsTakenOrToldHowToBeWritten` does the same with
edits that take nothing from a check, and fails if one is refused without a
finding that names it and says what to write, or is called a check that can
never fail. A finding that a list "does not make this check of the chart"
is about one of the cases the Go tests make too (`chartCasesMissing` in
`test/repository/helmtest_test.go` lists them, each with its command line):
both lists make it with exactly that command line, so a case changed on
purpose is changed there and in both lists.

The files those cases render are in `testdata/chart`: a static target
document, `indented-first-line.yaml`, and `parent`, a chart whose one
dependency is the exporter's chart by a `file://` path. `helm dependency
build` writes a `charts` directory and a `Chart.lock` into the parent, so
`make helm-test` and CI build it in a temporary copy of the two charts and the
working tree stays as it is; it needs no network. The parent pins the chart's
version, as a real parent does, and a test fails when a chart bump leaves it
behind.

Run `make hooks` once in a clone. It points git at `.githooks`, whose
`pre-commit` hook runs `make precommit` — `gofmt`, `golangci-lint` and `gopls
check` at the pinned versions, and `go vet` — and refuses the commit when any of them fails, so a
commit CI would fail on those never gets made. `git commit --no-verify` skips
it once. The tests are left to `make test` and CI, since the race run takes
minutes.

That pin is coupled to the Go toolchain in a way worth knowing about.
golangci-lint ships as a binary built with a particular Go release, and its type
checker cannot read standard-library sources from a newer one — run an older
build against a newer toolchain and it does not report a lint failure, it panics
with `file requires newer Go version go1.27 (application built with go1.25)`.
Since the build uses the current stable Go, the pinned linter has to be a
release built with at least that. When Go ships a new minor, golangci-lint needs
bumping with it. Beyond the standard linters it enables `bodyclose`, `errorlint`,
`gocritic`, `gosec`, `misspell`, `nilerr`, `noctx`, `perfsprint`, `revive`,
`unconvert` and `usestdlibvars`. The repository is gofmt-clean and CI fails on
unformatted sources rather than rewriting them.

Known vulnerabilities are reported by `.github/workflows/govulncheck.yml`, on
every push and pull request and weekly on Mondays, since an advisory is
published against code that has not changed. It is for reference only: the job
never fails, so a new advisory cannot block a merge. Findings are written to
the run's summary page and raise a warning on the run; a govulncheck that could
not complete (the vulnerability database unreachable, say) warns too. Keep it
out of the branch protection's required checks. `make vulncheck` runs the same
pinned version locally, and a test keeps the Makefile and the workflow in step
and checks that the step cannot fail the job.

`make test` also validates the GitHub Actions workflows: `test/repository/workflows_test.go`
decodes every file under `.github/workflows` with a parser that rejects
duplicate mapping keys, and checks that each step sets exactly one of `run` or
`uses` and uses no unknown keys. GitHub refuses to create a run for a workflow
it cannot parse, which produces no jobs at all, so a CI step cannot catch that
mistake in the commit that introduces it — the checker would be in the file
GitHub is refusing to read. Running the tests before pushing is what protects
you.

It validates the chart templates the same way. `test/repository/charts_test.go` checks that
every manifest a template renders begins its own YAML document. A template that
renders more than one manifest — several declared, or one wrapped in a range —
needs a `---` before each, and neither `helm lint` nor `helm template` notices a
missing one: helm prints whatever the template produced, so two manifests merge
into a single document and the chart only fails when someone applies it.
`helm-test` and CI render the chart with monitors enabled and additionally fail
when the number of manifests exceeds the number of documents the output parses
into.

A few `gosec` findings are deliberate and are suppressed narrowly, with the
reason stated at the suppression: `request.tls.insecure_skip_verify` is a
documented opt-in, and the exporter necessarily reads the configuration, target
document and credential files whose paths the operator supplies (G304).

Two more are scoped to the single file each applies to rather than excluded
globally. G704 reports the outbound request as server-side request forgery,
which is an accurate description of what this program is — an exporter whose job
is to fetch a URL an operator supplied — so it is excluded on
`internal/fetch/fetcher.go` only, with the exposure bounded by the scheme
allowlist, the response size limit and each collector's
`request.allowed_targets` and `request.denied_targets`, which the operator sets
to say which hosts, addresses and networks its requests may reach, checked
again on every redirect and connection (`internal/fetch/targetpolicy.go`). G703 reports the Dockerfile path that
`tools/depupdate` takes on the command line as attacker-controlled; that is a
developer tool with no untrusted caller, so it is excluded under `tools/`.

## The Go toolchain

CI asks setup-go for `stable`, so the build always uses the current stable Go
release and no workflow needs editing when Go ships a new one. The release
builds its binaries with the Go the image is built with: it reads `GO_VERSION`
from the Dockerfile and has setup-go resolve that minor to its newest patch
release, so the archives and the image of one release carry the same Go. No
workflow installs Go with `go-version-file`, which would pin the `go`
directive's exact version, an old patch release without its security fixes;
a test fails if one does. The `go` directive in `go.mod` is something different: it is the *minimum*
the module requires, raised by dependency updates rather than by whichever
toolchain builds it, so it stays where the dependencies put it. The Dockerfile
pins `GO_VERSION` to a released minor, which `tools/depupdate` keeps moving
within the major.

The two can drift apart in a way that is hard to read: a dependency bump raises
the go directive in a pull request that touches no workflow, and from then on
every build fails with `go.mod requires go >= X` with nothing nearby to explain
it. `test/repository/goversion_test.go` ties them together — it checks that every Go version the
workflows request, and the one the Dockerfile pins, satisfies the go directive —
so `go test ./...` catches the mismatch instead of the next red build.

GitHub Actions uses changed-path detection: Go tests/build/vet/race checks run
for Go source or module changes, while Helm lint/template checks run for changes
under `charts/`. A change under `charts/` runs the Go suite too, because the
template guard above lives there and is worth least on exactly the changes that
would break it. Documentation-only changes do not run either suite.
