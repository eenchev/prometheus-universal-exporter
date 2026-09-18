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

Exporter self-health metrics are available at `/self-metrics` by default (and `/metrics` remains a compatibility alias). Change the dedicated path with `--web.self-metrics-path=/exporter/metrics`. The Helm chart's optional self-metrics ServiceMonitor/PodMonitor scrapes the exporter pods/services separately from the target-probing monitor.

Optional OTLP/HTTP JSON export is configured at the top level. Probe metric sets and self-health metric sets are forwarded when enabled:

```yaml
otlp:
  enabled: true
  endpoint: http://otel-collector:4318/v1/metrics
  service_name: prometheus-universal-exporter
  timeout: 5s
```

OTLP export is best-effort and does not make a Prometheus probe fail.

## Configuration

Collectors contain request, response, decoder, transformation, error-policy, and limit settings. Target URLs are deliberately not stored in configuration.

Supported formats are `json`, `yaml`, `xml`, `csv`, `html`, `prometheus`, `text`, `python`, and `auto`. JSON and YAML expressions use the embedded jq-compatible engine (the expression language is also used for yq-compatible transformations). XML supports XPath, HTML supports CSS selectors, text supports regular expressions, and Prometheus input is parsed before filtering/renaming.

A simple extraction rule is useful when the desired metric name is known:

```yaml
metrics:
  - name: application_requests_total
    type: counter
    jq: .requests
```

Expression transformations can return metric objects:

```yaml
transform:
  type: jq
  expression: '[{name: "application_requests_total", type: "counter", value: .requests}]'
```

Errors are classified as HTTP, decode, transform, missing data, validation, or resource-limit failures. `error_handling` accepts `fail`, `warn`, and `ignore`; `allow_missing_keys` controls required extraction results. Limits default to conservative values and are enforced immediately before exposition.

CSV responses can use a native CSV transform without CSS or Python:

```yaml
response:
  format: csv
transform:
  type: csv
  expressions:
    - name: server_cpu
      type: gauge
      value: cpu
      labels:
        server: server
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

Monitor authentication is applied by Prometheus when it scrapes the exporter. To pass that credential to the discovered target, set `request.forward_authorization: true` on the selected collector. The chart supports Secret-backed `auth.type: bearer` and `auth.type: basic` settings on both monitor types. The exporter never forwards arbitrary incoming headers.

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
podMonitor:
  enabled: true
  headers:
    X-Tenant: team-a
  auth:
    type: bearer
    secretName: target-api-token
    secretKey: token
```

Header values in monitor parameters are not suitable for secrets. Use monitor `auth` with a Kubernetes Secret for bearer/basic authentication, and explicitly opt in per collector before forwarding the incoming Authorization header.

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

To use exporter Basic Auth and a Kubernetes Secret for the target token at the same time, disable the bridge and configure a mounted token file:

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
      bearer_token_file: /var/run/prometheus-universal-exporter/target-auth/token
```

For the Helm chart, set `targetAuth.enabled: true`, `targetAuth.secretName`, and optionally `targetAuth.secretKey`. The mounted Secret is read by the exporter and sent as `Authorization: Bearer ...` to the underlying endpoint. The PodMonitor can independently use `auth.type: basic` to authenticate its scrape of the exporter; `request.forward_authorization` must remain `false`.

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
