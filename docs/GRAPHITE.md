# Graphite

The `graphite` request type asks a Graphite render API — graphite-web,
carbonapi, or anything that answers `/render` the same way — for the series of
one or more Graphite expressions, and the `graphite` decoder turns what comes
back into a document that jq, yq or a Python script maps to Prometheus
metrics, with the same rules, limits, caching and error policies as any other
collector.

The exporter pulls, and so does this: it asks Graphite what it already holds.
It does not accept carbon's pushed lines on a port. A job that writes carbon
lines to a file can be read with a [`localfile` collector](#carbon-lines-from-a-file)
instead.

## A collector

```yaml
collectors:
  - name: graphite_app
    request:
      type: graphite
      targets:
        - app.*.requests.count
        - "seriesByTag('name=cpu.load', 'env={{param_env:prod}}')"
    response:
      graphite:
        max_age: 5m
    transform:
      type: jq
    metrics:
      - name: app_requests
        description: Requests per interval, as statsd flushed them
        items: .series[] | select(.segments[0] == "app")
        expression: .value
        required: false
        labels:
          - name: host
            expression: .segments[1]
      - name: cpu_load
        items: .series[] | select(.path == "cpu.load")
        expression: .value
        required: false
        labels:
          - name: env
            expression: .tags.env
```

The probe's `target` is the Graphite server, as an `http` collector's target
is its host:

```text
/probe?target=graphite.internal:8080&collector=graphite_app&param_env=staging
  -> GET http://graphite.internal:8080/render?target=app.*.requests.count
         &target=seriesByTag('name=cpu.load','env=staging')&from=-15min&until=now&format=json
```

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `graphite`. |
| `targets` | — | **Required.** The Graphite expressions to render, each sent as a `target` parameter. May use [placeholders](#placeholders). |
| `from` | `-15min` | The start of the window, as Graphite writes a time: `-1h`, `-30min`, Unix seconds, `18:00_20260924`. See [The window](#the-window). |
| `until` | `now` | The end of the window. |
| `path` | `/render` | Joined onto the target, for a Graphite served under a prefix or behind a proxy such as Grafana's. |
| `query` | — | Further render parameters, such as [`maxDataPoints`](#the-window). `target`, `from`, `until` and `format` are the type's own and refused here. |
| `headers`, credentials, `tls`, `retry`, `max_response_bytes`, `follow_redirects`, `enable_http2`, `allowed_schemes`, `forward_authorization`, `forward_headers` | as for `http` | The request is an `http` request in everything but its URL; see [Target requests](REQUESTS.md) and [Authentication](AUTHENTICATION.md). |

The request is a `GET`; `method` and `body` are refused, as is any key of
another request type. When the expressions together are too long for a URL
every proxy passes — over 2 KiB encoded — the same parameters go as a form
with `POST` instead, which graphite-web and carbonapi read alike; it is
retried as a `GET` would be, since it only reads, and the verbose
`http_method` label says `POST`. Which one a collector uses depends only on
its expressions, so it never changes from one scrape to the next.

Every expression is checked when the configuration loads — not empty, its
quotes closed, its brackets balanced, and listed once — so a typo stops the
exporter instead of failing every scrape. Inside quotes, `\` escapes the next
character, as in Graphite: `'it\'s'` is one string. Graphite's functions are
not checked: graphite-web and carbonapi differ, and an unknown function is the
render API's to refuse, which fails the probe with Graphite's own message.

The answer must be render JSON, a list of series. Anything else — the login
page of a proxy in front of Graphite, an error in JSON — fails the probe
saying Graphite did not answer with render JSON, and how its answer starts.

The same `/probe` parameters as `http` apply — `path`, `timeout`,
`insecure_skip_verify`, `follow_redirects`, `enable_http2`, `retry_attempts`,
`retry_backoff`, `header_<name>` and `param_<name>` — and `from` and `until`,
which replace the window for that probe: `params: {from: [-1h]}` on a
monitor. `method` and `body` get a `400`, and so do `from` and `until` for a
collector of another type.

## Placeholders

An expression may hold [`{{param_<name>}}` placeholders](REQUESTS.md#path-parameters),
filled from the probe's `param_<name>`, or the default after the colon:
`{{param_env:prod}}`. A placeholder with no default that the probe does not
fill fails the probe with `400` before Graphite is asked, as does a
`param_<name>` that no placeholder uses.

A value lands inside a Graphite expression, which has no escaping, so it may
hold only what a path node or a tag value is made of: letters, digits and
`_ - . : @ % + ~`. Anything else — a quote, a comma, a bracket, a `*` — is
refused with `400`, since it could change the expression rather than fill a
value in it: `x'),sumSeries('y` would otherwise add a series nobody
configured.

## The series document

Whatever Graphite answers is decoded into one document:

```json
{"series": [
  {"path": "app.web01.requests.count",
   "segments": ["app", "web01", "requests", "count"],
   "tags": {"name": "app.web01.requests.count"},
   "value": 42,
   "time": 1727000000,
   "points": [[40, 1726999940], [42, 1727000000]]}
]}
```

| Field | What it is |
| --- | --- |
| `path` | The series' name as Graphite answered it, without tags: its path, or what a function such as `alias()` named it. |
| `segments` | `path` split on dots, so a rule can label by position: `.segments[1]`. |
| `tags` | The series' tags, as the render API gives them, or as a tagged name (`cpu.load;env=prod`) writes them. Always holds `name`, the path when the render API's tags leave it out. |
| `value` | The points reduced to one value by [`response.graphite.value`](#which-value): the newest point by default. |
| `time` | The newest point's time, in Unix seconds. Only for rules to read: samples are exported without a timestamp, like every collector's. |
| `points` | Every point with a value, `[value, time]`, oldest first, for a rule that wants more than `value`. |

A point without a value — `null`, which Graphite writes for an interval
nothing was written in — is left out, and a series left with no point at all
is left out of the document, rather than failing a rule on every scrape as a
series without a value. So is a series older than
[`max_age`](#which-value), and a series answered twice — the same path, tags
and points, because two expressions both matched it. A series with the same
path but other points, from different functions, is kept: give it a
different name with `alias()` so the rules can tell the two apart.

What is left out is counted in the collector's
[self-metrics](SELF-METRICS.md), `http_exporter_decoder_series_left_out_total`,
and logged at debug level with how many for each reason, so a metric that is
missing because its series was left out can be told from one nobody wrote.

## Mapping series to metrics

Map the series with [`items`](CONFIGURATION.md#metrics-per-item): each rule
selects the series it is about and reads `.value`, with labels from the
segments or the tags, as the example above does. A series no rule selects is
not exported.

Set `required: false` on these rules, as the examples do. When Graphite has
nothing for a rule's series — a host that stopped writing, every series older
than `max_age` — its `items` select nothing, and a required rule logs a
missing value on every scrape. With `required: false` the rule produces no
series, which is what Prometheus should see: the series is gone. Rules are `gauge` unless they say otherwise; most Graphite series
fed by statsd are per-interval values rather than running totals, so keep
`type: counter` for those that only ever grow.

A Python transform reads the same document from `data`:

```yaml
collectors:
  - name: graphite_peaks
    request:
      type: graphite
      targets: [app.*.requests.count]
      from: -1h
    transform:
      type: python
      script: |
        for s in data["series"]:
            peak = max(value for value, _ in s["points"])
            metric(name="app_requests_peak", value=peak, labels={"host": s["segments"][1]})
```

For an answer the series document does not suit, set `decoder.type: json`, and
the render API's JSON reaches the rules as Graphite sent it.

## Which value

`response.graphite` sets how the decoder reduces a series:

```yaml
response:
  graphite:
    value: max    # last (default), max, min, avg or sum over the window's points
    max_age: 5m   # leave out a series whose newest point is older
```

| Key | Default | Notes |
| --- | --- | --- |
| `value` | `last` | `last` is the newest point, what a dashboard shows. `max`, `min`, `avg` and `sum` reduce every point in the window. |
| `max_age` | off | A series whose newest point is older is left out. Without it, a host that stopped writing keeps exporting its last value for as long as the window reaches back. |
| `invalid_lines` | `fail` | Carbon lines only: `fail` fails the decode on a line that cannot be read, naming it; `skip` [leaves it out](#carbon-lines-from-a-file). |

`response.graphite` applies to the `graphite` decoder, and a collector whose
`decoder.type` names another is refused at startup; with `auto` it applies to
the responses read as Graphite series. The `graphite` decoder is for `jq`,
`yq` and `python` transforms, which read the document; any other transform
with it is refused too.

## The window

The window, `-15min` to `now` by default, must hold at least one point with a
value of every series you map. Graphite answers `null` for an interval nothing
was written in yet, and for the current, unfinished interval of a series
stored at a coarse resolution, so a window of a few minutes over a series
stored every five or ten minutes, or one a statsd flush writes late, can hold
nothing but nulls. Fifteen minutes covers one-minute and five-minute
resolutions; for coarser ones set `from` to a few intervals back. The window
decides what `value` can reach, and [`max_age`](#which-value) how old the
newest point may be, so a wide window does not keep a stale series alive.

A long window returns every point in it, which is wasted on `value: last` and
can reach `max_response_bytes` for many series. `maxDataPoints` asks Graphite
to consolidate each series to at most that many points:

```yaml
request:
  type: graphite
  targets: [app.*.requests.count]
  from: -24h
  query:
    maxDataPoints: "24"
```

Consolidation averages each bucket by default — Graphite's
`consolidateBy()` changes that — so it changes what `points`, and `value`
other than `last`, see. It is not set unless you set it.

## Carbon lines from a file

The `graphite` decoder also reads carbon's plaintext lines, so a job that
writes them to a file — instead of to carbon, or as well — can be read with a
[`localfile`](LOCALFILE.md) collector:

```text
backup.web01.duration_seconds 42 1727000000
backup.web02.duration_seconds 17 1727000000
queue.depth;env=prod 7 1727000000
```

```yaml
collectors:
  - name: backups
    request:
      type: localfile
      root: /var/lib/metrics
      path: backup.graphite
    response:
      graphite:
        max_age: 26h
    transform:
      type: jq
    metrics:
      - name: backup_duration_seconds
        items: .series[] | select(.segments[0] == "backup")
        expression: .value
        required: false
        labels:
          - name: host
            expression: .segments[1]
```

A file ending in `.graphite` or `.carbon` is read this way with
`decoder.type` `auto`, and so is a file with another extension whose every
line is a carbon line — a path, a value and a timestamp, some path with a dot
or a tag. `decoder.type: graphite` reads any file this way.

Each line is `<path> <value> <timestamp>`, the timestamp in Unix seconds. A
line without one, or with `-1`, is taken as written now, as carbon takes it. A
timestamp in milliseconds — anything from 10¹¹, the year 5138 in seconds — is
refused, naming the line, rather than read as a time thousands of years away
that no `max_age` would ever call stale.
Lines of one path, and the same tags in any order, are one series with a point
per line, so a file that is appended to gives `last` its newest line. Blank
lines and lines starting with `#` are skipped; any other line that is not a
carbon line fails the scrape, naming the line. `NaN` and infinite values are
left out, as Graphite's `null`.

A file a job appends to can end in a line the job is still writing, and one
torn line then fails the whole file. `response.graphite.invalid_lines: skip`
leaves such lines out instead and reads the rest. Skipped lines are counted
in `http_exporter_decoder_lines_skipped_total` and logged at warn level with
the first of them — once, and then sparingly while they keep being skipped,
as a failing target is — and a read that skips none again logs that it did.

## Static targets

A [static target](STATIC-TARGETS.md) of a `graphite` collector is scraped by
the exporter on its interval. Its `target` is the Graphite server's URL, and
under `request` it may set everything a probe can, and its own `targets`,
`from` and `until`, which replace the collector's:

```yaml
interval: 1m
targets:
  - name: staging
    collector: graphite_app
    target: http://graphite.internal:8080
    params:
      param_env: staging
  - name: databases
    collector: graphite_app
    target: http://graphite.internal:8080
    request:
      targets: [db.*.connections]
      from: -15min
```

A target's own `targets` are sent as written, so they cannot hold
placeholders; the collector's are filled from the target's `params`. With
[caching](CONFIGURATION.md#response-caching), each target's own `targets`,
`from` and `until` are part of its cache key, so targets asking one server for
different series never share a result.

## Building without it

`graphite` is a request type like the others, carried by every build and the
published image. A build with
[`REQUEST_TYPES`](CONFIGURATION.md#choosing-request-types-at-build-time) that
leaves it out cannot load a `graphite` collector; the `graphite` decoder is
always there, so carbon lines in a file can be read by a build of `localfile`
alone.
