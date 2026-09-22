# Exporter self-metrics

Exporter self-health metrics are available at `/self-metrics` by default (and `/metrics` remains a compatibility alias). Change the dedicated path with `--web.self-metrics-path=/exporter/metrics`. The Helm chart's optional self-metrics ServiceMonitor/PodMonitor scrapes the exporter pods/services separately from target-probing monitors. Configure one or more entries in `monitors`, each with a unique `name` and `type: pod` or `type: service`; each entry supports Prometheus Operator `relabelings` and `metricRelabelings`.

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
`go_sched_gomaxprocs_threads` and `go_gc_duration_seconds_count` / `_sum` series
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

`go_gc_duration_seconds` is published as its `_count` and `_sum` only. The
quantiles a summary would carry are not available from `runtime.MemStats`, and
inventing them would be worse than leaving them out.

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

Scheduled targets are recorded the same way, and because their requests are
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
http_exporter_target_scrape_duration_seconds_bucket{collector="app_json",le="0.005"} 0
http_exporter_target_scrape_duration_seconds_bucket{collector="app_json",le="0.01"} 3
...
http_exporter_target_scrape_duration_seconds_bucket{collector="app_json",le="+Inf"} 42
http_exporter_target_scrape_duration_seconds_sum{collector="app_json"} 0.61
http_exporter_target_scrape_duration_seconds_count{collector="app_json"} 42
```

`http_exporter_scrape_duration_seconds` only holds the last probe's duration, so
a slow scrape between two Prometheus scrapes is lost. The histogram keeps all of
them, which is what "this collector got slow" alerts need:

```promql
histogram_quantile(0.95,
  sum by (collector, le) (rate(http_exporter_target_scrape_duration_seconds_bucket[5m])))
  > 2
```

The buckets are fixed: 5 ms, 10 ms, 25 ms, 50 ms, 100 ms, 250 ms, 500 ms, 1 s,
2.5 s, 5 s, 10 s, 30 s and 60 s. Only trips to the target are observed: a probe
answered from the response cache, or by [sharing another probe's
request](#shared-probes), made no trip and is not counted, so the histogram
describes the target and the collector's processing, not how quickly the
exporter could answer. Scheduled targets are observed like probes. Every
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
four were idle after a burst) or `idle` (unused for five minutes). The last three
are routine; the first four each cost the next scrape a fresh interpreter.

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
