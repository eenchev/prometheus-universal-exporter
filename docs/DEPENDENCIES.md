# Dependency updates

Updates are proposed, never applied: each one arrives as a pull request.

The repository keeps no `vendor/` directory: builds fetch the modules `go.sum`
pins from the module proxy. A vendor directory made locally with
`go mod vendor`, to build without network access, is ignored by git
(`.gitignore`) and left out of the image's build context (`.dockerignore`),
where `go build` would otherwise use it in place of the pinned modules.

Dependabot handles the Go modules and the GitHub Actions, configured in
`.github/dependabot.yml`. The Go modules' minor and patch updates are grouped
into one pull request, and their major bumps are excluded — a Go major version
lives at a different import path and needs real work. The GitHub Actions'
updates are grouped into one pull request, major versions included. The
workflows pin every action to a full commit SHA with its release as a comment
(`actions/checkout@<sha> # v7.0.1`), and Dependabot updates the SHA and the
comment together. Dependabot also handles the Python modules only the tests
use, pinned in `test/python/requirements.txt`, neither grouped nor excluding
major bumps — `prometheus-client`, whose strict OpenMetrics parser the suite runs the
exporter's answers through; CI runs the suite on a change to that file, with
the module required (see [Repeatable tests](DEVELOPMENT.md#repeatable-tests)).

The Dockerfile is updated by `.github/workflows/update-docker-deps.yml` instead.
Dependabot cannot read it: `GO_VERSION`, `PYTHON_VERSION` and the pip pins are
build arguments interpolated into the `FROM` lines and the `pip install`, which
the Docker ecosystem updater does not resolve. The resolver lives in
`tools/depupdate`, so its rules are covered by `go test ./...` like everything
else. It never crosses a major version, keeps each pin's granularity (`1.27`
stays two-component, because a two-component image tag already picks up patch
rebuilds and pinning it to `1.27.4` would freeze it), skips pre-releases and
yanked PyPI files, and draws image candidates from the exact tag the build
pulls, so a proposed version is known to exist as `golang:<version>-alpine`
rather than merely to have been released. A test keeps the resolver's pin table
and the Dockerfile in agreement, so a renamed build argument cannot leave a
dependency unwatched.

Go and the three Python libraries move by minor and patch versions. Python
itself does not move to another feature release (the `3.12` of
`PYTHON_VERSION`): your collector scripts run on that interpreter, a feature
release removes standard-library modules and changes behaviour, and the suite
cannot test scripts it does not have. `PYTHON_VERSION` therefore stays as it
is — a two-component `3.12` already picks up patch rebuilds, and a
three-component `3.12.4` moves to the newest `3.12.x` only. When a newer
feature release is published as `python:<version>-slim`, the pull request the
updater opens for other pins says so:

```text
Python 3.14 is available; PYTHON_VERSION stays at 3.12 until it is bumped by hand, because collector scripts run on it.
```

A newer Python alone opens no pull request; the line is in the workflow run's
log either way. To move, change `ARG PYTHON_VERSION` in the Dockerfile
yourself: CI reads the version from there and runs the Python tests on it,
and a test fails until the pages that write the base image out, such as the
chart README's `python:3.12-slim`, say the same.

When anything moves, the workflow installs the pinned golangci-lint and gopls
(`make lint-install gopls-install`) and the Python and libraries the updated
Dockerfile pins, runs the whole suite (`make ci`) and builds the image against
the new versions, and opens the pull request only if that passes; a test fails
if it stops installing any of them. Run it
by hand with the workflow dispatch button, or locally:

```sh
go run ./tools/depupdate --dry-run
```

### The pull request

The workflow opens the pull request with the repository's `GITHUB_TOKEN`, which
has two consequences.

The repository has to allow it: **Settings → Actions → General → Workflow
permissions → "Allow GitHub Actions to create and approve pull requests"**.
Without that setting the run fails at its last step with `GitHub Actions is not
permitted to create or approve pull requests`, after it has already pushed the
`deps/dockerfile` branch; turn the setting on and run the workflow again, or
open the pull request from that branch by hand.

And a pull request opened with that token does not start `ci.yml`: GitHub does
not run workflows for events its own token causes. The update workflow has
already run `make ci` and the image build on the new versions before it opened
the pull request, so the proposal is tested even though it shows no checks.
To run CI on the pull request itself, close it and reopen it.

Both schedules land mid-morning on a Tuesday in Sofia. Dependabot uses an
explicit `Europe/Sofia` timezone; the workflow's cron is UTC, which has no
daylight saving, so `0 8 * * 2` is 10:00 in winter and 11:00 in summer.

The default test suite is intentionally local-only; no third-party endpoint is required. The opt-in external checks in [DEVELOPMENT.md](DEVELOPMENT.md) are the exception, and they do not run unless you ask for them. The exporter exposes `/health`, `/ready`, `/self-metrics`, and `/probe`.

## Dockerfile build arguments

The Dockerfile exposes `GO_VERSION`, `PYTHON_VERSION`, `LXML_VERSION`,
`PYYAML_VERSION`, and `PYTHON_DATEUTIL_VERSION` build arguments, all with pinned
defaults. Override them with `docker build --build-arg NAME=value`.

`REQUEST_TYPES` is a build argument too, but not a pinned version: empty by
default, which builds every request type but the opt-in ones, or a
comma-separated list such as `http` to build only those. See
[Choosing request types at build time](CONFIGURATION.md#choosing-request-types-at-build-time).
`OPT_IN_REQUEST_TYPES` adds the opt-in types, which the published image
leaves out: `s3file` ([Objects in S3](S3FILE.md#building-with-s3file)),
`redis` ([Redis and Valkey](REDIS.md#building-with-redis)), `tcp`
([Text protocols over TCP](TCP.md#building-with-tcp)) and `ldap`
([LDAP directories](LDAP.md#building-with-ldap)).
`SQL_DRIVERS` adds the `sql` type's opt-in drivers.

## What the image contains

The runtime image is `python:<PYTHON_VERSION>-slim` with the exporter binary,
the [target allocator](ALLOCATOR.md)'s, `/bin/pue-allocator`, and three
Python libraries: lxml, PyYAML and python-dateutil. BeautifulSoup is not
bundled; `lxml.html` parses HTML (see [PYTHON.md](PYTHON.md)).

The build keeps the image's known vulnerabilities down to the ones nobody has
fixed yet:

- `apt-get upgrade` applies every Debian security update published by build
  time, so a rebuild picks up a fix without waiting for a new `python` image.
- pip is uninstalled after the libraries are installed, along with the wheels
  `ensurepip` keeps. The exporter never installs a package at runtime, so pip is
  only attack surface, and its advisories would otherwise be reported against
  the image.
- `LXML_VERSION` stays at 6.1.0 or later, the first release that fixes
  CVE-2026-41066. The scheduled updater never crosses a major version, so a
  security fix in a new major, like this one, is a manual bump.

A test (`TestDockerfileImageContents`) keeps all three in place.

The image runs as user and group 65532, named by number (`USER 65532:65532`)
rather than by name: Kubernetes can check `runAsNonRoot` only against a numeric
user, and refuses to start a pod whose image names one. The chart's
`podSecurityContext` sets the same user, group and `fsGroup`, so mounted
Secrets and ConfigMaps are readable. `TestTheImageUserIsNumeric` keeps the two
in step.

Scanners also report Debian packages in the base image — util-linux, glibc,
systemd, ncurses and others — for which Debian has not published a fix. The image
cannot fix those; they go away when the image is rebuilt after Debian ships the
fix, which `apt-get upgrade` then applies.

## Go modules

The exporter's direct dependencies are `gojq` (jq and yq expressions),
`antchfx/xmlquery`, `antchfx/htmlquery` and `antchfx/xpath` (XPath), `goquery`
(CSS selectors) and `andybalholm/cascadia`, the selector compiler goquery uses
underneath, which the exporter calls itself to compile each CSS selector once
and keep it (`internal/expr/exprcache.go`), `gopkg.in/yaml.v3`,
`github.com/BurntSushi/toml`, which the `toml` decoder reads a document with
(`internal/decode/toml.go`, see [NDJSON, logfmt, TOML and
INI](CONFIGURATION.md#ndjson-logfmt-toml-and-ini)), and which `go.mod`
already required as another module's dependency; `golang.org/x/text` for its character encodings (`encoding/htmlindex`,
`encoding/unicode`), which convert a response in an encoding other than UTF-8
to UTF-8 (`internal/decode/textencoding.go`, see [Character
encodings](CONFIGURATION.md#character-encodings)), and `golang.org/x/net` for
its `http/httpproxy` package, which reads the proxy environment variables per
transport rather than once per process; `golang.org/x/net` was already in the
build for goquery's HTML parser. The Prometheus text format is parsed by
the exporter itself (`internal/decode/promparse.go`), not by `prometheus/common`: that module
brought `prometheus/client_model`, the protobuf runtime and `goautoneg` with it
for one function, and a test fails if `go.mod` requires `prometheus/common`,
`client_model` or `goautoneg` again. The `ndjson`, `logfmt` and `ini`
decoders need no module: NDJSON is read by the exporter's own JSON decoder,
line by line, and logfmt and INI by small parsers of its own
(`internal/decode/logfmt.go`, `ini.go`), the dialects of INI agreeing on too
little for a module to settle.

The [`grpc`](GRPC.md) request type has three direct dependencies of its own:
`google.golang.org/grpc`, which also carries the reflection client and the
health service's types; `google.golang.org/protobuf`, for building messages
from descriptors (`dynamicpb`, `protodesc`) and the JSON mapping
(`protojson`); and `github.com/bufbuild/protocompile`, the pure-Go `.proto`
compiler behind `descriptors: proto`. Only the type's build-tagged files
import them, so a build without the type does not link them, and a test
(`TestOnlyTheTypesThatNeedThemLinkTheirModules`) keeps it so. Measured with
`-trimpath -ldflags="-s -w"` as the image builds, they add about 6 MB: 23.7 MB
for every type but `sql` against 17.9 MB for
`REQUEST_TYPES=http,localfile,graphite,sshfile`.
Dependabot's Go group and `govulncheck` cover them with no change.

The [`dns`](DNS.md) and [`tlscert`](TLSCERT.md) request types have no
dependency of their own: the first asks Go's own resolver (`net`), the second
makes its handshake with `crypto/tls` and verifies with `crypto/x509`, and the
same test finds none of the other types' modules in a build of either.

Nor has the [`kubernetes`](KUBERNETES.md) request type: it lists objects
over the API server's REST API with `net/http` and `encoding/json`, through
`internal/kubeapi`, which the target allocator's Kubernetes discovery shares,
and no Kubernetes client library (`k8s.io/client-go`), whose dependencies
would add some tens of megabytes to the binary for what a few `GET`s with a
continue token do. The same test finds none of the other types' modules in a
build of it.

Nor has the opt-in [`redis`](REDIS.md) request type, which a build carries
only when `OPT_IN_REQUEST_TYPES` names it: it speaks RESP2, the protocol of
Redis and Valkey, over `net` and `crypto/tls` with a client of its own of
some two hundred lines (`internal/fetch/redisresp.go`), and no Redis client
library (`github.com/redis/go-redis`, `github.com/gomodule/redigo`), whose
pools, cluster routing and pub/sub a scrape of a few read-only commands has
no use for, and whose readers allocate what a reply's length declares. The
same test finds none of the other types' modules in a build of it, and the
stand-in server of its tests (`internal/redistest`) is standard library
alone too.

Nor has the opt-in [`tcp`](TCP.md) request type: it writes a line and reads
the answer with `net` and `crypto/tls`, the protocol being whatever the
service's text is, which the decoders and transforms read as they read a
body. A test finds that a build of it alone links no package that a build
of no request type does not, but the standard library's, and the stand-in
server of its tests (`internal/testutil`'s `TCPStub`) is standard library
alone too.

Nor has the opt-in [`ldap`](LDAP.md) request type, which a build carries
only when `OPT_IN_REQUEST_TYPES` names it: it speaks LDAPv3 over `net` and
`crypto/tls` with a client of its own, the BER of the few operations a
scrape sends and reads (`internal/fetch/ldapber.go`) and a parser of
RFC 4515 filters (`internal/fetch/ldapfilter.go`), and no LDAP client
library (`github.com/go-ldap/ldap`), which would bring `go-asn1-ber` and an
NTLM and GSSAPI stack a simple bind has no use for, and whose BER reader
allocates what a message's length declares. The same test finds none of the
other types' modules in a build of it, and the stand-in server of its tests
(`internal/ldaptest`) is standard library alone too.

The `http` and `graphite` types' [login](AUTHENTICATION.md#logging-in-to-a-target)
has one: `golang.org/x/oauth2`, whose `clientcredentials` package asks a token
endpoint for a token with the OAuth 2.0 client credentials grant
(`internal/fetch/login.go`), and which `go.mod` already required as the
`snowflake` driver's dependency. Only the file of the login imports it, built
with either type, so a build of neither, such as `REQUEST_TYPES=grpc`, does
not link it. A `json` login needs no module: it is a request the exporter
makes itself, read with `gojq`.

The [`sshfile`](SSHFILE.md) request type has two direct dependencies of its
own: `golang.org/x/crypto`, for its SSH client (`ssh`) and the check of a
host's key against a known_hosts file (`ssh/knownhosts`), and
`github.com/pkg/sftp`, the SFTP client it reads files with. As for `grpc`,
only the type's build-tagged files import them, the in-process SSH server of
its tests (`internal/sshtest`) included, so a build without the type does not
link the SSH packages and the SFTP client, and a test
(`TestOnlyTheTypesThatNeedThemLinkTheirModules`) keeps it so; the rest of
`golang.org/x/crypto` is not its alone, since the `sql` type's `sqlserver`
driver links its MD4, for NTLM. They add about 1 MB to the binary.

The [`sql`](SQL.md) request type has a direct dependency for each of its
database drivers, each imported by one build-tagged file
(`internal/fetch/sqldriver_<name>.go`) and by nothing else:

- built in with the type: `github.com/go-sql-driver/mysql`, for MySQL and
  MariaDB; `github.com/jackc/pgx/v5`, for PostgreSQL, through its
  `database/sql` adapter (`stdlib`); and `github.com/microsoft/go-mssqldb`,
  for SQL Server;
- built in only with their `sql_driver_<name>` tag (`SQL_DRIVERS`):
  `github.com/sijms/go-ora/v2`, for Oracle, pure Go;
  `github.com/ClickHouse/clickhouse-go/v2`, for ClickHouse, which brings
  `ch-go`, compression libraries and OpenTelemetry's trace API;
  `github.com/snowflakedb/gosnowflake`, for Snowflake, which brings the AWS
  and Azure storage SDKs and Apache Arrow, by far the largest; and
  `github.com/vertica/vertica-sql-go`, for Vertica.

A build without the type links no driver, the default build links the three
built-in ones and none of the four others, and a build with an opt-in driver
links its modules and no other's: a test
(`TestOnlyTheBuildsWithASQLDriverLinkIt`) keeps it so. Measured as the image
builds, the three built-in drivers add about 6 MB, 29.8 MB for the default
build against 23.7 MB for one without `sql`, and the four opt-in ones about
46 MB more: a build with all seven drivers is 76.0 MB, most of it
Snowflake's. CI vets, builds and
tests a build with all four opt-in drivers, which no other build compiles
(see [Development](DEVELOPMENT.md#the-sql-request-type)). The stand-in
database of the tests (`internal/sqltest`) uses `database/sql/driver` alone.

The [`s3file`](S3FILE.md) request type, which a build carries only when
`OPT_IN_REQUEST_TYPES` names it, has four direct dependencies of its own,
all of the AWS SDK for Go v2: `github.com/aws/aws-sdk-go-v2`, the SDK's
core, for its configuration and credential types (`aws`);
`github.com/aws/aws-sdk-go-v2/config`, which finds the region and the
credentials by the SDK's default chain — the environment, a web identity
token, the shared files, the container's or the instance's role — and
brings the credential providers and the STS, SSO and instance metadata
clients they use; `github.com/aws/aws-sdk-go-v2/service/s3`, the S3 client
that signs and sends the HeadObject, GetObject, HeadBucket and
ListObjectsV2 requests; and `github.com/aws/smithy-go`, the SDK's runtime,
whose `APIError` tells a missing bucket by its error code. They were in
`go.mod` already, as indirect dependencies of the `snowflake` driver, at
the same versions. Only the type's two files, built under its tag alone
(`internal/fetch/requesttype_s3file.go` and `s3source.go`), import them,
so the default build and the published image link none of the AWS SDK, and
a test (`TestOnlyTheTypesThatNeedThemLinkTheirModules`) keeps it so, finding
the S3 client in a build with the type and no AWS module in one without
it; the `snowflake` driver links the AWS SDK too, its S3 client among it,
which is that driver's build to carry
(`TestOnlyTheBuildsWithASQLDriverLinkIt`). Measured as the image builds,
they add about 6.7 MB: 37.0 MB for the default build with `s3file`
against 30.3 MB without it. CI vets, builds, lints and tests a build with
it beside the default build's types, and each build of one type, `s3file`
among them. The stand-in store of the tests is an `httptest` server answering
the calls a read makes, in the tests themselves, with no SDK code.
