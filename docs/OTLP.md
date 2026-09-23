# OTLP export

Optional OTLP/HTTP JSON export is configured at the top level. Probe metric sets and self-health metric sets are forwarded when enabled:

```yaml
otlp:
  enabled: true
  endpoint: http://otel-collector:4318/v1/metrics
  service_name: prometheus-universal-exporter
  timeout: 5s
  interval: 30s
  # compression: gzip
  # max_pending_points: 100000
  # unready_after_failures: 0
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
which defaults to 5 seconds. The connection to the endpoint is kept and reused
from export to export (see [Connections](REQUESTS.md#connections)), and goes
through the proxy the environment names, if any. Self-health metrics are
included in every export interval even when no Prometheus self-metrics scrape
is running.

## Delivery

Requests are gzipped (`Content-Encoding: gzip`), which every OpenTelemetry
Collector accepts; set `otlp.compression: none` for an endpoint that does not.

An export that fails with a network error, or with `429`, `502`, `503` or
`504` — the answers the OTLP specification makes retryable — is tried again
after 1 second, then 2, 4, and so on up to 16, or after the `Retry-After` the
endpoint asks for. Retries go on while another attempt can still start within
`otlp.interval`, so an export never runs into the next one. When they run out,
the data points are kept and sent with the next export, unless a newer value of
the same series has arrived in the meantime. Only the latest value of each
series is kept, but an outage long enough can still see many series come and
go, so what waits is bounded by `otlp.max_pending_points`, 100000 by default:
past it the oldest data points — those of failed exports first — are dropped,
down to nine tenths of the limit, counted in
`http_exporter_otlp_points_dropped_total` and logged as a warning. Any other
answer, such as `400` or `401`,
would be given again: the data points are dropped and counted rather than sent
again forever. Each failure is logged as a warning, and the export status is in
the [self-metrics](SELF-METRICS.md#otlp-export-status):

```promql
# No export has got through for ten minutes.
time() - http_exporter_otlp_last_export_success_timestamp_seconds > 600
```

Failing exports do not make the exporter unready by default: it still answers
probes, and in Kubernetes a pod that is not ready stops receiving them. For an
exporter whose job is delivering [scheduled targets](#scheduled-targets) over
OTLP, set `otlp.unready_after_failures: 3` to have `/ready` answer `503` after
three failed exports in a row, until one gets through (see
[Readiness](CONFIGURATION.md#readiness)). The count is per endpoint: a reload
that changes `otlp.endpoint` starts it again.

On `SIGTERM` or `SIGINT` the exporter first lets the probes in progress finish,
then makes one last export, bounded by `otlp.timeout`, of what they and earlier
probes queued, with a last self-metric snapshot. Scheduled targets are not
scraped again for it. An export the shutdown interrupted is not lost: its data
goes out with that last export. A second signal ends the process at once,
without it (see [Shutting down](CONFIGURATION.md#shutting-down)).

Metrics keep their type:

| Prometheus | OTLP |
| --- | --- |
| gauge, untyped | gauge |
| counter | sum, monotonic, cumulative |
| histogram | histogram, cumulative: count, sum, bounds and per-bucket counts |
| summary | summary: count, sum and quantiles |

Prometheus counts histogram buckets cumulatively and OTLP counts each bucket on
its own, so the counts are converted; the `+Inf` bucket becomes the count above
the highest bound rather than a bound. Every series of a metric is a data point
of one OTLP metric. A `NaN` or infinite value is sent as `"NaN"`, `"Infinity"`
or `"-Infinity"`, as the OTLP JSON encoding writes them, rather than failing
the export.

## Scheduled targets

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
settings — overriding the collector's own request for that target only. Which of these
keys a target may set follows its collector's
[request type](CONFIGURATION.md#request-types); for `http` it is all of them,
and for [`localfile`](LOCALFILE.md#scheduled-targets-over-otlp) only `path` and
`timeout`, with `target` optional. It also takes static `headers` and its own target
credentials, inline or file-backed, as basic authentication or a bearer token.
Because the file is operator configuration rather than caller input, these
headers are applied directly and are not filtered through the collector's
`request.forward_headers` allowlist.

[Path parameters](REQUESTS.md#path-parameters) are the one probe feature a
scheduled target cannot use: there is no probe to supply `param_<name>`. A
target's own `request.path` must be written out in full, and a target can
borrow a collector whose path has `{{param_…}}` placeholders only when each one
has a default, which is what it will use. Otherwise the exporter refuses to
start, naming the target and the collector.

Check a target file together with its configuration before deploying it:
`prometheus-universal-exporter --dry-run --config.file=config.otlp.yaml --otlp.targets-file=targets.yaml`
reports whether each would load, including whether every target names a
collector that exists and whether OTLP export is enabled — see
[Dry run](CONFIGURATION.md#dry-run).

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
