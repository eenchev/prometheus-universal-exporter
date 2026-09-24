# Exporter self-metrics

Exporter self-health metrics are served at `/self-metrics` by default, and at that one path only. Change it with `--web.self-metrics-path`, for example `--web.self-metrics-path=/metrics` for the conventional path; a path another endpoint uses, such as `/probe`, is refused at startup. The Helm chart's optional self-metrics ServiceMonitor/PodMonitor scrapes the exporter pods/services separately from target-probing monitors. Configure one or more entries in `monitors`, each with a unique `name` and `type: pod` or `type: service`; each entry supports Prometheus Operator `relabelings` and `metricRelabelings`.

## Collector metrics

Every collector has these series, labelled `collector`, from the moment it is
configured:

| Metric | Type | Meaning |
| --- | --- | --- |
| `http_exporter_scrapes_total` | counter | Probes served, cache hits included. |
| `http_exporter_scrape_success_total` | counter | Probes that completed without a fatal error. |
| `http_exporter_scrape_duration_seconds` | gauge | Duration of the most recent probe. |
| `http_exporter_scrape_http_status_code` | gauge | Status of the most recent response; `0` when none arrived, `200` after a file read or a gRPC call answered `OK`. |
| `http_exporter_scrape_grpc_status_code` | gauge | [`grpc`](GRPC.md#self-metrics) collectors only: the gRPC status code of the most recent call, `0` for `OK`, `14` for `UNAVAILABLE`; `-1` before the first call, or when a scrape made none, as when the message did not fit. An exporter without `grpc` collectors has no such series. |
| `http_exporter_scrape_response_bytes` | gauge | Size of the most recent response body. |
| `http_exporter_decode_success_total` | counter | Responses decoded. |
| `http_exporter_parse_errors_total` | counter | Responses the decoder could not parse. |
| `http_exporter_transform_errors_total` | counter | Failed transforms. |
| `http_exporter_missing_keys_total` | counter | Values the response did not contain: a jq or yq value, a CSV column, a regex that matched no text, an XPath or CSS selector that matched no nodes, `items` that selected nothing, a required label. Counted when a transform fails for it, and for each series a metric rule carried on without under `error_mode` `log` or `ignore`. |
| `http_exporter_script_errors_total` | counter | Python script failures. |
| `http_exporter_script_duration_seconds` | gauge | How long the Python of the most recent probe that ran any took — pre-script and python transform together, not counting starting an interpreter. |
| `http_exporter_metrics_emitted_total` | counter | Metrics produced, across scrapes. |
| `http_exporter_invalid_utf8_total` | counter | Label values and help texts that were not valid UTF-8, whose invalid bytes were replaced with `�`. See [Character encodings](CONFIGURATION.md#character-encodings). |
| `http_exporter_decoder_series_left_out_total` | counter | Series the [`graphite` decoder](GRAPHITE.md#the-series-document) left out before the metric rules saw them: with no point that has a value, older than `response.graphite.max_age`, or answered twice. `0` for other decoders. |
| `http_exporter_decoder_lines_skipped_total` | counter | Carbon lines the `graphite` decoder could not read and skipped, under [`response.graphite.invalid_lines: skip`](GRAPHITE.md#carbon-lines-from-a-file). |
| `http_exporter_series_limit_exceeded_total` | counter | Scrapes rejected by a size or series limit. |
| `http_exporter_cache_hits_total`, `http_exporter_cache_misses_total` | counter | [Response cache](CONFIGURATION.md#response-caching) lookups: a probe answered from the cache is a hit, including one that found it filled while it waited to start; a probe that went to the target is a miss. A probe that [shared](#shared-probes) another's trip is neither, and is counted in `http_exporter_probes_coalesced_total`. |
| `http_exporter_cache_entries` | gauge | Entries the collector's cache holds, stale ones kept for `stale_if_error` included. |
| `http_exporter_cache_stale_served_total` | counter | Failed trips answered with the last good result under [`stale_if_error`](CONFIGURATION.md#serving-the-last-good-result-when-the-target-fails), probes and static target scrapes alike. |
| `http_exporter_probes_coalesced_total` | counter | Probes that [shared a request](#shared-probes). |
| `http_exporter_probes_in_flight` | gauge | Trips to the collector's targets in progress, which [`max_concurrent_probes`](CONFIGURATION.md#limiting-concurrent-probes) bounds. |
| `http_exporter_probes_rejected_total` | counter | Probes answered `503` because the collector was at `max_concurrent_probes`. |
| `http_exporter_collector_config_valid` | gauge | `1` for every loaded collector. |
| `http_exporter_rule_failures_total` | counter | Labelled `collector` and `metric`: the series a metric rule could not produce and the probe carried on without, under `error_mode` `log` or `ignore`. Every rule has its series from zero. A rule under `fail` fails the probe instead, counted in `http_exporter_transform_errors_total`. |

A failure rate, for example:

```promql
1 - rate(http_exporter_scrape_success_total[5m]) / rate(http_exporter_scrapes_total[5m])
```

Which of the error counters a failure raises depends on what failed, not on
the words in its message: a script error that happens to say "missing" is a
script error, not a missing key.

A collector that a reload removes stops being reported, here and over OTLP, so
Prometheus marks its series stale; one added again later under the same name
starts from zero. A collector a reload changes keeps its counters, but its
cached results are dropped, since they belong to the old definition.

Every counter ends in `_total` and nothing else does. The self-metrics path
and OTLP are built from the same definitions, so a family has the same type,
help and value in both.

## Build information

```text
http_exporter_build_info{goversion="go1.25.1",request_types="graphite,grpc,http,localfile",revision="4c1f2e9…",version="v1.4.0"} 1
```

As every Prometheus exporter does, one series with value `1` carries the
build as labels: the version, the git revision (`-modified` when built from a
changed checkout, `unknown` when not built from git), the Go version and the
[request types](CONFIGURATION.md#request-types) built in. `--version` prints the
same. The version is the one a release build sets with
`-ldflags "-X main.version=1.4.0"`, otherwise the module version Go stamps,
`(devel)` for a local build. Join it onto other series to see which build
produced them:

```promql
up * on (instance) group_left (version) http_exporter_build_info
```

## Static targets

| Family | Type | Meaning |
| --- | --- | --- |
| `http_exporter_static_targets` | gauge | [Static targets](STATIC-TARGETS.md) loaded from the static target file; 0 without one. |
| `http_exporter_static_targets_exported_via_otlp` | gauge | Those of them with `export_via_otlp`, also delivered over OTLP. |

A static target's scrapes are counted in its collector's families above, as
probes are. Each target's own health, `http_exporter_target_up` and
`http_exporter_target_scrape_duration_seconds`, is served with its results on
the [static targets endpoint](STATIC-TARGETS.md#the-static-targets-endpoint),
not here.

## Configuration reloads

A reload that is rejected — an invalid file, a duplicate collector name, a
pre-script that stops producing `data` — leaves the last valid configuration
running and logs why, once. These series keep saying so, under the names
Prometheus uses for its own configuration:

```text
http_exporter_config_last_reload_successful{file="config"} 0
http_exporter_config_last_reload_success_timestamp_seconds{file="config"} 1.7901e+09
http_exporter_config_reloads_total{file="config",result="success"} 4
http_exporter_config_reloads_total{file="config",result="failure"} 1
```

`file="config"` is the configuration with its
[collector files](CONFIGURATION.md#collector-files); `file="static_targets"` is the
[static target file](STATIC-TARGETS.md), reported only when there is
one. Loading at startup counts as a success; the counter counts reloads after
it, whatever triggered them: the watch, `SIGHUP` or
[`POST /-/reload`](CONFIGURATION.md#reloading-on-demand). Alert on a change that did not take:

```promql
http_exporter_config_last_reload_successful == 0
```

## OTLP export status

With [OTLP export](OTLP.md) enabled, these say how it is going. They are
exported over OTLP too, so the backend hears about failed exports from the
next one that gets through, and are scraped from the self-metrics path whatever
happens to OTLP:

| Metric | Type | Meaning |
| --- | --- | --- |
| `http_exporter_otlp_exports_total{result}` | counter | Exports, by `result`: `success` or `failure`. An export is one delivery of everything pending; a retried export that got through is one success. |
| `http_exporter_otlp_export_retries_total` | counter | Attempts repeated after a network error, `429`, `502`, `503` or `504`. |
| `http_exporter_otlp_points_dropped_total` | counter | Data points given up on: refused by the endpoint with an answer that is not retried, or the oldest waiting past `otlp.max_pending_points`. Data points of an export that ran out of retries are kept for the next and not counted until then. |
| `http_exporter_otlp_export_duration_seconds` | gauge | Duration of the most recent export, retries included. |
| `http_exporter_otlp_last_export_success_timestamp_seconds` | gauge | Unix time of the last export that got through; `0` before the first. |

```promql
rate(http_exporter_otlp_exports_total{result="failure"}[15m]) > 0
```

## Shared probes

`http_exporter_probes_coalesced_total{collector="..."}` counts the probes that
were answered by sharing an identical probe already in flight, instead of going
to the target themselves (see
[Identical probes share one request](CONFIGURATION.md#identical-probes-share-one-request)).
With several Prometheus replicas it is normal for it to be roughly
`(replicas - 1) / replicas` of `http_exporter_scrapes_total`; the trip to the
target itself — its status, bytes, decode and transform counters — is counted
once.

## Resource metrics

The exporter can publish the familiar `go_` and `process_` series describing its
own CPU and memory, under the names every Go dashboard already uses:

```yaml
web:
  self_metrics:
    resource_metrics_enabled: true
```

```text
go_goroutines 7
go_threads 8
go_info{version="go1.27.0"} 1
go_memstats_heap_inuse_bytes 1.589248e+06
go_memstats_alloc_bytes_total 767904
go_gc_duration_seconds_sum 0.0021
go_gc_duration_seconds_count 3
go_cpu_classes_user_cpu_seconds_total 0.27687507
process_cpu_seconds_total 0.31
process_resident_memory_bytes 1.3389824e+07
process_open_fds 8
```

The names and meanings match what `prometheus/client_golang` publishes, so an
existing dashboard or alert works unchanged — that is the entire point of the
prefix. The numbers come from the standard library rather than from a client
library: this exporter renders its own exposition and keeps no registry, so
pulling one in to gather these would add a dependency and an adapter for
nothing.

It is off by default because reading them is not free. `runtime.ReadMemStats`
briefly stops the world, and an exporter scraped every few seconds by several
Prometheus servers should not pay that unless somebody wants the numbers.

### What you get

The `go_memstats_*`, `go_goroutines`, `go_threads`, `go_info`,
`go_sched_gomaxprocs_threads` and `go_gc_duration_seconds` series
come from the runtime and are published on every platform the exporter builds
for.

The `go_cpu_classes_*` counters come from `runtime/metrics`, asked for by name.
A Go release that renames or drops one makes that series disappear rather than
publish a zero, so what you see is what the runtime the binary was built with
actually offers.

The `process_*` series are the operating system's view, read from `/proc`. They
appear on Linux — where the container runs — and are simply absent elsewhere.
They are deliberately not synthesised from runtime numbers on other platforms:
`process_cpu_seconds_total` is real CPU time the process consumed, while
`go_cpu_classes_total_cpu_seconds_total` is GOMAXPROCS multiplied by wall time
and includes idle. Publishing one under the other's name would be wrong in a way
that only shows up when somebody trusts the graph.

`go_gc_duration_seconds` is a summary, as in client_golang, published with its
`_count` and `_sum` only. The quantiles a summary would carry are not available
from `runtime.MemStats`, and inventing them would be worse than leaving them
out.

Like verbosity, this is configuration rather than a flag, so a reload turns it
on and off.

## Verbose per-request self-metrics

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

Static targets are recorded the same way, and because their requests are
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
URL than the one fetched — with one deliberate exception: a
[path parameter](REQUESTS.md#path-parameters) appears as its placeholder,
`/api/{{param_tenant}}/status`, rather than its value, for the same reason the
query string is dropped, and because one series per tenant would be unbounded.

A [`localfile`](LOCALFILE.md#errors-and-self-metrics) collector's reads carry
the file's `file://` URL, placeholders kept the same way, and
`http_method="READ"`.

A [`grpc`](GRPC.md#self-metrics) collector's calls carry
`grpc://host:port/package.Service/Method`, `grpcs://` over TLS, and
`http_method="POST"`, which is what gRPC sends over HTTP/2; the message and
metadata are left out, as a query string is.

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

### Scrape-time histograms

Verbose mode also publishes, per collector, a histogram of how long each trip to
the target took, from sending the request to having validated metrics:

```text
http_exporter_collector_scrape_duration_seconds_bucket{collector="app_json",le="0.005"} 0
http_exporter_collector_scrape_duration_seconds_bucket{collector="app_json",le="0.01"} 3
...
http_exporter_collector_scrape_duration_seconds_bucket{collector="app_json",le="+Inf"} 42
http_exporter_collector_scrape_duration_seconds_sum{collector="app_json"} 0.61
http_exporter_collector_scrape_duration_seconds_count{collector="app_json"} 42
```

`http_exporter_scrape_duration_seconds` only holds the last probe's duration, so
a slow scrape between two Prometheus scrapes is lost. The histogram keeps all of
them, which is what "this collector got slow" alerts need:

```promql
histogram_quantile(0.95,
  sum by (collector, le) (rate(http_exporter_collector_scrape_duration_seconds_bucket[5m])))
  > 2
```

The buckets are fixed: 5 ms, 10 ms, 25 ms, 50 ms, 100 ms, 250 ms, 500 ms, 1 s,
2.5 s, 5 s, 10 s, 30 s and 60 s. Only trips to the target are observed: a probe
answered from the response cache, or by [sharing another probe's
request](#shared-probes), made no trip and is not counted, so the histogram
describes the target and the collector's processing, not how quickly the
exporter could answer. Static targets are observed like probes. Every
configured collector has a histogram, empty until its first trip, and the
durations are only recorded while verbose mode is on.

### Python workers

For every collector with a Python transform or pre-script, verbose mode publishes
the state of its [Python workers](PYTHON.md#how-scripts-run):

| Metric | Labels | Meaning |
|---|---|---|
| `http_exporter_python_workers` | `state`: `starting`, `idle`, `busy` | Workers of the collector now in each state. |
| `http_exporter_python_worker_starts_total` | | Workers started. |
| `http_exporter_python_worker_start_failures_total` | | Workers that failed to start: Python missing, a library that does not import. |
| `http_exporter_python_worker_stops_total` | `reason` | Workers stopped, and why (below). |
| `http_exporter_python_runs_total` | `outcome` | Script runs, and how they ended (below). |

A worker stops because of a `timeout` (the script overran `limits.script_timeout`
and the worker was killed), a `crash` (the interpreter died), an `output_limit`
(it answered with more than `limits.max_output_bytes`), `cancelled` (the scrape
was abandoned mid-run), `retired` (it reached 1,000 runs), `surplus` (more than
four were idle after a burst), `idle` (unused for five minutes) or `reload` (a
reload changed or removed its script). The last four are routine; the first four
each cost the next scrape a fresh interpreter.

A run ends `ok`, `script_error` (the script raised or called `fail(...)`; the
worker carries on), `timeout`, `output_limit` or `failed` (the worker could not
be reached or its answer was unreadable).

```promql
# a collector whose script keeps timing out
increase(http_exporter_python_runs_total{outcome="timeout"}[15m]) > 0
# workers that cannot start at all
increase(http_exporter_python_worker_start_failures_total[5m]) > 0
```

Every label is from a fixed set, and a collector without Python has none of
these series. The counters are kept whether or not verbose mode is on, so
turning it on through a reload shows the counts since the exporter started.

#### The Python execution pool

The same five families are also published for the pool as a whole, with a
`python_pool` name and no `collector` label:

```text
http_exporter_python_pool_workers{state="starting"} 0
http_exporter_python_pool_workers{state="idle"} 3
http_exporter_python_pool_workers{state="busy"} 1
http_exporter_python_pool_worker_starts_total 12
http_exporter_python_pool_worker_start_failures_total 0
http_exporter_python_pool_worker_stops_total{reason="timeout"} 2
...
http_exporter_python_pool_runs_total{outcome="ok"} 4381
...
```

These are always there in verbose mode, even when no collector uses Python, so
a dashboard or alert on the pool works on every exporter without knowing which
collectors run scripts; on an exporter without Python collectors they read zero.
They sum every collector the pool has served, including collectors a reload has
since removed, so the counters never go backwards. They have their own names,
rather than being an unlabelled series of the per-collector families, so
`sum(http_exporter_python_runs_total)` never counts a run twice.

```promql
# workers are not being reused: more than one start per ten runs
rate(http_exporter_python_pool_worker_starts_total[15m])
  / sum(rate(http_exporter_python_pool_runs_total[15m])) > 0.1
# Python cannot start at all
increase(http_exporter_python_pool_worker_start_failures_total[5m]) > 0
```
