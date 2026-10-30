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
  only when *Run* is pressed: *Run as you type* is gone. *Fetch a target*
  fetches through the configured collector chosen, or, with the playground,
  through the one it was copied from with *Edit a copy*; the *Fetch through*
  list is gone, and a playground copied from none cannot fetch. *Scrape a
  static target* is a list of the static targets of that same collector,
  each shown with its address, credentials withheld, in place of a name to
  type, and follows reloads; `/debug?static_target=<name>` chooses the
  target and its collector. The pasted body starts empty with a configured
  collector, the example's answer only with the example.
- The exporter's pages show the project's logo as their browser-tab icon.

### New

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

### Fixed

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
