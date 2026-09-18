# Dependency updates

Updates are proposed, never applied: each one arrives as a pull request.

Dependabot handles the Go modules and the GitHub Actions, configured in
`.github/dependabot.yml`. Each ecosystem is grouped into one pull request, and
major bumps are excluded — a Go major version lives at a different import path
and needs real work.

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

When anything moves, the workflow runs the whole suite and builds the image
against the new versions, and opens the pull request only if that passes. Run it
by hand with the workflow dispatch button, or locally:

```sh
go run ./tools/depupdate --dry-run
```

Both schedules land mid-morning on a Tuesday in Sofia. Dependabot uses an
explicit `Europe/Sofia` timezone; the workflow's cron is UTC, which has no
daylight saving, so `0 8 * * 2` is 10:00 in winter and 11:00 in summer.

The default test suite is intentionally local-only; no third-party endpoint is required. The opt-in external checks in [DEVELOPMENT.md](DEVELOPMENT.md) are the exception, and they do not run unless you ask for them. The exporter exposes `/health`, `/ready`, `/metrics`, and `/probe`.

## Dockerfile build arguments

The Dockerfile exposes `GO_VERSION`, `PYTHON_VERSION`, `BEAUTIFULSOUP4_VERSION`,
`LXML_VERSION`, `PYYAML_VERSION`, and `PYTHON_DATEUTIL_VERSION` build arguments,
all with pinned defaults. Override them with `docker build --build-arg NAME=value`.
