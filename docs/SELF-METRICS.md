# Exporter self-metrics

Exporter self-health metrics are served at `/self-metrics` by default, and at that one path only. Change it with `--web.self-metrics-path`, for example `--web.self-metrics-path=/metrics` for the conventional path; a path another endpoint uses, such as `/probe`, is refused at startup. Like `/probe`, it answers `GET` and `HEAD`, and any other method `405` with `Allow: GET, HEAD`; and like `/probe`, it answers in OpenMetrics when the scraper asks for it and in the text format otherwise (see [OpenMetrics](CONFIGURATION.md#openmetrics)). The Helm chart's self-metrics monitor, `<fullname>-self`, scrapes the exporter itself, apart from the target-probing monitors of `monitors`. It is configured by the `selfMetrics` block: `enabled`, `type: service` (a ServiceMonitor) or `type: pod` (a PodMonitor), `path`, `interval`, `scrapeTimeout`, `labels`, `annotations`, and Prometheus Operator `relabelings` and `metricRelabelings` (see the [chart README](../charts/prometheus-universal-exporter/README.md#4-configure-prometheus)).

## Collector metrics

Every collector has these series, labelled `collector`, from the moment it is
configured:

| Metric | Type | Meaning |
| --- | --- | --- |
| `http_exporter_scrapes_total` | counter | Probes served, cache hits included. |
| `http_exporter_scrape_success_total` | counter | Probes that completed without a fatal error. |
| `http_exporter_scrape_duration_seconds` | gauge | Duration of the most recent probe. |
| `http_exporter_scrape_http_status_code` | gauge | Status of the most recent response; `0` when none arrived, `200` after a file read, or a gRPC call answered `OK` or with a code of its `accept_codes`. |
| `http_exporter_scrape_grpc_status_code` | gauge | [`grpc`](GRPC.md#self-metrics) collectors only: the gRPC status code of the most recent call, `0` for `OK`, `14` for `UNAVAILABLE`; `-1` before the first call, or when a scrape made none, as when the message did not fit. An exporter without `grpc` collectors has no such series. |
| `http_exporter_scrape_response_bytes` | gauge | Size of the most recent response body. |
| `http_exporter_decode_success_total` | counter | Responses decoded. |
| `http_exporter_parse_errors_total` | counter | Responses the decoder could not parse. |
| `http_exporter_transform_errors_total` | counter | Failed transforms. A transform cut off because every probe waiting for its trip went away, or by a shutdown, is not one. |
| `http_exporter_missing_keys_total` | counter | Values the response did not contain: a jq or yq value, a CSV column, a regex that matched no text, an XPath or CSS selector that matched no nodes, `items` that selected nothing, a required label. Counted when a transform fails for it, and for each series a metric rule carried on without under `error_mode` `log` or `ignore`. |
| `http_exporter_script_errors_total` | counter | Python script failures. |
| `http_exporter_script_duration_seconds` | gauge | How long the Python of the most recent probe that ran any took — pre-script and python transform together, as `limits.script_timeout` measures each: the script's own run, not counting starting an interpreter or handing it the response. |
| `http_exporter_metrics_emitted_total` | counter | Metrics produced, across scrapes. |
| `http_exporter_invalid_utf8_total` | counter | Label values and help texts that were not valid UTF-8, whose invalid bytes were replaced with `�`. See [Character encodings](CONFIGURATION.md#character-encodings). |
| `http_exporter_decoder_series_left_out_total` | counter | Series the [`graphite` decoder](GRAPHITE.md#the-series-document) left out before the metric rules saw them: with no point that has a value, older than `response.graphite.max_age`, or answered twice. `0` for other decoders. |
| `http_exporter_decoder_lines_skipped_total` | counter | Lines a decoder left out and read on: carbon lines the `graphite` decoder could not read, under [`response.graphite.invalid_lines: skip`](GRAPHITE.md#carbon-lines-from-a-file), and sample lines the `prometheus` decoder found to be [no part of their histogram or summary family](CONFIGURATION.md#collectors), such as Micrometer's `x{quantile="0.95"}` under `# TYPE x histogram`. |
| `http_exporter_series_limit_exceeded_total` | counter | Scrapes rejected by a size or series limit. |
| `http_exporter_cache_hits_total`, `http_exporter_cache_misses_total` | counter | [Response cache](CONFIGURATION.md#response-caching) lookups: a probe answered from the cache is a hit, including one that found it filled while it waited to start; a probe that went to the target is a miss. A probe that [shared](#shared-probes) another's trip is neither, and is counted in `http_exporter_probes_coalesced_total`; nor is one that [`max_concurrent_probes`](CONFIGURATION.md#limiting-concurrent-probes) or `--probe.max-concurrent` turned away, which is counted only as rejected, in `http_exporter_probes_rejected_total` or `http_exporter_probes_rejected_exporter_limit_total`. |
| `http_exporter_cache_entries` | gauge | Entries the collector's cache holds, stale ones kept for `stale_if_error` included. |
| `http_exporter_cache_stale_served_total` | counter | Failed trips answered with the last good result under [`stale_if_error`](CONFIGURATION.md#serving-the-last-good-result-when-the-target-fails), probes and static target scrapes alike. |
| `http_exporter_probes_coalesced_total` | counter | Probes that [shared a request](#shared-probes). |
| `http_exporter_probes_in_flight` | gauge | Trips to the collector's targets in progress, which [`max_concurrent_probes`](CONFIGURATION.md#limiting-concurrent-probes) bounds. |
| `http_exporter_probes_rejected_total` | counter | Probes answered `503`, and static target scrapes that found no slot in time, because the collector was at [`max_concurrent_probes`](CONFIGURATION.md#limiting-concurrent-probes). |
| `http_exporter_probes_rejected_exporter_limit_total` | counter | The same, because the exporter as a whole was at [`--probe.max-concurrent`](CONFIGURATION.md#limiting-concurrent-probes): the collector had room, the process did not. |
| `http_exporter_targets_refused_total` | counter | `http`, `graphite` and `grpc` collectors only: probes and static target scrapes whose target, or a redirect's, [`allowed_targets` or `denied_targets`](REQUESTS.md#restricting-targets) refused; a probe is answered `403`. A `localfile` collector, with no target to refuse, has no series. |
| `http_exporter_collector_config_valid` | gauge | `1` for every loaded collector. A rejected reload keeps the previous collectors at `1`; it shows in `http_exporter_config_last_reload_successful`. |
| `http_exporter_trips_in_flight` | gauge | Without a `collector` label: trips to targets in progress, probes and static target scrapes of every collector together, which `--probe.max-concurrent` bounds. A fair signal for scaling out on. |
| `http_exporter_trips_max_concurrent` | gauge | Without a `collector` label: `--probe.max-concurrent`, `0` for no limit. |
| `http_exporter_rule_failures_total` | counter | Labelled `collector` and `metric`: the series a metric rule could not produce and the probe carried on without, under `error_mode` `log` or `ignore`. Every rule with a name has its series from zero, a [`python` rule](PYTHON.md#what-a-rule-of-a-python-collector-is-for) too, whose series stays `0` since it makes no series to fail; a `prometheus` rule without a name has none: no series is exported with an empty `metric`, and what such a rule did not find moves only `http_exporter_missing_keys_total`. Rules that export one metric name share its series, which counts the failures of all of them, while the [log](LOGGING.md) tells them apart. A rule under `fail` fails the probe instead, counted in `http_exporter_transform_errors_total`. |

A failure rate, for example:

```promql
1 - rate(http_exporter_scrape_success_total[5m]) / rate(http_exporter_scrapes_total[5m])
```

Which of the error counters a failure raises depends on what failed, not on
the words in its message: a script error that happens to say "missing" is a
script error, not a missing key.

A collector that a reload removes stops being reported, here and over OTLP, so
Prometheus marks its series stale; one added again later under the same name
starts from zero, and a probe or scrape of the removed collector that began
before the reload, however far it had come by then, is not counted for it.
The series of its [Python workers](#python-workers) are held to the same. A
collector a reload changes keeps its counters, but its cached results are
dropped, since they belong to the old definition. A probe or scrape that
began before the reload caches nothing under the name of a collector the
reload removed or changed, so `http_exporter_cache_entries` counts only the
results of the collector as it is.

Every counter ends in `_total` and nothing else does. The self-metrics path
and OTLP are built from the same definitions, so a family has the same type,
help and value in both.

## Build information

```text
http_exporter_build_info{goversion="go1.25.1",request_types="graphite,grpc,http,localfile",revision="4c1f2e9…",version="v1.4.0"} 1
```

As every Prometheus exporter does, one series with value `1` carries the
build as labels: the version, the git revision (`-modified` when built from a
changed checkout; for a build without git, such as the image's, the one
`-ldflags "-X main.revision=<commit>"` sets, which the image's `REVISION`
build argument passes and the release workflow sets to the tag's commit;
`unknown` otherwise), the Go version and the
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
[`POST /-/reload`](CONFIGURATION.md#reloading-on-demand). With the watch on,
a [descriptor file](CONFIGURATION.md#descriptor-files) of a grpc collector
that changes is a reload too, although no configuration file did: one that
no longer has the collector's method, or no longer takes its message, turns
the series to `0` at the next tick. Alert on a change that did not take:

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
| `http_exporter_otlp_exports_total{result}` | counter | Exports, by `result`: `success` or `failure`. An export is one delivery of everything pending, however many requests it is sent in (`otlp.batch_max_size`, `otlp.batch_max_bytes`); a retried export that got through is one success, and one of which the endpoint refused a request is a failure. |
| `http_exporter_otlp_export_retries_total` | counter | Attempts repeated after a network error, `429`, `502`, `503` or `504`. |
| `http_exporter_otlp_points_dropped_total` | counter | Data points given up on: refused by the endpoint with an answer that is not retried, rejected by an export the endpoint accepted (its `partialSuccess`), the oldest waiting past `otlp.max_pending_points`, or those of the last export before exiting when it failed. Data points of an earlier export that ran out of retries are kept for the next and not counted until then. |
| `http_exporter_otlp_export_duration_seconds` | gauge | Duration of the most recent export, all its requests and retries included. |
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
once. It belongs to none of the probes sharing it: in the
[verbose per-request series](#verbose-per-request-self-metrics) it is counted
on the request whichever probe started it, also when that probe's caller went
away and the trip answered the others.

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

The two views are raised through the same path, so a counter is never raised
on one and not on the other; a test checks that. A collector's total is the
sum of its tracked requests' and of what started no tracked request: a probe
the collector's `allowed_targets` or `denied_targets` refused, a probe whose
caller went away before it was answered, a trip to the target cancelled
because every probe waiting for it had gone, and a probe past the limit
below. A request that has since expired takes its share with it too.

A trip that [identical probes share](#shared-probes) is counted on the request
when it ends, not with the probe that happened to start it. When that probe's
caller goes away and the trip goes on to answer the others, everything the
trip counted — the cache miss, the target's status and bytes, the decode,
transform and error counters, the series emitted, the time of the scrape — is
on the request those probes are counted on, once. The request's counters then
differ from the collector's only by the probe that left, in
`http_exporter_scrapes_total`. A request already tracked when a probe arrives
counts that probe, and its trip, whatever becomes of them: only a request not
tracked yet waits for a trip to end with an answer.

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
combinations are refused. A probe refused by the collector's `allowed_targets`
or `denied_targets` does not take a slot, so probing forbidden targets cannot
crowd out legitimate ones; nor does a probe whose caller went away before it
was answered, or a trip cancelled because every probe waiting for it had gone:
neither got the verdict. A combination nothing has probed or scraped for an
hour is dropped and its slot freed; the configured static targets' stay, and
leave when a reload removes the target or changes its URL. The truncation is
visible rather than silent:

```text
http_exporter_request_series_capped 1
```

It reads `0` normally, so you can alert on `== 1` without testing for an absent
series, and `http_exporter_request_series_tracked` reports how many combinations
are in use against that limit. Because verbosity is configuration rather than a
flag, a reload turns it on and off; turning it off drops the labelled series
instead of leaving stale ones exposed.

### Queues

Verbose mode also publishes how much is waiting for a slot, without a
`collector` label:

| Metric | Meaning |
|---|---|
| `http_exporter_trips_waiting` | Static target scrapes waiting for a trip slot, because their collector is at `max_concurrent_probes` or the exporter at `--probe.max-concurrent`. A probe never waits: at either limit it is answered `503` at once and counted in `http_exporter_probes_rejected_total`. |
| `http_exporter_python_pool_runs_waiting` | Script runs waiting for a Python worker under `--python.max-workers`; `0` without the limit. |

Every collector has a `max_concurrent_probes`, 32 unless it sets another, so
static target scrapes can wait even without `--probe.max-concurrent`. A queue
that stays above zero says a limit is lower than the load, before scrapes
start being skipped for it:

```promql
min_over_time(http_exporter_trips_waiting[10m]) > 0
```

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
| `http_exporter_python_worker_start_failures_total` | | Workers that failed to start: Python missing, a library that does not import, an interpreter not ready within its ten seconds. |
| `http_exporter_python_worker_stops_total` | `reason` | Workers stopped, and why (below). |
| `http_exporter_python_runs_total` | `outcome` | Script runs, and how they ended (below). |

A worker stops because of a `timeout` (the script overran `limits.script_timeout`
and the worker was killed), a `deadline` (the probe's or scrape's deadline
ended the script before `limits.script_timeout` did, and the worker was
killed), a `crash` (the interpreter died, during a run or while it sat idle),
an `output_limit` (it answered with more than `limits.max_output_bytes`),
`cancelled` (the scrape was abandoned mid-run), `retired` (it reached 1,000
runs), `surplus` (more than four were idle after a burst), `idle` (unused for
five minutes), `reload` (a reload changed or removed its script, or removed
its collector) or `evicted`
(it was idle when another script needed a worker under
`--python.max-workers`). `retired`, `surplus`, `idle` and `reload` are routine;
the first five each cost the next scrape a fresh interpreter, and many
`evicted` say the limit is too low for the scripts in use.

A run ends `ok`, `script_error` (the script raised or called `fail(...)`,
with a message of any length, or
left `data` or `metrics` the worker does not write, such as a list that holds
itself or strings and keys that are longer together than
`limits.max_output_bytes`; the worker carries on), `timeout` (it overran `limits.script_timeout`), `deadline`
(the probe's or scrape's deadline ended it first, or ran out while the
response was handed to the worker: the time to raise is the probe's, not
`script_timeout`), `output_limit` (it wrote an answer longer than
`limits.max_output_bytes`, and the worker was stopped) or `failed` (the worker
could not be reached or its answer was unreadable). A worker found dead when it was taken from the
pool is replaced before the run, and is no failed run.

```promql
# a collector whose script keeps timing out
increase(http_exporter_python_runs_total{outcome="timeout"}[15m]) > 0
# workers that cannot start at all
increase(http_exporter_python_worker_start_failures_total[5m]) > 0
```

Every label is from a fixed set, and a collector without Python has none of
these series. The counters are kept whether or not verbose mode is on, so
turning it on through a reload shows what was counted before it was on.
They count what the workers did, whoever asked: the scripts of a
[debug probe](CONFIGURATION.md#debugging-a-probe), which is counted in no
other self-metric, run in the collector's workers and are counted here.

They are the collector's, and follow a reload as its
[other counters](#collector-metrics) do. A reload that removes the collector
drops them: the series are gone, here and over OTLP, and a collector added
again under the name starts every one of them from zero, with workers of its
own. A script of the removed collector that is still under way then —
waiting for a worker, running, or ending — and the stop of its worker, for
the `reload` or for anything else, are counted under no collector's name,
whether or not the collector is back by then: they are counted for
[the pool](#the-python-execution-pool) alone. A worker is the collector's
that started it for as long as it lives. One of the removed collector is
stopped for the `reload`, at once when it is idle and when its script ends
when it is busy, also where the probe began before the reload and its script
after it, and is never handed to the collector added again, though the
script is the same; a script of the removed collector that is still to run
starts a worker of its own rather than take one of the collector that is
back. So the workers a collector has started, less those stopped under its
name, are always the ones its `idle` and `busy` series show. A collector
whose definition a reload changed keeps its counters, the `reload` stop of
the worker of a script that changed among them, and an unchanged collector
keeps everything, its idle workers too.

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
since removed and what the scripts and workers of such a collector did after
the reload, so the counters never go backwards. They have their own names,
rather than being an unlabelled series of the per-collector families, so
`sum(http_exporter_python_runs_total)` never counts a run twice.

```promql
# workers are not being reused: more than one start per ten runs
rate(http_exporter_python_pool_worker_starts_total[15m])
  / sum(rate(http_exporter_python_pool_runs_total[15m])) > 0.1
# Python cannot start at all
increase(http_exporter_python_pool_worker_start_failures_total[5m]) > 0
```

## Created timestamps

A counter says how much, not since when: after a restart, or when a collector
is removed and added again, a counter starts from zero, and a scraper that
sees `5` and then `3` knows it started again, while one that sees `5` and then
`7` cannot tell a counter that grew by 2 from one that started again and
counted 7. OpenMetrics has a sample for that, `_created`: the Unix time since
which a counter, a histogram or a summary series has been counting. The
exporter knows it of its own series, and writes it when asked to:

```yaml
web:
  self_metrics:
    created_timestamps: true
```

```text
# TYPE http_exporter_scrapes counter
http_exporter_scrapes_total{collector="app_json"} 2
http_exporter_scrapes_created{collector="app_json"} 1759491000.12
# TYPE http_exporter_collector_scrape_duration_seconds histogram
http_exporter_collector_scrape_duration_seconds_bucket{collector="app_json",le="0.005"} 0
...
http_exporter_collector_scrape_duration_seconds_sum{collector="app_json"} 0.25
http_exporter_collector_scrape_duration_seconds_count{collector="app_json"} 2
http_exporter_collector_scrape_duration_seconds_created{collector="app_json"} 1759491000.12
```

The sample follows its series, in the series' family and with its labels, and
only in an [OpenMetrics](CONFIGURATION.md#openmetrics) answer of the
self-metrics endpoint: the text format has no such sample and is unchanged,
and so are the answers of `/probe` and the static targets endpoint, whose
series come from targets that do not say when they started. Like the other
self-metrics settings it is configuration, so a reload turns it on and off.

It is off by default, as it is in Prometheus' own Go client
(`EnableOpenMetricsTextCreatedSamples`), because of what Prometheus does with
the samples:

- Without its created-timestamp feature, Prometheus stores every `_created`
  line as a series of its own, `http_exporter_scrapes_created{...}` with the
  time as its value: one more series for each counter, histogram and summary
  series, which with [verbose](#verbose-per-request-self-metrics) self-metrics
  is some twenty per tracked request.
- With `--enable-feature=created-timestamp-zero-ingestion`, a Prometheus that
  reads created timestamps from the OpenMetrics text format takes the time as
  the start of the series instead: it writes a sample of `0` at that time
  before the series' first value, so `rate()` and `increase()` count a new
  series from zero and see one that started again, and stores no `_created`
  series. Check that your Prometheus version does this for OpenMetrics text
  before turning the setting on for it; early versions of the feature read
  created timestamps only from the protobuf format, which the exporter does
  not write.

The time is when the series began to count:

| Series | Created |
| --- | --- |
| The [collector metrics](#collector-metrics), `http_exporter_rule_failures_total`, the [scrape-time histogram](#scrape-time-histograms) and the [Python worker](#python-workers) counters of a collector the exporter started with | The exporter's start. |
| The same, of a collector a reload added, or removed and brought back | When the collector's counters were made, from zero: at the first probe or the first read of the self-metrics after the reload. |
| The [per-request](#verbose-per-request-self-metrics) counters | When the first probe of the request began — of several first probes at once, the one that ended first — or when a static target's request was registered. A request dropped — not asked for within the hour, removed with its collector or static target, or with verbose mode switched off — and asked for again counts from zero, since a later time than it showed before. |
| `http_exporter_config_reloads_total`, the [OTLP](#otlp-export-status) counters, the counters of the [Python execution pool](#the-python-execution-pool), and the `go_` and `process_` counters and `go_gc_duration_seconds` | The exporter's start. The pool's counters keep what a collector counted when a reload removes it, so they do not start again. |

The exporter's start is the start of its process, the same time
`process_start_time_seconds` reports — where the platform does not say, the
moment the exporter was loaded — and the same at every scrape. A series that
goes on counting across a reload keeps its time, and no series' time changes
while the series is shown: a slow probe that began before a request's time and
ends after it adds its counts to the request's series and leaves the time
alone. When such a probe is the one that brings a dropped request back, the
request counts since the probe ended, which is later than the time it showed
before, rather than since the probe began.

One counter has no `_created` sample: `go_memstats_alloc_bytes_total`. Its
OpenMetrics family would be named `go_memstats_alloc_bytes`, which is the
gauge beside it, so in OpenMetrics it is written as an `unknown` family (see
[OpenMetrics](CONFIGURATION.md#openmetrics)), and those have none.

Over [OTLP](OTLP.md) the same time is the start time of the series' points,
whatever this setting says: OTLP has a field for it, so it costs no series.
