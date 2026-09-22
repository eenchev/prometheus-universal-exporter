# Configuration

The exporter reads one YAML document, named by `--config.file`. It declares the
collectors — how to call a target, how to read what comes back, and which
metrics to publish — and, optionally, the exporter's own settings under `web`
and `otlp`. Target URLs are deliberately not part of it: Prometheus supplies
each one per scrape.

`config.example.yaml` in the repository root is a complete working document to
start from. This page is the reference for what it may contain.

## Collectors

Collectors contain request, response, decoder, transformation, error-policy, and limit settings. Target URLs are deliberately not stored in configuration.

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

All non-Python transforms use the same collector-level metric declaration. Each
entry has `name`, `description`, `type`, `labels`, and a transform-specific
`expression`. The only allowed metric types are `gauge`, `counter`,
`histogram`, `summary`, and `untyped`:

```yaml
collectors:
  - name: app_json
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
            type: expression
            expression: .environment
```

The expression and label values are interpreted by the selected transform:

- `jq`/`yq`: jq expressions evaluated against decoded data.
- `regex`: a RE2 expression; the first capture group is the numeric value and
  labels map to capture-group numbers or names.
- `csv`: the expression is the numeric column name and labels map to column
  names.
- `css`: the expression selects HTML nodes whose text is numeric; labels are
  selectors relative to each selected node.
- `xpath`: the expression selects XML/HTML nodes whose text is numeric; labels
  are relative XPath expressions or `@attribute` selectors.
- `prometheus`: the expression matches source metric names; it can remap the
  name, description, type, and selected labels.

CSS remains available specifically for HTML tables and HTML status pages; it is
not used for CSV.

Metric labels are explicit typed entries. Use `type: expression` when the
value comes from the response, or `type: string` with `value` for a literal:

```yaml
labels:
  - name: server
    type: expression
    expression: server       # CSV column for the current row
  - name: environment
    type: string
    value: production
```

Label expressions use the same transform-specific language as the metric
expression. For CSV, each row produces a metric and `expression: server`
selects that row's `server` column.

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

## Environment variables

A configuration file is usually committed, and some of what belongs in it is
not: an internal hostname, a tenant identifier, a token. Pass
`--config.export-env` and the exporter substitutes `${NAME}` references from its
own environment before parsing the document:

```yaml
collectors:
  - name: example
    request:
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

A collector may set `cache` to a Go duration such as `60s`, `1m`, or `3h`:

```yaml
collectors:
  - name: expensive_api
    cache: 60s
    limits:
      max_cache_entries: 1000
```

While a cached result is younger than that interval, a repeat of the same probe
is answered from memory and the target is not contacted again. Caching is off by
default; omitting `cache` or setting `0s` disables it, and a negative value is
rejected at startup.

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

Cache activity is visible per collector in the self-metrics as
`http_exporter_cache_hits_total`, `http_exporter_cache_misses_total`, and
`http_exporter_cache_entries`. A cache hit counts as a successful scrape and
cached metrics are still queued for OTLP export, while
`http_exporter_scrape_http_status_code` and
`http_exporter_scrape_response_bytes` continue to describe the last real target
request.

## Watching the configuration

The exporter reads its configuration once at startup. Pass `--config.watch` to
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

The watch does not relax any reload rule. An invalid configuration, one that
would disable OTLP while scheduled targets are loaded, and a pre-script that
stops producing `data` are all still rejected, with the last valid configuration
left active and the reason logged.

## Related pages

- [PYTHON.md](PYTHON.md) — the Python transform and pre-script API.
- [REQUESTS.md](REQUESTS.md) — redirects, HTTP/2, retries, TLS and per-scrape overrides.
- [AUTHENTICATION.md](AUTHENTICATION.md) — credentials for the target and for the exporter itself.
- [SELF-METRICS.md](SELF-METRICS.md) — the exporter's own metrics.
- [OTLP.md](OTLP.md) — OTLP export and scheduled targets.
