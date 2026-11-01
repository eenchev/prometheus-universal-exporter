# Changelog

What each release changes for the people who run it. The exporter and the Helm
chart are released separately ([Releases](docs/RELEASING.md)), so each has
sections of its own: `## exporter X.Y.Z` and `## chart X.Y.Z`, newest first.
A section's text is its GitHub Release's description.

Every pull request adds its lines to `## Unreleased`; a release moves the
lines it ships into its own section. Each section begins with
`### What changed for you`: what someone upgrading has to do or will notice,
such as a renamed configuration key, a changed default, something removed or a
changed metric, or "Nothing; you can upgrade without changes." when there is
nothing. `### New` and `### Fixed` follow when they have lines.

## Unreleased

### What changed for you

- Fewer Python workers stay idle, for less long: after a burst of scrapes one
  worker of each collector's script stays idle, rather than four, and an idle
  worker stops after two minutes unused, rather than five. Each is a CPython
  process of about 11 MB, 17 MB with lxml, so ten Python collectors keep
  about 110 to 170 MB in idle workers rather than 440 to 680 MB. Two minutes
  is twice a scrape interval of a minute, so a collector scraped every minute
  or more often still keeps its worker warm. `--python.max-idle-workers` and
  `--python.idle-timeout` bring the old behaviour back (`4` and `5m`).
- The `go_` and `process_` resource metrics are published on the
  self-metrics endpoint by default: reading them no longer stops the world,
  because the memory statistics now come from `runtime/metrics` and the
  garbage collection pauses from `runtime/debug.ReadGCStats` instead of
  `runtime.ReadMemStats`. Set `web.self_metrics.resource_metrics_enabled:
  false` to leave them out. `go_gc_duration_seconds` now carries the
  quantiles 0, 0.25, 0.5, 0.75 and 1 beside its `_sum` and `_count`, as in
  client_golang. The series names, types and help texts are as before.
- The JSON schemas refuse more of what the exporter refuses, so an editor now
  marks it: durations and sizes under the exporter's least (`script_timeout`
  under 1ms, `max_script_memory` under 32MiB, intervals under 1s, a zero
  `failure_backoff` duration), a client certificate without its key or a key
  without its certificate, two credentials in one request, a pagination key of
  another paging type, `grpc` descriptor files beside the wrong source, and a
  `python` transform without a `script`.
- A key that takes one of a few words takes them only as the schemas write
  them: `request.type`, `transform.type`, `decoder.type`, the
  `error_handling` policies and a rule's `error_mode`,
  `request.pagination.type`, `request.descriptors` and `response.graphite`'s
  `value` and `invalid_lines` in lower case and without blanks around them
  (`log`, not `LOG`; `type: http`, not `HTTP`), and `request.method` in upper
  case or lower case (`GET` or `get`, not `Get`). A configuration that writes
  one otherwise is refused, naming the words taken: write them in lower case.
  The static target file's schema now takes a `method` in lower case, as the
  exporter does.
- An entry of `accept_status` or `retry.statuses` written in quotes is taken
  only as the schemas take it: a status's three digits (`"503"`) or a class
  (`"5xx"`). Text with a sign, leading zeros or blanks, such as `"+503"`,
  `"0503"` or `" 503 "`, is refused when the configuration loads; write
  `503`. An unquoted number is read as before.
- The schemas refuse an entry of a `grpc` collector's or target's
  `retry.codes` and `accept_codes` that names no gRPC status code, or `OK`,
  as the exporter does, so an editor now marks it.
- `starttls` on an `ldap` collector is refused as a key `ldap` does not take,
  pointing to `start_tls: true`; `start_tls` on a `tcp` or `tlscert` collector
  points to `starttls` and its protocols.
- The image declares a `HEALTHCHECK`, so Docker shows the container
  `healthy` while the exporter is ready. A container started with another
  `--web.listen-address` needs `--health-cmd` with it, and the target
  allocator run from the image needs `--no-healthcheck`.
- The debug page calls the collector written on it the playground, and runs
  only when *Run* is pressed: *Run as you type* is gone. *Probe a target*
  probes through the configured collector chosen, or, with the playground,
  through the one it was copied from with *Edit a copy*; the *Fetch through*
  list is gone, and a playground copied from none cannot fetch. *Scrape a
  static target* is a list of the static targets of that same collector,
  each shown with its address, credentials withheld, in place of a name to
  type, and follows reloads; `/debug?static_target=<name>` chooses the
  target and its collector. The pasted body starts empty with a configured
  collector, the example's answer only with the example.
- The exporter's pages show the project's logo as their browser-tab icon.
- The status page shows a long error — and a long target, labels or probe
  parameters — as its first 160 characters or three lines with *Show all*,
  which opens the whole text in a box over the page; the page does not
  refresh while the box is open. On the debug page *Run* stands level with
  the timeout box beside it, and the status page's Refresh button and
  auto-refresh selector are one height, centred on one line.
- A `SIGTERM` or `SIGINT` no longer waits for the static target scrapes in
  flight: they are cut short at once, logging nothing above `debug`, so a
  target that does not answer no longer holds Ctrl-C up for all of
  `--web.shutdown-timeout`. The last OTLP export goes without what those
  scrapes would have queued. Probes in progress are still waited for, and the
  log now says how many, for how long at most, what is still awaited every 5
  seconds, and what it gave up on when the timeout runs out. A second signal
  logs `second signal: exiting now, …` before the process ends by it, as
  before. The line the shutdown begins with now reads
  `shutting down: waiting for 1 probe in progress, at most 15s; …`, or
  `shutting down: no probes in progress; exiting`.
- The debug page no longer keeps the playground, its body or the collector
  it was copied from in the browser between visits. A collector run on what
  another collector fetches, through `request_from` or a static target,
  must now be of that collector's request type (`400` otherwise), where it
  had to be an `http` collector before.
- The OTLP export writes `aggregationTemporality` as the number `2` where it
  wrote the name `AGGREGATION_TEMPORALITY_CUMULATIVE`: the OTLP/JSON encoding
  requires an enum's number, which every receiver accepts. A receiver or a
  test that matched the request body's text sees the number now.

### New

- `--python.max-idle-workers` (the chart's `server.pythonMaxIdleWorkers`),
  `1` by default, says how many workers of each collector's script stay idle
  after a burst of concurrent scrapes; `0` keeps none, each worker stopping
  when its run ends. `--python.idle-timeout` (`server.pythonIdleTimeout`),
  `2m` by default and at least `1s`, says how long an idle worker is kept
  unused. Idle workers are checked every minute, or every half the timeout
  when that is shorter. See
  [Idle workers and memory](docs/PYTHON.md#how-scripts-run).
- `--web.debug.dangerous` (the chart's `server.debugDangerous`), with
  `--web.debug=full` only, lets the debug page's playground run Python, name
  the server's files and probe a target with its own request, for writing a
  collector on a machine of one's own; never on a shared deployment.
- The pages' bar has a *Theme* picker: *Auto*, the default, follows the
  browser's light or dark preference, and *Light* or *Dark* holds whatever it
  prefers, kept in the browser for every page.
- `--healthcheck` asks a running exporter whether it is ready (`/ready`), or
  with `--healthcheck=health` alive (`/health`), and exits 0 or 1, for a
  container health check in an image without `curl`.
- The chart's `healthProbes.mode: exec` runs the liveness and readiness
  probes as the exporter's `--healthcheck` in the container instead of HTTP
  GETs from the kubelet.
- `request.unix_socket` sends an `http` collector's requests over a Unix
  domain socket, such as Docker's `/var/run/docker.sock` or a php-fpm status
  socket, as `curl --unix-socket` does; the URL's host is still the `Host`
  header. A path longer than the system allows (107 bytes on Linux, 103 on
  macOS) is refused when the configuration loads.
- `--dry-run` lists how each `http` and `graphite` collector connects under
  `request_policies`: its `unix_socket`, or the `proxy` in force — its
  `proxy_url`, from its own request or its file's `defaults`, `direct`, or
  `environment` when it follows `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY` —
  with the paths of `proxy_basic_auth_file`, never the credentials.
- `otlp.logs` sends the exporter's own log lines over OTLP as well, to the
  collector its metrics go to (`/v1/logs`), from `otlp.logs.level` up, with
  the `otlp` block's headers, TLS and retries; stderr is written as before.
  Logging never waits for it: a line that finds `max_pending_records` waiting
  is dropped and counted (`http_exporter_otlp_log_*`), and the status page
  shows the log export. It never makes the exporter unready.
- The status page has a Refresh button and an auto-refresh selector, as a
  Grafana dashboard has: `Off`, or every `5s` to `1h`, kept in the URL as
  `?refresh=30s`, refreshing the page's content in place, pausing while the
  tab is hidden, and saying once, beside the controls and in the browser's
  time, the time the data shown is of, or that a refresh failed.
  `--web.status-refresh`, and the chart's `server.statusRefresh`, set the
  interval a URL without one gets; off by default.
- `otlp.metric_names: otel` names the OTLP export's metrics as OpenTelemetry
  does: a counter without its `_total`, and a unit suffix such as `_seconds`
  or `_bytes_per_second` moved into the metric's unit (`s`, `By/s`). The
  default, `prometheus`, keeps the names as they are.
- `otlp.scope` sets the instrumentation scope's name and version of the
  exported metrics and log records; left out, it is
  `prometheus-universal-exporter` without a version, as before.
- A static target's `otlp.labels_as_resource` moves labels of its own from
  every exported data point to its OTLP resource, and `otlp.instance_id: true`
  gives the resource `service.instance.id`, the target's name.
- The debug page shows a run's series as a table, beside the Prometheus
  text: a row for each sample, a column for each label, the value and the
  type, sorted by a column's header, filtered by text and by metric, and the
  metric lint's advice on the rows of the metric it is about. The text view
  is the whole answer, as before, and `/debug/run` answers the series one
  sample at a time as `samples`. The response radio that probes a target is
  now called *Probe a target*. A pasted response's headers and the probe
  parameters are rows of a name and a value, added with *+ Add header* or
  *+ Add parameter* and removed with *×*, in place of a text box of lines:
  a row is checked as the server would check it and marked with why, the
  collector's own `{{param_<name>}}` parameters are rows to start with,
  their defaults shown, and the others it takes are suggested as names. A
  run's `headers` and `params` take a list of values for a repeated name.
  *Copy as* gives a probe of the form as its `/probe` URL, a `curl` command,
  a Prometheus `scrape_config` for the collector and the target, and the
  debug probe's URL, and a static target as its `/static-targets` URL and
  `curl` command; no credential is put in them.
- The debug page's editor numbers its lines in a gutter beside the text, and
  marks the lines a refusal of the playground names, in the gutter and with
  a band behind each; `line 7` in the error is a link that selects the line.
  In the playground Tab indents by two spaces and Shift+Tab outdents (Esc,
  then Tab, leaves the editor), and Ctrl+Enter or ⌘+Enter runs from
  anywhere on the page.

### Fixed

- The debug page no longer shows a credential written in free text of a
  configured collector's request, in its view or in *Edit a copy*: an `http`
  body, a `grpc` message, a `tcp` send (`AUTH …`, `PASS …`), a `redis`
  command's argument, a `sql` statement or argument, a query parameter's
  value, a folded block among them, is shown `<redacted>` whole when
  something in it reads as a credential.
- *Edit a copy* of a collector whose `tls` or other block is written on one
  line (`tls: {ca_file: /etc/ca.pem, server_name: api}`) loads: the comment
  for the dropped file stands on a line of its own, where a stand-in key was
  left that the playground refused.
- *Edit a copy* of a `grpc` collector with `descriptors: proto` or
  `protoset` loads: the playground, which makes no request, no longer asks
  for the `proto_files` or `protoset_file` the copy leaves out.
- *Edit a copy* is disabled until the collector's YAML is read, so it no
  longer copies the page's `Reading the configuration…` line or an error.
- *Copy as* *scrape_config* writes `metrics_path` as the path reads
  (`/my pfx/probe`), which Prometheus escapes once, not escaped twice.
- The status page no longer swaps its content under the long-text box when
  a refresh already on its way answers while the box is open; the answer is
  shown when it closes, and focus goes back to *Show all*.
- The collectors page's *Debug* link no longer keeps a target cleared from
  the form since it was last followed.
- The debug page's `Ctrl+Enter runs` hint is level with *Run* and the
  timeout's box.
- The debug page asks for what the chosen collector's request type takes,
  not for an HTTP target: the target as the type names it, with an example
  of its forms (`SSH host`, `host or host:22`; `Name to resolve`,
  `example.com`), no target field for a collector that names its own
  (`kubernetes`), the collector's own `{{param_...}}` parameters with their
  defaults, or a note that it takes none, and, for a pasted response, what
  the type's answer is, with a status and headers only for `http` and
  `graphite`. A response pasted for a `sql` collector is read as its rows,
  a JSON list of objects, where it gave no series before. The collectors
  page labels and hints each target the same way.
- The problems of a one-line block (`defaults: {...}`, a collector's
  `request`, `otlp`, `web.basic_auth`) are said in the order of the keys on
  the line.
- An answer sent uncompressed no longer allocates for the answer-size
  counters.
- A `cache`, `failure_backoff` or `request.tls` block, and a static target's
  `request`, say every unknown key and value of the wrong kind they hold, not
  only their first unknown key; a mistake in a `cache` or `failure_backoff`
  no longer hides the problems in the rest of the file.
- The schemas take `request.pagination.size: 0`, which the exporter reads
  as `size` left out: the offset moves on by the items each page held.
- Reading a scraper's `Accept-Encoding` header no longer allocates on every
  scrape.
- GitHub Releases are described from `CHANGELOG.md` instead of a list of pull
  requests.
- The status page has the bar the other pages have at their top: back to the
  landing page, the other pages and the documentation. Every HTML page now
  carries the same bar, the page shown marked.
- The debug page shows the configured collector chosen as YAML, as its file
  writes it, read-only, with its credentials and file paths withheld and the
  keys it takes from its file's defaults marked, instead of the example,
  which it does not run; *Edit a copy* writes it on the page. A link from
  `/collectors` opens the page with it shown.
- The debug page's playground may be of any request type the build has, so
  *Edit a copy* of a `tlscert`, `sql`, `dns` or other collector now runs: on
  a pasted answer of that type, or on what the collector it was copied from
  answers for a target or a static target. Before, every run of such a copy
  was refused with `422: ... a collector written on the debug page is an
  http collector`. The playground's request block still makes no request,
  and a playground with a `*_file` key, a `login` or Python is still
  refused.
- The debug page opens with the example playground every time, not with
  what was written or copied on it at an earlier visit; *Back to the
  example* puts it back in place of a copy.
- A pasted response asks for a `Content-Type`, a status and headers only
  when the playground's `request.type`, as the editor writes it, is `http`
  or `graphite`, and its placeholder says what that type's answer is; the
  fields follow the type as it is typed.
- *Scrape a static target* says whose static targets it lists: the
  collector chosen, or the one the playground was copied from.
- A `localfile` read's `READ` line in a debug report, and its verbose `url`
  label, show the file's `file://` URL escaped as a URL is: a space in the
  path is `%20`, where it was shown as a literal space, and a `?` or a `#`
  in a file's name no longer reads as a query or a fragment. The `sftp://`
  URL of an `sshfile` read, the `s3://` URL of an `s3file` object and the
  database of a `sql` statement's URL are escaped the same way.
- The debug page shows a configured collector's folded block scalar, such
  as a `statement: >-` written over several lines, over the lines its file
  writes it on, where it was folded into one long line that ran off the
  right of the editor.
- A debug report's request lines, on `/probe?debug=true` and the debug page,
  show the values of the query parameters the collector's configuration
  sets as its file writes them, where every value was `<redacted>`: a
  `graphite` collector's `target`, `from`, `until` and `format=json`, an
  `http` collector's `request.query` values without placeholders and a
  `kubernetes` listing's `limit` and selectors. A value a probe parameter,
  a placeholder or a `${NAME}` reference gives, the target's own query and
  any parameter whose name reads as a credential's stay masked, as every
  query value of an error, which a probe's caller is told, does.
- *Edit a copy* of a configured collector that names files (`tls.ca_file`,
  `password_file`, `bearer_token_file`, `known_hosts_file`, …) or logs in
  gives a playground that loads: those lines are left out, each replaced by
  a comment saying why, with a block they leave empty, and a note above the
  editor lists them. It was refused until they were deleted by hand.
- `--healthcheck` with `--web.listen-address=[::]:PORT` asks `127.0.0.1`,
  which the exporter's `[::]` listener answers too, where it asked `[::1]`
  and failed on a host without IPv6, so `healthProbes.mode: exec` restarted
  the pod for ever. A named port (`:http`) and an IPv6 zone
  (`[fe80::1%eth0]:8080`) give a URL the check can ask, where it could not.
- With `otlp.metric_names: otel` two counters that are one name there, such
  as `x_seconds` and `x_seconds_total`, or `requests` and `requests_total`,
  are no longer merged into one metric that kept one point of the two where
  their labels were alike: the one written last is exported, and the other
  is left out and logged as a warning, `OTLP metric name written for two
  Prometheus metric names under one resource`, as a name of two kinds is.
- The OTLP log export sends each attribute key of a record once, the value
  given last, where `logger.With("a", 1)` and a line's own `a` gave two
  attributes `a`; and a value whose `Error` or `String` panics, as a nil
  error's may, is `<nil>` or `!PANIC: …`, as on stderr, where it took the
  exporter down.

## exporter 1.8.0

### What changed for you

- Answers of `/probe`, the self-metrics and the static targets are gzipped at
  level 1 instead of 6: a few percent bigger and about a third faster to make.
  `--web.gzip-level` sets the level, 0 to answer uncompressed.
- A failed probe's answer no longer quotes what the target answered: a value
  a decoder could not read, a database's message (the caller is told its error
  code), an identity provider's refusal and an `sshexec` command's stderr are
  replaced by `[withheld; in the exporter's log]`. The log and the debug report
  keep the whole text. A probe that panicked is answered a fixed text.
- The status page shows a probe's or a static target's error as the probe's
  caller is told it; the whole text only with `--web.debug=full`.
- A `vertica` statement the driver would not prepare is refused, and the SQL
  page says a Vertica server can read the exporter's files through its driver:
  hold `vertica` collectors to trusted servers with `allowed_targets`.

### New

- A top-level `defaults` block, in the configuration file and in each collector
  file, gives that file's collectors every key they do not write themselves;
  `failure_backoff: {enabled: false}` turns off a backoff the defaults give.
- `rate_limits`: limits per host, shared by every `http` and `graphite`
  collector.
- `request.proxy_url` and `request.proxy_basic_auth_file`: a proxy of the
  collector's own for `http` and `graphite`, or `direct`.
- Static targets may carry their own credentials and `tls` block (`redis`,
  `ldap`, `tcp`, `sql`, `grpc`, `http`, `graphite`).
- A status page for the static targets at `/static-targets/status`
  (`?format=json` for scripts), with the recent probes and the OTLP export.
- `derived`: sum, difference and ratio series computed from a collector's own
  series, under every transform.
- Metric lint: advice about the series a collector makes, in `--dry-run` and
  every debug run.
- The `ntp` request type, in the default build, and the opt-in `sshexec`
  request type, which runs a fixed command on a host over SSH.
- `tcp`: `request.greeting` for services that speak first, `request.starttls`
  (smtp, imap, pop3, ftp), `request.document` for a JSON document of the
  exchange, and `X-Tcp-*` timing headers on every answer.
- `tlscert`: `request.starttls` reads a mail or FTP server's certificate.
- Self-metrics `http_exporter_answer_bytes_total` and
  `http_exporter_answer_sent_bytes_total`, by endpoint, before and after
  compression.

### Fixed

- A static target removed at a reload is forgotten then, so one brought back
  starts as not yet scraped; a scrape skipped by `failure_backoff` no longer
  shows as a failed one on the status page.
- `tcp`: a race between the TLS handshake and the deadline, and a `send` its
  placeholders leave empty, now refused with 400.

## chart 1.8.0

### What changed for you

- Deploys exporter 1.8.0.
- Rendering fails when `ingress.enabled`, or a `service.type` of `NodePort` or
  `LoadBalancer`, exposes the exporter without `webAuth`, or with a rendered
  configuration whose `web.basic_auth` does not read the mounted password.
  Enable `webAuth`, or set `exposure.allowWithoutWebAuth: true` when callers
  authenticate elsewhere.

### New

- `server.gzipLevel` sets `--web.gzip-level`.
