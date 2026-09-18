# Generic HTTP Prometheus Exporter — Implementation Specification

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
    ...
  - name: legacy_application
    ...
```

### 3.2 Probe endpoint

Implement:

```text
GET /probe?target=<TARGET>&collector=<COLLECTOR>
```

Parameters:

- `target`: required target address/URL supplied by Prometheus relabeling.
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

Invalid request overrides MUST return a client error. When `timeout` is absent,
the exporter MUST use the incoming scrape request context as the target request
deadline rather than a collector-configured timeout.

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

The exporter MUST expose HTTP response status and request duration in its self-metrics.

---

## 5. Collector configuration

Recommended top-level structure:

```yaml
collectors:
  - name: example
    request:
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
    cache: 60s
```

A collector MUST have a unique name.

A collector MAY set `cache` to a Go duration such as `60s`, `1m`, or `3h`. The
value is the time to live of a cached collector result. Omitting `cache`, or
setting it to `0s`, MUST disable caching for that collector. A negative value
MUST be rejected during configuration validation. Section 42.13 defines the
caching contract.

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

`response.format` is optional and defaults to `auto`. When it is omitted, the
implementation MUST infer a deterministic response decoder from the transform
where possible: `regex` to text, `csv` to CSV, `css` to HTML, and `prometheus`
to Prometheus exposition. jq/yq, XPath, and Python MAY use content detection
because they can operate on more than one response representation. If the
decoded response cannot be mapped to the selected transform, the exporter MUST
return a clear transform error. An explicit response format remains available
for ambiguous or incorrectly labeled endpoints.

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
  response:
    format: json

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
          type: expression
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
  response:
    format: yaml

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
  response:
    format: xml

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
- Python `beautifulsoup4` for Python decoding when Python is selected.

Example conceptual configuration:

```yaml
- name: application_html
  response:
    format: html

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
          type: string
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
| beautifulsoup4 | `bs4` | HTML parsing and CSS selection |
| lxml | `lxml` | XML/HTML parsing and XPath |
| PyYAML | `yaml` | YAML parsing |
| python-dateutil | `dateutil` | Date/time parsing |

Do NOT bundle networking clients merely for Python convenience.

Initially do NOT bundle general-purpose data science packages such as `pandas`, `numpy`, or `scipy` unless a concrete project requirement is later established.

### 16.4 `required_libs` / `libraries`

The collector configuration MUST allow Python scripts to declare third-party dependencies.

Preferred syntax:

```yaml
transform:
  type: python
  libraries:
    - beautifulsoup4
    - lxml
    - PyYAML
  script: |
    from bs4 import BeautifulSoup
    from lxml import etree
    import yaml
```

The implementation may use `required_libs` instead, but the field must be clearly documented.

These declarations are metadata/validation, not an instruction to perform `pip install` during a scrape.

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
        type: expression
        expression: .environment
    expression: .requests
```

`name`, `description`, `type`, `labels`, `expression`, and `error_mode` are the
standard shape. `description` becomes the Prometheus HELP text. `type` MUST be one of
`gauge`, `counter`, `histogram`, `summary`, or `untyped`; omitted types default
to `gauge`. Metric declarations MUST be placed on the collector, alongside
`transform`, rather than using transform-specific arrays such as `rules` or
`expressions`.

`error_mode` MUST be `log` or `ignore` and defaults to `log`. When an
individual metric cannot be extracted, `log` MUST record the metric-specific
error and skip that metric; `ignore` MUST skip it without logging. A
metric-level error MUST NOT fail unrelated metrics in the same collector.

Each label entry MUST have `name` and `type`. `type` MUST be either `string` or
`expression`. A `string` label MUST use `value` as its literal value. An
`expression` label MUST provide `expression`, interpreted by the same transform
as the metric expression:

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
    type: expression
    expression: server       # current CSV row's server column
  - name: environment
    type: string
    value: production
```

Python transforms are the exception: their script emits the common metric
objects through `metric(...)`, so a `metrics` array is optional for them.

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
  on_http_error: fail
  on_decode_error: fail
  on_transform_error: fail
  allow_missing_keys: false
```

Allowed policies SHOULD include:

```text
fail
warn
ignore
```

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

### 19.2 Missing keys

With:

```yaml
allow_missing_keys: false
```

accessing an absent field required by a metric SHOULD cause that metric or scrape to fail according to the selected error policy.

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

This overrides collector defaults where appropriate.

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

Metric names SHOULD be normalized only when explicitly configured; silent surprising renaming is undesirable.

---

# 22. Exporter self-metrics

Expose exporter health metrics on `/metrics`.

At minimum:

```text
http_exporter_scrape_success
http_exporter_scrape_duration_seconds
http_exporter_scrape_http_status_code
http_exporter_scrape_response_bytes

http_exporter_decode_success
http_exporter_parse_errors_total

http_exporter_transform_errors_total
http_exporter_missing_keys_total

http_exporter_script_errors_total
http_exporter_script_duration_seconds

http_exporter_metrics_emitted
http_exporter_series_limit_exceeded

http_exporter_cache_hits_total
http_exporter_cache_misses_total
http_exporter_cache_entries

http_exporter_collector_config_valid
http_exporter_scheduled_targets
```

Labels should include `collector` and, where appropriate, `target`.

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

### 22.1 Verbose per-request self-metrics

The exporter MUST support an opt-in verbose mode that republishes its own
metrics broken down by the individual request that produced them. Verbose mode
MUST NOT remove or alter the per-collector series: the labelled series are
published in addition to them, so a dashboard built on the per-collector view
keeps working when verbose mode is switched on.

Every self-metric family of section 22 MUST be republished with the labels
`collector`, `http_method` and `url`, with the single exception of
`http_exporter_cache_entries`, which counts what a collector's response cache
holds and belongs to no individual request.

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
request uses, so a label can never describe a URL that was not the one fetched.
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

- `/health`: process is alive.
- `/ready`: configuration loaded and exporter ready to serve requests.
- `/self-metrics`: exporter self-metrics by default; the path MUST be configurable and `/metrics` MAY remain as a compatibility alias.
- `/probe`: execute a collector against a supplied target.

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

Enabling the watch MUST NOT weaken any reload rule: an invalid configuration, a
configuration that would disable OTLP while scheduled targets are loaded, and a
pre-script that stops producing `data` MUST all still be rejected with the last
valid configuration left active.

When a disabled watch is configured, the exporter MUST NOT run a polling loop at
all.

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
      method: GET
      path: /api/status

    response:
      format: json

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
      path: /status.yaml

    response:
      format: yaml

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
      path: /status.xml

    response:
      format: xml

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
            type: expression
            expression: server
```

## 28.5 HTML + CSS selector

```yaml
collectors:
  - name: app_html
    request:
      path: /status

    response:
      format: html

    transform:
      type: css
    metrics:
      - name: server_cpu
        description: Server CPU utilization
        type: gauge
        error_mode: log
        expression: '#servers tr'
        labels:
          - name: server
            type: expression
            expression: 'td:nth-child(1)'
```

A simple tag extraction is also valid by setting `expression: h1` in a metric
declaration.

## 28.6 Prometheus input

```yaml
collectors:
  - name: vendor_prometheus
    request:
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
      path: /status

    transform:
      type: jq
      libraries:
        - beautifulsoup4
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
            type: expression
            expression: .workers[].name
```

## 28.9 Python transform

For collectors whose metric names are not known until the response is read. The
script emits through `metric(...)` and the `metrics` array is omitted:

```yaml
collectors:
  - name: dynamic_vendor
    request:
      path: /status

    response:
      format: auto

    transform:
      type: python
      libraries:
        - beautifulsoup4
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

---

# 29. Prometheus Operator manifests

Provide complete examples for:

- Service deployment
- Exporter Service
- ServiceMonitor
- PodMonitor

Example ServiceMonitor pattern:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: generic-http-exporter-target
spec:
  selector:
    matchLabels:
      app: target
  endpoints:
    - port: http
      path: /probe
      params:
        collector:
          - legacy_text
      relabelings:
        - sourceLabels: [__address__]
          targetLabel: __param_target
        - sourceLabels: [__param_target]
          targetLabel: instance
        - targetLabel: __address__
          replacement: generic-http-exporter:8080
```

The example must be tested against Prometheus Operator semantics.

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
```

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

---

# 32. Dependency strategy for Python

Do NOT dynamically execute `pip install` during startup or scraping by default.

The supported Python dependencies should be bundled in the image and version-pinned at build time.

`libraries`/`required_libs` in collector config is used to declare/validate intended dependencies.

Document exact package versions in the project.

A future optional extension may support externally supplied Python environments, but this is out of scope for the initial implementation.

---

# 33. Helm chart

The repository MUST include a production-ready Helm chart for deploying the exporter to Kubernetes.

The Helm chart MUST be maintained in the same repository as the application source code and MUST be usable without manually creating Kubernetes manifests for the core deployment.

Recommended repository location:

```text
charts/prometheus-universal-exporter/
```

The chart SHOULD follow Helm conventions and include at minimum:

```text
charts/prometheus-universal-exporter/
├── Chart.yaml
├── values.yaml
├── README.md
├── templates/
│   ├── _helpers.tpl
│   ├── deployment.yaml
│   ├── service.yaml
│   ├── configmap.yaml
│   ├── serviceaccount.yaml
│   ├── servicemonitor.yaml
│   ├── podmonitor.yaml
│   └── NOTES.txt
```

Not every listed template must be rendered by default, but the chart structure MUST cleanly support the corresponding features.

### 33.1 Deployment

The chart MUST deploy the exporter as a Kubernetes `Deployment`.

The chart MUST expose the exporter's process settings as values rather than
hardcoding them in the Pod template. At minimum `server.listenAddress` MUST set
`--web.listen-address` and `server.pythonPath` MUST set `--python.path`. The
container port MUST be derived from the port in `server.listenAddress`, and a
value without a valid TCP port MUST fail rendering with a clear message. The
container port MUST keep the name `http` so Service, Ingress, ServiceMonitor,
and PodMonitor references remain valid when the port changes.
`server.pythonPath` MUST default to the interpreter path in the published
container image.

Arguments rendered into the Pod template MUST be quoted so the argument value
reaches the process exactly as configured, without literal quote characters.

Configurable values SHOULD include at least:

```yaml
image:
  repository: ...
  tag: ...
  pullPolicy: IfNotPresent

replicaCount: 1

resources:
  requests: {}
  limits: {}

service:
  enabled: true
  type: ClusterIP
  port: 8080

podSecurityContext: {}
securityContext: {}
nodeSelector: {}
tolerations: []
affinity: {}
```

The deployment SHOULD run as a non-root user where practical.

The chart MUST configure liveness/readiness probes using the exporter health endpoints.

### 33.2 Exporter configuration

Collector configuration MUST be supplied through a Helm-managed ConfigMap by default.

Example:

```yaml
config:
  enabled: true
  data:
    config.yaml: |
      collectors:
        - name: example
          ...
```

The chart MUST mount the configuration into the exporter container using a stable path such as:

```text
/etc/prometheus-universal-exporter/config.yaml
```

The container arguments MUST reference that path.

### 33.3 Configuration reload / rollout

The chart MUST ensure that changes to the ConfigMap eventually cause the exporter to use the new configuration.

Preferred behavior:

- If the exporter supports live configuration reload, mount the ConfigMap and configure the exporter to reload it.
- Otherwise, use a checksum annotation on the Deployment pod template so a ConfigMap change triggers a rollout.

The implementation MUST document which behavior is used.

### 33.4 Service

The chart MUST expose a `service.enabled` value that defaults to `true`. When
enabled, the chart MUST create a Kubernetes `Service` exposing the exporter
HTTP port. When disabled, the Service resource MUST NOT be rendered.

The Service MUST be usable as the target of Prometheus Operator
`ServiceMonitor` resources. The documentation MUST warn that the chart's
generated ServiceMonitor and PodMonitor routing requires this Service.

### 33.5 ServiceMonitor support

The chart MUST provide optional `ServiceMonitor` resources, selected through
entries in the shared `monitors` values array.

Example values:

```yaml
monitors:
  - name: application-services
    enabled: true
    type: service
    interval: 30s
    scrapeTimeout: 10s
    labels: {}
    annotations: {}
    targetSelector: {}
    collector: example
    params: {}
    relabelings: []
    metricRelabelings: []
```

The chart MUST allow configuring `params.collector`, user-provided
`relabelings`, and `metricRelabelings` to route discovered targets through
`/probe` and filter or rewrite scraped samples.

Because collector selection is target-specific, the chart MUST support a documented configuration pattern where a ServiceMonitor endpoint passes:

```yaml
params:
  collector:
    - example
```

and rewrites the target using:

```yaml
relabelings:
  - sourceLabels: [__address__]
    targetLabel: __param_target
  - sourceLabels: [__param_target]
    targetLabel: instance
  - targetLabel: __address__
    replacement: <exporter-service>:<port>
```

Each monitor entry MUST have a unique name and select one collector. Multiple
entries MUST be supported for different target selectors, collectors, or
scrape settings.

### 33.6 PodMonitor support

When a `monitors` entry has `type: pod`, the chart MUST provide the equivalent
optional `PodMonitor` resource using that entry's settings.

It MUST implement the equivalent target relabeling and collector parameter behavior described for `ServiceMonitor`.

### 33.7 CRD availability

The exporter Helm chart MUST NOT install Prometheus Operator CRDs itself.

If `ServiceMonitor` or `PodMonitor` is enabled and the required CRDs are absent, the chart SHOULD fail clearly or document the dependency on Prometheus Operator.

### 33.8 RBAC

The exporter does not inherently need Kubernetes API access for target discovery because target discovery is performed by Prometheus Operator.

Therefore the chart SHOULD default to no additional Kubernetes API permissions.

A dedicated ServiceAccount MAY be created for standard Kubernetes deployment conventions, but no broad `ClusterRole`/`ClusterRoleBinding` should be installed unless a future feature explicitly requires it.

### 33.9 Network and security settings

The chart SHOULD support:

- Pod security context
- Container security context
- Read-only root filesystem where practical
- Dropping Linux capabilities
- `allowPrivilegeEscalation: false`
- NetworkPolicy as an optional feature

If a NetworkPolicy is provided, it MUST account for the exporter needing to reach configured target endpoints.

### 33.10 Helm values

`values.yaml` MUST document all user-configurable settings. At minimum:

```yaml
image: {}
replicaCount: 1
nameOverride: ""
fullnameOverride: ""
resources: {}
server: {}
service: {}
config: {}
otlpTargets: {}
serviceAccount: {}
securityContext: {}
podSecurityContext: {}
nodeSelector: {}
tolerations: []
affinity: {}
monitors: []
networkPolicy: {}
```

The chart MUST provide sane production defaults and avoid hardcoding environment-specific values.

### 33.11 Helm validation

The repository MUST include automated Helm validation covering at least:

- `helm lint`
- `helm template` with default values
- `helm template` with one enabled `monitors` entry of `type: service`;
- `helm template` with one enabled `monitors` entry of `type: pod`;
- `helm template` with multiple enabled `monitors` entries;
- `helm template` with `monitors: []`.
- every rendered manifest starting its own YAML document, with monitors enabled
  and the self-metrics monitor rendering alongside them
- ConfigMap generation
- Deployment generation
- Service generation
- Correct `/probe` path and collector parameter configuration

If feasible, use a Kubernetes schema/testing tool such as `kubeconform` or an equivalent to validate rendered manifests.

### 33.12 Helm examples

The repository MUST include examples showing:

1. Basic exporter installation.
2. Exporter with a JSON collector.
3. ServiceMonitor targeting a Service and selecting a collector.
4. PodMonitor targeting Pods and selecting a collector.
5. Configuration containing multiple collectors.

Examples MUST not contain real credentials.

### 33.13 Chart documentation

The Helm chart README MUST document:

- Installation
- Upgrade
- Uninstallation
- Configuration values
- How to supply collectors
- ServiceMonitor usage
- PodMonitor usage
- Prometheus Operator prerequisite
- Resource/security configuration
- Configuration reload behavior

---

# 34. Testing requirements

Testing is a first-class project requirement. The implementation MUST include a comprehensive automated test suite covering unit behavior, decoder behavior, configuration validation, integration with an HTTP test server, Prometheus exposition, Python execution, Kubernetes/Prometheus Operator integration, Helm rendering, resource limits, concurrency, security restrictions, and failure modes.

The repository MUST be testable with a standard, documented command such as:

```text
make test
```

or equivalent.

The test suite MUST be deterministic and MUST NOT require access to real third-party services or the public internet.

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
- Invalid metric `error_mode`.
- Missing `error_mode` defaults to `log`.
- Transform-specific response format incompatibility.
- Response format inference when `response.format` is omitted.
- Invalid error policy values.
- Missing required configuration fields.
- Unknown configuration fields according to the chosen strictness policy.

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

Test the canonical metric declaration shape with `name`, `description`,
`type`, `labels`, and `expression` for every non-Python transform. Verify that
only the five allowed Prometheus metric types are accepted, that descriptions
become HELP text, and that transform-specific expression semantics are applied
consistently.

Test that a transform `pre_script` runs once before extraction, can mutate or
replace `data`, is subject to timeout/output restrictions, and is reparsed for
HTML/XML output.

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

- BeautifulSoup4: parse HTML and select an element.
- lxml: parse XML and run XPath.
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

## 34.28 ServiceMonitor and PodMonitor tests

The repository MUST contain fixture manifests for:

- ServiceMonitor with one collector.
- ServiceMonitor with multiple collector configurations.
- PodMonitor with one collector.
- PodMonitor with multiple collector configurations.

Rendered manifests MUST be tested for:

- Correct scrape path `/probe`.
- Correct `collector` parameter.
- Correct `__param_target` relabeling.
- Correct `instance` label relabeling.
- Correct exporter `__address__` rewrite.
- Correct port/service references.
- Selector behavior.

Where practical, run an integration test against a Kubernetes test cluster and Prometheus Operator CRDs.

## 34.29 Helm tests

The Helm chart MUST be tested using:

```text
helm lint
helm template
```

with at least these values combinations:

1. Default configuration.
2. Monitor disabled.
3. Monitor enabled with `type: service`.
4. Monitor enabled with `type: pod`.
5. Custom target and metric relabelings.
6. Custom image/repository/tag.
7. Custom resources.
8. Custom securityContext.
9. Custom exporter arguments/configuration, including a custom
   `server.listenAddress` and `server.pythonPath`, and an invalid
   `server.listenAddress` that MUST fail rendering.
9a. Scheduled targets enabled, which MUST add the `--otlp.targets-file`
   argument and render the target document into the exporter ConfigMap. When
   the chart manages the configuration, enabling scheduled targets without
   `otlp.enabled: true`, or with an empty document, MUST fail rendering with an
   explicit message rather than producing a Deployment that cannot start.
10. Multiple collectors in ConfigMap content.
11. Existing Secret references for credentials where supported.

Every manifest a template renders MUST begin its own YAML document. A template
that renders more than one manifest, whether because it declares several or
because it wraps one in a range, MUST emit a `---` before each. `helm template`
and `helm lint` do not detect a missing separator — helm prints whatever the
template produced — so two manifests silently merge into a single document and
the chart only fails when it is applied.

Two checks MUST cover this, because neither alone is enough. A test in the Go
suite MUST verify statically that every manifest a template renders is preceded
by a separator, and the CI path filters MUST run that suite for changes under
`charts/`, since a chart-only change is exactly the change that would break it.
A CI step MUST additionally render the chart with monitors enabled and fail when
the number of manifests rendered exceeds the number of YAML documents the output
parses into.

Rendered manifests SHOULD be validated with `kubeconform`, `kubeval`, or equivalent.

Helm tests MUST verify that invalid combinations either fail rendering clearly or are rejected by chart validation.

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

The image build MUST be reproducible and MUST pin dependency versions sufficiently for production use. The Dockerfile MUST expose the Go base version, Python base version, and each bundled Python dependency version as `ARG` variables with documented defaults, so builds can override them without editing the Dockerfile.

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
- Failed probes are not cached.
- Entries beyond `limits.max_cache_entries` are evicted, expired entries first.
- A cached metric set is copied on store and on read, so neither the producer
  nor a reader can mutate the stored entry.
- Concurrent probes of a cached collector are race-free under `go test -race`.
- Cache hit, miss, and entry self-metrics are exposed per collector.
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
- Every Go version the workflows request satisfies the go directive in
  `go.mod`, as does the version the Dockerfile pins, and the comparison itself
  is covered for versions of differing granularity.
- The golangci-lint version pinned in the Makefile and the one pinned in CI are
  the same.

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

The Python documentation MUST explicitly state that networking is owned by the exporter and that `requests`/`httpx` are unnecessary.

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

The Helm chart MUST expose values for the self-health path and MUST provide an
optional ServiceMonitor and PodMonitor that select the exporter Service/Pods
and scrape the dedicated endpoint. The self-health monitor MUST be separate
from the target-probing monitor: a monitor that selects discovered target Pods
MUST NOT accidentally scrape the exporter health endpoint on those target
Pods.

The Helm chart MUST support:

- `namespaceOverride` for namespaced resources;
- configurable Deployment strategy, including RollingUpdate settings;
- CPU and memory requests and limits through `resources.requests` and
  `resources.limits`;
- `tolerations` and `affinity`;
- an optional Ingress resource with class, host, path, TLS, and annotations;
- optional NEG integration, implemented by a configurable Service annotation
  such as the GKE `cloud.google.com/neg` annotation.

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

The initial implementation MAY use the OTLP/HTTP JSON representation. Gauge,
counter, labels, timestamps, descriptions, and resource attributes MUST be
preserved where available. Histogram and summary data SHOULD be preserved or
clearly documented when represented as a reduced OTLP form.

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
        type: expression
        expression: server
transform:
  type: csv
```

CSS remains a first-class transformation for HTML responses. It MUST NOT be
required for CSV responses and MUST NOT be used as the CSV transformation
name.

## 42.3 Additional tests and documentation

Tests MUST cover the configurable self-health path, separate Operator monitor
resources, namespace override, Deployment strategy, resources, scheduling
constraints, Ingress/NEG rendering, CSV transformation, OTLP payload shape,
OTLP timeout/failure behavior, and the guarantee that OTLP failures do not
fail Prometheus probes.

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
3. The Helm chart MUST expose a monitor `headers` map for non-secret target
   headers. Each entry MUST be encoded as a `header_<Header-Name>` endpoint
   parameter. The exporter MUST forward those values only when the selected
   collector lists the header in `request.forward_headers`.
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

The test suite MUST verify bearer and basic monitor rendering, forwarding of
an explicitly enabled Authorization header, forwarding of allowlisted
`header_*` values, and non-forwarding of unallowlisted or transport headers.

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

The Helm chart MUST default `targetAuth.enabled` to false and mount no target
credential Secret by default. It SHOULD provide an optional Secret volume
controlled by `targetAuth.enabled`, `targetAuth.type`, `targetAuth.secretName`,
and the corresponding bearer or basic-auth key/file settings, so the file path
can be declared in exporter configuration without placing credentials in a
ConfigMap. For basic authentication, the exporter MUST support:

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

## 42.7 Helm monitor arrays and opt-in monitor authentication

The Helm chart MUST expose a `monitors` array. Each entry MUST contain a
unique optional resource name, an `enabled` flag, and a `type` of `pod` or
`service`:

```yaml
monitors:
  - name: application-services
    enabled: true
    type: service # service or pod
```

Each enabled entry MUST render exactly one corresponding PodMonitor or
ServiceMonitor. The chart MUST support multiple enabled entries and produce
unique resource names. A self-health monitor MUST be rendered once per
monitor type used by the array when self-health monitoring is enabled.

Monitor authentication MUST be explicitly opt-in and disabled by default for
each array entry:

```yaml
auth:
  enabled: false
  type: bearer # bearer or basic
```

When `auth.enabled` is false, the chart MUST NOT render `authorization` or
`basicAuth`, regardless of the configured `auth.type`. When enabled, `auth.type`
MUST select bearer or basic authentication and the chart MUST render the
corresponding SecretKeySelectors. Tests MUST cover the disabled default, both
monitor selector types, and enabled bearer/basic authentication rendering.

## 42.8 Monitor relabeling and Deployment rollout behavior

Each Helm `monitors` entry MUST expose native Prometheus Operator
`relabelings` and `metricRelabelings` lists. The chart MUST preserve its
mandatory target-routing relabelings and append user-provided entry
`relabelings` after them. Entry `metricRelabelings` MUST be rendered on the
selected ServiceMonitor endpoint or PodMonitor pod metrics endpoint.
These lists MUST support the standard fields, including `sourceLabels`,
`targetLabel`, `regex`, `replacement`, `action`, and `modulus`, as applicable.

The self-health monitor MUST independently support
`selfMetrics.relabelings` and `selfMetrics.metricRelabelings`.

The default Deployment strategy MUST work with `replicaCount: 1`. The default
`RollingUpdate` settings MUST use explicit `maxUnavailable: 0` and
`maxSurge: 1`, keeping the old ready Pod until the replacement is ready and
temporarily allowing two Pods. The chart MAY accept percentage values as
supported by Kubernetes, but MUST document their rounding behavior. Users that
require no overlap MAY choose `strategy.type: Recreate`.

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

The Helm chart MUST expose these parameters as list-valued `params` entries on
each `monitors` item, and MUST expose `interval` and `scrapeTimeout` on each
item as the Prometheus Operator scrape settings. The monitor scrape timeout
and the exporter target-request timeout override are distinct: the former is
set on the generated ServiceMonitor or PodMonitor, while the latter is passed
to `/probe` as `params.timeout`. The TLS override is passed as
`params.insecure_skip_verify`; when absent, the collector's TLS setting MUST be
preserved. `params.retry_attempts` and `params.retry_backoff` override the
collector's retry settings for that scrape; when absent, the collector values
MUST be preserved.

## 42.11 Helm-wide default metadata

The Helm chart MUST expose `defaultLabels` and `defaultAnnotations` maps. The
chart MUST apply them to the metadata of every Kubernetes object it creates,
including the Deployment Pod template and conditionally rendered ConfigMap,
ServiceAccount, Service, Ingress, NetworkPolicy, ServiceMonitor, and
PodMonitor resources. Resource-specific metadata maps MUST be applied after
the defaults and therefore MUST override a same-named default. The chart's
generated identity labels and required operational annotations MUST remain
valid and authoritative where they conflict with user defaults.

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

The chart release workflow MUST be triggered by tags under `chart` and MUST:

- verify that the version implied by the tag matches the `version` field in
  `Chart.yaml`, failing the release when they disagree;
- re-run the chart lint and template scenarios;
- package the chart without overriding `version` or `appVersion`, so the
  published artifact carries exactly what the committed `Chart.yaml` declares;
- publish the chart as an OCI artifact; and
- create a GitHub Release containing the chart archive.

`Chart.yaml` MUST be the source of truth for the chart version. `appVersion`
records the exporter release a chart version was validated against and MUST be
maintained by hand rather than derived from a release tag.

Both release workflows MUST use `GITHUB_TOKEN` with `contents: write` and
`packages: write` permissions and MUST run their respective validation checks
before publishing artifacts.

The repository's tests MUST cover the release tag contract: that each release
workflow triggers on its whole namespace, that the format it enforces accepts
well-formed tags and rejects malformed ones, and that the version committed in
`Chart.yaml` would produce a tag the chart release accepts.

## 42.13 Collector response caching

Each collector MUST support a `cache` parameter holding a Go duration, for
example `60s`, `1m`, or `3h`:

```yaml
collectors:
  - name: expensive_api
    cache: 60s
    limits:
      max_cache_entries: 1000
```

When `cache` is greater than zero and a probe repeats a request the exporter
has already served within that interval, the exporter MUST return the stored
result and MUST NOT contact the target again. Omitting `cache` or setting it to
`0s` MUST disable caching for that collector, which is the default. A negative
value MUST be rejected during configuration validation.

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
MUST retire every entry cached under the previous definition. Credentials read
from files at request time are covered only through their configured paths, so
a rotated credential file takes effect for a cached request once the entry
expires; deployments that rotate credentials faster than the cache interval
SHOULD shorten `cache` accordingly.

Only a fully successful probe MAY be cached. Requests that fail at the HTTP,
decode, transform, or validation stage MUST NOT be stored.

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
```

A cache hit MUST count as a successful scrape in `http_exporter_scrapes_total`
and `http_exporter_scrape_success`, and the cached metrics MUST still be queued
for OTLP export. Target-request self-metrics such as
`http_exporter_scrape_http_status_code` and
`http_exporter_scrape_response_bytes` describe the last real target request and
MUST NOT be altered by a cache hit.

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
export collector metrics for that target.

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
`error_handling.on_http_error` is `fail` therefore fails the probe, which is the
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

These settings replace the earlier `request.redirect_policy` string, which MUST
NOT be accepted any more. Because the configuration decoder rejects unknown
fields, a configuration still carrying it fails to load rather than silently
changing how a collector follows redirects.


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
