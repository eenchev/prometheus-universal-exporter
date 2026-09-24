# Generic HTTP Prometheus Exporter — Exporter Specification

This is the specification for the exporter itself: its endpoints, configuration,
decoders, transforms, limits, logging, CLI, container image and tests. The Helm
chart that deploys it has its own specification in
[SPECIFICATION-CHART.md](SPECIFICATION-CHART.md).

Section numbers are the ones this specification has always used, and they did not
change when it was split. Each document therefore keeps its own sections' numbers
and skips the other's, so a reference to § 33.13 or § 42.8 still points at the
same requirement it always did. A section that carried requirements for both is
present in both documents under the same number, each holding only its own half
and linking the other.

Sections about the repository as a whole — purpose, implementation order,
acceptance criteria, CI gates, documentation requirements — stay here, and name
the chart document where a chart requirement belongs to it.

## 1. Purpose

Build a production-grade Prometheus exporter written in Go that converts arbitrary HTTP endpoint responses into Prometheus metrics.

The exporter is intended for systems that expose useful operational/status data over HTTP but do not provide suitable Prometheus metrics.

The exporter MUST:

- Discover targets through Prometheus Operator `ServiceMonitor` / `PodMonitor` rather than hardcoding target URLs in the exporter configuration.
- Select a configured `collector` through a Prometheus scrape parameter, e.g. `/probe?target=<TARGET>&collector=<COLLECTOR>`.
- Fetch HTTP/HTTPS endpoints itself, including authentication, custom headers, TLS configuration, timeouts, and HTTP methods.
- Support multiple response decoders as first-class peers.
- Support JSON, YAML, XML, CSV, HTML, Prometheus exposition format, plain text, and Python.
- Support jq transformations for JSON.
- Support yq transformations for YAML.
- Support XPath for XML/HTML.
- Support CSS selectors for HTML.
- Support regex-based extraction for plain text.
- Support full Python as a first-class decoder/transformation mechanism, not merely as a fallback.
- Normalize all decoder output into a common internal metric representation before Prometheus exposition.
- Provide explicit handling for HTTP failures, decoding failures, transformation failures, and missing fields/keys.
- Protect against excessive response size, execution time, metric count, label count, and other cardinality/resource abuse.
- Expose exporter self-metrics for scrape, decode, transform, and configuration health.

The design MUST remain extensible so additional decoders or transformation engines can be added without changing the HTTP fetcher or Prometheus output layer.

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
  HTTP Fetcher
        |
        v
 HTTP Response
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
- `retry_attempts`: optional non-negative integer overriding the configured
  number of retries after the initial target request.
- `retry_backoff`: optional non-negative Go duration overriding the fixed delay
  between retry attempts.
- `follow_redirects`: optional boolean overriding whether the target request
  follows HTTP redirect statuses.
- `enable_http2`: optional boolean overriding whether the target request may
  negotiate HTTP/2.

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

#### 3.2a Probe deadline

When a probe carries `X-Prometheus-Scrape-Timeout-Seconds`, as Prometheus sends
on every scrape, the exporter MUST bound the trip to the target — the request or
file read, decoding, transforms and scripts — by that timeout less
`--probe.timeout-offset`, which MUST default to 500ms and MUST reject a
negative value as a command-line error (exit 2, before `--dry-run` or startup).
When the offset would leave less than half the timeout, the probe MUST keep
half. When the budget runs out the probe MUST fail in the stage that was
running, and the error MUST say that the probe's budget, with its length, ran
out, so Prometheus receives the reason before its own timeout. A missing,
unparseable, non-positive or non-finite header MUST leave the probe
unbounded by it. The `timeout` parameter keeps bounding the request alone; the
earlier deadline wins. A cache hit needs no budget, and identical probes that
share one trip (§ 42.13a) share the budget of the probe that started it.
Scheduled targets are not affected.

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
retry transport failures and transient HTTP statuses `408`, `425`, `429`, and
`500` through `599`. Other HTTP statuses MUST be returned without retrying.
The retry loop MUST share the incoming scrape or explicit target timeout.

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
`legacy.example`. IPv6 literals in brackets and credentials in the userinfo
MUST survive. `allowed_schemes` MUST apply to the result, so a bare target is
never upgraded to `https`.

When neither `request.path` nor a `path` probe parameter is given, the target
MUST be requested exactly as given, including any path it carries.

Rendering a target for a log line or an error body MUST never fail, whatever
the probe sent. A target that cannot be parsed MUST be withheld rather than
echoed, since the credentials in it could not be located to redact them.

The exporter MUST expose HTTP response status and request duration in its self-metrics.

---

## 5. Collector configuration

Recommended top-level structure:

```yaml
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
- A collector file MUST be a mapping whose only key is `collectors`, holding a
  non-empty list. Any other key — the exporter-wide `web` and `otlp` settings,
  a nested `collector_files`, a misspelling — MUST be an error naming the file,
  the key and its line, so a file of collectors can never change exporter-wide
  behaviour. An empty file and an empty list MUST be errors. Unknown keys inside
  a collector MUST be rejected exactly as in the configuration.
- The collectors MUST be merged in a defined order: the configuration's own,
  then each entry's files in the order listed, a pattern's matches in file name
  order. The merged list MUST then be validated as one: defaults, the rule
  checks of § 24.2 and the Python checks of § 16 apply to a collector from a
  file exactly as to one written in the configuration.
- With `collector_files`, the configuration's own `collectors` MAY be omitted;
  the merged list MUST NOT be empty.
- `${NAME}` expansion (§ 42.15a) MUST apply to collector files whenever it
  applies to the configuration.

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
exceeds that limit, MUST be rejected the same way; a name only known at scrape
time MUST be checked against the limit with every other exported name (§ 21).

The prefix MUST be applied in one place, to the output of whichever transform
ran — declared metrics, names emitted by a Python script, and names passed
through or renamed by a `prometheus` transform alike — and before the limits
are checked, the result is cached, or it is written to `/probe` or queued for
OTLP, so every consumer sees the same names. A histogram or summary keeps its
family under the prefixed name. The response cache MUST be keyed on the
collector definition including the prefix. The exporter's own `http_exporter_*`
metrics, including scheduled-target health metrics, MUST NOT be prefixed. Logs
and probe errors MUST name a metric rule as configured, without the prefix.

### 5.0a Sizes

Every setting that is a number of bytes — `request.max_response_bytes`,
`limits.max_response_bytes`, `limits.max_output_bytes` and
`request.max_total_bytes` — MUST accept a YAML integer of bytes, or a string of
a number, optionally with a fraction, followed by an optional space and unit:
`B`; `kB`, `MB`, `GB`, `TB` as powers of 1000; `KiB`, `MiB`, `GiB`, `TiB` as
powers of 1024. The unit MUST be case insensitive, the `B` MAY be left out, and
a fraction MUST be rounded down to whole bytes. Anything else, including a
negative number, an exponent or an unknown unit, MUST fail to load naming the
value. The configuration schema MUST accept both forms for these settings.

### 5.1 Request types

Every collector MUST declare `request.type`, which selects how it reaches its
data. `http` and `localfile` are implemented; gRPC and FTP are anticipated. The
type MUST be required rather than defaulted, so that no configuration means
`http` by accident. A missing type
MUST be rejected at startup and on reload with a message naming the collector,
stating that the type is required, listing the supported types and showing
`type: http`; an unknown type MUST be rejected with the supported types listed.
The value MUST be matched without regard to case or surrounding whitespace and
stored in lower case.

Each type MUST own, and the implementation MUST keep in one registry:

- the request keys it accepts. A key set on a collector that its type does not
  accept MUST be rejected, naming the key and the type, so a configuration can
  never carry a setting that is silently ignored. Keys MUST be checked before
  the type fills in defaults, so a default is never mistaken for something the
  author wrote;
- the `/probe` parameters it accepts (§ 42.10). A parameter that some type
  accepts but the collector's type does not MUST be rejected with
  `400 Bad Request` naming the parameter and the type, before the target is
  contacted. A parameter no type accepts is not an override and MUST be ignored,
  as it always has been, since a monitor may carry parameters of its own;
- the keys a scheduled target's `request` block may set (§ 42.14). A key its
  collector's type does not accept MUST be rejected at startup naming the
  target, the key, the collector and the type;
- its own validation — required keys, defaults and cross-key rules;
- its own fetch. Decoding, transforms, error policies, limits, caching and
  exposition MUST be shared by every type, so a new type adds only how bytes
  are obtained;
- what a target is for it: whether a probe or scheduled target may leave
  `target` out, how a target is checked — a probe's failing check answered with
  `400` before anything is fetched, a scheduled target's at load — how a target
  is shown in logs, error bodies and the `target` label of scheduled health
  series, the `url` and `http_method` labels of its verbose self-metrics
  (§ 22.1), and the name of the stage a failed fetch is reported under.

Every key of the request block, and of a scheduled target's request block, MUST
be accepted by at least one type, and a test MUST enforce it, so a key cannot be
added without deciding which types it belongs to.

#### Selecting request types at build time

Which request types a binary carries MUST be decided at build time, so a build
can leave out types it does not need together with the code and libraries only
they use. A default build — no tags, and the published image — MUST carry every
type.

- Each type MUST live in its own `internal/fetch/requesttype_<name>.go`, which registers it
  from `init` and carries exactly the constraint
  `//go:build !select_request_types || request_type_<name>`. Code and imports
  that only that type needs MUST live behind the same constraint, and so MUST
  `requesttype_<name>_test.go`, the tests that only that type needs, so that
  vetting a single-type selection compiles its tests too.
- A build with `-tags select_request_types` MUST carry only the types named by
  `request_type_<name>` tags. One that names none MUST fail to compile, which
  `requesttype_none.go` does with the constraint
  `select_request_types && !request_type_<a> && !request_type_<b> ...`
  listing every type.
- The source MUST keep a list of every type in the tree, whether or not the
  build carries it. A collector naming a type that exists but was left out of
  the build MUST be rejected with a message saying so, listing the types the
  build carries and naming the tag that would include it; a name that is no
  type at all keeps the unknown-type message.
- `tools/request-type-tags.sh` MUST turn a comma-separated `REQUEST_TYPES` list
  into those tags, printing nothing for an empty list and failing on a name
  that is not a type in the tree. The Dockerfile's `REQUEST_TYPES` build
  argument and the Makefile's `REQUEST_TYPES` variable MUST both use it.
- The types the binary carries MUST appear on the startup log line and in the
  `--dry-run` report (§ 30.1).
- CI MUST vet and build each single-type selection as well as the default
  build, so no type depends on code only another type compiles.

#### `http`

`type` is the only required key. Everything else is optional:

| Key | Default | Rule |
| --- | --- | --- |
| `method` | `GET` | One of GET, POST, PUT, PATCH, DELETE, HEAD, case-insensitive. |
| `path` | none | Joined onto the target URL; may carry path parameters (§ 42.10a). Empty is valid, since the target URL may carry the whole path. |
| `query` | none | Query parameters added to the request. |
| `headers` | none | Headers sent to the target. |
| `body` | none | Raw request body. |
| `basic_auth` / `basic_auth_file` | none | Mutually exclusive; the file form needs both paths. |
| `bearer_token` / `bearer_token_file` | none | Mutually exclusive, and exclusive with basic authentication. |
| `forward_authorization` | `false` | Forward the probe's `Authorization` header (§ 42.4). |
| `forward_headers` | none | Allowlist for `header_<name>` probe parameters (§ 42.4). |
| `tls` | verify | CA, client certificate and `insecure_skip_verify` (§ 42.9). |
| `retry` | none | `attempts` and `backoff`, both non-negative. |
| `max_response_bytes` | limit | Response size cap. |
| `follow_redirects` | `false` | § 42.15. |
| `enable_http2` | `false` | § 42.15. |
| `allowed_schemes` | `http`, `https` | Schemes a target may use. |

It MUST accept these `/probe` parameters: `method`, `path`, `timeout`, `body`,
`insecure_skip_verify`, `follow_redirects`, `enable_http2`, `retry_attempts`,
`retry_backoff`, `header_<name>` and `param_<name>`. A scheduled target using an
`http` collector MAY set `method`, `path`, `body`, `timeout`,
`insecure_skip_verify`, `follow_redirects`, `enable_http2`, `retry`, `headers`,
and the basic and bearer credential keys.

#### `localfile`

A `localfile` collector reads a file from the exporter's own filesystem. It
exists for what writes metrics or a status to disk rather than serving it —
above all the Prometheus text files batch jobs leave for node_exporter's
textfile collector — and follows that collector's practice.

| Key | Default | Rule |
| --- | --- | --- |
| `root` | none | **Required.** An absolute directory, stored cleaned. The filesystem root MUST be refused. Its existence is not checked at load, so a volume mounted later works. |
| `path` | none | Relative to `root` and the target; MUST NOT be absolute or lead outside with `..`. May carry path parameters (§ 42.10a). |
| `max_age` | off | Non-negative duration. A file last modified longer ago MUST fail the scrape, naming its age and the limit. |
| `max_response_bytes` | limit | As for `http`; with `files`, the limit of each file. |
| `files` | none | A list of file name patterns, `path.Match` syntax. Setting it makes the collector read a directory (§ 5.1a). MUST NOT be combined with `path`; a pattern MUST NOT be empty, contain `/`, `\` or NUL, or be malformed. |
| `max_files` | 100 | With `files` only; not negative. |
| `max_total_bytes` | 64 MiB | With `files` only; not negative. A size (§ 5.0a). |

`max_files` or `max_total_bytes` without `files` MUST be rejected, naming the
key.

The file read MUST be `root` / target / `path`:

- `target` MAY be left out, by a probe and by a scheduled target. When given it
  MUST be a path relative to `root`, an absolute path inside `root`, or a
  `file://` URL of one, and a target leading outside `root` MUST be refused —
  a probe with `400` before anything is read, a scheduled target at load. A
  `file://` URL MUST carry an absolute path.
- A `path` probe parameter replaces `request.path` and is held to the same
  rules. A path parameter value MUST be a single file or directory name: `/`,
  `\`, NUL, `.` and `..` MUST be refused.
- When neither names a file the scrape MUST fail saying so.

Every read MUST be confined to `root` by the operating system (Go's
`os.Root`), so that neither `..` nor a symbolic link can reach outside it
whatever the probe asks for. A symbolic link MAY be followed only while it
stays inside `root`; an absolute link MUST be refused. Only regular files MUST
be read: a directory, device, socket or named pipe MUST be refused before it is
read, and files MUST be opened non-blocking so a named pipe cannot hang a
scrape. The read MUST stop at the collector's response limit and fail beyond
it, counted as a limit error. A file whose size or modification time changes
while it is read MUST be read again, and the scrape MUST fail, advising an
atomic rename, if it changes a second time. A read MUST NOT hold a probe past
its `timeout`, budget (§ 3.2a) or context, even when the filesystem does not
answer. Because a read cannot be cancelled, the reads of one collector still in
progress — including those whose probe has given up — MUST be capped, at four.
A probe that would exceed the cap MUST fail at once, saying the filesystem is
not answering, and the slot MUST be freed when the read returns rather than
when its probe does.

A successful read MUST be presented to the shared pipeline as a response with
status `200` and the headers `Content-Type`, chosen from the extension
(`.prom` as Prometheus text version 0.0.4, `.json`, `.yaml`/`.yml`, `.xml`,
`.csv`, `.html`/`.htm`) so `decoder.type: auto` picks the decoder,
`Content-Length`, and `Last-Modified`, the file's modification time. A failed
read MUST be reported in the `file` stage and follow `on_fetch_error`. A file
that does not exist, and one the exporter may not read, MUST each be named as
such.

It MUST accept these `/probe` parameters: `path`, `timeout` and
`param_<name>`. A scheduled target using a `localfile` collector MAY set
`path` and `timeout`. Its verbose self-metrics MUST carry the file's `file://`
URL, with path parameters as their placeholders, as `url`, and `READ` as
`http_method`, so they are never mistaken for the per-collector series, which
carry no method.

A collector with `path`, or none, reads one file per probe. Merging files MUST
happen only as § 5.1a specifies, where each file is checked on its own and
named, never as one concatenated exposition, which would hide which file a
broken series came from and fail every file's metrics when one is malformed.

##### 5.1a Reading a directory

A `localfile` collector with `files` MUST read the directory `root` / target —
`root` when the target is left out — and every entry directly in it whose name
matches one of the patterns. Subdirectories MUST NOT be read, and a name
starting with `.` MUST match only a pattern starting with `.`. A missing
directory, or a target naming something other than a directory, MUST fail the
scrape in the `file` stage under `on_fetch_error`. There is no file to name, so
the `path` and `param_<name>` probe parameters MUST be refused with `400`, and
`request.path` in a scheduled target at load, each saying why.

The directory MUST be listed in batches, and the listing MUST stop after ten
times `max_files` entries, and at least 1000, whatever they are, logging a
warning with how many were listed; only the files found by then are
considered.

Matching files MUST be taken in name order, at most `max_files` of them; the
rest MUST NOT be read or looked at, and MUST be counted and logged as a warning
with how many matched and the first skipped. Every file taken MUST be read by
the rules for one file above — confined to `root`, only regular files,
non-blocking, re-read when it changes under the read — and additionally:

- Which files are read MUST be decided from their sizes, in name order, before
  any is read, so the choice does not depend on which read finishes first.
- A file larger than the response limit MUST be refused from its size, before
  it is opened.
- A file that would take the bytes read in this scrape past `max_total_bytes`
  MUST be refused in the same way; later files that fit MUST still be read.
- What `max_total_bytes` leaves after the files chosen MUST be shared among
  them as room to grow, and a file that grows past its size plus its share
  before it is read MUST fail alone, so a scrape never reads more than
  `max_total_bytes`.
- `max_age` MUST apply to each file.

The files chosen MUST be read four at a time, and answered in name order
whichever finished first.

The whole directory read MUST be one read for the pending-read cap and MUST end
with the probe's `timeout`, budget or context. When it ends before the read is
done, the files read by then MUST be answered, and each file still being read
or not yet reached MUST fail alone with an error saying it was not read before
the deadline, its modification time reported when it had been taken; no further
file MUST be started. Only a directory not listed in time MUST fail the probe.

Each file read MUST then be decoded, transformed and validated on its own, as a
response of its own with the headers a single file gets, so its decoder follows
its extension under `decoder.type: auto`. Its series MUST be given a label
`file` with its name. A file MUST be left out, alone, when it could not be read
or was refused, when decoding, the transform or validation fails — including a
metric rule with `error_mode: fail` — when a series of it already has a `file`
label or bears one of the names below, or when one of its metrics has another
type than the same metric in a file taken before it. A left-out file MUST NOT
fail the probe: it MUST be logged as a warning with the file, the stage and the
error, and counted in the collector's self-metrics as the stage's failure
would be, and every other file's series MUST be answered. The series of a
family MUST stay one contiguous block, declared once.

The answer MUST also carry, as gauges with help text:

```text
localfile_mtime_seconds{file}   modification time, for every file whose time was read
localfile_scrape_error{file}    1 for a left-out file, 0 for every other file taken
localfile_files_skipped         the files beyond max_files
```

A file beyond `max_files` MUST NOT get a series of its own, so the `file`
label never has more than `max_files` values. `limits.max_metrics` MUST apply to the whole answer. The verbose
`url` label MUST be the directory's `file://` URL ending in `/`. Scheduled
targets MUST read directories the same way.

A collector MAY set `cache`, a mapping of `ttl`, the time to live of a cached
collector result, and `stale_if_error`, how much longer a result stands in for
a trip that fails. Omitting `cache`, or setting both to `0s`, MUST disable
caching for that collector. Section 42.13 defines the caching contract.

Every collector uses the same `transform` and `metrics` structure. The decoder
only selects how the response is parsed; it does not contain executable
collector logic. Python is a transform (`transform.type: python`) and its
script and optional library declarations live under `transform`, just like
`pre_script`. A Python transform emits its metric definitions dynamically
through `metric(...)`, so it MAY omit the `metrics` array entirely; an empty
`metrics: []` MUST NOT be required. A collector that needs Python only to parse
its response SHOULD instead reshape the response in `transform.pre_script` and
declare its metrics in the ordinary `metrics` array, as section 18 defines.

Collector names SHOULD use Prometheus-label-safe/simple names such as:

```text
elasticsearch_cluster
legacy_application
vendor_status
```

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
auto
```

`auto` is optional but strongly recommended.

Python is a transform, not a decoder. Supported transform types MUST include
`jq`, `yq`, `xpath`, `css`, `csv`, `regex`, `prometheus`, and `python`.

The decoder MUST be chosen by `decoder.type`, the one key for it; `response`
MUST NOT take a format. `decoder.type` is optional and defaults to `auto`. When
it is omitted, the implementation MUST infer a deterministic response decoder from the transform
where possible: `regex` to text, `csv` to CSV, `css` to HTML, and `prometheus`
to Prometheus exposition. jq/yq, XPath, and Python MAY use content detection
because they can operate on more than one response representation. If the
decoded response cannot be mapped to the selected transform, the exporter MUST
return a clear transform error. An explicit decoder remains available for
ambiguous or incorrectly labeled endpoints.

The entire `response` block is optional. CSV decoding MUST use a header row by
default when `response.csv.header` is omitted. The `response.csv` block is only
needed for non-default CSV options such as a custom delimiter, trimming, or a
headerless response. Omitting response configuration MUST NOT weaken validation:
decode failures and transform/input incompatibilities remain collector-level
errors controlled by `error_handling`; `metric.error_mode` applies after a
response has been decoded and an individual metric is being extracted.

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
```

Ambiguous formats SHOULD be rejected or require explicit configuration rather than guessed incorrectly.

### 6.1a Character encodings

A response MUST be converted to UTF-8 before its format is detected or it is
decoded, from the first of: a byte order mark (UTF-8, UTF-16LE, UTF-16BE),
which MUST be removed; the collector's `response.charset`; the `charset`
parameter of `Content-Type`; and, only when none of those named one, a
`<meta charset>` or `<meta http-equiv="Content-Type">` in the first 1024 bytes
of HTML, or the encoding of an XML declaration. Encoding names MUST be looked
up as the WHATWG Encoding Standard defines them. An unknown
`response.charset` MUST fail to load; an unknown declared name MUST fail the
`decode` stage naming it. The converted body MUST be what decoders, transforms
and Python scripts see, with `charset=utf-8` in its `Content-Type`, and an XML
declaration naming another encoding MUST be rewritten to UTF-8 so the XML
parser does not convert it again.

After every transform, label values and help texts that are not valid UTF-8
MUST have their invalid bytes replaced with U+FFFD rather than failing the
scrape, since Prometheus refuses a whole scrape over one of them. Each value
repaired MUST be counted in `http_exporter_invalid_utf8_total` for the
collector, and a probe that repaired any MUST log a warning with the count and
the first metric, suggesting `response.charset`. A label map a transform shares
among metrics MUST NOT be changed in place.

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

1. `model`: the shared data types.
2. `expr`: expression compilation and its caches.
3. `fetch`: request types, probe parameters and transports.
4. `decode`: response decoding and charset conversion.
5. `transform`: transforms, metric rules and the Python worker pool.
6. `config`: loading, validating and reloading the configuration and target
   files, and their schemas.
7. `exporter`: the HTTP server, the probe pipeline, self-metrics, scheduled
   targets and OTLP export.

`internal/testutil` MAY hold helpers shared by the tests of several packages
and MUST NOT be imported outside tests.

A probe and a scheduled target's scrape MUST make their trip to the target —
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
- Detect malformed JSON.
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

# 10. YAML decoder

The YAML decoder MUST:

- Parse YAML safely.
- Support YAML documents commonly returned by HTTP APIs.
- Support yq transformations.
- Handle missing keys according to collector error policy.

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
      expression: '#servers td:nth-child(2)'
      labels:
        - name: environment
          value: production
```

For CSS transforms, the metric expression selects the element whose text is
converted to a number. Expression labels are evaluated against that selected
element; use XPath when labels need to be read from a sibling or ancestor
element.

XPath equivalent SHOULD be supported.

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

The normalized representation MUST be available to jq/Python transformations where practical.

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

The decoder parses the text exposition format, version 0.0.4, with its own
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
- Metric and label names MAY be quoted UTF-8, including the braces form
  `{"my.metric", key="value"} 1` for a metric name that is not a bare name.
  `__name__` is reserved, and a label name may appear once per sample.
- HELP text and label values unescape `\\`, `\n` and `\"`; any other escape is
  an error.
- A value is a Go float without `p`, `P` or `_`, so `NaN`, `+Inf` and `-Inf`
  are accepted and hexadecimal floats and digit separators are not. A
  timestamp is an integer number of milliseconds.
- Comments other than HELP and TYPE, including OpenMetrics' `# EOF`, are
  ignored. A family that ends up without samples is dropped.
- Every error names its line: `text format parsing error in line N: ...`.

It deliberately differs from expfmt where expfmt was wrong for a scrape target:

- It accepts a body whose last line has no newline, CRLF line endings, and
  trailing blanks after the value, the timestamp or the TYPE; expfmt rejected
  all three.
- It rejects a histogram or summary count, or a bucket count, that is negative,
  NaN or infinite, which expfmt silently turned into an arbitrary integer.
- It rejects a label set with no metric name, such as `{a="b"} 1`, which
  expfmt attached to the previous line's family, and names that mix bare and
  quoted parts, such as `a"b"`, which expfmt spliced together.
- It never panics. expfmt panicked on inputs such as `{b="c",} 1`, which a
  target could serve to crash the exporter from a scheduled scrape, where no
  HTTP handler recovers the panic.

Families are returned in the order they were first seen, and series in the
order of their first sample, so a decode is deterministic.

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
- `limits.script_timeout` MUST bound the run of a script, not the start of the
  interpreter, which has its own budget of 10 seconds. A script that overruns
  MUST fail the run with a timeout error naming the limit, and its worker MUST be
  killed.
- A script error, including `SystemExit`, MUST fail that run with the Python
  error and leave the worker in service. A worker that exits, crashes, or writes
  an answer longer than `limits.max_output_bytes` MUST be discarded, and the next
  scrape MUST start another.
- A healthy worker MUST be reused at most 1000 times; at most four idle workers
  MUST be kept per pool, and an idle worker MUST be stopped after five minutes.
  Idle workers MUST be checked against that timeout on a timer, every minute,
  and not only when a worker is next asked for, so a collector that is no
  longer scraped does not keep its interpreters.
- A reload that removes or changes a collector's script MUST stop that script's
  idle workers at once and each busy one when its run finishes, counting them
  with the stop reason `reload`; workers of scripts the reload keeps MUST be
  left alone.
- The time each run takes in its worker — not starting one — MUST be kept per
  probe, the pre-script's and the python transform's together, and published
  as `http_exporter_script_duration_seconds` of the collector and of the
  request (§ 22).
- A worker MUST exit when the exporter does, which closing its request pipe
  achieves.

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

Every jq program MUST have `$root` bound to the whole decoded document, so an
expression evaluated against one item (§ 18.2) can reach the rest of it.

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
to `gauge`. Metric declarations MUST be placed on the collector, alongside
`transform`, rather than using transform-specific arrays such as `rules` or
`expressions`.

`error_mode` MUST be `ignore`, `log` or `fail` and defaults to `log`; the same
vocabulary as `error_handling` (§ 19). `warn`, the older spelling of `log` in
`error_handling`, MUST be accepted as meaning `log` and reported as deprecated
(§ 19). Any other value MUST be rejected at startup and on reload, and the
message MUST list the accepted values. It governs what happens when an individual metric cannot be
extracted — its expression or a label expression errors, its value is absent
while the metric is required, a required label is absent (§ 18.1), or its
value is not a number:

- `ignore` MUST skip that metric without logging and carry on. The probe MUST
  serve every metric that could be extracted; when none could, it MUST succeed
  with an empty body rather than fail.
- `log` MUST record the metric-specific error, naming the collector and the
  rule, and otherwise behave exactly as `ignore`.
- `fail` MUST record the error as `log` does and MUST then fail the whole scrape
  at that metric. No metric from that scrape MUST be served, including metrics
  that were extracted successfully, so a response is either complete or an
  error and never silently partial.

Under `ignore` and `log`, a metric-level error MUST NOT fail unrelated metrics in
the same collector. Modes apply per rule: a rule with `fail` that succeeds MUST
NOT fail the scrape because another rule, under `ignore` or `log`, did not.

A metric that is not required — `required: false`, or a collector with
`allow_missing_keys` — is not failing when its value is absent, and no mode
applies to it; it MUST be omitted without logging, under `fail` as under the
others.

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

A scheduled target (§ 42.14) has no HTTP response to carry the error. Under
`fail` its scrape MUST export nothing but `http_exporter_target_up` at 0, exactly
as for any other failed scheduled scrape; under `ignore` and `log` it MUST export
what could be extracted.

Each label entry MUST have `name` and exactly one of `value` and `expression`;
setting both or neither MUST be rejected at startup. A `value` label is static:
its value MUST be exported as written and never evaluated. An `expression`
label MUST be interpreted by the same transform as the metric expression:

| Transform | `expression` | `labels` |
| --- | --- | --- |
| `jq`, `yq` | jq-compatible expression evaluated against decoded data | jq-compatible expressions evaluated against the same data |
| `regex` | RE2 expression; capture group 1 is the numeric value | capture-group number or named capture group |
| `csv` | numeric column name | column names |
| `css` | CSS selector for numeric text | selectors relative to the selected element |
| `xpath` | XPath selecting numeric text | relative XPath or `@attribute` |
| `prometheus` | regular expression matching source metric names | destination label name to source label name |

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
shortened value would surprise. Truncation MUST apply to the labels of declared
metrics from every transform, and MUST happen before `metrics_prefix` (§ 5.0a)
is added.

An expression label that gives a series no value — a selector or path matching
nothing, a missing attribute, column, capture group or source label, a null —
or an empty value MUST be left off that series, in every transform, so the
text exposition and OTLP agree. An `expression` label MAY set
`required: true`. A series missing a required label MUST then be a missing
value of its metric: handled by the metric's `error_mode`, where `ignore` and
`log` drop that series alone and `fail` fails the scrape with an error naming
the label, counted as a missing key, and regardless of the metric's `required`
and `error_handling.allow_missing_keys`. For jq without `items`, where label
values pair with series by position, a required label giving neither one value
nor one per series MUST fail the metric. `required` on a `value` label, or on a
label of the python transform, MUST be rejected at startup.

Python transforms are the exception: their script emits the common metric
objects through `metric(...)`, so a `metrics` array is optional for them.

## 18.2 Metrics per item

For the `jq`, `yq` and `css` transforms a metric MAY set `items`. For `jq` and
`yq` it is a jq expression
selecting the things the metric is about. The value expression and every label
expression MUST then be evaluated once per item, with the item as `.` and the
whole document as `$root`, instead of as parallel streams over the whole
document paired by position. Parallel streams drift silently: a label
expression that yields nothing for one element shifts every later value onto
the wrong series, and one that yields a single value is applied to all.

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
label selector matching nothing leaves the label off. Without `items`, the
value is the text of each element the expression selects and label selectors
are matched within that element. `items` on any other transform MUST be
rejected at startup.

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
or attribute, augmenting it, binding it as a loop or `with` target, and calling
a method that mutates it all satisfy this.

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
when that interpreter is unusable. A configuration containing no Python MUST NOT
invoke an interpreter at all, so a deployment that uses none is unaffected.

A pre-script that returns a mapping or a sequence produces structured data. When
the collector's transform reads structured data — `jq`, `yq`, `none`, or an
unset transform — the exporter MUST treat that result as the decoded response
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

The policies MUST apply alike to a probe and to a scheduled target's scrape
(§ 42.14). A probe carrying on answers `200` with no collector metrics; a
scheduled scrape carrying on exports `http_exporter_target_up` 1 and no
collector metrics, counts as a success, and is logged at warning level under
`log` as `scheduled target stage failed; continuing` — through the failure log
(§ 25.1), and without logging the target as recovered, since the stage still
failed. A metric rule with `error_mode: fail` (§ 18.1) MUST fail the scrape of
either whatever `on_transform_error` says. Failing to read target credentials,
the concurrency limit (§ 42.13b) and validation of the result are not stages
these policies cover, and always fail.

`on_fetch_error` MUST govern failing to obtain the response, whatever the
request type (§ 5.1): for `http` a transport failure or a non-success status,
for `localfile` a file that cannot be read. It is named for the stage rather
than for one type, so it reads the same on every collector.

This is the vocabulary `error_mode` uses (§ 18.1). `warn`, this setting's
original spelling of `log`, MUST still be accepted and treated as `log`, and each
use MUST be reported as deprecated: logged at warning level on startup and on
every reload, naming the collector, the key and the replacement, and listed
under `deprecations` in the `config` check of `--dry-run` (§ 30.1), which still
passes. The value MUST be matched case-insensitively; anything else MUST be
rejected naming the collector, the key and the accepted values.

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
required value a rule could not find: a jq or yq value, a CSV column, a regex
that matched no text, an XPath or CSS selector that matched no nodes, and an
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
  script_timeout: 100ms
  max_cache_entries: 1000
```

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

Metric names SHOULD be normalized only when explicitly configured; silent surprising renaming is undesirable. A collector's `metrics_prefix` (§ 5.0a) is such explicit configuration, and validation applies to the prefixed names.

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
transform's output passes through, so it applies to probes, scheduled targets
and each file of a directory alike. A label map shared among metrics MUST NOT
be changed in place. Two labels of one series that escape to one name MUST fail
the scrape naming both, and two metrics that do so are duplicate series and
fail as such.

---

# 22. Exporter self-metrics

Expose exporter health metrics on `/metrics`.

At minimum:

```text
http_exporter_scrapes_total
http_exporter_scrape_success_total
http_exporter_scrape_duration_seconds
http_exporter_scrape_http_status_code
http_exporter_scrape_response_bytes

http_exporter_decode_success_total
http_exporter_parse_errors_total

http_exporter_transform_errors_total
http_exporter_missing_keys_total

http_exporter_script_errors_total
http_exporter_script_duration_seconds

http_exporter_metrics_emitted_total
http_exporter_invalid_utf8_total
http_exporter_series_limit_exceeded_total

http_exporter_cache_hits_total
http_exporter_cache_misses_total
http_exporter_cache_entries
http_exporter_cache_stale_served_total

http_exporter_probes_coalesced_total
http_exporter_probes_in_flight
http_exporter_probes_rejected_total

http_exporter_build_info
http_exporter_collector_config_valid
http_exporter_scheduled_targets

http_exporter_config_last_reload_successful
http_exporter_config_last_reload_success_timestamp_seconds
http_exporter_config_reloads_total

http_exporter_otlp_exports_total
http_exporter_otlp_export_retries_total
http_exporter_otlp_points_dropped_total
http_exporter_otlp_export_duration_seconds
http_exporter_otlp_last_export_success_timestamp_seconds
```

Labels should include `collector` and, where appropriate, `target`.

Names MUST follow the Prometheus conventions: a counter's name MUST end in
`_total`, and nothing else's may. Every family's name, type, help and value
MUST come from one definition, from which one set of self-metrics is built and
both rendered as text on `/metrics` and the self-metrics path and exported over
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
`targets` for the scheduled target document, reported only when one is
configured. Loading at startup MUST count as a successful load, setting the
first two; `http_exporter_config_reloads_total` MUST count only reloads after
startup, with `result` `success` or `failure`, both published from the start.
A rejected reload MUST set `http_exporter_config_last_reload_successful` to 0
and leave the timestamp at the last success, and a reload of one file MUST NOT
change the other's series.

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

An export is one delivery of everything pending, its retries included:
`http_exporter_otlp_exports_total` MUST count it once, with `result`
`success` when it got through and `failure` when it did not, both published
from the start. Retries MUST be counted in
`http_exporter_otlp_export_retries_total`. Data points dropped because the
endpoint refused them, or because they were the oldest waiting past
`otlp.max_pending_points` (§ 42.1a), MUST be counted in
`http_exporter_otlp_points_dropped_total`; data points kept for the next
export MUST NOT be. The duration MUST be that of the most recent export,
retries included, and the timestamp that of the last export that got through,
0 before the first. An export cut short by shutdown MUST NOT be counted, since
the last export delivers its data. These families MUST be part of the one
self-metric set, so they are also exported over OTLP.

### 22.0d Build information

The exporter MUST publish `http_exporter_build_info`, a gauge of value 1 whose
labels are `version`, `revision`, `goversion` and `request_types`, the built
request types joined with commas. The version MUST be the one set at build time
with `-ldflags "-X main.version=…"` when set, else the module version Go stamps
into the binary, `(devel)` when there is none. The revision MUST be the git
commit Go stamps, with `-modified` appended when the checkout had changes, and
`unknown` when the build was not from git. `--version` MUST print the same
four values on one line to stdout and exit 0 (§ 30), and the startup log line
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

`/metrics` MUST NOT require a target query parameter.

### 22.0a Resource metrics

The exporter MUST support publishing the standard `go_` and `process_` series
describing its own CPU and memory, configured under
`web.self_metrics.resource_metrics_enabled` and defaulting to false.

The names and semantics MUST match what `prometheus/client_golang` publishes, so
an existing Go dashboard or alert works against this exporter unchanged. That is
the only reason to use the prefix at all; a series named `go_goroutines` that
means something else is worse than no series.

They MUST default to off. Collecting them is not free — reading the memory
statistics briefly stops the world — and an exporter scraped frequently by
several Prometheus servers should not pay that cost unless it is asked for.

The runtime series MUST be available on every platform the exporter builds for.
The CPU class series MUST be requested from the runtime by name and omitted when
the Go release in use does not provide them, so the exporter publishes what its
runtime actually offers rather than a fixed list a toolchain upgrade could
invalidate.

The `process_` series MUST reflect the operating system's view of the process
and MUST be omitted where that view is unavailable. They MUST NOT be synthesised
from runtime accounting: `process_cpu_seconds_total` is CPU the process
consumed, while the runtime's total CPU class is GOMAXPROCS multiplied by wall
time and includes idle. Publishing one under the other's name is wrong in a way
that only appears when somebody trusts the graph.

Where a standard series cannot be produced faithfully it MUST be omitted rather
than approximated. `go_gc_duration_seconds` MUST therefore be published as a
summary with its count and sum only, because the quantiles are not available
from the runtime statistics the exporter reads; it MUST NOT be published as two
counters, which would give the family a type client_golang does not give it.

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
what a collector's response cache holds, and `http_exporter_probes_in_flight`,
which counts its trips in progress. The two labels are the request
type's (§ 5.1): for `http` the method and the URL described below, for
`localfile` `READ` and the file's `file://` URL.

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
its requests' values for every counter, and the repository's tests MUST check
this.

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

A metric family may be described only once in an exposition. The labelled series
MUST therefore join the family the per-collector block has already declared,
without a second `HELP` or `TYPE` line; only the families verbose mode
introduces may declare their own.

Scheduled targets MUST be recorded the same way as probe requests. A scheduled
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
applies to both `/probe` and scheduled targets.

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
```

`http_exporter_collector_scrape_duration_seconds` MUST observe the duration of
every trip to the target, from sending the request to having validated metrics,
for `/probe` and scheduled targets alike. A probe answered from the response
cache, or by sharing another probe's request (§ 42.13a), made no trip and MUST
NOT be observed. The buckets MUST be fixed at 0.005, 0.01, 0.025, 0.05, 0.1,
0.25, 0.5, 1, 2.5, 5, 10, 30 and 60 seconds, plus `+Inf`. Every configured
collector MUST have a histogram, empty until its first trip. Durations MUST only
be recorded while verbose mode is on, so turning it on does not publish a
history nobody asked to be kept.

Its name MUST differ from `http_exporter_target_scrape_duration_seconds`, the
gauge a scheduled target's health series carry over OTLP (§ 42.14): the two
would otherwise reach an OTLP backend as one name with two types.

The Python families MUST be published for every collector with a Python
transform or pre-script, and for no other. `state` MUST be one of `starting`,
`idle` and `busy`; `reason` one of `timeout`, `crash`, `output_limit`,
`cancelled`, `retired`, `surplus`, `idle` and `reload`; `outcome` one of `ok`,
`script_error`, `timeout`, `output_limit` and `failed`. Every value of `reason`
and `outcome` MUST be published, zero included, so a rate can be taken before
the first event. The pool MUST keep these counts regardless of verbose mode,
since it maintains them anyway; only their publication depends on it.

The `http_exporter_python_pool_` families MUST report the Python execution pool
as a whole, with the same `state`, `reason` and `outcome` values. They MUST be
published whenever verbose mode is on, whether or not any collector uses Python,
so the pool's status can be monitored without knowing which collectors run
scripts. They MUST sum every collector the pool has served, including one a
reload has removed, so a counter never decreases. They MUST have names of their
own rather than being unlabelled series of the per-collector families, so that
summing a per-collector family never counts a run twice.

Each family MUST declare `HELP` and `TYPE` once, before its series, and MUST be
delivered over OTLP like the rest of the self-metrics.

---


# 23. Health endpoints

Implement:

```text
/health
/ready
/metrics
/probe
```

Recommended behavior:

- `/health`: process is alive. MUST answer `200` for as long as the process
  serves requests.
- `/ready`: the exporter is doing what it was configured to do. It MUST answer
  `503` while the last reload of the configuration or of the scheduled target
  file was rejected (§ 22.0b), until a reload of it is accepted, and, with OTLP
  export enabled and `otlp.unready_after_failures` set to N above 0, while the
  last N exports to the current endpoint failed (§ 42.1a), until one gets
  through; from a `SIGTERM` or `SIGINT` on, while the exporter shuts down
  (§ 30); `200` otherwise. `otlp.unready_after_failures` MUST default to 0,
  which never makes the exporter unready over OTLP: an exporter whose exports
  fail still answers probes, and a Kubernetes pod that is not ready stops
  receiving them. The failures MUST be counted per endpoint, so a reload that
  changes `otlp.endpoint` starts the count again, and a negative value MUST be
  rejected. A `503` body MUST name each reason on a line of its
  own starting `not ready:`, and MUST NOT include an error's text, since the
  endpoint is never authenticated and an error can quote a path, a URL or a
  line of the configuration.
- `/self-metrics`: exporter self-metrics by default; the path MUST be configurable and `/metrics` MAY remain as a compatibility alias.
- `/probe`: execute a collector against a supplied target. A request without
  `collector` MUST answer `400` saying the collector parameter is required,
  with the URL's shape, and one without a target its collector's request type
  requires MUST answer `400` naming the collector and the request type.

`/probe`, `/metrics` and the self-metrics path MUST compress their answer with
gzip when the request's `Accept-Encoding` accepts `gzip` (or `x-gzip`, or `*`)
with a non-zero quality — Prometheus asks for it on every scrape — and answer
uncompressed otherwise, and for `HEAD`. A compressed answer MUST carry
`Content-Encoding: gzip` and no `Content-Length`; each of these answers, compressed or
not, MUST carry `Vary: Accept-Encoding`. Errors are compressed like any other
answer. `/health`, `/ready` and `/-/reload` MUST NOT be compressed: their
answers are a line or two, and a load balancer checking health may not decode
gzip.

The exporter's own HTTP server MUST bound what a client can hold open: request
headers within 10 seconds, the whole request within 30 seconds, and an idle
keep-alive connection closed after two minutes. It MUST NOT have a write
timeout, which would cut off a probe that legitimately takes as long as its
scrape timeout.

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
configuration file the exporter was given, including the scheduled target
document, so a deployment does not have to reason about which files are watched.

The watch interval MUST be configurable and MUST have a documented default of
60 seconds, which keeps an idle exporter from stating its configuration files
continuously while still picking a change up promptly enough for a reload. A
non-positive interval MUST be rejected at startup rather than silently disabling
the watch that was explicitly requested. The exporter SHOULD report whether the
watch is active in its startup log.

Changes MUST be detected by file modification time rather than by filesystem
event notification. Kubernetes republishes a mounted ConfigMap by atomically
swapping a `..data` symlink, which replaces the inode a file-level event watch
is attached to; such a watch stops firing after the first change unless it
watches the directory and re-arms. Polling is unaffected by this.

The watch MUST cover the collector files (§ 5.0): a collector file changed, a
file newly matching a pattern, and a file removed MUST each reload the
configuration, although the configuration file itself did not change. A reload
that fails MUST NOT be retried until one of the files changes again.

Enabling the watch MUST NOT weaken any reload rule: an invalid configuration, a
configuration that would disable OTLP while scheduled targets are loaded, a
collector name defined twice, and a pre-script that stops producing `data` MUST
all still be rejected with the last valid configuration left active.

When a disabled watch is configured, the exporter MUST NOT run a polling loop at
all.

### 24.1a Reloading on demand

The exporter MUST reload when asked, not only when the watch finds a change:

- On `SIGHUP`, always.
- On `POST` or `PUT` `/-/reload`, when started with `--web.enable-lifecycle`,
  which MUST default to off. Without the flag the endpoint MUST answer `403`
  saying how to enable it; any other method MUST answer `405` with
  `Allow: POST, PUT`. The endpoint MUST be protected by `web.basic_auth` like
  `/probe` and `/metrics`. It MUST answer `200` when every file was accepted,
  and `500` with the reason when one was rejected, the previous configuration
  staying in force.

After any reload, whatever its trigger, the per-collector state MUST follow the
new configuration before it is next used: a removed collector's self-metric
series, per-request series, scrape-time histogram, cached results and
remembered failures MUST be dropped, so its series stop being exposed and
exported and Prometheus marks them stale, and a collector added again under
the name MUST start from zero; a collector whose definition changed MUST keep
its counters and MUST have its cached results dropped, since their keys carry
the old definition; an unchanged collector MUST keep everything.

Both MUST reload the configuration, with its collector files, and the
scheduled target file when there is one, whether or not they changed, under the
same rules as the watch (§ 24.1), and record the result in the reload
self-metrics (§ 22.0b). Every reload, whatever its trigger, MUST be logged with
the trigger — `watch`, `sighup` or `http` — and reloads MUST be serialized, so
two triggers at once never interleave. The Helm chart MUST expose the flag as a
value (SPECIFICATION-CHART.md § 33.10).

### 24.2 Validation of metric rules

Everything about a metric rule that can be known before a scrape MUST be
checked when the configuration loads — at startup, on reload and by `--dry-run`
— with a message naming the collector, the rule and, where it applies, the
label:

- A declared metric name MUST be a valid Prometheus metric name and MUST NOT
  start with `__`.
- Every expression MUST compile in its transform's language: jq and yq value,
  `items` and label expressions (compiled, not only parsed, so an undefined
  function or variable is caught); regular expressions; CSS selectors, which
  goquery would otherwise silently treat as matching nothing; XPath expressions
  and relative label expressions, with the collector's namespaces; and a
  prometheus transform's patterns, `include` and `exclude`.
- A regex label MUST name a capture group the regex has, by number or name.
- A prometheus transform's `rename` targets MUST be valid metric names, and its
  `labels` keys and `rename_labels` targets valid label names.

### 24.2a Decoding errors

A configuration, collector or scheduled target file that cannot be decoded
MUST be refused with errors in the file's own terms, never the
implementation's: each names its line and says what was wrong there — an
unknown key and where it was found (`in a collector`, `in retry`, `in a
scheduled target's request`), a value of the wrong kind with what was
expected (`a duration such as 30s`, `a whole number`, `a list of values`,
`true or false`) and what was found (`a list`, `a mapping`, `the string
"fast"`, `the number 3`). A message MUST NOT name a Go type or package or a
YAML tag. Every error in the file MUST be reported at once, joined by `; `,
rather than only the first. A YAML syntax error is reported as the YAML
parser words it.

Unknown keys MUST be refused everywhere in these files, including in blocks
decoded by custom code such as a scheduled target's `request`, where they
would otherwise be ignored without a word.

### 24.3 Configuration schema

The repository MUST publish a JSON Schema (draft 2020-12) of the configuration
file, `config.schema.json`, so editors can complete keys, show descriptions and
flag unknown keys and invalid values. It MUST be generated from the Go
configuration structs, with the allowed values, patterns, required keys and
descriptions the structs cannot express added by path, so a key added to the
configuration cannot be missing from it. `--config.schema` MUST print the
schema of the running binary — its `request.type` values are the request types
that binary was built with (§ 5.1) — and exit 0.

A test MUST fail when the committed file differs from what the code generates,
when a key the configuration reads is missing from the schema or the schema has
a key the configuration does not read, when any shipped configuration (the
examples, the demo configurations and the chart's default configuration) does
not validate against it, and when it accepts any of a set of invalid documents.
The example configurations MUST begin with the `yaml-language-server` modeline
pointing at the published schema.

The configuration schema MUST require `collectors` or `collector_files`, each
non-empty when it is the one present. The repository MUST also publish
`collector-file.schema.json`, the schema of a collector file (§ 5.0): a required,
non-empty `collectors` list and no other key, its collectors described by the
same rules as the configuration's, which a test MUST check.
`--config.collector-file-schema` MUST print it and exit 0; a test MUST fail when
the committed file differs from what the code generates.

The schema describes the canonical spelling, and MUST allow an unquoted number
or boolean where the exporter reads a string, since YAML reads `expression: 1`
as a number. Startup validation remains the authority on what is valid.

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

The configured log level MUST apply to those lines too.

A logged metric extraction failure MUST name the collector as well as the rule.
A metric name is not unique across collectors — the same rule is often copied
between them — so the rule name alone does not say which collector to go and
look at. A logging path MUST NOT be what fails a scrape, so an absent collector
MUST degrade to an empty name rather than panicking.

The startup line MUST report the listen address, the number of collectors, the
number of scheduled targets, and whether the configuration watch is enabled.
When the watch is enabled it MUST also report the interval, because that is what
bounds how stale a running configuration can be; when it is disabled the
interval MUST be omitted rather than reported as a value that has no effect.

### 25.1 Repeated failures

A failure MUST be logged in full the first time. While the same thing keeps
failing the same way — the same collector, target, file of a directory where
there is one, stage and error text — a repeat MUST be logged at debug level
only, marked `"repeat":true`, except that once five minutes have passed since
the last line at the failure's own level it MUST be logged at that level again
with `repeated`, the occurrences since that line, and `failing_since`, when the
failure began. A different stage or error MUST be logged at once as a new
failure. The first success after a failure MUST be logged at info level with
the stage, `failed_for` and `failures`. This MUST apply to failed probes and
stages continuing under `log`, a rule failing under `error_mode: fail`, probes
rejected by `max_concurrent_probes`, failed scheduled scrapes, failed files of a
directory, a directory over `max_files` or its listing bound, and repaired
invalid UTF-8. At most 10,000 failures MUST be remembered; when full, those not
reported for an hour MUST be forgotten, at most once a minute, and a new one
that finds no room MUST be logged every time. The failures of a collector a
reload removed MUST be forgotten.

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

---

# 26. Security

The exporter will be capable of fetching arbitrary URLs and executing configured Python code; therefore security must be considered explicitly.

Requirements:

- Never log credentials.
- Avoid SSRF escalation where reasonable.
- Consider configurable allowed URL schemes (`http`, `https`).
- Consider optional target allowlists/deny lists.
- Protect against excessively large responses.
- Bound regex, jq, yq, and Python execution.
- Restrict Python networking/process/file capabilities.
- Do not install packages dynamically during probes.
- Do not accept arbitrary Python code through query parameters.
- Collector names MUST map only to server-side configured code/config.

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

`testdata/config.grafanastatus.json-test.yaml` is the reference: a collector
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
--python.path=/usr/local/bin/python3
--otlp.targets-file=/etc/exporter/targets.yaml
--config.watch
--config.watch-interval=60s
--config.schema
--config.collector-file-schema
--probe.timeout-offset=500ms
--web.enable-lifecycle
--web.shutdown-timeout=5s
--web.shutdown-delay=0s
--version
```

`--web.shutdown-timeout` bounds how long a `SIGTERM` or `SIGINT` waits for the
probes in progress, 5 seconds by default; a value that is not positive MUST be
refused as a malformed command line. When it runs out, the connections still
open MUST be closed, which MUST be logged with the timeout, before the last
OTLP export (§ 42.1a) and a clean exit.

`--web.shutdown-delay` is how long a `SIGTERM` or `SIGINT` keeps serving before
that graceful shutdown begins, 0 by default, which begins it at once. From the
signal on, `/ready` MUST answer `503` with the reason `the exporter is shutting
down`, while `/probe`, `/metrics`, the self-metrics path and `/health` MUST
keep answering as before, so a load balancer or a Kubernetes Service stops
sending probes before the listener closes rather than having them refused.
Connections MUST NOT be kept alive past their current request during the
delay, so clients reconnect to another instance. The exporter MUST log the
delay when it starts one. A second signal during the delay MUST end the process
at once, as during the shutdown timeout. A negative value MUST be refused as a
malformed command line.

`--version` prints the build information of § 22.0d and exits 0.

`--config.schema` prints the configuration file's JSON Schema and exits, and
`--config.collector-file-schema` the schema of a collector file (§ 24.3).

`--otlp.targets-file` is optional and selects the scheduled target document
defined in section 42.14.

`--python.path` selects the interpreter used by the `python` transform. It MUST
default to an interpreter resolvable through `PATH` and MUST accept an absolute
path so a deployment can point at a specific interpreter.

Optional:

```text
--log.level=info
--log.format=json
```

The exact flag names can follow Prometheus ecosystem conventions.

### 30.1 Configuration check

`--dry-run` MUST validate what startup would load and exit, without binding the
listen address, starting the configuration watch or the OTLP loop, or contacting
any target. It exists so a pipeline, a pre-deploy hook or an init container can
ask whether a configuration would start without starting it.

The check MUST run the same validation functions startup runs, in startup's
order, and MUST honour the flags that change what startup loads:
`--config.file`, `--otlp.targets-file`, `--config.export-env`, `--python.path`,
and `--config.watch` with `--config.watch-interval`. A configuration that
`--dry-run` passes MUST start with the same files and flags, and one it fails MUST
be refused by startup; a test MUST pin this agreement for every failure the
check can report.

The steps MUST be:

- `config` — the configuration file and its collector files (§ 5.0) load and
  validate, including the rule checks of § 24.2 and the uniqueness of collector
  names; the collector files read are listed under `details.collector_files`;
  deprecated spellings accepted during validation are listed under
  `details.deprecations` and logged, and do not fail the check;
- `python_scripts` — every Python script compiles and every pre-script produces
  `data` (§ 16), with each faulty script reported as its own error. A
  configuration without Python MUST pass without needing an interpreter;
- `config_watch` — only when `--config.watch` is set, that the interval is
  positive;
- `targets` — only when `--otlp.targets-file` is set, that the file is valid on
  its own and against the configuration (§ 42.14).

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
MUST print usage to stdout and exit `0`.

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
with the race detector, twice, shuffled (`go test -race -count=2 -shuffle=on`).
A test MUST NOT depend on state another test, or an earlier run of itself, left
behind. Process-wide state a test reads counts from MUST be replaceable per
test: the Python worker pool MUST be reached through one replaceable reference,
each test that uses it MUST get a fresh pool, and that pool's workers MUST be
stopped when the test ends.

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

CI MUST run the complete test suite on every change and MUST fail on test, build, vet, lint, or required Helm validation failures.

## 34.3 HTTP fetcher tests

Test at minimum:

- GET requests.
- POST requests with body.
- Configured request path joining with target URL.
- Query parameters.
- Custom headers.
- Basic authentication.
- Bearer authentication.
- TLS with a test CA/certificate.
- TLS verification failure.
- Target TLS verification can be disabled in collector configuration and
  overridden per scrape with `insecure_skip_verify=true|false`.
- Configured and per-scrape fixed-delay retries for transient failures.
- Per-scrape request timeout override.
- Incoming scrape deadline propagation when no timeout override is supplied.
- HTTP redirects according to policy.
- HTTP 2xx handling.
- HTTP 3xx handling according to policy.
- HTTP 4xx handling.
- HTTP 5xx handling.
- Invalid target URLs.
- Unsupported HTTP methods.
- Invalid method, path, body, and timeout overrides.
- Invalid retry count and retry backoff overrides.
- Empty response bodies.
- Response body exactly at the maximum allowed size.
- Response body exceeding the configured maximum size.
- Slow responses that exceed timeout.
- Connection failures.
- Concurrent requests to multiple targets.

Verify that secrets such as Authorization headers are never written to normal logs or error responses.

## 34.4 Probe endpoint tests

Test `/probe` with:

- Missing `target`.
- Missing `collector`.
- Empty `target`.
- Empty `collector`.
- Unknown collector.
- Valid collector.
- Invalid target scheme.
- Valid HTTP target.
- Valid HTTPS target.
- Collector-specific request configuration.
- Multiple simultaneous probes for the same collector.
- Multiple simultaneous probes for different collectors.

- `POST`, `PUT`, `DELETE`, `PATCH` and `OPTIONS` answered `405` with
  `Allow: GET, HEAD`, the target not contacted and no probe counted; `HEAD`
  and `GET` still probe.

Verify correct HTTP status codes, useful error text, and that probe failures do not crash the process.

## 34.5 Configuration tests

Test:

- Valid configuration.
- Empty collector list.
- Duplicate collector names.
- Invalid collector names.
- Unknown decoder type.
- Unknown transformation type.
- Invalid HTTP method.
- Invalid timeout values.
- Invalid limits.
- Invalid authentication configuration.
- Invalid TLS configuration.
- Invalid jq expression.
- Invalid yq expression.
- Invalid XPath.
- Invalid CSS selector.
- Invalid regex.
- Invalid Python source.
- Invalid CSV configuration.
- Invalid metric `error_mode`, with a message listing `ignore`, `log` and `fail`.
- Missing `error_mode` defaults to `log`.
- `error_mode: fail` is accepted.
- Transform-specific response format incompatibility.
- Decoder inference when `decoder.type` is omitted, and a leftover
  `response.format` rejected with a pointer to `decoder.type`.
- Invalid error policy values.
- Missing required configuration fields.
- Unknown configuration fields according to the chosen strictness policy.
- Decoding errors in the file's terms (§ 24.2a): an unknown key named with
  its place, a wrong kind of value with what was expected and found, a bad
  duration or size with its line, several errors reported together, no Go
  type or YAML tag in any message, and YAML syntax errors unchanged.
- Unknown keys in a scheduled target's `request`, its `retry` and
  `basic_auth` refused, and a valid block accepted.

Configuration validation MUST identify the collector and relevant field in the error message.

## 34.6 Configuration reload tests

Test:

- Initial valid configuration loads successfully.
- Invalid initial configuration prevents startup as documented.
- Valid configuration can be reloaded.
- Adding a collector makes it available after reload.
- Removing a collector makes it unavailable after reload.
- Modifying a collector changes its behavior after reload.
- Invalid reload does not destroy the last known-good configuration.
- Concurrent probes continue safely during reload.
- Removed collectors are not served after a successful reload.
- Deleted/renamed collectors do not leave stale state.

If reload uses a signal, file watcher, or admin endpoint, test the actual mechanism.

## 34.7 Common metric model tests

The internal `Metric`/`MetricSet` representation MUST be tested independently of every decoder.

Test:

- Gauge.
- Counter.
- Histogram.
- Summary.
- Metric help text.
- Metric names.
- Label names.
- Label values.
- Empty label sets.
- Multiple labels.
- Timestamp handling.
- Duplicate metric/label-series detection.
- Invalid metric names.
- Invalid label names.
- NaN.
- Positive infinity.
- Negative infinity.
- Zero and negative gauge values.
- Counter constraints/validation as applicable.
- Collision handling when two extraction rules create the same series.

## 34.8 JSON decoder and jq tests

Test valid JSON:

- Scalar root.
- Object root.
- Array root.
- Nested objects.
- Arrays of objects.
- Null values.
- Boolean values.
- Numeric values.
- Strings containing escapes.
- Large but valid documents.

Test invalid JSON:

- Truncated document.
- Invalid syntax.
- Invalid UTF-8 where relevant.

Test jq:

- Simple field extraction.
- Nested field extraction.
- Array iteration.
- Filtering.
- Mapping.
- Arithmetic.
- String conversion.
- Object construction.
- Label construction.
- Returning zero results.
- Returning multiple results.
- Returning invalid metric data.
- Syntax errors.
- Runtime jq errors.
- Type mismatches.
- Missing keys.

Verify `allow_missing_keys` and per-metric `required` behavior separately from malformed JSON behavior.

## 34.9 YAML decoder and yq tests

Test:

- Valid YAML mappings.
- Valid YAML sequences.
- Nested YAML.
- YAML scalar values.
- Multi-document YAML if supported; otherwise verify that it is rejected clearly.
- YAML anchors/aliases if supported by the implementation.
- Nulls.
- Booleans.
- Numeric values.
- Strings.
- Malformed YAML.

Test yq expressions analogous to the jq tests:

- Field extraction.
- Nested extraction.
- Array iteration.
- Filtering.
- Mapping.
- Arithmetic.
- String operations.
- Object construction.
- Multiple results.
- Zero results.
- Syntax errors.
- Runtime errors.
- Missing keys.

## 34.10 XML decoder and XPath tests

Test:

- Simple XML.
- Nested XML.
- Attributes.
- Multiple matching nodes.
- Namespaces.
- Namespace-prefixed XPath.
- Text nodes.
- Numeric conversion.
- Empty nodes.
- XML entities.
- XML declarations.
- Malformed XML.
- XPath syntax errors.
- XPath expressions yielding zero results.
- XPath expressions yielding multiple results.

Test both metric values and label extraction from attributes/elements.

## 34.11 CSV decoder tests

Test:

- Header row.
- Headerless CSV when supported.
- Standard comma delimiter.
- Tab-delimited data.
- Alternate delimiters.
- Quoted fields.
- Escaped quotes.
- Commas inside quoted fields.
- Newlines inside quoted fields.
- Empty fields.
- Missing values.
- Extra columns.
- Missing columns.
- Unicode.
- Empty input.
- Malformed CSV.
- Leading/trailing whitespace according to configuration.
- Numeric conversion failures.

Verify the normalized row/object representation before metric generation.

## 34.12 HTML decoder tests

Test:

- Basic HTML document.
- Tables.
- Multiple rows.
- Nested elements.
- CSS selectors.
- XPath selectors.
- Bare element selectors such as `h1`.
- Attributes.
- Text extraction.
- HTML entities.
- Whitespace normalization.
- Missing selectors.
- Selectors matching multiple nodes.
- Malformed but browser-like HTML that should still parse.
- Invalid CSS selectors.
- Invalid XPath expressions.

Include representative ugly/legacy HTML fixtures because this is a core use case.

## 34.13 Prometheus decoder tests

The Prometheus decoder MUST be tested against representative Prometheus exposition data including:

- Gauge.
- Counter.
- Histogram.
- Summary.
- `_created` series when present.
- `+Inf`, `-Inf`, and `NaN` where supported.
- Multiple labels.
- Escaped label values.
- HELP lines.
- TYPE lines.
- Timestamps.
- Multiple metrics.
- Duplicate samples.
- Invalid metric names.
- Invalid label names.
- Invalid exposition.
- Empty exposition.
- Comments.

Test that the decoder converts the exposition into the common internal metric model rather than simply copying raw text.

Also required, for the parser in §14.1:

- Histogram and summary grouping, independent of label order.
- Quoted UTF-8 metric and label names, including the braces form.
- Every error case names its line.
- The inputs on which expfmt panicked are rejected without a panic.
- A missing final newline, CRLF and trailing blanks are accepted.
- Negative, NaN and infinite counts are rejected.
- Output order is deterministic.
- `go.mod` does not require `prometheus/common`, `prometheus/client_model`,
  `google.golang.org/protobuf` or `munnerz/goautoneg`.
- A fuzz target (`FuzzParsePrometheusText`) whose seeds run with the ordinary
  suite, and which never produces a metric without a name.

Test the canonical metric declaration shape with `name`, `description`,
`type`, `labels`, and `expression` for every non-Python transform. Verify that
only the five allowed Prometheus metric types are accepted, that descriptions
become HELP text, and that transform-specific expression semantics are applied
consistently.

Test that a transform `pre_script` runs once before extraction, can mutate or
replace `data`, is subject to timeout/output restrictions, and is reparsed for
HTML/XML output.

Test resource metrics:

- None of the `go_` or `process_` series appear unless configured.
- With the setting on, every runtime series is published, `go_info` carries this
  binary's Go version, and the values are the live ones rather than zeros.
- The CPU class series are requested from the runtime by name and are counters.
- The `process_` series appear where the operating system's view is available
  and are absent where it is not, rather than being synthesised.
- The process statistics are parsed correctly when the executable name contains
  spaces or parentheses, and a malformed line yields nothing.
- Publishing them beside the verbose series declares no metric family twice.
- A reload turns them on and off.
- They reach the OTLP self-metric set on the same terms.

Test verbose per-request self-metrics:

- No labelled series and none of the verbose-only indicators appear while
  verbose mode is off, and the per-collector self-metrics are unaffected.
- With verbose mode on, every self-metric family except
  `http_exporter_cache_entries` is published per request, alongside the
  unchanged per-collector series.
- Each metric family is declared exactly once in the exposition: publishing both
  shapes must not repeat a `HELP` or `TYPE` line.
- The collector's totals equal the sum of its requests' values, across
  successful and failing probes alike.
- The `url` label drops userinfo credentials and the query string, for a query
  in the target, a query from `request.query`, and a scheme-less target.
- Distinct URLs and methods produce distinct series, and repeating a request
  updates its series rather than adding one.
- A failing scrape records its HTTP status code on its own series.
- Scheduled target scrapes are recorded per request.
- The series set stops growing at 1000 combinations, an already-tracked request
  keeps updating past the limit, and the capped indicator reads 1.
- A response served from the collector cache leaves the last-scrape timestamp
  and status of the scrape that filled it untouched, and the cache hit is
  counted on the request's own series.
- A scheduled target is listed with zero counters and a zero timestamp before
  its first collection, and `http_exporter_request_series_tracked` counts it.
- `http_exporter_request_series_tracked` counts distinct combinations, so
  scraping the same request twice does not increase it.
- Registering a request never invents a scrape and never overwrites one that
  has already been recorded.
- A configuration reload turns verbose mode on and off, turning it off drops the
  labelled series, and the per-collector series are unaffected by either switch.

Test the configuration watch:

- The watch is off by default, and the reload loop returns immediately rather
  than idling, so a configuration change is not picked up.
- An enabled watch reloads a changed configuration file and a changed scheduled
  target file.
- The watch stops when its context is cancelled and reloads nothing afterwards.
- An enabled watch still rejects an invalid configuration and leaves the
  previous one active, and a later valid configuration still loads.
- A non-positive watch interval leaves the watch disabled at the manager level
  and is rejected at startup when the watch was explicitly requested.

Test the pre-script `data` contract:

- A pre-script that produces `data` is accepted for every shape that does so:
  replacing it, mutating it by key, nested key or attribute, augmenting it,
  annotated and tuple and starred assignment, a mutating method call on it or on
  a nested part of it, and binding it as a loop or `with` target.
- A pre-script that assigns another name, computes and discards, only reads
  `data`, or mutates a different object is rejected, naming the collector.
- A pre-script that reads `data` extensively — including through a method call
  such as `data["rates"].items()` — and assigns its result to another name is
  rejected. Reading is not producing, and each non-mutating method is covered
  individually so none of them can be mistaken for a mutation.
- A reload is held to the same rule, so a script that only reads `data` is not
  activated by one either.
- A Python syntax error in a pre-script or a `python` transform script is
  rejected at startup.
- A `python` transform script that never mentions `data` is accepted.
- Every faulty script is reported, not only the first, and valid collectors are
  not named.
- A configuration with no Python scripts needs no interpreter; one with scripts
  fails clearly when the interpreter is unusable.
- A reload whose pre-script stops producing `data` is rejected and the previous
  configuration stays active.
- The shipped example configurations satisfy the contract.

Test structured pre-script results:

- A text response reshaped into a mapping is extracted by ordinary `jq` metric
  rules, including label expressions, and produces the declared name, help, and
  type.
- Promotion applies to `jq`, `yq`, `none`, and an unset transform, for any
  original response format, and to both mapping and sequence results.
- Promotion does not apply to `csv`, `regex`, `css`, `xpath`, or `python`, which
  keep receiving their decoded format.
- A scalar pre-script result leaves the decoded format unchanged, so HTML output
  is still reparsed and regex input is still text.
- A structured transform whose pre-script returns a scalar still fails with the
  response-format error.

Test that metric-level `error_mode: log` records an extraction error and skips
only that metric, while `error_mode: ignore` skips it silently without failing
the collector or unrelated metrics.

Test `error_mode` end to end through `/probe`, for each mode, with one rule that
succeeds beside one that fails:

- `ignore` answers 200 with the successful metric and logs nothing; `log` does
  the same and logs one `metric extraction failed` line naming the collector,
  the rule and the mode.
- `fail` answers 502 with a JSON body carrying `status`, `stage`, `collector`,
  `metric`, `target` and `error`, serves no exposition text at all — not even
  the metric that succeeded — and logs the failure.
- `ignore` and `log` answer 200 with an empty body when no metric can be
  extracted.
- A `fail` rule that succeeds beside a failing `log` rule answers 200.
- An optional rule (`required: false`, or `allow_missing_keys`) with an absent
  value is not a failure under `fail`, and is not logged.
- `fail` still answers 502 when the collector sets `on_transform_error` to
  `ignore` or `warn`.
- A `fail` response is not cached: two probes contact the target twice.
- The self-metrics count a `fail` as a failed probe, a transform error and, for
  an absent value, a missing key.
- Credentials in the target URL do not appear in the JSON body.
- Every per-rule transform — jq, regex, CSV, XPath over XML and HTML, and CSS —
  reports a failing `fail` rule as the same identifiable metric failure.
- On a scheduled target, `fail` exports `http_exporter_target_up` at 0 and no
  collector metrics, while `log` exports what could be extracted.

Test subsequent transformations such as:

- Include/filter by metric name.
- Exclude metric name.
- Rename metric.
- Add labels.
- Remove labels where supported.
- Label replacement where supported.
- Collision detection after transformation.

## 34.14 Plain-text and regex tests

Test:

- One match.
- Multiple matches.
- No matches.
- Capture groups.
- Named capture groups if supported.
- Numeric capture conversion.
- Labels captured from text.
- Multiple metrics from one line.
- Multiple metrics from multiple lines.
- Regex syntax errors.
- Runtime matching failures where relevant.
- Very long lines.
- Empty lines.
- Unicode text.

Verify that a valid response with no matching rules is distinguishable from a decoder failure.

## 34.15 Python transform tests

Python is a first-class transform and requires a dedicated test suite.

Test:

- Basic Python script execution.
- Python script receiving raw body.
- Python script receiving decoded data when available.
- Headers access.
- HTTP status access.
- Target access.
- Metric creation.
- Multiple metrics.
- Labels.
- Metric types.
- Help text.
- Timestamps.
- Empty result set.
- Python exceptions.
- Syntax errors.
- Invalid metric definitions.
- Standard library imports.
- Every bundled third-party library documented as supported.
- `required_libs`/`libraries` validation.
- Unsupported external library declaration.
- Version/availability checks for bundled dependencies if version constraints are supported.
- Execution timeout.
- Excessive metric generation.
- Excessive output/logging if limited.

Python scripts MUST NOT be able to perform arbitrary network or process execution. Test that prohibited functionality is unavailable or denied, including at minimum:

```text
socket/network access
subprocess
os.system
process creation
arbitrary executable launch
```

Do not make security claims based only on code review; include automated negative tests for the enforced runtime restrictions.

## 34.16 Python library compatibility tests

For each bundled external Python library, add a minimal test script that imports it and exercises the primary documented functionality.

At minimum, for the initially supported libraries:

- lxml: parse XML and run XPath, and parse HTML with `lxml.html` inside the
  sandbox and select table cells with XPath.
- PyYAML: parse YAML.
- python-dateutil: parse a representative timestamp.

The test suite MUST fail if a declared supported library is missing from the shipped runtime.

## 34.17 Python isolation/resource tests

Test that Python execution obeys configured limits:

- Maximum execution time.
- Maximum emitted metrics.
- Maximum labels/label length if enforced globally.
- Maximum script output/log size if enforced.
- Maximum memory or documented process/container boundary behavior.

A script that exceeds its execution limit MUST terminate cleanly without taking down the exporter.

## 34.18 Error policy tests

All decoder and transformation paths MUST test error behavior under the configured policy.

For each relevant failure type test:

```text
fail
warn
ignore
```

Failures to test:

- HTTP error.
- Decode error.
- Transformation error.
- Missing key.
- Missing XML node.
- Missing HTML selector match.
- Missing CSV column.
- Regex with no match where a value is required.
- Python exception.
- Metric validation error.
- Series/cardinality limit exceeded.

Verify the resulting probe status, emitted metrics, logs, and exporter self-metrics.

## 34.19 Missing-key/optional-field tests

Explicitly test:

```yaml
allow_missing_keys: false
```

versus:

```yaml
allow_missing_keys: true
```

and per-metric:

```yaml
required: true
```

versus:

```yaml
required: false
```

The tests MUST verify that:

- Missing required data causes the configured failure behavior.
- Missing optional data does not necessarily fail the scrape.
- A malformed document is still a decode error even when optional fields are allowed to be missing.
- A present-but-invalid value remains a transformation/validation error and is not silently treated as missing.

## 34.20 Auto-detection tests

Test `format: auto` against representative content types and bodies:

- JSON.
- YAML.
- XML.
- CSV.
- HTML.
- Prometheus exposition.
- Plain text.
- Ambiguous `text/plain`.
- Incorrect/missing Content-Type.
- Unsupported Content-Type.

Verify that explicit format always takes precedence where configured.

## 34.21 Cardinality and resource-limit tests

Test every configured protection, including as applicable:

- Maximum response bytes.
- Maximum metrics per scrape.
- Maximum labels per metric.
- Maximum label value length.
- Maximum metric name length.
- Maximum help length.
- Python execution time.
- jq/yq execution limits if implemented.
- Maximum concurrent probes if implemented.

Test boundary values exactly at the limit and one beyond the limit.

A limit violation MUST be reported through the appropriate self-metrics and MUST NOT crash the exporter.

## 34.22 Metric exposition tests

Every supported decoder/transform combination MUST have end-to-end tests that verify the final `/probe` response is valid Prometheus exposition.

Test:

- Correct metric type.
- Correct metric names.
- Correct label escaping.
- Correct numeric formatting.
- HELP/TYPE output where required.
- Multiple metrics.
- Histograms and summaries where supported.
- A histogram's `+Inf` bucket written once, including when the decoded histogram
  already carries it.
- No malformed exposition after transformation.

The test suite SHOULD parse the generated exposition with a Prometheus parser rather than relying only on string comparisons.

Golden-output tests MAY be used in addition to semantic parsing tests.

## 34.23 Self-metrics tests

Verify exporter self-metrics for:

- Probe count.
- Probe duration.
- Probe success/failure.
- HTTP status.
- Response bytes.
- Decode success/failure.
- Transformation success/failure.
- Missing-key events.
- Python execution errors.
- Series/limit violations.
- Collector configuration validity.

Verify that every self-metric family is published with a description of its own:
none is empty, none repeats another, none is the old shared placeholder, the
descriptor list and what the endpoint actually exposes do not drift apart in
either direction, and the self-metrics delivered over OTLP carry the same text
as the exposition.

Verify labels such as `collector` and `target` are present only where appropriate and do not create uncontrolled cardinality.

## 34.24 Logging tests

Test that logs:

- Include collector context on relevant errors.
- Include target context where safe.
- Distinguish HTTP, decode, transformation, and validation errors.
- Never expose authentication secrets.
- Do not dump entire potentially sensitive response bodies by default.
- Respect configured log level, including for lines written through the default
  logger.
- Are JSON on every line: a metric rule failing under `error_mode: log` — which
  reports from inside a transform rather than through a passed-down logger —
  produces a JSON object carrying the time, level, message, the failing rule's
  name and its collector.
- Name the collector even when two collectors declare the same metric name, and
  degrade to an empty name rather than panicking when no collector is available.
- Are silent for the other error modes, so `ignore` really does stop the noise.
- Report the watch interval in the startup line when the watch is enabled, and
  omit it when it is not.

## 34.25 Concurrency and race tests

The exporter MUST support concurrent probes safely.

Test:

- Many simultaneous probes to one target.
- Many simultaneous probes to many targets.
- Simultaneous probes while configuration reload occurs.
- Simultaneous probes for different collectors.
- Python transform execution concurrently.
- jq/yq execution concurrently.

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

- SSRF protections according to the documented target policy.
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
- `/metrics` exposes exporter metrics.
- `/probe` is reachable.
- Default configuration loads.
- Python transform execution is available.
- Every documented bundled Python library imports successfully.
- No runtime package installation is required.
- Expected filesystem permissions are respected.
- The image runs as the configured non-root user when non-root mode is enabled.

The image build MUST be reproducible and MUST pin dependency versions sufficiently for production use. A test MUST check the Dockerfile for the image contents §31 requires: no BeautifulSoup, pip removed, Debian updates applied, and `LXML_VERSION` at or above 6.1.0. The Dockerfile MUST expose the Go base version, Python base version, and each bundled Python dependency version as `ARG` variables with documented defaults, so builds can override them without editing the Dockerfile.

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

- Go tests, race tests, vet, formatting, and build run when Go source or Go
  module files change (`**/*.go`, `go.mod`, or `go.sum`).
- Helm lint and template scenarios run when chart files change (`charts/**`).
- A documentation-only or unrelated change MAY complete without running either
  suite.

The path filter MUST evaluate the correct comparison base for both push and
pull-request workflows.

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

## 34.35 Scheduled target tests

Required:

- A target document is rejected unless OTLP export is enabled and an endpoint is
  configured, and the exporter exits non-zero with that error at startup.
- A target naming an unconfigured collector is rejected.
- Document validation rejects a missing collector or target, a relative target
  URL, an invalid or duplicate target name, an unsupported method, a negative
  timeout or retry setting, conflicting credential sources, and an invalid
  label name; unknown fields are rejected.
- Every request parameter round-trips from the document into the per-scrape
  overrides, the request headers, and the cache key.
- Target credentials and headers reach the target request.
- Metrics are grouped by the target's OTLP resource, per-target resource
  attributes merge over the exporter-wide ones, and an absent per-target service
  name falls back to the exporter default.
- Target labels are applied to exported metrics without overwriting labels the
  collector extracted, and without mutating the cached metric set.
- A scheduled scrape reuses the collector cache, and the reused scrape is
  counted in the existing per-collector self-metrics.
- A failed scrape exports a zero health metric and no collector metrics.
- A failed fetch, HTTP status, decode or transform follows the collector's
  `error_handling`: under `fail` the target is down and the failure logged at
  error level; under `log` it is up, with no collector metrics, a success
  counted and one warning; under `ignore` the same with nothing logged at info
  or above. A probe of the same target agrees on whether it succeeded, a
  metric rule with `error_mode: fail` fails both whatever
  `on_transform_error` says, and a target carrying on is never logged as
  recovered.
- The delivered OTLP payload contains one `resourceMetrics` entry per distinct
  resource.
- A configuration reload that would disable OTLP while targets are loaded is
  rejected, and an invalid target reload keeps the previous document.

## 34.36 Collector cache tests

Required:

- A repeated identical probe is served from the cache and the target receives
  exactly one request.
- A collector without `cache` configured never caches; every probe reaches the
  target.
- An expired entry is not served and the next probe refetches.
- The cache key changes when the target, the collector definition, any probe
  parameter, any forwarded header, or the forwarded credential changes, and
  when any of those is removed. Absence and presence MUST produce different
  keys.
- A probe carrying no credential never reads an entry stored by a probe that
  carried one, and probes carrying different credentials never share an entry.
- A configuration reload retires entries cached under the previous collector
  definition.
- A collector's fingerprint is the same whether worked out afresh or
  remembered; it is remembered per configuration and worked out afresh for
  the next one, a probe holding the previous configuration still gets its
  own, a collector outside the configuration is never remembered, concurrent
  probes over two configurations get the right fingerprint, and the key a
  probe files its result under is the one `probeCacheKey` gives.
- Failed probes are not cached.
- Entries beyond `limits.max_cache_entries` are evicted, expired entries first.
- A cached metric set is copied on store and on read, so neither the producer
  nor a reader can mutate the stored entry.
- Concurrent probes of a cached collector are race-free under `go test -race`.
- Cache hit, miss, and entry self-metrics are exposed per collector.
- `cache` parses `ttl` and `stale_if_error`, alone or together; a single
  duration is refused with the mapping form in the message; an unknown key, a
  list, a bad duration and negative values are rejected.
- An entry is fresh within `ttl`, served only to a failed trip within
  `stale_if_error` after it, then dropped; `ttl` 0 with `stale_if_error` keeps
  a fallback without answering from the cache; neither stores nothing.
- A failed trip — a `503`, and a page without the value under `error_mode:
  fail` — within `stale_if_error` is answered `200` with the last good value,
  `http_exporter_result_stale` 1 and the entry's age; it is counted in
  `http_exporter_cache_stale_served_total` and not as a success. With `ttl` 0
  every probe reaches the target. A recovered target answers its new value
  with `http_exporter_result_stale` 0; past `stale_if_error` the failure is
  answered `502`.
- With a `ttl`, a fresh hit carries its age and does not reach the target;
  past the `ttl` a failed trip is answered stale.
- Without `stale_if_error` the gauges are not added and a failure is answered.
- A rule producing `http_exporter_result_stale` fails validation with
  `stale_if_error` and is allowed without it.
- A scheduled target whose scrape fails exports the last good result marked
  stale, with `http_exporter_target_up` 0, and the stale export is counted.
- Configuration parsing accepts durations such as `90s` and rejects a negative
  `cache` value.

## 34.37 Dependency update tests

Required:

- A version selection never crosses a major, for every pin in the table.
- A selection keeps the pin's granularity: a two-component pin is never
  replaced by a three-component version, and the reverse.
- Pre-release, release-candidate and development versions are never selected;
  PEP 440 post-releases are ordered correctly.
- Image candidates are restricted to the exact tag form the build pulls, so a
  version published only under a different variant is not proposed.
- PyPI releases whose files have all been yanked, and releases with no files,
  are not proposed.
- Rewriting a build argument leaves the interpolations and the stage-local
  declarations untouched, and fails rather than doing nothing when the argument
  is absent or declared more than once.
- Every version the Dockerfile pins is covered by the resolver's table, and the
  table names no argument the Dockerfile has stopped declaring.
- A source that cannot be reached is reported as an error rather than as
  "nothing to update".
- Tag listings are followed across pages.

Test environment variable expansion:

- References are literal without the flag and substituted with it, in both the
  configuration and the scheduled target document.
- Only `${NAME}` is a reference: `$NAME`, a trailing `$`, an unterminated `${`
  and a name that does not match the spelling are all left alone, and `$$`
  produces a literal dollar.
- An unset variable is an error naming every missing variable and the document;
  a variable set to an empty string substitutes normally.
- A value containing a line break is refused.
- A reload keeps expanding, a reload does not start expanding when startup did
  not, and a reload with an unresolvable reference leaves the previous
  configuration active.
- Expansion precedes parsing, so a reference can supply a non-scalar part of the
  document.
- Every Go version the workflows request satisfies the go directive in
  `go.mod`, as does the version the Dockerfile pins, and the comparison itself
  is covered for versions of differing granularity.
- The golangci-lint version pinned in the Makefile and the one pinned in CI are
  the same.

## 34.38 Path parameter tests

Test, through `/probe` against a target that records the request line it
receives:

- A supplied value is bound, and an unsupplied one with a default takes the
  default; an empty value is treated as unsupplied.
- A value is one escaped segment: `/`, `?`, `#`, spaces and non-ASCII
  characters arrive percent-encoded, and a dot inside a value is left alone.
- A missing value without a default, an empty value without a default, a
  parameter given twice, `.`, `..`, and a `param_` parameter the path does not
  use are each rejected with `400` naming the parameter, and the target is not
  contacted.
- A `path` probe parameter is used as given; a `param_` parameter beside it is
  rejected as unused; a collector without placeholders is unaffected and still
  rejects an unused `param_` parameter.
- An explicit empty default binds nothing.
- Malformed placeholders — unclosed, unprefixed, empty or invalid names, padded
  names, a default containing a brace — are rejected at startup naming the
  collector, and the brace case points at `--config.export-env`; well-formed
  placeholders, repeated placeholders and a stray `}}` are accepted.
- An environment reference in a default is expanded at load with
  `--config.export-env`, keeping the placeholder, and is refused without it.
- Two tenants never share a cache entry, and a repeat of one is served from
  the cache.
- The verbose `url` label carries the placeholder and never the value.
- A scheduled target rejects placeholders in its own path, rejects a collector
  placeholder without a default unless it sets its own path, and binds the
  default otherwise.
- No placeholder token reaches the requested URL, whatever the value.

## 34.39 Configuration check tests

Test `--dry-run` through the real command line, with stdout, stderr and the exit
status captured:

- The shipped example configurations, with and without the example target file,
  pass with exit `0`, and the report lists their collectors and targets.
- Only the steps that apply are reported: no `targets` step without a target
  file and no `config_watch` step without `--config.watch`.
- An invalid configuration and a missing one fail with exit `1` and name the
  fault; the Python step is then `skipped` with a reason.
- Two faulty pre-scripts are reported as two errors, each naming its collector.
- A missing interpreter fails a configuration with scripts and does not affect
  one without.
- A target file invalid on its own fails; a valid one that does not match the
  configuration fails naming the mismatch; a valid one beside a broken
  configuration is `skipped` and still lists its targets.
- A non-positive watch interval fails only when the watch is on.
- An unset variable fails the check under `--config.export-env` and not
  without it.
- For every failing case, startup with the same arguments also exits `1`.
- stdout carries exactly one JSON document and every stderr line is JSON; each
  step is logged at its level; `--log.level=error` silences the log but not the
  report.
- An unbindable `--web.listen-address` does not affect the check.
- An unparseable command line exits `2` with a JSON log line and nothing on
  stdout, and `-h` exits `0` with usage on stdout.

## 34.40 Request type tests

- A collector without `request.type` is rejected, and the message names the
  collector, says the type is required, lists the supported types and shows
  `type: http`; `--dry-run` reports it as a failed configuration.
- Unknown types — including the anticipated `grpc` and `ftpfile` — are
  rejected with the supported types listed.
- The type is matched case-insensitively and stored in lower case.
- An `http` collector with only `type` is valid and defaults `method` to GET;
  one setting every `http` key together is valid; the `http` cross-key rules
  (method, credential exclusivity, retries, path parameters) still apply.
- Every request key and every scheduled-target request key is accepted by at
  least one type, and every type has validation and a fetch.
- With a second type registered for the test that accepts only `path`: a
  collector of that type setting `method` is rejected naming the key and type;
  a probe of it is served through that type's fetch; probe parameters that
  belong to `http` are rejected with 400 naming the parameter and the type,
  while `path` and a parameter no type knows are accepted; a scheduled target
  setting `method` for it is rejected and one setting `path` is not.
- An `http` collector accepts every `http` probe parameter together.
- The configuration the Helm chart ships by default is valid.
- Build-time selection: every `internal/fetch/requesttype_<name>.go` carries its selection
  constraint and the list of known types matches the files; the guard's
  constraint names every type; evaluated with the Go toolchain's constraint
  rules, a default build compiles every type and not the guard,
  `select_request_types,request_type_http` compiles http and not the guard, and
  `select_request_types` alone compiles only the guard; a default build
  registers every known type; a known type missing from the build is rejected
  as left out of the build, naming the tag, while an unknown name is not;
  registering a type twice panics; the `--dry-run` report lists the built
  types; `tools/request-type-tags.sh` maps lists to tags, de-duplicates, and
  refuses names that are not types; the Dockerfile builds with its tags.

## 34.41 Target scheme tests

- Bare IPv4, hostname, hostname without port, bracketed IPv6, padded and
  path-carrying targets resolve to `http://`; targets naming `http` or `https`
  keep it.
- Credentials in a scheme-less target reach the request and are redacted when
  the target is shown.
- Rendering a target never panics, including for an empty, unparseable or
  scheme-less value, and never echoes a password it could not redact.
- A collector allowing only `https` rejects a bare target.
- A probe whose target is the bare `host:port` of a running server — what a
  chart-generated monitor sends — is served.

## 34.42 Metrics prefix tests

- Valid prefixes (`grafana`, `vendor_eu`, single letters, mixed case, digits
  after the first character) are accepted; a leading or trailing underscore, a
  double underscore, a leading digit, `-`, `:`, spaces, a newline, non-ASCII
  letters and `_` alone are rejected with a message naming the collector, the
  rule and the separator.
- An unset or empty prefix leaves names unchanged; the key is read from a
  configuration file, where unknown keys are rejected.
- The jq, yq, regex, csv, css, xpath and prometheus transforms, a prometheus
  rename, and a Python transform all export prefixed names.
- On `/probe`, HELP and TYPE lines and a histogram's `_bucket`, `_sum` and
  `_count` series carry the prefix; no unprefixed series leaks; a second
  collector without a prefix is unaffected; the exporter's own metrics are not
  prefixed.
- OTLP export, from a probe and from a scheduled target, carries the prefixed
  names, and the scheduled target's health metrics do not.
- A declared name too long once prefixed, and a prefix leaving no room for a
  name, are rejected at startup; a name exactly at the limit is accepted; a
  prefixed name over the limit fails validation at scrape time.
- `--dry-run` reports an invalid prefix as a failed `config` check.
- Changing the prefix changes the cache key.
- `config.example.yaml` demonstrates the key.

## 34.43 Metric rule validation tests

- Invalid metric names (a hyphen, a space, a leading digit, non-ASCII, a `__`
  prefix) are rejected naming the collector and the rule; valid ones, including
  colons and a leading underscore, pass.
- A jq expression with a syntax error, an undefined function or an undefined
  variable; a jq label or `items` expression; a regex; a regex label naming a
  capture group the regex lacks, by name or by number; a CSS selector and a CSS
  label selector and a CSS `items` selector; an XPath expression and an XPath
  label; a prometheus pattern; and `items` on a transform other than jq, yq and
  css are each rejected naming the collector, the
  rule and the label. Named and numbered captures, `@attribute` labels,
  namespaced XPath and `$root` pass.
- A prometheus transform's invalid `include` and `exclude` patterns, `rename`
  targets, `labels` keys and `rename_labels` targets are rejected.
- `--dry-run` reports an expression that does not compile as a failed `config`
  check.

## 34.44 Error policy vocabulary tests

- `warn` in `error_handling` and in `error_mode` is normalised to `log`, each
  use is recorded as a deprecation naming the collector, the key and the
  replacement, and values are matched case-insensitively.
- Anything else is rejected naming the collector, the key and the accepted
  values.
- `--dry-run` passes with a deprecated spelling, lists it under
  `details.deprecations`, and logs it.
- A rule's `fail` still takes precedence over `on_transform_error` of `ignore`,
  `log` and `warn`.

## 34.45 Items tests

- Per item, the value and each label are evaluated against that item; `$root`
  reaches the rest of the document; a label that yields nothing for one item is
  absent on that series only, where parallel streams would have shifted it.
- The same works with `yq`.
- A value missing or null for one item drops that series under `log`, fails the
  scrape naming the item under `fail`, and is skipped silently when not required.
- A value or label expression that yields two values for one item is an error.
- `items` selecting nothing is a missing metric when required and nothing
  otherwise.
- Without `items`, `$root` is the whole document.
- With `css`, `items` selecting table rows gives one series per row, its value
  and labels read from that row's cells; a header row is left out with
  `:has(td)`, a row without the value cell is that row's missing metric, and a
  selector matching two elements in one row is an error.

## 34.46 Label truncation tests

- Truncation cuts to the byte limit, counts the mark within it, never splits a
  character, and drops the mark when the limit is smaller than the mark.
- Only a label with `truncate: true` is cut; another label over the limit still
  fails validation; short values are untouched.
- Truncation works with `metrics_prefix` and with transforms other than jq.

## 34.47 Python worker tests

- Sequential runs of one collector reuse one interpreter.
- A global from one run is not visible to the next, and module state one
  collector leaves is not visible to another.
- A script that overruns fails naming the timeout, promptly, and the next run
  succeeds in a new worker.
- A cold start with a 25 ms `script_timeout` succeeds, since start-up is not
  counted.
- Printing JSON to stdout, writing to stderr and reading stdin do not corrupt
  the protocol.
- `raise`, `fail(...)` and `sys.exit` fail the run with the Python error and
  leave the worker in service.
- A worker that exits is replaced on the next run.
- An answer over `max_output_bytes` fails with the output limit error, and the
  next run succeeds.
- `socket`, `subprocess` and `threading` imports, `open`, `os.system`, and
  reading or writing the protocol descriptors are refused on every run.
- A declared library is preloaded, including one that imports a blocked module.
- Expired idle workers are stopped; pools are keyed by script; a burst of
  concurrent runs succeeds and leaves at most the idle limit.

## 34.48 Expression cache tests

- A program compiled at load is the one scrapes run.
- The cache is bounded and does not keep failed compiles.
- XPath keys include namespace bindings.
- One program serves concurrent evaluations.

## 34.49 Configuration schema tests

See § 24.3. In addition, the README's command-line table lists exactly the flags
the exporter has.

## 34.50 Shared probe tests

- Five concurrent identical probes make one request to the target and get
  identical answers; the self-metrics show five scrapes, five successes, one
  decode and four coalesced probes; nothing is left in flight.
- Two probes that do not overlap make two requests.
- Probes to a different target, with a different probe parameter, or with a
  different forwarded `Authorization` header each make their own request.
- A target failure reaches every waiting probe as the same `502`, is logged
  once, and counts as a failure; a rule's `error_mode: fail` JSON body reaches
  every waiting probe.
- The first probe's client going away neither fails the others nor cancels the
  request.
- Every waiting probe going away cancels the request, and a later probe starts
  afresh and succeeds.
- `coalesce: false` makes one request per probe.
- With a cache, concurrent probes make one request and fill the cache, and the
  next probe is a cache hit.
- A panic in the shared work answers with `500` and leaves nothing in flight.

## 34.51 Verbose collector metric tests

See § 22.1a.

- The histogram's buckets are the ones listed, cumulative, with `_sum` and
  `_count`; it is published when verbose, over `/metrics` and over OTLP, and not
  published or recorded when verbose is off.
- A probe to the target is observed; a cache hit and a coalesced probe are not;
  a scheduled scrape is.
- A Python collector's runs are counted by outcome — `ok`, `script_error`,
  `timeout` — and a timed-out or crashed worker is counted as a stop with that
  reason; worker states read idle after the runs; starts are counted.
- A worker that cannot start is counted as a start failure.
- A collector without Python has no Python series.
- The pool families are published in verbose mode with no Python collector, at
  zero, with every state, reason and outcome, and are absent without verbose.
- The pool values are the per-collector values summed, and keep counting the
  runs of a collector no longer configured.
- Every family has one `HELP` and one `TYPE` line, and every family added here
  is absent without verbose mode.
- No family added here reuses the name of a scheduled target health series
  (§ 42.14), which travel over OTLP beside them.
- A histogram passed through from a Prometheus target is written with exactly
  one `le="+Inf"` bucket.

## 34.52 Collector file tests

See § 5.0.

- The configuration's collectors, a named file and a pattern's files merge in
  the defined order, and their sources and the files read are recorded; merged
  collectors get the configuration's defaults.
- A configuration made only of collector files loads.
- Relative entries resolve against the configuration's directory; absolute
  entries are used as given.
- A collector file with `web`, `otlp`, a nested `collector_files` or a misspelt
  key, an empty file, a comment-only file, an empty list, a list at the top, an
  unknown collector key and invalid YAML are each refused with the file named.
- An invalid expression and an invalid collector name in a collector file are
  refused as in the configuration.
- A duplicate name is refused between the configuration and a file, between two
  files, between two matches of one pattern, within one file and within the
  configuration, naming both places.
- A file matched twice is read once, and `*.yaml` beside the configuration does
  not read the configuration itself.
- A missing file, a directory, an empty entry and a malformed pattern are
  refused; a pattern matching nothing is accepted unless nothing then defines
  a collector.
- `${NAME}` references in a collector file are expanded, and an unset one is
  refused naming the file.
- Startup and `--dry-run` both refuse a duplicate across files, and the dry run
  lists the collector files it read.
- The watch reloads when a collector file is edited, added or removed, does not
  reload when nothing changed, and rejects a reload that adds a duplicate,
  keeping the configuration in force.
- `collector-file.schema.json` is current, printed by its flag, describes
  collectors as the configuration schema does, accepts a collectors list and
  rejects any other key, an empty list and an invalid collector; the
  configuration schema accepts a configuration of collector files alone and
  rejects one with neither key, an empty `collector_files` and an empty entry.

## 34.53 Local file request type tests

See § 5.1, `localfile`.

- `root` is required, absolute, cleaned and not `/`; `path` is relative and
  stays under `root`; `max_age` and `max_response_bytes` are not negative;
  path parameter syntax is checked; `http` keys on a `localfile` collector and
  `root` or `max_age` on an `http` collector are rejected naming the key and the
  type. The configuration schema requires `root` for `localfile`.
- A probe with no target reads `request.path`. A target names a file or a
  directory the path is read in, relative, absolute inside `root`, or as a
  `file://` URL; a `path` probe parameter replaces `request.path`.
- A target leading outside `root`, and a `file://` URL without an absolute
  path, are refused with `400`; a `path` parameter leading outside or absolute,
  and neither a target nor a path, fail the scrape.
- A path parameter fills one name; one with `/` is refused, `..` is a `400`, an
  unused one is a `400`, and the default applies when none is given.
- `http` probe parameters are refused with `400`; `timeout` is accepted; an
  `http` probe without a target is still refused.
- A relative link inside `root` is followed; a link leading outside, an
  absolute link and a relative link climbing out are refused. A directory, a
  named pipe (without hanging), a missing file and an unreadable file each fail
  with their own message.
- A file over the size limit fails and counts as a limit error.
- A file older than `max_age` fails naming the limit; a fresh one is read.
- A file changed once during the read is read again and the new content served;
  one that keeps changing fails advising an atomic rename.
- A read that does not return is abandoned when the probe's timeout ends.
- With four reads of one collector stuck, a fifth probe fails at once; another
  collector is unaffected; the slots come back when the reads return and the
  next probe succeeds.
- The response carries status `200`, the body, `Content-Type` by extension,
  `Content-Length` and `Last-Modified`.
- Verbose series carry the `file://` URL with placeholders and `READ`.
- Scheduled targets with and without a target are scraped and exported over
  OTLP; a target outside `root`, an `http` key, a credential and a placeholder
  path are refused at load.
- The examples in `docs/LOCALFILE.md` load and serve as documented.
- The shipped example configurations, target file and schema include a
  `localfile` collector and target.

## 34.53a Directory read tests

See § 5.1a.

- `files` gets the defaults; `files` with `path`, a pattern with `/`, a
  malformed or empty pattern, a negative `max_files` or `max_total_bytes`,
  either without `files`, and `files` on an `http` collector are rejected.
- Every matching file is read, with `file` labels, mtimes and scrape errors of
  0, and families declared once; non-matching, hidden and nested files are not
  read; a pattern starting with a dot reads hidden files.
- A malformed file, one with a `file` label, one using a reserved name and one
  whose metric type conflicts with an earlier file each fail alone with a scrape
  error of 1 and a mtime, logged with their stage; the probe succeeds; the
  failure is counted.
- Text files with a regex and JSON files with jq are read; a rule with
  `error_mode: fail` fails its file only.
- With more files than `max_files`, the first by name are read and the rest
  skipped, counted and logged.
- A file over the per-file limit, and one past `max_total_bytes`, are refused
  without being read, while a later file that fits is read; both are counted
  as limit errors and logged.
- A file older than `max_age` fails alone, with its mtime.
- A target names a subdirectory; an empty directory answers with no files; a
  missing directory or a file as target fails the probe; a target outside
  `root`, `path` and `param_<name>` are `400`.
- A link inside `root` is followed, one outside fails its file, and a named pipe
  fails its file without hanging.
- The verbose `url` is the directory's `file://` URL.
- A scheduled target reads a directory; one setting `request.path` is refused.
- The documented example loads and reads a directory.

## 34.53b Build, size, shutdown, OTLP buffer and directory read tests

See § 5.0a, § 5.1a, § 22.0d, § 23 and § 42.1a.

- `--version` prints the version, revision, Go version and request types;
  `-X main.version` wins over the stamped version; `http_exporter_build_info`
  carries the same as a gauge of 1.
- Sizes with every unit, a space, a fraction and none are read; an empty,
  unit-only, negative, exponent, unknown-unit, trailing-garbage and overflowing
  size is refused; YAML integers and quoted numbers are read; a list is not.
  Each byte setting takes a unit in a loaded configuration, a malformed one
  fails naming it, and the schema carries the size pattern.
- A second `SIGINT` during a shutdown held up by a probe in progress ends the
  process within two seconds, killed by the signal, while the first alone did
  not end it.
- Past `otlp.max_pending_points` the points kept from a failed export go
  before those queued since, down to nine tenths, counted and logged;
  replacing a waiting series is not a new point; draining resets the count.
  Negative `max_pending_points` and `unready_after_failures` are rejected.
- Failing exports leave `/ready` at 200 by default; with
  `unready_after_failures` 2 it is 503 after two and 200 after one gets
  through; a reload to another endpoint makes it ready at once.
- A directory whose file is held past the probe's timeout answers the other
  files, and that file fails with its mtime.
- Files of a directory are read at most four at a time and more than one at
  once, and answered in name order.
- A listing past its bound is logged with how many entries were listed.
- A file that grows during its read past its share of `max_total_bytes` fails
  alone, saying it grew.

## 34.53c Encoding, shutdown timeout and credential file tests

See § 6.1a, § 30 and § 42.5.

- A Latin-1 body declared `iso-8859-1`, `windows-1252` or `latin1` gives
  `café`; declared UTF-8 is left alone; `response.charset: windows-1251`
  converts an undeclared or wrongly declared Cyrillic body; an unknown
  `response.charset` fails to load and an unknown declared charset fails the
  decode.
- UTF-8, UTF-16LE and UTF-16BE byte order marks win over a wrong declaration
  and are removed.
- HTML with `<meta charset>` or `http-equiv`, and XML with an encoding
  declaration, with and without a header saying the same, decode to `café`
  once; the converted XML says UTF-8 and its `Content-Type` `charset=utf-8`.
- A declared-UTF-8 body with an invalid byte answers 200 with U+FFFD, parses,
  is counted and logged; a shared label map is not changed in place; files of
  a directory are converted and repaired one by one.
- `--web.shutdown-timeout` of 1s ends a shutdown held by a probe within
  seconds, exit 0, logging the timeout; zero and negative values exit 2.
- Credentials from `username_file` and `password_file`, and an inline
  username with a password file, admit the right credential and refuse wrong
  ones; a rotated password file takes effect and the old password stops
  working; a removed file answers 500; both forms of one field, neither, a
  missing and an empty file are rejected at load; a disabled `basic_auth` reads
  nothing.

## 34.53d UTF-8 name tests

See § 21.1.

- Classic names, including one with `:`, are unchanged; `http.server.duration`,
  `a_b.c`, a leading digit, an accented letter, an emoji and invalid UTF-8
  escape as specified with `underscores` and `values`; `:` in a label name is
  escaped; `fail` changes nothing.
- `name_escaping` defaults to `fail`; `dots` is rejected naming the values.
- Through a probe of a target serving a UTF-8 metric and label name: `fail`
  answers 502 naming the metric and `name_escaping`; `underscores` and `values`
  answer the escaped names with the classic ones unchanged, and the answer
  parses; with `metrics_prefix` the `values` name starts with `U__` and the
  prefix; a UTF-8 label name alone fails by default.
- Two labels escaping to one name fail naming both; two metrics escaping to one
  name fail as duplicate series; a shared label map is not changed in place.

## 34.53e Reload state, probe parameter, server timeout and failure log tests

See § 23, § 24.1a and § 25.1.

- After a reload removes a collector, its self-metric, cache-entry, histogram
  and per-request series are gone and its cache is empty, while a kept
  collector's counters stay; added again, it starts from zero.
- A changed collector keeps its counters, its cache-entry gauge reads 0 at
  once, and the next probe goes to the target with the new definition; an
  unchanged reload keeps the cache.
- A probe without `collector`, with or without a target, and one without a
  required target, answer `400` naming what is missing; an unknown collector
  is named.
- The server's header, read and idle timeouts are 10s, 30s and 2m with no write
  timeout, and an idle keep-alive connection is closed.
- Six failures a minute apart log the first and, at five minutes, one line with
  `repeated` 5 and `failing_since` at the failure's level; another error is
  logged at once; a recovery logs `failed_for` and `failures`, and a success
  without a failure logs nothing; at debug level each repeat is logged marked.
- The remembered failures stop at the bound, a new one past it is not
  remembered, those unseen for an hour make room, and a removed collector's
  are forgotten.
- Four failed probes of a target log one line and its recovery counts four; a
  scheduled target failing three times logs once and its recovery; a file of a
  directory failing on three probes logs once and its recovery.

## 34.53f Request template tests

See § 42.10a and § 42.10b.

- Body placeholders are found among braces of the body's own, with a default
  and a filter; `{{"…` is text; spaces, an unknown filter, an unclosed
  placeholder, a bad name, a brace in a default, and a filter in a header or
  query value are rejected.
- `json` escapes quotes, backslashes, line breaks and keeps non-ASCII; `number`
  accepts a JSON number and refuses `12; DROP` and `0x10`; `form` and `xml`
  escape; `raw` and no filter write the value; a default takes its filter; a
  header value with CR LF is refused.
- A probe fills the path, a header, a query value (its default, and a given
  value with `&` staying one value) and a JSON body; a missing body or path
  parameter, a non-number, a header value with a line break, and an unused
  parameter are `400` without contacting the target; a `body` override replaces
  the template and makes a body-only parameter unused.
- A placeholder in a header or query name, a filter in a header value and a
  malformed body placeholder fail to load.
- Probes differing only in a body parameter are cached apart.
- A scheduled target's `params` fill path, header and body; a missing, unused,
  unfit or badly named parameter fails to load naming it; targets differing only
  in `params` have different cache keys.

## 34.53g Shutdown delay and compression tests

See § 23 and § 30.

- After a `SIGTERM` with `--web.shutdown-delay` of 1s, `/ready` answers `503`
  naming the shutdown while a probe is still answered `200`; the process exits
  `0` after the delay, logging it; a negative value exits 2. Without the delay
  nothing changes: the exporter stops at once.
- A server marked as shutting down answers `/ready` `503` with the reason and
  `/health` `200`.
- `Accept-Encoding` parsing: `gzip`, `x-gzip`, `*`, a list with `gzip;q=0.5`
  and mixed case accept gzip; no header, `identity`, `br`, `gzip;q=0` and
  `gzip;q=0.0` do not.
- `/probe`, `/metrics` and the self-metrics path answer gzip, with
  `Content-Encoding` and `Vary`, when asked, decompressing to exactly the
  uncompressed answer; they are uncompressed without the header, with `q=0`
  and for `HEAD`; an error answer is compressed too; `/health` and `/ready` are
  never compressed.

## 34.53h Source layout tests

See § 7.1.

- Every package under `internal/` imports only the packages before it in the
  layer order, `internal/testutil` imports only `internal/model`, and no
  non-test file imports `internal/testutil`.
- Only the shared trip fetches, decodes and transforms, apart from a
  directory's files (§ 5.1), which it decodes and transforms one at a time.
- A scheduled target's scrape failed by a metric rule with `error_mode: fail`
  is logged with stage `metric` and the metric's name, as a probe's is.

## 34.54 Probe deadline tests

See § 3.2a.

- The budget is the header less the offset; a missing, unparseable, zero,
  negative, NaN or infinite header gives none; an offset of half or more leaves
  half; an offset of zero leaves the whole timeout.
- A target slower than the budget gets a `502` naming the budget and the flag,
  about the budget's length after the probe started.
- The budget stops a hung file read; without the header, and with a generous
  one, the probe succeeds.
- A negative `--probe.timeout-offset` exits 2 with a JSON log line and nothing
  on stdout, also with `--dry-run`.

## 34.55 Self-metric exposition and reload status tests

See § 22.0a and § 22.0b.

- On a verbose exporter with resource metrics and Python, the self-metrics
  parse; each family is declared once and its samples are contiguous; every
  counter ends in `_total` and nothing else does; each per-collector family has
  the type its definition gives it; and every family in the OTLP set has the
  type the text declares.
- At startup the configuration reports successful with a timestamp and no
  reloads; a rejected reload reads 0, counts a failure and keeps the timestamp;
  a later successful reload reads 1, counts a success and moves the timestamp.
  No `targets` series appears without a target file.
- With a target file, a rejected target reload reports under `file="targets"`
  and leaves `file="config"` successful.
- The rules of § 19 use `on_fetch_error` for both request types.

## 34.56 Connection reuse, worker lifecycle, script duration and error kind tests

See § 42.15b, § 16 and § 19.1.

- Five probes of one target open one connection, over HTTP and over HTTPS with
  a CA file; three OTLP exports open one connection.
- The same settings share a pool; `insecure_skip_verify` and HTTP/2 get their
  own; a rotated CA file gets a new pool; a missing one is an error; a pool
  unused for five minutes is dropped; idle connections time out.
- `http_exporter_script_duration_seconds` reports at least the time a script
  sleeps, on the collector and the request, and 0 for a collector without
  Python; the probe's timer adds its runs up.
- An idle worker aged past the timeout is stopped by the timer alone and
  counted as `idle`.
- A reload that changes one script stops its idle worker at once and its busy
  worker when the run ends, both counted as `reload`, leaves another script's
  idle worker alone, and the changed script's next worker is kept.
- A marked error keeps its message and its kind through wrapping and has no
  other kind; an unmarked error that mentions the words is none of them. A
  Python error saying "missing" and "response size" counts only as a script
  error; a regex that matched nothing counts as a missing value; an oversized
  response counts as a limit error.

## 34.57 OTLP metric type tests

See § 42.1.

- A histogram with a `+Inf` bucket, without one, with buckets out of order,
  with a count that falls and without buckets is exported with the right
  bounds, per-bucket counts, count, sum and attributes, and one count more than
  bounds.
- A summary with quantiles, and one without, keeps count, sum and quantiles.
- Two series of one counter are two points of one monotonic sum; a histogram
  type without data is a gauge of its value.
- NaN and the infinities encode and decode.
- End to end, a histogram passed through from a scheduled Prometheus target,
  the verbose scrape-time histogram and the GC summary reach an OTLP endpoint
  with their data, and the scrape-time histogram's bucket counts add up to its
  count.

## 34.58 Reload-on-demand tests

See § 24.1a.

- Without `--web.enable-lifecycle`, `POST /-/reload` is `403` naming the flag
  and reloads nothing.
- With it: `GET` is `405` with `Allow: POST, PUT`; `POST` applies a changed
  configuration at once and answers `200`; `PUT` reloads an unchanged one; a
  duplicate collector is `500` naming it, keeping the previous configuration;
  the reload counters count successes and failures.
- The scheduled target file is reloaded too, and its rejection is a `500`
  naming it, keeping the previous targets.
- With `web.basic_auth`, the endpoint needs the credentials.
- `SIGHUP` reloads.
- Reloads from `/-/reload` and the watch at once do not interleave.

## 34.59 Concurrent probe limit tests

See § 42.13b.

- The default is 32; a set limit is used; a negative one is rejected.
- With a limit of 2 and two probes held at the target, a third is `503` at once
  naming `max_concurrent_probes`, never reaches the target, and is counted;
  the in-flight gauge reads 2; the held probes succeed; the slots come back and
  a later probe succeeds.
- With a limit of 1, an identical probe sharing the request in flight, and a
  cache hit while another target holds the slot, are both answered.
- A scheduled target fails in the `concurrency` stage when no slot frees within
  its budget, and succeeds when one frees while it waits.
- Waiting for a slot ends with its context; collectors have limits of their
  own.

## 34.60 OTLP delivery, readiness and proxy tests

See § 22.0c, § 23, § 42.1a and § 42.15b.

- Exports are gzipped by default and decode; with `compression: none` they
  are plain JSON; another value is rejected.
- `429`, `502`, `503` and `504` are retried until the export gets through:
  one success, the retries counted, the last success timestamp set, nothing
  left pending.
- `Retry-After` in seconds and as a date is read, a past or unreadable one is
  ignored, and the backoff doubles up to its cap. Retries end within the
  budget, and a `Retry-After` longer than it ends the export at once.
- An export that runs out of retries is one failure; its probe metrics are
  kept, the self-metric snapshot is not, a newer value queued meanwhile wins,
  and the kept metrics are sent once the endpoint recovers. An unreachable
  endpoint is retried and its data kept.
- A `400` is tried once, its data points dropped and counted.
- The status families exist only with OTLP enabled, and the timestamp is 0
  before the first success.
- The export loop stops when its context ends; an export it cut short is not
  counted and its data is kept; the last export sends it, data queued since,
  and a self-metric snapshot, in one request. Without OTLP there is none.
- `/ready` is `503` after a rejected reload, without quoting the error, and
  `200` after an accepted one; `503` after three failed exports in a row and
  `200` after one gets through; OTLP failures do not count with OTLP disabled.
- With `HTTP_PROXY` set, a probe and an OTLP export go through the proxy, and a
  host in `NO_PROXY` does not.

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
    scheduled-target setups, formats, the node_exporter practices it follows,
    errors and self-metrics, and mounting files in Kubernetes

The Python documentation MUST explicitly state that networking is owned by the exporter and that `requests`/`httpx` are unnecessary.

The documentation MUST be split by audience rather than gathered in one file.
The repository README is what someone reads before deciding to use the exporter
and while starting it for the first time, so it MUST carry only what serves
that: what the exporter is, how the probe pattern works, a minimal configuration
and the commands to run it, the endpoints, the command-line flags, and an index
of everything else. It MUST NOT carry reference material — decoder and transform
detail, caching, transport settings, authentication models, operator manifests —
because a README that answers every question stops answering the first one.

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
- Database connectors
- SNMP
- Kafka
- gRPC/Protobuf decoding
- GraphQL client logic
- Long-running background jobs per target
- Scheduled scraping outside Prometheus

The architecture should leave room for future decoders, but the initial implementation should stay focused on HTTP-to-Prometheus conversion.

---

# 38. Recommended implementation order

Implement in this order:

1. Go HTTP server and `/probe`, `/metrics`, `/health`, `/ready`.
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
                 |  HTTP Fetcher |
                 +-------+-------+
                         |
                         v
                   HTTP Response
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

The central invariant is:

> **Any supported response format must be convertible into the same internal metric representation, after which validation, limits, self-observability, and Prometheus exposition are format-independent.**

# 42. Dedicated self-health endpoint, OTLP, and deployment controls

The exporter MUST expose exporter self-health metrics separately from target
probe output. The dedicated endpoint MUST be configurable, for example:

```text
--web.self-metrics-path=/self-metrics
```

The default path SHOULD be `/self-metrics`. `/metrics` MAY remain as a
backwards-compatible alias, but the dedicated path is the canonical endpoint.
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

`otlp.interval` MUST control the export cadence and SHOULD default to 30
seconds. The exporter MUST buffer the latest general metric value for each
metric/label set between exports and include an exporter self-health snapshot
in each interval export. `otlp.timeout` MUST bound each export request and
SHOULD default to 5 seconds.

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
- A histogram MUST be an OTLP histogram with cumulative temporality, its count
  and sum, and its buckets converted from Prometheus's cumulative counts to
  OTLP's per-bucket counts: the finite upper bounds, in ascending order, as
  explicit bounds, and one count more than bounds, the last being everything
  above the highest bound — Prometheus's `+Inf` bucket, which MUST NOT appear
  as a bound. A cumulative count that falls MUST count as an empty bucket rather
  than wrap around.
- A summary MUST be an OTLP summary with its count, sum and quantiles, and a
  summary without quantiles, such as `go_gc_duration_seconds` (§ 22.0a), MUST
  still carry its count and sum.
- A metric declared a histogram or summary but carrying no such data MUST be
  exported as a gauge of its value.
- Every series of a family MUST be a data point of one OTLP metric, not a
  metric of its own.
- NaN and the infinities MUST be encoded as the protobuf JSON mapping writes
  them — `"NaN"`, `"Infinity"`, `"-Infinity"` — since a JSON number cannot
  express them, and one such value MUST NOT fail the encoding of an export.

### 42.1a Delivery

Export requests MUST be gzipped, with `Content-Encoding: gzip`, unless
`otlp.compression` is `none`; `gzip` MUST be the default, and any other value
MUST be rejected when the configuration loads.

An export that fails with a network error, `429`, `502`, `503` or `504` — the
responses the OTLP/HTTP specification makes retryable — MUST be retried with
exponential backoff, 1 second doubling up to 16, or after the delay a
`Retry-After` header gives in seconds or as an HTTP date. `otlp.timeout` MUST
bound each attempt. Retries MUST stop when another attempt could not start
within `otlp.interval` of the export's start, so one export never runs into the
next, and MUST stop at once when the exporter shuts down.

When retries run out, the data points of the export MUST be kept for the next
export, except where a newer value of the same series has been queued since;
the self-metric snapshot MUST NOT be kept, since the next export takes a new
one. Any other non-2xx response MUST NOT be retried, and the export's data
points MUST be dropped rather than kept, since the endpoint would refuse them
again. Every failed export MUST be logged as a warning with the retries made,
and counted (§ 22.0c).

On `SIGTERM` or `SIGINT`, the exporter MUST stop accepting requests, let the
probes in progress finish, stop the export loop, and then make one last
export, bounded by `otlp.timeout`, of everything pending — including the data
of an export the shutdown cut short — with a last self-metric snapshot, before
it exits. Scheduled targets MUST NOT be scraped again for it. The shutdown MUST
be logged, and once it has begun a second `SIGTERM` or `SIGINT` MUST end the
process at once, without waiting for the probes or the last export.

The data points waiting for export MUST be bounded by `otlp.max_pending_points`,
100000 by default, a negative value being rejected. Past it, the oldest MUST be
dropped — points kept from failed exports before any queued since, and among
those the older first — down to nine tenths of the limit, so that dropping is
not repeated on every point; they MUST be counted (§ 22.0c) and logged as a
warning. Replacing the value of a series already waiting MUST NOT count as a
new point.

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
   `request.forward_headers`. The chart value that produces those parameters
   is specified in [SPECIFICATION-CHART.md](SPECIFICATION-CHART.md) § 42.4.
4. The exporter MUST reject or ignore forwarding of transport and hop-by-hop
   headers, including `Host`, `Connection`, `Content-Length`, `Proxy-*`,
   `Transfer-Encoding`, `Trailer`, `Upgrade`, and `TE`. `Authorization` MUST
   be controlled only by `request.forward_authorization`, not by the generic
   header allowlist.
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

The username MAY instead be read from `username_file` and the password from
`password_file`, for a credential mounted from a Secret rather than written in
the configuration; exactly one of `username` and `username_file`, and one of
`password` and `password_file`, MUST be set. A file's content MUST be trimmed
of surrounding whitespace. A file that is missing, unreadable or empty when
the configuration loads MUST reject it. A file MUST be read again when its
size or modification time changes, so a rotated Secret takes effect without a
restart or reload; one that can no longer be read MUST make every protected
request fail with `500`, logged, rather than admit it. The username and
password MUST both be compared in constant time whatever the first
comparison found.

When enabled, the exporter MUST require valid Basic Authentication for
`/probe`, `/metrics`, and the configured self-health metrics endpoint. The
`/health` and `/ready` endpoints SHOULD remain unauthenticated so Kubernetes
liveness and readiness probes can operate without credentials.

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
missing or empty credentials.

Exporter credentials and target credentials MUST remain independent. The
incoming monitor's Basic Auth may authenticate the exporter, while the
exporter's configured bearer or basic credentials authenticate the underlying target. The
Authorization bridge MUST remain disabled in this mode, and configuration
validation MUST reject a collector that enables it alongside exporter Basic
Authentication.

Tests MUST cover bearer-token-file loading, basic-auth-file loading, whitespace
trimming, missing and empty files, independent exporter Basic Auth, and the
fact that the exporter Basic Auth credential is not forwarded to the target.

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
when explicitly requested. The exporter MUST retain TLS 1.2 or newer and MUST
not log certificate contents or credentials. The OTLP HTTP client MUST use the
same configured timeout and best-effort failure behavior as other OTLP exports.

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
and the message MUST say to run with `--config.export-env`. A literal `}}` with
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
value. The values `.` and `..` MUST be rejected with `400`, since escaping cannot
make them safe and a server resolving them would serve a different path.

The probe MUST also be rejected with `400` when a `param_` parameter is given
more than once, or when the collector's path does not use it. The latter is
almost always a misspelling, and with a default in place a misspelled parameter
would otherwise succeed against the default and report one tenant's data as
another's.

Path parameters MUST be bound only in the collector's `request.path`. A `path`
probe parameter replaces that path and MUST be used as given, and a `param_`
parameter sent with it is unused and therefore rejected.

Path parameters and environment references (§ 42.15a) MUST NOT overlap
syntactically. Environment references are expanded once, textually, when the
file is read; path parameters are bound on every probe. They MUST compose, so a
default may be an environment reference expanded at load time:
`{{param_tenant:${DEFAULT_TENANT}}}`.

Every probe parameter is part of the response cache key (§ 42.13), so two probes
differing only in a path parameter MUST NOT share a cache entry.

The verbose self-metrics `url` label (§ 22.1) MUST show a bound placeholder as
written, not its value. A value is typically a tenant or an account, which the
label already keeps out of the query string, and one series per value would be
unbounded.

A scheduled target (§ 42.14) has no probe to supply a value; it MAY give
values under `params`, a map of `param_<name>` names to values, which fill the
collector's placeholders as the probe parameters of the same names would. Its
own `request.path`, `body` and `headers` MUST NOT contain placeholders. Every
placeholder of the collector's request — path, body, header and query values —
MUST be filled by `params` or a default, and every entry of `params` MUST fill
one and be a valid name; any other combination MUST be rejected at startup with
a message naming the target, the collector and the parameter. `params` MUST be
part of the target's cache key.

## 42.10b Placeholders in the body, headers and query

An `http` collector's `request.body`, the values of `request.headers` and the
values of `request.query` MAY contain the placeholders of § 42.10a, filled by
the same probe parameters with the same defaults; a missing value, one given
twice and one no placeholder of the request uses MUST be refused with `400`
before the target is contacted. In these fields `{{` MUST open a placeholder
only when `param_` follows it, and `{{` followed by spaces and `param_` MUST be
rejected at load. A header name or query name containing `{{` MUST be rejected
at load. When the `body` probe parameter replaces the body, its placeholders
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
anywhere. The values MUST NOT reach any self-metric label.

## 42.12 CI, container, and chart releases

CI MUST materialize Go module checksums before running tests and MUST run the
test, race, vet, build, and Helm validation checks.

The exporter and the Helm chart MUST be released on independent cycles, from
separate tags and separate workflows. Releasing one MUST NOT publish the other,
so a chart fix does not require an exporter release and an exporter release does
not republish an unchanged chart.

Release tags MUST be namespaced by what they release: `exporter/<name>-vX.Y.Z`
for the exporter and `chart/<chart-name>-X.Y.Z` for the chart.

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
- publish versioned and `latest` container tags to GHCR;
- build release binaries for the documented target platforms; and
- create a GitHub Release containing the software archives.

The chart release workflow is specified in
[SPECIFICATION-CHART.md](SPECIFICATION-CHART.md) § 42.12.

Both release workflows MUST use `GITHUB_TOKEN` with `contents: write` and
`packages: write` permissions and MUST run their respective validation checks
before publishing artifacts.

The repository's tests MUST cover the release tag contract: that each release
workflow triggers on its whole namespace, that the format it enforces accepts
well-formed tags and rejects malformed ones, and that the version committed in
`Chart.yaml` would produce a tag the chart release accepts.

## 42.13 Collector response caching

Each collector MUST support a `cache` mapping with two Go durations:

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
- every query parameter of the `/probe` request, including `method`, `path`,
  `timeout`, `body`, `insecure_skip_verify`, `retry_attempts`,
  `retry_backoff`, and any `header_<name>` parameter, with all values; and
- every header forwarded to the target, including a forwarded `Authorization`
  value.

The presence and the absence of a parameter, a header, or a credential MUST
produce different keys. A probe that supplies no credential, no forwarded
header, or no TLS override therefore MUST NOT be able to read an entry stored
by a probe that supplied one, and two probes presenting different credentials
MUST NOT share an entry. This is a confidentiality requirement: a cached result
may only ever be returned to a byte-for-byte identical request.

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

So that a stale answer never passes for a fresh one, while `stale_if_error` is
set every answer of the collector MUST carry two gauges of the exporter's own,
without labels, after the collector's metrics:

```text
http_exporter_result_stale        1 for a stale result standing in for a failed trip, else 0
http_exporter_result_age_seconds  seconds since the answered result was fetched from the target
```

The age MUST be 0 for a result just fetched and the stored entry's age for a
cached answer, fresh or stale. A result in which a rule produced either name
MUST fail validation, since the names would clash; without `stale_if_error`
the names are free and the gauges are not added. They MUST NOT count against
`limits.max_metrics`. Samples of a stale answer carry no timestamps of their
own, so Prometheus stores them at the time of the scrape.

Each stale answer MUST be counted in
`http_exporter_cache_stale_served_total{collector}` and logged at warn level
with the result's age, sparingly like repeated failures (§ 25), with a line
when the collector answers with a fresh result again. Identical probes sharing
a trip (§ 42.13a) MUST share the stale answer. A scheduled target (§ 42.14)
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
entries closest to expiry.

The exporter MUST expose per-collector cache self-metrics:

```text
http_exporter_cache_hits_total
http_exporter_cache_misses_total
http_exporter_cache_entries
http_exporter_cache_stale_served_total
```

A cache hit MUST count as a successful scrape in `http_exporter_scrapes_total`
and `http_exporter_scrape_success_total`, and the cached metrics MUST still be queued
for OTLP export. Target-request self-metrics such as
`http_exporter_scrape_http_status_code` and
`http_exporter_scrape_response_bytes` describe the last real target request and
MUST NOT be altered by a cache hit.

## 42.13a Sharing identical probes in flight

A probe that arrives while an identical probe is already in flight MUST NOT
send its own request to the target. It MUST wait for the one in flight and
answer with an exact copy of its result: status, headers and body, a failure
included. Several Prometheus replicas probing the same target at the same
moment would otherwise each reach it, multiplying the load on a slow or
rate-limited endpoint.

- Identical MUST mean the response cache key (§ 42.13): the collector
  definition, the target, every probe parameter and every forwarded header,
  credentials included. Probes that could get different answers MUST NOT share.
- It MUST work with the response cache off. With it on, the cache is checked
  first, and the shared request fills it once. The probe that starts a shared
  request MUST check the cache again before going to the target, since one that
  just finished may have filled it.
- The shared request's own work MUST be counted and logged once: its HTTP
  status, response bytes, decode, transform, validation and error counters, its
  log lines, its cache entry and its OTLP export. Every probe MUST still count
  in `http_exporter_scrapes_total` and, by the shared outcome,
  `http_exporter_scrape_success_total`, with its own duration. Each probe answered by
  another's request MUST increment `http_exporter_probes_coalesced_total` for
  its collector.
- The shared request MUST run detached from the probe that started it, so that
  probe's client going away MUST NOT fail the others. It MUST be cancelled when
  every probe waiting on it has gone, and a probe arriving after that MUST start
  a new request rather than join the cancelled one.
- A panic in the shared work MUST answer the waiting probes with `500` rather
  than stop the exporter.
- It MUST be on by default, and a collector MUST be able to turn it off with
  `coalesce: false`, for a target that must see every probe as a request.
- Scheduled targets (§ 42.14) are scraped once per interval by the exporter
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
- A scheduled target MUST share its collector's limit, and MUST wait for a slot
  within its scrape budget instead of failing at once; if none frees in time it
  MUST fail in the `concurrency` stage and be counted as rejected.
- A slot MUST be freed when the trip ends, and `http_exporter_probes_in_flight`
  MUST report the trips in progress per collector.

## 42.14 Scheduled targets exported over OTLP

The exporter MAY be started with an optional scheduled target document:

```text
--otlp.targets-file=/etc/prometheus-universal-exporter/targets.yaml
```

The document lists fully specified requests that the exporter scrapes itself:

```yaml
targets:
  - name: legacy_eu
    collector: legacy_text
    target: http://legacy.eu.example:8080
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
    otlp:
      service_name: legacy-app
      resource_attributes:
        deployment.environment: production
```

The feature exists only to deliver metrics over OTLP. The exporter MUST refuse
to start when a target document is supplied while `otlp.enabled` is false or no
`otlp.endpoint` is configured, and MUST report that requirement explicitly
before exiting with a non-zero status. A configuration reload that would put the
exporter into that state while targets are loaded MUST be rejected, and the last
valid configuration MUST remain active. Every target MUST name a configured
collector; an unknown collector MUST be rejected at startup and at reload.
A target MAY set `params`, the values of its collector's placeholders
(§ 42.10a). The keys a target's `request` block may set are those its collector's request
type accepts (§ 5.1); any other key MUST be rejected at startup naming the
target, the key, the collector and the type. What its `target` may be is also
the type's: an `http` target MUST be an absolute URL, and a `localfile` target
MAY be left out and MUST otherwise lie under the collector's `root`. A target does not declare a type
of its own: it inherits its collector's.

The target document MUST be reloadable on the same terms as the exporter
configuration: an invalid document MUST be rejected with the previous document
left in force.

Each target MUST accept every per-scrape parameter the `/probe` endpoint
accepts — `method`, `path`, `body`, `timeout`, `insecure_skip_verify`,
`retry.attempts`, and `retry.backoff` — with the same semantics, overriding the
collector's own request settings for that target only. Each target MUST also
accept static request `headers` and its own target credentials, as inline or
file-backed basic authentication or a bearer token. Because the document is
operator configuration rather than caller input, these headers are applied
directly and MUST NOT be filtered through the collector's
`request.forward_headers` allowlist.

Each target MAY declare `labels`, which the exporter MUST add to every metric
that target produces. A label the collector already extracted MUST NOT be
overwritten.

Each target MAY declare `otlp.service_name` and `otlp.resource_attributes`.
These form the OTLP resource the target's metrics are exported under. Both MUST
default to the exporter-wide `otlp.service_name` and `otlp.resource_attributes`,
and per-target attributes MUST be merged over the exporter-wide ones rather than
replacing them. The exporter MUST emit one `resourceMetrics` entry per distinct
resource in an export, so metrics from targets with different identities are not
conflated.

The scrape period MUST be the OTLP export interval, so every export carries a
freshly collected set. The exporter MUST bound one scrape pass by that interval
and SHOULD limit how many targets it scrapes concurrently. Scheduled scrapes MUST
run through the same fetch, decode, and transform path as `/probe`, including the
collector's cache, limits, and validation. A scheduled scrape and a `/probe`
request that would produce a byte-for-byte identical request MUST share cache
entries, which requires the cache key to be derived from the same request
fingerprint.

Scheduled targets MUST NOT be exposed on `/metrics` or reachable through
`/probe`; their metrics are delivered only over OTLP.

Each scheduled scrape MUST export a health result under that target's resource
and labels:

```text
http_exporter_target_up
http_exporter_target_scrape_duration_seconds
```

Without them a failing target is absent from the OTLP stream and cannot be
distinguished from a target that was never configured. A failed scrape MUST
export the health result with `http_exporter_target_up` set to zero and MUST NOT
export collector metrics for that target. Whether a failed stage fails the
scrape is the collector's `error_handling` to say, as on a probe (§ 19).

Scheduled scrapes MUST be counted in the existing per-collector self-metrics
rather than in per-target series, so exporter self-metric cardinality does not
grow with the number of targets. The exporter MUST expose
`http_exporter_scheduled_targets` so an operator can confirm the document
loaded.

## 42.15 Redirect following and HTTP/2 negotiation

Collector target requests MUST support two transport settings:

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
redirects using the HTTP client's normal limit.

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

Scheduled targets MUST accept both settings in their `request` block with the
same semantics.

The Helm chart MUST expose both as list-valued `params` entries on each
`monitors` item.

### 42.15b Connection reuse

Requests to targets and OTLP exports MUST reuse their connections. The
exporter MUST keep one connection pool per distinct set of TLS settings — the
CA, client certificate and key files and `insecure_skip_verify` — and HTTP/2
choice, shared by every collector and scrape that uses the same set, rather
than a pool per request, which would reuse no connection, pay a TLS handshake
on every HTTPS scrape, and leave each idle connection open until the other end
closed it. A scrape overriding `insecure_skip_verify` or `enable_http2` MUST
use the pool of its own settings. A TLS file rotated on disk MUST be picked up
by the next request, with the previous pool's idle connections closed. Idle
connections MUST time out (90 seconds), and a pool unused for five minutes —
such as one a reload made obsolete — MUST be closed and dropped. A response
body MUST be read to its end before it is closed where the exporter can, so
the connection returns to the pool.

Every transport MUST send its requests through the proxy the environment names:
`HTTPS_PROXY` for `https` URLs, `HTTP_PROXY` for `http` ones, `NO_PROXY` for
the hosts, domains, addresses and CIDR ranges reached directly, and the
lower-case spellings, as Go's `golang.org/x/net/http/httpproxy` interprets
them; requests to localhost and loopback addresses MUST go direct. This MUST
apply to target requests and OTLP exports alike. The environment MUST be read
when a transport is built rather than once per process, so a test can set it.
There MUST NOT be a proxy setting in the configuration: the proxy belongs to
where the exporter runs, not to a collector.

These settings replace the earlier `request.redirect_policy` string, which MUST
NOT be accepted any more. Because the configuration decoder rejects unknown
fields, a configuration still carrying it fails to load rather than silently
changing how a collector follows redirects.


## 42.15a Environment variable expansion in configuration

The exporter MUST support an optional `--config.export-env` flag that
substitutes `${NAME}` references in the configuration document, in its
collector files (§ 5.0) and in the scheduled target document, from the process
environment before each is parsed.

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
empty string MUST substitute normally: that is a deliberate choice.

Substitution is textual and precedes parsing, so a reference MAY supply any part
of the document rather than only a scalar. For the same reason a value
containing a line break MUST be refused: it would end the line and turn the
remainder into YAML rather than setting a long value.

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
Dependabot, configured in `.github/dependabot.yml`. Each ecosystem MUST be
grouped into a single pull request, because these move together and reviewing
one green build is better than triaging one pull request per module. Major
version updates MUST be excluded: a Go major version lives at a different import
path and needs code changes, so it is deliberate work rather than an automated
proposal.

The versions pinned in the Dockerfile MUST be updated by a scheduled workflow
rather than by Dependabot. They are declared as build arguments and interpolated
into the `FROM` lines and the `pip install`, which the Docker ecosystem updater
cannot resolve; a configuration that appears to cover them would silently
propose nothing.

The resolver MUST live in the repository as ordinary Go code under `tools/`, so
its rules are covered by the same test suite as the exporter, and MUST:

- never cross a major version, for any pin;
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

The CI and release workflows MUST build with the current stable Go release
rather than a pinned version, so the build follows Go's releases without anyone
editing a workflow. The pinned golangci-lint version MUST be a release built
with at least that Go: golangci-lint ships as a binary carrying its own type
checker, which cannot read standard-library sources from a newer toolchain and
panics rather than reporting a lint failure. The linter version MUST be pinned
identically in the Makefile and in CI, and a test MUST keep the two in step, so
that a clean local `make lint` continues to mean a clean CI run. The `go` directive in `go.mod` MUST remain the minimum the
module requires — it is raised by dependency updates, not by the toolchain the
build happens to use — and the Dockerfile MUST pin a Go version that satisfies
it.

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

Both schedules MUST land during Bulgarian working hours on a working day.
Dependabot MUST use an explicit `Europe/Sofia` timezone so the run does not
drift when daylight saving changes. The workflow's cron is evaluated in UTC,
which has no daylight saving, so its hour MUST be chosen to fall mid-morning in
Sofia in both halves of the year.
