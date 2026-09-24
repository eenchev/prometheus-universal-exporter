# Configuration

The exporter reads one YAML document, named by `--config.file`, and any
[collector files](#collector-files) it lists. It declares the
collectors — how to call a target, how to read what comes back, and which
metrics to publish — and, optionally, the exporter's own settings under `web`
and `otlp`. Target URLs are deliberately not part of it: Prometheus supplies
each one per scrape.

`config.example.yaml` in the repository root is a complete working document to
start from. This page is the reference for what it may contain.

## Collectors

Collectors contain request, response, decoder, transformation, error-policy, and limit settings. Target URLs are deliberately not stored in configuration.

A collector's `name` is what a probe asks for, so it must be unique: across the
configuration's `collectors` and every [collector file](#collector-files). The
same name twice stops the exporter at startup, fails `--dry-run`, and rejects a
reload, with an error naming both places it was defined:

```text
duplicate collector "app_json": defined in /etc/exporter/config.yaml and in /etc/exporter/collectors.d/payments.yaml
```

`response.format` is optional and defaults to `auto`. When omitted, the
transform selects a deterministic decoder where possible: `regex` uses text,
`csv` uses CSV, `css` uses HTML, and `prometheus` uses Prometheus exposition.
JSON/YAML transforms use content detection. If the decoded response cannot be
used by the selected transform, the probe fails with a clear mapping error.
An explicit format remains useful for ambiguous or mislabeled endpoints.

Supported response formats are `json`, `yaml`, `xml`, `csv`, `html`,
`prometheus`, `text`, and `auto`. Supported transforms include jq/yq, XPath,
CSS, CSV, regex, Prometheus filtering, and Python. JSON and YAML expressions
use the embedded jq-compatible engine (the expression language is also used
for yq-compatible transformations). XML supports XPath, HTML supports CSS
selectors and XPath (including bare element selectors such as `h1`), text
supports regular expressions, and Prometheus input is parsed before
filtering/renaming.

### Character encodings

Everything the exporter answers is UTF-8, as Prometheus requires; it refuses
a whole scrape over one label value that is not. A response in another
encoding is converted before it is decoded. The encoding comes from, in this
order: a byte order mark (UTF-8, UTF-16LE or UTF-16BE); the collector's
`response.charset`; the `charset` of the `Content-Type` header; and, for HTML,
a `<meta charset>` near the top of the page or, for XML, the encoding of the
XML declaration.

```yaml
  - name: legacy_status
    request:
      type: http
    response:
      # The target sends windows-1251 and says nothing, or says the wrong thing.
      charset: windows-1251
```

`response.charset` is for a target that does not declare its encoding, or
declares the wrong one, and for [local files](LOCALFILE.md), which declare
none. Names follow the WHATWG Encoding Standard, as in a browser: `utf-8`,
`windows-1252`, `iso-8859-2`, `windows-1251`, `koi8-r`, `shift_jis`, `gbk`,
`euc-kr` and the rest; `iso-8859-1` and `latin1` are read as `windows-1252`,
as browsers do. An unknown name in `response.charset` fails to load; an
unknown name declared by a target fails the `decode` stage, naming it.
Transforms and Python scripts see the converted body, and its `Content-Type`
says `charset=utf-8`.

What still is not valid UTF-8 after that — a target that says UTF-8 and is
not — no longer fails the scrape: the invalid bytes in label values and help
text are replaced with `�` (U+FFFD), counted in
`http_exporter_invalid_utf8_total`, and logged as a warning naming the first
metric. Setting `response.charset` fixes it at the source.

Prometheus input is the text exposition format, version 0.0.4, read by the
exporter's own parser. It follows the reference parser's rules — families from
HELP and TYPE lines, `_sum`/`_count`/`_bucket` grouped into summaries and
histograms, quoted UTF-8 names such as `{"my.metric", key="value"} 1` — and
accepts three things the reference parser rejected: a body without a final
newline, CRLF line endings, and trailing blanks on a line. It rejects a
negative, NaN or infinite histogram or summary count. A malformed body fails the
decode with the offending line number, for example
`text format parsing error in line 3: expected float as value, got "n/a"`.

All non-Python transforms use the same collector-level metric declaration. Each
entry has `name`, `description`, `type`, `labels`, and a transform-specific
`expression`. The only allowed metric types are `gauge`, `counter`,
`histogram`, `summary`, and `untyped`:

```yaml
collectors:
  - name: app_json
    request:
      type: http
    transform:
      type: jq
      pre_script: |
        data["requests"] = data.get("requests", 0)
    metrics:
      - name: application_requests_total
        description: Total application requests
        type: counter
        error_mode: log
        expression: .requests
        labels:
          - name: environment
            expression: .environment
```

The expression and label values are interpreted by the selected transform:

- `jq`/`yq`: jq expressions evaluated against decoded data.
- `regex`: a RE2 expression; the first capture group is the numeric value and
  labels map to capture-group numbers or names.
- `csv`: the expression is the numeric column name and labels map to column
  names.
- `css`: the expression selects HTML elements whose text is numeric; labels are
  selectors within each selected element. For tables, select the rows with
  [`items`](#metrics-per-item) and the value and the labels as cells of each row.
- `xpath`: the expression selects XML/HTML nodes whose text is numeric; labels
  are relative XPath expressions or `@attribute` selectors.
- `prometheus`: the expression matches source metric names; it can remap the
  name, description, type, and selected labels.

CSS remains available specifically for HTML tables and HTML status pages; it is
not used for CSV.

Each label sets one of two keys. `expression` reads the label from the
response, and `value` gives a static label, exported exactly as written:

```yaml
labels:
  - name: server
    expression: server       # CSV column for the current row
  - name: environment
    value: production        # static, on this metric only
```

A label setting both, or neither, is refused at startup. For a static label on
every metric of a collector, use `transform.labels` instead.

Label expressions use the same transform-specific language as the metric
expression. For CSV, each row produces a metric and `expression: server`
selects that row's `server` column.

A label expression that gives a series no value — a selector or path that
matches nothing, a missing attribute, column or capture group, a null — leaves
the label off that series, as does an empty value, which Prometheus treats the
same way. When a series is wrong without the label, mark it `required`:

```yaml
labels:
  - name: server
    expression: td.name
    required: true    # a row without a name is an error, not an unlabelled series
```

A series missing a required label is a missing value of its metric, handled by
the metric's [`error_mode`](#when-a-metric-cannot-be-extracted): `ignore` and
`log` drop that one series and keep the rest, `fail` fails the probe with an
error naming the label. It is counted in `http_exporter_missing_keys_total`,
and applies whatever `required` and `error_handling.allow_missing_keys` say
about the value. Without `items`, jq pairs label values with series by
position, so a required label must give one value, applied to every series, or
exactly one per series; any other count fails the metric rather than put labels
on the wrong series. `required` applies to `expression` labels, and not to
the python transform, whose labels come from its script.

### Prefixing a collector's metrics

`metrics_prefix` puts a namespace in front of everything a collector exports.
The exporter joins it with `_`, so

```yaml
collectors:
  - name: grafana_status
    metrics_prefix: grafana
    ...
    metrics:
      - name: statuspage_status
```

exports `grafana_statuspage_status`. It is optional; without it, names are
exactly as declared.

### UTF-8 names

Prometheus 3 lets a target name a metric or a label in any UTF-8 —
`http.server.duration`, `{service.name="api"}` — as OpenTelemetry names do.
The exporter answers in the classic text format, whose names are limited to
letters, digits, `_` and, for metrics, `:`, as are the names older
Prometheus servers, recording rules and dashboards expect. A name outside
that can come from a `prometheus` transform passing a target through, a Python
script or a pre-script. `name_escaping` says what happens to it:

```yaml
collectors:
  - name: otel_app
    name_escaping: underscores   # fail (the default), underscores or values
    transform:
      type: prometheus
```

| `name_escaping` | `{"http.server.duration", "service.name"="api"} 0.25` becomes |
| --- | --- |
| `fail` (default) | a failed scrape: ``metric name "http.server.duration" is not a classic Prometheus name; set the collector's name_escaping to underscores or values to export it escaped`` |
| `underscores` | `http_server_duration{service_name="api"} 0.25` |
| `values` | `U__http_2e_server_2e_duration{U__service_2e_name="api"} 0.25` |

`underscores` replaces every character a classic name may not have with `_`,
and a leading digit too, which reads naturally but cannot be undone.
`values` is Prometheus's reversible encoding: `U__`, then the name with `_`
doubled and every other character written as `_` + its hexadecimal code
point + `_`. Prometheus 3 and its client libraries can turn it back into the
original name. They are the escaping schemes of the same names Prometheus
negotiates with its targets; the third, `dots`, is not offered, because it
rewrites every name that has an underscore, classic ones included.

With either scheme a classic name is never changed, and label values are never
escaped, since they may hold any UTF-8. `metrics_prefix` is joined first, so a
prefixed `values` name still starts with `U__`: `metrics_prefix: otel` gives
`U__otel__http_2e_server_2e_duration`. Two names that escape to the same name
— `a.b` and `a_b` with `underscores` — are not merged: two such metrics are a
duplicate series, and two such labels of one series fail the scrape naming
both. The default is `fail` so a name never changes without someone having
asked for it.

The prefix applies to every metric the collector produces, whatever the
transform: declared metrics, the names a Python script passes to `metric(...)`,
and the source names a `prometheus` transform passes through or renames. A
histogram or summary keeps its family, so `_bucket`, `_sum` and `_count` follow
the prefixed name. /probe, OTLP export and scheduled targets all see the same
prefixed names, and the response cache is keyed by the collector's definition,
so a changed prefix never serves metrics cached under the old names. The
exporter's own `http_exporter_*` metrics, including a scheduled target's health
metrics, describe the exporter rather than the target and are never prefixed.

A prefix must match `^[a-zA-Z][a-zA-Z0-9]*(_[a-zA-Z0-9]+)*$`: a letter first,
then letters and digits, in parts joined by single underscores.

| Prefix | |
| --- | --- |
| `grafana`, `vendor_eu`, `acme2` | Valid |
| `grafana_` | Invalid: the exporter adds the `_`, which would double it |
| `_grafana`, `a__b` | Invalid: names starting with `__` are reserved by Prometheus, and a double underscore anywhere reads as one |
| `grafana:cloud` | Invalid: `:` is reserved for recording rules |
| `1grafana`, `graf-ana` | Invalid: not a metric name |

An invalid prefix stops the exporter at startup, and `--dry-run` reports it,
naming the collector. So does a declared metric whose prefixed name would be
longer than `limits.max_metric_name_length` (200 by default); a name produced at
scrape time, by a Python script or a `prometheus` transform, is checked against
the same limit when the scrape happens.

Log lines and probe errors name a metric rule as it is written in the
configuration, without the prefix, so it can be found in the file. The prefix is
added blindly: a rule already called `grafana_status` becomes
`grafana_grafana_status`, so drop it from the rule names when adding it to the
collector.

### Metrics per item

A jq or yq metric can set `items`, which selects the things the metric is about
— servers, rows, components. The value and every label are then evaluated once
per item, against that item as `.`, with the whole document available as
`$root`:

```yaml
metrics:
  - name: server_cpu
    description: CPU utilization per server
    type: gauge
    items: .servers[]
    expression: .cpu
    labels:
      - name: server
        expression: .name
      - name: site
        expression: $root.site
```

Without `items`, the value expression and each label expression run over the
whole document and are paired by position: the third value gets the third
label value. That works while every expression yields exactly one value per
element, and goes quietly wrong when one does not — a label that yields nothing
for one server shifts every later label onto the wrong series, and a label that
yields a single value is applied to all of them. With `items` there is nothing
to pair: a label that yields nothing for an item is simply absent on that
series.

Per item, the value and each label must yield at most one value; two is an
error, since there is no telling which belongs to the series. A value that is
missing or null for one item is that item's missing metric, handled by
`required` and `error_mode` like any other: `log` drops that one series and keeps
the rest. `items` selecting nothing is a missing metric when the metric is
required, and produces nothing when it is not. `$root` works without `items`
too, where it is the same document as `.`.

The `css` transform takes `items` too, for HTML tables and lists: `items`
selects the rows, and the expression and each label are selectors within one
row. Without it, the value is the whole text of each element the expression
selects, and a label selector looks inside that element, so a label could only
read text that is part of the number.

```yaml
transform:
  type: css
metrics:
  - name: server_cpu
    items: '#servers tr:has(td)'   # the rows with cells, not the header row
    expression: td:nth-child(2)
    labels:
      - name: server
        expression: td:nth-child(1)
```

The rules are the same as for jq: within a row, the value and each label
selector must match at most one element, a row without the value cell is that
row's missing metric, and a label selector matching nothing leaves the label
off.

### Long label values

Label values are capped by `limits.max_label_value_length`, 500 bytes by
default, and a value over the cap fails the whole scrape rather than one series:
a silently shortened value would be a surprise. A label that carries free text
can ask to be cut instead:

```yaml
labels:
  - name: message
    expression: .latest_update
    truncate: true
```

A longer value is then cut to the cap, on a character boundary, and ends in
`…`, which counts towards the cap. Truncation applies to declared metrics from
every transform, before any `metrics_prefix` is added.

### Turning a status into metrics

A status such as `operational` or `major_outage` is text, and a metric value is
a number. Put the text in a label and the value `1` on one series per thing that
has a status:

```yaml
- name: statuspage_component_status
  description: Status of a component, 1 with the current status as a label
  type: gauge
  items: .components[]
  expression: 1
  labels:
    - name: component
      expression: .name
    - name: status
      expression: .status
```

```promql
statuspage_component_status{status="major_outage"}
```

Only the current status has a series. When it changes, the old series ends and
a new one starts, which Prometheus marks stale on the next scrape. The
alternative is one series per possible status, `1` for the current one and `0`
for the rest, which keeps every series alive at the cost of one series per
status per component; choose it when you want `== 0` comparisons or an unbroken
history per status. Counts — incidents by impact, say — are different: a `0`
there is a real value, not a status that does not apply, so keep it.

Keep free text — an incident's latest update, say — off such a series. Its
value changes whenever the text does, and every change starts a new series, so
an alert on the status would reset each time the page posts an update. Put the
text on a separate series that exists only while it matters, with
`truncate: true`, and join on it when you want it:

```promql
statuspage_component_status{status!="operational"}
  and on (component_id) statuspage_component_incident_info
```

[`testdata/config.grafanastatus.json-test.yaml`](../testdata/config.grafanastatus.json-test.yaml)
is a complete collector for [status.grafana.com](https://status.grafana.com),
and for any page hosted on Atlassian Statuspage, which all publish the same
`/api/v2/summary.json`:

```sh
curl 'http://localhost:8080/probe?collector=statuspage&target=https://status.grafana.com'
```

It exposes the page's overall indicator, every component and component group,
unresolved incidents by impact, scheduled maintenances by status, and the start
of the next maintenance. Component names repeat on that page — the same region
appears under most product groups, and occasionally twice in one group — so
each component series also carries `component_id` and `group`, which keeps the
series distinct. The group is found through `$root`:

```yaml
- name: group
  expression: '.group_id as $id | first($root.components[] | select(.id == $id)) | .name'
```

The component series also carry `cloud_provider` and `cloud_zone`, parsed from
names such as `AWS Ireland - prod-eu-west-6: API` (`AWS`, `prod-eu-west-6`) with
jq's `capture`:

```yaml
- name: cloud_zone
  expression: 'first(.name | capture("^(?<provider>AWS|Azure|GCP|GCS) (?<location>.+?)(?: - | )(?<zone>[a-z][a-z0-9-]*[0-9])(?::|$)")) | .zone'
```

A component whose name carries neither, such as `Support Tickets`, matches
nothing, and so has neither label.

For every component that is not operational it also exposes
`statuspage_component_incident_info`, whose labels name the incident or
maintenance in progress behind the status and carry that event's latest update
as `message`, collapsed to one line and cut to 300 bytes by `truncate: true`.
Maintenance that is only scheduled is not counted as the cause. A component
marked down with no event behind it still gets the series, without those labels.

No series carries the page's name: Prometheus labels every series with the
target it probed (`instance`), which already tells two pages apart.
`statuspage_info{page="Grafana Cloud"}` carries the name once, for dashboards.

### Request types

Every collector's `request` block starts with `type`, which is required and
says how the collector reaches its data:

```yaml
request:
  type: http
  path: /api/status
```

There are two types: `http` asks a URL, and `localfile` reads a file from the
exporter's own filesystem — see [Local files](LOCALFILE.md). A collector
without `type` stops the exporter at startup with a message saying what to add,
and so does an unknown type.

Each type accepts its own keys. For `http`, `type` is the only required one —
`path` can be left out when the target URL already carries the whole path, and
`method` defaults to `GET`:

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `http`. |
| `method` | `GET` | GET, POST, PUT, PATCH, DELETE or HEAD. |
| `path` | — | Joined onto the target; may use [path parameters](REQUESTS.md#path-parameters). |
| `query` | — | Query parameters added to the request; values may use [placeholders](REQUESTS.md#in-the-body-headers-and-query). |
| `headers` | — | Sent to the target; values may use [placeholders](REQUESTS.md#in-the-body-headers-and-query). |
| `body` | — | Request body; may use [placeholders](REQUESTS.md#in-the-body-headers-and-query), encoded with `\|json`, `\|number`, `\|form` or `\|xml`. |
| `basic_auth`, `basic_auth_file` | — | Use one; see [Authentication](AUTHENTICATION.md). |
| `bearer_token`, `bearer_token_file` | — | Use one; not together with basic auth. |
| `forward_authorization`, `forward_headers` | off | See [Authentication](AUTHENTICATION.md). |
| `tls` | verify | See [Target requests](REQUESTS.md#tls). |
| `retry` | none | See [Target requests](REQUESTS.md#retries). |
| `max_response_bytes` | limit | Response size cap. |
| `follow_redirects`, `enable_http2` | off | See [Target requests](REQUESTS.md#redirects-and-http2). |
| `allowed_schemes` | `http`, `https` | Schemes a target may use. |

For `localfile`, `root` is required, and `path`, `max_age` and
`max_response_bytes` are optional, or `files`, `max_files` and
`max_total_bytes` to [read a whole directory](LOCALFILE.md#reading-a-directory);
its table is in [Local files](LOCALFILE.md#a-collector).

A key that belongs to a different type is an error rather than being ignored,
and the same holds for `/probe` parameters: a parameter that only another type
accepts gets a `400`. `localfile` accepts only `path`, `timeout` and
`param_<name>` — only `timeout` when it reads a directory — and its `target`
is optional.

#### Choosing request types at build time

Every build of the exporter carries every request type, and so does the
published image. A build can instead carry only the types it needs, which keeps
the other types' code, and the libraries only they use, out of the binary:

```sh
make build REQUEST_TYPES=http
docker build --build-arg REQUEST_TYPES=http -t exporter:http .
go build -tags "$(sh tools/request-type-tags.sh http)" .
```

`REQUEST_TYPES` is a comma-separated list of type names. Empty, the default,
means every type. A name that is not a request type fails the build, and so does
a selection that names none. The script turns the list into Go build tags —
`select_request_types` plus `request_type_<name>` per type — which is all
`go build -tags select_request_types,request_type_http` needs by hand.

A collector whose type the build left out stops the exporter at startup, saying
the type exists but this build does not include it, and which types it does. The
startup log line and the [dry run](#dry-run) report list the types the binary
carries, so a configuration can be checked against the build that will run it.
An http-only build leaves `localfile` out, and a build with
`REQUEST_TYPES=localfile` reads files and makes no HTTP requests to targets at
all.

### When a metric cannot be extracted

Each metric sets what happens when its value cannot be produced — the
expression matches nothing, the value is not a number, a label expression fails:

| `error_mode` | The failing metric | The rest of the probe | Logged |
| --- | --- | --- | --- |
| `ignore` | dropped | served — every metric that could be extracted, or an empty response if none could | no |
| `log` (default) | dropped | served, as with `ignore` | yes |
| `fail` | — | **not served**: the probe fails with a JSON error | yes |

```yaml
metrics:
  - name: service_up
    expression: .up
    error_mode: fail      # without this, the scrape means nothing
  - name: service_queue_depth
    expression: .queue.depth
    error_mode: log       # nice to have; carry on without it
```

`ignore` and `log` keep the scrape going, so a response carries whatever could
be extracted. That is the right choice for a metric that is useful but not
essential: one missing value does not cost you the others. When nothing at all
can be extracted, the probe still succeeds with an empty body.

`fail` is for a metric the scrape is meaningless without. A single failing rule
with `fail` fails the whole probe, even when every other metric was extracted
perfectly well, because the point of the mode is that a response is either
complete or an error — never quietly partial. The probe answers `502 Bad
Gateway` with a JSON body saying which collector, which rule and why:

```json
{"status":"error","stage":"metric","collector":"exchange_rates","metric":"exchange_rate","target":"https://api.frankfurter.dev/v1/latest","error":"metric \"exchange_rate\" value is missing"}
```

Prometheus only looks at the status, which marks the scrape down (`up` becomes
0); the body is for whoever runs the probe by hand. Credentials in the target
URL are redacted from it. A failed probe is never cached, so the next scrape
goes back to the target.

Modes are per metric, so a collector can mix them: a `fail` rule that succeeds
does not fail the probe because a `log` rule beside it did not.

`fail` takes precedence over the collector's `error_handling.on_transform_error`.
That policy governs the transform as a whole — a pre-script that raises, a
response the transform cannot read — while `error_mode: fail` is a statement
about one metric, so a lenient `on_transform_error: ignore` does not turn it
back into a partial success.

A metric that is optional — `required: false`, or a collector with
`error_handling.allow_missing_keys: true` — is not failing when its value is
absent, so no mode applies to it, `fail` included: it is simply left out.

### When a stage of the probe fails

`error_handling` covers the stages before any metric: fetching — reaching an
`http` target and its HTTP status, or reading a `localfile` file — decoding the
response, and the transform as a whole. It uses the same words as
`error_mode`:

```yaml
error_handling:
  on_fetch_error: fail        # the default for all three
  on_decode_error: fail
  on_transform_error: log
```

| Policy | The probe | Logged |
| --- | --- | --- |
| `fail` (default) | fails with `502 Bad Gateway` | yes, at error level |
| `log` | carries on without that stage's output | yes, at warning level |
| `ignore` | carries on without that stage's output | only at debug level |

A [scheduled target](OTLP.md#scheduled-targets) follows the same policies.
Under `fail` its scrape fails: `http_exporter_target_up` is `0` and, with
[`cache.stale_if_error`](#serving-the-last-good-result-when-the-target-fails),
the last good result is exported in its place. Under `log` and `ignore` the
scrape carries on as a probe does: the target is up, with nothing of the
collector's to export, and the scrape counts as a success. A metric rule with
`error_mode: fail` fails the scrape whatever `on_transform_error` says, on a
probe and a scheduled target alike.

`warn` is the older spelling of `log` here. It still works, but each use is
logged as deprecated at startup and on every reload, and listed by `--dry-run`;
change it to `log`.

### Checked when the configuration loads

Everything about a metric that can be known before a scrape is checked at
startup, on reload and by `--dry-run`, and an error names the collector, the
metric and the label:

- a metric name must be a valid Prometheus metric name, and not start with
  `__`;
- every expression must compile in its transform's language — jq and yq
  (including `items`, and undefined functions and variables), regular
  expressions, CSS selectors, XPath with the collector's namespaces, and a
  `prometheus` transform's patterns, `include` and `exclude`;
- a `regex` label must name a capture group the regex has;
- a `prometheus` transform's `rename` targets must be metric names, and its
  `labels` and `rename_labels` label names.

A CSS selector that does not compile used to match nothing, on every scrape,
without saying why; it is now refused when the configuration loads. The
expressions are compiled once, then, and every scrape reuses them.

A file that cannot be read as a configuration is refused in its own terms:
each error names the line and what was expected there, as in

```text
line 3: unknown key "requst" in a collector; line 7: "fast" is not a duration; write one such as 500ms, 30s or 1m30s
```

Every unknown key is refused, a scheduled target's `request` block
included, since a misspelt key would otherwise be ignored without a word.

### Editor support

[`config.schema.json`](../config.schema.json) is a JSON Schema of this file.
With the YAML extension for VS Code, or any editor that uses the YAML language
server, start a configuration with

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/eenchev/prometheus-universal-exporter/main/config.schema.json
```

and the editor completes keys, shows what each one does, and flags unknown keys
and values that are not allowed as you type. The example configurations start
with it.

`prometheus-universal-exporter --config.schema` prints the schema of the binary
you are running; its `request.type` values are the request types that binary
was built with. The schema describes the canonical spelling and is not the last
word: startup validation also checks what a schema cannot, such as that an
expression compiles.

On a scheduled target there is no HTTP response to carry an error. `fail` there
means the scrape exports nothing except `http_exporter_target_up` at 0, and
`log` and `ignore` export what could be extracted.

Every transform may define `transform.pre_script`. It runs once per scrape
after decoding and before metric extraction. The script receives the decoded
value as `data` and may mutate it or replace it by assigning to `data`.
HTML/XML pre-scripts receive raw document text, which is parsed again after the
script. Python transforms emit metrics with the `metric(...)` API.

A pre-script **must** leave its result in `data` — that is the variable the
exporter reads back. A script that computes a value under another name throws it
away: the transform then runs against the untouched response, so the collector
looks like it is extracting badly rather than configured wrongly. The exporter
therefore refuses to start when a pre-script never produces `data`, naming the
collector, and rejects such a configuration on reload with the previous one left
active. Replacing `data`, mutating it by key or attribute, augmenting it,
binding it as a loop or `with` target, and calling a method that mutates it all
count.

Reading `data` does not count, however much of it the script does. A method call
only counts when the method mutates: `data.update(...)` and
`data["rates"].append(...)` produce `data`, while `data.items()`, `data.get(...)`
and `data.copy()` are reads. This matters because the mistake usually looks
busy — a script that walks `data` thoroughly and assigns the result to a
neighbouring name:

```yaml
transform:
  pre_script: |
    import json
    data = json.loads(response.text)["payload"]   # produces data

    # result = json.loads(...)  would be rejected at startup: nothing reaches
    # the transform, because the exporter only reads back `data`.

    # So would this, despite reading data three times — it never produces it:
    #   reshaped = {
    #       "base": data["base"],
    #       "rates": [r for r in sorted(data["rates"].items())],
    #   }
```

The same startup check compiles every configured script, so a Python syntax
error in a pre-script or a `python` transform script is reported before the
exporter serves traffic rather than at the first scrape. All faults are listed
in one message. A `python` transform emits through `metric(...)` and is not
required to produce `data`.

The check needs the interpreter from `--python.path`, so a configuration that
contains any Python fails to start if that interpreter is unusable. A
configuration with no Python scripts never invokes one.

## Collector files

A long list of collectors is easier to own in several files — one per team, per
application, or per ConfigMap key. List them under `collector_files`:

```yaml
collector_files:
  - shared.yaml             # one file
  - collectors.d/*.yaml     # every file the pattern matches
web:
  self_metrics:
    verbose: true
collectors:                 # optional when collector_files supplies them
  - name: local_status
    ...
```

Each collector file holds a `collectors` list and nothing else:

```yaml
# collectors.d/payments.yaml
collectors:
  - name: payments_api
    request:
      type: http
      path: /status
    transform:
      type: jq
    metrics:
      - name: payments_queue_depth
        expression: .queue.depth
```

- **Paths.** An entry is a path or a glob pattern (`*`, `?`, `[...]`),
  resolved against the directory of the configuration file, not the working
  directory; an absolute path is used as it is. A path must exist; a pattern may
  match nothing, so an empty directory of collector files is fine. A directory
  is not a file: write `collectors.d/*.yaml`. A file matched by two entries is
  read once, and the configuration file itself is never read as a collector
  file, so `*.yaml` next to it is safe — although another YAML file in the same
  directory, such as a scheduled target file, would be read and refused, so a
  subdirectory or a naming pattern such as `collectors-*.yaml` is the better
  habit.
- **Only collectors.** Any other key in a collector file — `web`, `otlp`, a
  misspelt `colectors`, or a nested `collector_files` — is an error naming the
  file, the key and its line. The exporter-wide settings belong to the
  configuration alone, so a file of collectors can never change them. A file
  that is empty or has an empty `collectors` list is an error too.
- **Order.** The configuration's own collectors come first, then each entry's
  files in the order listed, a pattern's matches in file name order.
- **Validated together.** The merged collectors are validated exactly as if
  they had been written in the configuration: defaults, expressions, Python
  scripts, everything in
  [Checked when the configuration loads](#checked-when-the-configuration-loads).
  A configuration needs at least one collector across all of them; with
  `collector_files`, its own `collectors` key may be left out.
- **Unique names.** A collector name must be unique across the configuration
  and every collector file; see [Collectors](#collectors).
- **Environment variables.** With `--config.export-env`, `${NAME}` references
  are expanded in collector files as in the configuration.
- **Reloading.** With `--config.watch`, editing, adding or removing a collector
  file reloads the configuration, even though the configuration file itself did
  not change. A reload that would break any rule above is rejected and the last
  valid configuration stays active.
- **Checking.** `--dry-run` reads the collector files too and lists them under
  `details.collector_files` of its `config` entry.

`prometheus-universal-exporter --config.collector-file-schema` prints the JSON
Schema of a collector file, published as
[`collector-file.schema.json`](../collector-file.schema.json). Start a collector
file with

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/eenchev/prometheus-universal-exporter/main/collector-file.schema.json
```

and the editor checks it the way it checks the configuration, including that it
has no key but `collectors`.

## Environment variables

A configuration file is usually committed, and some of what belongs in it is
not: an internal hostname, a tenant identifier, a token. Pass
`--config.export-env` and the exporter substitutes `${NAME}` references from its
own environment before parsing the document:

```yaml
collectors:
  - name: example
    request:
      type: http
      path: ${API_PATH}
      headers:
        Authorization: Bearer ${API_TOKEN}
```

```sh
API_PATH=/v1/status API_TOKEN=... \
  prometheus-universal-exporter --config.file=config.yaml --config.export-env
```

It is off by default, and that default is the point. A configuration is full of
dollar signs that are not references — a regex metric rule, a jq expression, a
Python pre-script — and expanding them by default would rewrite an operator's
own text behind their back. With the flag off, nothing in the file means
anything but itself.

Even with the flag on, only the braced form is a reference. `$VAR` is left
exactly as written, so `expression: '\$([0-9]+)'` and shell-style text in a
pre-script keep working. Write `$$` for a literal dollar: `$${NOT_A_REFERENCE}`
survives as `${NOT_A_REFERENCE}`.

A reference to a variable that is not set is a startup error, not an empty
string:

```text
config.yaml: environment variable "API_TOKEN" not set; --config.export-env
requires every ${NAME} it finds to be defined
```

An empty substitution would produce a document that parses and is wrong — a
collector with no path, credentials that are silently blank — and the exporter
would serve it. Every missing name is listed at once. A variable that is set to
an empty string is a deliberate choice and substitutes normally.

Substitution is textual and happens before the YAML is parsed, so a reference
can supply any part of the document, not only a scalar. For the same reason a
value containing a line break is refused: it would end the line and turn the
rest into YAML rather than setting a long string.

The scheduled target document is read the same way — it carries the addresses
and credentials of the things being scraped, which is exactly the material worth
keeping out of a committed file — and `--config.watch` re-expands on every
reload, so a reload cannot quietly replace a working configuration with literal
references.

Environment references are fixed when the file is read. For a value that
changes per scrape — a tenant in the URL path — use a
[path parameter](REQUESTS.md#path-parameters), `{{param_tenant}}`, which the probe
fills in. The two syntaxes never overlap, and a path parameter's default can
itself be an environment reference.

## Response caching

A collector may cache its results with `ttl`, a Go duration such as `60s`,
`1m`, or `3h`:

```yaml
collectors:
  - name: expensive_api
    request:
      type: http
    cache:
      ttl: 60s
    limits:
      max_cache_entries: 1000
```

While a cached result is younger than `ttl`, a repeat of the same probe is
answered from memory and the target is not contacted again. Caching is off by
default; omitting `cache` or setting `0s` disables it, and a negative value is
rejected at startup. `cache` is always a mapping: `cache: 60s` is refused with
the form to write instead.

The cache is in-memory and local to the exporter process. Nothing is written to
disk, replicas do not share entries, and a restart empties it.

A stored result is only ever returned to an identical request. The cache key
covers the collector name and its full effective configuration, the `target`,
every `/probe` query parameter (`method`, `path`, `timeout`, `body`,
`insecure_skip_verify`, `follow_redirects`, `enable_http2`, `retry_attempts`,
`retry_backoff`, and any `header_<name>` entry), and every header forwarded to
the target, including a forwarded `Authorization` value. Presence and absence differ: a probe that sends
no credential, no forwarded header, or no TLS override cannot read an entry
stored by a probe that sent one, and two probes with different credentials never
share an entry. Because the collector definition is part of the key, a
configuration reload retires the entries cached under the previous definition.
Credentials loaded from files are covered through their configured paths, so a
rotated credential file applies to a cached request once the entry expires; keep
`cache` shorter than the rotation interval where that matters.

Only fully successful probes are cached; HTTP, decode, transform, and validation
failures are not. `limits.max_cache_entries` bounds each collector's live
entries and defaults to 1000, dropping expired entries first and then the ones
closest to expiry.

### Serving the last good result when the target fails

A target that is briefly unavailable — restarting, overloaded, behind a flaky
network — fails its probes, and every series of the collector disappears from
Prometheus for as long as it does, breaking graphs and firing absent-series
alerts. `stale_if_error` keeps each result for that much longer after `ttl`,
and answers a probe whose trip fails with the last successful result instead
of the error:

```yaml
    cache:
      ttl: 30s
      stale_if_error: 5m
```

Within 30 seconds of a successful probe, repeats are answered from memory as
above. After that the target is asked again; if it answers, the new result is
served and stored. If the trip fails — the target is down, times out, answers
an error status, its response cannot be decoded or transformed, or the
collector is at `max_concurrent_probes` — and the last good result of the same
probe is less than 5m30s old, that result is answered with `200`. Past that,
the failure is answered as usual. `ttl` may be `0s`: every probe then goes to
the target, and the last good result is kept only as a fallback.

A stale answer must never pass for a fresh one, so while `stale_if_error` is
set every answer of the collector carries two series of the exporter's own:

```text
http_exporter_result_stale 0
http_exporter_result_age_seconds 0
```

`http_exporter_result_stale` is `1` when the answer is the last good result
standing in for a failed trip. `http_exporter_result_age_seconds` is how long
ago the answered result was fetched from the target — `0` for a trip just
made, the entry's age for a cached answer. Watch them rather than `up`, which
stays `1` while stale results are answered: `http_exporter_result_stale == 1`
selects the targets currently bridged by an old result.

The failure is still logged and counted in the self-metrics as it would have
been, a warning says the last good result was answered and how old it is, and
`http_exporter_cache_stale_served_total` counts the stale answers. A stale
answer does not count in `http_exporter_scrape_success_total`. A collector's
rules must not produce either series name while `stale_if_error` is set.

Samples are served without timestamps, so Prometheus stores the old values at
the time of each scrape: a counter stays flat and a gauge repeats its last
value. Keep `stale_if_error` to the outages you would rather bridge than see —
minutes, not hours. A changed collector definition never serves a result stored
under the old one. A [scheduled target](OTLP.md#scheduled-targets) whose scrape
fails exports the last good result the same way, marked stale, while its
`http_exporter_target_up` stays `0`.

Cache activity is visible per collector in the self-metrics as
`http_exporter_cache_hits_total`, `http_exporter_cache_misses_total`,
`http_exporter_cache_stale_served_total` and `http_exporter_cache_entries`,
which counts stale entries too. A cache hit counts as a successful scrape and
cached metrics are still queued for OTLP export, while
`http_exporter_scrape_http_status_code` and
`http_exporter_scrape_response_bytes` continue to describe the last real target
request.

## Identical probes share one request

Several Prometheus replicas scraping the same targets on the same interval tend
to probe at the same moment. When a probe arrives while an identical one is
already waiting on the target, it does not send a request of its own: it waits
for the one in flight and gets an exact copy of its answer — the metrics, or
the same error. A slow endpoint is then asked once instead of once per replica,
and a rate-limited one is not pushed over its limit.

Identical means the same thing it does for the response cache: the same
collector definition, target, probe parameters and forwarded headers,
credentials included, so two probes that could get different answers never
share one. It needs no cache: the cache helps the probes that come after one
has finished, and this helps the ones that arrive while it is still running.
With a cache, the probes that share a request fill the cache once.

The shared request belongs to no single probe. If the probe that started it
goes away — its Prometheus timed out, say — the others still get their answer;
the request is cancelled only when every probe waiting on it has gone.

The trip to the target is counted once in the self-metrics, whatever number of
probes shared it: every probe still counts in `http_exporter_scrapes_total` and
`http_exporter_scrape_success_total`, and each one that shared another's request also
counts in `http_exporter_probes_coalesced_total`. A failure is logged once.

It is on by default. A collector whose target must see every probe as its own
request can turn it off:

```yaml
collectors:
  - name: counts_every_call
    coalesce: false
```

## Limiting concurrent probes

Identical probes share one request, but probes of different targets, or with
different parameters, each make their own — and all of a collector's targets
are often one backend. `max_concurrent_probes` bounds how many trips to its
targets a collector makes at once:

```yaml
collectors:
  - name: inventory_api
    max_concurrent_probes: 8   # optional; 32 when omitted or 0
```

A trip is the request or file read, with the decoding and transforms after it.
A probe that would exceed the limit is not queued: it is answered at once with
`503 Service Unavailable` — `collector inventory_api already has 8 probes to
its targets in progress, its max_concurrent_probes; this one was not sent` —
and counted in `http_exporter_probes_rejected_total`, while
`http_exporter_probes_in_flight` shows how close to the limit a collector runs.
Prometheus records the rejected scrape as `up` 0 with that reason, rather than
as a timeout.

Probes that make no trip take no slot: one answered from the
[response cache](#response-caching), and one that
[shares a request](#identical-probes-share-one-request) already in flight. A
[scheduled target](OTLP.md#scheduled-targets) shares its collector's limit, but
waits for a free slot within its scrape budget instead of failing at once,
since nothing is waiting on its answer; if none frees up in time, the scrape
fails in the `concurrency` stage. The default, 32, is well above what one
Prometheus usually sends a single backend at once; lower it for a backend that
cannot take many requests at a time, raise it for a collector with many slow
targets. A negative value is a configuration error.

## Probe deadlines

Prometheus says how long it will wait for each scrape, in the
`X-Prometheus-Scrape-Timeout-Seconds` header — the job's `scrape_timeout`, or a
monitor's `scrapeTimeout`. A probe that takes longer is abandoned, and all
Prometheus records is `up` 0 and a generic timeout; why the probe was slow never
reaches it.

So a probe gives itself that long, less `--probe.timeout-offset` (500ms by
default, as blackbox_exporter uses), and when the time runs out it stops and
answers with the reason while Prometheus is still waiting:

```text
collector legacy_text http failed: HTTP request failed: ... context deadline exceeded (the probe ran out of its 9.5s budget: Prometheus's scrape timeout less --probe.timeout-offset)
```

- The budget bounds the whole trip: the request or file read, decoding,
  transforms and Python scripts. The `timeout` probe parameter still bounds the
  request alone; whichever ends first stops the probe.
- An offset of half the scrape timeout or more would leave too little, so a
  probe always keeps at least half.
- Without the header — a probe from `curl`, or from anything other than
  Prometheus — nothing changes.
- A probe answered from the [response cache](#response-caching) needs no budget.
  Identical probes that [share one request](#identical-probes-share-one-request)
  share the budget of the probe that started it.
- The offset covers writing the answer and the network between the exporter
  and Prometheus. Raise it if Prometheus still times out first; `0` uses the
  whole scrape timeout. A negative value is a command-line error.

Scheduled targets are unaffected: their scrapes are bounded by `otlp.interval`
(see [OTLP](OTLP.md#scheduled-targets)).

## Watching the configuration

The exporter reads its configuration once at startup, and again when asked —
see [Reloading on demand](#reloading-on-demand). Pass `--config.watch` to
have it re-read the configuration file and, when one is configured, the
scheduled target file whenever either changes on disk:

```sh
prometheus-universal-exporter \
  --config.file=config.yaml \
  --config.watch \
  --config.watch-interval=60s
```

`--config.watch-interval` defaults to 60s and must be positive; passing zero or a
negative duration alongside `--config.watch` is a startup error rather than a
silently disabled watch. Without `--config.watch` no polling loop runs at all,
and configuration changes take effect on restart.

Changes are detected by modification time rather than filesystem events. That is
deliberate: Kubernetes republishes a mounted ConfigMap by atomically swapping
the `..data` symlink, which replaces the inode a file-level event watch is
attached to, so such a watch would stop firing after the first change.

The watch follows the [collector files](#collector-files) too: a collector file
edited, a new file matching a pattern, or a file removed triggers a reload.

The watch does not relax any reload rule. An invalid configuration, one that
would disable OTLP while scheduled targets are loaded, a collector name defined
twice, and a pre-script that stops producing `data` are all still rejected, with the last valid configuration
left active and the reason logged. `http_exporter_config_last_reload_successful`
then reads `0` until a reload succeeds, so a change that did not take can be
alerted on — see [Configuration reloads](SELF-METRICS.md#configuration-reloads).

## Reloading on demand

A tool that has just written the configuration can reload the exporter at once
instead of waiting for the watch, and find out whether the new configuration
was accepted:

```sh
kill -HUP "$(pidof prometheus-universal-exporter)"   # always available

curl -X POST http://exporter:8080/-/reload            # with --web.enable-lifecycle
```

Both reload the configuration, with its collector files, and the scheduled
target file, whether or not they changed, under exactly the rules the watch
follows. `POST` (or `PUT`) `/-/reload` answers:

| Status | Meaning |
| --- | --- |
| `200` | Every file was accepted and is in force. |
| `500` | A file was rejected; the body says why, and the previous configuration stays in force. |
| `403` | The exporter was started without `--web.enable-lifecycle`. |
| `405` | Any method other than `POST` or `PUT`. |

The endpoint is off unless `--web.enable-lifecycle` is passed, as in Prometheus,
and when `web.basic_auth` is configured it needs the same credentials as the
other endpoints. `SIGHUP` needs no flag. Either way the reload is logged with
what triggered it — `"trigger":"http"`, `"sighup"` or `"watch"` — and counted
in the [reload self-metrics](SELF-METRICS.md#configuration-reloads). Reloads
from different triggers never interleave: one runs at a time.

Whatever the trigger, the exporter's state follows the new configuration: a
removed collector's [self-metrics](SELF-METRICS.md#collector-metrics) stop,
and what was kept about it is dropped, and a changed collector's cached results
are dropped.

## Readiness

`/health` answers `200` for as long as the process runs. `/ready` answers `200`
when the exporter is doing what it was configured to do, and `503` when it is
not, with one `not ready:` line per reason:

- The last reload of the configuration, or of the scheduled target file, was
  rejected. The previous configuration is still in force and still answers
  probes, but it is not the one that was deployed. Ready again once a reload
  is accepted.
- With `otlp.unready_after_failures` set, that many OTLP exports to the
  current endpoint failed in a row, retries included (see
  [Delivery](OTLP.md#delivery)). Ready again once an export gets through. It
  is off by default, since an exporter whose exports fail still answers
  probes.
- The exporter received `SIGTERM` or `SIGINT` and is waiting out
  `--web.shutdown-delay` before it stops; see [Shutting down](#shutting-down).
  It never becomes ready again.

Neither endpoint needs credentials, so the reasons never include an error's
text; the log and the [self-metrics](SELF-METRICS.md) have the details.

In Kubernetes, a pod that is not ready is taken out of its Service, and
Prometheus stops probing through it. With the chart's defaults a configuration
change rolls the Deployment, and a new pod whose configuration is rejected never
starts, so the first reason arises only with `server.watchConfig`,
`server.enableLifecycle` or a `SIGHUP`. Setting `otlp.unready_after_failures`
takes a pod out of its Service while its OTLP endpoint fails, which also stops
Prometheus probing through it; set it only where OTLP delivery is the pod's
job.

## Shutting down

On `SIGTERM` or `SIGINT` the exporter first waits `--web.shutdown-delay`, 0 by
default, still answering probes but with `/ready` answering `503` (`not ready:
the exporter is shutting down`), so a load balancer or a Kubernetes Service
takes it out of rotation before it stops listening. Kubernetes takes a few
seconds to remove a terminating pod from its Service; without the delay,
probes that arrive in that gap are refused and Prometheus records failed
scrapes on every rollout. The Helm chart sets it to 5 seconds.

Then it stops accepting connections, lets the
probes in progress finish for up to `--web.shutdown-timeout` (5 seconds by
default), makes the [last OTLP export](OTLP.md#delivery) when OTLP is enabled,
and exits `0`. It logs that it is shutting down, and, when the timeout runs
out with probes still in progress, that it closed them — Prometheus records
those as failed scrapes. Keep the timeout at least as long as the longest
scrape timeout of the monitors that probe the exporter, so a rollout does not
cut probes off:

```sh
prometheus-universal-exporter --web.shutdown-delay=5s --web.shutdown-timeout=30s
```

In Kubernetes the pod must also be allowed to run that long: its
`terminationGracePeriodSeconds`, 30 by default, has to cover the delay, the
timeout and the last OTLP export. The Helm chart's `server.shutdownDelay` and
`server.shutdownTimeout` set the flags and raise the grace period to match. A second `SIGTERM` or `SIGINT` during that time
ends the process at once — the second Ctrl-C of an impatient operator, or a
supervisor that signals twice — without waiting for the probes or the export.

## Sizes

Every setting that is a number of bytes — `max_response_bytes`,
`limits.max_output_bytes`, `max_total_bytes` — takes either a number of bytes
or a number with a unit:

| Written | Bytes |
| --- | --- |
| `1048576` | 1048576 |
| `512KiB`, `512 KiB` | 524288 |
| `10MB` | 10000000 |
| `64MiB` | 67108864 |
| `1.5GiB` | 1610612736 |

`kB`, `MB`, `GB` and `TB` are powers of 1000, `KiB`, `MiB`, `GiB` and `TiB`
powers of 1024, and `B` or no unit is bytes. The unit is case insensitive, and
a fraction is rounded down to whole bytes. Anything else, such as `lots` or
`-1`, fails to load, naming the value.

## Dry run

`--dry-run` answers "would this start?" without starting anything. It runs the
same validation startup runs, prints a JSON report on stdout, and exits:

| Exit status | Meaning |
| --- | --- |
| `0` | Every check passed; the exporter would start with these files and flags. |
| `1` | At least one check failed; the report says which, and why. |
| `2` | The command line itself could not be parsed, so nothing was checked. |

```sh
prometheus-universal-exporter --dry-run --config.file=config.yaml
prometheus-universal-exporter --dry-run \
  --config.file=config.otlp.yaml --otlp.targets-file=targets.yaml
```

It checks everything startup checks, including that every expression compiles
(see [Checked when the configuration loads](#checked-when-the-configuration-loads)).
A deprecated spelling does not fail the check; it is listed under
`details.deprecations` of the `config` entry and logged. The
[collector files](#collector-files) the configuration read are listed under
`details.collector_files`, and a collector name defined twice fails the check.

It takes the same flags a real start does, and they matter: `--config.file` and
`--otlp.targets-file` choose what is checked, `--config.export-env` decides
whether `${NAME}` references are expanded — so a check run where a referenced
variable is not set fails, exactly as startup would — `--python.path` is the
interpreter the Python scripts are compiled with, and `--config.watch` with
`--config.watch-interval` are checked when the watch is on. It never binds a
port, starts a watch or contacts a target.

The report lists one entry per startup step, in the order startup runs them:

```json
{
  "status": "failed",
  "request_types": ["http"],
  "checks": [
    {
      "check": "config",
      "file": "config.yaml",
      "status": "ok",
      "details": {"collectors": ["app_json", "legacy_text"], "otlp_enabled": false, "config_export_env": false}
    },
    {
      "check": "python_scripts",
      "file": "config.yaml",
      "status": "failed",
      "errors": ["collector app_json pre_script must produce its result in a variable named 'data'; assign to data or mutate it in place. ..."]
    },
    {
      "check": "targets",
      "file": "targets.yaml",
      "status": "failed",
      "errors": ["scheduled targets require OTLP export; set otlp.enabled: true or remove the target file"],
      "details": {"targets": ["legacy_eu", "legacy_us"]}
    }
  ]
}
```

| `check` | Present | What it validates |
| --- | --- | --- |
| `config` | always | The configuration file and its collector files load and are valid, and no collector name is defined twice. |
| `python_scripts` | always | Every pre-script and `python` transform compiles, and every pre-script produces `data`. Each faulty script is its own entry in `errors`. A configuration without Python needs no interpreter and passes with `"scripts": 0`. |
| `config_watch` | with `--config.watch` | `--config.watch-interval` is positive. |
| `targets` | with `--otlp.targets-file` | The target file is valid on its own, and against the configuration: every collector exists and OTLP export is enabled. |

Each entry's `status` is `ok`, `failed` with `errors`, or `skipped` with a
`reason` when it depends on a step that failed: the Python scripts cannot be read
from a configuration that did not load, and a target file that is valid on its
own cannot be paired with it. A skipped check counts as not passing, and the
report still shows it, so it is never shorter because something went wrong. The
top-level `status` is `ok` only when every entry is. `request_types` lists the
request types the binary was built with (see
[Choosing request types at build time](#choosing-request-types-at-build-time)).

stderr carries one JSON log line per check — `configuration check passed` at
INFO, `skipped` at WARN, `failed` at ERROR with its errors — and a final
`configuration check complete`, so a job's log reads like the exporter's own.
`--log.level` quietens the log; it never changes the report. Pipe the report
through `jq` in CI:

```sh
prometheus-universal-exporter --dry-run --config.file=config.yaml \
  | jq -e '.status == "ok"'
```

The container image runs the same way, with the files mounted:

```sh
docker run --rm -v "$PWD:/config:ro" \
  ghcr.io/eenchev/prometheus-universal-exporter:latest \
  --dry-run --config.file=/config/config.yaml
```

In Kubernetes, run it as a Job or an init container rather than as the exporter
itself: a pod started with `--dry-run` validates and exits instead of serving,
which is why the Helm chart rejects it in `extraArgs`.

## Related pages

- [PYTHON.md](PYTHON.md) — the Python transform and pre-script API.
- [REQUESTS.md](REQUESTS.md) — redirects, HTTP/2, retries, TLS and per-scrape overrides.
- [AUTHENTICATION.md](AUTHENTICATION.md) — credentials for the target and for the exporter itself.
- [SELF-METRICS.md](SELF-METRICS.md) — the exporter's own metrics.
- [OTLP.md](OTLP.md) — OTLP export and scheduled targets.
