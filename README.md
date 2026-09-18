# prometheus-universal-exporter

`prometheus-universal-exporter` is a Go HTTP-to-Prometheus adapter. Prometheus Operator discovers the real target; the exporter receives it as `target` and selects server-side collector configuration with `collector`:

```text
/probe?target=http%3A%2F%2Flegacy.example%3A8080&collector=legacy_text
```

The repository includes a self-contained Go service, a Helm chart, Operator examples, and a configuration example. Start locally with:

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

## Configuration

Collectors contain request, response, decoder, transformation, error-policy, and limit settings. Target URLs are deliberately not stored in configuration.

Supported formats are `json`, `yaml`, `xml`, `csv`, `html`, `prometheus`, `text`, `python`, and `auto`. JSON and YAML expressions use the embedded jq-compatible engine (the expression language is also used for yq-compatible transformations). XML supports XPath, HTML supports CSS selectors and XPath (including bare element selectors such as `h1`), text supports regular expressions, and Prometheus input is parsed before filtering/renaming.

All non-Python transforms use the same collector-level metric declaration. Each
entry has `name`, `description`, `type`, `labels`, and a transform-specific
`expression`. The only allowed metric types are `gauge`, `counter`,
`histogram`, `summary`, and `untyped`:

```yaml
collectors:
  - name: app_json
    response:
      format: json
    transform:
      type: jq
      pre_script: |
        data["requests"] = data.get("requests", 0)
    metrics:
      - name: application_requests_total
        description: Total application requests
        type: counter
        expression: .requests
        labels:
          environment: .environment
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

Every transform may define `transform.pre_script`. It runs once per scrape
after decoding and before metric extraction. The script receives the decoded
value as `data` and may mutate it or replace it by assigning to `data`.
HTML/XML pre-scripts receive raw document text, which is parsed again after the
script. Python transforms and decoders continue to emit metrics with the
`metric(...)` API.

Errors are classified as HTTP, decode, transform, missing data, validation, or resource-limit failures. `error_handling` accepts `fail`, `warn`, and `ignore`; `allow_missing_keys` controls required extraction results. Limits default to conservative values and are enforced immediately before exposition.

CSV responses can use a native CSV transform without CSS or Python:

```yaml
response:
  format: csv
metrics:
  - name: server_cpu
    description: Server CPU utilization
    type: gauge
    expression: cpu
    labels:
      server: server
transform:
  type: csv
```

CSS remains available specifically for HTML tables and HTML status pages; it is not used for CSV.

## Python

Python is a peer decoder, not a fallback. The Go process performs the HTTP request and passes `response.status_code`, `response.headers`, `response.body`, `response.text`, `target`, `collector`, and decoded `data` to the script. Scripts emit metrics with `metric(...)` and may call `fail(...)`.

The launcher blocks `socket`, `subprocess`, `ctypes`, `multiprocessing`, `threading`, shell execution, and package installation. Python has no supported network API; `requests` and `httpx` are unnecessary. `script_timeout` and metric/output limits apply. Declared `libraries` are validated against the supported names (`beautifulsoup4`, `lxml`, `PyYAML`, and `python-dateutil`); they are never installed during a scrape.

## Prometheus Operator

The chart and `examples/operator.yaml` show the required relabeling:

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
```

The body is opaque text and does not need to be JSON. Without a `timeout` parameter, the exporter uses the incoming Prometheus scrape context as the target request timeout.

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
  --set-file config.data.config.yaml=config.example.yaml
```

The ConfigMap is mounted at `/etc/prometheus-universal-exporter/config.yaml`; its checksum is part of the Deployment pod template, so configuration changes roll the Deployment. See the chart README for ServiceMonitor, PodMonitor, security, and network-policy values.

## Development

```sh
make test
make vet
make build
make helm-test
```

The test suite is intentionally local-only; no third-party endpoint is required. The exporter exposes `/health`, `/ready`, `/metrics`, and `/probe`.
