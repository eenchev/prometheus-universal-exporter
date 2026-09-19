<img src="docs/logo.svg" alt="prometheus-universal-exporter" width="104">

# prometheus-universal-exporter

[![CI](https://img.shields.io/github/actions/workflow/status/eenchev/prometheus-universal-exporter/ci.yml?branch=main&label=CI&logo=github)](https://github.com/eenchev/prometheus-universal-exporter/actions/workflows/ci.yml)
[![Exporter release](https://img.shields.io/github/v/release/eenchev/prometheus-universal-exporter?filter=exporter%2F*&label=exporter&color=fe7d37)](https://github.com/eenchev/prometheus-universal-exporter/releases)
[![Chart release](https://img.shields.io/github/v/release/eenchev/prometheus-universal-exporter?filter=chart%2F*&label=chart&color=fe7d37&logo=helm&logoColor=white)](https://github.com/eenchev/prometheus-universal-exporter/releases)
[![Go](https://img.shields.io/github/go-mod/go-version/eenchev/prometheus-universal-exporter?logo=go&logoColor=white)](go.mod)
[![License](https://img.shields.io/github/license/eenchev/prometheus-universal-exporter?color=blue)](LICENSE)

Turn an HTTP endpoint that was never meant for Prometheus into a Prometheus
target, without writing an exporter for it.

Point the exporter at a service that answers with JSON, YAML, XML, CSV, HTML,
plain text, or Prometheus exposition. A collector in the configuration says how
to call it, how to read the response, and which metrics to publish. The
exporter does the rest — no code, no rebuild, and nothing to redeploy when the
rules change.

## How it works

Targets are never written into the configuration. Prometheus discovers the real
service and passes it to the exporter as `target`; the `collector` parameter
picks which server-side configuration reads it:

```text
/probe?target=http%3A%2F%2Flegacy.example%3A8080&collector=legacy_text
```

One exporter therefore serves many services and many response shapes, and a
collector is reusable across every target that answers the same way.

## Quick start

Describe a collector:

```yaml
collectors:
  - name: app_json
    request:
      path: /status
    transform:
      type: jq
    metrics:
      - name: application_requests_total
        description: Total application requests
        type: counter
        expression: .requests
```

Run the exporter and probe a target through it:

```sh
go run . --config.file=config.example.yaml

curl 'http://localhost:8080/probe?target=http://127.0.0.1:9000&collector=app_json'
```

The response is ordinary Prometheus exposition, which is what Prometheus
scrapes. `config.example.yaml` in the repository root is a complete working
document covering every decoder.

Or run the published image:

```sh
docker run -p 8080:8080 \
  -v "$PWD/config.yaml:/etc/prometheus-universal-exporter/config.yaml:ro" \
  ghcr.io/eenchev/prometheus-universal-exporter:latest
```

## Install with Helm

Prometheus Operator CRDs are not installed by this chart.

```sh
helm install exporter charts/prometheus-universal-exporter \
  --set-file 'config.data.config\.yaml=config.example.yaml'
```

The chart creates the Deployment, Service, ConfigMaps and, on request,
`ServiceMonitor` or `PodMonitor` resources with the relabeling the probe
pattern needs. See the
[chart README](charts/prometheus-universal-exporter/README.md) for its values.

## Endpoints

| Path | Purpose |
| --- | --- |
| `/probe` | Scrape a target through a collector. Takes `target` and `collector`. |
| `/metrics` | The exporter's own metrics. |
| `/self-metrics` | The same self-metrics on a dedicated path, so a monitor can scrape them separately. |
| `/health`, `/ready` | Kubernetes probes. Never authenticated. |

## Command-line flags

| Flag | Default | Purpose |
| --- | --- | --- |
| `--config.file` | `/etc/prometheus-universal-exporter/config.yaml` | The configuration document. |
| `--web.listen-address` | `:8080` | Address the HTTP endpoints listen on. |
| `--web.self-metrics-path` | `/self-metrics` | Path for the dedicated self-metrics endpoint. |
| `--python.path` | `python3` | Interpreter used by the `python` transform. |
| `--log.level` | `info` | `debug`, `info`, `warn` or `error`. |
| `--config.watch` | off | Re-read the configuration when it changes on disk. |
| `--config.watch-interval` | `60s` | How often to check, with `--config.watch`. |
| `--config.export-env` | off | Expand `${NAME}` references in the configuration. |
| `--otlp.targets-file` | none | Scheduled targets the exporter scrapes itself. |

## Documentation

- [Configuration](docs/CONFIGURATION.md) — collectors, decoders, transforms, metric rules, caching, environment variables, reloading.
- [Target requests](docs/REQUESTS.md) — redirects, HTTP/2, retries, TLS, per-scrape overrides.
- [Authentication](docs/AUTHENTICATION.md) — credentials for the target and for the exporter itself.
- [Prometheus Operator](docs/PROMETHEUS-OPERATOR.md) — ServiceMonitor and PodMonitor.
- [Helm chart](charts/prometheus-universal-exporter/README.md) — every chart value.
- [Python](docs/PYTHON.md) — the transform and pre-script API.
- [Self-metrics](docs/SELF-METRICS.md) — what the exporter reports about itself.
- [OTLP](docs/OTLP.md) — OTLP export and scheduled targets.
- [Logging](docs/LOGGING.md) — the log format.
- [Exporter specification](docs/SPECIFICATION-EXPORTER.md) — the implementation specification for the exporter.
- [Chart specification](docs/SPECIFICATION-CHART.md) — the implementation specification for the Helm chart.

For contributors: [development](docs/DEVELOPMENT.md),
[dependencies](docs/DEPENDENCIES.md) and [releasing](docs/RELEASING.md).

## License

See [LICENSE](LICENSE).
