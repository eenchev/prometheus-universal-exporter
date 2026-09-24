<img src="docs/logo.svg" alt="prometheus-universal-exporter" width="104">

# prometheus-universal-exporter

[![CI](https://img.shields.io/github/actions/workflow/status/eenchev/prometheus-universal-exporter/ci.yml?branch=main&label=CI&logo=github)](https://github.com/eenchev/prometheus-universal-exporter/actions/workflows/ci.yml)
[![Artifact Hub](https://img.shields.io/endpoint?url=https://artifacthub.io/badge/repository/prometheus-universal-exporter)](https://artifacthub.io/packages/search?repo=prometheus-universal-exporter)
[![Exporter release](https://img.shields.io/github/v/release/eenchev/prometheus-universal-exporter?filter=exporter%2F*&label=exporter&color=fe7d37)](https://github.com/eenchev/prometheus-universal-exporter/releases)
[![Chart release](https://img.shields.io/github/v/release/eenchev/prometheus-universal-exporter?filter=chart%2F*&label=chart&color=fe7d37&logo=helm&logoColor=white)](https://github.com/eenchev/prometheus-universal-exporter/releases)
[![Go](https://img.shields.io/github/go-mod/go-version/eenchev/prometheus-universal-exporter?logo=go&logoColor=white)](go.mod)
[![License](https://img.shields.io/github/license/eenchev/prometheus-universal-exporter?color=blue)](LICENSE)

Turn an HTTP endpoint that was never meant for Prometheus into a Prometheus
target, without writing an exporter for it.

Point the exporter at a service that answers with JSON, YAML, XML, CSV, HTML,
plain text, or Prometheus exposition — or at a file on disk, such as the
`.prom` files a batch job leaves for node_exporter. A collector in the configuration says how
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

For a fixed list of targets there is a second way in: list them as
[static targets](docs/STATIC-TARGETS.md), which the exporter scrapes itself on
their own intervals and serves together on one endpoint, `/static-targets`, for
Prometheus to scrape like any other exporter, and, one by one, over OTLP.

## Quick start

Describe a collector:

```yaml
collectors:
  - name: app_json
    metrics_prefix: myapp   # optional; exports myapp_application_requests_total
    request:
      type: http
      path: /status
    transform:
      type: jq
    metrics:
      - name: application_requests_total
        description: Total application requests
        type: counter
        expression: .requests
```

`metrics_prefix` is optional: when set, it is joined with `_` to the front of
every metric the collector exports. See
[Prefixing a collector's metrics](docs/CONFIGURATION.md#prefixing-a-collectors-metrics).
For a list of things — servers, rows, components — give a metric `items` and
write its value and labels against one item at a time; see
[Metrics per item](docs/CONFIGURATION.md#metrics-per-item).
Collectors can also live in files of their own, listed under `collector_files`
— one per team or per ConfigMap key; a collector name must be unique across all
of them. See [Collector files](docs/CONFIGURATION.md#collector-files).

Your editor can check the file as you type: start it with

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/eenchev/prometheus-universal-exporter/main/configs/config.schema.json
```

and the YAML language server completes keys and flags mistakes against
[`configs/config.schema.json`](configs/config.schema.json). Every expression is also compiled
when the exporter starts, so a typo stops it with a message naming the
collector and metric, rather than failing each scrape.

Run the exporter and probe a target through it:

```sh
go run . --config.file=configs/config.example.yaml

curl 'http://localhost:8080/probe?target=http://127.0.0.1:9000&collector=app_json'
```

The response is ordinary Prometheus exposition, which is what Prometheus
scrapes. `configs/config.example.yaml` is a complete working
document covering every decoder.

Or run the published image:

```sh
docker run -p 8080:8080 \
  -v "$PWD/config.yaml:/etc/prometheus-universal-exporter/config.yaml:ro" \
  ghcr.io/eenchev/prometheus-universal-exporter:latest
```

## Install with Helm

The chart is published to GitHub Container Registry as an OCI artifact. It is public, so no registry login is needed. Prometheus Operator CRDs are not installed by this chart.

```sh
helm install exporter \
  oci://ghcr.io/eenchev/charts/prometheus-universal-exporter \
  --version 0.2.1 \
  --set-file 'config.data.config\.yaml=configs/config.example.yaml'
```

Omitting `--version` takes the newest published chart; pin it for anything you deploy more than once. To install from a checkout instead:

```sh
helm install exporter charts/prometheus-universal-exporter \
  --set-file 'config.data.config\.yaml=configs/config.example.yaml'
```

The chart creates the Deployment, Service, ConfigMap and, on request,
`ServiceMonitor` or `PodMonitor` resources with the relabeling the probe
pattern needs. See the
[chart README](charts/prometheus-universal-exporter/README.md) for its values.

## Endpoints

| Path | Purpose |
| --- | --- |
| `/` | A landing page: the build and links to the endpoints. |
| `/collectors` | Each collector with a form that probes a target through it, taking its request parameters, forwarded headers and, when it forwards `Authorization`, a target credential. See [Probing from the browser](docs/AUTHENTICATION.md#probing-from-the-browser). |
| `/probe` | Scrape a target through a collector. Takes `target` and `collector`, with `GET` or `HEAD`; any other method is answered `405`. |
| `/self-metrics` | The exporter's own metrics, at `--web.self-metrics-path`. See [Self-metrics](docs/SELF-METRICS.md). |
| `/static-targets` | The latest results of the [static targets](docs/STATIC-TARGETS.md), at `--web.static-targets-path`. |
| `/-/reload` | `POST` reloads the configuration, with `--web.enable-lifecycle`. |
| `/health`, `/ready` | Kubernetes probes. `/ready` is `503` while a reload is rejected, OTLP exports keep failing or the exporter is shutting down; see [Readiness](docs/CONFIGURATION.md#readiness). Never authenticated. |

`/probe`, the self-metrics path and the static targets path answer gzip-compressed when the client accepts it, as Prometheus does on every scrape.

## Command-line flags

| Flag | Default | Purpose |
| --- | --- | --- |
| `--config.file` | `/etc/prometheus-universal-exporter/config.yaml` | The configuration document. |
| `--web.listen-address` | `:8080` | Address the HTTP endpoints listen on. |
| `--web.self-metrics-path` | `/self-metrics` | Path of the exporter's own metrics, served there and nowhere else; `/metrics` for the conventional path. A path another endpoint uses is refused. |
| `--web.static-targets-path` | `/static-targets` | Path the static targets' latest results are served at, for Prometheus to scrape. A path another endpoint uses is refused. |
| `--python.path` | `python3` | Interpreter used by the `python` transform. |
| `--log.level` | `info` | `debug`, `info`, `warn` or `error`. |
| `--config.watch` | off | Re-read the configuration when it changes on disk. |
| `--config.watch-interval` | `60s` | How often to check, with `--config.watch`. |
| `--config.export-env` | off | Expand `${NAME}` references in the configuration. |
| `--static-targets-file` | none | Static targets the exporter scrapes itself, on their own intervals. See [Static targets](docs/STATIC-TARGETS.md). |
| `--config.schema` | off | Print the JSON Schema of the configuration file, for editors, and exit. See [Editor support](docs/CONFIGURATION.md#editor-support). |
| `--config.collector-file-schema` | off | Print the JSON Schema of a collector file, for editors, and exit. See [Collector files](docs/CONFIGURATION.md#collector-files). |
| `--static-targets-file-schema` | off | Print the JSON Schema of the static target file, for editors, and exit. See [Static targets](docs/STATIC-TARGETS.md). |
| `--probe.timeout-offset` | `500ms` | How much of Prometheus's scrape timeout a probe leaves unused, so it answers with its own error first. See [Probe deadlines](docs/CONFIGURATION.md#probe-deadlines). |
| `--probe.default-timeout` | `30s` | How long a probe may take when it names no deadline: no `X-Prometheus-Scrape-Timeout-Seconds` header and no `timeout` parameter, as from curl or a script. `0` leaves such a probe unbounded. See [Probe deadlines](docs/CONFIGURATION.md#probe-deadlines). |
| `--web.shutdown-delay` | `0s` | How long a shutdown keeps serving, with `/ready` answering `503`, before it begins, so a load balancer stops sending probes first. The Helm chart sets `5s`. See [Shutting down](docs/CONFIGURATION.md#shutting-down). |
| `--web.shutdown-timeout` | `5s` | How long a shutdown waits for the probes in progress. Keep it at least as long as Prometheus's scrape timeout. See [Shutting down](docs/CONFIGURATION.md#shutting-down). |
| `--web.enable-lifecycle` | off | Enable `POST /-/reload`, which reloads the configuration and reports whether it was accepted. `SIGHUP` reloads either way. See [Reloading on demand](docs/CONFIGURATION.md#reloading-on-demand). |
| `--version` | off | Print the version, git revision, Go version and request types of the build, and exit. The same is in the `http_exporter_build_info` self-metric. |
| `--dry-run` | off | Validate the files and flags above, print a JSON report and exit `0` or `1`, without starting. See [Dry run](docs/CONFIGURATION.md#dry-run). |

## Documentation

- [Configuration](docs/CONFIGURATION.md) — collectors, decoders, transforms, metric rules, caching and serving the last good result while a target is down, environment variables, reloading.
- [Target requests](docs/REQUESTS.md) — redirects, HTTP/2, retries, TLS, proxies, per-scrape overrides.
- [Local files](docs/LOCALFILE.md) — the `localfile` request type: reading metrics and status files from disk, one file or a whole directory.
- [Authentication](docs/AUTHENTICATION.md) — credentials for the target and for the exporter itself.
- [Prometheus Operator](docs/PROMETHEUS-OPERATOR.md) — ServiceMonitor and PodMonitor.
- [Helm chart](charts/prometheus-universal-exporter/README.md) — every chart value.
- [Python](docs/PYTHON.md) — the transform and pre-script API.
- [Self-metrics](docs/SELF-METRICS.md) — what the exporter reports about itself.
- [Static targets](docs/STATIC-TARGETS.md) — targets the exporter scrapes itself on their own intervals and serves on one endpoint.
- [OTLP](docs/OTLP.md) — OTLP export, its retries and compression.
- [Logging](docs/LOGGING.md) — the log format.
- [Exporter specification](docs/SPECIFICATION-EXPORTER.md) — the implementation specification for the exporter.
- [Chart specification](docs/SPECIFICATION-CHART.md) — the implementation specification for the Helm chart.

For contributors: [development](docs/DEVELOPMENT.md),
[dependencies](docs/DEPENDENCIES.md) and [releasing](docs/RELEASING.md).

## License

See [LICENSE](LICENSE).
