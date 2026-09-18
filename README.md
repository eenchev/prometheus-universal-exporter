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

The exporter and the Helm chart are released independently, each from its own
tag and workflow. A chart fix does not require an exporter release, and an
exporter release does not republish the chart.

### Exporter

An `exporter/prometheus-universal-exporter-vMAJOR.MINOR.PATCH` tag runs
`release.yml`, which publishes the container image to GHCR, builds the
cross-platform archives, and creates a GitHub Release containing them:

```sh
git tag -a exporter/prometheus-universal-exporter-v1.0.0 -m "Exporter v1.0.0"
git push origin exporter/prometheus-universal-exporter-v1.0.0
```

The image is published as `ghcr.io/eenchev/prometheus-universal-exporter:1.0.0`,
also tagged `1.0` and `latest`.

### Helm chart

Chart tags are namespaced under `chart/`. Bump `version` in
`charts/prometheus-universal-exporter/Chart.yaml` first — it is the source of
truth, and `release-chart.yml` refuses to publish a tag that disagrees with it:

```sh
# after setting version: 0.2.0 in Chart.yaml
git tag -a chart/prometheus-universal-exporter-0.2.0 -m "Chart 0.2.0"
git push origin chart/prometheus-universal-exporter-0.2.0
```

The workflow re-runs the chart lint and template checks, packages the chart
exactly as `Chart.yaml` declares it, pushes it to
`oci://ghcr.io/eenchev/charts/prometheus-universal-exporter`, and creates a
GitHub Release with the archive.

`appVersion` in `Chart.yaml` records the exporter release a chart version was
validated against and is maintained by hand. It does not drive deployments:
`image.tag` defaults to `latest`, so pin it in your values if you want a
deployment tied to a specific exporter version.

Both workflows trigger on every tag in their namespace, not only well-formed
ones, and fail fast on a tag that does not match the required format — a tag
like `exporter/prometheus-universal-exporter-1.0.0` (no `v`) or
`chart/prometheus-universal-exporter-0.2` reports the expected format instead of
matching no workflow and looking like it released.

The GHCR packages may need to be made public once in the repository's package
settings.

### Verbose per-request self-metrics

By default the exporter's own metrics are per collector. Setting
`web.self_metrics.verbose` republishes every one of them broken down by the
request that produced it, labelled with `collector`, `http_method` and `url`:

```yaml
web:
  self_metrics:
    verbose: true
```

```text
http_exporter_scrapes_total{collector="app_json"} 2
http_exporter_scrapes_total{collector="app_json",http_method="GET",url="http://api.example:8080/api/status"} 1
http_exporter_scrape_http_status_code{collector="app_json",http_method="GET",url="http://api.example:8080/api/status"} 200
http_exporter_scrape_duration_seconds{collector="app_json",http_method="GET",url="http://api.example:8080/api/status"} 0.0142
http_exporter_request_last_scrape_timestamp_seconds{collector="app_json",http_method="GET",url="http://api.example:8080/api/status"} 1.7896896e+09
```

The per-collector series stay exactly as they were, so dashboards built on them
keep working; the labelled series are published alongside. Both shapes share a
metric name, so constrain the label when you query one of them:

```promql
http_exporter_scrapes_total{http_method=""}   # per collector
http_exporter_scrapes_total{http_method!=""}  # per request
```

Everything in the self-metric set is republished except
`http_exporter_cache_entries`, which counts what a collector's response cache
holds and belongs to no single request. `http_exporter_request_last_scrape_timestamp_seconds`
is the one series that exists only in verbose mode: the per-collector view has
no timestamp of its own. Status codes carry `0` when the request failed before a
response arrived, so a transport failure is distinguishable from an HTTP error.

The two views are raised through the same path, so a collector's total is always
the sum of its requests'; a test checks that.

Scheduled targets are recorded the same way, and because their requests are
fully described by the target file they are listed from startup with zero
counters and a zero timestamp, before their first collection. A `/probe` request
cannot be listed in advance — its URL comes from the probe's own `target`
parameter — so it appears the first time that probe is served. Until then the
exporter reports an empty set rather than nothing at all:

```text
http_exporter_request_series_tracked 0
http_exporter_request_series_capped 0
```

A response served from the collector response cache is not a scrape: no request
reaches the target, so the last-scrape timestamp and status keep describing the
scrape that filled the cache. The cache hit itself is counted, on the collector
and on the request alike.

The `url` label carries only the scheme, host and path. Userinfo credentials and
the whole query string are dropped, because `request.query` or a probe parameter
can carry a token or tenant identifier and a metric label is persisted by
Prometheus and handed to anything federating from it. The label is built from
the same resolution the real request uses, so it can never describe a different
URL than the one fetched.

A request URL is an unbounded label value and each combination now carries a
whole metric family, so tracking is capped at 1000 collector/URL/method
combinations. Requests already tracked keep updating past the limit; only new
combinations are refused. The truncation is visible rather than silent:

```text
http_exporter_request_series_capped 1
```

It reads `0` normally, so you can alert on `== 1` without testing for an absent
series, and `http_exporter_request_series_tracked` reports how many combinations
are in use against that limit. Because verbosity is configuration rather than a
flag, a reload turns it on and off; turning it off drops the labelled series
instead of leaving stale ones exposed.

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
`insecure_skip_verify`, `follow_redirects`, `enable_http2` and the `retry`
settings — overriding the collector's own request for that target only. It also takes static `headers` and its own target
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

A pre-script **must** leave its result in `data` — that is the variable the
exporter reads back. A script that computes a value under another name throws it
away: the transform then runs against the untouched response, so the collector
looks like it is extracting badly rather than configured wrongly. The exporter
therefore refuses to start when a pre-script never produces `data`, naming the
collector, and rejects such a configuration on reload with the previous one left
active. Replacing `data`, mutating it by key or attribute, augmenting it,
binding it as a loop or `with` target, and calling a method that mutates it all
count.

Reading `data` does not count, however much of it the script does. A method call
only counts when the method mutates: `data.update(...)` and
`data["rates"].append(...)` produce `data`, while `data.items()`, `data.get(...)`
and `data.copy()` are reads. This matters because the mistake usually looks
busy — a script that walks `data` thoroughly and assigns the result to a
neighbouring name:

```yaml
transform:
  pre_script: |
    import json
    data = json.loads(response.text)["payload"]   # produces data

    # result = json.loads(...)  would be rejected at startup: nothing reaches
    # the transform, because the exporter only reads back `data`.

    # So would this, despite reading data three times — it never produces it:
    #   reshaped = {
    #       "base": data["base"],
    #       "rates": [r for r in sorted(data["rates"].items())],
    #   }
```

The same startup check compiles every configured script, so a Python syntax
error in a pre-script or a `python` transform script is reported before the
exporter serves traffic rather than at the first scrape. All faults are listed
in one message. A `python` transform emits through `metric(...)` and is not
required to produce `data`.

The check needs the interpreter from `--python.path`, so a configuration that
contains any Python fails to start if that interpreter is unusable. A
configuration with no Python scripts never invokes one.

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
`insecure_skip_verify`, `follow_redirects`, `enable_http2`, `retry_attempts`,
`retry_backoff`, and any `header_<name>` entry), and every header forwarded to
the target, including a forwarded `Authorization` value. Presence and absence differ: a probe that sends
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

### Redirects and HTTP/2

Two transport settings live on the collector request, both off by default:

```yaml
request:
  follow_redirects: false
  enable_http2: false
```

`follow_redirects` decides whether a redirect status on the target request is
followed. Left at `false`, the exporter returns the redirect response itself, so
the collector sees the 3xx status and — with the default `on_http_error: fail` —
the probe fails. That is deliberate: a target that has moved is worth noticing
rather than quietly scraping somewhere else. Set it to `true` for endpoints that
legitimately redirect, such as an API whose documented host forwards to another.

`enable_http2` decides whether the target request may negotiate HTTP/2. HTTP/2
is negotiated through ALPN over TLS, so this only affects HTTPS targets;
cleartext HTTP/2 is never attempted. The default of `false` is the protocol the
exporter has always used.

Both are overridable per scrape:

```yaml
params:
  follow_redirects: ["true"]
  enable_http2: ["true"]
```

Each accepts exactly `true` or `false`; anything else returns HTTP 400 before
the target is contacted, so a typo cannot quietly fall back to a default. An
absent parameter leaves the collector's setting in force, and both parameters
are part of the response cache key, so a scrape asking for different transport
behaviour never reads another scrape's cached result. Scheduled targets accept
both in their own `request` block.

These replace the earlier undocumented `request.redirect_policy`. A
configuration still setting it now fails to load with an unknown-field error
rather than silently changing behaviour.

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

### Logging

Every line the exporter writes is a JSON object, at the level set by
`--log.level` (`debug`, `info`, `warn` or `error`):

```json
{"time":"2026-09-18T21:49:52+03:00","level":"INFO","msg":"starting exporter","address":":8080","collectors":1,"scheduled_targets":0,"config_watch":true,"config_watch_interval":"1m30s"}
{"time":"2026-09-18T21:49:54+03:00","level":"ERROR","msg":"metric extraction failed","collector":"exchange_rates","metric":"exchange_rate_observation_timestamp_seconds","error":"metric \"exchange_rate_observation_timestamp_seconds\" value is missing"}
```

There is no second format. That is worth stating because it is easy to lose: a
metric rule failing under `error_mode: log` reports from inside a transform,
several calls below anything holding a logger, so it goes through Go's default
logger rather than the exporter's. The exporter installs its JSON logger as the
process default at startup so those lines are JSON too, instead of arriving as
`2026/09/18 21:43:35 ERROR metric extraction failed metric=...` in the middle of
a stream your collector is parsing.

A failing rule is reported with its collector as well as its name, because the
same metric name is often declared by several collectors and the rule name alone
would not say which one to go and look at.

`config_watch_interval` appears only when `--config.watch` is on, since that is
what bounds how stale a running configuration can be; with the watch off there
is no interval to report.

### Watching the configuration

The exporter reads its configuration once at startup. Pass `--config.watch` to
have it re-read the configuration file and, when one is configured, the
scheduled target file whenever either changes on disk:

```sh
prometheus-universal-exporter \
  --config.file=config.yaml \
  --config.watch \
  --config.watch-interval=60s
```

`--config.watch-interval` defaults to 60s and must be positive; passing zero or a
negative duration alongside `--config.watch` is a startup error rather than a
silently disabled watch. Without `--config.watch` no polling loop runs at all,
and configuration changes take effect on restart.

Changes are detected by modification time rather than filesystem events. That is
deliberate: Kubernetes republishes a mounted ConfigMap by atomically swapping
the `..data` symlink, which replaces the inode a file-level event watch is
attached to, so such a watch would stop firing after the first change.

The watch does not relax any reload rule. An invalid configuration, one that
would disable OTLP while scheduled targets are loaded, and a pre-script that
stops producing `data` are all still rejected, with the last valid configuration
left active and the reason logged.

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
make fmt        # rewrite the whole tree with gofmt, tools/ included
make fmt-check  # fail if any source needs gofmt
make lint       # golangci-lint, same configuration as CI
make test       # go test ./... followed by go test -race ./...
make vet
make build
make helm-test  # helm lint and the template scenarios CI renders
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

It validates the chart templates the same way. `charts_test.go` checks that
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
document and credential files whose paths the operator supplies.

### The Go toolchain

CI and the release workflows ask setup-go for `stable`, so the build always uses
the current stable Go release and no workflow needs editing when Go ships a new
one. The `go` directive in `go.mod` is something different: it is the *minimum*
the module requires, raised by dependency updates rather than by whichever
toolchain builds it, so it stays where the dependencies put it. The Dockerfile
pins `GO_VERSION` to a released minor, which `tools/depupdate` keeps moving
within the major.

The two can drift apart in a way that is hard to read: a dependency bump raises
the go directive in a pull request that touches no workflow, and from then on
every build fails with `go.mod requires go >= X` with nothing nearby to explain
it. `goversion_test.go` ties them together — it checks that every Go version the
workflows request, and the one the Dockerfile pins, satisfies the go directive —
so `go test ./...` catches the mismatch instead of the next red build.

GitHub Actions uses changed-path detection: Go tests/build/vet/race checks run
for Go source or module changes, while Helm lint/template checks run for changes
under `charts/`. A change under `charts/` runs the Go suite too, because the
template guard above lives there and is worth least on exactly the changes that
would break it. Documentation-only changes do not run either suite.

### Dependency updates

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

The test suite is intentionally local-only; no third-party endpoint is required. The exporter exposes `/health`, `/ready`, `/metrics`, and `/probe`.
