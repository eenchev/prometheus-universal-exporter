# Development

```sh
make fmt        # rewrite the whole tree with gofmt, tools/ included
make fmt-check  # fail if any source needs gofmt
make lint       # golangci-lint, same configuration and version as CI
make gopls-check  # gopls check, what VS Code shows, at CI's version
make hooks      # once per clone: a pre-commit hook runs make precommit
make precommit  # fmt-check, lint, gopls-check and vet: what the hook runs
make test-quick # while writing a change: go test -short ./..., once, no race detector, the slow tests left out; a few minutes
make test       # go test ./..., then with -race, twice, in a random order; the scaffold test is left to test-scaffold
make test-scaffold  # TestANewRequestTypeIsWiredByTheScaffold alone: make new-request-type on a copy of the tree
make vet
make build      # bin/prometheus-universal-exporter and bin/pue-allocator, every request type; REQUEST_TYPES=http builds only those listed, OPT_IN_REQUEST_TYPES=mqtt adds opt-in types
make run EXAMPLE=open-meteo  # make build, then bin/prometheus-universal-exporter with the example's
                #   configuration and static targets on 127.0.0.1:8080; PORT=, HOST= and ARGS= change
                #   that, any opt-in types the example needs are added, and EXAMPLE= alone lists the examples
make build-request-types  # go build ./..., then go vet and go build for each request type on its own
make test-request-types  # go test once for each request type built on its own
make lint-request-types  # golangci-lint for each request type built on its own
make build-opt-in-types test-opt-in-types lint-opt-in-types  # vet, build, test and lint the build with every opt-in request type
make helm-test  # helm lint and the template scenarios CI renders, with the pinned helm
make vulncheck  # govulncheck, for reference; not part of make ci
make new-request-type NAME=<name> [OPT_IN=1] [GO_NAME=<GoName>]  # a new request type's files and wiring; not part of make ci
make ci         # what CI runs, one after another: fmt-check, lint, lint-request-types,
                #   gopls-check, test, test-scaffold, vet, build-request-types, test-request-types,
                #   the sql drivers' and the opt-in request types' builds, and helm-test

make test-external  # opt-in; probes real third-party endpoints
```

**Binaries.** `make build` puts the binaries in `bin/`, and `make run`
always builds before it runs, so the binary that runs is never one built
before a pull. A plain `go build .` leaves `./prometheus-universal-exporter`
in the repository root instead, ignored by git and soon older than the one
in `bin/`; `make build` warns while it is there.

**Examples.** An example is either a directory under `examples/`, named by
the directory, such as `open-meteo`, or a file
`examples/config.<name>.<format>-test.yaml`, named by `<name>`, such as
`ecb`. A directory's configuration is its `config.yaml`, next to an
optional static target file and any collector files that its
`collector_files` names. `tools/run-example.sh` reads the names, the files
and the opt-in request types that an example needs (a `type: <name>` line
of an opt-in type) from the tree.

Before it runs the binary, it prints:

- the landing page's URL;
- the status page's URL, which every example has (its recent probes fill as
  soon as anything probes);
- the first probe URL that the comment opening the configuration gives as
  `curl '…/probe?…'`, when the collector it names belongs to the example
  and its target is one that a local run can reach: `localhost`,
  `127.0.0.1`, `[::1]`, or a DNS name with a dot that is not a placeholder.
  A first label ending in `-host`, an address of one network, or a name
  under `example.com` or a top-level domain such as `example`, `local`,
  `internal` or `svc` is a placeholder. For a placeholder, such as
  filebeat's `filebeat-host`, it prints the probe with `<your target>` in
  its place.

`HOST` is `127.0.0.1`, so that an example is reachable from this machine
only. `HOST=0.0.0.0` or `HOST=::` serves it to the network, and the URLs
printed are then on `localhost` or `[::1]`, which a browser on this machine
can open. An IPv6 `HOST` may be given bare or in brackets (`HOST=::1`). An
empty `HOST=` fails before anything is built.

## Changelog

A pull request that changes what someone running the exporter or installing
the chart sees adds its lines to `## Unreleased` in
[`CHANGELOG.md`](../CHANGELOG.md):

- under `### What changed for you`, what an upgrade makes them do or notice
  (a key or value renamed or removed, a default changed, a metric renamed);
- under `### New` and `### Fixed`, the rest.

The section becomes a GitHub Release's description, so its lines are
written for users. A refactor, a test or a CI change adds none.
[Releases](RELEASING.md#release-notes) describes how a release moves the
lines into their own section.

## The specifications

A change writes its rules once, as MUST rules, in the section of the feature
that it changes:

- [`SPECIFICATION-EXPORTER.md`](SPECIFICATION-EXPORTER.md) for the exporter;
- [`SPECIFICATION-CHART.md`](SPECIFICATION-CHART.md) for the chart;
- `SPECIFICATION-<TYPE>.md` for what belongs to one request type alone, its
  section of § 5.1 (every type except `http` has one, listed under
  [The other request types](SPECIFICATION-EXPORTER.md#the-other-request-types)).

§ 34 holds only the rules that the test suite is held to. A change adds no
list of its tests or of the cases they cover, because each test says what it
checks in its name and its comment. `TestTheSpecificationsListNoTests` fails
on a test named in a specification or on a § 34 section that lists an
area's cases.

## Goroutines of a fetch

A panic of a scrape is recovered where its goroutine starts, and fails only
that scrape. A goroutine that a fetch starts itself is outside that
recovery, and its panic would end the exporter. Each such goroutine that
runs the type's code (asks a server, reads or parses an answer) therefore
hands its panic back to the scrape (`internal/fetch/goroutinepanic.go`):

- `defer panicked.keep()` in each of several goroutines that the fetch waits
  for, with `panicked.raise()` after the wait;
- or `panicToError` around the work of a goroutine whose error is what it
  hands over, which `FetchCollector` raises.

A goroutine that only closes a connection or copies bytes needs neither.

## Code layout

The `main` package at the root holds only the command line (`main.go`), its
allocator mode (`allocator.go`), the `--dry-run` report (`check.go`) and
`--healthcheck` (`healthcheck.go`), with the tests of the command line
itself (`cli_test.go`, `shutdown_test.go`).

- The target allocator's command line is `cmd/pue-allocator`, with its own
  tests.
- `tools/depupdate` is the command that proposes updates for the versions
  that the Dockerfile pins ([Dependency updates](DEPENDENCIES.md)).
- The tests that check the repository as a whole (the documentation, the
  chart, the workflows, the Dockerfile, the shipped examples and schemas,
  and the layering below) are in `test/repository`, a package of tests only,
  which runs from the repository root.

Everything else is under `internal/`, in packages that each import only the
ones above them in this list:

| Package | What it holds |
| --- | --- |
| `internal/model` | The shared data types: the configuration as written, the static target file, and `MetricSet`, what a probe produces. |
| `internal/allocapi` | What the target allocator and an exporter in allocator mode say to each other: the heartbeat, the bundle, the API's paths and timings, the rendezvous hash (`Score`) the allocator's weighted placement ranks the members by, and the bounds of a replica's capacity. |
| `internal/kubeapi` | Listing objects from a Kubernetes API server over its REST API, with the standard library alone: the in-cluster address, token, CA and namespace of a pod's service account, and a listing of a path page by page with continue tokens, a bound on each page and the API server's refusals as errors; the `kubernetes` request type and the target allocator's Kubernetes discovery list with it. |
| `internal/expr` | jq, regex, CSS and XPath compilation, with bounded caches. |
| `internal/fetch` | Request types — `http`, `localfile`, `graphite`, `grpc`, `sshfile`, `sql`, `dns`, `tlscert`, `ntp`, `kubernetes`, `s3file`, `redis`, `tcp`, `ldap`, `sshexec`, `icmp` and `snmp`, and the opt-in `smtp`, `modbus`, `mqtt`, `websocket`, `ftp`, `imap`, `pop3`, `radius`, `whois`, `sip`, `amqp` and `nats`, each in its own build-tagged `requesttype_<name>.go`, what the opt-in types share in `nettypes.go`, `nettls.go` and `netpassword.go`, `icmp`'s unprivileged ICMP socket in `icmpsocket.go` and `icmpsocket_unix.go`, `snmp`'s BER encoding and SNMPv3 security in `snmpber.go` and `snmpusm.go`, `http`'s FastCGI client in `fastcgi.go`, `request.unix_socket`'s checks, of `http` and `tcp`, in `unixsocketpath.go`, with `grpc`'s descriptors, connections and calls in `grpc*.go`, the SSH connections, keys and targets `sshfile` and `sshexec` share in `sshconn.go` and `sshtarget.go`, behind the tags of the two, `sshfile`'s SFTP reads in `sshsource.go` and `sftplist.go`, the value rule of `tcp`'s and `sshexec`'s placeholders in `linevalue.go`, `sql`'s pools, rows and drivers in `sql*.go` and `s3file`'s reads of a bucket in `s3source.go` behind their tags, and the file reading `localfile`, `sshfile` and `s3file` share in `filereads.go` and `localfile_directory.go`, behind the tags of the three — probe parameters, path parameters, request templates and the HTTP transports. |
| `internal/decode` | Decoders for every response format, the Prometheus text parser, and charset conversion. The `protobuf` decoder's descriptors are read in `internal/fetch` (`protobufdecoder.go`), with the `grpc` type's, behind its tag. |
| `internal/transform` | The transforms and metric rules, the Python worker pool, the states scripts keep between runs (`pythonstate.go`), the checks run on rules and scripts at load, and the metric lint (`lint.go`), the advice on the series a collector makes that `--dry-run` and the debug report give. |
| `internal/config` | Loading, validating and reloading the configuration, collector and target files, and their JSON Schemas. |
| `internal/allocclient` | The exporter's side of the target allocator in allocator mode (`--allocator.url`): the heartbeats to every instance of the allocator, the long poll for its bundle, and the cache directory a bundle is written into, checked in and put in force by swapping one symbolic link, for the configuration manager to reload ([Target allocator](ALLOCATOR.md)). |
| `internal/pagekit` | What the HTML pages of the exporter and of the target allocator share, so they look and behave alike: the stylesheet with its light and dark colours, the theme picker and the head script that applies the theme kept, the logo as the browser-tab icon (`logo.svg`, a copy of `docs/logo.svg`), the pages' Content-Security-Policy, the refresh controls of a page that refreshes itself, and how a page writes a duration (`1m`, not `1m0s`). |
| `internal/exporter` | The HTTP server and the one pipeline probes and static targets share (`pipeline.go`): cache, shared probes, limits, self-metrics, readiness, static targets and OTLP export. |
| `internal/allocator` | The target allocator, whose command line is `cmd/pue-allocator`: its input files and the probe targets file, the member list from heartbeats, allocation and handover, bundles, service discovery and its HTTP API. Its time goes through a `Clock`, so its tests move a clock of their own by hand ([Target allocator](ALLOCATOR.md)). |
| `internal/testutil` | Helpers shared by the tests of several packages; imported only by tests. Among them `TCPStub`, the stand-in server of the `tcp` type's tests in `internal/fetch` and `internal/exporter`: a TCP or TLS server, its certificate written to a file, handing each connection to the test, which plays memcached, ZooKeeper or a server that never ends its answer; `DNSStub`, the stand-in DNS server of the `dns` type's tests and the allocator's `dns_sd` tests, over UDP, TCP and TLS; and `SNMPAgent`, the stand-in agent of the `snmp` type's, answering v1, v2c and v3 at every security level. The package within it, `internal/testutil/alloctest`, is how a test measures what a function allocates and how it takes a smaller input under the race detector ("Repeatable tests" below), and imports nothing of the module so that the tests of `internal/model` can use it too. `internal/testutil/tier`, which imports nothing of the module either, holds `tier.Slow`, the first line of a slow test, which `make test-quick` leaves out ("Running the tests" below). |
| `internal/grpctest` | An in-process gRPC server for the `grpc` type's tests, with a queue service compiled from `.proto` sources at run time, reflection and TLS; imported only by tests, and behind the type's build tag. |
| `internal/sqltest` | A stand-in database for the `sql` type's tests: a `database/sql/driver` that answers each statement with the rows the test gave it, holds a query until its context ends, fails pings or connections, and records the transactions begun and how, the statements run before the query, the values bound and the connections opened and closed. It imports `database/sql/driver` alone, and the exporter reaches it through the type's own pools (`fetch.UseSQLTestDriver`). |
| `internal/kubetest` | A stand-in Kubernetes API server for the tests of `internal/kubeapi` and of the `kubernetes` type: an HTTPS server answering a listing of the objects a test gave it a page at a time, as the limit asks, with continue tokens, refusing a request without the token of its service account directory with `401`, answering `403` and `404` as the API server does, slow to answer when told to, and recording the requests; it writes the service account directory and sets the environment of a pod. It imports nothing but the standard library. |
| `internal/redistest` | A stand-in Redis server for the `redis` type's tests: a TCP or TLS server, its self-signed certificate written to a file, answering RESP2 commands as Redis and Valkey do — AUTH with or without a username against the password it was given, NOAUTH before it, SELECT of the databases 0 to 15, INFO of the sections a test gave it, all or one by name, and any other command with the raw reply the test set — with a hook that answers a command with any bytes, to play a broken, hostile or silent server, and the commands it was sent recorded. It imports the standard library alone; imported only by tests. |
| `internal/ldaptest` | A stand-in LDAP server for the `ldap` type's tests: a TCP server, or a TLS one for `ldaps://`, its self-signed certificate written to a file, answering LDAPv3 messages in BER as a directory server does — a simple bind, anonymous or as an account it was given, `invalidCredentials` to a wrong password, StartTLS unless told to refuse it, and a search of the entries a test gave it by base, scope and filter, with the attributes asked for, a page at a time with the Simple Paged Results control unless told to ignore it, and continuation references — with a hook that answers a request with any bytes, to play a broken, hostile or silent server, and the requests it was sent recorded, each filter written back as RFC 4515 text. It imports the standard library alone; imported only by tests. |
| `internal/sshtest` | An in-process SSH server with an SFTP subsystem for the `sshfile` type's tests, and commands for the `sshexec` type's, run by the test's own function or by `/bin/sh`, with their exit statuses, signals and stderr, serving the test's own temporary directories, with host keys, key and password authentication, encrypted client keys, a switch that makes it stop answering, a limit on the sessions of a connection, counts of the sessions open and of the directory entries listed, and a hook that serves SFTP sessions in the test's place; imported only by tests, and behind the two types' build tags, so a build without them compiles no SSH code even for its tests. |

A package's tests live next to it, unless they need more than it can import.
A test that validates a whole configuration lives in `internal/config`, and
one that probes through a running `Server` lives in `internal/exporter`,
even when what it checks is a transform or a request type.

### How a request type is registered

A request type registers a `fetch.RequestType` from the `init` function of
its `internal/fetch/requesttype_<name>.go`, under the build constraint that
decides whether a build includes it. The registration holds everything that
the exporter knows about the type. The rest of the exporter reads it from
the registry, `fetch.RequestTypes`, instead of naming the type: nothing in
package `main`, `internal/config`, `internal/decode`, `internal/exporter` or
`internal/transform` names a request type. `TestCorePackagesNameNoRequestType`
keeps it so, with an allowlist of the same words used with another meaning,
such as the graphite decoder or `tcp` as a network.

- `Name`, `Fields`, `Config`, `Overrides` and `TargetFields`: the type's
  name, the keys of the request block's common part that it accepts, the
  struct of the keys that only it accepts (below), the probe parameters that
  may change them, and the keys that a static target may set.
- `Requester`: what every type does. It is an interface, and each
  registration gives a small value of it (`httpRequester{}`,
  `tcpRequester{}`).
  - `Validate` checks a collector's request and fills in its defaults.
  - `Fetch` performs one request.
  - `CheckTarget` checks a target.
  - `Label` and `Method` give the `url` and `http_method` labels of the
    verbose self-metrics.
  - `Display` renders a target for logs.
  - A type whose target is a URL embeds `fetch.URLTarget` (http's label,
    method and display) and overrides what differs, as graphite does with
    `Method`.
- The optional hooks, function fields that a type without the capability
  leaves nil:
  - a target that a probe may leave out (`OptionalTarget`);
  - the checks of a static target's request (`CheckOverride`,
    `CheckTargetRequest`);
  - the files read at a request or at load (`CheckFiles`, `ReadFiles`,
    `NamedFiles`);
  - what `--dry-run` shows of its request's policy (`Policy`);
  - the method and body of a request made over HTTP (`MethodBody`);
  - connections kept across scrapes (`Follow`, `CloseConnections`,
    `Connections`, `ConnectionLimit`, `ConnectionPools`);
  - logins (`Logins`).

  They stay fields instead of methods of `Requester`, so that a type without
  the capability says nothing, instead of implementing a method to say that
  it has none.
- What the rest of the exporter knows about the type:
  - its own self-metric families (`Families`); the shared ones follow from
    its keys and hooks, the target lists giving
    `http_exporter_targets_refused_total`;
  - whether its cached responses are revalidated (`Revalidates`);
  - what the collectors page and the debug page ask for as its target
    (`TargetHint`, a label and an example of its forms, or `NoTarget`), and
    what a response of it pasted on the debug page is (`AnswerHint`). Both
    are required: a type registered without them fails at its start;
  - whether the debug page writes it (`DebugPage`);
  - its decoder (`JSONAnswer`, `Decoder`, `Rows`, `FileExtensions`);
  - the article that its name is read with (`Article`);
  - its rule in the configuration schema (`SchemaRule`) and what the schema
    says about its own keys (`KeyRules`);
  - the templated parts of its request and the encoding of their values
    (`Templates`, `templateEscapers`), the filters that a field of its own
    kind takes (`templateFilterRules`, as grpc's message takes only those
    that write JSON), and whether a probe's overrides replace it
    (`templateReplacers`);
  - the places that a refusal of unused probe parameters names
    (`UnusedParams`).

  The shared template code, `requesttemplate.go`, keys no rule by a field's
  place: `TestTheSharedTemplateCodeKnowsNoFieldOfOneType` fails for a file
  of `internal/fetch` that every build compiles and that compares a
  templated field's `where` with a literal.

**A type's own keys.** A collector's `request` block is written flat, with
its type's keys beside `type`, but only the keys that several types take are
fields of `model.RequestConfig`, its common part.
`TestTheCommonPartHoldsNoKeyOfOneType` fails for a field there that only one
type takes. The keys that only one type takes are fields of the type's own
struct, declared in its file next to the registration and given as its
`Config`, such as `fetch.TCPConfig{}` with `send` and `terminator`.

- The loader reads `request.type` and puts the keys that belong to that type
  into a value of its struct, which the collector holds in `Request.Own`.
  It is written back flat when the collector is written as YAML (its cache
  key, a reload's comparison).
- A key that belongs to another type goes into `Request.Foreign`, which the
  validation refuses, as it refuses a key of the common part that the type
  does not take.
- The block keeps the order of its keys in the file, `Request.Order`
  (`internal/model/keyorder.go`). Of several refused keys, the error
  therefore names the first in the file, whatever refuses each, and a check
  of several similar keys ranges over them in that order
  (`model.InFileOrder`), never over a map.
- The decoder asks the registry which keys a type refuses
  (`model.RequestKeys.Refuses`), so that an unknown key after one of them is
  refused only once that key is gone.
- The type reads its keys with a typed accessor (`tcpRequest(c).Send`) and
  writes a changed value back into `Own`. `Own` is a value, so a copy of a
  collector is a copy of its keys.
- A key's schema is made from its field's Go type, like every other key's,
  with what the type's `KeyRules` say about it by its path under the request
  block: its description, and any pattern or bound.

**Builds without the type.** A build without the type contains neither the
struct nor the rules. It still knows the keys, from
`internal/fetch/requestkeys_catalogue.go`, a catalogue of the own keys of
every type in the tree, with the Go types of their fields and their
schemas.

- A type of `internal/model` is recorded by its name. A type that belongs to
  the request type, such as redis's `RedisCommand`, is recorded by its
  structure, which a build without the request type rebuilds to read the key
  into. Such a build therefore refuses what a build with the type refuses of
  the value.
- A build that left out `tcp` refuses `send` under another type as a key of
  another type, in the same words as a build with every type, and its schema
  describes `send` in the same way.
- `make schemas` writes the catalogue from a build with every type, by
  running `TestTheRequestKeyCatalogueIsCurrent` (`internal/config`) with
  `-update-request-keys`. Otherwise that test holds the catalogue to the
  structs.
- What a rule derives from the build, such as the drivers that
  `request.driver` names, comes from the generating build, which has the
  default drivers: a build without `sql` describes `driver` with them.

A new type is therefore its own files and the build wiring (the guard file,
the tag script, the Makefile's and CI's loops and the documentation's marked
lists, each checked by a test), the catalogue that `make schemas` writes, and
nothing in the core. `make new-request-type` writes the files and the wiring
([Adding a request type](#adding-a-request-type)). Only `requesttype.go` in
`internal/fetch` names every type in the tree, built or not, in
`knownRequestTypes` and `optInRequestTypes`, so that a configuration that
names a type the build left out is told so.

**Errors that name a position.** An error whose text names where in the
response it happened (a row, a node, an item, a line), or a size or a
duration that it measured, is made with `model.Errorf`, with the number
given as a `model.Position`, a `model.Size` or a `model.Elapsed`:
`model.Errorf("CSV column %q is empty in row %d", column, model.Position(row))`.

- The text reads as `fmt.Errorf`'s would. The type marks which part of it
  changes from scrape to scrape, so that the
  [failure log](LOGGING.md#repeated-failures) treats the failure as the same
  wherever it happens (`model.SameFailureText`,
  `internal/model/samefailure.go`). Made with `fmt.Errorf`, such an error is
  a new failure for the log on every scrape where it moves.
- A number that the configuration gave, and a value read from the response,
  are written as they are.
- `model.Errorf` builds its text when the text is requested, not when the
  error is made, so a rule that fails on every row costs no text for the rows
  that are only counted. Give it values that do not change (strings,
  numbers, errors), never a byte slice, a builder or a buffer that is written
  to again: build the string first.
- It wraps what `fmt.Errorf` would: the errors that its format names with
  `%w`.
- Where a failure names one of several things kept in a map, such as the
  labels of a series, name the first by name, so that the text is the same
  on every scrape.

**Errors from libraries.** An error whose text a library built cannot carry
this information, so it is given the text that identifies it where it
enters the exporter, with `model.SameFailureAs`. This happens for a CSV, an
XML and a YAML syntax error in `internal/decode`, and for whatever a failed
fetch has of the one connection or attempt in
`internal/fetch/samefailure.go`, the one place where every request type's
error leaves the package.

- The value is taken from the error's type where the type has it
  (`(*net.OpError).Source`, a stream's number).
- Where a library keeps the value only in its text (a YAML line, a gRPC
  status message), that text is parsed for the one form that the library
  writes, in that place only, and a text of another form is left whole. Add
  a form there with a test that provokes the real failure, never a search
  for numbers.
- A gRPC status message comes from the target as often as from the client,
  and a value that the target sent must distinguish failures. A form there
  is therefore the library's words in full up to the value (`read tcp `,
  `grpc: received message larger than max (`), never only the value's shape.
- Reading an error must never be what fails a probe: `sameFetchFailure`
  passes the error on unchanged if anything in it panics.

**Bounding error text.** A decode error is bounded at the one place where
every decoder's error leaves (`boundedFailure` in
`internal/decode/failurebound.go`, called by `decode.Decode`). Beyond 2,000
bytes, the text and its identifying text are cut and end with the length,
with the mark in the identifying text. That is the last resort, which loses
what the error says after the long part: no error of a decoder's own may
reach it because of one long value.

- The bound itself is `model.BoundedFailure`
  (`internal/model/failurebound.go`). Every other stage's error passes it
  once, where the trip reports the error (`boundedTripFailure` in
  `internal/exporter/pipeline.go`: the stages of `collect`, a file of a
  directory in `collectDirectory`, a static target's stages before its trip
  in `logCollectFailure`), and a rule's first failure passes it where the
  transform hands it to the report (`ruleFailures.finish`).
- An error is examined (`errors.Is`, `errors.As`) before it is bounded: a cut
  error is a new error.
- A new place that answers, logs or remembers an error's text takes the
  error from one of those places, or bounds it itself. The failure log cuts
  what it remembers in any case.
- Outside the decoders, a site whose error explains the cause after a part
  that the target, the scraper or a script can make long shows that part by
  its start, as the decoders do. Use `model.Shown(text, limit)` for free
  text, or one of the existing helpers (`shortURLErrors` for a URL in a
  fetch's error, `shownScriptError` and `shownStderr` for what a script and
  its interpreter said, `shownName` in `MetricSet.Validate`). It is
  identified with the mark in place of the length.
  `TestTheLargestFailureOfEveryStageIsAnsweredLoggedAndRememberedWithinTheBound`
  has a case for each stage, and a new stage gets one.
- A log line that names, in an attribute, a metric whose name the target or
  a script chose gives it as `model.ShownName(name)`, the rule of
  `shownName` without the error's quotes. It leaves the key that its failure
  is remembered under as it is, with the whole name where the name is part
  of it.
- An error that names a value of the body is made with `model.Errorf` and
  gives the value as `model.Quoted(value)`, or as `model.Bare(name)` where
  the message writes a name without quotes, never with `%q`, `%s` or `%v`.
  The argument formats as `model.QuoteValue` cuts a value, to 64 bytes and
  its length, and copies no more than those bytes, so the error holds
  nothing of the body. It is identified with the mark in place of the
  length.
- A text of several such values, such as a series named by its labels, is
  joined from each value's `String()` and `Same()` and given as
  `model.ShownAs(text, same)`.
- Where a library built the text, the part is cut where the error enters,
  in the library's one form, as `yamlPartCut` and `yamlKeyCut` do
  (`internal/decode/yamlcut.go`) and `xmlNamesCut` does for the names that
  an XML error holds (`internal/decode/xmlcut.go`). A text of no known form
  is left whole, for the bound.
- A cut text is built anew (`head + mark`), never as a slice of the long one,
  and the new error does not wrap the long one: either would keep the whole
  text alive for as long as the failure log remembers the failure.

**Counting rule failures.** The failure log counts, for each trip, the
failures of rules that it remembers (`ruleFailures` in
`internal/exporter/failurelog.go`). A scrape with none remembered therefore
asks once and builds no rule key, whose size is that of the rule's
expression.

- A count at zero beside a remembered failure would leave a recovery
  unlogged. An entry is therefore made in `putLocked` and dropped in
  `dropLocked` and nowhere else, which
  `TestTheRuleFailuresCountedAreTheEntriesThereAre` checks for every
  operation.
- The trip that a failure is counted for is the key that its scrape
  reported it with (`ruleFailedFor`), which the entry keeps, and that is the
  key the scrape asks with. It is not parsed back out of the rule's key.

**Nothing is parsed out of a key.** A key's parts come from the operator and
the scraper: a probe's target is any bytes its caller sends, and a directory
and its files can have any names. When a key was those parts joined by NUL,
one subject's key could equal, or be read as, another's:

- the key of a directory's file named `rule` ended as the marker of a rule's
  key begins, and read at its first marker, a rule of that file was counted
  for a key that no scrape asks with, so its recovery was never logged
  (`TestARulesFailureIsCountedForTheTripItsScrapeAsksWith`);
- what was remembered of a probe of the target `static target one` was
  forgotten with the static target `one`;
- a file named `schedule` in a directory of that name had the key of that
  target's skipped turns;
- a target with NULs in it could be crafted to have the key of a rule of
  another target's probe.

A key is therefore built by one of a few constructors, in one form
(`subjectKey` in `failurelog.go`):

1. the kind of its subject, a letter: a probe (`probeFailureKey`), a static
   target's own failure (`staticTargetKey`), what a trip found at the
   address it went to (`failureKey`, `aspectKey`), or a metric that the
   static targets endpoint left out (`staticClashKey`);
2. each part of the subject, after its length and a NUL, so that a part ends
   where its length says, whatever it holds;
3. what of the subject failed, when that is not the subject itself: one of a
   fixed few aspects (`failureAspect`), or the marker of a rule's key with
   the rule's name and expression, each after its length, and its `items`
   (`appendRuleFailureKey`).

No two subjects have one key, whatever their parts hold:
`TestNoTwoSubjectsHaveOneKey` reads every generated key back as the subject
it was built from, which only a test does.

- What the log needs to know about a key is passed beside it and kept with
  the entry (`failureState`): the trip of a rule's failure, and whose
  failure it is (its collector, and the static target when the failure is a
  static target's own). A reload goes by that when it forgets
  (`forgetCollectorsLocked`, `forgetStaticTargetsLocked`), and a trip is
  held to it (`logStands`), so the two drop exactly what belongs to the
  collector and the static target
  (`TestForgettingDropsExactlyWhatIsTheCollectorsOrTheStaticTargets`).
- A new thing to remember gets its own constructor or aspect, with a line in
  the tables of those two tests, never a string appended to a key where the
  failure is reported.
- A trip's key is built once, in one allocation, and the key of an aspect
  with its aspect (`aspectKey`).

**No key per scrape.** A scrape with a rule's failure remembered builds no
key either, and no scrape builds one at all: the log does, when it first
remembers a failure.

- A scrape reports a rule's failure by what distinguishes the rule: its
  name, its expression and its `items` (`ruleFailedFor`). The log writes the
  bytes of the rule's key over the previous one's (`ruleLocked`), under its
  lock, finds the failure by them, and builds a string of them only to
  remember a failure it did not know (`failedOf`). That happens once for a
  rule that fails on every scrape, and never for a failure that the full
  log does not remember; such a failure is logged in full on every scrape
  for as long as the log stays full, and would otherwise have had its key
  built on every scrape.
- The log returns the key that the failure is remembered under, or none,
  which is what the scrape keeps of the rules that failed on it. A rule
  whose failure was not remembered is identified by the rule's parts
  (`failedAmong`), in case another scrape of the trip had the failure
  remembered meanwhile
  (`TestARuleRememberedByAnotherScrapeMeanwhileIsNotLoggedAsRecovered`).
- A debug probe, which the log is told nothing about, asks for the key that
  an earlier scrape's failure is remembered under (`rememberedRule`), and
  builds none.
- A scrape asks about each of its rules only when a failure is remembered of
  a rule other than those that failed on it (`remembersRules`). A scrape of
  a target that lacks a value, on which the same rule fails every time,
  never does.
- The bytes of a key are built in one place (`appendRuleFailureKey`), so a
  failure is looked up by what it is remembered under.

**Measuring it.** What the log costs a scrape is measured with
`go test -run '^$' -bench 'LogRuleFailures' ./internal/exporter/`, where
`/every_key` is the scrape as it was while it built every rule's key, and
`/full_log` is the scrape whose rules fail while the log is full of other
targets' failures.
`TestAScrapeWithARuleRememberedAllocatesNothingOfItsExpressionsSize` holds a
scrape with a failure remembered, and
`TestAScrapeWhoseRulesAFullLogDoesNotRememberAllocatesNothingOfItsExpressionsSize`
one whose failures the full log does not remember, and a debug probe's, to
allocating the same for expressions of 200 bytes and of 20 KB. A rule's
failure reported with its key built beforehand is kept for the tests
(`failedUnderKey`), and
`TestARulesFailureIsLoggedAndRememberedByItsPartsAsUnderItsKey` holds the
two to the same lines and the same entries, including with a full log.

### Adding a request type

```sh
make new-request-type NAME=memcache                          # a type of every build
make new-request-type NAME=memcache OPT_IN=1                 # an opt-in type
make new-request-type NAME=mqtt OPT_IN=1 GO_NAME=MQTT        # its Go name given
make new-request-type NAME=uuid GO_NAME=UUID ARTICLE=a        # its article given
```

runs `tools/new-request-type.sh`, written in POSIX `sh`, `sed` and `awk` as
macOS has them. Before it writes anything, it refuses a name that is not
lowercase letters and digits starting with a letter, or that is already a
type in the tree.

- **`GO_NAME`** is how the type's Go identifiers spell the name:
  `RequestType<GoName>`, `<GoName>Config` and its tests' names. It is the
  name with the letters of an initialism in capitals, as Go writes them,
  such as `SSHExec` for `sshexec`. When left out, it is the name with its
  first letter in capitals, `Memcache`. A Go name that is not the name's
  letters, in their order, or that does not start with a capital, is
  refused.
- **The article.** The generated comments, docs and test names use the
  article that the name is read with ("an `ntp` collector",
  `TestAnNTPCollector…`, "a `dns` collector"). The script works it out from
  the name and its Go name. An initialism, a Go name with two or more
  capitals at its start (`NTP`, `S3File`), or a name without a vowel
  (`smtp`) is read letter by letter, so "an" goes before a letter whose name
  starts with a vowel sound: a, e, f, h, i, l, m, n, o, r, s and x. A word
  (`imap`, `redis`) is read as one word, so "an" goes before a, e, i, o and
  u. `ARTICLE=a` or `ARTICLE=an` sets it where that reading is wrong, as for
  `uuid` written without its Go name.
  `sh tools/new-request-type.sh --article <name> [<GoName>]` prints the
  choice without writing anything.

For a new name it writes:

- `internal/fetch/requesttype_<name>.go`, under the type's build constraint
  (only the tag of an opt-in type), with the registration: a
  `<name>Requester` whose `Validate` and `CheckTarget` accept anything and
  whose `Fetch` answers `the <name> request type is not implemented yet`; an
  empty `<Name>Config` given as `Config`, with its `KeyRules`; empty
  `Fields`; the `timeout` and `param_<name>` that every type takes as
  `Overrides`, and `timeout` as `TargetFields`; an `UnusedParams` that names
  only the label values; and every other hook and setting at its zero value,
  each with a comment saying what it is.
- The tests, under the same constraint:
  - `internal/fetch/requesttype_<name>_test.go`, whose test checks that a
    collector of the type validates and that its scrape fails as not
    implemented, and, for an opt-in type, checks its `optInRequestKeys`;
  - `internal/exporter/requesttype_<name>_test.go`, a probe of a collector
    with a python transform that fails in the same way;
  - the type's table of schema rows, empty, with its check
    (`test/repository/schemakeys_<name>_test.go`, and for an opt-in type its
    rows in `schemakeys_<name>rows_test.go`, which the default build
    compiles), added to the tables that
    `TestEveryConstrainedKeyOfTheSchemasIsInATable` reads.

  They use the helpers that every type's tests share, which a lint of the
  type built alone would otherwise report as unused.
- `docs/<NAME>.md`, with the sections of the other types' pages: Building
  with `<name>` for an opt-in type, A collector with its keys' table, The
  target, Static targets, Limits, Errors and self-metrics, Limitations and
  Troubleshooting.
- `docs/SPECIFICATION-<NAME>.md`, the type's section of spec § 5.1 in its
  own document, opened like the other types', with a `TODO(<name>)` for its
  rules.

It also adds the type to every list that names each type:

- its `RequestType<Name>` constant, `knownRequestTypes` and, for an opt-in
  type, `optInRequestTypes` in `internal/fetch/requesttype.go`;
- the guard of `requesttype_none.go`;
- the Makefile's loops over the types and its builds of every opt-in type,
  and ci.yml's `request-types` matrix, its build and test of every opt-in
  type, and their lint's tags;
- for an opt-in type, the constraints that name every opt-in type
  (`optinkeys_test.go`, `optinkeys_check_test.go`,
  `schema_fullbuild_test.go`);
- the lists that the documentation marks `<!-- request-types -->` (a row in
  a table of the types, and a sentence in the others), and the build
  information's list of a default build;
- the README's index and its line of the types' specifications, the related
  pages of `docs/CONFIGURATION.md`, spec § 35's list of pages and § 5.1's
  list of the types' specifications;
- for a type of the default build, every copy in the documentation of the
  sorted list of a default build's types that the exporter writes in its
  messages (`it was built with: dns, graphite, …` in the opt-in types'
  pages, `write one of dns, graphite, …`);
- the tables of the repository and fetch tests that hold a value for each
  type: the words that the READMEs name it with (`readmeSourceNames`,
  `chartSourceNames`), its refusal of an unused probe parameter
  (`unusedRefusals`), and the pages whose Static targets section a test holds
  to the keys that a target may set, including its own page, whose section
  names `timeout`.

**Order.** Where it adds the type follows how each list is ordered.

- A list that the exporter or a script writes is sorted, and so are the
  Makefile's loops, ci.yml's matrix and the lists of the types'
  specifications.
- The documentation's lists of the types name those of the default build
  first and the opt-in ones after them, each group in the order the types
  were added. A table of the types (`docs/STATIC-TARGETS.md`), the related
  pages of `docs/CONFIGURATION.md` and spec § 35's list get a type of the
  default build after the last of that group, before the opt-in ones, and an
  opt-in type last.
- The README's index groups the pages by what they read, so the type's line
  goes after the last type's, its title and words a `TODO(<name>)` that says
  to move it beside the types it is like.
- The sentences in a marked list get the type last, with a `TODO(<name>)` to
  write it in its place.
- `knownRequestTypes` and the opt-in lists of the Makefile and ci.yml are in
  the order the types were added, with the new one last.

Then it runs `make schemas`, because the committed schemas list the types of
a default build. The lists that the tests read from the tree, the types of
`internal/fetch/requesttype_*.go`, need no edit: `tools/request-type-tags.sh`
finds a type by its file and its constraint, and so do the tests of the
opt-in builds and of the modules that each type links.

**Checks.** The tree then builds, vets, lints and passes its tests, in the
default build, with every opt-in type and with the new type alone, except
the tests of the README's and the chart's words for the type, which fail
until those are written (step 4 below).

- `TestANewRequestTypeIsWiredByTheScaffold` (`test/repository`) runs the
  script on a copy of the tree, for an opt-in type and a type of every
  build. It checks that the copy builds and vets in those builds, the
  generated tests, and the tests that compare the lists of the types. It
  also checks the article and the type's places in the lists, scaffolding a
  type of the default build read with "an" and an opt-in one read with "a".
- It needs a few minutes with a cold build cache, so it runs only in the
  default build, and not under `-short` or the race detector. CI runs it in
  its own job, `scaffold`, and `make test-scaffold` runs it alone.
- Every place left to a person says `TODO(<name>)`, and the type's file
  fails its scrapes with `the <name> request type is not implemented yet`.
  `TestNoScaffoldPlaceholderIsLeftInTheTree` (`test/repository`) fails while
  either is left for a type of the tree, so a half-written type cannot be
  committed.

The script ends by printing what is left to do:
`TestANewRequestTypeIsWiredByTheScaffold` (`test/repository`) runs the
script on a copy of the tree, for an opt-in type and a type of every build,
and holds the copy to building and vetting in those builds, to the
generated tests and to the tests that compare the lists of the types. It
needs a few minutes of a
cold build cache, so it runs in the default build only, and not under
`-short` or the race detector; CI runs it in a job of its own, `scaffold`,
and `make test-scaffold` runs it alone. It holds the article and the
places of the type in the lists too, scaffolding a type of the default
build read with "an" and an opt-in one read with "a". Every place left to
a person says `TODO(<name>)`, and the type's file fails its scrapes with
`the <name> request type is not implemented yet`;
`TestNoScaffoldPlaceholderIsLeftInTheTree` (`test/repository`) fails while
either is left for a type of the tree, so a type is not committed half
written. The script ends by printing what is left:

1. the logic, in the type's file: its keys and their `KeyRules`, `Fields`,
   `Overrides`, `TargetFields`, the `Requester`'s methods and the hooks the
   type has, then `make schemas` again;
2. its tests, in place of the not-implemented ones, and a schema row of
   each key the schema holds to allowed values, a pattern or a length;
3. its page and the TODOs of the marked lists, the indexes and spec § 35;
4. the README's table of sources (in its build's column), the chart
   README's opening and, for a type of the default build, `Chart.yaml`'s description, in the
   words it gave `readmeSourceNames` and `chartSourceNames`;
5. the prose that lists every type, below, which the script cannot write,
   since each sentence says what the types it names have in common;
6. its specification, `docs/SPECIFICATION-<NAME>.md`: the type's rules, as
   MUST rules, as the other types' specifications hold theirs;
7. examples, and a section of the chart README if the type is deployed
   with anything of its own.

`make ci` does not run `make new-request-type`, which changes the tree.

#### Prose that names the types

These sentences and comments name the request types in prose, each saying
what the types it lists have in common, so a new type is added by hand to
those that are true of it. Most have no test that fails when the type is
left out (the README's and the chart's words do: `readmeSourceNames`,
`chartSourceNames`).

- `README.md`: the table of sources under "What it can read", in its
  build's column and the row of what it reads, in the words of
  `readmeSourceNames`.
- `charts/prometheus-universal-exporter/README.md`: the opening paragraph,
  in the words of `chartSourceNames`, and the features list's line of the
  collectors other than `http`'s (a type of the default build before "and",
  an opt-in one after it); `Chart.yaml`'s `description`, for a type of the
  default build.
- `docs/CONFIGURATION.md`: the list after the `request.type` table of what
  each type does ("`dns` resolves a name; see [DNS](DNS.md).", …); under
  Choosing request types at build time, the libraries that each type links,
  or that it needs nothing outside the standard library.
- `docs/REQUESTS.md`: the types that `allowed_targets` and `denied_targets`
  bound, and those that refuse the cloud metadata service.
- `docs/SELF-METRICS.md`: the types that
  `http_exporter_targets_refused_total` counts, when the type takes the
  target lists.
- `docs/STATIC-TARGETS.md`: the types whose target's port is checked at load
  ("this applies to a `grpc`, `sshfile`, … target too"), and the row of the
  credentials table that the type belongs in.
- `docs/DEVELOPMENT.md`: the code layout's row for `internal/fetch`, and the
  types with a table of schema rows under the schema tests.
- `internal/fetch/requesttype.go`: the comment on the request types, about
  what each does.
- `docs/SPECIFICATION-EXPORTER.md`: § 1's bullet "Reach its sources itself",
  § 5.1's sentence of the types that are implemented and its rule of the
  types that import nothing outside the standard library, and § 26.1's
  opening, of the types that take the target lists.
- `test/repository/requesttypedeps_test.go` and `sqldriverdeps_test.go`: the
  builds whose linked modules are checked, for a type that needs nothing
  outside the standard library.
- For an opt-in type: the Makefile's comment on `OPT_IN_REQUEST_TYPES`,
  which names the opt-in types and their pages; the paragraph of
  `docs/CONFIGURATION.md` on the opt-in types under Choosing request types
  at build time, with how many there are and what they add to a binary
  together; § 5.1's sentence of the opt-in types in
  `docs/SPECIFICATION-EXPORTER.md`; and, for a type that reaches a server
  over TCP, the comments opening `internal/fetch/nettypes.go` and
  `nettls.go`.

### Adding a secret store

A [secret store](AUTHENTICATION.md#secret-stores) has a type, a value of
`secret_stores[].type`. Each type is one file,
`internal/fetch/secretstore_<name>.go`, that registers a
`fetch.SecretStoreType` from its `init`. Everything else is the same for
every type and lives in `secretstore.go`: the references, the cache, the use
of a secret past `cache_ttl`, the counts and self-metrics, the checks of a
configuration, and the schema. To add a type:

1. **The store.** Write a `SecretBackend`. `Read(ctx, ref)` asks the store
   for the secret at `ref.Path`, at `ref.Version` when it is set, within
   `ctx`, and returns a `SecretDocument`: `Text` for a secret that is one
   value, `Fields` for one that has several. It returns the whole secret,
   whatever field the reference names, and is called by several scrapes at
   once. `secretClient` builds its HTTP client, `secretHTTP` a request held
   to the answer's bound, and `secretStoreSaid` the masked text of a
   refusal.
2. **The registration.** `Name`; `Keys`, the keys of an entry that the type
   takes in addition to the five that every type takes; `Check`, its rules
   at load; `New`, its constructor; `FieldRequired` when its secrets always
   have fields; and `KeyRules`, each key's description for the schema,
   starting with the type's name.
3. **The keys.** A key that no type takes yet is a new field of
   `model.SecretStore`. A key that another type has is shared, and each type
   describes it.
4. **A type that some builds lack** registers in every build, as `aws` does,
   with a `Check` that says how to build with it, so that its name and keys
   are in every build's schema.
5. **Tests** against a stand-in of the store, as `secretstore_test.go` has
   for the others; `make schemas`; and a row in
   `test/repository/schemakeys_http_test.go` for a key that the schema holds
   to values or a pattern.
6. **Documentation.** Its section of
   [AUTHENTICATION.md](AUTHENTICATION.md#secret-stores), with an example
   that loads, its rule in § 42.6c of the specification, and a changelog
   line.

A test fails for a type without a constructor, with a key that
`model.SecretStore` has no field for, or with a key that it does not
describe, and for a page that leaves out a type.

## Running the tests

There are two tiers. While you write a change, run the quick one:

```sh
make test-quick                                   # every package
go test -short ./internal/fetch/                  # one package
go test -short -run 'Login' ./internal/fetch/     # the tests you are working on
```

It runs each test once, without the race detector, and leaves out two kinds
of test: the scaffold's test, which builds a copy of the tree, and every
test that calls `tier.Slow(t)` (`internal/testutil/tier`) as its first line,
which `-short` skips.

- A test is marked slow when it takes a second or more on its own: the tests
  that compare a parser with an oracle over many generated inputs, render
  the chart with helm, or run a schedule on the real clock.
- About two hundred of some 4,400 tests are slow. The quick tier takes a few
  minutes, where `make test`, which runs the suite three times, twice under
  the race detector, takes far longer.

The quick tier is not a gate. Before you push, run `make test`, or `make ci`
for everything that CI runs. CI runs every test, including the slow ones, on
every change, and neither `make test` nor any workflow passes `-short` (a
test checks this). A new test that takes a second or more starts with
`tier.Slow(t)`; a fast one leaves it out, so that the quick tier still
covers what it covers.

## Repeatable tests

Every test must pass however many times it runs and in whatever order.
`make test` checks this with
`go test -race -count=2 -shuffle=on -timeout 20m ./...`, and CI with the
same command over each side's packages (see the CI section below): twice on
a push to `main` or of a tag, and once on a pull request, where the two race
jobs were the longest of a run.

- A failure there names the seed that it used; run it again with
  `-shuffle=<seed>`.
- The time limit is per package. The command names it because `go test`'s
  own ten minutes are meant for one run without the race detector. A test
  keeps the Makefile and `ci.yml` running the same command.

**Refused connections.** A test that needs a TCP address where nothing
answers uses `testutil.RefusingAddress`: a socket bound to the address that
never listens, so every connection is refused immediately and no other
socket can get the port while the test runs. The address of a closed
listener is no substitute. The next server that asks for any free port may
get it, and a test that waited for a refused connection then reached that
server instead; a redirect to it once went round ten times in CI.

**Shared state** is what breaks repeatability. The Python worker pool is one
example: its counts (starts, runs, stops, idle workers) would carry over
from test to test.

- A test that runs Python calls `requirePython(t)`, which also gives it its
  own pool and stops that pool's workers when the test ends. A test that
  uses the pool without an interpreter calls `usePythonPool(t)`.
- Tests therefore must not use `t.Parallel`, which the swap assumes. The one
  exception is `test/repository`, which swaps no pool: its tests that render
  the chart, each of which spends most of its time waiting for `helm`, are
  made parallel by `requireHelm(t)`, and run beside each other once the
  package's other tests are done.

What the pool counts for a collector belongs to the collector only while
the collector exists
([self-metrics](SELF-METRICS.md#python-workers)):

- A run counts in the statistics that its trip took for the collector it
  read (`transform.PythonStats`, passed on with the script timer), and a
  worker in those of the run that started it.
- A reload that removes the collector retires them (`PythonPool.Retire`,
  called where the exporter drops the collector's own statistics).
- A worker belongs to its statistics for as long as it lives, although the
  pool keeps workers by the collector's name and script, which a re-added
  collector has too. `acquire` gives a run only an idle worker of the
  statistics that the run counts in, and `Retire` and `release` stop the
  workers whose statistics are retired, so none is idle for a re-added
  collector to be given.
- Nothing in the pool therefore counts by a collector's name. A new place
  that counts takes the run's or the worker's statistics and counts through
  `countedLocked`, which sends what is retired to the pool's own count.
- A test of what is counted, and under which name, needs no interpreter.
  `scriptWorkers` and `fakeWorker` (`internal/transform/pythonstats_test.go`)
  stand in for workers whose start and script the test holds and releases.
  `noInterpreter` (`internal/exporter/pythonstats_http_test.go`) is a
  `python3` that passes the load-time check and fails every worker's start,
  which is counted like any other.

**Allocations** are shared in the same way. `testing.AllocsPerRun` and
`runtime.MemStats` count the allocations of every goroutine, not only those
of the function that a test measures. A test server still closing its
connections, a timer, a worker of the Python pool and the garbage collector
itself (which starts its workers at the first collection of the process and
empties `sync.Pool` at every collection) allocate meanwhile, more so under
the race detector, in a shuffled run and on a busy machine. A bound of 4
allocations for a function that makes 1 has seen 12 in CI, and a test that
ran first in its process counted the collector's workers as its own.

The other goroutines can only add to a count, never subtract from it. A test
that bounds or compares allocations or allocated bytes therefore measures
them only through `internal/testutil/alloctest`, which measures several
times and takes the minimum, with a collection before each measurement:

- `AllocsAtMost` and `BytesAtMost`, for a test with a bound, stop at the
  first measurement within the bound, so a quiet machine pays for only one;
- `Allocations`, for a count without a bound (one that another count is
  compared with exactly), is the minimum of five;
- `Once` is a single measurement, for a cost of which a test takes only a
  fraction to bound another, and which takes too long to measure five
  times.

A test (`TestAllocationsAreMeasuredOnlyThroughAlloctest`) fails for a test
file outside `internal/testutil` that calls `testing.AllocsPerRun` or reads
the allocation counters of `runtime.MemStats` or of a benchmark's result
itself.

**Stack size** is also counted for the whole process, and depends on the
garbage collector as well. A goroutine that outgrows its stack is given a
larger one, and the old one is released immediately only while no collection
is running. During a collection it is kept until the collection ends, so a
call that ended before the collection did was measured with every stack it
had outgrown in addition to its current one: 24 MB and 32 MB for a stack of
16, on a machine busy enough for a collection to last from the stack's last
doubling to the end of the call. That was a failure of `internal/decode`
that nobody could repeat on a quiet machine.

A test that bounds a stack therefore measures it with no collection running,
as `yamlStackOf` does (`yamlkeysdepth_test.go`): it turns the collector off,
finishes the collection in progress, runs the call on its own goroutine, and
turns the collector back on. The measurement is then the goroutine's stack to
the byte (16 MB, 4 MB or nothing), whatever else the machine does.

The race detector changes what is allocated. Under it, `sync.Pool`, in which
`fmt` and the regexp and XPath engines keep their working memory, returns
only some of what it is given, and some allocations are larger. A bound that
is tight in a plain build can simply be wrong there. Measure a new
allocation test under `-race` too. Where its bound does not hold there with
room to spare, the test skips its allocation assertions when
`alloctest.RaceDetector` says that the detector is on
(`t.Skip("the race detector changes what is allocated")`, or a return before
the counts where the test checks other things as well), and its comment says
so.

**Time** is the other thing that breaks repeatability. CI runs the suite
under the race detector on two cores, with another package's tests running
beside it, and there a millisecond's work has taken seconds. A test
therefore waits for the event it is about, never for a length of time: it
polls a condition with `testutil.WaitFor`, or reads a channel that a hook or
its test server closes, under a bound of half a minute that only ends a
hang.

- Where a test has to run against one of the exporter's own timeouts, choose
  the values so that a slow machine makes the test slower and never fails
  it. What must not happen is given a long time not to happen in, and what
  must happen has no bound other than the hang's.
- A test does not assert that something took less than some time. It
  asserts an order, a count, or that something took at least a given time.
- A sleep is for a test server's own slowness, for a clock that has to read
  later, or for giving something time not to happen. It is not a way to let
  something else get ahead.
- The two tests that hold the static target loop to its interval on the real
  clock (`statictargetcadence_test.go`) are the exception: they compare a
  time with a multiple of the interval, each next to a test of the same
  schedule on its own clock.

Two helpers apply that rule to what many tests share.

- **Script timeout.** A Python script has a minute in every test, whatever
  its `limits.script_timeout`. `TestMain` in the packages that run scripts,
  and `usePythonPool` on each fresh pool, set it
  (`PythonPool.SetLeastScriptTimeout`), because a script of a millisecond
  has overrun the default 100ms on a busy machine. A test of the timeout
  itself calls `holdScriptsToTheirTimeout(t)`, and its script never ends, so
  the timeout is what stops it however slow the machine is.
- **Interpreter start.** An interpreter also has a minute to start in, where
  the exporter gives it ten seconds, which one start on a busy machine has
  exceeded. The same two places set it (`PythonPool.SetStartTimeout`), and
  `holdScriptsToTheirTimeout` leaves it, because a test of the script's
  timeout starts an interpreter like any other. A test of the start's own
  limit sets a short one and starts a stand-in for `python3` that never
  reports that it is ready (`pythonstart_test.go`).
- Neither setter may be called outside a test, which a repository test
  checks (`TestOnlyTestsGiveScriptsALeastTime`).

**gRPC ports.** A test's gRPC server never listens on a port that another
had in the same process (`grpctest.Start`, and `grpctest.Listen` for
anything else that a test has the exporter call as a gRPC target). The
exporter keeps a connection per address and a reflection answer per
connection for minutes, and the kernel hands out a freed port again, so a
server on a reused port was answered for by what was kept from the previous
one. A test of a server that comes back gives the connection an hour to wait
by itself and the call a minute to connect in (`connectionsWaitAnHour`,
`callsWaitForAConnection`), so that only what the test is about reconnects,
however long it takes.

**TLS handshake and request headers.** Two more of the exporter's
ten-second limits apply to tests that are not about them:

- the TLS handshake with a target, for every test of an `https` target (15
  tests of `internal/fetch` and 20 of `internal/exporter` fail when a
  handshake has a nanosecond);
- the time that a client has to send a request's headers in, for every test
  that calls the exporter's HTTP server over a real connection: four tests
  of `internal/exporter`, and the eight of the main package that run the
  exporter in a child process.

Both are half a minute in the tests, the bound of a hang. `TestMain` of
`internal/fetch` and of `internal/exporter` sets the first
(`fetch.SetTLSHandshakeTimeout`), and `TestMain` of `internal/exporter` and
the child process of the main package's tests set the second
(`exporter.SetReadHeaderTimeout`).

- A test of either limit sets a short one against something that never
  ends: a target that accepts the connection and never answers the
  handshake (`internal/fetch/tlshandshake_http_test.go`), and a client that
  never ends its headers (`TestAClientThatNeverEndsItsHeadersIsDropped`).
- Each also reads the limit where the code put it (the pool's
  `TLSHandshakeTimeout`, and the read deadline on the exporter's side of the
  connection), so that a limit other than the one set fails the test
  immediately and not by its length.
- The exporter's own values are asserted with nothing set
  (`TestATLSHandshakeHasTenSecondsInTheExporterAndHalfAMinuteInTheTests`,
  `TestHTTPServerTimeouts`), and neither setter may be called outside a test
  (`TestOnlyTestsGiveScriptsALeastTime`).

**gRPC connect.** The twenty seconds that an attempt to connect to a grpc
target has (`grpcConnectTimeout` in `internal/fetch/grpcconn.go`: the TCP
connection, the TLS handshake and the server's answer to the HTTP/2
preface) are such a limit too, for every test that calls a grpc server: 22
tests of `internal/fetch` and 6 of `internal/exporter` fail or hang when an
attempt has a nanosecond, and a server whose preface came 21 seconds late
failed `TestGRPCHealthNeedsNoDescriptors`.

- It is half a minute in the tests as well, set by `TestMain` of both
  packages (`fetch.SetGRPCConnectTimeout`, next to the handshake's setter in
  `internal/fetch/transport.go`, where a build without the grpc request type
  has it too). The main package's child process calls no grpc target. The
  waits between attempts (`grpcBackoff`) keep the exporter's values.
- The test of the limit sets 200 milliseconds against a target that accepts
  the connection and never answers the preface
  (`TestAGRPCTargetThatNeverAnswersThePrefaceIsGivenUpAtTheConnectLimit`),
  and shortens the wait before the next attempt with it. grpc-go gives an
  attempt the longer of the two, so under the exporter's wait of a second,
  the second would be what the attempt gets, and under the hour of a test
  that makes a connection wait before it retries (`connectionsWaitAnHour`),
  an attempt gets the hour, whatever the limit.
- What a connection is made with when nothing is set, the exporter's values
  one for one, is asserted
  (`TestAnAttemptToConnectHasTwentySecondsInTheExporterAndHalfAMinuteInTheTests`),
  and the setter is restricted to tests like the others.
- The other limits of a grpc call are not under half a minute: a reflection
  query has thirty seconds, the dialer of a collector with a target policy
  thirty, and a call its request's own timeout, and no keepalive pings are
  sent. The second that a call waits for a connection that had failed
  (`reconnectWait`) decides no test that is not about it: with a nanosecond
  two tests fail, both about the wait itself and on their own clock
  (`synctest`), and the tests of a server that comes back give it a minute
  (`callsWaitForAConnection`).

**Deadlines** are where a test most easily waits for a length of time:
something has to have happened when the deadline passes, and a deadline on
the machine's clock allows exactly that long. Three approaches keep the
clock out of it, in this order of preference:

- **The test ends the deadline itself**, once it has seen what had to come
  first. `testutil.DeadlineEndedByHand` gives a context that ends like one
  whose deadline has passed, when the test says so.
  - The tests of a directory read that is cut short end it once the hook that
    holds a file's read has been called (`localfile_reads_test.go`).
  - A test whose reads or runs have to overlap holds each until all of them
    are there, where a sleep in each only made it likely
    (`TestLocalDirectoryReadsFilesConcurrently`, `leaveIdleWorkers`, and the
    burst of `TestPythonWorkersAreCappedProcessWide`, whose two starts wait
    until the other four runs wait for a worker).
  - What a test's server does once the client has read something, it does
    when the client has read it. The target that resets a connection in the
    middle of an answer waits for the client's side of the connection to have
    read the start (`startRead` in
    `internal/fetch/samefailure_http_test.go`), where it used to sleep a
    second. A test server that answers after the probe has given up is held
    until it has (`grpctest.Options.ReflectionHold`, the relay's
    `holdAnswers`), not for a length of time.
- **The code runs on the test's own clock** (`testing/synctest`), where
  nothing tells the test that the earlier step is over. That clock stands
  still while any goroutine of the test is working or waiting for the
  network, and moves on when all of them wait for the clock or for each
  other. A deadline on it passes when the fetch has got as far as it can by
  itself, and the test asserts the time taken to the nanosecond
  (`onItsOwnClock` in `internal/fetch/fetcher_test.go`,
  `TestGRPCEachRetryOfACallWithoutAConnectionDialsAgain`, the stand-in
  workers of `internal/transform`).
  - What the test talks to is started outside that clock, because a server
    waits for connections for as long as it runs and would hold the clock.
  - An HTTP target answers with `Connection: close`, because a connection
    kept open is one still being read from.
  - What would outlive the test with the clock's time or timers in it (a
    connection pool, a gRPC connection) is kept in the test's own cache, and
    the gRPC connection is closed before the clock is left.
  - Code that only computes never lets such a clock move, so this is not for
    a jq program or a real Python script.
- **A short deadline on the machine's clock, retried**, where neither is
  possible, as with a real interpreter or a jq program that must be running
  when the deadline passes. The test looks at how the run ended: when it
  ended because the earlier step had not been reached, the test runs it
  again with twice the time, up to about sixteen seconds
  (`TestRuleFailuresBeforeTheDeadlineAreKept`,
  `TestPythonRunCutByTheProbesDeadlineIsNotATimeout`, and the alarm of
  `TestPythonWorkerKilledByItsOwnAlarmIsReplaced`). A slow machine pays for
  the longer runs, and a quiet one only for the first.

A context that only keeps a test from hanging lasts a minute, like the
deadline of a probe that must be answered. Ten seconds, and five, were what
a request that must succeed had on a machine that once took longer than
that to start an interpreter.

**The exporter's own limits** are handled the same way wherever a test runs
against one. What is meant to get through has half a minute, the bound of a
hang, instead of the exporter's seconds:

- a static target scrape that a test makes itself (`scrapeStaticTargets`
  with a budget of 0);
- an OTLP export and each of its attempts (`otlpConfig`, whose `timeout` is
  half a minute);
- a probe with a `timeout` parameter;
- a target of the scrape loop, whose interval ends its scrape and is
  therefore minutes long, with a name whose first scrape is due immediately
  (`soonScraped`).

Other cases:

- **A budget within which something has to happen first** (the target has to
  answer, for the deadline to pass inside a rule or in the wait before a
  retry; an attempt has to end, for there to be time for another). The test
  starts with a short budget and repeats the round with twice the budget and
  a new exporter, up to half a minute, for as long as the round itself shows
  that the budget ran out too early
  (`TestADeadlineInsideARuleFailsTheProbe`,
  `TestAProbeWhoseRetryRunsOutOfTimeReportsTheTargetsAnswer`). What shows
  that must be something that no failure of what the test is about can look
  like. `TestUnreachableOTLPEndpoint` repeats a round without a retry, which
  is what it is about: an exporter that does not retry makes none in any
  round, immediately, and fails the test at the last round.
- **A cached result** that a second probe is to find is not left to be found
  within its `cache.ttl` on the machine's clock. The test reads the ttl from
  the entry and moves the entry ahead (`ageEntries` with a negative age), as
  it moves one back to expire it
  (`TestThePrometheusDemoExampleAnswersARepeatedScrapeFromMemory`, whose
  example keeps a result for fifteen seconds).
- **Static target scrapes during something else.** A test that wants scrapes
  of static targets while something else happens makes them itself
  (`scrapeStaticTargets`) beside the loop. The loop looks for a reload when
  a scrape is due and once a second besides, and a target that a reload
  restarts before its first scrape was due waits again, so reloads a few
  milliseconds apart leave the loop one scrape or two
  (`TestProbesScrapesAndReloadsTogetherLeaveOnlyWhatIsInForce`).
- **The shutdown delay.** A test that has the exporter do something during
  its `--web.shutdown-delay` runs it in a child process that stays in the
  delay, once it is over, until the test releases it
  (`exporterProcess.endDelay` in `shutdown_http_test.go`). The child replaces
  the wait, `waitOutShutdownDelay` in `main.go`, which is a variable for that
  purpose only and which only a test may set
  (`TestOnlyTestsHoldTheShutdownDelay`).
- **A configuration watch tick that reloaded nothing.** Such a tick logs
  nothing, so the test has nothing of the exporter's to wait for. Its child
  reports when each tick is over, after whatever the tick logged
  (`config.SetWatchTicked`, set by the child for `helperSaysTicksEnv` in
  `shutdown_test.go`, and restricted to tests by
  `TestOnlyTestsGiveScriptsALeastTime`), and the test reads the child's
  lines in order (`watchstartup_grpc_test.go`). What that test has the
  child's interpreter wait on is a named pipe whose two ends the test holds,
  not a file polled in a loop, and the child leads a process group that the
  test ends, so a test that fails midway leaves nothing running.
- **A deadline that the exporter sets on a connection** is read from the
  connection, not waited out (`notedConn` in
  `internal/exporter/answerwrite_test.go`).

What a test leaves behind in its own server follows the same rule. A client
that was stopped while it dialed leaves connections whose handshake the
server reads later. A test that counts what its server sees therefore ends
them first, on the server too: `namedServer.done` in
`internal/fetch/wirename_test.go` closes what the test's dialer made and
waits for the server to have closed each. A handshake read late was counted
with the next case's, and occasionally failed that case with the previous
case's name.

**Time under the race detector** is limited in the tests too. Code runs
several times slower there and the command runs every test twice, so a test
over tens of thousands of generated documents, or a body of megabytes, costs
minutes there that it does not cost a plain run. Two packages once took most
of the ten minutes that `go test` gave them.

- A test whose time depends on the size of its input names both sizes with
  `alloctest.UnlessRaced(plain, raced)` (a count of generated cases, the
  length of a body, the rounds of a loop, one row in so many of a table that
  is multiplied out), or branches on `alloctest.RaceDetector`
  (`raceDetector` in the packages that have the constant).
- The plain run keeps the size that makes the test thorough. The run under
  the detector uses a smaller one of the same kind and checks everything
  that the plain run checks: no test is skipped there and no assertion left
  out. A text or count expected of the larger input is computed from the
  size, and a floor on what a generated corpus held (so many documents
  refused, so many with an alias) is scaled with the corpus and never
  dropped.
- Where a test is about a bound in the code, the smaller input still
  exceeds it. A test of concurrency keeps its goroutines and reduces rounds.
- Where an input cannot shrink, because it is at a bound, and the slow part
  is an oracle that the test compares with, the result under the detector
  may be checked against the answer that the oracle is known to give, as
  long as the plain run asks the oracle itself
  (`yamlkeysaliasdepth_test.go`, where the YAML library takes seconds over a
  chain of aliases at the depth limit).
- A test that waits on the clock or on a Python process does not get faster
  this way and is left as it is.

Aim for a second under the detector. To see where a package's time goes:

```sh
go test -race -count=1 -json ./internal/decode |
  jq -r 'select(.Action == "pass" and .Test != null and (.Test | contains("/") | not)) | "\(.Elapsed)\t\(.Test)"' |
  sort -rn | head -20
```

**Python versions.** The Python tests run `python3` from `PATH`. Every
workflow that runs the suite (CI, the exporter release and the Dockerfile
update) installs the Python that the image ships, the Dockerfile's
`PYTHON_VERSION`, with the libraries that the image ships at the
Dockerfile's versions (`lxml`, `PyYAML`, `python-dateutil`; PyYAML is also
what `tools/check-manifests.py` reads with), and a test keeps each workflow
reading them.

- Run them with that version locally too. The sandbox depends on what the
  standard library imports, and that changes between releases: from 3.12,
  `zoneinfo` loads `sysconfig`, which imports the blocked `threading`, so a
  sandbox change can pass on 3.11 and fail in the image.
- The tests in `internal/transform/pythonversions_test.go`, and the tests of
  how a script's error is cut in `scripterrorcut_test.go`, also run the
  worker with `python3.12` and `python3.13` from `PATH`, each where it is
  installed, and skip the other with a message saying it is not, which
  `go test -v` shows.
- To run the whole suite with another interpreter, put a `python3` that is
  that interpreter first on `PATH`
  (`mkdir -p /tmp/py && ln -s /usr/bin/python3.13 /tmp/py/python3 &&
  PATH=/tmp/py:$PATH go test . ./internal/transform/ ./internal/exporter/`).
  Those three packages pass this way with 3.11, 3.12 and 3.13, and a test of
  a library that the interpreter lacks is skipped with what is missing.
- A test that expects the text of a traceback takes it from the interpreter
  it runs: from what the previous worker writes for the same script there
  (`formerFailure`), or, for a literal example, without the markers that
  3.13 draws under a source line (`withoutMarkers`, on both sides). A new
  test should do the same instead of holding one release's text.

**Test-only Python modules.** Some tests use a Python module that the image
does not ship. The tests named `TestTheStrictOpenMetricsParser…`, and the
self-metrics tests that end in a subtest `the strict reference parser`, pass
the exporter's OpenMetrics answers to the strict parser of
`prometheus_client`. The module is pinned in
`test/python/requirements.txt`, the one place for modules that only the
tests use:

```sh
python3 -m pip install -r test/python/requirements.txt
```

Where `python3` does not have the module, those tests are skipped with a
message saying what is missing, so the suite runs on a machine without it.

- The workflows that run the suite (CI, the release and the Dockerfile
  update) install the file and set `STRICT_OPENMETRICS_PARSER=required` for
  the job. With that variable set, to any value, a missing module or a
  missing `python3` fails those tests instead, so a green run there has run
  them. Set it locally to check the same.
- The rules that the parser checks are also written out in Go next to the
  tests (`strictOpenMetricsError`) and checked by tests that run everywhere;
  the module confirms that they are the parser's.
- Dependabot proposes new versions of the pinned module, and CI runs the
  suite on a change to the file.

## Tests of a build with only some request types

A build can include only some request types
([Choosing request types at build time](CONFIGURATION.md#choosing-request-types-at-build-time)),
and such a build's tests have to pass as the default build's do.
`make test-request-types` runs the suite once for each type built on its
own. CI runs the same over the exporter's packages, each type in its own
job, and `make ci` includes it. One of them by hand:

```sh
go test -tags "$(sh tools/request-type-tags.sh localfile)" ./...
```

A test that needs a request type says so with a build constraint on its
file, the same one that the type's own code carries:

```go
//go:build !select_request_types || request_type_http
```

That applies to every test whose configuration names the type (`type: http`,
or `testutil.MinimalConfig` and `testutil.Collector`, which are http
collectors), that starts a stand-in for a target of the type, or that
asserts what only a build with the type does, such as a probe parameter of
http's being refused for a collector of another type.

- A test that needs two types names both,
  `!select_request_types || (request_type_http && request_type_localfile)`,
  as the tests that load `configs/config.example.yaml` do.
- A test that holds only with every type, such as the comparison of the
  committed schemas with what the code generates, carries
  `!select_request_types` alone.
- A test is never skipped at run time for a type that the build lacks: the
  constraint states what the test needs where it can be read, and vetting a
  single-type build compiles exactly the tests that the build runs.

A test of one type that contrasts it with another is split into two tests,
so that the type's own build runs what is about the type.
`TestGRPCValidation` checks what a grpc collector's request must be and runs
wherever grpc is built. The row that was its contrast, an http collector
refused `retry.codes`, is a separate test in a file with the constraint that
row needs.

- That constraint is the other type alone where the row only configures the
  other type (`TestTheKeysOfOtherTypesDoNotApplyToHTTP`).
- It is both types together only where the assertion holds with both built
  and not otherwise, as for a `method` probe parameter refused for a
  graphite collector: without http in the build, `method` is a parameter
  that no type knows (`TestAGraphiteProbeRefusesAProbeParameterOfHTTPs`).
- One row that needs a second type never puts the whole test behind a
  constraint of two types.

Tests that hold whatever the build includes stay in a file without a
constraint, so every build runs them.

- Where a file's tests are of both kinds, those that need a type are in a
  file next to it named for the type: `cache_test.go` and
  `cache_http_test.go`, `requesttype_grpc_test.go` and
  `requesttype_grpc_http_test.go`.
- A helper lives where everything that calls it is compiled:
  `helpers_test.go` for every build, `helpers_http_test.go` for a build with
  http, `helpers_http_or_localfile_test.go` for a build with either.
- A helper compiled into a build in which nothing calls it is reported as
  unused by the linter when it runs with that build's tags
  (`golangci-lint run --build-tags "$(sh tools/request-type-tags.sh grpc)"`),
  which `make lint-request-types` does for every type, and CI in each type's
  job.

### Opt-in request types

Every request type in the tree is in the default build. A type may instead
be opt-in, for a type whose libraries few deployments want in the binary.
`make new-request-type NAME=<name> OPT_IN=1` makes one, which lives in its
`internal/fetch/requesttype_<name>.go` under its own tag only,

```go
//go:build request_type_<name>
```

as do the files that only it needs and its tests. A build therefore includes
it only when its tag is given.

- `sh tools/request-type-tags.sh '' '' <name>`, from the third argument,
  `OPT_IN_REQUEST_TYPES`, adds it to the default build's types, and
  `REQUEST_TYPES` may name it like any type.
- It is listed in `optInRequestTypes` (`internal/fetch`), and a build without
  it refuses a configuration that names it, with an error that says how to
  build one with it.
- Each type built on its own includes it, in the loops of
  `make build-request-types` and `make test-request-types` and in CI's matrix
  over the request types.
- The build with every opt-in type next to the default build's types is
  vetted, built, tested (`internal/fetch` and `test/repository`) and linted
  by `make build-opt-in-types`, `make test-opt-in-types` and
  `make lint-opt-in-types`, as CI does, and a test keeps their lists naming
  every opt-in type in the tree. With none, the lists are empty, and the
  targets and CI's steps say so and stop.
- The default build's tests know an opt-in type's request keys without the
  type (`optInRequestKeys`, `internal/fetch`). Its schema rows are compiled
  in the default build, which checks that every constrained key of the
  schemas has a row, and compared with the exporter's verdicts against the
  schema that the binary generates in a build with the type
  (`built: true`), because the committed schemas, the default build's, do
  not have the type.

### The sql request type

The `sql` type's drivers each live in their own file,
`internal/fetch/sqldriver_<name>.go`: `mysql`, `postgres` and `sqlserver`
under the type's constraint, and `oracle`, `clickhouse`, `snowflake` and
`vertica` under the type's constraint and their own tag,

```go
//go:build (!select_request_types || request_type_sql) && sql_driver_oracle
```

which `tools/request-type-tags.sh` produces from its second argument, the
`SQL_DRIVERS` list: `sh tools/request-type-tags.sh '' oracle,snowflake`.
With `snowflake` it also adds gosnowflake's `minicore_disabled`, which keeps
its native library out (a test of the snowflake driver fails in a build
without it).

- A test of an opt-in driver carries its tag too, and runs in the build with
  every opt-in driver. `make build-sql-drivers` and `make test-sql-drivers`
  vet, build and test that build, and `make lint-sql-drivers` lints it with
  golangci-lint (no other lint run compiles the drivers, and a build with
  more packages can give the analyses more to see). CI runs them, and
  `make ci` includes them. A test keeps their lists naming every opt-in
  driver in the tree.
- That build's snowflake driver takes a few minutes to compile the first
  time.

The tests of the query path reach the stand-in database of `internal/sqltest`
through the type's pools and connectors, with only the driver replaced
(`sqlConnectHook` in the package, `fetch.UseSQLTestDriver` from others), and
wait for the pools' events (`sqlPoolEvent`), not for time.

The tests against real databases (`TestLiveSQL`, `internal/fetch`) are
skipped unless a variable names a database (`PUE_TEST_POSTGRES`,
`PUE_TEST_MYSQL`, `PUE_TEST_SQLSERVER` as `host:port`, each with `_USER` and
`_PASSWORD`), and `make test-sql-live` runs them. CI does not run them: its
`sql-live` job, which ran them against PostgreSQL, MySQL and SQL Server
service containers, is commented out in `.github/workflows/ci.yml` for now.
Locally:

```sh
docker run -d -p 5432:5432 -e POSTGRES_PASSWORD=pw postgres:17
PUE_TEST_POSTGRES=127.0.0.1:5432 PUE_TEST_POSTGRES_USER=postgres PUE_TEST_POSTGRES_PASSWORD=pw make test-sql-live
```

`FuzzVerticaStatementSplitting` (`internal/fetch`, under the vertica tag)
checks the refusal of a `vertica` statement with several parts against the
driver's splitting, against `verticaParts`, and against a separate reading
of the server's rules written in the test, on any text and on two statements
joined by a `;`. Its seed corpus, the cases of the vertica statement tests,
runs as a test in the build with every opt-in driver, as the run of
`./internal/fetch/` by CI and by `make test-sql-drivers` does. To fuzz it:

```sh
go test -tags "$(sh tools/request-type-tags.sh '' vertica)" -run '^$' \
  -fuzz FuzzVerticaStatementSplitting -fuzztime 60s ./internal/fetch/
```

A failing input is written under `internal/fetch/testdata/fuzz/`. Add it to
the seeds in the test (`verticaFuzzSeeds`) instead of committing that file.

## Test data

The files under `testdata/` are what the tests read, with a directory per
format and `testdata/chart` for the chart's files. A fixture is written in
the shape that its source writes or documents, not captured from it, and
the test that reads it says what it stands for.

`testdata/csv` holds CSV in the shapes that it comes in. The `csvfixtures`
tests read each file at three levels:

- the rows that it decodes into (`internal/decode/csvfixtures_test.go`);
- the series, the failures and the log lines of a collector's rules over it
  (`internal/transform/csvfixtures_test.go`);
- whole probes: of an `http` collector at a stand-in that sends the file
  with one `Content-Type` or another, and of a `localfile` collector reading
  the file or a directory of them
  (`internal/exporter/csvfixtures_probe_test.go` and
  `csvfixtures_localfile_test.go`). Every series of each answer is asserted,
  in the order it is answered, in the text format and, where the two differ,
  in OpenMetrics.

A table of 5,000 rows, for the limits, is generated by the test that reads
it.

What the user documentation tells a reader to expect of a file or a page
([Reading CSV](CONFIGURATION.md#reading-csv-what-to-expect),
[Reading HTML](CONFIGURATION.md#reading-html-what-to-expect)) is checked line
by line, over small bodies written in the test, by
`internal/transform/csvfixtures_expect_test.go` and
`internal/exporter/htmlfixtures_expect_test.go`. The table of what a
`Content-Type` names is checked by `internal/decode/detectformat_test.go`. A
line added to either table gets its case there.

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

Several of them are written in a way that an editor would undo (CRLF line
ends, lone carriage returns, a byte order mark, UTF-16 and legacy encodings,
blanks at the end of a line, no final line end), and a test checks that each
still is. A file added to `testdata/csv` goes in this table and in the list
of `internal/decode/csvfixtures_test.go`, which fails until it is in both.

## The configuration schema

These schemas are generated:

- `configs/config.schema.json`, `configs/collector-file.schema.json` for
  [collector files](CONFIGURATION.md#collector-files) and
  `configs/static-targets.schema.json` for the
  [static target file](STATIC-TARGETS.md), from the configuration structs;
- the target allocator's `configs/allocator-static-targets.schema.json` and
  `configs/probe-targets.schema.json`, from the structs that it decodes,
  with their rules in `internal/allocator/schema.go`.

After adding or changing a key, regenerate them all, or the test suite
fails:

```sh
make schemas
```

The committed files are the schemas of a build with every request type
except the opt-in ones, and with only the sql type's default drivers, which
is what `make schemas` runs. The tests that compare them with what the code
generates run only in that build.

- The schema that a single-type binary prints lists the types that it
  includes, and one built with an opt-in type or with `SQL_DRIVERS` lists the
  opt-in types and drivers that it includes, as its loader accepts them. A
  test in every build checks this.
- The file of those comparisons, `test/repository/schema_fullbuild_test.go`,
  is therefore built without `select_request_types` and without any
  `request_type_<name>` tag of an opt-in type or `sql_driver_<name>` tag. A
  test keeps its constraint naming every opt-in type and driver in the tree.
- The rest of `test/repository` also passes in the build with every opt-in
  driver, and in the one with every opt-in type.

Allowed values, patterns and descriptions that a struct cannot express are
added by path: in `configSchemaRules` for the configuration and collector
files and in `staticTargetsSchemaRules` for the target file, both in
`internal/config/configschema.go`, and, for a key that only one request type
takes, in that type's `KeyRules`, by the key's path under the request block.

When the exporter starts refusing a value, give the schema the rule too,
where a schema can express it, and add the value to
`test/repository/schemaloader_test.go`. That test passes each document of
its tables through both the committed schema and `config.Load`, and fails
when they disagree. What a schema cannot express goes in the key's
description and in that file's table of what only the exporter refuses: a
minimum duration, the range of a size, or how many bytes a size written with
a unit comes to (which is why a `limits.max_output_bytes` of `20B` is left to
the exporter, while one of `20` is refused by both).

A key that the schema holds to allowed values, a pattern or a length also
gets a row in the table of its request type,
`test/repository/schemakeys_http_test.go` and its neighbours for `grpc`, `graphite`, `localfile`, `sshfile`, `sql`, `dns`, `tlscert`, `ntp`, `kubernetes`, `s3file`, `redis`, `tcp`, `ldap`, `sshexec`, `icmp` and `snmp`.
A row has the document, the place of the key, a value that both accept and
one that both refuse, and whether both accept the file without the key and
with the key written `""`.

- An optional key treats `""` as the key left out; `optionalEnum` and
  `optionalPattern` say so in the schema.
- A rule about a key is built from `writtenKey`, which depends on the key
  being written, not on its being present.
- A test of the build with every request type fails until a constrained key
  has its row.

A key that takes a size (`model.ByteSize`) says `size: true` in its row,
which runs it through the ways that a number of bytes is written
(`sizeForms`). A number that YAML reads as a number is the whole number it
equals, to the exporter as to a schema, which receives the number and not
its spelling: `1e3` and `1.0` are sizes, and a fraction, a negative number
and the same spellings in quotes are refused by both.

- A test finds the size keys of the schemas by their pattern and fails until
  a new one has its row marked this way.
- The validator that those tests use accepts as an integer what a JSON
  Schema validator does, a number without a fraction however large: the
  range of a size is for the exporter to refuse, and the tables say so.

No key accepts a value that YAML reads as none (nothing after its colon,
`null`, `~`), and no list accepts such an entry. To a schema, null is none of
a key's types, and the exporter refuses it in the one walk of a document
beside the types that the schemas are generated from (`valueProblems`,
`internal/config/yamlvalues.go`), so a new key needs nothing for it.

- `test/repository/schemanovalue_test.go` reads every key, list and mapping
  of names to values out of the committed schemas and passes a document with
  nothing at that one place through both. The tables above also pass each of
  their keys through both, written as `null` and as `~`.
- The validator that those tests use (`validateAgainstSchema`) reads null as
  a JSON Schema validator does. Do not let it accept null as any type again:
  that is how the schemas and the exporter came to differ on it unnoticed.

A key accepts the kinds of value that its schema allows, and the exporter
reads which kinds from the schema. The same walk refuses a scalar that YAML
reads as a boolean or a number at a key whose schema type is only `string`
(`schemaKinds`), and a YAML 1.1 boolean word, such as `yes` or `off`, at a
key that takes a boolean; the decoder accepts both.

- `schemaFor` gives a text key the types `string`, `number` and `boolean`,
  because free text written `1` or `true` is the text it spells, and makes a
  key with allowed values or a pattern `string` only.
- Giving a key an `enum` or a `pattern` therefore makes the exporter refuse
  `key: 1` there, with nothing more to do: the error says to quote the text
  or, for a key with an `enum`, which values it accepts.
  `TestTheKeysHeldToTextAloneAreTheSchemas` (`internal/config`), which names
  those keys, fails until the key is added to it.
- A key that is a name or a word without a pattern says `"type": "string"`
  in its rule, as a rule's `name` does.
- `test/repository/schemakinds_test.go` reads every place of the committed
  schemas where a single value is written and passes a boolean, a number,
  such a word and a quoted number there through both. The tables above pass
  each of their keys through both, written as `true` and as `1`. A row whose
  key takes more than text, and whose text the exporter refuses for reasons
  that a schema cannot express, says which (`booleanAlone`, `numberAlone`).
- The validator receives a document as an editor would pass it
  (`documentValue`): a mapping key by its text, and a date written without
  quotes as the text that it is in YAML 1.2.

A name that a collector writes (a rule's or a label's) is a classic name, or
any name, depending on the collector's `name_escaping`, which a key's own
pattern cannot read. The patterns are in `collectorSchemaRule`, under a
condition on the collector, and the loader's side is
`transform.TakesLabelName` and `checkMetricName`.
`test/repository/schemaloader_names_test.go` checks that the two agree under
each value of the key and each kind of transform.

A block that `enabled` switches on is left unchecked while it is off, by the
schemas as by the exporter. A rule about the value of one of its keys
therefore goes in the block's `then`, which applies with `enabled: true`
(`otlpSchemaRule`), not on the key. A keyword that the key's type gives it,
such as a whole number's `minimum`, is removed from the key by `nil` in its
rule. `test/repository/schemaloader_otlpoff_test.go` passes every key of the
`otlp` block through both, switched off and on, and fails for a key that its
table lacks.

A key written as only blanks is not the key written `""`: it is text, which
the key accepts or refuses. The table of
`test/repository/schemablanks_http_test.go` records what the schema and the
exporter say about both forms, for the keys of a rule, of its labels and of
a collector that say what is read. A new key of that kind gets a row there.
Where blanks can only be a mistake that the exporter would go on to read
something with, as a label's `expression` of blanks was, both refuse them.

## Measuring speed

`internal/exporter/probe_bench_test.go` benchmarks a whole probe (the
request to a target on the same machine, the decode, the transform,
validation and the answer) for each transform over the same items, 100 and
5,000 of them:

```sh
go test -run '^$' -bench 'Probe' -benchtime 2s ./internal/exporter/
```

- `jq_cached` is answered from the cache, so it measures only writing an
  answer, and `/gzip` writing it compressed.
- `jq_detected` and `jq_sniffed` are `jq_items` with `decoder.type` left to
  the response: named by its `Content-Type`, and detected from the content
  of a body served as `text/plain`.
- `python` runs a script that emits one series per item, and `prescript` a
  pre-script that passes the items on to `jq` rules, so the two measure
  handing a document to a Python worker and reading its answer back.

Measure a change to the pipeline with these before and after, on a machine
doing nothing else. `ns/op` varies with whatever else runs, while `B/op` and
`allocs/op` do not, and they say the most about a change that saves
allocation. A profile shows where the time goes:

```sh
go test -run '^$' -bench 'Probe/prom/n=5000$' -benchtime 2s -cpuprofile cpu.out ./internal/exporter/
go tool pprof -top cpu.out
```

Two results of these benchmarks, measured on two shared cores (the times are
the range of three runs, old and new in turn; bytes and allocations hardly
vary from run to run):

| Benchmark | Before | After |
| --- | --- | --- |
| `jq_sniffed/n=100` | 2.2–3.9 ms, 386 kB, 5,362 allocations | 1.4 ms, 265 kB, 1,741 allocations |
| `jq_sniffed/n=5000` | 84–97 ms, 20.3 MB, 260,777 allocations | 39–41 ms, 13.3 MB, 80,496 allocations |
| `python/n=100` | 5.4–5.9 ms, 474 kB, 5,082 allocations | 2.6–3.1 ms, 216 kB, 1,548 allocations |
| `python/n=5000` | 158–181 ms, 28.0 MB, 241,103 allocations | 95–103 ms, 13.3 MB, 65,945 allocations |
| `prescript/n=100` | 4.2–5.0 ms, 403 kB, 4,089 allocations | 3.0–4.7 ms, 267 kB, 1,996 allocations |
| `prescript/n=5000` | 118–128 ms, 22.6 MB, 200,470 allocations | 70–79 ms, 14.2 MB, 95,515 allocations |

`jq_sniffed` read the body twice: the detection parsed it with
`encoding/json` to see that it is JSON and discarded the result, and the
json decoder read it again. The detection now reads it with the json decoder
and the decode returns that result, so `jq_sniffed` costs the same as
`jq_items` and `jq_detected` (36–41 ms at 5,000 items).

The csv decoder has its own benchmarks, which report, in addition to what a
decode allocates, what the decoded rows still hold after a collection
(`held-B/op`):

```sh
go test -run '^$' -bench 'CSVDecode' -benchtime 3x ./internal/decode/
```

- `wide_header_short_rows/k=2000` is a header of 2,000 columns over 2,000
  lines of `1`, 15 kB;
- `one_column/10MiB` is a header and lines of `1`;
- `ten_columns/1MiB` is a monitoring export with a host name, a time and
  eight numbers per line.

Each row used to be a map with an entry for every column that the header
names, with an empty one for each column that a short row lacks. The rows
are now the reader's own cells, with the header's names kept once (`CSVRows`
in `internal/decode/csvrows.go`). Measured old and new in turn on two shared
cores:

| Benchmark | Before | After |
| --- | --- | --- |
| `wide_header_short_rows/k=2000` | 0.64–0.89 s, 649 MB, 66,108 allocations, 328 MB held | 0.5–1.0 ms, 696 kB, 2,099 allocations, 241 kB held |
| `one_column/10MiB` | 3.3–4.3 s, 3.1 GB, 21.0 million allocations, 1.93 GB held | 1.5–1.9 s, 852 MB, 5.2 million allocations, 238 MB held |
| `ten_columns/1MiB` | 23–34 ms, 20.6 MB, 231,369 allocations, 12.5 MB held | 4.9 ms, 4.6 MB, 27,260 allocations, 3.6 MB held |
| `Probe/csv/n=100` | 0.53–0.55 ms, 144 kB, 1,273 allocations | 0.46–0.49 ms, 99 kB, 666 allocations |
| `Probe/csv/n=5000` | 11.4–13.3 ms, 6.6 MB, 50,405 allocations | 9.5–10.5 ms, 4.3 MB, 20,389 allocations |

What a line of one column still costs, about 48 bytes for a line of two
bytes, belongs to the reader: a slice of fields and a string for each record,
and its place in the list that `encoding/csv`'s `ReadAll` grows, which is
most of the bytes allocated
(`TestCSVRowsOfOneColumnHoldASmallMultipleOfTheBody` holds the rows under
32 times the body; they are about 20 times it).

**The Python hand-over.** `python` took four times as long as `jq_items`,
and nearly all of the difference was the hand-over, not the script.

- At 5,000 items the exporter spent 20 to 24 ms writing the request with
  `json.Marshal` (a copy of the document to mark its NaNs, then reflection
  and a sort of every object's keys), and 26 to 28 ms reading the answer
  with `encoding/json` into a map for each metric and then into series.
- The worker spent 58 ms of processor time: 15 ms reading the request with
  `json.loads`, 12 ms in the script (nearly all of it in its 5,000 calls of
  `metric(...)`), 22 ms walking the answer to replace NaN and the
  infinities, and 8 ms writing it with `json.dumps`.
- Now the exporter writes the same request line itself in about 5 ms and
  reads the answer into series in about 5 ms. The worker writes an answer of
  plain values without the walk, after scanning it in 4 ms, and
  `metric(...)` takes a string label as it is, 8.5 ms for the 5,000 calls:
  37 ms of processor time in the worker. What remains there is `json.loads`
  of the request, 15 ms, and `json.dumps` of the answer, 8 ms.
- The scan through the answer also counts its values, a check for each list
  and dict that adds half a millisecond at 5,000 metrics, and weighs the
  answer where the count grows fourfold, down to a sixty-fourth of the
  count. That is what prevents an answer that holds one list many times
  over, or contains itself, from being traversed as it is written (`weigh`
  in `pythonworker.go`, and `pythonshared_test.go`).

**Key lengths.** The scan also adds up the lengths of the answer's strings,
one addition each, and refuses an answer that they alone make longer than
`limits.max_output_bytes`. It does not add up the keys of every dict,
although a long key held by every row is written as often as a long value.

- The cheapest way, `sum(map(len, d))` for each dict, took the scan of 5,000
  metrics from 5.8 ms to 9.1, of 5,000 rows of seven keys from 4.0 ms to
  5.9, and the copy of 5,000 metrics that hold a NaN from 18.7 ms to 22.6.
  That is half again of the scan, and a sixth of the whole answer, for keys
  of a few characters each in nearly every answer.
- The scan therefore adds up the keys of a few dicts: of each level of four
  values or more, the fourth value and every sixty-first after it, and of
  the dicts that a copy is made of, the fourth and every sixty-first after
  it. It takes each for the values around it, as often as the level has
  values for each one examined, and adds up all the keys only where those
  then say the answer is longer than the limit (`keyed`).

Measured against the worker before it examined keys, as the minimum of
fifteen runs under a limit of 1 MiB (Python 3.12; 3.10 is within a few
points), the scan through an answer costs:

| answer | before | now | |
|---|---|---|---|
| 1 metric | 4.6 µs | 5.8 µs | +26 % |
| 100 metrics | 130 µs | 133 µs | +2 % |
| 5,000 metrics | 7.1 ms | 7.3 ms | +2 % |
| 5,000 rows of seven keys | 4.6 ms | 4.7 ms | +2 % |
| a nested document of 1 MiB | 17.2 ms | 18.1 ms | +5 % |
| a chain of lists 450 deep | 197 µs | 210 µs | +7 % |
| the same, four items a level | 334 µs | 414 µs | +24 % |
| 5,000 metrics, one a NaN (copied) | 21.8 ms | 22.6 ms | +4 % |
| 5,000 rows, one a NaN (copied) | 17.6 ms | 17.6 ms | +1 % |
| one dict of 50,000 keys as the fourth value of four | 6.1 ms | 7.0 ms | +15 % |
| 5,000 rows, a key of 20 kB in the fourth | 4.5 ms | 6.9 ms | +53 % |
| 5,000 metrics, a label of 20 kB on the first | 6.5 ms | 11.5 ms | +78 % |
| the last rows, one a NaN (copied) | 17.0 ms | 19.8 ms | +16 % |

Writing the whole answer costs 1 to 5 % more for the ordinary answers, 4 %
for the one dict of many keys, and 21 to 31 % for the last three.

- An answer of a few values pays for its levels, a microsecond in all: each
  level is kept, and one of four values or more has its fourth value
  examined.
- The last three rows are the worst case for an answer within the limit: one
  key unlike the rest stands where the scan passes, the dicts examined
  overestimate by sixty-one times, and the keys of every dict are added up to
  find that the answer is within the limit after all. That costs what adding
  them up for every answer would (`sum(map(len, d))` in the scan, measured
  beside it: +54 % for the rows, +65 % for the metrics), plus a tenth of the
  scan where the dicts are a few among many values, because they are picked
  out of the levels afterwards.
- It is done from the levels that the scan kept: the dicts of each are
  picked out in one pass and the keys of each joined by the interpreter,
  1,024 values at a time: `sum(map(len, map("".join, dicts)))`. A key is not
  asked for its length, which would run the `__len__` of a class of the
  script's own, and the dicts examined before are left out.
- The first form of this took each dict examined as sixty-one wherever it
  stood, and added up the keys in a second walk through the answer, four
  calls per dict. The one dict of 50,000 keys then cost 18.6 ms where it
  cost 6.1, the rows 12.5 ms and the metrics 19.7, three times the scan,
  whenever such a dict stood fourth and never when it stood third.
- What the scan keeps for this is the lists that it made anyway, eight bytes
  for each value of the answer until it ends (472 kB where it held 429 for
  5,000 metrics), and for a copy a list of its dicts. The keys of an
  examined dict are joined into one string as long as they are, which is
  released immediately.

A row keyed by a hundred thousand characters 5,000 times, 500 MB written, is
refused in 10 to 40 ms and 15 MiB, where the worker used to take 3.5 s and
1.4 GB to write it for the exporter to refuse.

A clock is not accurate to two percent on a shared machine, so
`TestTheLookAtKeysCostsAnAnswerWithinTheLimitNoMoreThanATwentieth` checks
the cost by counting:

- the calls that the worker makes for an answer, of its own functions and of
  the interpreter's (`sys.setprofile`), before and after the change, in the
  worker itself, for the ordinary answers and for the three kinds above;
- how many dicts it examines;
- that the keys of all dicts are added up once for the answers with one long
  key, and not at all for the others, including the one dict of many keys.

A count of calls does not see what the interpreter does inside one call, so
it checks that no walk is made one call at a time, not how long a join
takes: measure the table above again when `keyed` or `key_size` changes.
Counting bytecodes with `sys.settrace` would say more, but from Python 3.12
that count stops short the first time a function is traced. What the scan
misses is described in `docs/PYTHON.md`: an answer whose long keys are where
the scan does not pass, or in too few of the dicts that it does examine.

The shortest lines that a worker writes set the minimum
`limits.max_output_bytes`. `MinPythonOutputBytes` in `pythonworker.go` is
the longest of: the line with which a worker reports it is ready, the line
with which it accepts a request, and the answers of a transform script that
emits nothing and of a pre-script that leaves `None`. That is 38 bytes.

- The loader refuses a smaller limit, and the schemas a smaller number
  (`leastOutputRule`). The worker and the pool have no floor, and run under
  any limit that a test gives them.
- `pythonleastoutput_test.go` runs a worker and compares its lines with
  those constants, so an answer that gains or loses a key fails there.
  Change the lines, and the 38 that `docs/PYTHON.md`,
  `docs/CONFIGURATION.md` and the specification state.

The lines that the exporter and a worker exchange are unchanged, byte for
byte, so `limits.max_output_bytes` bounds what it bounded before and a
script is given what it was given before. Two shortcuts were measured and
not taken, because a script would see them:

- Handing a worker the bytes of a JSON response instead of the decoded
  document would save writing `data`, but the worker would then read a
  different document: its keys in the target's order instead of sorted,
  `1.0` as a float instead of the int that the exporter's `1` becomes, and
  `1e400` as `inf` instead of the text that the json decoder keeps.
- A request in a binary form that Python loads faster than JSON (`pickle`
  reads this one in 7 ms, `marshal` in 6) would change the protocol that
  the specification fixes: one JSON document per line.

`BenchmarkYAMLMerge` in `internal/decode` decodes a YAML body with a merge
(`<<: *base`) of a mapping of 20,000 and of 200,000 keys, next to the same
body without the merge and with the merged pairs written out. Divide
`B/op` and `allocs/op` by `body-bytes`:

```sh
go test -run '^$' -bench 'YAMLMerge' -benchtime 3x ./internal/decode/
```

The walk that decodes a document with a large mapping used to hand the
library each merged key and each merged value separately, two calls per
pair. It now hands over a run of 128 pairs at a time: the keys, and then the
values of the keys that the merge provides. Decoding the parsed document,
without parsing (the minimum of several runs, old and new in turn; 20,000
keys):

| Document | Before | After |
| --- | --- | --- |
| the mapping, not merged | 19 ms, 19.5 bytes and 0.24 allocations per body byte | the same |
| merged once | 48 ms, 52.3 bytes, 0.85 allocations | 31 ms, 33.3 bytes, 0.49 allocations |
| merged four times | 146 ms, 150.5 bytes, 2.68 allocations | 77 ms, 74.6 bytes, 1.24 allocations |
| a list of two, half their keys shared | 102 ms, 48.4 bytes, 0.78 allocations | 67 ms, 31.6 bytes, 0.46 allocations |

A merged pair costs 230 bytes in four allocations, where it cost 540 in ten,
and a pair of the mapping itself costs 320 in four. `yamlkeysmerge_test.go`
checks this: a merge may allocate no more than the mapping it merges, the
library is called twice for every 128 keys of a merge, and the previous walk
(`yamlkeysmergeoracle_test.go`) decodes every document of the tests into the
same value or error.

Following a reload has its own benchmark,
`internal/exporter/reloadfollow_bench_test.go`:

```sh
go test -run '^$' -bench 'FollowReload' -benchtime 20x ./internal/exporter/
```

It reloads between two configurations of 50, 500 and 2,000 collectors: the
same collectors read again (`unchanged`), every one changed (`changed`), and
half of them removed and as many added (`half`), each also with a static
target for every collector in a file reloaded with them (`/static`).

- The time of the operation is the whole following, including what a reload
  prepares of it before it puts its configuration in force (below).
- `following-ns/op` is what remains of it once the configuration is in
  force.
- `locked-ns/op` is how long the statistics lock was held. Every probe takes
  that lock to find its collector's statistics, and so does the read of the
  self-metrics.

As measured on two shared cores, with and without static targets, which make
no visible difference:

| Benchmark | Before | After |
| --- | --- | --- |
| `unchanged/n=50` | 25–34 ms, all of it under the lock; 17.7 MB, 53,113 allocations | 11–13 ms, 2–3 µs of it under the lock; 9.7 MB, 29,219 allocations |
| `unchanged/n=500` | 205–257 ms, all of it under the lock; 176.6 MB, 531,013 allocations | 108–145 ms, 10–14 µs of it under the lock; 97.2 MB, 292,064 allocations |
| `changed/n=500` | 229–286 ms, all of it under the lock; 176.7 MB, 531,032 allocations | 119–142 ms, 21–34 µs of it under the lock; 97.2 MB, 292,083 allocations |
| `half/n=500` | 199–227 ms, all of it under the lock; 132.5 MB, 398,294 allocations | 107–145 ms, 97–147 µs of it under the lock; 92.8 MB, 278,820 allocations |
| `unchanged/n=2000` | 0.7–1.3 s, all of it under the lock; 706.5 MB, 2,124,026 allocations | 345–562 ms, 32–45 µs of it under the lock; 388.7 MB, 1,168,226 allocations |
| `half/n=2000` | 0.5–1.0 s, all of it under the lock; 530.2 MB, 1,593,070 allocations | 344–720 ms, 0.35–0.57 ms of it under the lock; 371.3 MB, 1,115,171 allocations |

**Fingerprints.** Nearly all of the time went to one thing. A collector's
fingerprint is its definition written as YAML and hashed, about 0.2 ms and
177 kB for a collector of one rule, and a reload computed it for every
collector of the new configuration and again for every collector of the one
it had followed, with the lock held, to find the collectors that changed.
Comparing the static targets took 0.6 ms for 500 of them, and the maps and
the drops took the rest.

- The fingerprints of a configuration are now kept with it once it is
  followed, so a reload computes only those of the new configuration.
- What the following is to drop and replace is computed into a plan before
  the lock is taken.
- The lock is then taken to carry out the plan, once the plan is confirmed
  to still belong to the configuration in force and to start from the one
  followed (`planFollowing` and `followLocked` in `reconcile.go`).

`reloadplan_http_test.go` checks this by counts and by the order of events,
not by time: no definition is encoded while the lock is held, a reload
encodes each collector of the new configuration once, a probe is answered
while a reload is held between its plan and the lock, and over generated
sequences of reloads, with probes and scrapes held across them, a server
ends up in the same state as one that follows in the previous way, which is
kept next to the test.

**Preparing fingerprints.** A probe that arrives when a reloaded
configuration is in force but the reload has not yet followed it needs it
followed before it reads its collector there, and computes the same plan.
While the plan computed the fingerprints, such a probe waited for those that
the reload had not computed yet, at worst all of them, at every reload.

A reload therefore computes them before it puts its configuration in force.
The configuration manager lets the server prepare what a reload has read and
checked (`config.Manager.OnPrepare`, `prepareReload` in `reconcile.go`), on
the reloading goroutine, while `Get` and `InForce` still return the previous
configuration and the probes continue with it. The configuration then goes
in force with every fingerprint computed, including those of the collectors
of the followed configuration that it is compared with. Whoever follows it
first, the reload or a probe, has only the maps of the plan left to build.

- The reload as a whole takes as long as before. Its configuration is in
  force later by as long as the fingerprints take, about 0.1 s for 500
  collectors.
- A probe never waits for a reload's fingerprints.

`BenchmarkFollowReloadProbeWait`, in the same file and selected by the same
`-bench`, reads a file of 50, 500 and 2,000 collectors again as `SIGHUP`
reads it, holds the reload where its configuration is in force and not yet
followed, and requests the state there as a probe does. `probe-wait-ns/op`
is how long that took, and the time of the operation is the whole reload. As
measured on two shared cores:

| Collectors | A probe waited | It waits | Left of the following once in force (`following-ns/op`) |
| --- | --- | --- | --- |
| 50 | 13–19 ms | 15–70 µs | 12–43 µs |
| 500 | 121–181 ms | 0.11–0.30 ms | 0.13–0.37 ms |
| 2,000 | 0.38–0.61 s | 0.5–0.7 ms | 0.5–2.3 ms |

The time, the memory and the allocations of a whole reload are unchanged, and
so is the time under the lock.

- Only the fingerprints are prepared. They belong to a configuration alone,
  which never changes once loaded, so they are correct whatever is followed
  when they are used.
- The plan is not prepared, because it depends on the followed
  configuration, which a test, or anything that puts a configuration in force
  without a reload, can change between the two steps.
- The prepared fingerprints are kept next to those that the probes last used
  (`fingerprintMemo`), so a probe that still holds the previous configuration
  does not have the new one's computed again. The memo holds those of at most
  two configurations: the one in force, and the previous one while probes
  still read it.
- `reloadprepare_http_test.go` checks all of this by counting where a
  definition is encoded: none once a reloaded configuration is in force, by
  the reload's own following or by a probe held between the two; each
  collector of the configuration once per reload; none for a static target
  file reloaded alone; and after 1,000 reloads no configuration is held
  except the last few. `onprepare_http_test.go` in `internal/config` checks
  the order: prepared, in force, notified.

**The static target schedule.** The static targets make no visible
difference above because their schedule is not part of what is measured
there. It belongs to the scrape loop (`targetSchedule` in
`statictargetschedule.go`), and examines a reloaded configuration on that
loop's goroutine, within a second of the reload. A target restarts when its
collector's definition changed, which the schedule detects by the
collector's fingerprint, kept with the target's place in the schedule.

- The schedule used to compute that fingerprint itself: every collector with
  a static target was encoded a second time for every reload, and so was the
  collector of every target that a static target file reloaded alone
  started. That kept no probe waiting, but for 2,000 collectors with a target
  each it was a third of a second of one core and 355 MB allocated after
  every reload.
- The schedule now reads the fingerprints that the reload computed, which
  the followed configuration keeps (`followedConfig.fingerprintOf` in
  `reconcile.go`). The loop receives the configuration, the static target
  file and their following together (`followedInForce`), none of which
  changes once followed.
- The fingerprints are read only when the configuration that the schedule
  plans with is exactly the one that the following belongs to. For any other,
  and for a caller that follows none, as in the tests of the schedule alone,
  the definition is encoded as before.
- The schedule takes no lock for this. Where the exporter started with the
  configuration, whose fingerprints no reload prepared, it computes the ones
  that nothing has requested yet, once, and the probes and the next reload
  find them computed.

`BenchmarkFollowReloadSchedule`, in the same file and selected by the same
`-bench`, reloads as `BenchmarkFollowReload` does, with a static target for
every collector, and times only the schedule's subsequent examination.
`encodes/op` is how many definitions the reload and the examination encoded
together, and `schedule-encodes/op` how many of them the examination encoded.
As measured on two shared cores, old and new in turn, three times:

| Benchmark | Before | After |
| --- | --- | --- |
| `unchanged/n=50` | 10–12 ms; 8.9 MB, 26,735 allocations; 100 definitions encoded, 50 by the look | 0.08–0.14 ms; 49 kB, 185 allocations; 50 encoded, none by the look |
| `unchanged/n=500` | 92–98 ms; 88.8 MB, 267,268 allocations; 1,000 encoded, 500 by the look | 1.3–1.6 ms; 533 kB, 1,768 allocations; 500 encoded, none by the look |
| `changed/n=500` | 94–98 ms; 88.7 MB, 267,018 allocations; 1,000 encoded, 500 by the look | 0.7–1.3 ms; 397 kB, 1,518 allocations; 500 encoded, none by the look |
| `half/n=500` | 98–123 ms; 88.7 MB, 267,143 allocations; 1,000 encoded, 500 by the look | 0.9–1.1 ms; 465 kB, 1,643 allocations; 500 encoded, none by the look |
| `unchanged/n=2000` | 320–333 ms; 355.2 MB, 1,069,038 allocations; 4,000 encoded, 2,000 by the look | 8.2–8.6 ms; 2.1 MB, 7,038 allocations; 2,000 encoded, none by the look |
| `changed/n=2000` | 315–347 ms; 354.7 MB, 1,068,038 allocations; 4,000 encoded, 2,000 by the look | 6.1–7.3 ms; 1.6 MB, 6,038 allocations; 2,000 encoded, none by the look |

**Finding a collector by name.** What then remained of the examination for
2,000 collectors was mostly finding each target's collector by its name: the
schedule went through the configuration's collectors once for every
collector with a static target, so 2,000 times through 2,000, and 10,000
times through 10,000. A probe did the same once for its collector and once
more for that collector's fingerprint, when the collector caches, and so did
the scrape of a static target. With 10,000 collectors, a probe of the last
one answered from the cache took 0.1 to 0.2 ms, where one of the first took
0.02.

- The place of each collector is now kept by its name with the
  configuration's fingerprints (`fingerprintGeneration.place` in
  `fingerprint.go`), which live as long as the configuration does and are
  shared by the same callers.
- The collectors are scanned once per configuration, by the reload that
  prepares it before it is in force, or, for the configuration that the
  exporter started with, by whoever first asks for a name. That takes 0.1 ms
  and 109 kB for 2,000 collectors, where the reload takes a third of a
  second.
- The map is built under a `sync.Once` and never changed afterwards, so the
  scrape loop, the probes and a reload read it without a lock.
- `followedConfig.collectorOf` and `fingerprintOf` read it only for the
  configuration that their following belongs to, and
  `fingerprintMemo.fingerprint` only while the remembered fingerprints are
  that configuration's. For any other configuration, as for a scrape that
  waited for a slot across a reload, and for a caller that follows none, the
  collectors are scanned as before.
- `schedule-scans/op` is how many times the examination scanned them: none
  now, and once for every collector with a static target before.

The benchmark also has 10,000 collectors with a target each, for which
`-bench 'FollowReloadSchedule/.*/n=10000$' -benchtime 5x` is enough (a
reload of that many takes seconds, which are not timed but are still waited
for), and 500 collectors with 20 targets each (`/targets=20`). As measured on
two shared cores, old and new in turn; the bytes and allocations of the
examination are unchanged, because a scan allocated nothing:

| Benchmark | Before | After |
| --- | --- | --- |
| `unchanged/n=50` | 0.08–0.10 ms | 0.07–0.11 ms |
| `unchanged/n=500` | 1.1–2.2 ms | 0.9–1.0 ms |
| `changed/n=500` | 0.6–1.0 ms | 0.3–0.6 ms |
| `unchanged/n=2000` | 7.1–10.3 ms | 4.1–6.6 ms |
| `changed/n=2000` | 5.1–8.5 ms | 1.3–4.7 ms |
| `half/n=2000` | 10.6–15.2 ms | 3.1–5.1 ms |
| `unchanged/n=10000` | 298–307 ms | 64–78 ms |
| `changed/n=10000` | 263–339 ms | 20–25 ms |
| `half/n=10000` | 228–243 ms | 22–31 ms |
| `unchanged/n=500/targets=20` | 14–24 ms | 14–31 ms |
| `changed/n=500/targets=20` | 7–17 ms | 7–17 ms |

With 20 targets per collector the examination takes as long as before: it
also asked once for each collector then, 500 times through 500, and the time
is that of the 10,000 targets, each compared with its place in the schedule.

- A probe of the last of 2,000 collectors answered from the cache takes
  17–24 µs where it took 23–37, and of 10,000, 14–22 µs where it took
  106–224. The scrape of the last static target of 2,000, answered from the
  cache, takes 13–15 µs where it took 22–53. What a probe allocates is
  unchanged.
- `reloadschedule_http_test.go` checks this by counting how often the
  collectors are scanned (`collectorsScannedHook`), never by time: once by
  the schedule's first examination, once by a reload, and not at all by the
  examination after it, by a probe or by a scrape, for 60 collectors and for
  240, and with five targets per collector.
- `collectorplace_test.go` checks that what is found by a name is what
  scanning the collectors found, with the previous lookups kept next to the
  test.
- `reloadschedule_http_test.go` also checks the rest by counting where a
  definition is encoded: a reload of a configuration whose collectors all
  have a static target encodes each collector once, before it is in force,
  and the examination after it encodes none, driven as the loop drives it and
  with the loop running; the fingerprints that the schedule requests first
  are the ones that the probes use; a static target file reloaded alone has
  the examination encode only a collector that nothing had asked about; and
  over generated runs of reloads every examination leaves the schedule as the
  previous version, kept next to the test, leaves it.

**Other lookups by name.** Two more lookups by name grew with the
configuration, both for the static targets, and a third in the verbose
self-metrics:

- A scrape of a static target asked, when it ended and before it published,
  whether a target of its name was in force, by scanning the file's targets
  (`staticTargetInForce` in `statictargetsendpoint.go`).
- A read of the static targets endpoint that names targets recorded every
  name of the file again to look up its few names (`requestedStaticTargets`).
- A read of the verbose self-metrics, which finds the request of every static
  target to keep it tracked (`seedStaticRequests` in `requeststats.go`),
  scanned the configuration's collectors once for every target
  (`model.CollectorByName`): 2,000 times through 2,000 at every read.

Now:

- The first two read the names that the following of a file records anyway,
  once per file, with the generation that each target has existed since
  (`followedConfig.targetNames`, `targetsDefinedFrom` in `reconcile.go`).
  Nothing more is kept, and a reload does no more work than before:
  `BenchmarkFollowReload` allocates the same, 390.8 MB in 1,178,256
  allocations for 2,000 collectors with a static target each.
- The names belong to the followed file and are read only when the file in
  force is exactly that file. Between a reload putting another file in force
  and that reload's following of it (which it does immediately and computes
  before the statistics lock is taken), the targets are scanned as before,
  so the answer reflects the file in force either way.
- The read of the verbose self-metrics finds each target's collector where
  the followed configuration keeps its place (`followedConfig.collectorOf`),
  as a probe and a scrape do, and under the same condition. The read follows
  what is in force before it looks for the requests, so only a reload that
  comes between the two makes it scan the collectors as before.

`staticlookups_bench_test.go` has both, for 100, 2,000 and 10,000 static
targets of as many collectors:

```sh
go test -run '^$' -bench 'StaticTargetLookup|SelfMetricsSeedStatic' ./internal/exporter/
```

- `BenchmarkStaticTargetLookup` measures what the scrape of the last static
  target asks when it ends (`in-force`), that whole scrape answered from the
  cache (`scrape`), and what a read of the endpoint that names that one
  target asks (`named`).
- `BenchmarkSelfMetricsSeedStatic` measures what a verbose read does for the
  static targets: finding the request of each (`keys`), and that plus
  notifying the tracker of them (`seed`).

A configuration of 10,000 collectors takes seconds to load, which are not
timed; `-bench 'StaticTargetLookup/n=2000$'` selects one size. As measured
on two shared cores that other work kept busy throughout, old and new in
turn, seven to ten times, which is why the ranges are wide. What `in-force`,
`scrape`, `keys` and `seed` allocate is unchanged:

| Benchmark | Before | After |
| --- | --- | --- |
| `n=100/in-force` | 0.6–1.5 µs | 14–106 ns |
| `n=2000/in-force` | 6–19 µs | 16–76 ns |
| `n=10000/in-force` | 60–188 µs | 15–46 ns |
| `n=100/scrape` | 20–61 µs | 20–76 µs |
| `n=2000/scrape` | 29–75 µs | 18–63 µs |
| `n=10000/scrape` | 120–239 µs | 16–36 µs |
| `n=100/named` | 16–56 µs, 6,968 bytes, 12 allocations | 0.7–1.7 µs, 272 bytes, 3 allocations |
| `n=2000/named` | 0.5–1.5 ms, 218,248 bytes, 32 allocations | 0.5–1.4 µs, 272 bytes, 3 allocations |
| `n=10000/named` | 2.0–6.2 ms, 873,544 bytes, 82 allocations | 0.6–1.5 µs, 272 bytes, 3 allocations |
| `n=100/keys` | 0.21–0.60 ms | 0.20–0.43 ms |
| `n=2000/keys` | 11–36 ms | 5–16 ms |
| `n=10000/keys` | 0.61–1.74 s | 27–70 ms |
| `n=2000/seed` | 92–330 ms | 113–338 ms |
| `n=10000/seed` | 1.4–3.6 s | 0.8–1.9 s |

What remains of `keys` belongs to each target, about 3 µs and six
allocations each: the overrides of its request and the label of its URL,
built again at every read. `seed` with more than 1,000 static targets was
dominated by the tracker and was not changed by this. The tracker tracks
`VerboseRequestSeriesLimit` requests, the static targets beyond that are
offered to it again at every read, and each offer scanned everything tracked
for an idle request to make room (`setStatic`, `adoptLocked`,
`expireLocked`): 1,000 times through 1,000 for 2,000 targets and 9,000 times
for 10,000. That is most of the time above, and it hid what `keys` gained.
The tracker no longer does that: see below. The read that names a target
still filters every series of the endpoint by the names, which takes as long
as the endpoint is large.

This is checked by counting, never by time. `targetsScannedHook`, next to
`collectorsScannedHook`, is called whenever the targets of a file are
scanned to find whether one has a name or to record their names.
`staticlookups_http_test.go` counts both:

- A read of the verbose self-metrics scans the collectors once, the first
  time, for the configuration that the exporter started with, and not at all
  afterwards, where it used to scan them once for every static target.
- The scrape of the last static target and a read that names it scan the
  targets not at all, where each used to scan them once.
- A reload scans the collectors of its configuration once and the targets of
  its file once.

These hold for 60 collectors with a target each, for 240, and with five
targets per collector. The previous lookups are kept next to the tests:

- Over 200 generated target files given to the server as they are (with
  targets that share a name, names that differ by letter case or blanks, and
  the empty name), a name is reported in force, and a read that names
  targets is given its names or refused, exactly as the previous lookups do
  (`statictargetnames_test.go`). This holds with the file followed, with one
  in force that is not followed yet, and with nothing followed.
- Over a run of reloads of both files, of either alone and of refused files,
  the same holds when the files are read and not yet in force, when they are
  in force and not yet followed, and when the reload is done. Every scrape
  that read its target before a reload publishes exactly when the previous
  lookup and the target's lifetime say it does.
- Over 60 generated configurations (with collectors that share a name, and
  targets of a collector that the configuration does not have), the
  requests found are those of the previous search, and a verbose read is
  answered with the same bytes as by a server that tracked the previous
  search's requests, except for the families of the Go runtime and of the
  process.
- Two readers that ask all three questions while reloads replace both files
  and either alone are answered as the previous lookups answer whenever no
  reload came during the question. Run with the race detector, this also
  shows that what was recorded for the followed files is read while a reload
  records the next.
- Only a test sets one of the hooks: the exporter declares each and reads it
  in one place (`test/repository/scanhooks_test.go`).

**The request tracker.** Past the limit, a static target that the tracker
has no room for is offered to it at every read of the verbose self-metrics,
and every offer looked for an idle request to make room by scanning all the
tracked requests (`expireLocked`): with 2,000 static targets, 1,000 times
through 1,000 at each read, once for every scrape of the self-metrics.

The tracker now keeps a time before which no request that can expire was
last used (`idleFloor`). It is set when the tracker scans the requests and
moved earlier when a request is used at an earlier time, and the tracker
scans them only when that time is an hour old, that is, when one of them may
be idle. What it drops, keeps and counts is unchanged.

`BenchmarkVerboseSelfMetricsRead` in `staticlookups_bench_test.go` measures
what the tracked requests make a read do (the static targets' requests
reported to the tracker, the idle ones expired, and the series of each),
with the tracker full of the static targets' requests (`static`) or of
requests of probes that leave no room for any of them (`probed`).
`BenchmarkSelfMetricsSeedStatic/seed` is the first part. Old and new back to
back on two shared cores; neither allocates more or less than before:

```sh
go test -run '^$' -bench 'SelfMetricsSeedStatic/.*/seed|VerboseSelfMetricsRead' ./internal/exporter/
```

| Benchmark | Before | After |
| --- | --- | --- |
| `n=1000/static` | 3.0–3.5 ms, 1.57 MB, 8,042 allocations | 3.8–4.0 ms, the same |
| `n=1000/probed` | 54–55 ms, 1.89 MB, 9,042 allocations | 4.0–4.5 ms, the same |
| `n=2000/static` | 68–69 ms, 2.40 MB, 15,051 allocations | 5.5–7.7 ms, the same |
| `n=2000/probed` | 103–105 ms, 2.73 MB, 16,051 allocations | 5.4–7.1 ms, the same |
| `n=10000/static` | 0.65 s, 8.51 MB, 71,101 allocations | 26–28 ms, the same |
| `n=10000/probed` | 0.53–0.60 s, 9.05 MB, 72,118 allocations | 26–36 ms, the same |
| `n=2000/seed` | 71 ms | 3.4 ms |
| `n=10000/seed` | 0.60 s | 25 ms |

What remains grows with the number of static targets. About two thirds is
finding each target's request (`staticRequestKeys`, mostly
`fetch.RequestLabelFor` building its URL's label again), and most of the
rest is `setStatic` itself, which builds new statistics for each left-over
target that the tracker then rejects.

- This is checked by counting, not by time. `requestsScannedHook` is called
  whenever `expireLocked` scans the requests. A read with twice as many
  static targets as the tracker holds scans them at most once, with the
  tracker full of probe requests, with those requests idle an hour later,
  and with it full of static targets' requests
  (`requeststats_oracle_test.go`), where it used to scan them 2,001 times.
- The previous tracker is kept next to the tests as an oracle. Over
  generated runs of probes of new and tracked requests, scrapes of static
  targets, reads with the static targets unchanged or changed by a reload
  (to a few, nearly as many as the tracker holds, or a few more), removed
  collectors, verbose switched off, and time passing by seconds, minutes,
  about an hour and backwards, the tracker keeps the same requests with the
  same values, times and counts as the previous one after every step, and
  returns the same series at every read. Past the limit, the previous tracker
  took the static targets in the map's order, and the new one is held to an
  order that it could have had. It costs about a second, plain or under the
  race detector.

**The debug scrape.** The debug scrape of a static target,
`/static-targets?debug=<name>`, finds its target and the target's collector
before it scrapes (`staticDebugTarget` in `probedebug.go`). It iterated over
the targets by value, keeping a pointer to the loop's copy, which put a copy
of each target it passed (416 bytes) on the heap, and then scanned the
collectors (`model.CollectorByName`). It now iterates over the targets by
index and points into the file in force, which nothing in the debug scrape
writes to, and finds the collector where the followed configuration keeps
its place (`followedConfig.collectorOf`), as a scrape does, falling back to
scanning the collectors where a scrape does. `BenchmarkStaticTargetLookup`
times it with the last target requested (`debug`), old and new back to back:

| Benchmark | Before | After |
| --- | --- | --- |
| `n=100/debug` | 38 µs, 41,600 bytes, 100 allocations | 0.4 µs, none |
| `n=2000/debug` | 0.61 ms, 832,000 bytes, 2,000 allocations | 4.8 µs, none |
| `n=10000/debug` | 2.3 ms, 4,160,000 bytes, 10,000 allocations | 54 µs, none |

What remains is comparing the names of the targets passed, which costs much
less than the scrape that follows; the targets are not indexed by name for
it. `staticdebugtarget_test.go` checks this:

- finding the last of 2,000 and of 10,000 targets allocates nothing, where
  the previous lookup, measured next to it, allocates once per target;
- over 200 generated pairs of configuration and target file (with targets
  that share a name, collectors that share one, and targets of a collector
  that the configuration lacks), the target and collector found are those
  that the previous lookup found (the target being the first of its name, in
  the file itself). This holds with the configuration followed, which scans
  its collectors once in all (`collectorsScannedHook`), and with a followed
  configuration that keeps no places, which scans them once for each target
  found.

**Checking a static target file.** The check of a static target file against
a configuration, which startup and `--dry-run` make once and a reload once
for each pair of the two that it asks about (see below), has a benchmark in
`internal/config/targetcheck_bench_http_test.go`:

```sh
go test -run '^$' -bench 'StaticTargetsAgainst|TargetsChecked' ./internal/config/
```

- `BenchmarkValidateStaticTargetsAgainst` is the check
  (`ValidateStaticTargetsAgainst`) of n targets against n collectors: `own`
  with each target of its own collector, `last` with every target of the
  last collector, whose path has a placeholder, and `one` with a single
  target.
- `BenchmarkTargetsChecked` is the search for the collectors whose descriptor
  files the check opens (`targetsChecked`), which a reload that reads the
  target file makes for the configuration read and the one in force:
  `no_message` with no target setting a `request.message`, as in every file
  without a grpc collector, `every_message` and `one_message`.

The check found a target's collector by scanning the configuration's
collectors (`model.CollectorByName`), four times for every target, and the
search scanned them once more for every target, whether or not it set a
message, reading a name out of each definition of 1,224 bytes. For as many
targets as collectors, that is the square of their number. Now the
collectors are scanned once per check, to record where each is by its name
(`collectorsByName` in `statictargets.go`), in the map that the check
already built to know the names, and the search does so only once it meets
a target that sets a message. `indexes/op` is how many times they were
scanned. As measured on two shared cores, old and new in turn (the times are
the range of two or three runs):

| Benchmark | Before | After |
| --- | --- | --- |
| `own/n=100` | 0.62–0.75 ms, 41,388 bytes, 513 allocations | 0.49–0.63 ms, 38,237 bytes, 508 allocations |
| `own/n=2000` | 58–70 ms, 906,182 bytes, 10,031 allocations | 10.7–12.2 ms, 797,449 bytes, 10,012 allocations |
| `own/n=10000` | 2.07–2.35 s, 4,313,560 bytes, 50,083 allocations | 57–62 ms, 3,877,267 bytes, 50,038 allocations |
| `last/n=2000` | 122–128 ms, 1,482,083 bytes, 18,029 allocations | 11.9–16.7 ms, 1,373,336 bytes, 18,010 allocations |
| `last/n=10000` | 5.8–6.3 s, 7,193,448 bytes, 90,081 allocations | 62–67 ms, 6,757,032 bytes, 90,035 allocations |
| `one/n=10000` | 2.4–2.7 ms, 873,921 bytes, 88 allocations | 1.2–1.4 ms, 437,551 bytes, 43 allocations |
| `no_message/n=2000` | 11.8–15.0 ms | 2.2–2.3 µs |
| `no_message/n=10000` | 0.56–1.27 s | 11–18 µs |
| `every_message/n=2000` | 24–28 ms, 9,026,688 bytes, 14 allocations | 10.9–11.1 ms, 9,136,304 bytes, 25 allocations |
| `every_message/n=10000` | 0.67–0.85 s, 58,424,448 bytes, 20 allocations | 67–78 ms, 58,861,712 bytes, 55 allocations |
| `one_message/n=10000` | 0.53–0.54 s, 1,280 bytes, 1 allocation | 1.3–1.6 ms, 438,544 bytes, 36 allocations |

What then remained of the check belonged to each target, 4 to 6 µs each
whatever the configuration holds:

- about half was reading which keys the target's `request` block sets, by
  reflection over the block's fields and their tags (`setKeys` in
  `internal/fetch`);
- a quarter was binding the target's params, which parsed the placeholders
  of the collector's request again for every target of it, the path's twice
  (`fetch.CheckRequestParams`).

Which field of a block has which yaml key depends only on its type, and is
now read from the tags once per type and kept (`yamlKeysOf`); only whether
each field is set is examined for each target. A collector's placeholders are
found once per check, for its first target, and kept for its others
(`fetch.RequestParamsCheck`, which lives for one check, so that what it keeps
cannot go stale). `CheckRequestParams` itself, which a probe calls, finds
them once where it found them twice. The same benchmark, old and new in
turn, twice:

| Benchmark | Before | After |
| --- | --- | --- |
| `own/n=100` | 0.38–0.47 ms, 38,236 bytes, 508 allocations | 0.16 ms, 36,293 bytes, 451 allocations |
| `last/n=100` | 0.57–0.66 ms, 66,752 bytes, 904 allocations | 0.13 ms, 25,499 bytes, 508 allocations |
| `own/n=2000` | 7.4–7.9 ms, 797,452 bytes, 10,012 allocations | 3.4–3.5 ms, 817,717 bytes, 8,709 allocations |
| `last/n=2000` | 9.7–11.7 ms, 1,373,334 bytes, 18,010 allocations | 2.6–3.3 ms, 541,644 bytes, 10,014 allocations |
| `own/n=10000` | 39–40 ms, 3,877,267 bytes, 50,038 allocations | 18–22 ms, 3,828,729 bytes, 43,451 allocations |
| `last/n=10000` | 49–50 ms, 6,757,022 bytes, 90,034 allocations | 15 ms, 2,597,295 bytes, 50,038 allocations |

What remains for a target is 1.5 to 2 µs: whether each field of its
`request` block is set, still by reflection, a fifth of the check;
`url.Parse` of its address, which is what checking the address is, as much
again; and binding its params against the placeholders found. `own` also
finds the placeholders of each collector, each for its first target.
Recording where 10,000 collectors are takes 1 to 2 ms. The search with every
target setting a message is dominated by the copies it returns, a
collector's definition for each such target.

One case costs more than before: a target file of fewer than about ten
targets, one of which sets a message, checked against thousands of
collectors. There, scanning the collectors a few times cost less than
recording where all of them are: 0.6 ms where it was 0.1 for one target and
10,000 collectors, during a reload that reads a configuration of that size
in seconds.

This is checked without a duration:

- `targetcheck_http_test.go` and `targetcheck_test.go` count the scans of the
  collectors (`collectorsIndexedHook`): once per check, for one target and
  for 400, for an accepted file and for one refused at its last target; and
  for the search, once for each configuration given, and not at all for a
  file in which no target sets a message.
- `test/repository/collectorbyname_test.go` reads the two source files and
  fails when either uses `model.CollectorByName` again.
- The previous check and search are kept next to the tests. Over 20,000
  generated pairs of a configuration and a target file (with collectors that
  share a name, a request type that the build lacks, targets refused for each
  thing that the check refuses and for several at once), and over every
  static target file that the repository ships against every configuration
  it ships, the check returns the same result, message for message, and the
  search finds the same collectors in the same order.
- That a collector's placeholders are found once per check and a block's
  yaml keys once per type is checked by counting (`placeholdersParsedHook`
  and `yamlKeysReadHook` in `internal/fetch`, `targetcheckcost_test.go`):
  1,000 checks of the params of three collectors, whatever parts of the
  request each replaces, find the placeholders three times, and the request
  blocks of 1,000 targets are read for their keys at most once.
- It is also checked by allocations (`targetcheck_http_test.go`): checking
  1,100 targets of a collector with a placeholder in its path allocates no
  more than five times for each target beyond 100, where it allocated nine
  times.
- Next to them, the previous `setKeys` and `CheckRequestParams` check the
  current ones. Over 4,000 generated collectors, with well-formed or
  malformed placeholders in each part of the request and in label values,
  checked with generated params and overrides through `CheckRequestParams`
  and a `RequestParamsCheck` shared by eight checks, they report the same
  unused params or the same error. Over generated request blocks of a
  collector and of a target, with every field set and unset, they return the
  same keys in the same order.

**Checking each pair once.** A reload asks whether a target file and a
configuration agree up to five times (`Manager.apply` in
`internal/config/config.go`): for the pair that it read, for each file that
it read with the other one in force, and again for the error of each file
that it refuses. It checked a pair each time it asked, the same pair up to
three times, so a refused reload of a target file of 10,000 targets repeated
a check of about 30 ms under the reload lock.

- A reload now checks each pair once, and returns the same verdict, the very
  same error, when it asks again (`pairVerdicts`).
- A target file read alone, whose check opens the descriptor files of the
  configuration read and of the one in force, which are then the same,
  looks through that configuration once (`targetsChecked`).

`BenchmarkRefusedPairReload` in `internal/config/refusedpair_http_test.go`
makes each kind of reload at 10,000 targets, with the files read from disk,
each refused at its last target where it is refused. `checks/op` is how many
checks it made:

```sh
go test -run '^$' -bench 'RefusedPairReload' ./internal/config/
```

As measured on two shared cores, old and new in turn (the times are the
range of two runs):

| Benchmark | Before | After |
| --- | --- | --- |
| `both_agree`, the files agree | 189–192 ms, 52.2 MB, 791,177 allocations, 1 check | 189–192 ms, 52.2 MB, 791,177 allocations, 1 check |
| `targets_refused`, the target file read and refused | 250–253 ms, 56.1 MB, 850,287 allocations, 3 checks | 186–195 ms, 52.1 MB, 790,278 allocations, 1 check |
| `config_refused`, the configuration read and refused | 88–92 ms, 6.1 MB, 90,563 allocations, 3 checks | 30–34 ms, 2.1 MB, 30,550 allocations, 1 check |
| `config_alone`, both read, the configuration goes alone | 259–276 ms, 58.1 MB, 881,197 allocations, 4 checks | 217–221 ms, 54.2 MB, 821,194 allocations, 2 checks |
| `targets_alone`, both read, the target file goes alone | 279–282 ms, 58.1 MB, 880,830 allocations, 4 checks | 249–262 ms, 56.1 MB, 850,829 allocations, 3 checks |
| `neither_alone`, both read and refused | 295–305 ms, 60.1 MB, 910,839 allocations, 5 checks | 251–253 ms, 56.1 MB, 850,831 allocations, 3 checks |

The rest is reading the files. This is checked without a duration:

- `TestAReloadChecksEachPairOfFilesOnce` counts the checks of each of these
  reloads (`targetsValidatedHook`, which only a test sets,
  `test/repository/scanhooks_test.go`): one for each pair asked about.
- `TestATargetFileReadAloneLooksThroughTheConfigurationInForceOnce` counts
  how often the collectors are scanned for a target file read alone.
- The previous reload is kept next to the tests (`oldApply`). Over generated
  sequences of reloads (of http collectors and of a grpc collector whose
  descriptor set is replaced, removed and restored; each file accepted,
  refused for itself or refused because of the other; one file read or both,
  on demand or by a tick, with the watch on and off), two managers of the
  same files, one reloading as before and one as now, leave the same files
  in force, return the same errors, log the same lines in the same order,
  count the same reloads, and watch the same files, stamped alike, for a
  refused reload.

**Collector statistics.** The read of every collector's statistics, which
each scrape of the self-metrics makes under the same lock, has a benchmark
next to that one, `internal/exporter/collectorstats_bench_test.go`:

```sh
go test -run '^$' -bench 'CollectorStats/steady' ./internal/exporter/
go test -run '^$' -bench 'CollectorStats/reloaded' -benchtime 20x ./internal/exporter/
```

- `steady` is a read that finds every collector's statistics already there,
  as every read except the first after a reload does.
- `reloaded` is the first read after a reload that replaced every collector
  with another, which builds the statistics of each with the lock held. The
  reload is not timed and takes far longer than the read, hence the fixed
  number of rounds.

It reports `locked-ns/op` as `FollowReload` does. As measured on two shared
cores, old and new in turn, three times:

| Benchmark | Before | After |
| --- | --- | --- |
| `steady/n=50` | 26–31 µs, 10–11 µs of it under the lock; 20,304 bytes, 70 allocations | 23–31 µs, 0.9–1.3 µs of it under the lock; 17,872 bytes, 60 allocations |
| `steady/n=500` | 270–319 µs, 106–120 µs of it under the lock; 228,560 bytes, 531 allocations | 205–310 µs, 8–13 µs of it under the lock; 194,992 bytes, 510 allocations |
| `steady/n=2000` | 1.3–2.2 ms, 0.5–0.9 ms of it under the lock; 940,216 bytes, 2,054 allocations | 1.1–1.3 ms, 89–92 µs of it under the lock; 779,680 bytes, 2,022 allocations |
| `reloaded/n=50` | 41–70 µs, 29–48 µs of it under the lock; 36,304 bytes, 120 allocations | 29–44 µs, 12–15 µs of it under the lock; 33,872 bytes, 110 allocations |
| `reloaded/n=500` | 397–438 µs, 236–266 µs of it under the lock; 388,560 bytes, 1,031 allocations | 311–332 µs, 129–155 µs of it under the lock; 354,992 bytes, 1,010 allocations |
| `reloaded/n=2000` | 1.7–2.1 ms, 1.0–1.4 ms of it under the lock; 1,580,216 bytes, 4,054 allocations | 1.2–1.3 ms, 0.58–0.59 ms of it under the lock; 1,419,680 bytes, 4,022 allocations |

The read used to take the lock and do everything that produced the
statistics by name under it. Of the 100–110 µs for 500 collectors:

- about 25 were the copy of each collector's definition, 1,224 bytes, which
  `for _, c := range` made to read a name;
- about 50 were the map of the statistics, created without its size and
  grown as it was filled;
- about 9 were the slice of the names, grown in the same way;
- the rest was filling the map and finding each collector's statistics, of
  which only the finding, 6–9 µs, needs the lock.

Now the names are read by index, and the map and the slices are created at
their size before the lock is taken. The statistics are collected into a
slice under the lock, and the map is filled from it once the lock is
released (`collectorStats` in `selfmetrics.go`).

The first read after a reload also builds the statistics of the collectors
that the reload added, a quarter of a microsecond each: 320 bytes, which is
most of it, and a reading of the clock, which marks when the collector's
counters began. It used to build them with the lock held. Now it takes the
lock twice: once to find the statistics there are, and, when some are
missing, once more to keep those it built in between with the lock
released (`statsFoundLocked` and `statsSinceLockedWith` in `server.go`). A
probe that built a collector's statistics in between keeps them, and the
read returns those. A read that finds every collector's statistics takes
the lock once, as before. As measured on two shared cores, old and new in
turn, three times each, 50 rounds:

| Benchmark | Under the lock before | Under the lock after |
| --- | --- | --- |
| `reloaded/n=50` | 15–20 µs | 5.7–7.2 µs |
| `reloaded/n=500` | 131–148 µs | 46–56 µs |
| `reloaded/n=2000` | 0.65–0.76 ms | 0.30–0.35 ms |

The whole read takes as long as before, and one allocation more. The sort
of the names and the copy of each collector's counters, under that
collector's own lock, are unchanged and make up the rest of the read.

`collectorstats_http_test.go` checks this without a duration:

- One read of 500 collectors may make 515 allocations of 210,000 bytes. It
  makes 510 of 194,992, and made 531 of 228,560 while the map and the names
  grew; either one left without its size exceeds the bound on its own.
- Over generated sequences of reloads that remove, add and restore
  collectors, in every order, a server read the new way returns the same
  names, counters and statistics as one read the previous way, which is kept
  next to the test, with and without a reload between the read of the
  configuration and the lock.
- The copy allocates nothing, so the linter checks it: gocritic's
  `rangeValCopy` reports a loop that copies a kilobyte or more per
  iteration, which in this code is only a loop over collectors by value
  (static analysis, below).

**OTLP keys.** `BenchmarkOTLPPoints` (`otlpkeys_bench_test.go`) measures what
an OTLP export does for each of its points, apart from encoding them.
`queue` queues n series under a resource, as a probe or a static target's
scrape does, and empties the queue in order, as an export does. `start`
turns them into the points of an export, each with the start time of its
series:

```sh
go test -run '^$' -bench 'OTLP' -benchtime 2s ./internal/exporter/
```

Both build a key for every point (`otlpKey` in `otlp.go`): what distinguishes
a resource, and a series of a resource, from every other, in the places where
points wait for an export, among the resources of an export, and where start
times are kept.

- A key used to be its parts joined, with a NUL between them and an `=`
  between a name and its value, and the parts come from a target and the
  operator. The series `m{a="1\x00b=2"}` had the key of `m{a="1",b="2"}`,
  and was never exported while the other was queued after it. The resource
  with the attribute `a=b` of value `c` had the key of the one with `a` of
  value `b=c`, and the points of both went out under the attributes of the
  first. A resource whose last attribute held what another's series began
  with shared a start time with it.
- A key is now that join followed by the length of every part, so that no two
  subjects have one key, whatever their parts hold
  (`TestAnOTLPKeyIsReadBackToItsParts` reads every generated key back into
  its parts, which only a test does).
- The join is kept, where the failure log's keys have each part after its
  length, because an export sends its resources, and the points of each, in
  the order of their keys, which was the order of the joins. A NUL of the
  join is written with a `0x01` after it, and two NULs end it; written this
  way, the keys are still in the order of the joins
  (`TestOTLPKeysAreInTheOrderTheOldKeysHad`).
- `TestOTLPExportsAreWhatTheyWereWhereNoTwoSubjectsHadOneKey` checks
  generated runs of exports, byte for byte, against the export with the old
  keys, which are kept next to the tests, wherever no two subjects of a run
  had one old key, and checks every run against an export whose keys belong
  to each subject alone.

The old key grew as it was written, next to a slice of the label names in
order, and a start time's key was joined to its resource's: four allocations
for a series of three labels, five for its start time. The new one is
created at its size, with the parts sorted in place: one allocation
(`TestAnOTLPKeyIsMadeInOneAllocation`). As measured on two shared cores (the
times are the range of four runs, old and new in turn):

| Benchmark | Before | After |
| --- | --- | --- |
| `queue/n=100` | 136–231 µs, 100.8 kB, 721 allocations | 133–242 µs, 87.2 kB, 420 allocations |
| `queue/n=5000` | 6.8–11.3 ms, 5.50 MB, 35,058 allocations | 7.3–10.5 ms, 4.89 MB, 20,057 allocations |
| `start/n=100` | 130–205 µs, 86.0 kB, 912 allocations | 122–209 µs, 67.6 kB, 515 allocations |
| `start/n=5000` | 6.0–10.8 ms, 4.53 MB, 45,069 allocations | 5.8–8.2 ms, 3.61 MB, 25,071 allocations |

A key is stored, where a point waits and where a start time is kept, as
itself up to 256 bytes (`otlpKeyLongest`), and beyond that as its SHA-256
digest (`otlpKeyOf`), so that what a series stores there is bounded whatever
its labels hold. With `otlp.probe_attributes`, a probe's target, up to
8 KiB, was copied into every waiting point and every start time, 8.4 KiB
each, 1.6 GiB at the start times' bound
(`TestWhatIsRememberedOfASeriesIsBoundedWhateverTheTarget`).

- Shorter keys are not turned into digests: a digest of a key of 100 bytes
  takes about half a microsecond on the machine above, a fifth more than a
  point costs in the queue.
- A long key is rebuilt from its point when a queue is emptied, so the
  points are still in the order of their keys, and an export is unchanged
  byte for byte
  (`TestOTLPExportsAreWhatTheyWereWithKeysRememberedByTheirDigests`, against
  the export with every key whole).
- A key is now built in the caller's buffer: one allocation up to 256 bytes
  and two beyond, and a start time that is already known is found with none.

| Benchmark | Keys whole | Keys up to 256 bytes, digests past it |
| --- | --- | --- |
| `queue/n=100` | 262–283 µs, 87.2 kB, 420 allocations | 220–247 µs, 87.2 kB, 420 allocations |
| `queue/n=5000` | 13.8–16.2 ms, 4.89 MB, 20,057 allocations | 13.2–15.2 ms, 4.89 MB, 20,057 allocations |
| `start/n=100` | 236–292 µs, 67.6 kB, 515 allocations | 173–217 µs, 51.8 kB, 415 allocations |
| `start/n=5000` | 10.9–11.7 ms, 3.62 MB, 25,120 allocations | 8.5–9.0 ms, 2.82 MB, 20,134 allocations |

**gRPC descriptor files.** `BenchmarkDescriptorFilesCheck`
(`internal/fetch/grpcdescriptors_bench_test.go`) measures what a call of a
grpc collector with descriptor files costs before it uses them, when they
did not change: the check of each file that the set was read from
(`fileSets.getStamped`), for 1, 10 and 50 files, each at a realistic depth
(`deep`) or behind a link through `..data` as Kubernetes mounts a ConfigMap
(`kubernetes`). `BenchmarkProbeGRPC`
(`internal/exporter/probe_bench_grpc_test.go`) is a whole probe of a
collector with `.proto` files and of one with a descriptor set, against a
server on the same machine:

```sh
go test -run '^$' -bench 'DescriptorFilesCheck' -benchtime 1s ./internal/fetch/
go test -run '^$' -bench 'ProbeGRPC' -benchtime 1s ./internal/exporter/
```

The check determines, as the configuration's watch does, which file a path
leads to and its permissions, in addition to its time and size.

- The watch resolves the path's links (`filepath.EvalSymlinks`), a lookup
  per component. Done at every call, that cost 18–19 µs for one file and
  0.8–1.0 ms for 50, against 2–5 µs and 85–120 µs for the time and size
  alone, and 46 allocations for one file against 5.
- The check takes the file's device and inode from the stat that it makes
  anyway (`fetch.FileIdentity`, `fileidentity_unix.go`, which the watch's
  stamp also writes; on other systems it resolves the links), and formats
  their numbers without allocating. One file costs 4 allocations and 50 cost
  110, against 5 and 160 before, in the same time within the noise of two
  shared cores. A probe of `BenchmarkProbeGRPC` (0.3–0.7 ms here) allocates
  no more than before (560 and 549 allocations against 563 and 551).
- `TestTheQuietCheckOfDescriptorFilesAllocatesNoMoreThanBefore` holds the
  check to no more allocations than the old one, which is kept next to the
  tests.

**Keeping it fast.** What was made fast stays fast through tests, not
benchmarks:

- The decoders, the transforms, the duplicate check and the body read each
  have a test that bounds their allocations per series or per body, and a
  test that compares them, over a table and tens of thousands of generated
  inputs, with the simpler code they replaced, which is kept next to the
  tests.
- The hand-over to Python is compared in the same way, down to the worker's
  previous script (`internal/transform/pythonoracle_test.go`), which a second
  pool of workers runs next to the current one.
- What the failure log costs a scrape whose rules fail has its own
  benchmark, `LogRuleFailures` (nothing failing; one rule and thirty failing
  and remembered; and, as `/full_log`, one and thirty failing while the log
  is full and remembers neither), and a bound that does not depend on the
  length of the rules' expressions, for the remembered failures and for the
  full log alike (see the failure log, above).
- The allocation bounds are skipped under `-race`, which changes what is
  allocated; `make ci` also runs the tests without it.

## Tests that reach the internet

`make ci` never uses the network. The demo configurations under `examples/`
describe real services, though, and a stub replaying a captured response
cannot tell you when one of those services renames a field or a column: the
local tests keep passing while the shipped configuration silently stops
working.

`internal/exporter/external_e2e_test.go` closes that gap by probing the real
endpoints. It is off unless requested:

```sh
EXTERNAL_E2E=1 go test -run TestExternal -v ./...
# or
make test-external
```

Without `EXTERNAL_E2E` the tests are skipped with a message saying how to run
them, so `go test ./...` stays offline and deterministic. They are
deliberately not in CI: a red build caused by someone else's outage teaches
everyone to ignore red builds. Run them when changing a demo configuration,
and occasionally to catch a source that has changed.

A failure here usually means that the service changed or is unreachable, not
that this code broke, and the assertions say so. They check that the probe
returned 200, that each metric the configuration declares is present, and
that the per-row or per-entry labels survived.

The suite also holds the detailed tests of the status page demo,
`internal/exporter/grafanastatus_external_e2e_test.go`, which run its
configuration against a captured copy of status.grafana.com's summary. They
need no network but are opt-in like the rest.

A test file joins the suite by being listed in
`test/repository/externalgate_test.go`. Every test in it must be named
`TestExternal*`, so that `make test-external` picks it up, and must start
with `requireExternalE2E(t)`. `TestExternalSuiteTestsAreOptIn`, which does
run by default, fails when either is missing.

The default suite also covers every example, without the network.

- The tests in `test/repository` that check the examples (against the
  schemas, that each loads, that its scripts satisfy the contract, and that
  a static target file is valid with the configuration in its directory)
  walk `examples/` at any depth. A new example, a file or a directory like
  `examples/open-meteo/`, is therefore covered without a test naming it.
- A directory's collector files are read with its `config.yaml`, not as
  examples of their own, and a collector file that its `collector_files`
  does not name fails the tests.
- Nine examples run against a local stand-in that answers in the documented
  shape of their service, with every series asserted: Filebeat's
  (`internal/exporter/filebeat_example_test.go`), Open-Meteo's
  (`internal/exporter/openmeteo_example_test.go`, which also checks the query
  that the collector sent and scrapes the example's static targets), and the
  ECB's, the METAR one with its static targets, mempool.space's, the
  Prometheus demo server's, Frankfurter's with its pre-script, the USGS
  earthquake feed's with the warning that its incomplete row is logged with,
  and scrapethissite.com's countries page (`ecb_example_test.go`,
  `metar_example_test.go`, `mempool_example_test.go`,
  `promdemo_example_test.go`, `frankfurter_example_test.go`,
  `usgs_example_test.go` and `scrapethissite_example_test.go` next to them,
  on the stand-in of `example_standin_test.go` or, for the last, the plain
  site of `htmlfixtures_test.go`).
- Their fixtures under `testdata/` are written in the shape that each
  service documents, not captured from it, so it is the external suite that
  tells whether a service still answers that way.

The pages under `testdata/html` are written in the same way, each in the
shape of a page that collectors are pointed at: a load balancer's statistics
report, an appliance's layout tables around its data tables, lists and
dashboard cards, markup that no validator would accept, entities and
typographic numbers, what is in a page but is not its content, attribute
names of JavaScript frameworks, the forms that a value takes, and a hosted
status page.

- `internal/exporter/htmlfixtures_test.go` lists every file with what it
  stands for, and a test fails on a file that the list does not have and on
  one that no test reads.
- The `htmlfixtures_*_test.go` files next to it read each page with `css`
  and with `xpath` collectors written as a configuration file holds them:
  through the decoder and the transform directly, through `/probe` from a
  local server sending the `Content-Type` that a real one would, and from
  disk with the `localfile` request type, naming every series and every log
  line.
- The files under `testdata/html/charset` are one page in the encodings that
  a target may answer in (windows-1251, Shift_JIS, UTF-16 and the rest), and
  are deliberately not UTF-8: an editor that saves them as UTF-8 breaks the
  tests that read them. Some of them contain what only looks like a
  declaration of the encoding before their own (a `<meta>` in a comment, a
  description that mentions a charset), and the `.xhtml` ones start with an
  XML declaration, which is their only real declaration and names a
  different encoding from the other `<meta>`.

**golangci-lint.** Static analysis is configured in `.golangci.yml`, so a
local `make lint` and the CI run check exactly the same rules. Install the
pinned version with `make lint-install`; the Makefile and the CI workflow pin
the same version, and a test keeps them in step.

- `make lint` refuses to run with any other installed release: a different
  release enables different checks, so an older one passes locally what CI
  then fails (gosec's G705 taint check, for example, is newer than v2.5).
- `make lint-install` builds the linter with `go install`, which needs a Go
  at least as new as the one that release requires. Homebrew's
  `golangci-lint` or the release binary from GitHub work as well, as long as
  the version matches.

**gopls.** What VS Code and other editors underline comes from gopls, the Go
language server, not from golangci-lint, and some of its analyzers exist
only in gopls (`writestring`, which flags `b.WriteString(a + b)`, for
example). `make gopls-check` runs `gopls check` over every Go file of the
exporter's own (which excludes the copy of gojq in `third_party`) and fails
on any finding, and CI runs it too.

- Like the linter, it is pinned (`GOPLS_VERSION`) and refuses any other
  installed version.
- `make gopls-install` installs it into the same `GOPATH/bin` that the VS
  Code Go extension uses, so the editor then shows exactly what CI checks.
- If you let the extension auto-update gopls, the editor may show findings of
  a newer release before CI has them; bump the pin to follow.

**helm.** helm is pinned the same way: `HELM_VERSION` in the Makefile, which
every workflow that installs helm also installs, and a test keeps them equal.

- `make helm-test` refuses any other installed release, because helm
  releases word the schema's errors and render details differently: a chart
  test that passes with one can fail with another.
- `make helm-install` installs it with `go install`, setting the version at
  link time as helm's release build does. Built from source without that,
  helm reports only its release line, `v4.3`.
- `make helm-test` reads a release line alone as its `.0` release, so a helm
  built from source passes a `v4.3.0` pin and is refused against a `v4.3.1`
  pin. A version with a patch is compared in full, `v4.3.1` against `v4.3.0`
  too. (Homebrew's `helm` works too when its version matches.)
- To move to a newer helm, change `HELM_VERSION` and the workflows'
  `version:` together.

`make helm-test` makes every chart check that `ci.yml` makes: the same
renders, the same values that have to be rejected, and the same lines looked
for in what is rendered. A test
(`TestMakeHelmTestChecksWhatTheCIWorkflowChecks`) reads both and fails when
one has a case that the other lacks, so add a chart case to both.

Both go beyond the plain renders. They check:

- the Recreate strategy without a `rollingUpdate`;
- a monitor's `port` and `namespaceSelector` on both monitor types;
- the garbage collector's target (`goGC.percent` rendered as `GOGC`, and no
  `GOGC` by default);
- whole numbers given as `--set-json` gives them, as floating point, and
  rendered written out with no exponent (a count above 2147483647 is
  refused);
- a configuration file whose first line is indented;
- the chart as a dependency of a parent chart;
- the values that must be refused, among them: a monitor's `auth` without a
  `type`, a monitor's `port` given as a number, a `scrapeTimeout` longer than
  its `interval`, an Ingress without the Service, a mount at the
  configuration directory written with a trailing slash, a `goGC.percent`
  that is `off` with no Go memory limit, `0`, or set beside a `GOGC` entry
  in `env`, a `selfMetrics.path` at the static targets' status page, and an
  Ingress or a `LoadBalancer` or `NodePort` Service without `webAuth`, or
  with `webAuth` beside a configuration whose `web.basic_auth` does not read
  its files (with the renders with `exposure.allowWithoutWebAuth`, and with
  `testdata/chart/web-auth.yaml`, which does read them, next to them).

The Go tests in `test/repository` check the same behaviour in more detail,
but are skipped without helm, so a case worth keeping also goes into these
two lists.

Write a case that inspects what is rendered so that it keeps the render
first (`out="$(helm template ...)"`) and then reads `"$out"`. Neither `make`
nor the workflow's shell fails a pipeline whose first command failed, so a
check that some text is absent would pass on a render that failed. Every
such check in the two lists is written that way, and
`tools/check-manifests.py`, which the render is piped into, refuses a render
with no manifests for the same reason.

A check must also be able to fail, and the same reader reports whether each
one can (`TestEveryHelmCheckOfMakeAndCIFailsWhenItShould`), naming the file,
the line, and the recipe or step of a check that cannot. What it requires of
a check follows from how the two files run their shell:

- **How the shells run.** make runs each recipe line, with the lines
  continued onto it, in its own shell started without `-e`, and the line's
  result is that of its last command. A line of more than one command
  therefore starts with `set -eu`, and no line starts with make's `-`, which
  ignores a failure.
  - The Makefile leaves make's way of running a recipe alone: `.IGNORE`,
    `.ONESHELL`, `.SHELLFLAGS`, `MAKEFLAGS`, a `PATH`, a `SHELL` other than
    sh or bash, and a second rule for `helm-test` are reported.
  - GitHub starts a step's shell as `bash -e`, so a step needs no `set -e`.
    These are reported: a `shell:` (the step's, the job's default or the
    workflow's) that is not bash or sh started with `-e`;
    `continue-on-error` on a step or on the job; a step's `if` other than the
    chart steps'; any `if` on the job; and a `changes` step whose `chart`
    filter no longer includes `charts/**`.
- **A render that must succeed, or a line that must be found**, is a command
  on its own: nothing follows it with `||`, `&&` or `&`, each of which
  discards its failure, under `set -e` too. The one exception hands the
  failure to an `exit`, which ends the shell there: `check || exit 1`, or
  `check || { echo "..." >&2; exit 1; }`, on one line or on several.
- **A render that must be refused, or a text that must be absent**, is the
  only condition of an `if` whose `then` branch runs `exit 1`:
  `if helm template ... >/dev/null 2>&1; then echo "..." >&2; exit 1; fi`.
  Without the `exit 1`, the check prints its message and passes.
- **A check runs on every run of its block.** It stands at the top of the
  block, in the body of a `for name in value ...; do` loop whose values are
  written out, or is the condition of an `if` that stands there, and nowhere
  else: there is no guard that a check may stand behind. A check in a branch
  of an `if` (`if false`, `if [ -n "$SKIP" ]`, the `then` of a refusal),
  after `&&` or `||`, or in a loop over a variable, a command or nothing is
  reported. So is an `exit`, of any status, that does not belong to a check:
  an early `exit 0` ends the block before its checks.
- **Nothing in a block changes what its commands are or how it ends.** These
  are reported: a function or an `alias` (either can stand in for `helm`,
  `grep`, `python3` or `exit`), `.` or `source`, a `PATH`, `hash`, `return`,
  `exec`, `break`, `continue`, `set +e`, a `set` option other than `-e`,
  `-u`, `-x`, `-v` and `pipefail`, and a `trap`. The only trap allowed is the
  clean-up that both lists have, `trap 'rm -rf "$tree"' EXIT`: an `rm` alone,
  on `EXIT`.
- **How a render is read.** helm is piped only into `grep -q` as a command on
  its own, or into `tools/check-manifests.py`, both of which fail on an empty
  render. A kept render holds the one helm command, and is read as
  `echo "$out" |` or `printf '%s\n' "$out" |` into `grep -q` and a pattern,
  `python3 -c` or `tools/check-manifests.py`. Any other way of inspecting a
  render (a here-string, `[[ "$out" == *X* ]]`, a `grep` of a file,
  `grep -c`, `-F` or `-e`) does not count as a check, and is reported by name
  with the line to write instead: `echo "$out" | grep -q -- 'X'`.

The reader reports what it does not understand (`!`, `elif`, `while`,
`case`, a subshell, `{` anywhere except after a check's `||`) instead of
passing it.

- `TestAHelmCheckMadeToothlessIsNoticed` makes each such change (a
  `|| true`, a dropped `exit 1` or `set -eu`, a leading `-`,
  `continue-on-error`, an early `exit 0`, an `if false` around the checks, a
  `trap 'exit 0' EXIT`, a `helm` function, an `if` on the job and the like)
  to an in-memory copy of the Makefile's or the workflow's text, and fails if
  the reader does not report it.
- `TestAHarmlessEditOfAHelmCheckIsTakenOrToldHowToBeWritten` does the same
  with edits that weaken no check, and fails if one is refused without a
  finding that names it and says what to write, or is called a check that can
  never fail.
- A finding that a list "does not make this check of the chart" concerns one
  of the cases that the Go tests make too (`chartCasesMissing` in
  `test/repository/helmtest_test.go` lists them, each with its command line).
  Both lists make it with exactly that command line, so a case changed on
  purpose is changed there and in both lists.

The files that those cases render are in `testdata/chart`: a static target
document, `indented-first-line.yaml`, and `parent`, a chart whose one
dependency is the exporter's chart by a `file://` path.
`helm dependency build` writes a `charts` directory and a `Chart.lock` into
the parent, so `make helm-test` and CI build it in a temporary copy of the
two charts and the working tree stays unchanged; it needs no network. The
parent pins the chart's version, as a real parent does, and a test fails
when a chart version bump leaves it behind.

**The pre-commit hook.** Run `make hooks` once in a clone. It points git at
`.githooks`, whose `pre-commit` hook runs `make precommit` (`gofmt`,
`golangci-lint` and `gopls check` at the pinned versions, and `go vet`) and
refuses the commit when any of them fails, so a commit that CI would fail on
those checks is never made. `git commit --no-verify` skips it once. The tests
are left to `make test` and CI, because the race run takes minutes.

**The linter and the Go toolchain.** The linter pin is coupled to the Go
toolchain. golangci-lint type-checks with the `golang.org/x/tools` that it
was released with, which knows only the Go releases and export data formats
that existed then. Run an older release against a newer toolchain and it
does not report a lint failure; it fails on the standard library:
`file requires newer Go version` for a new minor, or, as v2.13.2 does against
Go 1.27.2, `export data version 5 is greater than maximum supported version
4`. Building it from source with the new Go does not help, because the
format reader comes from its pinned `x/tools`. The build uses the current
stable Go, so the pinned linter must be a release that supports that Go
(v2.14.0 for Go 1.27.2), and needs a bump when a Go release (usually a new
minor, here a patch) outruns it.

In addition to the standard linters, it enables `bodyclose`, `errorlint`,
`gocritic`, `gosec`, `misspell`, `nilerr`, `noctx`, `perfsprint`, `revive`,
`unconvert` and `usestdlibvars`. The repository is gofmt-clean, and CI fails
on unformatted sources instead of rewriting them.

Of gocritic's checks that are off by default, one is on: `rangeValCopy`, from
1,024 bytes. A collector's definition (`model.Collector`) is 1,224 bytes, and
the largest other element that a loop takes by value is 656, in a test, so
what it reports is a loop over collectors by value, which copies every
collector to inspect it. Write `for i := range cfg.Collectors` and take
`c := &cfg.Collectors[i]`, or read `cfg.Collectors[i].Name`. A loop that
needs the copy, to change it and leave the configuration alone, keeps it and
says so beside a `//nolint:gocritic`, as the other exceptions in the code
do. A test in `test/repository` fails when the threshold is raised beyond a
collector's size or the check is dropped.

**govulncheck.** Known vulnerabilities are reported by
`.github/workflows/govulncheck.yml`, on every push and pull request and
weekly on Mondays, because an advisory can be published against code that
has not changed.

- It is for information only: the job never fails, so a new advisory cannot
  block a merge. Findings are written to the run's summary page and raise a
  warning on the run. A govulncheck that could not complete (for example,
  with the vulnerability database unreachable) warns too. Keep it out of the
  branch protection's required checks.
- `make vulncheck` runs the same pinned version locally, and a test keeps
  the Makefile and the workflow in step and checks that the step cannot fail
  the job.

What is scanned is what ships and what the workflows run: the exporter, the
package at the root, and the repository's tools (`. ./tools/...`), each with
everything it imports.

- The packages that only tests use are left out: `internal/grpctest`, the
  stand-in gRPC server of the grpc tests; `internal/sshtest`, the stand-in
  SSH and SFTP server of the sshfile and sshexec tests; `internal/testutil`
  with its `alloctest`; and `test/repository`. An advisory for code that only
  a test reaches, such as the gRPC server (which the exporter never is),
  therefore raises no warning; the cost is that such an advisory is not
  reported at all.
- A test (`TestTheVulnerabilityCheckScansWhatShips`) keeps the Makefile's
  and the workflow's packages equal, fails for a command of the module that
  they do not cover, and holds the list of what is left out: a new package
  is either imported by something scanned or added to that list with its
  reason.

**Workflow files.** `make test` also validates the GitHub Actions workflows:
`test/repository/workflows_test.go` decodes every file under
`.github/workflows` with a parser that rejects duplicate mapping keys, and
checks that each step sets exactly one of `run` or `uses` and uses no unknown
keys. GitHub refuses to create a run for a workflow that it cannot parse,
which produces no jobs at all, so a CI step cannot catch that mistake in the
commit that introduces it: the checker would be in the file that GitHub
refuses to read. Running the tests before pushing is what protects you.

Every `uses:` in a workflow names its action by a full commit SHA, with the
release as a comment: `uses: actions/checkout@<sha> # v7.0.1`. Whoever
controls the action's repository can move a tag to other code, but not a
SHA. Write a new step in that form, looking up the SHA from the release's
tag, and leave the updates to Dependabot, which moves the SHA and the comment
together.
`test/repository/workflowpins_test.go` fails for any other form, a
commented-out `uses:` included, and for a `.github/dependabot.yml` without its
`github-actions` update.

**Chart templates.** `test/repository/charts_test.go` checks that every
manifest a template renders begins its own YAML document. A template that
renders more than one manifest (several declared, or one wrapped in a range)
needs a `---` before each, and neither `helm lint` nor `helm template`
notices a missing one: helm prints whatever the template produced, so two
manifests merge into a single document, and the chart fails only when
someone applies it. `helm-test` and CI render the chart with monitors
enabled, and also fail when the number of manifests exceeds the number of
documents that the output parses into.

A template never prints a number from the values directly. Helm reads the
numbers of a values file, and of `--set-json`, as floating point, and
`{{ .Values.replicaCount }}`, `toString` and `printf "%v"` print one of a
million or more with an exponent, `2e+06`. `--set` passes an integer, so a
test that sets the value with `--set` does not reveal this.

- A whole number goes through the `wholeNumber` helper of `_helpers.tpl`,
  with its name and its range. The helper writes it out and fails rendering
  for a fraction or a number outside the range, and the values schema has
  the same maximum.
- A value that may also be text goes through the `text` helper before it is
  inspected.
- A sub-tree passed to `toYaml` needs neither: `toYaml` writes out a whole
  number of up to eighteen digits, because it goes through JSON, which does
  (`runAsUser: 1000680000`, and `2e9` as `2000000000`). Only a fraction of a
  million or more, and a number beyond what 64 bits hold, come out with an
  exponent.
- `test/repository/chartwholenumbers_test.go` renders each such value from a
  values file, and a test there fails for an integer of the values schema
  that has no maximum and is not listed as one that a sub-tree passes to
  `toYaml`.

**gosec suppressions.** A few `gosec` findings are deliberate and are
suppressed narrowly, with the reason stated at the suppression:
`request.tls.insecure_skip_verify` is a documented opt-in, and the exporter
necessarily reads the configuration, target document and credential files
whose paths the operator supplies (G304).

Two more are limited to the one file each applies to instead of being
excluded globally:

- G704 reports the outbound request as server-side request forgery, which
  accurately describes what this program is: an exporter whose job is to
  fetch a URL that an operator supplied. It is excluded on
  `internal/fetch/fetcher.go` only. The exposure is bounded by the scheme
  allowlist, the response size limit and each collector's
  `request.allowed_targets` and `request.denied_targets`, which the operator
  sets to say which hosts, addresses and networks its requests may reach,
  checked again on every redirect and connection
  (`internal/fetch/targetpolicy.go`).
- G703 reports the Dockerfile path that `tools/depupdate` takes on the
  command line as attacker-controlled. That is a developer tool with no
  untrusted caller, so it is excluded under `tools/`.

## The Go toolchain

CI asks setup-go for `stable`, so the build always uses the current stable
Go release, and no workflow needs editing when Go ships a new one.

- The release builds its binaries with the Go that the image is built with:
  it reads `GO_VERSION` from the Dockerfile and has setup-go resolve that
  minor to its newest patch release, so the archives and the image of one
  release use the same Go.
- No workflow installs Go with `go-version-file`, which would pin the exact
  version of the `go` directive, an old patch release without its security
  fixes; a test fails if one does.
- The `go` directive in `go.mod` is something different: it is the *minimum*
  that the module requires, raised by dependency updates and not by
  whichever toolchain builds it, so it stays where the dependencies put it.
- The Dockerfile pins `GO_VERSION` to a released minor, which
  `tools/depupdate` keeps moving within the major.

The two can drift apart in a confusing way: a dependency bump raises the go
directive in a pull request that touches no workflow, and from then on every
build fails with `go.mod requires go >= X`, with nothing nearby to explain
it. `test/repository/goversion_test.go` ties them together: it checks that
every Go version that the workflows request, and the one that the Dockerfile
pins, satisfies the go directive, so `go test ./...` catches the mismatch
instead of the next red build.

**Changed paths.** GitHub Actions uses changed-path detection
(`.github/workflows/ci.yml`).

- The checks of the whole repository (formatting, lint, gopls, vet and build,
  also with each request type on its own, and the repository's own tests,
  `test/repository`) run for changes to anything that the repository tests
  read: Go sources, `go.mod` and `go.sum`, the workflows, `charts/`,
  `configs/`, `examples/`, `testdata/`, `docs/` and every top-level `*.md`,
  the `Dockerfile`, `.dockerignore`, the `Makefile`, `tools/`,
  `.golangci.yml`, `test/python/`, `.github/dependabot.yml`, `.gitignore`,
  `.githooks/`, `LICENSE` and `.artifacthub-repo.yml` (the filter's `go`
  output). A documentation-only change therefore runs the repository tests
  too, which check the docs.
- `TestTheGoFilterMatchesEveryFileTheRepositoryTestsRead` takes every string
  literal of `test/repository`'s Go files that names an existing path of the
  repository, from its root, and fails for a file that no pattern of the
  `go` output matches: a file that those tests read, whose change alone
  skipped them, would be merged unchecked. When it fails, add the file to the
  `go` output in every job.
- Under the same output, `build` builds the code for `darwin` on `amd64` and
  `arm64`, linking both commands, because the release builds archives for
  macOS from it. `image` builds the image from the `Dockerfile` with
  `docker/build-push-action`, pushing nothing, loads it, and runs its
  `--version` to check the revision that it was given. A change that breaks
  either fails on its pull request instead of at release time.
- The Helm lint and template checks run for changes under `charts/`,
  `configs/` and `testdata/chart/`, the files that the chart steps render. A
  change under `charts/` runs the Go checks because the template guard above
  lives there, and is least useful on exactly the changes that would break
  it.
- A tag push runs everything. The filter compares a tag with its merge base
  on `main`, which for a tag on `main`'s newest commit finds no change, so
  the steps run under the outputs of the step `changes` after the filter:
  all true on a tag push, and the filter's own otherwise
  (`TestATagPushRunsEveryStepOfCI` runs that step's script). A step reads
  those outputs, never the filter's own.

**Sides.** The tests of the packages themselves are split into two sides,
each run only when something that it depends on changed.

- `tools/ci-packages.sh allocator` lists the target allocator's packages,
  `cmd/pue-allocator` and `internal/allocator`.
  `tools/ci-packages.sh exporter` lists every other package except
  `test/repository`, so a new package lands on the exporter's side by
  default.
- The filter's `allocator` and `exporter` outputs list, for each side: the
  directories of the module's packages that its packages and their tests are
  built from (`go list -deps -test`); the files outside those that its tests
  open (the exporter's tests read `examples/`, `configs/`, `testdata/`,
  `charts/`, `docs/`, the README and the `Dockerfile`); and what CI runs them
  with (`go.mod`, `go.sum`, `ci.yml`, the scripts, and for the exporter
  `test/python/`).
- A change to only `internal/allocator` runs the allocator's tests and the
  repository checks, not the exporter's. A change to only
  `internal/exporter` runs both sides, because the allocator's end-to-end
  test runs the exporter.
- `test/repository/cisides_test.go` recomputes the lists (every Go file,
  embedded file and `testdata/` file of those packages, and every existing
  path outside its own directory that a string literal of a side's package
  names). It fails for a file that a side needs and its list leaves out, for
  an entry that matches none of them, for a file that a side's list matches
  and the `go` output does not (the setup steps run under `go`), and for a
  package tested by no side or by both. When it fails for a new package or a
  new file that a test reads, add its directory or the file to the side's
  list in every job.

**Jobs.** CI runs its jobs side by side, so it takes about as long as its
longest job:

- `lint` runs gofmt, golangci-lint (the default build, the opt-in sql
  drivers' and the opt-in request types') and gopls.
- `build` runs `go vet`, the default build, darwin's, and the builds with
  the opt-in sql drivers and request types with their tests.
- `image` builds the image, with its layers read from and written to the
  GitHub Actions cache (`type=gha`, `mode=max`) through buildx, so a change
  to the sources alone downloads no module and installs nothing in the
  runtime stage again.
- `race` runs the race suite of `internal/exporter`, which has by far the
  most tests, and `race-rest` that of every other package of the exporter's
  side (`tools/ci-packages.sh exporter-core` and `exporter-rest`, which
  together list each package of the side once, as a test checks).
- `request-types`, a matrix over the request types (its checks are named
  `test (<type>)`), vets, builds and lints each type on its own under the
  `go` output, and runs the exporter's tests with it under `exporter`.
- `scaffold` runs `TestANewRequestTypeIsWiredByTheScaffold` alone under
  `go`: the minutes that it takes to build a copy of the tree with two new
  request types. The step of `test` that runs the repository tests skips it
  with `-skip`.
- `allocator` runs the allocator's tests, once and then under the race
  detector, when the `allocator` output reports a change. The allocator's
  tests are not run with each type on its own: those that need a type are
  built with it, so such a build runs only part of them, and each type's job
  already vets and builds the allocator with it.
- `test` keeps the repository tests, the exporter's tests (under `exporter`)
  and the chart steps; a test fails when it runs a linter or a build again.

Each of the other jobs starts with `test`'s setup steps except helm's,
written out again, and `TestTheGoJobsOfCIStartWithTheSetupOfTest` fails when
a copy differs from `test`'s, so change them in every job at once. `image`
runs no Go and no Python, which the `Dockerfile` brings itself, so it starts
with only the first three of them (checkout, the filter and the step
`changes`), and the test requires the whole setup of a job as soon as it
sets up or runs Go or Python.

The jobs gate their steps on the changed-path filter's outputs instead of
using a job-level `if`: a job whose steps are skipped still passes under its
own name, while a matrix job skipped as a whole is reported under a name that
no check expects.

`make ci` runs the same commands one after another, each over every package
(`./...`) where CI names a side's packages, so it runs everything whatever
changed. `make test` skips the scaffold test as `test` does, and
`make test-scaffold` runs it as `scaffold` does.
`TestTheScaffoldTestRunsInAJobOfItsOwn` fails for a test that a go command of
CI or of `make ci` skips and that no other command there runs with `-run`,
so a test moved out of a suite into its own job still runs.

**Permissions.** Every workflow sets `permissions` at its top, read-only, so
no job runs with the repository's default token, which may write to
everything. A job that has to write grants itself the scopes it needs, and
`test/repository/workflowpermissions_test.go` lists each job's write grants
and fails for any other, so a new grant is a deliberate change to that list.
The release workflows publish only once CI has passed on the tagged commit:
their `preflight` job runs `tools/ci-passed.py`, which `cipassed_test.go`
runs against a stand-in of the GitHub API (`docs/RELEASING.md` says what
counts as passed and how to release once it has).
