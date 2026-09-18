<img src="docs/logo.svg" alt="prometheus-universal-exporter" width="104">

# prometheus-universal-exporter

[![CI](https://img.shields.io/github/actions/workflow/status/eenchev/prometheus-universal-exporter/ci.yml?branch=main&label=CI&logo=github)](https://github.com/eenchev/prometheus-universal-exporter/actions/workflows/ci.yml)
[![Exporter release](https://img.shields.io/github/v/release/eenchev/prometheus-universal-exporter?filter=exporter%2F*&label=exporter&color=fe7d37)](https://github.com/eenchev/prometheus-universal-exporter/releases)
[![Chart release](https://img.shields.io/github/v/release/eenchev/prometheus-universal-exporter?filter=chart%2F*&label=chart&color=fe7d37&logo=helm&logoColor=white)](https://github.com/eenchev/prometheus-universal-exporter/releases)
[![Go](https://img.shields.io/github/go-mod/go-version/eenchev/prometheus-universal-exporter?logo=go&logoColor=white)](go.mod)
[![License](https://img.shields.io/github/license/eenchev/prometheus-universal-exporter?color=blue)](LICENSE)

`prometheus-universal-exporter` is a Go HTTP-to-Prometheus adapter. Prometheus Operator discovers the real target; the exporter receives it as `target` and selects server-side collector configuration with `collector`:

```text
/probe?target=http%3A%2F%2Flegacy.example%3A8080&collector=legacy_text
```

The repository includes a self-contained Go service, a Helm chart, a configuration example, and a scheduled-target example. Start locally with:

```sh
go run . --config.file=config.example.yaml
```

## Documentation

- [Configuration](#configuration) and the sections below cover everyday use.
- [docs/PYTHON.md](docs/PYTHON.md) — Python transforms and pre-scripts.
- [docs/SELF-METRICS.md](docs/SELF-METRICS.md) — the exporter's own metrics.
- [docs/OTLP.md](docs/OTLP.md) — OTLP export and scheduled targets.
- [docs/SPECIFICATION.md](docs/SPECIFICATION.md) — the full implementation specification.

For contributors: [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md),
[docs/DEPENDENCIES.md](docs/DEPENDENCIES.md) and
[docs/RELEASING.md](docs/RELEASING.md).

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

## Caching

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

## Redirects and HTTP/2

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

## Logging

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

## Watching the configuration

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

The Helm chart supports `defaultLabels` and `defaultAnnotations` for metadata
that should be applied to every chart-created Kubernetes object. Object-specific
metadata overrides a same-named default.
