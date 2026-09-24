# Static targets

Besides answering `/probe`, the exporter can scrape a fixed list of targets
itself, each on its own interval, and serve their latest results together on
one endpoint for Prometheus to scrape, like any other exporter's metrics. A
target can also be delivered over OTLP.

Use them when the list of targets is known and fixed, and a Prometheus job per
target, or service discovery and relabelling for the probe pattern, would be
more machinery than the list deserves. For targets Prometheus discovers, keep
`/probe`.

## The target file

Pass the list with `--static-targets-file`.
`configs/static-targets.example.yaml` is a complete example, and
`configs/static-targets.schema.json`, printed by
`--static-targets-file-schema`, is the file's JSON Schema, for editors; start a
target file with

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/eenchev/prometheus-universal-exporter/main/configs/static-targets.schema.json
```

A target file looks like this:

```yaml
interval: 1m            # required: for every target that sets none
targets:
  - name: legacy_eu
    collector: legacy_text
    target: http://legacy.eu.example:8080
    interval: 30s       # this target's own
    request:
      path: /status
      timeout: 5s       # at most the interval
      retry:
        attempts: 2
        backoff: 2s
      headers:
        X-Tenant: team-a
      bearer_token_file: /var/run/prometheus-universal-exporter/target-auth/token
    labels:
      region: eu
  - name: legacy_us
    collector: legacy_text
    target: http://legacy.us.example:8080
    labels:
      region: us
```

`interval` and `targets` are required, and the file refuses keys it does not
know. Each target names a collector from the exporter configuration and takes
every per-scrape parameter `/probe` accepts — `method`, `path`, `body`,
`timeout`, `insecure_skip_verify`, `follow_redirects`, `enable_http2` and the
`retry` settings — overriding the collector's own request for that target only.
Which of these keys a target may set follows its collector's
[request type](CONFIGURATION.md#request-types); for `http` it is all of them,
and for [`localfile`](LOCALFILE.md#static-targets) only `path` and `timeout`,
with `target` optional. It also takes static `headers` and its own target
credentials, inline or file-backed, as basic authentication or a bearer token.
Because the file is operator configuration rather than caller input, these
headers are applied directly and are not filtered through the collector's
`request.forward_headers` allowlist.

A collector's [`{{param_…}}` placeholders](REQUESTS.md#path-parameters) — in
its path, body, header values and query values — are filled by the target's
`params`, since there is no probe to supply `param_<name>`:

```yaml
interval: 1m
targets:
  - name: acme_checkout
    collector: graphql_status
    target: https://api.example
    params:
      param_tenant: acme
      param_service: checkout
```

Every placeholder must be filled, by `params` or a default, and every entry of
`params` must fill one; otherwise the exporter refuses to start, naming the
target, the collector and the parameter. A target's own `request.path`, `body`
and `headers` are written out in full, without placeholders.

`labels` are added to every metric the target produces, without overwriting a
label the collector already extracted. `static_target` is the endpoint's own
label, so a target may not set it.

Check a target file together with its configuration before deploying it:
`prometheus-universal-exporter --dry-run --config.file=config.yaml --static-targets-file=static-targets.yaml`
reports whether each would load, including whether every target names a
collector that exists and whether a target exported over OTLP has OTLP export
to go to — see [Dry run](CONFIGURATION.md#dry-run).

## Scraping

Each target is scraped on its own `interval`, as Prometheus scrapes a probe on
its `scrape_interval`. Neither Prometheus's scrapes of the endpoint nor the OTLP
export decide when a target is scraped: the endpoint serves, and the export
delivers, what the last scrape of each target left.

A target's first scrape comes at a point within its interval set by its name,
so targets sharing an interval are spread over it rather than all scraped at
once, and then every interval from there, however long a scrape takes. A scrape
must end within its interval, so `request.timeout` may not be longer; one still
running when the next is due makes that one skipped, with a
`static target scrape skipped` warning, rather than overlapping it. The
interval is at least `1s`. At most eight targets are scraped at once, and each
waits for a slot of its collector's `max_concurrent_probes`, which it shares
with the probes.

Retries come from the collector's `request.retry`, and a target's own
`request.retry` replaces them, as the `retry_attempts` and `retry_backoff`
probe parameters do for a probe. Scrapes go through the same fetch, decode and
transform path as `/probe`, so collector limits, the response cache and
[`error_handling`](CONFIGURATION.md#when-a-stage-of-the-probe-fails) all
apply. A static target scrape and an identical `/probe` request share cache
entries, and under `log` or `ignore` a failed stage leaves the target up with
nothing of the collector's to serve, as it answers a probe `200` with an empty
body. With [`cache.stale_if_error`](CONFIGURATION.md#serving-the-last-good-result-when-the-target-fails),
a failed scrape serves the target's last good result, marked by
`http_exporter_result_stale` 1, while its `http_exporter_target_up` is `0`.

Static targets are not reachable through `/probe`.

## The static targets endpoint

The latest result of every target is served at `--web.static-targets-path`,
`/static-targets` by default:

```text
# TYPE application_connections gauge
application_connections{region="eu",static_target="legacy_eu"} 42
application_connections{region="us",static_target="legacy_us"} 17
# HELP http_exporter_target_up Whether the last scrape of this static target succeeded.
# TYPE http_exporter_target_up gauge
http_exporter_target_up{collector="legacy_text",region="eu",static_target="legacy_eu",target="http://legacy.eu.example:8080"} 1
http_exporter_target_up{collector="legacy_text",region="us",static_target="legacy_us",target="http://legacy.us.example:8080"} 1
```

- Every series carries `static_target`, the target's name: the targets share
  one endpoint, and the same metric from two targets stays two series.
- `http_exporter_target_up` and `http_exporter_target_scrape_duration_seconds`
  report each target's last scrape, so a failing target is visible rather
  than simply absent.
- A target that has not been scraped yet is not on the endpoint, and one
  removed from the file leaves it with the reload.
- The same metric from two targets must have one type. A target whose metric
  another target already serves with a different type has that metric left
  out, with a warning in the log, and the rest of both is served.
- Reading the endpoint contacts no target: it is as fast as the self-metrics,
  whatever the targets are doing, so Prometheus can scrape it on any interval.
  An interval shorter than the targets' serves the same values again; a longer
  one misses the values in between.
- The path must be one fixed path that no other endpoint uses, including
  `--web.self-metrics-path`; otherwise the exporter does not start.
- It is gzipped when asked, like `/probe`, and behind the exporter's
  [Basic Auth](AUTHENTICATION.md) when that is on.

Scrape it with a plain Prometheus job, keeping the series' own labels:

```yaml
scrape_configs:
  - job_name: static-targets
    metrics_path: /static-targets
    honor_labels: true
    static_configs:
      - targets: ['exporter:8080']
```

`honor_labels` keeps `static_target`, `target` and the targets' own labels as
they are, rather than prefixed with `exported_` where Prometheus has a label of
the same name. With the Helm chart, `staticTargets.monitor` renders the
ServiceMonitor or PodMonitor that does this — see the
[chart README](../charts/prometheus-universal-exporter/README.md#static-targets).

## Exporting over OTLP

A target with `export_via_otlp: true` is also delivered over
[OTLP](OTLP.md), on `otlp.interval`, besides being served on the endpoint:

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

It is off by default. Each export delivers what the target's scrapes left
since the last one, the latest value of each series, with the target's health
metrics: a target scraped more often than `otlp.interval` exports only its
latest values; one scraped less often exports its last result again only when
scraped again.

`otlp.service_name` and `otlp.resource_attributes` set the OTLP resource the
target's metrics arrive under; both fall back to the exporter-wide `otlp`
settings, and per-target attributes are merged over the exporter-wide ones.
Targets with different identities are exported as separate `resourceMetrics`
entries. The `otlp` block is only for a target with `export_via_otlp`, and
refused on any other. Over OTLP the series do not carry `static_target`: the
resource tells the targets apart.

`export_via_otlp` needs OTLP export: a target with it while `otlp.enabled` is
`false`, or without an `otlp.endpoint`, makes the exporter log
`invalid static target configuration; exiting` and stop.
`configs/config.otlp.example.yaml` is the example configuration with OTLP
export enabled that goes with `configs/static-targets.example.yaml`.

## Reloading

The file is reloaded on the same terms as the exporter configuration — on
`SIGHUP`, `POST /-/reload` and, with `--config.watch`, when it changes (see
[Reloading on demand](CONFIGURATION.md#reloading-on-demand) and
[Watching the configuration](CONFIGURATION.md#watching-the-configuration)). An invalid
document, or a configuration change that would disable OTLP export while a
loaded target sets `export_via_otlp`, is rejected, and the last valid pair
stays active. A target whose interval changes starts a cadence of its own.

## Self-metrics

Static target scrapes are counted in the per-collector
[self-metrics](SELF-METRICS.md), as probes are, rather than in per-target
series; the endpoint's health metrics are the per-target view.
`http_exporter_static_targets` reports how many targets are loaded, and
`http_exporter_static_targets_exported_via_otlp` how many of them are also
exported over OTLP. The file's reloads are reported under
`file="static_targets"` in the [reload metrics](SELF-METRICS.md#configuration-reloads).
