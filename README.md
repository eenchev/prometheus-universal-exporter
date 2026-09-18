# prometheus-universal-exporter

`prometheus-universal-exporter` is a Go HTTP-to-Prometheus adapter. Prometheus Operator discovers the real target; the exporter receives it as `target` and selects server-side collector configuration with `collector`:

```text
/probe?target=http%3A%2F%2Flegacy.example%3A8080&collector=legacy_text
```

The repository includes a self-contained Go service, a Helm chart, a configuration example, and a scheduled-target example. Start locally with:

```sh
go run . --config.file=config.example.yaml
```

The full implementation specification is [docs/SPECIFICATION.md](docs/SPECIFICATION.md).

The Helm chart supports `defaultLabels` and `defaultAnnotations` for metadata
that should be applied to every chart-created Kubernetes object. Object-specific
metadata overrides a same-named default.

## Releases

Pushing a semantic version tag such as `v1.0.0` runs the release workflow. It
publishes the container image to GHCR, publishes the Helm chart as an OCI
artifact, and creates a GitHub Release containing the chart archive and
cross-platform software archives:

```sh
git tag -a v1.0.0 -m "Release v1.0.0"
git push origin v1.0.0
```

The resulting artifacts are available as
`ghcr.io/eenchev/prometheus-universal-exporter:1.0.0` and
`oci://ghcr.io/eenchev/charts/prometheus-universal-exporter`, respectively.
The GHCR packages may need to be made public once in the repository's package
settings.

Exporter self-health metrics are available at `/self-metrics` by default (and `/metrics` remains a compatibility alias). Change the dedicated path with `--web.self-metrics-path=/exporter/metrics`. The Helm chart's optional self-metrics ServiceMonitor/PodMonitor scrapes the exporter pods/services separately from target-probing monitors. Configure one or more entries in `monitors`, each with a unique `name` and `type: pod` or `type: service`; each entry supports Prometheus Operator `relabelings` and `metricRelabelings`.

The Dockerfile exposes `GO_VERSION`, `PYTHON_VERSION`, `BEAUTIFULSOUP4_VERSION`,
`LXML_VERSION`, `PYYAML_VERSION`, and `PYTHON_DATEUTIL_VERSION` build arguments,
all with pinned defaults. Override them with `docker build --build-arg NAME=value`.

Optional OTLP/HTTP JSON export is configured at the top level. Probe metric sets and self-health metric sets are forwarded when enabled:

```yaml
otlp:
  enabled: true
  endpoint: http://otel-collector:4318/v1/metrics
  service_name: prometheus-universal-exporter
  timeout: 5s
  interval: 30s
  # headers:
  #   X-OTLP-Tenant: production
  # tls:
  #   ca_file: /etc/prometheus/tls/ca.crt
  #   cert_file: /etc/prometheus/tls/client.crt
  #   key_file: /etc/prometheus/tls/client.key
  #   insecure_skip_verify: false
  # resource_attributes:
  #   deployment.environment: production
```

OTLP export is best-effort and does not make a Prometheus probe fail. Metric
values are buffered as latest values and exported every `otlp.interval`;
the default is 30 seconds. Each export request is bounded by `otlp.timeout`,
which defaults to 5 seconds. Self-health metrics are included in every export
interval even when no Prometheus self-metrics scrape is running.

### Scheduled targets

The exporter can also scrape a fixed list of targets itself and deliver only
those metrics over OTLP, with no Prometheus involved. Pass the list with
`--otlp.targets-file`; `targets.example.yaml` is a complete example, and
`config.otlp.example.yaml` is the matching exporter configuration with OTLP
export enabled:

```yaml
targets:
  - name: legacy_eu
    collector: legacy_text
    target: http://legacy.eu.example:8080
    request:
      path: /status
      timeout: 5s
      retry:
        attempts: 2
        backoff: 2s
      headers:
        X-Tenant: team-a
      bearer_token_file: /var/run/prometheus-universal-exporter/target-auth/token
    labels:
      region: eu
    otlp:
      service_name: legacy-app
      resource_attributes:
        deployment.environment: production
```

Each target names a collector from the exporter configuration and takes every
per-scrape parameter `/probe` accepts — `method`, `path`, `body`, `timeout`,
`insecure_skip_verify` and the `retry` settings — overriding the collector's own
request for that target only. It also takes static `headers` and its own target
credentials, inline or file-backed, as basic authentication or a bearer token.
Because the file is operator configuration rather than caller input, these
headers are applied directly and are not filtered through the collector's
`request.forward_headers` allowlist.

`labels` are added to every metric the target produces, without overwriting a
label the collector already extracted. `otlp.service_name` and
`otlp.resource_attributes` set the OTLP resource the target's metrics arrive
under; both fall back to the exporter-wide `otlp` settings, and per-target
attributes are merged over the exporter-wide ones. Targets with different
identities are exported as separate `resourceMetrics` entries rather than
being conflated.

Targets are scraped once per `otlp.interval`, through the same fetch, decode and
transform path as `/probe`, so collector limits, error handling and the response
cache all apply — a scheduled scrape and an identical `/probe` request share
cache entries. Scheduled targets are never exposed on `/metrics` and are not
reachable through `/probe`.

Every scheduled scrape also exports `http_exporter_target_up` and
`http_exporter_target_scrape_duration_seconds` under that target's resource and
labels, so a failing target is visible in the OTLP backend instead of simply
being absent. Their scrapes are counted in the existing per-collector
self-metrics rather than per-target series, and `http_exporter_scheduled_targets`
reports how many targets loaded.

The file is only accepted when OTLP export is enabled. Starting the exporter
with a targets file while `otlp.enabled` is `false`, or without an
`otlp.endpoint`, logs `invalid scheduled target configuration; exiting` and
terminates with a non-zero exit code. The file is reloaded on the same terms as
the exporter configuration: an invalid document, or a configuration change that
would disable OTLP while targets are loaded, is rejected and the last valid pair
stays active.

## Configuration

Collectors contain request, response, decoder, transformation, error-policy, and limit settings. Target URLs are deliberately not stored in configuration.

`response.format` is optional and defaults to `auto`. When omitted, the
transform selects a deterministic decoder where possible: `regex` uses text,
`csv` uses CSV, `css` uses HTML, and `prometheus` uses Prometheus exposition.
JSON/YAML transforms use content detection. If the decoded response cannot be
used by the selected transform, the probe fails with a clear mapping error.
An explicit format remains useful for ambiguous or mislabeled endpoints.

Supported response formats are `json`, `yaml`, `xml`, `csv`, `html`,
`prometheus`, `text`, and `auto`. Supported transforms include jq/yq, XPath,
CSS, CSV, regex, Prometheus filtering, and Python. JSON and YAML expressions
use the embedded jq-compatible engine (the expression language is also used
for yq-compatible transformations). XML supports XPath, HTML supports CSS
selectors and XPath (including bare element selectors such as `h1`), text
supports regular expressions, and Prometheus input is parsed before
filtering/renaming.

All non-Python transforms use the same collector-level metric declaration. Each
entry has `name`, `description`, `type`, `labels`, and a transform-specific
`expression`. The only allowed metric types are `gauge`, `counter`,
`histogram`, `summary`, and `untyped`:

```yaml
collectors:
  - name: app_json
    transform:
      type: jq
      pre_script: |
        data["requests"] = data.get("requests", 0)
    metrics:
      - name: application_requests_total
        description: Total application requests
        type: counter
        error_mode: log
        expression: .requests
        labels:
          - name: environment
            type: expression
            expression: .environment
```

The expression and label values are interpreted by the selected transform:

- `jq`/`yq`: jq expressions evaluated against decoded data.
- `regex`: a RE2 expression; the first capture group is the numeric value and
  labels map to capture-group numbers or names.
- `csv`: the expression is the numeric column name and labels map to column
  names.
- `css`: the expression selects HTML nodes whose text is numeric; labels are
  selectors relative to each selected node.
- `xpath`: the expression selects XML/HTML nodes whose text is numeric; labels
  are relative XPath expressions or `@attribute` selectors.
- `prometheus`: the expression matches source metric names; it can remap the
  name, description, type, and selected labels.

Metric labels are explicit typed entries. Use `type: expression` when the
value comes from the response, or `type: string` with `value` for a literal:

```yaml
labels:
  - name: server
    type: expression
    expression: server       # CSV column for the current row
  - name: environment
    type: string
    value: production
```

Label expressions use the same transform-specific language as the metric
expression. For CSV, each row produces a metric and `expression: server`
selects that row's `server` column.

Each metric may set `error_mode: log` or `error_mode: ignore`. `log` records a
metric-specific extraction error and skips that metric; `ignore` skips it
silently. The default is `log`.

Every transform may define `transform.pre_script`. It runs once per scrape
after decoding and before metric extraction. The script receives the decoded
value as `data` and may mutate it or replace it by assigning to `data`.
HTML/XML pre-scripts receive raw document text, which is parsed again after the
script. Python transforms emit metrics with the `metric(...)` API.

### Reshaping a response instead of writing a Python transform

When a response only needs parsing, prefer a pre-script that returns a mapping
or a sequence over `transform.type: python`. A structured pre-script result
becomes the decoded response for the `jq`, `yq`, and `none` transforms whatever
the endpoint actually returned, so the metrics are declared exactly like any
other collector's:

```yaml
transform:
  type: jq
  pre_script: |
    import re
    data = {"workers": [{"name": m.group(1), "cpu": int(m.group(2))}
                        for m in re.finditer(r"Worker (\S+) CPU: (\d+)%", response.text)]}
metrics:
  - name: vendor_worker_cpu
    description: Worker CPU utilization
    type: gauge
    error_mode: log
    expression: .workers[].cpu
    labels:
      - name: worker
        type: expression
        expression: .workers[].name
```

Python parses, the metric declaration stays uniform, and `error_mode`,
`required`, `description`, and `type` behave as they do everywhere else — none
of which apply to metrics emitted from a `python` transform. Reserve
`transform.type: python` for collectors whose metric *names* are not known until
the response is read; those still emit through `metric(...)` and may omit the
`metrics` array entirely.

The promotion is deliberately limited to the transforms that read structured
data. `csv`, `regex`, `css`, `xpath`, and `prometheus` keep receiving their own
decoded format, and a pre-script that returns a string still leaves the format
alone, so HTML and XML output is reparsed as before.

Errors are classified as HTTP, decode, transform, missing data, validation, or resource-limit failures. `error_handling` accepts `fail`, `warn`, and `ignore`; `allow_missing_keys` controls required extraction results. Limits default to conservative values and are enforced immediately before exposition.

CSV responses can use a native CSV transform without CSS or Python:

```yaml
metrics:
  - name: server_cpu
    description: Server CPU utilization
    type: gauge
    error_mode: log
    expression: cpu
    labels:
      - name: server
        type: expression
        expression: server
transform:
  type: csv
```

The entire `response` block may be omitted. The exporter infers CSV for the
`csv` transform, and header-based CSV parsing is enabled by default. Use
`response.csv` only when changing CSV behavior, such as selecting a custom
delimiter or disabling the header row. Likewise, `response.format: text` is
unnecessary for a regex or Python transform unless an explicit decoder is
needed for an ambiguous endpoint.

`error_mode` applies after decoding, when an individual metric is extracted.
Decode failures and response/transform incompatibilities are collector-level
errors controlled by `error_handling`.

### Caching

A collector may set `cache` to a Go duration such as `60s`, `1m`, or `3h`:

```yaml
collectors:
  - name: expensive_api
    cache: 60s
    limits:
      max_cache_entries: 1000
```

While a cached result is younger than that interval, a repeat of the same probe
is answered from memory and the target is not contacted again. Caching is off by
default; omitting `cache` or setting `0s` disables it, and a negative value is
rejected at startup.

The cache is in-memory and local to the exporter process. Nothing is written to
disk, replicas do not share entries, and a restart empties it.

A stored result is only ever returned to an identical request. The cache key
covers the collector name and its full effective configuration, the `target`,
every `/probe` query parameter (`method`, `path`, `timeout`, `body`,
`insecure_skip_verify`, `retry_attempts`, `retry_backoff`, and any
`header_<name>` entry), and every header forwarded to the target, including a
forwarded `Authorization` value. Presence and absence differ: a probe that sends
no credential, no forwarded header, or no TLS override cannot read an entry
stored by a probe that sent one, and two probes with different credentials never
share an entry. Because the collector definition is part of the key, a
configuration reload retires the entries cached under the previous definition.
Credentials loaded from files are covered through their configured paths, so a
rotated credential file applies to a cached request once the entry expires; keep
`cache` shorter than the rotation interval where that matters.

Only fully successful probes are cached; HTTP, decode, transform, and validation
failures are not. `limits.max_cache_entries` bounds each collector's live
entries and defaults to 1000, dropping expired entries first and then the ones
closest to expiry.

Cache activity is visible per collector in the self-metrics as
`http_exporter_cache_hits_total`, `http_exporter_cache_misses_total`, and
`http_exporter_cache_entries`. A cache hit counts as a successful scrape and
cached metrics are still queued for OTLP export, while
`http_exporter_scrape_http_status_code` and
`http_exporter_scrape_response_bytes` continue to describe the last real target
request.

CSS remains available specifically for HTML tables and HTML status pages; it is not used for CSV.

## Python

Python is a transform, not a decoder. Configure it under the same
`transform` block as every other collector; the selected response decoder
first parses the response when applicable, then the Python script receives
`response.status_code`, `response.headers`, `response.body`, `response.text`,
`target`, `collector`, and decoded `data`. Scripts emit metrics with
`metric(...)` and may call `fail(...)`.

For example:

```yaml
transform:
  type: python
  script: |
    import re
    for line in response.text.splitlines():
        match = re.match(r"Worker (\S+) CPU: (\d+)%", line)
        if match:
            metric(name="vendor_worker_cpu", type="gauge",
                   value=float(match.group(2)),
                   labels={"worker": match.group(1)})
metrics: []
```

`metrics: []` is explicit for Python because the script creates the metric
definitions dynamically through `metric(...)`.

The launcher blocks `socket`, `subprocess`, `ctypes`, `multiprocessing`, `threading`, shell execution, and package installation. Python has no supported network API; `requests` and `httpx` are unnecessary. `script_timeout` and metric/output limits apply. Declared `libraries` are validated against the supported names (`beautifulsoup4`, `lxml`, `PyYAML`, and `python-dateutil`); they are never installed during a scrape.

## Prometheus Operator

The chart's generated ServiceMonitor and PodMonitor show the required relabeling:

```yaml
params:
  collector: [legacy_text]
relabelings:
  - sourceLabels: [__address__]
    targetLabel: __param_target
  - sourceLabels: [__param_target]
    targetLabel: instance
  - targetLabel: __address__
    replacement: generic-http-exporter:8080
```

One ServiceMonitor endpoint selects one collector. Use multiple endpoints or monitor resources for multiple collector configurations. The same pattern works for PodMonitor.

Each Helm `monitors` entry can set `interval` and `scrapeTimeout` for the Prometheus scrape. Its `params` map can override the selected collector's request method, path, timeout, or raw body for that scrape:

```yaml
monitors:
  - name: write-status
    enabled: true
    type: service
    collector: legacy_text
    interval: 30s
    scrapeTimeout: 10s
    params:
      method: [POST]
      path: [/api/status]
      timeout: [5s]
      body: [raw request body]
      retry_attempts: ["2"]
      retry_backoff: ["2s"]
```

The body is opaque text and does not need to be JSON. Without a `timeout` parameter, the exporter uses the incoming Prometheus scrape context as the target request timeout.

Target retries can be configured in the collector and overridden for one
scrape:

```yaml
request:
  retry:
    attempts: 2   # retries after the initial request
    backoff: 2s   # fixed delay between attempts
```

The exporter retries transport failures and transient HTTP responses (`408`,
`425`, `429`, and `5xx`). Other HTTP statuses are returned immediately. The
retry count and fixed delay can be overridden with the `retry_attempts` and
`retry_backoff` probe parameters shown above. Retries share the scrape/target
timeout, so the retry loop cannot extend the configured deadline indefinitely.

The collector can configure target TLS verification and trust material:

```yaml
request:
  tls:
    ca_file: /etc/prometheus/tls/ca.crt
    cert_file: /etc/prometheus/tls/client.crt
    key_file: /etc/prometheus/tls/client.key
    insecure_skip_verify: false
```

For a one-off scrape, the `insecure_skip_verify` probe parameter overrides the
collector setting. Set it to `true` only for endpoints where certificate
verification is intentionally unavailable; it disables server certificate
verification and should not be used as a general workaround.

```yaml
params:
  insecure_skip_verify: ["true"]
```

Monitor authentication is applied by Prometheus when it scrapes the exporter. To pass that credential to the discovered target, set `request.forward_authorization: true` on the selected collector. Each `monitors` entry supports Secret-backed `auth.type: bearer` and `auth.type: basic` settings. The exporter never forwards arbitrary incoming headers.

For non-secret target headers, configure an allowlist in the collector and use the chart's monitor `headers` map. The chart encodes these as `header_<Header-Name>` probe parameters, which the exporter forwards only when the header is listed in `request.forward_headers`:

```yaml
# exporter config
collectors:
  - name: tenant_status
    request:
      path: /status
      forward_authorization: true
      forward_headers: [X-Tenant]
```

```yaml
# Helm values
monitors:
  - name: application-services
    enabled: true
    type: pod
    headers:
      X-Tenant: team-a
    auth:
      enabled: true
      type: bearer
      secretName: target-api-token
      secretKey: token
```

Header values in monitor parameters are not suitable for secrets. Monitor authentication is disabled by default; set an entry's `auth.enabled: true` and use its `auth` block with a Kubernetes Secret for bearer/basic authentication. Explicitly opt in per collector before forwarding the incoming Authorization header.

The exporter endpoints can also be protected with exporter-side Basic Auth:

```yaml
web:
  basic_auth:
    enabled: true
    username: exporter
    password: change-me
```

When enabled, Basic Auth is required for `/probe`, `/metrics`, and the configured self-metrics endpoint. `/health` and `/ready` remain unauthenticated for Kubernetes probes. Exporter-side Basic Auth is mutually exclusive with `request.forward_authorization`; enable one model or the other so the incoming Authorization header cannot be confused with the exporter credential.

This conflict is rejected during startup: the exporter logs `invalid startup configuration; exiting` and terminates with a non-zero exit code. Invalid configurations detected during file reload are rejected while the last valid configuration remains active.

To use exporter Basic Auth and a Kubernetes Secret for target credentials at the same time, disable the bridge and configure a mounted credential file. `targetAuth.enabled` is `false` by default; for basic auth:

```yaml
web:
  basic_auth:
    enabled: true
    username: exporter
    password: change-me

collectors:
  - name: protected_status
    request:
      path: /status
      basic_auth_file:
        username: /var/run/prometheus-universal-exporter/target-auth/username
        password: /var/run/prometheus-universal-exporter/target-auth/password
```

For the Helm chart, set `targetAuth.enabled: true`, `targetAuth.type: basic`, `targetAuth.secretName`, `usernameKey`, and `passwordKey`. The mounted Secret is read by the exporter and sent as HTTP Basic Auth to the underlying endpoint. For bearer auth, use `type: bearer`, `secretKey`, `fileName`, and `request.bearer_token_file`. The selected monitor can independently use its `auth.type: basic` to authenticate its scrape of the exporter; `request.forward_authorization` must remain `false`.

## Helm

Prometheus Operator CRDs are not installed by this chart.

```sh
helm install exporter charts/prometheus-universal-exporter \
  --set-file 'config.data.config\.yaml=config.example.yaml'
```

The exporter's own flags are chart values. `server.listenAddress` sets
`--web.listen-address` and the container port together, and `server.pythonPath`
sets `--python.path`, the interpreter used by the `python` transform. Its
default, `/usr/local/bin/python3`, is where the exporter image's `python:3.12-slim`
base installs Python; override it for a custom image.

```sh
helm install exporter charts/prometheus-universal-exporter \
  --set server.listenAddress=0.0.0.0:9115 \
  --set server.pythonPath=/usr/bin/python3.11
```

The ConfigMap is mounted at `/etc/prometheus-universal-exporter/config.yaml`; its checksum is part of the Deployment pod template, so configuration changes roll the Deployment. See the chart README for ServiceMonitor, PodMonitor, security, and network-policy values.

## Development

```sh
make fmt        # rewrite sources with gofmt
make fmt-check  # fail if any source needs gofmt
make lint       # golangci-lint, same configuration as CI
make test
make vet
make build
make helm-test
make ci         # everything above, in CI order
```

Static analysis is configured in `.golangci.yml`, so a local `make lint` and the
CI run check exactly the same rules. Install the pinned version with `make
lint-install`. Beyond the standard linters it enables `bodyclose`, `errorlint`,
`gocritic`, `gosec`, `misspell`, `nilerr`, `noctx`, `perfsprint`, `revive`,
`unconvert` and `usestdlibvars`. The repository is gofmt-clean and CI fails on
unformatted sources rather than rewriting them.

`make test` also validates the GitHub Actions workflows: `workflows_test.go`
decodes every file under `.github/workflows` with a parser that rejects
duplicate mapping keys, and checks that each step sets exactly one of `run` or
`uses` and uses no unknown keys. GitHub refuses to create a run for a workflow
it cannot parse, which produces no jobs at all, so a CI step cannot catch that
mistake in the commit that introduces it — the checker would be in the file
GitHub is refusing to read. Running the tests before pushing is what protects
you.

A few `gosec` findings are deliberate and are suppressed narrowly, with the
reason stated at the suppression: `request.tls.insecure_skip_verify` is a
documented opt-in, and the exporter necessarily reads the configuration, target
document and credential files whose paths the operator supplies.

GitHub Actions uses changed-path detection: Go tests/build/vet/race checks run
for Go source or module changes, while Helm lint/template checks run for
changes under `charts/`. Documentation-only changes do not run either suite.

The test suite is intentionally local-only; no third-party endpoint is required. The exporter exposes `/health`, `/ready`, `/metrics`, and `/probe`.
