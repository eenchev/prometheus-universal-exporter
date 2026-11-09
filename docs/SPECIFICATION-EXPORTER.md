# Prometheus Universal Exporter — Exporter Specification

This is the specification for the exporter itself: its endpoints, configuration,
decoders, transforms, limits, logging, CLI, container image and tests. The Helm
chart that deploys it has its own specification in
[SPECIFICATION-CHART.md](SPECIFICATION-CHART.md), and each request type but
`http` has its section of § 5.1 in one of its own, `SPECIFICATION-<TYPE>.md`,
listed under [The other request types](#the-other-request-types).

Section numbers are the ones this specification has always used, and they did not
change when it was split. Each document therefore keeps its own sections' numbers
and skips the other's, so a reference to § 33.13 or § 42.8 still points at the
same requirement it always did. A section that carried requirements for both is
present in both documents under the same number, each holding only its own half
and linking the other. The request types' documents hold no numbers of their
own but the ones they took with them: § 5.1a, reading a directory, is in
`localfile`'s, and § 9a, gRPC answers as JSON, in `grpc`'s.

Sections about the repository as a whole — purpose, implementation order,
acceptance criteria, CI gates, documentation requirements — stay here, and name
the chart document where a chart requirement belongs to it.

## 1. Purpose

Build a production-grade Prometheus exporter written in Go that converts what a source that was never meant for Prometheus answers — an HTTP API, a gRPC service, a Graphite server, a file on the exporter's host, on another host over SSH or in an S3 bucket, a SQL database, a Redis or Valkey server, a service that answers a line sent over TCP, an LDAP directory, a DNS name, a TLS certificate, a Kubernetes object — into Prometheus metrics.

The exporter is intended for systems that expose useful operational/status data — over HTTP, gRPC or SQL, in files, in DNS, in their certificates or in the Kubernetes API — but do not provide suitable Prometheus metrics.

The exporter MUST:

- Discover targets through Prometheus Operator `ServiceMonitor` / `PodMonitor` rather than hardcoding target URLs in the exporter configuration.
- Select a configured `collector` through a Prometheus scrape parameter, e.g. `/probe?target=<TARGET>&collector=<COLLECTOR>`.
- Reach its sources itself, each through a request type (§ 5.1): HTTP/HTTPS endpoints, including authentication, custom headers, TLS configuration, timeouts, and HTTP methods (`http`); gRPC methods (`grpc`); Graphite's render API (`graphite`); files on the exporter's host (`localfile`) or on another over SSH (`sshfile`); a command's output on another host over SSH (`sshexec`); SQL databases (`sql`); DNS lookups (`dns`); TLS handshakes (`tlscert`); NTP servers' time (`ntp`); Kubernetes API listings (`kubernetes`); objects in S3 (`s3file`); the INFO and read-only commands of Redis and Valkey servers (`redis`); the answers of text-protocol services to a line sent over TCP, over UDP or a Unix socket, and whether a port takes a connection (`tcp`); searches of LDAP directories (`ldap`); the echo replies of hosts pinged over ICMP (`icmp`); and the objects and tables of SNMP agents, v1, v2c and v3 (`snmp`).
- Support multiple response decoders as first-class peers.
- Support JSON, NDJSON, YAML, TOML, INI, XML, CSV, HTML, logfmt, Prometheus exposition format, plain text, Graphite, binary layouts, and Python.
- Support jq transformations for JSON.
- Support yq transformations for YAML.
- Support XPath for XML/HTML.
- Support CSS selectors for HTML.
- Support regex-based extraction for plain text.
- Support full Python as a first-class decoder/transformation mechanism, not merely as a fallback.
- Normalize all decoder output into a common internal metric representation before Prometheus exposition.
- Provide explicit handling for fetch failures, an HTTP status or any other request type's, decoding failures, transformation failures, and missing fields/keys.
- Protect against excessive response size, execution time, metric count, label count, and other cardinality/resource abuse.
- Expose exporter self-metrics for scrape, decode, transform, and configuration health.

The design MUST remain extensible so additional request types, decoders or transformation engines can be added without changing the other layers or the Prometheus output layer.

---

## 2. Core design principle

Separate the system into these conceptual layers:

```text
Prometheus Operator
        |
        | target + collector
        v
     /probe
        |
        v
  Request type
 (http, grpc, sql, ...)
        |
        v
    Response
        |
        v
    Decoder
        |
        v
 Decoded Object
        |
        v
 Transformer / Metric Extraction
        |
        v
  []Metric / MetricSet
        |
        v
 Validation / Limits
        |
        v
 Prometheus exposition
```

Important:

- A request type is a first-class component: it reaches the source and hands
  the decoder a response, which every type's later layers share (§ 5.1).
- A decoder is a first-class component.
- Python is a first-class transform, not a decoder or fallback.
- Prometheus is a first-class decoder capable of converting an existing Prometheus endpoint into the internal metric model and then applying optional filtering/renaming/label transformations.
- No decoder should emit Prometheus text directly. All decoders MUST produce a common internal representation.

---

## 3. Target discovery and scrape API

### 3.1 No hardcoded targets

The exporter configuration MUST contain collectors only. It MUST NOT contain a list of target URLs.

Example:

```yaml
collectors:
  - name: elasticsearch_cluster
    request:
      type: http
    ...
  - name: legacy_application
    request:
      type: http
    ...
```

### 3.2 Probe endpoint

Implement:

```text
GET /probe?target=<TARGET>&collector=<COLLECTOR>
```

Parameters:

- `target`: the target address/URL supplied by Prometheus relabeling. What it
  may be is the collector's request type's to say (§ 5.1): `http` requires it;
  `localfile` accepts a file or directory under its root, or no target at all.
- `collector`: required collector name.
- `method`: optional request method override (`GET`, `POST`, `PUT`, `PATCH`,
  `DELETE`, or `HEAD`).
- `path`: optional request path override.
- `timeout`: optional positive Go duration controlling the underlying target
  request for this scrape.
- `body`: optional opaque request body override. It MUST be treated as raw
  text and MUST NOT require JSON encoding.
- `insecure_skip_verify`: optional boolean override for the collector's target
  TLS setting. When `true`, the target request MUST disable server certificate
  verification for this scrape only.
- `retry_attempts`: optional integer from 0 to 10 overriding the configured
  number of retries after the initial target request. `retry.attempts` in the
  configuration and a static target MUST be at most 10 too, so no caller can
  make one probe send a failing target requests in a tight loop.
- `retry_backoff`: optional non-negative Go duration overriding the fixed delay
  between retry attempts.
- `follow_redirects`: optional boolean overriding whether the target request
  follows HTTP redirect statuses.
- `enable_http2`: optional boolean overriding whether the target request may
  negotiate HTTP/2.
- `new_connection`: optional boolean overriding whether every request of the
  probe is sent on a connection of its own (§ 42.15b).
- `ip_family`: optional `ip4`, `ip6`, `prefer_ip4` or `prefer_ip6`
  overriding the address family the probe's requests connect over
  (§ 42.15b).

When the selected collector enables caching, the exporter MUST serve a stored
result instead of contacting the target while a cached entry for the identical
request is still valid. Section 42.13 defines caching.

The exporter MUST reject missing or unknown collector names with a useful error.

`/probe` MUST accept `GET` and `HEAD`; `HEAD` runs the probe in full and
answers its status and headers without the body. Any other method MUST be
answered `405 Method Not Allowed` with `Allow: GET, HEAD`, before the target
is contacted or the probe counted in the self-metrics. Authentication, where
configured (§ 42.5), is checked first. The `method` parameter above is the
target request's method and is unrelated.

Invalid request overrides MUST return a client error. When `timeout` is absent,
the exporter MUST use the incoming scrape request context as the target request
deadline rather than a collector-configured timeout.

`target`, `collector` and every parameter the collector's request type accepts
MUST be given at most once: a probe that gives one of them more than once MUST
be answered `400` naming the parameter, before the target is contacted or the
probe counted, rather than read for one of its values. `header_<name>` is the
exception: every value of it is forwarded (§ 42.4). The value of `target`,
`collector` and of every parameter the request type accepts, `header_<name>`
and `param_<name>` among them, MUST be at most 8 KiB (8192 bytes); a longer
one MUST be answered `400` naming the parameter and the limit, without
repeating the value, so what the exporter keeps about a probe after
answering it — a failure log entry (§ 25.1), a tracked request (§ 22.1) — is
bounded by what a probe may name. A value of a `header_<name>` parameter for a
header the collector forwards (§ 42.4) that holds a control character other
than tab MUST be answered `400` naming the parameter, before the target is
contacted or the probe counted, as a configured header value with one is
refused at load; it MUST NOT be sent, retried or logged as the target's
failure. A parameter no request type accepts MUST stay
ignored, repeated or long as it may be.

#### 3.2a Probe deadline

When a probe carries `X-Prometheus-Scrape-Timeout-Seconds`, as Prometheus sends
on every scrape, the exporter MUST bound the trip to the target — the request or
file read, decoding, transforms and scripts — by that timeout less
`--probe.timeout-offset`, which MUST default to 500ms and MUST reject a
negative value as a command-line error (exit 2, before `--dry-run` or startup).
A timeout above one hour MUST count as one hour, never overflowing.
When the offset would leave less than half the timeout, the probe MUST keep
half. When the budget runs out the probe MUST fail in the stage that was
running, and the error MUST say that the probe's budget, with its length, ran
out, so Prometheus receives the reason before its own timeout. A budget that
runs out while a metric rule is evaluated MUST fail the probe in the
`transform` stage whatever the rule's `error_mode` (§ 18.1). A missing,
unparseable, non-positive or non-finite header — `Inf` in any spelling, as
`NaN` — MUST leave the probe unbounded by it, and so bounded by
`--probe.default-timeout` below; it MUST NOT count as the longest timeout. A
positive finite header MUST always give a budget, however small: one that
rounds to no time at all, such as `1e-10`, MUST end the probe at once rather
than count as no header. The `timeout` parameter keeps bounding the request alone; the
earlier deadline wins.

A probe without a usable header, as from curl, a script or the collectors
page without its script, MUST be bounded the same way by
`--probe.default-timeout`, whatever `timeout` parameter it gives: the
parameter bounds the request within that budget and MUST NOT lift it. The flag MUST default to 30s,
MUST reject a negative value as a command-line error like the offset, and
leaves such a probe unbounded at 0. Its error MUST name that flag rather than
Prometheus's scrape timeout, so a probe that ran out says which deadline it
had. A target that accepts the connection and never answers MUST NOT be able
to hold a probe, and its collector's `max_concurrent_probes` slot, for ever. A cache hit needs no budget, and identical probes that
share one trip (§ 42.13a) share the budget of the probe that started it.
Static targets are not affected.

The `insecure_skip_verify` override MUST take precedence over
`request.tls.insecure_skip_verify` for the individual scrape. Invalid boolean
values MUST return HTTP 400 before the target is contacted. Disabling
certificate verification is an explicit security trade-off and MUST be
documented as unsafe for general use.

The collector MAY configure fixed-delay retries as follows:

```yaml
request:
  retry:
    attempts: 2
    backoff: 2s
```

`attempts` counts retries after the initial request and defaults to zero.
`backoff` defaults to zero and MUST NOT be exponential. The exporter SHOULD
retry transport failures and the HTTP statuses `retry.statuses` lists, which
are, when it is left out, the transient statuses `408`, `425`, `429`, and
`500` through `599`. Other HTTP statuses MUST be returned without retrying.
`retry.statuses`, for `http` and `graphite`, MUST be written and checked as
`accept_status` is (§ 5.1, `http`), statuses from 100 to 599 and classes such as
`5xx`, refused in the same words, and MUST NOT take `any`; a list MUST replace the default set, not add
to it, and an empty list MUST retry no status while a transport failure is
still retried. An entry every status of which the collector accepts — the
`accept_status` in force, every 2xx without one, every status from 100 to 599
with `[any]` — MUST be refused when the
configuration loads, naming the collector, or the static target, and the
entry, since it could never take effect; a class only some of whose statuses
are accepted MUST be taken, and an accepted status MUST NOT be retried.
`retry.statuses` MUST be refused for `grpc`, in a collector and a static
target, as `retry.codes` is for every other type; a static target's MUST
replace its collector's whole, and no probe parameter sets it. A debug report
of an `http` or `graphite` request with retries MUST say, after its requests,
how often, how far apart and on which statuses it is retried.
`retry.retry_after`, for `http` and `graphite`, MUST default to off, and off,
a `Retry-After` header MUST NOT change any wait. On, the wait before retrying
an answer of a retried status — one `retry.statuses` lists, that is not
accepted, with an attempt left — MUST be what its `Retry-After` asks for in
place of `backoff`: delta-seconds, one or more digits, or an HTTP date (RFC
9110, section 10.2.3), taken from the exporter's clock when the answer is
read, a date already past asking for no wait. A value that is neither — a
sign, a fraction, a date that does not parse — MUST be ignored and `backoff`
waited. When the wait asked would end at or after the request's deadline,
the exporter MUST NOT wait: that answer MUST be the request's answer at
once, and its `http_status` failure MUST say that it was not retried, with
the wait asked and the time left. The wait after a transport failure MUST
NOT read the header, and the resend of a login's `401` (§ 42.6a) MUST stay
as it is. `retry.retry_after` MUST be refused for `grpc`, in a collector and
a static target, as `retry.non_idempotent` is; a static target's MUST
replace its collector's and be part of its cache key, and no probe parameter
sets it. A debug report MUST say in its retry line that a `Retry-After`
replaces the backoff, and MUST show below each request retried under it the
wait taken and why — as `Retry-After` asked, or `retry.backoff` and why the
header was not used — or why it was not retried.
A refused target (§ 26.1) and response headers over their bound (§ 4) MUST
NOT be retried. An HTTP/2 connection closed for a protocol error before the
response's headers were read (§ 4) MUST be retried once, as one of
`attempts`, and MUST NOT be retried again when the retry ends the same way.
A connection that breaks while the body is read — a reset, a body shorter than
its `Content-Length` — MUST be retried as a transport failure is, unless the
probe's deadline is what ended the read, and a debug report MUST show the
attempt's status followed by the read error.
The retry loop MUST share the incoming scrape or explicit target timeout. When
that deadline, a shutdown or the caller ends the wait before a retry, the
exporter MUST keep what it received: after a retryable status, that response,
so the failure is reported in the `http_status` stage with the status and the
body; after a transport failure, that failure's error, saying the wait was cut
short. A bare deadline error MUST NOT replace them.
Only a request whose method is idempotent — `GET`, `HEAD`, `OPTIONS`, `TRACE`,
`PUT` or `DELETE` — MUST be retried, unless `retry.non_idempotent` is set, since
sending a `POST` or `PATCH` again may repeat what it did; this holds for a
method a probe parameter chooses too. A collector with `retry.attempts` and a
non-idempotent method, without `retry.non_idempotent`, MUST be reported as a
configuration warning. When a target answers with a status outside `2xx`, the
logged failure SHOULD carry the start of the body, at most 256 bytes on one
line, and the probe's answer MUST NOT. What in it reads as a credential — a
`Bearer` or `Basic` credential, a JSON Web Token, and the value of a field or
parameter whose name reads as a credential's by the one rule for a name
(§ 42.17), a JSON field's written as a string in JSON, its quotes escaped,
among them, and the text or CDATA content of an XML element whose local
name reads as one — MUST be masked before it is logged, including one the 256-byte
cut runs through.

The exporter MUST validate and normalize the target according to collector request configuration.

### 3.3 Prometheus Operator integration

The project MUST provide working examples for both `ServiceMonitor` and `PodMonitor`.

The examples MUST use relabeling to:

1. Preserve the discovered target in `__param_target`.
2. Set the Prometheus `instance` label from the target.
3. Rewrite `__address__` to the exporter service address.
4. Pass the collector name through `params.collector`.

Example conceptual flow:

```text
Pod/Service
    |
    v
PodMonitor/ServiceMonitor
    |
    v
Prometheus
    |
    +--> target=<pod-or-service-address>
    +--> collector=<configured-collector>
    |
    v
Generic exporter /probe
```

### 3.4 Multiple collectors over one exporter

A single exporter deployment MUST be able to serve many collectors and many discovered targets concurrently.

---

## 4. HTTP fetcher

The HTTP fetcher is owned by the Go exporter. Python code MUST NOT perform HTTP requests as part of normal operation.

Support:

- GET
- POST
- PUT if needed by collector configuration
- Configurable request path
- Query parameters
- Request body
- Custom headers
- Authentication
- HTTP basic authentication
- Optional file-backed basic authentication credentials
- Bearer token authentication
- A login before the request, whose token is sent on it: a JSON login or the
  OAuth 2.0 client credentials or refresh token grant (§ 42.6a)
- TLS CA configuration
- Configurable target certificate verification, including an explicit
  `insecure_skip_verify` opt-out
- Optional client certificates if practical
- Fixed-delay retries for transient target request failures
- Per-scrape timeout override through the probe request parameter
- Configurable maximum response size
- Configurable redirect following, disabled by default
- Configurable HTTP/2 negotiation, disabled by default
- HTTP status handling

Example:

```yaml
request:
  type: http
  method: GET
  path: /api/status
  query:
    detail: full
  headers:
    Accept: application/json
  body: |
    raw request body for a POST or PUT
```

Target URL and collector request path MUST be combined safely.

A target without a scheme MUST be treated as `http`. Service discovery
produces `__address__` as a bare `host:port`, and monitors pass it through as
the target, so this is the common case. The decision MUST be made on the text —
no `://` means no scheme — before the target is parsed, because a URL parser
rejects `10.0.0.5:8080` outright and reads `legacy.example:8080` as the scheme
`legacy.example`. Only a `://` before the first `/`, `?` or `#` of the target
MUST count as a scheme's: `h:8080/login?next=http://other/` has no scheme and
MUST be requested as `http`, its query unchanged. IPv6 literals in brackets and credentials in the userinfo
MUST survive. `allowed_schemes` MUST apply to the result, so a bare target is
never upgraded to `https`, and to the URL of every redirect followed: a
redirect to a scheme the collector does not allow MUST fail the request,
naming the scheme, before it is followed.

When neither `request.path` nor a `path` probe parameter is given, the target
MUST be requested exactly as given, including any path it carries. A path
ending in `/` MUST keep one trailing slash and never gain a second: `path: /`
on `http://h:5066` or `http://h:5066/` requests `/`, not `//`, which a Go
server redirects or refuses.

A percent-escape written in a request path — `%` and two hexadecimal digits,
in `request.path`, the `path` probe parameter or a static target's
`request.path` — MUST be sent as written, neither decoded nor escaped again:
`path: /projects/group%2Fproject` requests `/projects/group%2Fproject`.
Everything else in the path MUST be escaped as a path, and a `%` that begins
no escape MUST be sent as `%25`. An escaped dot segment (`%2E%2E`) MUST be sent
as written and MUST NOT be resolved. The escapes of the target's own path MUST
be kept likewise.

The target's own query MUST be sent byte for byte as it was written: a bare
key MUST NOT gain a `=`, a pair containing `;` MUST NOT be dropped, and the
pairs MUST NOT be reordered or escaped anew; only a space, a double quote,
`<`, `>` and a byte outside ASCII, which cannot be sent as they are, MUST be
percent-escaped where they stand. `request.query` MUST be appended
after it, encoded, and the parameters a request type builds itself after
that. A name both the target's query and `request.query` carry MUST be sent
twice, the target's first. A pair of the target's query naming a parameter
the request type builds itself (§ 5.1, `graphite`) MUST be dropped, every
other pair left as written.

A response's status line and headers MUST be bounded at 1 MiB, apart from
the body's bound (§ 20). A response over it MUST fail the request. Over
HTTP/1 the failure MUST be a limit error that says the headers were too
large, and MUST NOT be retried. Over HTTP/2 it MUST be that same limit
error, not retried either, where
the client reports the headers as too large, which it does for a header list
it reads to its end; a header list still arriving when it passes the bound
makes the client close the connection with the protocol error it reports for
any answer that breaks HTTP/2, which cannot be told from one, and every
request waiting on that connection is handed the same error. A request
whose HTTP/2 connection is closed for a protocol error before its response's
headers are read MUST therefore be retried once, within the collector's
`retry` settings (one of `retry.attempts`, after `retry.backoff`, and not
at all with `attempts: 0`), and the retry MUST be sent on a connection of
its own, one made for it, which no other request is put on and which is
closed once the answer is read, so that a request which only shared the
connection with the answer that closed it is answered, also when the request
that drew that answer is retried at the same moment: the two retries MUST
NOT meet on one connection again. A request that draws the error a second
time MUST NOT be retried again, whatever `retry.attempts` leaves. Where the
request ends with that error it MUST fail as a transport failure, not a
limit error, and its error MUST say that response headers over the bound end
this way.

Rendering a target for a log line or an error body MUST never fail, whatever
the probe sent. A target that cannot be parsed MUST be withheld rather than
echoed, since the credentials in it could not be located to redact them.

The exporter MUST expose HTTP response status and request duration in its self-metrics.

---

## 5. Collector configuration

Recommended top-level structure:

```yaml
rate_limits: []   # optional; rate limits per host for every collector (§ 5.1, http)
defaults: {}      # optional; what this file's collectors take for keys they do not set (§ 5.0b)
collectors:
  - name: example
    metrics_prefix: example   # optional
    name_escaping: fail       # optional: fail, underscores or values
    request:
      type: http
      ...
    response:
      ...
    decoder:
      ...
    transform:
      ...
    metrics: []
    error_handling:
      ...
    limits:
      ...
    cache:
      ttl: 60s
    coalesce: true   # optional; identical probes in flight share a request (§ 42.13a)
    max_concurrent_probes: 32   # optional; trips to the targets at once (§ 42.13b)
```

A collector MUST have a unique name. Uniqueness MUST hold across the
configuration's `collectors` and every collector file (§ 5.0): a name defined
twice, in one file or in two, MUST be refused at startup, by `--dry-run` and on
reload, with an error naming the collector and both files it was defined in.

### 5.0 Collector files

The configuration MAY list further files of collectors under `collector_files`:

```yaml
collector_files:
  - shared.yaml
  - collectors.d/*.yaml
```

- Each entry MUST be a non-empty path or glob pattern. A relative entry MUST be
  resolved against the directory of the configuration file, not the working
  directory, so a configuration means the same wherever the exporter is started
  from. A plain path MUST exist and MUST NOT be a directory. A pattern MAY match
  nothing, so an empty directory of collector files is valid.
- A file matched by more than one entry MUST be read once, and the
  configuration file itself MUST NOT be read as a collector file.
- A collector file MUST be a mapping whose only keys are `collectors`, holding a
  non-empty list, and `defaults` (§ 5.0b). Any other key — the exporter-wide `web` and `otlp` settings,
  a nested `collector_files`, a misspelling — MUST be an error naming the file,
  the key and its line, so a file of collectors can never change exporter-wide
  behaviour. An empty file and an empty list MUST be errors. Unknown keys inside
  a collector MUST be rejected exactly as in the configuration.
- The collectors MUST be merged in a defined order: the configuration's own,
  then each entry's files in the order listed, a pattern's matches in file name
  order. The merged list MUST then be validated as one: defaults, the rule
  checks of § 24.2 and the Python checks of § 16 apply to a collector from a
  file exactly as to one written in the configuration.
- A mistake validation finds in a collector that a collector file defines
  MUST name that file in its text, as `collector file <path>: collector ...`,
  at startup, in `--dry-run` and on reload, since that file, not the
  configuration listing it, is the one to edit. The log line of a rejected
  reload MUST carry as `file` the collector file when every mistake of the
  reload is in that one collector file — an error of reading it included —
  and the configuration file otherwise. A problem the Python checks of § 16
  find with such a collector is a mistake of that kind and MUST be reported
  the same way, at startup, in `--dry-run` and on reload, in the order of
  the collectors; which file a problem belongs to MUST NOT be read out of
  the problem's text.
- With `collector_files`, the configuration's own `collectors` MAY be omitted;
  the merged list MUST NOT be empty.
- `${NAME}` expansion (§ 42.15a) MUST apply to collector files whenever it
  applies to the configuration.

### 5.0b Defaults for the collectors of a file

The configuration file and each collector file MAY have a top-level
`defaults` block, which gives the collectors of that file, and of no other,
the keys of it they do not set themselves:

- `defaults` MUST take exactly the keys of `request.tls`, `request.retry`,
  `request.max_response_bytes`, `request.allowed_targets`,
  `request.denied_targets`, `request.proxy_url`,
  `request.proxy_basic_auth_file`, `limits`, `error_handling`,
  `failure_backoff` and `cache`, each of the kind a collector's takes. A
  collector that writes a `proxy_url` of its own MUST leave out the
  `proxy_basic_auth_file` the block gives it when the block sets
  `proxy_url` too, whatever proxy it names or `direct`: those credentials
  are for the block's proxy alone, and MUST NOT be sent to another. Given
  without a `proxy_url`, the block's credentials MUST go to the collectors
  that name their own proxies, and a collector that writes `proxy_url:
  direct` MUST leave them out. A collector leaving the credentials out MUST
  NOT list `request.proxy_basic_auth_file` under `--dry-run`'s
  `details.collector_defaults`, and credentials no collector takes MUST be
  refused as a key that applies to no collector, saying that each collector
  names its own proxy or goes direct. A collector with a `request.unix_socket`
  (§ 42.15b) MUST take neither `proxy_url` nor `proxy_basic_auth_file` from
  the block, and a block's proxy key that only such collectors would take
  MUST be refused as one that applies to no collector, saying so. One that writes the credentials itself
  beside `direct` MUST be refused. Any other key — of a
  collector, of a request type's own, `request.type`, or of nothing — and a
  value of the wrong kind MUST be refused when the configuration loads, at
  startup, by `--dry-run` and on reload, naming the key and its line, and the
  collector file when it is in one.
- A collector MUST take a key of the block exactly when it does not write
  the key itself, at every depth, whatever value it writes but `""`, which
  is the key left out, in the collector and in the block alike: one that writes
  `limits.max_metrics` keeps the block's other limits. A list MUST be one key,
  replaced whole and never joined. A `request.tls` or `failure_backoff`
  written in the block, even empty, MUST be taken as written by a collector
  that does not write it. A collector that writes `failure_backoff` with
  `enabled: false` MUST take none of the block's `failure_backoff` keys, and
  the block's own `failure_backoff.enabled: false` MUST be refused as
  `defaults.failure_backoff.enabled ...`. What a merge key (`<<`) brings in MUST count as
  written where it is merged, as the decoder reads it.
- A key of `request` MUST be given only to the collectors whose request type
  takes it (§ 5.1), and a key of `retry` only where it applies to the type, so
  collectors of several types can share a file. A key of the block that no
  collector of the file takes — because no type of theirs takes it, or each
  collector that would take it writes it or turns `failure_backoff` off — MUST
  be refused naming the key and why, since a setting nothing uses would be
  silently ignored, and a key of a `request.tls` or `failure_backoff` block
  refused so MUST NOT be refused again on its own; so MUST a
  `defaults` block in a configuration file that defines no collectors of its
  own. The defaults of one file MUST NOT reach the collectors of another.
- The block MUST be given before the collectors are validated, and a
  collector MUST be validated, fingerprinted (§ 42.13) and compared on a
  reload (§ 24.1a) with the values it took as if it had written them: moving
  a value from every collector into `defaults` MUST change no collector, and
  changing a default MUST change each collector that takes it. A value of
  the block that is wrong whatever collector takes it — a limit out of its
  range, an unknown error policy, retry attempts or backoff out of range, a
  `failure_backoff` or `cache` value the collector checks refuse — MUST be
  refused once, as `defaults.<key> ...`, and not once for each collector,
  and the block then given to none; the collectors of its file MUST then not
  be validated, since without the block they would be refused for values it
  gives them. A problem validation finds with one collector in a value it
  took, and a warning it gives about one, MUST end by saying so, as `(<key>
  comes from the defaults of the file)`. The problems with the block's keys
  and values of the wrong kind MUST all be reported, in the order of the
  file, those of the blocks that check their own keys among the others.
- The request keys a collector takes MUST follow its own in the order its
  request block's keys are checked in (§ 5.1), in the order the block has
  them, so that of several keys refused the error names one the collector
  writes first, the same at every load.
- `--dry-run` MUST list under `details.collector_defaults`, for each
  collector that took any, the keys it took (§ 30.1). `${NAME}` expansion
  (§ 42.15a) MUST apply to the block as to the rest of its file, and the
  target allocator's bundle (§ 42.14d) MUST carry the files as written,
  `defaults` and all. A collector
  written on the debug page MUST refuse a `defaults` key, which is a key of
  a file.

### 5.0a Metrics prefix

A collector MAY set `metrics_prefix`. When set, the exporter MUST join it with
`_` to the front of every metric the collector exports: `metrics_prefix:
grafana` and a metric `statuspage_status` export `grafana_statuspage_status`.
Unset or empty, names MUST be exported exactly as declared.

The prefix MUST match `^[a-zA-Z][a-zA-Z0-9]*(_[a-zA-Z0-9]+)*$`: a letter, then
letters and digits in parts joined by single underscores. This rules out a
leading underscore (names beginning `__` are reserved by Prometheus), a trailing
one (the joining `_` would double it), `__` anywhere, `:` (reserved for
recording rules), a leading digit, and anything outside ASCII letters, digits
and `_`. The exporter adds the separator rather than expecting it in the
prefix, so a prefix is a name fragment and cannot produce a malformed name.

An invalid prefix MUST be rejected at startup, on reload and by `--dry-run`,
with a message naming the collector, the rule, an example, and that the
separator is added. A prefix that leaves no room for a name within
`limits.max_metric_name_length`, and a declared metric whose prefixed name
exceeds that limit, MUST be rejected the same way, as a rule's name without a
prefix that exceeds it is (§ 21.1); a name only known at scrape time MUST be
checked against the limit with every other exported name (§ 21).

The prefix MUST be applied in one place, to the output of whichever transform
ran — declared metrics, names emitted by a Python script, and names passed
through or renamed by a `prometheus` transform alike — and before the limits
are checked, the result is cached, or it is written to `/probe` or queued for
OTLP, so every consumer sees the same names. A histogram or summary keeps its
family under the prefixed name. The response cache MUST be keyed on the
collector definition including the prefix. The exporter's own `http_exporter_*`
metrics, including static target health metrics, MUST NOT be prefixed. Logs
and probe errors MUST name a metric rule as configured, without the prefix.

### 5.0a Sizes

Every setting that is a number of bytes — `request.max_response_bytes`,
`limits.max_response_bytes`, `limits.max_output_bytes`,
`limits.max_script_memory` and
`request.max_total_bytes` — MUST accept a YAML number that is a whole number
of bytes, or a string of
a number, optionally with a fraction, followed by an optional space and unit:
`B`; `kB`, `MB`, `GB`, `TB` as powers of 1000; `KiB`, `MiB`, `GiB`, `TiB` as
powers of 1024. The unit MUST be case insensitive, the `B` MAY be left out, and
a fraction of a unit MUST be rounded down to whole bytes. Anything else MUST
fail to load naming the value and its line, and MUST NOT be read as some
other size or as the default: a negative number, as a YAML number or in a
string; an exponent in a string, or an unknown unit; a number with a fraction
and no unit,
such as `1.5`; and a size of 2^63 bytes or more, with a unit (`8388608TiB`)
or without, which is past what a size holds; and a string with space before
or after the size. A whole number of bytes MUST be read exactly, up to
2^63 - 1. A number YAML reads as one — written without quotes — MUST be the
whole number of bytes it equals however it is written: with an exponent
(`1e6`, `2.5e8`), with a fraction of zero (`1048576.0`) and with a sign
(`+1e6`), as a key of a whole number reads it (§ 24.2) and as a schema takes
it, which is handed the number and not its spelling. It MUST be read by its
digits and not as the floating-point number nearest to it:
`9007199254740993.0` is that many bytes, `9.223372036854775807e18` is the
largest size, and a fraction too small for a floating-point number to hold
is one. A number with a fraction (`1.5`, `1e-1`), a negative one (`-1e3`),
one of 2^63 or more (`1e19`), infinity and not-a-number MUST be refused,
and so MUST the same spellings in quotes:
text is held to the pattern, so `"1e3"` and `"1.0"` are no size. A whole
number written with a point or an exponent that no size holds MUST be
refused for what it is, with the message of the same number written as an
integer and shown as it is written: one under 0 (`-1e3`, `-1.0`) as
negative, and one of 2^63 or more (`1e19`, `9223372036854775808.0`) as too
large, neither as a spelling that is no size or as a number with a
fraction. Zero is not negative whatever its sign (`-0.0`), and a negative
number with a fraction (`-1.5`) is refused as the spelling it is. Reading a
number this way MUST NOT change what any other spelling is read as or
refused with, which a
test MUST show against the reading as it was over generated spellings.
The configuration schema MUST accept both forms for these
settings, and its pattern for the string MUST accept exactly the strings
the exporter does but for the range, which the exporter alone checks; a
test MUST hold the pattern and the exporter to the same verdict over a
table of spellings. The schemas and the exporter MUST give one verdict on a
number too, in every way YAML writes one, at every size key of the
configuration and of a collector file, which a test MUST find by the
pattern, but for what a schema cannot tell of the number it is handed: its
range, and a fraction written past the digits a floating-point number
holds, such as `1.00000000000000000001`, which reaches a schema as a whole
number and which the exporter MUST refuse.

### 5.1 Request types

Every collector MUST declare `request.type`, which selects how it reaches its
data. `http`, `localfile`, `graphite`, `grpc`, `sshfile`, `sql`, `dns`,
`tlscert`, `ntp`, `s3file`, `kubernetes`, `redis`, `tcp`, `ldap`, `sshexec`,
`icmp` and `snmp` are implemented;
FTP is anticipated. The
type MUST be required rather than defaulted, so that no configuration means
`http` by accident. A missing type
MUST be rejected at startup and on reload with a message naming the collector,
stating that the type is required, listing the supported types and showing
`type: http` — or, in a build that left `http` out (below), the first of the
types it carries, never one the build would then refuse; an unknown type MUST
be rejected with the supported types listed.
The value MUST be written in lower case and without blanks around it, as the
schemas take it (§ 24.3): one written otherwise, `HTTP` or ` http `, MUST be
rejected, the message saying how the type it spells is written.

Each type MUST own, and the implementation MUST keep in one registry:

- the request keys it accepts. A key set on a collector that its type does not
  accept MUST be rejected, naming the key and the type, so a configuration can
  never carry a setting that is silently ignored. Keys MUST be checked before
  the type fills in defaults, so a default is never mistaken for something the
  author wrote. Of several keys a request block sets that its type does not
  take — keys of the common part, keys of other types, a key of `retry` that
  applies to other types, and unknown keys — the error MUST name the first in
  the file, whatever refuses each: an unknown key after a key the type does
  not take is refused once that key is gone, and one before it as an unknown
  key (§ 24.2a). The first in the file is the one on the earlier line, or
  further left on the same line; a key a merge key brings in is where its
  anchor has it. So MUST a collector file's, and so MUST each check that
  refuses several keys alike name the first in the file, never in an order
  that changes from one load to the next: `keepalive_interval` and
  `idle_timeout` negative, the credential files blank or with a
  placeholder, `label_selector` and `field_selector` blank, a login's
  `token` and `expires_in` blank, and a login's keys of the other type;
- the `/probe` parameters it accepts (§ 42.10). A parameter that some type
  accepts but the collector's type does not MUST be rejected with
  `400 Bad Request` naming the parameter and the type, before the target is
  contacted. A parameter no type accepts is not an override and MUST be ignored,
  as it always has been, since a monitor may carry parameters of its own;
- the keys a static target's `request` block may set (§ 42.14). A key its
  collector's type does not accept MUST be rejected at startup naming the
  target, the key, the collector and the type, and of several such keys, a
  key of `retry` among them, the first in the file. An unknown key is refused
  when the file is read, before its collector is known (§ 24.2a);
- its own validation — required keys, defaults and cross-key rules;
- its own fetch. Decoding, transforms, error policies, limits, caching and
  exposition MUST be shared by every type, so a new type adds only how bytes
  are obtained;
- what a target is for it: whether a probe or static target may leave
  `target` out, how a target is checked — a probe's failing check answered with
  `400` before anything is fetched, a static target's at load — how a target
  is shown in logs, error bodies and the `target` label of static target health
  series, the `url` and `http_method` labels of its verbose self-metrics
  (§ 22.1), and the name of the stage a failed fetch is reported under.

Every key of the request block, and of a static target's request block, MUST
be accepted by at least one type, and a test MUST enforce it, so a key cannot be
added without deciding which types it belongs to.

#### Selecting request types at build time

Which request types a binary carries MUST be decided at build time, so a build
can leave out types it does not need together with the code and libraries only
they use. A default build — no tags, and the published image — MUST carry every
type but the opt-in ones. An opt-in type is one few deployments would want
in the binary, for the libraries it links; there are none now, every type
of the tree being in the default build and the published image. No build
MAY carry an opt-in type unless asked for it by its tag, and the source MUST
list the opt-in types.

- Each type MUST live in its own `internal/fetch/requesttype_<name>.go`, which registers it
  from `init` and carries exactly the constraint
  `//go:build !select_request_types || request_type_<name>`, or, for an
  opt-in type, exactly `//go:build request_type_<name>`, so that its tag
  adds it to a default build and to a selective one alike. Code and imports
  that only that type needs MUST live behind the same constraint, and so MUST
  `requesttype_<name>_test.go`, the tests that only that type needs, so that
  vetting a single-type selection compiles its tests too.
- What the rest of the exporter knows of a type MUST be part of the type's
  registration (`fetch.RequestType`): what every type does as its
  `Requester` interface (validating a request, fetching, checking,
  labelling and displaying a target), and what only some types have, or
  what the core needs to know, as a field of its own, such as the
  self-metric families its collectors have, its target hint, its decoder,
  its rule in the configuration schema, the templated parts of its request
  and whether the debug page writes it. The core (package `main`,
  `internal/config`, `internal/decode`, `internal/exporter` and
  `internal/transform`) MUST read these from the registry and MUST NOT name
  a request type, so that adding a type touches its own files and the build
  wiring alone. A test MUST find no `RequestType<Name>` constant and no
  string literal that is a request type's name in the core's non-test
  files, but for an exact allowlist of the same words meaning something
  else, each with its reason. The encoding of a templated field of one
  type, which filters it takes and whether a probe's overrides replace it
  MUST be registered by that type for the field's kind, not keyed by the
  field's place in the shared template code, and a test MUST find no
  comparison of a templated field's place with a literal in a file of
  `internal/fetch` every build compiles.
- A request key that only one type accepts MUST be that type's own: read
  into a struct its registration declares, behind the type's constraint,
  so that a build without the type does not carry it. The request block's
  common part MUST hold only `type` and the keys that several types accept,
  and a test MUST enforce it. The configuration MUST stay as it is written,
  a type's own keys flat beside the common ones under `request`. Every build
  MUST know the own keys of every type of the tree, from a catalogue
  generated from a build with every type: a key of a type the build left out,
  set under another type, MUST be rejected as a key the type does not accept,
  in the same words as in a build with every type, and the configuration
  schema MUST describe it as that build's does.
- Code that some types share and others do not, such as the rules of a
  request made over HTTP, which `http` and `graphite` share, or the credential
  keys' rules, which `http`, `graphite` and `grpc` share, MUST live behind a
  constraint naming each type that uses it, so no selection compiles code it
  does not use.
- A library only one type uses MUST be imported only by files behind that
  type's constraint, so a build without the type does not link it; a test
  MUST list the packages a build of each other type links and find none of
  it. The gRPC and protobuf modules are `grpc`'s alone,
  `golang.org/x/crypto/ssh` is `sshfile`'s and `sshexec`'s, and
  `github.com/pkg/sftp` `sshfile`'s alone: only files behind a constraint
  naming the types that use one MAY import it, the stand-in SSH server of
  their tests (`internal/sshtest`) included, and the test MUST also find
  each type's modules in a build of that type and in the default build. What
  `sshfile` and `sshexec` share — the SSH connection, its keys and its
  target — MUST live in files built with either, which import no SFTP, so
  that a build of one carries none of the other's own code. The
  database drivers are `sql`'s, each imported only by its own file (§ 5.1,
  `sql`), and the test MUST find none of them in a build without `sql`, the
  three default drivers and none of the opt-in ones in a build with it, the
  default build included, and an opt-in driver's modules — Snowflake's AWS
  and Azure SDKs and Apache Arrow among them — only in a build that asks for
  that driver. `dns`, `tlscert`, `ntp`, `kubernetes`, `redis`, `tcp`, `ldap`, `icmp` and `snmp` MUST
  import nothing outside the standard library and the module's own packages
  — no Kubernetes, Redis or LDAP client library, the stand-in servers of
  `redis`'s and `ldap`'s tests (`internal/redistest`, `internal/ldaptest`)
  and of `tcp`'s and `snmp`'s (`internal/testutil`) included — and the test MUST find none of the other
  types' modules in a build of any of them, and in a build of `tcp` alone no
  package a build of no request type lacks but the standard library's. The AWS SDK's modules, `github.com/aws/aws-sdk-go-v2` and
  `github.com/aws/smithy-go`, are `s3file`'s among the types: the test MUST
  find them in a build of `s3file` alone and its S3 client and
  configuration in the default build, and none of them in a build of any
  other one type, nor in a build of every type but `s3file`; the `snowflake` driver links the
  AWS SDK too, which only its own build may.
- A build with `-tags select_request_types` MUST carry only the types named by
  `request_type_<name>` tags. One that names none MUST fail to compile, which
  `requesttype_none.go` does with the constraint
  `select_request_types && !request_type_<a> && !request_type_<b> ...`
  listing every type.
- The source MUST keep a list of every type in the tree, whether or not the
  build carries it. A collector naming a type that exists but was left out of
  the build MUST be rejected with a message saying so, listing the types the
  build carries and naming the tag that would include it, and, for an opt-in
  type, saying that it is one, which the default build and the published
  image leave out, and naming `OPT_IN_REQUEST_TYPES`; a name that is no
  type at all keeps the unknown-type message.
- `tools/request-type-tags.sh` MUST turn a comma-separated `REQUEST_TYPES` list
  into those tags, printing nothing for an empty list and failing on a name
  that is not a type in the tree, and a second list, `SQL_DRIVERS`, into the
  `sql_driver_<name>` tags of the `sql` type's opt-in drivers, failing on a
  name that is no opt-in driver of the tree — a default driver included —
  and on drivers with a `REQUEST_TYPES` that leaves `sql` out, and a third
  list, `OPT_IN_REQUEST_TYPES`, into the `request_type_<name>` tags of
  opt-in types, added to the default build's types or to those
  `REQUEST_TYPES` lists, failing on a name that is no opt-in type of the
  tree, a type of every build included. `REQUEST_TYPES` MUST take an opt-in
  type as any other. The Dockerfile's `REQUEST_TYPES`, `SQL_DRIVERS` and
  `OPT_IN_REQUEST_TYPES` build arguments and the Makefile's variables of
  those names MUST all use it.
- `make new-request-type NAME=<name>`, with `OPT_IN=1` for an opt-in type,
  MUST add a request type to the tree with `tools/new-request-type.sh`,
  POSIX `sh`, `sed` and `awk` that macOS's run: it MUST refuse a name that
  is not lowercase letters and digits starting with a letter, or that is a
  type of the tree, before it writes anything; and it MUST write the type's
  file, under its constraint, with a `Requester` whose `Fetch` answers that
  the type is not implemented yet and an empty struct of its own keys, the
  type's tests in `internal/fetch`, `internal/exporter` and its table of
  schema rows, which pass, and `docs/<NAME>.md` with the sections of the
  other types' pages, and add the type to every list in the tree that names
  each type, then run `make schemas`. What it writes MUST put the article
  the name is read with before it, "an" before an initialism whose first
  letter's name starts with a vowel sound or a word starting with a vowel
  (`an ntp collector`, `TestAnNTP…`), "a" otherwise, an initialism being a
  Go name with two capitals or more at its start or a name without a
  vowel; `ARTICLE=a` or `ARTICLE=an` MUST override it. A type of the default
  build MUST be added to the documentation's lists that name the default
  build's types and then the opt-in ones after the last of the default
  build's, and an opt-in type last; the sorted lists of a default build's
  types the documentation quotes from the exporter's messages MUST name a
  type of the default build. Each place left to a person MUST say `TODO(<name>)`, and
  the script MUST end by printing what is left, pointing to the list in
  `docs/DEVELOPMENT.md` of the prose that names the types, which it cannot
  write. A test MUST fail while a `TODO(<name>)` of a type of the tree, or
  the not-implemented failure in the type's file, is left anywhere in the
  tree. A test MUST run it on a
  copy of the tree, for an opt-in type and a type of every build, and find
  that the copy builds and vets in the default build, with every opt-in
  type and with each new type alone, that the generated tests pass, that
  the tests comparing the lists of the types pass, that the article and
  the places in the lists are as above, and that the test of what is left
  fails in it. `make ci` MUST NOT run it.
- The types the binary carries MUST appear on the startup log line and in the
  `--dry-run` report (§ 30.1), and the `sql` type's drivers on the startup
  log line, as `sql_drivers`, empty in a build without the type.
- CI MUST vet and build each single-type selection as well as the default
  build, so no type depends on code only another type compiles;
  `make build-request-types`, which `make ci` includes, MUST run the same
  commands. `make build` MUST build the main package into
  `bin/prometheus-universal-exporter`, with the tags of `REQUEST_TYPES`, and
  the documentation that gives it MUST say where the binary lands, and MUST
  warn when `./prometheus-universal-exporter` or `./pue-allocator`, which
  a plain `go build` leaves in the repository root and `.gitignore` MUST
  ignore, is there. `make run EXAMPLE=<name>` MUST build as `make build`
  does, with the opt-in request types the example's files name on a
  `type: <name>` line added to `OPT_IN_REQUEST_TYPES`, so that the binary
  it runs is never older than the tree, and MUST then print the URL of the
  landing page, of `/static-targets/status` for every example, saying it
  shows the static targets' last scrapes for an example with static targets
  and the recent probes always, and of the first probe that the comment
  opening the example's configuration gives as `curl '<url>/probe?<query>'`,
  on `HOST:PORT`, when the collector it names is one of the example's (none
  otherwise) and its target is one a local run can reach — a host of
  `localhost`, `127.0.0.1` or `[::1]`, or a DNS name with a dot that is no
  placeholder: not one whose first label ends in `-host`, not an IP address
  but the loopback ones, not under `example.com`, `example.net`,
  `example.org` or the top-level domains `example`, `invalid`, `test`,
  `local`, `localhost`, `internal`, `lan`, `home`, `corp` or `svc` — and
  otherwise a hint, the same probe with `<your target>` for its target, and
  run `bin/prometheus-universal-exporter` with
  the example's configuration, its static target file when it has one, the
  listen address `HOST:PORT`, `127.0.0.1:8080` unless the command line sets
  them, and the flags of `ARGS`. A bare IPv6 `HOST` MUST be bracketed, in
  the listen address and the URLs (`[::1]:8080`), an already bracketed one
  kept; for `HOST=0.0.0.0` and `HOST=::` the URLs MUST be on `localhost`
  and `[::1]`, which a browser on the same machine can open, and the run
  MUST say the exporter listens on every interface; an empty `HOST` MUST
  fail with a message before anything is built. An example's name MUST come from the tree
  (`tools/run-example.sh`): a directory under `examples/` is named by the
  directory, its configuration being its `config.yaml` and its other YAML
  files its static target file and collector files, which MUST NOT be
  taken for a second configuration, `examples/config.<name>.<anything>.yaml`
  by `<name>`, and a name two examples share MUST fail, as MUST every use
  of the script, `list` among them, while the tree has a directory of YAML
  without a `config.yaml` or with two static target files; `make run` without `EXAMPLE` MUST list
  the names and exit non-zero, and an unknown name MUST fail before
  anything is built. `make ci` MUST NOT reach `run`. CI MUST
  also vet, build and test (the `internal/fetch` package) a build with every
  opt-in `sql` driver, which `make build-sql-drivers` and
  `make test-sql-drivers`, which `make ci` includes, MUST run the same; CI
  MUST lint that build with golangci-lint too, as `make lint-sql-drivers`,
  which `make ci` includes, does; a test MUST keep these lists naming every
  opt-in driver of the tree. CI MUST likewise vet, build, lint and test (the
  `internal/fetch` and `test/repository` packages) a build with every
  opt-in type beside the default build's types, as `make
  build-opt-in-types`, `make lint-opt-in-types` and `make
  test-opt-in-types`, which `make ci` includes, do, and a test MUST keep
  their lists naming every opt-in type of the tree, and, while the tree has
  none, saying so and stopping; each single-type
  selection, an opt-in type's included, is vetted, built, linted (§ 34.33)
  and tested as below.
- Every single-type selection MUST pass its tests, and CI MUST run them:
  `go test` once under each selection's tags, over the exporter's packages
  when they are to run (§ 34.33), which `make test-request-types` runs over
  every package locally and `make ci` includes. A test that needs a request type —
  its configuration names the type, it stands in for a target of the type,
  or it asserts what only a build with the type does — MUST say so with a
  build constraint on its file, `!select_request_types || request_type_<name>`,
  with `(request_type_<a> && request_type_<b>)` in place of the one type
  where it needs two, and `!select_request_types` alone where it holds only
  with every type; it MUST NOT be skipped at run time instead. A test of one
  type MUST NOT carry a constraint of two for the sake of a row or an
  assertion that contrasts the type with another: what needs the type alone
  MUST be a test in a file with the type's constraint, which the type's own
  selection runs, and the contrast a test of its own in a file with the
  constraint it needs — the other type alone, or the two together where it
  holds only with both built. A test that holds whatever the build carries
  MUST stay without a constraint, so every selection runs it, and a test
  helper MUST be compiled exactly where something that calls it is.
- A build that left a type out MUST behave as if the type did not exist,
  apart from the message above for a collector that names it: a `/probe`
  parameter that only left-out types accept is a parameter no type accepts
  and MUST be ignored as one, and the schema the binary prints (§ 24.3)
  lists only its own types. The committed schemas are those of a default
  build, of every type but the opt-in ones and the sql type's default
  drivers alone, and the test that compares them with what the code
  generates MUST run in that build alone, not in one with an opt-in type or
  sql driver, whose schema names that type or driver as its loader takes it.
  An opt-in type's request keys are struct keys, in every build's schema;
  the default build's test that every request key belongs to a type MUST
  know them without the type, and a test in a build with the type MUST keep
  that list the type's own.

#### `http`

`type` is the only required key. Everything else is optional:

| Key | Default | Rule |
| --- | --- | --- |
| `method` | `GET` | One of GET, POST, PUT, PATCH, DELETE, HEAD, in upper case or lower case, as the schemas take it, and sent in upper case; `Get`, and a method with blanks around it, MUST be refused naming the methods. A static target's `method` alike. |
| `path` | none | Joined onto the target URL; may carry path parameters (§ 42.10a). Empty is valid, since the target URL may carry the whole path. A percent-escape in it MUST be sent as written (§ 4). |
| `query` | none | Query parameters added to the request, after the target's own query (§ 4). A name MUST NOT be empty or hold a control character, either of which MUST be refused at load; any other name is sent escaped. |
| `headers` | none | Headers sent to the target. |
| `body` | none | Raw request body. |
| `basic_auth` / `basic_auth_file` | none | Mutually exclusive; the file form needs both paths. A username with a colon MUST be refused (§ 42.6). |
| `bearer_token` / `bearer_token_file` | none | Mutually exclusive, and exclusive with basic authentication. |
| `login` | none | A login made before the request, whose token is sent on it (§ 42.6a). |
| `forward_authorization` | `false` | Forward the probe's `Authorization` header (§ 42.4). |
| `forward_headers` | none | Allowlist for `header_<name>` probe parameters (§ 42.4). |
| `tls` | verify | CA, client certificate and `insecure_skip_verify` (§ 42.9). |
| `retry` | none | `attempts` and `backoff`, both non-negative; `non_idempotent`; `statuses`, the statuses retried, `408`, `425`, `429` and `5xx` when left out; `retry_after`, off by default, waiting a retried answer's `Retry-After` in place of `backoff` (§ 3.2a). |
| `max_response_bytes` | 10 MiB | Response size cap (§ 20). |
| `follow_redirects` | `false` | § 42.15. |
| `redirect_trusted_hosts` | none | Hosts a followed redirect may carry the request's headers, credentials and body to, and present its TLS client certificate to, besides its own origin (§ 42.15c). |
| `enable_http2` | `false` | § 42.15. |
| `new_connection` | `false` | Every request on a connection of its own, closed after its answer (§ 42.15b). |
| `ip_family` | both | The address family the requests connect over: `ip4`, `ip6`, `prefer_ip4` or `prefer_ip6` (§ 42.15b). |
| `allowed_schemes` | `http`, `https` | Schemes a target may use. Each entry MUST be `http` or `https`, in any case; any other, including an empty one and one with a space or `://` around it, MUST be refused at load naming the collector and the entry. |
| `accept_status` | every 2xx | A static target MAY set its own, replacing the collector's for it, checked at load and part of its cache key; so MAY a `grpc` target for `accept_codes`. Statuses whose answers are decoded: numbers from 100 to 599 and classes such as `2xx`, written as YAML numbers or strings, or `any` as the only entry, which MUST accept every status from 100 to 599 and no other; any other entry, `any` beside another entry and `any` in another case MUST be refused at load, naming the collector or the static target, and the schemas MUST refuse them too. An entry YAML reads as a number MUST be the status it equals however it is written — with a point, an exponent, in hex or octal, with a sign or leading zeros: `503.0`, `5.03e2`, `0x1F7`, `0o767`, `+503` and `0503` are 503 — read by its digits, as a schema reads it, in the configuration, a collector file and the static target file; an entry in quotes MUST be read as the text it is, and MUST be taken exactly when the schemas' pattern `^[1-5][xX][xX]$|^[1-5][0-9][0-9]$` takes it: a status's three digits or a class in either case; `"503.0"`, and text with a sign, leading zeros or blanks (`"0503"`, `"+503"`, `" 503 "`), MUST be refused at load with a message naming the entry and that form. The same holds for `retry.statuses`, a collector's and a static target's. A response with another status MUST fail in the `http_status` stage, and an accepted status MUST NOT be retried. |
| `allowed_targets`, `denied_targets` | none | Hosts, globs, addresses and networks its requests may and may not reach (§ 26.1). |
| `pagination` | none | Fetch the further pages of a paginated answer and merge them into one document (below). |
| `rate_limit` | none | The most requests a second sent to one origin of the collector's targets (below). |
| `proxy_url` | the environment's | The proxy every request of the collector goes through, or `direct` (§ 42.15b). |
| `proxy_basic_auth_file` | none | The files of the username and the password sent to `proxy_url` (§ 42.15b). |
| `fastcgi` | none | Send the requests as FastCGI requests to the URL's host and port, or `unix_socket`, in place of HTTP (§ 42.15b). |
| `unix_socket` | none | The absolute path of the Unix domain socket every request of the collector is sent over in place of a TCP connection to the URL's host (§ 42.15b). The `http` type's and `tcp`'s (§ 5, `tcp`), and of the request's common part. |

It MUST accept these `/probe` parameters: `method`, `path`, `timeout`, `body`,
`insecure_skip_verify`, `follow_redirects`, `enable_http2`, `new_connection`,
`ip_family`, `retry_attempts`, `retry_backoff`, `header_<name>` and `param_<name>`. A static
target using an `http` collector MAY set `method`, `path`, `body`, `timeout`,
`insecure_skip_verify`, `follow_redirects`, `enable_http2`, `new_connection`,
`ip_family`, `retry`, `headers`,
the basic and bearer credential keys, `accept_status`, `login` and
`rate_limit`. No probe parameter MAY set or change `login` or `rate_limit`.
Neither a probe parameter nor a static target MAY set `proxy_url` or
`proxy_basic_auth_file`, nor `unix_socket`, which `defaults` MUST NOT set
either.

##### Pagination

`request.pagination` MUST fetch the pages of a paginated answer after the
first and merge them into one JSON document, which the decoder, the
transform and the limits MUST read as the trip's one answer: the first page,
with the array `items` names holding the items of every page, in order. It
is the `http` type's alone, and neither a static target nor a probe
parameter MAY set or change it. Its keys:

| Key | Default | Rule |
| --- | --- | --- |
| `type` | none | Required: `link`, `next_url`, `cursor`, `page` or `offset`, in lower case and without blanks around it; any other spelling MUST be refused naming the five. |
| `items` | none | Required: a jq path to the array of a page's items, such as `.items` or `.`. An expression that is not one path of keys and indexes — evaluated against `null` at load, it fails or names other than one place — MUST be refused at load. |
| `next` | none | Required with `next_url` and `cursor`, refused with the others: a jq expression over the page, with `$status` and `$headers` bound as in a rule, giving the next URL or cursor. |
| `param` | none | Required with `cursor`, `page` and `offset`, refused with the others: the query parameter the cursor, page number or offset is sent in. A name with a control character, and a `request.query` key that is the same name in any case, MUST be refused at load. |
| `start` | 1 for `page`, 0 for `offset` | `page` and `offset` only, not negative: the first page's number or offset, sent on the first page. `start: 0` with `page` MUST number the pages from 0. |
| `size` | the items received | `offset` only, not negative: how far the offset moves from one page to the next. `0` MUST be taken as the key left out, beside any type, the offset moving on by the items the page held. |
| `max_pages` | 10 | From 1 to 1000: the most pages one trip fetches, the first included. |

A key that does not apply to the type MUST be refused at load naming it and
how the type finds its next page, and so MUST `pagination` with method
`HEAD`, whose answers have no body. Every refusal MUST name the collector.
The pages are read as JSON: a collector with `pagination` MUST decode with
`json`, a `decoder.type` left out or `auto` MUST become `json`, and any other
MUST be refused at load.

The next page MUST be: for `link`, the target of the `Link` header's link
whose first `rel` lists `next`, compared without regard to case (RFC 8288),
a comma or semicolon inside a quoted value separating nothing and a malformed
link skipped without hiding the others; for `next_url`, the URL `next` gives;
both resolved against the URL of the page that names them, without a
fragment. For `cursor`, it MUST be the URL of the page before with `param`
set to the text `next` gives, a number as its text; for `page`, `param` one
more than the page before's, from `start`; for `offset`, `param` moved on
from `start` by the number of items the page before held, or by `size`. A
parameter of the target's query or of a next URL that is `param` in any case
MUST be dropped where `param` is set, and `param` MUST be sent last. `next`
giving `null`, `false` or `""` MUST end the pages, more than one value or
another kind MUST fail the trip, the expression's values read no further
than the second and its run ended by the trip's deadline, and a login's
`token` and `expires_in` expressions likewise by the second value and the
login's deadline, so that an expression without end holds no trip, and so MUST items that are neither an array
nor missing or `null`, which are no items.

The pages MUST end at the first that names no next page — no `rel="next"`,
no next URL or cursor, or, for `page` and `offset`, no items — or at
`max_pages`. A trip that stops at `max_pages` while the last page named
another page, or for `page` and `offset` held items, MUST NOT fail: its
answer MUST be the pages fetched, the trip MUST be counted in
`http_exporter_pagination_max_pages_reached_total` and logged at warn level
through the failure log (§ 25.1), with the pages and `max_pages`, and the end of
it logged when a trip of that collector and target fetches every page again.
Every page fetched by a trip that ends with an answer MUST be counted in
`http_exporter_pages_fetched_total` (§ 22).

Every page MUST be a request of the collector's own, made as the first is:
the same method, body, headers, credentials, forwarded headers, TLS
settings, retries, redirect rules and target policy (§ 26.1), the policy
checked on each page's connections. A next page whose scheme, host or port
— a port left out being its scheme's — is not the first page's MUST fail the
trip before it is asked for, since it would be sent the target's
credentials, and so MUST a next page whose URL a page of the trip already
had. The one exception is the virtual host of the request's `Host` override,
a collector's `request.headers` `Host` or a static target's, the latter
replacing the former: a next page with the first page's scheme, the
`Host`'s host (in any case) and the `Host`'s port, its scheme's when the
`Host` names none, MUST be fetched from the first page's host and port, with
the same `Host`, and MUST NOT be fetched by looking the virtual host up; a
next page on another port or scheme of it MUST still fail the trip. The probe's `timeout` and the trip's deadline MUST cover every page
together. The response limit (§ 20) MUST hold each page, the pages' bodies
added up, failing the trip after the page that takes them past it, and the
merged document, each failure a limit failure (`ErrLimitExceeded`).

A first page that is not JSON, or answered with a status the collector does
not accept, MUST be the answer as it came, failing in the `decode` or the
`http_status` stage as without pagination. A later page answered with such
a status MUST be the trip's answer, with its status, headers and body,
failing in the `http_status` stage with an error that names its number and
its URL, with credential query values masked:
`received HTTP status 503 on page 3 of request.pagination, <url>`. A later
page that is not JSON, or whose request failed, MUST fail the trip naming
the page. An answer of one page MUST be the page byte for byte; the merged
document of several MUST be written again as JSON, each number as it was
written, the first page's status and headers, without its
`Content-Length`.

##### Rate limiting

`request.rate_limit` MUST bound how fast the collector asks one origin of its
targets. It is the `http` and `graphite` types' alone; a collector of any
other type that sets it MUST be refused at load as setting a key that does
not apply to its type. Its keys:

- `requests_per_second`, **required**: a number greater than 0; 0, a
  negative number, NaN and an infinity MUST be refused at load, naming the
  collector and the key.
- `burst`: how many requests MAY be sent at once after a pause, at least 1;
  0 or less MUST be refused at load. Left out, it MUST be 1, filled in when
  the configuration loads.

- The limit MUST be kept for each pair of collector and origin — the scheme,
  the host in lower case, and the port, the scheme's own when the URL leaves
  it out — so that two collectors asking one host are limited apart and one
  collector's requests never wait on another's. A static target's own
  `rate_limit` MUST replace the collector's for its requests, both keys
  together, `burst` 1 when it leaves it out, and MUST have a limit of its
  own for each origin, apart from the one the collector's other targets
  share there.
- Every request sent MUST take a turn, against the origin it is sent to:
  each attempt of `retry`, each page of `pagination`, each hop of a followed
  redirect, once the redirect's checks have passed, and each request of a
  `login`. A turn MUST come at once while the bucket, which holds `burst`
  turns and fills again at `requests_per_second`, has one, and otherwise
  when it fills enough for one.
- A request whose turn would come at or past its deadline — the probe's or
  the scrape's, with the probe's `timeout`, or a login's own — MUST fail at
  once, without waiting, with an error naming the limit, the origin, the
  wait and the time left, and MUST NOT be retried:
  `rate limit of 2 requests per second for http://legacy.example:80 would
  delay the request past its deadline: it would wait 1.5s for its turn, and
  800ms is left; ...`. A wait cut short MUST give its turn back.
- A debug run's requests (§ 42.17) MUST take their turns as any other's, and
  MUST NOT be counted in the collector's self-metrics (§ 22).
- The buckets MUST be bounded: one that has filled again MAY be forgotten,
  holding nothing a new one would not, and no more than 10,000 MUST be kept,
  the least recently used forgotten first beyond that. A reload MUST forget
  the buckets of the collectors it removed, and the exporter's shutdown
  every bucket.

###### Rate limits per host

The top-level `rate_limits` list MUST bound how fast the `http` and
`graphite` requests of every collector together ask a host. Each entry has:

- `name`, **required**: a name as a collector's is (§ 5), unique among the
  entries; an entry without one, with an invalid one, or with the name of
  another MUST be refused at load.
- `hosts`, **required**, at least one: host names, matched in any case and
  without a final dot; IP addresses, matched in canonical form, an IPv6 one
  with or without brackets; and `*.example.com` wildcards, which MUST match
  every subdomain of `example.com`, at any depth, and MUST NOT match
  `example.com` itself. An IPv4 address mapped into IPv6 MUST be matched, in
  an entry and in a request's URL, as the IPv4 address. A request's IPv6 zone
  MUST be ignored. An internationalised name MUST be matched in its `xn--`
  form, the only form a request that takes a turn names it in, since a
  request whose host has a character outside ASCII is refused before it is
  sent (§ 26.1). A host with a scheme, a port, a path, a `*` anywhere but a
  leading `*.`, a wildcard over an IP address, an IPv6 zone, or a character
  outside ASCII, the last refused naming the character and saying to write
  the `xn--` form, MUST be refused at load. A host, in canonical form —
  `::ffff:10.0.0.1` and `10.0.0.1` one host, as `XN--BCHER-KVA.example.` and
  `xn--bcher-kva.example` are — MUST be listed once across every entry;
  listed twice it MUST be refused at load, naming both entries. A host that a
  wildcard of an earlier entry matches — an exact name or a narrower wildcard
  under it — MUST be refused at load, naming both entries and saying to list
  the narrower entry first, since the later entry could never apply to it;
  listed before the broader wildcard it MUST be accepted. A host that a
  wildcard of its own entry matches MUST be accepted.
- `rate_limit`, **required**: the keys of `request.rate_limit`, checked as
  they are and refused at load as they would be, naming the entry, `burst`
  filled in as 1 when left out.

- An entry MUST match a request by the host of its URL alone, on every port
  and scheme. The first entry, in the list's order, whose hosts match MUST
  apply; a request MUST take a turn under at most one entry.
- Every `http` and `graphite` request that `request.rate_limit` counts — each
  attempt, page, followed redirect hop, against the host it goes to, and
  login request — MUST take a turn from the bucket of the entry its host
  matches, one bucket for each entry whichever of its hosts a request goes
  to, from any collector, probe or static target, in addition to the turn
  it takes under its collector's or static target's own `rate_limit`, after
  it. The turn MUST be taken, waited for and refused as under
  `request.rate_limit`, with an error naming the entry and the host:
  `rate limit of 5 requests per second of rate_limits entry "billing_api"
  for api.billing.example would delay the request past its deadline: ...`.
  A request refused under one limit MUST give back the turn it took under
  the other.
- Requests of every other type MUST NOT take a turn under an entry.
- A debug run's requests MUST take their turns, and MUST NOT be counted in
  the entries' self-metrics (§ 22).
- A reload MUST keep the bucket of an entry it leaves as it was, its name,
  hosts and `rate_limit` alike, and MUST start an entry it changes from a
  full bucket. It MUST forget the bucket and the counts of an entry it
  removes, and the exporter's shutdown every bucket and count.
- The entries in force MUST be those of the latest configuration in force:
  of two reloads close together, the entries of the earlier MUST NOT be put
  in force after those of the later, however the two followings interleave.
  So MUST the collectors and static targets in force that the request types
  keeping connections across scrapes, and the collectors' own
  `request.rate_limit`, follow.
- An entry's bucket MAY be kept by each exporter process alone: several
  replicas, behind the allocator (§ 42.14d) or otherwise, MAY together ask
  an entry's hosts up to their number times its `requests_per_second`.
  NOTE: per-process buckets are intended, as they need no state shared
  between replicas; the documentation MUST say so and how to keep within a
  quota.

#### The other request types

Every request type but `http` has its section of § 5.1 in a document of its
own, so that a change to one type is a change to one document. Each holds
what is the type's alone — its keys, what a target is for it, its fetch and
what it answers — and is held to the rules above, which every type shares,
as it is to the rest of this specification:

- [`dns`](SPECIFICATION-DNS.md)
- [`graphite`](SPECIFICATION-GRAPHITE.md)
- [`grpc`](SPECIFICATION-GRPC.md)
- [`icmp`](SPECIFICATION-ICMP.md)
- [`kubernetes`](SPECIFICATION-KUBERNETES.md)
- [`ldap`](SPECIFICATION-LDAP.md)
- [`localfile`](SPECIFICATION-LOCALFILE.md)
- [`ntp`](SPECIFICATION-NTP.md)
- [`redis`](SPECIFICATION-REDIS.md)
- [`s3file`](SPECIFICATION-S3FILE.md)
- [`snmp`](SPECIFICATION-SNMP.md)
- [`sql`](SPECIFICATION-SQL.md)
- [`sshexec`](SPECIFICATION-SSHEXEC.md)
- [`sshfile`](SPECIFICATION-SSHFILE.md)
- [`tcp`](SPECIFICATION-TCP.md)
- [`tlscert`](SPECIFICATION-TLSCERT.md)

---

## 6. Response format / decoder model

Supported decoder types MUST include:

```text
json
yaml
xml
csv
html
prometheus
text
graphite
ndjson
logfmt
toml
ini
none
auto
```

`auto` is optional but strongly recommended.

`none` MUST read no body: the document MUST be null — jq and yq rules read
`null`, with `$status` and `$headers` as for any decoder, and a Python
script's `data` MUST be `None` while `response.body` and `response.text`
are the body — and the decode MUST NOT fail, whatever the body holds, empty,
HTML, broken JSON or in an encoding the exporter does not know. The body
MUST NOT be converted to UTF-8 (§ 6.1a), so a debug report MUST name no
encoding it was converted from, and `response.charset` beside `none` MUST be
refused at load, naming the collector, the key and the decoder. `auto` MUST
never choose `none`.

Python is a transform, not a decoder. Supported transform types MUST include
`jq`, `yq`, `xpath`, `css`, `csv`, `regex`, `prometheus`, and `python`.
`transform.type` MUST be set on every collector: a collector without it, or
with any other value, MUST be rejected at startup naming the accepted types.
The decoder and transform types MUST each be listed once in the code, the list
validation and the JSON Schema both read.

`transform.labels`, `transform.remove_labels` and `transform.rename_labels`
MUST apply to every metric a collector produces, whatever its transform, after
its metric rules and in that order: add, remove, rename. The renames MUST be
made at once from the labels as they were before any of them, so they never
chain and the result does not depend on the order they are listed or walked in.
A `transform.labels` value MUST replace a label of the same name the series
has from a rule, a script or the target. A value written `""` MUST be the
label left out, as a key written `""` is the key left out (§ 24.3): it MUST
add no label — no `site=""` in an answer, in the OTLP export or in the count
`limits.max_labels_per_metric` is held to — and MUST NOT take away or change
a label of that name the series has. A value MAY hold `{{param_<name>}}`
placeholders, filled from the probe (§ 42.10c); one filled to nothing is a
value written `""`.
`transform.include`, `transform.exclude` and `transform.rename` pick and rename
the metrics a `prometheus` transform passes through, and apply only to one
without `metrics` rules. An entry of `include` or `exclude` is a regular
expression matched anywhere in the name the target gives a metric, before
`rename` and `metrics_prefix`. An entry that is the empty string, which
matches every name, and one that is nothing but blanks — text that
`strings.TrimSpace` leaves nothing of — which matches only the names that
hold them, MUST be refused at load, by the exporter and by the schemas
(§ 24.3): an entry of a list is not an optional key, so `""` there is not
the key left out. The error MUST name the collector and the key, quote an
entry of blanks, and say what an entry is and what to write: `'.*'` to match
every name, the key left out to filter nothing, and `'[ ]'` or `'\x20'`, in
single quotes, for a pattern that does mean a blank. Every other entry, and
every other transform setting, MUST get from the load what it got before
this rule.

The decoder MUST be chosen by `decoder.type`, the one key for it; `response`
MUST NOT take a format. `decoder.type` is optional and defaults to `auto`. When
it is omitted, the implementation MUST infer a deterministic response decoder from the transform
where possible: `regex` to text, `csv` to CSV, `css` to HTML, and `prometheus`
to Prometheus exposition, and a `graphite` collector to the `graphite`
decoder. jq/yq, XPath, and Python MAY use content detection
because they can operate on more than one response representation. If the
decoded response cannot be mapped to the selected transform, the exporter MUST
return a clear transform error. An explicit decoder remains available for
ambiguous or incorrectly labeled endpoints.

Which decoder's document each transform reads MUST be held in one table, read
by the scrape and by the load alike: `jq` and `yq` read `json`, `yaml`,
`graphite`, `ndjson`, `logfmt`, `toml`, `ini` and `none`; `regex` reads `text`; `csv` reads `csv`; `css` reads `html`;
`xpath` reads `xml` and `html`; `prometheus` reads `prometheus`; `python`
reads every decoder. An explicit `decoder.type` the collector's transform
does not read MUST be refused when the configuration loads, with a message
naming the collector, the decoder, the transform, the decoders that transform
reads and the transforms that read that decoder, since every scrape would
fail whatever the target answered. A `jq` or `yq` transform with a
`pre_script` MUST be exempt: its rules read the mapping or list the script
leaves, whatever was decoded (§ 16); for any other transform a `pre_script`
MUST NOT lift the check. A decoder left to each response (`auto`) MUST be
checked against the same table on the scrape.

A setting that belongs to a decoder or a transform the collector does not
have MUST be refused at load rather than ignored, naming the setting and the
decoder or transform the collector has: `transform.script` without a `python`
transform; `transform.libraries` and `transform.required_libs` on a collector
with neither a `python` transform nor a `pre_script`; `response.csv` on a
collector whose decoder is neither `csv` nor `auto`; `response.namespaces`
without an `xpath` transform; and `response.graphite` as § 15a says.

Where the transform implies no decoder and `decoder.type` is left unset, each
response MUST be decoded by its `Content-Type` header for `http`, by its file
extension for `localfile`, and by its content when those do not say, where an
HTML doctype or `<html>` root element, in any case, within the first KiB after
whitespace, comments and an XML declaration, MUST be recognised as HTML before
other markup is taken for XML, and a body whose every line other than blank and `#`
lines is `<path> <number> <number>`, with some path holding a dot or a `;`
and none a brace or quote, MUST be recognised as carbon lines, after JSON,
HTML and XML and before text. A body that starts with `{` or `[` MUST be
taken for JSON when it is one JSON value none of whose numbers lies beyond a
64-bit float (`1e400`), and for what else it may be otherwise; a body so
taken for JSON MUST be read once, the decoder returning what the detection
made of it rather than reading the body a second time. A local file ending
in `.graphite` or `.carbon` MUST be given the Content-Type `text/x-graphite`,
which MUST select the `graphite` decoder; one ending in `.ndjson` or `.jsonl`
MUST be given `application/x-ndjson`, and one ending in `.toml`
`application/toml`. `application/x-ndjson` and `application/jsonl` MUST
select the `ndjson` decoder and `application/toml` the `toml` decoder. No
content MUST be taken for NDJSON, logfmt, TOML or INI, and no Content-Type or
extension for logfmt or INI: those two MUST be read only by an explicit
`decoder.type`. Such a
collector MUST be reported as a configuration warning, naming it and how it
decodes, at startup, on every reload and in the `--dry-run` report; it MUST NOT
fail the load. An explicit `decoder.type`, `auto` included, MUST NOT be
warned about.

The entire `response` block is optional. CSV decoding MUST use a header row by
default when `response.csv.header` is omitted. The `response.csv` block is only
needed for non-default CSV options such as a custom delimiter, trimming, or a
headerless response. Omitting response configuration MUST NOT weaken validation:
decode failures and transform/input incompatibilities remain collector-level
errors controlled by `error_handling`; `metric.error_mode` applies after a
response has been decoded and an individual metric is being extracted.

### 6.0b The binary decoder

`decoder.type: binary` MUST read a body by `response.binary`, its fields in
order, into a mapping of each field's name to its value, for `jq`, `yq` and
`python` alone; `auto` MUST never choose it, and no charset MUST be applied
to the body it reads.

- A field MUST start at its `offset`, from 0 at the start of the body, or
  else at the byte after the field before it. `u8` to `u64` and `i8` to
  `i64` MUST be read as unsigned and two's complement integers of their
  size, `f32` and `f64` as IEEE 754 floats, in the field's `byte_order`, or
  the layout's, `big` when neither is written; an integer MUST be an int of
  the document, but a `u64` past what an int holds, which MUST be a float.
  With `count` the field MUST be a list of that many values read one after
  another. `string` MUST be `length` bytes and `cstring` the bytes up to a
  NUL, within `length` bytes when it is written, the NUL read and not kept,
  each byte that is no UTF-8 read as U+FFFD; `skip` MUST pass `length`
  bytes over and be in no document. Bytes after the last field MUST be left
  unread.
- A field that runs past the end of the body, and a `cstring` whose NUL does
  not come within the body or its `length`, MUST fail the decode naming the
  field, quoted, the byte it starts at and the bytes the body has.
- At load MUST be refused: `response.binary` beside another decoder, or with
  the decoder left to the response; the decoder with no field, with more than
  1,000, or with a transform other than `jq`, `yq` and `python`; a
  `byte_order` other than `big` and `little`; a field of another type, of no
  name but a `skip`, a `skip` with a name, a name another field has, a
  negative `offset`, `count` or `length`, a `count` above 65,536 or on a
  `string`, `cstring` or `skip`, a `string` or `skip` without `length`, and a
  number with one.

### 6.0a NDJSON, logfmt, TOML and INI

The `ndjson`, `logfmt`, `toml` and `ini` decoders MUST make the values the
`json` decoder makes — mappings with text keys, lists, text, whole numbers,
floats, booleans and null — so that `jq` and `yq` read them as JSON, and a
Python script is handed them as it is handed JSON (§ 16). Each MUST fail the
decode over what it cannot read, naming the line, as a position that is no
part of what the failure is recognised by, and MUST quote no more of a value
of the body than its first 64 bytes and its length, as every decoder's error
does (§ 6.1a).

- `ndjson`: the document MUST be the list of the values of the body's lines,
  in their order, a line being what lies between two line feeds. A line of
  nothing but JSON whitespace MUST be passed over, and a body of no value
  MUST be the empty list. Each other line MUST be one value read as the
  `json` decoder reads a body, its numbers made alike, with nothing but
  whitespace after it on the line; a line that is no value, that goes on
  after its value, or that ends inside it MUST fail the decode naming the
  line, and the column where there is one, counted in the whole body. The
  list MUST count as the outermost level, so a line's value nests
  `MaxDepth - 1` deep at most.
- `logfmt`: the document MUST be the list of the records, one for each line
  that holds a pair, in their order. A line MUST be read as
  `github.com/go-logfmt/logfmt` reads one: pairs separated by bytes up to and
  including the space; a key a run of bytes above the space other than `=`
  and `"`; a value a run of the same bytes, or a double-quoted string with the
  escapes of a JSON string and no raw control character. A record MUST map
  each key to its value as text, a key written without `=` to null and a key
  written with `=` and nothing after it to the empty string. Of a key written
  twice on one line the later MUST be kept. A line of nothing but blanks and
  control characters MUST be passed over; any other mistake MUST fail the
  decode naming the line, the column and the byte, and no more of the body.
  The exporter MUST NOT depend on a logfmt module for this.
- `toml`: the document MUST be read with `github.com/BurntSushi/toml`:
  tables as mappings, arrays and arrays of tables as lists, integers as whole
  numbers, floats as floats, `nan` and `inf` included, and a date or time as
  its text — an offset date-time in RFC 3339 with the fraction it was written
  with, a local date-time as `YYYY-MM-DDTHH:MM:SS` with its fraction, a local
  date as `YYYY-MM-DD`, a local time as `HH:MM:SS` with its fraction. Before
  the library reads the body, a body whose arrays and inline tables nest
  deeper than `MaxDepth`, or one with a value named by more than 16 parts,
  counting those of its table header, of its dotted key and of the keys of
  the inline tables around it, MUST be refused naming the line: the library
  parses nested brackets by recursion and costs the square of a key's parts
  for each key. The scan that keeps these bounds MUST pass over the byte
  order mark the library passes over itself (UTF-8, UTF-16LE or UTF-16BE),
  after the one taken off the response, and where it cannot follow the body
  it MUST refuse it naming the line, and MUST NOT hand the library a body it
  has not scanned to its end. A document whose tables and arrays together
  nest deeper than `MaxDepth` MUST be refused too. An error of the library MUST be its message
  after the line it names, without the last key it read, and a message past
  256 bytes MUST be cut there with its length.
- `ini`: the document MUST be a mapping of each section's name to the mapping
  of its keys to their values, as text, and of each key written before the
  first section to its value. A line MUST be read without the spaces, tabs
  and carriage returns around it; an empty line and one starting with `;` or
  `#` MUST be passed over, a comment being only a whole line. `[name]` MUST
  start a section, may be followed by a comment and nothing else, and MUST
  name it by the text between the brackets without the blanks around it,
  which MUST NOT be empty; a dotted name MUST be one name. A section written
  twice MUST be one section. Any other line MUST be a key and its value split
  at the first `=` or `:`, each without the blanks around it; the key MUST NOT
  be empty, and the value MUST be taken as written, quotes included. Of a key
  written twice in one section the later MUST be kept. A key before the first
  section and a section of the same name MUST fail the decode naming both
  lines, and so MUST any line that is none of these.

### 6.1 Auto detection

When `format: auto` is used, determine format using, in order:

1. Explicit configuration where present.
2. HTTP `Content-Type`.
3. Content inspection for ambiguous `text/plain` responses.

Examples:

```text
application/json          -> json
application/yaml          -> yaml
text/yaml                 -> yaml
application/xml           -> xml
text/xml                  -> xml
text/csv                  -> csv
text/html                 -> html
text/plain                -> text
text/plain; version=0.0.4 -> potentially prometheus
application/openmetrics-text -> prometheus, read as OpenMetrics (§ 14.1)
application/x-ndjson      -> ndjson
application/jsonl         -> ndjson
application/toml          -> toml
```

The type MUST be compared without its parameters and in any case; a type
ending in `+json` is JSON and `application/x-yaml` is YAML. Only `text/csv`
names CSV: a body of any other type — `application/csv` and
`text/tab-separated-values` among them — or of none MUST be read by its
content, and no content is taken for CSV, since nothing in a CSV body tells
it from text. Such a body is text to a transform that reads what the decoder
gives it, and CSV to a `csv` transform, which names its decoder itself. The
documentation MUST tabulate the types, and the extensions of a `localfile`
file (§ 5.1), that name each decoder.

Ambiguous formats SHOULD be rejected or require explicit configuration rather than guessed incorrectly.

### 6.1a Character encodings

A response MUST be converted to UTF-8 before its format is detected or it is
decoded, from the first of: a byte order mark (UTF-8, UTF-16LE, UTF-16BE),
which MUST be removed; the collector's `response.charset`; the `charset`
parameter of `Content-Type`; and, only when none of those named one, what the
document declares in itself: for a document decoded as XML, the encoding of
its XML declaration; for a document decoded as HTML, a `<meta>` in its first
1024 bytes and, when no `<meta>` there declares an encoding, the encoding of
an XML declaration the document starts with, as the WHATWG HTML standard
orders them. An XML declaration and a `<meta>` of one HTML document that
disagree MUST be read as the `<meta>` says. The XML declaration of a document
decoded as HTML MUST stand at the first byte of the document: after
whitespace or anything else it MUST declare nothing. A document decoded by
any other decoder declares nothing.

A `<meta>` MUST be found as the WHATWG HTML standard's prescan ("prescan a
byte stream to determine its encoding") finds it: a comment MUST be passed
over; only a `<meta` tag counts, with a `charset` attribute, or with a
`content` attribute from which the standard's "extracting a character
encoding from a meta element" yields a name together with an `http-equiv`
attribute whose value is `content-type`; attribute names and that value MUST
be matched in any case, values MAY be quoted either way or bare, with
whitespace around `=`, and the first attribute of a name is the one that
counts; a `charset` attribute MUST be taken over a `content`; a `content`
without `http-equiv="content-type"` MUST declare nothing; every other tag
MUST be passed over with its attributes; and the first `<meta>` that
declares an encoding MUST be the one read. A declaration that is not whole
within the first 1024 bytes MUST declare nothing. As in the prescan, a
`<meta>` naming what is no encoding's name — the name is the whole value of
the attribute, so the `utf-8/` of `<meta charset=utf-8/>` is none — MUST be
passed over, the next `<meta>` being looked at, and the XML declaration of a
document decoded as HTML naming no encoding MUST declare nothing: neither
MUST fail the scrape. The documentation MUST say what this makes of a page
in a legacy encoding whose `<meta>` is written that way.

The `charset` parameter of a `Content-Type` that is not well formed MUST
still be read: the first `charset` parameter, its name in any case and its
value quoted or a bare token, among the `;`-separated parameters of the first
of the header's comma-separated values. A header that does not start with a
type and a subtype, and a `charset` whose value is empty, is no token or
opens a quote that nothing closes, MUST name nothing, without an error.

Encoding names MUST be looked up as the WHATWG Encoding Standard defines
them. As the WHATWG HTML prescan does, a `<meta>`, and likewise the XML
declaration of a document decoded as HTML, naming UTF-16 (`utf-16`,
`utf-16le`, `utf-16be`) MUST be read as UTF-8 and one naming `x-user-defined`
as `windows-1252`; a byte order mark, `response.charset` and the
`Content-Type` header MUST be taken as they say. An unknown
`response.charset` MUST fail to load; an unknown name in the `Content-Type`
header, or in the XML declaration of a document decoded as XML, MUST fail the
`decode` stage naming it. The converted body MUST be what decoders, transforms
and Python scripts see, with `charset=utf-8` in its `Content-Type` — in one
that is not well formed, in the place of the `charset` value that was read,
the rest of the header kept — and an XML declaration naming another encoding
MUST be rewritten to UTF-8 so the XML parser does not convert it again.
Converting MUST NOT change the bytes of the body that was fetched: a debug
report (§ 42.17) shows them.

After every transform, label values and help texts that are not valid UTF-8
MUST have their invalid bytes replaced with U+FFFD rather than failing the
scrape, since Prometheus refuses a whole scrape over one of them. Each value
repaired MUST be counted in `http_exporter_invalid_utf8_total` for the
collector, and a probe that repaired any MUST log a warning with the count and
the first metric (`first_metric`, a name past 200 bytes by its first 200 and
its length, as below), suggesting `response.charset`. A label map a transform
shares among metrics MUST NOT be changed in place.

A decoder names what it could not read with that part of the body — a token
that is no number, a name written twice, an element that was not closed, a
line that is no carbon line — which is the target's to make as long as the
body, and the error is logged as one line, answered to the scraper and shown
in a debug report. So an error in the exporter's own words MUST show no more
of a value of the body than its first 64 bytes, the cut between two
characters, followed by the value's length, as a value is quoted in any
error (§ 18.1), and MUST then say what it said after the value, which is
what is wrong and what to change: `expected float as value, got
"aaaa..."... (1000000 bytes)`, `label value "aaaa..."... (1000000 bytes)
contains unescaped new-line`, `"a b c d aaaa..."... (1000008 bytes) has 5
fields; want <path> <value> <timestamp>`. A value of 64 bytes or fewer MUST
read exactly as it did: quoted as Go quotes a text, or as it is where the
message writes a name without quotes, and a name written so is cut alike,
its length after its start. This holds for every such error that shows a
value (a JSON error names one character, and a YAML error, worded as the
YAML library words it, holds a part of the document by the rules of § 10):

- of the Prometheus and OpenMetrics parser (§ 14.1), a metric's name, a
  label's name and value, a sample's value and timestamp, what follows a
  timestamp, a metric's type and a help text; where an error names a
  histogram or summary series by its labels (`the histogram
  name{label="value"}`), the family's name and each label's name and value
  are cut by themselves;
- of the Graphite decoder (§ 15a), a carbon line, its value and its
  timestamp, a series' name and a tag, and a point of the render API that is
  no `[value, timestamp]`, which is shown as it was, `[x 1]`, by the first 64
  bytes of that;
- of the CSV decoder (§ 13), a column the header names twice;
- the name of an encoding that is none, from `response.charset`, a
  `Content-Type` or an XML declaration;
- the report of the first line a decoder left out of a body it decoded: a
  carbon line that is skipped, and the sample and its family of a sample
  line of an exposition that is left out, which is logged as a warning and
  remembered as a failure is.

The names the XML parser writes into its errors MUST be cut alike, each to
its first 64 bytes and its length, in the library's words and with what the
library says after the name: an element's (`unexpected end element </name>`,
`element <name> closed by </name>`, `invalid characters between </name and
>`), a name space prefix's (`element <name> in space prefix closed by
</name> in space prefix`, `xmlquery: invalid XML document, namespace prefix
is missing`), an entity's (`invalid character entity &name;`, with ` (no
semicolon)`), a name that is none (`invalid XML name: name`), and the
version and the encoding of the XML declaration (`xml: unsupported version
"1.1"; only version 1.0 is supported`, `xml: opening charset "name":
unsupported charset: "name"`). The names are read from the library's text in
the forms the library writes them in and nowhere else, a text of a form to
its last byte and of the first form it reads as; an error in none of the
forms, and one whose names are all within 64 bytes, MUST be the library's
own, untouched. The error with a name cut MUST be a new one that holds
neither the long text nor the library's error.

No text of the size of the value MUST be made to refuse it: the value is
neither quoted nor formatted whole and then cut, and the error MUST hold
nothing of the body but the bytes it shows. Decoding a body that decodes MUST
cost what it cost. The failure MUST be recognised (§ 25.1) by the error's
text with the mark in place of each such length, so the same mistake with a
longer or a shorter value that starts the same is the same failure, and one
whose value starts otherwise another; a value of 64 bytes or fewer is part of
what the failure is, as it was.

As the last resort, the error of a body that cannot be decoded MUST be no
longer than 2,000 bytes, whichever decoder read the body and whatever it
holds; no error of the exporter's own decoders may reach that bound for one
long value, and what does is an error long by many values, as a series named
by a hundred labels, or a library's text of a form that is not known. A
longer error MUST be cut to its start and end with the length it had (`...
(6400 bytes)`), 2,000 bytes together, the cut between two characters; one
within the bound MUST be the decoder's own error, untouched. The text the
failure is
recognised by (§ 25.1) MUST be bounded alike, cut where it is itself over
2,000 bytes whatever the error's length, with the mark in place of the
length. The cut error MUST NOT hold the text it was cut from, nor the error
that held it, so that whoever keeps the error, as the failure log keeps what
it is recognised by, keeps no more than that; that the error is a limit that
was exceeded (§ 20) MUST be kept, and what else it was to `errors.As` MAY be
lost. The error a decoder reports of the first line it left out of a body it
decoded MUST be bounded alike.

The bound is that of every failure, not of a decode's alone. The error of
every stage a probe or a static target's scrape can fail at — the fetch,
whatever its request type, and the status it found; the target policy's
refusal; the transform, each of its rules and its scripts; the validation;
each stage a file of a directory is left out at; and the stages of a static
target's scrape before its trip — MUST be no longer than 2,000 bytes where
the trip reports it: in the answer to the scraper, which is that error after
what the answer always wrote around it (the collector and the stage, or the
JSON object of a rule under `error_mode: fail`); in the `error` of every log
line; in a debug report, in each line that names the failure and in how each
request ended; in the first failure a rule is reported with (§ 25); and in
the text the failure log remembers the failure by (§ 25.1), which MUST be no
longer whoever reports the failure. It MUST be bounded as a decode error is:
one within the bound MUST be the stage's own error, untouched, made at no
cost but the reading of its text and of what it is recognised by; a longer
one MUST be cut to its start and its length, recognised by the start of what
it was recognised by and the mark, and MUST NOT hold the error it was cut
from, of which it keeps what kind of failure it was (§ 20: a limit exceeded,
a value missing, a script that failed). What an error is to the trip — a
refusal, a credential refused, a rule's failure under `fail`, a deadline —
MUST be read before it is cut. A trip that ran out of its budget MUST still
say so, after the bounded error. The text of a panic a probe or a scrape
recovered from MUST be bounded alike in a debug report and in the log; its
stack is logged as it was. A probe that panicked MUST be answered `500` with
the fixed text `probe failed: the scrape panicked; the exporter's log has the
panic`, never the panic's text (a collector with `report_failures` MUST answer
it `200` with success `0` and the stage `panic`, § 19.5): its caller keeps the answer as the scrape's
error, out of the exporter's hands, and a panic says whatever its code had at
hand, a target's URL with its password as much as anything.

A panic MUST be the failure of its trip alone wherever in the trip it is
raised, and MUST NOT end the exporter. A fetch that runs part of its work on
goroutines of its own — the servers of an `ntp` scrape, the addresses of an
`icmp` scrape and the reader of its socket, the ports of a `tcp` check, the
requests of a `kubernetes` scrape, the read of a file or a directory and
the run of a command over SSH, which a deadline may leave behind, a login
and a gRPC reflection question shared by
the requests waiting for them — MUST recover a panic of each, log it once with
the stack of the goroutine it was raised on, and raise it on the trip's own
goroutine, where it is answered and counted as any panic of the trip is. A
panic of a login or of a reflection question MUST be the error of every
request that waited for it.

That bound loses what an error says after a long part, so where an error of
those stages says why after a part the target, the scraper or a script can
make as long as a response, the part MUST be shown by its start and its
length, as a decoder shows a value, and the error MUST go on as it did:

- a URL the error of an HTTP request quotes (`Get "<URL>": <reason>`), and
  the same URL where the refusal of a redirect writes it again, by its first
  512 bytes, the cut between two characters, with its credentials withheld
  as they were;
- the name of a metric or of a label the validation quotes, by its first 200
  bytes, the default `limits.max_metric_name_length` and
  `limits.max_label_name_length`, quoted, with its length after the quotes;
- a script's error and what its interpreter wrote to stderr, as § 16.7 says
  of them;
- the metrics of a directory's file that clash in type with another file's,
  named while their clashes are within 1,200 bytes together, the rest
  counted (`...; and 4983 more: a metric has one type across the
  directory's files`).

Each such length and count MUST be the mark in what the failure is
recognised by, and an error none of whose parts is over its bound MUST be
the error it was, to the letter, at the cost it had.

The name of a metric has one rule wherever it is shown. A log line that names
a metric in an attribute whose name the target or a script chose — the
`first_metric` of the warning of repaired UTF-8, which is logged before the
set is validated, when the name is as long as the response; the `metric` and
the `clashes_with` of a static target's metric left out of the static targets
endpoint and the `metric` of its return (§ 42.14a); the `metric` of a name an
OTLP export had as two kinds, and the `metric`, `prometheus_name` and
`left_out_prometheus_name` of one it had for two Prometheus names (§ 42.1),
whose names are as long as a raised
`limits.max_metric_name_length` lets them be — MUST show a name longer than
200 bytes by its first 200 bytes, the cut between two characters, followed by
`... (N bytes)` with the length of the whole name, as the error shows it
without the error's quotes, and MUST NOT hold the name it was cut from; a name
of 200 bytes or fewer MUST be the attribute's value as it was, the line the
same to the byte. What such a failure is remembered by (§ 25.1) MUST NOT
change with the cut: the warning of repaired UTF-8 is its collector's, its
address's and its file's whatever its first metric, so another first metric is
the same failure again, and a metric left out of the endpoint or exported as
one kind of two is remembered under its whole name, so two names that are the
same for 200 bytes are two failures.

---

## 7. Common internal data model

All decoders MUST ultimately produce a common representation.

Recommended Go types:

```go
type MetricType string

const (
    GaugeMetricType     MetricType = "gauge"
    CounterMetricType   MetricType = "counter"
    HistogramMetricType MetricType = "histogram"
    SummaryMetricType   MetricType = "summary"
    UntypedMetricType   MetricType = "untyped"
)

type Metric struct {
    Name      string
    Help      string
    Type      MetricType
    Value     float64
    Labels    map[string]string
    Timestamp *int64
}

type MetricSet struct {
    Metrics []Metric
}
```

The exact internal representation may be improved during implementation, but the following properties are mandatory:

- Metric name
- Metric type
- Numeric value
- Labels
- Optional help text
- Optional timestamp

For native Prometheus input, preserve supported metric metadata where possible.

These types, and the configuration types, MUST live in one package that
imports no other package of the exporter (`internal/model`), so every stage can
share them.

## 7.1 Source layout

The root package MUST hold only the command line and the `--dry-run` report,
with their own tests. The tests that check the repository as a whole — the
documentation, the chart, the workflows, the Dockerfile, the committed schemas
and the examples — MUST live in `test/repository`, a package of tests only. The
rest MUST live under
`internal/`, in packages layered so that each imports only packages before it in
this order, without cycles:

1. `allocapi`: what the target allocator and an exporter in allocator mode
   say to each other (§ 42.14d).
2. `model`: the shared data types.
3. `kubeapi`: listing objects from a Kubernetes API server, which the
   `kubernetes` request type and the target allocator share.
4. `expr`: expression compilation and its caches.
5. `fetch`: request types, probe parameters and transports.
6. `decode`: response decoding and charset conversion.
7. `transform`: transforms, metric rules and the Python worker pool.
8. `config`: loading, validating and reloading the configuration and target
   files, and their schemas.
9. `allocclient`: the exporter's client of the target allocator (§ 42.14e).
10. `pagekit`: what the HTML pages of the exporter and of the target
   allocator share: the stylesheet, the theme, the icon, the refresh
   controls and how a duration is written.
11. `exporter`: the HTTP server, the probe pipeline, self-metrics, static
   targets and OTLP export.
12. `allocator`: the target allocator (§ 42.14d).

The target allocator's command line MUST live in `cmd/pue-allocator`, and
nothing else of it there.

`internal/grpctest` and `internal/sshtest`, the stand-in gRPC and SSH
servers of the `grpc` and `sshfile` tests, MUST NOT be imported outside tests
and MUST carry their type's build constraint. `internal/sqltest`, the
stand-in database of the `sql` tests, `internal/kubetest`, the stand-in
Kubernetes API server of the `kubernetes` and `internal/kubeapi` tests,
`internal/redistest`, the stand-in Redis server of the `redis` tests, and
`internal/ldaptest`, the stand-in LDAP server of the `ldap` tests, MUST
NOT be imported outside tests and MUST import nothing but the standard
library. `internal/testutil` MAY hold
helpers shared by the tests of several packages, the stand-in server of the
`tcp` tests among them, which MUST import nothing but the standard library,
and MUST NOT be imported outside tests; nor may the package within it,
`internal/testutil/alloctest`, the tests' measure of allocations (§ 34), which
MUST NOT import a package of the module, so that the tests of every package,
`model` included, can use it.

A probe and a static target's scrape MUST make their trip to the target —
fetch, decode, transform, validate, with the collector's `error_handling` —
through one shared function, so the two cannot drift apart; only what
surrounds the trip differs (waiting for or refusing a concurrency slot, the
probe's time budget, and where the result goes). Its failures are logged
under each caller's own messages and failure-log key (§ 25.1).

The binary MUST still be built from the
repository root (`go build .`), and `-X main.version` MUST still set the version
it reports.

---

## 8. Decoder interface

Use an interface similar to:

```go
type Decoder interface {
    Decode(ctx context.Context, response *HTTPResponse) (*MetricSet, error)
}
```

Or, if transformation is intentionally separated internally:

```go
type Decoder interface {
    Decode(ctx context.Context, response *HTTPResponse) (any, error)
}

type Transformer interface {
    Transform(ctx context.Context, input any) (*MetricSet, error)
}
```

The preferred architecture is composable:

```text
HTTP Response
    -> Decoder
    -> Optional Transformer
    -> MetricSet
```

Do not hardcode decoder-specific logic in the Prometheus emitter.

---

# 9. JSON decoder

The JSON decoder MUST:

- Parse JSON safely.
- Detect malformed JSON, and say where it is malformed: what is wrong, and
  the line and column of the byte that cannot be there.
- Refuse anything but whitespace after the value, such as NDJSON's second
  record, saying NDJSON is not supported, rather than dropping it.
- Read arrays and objects nested 10,000 deep, and refuse a document nested
  deeper, saying so with the line and column where it is. That depth is the
  one limit on how deep the lists and mappings of a decoded value nest,
  whichever decoder made it: a response of any format MUST decode into a
  value nested no deeper, and one that would nest deeper MUST be refused when
  it is decoded, saying so (§ 10 for YAML; the other formats decode into
  values a few levels deep, or into text). The limit MUST be stated once,
  with the decoders; what depends on how deep a decoded value nests (§ 16.7)
  MUST take it from there, and MUST NOT state a limit of its own beside it.
- Read a whole number of more than 4,096 digits, sign included, as its text,
  every digit kept, rather than as a number: reading that many digits as an
  integer takes time growing with the square of their count, which a body
  within the size limit could spend seconds on with no deadline to stop it. A
  pre-script's result MUST be read by the same rule.
- Provide decoded data to the transformation layer.
- Support jq transformations.
- Support simple metric extraction without jq if practical.

When a jq metric expression emits multiple values, each value becomes one
metric sample. An expression label that emits the same number of values is
paired with those samples by array index. A scalar expression label is reused
for every sample. Missing or null metric values are handled according to the
metric's `error_mode` and the collector's missing-key policy.

Example:

```yaml
- name: application_json
  decoder:
    type: json

  transform:
    type: jq
  metrics:
    - name: application_requests_total
      description: Total application requests
      type: counter
      error_mode: log
      expression: '.requests'
      labels:
        - name: environment
          expression: '.environment'
```

---

# 9a. gRPC answers as JSON

In the `grpc` request type's specification,
[SPECIFICATION-GRPC.md](SPECIFICATION-GRPC.md#9a-grpc-answers-as-json).

---

# 10. YAML decoder

The YAML decoder MUST:

- Parse YAML safely.
- Support YAML documents commonly returned by HTTP APIs.
- Refuse a stream of more than one document, saying multi-document YAML is
  not supported, rather than dropping all but the first; a leading `---`, and
  an empty document after the first, as a trailing `---` makes, are not a
  second document.
- Keep a scalar YAML reads as a timestamp, such as `2024-06-01`, as the text
  it was written as, and an integer beyond int64 as an integer, so jq and
  labels can use both.
- Support yq transformations.
- Handle missing keys according to collector error policy.
- Name no more than the first ten problems of a document it refuses with a
  list of them, as a key written k times is a problem for each two of them,
  and count the others (`... and 719390 more problems`): the error's text
  MUST NOT grow with the square of the document, and the text of the whole
  list MUST NOT be made. A list of ten problems or fewer MUST read as the
  YAML library wrote it, but for a key that is cut (next).
- Quote no more than the start of a part of the document in an error,
  however long the part is, and say how long it was. The YAML library writes
  into its errors, whole, a key written twice (or the name of the anchor an
  alias key stands for), the Go syntax of a key that is a sequence or a
  mapping with all it holds (`invalid map key: []interface {}{1, 2, 3}`,
  which the exporter refuses in words of its own that hold nothing of the
  key, below), a
  scalar that does not fit its tag (``cannot decode !!str `...` as a
  !!int``), and the name of an anchor that is not known or that holds itself;
  a key of 200,000 numbers is an error of 1.4 MB. Of a key written twice the
  problem MUST show the first 64 bytes, quoted, and then the key's length, as
  a value is cut in any error (§ 18.1): `line 5: mapping key "aaaa..."...
  (100000 bytes) already defined at line 1`, so that ten problems of long
  keys are all ten read, in under 1,500 bytes where the keys are plain text;
  a key of 64 bytes or fewer MUST read as the library wrote it. Of the part
  another error holds, the error MUST show the first 256 bytes as the library
  wrote them, what closes the part, the part's length, and then what the
  library says after it (``cannot decode !!str `aaaa...`... (1000000 bytes)
  as a !!int``, `unknown anchor 'aaaa...'... (1000000 bytes) referenced`); a
  part of 256 bytes or fewer MUST read as the library wrote it. The words of
  a panic (below) MUST be cut alike, to 256 bytes, the exporter's own still
  ending with what it says of the defect.
  Every cut MUST fall between two characters. This holds on every way a
  document is refused: by the library, by the refusal of keys written twice
  made without it, which MUST NOT quote a whole key to cut it, by the walk
  in the library's words, and by a panic; an error in none of these forms is
  bounded as every decoder's is (§ 6). The part is read from the library's
  text in the one form the library writes it in and nowhere else: a key
  between `mapping key ` and the last ` already defined at line `, a scalar
  between the first backquote and the last `` ` as a ``, an anchor's name
  between its quotes; the scanner's and the parser's errors (`yaml: line N:
  ...`) quote nothing of the document, and `cannot unmarshal` holds seven
  bytes of a scalar. The error returned MUST NOT hold the long text or the
  library's error: it is a new error of the short text, still a list of
  problems to `errors.As` where the library's was. The failure MUST be
  recognised (§ 25.1) by the same text with the mark in place of each length,
  so a key or a part that starts the same and is longer or shorter is the
  same failure, and one that starts otherwise another; two keys written twice
  that start with the same 64 bytes, both cut, are one problem to the log,
  which is accepted: the error tells them apart by their lines.
- Decode a document, and refuse one, in time and memory linear in its size,
  whatever it holds: a mapping of any number of keys MUST NOT cost time that
  grows with the square of that number, as comparing every two of its keys
  does, and a key written many times MUST NOT cost memory for each two of
  them. The limits on a response's size (§ 20) thereby bound what decoding
  it costs. A mapping of many keys MUST NOT make the rest of its document
  cost a call of the YAML library for each item of a sequence beside it:
  the items are handed to the library together, no more than 128 at a time.
  A mapping merged into another (`<<`) MUST NOT cost a call of the library
  for each of its keys and values either: the keys of a run of its pairs
  are handed to the library together, and then the values of those the
  merge gives the mapping, no more than 128 at a time, so that decoding a
  mapping and a merge of it allocates no more than twice what decoding the
  mapping does.
  A pair MAY be handed alone where its key is no scalar, where the walk
  decodes its value itself or the library could refuse the value for its
  aliases in a part made for it, and where a key of its run is not decoded
  without a word, as a null among text keys. What a merge decodes into, the
  order its problems are listed in, and where a document is refused for its
  aliases MUST be what they are with every pair handed alone.
  And looking through a document before it is decoded MUST NOT take a stack
  deeper than the document is nested, however its aliases follow one
  another: anchors that each hold an alias of the one before MUST NOT be a
  call each. Decoding a document MAY take the stack the library's own
  decoding takes, which follows an alias into what it stands for; that stack
  is as deep as the document is nested plus the length of its alias chains,
  which the library bounds by neither its limit on nesting nor its limit on
  how much the aliases may expand to, so it is bounded here instead (below).
- Refuse a document nested deeper than 10,000, the one limit of every
  decoded value (§ 9), before anything decodes it, on every way a document is
  decoded, the library's own included, and whether or not it has an alias,
  with one message for every such document: `yaml: the document is nested
  more than 10000 deep, counting what its aliases stand for; a response may
  nest 10000 deep at most`. What is counted is what § 9 counts, the
  sequences and mappings one inside another: a scalar is no level, a sequence
  or a mapping is 1 more than the deepest of the nodes it holds, and an alias
  is as deep as what it stands for, itself no level. The nodes a mapping
  holds are its keys, its values and the value of a merge key, which counts
  as it is written, one level in, though its keys become the mapping's own.
  A document that decodes therefore decodes into a value nested no deeper
  than 10,000, and exactly as deep as it is counted wherever the value is
  nested as the document is written, which a mapping merged is not. The
  library MUST NOT be relied on for the limit: its parser bounds nesting by
  indentation and nesting in brackets apart, at 10,000 each (`max_indents`,
  `max_flow_level`), and refuses a document past either in its own words
  (`yaml: exceeded max depth of 10000`, or `yaml: line N: exceeded max depth
  of 10000` where the line it names is not the document's first); brackets
  inside indentation therefore parse nested 20,000 deep, and 30,000 where
  sequences stand under mappings' keys without being indented, a level of
  indentation being two collections there. The parser's refusal is of a
  document nested deeper than the limit as well, each of its levels being a
  sequence or a mapping, and its words do not say what the 10000 bounds: it
  MUST read as the exporter's own, the one message above, so that a document
  nested too deep is refused in the same words whichever of the two refuses
  it. Where the parser names a line the message MUST keep it, in the
  library's place for one (`yaml: line 3: the document is nested more than
  10000 deep, ...`): the line the level too many opens on, or, of levels of
  indentation, the line of the last key or value written before them. The
  failure MUST be recognised (§ 25.1) by the message without the line, with
  or without one and whichever of the two refused the document. Only the
  whole of the library's text for that refusal MUST be read as it — `exceeded
  max depth of ` and the depth that is the limit, after `line N: ` or after
  nothing, N a number from 1 — and any other text MUST be left as the
  library wrote it. The
  depth of a document without an alias MUST be found in the one look through
  the document already taken, with no second pass over it and no allocation;
  through aliases it MUST be found memoised and in a stack no deeper than the
  document is nested, so that an alias chain is no stack of its own, and MUST
  terminate on an anchor that holds itself, which keeps the refusal it has
  (`anchor 'a' value contains itself`). The same count bounds the stack of
  the decoding, where the library calls itself for each sequence and mapping
  and, on the way to one, for no more than one alias and one merge: a
  document within the limit MUST be decoded in a stack of no more than 16 MB.
  This refuses a chain of anchors that each hold an alias of the one before
  (`a0: &a0 [x]`, `a1: &a1 [*a0]`, ...) more than 10,000 links deep, which the
  library decodes today, a stack of one level a link that a chain within
  `max_response_bytes` could grow past Go's own limit and fail the process
  fatally, and a document without an alias that the parser's two bounds let
  through nested deeper than 10,000, which the library decodes too; that the
  exporter refuses such documents where the library decodes them is the
  accepted cost, a further way it MAY treat a document otherwise than the
  library does (below).
- Refuse a key written twice in one mapping, in the YAML library's words
  (`line 3: mapping key "a" already defined at line 1`, a problem for each
  two of them, named and counted as above), and decode every other document
  into the values, of the types, that the YAML library decodes it into,
  failing in the library's words where it fails. A document whose mappings
  have no more than 128 keys each, and whose keys written twice, if any, are
  problems of no more than 64 kB of text, counted as 64 bytes and the key
  for each — a key of one or two bytes written 45 times, one of three bytes
  written 44 times — MUST be decoded, and refused, by the library itself,
  unless it is nested past the bound above, which refuses any document
  whatever its mappings. Only a document past one of these MAY be
  treated otherwise than the library treats it, and only in this:
  - Nested past the bound above, it is refused for its depth before it is
    decoded, where the library, which bounds neither the whole of a
    document's nesting nor the stack of its own decoding through aliases,
    would decode it.
  - With keys written twice, its error is of the keys written twice alone,
    each mapping's listed once, where it is written. The library reported
    another error instead where it met one that ends the decoding, listed a
    problem of another kind among them, listed a mapping's problems again
    for each alias of it and after its other keys' when it is merged, and
    did not read a value that a merge leaves out, so that a document with a
    key written twice nowhere else was decoded: that document is refused.
  - A merge into a mapping one of whose keys, or of a merged mapping's, is
    a sequence or a mapping MUST be refused, as the library refuses it: the
    library reads such a key once more for the merge, and fails on what is
    wrong in it (`invalid map key`, `cannot decode ... as a !!int`, `anchor
    ... value contains itself`) or outright. The error MAY be another than
    the library's: that of the key (the next rule), of a later value, or of
    the merge (`map merge requires map or sequence of maps as the value`),
    whichever is met first.
  - A document is refused for what its aliases expand to (`document
    contains excessive aliasing`) at the share of aliased nodes the library
    refuses it at, counted for a part of the document at a time where the
    library counts node by node: a document at that share itself MAY be
    decoded where the library refused it, or refused where it decoded it.
    A document well under that share MUST NOT be refused for the aliases of
    one part of it: what the library is handed at a time, counted with the
    node made to hold it, MUST stay under what the library refuses alone.
- Refuse a document the YAML library refuses, or panics on, for a mapping
  key that is a sequence or a mapping, or an alias of one, in words that say
  so and where: `yaml: the mapping key at line 3 is a sequence; only scalar
  keys (text, numbers, booleans) can be read` (`a mapping`, `an alias
  of a sequence`), naming the first such key of the document in the order it
  is written. A decoded document's keys are the keys of JSON objects, which
  such a key cannot be, and the library's own words say nothing of it: one
  alone is `invalid map key: []interface {}{"a"}`, with no line, two
  different ones in a mapping are `mapping key "" already defined`, as the
  library tells keys apart by their text and these have none, one tagged
  `!!str` or merged among text keys is `cannot unmarshal !!seq into
  string`, and one in a merged mapping makes the library panic. The failure
  MUST be one to the log wherever in the document the key is (its line left
  out). A document with such a key that the library refuses for something
  else, or decodes, as it does one whose key is in a value a merge leaves
  out and never reads, MUST be refused, or decoded, as before.
- Refuse a document with a mapping two of whose keys, written otherwise,
  are one text in the decoded document, whose keys are text: `1` and `1.0`,
  `16` and `0x10`, `true` and `True`, `'1'` and `1`, a key and an alias of
  it. The library tells keys apart by how they are written and refuses none
  of these, and of two that decode into one value it keeps the later
  without a word. The error MUST be in the words of a key written twice,
  naming the key by that text and both lines (`line 2: mapping key "1"
  already defined at line 1`), named and counted as above; keys merged into
  a mapping with `<<` that are the text of one of its own MUST be refused
  too (`mapping key "1" is in a mapping twice, written otherwise each time,
  once merged into it with <<`). Which key is refused MUST NOT depend on the
  order of a Go map. A null key (`~`, `null`, or none) has no text and MUST
  be refused at its line: `line 2: mapping key "~" is null, which is no
  text; quote it to read it as text`.
- Fail the decode, and nothing more, of a document the decoding panics on.
  The YAML library panics on a merge into a mapping that has a key that is
  no text where a merged mapping has a sequence or a mapping for a key,
  which MUST be refused for that key (above); any other panic of the
  library MUST give the library's own words (`the YAML library failed on the
  document: runtime error: hash of unhashable type []interface {}`) and no
  stack. A panic of the exporter's own code MUST NOT be put on the library:
  whose it is MUST be told by the innermost function of the library or of
  the exporter that was running when it was raised, the standard library's
  passed over, and the exporter's own MUST say that it is a defect of the
  exporter, and where, the file, the line and the function (`the exporter
  failed on the YAML document (yamlkeys.go:<line> decode.(*yamlMap).set):
  interface conversion: interface {} is int, not string; this is a defect
  of the exporter and not of the document, please report it`), and no
  stack. A runtime error MUST be recognised by the log (§ 25.1) by its text
  up to the words `runtime error` and by the file and the function it was
  raised in (`the YAML library failed on the document: runtime error
  (decode.go yaml.v3.(*decoder).mapping)`), so that two runtime errors
  raised in two functions are two failures and one whose numbers change is
  one; the line MUST NOT be part of what any of these is recognised by. The
  panic MUST NOT leave the decoder.

Example:

```yaml
- name: application_yaml
  decoder:
    type: yaml

  transform:
    type: yq
  metrics:
    - name: application_requests_total
      description: Total application requests
      type: counter
      error_mode: log
      labels: []
      expression: '.status.requests'
```

---

# 11. XML decoder

The XML decoder MUST:

- Parse XML safely.
- Support XPath queries.
- Handle XML namespaces.
- Support extraction of attributes and element values.

Example:

```yaml
- name: application_xml
  decoder:
    type: xml

  transform:
    type: xpath
  metrics:
    - name: application_requests_total
      description: Total application requests
      type: counter
      error_mode: log
      labels: []
      expression: '/status/requests'
```

The implementation MUST document XPath behavior and namespaces.

A label's expression MUST be evaluated at each node its rule selects — an
element, a text node or an attribute — and an absolute path in it, one that
starts with `/` or `//`, MUST start at the document, as XPath has it and as
the same path does in a rule's expression, over XML and over HTML and
wherever it stands in the expression: at its start, in an argument, in a
predicate, in an operand or in a union. `/status/@site`, `//status/@site`,
`concat(/status/@site, '-', @id)` and `../x[@ref=//y/@id]` MUST read what
they say at every selected node; such a label MUST NOT be read as a path
beneath the selected node, which matches nothing and leaves the label off
without a word. `//name` in a label MUST be the first `name` of the
document, and the documentation MUST say that one beneath the node is
`.//name`. A label without an absolute path MUST be read as it was and
MUST NOT cost more for it: whether a label may reach the root MUST be
decided once for the rule, from its expression, and where that cannot be
told for certain the label MUST be taken to reach it.

A label whose expression cannot depend on the selected node MUST be
evaluated once for the rule and the response, and its value given to every
series of the rule; it MUST NOT be evaluated at each node, which walks the
document as many times as the rule selects nodes. That MUST be taken of two
shapes alone, whitespace around them aside, and decided once for the rule,
from the expression: (a) one absolute location path, `/` or `//` and steps,
with no operator, union, comma, parenthesis or function call outside its
predicates, inside which anything may stand, since a predicate is evaluated
relative to the path's own nodes; and (b) one call of `count`, `sum`,
`string`, `number`, `boolean`, `not`, `normalize-space`, `string-length`,
`name`, `local-name`, `round`, `floor` or `ceiling` with one such path for
its only argument. Every other expression MUST be evaluated at each node,
whether it depends on the node or not. What a label read once gives MUST be
exactly what its evaluation at the node gave, at every node, over XML and
over HTML and for a rule that selects elements, text nodes or attributes:
its text, trimmed; no label where it selects nothing or computes blanks; a
required label that is missing; the sum the exporter adds up; and the
failure of a sum over text that is no number, which MUST name each node as
before. Its text MUST be written once too, a sum's included, and not
formatted again at each node. A rule without such a label MUST cost what it cost. The
documentation MUST say which labels are evaluated once, and that any other
label with an absolute path in it walks the document at every node.

An XPath expression MUST be read to its end by the engine or be refused
where it is compiled, which is at load for a rule's expression and for a
label's, and in the configuration check. The engine's parser stops where
one complete expression ends and ignores what follows, so that an
expression with something after it is evaluated as its start. A closing
`)` or `]` outside a string that closes nothing MUST be refused with its
place, as `XPath "sum(//a))": the ")" at byte 9 closes nothing`, and
anything else that follows a complete expression — `//a 'x'`, `1 2`,
`//a/sum(b)` — with the byte where what would be ignored starts. A
predicate after parentheses within a path, as in `//a/(b)[1]`, of which
the engine's parser reads none, MUST be refused as that, and the error
MUST show where to write it: `//a/(b[1])`. Predicates that follow one
another after an expression in parentheses, a function call or a literal,
as `(//a)[@x][1]`, are read and evaluated in turn by the engine and MUST
NOT be refused. An expression with a NUL byte in it, which the
engine's scanner takes for the end of the text, MUST be refused with the
byte's place before anything is made of it, the key of the compiled
expressions included. The rule has no exception for depth: an expression
nested as deep as the engine's parser reads — 199 levels of parentheses,
predicates and function arguments — cannot be seen in one more pair of
parentheses, and MUST be refused as too deeply nested, with or without
something after it. An expression the engine reads to its end MUST NOT be
refused otherwise, a bracket in a string and an expression nested 198 deep
included. The search for the byte where what would be ignored starts MUST
be bounded, in the bytes of expression it compiles, by a constant: an
expression of up to 250 bytes MUST be refused with the place, and a longer
one MAY be refused without it, so that an expression of 100 KB is refused
in the time it takes to compile it.

The XPath engine compiles expressions it then panics on, by what a response
holds: `sum('abc')`, `sum(string(//a))` over text, `//a[contains(@x, 5)]`
once an `a` has an `x`, `substring(//a, '1')`, `substring(//a, 0 div 0)`.
A panic while a rule is evaluated — its expression, a label of it, the
nodes of a sum in either — MUST be that rule's failure, handled by its
`error_mode` like any other failure of the rule, and MUST NOT leave the
transform: the other
rules MUST give their series, `on_transform_error` MUST NOT be asked, the
failure MUST be counted, logged and held back on repeats as a rule's
failure is, and under `fail` the probe MUST answer as it does any rule's
failure. The rule MUST give no series for that response, and those it made
before the panic MUST be given back to `limits.max_metrics`. The rule
fails once, as a whole: under `log` and `ignore` the failures of its nodes
gathered in that transform before the panic MUST be replaced by the
engine's, so that the rule is reported with one failure and no missing
value, counted so in the self-metrics, and logged with the engine's
failure and not with that of the first node, which the log would hold back
as the repeat of an earlier scrape's. The error MUST
name the metric, the expression — a label's, with the label's name, when a
label was being evaluated — and the engine's own words, without a stack:
`metric "m" XPath "sum('abc')" cannot be evaluated: the XPath engine failed
on it: sum() function argument type must be a node-set or number`. A
runtime error, of the engine or of the exporter's own code, MUST be
recovered the same way and MUST read `runtime error: ...` there. What
follows `runtime error:` moves with the response, as the bounds of
`substring(//a, 0 div 0)` over texts of different lengths do, so such a
failure MUST be recognised by the log by its text up to and including
`runtime error:`, while its text is logged and answered in full; a panic
with the engine's own message MUST be recognised by its whole text. The
stack of a runtime error MUST be logged once for the failure at debug
level, under every `error_mode`, where the rule's failures are logged, and
MUST NOT be in the error's text or in a line of another level. The panic
MUST be recovered once for the rule and not at each node or label, so that
a rule the engine does not panic on allocates what it allocated, and a
compiled expression the engine panicked on MUST NOT be used again. This
holds over HTML as over XML.

A rule MAY select attributes, as `//job/@size`. Its labels MUST then be
evaluated from each attribute as XPath has it — `.` its value, `name()` its
name, `..` its element — and MUST NOT end the scrape with an error of the
XPath library's own. One label is not as XPath has it: `text()` MUST give
the attribute's value, as it always did and as it does over HTML, although
to XPath an attribute has no text node; configurations tell the series of
such a rule apart by it, and without it they are duplicates of one. Only the
bare label is so read. Any other expression that asks for a node beneath the
attribute — `./text()`, `text()[1]`, `node()`, `self::*`,
`descendant::text()`, or a function of one, as `normalize-space(text())`,
`string(text())` or `substring(text(), 1, 2)` — MUST give no label,
`contains(text(), '1')` MUST be false and `count(text())` 0, over HTML as
over XML, and the documentation MUST say to write them of `.`.

This MUST hold over HTML as over XML. An attribute an HTML rule selects, as
`//td/@data-value` or `//time/@datetime`, MUST have its element for its
parent: `../@name` — by the name as written, as below, `../@og:type` and
`../@2x` included — `../../@id`, `name(..)`, a path through `..` such as
`../../td[1]`, and an expression such as `concat(../@a, '-', .)` MUST read
what they read of the same document as XML, so that the series of the rule
are told apart by its element and the elements around it. A rule that selects
elements or text nodes MUST read what it read before attributes had parents.

An XPath expression that computes a value rather than selecting nodes — a
number, a string or a boolean, as `count(//job)`, `string(/s/@load)` or
`/s/@state = 'ok'` — MUST make one series of that value, a boolean as 1 or 0,
with its labels read relative to the document; a NaN, an empty string or a
string that is not a number MUST be the rule's missing value. A label
expression that computes a value MUST give its text, a number written
shortest. This holds for XPath over HTML too.

A `sum()` over nodes MUST have a value only when every node it adds up is a
number, and MUST NOT be exported as the sum of the others, which is what the
XPath engine computes: it leaves out, without a word, each node whose text
it cannot read as a number, a number with whitespace around it among them.
This holds for the calls evaluated in the expression's own context, outside
every predicate, which MUST be found once, when the expression is compiled,
and not at each node; a rule and a label without such a call MUST cost what
they cost, to the allocation.

- An expression that is one `sum(...)` and nothing else, whitespace around
  it aside, MUST be added up by the exporter: every node the argument
  selects, in the engine's order, its text trimmed of whitespace and read
  as the engine reads a number, so that a sum of numbers written without
  whitespace is the float the engine gives, bit for bit, and one of numbers
  with whitespace around them is their sum. A node that is not a number
  MUST leave the rule without a value: its missing value, as a computed NaN
  is, by `required` and `error_mode`, with an error naming the metric, the
  expression, the first such text, quoted as a value in an error is, and
  how many of the nodes are such.
- A `sum()` that is a part of a larger expression (`sum(//v) div
  count(//v)`, `round(sum(//v))`) is the engine's to compute. Its nodes
  MUST be read first as the engine reads them, untrimmed: where the engine
  would leave one out, the rule MUST be missing its value, and the error
  MUST name the sum and the text and say that whitespace around a number
  counts there.
- A label whose expression has such a `sum()` MUST be held to the same. A
  label has no missing value, so its series MUST fail as one with a label
  that cannot be read does, by the rule's `error_mode` and whatever
  `required` says, with an error naming the metric, the node, the label and
  the text, and the series of the rule's other nodes MUST be made.
- A `sum()` inside a predicate, and one whose argument is a number or a
  string and no node-set, are the engine's as before, and the nodes of a
  sum are read whether or not the engine would come to evaluate it; the
  documentation MUST say so. A string argument is the number it reads as,
  and one that is no number makes the engine panic, which is the rule's
  failure as above; the documentation MUST say what a string argument
  gives.
- A panic of the engine while the nodes of a `sum()` are read MUST NOT
  fail the rule by itself: that sum MUST be left to the engine, which gives
  the expression its value where it does not come to the call, as on the
  right of an `or` whose left is true, and fails the rule where it does.
- A prefix before the name MUST be passed over, as the engine passes over
  it: `fn:sum(//v)` alone MUST be added up by the exporter as `sum(//v)`
  is, and in a larger expression it is a part like any other.

Text that is `NaN`, `Inf` or `Infinity` is a number where a document writes
it (§ 18.1) and MUST be exported, and a `sum()` MUST add it up; a NaN an
expression computes — `number('n/a')`, a sum that comes to NaN — stays the
rule's missing value. The documentation MUST say why the two differ: the
first is the value the source gives, the second the engine saying it read
no number. All of this holds over HTML as over XML.

A label read from an element's text or from an attribute, or computed as a
string, MUST be trimmed of leading and trailing whitespace, as a CSS label
is, so the indentation of pretty-printed XML is no part of it. This holds
for the node's own attribute, `@title`, as for one read through a path,
`../@title`, over XML and over HTML; an attribute of blanks alone is an empty
value, which leaves the label off (§ 18.1).

`response.namespaces` MUST map prefixes to namespace URIs for every XPath
expression of the collector, its metrics' and its labels' alike. A prefixed
name in an expression MUST then match by the URI, whatever prefix the document
writes for that namespace, or none, and an expression using a prefix the map
does not have MUST be refused at load. Without `response.namespaces` a prefix
MUST be matched against the prefixes as the document writes them. The prefix
`xml` MUST always be bound to `http://www.w3.org/XML/1998/namespace`, which
XML reserves it for and no document declares: an expression using it, as
`@xml:lang` and `../@xml:lang` do, MUST load and match with
`response.namespaces` set or not, without the map naming it and whatever the
map holds for it.

A label expression that is `@` followed by the name of one attribute of the
node MUST be read from the node by that name as the document writes it,
where that is what it means:

- Over HTML, which has no namespaces, every name: whatever follows the `@`
  when it holds no `/`, `|`, `[`, `]`, `(`, `)`, `=`, `<`, `>`, `!`, `+`,
  `*`, `,`, `$`, quote or blank — `@xml:lang`, `@og:type`, `@v-on:click`,
  `@:href`, `@@click`, `@2x`, `@data-id` — whatever `response.namespaces`
  holds. The same name after one or more `../` steps MUST be read from that
  ancestor by the name as written, whether the node the rule selected is an
  element or not — a text node, as `//td/text()` selects them, has an
  element for its parent like any other — and an attribute the HTML parser
  gives a namespace, as `xlink:href` inside `svg`, MUST be found by its name
  as written too.
- Over XML, a plain name, `^@[A-Za-z_][\w.-]*$`, as `@name` or `@data-id`;
  and, while `response.namespaces` is not set, a plain name with a plain
  prefix, `@m:unit`, the prefix being the document's own.

Every other label expression, those starting with `@` included — over XML
with `response.namespaces` set a prefixed attribute such as `@x:unit`, whose
prefix MUST then mean what the map says and MUST be refused at load when the
map lacks it and it is not `xml`; over either a union, a predicate, a
comparison, a step after the attribute — MUST be compiled and evaluated as
XPath with the collector's namespaces, and checked at load (§ 24.2); it MUST
NOT be taken for an attribute's name, which leaves the label off without a
word. For a collector whose decoder is left to each response, unset or
`auto`, and which does not set `response.namespaces`, a label MUST
be accepted at load when the document of either kind would read it. One that
sets `response.namespaces` reads XML, the one kind of document namespaces
mean anything in: with the decoder left to each response its labels MUST be
checked at load as those of an `xml` decoder are, so that a label with a
prefix the map lacks, `@x:unit` or `../@x:kind`, and a name only HTML can
have, `@1x`, `@a:b:c` or `../@:kind`, are refused with the message the `xml`
decoder gives; where the label is one attribute's name as an HTML document
would read it, the message MUST go on to say that `response.namespaces` has
the labels checked as XML's and that `decoder.type: html` is the setting for
a target that answers HTML. An `html` decoder's labels MUST be checked as HTML's whatever
`response.namespaces` holds. On the scrape, a label that cannot be read in
the kind of document that arrived — XML, for a name only HTML can have, where
the load accepted it for HTML — MUST NOT be left off: the rule MUST fail,
handled by its `error_mode` as any failure of the rule is, with a message
naming the label, the kind of document and why it cannot be read there. A
rule that selects no node makes no series and MUST NOT fail for it.

The XML decoder MUST refuse a document whose elements nest more than 512
deep, the bound the HTML parser has for its own, as a `decode` failure saying
so; XML a pre-script leaves MUST be held to the same bound. The text of the
nodes a rule selects MUST NOT cost the product of their number and the depth
they nest to: where a selected node lies within the one selected before it,
what is beneath them MUST be read once, each node's text being the text the
document gives for it alone.

A compiled XPath expression MUST NOT be evaluated by two scrapes at once:
antchfx/xpath runs an expression's query tree in place. The expression cache
(`internal/expr`) MUST give each use a compiled copy of its own, from a pool
per expression, so scrapes of one collector, and collectors sharing an
expression, run concurrently without a data race.

---

# 12. HTML decoder

The HTML decoder MUST support parsing imperfect real-world HTML.

Support:

- XPath
- CSS selectors
- Bare element selectors such as `h1`, `title`, and `meta`.

HTML extraction MUST operate on the parsed document tree, not only on class or
ID selectors. For example, `selector: h1` MUST select an `h1` element and use
its text as the metric value. XPath MUST remain available for HTML structures
that are better expressed with XPath predicates or attributes.

Recommended implementation libraries:

- Go HTML parser for native parsing, and/or
- Python `lxml.html` for Python decoding when Python is selected.

Example conceptual configuration:

```yaml
- name: application_html
  decoder:
    type: html

  transform:
    type: css
  metrics:
    - name: application_server_cpu
      description: Server CPU utilization
      type: gauge
      error_mode: log
      items: '#servers tr:has(td)'
      expression: td:nth-child(2)
      labels:
        - name: server
          expression: td:nth-child(1)
        - name: environment
          value: production
```

For CSS transforms, the metric expression selects the element whose text is
converted to a number. Without `items` a metric is one value: the expression
MUST match at most one element, a match of several failing the rule under its
`error_mode` with an error pointing at `items`, and only static `value` labels
are allowed, an expression label being refused at load. With `items`, the
expression and each expression label are selectors within one selected
element, such as a table row: they MUST be matched against what is beneath
the item and nothing else, so neither the item itself nor an element around
it is a match. Use XPath when a value is the item itself, or when a label
must be read from a sibling or an ancestor of that element. A CSS selector
MUST read the text of the element it selects and never an attribute; a value
or a label that is an attribute is XPath's to read, and the documentation
MUST say so.

XPath equivalent SHOULD be supported.

Rules over HTML are written against what a browser shows of a page, so the
text of an HTML element, wherever the `css` and `xpath` transforms read it —
a value, a label, a cell of an item, the first node a label's XPath selects —
MUST be the text of every text node beneath the element, comments left out,
without the text that stands inside a `script` or a `style` element beneath
it, in `svg` too. A rule or a label that selects a `script` or a `style`
element itself, or a text node inside one, MUST read that text: a page's
data in `<script type="application/ld+json">` stays readable. Where selected
nodes nest (§ 11), each MUST still read the text it has alone.

The content of a `template` element MUST NOT be part of the document the
transforms read, as it is not part of a browser's: it MUST be detached when
the page is parsed, a page a pre-script leaves included, so that CSS
selectors, `items`, XPath expressions and labels match no node inside a
template and its content is no part of the text of an element around it. The
`template` element itself MUST remain, empty, with its attributes. An element
named `template` inside `svg` or `math` is no HTML template and MUST keep its
content. A declarative shadow root — an HTML `template` with a
`shadowrootmode` attribute, whatever its value, or the earlier `shadowroot`
attribute — is content a browser attaches to the element around it and
renders: its content MUST be kept, as children of the `template` element
that selectors and expressions match like any other, a `template` inside it
being emptied as above.

Nothing else is left out: an element hidden by an attribute or a style, the
markup inside `noscript` and `iframe`, which the parser keeps as their text,
an `object`'s fallback, a `textarea` and a `pre` MUST be read as the parser
gives them. What an expression itself asks of its library is the library's
and is not rewritten: XPath's functions and comparisons work on the string
value of a node and the `:contains()` of a selector looks in all its text,
scripts and styles included; the documentation MUST say that this differs
from the exporter's own reading of a selected node. An XML document has no
scripts, styles or templates: elements of those names there MUST be read like
any other. A page without a script, a style or a template MUST NOT cost a
rule more allocations to read than it did, nor its parse more than one
further pass over the nodes of the document.

---

# 13. CSV decoder

The CSV decoder MUST support:

- Header row
- Configurable delimiter
- Quoted fields
- Escaped quotes
- Empty fields
- Optional trimming of whitespace
- TSV as a delimiter configuration

CSV SHOULD normalize into an array-of-records model.

Example input:

```csv
server,cpu,memory,status
web01,72,61,up
web02,31,48,up
```

Normalized conceptual object:

```json
[
  {"server":"web01","cpu":"72","memory":"61","status":"up"},
  {"server":"web02","cpu":"31","memory":"48","status":"up"}
]
```

The normalized representation MUST be available to Python transforms and
pre-scripts as `data`. A `jq` or `yq` transform MUST refuse a CSV response
unless a pre-script turns it into structured data first; the `csv` transform
reads the rows directly. A header naming one column twice MUST fail the
decode naming the column, rather than one column silently overwriting the
other. A column with an empty header name that holds a value in any row MUST
fail the decode naming its number; one empty in every row, as a trailing
delimiter leaves, MUST be left out of the rows. With a header row, a row with
more fields than the header in which any field past the header's last holds
a value — one that is not empty, nor, under `response.csv.trim_space`,
whitespace alone — MUST fail the decode naming the line of the body that
value is on and its column number (`CSV line 5 has a value in column 5,
which the header does not name`), rather than the value being dropped: it
has no name to be read by, and a delimiter that is not the body's or a
quoted field not read as one shows in nothing else. Fields past the header's
last that all hold nothing MUST be left out, as a trailing delimiter's
column is; a row with fewer fields than the header MUST have empty text in
the columns it lacks; and without a header row rows MAY have any number of
fields each, and the first line of the body MUST be a row like the others:
there is no setting that skips lines. The memory the decoded rows take MUST
grow with the fields the body holds, not with the header's columns times the
rows: the empty text of a column a short row lacks MUST be what a rule, a
script and an error read in it without being stored in each row, so that a
header of 2,000 columns over 2,000 lines of one field, a body of 15 kB, is
decoded in a few megabytes at most rather than as four million cells.
Likewise the work of finding which unnamed header columns are empty in every
row MUST grow with the cells the rows hold, not with the unnamed columns
times the rows, so that a header of 50,000 unnamed columns over 50,000
lines of one field is gone through once. A
line MUST end at a line feed, with a carriage return before it or without
one, and at a carriage return that no
line feed follows, as a spreadsheet's "CSV (Macintosh)" ends its lines: a
body MAY end some lines one way and some another, and the lines an error
names MUST count each. Inside a quoted field a carriage return MUST be the
field's text, as a line feed is, but for one with a line feed after it, which
MUST be read as the line feed alone, and under `response.csv.trim_space` one
that ends a line MUST NOT be taken for whitespace before the field after
it. A quoted field left open has no end to be inside of: the body MUST be
refused with the error the same body gives with line feeds in place of the
carriage returns after that field's quote — the line the body ends on and
the column there, not the field's line and the length of the rest of the
body. A body without a carriage return that ends a line MUST be read from the
bytes it arrived in, without a copy. An empty line MUST be no row; a
line of whitespace alone MUST be a row, which holds no value; and a last
line without a line end MUST be read like the others. Quoting is no
setting: a field that starts with a quote MUST be read as a quoted field
whatever the delimiter, a tab included. A quote inside a field that
does not start with one MUST be read as written rather than failing the
decode; a quoted field left open, or holding a quote that is neither doubled
nor the one closing it, MUST still fail it, with the error it has in a body
without such a field, whatever the other fields of the body hold: it MUST NOT
be read on over the rows after it. `response.csv.trim_space` MUST trim the
whitespace on both sides of every field, with a header row and without one,
and a field whose first character after whitespace is a quote MUST be read
as a quoted field, the delimiters inside it kept. Whitespace between a
quoted field's closing quote and the delimiter or the end of the line after
it MUST be trimmed as well, whatever the delimiter, a doubled quote, a
delimiter and a line break inside the field kept as they are; anything else
after a closing quote MUST still fail the decode, and without `trim_space`
so MUST whitespace there. With a delimiter that is
itself whitespace and not a space, a tab above all, it MUST NOT take the
delimiter for whitespace of the field after it: an empty field MUST keep its
column, while whitespace other than the delimiter between the start of a
field and its opening quote MUST still be skipped, whitespace inside a
quoted field MUST be left as written, and an error MUST name the line and
column the body has it in. With a space as the delimiter the whitespace a
field starts with MUST be skipped, spaces included, so that a run of spaces
is one delimiter and columns aligned with spaces are read; an empty field
is written there as a quoted field with nothing in it (`""`), which MUST be
read as one. A space after a closing quote is the delimiter there, and only
other whitespace is trimmed after it. Without `trim_space` every space MUST
be a delimiter of its own, as any other delimiter is.

`response.csv.delimiter` MUST be exactly one character, and one the CSV
reader can split on: not a double quote, a carriage return, a line feed or
the Unicode replacement character. Anything else — two characters such as
the backslash and `t` of a single-quoted `'\t'`, a word such as `tab`, a
quote — MUST be refused when the configuration loads, with a message showing
the value and how a tab is written: `delimiter: "\t"` in double quotes, where
YAML reads `\t` as the tab character.

---

# 14. Prometheus decoder

The Prometheus exposition format MUST be supported as an input decoder.

Example input:

```text
# HELP http_requests_total HTTP requests
# TYPE http_requests_total counter
http_requests_total{service="api"} 123

# HELP queue_depth Queue depth
# TYPE queue_depth gauge
queue_depth 42
```

The decoder MUST convert source samples into the internal `MetricSet` representation.

It SHOULD preserve:

- Metric names
- Metric types
- Help strings
- Labels
- Timestamps where supported
- Histogram and summary series where feasible

Optional post-decode operations SHOULD support:

- Include/filter metric names
- Exclude metric names
- Rename metrics
- Add labels
- Remove labels
- Rename labels
- Filter based on labels

Example:

```yaml
- name: vendor_prometheus
  transform:
    type: prometheus
  metrics:
    - name: application_requests_total
      description: Vendor HTTP requests
      type: counter
      error_mode: log
      labels: []
      expression: '^vendor_requests_total$'
```

The implementation MUST avoid double-encoding Prometheus text. It must parse it into the common representation first.

## 14.1 Parser

The decoder parses the text exposition format, version 0.0.4, and
OpenMetrics 1.0 text (below), with its own
parser (`internal/decode/promparse.go`) rather than `github.com/prometheus/common/expfmt`. That
package was the only reason the binary carried `prometheus/common`,
`prometheus/client_model`, the protobuf runtime and `munnerz/goautoneg`; the
parser that replaces it is a few hundred lines, and dropping them cut the
binary by about 14%. The exporter MUST NOT depend on those modules again, and a
test fails if `go.mod` requires them.

The parser MUST follow expfmt's rules:

- A sample belongs to the family named by an earlier HELP or TYPE line, or by
  the sample itself. For a summary or histogram family `foo`, `foo_sum` and
  `foo_count` (and, for a histogram, `foo_bucket`) belong to `foo`; for any
  other family they, like an OpenMetrics `_total`, are families of their own.
- A family without a TYPE line is untyped. A TYPE line after the family's first
  sample, and a second HELP or TYPE line, are errors. The type is
  case-insensitive; `gauge_histogram` and anything else outside counter, gauge,
  summary, histogram and untyped are errors.
- Summary and histogram series are grouped by their labels without `quantile`
  and `le`, regardless of label order, and those two labels MUST be floats.
  Two different label sets MUST be two series whatever bytes their values
  hold, a value that is not UTF-8 among them.
- Metric and label names MAY be quoted UTF-8, including the braces form
  `{"my.metric", key="value"} 1` for a metric name that is not a bare name.
  `__name__` is reserved, and a label name may appear once per sample.
- HELP text and label values unescape `\\`, `\n` and `\"`; any other escape is
  an error. A label value that is not valid UTF-8 MUST NOT fail the parse: it
  is repaired with U+FFFD and counted after the transform, as every decoder's
  output is (§ 6.1a).
- A value is a Go float without `p`, `P` or `_`, so `NaN`, `+Inf` and `-Inf`
  are accepted and hexadecimal floats and digit separators are not. A
  timestamp is an integer number of milliseconds.
- Comments other than HELP and TYPE, including OpenMetrics' `# EOF`, are
  ignored. A family that ends up without samples is dropped.
- Every error names its line: `text format parsing error in line N: ...`. An
  error about a histogram or a summary as a whole, which is not on one line,
  names the series and the line of its first sample: `text format parsing
  error: the histogram h{op="get"}, which starts in line N, ...`.

It deliberately differs from expfmt where expfmt was wrong for a scrape target:

- It accepts a body whose last line has no newline, CRLF line endings, and
  trailing blanks after the value, the timestamp or the TYPE; expfmt rejected
  all three.
- It rejects a histogram or summary count, or a bucket count, that is negative,
  NaN or infinite, which expfmt silently turned into an arbitrary integer, and
  one that is a fraction, which would otherwise lose it: a count MUST be a
  whole number, in any spelling (`7`, `7.0`, `1e3`). A count a pre-script
  leaves (§ 16) MUST be held to the same rule.
- It keeps a histogram or a summary as the target wrote it. One without a
  `_sum` or without a `_count`, which OpenMetrics allows and expfmt read as a
  sum or a count of 0, MUST be marked as having none, so that none is
  exported (§ 21); a histogram without a `_count` MUST take its count from its
  `+Inf` bucket, and one with a `_count` and no `+Inf` bucket is given the
  bucket when it is written, as before.
- It MUST NOT reject a histogram whose `+Inf` bucket and `_count` differ, nor
  one with neither of the two. A target that updates its buckets and its
  count without a lock is scraped between the two now and then, and one such
  series MUST NOT fail every family of the scrape. The histogram MUST be
  kept with both numbers as they were read, the `+Inf` bucket's own count
  beside the `_count`, or with neither, and § 21 says how each output writes
  it.
- It leaves out a sample line that belongs to a histogram or summary family
  by its name and is none of its samples: under a histogram `h`, a sample
  named `h`, or `h_bucket` without an `le` label; under a summary `s`, a
  sample `s` without a `quantile` label. Client libraries write such lines
  and Prometheus ingests them (Micrometer's `x{quantile="0.95"}` under
  `# TYPE x histogram`, VictoriaMetrics' `x_bucket{vmrange="..."}`), so the
  line MUST NOT fail the decode: every other sample of the family and every
  other family MUST be read as if the line were not there. The value of such
  a line has no place in a series, so it MUST NOT make a series, of its
  family or of a second family of the same name, which an exposition cannot
  hold; and it MUST NOT be dropped silently. The lines left out of the
  families that are kept MUST be reported with the decoded series, as their
  number and, for the first, its line, the sample and the samples the family
  has (`line N: expected h_bucket with an le label, h_sum or h_count as a
  sample of the histogram h, got h`); they MUST be counted in
  `http_exporter_decoder_lines_skipped_total` and logged at warn level, once
  for a scrape with that first line and the number, repeats suppressed and
  the end logged as the failure log does (§ 25.1), per collector, target and
  file. A line that is malformed as syntax MUST still fail the parse. An
  OpenMetrics `_created` sample is no such line, and is read and dropped
  uncounted (below).
- It MUST NOT reject a value the text format allows because OpenMetrics does
  not allow it a series of its type: a counter or a `_sum` that is NaN or
  negative, buckets or quantiles out of order, bucket counts that fall, a
  `le` of NaN, a quantile outside 0 to 1. Such a series is passed on, and § 21
  says how each output writes it.
- It rejects what cannot be one series, where expfmt kept the last sample or
  wrote both: a second `_sum` or `_count` sample of a series, naming its
  line; and, naming the series with its labels and the line of its first
  sample, two buckets with one upper bound however written (`le="1"` and
  `le="1.0"`) and two values of one quantile.
  A series the collector's rules do not keep is not held and so is checked
  only sample by sample.
- It rejects a label set with no metric name, such as `{a="b"} 1`, which
  expfmt attached to the previous line's family, and names that mix bare and
  quoted parts, such as `a"b"`, which expfmt spliced together.
- It leaves a label written with an empty value off its series, which
  expfmt kept: `m{l="",k="v"}` MUST be read as `m{k="v"}`, since Prometheus
  reads a label with an empty value as none and no series the exporter makes
  has one (§ 18.1). An exposition MUST be read as the same exposition without
  those labels is read: the same series, in the same order, or the same
  error. The label MUST be read and checked as any other before it is left
  off, so what was refused about it still is, in the same words: a label
  name written twice on a sample, when one of its values or both are empty,
  and an empty `le` of a histogram's sample or `quantile` of a summary's,
  which is a bound that is no float. On a series without buckets or
  quantiles, an OpenMetrics gauge histogram's `foo_bucket` among them, `le`
  and `quantile` are labels as any other and MUST be left off when empty.
  The label MUST be off before a series is made of the sample, so that a
  histogram's or a summary's samples are grouped without it, and a
  pre-script, a `python` transform and the rules of a `prometheus`
  transform are given the series without it. Two series a target tells
  apart by nothing else are then one: a histogram's or a summary's samples
  written so MUST be one series' samples, refused where that series has a
  sample twice, and two plain series MUST both be returned, without the
  label, for validation to refuse as the duplicates they are (§ 21).
  Reading an exposition that has no such label MUST cost no allocation it
  did not cost.
- It never panics. expfmt panicked on inputs such as `{b="c",} 1`, which a
  target could serve to crash the exporter from a static target scrape, where no
  HTTP handler recovers the panic.

Families are returned in the order they were first seen, and series in the
order of their first sample, so a decode is deterministic.

The body MUST be read as OpenMetrics 1.0 text when its `Content-Type` is
`application/openmetrics-text`, or, when the `Content-Type` does not say
`text/plain; version=0.0.4`, when its last line is `# EOF`. The same parser
reads it (`internal/decode/openmetrics.go`), without `prometheus/common`, and:

- a timestamp MUST be read as a float number of seconds and kept in
  milliseconds;
- an exemplar after the value or the timestamp MUST be skipped;
- a counter family `foo`'s sample `foo_total` MUST be the counter `foo_total`,
  with the family's type and help, and a counter family named `foo_total` MUST
  be read the same way;
- `_created` samples of counters, histograms and summaries MUST be dropped,
  since the model has no creation time;
- `unknown` MUST be read as untyped, `stateset` as a gauge, `info` as the
  gauge `foo_info`, and `gaugehistogram` as the gauges `foo_bucket` (with `le`
  a plain label), `foo_gcount` and `foo_gsum`, each with the family's help;
  any other type MUST be an error;
- `# UNIT` lines MUST be ignored, and a line other than a blank one after
  `# EOF` MUST be an error; a body without `# EOF` is accepted.

The text format 0.0.4 MUST be read as before: integer millisecond timestamps,
`_created` and `_total` as families of their own, and `unknown` an error. The
http request type sends no `Accept` header of its own; a collector asks for
OpenMetrics through `request.headers`.

For a `prometheus` transform without a `pre_script`, the decoder MUST keep only
the series the transform passes on — those a rule's expression (or name)
matches, or, without rules, those `include` and `exclude` pick — and MUST stop
at the first kept series past `limits.max_metrics` with the limit's error
(§ 20). The other lines MUST still be parsed and checked. With a
`pre_script`, which is given every series, every series MUST be kept.

The decoder MUST stop only for series the transform is sure to make a series
of. A rule whose `error_mode` is `log` or `ignore` carries on without a
series it cannot make: one its `type` cannot apply to, a histogram or a
summary under another type and any other metric under one of those two; a
histogram or a summary under a `scale`; and a series that lacks a label the
rule requires. A series of a metric that no rule matching its name is sure
to keep — every such rule carrying on, and setting a type or a scale that
cannot apply to a metric of that type, or requiring a label — MUST be kept
and MUST NOT count while it is decoded: the transform counts the series it
makes of them (§ 20). A scrape of which the rules keep no more series than
the limit MUST NOT be refused for the series they carry on without. A series
of a metric that one matching rule is sure to keep, a rule under `fail`
among them, MUST count as before, so that the decoder stops as it did for
every collector none of whose rules can carry on without a series.

---

# 15. Plain text decoder

The text decoder MUST support arbitrary text responses.

At minimum support:

- Raw body access
- Regex extraction
- Capture groups
- Optional labels from capture groups
- Numeric conversion

Example:

```yaml
- name: legacy_text
  transform:
    type: regex
  metrics:
    - name: application_connections
      description: Current application connections
      type: gauge
      error_mode: log
      labels: []
      expression: 'Connections:\s+(\d+)'
```

Complex parsing can use Python. Prefer a `transform.pre_script` that reshapes
the response into structured data, so the metric declaration stays identical to
every other transform; use the Python transform when metric names are not known
until the response is read.

---

# 15a. Graphite decoder

The `graphite` decoder MUST read Graphite series from either of:

- the render API's `format=json` answer, a body starting with `[`: a list of
  `{"target", "tags", "datapoints": [[value, timestamp], ...]}`;
- carbon plaintext lines, `<path> <value> [<timestamp>]` one a line, blank and
  `#` lines skipped. A missing timestamp, or `-1`, MUST be the time of the
  decode. A timestamp of 10¹¹ or more MUST be refused as one in milliseconds,
  naming the line. A value or a timestamp with an underscore (`1_000`) or in
  hexadecimal form (`0x1p-2`) MUST NOT be read as a number, as text a rule
  reads is not (§ 18.1): its line is not a carbon line, in a body still
  recognised as carbon lines by its content (§ 6). Lines of one path and
  one set of tags, in any order, MUST be one series with a point per line.

For a `graphite` collector the answer MUST be render JSON: any other, empty
included, MUST fail the decode saying the Graphite server did not answer with
render JSON and quoting the start of the answer, never be read as carbon
lines.

A tagged name, `path;tag=value;...`, MUST be split into its path and tags
at the first `;` outside brackets and quotes, so a function's result named
after a tagged argument, `movingAverage(cpu.load;env=prod,'5min')`, keeps that
name as its path; the tags after it MUST then be split on every `;`, since a
tag value may hold a quote or a bracket, `owner=o'neil`, which MUST NOT swallow
the tags after it; a render target that does not read as a tagged name but whose
tags the render API gave MUST be kept as its name. The
decoder MUST produce one document for every transform:

```json
{"series": [{"path": "a.b", "segments": ["a", "b"], "tags": {"name": "a.b"},
             "value": 42, "time": 1727000000, "points": [[40, 1726999940], [42, 1727000000]]}]}
```

- `path` MUST be the series' name without tags; `segments` it split on dots;
  `tags` the render API's tags when it gives them, else those of the tagged
  name, always holding `name`, the path when the render API's tags lack it.
- `points` MUST be every point with a finite value and time, oldest first,
  points for one time kept in the order read. A `null`, NaN or infinite value
  MUST be left out, graphite-web's `1e9999` and `-1e9999`, out of the float
  range, read as infinities rather than failing the decode, and a series left with no point MUST be left out of the
  document.
- `value` MUST be the points reduced by `response.graphite.value`: `last`, the
  default, the newest point, the last read among points for one time; or
  `max`, `min`, `avg` or `sum` over every point. `time` MUST be the newest
  point's time in Unix seconds. Samples MUST NOT be exported with it as a
  timestamp.
- A series whose newest point is older than `response.graphite.max_age`, when
  set, MUST be left out. Its age MUST be computed from whole and fractional
  seconds, without overflow for any time a float can hold.
- A render series equal to an earlier one — path, tags and points — MUST be
  left out; one with the same path and other points or tags MUST be kept.
- Series MUST keep the order the answer or the file gave them.
- A malformed answer — a point that is not two numbers, a series without a
  path, a tag that is not `name=value` — MUST fail the decode, naming the
  series. A line that is not a carbon line MUST fail it naming the line
  number, unless `response.graphite.invalid_lines` is `skip`, when it MUST be
  left out and the rest read.
- What was left out — series with no point, older than `max_age` and repeated,
  and skipped lines, the first described — MUST be reported with the
  document. The series left out MUST be counted in
  `http_exporter_decoder_series_left_out_total` and logged at debug level with
  the count for each reason; skipped lines MUST be counted in
  `http_exporter_decoder_lines_skipped_total` and logged at warn level with
  the first of them, repeats suppressed and the end logged as the failure log
  does (§ 25.1), per collector, target and file.

`response.graphite` MUST be refused at load on a collector whose decoder is
neither `graphite` nor `auto`, as must a `value` outside the five, an
`invalid_lines` other than `fail` or `skip`, and a negative `max_age`. A collector whose decoder is `graphite` MUST have a `jq`,
`yq` or `python` transform; any other MUST be refused at load.

---

# 16. Python transform

Python MUST be a first-class transform type.

It is not a fallback or a separate decoder. It MUST be documented alongside
the jq/yq, XPath, CSS, CSV, regex, and Prometheus transforms.

The configured response decoder runs before the Python transform. Python
scripts MUST operate only on data already fetched by the Go exporter.

Python MUST NOT need `requests`, `httpx`, `urllib3`, `socket`, or equivalent networking libraries.

### 16.1 Python inputs

Expose a clean response context, preferably:

```python
response.status_code
response.headers
response.header(name, default=None)
response.body
response.text
response.json()
response.yaml()
```

Also expose:

```python
target
collector
```

Scripts can operate directly on the raw response or decoded structures.

`target` MUST be the target as given, credentials included; the
documentation MUST say so, and that a script printing it or labelling with it
exposes them.

`response.headers` MUST be each header's values as a list, by canonical name.
`response.header(name)` MUST return the values of the header of that name, in
any case, joined by `, `, as jq's `$headers` has them, and `default` when the
response has none.

`data` MUST be the decoded response: `None` for `none`; the document for JSON, YAML,
graphite, NDJSON, logfmt, TOML and INI; the rows for CSV, dicts by header or lists without one; the body as
a string for text, HTML and XML; and, for Prometheus exposition,
`{"metrics": [...]}` with a dict per series of its `name`, `type`, `help`,
`labels`, and `value` — or a histogram's `buckets` (`le`, `count`), `sum` and
`count`, or a summary's `quantiles` (`quantile`, `value`), `sum` and `count` —
and its `timestamp` in milliseconds when it has one. A histogram or summary
read without a `_sum` or a `_count` MUST have no `sum` or `count` key, and one
a pre-script leaves without the key, or with `None` for it, MUST be read back
as having none; what a pre-script leaves MUST be checked as the decoder
checks a target's (§ 14.1): duplicate bounds or quantiles MUST fail the scrape
naming the series. A histogram's `count` and the `count` of the `+Inf` entry
of its `buckets` MUST each reach the script as the target wrote it, also when
they differ, and MUST each be read back as the script left it: a `count` that
is not the `+Inf` bucket's, and a histogram left with neither, MUST be passed
on as a target's is (§ 14.1), not refused. A histogram left with no
`buckets`, no `sum` and no `count`, and a summary left with no `quantiles`,
no `sum` and no `count`, which the decoder never reads from a target and a
misspelled key leaves, MUST fail the scrape, naming the series, the keys
such a series has and the keys it has; it MUST NOT be exported as a TYPE
line without a sample. NaN and the infinities
MUST reach a script as Python floats, and MUST come back as floats, in a
pre-script's `data` and in `metric(...)`, although JSON, which the exporter
and its workers exchange, has no form for them. The numbers of a worker's
answer MUST be read as the JSON decoder reads a response's, integers of any
length as integers, so an ID above 2^53 that a pre-script passes through keeps
every digit rather than being rounded to a float64.

### 16.2 Metric API

Expose a simple Python API such as:

```python
metric(
    name="server_cpu",
    type="gauge",
    value=72,
    labels={"server": "web01"}
)
```

Also provide useful helpers where appropriate:

```python
log(...)
fail(...)
```

The Python API MUST ultimately produce the same internal `MetricSet` as every other decoder.
`metric(...)` MUST take a value as the other transforms do — a number, a
numeric string, a boolean as 1 or 0 — and refuse anything else naming the
metric; a timestamp MUST be milliseconds, a float cut to whole milliseconds,
and a non-finite one, or one beyond an int64 of milliseconds, refused. A
metric a script appends to `metrics` itself MUST be read the same way: its
value and timestamp as `metric(...)` reads them, `None` refused, its label
values written as label text with `None` leaving the label off, and a missing
type `gauge`. A label a script gives as the empty string — to `metric(...)`,
in an entry it appends, or on a series a pre-script leaves a `prometheus`
transform — MUST be left off its series, as `None` is and as an expression
label with an empty value is (§ 18.1). It MUST be left off where the
script's answer is read, so that the rules of a `prometheus` transform,
`truncate`, the check for duplicate series, the text exposition and OTLP all
see the series without it: the rule is about the value the script gave. A
label a target's own exposition writes with an empty value is left off by
the decoder, with a pre-script or without one (§ 14.1), so a pre-script
that leaves `data` as it got it MUST change nothing of what the collector
answers. A name, type or help that is not a string (`None` for the type
or the help being none given), labels that are not a mapping, and an entry of
`metrics` that is not a mapping MUST fail the scrape with a message naming the
metric and the argument, whether given to `metric(...)` or appended; the
message MUST NOT be the JSON decoder's error about the exporter's own types.

A metric is flat: its name, type, value, help and timestamp are one value
each, and its labels a mapping of one value each. A list or a dict where one
value belongs — given to `metric(...)`, or in an entry appended to `metrics`,
or as such an entry — MUST fail the scrape as a script's failure (§ 16.7),
with a message that names the metric where it has a name, the part of it,
and what stands there by its kind and the number of its items, as a value is
named elsewhere (§ 18.1): `metric "m" help an array of 3 items is not a
string`, `metric "m" label "l" is an array of 3 values, not a single value;
select one, or join them with join(",")` for an entry, `metric 'm' help a
list of 3 items is not a string` from `metric(...)`. The message MUST be the
same whatever the list or the dict holds and however deep it nests, a list
that holds itself included: it MUST NOT be written out in the message, and
the failure MUST NOT be the interpreter's `RecursionError` or a traceback of
the worker's own frames. A key of an entry that no metric has MUST NOT be
read, whatever it holds.

A series typed `histogram` MUST have buckets and one typed `summary`
quantiles, and a series with either MUST have that type; a `prometheus` rule
MUST NOT give a histogram or summary another type, nor another series one of
theirs. A count of observations MUST be refused outside 0 to 2^64-1.

A pre-script of a transform reading HTML or XML (`css`, `xpath`) MUST leave
`data` a string, which is parsed again; anything else MUST fail the scrape
naming what the script left and that a `jq` or `yq` transform reads structured
data.

A pre-script of a `prometheus` transform MUST receive the series as
`{"metrics": [...]}`, as a Python transform does, and what it leaves MUST be
read back into series for the rules, anything not of that shape refused naming
the series. A series' `help` or `type` that is neither a string nor `None`,
and `labels` that are neither a mapping nor `None`, MUST fail the scrape as
a script's failure (§ 16.7), with a message naming the series, the key, what
stands there by its kind and the number of its items (§ 18.1) and what
belongs there: `data["metrics"][0]: up help 5 is not a string`,
`... up labels are an array of 1 item, not a mapping of label names to
values`. They MUST NOT be read as none given, which exported the series
without its help, untyped, or without its labels, as another series than
the script meant. `None` for one of the three MUST be none given, as the key
left out is, and of several the first MUST be reported, in the order type,
help, labels. A type that is a string and no metric type MUST be read as
written and refused by validation, as a rule's is. A label value a
pre-script left in a CSV row MUST be written as
label text, `None` leaving the label off.

What a script prints MUST be kept to its first 4 KiB, logged at debug level,
and counted against `limits.max_output_bytes` only as far as that.

A script and a pre-script MUST have the value-parsing helpers of § 17,
`parse_bytes`, `parse_duration`, `parse_time`, `parse_percent` and
`parse_quantity`, as functions of one argument among the names they are
given, as `metric` and `fail` are. Each MUST give the same number as the jq
function of its name for every input, a `float`, or an `int` or a `float`
given to `parse_bytes`, `parse_duration` or `parse_quantity` back unchanged (a `bool` being no number), and MUST raise a
`ValueError` with exactly the jq function's message where that fails on
text, and a `TypeError` saying what the helper reads where it fails on
anything else. The exception MUST fail the script as any other does (§
16.7), the helper's own frames, which are the worker's, left out of the
traceback.

### 16.3 Python library model

Only a small curated set of third-party libraries should be bundled with the exporter.

The project MUST document supported versions and imports.

#### Always available: Python standard library

At minimum:

```text
base64
collections
csv
datetime
decimal
functools
hashlib
itertools
json
math
re
statistics
string
time
urllib.parse
xml.etree.ElementTree
```

The standard library MAY be broader than this list, subject to security constraints.

#### Bundled third-party libraries

Initial recommended set:

| Package | Import | Purpose |
|---|---|---|
| lxml | `lxml` | XML parsing, HTML parsing (`lxml.html`) and XPath |
| PyYAML | `yaml` | YAML parsing |
| python-dateutil | `dateutil` | Date/time parsing |

Do NOT bundle networking clients merely for Python convenience.

BeautifulSoup (`beautifulsoup4`) MUST NOT be bundled. `lxml.html` does its job,
and it could not run in the sandbox anyway: it imports `logging`, which imports
the blocked `threading` module. A collector that declares `beautifulsoup4` or
`bs4` MUST fail validation with a message that points to `lxml.html`.

Initially do NOT bundle general-purpose data science packages such as `pandas`, `numpy`, or `scipy` unless a concrete project requirement is later established.

### 16.4 `required_libs` / `libraries`

The collector configuration MUST allow Python scripts to declare third-party dependencies.

Preferred syntax:

```yaml
transform:
  type: python
  libraries:
    - lxml
    - PyYAML
  script: |
    import lxml.html
    from lxml import etree
    import yaml
```

The implementation may use `required_libs` instead, but the field must be clearly documented.

These declarations are metadata/validation, not an instruction to perform `pip install` during a scrape. The declared libraries are also imported when a worker starts (§ 16.7), so their import time is not counted against `script_timeout`.

The exporter MUST validate that declared libraries are from the supported bundled set.

The exporter SHOULD fail collector configuration validation for unsupported libraries.

### 16.5 Python network/process restrictions

Python scripts MUST NOT have direct access to:

- outbound network sockets
- arbitrary process creation
- shell execution
- package installation
- arbitrary subprocesses
- unrestricted filesystem access

At minimum, prohibit/restrict:

```text
subprocess
os.system
socket
ctypes
multiprocessing
threading
```

The exact sandboxing mechanism is an implementation decision. Trusted collector configuration may permit a broader runtime than untrusted configuration, but the default should be conservative.

The worker's sandbox:

- MUST start the interpreter without writing bytecode caches (`-B`), as the
  script check does. Where a module's cache is missing or stale and its
  directory writable, the importer would otherwise write one through the
  replaced `_io.FileIO` below, whose refusal it does not expect, and the
  import would fail.
- MUST refuse importing, from anywhere, `socket`, `ssl`, `subprocess`,
  `ctypes`, `multiprocessing`, `threading`, `mmap`, `pty`, `pathlib`, `shutil`,
  `tempfile`, `urllib.request`, `urllib.error` and `urllib.robotparser`, and
  the C modules beneath them (`_socket`, `_ssl`, `_posixsubprocess`,
  `_ctypes`, `_multiprocessing`), and MUST remove them from `sys.modules`, so a
  module a declared library loaded is not reachable there.
- MUST refuse a script importing, itself, `posix`, `_io`, `_thread`, `select`,
  `selectors`, `fcntl`, `termios` and `importlib`, whose `import_module` would
  go around the import guard. The standard library may still import them, so
  modules such as `dataclasses` keep working. `posix` MUST also be removed from
  `sys.modules`.
- MUST replace `open`, `io.open`, `io.FileIO`, `_io.FileIO` and the `os`
  functions that start processes, open, list, change or remove files, or read,
  write, duplicate or close descriptors. `_io.open`, which the importer reads
  module source and bytecode with, MUST only open `.py` and `.pyc` files, for
  reading. All of them MUST still open time zone data for reading: files
  whose resolved path is under a directory of `zoneinfo.TZPATH` or the usual
  system zone directories, `python-dateutil`'s bundled zoneinfo or the
  `tzdata` package, and `/etc/localtime`, so named zones work; any other
  file, a path leaving those directories by `..` or a symlink included, MUST
  be refused. The image MUST ship the system's zone data. `zoneinfo` MUST
  be importable by a script on every supported Python: from 3.12 it loads
  `sysconfig`, which imports the blocked `threading`, so the worker MUST
  import `zoneinfo` before installing the sandbox and MUST then drop
  `sysconfig`'s reference to `threading`, so the blocked module is not left
  reachable through it.
- MUST import `tokenize` before installing the sandbox and MUST make the
  `open` it reads files with (`tokenize._builtin_open`) open only what
  `_io.open` does: `.py` and `.pyc` files and time zone data, for reading.
  A traceback reads a library frame's source line through `linecache` and
  `tokenize.open`, and that line MUST be shown on every supported Python;
  Python 3.13 imports `tokenize` only when `linecache` first reads a file,
  after the sandbox, where the read is refused. `tokenize.open` and
  `linecache` MUST NOT read any other file for a script.
- MUST run on the image's Python, 3.12, the supported one; the test suite
  MUST also run the sandbox, a library's traceback and how a script's error
  is cut on Python 3.13 where it is installed, and say where it is not.

A sandbox inside the interpreter guards against a script doing by mistake what
it should not; it is not a boundary against a script written to escape it,
since Python leaves too many ways to reach the objects it replaces. The
collector configuration MUST be trusted as the exporter's own code is, and the
container — a non-root user, a read-only root file system, and network policy —
is the boundary.

### 16.6 Python execution controls

Support configurable/default:

- Execution timeout
- Maximum output/metric count
- Memory/resource limits where technically feasible
- Error reporting with collector and script context

### 16.7 Python workers

Scripts MUST run in long-lived worker interpreters, not in a new process per
scrape: starting CPython and importing a library costs far more than running a
typical script.

- A worker MUST only run the scripts of one collector. Workers MUST be pooled
  per interpreter path, collector, declared libraries, output limit and script
  text, so no collector can see or disturb another's module state, and a
  reloaded script gets new workers.
- Each run MUST get fresh globals holding the names scripts have always had
  (`data`, `response`, `target`, `collector`, `metric`, `fail`, `Response`,
  `metrics`, `json`, `sys`, `os`, `io`, `contextlib`, `builtins`).
- Requests and answers MUST travel on dedicated descriptors (3 and 4), one JSON
  document per line, never on stdin or stdout, so a script that prints or reads
  stdin cannot corrupt the protocol. A script's stdout and stderr MUST be
  captured per run.
- The sandbox (§ 16.5) MUST be installed once per worker, after the declared
  libraries (§ 16.4) are imported and before any script runs. Declared libraries
  MUST be imported at start-up, so their import time is not the script's, and so
  a library that itself imports a module the sandbox blocks still loads.
- The response's body MUST be sent to a worker once: `response.text`,
  `response.body` and, where the script's `data` is the body as text, `data`
  MUST be one string in the worker, not a copy each in the request.
- The csv decoder's rows by header MUST be sent to a worker as the header's
  columns once and each row's cells, as long as the body, and the worker MUST
  make of them the list a script reads, a dict of every column the header
  names to its cell, `""` for a cell the row is short of; they MUST NOT be
  sent as that list written out, which is as long as the header times the
  rows. A request MUST be no longer than 32 times the response limit and
  `limits.max_output_bytes` together, and the script, and one longer MUST be
  refused while it is written, before it is written to its end and before
  any worker reads it, failing the transform or the pre-script with `python
  transform did not run: the response and the data decoded from it come to
  more than N bytes written out for the worker, ...`.
- The hand-over MUST NOT show in what a script is given, nor in what its
  answer is read as, however the lines are written and read. A request MUST
  carry `data` as the decoder made it: an object's keys in the order of their
  bytes, which is the order a script iterates a dict in, a float that is whole
  (`1.0`) as a whole number, a number the `json` decoder kept as text as text,
  and a byte that is not UTF-8 as U+FFFD. The bytes of a JSON response MUST
  NOT be handed to a worker in place of the decoded document, which a script
  would read as another: its keys in the target's order, `1.0` as a float,
  `1e400` as `inf`. A worker's answer MUST be the line it is when every value
  of it is walked for NaN and the infinities; a worker MAY write without that
  walk an answer of dicts, lists, tuples, strings, booleans, `None` and finite
  numbers, each of exactly that type, nested no deeper than half the
  interpreter's recursion limit less ten, and a transform's no deeper than
  its metrics' label values, and MUST walk any other it writes, so that
  `limits.max_output_bytes` bounds what it bounded. An answer MUST be read
  into the series, the
  data or the error it holds whichever way it is read: an entry of `metrics`
  that is not what `metric(...)` appends MUST be checked as § 16.2 says, a
  metric past `limits.max_metrics` MUST be refused before one that is wrong,
  and a line that is no answer MUST fail the run.
- The hand-over MUST NOT bound how deep `data` nests: a response that
  decodes MUST reach its script, and what a pre-script leaves in `data` MUST
  be read back, nested as deep as a decoder makes a value, whichever Python
  runs the script. That depth MUST be stated once, with the decoders — it is
  the one limit of them all, the 10,000 levels of a JSON document (§ 9), to
  which a YAML document is held as well (§ 10) — and whatever writes a
  request, reads an answer or runs in a worker MUST take it from there,
  counting the object its own line puts around `data` apart, so that no two
  of them are equal only by having been written alike. A worker's
  interpreter bounds how deep its `json` module
  follows a document: by the recursion limit, 1000, up to Python 3.11; from
  3.12 by a limit of its own on nested calls in C, 10,000 in a release build
  on Linux, which no script changes; from 3.14 by what the stack holds. Where
  `json` refuses a request, or what a pre-script left in `data`, for its
  depth, the worker MUST read or write it without a call for each level, into
  the value and the line `json` makes of what it does not refuse, and MUST
  NOT raise the interpreter's recursion limit to do so, which trades the
  error for the end of the interpreter. Every other request and answer MUST
  be read and written as it was. `data` a pre-script leaves nested deeper
  than a decoder makes a value, or holding itself, MUST fail the run as the
  script's failure, saying how deep `data` may nest; the worker MUST outlive
  it, as it does the deepest it carries. What a transform appends to
  `metrics` is not a response's and is not carried: a metric is flat
  (§ 16.2), and nothing is read of what is nested inside a list or a dict
  that stands where a metric has one value, or under a key no metric has.
  Metrics that the walk and `json` cannot follow for what a script nested in
  them, however deep or holding itself, MUST therefore be written without
  it, each item of such a list or dict as `null`, so that the entry is read,
  and refused or taken, as it is where the worker writes all of it; every
  other answer MUST be the line it was. The worker MUST outlive it and MUST
  NOT raise the recursion limit for it either, and MUST NOT look through a
  transform's answer level after level for what a script nested in it, which
  for a list holding itself twice is twice as much at each.
- A list or a dict that is in an answer more than once has no JSON form but
  its copy: it MUST be written each time it is met, so that `x = [1, 2];
  data = [x, x]` is read back as two lists. What an answer is written as can
  therefore be many times what its script built — lists that each hold the
  next twice, forty of them, are forty lists and 2^40 numbers written — and
  has no end where a list or a dict holds itself. A worker MUST NOT go
  through such an answer as it is written: whatever it does with an answer
  that holds a list or a dict more than once, or in itself, MUST take time
  and memory in proportion to what the script built, each list and dict gone
  through once however often it is there, or to `limits.max_output_bytes`,
  and to neither the depth of the answer nor the length it is written in.
  `data` that holds a list or a dict in itself MUST fail the run as the
  script's failure, with the message of `data` nested too deep, found where
  a walk that knows the lists and dicts it is inside of comes back to one of
  them, and so at the same cost whether it holds itself once or twice.
  `data` that holds a list or a dict more than once and cannot be written
  within `limits.max_output_bytes` whatever its values are — a byte for each
  value, a string's and a key's length for each — MUST fail the run as the
  script's failure, `python pre-script failed: OverflowError: what the
  script left in data is longer than limits.max_output_bytes (N bytes)
  written out, a list or a dict that is there more than once being written
  each time; leave less there, or raise limits.max_output_bytes`, without
  being written or copied out. A transform's `metrics` that hold such a list
  or dict MUST be written without what no metric has, as those the walk
  cannot follow are, an entry that is there many times made once; and where
  they are longer than the limit even so, the run MUST fail the same way,
  `... left in metrics ...`. The worker MUST outlive each of these. An
  answer that holds no list or dict twice MUST NOT be measured by the
  worker: it MUST be written as it was whatever its length, the walk that
  looks through it only counting its values, and one longer than
  `limits.max_output_bytes` MUST end as it did, with the output limit's
  error and its worker. An answer written before MUST be the line it was.
- A string that an answer holds many times is written each time too: a
  text of 100,000 characters that is the item of a list 5,000 times, the
  value of every key of a dict, or a label of every metric, is 500 MB
  written from 100 kB the script made. A worker MUST NOT make or write such
  an answer for the exporter to refuse. The walk that goes through an
  answer before it is written MUST add up the lengths of its strings, each
  as often as it is met and as long as it is, in characters: a string is
  written at least that long, whatever its characters are escaped as. Where
  the sum is more than `limits.max_output_bytes`, `data` and `metrics` MUST
  fail the run as the script's failure with the `OverflowError` of an
  answer that holds a list too often, to the letter, naming `data` or
  `metrics`, before any of it is written, and the worker MUST outlive it:
  whether the answer is plain, has a value that is no JSON beside the
  strings, or is nested deeper than the worker's own walk follows. The sum
  MUST be of what would be written: where a transform's `metrics` are
  written without what no metric has, what is left out MUST NOT count. It
  is of the strings that are values, of exactly the type `str`, and of the
  keys the next rule counts; numbers and punctuation MUST NOT count, so
  that the sum is never more than the answer is long and no answer the
  exporter would take is refused. An answer whose strings and counted keys
  are within the limit and which is longer than it written out — the sum
  exactly at the limit, or numbers alone — MUST end as it did, written,
  with the output limit's error and its worker; an answer of exactly
  `limits.max_output_bytes` MUST be taken; and an answer within the limit
  MUST be the line it was, byte for byte. An answer that is no script's
  `data` or `metrics` is not measured so: the worker's own word MUST be
  written as it is, and a script's error as the rule on it below says,
  never longer than the limit. Counting the strings MUST add no second walk
  of an answer: the cost is one addition for each string the walk meets.
- A key is written each time its dict is: a text of 100,000 characters
  that is the one key of each of 5,000 rows, or the name of a label of
  5,000 metrics, is 500 MB written from 100 kB the script made, and a
  worker MUST NOT make or write that either. But the walk MUST NOT add up
  the keys of every dict of every answer, which costs an answer half again
  of the look through it: the look at keys MUST cost an answer within the
  limit — metrics as `metric()` makes them, rows of short keys, and either
  where one dict the look passes has keys unlike the rest, many of them or
  one long one — no more than a twentieth of the calls the worker made for
  it before, and one the worker copies before it writes it no more than a
  tenth. The walk MUST look at the keys of a few dicts, chosen by where
  they stand in the answer. In an answer it writes as it is: of each level
  that has four values or more, and another level below it, the fourth
  value and every 61st after it, where that is a dict, a level being the
  values of the lists and dicts of the level above and the answer the
  first. In an answer it copies: of the dicts its copy is made of, in the
  order the copy finished them, a dict after the dicts inside it and the
  answer itself last, the fourth and every 61st after it. The lengths of
  the keys looked at MUST be taken as often as the level has values, or the
  copy dicts, for each one looked at: 4 times in a level of four values, 64
  times in one of 64, about 61 times in a long one, and never 61 times for
  a dict that stands among fewer values. Where the look found keys that are
  strings, and the keys taken so and the sum of the strings are together
  more than `limits.max_output_bytes`, or within 64 characters of it —
  which stand for the keys of the levels of fewer than four values, the
  answer's own among them — the worker MUST add up the keys of all the
  answer's dicts, each as often as it is written, and the answer MUST fail
  as one whose strings are too long does, with the same `OverflowError`,
  exactly when its keys and its strings are together more than the limit.
  That sum MUST be made once for an answer, MUST NOT go through the answer
  again a value at a time, MUST NOT add up again the keys of the dicts
  already looked at, and MUST stop where it is past the limit; an answer
  whose keys, taken so, are not that near the limit MUST NOT cost it. What
  writes an answer nested deeper than the worker's copy follows MUST add
  each key as it writes it. A key counts when it is a string, of the type
  `str` or of a class made of it, as long as it is in characters, whatever
  a class of the script's own says of its length, in all three; a key that
  is a number, a boolean or `None` MUST NOT count. The look MUST run
  nothing of the script's, and MUST NOT fail an answer the worker would
  have written: it MUST take a value for a dict and a key for a string by
  its type, not by what it answers when asked for its class. A mapping the
  copy has always gone through by its `items()` — a `weakref.proxy` of a
  dict, an object that gives the class of what it stands for as its own —
  and a dict with a key that is a number whose class says it is `str`, or
  raises when it is asked, MUST be written wherever they stand in the
  answer. The answer's own keys are among those counted: `ok`, `log`,
  `data` or `metrics`, and those the worker writes for each metric. So an
  answer whose keys and strings are exactly the limit MUST end as it did,
  with the output limit's error and its worker, and one character more MUST
  be the script's failure where the look passes its keys; and an answer in
  which the dicts looked at do not say its keys are long MAY be written and
  refused by the exporter, as before. That is one whose long keys are only
  where the look does not pass — the first three values of a level, a level
  of fewer than four values, the values between one looked at and the 61st
  after it (of rows of exactly 61 values, every value but the fourth of
  each row), the first three dicts of a copy — and one whose long keys are
  in so few of the dicts it does pass that, taken for the values around
  them, they stay within the limit. The same answer MUST end the same way
  each time: the look MUST NOT go by chance or by where a value lies in
  memory, and MUST NOT tell the dicts of a copy apart by their addresses,
  which one made for the copy and let go of gives to the next.
- A worker MUST say, with a line of its own before it runs the script, that
  it has read and parsed the request. `limits.script_timeout` MUST bound the
  run of a script from that line on: not the start of the interpreter, which
  has its own budget of 10 seconds, and not handing the worker the request,
  which MUST be bounded by the probe's deadline, and by 30 seconds where that
  is later or there is none. A script that overruns MUST fail the run with a
  timeout error naming the limit, and its worker MUST be killed. A worker
  pool MAY be told a least time to give every script whatever its limit,
  which the tests set on the pools they run scripts in (§ 34); the exporter
  MUST NOT set one, so that the limit is what the collector configures. The
  same holds for the start's budget: a pool MAY be told another, which the
  tests do, and the exporter MUST NOT, so that its start has the 10 seconds.
- A start that fails MUST say what was true of the interpreter when it
  failed, found out before the interpreter is stopped. One that had exited
  MUST be said to have exited, with the end of what it wrote to stderr, read
  once the process has been waited for. One still running when the budget
  ran out MUST be said to have been running and not ready, and that this is
  no crash but a busy machine or a slow import, with what it had written to
  stderr until then; it MUST NOT be said to have exited. One whose first
  answer is not that it is ready MUST be said to have answered that, the
  answer shown as a value is quoted in any error (§ 18.1), and what
  `--python.path` must name. What a running interpreter had written to
  stderr depends on when it was stopped, so the failure MUST be recognised
  without it (§ 25.1). Of what an interpreter wrote to stderr, at a start or
  when it died in a run, an error MUST show the last 1,500 bytes, from a
  character's start, after `... ` when it wrote more, so that the error is
  within the bound of every failure (§ 6) with where the interpreter said
  why it stopped, and with the hint about `limits.max_script_memory` that
  follows it, whole; stderr within that MUST be in the error as it was.
- A run that the probe's or scrape's deadline ends before
  `limits.script_timeout` does MUST be stopped the same way and MUST NOT be
  reported as a timeout of the script: its error MUST say that the probe's
  time ran out, how long the script had run and the `script_timeout` it had
  not reached, or that the script never ran when the deadline came while the
  request was handed over; it MUST be counted as a run of outcome `deadline`
  and a worker stopped for the reason `deadline` (§ 22.1a).
- A worker of the pool that died while idle MUST NOT fail a scrape. An idle
  worker whose process has ended MUST be found so when it is taken from the
  pool, stopped, counted with the reason `crash`, and replaced by the next
  idle worker or a new one. A worker from the pool that turns out dead before
  it said it has the request, when nothing of the script has run, MUST be
  replaced the same way and the request given to another; a worker just
  started that dies, or one that dies after it took the request, fails the
  run.
- A script error, including `SystemExit`, MUST fail that run with the Python
  error and leave the worker in service. The error MUST be the traceback of
  the script's own frames, the five innermost, so the failing line is always
  among them, each with its line of the script (`File "<collector-python>",
  line N, in f`), then the exception; the worker's own frames, those of the
  exporter's `metric` and `fail` included, MUST NOT appear, and a chained
  exception MUST be trimmed alike. A worker that exits, crashes, or writes
  an answer longer than `limits.max_output_bytes` MUST be discarded, and the next
  scrape MUST start another.
- A script's error of 1,500 bytes or fewer MUST be in the transform's error
  whole, as it was. A longer one — a message as long as the data, a source
  line of kilobytes, a chain of exceptions raised from one another, each
  written out — MUST be shown by what says most, in an error within the
  bound of every failure (§ 6) that is not cut again: each line by its first
  200 bytes and its length; from the exception's own line on, which is the
  first line after the last frame that is not indented, the lines that fit
  in 750 bytes, or in 1,500 where the text has no frame, and then how many
  lines that part had (`... (4001 lines)`) when some were left out; and
  before it the last lines that fit in what is left of the 1,500 bytes,
  after how many lines stood before the exception's own (`... (3613
  lines)`) when some were left out. Which lines are shown MUST be decided
  by their lengths with the mark in place of every length measured, and
  the failure MUST be recognised (§ 25.1) by the text with those marks, so
  that the same failure of a longer message, or of a longer chain that ends
  alike, is shown by the same lines and is one failure. The traceback is
  the text the interpreter's `traceback` module writes, and the lines of
  position markers it draws under a source line (`~~~~^^^^`), which Python
  3.13 draws under more lines than 3.12, MUST count as lines like any other:
  a release that draws more MAY show fewer frames of the same failure.
- A script's error MUST NOT be output over the limit, whatever its length:
  an exception whose text is longer than `limits.max_output_bytes` — a
  message as long as the response, a `KeyError` of a large key, a
  `SyntaxError` on a long line, exceptions raised from one another — MUST
  fail the run with the error the rule above makes of the whole traceback,
  recognised as that is, MUST be counted as a script's failure (the outcome
  `script_error`, § 22.1a) and MUST leave the worker in service. A worker
  MUST NOT write an error's line longer than the limit. An error of 16,384
  characters or fewer whose line is within the limit MUST be written whole,
  the line it was. Any other the worker MUST cut itself, by the rule above
  to the byte — the text trimmed of the space around it as the exporter
  trims it, its lines told by line feeds alone, lengths in bytes of UTF-8
  with half a surrogate pair as U+FFFD and the two halves of one as their
  character, as the exporter reads them — and MUST write as `shown` in place
  of `error`: the text in parts, each a string or a number that was
  measured (a line's length, how many lines a part had), which the exporter
  MUST write in its place and MUST leave out of what the failure is
  recognised by. The exporter MUST NOT make of `shown` more than it shows
  of an error: parts that come to more than 1,689 bytes — the longest the
  rule above shows, 1,500 bytes of lines, how many lines were left out
  before them and after, and nine numbers of nineteen digits — are no
  worker's, and MUST be shown as the text of an error of that length is; a
  part that is neither a string nor a number MUST be passed over.
  What is shown of an error is within 16 KiB however it is
  written, so an error that reached the exporter whole under a limit MUST
  still be reported as it was under that limit. Where the limit holds
  neither the whole error nor what is shown of it, the error MUST be what
  fits: what the rule above shows in half its 1,500 bytes, and half again
  down to none, which leaves out frames and lines of the message and keeps
  the exception's own line by its first 200 bytes; and where that line
  does not fit either, as many of its first characters as do. The
  `MemoryError` that names `limits.max_script_memory` MUST be written the
  same way. To cut an
  error a worker MUST NOT join, copy or encode it whole, one with halves of
  surrogate pairs in it no more than another: it MUST look at the
  pieces the traceback module hands out one at a time, those shorter than
  64 KiB joined until they come to that much, less than 128 KiB together,
  so that a message is held no more often
  than that module makes it. A worker MUST answer that the script failed
  whatever making the line of its error raises — a `MemoryError` where
  `limits.max_script_memory` does not hold the copy the traceback module
  makes of a message, an exception of a class that cannot be asked for its
  name: the error MUST then be `T: (the text of this error could not be
  written: F)`, T the type of the script's exception as a traceback names
  it, by its first 80 characters, or `an exception` where the type gives no
  name, and F the type of what was raised; under a limit too small for
  that, as many of its first characters as fit. Nothing of the answer MUST
  have been written before, what the attempt held MUST be let go of before
  that answer is made, the run MUST be counted as a script's failure and
  the worker MUST stay in service. A worker MUST NOT make ready at its
  start what cuts an error: it MUST do so when an error first needs to be
  cut, and an attempt that fails MUST leave it to be made for the next. An
  answer that is no error MUST end as it did:
  `metrics` or `data` longer than the limit with the output limit's error
  and their worker, or with the worker's own refusal.
- `limits.max_output_bytes`, a size (§ 5.0a), bounds the one line a worker
  answers a request with, without its line break; left out or `0` it MUST be
  1 MiB. Set, it MUST be at least 38 bytes, and a configuration that sets
  less MUST be refused when it loads — at startup, by `--dry-run` and on a
  reload, in a collector file as in the configuration — whatever the
  collector is, with a script, with a pre-script or with neither, since the
  key bounds nothing else: `collector "c" limits.max_output_bytes is 26, and
  a Python script that emits no metric answers in 38 bytes, so no
  transform's script could answer within it; set at least 38, or leave it
  out, or 0, for the default, 1MiB`, naming the collector, the key, the
  bytes the value comes to however it is written, the least and the
  default. 38 bytes is the least limit under
  which a worker starts and a script of either kind that has nothing to say
  answers, taken of a script that prints nothing, what a script prints being
  in its answer: the line a worker is ready with is 27 bytes, so under 27
  none starts and every scrape fails as a start that failed; the line it
  takes a request with is 17; a pre-script's answer is 34 bytes at its
  shortest, with a number of one digit left in `data`, 35 with `{}`, `[]` or
  `""` and 37 with `None`; and a transform's script that emits no metric
  answers in 38, `{"ok": true, "log": "", "metrics": []}`. The least is
  one, of the key: a limit of 34 to 37 bytes, under which a pre-script that
  leaves no more than that in `data` could have answered and no transform's
  script could, MUST be refused with the rest. A smaller limit once loaded,
  passed `--dry-run` and failed every scrape of a collector whose transform
  is a script, as output over the limit or as a start that failed. The
  least MUST be a constant beside the worker, worked out from those lines
  as the launcher writes them, and a test MUST hold it to the launcher: a
  real worker MUST answer with exactly those lines and MUST start and
  answer both kinds of script under the least, and a transform's script
  MUST fail as output over the limit under a byte less; and a stand-in that
  writes the same
  lines MUST be read under the least and refused under a byte less, the
  exporter measuring a line as the constant counts it. The loader holds the
  least and the worker and the pool MUST NOT: a worker MUST run under
  whatever limit it is given, cutting a script's error to it (above). The
  schemas MUST refuse a number from 1 to 37 at the key and take 0, and a
  size written as text that comes to that many bytes (§ 24.3).
- `limits.max_script_memory`, a size, MUST bound each of the collector's
  workers' address space (`RLIMIT_AS`), set after the declared libraries are
  imported. A script that needs more MUST fail the run with a `MemoryError`
  naming the limit and leave the worker in service. It MUST be at least 32MiB
  or `0`, the default, for none; a smaller one MUST be refused at load. A
  different limit MUST mean a different pool of workers. What counts against
  the limit is the interpreter, the preloaded libraries and what the script
  allocates, and nothing the worker reserves without using: a worker MUST be
  started with `MALLOC_ARENA_MAX=1` added to the exporter's environment, so
  that glibc does not reserve a second arena of 64 MiB for the thread that
  watches the worker's parent, unless the exporter's environment sets that
  variable, whose value MUST then be passed on unchanged. The variable MUST
  be set on every system, since those it means nothing to ignore it.
- `--python.max-workers` MUST bound the workers alive at once, starting, busy
  or idle, of every collector together. A run that finds no idle worker for its
  script while the limit is reached MUST stop the idle worker unused for
  longest, of any script, counting it with the reason `evicted`, and start its
  own; when every worker is busy it MUST wait in line for one within its own
  deadline and otherwise fail naming the flag, each worker that frees letting
  the first waiter through, and a waiter giving up passing its turn on. A
  worker that dies mid-run under `limits.max_script_memory` MUST say so in
  its error, since a C library out of memory can end the interpreter rather
  than raise `MemoryError`. `0`, the default, MUST leave the workers
  bounded only per pool; a negative value MUST be a command-line error.
- A healthy worker MUST be reused at most 1000 times; at most
  `--python.max-idle-workers` idle workers, 1 by default, MUST be kept per
  pool, a worker whose run ends with that many idle already being stopped with
  the reason `surplus`, and an idle worker unused for longer than
  `--python.idle-timeout`, 2 minutes by default, MUST be stopped with the
  reason `idle`. `--python.max-idle-workers=0` MUST keep no worker idle, each
  stopping when its run ends, and MUST NOT limit how many runs of a script run
  at once; a worker so stopped MUST free its place under
  `--python.max-workers` for a run waiting for one. A negative
  `--python.max-idle-workers` and an `--python.idle-timeout` under 1 second
  MUST be command-line errors (exit 2); neither has an upper bound. The
  default timeout is twice a scrape interval of a minute, so a collector
  scraped every minute or more often keeps its worker warm through a late or
  a missed scrape. Idle workers MUST be checked against the timeout on a
  timer, every minute or every half the timeout when that is shorter, and not
  only when a worker is next asked for, so a collector that is no longer
  scraped does not keep its interpreters.
- A reload that removes or changes a collector's script MUST stop that script's
  idle workers at once and each busy one when its run finishes, counting them
  with the stop reason `reload`; workers of scripts the reload keeps MUST be
  left alone. The workers of a collector a reload removed MUST be stopped so
  though a later reload has brought the collector back with the script it
  had by the time their runs finish (§ 22.1a).
- The time each script runs in its worker, as `limits.script_timeout`
  measures it — not starting a worker, nor handing it the request — MUST be
  kept per probe, the pre-script's and the python transform's together, and
  published as `http_exporter_script_duration_seconds` of the collector and of
  the request (§ 22).
- A worker MUST exit when the exporter does. Closing its request pipe ends an
  idle one. A worker busy in a script does not read the pipe, and an exporter
  that was killed stops no worker, so every worker MUST also watch for the
  process that started it — a thread of its own that checks about once a
  second whether its parent process is still the one it started with — and
  exit when it is not. The watch MUST be started, and running, before the
  memory limit and the sandbox are installed, MUST NOT depend on anything a
  script can replace, and MUST NOT make a module the sandbox blocks reachable
  by a script. It MUST NOT end while the worker lives: an error raised in
  it, such as the `MemoryError` of a look taken while a script holds all of
  `limits.max_script_memory`, MUST be passed over and the look taken again a
  second later. The exporter MUST NOT instead ask the kernel to signal the worker
  at its parent's death (`PR_SET_PDEATHSIG`), which follows the thread that
  started the worker, not the exporter.

---

# 17. jq / yq execution

jq and yq should be treated as first-class transformation engines.

The implementation may use embedded libraries or subprocesses depending on technical feasibility.

Do not require users to separately install jq/yq in the container unless explicitly documented.

Preferred behavior:

```yaml
decoder:
  type: json

transform:
  type: jq
metrics:
  - name: application_requests_total
    description: Total application requests
    type: counter
    error_mode: log
    expression: '.requests'
```

and:

```yaml
decoder:
  type: yaml

transform:
  type: yq
metrics:
  - name: application_requests_total
    description: Total application requests
    type: counter
    error_mode: log
    labels: []
    expression: '.status.requests'
```

Execution MUST be bounded and failures MUST be reported as transform errors.

Expressions MUST be compiled once and reused by every scrape, not parsed and
compiled per evaluation: configuration validation compiles every jq, regular
expression, CSS selector and XPath expression a configuration holds (§ 24.2),
and scrapes run those programs. The compiled programs MUST be safe for
concurrent use, cached by expression text (and namespace bindings for XPath),
and bounded in number, so repeated reloads cannot grow the cache without limit.
The bound MUST keep the expressions in use compiled rather than empty the
cache when it is reached: each cache MUST keep two generations of at most
4096 expressions, or of as many as the configuration in force holds when that
is more, an expression found in the older moving to the recent one, so an
expression evaluated at least once per generation is never compiled again.

A jq or yq expression that is `.` or a field path alone — `.a`, `.a.b`,
`."a key".b`, as gojq parses it, without brackets, `?`, iteration or
interpolation — MAY be evaluated by looking the fields up directly, but MUST
give exactly what gojq gives: a field of an object its value, or `null`, and a
field of `null` `null`. Where a step is anything else, the program MUST run, so
the error is gojq's.

Every jq program MUST have the value-parsing helpers `parse_bytes`,
`parse_duration`, `parse_time`, `parse_percent` and `parse_quantity`, each a
function of no
arguments reading its input, added as gojq custom functions (none MUST be
the name of a gojq builtin, which it would hide or be hidden by), and the
Python transform MUST have the same helpers (§ 16.2). The two MUST give the
same results, which MUST be tested against one file of vectors,
`testdata/values/helpers.json`, by the tests of both. Each helper MUST drop
spaces, tabs, carriage returns and line feeds around its text and nothing
else, and read a number as ASCII digits with an optional fraction after a
dot and at least one digit, no exponent, no separators, and at most 100
digits; it MUST read the number exactly and round the value it gives to the
nearest float once, so `1.1GB` is 1100000000 and `0.1s0.2s` 0.3.

- `parse_bytes` MUST read a number, blanks or none, and a unit or none, the
  unit's ASCII letters in any case: none and `B` bytes; `KiB` to `EiB`
  powers of 1024; `kB` to `EB` powers of 1000; and `K`, `M`, `G`, `T`, `P`
  and `E` alone powers of 1024. It MUST refuse a sign, any other unit and
  anything after the unit.
- `parse_duration` MUST give seconds of Go's duration syntax with `d` for 24
  hours (units `ns`, `us`, `µs`, `μs`, `ms`, `s`, `m`, `h`, `d`, case as
  written, no blanks, `0` alone), which alone MAY have a leading `+` or `-`;
  of a clock, `MM:SS`, `HH:MM:SS`, `D-HH:MM:SS` and `D.HH:MM:SS` with an
  optional fraction after the seconds, the first field of any length and
  each after it two digits, hours after days under 24 and minutes and
  seconds under 60; and of an ISO 8601 duration `PnWnDTnHnMnS` in upper
  case, each part optional but one there, a fraction on the last part only.
  It MUST refuse ISO 8601 years and months, saying they have no fixed length
  in seconds.
- `parse_time` MUST give Unix seconds, with the text's fraction of a second,
  of RFC 3339 (`T`, `t` or a blank between date and time, `Z`, `z`, `+HH:MM`,
  `+HHMM` or `+HH`) and of the RFC 1123 HTTP date with `GMT`, `UTC` or
  `+HHMM`, its names in any case and its weekday checked against the date.
  Every year from `0000` to `9999` MUST be read in the proleptic Gregorian
  calendar, a moment before 1970 as negative seconds and its weekday as
  that of its own date, alike in jq and in Python.
  It MUST refuse a date and time without a zone, saying it names no single
  moment, a second of 60 and a date that does not exist, and MUST NOT read
  any other layout: `time_format` and jq's `strptime` read those.
- `parse_percent` MUST read an optional sign, a number, blanks or none, and
  a `%` that MUST be there, and give the ratio, a hundredth of it, which is
  Prometheus's base unit.
- `parse_quantity` MUST read a Kubernetes quantity: an optional sign, a
  number, and, with no blank before it, a suffix in its case or none: `Ki`,
  `Mi`, `Gi`, `Ti`, `Pi` and `Ei` powers of 1024; `n`, `u`, `m`, `k`, `M`,
  `G`, `T`, `P` and `E` powers of 1000 from 10^-9 to 10^18; or `e` or `E`, a
  sign or none and one or two digits, a power of ten. It MUST give the
  quantity in its base unit, cores or bytes (`100m` 0.1, `2Gi` 2147483648),
  and MUST refuse any other suffix, `K` among them, and anything after it.

Each helper MUST read text; `parse_bytes`, `parse_duration` and
`parse_quantity` MUST give a number back unchanged, as being bytes,
seconds or a quantity already, and anything else
(`null`, a boolean, an object, an array, a number for `parse_time` and
`parse_percent`) MUST fail saying what the helper reads and what it was
given by its kind. Text a helper cannot read MUST fail with
`<helper> cannot read <text> as <what>: <what to write>`, the text quoted
and cut as a value of the response is (§ 18.1), as a jq error that fails the
rule under its `error_mode` as any jq error does and that `try` catches.

Every jq program MUST have `$root` bound to the whole decoded document, so an
expression evaluated against one item (§ 18.2) can reach the rest of it,
`$status` bound to the response's status, and `$headers` to its headers,
an object of lower-case names, each with its values joined by `, `. The
status MUST be the HTTP status for `http` and `graphite`, the call's status
code for `grpc`, `0` for `OK`, and `null` for `localfile`, which has none;
Python's `response.status_code` MUST be the same, `None` for `null`.

Every jq program MUST also have `$timings` bound, whatever the collector's
request type, so that a rule reading it compiles in every collector. For an
`http` or `graphite` collector it MUST be an object of `dns`, `connect`,
`tls`, `first_byte` and `total`, seconds as numbers, measured with
`net/http/httptrace` on the request whose status `$status` is: the last
attempt of a retried request, the request a followed redirect led to, the
first page of a paginated collector or the page whose status failed it, and
the conditional request of a revalidation answered `304`. `first_byte` MUST
run from the request's start, when it asks for a connection, to the
answer's first byte, and `total` from that start to the end of reading the
body. A phase the request did not have MUST be `null`, not `0`: no lookup,
connection or handshake on a reused connection, which `new_connection`
(§ 42.15b) leaves no request with, no handshake over plain
HTTP, no lookup to an IP address or a Unix socket, and no handshake the
exporter's own dialer makes to an `https://` proxy. `$timings` MUST be
`null` for every other request type, for a response pasted on the debug
page, and in a pagination's `next` and a login's `token` expression.
Python's `response.timings` MUST be the same, a dict with `None` for
`null`, and `None` whole where `$timings` is `null`. The phases MAY be left
unmeasured for a collector none of whose texts names `$timings` and that
runs no Python script, since no rule of it can read them, and names no
`$remote_ip` either, which the same trace sees; a debug run of it
MUST still measure them, since its report shows them. The values of
`$timings`, `$tls`, `$url`, `$redirects`, `$http_version` and `$remote_ip`
MAY likewise be made only for a program whose text names one of them.

Every jq program MUST also have `$tls` bound. For the request `$timings`
describes, sent over TLS, it MUST be an object of the connection the answer
came on, named and written as the `tlscert` document is: `tls_version`, the
TLS version (`"TLS 1.3"`); `cipher_suite`, the cipher suite's IANA name; `server_name`, the name sent as SNI, `""` for none;
`verified`, whether the exporter verified the chain, `false` under
`insecure_skip_verify`; `not_before` and `not_after`, the leaf's validity in
Unix seconds; and `subject_cn` and `issuer_cn`, the common names of the
leaf's subject and issuer. Nothing else of the connection, and nothing
secret, MUST be in it. It MUST be `null` over plain HTTP, a plain HTTP
request answered over a TLS connection to an `https://` proxy among them,
and wherever `$timings` is `null`. Python's `response.tls` MUST be the same.

Every jq program MUST also have `$url` and `$redirects` bound. For the
request `$timings` describes, `$url` MUST be its URL, where the followed
redirects led, rendered by the redaction a target shown in a log line has:
the value of each query parameter, and of each path parameter, whose name
reads as a credential's by the one rule for a name (§ 42.17) masked as
`<redacted>` where it stands, every other parameter and the path's segments
as they were sent, and the fragment left out; and its userinfo
MUST be left out, not shown as `redacted:redacted`. `$redirects` MUST be
the number of redirects followed to it, `0` for none. Both MUST be `null`
wherever `$timings` is. Python's `response.url` and `response.redirects`
MUST be the same.

Every jq program MUST also have `$http_version` and `$remote_ip` bound. For
the request `$timings` describes, `$http_version` MUST be the version of
HTTP its answer came in, `"1.0"`, `"1.1"` or `"2"`, as blackbox_exporter's
`probe_http_version` writes it, whether or not the request was traced.
`$remote_ip` MUST be the IP address the request's connection was made to,
kept or new, as Go writes it, an IPv4 address mapped into IPv6 written as
IPv4; it MUST be `null` for a request through a proxy, whose connection is
to the proxy and whose address is not the target's, for one over a Unix
socket, and for one that was not traced. A collector whose texts name
`$remote_ip` MUST have its requests traced as one that names `$timings`
does. Both MUST be `null` wherever `$timings` is. Python's
`response.http_version` and `response.remote_ip` MUST be the same. A debug
probe's report and the debug page MUST show `$timings`, `$tls`, `$url`,
`$redirects`, `$http_version` and `$remote_ip`, in that order, as JSON not
escaped for HTML, under the response's status, `$url` with its query values
masked and its length cut as the report's request lines have a URL, and
otherwise as the rules read it: the URL MUST NOT be parsed and written
again, so its path's escapes are the ones `$url` has.

---

# 18. Transformation model

The preferred internal model is:

```text
HTTP response
    |
    v
Decoder
    |
    v
Decoded data
    |
    v
Transformer
    |
    v
MetricSet
```

This allows combinations such as:

```text
JSON + jq
JSON + Python
YAML + yq
YAML + Python
XML + XPath
XML + Python
CSV + Python
HTML + CSS
HTML + XPath
HTML + Python
Text + regex
Text + Python
Prometheus + native filter/transform
Prometheus + Python
```

Python does not need to be the only way to handle difficult cases.

---

## 18.1 Canonical metric declarations

All non-Python transformations MUST use one common collector-level `metrics`
array. A metric declaration MUST support these fields:

```yaml
metrics:
  - name: application_requests_total
    description: Total application requests
    type: counter
    error_mode: log
    labels:
      - name: environment
        expression: .environment
    expression: .requests
```

`name`, `description`, `type`, `labels`, `expression`, and `error_mode` are the
standard shape. `description` becomes the Prometheus HELP text. `type` MUST be one of
`gauge`, `counter`, `histogram`, `summary`, or `untyped`; omitted types default
to `gauge`, except on a `prometheus` rule, which MUST keep the type of each
series it passes through when it sets none. `histogram` and `summary` MUST be
refused at load on a rule of any other transform, since a rule reading one
value has no buckets or quantiles to expose, except `histogram` on a rule
with a `histogram` block (below), and the refusal of `histogram` on a rule of
a transform that takes the block MUST name the block as the way to count the
values in a histogram. Metric declarations MUST be placed on the collector, alongside
`transform`, rather than using transform-specific arrays such as `rules` or
`expressions`.

A rule of `type: histogram` MAY set a `histogram` block with `buckets`, the
upper bounds of its buckets, to count the values it reads in a histogram
instead of making each a series; nothing MUST change for a rule without the
block. The block MUST be taken by the `jq`, `yq`, `xpath`, `css`, `csv` and
`regex` transforms and refused at load on a `prometheus` rule and a `python`
rule, and on a rule of any other type or of none. `buckets` MUST be refused
at load when it is empty, longer than 100, holds a bound that is not finite
(`+Inf`, which every histogram has as its last bucket, refused saying so), or
holds a bound not greater than the one before it; a rule with the block MUST
NOT have a label named `le`. The values counted MUST be those the rule would
otherwise make series of, read as such a rule reads them — every value of a
`jq` or `yq` expression or one per item, each node an XPath expression
selects or the one value it computes, each match of a regex, a column's cell
in each row — and for a `css` rule without `items` the text of every element
its selector matches, which a rule of another type MUST refuse more than one
of; `value_map`, `time_format` and `scale` MUST apply to each value before it
is counted. A value MUST be counted in every bucket whose bound it is at most
and in `+Inf`, its scaled value added to the sum and 1 to the count; `-Inf`
MUST be counted in every bucket and `+Inf` in `+Inf` alone. NaN, a value that
is no number and a missing value MUST be handled by the rule's `error_mode`
and `required` as any rule's value is, the histogram counting the others
under `log` and `ignore`. The values of one set of label values, a label with
an empty value being none, MUST make one histogram, and two different sets
two whatever bytes the values hold, a value that is not UTF-8 among them;
the histograms are in the order of their first values, and each MUST take
one series of `limits.max_metrics` when its first value comes, and the
values none. A rule
that reads no value at all — an expression that gives none, items, nodes or
elements selected none, no match, no rows — MUST make one histogram of count
0 with every bucket and the sum 0 when its labels are all static, and none
otherwise, and MUST NOT count it as a missing value. The histogram MUST be
made anew from each response, counting nothing of the scrape before, and
MUST be exported as a histogram in the text format, OpenMetrics and OTLP, as
one read from a target is.

A rule MAY set `value_map`, a mapping of text to numbers, and `scale`, a
number. The value an expression gives — a regex capture, a CSS element's or
XPath node's text or computed value, a CSV cell, a jq or yq result — MUST be
looked up in `value_map` as text: without surrounding blanks, a number as JSON
writes it and a boolean as `true` or `false`, case-sensitively. A value it
lists MUST take the mapped number; any other MUST take the number mapped to
`"*"` when there is one, numbers included, and otherwise be read as a number
as without the map, failing the rule, naming `value_map`, when it is not one.
`scale` MUST then multiply the value, mapped or read; a `scale` that is
exactly one over a whole number of magnitude 2 or more, as `0.001` is
written — the scale being the float64 nearest to that reciprocal — MUST
divide by that number instead, so the result is the nearest number to the
exact quotient. A scale that only comes close to such a reciprocal, as
`0.3333333333` and `1.5e-9` do, MUST multiply. A label MAY set
`value_map` too, text to text, applied to the value its expression gives, as
text without surrounding blanks, with `"*"` for any other value; without a
match and without `"*"` the value MUST be kept, and a value mapped to `""`
MUST leave the label off. It MUST be applied to the series of its rule, by the
rule's name, before labels are truncated or renamed, and refused at load on a
static label, a `python` rule, a rule without a name, and, mapping to `""`, a
`required` label. Both MUST apply the same
way in the `regex`, `css`, `xpath`, `csv`, `jq` and `yq` transforms; on a
`prometheus` rule `scale` MUST multiply plain samples and fail the rule for a
histogram or summary, and `value_map` MUST be refused at load; a `python`
rule MUST refuse both at load. A `scale` of 0 or one that is not finite, and a
`value_map` key that is empty or has surrounding blanks, MUST be refused at
load.

A rule MAY set `time_format`, and with it `time_zone`. The text its
expression gives, without surrounding blanks, MUST then be read as a time, and
the value MUST be that time in Unix seconds, as a float64 with its fraction of
a second, which `scale` MUST then multiply as it does any value. `time_format`
MUST be `rfc3339` (RFC 3339, with or without fractional seconds), `rfc1123`
(the HTTP date, `Mon, 02 Jan 2006 15:04:05 GMT`, and the same with a numeric
zone), either name in any case, or a layout: the reference time
`Mon Jan 2 15:04:05 MST 2006` written as the text writes its times, the
convention of Go's `time` package, Promtail and Telegraf, such as
`2006-01-02 15:04:05`. Seconds a layout writes `05`, or with nines after them
(`05.999`), MUST read a fraction of any length after the seconds, and none;
zeros after them (`05.000`) stand for exactly that many digits. Text in a
layout that is an element of the reference time is that element wherever it
stands, as the convention has it — the `4` of `Q4` is a minute — and a year
of two digits is one of 1969 to 2068. There MUST NOT be names for Unix
seconds or milliseconds: a number is read without `time_format`, with
`scale`. A text that gives its zone or offset MUST be read by it when the
layout has a zone element there (`Z07:00`, `-0700`, `-07:00`, `MST`); any
other, one whose zone the layout writes as plain text (`Z`, `UTC`) included,
MUST be read in `time_zone`, an IANA zone name, by the offset the zone has at
that time, and in UTC when `time_zone` is unset or `UTC`. A local time that
the zone's change of clocks gives twice or not at all is read by one of the
two offsets, as Go's `time.Date` reads it, and this specification does not
say which. The binary MUST carry the IANA zones (`time/tzdata`), so a zone is
known on a host without zone files. A zone the text names by an abbreviation
MUST be one of `time_zone`'s own, `UTC` or `GMT`, or `GMT` with a signed
whole number of hours (`GMT+3`, `GMT-5`), which MUST be read as that many
hours ahead of UTC or behind it whatever `time_zone` is; any other MUST fail
the rule rather than be read as UTC. It MUST work
the same way in the `regex`, `css`, `xpath`, `csv`, `jq` and `yq` transforms.
A value that is not text — a jq or yq number, boolean, object or array, an
XPath number — MUST fail the rule with an error saying `time_format` reads
text; text that is no time in the format MUST fail the rule, handled by its
`error_mode`, with an error that quotes the text, cut as a value that is no
number is, and the format, in the exporter's words and not the time
package's; an empty or absent text MUST stay a missing value. At load a
layout MUST be refused when it holds no element of the reference time, as
`yyyy-mm-dd` and `%Y-%m-%d` do, with an error showing the reference time and
an example; when it lacks a year, a month or a day, since a time without a
date has no Unix time, naming what it lacks; when it cannot read back a
time it writes; when it begins or ends with a blank, which the text it reads
never does; and when it has the hour of a 12-hour clock (`03`, `3`) without
`PM`, which would read no afternoon, with an error saying to write `15` or to
add `PM`. The error for a layout that lacks a date or cannot read what it
writes MUST end, as the first does, with what a layout is, since a sample
date or the name of another format is refused by these checks. `time_zone`
without `time_format`, a `time_zone` that names no
zone (`Local` included, which differs from host to host), `time_format`
beside `value_map`, and `time_format` on a `python` or a `prometheus` rule
MUST be refused at load, the last two in the words `value_map` is refused
there. A rule without `time_format` MUST read its value exactly as before,
at the cost of one comparison. `time_format` or `time_zone` written as the
empty string MUST be the key left out, as an optional key of free text is
elsewhere in the configuration, to the exporter and to the published schemas
(§ 24.3) alike: `time_zone: ""` MUST be accepted without `time_format` and
be UTC beside one, `time_format: ""` MUST be accepted beside `value_map` and
on a `python` or `prometheus` rule, and `time_format: ""` beside a
`time_zone` that names a zone MUST be refused as `time_zone` without
`time_format`.

A rule MUST NOT take a condition of its own (a `when` key): which values
become series is said in each transform's language — jq and yq `select` and
`empty`, XPath predicates, CSS selectors in `items`, the regex pattern, and a
pre-script for CSV rows — and a label with no value, or one `value_map` maps to
`""`, is left off. CONFIGURATION.md MUST show how for each transform, and say
that a conditional series goes stale when its condition stops holding, so a
series whose absence means something is better exported as `0` and `1`.

`error_mode` MUST be `ignore`, `log` or `fail` and defaults to `log`; the same
vocabulary as `error_handling` (§ 19). Any other value MUST be rejected at
startup and on reload, and the message MUST list the accepted values. It governs what happens when an individual metric cannot be
extracted — its expression or a label expression errors, its value is absent
while the metric is required, a required label is absent (§ 18.1), or its
value is not a number.

Text an expression gives — a regex capture, the text of a CSS element or an
XPath node, a CSV cell, a jq or yq string — MUST be read as a number when it
is written as a decimal number: digits with or without a point, digits on
either side of the point or on one, an exponent after `e` or `E`, and a
leading `+` or `-`; or as `NaN`, or `Inf` or `Infinity` with or without a
sign, in any case; with whitespace around it or without. An integer of more
digits than a float holds exactly MUST be read as the nearest float, and a
decimal number nearer to zero than a float holds (`1e-400`) as 0; one
beyond the largest float (`1e400`) MUST NOT be read as infinity and fails
its rule, as below. Text the exporter itself reads as a value MUST NOT be
read as a number when it has an underscore (`1_000`) or is in hexadecimal
form (`0x1p-2`, `0x1F`), though the language the exporter is written in
reads the first as 1000 and the second as 0.25: it is text that is not a
number, as it is to the Prometheus decoder (§ 14) and in a carbon line
(§ 15a), and so is every other
text — a thousands separator, a decimal comma, a unit, a word. The numbers
of the configuration (`scale`, the values of `value_map`, limits) are YAML's
and are not affected, and a value a Python script gives `metric(...)` is the
number the script made. So is a number an XPath function or operator
computes (`number(//v)`, `sum(//v)`, `//v * 1`), which the XPath engine
converts, reading `1_000` as 1000 and `0x1p-2` as 0.25, and an unquoted
scalar of a YAML response that YAML's syntax makes a number (`1_000`, `0x10`,
`017`), which the YAML decoder reads, as 1000, 16 and 15: neither is text an
expression gives, and CONFIGURATION.md MUST say so of both.

An error saying a value is not a number MUST name the value as a person reads
it, never in Go's syntax or with Go's parser's wording: text quoted, cut to its
first 64 bytes with its full length when it is longer (`value "n/a" is not a
number; map text to numbers with value_map`), a number too large for a float
as beyond its range, and an object or an array by its kind and size, not its
content (`value is an object with 2 keys, not a number`, `an array of 3
items`, `null`). A boolean MUST be read as `1` or `0`. What it does:

- `ignore` MUST skip that metric without logging and carry on. The probe MUST
  serve every metric that could be extracted; when none could, it MUST succeed
  with an empty body rather than fail.
- `log` MUST record the metric-specific error, naming the collector and the
  rule, and otherwise behave exactly as `ignore`. A rule that fails for several
  series in one scrape — rows of a table, items — MUST be recorded once for that
  scrape, with its first error and the number of series that failed, not once
  per series. Rules MUST be told apart by their metric name, their expression
  and their `items`, and each MUST be recorded by itself: several rules of
  one metric name MUST NOT be recorded as one, with the error of the first;
  rules alike in all three, whose series their labels tell apart (alike in
  those too they are the same rule, refused at load, as below), are one
  rule, which MUST be recorded as `log`
  records it when either of them has `log`, whichever comes first. Two rules
  that differ in any of the three MUST NOT be taken for one whatever their
  expression and `items` hold, a NUL in a comment or a string of a jq or css
  expression included. Every series a rule carries on
  without, under `ignore` or `log`,
  MUST be counted in `http_exporter_rule_failures_total{collector, metric}`, a
  series present from zero for every rule that has a name, which the rules of
  one metric name share and which counts the failures of all of them, and in
  `http_exporter_missing_keys_total` when the value was missing. A
  `prometheus` rule without a name is the exception: it has no
  `http_exporter_rule_failures_total` series, none being exported with an
  empty `metric`, and what it did not find is counted in
  `http_exporter_missing_keys_total` only.
- `fail` MUST record the error as `log` does and MUST then fail the whole scrape
  at that metric. No metric from that scrape MUST be served, including metrics
  that were extracted successfully, so a response is either complete or an
  error and never silently partial.

Under `ignore` and `log`, a metric-level error MUST NOT fail unrelated metrics in
the same collector. Modes apply per rule: a rule with `fail` that succeeds MUST
NOT fail the scrape because another rule, under `ignore` or `log`, did not.

The error of a `css` and of an `xpath` rule MUST name the metric first, as
`metric "<name>"`, and, where the failure belongs to one item of a css
rule's `items` or to one of the nodes an xpath rule selected, MUST name it
by its place among them, counted from 0, as `item 3` and `node 3`: a value
that is missing (`metric "m" value is missing for node 3: XPath "//v"
selected a node without a value`), a value that is not a number or not a
time (`metric "m" node 3: value "n/a" is not a number; map text to numbers
with value_map`) and a required label that is missing (`metric "m" label
"host" is missing for node 3`). An expression or a selector that matched
nothing, one that does not compile when a response arrives, and an XPath
expression that computed NaN or an empty string MUST name the metric and
the expression (`metric "m" XPath "//v" matched no nodes`, `metric "m" CSS
selector "td.v" matched no nodes`). The error MUST read the same under
`fail`, `log` and `ignore`.

A metric that is not required — `required: false`, or a collector with
`allow_missing_keys` — is not failing when its value is absent, and no mode
applies to it; it MUST be omitted without logging, under `fail` as under the
others.

A value MUST be treated as absent in the same way by every transform when
nothing matched, when it is null, and when it is text that is empty or only
whitespace: an empty CSV cell, an empty JSON or YAML string, an XML or HTML
node without text, a regex whose value group (the capture group named
`value`, or else the first) took no part in the match or captured only
whitespace. Nothing matched MUST include a `prometheus` rule
whose pattern, or whose name when it has no pattern, matches no metric of the
response, and a `csv` rule over a response without a row: each MUST be the
missing value of its rule, once for the scrape, failing it under `fail`,
logged and counted under `log`, counted under `ignore`, and omitted without
logging when the rule is not required. Text that is present and is not a
number MUST NOT be treated as absent: it is a failure of the rule, whatever
`required` says.

A context that is done — the probe's budget ran out (§ 3.2a), or the trip was
cancelled — MUST NOT be handled as a rule's failure. Once it is done no rule
MUST carry on under `ignore` or `log`, nothing MUST be logged or counted
against a rule, and the transform MUST fail as a whole with the context's
error, naming the rule it was at: the `transform` stage's failure, under
`fail` too, and never the `metric` stage's, so that the series the other
rules made are neither answered nor cached as if they were the whole
response. Failures of rules that failed before it MUST still be counted. The
transforms that walk a response themselves MUST ask after the context before
each node an XPath selected, each item of a css rule, each CSV row and each
regex match; a jq program stops by itself.

A probe failed by `fail` MUST respond `502 Bad Gateway` with
`Content-Type: application/json` and a body of the form:

```json
{"status":"error","stage":"metric","collector":"<collector>","metric":"<rule>","target":"<target>","error":"<message>"}
```

`status` MUST always be `error`, so a client can test a single field. `target`
MUST have any user information in the URL redacted, since the body can reach
logs and tickets the credential should not. The failure MUST be counted as a
failed probe in the self-metrics — no increment of `http_exporter_scrape_success_total`,
an increment of `http_exporter_transform_errors_total`, and of
`http_exporter_missing_keys_total` when the value was missing — and MUST NOT be
cached. Besides the per-rule line, the exporter MUST log a `probe failed` line
with `stage` `metric`, the collector, the rule and the redacted target.

`fail` MUST take precedence over the collector's `on_transform_error`. That
policy governs failures of the transform as a whole, while `fail` is a
statement about one metric, more specific than the collector-wide setting; a
lenient `on_transform_error` MUST NOT turn it back into a partial success.

A static target (§ 42.14) has no HTTP response to carry the error. Under
`fail` its scrape MUST produce nothing but `http_exporter_target_up` at 0,
exactly as for any other failed static target scrape; under `ignore` and `log`
it MUST produce what could be extracted.

Each label entry MUST have `name` and exactly one of `value` and `expression`;
setting both or neither MUST be rejected at startup. A `value` label is static:
its value MUST be exported as written and never evaluated, a value of nothing
but blanks included, which is a constant of blanks; the one thing filled in
it is a `{{param_<name>}}` placeholder, from the probe (§ 42.10c). An `expression` that is
written and is nothing but blanks — text that `strings.TrimSpace` leaves
nothing of — MUST be rejected at load, under every transform and whether
`value` is set beside it, written `""` or left out, with an error that names
the collector, the metric and the label, quotes the expression, and says to
write the expression that reads the label or, for a constant, to leave
`expression` out and set `value`: blanks are not the key left out, which only
`""` is (§ 24.3), so such a label is not static and its `value` would never
be exported, and they are nothing to read a label with. Every other label
MUST get from the load the verdict and the error it got before this rule. An
`expression` label MUST be interpreted by the same transform as the metric
expression:

| Transform | `expression` | `labels` |
| --- | --- | --- |
| `jq`, `yq` | jq-compatible expression evaluated against decoded data | jq-compatible expressions evaluated against the same data |
| `regex` | RE2 expression with at least one capture group; the capture group named `value`, or else capture group 1, is the numeric value | capture-group number or named capture group |
| `csv` | numeric column name | column names |
| `css` | CSS selector for numeric text; without `items`, matching at most one element | selectors relative to the selected element |
| `xpath` | XPath selecting numeric text | XPath evaluated at each selected node, an absolute path in it from the document, or `@attribute` |
| `prometheus` | regular expression matching source metric names | destination label name to source label name |

A `prometheus` rule's `expression` is a regular expression matched anywhere
in the name the target gives a metric; a rule without one MUST match the
metric its `name` names, and an `expression` written `""` is the key left
out (§ 24.3). An `expression` that is written and is nothing but blanks —
text that `strings.TrimSpace` leaves nothing of — which matches only the
names that hold those blanks, MUST be refused at load, by the exporter and
by the schemas (§ 24.3), as an entry of `transform.include` is (§ 6). The
error MUST name the collector and the metric, quote the expression, and say
what an expression is and what to write: the pattern that was meant,
`'[ ]'` or `'\x20'`, in single quotes, for a pattern that does mean a blank,
or the key left out for the rule to pass on the metric of its name. It MUST
be reported beside the rule's other mistakes, after them, and every rule
without such an expression MUST get from the load what it got before this
rule. Under every other transform that reads an expression, one of nothing
but blanks is no expression, and the rule MUST be refused as one that has
none.

A `prometheus` rule MUST have a `name` or an `expression`, written with
something other than `""`, which is the key left out: they are how it says
which of the target's metrics it passes on. A rule with neither would be
matched by the empty name, which no metric has, so it would make no series
whatever the response held and whatever else it set, and, being required
unless it says otherwise, be reported as missing its value on every scrape,
a mistake of the configuration told as the target's. It MUST be refused at
load, by the exporter and by the schemas (§ 24.3). The rule has no name to
be named by, so the error MUST name the collector and the rule's place
among the collector's `metrics`, counted from 1, as in `collector "node"
metrics rule 2 has neither a name nor an expression`, and MUST say what a
`prometheus` rule needs — an `expression` to match metric names by, or a
`name` to match the metric of that name — and what to write, `expression:
'.*'` for a rule about every metric among it. Every such rule of a collector
MUST be reported, in order, beside what the collector's other rules are
refused for. A rule with neither that the load refuses for something else —
a type or an `error_mode` that is none, `items`, a label it does not take —
MUST be refused for that alone, as it was, and every rule with a
`name` or an `expression`, and every rule of another transform, MUST get
from the load what it got before this rule.

A `prometheus` rule's `name` is one metric's name and no pattern. A name
that is no metric name MUST be refused as one, in the words the rule of any
transform is (§ 24.2), and where it holds a character a regular expression
gives a meaning — `.`, `*`, `+`, `?`, `^`, `$`, `|`, `(`, `)`, `[`, `]`,
`{`, `}` or a backslash — the error of a `prometheus` rule MUST say as well
that a pattern to match the target's metric names by is the rule's
`expression`, not its `name`, and that `name`, beside an expression, is the
one name the series it matches are exported under. A name that is none for
another reason, a name Prometheus reserves, and any name under another
transform, whose rules have no pattern to be mistaken for, MUST NOT be told
that. Which names are metric names is the collector's `name_escaping` to
say (§ 21.1): under `fail` a name with a dot MUST be refused, under
`prometheus` with that advice beside what would export it, and under
`underscores` and `values`, where it is a name like a target's, it MUST be
taken as the metric of that very name, with no advice, a dot being as much
a character of `http.server.duration` as of a pattern.

Two rules of a collector MUST NOT be the same rule: alike in everything
that decides which series a rule makes, so that each makes every series the
other makes, of one name and one set of labels, and every scrape fails on a
duplicate series (§ 21), a mistake of the configuration told for ever as a
scrape's. They MUST be refused at load. Two rules are the same rule when
they are alike in

- `name`, `expression` and `items`, each as written, `""` being the key
  left out;
- their labels: the same label names, each with the same `value` or the
  same `expression`, in whatever order the labels are written. Labels of
  one name keep the order they are written in, the last being the one a
  series gets; and in a `prometheus` rule one of whose labels reads a label
  the rule sets under another name, where the order decides the value, all
  the labels are compared in the order they are written;
- `value_map` and `time_format`, which say which texts the rule reads a
  value from: of two rules that differ in one, each MAY read what the other
  cannot — a field that is `up` or a number, a time written one way or
  another — and a scrape then gets one series, so they are two rules.

What else a rule says MUST NOT make it another rule: `scale`, `time_zone`,
`description`, `required` and `error_mode` change the value or the help
text of a series, or what is done when there is none, and a label's
`truncate` and `required` change no label's name, while a label's
`value_map` and a rule's `type` are one for the rules of a name. The error
MUST name the collector, both rules by their places among its `metrics`,
counted from 1, and the metric — of a `prometheus` rule without a name, the
expression — as in `collector "node" metrics rule 1 and rule 3 are the same
rule of metric "up"`, and MUST say to take one of the two out or tell their
series apart by a label. Each rule that repeats an earlier one MUST be
reported once, against the first of its kind, after everything else the
collector's rules are refused for. Only rules the load takes otherwise are
compared: a rule refused for something else MUST be refused for that alone,
and a collector without two such rules MUST get from the load the verdict
and the words it got before this rule. Rules of one name that differ in
expression, `items` or a label MUST load, several rules being the way one
family is read from several places (§ 24.2); among them, rules alike in
name, expression and `items` that their labels tell apart stay one rule to
the rule report and to the log (above, § 25.1). The comparison is of what
is written: two expressions of one meaning are two rules to it and load. A
`python` rule makes no series, so two alike MUST load. No schema can
compare the items of a list so, and the refusal is the exporter's alone
(§ 24.3).

Of two `prometheus` rules of a collector, the `expression` of one MUST NOT
match the `name` the other, having no expression, passes on the metric of,
where the two give that metric's series one name and the same labels: each
then makes every series of the metric that it does not fail on, and a
scrape that has a series twice fails on a duplicate series (§ 21). Where
neither fails on a series, that is every scrape of a target that has the
metric, and of a target that has none the rule of the name makes nothing,
so that rule, which is about the one metric, makes a series of no scrape
that passes. The pair MUST be refused at load. Whether the expression
matches is decided as the transform decides it of a metric of the target:
the expression compiled the same and matched anywhere in the name. The
rules give the series one name when the rule with the expression has no
`name` or has that same `name`; with another it exports the metric under
that, and the two MUST load. Their labels are the same when the rules are
alike in them as two rules that are the same rule are (above); rules that
differ in a label MUST load. What else the rule of the name says MUST NOT
change the verdict — `scale`, `type`, `description`, `required`,
`error_mode`, a label's `truncate` and `required` — since where one of them
keeps that rule from making a series, it makes none. What else the rule with
the expression says MUST NOT change the verdict either. The one exception to
both is a `type` of `histogram` or `summary` in the rule with the expression
(below). A rule that
sets a `type`, a `scale` or a `required` label, and whose `error_mode` is
not `fail`, MAY fail on a series of the metric — a histogram or a summary
keeps its type and takes no scale, a `type` of `histogram` or `summary`
applies to no other metric, and a series may not have the label — and carry
on, the scrape passing with the series the rule of the name made, which
only the target decides: a `type` of `gauge`, `counter` or `untyped` and a
`scale` fail the scrape of a metric that is a gauge, a counter or untyped
and pass that of a histogram or a summary, a `type` of `histogram` or
`summary` fails the scrape of a metric of that type alone, and a `required`
label fails the scrape whose series have it. A pair with such a rule MUST
be refused as every other is, whatever a target would have made of it,
every series the rule does not fail on being made by each rule. One such
pair MUST load instead: that whose rule with the expression has a `type` of
`histogram` or `summary` and an `error_mode` other than `fail`, unless the
rule of the name sets that same `type`. Such a `type` applies to a metric of
that type and to no other, so the rule fails on, and carries on without,
every metric of another type: it is a rule of the histograms, or of the
summaries, alone, and leaves an ordinary metric to the rule of its name, as
`name: up` beside `expression: '.*'` with `type: histogram` and `error_mode:
ignore`, the metric up and every histogram. The `type` alone decides: the
pair MUST load whatever else that rule sets, a `scale` or a `required`
label, which keep it from more series and from no fewer, and whatever other
`type` the rule of the name sets. Where the rule of the name sets that same
`type` it makes the metric only where the other rule makes it too, and under
`error_mode: fail` the rule fails the scrape on a metric of another type:
both MUST be refused, in the words given below for a rule that can fail on a
series and carry on and for one that cannot. A pair that loads so still
makes a series twice where the target's metric of that name is itself of
that type, which only the target decides; the scrape of such a target MUST
fail on the duplicate metric series, as any scrape that has a series twice
does, and the user documentation MUST say so and what to change. Nothing
else is refused: two rules that both have an expression, a rule of a name
beside one that exports another metric under that name, and rules whose
labels differ as written — a `required` label that the rule of the name has
not among them — MUST load, and fail the scrape that has a series twice as
they did. The error MUST name the collector, both rules by their places
among its `metrics`, counted from 1, the metric and the expression, as in
`collector "node" metrics rule 1 and rule 2 both pass on metric "up": rule
1 passes on the metric of that name, the expression "^up$" of rule 2
matches that name`, and MUST say to take one of the two out, write the
expression so that it does not match the name, or tell their series apart
by a label. Of a rule with an expression that makes a series of every
series it matches or fails the scrape — it sets no `type`, no `scale` and
no `required` label, or its `error_mode` is `fail` — it MUST say, as it
did, `and their labels are alike, so each makes every series of the metric,
and a scrape that has a series twice fails, as a duplicate metric series`.
Of a rule that can fail on a series and carry on it MUST NOT say that each
makes every series: it MUST name what that rule sets of the three — the
type, the scale, and each required label by its name, once — and say that
these do not keep the rule from the metric, as in `and their labels are
alike; the type and the scale of rule 2 do not keep it from the metric,
since every series of the metric that rule 2 does not fail on is made by
each rule, and a scrape that has a series twice fails, as a duplicate
metric series`, followed, where one of the two rules sets a `type` the
other does not, by `or, where the two rules give the metric different
types, as a metric of inconsistent types`. Each such pair MUST be reported
once, whichever of its rules is written first, after the rules that are the
same rule as an earlier one; a rule that is such a copy MUST be reported as
that alone, and a rule refused for something else for that alone. A
collector without such a pair — every collector of another transform among
them — MUST get from the load the verdict and the words it got before this
rule. No schema can ask whether one item's expression matches what another
item names, and the refusal is the exporter's alone (§ 24.3).

A `regex` rule's value MUST be the text of the capture group named `value`
(`(?P<value>...)`) when its expression has one, wherever the group stands,
and of the first capture group otherwise; an expression without a group
named `value` MUST give exactly what it gave before the name had a meaning.
Everything said of the value holds for that group: `value_map`, `scale` and
`time_format` apply to its text, and a match in which it took no part or
captured only whitespace is a missing value of that match, whose error names
the group. Labels MUST go on referring to groups by number or by name, a
group's number being its place in the expression whichever group is the
value, so a label can read a group that stands before the value; a label MAY
read the value's group itself, by `value` or by its number, and takes its
text as written. An expression with two or more groups named `value` MUST be
refused at load.

For example, these labels distinguish a response-derived value from a
constant:

```yaml
labels:
  - name: server
    expression: server       # current CSV row's server column
  - name: environment
    value: production
```

A label MAY set `truncate: true`. A value longer than
`limits.max_label_value_length` then MUST be cut to that many bytes, on a
character boundary, ending in `…`, the mark counted within the limit; without
it, such a value MUST fail the scrape as before (§ 21), since a silently
shortened value would surprise. A limit of 1 or 2 bytes has no room for the
mark, and the value MUST be cut without it; a value of which nothing is then
left, its first character being longer than the limit, MUST leave the label
off its series, an expression's and a constant's alike, rather than export it
with an empty value. Truncation MUST apply to the labels of declared
metrics from every transform, a `prometheus` rule without a name included and
the series of a `python` script that a rule names (the rule makes no series;
it declares the script's metric and the label to cut), and
MUST happen before `transform.rename_labels` and before `metrics_prefix`
(§ 5.0a) is added, so a renamed label is still cut, after invalid UTF-8
is repaired, so the repair cannot grow a cut value past the limit, and after
the label's `value_map` has mapped the value, for every kind of rule, a
`prometheus` rule without a name included, so the value exported is the one
cut and a cut value is never looked up in, nor mapped long again by, a map.
`truncate: true` on a label of a rule with a name MUST cut that label of
every series of that name, as a label's `value_map` maps it: those of the
other rules of the name, and those a `prometheus` rule without a name keeps
under it.

A jq or yq expression label's value MUST be written as text the way JSON
writes it: a number without an exponent when its magnitude is at least 1e-6
and below 1e21, so integers such as IDs keep every digit (JSON integers MUST
reach jq as integers, of any length), and in shortest
exponent form otherwise; `NaN`, `+Inf` and `-Inf` for non-finite numbers;
`true` and `false` for booleans; a string as it is. An object or an array MUST
be a failure of the metric rule, handled by its `error_mode`, with an error
saying to select a field or join the array, rather than a label in any
language's syntax for it. A Python script's label values passed to `metric()`
MUST be written the same way: a number or a boolean as its text, `None` and
the empty string as no label (§ 16.2), and a list, tuple, set or dict
refused with an error naming the label, as MUST be `labels` that is not a
mapping. An XPath label that computes a number, a label that is
one `sum()` included, MUST be written as a jq label's number is, so the same
number is the same label whichever language computed it.

An expression label that gives a series no value — a selector or path matching
nothing, a missing attribute, capture group or source label, an empty CSV
cell, a null — or an empty value MUST be left off that series, in every
transform, so the text exposition and OTLP agree. A series the exporter
makes MUST NOT have a label with an empty value, whatever gave it the label: one
of a target's own exposition is left off by the `prometheus` decoder, so a
`prometheus` rule's label read from it is a missing source label (§ 14.1); a
script's is left off where its answer is read (§ 16.2); a `transform.labels`
value and a static target's label written `""` are the label left out (§ 6,
§ 42.14); a value `truncate: true` cuts to nothing leaves the label off
(above); and a rule's constant written `""` is no `value` at all, refused at
load when the label has no `expression` either (above). The exporter's own
series MUST NOT have one either: a static target that names no target has no
`target` label on its health series (§ 42.14), and an OTLP resource
attribute written `""` is the attribute left out (§ 42.1). A `csv` label naming a
column the response does not have is no such label: it MUST fail its rule
(§ 42.2). An `expression` label MAY set
`required: true`. A series missing a required label MUST then be a missing
value of its metric: handled by the metric's `error_mode`, where `ignore` and
`log` drop that series alone and `fail` fails the scrape with an error naming
the label, counted as a missing key, and regardless of the metric's `required`
and `error_handling.allow_missing_keys`. `required` on a `value` label, or on a
label of the python transform, MUST be rejected at startup.

Python transforms are the exception: their script emits the common metric
objects through `metric(...)`, so a `metrics` array is optional for them. A
rule of a `python` collector makes no series, and its label names a label of
the script's series to cut with `truncate: true` and nothing else. A `value`
on such a label MUST be rejected at load, by the exporter and by the schemas
(§ 24.3), naming the collector, the metric and the label and saying that the
script sets its labels itself, that a rule's label there only names one to
cut with `truncate: true`, and that a constant for every series belongs in
`transform.labels`: it would otherwise load and be given to no series. A
`value` written `""` is the key left out there as everywhere. Every other
rule, of a `python` collector and of any other, MUST get from the load the
verdict and the error it got before this rule.

A rule of a `python` collector names a series the script makes, and MUST say
nothing the script says of its series itself. It MUST have a `name`, the
name the script gives the series: by it a debug probe's report lists the
rule among the rules that gave no series when the script made none, the
rule has its `http_exporter_rule_failures_total` series, which stays 0 since
a rule that makes no series fails none, and its labels find the series they
cut. Each of its labels MUST set `truncate: true`, which is all such a label
does; its `expression`, which every label without a `value` has, is not
read, and neither is the rule's own, which MAY be written. A rule's `type`,
`description`, `required` and `error_mode`, a rule without a `name` and a
label without `truncate: true` MUST be rejected at load, by the exporter and
by the schemas (§ 24.3): each would otherwise load and do nothing — a rule
saying `counter` leaving the series the gauge the script made it, a
`description` being no series' help, `error_mode: fail` failing no scrape
whose script made no such series, a rule without a name cutting no label,
and a label's `expression` reading no label. The error MUST name the
collector, the metric — a rule without a name by its place (§ 24.2) — and,
for a label, the label, and MUST say what to do
instead: that the script gives each series its type and its help text, with
`metric(..., type="counter")` and `metric(..., help="...")`; that a script
which cannot do without something fails the scrape itself, with
`fail("...")`, and that `error_handling.on_transform_error` says what the
collector does then; that a rule names a series of the script, by the name
`metric(...)` gives it; and that a label there only names one to cut, the
script setting its labels itself. A rule that says several of these MUST be
told all of them, in that order — the name, `type`, `description`,
`required`, `error_mode`, then its labels in theirs. A text key written `""`
is the key left out there as everywhere, and `required` is written when it
is there, `false` as `true`. These checks MUST come after every other check
of the rule, so that a rule refused before them is refused in the words it
was — a type or an `error_mode` that is none, `items`, `scale`, a
`value_map`, a `time_format`, a label that is required, has a `value_map` or
a `value` — and every rule they do not concern, of a `python` collector and
of any other, MUST get from the load the verdict, the error and the defaults
it got before them. PYTHON.md MUST say, in one place, what a rule of a
`python` collector is for and what it does not take.

A collector MAY set `derived`, a list of entries each computing a metric
family from the collector's own series, the same way under every transform.
An entry MUST have a `name` and MAY have a `description`, its help text, and
a `type`, `gauge`, `counter` or `untyped`, defaulting to `gauge`; any other
type MUST be rejected at load. It MUST set exactly one of `sum`,
`difference` and `ratio`, each a list of the names of the families it reads:
`sum` of two families or more MUST add up their series, `difference: [a, b]`
MUST be `a - b` and `ratio: [a, b]` `a / b`, each of the series of the
operands matched one to one as PromQL matches them without `on` or
`ignoring`, by identical label sets, a label of an empty value being no
label; the result MUST have those labels. A series with no partner in every
other operand, and a label set two series of one operand have, MUST make no
derived series. `by` MUST be taken only beside a `sum` of exactly one
family, whose series it MUST add up into one series for each set of values
of the labels it lists, with those labels only, a label of an empty value
being absent; `by: []` MUST give one series without labels. A result that is
NaN or infinite, a ratio over 0 among them, MUST make no series and MUST NOT
be a failure. A derived series MUST have no timestamp and no created
timestamp.

The entries MUST be computed in their order, and an entry MAY read an
earlier one; `hidden: true` MUST compute an entry for the later ones without
exporting it. Derivation MUST read the series as the rules made them —
after the rules' `value_map`, `scale` and `time_format`, the label value
maps and truncation — and MUST come before `transform.labels`,
`remove_labels`, `rename_labels`, `metrics_prefix` and name escaping, which
MUST apply to derived series as to every other: operands and `by` labels are
written as the rules write their names. Each exported derived series MUST
count against `limits.max_metrics`, a scrape past it failing as one whose
rules make too many does; a hidden entry's series MUST NOT be counted. The
derived series of a prometheus pass-through MUST be added without writing
to the decoded series.

At load the exporter MUST reject, naming the collector and the entry (by its
place, counted from 1, when it has no name): an entry without a name; a
name that is no valid metric name under the collector's `name_escaping`,
that is exported longer than `limits.max_metric_name_length`, that is a
rule's name, a `_bucket`, `_sum` or `_count` of a histogram or summary rule,
or another entry's; a `description` longer than `limits.max_help_length`;
no operation or more than one; a `sum` of fewer than two families without
`by`, a `difference` or `ratio` of other than two; `by` beside another
operation or a `sum` of other than one family, and a `by` label that is no
label name or is listed twice; an operand that is empty, the entry itself, a
later entry, a histogram or summary rule, or, under every transform but
`python` and `prometheus`, neither a rule's name nor an earlier entry's; and
a hidden entry that no later entry reads. Under `python` and `prometheus`,
whose series are named at run time, an operand no rule names MUST be taken
and read from the series of that name each scrape makes; a family of it a
scrape shows to be a histogram or a summary MUST make no series of the entry
and MUST be a failure of the entry under `error_mode: log`, logged and
counted in `http_exporter_rule_failures_total` under the entry's name, a
series every entry MUST have from 0. Under those two, an exported entry
whose name is a family of the series the scrape made MUST make no series,
MUST leave that family as the scrape made it, MUST NOT fail the scrape on
the duplicate series its own would have been, and MUST be a failure of the
entry as a histogram operand is; a later entry that reads it MUST read no
series. An operand the scrape has no series of MUST give the entry no
series and MUST NOT be a failure. A debug probe's report MUST mark each
derived family it lists as derived, with its operation and operands, MUST
NOT mark so a family of an entry that failed for its name, and MUST list
the exported entries that gave no series apart from the rules, each with
the operands that had no series in the scrape, `operand x: no series`,
those of an entry it reads among them.

## 18.2 Metrics per item

For the `jq`, `yq` and `css` transforms a metric MAY set `items`. For `jq` and
`yq` it is a jq expression
selecting the things the metric is about. The value expression and every label
expression MUST then be evaluated once per item, with the item as `.` and the
whole document as `$root`, instead of as parallel streams over the whole
document paired by position. Parallel streams drift: a label expression that
yields nothing for one element shifts every later value onto the wrong series.
Without `items`, a label expression yielding no value MUST leave the label off
every series, one value MUST apply to every series, and one per series MUST
pair by position; any other count MUST fail the metric under its `error_mode`,
naming the label and the counts, rather than export mislabelled series.

Such a metric fails as a whole, and MUST be reported as one failure: no
series of it MUST be exported; under `log` and `ignore` exactly one failure
MUST be counted for it, and logged under `log`, being the failure of the rule
and not of any of its values; and under `fail` the scrape's error MUST be that
failure. What fails a rule without `items` as a whole MUST be, in this order:
its expression failing at any of its values (`metric "m" expression: …`);
then the first of its labels, as the rule lists them, whose expression fails
at any of its values, which gives another count than none, one or one per
series (`label "a" gave 7 values for 4 series, so they cannot be paired; give
one value, or one per series, or set items to evaluate labels per element`),
or which gives a series a value that is an object or a list
(`metric "m" labels: …`). A value that fails by itself — missing, no number,
its series lacking a required label — MUST be counted and reported as that
value's failure only where nothing fails the rule as a whole, whichever is
met first; under `fail` the first such value MUST then be the scrape's error.
The exporter MAY take the values one at a time and stop at the first series
past `limits.max_metrics` (§ 20), which then is the scrape's failure; to tell
whether the rule fails as a whole it MUST NOT keep the values it reads.

```yaml
metrics:
  - name: server_cpu
    items: .servers[]
    expression: .cpu
    labels:
      - name: server
        expression: .name
      - name: site
        expression: $root.site
```

For one item, the value expression and each label expression MUST produce at
most one value; more MUST be an error naming the expression. A missing or null
value MUST be that item's missing metric, handled by `required` and `error_mode`
as for any other metric, and the other items MUST be unaffected under `ignore`
and `log`. A missing or null label MUST leave that label off the series,
unless it is required (§ 18.1). An
`items` expression that selects nothing MUST be a missing metric when the metric
is required and produce nothing otherwise.

For `css`, `items` is a CSS selector choosing the elements the metric is
about, typically table rows, and the value expression and every label
expression MUST be CSS selectors matched within one item at a time. Within an
item each MUST match at most one element, the rest of this section applying as
for jq: a value selector matching nothing is that item's missing metric, and a
label selector matching nothing leaves the label off. Items MAY be nested in
one another — `items: div` over nested divs, `li` over nested lists — and a
selector then matches within an item everything beneath it, the items nested
in it included. Matching a selector within every item MUST cost what the
items' subtrees hold, each node matched once, and not the depth of the
nesting times that. Without `items`, a
css metric is one series, whose value is the text of the element the
expression selects; an expression matching more than one element MUST be a
failure of the rule, handled by its `error_mode`, and the error SHOULD point
to `items`. No series of the rule may be exported when it fails that way. A
label selector could only read text that is part of the number: a css metric
with an `expression` label and no `items` MUST be rejected at startup, and its
static `value` labels are unaffected. `items` on any other transform MUST be rejected
at startup.

Every transform MAY define one `transform.pre_script`. The exporter MUST run
it exactly once per scrape, after decoding and before evaluating the metric
array. The script receives `data`, `response`, `target`, and `collector`; it
MAY mutate `data` or replace it by assigning a new value to `data`. JSON/YAML,
CSV, and text receive decoded values; HTML/XML receive raw document text and
the exporter MUST parse the returned text again before CSS/XPath extraction.
The pre-script uses the same timeout, output limit, and Python restrictions as
Python metric scripts. A pre-script failure is a transform error.

A pre-script MUST produce its result in the variable `data`. The exporter
reads `data` back after the script runs, so a script that computes a value and
leaves it in another name discards its work silently: the transform then runs
against the untouched decoded response and the collector appears to be
mis-extracting rather than misconfigured. Replacing `data`, mutating it by key
or attribute, augmenting it, binding it as a loop or `with` target or with an
assignment expression (`(data := ...)`), deleting from it (`del data["x"]`),
and calling a method that mutates it all satisfy this.

A part of `data` under another name is `data` still, and changing it MUST
satisfy the rule as changing `data` does: a name assigned a part
(`rows = data["rows"]`, `rows = data.get("rows") or []`), the target of a
loop or a comprehension over `data` or a part of it, `enumerate`, `sorted`
and the like between them or not (`for row in data["rows"]: row["cpu"] =
...`), and a name bound to a part of one of those in turn. Handing `data`
or such a part to a function the script defines MUST satisfy it too, since
the function may change what it is handed. A copy does not: `dict(data)`,
`list(data["rows"])` and `data.copy()` under another name are other
objects, binding a part's name anew changes nothing, and a builtin or a
module's function handed `data` reads it.

Reading `data` MUST NOT satisfy it, however thoroughly the script reads. A
method call counts only when the method mutates its receiver: `data.update(...)`
and `data["rates"].append(...)` produce `data`, while `data.items()`,
`data.get(...)` and `data.copy()` are reads. A script that reads `data` at
length and assigns its result to another name is the mistake this rule exists to
catch, and treating any method call on `data` as a mutation would let exactly
that script start.

This MUST be enforced as a configuration error rather than a scrape error. The
exporter MUST refuse to start when a configured pre-script never produces
`data`, MUST reject such a configuration on reload with the last valid
configuration left active, and MUST report the offending collector by name. It
MUST report every faulty script in one message rather than only the first.

The same check MUST reject a Python syntax error in any configured script,
including a `python` transform script, so a script that cannot compile is caught
before the exporter serves traffic. A `python` transform emits through
`metric(...)` rather than `data` and MUST NOT be required to produce it.

Enforcing this requires parsing the script, so a configuration that contains any
Python MUST be checked with the configured interpreter and MUST fail to start
when that interpreter is unusable, with an error naming its path and why it
failed, followed by what it wrote to stderr only when it wrote anything. A configuration containing no Python MUST NOT
invoke an interpreter at all, so a deployment that uses none is unaffected.
One check of a configuration MUST start the interpreter once, whether its
scripts are sound or faulty and however many collector files it reads: the
collector file a faulty script is reported against (§ 5.0) MUST be found by
the name of the script's collector, not by checking each file's scripts
again.

A pre-script that returns a mapping or a sequence produces structured data. When
the collector's transform reads structured data — `jq` or `yq` — the exporter
MUST treat that result as the decoded response
and MUST NOT reject the collector because of the response's original format. The
decoded format becomes JSON for the rest of the scrape, whatever the response
originally was. This lets an unstructured response be reshaped once in Python
and then declared with the ordinary `metrics` array, so a collector that needs
Python only to parse its response keeps the same configuration shape as every
other collector.

The promotion MUST NOT apply when the transform consumes the original response
format. `csv`, `regex`, `css`, `xpath`, `prometheus`, and `python` MUST continue
to receive the decoded format they already require, and a pre-script returning a
scalar MUST leave the decoded format unchanged for every transform. A structured
transform whose pre-script returns a scalar MUST still be rejected with the
existing response-format error.

# 19. Missing keys and error handling

Error behavior MUST be explicit and configurable.

Recommended collector configuration:

```yaml
error_handling:
  on_fetch_error: fail
  on_decode_error: fail
  on_transform_error: fail
  allow_missing_keys: false
```

The policies MUST be:

```text
fail     fail the probe at that stage
log      carry on and log the failure at warning level
ignore   carry on, logging the failure only at debug level
```

The policies MUST apply alike to a probe and to a static target's scrape
(§ 42.14). A probe carrying on MUST be answered exactly as a probe whose trip
went through whole and produced no series is answered, by the same writer:
`200` with an empty exposition in the format the request asks for (§ 21) —
the text format's `Content-Type`, or the OpenMetrics one of the version asked
for and `# EOF` — with `X-Content-Type-Options: nosniff` and `Vary: Accept`,
compressed as any answer of `/probe` is (§ 23), and to `HEAD` the same
headers. An exposition is identified by its `Content-Type`: the answer MUST
NOT be sent without one, or as OpenMetrics without `# EOF`. That MUST hold
for every stage the policies cover, under `log` and `ignore` alike, and
probes sharing the trip (§ 42.13a) MUST each get the format they asked for.
While the collector sets `cache.stale_if_error` the answer MUST hold the two
freshness gauges every answer of the collector has (§ 42.13.1),
`http_exporter_result_stale` 0 and `http_exporter_result_age_seconds` 0, and
nothing else. A trip that carried on is no result: it MUST NOT be stored in
the response cache, where an earlier result MUST stay as it was, and MUST NOT
be queued for OTLP; and it is no failure: it MUST NOT be answered with a
stale result, the failed stage MUST be counted in the self-metrics as under
`fail`, and the probe MUST count in `http_exporter_scrape_success_total`,
unless the collector sets `report_failures`, which makes such a probe a
failure (§ 19.5). A
debug probe (§ 42.17) MUST report the stage as failed and carried on past,
and that a probe would have been answered `200` with no series of the
collector's. A
static target scrape carrying on produces `http_exporter_target_up` 1 and no
collector metrics — its health series alone, without the freshness gauges —
counts as a success, and is logged at warning level under
`log` as `static target stage failed; continuing` — through the failure log
(§ 25.1), and without logging the target as recovered, since the stage still
failed. A metric rule with `error_mode: fail` (§ 18.1) MUST fail the scrape of
either whatever `on_transform_error` says. Failing to read target credentials,
the concurrency limit (§ 42.13b) and validation of the result are not stages
these policies cover, and always fail.

`on_fetch_error` MUST govern failing to obtain the response, whatever the
request type (§ 5.1): for `http` a transport failure or a non-success status,
for `localfile` a file that cannot be read. It is named for the stage rather
than for one type, so it reads the same on every collector.

This is the vocabulary `error_mode` uses (§ 18.1). The value MUST be written
in lower case and without blanks around it, as the schemas take it; anything
else, `LOG` and ` log ` included, MUST be rejected naming the collector, the
key and the accepted values.

No configuration spelling is deprecated at present. When one is replaced and
the old spelling kept for a while, each use of it MUST be accepted with the
new meaning and reported as deprecated: logged at warning level on startup and
on every reload, naming the collector, the key and the replacement, and listed
under `deprecations` in the `config` check of `--dry-run` (§ 30.1), which still
passes.

### 19.1 Distinguish failure types

These MUST be treated as different classes:

1. HTTP failure
2. HTTP non-success status
3. Response-size limit exceeded
4. Decode/parse failure
5. Transform failure
6. Missing key/field
7. Metric validation failure
8. Cardinality/limit failure
9. Python execution failure

These classes that have self-metrics of their own — a response over the size
limit (`http_exporter_series_limit_exceeded_total`), a value the response did
not contain (`http_exporter_missing_keys_total`) and a Python failure
(`http_exporter_script_errors_total`) — MUST be told apart by the kind of the
error, set where the error is made and read with `errors.Is`, and never by
searching the error's text: an error that merely mentions "missing", "response
size" or "python" MUST NOT raise those counters, and rewording a message MUST
NOT stop one being counted. A value the response did not contain is any
required value a rule could not find: a jq or yq value, a CSV column, a CSV
response without a row, a regex that matched no text, an XPath or CSS selector
that matched no nodes, a prometheus rule that matched no metric, and an
`items` expression that selected nothing.

### 19.2 Missing keys

With:

```yaml
allow_missing_keys: false
```

accessing an absent field required by a metric SHOULD cause that metric or scrape to fail according to the selected error policy. For a declared metric that policy is the rule's `error_mode` (§ 18.1): `ignore` and `log` omit the metric, and `fail` fails the scrape.

With:

```yaml
allow_missing_keys: true
```

missing optional fields SHOULD result in omitted metrics rather than an entire scrape failure.

### 19.3 Per-metric requiredness

Support or plan to support:

```yaml
metrics:
  - name: important_metric
    required: true

  - name: optional_metric
    required: false
```

This overrides collector defaults where appropriate. A metric that is not
required is never a failure when its value is absent, so its `error_mode` does
not apply; `required` decides whether an absent value is a failure at all, and
`error_mode` decides what a failure does.

Neither setting makes anything else a missing value: a body the decoder
cannot read MUST still be a decode error with `allow_missing_keys: true`, and
a value that is present but invalid — text that is not a number, a label
that is an object or an array — MUST still be the rule's error, by its
`error_mode` (§ 18.1), never an absent value left out without one. The one
exception is § 11's: an XPath expression that computes a value, whose
string that is not a number is the rule's missing value.

### 19.4 What a failed probe's caller is answered

Whoever may call `/probe` may name the target and the collector's request
parameters, so what the target answered MUST NOT reach a failed probe's
answer. The answer — the text after `collector <name> <stage> failed: `, and
`error` in the JSON object of a rule under `fail` — MUST be the failure's
text as a probe's caller is told it (`model.CallerText`):

- every value of the answer a failure quotes (`model.Quoted`, `Bare`,
  `Shown` and `ShownAs` in an error made by `model.Errorf`) MUST be
  replaced by `[withheld; in the exporter's log]`, the rest of the text as
  it reads; a value may name what the caller is told of it instead
  (`QuotedValue.ForCaller`);
- an error marked as the operator's (`model.WithOperatorDetail`) MUST be
  replaced by the text it gives the caller, wherever it is in the chain;
- when a wrapper rewrote the text of an error that has something to
  withhold, so that its text is not found in the whole, the answer MUST be
  `what failed is in the exporter's log`, not the rewritten text.

These MUST be withheld so: an XML syntax error's names and entities (each
form `xmlcut.go` reads, its words kept), a TOML parser message that quotes a
token, a jq program's error on the answer (`the jq program failed on the
answer: [withheld; in the exporter's log]`), the start of a Graphite
server's answer that is not render JSON (not that it is empty), a `tcp`
server's greeting that its collector's `greeting` does not match, or that
the server cut short, and what Go's
HTTP client quotes of an answer that is no HTTP (`malformed HTTP response`,
`malformed HTTP status code`, `malformed HTTP version` and the `malformed
MIME header` forms, the text after Go's words withheld), for a target's
request and a login's. A Python script's error MUST be answered as the
exception's type, ` at line <n> of the script` for the innermost frame of
the script when there is one, and `; the rest is in the exporter's log`, and
as `[withheld; in the exporter's log]` when its exception's line names no
type; what a worker wrote to stderr MUST be withheld, its error answered
with `; what it wrote to stderr is in the exporter's log` (or `what it had
written`, of a start that was stopped). An error over `model.MaxFailureBytes`
MUST keep what the caller is told of it when it is cut (§ 25), cut to the
same bound. The names of valid series are not values of the answer the
failure withholds: a successful probe answers them.

The addresses of the exporter's side of the network MUST be withheld from
the caller of any request type's failed fetch, read from the errors' types
as the failure log reads them (§ 25): the address a connection was made
from (`*net.OpError`'s `Source`), the whole of what a connection to a proxy
failed with (a `proxyconnect` error: the proxy's address, the name it is
reached by, its certificate), the whole of a SOCKS proxy's failure (a
`socks connect` error: the proxy's address, written where the source's
would be, and what its handshake said, told as `socks connect tcp:
[withheld; in the exporter's log]`), what a proxy said in refusing a tunnel (§
42.15b), the path of the Unix socket a request went to (a `*net.OpError` of a
`unix` network's `Addr`, § 42.15b), the name server a lookup asked (`*net.DNSError`'s `Server`), and the source
address in a resolver's exchange or a gRPC status as those write it. The
address the connection went to MUST stay. An error that rewrites the text
of the one it wraps, as the redaction of URLs does, MUST tell the caller
the wrapped error's caller text rewritten alike (`model.TextRewriter`).

The exporter's log, a debug probe's report and, with `--web.debug=full`,
the status page MUST show each failure whole.

### 19.5 Failed probes reported as series

A collector MAY set `report_failures: true`, a key of the collector beside
`error_handling`, `cache` and `failure_backoff`, `false` when left out. Every
probe of such a collector that its target was asked for, or answered for from
the response cache, MUST be answered `200` with an exposition, in the format
the request asks for (§ 21), and never `502`. Its answer MUST hold, after the
collector's own series, the gauges `http_exporter_probe_success`, `1` when the
probe succeeded and `0` when it failed, and
`http_exporter_probe_elapsed_seconds`, the probe's own duration, from its
request to its answer being ready, the duration its
`http_exporter_probe_duration_seconds{kind="probe"}` observation is; and, on
a failure alone, `http_exporter_probe_failed_stage{stage}` `1`, naming the
stage the failure is counted and logged in: `http` (whatever failed in reaching
an `http` target, its name, its connection or its TLS handshake),
`http_status`, `decode`, `transform`, `validation`, `metric` (a rule with
`error_mode: fail`), a request type's own fetch stage (`dns`, `tls`,
`file`, `grpc` and the others, § 5.1), or `panic` (a probe whose trip
panicked, in whatever stage, § 6.1a). The probe series MUST carry no label but
`stage`, and nothing of the failure's text: what a failed probe's caller is
told (§ 19.4) MUST NOT be answered at all. A failed probe MUST answer no
series of the collector's and no freshness gauges (§ 42.13.1).

A probe MUST be a failure to the series exactly when it is one of these:

- a stage under `fail` failed, a rule under `error_mode: fail` failed,
  `failure_backoff` kept the trip from the target (its fetch stage), or the
  trip panicked (`panic`): success MUST be `0` whenever the self-metrics
  count the probe failed, whether or not a stage's error failed it;
- a stage failed and `error_handling` carried on past it (`log`,
  `ignore`): the answer MUST hold what a probe carrying on holds (§ 19) and
  success `0` with the stage, and the probe MUST be a failure wherever the
  exporter counts or records it, as any failure below is: no count in
  `http_exporter_scrape_success_total`, `failed` with its error on the
  status page, answered with the last good result under
  `cache.stale_if_error`, and a failed probe's trace (§ 42.1c), whose
  `error.type` MUST be the stage, under the `errors` sampling. It MUST be
  logged as its policy says, at warning level under `log` and not above
  debug under `ignore`, and MUST NOT be cached or queued for OTLP. A rule
  under `error_mode` `log` or `ignore` that misses its value fails no stage,
  and the probe MUST stay a success;
- `cache.stale_if_error` answered the last good result in place of a failed
  trip: the stale series and freshness gauges MUST be answered with success
  `0` and the stage of the trip's failure.

A probe answered from the response cache MUST be a success. A probe the
target policy refused (`403`), one the concurrency limit turned away (`503`),
one whose caller went away and a mistake of the caller's (`400`) MUST be
answered as without the key. A failure MUST be
logged, counted in the self-metrics, kept from the response cache and from
the OTLP export, and recorded on the status page as without the key; the
status page MUST give its status as `200` and its series as those answered.
A failed probe's trace (§ 42.1c) MUST give `http.response.status_code`
`200` and, as its `error.type`, the stage that failed, never the status
the probe would have been answered without the key; a stale result
standing in keeps `stale`, and a probe `failure_backoff` kept from the
target `backoff`, as without the key.
The response cache MUST keep, and the OTLP export (§ 42.1) MUST export, the
collector's series alone, never the probe series; probes sharing a trip (§
42.13a) MUST each answer their own duration from the one result, which MUST
NOT be changed. A debug probe (§ 42.17) MUST say that a probe would have
answered `200` with the probe series, and show them; where the last good
result stands in for the failure, it MUST say that the probe would have
answered that result, its age and its being stale, with success `0` and the
stage, not the failure's own answer. Static targets MUST NOT be changed by
the key: a static target's scrape has `http_exporter_target_up` (§ 42.14).

A rule, or a derived family that is not hidden (§ 18.1), of a collector with
`report_failures` exported under one of the three names, after
`metrics_prefix` and `name_escaping`, MUST be refused at load, naming the
collector, the metric and the name; a series of one of the names a Python
script or a `prometheus` transform makes on the scrape MUST fail the
`validation` stage, as a freshness gauge's name does (§ 42.13.1). A collector
without the key MUST take such names as before.

---

# 20. Resource and cardinality limits

The exporter MUST have configurable safety limits.

At minimum:

```yaml
limits:
  max_response_bytes: 10485760
  max_metrics: 10000
  max_labels_per_metric: 20
  max_label_value_length: 500
  max_metric_name_length: 200
  max_label_name_length: 200
  script_timeout: 100ms
  max_cache_entries: 1000
```

The response a collector reads MUST be bounded by `request.max_response_bytes`
and `limits.max_response_bytes`: the smaller when both are set, the one set
otherwise, and 10 MiB when neither is. Neither MUST be filled in with the
default, so that either alone may raise the bound past it; a negative value
MUST be refused, and so MUST a value from 1 to 1023 bytes, naming the
collector, the key, the value and the least, 1KiB: a size without a unit is a
number of bytes, and a limit under a kibibyte is one whose unit was left out
(`max_response_bytes: 10` for 10 MiB), which fails every scrape. The bound MUST be held to one byte under the largest 64-bit
number, for every request type, so that the largest value that can be written
reads the whole answer rather than none of it. An HTTP answer whose `Content-Length` is over the bound MUST
be refused before its body is read, the error giving that size; one without
it MUST be refused once reading passes the bound, and the error MUST NOT give
a size it does not know. A `HEAD` answer's `Content-Length` MUST NOT be held
against it. The bound MUST count the answer decompressed: `Accept-Encoding`
is the exporter's own, so one in a collector's or a static target's
`request.headers` MUST be refused at load, and one the probe carries MUST NOT
be forwarded; listed in `forward_headers`, it MUST be refused at load
(§ 42.4).

An answer without a `Content-Length`, a chunked or a compressed one, SHOULD
be read into a buffer the size of the last such answer of the same collector
and target, with an eighth more, rather than into buffers grown as it comes.
That size is a hint only: an answer longer than it MUST be read whole within
the bound and one shorter MUST be the bytes that came. The hints MUST take a
bounded amount of memory whatever targets are probed, MUST NOT make a first
buffer of more than 4 MiB or the bound, and an answer much shorter than its
buffer MUST NOT keep the buffer: the response, which the cache may hold,
holds little more than its body. A `HEAD` answer and one whose
`Content-Length` is 0 have no body, and MUST be read without the hint; an
empty body MUST leave the hint as it was.

A probe's cache and coalescing key MUST write the length of every list before
it — the query's parameters, each parameter's values, the headers and each
header's values — so where one list ends and the next begins is part of the
key, and MUST treat header names without regard to case.

A trip cancelled because every probe waiting for it went away MUST NOT be
reported as a failure of the target: nothing logged, no stale answer counted
or queued for OTLP. One cancelled while its transform ran MUST NOT be counted
as a failed transform either — in `http_exporter_transform_errors_total`,
`http_exporter_missing_keys_total`, `http_exporter_script_errors_total` or
against the rule it was at — since the error that ended the transform is the
cancellation; what the trip had done by then, its request and its decode,
MUST stay counted.

`max_metrics` MUST bound the memory of a scrape as well as its size: every
transform MUST stop at the first series past it, and the prometheus decoder
at the first series its transform would pass on past it (§ 14.1), rather
than build every series the response describes and count them afterwards.
The error MUST read `metric count N exceeds limit M`, as the count afterwards
reads, where N is then M+1; it MUST be the scrape's `validation` stage failure,
counted in `http_exporter_series_limit_exceeded_total`, whatever
`error_handling` says. The limit MUST travel with the transform's context,
spanning every rule of the collector, not in a global. A regex rule MUST ask
for at most one more match than the limit has room for, and for more only
when matches made no series; a jq `items` expression's items MUST be taken
as the program produces them, and so MUST the values of a jq rule without
`items`, which MUST make the labels of a series when it makes the series and
of no value it does not keep, and MUST fail with the same error as one with
`items`. A value that makes no series MUST take no room. A jq rule without
`items` that fails as a whole after it made series — its program failing part
of the way, a label's values not pairing — MUST drop them and give their room
back, as a rule with `items` does. Room made for the series of a jq rule
whose `items` are counted beforehand SHOULD be made once for it and the
rules after it over the same `items`, and MUST NOT be more than the limit
leaves. A python transform's answer is bounded by
`max_output_bytes`, and its metrics MUST be counted before they are read.

The room is held by the series a scrape keeps, and by nothing else once a
rule has ended: when a transform ends, the series counted MUST be those it
returns, whatever its rules carried on without and whichever of them failed
as a whole, and a rule MUST find all the room that the series kept before
it leave. This holds for the prometheus decoder too, which MUST NOT count a
series its transform may carry on without (§ 14.1). While a rule runs, the
series it has made count: a rule that makes more than the limit has room
for MUST end the scrape with the limit's error at the first series past it,
also when the rule would have failed as a whole further on and given them
up, and MUST NOT go on through the response to find that out. The rules of
a transform run one after another, so the count needs no lock.

`max_cache_entries` bounds the number of live cache entries a single collector
may hold. It MUST default to a finite value and MUST be enforced by evicting
expired entries first and then the entries closest to expiry.

Implementation may also add:

- Maximum header size
- Maximum body size
- Maximum JSON/YAML nesting depth
- Maximum regex input size
- Maximum regex execution time
- Maximum Python instructions/CPU time
- Maximum transformation output size

The exporter MUST reject or safely terminate processing when limits are exceeded.

---

# 21. Prometheus metric validation

Before exposition, validate:

- Metric name syntax
- Label name syntax
- Label cardinality limits
- Duplicate metric definitions
- Duplicate label names
- Type consistency
- Numeric values
- Invalid NaN/Inf behavior according to Prometheus client conventions

A label name starting with `__` MUST fail validation, and MUST be refused at
load wherever the configuration names one (a rule's labels,
`transform.labels`, `rename_labels`, a static target's labels): Prometheus
refuses `__name__` in what it scrapes and drops the rest. A histogram with a
label `le` of its own, and a summary with a label `quantile` of its own, MUST
fail validation naming the series and the label, whatever gave it the label —
a rule, `transform.labels` or a script — since its buckets or quantiles carry
that label and the series would be written with it on `_sum` and `_count`,
which no parser reads as intended; on a series of another type both MUST be
labels like any other. Series that differ
only in a label with an empty value MUST be duplicates, since Prometheus reads
such a label as absent, and the error MUST say so.

A label name longer than `limits.max_label_name_length` (200 bytes by default,
the default of `limits.max_metric_name_length`; 0 or left out is the default,
a negative value MUST be refused at load) MUST fail validation as a metric
name longer than `limits.max_metric_name_length` does: the scrape's
`validation` stage failure, counted in
`http_exporter_series_limit_exceeded_total`, whatever `error_handling` and a
rule's `error_mode` say, whatever gave the series the label — a rule,
`transform.labels`, `transform.rename_labels`, a script's `labels`, a series a
`prometheus` transform passes through — and measured as the scrape exposes it,
after `name_escaping`. A label name MUST be checked after its metric's name,
so a series with both names too long fails on the metric's, and after the
label name is found to be a label name, before anything else about the label:
of a label whose name is too long and whose value is too long, the name MUST
be named. `le` and `quantile`, which a histogram's buckets and a summary's
quantiles carry, are label names too and MUST fail validation under a limit
shorter than they are. A label name the configuration writes longer than the
limit, as the scrape would expose it — a rule's label, a key of `transform.labels`, what `transform.rename_labels` renames to — MUST
be refused at load, naming the collector, the label, its length and the
limit; a rule's label or a key of `transform.labels` that
`transform.rename_labels` renames or `transform.remove_labels` removes is not
exposed under that name and MUST NOT be held to the limit by it, nor to
anything else the scrape holds an exposed name to (§ 21.1). A static
target's label, which goes on its collector's series past
the validation, MUST be refused when the target file is checked against the
configuration, naming the target, the label, the limit and the collector;
so MUST a target whose collector's limit is shorter than a label the
exporter itself adds to the target's series past the validation —
`static_target` on every series, 13 bytes, and `collector` and `target` on
its health series — naming the target, the label, its length, the limit and
the collector. A label the exporter adds before the validation, the `file`
of a collector that reads a directory, MUST fail the scrape when it is
longer than the limit, as any label does.

A family named like a series of a histogram or a summary — `foo_bucket`,
`foo_sum` or `foo_count` next to a histogram `foo`, `foo_sum` or `foo_count`
next to a summary `foo` — MUST fail validation naming both, since the
exposition would hold two families of one name.

What would fail this validation on every scrape, whatever the target
answers, MUST instead be refused when the configuration loads, naming the
collector and the metric: two rules of one collector giving one metric name
different types (rules giving it the same type feed one family and MUST be
accepted; a `prometheus` rule without a `type` is not compared); a rule named
as a series of another rule's histogram or summary; a `description` longer
than `limits.max_help_length`; and a static label value, of a rule or of
`transform.labels`, longer than `limits.max_label_value_length`, unless the
label is cut at the scrape — it has `truncate: true`, or a rule of the name
its series have sets `truncate: true` on a label of that name — a
`value_map` maps the value to a shorter one, or `remove_labels` drops it. A `python` transform's rules make no series, so only
its `transform.labels` are checked. A value that holds placeholders
(§ 42.10c) MUST be measured with each replaced by its default, and by
nothing where it has none, and its refusal MUST say so. Where a `value_map`
of the rule's name maps such a value of a rule's label, it MUST be measured
as mapped: a `"*"` entry longer than the limit, which every value the probe
gives that the map does not list becomes, MUST be refused whatever the
defaults, naming the entry; a longer entry the map lists MUST be refused only
where the value with its defaults is that entry's key, and otherwise MUST be
accepted, the scrape of a probe that gives the key failing validation. A
`prometheus` rule without a name keeps each series' own name, which the
value maps and the cuts go by: its static label, a constant or one with
placeholders, MUST be measured as it is, for the names no rule has, and
besides, by the rules above, as the `value_map` and the `truncate: true` of
each other rule's name that its `expression` matches make it, the name matched
as the scrape matches it — the pattern unanchored unless it anchors itself,
against the name the target wrote, before `metrics_prefix` and
`name_escaping` — and its refusal MUST name the metric whose map it is.
So MUST a collector whose series carry more labels than
`limits.max_labels_per_metric` before the target says anything: the labels of
`transform.labels` and a rule's static labels, each name counted once, as
`remove_labels` and `rename_labels` leave them, the refusal naming the
collector, or the rule where its static labels take the count over, the
count and the limit. Only labels every series has MUST be counted: not a
value with placeholders, which a probe may fill with nothing, not a static
label of a name a `value_map` of the collector may map to `""`, and not the
labels the exporter adds past the validation, such as a static target's. A
`python` transform's rules make no series, so only its `transform.labels`
are counted. A collector refused for anything else of this paragraph MUST be
refused for it as it was.

A scrape that fails validation for a limit of its series MUST say which and
what to change: the metric, what is over the limit and by how much, the limit
and its key, and the remedy. A label value over the limit MUST read `metric
"M" label "L" value is N bytes, longer than limits.max_label_value_length
MAX; a label one of the collector's rules gives can be cut to fit with
truncate: true on that label, or raise limits.max_label_value_length`: the
check knows no collector, the names are those the scrape exposes (after
`metrics_prefix` and `transform.rename_labels`), and the label MAY be one no
rule gives, as a directory's `file`, so the message MUST NOT tell the operator
to set a key that may not exist; more labels than the limit, `metric "M" has
N labels, more than limits.max_labels_per_metric MAX; drop labels it does not
need or raise limits.max_labels_per_metric`; a help text over it, `metric
"M" help is N bytes, longer than limits.max_help_length MAX; shorten it or
raise limits.max_help_length`; and a label name over it, `metric "M" label
name "L" is longer than limits.max_label_name_length MAX; rename the label
with transform.rename_labels, or raise limits.max_label_name_length`, the
name shown as a metric name over its limit is. Lengths MUST be in bytes, as
the limits are.

Metric names SHOULD be normalized only when explicitly configured; silent surprising renaming is undesirable. A collector's `metrics_prefix` (§ 5.0a) is such explicit configuration, and validation applies to the prefixed names.

The exposition MUST be served as `text/plain; version=0.0.4; charset=utf-8`,
with `X-Content-Type-Options: nosniff`: label values and help come from
scraped targets, and a browser MUST show any markup in them as text rather
than render it.

Every answer — `/probe`, fresh or from the cache, the static targets
endpoint and the self-metrics, in the text format and in OpenMetrics — MUST
have the series of a family together, under one `HELP` and one `TYPE`, those
of the family's first series: the families in the order each first appears,
and a family's series in the order they were made. Series MUST NOT be
sorted. That MUST hold whatever order a collector's series come in — rules
of one name with another rule between them, a script that emits a family's
series apart, a `prometheus` collector whose rules, `transform.rename` or
`name_escaping` give two source families one name — and a set that has
every family's series together MUST be written in its own order, at no cost
for the check beyond a comparison a series. A `csv` collector with several
rules MUST read its rows in order, each by every rule, so that the first
failure of a rule, the logged failures and the series limit are those of
that order,
and SHOULD keep its series rule by rule, each rule's in the order of the
rows; it MUST do so when `limits.max_metrics` has room for a series of every
row for every rule and every row has the columns of the first, as decoded
rows do and rows a pre-script left may not. What is found in the finished
set is found in the order the series are kept in: the duplicate series
validation names (§ 21) and the first metric of a warning of repaired UTF-8
(§ 6) are then those of the earliest rule that has one, whatever row it
is on. OTLP MUST carry all the points
of a family in one metric (§ 42).

`/probe`, the self-metrics and the static targets endpoint MUST answer in
OpenMetrics 1.0.0 instead, as `application/openmetrics-text; version=1.0.0;
charset=utf-8` (or `version=0.0.1` when only that is asked for) with `nosniff`,
when the request's `Accept` gives `application/openmetrics-text` of version
1.0.0, 0.0.1 or none a higher quality than any of `text/plain`, `text/*` and
`*/*`; a tie, no `Accept`, or only types the exporter does not write MUST get
the text format. Every such answer MUST carry `Vary: Accept`. Probes sharing
one trip to the target (§ 42.13a) MUST each get the format they asked for. The
OpenMetrics answer MUST hold the same series as the text one, written as
OpenMetrics requires: a family's series together, in the order the family
first appears; a counter's family named without `_total` and its samples with
it; `untyped` as `unknown`; HELP with `\`, `\n` and `\"` escaped; timestamps in
seconds; `le` and `quantile` values as canonical floats (`1.0`); and `# EOF`
last. No `_created` sample MUST be written for a series read from a target,
since the exporter does not know when a target's counter started; the
exporter's own counters, histograms and summaries have one where the
configuration asks for it (§ 22.1b).
No name MUST be claimed by two OpenMetrics families, where a family claims its
name and its type's sample names (a counter `foo`: `foo`, `foo_total`,
`foo_created`; a histogram `foo`: `foo`, `foo_bucket`, `foo_count`, `foo_sum`,
`foo_created`; a summary `foo`: `foo`, `foo_count`, `foo_sum`, `foo_created`;
a gauge or `unknown` family: its name). Where the natural forms above would
claim a name twice, the families involved MUST be written as `unknown` under
the sample names the text format gives them, so either format yields the same
series, and independently of the order they appear in: first every counter
whose claims meet another family's becomes the `unknown` family of its own
name; then every histogram or summary whose claims still meet another's
becomes one `unknown` family per sample name (`foo_bucket`, `foo_sum`,
`foo_count`; or `foo`, `foo_sum`, `foo_count`); gauges and `unknown` families
keep their type unless their name is one of those sample names, when they and
the lines of that name share one `unknown` family.
A counter named exactly `_total`, whose family would have no name, MUST be
written as the `unknown` family `_total`. Every other counter's sample MUST
be its family's name and the plain text `_total`, and its family the
counter's name without a `_total` it ends in, whatever the name is: a
strict OpenMetrics parser, and the exporter's own reader (§ 14.1), find a
counter's sample only under that name. That holds for a name
`name_escaping: values` wrote (§ 21.1), where the `_total` of the original
name reads `__total`, as Prometheus writes such a counter:
`U__my_2e_requests__total` MUST be the family `U__my_2e_requests_` with the
sample `U__my_2e_requests__total`, and the counter `U__my_2e_errors` the
family of that name with the sample `U__my_2e_errors_total`. The suffix MUST
NOT be cut or added in escaped form.
A series' timestamp MUST end every line of it: a plain sample's, and each
bucket, quantile, `_sum` and `_count` line of a histogram or a summary.
A histogram or a summary read from a target without a `_sum` or without a
`_count` (§ 14.1) MUST be written without that line, in both formats, and
MUST NOT be given a sum or a count of 0. A histogram's `+Inf` bucket MUST be
written with the count the target gave that bucket, and its `_count` with
the count the target gave that, also when the two differ; a histogram read
with a `_count` and no `+Inf` bucket MUST be given the bucket with that
count, and one read with neither MUST be written with neither. In
OpenMetrics, where a histogram has both
or neither, a histogram family with a series that has one of the two MUST be
written as `unknown` families under its sample names, as a family that gives
way is, and an `unknown` family that would hold no line MUST NOT be written.
A histogram's buckets MUST be written in ascending order of their upper
bounds, with a bound that is not a number first and `+Inf` last, and a
summary's quantiles in ascending order, in both formats, in whatever order
they were read; the metric set an answer is written from MUST NOT be
reordered.
The text format MUST pass on every value a target wrote for a series (§ 14.1).
The OpenMetrics answer MUST be one a strict OpenMetrics parser accepts, so a
family with a series whose values OpenMetrics does not allow its type MUST be
written as `unknown` families under its sample names, as a family that gives
way is, with the same series and values as the text format. Those series are:
a counter that is NaN or negative; a histogram whose `_sum` is NaN or
negative, that has a `_sum` and a bucket with a negative bound, that has a
bound that is NaN, whose cumulative bucket counts fall from one bound to the
next higher or exceed its `+Inf` bucket's, whose `+Inf` bucket and `_count`
differ, or that has neither of the two (§ 14.1); and a summary whose `_sum` is
NaN or negative, with a quantile
that is NaN or outside 0 to 1, or with a negative value for a quantile. A
counter or a `_sum` of `+Inf`, a quantile's value of NaN, and any value of a
gauge or an untyped series MUST NOT cost a family its type, and a family with
no such series MUST keep its type unless it gives way over a name. The choice
MUST be made for the family, since a family has one type, and before any
family gives way over a name, so that the rule on claimed names holds for
what is written.

### 21.1 UTF-8 names

The exposition MUST use classic names only: `[a-zA-Z_:][a-zA-Z0-9_:]*` for
metrics and `[a-zA-Z_][a-zA-Z0-9_]*` for labels. A collector's
`name_escaping` MUST say what happens to any other name a transform produces,
including one a `prometheus` transform passes through from a UTF-8 target and
one a Python script or pre-script emits:

- `fail`, the default, MUST fail the scrape in validation, and when the name
  is valid UTF-8 the error MUST name it and say to set `name_escaping`, for a
  metric name and a label name alike;
- `underscores` MUST replace every character a classic name may not have at
  its position, including a leading digit and, in a label name, `:`, with `_`;
- `values` MUST write `U__`, then the name with `_` doubled, every character
  classic at its position kept, every other rune as `_`, its code point in
  lower-case hexadecimal and `_`, and invalid UTF-8 as `_FFFD_` — Prometheus's
  value encoding escaping, which it can reverse.

Any other value MUST be rejected when the configuration loads. A classic name
MUST NOT be changed by either scheme, and label values MUST NOT be escaped.
Escaping MUST happen after `metrics_prefix` is joined, in the one place every
transform's output passes through, so it applies to probes, static targets
and each file of a directory alike. A label map shared among metrics MUST NOT
be changed in place. Two labels of one series that escape to one name MUST fail
the scrape naming both, and two metrics that do so are duplicate series and
fail as such.

A name a collector's configuration writes — a rule's `name` and the `name`
of each of its labels, under every transform, a key of `transform.labels`,
and what `transform.rename` and `transform.rename_labels` rename to — MUST
be held, when the configuration loads, to what a scrape of that collector
holds a name to, and MUST be exported by the same escaping, in that one
place, and by no second one:

- under `fail`, written, left out or written `""`, a name that is not
  classic could only fail every scrape, so it MUST be refused at load, in
  the words it was, and where it is valid UTF-8 and not blanks alone the
  error MUST say as the scrape's does that `name_escaping` `underscores` or
  `values` would export it escaped;
- under `underscores` and `values` it MUST load, whatever characters it
  has, and its series MUST be exported under the name that scheme gives a
  target's same name, with `metrics_prefix` joined first, in the text
  format, in OpenMetrics and over OTLP alike.

A `prometheus` rule's `name` without an `expression` MUST match the
target's metric of that name as it is written, whole, and a `python` rule's
`name` and its labels' names the script's series and labels as the script
gives them. What a rule says of its series MUST find them by the names as
written, before they are escaped: a label's `value_map`, `truncate`,
`remove_labels` and what `rename_labels` renames. A log line, a probe's
error and the rule's failures among the self-metrics MUST name a rule as it
is written; a debug probe's lists of series and of the rules that gave none
(§ 42.17) are of exported names.

Whatever `name_escaping` is: a name of nothing but blanks MUST be refused,
being neither the key left out nor a name; a metric name MUST NOT be
exported beginning with `__` and a label name MUST NOT either, so a name
written so MUST be refused as it was, and one that `underscores` would
export so, such as `1_x`, MUST be refused under `underscores`, the error
naming what it is exported as, and taken under `values`; and a static
target's `labels` (§ 42.14) MUST be classic names, being added to what the
collector exported after its names were escaped. A rule's label or a key of
`transform.labels` that `transform.rename_labels` renames or
`transform.remove_labels` removes is an exception to all of this but the
blanks: the scrape renames and removes before it escapes the names and
validates the series, so such a name is never exported, and it MUST load
whatever its characters, `__tmp`, and `a.b` under `fail`, alike, while the
name a rename gives MUST be held to everything above; one that neither takes
off MUST be refused in the words it was. A rule's name, of every transform,
`prometheus` and `python` rules among them, exported longer than
`limits.max_metric_name_length` MUST be refused at load, since every series
of it would fail validation: the name as it is exported, after
`metrics_prefix` and escaped by `name_escaping`, measured as the scrape
measures it, a histogram's or a summary's family name and not the names of
its samples. The error MUST name the collector, the rule, the length and
the limit, and say to shorten the name or raise the limit; with a
`metrics_prefix` (§ 6) it MUST quote the name as it is exported, in the
words it always had. A `prometheus` rule without a `name` has no name to
measure. Two rules whose names differ
as written and are one name once escaped are not the same rule to the load
(§ 18.1), which compares what is written: they MUST load, and a scrape that
has both fails as it does of two such names of a target.

#### 21.1a `metric_name_validation_scheme`

A collector's `metric_name_validation_scheme` MUST be `legacy`, the default
when it is left out or written `""`, or `utf8`, Prometheus's names; any other
value MUST be refused when the configuration loads, naming the two. Under
`legacy` everything above holds. Under `utf8`:

- a metric or label name that is valid UTF-8 and not blanks alone MUST be
  taken, at load and at every scrape, under `name_escaping` `fail` too,
  which then fails nothing; `name_escaping` `underscores` and `values` MUST
  still escape what they escape, in the one place above, before the answer
  is written. The `__` prefix, the length limits and the blanks MUST be
  held to as under `legacy`;
- a name that is not classic MUST be kept as it is until an answer is
  written, and every answer that has one — `/probe`, the static targets
  endpoint — MUST write it as the media range it answers (§ 21) asks by its
  `escaping` parameter: `allow-utf-8` in Prometheus 3's quoted syntax, a
  metric's name first inside the braces as `{"name",…}`, a label's as
  `"name"="value"`, and a family's in HELP and TYPE as `"name"`, with `\`,
  `"` and newline escaped as in a label value, in the text format and in
  OpenMetrics alike; `underscores` as `underscores` above; `values` as
  `values` above; and `dots` with `.` as `_dot_`, `_` as `__` and every
  other character a classic name may not have at its position as `__`. A
  range without the parameter, or with another value, MUST be answered as
  `underscores`, Prometheus's default. A classic name MUST NOT be escaped
  under any of them, `dots` included, label values MUST NOT be, and a
  histogram's or summary's family name MUST be escaped before its `_bucket`,
  `_sum` and `_count` are joined, as Prometheus's Go client escapes a
  family. Two labels of one series that one escaping makes one name MUST be
  written as one label, the same in every answer: with the value of the one
  whose name is classic, or, of two escaped, of the first in order;
- such an answer's `Content-Type` MUST end with `; escaping=` and the
  escaping it was written with. An answer of classic names alone MUST be
  written, and typed, as under `legacy`, without the parameter. The static
  targets endpoint MUST tell the two apart by the names its stored results
  hold, not by the configuration in force: a result scraped under `utf8`
  and kept through a reload that takes its collector to `legacy` MUST be
  written as the scraper asks until the target is scraped again.

A debug report's text of an answer (§ 42.17) MUST write a name that is not
classic quoted, as it is. The OTLP export (§ 42.1) MUST send such a name as
it is, OTLP names being free text.

---

# 22. Exporter self-metrics

Expose exporter health metrics on the self-metrics path (§ 42), `/self-metrics` by default.

At minimum:

```text
http_exporter_scrapes_total
http_exporter_scrape_success_total
http_exporter_scrape_duration_seconds
http_exporter_probe_duration_seconds
http_exporter_scrape_http_status_code
http_exporter_scrape_grpc_status_code
http_exporter_scrape_response_bytes

http_exporter_decode_success_total
http_exporter_parse_errors_total

http_exporter_transform_errors_total
http_exporter_missing_keys_total

http_exporter_script_errors_total
http_exporter_script_duration_seconds

http_exporter_metrics_emitted_total
http_exporter_invalid_utf8_total
http_exporter_decoder_series_left_out_total
http_exporter_decoder_lines_skipped_total
http_exporter_pages_fetched_total
http_exporter_pagination_max_pages_reached_total
http_exporter_series_limit_exceeded_total

http_exporter_logins_total
http_exporter_login_failures_total

http_exporter_rate_limit_waits_total
http_exporter_rate_limit_refused_total
http_exporter_host_rate_limit_waits_total
http_exporter_host_rate_limit_refused_total

http_exporter_cache_hits_total
http_exporter_cache_misses_total
http_exporter_cache_entries
http_exporter_cache_stale_served_total

http_exporter_backoff_skipped_total
http_exporter_targets_backing_off

http_exporter_probes_coalesced_total
http_exporter_probes_in_flight
http_exporter_probes_rejected_total
http_exporter_probes_rejected_exporter_limit_total
http_exporter_targets_refused_total
http_exporter_sshfile_connections
http_exporter_sshfile_connects_total
http_exporter_sshfile_limit_waits_total
http_exporter_sshfile_limit_failures_total
http_exporter_sshexec_connections
http_exporter_sshexec_connects_total
http_exporter_sshexec_limit_waits_total
http_exporter_sshexec_limit_failures_total
http_exporter_sql_connections
http_exporter_sql_idle_connections
http_exporter_sql_targets
http_exporter_sql_connects_total
http_exporter_sql_keepalive_failures_total
http_exporter_sql_limit_waits_total
http_exporter_sql_limit_failures_total
http_exporter_sql_rows
http_exporter_trips_in_flight
http_exporter_trips_max_concurrent
http_exporter_static_targets_scrape_seconds_total
http_exporter_probes_seconds_total
http_exporter_answer_bytes_total
http_exporter_answer_sent_bytes_total

http_exporter_build_info
http_exporter_collector_config_valid
http_exporter_static_targets
http_exporter_static_targets_exported_via_otlp

http_exporter_config_last_reload_successful
http_exporter_config_last_reload_success_timestamp_seconds
http_exporter_config_reloads_total

http_exporter_rule_failures_total

http_exporter_otlp_exports_total
http_exporter_otlp_export_retries_total
http_exporter_otlp_points_dropped_total
http_exporter_otlp_export_duration_seconds
http_exporter_otlp_last_export_success_timestamp_seconds

http_exporter_debug_runs_total
```

Labels should include `collector` and, where appropriate, `target`.

`http_exporter_scrape_http_status_code` MUST be the status of the most recent
response, and `0` after a fetch that got none. `http_exporter_scrape_grpc_status_code`
MUST exist only for `grpc` collectors, so an exporter without them has no
series of it: the status code of the most recent call, `0` for `OK`, and `-1`
before the first call or after a scrape that made none.
`http_exporter_sshfile_connections`, a gauge, and
`http_exporter_sshfile_connects_total`, a counter, MUST exist only for
`sshfile` collectors, labelled `collector`, with no per-request series: the
connections the collector's scrapes use that are open now, a connection
collectors share counted for each, and the connections its scrapes have made
since the exporter started, every one made again included; a connection
whose making was cancelled — at the exporter's shutdown, at a reload, at
`max_open_connections`, or once no scrape waited for one the cache had
forgotten — or whose collector a reload removed while it was made, MUST NOT
be counted, and one the cache forgot while a scrape waited for it, which
that scrape then reads on, MUST be counted. `http_exporter_sshfile_limit_waits_total` and
`http_exporter_sshfile_limit_failures_total`, counters, MUST exist only for
`sshfile` collectors, labelled `collector`, with no per-request series: the
scrapes that waited for room at the collector's `max_open_connections`, and
those that failed there, `0` for a collector without the limit. The four
MUST be exported over OTLP as every other self-metric, and the counts MUST
start at 0 at the exporter's shutdown and be forgotten with a collector a
reload removes. `http_exporter_sshexec_connections`,
`http_exporter_sshexec_connects_total`,
`http_exporter_sshexec_limit_waits_total` and
`http_exporter_sshexec_limit_failures_total` MUST be the same of `sshexec`
collectors, and exist only for them.
`http_exporter_sql_connections`, `http_exporter_sql_idle_connections` and
`http_exporter_sql_targets`, gauges, and `http_exporter_sql_connects_total`,
`http_exporter_sql_keepalive_failures_total`,
`http_exporter_sql_limit_waits_total` and
`http_exporter_sql_limit_failures_total`, counters, MUST exist only for `sql`
collectors, labelled `collector`, with no per-request series: the
connections the collector's pools have open, those of them idle, the targets
it has pools open for (each server or account once, whatever databases and
accounts it reached there), the connections they made, its failed keepalive pings, and its scrapes that
waited at `max_per_target` and that failed there or at `max_targets`; and
`http_exporter_sql_rows`, a gauge, the rows its last successful statement
returned, `0` before one. They MUST be exported over OTLP as every other
self-metric, and the counts MUST start at 0 at the exporter's shutdown and be
forgotten with a collector a reload removes. A `sql` collector's refused
targets count in `http_exporter_targets_refused_total`, and so do an `s3file`
collector's refused buckets, a `dns` collector's refused names, and a
`tlscert`, `redis`, `tcp` or `ldap` collector's refused servers; each of these types MUST
have its series there, `0` before a refusal.
`http_exporter_pages_fetched_total` and
`http_exporter_pagination_max_pages_reached_total`, counters, MUST exist only
for `http` collectors, `0` for one without `request.pagination`, per
collector and per request in verbose mode: the pages `request.pagination`
fetched in trips that ended with an answer, the first of each included, and
the trips that stopped at `max_pages` with another page to come (§ 5.1,
`http`). `http_exporter_logins_total` and
`http_exporter_login_failures_total`, counters, MUST exist only for `http`
and `graphite` collectors, `0` for one without `request.login`, per
collector and with no per-request series: the logins the collector's login,
or a static target's own, made, and those of them that failed (§ 42.6a). The
counts MUST start at 0 at the exporter's shutdown and be forgotten with a
collector a reload removes. `http_exporter_rate_limit_waits_total` and
`http_exporter_rate_limit_refused_total`, counters, MUST exist only for
`http` and `graphite` collectors, `0` for one without `request.rate_limit`,
per collector and with no per-request series, never labelled by target or
origin: the requests the collector's rate limit, or a static target's own,
made wait for their turn, and those it refused for a turn past their
deadline (§ 5.1, `http`). The counts MUST start at 0 at the exporter's
shutdown and be forgotten with a collector a reload removes.
`http_exporter_host_rate_limit_waits_total` and
`http_exporter_host_rate_limit_refused_total`, counters, MUST be
exporter-wide, labelled `rule` with the name of an entry of `rate_limits`
and no `collector` label, a series of each for every entry in force from 0:
the requests of every collector that the entry made wait for their turn,
and those it refused for a turn past their deadline, a debug run's not
counted (§ 5.1, `http`, Rate limits per host). They MUST be exported over
OTLP as every other self-metric. An entry's counts MUST be kept across a
reload that keeps an entry of its name, changed or not, forgotten with an
entry a reload removes, and start at 0 at the exporter's shutdown; their
created time MUST be the exporter's start for an entry it started with, and
when the reload put it in force for one a reload added.
`http_exporter_backoff_skipped_total`, a counter, per collector and per
request in verbose mode, and `http_exporter_targets_backing_off`, a gauge,
per collector with no per-request series, MUST exist for every collector,
`0` for one without `failure_backoff`: the probes and static target scrapes
that failed without contacting their target because the collector was
backing off from it, and the targets it keeps its trips from now (§ 42.13c).
A debug run MUST NOT be counted in either.
`http_exporter_probe_duration_seconds`, a histogram, MUST be published
whether or not verbose mode is on, labelled `collector` and `kind` and by
nothing else, never by target: `kind="probe"` MUST observe every probe of
the collector, one answered from the response cache or by sharing another
probe's request (§ 42.13a) included, for the duration
`http_exporter_scrape_duration_seconds` takes of it, to the answer being
ready; `kind="static"` every static target scrape of the collector, for the
same duration its gauge takes. Every configured collector MUST have its
`kind="probe"` series, empty until its first probe, and its
`kind="static"` series exactly while a static target of the file in force
names it. A debug run MUST NOT be observed. The buckets, the exponential
buckets, the native histogram of the protobuf answer and the exemplars
MUST be those of the scrape-time histogram (§ 22.1a), the exemplar of a
probe or a scrape whose trace was exported, and its creation time that of
the collector's counters (§ 22.1b).
`http_exporter_answer_bytes_total` and
`http_exporter_answer_sent_bytes_total`, counters, MUST be exporter-wide,
with no `collector` label, labelled `endpoint` with `probe`, `self_metrics`
and `static_targets`, the endpoints whose answers are compressed (§ 23), a
series of each for every endpoint from 0 from the exporter's start, with or
without static targets, and its created time the exporter's start. The
first MUST count the bytes the endpoint's handler wrote, before
compression, and the second those written on to the client, after it, gzip's
header and trailer included; an answer sent uncompressed — at
`--web.gzip-level` 0, to a client that does not accept gzip, to `HEAD`, or a
`204` or `304` — MUST add the same to both. Every answer of the endpoints
MUST be counted, errors and refusals included, and a write that fails or is
cut short MUST count what the writer it was written to took. They MUST be
counted in the middleware that compresses the answers, with no lock taken
for a write, and an answer added to them once, when it is done, so that an
answer of the self-metrics never holds its own bytes and the next answer
holds them exactly. Counting an answer sent uncompressed MUST allocate
nothing: the writer that counts it MUST be reused from answer to answer, not
made for each, and a test MUST hold an uncompressed answer through the
middleware to what it allocates without the counting. They MUST be exported over OTLP as every other
self-metric.

Names MUST follow the Prometheus conventions: a counter's name MUST end in
`_total`, and nothing else's may. Every family's name, type, help and value
MUST come from one definition, from which one set of self-metrics is built and
both rendered as text on the self-metrics path and exported over
OTLP, so a family cannot be one type in one place and another type elsewhere.
In the text, each family MUST be one contiguous block — its `HELP` and `TYPE`
once, then every series of it, the per-collector ones followed by the
per-request ones of verbose mode (§ 22.1) — as the exposition format requires.
A test MUST check each of these properties on a rendered exposition.

### 22.0b Configuration reload status

A rejected reload leaves the last valid configuration in force (§ 24), which
changes nothing visible while the change somebody made is not running. The
exporter MUST therefore publish, under the names Prometheus uses for the same
purpose:

```text
http_exporter_config_last_reload_successful{file}                 gauge
http_exporter_config_last_reload_success_timestamp_seconds{file}  gauge
http_exporter_config_reloads_total{file, result}                  counter
```

`file` MUST be `config` for the configuration file with its collector files, and
`static_targets` for the static target document, reported only when one is
configured. Loading at startup MUST count as a successful load, setting the
first two; `http_exporter_config_reloads_total` MUST count only reloads after
startup, with `result` `success` or `failure`, both published from the start.
A rejected reload MUST set `http_exporter_config_last_reload_successful` to 0
and leave the timestamp at the last success, and a reload of one file MUST NOT
change the other's series. The watch's try at every tick of a file refused
because a file it reads could not be read (§ 24.1) MUST count a failure only
when it is refused otherwise than the last time.

`http_exporter_collector_config_valid` MUST read 1 for every collector of the
configuration in force: a configuration that fails validation is never loaded,
so it has no collectors to read 0 for, and a rejected reload leaves the previous
collectors at 1. Its `HELP` MUST say so rather than suggest the value can report
an invalid collector; the reload status above is where a rejected change shows.

### 22.0c OTLP export status

OTLP export is best-effort (§ 42.1), which MUST NOT mean silent. While OTLP
export is enabled, and only then, the exporter MUST publish:

```text
http_exporter_otlp_exports_total{result}                   counter
http_exporter_otlp_export_retries_total                    counter
http_exporter_otlp_points_dropped_total                    counter
http_exporter_otlp_export_duration_seconds                 gauge
http_exporter_otlp_last_export_success_timestamp_seconds   gauge
```

An export is one delivery of everything pending, its retries included,
however many requests it is sent in (§ 42.1a):
`http_exporter_otlp_exports_total` MUST count it once, with `result`
`success` when the endpoint accepted every request and `failure` when it did
not, both published from the start. Retries of every request MUST be counted
in `http_exporter_otlp_export_retries_total`. Data points dropped because the
endpoint refused them, because an export it accepted rejected them
(`partialSuccess`, § 42.1a), because they were the oldest waiting past
`otlp.max_pending_points` (§ 42.1a), or because the last export at shutdown
failed (§ 42.1a), MUST be counted in
`http_exporter_otlp_points_dropped_total`; data points kept for the next
export MUST NOT be. The duration MUST be that of the most recent export,
every request and retry included, and the timestamp that of the last export that got through,
0 before the first. An export cut short by shutdown MUST NOT be counted, since
the last export delivers its data. These families MUST be part of the one
self-metric set, so they are also exported over OTLP.

While the export of the exporter's own log is on as well (`otlp.logs`,
§ 42.1b), and only then, the exporter MUST publish:

```text
http_exporter_otlp_log_exports_total{result}     counter
http_exporter_otlp_log_records_sent_total        counter
http_exporter_otlp_log_records_dropped_total     counter
```

A log export is one delivery of the records waiting, however many requests
it is sent in, counted once by `result` as an export of metrics is; an
interval with no record waiting MUST make none. The records the endpoint
accepted MUST be counted as sent, less those it rejected in a
`partialSuccess`. A record dropped because it found
`otlp.logs.max_pending_records` waiting, because it was the oldest past it
after a failed export, because the endpoint refused its request or rejected
it, or because the last log export at shutdown failed, MUST be counted as
dropped; records kept for the next export MUST NOT be.

While the export of traces is on as well (`otlp.traces`, § 42.1c), and only
then, the exporter MUST publish:

```text
http_exporter_otlp_trace_exports_total{result}   counter
http_exporter_otlp_spans_sent_total              counter
http_exporter_otlp_spans_dropped_total           counter
http_exporter_otlp_traces_kept_total{reason}     counter
http_exporter_otlp_traces_dropped_total{reason}  counter
```

counted as the log export's are: a span of a trace that found no room for
all of its spans under `otlp.traces.max_pending_spans`, those of the oldest
traces dropped whole past it after a failed export or a reload that lowers
it, those of a request the endpoint refused or rejected,
and those of a failed last trace export at shutdown MUST be counted as
dropped; spans kept for the next export MUST NOT be. The traces counters
MUST count whole traces, not spans: `kept` each trace the sampling rules
kept and the rate cap let through to the queue, by `reason` `slow`, `error`,
`parent` or `ratio`; `dropped` each trace dropped whole, by `reason`
`not_sampled` (kept by no rule), `rate` (over `otlp.traces.max_per_second`)
or `queue` (kept, and with no room under `otlp.traces.max_pending_spans`
when queued or put back by a failed export). Every reason MUST be served
while the export is on, 0 until it counts one.

### 22.0d Build information

The exporter MUST publish `http_exporter_build_info`, a gauge of value 1 whose
labels are `version`, `revision`, `goversion`, `request_types`, the built
request types joined with commas, and `sql_drivers`, the `sql` type's built
drivers joined with commas, left out in a build without the type, whose
value would be empty. The version MUST be the one set at build time
with `-ldflags "-X main.version=…"` when set, else the module version Go stamps
into the binary, `(devel)` when there is none. The revision MUST be the git
commit Go stamps, with `-modified` appended when the checkout had changes, and
`unknown` when the build was not from git. `--version` MUST print the same
values on one line to stdout, the sql drivers left out as the label is, and
exit 0 (§ 30), and the startup log line
MUST carry the version and revision.

Avoid unbounded label values on exporter self-metrics.

Each family MUST be published with its own `HELP` text describing what that
family counts or measures. A shared placeholder such as "Exporter self metric"
MUST NOT be used: `HELP` is what a reader sees in a metric browser or in the raw
exposition, and repeating one line across every family documents nothing while
appearing to. The descriptions MUST come from a single source, so the text
exposition and the self-metrics delivered over OTLP carry the same text, and a
family exposed without a description MUST fail the repository's tests rather
than reach an operator undocumented.

The self-metrics path MUST NOT require a target query parameter. It MUST
answer `GET` and `HEAD`, as `/probe` and the static targets path do; any other
method MUST be answered `405 Method Not Allowed` with `Allow: GET, HEAD`,
before anything is gathered. Authentication, where configured (§ 42.5), is
checked first.

### 22.0a Resource metrics

The exporter MUST support publishing the standard `go_` and `process_` series
describing its own CPU and memory, configured under
`web.self_metrics.resource_metrics_enabled` and defaulting to true; false
MUST leave every one of them out.

The names, types, help texts and semantics MUST match what
`prometheus/client_golang` publishes, so an existing Go dashboard or alert works
against this exporter unchanged. That is the only reason to use the prefix at
all; a series named `go_goroutines` that means something else is worse than no
series.

Collecting them MUST NOT stop the world: the exporter MUST NOT call
`runtime.ReadMemStats` outside its tests. The memory series MUST be read from
`runtime/metrics`, every source in one `metrics.Read` per scrape, and computed
from the memory classes as client_golang's Go collector computes the
`runtime.MemStats` fields for Go 1.17 and later; `go_memstats_last_gc_time_seconds`
and `go_gc_duration_seconds` MUST come from `runtime/debug.ReadGCStats`. Because
nothing pauses the program they are on by default. The samples and the pause
history MUST be reused from one scrape to the next, so that overlapping scrapes
neither race on them nor allocate them again.

The runtime series MUST be available on every platform the exporter builds for.
Every `runtime/metrics` source MUST be requested by name and checked against the
descriptions the runtime gives (`metrics.All`); a series MUST be omitted when the
Go release in use does not describe one of its sources, so the exporter
publishes what its runtime actually offers rather than a fixed list a toolchain
upgrade could invalidate, and never a sum with a part missing.

The `process_` series MUST reflect the operating system's view of the process
and MUST be omitted where that view is unavailable. They MUST NOT be synthesised
from runtime accounting: `process_cpu_seconds_total` is CPU the process
consumed, while the runtime's total CPU class is GOMAXPROCS multiplied by wall
time and includes idle. Publishing one under the other's name is wrong in a way
that only appears when somebody trusts the graph.

Where a standard series cannot be produced faithfully it MUST be omitted rather
than approximated. `go_gc_duration_seconds` MUST be published as the summary
client_golang publishes: the quantiles 0, 0.25, 0.5, 0.75 and 1 of the recent
pauses, the total pause as its sum and the number of collections as its count.
It MUST NOT be published as two counters, which would give the family a type
client_golang does not give it.

`process_start_time_seconds` MUST be the same at every scrape of one process:
it MUST be worked out from the boot time the system reports and the process's
start since boot, not from the uptime and the time now, which would move it by
a fraction of a second between scrapes and read as a restart.

Publishing these alongside the exporter's own self-metrics MUST NOT declare any
metric family twice, and turning the setting off through a reload MUST drop the
series rather than leave them exposed.

### 22.1 Verbose per-request self-metrics

The exporter MUST support an opt-in verbose mode that republishes its own
metrics broken down by the individual request that produced them. Verbose mode
MUST NOT remove or alter the per-collector series: the labelled series are
published in addition to them, so a dashboard built on the per-collector view
keeps working when verbose mode is switched on.

Every self-metric family of section 22 MUST be republished with the labels
`collector`, `http_method` and `url`, except the families that describe the
collector rather than any request: `http_exporter_cache_entries`, which counts
what a collector's response cache holds, `http_exporter_probes_in_flight`,
which counts its trips in progress, and `http_exporter_targets_backing_off`,
which counts the targets its `failure_backoff` keeps its trips from
(§ 42.13c). The two labels are the request type's (§ 5.1): for `http` the
method and the URL described below, for `localfile` `READ` and the file's
`file://` URL.

In addition, verbose mode MUST expose

```text
http_exporter_request_last_scrape_timestamp_seconds
```

carrying the same three labels: the moment the request was last collected, which
has no per-collector equivalent.

Because both shapes of a family are exposed at once, a query that selects a
family without constraining the labels matches the collector total and its
requests together. The documentation MUST state that `http_method=""` selects
the per-collector series and `http_method!=""` the per-request ones.

A counter MUST be raised on the collector and on the request through the same
path, so the two views cannot drift: the collector's total MUST equal the sum of
its requests' values for every counter, but for the probes and trips that start
no tracked combination, below, and the repository's tests MUST check this.

Verbose mode MUST be configured in the exporter configuration under
`web.self_metrics.verbose` and MUST default to false. Because it is
configuration rather than a process flag, a reload MUST be able to turn it on
and off; turning it off MUST drop the per-request series rather than leaving
stale ones exposed. No labelled series, and none of the three indicators above
or below, may appear while verbose mode is off.

The `url` label MUST carry only the scheme, host and path of the resolved
request. Userinfo credentials and the entire query string MUST be removed: a
collector's `request.query` or a probe parameter may carry a token or a tenant
identifier, and a metric label is persisted by Prometheus and passed to anything
federating from it. The label MUST be derived from the same resolution the real
request uses, so a label can never describe a URL that was not the one fetched,
except that a path parameter (§ 42.10a) MUST appear as its placeholder rather
than its value, for the same reason the query string is removed.
The request parameters MUST therefore be resolved before any counter is raised,
so a probe cannot be counted against the collector alone.

A request URL is an unbounded label value, which section 22 warns against, and
each combination now carries a whole metric family, so the number of tracked
combinations MUST be capped. The limit MUST be 1000 combinations. A request
already tracked MUST keep updating after the limit is reached; only new
combinations are refused. Reaching the limit MUST NOT be silent: the exporter
MUST expose

```text
http_exporter_request_series_capped
```

which reads 1 once the limit has been reached and 0 otherwise, so an operator
can alert on truncation without having to test for an absent series. Alongside
it the exporter MUST expose

```text
http_exporter_request_series_tracked
```

the number of combinations currently tracked, so a verbose exporter that has not
been asked for anything yet reports an empty set rather than leaving the cap
indicator alone with nothing to explain it.

The slots MUST NOT be taken by requests the exporter never made: a probe whose
target the collector's `request.allowed_targets` or `denied_targets` refused
(§ 26.1) MUST NOT start a tracked combination, though it MUST keep updating one
already tracked, and it is counted on the collector as always. The same MUST
hold for a probe whose caller went away before it was answered, whether or not
the collector shares identical probes, and for a trip cancelled because every
probe waiting for it went away (§ 42.13a): neither got the target policy's
verdict. A trip that ends otherwise and was not refused MUST be counted on its
combination, starting it if it is not tracked yet, whichever probe started the
trip and whichever probes went away before its answer: what a trip shared by
identical probes counted — its cache miss, the target's status and bytes, its
decode, transform, validation and error counters, the series emitted, the time
of the scrape — MUST be on the combination the probes it answered are counted
on, once, and MUST NOT be kept with, and forgotten with, the probe that
started it. A collector's total is therefore the sum of its tracked
combinations' and of the probes and trips above that started none; with a
trip that answered a probe, the two differ only by the probes that went away,
in `http_exporter_scrapes_total`. A combination no
probe or static target scrape has asked for in the last hour MUST be dropped,
so a target probed once, or no longer probed, gives its slot back and does not
leave a series with a timestamp that never moves; the combinations of the
configured static targets MUST NOT expire, and MUST be dropped as soon as a
reload removes their target or changes its URL or method. Dropping
combinations MUST clear `http_exporter_request_series_capped` once there is
room again.

A metric family may be described only once in an exposition. The labelled series
MUST therefore join the family the per-collector block has already declared,
without a second `HELP` or `TYPE` line; only the families verbose mode
introduces may declare their own.

Static targets MUST be recorded the same way as probe requests. A static
target's request is fully described by the configuration, so its series MUST
exist from the first scrape of the self-metrics endpoint rather than only after
the target has been collected once, and MUST survive a reload that adds or keeps
the target. A request driven by `/probe` cannot be published in advance, because
its URL comes from the probe's own `target` parameter; it MUST appear the first
time that probe is served.

A request that is known but has not been scraped MUST report zero counters and a
timestamp of zero rather than the zero instant, which would otherwise read as a
scrape in 1970.

A response served from the collector response cache MUST NOT move the request's
last-scrape timestamp. No HTTP request is made, so the status, duration and
timestamp MUST keep describing the scrape that filled the cache; the cache hit
itself MUST still be counted, on the collector and on the request alike. This
applies to both `/probe` and static targets.

### 22.1a Verbose collector metrics

Verbose mode MUST additionally publish the following per-collector families.
None of them may appear while verbose mode is off, and every label value MUST
come from a fixed set, so the number of series is bounded by the number of
configured collectors.

```text
http_exporter_collector_scrape_duration_seconds       histogram {collector}
http_exporter_python_workers                          gauge     {collector, state}
http_exporter_python_worker_starts_total              counter   {collector}
http_exporter_python_worker_start_failures_total      counter   {collector}
http_exporter_python_worker_stops_total               counter   {collector, reason}
http_exporter_python_runs_total                       counter   {collector, outcome}
http_exporter_python_pool_workers                     gauge     {state}
http_exporter_python_pool_worker_starts_total         counter
http_exporter_python_pool_worker_start_failures_total counter
http_exporter_python_pool_worker_stops_total          counter   {reason}
http_exporter_python_pool_runs_total                  counter   {outcome}
http_exporter_python_pool_runs_waiting                gauge
http_exporter_trips_waiting                           gauge
http_exporter_answer_gzip_level                       gauge
```

`http_exporter_answer_gzip_level` MUST be `--web.gzip-level`, from `0` to
`9`, without a label, and MUST NOT be published while verbose mode is off.

`http_exporter_trips_waiting` MUST be the static target scrapes waiting for a
trip slot, because their collector is at `max_concurrent_probes` or the
exporter at `--probe.max-concurrent` (a probe never waits, § 42.13b), and
`http_exporter_python_pool_runs_waiting` the script runs waiting for a worker
under `--python.max-workers`, `0` without that limit; neither carries a
`collector` label.

`http_exporter_collector_scrape_duration_seconds` MUST observe the duration of
every trip to the target, from sending the request to having validated metrics,
for `/probe` and static targets alike. A probe answered from the response
cache, or by sharing another probe's request (§ 42.13a), made no trip and MUST
NOT be observed. The buckets MUST be fixed at 0.005, 0.01, 0.025, 0.05, 0.1,
0.25, 0.5, 1, 2.5, 5, 10, 30 and 60 seconds, plus `+Inf`. Every configured
collector MUST have a histogram, empty until its first trip. Durations MUST only
be recorded while verbose mode is on, so turning it on does not publish a
history nobody asked to be kept. The same durations MUST also be kept in
exponential buckets (§ 42.1, `otlp.exponential_histograms`), which the
protobuf answer below writes as native histograms.

`web.self_metrics.native_histograms`, a boolean that MUST default to true,
MUST make the self-metrics endpoint answer in Prometheus's delimited protobuf
format, as `application/vnd.google.protobuf;
proto=io.prometheus.client.MetricFamily; encoding=delimited` with `nosniff`
and `Vary: Accept`, a request whose `Accept` gives that media type, with those
two parameters, a higher quality than any type the text format or OpenMetrics
answers (§ 21); a tie, or the setting false, MUST get the text formats as
before. Only the self-metrics endpoint MAY answer protobuf: `/probe` and the
static targets endpoint MUST NOT, since what they pass on has classic buckets
alone. The answer MUST be one length-delimited
`io.prometheus.client.MetricFamily` per family of the text answer, with its
name, help, type and every series, labels in order of their names; a series'
timestamp in `timestamp_ms`; and, with `web.self_metrics.created_timestamps`
and only then, a counter's, histogram's or summary's creation time in its
`created_timestamp`. A histogram MUST carry its classic buckets without the
`+Inf` one, which `sample_count` is, unless that bucket has an exemplar:
it MUST then be written last, as Prometheus's Go client writes it, with
`upper_bound` `+Inf`, `cumulative_count` the `sample_count` and the
exemplar. It MUST carry its `sample_count` and `sample_sum`, and, for the scrape-time
histogram and `http_exporter_probe_duration_seconds` (§ 22), its native
buckets too: `schema` the recorder's scale lowered to
at most 3 by merging buckets, `zero_threshold` 2^-128, `zero_count`, and
`positive_span` and `positive_delta` as Prometheus defines them, bucket `i`
of OpenTelemetry being Prometheus's `i+1`. The protobuf answer MUST be
written without `google.golang.org/protobuf`, which only the `grpc` request
type may link (§ 14.1).

With the trace export on (§ 42.1c), each bucket of the scrape-time
histogram, the `+Inf` one included, MUST keep as its exemplar the latest
trip observed in it whose trace was exported, and none of a trace dropped
or not yet decided: the trace id and the probe's or the scrape's span id,
as `trace_id` and `span_id`, the trip's duration and the time it was
observed; a trace exported after a later trip's MUST NOT replace it. A
trip shared by identical probes MUST be of the trace of the probe that
started it. OpenMetrics MUST write a bucket's exemplar after its count, as
`# {trace_id="…",span_id="…"} <value> <timestamp in seconds>`; protobuf in
the classic bucket's `exemplar`, the `+Inf` one's on a `+Inf` bucket
written for it, and, the same ones in the order of their
values, in the histogram's `exemplars`; the OTLP export on the histogram's
and the exponential histogram's points, as `exemplars` with `traceId`,
`spanId`, `asDouble` and `timeUnixNano`. The Prometheus text format MUST
NOT carry any, and no other series MUST: the gauges of one probe's
duration can have none in OpenMetrics.

Its name MUST differ from `http_exporter_target_scrape_duration_seconds`, the
gauge a static target's health series carry on its endpoint and over OTLP
(§ 42.14): the two
would otherwise reach an OTLP backend as one name with two types.

The Python families MUST be published for every collector with a Python
transform or pre-script, and for no other. `state` MUST be one of `starting`,
`idle` and `busy`; `reason` one of `timeout`, `deadline`, `crash`, `output_limit`,
`cancelled`, `retired`, `surplus`, `idle`, `reload` and `evicted`; `outcome` one of `ok`,
`script_error`, `timeout`, `deadline`, `output_limit` and `failed`. `deadline` is
a run the probe's or scrape's deadline ended before `limits.script_timeout`
did, or before the worker had the request (§ 16.7). Every value of `reason`
and `outcome` MUST be published, zero included, so a rate can be taken before
the first event. The pool MUST keep these counts regardless of verbose mode,
since it maintains them anyway; only their publication depends on it.

The counts of a collector's Python workers are part of the state kept for
the collector, and MUST follow a reload as § 24.1a says of it: dropped when
a reload removes the collector, from zero for a collector added again under
the name, and kept by a collector that stays, whether or not its definition
changed. A run takes the statistics it counts in once, for the collector its
probe or scrape read, and a worker counts in those of the run that started
it, for as long as it lives: a run of a removed collector's script that is
waiting for a worker, running or ending when the statistics are dropped, one
that begins afterwards, and a worker of the removed collector that stops
afterwards, for whatever reason, MUST be counted under no collector, MUST
NOT be counted for a collector added again under the name, and MUST NOT
leave statistics under the name. The stop, for the reload, of the workers of
a collector that reload removed MUST be counted under no collector either.

A worker MUST serve only runs that count in the statistics it was started
for, though workers are kept by the collector's name and its script, and a
collector added again has both as the removed one had them. A worker of a
removed collector MUST NOT be given to a run of the collector added again
under the name, and a worker of the collector added again MUST NOT be given
to a run of the removed collector that is still under way: such a run MUST
start a worker of its own, within `--python.max-workers` as any other, whose
place under that limit it holds until it is stopped. A worker of a removed
collector MUST NOT be left idle: it MUST be stopped, with the reason
`reload`, when the collector's statistics are dropped if it is idle then,
and otherwise when its run ends, whether or not the script is in use again
by then, and whether the run began before the reload or after it, when the
script was no longer in use. So for every collector, after every event, the
workers started less the workers stopped, for every `reason` together, MUST
be the workers its `idle` and `busy` series count, and none of the three
`state` series is ever below zero.

One answer reads a collector's statistics before it reads what the
collector's Python workers counted. Where reloads between the two removed
the collector and added it again, the answer MUST NOT show what the workers
of the collector added again counted with the creation time (§ 22.1b) of the
collector it read: it MUST show that collector with its worker series at
zero.

The `http_exporter_python_pool_` families MUST report the Python execution pool
as a whole, with the same `state`, `reason` and `outcome` values. They MUST be
published whenever verbose mode is on, whether or not any collector uses Python,
so the pool's status can be monitored without knowing which collectors run
scripts. They MUST sum every collector the pool has served, including one a
reload has removed and what its runs and workers count once it is removed,
so a counter never decreases and dropping a collector's statistics changes
none of them. They MUST have names of their
own rather than being unlabelled series of the per-collector families, so that
summing a per-collector family never counts a run twice.

Each family MUST declare `HELP` and `TYPE` once, before its series, and MUST be
delivered over OTLP like the rest of the self-metrics.

---

### 22.1b Created timestamps

The exporter MUST know when each of its own counters, histograms and
summaries began to count, its creation time:

- the exporter's start, for a series that has counted since then: the
  counters of a collector of the configuration the exporter started with, its
  `http_exporter_rule_failures_total` series, its
  `http_exporter_collector_scrape_duration_seconds` and
  `http_exporter_probe_duration_seconds` histograms and the
  counters of its Python workers (§ 22.1a); the reload counters (§ 22.0b) and
  the OTLP ones (§ 22.0c); the `http_exporter_python_pool_` counters of
  § 22.1a, which the pool keeps whatever a reload does; and the `go_` and
  `process_` counters and summary (§ 22.0a).
  The exporter's start MUST be the start of its process, the very time
  `process_start_time_seconds` gives where the platform has it and the moment
  the exporter was loaded where it has not, and one value for the life of the
  process;
- the moment a collector's counters were made, for a collector a reload added,
  or removed and brought back (§ 24.1): its counters, rule failures,
  histogram and Python worker counters start from zero then;
- the start of the probe that got a request tracked, which of several probes
  of a request not yet tracked is the first to end, or the moment a static
  target's request was registered, for the per-request series of verbose mode
  (§ 22.1). A request dropped — not asked for within the hour, removed with
  its collector or its static target, or with verbose mode switched off — and
  tracked again MUST have a later time than it had, as it counts from zero
  again: when the probe that gets it tracked again began no later than a
  dropped request's creation time, the time MUST be the moment the request is
  tracked again, which is when that probe's counts become the series', and
  not the probe's start.

A series that goes on counting across a reload MUST keep its creation time,
and a creation time MUST NOT be later than anything the series has counted. A
creation time MUST NOT change while its series is tracked, so it is the same
in every answer and in every OTLP export: a probe that began before a
per-request series' creation time and ends after it adds its counts to the
series then, and MUST leave the time as it is. A trip to a target MUST be
observed in the scrape-time histogram that belongs to the counters the trip
counts in, so that a histogram holds no observation its collector's counters
do not, and none older than its creation time: a trip that counts in the
counters of a collector a reload has removed (§ 24.1a) is not observed in the
histogram of a collector brought back under the name. A histogram MUST have
its creation time in every answer it is in, the one during which a reload
added its collector included.

`web.self_metrics.created_timestamps`, a boolean that MUST default to false,
MUST make the self-metrics endpoint write the creation times in an OpenMetrics
answer: after each counter, histogram and summary series, in its family and
without a `TYPE` line of its own, one sample named as the family with
`_created`, with the series' labels and the time in seconds since the Unix
epoch — after a counter's `_total`, and after a histogram's or summary's
`_sum` and `_count`. A family written as `unknown` (§ 21) has no such sample,
which leaves out `go_memstats_alloc_bytes_total`, whose OpenMetrics family
would be named as the gauge `go_memstats_alloc_bytes` is. It is configuration,
so a reload MUST be able to turn it on and off. It is opt-in because a
Prometheus without its `created-timestamp-zero-ingestion` feature stores every
`_created` sample as one more series.

Nothing else MUST change with the setting: the text format's answer, which has
no `_created` sample, the format an `Accept` header is answered in, and every
answer of `/probe` and of the static targets endpoint, whose series are read
from targets and have no creation time, MUST be byte for byte what they are
without it. The exporter's own reader (§ 14.1) drops `_created` samples, so an
exporter probing another's self-metrics MUST give the same series with the
setting as without.

Over OTLP the creation time MUST be the start time of the series' cumulative
points (§ 42.1), whatever the setting.

# 23. Health endpoints

Implement:

```text
/
/collectors
/debug
/health
/ready
/self-metrics
/static-targets
/probe
```

Recommended behavior:

- `/`: an HTML landing page, as Prometheus exporters usually serve, naming the
  build and linking the endpoints and `/collectors`; with the lifecycle API
  on it MUST list `/-/reload` with every method the endpoint takes, `POST`
  and `PUT`, and no other. It MUST show when the last reload that put
  something in force did, and what it changed, as § 24.1a says it, and
  before the first that the exporter has not been reloaded since it
  started. It MUST link the status page (§ 42.14a), with static targets in
  force or without them. Only `/` itself MUST be
  the page, for `GET` and `HEAD`; another unknown path MUST still answer `404`,
  and another method `405`.
- `/collectors`: an HTML page listing the collectors in force, each with a form
  that probes a target through it and shows the answer in place. The form MUST
  offer a field for the target, required when the request type needs one,
  labelled and hinted as its request type names a target and shows its forms
  (§ 42.17), and disabled with the hint `none: leave it out` for a type that
  takes none; a
  timeout, 10 seconds to start with, sent as the
  `X-Prometheus-Scrape-Timeout-Seconds` header so the probe answers with its
  own error, with the page giving up a few seconds after it; for each
  parameter the collector takes, in its request or in a label value
  (§ 42.10c), required unless its placeholder has a default, which
  it shows; for each header in `request.forward_headers` that can be
  forwarded, sent as `header_<name>`; and, when `request.forward_authorization`
  is set, a bearer token or a username and password, sent to `/probe` as the
  `Authorization` header and never in the URL, and not at all when left
  blank. Credential fields MUST NOT be named form fields, so a submission
  without the page's script leaves them out. A credential the collector's configuration holds MUST be named by kind
  and its value MUST NOT appear on the page. With `--web.debug` at `edit` or
  `full`, and only then, each collector MUST link the debug page (§ 42.17)
  as `debug?collector=<name>`, and at `full` the link MUST carry the target
  entered in the form as `target`, as the form holds it when the link is
  followed: a target cleared since the link was last followed MUST NOT be
  carried; no form MUST ask `/probe` for a debug report.
- `/debug`, with `--web.debug` at `edit` or `full` only, `/debug/run`,
  which its runs post to, `/debug/collector`, which shows it a
  configured collector, and `/debug/otlp-test`, its OTLP endpoint test:
  § 42.17. With the flag, `/` and `/collectors` MUST
  link the page; without it, neither path is served (`404`) and neither page
  links it.
- Both pages, and the debug page, MUST be protected like `/probe` when the exporter's Basic
  Authentication is on (§ 42.5), since they list the collectors, MUST escape
  what they show, MUST NOT be cached, MUST NOT be framed by another site
  (`X-Frame-Options: DENY` and a `Content-Security-Policy` with
  `frame-ancestors 'none'`), since the collectors page takes credentials, and
  MUST lay out at phone width.
- Every HTML page the exporter serves MUST offer in its bar a theme picker
  of *Auto*, the default, *Light* and *Dark*. *Auto* MUST follow the
  browser's light or dark preference, and *Light* and *Dark* MUST hold
  whatever the browser prefers, `color-scheme` included, so the browser's
  own controls match. The choice MUST be kept in the browser's local
  storage and hold on every page; where the storage cannot be used, the page
  MUST still work and the choice MUST hold until the page is left. Each page
  MUST set the choice kept, as the `data-theme` attribute of its `html`
  element, from a script in its head, before it is first drawn, and a part
  of a page a script swaps in, as the status page's fragment, MUST NOT touch
  it. Without JavaScript the picker MUST be hidden and the pages MUST follow
  the browser.
- Every HTML page of the exporter and the allocator MUST show an outline in
  the accent's colour on what the keyboard focuses (`:focus-visible`); its
  text colours (`--fg`, `--muted`, `--accent`, `--bad`, `--good`) MUST read
  at 4.5:1 or more on `--bg`, `--panel` and `--code` in both themes, and a
  field's border (`--field`: a search box, a select, a text area) at 3:1 or
  more on `--bg` and `--panel`; the allocator's bar colours MUST stand out
  at 3:1 or more from both. A strip of marks MUST be told to a screen reader
  as its counts by result, in the order each first came, and its latest
  result, the marks hidden from it (`aria-hidden`); a filter's count of the
  rows it keeps MUST be a polite live region.
- The status page and the allocator's pages MUST write each moment as a
  `<time>` whose `datetime` is RFC 3339 in UTC: with its text the moment
  itself, which a page without JavaScript shows as it is, and with
  JavaScript in the browser's time, the time of day alone today and the
  date with it otherwise, its tooltip UTC and how long ago or until; with
  `class="ago"` its text how long ago, as the server wrote it, and its
  tooltip the moment in the browser's time and in UTC, after its
  `data-note`. A strip mark's tooltip MUST begin with its moment
  (`data-at`), written in the browser's time with UTC after it by the
  script. The script MUST run on the content after each refresh too, and
  MUST be one, `pagekit.LocalTimesScript`, on both.
- Every HTML page the exporter serves — `/`, `/collectors`, `/debug` and the
  status page (§ 42.14a) — MUST carry the same bar at its top, once, with
  the same style: a link back to `/` (on `/` itself, the exporter's name
  marked as the page shown), a link to each of the other pages, the debug
  page only with `--web.debug` at `edit` or `full`, the page shown marked
  rather than linked, and the documentation. Its links MUST be relative to
  the page, so they hold behind a proxy's path prefix and from the status
  page under a static targets path of several segments. A part of a page
  that a page's script swaps in, as the status page's `?format=fragment`,
  MUST NOT carry the bar.
- Every HTML page the exporter serves MUST have one icon, in its head: the
  project's logo, `docs/logo.svg`, byte for byte, as an `image/svg+xml`
  `data:` URL, which its `Content-Security-Policy` MUST let it load
  (`img-src data:`) and which needs no request. The copy the build embeds
  MUST be checked against `docs/logo.svg` by a test, so the two cannot
  drift apart.
- Every HTML page the exporter and the target allocator serve MUST write a
  duration it shows — an interval configured, a threshold, an age, how long
  something took — in Go's form with every unit that is zero left out:
  `1m`, `1m30s`, `2h` and `1h30s`, never `1m0s`, `2h0m0s` or `1h0m30s`; a
  fraction of a second MUST be kept (`1m0.5s`), one under a second MUST be
  written as Go writes it (`500ms`), and none as `0s`; an interval
  configured MUST NOT be cut to whole seconds, as an age, how long ago
  something was, may be. One helper (`pagekit.Duration`) MUST write them
  for both programs. The JSON answers
  MUST keep their values as they are: seconds as numbers, and a duration
  held as a string, as a static target's `interval`, in Go's form (`1m0s`).

- `/health`: process is alive. MUST answer `200` for as long as the process
  serves requests. Both endpoints are what `--healthcheck` asks (§ 30).
- `/ready`: the exporter should be sent probes. It MUST answer `503`, with OTLP
  export enabled and `otlp.unready_after_failures` set to N above 0, while the
  last N exports to the current endpoint failed (§ 42.1a), until one gets
  through; from a `SIGTERM` or `SIGINT` on, while the exporter shuts down
  (§ 30); in allocator mode, until a bundle is in force (§ 42.14e); `200`
  otherwise. `otlp.unready_after_failures` MUST default to 0,
  which never makes the exporter unready over OTLP: an exporter whose exports
  fail still answers probes, and a Kubernetes pod that is not ready stops
  receiving them. The failures MUST be counted per endpoint, so a reload that
  changes `otlp.endpoint` starts the count again, and a negative value, or
  one over 1000, MUST be rejected: the status page keeps when each failure
  of a run ended, up to the most the threshold may be, to say when the
  exporter became unready under any threshold (§ 42.14a). Failing exports of
  the exporter's own log (§ 42.1b) MUST NOT make it unready. A `503` body MUST name each reason on a line of its
  own starting `not ready:`, and MUST NOT include an error's text, since the
  endpoint is never authenticated and an error can quote a path, a URL or a
  line of the configuration. A rejected reload of the configuration or of the
  static target file MUST NOT make the exporter unready, for the reason OTLP
  failures do not by default: it keeps answering probes from the
  configuration in force, and every replica rejecting the same edit would
  otherwise leave a Service without endpoints. The rejection MUST stay
  visible in `http_exporter_config_last_reload_successful` (§ 22.0b), the
  reload's `ERROR` log line and the `500` of `/-/reload`.
- `/self-metrics`: exporter self-metrics by default; the path MUST be configurable (§ 42), and the self-metrics MUST be served at that one path only.
- `/static-targets`: the latest results of the static targets by default; the
  path MUST be configurable (§ 42.14a).
- `/probe`: execute a collector against a supplied target. A request without
  `collector` MUST answer `400` saying the collector parameter is required,
  with the URL's shape, and one without a target its collector's request type
  requires MUST answer `400` naming the collector and the request type. With
  `debug=true` and `--web.debug=full` it MUST answer a report of the trip
  instead (§ 42.17).

`/probe`, the self-metrics path and the static targets path MUST compress
their answer with gzip when the request's `Accept-Encoding` accepts `gzip` (or
`x-gzip`, or `*`, which `gzip` or `x-gzip` named overrides) with a non-zero quality — Prometheus asks for it on every scrape — and answer
uncompressed otherwise, and for `HEAD`. A compressed answer MUST carry
`Content-Encoding: gzip` and no `Content-Length`; each of these answers, compressed or
not, MUST carry `Vary: Accept-Encoding`. Errors are compressed like any other
answer. `/health`, `/ready` and `/-/reload` MUST NOT be compressed: their
answers are a line or two, and a load balancer checking health may not decode
gzip.

The gzip level MUST be `--web.gzip-level`, by default `1` (the fastest): `1`
to `9` are gzip's levels, and an answer at any of them MUST decompress to the
bytes of the uncompressed answer. `0` MUST answer these paths uncompressed
whatever the request accepts, without `Content-Encoding` and without
`Vary: Accept-Encoding`, since the answer then does not vary with it. Any
other value MUST be refused as a malformed command line, with exit status 2,
at startup and with `--dry-run`. The level MUST NOT change how OTLP exports
are compressed (`otlp.compression`). The bytes of these answers, before and after
compression, MUST be counted by endpoint in the self-metrics (§ 22,
`http_exporter_answer_bytes_total`).

The exporter's own HTTP server MUST bound what a client can hold open: request
headers within 10 seconds, the whole request within 30 seconds, and an idle
keep-alive connection closed after two minutes. The tests MAY give the headers
another time (§ 34); the exporter MUST NOT, so that its servers have the 10
seconds. A request's line and headers
together MUST be bounded at 64 KiB, a longer request being answered `431`. It
MUST NOT have a write timeout counted from the request's arrival, which would
cut off a probe that legitimately takes as long as its scrape timeout.
Instead every endpoint MUST set a write deadline of 30 seconds, a named
constant, when it starts writing its answer, status line included: a client
that has not read the answer by then MUST be dropped — its connection closed
and its handler, with the rendered answer, released — and that MUST be logged
at debug level at most, being no failure of the exporter or of a target. The
deadline MUST be the answer's alone: the time the answer took to make MUST NOT
count against it, and it MUST NOT outlive the answer on a kept-alive
connection.

---

# 24. Configuration lifecycle

Configuration MUST be external to the binary, preferably YAML.

Support startup validation.

Strongly recommended:

- Config syntax validation before activation, including the collector Python
  scripts, whose contract section 18 defines.
- Atomic config reload.
- Collector-level validation.
- No partially loaded collector state.
- Optional SIGHUP/config-file reload and/or HTTP reload endpoint.

If a new configuration is invalid, the exporter should retain the last known valid configuration where practical and expose an explicit configuration error metric/log.

### 24.1 Configuration watch

Watching the configuration files for changes MUST be opt-in through a CLI flag
and MUST be disabled by default, so an exporter started without it reads its
configuration once and picks up changes on restart. The flag MUST cover every
configuration file the exporter was given, including the static target
document, so a deployment does not have to reason about which files are watched.

The watch interval MUST be configurable and MUST have a documented default of
60 seconds, which keeps an idle exporter from stating its configuration files
continuously while still picking a change up promptly enough for a reload. A
non-positive interval with the watch on MUST be rejected as a command-line error
(exit 2), at startup and with `--dry-run`, rather than silently disabling
the watch that was explicitly requested. The exporter SHOULD report whether the
watch is active in its startup log.

Changes MUST be detected by file modification time, size and mode rather
than by filesystem event notification: any time other than the one last read
is a change, an older one included, and so is a change of a file's
permissions alone, of the configuration file, a collector file and the
static target file, which may have made it readable again or unreadable; and
the files MUST be stamped before they are
read, at startup and on reload, so an edit made while they are is seen by the
next tick. Kubernetes republishes a mounted ConfigMap by atomically
swapping a `..data` symlink, which replaces the inode a file-level event watch
is attached to; such a watch stops firing after the first change unless it
watches the directory and re-arms. Polling is unaffected by this.

The watch MUST cover the collector files (§ 5.0): a collector file changed, a
file newly matching a pattern, and a file removed MUST each reload the
configuration, although the configuration file itself did not change. A reload
that fails MUST NOT be retried until one of the files changes again, but for
one refused because a file it reads could not be read: the configuration
file, a collector file or the static target file missing, a directory, or
unreadable. Such a file MUST be read again at every tick until it is in
force or refused for another reason, although nothing its stamps hold
changed, since a file can become readable through its owner, an ACL or a
directory's permissions, which no stamp shows; and with it the other of the
two files when that one waits for it (§ 24.1a). A try at a tick that is
refused with the text of the last refusal of that file, as the failure log
recognises a failure (§ 25.1), MUST NOT be logged nor counted in the reload
self-metrics (§ 22.0b) again; one refused otherwise MUST be logged and
counted once, as any rejected reload. A file refused for what it says, not
YAML or not valid, MUST NOT be read at a tick at which none of its files
changed. The
collector files watched MUST be those the configuration file lists, even when
that configuration is refused, so a configuration adding a collector file with
a mistake in it reloads once that file is fixed. Starting the watch MUST NOT
reload a configuration that has not changed since it was loaded.

The watch MUST cover a sql collector's `request.statement_file` in force as
it covers a descriptor file, below: one changing, appearing or disappearing
MUST reload at the next tick, the statement read again and checked as the
load checks it. The watch MUST cover the descriptor files of the configuration in force: the
`request.protoset_file` and `request.proto_files` of its collectors, which
only a `grpc` collector may set (§ 5.1), and the files those `.proto` files
import, through however many files, which are every path the compile of the
`proto_files` looked at on disk, as set out below for a refused reload. The
configuration is checked against these files when it loads, and a collector
reads them again at a call when they change, as this watch tells a change
(§ 5.1), so one of them changing,
appearing or disappearing MUST reload at the next tick, as a change to the
configuration file does, although no configuration file changed: the
configuration MUST be read and checked again, against the descriptors as a
call would read them then (§ 5.1). A descriptor file that no longer defines a
collector's method, no longer takes its message or no longer compiles MUST
thus be a rejected reload at that tick — logged with the reason, counted, and
shown by the reload self-metrics (§ 22.0b) — and one the configuration still
fits MUST be a reload logged as any other, which MUST keep everything a reload
of an unchanged configuration keeps (§ 24.1a). A reload rejected for such a
file MUST leave the configuration in force and MUST NOT be tried again until a
file changes again; the collectors read the changed files at their next call
whatever became of it.

The static target file MUST be read and checked with the configuration at
such a tick when a target of the file in force sets a `request.message` for
a collector that names descriptor files, so that a message the changed files
refuse is a rejected reload of the target file, on its own line and in its
own series. Which descriptor file changed need not be told: the target file
MAY be read for a file of a collector none of its targets uses, and is then
found as it was. A target file none of whose targets does, and one whose own
last reload was refused, MUST NOT be read for a descriptor file: the latter
is read again only for what it was refused for, below. That holds for a
target file refused only because the configuration in force disagrees with
it, which waits for the configuration (§ 24.1a): it MUST be read again when
the configuration file or a collector file changes, or a file that a refused
configuration is tried again for, and MUST NOT be read because a descriptor
file of the configuration in force changed, unless its own check opened that
file. The configuration such a tick reads is the one that refused it.

A descriptor file MUST be stamped, as what its path leads to (below), before
the configuration is checked against it, at startup and on reload: a file
the configuration names before anything reads it, and a file one of those
imports when the files that import it have been read to learn which it is,
before the configuration is validated, a file that changed between that
reading and its stamp counting as changed. Which files a collector's
`.proto` files import MUST be learnt then from the configuration as its
files write it, its `request.type` and `request.descriptors` taken as the
validation takes them, as written (§ 5.1): a collector written `type: GRPC`,
which the validation refuses, has none stamped, and nothing is stamped once
the configuration is in force. The compile
that learns them MUST be the one the validation uses. A descriptor file
changed at any later moment — while the configuration is validated, while
the rest of a startup or a reload runs, or once the configuration is in
force — MUST therefore reload at the next tick, and starting the watch MUST
NOT reload for descriptor files that have not changed since the
configuration was loaded.
The files watched MUST be those of the configuration in force, from the
reload that put it in force: a reload that removes a collector, or names
other files for it, changes them. Without the watch, a startup and a reload
that is accepted MUST NOT stamp a descriptor file, nor read one to learn
what it imports: they read the descriptor files only to check the
configuration (§ 5.1), and nothing is watched in force.

While the configuration last read is refused, whatever triggered that reload,
the files it names that loading it opens MUST count among the files whose
change reloads it, with those the configuration in force names:
`otlp.tls.ca_file`, `cert_file` and `key_file` when `otlp` is enabled,
`web.basic_auth.username_file` and `password_file` when it is enabled, and a
collector's `request.protoset_file` and `request.proto_files`. A reload
refused because one of them was missing or half replaced — a Secret being
rotated — MUST thus be tried again at the tick after the file is in place,
without the configuration being touched. Such a file MUST be stamped before
the load reads it, as what its path leads to: its modification time, size
and permissions, the file a symbolic link resolves to, its identity (device
and inode) where the system gives it, or that there is none, so a swapped
`..data` link, and a file renamed over by another, are a change even when
the new file has the time and size of the old.

The files that a collector's `.proto` files import, which the configuration
does not name, MUST count among them too, through however many files: every
path the compile of the `proto_files` looked at on disk — a file it read,
where the import paths resolved it, and a place where it looked for a file
and found none, which for a missing import is every import path and for a
file found in a later import path each one before it. A reload refused for
an imported file that did not compile, was missing, or lacked what the
configuration uses MUST thus be tried again at the tick after that file
changes or appears. A file of the same name in a later import path than the
one it was found in, and the places where a well-known file
(`google/protobuf/*.proto`) that is built in was looked for, MUST NOT be
among them. Which files are imported is known only once the files that
import them have been read, so they MUST be stamped once they have, and a
file that changed between that reading of it and its stamp MUST reload at
the next tick, as a change made while a named file was being read does. The
descriptor files of the configuration that was read, named and imported,
MUST be watched while it is refused as they were stamped before it was
validated (above): one that changed at any moment since — mended after the
validation read it and before the reload was refused, say — MUST reload at
the next tick, and MUST NOT be stamped again as the refusal finds it.

A tick that finds nothing changed MUST NOT read the configuration, MUST NOT
compile a `.proto` file and MUST NOT log, with descriptor files in force as
without: it MAY only stamp the files. However many files changed since
the last tick, a tick MUST make one reload, logged as any other. Once the
configuration is in force, the files among these that are not descriptor
files — the `otlp.tls` files and the `web.basic_auth` files — MUST NOT
reload it: nothing in the configuration is checked against them, and what
uses them reads them again itself. Its descriptor files, named and imported,
MUST go on reloading it, as above.

The static target file MUST be held to the same. While the target file last
read is refused, whatever triggered that reload, the files that checking it
against the configuration opens MUST count among the files whose change
reloads it: the `request.protoset_file` and `request.proto_files` of the
collectors named by its targets that set a `request.message`, in the
configuration it was checked against and in the one in force, and the files
those `.proto` files import, as for the configuration. They MUST be
stamped before the check reads them, as above, the imported ones once the
files that import them have been read to learn which they are, so that one
that changes after the check read it reads the target file again; a tick
that finds them as
they were MUST NOT read the target file and MUST NOT log; and once the
target file is in force they reload it as descriptor files of the
configuration in force do, above. A target file refused
before that check — one that cannot be read as a target file, or is invalid
on its own — opens no other file, and no file but itself changing MUST read
it again. A target's own credential files are read at a scrape and MUST NOT
refuse a reload.

Enabling the watch MUST NOT weaken any reload rule: an invalid configuration, a
configuration that would disable OTLP while a loaded static target sets
`export_via_otlp`, a
collector name defined twice, and a pre-script that stops producing `data` MUST
all still be rejected with the last valid configuration left active.

When a disabled watch is configured, the exporter MUST NOT run a polling loop at
all.

### 24.1a Reloading on demand

The exporter MUST reload when asked, not only when the watch finds a change:

- On `SIGHUP`, always. The signal MUST be caught from the moment the process
  begins, before the configuration is read: one that arrives during the
  start MUST NOT end the process, as the default action of an uncaught
  `SIGHUP` would, and MUST cause one reload once the exporter has started.
- On `POST` or `PUT` `/-/reload`, when started with `--web.enable-lifecycle`,
  which MUST default to off. Without the flag the endpoint MUST answer `403`
  saying how to enable it; any other method MUST answer `405` with
  `Allow: POST, PUT`. The endpoint MUST be protected by `web.basic_auth` like
  `/probe` and the self-metrics path, and refuse a request of another
  site's page as every endpoint does (§ 42.5). It MUST answer `200` when every file was accepted,
  and `500` with the reason when one was rejected, the previous configuration
  staying in force.

Whatever its trigger — the watch, `SIGHUP`, `/-/reload` or a bundle of the
allocator — a reload that puts the configuration in force MUST say on its
`configuration reloaded` line, at `info`, under `changes`, the collectors
it added, removed and changed, and one that puts the static target file in
force on its `static targets reloaded` line the static targets: as
`collectors added: a, b; collectors removed: c; collectors changed: d`, the
lists that are not empty in that order, each sorted, or `no changes` when
none is. A collector MUST be changed when its fingerprint (§ 42.13)
differs, which the reload MUST read from those it works out to follow the
configuration, encoding no definition for it, a key it takes from its
file's `defaults` (§ 5.0b) as much as one it writes, and a static target when
anything in its entry differs. Each list MUST name at most 10 and count the
rest (`and 3 more`). A refused reload MUST NOT change what the last said.
The landing page (§ 23) MUST show the time of the last reload that put
something in force, and what it changed, both files' changes together.

After any reload, whatever its trigger, the per-collector state MUST follow the
new configuration when the reload is made: on the path that reloads, before
the reload returns or `/-/reload` answers, and without a probe having to ask
for it. Where it has not been, it MUST follow before it is next used. A removed collector's
self-metric series, per-request series, scrape-time histogram, the
statistics of its Python workers (§ 22.1a), cached results
and remembered failures MUST be dropped, so its series stop being exposed and
exported and Prometheus marks them stale, and a collector added again under
the name MUST start from zero. A probe or static target scrape that read the
removed collector in a configuration from before the reload, however far it
had come when the collector's state was dropped — waiting for a scrape slot,
about to count, at its target, running its script, or ending — MUST NOT be
counted in the counters, the scrape-time histogram, the Python worker series
or a per-request series of a collector
added again under the name, MUST NOT start a per-request series of the
removed collector, and MUST NOT leave statistics under the removed
collector's name for a collector added again to take over. A probe or scrape
of a collector the reload kept MUST be counted for it wherever it was at the
reload. A collector whose definition changed MUST keep its counters, those
of its Python workers among them, MUST
have its cached results dropped, since their keys carry the old definition,
and MUST have its remembered failures forgotten (§ 25.1), as a removed
collector's are, so that the first failure of the new definition is logged as
a first failure and not as a repeat of the old one's, and MUST have the
targets its `failure_backoff` was backing off from forgotten (§ 42.13c), as a
removed collector's are; an unchanged collector
MUST keep everything.

What a reload changed — the collectors and static targets it removed,
changed or left as they were — MUST be worked out before the lock of the
per-collector statistics is taken, which every probe takes to find its
collector's statistics and every read of the self-metrics takes: the lock
MUST be held only to drop and replace what the following changes, and the
exporter MUST NOT encode, hash or compare a collector's definition while it
holds it. Following a reload MUST work out the fingerprint of each collector
of the new configuration (§ 42.13) at most once and MUST NOT work out again
those of the configuration it had followed, which it MUST keep with that
configuration; a static target file reloaded alone MUST work out none. A
reload MUST work them out before it puts its configuration in force, once
the configuration has been read and checked, on the path that reloads and
with the configuration before it still in force, which probes MUST go on
being answered by meanwhile without waiting; with them it MUST work out the
fingerprints not yet worked out of the collectors of the configuration
followed that the new one still names. A reload that is refused MUST work
out none. Once a reloaded configuration is in force the exporter MUST NOT
encode or hash a collector's definition to tell what the reload changed,
on the path that reloads or on a probe's: a probe that comes before the
reload has followed its configuration MUST NOT wait for a fingerprint. The
schedule of the static targets, which starts a target again when the
definition of its collector changed (§ 42.14), MUST tell that by the same
fingerprints, those kept with the configuration followed, read only when
that is the very configuration it plans with, and MUST plan what it would
plan had it encoded each definition itself. It MUST NOT work out again a
fingerprint that is kept: after a reload of a configuration, whose
fingerprints the reload made, it MUST NOT encode or hash a definition, so
that each collector is encoded once in all for the reload, static targets
or none; a fingerprint that nothing had asked for yet, of the configuration
the exporter started with, it MUST work out once and keep with that
configuration, where the probes and the next reload find it made. To find a
collector of the configuration followed by its name — a probe its own, the
scrape of a static target and its debug scrape (§ 42.17) its target's, the
schedule the one whose fingerprint it reads — the exporter MUST NOT go
through the collectors of that configuration: where each is MUST be noted
once for the configuration, by the reload that works out its fingerprints,
before it is in force, or, for the configuration the exporter started with,
by the first to ask, and the collector found MUST be the one going through
them finds, the first of that name. A read of the verbose self-metrics, which finds the collector of
every static target in force to name the target's request (§ 22.1), MUST
find each so too, while the configuration it read is the one followed, and
MUST track the requests it tracked while it went through the collectors for
every target. Past the limit of tracked combinations (§ 22.1) such a read
offers again every static target whose request is not tracked, and making
room for one by dropping the combinations no probe or scrape asked for in
the hour MUST NOT go through the tracked combinations for each target
offered. It MUST go through them only when one that can expire may have
gone that long unasked, which it MUST tell without going through them, so
that a read goes through them at most once however many targets are left
over, and again only to drop one that has gone the hour unasked since; and
it MUST track, drop and count exactly what going through them for each
target did. The debug scrape of a static target MUST find its target, the
first of that name in the file in force, without copying the targets it
passes on the way, so that finding it allocates nothing however many targets
the file has, and MUST NOT write to the target it found. To tell whether a
static target of a name is one of the static target file followed — the
scrape of a static target that ends, before it publishes (§ 42.14), and a
read of the static targets endpoint that names targets, of every name it
gives (§ 42.14a) — the exporter MUST
NOT go through the targets of that file: the names its targets have MUST be
noted once for the file, by the following of the reload that put it in
force, before the lock of the per-collector statistics is taken, or, for
the file the exporter started with, when it starts; they MUST be kept only
for the file followed, so that they do not grow with the reloads; and what
is told MUST be what going through the targets tells, of the file in force
when it is asked: a name is told of a file exactly when a target of it has
that name, as it is written. For a file a reload has put in force and has
not followed yet, as for a configuration that is not the one followed, the
exporter MAY go through them as it did. The
fingerprints kept MUST NOT grow with the reloads: the exporter MUST keep
those of the configuration in force, and MAY keep those of one
configuration before it while probes still read that one. What was worked
out MUST be done only if, with the lock held, the configuration and the
static target file it was worked out for are still in force and the
configuration it was worked out from is still the one followed. Otherwise it
MUST be worked out again from what is followed then, so that a reload that
overtakes another's following is followed with it as one, or be found done:
a probe that needs the per-collector state while a reload works out its
following MUST NOT wait for a lock the reload holds, and MAY work out and do
the same following itself, of which exactly one MUST be done.

A read of the self-metrics MUST hold that lock only to find the statistics
of each collector of the configuration it read, and to make those of a
collector first heard of: it MUST read the collectors' names, and make what
it gives the statistics back in at the size the configuration says, before
it takes the lock, and MUST NOT copy a collector's definition to read its
name. What it gives back MUST NOT depend on that: every collector of the
configuration read, sorted by name, each with the statistics kept for it, or
with statistics retired and kept nowhere when a reload removed it between
the read of the configuration and the lock.

What a probe or static target scrape writes under its collector's name when
it ends MUST go nowhere once the collector it read is no longer the one in
force: a reload followed since has removed the collector, or changed its
definition, whether or not a collector is back under the name with the
definition it had. Its result MUST NOT be cached, for `cache.ttl` or for
`cache.stale_if_error`, so that it neither answers a collector brought back
under the name nor counts in the `limits.max_cache_entries` of the one now
there; its failure MUST NOT be remembered (§ 25.1) and MUST be logged at debug
level only, marked `"superseded":true`; and its success MUST NOT be logged as,
or count as, a recovery. The probe MUST still be answered with what it
collected. Whether the collector is still the one read MUST be decided by the
generations of the configurations followed, as for the statistics, and MUST
be asked under the lock of the cache, and of the failure log, with the
followed configuration replaced under those locks as the reload's drop is
made, so that nothing is written after the drop that the drop was to remove;
it MUST NOT take another lock on a probe's path. A probe or scrape of a
collector that every configuration followed since has had unchanged MUST
cache and log as it did, whichever configuration it read; one made with a
configuration that is not in force by a caller that does not say when it read
it MUST write nothing either. A failure that ends before the reload is
followed is one from before the reload: it MAY be logged and remembered, and
MUST be forgotten when the reload is followed.

The result a static target's scrape publishes (§ 42.14a), with the outcome
noted for its last success, MUST be held to the same, and to the target: it
MUST go nowhere, over OTLP included, once a reload followed since has removed
or changed the scrape's collector, or has removed the target or changed
anything in its definition, whether or not a target is back under the name as
it was. The static target file in force MUST therefore be followed with the
configuration, a reload of either alone included. So a scrape begun before
such a reload MUST NOT replace the result of the first scrape of the target
now under the name, however the two end. A scrape of a target that every
reload since has left as it was, with its collector, MUST publish as it did.

What a static target's scrape, and the schedule of its turns (§ 42.14a),
tells the failure log (§ 25.1) MUST be held to the target as well as to the
collector, as the result is: once a reload followed since has removed the
target or changed anything in its definition, its collector as it was, a
failure MUST NOT be remembered and MUST be logged at debug level only, marked
`"superseded":true`, and a success MUST NOT be logged as, or count as, a
recovery of the target now under the name. Following a reload MUST forget
what is remembered under the name of a static target the reload removed or
changed, as it forgets a changed collector's, under the lock of the failure
log with the followed configuration replaced under it, so that the first
failure of the new definition is logged as a first failure and not as a
repeat of the old one's. What is remembered of a target the reload left as
it was MUST stay, and so MUST what the static targets endpoint remembers of
a metric it left out and what a trip remembers under the address it went to,
which a probe of that address shares.

Whose a remembered failure is — which collector's, and whether a static
target's own — MUST be what the probe, the scrape or the schedule that
reported it said, kept with the failure, and MUST NOT be read out of the key
the failure is remembered under. Following a reload MUST therefore forget
exactly the failures of the collectors it removed or changed, and exactly
the own failures of the static targets it removed or changed — a failed
scrape, a skipped turn, a rule of the scrape — whatever a probe's target, an
address, a file's name, a metric name, an expression or `items` hold: what
is remembered of a probe whose target is the words `static target` and the
name of a static target, and of a file of a directory so named, MUST stay
when a reload removes or changes that static target.

A probe MUST NOT join a trip in flight (§ 42.13a) that was started by a probe
of another stay of its collector — one that read the collector before a
reload changed it and another changed it back, or before it was removed and
brought back — although the two have the same key: the trip writes under the
collector's name only what the probe that started it may, so the probe that
joined would have its result left uncached and its failure unremembered while
its own collector stands. It MUST make a trip of its own. Probes of one stay
MUST share as before, whichever configurations they read the collector in.

Both MUST reload the configuration, with its collector files, and the
static target file when there is one, whether or not they changed, under the
same rules as the watch (§ 24.1), and record the result in the reload
self-metrics (§ 22.0b). Every reload, whatever its trigger, MUST be logged with
the trigger — `watch`, `sighup` or `http` — and a rejected reload's line MUST
carry `file`, the path of the configuration or static target file rejected, so
an error that gives only a line number can be placed. With the watch on, a
rejected reload's line MUST also say, as `retried_when`, what the watch reads
that file again for, naming each of these that holds and nothing else: the
file itself changing, which for the configuration counts its collector
files; a file watched for it while it is refused (§ 24.1), when there is
one, the files a configuration names and the files those import each named
when there are such; and the other of the two files, when the file was
rejected only because it disagrees with the other as in force, which a
change to the other MUST then read again; and, after the changes, that the
file is read again at every tick while a file it reads cannot be read, when
it was refused for that (§ 24.1). For a target file that waits, a
change to the configuration is one to its file, to a collector file or to a
file a refused configuration is tried again for, and not one to a descriptor
file of the configuration in force (§ 24.1). Without the watch nothing is read
again until a reload is asked for, and the line MUST NOT carry
`retried_when`. Reloads MUST be
serialized, so two triggers at once never interleave. The Helm chart MUST expose the flag as a
value (SPECIFICATION-CHART.md § 33.10).

The configuration and the static target file are checked against each other,
so they MUST be put in force as one pair, in one step: when a reload accepts
both, no reader may find the new one of the two in force with the old other.
Whatever needs both — a static target scrape above all — MUST read the pair
once rather than each on its own, and a scrape MUST use the configuration
its target was read with. Validation MUST NOT write into a configuration or
a static target file in force, which scrapes are reading: whatever spelling
it normalizes — a target's `accept_status`, `retry.statuses`, `accept_codes`
and `retry.codes` — MUST be written once, when the file is loaded and before it
is in force, and the check of the target file against the configuration,
which a reload repeats on the file in force, MUST only read both. That check
MUST NOT go through the configuration's collectors for each target, which
for as many targets as collectors costs the square of their number: where
the collectors are MUST be noted once for a check, by their names, and each
target's collector found there, the one going through them finds, the first
of that name, so that the check accepts what it accepted and refuses a file
for the same target and with the same message. Nor may the check find the
placeholders of a collector's request (§ 42.10a, § 42.10b) again for each
target that names the collector, or read which keys a target's `request`
block can set from their declaration again for each target: the
placeholders MUST be found once for each collector in a check, and the keys
once, with the same verdict and the same message as before. The collectors
whose descriptor files the check opens (§ 24.1) MUST be found the same way, once
for each configuration the file is checked against, and only once a target
sets a `request.message`: a target file in which none does MUST NOT have
the collectors gone through for it at all. A reload MUST check each pair of
a static target file and a configuration it asks about at most once — the
pair it read, a file it read with the other in force, the pair a file is
refused for — and asked about the same pair again MUST give the verdict the
check gave, the same error; a target file read alone MUST have the
configuration in force gone through once to find those collectors, not
once as the configuration read and again as the one in force. A reload
that changes a collector's definition MUST make that collector's static
targets due again (§ 42.14).

### 24.2 Validation of metric rules

Everything about a metric rule that can be known before a scrape MUST be
checked when the configuration loads — at startup, on reload and by `--dry-run`
— with a message naming the collector, the rule and, where it applies, the
label. A rule MUST be named by its metric name, as in `collector "node"
metric "up"`. A rule that has no name to be named by — `name` left out,
written `""` or nothing but blanks — MUST be named by its place among the
collector's `metrics`, counted from 1, as in `collector "node" metrics rule
2`, in every message of the load about it: that it has no name, `collector
"node" metrics rule 2 has no name`, which a rule of any transform but
`prometheus` is refused for, and whatever else is wrong with it, since
`metric ""` says which rule of thirty it is to nobody. A name that is no
metric name for another reason MUST be quoted as written, and a message
about a rule that has a name MUST read as it did before this rule. What is
checked:

- A declared metric name MUST be a valid Prometheus metric name and MUST NOT
  start with `__`. Under `name_escaping` `underscores` or `values` it, and
  every other name the collector writes, MAY be any name that is not blanks
  alone and is not exported starting with `__` (§ 21.1).
- Every expression MUST compile in its transform's language: jq and yq value,
  `items` and label expressions (compiled, not only parsed, so an undefined
  function or variable is caught); regular expressions; CSS selectors, which
  goquery would otherwise silently treat as matching nothing; XPath expressions
  and relative label expressions, with the collector's namespaces, all but a
  label that is one attribute read by its name (§ 11); and a prometheus
  transform's patterns, `include` and `exclude`.
- A regex metric's expression MUST have at least one capture group — the
  value is the group named `value`, or else the first, and the error says so
  — and MUST NOT have two groups named `value`.
- A rule's `time_format` MUST be a name or a layout with a year, a month and
  a day, no blanks around it and `PM` beside the hour of a 12-hour clock, and
  its `time_zone` a known zone; `time_zone` without `time_format`,
  `time_format` beside `value_map`, and `time_format` on a `python` or
  `prometheus` rule MUST be refused (§ 18.1).
- A regex label MUST name a capture group the regex has, by number or name.
- A prometheus transform's `rename` targets MUST be valid metric names.
  `include`, `exclude` and `rename` MUST be rejected on any collector other
  than a prometheus transform without `metrics` rules, rather than ignored.
  An entry of `include` or `exclude` MUST NOT be the empty string or nothing
  but blanks (§ 6).
- A label of a `python` rule MUST NOT set `value` (§ 18.1).
- Two rules of a collector MUST NOT be the same rule (§ 18.1).
- The `expression` of a `prometheus` rule MUST NOT match the `name` another
  rule of the collector passes on the metric of, where the two give its
  series one name and the same labels, but for a rule that its `type` keeps
  to the histograms or the summaries (§ 18.1).
- `transform.labels` keys and `rename_labels` targets MUST be valid label
  names, and two `rename_labels` entries with one target MUST be rejected.

- A `{{param_...}}` placeholder (§ 42.10a, § 42.10b, § 42.10c) in a setting
  of a collector where a probe's parameters are not filled in MUST be refused,
  naming the field — `request.bearer_token`, `request.basic_auth.username`,
  a credential or TLS file, `request.tls.server_name`, a prometheus
  transform's `include` or `exclude`, a `value_map`, a key of
  `transform.labels`, a label's `name`, the `value` of a label that also
  sets `expression` — since it would be sent or exported as written, and the
  refusal MUST name the label values among the places that are filled. The
  values of `transform.labels` and the `value` of a rule's static label are
  filled (§ 42.10c) and MUST NOT be refused; a placeholder of one that is
  not well formed MUST be, naming the collector and the value, a rule
  without a name by its place. Every string the collector holds MUST be checked but
  those written in a language of their own, where the text `{{param_` is that
  language's and no placeholder: `transform.script` and
  `transform.pre_script`, a metric's `items`, `expression` and `description`,
  and a label's `expression` MUST NOT be searched, so that a Python f-string
  (`f"{{param_x}}"`), a jq string, a regex matching the text and a
  description naming a parameter load and run as written; nothing fills a
  placeholder there. The fields that are filled in MUST be asked of the code
  that fills them rather than listed a second time, so the check stays
  complete as fields are added. Braces that open no `param_` placeholder are
  text.
- A limit that is a whole number — the `limits` counts and lengths,
  `max_concurrent_probes`, `retry.attempts`, `max_files`, the OTLP and static
  target counts — MUST NOT be negative and MUST be written as a whole number:
  a negative one MUST be refused naming the key and the value, and a number
  with a fraction, such as `max_metrics: 0.5` or `max_concurrent_probes: 1.9`,
  MUST be refused naming the key, the value and the line, in the
  configuration, a collector file and the static target file alike, rather
  than becoming the default or losing its fraction. A whole number written
  as `4.0` or `1e3` is one, and MUST be read by its digits and not as the
  floating-point number nearest to it: `9007199254740993.0` is that number,
  and `9.223372036854775807e18` is 2^63 - 1. A whole number so written that
  the key's integer does not hold, under -2^63 or from 2^63 (`1e19`, `-1e19`,
  `9.223372036854775808e18`), MUST be refused once, naming the key, the
  number as written and the range, never wrapped around or read otherwise on
  one platform than on another; one it holds MUST then be checked as the
  key checks the same number written with digits alone, in the same words.
  A number with a fraction MUST be refused however small the fraction
  (`1.00000000000000000001`). A number so written past the range of a
  floating-point number (`1e400`), which YAML 1.2 reads as a number and the
  YAML decoder as text, MUST be refused as past the key's integer, in the
  same message; in quotes or tagged `!!str` it is text. Such a number MUST be
  written into the key the YAML decoder decodes it into: a key written as an
  alias is the key its anchor holds, not the anchor's name, and a message
  MUST name it so. A whole number written with digits alone MUST
  be read as the YAML decoder reads it. A negative `limits.script_timeout` MUST be
  refused. 0, like a limit left out, keeps its meaning: the default. A
  `limits.script_timeout` that is set MUST be at least 1ms, under which a
  script's time measures the pipes to its worker and the machine's load more
  than the script, and a smaller one MUST be refused naming the collector,
  the key, the value, the least and the default. A
  `limits.max_output_bytes` that is set MUST be at least the 38 bytes a
  Python script that emits no metric answers in, and a smaller one MUST be
  refused naming
  the collector, the key, the value in bytes, the least and the default
  (§ 16.7). The check of the limits MUST say of every other set of limits
  what it said, which a test MUST show against the check as it was.
- A mapping key YAML reads as null — `null`, `~`, or an empty key — in any
  mapping of names to values, such as `value_map`, `headers` or `labels`,
  MUST be refused naming the mapping and the line, since the decoder would
  drop the entry; quoted, it is that text.
- A mapping key that is a sequence or a mapping, or an alias of one,
  anywhere in the configuration, a collector file or the static target
  file — a block merged in and an `x-` block nothing uses included — MUST
  be refused before the file is decoded, each such key at its line, as in
  `line 3: a mapping key is a sequence; only scalar keys (text, numbers,
  booleans, null) can be read`, a collector file's error naming the file as
  its other errors do; two of a kind on one line MUST be said once. A file
  without such a key MUST load, or be refused, as it was, which a test MUST
  show against the load without this refusal.
- A value YAML reads as none — nothing after the colon or the dash, `null`
  or `~`, in any spelling YAML takes for them — MUST be refused wherever a
  file takes a value: at every key the configuration, a collector file and
  the static target file have, required or not and whatever it takes, a
  block and a list among them; at every entry of every list; and at every
  value of every mapping of names to values. The decoder takes such a key
  for the key left out, or, of a static target's `request.path` and
  `request.body`, for one written `""`, drops such an entry, so that a rule
  meant under a bare dash is missing with nothing said, and takes such a
  value of a mapping for `""` or 0. The error MUST name the line and the
  place — the key, as in `line 7: ttl has nothing after its colon, which
  YAML reads as no value at all; write its value, or take the key out`, the
  list and the entry's place in it, counted from 1, as in `line 12: metrics
  entry 1 is a dash with nothing after it, …; write a metric rule there, or
  take the entry out`, or the mapping and its key — and MUST say how the
  value is written and what to do. Each such value MUST be reported, in the
  order of the file, beside whatever else the decoding refuses, and the
  checks of the configuration that come after decoding are not reached. The
  check MUST be of every key the files' types have, found from the types the
  schemas are generated from and not listed by hand, so that a key added
  later is held to it. A top-level `x-` key (§ 24.2a) is the file's own, and
  MAY hold such a value; text that is empty, `""`, is a value.
- A value MUST be written as the kind its key takes, in the configuration, a
  collector file and the static target file alike, and what a key takes MUST
  be what its schema says (§ 24.3), read from the schemas the files' types
  generate and not listed by hand. The decoder hands a key of text the text
  of whatever is written. A key of free text — one the schemas hold to no
  allowed values and no pattern: a description, an expression, a label's
  `value`, a header, a query parameter, a body, a path, a value of a mapping
  of names to text — MUST take a scalar YAML reads as a boolean or a number
  as the text it spells, so that `value: 1` and `description: 404` need no
  quotes, and a key of a mapping is text however it is written
  (`value_map: {1: running}`). A key the schemas hold to text alone — one
  with allowed values or a pattern, and the few they say are text
  themselves: a collector's, a rule's, a label's and a static target's
  `name`, `metrics_prefix`, `name_escaping`, `request.type`, `method`, `rpc`
  and `descriptors`, `decoder.type`, `transform.type`, the error policies, a
  rule's `type`, `error_mode` and `time_zone`, `response.csv.delimiter`,
  `response.graphite.value` and `invalid_lines`, and an entry of
  `collector_files`, `redirect_trusted_hosts`, `accept_codes`,
  `retry.codes`, `transform.libraries` and `required_libs` — MUST refuse
  such a scalar, which the decoder took as its text, so that `name: true`
  was the name `true`: `line 9: name is written true, which YAML reads as a
  boolean, not as text; to use that text there, quote it: "true"`, naming
  the line and the key, or the list and the entry's place in it, how the
  value is written and whether YAML reads it as a boolean or as a number. A
  key the schemas hold to allowed values MUST be told those values in place
  of the advice to quote, which would not make the value one of them: `line
  5: type is written 1, which YAML reads as a number, not as text; write one
  of dns, graphite, grpc, http, icmp, kubernetes, ldap, localfile, ntp, redis, s3file, snmp, sql, sshexec, sshfile, tcp, tlscert`. A
  rule's and a label's `name` are among them under every `name_escaping`. A
  key that takes a boolean MUST take `true` and `false`, in the three cases
  YAML reads as one, and MUST refuse the words YAML 1.1 read as booleans —
  `y`, `yes`, `on`, `n`, `no`, `off` and their capitalised and upper-case
  forms, quoted or not — which are text to YAML 1.2 and to an editor and
  which the decoder took for booleans: `line 3: enabled is written yes,
  which YAML reads as text, not as a boolean; write true`, saying which of
  the two the word was read as. Text MUST be taken wherever text is, a date
  written without quotes and those words among it; a number or a boolean in
  quotes is text, and the decoder's own refusal of it at a key of numbers or
  booleans stands. Each such value MUST be reported, in the order of the
  file, beside whatever else the decoding refuses, and a document that
  writes none MUST load or be refused as before, message for message. A
  test MUST name the keys held to text alone, so that a key that comes to be
  among them, or to leave them, is seen.
- These four checks MUST look at the values the decoder uses (§ 24.2a): with
  a merge key, the mapping's own key and not the merged one it replaces,
  wherever the merge key stands, and of a list of merges the first that sets
  the key, a merged mapping's own before what it merges in itself. A
  fraction in an anchor that every mapping using it overrides MUST NOT be
  refused; one that is used MUST be, at the line it is written on. A null
  key replaces nothing and is replaced by nothing, so one merged in MUST be
  refused.

An error about an expression MUST quote it: `collector "a" metric "x"
expression ".foo[": unexpected EOF`, as a CSS selector's and an XPath's do.

Every mistake found MUST be reported in one run, not only the first: the
mistakes of every collector, and of every rule of a collector, and of every
collector file, in the order of the files, collectors and rules, one per line.
A collector file that cannot be read MUST NOT keep the collectors of the
others from being checked. Within a collector the settings the rest depend
on — the request, limits, cache, formats and error policies — MAY stop that
collector at its first mistake, and within a rule its own settings — type,
name, labels — at its first; the rule's expressions MUST then each be checked.
Past 20 mistakes the rest MUST be counted (`and 5 more problems`) rather than
listed; `--dry-run` MUST list every one as an error of its own.

### 24.2a Decoding errors

A configuration, collector or static target file that cannot be decoded
MUST be refused with errors in the file's own terms, never the
implementation's: each names its line and says what was wrong there — an
unknown key and where it was found (`in a collector`, `in retry`, `in a
static target's request`), a value of the wrong kind with what was
expected (`a duration such as 30s`, `a whole number`, `a list of values`,
`true or false`) and what was found (`a list`, `a mapping`, `a string`,
`the number 3`). A string found MUST NOT be quoted back: under
`--config.expand-env` it may be a secret an environment reference put where a
mapping belongs, and the message reaches the log and the reload status; the
line number says where it is. A message MUST NOT name a Go type or package or a
YAML tag. Every error in the file MUST be reported at once, joined by `; `,
rather than only the first, in the order of the file within a block, and on
one line by the column of the key, or the entry of a list, each is about. A
block that checks its own keys — `cache`, `failure_backoff`, `request.tls`
and a static target's `request` — MUST say every unknown key and every value
of the wrong kind it holds, not stop at its first unknown key, and neither it
nor a `cache` or `failure_backoff` written as one value MUST end the reading
of the rest of the file. A value of the wrong kind, which the YAML decoder says with its line alone,
MUST be placed at its own key, not where the block around it begins, so that
on a line such as `defaults: {bogus: 1, limits: {max_metrics: x}}` the
unknown key comes first, in the defaults block, a collector's `request`
block and a block an `enabled` key switches on alike, and loading the same
file again MUST give the same order; the
exception is an unknown key of a collector's `request` block that a key its
type does not take comes before, which MUST be left to be refused once that
key is gone, so that the error names the first in the file (§ 5.1). A key
of a collector's `request` block that its type does not take MUST be refused
as such whatever its value: a value of the wrong kind for the type that
takes it, `starttls: true` in an `ldap` collector or `start_tls: smtp` in a
`tlscert` one, MUST NOT be refused for its kind before it, neither by the
decoder's word nor by the words of what text YAML read as a boolean or a
number, which would tell the values of another type's key; a mistake of
another key that is on the same line MUST still be said. A YAML syntax error is reported as the YAML
parser words it.

Unknown keys MUST be refused everywhere in these files, including in blocks
decoded by custom code such as a static target's `request`, where they
would otherwise be ignored without a word. The one exception is a top-level
key of the configuration, a collector file or the static target file that
begins with `x-` and has more after it: it MUST be ignored, whatever it holds,
and the rest of the file decoded and checked as usual, so the file can keep
YAML anchors (`&name`) there for the rest of it to reuse with aliases
(`*name`) and merge keys (`<<: *name`), such as the request and transform
several collectors share. An `x-` key anywhere below the top level, and a bare
`x-`, MUST be refused as unknown keys. The published schemas MUST accept such
top-level keys through `patternProperties`. With environment expansion on, a reference inside a
top-level `x-` block MUST be expanded only when an alias outside the `x-`
blocks uses what the block defines, directly or through other aliases; an
unused block's references MUST be left alone, their variables not required.
Aliases and merge keys MUST work
in all three files as YAML defines them: a key set beside a merge replaces the
merged value of that key whole, and of a list of merged mappings the first
that sets a key supplies it. They MUST work in every mapping, including
the blocks decoded by custom code — a collector's `cache`, a static target's
`request`, `otlp`, `web.basic_auth` and the top level of a collector file —
with `<<` taking one mapping or a list of them. Wherever the exporter looks
at a mapping's keys itself, a key a merge supplies MUST count as a key of
the mapping: an unknown one MUST be refused as if written there, at any
depth and through an alias, and a `path` or `body` merged into a static
target's `request` MUST replace the collector's as one written out does. A
quoted `"<<"` is an ordinary key. Wherever the exporter looks at a mapping's
keys itself, a key written as an alias of an anchored scalar (`*k : value`
with `&k ttl` elsewhere) MUST be the key its anchor holds, as the YAML
decoder reads it, not the anchor's name: it MUST be accepted where that key
is, a `path` or `body` so written MUST set that key of a static target's
`request`, and a message about it MUST name that key, an unknown one
included. A top-level alias key whose anchor holds an `x-` key MUST be
ignored as one written out is, by environment expansion too. An alias of
`<<` is the text `<<` to the decoder and MUST NOT merge: it is an unknown
key.

Each of these files MUST hold one YAML document. A second document after a
`---` MUST be refused naming the line it starts on, since it would otherwise
be ignored without a word. A leading `---`, and a final `---` or `...` with
nothing after it but comments, MUST be accepted.

### 24.3 Configuration schema

The repository MUST publish a JSON Schema (draft 2020-12) of the configuration
file, `configs/config.schema.json`, so editors can complete keys, show descriptions and
flag unknown keys and invalid values. It MUST be generated from the Go
configuration structs, with the allowed values, patterns, required keys and
descriptions the structs cannot express added by path, so a key added to the
configuration cannot be missing from it. `--config.schema` MUST print the
schema of the running binary — its `request.type` values are the request types
that binary was built with (§ 5.1) — and exit 0.

A test MUST fail when the committed file differs from what the code generates
in a build with every request type, which is the build the committed files
describe (§ 5.1),
when a key the configuration reads is missing from the schema or the schema has
a key the configuration does not read, when any shipped configuration (the
examples, the demo configurations and the chart's default configuration) does
not validate against it, and when it accepts any of a set of invalid documents.
The example configurations in `configs/` MUST begin with the
`yaml-language-server` modeline pointing at the published schema.

The configuration schema MUST require `collectors` or `collector_files`, each
non-empty when it is the one present. The repository MUST also publish
`configs/collector-file.schema.json`, the schema of a collector file (§ 5.0): a required,
non-empty `collectors` list, `defaults` and no other key, its collectors described by the
same rules as the configuration's, which a test MUST check. Both schemas MUST
describe `defaults` (§ 5.0b) with exactly the keys it takes, each by the rules
of the collector's key it sets, which a test MUST check.
`--config.collector-file-schema` MUST print it and exit 0; a test MUST fail when
the committed file differs from what the code generates.

The repository MUST likewise publish `configs/static-targets.schema.json`, the schema
of the static target file (§ 42.14), generated from the Go target structs
with its own rules by path, and `--static-targets-file-schema` MUST print it and
exit 0. A test MUST fail when the committed file differs from what the code
generates, when the example target file does not validate against it, and when
it accepts any of a set of invalid target files; the example target file MUST
begin with the modeline pointing at it.

The schemas and the example configurations MUST live together in `configs/`,
not at the repository root, and the schemas' published addresses MUST be under
it. `make schemas` MUST regenerate all three, and the target allocator's two
(§ 42.14d).

A key the schemas hold to a set of words MUST be taken by the exporter in the
spelling the schemas take and no other, so that a file an editor marks is one
the exporter refuses: `request.type`, `transform.type`, `decoder.type`, the
three `error_handling` policies, a rule's `error_mode`,
`request.pagination.type`, `request.descriptors`, `response.graphite.value`
and `response.graphite.invalid_lines` in lower case and without blanks around
them, and `request.method`, a collector's and a static target's, in upper case
or lower case; any other spelling MUST be refused at load naming the values
taken. A key whose values the exporter matches in any case, as a standard or
its own documentation has them — `request.driver`, `request.record_type`, a
header name, an `allowed_schemes` entry, a gRPC status code of
`retry.codes` or `accept_codes` — MUST be taken in any case by the schemas
too, and the schemas MUST refuse what the exporter refuses there: a gRPC
status code's entry that names no code, or `OK`, MUST be refused by the
schemas' pattern as by the load.

The schema describes the canonical spelling, and MUST allow an unquoted number
or boolean where the exporter reads free text, since YAML reads `expression: 1`
as a number. A key with allowed values or a pattern MUST be a string alone,
a rule's and a label's `name` among them whatever the collector's
`name_escaping`, and the exporter MUST refuse a number or a boolean there as
the schema does; a boolean key MUST be a boolean alone, and the exporter
MUST refuse the words YAML 1.1 read as one (§ 24.2). A test MUST read every
place of the three committed schemas where a single value is written — each
key, each entry of a list, each value of a mapping — and put a boolean, a
number, such a word and a quoted number or boolean at it through the schema
and the exporter, and require that the schema finds a problem at the place
exactly when the exporter refuses what is on its line while reading the
file; the tables of keys MUST put each of their keys through both written
`true` and `1` as well. The exporter's reader takes a few more spellings
for a number than YAML 1.2 does, such as `1_000` and `0b101`, which an
editor reads as text and the exporter MUST refuse as numbers at a key of
text alone: the documentation of the schemas MUST say so. Startup
validation remains the authority on what is valid.

Where a schema can tell, it MUST refuse what the exporter refuses, and a test
MUST put a table of documents through both the committed schema and the
exporter's loader and require the same verdict of each, for at least:
`response.csv.delimiter`, a string of at most one character (`maxLength`,
which counts characters, so a tab or a letter of several bytes is one) that
is not a double quote, a carriage return, a line feed, NUL or U+FFFD; the
sizes of § 5.0a; the blocks an `enabled` key switches on (§ 42.1), every
key of which the test MUST try; and durations, in a key of each kind — one
that takes zero, one whose zero is its default, and the target file's
required `interval` — of each of the three schemas. A duration key's pattern
MUST take what the exporter reads as a duration that is not negative, and
beyond that only a duration too long to be held: numbers with units, each
number with a digit before or after its point, or a bare `0`; a `+` before
any of them; and a `-` only before a zero written with no other digit, such
as `-0s`. YAML reads an unquoted `0`, `+0` or `-0` as a number: the exporter
MUST read it as the duration zero it spells, and a duration key of the
schemas MUST take the whole number 0 beside the text, and no other number,
through the one description every duration key shares, which a test MUST
check of every duration key of the three schemas, found by its type. A
number that is not zero has no unit and MUST be refused by both:
`timeout: 30` is no duration, not 30 of anything. No key takes a negative
duration, so `timeout: -5s` MUST be
refused by the schema as it is at the load. A duration written with a `-`
and a digit other than zero is negative however small: the exporter MUST
read one that rounds to zero, such as `-0.4ns`, as negative, so that each
key refuses it as it refuses `-1ns`, and the pattern with it. Only
`otlp.timeout` and `otlp.interval` MUST take a negative duration, in a block
with `enabled: false`, which the exporter keeps unchecked, and MUST refuse
it with `enabled: true`. A duration too long to be held — more than
2^63 - 1 nanoseconds, some 292 years, such as `2562048h` — is written as
one, which is all a pattern can tell: the schemas take it, it stays the
exporter's to refuse, as no duration, the test MUST hold both to that, and
the documentation of the schemas MUST say so. A zero that YAML reads as a
number and that is not written `0` — `0.0`, `00`, `0x0`, `0e0` — reaches a
schema as the number 0, which it takes, while the exporter reads what is
written and MUST refuse it as no duration; the test MUST hold both to that
by name too, and the documentation MUST say it. These two MUST be the only
differences in how a duration is written. A duration key with a least MUST
be held to it by the schemas as far as the digits of a duration of one part
tell it, by a `not` of the text of one number and one unit whose number is
under the least divided by the unit, generated from the constant the loader
checks against: `limits.script_timeout` 1ms, `request.keepalive_interval`
1s, an `sql` collector's and target's `connections.keepalive.interval` 1s,
the static target file's `interval` and a target's `interval` 1s,
`otlp.interval` 1s in a block that is switched on, the allocator's
`refresh_interval` 1s, and `failure_backoff`'s `initial`, `max` and
`min_budget`, which MUST be more than zero. Where zero is the key's default,
zero and a part that comes to no whole nanosecond (`0.5ns`) MUST stay taken;
where it is not, the schemas MUST refuse zero however it is written, the
number 0 and `-0s` and `0s0m` too, as the exporter does. A duration of
several parts under the least, such as `400us400us`, whose parts a pattern
cannot add up, and that `failure_backoff.max` is not shorter than `initial`,
MUST stay the exporter's to refuse, and the documentation of the schemas
MUST say so. A test MUST check the generated patterns against the
exporter's reading of numbers around each least in every unit, and the test
of the two verdicts MUST hold both to a duration under the least of each
such key. What else a schema cannot tell MUST be said in the key's
description and stay the exporter's to refuse: the range of a size. A key of
any other whole number
takes a number written with a point or an exponent as the whole number it
equals, to the exporter as to a schema (§ 24.2): the range of the key's
integer, from -2^63 to 2^63 - 1, which no schema of the exporter bounds, and
a fraction past the digits of the floating-point number a schema is handed,
such as `1.00000000000000000001`, stay the exporter's to refuse, the
documentation of the schemas MUST say so, and a test MUST put such numbers
through both. An entry of `request.accept_status`, and of
`request.retry.statuses`, written as a number is the status it equals to both (§ 5.0), which the tables of keys MUST show of a
number with a point, an exponent, in hex, in octal, with an underscore, and
with a sign and leading zeros; the spellings of a number the exporter's
reader takes beyond YAML 1.2, such as `5_03`, are text that is no status to
an editor, which the documentation MUST say. Text the exporter takes as an
entry and the pattern of an entry refuses — blanks around the digits, and in
quotes a `+` or leading zeros before them — stays a difference of the two,
an editor flagging what the exporter takes as the status the digits read as
(`"0503"` and `"+503"` are 503). A number past the range of a floating-point
number, such as `1e400`, is a number to both, which the exporter refuses at a
key of a whole number (§ 24.2).
A size written as an unquoted
number with an exponent or a fraction of zero (`1e3`, `1.0`) is the whole
number it equals, to the exporter as to a schema (§ 5.0a). The least of
`limits.max_output_bytes` (§ 16.7) MUST be in the schemas of the
configuration and of a collector file as far as a schema can tell it: a
number at the key MUST be 0 or at least 38, with the least taken from the
constant the loader checks against, so that `1`, `37`, `1e1` and `26.0` are
refused by both and `0` and `38` taken by both, which the test of the two
verdicts MUST show. So MUST the least of `request.max_response_bytes` and
`limits.max_response_bytes`, 1KiB (§ 20), from the constant the loader
checks against: `1`, `1e1` and `1023` refused by both, `0` and `1024` taken
by both. So MUST the least of `limits.max_script_memory`, 32MiB, and of
`otlp.batch_max_bytes`, 64KiB, in a block that is switched on. A size
written as text — in quotes, or with a unit, such as `"26"`, `20B`,
`0.01KiB` or `16MiB` — MUST be held to the least of such a key by a `not` of
the text of a number, with a unit or without, that comes to at least a byte
and under the least, read from its digits, so that it is refused by both;
one that comes to no byte, such as `0.5B`, is the zero, and the default, to
both. The test of the two verdicts MUST hold both to it. The other sizes
MUST stay described as every size is.

A `tls` block MUST be held by the schemas to a client certificate with its
key or neither, `""` being a file left out, as the exporter refuses one of
the two alone when the configuration loads: a collector's `request.tls` and
`request.login.tls`, a static target's, and `otlp.tls` in a block that is
switched on. A collector takes a file its file's `defaults` write, so a
collector MUST be held to the file of the two the defaults do not write, and
the `defaults` block to neither; of defaults that write one and a collector
that writes neither, the exporter alone MUST refuse it. A request's
credentials MUST be held by the schemas to what the exporter refuses of
them, a collector's and a static target's: `bearer_token` beside
`bearer_token_file`, `basic_auth` beside `basic_auth_file`, a Basic
credential beside a bearer token, and a `basic_auth_file` without both its
files written with something other than blanks. So MUST an `http`
collector's `pagination` keys of another way of paging (§ 5.1, Pagination), a `grpc`
collector's descriptor files beside another source of `descriptors`, or
that source without its files, and a `python` transform without a `script`.
A test MUST put documents of each through the committed schemas and the
exporter and require one verdict of both.

The names of `request.headers`, a collector's and a static target's, and of
`otlp.headers` in a block that is switched on, MUST be held by the schemas to
what the exporter takes (§ 42.10): an HTTP token, letters, digits and
``!#$%&'*+-.^_`|~``, that is not `Accept-Encoding` in any case; two names
that are one header in different case stay the exporter's to refuse, as a
schema sees each name alone. The names of `request.query` MUST be held to
text of at least a character with no control character, and an entry of
`request.forward_headers` to a header name, with blanks around it or not,
that is none of the headers a probe never forwards (§ 42.4), each taken from
the list the exporter goes by. The tables of keys MUST show each of them,
and the schemas as they were, which took any name.

A key written as the empty string MUST be the key left out, to the exporter
and to the schemas alike, and CONFIGURATION.md MUST say so once, where it
describes the schemas. An optional key that the schemas hold to a set of
values or to a pattern MUST take `""` beside them, in one form each — `""`
last among the allowed values, and `^$|` before the pattern — and the
exporter MUST load it as the key left out: `decoder.type`, `name_escaping`,
`request.method`, `request.descriptors`, `request.rpc`, the three
`error_handling` policies, a rule's `type`, `error_mode` and `name`,
`metrics_prefix`, `response.graphite.value`,
`response.graphite.invalid_lines`, `otlp.compression`, and a static target's
`name` and `request.method`. A rule of the schemas about a key MUST go by
the key being written with a value other than `""`, not by its being there:
a label MUST have exactly one of `value` and `expression` so written, and
its `expression` MUST NOT be text of nothing but blanks (§ 18.1), which the
schemas MUST refuse by a pattern of the blanks `strings.TrimSpace` takes
off, under `not`, so that the key keeps the types it takes and `""` stays
the key left out; a
`grpc` collector MUST have `rpc` so written, and `descriptors` unless its
`rpc` names a method of `grpc.health.v1.Health`; a `localfile` collector
MUST have `root` so written; and every rule of a collector whose transform
is not `prometheus` MUST have `name` so written. The rule
of a block that `enabled` switches on (§ 42.1) goes by the keys the block
has, to both, so a key written `""` there still needs `enabled`. A key the
exporter requires MUST be refused by both written `""`, as it is left out:
`transform.type`, `request.type`, a collector's and a label's `name`, and a
static target's `collector`. A duration and a size have no empty form, and
MUST be refused by both written `""`; so MUST an empty entry of
`collector_files`, `request.accept_status`, `request.retry.statuses`,
`request.allowed_targets`, `request.denied_targets`,
`request.redirect_trusted_hosts`, `transform.include`, `transform.exclude`,
`transform.libraries` and `transform.required_libs`, and an empty key of a
`value_map`, of `request.metadata`, `request.headers` and `request.query`,
of a static target's `params`, `labels` and `request.headers`, and an empty
entry of `request.forward_headers`. An entry of `transform.include` or `transform.exclude` of nothing
but blanks MUST be refused by both too (§ 6), by the pattern a label's
`expression` is held to. A label of a rule of a collector whose
`transform.type` is `python` MUST NOT have `value` written with a value other
than `""`, to the schemas as to the exporter (§ 18.1), and MUST have
`truncate: true`; a rule of such a collector MUST NOT have `type`,
`description` or `error_mode` so written, nor `required` at all, a boolean
having no empty form. A rule's `expression` of a collector whose
`transform.type` is `prometheus` MUST NOT be text of nothing but blanks, by
the pattern a label's `expression` is held to (§ 18.1), and a rule of such
a collector MUST have `name` or `expression` written with a value other than
`""` (§ 18.1).

A key written with no value at all — nothing after its colon, `null` or
`~` — is not the key written `""`, and MUST be refused by the schemas and
by the exporter alike, as an entry of a list and a value of a mapping so
written MUST be (§ 24.2): null is none of the types a key of the schemas
takes, and the exporter MUST NOT take what they refuse. The schemas were
not widened to take null at each optional key: that makes every key's type
a list with `null` in it and every rule about a key being written a rule
about its not being null either, some quarter more of each schema, and an
editor would no longer flag a key left empty by mistake. A test MUST find
every key, every list and every mapping of names to values from the
committed schemas themselves, in the configuration, a collector file and
the target file, and hold the schema and the exporter to refusing a
document that writes that one place with nothing, with `null` and with `~`,
the exporter by the line and the place; and the tables of constrained keys
(below) MUST put each of their keys through both written `null` and `~`.
The test's own validator MUST read null as a JSON Schema validator does, as
of the type `null` alone.

A rule's `name` and a label's `name` MUST be classic names to the schemas
where the exporter holds them to that (§ 21.1) and any name that is not
blanks alone where it does not: the pattern of each MUST stand under a rule
of the collector that holds unless its `name_escaping` is `underscores` or
`values`, and the keys' descriptions MUST say so. That a name is not
exported beginning with `__`, as written or as `underscores` escapes it,
and what the keys of `transform.labels` and the targets of `rename` and
`rename_labels` may be, which the schemas hold to nothing, stay the
exporter's to refuse, and a test MUST show each past the schema and refused
by the exporter.

A block that `enabled` switches on is kept unchecked while it says
`enabled: false`, and that MUST hold of the schemas as of the exporter, for
every key of the block: what the exporter refuses of a key only in a block
that is switched on, the schemas MUST refuse only there, under a rule that
goes by `enabled` being `true`. Of the `otlp` block that is a negative
`timeout` or `interval`, a `compression` that is neither `gzip` nor `none`,
a negative `max_pending_points`, `unready_after_failures` or
`batch_max_size`, an `unready_after_failures` over 1000, and a
`batch_max_bytes` written as a number under its least; the rest of what the exporter checks of the block — the endpoint, the
headers, the `tls` files, the least interval, `service.name` among the
attributes, a `batch_max_bytes` under its least written as text — no schema
can tell. What is not of a key's type at all — text for a number, a list for
text, an unknown key — is not a setting kept for later, and MUST be refused
by both in a block switched off as in one switched on. A test MUST put every
key of the `otlp` block, found from its struct, through both, switched off
and on, with the values a block that is on is refused for and with values
that are none of the key's type.

The schemas MUST refuse of a `value_map`, a rule's and a label's, what the
exporter refuses of its keys and values (§ 18.1): a key that is empty or
has a blank before or after it, a blank being what `strings.TrimSpace`
takes off, by a pattern Go and JavaScript read alike; a rule's value that
is no number; and a value of `""` in the map of a `required` label. `"*"`
is a key like any other to a schema. A key written twice and a key YAML
reads as no key at all are YAML's and the exporter's to refuse, a validator
being handed the mapping made of them; and where a `value_map` may stand —
not on a static label, in a `python` or `prometheus` rule or in a rule
without a name — stays the exporter's to refuse.

The schemas MUST refuse of a rule's `histogram` block (§ 18.1) what a schema
can tell, as the exporter does: the block on a rule whose `type` is not
`histogram` or is left out, on a `python` or `prometheus` rule, without
`buckets`, and with no bucket or more than 100. Whether the bounds are
finite, in increasing order and each once, and whether such a rule has a
label `le`, stay the exporter's to refuse, the description saying so, and a
test MUST show both kinds.

That no two rules of a collector are the same rule (§ 18.1) is the
exporter's alone to refuse. A schema compares the items of a list only
whole, with `uniqueItems`, and that is another verdict: it would take two
rules that differ in a `description` or a `scale`, or of which one writes
`expression: ""` and the other nothing, which are the same rule. The
schemas MUST take each such pair, the description of `metrics` MUST say
what the exporter checks of it, and a test MUST show such pairs past the
schema and refused by the exporter, and the pairs that are two rules taken
by both. The same holds of a `prometheus` rule of a name beside one whose
expression matches that name (§ 18.1): whether an expression matches what
another item of the list names is nothing a schema asks, so the schemas
MUST take the pair, the description MUST say that the exporter checks it,
and the test MUST show it.

A test MUST hold every key that a schema holds to allowed values, a pattern
or a length to one verdict from the committed schema and from the loader,
four ways: left out, written `""`, with a value both take and with one both
refuse, in the configuration, in a collector file and in the target file;
a key taken both left out and written `""` MUST load as the same thing both
ways. The keys MUST come from tables, one for each request type, in a file
with that type's build constraint, and a test of the build with every type
MUST fail when a schema holds a key to such a rule that no table has. The
tables MUST also say what the schemas said of each case before `""` was the
key left out to them, and the test MUST hold a schema as it was — the
committed one with those changes taken out — to that, so that every case
the changes are not about is shown to get the verdict it got.

---

# 25. Logging

Every line the exporter writes to its log MUST be a JSON object, with no
exceptions and no second format. Logs are read by machines, so a line in another
shape is not a cosmetic inconsistency: it is a line the collector drops or
chokes on, in the middle of a stream it otherwise parses.

This MUST hold for lines the exporter does not write through a logger it passed
down the call chain. Metric extraction reports a failing rule from inside a
transform, several calls below anything holding a logger, and reaches Go's
default logger instead. The exporter MUST therefore install its own JSON logger
as the process default at startup, rather than relying on every call site to
have been given one — a rule enforced only by convention is one a later change
breaks silently, and the resulting line looks like

```text
2026/09/18 21:43:35 ERROR metric extraction failed metric=demo_value error="..."
```

which is the text handler's default format, not the exporter's.

The configured log level MUST apply to those lines too. `--log.level` MUST be
one of `debug`, `info`, `warn` and `error`, matched case-insensitively; any
other value MUST be refused as a malformed command line, with exit status 2
before `--dry-run` or startup, rather than falling back to `info`.

With `otlp.logs` (§ 42.1b) the lines MUST also be exported over OTLP, through
the logger installed as the process default, so the default logger's lines
are exported too; stderr MUST be written exactly as it is without it.

A logged metric extraction failure MUST name the collector as well as the rule.
A metric name is not unique across collectors — the same rule is often copied
between them — so the rule name alone does not say which collector to go and
look at. On a probe or static target scrape it MUST also carry the target, as
that scrape's failure lines do (`target` and `url` for a probe, `target` and
`address` for a static target, and `file` for a file of a directory). A rule
under `error_mode: log` MUST be logged at warning level, since the scrape was
answered, as `metric extraction failed`, and MUST go through the failure log
(§ 25.1), keyed by collector, target and rule, so a rule failing on every
scrape is logged once and then as a repeat, and `metric extraction recovered`
is logged once it produces its series again. The rule is told apart as § 18.1
tells it, by its metric name, its expression and its `items`: of several
rules of one metric name each MUST be logged, remembered and recovered by
itself, so that one starting to fail beside one that has been failing is
logged in full, and one that produces its series again is logged as
recovered while the other is still remembered. Where several rules of the
collector that differ in expression or `items` export the metric name, the
rule's lines — the failure, its repeats and its recovery — MUST carry the
rule's `expression`, and its `items` when it has any, after `metric`;
whether they do MUST follow from the collector's rules and not from which of
them failed on a scrape. A scrape of a target, or a directory's file, none
of whose rules has a failure remembered — as nearly every scrape is — MUST
cost the failure log one lookup: it MUST NOT make the key of each rule, whose
size is that of the rule's expression, to ask whether that rule recovered.
Neither MUST a scrape make it while a failure of one of its rules is
remembered — as it is on every scrape of a target that lacks one value, for
as long as it lacks it: what such a scrape allocates for the failure log
MUST NOT grow with the length of its rules' expressions, a rule's key being
made when its failure starts to be remembered and not on the scrapes that
find it remembered. Nor MUST a rule's key be made for a failure the failure
log does not remember — one that starts while the log is full (§ 25.1) and
is logged every time, on every scrape for as long as the log stays full, and
one a debug probe reports, which the failure log is told nothing of: what
such a scrape allocates for the failure log MUST NOT grow with the length of
its rules' expressions either. A rule that failed on a scrape MUST NOT be
logged as recovered by that scrape, whether or not its failure was
remembered when the scrape reported it.
What the failure log remembers of a rule MUST be found by the scrape of the
target, or the directory's file, the rule failed on, whatever the collector,
the target and the file are named: a rule's failure MUST be remembered as
one of the trip or file its scrape reported it for, and the scrape MUST ask
with that same key, compared whole. The trip or file MUST NOT be read back
out of the key the rule's failure is remembered under, in which a target, a
file's name or an expression may hold what the key's parts are joined with:
the rules of a directory's file named `rule` MUST be logged as recovered as
those of any other file are, and their failures forgotten then.
The lines of a metric name only one rule exports
MUST carry neither. A `prometheus` rule without a name MUST be logged with
an empty `metric`, and one with a name and no expression, which matches by
its name, with an empty `expression` where its name is shared. A rule under
`error_mode: fail`
MUST NOT have a line of its own: the scrape's failure, which carries `metric`,
is its one line. A logging path MUST NOT be what fails a scrape, so an absent collector
MUST degrade to an empty name rather than panicking.

The startup line MUST report the listen address, the number of collectors, the
number of static targets, and whether the configuration watch is enabled.
When the watch is enabled it MUST also report the interval, because that is what
bounds how stale a running configuration can be; when it is disabled the
interval MUST be omitted rather than reported as a value that has no effect.

### 25.1 Repeated failures

A failure MUST be logged in full the first time. While the same thing keeps
failing the same way — the same collector, target, file of a directory where
there is one, rule where the failure is a rule's (§ 25), stage and failure —
a repeat MUST be logged at debug level
only, marked `"repeat":true`, except that once five minutes have passed since
the last line at the failure's own level it MUST be logged at that level again
with `repeated`, the occurrences since that line, and `failing_since`, when the
failure began. A different stage or failure MUST be logged at once as a new
failure.

The same failure is the same error text, its position in the response and its
measured sizes apart: the row, node, item, series, point, line or column an
error names, the place of a value among a label's, and a length, a count or a
duration measured of the response — a value's bytes, the labels of a series,
the elements a selector matched, a file's age — MUST NOT tell one failure from
another, so a failure that moves in a changing response is logged once, summed
up in `repeated`, and recovered from with every occurrence counted. Every line,
a repeat too, and the answer to the scraper MUST carry the error in full, with
the position and the size it had on that scrape. What else the text says MUST
still tell failures apart: a name, a limit, a value read from the response
(`value "n/a" is not a number` and `value "N/A" is not a number` are two
failures). Which part of a text is a position or a size MUST be said by the
error where it is made and MUST survive the errors that wrap it; no text is
searched for numbers. An error whose text a library made is recognised by its
text as it is, but for a CSV and an XML syntax error, whose line and column
are known apart from the message, and for the errors below, of which a value
the library keeps only in its text MAY be read from that text, in the one
form the library writes it in and nowhere else; a text not of that form MUST
be recognised whole. The line a YAML error names — `yaml: line N:`, each
`line N:` of its list of problems, and the `already defined at line N` of a
key written twice — MUST NOT tell one failure from another, and neither MUST
how many times a problem is listed: problems that read the same without their
lines are one, so a list with the same problem in every item is the same
failure when it grows by an item. No more than ten different problems MUST be
told apart; past ten the failure is recognised by the first ten and by there
being more, however many. An error on a document's first line, for which the
library writes no line, MUST be the same failure as the same error on a later
line. The length a decode error gives of a part or a value it cut, or of
itself (`... (100000 bytes)`, § 6 and § 10), is a size like any other and
MUST NOT tell one failure from another: what was cut is recognised by the
start that is shown, and an error that shows several values, as a series
named by its labels, by the start of each.
Neither MUST what
a failed fetch has of the one connection or attempt: the address a connection
was made from, of every network error in the failure and in the text of a
name server exchange that failed; the stream of an HTTP/2 stream error and
the last stream of a `GOAWAY`; the time an expired or not yet valid
certificate was held against; the offset at which compressed data was found
corrupt; and, in the message of a gRPC status, that address, that time and
the size of a message over a limit, each only where it stands after the words
the client's own library leads up to it with: `read` or `write`, a TCP or UDP
network, and the address before `->`; `x509: certificate has expired or is
not yet valid: current time`; and `grpc: received message larger than max (`
or `grpc: message after decompression larger than max (`. A status message is
the target's as often as the client's: a value the target sent in it — an
address before `->`, a time, a size over a limit of its own — MUST still tell
failures apart, and MUST NOT be taken for the client's by its shape; only a
message that holds those very words has the value after them left out. The
same holds for the text of a name server exchange. Reading an error for
these parts MUST NOT fail the probe: an error of a shape that is not the
expected one, or one that panics when read, MUST be recognised by the whole of
its text. The address a connection went to, the
name server asked, the error code, the certificate's own time and the limit
MUST still tell failures apart. Of several
labels of a series that cannot be exposed, and of several labels a script gave
whose values are no text, the failure MUST name the first by
name, the same on every scrape, and not whichever the labels' map gave first.
The first success after a failure MUST be logged at info level with
the stage, `failed_for` and `failures`. This MUST apply to failed probes and
stages continuing under `log`, a rule failing under `error_mode: fail` or
`log`, probes
rejected by `max_concurrent_probes`, failed static target scrapes, failed files of a
directory, a directory over `max_files` or its listing bound, and repaired
invalid UTF-8. At most 10,000 failures MUST be remembered; when full, those not
reported for an hour MUST be forgotten, at most once a minute, and a new one
that finds no room MUST be logged every time. The failures of a collector a
reload removed or changed MUST be forgotten when the reload is made, and a
failure or success reported after the reload by a probe or scrape of a
collector the reload removed or changed MUST change nothing that is
remembered (§ 24.1a).

Every probe failure should include enough context to identify:

- collector
- target
- operation stage
- error

Example conceptual log:

```text
level=error
collector=legacy_application
target=https://example/status
stage=decode
error="invalid JSON at position 381"
```

Do not log credentials, authorization headers, or sensitive request bodies by default.

The failure log MUST tell targets apart by the target as given, not as it is
shown: two targets shown alike because a credential in their query is
masked are two targets, each with failures of its own. A probe MUST be told
apart by its parameters and forwarded headers too, and its lines MUST carry
its `url` label. A failure not reported for an hour MUST be forgotten, whether
or not the log is full: its return is a new failure, and its recovery after
the silence is not logged.

Two things the failure log tells apart MUST NOT be taken for one, whatever
their parts hold. They are: a probe, told by its collector, its target and
its parameters and forwarded headers; a static target, told by its collector
and its name; what a trip found at the address it went to — bytes that were
not UTF-8, lines a decoder left out, a directory over a bound, a directory's
file — told by the collector, the address and the file; a metric the static
targets endpoint left out; and of each, what failed: itself, a stale answer,
a skipped turn, or a rule, told as § 18.1 tells it. The key a failure is
remembered under MUST be that of two of them only when they are one, of one
kind and alike in every part, a part being any bytes: a probe's target may
hold NUL bytes, a directory or a file may be named `schedule`, `stale` or
`rule`, and a target may begin with the words `static target`. So no choice
of a target, an address, a file's name, a metric name, an expression or
`items` MUST make a failure of one a repeat of another's, or make the
recovery of one end another's failure.

---

# 26. Security

The exporter will be capable of fetching arbitrary URLs and executing configured Python code; therefore security must be considered explicitly.

Requirements:

- Never log credentials.
- Avoid SSRF escalation where reasonable.
- Consider configurable allowed URL schemes (`http`, `https`).
- Offer per-collector target allowlists and deny lists (§ 26.1).
- Send the collector's headers, credentials and request body, and present
  its TLS client certificate, only to the origin a request is made to: a
  followed redirect MUST NOT carry them anywhere else, unless the collector
  lists the host (§ 42.15c).
- Send a login's secrets only where the configuration's `request.login.url`
  says, held to the collector's lists, following no redirect; no probe
  parameter MAY change where a login goes (§ 42.6a).
- Protect against excessively large responses.
- Bound regex, jq, yq, and Python execution.
- Restrict Python networking/process/file capabilities.
- Do not install packages dynamically during probes.
- Do not accept arbitrary Python code through query parameters.
- Collector names MUST map only to server-side configured code/config.

## 26.1 Target allowlists and deny lists

An `http`, `graphite`, `grpc`, `sshfile`, `sql`, `dns`, `tlscert`, `ntp`,
`icmp`, `s3file`, `redis`, `tcp`, `ldap`, `sshexec` or `snmp` collector MAY set `request.allowed_targets`
and `request.denied_targets`, lists of host names, globs of host names
(`*` and `?`, `*` matching dots too, so `*.example.com` matches
`a.b.example.com` but not `example.com`), IP addresses and CIDR networks. An
entry that is none of these, such as one with a scheme, a port or a path,
MUST be refused at load; another request type — `localfile`, and
`kubernetes`, which takes no target — setting either MUST be refused as any
key of another type is. A `dns` collector's lists are matched against the
name alone, and a `PTR` target's against its address (§ 5.1, `dns`). An
`s3file` collector's lists name buckets, whose target is one (§ 5.1,
`s3file`), and are held to the rules of names alone. A name entry MAY hold letters, digits, dots,
hyphens and underscores, a hyphen first or last included. An entry MUST be
written in ASCII: one with any character outside it — an internationalised
name in its own letters, a full-width spelling, a space outside ASCII beside
the entry — MUST be refused at load, with an error naming the key, the entry
and the character and saying that an internationalised name is written in
its ASCII (`xn--`) form, and MUST NOT be read as the entry lower case or
trimming would make of it. The configuration schema MUST refuse such an
entry too. Names MUST be compared without case and
without a final dot, and an IPv4-mapped IPv6 address as its IPv4 address. A
host MUST be checked as it is dialed. A host is an IP address — an IPv6
address MAY carry a zone — or a name, and a name is made of ASCII letters,
digits, `.`, `-` and `_` alone. Such a name is dialed as
written, so it MUST be checked as written, in lower case and without a final
dot, and MUST NOT be refused for what a registered domain may not hold: an
underscore (`my_service`), hyphens in its third and fourth place
(`db--primary`) or a hyphen first (`-edge`).

The name judged MUST be the name the request is made to. For every request
of a collector — the first URL and every redirect followed, direct or through
a proxy, over HTTP/1 or HTTP/2, `http` or `https` — the host the lists
judged, the name looked up and dialed, the `Host` header or `:authority`,
the name the TLS handshake asks for, and the host in the request line or the
`CONNECT` a proxy reads MUST be one name, capitals and a final dot aside
(TLS names no address and no final dot), or the request MUST be refused
before anything is sent; only a `Host` header the collector sets or forwards
and its `tls.server_name` MAY replace that name, in the header and the
handshake. A host with a character outside ASCII — an
internationalised name in its own letters, or a name or an address in
full-width characters, with another dot (U+3002, U+FF0E, U+FF61), with a
combining mark, in capitals, or with a byte that is no UTF-8 — MUST therefore
be refused as a target is refused (`403`), whatever it would convert to,
whatever the lists are and with none set, naming the character and saying
that an internationalised name is written in its ASCII (`xn--`) form: the
HTTP client dials, and names in the handshake and in a `CONNECT`, the form
such a host maps to (`ｏrigin.test` and `origin。test` are `origin.test`) and
writes the `Host` header, the `:authority` and a proxy's request line in the
unmapped one (`xn--rigin-qr33a.test`, `xn--origintest-sh3i`), so a verdict on
either form is a verdict on a name part of the request is not made to. A
name in its `xn--` form is ASCII and MUST be judged and sent as it is
written, without being decoded or validated as punycode. A host that is not
an address and holds any other
character — `%`, which is how a URL's host carries one written `%25`, a
comma, a semicolon, `=`, `*`, a space, a control character — MUST be refused
as a target is refused (`403`), saying
which character, before anything is looked up or sent, whatever the lists
are and whether or not a proxy is in between: a proxy that decodes
`intern%61l.example` would fetch a name the lists never saw. Both rules MUST
hold for the first URL, for the host of every redirect followed and for a
`grpc` target in each of its forms. An IPv4 address written as one number, as fewer
than four parts, or with parts in hexadecimal (`0x`) or octal (a leading
`0`), as resolvers and proxies read them (`2130706433`, `127.1`,
`0x7f.0.0.1`), MUST be checked as the address it is, before anything is sent,
whether or not a proxy is in between.

- Every such collector MUST refuse the cloud metadata addresses,
  `169.254.169.254` and `fd00:ec2::254`, even with neither list set, unless
  `allowed_targets` lists the address or a network holding it; a name
  allowed by name that resolves to one MUST still be refused, and the
  refusal MUST say it is a cloud metadata service.
- A target MUST be refused when its host matches `denied_targets` by name, or
  any address it resolves to is in a denied network.
- With `allowed_targets` set, a target MUST be refused unless its host
  matches it by name, or every address it resolves to is in an allowed
  network. `denied_targets` MUST win over `allowed_targets`.
- An IPv6 address with a zone (`fe80::1%eth0`) MUST be checked as the address
  without its zone, against the lists and the metadata addresses alike.
- Names MUST be checked before the request and again for the host of every
  redirect followed, and so MUST a host written as an address, needing no
  lookup, before anything is sent. For a target reached directly, addresses MUST be checked
  against the address each connection is made to, without a lookup of the
  exporter's own before it, so a name is resolved once per connection and one
  that resolves elsewhere between lookups is still refused. For a target
  reached through a proxy, the environment's or the collector's
  `request.proxy_url`, the connection is the proxy's and MUST NOT be
  checked as the target's, nor the proxy's address held to the lists, which
  the configuration names; the exporter MUST resolve the target's name itself
  before the request and check those addresses. When that lookup fails, the
  request MUST go on to the proxy, which resolves the name, unless the
  collector's own lists hold an address or a network, in which case it MUST
  fail with an error naming the lists and the failed lookup; the name rules
  MUST apply either way, and a name `allowed_targets` does not match, when it
  holds names only, MUST be refused. The same holds for `grpc`. For an
  `http` collector with a `request.unix_socket` (§ 42.15b) no connection is
  made to an address of the host: the name rules and, for a host written as
  an address, the address rules MUST apply before the request and on every
  redirect, the name MUST NOT be looked up, and a name `allowed_targets`
  does not match by name MUST be refused, whatever networks the list holds.
  The
  documentation MUST say what this leaves unchecked: the address a proxy
  resolves a name to, the cloud metadata addresses included. Whether a request goes
  through a proxy MUST be decided by the proxy function of the transport that
  sends it, and for `grpc` as grpc-go decides, so the check and the
  connection never disagree. A `grpc` call MUST check its connections as
  they are made, looking the server up before the call only behind a proxy,
  and a connection through a proxy MUST keep grpc-go's own proxy dialing. A
  connection the policy refuses MUST be reported as the refusal, not as the
  `UNAVAILABLE` grpc-go makes of a dialer's error, whether the call or the
  reflection question before it made it. A refused request MUST NOT be
  retried. A connection to a host the request did not check, when nothing it
  checked goes through a proxy, MUST be refused. A UDP socket, an `ntp`
  request's, MUST be checked as a connection is, against the address it is
  connected to, before anything is sent.
- An idle connection MUST NOT carry a request of a collector whose lists
  differ from those of the collector that opened it: connections are checked
  once, when made, so collectors with different lists MUST NOT share a
  connection pool.
- A refused probe MUST be answered `403 Forbidden` naming the rule, whatever
  `error_handling.on_fetch_error` says, without contacting the target, and
  MUST be counted in `http_exporter_targets_refused_total`; a refused static
  target MUST fail in the `target_policy` stage and be counted there too.

The `collector` URL parameter selects configuration; it MUST NOT contain executable code.

---

# 27. Concurrency

The exporter MUST safely serve concurrent Prometheus probes.

Requirements:

- No global mutable scrape state that causes collectors/targets to interfere.
- Concurrent probes to different targets are allowed.
- Respect configured global and per-collector concurrency limits if implemented.
- Avoid race conditions during configuration reload.
- Serve the collector response cache safely under concurrent probes, and never
  hand a caller a metric set that another caller can mutate.

---

# 28. Suggested configuration examples

## 28.1 JSON + jq

```yaml
collectors:
  - name: app_json
    request:
      type: http
      method: GET
      path: /api/status

    decoder:
      type: json

    transform:
      type: jq
    metrics:
      - name: application_requests_total
        description: Total application requests
        type: counter
        error_mode: log
        labels: []
        expression: .requests
```

## 28.2 YAML + yq

```yaml
collectors:
  - name: app_yaml
    request:
      type: http
      path: /status.yaml

    decoder:
      type: yaml

    transform:
      type: yq
    metrics:
      - name: application_requests_total
        description: Total application requests
        type: counter
        error_mode: log
        labels: []
        expression: '.status.requests'
```

## 28.3 XML + XPath

```yaml
collectors:
  - name: app_xml
    request:
      type: http
      path: /status.xml

    decoder:
      type: xml

    transform:
      type: xpath
    metrics:
      - name: application_requests_total
        description: Total application requests
        type: counter
        error_mode: log
        labels: []
        expression: '/status/requests'
```

## 28.4 CSV

```yaml
collectors:
  - name: app_csv
    request:
      type: http
      path: /status.csv

    transform:
      type: csv
    metrics:
      - name: server_cpu
        description: Server CPU utilization
        type: gauge
        error_mode: log
        expression: cpu
        labels:
          - name: server
            expression: server
```

## 28.5 HTML + CSS selector

```yaml
collectors:
  - name: app_html
    request:
      type: http
      path: /status

    decoder:
      type: html

    transform:
      type: css
    metrics:
      - name: server_cpu
        description: Server CPU utilization
        type: gauge
        error_mode: log
        items: '#servers tr:has(td)'
        expression: 'td:nth-child(2)'
        labels:
          - name: server
            expression: 'td:nth-child(1)'
```

A simple tag extraction is also valid by setting `expression: h1` in a metric
declaration.

## 28.6 Prometheus input

```yaml
collectors:
  - name: vendor_prometheus
    request:
      type: http
      path: /metrics

    transform:
      type: prometheus
    metrics:
      - name: vendor_requests_total
        description: Vendor HTTP requests
        type: counter
        error_mode: log
        labels: []
        expression: '^vendor_requests_total$'
```

## 28.7 Plain text + regex

```yaml
collectors:
  - name: legacy_text
    request:
      type: http
      path: /status

    transform:
      type: regex
    metrics:
      - name: application_connections
        description: Current application connections
        type: gauge
        error_mode: log
        labels: []
        expression: 'Connections:\s+(\d+)'
```

## 28.8 Python pre-script with declared metrics

The preferred shape when Python is only needed to parse the response. The
pre-script returns structured data and the metrics are declared exactly as they
are for every other transform:

```yaml
collectors:
  - name: weird_vendor
    request:
      type: http
      path: /status

    transform:
      type: jq
      libraries:
        - lxml
        - python-dateutil

      pre_script: |
        import re

        data = {"workers": [{"name": m.group(1), "cpu": int(m.group(2))}
                            for m in re.finditer(r"Worker (\S+) CPU: (\d+)%",
                                                 response.text)]}
    metrics:
      - name: vendor_worker_cpu
        description: Worker CPU utilization
        type: gauge
        error_mode: log
        expression: .workers[].cpu
        labels:
          - name: worker
            expression: .workers[].name
```

## 28.9 Python transform

For collectors whose metric names are not known until the response is read. The
script emits through `metric(...)` and the `metrics` array is omitted:

```yaml
collectors:
  - name: dynamic_vendor
    request:
      type: http
      path: /status

    transform:
      type: python
      libraries:
        - lxml
        - python-dateutil

      script: |
        import re

        for line in response.text.splitlines():
            match = re.match(r"(\w+)\s*=\s*(\d+)", line)
            if match:
                metric(
                    name="vendor_" + match.group(1).lower(),
                    type="gauge",
                    value=float(match.group(2)),
                )
```

## 28.10 Status page: a status as a label

A status is text; it MUST be exposed as a single series per thing that has a
status, with the value `1` and the current status as a label. A status that
does not apply MUST NOT have a series — no `0` series for the other possible
values — so an alert selects on the label alone. When a status changes, its old
series ends and a new one starts. Counts, such as incidents by impact, are not
statuses: a count of `0` is a real value and MUST still be exported. The
per-component and per-group metrics MUST use `items` (§ 18.2), so each label is
evaluated against its own component and `$root` reaches the rest of the page.

`examples/config.grafanastatus.json-test.yaml` is the reference: a collector
for any Atlassian Statuspage page, demonstrated against
<https://status.grafana.com>, reading `/api/v2/summary.json`. It MUST expose:

| Metric | Labels | Value |
| --- | --- | --- |
| `statuspage_info` | `page`, `time_zone` | Always 1 |
| `statuspage_status` | `indicator` (`none`, `minor`, `major`, `critical`) | 1, one series, labelled with the page's current indicator |
| `statuspage_component_status` | `component_id`, `component`, `group`, `cloud_provider`, `cloud_zone`, `status` (`operational`, `degraded_performance`, `partial_outage`, `major_outage`, `under_maintenance`) | 1, one series per component, labelled with its current status; components that are groups are excluded; `group` is absent for a component in no group |
| `statuspage_component_incident_info` | `component_id`, `component`, `group`, `cloud_provider`, `cloud_zone`, `status`, `incident`, `incident_status`, `impact`, `message` | 1, only for a component that is not operational |
| `statuspage_component_group_status` | `group`, `status` | 1, one series per group, labelled with its current status |
| `statuspage_unresolved_incidents` | `impact` | Unresolved incidents with that impact |
| `statuspage_scheduled_maintenances` | `status` (`scheduled`, `in_progress`, `verifying`) | Maintenances in that status |
| `statuspage_next_maintenance_start_timestamp_seconds` | none | Start of the earliest scheduled maintenance; absent when none is scheduled |
| `statuspage_updated_timestamp_seconds` | none | When the page was last updated |

`cloud_provider` and `cloud_zone` MUST be parsed from the component name, which
on status.grafana.com takes the forms `AWS Ireland - prod-eu-west-6`,
`AWS Ireland - prod-eu-west-6: API`, `AWS East (VA) prod-us-east-1` and
`Azure US Central - us-central2`: the provider is the leading `AWS`, `Azure`,
`GCP` or `GCS`, as written, and the zone is the lowercase token, containing a
digit, that ends the name or precedes a `: service` suffix. A name that does not
start with a provider MUST leave both off the series. A test MUST cover every form, and
names that carry neither, including `Federal Cloud - AWS US Gov West`.

The page's name MUST appear only on `statuspage_info`. Prometheus labels every
probed series with its target (`instance`), which already distinguishes pages,
so the name on every series would only add bytes.

`statuspage_component_incident_info` MUST name, for a component that is not
operational, the most recently updated unresolved incident or maintenance in
progress or verifying that lists it, and carry that event's latest update (by
`display_at`) as `message`, with whitespace collapsed to single spaces and
`truncate: true` (§ 18.1) cutting it to `limits.max_label_value_length`, which
the collector sets to 300. Maintenance that is only scheduled MUST NOT count. A
component with no such event MUST still have the series, without the event
labels. The message MUST NOT be a label on `statuspage_component_status`: it
changes with every update, and each change would start a new status series and
reset any alert on the status, though the status itself had not changed.

Component names are not unique — status.grafana.com lists the same region under
most product groups, and a few names repeat within one group — so
`component_id` MUST be a label; without it the scrape fails on duplicate series.

It MUST be covered, as part of the opt-in external suite (§ 34.31a), by tests
against a trimmed capture of the real summary
(`testdata/json/grafana-status-summary.json`) that keeps grouped and ungrouped
components, repeated names within a group, a component in outage, an unresolved
incident with several updates, and maintenances both scheduled and in progress;
by a case with nothing scheduled; by a component down with only a scheduled
maintenance or nothing behind it; by a long, multi-line, non-ASCII update; and
by an opt-in external case (§ 34.31a).

---

# 30. CLI

Provide clear CLI flags, for example:

```text
--config.file=/etc/exporter/config.yaml
--web.listen-address=:8080
--web.self-metrics-path=/self-metrics
--web.static-targets-path=/static-targets
--python.path=/usr/local/bin/python3
--static-targets-file=/etc/exporter/static-targets.yaml
--config.watch
--config.watch-interval=60s
--config.schema
--config.collector-file-schema
--probe.timeout-offset=500ms
--probe.default-timeout=30s
--probe.max-concurrent=0
--python.max-workers=0
--python.max-idle-workers=1
--python.idle-timeout=2m
--runtime.memory-limit-ratio=0
--web.enable-lifecycle
--web.debug=off
--web.shutdown-timeout=15s
--web.shutdown-delay=0s
--version
```

`--runtime.memory-limit-ratio`, 0 by default or from 0.1 to 1, MUST set the Go
memory limit at startup to that share of the container's memory limit, and
log both. The limit MUST be read along the process's own cgroup, as
`/proc/self/cgroup` names it — v2 `memory.max`, and v1
`memory.limit_in_bytes` under the memory controller — from that cgroup up to
the root, the smallest found being the one in force, so it is found with a
cgroup namespace, where the path is `/`, and without one, where the root holds
the host's `max`. Without a container limit, at 0, or with `GOMEMLIMIT` set in the
environment, it MUST leave the limit alone, logging why for the first and
last. A value outside 0 to 1 MUST be a command-line error, also with
`--dry-run`, and so MUST one above 0 and below 0.1, with a message saying
the floor and that below it the Go runtime spends its time collecting
garbage: such a limit leaves the Go heap almost nothing.

`--web.shutdown-timeout` bounds how long a `SIGTERM` or `SIGINT` waits for the
probes in progress, 15 seconds by default, longer than a typical scrape timeout; a value that is not positive MUST be
refused as a malformed command line. The wait MUST be for the requests to the
exporter in progress alone — the probes, and any other request being
answered — and not for work the exporter started on its own: the static
target scrapes in flight are cut short as it begins (below), and the
configuration watch, the reload loop and the Python worker reaping stop then
and are not waited for. When it runs out, the connections still
open MUST be closed, which MUST be logged at `error` with the timeout and the
number of probes and of other requests given up on, before the last
OTLP export (§ 42.1a) and a clean exit, `0`.
The line logged as the graceful shutdown begins MUST say what it waits for —
the number of probes in progress, and of other requests when there are any,
in words and as the attributes `probes` and `other_requests` — and the
timeout, or, with nothing in progress, that there is nothing to wait for, in
which case the exporter MUST NOT wait. It MUST name the last OTLP
export only when OTLP export is enabled with an endpoint, the only case in
which one is sent. While the wait goes on, the exporter MUST log, every 5
seconds and no more often, what it still waits for and the time left, so a
shutdown run from a terminal never looks hung.

A second `SIGTERM` or `SIGINT`, from the first signal on, the delay below
included, MUST end the process at once, without waiting for the probes or
the last OTLP export, after logging one line that says so and how many probes
and other requests it leaves unfinished. The process MUST then end by that
signal's default action, so it reports itself killed by the signal (a shell's
`130` for `SIGINT`, `143` for `SIGTERM`) rather than exiting `0`; a process
started with that signal ignored, which the default action does not end, MUST
exit with that status, 128 and the signal's number, within a second. `SIGTERM` and
`SIGINT` MUST behave the same throughout.

`--web.shutdown-delay` is how long a `SIGTERM` or `SIGINT` keeps serving before
that graceful shutdown begins, 0 by default, which begins it at once. From the
signal on, `/ready` MUST answer `503` with the reason `the exporter is shutting
down`, while `/probe`, the self-metrics path and `/health` MUST
keep answering as before, so a load balancer or a Kubernetes Service stops
sending probes before the listener closes rather than having them refused.
Connections MUST NOT be kept alive past their current request during the
delay, so clients reconnect to another instance. The exporter MUST log the
delay when it starts one. A second signal during the delay MUST end the process
at once, as during the shutdown timeout, with the same line. A negative value MUST be refused as a
malformed command line.

Static targets (§ 42.14) MUST keep being scraped during the delay, while their
endpoint is still served. When the delay ends no static target scrape MUST
start, and one waiting for a slot MUST give up without logging advice about
`concurrency`; one that comes by a slot as the delay ends, or after, MUST NOT
begin either. Those in flight MUST be cut short as the delay ends, and MUST NOT
be waited for: what they would publish is served by this process alone, which
is stopping, and a target that does not answer would hold the shutdown for
all of `--web.shutdown-timeout`. One cut short
MUST publish nothing — on the endpoint or over OTLP, so the last OTLP export
goes without it — and MUST NOT be logged as
a failure, or at any level above `debug`: the target did not fail, so it MUST NOT be reported down, and its
last result stands.

`--version` prints the build information of § 22.0d and exits 0.

`--healthcheck` is a container health check for an image without `curl` or
`wget`. It MUST send one `GET /ready`, or with `--healthcheck=health` one
`GET /health`, to the exporter at `--web.listen-address`, and exit 0 when it
answers `200` and 1 otherwise: another status, a redirect, which it MUST NOT
follow, a refused connection, or no answer within 5 seconds. On failure it
MUST write one JSON line to stderr naming the URL and the status with the
answer's body on one line, or the error. A listen address with an empty host
or a wildcard, IPv4, IPv6 or IPv4-mapped, MUST be asked on `127.0.0.1`, which
a `[::]` listener answers too and which works on a host without IPv6, and any
other host as written, with an IPv6 zone escaped in the URL as `%25`; a named
port MUST be asked by its number. It MUST NOT bind the listen address, read the
configuration or static target files, start any loop, or send credentials,
since § 23's endpoints are never authenticated, and MUST NOT use a proxy from
the environment. A value other than `ready` or `health` MUST be a
command-line error, exit 2; the other flags MUST be checked as at a start, so
a container's own arguments can be given with it. The image MUST declare a
`HEALTHCHECK` running it, in exec form, at the listen address of its `CMD`.

`--config.schema` prints the configuration file's JSON Schema and exits, and
`--config.collector-file-schema` the schema of a collector file (§ 24.3).

`--static-targets-file` is optional and selects the static target document
defined in section 42.14, and `--web.static-targets-path` where its targets are
served (§ 42.14a). `--static-targets-file-schema` prints the document's schema
and exits, like the other schema flags.

`--python.path` selects the interpreter used by the `python` transform. It MUST
default to an interpreter resolvable through `PATH` and MUST accept an absolute
path so a deployment can point at a specific interpreter.

Optional:

```text
--log.level=info
```

Logs MUST always be JSON, one object per line (docs/LOGGING.md); there MUST
NOT be a flag selecting another format. No document MAY name an exporter flag
the exporter does not have.

The exact flag names can follow Prometheus ecosystem conventions.

### 30.1 Configuration check

`--dry-run` MUST validate what startup would load and exit, without binding the
listen address, starting the configuration watch or the OTLP loop, or contacting
any target. It exists so a pipeline, a pre-deploy hook or an init container can
ask whether a configuration would start without starting it.

The check MUST run the same validation functions startup runs, in startup's
order, and MUST honour the flags that change what startup loads:
`--config.file`, `--static-targets-file`, `--config.expand-env`,
`--static-targets.expand-env`, `--python.path`,
and `--config.watch` with `--config.watch-interval`. A malformed flag, such as
a `--web.listen-address` that is not `host:port` with a TCP port or a
non-positive `--config.watch-interval` with `--config.watch`, MUST be a
command-line error (exit 2) before the check. A configuration that
`--dry-run` passes MUST start with the same files and flags, and one it fails MUST
be refused by startup; a test MUST pin this agreement for every failure the
check can report.

The steps MUST be:

- `config` — the configuration file and its collector files (§ 5.0) load and
  validate, including the rule checks of § 24.2 and the uniqueness of collector
  names; the collector files read are listed under `details.collector_files`;
  each collector that took keys from its file's `defaults` (§ 5.0b) is listed
  under `details.collector_defaults` with the keys it took, in the block's
  order;
  each collector that sets `allowed_targets`, `denied_targets`,
  `accept_status`, `accept_codes`, `retry.statuses`, `retry.retry_after`,
  `pagination`, `login` or `rate_limit` is listed under `details.request_policies` with
  those settings as normalised for use, `retry.statuses` as
  `retry_statuses`, an empty list as `[]`, `retry.retry_after` as
  `retry_after: true`, a `pagination` with its defaults filled in, a
  `rate_limit` with its `burst` filled in, a `login` with the header
  and prefix its token is sent with filled in and the values of its
  `fields` and `endpoint_params` whose names read as credentials' withheld
  (§ 42.6a); every collector whose type takes `proxy_url` (`http`,
  `graphite`) MUST be listed there with how it connects: `unix_socket`, the
  socket's path, when it sets one, and else `proxy`, the proxy in force —
  its `proxy_url`, its own or from its file's `defaults`, `direct`, or
  `environment` when it names none — with `proxy_basic_auth_file`'s paths
  and never the contents of its files; the environment's proxy settings
  MUST NOT be printed, since a proxy URL there may hold credentials;
  deprecated spellings accepted during validation are listed under
  `details.deprecations` and logged, and do not fail the check; the metric
  lint's findings on the configuration (§ 30.1a) are listed under
  `details.lint`, each an object of `collector`, `rule`, `metric` and
  `message`, and each logged at WARN as `metric lint`, and do not fail the
  check;
- `python_scripts` — every Python script compiles and every pre-script produces
  `data` (§ 16), with each faulty script reported as its own error. A
  configuration without Python MUST pass without needing an interpreter;
- `config_watch` — only when `--config.watch` is set, reporting the interval
  under `details.interval` (a non-positive one never reaches the check);
- `static_targets` — only when `--static-targets-file` is set, that the file is
  valid on its own and against the configuration (§ 42.14).

The report MUST be a single JSON document on stdout:

```json
{"status": "ok|failed", "request_types": ["http"], "checks": [{"check": "...", "file": "...", "status": "ok|failed|skipped", "errors": ["..."], "reason": "...", "details": {}}]}
```

`request_types` MUST list the request types the binary was built with (§ 5.1),
so a configuration can be checked against the build that will run it.

A step whose input failed to load MUST be reported as `skipped` with a `reason`
rather than omitted, so a report never looks shorter because something went
wrong; the Python step is skipped when the configuration did not load, and the
target step when the target file is valid on its own but the configuration did
not load. A skipped step MUST count as not passing. An `ok` step SHOULD carry
`details` saying what it loaded — collector names, target names, the number of
scripts — so a passing report still says what it passed. The top-level `status`
MUST be `ok` only when every step is `ok`.

Every line on stderr MUST be JSON (§ 25): one log line per step, at INFO when it
passed, WARN when skipped and ERROR with its errors when it failed, and a final
summary line. `--log.level` MUST affect only the log, never the report.

The exit status MUST be `0` when the report's status is `ok` and `1` otherwise.
A command line that cannot be parsed checks nothing, so it MUST exit `2` —
distinguishing "your command is wrong" from "your configuration is wrong" —
and MUST report the problem as a JSON log line rather than plain text. `-h`
MUST print usage to stdout and exit `0`. An argument that is not a flag is
such a command line, at a start as with `--dry-run`: the exporter takes none,
and the flags after one would be left unread, so it MUST exit `2` with
`unexpected argument "<argument>"; flags take --name=value` rather than run
with half its flags.

### 30.1a Metric lint

The metric lint MUST be advice about the series a collector makes, given by
`--dry-run` (§ 30.1) and by every debug run (§ 42.17), and nowhere else. It
MUST NOT change `--dry-run`'s exit status or a check's status, nor what a
probe, a static target's scrape or a debug run answers, caches, counts or
exports. Each finding MUST name the collector, its rule by its id and the
metric by its exported name, with the collector's `metrics_prefix`. The
rules and their ids MUST be:

- `counter-total`: a rule of type `counter` whose name does not end in
  `_total`, since the exporter writes it without the suffix in the text
  format and with it in OpenMetrics (§ 21); and a rule of type `gauge`
  whose name ends in `_total`.
- `unit-suffix`: a name whose last word, or last before a counter's
  `_total`, is `ms`, `millis`, `milliseconds`, `us`, `microseconds`, `ns`,
  `nanoseconds`, `minutes`, `hours`, `days`, `kb`, `kilobytes`, `kib`,
  `mb`, `megabytes`, `mib`, `gb`, `gigabytes`, `gib`, `percent` or
  `percentage`, with the base unit and the scale that converts to it — `us`
  and `ns` only in a name with a duration word; else a name with one of the
  words `duration`, `latency`, `elapsed`, `uptime`, `age`, `time` or
  `timestamp` and neither `seconds` nor `ratio`; else a name with the word
  `memory` and neither `bytes` nor `ratio`. A name's words are its parts
  between underscores, in any case. No other word MAY make it fire.
- `unbounded-label`, in a debug run only: a label of a metric with more
  than 100 distinct values among the run's series, or else with a value
  that is a UUID, a hexadecimal string of 16 or more digits, a number of 10
  or more digits, or a date or an RFC 3339 time, quoted by its first 64
  bytes; once per label of a metric. The shape of a value MUST NOT be held
  against a metric of one series in the run, since one series cannot grow
  its metric's series within a scrape (an info metric's
  `revision="<git sha>"` is such a value); the count of distinct values
  MUST be.
- `name-clash`: a metric name a collector's rules or derived entries make
  that a rule or a derived entry of another collector makes with another
  type, another help where both have a `description`, or other label names,
  the rules' and `transform.labels` as `transform.remove_labels` and
  `transform.rename_labels` leave them, and for a derived `sum` with `by`
  the labels of `by` and `transform.labels` so left; reported for each of
  the two, naming the other. A side that cannot know one of these MUST NOT
  be compared by it: the names of a `python` script and of a `prometheus`
  rule without a `name`, the type of a `prometheus` rule without a `type`,
  the labels of a `prometheus` transform, and the labels of a derived
  entry's series by series operation (§ 18.1).

`--dry-run` MUST apply the rules to the names the configuration gives, a
rule's `name` and the `name` of each derived entry that is not `hidden`
(§ 18.1), of the `type` it is written with, `gauge` without one; and a debug run MUST apply them to those of its collector and
to the names of the series the run made too, showing a finding once.

A collector's `lint.ignore` MUST silence the rules it lists for that
collector, and MUST take the ids above only: another fails the
configuration, as any wrong value does.

---

# 31. Container image

Provide a production container image.

Requirements:

- Non-root runtime where possible.
- Minimal base image.
- CA certificates included.
- Python runtime and approved bundled Python libraries included if Python support is enabled.
- jq/yq dependencies available without requiring users to install them manually.
- Reproducible/pinned build dependencies.

The final runtime image MUST be self-contained for the documented supported feature set.

The runtime image MUST keep its known vulnerabilities to those without a fix:

- The build MUST run `apt-get upgrade`, so Debian security fixes land on the
  next rebuild rather than waiting for a new `python` base image.
- pip MUST be uninstalled once the bundled libraries are installed, together
  with the wheels `ensurepip` keeps. The exporter never installs a package at
  runtime (§32), so pip would only be attack surface, and its advisories would
  be reported against the image.
- Bundled Python libraries MUST be pinned at or above the first release that
  fixes a known vulnerability. `lxml` MUST be at least 6.1.0, which fixes
  CVE-2026-41066. That is a major version above 5.x, which the automated
  updater (§42.16) never crosses on its own.

Findings in Debian packages that have no fixed version yet cannot be fixed by
the image; they are resolved by rebuilding once Debian publishes a fix.

---

# 32. Dependency strategy for Python

Do NOT dynamically execute `pip install` during startup or scraping by default.

The supported Python dependencies should be bundled in the image and version-pinned at build time.

`libraries`/`required_libs` in collector config is used to declare/validate intended dependencies.

Document exact package versions in the project.

A future optional extension may support externally supplied Python environments, but this is out of scope for the initial implementation.

---

# 34. Testing requirements

Testing is a first-class project requirement. The implementation MUST include a comprehensive automated test suite covering unit behavior, decoder behavior, configuration validation, integration with an HTTP test server, Prometheus exposition, Python execution, Kubernetes/Prometheus Operator integration, Helm rendering, resource limits, concurrency, security restrictions, and failure modes.

The repository MUST be testable with a standard, documented command such as:

```text
make test
```

or equivalent.

The test suite MUST be deterministic and MUST NOT require access to real third-party services or the public internet.

It MUST also be repeatable: every test MUST pass when the suite runs more than
once in one process and in a random order, and `make test` and CI MUST run it
with the race detector, twice, shuffled and with a limit for each package of at
least twenty minutes (`go test -race -count=2 -shuffle=on -timeout 20m`), by
one command that is the same in both, `make test` over every package and CI
over each side's packages when that side's tests are to run (§ 34.33).
A test MUST NOT depend on state another test, or an earlier run of itself, left
behind. Process-wide state a test reads counts from MUST be replaceable per
test: the Python worker pool MUST be reached through one replaceable reference,
each test that uses it MUST get a fresh pool, and that pool's workers MUST be
stopped when the test ends.

What the runtime counts of allocations is process-wide too, and cannot be
replaced: the goroutines other tests left running and the collector allocate
while a test measures a function, and can only add to its count. A test that
bounds or compares allocations or allocated bytes MUST therefore measure them
through `internal/testutil/alloctest` and MUST NOT call `testing.AllocsPerRun`
or read the allocation counters of `runtime.MemStats` or of a benchmark's
result itself. A measurement there MUST run the collector and let the other
goroutines run before it counts, and MUST be repeated, up to five times, with
the least taken: until one is within the bound, where the test gives one, and
five times where it gives none; a single measurement is allowed only for a cost
the test takes a fraction of to bound another by. The race detector changes
what is allocated, so each such test MUST be measured under it too, and where
its bound does not hold there with room to spare the test MUST skip its
allocation assertions under the race detector and say so in its comment.

How much stack a call takes is counted for the process too, and a stack a
goroutine has outgrown is kept until a collection in progress has ended. A
test that bounds the stack a call takes MUST measure it with no collection
running: it finishes the one in progress and lets none start until the call
has ended.

Nor MAY a test depend on how fast the machine is. A test MUST wait for the
event it is about — a condition it polls, a hook, a channel — and not for a
length of time, under a bound that only ends a hang. Where a test runs
against one of the exporter's own timeouts, its values MUST be chosen so that
a slower machine makes the test slower and does not fail it: what must not
happen is given long, and what must happen has no bound but the hang's. A
test MUST NOT assert that a measured duration is below a value; it asserts an
order, a count, or a least duration. Where something has to have happened
when a deadline passes, the test ends the deadline itself once it has seen
what had to come first, or runs the code on a clock of its own that stands
still while the code is at work, or runs again with twice the time for as
long as the deadline passed too early, up to the bound that ends a hang; a
deadline that is on the machine's clock and is not tried again MUST leave
the earlier step a second or more. The tests that hold the static target
loop to its interval on the real clock MAY compare a time with a multiple of
that interval, each beside a test of the same schedule on a clock of its own.
Two things many tests meet are held to this in one place. The worker pool a
test runs Python scripts in MUST leave each script at least a minute,
whatever its `limits.script_timeout`, unless the test is of that timeout and
says so, and MUST leave an interpreter a minute to start in, unless the test
is of that limit, which then starts what never becomes ready. And a test
server that the exporter calls as a `grpc` target MUST listen on a port no
such server of the test process had before, since the exporter keeps a
connection per address and a reflection answer per connection, and the
kernel gives a freed port out again. A test that has the exporter do
something within its `--web.shutdown-delay` MUST keep the exporter in the
delay until that is done, and MUST NOT rely on the delay being long enough;
the wait for the delay MUST be replaceable for that, and nothing but a test
MAY replace it. And the two limits of ten seconds that tests not about them
run under — the TLS handshake with a target, and the time a client has to
send a request's headers to the exporter in — MUST be half a minute in the
tests: each MUST be settable for that, nothing but a test MAY set either,
and a test of the limit itself MUST set a short one against something that
never ends, a target that never answers the handshake or a client that never
ends its headers. The twenty seconds an attempt to connect to a `grpc` target
has (§ 5.1, `grpc`) MUST be half a minute in the tests in the same way, in
every package whose tests call a `grpc` server, with the waits between
attempts left the exporter's: its setter MUST be there in a build without the `grpc`
request type too, since the tests are set up by the same code whatever was
built; the test of the limit MUST connect to a target that never answers the
HTTP/2 preface, with the wait before the next attempt shorter than the
limit, since an attempt has the longer of the two; and a test MUST hold what
a connection is made with when nothing is set to the exporter's values, one
for one.

A test MAY work on a smaller input when the tests are built with the race
detector, under which the suite runs twice and several times slower: fewer
generated cases, a shorter body, fewer rounds, or a part of a table that is
multiplied out. It MUST take that size through
`internal/testutil/alloctest` (`UnlessRaced`, `RaceDetector`), so that a run
without the detector works on the full size. Under the detector such a test
MUST still assert everything it asserts without: it MUST NOT be skipped
there, an expectation that depends on the size MUST be computed from it or
given for both sizes, a floor on what a generated corpus held MUST be scaled
with the corpus and MUST NOT be dropped, an input that is there to pass a
bound of the code MUST still pass it, and a test of concurrency MUST keep the
goroutines it runs together. Where an input cannot be smaller, because it is
at a bound, and what takes the time there is an oracle the test compares
with, the test MAY hold the result under the detector to the answer the
oracle is known to give, provided a run without the detector compares with
the oracle itself and the test's comment says so.

This chapter holds the rules the test suite as a whole is held to. It
names no test, keeps no record of the tests a change added, and lists no
area's cases: what each test checks is said by its name and its comment,
beside the code it holds, and a requirement a test holds the code to is
written once, where that requirement is, in the section of the feature.

A test of the `grpc` request type MUST run against an in-process gRPC server on a loopback
listener, with its service compiled from `.proto` sources at run time, so
that it needs neither the network nor `protoc`.

The suite MUST have two tiers. The whole suite, which `make test` and CI
run on every change, MUST run every test. A quick tier, `make test-quick`,
for while a change is being written, MUST run each test once, without the
race detector, under `go test -short`, and MUST leave out the scaffold's
test and every test that takes a second or more, each of which
MUST say so as its first line by calling `tier.Slow`
(`internal/testutil/tier`), which skips a test under `-short` alone.
What gates a change — `make test`, `make ci` and every job of CI — MUST NOT
pass `-short`, and a test MUST hold them to that.

## 34.1 Test layers

Use at least these test layers:

1. Unit tests for isolated Go packages and components.
2. Decoder/transformer tests using in-memory responses.
3. HTTP integration tests using local `httptest` servers.
4. End-to-end probe tests exercising `/probe` and validating the returned Prometheus exposition.
5. Configuration/reload tests.
6. Helm rendering and manifest validation tests.
7. Optional Kubernetes integration tests for rendered manifests and Prometheus Operator resources when a test cluster is available.

Tests SHOULD use fixtures and golden files for representative input/output combinations.

What the tests of an area MUST cover are the rules of its sections:

- Every MUST and MUST NOT of a feature's section, and every SHOULD the
  exporter follows, MUST be held by a test that fails without it, its
  failure cases included: each kind of malformed input, each invalid value of
  each key refused when the configuration loads, a bound at its limit and
  one past it, and every mode of a setting — each `error_mode`,
  `on_decode_error` and `on_transform_error` — checked for the probe's
  status, the series it answers, what is logged and the self-metrics.
- A failure MUST be checked where its caller meets it: a probe failure
  through `/probe`, for its status, its text and that the process carries
  on; a reload through the mechanism that triggers it — the signal, the file
  watch or `/-/reload`.
- The common metric model (§ 7) MUST be tested apart from every decoder,
  and every combination of a decoder and a transform the exporter supports
  end to end through `/probe`, for an exposition a Prometheus parser reads,
  which the test SHOULD parse rather than only compare as text.
- The HTML decoder's tests MUST include malformed and legacy HTML, since
  scraping pages never meant for it is what it is for.
- The Python sandbox (§ 16.5) MUST be held by tests that try what it
  forbids — a socket, a subprocess, `os.system`, starting a process or an
  executable — and find it refused, not by reading the code; and each
  library the runtime ships (§ 16.3) by a script that imports it and uses
  what it is shipped for, so that a library missing from the image fails the
  suite.
- A credential — an `Authorization` header, a password, a token — MUST NOT
  appear in a log line, a probe's answer or a self-metric, and a test that
  sends one MUST check that it does not.

## 34.2 Required test tooling

The Go test suite SHOULD use standard Go tooling where possible:

```text
go test ./...
go vet ./...
go test -race ./...
```

The repository SHOULD also provide linting and static analysis, for example:

```text
gofmt -w / gofmt -d
staticcheck ./...
```

or an equivalent configured toolchain.

CI MUST run, on every change, every test the change can affect — the repository's own checks and the tests of each side, the exporter's and the target allocator's, whose packages or the files their tests read changed (§ 34.33) — and MUST fail on test, build, vet, lint, or required Helm validation failures.

## 34.25 Concurrency and race tests

The exporter MUST support concurrent probes safely.

Test:

- Many simultaneous probes to one target.
- Many simultaneous probes to many targets.
- Simultaneous probes while configuration reload occurs.
- Simultaneous probes for different collectors.
- Python transform execution concurrently.
- jq/yq execution concurrently.
- One cached XPath expression, computing a value and selecting nodes, with
  computed and selected labels, evaluated by many scrapes at once under
  `-race`.

CI MUST run:

```text
go test -race ./...
```

or an equivalent race-detection suite.

## 34.26 Fuzz testing

Add Go fuzz tests for parsers and other components where practical, especially:

- JSON input.
- YAML input.
- XML input.
- CSV input.
- HTML input.
- Prometheus exposition input.
- Regex/text extraction.
- Metric-name/label validation.
- Configuration parsing.

Fuzzing MUST verify that malformed or adversarial input does not cause panics, uncontrolled resource use within configured limits, or process crashes.

Maintain a documented seed corpus of representative difficult inputs.

## 34.27 Security tests

Test at minimum:

- SSRF protections according to the documented target policy: a denied
  name, glob, address or network, an address outside `allowed_targets`, a
  redirect to a refused host, and a connection made to a refused address
  after the check are each refused, and a refused probe is answered `403`
  without contacting the target, also for `grpc`.
- Target URL validation.
- Disallowed schemes if only HTTP/HTTPS are intended.
- TLS verification behavior.
- Credential redaction.
- Python no-network restriction.
- Python no-process restriction.
- Response size limits.
- Script execution limits.
- Cardinality limits.
- Malicious HTML/XML/text input.
- XML parser configuration against dangerous external entity behavior where applicable.
- Safe handling of redirects to unexpected schemes/hosts according to policy.

Security tests MUST be runnable without internet access.

## 34.30 Container/image tests

Test the built container image for:

- Process starts successfully.
- `/health` returns healthy.
- `/ready` returns the expected readiness state.
- `/self-metrics` exposes exporter metrics, and `/metrics` answers `404`.
- `/probe` is reachable.
- Default configuration loads.
- Python transform execution is available.
- Every documented bundled Python library imports successfully.
- No runtime package installation is required.
- Expected filesystem permissions are respected.
- The image runs as a non-root user named by number, `USER 65532:65532`, so
  Kubernetes can verify `runAsNonRoot`; a test MUST check that the image's
  `USER` is numeric and that the chart's `podSecurityContext` runs the same user.

The image build MUST be reproducible and MUST pin dependency versions sufficiently for production use. A test MUST check the Dockerfile for the image contents §31 requires: no BeautifulSoup, pip removed, Debian updates applied, and `LXML_VERSION` at or above 6.1.0. The Dockerfile MUST expose the Go base version, Python base version, and each bundled Python dependency version as `ARG` variables with documented defaults, so builds can override them without editing the Dockerfile. The build stage MUST declare its build arguments (`VERSION`, `REVISION`, `TARGETOS`, `TARGETARCH`, `REQUEST_TYPES`, `OPT_IN_REQUEST_TYPES`, `SQL_DRIVERS`) after `go mod download`, not before it: every `RUN` after an `ARG` depends on its value, and `REVISION` changes with every commit, so an earlier declaration would make the module download miss the layer cache on every build. A test MUST check that no build-stage `ARG` precedes the module download.

## 34.31 End-to-end scenario matrix

The repository MUST include at least one complete end-to-end scenario for every supported response decoder and transform:

| Scenario | Input | Decoder | Transformation | Expected result |
|---|---|---|---|---|
| JSON API | JSON | json | jq | valid Prometheus metrics |
| YAML API | YAML | yaml | yq | valid Prometheus metrics |
| XML API | XML | xml | XPath | valid Prometheus metrics |
| CSV API | CSV | csv | extraction/transform | valid Prometheus metrics |
| HTML status page | HTML | html | CSS/XPath | valid Prometheus metrics |
| Existing metrics | Prometheus | prometheus | filter/rename | valid Prometheus metrics |
| Legacy endpoint | Plain text | text | regex | valid Prometheus metrics |
| Scripted endpoint | Plain text/decoded data | text/json/etc. | Python transform | valid Prometheus metrics |

Each scenario SHOULD include at least one failure case and one optional/missing-field case.

## 34.31a External endpoint tests

The repository ships demo configurations that describe real third-party
services. A stub replaying a captured response cannot detect that one of those
services has renamed a field or a column, so the configuration keeps passing
every local test while no longer working.

The repository MUST therefore include tests that probe those endpoints for real,
and they MUST be opt-in and skipped by default: the rest of the suite is
local-only and deterministic, and a suite that fails when someone else's service
is down teaches maintainers to ignore failures. The opt-in MUST be explicit, and
the skip message MUST say how to run them. They MUST NOT run in CI for the same
reason.

Each case MUST assert that the probe succeeded, that every metric the
configuration declares is present, and that the per-row or per-entry labels
survived, so a source that changes shape is reported rather than silently
producing an empty scrape. One case MUST cover the response cache against a real
response.

A demo configuration's detailed tests against a captured response MAY belong to
the same suite, as the status page's do (§ 28.10): they then follow the same
rules even though they need no network. The suite's files MUST be listed in one
place, every test in them MUST be named `TestExternal*` so that
`make test-external` (`-run TestExternal`) selects it, and each MUST start by
calling the opt-in check. A test in the default suite MUST enforce both, by
reading the source of the listed files.

Whether or not its service is probed for real, every example under
`examples/` MUST be covered by the default suite, at any depth: an example
may be one file or a directory holding a configuration, its `config.yaml`,
with the static target file and the collector files that go with it. The
tests that check every example — against the committed schemas, that it
loads, and that its scripts satisfy the contract — MUST take their list from
the directory tree rather than from a file name pattern or a written list,
telling a configuration from a static target file by its content, and MUST
fail on a YAML file there that is neither. In a directory the configuration
MUST be `config.yaml` alone: any other file of collectors there or below is a
collector file, checked as its configuration loads it and never as an
example of its own, and the tests MUST fail on one the configuration's
`collector_files` does not name, and on a directory of YAML without a
`config.yaml`. An
example's static target file MUST be valid against the configuration in its
directory, as startup checks the two. An example of a request type a build
leaves out MUST be skipped, for that reason alone, in that build, and one of
an opt-in type MUST be loaded, and checked against the schema the build
generates, in a build with its type, as CI's build with every opt-in type
runs the repository's tests; every other example is checked against the
committed schemas. An example SHOULD also be run in the
default suite against a local stand-in answering in the service's documented
shape, asserting every series and the request the collector sent, as the
Filebeat, Open-Meteo, ECB, METAR, mempool.space, Prometheus demo, Frankfurter,
USGS, scrapethissite.com and tcp examples are.

## 34.32 Regression tests

Every bug fixed in the project MUST add a regression test reproducing the bug before or alongside the fix.

Regression fixtures SHOULD be named with a descriptive issue/reference identifier where appropriate.

## 34.33 CI quality gates

CI MUST enforce at minimum:

```text
gofmt check
golangci-lint run
go test ./...
go test -race ./...
go vet ./...
build
helm lint
helm template for required scenarios
manifest schema validation where configured
```

Static analysis MUST be configured in the repository rather than left to each
developer, so a local run and a CI run agree. The configuration MUST pin the
linter version used by CI. Formatting MUST be checked rather than silently
rewritten: a CI step that reformats files without failing never reports a
violation, so the check MUST fail on unformatted sources and the repository MUST
stay gofmt-clean.

The repository MUST validate its own CI workflow definitions in a check that
runs locally, not only in CI. A workflow file that GitHub cannot parse produces
no jobs, so a check defined inside that workflow cannot report the failure in
the commit that causes it. The validation MUST reject duplicate mapping keys,
which a permissive YAML parser accepts silently, and MUST verify that each step
declares exactly one of `run` or `uses`. Changes under `.github/workflows` MUST
be part of the changed-path filter that runs this validation.

Findings that cannot be acted on MUST be suppressed narrowly and with a stated
reason — a per-line annotation or a specific rule exclusion — rather than by
disabling a linter wholesale. Security findings for settings the exporter
deliberately exposes, such as the documented `insecure_skip_verify` opt-out and
reads of operator-supplied file paths, fall in this category.

CI SHOULD use changed-path detection to avoid running unrelated suites:

- The repository's own tests (`test/repository`), vet, formatting, lint and
  build run when anything the repository tests read changes (§ 42.12): Go
  source and module files, the workflows, the chart, `configs`, `examples`,
  `testdata`, the docs and READMEs, the Dockerfile, `.dockerignore`, the
  Makefile, `tools`, the lint configuration, `test/python`, the Dependabot
  configuration, `.gitignore`, the git hooks, `LICENSE` and the Artifact Hub
  repository file. A test MUST fail for a file a string literal of
  `test/repository`'s sources names, from the repository root, that this
  filter does not match.
- The tests of the packages are split in two sides: the target allocator's,
  `cmd/pue-allocator` and `internal/allocator`, and the exporter's, every
  other package of the module but `test/repository`. CI MUST run a side's
  tests, plain, under the race detector and, for the exporter, under each
  single-type selection, when a file that side's tests need changed: a file
  of a package of the module its packages and their tests are built from
  (`go list -deps -test`), whatever its build tags, with its embedded and
  `testdata` files, a path outside its packages' directories their tests
  open, or what CI runs them with (the module files, the workflow, the
  scripts listing the packages and the tags, and for the exporter the
  Python the Dockerfile names and `test/python`). The package lists MUST be
  computed, not written out, so a new package lands on a side by itself, and
  every package of the module MUST be tested by exactly one side or be
  `test/repository`. A test MUST keep each side's path filter matching every
  file the side needs and no entry matching none of them, and every file a
  side's filter matches MUST be matched by the filter the setup steps run
  under. `make ci` MUST still run every test of every side.
- Helm lint and template scenarios run when chart files, or the `configs` and
  `testdata/chart` files they render, change.
- A change to none of these MAY complete without running either suite.

The path filter MUST evaluate the correct comparison base for both push and
pull-request workflows. A tag push MUST run every step, whatever changed:
paths-filter compares a tag with its merge base on the default branch, which
for a tag on that branch's newest commit is the commit itself, so the steps
MUST run under outputs that are all true on a tag push and the filter's own
otherwise, and a test MUST run the step that sets them.

CI MAY run its checks in parallel jobs, and SHOULD run its slowest — the race
suite and the tests of each single-type selection — beside the rest rather
than after it. The linters (gofmt, golangci-lint and gopls) MUST run in a job
of their own, the builds (vet, the default build, darwin's and those with the
opt-in drivers and request types) in another, the image's build in a third,
which MUST keep the image's layers in the GitHub Actions cache between runs,
the exporter's race suite in two, one for `internal/exporter` and one for
the rest of the exporter's side, whose two package lists MUST together be
that side, each package once, and each
single-type selection's vet and build in that selection's job of the
single-type matrix, not in a loop in one job: the job that runs the
repository's checks and the exporter's tests MUST NOT also run them. Each job
of that matrix MUST also lint its selection's build alone with golangci-lint,
at the version and action pin of the linters' job, with the tags
`tools/request-type-tags.sh` prints for the type, under the `go` output: the
linters' job lints builds that carry every type, in which a helper only
another type's code or tests call is used, so a helper a single-type build
leaves unused is reported nowhere else. `make lint-request-types`, which
`make ci` includes, MUST lint the same builds, and a test MUST keep its loop
naming every request type of the tree. The scaffold test, which builds a
copy of the tree with new request types (§ 5.1), MUST run in a job of its own
under the `go` output, and the step that runs the repository's checks MUST
skip it; `make ci` MUST skip it and run it the same way. A test that a go
command of CI or of `make ci` skips MUST be run by another command there, by
the same pattern over the same packages, under the same condition, and a test
MUST fail for one that is not. Every job that runs Go checks MUST start with the same setup
steps (checkout, the changed-path filter, Go, the image's Python with its
libraries and the test-only modules, the module download) and the same
environment; a job that sets up and runs neither Go nor Python, as the
image's build, MUST start with the same first of them, checkout, the filter
and the step of the outputs its steps read, and nothing of the rest. Every
job MUST gate its steps on that filter's outputs rather than with
a job-level condition, so that a job with nothing to do still succeeds under
its own name; a test MUST keep those setups identical, and MUST keep the
matrix of the single-type tests naming every request type of the tree.

CI SHOULD additionally run fuzz smoke tests, image tests, and Kubernetes integration tests in a dedicated pipeline stage when infrastructure is available.

No feature should be considered complete if its relevant tests are absent or disabled.

## 34.34 Test fixtures and local development

The repository MUST include local fixtures for all supported formats under a clear test-data directory, for example:

```text
testdata/
  json/
  yaml/
  xml/
  csv/
  html/
  prometheus/
  text/
  python/
```

The project MUST document how to run the complete suite locally without external services.

The test suite SHOULD avoid time-dependent assertions and random network behavior. Where time is required, use injectable clocks or bounded assertions.

# 35. Documentation requirements

The repository MUST include documentation covering:

1. Architecture
2. Installation
3. Configuration
4. Collector authoring
5. Every supported response decoder and transform
6. jq examples
7. yq examples
8. XPath examples
9. CSS selector examples
10. Regex examples
11. Python transform API
12. Supported Python standard library
13. Supported bundled third-party Python libraries and exact versions
14. Unsupported Python libraries/capabilities
15. Error handling
16. `allow_missing_keys`
17. Resource/cardinality limits
18. Security model
19. ServiceMonitor example
20. PodMonitor example
21. Troubleshooting
22. Example collectors for JSON/YAML/XML/CSV/HTML/Prometheus/text/Python
23. Each request type other than `http`, on a page of its own: `localfile` in
    `docs/LOCALFILE.md`, covering its keys, which file is read, Prometheus and
    static target setups, formats, the node_exporter practices it follows,
    errors and self-metrics, and mounting files in Kubernetes
24. `grpc` in `docs/GRPC.md`: its keys, targets, the three descriptor sources
    with examples that load, the health check, the JSON mapping choices,
    placeholders, the status table and `retry.codes`, authentication and TLS,
    probe parameters, static targets, its self-metrics, gRPC-Web and Connect
    through `http`, and building without it, with the size it adds; its
    examples MUST be loaded and run by a repository test
25. Every other request type, each on a page of its own covering at least a
    collector's keys, the target, and static targets: `graphite` in
    `docs/GRAPHITE.md`, `sshfile` in `docs/SSHFILE.md`, `sql` in
    `docs/SQL.md`, `dns` in `docs/DNS.md`, `tlscert` in `docs/TLSCERT.md`,
    `ntp` in `docs/NTP.md`, `kubernetes` in `docs/KUBERNETES.md`, `s3file` in
    `docs/S3FILE.md`, `redis` in `docs/REDIS.md`, `tcp` in `docs/TCP.md`,
    `ldap` in `docs/LDAP.md`, `sshexec` in `docs/SSHEXEC.md`, `icmp` in `docs/ICMP.md` and `snmp` in `docs/SNMP.md`, with `localfile` and `grpc` above and `http` in
    `docs/CONFIGURATION.md` and `docs/REQUESTS.md`. A test MUST find, for
    each `internal/fetch/requesttype_<name>.go` but `http`'s, the page
    `docs/<NAME>.md`, named in this list and linked from the README's index
    and from the related pages of `docs/CONFIGURATION.md`
26. The target allocator, `pue-allocator`, in `docs/ALLOCATOR.md`
26a. A guide for a reader new to the exporter, `docs/GETTING-STARTED.md`,
    linked from the README's index: written plainly, in short sentences,
    it MUST take the reader from nothing to Prometheus scraping a first
    metric — running the exporter, a collector of a JSON API, a probe and
    its answer, Prometheus's scrape configuration of the probe pattern, and
    static targets — and its examples MUST be loaded and probed by a
    repository test, which MUST find the series the page shows. Beside it,
    `docs/HOW-DO-I.md`, linked from the README's index, MUST list tasks a
    reader comes with, each linked to the section of the reference that
    does it; its links, as every page's, MUST resolve.
27. Uptime probes of HTTP sites, in `docs/REQUESTS.md`, linked from the
    README's index and from `docs/CONFIGURATION.md`: the collector of
    `configs/config.uptime.example.yaml` — `report_failures`, `method:
    HEAD`, `follow_redirects`, `new_connection`, `accept_status: [any]`, `decoder.type: none`,
    and jq rules of `$status`, `$timings`, `$tls` and `$redirects` — the
    Prometheus job that relabels each site into `target`, what each series
    means, a table of blackbox_exporter's `http` prober series with what
    answers each here and how they differ, and what it does not do (ICMP,
    raw TCP, the rest of the chain). The example MUST start with the
    configuration schema's modeline, match the schema, and be loaded and
    probed by a test against local sites answering `200`, `503`, a redirect
    and TLS, and a closed port, which MUST find the series the page
    documents and the example lint clean

A list in the documentation that stands for every request type — the keys of
each type in `docs/CONFIGURATION.md`, what a static target of each takes in
`docs/STATIC-TARGETS.md`, the types label placeholders work with in
`docs/REQUESTS.md` — MUST be marked with a line `<!-- request-types: all -->`
before it and a line `<!-- /request-types -->` after it, and a test MUST
find every type of the tree, the opt-in ones included, named in each marked
list as `` `name` ``, or in the form the marker gives after `all`, such as
`"For `%s`"`. A list of the types of a default build, as the
`http_exporter_build_info` example in `docs/SELF-METRICS.md`, MUST be marked
`<!-- request-types: default -->`, and the test MUST find every type but the
opt-in ones in it, and no opt-in one. So a new request type fails the test
until each such list names it. The code layout of `docs/DEVELOPMENT.md` MUST
name every package `go list ./...` lists, and every file of the root
package that is not a test, which a test MUST check too.

A sentence that says which request types take a key MUST name exactly the
types whose registration in `internal/fetch` lists it, and a test MUST read
the registrations and hold the sentence to them: the types that take
`allowed_targets` in § 26.1, the targets that take their own credentials in
§ 42.14 and `docs/STATIC-TARGETS.md`, and the types that take the `tls`
block in `docs/TLSCERT.md`. The formats of § 1 and the comment on
`model.DecoderConfig` MUST name every decoder of `decoder.type`, and
`docs/ENDPOINTS.md` MUST have a row for every path the server routes, the
self-metrics and static targets paths at their defaults, each held so by a
test.

The Python documentation MUST explicitly state that networking is owned by the exporter and that `requests`/`httpx` are unnecessary.

The documentation MUST be split by audience rather than gathered in one file.
The repository README is what someone reads before deciding to use the exporter
and while starting it for the first time, so it MUST carry only what serves
that: what the exporter is, how the probe pattern works, a minimal configuration
and the commands to run it, a short account of the endpoints, and an index of
everything else. It MUST NOT carry reference material — decoder and transform
detail, caching, transport settings, authentication models, operator manifests,
the table of endpoints and the table of command-line flags — because a README
that answers every question stops answering the first one. The endpoints, with
what each takes and answers, MUST live in `docs/ENDPOINTS.md`, and every flag
of the exporter, with its default, in `docs/FLAGS.md`, which a test MUST hold
to exactly the flags the exporter has; the README MUST link both and
name `--help`. The target allocator's flags MUST live on its own page,
`docs/ALLOCATOR.md`, which `docs/FLAGS.md` MUST link.

What the exporter is MUST cover every source it reads, not the first one it
learned: the README's tagline and the sources paragraph after it MUST each name
every request type in the tree, and a test MUST find each
`internal/fetch/requesttype_<name>.go` named in both, failing for a request
type the test has no README name for, so a new type cannot ship while the
README still describes the exporter without it.

Each subject above MUST live in a dedicated page under `docs/`, and the README's
index MUST link every one of them. The Helm chart's own README remains the
reference for chart values, per [SPECIFICATION-CHART.md](SPECIFICATION-CHART.md)
§ 33.13, and the repository README MUST
link to it rather than restate it. Documentation that moves MUST move whole: a
subject MUST have exactly one home, so that a reader who follows a link is not
sent back to a shorter version of what they just left.

---

# 36. Extensibility requirements

New decoders must be addable without modifying:

- HTTP client
- Prometheus exposition layer
- ServiceMonitor/PodMonitor discovery model
- common error handling
- common validation

A new decoder should implement the decoder interface and register itself.

Keep decoder-specific dependencies isolated.

---

# 37. Non-goals for initial version

Do NOT implement these unless required to support the core design:

- Arbitrary outbound network access from Python
- Runtime package installation
- General-purpose data science environment
- Database access from Python (databases are the `sql` request type, § 5.1)
- Reading MIBs: an `snmp` collector names its objects by number (§ 5.1, `snmp`)
- Kafka
- Protobuf-encoded HTTP responses (gRPC calls are a request type, § 5.1)
- GraphQL client logic
- Long-running background jobs per target other than the scrape loops of static targets

A non-goal MUST NOT name what the exporter does: when a feature listed here
is implemented, its entry is removed or narrowed to what is still left out,
and a test fails on an entry naming a request type of the tree or static
targets. The architecture should leave room for future decoders and request
types, but every source stays one request type that turns what it reads into
Prometheus metrics.

---

# 38. Recommended implementation order

Implement in this order:

1. Go HTTP server and `/probe`, `/self-metrics`, `/health`, `/ready`.
2. Collector configuration and validation.
3. HTTP fetcher.
4. Common metric model.
5. Text + regex decoder.
6. JSON + jq.
7. YAML + yq.
8. Prometheus decoder.
9. XML + XPath.
10. CSV decoder.
11. HTML decoder + CSS/XPath.
12. Python runtime/decoder.
13. Error policies and missing-key semantics.
14. Resource/cardinality limits.
15. Self-metrics.
16. Config reload.
17. ServiceMonitor/PodMonitor examples.
18. Helm chart and Helm validation.
19. Container image.
20. Full integration tests.
21. Documentation.

The implementation may change this order when necessary, but all listed capabilities are required for feature completeness.

---

# 39. Acceptance criteria

The implementation is considered complete when all of the following are true:

- A Prometheus Operator `ServiceMonitor` can discover targets and invoke `/probe` with a configured collector.
- A `PodMonitor` can do the same.
- No target URLs need to be hardcoded into the collector configuration.
- A collector can select its HTTP method, path, headers, authentication, TLS, request body, and other request properties.
- A probe request can override a collector's method, path, body, target-request timeout, target TLS certificate verification, retry count, or retry backoff for one scrape.
- JSON responses can be transformed using jq.
- YAML responses can be transformed using yq.
- XML responses can be transformed using XPath.
- HTML responses can be transformed using XPath and/or CSS selectors.
- CSV responses can be parsed and transformed.
- Plain text responses can be parsed with regex.
- Prometheus exposition responses can be decoded into the internal metric model and transformed.
- Python can be selected directly as a decoder/transformation mechanism.
- Python scripts can import the documented bundled third-party libraries without installing packages at runtime.
- Python has access to the fetched HTTP response but not arbitrary network/process execution.
- Missing keys can either fail the scrape or be ignored/treated as optional according to configuration.
- Parse and transformation errors have configurable behavior.
- Resource/cardinality limits are enforced.
- Self-metrics clearly identify scrape/decode/transform failures.
- Metrics emitted by every decoder/transform combination have consistent Prometheus behavior.
- The complete project builds reproducibly and includes tests, documentation, and a working Helm chart.
- Every supported response decoder and transform has automated unit and end-to-end coverage, including representative success and failure cases.
- Python transform execution, bundled library availability, sandbox restrictions, and execution limits are covered by automated tests.
- Missing-key and configurable error-policy semantics are covered by automated tests.
- Concurrency is covered by tests and the race detector passes.
- Parser/configuration fuzz tests exist for applicable components and do not expose panics in the maintained corpus.
- CI runs the required Go, build, Helm, and manifest-validation gates.
- The repository contains local test fixtures for JSON, YAML, XML, CSV, HTML, Prometheus, text, and Python scenarios.
- `helm lint` and representative `helm template` configurations pass for the repository chart.

---

# 40. Preferred project identity

Use the project name:

```text
prometheus-universal-exporter
```

Recommended repository/chart naming:

```text
prometheus-universal-exporter
charts/prometheus-universal-exporter
```

The exact Go module path may be chosen during implementation.

---

# 41. Final design summary

The resulting system should behave as a generic Prometheus adapter:

```text
                    Prometheus
                         |
                 ServiceMonitor/
                    PodMonitor
                         |
                         v
              /probe?collector=X
                       &target=Y
                         |
                         v
                 +---------------+
                 | Request type  |
                 | http, grpc,   |
                 | sql, files... |
                 +-------+-------+
                         |
                         v
                      Response
                         |
                         v
                 +---------------+
                 |    Decoder    |
                 +-------+-------+
                         |
       +-----------------+---------------------------+
       |        |        |        |        |          |
      JSON     YAML     XML      CSV      HTML      Prometheus
       |        |        |        |        |          |
      jq       yq      XPath     ...     CSS/XPath   native
       |        |        |        |        |          |
       +--------+--------+--------+--------+----------+
                         |
                         v
                      Python
                   (peer option,
                not fallback-only)
                         |
                         v
                    MetricSet
                         |
                         v
                validation/limits
                         |
                         v
               Prometheus exposition
```

The request types and decoders drawn are a sample: every request type of
§ 5.1 hands the decoder its response, and every decoder of §§ 9–15a and
§ 6.0a — NDJSON, TOML, INI, logfmt, plain text and Graphite besides those
drawn — feeds the same transforms. A static target (§ 42.14) enters at the
request type, as a probe does.

The central invariant is:

> **Any supported response format must be convertible into the same internal metric representation, after which validation, limits, self-observability, and Prometheus exposition are format-independent.**

# 42. Dedicated self-health endpoint, OTLP, and deployment controls

The exporter MUST expose exporter self-health metrics separately from target
probe output. The dedicated endpoint MUST be configurable, for example:

```text
--web.self-metrics-path=/self-metrics
```

The default path MUST be `/self-metrics`, and the self-metrics MUST be served
at the configured path and nowhere else: one endpoint to scrape and protect,
with no alias. A deployment that wants the conventional `/metrics` sets
`--web.self-metrics-path=/metrics`. The path MUST be one fixed path of plain
segments — no query, fragment, trailing slash or pattern syntax — and a path
another endpoint uses (`/`, `/probe`, `/health`, `/ready`, `/collectors`,
`/-/reload`, `/debug`, `/debug/run`, `/debug/collector`, `/debug/otlp-test`) MUST be a command-line error (exit 2, before `--dry-run` or
startup) rather than moved aside.
The endpoint MUST NOT require `target` or `collector` query parameters and
MUST expose configuration, scrape, decode, transform, Python, limit, status,
duration, response-size, and emitted-series health metrics.

The chart values and monitors that expose this endpoint are specified in
[SPECIFICATION-CHART.md](SPECIFICATION-CHART.md) § 42.

## 42.1 OTLP export

The exporter SHOULD support optional OTLP/HTTP metrics export. When enabled,
configuration MUST include an OTLP endpoint and SHOULD support headers,
timeout, interval, TLS verification settings, service name, and resource
attributes.

`otlp.enabled` is the block's switch, and a block that sets any other key
MUST say it: `true` turns the export on, and `false` keeps the settings
without using or checking them. An `otlp` block with other keys and no
`enabled`, or an `enabled` left empty, MUST be refused when the configuration
loads, naming the block, the keys it sets, the line and both ways out —
settings without the switch would be ignored without a word, the endpoint
nothing is exported to. An empty block, `{}`, and none, need no switch; a
block written with no value at all is a key with no value, refused as
every such key is (§ 24.2). The same
rule MUST hold for `web.basic_auth` (§ 42.5), and for any block added later
that does nothing until an `enabled` key turns it on. The configuration
schema MUST say so, each other key of such a block requiring `enabled`
(`dependentRequired`). A key a merge key supplies counts as said (§ 24.2a),
unless the block sets it itself: an `enabled` the block leaves empty is the
one read, whatever a merged mapping says.

`otlp.interval` MUST control the export cadence and SHOULD default to 30
seconds. The interval MUST be that of the configuration in force: when a
reload, or an allocator's bundle (§ 42.14d), puts one in force, the next export
of the metrics, of the log records (`otlp.logs.interval`, § 42.1b) and of
the spans (`otlp.traces.interval`, § 42.1c) MUST be an interval of the new
configuration after the last export of each ended, and at once when that is
past, not when a wait armed with the interval before runs out. The exporter MUST buffer the latest general metric value for each
metric/label set between exports and include an exporter self-health snapshot
in each interval export. `otlp.timeout` MUST bound each export request and
SHOULD default to 5 seconds.

What an enabled export sends and connects with MUST be checked when the
configuration loads, by the checks a collector's request gets (§ 4), rather
than failing every export:

- `otlp.headers` names MUST be valid header names, none set twice in
  different case, and their values MUST hold no control character other than
  a tab; a name in `{{...}}` is no header name.
- `otlp.tls.cert_file` and `key_file` MUST be set together, and the files the
  `tls` block names — `ca_file`, `cert_file`, `key_file` — MUST be readable
  and hold a certificate or key, since the export needs them from its first
  request.
- `otlp.interval` MUST be at least `1s`, and `otlp.timeout` MUST NOT be
  negative; a shorter or negative interval MUST be refused naming the value
  and the least, not replaced by the default.
- `otlp.resource_attributes` MUST NOT set `service.name`, which
  `otlp.service_name` sets: the resource would carry the key twice. The error
  MUST point at `service_name`. The same MUST hold for a static target's
  `otlp.resource_attributes` (§ 42.14).

An attribute of `otlp.resource_attributes` written `""` MUST be the
attribute left out, as a `transform.labels` value written `""` is the label
left out (§ 6): the resource MUST NOT carry it. An export MUST NOT hold an
attribute without a value, on a resource or on a data point; a data point's
attributes are its series' labels, none of which is empty (§ 18.1).

Both metric classes MUST be exportable through the same OTLP exporter:

1. general metrics emitted by `/probe`; and
2. exporter self-health metrics from the dedicated self-health endpoint.

OTLP export MUST be best-effort by default. An OTLP destination failure MUST
NOT turn a successful Prometheus probe into a failed probe. OTLP requests MUST
have bounded timeouts and MUST NOT log authorization headers or credentials.

The implementation MAY use the OTLP/HTTP JSON representation. Gauge,
counter, histogram and summary data, labels, timestamps, descriptions, and
resource attributes MUST be preserved:

- A gauge MUST be an OTLP gauge, and a counter a monotonic sum with cumulative
  temporality.
- In the JSON representation every enum MUST be written as its number, as
  the OTLP/JSON encoding requires, never as its name: `aggregationTemporality`
  MUST be `2`, cumulative, and a log record's `severityNumber` a number.
- A histogram MUST be an OTLP histogram with cumulative temporality, its count
  and sum — without the sum, which OTLP allows, when it was read from a target
  without a `_sum` (§ 14.1) — and its buckets converted from Prometheus's
  cumulative counts to OTLP's per-bucket counts: the finite upper bounds, in ascending order, as
  explicit bounds, and one count more than bounds, the last being everything
  above the highest bound — Prometheus's `+Inf` bucket, which MUST NOT appear
  as a bound. The per-bucket counts MUST add up to the count.
- With `otlp.exponential_histograms`, a boolean that MUST default to false,
  the exporter's own duration histograms (§ 22) MUST be exported as OTLP
  exponential histograms instead, and nothing else: a histogram read from a
  target has classic buckets alone and stays an explicit-bucket histogram.
  Such a point MUST have cumulative temporality (`aggregationTemporality`
  `2`), its count, sum, `scale`, `zeroCount` (the observations of 0), its
  positive buckets as `positive.offset` and per-bucket `bucketCounts`, no
  negative buckets, and `min` and `max` once it has observed anything; the
  zero count and the bucket counts MUST add up to the count. Bucket `i` MUST
  hold the durations `v` with base^i < v <= base^(i+1), base being
  2^(2^-scale), mapped as OpenTelemetry's data model specifies: an exact
  power of two by its exponent, any other value as ceil(log2(v) × 2^scale)
  − 1. The recorder MUST start at scale 20 and keep at most 160 buckets, as
  OpenTelemetry's SDKs do by default, lowering the scale by as little as
  keeps every bucket within 160 whenever an observation would need more, by
  merging each 2^n neighbouring buckets into the bucket i >> n.
- A summary MUST be an OTLP summary with its count, sum and quantiles, and a
  summary without quantiles, such as `go_gc_duration_seconds` (§ 22.0a), MUST
  still carry its count and sum. OTLP's summary cannot leave either out, an
  unset field being 0, so a summary read from a target without a `_count` or a
  `_sum` is exported with 0 for it.
  A summary's quantiles MUST be exported in ascending order.
- A family with a series whose values its type does not allow — the value
  rules of § 21: a counter that is NaN or negative; a histogram whose `_sum`
  is NaN or negative or stands beside a negative bound, with a bound that is
  NaN, whose cumulative bucket counts fall or exceed its `+Inf` bucket's,
  whose `+Inf` bucket and `_count` differ, or that has neither; a summary
  whose `_sum` is NaN or negative, with a quantile that is NaN or outside 0
  to 1, or with a negative value for a quantile — cannot be a monotonic sum,
  a histogram or a summary in OTLP either, and MUST be exported as gauges
  under the names of its samples, with the series and values the text format
  writes: a counter as the gauge of its name; a histogram `h` as `h_bucket`
  with an `le` attribute, in ascending order and with `+Inf`, and `h_sum` and
  `h_count` where the series has them; a summary `s` as `s` with a `quantile`
  attribute, and `s_sum` and `s_count` likewise; `le` and `quantile` written
  as the text format writes them, over a label of the series' own by that
  name. Every series of the family in the export MUST be exported so, so that
  a name is one kind of metric in it; such a point MUST NOT be dropped for
  its values, and MUST NOT carry a start time. A family with no such series
  MUST keep its kind.
- A metric declared a histogram or summary but carrying no such data MUST be
  exported as a gauge of its value.
- Every series of a family MUST be a data point of one OTLP metric, not a
  metric of its own.
- NaN and the infinities MUST be encoded as the protobuf JSON mapping writes
  them — `"NaN"`, `"Infinity"`, `"-Infinity"` — since a JSON number cannot
  express them, and one such value MUST NOT fail the encoding of an export.
- A data point's time MUST be the time of its scrape or the
  timestamp its series carries, never the time of the export that sends it,
  and MUST survive the wait for the export and any retry. The scrape is the
  trip that fetched the value from the target: an answer from the response
  cache, fresh or stale (§ 42.13), of a probe or of a static target's scrape,
  MUST be exported as of the trip that filled the entry — the very time the
  trip's own export carried — not as of the answer. The series the exporter
  adds about the answer itself — `http_exporter_result_stale`,
  `http_exporter_result_age_seconds` and a static target's health metrics —
  MUST be as of the answer.
- A cumulative point — a counter's sum, a histogram, a summary — MUST carry a
  start time: the time of its series' first exported point, per resource, and
  of the first point after a reset, when its count or value went down. A
  series not exported for an hour MUST be forgotten and start again. The
  series remembered MUST be at most twice `otlp.max_pending_points` (its
  default when unset); past that, the series exported least recently MUST be
  forgotten first, and start again when it comes back. What is remembered of
  a series MUST NOT grow with its labels past 256 bytes of its resource,
  name, type and labels: a series that takes more MUST be remembered by a
  digest of them, SHA-256 or one as hard to make two series share, and a
  series MUST NOT have the start time of another. A gauge MUST NOT carry
  one. A cumulative point of one of the exporter's own series MUST instead
  start at the series' creation time (§ 22.1b), which the exporter knows, and
  need not be remembered.
- A resource MUST be told from every other by its service name and the name
  and value of each of its attributes, and a series of a resource by its
  name, its type and the name and value of each of its labels, whatever bytes
  they hold: a label's value is the target's and may be any text, an `=` or a
  NUL included, and a service name and an attribute's name and value are
  whatever the operator wrote. The series whose label `a` is `1`, a NUL and
  `b=2` is not the series whose labels are `a="1"` and `b="2"`, and the
  resource with the attribute `a=b` of the value `c` is not the one with the
  attribute `a` of the value `b=c`. Two resources that differ in any part
  MUST be exported as two, each with its own points and none of the other's,
  the exporter's own resource among them. Two series that differ in any part
  MUST both be exported: a point of one MUST NOT replace a waiting point of
  the other, and MUST NOT keep an undelivered point of the other from waiting
  again (§ 42.1a). Each MUST have its own start time and its own resets, also
  when the two are of different resources.
- An export MUST have one writer for each stream. To a receiver a data point
  is its resource's, its metric's — the name and the kind: gauge, sum,
  histogram or summary — and its attributes', and a name is one metric of
  one kind under a resource; what waits for an export is told apart as
  series, by name, type and labels, and two of those may be exported as one
  stream, or as one name of two kinds. One scrape's result has neither
  (§ 21); the results of two under one resource — two probes, a probe and
  a static target without a resource of its own, a probe and the exporter's
  own metrics — may. An export MUST NOT hold two data points of one metric
  of a resource with the same attributes, and MUST NOT hold two metrics of
  one name under a resource, by the rule of a waiting series, that the
  later replaces the earlier:
  - of the points that would be one metric's with the same attributes, the
    one written last MUST be exported and no other: a gauge and an untyped
    series of one name and labels; a sample of a family exported as gauges
    and a gauge of that sample's name and attributes, `le` or `quantile`
    among them; a probe's series and the one of the exporter's own it is
    named and labelled like. Nothing MUST be logged for it;
  - of the kinds a name of a resource would be exported as, the kind of the
    point written last MUST be exported, with every point of that kind, and
    no point of another kind, whatever the points' attributes, those of
    `otlp.probe_attributes` included. It MUST be logged as a warning that
    names the metric, the kind exported, the kinds and the number of points
    left out and the resource's service name, and says to rename a metric or
    to give a static target a resource of its own; as one repeated failure
    (§ 25.1) for the resource and the name, whichever kind is written last at
    each export, with no line when it ends;
  - of the Prometheus names, the names the exposition writes the points
    under, that a name of a resource would be exported for in the kind
    exported, which under `otlp.metric_names: otel` may be more than one —
    the counters `x_seconds` and `x_seconds_total` — the name of the point
    written last MUST be exported, with every point of it, and no point of
    another, whatever their attributes. It MUST be logged as a warning that
    names the metric, the Prometheus name exported, the Prometheus names and
    the number of points left out and the resource's service name, and says
    to rename a metric or to export with `metric_names: prometheus`; as one
    repeated failure for the resource and the name, apart from the name's
    clash of kinds, with no line when it ends.

  Written last is queued last, a point that waits again after a failed
  export keeping the place it was first queued with (§ 42.1a), and the
  exporter's own points, taken at the export, are written after every queued
  one. A point left out so is replaced, not dropped: it MUST NOT be counted
  in `http_exporter_otlp_points_dropped_total`. A cumulative point left out
  for another kind of its name MUST still be a point of its series to the
  start times, a count the series had: the series starts at the first point
  an export had of it, sent or left out, a count that fell in a point left
  out is a reset, and the series is forgotten an hour after the last. Two
  writers of one counter, histogram or summary — one name, type and labels —
  are one series to the start times: the lower count of one after the
  other's is a reset. A family is exported as gauges, as above, when any of
  its series in the export is one whose values its type does not allow,
  whichever writer's and whether or not that point is then left out. An
  export in which no two series can be one stream — every name of one type,
  no family exported as gauges, no series named like one of the exporter's
  own — MUST be what it was before this rule, byte for byte, and MUST be made
  in one pass over its series, as it was.

`otlp.metric_names` MUST name the export's metrics: `prometheus`, the
default and `""`, as the exposition writes them, with no unit; `otel` as
OpenTelemetry names them. Under `otel` a counter's name MUST lose a trailing
`_total`, and then a name, a histogram's or a summary's family name
included, MUST lose a unit suffix to the metric's `unit`: its last word after
a `_`, with something before it, when that is a unit the OpenTelemetry
Collector's Prometheus translator writes — `days` `d`, `hours` `h`,
`minutes` `min`, `seconds` `s`, `milliseconds` `ms`, `microseconds` `us`,
`nanoseconds` `ns`, `bytes` `By`, `kibibytes` `KiBy`, `mebibytes` `MiBy`,
`gibibytes` `GiBy`, `tibibytes` `TiBy`, `kilobytes` `KBy`, `megabytes`
`MBy`, `gigabytes` `GBy`, `terabytes` `TBy`, `meters` `m`, `volts` `V`,
`amperes` `A`, `joules` `J`, `watts` `W`, `grams` `g`, `celsius` `Cel`,
`hertz` `Hz`, `percent` `%`, `ratio` `1` — or its last three words when
they are such a unit but `ratio` and `percent`, `per`, and `second`,
`minute`, `hour` or `day`, which MUST be the unit `<unit>/s`, `/min`, `/h`
or `/d`. It MUST hold for the exporter's own metrics, the probes' and the
static targets' alike. A family exported as gauges under its samples' names
MUST keep those names and have no unit. Two names the translation makes one
MUST be one name to the rule of one writer below, a different unit making a
different kind of it, logged with the unit, and two Prometheus names of one
kind and unit two writers of the name, never merged into one metric. Any other value MUST be refused
when the configuration loads, in a block that is switched on.

`otlp.scope` MAY name the instrumentation scope of the export, its `name`
and `version`; left out, every metric and every log record (§ 42.1b) MUST be
under the scope named `prometheus-universal-exporter` without a version, as
before, and written, under the scope it names, the version left out where it
is empty. A `scope` written MUST have a `name`, and its name and its version MUST
NOT hold a control character; it MUST be refused otherwise when the
configuration loads, in a block that is switched on, by the exporter and the
schema alike. A static target MUST NOT set a scope of its own.

`otlp.exporter_instance` MAY name the exporter replica that sends the
export. It is a block `enabled` switches, as `otlp.logs` is (§ 42.1b): one
that sets another key without it MUST be refused, and one switched off, or
in an `otlp` block switched off, MUST be kept unchecked and MUST leave the
export what it was without the block, byte for byte. Switched on:

- Every resource the replica sends MUST carry the attribute
  `otlp.exporter_instance.attribute`, `pue.exporter.instance` when left out
  or written `""`, with its value: the exporter-wide resource of the probes'
  series and the exporter's own metrics, the resource of every static target
  exported over OTLP, with an `otlp` block of its own or without one, and
  the resource of the log records (§ 42.1b). It MUST be part of the
  resource's identity as every attribute is, and the rule of the writers of
  one resource MUST be unchanged.
- The value MUST be `otlp.exporter_instance.value` as written, `${NAME}`
  expanded with `--config.expand-env`; left out or `""`, the
  `--allocator.member` name in allocator mode, at each bundle and reload as
  at the start, and otherwise the host name. It MUST be resolved when the
  configuration loads, at the start and at each reload; a host name that
  cannot be read, or is empty, with no value written MUST refuse the
  configuration, saying to set the value. The target allocator, which is no
  replica, MUST leave the value as written.
- The attribute MUST NOT be blanks alone, `service.name` or a key of
  `otlp.resource_attributes`, written `""` or not, and neither the attribute
  nor the value MUST hold a control character; the configuration MUST be
  refused otherwise, by the exporter and, but for the keys of
  `otlp.resource_attributes`, the schema alike. A static target exported
  over OTLP whose own `otlp.resource_attributes`, written `""` or not, or
  `otlp.labels_as_resource` name the attribute, or that sets
  `otlp.instance_id` while the attribute is `service.instance.id`, MUST be
  refused naming the target. `service.instance.id` MUST otherwise be taken.
- The start's `starting exporter` line MUST carry
  `otlp_exporter_instance_attribute` and `otlp_exporter_instance` when the
  configuration it starts with has the block on, `--dry-run` MUST list them
  under `details.otlp_exporter_instance` of the `config` check, resolved as
  startup resolves them, and the status page's OTLP section (§ 42.14a) MUST
  show them.

Probe results MUST be queued under the exporter-wide resource, a series known
by its name, type and labels, so a later probe's point for the same series
replaces an earlier one's. With `otlp.probe_attributes: true`, every point a
probe queues MUST carry a `collector` attribute and, when the probe named a
target, a `target` attribute holding the target as logs show it, without
credentials, so probes of different targets or collectors are exported apart;
a label of the series' own by either name MUST be kept. Off, the default,
nothing MUST be added.

### 42.1a Delivery

Export requests MUST be gzipped, with `Content-Encoding: gzip`, unless
`otlp.compression` is `none`; `gzip` MUST be the default, and any other value
MUST be rejected when the configuration loads, in a block that is switched
on (§ 24.3).

An export MUST be sent in requests of at most `otlp.batch_max_size` data
points, 8192 by default, whose bodies hold at most `otlp.batch_max_bytes` of
JSON before compression, 4MiB by default; a negative `batch_max_size`, and a
`batch_max_bytes` under 64KiB, MUST be rejected when the configuration loads,
in a block that is switched on, 0 being the default of either. An export
within both MUST be the one request it would be without them. One over
either MUST be split in the order of its resources, their metrics and their
data points, each request taking as many as fit within both before the next
is begun; a request MUST carry the resource and the scope of each of its data
points, and the metric — name, kind, description — of each, written again in
every request a resource or a metric goes on in. A data point that is over
`batch_max_bytes` alone MUST be sent in a request of its own. The requests of
an export MUST be sent one after another, each with its own retries, and a
request after the first MUST NOT be started when the export has no time left
for an attempt.

A request that fails with a network error, `429`, `502`, `503` or `504` — the
responses the OTLP/HTTP specification makes retryable — MUST be retried with
exponential backoff, 1 second doubling up to 16, or after the delay a
`Retry-After` header gives in seconds or as an HTTP date. `otlp.timeout` MUST
bound each attempt. Retries MUST stop when another attempt could not start
within `otlp.interval` of the export's start, so one export never runs into the
next, and MUST stop at once when the exporter shuts down.

When the retries of a request run out, or no time is left to send it, the
export MUST stop, and the data points of that request and of every request of
the export not yet sent MUST be kept for the next export, except where a
newer value of the same series has been queued since; those of the requests
the endpoint accepted before it MUST NOT be kept, and the self-metric
snapshot MUST NOT be kept, since the next export takes a new one. A 3xx
response MUST NOT be followed (§ 42.15 rule 9): it is a misconfigured
endpoint, refused as below, and its warning MUST say that the redirect was
not followed, name `otlp.endpoint` as what to change, and carry the status
and, as `location_host`, the host its `Location` leads to (`no host` when it
names none), never a header's value. Any other
non-2xx response MUST NOT be retried, and the data points of that request,
and only those, MUST be dropped rather than kept, since the endpoint would
refuse them again; the export MUST go on with its next request. Each refused
request MUST be logged as a warning with the number of data points dropped
and the start of the response body, where the endpoint says why. Every
failed export MUST be logged as a warning with the retries made, and counted
(§ 22.0c).

A 2xx JSON response MAY carry a `partialSuccess` with `rejectedDataPoints`,
written as a string or a number, and an `errorMessage`, for the request it
answers. Rejected data points MUST be counted in `http_exporter_otlp_points_dropped_total` and logged as a
warning with the count and the message, and MUST NOT be sent again; the export
is still a success. A message with no rejected data points MUST be logged as a
warning. A response that is empty, not JSON, or has no `partialSuccess` is a
full success.

On `SIGTERM` or `SIGINT`, the export loop MUST keep running through
`--web.shutdown-delay`, as the static targets keep being scraped, and the
exporter MUST then stop accepting requests, let the probes in progress finish,
stop the export loop, and make one last export, bounded by `otlp.timeout`, of everything pending — including the data
of an export the shutdown cut short — with a last self-metric snapshot, before
it exits. Static targets MUST NOT be scraped again for it. When that last
export fails, there is no next export to keep its data points for: they MUST
be dropped, counted in `http_exporter_otlp_points_dropped_total` (§ 22.0c),
and the warning MUST say that they are dropped and how many; a failed export
before the shutdown MUST still keep its data points as above. The shutdown MUST
be logged, and once it has begun a second `SIGTERM` or `SIGINT` MUST end the
process at once, without waiting for the probes or the last export. A
`SIGHUP` MUST keep reloading until the process exits, and MUST NOT end it.

The data points waiting for export MUST be bounded by `otlp.max_pending_points`,
100000 by default, a negative value being rejected. Past it, the oldest MUST be
dropped — points kept from failed exports before any queued since, and among
those the older first, a point keeping its age through every failed export
that queues it again — down to nine tenths of the limit, so that dropping is
not repeated on every point; they MUST be counted (§ 22.0c) and logged as a
warning. Replacing the value of a series already waiting MUST NOT count as a
new point. A point waiting MUST keep, besides its labels, no more than 256
bytes to know its series by, a digest of a longer series as a start time is
kept by (§ 42.1), and its resource's points MUST be exported in the order
of their series' keys still. With `otlp.probe_attributes`, the points of one
probe MUST share one copy of its target attribute, and MUST NOT keep any
more of the probe's request than that copy.

### 42.1b The exporter's own log

With `otlp.logs.enabled` true in an `otlp` block switched on, the exporter
MUST export its own log lines as OTLP log records, besides writing them to
stderr as it does without it, and MUST NOT add a dependency for it.
`otlp.logs` is a block `enabled` switches (§ 42.1): one that sets another key
without it MUST be refused, and one switched off, or in an `otlp` block
switched off, MUST be kept unchecked, by the exporter and the schema alike.

- The records MUST be sent as OTLP/HTTP JSON `ExportLogsServiceRequest`s to
  `otlp.logs.endpoint`, which left out MUST be `otlp.endpoint` with its
  trailing `/v1/metrics` made `/v1/logs`; an `otlp.endpoint` without that
  path and no `otlp.logs.endpoint` MUST be refused when the configuration
  loads. The requests MUST carry the `otlp` block's headers, TLS and
  compression, be bounded by its timeout, be retried, refused and dropped by
  the rules of § 42.1a, and be split by `otlp.batch_max_size` records and
  `otlp.batch_max_bytes` of JSON. A redirect MUST NOT be followed.
- Each record MUST be under the resource the exporter's own metrics are
  under (`otlp.service_name`, `otlp.resource_attributes` and, switched on,
  `otlp.exporter_instance`, § 42.1), and the scope
  they are under (`otlp.scope`, § 42.1), and carry the
  line's time, the time it was taken as its observed time, the OTLP severity
  number of its slog level — DEBUG 5, INFO 9, WARN 13, ERROR 17, a level
  between them as far from theirs, within 1 to 24 — with the level's name as
  the severity text, the message as its body, and the line's attributes,
  each a string, an int, a double or a bool as its value is, and an
  attribute of a group named after the group and a dot. A value JSON has no
  number for, a whole number past an int, a duration, a time and an error
  MUST be strings. A record MUST have each attribute key once, as OTLP
  requires: of a key given twice — by `With` and by the line, or by a group
  and a name its dot makes alike — the value given last, where the key was
  first given. A value whose `Error` or `String` panics MUST be the string
  `<nil>` when it is a nil pointer and `!PANIC: ` and the panic otherwise, as
  slog's handlers write it, and the line MUST be logged and exported.
- Only the lines at or above `otlp.logs.level` — `debug`, `info`, `warn` or
  `error`, in any case, `info` by default, any other value refused — MUST be
  exported, whatever `--log.level` writes to stderr.
- Logging MUST NOT wait for the export: the records waiting MUST be bounded
  by `otlp.logs.max_pending_records`, 10000 by default, a negative value
  refused, and a line that finds the bound reached MUST be dropped and
  counted (§ 22.0c). They MUST be sent every `otlp.logs.interval`, 5 seconds
  by default and at least 1 second, in the background. What an export could
  not deliver for a reason worth retrying MUST wait for the next export,
  before the lines logged since, the oldest dropped past the bound.
- What the export logs of itself MUST NOT be exported, so that a failing
  collector cannot feed the export lines about itself: it MUST be written to
  stderr alone, and logged as a repeated failure (§ 25), once and then at
  most every five minutes, with a recovery line when an export gets through
  again.
- A reload that switches `otlp.logs` off MUST stop taking lines at once and
  drop those waiting; one that switches it on MUST take the lines logged
  from then on.
- At shutdown, after the metrics' last export (§ 42.1a), the records waiting
  MUST be sent in a last export within what is left of the same
  `otlp.timeout`, and at least a second; when it fails its records MUST be
  dropped and counted. No line logged after it is exported.
- The log export MUST NOT change readiness (§ 23), and its state MUST be on
  the status page (§ 42.14a).
- With `otlp.traces` on too (§ 42.1c), a record of a line logged during a
  probe or a scrape, its own or a shared trip's, MUST carry the trace's
  `traceId`, the `spanId` of the innermost stage span whose times hold the
  line's time, else the probe's or the scrape's span, and `flags` 1, the
  sampled bit, when the trace was exported by the time the export takes the
  record from its queue, and none of them otherwise: a trace dropped, or
  not yet decided, MUST NOT be linked, then or later. A stage's failure
  MUST be logged before the stage ends. A trace's ids MUST be made when the
  probe or the scrape begins, so that the line, which waits in the queue,
  is linked to the spans made of them at its end. The lines on stderr MUST
  carry no trace or span id.

### 42.1c Traces of slow requests

With `otlp.traces.enabled` true in an `otlp` block switched on, the exporter
MUST export a trace of each `/probe` request and each scheduled static
target scrape that took `otlp.traces.threshold` or longer, with
`otlp.traces.errors` of each that failed whatever it took, with
`otlp.traces.parent_based` of each probe whose caller sampled it, and of an
`otlp.traces.sample_ratio` share of the rest, at most
`otlp.traces.max_per_second` a second, and MUST NOT add a dependency for
it. `otlp.traces` is a block `enabled` switches (§ 42.1), as
`otlp.logs` is (§ 42.1b).

- The spans MUST be sent as OTLP/HTTP JSON `ExportTraceServiceRequest`s to
  `otlp.traces.endpoint`, which left out MUST be `otlp.endpoint` with its
  trailing `/v1/metrics` made `/v1/traces`; an `otlp.endpoint` without that
  path and no `otlp.traces.endpoint` MUST be refused when the configuration
  loads. The requests MUST carry the `otlp` block's headers, TLS and
  compression, be bounded by its timeout, be retried, refused and dropped by
  the rules of § 42.1a, a `partialSuccess`'s `rejectedSpans` dropped and
  counted, and be split by `otlp.batch_max_size` spans and
  `otlp.batch_max_bytes` of JSON. A redirect MUST NOT be followed.
- `otlp.traces.threshold` MUST be 1 second by default and at least 1
  millisecond; `otlp.traces.errors` MUST be off by default. A probe MUST be
  timed, and its span MUST end, when its answer is ready, before it is
  written, as its duration in the self-metrics is: the time its caller takes
  to read the answer MUST NOT count. A failed probe
  is one answered with an error, with a stale result, or turned away by the
  concurrency limit, or one of a collector with `report_failures` that
  `error_handling` carried on past a failed stage (§ 19.5); a failed scrape
  one that left its target `up` 0.
- Which trace to export MUST be decided at the probe's or the scrape's end,
  from stages and requests recorded in memory as it went, by the code that
  times a debug run's stages: each stage once, for both. A trace
  not exported MUST be forgotten, its spans never made. While the export is
  off, a probe MUST record nothing and allocate nothing for it.
- A trace MUST be a span for the probe — SERVER, named `probe <collector>`,
  with `http.request.method`, `url.path`, `http.response.status_code` — or
  for the scrape — INTERNAL, named `scrape <static target>` — with
  `collector`, `target` as the log shows it, `static_target`,
  `request.type`, `series`, `response.bytes`, `cache.hit` and, when it
  failed, `error.type` and the status ERROR with its failure; under it an
  INTERNAL span for each stage the debug report names (the fetch, `files`,
  `decode`, `transform`, `validation`), for the cache's answer, the wait for
  a slot, an identical probe's shared trip and a stale answer, a `python`
  span within the transform of the scripts' time, and under the fetch's a
  CLIENT span, named by its method, for each request sent — each attempt of
  `request.retry`, each redirect, each page of `request.pagination`,
  `request.login`'s login — with `http.request.method`, `url.full`,
  `server.address`, `server.port`, `http.response.status_code`,
  `http.request.resend_count`, `retry.attempt`, `pagination.page`,
  `redirect` and `login`, ERROR for a request that got no answer, or an
  answer its request type takes for a failure, or an HTTP answer of 400 or
  more, and a `rate_limit` span before a request that waited for its
  turn. Under an http, graphite or kubernetes request's CLIENT span (a
  listing's page, or a kubelet's summary) there MUST be an
  INTERNAL `http.first_byte` span from the request's start, when it asked
  for a connection, to the answer's first byte, and under it `http.dns`,
  `http.connect` and `http.tls` for the lookup, the connection and the
  handshake the request made, each only when it made it and none on a
  connection it reused, with the times `$timings` measures; a request that
  failed before its first byte MUST have the phases that went through under
  its own span. The phases MUST be timed while the trip is traced whether or
  not a rule reads `$timings`, and each request's MUST be its own, a
  redirect's not the request's before it, and a redirect the trip refused
  to follow, never sent, MUST have none. Under the CLIENT span of a tcp,
  tlscert, ldap, redis, sshexec, sshfile or grpc request there MUST be
  INTERNAL `connection.dns`, `connection.connect` and `connection.tls`
  spans, `connection.handshake` for an SSH handshake, for the connection
  made for it, each only when it was made: of tcp's and tlscert's request,
  whose connection is made for it; of the first command, operation or
  call sent on an ldap or redis connection, a redis Sentinel's and its
  primary's each their own, and `connection.tls` of ldap's StartTLS that
  of the STARTTLS operation; of the first request of the sshexec or
  sshfile scrape that began making its connection, none of another scrape
  that came to wait for it, and none on one kept; of the grpc call during
  which grpc-go made its connection, not one it made before the call, without
  `connection.dns`, the name's lookup being grpc-go's resolver's, and none
  on a connection kept or behind a proxy for `connection.connect`. A
  request's span MUST begin no later than the phases under it. Trace and span ids MUST come from `crypto/rand` and be written in
  lowercase hex; times MUST be decimal nanoseconds.
- A probe whose request carries a readable W3C `traceparent` MUST continue
  its trace: its span of the header's trace id, the header's span its
  parent; its sampled flag MUST keep the trace only under `parent_based`,
  and the other rules MUST decide it as any other.
- With `otlp.traces.propagate`, false by default, each HTTP request (a
  graphite one's, and each page's request of a kubernetes listing to the
  API server, too) and each gRPC call, as metadata, of a probe or a
  scrape MUST carry a W3C `traceparent` of its trace with a span id of the
  request's own, which its CLIENT span MUST then have, a redirect followed
  one of its own, and the `tracestate` its spans would carry if kept by the
  rule that has already kept it, else the caller's without a `th`. Its
  sampled flag MUST be `01` only where `parent_based` (the caller's flag)
  or `sample_ratio` (the trace id) has kept the trace when the request is
  sent, and `00` otherwise; a debug run's (§ 42.17), never exported, MUST
  be `00`. With the trace export off, a probe's caller's
  readable `traceparent`, and its readable `tracestate`, MUST be passed on
  as they came, no id made. A `traceparent` the request sets itself, in
  `request.headers`, `request.metadata` or a forwarded header, MUST be
  sent as it is, with no `tracestate` added; a `tracestate` it sets
  without a `traceparent` MUST be dropped, on an HTTP request and a gRPC
  call alike, wherever the exporter sends a `traceparent` of its own or of
  its caller's; a login's request MUST carry none. A trip shared by identical probes MUST be sent under the trace of
  the probe that started it, and only that probe's span of a request MAY
  have the id it was sent under. Without `propagate`, the exporter MUST
  NOT send a `traceparent` to its targets.
- A trace MUST be decided once, at its end, by these rules in this order,
  the first that keeps it naming why, and MUST be dropped when none does:
  `slow`, it took `otlp.traces.threshold` or longer; `error`, it failed,
  under `otlp.traces.errors`; `parent`, under `otlp.traces.parent_based`
  (off by default), the readable `traceparent` of its probe's caller has
  the sampled flag (`01`) set; `ratio`, its randomness is at least the
  rejection threshold `2^56 − round(sample_ratio × 2^56)`.
  `otlp.traces.sample_ratio` MUST be 0 by default, which keeps nothing by
  the ratio, and from 0 to 1, a value outside refused when the
  configuration loads; 1 MUST keep every trace. A trace's randomness MUST
  be the last 56 bits of its trace id — the caller's as it came, or the
  exporter's own from `crypto/rand` — or the caller's explicit randomness,
  `ot=rv` of 14 hex digits in its `tracestate`, where it sent one, so that
  the same trace id MUST be decided alike every time and as OpenTelemetry's
  consistent probability sampling decides it. There MUST be no
  per-collector or per-target rule.
- `otlp.traces.max_per_second`, 0 by default for no cap and refused when
  negative, MUST cap every trace the rules kept, slow and failed ones too,
  by a token bucket of as many tokens that fills at as many a second and
  starts full; a trace that finds no token MUST be dropped and counted as
  `rate`. A trace kept by `parent` or `ratio` MUST take a token only while
  half the bucket, rounded down, is left after it. The bucket MUST go by
  the exporter's clock and MUST NOT make a probe wait.
- The spans of a trace the ratio kept MUST carry, in `traceState`, the
  `ot` member with `th` the rejection threshold as 14 hex digits with the
  trailing zeros removed (`0` for 1), before the caller's `tracestate`
  members, the `ot` member's other fields kept and its `th` replaced, the
  members past 32 dropped from the right; those of a trace kept by
  `parent` MUST carry the caller's `tracestate` as it came; those of one
  kept as `slow` or `error` MUST carry it without a `th`. A `tracestate`
  that is not readable, or beside no readable `traceparent`, MUST NOT be
  propagated. With `parent_based` or `sample_ratio` on, the probe's or the
  scrape's span MUST carry `sampling.reason`, the rule that kept it; with
  neither, the export of a trace MUST be what it was without these rules,
  no `sampling.reason`, and no `traceState` without a caller's
  `tracestate`.
- A request's `url.full` MUST withhold its userinfo, every query value and
  its fragment, as an error in the log quotes a URL, and `target` the values
  of query parameters named like credentials, as the log shows a target; a
  span MUST carry no header and no body.
- A span MUST carry nothing the log does not. Of a request, a stage or a
  probe that went through it MUST carry no text the target sent — a tcp
  greeting, a command's stderr, an HTTP status line's reason, a request's
  outcome as a debug report tells it — but only numbers, yes-or-no
  answers and names of a set of the exporter's own that its request type
  works out of the answer, each attribute of its value's type (int,
  bool, double or string): for dns `dns.question.type`,
  `dns.answer.count` and, for a name not found, `dns.response.not_found`;
  for sql `db.response.returned_rows`, `sql.columns` and
  `sql.connection.reused`; for grpc `rpc.grpc.status_code`, the code's
  number; for ntp `ntp.stratum`, `ntp.offset_seconds` and
  `ntp.delay_seconds`; for ldap `ldap.result_code` of each operation the
  server answered and `ldap.search.entries` of each `SEARCH` page; for
  tlscert `tls.protocol.name`, `tls.protocol.version`, `tls.cipher`,
  `tls.server.not_after`, `tlscert.certificates`, `tlscert.verified`,
  `tlscert.ocsp.stapled`, `tlscert.ocsp.status` when a staple gives one,
  and `tlscert.leaf.days_until_expiry`; for redis `redis.reply.type` and a
  bulk string's `redis.reply.bytes` or an array's `redis.reply.elements`,
  never a reply's value; for sshfile `sshfile.files` and `sshfile.bytes`
  and for s3file `s3file.objects` and `s3file.bytes`, of the read that
  went through; for tcp
  `tcp.greeting.bytes`, `tcp.greeting.matched` and `tcp.answer.bytes`, for
  sshexec `process.exit.code`, `sshexec.stdout.bytes` and
  `sshexec.stderr.bytes`. Of a failure it MUST carry what the log line of
  that failure has: a failed stage's message, and that of the last request
  sent within a fetch stage that failed, MUST be the error the log line of
  the stage's failure has; an HTTP answer of 400 or more MUST say
  `received HTTP status <code>`; a request that failed and was sent again,
  or was followed by one that went through, MUST have no message, only its
  `error.type`.
- A probe's trace MUST be under the exporter-wide resource, a static
  target's under its own (`targetResource`, § 42.1), each with
  `otlp.exporter_instance` when it is on, and under `otlp.scope`, by the code
  that makes the metrics' resources and scope.
- A debug run MUST NOT be exported, counted or take a token of the rate
  cap; with the export on, the debug page's OTLP tab MUST show the spans
  the export would make of a run, by the export's own code, and whether a
  probe or a scrape like it would be exported, by which rule, or why no
  rule would keep it, with the ratio's randomness and threshold when the
  ratio was tried, and that the rate cap is not taken by a run; its
  endpoint test MUST offer the traces endpoint, sent
  `{"resourceSpans":[]}`. A run MUST have trace ids, which its requests
  carry under `propagate` and its request lines show, with the sampled
  flag `00` and a `tracestate` without a `th`, whatever the rules would
  make of a probe's, since a run's trace is never exported; its log records MUST
  show the trace and the span they would be linked to, saying that a run's
  are not, and its log export body MUST carry none.
- A probe MUST NOT wait for the export: the spans waiting MUST be bounded
  by `otlp.traces.max_pending_spans`, 10000 by default, a negative value
  refused, and a trace that finds no room for all of its spans MUST be
  dropped whole and counted (§ 22.0c). They MUST be sent every
  `otlp.traces.interval`, 5 seconds by default and at least 1 second, in the
  background; what an export could not deliver for a reason worth retrying
  MUST wait for the next export, before the spans queued since. The queue
  MUST be kept as whole traces and MUST NOT be cut inside one: past the
  bound, after a failed export or a reload that lowers it, the oldest
  traces MUST be dropped whole and counted, the rest of a trace an export
  delivered the start of being a trace of its own.
- What the trace export logs of itself MUST be written to stderr alone, as
  the log export's lines are (§ 42.1b), as a repeated failure (§ 25).
- A reload that switches `otlp.traces` off MUST drop the spans waiting and
  stop the recording; one that switches it on MUST record the probes and
  scrapes from then on. A probe or a scrape that ends after a reload
  switched the export off MUST NOT be queued, whatever the configuration it
  began under said, and MUST be counted nowhere, so that a later reload
  switching the export on again sends nothing of it.
- A trip shared by identical probes MUST record its spans apart from any
  one probe's: every probe that used it MUST have the trip's stages and
  requests in its trace, with span ids of its own — the probe that started
  it under its probe span, one that joined it under its `shared_trip` span.
  A probe whose caller went away before the trip ended MUST end its trace
  with the status ERROR, `error.type` `abandoned` and a message beginning
  `the caller went away`, saying whether the probe started the trip or
  waited for an identical probe's, and MUST have what the trip had recorded
  by then, each request not yet answered ERROR, with `request.unfinished`
  `true`, ending when the probe did.
- At shutdown, after the metrics' last export (§ 42.1a) and before the log
  records' (§ 42.1b), the spans waiting MUST be sent within what is left of
  the same `otlp.timeout`, and at least a second; when it fails they MUST be
  dropped and counted.
- The trace export MUST NOT change readiness (§ 23), and its state MUST be
  on the status page (§ 42.14a).

## 42.2 CSV transformation

CSV MUST be usable without CSS or Python for the common case. The CSV decoder
MUST normalize header-based records, and a first-class `csv` transformation
MAY map a numeric column to a metric and named columns to labels:

```yaml
metrics:
  - name: server_cpu
    description: Server CPU utilization
    type: gauge
    error_mode: log
    expression: cpu
    labels:
      - name: server
        expression: server
transform:
  type: csv
```

With `response.csv.header: false`, a `csv` rule and its labels MUST name
columns by number, from 1, and a name — or `0`, or a number written with a
leading zero — MUST be refused at load.

A response has a column when any of its rows does: with a header row, a
column the header names, which every row then has, empty where the row is
shorter; without one, a number up to the length of the longest row; and of
rows a pre-script left, a key any row has. A row that lacks a column other
rows have holds an empty cell there.

A rule whose value is in a column the response does not have MUST be missing
its value on every row, with an error that says so and lists the columns the
response has — the names in order, at most twelve of them and the number of
the rest, each cut as a quoted value is, or for rows read by number how many
the longest has (`CSV column "used" is not in the response, whose columns
are "Used", "Host"; column names are matched exactly`). A name that equals
the one asked for once the blanks around both are trimmed and case is folded
MUST come before the others, so that the cut does not hide the name that
says what is wrong. A cell that is empty
in a column the response has MUST be a missing value that names the row,
counted from 1 without the header's line (`CSV column "used" is empty in row
3`). Both are missing values: `required` and `error_mode` decide, as for any
other. The rows MUST NOT be gone through for a column until a row lacks one
a rule names, and then once for that column.

A label read from an empty cell MUST be left off that series. A label naming
a column the response does not have MUST fail its rule instead, once for the
response however many rows it has, with an error naming the metric, the
label, the column and the columns the response has (`metric "used" label
"zone": CSV column "zone" is not in the response, whose columns are "host",
"used"; column names are matched exactly`). It is a failure and no missing
value: a rule that is not required and a label that is not required MUST
fail alike, the rule's `error_mode` decides what follows, as for a label
that cannot be read in the `xpath` transform, and the rule MUST make no
series of that response. A rule that finds no value in any row does not read
its labels, and reports the value.

A pre-script of a `csv` transform MUST be given the rows as the decoder made
them and MUST leave rows: `data` a list, of which a row that is a mapping
MUST be read by its keys and a row that is a list by number, from 1, so a
script may drop rows, change cells and add columns. Anything but a list left
in `data` MUST fail the transform, with an error that says what the script
left and what the transform reads (`python pre-script of a csv transform
left data as an object with 1 key; it must leave a list of rows, each row a
dict by column name or a list by column number`), rather than one that
speaks of the response. A row that is neither a mapping nor a list MUST fail
the transform too, naming the row's number, from 1, and what it is, rather
than be read as a row missing every column. Both MUST be script failures,
counted in `http_exporter_script_errors_total`, as what a pre-script of a
`css` or `xpath` transform leaves that is no markup is.

CSS remains a first-class transformation for HTML responses. It MUST NOT be
required for CSV responses and MUST NOT be used as the CSV transformation
name.

## 42.3 Additional tests and documentation

Tests MUST cover CSV transformation, OTLP payload shape, OTLP timeout/failure
behavior, and the guarantee that OTLP failures do not fail Prometheus probes.
The chart rendering these tests are the counterpart to is covered in
[SPECIFICATION-CHART.md](SPECIFICATION-CHART.md) § 42.3.

## 42.4 PodMonitor/ServiceMonitor header and authentication propagation

The exporter MUST support a monitor-to-target propagation model for cases
where Prometheus authenticates the scrape of the exporter but the discovered
HTTP target requires the same credential. The model MUST be explicit and
collector-scoped:

1. A `ServiceMonitor` or `PodMonitor` MAY configure Secret-backed bearer or
   basic authentication for the scrape of the exporter using the Prometheus
   Operator `authorization` or `basicAuth` fields.
2. The exporter MUST NOT forward the incoming `Authorization` header unless
   the selected collector sets `request.forward_authorization: true`.
3. The exporter MUST forward a `header_<Header-Name>` endpoint parameter to the
   target only when the selected collector lists that header in
   `request.forward_headers`. An empty value MUST NOT be forwarded, as an
   empty `param_<name>` counts as not given. The chart value that produces
   those parameters is specified in
   [SPECIFICATION-CHART.md](SPECIFICATION-CHART.md) § 42.4.
4. The exporter MUST reject or ignore forwarding of transport and hop-by-hop
   headers, including `Host`, `Connection`, `Content-Length`, `Proxy-*`,
   `Transfer-Encoding`, `Trailer`, `Upgrade`, and `TE`, and MUST NOT forward
   `Accept-Encoding` (§ 20). `Authorization` MUST be controlled only by
   `request.forward_authorization`, not by the generic header allowlist. A
   `request.forward_headers` entry that names one of these headers, in any
   case and with blanks around it or not, or that is not a header name, MUST
   be refused at load, saying why it would never be forwarded, of an `http`,
   `graphite` and `grpc` collector alike; the forwarding and the check MUST
   go by one list of these headers.
5. Monitor header parameters MUST be documented as non-secret. Basic and
   bearer credentials MUST be supplied through Kubernetes Secret references,
   not URL parameters.

This follows the blackbox-exporter separation between monitor discovery
parameters and exporter-side request behavior: the monitor supplies the target
and module-like selection, while the collector configuration owns the target
HTTP request policy. The PodMonitor API does not provide an arbitrary header
map equivalent to Prometheus `http_config.http_headers`, so the
`header_<Header-Name>` parameter convention is the chart-compatible bridge.

The test suite MUST verify forwarding of an explicitly enabled Authorization
header, forwarding of allowlisted `header_*` values, and non-forwarding of
unallowlisted or transport headers.

## 42.5 Exporter endpoint Basic Authentication

The exporter MAY protect its own HTTP endpoints with opt-in Basic
Authentication configured independently of target collectors:

```yaml
web:
  basic_auth:
    enabled: true
    username: exporter
    password: change-me
```

`enabled` is the block's switch, and a `web.basic_auth` block that sets a
credential MUST say it, `true` or `false`: one that sets any other key
without `enabled` MUST be refused when the configuration loads, as § 42.1
says for `otlp`, since the credentials would protect nothing.

The username MAY instead be read from `username_file` and the password from
`password_file`, for a credential mounted from a Secret rather than written in
the configuration; exactly one of `username` and `username_file`, and one of
`password` and `password_file`, MUST be set. A file's content MUST be trimmed
of surrounding whitespace. A file that is missing, unreadable or empty when
the configuration loads MUST reject it. A username, written or read from
`username_file`, that holds a colon MUST reject the configuration when it
loads, naming the key, and MUST make every protected request fail with
`500`, logged, when a rotated file comes to hold one: a client's Basic
credential ends the username at the first colon (RFC 7617), so no client
could present it. A password MAY hold one. A file MUST be read again when its
size or modification time changes, so a rotated Secret takes effect without a
restart or reload; one that can no longer be read MUST make every protected
request fail with `500` rather than admit it, and MUST be logged as a
repeated failure is (§ 25.1): one error, the requests that fail after it at
debug level and at error level once per repeat interval, and the first
request that reads the file again logged as the recovery. It MUST NOT log a
line at error level for each request. The username and
password MUST both be compared in constant time whatever the first
comparison found, and the time taken MUST NOT depend on the length of either
the presented or the configured value: each MUST be compared as a digest of
fixed length, such as its SHA-256, with the digest of the configured value,
which MUST be made when the configuration loads or a credential file is read
again rather than for every request. Any other credential the exporter
checks on its own endpoints MUST be compared the same way.

When enabled, the exporter MUST require valid Basic Authentication for
`/probe`, the configured self-health metrics endpoint, the static targets
endpoint, the landing page at `/`, the collectors page at `/collectors` and,
when enabled, `/debug`, `/debug/run` and `/debug/collector`. The
`/health` and `/ready` endpoints SHOULD remain unauthenticated so Kubernetes
liveness and readiness probes can operate without credentials.

A browser sends the exporter's credential, once typed in, with every request
to the exporter, those a page of another site makes included, and a page of
another site can reach an exporter on the visitor's own network without
any. Every endpoint but `/health` and `/ready` MUST therefore answer `403`,
with a body saying why, to a request whose `Sec-Fetch-Site` is `cross-site`
or `same-site` (in any case), with Basic Authentication on or off, before
the credential is checked, a target contacted or a reload made. A request
whose `Sec-Fetch-Site` is `same-origin` or `none`, or that has none, as
Prometheus's scrapes and other clients that are not browsers do, MUST NOT
be refused for it. `/-/reload` MUST keep taking `POST` and `PUT` (§ 24.1a).
The `Origin` header MUST NOT be compared with the request's `Host`, which a
reverse proxy in front of the exporter may rewrite. In allocator mode, until the first bundle,
which carries `web.basic_auth`, is in force, every one of them MUST answer
`503` instead (§ 42.14e): what the credential protects MUST NOT
be served before the exporter knows whether one is required.

Exporter-side Basic Authentication MUST be mutually exclusive with the
Authorization bridge. Configuration validation MUST reject an enabled
`web.basic_auth` when any collector sets
`request.forward_authorization: true`. This prevents the same incoming
`Authorization` header from being interpreted both as credentials for the
exporter and as credentials to forward to a discovered target.

At process startup, this validation failure MUST be logged as an error and the
process MUST exit with a non-zero status before serving HTTP requests. During
configuration reload, an invalid conflicting configuration MUST be logged and
rejected while the last valid configuration remains active.

The test suite MUST verify unauthorized and authorized access to protected
endpoints, the `WWW-Authenticate` challenge, unauthenticated health/readiness
access, and rejection of the Basic Authentication/Authorization-bridge
combination.

## 42.6 Independent exporter authentication and target credentials

The exporter MUST support a configuration-only non-bridge mode in which:

1. exporter-side Basic Authentication protects incoming `/probe`, metrics, and
   self-health scrapes;
2. `request.forward_authorization` is disabled; and
3. the exporter supplies the target's bearer token from
   `request.bearer_token_file` or basic credentials from
   `request.basic_auth_file`.

The bearer token file MAY be populated by a projected or mounted Kubernetes
Secret. The exporter MUST trim surrounding whitespace, reject an unreadable
or empty token file, and send the resulting value only as:

```http
Authorization: Bearer <token>
```

For basic authentication, the exporter MUST support:

```yaml
request:
  basic_auth_file:
    username: /var/run/prometheus-universal-exporter/target-auth/username
    password: /var/run/prometheus-universal-exporter/target-auth/password
```

The exporter MUST trim the mounted username and password files and reject
missing or empty credentials. A username, of a collector's or a static
target's `basic_auth` or read from its `basic_auth_file`, MUST NOT hold a
colon, which ends the username in the Basic credential (RFC 7617), so that
the target would read a shorter username and a password starting with the
rest: one written in the configuration or the target file MUST be refused
at load, naming the key and not the value, and one read from a file MUST
fail the request before anything is sent, naming the file and not what it
holds. A password MAY hold one. The schemas MUST refuse a written one too.

Exporter credentials and target credentials MUST remain independent. The
incoming monitor's Basic Auth may authenticate the exporter, while the
exporter's configured bearer or basic credentials authenticate the underlying target. The
Authorization bridge MUST remain disabled in this mode, and configuration
validation MUST reject a collector that enables it alongside exporter Basic
Authentication.

Tests MUST cover bearer-token-file loading, basic-auth-file loading, whitespace
trimming, missing and empty files, independent exporter Basic Auth, and the
fact that the exporter Basic Auth credential is not forwarded to the target.

## 42.6a Logging in to a target

An `http` or `graphite` collector, and a static target of one, MAY set
`request.login`: a login made before the request, whose token MUST be sent
on it. A `grpc` collector MUST refuse it as a key of another type. Three
kinds MUST be supported, `steps` as § 42.6b says:

- `type: json` MUST post, with `Content-Type: application/json`, a JSON
  object of the members of `fields`, as written, and of `field_files`, each
  read from its file at every login and trimmed as a credential file is
  (§ 42.6), to `url`. `field_files` MUST hold one field at least, and a field
  MUST NOT be in both. The token MUST be read from exactly one of: `token`, a
  jq expression on the answer's JSON body with `$status` and `$headers` as in
  a rule, which MUST give text that is not empty; `token_header`, a header of
  the answer; `token_cookie`, a cookie the answer sets, which MUST be sent
  back as that cookie in the `Cookie` header. `expires_in`, a jq expression,
  MAY give the token's lifetime in seconds, a number above 0 or text of one,
  or `null` when the answer does not say.
- `type: oauth2` MUST ask the token endpoint `url` for a token with the
  client credentials grant (RFC 6749 § 4.4): `client_secret_file` is
  required, and exactly one of `client_id` and `client_id_file`; the id and
  secret MUST be sent as a Basic credential, or as form parameters with
  `auth_style: params`; `scopes` as the `scope` parameter, and
  `endpoint_params` as further parameters, which MUST NOT name `grant_type`,
  `scope`, `client_id`, `client_secret` or `refresh_token`. The answer's
  `expires_in` is the token's lifetime.
- With `grant_type: refresh_token`, `type: oauth2` MUST ask for a token with
  the refresh token grant (RFC 6749 § 6) instead, sending the content of
  `refresh_token_file` as `refresh_token`; `refresh_token_file` is required,
  `client_secret_file` MAY be left out, for a public client, whose id MUST
  then be sent as a form parameter, and `auth_style: header` MUST be refused
  without it. A refresh token the answer gives MUST be sent at the next login
  to the same endpoint, at the same virtual host, in place of the file's
  while the file's content is unchanged, kept in memory only and never
  written to the file; the login of a relative `url` at another target MUST
  send the file's until its own endpoint answers one. `refresh_token_file` MUST be
  refused without `grant_type: refresh_token`, and a `grant_type` other than
  `client_credentials`, the default, and `refresh_token` MUST be refused.

A key of another kind MUST be refused, and so MUST a login without `type`,
or, but for `steps`, without `url`, at load, naming the collector or the target and the key. A secret
MUST NOT be read from the configuration: it MUST be read from its file at
each login, so a rotated file is used by the next one. `--dry-run` MUST read
the login's secret files and the files of its own `tls` block, and those of
the collector's `tls` block or a static target's own, warning of one a
login or a request would fail on.

The token MUST be sent in `header`, `Authorization` when left out, after
`prefix`, `"Bearer "` in `Authorization` and nothing in another header when
left out. `header`'s name MUST read as a credential's, as the redaction of
§ 42.17 reads names, or be refused at load, so that whatever shows a
request's headers withholds the token. `token_cookie` MUST NOT be set with
`header` or `prefix`. A login sent in `Authorization` MUST be refused beside
the basic or bearer credential keys of the same collector or static target,
and a `request.headers` entry of the login's header MUST be refused. A
static target's `login` MUST replace the collector's, and the collector's
credential sent in the same header; a header the request carries in the
login's header — a static target's own credential or header, a probe's
forwarded `Authorization` or header — MUST be sent instead of the token, and
no login made for that request.

Where a login goes MUST be the configuration's alone. `url` MUST be an
absolute `http` or `https` URL of a scheme `allowed_schemes` allows, or a
path from the root (starting with `/`), with a query if need be, resolved
against the target's scheme, host and port alone: the target's path, query,
credentials and fragment MUST NOT play a part in where the login goes or in
which token is kept. A login whose `url` is a path MUST be sent with the
request's `Host` override, a collector's `request.headers` `Host` or a
static target's, the latter replacing the former, and one whose `url` is
absolute MUST NOT. A relative reference that does not start with `/`
(`login`, `../login`, `?x`) MUST be refused at load, and in the schema. It
MUST hold no credentials, fragment or placeholder, and a reference naming a
host without a scheme (`//host/...`) MUST be refused. The login request MUST be held to the
collector's `allowed_targets` and `denied_targets` (§ 26.1), and a login URL
they refuse MUST fail the trip as a failed login, not as a refused target. A
login MUST NOT follow a redirect. It MUST be made with `login.tls` when set,
and otherwise with the collector's `request.tls`, never with an
`insecure_skip_verify` a probe parameter or a static target sets for the
target.

A token MUST be kept, by the collector, the login block, the `tls` block the
login was made with (the login's own, a static target's or the
collector's), the URL the login was sent to and the `Host` it was sent with
(for a `steps` login, its first step's, and, when that is absolute and
another step's `url` a path, the target's scheme, host and port as well),
and sent on every request
until it is due to be fetched again: when a tenth of its life is left or 30 seconds, whichever is more,
but not before half of it has passed. Its lifetime is what the answer says,
else, for a token that is a JSON Web Token, its `exp` claim, read without
checking the signature, when it is after the login's start, else `ttl`, else until the target refuses it. Requests that need a token
while its login is made MUST wait for that one login, each within its own
deadline, and the login MUST NOT be cut short by the request that began it
going away or reaching its deadline: it MUST run within a bound of its own,
30 seconds, and a request that stops waiting MUST NOT fail it. A login that fails while the token kept has not expired MUST
leave that token in use, counted as a failure. A target that answers `401`,
or, with `relogin_on`, one of the statuses it lists in its place, each
`4xx` and anything else refused at load, to a request that carried the token — the last request of a followed
redirect, which MUST NOT be taken for a refusal of the token when a hop to
another origin left it behind — MUST have the token it was sent dropped, unless a newer one replaced it
meanwhile, and the request MUST be sent once more with the token of a new
login, whatever its method; that request MUST NOT be one of
`retry.attempts`, and MUST be made within the trip's deadline. At most 1000
tokens MUST be kept, the one used longest ago dropped first, and a reload
MUST drop the tokens of a login it changes or removes, or whose `tls` block
it changes.

A login that fails MUST fail the trip in the `login` stage, saying where it
went, with its query's values masked, and why: for an answer that is not
`2xx`, its status and the start of its body, at most 256 bytes, with what
reads as a credential masked (§ 42.17), and for `oauth2` the error code and
description RFC 6749 has the server send. The start of the body, the error
code and the description MUST be in the log, and with `--web.debug=full` in
a debug probe's report and on the status page, alone: the `502` a probe is
answered MUST say the status, for `oauth2` the error code only when it is
one RFC 6749 § 5.2 registers (`invalid_request`, `invalid_client`,
`invalid_grant`, `unauthorized_client`, `unsupported_grant_type`,
`invalid_scope`) and otherwise that the code is not one it registers, and
that what the server said is in the exporter's log, and MUST NOT hold any of
the body or the description. What the OAuth library says of an answer it
could not read as a token MUST be answered as that it could not be, the
library's words in the log alone; a connection that failed and an answer
without `access_token` MUST be answered as logged. A failure the login
cache hands to every request waiting for one login, or that the
collector's target policy refused, MUST be answered to each request's
caller as it would be alone. What the login sent MUST NOT be
shown anywhere: not its body, a file's content or the token. An error of a
`token` or `expires_in` expression MUST say the kind of value it gave and
never the value. A debug report MUST list a login request, marked as one,
with its headers redacted as every request's are and without its body. A
debug run (§ 42.17) MUST use the token kept and keep the one its login
gives, the collector's, but its logins MUST NOT be counted in
`http_exporter_logins_total` or `http_exporter_login_failures_total`, unless
a request that is no debug run's waited for the same login. A
static target's `login` MUST be part of its cache key, and the debug page
MUST refuse a collector written on it that sets `login`.

## 42.6b A login of several steps

`type: steps` MUST make the requests of `steps`, at least one and at most
16, in order, sharing one cookie jar of the login's own, each reading
values from its answer, and send the value `token` names as the token.

- **A step.** `url` MUST be held as a login's `url` is (§ 42.6a), the
  `Host` override sent with a step whose `url` is a path; it MUST hold no
  placeholder. `method` MUST be `GET`, `POST`, `PUT` or `PATCH`, `GET` when
  left out and `POST` with a body; a body with `GET` MUST be refused.
  `query` MUST be added to the URL's query, a parameter of it replacing
  one of the same name; `headers` sent, but `Host`,
  `Content-Type`, `Content-Length`, `Transfer-Encoding`, `Connection`,
  `Cookie`, `Accept-Encoding`, and `Authorization` beside
  `oauth1`, refused; `form` sent as `application/x-www-form-urlencoded` and
  `json` as a JSON object of text, not both. A step MUST succeed on a `2xx`
  answer, or, with `status`, on one it lists; it MUST NOT follow a
  redirect, and one not listed, whether `status` is set or not, MUST fail
  the login saying to list it. Each run of `steps` or `refresh_steps` MUST
  start with an empty cookie jar.
- **Values.** What a step sends MAY hold `{{login.<name>}}`: a value of
  `values`, as written, of `secret_files`, read from its file at each
  login, or read by a step before it; any other MUST be refused at load,
  naming the step and the key. A name MUST be letters, digits and `_`, not
  first a digit, given once. `extract` MUST read each value from exactly
  one of: `regex`, its first group or its whole match, on the body; `json`,
  a jq expression on the body, giving text or a number; `form`, a field of
  the body read as a form; `header`; `cookie`, whose `Max-Age` or `Expires`
  is the value's expiry, one the answer deletes, with `Max-Age` below 0 or
  an `Expires` past, not read. A value not found or empty MUST fail the login,
  naming the step and the value and never what the answer holds. The last
  step MUST read something, and `token` MUST name a value a step reads.
- **OAuth 1.0a.** `oauth1` MUST sign the step with HMAC-SHA1 as RFC 5849
  § 3.4 says, over the method, the URL, the query and a `form` body, with a
  fresh nonce and the time, in `Authorization`; `consumer_key` and
  `consumer_secret` are required, and `token` and `token_secret` go
  together.
- **Lifetime.** The token's lifetime MUST be, of the first that says one,
  the value `expires_in` names, in seconds, or `expires_at`, a time in
  seconds or milliseconds since 1970, RFC 3339 or an HTTP date, not both,
  an `expires_at` not after the login's start failing the login;
  the token's cookie's expiry; its JWT `exp`; `ttl`. A `form` value MUST be
  read among the pairs of the body Go reads, a pair it refuses, with a
  semicolon, not failing the step.
- **Refresh.** `keep` MUST name values of `steps`, each with a duration, 0
  for none, and MUST go with `refresh_steps`, which MAY send them and the
  given values, and MUST read `token`, and `expires_in` or `expires_at`
  when set; they MAY read a value kept anew, which no other given value
  may be. When a token is due, `refresh_steps` MUST be made while
  every value kept lasts — its `keep` duration from the login that read it,
  shortened by its cookie's or JWT's expiry — and the whole `steps` when one
  has expired or the refresh fails, which MUST be logged as a warning. A
  value `refresh_steps` read anew MUST replace the one kept; one they sent
  and did not read MUST keep its expiry. `keep` naming the token MUST be
  refused.
- **The keep file.** `keep_file`, an absolute path, MUST be written after
  each login with the token, its expiry and the values kept, under a
  digest of the login's key, whole, beside itself and renamed over,
  readable by the exporter's user alone, keeping the other logins it holds
  but those of which nothing would be used, their token and every value
  kept expired.
  A login MUST read it first, each time, and send a token it holds that is
  not the last one this exporter's logins gave while it lasts past its
  refresh margin, counted as no login; the values kept it holds MUST be
  used for `refresh_steps` when another exporter wrote them, the token not
  this one's last, or when this one keeps none, and this one's kept
  otherwise, so that a write that failed leaves no older values used. A file that cannot be written
  MUST leave the login made, logged as a warning, and `--dry-run` MUST warn
  of a directory that is not there.
- **Counted, traced and shown.** A login of `refresh_steps` MUST be
  counted in `http_exporter_login_refreshes_total` besides
  `http_exporter_logins_total`, but for a debug run's. Each step MUST be
  a login request in a debug report and a span of the trip's trace.
  `--dry-run` MUST show the steps with what they send masked as a json
  login's `fields` are, and an OAuth 1.0a secret written in them withheld.
  The states of a login MUST be dropped as its tokens are (§ 42.6a), and at
  most 1000 kept.

## 42.9 OTLP TLS configuration

The OTLP configuration MUST support HTTPS endpoints with optional custom
trust and client credentials:

```yaml
otlp:
  enabled: true
  endpoint: https://otel-collector.example/v1/metrics
  tls:
    ca_file: /etc/prometheus/tls/ca.crt
    cert_file: /etc/prometheus/tls/client.crt
    key_file: /etc/prometheus/tls/client.key
    insecure_skip_verify: false
```

`ca_file` MUST extend the system trust roots, while `cert_file` and `key_file`
MUST configure an optional client certificate for mutual TLS. Setting
`insecure_skip_verify: true` MUST disable server certificate verification only
when explicitly requested, and it MUST be the only key that does: the `otlp`
block MUST NOT have an `insecure_skip_verify` of its own beside `tls`. The exporter MUST retain TLS 1.2 or newer and MUST
not log certificate contents or credentials. The OTLP HTTP client MUST use the
same configured timeout and best-effort failure behavior as other OTLP exports.
`tls.server_name`, here and on a collector's request, MUST set the name the
server's certificate is checked against and sent as SNI.

## 42.10 Per-scrape request overrides

Collector target requests MAY configure TLS verification and trust material:

```yaml
request:
  tls:
    ca_file: /etc/prometheus/tls/ca.crt
    cert_file: /etc/prometheus/tls/client.crt
    key_file: /etc/prometheus/tls/client.key
    insecure_skip_verify: false
```

`request.tls.insecure_skip_verify` MUST default to `false`. When set to
`true`, only server certificate verification is disabled; TLS remains enabled
and the exporter MUST retain TLS 1.2 or newer. This setting SHOULD be avoided
unless the target's certificate cannot be validated through configured or
system trust roots.

`request.tls.server_name` MUST set the name the certificate is checked against,
and the SNI sent, for a target addressed by something its certificate does not
name. A `Host` in `request.headers`, or in a static target's, MUST be the
host the request is sent with, as Go sends only the request's own host and
ignores a `Host` header. A literal header value holding a control character
other than tab MUST be refused at load, collector and static target alike,
since it could never be sent. So MUST an inline `bearer_token`, sent as the
`Authorization` header or the `authorization` metadata, and an inline
`basic_auth` `username` or `password`, which RFC 7617 does not let hold one,
of a collector and a static target of every type that takes them, naming
the key and the character and never the value, and saying that a line break
at the end is usually one the Secret or the variable ended with and that the
key's `_file` form trims it. A credential file is read at the request and
trimmed (§ 42.6).

A `request.path` of an `http` or `graphite` collector MUST NOT hold `?` or `#`:
it is joined onto the target as a path, so they would be sent escaped. It
MUST be refused at load naming `request.query`, and so must a static target's
`request.path`; a `path` probe parameter holding either MUST be answered `400`.
A `localfile` path names a file and MAY hold both. A static `http` target
whose scheme `allowed_schemes` does not allow MUST be refused at load, and so
MUST a static `http` or `graphite` target whose URL's port is 0 or past
65535, naming the port and not the URL, which may carry credentials, as a
`grpc` or `sshfile` target's port out of that range is.

Joining `request.path` onto a target MUST keep the target's own escapes, so a
`%2F` inside a segment is sent as written rather than decoded into a new
segment. A header name that is not an HTTP token, and two names equal once
canonicalised, MUST be refused at load, in a collector and a static target. A
`tls` block setting only one of `cert_file` and `key_file` MUST be refused at
load.

The collector configuration MUST support a raw `request.body` value for
requests whose method accepts a body. The value MUST be sent as provided and
MUST NOT be restricted to JSON. The collector configuration MUST NOT contain a
`request.timeout` field.

The `/probe` endpoint MUST accept these optional query parameters for a
single scrape:

```text
method=<GET|POST|PUT|PATCH|DELETE|HEAD>
path=<request path>
timeout=<positive Go duration>
body=<raw request body>
insecure_skip_verify=<true|false>
follow_redirects=<true|false>
enable_http2=<true|false>
new_connection=<true|false>
ip_family=<ip4|ip6|prefer_ip4|prefer_ip6>
retry_attempts=<non-negative integer>
retry_backoff=<non-negative Go duration>
```

When supplied, these parameters MUST override the selected collector's
`request.method`, `request.path`, and `request.body`. The `timeout` parameter
MUST bound the target request with a child context of the incoming scrape
context. When it is absent, the target request MUST use the incoming scrape
context directly, so the Prometheus scrape timeout is the effective request
deadline. The `insecure_skip_verify` parameter MUST override
`request.tls.insecure_skip_verify` only for the current target request. Invalid
methods, timeout values, or boolean values MUST return HTTP 400 before the
target is contacted. Disabling certificate verification is an explicit
security trade-off and MUST be documented as unsafe for general use.

The chart values that render these overrides onto a monitor are specified in
[SPECIFICATION-CHART.md](SPECIFICATION-CHART.md) § 42.10.

## 42.10a Path parameters

A collector's `request.path` MAY contain placeholders bound from the probe, so
one collector serves targets whose paths differ only by a per-scrape value:

```yaml
request:
  path: /api/{{param_tenant}}/v{{param_version:2}}/status
```

A placeholder MUST be written `{{param_<name>}}` or
`{{param_<name>:<default>}}`, where `<name>` is one or more letters, digits or
underscores. It MUST be filled from the `/probe` query parameter of exactly the
same name, `param_<name>`. The shared `param_` prefix keeps these parameters
disjoint from the probe's own (`target`, `collector`, `path`, `method`,
`timeout`, `body`, `header_*`, the retry and transport overrides), so no name
needs to be reserved.

`{{` MUST always open a placeholder in `request.path`. A path that contains `{{`
not forming a well-formed placeholder — unclosed, a name without the `param_`
prefix or with other characters, or a default containing `{` or `}` — MUST be
rejected at startup and on reload with a message naming the collector. A default
containing a brace almost always means an environment reference left unexpanded,
and the message MUST say to run with `--config.expand-env`. A literal `}}` with
no opening `{{` is ordinary text.

For each placeholder, the value MUST be the probe parameter when it is given and
non-empty, otherwise the default when one is written, otherwise the probe MUST
fail with `400 Bad Request` naming the parameter, before the target is contacted
and before anything is counted against it. An empty probe value MUST be treated
as not given. A default MAY be empty, `{{param_suffix:}}`, and then binds
nothing.

A bound value MUST occupy exactly one path segment: it MUST be escaped as a path
segment, so that `/`, `?`, `#`, spaces and non-ASCII characters are
percent-encoded rather than changing the structure of the URL, and binding MUST
happen after the path is joined and cleaned, so that cleaning cannot rewrite a
value. A `%` in a value MUST be escaped too, as `%25`, whatever follows it:
only an escape written in the path itself is sent as written (§ 4). The values `.` and `..` MUST be rejected with `400`, since escaping cannot
make them safe and a server resolving them would serve a different path; a
default of `.` or `..` MUST be rejected at load, naming the collector and the
parameter. A request type that holds a value to more MUST hold a default to
the same, by the same check: a `localfile` path (§ 5.1) MUST reject at load a
default that is `.` or `..` or holds `/`, `\` or NUL, naming the collector,
the parameter and the default.

The probe MUST also be rejected with `400` when a `param_` parameter is given
more than once, or when nothing of the collector uses it: its path, the
places of § 42.10b, or a label value (§ 42.10c). The latter is
almost always a misspelling, and with a default in place a misspelled parameter
would otherwise succeed against the default and report one tenant's data as
another's. The refusal MUST name the places a placeholder of the collector
can stand in, which are those of its request type and its label values, and
no place its type does not have: the path, the body and the header and query
values of an `http` collector; the expressions, the path and the header and
query values of a `graphite` one; the message and the metadata values of a
`grpc` one; the path of a `localfile` one.

Path parameters MUST be bound only in the collector's `request.path`. A `path`
probe parameter replaces that path and MUST be used as given, and a `param_`
parameter sent with it that only the path used is unused and therefore
rejected.

Path parameters and environment references (§ 42.15a) MUST NOT overlap
syntactically. Environment references are expanded once, when the file is
read; path parameters are bound on every probe. They MUST compose, so a
default may be an environment reference expanded at load time:
`{{param_tenant:${DEFAULT_TENANT}}}`.

Every path parameter is part of the response cache key (§ 42.13), so two probes
differing only in a path parameter MUST NOT share a cache entry.

The verbose self-metrics `url` label (§ 22.1) MUST show a bound placeholder as
written, not its value. A value is typically a tenant or an account, which the
label already keeps out of the query string, and one series per value would be
unbounded.

A static target (§ 42.14) has no probe to supply a value; it MAY give
values under `params`, a map of `param_<name>` names to values, which fill the
collector's placeholders as the probe parameters of the same names would. Its
own values — `target`, `request.path`, `request.body` and header values —
MUST NOT contain a placeholder: they are sent as written, and one would reach
the target as text. The file MUST be refused when it loads, naming the target
and the field; in `request.path` any `{{` is refused, as in a collector's path,
and elsewhere `{{` followed by `param_`, spaces allowed between, so a JSON
body's own braces are not. The check MUST run after environment expansion
(§ 42.15a), so a placeholder a variable supplies is refused too. Every
placeholder of the collector — in its request's path, body, header and query
values, and in its label values (§ 42.10c) —
MUST be filled by `params` or a default, and every entry of `params` MUST fill
one and be a valid name; any other combination MUST be rejected at startup with
a message naming the target, the collector and the parameter. `params` MUST be
part of the target's cache key. A target's own `labels` are literal: a
placeholder there MUST NOT be filled.

## 42.10b Placeholders in the body, headers and query

An `http` collector's `request.body`, the values of `request.headers` and the
values of `request.query` MAY contain the placeholders of § 42.10a, filled by
the same probe parameters with the same defaults; a missing value, one given
twice and one no placeholder of the request or of a label value (§ 42.10c)
uses MUST be refused with `400` before the target is contacted. In these fields `{{` MUST open a placeholder
only when `param_` follows it, and `{{` followed by spaces and `param_` MUST be
rejected at load. Every `{{` that `param_` follows opens one, whatever
stands before it: a brace directly before a placeholder is text, so
`{{{param_x}}}` MUST be filled as `{`, the value and `}`, and
`{{{ param_x}}` MUST be rejected at load as `{{ param_x}}` is. A header
name or query name containing `{{` MUST be rejected at load. When the
`body` probe parameter replaces the body, its placeholders
MUST NOT be bound, and a parameter only they use is unused.

Each value MUST be written as its place requires:

- a header value MUST be refused with `400` when it contains a control
  character other than tab;
- a query value MUST be encoded as one query value;
- in the body, a placeholder MAY name a filter after its default,
  `{{param_name:default|filter}}`: `json` writes a JSON string, quoted and
  escaped; `number` writes the value only if it is a JSON number and MUST
  refuse it with `400` otherwise; `form` writes it URL form encoded; `xml`
  writes it as escaped XML text; `raw`, or no filter, writes it as given.

A filter MUST be rejected at load outside the body, and an unknown filter
anywhere. A default MUST be held to the rule of its place at load: one its
place refuses — a default that is no number under `number`, one with a
control character in a header or a gRPC metadata value, one a Graphite
expression cannot hold — MUST be rejected, naming the collector, the field
and the parameter, whether or not the field's other placeholders have
defaults. The values MUST NOT reach any self-metric label.

Placeholders are filled in the request's path (§ 42.10a), in the places
above and in the collector's fixed label values (§ 42.10c), and nowhere
else: a `{{param_...}}` in any other setting of a collector MUST be refused
at load, naming the field (§ 24.2).

## 42.10c Placeholders in label values

The two places a collector writes a fixed label value MAY contain the
placeholders of § 42.10a, filled by the same probe parameters: the values of
`transform.labels` (§ 6), and the `value` of a metric rule's static label,
under every transform whose rules take one, which is every transform but
`python` (§ 18.1). The same parameter MAY fill the request and a label.

```yaml
transform:
  type: jq
  labels:
    tenant: "{{param_tenant}}"
    region: "{{param_region:eu}}"
metrics:
  - name: status_up
    expression: .up
    labels:
      - {name: source, value: "api-{{param_tenant}}"}
```

A value MUST be read as a header value is (§ 42.10b): any number of
`{{param_<name>}}` and `{{param_<name>:<default>}}` among text of its own,
`{{` opening a placeholder only when `param_` follows it, and every other
brace text, a brace directly before a placeholder among them:
`{{{param_dc}}}` filled with `ams` MUST be `{ams}`. `{{` followed by spaces
and `param_`, a placeholder left open or named otherwise, a default
containing a brace, and a filter MUST be rejected
at load, naming the collector and the value; the refusal of a filter MUST
say that a label value is written one way. The `value` of a label that also
sets `expression`, a label's name and a key of `transform.labels` are not
such values, and a placeholder there MUST be refused (§ 24.2).

A value MUST be filled as given, with nothing of it escaped: what a label
value may not hold bare is the exposition writers' to escape. A
parameter's value that is not valid UTF-8 MUST be refused with `400` naming
the parameter; nothing else about its characters is refused. A default that
is not valid UTF-8 MUST be refused when the collector is checked; no
configuration file can hold one, since a document that is not UTF-8 is
refused whole.

The probe's rules MUST be those of § 42.10a. A placeholder with no value and
no default MUST answer `400` naming the parameter and the value, before the
target is contacted and before anything is counted against it; an empty
probe value MUST count as not given; an empty default binds nothing. A
`param_` parameter that fills a label value is used, whatever the request
names: it MUST NOT be refused as unused, and a `path`, `body` or `message`
probe parameter, which takes away the request's use of a parameter, MUST NOT
take away a label's. A parameter no place uses MUST still be refused.

A label whose filled value is empty MUST be left off the series. For
`transform.labels` it is the value written `""` (§ 6): no label is added, and
a label of that name the series has is kept. For a rule's label it is the
label the rule does not set: the series has no such label from the rule,
and a series a `prometheus` rule passes on keeps the one it has.

Everything that reads a label's value MUST treat a filled value as the same
text written in the configuration: `limits.max_label_value_length` and a
label's `truncate`, the `value_map` of the rule's name, `remove_labels` and
`rename_labels` in their order, `name_escaping` and `metrics_prefix`,
`limits.max_labels_per_metric`, and the check for a series made twice. The
answer of a collector written with placeholders, probed with their values,
MUST be the answer of the collector written with the values.

The checks that hold a collector's rules against each other when the
configuration loads (§ 18.1, § 24.2) MUST compare a label's value as it is
written. Two rules that differ only in the placeholders of a label's value
are two rules; a probe that fills them alike makes one series twice, and
that scrape MUST fail as a duplicate series does. The check of a static
value's length MUST measure a value that holds placeholders with each
replaced by its default, and, where a `value_map` of the rule's name maps the
label, as that map makes it, a `"*"` entry being what any value not listed
becomes (§ 24.2).

A label whose value holds a placeholder is a static label in what may stand
beside its value: `required` and a `value_map` of its own MUST be refused at
load (§ 18.1), each for a reason that is true of a value the probe gives.
The refusal of `required` MUST say that the value comes from the probe and
that `required` is for a label read from the response, not that the value is
always there; the refusal of `value_map` MUST say that the probe is to give
the value wanted, or that the `value_map` of a label of that name which
another rule of the metric's name reads with an expression maps it, not that
the value it maps to be written. A constant's refusals MUST read as they
did.

A static target's `params` MUST fill the placeholders of a label value as
they fill the request's, and the target file MUST be held to both at load
(§ 42.10a). Every `param_` parameter is part of the response cache key
(§ 42.13) and of what makes two probes identical (§ 42.13a), so two probes
that differ only in a parameter a label takes MUST NOT get each other's
series: from a fresh cache entry, from a stale one answering a failed trip,
or from one request shared while both are in flight. The values MUST NOT
reach any self-metric label.

Whether a collector's label values hold placeholders MUST be decided when
the configuration loads, once, and the parsed values kept with it; a probe
of a collector that has none MUST do no more for the feature than see that,
and MUST allocate nothing for it. A probe of a collector that has some reads
a copy of the collector's labels with the values filled in; the collector's
own configuration, which its probes share, MUST NOT be written to. The
collectors page (§ 23) MUST list a label's parameters with the request's.

## 42.10d Clock placeholders

A clock placeholder MUST be accepted wherever a `{{param_<name>}}`
placeholder is (§ 42.10a, § 42.10b), in every request type, and MUST be
filled, at every scrape, with the time of that scrape. It is written
`{{name[~last_success(fallback)][:offset][/unit][@zone]}}`, the parts after
the name optional and in that order, and, where a filter may stand, MAY end
with one (§ 42.10b).

- **Formats.** `name` MUST be one of `date` (`2006-01-02`), `date_compact`
  (`20060102`), `date_eu` (`02.01.2006`), `date_us` (`01/02/2006`), `month`
  (`2006-01`), `year` (`2006`), `time` (`15:04:05`), `datetime` (RFC 3339,
  seconds), `datetime_ms` (RFC 3339, milliseconds, always three digits),
  `unix` (seconds since the epoch) and `unix_ms` (milliseconds).
- **Offset.** `+` or `-` and one or more amounts of `w`, `d`, `h`, `m` and
  `s`, each at most 100000. Weeks and days MUST move the calendar date in
  the zone, so that `-1d` is the day before also across a daylight saving
  change; hours, minutes and seconds MUST be elapsed time.
- **Truncation.** `d`, `h` or `m`: the start of the day, hour or minute in
  the zone, taken after the offset.
- **A zone's changes of clock.** A placeholder MUST write the moment it is
  in the hour a zone's clocks show twice when its summer time ends, in
  both passes of it: its `unix` MUST be the moment's, its `datetime` MUST
  carry the offset in force, and the start of its hour and of its minute
  MUST be in the same pass. An offset of hours, minutes or seconds alone
  MUST be elapsed time from that moment. The start of a day MUST be the
  first moment of that date in the zone: the earlier midnight where the
  clocks are set back over midnight, and, where they are set forward over
  it so that the day has no 00:00, the moment they first show that date,
  never an hour of the day before.
- **Zone.** An IANA time zone name, `UTC` or `Local`; UTC when left out.
  The exporter MUST carry the time zone database, so that a name is read
  where the host has none. `datetime` and `datetime_ms` MUST write the
  zone's offset, `Z` for UTC.
- **Load.** A clock placeholder written wrong (an offset, a truncation or a
  zone not as above, or the parts out of order) MUST be refused when the
  configuration loads, saying what is wrong. A format its place cannot
  write (`date` under `|number`, a `date_us` in a `localfile` path, a
  value a Graphite expression may not hold) MUST be refused at load as the
  same default would be, naming the clock placeholder. A name of a path
  placeholder that is neither `param_<name>` nor a clock format MUST be
  refused listing the clock formats. Outside the path, `{{` MUST open a
  clock placeholder only where a format's name follows it directly, then
  `}}`, `~`, `:`, `/`, `@` or `|`; `{{ date }}` and `{{dates}}` stay text.
- **One moment.** Every clock placeholder of a trip, wherever it stands and
  in a static target's parameter values, MUST be filled from one reading
  of the clock, taken when the trip starts; a retry and a page after the
  first MUST send the values the first request sent.
- **Since the last success.** `~last_success(fallback)` MUST start the
  placeholder from the time (as above) of the last successful trip of the
  same collector, static target (none for a probe), target, `path` and
  `param_<name>` parameters, and, while none is known, from the trip's time
  moved by the fallback, which is required and is `-` and amounts as an
  offset's; the offset, truncation and zone MUST apply after either. A
  trip that fails, or carries on past a failed stage, a debug run and an
  answer from the response cache MUST keep no last success; a debug run
  MUST start from the one kept. The times MUST be kept in memory, for
  10,000 keys at most, the one kept longest ago forgotten past that, and a
  later time MUST NOT be replaced by an earlier one.
- **No probe parameter.** A clock placeholder MUST NOT be filled from a
  probe, MUST NOT make a probe parameter required or used (a `date` probe
  parameter is no `param_<name>` one), and MUST NOT be listed among a
  collector's parameters on the collectors page (§ 23).
- **A static target's parameters.** A value of a static target's `params`
  MAY hold clock placeholders, with text around them, filled in at each
  of its trips; the target's own value MUST NOT be written over. A value
  with a `{{param_<name>}}` placeholder, a filter, a clock placeholder
  written wrong, or a clock placeholder in a parameter that fills a label
  value of the collector (§ 42.10c), MUST be refused when the targets load,
  naming the target and the parameter. At load the value MUST be checked
  where the collector writes it as it is written at the moment the
  configuration's check uses for clock placeholders. A value with braces
  that open no clock placeholder MUST be sent as written.
- **Not in a label value.** A clock placeholder in a label value (§ 42.10c)
  MUST be refused at load, saying that it may stand in a request only.
- **Cache.** The clock MUST NOT be part of the response cache key: a cached
  answer is kept for its `cache.ttl`, and the documentation MUST say so. A
  request with a clock placeholder, the collector's or a static target's
  parameter's, MUST NOT be revalidated (§ 42.13.2): another day's validators
  could have the target answer `304` for a different request.

## 42.12 CI, container, and chart releases

CI MUST materialize Go module checksums before running tests and MUST run the
test, race, vet, build, and Helm validation checks. On every pull request
whose changes the go filter (§ 34.33) matches, CI MUST also build the code
for `darwin` on `amd64` and `arm64`, linking both commands, since the release
builds archives for it, and MUST build the image from the Dockerfile without
pushing it, so a change that breaks either fails before a release does.

Known vulnerabilities MUST be reported by a workflow of its own, on every
push and pull request and on a weekly schedule, for reference: it MUST NOT
fail, whatever it finds and when the check could not run, and MUST write its
findings to the run's summary and raise a warning. It MUST scan what ships and
what the workflows run — every command of the module, the exporter and the
repository's tools, with all each imports — and MUST NOT scan the packages
only tests use, so that an advisory for code only a test reaches, as the gRPC
server of the tests' stand-in is, is not reported against the exporter.
`make vulncheck` MUST scan the same packages with the same pinned version. A
test MUST keep the two equal, MUST fail for a command they do not cover, and
MUST hold the list of the module's packages left out, failing for a package
that is neither scanned nor on it.

The exporter and the Helm chart MUST be released on independent cycles, from
separate tags and separate workflows. Releasing one MUST NOT publish the other,
so a chart fix does not require an exporter release and an exporter release does
not republish an unchanged chart.

Release tags MUST be namespaced by what they release: `exporter/<name>-vX.Y.Z`
for the exporter and `chart/<chart-name>-X.Y.Z` for the chart.

Each GitHub Release's description MUST be its section of `CHANGELOG.md` at the
repository root, never notes generated from the merged pull requests. Its
sections MUST be headed `## Unreleased`, which MUST come first and exactly
once, `## exporter X.Y.Z` or `## chart X.Y.Z`, each component's versions
strictly descending. Every section MUST begin with the subsection
`### What changed for you`, holding at least one line: what someone upgrading
has to do or will notice, such as a renamed or removed key or value, a changed
default or a changed metric, or "Nothing; you can upgrade without changes.";
the only other subsections MUST be `### New` and `### Fixed`, in that order.
Every pull request that changes what a user sees MUST add its lines to
`## Unreleased`, and a release MUST move the lines it ships into its own
section. `tools/release-notes.sh <exporter|chart> <version>` MUST print the
section's text without its heading and MUST fail, naming the section, when it
is missing or does not begin with `### What changed for you` and a line under
it. Both release workflows MUST run it before they build or check anything
but the tag, and MUST pass what it prints to `gh release` with `--notes-file`.
`CHANGELOG.md` MUST have a section for the chart version `Chart.yaml`
declares and one for the exporter release its `appVersion` names.

Each release workflow MUST be triggered by every tag under its namespace, not
only by well-formed ones, and MUST fail when the tag does not match the required
format. A workflow triggered only by the strict pattern would leave a malformed
release tag matching no workflow at all, which is indistinguishable from a
successful release that produced no artifacts. Because a tag filter's `*` does
not match `/`, covering a namespace requires both the `<prefix>/**` and
`<prefix>*` patterns. The format check MUST run before any other step, so an
invalid tag costs nothing.

The exporter release workflow MUST be triggered by tags under `exporter` and
MUST:

- resolve the release version from the tag, which carries a prefix and
  therefore cannot be parsed as a bare semantic version, and fail when the tag
  does not end in `MAJOR.MINOR.PATCH`;
- publish versioned and `latest` container tags to GHCR, moving `latest` only
  to the newest release of all and the `MAJOR.MINOR` tag only to the newest
  release of that line, so a patch to an older line never takes them back;
- run the test suite against the Python the image ships, the Dockerfile's
  `PYTHON_VERSION`, with the Dockerfile's pinned `lxml`, `PyYAML` and
  `python-dateutil`, and with the modules of `test/python/requirements.txt`
  installed and required, as CI does;
- build release binaries for the documented target platforms, linked with
  the `-ldflags` the Dockerfile links the image's with, the version and the
  revision from the same values, so `--version` reports the same for an
  archive's binary as for the image's;
- create a GitHub Release, described by the version's section of
  `CHANGELOG.md`, containing the software archives and a file of
  their SHA-256 checksums, `prometheus-universal-exporter-<version>-sha256sums.txt`
  in `sha256sum` format, so a download can be checked with `sha256sum -c`.
  The release documentation MUST give the check for every platform an archive
  is built for: macOS has no `sha256sum`, so beside the Linux command it MUST
  give `shasum -a 256 -c`, which reads the same file;
- push the image with an SBOM and the full provenance of its build
  (buildx's `sbom: true` and `provenance: mode=max`) attached as
  attestations;
- sign the image with cosign, keyless with the workflow's OIDC identity, by
  the digest its push reported, never by a tag, which could be moved to other
  content between the push and the signature, and verify that signature
  against the workflow's identity; `latest` and `MAJOR.MINOR`, moved to the
  same digest, are covered by it;
- sign the checksums file, the same way, into a Sigstore bundle,
  `prometheus-universal-exporter-<version>-sha256sums.txt.sigstore.json`,
  verify it, and publish the bundle with the archives. The release
  documentation MUST give the commands that verify the bundle
  (`cosign verify-blob --bundle`) and the image (`cosign verify`), each
  naming the issuer `https://token.actions.githubusercontent.com` and an
  identity that matches only `release.yml` run for an exporter release tag;
  and
- publish nothing before every archive, the checksums and the image are
  built, the image without being pushed, and then publish in this order: the
  image under its version tag alone, then its signature and the checksums'
  signature; the GitHub Release, created as a draft with its assets, the
  checksums' bundle among them, and then published; and last `latest` and
  `MAJOR.MINOR`, moved to the digest the version tag was pushed with. A
  failure at any step MUST leave `latest` and `MAJOR.MINOR` unmoved unless
  the Release is published, and no Release published without its image. A
  rerun MUST succeed where the Release already exists, draft or published,
  by replacing its assets and setting its description again
  (`gh release edit --notes-file`) rather than creating it again.

Every workflow that runs the test suite — CI, the exporter release, and the
Dockerfile update workflow, which runs `make ci` against the updated pins
before proposing them — MUST first install that Python and those libraries,
read from the Dockerfile; one that runs `make ci` MUST also install the pinned
golangci-lint and gopls (`make lint-install gopls-install`), since `make ci`
runs both.

The chart release workflow is specified in
[SPECIFICATION-CHART.md](SPECIFICATION-CHART.md) § 42.12.

Every workflow MUST set `permissions` at its top, granting no write there, so
no job runs with the repository's default token, which may write to
everything; a job that has to write MUST grant itself only the scopes it
needs. CI and the vulnerability check MUST NOT write at all. The exporter
release's publishing job MAY write `contents` (the GitHub Release),
`packages` (the image) and `id-token` (cosign's keyless signatures of the
image and the checksums); the chart release's MAY write `contents`, `packages`
(the chart and its Artifact Hub metadata) and `id-token` (cosign's keyless
signature); the Dockerfile update job MAY write `contents` and
`pull-requests` (the branch and the pull request it opens). `id-token` MUST
be granted to exactly the job of each release workflow that signs. A test
MUST hold the write grants to that list, failing for a workflow without top-level
permissions, for a write granted at the top, and for a job's write grant that
is not on the list.

Every action a workflow uses MUST be pinned to a full 40-character commit SHA,
followed on the same line by the release it is in as a comment
(`uses: actions/checkout@<sha> # v7.0.1`), the first-party `actions/*`
included, so one rule covers every workflow. A tag or branch can be moved by
whoever controls the action's repository, and the next run would execute what
it then names with the job's token, which in the release and Dockerfile update
jobs may write; a commit SHA names one tree for good. An action in this
repository (`./...`) and an image (`docker://...`) are exempt. The pins MUST be
kept current by Dependabot's `github-actions` updates of the repository root
(§ 42.16), which move the SHA and rewrite the version comment together. A test
MUST fail for a `uses:`, a commented-out one included, that is not in that
form, and for a `.github/dependabot.yml` without that update.

Both release workflows MUST run their respective validation checks before
publishing artifacts, and MUST publish nothing unless CI has passed on the
commit the tag names. CI has passed on a commit when the newest finished
`ci.yml` run that a push of that commit started, and that ran the suite,
concluded `success`. A run that did not succeed MUST count whatever it ran; a
run that succeeded MUST count only when its step that runs the repository's
checks concluded success, found by its name, which a test MUST keep the same
in `ci.yml` and the script: a push whose changed paths ran none of the Go
steps passes with every step skipped, and MUST NOT take back a failure before
it. A run a pull request started does not count, since it tested the pull
request merged into its base. A first
job, which every publishing job needs, MUST validate the tag and then check
this with `tools/ci-passed.py`, reading the Actions API with a token limited
to `contents: read` and `actions: read`. A commit whose CI runs are queued or
running, or that has none yet — a tag is often pushed right after its
commit, and the tag push starts a CI run of its own — MUST be waited for, up
to an hour; when every run has finished and the newest counted one did not
succeed, when none ran the suite, when the wait runs out, and when the API refuses the request, the release MUST
fail, saying that CI has not passed on the commit and to run CI or wait for
it, then push the tag again or re-run the release.

The repository's tests MUST cover the release tag contract: that each release
workflow triggers on its whole namespace, that the format it enforces accepts
well-formed tags and rejects malformed ones, and that the version committed in
`Chart.yaml` would produce a tag the chart release accepts. They MUST run
`tools/ci-passed.py` against a stand-in of the API, for a run that passed, one
that failed, a run that skipped the suite after one that failed, runs that
finish while it waits, no run, and a wait that runs out, and MUST fail when a publishing job of a release workflow does not need
the job that runs it. They MUST also
fail when a workflow runs the test suite without first setting up the image's
Python and libraries, or runs `make ci` without installing golangci-lint and
gopls; when the exporter release publishes its archives without the checksums
file, publishes anything before every archive and the image are built,
publishes out of the order above, pushes the image with tags other than its
version or without its SBOM and full provenance, signs the image other than
by the digest it pushed, after `latest` moves or before the push, does not
sign the checksums file before the Release or leaves its bundle out of the
Release's assets, grants `id-token` to a job that does not sign, or links an archive's binaries with other `-ldflags` than the
image's; when CI builds no darwin binaries or no image; and when the chart release pushes before checking its default image, or
signs the chart by tag rather than by the digest it pushed (SPECIFICATION-CHART.md
§ 42.12).

## 42.13 Collector response caching

Each collector MUST support a `cache` mapping with two Go durations, and
`revalidate` (§ 42.13.2):

```yaml
collectors:
  - name: expensive_api
    request:
      type: http
    cache:
      ttl: 60s
      stale_if_error: 5m
    limits:
      max_cache_entries: 1000
```

When `ttl` is greater than zero and a probe repeats a request the exporter has
already served within that interval, the exporter MUST return the stored
result and MUST NOT contact the target again. Omitting `cache`, or both of its
keys, or setting them to `0s` MUST disable caching for that collector, which is
the default. A negative value, an unknown key, and a `cache` that is not a
mapping MUST be rejected during configuration validation; a single duration
such as `cache: 60s` MUST be refused with a message showing the mapping form.

The cache MUST be in-memory and process-local. It MUST NOT be written to disk
and MUST NOT be shared between exporter replicas. Cached entries are lost on
restart, which is expected and MUST NOT be treated as an error.

An entry MUST be served only for a request that matches the stored request
exactly. The cache key MUST be a fingerprint covering at least:

- the collector name and the full effective collector definition;
- the `target` value;
- every `/probe` parameter the collector's request type accepts (its
  declared overrides), including `method`, `path`, `timeout`, `body`,
  `insecure_skip_verify`, `retry_attempts`, `retry_backoff` and every
  `param_<name>`, with all values; and
- every header forwarded to the target, including a forwarded `Authorization`
  value and those given as `header_<name>` parameters.

The key MUST NOT cover a parameter that changes nothing sent: one no request
type accepts, which the probe ignores (`debug` among them), or a
`header_<name>` parameter for a header the collector does not forward. Probes
differing only in such parameters are the same request, and MUST share a
cache entry and a trip (§ 42.13a); otherwise a caller could fill the cache
with copies of one result, evicting the entries `stale_if_error` falls back
on, by adding parameters of its own. A collector whose definition cannot be
fingerprinted has no key: its probes MUST be neither cached nor shared.

The key MUST cover a probe's parameters as the probe reads them, not as the
caller wrote them: the method upper-cased and trimmed, a duration as a
duration, a boolean as a boolean, a number as a number, and a `param_<name>`
left empty as one not given. `method=get` and `method=GET`, `timeout=5s` and
`timeout=5000ms` MUST be one key, one cache entry and one shared trip; a
parameter given more than once is refused before a key is made (§ 3.2). A
static target's scrape and the probe making the same request MUST still share
an entry.

The presence and the absence of a parameter, a header, or a credential MUST
produce different keys. Every list the key is built from — a parameter's
values, a header's values, and a static target's own sections (`targets`,
`metadata`, `retry_codes`, `retry_statuses`) — MUST be written with its length, so a value that
reads like the next item cannot move a boundary and make two different
requests share a key. A probe that supplies no credential, no forwarded
header, or no TLS override therefore MUST NOT be able to read an entry stored
by a probe that supplied one, and two probes presenting different credentials
MUST NOT share an entry. This is a confidentiality requirement: a cached result
may only ever be returned to a probe that would send the target the very same
request.

A stored entry MUST be the cache's own copy of the result, taken when it is
stored. Hits MAY share that copy rather than copy it again, provided nothing
changes a stored entry and a reader that adds series to what it read, as the
freshness series do, never writes into the entry's storage.

Because the collector definition is part of the key, a configuration reload
MUST retire every entry cached under the previous definition. A loaded
configuration is never changed in place — a reload publishes a new one — so
the definition's fingerprint SHOULD be worked out once per collector and
configuration and remembered, rather than on every probe: encoding a large
collector costs far more than the rest of the key. A collector that is not
one of the current configuration's own MUST be fingerprinted afresh. Credentials read
from files at request time are covered only through their configured paths, so
a rotated credential file takes effect for a cached request once the entry
expires; deployments that rotate credentials faster than the cache interval
SHOULD shorten `cache` accordingly.

Only a fully successful probe MAY be cached. Requests that fail at the HTTP,
decode, transform, or validation stage MUST NOT be stored.

### 42.13.1 Serving stale results when the target fails

`stale_if_error` MUST keep each stored result for that long after its `ttl`
runs out. A result past its `ttl` is stale: it MUST NOT answer a probe that the
target could answer, only one whose trip fails, as RFC 5861's
`stale-if-error` does for HTTP caches. A trip fails when the probe would
otherwise be answered with an error: the target cannot be reached or times
out, answers a status outside 2xx, its response cannot be decoded, transformed
or validated, a metric rule with `error_mode: fail` has no value, or the
collector is at `max_concurrent_probes`. Such a probe MUST be answered `200`
with the stored result of the identical request (the same cache key), if one
is within `stale_if_error`, instead of the error. The failure MUST still be
logged and counted in the self-metrics as it would have been, and the answer
MUST NOT count in `http_exporter_scrape_success_total`. An error policy that
carries on (`log`, `ignore`) is not a failure. `ttl` MAY be `0s` with
`stale_if_error` set: every probe then goes to the target, and the last good
result is kept only to stand in for a failure. A collector whose definition
changes MUST NOT serve a result stored under the previous one.

A trip that failed because the target refused its credential — HTTP `401` or
`403`, or gRPC `UNAUTHENTICATED` or `PERMISSION_DENIED` — MUST NOT be answered
with a stale result, by a probe or a static target's scrape: the credential
may have been revoked, and the stored result was given to another. Nor MUST a
trip the collector's `allowed_targets` or `denied_targets` refused (§ 26.1),
answered `403`, whether before the request or on a redirect: the target may
not be reached, and no earlier result stands in for it. A debug probe or
debug scrape (§ 42.17) MUST report either as having no stale result. The entry
MUST stay stored, to stand in for a later failure of another kind.

So that a stale answer never passes for a fresh one, while `stale_if_error` is
set every answer of the collector MUST carry two gauges of the exporter's own,
without labels, after the collector's metrics:

```text
http_exporter_result_stale        1 for a stale result standing in for a failed trip, else 0
http_exporter_result_age_seconds  seconds since the answered result was fetched from the target
```

The age MUST be 0 for a result just fetched and the stored entry's age for a
cached answer, fresh or stale. On the static targets endpoint (§ 42.14), which
serves a target's last result until its next scrape, the age MUST be worked
out at every read of the endpoint, from when the result's data came from the
target, so it says how old the data is when it is read. A result in which a rule produced either name
MUST fail validation, since the names would clash; without `stale_if_error`
the names are free and the gauges are not added. They MUST NOT count against
`limits.max_metrics`. Samples of a stale answer carry no timestamps of their
own, so Prometheus stores them at the time of the scrape.

Each stale answer MUST be counted in
`http_exporter_cache_stale_served_total{collector}` and logged at warn level
with the result's age, sparingly like repeated failures (§ 25), with a line
when the collector answers with a fresh result again. Identical probes sharing
a trip (§ 42.13a) MUST share the stale answer. A static target (§ 42.14)
whose scrape fails MUST export the stale result, with its target labels and
the two gauges, under its OTLP resource, while `http_exporter_target_up` stays
`0`, since the target was not scraped; a probe answered stale and exported
over OTLP exports the answer as served. `http_exporter_cache_entries` MUST
count stale entries, and `limits.max_cache_entries` MUST evict by the end of
the stale window.

Stored metric sets MUST be copied when written and when read, so a cached entry
can never be mutated by the probe that produced it or by a later reader, and
concurrent probes MUST be safe.

The cache MUST be bounded per collector by `limits.max_cache_entries`. When the
limit is exceeded, the exporter MUST drop expired entries first and then the
entries closest to expiry — by the end of the stale window, not of `ttl` —
the smaller key first among entries expiring at the same instant. Every store
keeps the bound under the cache's lock, so it MUST NOT look at or sort every
entry of the collector: the work of a store MUST grow no faster than the
logarithm of the collector's entries, times the entries it removes.

The exporter MUST expose per-collector cache self-metrics:

```text
http_exporter_cache_hits_total
http_exporter_cache_misses_total
http_exporter_cache_entries
http_exporter_cache_stale_served_total
```

A probe answered from the cache, including one that finds it filled while it
waits to start its trip, MUST count as a hit; a probe whose trip goes to the
target MUST count as a miss; a probe answered by sharing another's trip MUST
count as neither, and as coalesced. A probe or a static target scrape that
finds no cache entry and is then turned away by a concurrency limit (§ 42.13b),
and a static target scrape the shutdown cuts short while it waits for a slot,
MUST NOT count as a miss; one turned away MUST count only as rejected, as
§ 42.13b says.

A cache hit MUST count as a successful scrape in `http_exporter_scrapes_total`
and `http_exporter_scrape_success_total`, and the cached metrics MUST still be queued
for OTLP export. Target-request self-metrics such as
`http_exporter_scrape_http_status_code` and
`http_exporter_scrape_response_bytes` describe the last real target request and
MUST NOT be altered by a cache hit.

### 42.13.2 Revalidating expired responses

An `http` or `graphite` collector that caches MUST keep, with each stored
result, the response it was made from — status, headers and body as the
target sent them, before the decode converted the body or rewrote a header —
when that response carried an `ETag` or a `Last-Modified` header, the request
was sent with `GET`, `HEAD` or no method of its own, the collector has no
`request.pagination`, and `cache.revalidate` is not `false`. `revalidate` is
a boolean key of the `cache` mapping and MUST default to `true`. A paginated
collector's response is pages merged into one, which no page's validator
speaks for, and MUST be neither kept nor revalidated. Another method MUST NOT
be made conditional: a matching `If-None-Match` on it asks for `412
Precondition Failed` (RFC 9110 § 13.1.2); a `graphite` request sent as a form
with `POST`, because its expressions are too long for a URL, is such a
request.

When an entry with a kept response is no longer fresh, the next trip of its
cache key MUST send `If-None-Match` with the kept `ETag` value exactly as
received, a weak one included, and `If-Modified-Since` with the kept
`Last-Modified` value exactly as received, each when the response had it.
Every attempt of the request MUST carry them: a retry, and the request sent
again after a `401` made `request.login` log in again (§ 42.6a). A request
that `request.headers` or a forwarded header already makes conditional MUST
be sent as configured and its answer MUST NOT be taken for a revalidation.

A `304 Not Modified` answer to the conditional request MUST be answered with
the kept response: its status and body, and its headers with every field the
304 sent replacing that field's values, as RFC 9111 § 4.3.4 says, except
`Content-Length`, `Content-Encoding`, `Transfer-Encoding`, `Content-Type`,
`Content-Range`, the hop-by-hop fields and those the 304's `Connection`
names, which MUST be kept as stored. The trip MUST then decode, transform and
validate that response as one just downloaded, the rules reading the kept
status (`$status`, `response.status_code`) and the updated headers, and on
success MUST store the new result with the updated response, fresh for
`ttl`. The 304 MUST NOT be checked against `accept_status`, and MUST NOT be
treated as a failed status. The exporter MUST NOT cache the rendered answer
in place of transforming again: only the download is saved. Any other answer
MUST be handled as an unconditional trip's: an accepted one replaces the
entry, a failing one fails the trip. A 304 to a request that was not made
conditional on a kept response MUST fail the trip in the `http_status` stage
with an error that says the request was not conditional on a cached
response, unless the collector accepts `304`.

An entry with a kept response MUST NOT be removed when its `ttl` and
`stale_if_error` have run out: it MUST answer no probe, and MUST stay for the
next trip to revalidate until a new result replaces it, a reload drops the
collector's entries, or `limits.max_cache_entries` needs its place. Over the
limit, expired entries MUST still go first, an expired entry kept for
revalidation among them. `http_exporter_cache_entries` MUST count it. An
entry without a kept response MUST expire as § 42.13 and § 42.13.1 say.

Revalidation and `stale_if_error` MUST compose in this order: a fresh entry
answers without a trip; past `ttl` the trip is conditional when the entry
kept a response; a 304 or an accepted new response answers fresh; a trip that
fails, the conditional request included, MUST be answered with the stale
result while it is within `stale_if_error` (§ 42.13.1), and with the failure
past it.

Each trip that revalidated MUST be counted in
`http_exporter_cache_revalidations_total{collector,result}`, a counter with a
series from zero for each `result` of every `http` and `graphite` collector:
`not_modified` for a 304 the kept response answered, `modified` for a new
response the collector accepted. It has no per-request series in verbose
mode, and is exported over OTLP with the other self-metrics. After a 304,
`http_exporter_scrape_http_status_code` MUST be `304` and
`http_exporter_scrape_response_bytes` `0`, which describe what the target
sent. A debug probe's report (§ 42.17) MUST show the conditional headers
among the request's headers, and the request's outcome MUST say `304 Not
Modified` and that the cached body was reused, with its size and status.

## 42.13a Sharing identical probes in flight

A probe that arrives while an identical probe is already in flight MUST NOT
send its own request to the target. It MUST wait for the one in flight and
answer with an exact copy of its result: status, headers and body, a failure
included. Several Prometheus replicas probing the same target at the same
moment would otherwise each reach it, multiplying the load on a slow or
rate-limited endpoint.

- Identical MUST mean the response cache key (§ 42.13): the collector
  definition, the target, every probe parameter that makes the request and
  every forwarded header, credentials included. Probes that could get
  different answers MUST NOT share; probes differing only in parameters the
  key leaves out MUST. A probe without a key MUST NOT share. Nor MUST probes
  of different stays of a collector that reloads removed and brought back, or
  changed and changed back, while a trip was in flight (§ 24.1a).
- It MUST work with the response cache off. With it on, the cache is checked
  first, and the shared request fills it once. The probe that starts a shared
  request MUST check the cache again before going to the target, since one that
  just finished may have filled it; an answer found so is a cache hit, queued
  for OTLP once, as a hit found before the shared request is.
- The shared request's own work MUST be counted and logged once: its HTTP
  status, response bytes, decode, transform, validation and error counters, its
  log lines, its cache entry and its OTLP export. Every probe MUST still count
  in `http_exporter_scrapes_total` and, by the shared outcome,
  `http_exporter_scrape_success_total`, with its own duration. Each probe answered by
  another's request MUST increment `http_exporter_probes_coalesced_total` for
  its collector. The shared request's counts belong to none of the probes
  sharing it: per request (§ 22.1) they MUST be counted when it ends, whichever
  probe started it and whether or not that probe is still waiting.
- The shared request MUST run detached from the probe that started it, so that
  probe's client going away MUST NOT fail the others. It MUST be cancelled when
  every probe waiting on it has gone, and a probe arriving after that MUST start
  a new request rather than join the cancelled one.
- A panic in the shared work MUST answer the waiting probes with `500` and
  the fixed text of a probe that panicked (§ 6.1a) rather than stop the
  exporter.
- It MUST be on by default, and a collector MUST be able to turn it off with
  `coalesce: false`, for a target that must see every probe as a request.
- Static targets (§ 42.14) are scraped once per interval by the exporter
  itself and are not affected.

## 42.13b Limiting concurrent probes

A collector MAY set `max_concurrent_probes`, the number of trips to its
targets — the request or file read, with decoding and transforms — it makes at
once. Unset or `0` MUST mean 32; a negative value MUST be rejected at load.
Coalescing (§ 42.13a) bounds identical probes only, so without this probes of
many targets of one backend would reach it with no bound.

- A probe that would exceed the limit MUST be answered `503 Service
  Unavailable` at once, naming the collector and the limit, without contacting
  the target, and MUST be counted in `http_exporter_probes_rejected_total`. It
  MUST NOT be queued: Prometheus has a scrape timeout, and an immediate reason
  is better than none.
- A probe answered from the cache, or by sharing a request in flight, makes no
  trip and MUST NOT take a slot.
- A static target MUST share its collector's limit, and MUST wait for a slot
  within its scrape budget instead of failing at once; if none frees in time it
  MUST fail in the `concurrency` stage and be counted as rejected.
- A slot MUST be freed when the trip ends, and `http_exporter_probes_in_flight`
  MUST report the trips in progress per collector.
- `--probe.max-concurrent` MUST bound the trips of every collector together,
  on top of each collector's limit, since each trip holds a response and its
  series in memory. A probe over it MUST be answered `503` naming the flag and
  counted in `http_exporter_probes_rejected_exporter_limit_total`, apart from
  `http_exporter_probes_rejected_total`, which counts the collector's own
  limit; a static target MUST wait for a slot as for its collector's. `0`,
  the default, MUST leave only the per-collector limits; a negative value
  MUST be a command-line error, also with `--dry-run`.
  `http_exporter_trips_in_flight` MUST report the trips of every collector
  together, and `http_exporter_trips_max_concurrent` the limit.
- Static target scrapes waiting for a slot MUST wait in line, and a slot
  that frees MUST be handed to the first waiter it can serve under both its
  collector's limit and the exporter's, not broadcast to every waiter.

## 42.13c Backing off from failing targets

A collector MAY set `failure_backoff`, with `after`, `initial`, `max` and
`min_budget`, to stop contacting a target that keeps failing. A target that does not answer
holds a trip slot (§ 42.13b) until its timeout on every scrape, so without
it a few dead endpoints can keep the slots full.

- `after` MUST default to 3 and be at least 1, `initial` MUST default to
  `30s` and be a positive duration, and `max` MUST default to `10m`, or
  `initial` when that is longer, and MUST NOT be shorter than `initial`;
  `min_budget` MUST default to `1s` and be a positive duration; anything
  else MUST be refused at load, naming the collector and the key.
  Without `failure_backoff` every probe and scrape MUST contact the target,
  and so MUST it with `failure_backoff.enabled: false`, which MUST turn off
  the backoff a file's `defaults` give the collector (§ 5.0b) and MUST make
  the collector the same, compared on a reload and in its fingerprint
  (§ 42.13), as one without the block; its other keys MUST still be checked.
  `enabled` MUST default to true.
- A trip MUST count as failed for backoff when its fetch failed — the
  connection, a timeout, TLS, a status the collector does not accept, or the
  request type's own fetch error — whatever `error_handling.on_fetch_error`
  made of it. A decode, transform or validation failure MUST count as a
  success: the target answered. A trip the target policy refused (§ 26.1),
  one a trip limit turned away before it started (§ 42.13b) and one
  cancelled because every probe waiting for it went away or the exporter
  shut down MUST change nothing. Any success MUST forget the target.
- A trip whose fetch failed because the trip ran out of its budget — its
  context's deadline came, or the fetch error is that deadline's — MUST
  count as failed only when that budget was at least `min_budget`; below it
  the trip MUST change nothing, neither counting as a failure nor forgetting
  the failures before it. A probe's budget comes from its caller's
  `X-Prometheus-Scrape-Timeout-Seconds` (§ 3), which is no part of the key a target is told apart
  by, so a caller asking for less time than the target needs MUST NOT be
  able to back the target off for every probe and scrape sharing its state.
  A static target's scrape has its interval for a budget, and the floor MUST
  apply to it the same way. A failure that is not the deadline's — a
  refused connection, TLS, a status the collector does not accept, the
  target's own error, or the expiry of a timeout of the request's own such
  as the `timeout` parameter — MUST count whatever the budget.
- After `after` consecutive failed trips, the target's probes and static
  target scrapes MUST NOT contact it for `initial`. Each MUST fail at once in
  the fetch stage with an error naming the target, the consecutive failures
  and the time of the next try, RFC 3339 in UTC (`<target> is backing off
  after 3 consecutive failed scrapes; next try after
  2026-10-23T09:14:05Z`), and everything that applies to a failed fetch MUST
  apply to it: `error_handling.on_fetch_error`, `cache.stale_if_error`
  (§ 42.13.1), the failure log (§ 25.1), which MUST take its repeats for one
  failure, and a static target's `http_exporter_target_up` 0. It MUST take no
  trip slot, and MUST NOT change `http_exporter_scrape_http_status_code`,
  `http_exporter_scrape_response_bytes`, the scrape-time histogram or the
  last-scrape timestamp, which describe trips to the target.
- When the wait has passed, the next trip MUST go to the target, and only
  that one while it is in progress: a probe identical to it MUST share it as
  any probe shares a trip in flight (§ 42.13a), and any other trip of the
  target MUST fail at once as during the wait. Its failure MUST double the
  wait, up to `max`; its success MUST forget the target. A trip let through
  that ends without an outcome — turned away, cancelled, or panicking — MUST
  let the next trip go to the target.
- A target MUST be told apart as the response cache tells probes apart
  (§ 42.13): probes differing in a `param_<name>` or another parameter of
  the key MUST back off apart, and a static target and the identical probe
  MUST share one state.
- The targets remembered MUST be bounded per collector, at 10,000, the one
  that failed longest ago forgotten first, since probe targets come from the
  query string. A target MUST be forgotten when a trip to it succeeds, and a
  reload that removes or changes the collector MUST forget all of its
  targets (§ 24.1a); a trip of the collector as it was before such a reload
  MUST NOT record its outcome afterwards.
- A debug probe, debug scrape or run of the debug page MUST NOT ask or
  change the state, and MUST say in its report when the target is backing
  off (§ 42.17).
- `http_exporter_backoff_skipped_total` MUST count the trips that failed
  without contacting their target, and `http_exporter_targets_backing_off`
  the targets within their wait or with the trip after it in progress
  (§ 22).

## 42.14 Static targets

The exporter MAY be started with an optional static target document:

```text
--static-targets-file=/etc/prometheus-universal-exporter/static-targets.yaml
```

The document lists fully specified requests that the exporter scrapes itself,
each on its own interval, and serves together on the static targets endpoint
for Prometheus to scrape:

```yaml
interval: 1m
targets:
  - name: legacy_eu
    collector: legacy_text
    target: http://legacy.eu.example:8080
    interval: 30s
    request:
      method: GET
      path: /status
      body: ""
      timeout: 5s
      insecure_skip_verify: false
      retry:
        attempts: 2
        backoff: 2s
      headers:
        X-Tenant: team-a
      bearer_token_file: /var/run/prometheus-universal-exporter/target-auth/token
    labels:
      region: eu
    export_via_otlp: true
    otlp:
      service_name: legacy-app
      resource_attributes:
        deployment.environment: production
```

`interval` and `targets` MUST be required, and `targets` MUST NOT be empty; a
document without either MUST be refused. A document with the target
allocator's `discovered_targets` (§ 42.14d) MUST be refused, naming the
allocator, since the exporter discovers no targets. `--static-targets-file-schema` MUST
print the document's JSON Schema, and the committed
`configs/static-targets.schema.json` MUST be that output (§ 24.3). Every
target MUST name a configured collector; an unknown collector MUST be rejected
at startup and at reload. A target MAY set `params`, the values of its
collector's placeholders (§ 42.10a). The keys a target's `request` block may
set are those its collector's request type accepts (§ 5.1); any other key MUST
be rejected at startup naming the target, the key, the collector and the type.
What its `target` may be is also the type's: an `http` target MUST be an
absolute URL, and a `localfile` target MAY be left out and MUST otherwise lie
under the collector's `root`. A target does not declare a type of its own: it
inherits its collector's.

The target document MUST be reloadable on the same terms as the exporter
configuration: an invalid document MUST be rejected with the previous document
left in force.

Within what its type accepts, a target MUST accept every per-scrape parameter
the `/probe` endpoint accepts — for `http`, `method`, `path`, `body`,
`timeout`, `insecure_skip_verify`, `retry.attempts`, and `retry.backoff` — with
the same semantics, overriding the collector's own request settings for that
target only. An `http` or `graphite` target MUST also accept static request
`headers`, and an `http`, `graphite` or `grpc` target its own target
credentials, as inline or file-backed basic authentication or a bearer
token; an `sshfile` or `sql` target MAY instead name its own user and key
or password files, a `redis` target its own `username` and `password_file`
and an `ldap` target its own `bind_dn` and `password_file`, as § 5.1 says
for each. A `redis`, `ldap`, `tcp`, `sql`, `grpc`, `http` or `graphite`
target MAY name its own `tls` block, which MUST replace the collector's whole
block for that target, no key of the collector's being taken for a key it
leaves out; written, even as `tls: {}`, it MUST ask for TLS, so a target MAY
ask for TLS its collector does not but MUST NOT leave out TLS its collector
asks for. For `http` and `graphite`, whose URL's scheme decides whether TLS
is spoken, `tls: {}` MUST verify with the system roots alone, a block on an
`http://` target MUST be taken as the collector's is, and the target's block
MUST be the one every request of the target's trips is made with: its
pages, every hop of a followed redirect, whose client certificate rule
(§ 42.15c, rule 5a) MUST apply with the target's certificate, and its
login, unless the login block has a `tls` of its own; beside it the
target's `insecure_skip_verify` MUST be refused at load. A target's own
account and `tls` block MUST be checked at load as the collector's are —
cert and key files both or neither, a `username` or `bind_dn` with its
`password_file`, a target address that asks for plaintext refused beside a
block of its own — MUST NOT hold a placeholder, and MUST NOT be settable by
any probe parameter. They MUST be part of every key that tells the target's
requests apart: its cache key, the key of the trip it shares with identical
requests, `failure_backoff`'s key, and the key of every connection kept for
it, a `sql` pool, a `grpc` connection, an `sshfile` connection or an `http`
connection pool (§ 42.15b), and, for `http` and `graphite`, the key of the
login token kept for it, so two targets that differ in them never share an
answer, a trip, a backoff, a connection or a token. Their files MUST be read when the scrape needs them, as the
collector's are, and by `--dry-run`, which MUST warn under the target's name
of one a scrape would fail on; a debug report MUST mask a target's password
as it masks the collector's. A request that carries an
`Authorization` of its own — a static target's credential, or one a probe
forwards — sends it instead of the collector's, and the collector's
credential files MUST NOT be read for it: a missing or empty one MUST fail
only the requests that would have sent it. Because the document is
operator configuration rather than caller input, these headers are applied
directly and MUST NOT be filtered through the collector's
`request.forward_headers` allowlist.

Each target MAY declare `labels`, which the exporter MUST add to every metric
that target produces. A label the collector already extracted MUST NOT be
overwritten, and a target's `le` MUST NOT be added to a histogram, nor its
`quantile` to a summary, whose buckets and quantiles carry that label. A
label written `""` MUST be the label left out, as a `transform.labels` value
written `""` is (§ 6): it MUST be added to no series of the target, its
health series included.
`static_target` MUST be refused as a target label, since the
endpoint sets it, and so MUST `job` and `instance`, which Prometheus sets when
it scrapes the endpoint and which, kept with `honor_labels`, a target's would
replace. A target's label longer than its collector's
`limits.max_label_name_length`, and a target whose collector's limit is
shorter than `static_target`, 13 bytes, which the endpoint adds past the
collector's validation, MUST be refused when the file is checked against the
configuration (§ 21).

Each target MUST be scraped on its own `interval`, independent of when
Prometheus scrapes the static targets endpoint and of the OTLP export
interval. A target's `interval` MUST default to the file's `interval`, MUST be
at least one second, and MUST NOT be shorter than the target's
`request.timeout`; the file's `interval` MUST be at least one second too.
Retries whose waits alone — `attempts` × `backoff`, the target's
`request.retry` or else its collector's — reach the target's interval MUST be
refused naming the target, since the scrape ends with its interval and the
last retries could never be made.
A target new to the schedule — at startup, or added or changed in any way by
a reload: its address, request, params, labels or interval, or anything in
the definition of its collector — MUST be first
scraped within ten seconds, or within its interval if
that is shorter, so it does not stay absent from the endpoint, or served as
a definition no longer in force made it, for up to an
interval. So MUST a target that is defined as it was when the schedule last
looked, with its collector, where reloads followed since (§ 24.1a) changed
either and changed it back, or removed either and brought it back: a scrape
of it in flight read what a reload retired and publishes nothing, so the
target, in force anew, MUST have a first scrape of its own, made once that
scrape has ended, and only one. A target that did not change, of a collector
whose definition did
not change, MUST keep its place in the schedule through a reload. Its scrapes MUST then keep a fixed cadence, which SHOULD be offset
within its interval by a stable hash of its name so targets are spread over
it, starting at its first point no sooner than one whole interval after the
first scrape was due and sooner than two; with an interval of ten seconds or
less the first scrape is due at a point of the cadence, so the second scrape
MUST be due one interval after it. The interval MUST be counted from the
time the first scrape was due and not from the moment the scheduler came to
start it, which is always a little later: counted from that moment, the
point one interval on is never far enough, and every such target is scraped
a second time only two intervals after the first. Only for a first scrape
that was put off, because a scrape begun on the target's old definition
still ran, and for one the scheduler came to start more than a tenth of the
interval after it was due, as after the process was held up, MUST the
interval be counted from when the first scrape began: counted from a time
that far back the cadence would start while the first scrape may still run,
or in the past, and a second scrape be cut short, lost, or made at once.
The first scrape, which has its interval to end in, has then
ended when the cadence starts, so that the first turn of the cadence
waits for no scrape and has a whole interval, and a target whose scrapes take
most of their interval is not cut short after a start or a reload. A scrape
MUST be bounded by its interval, and two scrapes of one target MUST NOT run
at once. A turn that comes while the target's last scrape still runs — a
scrape that used its whole interval ends just after the next turn has come —
MUST NOT be given up: it MUST wait for that scrape, start as soon as it has
ended, without waiting for a further interval, and end by the turn after it
— a budget of the steady cadence, which the first scrape runs into by no
more than the scheduler started it late, a tenth of the interval at most —
so a target that never answers is scraped once per interval, each scrape
failing, and one that answers again is seen within one interval. A turn MUST
be skipped, and logged as `static target scrape skipped`, only when it is
lost: the scrape it waited for was still running when the turn after it
came, the newer turn then waiting in its place, or no scrape slot came free
(below). A failed scrape MUST NOT by itself log a skipped one. The first
scrape of a target a reload changed, due while a scrape begun on its old
definition runs, MUST likewise wait for that scrape, neither skipped nor
overlapping it, and be made when it ends. A scrape that
starts after skipped ones MUST end their run in the failure log. A scrape that runs out
of its interval MUST fail saying so: that the scrape ran out of its interval's
budget, not only that a deadline was exceeded. A result
of a target a reload removed while its scrape was in flight MUST NOT be
published or exported, nor that of a target a reload changed, or whose
collector it removed or changed, meanwhile (§ 24.1a). The exporter
MUST scrape at most the document's `concurrency` targets at once, 8 when it is
unset or 0, and MUST refuse a negative one; a target due while all are busy
MUST wait for a slot within its interval and be skipped, logged, if none
frees. A reload that changes `concurrency` MUST apply to the scrapes that
start after it and to those waiting. The slots MUST be shared between the
collectors the document names: a collector MUST be able to take a slot
while it holds fewer than its share, `concurrency` divided by the number of
collectors, at least one; beyond its share it MUST take one only while one
stays free for every other collector holding none, half the slots at most; a slot freed MUST go to
the waiting target whose collector holds the fewest, the first to wait of
those. A
panic during a static target scrape MUST NOT end the process: it MUST be
logged with its stack and end that scrape as failed, with
`http_exporter_target_up` 0, leaving the other targets' scrapes unaffected.
Retries MUST follow the collector's `request.retry`, each key a target's
`request.retry` sets replacing the collector's and each it leaves out keeping
it, as the `retry_attempts` and `retry_backoff` probe parameters each replace
one. Static target scrapes MUST run through the same
fetch, decode, and transform path as `/probe`, including the collector's
cache, limits, and validation. A static target scrape and a `/probe` request
that would produce a byte-for-byte identical request MUST share cache entries,
which requires the cache key to be derived from the same request fingerprint.

Static targets MUST NOT be reachable through `/probe`, and MUST NOT appear on
the self-metrics path.

Each static target scrape MUST produce a health result with that target's
labels:

```text
http_exporter_target_up
http_exporter_target_scrape_duration_seconds
http_exporter_target_last_success_timestamp_seconds
```

Without them a failing target is absent and cannot be distinguished from a
target that was never configured. A target that names no target, as one of a
`localfile` collector may, MUST have no `target` label on them rather than
one with an empty value (§ 18.1), and a label of the target's own named
`target` MUST NOT take its place. The last success timestamp MUST be the Unix
time of the target's last successful scrape, kept through later failures, and
0 until one succeeds, so how long a target has given nothing new can
be alerted on. A failed scrape MUST produce the health
result with `http_exporter_target_up` set to zero and MUST NOT produce
collector metrics for that target, except the last good result, marked stale,
under `cache.stale_if_error` (§ 42.13): the metrics an earlier scrape
produced MUST NOT go on being served as the target's after a scrape of it
failed. A turn that publishes nothing — one skipped, or a scrape a shutdown
cut short — MUST leave the result of the last scrape that ended as it was.
The documentation MUST say what the endpoint serves in each of the three
cases. Whether a failed stage fails the
scrape is the collector's `error_handling` to say, as on a probe (§ 19).

### 42.14a The static targets endpoint

The exporter MUST serve the latest result of every static target — its
metrics with its labels, or its stale result, and its health result — at
`--web.static-targets-path`, which MUST default to `/static-targets`:

- Every series MUST carry `static_target` with the target's name, so the same
  metric from two targets stays two series.
- Each family's series MUST be served together under one `HELP` and `TYPE`,
  as the text format requires. A family a target produces with a different
  type than an earlier target, in target name order, MUST be left out for that
  target and logged, sparingly like repeated failures (§ 25); the rest of both
  targets MUST be served. So MUST a family whose name is that of a series of
  an earlier target's histogram or summary — `x_bucket`, `x_sum` or `x_count`
  beside a histogram `x`, `x_sum` or `x_count` beside a summary `x` — and a
  histogram or summary one of whose series has the name of an earlier
  target's family, since the exposition would hold two families of one name;
  the log line MUST name the family it clashes with. A clash that a later
  read no longer finds MUST be
  logged as ended while its target is still served, and forgotten without a
  line when its target is gone, so a clash that comes back is logged anew.
- A target not yet scraped MUST be absent, and a target removed from the
  document MUST leave the endpoint with the reload. A scrape that read a
  target, or its collector, before a reload removed or changed it MUST
  publish nothing (§ 24.1a).
- Serving the endpoint MUST NOT contact a target: it reads what the targets'
  last scrapes left. A target's result SHOULD be stored already labelled with
  `static_target` when its scrape publishes it, and a read MUST NOT change
  what is stored.
- The merge of the results SHOULD be kept between reads, and MUST be made
  again once a scrape publishes a result or a reload changes the targets, so a
  read between two scrapes only writes the kept merge: an unfiltered read of
  targets without `http_exporter_result_age_seconds` SHOULD write the kept
  rendering as it is. A kept merge MUST still serve each
  `http_exporter_result_age_seconds` as of the read. A type clash is then
  logged when the merge is made, not at every read.
- An optional `targets` query parameter MUST narrow a read to the targets it
  names, given separated by commas, as the parameter repeated (as Prometheus
  renders a scrape config's `params`), or both; spaces around a name and empty
  names MUST be ignored. A name no target in force has MUST be answered `400`
  naming every such name, and so MUST a parameter that names no target,
  rather than an empty answer. A named target not yet scraped MUST be absent,
  as without the parameter. A narrowed read MUST serve exactly the lines of
  the named targets that a whole read would serve at that moment: the type
  clash rule above MUST be applied over every target, so which target keeps a
  clashing family, and what is logged about the clash, does not depend on
  the parameter.
- The endpoint MUST answer `GET` and `HEAD`, and `405` otherwise. It MUST be
  gzipped like `/probe` (§ 23) and protected by `web.basic_auth` like the
  self-metrics path (§ 42.5).
- The path MUST be one fixed path of plain segments, and MUST NOT be the
  self-metrics path or another endpoint's; otherwise the exporter MUST exit
  with status 2 before `--dry-run` or startup, as for the self-metrics path
  (§ 42).

The endpoint MUST be served, empty, without a static target document.

The exporter MUST serve a status page at the static targets path followed by
`/status`, `/static-targets/status` by default, for people who cannot ask
Prometheus: a section of the static targets, then one of the recent probes,
then one of the OTLP export. Of the static targets:

- It MUST have one row per static target in force, with its name, its
  collector, its target as its health series show it (with any credential
  withheld, as in the logs), its labels, its interval and whether it sets
  `export_via_otlp`; and, once it has been scraped, when its last scrape
  began, how long ago, how long it took, and its result: `success`,
  `failed`, `stale result served` (a failed scrape answered with the stale
  result under `cache.stale_if_error`) or `skipped by backoff` (a scrape
  `failure_backoff` kept from the target); and whether the response cache
  answered it. A target not yet scraped MUST be shown as `not yet scraped`.
  A scraped target's name MUST link to the endpoint narrowed to it,
  `?targets=<name>`, relative to the page, so the link holds under a prefix
  a proxy serves the exporter at, and one not yet scraped, of which the
  endpoint has nothing, MUST have no link; under `--web.debug=full`, and
  under no other level, a scraped target's row MUST link its debug scrape,
  `?debug=<name>`, beside its name. The links MUST follow
  `--web.static-targets-path`.
  A target whose collector or address a reload changed MUST be shown as not
  yet scraped until its own scrape.
- With JavaScript, the table MUST sort by a column whose heading is
  clicked, the other way at a second click, Interval, Last scrape, Recent
  (by the strip's scrapes that were no success) and Failures by their
  values and the rest as text with the numbers in it by value and case
  ignored, ties in the page's order; each heading MUST be a button with
  `aria-sort`. A filter box MUST keep the rows whose name, collector,
  target, labels or result hold its text, in any case, and say how many of
  the rows match, or that none does. The order and the filter MUST be kept
  through a refresh, the box keeping focus and caret. Without JavaScript
  the headings MUST be plain and the filter not shown. The allocator's
  probe jobs table MUST sort and filter with the same script.
- Each row MUST show a strip of the target's last 10 scrapes, oldest first,
  marked ✓ for `success`, ✗ for `failed`, ~ for `stale result served` and …
  for `skipped by backoff`, each with its start and result as its tooltip;
  the JSON MUST give them as `recent`, each with `at`, `duration_seconds`
  and `result`, `[]` for a target not yet scraped.
- Each row MUST show the consecutive failed scrapes, the error of the last
  failed scrape, kept through later successes, with when that scrape began,
  and, while `failure_backoff` keeps scrapes from the target, until when, or
  that the try after the wait is in progress, with the failures the backoff
  counts. A scrape skipped by backoff MUST be shown as its own result,
  `skipped by backoff`; it MUST NOT count as a consecutive failed scrape, and
  MUST leave the error of the last failed scrape, and when it began, as they
  were. The error MUST be the text the
  log has, except that below `--web.debug=full` it MUST be shown as a
  probe's caller is answered it (§ 19.4), without the values of the answer
  it quotes and what it holds for the operator alone (as an `sshexec`
  command's stderr), the page being read by whoever may call `/probe`, with every URL's credentials withheld, masked again for what
  reads as a credential, a URL's userinfo (`scheme://user:password@`)
  anywhere in it among that, and cut to the bound of a failure's text
  (§ 25); nothing else of a target's answer MAY be shown. A scrape that
  panicked MUST be shown with a fixed error saying so; the panic's text MUST
  NOT be shown, only logged.
- An error kept through a later success MUST NOT be shown as a failing
  one's: while the target's last scrape succeeded, the page MUST show it
  muted, under a label saying it is an earlier failure recovered since, with
  when its scrape began and how long ago, in the page's age format, and the
  long text's preview and whole text as any other's; an error of a target
  whose last scrape did not succeed MUST be shown as before. The JSON MUST
  say which with `last_error_recovered` beside `last_error`, `true` for the
  first and `false` otherwise, and leave it out with `last_error`. The same rule MUST hold for the recent
  probes' error, recovered while the latest probe succeeded (`success` or
  `cached`), and for the OTLP, log and trace exports' last error, recovered
  while the last export got through, each with its own
  `last_error_recovered`.
- A summary line MUST give the number of targets, of those failing, and of
  those not yet scraped; the failing targets MUST come first, each part in
  name order.
- Every value MUST be HTML-escaped, and the page MUST be served as the
  other pages are (no caching, the pages' Content-Security-Policy).
- `?format=json` MUST serve the same rows and summary as JSON, and
  `?format=fragment` the page's content section alone, rendered by the
  page's template, with no controls or script, with the page's headers
  (`Content-Type: text/html; charset=utf-8`, `Cache-Control: no-store`, the
  pages' Content-Security-Policy and `X-Frame-Options: DENY`), behind the
  same Basic Auth and refusal of requests from other sites; another
  `format` MUST be answered `400`.
- The page MUST have a Refresh button, which without JavaScript MUST be a
  link reloading the page, and an auto-refresh selector offering `Off`,
  `5s`, `10s`, `30s`, `1m`, `5m`, `15m`, `30m` and `1h`. Its inline script
  MUST refresh by fetching the page's URL with `?format=fragment`,
  same-origin with credentials, and putting the answer in place of the
  content section; it MUST NOT start a refresh while one is waiting for its
  answer, MUST NOT refresh while the tab is hidden, MUST say the time the
  data shown is of once (beside the controls, hiding the content's own
  as-of line, which a page without JavaScript shows), and when a refresh
  fails MUST say so and keep the data shown.
- A value of free text past 160 characters or three lines — an error of a
  static target, of a probe, of the OTLP export or of the log export, a
  target, a static target's labels, a probe's parameters — MUST be shown as
  its start, ending in `…`, in a `<details>` whose summary is a control
  labelled `Show the whole <what it is>` and whose body is the whole text,
  so that without JavaScript the whole text is on the page; a shorter value
  MUST be shown whole, with no control. Both MUST be escaped as every value
  is. With JavaScript the control MUST open the whole text in a modal
  `<dialog>` outside the content a refresh swaps, by a listener on the
  content element, so that controls a refresh brings open it too; the text
  MUST be put in the dialog with `textContent`, never parsed as HTML; the
  dialog MUST close on its Close button, on Esc and on a click on its
  backdrop, MUST give focus back to the control that opened it, and MUST say
  the time its text is of. It MUST have no heading, its label naming the
  kind of text it holds (`The whole error`) for a screen reader. No refresh MUST swap the content while the
  dialog is open, one whose answer comes while it is open included: that
  answer MUST be held and swapped in when the dialog closes, after focus is
  given back, the control in its opener's place then taking focus; closing
  it MUST resume the refresh, at once if one fell due. `?format=fragment` MUST have the `<details>` and no dialog, and the
  JSON MUST be as it is, every value whole.
- Controls on one line MUST line up: the Refresh button, the auto-refresh
  selector and its label MUST be centred on one line at one height; on the
  debug page *Run* MUST have the height of the timeout box beside it and
  stand on its line, under the box's label, and on the debug and collectors
  pages the fields of a row MUST stand on one line when one label wraps.
- The interval MUST be `?refresh=` when that is `off` or a duration in whole
  seconds from 5s to 24h, and otherwise `--web.status-refresh`; another
  `?refresh=` MUST be ignored. An interval none of the selector's MUST be
  listed in it in its place by length. Choosing an interval MUST put it in
  the URL as `?refresh=`. With an interval, the page MUST reload itself on
  it without JavaScript (a `<meta http-equiv="refresh">` in `<noscript>`),
  and the selector MUST put the interval chosen in the URL through a form.
- `--web.status-refresh` MUST take `off` or a duration in whole seconds from
  5s to 24h, by default `off`; any other value, `0s` and the empty value
  among them, MUST be a command-line error, exit `2`.
- In allocator mode the page MUST say that it shows the targets the
  allocator gave this replica, and the probes this replica answered. It
  MUST name the member the replica is, linked to the allocator's view of
  it, `--allocator.url` followed by `/?member=<name>`; say the generation of
  the bundle in force, the one the probes are answered from, or that none
  is; say since when it is in force once the client has heard that the
  reload that put it in force returned, the start for the bundle the
  exporter started with, and say nothing of a time while the client's
  latest bundle is another; and say when the allocator was last heard from.
  The JSON MUST have them as `allocator`, with `member`, `member_view`,
  `generation`, `in_force_since` and `last_sync`, a value missing left out,
  and MUST NOT have `allocator` outside allocator mode.
- Each row's collector MUST link its section of the collectors page,
  `#collector-<name>`, relative to the page.
- The page MUST be served with or without static targets in force; without
  them its static targets section MUST say that none are configured, and the
  JSON MUST have an empty `static_targets` array.
- It MUST answer `GET` and `HEAD`, and `405` otherwise with `Allow: GET,
  HEAD`, and MUST be protected by `web.basic_auth` like every endpoint but
  `/health` and `/ready`.
- What it keeps of a target MUST be one record replaced by each scrape, and
  MUST be forgotten with the target: when a reload, or a bundle from the
  allocator, puts in force static targets without it, whether or not the
  static targets endpoint is read, together with the target's stored result
  and last success. A target brought back under its name MUST be shown as
  not yet scraped, with no failures and no error, until its own scrape; a
  scrape of the removed target that ends after the removal MUST record
  nothing.
- A self-metrics path equal to the status page's path MUST make the exporter
  exit with status 2, as a self-metrics path equal to the static targets
  path does.

Of the recent probes:

- Every `/probe` the exporter answers MUST be recorded under its collector,
  its target as the logs show it (with any credential withheld) and its probe
  parameters that change the request, each value with what reads as a
  credential masked, a URL's userinfo in it among that, withheld whole when
  its parameter's name reads as a credential, and cut; forwarded headers
  MUST NOT be shown. A probe answered
  from the response cache MUST be recorded as `cached`, and one answered by
  another probe's trip (§ 42.13a) MUST be recorded and marked shared. A
  debug probe MUST NOT be recorded, nor a probe answered `400`, nor one whose
  caller went away before it was answered.
- Each probe MUST be recorded with when it began, how long it took, its
  result — `success`, `cached`, `failed`, `refused by policy` (`403`),
  `rejected by limit` (`503`), `stale result served` or `backing off` — the
  HTTP status the exporter answered, the series it gave and its error, shown
  and cut as a static target's error is; a probe that panicked MUST be shown
  with the fixed error it was answered, the one a static target's scrape that
  panicked is shown with.
- Under `--web.debug=full`, and under no other level, a row whose target
  and parameters are shown as they were sent, none withheld, masked or cut,
  MUST link the probe sent again with `debug=true`: `probe?` relative to the
  page, with the collector, the target and the parameters; any other row
  MUST have no such link.
- The last 10 probes of each collector and target MUST be kept, and no more
  than 500 collectors and targets: a new one MUST make the one probed least
  recently forgotten. A target MUST be cut to 512 bytes and the parameters to
  512 bytes in all.
- Recording a probe MUST NOT redact or sort its parameters again, or make
  its probe key's query again, when a probe with the same cache key
  (§ 42.13) was recorded before: the key it is recorded under MUST be
  remembered by the cache key, for no more than 1,024 cache keys, so that a
  probe like one before it, a cache hit included, is recorded without an
  allocation. What is shown MUST be what the parameters make, the redaction
  and the cuts included.
- The page MUST have a row per collector and target with its latest probe's
  details, the results of its kept probes as a strip, oldest first, and the
  error of the latest failed probe kept with when it began; the rows failing
  their latest probe (any result but `success` and `cached`) MUST come first,
  each part in order of collector, target and parameters, under a summary
  line with the number of rows and of those failing. Every value MUST be
  HTML-escaped.
- `?format=json` MUST give `probe_keys`, `failing_probe_keys` and `probes`,
  an array with an object per row and, in it, the row's probes, oldest first.
- A reload MUST forget the recent probes of every collector it removes or
  changes, with the rest of the state kept per collector, and a probe of the
  old definition that ends after the reload MUST record nothing.

Of the OTLP export:

- With OTLP export off (`otlp.enabled` false, or no endpoint) the section
  MUST say so in one line, and the JSON's `otlp` MUST be `{"enabled":
  false}` alone.
- With it on, the section MUST show the endpoint with the URL's userinfo
  withheld and the values of query parameters named like credentials masked,
  as a target is shown, and MUST NOT show `otlp.headers`; the protocol, `http/json`;
  `otlp.interval`; and what the exports carry: the probes' results, the
  number of static targets with `export_via_otlp`, and the self-metrics.
- It MUST show the last export to the endpoint: when it began, how long ago,
  how long it took, its result — `success`, `failed`, or `partial success`
  when the endpoint took it and rejected data points in its
  `partialSuccess`, with how many — the requests of it sent, the data points
  they carried and its retries; when the last export that got through
  ended, or that none has; and the failed exports in a row. With
  `otlp.unready_after_failures` set it MUST show the readiness `/ready`
  answers (§ 23), from the same reasons, not from the failed exports alone:
  `ready`, or `unready` with the reasons `/ready` gives, in the JSON as
  `unready_reasons`, so that it says unready while the exporter shuts down;
  and, when the failed exports are among the reasons, when the failed export
  that made it so ended — the Nth of the current run of
  failures under `otlp.unready_after_failures` N, whenever N was set: a
  reload that lowers N to the run's length or under MUST show the end of the
  new Nth failure at once, and one that raises it over the run's length
  `ready`. It MUST keep for that the end of each failure of the run up to
  the most N may be, 1000, and no more, whatever the run's length.
- It MUST show the error of the last failed export, one of which the
  endpoint refused a request included, with when that export began, kept
  through later successes, shown as a static target's error is: every URL's
  credentials withheld, masked again for what reads as a credential, and cut
  to the bound of a failure's text (§ 25). The error MUST NOT show the
  endpoint's userinfo or a credential in its query, and neither MUST the
  warning that logs it.
- It MUST show the data points waiting for the next export and
  `otlp.max_pending_points`; and, since the start, the data points dropped,
  in all and by why (refused, rejected in a `partialSuccess`, over
  `otlp.max_pending_points`, of the last export at shutdown), the retries,
  and the exports that succeeded and failed. These MUST be the counters
  `http_exporter_otlp_points_dropped_total`,
  `http_exporter_otlp_export_retries_total` and
  `http_exporter_otlp_exports_total` are read from, and MUST equal them.
- What the page keeps of its own MUST be one record of the endpoint in
  force: the last export, the last failure's error and the ends of the
  current run's failures up to 1000, bounded whatever the exports. A reload that changes `otlp.endpoint` MUST show the new endpoint
  at once, without the old endpoint's last export, last success, failures
  or error, until the new endpoint's own export; the numbers since the start
  MUST go on.
- While the last export failed, a line at the top of the page MUST say OTLP
  export is failing, with the failed exports in a row and whether the
  exporter is unready.
- In allocator mode the section MUST say the exports are this replica's.
- With `otlp.exporter_instance` on, the section MUST show its attribute and
  the value in force (§ 42.1), and the JSON MUST have them as the `otlp`
  object's `exporter_instance`, `{"attribute": ..., "value": ...}`, left out
  while the block is off.
- With `otlp.logs` on, the section MUST show the log export (§ 42.1b): its
  endpoint with its credentials withheld as `otlp.endpoint`'s are, its level
  and interval, when the last log export began with its result and the
  records it carried, the last that got through, the failed log exports in
  a row, the last failure's error with when that export began, shown as an
  export's error is, the records waiting and `otlp.logs.max_pending_records`,
  and since the start the records dropped, in all and by why (over the
  bound, refused or rejected, of the last export at shutdown), the records
  sent and the log exports that succeeded and failed, from the counters of
  § 22.0c; nothing of readiness. In the JSON it MUST be the `otlp` object's
  `logs`, `{"enabled": false}` alone while `otlp.logs` is off, as the
  section MUST then say in one line. A reload that changes
  `otlp.logs.endpoint` MUST start its record again.
- With `otlp.traces` on, the section MUST show the trace export (§ 42.1c)
  as it shows the log export: its endpoint with its credentials withheld,
  its threshold, whether `errors` is on, its `parent_based`,
  `sample_ratio` and `max_per_second`, its interval, the traces kept by
  reason and dropped by reason since the start (§ 22.0c), its last export
  with its result and the spans it carried, the last that got through, the
  failed trace exports in a row, the last failure's error, the spans
  waiting and `otlp.traces.max_pending_spans`, and since the start the spans
  dropped, in all and by why, the spans sent and the trace exports that
  succeeded and failed; nothing of readiness. In the JSON it MUST be the
  `otlp` object's `traces`, `{"enabled": false}` alone while `otlp.traces`
  is off, as the section MUST then say in one line.
- Every value MUST be HTML-escaped, and `?format=json` MUST give the section
  as `otlp`.

Every replica of the exporter scrapes every static target; running static
targets with more than one replica, or with autoscaling, contacts each target
once per replica and exports each target with `export_via_otlp` once per
replica, as duplicate series, and the chart MUST say so
(SPECIFICATION-CHART.md § 33.1).

### 42.14b Static targets over OTLP

A target MAY set `export_via_otlp`, which MUST default to `false`. A target
with it MUST also be delivered over OTLP: each scrape's result and health
result MUST be queued for export under the target's OTLP resource, and
delivered on `otlp.interval` like probe results (§ 42.1a). A target without it
MUST NOT be queued. The exporter MUST refuse to start when a target sets
`export_via_otlp` while `otlp.enabled` is false or no `otlp.endpoint` is
configured, naming the target, and a configuration reload that would put the
exporter into that state MUST be rejected with the last valid configuration
left in force. Static targets without `export_via_otlp` MUST NOT need OTLP
export.

Each target MAY declare `otlp.service_name` and `otlp.resource_attributes`,
only with `export_via_otlp`; set without it, the block MUST be refused rather
than ignored. These form the OTLP resource the target's metrics are exported
under. Both MUST default to the exporter-wide `otlp.service_name` and
`otlp.resource_attributes`, and per-target attributes MUST be merged over the
exporter-wide ones rather than replacing them. A target's attribute written
`""` MUST be the attribute left out (§ 42.1): it sets nothing, so the
exporter-wide attribute of that name MUST stay as it is. A target's
`otlp.resource_attributes` MUST NOT set `service.name`, which its
`otlp.service_name` sets; the file MUST be refused naming the target and
pointing at `service_name` (§ 42.1). The exporter MUST emit one
`resourceMetrics` entry per distinct resource in an export, so metrics from
targets with different identities are not conflated. Every series exported
over OTLP — the collector's metrics and the health result — MUST carry
`static_target`, as on the endpoint: targets sharing a resource, such as two
of one collector under the exporter-wide identity, would otherwise export the
same series, and one target's values would replace the other's.

A target MAY set `otlp.labels_as_resource`, a list of names of its own
`labels`, and `otlp.instance_id`, a boolean defaulting to `false`, only with
`export_via_otlp` like the rest of its `otlp` block. Each label named with a
value that is not empty MUST be an attribute of the target's resource, and
MUST be left out of every data point of the target exported over OTLP — its
collector's series and its health series — where the point's label of that
name has the target's value; a point whose own label of the name has another
value MUST keep it. `otlp.instance_id: true` MUST give the resource the
attribute `service.instance.id` with the target's name. Both MUST be part of
the resource's identity (§ 42.1): targets whose resources are alike in every
attribute MUST be one `resourceMetrics` entry, and targets that differ in one
MUST be two. The static targets endpoint MUST serve the labels as before.
The file MUST be refused, naming the target, for a name that is not among the
target's `labels`, a name given twice, a name that is an attribute of the
resource already — from the target's `otlp.resource_attributes` or the
exporter-wide ones, an attribute written `""` being none — and
`instance_id` beside a `service.instance.id` attribute; the static target
schema MUST refuse a name given twice. With `otlp.exporter_instance` on, the
target's resource MUST carry its attribute too, and the file MUST be refused
naming the target where the target's resource would have it already (§ 42.1). With neither key the export MUST be
what it was without them, byte for byte.

### 42.14c Static target self-metrics

Static target scrapes MUST be counted in the existing per-collector
self-metrics rather than in per-target series, so exporter self-metric
cardinality does not grow with the number of targets. The exporter MUST
expose `http_exporter_static_targets`, the number of targets loaded, and
`http_exporter_static_targets_exported_via_otlp`, the number with
`export_via_otlp`. For autoscaling, the exporter MUST expose the counters
`http_exporter_static_targets_scrape_seconds_total`, the seconds every static
target scrape took from its start to its end, waiting for a slot included and
a scrape whose result a reload superseded too, but not one the shutdown cut
short, and `http_exporter_probes_seconds_total`, the seconds every probe
counted in `http_exporter_scrapes_total` took until its answer was ready, a
cache hit, a shared probe and one a limit turned away included, but not a
debug probe or a request refused before it was counted. Each MUST add the
same duration as its collector's `http_exporter_scrape_duration_seconds`,
MUST be one series without labels, also without verbose self-metrics, and
MUST be exposed from the start at `0`, so that their rate, the average number
in progress, can be added up per pod. The document's reloads MUST be reported under
`file="static_targets"` (§ 22.0b).

A target MAY set `shard_key`, any text; the exporter MUST ignore it, and the
target allocator MUST allocate the target by it in place of its name
(§ 42.14d).

### 42.14d The target allocator

`pue-allocator`, built from `cmd/pue-allocator` with the request types and
the version of the exporter, into `bin/pue-allocator` by `make build` and
into the image at `/bin/pue-allocator`, MUST serve exporter replicas in
allocator mode their configuration and their share of the static targets,
and Prometheus the probe targets of its probe targets file, each pointed at a
replica. What the two sides exchange MUST be the types, paths, timings and
functions of `internal/allocapi`. It MUST keep nothing on disk: the same input
files and heartbeats MUST give the same bundles and answers. What discovery
sources find is input too, so instances with a discovery source MUST run as
peers (**Peers** below), of which only the leader asks the sources, and the
chart MUST run them so (SPECIFICATION-CHART.md § 33.10c).

**Inputs.** `--config.file` MUST be loaded and checked with the exporter's
configuration loader, `--static-targets-file` with the static target file's
loader and checks, against the configuration, and `--probe-targets-file` MUST
be a document of one key, `probe_jobs`, besides `x-` keys, whose jobs each
have a `name` (unique, `[A-Za-z0-9_][A-Za-z0-9_.-]*`), a `collector` of the
configuration, `targets` (not empty unless the job has `discovery`, each
once, each checked as a static target's `target` is for the collector), and
MAY have `discovery` and `target_template` (below), `params` (probe
parameters, checked as a probe checks them; `target`, `collector`, `debug`
and a name that makes no label name MUST be refused), `labels` (a label name,
not `instance` nor starting with `__`; `""` leaves the label out) and
`shard_key` (`target` or `address`; anything else MUST be refused). The
allocator MUST NOT open the files the configuration names
(`config.WithoutNamedFiles`): the `otlp.tls` files, the `web.basic_auth`
files and a grpc collector's `protoset_file` and `proto_files`, with what
they import, are checked by the replicas, which mount them; every other check
of the loader MUST be made, and a grpc collector whose descriptors come from
files, and the `message` of a static target using it, MUST then not be
checked against them. A
`collector_files` entry that is absolute or leaves the configuration's
directory MUST be refused, and so MUST a collector file whose path relative
to that directory is `config.yaml` or `static-targets.yaml` and an entry that
matches `static-targets.yaml`; so MUST a collector file whose path relative to
that directory begins with `..`, and an entry of more than one path element
whose first element begins with `..`, with `*`, `?`, `[` or `\`, or with a dot
and one of those, which on a replica could match the cache directory's own
names. The allocator MUST NOT expand `${NAME}`
references. Invalid inputs at the start MUST exit 1; files that change while
the start reads them MUST be read again after a pause of 1 second, up to 5
reads in all, and exit 1 only then. The files MUST be read
again every `--config.watch-interval` (`0` reads them once; from `0` to
`24h`, else exit 2); a change MUST be
put in force only when all are accepted, the last good inputs staying in
force otherwise, and one refusal MUST be logged and counted
(`pue_allocator_reloads_total{result="failure"}`) once while its text
(`model.SameFailureText`) stays the same. What is served MUST be the text
that was checked: files that change while they are checked MUST be read
again at the next check.

**Discovery.** A probe job's `discovery`, and an entry of the static target
file's `discovered_targets`, MUST be a list of sources each setting exactly
one of `file_sd`, `http_sd`, `kubernetes` and `dns_sd`, decoded strictly, with an
optional `name` (`[A-Za-z0-9_][A-Za-z0-9_.-]*`, unique in the list, by default
`<kind>_<index>`), `labels` (a label name, not starting with `__` nor `job`,
`instance` or `static_target`, set to the value of the source label it
names) and `max_targets` (0 or more; 0 takes `--discovery.max-targets`,
default `10000`, at least `1`); `refresh_interval` MUST be at least `1s`,
and a source that sets none MUST be asked every `--discovery.refresh-interval`
when that is given (from `1s` to `24h`, else exit 2), whatever its kind, and
else every `1m` for `file_sd` and `http_sd` and `30s` for `kubernetes` and
`dns_sd`; a
`timeout` written longer than it MUST be refused, the refusal naming the
interval as the source's `refresh_interval` when it sets one, as `the
default refresh interval <d> (--discovery.refresh-interval)` when it came
from that flag, and as `<kind>'s default refresh interval <d>` else, and
one left out MUST be
`10s` or the `refresh_interval` when that is shorter. `file_sd.files` MUST
be paths or patterns ending in `.json`, `.yml` or `.yaml`, relative ones taken
from the directory of the file naming them, read again at every refresh.
`http_sd.url` MUST be an absolute `http` or `https` URL without credentials,
asked with `GET` and `X-Prometheus-Refresh-Interval-Seconds`, with an
optional `bearer_token_file` read again at every refresh and a `tls` block
(only for `https`: a written one, even empty, with an `http` URL MUST be
refused at load), without following redirects. The text of an `http_sd`
failure, logged, on the status page and in `/v1/status`, and of a `url` refused at load MUST give the URL with the values of
query parameters whose names read as credentials' masked
(`MaskCredentialQueryValues`), and a `url` that does not parse MUST NOT be
shown. Both MUST read a list of
`{targets, labels}` groups strictly, refusing an empty target and a label
that is no label name. `kubernetes` MUST list `discovery.k8s.io/v1`
EndpointSlices with HTTPS `GET` requests to `KUBERNETES_SERVICE_HOST` and
`KUBERNETES_SERVICE_PORT`, made by `internal/kubeapi` as the `kubernetes`
request type's are (§ 5.1), each with the `User-Agent`
`pue-allocator/<version>`, verified with the service account's `ca.crt`,
with its token read again at every listing, in pages of 100 (`limit=100`,
following each `continue` token), a page larger than 16 MiB asked for
again as § 5.1's `kubernetes` asks for a page larger than its response
limit (half the limit, down to 1, the rest of the listing keeping it, the
next listing starting at 100) and the fall to a smaller limit logged once
per listing at debug level, the whole listing, every page of every
namespace and every page asked for again, bounded by its `timeout`, in each of
`namespaces` (DNS-1123 labels; none lists the namespace of the service
account's `namespace` file) or, with `all_namespaces`, cluster-wide, by
`label_selector` (checked at load), taking only the slices whose
`addressType` is `address_type`'s, `IPv4` by default or `IPv6`, or every
slice with `any`, any other value refused at load; its targets MUST be each address of each
endpoint whose `ready` condition is true or unknown (every endpoint with
`include_not_ready`) at the slice's port named or numbered `port`, as
`host:port`, each once, with the meta labels `docs/ALLOCATOR.md` lists,
`__meta_kubernetes_endpointslice_address_type` among them. `dns_sd` MUST
take `names` (at least one, each a DNS name of at most 253 characters and
labels of 1 to 63, not an IP address nor holding `/?#@:[]`, written once,
compared and asked in lower case without a trailing dot), `type` (`SRV`, the
default, `A` or `AAAA`, any other value refused at load), `port` (1 to
65535, required for `A` and `AAAA`, refused for `SRV`) and an optional
`server` (`host` or `host:port`, port `53` by default, read as the `dns`
request type's `request.server`, § 5.1); it MUST look each name up, in list
order, as an absolute name with Go's resolver, sending every question to
`server` when it is set and to the system's nameservers otherwise, all the
names within its `timeout`, and MUST be built in every selection of request
types. Each SRV record MUST be a target at its target host, lower-cased and
without its trailing dot, and port, a record whose target is `.` none; each
A or AAAA record a target at its address (IPv4-mapped addresses written as
IPv4, IPv6 in brackets) and `port`. A name that does not exist or has no
records of the type MUST give no targets and MUST NOT be a failure; any
other lookup failure of any name MUST fail the answer. A target given twice,
under one name or several, MUST be taken once with the labels of the first
name in the list that gives it, and the targets MUST be sorted by address,
so that the same records give the same answer in any order. Each target
MUST have `__meta_dns_name`, the name asked, and an SRV target
`__meta_dns_srv_record_target` (the host of its address) and
`__meta_dns_srv_record_port`. An
answer larger than 16 MiB (for `kubernetes`, a page of one slice), with
more targets than its bound or that cannot be read MUST be a failure; a failing source MUST keep the targets of its last
good answer, one that never answered MUST add none, and a failure MUST be
logged and counted (`pue_allocator_discovery_refreshes_total{result="failure"}`)
once while its text stays the same. A probe job's discovered target MUST be
its `target_template` (which MUST hold `{{address}}`, and MUST NOT be set
without `discovery`) with the address in place of `{{address}}`, or the
address without one; one listed already or that the collector refuses MUST
be left out. A `discovered_targets` entry MUST have a `name` of a static
target's name and `discovery`, MAY have `target_template` and `shard_key`
(only `address`), MUST NOT have `target`, and MUST be checked at load as a
static target of the file with `192.0.2.1:9100` for the address; each
discovered address MUST make a static target named `<name>_<address>`, the
address made only of `[a-z0-9.:\-\[\]]` and escaped to at most 128
characters written with each letter and digit as itself, `.` as `_` and
`:`, `-`, `[`, `]` as `_3A`, `_2D`, `_5B`, `_5D`, and any other address, or
one whose name a written target of the file has, written as its first 48
bytes lower-cased with each character but a letter or digit as `_`, then
`_X` and the first 16 hex digits of its SHA-256; the name MUST depend on the
entry, the address and the file alone, never on the other discovered
targets, and an entry whose `name` is another entry's followed by `_` MUST
be refused at load. The target's `target` MUST be made as a probe job's is,
its other keys MUST be the entry's and its shard key its name, or its
address with `shard_key: address`; one
the checks refuse MUST be left out, and what is left out logged once while
it stays the same. A `discovered_targets` file MAY leave `targets` out.
A discovered probe target's group MUST carry its source's labels that are
`__meta_` labels or label names but `job` and `instance`, under the job's
labels, a job label written `""` removing one; a discovered static target
MUST carry its source's labels but those starting with `__`, `job`,
`instance` and `static_target`, under the entry's labels. What the sources
find MUST be put in force as a reload is (handover, removal at once, a
bundle changing only with its share), and a reload MUST keep a source whose
owner and settings it leaves alone, with what it found. A source a reload
replaces with one of the same owner's kind, owner, source kind and name but
other settings MUST hand the new one what it found and its counts, so its
targets stay in force until the new source first answers, a failure before
then keeping them and being counted; a source of a new name or kind MUST
start with nothing found.

**Peers.** With `--peers`, `host:port` entries, comma-separated, each with a
host and a decimal port from 1 to 65535 (anything else MUST exit 2), an
instance MUST, at its start and then every check interval, `--peers.check-interval`, resolve each
entry to every address its host has (an IP address taken as it is, an
IPv4-mapped IPv6 address as IPv4; a lookup that fails keeping the addresses
of the last, logged once while its text stays the same) and ask every
address but its own at `GET /v1/peer` with the API's bearer token within
the check timeout, `--peers.check-timeout`, each lookup bounded by it too,
without a proxy. The peers' timings MUST be flags: `--peers.check-interval`
(default `2s`) from `500ms` to `1m`, `--peers.check-timeout` (default `1s`)
from `100ms` to less than the check interval, so a round, which waits for
its checks, stays shorter than the wait between two rounds,
`--peers.failure-threshold` (default 3) from 1 to 20, and
`--peers.snapshot-timeout` (default `30s`) from the check timeout to `5m`;
a value outside its bounds, and any of them without `--peers`, MUST exit 2
naming the flag, and every place that waits, counts failures or says a
timing (the rounds, the checks, the snapshot fetch, the logs, the status,
the page) MUST use the values in force, never the defaults. `--peers.self`, only with `--peers`
(exit 2 otherwise), MUST be an IP address, taking the port of
`--web.listen-address`, or an IP address and a port, and is the instance's
own address, a peer even when no entry resolves to it; without it, the
address whose answer names the instance's own process (a random instance
made at its start) MUST be taken for its own. A instance MUST be live once
it has answered, until the failure threshold's checks in a row have failed, and
MUST stop being live at once when it answers `503`, which `/v1/peer` MUST
answer from the first `SIGTERM` or `SIGINT`, the instance then serving its
API on, as it does during a shutdown otherwise, for its shutdown delay
(`PeerOptions.ShutdownDelay`: the check interval and the check timeout,
3s with the defaults) before its server shuts down, so
every peer's next check gets the `503` rather than a refused connection; and
otherwise its process's id (`id`), whether it discovers, and whether it
knows its own address (`true` without `--peers`). The leader of a instance
that knows its own address MUST be the lowest of the live instances and the
instance itself, ordered by `netip.AddrPort.Compare` (IPv4 before IPv6, then
by the address's bytes, then by port). An instance that does not know its
own address MUST NOT follow an instance that does not discover: it MUST
follow the lowest live instance that discovers and knows its own address,
else the live instance of the lowest id among those that discover, unless it
discovers itself and its own id is lower, and lead when no live instance
discovers; it MUST log an error once when its own address is still not known
after the failure threshold's rounds. `--peers`'s help MUST say to list every
instance, this one included. Only the leader MUST run the discovery sources'
refresh loops; a follower MUST stop them, keeping what they found, and fetch
the leader's `GET /v1/peer/snapshot` with the token every round, which MUST
answer every source of the instance's input files, whether it has answered
and what it last found, by the source's identity (the kind of its owner, the
owner, its kind and its name, not its settings), its targets written as
target groups, each run of targets of the same labels one group with its
labels once, as a file_sd or http_sd answer gives them, with an `ETag` of
its digest and `304` to an `If-None-Match` naming it; the follower MUST
fetch it within the snapshot timeout, `--peers.snapshot-timeout`, its client having no shorter
timeout of its own, beside its rounds rather than within them, one fetch at
a time (a round that finds the fetch of the same leader still running MUST
NOT start another), so a leader that hangs while it writes its snapshot MUST
NOT delay any check; a fetch MUST be cancelled, and MUST put nothing in
force nor count as a failure, once the instance follows another instance,
leads or shuts down; and take a body of at most 16 MiB for each of its own
sources (16 MiB with none), and MUST name in `If-None-Match` the digest of
what it holds itself, computed as the leader's is, never the `ETag` it was
last given, so any difference, of a reload that reached one instance first
or of the follower's own sources while it led, is answered with the
snapshot. The follower MUST put each source the snapshot holds in force as
that source's answer would be, so the instances' bundle generations and
service discovery answers are equal, after the same checks (no empty target,
no label of no label name) and the same bound, its own `max_targets` of the
source or `--discovery.max-targets`: a source's targets refused MUST be the
source's failure, logged and counted in
`pue_allocator_discovery_refreshes_total{result="failure"}` once while its
text stays the same, the source keeping what the follower holds, and MUST
keep what it holds of a source that has not answered on the leader. The
snapshot MUST list, as `draining`, the static targets the instance drains
because a change of the discovered targets moved them, each with its member
and the seconds left, and its digest MUST cover each such target and member
but not the seconds; it MUST be taken under the lock under which an answer
of a source is put in force, so it never holds an answer without the drains
the answer made. A follower taking a snapshot MUST drain those in place of
the drains of discovered targets it holds, even when the discovered targets
stay the same, keeping a target it drains for a member list or input files
that changed for the longer of the two, listed in its own snapshot and its
digest as the leader's drain while the leader's snapshot lists it (so a
follower holding the leader's targets and drains MUST NOT be told apart from
the leader by its own drain of one of them) and as its own drain after that,
and leaving out a drain on a member
not in its member list, of a target it lacks, or of one that member owns
there. A source
of the follower's that the snapshot lacks MUST be asked by the follower
itself, from a refresh loop of its own, until a snapshot has it (or a `304`
says the leader holds what the follower does), and only then stopped. A
follower that, having taken the snapshot, still holds something else MUST
NOT count it as taken for its snapshot's age, and MUST log so once after
the failure threshold's such fetches in a row and once when the two agree again. A
instance that comes to lead MUST start the loops with what its sources hold,
the snapshot it took last, so no discovered target leaves the bundles until
each source answers; one that leads without ever having taken a snapshot
MUST first take that of the lowest other live instance, if any, starting the
loops once that fetch ends, whether it succeeded or not. A follower whose
fetches fail MUST keep serving the snapshot it took last, count each
failure, and log once after the failure threshold's failures in a row and once when a
fetch succeeds again. A change of leader MUST be logged. Without `--peers` a
instance MUST ask its sources itself, from its start. With
`--peers.leader-only`, only with `--peers` (exit 2 otherwise), an instance
that does not lead, before its first round included, MUST answer `/metrics`
with `200` and the families of the peers alone (`pue_allocator_peers`,
`pue_allocator_peer_up`, `pue_allocator_is_leader`, `pue_allocator_leader`,
`pue_allocator_leader_changes_total`, `pue_allocator_snapshot_age_seconds`
and `pue_allocator_peer_failures_total`), and no other family; `/v1/status`
with `partial` true and `members`, `spread` and `sources` `null`; and `/`
with this instance and its peers alone, naming the leader's address. The
instance that leads MUST serve the whole of each, so the instances'
allocation is in one instance's metrics. `docs/ALLOCATOR.md` MUST describe
the peers, their flags, timings and metrics, how long a failover takes
with the timings (at most the failure threshold times a check interval and
a check timeout after the leader's last answer), and that a network split gives
each side a leader, the targets then flipping between the two until it
heals.

**Schemas.** The repository MUST publish the JSON Schemas of the
allocator's own files in `configs/`, generated from the structs the
allocator decodes with rules by path, as the exporter's are (§ 24.3):
`configs/allocator-static-targets.schema.json`, the exporter's static target
file schema with `discovered_targets`, its entries declaring a static
target's keys but `target`, with `name` (required), `discovery` (required,
not empty), `target_template` and `shard_key` (only `address`), and the
static target file then valid with `targets` empty or left out; and
`configs/probe-targets.schema.json`, the probe targets file's. A discovery
source MUST declare exactly the keys of the allocator's `DiscoveryConfig`,
with each kind's own, and require one kind. `pue-allocator
--static-targets-file-schema` and `--probe-targets-file-schema` MUST print
them and exit 0, and `make schemas` MUST write them. The exporter's
`configs/static-targets.schema.json` MUST keep refusing `discovered_targets`.
A test MUST fail when a committed file differs from what the code generates,
when a schema's keys differ from the keys the allocator decodes, the probe
job's of the chart's values schema included (SPECIFICATION-CHART.md
§ 33.10c), when an example of `docs/ALLOCATOR.md` does not validate against
its schema, and when a schema accepts any of a set of invalid files.

**Members.** The allocator MUST take its heartbeat interval from
`--heartbeat.interval` (default `10s`), and MUST refuse one below `1s` or
above `2m` as a command-line error; the member TTL MUST be three intervals
(`allocapi.MemberTTL`), and no other setting of either MUST exist, on the
allocator or on the exporter. `--heartbeat.interval` MUST NOT be checked
against `--settle`: the two are independent. It MUST NOT change the timings of the
peers either, which check each other every `--peers.check-interval` whatever it is. A member MUST be live while its last heartbeat is younger than
the member TTL and did not say `shutting_down`; one that did MUST stop
being live at once. A heartbeat whose `sequence` is not greater than that of
the last heartbeat applied of its member with the same `instance`, or whose
`instance` is the one its member's current `instance` replaced, MUST be
answered as any other and change nothing; a heartbeat of another `instance`
MUST be applied whatever its `sequence`. A member not live MUST be kept,
with its `instance` and `sequence`, until its last heartbeat is
the member TTL old, so a heartbeat arriving after the one saying
`shutting_down` does not make it live again. A heartbeat of the `instance`
a member's current one replaced that does not say `shutting_down` and comes
more than the member TTL after the replacement is no late one: two
exporters heartbeat under one member name, each holds the member's bundle
and each scrapes its share. The allocator MUST then change nothing of the
member, MUST say `same_name` in the answers to both, MUST log it once
while it lasts, naming the member and both addresses, MUST show
`same_name` on the member in the status and on the status page, and MUST
count the member in `pue_allocator_members_same_name`, until no such
heartbeat has come for the member TTL. An exporter whose heartbeat is
answered `same_name` MUST log so, once while the answers keep saying it.
A refusal a heartbeat reports
MUST be logged once while the member keeps reporting it, and again when it
reports one after a heartbeat without. A change of the live members MUST take effect when it has
held for `--settle` (default `15s`; from `0` to `1h`, else exit 2), so a
member live again within it keeps its share; the first member list MUST form only once the live members have
not changed for `--settle`, and MUST NOT form before a member is live,
before `max(--settle, --heartbeat.interval)` after the start, nor before
`I` after the first heartbeat heard, where `I` is the longest
`interval_seconds` a heartbeat heard before it formed said, brought within
`1s` and `2m`, 0 when none said one; a heartbeat without
`interval_seconds`, or with 0, MUST NOT change `I`, but counts as any other
for the first heartbeat heard. The first heartbeat comes once a replica has
found the instance, which a rollout's lookups do only some time after its
start, and since a replica says the longest interval the instances it finds
said (§ 42.14e), `I` after it covers the replicas still on an older, longer
interval for as long as an instance that said it is found. A member
that stops being live MUST leave the member list only after `--settle`, as
any other change does. The allocator MUST take at most `--members.max`
(default `1000`, at least `1`) live members: the heartbeat of a member it
does not hold, not shutting down, while that many are live MUST be refused
and add nothing, logged once until a new member is taken again and counted;
a member no longer live MUST no longer count against the bound. Time
MUST be read through an injectable clock, so tests move it by hand.

**Allocation.** Every target MUST be placed on a member of the member list by
its shard key: a static target by its `shard_key`, or its name; a probe target
by its target, or with `shard_key: address` by the host and port it reaches
(a URL's host, `:80` or `:443` added for `http`/`https` without a port,
compared without case; anything else as written). The placement MUST be the
weighted placement of `docs/ALLOCATOR.md` "Weights and capacity": the targets
of one shard key, static and probe of every job, MUST be one unit, placed on
one member and weighing the sum of their weights; static and probe units MUST
be placed in one pass over the same members, a member's load being both;
the units MUST be taken in the order of the first 8 bytes of SHA-256 of
`unit`, a NUL and the key, and each MUST go to the member of the highest
weighted rendezvous score, its capacity over `-ln h` with `h` from
`allocapi.Score`, whose load with the unit is at most
`--allocation.balance-factor` times its fair share (the total weight times
its capacity over the members' capacity, rounded down), and when none has
room to the member of the least load for its capacity, the higher score
first; no unit MUST ever be left out. Of members of one capacity the order
MUST be `allocapi.Owner`'s. Weights, capacities and the factor MUST be held
in thousandths, and every sum, comparison and logarithm MUST be integer
arithmetic, ties broken by the hash and then the member's name, never by the
order of a map, so two instances of the same inputs and members MUST place
alike. `--allocation.balance-factor` MUST default to `1.25` and MUST be
refused as a command-line error outside `1` to `10` or with more than three
decimals; the bound MUST apply with or without weights and capacities. A
unit heavier than the fair share of the member of the largest capacity MUST
be logged once while it stays so and listed in `/v1/status` `spread.heavy`.
A probe whose placed member is not among the members it is routed among
(**API** below) MUST be placed among those, in the same order and by the same
rule, over the loads of the placement. A static target whose
owner changes while its old owner stays in the member list MUST stay in the
old owner's share for one of its intervals, its own or the file's, and then
leave it, whether a change of the member list, a reload of the input files
or a change of the discovered targets changed its owner (its `shard_key`
changed, say); a static target a reload removes MUST leave every share at
once. A follower MUST NOT work out the drains of a change of the discovered
targets it takes from the leader's snapshot: it MUST drain those the
snapshot lists, each for what is left of it there (**Peers** above), so
the instances serve identical bundles once the follower has taken the
snapshot, whatever order the leader's sources answered in.

**Bundles.** A member's bundle MUST be `allocapi.Bundle`, sealed: the
configuration at `config.yaml` and every collector file at its path
relative to the configuration's directory, as written, and, when its share
is not empty, `static-targets.yaml`, a static target file of the file's
top-level keys but `targets` and `x-` keys, and of the share, draining
targets included, in file order, each with its name written out and with
aliases and merge keys replaced by what they stand for, `weight` and
`default_weight` taken out. A member not in the member list MUST get the
configuration alone. With `--allocation.balance-factor` other than `1.25`
every bundle MUST also carry `allocation.yaml` (`allocapi.AllocationFile`)
saying `balance_factor`, so instances of different factors serve bundles of
different generations; a collector file whose path relative to the
configuration's directory is `allocation.yaml`, and a `collector_files`
entry that matches it, MUST be refused at load.

**Weights.** A static target's `weight`, the static target file's and the
probe targets file's top-level `default_weight`, a `discovered_targets`
entry's and a probe job's `weight` MUST each be a YAML number above 0 and at
most 1000 (`MaxWeight`) with at most three decimals, and anything else MUST
be refused at load naming the file and the target, entry or job; a target,
entry or job without one MUST weigh its file's `default_weight`, else 1. A
written target's `weight_label`, a `weight_label` that is no label name, and
a probe job's without `discovery` MUST be refused. The allocator MUST take
`weight` and `default_weight` out of the static target file before the
exporter's loader reads it, and a share MUST carry neither; a file without
them MUST be read as written. A discovered target of an owner with
`weight_label` MUST weigh the value of that label of its source when it is
a weight, and its owner's weight otherwise (`missing`, `not_a_number`,
`out_of_range`, `too_many_decimals`), never left out for it; such a fallback
MUST be logged once for each kind, owner, source and reason while any
target falls back for it, and counted in
`pue_allocator_weight_label_fallbacks_total{kind,owner,reason}` as each
target begins to. A discovered weight within 20% of the weight in force for
the target at its address, both weights, MUST be replaced by the weight in
force in what the source keeps, so followers take it from the leader's
snapshot and every instance places alike. The schemas MUST declare
`weight`, `default_weight` and `weight_label`, a weight as a number above 0
and at most 1000; they MUST NOT say `multipleOf`, which editors work out in
floating point.

**Capacity.** A heartbeat's `capacity` MUST be taken in thousandths
(`allocapi.CapacityMilli`): 1 when it says none, `0.1` for one below it,
NaN included, and `100` for one above. A member's capacity MUST be part of
the member list: a changed capacity MUST take effect as a change of the
live members does, once it has held for `--settle`.

**API.** `POST /v1/heartbeat` MUST take an `allocapi.Heartbeat` and answer
an `allocapi.HeartbeatReply` with the generation of the member's bundle, `""`
before the member list has formed, `heartbeat_interval_seconds`, the
heartbeat interval in seconds, and `instance`, a random name of the
allocator's process made at its start, in every answer; a heartbeat without `member`, a
`host:port` `advertise` with a host and a decimal port from 1 to 65535, an
`instance` of 1 to 64 bytes or a `sequence` of 1 or more MUST be answered
`400`, a body over 64 KiB `413`, and one refused for
`--members.max` `429` with `Retry-After` of the member TTL in seconds, rounded up. `GET /v1/bundle?member=<id>`
MUST answer the bundle as JSON with its generation, quoted, as `ETag`, `304`
when `If-None-Match` names that generation, and `503` with `Retry-After`
until the member list has formed; with `wait=<duration>` (at most `60s`) a
request that has its bundle MUST be answered as soon as the bundle changes,
and `304` when the wait runs out or the allocator shuts down, a wait longer
than the HTTP server's read timeout included; a request without `member`, or with a `wait` that is no duration, MUST be answered
`400`. `GET /v1/sd/<job>` MUST answer Prometheus HTTP service discovery
JSON, one group per target, in file order, whose target is the routed
member's `advertise` and whose labels are the job's labels, `__param_<name>`
for each parameter, `__metrics_path__=/probe`, `__param_collector`,
`__param_target`, `instance` (the target) and `__meta_pue_allocator_member`,
and no `job` label; an unknown job MUST be answered `404`, and `503` while
the member list has not formed or no member is live. A probe MUST be routed
among the live members of the member list (all live members if none is),
narrowed to those whose heartbeat reports a bundle generation whose
configuration has the job's collector when any does. With
`--web.token-file`, every endpoint but `/healthz` and `/ready` MUST answer
`401` to a request without `Authorization: Bearer <token>`; an empty or
unreadable token file MUST stop the start. The file MUST be read again when
its modification time or size changes, so a rotated token is required from
the next request; a file that is empty or cannot be read after a change MUST
refuse every such request with `401` until it holds a token, the failure
logged once while it stays the same. `GET` and `HEAD` of `/`,
`/v1/status` and `/metrics`, and no other request, MUST also take the token
as the password of HTTP Basic auth, of any user name, compared in constant
time, and their `401` MUST carry a `Basic` challenge beside the `Bearer`
one, so a browser prompts; the token MUST NOT be taken from the URL. The
allocator MUST serve plain HTTP only.

**Status page.** `GET /v1/status` MUST answer the instance's status as JSON,
calling an allocator process an instance and its process's id its `id`, no
key saying copy: under `instance` its address among its peers when known,
else `--web.listen-address`, its `id`, the build's version and revision, its
start and uptime, whether it is ready, and `--config.watch-interval` in
seconds (`watch_interval_seconds`, `0` for files read once), which the page
MUST show; under `peers`, with `--peers`
(else `null`), its role, the leader, the peers' timings in force
(`check_interval_seconds`, `check_timeout_seconds`, `failure_threshold`,
`snapshot_timeout_seconds`), which the page's Peers section MUST say, the
live instances (`live_instances`),
the leader changes, the snapshot's age, whether it has taken one and holds
the leader's targets, its check and snapshot failures, every address of its
peers, its own included (`instances`), with liveness, the last answer's
`id`, discovering and address-known, the last check and the failures in a
row, and the series of the peers' metric families exactly as `/metrics`
writes them; the heartbeat interval, the member TTL and `--settle` in
seconds, which the page MUST show; every
member known with its address, last heartbeat, whether it is late (live,
with its last heartbeat more than one and a half heartbeat intervals old,
which the page MUST mark), whether it is live, in the member list and
shutting down, the generation it reports, the one it is to have, a refusal
it reports, the static targets it keeps draining, the capacity its last
heartbeat said and the one the member list places by; the spread, per member
of the member list and per other live member a probe is routed to, of the
static targets it owns as `pue_allocator_assigned` counts them, those it
drains, and the probe targets routed to it, with the totals, the average per
member and the imbalance (the most a member has divided by the average, `0`
with no target), and as much of their weight (`weight`, `total_weight`), a
moved static target's on its new owner and on the member draining it only
as draining, with each member's capacity, fair share, load ratio and whether
it is overloaded, the members' capacity, the balance factor, the highest load
ratio, the members overloaded and the units heavier than a fair share
(`heavy`); each probe job, in file order, with its collector, its
targets and their weight, the members it is routed among (`routing`), each
with the targets of the job routed to it, 0 included, and their weight, the
fewest, the average and the most of
those, the imbalance (the most divided by the average, `0` with no target),
uneven when the most is above the average by 3 targets or more and by 3
or more standard deviations of a member's count, √(targets × (1 −
1/members) ÷ members), so the noise of hashing a few targets is not
marked, and its discovery sources with their result; and every
discovery source with
its kind, owner, source kind, result (`ok`, `failing` with its error, or
`pending`), the targets whose weight fell back on their owner's
(`weight_fallbacks`), which the page MUST show beside the result, targets, the interval it is asked at
(`refresh_interval_seconds`: its `refresh_interval`, else
`--discovery.refresh-interval`, else its kind's default; for a source whose
targets a follower took from the leader's snapshot, the leader's interval of
it, which the snapshot MUST carry per source and its digest MUST cover, so a
follower names the leader's interval it took and is told of a change of
it), which the page MUST show, a source's and its owner's names wrapping
rather than widening the table past the page, last refresh and last success, and whether what it
holds came from the instance's own discovery or the leader's snapshot. `GET
/` MUST render the same as an HTML page framed as the exporter's pages are
(§ 42.14a: the bar, naming it `Prometheus Universal Exporter · Target
Allocator`, with the theme picker, the logo as its icon, the same
`Content-Security-Policy`, `X-Frame-Options: DENY` and `Cache-Control:
no-store`), every value escaped, calling each allocator process an instance
and never a copy (`This instance`, the peers' `Instance` column, `(this
instance)`, `live instances`) and its process's random id its `Instance ID`
(`ID` beside each peer's address), with each member's capacity in the
members table, one its heartbeats changed shown as the one in the list, an
arrow and the new one until it takes effect, the spread's table's columns
fixed (member, static targets owned and draining under one heading,
probe targets routed under another, total, weight, draining weight,
capacity, fair share and the bar, never a column per probe job), the
section opening with what a static target and a probe target are, each
with its bar's colour, and how many of each the files write and the
sources found, the sources named, its legend naming the segments static
targets, owned and draining, and probe targets, routed, its weight
the static and probe weight the member is placed by, its draining weight
apart, its bar drawn in CSS alone of the static, draining and probe weight
with the member's fair share marked on it, the imbalance of the weight the
highest load ratio, an overloaded member's weight marked with a sentence
saying how many are and why, and each heavy unit named, with its shard key,
weight and member, in a warning above the table; a static targets table,
before the probe jobs table, of a row of the targets the static target file
writes and a row of each `discovered_targets` entry that found any, in file
order, with its targets, their weight, the members, min, average, max and
the imbalance of the row's targets each member owns, marked as a job's, and
the entry's sources, an entry's name linking its view (`?entry=<name>`);
a probe jobs table
of a row per job with the job, linked to its view, collector, targets,
their weight, members, min, average, max, the imbalance, marked where uneven, and the
sources with their result or `listed`, sorted by a column at a click on its
heading and the other way at a second, numbers by value, text with the
numbers in it by theirs and in any case (`job2` before `job10`), and ties in
file order, its headings plain until the script makes them buttons, so a
page without JavaScript shows no sort mark, a mark apart from its heading's
name, and filtered as a filter box is typed by the job, collector and
sources' names (`data-match`), with plain JavaScript, a refresh keeping the
order, the filter and the caret, and the exporter's status page's refresh controls, `?refresh=`
keeping the interval and `?format=fragment` answering the content alone,
without the bar; another `format` MUST be answered `400`. Each member's id
in the members and in the spread MUST link to its view, `GET /?member=<id>`,
framed as the page is, with a link back to it and refreshed as it is: the
member as the members have it; the static targets it owns as
`pue_allocator_assigned` counts them, by name, with collector, target,
labels (one written `""` left out), interval and weight; those it keeps
draining with the member each moved to, how much longer it drains and its
weight; and the probe targets routed to it, counted and weighed by job and
listed with job, collector and weight, in file order of the jobs and by
target; each weight in a column of its own, right-aligned, and the member's
capacity among its facts. A target MUST be shown as
`fetch.DisplayTarget` shows it for its collector, credentials withheld, and
a static target's `params` and `request` MUST NOT be shown. `?filter=<text>`
MUST cut each list to the targets whose name, target, collector, a label as
`name=value`, job or member moved to holds the text, in any case, before
each list is cut to its first 1000 (`MemberViewLimit`), each text on its
own; a filter MUST be trimmed of spaces, and one of spaces alone MUST be no
filter; each list MUST say how many it holds, how many match and, cut short,
how many are left out. The page MUST have a filter box, outside the content
a refresh swaps, that hides as it is typed the rows none of whose texts
`?filter=` matches holds it, which each row MUST carry joined by line feeds
(`data-match`), not the row's text, with plain JavaScript, and submits
`?filter=`. At a phone's width each row's cells MUST stack, each beside its
name, the name on the value's first line, also when the value's first label
wraps. `GET /v1/status?member=<id>` MUST answer the same as JSON
(`allocator.MemberView`). A member the allocator does not know MUST be
answered `404` as JSON, and the page and its fragment MUST answer `200`
saying so, the filter box hidden, so a view refreshing itself shows the
member gone; an empty `member` MUST be answered `400` on both. A follower
under `--peers.leader-only` MUST NOT serve a member's targets: its view
names the leader and its JSON is `partial`, with the leader's address. A
member's view MUST link each probe job's name to the job's view. When the
member's last heartbeat said its static targets path, its view MUST link
its status page, `http://<address><path>/status`, beside its address, and
each static target it owns or keeps draining the series it serves of it,
`http://<address><path>?targets=<name>`; and no such link otherwise, nor
when the address would not be the URL's host alone (a `/`, an `@`, a `?`
in it). A heartbeat's `static_targets_path` other than a clean absolute
path of at most 256 bytes, not `/`, without `?`, `#`, `%`, `\`, spaces or
control characters, MUST be dropped and the heartbeat taken all the same:
the exporter takes a `--web.static-targets-path` of any length, and a
replica MUST NOT be kept out of the member list over a link. Each probe
job's view, `GET /?job=<name>`, MUST be framed, refreshed and filtered as a
member's view is: the job's collector, targets, their weight with the job's
weight or its `weight_label` (`job_weight`, `weight_label`), shard key, labels (one
written `""` left out), params, a param whose name reads as a credential's
(`fetch.CredentialName`) as `<redacted>` and any other through
`fetch.RedactText`, and discovery sources with their result and refresh
interval, a failing one's error on a line of its own; the members it
is routed among, linked to their views, with the targets of the job each
has, as the spread counts them, their weight and a bar, the most first; and
its targets by target, each shown as `fetch.DisplayTarget` shows it, with
the member `SD` routes it to, linked, the labels its source gave it but
those the job sets or takes away, and its weight, right-aligned. The page MUST leave out a target's source labels,
those whose names start with `__` (`__meta_…`), unless `?meta=1` asks for
them; a job with discovery sources MUST have a *Show source labels* box by
its filter that puts `?meta=1` in the URL, or takes it out, and refreshes
the content, so a refresh, which fetches the URL's fragment, keeps the
choice; a refresh waiting for another URL MUST be aborted for it and its
answer never swapped in, so the latest choice is the one shown, and a
refresh asked while one waits for the same URL MUST be left to that one. A
target MUST keep its line where it fits, its labels wrapping first.
`?filter=` MUST cut the targets to those whose target, member or a label
shown as `name=value` holds the text, before the first 1000, and a
row's `data-match`, which the box filters by as it is typed, MUST hold the
same, so a source label left out matches neither. `GET
/v1/status?job=<name>` MUST answer the same as JSON
(`allocator.JobView`), but with every label of a target, which its filter
matches; a job the probe targets file does not have MUST be
answered `404` as JSON and `200` saying so on the page and its fragment, the
filter box hidden; an empty `job` MUST be answered `400` on both; a follower
under `--peers.leader-only` MUST name the leader and answer `partial`.
Each `discovered_targets` entry's row of the static targets table, and a
static source's Feeds cell, MUST link the entry's view, `GET
/?entry=<name>`, framed, refreshed and filtered as a job's view is: the
entry's targets, their weight, its shard key, labels (one written `""` left
out) and discovery sources with their result; the members of the member
list, linked, with the entry's targets each owns, as the spread counts them,
their weight and a bar, the most first; and its static targets by name,
each with its target as `fetch.DisplayTarget` shows it, its collector, the
member that owns it, linked, its labels and its weight. `?filter=` MUST cut
the targets to those whose name, target, collector, a label as `name=value`
or member holds the text, before the first 1000, and a row's `data-match`
MUST hold the same. `GET /v1/status?entry=<name>` MUST answer the same as
JSON (`allocator.EntryView`); an entry the static target file does not have
MUST be answered `404` as JSON and `200` saying so on the page and its
fragment, the filter box hidden; an empty `entry` MUST be answered `400` on
both; a follower under `--peers.leader-only` MUST name the leader and
answer `partial`. All take the token as the status page does. `docs/ALLOCATOR.md` MUST describe
the page, the member's view, the probe job's view, the JSON and how a browser gives the token, and
the chart README how to reach a pod's page and find the leader's.

**Metrics and health.** `/metrics` MUST expose `pue_allocator_members`,
`pue_allocator_targets{kind}`, `pue_allocator_assigned{member,kind}`,
`pue_allocator_targets_per_member{kind}` (the targets allocated to the
members, by kind, divided by the number of members; `0` with no member),
`pue_allocator_assigned_weight{member,kind}` (the weight of what
`pue_allocator_assigned` counts, a moved static target on its new owner
alone), `pue_allocator_member_capacity{member}`,
`pue_allocator_member_load_ratio{member}` (a member's static and probe
weight over its fair share), `pue_allocator_overloaded_members` (the members
whose weight is above the balance factor times their fair share because a
unit is too heavy for any member's room: still above it without their
lightest unit, or filled by one unit above it alone; a member above it by
less than its lightest unit, as whole units and bounds rounded down leave
one at a balance factor near 1, MUST NOT be counted, nor marked overloaded
in the status or on the status page),
`pue_allocator_weight_label_fallbacks_total{kind,owner,reason}`,
`pue_allocator_generation_info{member,generation}`,
`pue_allocator_reloads_total{result}`,
`pue_allocator_member_changes_total`,
`pue_allocator_members_refused_total`,
`pue_allocator_members_same_name`, and for each discovery source, by
`kind`, `owner` and `source`, `pue_allocator_discovered_targets`,
`pue_allocator_discovery_refreshes_total{result}` and
`pue_allocator_discovery_last_success_timestamp_seconds`, and with
`--peers` `pue_allocator_peers` (the live instances, itself included),
`pue_allocator_peer_up{peer}` (`1` for each other address of the peers
that is live, `0` for one that is not),
`pue_allocator_is_leader`, `pue_allocator_leader{peer}` (the leader's
address, `self` while it leads without knowing its own),
`pue_allocator_leader_changes_total`, `pue_allocator_snapshot_age_seconds`
(since the instance last held what the leader's snapshot holds, having fetched
it, or the start before the first time; `0` while it leads) and `pue_allocator_peer_failures_total{request}` (`check` or
`snapshot`). The families' `HELP` texts, like the flags' help, MUST call an
allocator process an instance, never a copy, and every sample MUST stand
under its own family's `# TYPE` line. `/healthz` MUST answer `200`; `/ready`
`200` once the inputs are loaded and `503` from the first `SIGTERM` or
`SIGINT`, which MUST also end every long poll before the server shuts down.

### 42.14e Allocator mode

With `--allocator.url`, the exporter MUST take its configuration, collector
files and static targets from the target allocator rather than from files of
its own (allocator mode), as the bundle of files the allocator serves it
(`internal/allocapi`). `--allocator.url` MUST be an `http://` URL with a
host and no user, query or fragment. `--allocator.cache-dir` MUST be given
with it. `--allocator.url` together with `--config.file` or
`--static-targets-file` given explicitly, and an `--allocator.` flag without
`--allocator.url`, MUST be command-line errors (exit status `2`) naming the
flags, at a start and with `--dry-run` alike. `--allocator.member` MUST
default to the host name, and `--allocator.advertise` to the
`--web.listen-address` host when it is one address that is neither loopback
nor unspecified, else to the host's first address that is neither loopback
nor link-local, IPv4 before IPv6, with the listen port; an
`--allocator.advertise` that is not `host:port` MUST be refused.
`--allocator.capacity` MUST default to 1, MUST be refused as a command-line
error outside `0.1` to `100` or with more than three decimals, and MUST be
said as `capacity` in every heartbeat. The
`--allocator.member` name MUST be the value of `otlp.exporter_instance` that
the bundle's configuration leaves out (§ 42.1).

The exporter MUST heartbeat (`POST /v1/heartbeat`, an `allocapi.Heartbeat`)
at once, every heartbeat interval, and at once after each bundle it
put in force or refused, to every address the URL's host resolves to, each at
the URL's port with the URL's host as its `Host` header, or to the host alone
when it is an IP address. Each address MUST be heartbeated on a schedule
of its own, so an instance that is slow to answer or hangs delays no heartbeat
to another; a heartbeat to one address, and the lookup of the addresses,
made at once and every heartbeat interval, MUST be given up on after 5
seconds or the heartbeat interval, whichever is shorter, and a lookup that
fails MUST keep the addresses it resolved to last. A heartbeat MUST carry the member, the advertised
address, its `--web.static-targets-path` as `static_targets_path`, the generation in force (empty before the first), the last refused
generation with its error until a later bundle is put in force,
`shutting_down` from the first `SIGTERM` or `SIGINT` on, the `instance`, a
random name made once at the exporter's start, the `sequence`, 1 for the
process's first heartbeat and one more for each heartbeat built after it, so
that a heartbeat given up on and arriving late is known for an old one, and
`interval_seconds`, once a reply has said a `heartbeat_interval_seconds`
other than 0, and MUST leave `interval_seconds` out before. `interval_seconds`
MUST be the longest of the intervals the last answer of each instance of
the allocator said, each brought within `1s` and `2m`, an instance being
the reply's `instance`, or the address answered at for a reply without
one; an instance's MUST be kept while heartbeats to its address fail, and
dropped when its address no longer resolves or its last answer is the
member TTL (`allocapi.MemberTTL`) of the longest of them, or of the
interval in force when that is longer, old; when none of them says one, it
MUST stay as it is. When it grows to an interval longer than any it was
before, the first time it is set included, a heartbeat MUST be sent to
every address at once, not at the next turn of the interval; when it grows
to one it was before, the same MUST happen only when the last time was at
least an interval ago, and when it shrinks it MUST wait for the next turn.
On the shutdown signal the
exporter MUST send one at once, bounded by 2 seconds, before
`--web.shutdown-delay` begins. Heartbeats to an address MUST be sent one at a time, each
built once the one before to it is back, so no heartbeat without `shutting_down`
can reach the allocator after the one with it: a heartbeat on its way at
the signal MUST be waited for, within the 2 seconds, and when it is not
back by then the heartbeat with `shutting_down` MUST follow as soon as it
is. With `--allocator.token-file`, every request
MUST carry `Authorization: Bearer <token>`, the file read again for each.
A failure repeated at every try MUST be logged once, for each stage and,
for a heartbeat, for each address, until it changes or ends.

The heartbeat interval MUST be `allocapi.DefaultHeartbeatInterval`, 10
seconds, at the start, and the exporter MUST NOT have a setting of it. Of
the instances, as for `interval_seconds`, not dropped, whose last answer
came without error and with no heartbeat to its address failing since,
those whose reply said a `heartbeat_interval_seconds` other than 0 MUST make
the interval the shortest of them, each first brought within `1s` and `2m` (one
below, a negative one included, taken as `1s`, one above as `2m`), and the
next heartbeat to every address MUST follow that interval after the last
one to it, and the next lookup of the addresses that interval after the
last lookup ended, at once when that is past; when none says one, because every instance failed or none sends the
field, the interval MUST stay as it is. A change of the interval MUST be
logged, and only a change: instances answering one address in turn with
different intervals change nothing while each keeps its say.

The exporter MUST ask for its bundle with `GET
/v1/bundle?member=<member>&wait=30s`, with `If-None-Match: "<generation>"`
naming the last generation it fetched (put in force or refused), quoted as an
entity tag, and MUST take a `304`, or a bundle of that generation or of the
one in force, as no change. A `503` with a `Retry-After` of seconds, the
allocator's answer while it has no member list, MUST NOT be counted or logged
as a failure: the next request MUST follow after `Retry-After`, at least 1
second and at most 30, or sooner when a heartbeat reply names a generation
other than the last fetched. Which generation was last fetched MUST be read
when the heartbeat's replies are in, not when it was sent: a reply naming a
bundle fetched while it was on its way MUST NOT cut any wait short. A failed request or write MUST be tried again
after a wait doubling from 1 second to 30, which a heartbeat reply MUST NOT
cut short, so a failure that persists does not fetch the whole bundle at
every heartbeat; an answer without a change
that came within 1 second MUST wait 1 second before the next request, or
less when a heartbeat reply names a generation other than the last fetched,
so the exporter never asks as fast as the allocator answers.

A new bundle MUST be refused, before anything is written, when
`allocapi.CheckFiles` refuses it (a path that is not relative, leaves the
bundle's directory, or names a file that another path's whole leading
components also name, as `a` and `a/b` do, which no directory can hold; or
no `config.yaml`), when its generation is not
`allocapi.Generation` of its files, or when a top-level name begins with
`..`. Otherwise it MUST be written into a new directory of the cache
directory, files `0600` and directories `0700`, and checked there as a
reload would check it, the configuration and the static target file
together; a bundle that fails MUST be refused, its directory removed and the
files in force kept. A bundle that passes MUST be put in place by replacing
one symbolic link, `..data`, through which the cache directory's top-level
names link, under the configuration manager's reload lock and followed by a
reload of both files with the trigger `allocator` under the same lock, so no
reader opening a file through the cache directory finds files of two
generations, and no reload reads them half replaced. When that reload is
refused nonetheless, the previous files MUST be put back and reloaded, and
the bundle refused. When putting them back fails, the bundle MUST be counted
as a failure to write, neither refused nor taken as fetched, and tried
again, and before the next bundle is written the files in force MUST be put
back, and reloaded, under the reload lock. The other generations'
directories MUST be removed once a bundle is in force, and a refused
bundle's at once, but the directory `..data` names MUST never be removed
nor written again. With no generation in force, a bundle of the generation
`..data` names whose directory holds its files, as a cached bundle that did
not load at startup is when the allocator sends it again, MUST be checked
and reloaded in that directory, unwritten, so `config.yaml` stays readable
throughout and a refusal leaves the directory and `..data` as they were.
Every file MUST be synced before its directory, every directory of a
generation after the files and directories in it and before the swap, and
the cache directory after the new links are made, before `..data` is
replaced, and again after it is, so a restart after a crash finds the
generation before or the whole new one. A refused bundle MUST NOT be
fetched again until the allocator serves another generation. When that is
the generation in force, nothing MUST be written or reloaded, the refusal
MUST leave the heartbeats at once, and the next bundle request MUST name
the generation in force, so the allocator holds the request until it has
another bundle.

The manager MUST read `<cache-dir>/config.yaml` and
`<cache-dir>/static-targets.yaml` with the loader of file mode, so the
`--config.expand-env`, `--static-targets.expand-env` and `--config.watch`
flags apply; an absent static target file MUST be a file without targets. At
startup, a bundle the exporter installed in the cache directory that loads
MUST be put in force at once, ready, whether or not the allocator answers.
Only a bundle the exporter installed is one: `..data` linking to
`..gen-<generation>`, whose files are a bundle of that generation, with each
of its top-level names linking through `..data`. Before anything reads the
cache directory, the other generations' directories and `..tmp-link`, left
by a write a crash or a kill cut short, MUST be removed, and without an
installed bundle also `..data`, the links through it, and `config.yaml` and
`static-targets.yaml`, such as files put there by hand; with one, a
`config.yaml` or `static-targets.yaml` that does not link through `..data`.
Other files MUST be left alone. Without an installed bundle, or with one
that does not load (logged as a warning), the exporter MUST hold an empty
configuration: `/health` MUST answer, `/ready` MUST answer `503` with `not
ready: no configuration yet: waiting for the allocator`, every other
endpoint — `/probe`, the self-metrics, the static targets endpoint, the
landing and collectors pages, the debug page and its runs, and `/-/reload` —
MUST answer `503` with `no configuration yet: waiting for the allocator at
<url>` before anything else, whatever the method, since `web.basic_auth`
comes with the bundle and none of them MUST be served without the
credential the bundle may ask for; and no reload status (§ 22.0b) MUST be
reported until a configuration is in force. Only a reload that puts a configuration in force
MUST end that state. `--dry-run` with `--allocator.url` MUST check the
bundle in the cache directory, fail when there is none, fail a `config.yaml`
there that is not an installed bundle, and change nothing. On shutdown the
exporter MUST stop the allocator client once the probes have finished, and
wait for it, so a bundle being written is put in force or refused before
the process exits.

The exporter in allocator mode MUST expose, from its start:

```text
http_exporter_allocator_generation_info{generation}     gauge
http_exporter_allocator_last_sync_timestamp_seconds     gauge
http_exporter_allocator_sync_errors_total{stage}        counter
http_exporter_allocator_bundle_refused_total            counter
```

`generation_info` MUST be 1 for the generation of the bundle whose
configuration is in force, absent before the first: the generation MUST be
stored with the configuration, in the same step that puts it in force, so
from the moment a bundle's configuration answers probes and makes the
exporter ready the series names its generation, a refused bundle's never,
and a bundle put in force at startup from the cache directory names its
generation from the start. A heartbeat names the generation the client
knows to be in force, which it learns once the reload has returned; the
heartbeat sent at once after it names the new one. The timestamp MUST be the last answer to a bundle request, 0 before
one; `stage` MUST be `heartbeat` (one per address a heartbeat failed for),
`fetch` or `write`, each from zero. An exporter outside allocator mode MUST
have none of them.

## 42.15 Redirect following and HTTP/2 negotiation

Collector target requests MUST support two transport settings, besides
`new_connection` (§ 42.15b):

```yaml
request:
  follow_redirects: false
  enable_http2: false
```

`follow_redirects` controls whether the exporter follows HTTP redirect
statuses on the target request. It MUST default to `false`. When it is false the
exporter MUST return the redirect response itself, so the collector observes the
3xx status rather than being sent to another host silently; a collector whose
`error_handling.on_fetch_error` is `fail` therefore fails the probe, which is the
intended signal that the target moved. When it is true the exporter MUST follow
redirects using the HTTP client's normal limit, each redirect's URL held to
`allowed_schemes` (§ 3.3) and to `allowed_targets` and `denied_targets`
(§ 26.1), and each redirected request carrying what § 42.15c gives it.

`enable_http2` controls whether the target request may negotiate HTTP/2. It MUST
default to `false`, which is the protocol behaviour the exporter has always had,
because HTTP/2 is negotiated through ALPN over TLS and is therefore relevant to
HTTPS targets only. Cleartext HTTP/2 MUST NOT be attempted.

Both settings MUST be overridable per scrape through the `/probe` parameters
`follow_redirects` and `enable_http2`. Both MUST accept exactly `true` or
`false`; any other value MUST return HTTP 400 before the target is contacted, so
a typo cannot silently select a default. An absent parameter MUST leave the
collector's own setting in force, and the presence or absence of either
parameter MUST be part of the response cache key, so a scrape that requested
different transport behaviour never reads another scrape's cached result.

Static targets MUST accept both settings in their `request` block with the
same semantics.

The Helm chart MUST expose both, and `new_connection` and `ip_family`, as
list-valued `params` entries on each `monitors` item.

### 42.15b Connection reuse

Requests to targets and OTLP exports MUST reuse their connections. The
exporter MUST keep one connection pool per distinct set of TLS settings — the
CA, client certificate and key files, `insecure_skip_verify` and
`server_name`, a static target's own `tls` block being its set — HTTP/2
choice, `ip_family` (below), proxy (`request.proxy_url` and its credentials' files, below) and
Unix socket (`request.unix_socket`, below), shared by every collector and scrape that uses the same set, rather
than a pool per request, which would reuse no connection, pay a TLS handshake
on every HTTPS scrape, and leave each idle connection open until the other end
closed it. A scrape overriding `insecure_skip_verify` or `enable_http2` MUST
use the pool of its own settings. An `http` or `graphite` collector with
`request.new_connection: true` MUST instead send every request — each
attempt, each request a followed redirect makes, each page — on a
connection made for it, which no other request uses and which is closed once
its answer has been read, so that every probe looks the target's name up,
connects, and makes and verifies the TLS handshake again, and `$timings` and
`$tls` (§ 17) are of that connection; a connection kept from an earlier
request would answer with the address and the certificate it was made with.
It MUST default to `false`, MUST be overridable by the `new_connection` probe
parameter, accepting exactly `true` or `false` as `follow_redirects` is
(§ 42.15) and part of the cache key, and MUST be settable by a static target
of either type in its `request` block. A login's request MAY use a kept
connection.

`request.ip_family` of an `http` or `graphite` collector MUST choose the
address family every request of the collector's trip — each attempt, each
redirect followed, each page — connects to the target over: `ip4` and `ip6`
MUST dial that family alone (Go's `tcp4` and `tcp6`), and a target with no
address of it, a name whose lookup for the family finds none or an address of
the other family written as the target, MUST fail with an error saying the
target has no address of that family and naming `request.ip_family`;
`prefer_ip4` and `prefer_ip6` MUST dial that family first and the other when
the first has no address or none of its addresses takes the connection,
giving the first family at most half of the time left under the request's
deadline, the probe's or its own timeout, so that a family whose address
never answers leaves the other its half within the same probe, though the
HTTP client dials with a context detached from the request's; left
out, the exporter MUST dial as Go does, both families. Any other value MUST be
refused at load, naming the collector, and the schemas MUST refuse it. It
MUST be overridable by the `ip_family` probe parameter, whose value MUST be
one of the four, any other, an empty one included, answered `400` before the
target is contacted, and part of the cache key, and MUST be settable by a
static target of either type in its `request` block, checked at load. The
family MUST be part of the pool's settings, so a connection made over one
family never carries a request that asked for the other. The exporter MUST
NOT apply it to a connection it does not make to the target: it MUST be
refused at load beside `request.unix_socket` and beside a `request.proxy_url`
other than `direct`, as a probe parameter or a static target's key of such a
collector, and a request with it that the environment's proxy would carry
MUST fail, saying so, before the proxy is asked anything; a proxy looks the
target's name up and chooses the family itself. A login's request MAY be made
over either family. A TLS file rotated on disk MUST be picked up
by the next request, with the previous pool's idle connections closed. Idle
connections MUST time out (90 seconds), and a pool unused for five minutes —
such as one a reload made obsolete — MUST be closed and dropped. A TLS
handshake with a target MUST be given up after 10 seconds, whatever is left
of the probe's deadline; a pool MAY be built with another limit, which the
tests do (§ 34) and the exporter MUST NOT. A response
body MUST be read to its end before it is closed where the exporter can, so
the connection returns to the pool. A `101 Switching Protocols` answer MUST
NOT have its body read: what follows it on the connection is the protocol
switched to, such as a WebSocket's frames, which may never end. Its body
MUST be empty, and its connection MUST be closed at once, kept for no
other request, so a probe of a WebSocket endpoint's handshake ends when the
handshake is answered. The `Connection` and `Upgrade` headers of a
collector's `request.headers` MUST be sent as written, so an `http`
collector can ask for the upgrade.

Every transport MUST send its requests through the proxy the environment names:
`HTTPS_PROXY` for `https` URLs, `HTTP_PROXY` for `http` ones, `NO_PROXY` for
the hosts, domains, addresses and CIDR ranges reached directly, and the
lower-case spellings, as Go's `golang.org/x/net/http/httpproxy` interprets
them; requests to localhost and loopback addresses MUST go direct. This MUST
apply to target requests and OTLP exports alike. The environment MUST be read
when a transport is built rather than once per process, so a test can set it.
An `http` or `graphite` collector MAY name its own proxy, `request.proxy_url`,
which MUST then carry every request of the collector — its retries, pages,
followed redirects and login included, `localhost` too — in place of the
environment's; `direct` MUST send them to their targets without a proxy,
whatever the environment names, and leaving the key out MUST leave the
environment's in force. `proxy_url` MUST be `http://`, `https://`,
`socks5://` or `socks5h://` with a host and an optional port from 1 to 65535,
and nothing else; a path, a query, a fragment, a placeholder, an unknown
scheme and userinfo MUST be refused when the configuration loads, the
refusal naming the URL by its scheme and host alone, never the credentials
or path written in it. Credentials for the proxy MUST come from
`request.proxy_basic_auth_file`, `username` and `password` files, both
required, read at every request and sent as `Proxy-Authorization` or as the
SOCKS5 username and password; the files without a `proxy_url` to send them
to MUST be refused. A credential file that cannot be read, or is empty,
MUST fail the request at once, without its retries, as an unreadable
`basic_auth_file` does. The scheme MUST be taken in any case
(`HTTP://proxy:3128`), by the exporter and the schemas' pattern alike. The TLS connection to an `https://` proxy MUST be
verified against the system's roots, with the proxy's host as the server
name and no client certificate: the collector's `tls` block MUST apply to
the target alone, through the tunnel. The proxy and its credentials' files
MUST be part of the pool's settings, so collectors with different proxies or
credentials never share a connection. The proxy is configuration: no probe
parameter and no static target MAY set it. A `CONNECT` the proxy answers
other than `200` MUST fail as the proxy's refusal, naming the target and the
proxy's status (`the proxy refused the tunnel to api.example:443: 403
Forbidden`), the environment's proxy and a collector's alike.

An `http` collector MAY name a Unix domain socket, `request.unix_socket`,
over which every request of the collector — its retries, pages, followed
redirects and login included, whatever host each names — MUST then be sent in
place of a TCP connection to the URL's host, as `curl --unix-socket` sends
it. The URL's host MUST still be the `Host` header and, for an `https` URL,
the TLS server name, the handshake being made over the socket with the
collector's `tls` block; its path and query MUST be the request's as without
the socket. The host MUST NOT be looked up or dialed. No proxy, the
environment's included, MUST apply to such a collector. The socket MUST be
an absolute path; one of nothing but blanks, a relative one, one with a
placeholder or a control character, and one beside `proxy_url` or
`proxy_basic_auth_file` MUST be refused when the configuration loads, and
`""` MUST be the key left out, by the exporter and the schema alike. A path
longer than the `sun_path` of the system the exporter runs on holds, less
its closing NUL — 107 bytes on Linux, 103 on macOS and the BSDs — MUST be
refused when the configuration loads, by the exporter alone, with a message
giving the path's length and the limit; the schema MUST NOT bound the
length, which no one value would state for every system. The
socket MUST be part of the pool's settings, so collectors with different
sockets never share a connection, and none shares one with a collector
that dials TCP. The socket is configuration: no probe parameter, no static
target and no `defaults` block MAY set it, and its path MUST NOT be held to
`allowed_targets` and `denied_targets` (§ 26.1). A failure to reach it MUST
name its path in the exporter's log, and the path MUST be withheld from a
probe's caller (§ 25).

An `http` collector MAY speak FastCGI to its target, `request.fastcgi`, a
block of `document_root` and `params`, `{}` written with neither. Its every
request, its retries, pages and followed redirects, MUST then be sent as a
FastCGI responder's request (FastCGI 1.0, `FCGI_KEEP_CONN` unset) on a
connection of its own to the URL's host and port, 80 without one, or to
`unix_socket`, through the pool's dialer and so the target policy: the
parameters MUST be `GATEWAY_INTERFACE` `CGI/1.1`, `REQUEST_METHOD`,
`REQUEST_URI`, as the request writes it, `SCRIPT_NAME` and `DOCUMENT_URI` the URL's path decoded,
`SCRIPT_FILENAME` the path joined to `document_root`, the path alone
without one, `DOCUMENT_ROOT`, `QUERY_STRING`, `SERVER_NAME`, `SERVER_PORT`,
`SERVER_ADDR`, `REMOTE_ADDR`, `CONTENT_LENGTH` with a body, `CONTENT_TYPE`
from its header, each header but `Content-Type`, `Content-Length` and
`Proxy` as `HTTP_` and its name in upper case with `_` for `-`, and each of
`params` in place of one of the same name, a length of 128 or more written
in four bytes; the body MUST be the request's stdin. The application's
stdout MUST be read as a CGI response: its `Status` header the status, 200
without one, a `Status` that is no HTTP status failing the request, its
other headers and body the answer's; the first 4 KiB of what it wrote to
stderr before its headers MUST be the answer's `X-Fastcgi-Stderr` header,
control characters as blanks. An answer that is no FastCGI record MUST fail
the request, saying so; an end of the connection before `FCGI_END_REQUEST`
and an end with a status other than complete MUST fail it. No proxy, the
environment's included, MUST apply; `request.fastcgi` beside `proxy_url`,
`proxy_basic_auth_file`, `login` or `tls`, a `document_root` that is not
absolute or holds a placeholder, and a parameter whose name is no CGI
variable's (letters, digits and `_`, not starting with a digit) or whose
value holds a placeholder or a control character MUST be refused when the
configuration loads, and a target that is not an `http://` URL, a bare
`host:port` being one, when it is checked; a redirect to an `https://` URL
MUST fail the request, never be followed over HTTPS. `request.fastcgi` MUST be part of the pool's settings, so a pool of
HTTP connections never carries a FastCGI request.

These settings replace the earlier `request.redirect_policy` string, which MUST
NOT be accepted any more. Because the configuration decoder rejects unknown
fields, a configuration still carrying it fails to load rather than silently
changing how a collector follows redirects.

### 42.15c What a followed redirect carries

A target that redirects chooses where the next request goes, so the exporter,
not the HTTP client's defaults, MUST decide what that request carries. The
rules apply to every followed redirect of an `http` or `graphite` collector's
request, whether `follow_redirects` is the collector's or was switched on by a
probe or a static target.

1. The *origin* of a URL is its scheme, its host and its port, a port left out
   being the scheme's own (80 for `http`, 443 for `https`). Hosts MUST be
   compared as the URLs write them, without case, and not in a canonical
   form: the verdict MUST be about the name the request is routed by. A host
   with a final dot and the same host without one MUST be two hosts, as a
   resolver takes them for; so MUST two spellings of one address, two zones
   of one IPv6 address, and a port written otherwise than the other URL
   writes it, the scheme's own port left out aside. A host the target policy
   refuses (§ 26.1) is no origin, and a redirect to one MUST NOT be followed:
   that is every host written with a character outside ASCII, whatever it
   converts to, so an internationalised name is trusted, and redirected to,
   in its `xn--` form alone. A redirect MUST be *trusted* when the URL it leads to
   has the origin of the request's first URL — the target with the
   collector's path and query applied — or when that URL's host matches an
   entry of the collector's `request.redirect_trusted_hosts`, and MUST NOT be
   trusted otherwise: a subdomain of the first host, the same host on another
   port and the same host over another scheme, the upgrade from `http` to
   `https` included, are other origins. Each
   redirect of a chain MUST be judged on its own, against the first URL and
   the list, whatever the redirects before it were: a chain that leaves the
   origin for a host that is not trusted and returns MUST carry everything
   again once it is back.
2. A trusted redirect MUST carry every header the first request carried: the
   collector's `request.headers`, the headers forwarded from the probe or set
   by a static target, and the credentials — `Authorization`, however it was
   made (`bearer_token`, `basic_auth`, their file forms, a forwarded or a
   static target's own), and `Cookie` — also where the HTTP client alone
   would have dropped them for another host. A credential in the userinfo of
   the target URL belongs to that URL: it MUST NOT be carried to a URL a
   redirect names with a host of its own, trusted or not, and is kept only
   across a redirect whose `Location` is a path. A user and password in the
   `Location` of a trusted redirect are left to the HTTP client, which sends
   them as basic auth only when the request carries no `Authorization`: the
   first request's own `Authorization` MUST be the one sent. A `Host` the first request
   set names a virtual host of the origin: it MUST NOT be sent with a
   redirect that leaves the origin, trusted or not, and on the origin is
   kept as the HTTP client keeps it, across a redirect whose `Location` has
   no scheme. The headers describing a body MUST be left out of a redirect
   that is sent without the body (rule 5).
3. A redirect that is not trusted MUST carry, of what the first request
   carried, only `Accept`, `Accept-Language` and `User-Agent`, with the
   values the first request had. Every other header MUST be withheld,
   whatever its name, and so MUST `Authorization`, `Cookie` and a `Host` the
   first request set. A user and password in the redirect's own `Location`
   MUST NOT be sent either, as an `Authorization` or otherwise, and MUST NOT
   remain in the URL a debug report shows. `Accept-Encoding` stays the
   transport's own (§ 20). The three headers go to every destination, so
   the documentation MUST say that no token belongs in them. The first URL's
   path and query are not carried to another host, a `Location` naming the
   whole URL, and need no rule here.
4. The exporter MUST NOT add a `Referer` to any redirected request, trusted
   or not. A `Referer` set in `request.headers` is a header of the
   collector's: sent on the first request and on trusted redirects, as
   written, and withheld from the others.
5. After a `301`, `302` or `303` a request whose method is not `GET` or
   `HEAD` MUST be sent on as a `GET` without its body, and such a redirect
   MUST be followed, trusted or not. After a `307` or `308` the request is
   sent again with its method and its body: to a trusted destination the
   body MUST be sent again; a redirect that would send the body to a
   destination that is not trusted MUST be refused before anything is sent
   there, failing the request with an error that names the redirect's URL,
   its credentials withheld as a debug report withholds a URL's (§ 42.17),
   the host, and `request.redirect_trusted_hosts`, and MUST NOT be retried.
   This MUST hold however the body arose: `request.body`, a `body` probe
   parameter or a static target's, and the form a `graphite` request posts
   for long expressions. A redirect without a body MUST be followed, with
   rule 3's headers when it is not trusted, unless rule 5a refuses it.

   5a. The collector's TLS client certificate (`request.tls.cert_file` and
   `key_file`), or a static target's own, in the `tls` block that replaces
   the collector's for it (§ 42.14), is a credential that every TLS
   connection of the request presents. When the request has one, a redirect over `https` to a
   destination that is not trusted MUST be refused before any connection is
   made there, with an error that names the redirect's URL, redacted as in
   rule 5, the destination, the certificate and
   `request.redirect_trusted_hosts`, and MUST NOT be retried; rule 5's
   refusal, where both apply, is the one reported. A trusted redirect
   presents the certificate, and a redirect over `http` that is not trusted
   presents nothing and MUST be followed. `tls.ca_file`,
   `tls.insecure_skip_verify` and `tls.server_name` are no credentials and
   apply to every hop: with `server_name` set, every `https` host a redirect
   leads to is verified against that name.
6. `request.redirect_trusted_hosts` is a list of host names, globs of host
   names (`*.example.com`; `*` matches any run of characters, dots
   included, and `?` any one) and IP addresses, written as `allowed_targets` writes them (§ 26.1);
   `"*"` alone trusts every host. An entry MUST be matched against the host
   of the redirect's URL as it is written there (rule 1), without case, on
   any port and over any scheme `allowed_schemes` allows: a name or glob
   with a final dot MUST match only a host written with one, and one without
   only a host written without; an address entry MUST match a host
   written as that address, in any of its textual forms and with the same
   IPv6 zone or none, and nothing else; no entry, `"*"` included, MUST match
   a host written with a character outside ASCII.
   No name MUST be resolved to decide trust. At load the exporter MUST
   refuse an empty entry, a CIDR network, a host with a port, an entry with
   a character outside ASCII, as `allowed_targets` refuses it (§ 26.1), and
   anything else that is not a host name, a glob or an address, such as a URL or an
   IPv6 address with a final dot (`1.2.3.4.` is a name, matched as written), each
   with a message that says what to write instead and, for a network, does
   not propose a glob. It MUST also refuse a glob that holds no letter,
   hyphen or underscore: one of digits, dots and wildcards, such as `10.*`
   or `192.168.*.*`, which is matched against names and would trust
   `10.evil.example`, with a message that says so and that addresses are
   listed one by one; and wildcards alone in any spelling but `"*"` — `**`,
   `*.`, `[*]`, `*.*`, `?`. The JSON Schema MUST
   refuse the same, but for an address-like entry that is no address. The
   documentation MUST say that `*` matches dots, with `*example.com` and
   `api.example.*` as what not to write. The
   key MUST be accepted for `http` and `graphite` collectors, with or
   without `follow_redirects`, and refused for the other request types as
   any key of another type is. It MUST be the collector's alone: no `/probe`
   parameter MUST set or extend it — a probe naming `redirect_trusted_hosts`
   is handled as one naming a parameter the exporter does not know — and a
   static target's `request` block MUST refuse it. A changed list MUST be a
   changed collector for the response cache and for static targets
   (§ 42.13).
7. Whether a redirect is followed at all MUST remain decided by
   `allowed_schemes`, `allowed_targets`, `denied_targets` and the client's
   limit of ten redirects, before anything is sent to where it leads and
   before rule 5's refusal is considered.
8. A debug probe (§ 42.17) MUST list each redirected request with the
   headers it was sent, and under a redirect that is not trusted the names,
   never the values, of the headers withheld from it and the reason. When
   the answer whose status fails a probe, in the `http_status` stage, came
   from a host a redirect
   led to and from which headers were withheld, the error MUST say that the
   collector's headers and credentials were not sent to that host and name
   `request.redirect_trusted_hosts`; an answer from the target itself, from
   a trusted redirect, or from a host nothing was withheld from MUST be
   reported as before. A credential in the userinfo of the target URL, when
   the HTTP client made the first request's `Authorization` of it, MUST
   count as withheld from a redirect that is not trusted, and be named so in
   the debug report. Where such a redirect leads to the first URL's own host
   under another scheme or on another port, this error, the debug report's
   reason and the refusals of rules 5 and 5a MUST name both origins, each
   with its scheme and the port it writes, and not the host alone. A request that follows no redirect MUST NOT be made
   to do any of this work.
9. These rules MUST NOT change what a request that follows no redirect, or
   only redirects on its own origin, sends, but for the `Referer` of rule 4.
   The OTLP exporter's own requests (§ 42.1) MUST NOT follow redirects at
   all: Go would send `otlp.headers`, which carry the endpoint's API keys
   and tenant, and the body to wherever a redirect points, another host and
   plain `http://` included. A 3xx answer to an export MUST be handled as
   § 42.1 says. A retry MUST start again at the first
   URL with everything the first request carried.

## 42.15a Environment variable expansion in configuration

The exporter MUST support an optional `--config.expand-env` flag that
substitutes `${NAME}` references in the configuration document and in its
collector files (§ 5.0), and an optional `--static-targets.expand-env` flag
that does the same for the static target document (§ 42.14), from the process
environment before each is parsed. The two MUST be independent: each document
MUST be expanded only when its own flag is on, so an operator can take the
static targets' addresses and credentials from the environment while the
configuration is used as written, or the other way round. Everything below
applies to both, and an error about an unset variable MUST name the flag that
asked for the expansion.

It MUST default to off. A configuration legitimately contains dollar signs that
are not references — a regex metric rule, a jq expression, a Python pre-script —
and expanding by default would rewrite an operator's own text without being
asked. With the flag off, a document MUST be used exactly as written.

Only the braced form MUST be treated as a reference. `$NAME` MUST be left
untouched even with the flag on, so regexes and shell-style text keep working.
`$$` MUST produce a literal dollar, so `$${NAME}` survives as `${NAME}`. A name
MUST match the usual environment variable spelling; anything else, including an
unterminated `${`, MUST be left alone rather than reported as an error.

A reference to a variable that is not set MUST be an error that stops startup,
not an empty substitution. An empty substitution yields a document that parses
and is wrong — a collector with no path, a credential that is silently blank —
which the exporter would then serve. Every unset name MUST be reported in one
message, and the message MUST name the document. A variable that is set to an
empty string MUST substitute normally: that is a deliberate choice. As the
whole of an unquoted value it MUST give what the value written `""` gives —
loaded, or refused for the reason `""` is — and MUST NOT become YAML's null,
which no key takes.

A reference MUST be expanded where the document holds it as a value — a
scalar value or key, plain or quoted, whole or in part — and the expansion
MUST read back as exactly the variable's value, whatever it holds: a ` #`
MUST NOT start a comment, a leading `*` or `&` MUST NOT become an alias or an
anchor, and a line break MUST NOT end the line and turn the rest into YAML. A
variable supplies a value, never structure. The expansion SHOULD read as the
value would written by hand — unquoted where that reads it back unchanged, so
a number stays a number, and a double-quoted string otherwise. A reference in
a comment MUST be left alone, set or not. This MUST hold in a flow sequence
or mapping, where a bare reference is not valid YAML until it is expanded,
whether the reference is the whole value or key, part of a longer one, as in
`{Authorization: Bearer ${TOKEN}}`, or stands beside quoted values, as in
`{X-Name: "x", X-Token: ${TOKEN}}`; after an anchor; and for values YAML
reads specially on their own:
an expanded key, an empty value and `-` MUST always be written quoted. Reading a document with a
bare flow reference MUST NOT change how any other value of it expands: a
reference in a block value, or in a plain value elsewhere, MUST expand to
the variable's value exactly as in a document without one, and nothing the
exporter adds to read the document — a quote, a placeholder — may end up in
a value. Text of the document that only looks like what the exporter writes
in a reference's place while reading MUST be kept as written. A block value
(`|` or `>`) MUST be expanded in every one of its lines, also when its header
carries an indentation indicator, as in `|2`, and its first line is indented
further than the rest. Where a value cannot be rewritten
in place — a block value given a line break, an unquoted value folded over
lines, an explicitly tagged one — the document MUST be refused naming the
line and how to write it. The expanded document MUST keep the file's line
numbers, which its errors name.

A reload MUST read the documents the way startup did. A reload that stopped
expanding would replace a working configuration with one full of literal
references, and a reload that started expanding would rewrite a configuration
the operator never asked to have expanded. A reload whose references cannot all
be resolved MUST be rejected with the previous configuration left active.

Environment references MUST NOT be confused with path parameters (§ 42.10a),
which use `{{param_<name>}}` and are bound per probe rather than at load time.

## 42.16 Automated dependency updates

Dependency updates MUST be proposed automatically and MUST never be applied
automatically: every update arrives as a pull request a maintainer merges.

The Go modules and the GitHub Actions used by the workflows MUST be updated by
Dependabot, configured in `.github/dependabot.yml`. The minor and patch updates
of each MUST be grouped into a single pull request, because these move together
and reviewing one green build is better than triaging one pull request per
module. The Go modules' major version updates MUST be excluded, by an `ignore`
of `version-update:semver-major` for every module: a Go major version lives at
a different import path and needs code changes, so it is deliberate work rather
than an automated proposal. The GitHub Actions' major version updates MUST be
proposed, in a group of their own apart from the minor and patch one: every
action is pinned to a commit SHA, so a new major runs only once its pull
request is merged, and in a pull request of its own it is reviewed by itself
and holds up no minor or patch update. Tests MUST keep the Go modules' ignore
and the Actions' two groups by reading the configuration.

The Python modules that only the tests use — `prometheus-client`, for its
strict OpenMetrics parser — are no part of the image and MUST be pinned, each
to one version, in `test/python/requirements.txt`, and updated by Dependabot's
`pip` updates of that directory, which MUST stay within each module's major
version by the same `ignore` as the Go modules': a new major of the parser can
read the exposition formats differently, which the suite's verdicts rest on.
Every workflow that runs the test suite MUST
install that file before the suite and MUST run the suite with
`STRICT_OPENMETRICS_PARSER` set, and the tests that give answers to that
parser MUST then fail, with the message they are otherwise skipped with, where
`python3` or the module is missing: a run of a workflow MUST NOT pass with
those tests left out. Without the variable they MUST be skipped there, so the
suite runs on a machine without the module. CI MUST run the Go suite when that
file changes. Tests MUST enforce the install step, the variable and the
Dependabot entry by reading the workflows and the configuration.

The versions pinned in the Dockerfile MUST be updated by a scheduled workflow
rather than by Dependabot. They are declared as build arguments and interpolated
into the `FROM` lines and the `pip install`, which the Docker ecosystem updater
cannot resolve; a configuration that appears to cover them would silently
propose nothing.

The resolver MUST live in the repository as ordinary Go code under `tools/`, so
its rules are covered by the same test suite as the exporter, and MUST:

- never cross a major version, for any pin;
- never move `PYTHON_VERSION` to another Python feature release (its second
  component, `3.12` to `3.13`): collector scripts run on that interpreter, a
  feature release removes standard-library modules and changes behaviour, and
  the suite cannot cover scripts it does not have. A `PYTHON_VERSION` pinned to
  a patch release MAY move within its feature release. Go's version and the
  Python libraries MUST keep moving by minor and patch versions;
- when a newer Python feature release is published in the image form the build
  pulls, say so in the pull request summary, naming the release, the one
  `PYTHON_VERSION` stays at, and that it moves by hand only (`Python 3.14 is
  available; PYTHON_VERSION stays at 3.12 until it is bumped by hand, because
  collector scripts run on it`), and in the run's output. That line MUST appear
  only in a summary written for other updates: a newer Python alone MUST NOT
  write a summary or change the Dockerfile, so it opens no pull request;
- keep each pin's granularity, so a two-component pin such as `1.27` is only
  ever replaced by another two-component version. A two-component image tag is a
  floating tag that already picks up patch rebuilds, so rewriting it to
  `1.27.4` would freeze it and make the pin worse rather than better;
- reject pre-release, release-candidate and development versions, and accept
  PEP 440 post-releases, which one of the pinned Python packages uses;
- draw image candidates from the exact tag form the build pulls, so a proposed
  version is known to exist as `golang:<version>-alpine` rather than merely to
  have been released upstream;
- ignore PyPI releases whose files have all been yanked, which pip will not
  install; and
- fail rather than silently propose nothing when a source cannot be reached, or
  when the build argument it is asked to rewrite is missing or declared more
  than once.

The pin table and the Dockerfile MUST be kept in agreement by a test, so a
renamed or removed build argument cannot leave a dependency unwatched.

The CI workflows MUST build with the current stable Go release rather than a
pinned version, so the build follows Go's releases without anyone editing a
workflow. The release workflow MUST build its binaries with the Go version the
Dockerfile pins, resolved to that minor's newest patch release, so a release's
archives and image carry the same Go. A workflow MUST NOT install Go from the `go`
directive (`go-version-file`), since that installs exactly the minimum the
module declares, which may be a patch release missing security fixes. The pinned golangci-lint version MUST be a release that
supports that Go: golangci-lint type-checks with the `golang.org/x/tools` it was
released with, which cannot read the standard library of a newer toolchain or a
newer export data format (Go 1.27.2's against v2.13.2) and fails rather than
reporting a lint finding. The linter version MUST be pinned
identically in the Makefile and in CI, and a test MUST keep the two in step, so
that a clean local `make lint` continues to mean a clean CI run. The `go` directive in `go.mod` MUST remain the minimum the
module requires — it is raised by dependency updates, not by the toolchain the
build happens to use — and the Dockerfile MUST pin a Go version that satisfies
it.

`make helm-test` MUST make every chart check the CI workflow makes — the same
renders, the same values that must be rejected, the same lines looked for in
what is rendered — and no other, and a test MUST compare the two, so that a
clean local run means a clean CI run and the reverse. `make helm-test` MUST
refuse any helm but the pinned release, reading a version without a patch,
the release line a helm built from source reports (`v4.3`), as its `.0`
release (`v4.3.0`) and comparing any other whole, and `make helm-install`
MUST install one it accepts: built from source with `go install`, helm
reports only its release line (`v4.3`) unless the version is set at link time
as helm's own release build sets it
(`-X helm.sh/helm/v4/internal/version.version`), so the target MUST pass that
flag with the pinned version.

`make ci` MUST run every go command the CI workflow's jobs run, beside
the `go mod download` that fills their module cache, and reach every make
target the jobs run other than the installing ones, so that a clean local
`make ci` means a clean CI run; every target of the Makefile MUST be
`.PHONY`, since none makes a file of its name. A test MUST check both.

Every workflow step that runs `go mod download` MUST try it up to three
times, waiting between tries: the module proxy now and then breaks off a
download (`stream error: … INTERNAL_ERROR`), which would otherwise fail a job
for nothing the change did. A test MUST check every such step of the
workflows.

A dependency update can raise the go directive in a pull request that touches no
workflow, which leaves every later build failing with `go.mod requires go >= X`
and nothing nearby to explain it. A test MUST therefore verify that every Go
version a workflow requests, and the version the Dockerfile pins, satisfies the
go directive; a workflow requesting the current stable release satisfies it by
definition.

The scheduled workflow MUST run the resolver, and when anything moved MUST run
the full quality suite and build the container image against the new versions
before opening a pull request. A proposed version that does not build or breaks
a test MUST fail the workflow rather than arrive as a pull request that looks
ready to merge.

The workflow opens the pull request with the repository's `GITHUB_TOKEN`. The
documentation MUST say which repository setting that needs ("Allow GitHub
Actions to create and approve pull requests", without which the run fails at
its last step after pushing the `deps/dockerfile` branch), and that a pull
request opened with that token does not start `ci.yml`, so the suite the
update workflow ran is the proposal's test until the pull request is closed
and reopened.

Both schedules MUST land during Bulgarian working hours on a working day.
Dependabot MUST use an explicit `Europe/Sofia` timezone so the run does not
drift when daylight saving changes. The workflow's cron is evaluated in UTC,
which has no daylight saving, so its hour MUST be chosen to fall mid-morning in
Sofia in both halves of the year.

## 42.17 Debugging collectors

One flag, `--web.debug`, MUST set how far collectors can be debugged: `off`,
the default; `edit`; or `full`, in any case. Any other value MUST be a
command-line error (exit 2, before `--dry-run` or startup). There MUST be no
other flag for any part of it but `--web.debug.dangerous` (below), which
lifts what the playground is refused.

- At `off`, `/debug` and `/debug/run` MUST answer `404`, and a debug probe
  and a debug scrape `403`.
- At `edit`, the exporter MUST serve the debug page, `/debug` (below), on pasted
  responses only: nothing a target answers may be shown. A debug probe, a
  debug scrape and a page's run that would fetch a target or scrape a static
  target MUST be answered `403` naming `--web.debug=full`, without
  contacting the target.
- At `full`, the exporter MUST serve the debug page with every source, debug
  probes and debug scrapes.

Every run — a debug probe, a debug scrape, a run of the page — MUST be made
by one engine, given the collector and what it reads, and reported in one
structure: the requests, the response, the stages, what was logged, what a
probe or the scrape would have answered and the series it would have served.
A debug probe and a debug scrape MUST answer it as the text report below,
and the debug page as JSON with the same report beside it.

`/probe` MUST accept `debug`: `true`, `1` or empty turn a probe into a debug
probe, `false` or `0` leave it a probe, and any other value MUST be answered
`400`. A debug probe MUST be answered `403` naming `--web.debug=full` unless
the exporter runs with `--web.debug=full`, at `edit` too, before anything
else about the probe is checked and without contacting the target. It MUST
be authenticated like a probe (§ 42.5).

A debug probe MUST make one trip exactly as a probe would, through the
target policy (§ 26.1), the trip limits (§ 42.13b) and the probe's deadline,
and MUST answer `200` with a `text/plain` report of it, with
`X-Content-Type-Options: nosniff`, since it holds what the target sent:

- the status and reason a probe would have answered, and where
  `cache.stale_if_error` would have answered with the last good result, that
  it would have, and its age;
- every request sent, with retries and redirects, its method, URL, headers
  and outcome, a redirect with the headers it was sent and the names of the
  ones withheld from it (§ 42.15c); a request of `request.pagination` with
  the number of the page it asked for, `(page 2)` (§ 5.1, `http`); a request
  that waited for its turn under `request.rate_limit`, or under an entry of
  `rate_limits` naming the entry, with how long and why,
  its duration counted from when it was sent (§ 5.1, `http`); a `grpc`
  call with its method, metadata and code; a `localfile` read as its file;
- the response's status, headers and body as the target sent them, before
  the body was converted to UTF-8 (§ 6.1a): the `Content-Type` MUST be the
  target's own. The body is shown up to 64 KiB and only when it is text. A
  body that was converted MUST be listed with its size as sent and the
  encoding it was converted from, and shown as the same text in UTF-8, in
  which an XML declaration still names the encoding it was sent in. A
  directory is shown as its files, with their sizes as read. A response of
  `request.pagination` MUST say how many pages it is merged from, or which
  page's status failed the trip, and whether the pages stopped at
  `max_pages` with another to come, and the fetch stage's note MUST give the
  number of pages;
- each stage, its duration and outcome, saying when `error_handling` carried
  on;
- the series by metric name, the rules that gave none, each by the name its
  series are exported under, with the collector's `metrics_prefix` and
  escaped as its `name_escaping` escapes it (§ 21.1), so that a rule whose
  series are in the first list is not in the second, and the rules that
  carried on without some series, each rule by itself with how many and its
  first error, under its metric name and, where several rules of the
  collector export that name, its expression and its `items`; a `prometheus`
  rule without a name MUST be shown as `rule without a name`, not as an
  empty name;
- every line the trip logged, at every level whatever `--log.level` is,
  including rule failures and what a Python script printed;
- a `Lint` section, after the logs, with the metric lint's findings on the
  collector, against the configuration in force, and on the series the run
  made (§ 30.1a), one a line as `[<rule>] metric "<name>": <message>`, or
  `none`, and the rules the collector's `lint.ignore` silences; the same
  for a debug scrape and a run of the page;
- the exposition a probe would have served.

What a report shows is the target's, the scraper's or a script's to make as
long as a response, or longer: the URL a redirect led to, a header's value,
the message of a gRPC status the collector accepts, a metric's name. The
report is read by a person and made in memory whole, so everything in it but
the body, which has its bound, MUST be bounded, and what is cut MUST say so
with its whole length, as an error does (§ 6.1a):

- a line of the report longer than 8,192 bytes MUST be shown by its first
  8,192 bytes, the cut between two characters, followed by `... (N bytes)`
  with the length of the whole line, whichever section the line is of and
  whatever it is made of, a line of the logs and of the exposition
  included. The lines of the body MUST NOT be cut so. The bound is past
  every line whose parts are bounded already, which MUST NOT be cut again:
  a failure at its 2,000 bytes MUST keep its own length in the line that
  quotes it, and the line of what a script printed (§ 16.2) MUST be shown
  whole;
- the URL of a request MUST be listed by its first 512 bytes and its
  length, and a metric's name in the list of series by its first 200 bytes
  and its length, as an error shows them (§ 6.1a), with how the request
  ended and the number of the metric's series after them as before;
- a header's value MUST be shown by its first 1,024 bytes and its length,
  and of the headers of a request or of a response the first 100 MUST be
  listed, in the order of their names, every value of a name counted,
  followed by `... (N headers)` with their number when there are more;
- where a line of the exposition is cut, the section MUST say before the
  exposition that lines longer than the bound are cut, how many, and that
  it is not valid exposition as shown. An exposition without such a line
  MUST be shown as a probe serves it, to the byte.

What the report withholds (below) MUST be withheld before anything is cut:
a cut MUST NOT show what redaction hides, and one that would end inside a
`<redacted>` MUST show it whole instead. A report whose every part is within
these bounds MUST be the report it would be without them, byte for byte, and
the bounds MUST NOT change what a probe answers, what is cached, counted or
exported, or the line that says what a probe would have answered. The number
of the report's lines is bounded by the collector's limits alone: one for
each metric name and for each series, up to `limits.max_metrics`, and one
for each file of a directory read or skipped (§ 5.1a).

The report's copy of the response MUST be made before the decode and MUST
share the fetched body rather than copy it; a trip that is not reported MUST
NOT keep the body the target sent beside the converted one.

A debug probe MUST NOT be answered from or fill the response cache, share a trip with a
probe in flight (§ 42.13a), ask or change the state of `failure_backoff`
(§ 42.13c) — it MUST contact the target while the collector is backing off
from it, and its report MUST then say so, with the failures and the time of
the next try — be counted in any self-metric but
`http_exporter_debug_runs_total` (below) and the Python worker series, or be
queued for OTLP. It reads the cache only for the stale result a probe would
have answered with, and for the response a probe would revalidate, which it
MUST revalidate with the same conditional request and MUST NOT refresh in the
cache (§ 42.13.2). Its failures and recoveries MUST NOT reach the exporter's log or the
failure log's state (§ 25): the next probe's failure is logged as the first.
The one `info` line that a debug probe or a debug scrape was served MUST
be written to stderr alone, and MUST NOT be taken by the log export.

The Python worker series (§ 22.1a), per collector and of the pool, are the
exception, for a debug probe and a debug scrape alike: they count what the
pool's workers did, and MUST count the scripts a debug probe runs as they
count a probe's, the workers it starts and stops and how its runs end. A
debug probe's scripts run in the collector's own workers, and the worker one
starts is the collector's idle worker afterwards: its start counted nowhere
would leave the collector's series showing a worker the collector never
started. They MUST be counted for the collector the debug probe or scrape
read while a reload has not removed it since, wherever a reload that kept it
came, and under no collector once one has (§ 24.1a).
The exporter MUST log one `info` line per debug probe served, naming the
collector and the target.
A debug probe or debug scrape MUST give back its trip slot however its trip
ends; a panic in it MUST be recovered, logged at `error` with its stack, and
reported as the `500` with the fixed text a probe that panicked is answered
(a debug scrape: `target up 0` with that text), and the report's log MUST
show the panic's text with what reads as a credential, a URL's userinfo
among it, withheld (`fetch.RedactText`), redacted before it is cut.

The report MUST NOT show query values, a URL's userinfo, request bodies, or
the values of request or response headers whose names read as a credential's
by the rule below: in any case, `auth`, `cookie`,
`token`, `secret`, `password`, `passwd`, `passphrase`, `passcode`, `key`,
`session`, `signature`, `credential` or `jwt` is in them once their ordinary
words (`author`, `authority`, `monkey`, `keyboard`, …) are taken out, or
they have `sig`, `pwd`, `pw`, `pass`, `sid`, `hmac` or `otp` as a whole
word, and a query parameter's name also `code` or `ticket`. One exception: a request line MUST show the value
of a query pair that the configuration of the collector that made the
request sets as its file writes it — a `request.query` value without a
placeholder, a `graphite` collector's expressions without placeholders, its
`from` and `until` (their defaults when it writes none) and `format=json`, a
`kubernetes` listing's `limit` and the selectors it writes — when the
pair's name does not read as a credential's and no `;` is between the `&`
before it and the one after it, its name and value compared with their
escapes decoded. A value a probe parameter or a placeholder gave, one a
`${NAME}` reference expanded to, the target's own query, a static target's
own request keys and the API server's `continue` token MUST stay masked,
and so MUST every query value of an error, which a probe's caller is told
as well.
A response or request header that carries a URL — `Location` and
`Content-Location`, a URL whole, the URL of `Refresh` after its delay, and
each `<…>` of `Link` — MUST have that URL shown as a request line shows one
whose configuration set no pair: every query value and the value of a path
parameter whose name reads as a credential's masked, its userinfo withheld
and its fragment left out, the path's escapes and the rest of the header's
value kept; a URL that does not parse MUST be shown as `<redacted>`. A
redirect's `Location` is where the next request goes, and carries what it
does.

With `--web.debug=full`, `/static-targets?debug=<name>` MUST scrape the static
target of that name once, as its schedule would — with its own request
settings, headers and credentials, its interval as the budget, and waiting for
a slot of the collector's `max_concurrent_probes` — and answer the same
report, saying what the scrape would have published: `http_exporter_target_up`
`1` or `0`, why, whether a stale result would stand in, and the series with the
target's labels and `static_target`. It MUST NOT publish anything on the
endpoint, count in a self-metric but `http_exporter_debug_runs_total` and the
Python worker series (above) or be exported over OTLP, and MUST skip the
response cache. Below `full` it MUST be answered `403`; a name no target
in force has `404`; more than one `debug` value, an empty one, or `debug`
together with `targets`, `400`.

Every place a URL, a header or text from a target is shown — a displayed
target, a metric label, a debug report, a logged body excerpt — MUST withhold
credentials by the same rules, kept in one place: userinfo is never shown; a
metric label drops the query, a debug report masks its values (but those
of the collector's configuration, above), and a
displayed target — in a log line, the static targets endpoint's `target`
label and the OTLP `target` attribute — masks the values of the parameters
whose names read as credentials, and keeps the rest, in order, so targets
that differ in anything else stay apart; a header value is withheld when its
name reads as a credential's. A query MUST be shown as it was sent (§ 4),
masked where it stands, not parsed and written again: every pair MUST keep
its place and its spelling, a bare key MUST stay one, and a pair with a
malformed escape MUST be shown, its value masked or not by the rule for its
name. Pairs MUST be told apart at `&` and at `;` alike, so the token of
`a=1;token=SECRET` is masked; the text from an `&`, or from the start, to
the first `=` after it MUST be taken for a name too, its `;` included, as a
server that splits at `&` alone reads it, and the value after that `=`
masked when either name reads as a credential's (`token;id=SECRET` is shown
as `token;id=<redacted>`, `key;=SECRET` as `key;=<redacted>`); and after a
masked value, up to the next `&`,
the value of every later pair MUST be masked and a piece without `=` masked
whole, since that is the rest of the value to a server that splits at `&`
alone (`token=SE;CRET` is shown as `token=<redacted>;<redacted>`). A pair's
name MUST be compared with its escapes decoded, a malformed one left as it
is (`%74oken` is `token`), and, where that differs from it, as it was
written too; it MUST be shown as written. One rule MUST say whether a name — a header's,
a query parameter's, a field's or an XML element's in logged or shown text —
reads as a credential's. The name MUST be read as words, split at
punctuation and at a change to upper case. It reads as a credential's when,
in any case, `auth`, `cookie`, `token`, `secret`, `password`, `passwd`,
`passphrase`, `passcode`, `key`, `session`, `signature`, `credential` or
`jwt` is in it, within a word or across a change to upper case but not
across punctuation, once each word that is an ordinary word only holding
one of them (`author`, `authors`, `authority`, `monkey`, `keyboard`,
`keyword`, `tokenizer` and the like, whole) is taken out — so `apikey`,
`accesstoken`, `authToken`, `author_token` and `Authorization` read as a
credential's and `author`, `cert-authority`, `keyboard-interactive` and
`tokenizer` MUST NOT; or when `sig`, `pwd`, `pw`, `pass`, `sid`, `hmac` or
`otp` is a whole word of it (`sig`, `db_pwd`, `userPass`, `X-Hmac`, but not
`design`, `signal`, `bypass` or `side`). A query parameter's name MUST also
read as a credential's when `code` (OAuth's authorization code) or `ticket`
(a CAS service ticket) is a whole word of it (`code`, `auth_code`,
`country_code`, but not `barcode` or `codec`); a header's, a field's or an
element's name with them MUST NOT, so an error body's `<code>401</code>` is
shown, but in text (`fetch.RedactText`) a name right after a `?`, a `&` or a
`;` and right before a `=`, a parameter of a URL's query or path, MUST be
read as a query parameter's, so a URL in a logged body has its `code` and
`ticket` masked as it would anywhere else. Wherever a URL is shown — a log line, a label, `$url`, a debug
report, an error — the value of each path parameter, what follows a `;`,
written as it is or escaped as `%3B` in either case, in a segment of its
path up to the next such `;` or `/`, whose name reads as a
query parameter's would MUST be masked as `<redacted>` where it stands
(`/app;jsessionid=<redacted>`), the path's escapes kept; other path
parameters and the segments themselves MUST be shown as they are. A URL's fragment MUST NOT
be shown in any of these forms. An error that quotes a URL, as Go's HTTP client
quotes the one a failed request was sending or `url.Parse` the text it could
not parse, MUST quote it with its userinfo withheld and every query value
masked, wherever the error is shown: a log line, a probe's answer or a debug
report. The error MUST still unwrap to what it was.

At `edit` and `full` the exporter MUST serve `/debug`, a page for running a
collector, and take its runs at `POST /debug/run`. Both MUST be
authenticated like a probe (§ 42.5). The page MUST be one document whose
style and script are inline, loading nothing from anywhere, with the
headers, escaping and layout of the other pages (§ 23), and MUST post its
runs relative to itself, so it works under a proxy's path prefix. It MUST
run a collector when the visitor presses *Run*, and at no other time — not
after an edit, and with no setting that would run one as the visitor types
— a run cancelling the one before it, and MUST start from the collector,
and at `full` the target or the static target, its URL names (`collector`,
`target`, `static_target`), as the collectors page links it. It MUST NOT run
anything when it opens, whatever its URL names: a link on any page can set
the URL. It MUST offer the configured collectors at every level, beside the
playground, the collector written on the page, which the page MUST call
*Playground*, and the fetch and the static target's scrape at `full` only.

A run MUST be a JSON object of the collector — `collector`, the YAML of one
collector as it is written under `collectors:`, or `configured`, the name of
one of the configuration in force, not both — and one source: a pasted
response — `body`, with `content_type`, `status` (200 when left out, else
100 to 599) and `headers` — or `target`, with `request_from`, a collector of
the configuration in force, and `params`, probe parameters, as `/probe` takes
them, without `target`, `collector` or `debug`; or `static_target`, the name
of a static target, without `params`. `headers` and `params` MUST be objects
of names, each with a string or a list of strings, a name given more than
once: a pasted response MUST have a header once for each of its values, in
their order, at most 100 values in all, a `header_<name>` parameter MUST
forward each, and any other parameter given more than once MUST be refused
as `/probe` refuses it; a value of another kind MUST be answered `400`. A run that is not that object, names
two collectors or none, or sends two sources or none, MUST be answered
`400`, and so MUST a configured collector or a `request_from` the
configuration does not have; a request over 2 MiB, a collector over 256 KiB
or a body over 1 MiB, `413`; a method other than `POST`, `405`; a static
target no target in force has, `404`. Every refusal MUST be a JSON object
with `error`.

A run MUST carry the `X-Debug-Run` header and `Content-Type:
application/json`, which a page of another site cannot send without a CORS
preflight the exporter never allows: without the header, or with a
`Sec-Fetch-Site` other than `same-origin` or `none`, it MUST be answered
`403`, and with another content type `415`, before the collector is read.

A collector written on the page MUST be read as the loader reads an entry of
the collectors list — the same decoding, the same checks of each value's
kind against the schema, the same validation — and one the loader would
refuse MUST be answered `422` with the loader's message, its line numbers
counted from the collector's first line. `${NAME}` references MUST NOT be
expanded, whatever `--config.expand-env` says, and reading it MUST NOT size
the expression caches, which are the configuration in force's. Before it is
checked, at `full` as at `edit`, unless `--web.debug.dangerous` is on, the
page MUST refuse with `422`, saying why: a collector that runs Python (`transform.type: python` or
`transform.pre_script`), which MUST NOT be run; and one whose request block
sets a key ending in `_file` or `_files` (but `max_files`, a count), at its
top or in a mapping it holds, of any request type — `request.bearer_token_file`,
a `request.tls` file, `request.known_hosts_file` — or `request.login`
(§ 42.6a), naming every such key in the order the collector has them.
Nothing a file holds
MUST reach the answer. Without `--web.debug.dangerous`, its own request
block MUST NOT make a request on any path of a run — a pasted response, a target fetched through `request_from`
or a static target's scrape — whatever its type: it MUST be accepted of
every request type the build has, its type saying only how its answer is
read (its default decoder, a `sql` collector's rows, the JSON document of
the types that make one), and the keys naming files that a request of its
type needs (an `sshfile` or `sshexec` collector's `known_hosts_file` and key
or password, a `kubernetes` collector's credential beside `api_server`, the
password files of an `ldap` `bind_dn` and a `redis` `username`, a `grpc`
collector's `protoset_file` or `proto_files`) MUST NOT be required of it.

`--web.debug.dangerous`, a boolean flag off by default, MUST be a
command-line error (exit 2, before `--dry-run` or startup) unless
`--web.debug` is `full`, and the exporter MUST log a line at `warn` when it
starts with it, saying that the playground runs Python and reads the
server's files with the exporter's permissions. With it, a collector
written on the page MUST NOT be refused for Python, for a key naming a file
or for `request.login`. Its Python MUST run as a configured collector's
runs, in the same worker pool and held to the same limits, and MUST count
in the pool's worker series alone, under no collector's name, a configured
collector of the same name included, leaving no worker of it idle. A run
of it on a target without `request_from` MUST make its own request block's
request, as a probe of a configured collector makes one, through the
target policy and with the credentials it writes, and it MUST then be
checked as a load checks a request, the files a request needs required;
with `request_from`, the target MUST still be fetched through that
collector, and a pasted response MUST still make no request. `${NAME}`
references MUST still NOT be expanded. The debug page MUST then say, in a
warning set apart from its notes, that the playground runs Python and reads
the server's files with the exporter's permissions and makes its own
request, in place of the note that it runs no Python; every page's bar
MUST carry a *Dangerous debug mode* badge, and every answer of a run of
`/debug/run` MUST carry `dangerous: true`. Without the flag, or below
`full`, none of these MUST show.

A configured collector MUST be run as it is configured, its Python
included, at `edit` as at `full`: its scripts are the operator's, and read
what a target sends on every probe, as they read a pasted body here. Its
scripts MUST run in its own workers and count in its Python worker series,
as a debug probe's do.

The page's editor MUST show the collector that runs: the playground while no
configured collector is chosen, starting from the example,
and the configured one chosen, from the list or from the `collector` its URL
names, in its place, read-only, with the label saying which runs. A
configured collector MUST be shown as YAML that its file writes: the
collector's mapping as written, its keys in the file's order, its `${NAME}`
references unexpanded whatever `--config.expand-env` says, the keys aliases
and merge keys give it written out as the decoder reads them, without the
file's comments, a folded block scalar (`>`, `>-`, `>+`) with its header
and over the lines the file writes it on, which MUST read as the value
shown — one that would not, or whose header gives an indentation, MAY be
written on one line — and with each key it takes from its file's `defaults`
added where it would write it, marked `# from the file's defaults`. A
collector the exporter holds without a file MUST be shown as it holds it,
its empty keys left out. At `edit` as at `full` the YAML MUST NOT show a
credential: a scalar value whose key reads as a credential's, by the rule above —
but where a login's answer holds its token and how it authenticates, and a
redis key — MUST be shown as `<redacted>`, and so MUST every value of a key
ending in `_file` or `_files`; a URL's userinfo and the values of its query
parameters whose names read as credentials MUST be withheld; every other
string of the request block — a header's or a query parameter's value, an
`http` body, a `grpc` message, a `tcp` send, a `redis` command's argument,
a `sql` statement or argument — in which text reads as a credential, by the
rule that masks text the exporter did not write, or that writes a text
protocol's login command, in any case (`AUTH`, `PASS` or `APOP` at the
start of a line or after a `;`, IMAP's tagged `LOGIN` with a user and a
password, Redis's `HELLO` with `AUTH`, and `AUTH` framed as a RESP bulk
string followed by another), MUST be shown as `<redacted>` whole, in the copy too, a folded
block scalar among them then on one line; a Python script MUST be shown
with what reads as a credential in it masked, as text the exporter did not
write is. The page MUST offer *Edit a copy*, which makes the playground a
copy of the YAML shown, which then runs, and MUST remember, through edits
while it is open, the configured collector the playground was copied from.
The copy MUST leave out what the playground refuses of its request — every
key ending in `_file` or `_files` but `max_files`, at any depth of the
request's mappings, and `login` — each replaced by a YAML comment, where its
line was, that names it and says why it was dropped, and a mapping left
with nothing MUST be left out with it, its comment naming each of its keys,
so that a copy of a collector without Python loads as a playground; a
mapping written in flow style (`tls: {ca_file: …, server_name: …}`) that
loses a key, and each mapping that holds it, MUST be written in block
style, so that every comment has a line of its own; the
page MUST say above the editor which keys the copy left out, and, for a
collector that runs Python, that a run of the copy is refused until the
script is taken out. With `--web.debug.dangerous` the copy MUST keep a
`login`, and MUST still leave out the keys ending in `_file` or `_files`,
whose paths the page withholds, each comment saying to write the path in its
place. `/debug/collector` MUST answer the copy as `copy` and the note as
`copy_note`, and the page opened on a configured collector MUST hold them
for *Edit a copy*. *Edit a copy* MUST be disabled while the collector's YAML
is being read and after a read of it failed, when the editor holds what the
page says of it, and MUST copy nothing but the copy `/debug/collector`
answered.

The editor MUST number its lines in a gutter beside it, for a configured
collector's YAML and for the playground alike, each number level with its
line however the editor is scrolled, down or sideways; the editor MUST NOT
wrap a line. When a run of the playground the editor still holds is refused
with an error that names lines (`line <n>:`, counted from the collector's
first line), the page MUST mark each of those lines in the gutter and with
a band behind it, MUST make each `line <n>` of the error a control that
selects that line in the editor, and MUST clear the marks at the next edit,
at a run that is not refused and at a change of collector. In the
playground Tab MUST put in two spaces, or indent the lines selected by two,
and Shift+Tab MUST take up to two spaces from the start of each line the
selection is on; Esc, then Tab, MUST leave the editor, and a read-only
editor MUST take Tab as any field does. The editor MUST colour the YAML, as
it is typed, by a reading of its lines that never fails whatever they hold:
keys, strings, numbers, booleans and null, comments, a list's dashes,
punctuation, a block scalar's indicator and its lines (those indented past
its key, or its dash), anchors and aliases, tags, `{{param_<name>}}`
placeholders and `${NAME}` references each distinctly, and a withheld value
(`<redacted>`) muted; each colour MUST read at 4.5:1 or more on the
editor's backgrounds in either theme. The playground MUST stay a textarea,
its text transparent over the coloured layer, with its caret and selection
drawn, the layer laid out and scrolled as the textarea's text is, down and
sideways, to the device pixel, so that undo, Tab, input methods and paste
are the browser's own; the layer MUST follow an edit before the next frame
is drawn and MUST be made with text alone, never HTML. A line that opens a
block — a key with more indented lines after it, a list item with more of
it below, a block scalar — MUST have a fold button in the gutter, which
says whether it is expanded (`aria-expanded`) and which lines it folds, a
single one of them reached by Tab and the arrow keys, Home and End going to
the others; a folded block's first line MUST end in how many lines it
hides, said outside its text and unfolding the block when clicked, and the
gutter MUST number every line shown with its own number, a line an error
names that is folded away marked on the line of its fold. *Expand all* and *Collapse all* MUST unfold every block or fold them
all. While a block of the playground is folded the playground MUST be shown
read-only and MUST say so, and a click on its text, Enter or *Expand all*
MUST unfold it, the caret where it was clicked; *Run*, *Copy as*, *Edit a
copy*, the request type the page follows and the lines an error names MUST
take the whole text, whatever is folded. Ctrl+Enter and Cmd+Enter MUST run
from anywhere on the page, the editors and fields included, as *Run* does,
which MUST say so in its title, its `aria-keyshortcuts` and a hint beside
it, centred level with *Run* and the timeout's box; Enter with either in a parameter's value MUST NOT add a row.
The page MUST open with the example playground, an `http` collector,
whatever was written or copied on it before: it MUST NOT keep the
playground, the body or the collector copied from in the browser. While
the playground is a copy, the page MUST offer *Back to the example*, which
puts the example back. The YAML
shown, with the lines that hold what was withheld left out and written on
the page, MUST be read as a collector and give the series the configured
collector gives. `GET /debug/collector?name=<name>`, served and
authenticated as `/debug` is, MUST answer the YAML as JSON with `name`,
`yaml` and `from_defaults`, the keys taken from defaults, and, at `full`
only, `static_targets`, the static targets in force scraped through that
collector, each with its `name` and its `target` shown as the status page
shows it (§ 42.14a), with any credential in it withheld, an empty list when
there is none; and `form`, what the page asks a run of the collector for
(below); a name the configuration in force does not have `404` with
`error`.

What the page asks for MUST follow the request type of the collector it is
for, as the type says it in its own registration, so a type a build leaves
out takes its words with it and every type MUST say them (a type that does
not MUST fail to build): the collector run reads what is pasted, and a
target is probed through the collector a target would be fetched through.
The page MUST hold the form of the playground, of a playground of each
request type the build has and of each configured collector, and MUST take
the one `/debug/collector` answers with a collector when it reads it again.
The playground's MUST follow the `request.type` its editor holds, read as
it is typed from the top mapping's `request`, in block or flow style,
quoted or not, comments left out; with none, or a type the page holds no
form of, the page MUST ask as for `http`. A field the type hides MUST keep
what was typed in it, and MUST NOT be sent:

- the target MUST be asked for by its type's name for it and an example of
  its forms — `URL` and `https://host:port` for `http`, `Name to resolve` and
  `example.com` for `dns`, `Database server` and `host or host:<the
  driver's port>` for `sql`, `SSH host` and `host or host:22` for `sshfile`
  and `sshexec`, `host or host:123` for `ntp`, `host or host:443` (or the
  `starttls` protocol's port) for `tlscert` — said to be optional where the
  type may leave it out (`localfile`). A collector of a type that takes no
  target (`kubernetes`) MUST be offered no target field, the page saying
  `This collector names its own target.`, and its run MUST send an empty
  target, which is not refused for want of one;
- the probe parameters MUST be asked for as rows of a name and a value (below):
  a row for each of the collector's own `{{param_<name>}}` placeholders, its
  name filled in, its value empty with its default, if it has one, as the
  value's placeholder, and the other parameters its type takes from a probe,
  `header_<name>` for each header it forwards, `timeout` left out, offered as
  names to choose; `/debug/collector`'s `form` MUST give them as `params`,
  each a `name` and its `default`, and `other_params`. A collector that takes
  none MUST be offered no rows, the page saying `This collector takes no
  probe parameters.`;
- a pasted response's body MUST be asked for as what an answer of the type
  is — what the target answers for `http`, the rows as JSON for `sql`, the
  JSON document the exporter makes for the types that make one, the file's
  content for the types that read files — with a status and headers only
  for a type whose answers are HTTP responses (`http`, `graphite`), and a
  `Content-Type` only where it can choose the decoder: not for a type that
  makes its answer, reads rows or has a decoder of its own. The page MUST
  send no status, headers or `Content-Type` it does not ask for;
- the page MUST NOT say that a target of another type than `http` is
  fetched: it is probed as `/probe?debug=true` probes it, through the
  collector named (`Probed through <name>, …`).

A pasted response's other headers and the probe parameters MUST be asked for
as rows, each a name and a value input with a button removing the row, and a
button adding a row (*+ Add header*, *+ Add parameter*), every input and
button labelled for assistive technology with its row's number (`Header name
1`, `Remove parameter 2`). The headers MUST start with one empty row. A row
whose name and value are both blank MUST NOT be sent; one with a value and no
name, a header name that is not an HTTP token, worded as the server words
it, and a probe parameter `/probe` would refuse — `target`, `collector` or
`debug`, or a name given again but a `header_<name>` — MUST be marked on its
row with why and focused, and the run MUST NOT be sent; the server MUST
still check all it is sent. A header name given again, in any case, MUST be
sent as one name with each value in the order of the rows. Adding a row
MUST focus its name, and removing one the row that takes its place, the one
before, or the adding button. A change of collector MUST keep the header
rows, and make the parameter rows again for the collector a target is
probed through, keeping the values typed for names it takes too. The rows
MUST be made with `value` and `textContent` alone, never as HTML, and MUST
keep a row's inputs and its button one height, on one line while the name
and value fit side by side, and the page without sideways scrolling at a
phone's width.

A pasted response MUST NOT make any request. It MUST go through the trip a
probe makes: the collector's `accept_status`, its response limit, which a
body over fails as a fetched one does, the decode, the transform and the
validation under the collector's limits. A response pasted for a `sql`
collector MUST be read as its rows, as its driver's are handed on (§ 5.1,
`sql`): a JSON list of objects, each a row's columns by name, a whole
number an integer, `true` and `false` 1 and 0, and `null` leaving the
column out of its row; anything else, and a value that is a list or an
object, MUST be refused with `400` saying what to paste. A fetch, at `full` only, MUST be
made exactly as a debug probe of the collector that fetches would make it:
the configured collector run, or the one `request_from` names, which a
written collector MUST name (`400` otherwise) — through its request block,
target policy (§ 26.1), credentials, `accept_status`, forwarded
`header_<name>` parameters and trip limits (§ 42.13b), and with nothing of
the browser's request — its `Authorization`, cookies or other headers —
forwarded; a target missing or a parameter wrong MUST be answered as
`/probe` answers them. The page MUST fetch through the configured
collector chosen, or, with the playground, through the configured collector
it was copied from, sent as `request_from`, saying which, and MUST NOT offer
another collector to fetch through; while the playground was copied from
none, the page MUST offer *Probe a target* disabled, saying to choose a
configured collector or *Edit a copy* of one. The pasted body MUST start as
the example's answer with the example playground and empty with a
configured collector or a copy of one, and a change of collector MUST NOT
replace a body the visitor wrote. A static target's scrape, at `full` only, MUST be
made as `/static-targets?debug=<name>` makes it, through the target's own
collector. The page MUST offer it as a list of the static targets in force
scraped through the collector a target would be fetched through — the
configured collector chosen, or the one the playground was copied from —
each by its name and its target as `/debug/collector` shows it, never as a
name to type; it MUST open with those of the collector its address chooses
and read them from `/debug/collector` again whenever that collector
changes, so that they follow a reload. With none, the page MUST offer
*Scrape a static target* disabled, saying that no static target uses the
collector, that none is configured, or, for a playground copied from none,
to choose a configured collector or *Edit a copy* of one. `?static_target=<name>`
MUST open the page with that target chosen and the collector it is scraped
through chosen on the left, whatever collector the address names besides; a
name no static target in force has MUST be ignored. A scrape MUST be
answered as what the
scrape would have published, its series with the target's labels and
`static_target`; nothing MUST be published. A collector run on what
another collector fetched — a written one, or one other than the static
target's — MUST be given what a collector of its own request type fetched
only (`400` otherwise, before anything is fetched), and decodes, transforms
and validates it as its type reads an answer. The page MUST say whose
static targets it lists: `Static targets of <name>, the collector chosen.`
or `…, the collector this playground was copied from.`

*Probe a target* MUST offer *Copy as*, made from the form as it stands and
made again as the target and the parameter rows change: the `/probe` URL,
a `curl` command for it, a Prometheus `scrape_config` and the debug probe's
URL. The URL MUST be absolute, resolved against the page's own address as
the page's requests are, so that a path prefix in front of the exporter is
kept, and MUST carry `collector`, the collector a target is probed through
— the configured one chosen, or the one the playground was copied from, the
page then saying that `/probe` runs the configured collectors only and so
the playground's edits are not in it — `target`, left out for a collector
that names its own, and each filled parameter row, a repeated
`header_<name>` once for each row, URL-encoded; only the debug probe's URL
MUST carry `debug=true`. The command MUST be `curl -s` and the URL in single
quotes, a single quote in it written `'\''`. The `scrape_config` MUST be the
multi-target job of `docs/LOCALFILE.md`: `job_name` the collector's name,
`metrics_path` the URL's path as it reads, its percent-escapes decoded,
since Prometheus escapes the path itself (a prefix `/my%20pfx` MUST be
written `/my pfx`; a path whose escapes decode to no text MUST be written as
it stands), `scheme: https` when the page is served over
HTTPS, `params` of `collector` and each parameter with its values in the
order of the rows, and, for a collector that takes a target, the target in
`static_configs`, moved by `relabel_configs` into `__param_target` and
`instance`, `__address__` set to the host and port the page was reached at;
for one that names its own, that host and port as the target. Every value
MUST be a single-quoted YAML scalar, a single quote in it written `''`, and
a key that YAML would read as other than its text quoted as well. With
`web.basic_auth` on, the command MUST add `-u USER`, the job MUST carry a
commented `basic_auth` to fill in, and the page MUST say so; the page MUST
NOT be given or copy the credential. The buttons MUST be disabled, saying
why, while the target is empty for a collector that takes one or a
parameter row is wrong. *Scrape a static target* MUST offer *Copy as* the
`/static-targets?targets=<name>` URL of the target chosen and its `curl`
command. A button MUST show what it copies in a box that can be selected,
copy it with the browser's clipboard where the browser allows it, and say
whether it did; the text MUST be put in with `textContent` alone.

A run MUST be bounded by the probe's deadline (§ 3.2a), and by
`--probe.default-timeout`'s default when that is `0`, or a static target's
by its interval; at most 4 runs of the page MUST be in progress at once, a
run past them answered `503`. A run MUST be answered `200` with a JSON
object of `ok`, true when every stage succeeded and none carried on;
`verdict`, what a probe or the scrape would have answered, as the text
report says it; `series`, the exposition a probe would have served, to the
byte; `samples`, the same series one sample at a time, in the order
`series` writes them, made from the series and not by reading the text
again, each an object of `name`, the sample's name with a histogram's or a
summary's suffix, `metric`, the metric's name, `type`, the metric's type,
`labels`, a bucket's `le` and a quantile's `quantile` among them, and
`value`, a string as the exposition writes it (`NaN`, `+Inf`, a count as a
whole number); `requests` and `response`, what was sent and read, redacted and
bounded as the text report redacts and bounds them; `stages` and `notes`,
the stages and the warnings the load would give with every line the run
logged; `lint`, the findings of the metric lint (§ 30.1a), each an object of
`collector`, `rule`, `metric` and `message`, which the page MUST show; and
`report`, the text report of a debug probe, whose first line names it a
debug run, the collector, whether it was written on the page, and what it
read.

The page MUST show a run's series both as a table, by default, and as
`series` unchanged, switched between by *Table* and *Text*, MAY keep the
choice in the browser, and MUST show a refused run's error as text. The
table MUST have a row for each of `samples` and the columns `name`, one for
each label name any sample has, in alphabetical order, `value` and `type`,
and MUST put every text a run answers in it as text, never as HTML.
Clicking a column's header MUST sort the rows by it, ascending and then
descending; `value`, `le` and `quantile` MUST sort as numbers, `+Inf` and
`-Inf` included, and a sample without the label MUST sort last either way.
A filter MUST keep the rows whose name, a `label=value` or value holds its
text, case-insensitively, and a list of the metrics MUST keep one metric's;
the page MUST say how many rows are left of how many, `<n> of <m> series`.
It MUST draw at most 2000 rows, saying when there are more that it shows
the first 2000 and that a filter narrows them, and the text MUST stay whole.
A lint finding MUST be shown on the rows of the metric it names, by
`metric`, a series recording no rule, as its rule opening to its advice by
click or keyboard, not by hover alone; one about a metric no row is of MUST
be listed above the table. On a narrow screen the table MUST scroll in its
own box and the page not.

With `otlp.enabled` and an endpoint, and only then, every answer of a run
that ran MUST carry `otlp`, what the next OTLP export would send of the
run's series, and the page MUST offer an *OTLP* tab beside *Table* and
*Text*, at `edit` as at `full`, for every kind of run — a pasted response,
a target fetched, a static target's scrape, of a configured collector or
of the playground. It MUST be made by the export's own code, not by a copy
of it: the series a probe would answer, with `otlp.probe_attributes`'
attributes, under the exporter-wide resource, the playground's as a probe
of it would be; a static target's scrape's under the target's resource,
its `otlp.labels_as_resource` moved to it and `otlp.instance_id` set,
whether or not it sets `export_via_otlp`, which the tab MUST then say;
either resource with `otlp.exporter_instance`'s attribute and value, with
a note saying what the attribute is;
each series once and in the order a drained queue has them, the later of
two alike kept; named under `otlp.metric_names`; and checked against one
another as an export checks a resource's points, and against no other
point: not the other probes' or static targets', and not the exporter's
own series. A cumulative point's start time MUST be its own time, as at a
series' first export, which the tab MUST say. Making it MUST NOT queue
anything, ask for or remember a series' start time, count in the
export's self-metrics or status, or send anything. `otlp` MUST be an object
of `metric_names`; `resource`, the resource's attributes, each a `key` and
a `value`, `service.name` first, and `resource_notes`, where they come
from; `scope`, its `name` and `version`; `points`, each with `prometheus`,
the name the exposition writes it under, `name`, `kind` (`gauge`, `sum
(cumulative, monotonic)`, `histogram (cumulative)` or `summary`), `unit`,
`attributes`, `value` (a histogram's count, sum and each bucket's own
count, a summary's count, sum and quantiles) and `notes`, one for each
change the export made of its series — a family exported as gauges and
why, `_total` taken off, a unit from a suffix, an attribute added by
`otlp.probe_attributes`, a label moved to the resource, a series' own
timestamp, a sum or count OTLP cannot leave out; `left_out`, each point
the export would leave out, with its `reason` in the words the export
logs it; `clashes`, the lines the export would log of them; `notes`; and
`body`, the `ExportMetricsServiceRequest` the export would send of them,
its points at the run's time in whole milliseconds as the export's queue
keeps a point's,
indented, `{"resourceMetrics": []}` for none. The tab MUST show the
resource, the scope, the points as a table that sorts by a column's
header and keeps the rows that hold a filter's text, a badge on a row with
notes that opens to them by click or keyboard, the points left out, and
the body folded away under the heading *Request body*, the part it is in
saying whose it is, with a *Copy* button, every text put in as text,
never as HTML. The page MUST show a body, the metrics' and the logs', as a
tree when it is JSON — a tree for each of several requests, one blank line
between them — and as text, saying it is not JSON, when it is not: each
object and array a node that folds behind a button reached by click or
keyboard that says whether it is open (`aria-expanded`), showing when
folded how many keys or items it has and its `name` or `key` where it has
one of them as a string; keys, strings, numbers, booleans, `null` and
punctuation each in a colour of the theme, readable in light and in dark;
a number as the body writes it; nodes open down to four levels below the
top and folded below; a folded node's comma, where another follows it,
after what it says of the node; a folded object of a `key` string and a
`value` object of one `*Value` field, as OTLP writes an attribute, saying
`<key> = <value>`; *Expand all* and *Collapse all* beside *Copy*, the
latter folding every node but the top. A node's children MUST be made
when it is first opened, and *Expand all* MUST make them in slices that
leave the page responsive. A node of any number of children MUST open
with all of them: they MUST be made in chunks, a chunk a frame, and MUST
NOT be passed as the arguments of one call, which a browser limits to
some 100000; a node MUST count as made only once its last child is. A
node whose children cannot be made MUST say so in their place, and a body
that cannot be shown MUST say so in place of the tree, never leaving the
last body's tree shown. *Copy* MUST copy the body's indented text, as
the answer has it. A long string MUST wrap within the panel, which keeps
its height and scrolls. With `otlp.logs` on too, `otlp.logs` MUST be the run's own
log lines as the records the log export makes of them, by the log
export's own conversion — `time`, `severity_number`, `severity_text`,
`body`, `attributes`, flattened and each key once, and `exported`, false
with a `note` for a line below `otlp.logs.level` — with its `level`,
`notes` and `body`, the `ExportLogsServiceRequest` of those exported, and
the tab MUST show them as *Log records*. None of them MUST be queued for
the log export. Without OTLP, a run MUST NOT carry `otlp` and the page
MUST NOT offer the tab.

With `otlp.enabled`, the page MUST have a panel of its own, *OTLP
endpoints*, below *Output* and outside the tab, shown whether or not
anything has run, which MUST offer *Test the metrics endpoint*, with
`otlp.logs` on *Test the logs endpoint*, and with `otlp.traces` on *Test
the traces endpoint*, which `POST` to
`/debug/otlp-test` a JSON object with `signal`, `metrics` (also when left
out), `logs` or `traces`, with the `X-Debug-Run` header, refused as a run is
without it, from another site, with another content type or method, and
behind Basic Auth. Below `full` the buttons MUST be disabled with a note,
and the test MUST be answered `403`; with the export off, `logs` with
the log export off, or `traces` with the trace export off, `409`; another `signal` `400`; and while a test is in
progress, the server's whole, `429`, sending nothing. A test MUST send
one request, an empty `{"resourceMetrics":[]}`, `{"resourceLogs":[]}` or
`{"resourceSpans":[]}`,
to the endpoint the configuration in force names and no other, with the
export's headers, TLS, compression and timeout, and MUST NOT retry it,
whatever it answers. It MUST answer `200` with `ok`, `signal`,
`endpoint`, its credentials withheld, `sent`, the headers sent, every
`otlp.headers` value `<redacted>` whatever its name and the `Content-Type`
and `Content-Encoding` the export sets as sent, `body`, `status`, `took`, `failure`, the
connection's, TLS's or the timeout's error or the refusal in full,
`outcome`, what an export would make of the answer, `partial_success`,
`headers` and `response`, the start of the answer's body, and `notes`. It
MUST NOT count in the export's self-metrics, change what the status page
shows of the exports or readiness, or be taken by the log export.

The collector MUST NOT be put in force. A run MUST NOT be answered from or
fill the response cache, reading it only as a debug probe does, share a trip, reach the failure log or the exporter's log
but for one `info` line per run, which the log export MUST NOT take, be queued for OTLP, or be counted in any
collector's self-metrics, but a configured collector's Python worker series,
or in `http_exporter_probes_seconds_total`. A run that panics MUST be
reported as a debug probe that panics is, and logged with its stack, and a fetch MUST give back its trip slot however it ends.

Every run MUST be counted in
`http_exporter_debug_runs_total{kind, result}`, from 0: `kind` is `probe`,
`static` or `page`; `result` is `success` when the trip went through whole,
`failure` when it ran, or was taken and could not run for a trip slot or
the page's bound, and did not, and `invalid` when it was refused before it
ran for what it asked: a parameter, a collector, a static target's name, a
run's form or headers, or a source the level does not allow. A debug probe
or scrape the level does not serve, or whose `debug` value is not a
boolean, is no run, and MUST NOT be counted. The family MUST be absent at
`off`, and present with the kinds the level serves: `page` alone at `edit`,
every kind at `full`.
