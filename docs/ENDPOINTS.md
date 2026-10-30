# Endpoints

The exporter serves its endpoints over plain HTTP on `--web.listen-address`
(`:8080` by default). Which of them are served, and at which path, is set by
the [command-line flags](FLAGS.md).

| Path | Purpose |
| --- | --- |
| `/` | A landing page: the build, links to the endpoints, and when the last reload put something in force and [what it changed](CONFIGURATION.md#what-a-reload-changed). |
| `/collectors` | Each collector with a form that probes a target through it, taking its request parameters, forwarded headers and, when it forwards `Authorization`, a target credential; with `--web.debug`, a link into the debug page. See [Probing from the browser](AUTHENTICATION.md#probing-from-the-browser). |
| `/debug` | With `--web.debug`: run a collector, configured or written there in the playground, and see each stage of its trip and the series it gives, on a pasted response or, at `full`, a fetched target. See [Debugging collectors](CONFIGURATION.md#debugging-collectors). |
| `/debug/run` | With `--web.debug`: where the debug page posts each run, `POST` with a JSON body and `X-Debug-Run`, answered with the run's stages, series and report as JSON; any other method is answered `405`. See [Debugging collectors](CONFIGURATION.md#debugging-collectors). |
| `/debug/collector` | With `--web.debug`: a configured collector as the debug page shows it, `?name=<name>`, with `GET`: its YAML as its file writes it, credentials and file paths withheld, and at `full` the static targets scraped through it, as JSON. See [The debug page](CONFIGURATION.md#the-debug-page). |
| `/probe` | Scrape a target through a collector. Takes `target` and `collector`, with `GET` or `HEAD`; any other method is answered `405`. With `--web.debug=full`, `&debug=true` answers with a report of the trip instead; see [Debug probes](CONFIGURATION.md#debug-probes). A failed probe is answered its stage and its error without what the target answered; see [What a failed probe's caller is told](CONFIGURATION.md#what-a-failed-probes-caller-is-told). |
| `/self-metrics` | The exporter's own metrics, at `--web.self-metrics-path`, with `GET` or `HEAD`; any other method is answered `405`. See [Self-metrics](SELF-METRICS.md). |
| `/static-targets` | The latest results of the [static targets](STATIC-TARGETS.md), at `--web.static-targets-path`. `?targets=a,b` serves only the targets named; with `--web.debug=full`, `?debug=<name>` [reports one scrape](STATIC-TARGETS.md#debugging-a-static-target). |
| `/static-targets/status` | The status page: each static target's last scrape — when, how long, its result (success, failed, stale result served, skipped by backoff, not yet scraped), the last error and whether it is backing off — the recent probes of each collector and target, and how the OTLP export is going, as an HTML page that refreshes itself every `?refresh=` interval or `--web.status-refresh`, or as JSON with `?format=json`, with `GET` or `HEAD`; `?format=fragment` is the page's content section alone. Always the static targets path followed by `/status`; served with or without static targets. See [Status page](#status-page). |
| `/-/reload` | `POST` or `PUT` reloads the configuration, with `--web.enable-lifecycle`, as either does Prometheus's own; any other method is answered `405`. See [Reloading on demand](CONFIGURATION.md#reloading-on-demand). |
| `/health`, `/ready` | Kubernetes probes, and the exporter's own [`--healthcheck`](CONFIGURATION.md#health-checks) for a container health check. `/ready` is `503` while OTLP exports keep failing (when `otlp.unready_after_failures` asks for it), the exporter is shutting down, or in allocator mode before the first bundle, not after a rejected reload; see [Readiness](CONFIGURATION.md#readiness). Never authenticated. |

`/probe`, the self-metrics path and the static targets path answer gzip-compressed when the client accepts it, as Prometheus does on every scrape. They are compressed at `--web.gzip-level`, by default `1`, the fastest: an answer is compressed on every scrape and read once, and level `1` compresses one in a fifth or less of the time level `6` takes, for an answer at most about a tenth larger. `9` makes the smallest answers, for a scrape over a slow link; `0` answers uncompressed whatever the client accepts, which Prometheus takes as well, when the network is cheaper than the exporter's CPU. The self-metrics count the bytes of these answers before and after compression, by endpoint ([Answer sizes](SELF-METRICS.md#answer-sizes)).

The HTML pages — `/`, `/collectors`, `/debug` and the status page — share
one bar at the top: the way back to the landing page, a link to each of the
other pages, the one shown marked, and the documentation. Its links are
relative to the page, as every link of the pages is, so they hold behind a
reverse proxy that serves the exporter under a path of its own. Each page's
browser-tab icon is the project's logo, [`docs/logo.svg`](logo.svg), carried
in the page itself as a `data:` URL, so it costs no request of its own.

Every endpoint but `/health` and `/ready` answers `403` to a request the
browser marks as made by another site's page, `Sec-Fetch-Site: cross-site`
or `same-site`, with Basic Auth on or off: a browser sends the exporter's
credential with such requests too. The exporter's own pages, an address typed
in, and clients that are not browsers, Prometheus among them, pass. See
[Requests from other sites](AUTHENTICATION.md#requests-from-other-sites).

## Status page

For people who have no Prometheus to ask, the exporter serves a status page
at the static targets path followed by `/status`, `/static-targets/status` by
default, with or without static targets; the landing page links it. It moves
with `--web.static-targets-path`, so a `--web.self-metrics-path` on it is
refused. It has three sections:

- the static targets, a row per target with how its last scrape went (see
  [Status page](STATIC-TARGETS.md#status-page) in Static targets), or a line
  saying none are configured;
- the recent probes: the `/probe` requests the exporter answered, which is
  how Prometheus scrapes most setups;
- the [OTLP export](#otlp-export): whether exports get through, or a line
  saying OTLP export is off.

While the last OTLP export failed, a line at the top of the page says so,
with the failed exports in a row and whether they made the exporter unready.

### Refreshing

The page refreshes as a Grafana dashboard does. Its Refresh button fetches
the page's content again and puts it in place, without reloading the page;
its auto-refresh selector does so every `5s`, `10s`, `30s`, `1m`, `5m`, `15m`,
`30m` or `1h`, or not at all, `Off`. Beside them the page says the time the data
shown is of, once, in the browser's time, with the UTC time as its tooltip:
the content's own "As of" line, in UTC, is for a page without JavaScript, and
the script hides it. When a refresh fails — the exporter is restarting, or
answers an error — the page says that it failed, why, and the time the data
still shown is of; the next refresh tries again.

The interval chosen is kept in the page's URL, `?refresh=30s`, so a bookmark
or a shared link opens the page with it. A URL without one takes
`--web.status-refresh`, off by default. `?refresh=` takes `off` or a duration
in whole seconds from `5s` to `24h`, such as `90s`, which the selector then
lists in its place among the others; any other value is ignored and the
flag's interval applies, so a mistyped link still shows the page.

The page is not refreshed while its tab is hidden; back in view, it is
refreshed at once if a refresh fell due meanwhile. A refresh still waiting
for its answer is never overlapped: the tick that finds one is skipped.

A refresh fetches the page's own URL with `?format=fragment`: the content
section alone, without the bar at the top, rendered by the page's template, as HTML, with the page's
headers — `Cache-Control: no-store`, so nothing between keeps a copy, and
the pages' Content-Security-Policy. The fetch goes to the same origin, so
the browser sends it with the Basic Auth credential the page was opened with
and marks it `Sec-Fetch-Site: same-origin`, which the
[refusal of requests from other sites](AUTHENTICATION.md#requests-from-other-sites) lets through. Without JavaScript the page
still works: the Refresh button reloads it, and the selector, with an Apply
button, puts the interval in the URL, on which the page reloads itself.

### Recent probes

Every probe the exporter answers is recorded under its collector, its target
as the log shows it, with a credential in its URL withheld, and its probe
parameters: `param_<name>`, `path`, `method`, `body`, `message`, `timeout` and
the rest that change the request, since each makes a request of its own, a
tenant or a path. A parameter's value is shown with anything reading as a
credential masked, a URL's `user:password@` among it, the whole value withheld when its name reads as one
(`param_token`), and cut to 64 bytes; forwarded headers (`header_<name>`) are
not shown. A probe answered from the response cache, or by a trip another
identical probe started at the same moment, is recorded too, marked `cached`
or shared: it is what Prometheus was given. A debug probe
(`&debug=true`) is not recorded, nor is a probe refused for its parameters
(`400`), nor one whose caller went away before it was answered.

Each collector and target has a row, the failing ones first, then in order of
collector, target and parameters:

- the collector, target and parameters;
- when its latest probe began, how long ago that was and how long it took;
- its result: `success`, `cached` (answered from the response cache),
  `failed`, `refused by policy` (`allowed_targets` or `denied_targets`
  refused the target, `403`), `rejected by limit` (`max_concurrent_probes` or
  `--probe.max-concurrent` turned it away, `503`), `stale result served`
  (failed, and the last good result served under `cache.stale_if_error`) or
  `backing off` (`failure_backoff` kept it from the target); whether it was
  shared; the HTTP status the exporter answered and the series it gave;
- the results of its last 10 probes as a strip, oldest first: `✓` success or
  cached, `✗` failed, refused or rejected, `~` stale result served, `…`
  backing off; each mark's title has the probe's time, result and status;
- the error of the latest failed probe of the ten, and when that probe began,
  shown as the static targets' errors are: the log's text, with every URL's
  credentials withheld, its userinfo too where the text only quotes the URL,
  anything else that reads as a credential masked, and cut to 2000 bytes. A
  failure is shown as the probe's caller was answered it, without the values
  of the target's answer it quotes or what is for the operator alone, such as
  what an [`sshexec`](SSHEXEC.md) command wrote to stderr
  ([What a failed probe's caller is told](CONFIGURATION.md#what-a-failed-probes-caller-is-told)),
  unless `--web.debug` is `full`: the page is read by whoever may call
  `/probe`. A
  probe that panicked shows `the scrape panicked; the exporter's log has the
  panic`, the text it was answered: the panic's own text is only in the log.

A row is failing when its latest probe did not succeed: `success` and
`cached` are the successes. A summary line says how many collectors and
targets were probed and how many are failing.

The targets come from whoever may call `/probe`, so what is kept is bounded:
the last 10 probes of each collector and target, and no more than 500 of
them, the one probed least recently forgotten when a new one comes. A target
is cut to 512 bytes and the parameters to 512 bytes in all, so two probes that
differ only past the cut, or in what is withheld, share a row. The errors
kept take at most 500 × 10 × 2000 bytes, 10 MB, and far less in practice: a
probe failing as the one before keeps the one text. The bounds are fixed;
there is no flag for them.

Every probe answered is recorded, a cache hit as much as any, so recording
one is kept cheap: the row of a probe is remembered by the probe's cache key,
and a probe like one before it, as nearly every probe is, finds its row by
that key without its parameters being sorted and redacted again. Up to 1,024
such keys are remembered, each with its row's collector, target and
parameters bounded as above, under 1.1 MB in all; when that is full they are
forgotten together and remembered again as probes come.

A reload forgets the recent probes of every collector it removes or changes,
as it does the rest of what is kept per collector; a probe of the old
definition that ends after the reload records nothing. A collector a reload
leaves as it was keeps its rows.

`?format=json` serves the same as JSON: `probe_keys` and
`failing_probe_keys` beside the static targets' summary, and `probes`, an
array with an object per row and, in it, the row's probes, oldest first:

```json
{"generated_at": "2026-10-24T09:00:00Z", "allocator_mode": false, "targets": 0, "failing": 0, "not_yet_scraped": 0,
 "static_targets": [], "probe_keys": 2, "failing_probe_keys": 1,
 "probes": [
  {"collector": "billing", "target": "https://billing-eu.example", "params": "param_tenant=acme",
   "result": "failed", "failing": true, "shared": false,
   "last_probe": "2026-10-24T08:59:45Z", "last_probe_age_seconds": 15.1, "duration_seconds": 0.212,
   "status": 502, "series": 0,
   "last_error": "fetch https://billing-eu.example/v1/usage/acme: received HTTP status 503",
   "last_error_at": "2026-10-24T08:59:45Z",
   "probes": [
    {"at": "2026-10-24T08:59:15Z", "duration_seconds": 0.198, "result": "success", "shared": false, "status": 200, "series": 41},
    {"at": "2026-10-24T08:59:45Z", "duration_seconds": 0.212, "result": "failed", "shared": false, "status": 502, "series": 0,
     "error": "fetch https://billing-eu.example/v1/usage/acme: received HTTP status 503"}
   ]},
  {"collector": "weather", "...": "..."}
 ]}
```

`params`, `last_error` and a probe's `error` are left out when empty.

The page shows nothing the self-metrics, the series and the log do not: it is
protected by `web.basic_auth` like every endpoint, answers `GET` and `HEAD`
only, and every value on it is escaped. It shows the moment it was served,
and refreshes as [Refreshing](#refreshing) says. In allocator mode it says that it shows
the static targets the allocator gave this replica, the probes this
replica answered and this replica's OTLP exports: each replica has a page of
its own, and Prometheus spreads its probes over them.

### OTLP export

With [OTLP export](OTLP.md) on, the section shows:

- the endpoint as `otlp.endpoint` has it, with its credentials withheld as a
  target's are: the URL's user and password are `redacted:redacted`, and the
  values of query parameters named like credentials (`token`, `api_key`, …)
  `<redacted>`; `otlp.headers` are never shown. Then the protocol, always
  `http/json` (OTLP/HTTP with a JSON body, the only one the exporter
  speaks), and `otlp.interval`;
- what the exports carry: the probes' results, the static targets with
  `export_via_otlp` (how many), and the exporter's own metrics, which go
  with every export;
- the last export: when it began, how long ago, how long it took with its
  retries, its result — `success`, `failed`, or `partial success` when the
  endpoint took it but rejected some of its data points in its
  `partialSuccess`, with how many — the requests of it that were sent
  (`otlp.batch_max_size` and `otlp.batch_max_bytes` split an export), the
  data points they carried and the retries it made;
- when the last export that got through ended, or `never`;
- the failed exports in a row, and, with `otlp.unready_after_failures` set,
  the readiness as `/ready` answers it, from the same reasons: `ready`, or
  `unready` with the reasons `/ready` gives — the failed exports, the
  exporter shutting down, or in allocator mode no configuration yet — and,
  when the failed exports made it so, since the end of the failed export
  that did (see [Readiness](CONFIGURATION.md#readiness));
- the error of the last failed export and when that export began, kept
  after exports get through again, as a static target's is: the log's text,
  with every URL's credentials withheld, anything else that reads as a
  credential masked — a URL an error quotes has every query value masked,
  as in the log — and cut to 2000 bytes. An export of which the endpoint
  refused a request is a failed export, and its error is the refusal's, such
  as `the OTLP endpoint answered 400`;
- the data points pending — waiting for the next export, not those of an
  export in progress — and `otlp.max_pending_points`;
- since the start, the data points dropped, by why: refused by the endpoint,
  rejected in a `partialSuccess`, the oldest over `otlp.max_pending_points`,
  and those of the last export at shutdown when it failed; the retries; and
  the exports that succeeded and failed.

The numbers since the start are the
[self-metrics'](SELF-METRICS.md#otlp-export-status)
`http_exporter_otlp_points_dropped_total`,
`http_exporter_otlp_export_retries_total` and
`http_exporter_otlp_exports_total`, read where they are kept; the page keeps
of its own only the last export's details and the last failure's error, one
record whatever the exports.

With [`otlp.logs`](OTLP.md#the-exporters-own-log) on, a part of the section,
**Log export**, shows the export of the exporter's own log: its endpoint, with
its credentials withheld as `otlp.endpoint`'s are, its level and interval; when
the last log export began, its result and the records it carried; the last
that got through; the failed log exports in a row; the last failure's error and
when that export began; the records waiting and `otlp.logs.max_pending_records`;
and since the start, the records dropped, by why — over the bound, refused or
rejected by the endpoint, and of the last export at shutdown — the records
sent, and the exports that succeeded and failed. It says nothing of readiness,
which the log export does not touch. A reload that changes
`otlp.logs.endpoint` starts its record again, as for `otlp.endpoint`. With
`otlp.logs` off it is a line saying so.

A reload that changes `otlp.endpoint` shows the new endpoint at once, and
starts that record again: no last export, last success, failures or error
until the new endpoint's first export, as the failures that make the
exporter unready start again. The numbers since the start go on, as the
self-metrics do. A reload that changes the other settings shows them at
once and keeps the record.

In the JSON the section is `otlp`, `{"enabled": false}` alone while OTLP
export is off, and otherwise:

```json
"otlp": {"enabled": true,
 "endpoint": "https://redacted:redacted@otel.example/v1/metrics?token=<redacted>&tenant=acme",
 "protocol": "http/json", "interval": "30s",
 "exported": {"probes": true, "static_targets": 2, "self_metrics": true},
 "allocator_mode": false, "failing": true,
 "last_export": {"started": "2026-10-24T08:59:30Z", "age_seconds": 30.2, "duration_seconds": 29.6,
  "result": "failed", "requests": 1, "data_points": 812, "rejected_points": 0, "retries": 4},
 "last_success": "2026-10-24T08:58:31Z",
 "consecutive_failures": 2,
 "unready_after_failures": 3, "readiness": "ready",
 "last_error": "Post \"https://redacted:redacted@otel.example/v1/metrics?token=<redacted>&tenant=<redacted>\": dial tcp 10.0.0.7:443: connect: connection refused",
 "last_error_at": "2026-10-24T08:59:30Z",
 "pending_points": 812, "max_pending_points": 100000,
 "points_dropped": 0,
 "points_dropped_by_reason": {"refused": 0, "rejected": 0, "over_max_pending_points": 0, "last_export_at_shutdown": 0},
 "retries": 9, "exports_succeeded": 41, "exports_failed": 2,
 "logs": {"enabled": true,
  "endpoint": "https://redacted:redacted@otel.example/v1/logs?token=<redacted>&tenant=acme",
  "level": "info", "interval": "5s",
  "last_export": "2026-10-24T08:59:55Z", "last_result": "success", "last_records": 3,
  "last_success": "2026-10-24T08:59:55Z",
  "failing": false, "consecutive_failures": 0,
  "pending_records": 0, "max_pending_records": 10000, "records_sent": 4210,
  "records_dropped": 0,
  "records_dropped_by_reason": {"over_max_pending_records": 0, "refused": 0, "last_export_at_shutdown": 0},
  "exports_succeeded": 512, "exports_failed": 0}}
```

`logs` is `{"enabled": false}` alone while `otlp.logs` is off. Its
`last_export`, `last_result`, `last_records`, `last_success`, `last_error` and
`last_error_at` are left out when there is nothing to say.

`failing` says the last export failed. `last_export`, `last_success`,
`last_error`, `last_error_at`, `unready_after_failures`, `readiness`,
`unready_reasons` and `unready_since` are left out when there is nothing to
say: `readiness` without `otlp.unready_after_failures`, `unready_reasons`
while ready, and `unready_since` unless the failed exports are among the reasons (never
while shutting down, the one reason then). `unready_reasons` are
the lines of `/ready`'s body, such as `["the exporter is shutting down"]` or
`["the last 3 OTLP exports failed"]`.
`unready_since` is when the failed export ended that made the exporter
unready, the Nth of the current run of failures under
`otlp.unready_after_failures` N, whenever N was set: a reload that lowers it
to the run's length or under shows the end of the new Nth failure at once, and
one that raises it over the run's length shows `ready`.
