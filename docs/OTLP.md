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
  # batch_max_size: 8192
  # batch_max_bytes: 4MiB
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
  # metric_names: prometheus
  # exponential_histograms: false
  # scope:
  #   name: prometheus-universal-exporter
  #   version: ""
  # logs:
  #   enabled: true
  #   level: info
  # traces:
  #   enabled: true
  #   threshold: 1s
  #   errors: false
  # exporter_instance:
  #   enabled: true
  #   attribute: pue.exporter.instance
  #   value: ""
```

`logs` forwards the exporter's own log too; see
[The exporter's own log](#the-exporters-own-log). `traces` exports a trace
of each slow probe and static target scrape; see
[Traces of slow requests](#traces-of-slow-requests). `metric_names` is how
metrics are named ([Metric names](#metric-names)), `scope` the
instrumentation scope they are sent under
([Instrumentation scope](#instrumentation-scope)), and `exporter_instance`
names the replica on every resource it sends
([The exporter instance](#the-exporter-instance)).

`enabled` is the switch, and a block that sets anything must say it: `true`
exports, `false` keeps the settings without using or checking them: a block
that is switched off may hold an endpoint that is no URL, a `compression`
that is neither `gzip` nor `none`, a `metric_names` that is neither
`prometheus` nor `otel`, a `scope` without a name, a negative duration or
count, and the
[schema](CONFIGURATION.md#editor-support) flags none of them, as the exporter
refuses none. Only what cannot be read as the key's kind of value at all,
such as `timeout: soon` or an unknown key, is refused there too. An `otlp`
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

`tls.insecure_skip_verify: true` turns off the check of the endpoint's
certificate; it is the only key that does — an `insecure_skip_verify` beside
`tls` is refused as a key the `otlp` block does not have.

An attribute of `resource_attributes` written `""` is the attribute left
out, as a [`transform.labels`](CONFIGURATION.md#collector-wide-labels) value
written `""` is the label left out: the resource does not carry it, where it
used to carry an attribute without a value, and a static target's sets
nothing, so the exporter-wide attribute of that name stays. A data point
carries no such attribute either: a label with an empty value is on no
series the exporter makes, whether a rule, a script or the target's own
exposition gave it, so an export has no attribute that says nothing.

OTLP export is best-effort and does not make a Prometheus probe fail. Metric
values are buffered as latest values and exported every `otlp.interval`;
the default is 30 seconds. Each export request is bounded by `otlp.timeout`,
which defaults to 5 seconds. The connection to the endpoint is kept and reused
from export to export (see [Connections](REQUESTS.md#connections)), and goes
through the proxy the environment names, if any. Self-health metrics are
included in every export interval even when no Prometheus self-metrics scrape
is running.

An interval is that of the configuration in force, not of the one in force
when the wait for the next export began: a reload, or in allocator mode a
bundle, that changes `otlp.interval`, `otlp.logs.interval` or
`otlp.traces.interval` has the next export of that signal an interval of the
new configuration after the last one ended, and at once when that is past.
Shortening an interval from `45s` to `1s` has the next export a second after
the last rather than 45 seconds; lengthening it from `1s` to `1h` makes none
when the second runs out; and an exporter in allocator mode, which begins
with no configuration and so with the default intervals, follows its first
bundle's from when it comes.

Each data point carries the time it was scraped, or the timestamp the target
gave it, in whole milliseconds as a Prometheus sample's timestamp is, not
the time of the export that sends it, so a point that waited for
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
least recently is forgotten first, and starts again if it comes back. Each
keeps at most 256 bytes of its series, a series whose resource, name and
labels take more by a digest of them, so a start time is a few hundred bytes
however long a label is — a probe's target, with `probe_attributes`, may be
8 KiB — and the start times at their bound take at most about 80 MB.

A series is its resource, its name, its type and its labels, exactly as they
are. A label's value may be any text a target gives, an `=` or a NUL
character included, and a service name and a resource attribute's name and
value are whatever the configuration says: two series that differ anywhere,
and two resources that differ in the service name or in an attribute's name
or value, are exported apart, each with its own points and its own start
times.

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

## Two writers of one series or one name

To an OTLP receiver a data point is one of a stream: its resource, its
metric — the name and the kind: gauge, sum, histogram or summary — and its
attributes. A stream has one writer, and a name is one metric of one kind
under a resource. Sent two points of one stream in a request, a receiver
keeps either or refuses the request; sent a name as a gauge and as a sum, it
keeps one, or neither.

One probe's answer has neither: a series twice, a name with two types, or a
gauge named like a sample of a histogram beside it, fails the scrape. But
everything under one resource goes out together — the answers of every probe,
the static targets without a resource of their own, and the exporter's own
metrics — and two of them can write what is one stream, or one name, to the
receiver. An export has each stream once and each name as one kind, by the
rule a series already has while it waits: **the later replaces the earlier**.

- **Two points of one stream.** The one written last is exported. That is
  two probes answering the same series, as [above](#probes-of-several-targets),
  and also what is two series to Prometheus and one stream over OTLP: a gauge
  and an untyped series of one name and labels, both gauges here; a histogram
  `h` exported [as gauges](#delivery) and another probe's gauge `h_bucket`
  with the `le` of one of its buckets; and a series a probe reads that is
  named and labelled like one of the exporter's own, as when a collector
  scrapes the exporter's `/metrics` — the exporter's own point, taken at the
  export, is the one sent, where the export used to carry both. Nothing is
  logged, as nothing is when a later scrape's value replaces a series'.
- **One name as two kinds.** A gauge `jobs` of one probe and a counter `jobs`
  of another are one name, whatever their labels: `otlp.probe_attributes`
  keeps points apart, not metrics. With
  [`metric_names: otel`](#metric-names) the names are those exported: a gauge
  `jobs` and a counter `jobs_total` are the name `jobs` of two kinds, and a
  gauge `latency` and a gauge `latency_seconds` the name `latency` of two
  units, which is two kinds here too, the log line naming the unit
  (`"left_out_kind":"gauge of unit s"`). The kind written last is exported, with
  every point of that kind, and the points of the other kind are left out of
  that export, where it used to carry two metrics of the name. Which of the
  two is last may change from one export to the next, so this is something to
  fix, and it is logged as a warning, once and then as a
  [repeated failure](LOGGING.md#repeated-failures), whichever kind wins:

  ```json
  {"level":"WARN","msg":"OTLP metric name written as two kinds under one resource; the data points of the kind written earlier are left out of the export","metric":"jobs","kind":"sum","left_out_kind":"gauge","left_out_points":2,"service_name":"prometheus-universal-exporter","error":"two writers of one OTLP resource - probes, static targets, or the exporter with its own metrics - export this metric name as different kinds, and a name is one metric of one kind there; rename one of the metrics, or give a static target a resource of its own with its otlp.service_name or otlp.resource_attributes"}
  ```

  Rename one of the metrics — a rule's `name`, or the collector's
  [`metrics_prefix`](CONFIGURATION.md#prefixing-a-collectors-metrics) — or give a static
  target a [resource of its own](#static-targets). The line is not followed by
  one that says it is over: a name not written as two kinds for an hour is
  forgotten, and logged in full if it happens again.
- **One name for two Prometheus names.** With
  [`metric_names: otel`](#metric-names) two metrics of one kind and unit
  whose Prometheus names differ are one name too: a counter `x_seconds` and a
  counter `x_seconds_total` are both the sum `x` of unit `s`, and a counter
  `requests` and a counter `requests_total` both the sum `requests`. They are
  two series to Prometheus, and one metric of the export would merge them,
  keeping one point where their labels are the same. The Prometheus name
  written last is exported, with every point of it, and the points of the
  other, whatever their labels, are left out of that export; it is logged as
  a warning as a name of two kinds is, once and then as a repeated failure,
  apart from it:

  ```json
  {"level":"WARN","msg":"OTLP metric name written for two Prometheus metric names under one resource; the data points of the name written earlier are left out of the export","metric":"x","prometheus_name":"x_seconds_total","left_out_prometheus_name":"x_seconds","left_out_points":1,"service_name":"prometheus-universal-exporter","error":"otlp.metric_names: otel exports these Prometheus metric names of one OTLP resource under one name, and a name is one metric there, whose points of the two would be one stream where their labels are the same; rename one of the metrics, or export with otlp.metric_names: prometheus"}
  ```

  Rename one of the metrics, or export with `metric_names: prometheus`,
  which keeps every name as the exposition writes it. A series written twice
  under one Prometheus name, as a histogram `h` exported as gauges and a
  gauge `h_bucket`, is one stream, the first case above, and not this.

Written last is queued last: the probe or scrape that ran last, whatever
timestamps its series carry, with the exporter's own metrics after every one
of them. A point that waits again after a failed export is as old as when it
was first queued, so it is still the earlier beside one queued since. Points
left out so are replaced, not given up on: they are not counted in
`http_exporter_otlp_points_dropped_total`.

A counter, a histogram or a summary left out of an export for another kind
of its name has still been seen: its start time is that of the first point
the exporter had of it, and a count that fell in an export that left it out
is the reset it is. Two probes that write one counter are one series to the
start times as to the receiver: when one's count is below the other's last
it reads as a reset and the series starts again, and the higher count after
it as growth — one counter cannot be two.

## Shutting down

The export keeps running through `--web.shutdown-delay`, while the endpoints
are still served and the static targets still scraped, and stops when the
graceful shutdown begins; what was queued since goes out in one last export,
bounded by `otlp.timeout`. If that last export fails, there is no next one to
keep its data points for: they are dropped, counted in
`http_exporter_otlp_points_dropped_total`, and the warning says how many, as
`dropped_points`.

## Delivery

A request is OTLP/HTTP with a JSON body (`Content-Type: application/json`),
in the OTLP specification's JSON encoding: an enum is written as its number,
so a sum's and a histogram's `aggregationTemporality` is `2`, cumulative, and
a log record's `severityNumber` a number, as every OTLP receiver reads them.

Requests are gzipped (`Content-Encoding: gzip`), which every OpenTelemetry
Collector accepts; set `otlp.compression: none` for an endpoint that does not.
Any other value is refused when the configuration loads, in a block that is
switched on.

An export is sent in requests of at most `otlp.batch_max_size` data points,
8192 by default, and at most `otlp.batch_max_bytes` of JSON before
compression, 4MiB by default and at least 64KiB: a receiver refuses a request
past its own bound — the OpenTelemetry Collector's OTLP/HTTP receiver one of
over 20MiB, its gRPC receiver one of over 4MiB — and an export of every point
that waited, which after an outage can be `otlp.max_pending_points` of them,
would otherwise be refused whole. Nearly every export is one request. One of
more is split in the order of its resources, their metrics and their points,
each request as full as the bounds let it be, and a resource or a metric that
goes on in the next request is written again there; a single data point
larger than `otlp.batch_max_bytes` on its own is sent in a request of its own,
for the receiver to take or refuse. The requests of one export are sent one
after another, within the export's time, each with its own retries, and the
export stays one export to the [self-metrics](SELF-METRICS.md#otlp-export-status).

A request that fails with a network error, or with `429`, `502`, `503` or
`504` — the answers the OTLP specification makes retryable — is tried again
after 1 second, then 2, 4, and so on up to 16, or after the `Retry-After` the
endpoint asks for. Retries go on while another attempt can still start within
`otlp.interval`, so an export never runs into the next one. When they run out,
the data points of that request and of the requests of the export not sent
yet are kept and sent with the next export, unless a newer value of the same
series has arrived in the meantime; those of the requests the endpoint took
before it are not sent again. Only the latest value of each
series is kept, but an outage long enough can still see many series come and
go, so what waits is bounded by `otlp.max_pending_points`, 100000 by default.
A data point waiting keeps its labels, and of its series no more than 256
bytes besides, a digest of a longer series; with `probe_attributes`, the
points of one probe share one copy of its target, and nothing else of the
probe's request. So a point is a few hundred bytes, and one whose probe's
target is long that much more: with every point of a different probe whose
target is 8 KiB, the most a target may be, the points at the default bound
take about 870 MB, which a lower `max_pending_points` lowers. Past the bound
the oldest data points — those of failed exports first, the
longest-waiting of them first however many exports in a row have failed — are
dropped, down to nine tenths of the limit, counted in
`http_exporter_otlp_points_dropped_total` and logged as a warning. Any other
answer, such as `400` or `401`,
would be given again: the data points of that request are dropped and counted
rather than sent again forever, the warning quotes the start of the endpoint's
explanation as `response_body`, and the export goes on with its next request.
A redirect (`3xx`) is not followed, since that would send `otlp.headers` and
the data to wherever the endpoint points, another host or plain `http://`
included: it is refused the same way, and its warning says the redirect was
not followed and carries the status and, as `location_host`, the host it
leads to. Set `otlp.endpoint` to the URL the export should go to.
An endpoint can also accept a request but reject some of
its data points, saying so in the answer's `partialSuccess`: those points are
counted in `http_exporter_otlp_points_dropped_total` too, and the warning
carries `rejected_points` and the endpoint's `error_message`, for each request
that says so. The export itself still counts as a success. A `partialSuccess` with a message and nothing
rejected is logged as a warning only. Each failure is logged as a warning, and the export status is in
the [self-metrics](SELF-METRICS.md#otlp-export-status):

```promql
# No export has got through for ten minutes.
time() - http_exporter_otlp_last_export_success_timestamp_seconds > 600
```

The [status page](ENDPOINTS.md#otlp-export) shows the same without a
Prometheus to ask: the endpoint with its credentials withheld, the last
export and the last that got through, the failures in a row and the
readiness they make, the last failure's error, the data points pending and
those dropped, by why, and the retries.

Failing exports do not make the exporter unready by default: it still answers
probes, and in Kubernetes a pod that is not ready stops receiving them. For an
exporter whose job is delivering [static targets](STATIC-TARGETS.md) over
OTLP, set `otlp.unready_after_failures: 3` to have `/ready` answer `503` after
three failed exports in a row, until one gets through (see
[Readiness](CONFIGURATION.md#readiness)). The count is per endpoint: a reload
that changes `otlp.endpoint` starts it again. It is at most 1000: the status
page keeps when each failure of a run ended, up to that many, so that it can
say when the exporter became unready under any count a reload sets.

On `SIGTERM` or `SIGINT` the exporter first lets the probes in progress finish,
then makes one last export, bounded by `otlp.timeout`, of what they, earlier
probes and static targets queued, with a last self-metric snapshot. Static
targets are not scraped again for it, and their scrapes in flight are cut
short rather than waited for, so what those would have queued is not in it. An export the shutdown interrupted is not
lost: its data goes out with that last export; if the last export fails, its
data points are dropped and counted. A second signal ends the process at once,
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
ascending order, in whatever order the target wrote them. A histogram a rule
counts of the values it reads
([Histograms from a list of values](CONFIGURATION.md#histograms-from-a-list-of-values))
is sent as one the target wrote is. It describes the values of one response,
so its counts fall when the target's list shrinks, which starts its series
again as a cumulative histogram whose count went down does; a backend that
takes it for a running total reads it as reset on every such scrape.

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

## Seeing what is exported

With `--web.debug`, the debug page's *OTLP* tab shows what the next export
would send of a run's series — the resource, the scope, each data point
with what was made of it, the points left out and why, and the request's
JSON — and, with `otlp.logs`, the run's log lines as log records, and with
`otlp.traces` the run's spans ([below](#traces-of-slow-requests)); at
`full` it tests each endpoint with one empty export. Nothing of a run is
exported. See [The debug page](CONFIGURATION.md#the-debug-page).

## Metric names

`otlp.metric_names` is how the export names metrics. `prometheus`, the
default, names each as the exposition writes it, `http_requests_total` and
`request_duration_seconds`, with no unit. `otel` names them as
OpenTelemetry does: a counter's name loses its `_total`, and a name that ends
in a unit loses it to the metric's `unit` field, a
[UCUM](https://ucum.org/ucum) code, so the counter `http_requests_total` is
`http_requests`, the histogram `request_duration_seconds` is
`request_duration` of unit `s`, and the counter `process_cpu_seconds_total` is
`process_cpu` of unit `s`. A UTF-8 name a collector keeps
([`metric_name_validation_scheme: utf8`](CONFIGURATION.md#keeping-utf-8-names)),
`http.server.duration_seconds`, is sent as it is under `prometheus` and as
`http.server.duration` of unit `s` under `otel`, its labels' names as its
attributes' keys, dots and all. The units are those the OpenTelemetry Collector's
Prometheus translator writes as a name's suffix, read backwards:

| Suffix | Unit | Suffix | Unit | Suffix | Unit |
| --- | --- | --- | --- | --- | --- |
| `_days` | `d` | `_bytes` | `By` | `_meters` | `m` |
| `_hours` | `h` | `_kibibytes` | `KiBy` | `_volts` | `V` |
| `_minutes` | `min` | `_mebibytes` | `MiBy` | `_amperes` | `A` |
| `_seconds` | `s` | `_gibibytes` | `GiBy` | `_joules` | `J` |
| `_milliseconds` | `ms` | `_tibibytes` | `TiBy` | `_watts` | `W` |
| `_microseconds` | `us` | `_kilobytes` | `KBy` | `_grams` | `g` |
| `_nanoseconds` | `ns` | `_megabytes` | `MBy` | `_celsius` | `Cel` |
| `_ratio` | `1` | `_gigabytes` | `GBy` | `_hertz` | `Hz` |
| `_percent` | `%` | `_terabytes` | `TBy` | | |

A unit followed by `_per_` and `second`, `minute`, `hour` or `day` is a unit
of a time, `s`, `min`, `h` or `d`: `transfer_bytes_per_second` is `transfer`
of unit `By/s`. The translator writes a unit `By/m` as `bytes_per_minute`,
but `m` is the metre in UCUM, so a minute here is `min`; a `_per_` after
anything but a unit of the table, as in `requests_per_second`, stays in the
name.

A suffix is a whole word at the end of the name, after its last `_`, with
something before it: `latency_seconds` loses `_seconds`, `xseconds`,
`seconds_count` and a gauge named `seconds` keep their names, and a recording
rule's `job:latency_seconds:mean` keeps its name too. Only a counter loses
`_total`; a gauge `errors_total` is exported as it is named. A histogram and a
summary are named by their family, `request_duration_seconds`, not their
samples. The names are the exporter's own metrics' too:
`http_exporter_otlp_points_dropped_total` is `http_exporter_otlp_points_dropped`, and
`http_exporter_otlp_export_duration_seconds` is
`http_exporter_otlp_export_duration` of unit `s`.

A family [exported as gauges](#delivery) under the names of its samples, as a
counter whose value is `NaN` is, keeps those names and has no unit: they are
the samples the exposition writes, which no OpenTelemetry metric is named
after. A Prometheus target's `# UNIT` line of OpenMetrics is not read: the
exporter keeps no unit of a family, so the unit is the name's.

Two names the translation makes one — a gauge `jobs` and a counter
`jobs_total`, or a gauge `latency` and a gauge `latency_seconds` — are one
name under the resource, written as two kinds
([above](#two-writers-of-one-series-or-one-name)): what was written last is
exported, and the other is left out and logged.

## Exponential histograms

The exporter's own duration histograms,
`http_exporter_probe_duration_seconds` (see
[Probe durations](SELF-METRICS.md#probe-durations)) and, in verbose mode,
`http_exporter_collector_scrape_duration_seconds` (see
[Scrape-time histograms](SELF-METRICS.md#scrape-time-histograms)), are kept in
fixed buckets and in exponential ones. They are exported with their fixed
buckets unless the block says otherwise:

```yaml
otlp:
  exponential_histograms: true
```

Each is then an OTLP exponential histogram: cumulative, with its `scale`, the
count of durations of 0 in `zeroCount`, the counts of its buckets from
`positive.offset` on, its sum, count, `min` and `max`. Bucket `i` holds the
durations above base^i and up to base^(i+1), the base being 2^(2^-scale),
as OpenTelemetry's SDKs bucket them: the histogram starts at scale 20 and
lowers the scale, merging neighbouring buckets in pairs, whenever it would
need more than 160 buckets, so a collector whose scrapes take 5 ms to a
minute is sent at scale 3, a bucket per 9% of the duration.

It is off by default because the receiver has to take exponential
histograms: an OpenTelemetry Collector does, while Prometheus' OTLP endpoint
stores them as native histograms only when it ingests those. Every other
histogram — anything read from a target — has fixed buckets alone and is sent
as before. The self-metrics endpoint answers the same buckets to a Prometheus
that asks for protobuf ([native histograms](SELF-METRICS.md#native-histograms)).

## Instrumentation scope

Every metric of the export is under the instrumentation scope
`prometheus-universal-exporter`, without a version, and every log record of
[`otlp.logs`](#the-exporters-own-log) and span of
[`otlp.traces`](#traces-of-slow-requests) too. `otlp.scope` names another,
with a version if wanted, for all three:

```yaml
otlp:
  enabled: true
  endpoint: http://otel-collector:4318/v1/metrics
  scope:
    name: acme.example/edge-exporter
    version: "2.4.0"
```

A `scope` written has a `name`, which is not empty, and neither the name nor
the version holds a control character; a block switched on is refused
otherwise when the configuration loads. The scope is the export's: a static
target sets its own resource, not its own scope.

## The exporter instance

Every replica of the exporter sends its export under the same
`service_name` and `resource_attributes`, so a backend cannot tell from a
resource which replica a point or a log record came from. With
`otlp.exporter_instance` switched on, every resource the replica sends
carries one more attribute naming it: the exporter-wide resource of the
probes' series and of the exporter's own metrics, the resource of each
[static target](#static-targets), with or without an `otlp` block of its
own, and the resources of the [log records](#the-exporters-own-log) and of
the [traces](#traces-of-slow-requests):

```yaml
otlp:
  enabled: true
  endpoint: http://otel-collector:4318/v1/metrics
  exporter_instance:
    enabled: true
    # attribute: pue.exporter.instance
    # value: ""
```

- `enabled` switches the block, as `logs` is switched: a block that sets
  anything must say it, and `enabled: false`, the default, keeps its settings
  unchecked and the export as it was, byte for byte.
- `attribute` is the attribute's key, `pue.exporter.instance` by default. It
  is not blanks alone and holds no control character.
- `value` is the attribute's value, as written, `${NAME}` expanded with
  [`--config.expand-env`](CONFIGURATION.md#environment-variables), as every
  value is. Left out, or written `""`, it is the
  [`--allocator.member`](ALLOCATOR.md) name in allocator mode, which is the
  name the allocator knows the replica by, and otherwise the host name, a
  pod's name in Kubernetes. It holds no control character.

The value is resolved when the configuration loads, at startup and at each
reload, and in allocator mode at each bundle; the start's `starting exporter`
line says it as `otlp_exporter_instance_attribute` and
`otlp_exporter_instance`, the [status page](ENDPOINTS.md#otlp-export) shows
the attribute and the value in force, and
[`--dry-run`](CONFIGURATION.md#dry-run) reports them under
`otlp_exporter_instance`. A host name that cannot be read, with no value
written, is refused when the configuration loads, rather than the attribute
left out without a word: `otlp.exporter_instance.value is left out and the
host name, its default, cannot be read (...); set the value, such as
${POD_NAME} with --config.expand-env`.

The attribute is part of each resource's identity, as every attribute is
([above](#static-targets)): static targets with different identities are
still exported apart, and the [two writers](#two-writers-of-one-series-or-one-name)
of one resource are the same writers. Since the resource would otherwise
carry the key twice, or one value would quietly replace the other, the
configuration is refused when the attribute is:

- `service.name`, which `service_name` sets;
- a key of `otlp.resource_attributes`, written `""` or not;
- on a static target exported over OTLP, a key of its own
  `otlp.resource_attributes`, a label of its `otlp.labels_as_resource`, or
  `service.instance.id` where the target sets `otlp.instance_id`; the message
  names the target.

The default is not `service.instance.id`, OpenTelemetry's attribute for the
instance of a service, for two reasons. Prometheus' OTLP receiver makes
`service.instance.id` the `instance` label of every series it ingests, in
place of the target the exporter scraped, which is what `instance` says of a
probe; and a static target's [`otlp.instance_id`](#static-target-labels-as-resource-attributes)
sets it to the target's name. `attribute: service.instance.id` is taken where
no static target sets `instance_id`, for a backend that wants it.

With the [allocator](ALLOCATOR.md), a static target is scraped by one replica
at a time, and moves when replicas come and go. After a move its data goes on
under the other replica's name: a new resource, and so new series, in a
backend that keys series on resource attributes. Prometheus keys them on
`job` and `instance` alone, from `service.name` and `service.instance.id`, and
keeps the other resource attributes on `target_info`, so there the move shows
as a new `target_info` series, unless the attribute is promoted to a label of
every series (`otlp.promote_resource_attributes`), which makes it one more
label that changes on a move.

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
entries; a target without an `otlp` block is exported under the exporter-wide
resource, beside the probes' series and the exporter's own, where a name it
shares with them must be one kind of metric
([above](#two-writers-of-one-series-or-one-name)). Each export delivers the latest value of each series the targets'
scrapes left since the last one: they are scraped on their own intervals, not
on `otlp.interval`. A target that sets `export_via_otlp` while OTLP export is
disabled or has no endpoint stops the exporter at startup, and a reload that
would disable OTLP export while one is loaded is rejected.

### Static target labels as resource attributes

A target's `labels` are on every series of it, and over OTLP on every data
point. Labels that say what the target is, such as its city or its plant,
belong to its resource instead, where a backend groups by them.
`otlp.labels_as_resource` names them, and `otlp.instance_id: true` gives the
resource the attribute `service.instance.id`, the target's name:

```yaml
interval: 1m
targets:
  - name: plant_berlin
    collector: legacy_text
    target: http://plant.berlin.example:8080
    labels:
      city: berlin
      line: "3"
    export_via_otlp: true
    otlp:
      labels_as_resource: [city]
      instance_id: true
```

The target's resource then has `city: berlin` and
`service.instance.id: plant_berlin` besides its service name and its
attributes, and its data points, of its own series and of its health series
alike, have no `city`. A series whose own label of the name has another value
— one its collector gave it, which a target's label does not replace, or a
health series' `collector` or `target` — keeps it: the resource does not say
it. A label written `""` is on no series and is no attribute. The static
targets endpoint serves the labels as before; only the export moves them.

The attributes are part of the resource's identity: targets alike in every
attribute, `city` included, are one resource, and their series are kept apart
by `static_target`, as before; a target with `instance_id` is always one of
its own. Both keys are off by default, and the export is then what it was.
Each is checked when the file loads, with the configuration it goes with, and
refused:

- a name that is not among the target's `labels` — a label the collector
  extracts can differ from series to series, which a resource cannot;
- a name given twice;
- a name the resource has as an attribute already, from the target's
  `otlp.resource_attributes` or the exporter-wide ones it keeps, and
  `instance_id` beside a `service.instance.id` attribute: the resource would
  carry the key twice. A label's name has no dot, so it is never
  `service.name`;
- either key on a target without `export_via_otlp`, as every key of its
  `otlp` block is;
- a label that is the [`otlp.exporter_instance`](#the-exporter-instance)
  attribute, and `instance_id` where that attribute is `service.instance.id`.

## The exporter's own log

With `otlp.logs`, the exporter sends its own log lines over OTLP as well, to
the collector its metrics go to, as OTLP log records. stderr keeps every line
as it was:

```yaml
otlp:
  enabled: true
  endpoint: http://otel-collector:4318/v1/metrics
  logs:
    enabled: true
    # endpoint: http://otel-collector:4318/v1/logs
    # level: info
    # interval: 5s
    # max_pending_records: 10000
```

`logs` is switched by its own `enabled`, as the `otlp` block is: a `logs`
block that sets anything must say it, and one with `enabled: false` keeps its
settings unchecked. It is used only while the `otlp` block is switched on,
and takes that block's `headers`, `tls`, `compression`, `timeout`,
`batch_max_size`, `batch_max_bytes`, `service_name` and
`resource_attributes`: the records are under the resource the exporter's own
metrics are under, with the [scope](#instrumentation-scope) they are under,
`prometheus-universal-exporter` unless `otlp.scope` names another. Its own
keys:

- `endpoint` is the OTLP/HTTP logs endpoint. Left out, it is `otlp.endpoint`
  with its `/v1/metrics` made `/v1/logs`, the collector's standard path for
  logs, its query kept; an `otlp.endpoint` that does not end in `/v1/metrics`
  has no such path, so `endpoint` is then required, and its absence refused
  when the configuration loads. The export is OTLP/HTTP with a JSON body, as
  the metrics' is: the exporter speaks no gRPC.
- `level` is the least level of a line exported, `debug`, `info`, `warn` or
  `error` in any case, `info` by default, whatever `--log.level` writes to
  stderr: with `level: debug` and `--log.level=info` the debug lines are
  exported and not written, and with `level: warn` the info lines are written
  and not exported.
- `interval` is how often the records waiting are sent, `5s` by default and
  at least `1s`.
- `max_pending_records` bounds the records waiting, `10000` by default.

Each line is one record: its time, and as its observed time when the export
took it; its level as the severity number the OTLP specification gives a
slog level — `DEBUG` 5, `INFO` 9, `WARN` 13, `ERROR` 17 — with the level's
name as the severity text; its message as the body; and its attributes, each
of its own type: text as a string, a whole number as an int, a number with a
fraction as a double, `true` and `false` as a bool. A whole number past the
range of an int, and a number that is not one or is infinite, is a string, as
are a duration (`1.5s`), a time (RFC 3339) and an error (its text). An
attribute of a group is named after the group and a dot, `group.key`, as
stderr's JSON nests it. A record has each key once, as OTLP requires: a key
given twice, by the logger and by the line, is the value the line gave,
where stderr writes both. A value whose text cannot be had — a nil error
whose `Error` does not guard against nil — is `<nil>`, as on stderr, and one
whose `Error` or `String` panics otherwise is `!PANIC: ` and the panic, as
slog writes it. The lines say what stderr says, credentials withheld
alike: the redaction is where a line is made, not in what writes it.

Logging never waits for the export. A line is queued as it is written, and one
that finds `max_pending_records` waiting is dropped and counted in
`http_exporter_otlp_log_records_dropped_total` rather than waited for: a
collector that is down holds up nothing but the records. An export sends
everything waiting, in requests of at most `otlp.batch_max_size` records and
`otlp.batch_max_bytes` of JSON before compression, one after another, and an
interval with nothing waiting sends nothing. Retries are those of the metrics'
export ([Delivery](#delivery)): a request that fails with a network error,
`429`, `502`, `503` or `504` is tried again within the interval, and what was
not delivered when they run out waits for the next export, before the lines
logged since, the oldest dropped and counted past `max_pending_records`. A
request the endpoint refuses with any other answer, or a redirect, is dropped
and counted, and so are the records the endpoint rejects in a
`partialSuccess` (its `rejectedLogRecords`).

The export's own lines are not exported: what it logs of itself — a failure,
a retry at debug level, a recovery — is written to stderr alone, and says so
(`this line is not exported`). A collector that is down would otherwise have
each failed export queue a line about itself for the next one, which fails
too. A failure is a [repeated failure](LOGGING.md#repeated-failures), logged
once and then every five minutes while it goes on:

```json
{"level":"WARN","msg":"OTLP log export failed; its log records are kept for the next export; this line is not exported","endpoint":"http://otel-collector:4318/v1/logs","records":12,"retries":2,"kept_records":12,"error":"the OTLP endpoint answered 503"}
{"level":"INFO","msg":"OTLP log export recovered; this line is not exported","endpoint":"http://otel-collector:4318/v1/logs","stage":"export","failed_for":"1m0s","failures":12}
```

The lines the metrics' export logs are the exporter's like any other, and are
exported: a warning that a metrics export failed reaches the backend with the
next log export that gets through.

Nothing of debugging is exported either: the one line a debug probe, a
debug scrape, a run of the debug page or its endpoint test logs that it was
served (`probe debug report served`, `static target debug report served`,
`debug run served`, `debug OTLP endpoint test served`) is written to stderr
alone, as a debug run is not exported.

Failing log exports never make the exporter unready, whatever
`otlp.unready_after_failures` says: that counts the metrics' exports alone. The
log export's state is in the [self-metrics](SELF-METRICS.md#otlp-export-status)
and on the [status page](ENDPOINTS.md#otlp-export).

A reload that switches `logs` off stops taking lines at once and drops those
waiting, uncounted, as the metrics' export drops what waits while it is off; one
that switches it on takes the lines logged from then on. A reload that changes
the endpoint sends what waits to the new one. The lines logged while the
configuration is first read, before the exporter knows the export is on, are
on stderr alone.

At shutdown, the records waiting are sent after the metrics'
[last export](#shutting-down), with the lines that one logged among them,
within the same `otlp.timeout`: the log export has what the metrics' leaves of
it, and at least a second. If it fails, its records are dropped and counted.
Nothing logged after it is exported.

## Traces of slow requests

With `otlp.traces`, a probe or a static target's scrape that took
`threshold` or longer is exported as a trace, to the collector the metrics
go to: a span for the probe or the scrape, and under it a span for each
stage of its trip and each request it sent, which shows where its time went.
A fast one is not exported, and neither is a failed one unless `errors` says
so, nor one its caller sampled unless `parent_based` does, and a
`sample_ratio` share of the rest is
([Which traces are exported](#which-traces-are-exported)):

```yaml
otlp:
  enabled: true
  endpoint: http://otel-collector:4318/v1/metrics
  traces:
    enabled: true
    # endpoint: http://otel-collector:4318/v1/traces
    # threshold: 1s
    # errors: false
    # parent_based: false
    # sample_ratio: 0
    # max_per_second: 0
    # interval: 5s
    # max_pending_spans: 10000
    # propagate: false
```

`traces` is switched by its own `enabled`, as `logs` is: a block that sets
anything must say it, and one with `enabled: false` keeps its settings
unchecked. It is used only while the `otlp` block is switched on, and takes
that block's `headers`, `tls`, `compression`, `timeout`, `batch_max_size`,
`batch_max_bytes`, [`scope`](#instrumentation-scope) and resources. Its own
keys:

- `endpoint` is the OTLP/HTTP traces endpoint. Left out, it is
  `otlp.endpoint` with its `/v1/metrics` made `/v1/traces`, its query kept;
  an `otlp.endpoint` that does not end in `/v1/metrics` needs one written,
  and its absence is refused when the configuration loads. The export is
  OTLP/HTTP with a JSON body, as the metrics' is.
- `threshold` is the least duration of a probe or a scrape whose trace is
  exported, `1s` by default and at least `1ms`. A probe is timed from when
  it arrived to when its answer was ready, before it is written, as its
  duration in the self-metrics is, and its span ends then: a caller slow to
  read the answer makes no probe slow. A scrape is timed from its turn to its
  result.
- `errors: true` exports the trace of every probe and scrape that failed
  too, however long it took: one answered with an error, one answered with a
  [stale result](CONFIGURATION.md#serving-the-last-good-result-when-the-target-fails)
  in place of a fresh one, one the concurrency limit turned away, and a
  scrape that left its target `up` 0. Off, the default, only slow ones are
  exported, failed or not; a failure is in the log and the self-metrics
  already.
- `parent_based: true` exports the trace of every probe whose caller sent a
  `traceparent` with its sampled flag set, `01`, as the caller's tracer
  keeps its own part of the trace. Off, the default, the flag is not read.
- `sample_ratio` is the share, from `0` to `1`, of the other traces
  exported, decided by the trace id; `0`, the default, exports none of
  them, and `1` every one. A ratio outside `0` to `1` is refused when the
  configuration loads.
- `max_per_second` is the most traces exported a second, whatever kept
  them; `0`, the default, is no cap. It may not be negative.
- `interval` is how often the spans waiting are sent, `5s` by default and at
  least `1s`.
- `max_pending_spans` bounds the spans waiting, `10000` by default.
- `propagate: true` sends the trace context to the targets
  ([Propagation to the targets](#propagation-to-the-targets)); off, the
  default, the targets are sent none. It is the one key read with the trace
  export off, `enabled: false` here or in the `otlp` block: a probe's
  caller's trace context is then passed on.

What is a trace: each `/probe` request, with or without its caller's
`traceparent`, and each scheduled scrape of a static target. A probe that
shares an identical probe's trip ([shared](CONFIGURATION.md#identical-probes-share-one-request))
is a trace of its own whose `shared_trip` span is the wait, and under it is
a copy of the trip's spans, its stages and its requests, with span ids of
the probe's own: the trip's spans are in the trace of every probe that used
it, as they are, under the probe's span, in the trace of the probe that
started it. They keep the trip's times, so those of a probe that joined
the trip late begin before its `shared_trip` span does. The trip is no
probe's own: one whose caller goes away before the trip ends, the probe that
started it as much as one that joined it, has the status `ERROR`, the
`error.type` `abandoned` and the message `the caller went away before the
probe's trip ended`, or `… while the probe waited for an identical probe's
trip`, its `shared_trip` span `cancelled`; under it is what the trip had
done by then, a request still unanswered `ERROR` with `request.unfinished`
`true` and ending as the probe did. The stage it was in, not ended, has no
span. The trip goes on for the probes still waiting, whose traces have it
whole. A debug run, of the debug
page, a debug probe or a debug scrape, is never exported, as nothing of a
run is; the debug page shows the spans the export would make of it
([below](#seeing-a-runs-spans)).

The spans, by OpenTelemetry's conventions where one applies:

| Span | Kind | Parent | Attributes |
| --- | --- | --- | --- |
| `probe <collector>` | SERVER | the caller's span, when its `traceparent` names one | `http.request.method`, `url.path` (`/probe`), `http.response.status_code`, `collector`, `target` as the log shows it, `request.type`, `series` answered, `response.bytes` fetched, `cache.hit`, `error.type` when it failed |
| `scrape <static target>` | INTERNAL | none | `collector`, `target`, `static_target`, `request.type`, `series`, `response.bytes`, `cache.hit`, `error.type` (`scrape_failed`) when it failed |
| `cache` | INTERNAL | the probe or scrape | `stage.outcome`: `hit` or `miss`, for a collector with a `cache.ttl` |
| `slot` | INTERNAL | the probe or scrape | the wait for one of the collector's `max_concurrent_probes`: a scrape's wait, or a probe turned away |
| `shared_trip` | INTERNAL | the probe | the wait for an identical probe's trip, a copy of the trip's spans under it; `cancelled` when the probe's caller left first |
| `http`, `dns`, `sql`, … | INTERNAL | the probe or scrape | the fetch, named as the debug report names its stage; `stage.outcome`, `stage.note` |
| `GET`, `POST`, `SEARCH`, … | CLIENT | the fetch | one for each request sent: each attempt under `request.retry`, each redirect followed, each page of `request.pagination` and `request.login`'s login. `http.request.method`, `url.full`, `server.address`, `server.port`, `http.response.status_code`, `http.request.resend_count` for a retry or a redirect, `retry.attempt`, `pagination.page`, `redirect`, `login`, `error.type`; for dns, `dns.question.type` (the record type), `dns.answer.count` and `dns.response.not_found` when the name was not found; for sql, `db.response.returned_rows`, `sql.columns` and `sql.connection.reused`; for grpc, `rpc.grpc.status_code`, the code's number; for ntp, `ntp.stratum`, `ntp.offset_seconds` and `ntp.delay_seconds`; for ldap, `ldap.result_code` and, of a `SEARCH` page, `ldap.search.entries`; for tlscert, `tls.protocol.name`, `tls.protocol.version`, `tls.cipher`, `tls.server.not_after`, `tlscert.certificates`, `tlscert.verified` and `tlscert.leaf.days_until_expiry`; for redis, `redis.reply.type` (`integer`, `simple_string`, `bulk_string`, `array`, `nil` or `error`) and `redis.reply.bytes` or `redis.reply.elements`; for sshfile and s3file, `sshfile.files` and `sshfile.bytes`, `s3file.objects` and `s3file.bytes`, of the files read; for tcp, `tcp.greeting.bytes` and `tcp.greeting.matched` when it read a greeting and `tcp.answer.bytes`; for sshexec, `process.exit.code`, `sshexec.stdout.bytes` and `sshexec.stderr.bytes`. A localfile collector's reads are no request and have no span |
| `rate_limit` | INTERNAL | the fetch | the request's wait for its turn under `request.rate_limit` |
| `files`, `decode`, `transform`, `validation` | INTERNAL | the probe or scrape | the stages after the fetch, as the debug report names them |
| `python` | INTERNAL | `transform` | the time the collector's Python scripts took, added up, from the transform's start |
| `stale` | INTERNAL | the probe | the last good result answered in place of a failed trip |

A span that failed has the status `ERROR` and says why: the probe's or the
scrape's failure as the status page shows it, a stage's error as the log
line of its failure has it, and a request that got no answer, or an answer
its request type takes for a failure, or an HTTP answer of `400` or more, as
below. A span that did not fail has no
status, which is `UNSET`. Ids are random, from `crypto/rand`, and written in
hex, as OTLP/JSON writes them; times are nanoseconds since the epoch.

A probe whose caller sends a W3C `traceparent` header, as Prometheus does
not but a caller of its own may, continues the caller's trace: its span is
of the caller's trace id, with the caller's span as its parent, and its
spans carry the caller's `tracestate` header
([below](#which-traces-are-exported)). Its sampled flag keeps the trace
only under `parent_based`; the other rules decide it as any other. A
`traceparent` that cannot be read is no parent, and the `tracestate` beside
it is not read. Without [`propagate`](#propagation-to-the-targets), the
exporter sends no `traceparent` to its targets.

A trace is under the resource its series are exported under: a probe's
under the exporter-wide resource, a static target's under
[its own](#static-targets), its `otlp.service_name`,
`otlp.resource_attributes`, `otlp.labels_as_resource` and
`otlp.instance_id` included, which a target without `export_via_otlp` has
not; [`otlp.exporter_instance`](#the-exporter-instance) is on both. The
attributes are the spans', so `otlp.probe_attributes` is not used.

A span carries nothing the log does not, since a trace leaves the exporter
as a log line does. A request's `url.full` is its URL as an error in the log
quotes one, the userinfo withheld, every query value masked and no fragment,
and a probe's `target` is the target as the log shows it, the values of
query parameters named like credentials masked. No header, request body or
response body is in a span.

Nor is any text the target sent of a request, a stage or a probe that went
through, which the log says nothing of: no greeting of a tcp server, no
stderr of an sshexec command, no reason of an HTTP status line, no outcome
as a debug report tells it. Such a span has, besides the attributes above,
only the numbers, yes-or-no answers and names of the exporter's own
vocabulary that its request type works out of the answer, as the table
says: sizes, counts, an exit status, a result code, whether a greeting
matched, a TLS version and cipher by their registered names, a reply's
type. A gRPC status message, an LDAP diagnostic, a certificate's subject,
a column's name and a record's data are the target's text, and none of a
span's. A stage that went through has its `stage.note`, which the exporter
writes of counts, sizes, statuses, times and the directory read alone.

Of a failure, a span says what the log line of that failure says, and
nothing the log does not:

- a stage that failed has the error the log line of its failure has, the
  one the probe's `probe failed` line or the scrape's has, as its message,
  and the probe's or the scrape's span its failure as the status page shows
  it;
- the request the fetch failed on, the last it sent, has the fetch stage's
  error as its message, the same text;
- an HTTP answer of `400` or more has `received HTTP status <code>` as its
  message, as the log says it, never its status line's reason;
- a request that failed and was sent again, under `request.retry`, or that
  a request that went through followed, is in no line of the log: its span
  has the status `ERROR` and its `error.type`, `request_failed` or the
  status, and no message.

What it costs. While `traces` is off, a probe records nothing and allocates
nothing more for it. While it is on, every probe and scrape records its
stages and requests in memory, without their headers, as it goes — about a
dozen allocations and a kilobyte of a probe's own — and only one exported
has its spans
made and encoded: the decision is made at its end, when its duration is
known. The spans wait in a queue bounded by their count,
`max_pending_spans`: a trace that finds no room for all of its spans is
dropped whole and counted in `http_exporter_otlp_spans_dropped_total` rather
than waited for or cut, and a probe never waits for the export. An export
sends everything waiting, in requests of at most `otlp.batch_max_size` spans
and `otlp.batch_max_bytes` of JSON before compression, retried as the log
export's are ([above](#the-exporters-own-log)); what was not delivered waits
for the next export, before the traces queued since, the oldest traces
dropped whole past `max_pending_spans` and counted, and spans
the endpoint refuses, or rejects in a `partialSuccess` (its `rejectedSpans`),
are dropped and counted.

### Which traces are exported

A trace is decided once, at the end of its probe or scrape, by the rules
in this order; the first that keeps it says why, and one that none keeps is
dropped:

1. `slow`: it took `threshold` or longer;
2. `error`: it failed, under `errors: true`;
3. `parent`: its caller's `traceparent` has the sampled flag set, under
   `parent_based: true`; a probe without a `traceparent`, or with one that
   cannot be read, and a static target's scrape, have none;
4. `ratio`: its trace id is in the `sample_ratio` share.

The ratio is decided as OpenTelemetry's `TraceIdRatioBased` sampler and its
consistent probability sampling decide it, so that the exporter and every
tracer of the trace that samples at the same ratio keep the same traces: the
trace's randomness, the last 56 bits of its trace id, which W3C trace
context level 2 makes random, is compared with the rejection threshold
`(1 - sample_ratio) × 2^56`, rounded as OpenTelemetry rounds it, and the
trace is kept when its randomness is not under it. The same trace id is
decided alike every time. The exporter's own trace ids are from
`crypto/rand`; a caller's is used as it came, or, where the caller's
`tracestate` gives the trace an explicit randomness, `ot=rv:<14 hex
digits>`, that.

The spans of a trace the ratio kept carry its threshold, `th`, in the `ot`
member of their `tracestate`, OTLP's `traceState`, as OpenTelemetry's
probability sampling in `tracestate` writes it: 14 hex digits with the
trailing zeros left out, so `sample_ratio: 0.25` is `ot=th:c`, `0.5`
`ot=th:8`, `0.01` `ot=th:fd70a3d70a3d71`, and `1`, which keeps every trace,
`ot=th:0`. A backend counts by it how many traces each kept one stands for.
The caller's `tracestate` is kept behind it: the `ot` member, its other
fields such as `rv` kept and its `th` replaced, is put first, as the W3C
asks of a member changed, and the members past 32 are dropped from the
right. A trace its caller's flag kept carries the caller's `tracestate` as
it came, its `th` being the probability the caller kept it by. A trace kept
for being slow or failed carries the caller's `tracestate` without a `th`,
the `ot` member moved first if it lost one: no probability kept it, and a
backend counting by the threshold would count it too many times. A
`tracestate` that cannot be read — a member that is no `key=value` of the
W3C's characters, a key twice, more than 32 members — is not propagated.

With `parent_based` or `sample_ratio` on, the probe's or the scrape's span
says why its trace was kept, in `sampling.reason`: `slow`, `error`,
`parent` or `ratio`. With neither, a trace is kept for being slow or failed
alone, as its duration and status say, and its span has no
`sampling.reason`; a trace of a probe without a caller's `tracestate` then
has no `traceState` either, as before the rules.

`max_per_second` is held to after the rules, by every trace they kept,
slow and failed ones too: a token bucket of `max_per_second` tokens that
fills at `max_per_second` a second, so a burst of as many traces goes at
once, then as many a second. It starts full, and a reload that changes the
cap keeps its tokens, within the new one. A trace kept takes a token, and
one that finds none is dropped and counted as `rate`. So that the traces
the export is there for are not crowded out by sampled ones, which are many
at a high ratio, a trace kept by `parent` or `ratio` takes a token only
while half the bucket, rounded down, is left after it: a slow or failed
one may take the rest. A cap of `1` keeps nothing back, and at a steady
rate the sampled traces still get every token the bucket fills with above
that half. The bucket's lock is held for a few sums; a probe never waits
for it.

What became of the traces is counted, whole traces and not spans, in
`http_exporter_otlp_traces_kept_total{reason}` by the rule that kept them,
and in `http_exporter_otlp_traces_dropped_total{reason}`: `not_sampled` by
no rule, `rate` over `max_per_second`, and `queue` kept and with no room
under `max_pending_spans`, when queued or when a failed export put it back,
which a trace kept is counted in too. The spans of the traces are counted as
before, in `http_exporter_otlp_spans_dropped_total` and
`http_exporter_otlp_spans_sent_total`
([self-metrics](SELF-METRICS.md#otlp-export-status)). The status page shows
the rules in force and the same counts.

The trace export's own lines go to stderr alone, as the log export's do, and
say `this line is not exported`. Failing trace exports never make the
exporter unready. The export's state is in the
[self-metrics](SELF-METRICS.md#otlp-export-status), while it is on, and on
the [status page](ENDPOINTS.md#otlp-export). A reload that switches it off
drops the spans waiting, uncounted, and probes stop recording; a probe or a
scrape already under way when it did is not exported either, and counted
nowhere, so nothing of it is sent if a later reload switches the export on
again; one that
switches it on records the probes from then on; one that lowers
`max_pending_spans` below the spans waiting drops the oldest traces whole,
and counts them. The queue is never cut inside a trace, which would leave
spans whose parent is gone. At shutdown, the spans
waiting are sent after the metrics' [last export](#shutting-down), within
the same `otlp.timeout`, and before the log records'.

### Seeing a run's spans

The debug page's *OTLP* tab shows, with `otlp.traces` on, the spans the
export would make of a run: a tree of them, each with its start, its
duration, a bar of its time within the run's, its status and its
attributes, and the ExportTraceServiceRequest they would be sent in, in the
JSON viewer. Above them it says whether a probe or a scrape that took as
long and ended as the run did would be exported, and by which rule, or why
no rule would keep it: under `threshold`, failed or not under `errors`,
with no caller's `traceparent` under `parent_based` — a run has none — and,
under `sample_ratio`, the run's trace id's randomness against the
threshold, as in *would be exported: it was sampled at 1% by
otlp.traces.sample_ratio (trace id randomness 0xffd4… ≥ threshold
0xfd70a3d70a3d71)*. `max_per_second` is not taken: what it would leave
depends on the traces exported before, and the line says a trace kept may
still be dropped by it. The run itself is never exported. At `--web.debug=full`, *Test the
traces endpoint* sends `{"resourceSpans":[]}` to the traces endpoint, once,
as *Test the metrics endpoint* does.

## Linking metrics, logs and traces

With `otlp.traces` on, a probe's or a scrape's trace is linked to the rest
of what the exporter tells of it, and every link points at a trace that was
exported: a trace that is dropped, as nearly every one is, is linked to by
nothing. Without the trace export there are no trace ids, and nothing is
linked.

A trace is decided only at the end of its probe or scrape
([Which traces are exported](#which-traces-are-exported)), after its log
lines were logged, its trip observed and its requests sent. So its ids are
made when it begins: the trace id, the caller's where it sent a
`traceparent`, and the probe's or the scrape's own span id; the spans are
made with them if the trace is kept.

### Log records

With `otlp.logs` on too, a log record of a line logged during a probe or a
scrape — the exporter's own lines, such as `probe failed`, `probe stage
failed; continuing`, `metric extraction failed` or `static target scrape
failed` — carries, if the trace was exported, the trace's `traceId`, the
`spanId` of the span that was running when the line was logged, and
`flags` `1`, the sampled bit of the W3C trace flags. The span is told by
the line's time: the innermost stage span whose times hold it, else the
probe's or the scrape's own span. A stage that fails logs its failure
before it ends, so a failure's line is linked to the span of the stage that
failed, `http_status` for an HTTP answer of 500.

A record is linked when the log export takes it from its queue, every
`otlp.logs.interval`. A trace is decided when its probe ends, so a record
taken while its probe is still running, or of a probe that ended moments
before, as a probe slower than the interval may be, leaves without the ids,
and is not linked later: the log export does not wait for traces. A trace
counts as exported once its spans are queued for the trace export; one the
endpoint refuses later is linked to all the same.

The lines on stderr carry no trace or span id. They are written as they are
logged, before the trace is decided, and an id there would point, for
nearly every line, at a trace that was never exported; the link is on the
OTLP records alone.

### Exemplars

Each bucket of `http_exporter_probe_duration_seconds` has an exemplar: the
latest probe or static target scrape observed in it whose trace was
exported, with the trace id and the probe's or the scrape's own span id,
its duration and the time it was observed, as Prometheus' Go client keeps
the latest exemplar of each bucket. With verbose self-metrics
(`web.self_metrics.verbose`), so has each bucket of
`http_exporter_collector_scrape_duration_seconds`, of the latest trip
observed in it; a trip shared by several probes is of the trace of the
probe that started it. A bucket whose observations' traces were all
dropped has none.

The exemplars are written where a format carries them
([self-metrics](SELF-METRICS.md#exemplars)): in OpenMetrics, after the
bucket's count, `# {trace_id="…",span_id="…"} 0.067 1793646517.204`; in
protobuf, on the classic bucket, the `+Inf` one's on a `+Inf` bucket
written for it, and among the native histogram's exemplars; and over OTLP, on the histogram's and the exponential
histogram's points, as `exemplars` with `traceId`, `spanId`, `asDouble` and
`timeUnixNano`. The Prometheus text format has no exemplars. The series
that measure one probe, `http_exporter_scrape_duration_seconds` and a
static target's `http_exporter_target_scrape_duration_seconds`, are gauges,
which OpenMetrics gives no exemplars; a target's series have none.

### Propagation to the targets

With `propagate: true`, each HTTP request of a probe or a scrape, a
graphite one's and a kubernetes collector's to the API server too, and each
gRPC call, as its metadata, carries a
`traceparent` of the trace, with a span id of the request's own, which its
`CLIENT` span is then made with, so that the target's span is a child of it,
and a redirect followed has one of its own. A `tracestate` goes with it:
the caller's, as the spans carry it.

The sampled flag says what is known when the request is sent. A trace is
decided at its end, and only two rules can decide before: `parent_based`,
by the caller's sampled flag, and `sample_ratio`, by the trace id, which is
known from the start. Either keeps the trace whatever else becomes of it,
so the flag is `01` when one of them has, and `00` otherwise: the threshold
and `errors`, which keep most of what is exported, are known only at the
end, and the W3C asks for the flag only where the caller may have recorded
the trace. A target's tracer that samples by its parent then records its
part of the traces the exporter is sure to keep. The `tracestate` is as the
spans of a trace kept by that rule carry it: `ot=th` of the ratio's
threshold before the caller's members, or the caller's as it came when its
flag kept the trace, and otherwise the caller's without a `th`.
`max_per_second` may still drop a trace sent `01`, as it may drop one its
caller sampled.

With the trace export off, `propagate: true` passes a probe's caller's
`traceparent` and `tracestate` on as they came, and makes no ids: the
target's span is a child of the caller's. One that cannot be read is not
passed on, as the W3C asks; a scrape has no caller, and sends none.

A `traceparent` the request sets itself, in `request.headers`,
`request.metadata` or a forwarded header (`forward_headers`), is the
operator's and is sent as it is, with no `tracestate` of the exporter's. A
kubernetes collector's requests have no header of the operator's, and
always carry the exporter's.
A login's request (`request.login`) carries none: it goes to an identity
provider, often no part of the target, and the token it fetches serves
later probes, of other traces. Probes that share a trip
([shared](CONFIGURATION.md#identical-probes-share-one-request)) send the
trace context of the probe that started it.

### On the debug page

A run of the debug page with `otlp.traces` on has trace ids too. Its
requests carry the run's `traceparent` with `propagate: true`, and the
request lines show it. Its log records, in the *OTLP* tab, show the trace
and the span each would be linked to, those of the tab's spans, and say
that they would be linked if the trace were exported: a run's never is, so
the request body shows them without. A run's series are a target's, and
have no exemplars.
