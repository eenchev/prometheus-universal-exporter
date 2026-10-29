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

- Nothing; you can upgrade without changes.

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
