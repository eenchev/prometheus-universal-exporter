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
| `internal/testutil` | Helpers shared by the tests of several packages; imported only by tests. The package within it, `internal/testutil/alloctest`, is how a test measures what a function allocates and how it takes a smaller input under the race detector ("Repeatable tests" below), and imports nothing of the module so that the tests of `internal/model` can use it too. |
| `internal/grpctest` | An in-process gRPC server for the `grpc` type's tests, with a queue service compiled from `.proto` sources at run time, reflection and TLS; imported only by tests, and behind the type's build tag. |

A package's tests live beside it, unless they need more than it can import: a
test that validates a whole configuration lives in `internal/config`, and one
that probes through a running `Server` lives in `internal/exporter`, even when
what it checks is a transform or a request type.

An error whose text names where in the response it happened — a row, a node,
an item, a line — or a size or a duration it measured is made with
`model.Errorf`, the number given as a `model.Position`, a `model.Size` or a
`model.Elapsed`: `model.Errorf("CSV column %q is empty in row %d", column,
model.Position(row))`. The text reads as `fmt.Errorf`'s would; the type says
which part of it moves from scrape to scrape, so that the
[failure log](LOGGING.md#repeated-failures) takes the failure for the same
wherever it happens (`model.SameFailureText`, `internal/model/samefailure.go`).
Made with `fmt.Errorf`, such an error is a new failure to the log on every
scrape it moves. A number the configuration gave, and a value read from the
response, are written as they are.

`model.Errorf` makes its text when the text is asked for, not when the error
is made, so that a rule failing on every row costs no text for the rows that
are only counted. Give it values that stay what they are — strings, numbers,
errors — and never a byte slice, a builder or a buffer that is written to
again: make the string first. It wraps what `fmt.Errorf` would, the errors
its format names with `%w`. Where a failure names one of several things kept
in a map, as the labels of a series are, name the first by name, so that the
text is the same on every scrape.

An error whose text a library made cannot say so by how it is made, so it is
given the text it is recognised by where it enters the exporter, with
`model.SameFailureAs`: a CSV, an XML and a YAML syntax error in
`internal/decode`, and whatever a failed fetch has of the one connection or
attempt in `internal/fetch/samefailure.go`, at the one place every request
type's error leaves the package. The value is taken from the error's type
where the type has it (`(*net.OpError).Source`, a stream's number). Where a
library keeps it only in its text — a YAML line, a gRPC status message — that
text is read for the one form the library writes, in that place alone, and a
text of another form is left whole: add a form there with a test that
provokes the real failure, never a search for numbers. A gRPC status message
is the target's as often as the client's, and a value the target sent must
tell failures apart, so a form there is the library's words in full up to the
value (`read tcp `, `grpc: received message larger than max (`), never the
value's shape alone. And reading an error must not be what fails a probe:
`sameFetchFailure` hands the error on as it came if anything in it panics.

A decode error is bounded at the one place every decoder's error leaves by
(`boundedFailure` in `internal/decode/failurebound.go`, called by
`decode.Decode`): past 2,000 bytes the text, and what it is recognised by, are
cut and end with the length, the mark in the recognised text. That is the
last resort, which loses what the error says after the long part: no error
of a decoder's own may reach it for one long value. An error that names a
value of the body is made with `model.Errorf` and gives the value as
`model.Quoted(value)`, or as `model.Bare(name)` where the message writes a
name without quotes, never with `%q`, `%s` or `%v`: the argument formats as
`model.QuoteValue` cuts a value, to 64 bytes and its length, copies no more
than those bytes, so the error holds nothing of the body, and is recognised
with the mark in place of the length. A text of several such values, as a
series named by its labels, is joined of each value's `String()` and `Same()`
and given as `model.ShownAs(text, same)`. Where a library made the text, the
part is cut where the error enters, in the library's one form, as
`yamlPartCut` and `yamlKeyCut` do (`internal/decode/yamlcut.go`) and
`xmlNamesCut` does for the names an XML error holds
(`internal/decode/xmlcut.go`); a text of no known form is left whole, for the
bound. A cut text is made anew (`head + mark`), never a slice of the long
one, and the new error does not wrap the long one: either would keep the
whole text alive for as long as the failure log remembers the failure.

The failure log counts, for each trip, the failures of rules it remembers
(`ruleFailures` in `internal/exporter/failurelog.go`), so that a scrape with
none remembered asks once and makes no rule's key, whose size is that of the
rule's expression. A count at zero beside a remembered failure would leave a
recovery unlogged: an entry is made in `putLocked` and dropped in
`dropLocked` and nowhere else, which
`TestTheRuleFailuresCountedAreTheEntriesThereAre` holds every operation to.
The trip a failure is counted for is the key its scrape reported it with
(`ruleFailedFor`), which the entry keeps, and that is the key the scrape asks
with: it is not read back out of the rule's key. Nothing is read out of a
key. Its parts are the operator's and the scraper's — a probe's target is
any bytes its caller sends, a directory and its files have any names — and
while a key was those parts with a NUL between them, one subject's key could
be another's, or read as another's: the key of a directory's file named
`rule` ended as the marker of a rule's key begins, and read at its first
marker a rule of that file was counted for a key no scrape asks with, its
recovery never logged
(`TestARulesFailureIsCountedForTheTripItsScrapeAsksWith`); what was
remembered of a probe of the target `static target one` was forgotten with
the static target `one`; a file named `schedule` in a directory so named had
the key of that target's skipped turns; and a target with NULs in it could
be written to have the key of a rule of another target's probe.

So a key is made by one of a few constructors, in one form (`subjectKey` in
`failurelog.go`): the kind of its subject, a letter — a probe
(`probeFailureKey`), a static target's own (`staticTargetKey`), what a trip
found at the address it went to (`failureKey`, `aspectKey`), a metric the
static targets endpoint left out (`staticClashKey`); then each part of the
subject after its length and a NUL, so that a part ends where its length
says, whatever it holds; then what of the subject failed, when it is not the
subject itself: one of a fixed few aspects (`failureAspect`), or the marker
of a rule's key with the rule's name and expression, each after its length,
and its `items` (`appendRuleFailureKey`). No two subjects have one key,
whatever their parts hold: `TestNoTwoSubjectsHaveOneKey` reads every
generated key back as the subject it was made of, which only a test does.
What the log needs to know of a key it is told beside it, and keeps with the
entry (`failureState`): the trip of a rule's failure, and whose the failure
is — its collector, and the static target when the failure is a static
target's own. That is what a reload goes by when it forgets
(`forgetCollectorsLocked`, `forgetStaticTargetsLocked`) and what a trip is
held to (`logStands`), so the two drop exactly what is the collector's and
the static target's
(`TestForgettingDropsExactlyWhatIsTheCollectorsOrTheStaticTargets`). A new
thing to remember gets a constructor or an aspect of its own, with a line in
those two tests' tables, never a string appended to a key where the failure
is reported; and a trip's key is made once, in one allocation, the key of an
aspect with its aspect (`aspectKey`).
A scrape with a rule's failure remembered makes no key either, and no
scrape makes one at all: the log does, when it first remembers a failure. A
scrape reports a rule's failure by what tells the rule apart — its name,
its expression and its `items` (`ruleFailedFor`) — and the log writes the
bytes of the rule's key where the last one's were (`ruleLocked`), under its
lock, finds the failure by them, and makes a string of them only to remember
a failure it did not (`failedOf`): once for a rule that fails on every
scrape, and never for a failure the full log does not remember, which is
logged in full on every scrape for as long as the log stays full and would
have had its key made on every one. The log returns the key the failure is
remembered under, or none, which is what the scrape keeps of the rules that
failed on it; a rule whose failure was not remembered it tells by the
rule's parts (`failedAmong`), should another scrape of the trip have had the
failure remembered meanwhile
(`TestARuleRememberedByAnotherScrapeMeanwhileIsNotLoggedAsRecovered`). A
debug probe, which the log is told nothing of, asks for the key an earlier
scrape's failure is remembered under (`rememberedRule`), and makes none. And
a scrape asks about each of its rules only when
a failure is remembered of a rule other than those that failed on it
(`remembersRules`), which a scrape of a target that lacks a value, on which
the same rule fails every time, never is. The bytes of a key are made in one
place (`appendRuleFailureKey`), so a failure is looked up by what it is
remembered under. What the log costs a scrape is measured with
`go test -run '^$' -bench 'LogRuleFailures' ./internal/exporter/`, where
`/every_key` is the scrape as it was while it made every rule's key and
`/full_log` the scrape whose rules fail while the log is full of other
targets' failures; `TestAScrapeWithARuleRememberedAllocatesNothingOfItsExpressionsSize`
holds a scrape with a failure remembered, and
`TestAScrapeWhoseRulesAFullLogDoesNotRememberAllocatesNothingOfItsExpressionsSize`
one whose failures the full log does not remember and a debug probe's, to
allocating the same for expressions of 200 bytes and of 20 KB. A rule's
failure reported with its key made beforehand is kept beside the tests
(`failedUnderKey`), and
`TestARulesFailureIsLoggedAndRememberedByItsPartsAsUnderItsKey` holds the
two to the same lines and the same entries, a full log among them.

## Repeatable tests

Every test must pass however many times it runs and in whatever order, which
`make test` and CI check with
`go test -race -count=2 -shuffle=on -timeout 20m ./...`. A failure there names
the seed it used; run it again with `-shuffle=<seed>`. The limit is a
package's, and the command names it because `go test`'s own ten minutes are
meant for one run without the race detector; a test keeps the Makefile and
`ci.yml` running the same command.

State shared across tests is what breaks this. The Python worker pool is one
such thing: its counts — starts, runs, stops, idle workers — would carry over
from test to test. A test that runs Python calls `requirePython(t)`, which also
gives it a pool of its own and stops that pool's workers when it ends; a test
that uses the pool without an interpreter calls `usePythonPool(t)`. Tests
therefore must not use `t.Parallel`, which the swap assumes.

What the process allocates is shared the same way. `testing.AllocsPerRun` and
`runtime.MemStats` count the allocations of every goroutine, not those of the
function a test measures: a test server still closing its connections, a
timer, a worker of the Python pool and the collector itself — which starts its
workers at the first collection of the process and empties `sync.Pool` at
every one — allocate meanwhile, the more so under the race detector, in a
shuffled run and on a busy machine. A bound of 4 allocations for a function
that makes 1 has met 12 in CI, and a test that ran first in its process counted
the collector's workers as its own. The others can only add to a count, never
take from it, so a test that bounds or compares allocations or allocated bytes
measures them only through `internal/testutil/alloctest`, which measures
several times and takes the least, with a collection before each measurement:

- `AllocsAtMost` and `BytesAtMost`, for a test with a bound, stop at the first
  measurement within it, so a quiet machine pays for one;
- `Allocations`, for a count that has none — one another is compared with
  exactly — is the least of five;
- `Once` is a single measurement, for a cost a test only takes a fraction of to
  bound another by, which takes too long to measure five times.

A test (`TestAllocationsAreMeasuredOnlyThroughAlloctest`) fails for a test file
outside `internal/testutil` that calls `testing.AllocsPerRun` or reads the
allocation counters of `runtime.MemStats` or of a benchmark's result itself.

The race detector changes what is allocated: under it `sync.Pool`, which `fmt`
and the regexp and XPath engines keep their working memory in, hands back only
some of what it is given, and some allocations are larger. A bound that is
tight in a plain build can simply be false there. Measure a new allocation test
under `-race` too; where its bound does not hold there with room to spare, the
test skips its allocation assertions when `alloctest.RaceDetector` says the
detector is on — `t.Skip("the race detector changes what is allocated")`, or a
return before the counts where the test checks other things as well — and its
comment says so.

Time is the other thing that breaks it. CI runs the suite under the race
detector on two cores, with another package's tests beside it, and there a
millisecond's work has taken seconds. So a test waits for the event it is
about and never for a length of time: it polls a condition with
`testutil.WaitFor`, or reads a channel that a hook or its test server closes,
under a bound of half a minute that only ends a hang. Where a test has to
run against one of the exporter's own timeouts, choose the values so that a
slow machine makes the test slower and never fails it: what must not happen
is given long to not happen in, and what must happen has no bound but the
hang's. A test does not assert that something took less than some time; it
asserts an order, a count, or that it took at least so long. A sleep is for
a test server's own slowness, for a clock that has to read later, or for
what must not happen to not happen in; it is not a way to let something else
get ahead. The two tests that hold the static target loop to its interval on
the real clock (`statictargetcadence_test.go`) are the exception: they
compare a time with a multiple of the interval, each beside a test of the
same schedule on a clock of its own.

Two helpers hold that rule for what many tests share. A Python script has a
minute in every test, whatever its `limits.script_timeout`: `TestMain` in the
packages that run scripts, and `usePythonPool` on each fresh pool, set it
(`PythonPool.SetLeastScriptTimeout`), since a script of a millisecond has
overrun the default 100ms on a busy machine. A test of the timeout itself
calls `holdScriptsToTheirTimeout(t)`, and its script is one that never ends,
so the timeout is what stops it however slow the machine. An interpreter has
a minute to start in as well, where the exporter gives it ten seconds, which
one start on a busy machine has overrun: the same two places set it
(`PythonPool.SetStartTimeout`), and `holdScriptsToTheirTimeout` leaves it,
since a test of the script's timeout starts an interpreter like any other. A
test of the start's own limit sets a short one and starts a stand-in for
`python3` that never says it is ready (`pythonstart_test.go`). Neither
setter may be called outside a test, which a repository test holds
(`TestOnlyTestsGiveScriptsALeastTime`). And a test's gRPC
server never listens on a port another had in the same process
(`grpctest.Start`, and `grpctest.Listen` for anything else a test has the
exporter call as a gRPC target): the exporter keeps a connection per address
and a reflection answer per connection for minutes, and the kernel gives a
freed port out again, so a server on a reused port was answered for by what
was kept of the one before it. A test of a server that comes back gives the
connection an hour to wait by itself and the call a minute to connect in
(`connectionsWaitAnHour`, `callsWaitForAConnection`), so that only what the
test is about reconnects, in however long it takes.

What a test leaves behind in its own server is held to the same rule. A
client that was stopped while it dialed leaves connections whose handshake
the server reads later, and a test that counts what its server sees ends them
first, at the server too: `namedServer.done` in
`internal/fetch/wirename_test.go` closes what the test's dialer made and
waits for the server to have closed each. A handshake read late was counted
with the next case's, and failed that case now and then with a name that was
the case before's.

How long the suite takes under the race detector is held down in the tests
too. Code runs several times slower there and the command runs every test
twice, so a test over tens of thousands of generated documents, or a body of
megabytes, costs minutes there that it does not cost a plain run; two packages
once took most of the ten minutes `go test` gave them. A test whose time is
the size of what it works on names both sizes with
`alloctest.UnlessRaced(plain, raced)` — a count of generated cases, the
length of a body, the rounds of a loop, one row in so many of a table that is
multiplied out — or branches on `alloctest.RaceDetector` (`raceDetector` in
the packages that have the constant). The plain run keeps the size that makes
the test thorough. The run under the detector takes a smaller one of the same
kind and checks everything the plain run checks: no test is skipped there and
no assertion left out, a text or count expected of the larger input is
computed from the size, and a floor on what a generated corpus held — so many
documents refused, so many with an alias — is scaled with the corpus and never
dropped. Where a test is about a bound in the code, the smaller input is
still past it. A test of concurrency keeps its goroutines and gives up rounds.
One thing more is allowed where an input cannot shrink, being at a bound, and
the slow part is an oracle the test compares with: under the detector the
result is held to the answer the oracle is known to give, as long as the
plain run asks the oracle itself (`yamlkeysaliasdepth_test.go`, where the
YAML library takes seconds over a chain of aliases at the depth limit).
A test that waits on the clock or on a Python process is not made faster this
way and is left as it is. Aim for a second under the detector; to see where a
package's time goes:

```sh
go test -race -count=1 -json ./internal/decode |
  jq -r 'select(.Action == "pass" and .Test != null and (.Test | contains("/") | not)) | "\(.Elapsed)\t\(.Test)"' |
  sort -rn | head -20
```

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

A test of one type that contrasts it with another is two tests, so that the
type's own build runs what is about the type. `TestGRPCValidation` holds what
a grpc collector's request must be and runs wherever grpc is built; the row
that was its contrast, an http collector refused `retry.codes`, is a test of
its own in a file with the constraint that row needs. That is the other type
alone where the row only configures the other type
(`TestTheKeysOfOtherTypesDoNotApplyToHTTP`), and the two together only where
the assertion holds with both built and not otherwise, as a `method` probe
parameter refused for a graphite collector does: without http in the build
`method` is a parameter no type knows
(`TestAGraphiteProbeRefusesAProbeParameterOfHTTPs`). One row that needs a
second type never puts the whole test behind a constraint of two.

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
asserted, in the order it is answered, in the text format and, where the two
differ, OpenMetrics. A table
of 5,000 rows, for the limits, is generated by the test that reads it.

What the user documentation tells a reader to expect of a file or a page
([Reading CSV](CONFIGURATION.md#reading-csv-what-to-expect),
[Reading HTML](CONFIGURATION.md#reading-html-what-to-expect)) is held line
by line, over small bodies written in the test, by
`internal/transform/csvfixtures_expect_test.go` and
`internal/exporter/htmlfixtures_expect_test.go`, and the table of what a
`Content-Type` names by `internal/decode/detectformat_test.go`: a line added
to either table gets its case there.

| File | What it stands for |
| --- | --- |
| `status.csv` | The specification's own example: a header and two rows. |
| `tickets-rfc4180.csv` | A helpdesk's ticket export as RFC 4180 writes it: CRLF line ends, and fields with commas, doubled quotes and line breaks in quotes. |
| `inventory-semicolon.csv` | A stock list as a spreadsheet saves it with a German or Bulgarian locale: semicolons between the fields, and numbers with a decimal comma in quotes. |
| `stock-padded-quotes.csv` | A stock list as a report writer lays it out: semicolons between the fields, every text in quotes and padded with blanks after the closing quote to its column's width, numbers to the right. Read with `trim_space`. |
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
| `jobs-unquoted-comma.csv` | The same report with a job whose name has a comma and no quotes around it: a row with a value past the header's last column, which fails the decode. |
| `usage-duplicate-columns.csv` | A capacity report whose header names two pairs of columns alike. |
| `usage-unnamed-column.csv` | A capacity report saved from a spreadsheet with an empty header cell above a column of values. |
| `numbers.csv` | The ways exports write a number, and what they write in place of one. |
| `backups-times.csv` | A backup tool's report, each column's time written another way. |
| `usgs-all-hour.csv` | The USGS earthquake feed's `all_hour.csv`, in its documented columns, for `examples/config.usgs.csv-test.yaml`. |
| `service-status.csv` | A fleet's status export: a row per service and host, its state in words, counters and gauges. |
| `volumes-cr.csv` | A storage report as a spreadsheet's "CSV (Macintosh)" saves it: every line ends with a carriage return alone, and a note in quotes has one inside it. |
| `scale-mixed-line-ends.txt` | A balance's log that several tools appended to: no header, semicolons between the fields, texts in quotes padded with blanks, and lines that end with a carriage return alone, with CRLF and with a line feed. Read with `trim_space`. |

Several of them are written in a way an editor would undo — CRLF line ends,
carriage returns alone, a byte order mark, UTF-16 and legacy encodings, blanks that end a line, no
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

A key the schema holds to allowed values, a pattern or a length also gets a
row in the table of its request type, `test/repository/schemakeys_http_test.go`
and its neighbours for `grpc`, `graphite` and `localfile`: the document, the
place of the key, a value both take and one both refuse, and whether both
take the file without the key and with the key written `""`. An optional key
takes `""` as the key left out — `optionalEnum` and `optionalPattern` say so
in the schema — and a rule about a key is made of `writtenKey`, which goes by
the key being written, not by its being there. A test of the build with every
request type fails until a constrained key has its row.

A block that `enabled` switches on is kept unchecked while it is off, by the
schemas as by the exporter. A rule about the value of one of its keys
therefore goes in the block's `then`, which holds with `enabled: true`
(`otlpSchemaRule`), and not on the key; a keyword the key's type gives it,
such as a whole number's `minimum`, is taken off the key by `nil` in its
rule. `test/repository/schemaloader_otlpoff_test.go` puts every key of the
`otlp` block through both, switched off and on, and fails for a key its
table lacks.

A key written as nothing but blanks is not the key written `""`: it is text,
which the key takes or refuses. What the schema and the exporter say of a
key both ways is the table of `test/repository/schemablanks_http_test.go`,
for the keys of a rule, of its labels and of a collector that say what is
read; a new key of that kind gets a row there, and where blanks could only be
a mistake that the exporter would go on to read something with, as a
label's `expression` of blanks was, both refuse them.

## Measuring speed

`internal/exporter/probe_bench_test.go` benchmarks a whole probe — the request
to a target on the same machine, the decode, the transform, validation and
the answer — for each transform over the same items, 100 and 5,000 of them:

```sh
go test -run '^$' -bench 'Probe' -benchtime 2s ./internal/exporter/
```

`jq_cached` is answered from the cache, so it measures writing an answer
alone, and `/gzip` writing it compressed. `jq_detected` and `jq_sniffed` are
`jq_items` with `decoder.type` left to the response: named by its
`Content-Type`, and found from the content of a body served as `text/plain`.
`python` runs a script that emits one series for each item, and `prescript` a
pre-script that passes the items on to `jq` rules, so the two measure handing
a document to a Python worker and reading its answer back. Measure a change
to the pipeline with these before and after, on a machine doing nothing else:
`ns/op` moves with whatever else runs, while `B/op` and `allocs/op` do not,
and say most about a change that saves allocation. A profile says where the
time goes:

```sh
go test -run '^$' -bench 'Probe/prom/n=5000$' -benchtime 2s -cpuprofile cpu.out ./internal/exporter/
go tool pprof -top cpu.out
```

Two results of these benchmarks, as measured on two shared cores (the times
are the range of three runs, old and new in turn; bytes and allocations
hardly move from run to run):

| Benchmark | Before | After |
| --- | --- | --- |
| `jq_sniffed/n=100` | 2.2–3.9 ms, 386 kB, 5,362 allocations | 1.4 ms, 265 kB, 1,741 allocations |
| `jq_sniffed/n=5000` | 84–97 ms, 20.3 MB, 260,777 allocations | 39–41 ms, 13.3 MB, 80,496 allocations |
| `python/n=100` | 5.4–5.9 ms, 474 kB, 5,082 allocations | 2.6–3.1 ms, 216 kB, 1,548 allocations |
| `python/n=5000` | 158–181 ms, 28.0 MB, 241,103 allocations | 95–103 ms, 13.3 MB, 65,945 allocations |
| `prescript/n=100` | 4.2–5.0 ms, 403 kB, 4,089 allocations | 3.0–4.7 ms, 267 kB, 1,996 allocations |
| `prescript/n=5000` | 118–128 ms, 22.6 MB, 200,470 allocations | 70–79 ms, 14.2 MB, 95,515 allocations |

`jq_sniffed` was a body read twice: the detection parsed it with
`encoding/json` to see that it is JSON, threw the result away, and the json
decoder read it again. The detection now reads it with the json decoder and
the decode returns what that made, so `jq_sniffed` costs what `jq_items` and
`jq_detected` cost (36–41 ms at 5,000 items).

`python` was four times `jq_items`, and nearly all of the difference was the
hand-over, not the script. At 5,000 items the exporter spent 20 to 24 ms
writing the request with `json.Marshal` (a copy of the document to mark its
NaNs, then reflection and a sort of every object's keys), and 26 to 28 ms
reading the answer with `encoding/json` into a map for each metric and then
into series. The worker spent 58 ms of processor time: 15 ms reading the
request with `json.loads`, 12 ms in the script, nearly all of it in its
5,000 calls of `metric(...)`, 22 ms walking the answer to replace NaN and the
infinities, and 8 ms writing it with `json.dumps`. Now the exporter writes
the same request line itself in about 5 ms and reads the answer into series
in about 5 ms; the worker writes an answer of plain values without the walk,
after looking through it in 4 ms, and `metric(...)` takes a label that is a
string as it is, 8.5 ms for the 5,000 calls: 37 ms of processor time in the
worker. What is left there is `json.loads` of the request, 15 ms, and
`json.dumps` of the answer, 8 ms. The look through the answer also counts
its values, a check for each list and dict that adds half a millisecond at
5,000 metrics, and has it weighed where the count grows fourfold, as far as
a sixty-fourth of the count goes: that is what keeps an answer that holds
one list many times over, or in itself, from being gone through as it is
written (`weigh` in `pythonworker.go`, and `pythonshared_test.go`).

The lines the exporter and a worker exchange are the lines they were, byte
for byte, so `limits.max_output_bytes` bounds what it bounded and a script
is given what it was given. Two shortcuts were measured and not taken,
because a script would see them. Handing a worker the bytes of a JSON
response in place of the decoded document would save writing `data`, but
the worker would then read another document: its keys in the target's order
rather than sorted, `1.0` as a float rather than the int the exporter's
`1` becomes, `1e400` as `inf` rather than the text the json decoder keeps.
And a request in a binary form Python loads faster than JSON (`pickle`
reads this one in 7 ms, `marshal` in 6) would change the protocol the
specification fixes, one JSON document per line.

`BenchmarkYAMLMerge` in `internal/decode` decodes a YAML body with a merge
(`<<: *base`) of a mapping of 20,000 and of 200,000 keys, beside the same
body without the merge and with the merged pairs written out; `body-bytes`
is what to divide `B/op` and `allocs/op` by:

```sh
go test -run '^$' -bench 'YAMLMerge' -benchtime 3x ./internal/decode/
```

The walk that decodes a document with a large mapping handed the library
each merged key and each merged value alone, two calls of it for a pair.
It now hands them a run of 128 pairs at a time, the keys and then the values
of the keys the merge gives. Decoding the parsed document, parsing left out
(the least of several runs, old and new in turn; 20,000 keys):

| Document | Before | After |
| --- | --- | --- |
| the mapping, not merged | 19 ms, 19.5 bytes and 0.24 allocations per body byte | the same |
| merged once | 48 ms, 52.3 bytes, 0.85 allocations | 31 ms, 33.3 bytes, 0.49 allocations |
| merged four times | 146 ms, 150.5 bytes, 2.68 allocations | 77 ms, 74.6 bytes, 1.24 allocations |
| a list of two, half their keys shared | 102 ms, 48.4 bytes, 0.78 allocations | 67 ms, 31.6 bytes, 0.46 allocations |

A merged pair costs 230 bytes in four allocations where it cost 540 in ten,
and a pair of the mapping itself costs 320 in four. `yamlkeysmerge_test.go`
holds it: a merge may allocate no more than the mapping it merges, the
library is called twice for every 128 keys of a merge, and the walk as it
was (`yamlkeysmergeoracle_test.go`) decodes every document of the tests into
the same value or error.

Following a reload has a benchmark of its own,
`internal/exporter/reloadfollow_bench_test.go`:

```sh
go test -run '^$' -bench 'FollowReload' -benchtime 20x ./internal/exporter/
```

It reloads between two configurations of 50, 500 and 2,000 collectors: the
same collectors read again (`unchanged`), every one changed (`changed`), and
half of them removed and as many added (`half`), each also with a static
target of every collector in a file reloaded with them (`/static`). The
time of the operation is the whole following, with what a reload prepares of
it before it puts its configuration in force (below). Beside it the
benchmark reports `following-ns/op`, what is left of it once the
configuration is in force, and `locked-ns/op`, how long the statistics lock
was held: every probe takes that lock to find its collector's statistics,
and the read of the self-metrics takes it. As measured on two shared cores,
with and without static targets, which make no difference that shows:

| Benchmark | Before | After |
| --- | --- | --- |
| `unchanged/n=50` | 25–34 ms, all of it under the lock; 17.7 MB, 53,113 allocations | 11–13 ms, 2–3 µs of it under the lock; 9.7 MB, 29,219 allocations |
| `unchanged/n=500` | 205–257 ms, all of it under the lock; 176.6 MB, 531,013 allocations | 108–145 ms, 10–14 µs of it under the lock; 97.2 MB, 292,064 allocations |
| `changed/n=500` | 229–286 ms, all of it under the lock; 176.7 MB, 531,032 allocations | 119–142 ms, 21–34 µs of it under the lock; 97.2 MB, 292,083 allocations |
| `half/n=500` | 199–227 ms, all of it under the lock; 132.5 MB, 398,294 allocations | 107–145 ms, 97–147 µs of it under the lock; 92.8 MB, 278,820 allocations |
| `unchanged/n=2000` | 0.7–1.3 s, all of it under the lock; 706.5 MB, 2,124,026 allocations | 345–562 ms, 32–45 µs of it under the lock; 388.7 MB, 1,168,226 allocations |
| `half/n=2000` | 0.5–1.0 s, all of it under the lock; 530.2 MB, 1,593,070 allocations | 344–720 ms, 0.35–0.57 ms of it under the lock; 371.3 MB, 1,115,171 allocations |

Nearly all of the time was one thing: a collector's fingerprint is
its definition written as YAML and hashed, about 0.2 ms and 177 kB for a
collector of one rule, and a reload worked it out for every collector of the
new configuration and again for every collector of the one it had followed,
with the lock held, to tell the collectors it changed. Comparing the static
targets took 0.6 ms for 500 of them, and the maps and the drops the rest.
Now the fingerprints of a configuration are kept with it once it is
followed, so a reload works out those of the new configuration alone; what
the following is to drop and replace is worked out before the lock is taken,
into a plan; and the lock is taken to do that, once the plan is seen to be
still of the configuration in force and from the one followed
(`planFollowing` and `followLocked` in `reconcile.go`).
`reloadplan_http_test.go` holds this by counts and by the order of events,
not by time: no definition is encoded while the lock is held, a reload
encodes each collector of the new configuration once, a probe is answered
while a reload is held between its plan and the lock, and over generated
sequences of reloads with probes and scrapes held across them a server
leaves what one that follows as it did before leaves, the former following
kept beside the test.

A probe that comes when a reloaded configuration is in force and the reload
has not yet followed it needs it followed before it reads its collector
there, and works out the same plan. While the fingerprints were made by the
plan, such a probe waited for those the reload had not made yet, at worst
all of them, at every reload. So a reload makes them before it puts its
configuration in force: the configuration manager lets the server prepare
what a reload has read and checked (`config.Manager.OnPrepare`,
`prepareReload` in `reconcile.go`), on the goroutine that reloads, while
`Get` and `InForce` still answer with the configuration before it and the
probes go on with that one. The configuration then goes in force with every
fingerprint made, those of the collectors of the configuration followed that
it compares them with among them, and whoever follows it first, the reload
or a probe, has only the maps of the plan left to make. The reload as a
whole takes what it took; its configuration is in force later by as long as
the fingerprints take, about 0.1 s for 500 collectors; and a probe never
waits for a reload's fingerprints. `BenchmarkFollowReloadProbeWait`, in the
same file and found by the same `-bench`, reads a file of 50, 500 and 2,000
collectors again as `SIGHUP` reads it, holds the reload where its
configuration is in force and not yet followed, and asks for the state there
as a probe does; `probe-wait-ns/op` is how long that took, and the time of
the operation the whole reload. As measured on two shared cores:

| Collectors | A probe waited | It waits | Left of the following once in force (`following-ns/op`) |
| --- | --- | --- | --- |
| 50 | 13–19 ms | 15–70 µs | 12–43 µs |
| 500 | 121–181 ms | 0.11–0.30 ms | 0.13–0.37 ms |
| 2,000 | 0.38–0.61 s | 0.5–0.7 ms | 0.5–2.3 ms |

The time, the memory and the allocations of a whole reload are what they
were, and so is the time under the lock. Only the fingerprints are prepared:
they are of a configuration alone, which never changes once loaded, so they
are right whatever is followed when they are used. The plan is not prepared,
since it is from the configuration followed, which a test, or anything that
puts a configuration in force without a reload, can make another between
the two. The prepared fingerprints are kept beside those the probes last
used (`fingerprintMemo`), so a probe that still holds the former
configuration does not have the new one's made again, and the memo holds
those of two configurations at most, the one in force and one before it
while probes still read it. `reloadprepare_http_test.go` holds all of it by
counting where a definition is encoded: none once a reloaded configuration
is in force, by the reload's own following or by a probe held between the
two, each collector of the configuration once for a reload, none for a
static target file reloaded alone, and after 1,000 reloads no configuration
is held but the last few. `onprepare_http_test.go` in `internal/config`
holds the order: prepared, in force, told.

The static targets make no difference that shows above because their
schedule is not in what is measured there: it is the scrape loop's own
(`targetSchedule` in `statictargetschedule.go`), and looks at a reloaded
configuration on that loop's goroutine, within a second of the reload. A
target starts again when its collector's definition changed, which the
schedule tells by the collector's fingerprint, kept with the target's place
in the schedule. It made that fingerprint itself: every collector with a
static target was encoded a second time for every reload, and so was the
collector of every target a static target file reloaded alone started. That
kept no probe waiting, but for 2,000 collectors with a target each it was a
third of a second of one core and 355 MB allocated after every reload. Now
the schedule reads the fingerprints the reload made, which the configuration
followed keeps (`followedConfig.fingerprintOf` in `reconcile.go`). The loop
is given the configuration, the static target file and their following as
one (`followedInForce`), none of which changes once followed, and the
fingerprints are read only when the configuration the schedule plans with is
the very one that following is of; for any other, and for a caller that
follows none, as the tests of the schedule alone are, the definition is
encoded as it was. The schedule takes no lock for it; where the exporter
started with the configuration, whose fingerprints no reload prepared, it
makes the ones nothing asked for yet, once, and the probes and the next
reload find them made. `BenchmarkFollowReloadSchedule`, in the same file and
found by the same `-bench`, reloads as `BenchmarkFollowReload` does, every
collector with a static target, and times the look the schedule then takes,
alone; `encodes/op` is how many definitions the reload and the look encoded
together, and `schedule-encodes/op` how many of them the look did. As
measured on two shared cores, old and new in turn, three times:

| Benchmark | Before | After |
| --- | --- | --- |
| `unchanged/n=50` | 10–12 ms; 8.9 MB, 26,735 allocations; 100 definitions encoded, 50 by the look | 0.08–0.14 ms; 49 kB, 185 allocations; 50 encoded, none by the look |
| `unchanged/n=500` | 92–98 ms; 88.8 MB, 267,268 allocations; 1,000 encoded, 500 by the look | 1.3–1.6 ms; 533 kB, 1,768 allocations; 500 encoded, none by the look |
| `changed/n=500` | 94–98 ms; 88.7 MB, 267,018 allocations; 1,000 encoded, 500 by the look | 0.7–1.3 ms; 397 kB, 1,518 allocations; 500 encoded, none by the look |
| `half/n=500` | 98–123 ms; 88.7 MB, 267,143 allocations; 1,000 encoded, 500 by the look | 0.9–1.1 ms; 465 kB, 1,643 allocations; 500 encoded, none by the look |
| `unchanged/n=2000` | 320–333 ms; 355.2 MB, 1,069,038 allocations; 4,000 encoded, 2,000 by the look | 8.2–8.6 ms; 2.1 MB, 7,038 allocations; 2,000 encoded, none by the look |
| `changed/n=2000` | 315–347 ms; 354.7 MB, 1,068,038 allocations; 4,000 encoded, 2,000 by the look | 6.1–7.3 ms; 1.6 MB, 6,038 allocations; 2,000 encoded, none by the look |

What is left of the look for 2,000 collectors is mostly finding each
target's collector by its name among them, 4.6 ms, as it was before.
`reloadschedule_http_test.go` holds it by counting where a definition is
encoded: a reload of a configuration whose collectors all have a static
target encodes each collector once, before it is in force, and the look
after it none, driven as the loop drives it and with the loop running; the
fingerprints the schedule asks for first are the ones the probes use; a
static target file reloaded alone has the look encode only a collector that
nothing had asked about; and over generated runs of reloads every look
leaves the schedule as the former one, kept beside the test, leaves it.

The read of every collector's statistics, which each scrape of the
self-metrics makes under that same lock, has a benchmark beside that one,
`internal/exporter/collectorstats_bench_test.go`:

```sh
go test -run '^$' -bench 'CollectorStats/steady' ./internal/exporter/
go test -run '^$' -bench 'CollectorStats/reloaded' -benchtime 20x ./internal/exporter/
```

`steady` is a read that finds every collector's statistics there, as every
read but the first after a reload does, and `reloaded` the first read after
a reload that replaced every collector by another, which makes the
statistics of each with the lock held; the reload is not timed and takes far
longer than the read, hence the fixed number of rounds. It reports
`locked-ns/op` as `FollowReload` does. As measured on two shared cores, old
and new in turn, three times:

| Benchmark | Before | After |
| --- | --- | --- |
| `steady/n=50` | 26–31 µs, 10–11 µs of it under the lock; 20,304 bytes, 70 allocations | 23–31 µs, 0.9–1.3 µs of it under the lock; 17,872 bytes, 60 allocations |
| `steady/n=500` | 270–319 µs, 106–120 µs of it under the lock; 228,560 bytes, 531 allocations | 205–310 µs, 8–13 µs of it under the lock; 194,992 bytes, 510 allocations |
| `steady/n=2000` | 1.3–2.2 ms, 0.5–0.9 ms of it under the lock; 940,216 bytes, 2,054 allocations | 1.1–1.3 ms, 89–92 µs of it under the lock; 779,680 bytes, 2,022 allocations |
| `reloaded/n=50` | 41–70 µs, 29–48 µs of it under the lock; 36,304 bytes, 120 allocations | 29–44 µs, 12–15 µs of it under the lock; 33,872 bytes, 110 allocations |
| `reloaded/n=500` | 397–438 µs, 236–266 µs of it under the lock; 388,560 bytes, 1,031 allocations | 311–332 µs, 129–155 µs of it under the lock; 354,992 bytes, 1,010 allocations |
| `reloaded/n=2000` | 1.7–2.1 ms, 1.0–1.4 ms of it under the lock; 1,580,216 bytes, 4,054 allocations | 1.2–1.3 ms, 0.58–0.59 ms of it under the lock; 1,419,680 bytes, 4,022 allocations |

The read took the lock and did under it all that gave it the statistics by
name. Of the 100–110 µs for 500 collectors, about 25 were the copy of each
collector's definition, 1,224 bytes, which `for _, c := range` made to read
a name; about 50 the map of the statistics, made without its size and grown
as it was filled; about 9 the slice of the names, grown likewise; and the
rest filling the map and finding each collector's statistics, of which only
the finding, 6–9 µs, needs the lock. Now the names are read by index and the
map and the slices made at their size before the lock is taken, the
statistics are found into a slice under it, and the map is filled from that
once it is released (`collectorStats` in `selfmetrics.go`). What the first
read after a reload still does with the lock held is make the statistics of
the collectors the reload added, a quarter of a microsecond each: 320 bytes,
which is most of it, and a reading of the clock, which is when the
collector's counters began to count. The sort of the names and the copy of
each collector's counters, under that collector's own lock, are as they were
and are the rest of the read.

`collectorstats_http_test.go` holds it without a duration. One read of 500
collectors may make 515 allocations of 210,000 bytes: it makes 510 of
194,992, and made 531 of 228,560 while the map and the names grew; either
left without its size is past the bound alone. And over generated sequences
of reloads that remove, add and bring back collectors, in every order, a
server read as now gives the names, the counters and the very statistics
that one read as before gives, the former read kept beside the test, with a
reload between the read of the configuration and the lock and without. The
copy allocates nothing, so the lint holds it: gocritic's `rangeValCopy`
reports a loop that copies a kilobyte or more at each round, which in this
code is a loop over collectors by value and nothing else (static analysis,
below).

What was made fast stays fast by tests, not by the benchmarks: the decoders,
the transforms, the duplicate check and the body read each have a test that
bounds their allocations per series or per body, and a test that compares
them with the plainer code they replaced, kept beside the tests, over a
table and tens of thousands of generated inputs. The hand-over to Python is
compared the same way, down to the worker's script as it was
(`internal/transform/pythonoracle_test.go`), which a second pool of workers
runs beside the current one. What the failure log costs a scrape whose rules
fail has a benchmark of its own, `LogRuleFailures` — nothing failing, one
rule and thirty failing and remembered, and, as `/full_log`, one and thirty
failing while the log is full and remembers neither — and a bound that does
not depend on the length of the rules' expressions, for the remembered and
for the full log alike (the failure log, above). The
allocation bounds are
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
them. Some of them hold what only looks like a declaration of the encoding
before their own — a `<meta>` in a comment, a description that mentions a
charset — and the `.xhtml` ones start with an XML declaration, which is the
only declaration of one and names another encoding than the `<meta>` of the
other.

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

Of gocritic's checks that are off by default one is on: `rangeValCopy`, from
1,024 bytes. A collector's definition (`model.Collector`) is 1,224, and the
largest other element a loop takes by value is 656, in a test, so what it
reports is a loop over collectors by value, which copies every collector to
look at it. Write `for i := range cfg.Collectors` and take
`c := &cfg.Collectors[i]`, or read `cfg.Collectors[i].Name`. A loop that
wants the copy, to change it and leave the configuration alone, keeps it and
says so beside a `//nolint:gocritic`, as the other exceptions in the code
do. A test in `test/repository` fails when the threshold is raised past a
collector's size or the check is dropped.

Known vulnerabilities are reported by `.github/workflows/govulncheck.yml`, on
every push and pull request and weekly on Mondays, since an advisory is
published against code that has not changed. It is for reference only: the job
never fails, so a new advisory cannot block a merge. Findings are written to
the run's summary page and raise a warning on the run; a govulncheck that could
not complete (the vulnerability database unreachable, say) warns too. Keep it
out of the branch protection's required checks. `make vulncheck` runs the same
pinned version locally, and a test keeps the Makefile and the workflow in step
and checks that the step cannot fail the job.

What is scanned is what ships and what the workflows run: the exporter, the
package at the root, and the repository's tools (`. ./tools/...`), each with
everything it imports. The packages only tests use — `internal/grpctest`, the
stand-in gRPC server of the grpc tests, `internal/testutil` with its
`alloctest`, and `test/repository` — are left out, so an advisory for code
that only a test reaches, such as the gRPC server the exporter never is, raises
no warning; the price is that such an advisory is not reported at all. A test
(`TestTheVulnerabilityCheckScansWhatShips`) keeps the Makefile's and the
workflow's packages equal, fails for a command of the module they do not
cover, and holds the list of what is left out: a new package is either
imported by something scanned or added to that list with its reason.

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
