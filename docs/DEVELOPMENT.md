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

One test uses a Python module the image does not ship:
`TestTheStrictOpenMetricsParserReadsEveryAnswerOfOddValues` gives the
OpenMetrics answers for metric sets with odd values to the strict parser of
`prometheus_client`, and is skipped, saying what is missing, where `python3`
does not have the module (`pip install prometheus-client`). The rules that
parser checks are also written out in Go beside it
(`strictOpenMetricsError`) and checked by tests that run everywhere; the
module confirms that they are the parser's.

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
is covered without a test naming it. And two examples run against a local
stand-in answering in their service's documented shape, with every series
asserted: Filebeat's (`internal/exporter/filebeat_example_test.go`) and
Open-Meteo's (`internal/exporter/openmeteo_example_test.go`, which also checks
the query the collector sent and scrapes the example's static targets).

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
  starts with make's `-`, which ignores a failure. GitHub starts a step's
  shell as `bash -e`, so a step needs no `set -e`; a step with a `shell:` of
  its own that lacks the flag, with `continue-on-error`, or with an `if` other
  than the chart steps' is reported.
- A render that must succeed, or a line that must be found, is a command of
  its own: nothing follows it with `||`, `&&` or `&`, each of which takes its
  failure away, under `set -e` too.
- A render that must be refused, or a text that must be absent, is the one
  condition of an `if` whose `then` branch runs `exit 1`:
  `if helm template ... >/dev/null 2>&1; then echo "..." >&2; exit 1; fi`.
  Without the `exit 1` the check prints its message and passes.
- helm is piped only into `grep -q` as a command of its own or into
  `tools/check-manifests.py`, both of which fail on a render of nothing; a
  kept render holds the one helm command, and is read with `grep -q` and a
  pattern, `python3 -c` or `tools/check-manifests.py`.

The reader reports what it does not understand (`!`, `while`, `case`, a
subshell) rather than passing it. `TestAHelmCheckMadeToothlessIsNoticed`
makes each such change — a `|| true`, a dropped `exit 1` or `set -eu`, a
leading `-`, `continue-on-error` and the like — to a copy of the Makefile's
or the workflow's text in memory, and fails if the reader does not report it.

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
