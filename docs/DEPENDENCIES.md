# Dependency updates

Updates are proposed, never applied: each one arrives as a pull request.

Dependabot handles the Go modules and the GitHub Actions, configured in
`.github/dependabot.yml`. Each ecosystem is grouped into one pull request, and
major bumps are excluded — a Go major version lives at a different import path
and needs real work. It also handles the Python modules only the tests use,
pinned in `test/python/requirements.txt` — `prometheus-client`, whose strict
OpenMetrics parser the suite runs the exporter's answers through; CI runs the
suite on a change to that file, with the module required
(see [Repeatable tests](DEVELOPMENT.md#repeatable-tests)).

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
default, which builds every request type, or a comma-separated list such as
`http` to build only those. See
[Choosing request types at build time](CONFIGURATION.md#choosing-request-types-at-build-time).

## What the image contains

The runtime image is `python:<PYTHON_VERSION>-slim` with the exporter binary and
three Python libraries: lxml, PyYAML and python-dateutil. BeautifulSoup is not
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
(CSS selectors), `gopkg.in/yaml.v3`, and `golang.org/x/net` for its
`http/httpproxy` package, which reads the proxy environment variables per
transport rather than once per process; `golang.org/x/net` was already in the
build for goquery's HTML parser. The Prometheus text format is parsed by
the exporter itself (`internal/decode/promparse.go`), not by `prometheus/common`: that module
brought `prometheus/client_model`, the protobuf runtime and `goautoneg` with it
for one function, and a test fails if `go.mod` requires `prometheus/common`,
`client_model` or `goautoneg` again.

The [`grpc`](GRPC.md) request type has three direct dependencies of its own:
`google.golang.org/grpc`, which also carries the reflection client and the
health service's types; `google.golang.org/protobuf`, for building messages
from descriptors (`dynamicpb`, `protodesc`) and the JSON mapping
(`protojson`); and `github.com/bufbuild/protocompile`, the pure-Go `.proto`
compiler behind `descriptors: proto`. Only the type's build-tagged files
import them, so a build without the type does not link them, and a test
(`TestOnlyTheGRPCRequestTypeLinksGRPC`) keeps it so. Measured with
`-trimpath -ldflags="-s -w"` as the image builds, they add about 6 MB: 19.7 MB
for every type against 13.8 MB for `REQUEST_TYPES=http,localfile,graphite`.
Dependabot's Go group and `govulncheck` cover them with no change.
