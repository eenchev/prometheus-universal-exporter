# OTLP export

Optional OTLP/HTTP JSON export is configured at the top level. Probe metric sets, self-health metric sets and the [static targets](STATIC-TARGETS.md) that set `export_via_otlp` are forwarded when enabled:

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
  #   server_name: otel-collector.example
  # resource_attributes:
  #   deployment.environment: production
  # probe_attributes: false
```

`enabled` is the switch, and a block that sets anything must say it: `true`
exports, `false` keeps the settings without using or checking them. An `otlp`
block with an endpoint and no `enabled` would export nothing, so it is refused
when the configuration loads, with `otlp sets endpoint but not enabled; say
enabled: true to turn it on, or enabled: false to keep the settings without
using them`.

What the export sends and connects with is checked when the configuration
loads, as a collector's request is, rather than failing every export:

- `headers` names must be header names, each set once whatever its case, and
  their values hold no control character but a tab;
- `tls.cert_file` and `tls.key_file` are set together, and the `tls` files —
  `ca_file` too — must be there and hold a certificate or key: the export
  needs them from its first request. A reload that finds one missing or
  half replaced, as while a Secret is rotated, is rejected, and with
  `--config.watch` tried again once the file
  [is in place](CONFIGURATION.md#watching-the-configuration);
- `interval` is at least `1s`, and `timeout` is not negative; left out, they
  are `30s` and `5s`;
- `resource_attributes` may not set `service.name`, which `service_name`
  sets — the resource would carry it twice. The same holds for a static
  target's `otlp.resource_attributes` and its `otlp.service_name`.

OTLP export is best-effort and does not make a Prometheus probe fail. Metric
values are buffered as latest values and exported every `otlp.interval`;
the default is 30 seconds. Each export request is bounded by `otlp.timeout`,
which defaults to 5 seconds. The connection to the endpoint is kept and reused
from export to export (see [Connections](REQUESTS.md#connections)), and goes
through the proxy the environment names, if any. Self-health metrics are
included in every export interval even when no Prometheus self-metrics scrape
is running.

Each data point carries the time it was scraped, or the timestamp the target
gave it, not the time of the export that sends it, so a point that waited for
the next export, or through an outage, is not taken for a newer one. An answer
from a collector's [cache](CONFIGURATION.md#response-caching) is exported as
of the scrape that filled the entry, not as of the answer: a cached result
served for a minute, or a
[stale one](CONFIGURATION.md#serving-the-last-good-result-when-the-target-fails)
served for as long as the target is down, is one old measurement, not a new
one each time. The series the exporter adds about the answer itself —
`http_exporter_result_stale`, `http_exporter_result_age_seconds` and a static
target's `http_exporter_target_*` — are as of the answer. A
cumulative point — a counter, a histogram, a summary — also carries the time
its series started: the first export of the series, and again after a reset,
when its count went down, as the OpenTelemetry Collector's Prometheus receiver
does. The exporter cannot know when a target began counting, so this is the
earliest it can vouch for; a series not exported for an hour starts again.
The exporter's own counters, histograms and summaries are the exception: it
knows when each began to count, and their points start at that time (see
[Created timestamps](SELF-METRICS.md#created-timestamps)).
The start times remembered are bounded too, at twice
[`otlp.max_pending_points`](#delivery) (200000 by default), so series whose labels
keep changing cannot grow them without limit; past it the series exported
least recently is forgotten first, and starts again if it comes back.

## Probes of several targets

Probe results are exported under the one exporter-wide resource, where a
series is known by its name and labels. Two probes answering the same series —
two targets behind one collector, or two collectors that name a metric alike —
are one series there, and the later probe's point replaces the earlier's
before the export. That is the default, and right when a probe's own labels
already tell its series apart. Set `otlp.probe_attributes: true` to keep them
apart anyway: each point a probe queues then carries a `collector` attribute,
and a `target` attribute when the probe named one — the target as logs show
it, without credentials: its password is withheld, as are the values of query
parameters named like credentials (`token`, `api_key`, `secret`, …), while
other parameters, such as `tenant`, are kept, so targets that differ in them
stay apart. A label the series has of its own by either name is
kept. Static targets are unaffected: they have a resource of their own
([below](#static-targets)).

## Shutting down

The export keeps running through `--web.shutdown-delay`, while the endpoints
are still served and the static targets still scraped, and stops when the
graceful shutdown begins; what was queued since goes out in one last export,
bounded by `otlp.timeout`.

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
past it the oldest data points — those of failed exports first, the
longest-waiting of them first however many exports in a row have failed — are
dropped, down to nine tenths of the limit, counted in
`http_exporter_otlp_points_dropped_total` and logged as a warning. Any other
answer, such as `400` or `401`,
would be given again: the data points are dropped and counted rather than sent
again forever, and the warning quotes the start of the endpoint's explanation
as `response_body`. An endpoint can also accept an export but reject some of
its data points, saying so in the answer's `partialSuccess`: those points are
counted in `http_exporter_otlp_points_dropped_total` too, and the warning
carries `rejected_points` and the endpoint's `error_message`. The export itself
still counts as a success. A `partialSuccess` with a message and nothing
rejected is logged as a warning only. Each failure is logged as a warning, and the export status is in
the [self-metrics](SELF-METRICS.md#otlp-export-status):

```promql
# No export has got through for ten minutes.
time() - http_exporter_otlp_last_export_success_timestamp_seconds > 600
```

Failing exports do not make the exporter unready by default: it still answers
probes, and in Kubernetes a pod that is not ready stops receiving them. For an
exporter whose job is delivering [static targets](STATIC-TARGETS.md) over
OTLP, set `otlp.unready_after_failures: 3` to have `/ready` answer `503` after
three failed exports in a row, until one gets through (see
[Readiness](CONFIGURATION.md#readiness)). The count is per endpoint: a reload
that changes `otlp.endpoint` starts it again.

On `SIGTERM` or `SIGINT` the exporter first lets the probes in progress finish,
then makes one last export, bounded by `otlp.timeout`, of what they, earlier
probes and static targets queued, with a last self-metric snapshot. Static
targets are not scraped again for it. An export the shutdown interrupted is not
lost: its data goes out with that last export. A second signal ends the process at once,
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
the highest bound rather than a bound. A histogram the target wrote without a
`_sum` is sent without a sum, which OTLP allows. OTLP's summary has no way to
leave its count or its sum out — a field not set is `0` there — so a summary
the target wrote without a `_count` or a `_sum` is sent with `0` for it, where
the exposition formats leave the line out. Every series of a metric is a data point
of one OTLP metric. A `NaN` or infinite value is sent as `"NaN"`, `"Infinity"`
or `"-Infinity"`, as the OTLP JSON encoding writes them, rather than failing
the export. A histogram's buckets and a summary's quantiles are sent in
ascending order, in whatever order the target wrote them.

A family keeps its type only where its values allow it. The Prometheus text
format lets a target write a counter that is `NaN` or negative, a histogram
whose bucket counts fall, whose `_sum` is `NaN` or negative or stands beside a
negative bound, which has a bound that is not a number, whose `+Inf` bucket
and `_count` differ — as a target scraped between two of its updates writes
them — or which has neither of the two, and a summary whose
`_sum` is `NaN` or negative, with a quantile outside 0 to 1 or with a negative
value for one: the families the
[OpenMetrics output](CONFIGURATION.md#openmetrics) writes as `unknown`. None of
them can be a monotonic sum, a histogram or a summary in OTLP either — a
bucket's own count would be negative, a histogram point has one count where
the target wrote two, and OTLP's quantiles are within 0 to 1
and not negative. Such a family is exported as gauges under the names of its
samples, with the series and values the text format writes, so every point of
an export is valid and none is dropped:

| Prometheus family | OTLP gauges |
| --- | --- |
| counter `jobs_total` | `jobs_total` |
| histogram `h` | `h_bucket` with an `le` attribute, `+Inf` among them, with its own count, where the series has that bucket or a count; `h_sum` and `h_count` where the series has them |
| summary `s` | `s` with a `quantile` attribute; `s_sum` and `s_count` where the series has them |

`le` and `quantile` are written as the text format writes them (`0.5`, `1`,
`+Inf`). The whole family is exported so when one of its series in the export
is such a series, so that a name is one kind of metric in an export, and its
points carry no start time, as no gauge does. A family whose values its type
allows always keeps the type.

## Static targets

[Static targets](STATIC-TARGETS.md) are scraped by the exporter itself and
served on the static targets endpoint. A target with `export_via_otlp: true` is
also delivered here, on `otlp.interval`, with its health metrics, under an OTLP
resource of its own that its `otlp` block sets over the exporter-wide
`service_name` and `resource_attributes`:

```yaml
interval: 1m
targets:
  - name: legacy_eu
    collector: legacy_text
    target: http://legacy.eu.example:8080
    export_via_otlp: true
    otlp:
      service_name: legacy-app
      resource_attributes:
        deployment.environment: production
```

Targets with different identities are exported as separate `resourceMetrics`
entries. Each export delivers the latest value of each series the targets'
scrapes left since the last one: they are scraped on their own intervals, not
on `otlp.interval`. A target that sets `export_via_otlp` while OTLP export is
disabled or has no endpoint stops the exporter at startup, and a reload that
would disable OTLP export while one is loaded is rejected.
