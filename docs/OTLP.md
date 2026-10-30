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
  # logs:
  #   enabled: true
  #   level: info
```

`logs` forwards the exporter's own log too; see
[The exporter's own log](#the-exporters-own-log).

`enabled` is the switch, and a block that sets anything must say it: `true`
exports, `false` keeps the settings without using or checking them: a block
that is switched off may hold an endpoint that is no URL, a `compression`
that is neither `gzip` nor `none`, a negative duration or count, and the
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
  keeps points apart, not metrics. The kind written last is exported, with
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
targets are not scraped again for it. An export the shutdown interrupted is not
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
metrics are under, with the scope `prometheus-universal-exporter`. Its own
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
stderr's JSON nests it. The lines say what stderr says, credentials withheld
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
