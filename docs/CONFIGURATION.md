# Configuration

The exporter reads one YAML document, named by `--config.file`, and any
[collector files](#collector-files) it lists. The document declares the
collectors: how to call a target, how to read the answer, and which metrics to
publish. Target URLs are deliberately not part of it; Prometheus supplies one
per scrape.

The top-level keys are:

- `collectors` and `collector_files`: the collectors.
- [`defaults`](#defaults-for-the-collectors-of-a-file): keys the collectors
  of the file take when they do not set them.
- [`rate_limits`](REQUESTS.md#rate-limits-per-host): rate limits per host,
  shared by the requests of every collector. It is a top-level key, not part
  of a collector's `limits` (which bound one collector's scrape), because its
  entries belong to no collector and bound the requests of all of them
  together.
- [`secret_stores`](AUTHENTICATION.md#secret-stores): the stores a credential
  can be read from in place of a file.
- `web`: the exporter's own settings.
- `otlp`: export over [OTLP](OTLP.md) of probe results, the exporter's own
  metrics and, with `otlp.logs`, its own log.

Two blocks under `otlp` add to the export:

- [`otlp.traces`](OTLP.md#traces-of-slow-requests) exports a trace of each
  slow probe and static target scrape. It can also export each failed one
  (`errors`), each one whose caller is sampled (`parent_based`), and a
  `sample_ratio` share of the rest, at most `max_per_second` per second
  ([Which traces are exported](OTLP.md#which-traces-are-exported)). Traces are
  linked to the log records and the self-metrics' exemplars of the same
  probe and, with `propagate`, sent to the targets as their `traceparent`
  ([Linking metrics, logs and traces](OTLP.md#linking-metrics-logs-and-traces)).
- [`otlp.exporter_instance`](OTLP.md#the-exporter-instance) names the replica
  that sends the data on every resource.

`configs/config.example.yaml` is a complete working document to start from.
This page is the reference for what the document may contain.

## Contents

- [Collectors](#collectors)
  - [No decoder](#no-decoder)
  - [NDJSON, logfmt, TOML and INI](#ndjson-logfmt-toml-and-ini)
  - [Binary answers](#binary-answers)
  - [InfluxDB line protocol](#influxdb-line-protocol)
  - [Querying InfluxDB](#querying-influxdb)
  - [Protobuf responses](#protobuf-responses)
  - [Character encodings](#character-encodings)
  - [Reading CSV: what to expect](#reading-csv-what-to-expect)
  - [XML namespaces](#xml-namespaces)
  - [Collector-wide labels](#collector-wide-labels)
  - [Prefixing a collector's metrics](#prefixing-a-collectors-metrics)
  - [UTF-8 names](#utf-8-names)
  - [Metrics per item](#metrics-per-item)
  - [Labels from an object](#labels-from-an-object)
  - [Long label values](#long-label-values)
  - [Long label names](#long-label-names)
  - [Turning a status into metrics](#turning-a-status-into-metrics)
  - [Mapping text to values and scaling them](#mapping-text-to-values-and-scaling-them)
  - [Reading a date or a time as the value](#reading-a-date-or-a-time-as-the-value)
  - [Value-parsing helpers](#value-parsing-helpers)
  - [Programs written for jq](#programs-written-for-jq)
  - [Histograms from a list of values](#histograms-from-a-list-of-values)
  - [Derived series](#derived-series)
  - [Conditional metrics and labels](#conditional-metrics-and-labels)
  - [Request types](#request-types)
  - [When a metric cannot be extracted](#when-a-metric-cannot-be-extracted)
  - [When a stage of the probe fails](#when-a-stage-of-the-probe-fails)
  - [What a failed probe's caller is told](#what-a-failed-probes-caller-is-told)
  - [Reporting failed probes as metrics](#reporting-failed-probes-as-metrics)
  - [Checked when the configuration loads](#checked-when-the-configuration-loads)
  - [Two rules that are the same rule](#two-rules-that-are-the-same-rule)
  - [A name and a pattern that matches it](#a-name-and-a-pattern-that-matches-it)
  - [Editor support](#editor-support)
- [Reusing settings with YAML anchors](#reusing-settings-with-yaml-anchors)
- [Collector files](#collector-files)
- [Defaults for the collectors of a file](#defaults-for-the-collectors-of-a-file)
- [Environment variables](#environment-variables)
- [OpenMetrics](#openmetrics)
- [Response caching](#response-caching)
  - [Serving the last good result when the target fails](#serving-the-last-good-result-when-the-target-fails)
  - [Revalidating expired responses](#revalidating-expired-responses)
- [Identical probes share one request](#identical-probes-share-one-request)
- [Limiting concurrent probes](#limiting-concurrent-probes)
- [Backing off from failing targets](#backing-off-from-failing-targets)
- [Memory](#memory)
- [Probe deadlines](#probe-deadlines)
- [Watching the configuration](#watching-the-configuration)
  - [Descriptor files](#descriptor-files)
  - [Rejected reloads](#rejected-reloads)
- [Reloading on demand](#reloading-on-demand)
  - [What a reload changed](#what-a-reload-changed)
- [Metric lint](#metric-lint)
  - [counter-total](#counter-total)
  - [unit-suffix](#unit-suffix)
  - [unbounded-label](#unbounded-label)
  - [name-clash](#name-clash)
  - [Silencing a rule](#silencing-a-rule)
- [Debugging collectors](#debugging-collectors)
  - [The debug page](#the-debug-page)
  - [Dangerous debug mode](#dangerous-debug-mode)
  - [Debug probes](#debug-probes)
- [Readiness](#readiness)
  - [Health checks](#health-checks)
- [Shutting down](#shutting-down)
- [Sizes](#sizes)
- [Dry run](#dry-run)
- [Configuration from the target allocator](#configuration-from-the-target-allocator)
- [Related pages](#related-pages)

## Collectors

A collector holds request, response, decoder, transform, error-policy and limit settings. Target URLs are deliberately not stored in the configuration.

A collector's `name` is what a probe asks for, so it must be unique across the
configuration's `collectors` and every [collector file](#collector-files). A
duplicate name stops the exporter at startup, fails `--dry-run` and rejects a
reload, with an error that names both places where it was defined:

```text
duplicate collector "app_json": defined in /etc/exporter/config.yaml and in /etc/exporter/collectors.d/payments.yaml
```

`decoder.type` selects how the response is decoded. It is optional and
defaults to `auto`. With `auto`, the decoder follows from the transform when
the transform implies one:

- `regex` reads text, `csv` reads CSV, `css` reads HTML, and `prometheus`
  reads Prometheus exposition.
- A `graphite` collector uses the `graphite` decoder whatever its transform,
  and a `grpc` collector uses `json`, because a call is answered as JSON.

The other transforms (jq, yq, XPath, Python) decode each response by what it
declares itself to be: an `http` response by its `Content-Type` header, and a
`localfile` file by its extension. When neither says, the content decides:

- an HTML page by its doctype or `<html>` element, in any letter case, within
  the first KiB and after any whitespace, comments and XML declaration;
- other markup as XML;
- JSON by its opening bracket;
- Prometheus text by its `# TYPE` or `# HELP` lines;
- carbon lines (`<path> <value> <timestamp>`, with a dot in some path) as
  Graphite series;
- anything else as text.

If the selected transform cannot use the decoded response, the probe fails
with a clear mapping error.

With the decoder left to the response, these types and extensions select it:

| `Content-Type` | Extension of a `localfile` file | Decoder |
| --- | --- | --- |
| `application/json`, any type ending in `+json` | `.json` | `json` |
| `application/yaml`, `application/x-yaml`, `text/yaml` | `.yaml`, `.yml` | `yaml` |
| `application/xml`, `text/xml` | `.xml` | `xml` |
| `text/csv` | `.csv` | `csv` |
| `text/html` | `.html`, `.htm` | `html` |
| `application/openmetrics-text`, `text/plain; version=0.0.4` | `.prom` | `prometheus` |
| `text/x-graphite` | `.graphite`, `.carbon` | `graphite` |
| `application/x-ndjson`, `application/jsonl` | `.ndjson`, `.jsonl` | `ndjson` |
| `application/toml` | `.toml` | `toml` |
| any other, and none | any other, and none | by the content |

The type is read without its parameters and without regard to letter case, as
is the extension.

Some formats are never detected:

- **NDJSON, logfmt, TOML, INI and InfluxDB line protocol are not recognised by
  content.** An NDJSON body sent without its type is one JSON value followed
  by more, which is not JSON, so it is read as text. logfmt, INI and InfluxDB
  line protocol have no media type, and `.ini` selects no decoder. Those
  three are therefore read only with `decoder.type: logfmt`, `ini` or
  `influx`.
- **Only `text/csv` and `.csv` select CSV, and no content does.** A body sent
  as `application/csv` or `text/tab-separated-values`, and a file named `.tsv`
  or `.txt`, is text. A `python` script, or a pre-script of a `jq` collector,
  then receives one string where it expected rows. Set `decoder.type: csv`
  for those. A `csv` transform reads CSV whatever the response says.

Each transform reads what certain decoders produce:

| Transform | Decoders it reads |
| --- | --- |
| `jq`, `yq` | `json`, `yaml`, `graphite`, `ndjson`, `logfmt`, `toml`, `ini`, `influx`, `binary`, `protobuf`, `none` |
| `regex` | `text` |
| `csv` | `csv` |
| `css` | `html` |
| `xpath` | `xml`, `html` |
| `prometheus` | `prometheus` |
| `python` | every decoder, `none` among them |

A `decoder.type` that names a decoder the collector's transform does not read
could never work. It is refused when the configuration loads, with a message
that names both and says what would work:

```text
collector "app" decodes with json, which its regex transform cannot read: it reads text; set decoder.type to text, or use a transform that reads json: jq, yq or python
```

A `jq` or `yq` transform with a `pre_script` is the exception. Its rules read
the mapping or list the script leaves in `data`, whatever was decoded, so it
may name any decoder. The same table applies at scrape time, when the decoder
was left to each response.

Because of that fallback, a target that changes its `Content-Type`, or a file
renamed to another extension, is silently read another way. A collector that
leaves `decoder.type` unset where the transform implies none is therefore
logged at startup and on every reload, and listed in the [dry run](#dry-run)
report:

```text
{"level":"WARN","msg":"configuration warning","file":"config.yaml","warning":"collector \"app_json\" sets no decoder.type, so it decodes by the Content-Type header of each response, and by the content when that does not say; set decoder.type to fix the decoder"}
```

Setting `decoder.type` silences the warning. That includes setting it to
`auto`, when choosing per response is what you want. An explicit decoder is
also how to read an ambiguous or mislabeled endpoint:

```yaml
decoder:
  type: json   # the endpoint says text/plain, but sends JSON
```

Supported decoders are `json`, `yaml`, `xml`, `csv`, `html`,
`prometheus`, `text`, `graphite`, `ndjson`, `logfmt`, `toml`, `ini`, `influx`,
`binary`, `protobuf`, `none` and `auto`:

- `none` reads no body (see [No decoder](#no-decoder)).
- `binary` reads the answer of a binary protocol by the layout in
  `response.binary` (see [Binary answers](#binary-answers)), and `protobuf`
  reads a protobuf message by `response.protobuf` (see [Protobuf
  responses](#protobuf-responses)). `auto` never selects either of the two.
- `graphite` reads Graphite series, from a render API answer or carbon lines,
  for jq, yq and Python; see [Graphite](GRAPHITE.md).
- `ndjson`, `logfmt`, `toml` and `ini` read JSON Lines, key=value log lines,
  TOML and INI files into a document that jq, yq and Python read like JSON;
  see [NDJSON, logfmt, TOML and INI](#ndjson-logfmt-toml-and-ini).
- `influx` reads InfluxDB line protocol into a list of its points; see
  [InfluxDB line protocol](#influxdb-line-protocol).

Every collector sets `transform.type` to one of `jq`, `yq`, `xpath`, `css`,
`csv`, `regex`, `prometheus` and `python`. There is no default, and a
collector without one is refused at startup. JSON and YAML expressions use the
embedded jq-compatible engine, which also serves yq-compatible transforms. XML
supports XPath, HTML supports CSS selectors and XPath (including bare element
selectors such as `h1`), text supports regular expressions, and Prometheus
input is parsed before filtering and renaming.

**One value, one document.** A JSON body is one value and a YAML body is one
document. NDJSON, or anything other than whitespace after the JSON value,
fails the scrape with a message that says so (`decoder.type: ndjson` reads
NDJSON). So does a second YAML document after `---`; a leading `---`, and a
trailing one with nothing after it, are accepted. Nothing after the first
value is silently dropped. With `response.yaml.documents: list` the body is
instead the list of its documents, in order, whether it holds several, one or
none, which is how a stream of Kubernetes manifests is read. An empty
document is not counted. Rules then read `.[]`, or `items: .[]`.

**YAML mapping keys.** The keys of the decoded body are the keys of JSON
objects, which must be scalars, and the exporter refuses anything that would
lose a key silently:

- A key written twice in one mapping fails the scrape (`line 3: mapping key
  "a" already defined at line 1`), instead of the later value replacing the
  earlier one. The error names the first ten such problems and counts the
  rest.
- A key that is a sequence or a mapping (`? [a, b]`, `{x: 1}: 2`), or an
  alias of one, fails the scrape: `yaml: the mapping key at line 3 is a
  sequence; only scalar keys (text, numbers, booleans) can be read`.
- Keys are text; a number or a boolean key is its text. Two keys that are
  written differently but have the same text are therefore a duplicate, and
  fail the scrape the same way, naming that text: `line 2: mapping key "1"
  already defined at line 1`. Examples are `1` and `1.0`, `16` and `0x10`,
  `true` and `True`, `'1'` and `1`, and a key and an alias of it. Otherwise
  one of the two would be kept arbitrarily: for `16` and `0x10` the YAML
  library chose silently, and for the others the iteration order of a Go map
  decided. A key merged in with `<<` that has the text of one of the
  mapping's own keys fails too.
- A null key (`~`, `null` or no key at all) has no text and fails the scrape
  at its line: `line 2: mapping key "~" is null, which is no text; quote it
  to read it as text`.

**Numbers.** A whole number of any length keeps every digit, in YAML as in
JSON, so a thirty-digit identifier gives the same label from either. A number
written with a fraction or an exponent is a float. YAML reads more values as
numbers than JSON does, which shows in a label made from one: an unquoted
`017` is octal 15, `0x1F` is 31, `1.10` is 1.1 and `1e3` is 1000. A version or
a zero-padded identifier keeps its characters only when the target quotes it
(`"017"`). Where the target does not, read the body with `decoder.type: text`
and a `regex` transform.

**Cost and depth.** A YAML body costs time and memory in proportion to its
size, whatever it holds. A mapping of 200,000 keys decodes in about the time
that it takes to parse, and a key written 100,000 times is refused as
quickly. `max_response_bytes` therefore bounds what a target's answer can
cost.

A response of any format decodes into a value whose lists and mappings nest
at most 10,000 deep. A document nested deeper fails the scrape in the
`decode` stage:

- A JSON document fails with `arrays and objects nested more than 10000 deep,
  at line 1, column 10001`.
- A YAML document, with or without aliases and however it is written, fails
  with `yaml: the document is nested more than 10000 deep, counting what its
  aliases stand for; a response may nest 10000 deep at most`. The message
  follows `line N:` when it is the YAML library's parser that stops at the
  excess level and names a line (`yaml: line 3: the document is nested more
  than 10000 deep, ...`). That line is where the excess level opens, or the
  line of the last key or value before it. With or without a line, the
  failure is one failure to the
  [log of repeated failures](LOGGING.md#repeated-failures).

A YAML document's depth is counted like a JSON document's: by the sequences
and mappings written one inside another. An alias counts as what it points
at, so anchors that each hold an alias of the previous one are as deep as the
chain is long. A mapping merged with `<<` counts as it is written, one level
inside the mapping it is merged into.

The exporter enforces this bound itself, before the document is decoded,
because the YAML library has no bound on the whole document. The library's
parser bounds indentation and brackets separately, at 10,000 levels each, so
brackets inside indentation parse 20,000 deep and more. A document past
either bound is refused by the library with `exceeded max depth of 10000`;
the exporter replaces that with the message above, so the same mistake reads
the same whichever check finds it. The library also decodes a chain of
aliases on a stack as deep as the chain is long, which could exhaust the
process. A [Python script](PYTHON.md#what-data-is) receives a value as deep
as it decodes.

An XML document may nest its elements 512 deep, as may an HTML document. A
deeper one fails the scrape in the `decode` stage with a message that says so,
whatever its size.

**Failures inside the YAML library or the exporter.** A document the YAML
library itself fails on fails the scrape in the `decode` stage like any other
document that cannot be decoded (`the YAML library failed on the document:
...`). If the exporter's own code fails while decoding one, the error says so
and where: `the exporter failed on the YAML document (yamlkeys.go:<line>
decode.(*yamlMap).set): ...; this is a defect of the exporter and not of the
document, please report it`. That message is what to report.

**Error length.** An error quotes only the start of a part of the document,
followed by the length of the whole part, so it stays short whatever the
document holds:

- The YAML decoder quotes the first 64 bytes of a duplicate key (`mapping key
  "aaaa..."... (100000 bytes) already defined at line 1`), and the first 256
  bytes of a value that does not fit its tag and of an anchor's name.
- The other decoders quote the first 64 bytes of a value they cannot read,
  followed by the value's length, and then say what is wrong: `text format
  parsing error in line 7: label value "aaaa..."... (1000000 bytes) contains
  unescaped new-line`, `carbon line 3: "app.web01.requests 42 1727000000 x
  aaaa..."... (1000040 bytes) has 5 fields; want <path> <value> <timestamp>`,
  `CSV header names column "aaaa..."... (70000 bytes) twice, as columns 2 and
  3; rename one, ...`, `XML syntax error on line 2: element <aaaaaaaa... (90000
  bytes)> closed by </b>`.

An error is therefore a line of a few hundred bytes that ends with what is
wrong and what to change. The same holds for the warning about the first line
a decoder leaves out, such as a skipped carbon line or a sample line.

As a last resort, the whole error is limited to 2,000 bytes. A longer one,
such as a histogram series named by a hundred labels, each shown, is cut
there and ends with the length it had: `... (6400 bytes)`. The limit applies
to the error of every stage at which a probe or a static target's scrape can
fail, in the answer, in the log and in a debug report alike
([Logging](LOGGING.md#repeated-failures)). A jq rule that raises
`error(.message)`, a script that raises an exception with its data, a
redirect to a URL of a megabyte and a gRPC status message of ten megabytes
all fail in at most 2,000 bytes.

### No decoder

`decoder.type: none` reads no body. A collector whose rules use only the
status and the headers, such as an uptime check, has nothing to decode, and a
body that is empty, HTML or any other text causes no failure:

```yaml
request:
  type: http
  method: HEAD           # no body is sent back
  accept_status: [any]   # every status from 100 to 599 is the answer
decoder:
  type: none
transform:
  type: jq
metrics:
  - name: site_up
    expression: 'if $status >= 200 and $status < 300 then 1 else 0 end'
  - name: site_status_code
    expression: $status
```

- **jq and yq** rules read `null` as the document, with `$status` and
  `$headers` available as for any decoder ([Accepting other
  statuses](REQUESTS.md#accepting-other-statuses)). A rule that reads the
  document, such as `.x`, reads `null` and is a missing value.
- **Python**: a script's `data` is `None`, and `response.body` and
  `response.text` are the body as the target sent it, unconverted.
  `response.charset` is refused together with `none`, because no decoder
  reads the body in it.
- A `regex`, `csv`, `css`, `xpath` or `prometheus` transform reads a body, so
  `none` is refused with it when the configuration loads, like any decoder a
  transform cannot read.
- The body is still read, up to `max_response_bytes`. Use `method: HEAD` to
  keep the target from sending it.
- A request type whose answer is JSON (`grpc`, `dns` and the others) and a
  paginated collector keep `json` and refuse `none`, like any other decoder.

### NDJSON, logfmt, TOML and INI

These four decoders produce the same kinds of values as the `json` decoder:
mappings, lists, text, numbers, booleans and null. A `jq` or `yq` rule
therefore reads them like JSON, and a [Python script](PYTHON.md#what-data-is)
receives them like JSON. No other transform reads them, and a collector that
names one of them with another transform is refused when the configuration
loads. Each decoder fails the scrape in the `decode` stage on input it cannot
read. The error names the line, and the column where there is one, and quotes
only the start of a value, as every decoder does.

**`ndjson`** reads newline-delimited JSON, also called JSON Lines: one JSON
value per line.

- The document is the list of the values, in line order, so `items: '.[]'`
  makes a series for each record.
- Blank lines are skipped, as is a carriage return before a line feed. A body
  without a value is the empty list.
- Each line is read as the `json` decoder reads a body, with numbers handled
  the same way.
- A line that is not a JSON value, that continues after its value, or that
  ends inside it (as a pretty-printed value does) fails the scrape: `NDJSON
  decode: line 2, column 9: trailing data after the JSON value; NDJSON holds
  one value on each line`.
- The list is the outermost level of the document, so a line's value nests at
  most 9,999 deep.

```yaml
decoder:
  type: ndjson
transform:
  type: jq
metrics:
  - name: queue_depth
    items: '.[]'
    expression: .depth
    labels:
      - name: queue
        expression: .name
```

**`logfmt`** reads the `key=value` lines of Heroku's logs and of the Go
loggers that write them, as `go-logfmt` reads them.

- The document is the list of the records, one for each line that holds a
  pair. Each record maps its keys to their values as text, which a rule reads
  as a number when it is one
  ([What counts as a number](#what-counts-as-a-number)).
- A value is a run of bytes without blanks, `=` and `"`, or a quoted string
  with the escapes of a JSON string (`msg="GET \"/\""`).
- A key written alone (`cached`) is null, so `has("cached")` distinguishes it
  from a key the line does not have. `key=` is the empty string.
- When a key appears twice on one line, the later one is kept, as a JSON
  object keeps its later member. A logger may add a key a caller already
  gave, and refusing the body for that would lose every other record.
- Blank lines are skipped. Anything else fails the scrape, naming the line,
  the column and the byte: `logfmt decode: line 3, column 16: unexpected '='
  in a value; quote a value that holds it`.

**`toml`** reads a TOML document with `github.com/BurntSushi/toml`.

- Tables are mappings, arrays and arrays of tables are lists, and integers
  and floats are numbers (`nan` and `inf` are NaN and +Inf).
- A date or a time, which JSON does not have, is its text, which
  `time_format` reads as written:
  - an offset date-time in RFC 3339, with the fraction it was written with
    (`1979-05-27T00:32:00.5-07:00`);
  - a local date-time as `1979-05-27T07:32:00`, with a `T` where it was
    written with a space;
  - a local date as `1979-05-27`, and a local time as `07:32:00`.
- An error is the library's, after the line it names (`TOML decode: line 2:
  Key 'a' has already been defined.`). Its message is cut after 256 bytes,
  with its length, because a quoted token can make it any length.

The exporter enforces two bounds before the library reads the body, because
the library has none and a few bytes can cost it far more than their size:

- Arrays and inline tables nest at most 10,000 deep, like every decoder's
  document.
- A value may be named by at most 16 parts, counting the parts of its
  `[table]` header, of its dotted key and of the keys of the inline tables
  around it (`[a.b]` then `c.d = { e = 1 }` names `e` by 5: `a.b.c.d.e`).
  The library looks a key up from the root for each of its parts, so a key
  costs the square of its parts: a single key of 10,000 parts kept it busy
  for seconds. Within the bound, a document costs less than ten times what a
  flat one of its size does (a mebibyte of keys of 16 parts each decodes in
  about a second), and a real document names its values by a few parts.
  Past the bound, the scrape fails: `TOML decode: line 1: a value is named
  by more than 16 parts, ...`.

The scan that enforces the bounds skips a byte order mark, as the library
does. Where it cannot follow the body, which is then not TOML, it fails the
scrape itself and names the line (`TOML decode: line 2: expected a key and =
after it`), instead of passing the library a remainder it has not bounded.

**`ini`** reads an INI file with the exporter's own parser, because the INI
dialects agree on little more than what follows. The document maps each
section's name to a mapping of its keys to their values, as text, and each
key written before the first section to its value: `.pool.active` and
`.uptime`.

- `[name]` starts a section and may be followed by a comment. A dotted name
  is one name, not a path: `[db.primary]` is read as `.["db.primary"]`. A
  section written twice is one section, with the keys of both.
- Any other line is a key and its value, split at the first `=` or `:`, each
  without the surrounding blanks. The value may be empty and is taken as
  written, including quotes. It does not continue on the next line.
- A comment is a whole line that starts with `;` or `#`, so a value may
  contain either character, as a URL does. Blank lines are skipped.
- When a key appears twice in one section, the later one is kept, because an
  INI file is often a defaults file with lines appended to override it. Keys
  and names keep their letter case.
- A key before the first section and a section of the same name would be one
  member of the document, so the scrape fails and names both lines. It also
  fails on a line that is none of the above: `INI decode: line 2: neither a
  section, a key = value nor a comment`.

### Binary answers

`decoder.type: binary` reads the answer of a binary protocol by a layout the
collector writes in `response.binary`. Examples are a Modbus TCP read over
[`tcp`](TCP.md#binary-protocols), a game server's UDP query, or a device's
status frame over HTTP. The document maps each field's name to its value, so
a `jq` or `yq` rule reads `.registers[0]` and a [Python
script](PYTHON.md#what-data-is) reads `data["registers"][0]`. No other
transform reads it, and the rules never take bytes apart themselves.

```yaml
decoder:
  type: binary
response:
  binary:
    byte_order: big          # the default, as network protocols write numbers
    fields:
      - {name: transaction, type: u16}
      - {type: skip, length: 4}         # the protocol id and the length
      - {name: unit, type: u8}
      - {name: function, type: u8}
      - {name: bytes, type: u8}
      - {name: registers, type: u16, count: 2}
transform:
  type: jq
metrics:
  - name: inverter_power_watts
    expression: .registers[0]
```

| Key | Default | Notes |
| --- | --- | --- |
| `byte_order` | `big` | The byte order of a field's numbers when the field sets none: `big` or `little`. |
| `fields` | — | **Required** with the decoder, at most 1,000: the fields, read in order. |
| `fields[].name` | — | **Required** but for a `skip`: the field's member of the document, unique. A `skip` has no name. |
| `fields[].type` | — | **Required.** `u8`, `u16`, `u32`, `u64`: unsigned integers of 1, 2, 4 and 8 bytes; `i8` to `i64`: two's complement ones; `f32`, `f64`: IEEE 754 floats; `string`: `length` bytes of text; `cstring`: text up to a NUL byte; `skip`: `length` bytes passed over. |
| `fields[].offset` | after the field before | The byte the field starts at, counted from 0 at the start of the answer. A layout may jump ahead, or back to read bytes again. |
| `fields[].count` | one value | Numbers only: the field is a list of this many values, one after another, as a run of registers is; at most 65,536. |
| `fields[].length` | — | **Required** for `string` and `skip`: how many bytes. For a `cstring`, the most bytes its NUL is looked for in. No number takes it. |
| `fields[].byte_order` | the layout's | The field's own byte order, `big` or `little`. |

- An integer becomes a number of the document, as the `json` decoder makes a
  whole number, and so does a float. A `u64` above 2^63 becomes a float and
  loses its last bits.
- A `cstring` is the text before its NUL; the NUL is read but not kept.
- In a `string` or `cstring`, each byte that is not valid UTF-8 is read as
  U+FFFD, because a document holds text.
- Bytes after the last field are left unread, because an answer is often
  longer than its layout. A field that runs past the end of the answer fails
  the scrape in the `decode` stage, naming the field: `binary decode:
  response.binary field "registers" needs 4 bytes from byte 9, and the
  answer is 11 bytes`.
- No charset is applied to a binary answer.
- `response.binary` with any other decoder is refused when the configuration
  loads, as are a layout without the decoder and the decoder without a
  layout.

### InfluxDB line protocol

`decoder.type: influx` reads InfluxDB line protocol, the text format InfluxDB
is written with and that Telegraf's `influx` data format produces, one point
per line:

```text
cpu,host=web01,region=eu usage_idle=97.5,cores=8i 1556813561098000000
```

The document is the list of the points, in line order, which `jq`, `yq` and
`python` read like a JSON array of objects:

```json
[{"measurement": "cpu",
  "tags": {"host": "web01", "region": "eu"},
  "fields": {"usage_idle": 97.5, "cores": 8},
  "timestamp": 1556813561098000000}]
```

```yaml
decoder:
  type: influx
transform:
  type: jq
metrics:
  - name: cpu_usage_idle
    items: .[] | select(.measurement == "cpu")
    expression: .fields.usage_idle
    labels:
      - name: host
        expression: .tags.host
```

- `tags` are text. A field has the type that the line writes:
  - a float is a number;
  - an integer (`8i`) and an unsigned integer (`8u`) are whole numbers that
    keep every digit;
  - a boolean (`t`, `T`, `true`, `True`, `TRUE`, and the same forms of
    false) is true or false, and a rule over it reads 1 or 0, as over any
    boolean;
  - text in double quotes is a string.
- `timestamp` is the number as the line writes it, or `null` for a point
  without one. The line does not state its precision, which is nanoseconds
  unless the writer chose another, so a rule that wants seconds divides:
  `.timestamp / 1e9`.
- A point without tags has an empty `tags`. A key written twice in a point's
  tags or fields keeps the later value. Blank lines and lines that start
  with `#` are skipped. `\,`, `\=` and a backslash before a blank write those
  characters in a name.
- Anything else fails the scrape, naming the line, the column and what was
  expected there: `influx decode: line 3, column 9: unexpected 's' where
  the timestamp is, a whole number`.
- `auto` never selects this decoder, because no content type or file name
  identifies line protocol. Name it explicitly.

Line protocol is InfluxDB's input format, so read it where it is written: a
file that Telegraf's `file` output writes, with [`localfile`](LOCALFILE.md),
[`sshfile`](SSHFILE.md) or [`s3file`](S3FILE.md); an export of a bucket; or a
service that answers with it. InfluxDB's own query API answers CSV, which an
`http` collector reads with the [`csv` decoder](#reading-csv-what-to-expect).
The exporter accepts no pushed data, so it cannot act as a Telegraf output.

### Querying InfluxDB

An `http` collector can send InfluxDB a query and turn the answer into
metrics. This helps when you move from InfluxDB to Prometheus, or run both
for a while. Each probe asks the query again, and Prometheus stamps what the
query returns with the time of the scrape, not with the time InfluxDB holds.
So ask for the latest value of a recent window, such as `last()` over the
last 5 minutes.

The token below is read from the environment with
[`--config.expand-env`](FLAGS.md). InfluxDB 1.x without a token takes a
username and password: use `basic_auth` or `basic_auth_file` in place of the
`Authorization` header (see [Authentication](AUTHENTICATION.md)).

**InfluxQL**, of InfluxDB 1.x and of the 1.x API of later versions:
`/query` answers JSON, which the `jq` transform reads.

```yaml
collectors:
  - name: influxql_cpu
    request:
      type: http
      path: /query
      query:
        db: telegraf
        q: SELECT last("usage_idle") AS "idle" FROM "cpu" WHERE time > now() - 5m GROUP BY "host"
      headers:
        Authorization: Token ${INFLUX_TOKEN}
    transform:
      type: jq
    metrics:
      - name: influx_cpu_idle_percent
        type: gauge
        items: .results[0].series[]?
        expression: .values[0][1]
        labels:
          - name: host
            expression: .tags.host
```

- The answer has one entry in `results` for each statement. Each entry has
  one `series` for each group of `GROUP BY`, with the group's `tags`, the
  `columns` and the `values`, one row per point:
  `{"name": "cpu", "tags": {"host": "web01"}, "columns": ["time", "idle"],
  "values": [["2026-11-25T10:00:00Z", 97.5]]}`.
- A value is read by its place in the row. `time` is always first, so
  `.values[0][1]` is the first column after it.
- A query that finds nothing has no `series`, and the `?` after `[]` makes
  that no items rather than an error. A query that InfluxDB cannot parse
  answers an error status, which fails the probe. One that it parses but
  cannot run, such as one of a database that does not exist, answers status
  200 with an `error` in its entry of `results`, and has no items either. A
  required rule then reports its value missing.

**Flux**, of InfluxDB 2.x and InfluxDB Cloud: `/api/v2/query` answers
annotated CSV, which the `csv` transform reads when
`response.csv.comment` leaves out its annotation lines.

```yaml
collectors:
  - name: flux_cpu
    request:
      type: http
      method: POST
      path: /api/v2/query
      query:
        org: example
      headers:
        Authorization: Token ${INFLUX_TOKEN}
        Accept: application/csv
        Content-Type: application/vnd.flux
      body: |
        from(bucket: "telegraf")
          |> range(start: -5m)
          |> filter(fn: (r) => r._measurement == "cpu" and r._field == "usage_idle")
          |> last()
          |> keep(columns: ["host", "_value"])
    response:
      csv:
        comment: "#"
    transform:
      type: csv
    metrics:
      - name: influx_cpu_idle_percent
        type: gauge
        expression: _value
        labels:
          - name: host
            expression: host
```

The answer looks like this. The lines that begin with `#` are annotations,
and the first column has no name and is empty:

```text
#group,false,false,true,false
#datatype,string,long,string,double
#default,_result,,,
,result,table,host,_value
,,0,web01,97.5
,,1,web02,88
```

- `comment: "#"` leaves out the annotation lines. The first column is
  empty in every row, so it is left out, as any such column is.
- Every row has the columns `result` and `table`. Rules read the columns
  they need and leave those out.
- Tables with the same columns share one header. When the columns change
  from one table to the next, InfluxDB writes a blank line and a new header,
  which the decoder reads as a row. So give every table the same columns:
  `keep(columns: [...])` as above, or `pivot()` to make fields columns.

### Protobuf responses

`decoder.type: protobuf` reads a body that is one protobuf message, as an API
that answers `application/x-protobuf` sends, and passes it on as the same
JSON a [`grpc`](GRPC.md) answer is. The message's type comes from the files
that `response.protobuf` names, either `.proto` files or a descriptor set:

```yaml
decoder:
  type: protobuf
response:
  protobuf:
    message: metrics.v1.Report
    proto_files: [/etc/exporter/proto/report.proto]
transform:
  type: jq
metrics:
  - name: report_total
    expression: .total | tonumber
```

| Key | Default | Notes |
| --- | --- | --- |
| `message` | — | **Required.** The message the body is, by its full name, package and all. |
| `proto_files` | — | The `.proto` files that define it, compiled when the configuration loads. |
| `proto_import_paths` | each file's directory | The directories their imports are resolved in, as `protoc -I`. |
| `protoset_file` | — | In place of `proto_files`: a descriptor set, written with its imports by `protoc --descriptor_set_out --include_imports` or `buf build -o`. |

- The document names each field as the `.proto` file does, includes the
  fields the message left unset with their zero values, and writes a 64-bit
  integer as text, as protobuf's JSON mapping does. Read such a value with
  `tonumber`, or `int(...)` in a script.
- `jq`, `yq` and `python` read the document. `auto` never selects this
  decoder.
- The files are read when the configuration loads. A file that does not
  compile, or a message that is not in it, fails the configuration. A file
  that changes afterwards is read again.
- A body that is not the message fails the decode. Protobuf bodies do not
  identify their type, so a body of another message may decode as this one
  with different values. Name the message the endpoint actually sends.
- The decoder is in every build that has the `grpc` request type, including
  the default build, because it reads descriptors with the same libraries. A
  build without `grpc` refuses it.

### Character encodings

Everything the exporter serves is UTF-8, as Prometheus requires; Prometheus
refuses a whole scrape over one label value that is not. A response in
another encoding is converted before it is decoded. The encoding is taken
from the first of these that gives one:

1. A byte order mark (UTF-8, UTF-16LE or UTF-16BE).
2. The collector's `response.charset`.
3. The `charset` of the `Content-Type` header.
4. What the document declares in itself. For XML, that is the encoding of the
   XML declaration. For a document decoded as HTML, it is a `<meta>` in its
   first 1024 bytes or, in a page without a `<meta>` that declares an
   encoding, the encoding of an XML declaration the document starts with, as
   XHTML has one. Where the two disagree, the `<meta>` is used, as in a
   browser. The XML declaration of an HTML document counts only at its very
   first byte; after a blank or an empty line it is ignored.

**Finding a `<meta>`.** A `<meta>` is found the way a browser finds one (the
HTML standard's prescan): as a tag, not as the text `charset=`.

- It is a `<meta>` tag with a `charset` attribute, or with
  `http-equiv="Content-Type"` and a `content` attribute holding `charset=…`,
  in any letter case, quoted or not. The first tag that declares an encoding
  is used.
- These declare nothing:
  - a `<meta>` inside a comment;
  - the `content` of any other `<meta>`, such as a description that mentions
    `charset=windows-1251`, or the address of a refresh;
  - the word in another element's attributes or in a title;
  - a declaration that is not complete within the first 1024 bytes.
- As in a browser, a `<meta>` or an XML declaration of an HTML document that
  names UTF-16 is read as UTF-8, because a page whose declaration could be
  read is not UTF-16, and `x-user-defined` is read as `windows-1252`. A byte
  order mark or a header that names UTF-16 is taken at its word.
- A `<meta>` that names an encoding the exporter does not know declares
  nothing, as in a browser. The next `<meta>` is examined, then the XML
  declaration, and a page that declares nothing is treated as UTF-8. The same
  applies to the XML declaration of an HTML document.
- The name is the whole value of the attribute, so `<meta charset=utf-8/>`
  names `utf-8/`, which is not an encoding. A UTF-8 page is still read
  correctly, but a windows-1251 page whose tag is
  `<meta charset=windows-1251/>` has declared nothing: it is read as UTF-8
  and repaired, with the warning described below. Quote the value or leave a
  blank before `/>`.

**The `Content-Type` header.** Its `charset` is also read from a header that
is not well formed:

- one with a parameter that has no value (`text/html; charset=windows-1251;
  q`);
- two values that a proxy joined with a comma
  (`text/html; charset=windows-1251, text/html`), of which the first is read.

These name nothing: a header without a type and a subtype, a `charset`
without a value, and a `charset` whose value opens a quote that is not
closed (`charset="; q`).

```yaml
  - name: legacy_status
    request:
      type: http
    response:
      # The target sends windows-1251 and says nothing, or says the wrong thing.
      charset: windows-1251
```

`response.charset` is for a target that does not declare its encoding or
declares the wrong one, and for [local files](LOCALFILE.md), which declare
none.

- Names follow the WHATWG Encoding Standard, as in a browser: `utf-8`,
  `windows-1252`, `iso-8859-2`, `windows-1251`, `koi8-r`, `shift_jis`, `gbk`,
  `euc-kr` and the rest. `iso-8859-1` and `latin1` are read as
  `windows-1252`, as browsers do.
- An unknown name in `response.charset` fails the load. An unknown name in
  the `Content-Type` header, or in the XML declaration of a document decoded
  as XML, fails the `decode` stage, which names it.
- Transforms and Python scripts see the converted body, and its
  `Content-Type` says `charset=utf-8`. A [debug report](#debug-probes) shows
  the response as the target sent it, and says what its body was converted
  from.

**Invalid UTF-8.** Bytes that are still not valid UTF-8 after conversion, as
from a target that says UTF-8 and is not, do not fail the scrape.

- The invalid bytes in label values and help text are replaced with `�`
  (U+FFFD) and counted in `http_exporter_invalid_utf8_total`.
- They are logged as a warning that names the first metric
  (`first_metric`). A name longer than 200 bytes is shown by its first 200
  bytes and its length, as an [error](LOGGING.md#repeated-failures) shows
  one.

Setting `response.charset` fixes the problem at the source.

Each run of invalid bytes becomes one `�`, however long it is, so names that
differ only in such bytes are repaired into the same text. For example, a
page of Cyrillic names in windows-1251 that declares nothing gives every
one-word name the label `�`. The series are then duplicates of one, and the
scrape fails (`duplicate metric series`) after the warning. The remedy is the
same.

**Prometheus input.** Prometheus input is the text exposition format, version
0.0.4, or OpenMetrics 1.0 text, read by the exporter's own parser. The parser
follows the reference parser's rules: families from HELP and TYPE lines,
`_sum`/`_count`/`_bucket` grouped into summaries and histograms, and quoted
UTF-8 names such as `{"my.metric", key="value"} 1`.

- It accepts three things the reference parser rejected: a body without a
  final newline, CRLF line endings, and trailing blanks on a line.
- It rejects a histogram or summary count, or a bucket's count, that is
  negative, NaN, infinite or a fraction such as `1.5`. A count is a whole
  number of observations, however it is written (`7`, `7.0`, `1e3`).
- A malformed body fails the decode with the offending line number, for
  example `text format parsing error in line 3: expected float as value, got
  "n/a"`.
- A label value that is not valid UTF-8 does not fail the decode. It is
  repaired with `�` and counted, as described above.

**Labels with an empty value.** A label that a target writes with an empty
value is removed from its series: `m{l="",k="v"}` is read as `m{k="v"}`. To
Prometheus the two are one series, and the exporter exports no label with an
empty value, whatever its origin: a target's exposition, a rule's expression,
a script, or a constant written `""`.

- The label is removed before anything reads the series. A `prometheus`
  rule's label that reads `l` finds none, as when the target did not write
  it, and a `required` one is missing. A pre-script and a `python` transform
  receive the series without it. The text format, OpenMetrics and
  [OTLP](OTLP.md) carry no such label, with or without a script in between.
- What was refused about such a label is still refused: its name written
  twice on a sample, and an empty histogram `le` or summary `quantile`,
  which is a bound that is not a number rather than a label. On a series
  without buckets or quantiles, `le` and `quantile` are ordinary labels and
  are removed when empty.
- Two series that a target distinguishes only by such a label are the same
  series twice (`m{l=""}` beside `m`), and the scrape fails for the duplicate
  as it does for `m` written twice (`duplicate metric series "m"`). It failed
  before as well, because the check has always read an empty label as none.
- The samples of a histogram or a summary written this way belong to one
  series. A histogram whose buckets carry `l=""` and whose `_sum` and
  `_count` do not is one histogram, where it used to be two halves that
  failed the scrape. Two whole histograms distinguished by `l=""` alone fail
  the decode for the sample they then have twice (`second h_sum sample for
  the histogram h`).

**Histograms and summaries.** A histogram or a summary is passed on as the
target wrote it, with its buckets and quantiles in ascending order. That
includes values the text format allows and OpenMetrics does not, such as a
negative `_sum` or bucket counts that decrease (see [OpenMetrics](#openmetrics)
for how those are written).

OpenMetrics allows a histogram with buckets alone and a summary with
quantiles alone, and some text format targets leave `_sum` or `_count` out.
Such a series is exported without the line, in either exposition format,
never with a `_sum 0` or a `_count 0` that the target did not write. A
histogram's count is then the count of its `+Inf` bucket, which counts every
observation, and a histogram with a `_count` and no `+Inf` bucket is given
the bucket.

A histogram's `+Inf` bucket and its `_count` are the same number written
twice. A target that updates the two without a lock is occasionally scraped
between the updates: `h_bucket{le="+Inf"} 7` beside `h_count 8`. Such a
histogram does not fail the scrape, which would lose every other family of
the target with it. It is passed on as written: the text format answers
`h_bucket{le="+Inf"} 7` and `h_count 8`, each as it was read. The same
applies to a histogram with neither a `+Inf` bucket nor a `_count`, which is
exported with the buckets and the `_sum` it has, without either line. Neither
is a valid OpenMetrics histogram, so [OpenMetrics](#openmetrics) writes both
as `unknown` families and [OTLP](OTLP.md#delivery) as gauges, with the same
series and values.

Samples that cannot form one series fail the decode, as two samples of one
plain series fail the scrape. The error names the series and the line it
starts on:

- two buckets with the same bound, however it is written (`le="1"` and
  `le="1.0"`), or two values of one quantile
  (`the histogram h, which starts in line 2, has two buckets with the upper
  bound 1`);
- a second `_sum` or `_count` sample of the series.

**Sample lines that do not belong to their family.** Such a line does not
fail the decode. It is left out, and the rest of the answer is served.

- Under `# TYPE h histogram`, a sample named `h` itself, or an `h_bucket`
  without an `le` label, has a value that no histogram series has a place
  for. The same is true of a sample named after its summary without a
  `quantile` label.
- Client libraries write such lines and Prometheus ingests them. Micrometer's
  older Prometheus registry writes a timer's percentiles as
  `x{quantile="0.95"}` under `# TYPE x histogram`, and VictoriaMetrics'
  metrics library writes its buckets as `x_bucket{vmrange="..."}`.
- The line cannot be passed on as a series of its own, because that would be
  a second family under a name the histogram `x` uses for its own lines. Its
  value is therefore not exported. The histogram is exported with the
  buckets, `_sum` and `_count` it has, and so is every other family.
- The lines left out of the families that a collector passes on are counted
  in [`http_exporter_decoder_lines_skipped_total`](SELF-METRICS.md). They are
  logged at warn level once per scrape, not once per line, with the first of
  them and what was expected in its place (`sample lines left out ... line
  2: expected h_bucket with an le label, h_sum or h_count as a sample of the
  histogram h, got h`). While every scrape leaves out the same line, the
  warning is repeated sparingly, like a failure's.
- A line that cannot be read at all, such as one whose value is not a number,
  still fails the decode. OpenMetrics' `_created` samples are read and left
  out, as before, and are not counted.

**OpenMetrics.** A body is read as OpenMetrics when its `Content-Type` is
`application/openmetrics-text`. When the `Content-Type` does not say
`text/plain; version=0.0.4`, a body whose last line is `# EOF` is read as
OpenMetrics too, so an OpenMetrics file read by a
[`localfile`](LOCALFILE.md) collector qualifies. The exporter sends no
`Accept` header of its own, so a target that negotiates answers in the text
format. Set `Accept: application/openmetrics-text` in `request.headers` to
ask for OpenMetrics. When a body is read as OpenMetrics:

- timestamps are seconds, with a fraction, and are kept as milliseconds;
- exemplars (`... 1 # {trace_id="abc"} 0.5`) are skipped;
- a counter `foo`'s sample `foo_total` is the counter `foo_total`, with the
  family's help, as Prometheus names it; a family named `foo_total` is read the
  same way;
- `_created` samples of counters, histograms and summaries are dropped,
  because the exporter has no creation time to export;
- `unknown` is `untyped`, `stateset` is a gauge, and `info` is the gauge
  `foo_info`. A `gaugehistogram` becomes the gauges `foo_bucket` (with `le`
  as a plain label), `foo_gcount` and `foo_gsum`, because a histogram's
  buckets may only go up, while a gauge histogram's go up and down;
- `# UNIT` lines are ignored, and nothing but blank lines may follow `# EOF`.

All non-Python transforms use the same collector-level metric declaration.
Each entry has `name`, `description`, `type`, `labels`, and a
transform-specific `expression`. The allowed metric types are `gauge`,
`counter`, `histogram`, `summary` and `untyped`.

- A rule reads one value per series, so it is a `gauge` unless it sets
  another type.
- `histogram` and `summary` are refused at startup for every transform except
  `prometheus`, where a rule passes through a series that has its buckets or
  quantiles. The exception is a `histogram` rule with a
  [`histogram` block](#histograms-from-a-list-of-values), which counts the
  values it reads into a histogram.
- A `prometheus` rule without a `type` keeps each series' own type, so a
  counter stays a counter.

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

The selected transform interprets the expression and the label values:

- `jq`/`yq`: jq expressions evaluated against the decoded data.
- `regex`: a RE2 expression whose capture groups give the value and the
  labels.
- `csv`: the expression is the name of the numeric column, and labels map to
  column names.
- `css`: the expression selects the HTML element whose text is numeric.
- `xpath`: the expression selects XML or HTML nodes whose text is numeric, and
  labels are XPath expressions evaluated at each node.
- `prometheus`: the expression matches source metric names, and the rule can
  remap the name, description, type and selected labels.

The details for each transform follow.

**`regex`.** The capture group named `value`, as in `(?P<value>\d+)`, is the
numeric value. When no group has that name, the first capture group is the
value. Labels map to capture-group numbers or names.

- A named value can come after the text a label reads, as in
  `(?m)^(?P<host>\S+) load (?P<value>[\d.]+)$`, where the first group belongs
  to the `host` label (`expression: host`, or `"1"`). Groups are numbered by
  their position in the regex, whichever of them is the value. A label may
  also read the value's group, taking its text as written.
- A regex without a capture group, or with two groups named `value`, is
  refused at startup.
- A match whose value group captured nothing (an optional group that did not
  participate, or one that matched only blanks) is a missing value for that
  match.

**`csv`.** The expression is the numeric column's name, and labels map to
column names. With `response.csv.header: false` there are no names, and
columns are named by number from 1: `expression: "2"` reads the second
column. A name is then refused at startup.

Header and row shape:

- A header that names one column twice, or leaves a column that holds values
  unnamed, fails the scrape and names the column, so that one column never
  silently hides the other. Rename one, or read the columns by number with
  `header: false`. An unnamed column that is empty in every row, as a
  delimiter at the end of each line leaves, is left out.
- A row with more fields than the header has columns also fails the scrape
  when any extra field holds a value, naming the line and the column: `CSV
  line 5 has a value in column 5, which the header does not name`. The value
  has no name to be read by. This is the one sign that a row's values are in
  the wrong columns, which happens with text that contains a comma and no
  quotes, a delimiter that is not the file's, or a quoted field of
  tab-separated values read without `trim_space`. Extra fields that are all
  empty, as a delimiter at the end of the line leaves, are left out.
- A row with fewer fields than the header has empty values in the columns it
  lacks. With `header: false`, rows may have any length.
- A response with no rows (a header alone, or nothing at all) has no value
  for any rule, so a required rule is
  [missing its value](#when-a-metric-cannot-be-extracted).

Comment lines:

- `response.csv.comment: "#"` leaves out every line that begins with `#`,
  such as the annotation lines of InfluxDB's
  [annotated CSV](#querying-influxdb). Only a line that begins with the
  character is a comment. A line that begins with a blank, and a line inside
  a quoted field, are not.
- An error names a line by its number in the body, comment lines counted.
- The comment character is one character. It cannot be the delimiter, a
  double quote, white space or a control character.

Line ends and quotes:

- A line ends at a line feed, at a carriage return followed by a line feed,
  and at a carriage return alone, even within one file. Inside a quoted
  field, a line feed and a carriage return alone are part of the field's
  text, and a carriage return followed by a line feed is read as the line
  feed alone.
- A quote inside a field that does not start with one, as in `5" disk`, is
  read as written. A quoted field left open, or one with a quote in it that
  is not doubled, still fails the scrape, whatever the other rows hold,
  instead of being read on across the following rows. The error for a field
  left open names the line the file ends on. That is so in a file whose
  lines end with a carriage return alone as in one with line feeds: the
  carriage returns after the unclosed quote end lines too.

Blanks and delimiters:

- `response.csv.trim_space: true` trims the blanks on both sides of every
  field, with or without a header row, and lets a quoted field begin after
  blanks and end before them: `a,  "x, y"  ,b` is read as `a`, `x, y` and
  `b`. Without it, blanks after a closing quote fail the scrape as a stray
  quote does.
- With a tab, or another blank that is not a space, as the delimiter, an
  empty field stays where it is, and the fields after it stay in their
  columns. A quoted field written after spaces is still a quoted field, and a
  tab inside it is kept.
- With a space as the delimiter, a run of spaces is one delimiter, which is
  how columns aligned with spaces are read (`web01   72  "two words"` is
  three fields). The spaces after a closing quote are the delimiter too. An
  empty field is then written as a pair of quotes, `web01  ""  72`, because
  no blank can stand for it. This applies under `trim_space`. Without it,
  every space is a delimiter of its own, and aligned columns fail at the
  header, because of the column a run of spaces leaves unnamed.

[Reading CSV: what to expect](#reading-csv-what-to-expect) lists what the
files that tools write do to a rule.

**`css`.** The expression selects the HTML element whose text is numeric.

- Without [`items`](#metrics-per-item) a metric is one value, so the
  expression must match at most one element. For several values, and for
  labels read from the page, use `items`: select the rows with it, and the
  value and the labels as cells of each row.
- A selector reads the text of the element it selects and never an
  attribute. A value or a label that is in an attribute, such as
  `data-value` or the `datetime` of a `<time>`, needs `xpath`.
- [The text of an HTML element](#the-text-of-an-html-element) defines what
  that text is, and
  [Reading HTML: what to expect](#reading-html-what-to-expect) describes
  what a page does to a selector.

**`xpath`.** The expression selects XML or HTML nodes whose text is numeric.
Labels are XPath expressions evaluated at each node, such as `../@name`, or
`@name` for an attribute of the node itself.

Absolute paths in labels:

- An absolute path in a label starts at the document, as it does in a rule's
  expression, wherever it appears: `/status/@site`, `//status/@site`,
  `concat(/status/@site, '-', @id)` and `../x[@ref=//y/@id]` read what they
  say at every node the rule selects. So `//name` in a label is the first
  `name` of the document, the same for every series. A `name` beneath the
  node is `.//name`, and its child is `name`.
- A label that cannot depend on the node is evaluated once per rule and
  response, and every series gets its value. Such a label is either:
  - one absolute path and nothing else, such as `//status/@site` or
    `//row[v > 1]/name`;
  - or one call of `count`, `sum`, `string`, `number`, `boolean`, `not`,
    `normalize-space`, `string-length`, `name`, `local-name`, `round`,
    `floor` or `ceiling` with one such path as its argument, such as
    `count(//row)`.
- Every other expression that contains an absolute path is evaluated at each
  of those nodes and walks the document from the top each time. Examples are
  `concat(/status/@site, '-', @id)`, and also `count(//row) + 1`,
  `//a | //b` and `(//name)[1]`, which no node changes. Over thousands of
  nodes that costs seconds of a probe. Give the part that is the same
  everywhere a label of its own in one of the two forms above, and reach
  nearby nodes with `..` or `ancestor::`.

Checks at startup:

- An expression is read to its end or refused at startup. The XPath engine
  stops where one complete expression ends and ignores what follows, so
  `sum(//a))` would be read as `sum(//a)` and `//a 'x'` as `//a`. Such an
  expression is refused, with the position: `XPath "sum(//a))": the ")" at
  byte 9 closes nothing`, or `what stands from byte 5 on, "'x'", is no part
  of the expression before it`.
- A predicate after parentheses within a path, as in `//a/(b)[1]`, is
  refused the same way, because the engine reads none there. Write it inside
  the parentheses, after the step it applies to: `//a/(b[1])`. Predicates
  may follow one another after an expression in parentheses, a function call
  or a literal: `(//a)[@x][1]` is the first `a` of those with an `x`.
- An expression longer than a few hundred bytes may be refused without the
  position.
- Also refused are an expression that contains a NUL character, which the
  engine takes for its end, and one whose parentheses, predicates and
  function arguments nest more than 198 deep. That is the deepest level that
  can be verified to be read to its end.

Rules that select attributes:

- A rule may select attributes, as `//job/@size` does. A label is then
  relative to the attribute: `.` is its value, `name()` its name, and
  `../@name` another attribute of its element.
- The label `text()` also gives the attribute's value, as `.` does, so the
  series of such a rule can be distinguished by it. To XPath an attribute
  has no text beneath it, but the value is what this label has always given.
  Only that bare label is the value.
- An attribute has no nodes beneath it, in HTML as in XML. On such a rule,
  `./text()`, `text()[1]`, `node()`, `self::*`, `descendant::text()`,
  `normalize-space(text())`, `string(text())` and
  `substring(text(), 1, 2)` give nothing, `contains(text(), '1')` is false
  and `count(text())` is 0. Write them with `.` instead, as in
  `normalize-space(.)` or `substring(., 1, 2)`.
- This works for HTML as for XML. `//td/@data-value` with the labels
  `../@data-server` and `../../@id` is one series for each cell, named by
  its cell and its row. `//time/@datetime` with a
  [`time_format`](#reading-a-date-or-a-time-as-the-value) and the label
  `../../../td[1]` reads the time of every row of a table.

HTML and XML differences:

- In HTML, the text of a node is
  [the text of an HTML element](#the-text-of-an-html-element).
- In XML, prefixes in expressions mean what
  [`response.namespaces`](#xml-namespaces) says.
- HTML has no namespaces. A label that is `@` and one attribute name, on the
  node or after `../` steps, is read by that name as the page writes it,
  including a colon, as in `@xml:lang`, `@og:type`, `@v-on:click` or
  `../@data-id` ([Attribute labels](#attribute-labels)).

Computed values:

- An expression that computes a value instead of selecting nodes gives one
  series with that value. Examples are `count(//job)`, `sum(//job/@size)`,
  `string(/status/@load)` and a comparison such as `/status/@state = 'ok'`,
  which gives `1` or `0`.
- A label may compute its text too, as in `normalize-space(@name)`. A number
  it computes is written like a jq label's: without an exponent from a
  millionth up to 1e21, as described below.
- A computed value that is not a number, such as `number('n/a')`, is the
  rule's missing value. A NaN that an expression computes is the XPath
  engine's way of saying it could not read a number. `NaN` written in a cell
  is different: it is the value the source gives, and it is exported
  ([What counts as a number](#what-counts-as-a-number)).
- `sum()` needs every node it adds up to be a number. Over a cell of `n/a`
  or `1,234` the sum has no value, and the rule is missing its value, with
  an error that names the text
  ([Adding up nodes with `sum()`](#adding-up-nodes-with-sum)).

Whitespace in labels:

- A label read from an element's text or from an attribute, or computed as a
  string, is trimmed of leading and trailing whitespace, like a `css` label.
  The indentation of pretty-printed XML is therefore not part of it.
- Whitespace inside the text is kept, including line breaks and tabs.
  `normalize-space(.)` reduces it to single spaces, which `css` cannot do.
- An attribute read by its name on the node itself, `@title`, is trimmed
  like one read through a path, `../@title`. An attribute that holds only
  blanks gives no label.

**`prometheus`.** The expression matches source metric names, and the rule
can remap the name, description, type and selected labels. Without `type`, a
series keeps its own type.

- The expression is a regular expression, matched anywhere in the name the
  target gives a metric. A rule without an expression matches the metric
  that its `name` names.
- A rule that matches no metric of the response has no value, so a required
  rule is [missing its value](#when-a-metric-cannot-be-extracted). Give a
  rule for a metric the target may leave out `required: false`.
- An expression that is only blanks, such as `expression: " "`, is refused
  at startup, as [an entry of `transform.include`](#collector-wide-labels)
  is. It matched only names that contain those blanks, so the rule passed
  nothing on and, with `required: false`, reported nothing. The error shows
  how to write a pattern that does mean a blank: `'[ ]'` or `'\x20'` in
  single quotes. An expression written `""` is the same as leaving the key
  out.
- A rule needs a `name` or an `expression`, because they are how it says
  which metrics it is about. A rule with neither (`- {}`, or a rule with only
  a `type`, a `description` or `labels`) used to match no metric. It was
  reported as missing on every scrape, as if the target had left a metric
  out, and with `required: false` it silently did nothing. It is now refused
  at startup. Such a rule has no name, so the error names the collector and
  the rule's position, counted from 1, as in `collector "node" metrics rule
  2 has neither a name nor an expression, …`. It says what to write: an
  `expression`, a `name`, or `expression: '.*'` for a rule about every
  metric.
- A pattern belongs in `expression`, never in `name`. A rule's `name` is one
  metric's name: it is matched whole when the rule has no expression, and
  beside an expression it is the one name under which the matched series are
  exported. So `name: 'node_.*'`, `name: '^up$'` and `name: 'up|node_load1'`
  are refused as invalid metric names. A name that contains a character with
  a meaning in regular expressions (`. * + ? ^ $ | ( ) [ ] { }` or a
  backslash) is almost certainly a pattern under the wrong key, so the error
  says so: `… is not a valid Prometheus metric name; use letters, digits,
  underscores and colons, not starting with a digit, or set the collector's
  name_escaping to underscores or values to export it escaped; a pattern to
  match the target's metric names by is a prometheus rule's expression, not
  its name, so if this is one, write it as expression, …`.
- That check applies under [`name_escaping`](#utf-8-names) `fail`, the
  default. Under `underscores` and `values` a rule's name may be any name,
  as a target's may. `name: http.server.duration` is then the metric the
  target calls that, matched whole and exported escaped, and nothing
  distinguishes a name from a pattern: `name: 'node_.*'` loads as the metric
  with exactly that name, which no target has.
- A metric is passed on by every rule that matches it, and each rule makes
  its own series. A rule with a `name` and a rule whose expression matches
  that name therefore both pass the metric on. With the same labels they
  would produce every series twice, so they are
  [refused at startup](#a-name-and-a-pattern-that-matches-it).

CSS is for HTML tables and HTML status pages. It is not used for CSV.

Each label sets one of two keys. `expression` reads the label from the
response, and `value` gives a static label, exported exactly as written:

```yaml
labels:
  - name: server
    expression: server       # CSV column for the current row
  - name: environment
    value: production        # static, on this metric only
```

A label that sets both, or neither, is refused at startup. So is a label
whose `expression` is only blanks, such as `expression: " "`, with or without
a `value`. Blanks are not the same as leaving the key out, as `""` is, and
they read nothing. The error says to write the expression or, for a
constant, to set `value` and leave `expression` out. A `value` of blanks is a
constant like any other and is exported as written. For a static label on
every metric of a collector, use
[`transform.labels`](#collector-wide-labels) instead.

A `value` may contain `{{param_<name>}}` placeholders, which the probe's
parameters fill as they fill the request's: `value: "api-{{param_tenant}}"`
is `api-acme` on the series of a probe with `&param_tenant=acme`. See
[In label values](REQUESTS.md#in-label-values) for the rules.

Label expressions use the same transform-specific language as the metric
expression. For CSV, each row produces a metric, and `expression: server`
selects that row's `server` column.

**How a label's value is written.**

- A jq or yq label is the text of the value its expression gives. A number
  is written as it reads, without an exponent from a millionth up to 1e21,
  so an ID of `1234567` is the label `1234567` and a ratio of `0.5` is
  `0.5`. Booleans are `true` and `false`.
- A label is one value, so an object or an array fails the metric, which its
  [`error_mode`](#when-a-metric-cannot-be-extracted) handles. Select one of
  the object's fields, or join an array into one value with
  `.tags | join(",")`.
- An XPath label that computes a number, instead of selecting a node, writes
  it the same way: `count(//item) * 2000000` is the label `4000000`, as
  `2 * 2000000` is from jq, and `sum(//item/@n)` over `1234567` and `0.5` is
  `1234567.5`.
- A label that selects a node, as every css label does, is the node's text
  as the response writes it.

**Labels without a value.** A label expression that gives a series no value
leaves the label off that series. That covers a selector or path that
matches nothing, a missing attribute or capture group, an empty CSV cell and
a null. An empty value has the same effect, because Prometheus treats it the
same way. A label that a [Python script](PYTHON.md) gives as `None` or as
the empty string is left off too, and so is one that a target's own
exposition writes as `l=""` under a `prometheus` transform.

A `csv` label that names a column the response does not have at all is a
different case. No row could supply it, so the rule fails once for the
response, whether or not the rule or the label is `required`, as its
[`error_mode`](#when-a-metric-cannot-be-extracted) says. The error gives the
label, the column and the columns the response does have
([Reading CSV: what to expect](#reading-csv-what-to-expect)).

When a series is wrong without the label, mark the label `required`:

```yaml
labels:
  - name: server
    expression: td.name
    required: true    # a row without a name is an error, not an unlabelled series
```

A series that lacks a required label is a missing value of its metric,
handled by the metric's [`error_mode`](#when-a-metric-cannot-be-extracted).

- `ignore` and `log` drop that one series and keep the rest; `fail` fails the
  probe with an error that names the label.
- The series is counted in `http_exporter_missing_keys_total` and, under
  `ignore` and `log`, in `http_exporter_rule_failures_total`.
- This applies whatever `required` and `error_handling.allow_missing_keys`
  say about the value.
- `required` applies to `expression` labels, and not to the python
  transform, whose labels come from its script.

### Reading CSV: what to expect

Spreadsheets, databases and scripts each write CSV in their own way. This
table shows what a `csv` rule does with the variants:

| The file | What happens | What to do |
| --- | --- | --- |
| Has a header line, and is read with `header: false` | The first line is a row of data like the others, so every rule fails on it, on every scrape: `value "used" is not a number`. | There is no setting that skips lines. Read the columns by name, or drop the row in a pre-script: `data = data[1:]`. |
| Lacks a column a rule names | The rule's value: `CSV column "used" is not in the response, whose columns are "Used", "Host"; column names are matched exactly`, a missing value of every row. A label: the rule fails, once, with `metric "used" label "zone": CSV column "zone" is not in the response, …`, and makes no series. | See below. |
| Has an empty cell in a column a rule names | A missing value of that row, `CSV column "used" is empty in row 3`, the rows counted from 1 without the header's line. A label read from it is left off that series. | `required: false` where that is expected. |
| Has rows shorter than the header | The columns a row lacks are empty in it, so missing values. | `required: false` where that is expected. |
| Has blank lines, or a last line without a line end | A blank line is no row, and the last line is read like the others. | Nothing. |
| Has a line of blanks | It is a row, missing every value. | `required: false`, or drop it in a pre-script. |
| Ends its lines with a carriage return alone, as a spreadsheet's "CSV (Macintosh)" and some instruments do | Each is a line end, as LF and CRLF are, and a file may end some lines one way and some another. Inside a quoted field a carriage return is the field's text, and one with a line feed after it is read as the line feed alone. | Nothing. |
| Is tab-separated, with a field that starts with `"` | Quoting cannot be turned off: such a field is a quoted field and runs to its closing quote, over tabs and line ends, and one never closed fails the decode. A quote later in a field is text. | Write the quote doubled inside quotes, `"""big"" disk"`, or have the export put the field in quotes. |
| Has columns aligned with spaces | With `delimiter: " "` alone every space is a delimiter and the header fails as an unnamed column. | `trim_space: true`, which makes one delimiter of a run of spaces; an empty field is then written `""`. |
| Writes numbers for people: `1,5`, `85%`, `yes` | Text that is no number fails its rule. | [What counts as a number](#what-counts-as-a-number). |

**A column the response does not have** is one that no row has:

- with a header row, one that the header does not name;
- with `header: false`, a number past the end of the longest row;
- for the rows that a `pre_script` left, a key that none of them has.

A row that lacks a column that other rows have holds an empty cell there.

When the header lacks a column, look at the file, because nothing is wrong
with the rule. The usual causes are:

- the header was split by a delimiter other than `response.csv.delimiter`;
- the header is written `host, used` and read without `trim_space`, so the
  column is named ` used`;
- the name is in another letter case (`Used`);
- the file starts with a `sep=;` line, which is then the header;
- the file is in an encoding that nothing declares, so the names are not the
  text the rule has ([Character encodings](#character-encodings)).

The message lists the first twelve columns the response does have, in order
of their names. A name that is the rule's in another letter case or with
blanks around it comes first, so that a table of fifty columns shows it too.
The list shows which cause applies: one column named `host;used`, a name
with a blank before it, `Used`, or `sep=;`.

A missing column for a value is the rule's missing value, so
`required: false` silences it, whatever the cause. A missing column for a
label is not: the rule fails, required or not, because every series would
lack the label.

**Memory.** The rows of a CSV answer cost the memory of the cells their
lines hold. The empty cells of the columns a short row lacks are what a rule
and a script read there; they are not stored in every row. A header of
thousands of columns over lines of one field therefore costs what its bytes
do: a header of 2,000 columns over 2,000 lines of `1`, 15 kB in total,
decodes in under a megabyte. While it is scraped, the rows of a plain file
take several times its size, most for the shortest lines: about 23 times for
lines of one character, and 3.5 times for ten columns of a monitoring
export. This memory is used before `limits.max_metrics` is checked;
`max_response_bytes` is what bounds it.

**Pre-scripts.** A `pre_script` of a `csv` transform receives the rows and
must leave rows. `data` is a list in which each row is a dict keyed by the
header's names or, with `header: false`, a list of the row's fields. The
script may drop rows, change cells and add columns. Anything else left in
`data`, and a row that is neither a dict nor a list, fails the scrape with a
message that says what the script left ([Python](PYTHON.md#what-data-is)),
counted as a script error in `http_exporter_script_errors_total`.

### XML namespaces

In an XPath expression, a name without a prefix, such as `entry` or `@unit`,
matches what the document writes without a prefix. A name with a prefix,
such as `m:size`, is matched against the prefixes as the document writes
them. That works until the target picks another prefix for the same
namespace, and it cannot distinguish an `entry` in one default namespace
from an `entry` in another.

`response.namespaces` maps prefixes of your choice to namespace URIs, for all
of the collector's XPath expressions, in metrics and labels alike. A prefixed
name then matches by the URI, whatever prefix the document uses for it, or
none:

```xml
<feed xmlns="http://www.w3.org/2005/Atom" xmlns:m="urn:example:metrics">
  <entry><title>db01</title><m:size m:unit="bytes">5120</m:size></entry>
</feed>
```

```yaml
  - name: feed_sizes
    request:
      type: http
    decoder:
      type: xml
    response:
      namespaces:
        a: http://www.w3.org/2005/Atom
        x: urn:example:metrics
    transform:
      type: xpath
    metrics:
      - name: entry_size
        expression: //a:entry/x:size      # a prefixed element
        labels:
          - name: unit
            expression: '@x:unit'         # a prefixed attribute
          - name: entry
            expression: ../a:title
```

```text
entry_size{entry="db01",unit="bytes"} 5120
```

`x` finds what the document writes as `m:`, and `a:entry` finds the `entry`
of the Atom namespace, which the document writes without a prefix, and no
other. Once `response.namespaces` is set, the prefixes in the collector's
expressions are the map's own. A prefix it does not map, such as the
document's `m` here, is refused when the configuration loads, instead of
matching nothing on every scrape.

The prefix `xml` needs no mapping and accepts none. XML reserves it for
`http://www.w3.org/XML/1998/namespace`, which no document declares, so
`@xml:lang`, `../@xml:lang` and `//entry[@xml:lang='en']` work with or
without `response.namespaces`, whatever the map holds for `xml`.

#### Attribute labels

A label that is `@` and the name of one attribute of the node is read
directly from the node, by the name as the document writes it:

- **In XML**, a plain name (`@name`, `@data-id`: letters, digits, `_`, `.`
  and `-`, not starting with a digit, `.` or `-`) is always read this way. A
  prefixed name, `@m:unit`, is read this way too while `response.namespaces`
  is not set: the prefix is then the document's own, as it was before
  namespaces could be configured. With `response.namespaces` set, a prefixed
  attribute is XPath: its prefix means what the map says (`@x:unit` above),
  and a prefix the map does not have, other than `xml`, is refused when the
  configuration loads.
- **In HTML**, which has no namespaces, any name works: `@xml:lang`,
  `@og:type`, `@v-on:click`, `@x-on:click.prevent`, `@:href`, `@@click` for
  the attribute `@click`, `@2x`. A colon is a character of the name, and
  `response.namespaces` plays no part. An attribute of a parent is read the
  same way, `../@og:type`, also where the rule selects text nodes, as
  `//td/text()` does. So is an attribute to which HTML gives a namespace
  inside `svg` or `math`, such as `@xlink:href`. Names are as the HTML
  parser keeps them: in lower case, whatever the page wrote, except inside
  `svg` and `math`, where the parser keeps the capitals those languages
  have, as in `@viewBox`
  ([Reading HTML: what to expect](#reading-html-what-to-expect)). Deeper in
  an expression, XPath cannot express such a name; write
  `@*[name()='og:type']` there.

A name is everything after the `@`, provided it contains nothing XPath
treats as an operator: no `/`, `|`, `[`, `]`, `(`, `)`, `=`, `<`, `>`, `!`,
`+`, `*`, `,`, `$`, quote or blank. Anything else that starts with `@` is
XPath like every other label and is checked when the configuration loads:
`@id | @name` gives the first of two attributes that is present, and
`@state = 'ok'` gives `true` or `false`. A label that is neither a name nor
XPath, such as `@[attr]`, is refused at load.

- With `decoder.type` unset or `auto`, a label is accepted when either kind
  of document could read it. If the answer is then of the kind that cannot
  (XML, for a label like `@2x` or `@:href`, names only HTML can have), the
  label is not left off. The metric fails, as its
  [`error_mode`](#when-a-metric-cannot-be-extracted) says, with a message
  that says which label cannot be read in which kind of document.
- Setting `response.namespaces` declares that the collector reads XML. Its
  labels are then checked as with `decoder.type: xml`, even while the
  decoder is unset or `auto`. A prefix the map does not have, as in
  `@m:unit` or `../@m:kind`, is refused when the configuration loads, and so
  is a name only HTML can have. The error says to set `decoder.type: html`
  if the target answers HTML, where such a label is an attribute's name as
  written and `response.namespaces` plays no part.

### Collector-wide labels

Three `transform` settings change the labels of every metric a collector
exports, whatever its transform (jq, CSS, XPath, CSV, regex, Python or a
Prometheus passthrough). They are applied after the metric rules, in this
order:

```yaml
transform:
  type: jq
  labels:            # added to every metric
    environment: production
  remove_labels:     # dropped from every metric
    - internal_id
  rename_labels:     # renamed on every metric
    host: instance
```

All renames are applied at once, from the labels as they were before any
rename, so they never chain: with `a: b` and `b: c`, `b` gets the value `a`
had and `c` the value `b` had. Two renames to the same label are refused at
startup.

A `transform.labels` value replaces a label of the same name that a rule, a
script or the target gave a series: with `labels: {site: dc1}`, a series a
rule labelled `site="rack1"` is exported with `site="dc1"`. A value written
`""` means the label is left out, as a key written `""` means the key is
left out ([Editor support](#editor-support)). `labels: {site: ""}` therefore
adds no label, where it used to export `site=""`, and a series that has its
own `site` keeps it.

A `transform.labels` value may contain `{{param_<name>}}` placeholders, which
the probe's parameters fill as they fill the request's. One collector can
then label its series with a value only the scrape knows:

```yaml
transform:
  type: jq
  labels:
    tenant: "{{param_tenant}}"      # &param_tenant=acme -> tenant="acme"
    region: "{{param_region:eu}}"   # eu unless the probe says otherwise
```

A probe that omits a parameter without a default is answered `400` before
the target is contacted. A value filled with nothing (an empty default the
probe does not replace) means the label is left out, as `""` does. The rules
are in [In label values](REQUESTS.md#in-label-values).

A `prometheus` transform that passes metrics through without `metrics` rules
can also select and rename them: `include` and `exclude` are patterns a
metric name must, or must not, match, and `rename` maps source names to new
ones. With `metrics` rules, the rules select and name the metrics, so these
three keys are refused at startup there, as on any other transform, instead
of being ignored.

An entry of `include` or `exclude` is a regular expression, matched anywhere
in the name the target gives the metric, before `rename` and
`metrics_prefix`: `^node_` selects the names that start with `node_`, and
`load` those that contain `load`.

An entry that is the empty string, or only blanks, is refused at startup,
with an error that names the collector and the key. The empty pattern
matches every name, so `exclude: [""]` dropped every series and
`include: [""]` kept every one. A pattern of blanks matches only names that
contain those blanks, so `include: ["  "]` exported nothing, without an
error. Neither is what such an entry is meant to do; it is usually an empty
string, a string that picked up spaces, or a template that filled in
nothing. To match every name write `'.*'`, and to filter nothing leave the
key out. A pattern that does mean a blank, which a
[UTF-8 name](#utf-8-names) may contain, is written `'[ ]'` or `'\x20'` in
single quotes, where YAML leaves the backslash to the regular expression.

### Prefixing a collector's metrics

`metrics_prefix` puts a namespace in front of everything a collector exports.
The exporter joins it with `_`, so this collector

```yaml
collectors:
  - name: grafana_status
    metrics_prefix: grafana
    ...
    metrics:
      - name: statuspage_status
```

exports `grafana_statuspage_status`. The key is optional. Without it, names
are exactly as declared.

### UTF-8 names

Prometheus 3 lets a target name a metric or a label in any UTF-8, such as
`http.server.duration` or `{service.name="api"}`, as OpenTelemetry names do.
The exporter answers in the classic text format or OpenMetrics (see
[OpenMetrics](#openmetrics)). Their names are limited to letters, digits,
`_` and, for metrics, `:`, which is also what older Prometheus servers,
recording rules and dashboards expect. A name outside that set can come from
a `prometheus` transform that passes a target through, from a Python script
or a pre-script, or from the collector itself, as a rule's name.
`name_escaping` decides what happens to it:

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

- `underscores` replaces every character a classic name cannot have with
  `_`, and a leading digit too. The result reads naturally but cannot be
  reversed.
- `values` is Prometheus's reversible encoding: `U__`, then the name with
  `_` doubled and every other character written as `_` + its hexadecimal
  code point + `_`. Prometheus 3 and its client libraries can turn it back
  into the original name.

These are the escaping schemes of the same names that Prometheus negotiates
with its targets. The third, `dots`, is not offered, because it rewrites
every name that has an underscore, including classic ones.

With either scheme:

- A classic name is never changed, and label values are never escaped,
  because they may hold any UTF-8.
- `metrics_prefix` is joined first, so a prefixed `values` name still starts
  with `U__`: `metrics_prefix: otel` gives
  `U__otel__http_2e_server_2e_duration`.
- Two names that escape to the same name (`a.b` and `a_b` with
  `underscores`) are not merged. Two such metrics are a duplicate series,
  and two such labels of one series fail the scrape with an error that names
  both.

The default is `fail`, so that a name never changes unless someone asked for
it.

The names that a collector writes itself are checked the same way when the
configuration loads: a rule's `name` and the `name` of each of its labels,
under every transform; the keys of `transform.labels`; and the names that
`transform.rename` and `transform.rename_labels` rename to. Each of them
leaves the transform together with the names that the response gave, and is
escaped with them.

The exception is a rule's label, or a key of `transform.labels`, that
`transform.rename_labels` renames or `transform.remove_labels` removes. The
renames and removals happen before the names are escaped and the series
validated, so such a name is never exported. It loads whatever its
characters, `a.b` under `fail` or `__tmp` alike, unless it is only blanks.
The name it is renamed to is subject to every rule here.

```yaml
collectors:
  - name: otel_app
    name_escaping: underscores
    request:
      type: http
    transform:
      type: jq
    metrics:
      - name: http.server.duration     # exported as http_server_duration
        expression: .duration
        labels:
          - name: service.name         # exported as service_name
            expression: .service
```

| `name_escaping` | A rule named `http.server.duration` with a label `service.name` |
| --- | --- |
| `fail` (default) | is refused at startup, since every scrape could only fail: ``collector "otel_app" metric "http.server.duration" has invalid label name "service.name"; set the collector's name_escaping to underscores or values to export it escaped``, and of the metric's name ``… is not a valid Prometheus metric name; use letters, digits, underscores and colons, not starting with a digit, or set the collector's name_escaping to underscores or values to export it escaped`` |
| `underscores` | loads, and exports `http_server_duration{service_name="api"}` |
| `values` | loads, and exports `U__http_2e_server_2e_duration{U__service_2e_name="api"}` |

A `prometheus` rule's `name` without an `expression` is the target's metric
of that name, matched whole as written, so under `underscores` or `values`,
`name: http.server.duration` passes that metric on. A `python` rule's `name`
and its labels' names are the script's, as the script gives them.

Settings that refer to a rule's series find them by the names as written,
before they are escaped: a label's `value_map`, `truncate: true`,
`transform.remove_labels` and the names `rename_labels` renames. Logs, probe
errors and the rule's failures in the self-metrics name the rule as it is
written.

Three things hold whatever `name_escaping` is:

- A name of only blanks is not a name.
- A name is never exported beginning with `__`, which Prometheus reserves.
  `__up` is refused as written. Under `underscores`, so is a name that the
  scheme would turn into one, such as `1_min.load`, whose leading digit
  becomes `_`; the error names both. `values`, which writes `U__` before
  every name it escapes, accepts it.
- A [static target](STATIC-TARGETS.md)'s `labels` are classic names. They are
  added to what the collector exported, after its names were escaped.

Two rules whose names differ as written but are one name once escaped, such
as `a.b` and `a_b` under `underscores`, load, because the load compares what
is written. Each scrape that has both then fails, for the duplicate series,
or the metric of two types, that two such names of a target are.

#### Keeping UTF-8 names

`name_escaping` escapes a name once, for every scraper. Prometheus 3 can
take the name as it is and says so in its `Accept` header, while an older
server, or a dashboard written for escaped names, asks for an escaping. With
`metric_name_validation_scheme: utf8`, which is Prometheus's setting of the
same name, a collector keeps every name as it is, and each answer writes it
as its scraper asks:

```yaml
collectors:
  - name: otel_app
    metric_name_validation_scheme: utf8   # legacy (the default) or utf8
    transform:
      type: prometheus
```

| The scraper's `Accept` asks for | `{"http.server.duration", "service.name"="api"} 0.25` is answered as |
| --- | --- |
| `escaping=allow-utf-8`, as Prometheus 3 does | `{"http.server.duration","service.name"="api"} 0.25`, with `# HELP "http.server.duration" …` and `# TYPE "http.server.duration" gauge` |
| `escaping=underscores`, no escaping, or one the exporter does not know | `http_server_duration{service_name="api"} 0.25` |
| `escaping=dots` | `http_dot_server_dot_duration{service_dot_name="api"} 0.25` |
| `escaping=values` | `U__http_2e_server_2e_duration{U__service_2e_name="api"} 0.25` |

These are Prometheus's own escaping schemes, as its Go client writes them.
`underscores` and `values` work as under `name_escaping`. `dots` writes a dot
as `_dot_`, an underscore as `__`, and any other character a classic name
cannot have as `__`, so that the name can be read back.

- A classic name is written unchanged under all four schemes, including
  `dots`, and so is every label value.
- The quoted form is written in OpenMetrics as in the text format. A
  histogram's `_bucket`, `_sum` and `_count` follow its family's name:
  `{"my.latency_bucket",le="0.1"}` quoted, `my_latency_bucket` under
  `underscores`.
- The answer's `Content-Type` ends with the escaping it was written with,
  such as `text/plain; version=0.0.4; charset=utf-8; escaping=allow-utf-8`.
- An answer whose names are all classic, which includes any answer of a
  `legacy` collector and the self-metrics, is written and typed as it always
  was, without an escaping.
- A static target's result scraped under `utf8` keeps its names through a
  reload that returns the collector to `legacy`, until the target is scraped
  again. Until then the static targets endpoint writes them as the scraper
  asks.

Under `utf8`, a rule's name, its labels' names, the keys of
`transform.labels` and the names that `rename` and `rename_labels` produce
load whatever their UTF-8, as under `name_escaping` `underscores` and
`values`. A name of only blanks, a name beginning with `__` and the length
limits are still refused. `name_escaping` still applies first: `underscores`
or `values` escape a name for every scraper, which then finds nothing to
escape, and `fail`, the default, fails nothing under `utf8`. The default is
`legacy`, as before, so that a name never changes unless someone asked for
it.

Further points:

- A target that escapes its names unless asked, as Prometheus' Go client
  does, has to be asked. Give the collector's request the header
  `Accept: text/plain;version=1.0.0;escaping=allow-utf-8`.
- Over [OTLP](OTLP.md) a name is sent as it is, because OTLP names are free
  text. `otlp.metric_names: otel` moves a unit suffix such as `_seconds`
  into the metric's unit, as it does for a classic name.
- Two names that differ only in characters an escaping replaces, such as
  `a.b` and `a_b`, are one name to a scraper that asks for that escaping.
  Two such metrics reach it as a duplicate series. Two such labels of one
  series reach it as one label, with the value of the classic name, `a_b`
  (when both names are escaped, the first in order wins). Keep such names
  distinct, or escape them once with `name_escaping`, which refuses them.

**What the prefix applies to.** `metrics_prefix` applies to every metric the
collector produces, whatever the transform: declared metrics, the names a
Python script passes to `metric(...)`, and the source names a `prometheus`
transform passes through or renames.

- A histogram or summary keeps its family, so `_bucket`, `_sum` and `_count`
  follow the prefixed name. Those names belong to the family. A separate
  metric named `foo_count` next to a histogram or summary `foo`, as a
  `rename` can produce, fails the scrape with an error that names both,
  because the exposition would hold two families of that name.
- `/probe`, OTLP export and static targets all see the same prefixed names.
  The response cache is keyed by the collector's definition, so a changed
  prefix never serves metrics cached under the old names.
- The exporter's own `http_exporter_*` metrics, including a static target's
  health metrics, describe the exporter, not the target, and are never
  prefixed.

A prefix must match `^[a-zA-Z][a-zA-Z0-9]*(_[a-zA-Z0-9]+)*$`: a letter first,
then letters and digits, in parts joined by single underscores.

| Prefix | |
| --- | --- |
| `grafana`, `vendor_eu`, `acme2` | Valid |
| `grafana_` | Invalid: the exporter adds the `_`, which would double it |
| `_grafana`, `a__b` | Invalid: names starting with `__` are reserved by Prometheus, and a double underscore anywhere reads as one |
| `grafana:cloud` | Invalid: `:` is reserved for recording rules |
| `1grafana`, `graf-ana` | Invalid: not a metric name |

An invalid prefix stops the exporter at startup, and `--dry-run` reports it
and names the collector. The same happens for a declared metric whose
prefixed name would be longer than `limits.max_metric_name_length` (200 by
default), and for a rule's name that exceeds the limit without a prefix
([Checked when the configuration loads](#checked-when-the-configuration-loads)).
A name produced at scrape time, by a Python script or a `prometheus`
transform, is checked against the same limit when the scrape happens.

Log lines and probe errors name a metric rule as it is written in the
configuration, without the prefix, so that it can be found in the file. The
prefix is added unconditionally: a rule already called `grafana_status`
becomes `grafana_grafana_status`. Remove the prefix from the rule names when
you add it to the collector.

### Metrics per item

A jq or yq metric can set `items`, which selects the things the metric is
about, such as servers, rows or components. The value and every label are
then evaluated once per item, with that item as `.` and the whole document
available as `$root`:

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

**Without `items`**, the value expression and each label expression run over
the whole document and are paired by position: the third value gets the
third label value. A label that yields one value applies it to every series,
and one that yields none leaves the label off.

Any other count would put values on the wrong series; for example, a label
that yields nothing for one server would shift every later label. The metric
therefore fails under its `error_mode`, with an error that says how many
values the label gave for how many series. The metric fails as a whole,
once: none of its series is exported, and one failure is counted and logged
for it (the label's, not an additional one for each value that could not be
read). Under `fail`, the scrape's error names the pairing mistake. The same
applies to anything else that fails the metric as a whole: its expression
failing at any of its values, a label expression failing, or a label value
that is an object or a list.

**With `items`** there is nothing to pair. A label that yields nothing for an
item is simply absent on that series.

- Per item, the value and each label must yield at most one value. Two is an
  error, because there is no way to tell which belongs to the series.
- A value that is missing or null for one item is that item's missing
  metric, handled by `required` and `error_mode` like any other: `log` drops
  that one series and keeps the rest.
- `items` that selects nothing is a missing metric when the metric is
  required, and produces nothing when it is not.

**Variables.** Every jq and yq expression has these variables:

- `$root`: the whole document. It works without `items` too, where it is the
  same document as `.`.
- `$status` and `$headers`: the response's HTTP status, and its headers by
  lower-case name, for a target whose status or headers say something the
  body does not (see
  [Accepting other statuses](REQUESTS.md#accepting-other-statuses)).
- `$timings`, `$tls`, `$url`, `$redirects`, `$http_version` and
  `$remote_ip` describe an `http` or `graphite` collector's request. They
  give how long the phases of the request took, the TLS connection it was
  sent on, where its redirects led, how many redirects there were, the
  version of HTTP that the answer came in, and the address that its
  connection was made to. Each is `null` for other types (see
  [Timings, TLS and the final URL](REQUESTS.md#timings-tls-and-the-final-url)).
- `$params`: the [probe's parameters](REQUESTS.md#in-expressions) by name.
- `$scrape_time` and `$last_success`: when the scrape began, and when the
  last successful scrape of the same target and parameters began, in seconds
  since the epoch. `$last_success` is `null` while there is none
  ([What a script has](PYTHON.md#what-a-script-has)), so
  `$scrape_time - $last_success` is how long ago that was.

### Labels from an object

A rule can take its labels from an object of the response, such as the
labels of a Kubernetes object, where the names are the target's and cannot
be listed in advance. `labels_from` is an expression that gives the object.
Each key becomes a label, with the key's value as its value. With `items`,
the expression is evaluated for each item, for that item's series:

```yaml
name_escaping: underscores        # app.kubernetes.io/name -> app_kubernetes_io_name
metrics:
  - name: kube_pod_labels
    items: .items[]
    expression: "1"
    labels_from: .metadata.labels
    labels:
      - name: pod
        expression: .metadata.name
```

- A value is written as a label expression's value is: a number or a boolean
  as its text.
- A key whose value is `null` or empty produces no label, and neither does an
  item without the object. Anything else that is not an object, and a value
  that is an object or a list, is that item's failure, handled by the rule's
  `error_mode`.
- A label in the rule's `labels` replaces a key of the same name.
- The names are subject to [`name_escaping`](#utf-8-names) like every label
  name, so a name with a dot or a slash needs `underscores` or `values`.

The expression can reshape the object first, to give the names a prefix or
keep only some of them:

```yaml
labels_from: '.metadata.labels | with_entries(.key |= "label_" + .)'
labels_from: '.metadata.labels | with_entries(select(.key | startswith("app.")))'
```

Without `items`, the expression is evaluated once for the whole document,
and its keys are labels of every series of the rule. If it yields anything
other than an object of single values, the rule fails as a whole.

Every key is a label, so the target decides how many series a metric has.
Use `labels_from` only with objects whose keys are few and stable.

The `css` transform takes `items` too, for HTML tables and lists: `items`
selects the rows, and the expression and each label are selectors within one
row. Items may be nested, as with `items: li` over nested lists. Each
selector is matched once for all the items, so deep nesting costs only what
its size does.

Without `items`, the metric is one series. The value is the whole text of
the one element the expression selects, and an expression that matches
several elements is a failure of the rule, handled by its `error_mode`, with
an error that points to `items`. Its labels can only be static `value`
labels: a label that reads the page needs `items` and is refused at startup
without it.

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

The rules are the same as for jq. Within a row, the value and each label
selector must match at most one element. A row without the value cell is
that row's missing metric, and a label selector that matches nothing leaves
the label off.

Quote a selector that contains a `#`, as above. In YAML, a `#` after a blank
starts a comment. Without quotes, `expression: body #proxy` is the selector
`body`, which loads and reads the whole page, and `expression: #proxy` is
not an expression at all: the key has nothing after its colon and is refused
for that reason.

#### The text of an HTML element

Rules over HTML are written against what a browser's inspector shows, and
`css` and `xpath` read the text of an element accordingly. The text is the
text of everything inside the element, without comments and trimmed of
surrounding whitespace, with two exceptions:

- **Scripts and styles.** Text inside a `<script>` or a `<style>` is not part
  of the text of an element around it: `<td>6<script>track(6)</script></td>`
  reads `6`. A rule or a label that selects the `<script>` or the `<style>`
  itself, or a text node inside one, reads its text, so `script#state` and
  `//script[@type='application/ld+json']` still give the data a page keeps in
  a script.
- **Templates.** The content of a `<template>` is not part of the document,
  as in a browser. Selectors, `items`, XPath expressions and labels match
  nothing inside one, so a placeholder row such as
  `<tr><td>{{name}}</td></tr>` is not a row of its table. The `template`
  element itself is present, and empty. A declarative shadow root is the
  exception. A browser renders the content of a `<template>` that has a
  `shadowrootmode` attribute (or the older `shadowroot`), so that content is
  kept, and read as ordinary elements inside the `template` element, which
  `p.shadow` and `//p` find.

Everything else in a page is read as written:

- An element hidden by an attribute or a style is in the document and has
  its text.
- The markup inside `<noscript>` and `<iframe>` is their text and contains
  no elements to select.
- The fallback inside an `<object>` consists of ordinary elements.
- `<pre>` and `<textarea>` keep their text.

Use the selector to exclude what a rule should not read, as in
`tr:not([hidden])` or `//tr[not(@hidden)]`.

This describes how the exporter reads the node that a rule or a label
selects. What an expression itself asks of the library follows the library's
rules. The XPath functions and comparisons (`string(.)`,
`normalize-space(.)`, `sum(//td)`, `td = '6'`) work on XPath's string value,
and `:contains()` searches all the text of an element, including a script's
and a style's. To `sum(//td)` the cell above is `6track(6)`, which is not a
number, so the sum has [no value](#adding-up-nodes-with-sum). An XML document
has no scripts, styles or templates, only elements with those names, which
are read like any other.

#### Reading HTML: what to expect

A page is read as a browser's parser builds it, which is not always how its
source reads, and `css` and `xpath` differ in what they can express:

| The page, or the rule | What happens | What to do |
| --- | --- | --- |
| A table without `<tbody>` | The parser puts the rows in a `tbody` the page did not write, so `table > tr` and `/table/tr` match nothing — and `count(//table/tr)` is a quiet `0`. | `table tr` or `table > tbody > tr`; `//table//tr` or `//table/tbody/tr`. |
| `css` with `items`: the item is the value, as in `<li>12</li>` | The expression and the labels match beneath the item only, so `items: li` with `expression: li` is a missing value for every item. There is no selector for the item itself: `:scope` and a selector starting with `>` are refused when the configuration loads. | `xpath`, where the rule selects the nodes themselves: `//ul[@id='queues']/li`. |
| `css` with `items`: a label that stands above the rows, a heading over the table | It is beneath no row, so the label is left off without a word. | Make the item the element that holds both, where each holds one value; otherwise `xpath`, with `ancestor::div/h2`. |
| A row without the cell a rule reads | Under `css` `items` the row is an item whose value is missing, reported as the rule's `error_mode` and `required` say. An `xpath` rule selects the cells, so the row is never selected and nothing is reported. | `xpath` for rows that may lack the cell; `css` to be told of them. |
| Upper case in the page: `<TD ID="Load">` | Element and attribute names are lower case: `//TD` and `@ID` match nothing in XPath, while a selector's element name matches in any case. Values keep their case: `#load` does not match `id="Load"`. | Write names in lower case, and ids and classes as the page has them. |
| An inline `svg` or `math` | Their names keep their capitals: `//linearGradient` and `@viewBox` match and the lower-case forms do not. A selector lowers the names it is given, so `css` finds neither such an element nor such an attribute by name. An attribute with a prefix, `xlink:href`, is `href` to an expression: `@*[local-name()='href']`, not `name()='xlink:href'`; a label that is the one name, `@xlink:href`, reads it. | `xpath` with the names as the standard writes them; in `css`, an id, a class or the place: `svg > *:first-child`. |
| Label text over several lines | The blanks around it are trimmed and those inside it kept, line breaks and tabs included, in an element's text and in an attribute's value alike. | `normalize-space(.)` in `xpath`; `css` has no such function. |

### Long label values

Label values are limited by `limits.max_label_value_length`, 500 bytes by
default. A value over the limit fails the whole scrape, not just one series,
because a silently shortened value would be a surprise. The failure names
the metric, the label, the value's length in bytes and the limit, and gives
the two remedies: `metric "statuspage_incident" label "message" value is 812
bytes, longer than limits.max_label_value_length 500; a label one of the
collector's rules gives can be cut to fit with truncate: true on that label,
or raise limits.max_label_value_length`.

The metric and the label are named as the scrape exposes them: under the
collector's `metrics_prefix`, and with the name `transform.rename_labels`
gave the label. The label may be one that no rule gives, such as a directory
collector's `file`, or a label of a series that a `prometheus` transform
passes through unchanged. For those, raising the limit is the remedy. A
label that carries free text can ask to be cut instead:

```yaml
labels:
  - name: message
    expression: .latest_update
    truncate: true
```

A longer value is then cut to the limit, on a character boundary, and ends
in `…`, which counts towards the limit.

- A limit of 1 or 2 bytes has no room for the `…`, which is three bytes, so
  the value is cut without it. When the value's first character does not fit
  either (`日本` under a limit of 2), nothing is left, and the label is left
  off the series, like every label with an empty value. It used to be
  exported as `message=""`.
- Truncation applies to declared metrics from every transform, including a
  `prometheus` rule without a `name`. It happens before any `metrics_prefix`
  is added and before `transform.rename_labels`, so a label keeps its
  `truncate: true` under the name a rename gives it.
- It happens after a label's
  [`value_map`](#mapping-text-to-values-and-scaling-them), for every kind of
  rule, so the exported value is the cut one, never a cut value mapped to a
  long one again.
- Like a `value_map`, `truncate: true` applies by the metric's name. Set on a
  label in one rule, it cuts that label on every series with the rule's
  name, including those of other rules with that name and those a
  `prometheus` rule without a `name` keeps under that name.
- Text that is not the UTF-8 it claims to be is
  [repaired](#character-encodings) first, so a value cut to the limit stays
  within it.
- A static label whose value a
  [probe's parameter](REQUESTS.md#in-label-values) fills is measured as the
  filled text and, with `truncate: true`, cut as that text.

**Python scripts.** A `python` script's labels are cut the same way when a
rule of the collector names the script's metric and the label:
`metrics: [{name: up, labels: [{name: note, expression: note, truncate:
true}]}]` cuts the `note` label of the `up` series the script makes. The
rule makes no series itself, and its `expression` is not read. Without such
a rule, the script must cut the values itself, or the limit must be raised.

Naming a label to cut is all that a `python` rule's label does. One that
sets `value`, or that does not set `truncate: true`, is refused at startup:
the script's series would not get the constant, nor the label that an
`expression` there appears to read, and nothing would report it. A constant
for every series of the collector belongs under
[`transform.labels`](#collector-wide-labels), and one for some of them in the
script's `metric(..., labels={...})`. The rule itself only names the script's
series. Its `type`, `description`, `required` and `error_mode` are for the
script to set and are refused, as is a rule without a `name`. See
[What a rule of a `python` collector is for](PYTHON.md#what-a-rule-of-a-python-collector-is-for).

**Other limits on a series.** They report in the same way when a scrape fails
on them:

- More labels than `limits.max_labels_per_metric` (20 by default): `metric
  "M" has 24 labels, more than limits.max_labels_per_metric 20; drop labels
  it does not need or raise limits.max_labels_per_metric`.
- A help text over `limits.max_help_length`: `metric "M" help is 2100 bytes,
  longer than limits.max_help_length 2000; shorten it or raise
  limits.max_help_length`.
- A name over `limits.max_metric_name_length`: `invalid metric name "M":
  longer than limits.max_metric_name_length 200`. This is for a name the
  target or a script gives; a rule's own name over the limit is refused at
  startup.

### Long label names

A label's name is limited by `limits.max_label_name_length`, 200 bytes by
default, as a metric's name is by `limits.max_metric_name_length`.

- A name over the limit fails the whole scrape at validation, whatever gave
  the series the label (a series that a `prometheus` transform passes
  through, a script's `labels={...}`, a rule) and whatever `error_handling`
  says, as a metric name over its limit does. It counts in
  `http_exporter_series_limit_exceeded_total`: `metric "M" label name "L" is
  longer than limits.max_label_name_length 200; rename the label with
  transform.rename_labels, or raise limits.max_label_name_length`.
- A name longer than 200 bytes is shown by its first 200 bytes and its
  length, `"lll..."... (10485760 bytes)`, so a target that writes a label
  name of megabytes is neither served nor quoted in full.
- The name is measured as the scrape exposes it, after `name_escaping`.
- A series whose metric name is also over its limit fails on the metric
  name. A label whose name and value are both over their limits fails on
  the name.
- `le` and `quantile`, which a histogram's buckets and a summary's quantiles
  carry, are label names too. A limit under 2 bytes fails every histogram,
  and one under 8 fails every summary.
- The `file` label that a collector reading a directory adds is checked with
  the rest, and fails the scrape under a limit of 4 bytes.

Names written in the configuration are checked at startup:

- A label name in the configuration (a rule's label, a key of
  `transform.labels`, a `transform.rename_labels` target) that is over the
  limit as it would be exported is refused at startup.
- A rule's label or a key of `transform.labels` that
  `transform.rename_labels` renames or `transform.remove_labels` removes is
  not exported under that name. It is not checked against the limit (its
  new name is), nor against the character rules or the `__` that Prometheus
  reserves ([UTF-8 names](#utf-8-names)).
- A static target's label over its collector's limit is refused when the
  target file is loaded. So is every target of a collector whose limit is
  under 13 bytes, because the static targets endpoint adds `static_target`
  to each of its series, and `collector` and `target` to its health series,
  after the limit is checked.

### Turning a status into metrics

A status such as `operational` or `major_outage` is text, and a metric value
is a number. Put the text in a label and the value `1` on one series per
thing that has a status:

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

Only the current status has a series. When the status changes, the old
series ends and a new one starts, and Prometheus marks the old one stale on
the next scrape. The alternative is one series per possible status, `1` for
the current one and `0` for the rest. That keeps every series alive, at the
cost of one series per status per component. Choose it when you want `== 0`
comparisons or an unbroken history per status. Counts, such as incidents by
impact, are different: a `0` there is a real value, not a status that does
not apply, so keep it.

Keep free text, such as an incident's latest update, off such a series. Its
value changes whenever the text does, and every change starts a new series,
so an alert on the status would reset each time the page posts an update.
Put the text on a separate series that exists only while it matters, with
`truncate: true`, and join on it when you need it:

```promql
statuspage_component_status{status!="operational"}
  and on (component_id) statuspage_component_incident_info
```

[`examples/config.filebeat.json-test.yaml`](../examples/config.filebeat.json-test.yaml)
reads [Filebeat's monitoring endpoint](https://www.elastic.co/guide/en/beats/filebeat/current/http-endpoint.html)
(`/stats`, `/` and `/inputs/`) into `filebeat_*` metrics:

- Counters are exported as `_total`, and milliseconds and nanoseconds as
  seconds. Outcomes of one kind form one family with a label
  (`filebeat_output_events_total{outcome="failed"}`).
- The sections that only some Filebeats report use `error_mode: ignore`, so
  they are absent instead of failing the scrape.
- With the Kafka output, the example also reads the Kafka client's own
  metrics from `libbeat.outputs`, which other outputs do not have: bytes sent
  and received, requests in flight, and requests sent with their latency, as
  `filebeat_output_kafka_request_latency_seconds{quantile="0.99"}` and
  similar series.
- Its third collector, `filebeat_inputs`, reads `/inputs/`, an array with
  one object per running input, with `items: .[]`. It produces a series per
  input and metric, with the labels `id` and `input` (the type):
  - `filebeat_input_pipeline_events_total{id="nginx-access",input="filestream",outcome="published"}`
    for every input;
  - `filebeat_input_files_active`, and the file, message, byte and event
    counters, for `filestream` inputs;
  - `filebeat_input_processing_time_seconds{quantile="0.99"}` for the inputs
    that measure it. Filebeat reports that time as percentiles of a sample,
    not as buckets, so it is exported as gauges with a `quantile` label, not
    as a histogram.
- Every rule in that collector is `required: false`, because an input
  reports only the metrics of its type and a Filebeat may answer `[]`. What
  is not reported has no series, and nothing is logged.

[`examples/open-meteo/`](../examples/open-meteo/config.yaml) reads the current
weather of a place from [Open-Meteo](https://open-meteo.com), which needs no
API key. Its `config.yaml` takes the place from the probe (`latitude:
"{{param_latitude:42.6977}}"`, Sofia unless the probe says otherwise) and asks
for metres per second. It turns the answer into `weather_*` series that carry
the grid point the API answered for, with percentages as ratios and the
observation time as Unix seconds. A repeated probe of one place is answered
from memory for five minutes, and while the API is down the last result is
served, marked stale, for an hour (`cache.ttl`, `cache.stale_if_error`):

```sh
curl 'http://localhost:8080/probe?collector=open_meteo_current&target=https://api.open-meteo.com&param_latitude=43.2141&param_longitude=27.9147'
```

The [`static-targets.yaml`](../examples/open-meteo/static-targets.yaml) beside
it makes the exporter scrape three places itself, every ten minutes; see
[Static targets](STATIC-TARGETS.md#the-target-file).

[`examples/config.ecb.xml-test.yaml`](../examples/config.ecb.xml-test.yaml)
reads the [euro foreign exchange reference rates](https://www.ecb.europa.eu/stats/policy_and_exchange_rates/euro_reference_exchange_rates/html/index.en.html)
that the European Central Bank publishes every working day. The XML document
has every element in a namespace and the data in attributes.

- The collector gives the two namespaces its own prefixes with
  [`response.namespaces`](#xml-namespaces).
- One rule selects the `rate` attributes themselves,
  `//e:Cube[@currency]/@rate`, and reads each one's currency from the
  attribute beside it, `../@currency`.
- A second rule computes its value with `count(...)`.
- A third rule reads the day the rates are for, the `time` attribute, as a
  value with [`time_format`](#reading-a-date-or-a-time-as-the-value), so no
  series changes its labels from one day to the next.

```sh
curl 'http://localhost:8080/probe?collector=ecb_reference_rates&target=https://www.ecb.europa.eu'
```

```text
ecb_euro_reference_rate{currency="USD"} 1.1712
ecb_euro_reference_rates{sender="European Central Bank"} 29
ecb_euro_reference_rates_timestamp_seconds 1.7908992e+09
```

[`examples/config.scrapethissite.html-test.yaml`](../examples/config.scrapethissite.html-test.yaml)
reads an HTML page with the `css` transform:
[Countries of the World](https://www.scrapethissite.com/pages/simple/) on
scrapethissite.com, a site made for practising scraping. The page has one
`div.country` per country. [`items`](#metrics-per-item) selects those blocks,
and the population, the area and the country's name are selectors within one
block. The name is a [`required`](#collectors) label, so every country is a
series of each metric, named by the page itself:

```sh
curl 'http://localhost:8080/probe?collector=countries_html&target=https://www.scrapethissite.com'
```

```text
country_population{country="Andorra"} 84000
country_area{country="Andorra"} 468
```

[`examples/metar/`](../examples/metar/config.yaml) reads an airport's latest
weather report, its METAR, from the text file the US National Weather Service
keeps for every station in the world (`LBSF 030900Z 12004KT 9999 FEW043
BKN100 14/08 Q1021 NOSIG`), with the `text` decoder and one regex per metric.

- The station is a [path parameter](REQUESTS.md#path-parameters),
  `{{param_station:LBSF}}`.
- Every regex except one starts at the report's line, with the station's
  code in a group that a `station` label reads and the number further along
  in the group named `value`. The regex for the report's time starts at the
  line before it.
- A report may end with a forecast (`BECMG 27015G25KT 3000`) and with
  remarks, whose groups are written like the observation's own. Each rule
  therefore states where its group is and does not search the whole line:
  the wind directly after the station and the time, the visibility directly
  after the wind, the pressure directly after the temperature. A report with
  no gust, a variable wind or a wind in metres per second then gives no such
  series, and never the forecast's.
- A report omits what it has nothing to say about and writes some things in
  one of two ways, so the rules are `required: false`, and two rules may
  feed one metric. A temperature is read by one rule as `14` and by another,
  with `scale: -1`, as `M05`. A pressure in inches of mercury (`A3012`) is
  scaled to hectopascals beside the rule for `Q1021`. Knots become metres per
  second in the same way.
- The line before the report, `2026/10/03 09:00`, is when it was made.
  [`time_format: "2006/01/02 15:04"`](#reading-a-date-or-a-time-as-the-value)
  reads it as `metar_observation_timestamp_seconds`.

```sh
curl 'http://localhost:8080/probe?collector=metar&target=https://tgftp.nws.noaa.gov&param_station=EGLL'
```

The [`static-targets.yaml`](../examples/metar/static-targets.yaml) beside it
scrapes three airports every ten minutes, each adding its `city` label to the
`station` the collector reads from the report.

[`examples/config.mempool.json-test.yaml`](../examples/config.mempool.json-test.yaml)
reads three endpoints of [mempool.space](https://mempool.space/docs/api/rest)'s
REST API, which every self-hosted mempool instance also serves. Its three
collectors share one request through a YAML anchor:

- the recommended fee rates, one series per key of the answer with
  [`items: to_entries[]`](#metrics-per-item), with the keys renamed by a
  label's [`value_map`](#mapping-text-to-values-and-scaling-them);
- the backlog of unconfirmed transactions, with its fees scaled from
  satoshis to bitcoin;
- the height of the newest block, an answer that is only a number, read as
  text by a regex.

[`examples/config.promdemo.prometheus-test.yaml`](../examples/config.promdemo.prometheus-test.yaml)
passes a Prometheus server's own `/metrics` through, from the project's
[public demo server](https://prometheus.demo.prometheus.io) or any other. It
has no metrics rules. [`include` and `exclude`](#collector-wide-labels) keep
about two dozen families out of everything the server exposes, including a
whole histogram and a whole summary. `rename` gives the `process_*` and
`go_*` families, whose names every Go program exposes, names specific to the
server:

```sh
curl 'http://localhost:8080/probe?collector=prometheus_server&target=https://prometheus.demo.prometheus.io'
```

[`examples/config.grafanastatus.json-test.yaml`](../examples/config.grafanastatus.json-test.yaml)
is a complete collector for [status.grafana.com](https://status.grafana.com),
and for any page hosted on Atlassian Statuspage, because they all publish the
same `/api/v2/summary.json`:

```sh
curl 'http://localhost:8080/probe?collector=statuspage&target=https://status.grafana.com'
```

It exposes the page's overall indicator, every component and component group,
unresolved incidents by impact, scheduled maintenances by status, and the
start of the next maintenance. Component names repeat on that page: the same
region appears under most product groups, and occasionally twice in one
group. Each component series therefore also carries `component_id` and
`group`, which keeps the series distinct. The group is found through `$root`:

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
nothing and so has neither label.

For every component that is not operational, the example also exposes
`statuspage_component_incident_info`. Its labels name the incident or
maintenance in progress behind the status and carry that event's latest
update as `message`, collapsed to one line and cut to 300 bytes by
`truncate: true`. Maintenance that is only scheduled is not counted as the
cause. A component marked down with no event behind it still gets the
series, without those labels.

No series carries the page's name. Prometheus labels every series with the
target it probed (`instance`), which already distinguishes two pages.
`statuspage_info{page="Grafana Cloud"}` carries the name once, for
dashboards.

[`examples/tcp/`](../examples/tcp/config.yaml) reads three services that
report their statistics as text over TCP, with the [`tcp`](TCP.md) request
type: memcached's `stats`, read up to its `END` line, ZooKeeper's `mntr`,
and HAProxy's `show stat`. The first two are read with regular expressions,
and HAProxy's CSV by its columns. Its
[`static-targets.yaml`](../examples/tcp/static-targets.yaml) scrapes a
memcached pair, a ZooKeeper ensemble and an HAProxy:

```sh
curl 'http://localhost:8080/probe?collector=memcached&target=localhost:11211'
```

### Mapping text to values and scaling them

When a status has an order, such as up, degraded and down, one number per
status is simpler than a series per status. `value_map` turns the text an
expression gives into the value, and `"*"` maps any value it does not list:

```yaml
- name: app_state
  description: 1 up, 0.5 degraded, 0 down, -1 anything else
  expression: 'state: (\w+)'
  value_map:
    up: 1
    degraded: 0.5
    down: 0
    "*": -1
```

The value is looked up as text:

- a regex capture, the text of a CSS element or an XPath node, or a CSV
  cell, without surrounding blanks;
- a jq or yq number as JSON writes it (`7`, `0.5`);
- a boolean as `true` or `false`.

Letter case matters. `"*"` catches numbers too. Without it, a value the map
does not list is read as a number, and one that is not a number fails the
rule with a message that says so.

The error shows the value as you would read it. Text is quoted and cut to
its first 64 bytes when it is longer: `metric "state": value "n/a" is not a
number; map text to numbers with value_map`. An object or an array is
described by what it is, not by its whole content: `value is an object with
2 keys, not a number`, `an array of 3 items`, `null`. That usually means the
expression stops one field short. A jq or yq boolean, `true` or `false`, is
read as `1` or `0`. The same words as text, in a CSV cell or an HTML
element, are [not numbers](#what-counts-as-a-number).

#### What counts as a number

Text is a number when it is written as one:

- an integer or a decimal, with or without digits on either side of the
  point (`42`, `-17`, `007`, `3.14`, `.5`, `5.`);
- an exponent (`1.5e3`, `2E-3`) and a leading `+` or `-`;
- `NaN`, `Inf` and `Infinity` in any letter case, the last two with or
  without a sign.

Surrounding blanks do not matter. An integer too long for a float is read as
the nearest float: `9007199254740993` is `9007199254740992`. A number too
small for a float is `0` (`1e-400`). A number too large is not rounded to
infinity; it fails its rule (`value "1e400" is beyond the range of a 64-bit
float`). These rules apply equally to a CSV cell, a regex capture, the text
of a CSS element or an XPath node, and a jq or yq string.

Anything else is text and fails its rule as `value "..." is not a number`:

- a thousands separator (`1,234`, `1 234`, `1_000`) or a decimal comma
  (`1,5`);
- a percent sign or a unit (`85%`, `12.5 MB`), or a currency;
- hexadecimal, octal and binary (`0x1F`, `0x1p-2`, `0o17`, `0b101`);
- digits that are not ASCII;
- the words that exports write in place of a value: `N/A`, `-`, `null`, and
  `true`, `false`, `yes`, `no`, `on` and `off` written as text (a JSON or
  YAML boolean is `1` or `0`).

An empty cell is not a failure of this kind but a
[missing value](#when-a-metric-cannot-be-extracted).

The remedies are:

- The [value-parsing helpers](#value-parsing-helpers), which read a size, a
  duration, a time or a percentage written for people (`12.5 MB`, `1h2m`,
  `85%`) in a jq or yq expression or a Python script.
- `value_map`, for texts that stand for a value (`"N/A": 0`, `"yes": 1`,
  `"1_000": 1000`).
- A `pre_script`, which can rewrite a column before the rules read it: read
  `85%` with `parse_percent`, or turn `1.234,56` into `1234.56`.

`value_map` reads only the texts it lists, so it suits a handful of words,
not a column of decimal commas. That takes two lines of a pre-script:

```yaml
transform:
  type: csv
  pre_script: |
    for row in data:
        row["price"] = row["price"].replace(".", "").replace(",", ".")
```

Two kinds of value are already numbers before the exporter reads any text,
and keep the number their source produced:

- A number that an XPath function or operator computes (`number(//v)`,
  `sum(//v)`, `//v * 1`) is converted by the XPath engine, which reads
  `1_000` as 1000 and `0x1p-2` as 0.25. Select the node itself, `//v`, to
  have its text read by the rules above.
- An unquoted value of a YAML response is a number by YAML's own syntax:
  `v: 1_000` is 1000, `v: 0x10` is 16 and `v: 017` is 15. Only a quoted
  YAML string, like a JSON string, is text to a rule.

The numbers in the configuration itself follow YAML: `scale: 1_000` is 1000
there.

`NaN`, `Inf` and `Infinity` are numbers where a response writes them as the
text of a cell or an element. That is the value the source gives, and it is
exported, so `//v` over `<v>NaN</v>` is a series whose value is NaN. A NaN
that an XPath expression computes is different: `number(//v)` over
`<v>n/a</v>` is the XPath engine reporting that it could not read a number.
That is the rule's [missing value](#when-a-metric-cannot-be-extracted), with
the error `metric "v" XPath "number(//v)" computed NaN, not a number`. The
first is what the source says, and the second is what the engine says about
text that was not a number. A `sum()` over nodes of which one is `NaN`
results in NaN and is computed, so it is a missing value too. A `sum()` over
infinities of one sign is that infinity, and is exported.

`scale` multiplies the value, whether mapped or read as a number: `0.001`
for milliseconds to seconds, `100` for a fraction to a percentage. A scale
that is written as exactly one over a whole number, such as `0.001`,
`0.000000001`, `1e-9` or `0.5`, divides by that number instead. 412
milliseconds are then `0.412` seconds, not the `0.41200000000000003` that
multiplying by the binary approximation of `0.001` gives. A scale that only
comes close to such a number, such as `0.3333333333` or `1.5e-9`,
multiplies like any other:

```yaml
- name: api_latency_seconds
  expression: .latency_ms
  scale: 0.001
```

Both work in every transform with rules: `regex`, `css`, `xpath`, `csv`, `jq`
and `yq`.

A label takes a `value_map` too, from text to text, for example from a code
to its name, with `"*"` for any other value. Without a match and without
`"*"`, the value is kept as it is. A value mapped to `""` leaves the label
off:

```yaml
- name: worker_up
  items: .workers[]
  expression: 1
  labels:
    - name: state
      expression: .state
      value_map: {"1": running, "2": stopped, "0": "", "*": unknown}
```

Restrictions:

- A label's `value_map` needs a rule with a `name`, and an expression to
  map. It is refused on a static label, on a `python` rule and, when it maps
  a value to `""`, on a `required` label.
- A `prometheus` rule takes `scale`, for plain samples only (the bounds of a
  histogram or summary are values too), and not `value_map`, because its
  values are already numbers.
- A `python` script sets its values itself and takes neither.
- `scale` must be a finite number other than 0.

A map that is long, or shared by many rules, can go in a file.
`value_map_file` names one, on a rule or on a label, together with
`value_map` or in its place:

```yaml
- name: device_state
  items: .devices[]
  expression: .state
  value_map_file: /etc/exporter/maps/states.yaml
  labels:
    - name: site
      expression: .site_id
      value_map: {"0": unknown}
      value_map_file: /etc/exporter/maps/sites.yaml
```

- The file is a YAML or JSON mapping with one key and value per line
  (`"17": Frankfurt`), of at most 10,000 entries and 1 MiB. A rule's file
  maps to numbers, and a label's to text.
- It is read when the configuration loads and merged with the `value_map`
  beside it, whose entries take precedence. From then on the rule behaves as
  if the whole map were written in it.
- The path is absolute.
- With `--config.watch`, an edit of the file reloads the configuration. A
  file that is missing or is not a mapping refuses the load, with an error
  that names the rule and the file.
- The [target allocator](ALLOCATOR.md) does not open the file. Mount it on
  every replica.
- A collector written on the [debug page](#debugging-collectors) cannot name
  one, outside [dangerous debug mode](#dangerous-debug-mode).

Rules with the same `name` make one metric, so a label they share must be
mapped the same way. Two such rules that give one label different
`value_map`s are refused at load, because the same value would read as two
names in one series. Give them one `value_map`, or different names. The map
applies by the series' name: a `prometheus` rule without a `name` keeps each
series' own name, and its series with a name that another rule has take that
rule's `value_map`, including its static labels. A label with
`truncate: true` is cut after it is mapped, so the exported value is within
the limit (see [Long label values](#long-label-values)).

#### Adding up nodes with `sum()`

The XPath engine's `sum()` silently leaves out every node it cannot read as
a number. A number with blanks around it, such as the cell of a table
printed one cell per line, is one it cannot read. The sum of a column was
therefore a wrong number, often `0`, with no error, although XPath itself
defines the sum of text that is not a number as NaN. The exporter therefore
reads the nodes of a `sum()` itself, and a sum has a value only when every
node it adds up is a number:

- An expression that is one `sum(...)` and nothing else, such as
  `sum(//td[@class='bytes'])`, is added up by the exporter. It takes each
  node's text without surrounding blanks and reads it as the engine reads a
  number. A node that is not a number (`n/a`, `1,234`, `12 MB`, an empty
  cell) leaves the rule
  [missing its value](#when-a-metric-cannot-be-extracted), handled by
  `required` and `error_mode` like any missing value:
  `metric "bytes" HTML XPath "sum(//td[@class='bytes'])" cannot be computed:
  it adds up text that is not a number, first "n/a" (2 of 14 nodes)` (over
  XML the message says `XPath`). Exclude such nodes with a predicate, as in
  `sum(//td[@class='bytes'][number(.) = number(.)])`. Alternatively, select
  the nodes with a rule of their own, where `value_map` or a `pre_script`
  turns the text into numbers, and add the series up in PromQL.
- Where `sum()` is part of a larger expression, as in
  `sum(//v) div count(//v)` or `round(sum(//v))`, the engine computes it
  with the rest and reads each node as it is. The rule is missing its value
  when the engine would leave a node out, and here blanks around a number
  count: `metric "mean" XPath "sum(//v) div count(//v)" cannot be
  computed: sum(//v) leaves out text it cannot read as a number, first
  " 12 " (1 of 3 nodes), and blanks around a number count`. XPath 1.0
  cannot trim each node of a set (`normalize-space()` takes one node), so
  select the nodes with a rule of their own and add the series up in
  PromQL, or rewrite the text in a `pre_script`.
- A label follows the same rule: `sum(../td)` is the sum of all its cells,
  and a label with a cell that is not a number cannot be read. A label has
  no missing value, so its series fails, as the metric's `error_mode` says,
  with an error that names the metric, the node and the label. The series of
  the other nodes are still produced.

This covers the calls of `sum()` that are evaluated where the expression is,
that is, outside every predicate.

- A `sum()` inside a predicate, as in `//row[sum(v) > 10]`, is evaluated by
  the engine for each node the predicate is applied to, and still leaves out
  what it cannot read.
- An argument that is a number or a string, not a node-set, is also left to
  the engine. `sum(count(//v))` is that number. A string, as in
  `sum(string(//v))` or `sum(translate(//v, ',', ''))`, is the number it
  reads as, with no surrounding blanks. On a string that is not a number
  the engine fails, which is
  [the rule's failure](#when-the-xpath-engine-fails-on-an-expression):
  `metric "v" XPath "sum(string(//v))" cannot be evaluated: the XPath engine
  failed on it: sum() function argument type must be a node-set or number`.
  Use `number(...)` for one value that may not be a number; that is then the
  rule's missing value.
- A prefix before the name is ignored, as the engine ignores it:
  `fn:sum(//v)` is `sum(//v)`, alone and as part of a larger expression.

The nodes of a sum are read whether or not the engine would actually
evaluate it, as on the right of an `or` whose left side is true. Where the
engine fails on reading them, as it does on a predicate that it cannot
evaluate over the response, the sum is left to the engine. The rule then has
its value when the engine never reaches the call, and fails when it does. A
sum whose nodes are all numbers without blanks gives the same number as
before.

### Reading a date or a time as the value

A date or a time in a response, such as when a report was made, when a job
last ran or when a certificate expires, is worth a series whose value is
that time in Unix seconds. `time() - app_last_backup_timestamp_seconds` then
gives how long ago it was. `time_format` declares that the text an
expression gives is a time, and how it is written:

```yaml
- name: app_last_backup_timestamp_seconds
  expression: .backup.finished_at        # "2026-10-03 09:00:07"
  time_format: "2006-01-02 15:04:05"
  time_zone: Europe/Sofia
```

`time_format` is a name, in any letter case, or a layout:

- `rfc3339` reads `2026-10-03T09:00:00Z` and `2026-10-03T12:00:00.25+03:00`,
  with or without a fraction of a second.
- `rfc1123` reads the date of an HTTP header, `Sat, 03 Oct 2026 09:00:00 GMT`,
  and the same with a numeric zone, `+0300`.
- A layout is the reference time, `Mon Jan 2 15:04:05 MST 2006`, written the
  way the text writes its times. This is the convention of Go's `time`
  package, which Promtail and Telegraf also use for layouts. Each part of
  the reference time has its own number, so the layout shows where each part
  is:

| Part | Written in a layout as |
| --- | --- |
| Year | `2006`, or `06` for two digits |
| Month | `01`, `1`, `Jan` or `January` |
| Day | `02`, `2`, or `002` for the day of the year |
| Weekday | `Mon` or `Monday` |
| Hour | `15`, or `03` or `3` with `PM` |
| Minute | `04` |
| Second | `05`, which reads a fraction after the seconds too, of any length or none; `05.999999999` does the same, and `05.000` reads exactly three digits |
| Zone | `Z07:00` (`Z` or `+03:00`), `-0700`, `-07:00`, or `MST` for an abbreviation |

| Text | `time_format` |
| --- | --- |
| `2026-10-03` | `"2006-01-02"` |
| `2026/10/03 09:00` | `"2006/01/02 15:04"` |
| `03.10.2026 09:00:07` | `"02.01.2006 15:04:05"` |
| `Oct 3, 2026 9:00 PM` | `"Jan 2, 2006 3:04 PM"` |
| `20261003T090007Z` | `"20060102T150405Z07:00"` |

Quote a layout, so that YAML passes it on as text. The dots, the slashes, a
`T` between the date and the time, and any other text that is not one of the
parts above stand for themselves.

A part is a part wherever it appears, whatever was intended:

- the digits `1` to `5` and `01` to `06`, even inside a longer number, and
  `15`, `2006` and `002`;
- the names `Jan`, `January`, `Mon` and `Monday`;
- `PM`, `pm` and `MST`;
- `-07` and `Z07`;
- zeros or nines after a dot or a comma.

In `"Q4 2006-01-02"` the `4` is the minute, so the text may have any number
there. The `3` of `"… UTC+3"` is an hour, and the layout is refused because
it has no `PM` beside it. When the text has such digits or words around its
time, make the rule's expression return the time alone, as a regex capture
group does, and write the layout for that.

Fractions and two-digit years:

- A fraction of a second needs nothing in the layout: seconds written `05`
  read `07`, `07.5` and `07,123456` alike. Write `05` or, to the same
  effect, `05.999999999`.
- Zeros after the seconds stand for exactly that many digits: `05.000` reads
  `07.123` and neither `07` nor `07.5`.
- A two-digit year, `06`, is a year from 1969 to 2068: `69` to `99` are in
  the 1900s.

A layout is written without blanks before or after it, as the text is read
without its own. The hour of a 12-hour clock needs `PM` (or `pm`) in the
layout: `"2006-01-02 03:04:05"` could read no afternoon, so it is refused.
Write `15` for a 24-hour clock.

The value is the time in Unix seconds, with any fraction of a second kept.
[`scale`](#mapping-text-to-values-and-scaling-them) applies afterwards:
`1000` gives milliseconds. A time already written as a number of Unix
seconds needs no `time_format`, and one in milliseconds needs only
`scale: 0.001`.

**Time zones.**

- A text that names its zone or its offset from UTC is read by it, when the
  layout has the zone as one of its parts: `Z07:00`, `-0700`, `-07:00` or
  `MST`.
- A text that does not is read in `time_zone`, an IANA name such as
  `Europe/Sofia` or `America/New_York`, with the offset that zone has on
  that day, including summer time. Without `time_zone` it is read as UTC.
- A zone typed into the layout literally, as the text has it (the `Z` of
  `"2006-01-02T15:04:05Z"`, a `UTC` or a `GMT`), is text to match, not a
  zone. Such a text is therefore read in `time_zone` as well. Write `Z07:00`
  or `MST` in its place.
- The zones are built into the binary, so they are known in an image or on a
  host that has no zone files.
- An abbreviation in the text (`MST` in the layout) is read as one of
  `time_zone`'s own (`EET` and `EEST` with `Europe/Sofia`), or as `UTC` or
  `GMT`. `GMT+3` and `GMT-5`, that is `GMT` with a whole number of hours,
  are that many hours ahead of UTC or behind it. Any other abbreviation
  fails the rule, because an abbreviation alone does not determine an
  offset.

On the night the clocks change, a local time without a zone of its own is
one of two moments, or none. It is then read as Go's `time.Date` reads it,
by the offset before the change or the one after, with no guarantee of
which. In `Europe/Sofia`, `2026-10-25 03:30`, which the clocks show twice,
is read as the second occurrence, in winter time (01:30 UTC), and
`2026-03-29 03:30`, which they skip, as an hour after 02:30 (01:30 UTC). A
source that writes UTC or its offset has no such hour.

**What is read, and failures.** The text is read without its surrounding
blanks, like `value_map`'s. `time_format` works in every transform with
rules: on a regex capture, the text of a CSS element or an XPath node, a CSV
cell, and a jq or yq string.

- A jq or yq value that is not text, such as a number or an object, fails
  the rule with a message that says `time_format` reads text.
- An empty or absent text is a missing value, as for any rule.
- Text that is not a time in the format fails the rule, under its
  [`error_mode`](#when-a-metric-cannot-be-extracted), with an error that
  names the text and the format: `metric "app_last_backup_timestamp_seconds":
  value "never" is not a time in time_format "2006-01-02 15:04:05"; write
  the layout as the text writes the reference time, Mon Jan 2 15:04:05 MST
  2006`.

**Checks at load.** All of the following is
[checked when the configuration loads](#checked-when-the-configuration-loads):

- `time_format` and `value_map` cannot be combined, because each turns the
  text into the value. Neither a `python` nor a `prometheus` rule takes
  `time_format`.
- A layout must have a year, a month and a day. `15:04` alone is not a point
  in time, and `yyyy-mm-dd` or `%Y-%m-%d`, layouts of other conventions,
  contain no part of the reference time at all.
- A sample date, `2026-10-03`, or the name of another format, `iso8601`, is
  read as a layout too, with its digits as parts. It is refused for what
  that layout lacks or cannot read, with a message that ends by explaining
  what a layout is.

Either key written empty, `time_format: ""` or `time_zone: ""`, counts as
the key left out, like every optional key of free text in the configuration:

- A rule with `time_format: ""` reads its value as a number and may have a
  `value_map`.
- `time_zone: ""` is UTC beside a `time_format`, and has no effect without
  one.
- `time_format: ""` beside a `time_zone` that names a zone is refused as
  `time_zone` without `time_format`.

The [schema](#editor-support) describes each key the same way.

### Value-parsing helpers

Targets often write a value for people instead of as a number. Redis's
`used_memory_human` is `1.00M`, an API reports an uptime as `1h2m` and a
load as `55.12%`, a status page shows `Sat, 03 Oct 2026 09:00:00 GMT`, and a
Kubernetes object requests `250m` of CPU and `2Gi` of memory. Five helpers
read those forms, so that a rule does not need its own regular expression to
take them apart. In every jq and yq expression they are functions of their
input:

```yaml
- name: redis_memory_used_bytes
  expression: .info.memory.used_memory_human | parse_bytes   # "1.00M" -> 1048576
- name: app_uptime_seconds
  expression: .uptime | parse_duration                      # "1h2m3.5s" -> 3723.5
- name: app_last_backup_timestamp_seconds
  expression: .backup.finished_at | parse_time              # "2026-10-03T09:00:00Z" -> 1791018000
- name: app_cpu_ratio
  expression: .cpu | parse_percent                          # "55.12%" -> 0.5512
- name: kube_pod_container_memory_request_bytes
  expression: .resources.requests.memory | parse_quantity   # "2Gi" -> 2147483648
```

A [Python](PYTHON.md) script or pre-script has them as functions of the same
names, as in `parse_bytes(row["size"])`. The jq and Python versions give the
same number and fail with the same message for every input: both are tested
against one file of test vectors, `testdata/values/helpers.json`.

| Helper | Reads | Gives | Examples |
| --- | --- | --- | --- |
| `parse_bytes` | A size: a number, then blanks or none, then a unit or none | Bytes | `512` and `512B` → 512; `1.00M`, `1M` and `1 MiB` → 1048576; `1.5K` → 1536; `2.5 GB` → 2500000000; `1kB` → 1000 |
| `parse_duration` | A duration as Go writes one, as a clock, or in ISO 8601 | Seconds | `1h2m3.5s` → 3723.5; `500ms` → 0.5; `2d4h` → 187200; `01:02:03` → 3723; `1-02:03:04` → 93784; `PT1H2M3S` → 3723; `P1DT2H` → 93600 |
| `parse_time` | A date and time with its zone: RFC 3339, or an HTTP date | Unix seconds | `2026-10-03T09:00:00Z`, `2026-10-03T12:00:00.25+03:00` → 1791018000, 1791018000.25; `Sat, 03 Oct 2026 09:00:00 GMT` → 1791018000 |
| `parse_percent` | A number followed by `%` | A ratio | `55.12%` and `55.12 %` → 0.5512; `100%` → 1; `-3.5%` → -0.035 |
| `parse_quantity` | A [Kubernetes quantity](https://kubernetes.io/docs/reference/kubernetes-api/common-definitions/quantity/): a number, then a suffix or none | Its base unit: cores, bytes, a count | `250m` → 0.25; `2Gi` → 2147483648; `500M` → 500000000; `1e3` → 1000; `-100m` → -0.1 |

The exact rules for each:

- **A number** is digits, with or without a fraction after a dot (`3`, `3.5`,
  `.5`, `5.`), and at most 100 digits. It has no sign (except where
  `parse_duration`, `parse_percent` and `parse_quantity` accept one, below),
  no exponent such as `1e3` (except as `parse_quantity`'s suffix), no
  thousands separator and no decimal comma. Blanks around the whole text
  (spaces, tabs and line ends) are dropped, as a rule drops them around a
  value.
- **`parse_bytes`**: no unit and `B` are bytes; `KiB`, `MiB`, `GiB`, `TiB`,
  `PiB` and `EiB` are powers of 1024 (IEC); `kB`, `MB`, `GB`, `TB`, `PB` and
  `EB` are powers of 1000 (SI); and a bare letter, `K`, `M`, `G`, `T`, `P` or
  `E`, is a power of 1024, as Redis, `ls -h`, `df -h` and Java's `-Xmx` write
  sizes. The unit is read in any case, so `KB`, `kb` and `mib` are units too,
  and a lower-case `b` is bytes, not bits. Blanks between the number and the
  unit are allowed. A sign, an unknown unit (`1 Ki`, `1 KB/s`, `1 bytes`) or
  anything after the unit fails.
- **`parse_duration`**, in one of three forms:
  - as Go's `time.ParseDuration` reads it, with `d` for a day of 24 hours:
    numbers each followed by its unit, `ns`, `us` (or `µs`), `ms`, `s`, `m`,
    `h` or `d`, written together (`1h2m3.5s`, `1.5h`, `2d4h`), or `0` alone.
    Only this form takes a leading `-` or `+`, as Go's does: `-1.5s` is
    -1.5. The units are in lower case, and there are no blanks between the
    parts.
  - as a clock: `MM:SS`, `HH:MM:SS`, and with days `D-HH:MM:SS` or
    `D.HH:MM:SS`, as `ps -o etime`, Slurm and .NET write elapsed time, with a
    fraction of a second after the seconds (`01:02:03.250`). The first field
    is as long as it needs to be (`100:00:00` is 100 hours, `75:00` 75
    minutes); each after it is two digits, under 24 for the hours after days
    and under 60 for minutes and seconds. Two fields are minutes and
    seconds, not hours and minutes.
  - in ISO 8601: `P`, then weeks (`W`) and days (`D`), then `T` and hours
    (`H`), minutes (`M`) and seconds (`S`). Each part is optional, but at
    least one must be present. The parts are in that order and in upper
    case, and only the last part may have a fraction: `PT1H2M3S`, `P1DT2H`,
    `PT0.5S`, `P2W`. Years and months (`P1Y`, `P1M`) are refused, because
    they have no fixed length in seconds.
- **`parse_time`** reads
  - RFC 3339 and the ISO 8601 date and time that it is a form of, such as
    `2026-10-03T09:00:00Z`. Between the date and the time stands `T`, `t` or
    a blank. A fraction of a second is optional. The zone is `Z` (or `z`) or
    an offset: `+03:00`, `+0300` or `+03`;
  - the HTTP date of RFC 1123, `Sat, 03 Oct 2026 09:00:00 GMT`, with `GMT`,
    `UTC` or an offset such as `+0300`, the day in one digit or two, and
    the names of the day and the month in any case. The day of the week
    must be the date's.

  Any year from `0000` to `9999` is read, in the Gregorian calendar extended
  back before its adoption. A time before 1970 is negative:
  `1969-12-31T23:59:59Z` → -1.

  A date and time without a zone or an offset, `2026-10-03 09:00:00`, is
  refused instead of being read as UTC, because it does not identify one
  point in time. Seconds must be written and be under 60, and the date must
  exist (`2026-02-30` fails). To read a time in any other layout, use a
  rule's [`time_format`](#reading-a-date-or-a-time-as-the-value), which
  takes a layout and a `time_zone`, or `strptime` and `mktime` in jq. The
  helper reads only the two forms that carry their zone in the text.
- **`parse_percent`**: a number with or without a sign, optional blanks, and
  a `%`, which is required. It gives the ratio, a hundredth of the
  percentage, because Prometheus keeps ratios in their base unit, 0 to 1.
  `55.12%` is `0.5512`, consistent with the other ratios of a target, and a
  query needs no division by 100. Multiply by 100 with the rule's `scale` to
  keep the percentage.
- **`parse_quantity`**: a number with or without a sign, followed directly,
  with no blank, by a case-sensitive suffix, as Kubernetes writes CPU,
  memory, storage and counts:
  - `Ki`, `Mi`, `Gi`, `Ti`, `Pi` and `Ei` are powers of 1024.
  - `n`, `u`, `m`, `k`, `M`, `G`, `T`, `P` and `E` are powers of 1000, from
    a billionth to 10^18, so `m` is a thousandth and `M` a million.
  - `e3` or `E-3` (`e` or `E`, an optional sign, and one or two digits) is a
    power of ten.

  It gives the quantity in its base unit: `250m` of CPU is 0.25 cores, and
  `2Gi` of memory is 2147483648 bytes. `K`, a blank before the suffix, and
  anything after the suffix fail.

Each number is read exactly and rounded to a float once. `1.1GB` is
1100000000 and `0.1s0.2s` is 0.3, not 1100000000.0000002 and
0.30000000000000004, which multiplying and adding floats along the way would
give.

Each helper reads text. `parse_bytes`, `parse_duration` and `parse_quantity`
also accept a number (a JSON or YAML number in jq, an `int` or a `float` in
Python), treat it as bytes, seconds or a quantity already, and return it
unchanged, because an API that writes `"1.5K"` for one value may write
`1536` for another.

Anything else fails with a message that says what the helper reads, such as
`parse_time reads text, not a number`. That covers `null` (`None`), a
boolean, an object, an array, and a number given to `parse_time` or
`parse_percent`. Pass a field that a response may omit only when it is
present, as in `.uptime | values | parse_duration`, so that it is a
[missing value](#when-a-metric-cannot-be-extracted) instead of a failure.

Text that a helper cannot read fails like any other failing expression of
its transform:

- A jq or yq rule fails with the helper's error, under the rule's
  [`error_mode`](#when-a-metric-cannot-be-extracted). `try ... catch` and
  `?` catch it, as they catch any jq error.
- A Python script or pre-script gets a `ValueError` (a `TypeError` for input
  that is not text), which the script may catch and which otherwise fails it
  like any exception.

The message names the helper, quotes the text (cut to its first 64 bytes,
with its length, when it is longer) and says what to write:

```text
parse_bytes cannot read "12 KB/s" as a size: write a number, then optionally blanks and a unit: B, KiB to EiB, kB to EB, or K to E
parse_duration cannot read "P1M" as a duration: years and months have no fixed length in seconds
parse_time cannot read "2026-10-03 09:00:00" as a time: it names no zone or offset, so it is no single moment; add Z or an offset
parse_percent cannot read "55.12" as a percentage: write a number followed by %, such as 55.12%
parse_quantity cannot read "1K" as a quantity: write a number and a Kubernetes suffix or none: Ki to Ei, n, u, m, k, M to E, or an exponent such as e3
```

The helpers are gojq functions that the exporter adds, named so that none
hides a builtin of jq or gojq. They are available in every jq expression the
exporter evaluates, including those of [pagination](REQUESTS.md#pagination)
and [logins](AUTHENTICATION.md#logging-in-to-a-target).

### Programs written for jq

The engine is [gojq](https://github.com/itchyny/gojq), which lacks a few
names that jq has. The exporter adds them, so a program written and tested
with `jq` loads unchanged:

| Name | Here |
| --- | --- |
| `debug`, `debug(message)`, `stderr` | Hand their input on. A scrape writes nothing of it: the exporter has no terminal, and a target's answer is not for its log. A [debug probe](#debug-probes) and a run of the debug page show each value, as below. |
| `input_line_number`, `input_filename` | `0` and `null`: a program has one input, of no file. |
| `have_literal_numbers`, `have_decnum` | `true` and `false`: a whole number keeps every digit, and there are no decimal numbers. |
| `toarray` | Its input when that is an array, and otherwise an array of it. |
| `@base32`, `@base32d` | Encode text as base32, and decode it, padded or not, also in a string, as `@base32 "id=\(.id)"`. |

`input`, `inputs` and a module read with `import` or `include` are refused
when the configuration loads, because a program has exactly one input and no
files. `$ENV` and `env` are empty.

To see an intermediate value of an expression, put `debug` where the value
is and probe with `debug=true`, or run the collector on the debug page. Each
value is a line of the report's log, with its rule and its expression;
`debug(message)` shows the message instead.

```yaml
- name: queue_jobs
  items: .queues[] | debug
  expression: .jobs | debug("jobs of \(.)") | tonumber
```

```text
level=INFO msg="jq debug" rule="metric \"queue_jobs\"" expression=".queues[] | debug" value="{\"jobs\":\"7\",\"name\":\"mail\"}"
level=INFO msg="jq debug" rule="metric \"queue_jobs\"" expression=".jobs | debug(\"jobs of \\(.)\") | tonumber" value="\"jobs of 7\""
```

A value is written as JSON and cut after 1024 bytes, and a transform shows
its first hundred lines. A `debug` left in a rule costs a normal scrape
nothing.

### Histograms from a list of values

A response often lists things one by one: the requests of the last minute
with their durations, the files of a directory with their sizes, the jobs of
a queue with their ages. For such data a distribution is more useful than a
series per item, and cheaper to store. A rule of `type: histogram` with a
`histogram` block counts the values it reads into a histogram instead of
making each one a series, without a Python script:

```yaml
metrics:
  - name: request_duration_seconds
    description: Durations of the requests the target lists.
    type: histogram
    expression: .requests[].duration_ms
    scale: 0.001
    histogram:
      buckets: [0.005, 0.01, 0.05, 0.1, 0.5, 1, 5]
```

Nothing changes for a rule without the block: `type: histogram` without it is
still refused for every transform except `prometheus`. The block is accepted
by the `jq`, `yq`, `xpath`, `css`, `csv` and `regex` transforms. It is
refused on a `prometheus` rule, which passes on the target's histograms with
their own buckets, and on a `python` rule, whose script makes its series
itself.

**The values.** The values are the ones the rule would otherwise turn into
series, read in the same way:

- every value a `jq` or `yq` expression gives, or the one value of each item
  with `items`;
- the text of each node an XPath expression selects, or the one value it
  computes;
- each match of a regex;
- a column's cell in each row of a CSV;
- the text of each element a CSS selector matches within each item with
  `items` or, without `items`, of every element it matches. (A rule of
  another type refuses more than one such element, because nothing would
  distinguish their series.)

`value_map`, `time_format` and `scale` apply to each value before it is
counted, so the `scale: 0.001` above counts milliseconds in buckets of
seconds.

**The buckets.** `buckets` are the upper bounds of the histogram's buckets:
finite numbers in increasing order, each once, at most 100. The `+Inf`
bucket is added automatically. These are refused: `+Inf` written in the
list, a bound out of order or written twice, NaN, `-Inf`, an empty list, and
the block on a rule of any other type or of none.

- A value is counted in every bucket whose bound is greater than or equal to
  it, as Prometheus' `le` means (a value of exactly 0.1 is in `le="0.1"`),
  and in `+Inf`.
- The `_sum` is the sum of the values, after `scale`, and the `_count` is how
  many there were.
- An infinity is counted as client libraries count one: `-Inf` in every
  bucket, `+Inf` in the `+Inf` bucket alone.

The histogram is written like any other: a `_bucket` line for each bound and
for `+Inf`, then `_sum` and `_count`. If the example's target lists requests
of 3ms, 40ms and 2s, the result is:

```text
# HELP request_duration_seconds Durations of the requests the target lists.
# TYPE request_duration_seconds histogram
request_duration_seconds_bucket{le="0.005"} 1
request_duration_seconds_bucket{le="0.01"} 1
request_duration_seconds_bucket{le="0.05"} 2
request_duration_seconds_bucket{le="0.1"} 2
request_duration_seconds_bucket{le="0.5"} 2
request_duration_seconds_bucket{le="1"} 2
request_duration_seconds_bucket{le="5"} 3
request_duration_seconds_bucket{le="+Inf"} 3
request_duration_seconds_sum 2.043
request_duration_seconds_count 3
```

**Labels.** The labels apply to the histogram, not to each value. A static
label is on every histogram of the rule. A label read from the response
splits the values into one histogram for each distinct set of label values,
in the order in which each set first appears. `items` makes this natural: in
the example below, a CSV with one request per row gives a histogram of
durations per method:

```yaml
transform:
  type: csv
metrics:
  - name: request_duration_seconds
    type: histogram
    expression: duration_ms
    scale: 0.001
    labels:
      - name: method
        expression: method
    histogram:
      buckets: [0.01, 0.1, 1]
```

- A label with an empty value is not a label. Values whose labels differ
  only in such a label therefore belong to one histogram, as they are one
  series to Prometheus.
- The values are grouped by what the label expressions give. If a label's
  `value_map` or `truncate: true` makes two sets identical afterwards, the
  result is two histograms of one series, which fails the scrape as a
  duplicate series. Map such values in the expression instead.
- Two sets that differ are two histograms whatever bytes their values hold.
  A value that is not UTF-8 is grouped by its bytes as read, before the
  exporter repairs it.

**Values that cannot be counted** are handled by the rule's
[`error_mode`](#when-a-metric-cannot-be-extracted), like any rule's value.
That covers a value that is not a number; NaN, which belongs to no bucket and
would make the sum NaN permanently (`metric "request_duration_seconds":
value is NaN, which a histogram cannot count`); and a missing value, such as
a `null` among the durations, as `required` says. Under `log` and `ignore`
the histogram counts the other values, and under `fail` the scrape fails.

**An empty list** gives a histogram that counted nothing, with every bucket,
the `_sum` and the `_count` at 0. The series therefore exists whether or not
the target listed anything, and an empty list is not a missing value,
whatever `required` says. Empty lists are: a `jq` expression that gives no
value, `items` that select nothing, an XPath expression or a CSS selector
that selects nothing, a regex that matches nothing, and a CSV without rows.

- Only a rule whose labels are all static has such an empty histogram. A
  rule with a label read from the response has no labels to give a
  histogram of no values, so it produces none.
- A `jq` path into a key the response lacks, such as `.requests[]` on a
  document without `requests`, is not an empty list but an error of the
  expression, as for any rule.

**Limits.** Each histogram is one series for
[`limits.max_metrics`](#memory), however many values it counts, so a list of
a hundred thousand durations costs one series. A rule whose labels split its
values into more histograms than the limit allows fails the scrape, like a
rule with too many series. The number of values a response can hold is
bounded by its size (`max_response_bytes`).

**Reading the histogram.** The histogram describes the values of this one
response. It is built anew on every scrape and carries nothing over from the
previous one. Its buckets and its `_count` therefore go down as well as up
when the target's list changes, which never happens to a histogram kept by
Prometheus' client libraries. `rate()` and `increase()` over it are
meaningless unless the target's list itself only grows. Read it as it is,
for example with `histogram_quantile(0.95,
request_duration_seconds_bucket)` for the 95th percentile of the listed
durations, summed by `le` across targets where needed.

It is exported as a `histogram`: in the text format, in
[OpenMetrics](#openmetrics) (not as a `gaugehistogram`, which Prometheus
reads as gauges) and to [OTLP](OTLP.md), where it is a histogram data point
with the bounds and counts of each bucket. `--dry-run` checks the block as
startup does. A [debug probe](#debug-probes) counts each histogram as one
series of its metric and shows it among the metrics as the probe answers it.

### Derived series

A target often counts things that a dashboard wants as a ratio or a total.
memcached counts the gets that found their item and those that did not, not
its hit ratio. A queue lists its items by shard, where the depth of each
queue is wanted. A collector's `derived` list computes series from the
collector's own series after its transform has produced them. It works the
same way under every transform (`regex`, `csv`, `xpath`, `css`, `jq`, `yq`,
`python` and `prometheus`), so no Python script or recording rule is needed:

```yaml
- name: memcached
  request:
    type: tcp
    send: "stats\r\n"
    terminator: "END\r\n"
  transform:
    type: regex
  metrics:
    - name: memcached_get_hits_total
      type: counter
      expression: '(?m)^STAT get_hits (\d+)\r$'
    - name: memcached_get_misses_total
      type: counter
      expression: '(?m)^STAT get_misses (\d+)\r$'
  derived:
    - name: memcached_gets_total
      type: counter
      hidden: true
      sum: [memcached_get_hits_total, memcached_get_misses_total]
    - name: memcached_get_hit_ratio
      description: The share of gets that found their item
      ratio: [memcached_get_hits_total, memcached_gets_total]
```

```text
# HELP memcached_get_hit_ratio The share of gets that found their item
# TYPE memcached_get_hit_ratio gauge
memcached_get_hit_ratio 0.9
```

[examples/tcp/config.yaml](../examples/tcp/config.yaml) has this for a real
memcached. Each entry has a `name`, an optional `description` (the help
text) and a `type`: `gauge` (the default), `counter` or `untyped`. It sets
exactly one operation, with a list of the names of the families it reads:

| Operation | Computes |
| --- | --- |
| `sum: [a, b, ...]` | `a + b + ...` of two families or more, series by series |
| `difference: [a, b]` | `a - b`, series by series |
| `ratio: [a, b]` | `a / b`, series by series |
| `sum: [a]` with `by: [l, ...]` | the series of `a` added up into one for each set of values of the labels `l, ...`, the other labels dropped |
| `sum: [a]` with `by: []` | every series of `a` added up into one series without labels |

```yaml
derived:
  - name: queue_depth
    sum: [queue_items]
    by: [queue]
```

**Matching.** "Series by series" means what PromQL does when it matches two
vectors one to one without `on` or `ignoring`. A series of `a` is paired with
the series of `b` that has exactly the same labels, names and values (a
label with an empty value is not a label), and the result has those labels.
A series without a partner in every other family produces no derived series.
Neither does a label set that two series of one family share, because it has
no single partner. To compute over families whose labels differ, first add
up the one with more labels `by` the labels of the other, in a hidden entry,
and read that entry.

**Order and hidden entries.** The entries are computed in order, and an
entry may read an earlier one as it reads a rule. An entry with
`hidden: true` is computed for the entries after it and not exported, like
the sum of hits and misses above, which
`memcached_commands_total{command="get"}` already exports. A hidden entry
that no later entry reads is refused.

**No number, no series.** A result that is NaN or infinite produces no
series. A `ratio` over 0, such as the hit ratio of a server that has had no
get yet, is left out instead of being exported as NaN, which no query can
use. This is not a failure: the target answered every value it was asked
for.

**Where in the scrape.** The entries read the series as the rules produced
them:

- after each rule's `value_map`, `scale` and `time_format`, the label value
  maps and [truncation](#long-label-values);
- before `transform.labels`, `remove_labels` and `rename_labels`,
  [`metrics_prefix`](#prefixing-a-collectors-metrics) and
  [`name_escaping`](#utf-8-names), which then apply to the derived series as
  to every other.

Consequently, an operand is written as the rule's `name` is written, without
the prefix, and a label in `by` as the rule's label is named, before any
rename. A derived series gets the collector's static labels whatever its
operands' labels were, and has no timestamp, whatever its operands had.

Each exported derived series counts against
[`limits.max_metrics`](#memory). A scrape whose rules and derived entries
produce more fails, as one whose rules produce too many does. A hidden
entry's series are not counted. Derived series are cached, served to
[static targets](STATIC-TARGETS.md) and exported to [OTLP](OTLP.md) like any
other. A [debug probe](#debug-probes) lists each family it got as
`memcached_get_hit_ratio: 1 (derived: ratio of memcached_get_hits_total,
memcached_gets_total)`, and lists the entries that got none separately from
the rules.

**Checked at load.**

- An entry's name must not be the name of a rule of the collector or of
  another entry, nor a `_bucket`, `_sum` or `_count` of a histogram or
  summary rule. It is subject to the rules' name checks and to
  `limits.max_metric_name_length` as exported, and its `description` to
  `limits.max_help_length`.
- Each operand is the name of a rule of the collector or of an earlier
  entry, for a counter, a gauge or an untyped family. A histogram or a
  summary has no single value to compute with.
- `sum` reads two or more families, and `difference` and `ratio` read two.
  `by` is allowed only with a `sum` of one family, with each label listed
  once.

**Names known only at run time.** The series of a `python` script and of a
`prometheus` pass-through are named at run time. Under those two transforms:

- An operand that no rule names is accepted and read from the series of that
  name that each scrape produces. If a scrape shows a family of that name to
  be a histogram or a summary, the entry produces no series. That is a
  failure of the entry under `error_mode: log`:
  [logged](#when-a-metric-cannot-be-extracted) once per failing target,
  counted in `http_exporter_rule_failures_total` under the entry's name
  (which every entry has from 0), and shown by a debug probe.
- The target or the script may also export a family with an entry's own
  name, which no load check can detect. The entry's series would then be
  duplicates of the target's, which fail the whole scrape. The entry
  therefore produces none, and the scrape keeps the target's family as it
  came. That is a failure of the entry, logged, counted and shown like the
  histogram operand above, and a later entry that reads it reads no series.
- An operand of which the scrape has no series (one the target leaves out
  while it has nothing to count, or a name no target ever exports) gives the
  entry no series. That is not a failure, because the absence may be what
  the target means. A debug probe lists the entry among those that got none,
  with the operand that had none, as `requests_failed_ratio (operand
  requests_failed_total: no series)`, including the operands of a hidden
  entry it reads.

### Conditional metrics and labels

There is no `when` key. Each transform already expresses in its own language
which values become series, and a second, shared condition language would
duplicate that. Put the condition where the transform reads the value.

**jq and yq.** `select(...)` keeps the items that match, and
`if ... then ... else empty end` gives a value only when a condition holds.
`$status` and `$headers` are available for conditions on the answer itself.
A rule whose condition can leave it without a value needs
`required: false`. Otherwise the absence is a missing value, logged under
`error_mode`:

```yaml
- name: queue_depth
  items: '.queues[] | select(.state == "active")'
  expression: .depth
  labels:
    - name: queue
      expression: .name
- name: queue_depth_total
  expression: 'if $status == 200 then .total else empty end'
  required: false
```

**XPath.** A predicate selects the nodes: `//queue[@state='active']/depth`,
with labels read relative to each node, such as `../@name`.

**CSS.** `items` takes any selector, including `:has()`, so the rows kept
are the ones that contain what the condition looks for:

```yaml
- name: queue_depth
  items: 'tr:has(td.state.active)'
  expression: td.depth
  labels:
    - name: queue
      expression: td.name
```

**Regex.** The pattern is the condition: only text it matches becomes a
series. `(?m)^(\d+) (\w+) up$` reads the lines of services that are up, with
the first group as the value and the second as the `service` label
(`expression: "2"`). Where the label comes first in the line, name the
value's group: `(?m)^(?P<service>\w+) up (?P<value>\d+)$`, with
`expression: service`.

**CSV.** A rule reads every row. To keep only some rows, filter them in a
[pre-script](PYTHON.md), which leaves the rows it keeps in `data`:

```yaml
transform:
  type: csv
  pre_script: |
    data = [row for row in data if row["state"] == "active"]
```

When the filter can remove every row, give the rules `required: false`, as
for a jq condition. A response without a row is a missing value for every
required rule.

**Python.** The script calls `metric(...)` for what it wants exported, under
any condition.

**Labels.** A label whose expression gives no value, or `null`, is left off
the series (`if .shared then .owner else null end` in jq). So is a label
whose [`value_map`](#mapping-text-to-values-and-scaling-them) maps its value
to `""`. A `required` label turns its absence into a failure of the rule
instead.

**Series that come and go.** A conditional series exists only while its
condition holds. When the condition stops holding, Prometheus marks the
series stale: `rate()` and `increase()` have gaps across it, an alert on its
value stops firing instead of resolving, and `absent()` is the only way to
see that it is gone. When the absence itself means something, such as a
queue that is not active, prefer one series that always exists, with `1`
and `0`:

```yaml
- name: queue_active
  items: .queues[]
  expression: 'if .state == "active" then 1 else 0 end'
  labels:
    - name: queue
      expression: .name
```

`value_map` does the same for text in every transform: `{active: 1, "*": 0}`.
Keep conditions for series whose absence means nothing, such as the depth of
a queue that does not exist yet. A test checks the examples in this section,
so they stay correct.

### Request types

Every collector's `request` block starts with `type`, which is required and
determines how the collector reaches its data:

```yaml
request:
  type: http
  path: /api/status
```

There are seventeen types in the default build and the published image:

- `http` requests a URL.
- `localfile` reads a file from the exporter's own filesystem; see
  [Local files](LOCALFILE.md).
- `graphite` asks a Graphite render API for series; see
  [Graphite](GRAPHITE.md).
- `grpc` calls a unary or server streaming gRPC method; see
  [gRPC](GRPC.md).
- `sshfile` reads a file from another host over SFTP; see
  [Files over SSH](SSHFILE.md).
- `sshexec` runs a command on another host; see
  [Commands over SSH](SSHEXEC.md).
- `s3file` reads objects of S3; see [Objects in S3](S3FILE.md).
- `sql` runs statements on a database; see [Databases over SQL](SQL.md).
- `redis` reads a Redis or Valkey server; see
  [Redis and Valkey](REDIS.md).
- `tcp` speaks a protocol over TCP or UDP, or checks that a port accepts a
  connection; see [Text protocols over TCP](TCP.md).
- `ldap` searches an LDAP directory; see [LDAP directories](LDAP.md).
- `dns` resolves a name; see [DNS](DNS.md).
- `tlscert` reads the certificates of a TLS handshake; see
  [TLS certificates](TLSCERT.md).
- `ntp` asks an NTP server for the time; see [NTP](NTP.md).
- `icmp` pings a host; see [Ping over ICMP](ICMP.md).
- `snmp` reads an SNMP agent's objects and tables; see
  [SNMP agents](SNMP.md).
- `kubernetes` lists Kubernetes objects; see
  [Kubernetes objects](KUBERNETES.md).

A build that [asks for them](#choosing-request-types-at-build-time) has
twelve more:

- `smtp` holds a mail server's session without sending; see
  [SMTP](SMTP.md).
- `imap` counts the mail of an account's mailboxes; see [IMAP](IMAP.md).
- `pop3` counts the mail of a mailbox over POP3; see [POP3](POP3.md).
- `modbus` reads a device's registers; see [Modbus](MODBUS.md).
- `mqtt` subscribes to a broker's topics; see [MQTT](MQTT.md).
- `websocket` reads the messages of an endpoint; see
  [WebSocket](WEBSOCKET.md).
- `ftp` reads a file from an FTP server; see [Files over FTP](FTP.md).
- `radius` authenticates an account against a RADIUS server; see
  [RADIUS](RADIUS.md).
- `whois` reads a domain's registration; see [WHOIS](WHOIS.md).
- `sip` asks a SIP server whether it answers; see [SIP](SIP.md).
- `amqp` asks an AMQP broker how deep its queues are; see [AMQP](AMQP.md).
- `nats` times a NATS server and reads its JetStream; see [NATS](NATS.md).

An uptime check of HTTP and HTTPS sites, like the one blackbox_exporter's
`http` prober performs, is an `http` collector; see
[Uptime probes](REQUESTS.md#uptime-probes).

A collector without `type`, or with an unknown type, stops the exporter at
startup with a message that says what to add.

#### Which request type?

| You want | Type | Where it is |
| --- | --- | --- |
| A value from a JSON, XML, CSV, YAML or HTML API or page | `http` | [Target requests](REQUESTS.md) |
| Whether a site is up, its status, timings and certificate | `http` | [Uptime probes](REQUESTS.md#uptime-probes) |
| A metrics or status file on the exporter's own disk | `localfile` | [Local files](LOCALFILE.md) |
| A file on another host | `sshfile` | [Files over SSH](SSHFILE.md) |
| The output of a command on another host, such as `df` | `sshexec` | [Commands over SSH](SSHEXEC.md) |
| An object in S3, MinIO, Ceph, R2 or GCS | `s3file` | [Objects in S3](S3FILE.md) |
| A value in a database, such as a queue's backlog | `sql` | [Databases over SQL](SQL.md) |
| A Redis or Valkey server's INFO, or a queue's length | `redis` | [Redis and Valkey](REDIS.md) |
| A gRPC service's answer, or its health | `grpc` | [gRPC](GRPC.md) |
| Series from Graphite | `graphite` | [Graphite](GRAPHITE.md) |
| A line protocol such as memcached's `stats`, or an SMTP dialogue | `tcp` | [Text protocols over TCP](TCP.md) |
| Whether a port, or each of a host's ports, takes a connection | `tcp` | [Checking that a port is open](TCP.md#checking-that-a-port-is-open) |
| Entries of an LDAP directory, or its monitor counters | `ldap` | [LDAP directories](LDAP.md) |
| A name's records, their TTLs, a zone's serial or its signatures' expiry | `dns` | [DNS](DNS.md) |
| A TLS certificate's expiry and chain, of any TLS or STARTTLS server | `tlscert` | [TLS certificates](TLSCERT.md) |
| A clock's offset from an NTP server | `ntp` | [NTP](NTP.md) |
| Whether a host answers ping, and its round trip | `icmp` | [Ping over ICMP](ICMP.md) |
| A switch's, router's or UPS's counters over SNMP, as snmp_exporter reads them | `snmp` | [SNMP agents](SNMP.md) |
| Kubernetes objects, or each pod's ephemeral storage | `kubernetes` | [Kubernetes objects](KUBERNETES.md) |

<!-- request-types: all "For `%s`" -->
Each type accepts its own keys. For `http`, `type` is the only required key.
`path` can be omitted when the target URL already contains the whole path,
and `method` defaults to `GET`:

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `http`. |
| `method` | `GET` | GET, POST, PUT, PATCH, DELETE or HEAD. |
| `path` | — | Joined onto the target; may use [path parameters](REQUESTS.md#path-parameters) and [clock placeholders](REQUESTS.md#dates-and-times). A `%XX` escape in it is sent as written ([Target requests](REQUESTS.md#the-request-url)). |
| `query` | — | Query parameters added to the request, after the target's own query, which is sent as written; values may use [placeholders](REQUESTS.md#in-the-body-headers-and-query). A name is sent escaped, and may not be empty or hold a control character. |
| `headers` | — | Sent to the target; values may use [placeholders](REQUESTS.md#in-the-body-headers-and-query). Not `Accept-Encoding`, which is the exporter's own ([Target requests](REQUESTS.md#compression-and-the-response-size)). |
| `body` | — | Request body; may use [placeholders](REQUESTS.md#in-the-body-headers-and-query), encoded with `\|json`, `\|number`, `\|form` or `\|xml`. |
| `basic_auth`, `basic_auth_file` | — | Use one; see [Authentication](AUTHENTICATION.md). |
| `bearer_token`, `bearer_token_file` | — | Use one; not together with basic auth. |
| `login` | — | Log in before the request and send the token the login gives: `type: json` posts a JSON object to `url` and reads the token from the answer's body, a header or a cookie; `type: oauth2` asks a token endpoint with the client credentials grant, or, with `grant_type: refresh_token`, the refresh token of `refresh_token_file`, for an API on a user's account such as Gmail's; `type: jwt` signs a JWT with a service account's key and exchanges it for a token, for Google's APIs and GitHub Apps; `type: steps` makes a list of requests, each reading values from its answer for the steps after it, one of them the token, with `refresh_steps` and `keep_file` for a sign-in made seldom. Secrets are read from files at each login; the token is kept until shortly before it expires, and a `401`, or a status of `relogin_on`, makes one new login. See [Logging in to a target](AUTHENTICATION.md#logging-in-to-a-target). |
| `signature` | — | `http` alone: sign each request with an HMAC of a shared secret, sent in a header or a query parameter; see [Signing requests](AUTHENTICATION.md#signing-requests). |
| `aws_sigv4` | — | `http` alone: sign each request with AWS Signature Version 4, to read an AWS API; see [Signing requests for AWS](AUTHENTICATION.md#signing-requests-for-aws). |
| `forward_authorization`, `forward_headers` | off | See [Authentication](AUTHENTICATION.md). |
| `tls` | verify | See [Target requests](REQUESTS.md#tls). |
| `retry` | none | `attempts`, `backoff`, `non_idempotent`, `statuses`, the statuses retried: `[408, 425, 429, "5xx"]` when left out, and a list replaces that set, and `retry_after`, off by default: a retried answer's `Retry-After` is waited in place of `backoff`. See [Target requests](REQUESTS.md#retries) and [Waiting as Retry-After asks](REQUESTS.md#waiting-as-retry-after-asks). |
| `max_response_bytes` | 10 MiB | Response size cap, on the decompressed answer; one whose `Content-Length` is over it is refused before it is read ([Target requests](REQUESTS.md#compression-and-the-response-size)). With `limits.max_response_bytes` set too, the smaller wins; either alone may be above 10 MiB. It bounds the body; the response's headers are bounded apart, at 1 MiB, and a `grpc` answer's headers, and its trailers, each at the same ([gRPC](GRPC.md#errors-and-retries)). At least 1 KiB when set ([Target requests](REQUESTS.md#compression-and-the-response-size)). |
| `follow_redirects`, `enable_http2` | off | See [Target requests](REQUESTS.md#redirects-and-http2). |
| `new_connection` | off | Send every request, each retry and redirect too, on a new connection closed after its answer, rather than on one kept from an earlier request, so every probe looks the name up, connects and verifies the certificate the target serves now; for uptime checks. See [Connections](REQUESTS.md#connections). |
| `ip_family` | both | The address family its requests connect over: `ip4` or `ip6` alone, failing when the target has no address of it, or `prefer_ip4` or `prefer_ip6` first and the other after; left out, both, as Go connects. Not with `unix_socket` or a `proxy_url` other than `direct`. See [Address families](REQUESTS.md#address-families). |
| `redirect_trusted_hosts` | none | Hosts, globs and addresses, besides the request's own origin, that a followed redirect may carry the collector's headers, credentials and body to, and present its TLS client certificate to; see [What a followed redirect carries](REQUESTS.md#what-a-followed-redirect-carries). |
| `allowed_schemes` | `http`, `https` | Schemes a target may use: `http`, `https` or both. Any other entry stops the exporter at startup. |
| `accept_status` | every 2xx | Statuses whose answers are decoded, such as `["2xx", 503]`, or `[any]` alone for every status from 100 to 599; a status written as a number is the one YAML reads, so `503.0` and `0x1F7` are 503. See [Accepting other statuses](REQUESTS.md#accepting-other-statuses). |
| `allowed_targets`, `denied_targets` | none | Hosts, globs, addresses and networks its requests may and may not reach; see [Restricting targets](REQUESTS.md#restricting-targets). |
| `pagination` | none | Fetch the further pages of a paginated answer — by the `Link` header, a next URL or cursor read from the page, or a page number or offset — and merge their items into one JSON document; at most `max_pages`, 10 by default. The decoder is `json`. See [Pagination](REQUESTS.md#pagination). |
| `rate_limit` | none | The most requests a second sent to one origin of its targets, `requests_per_second`, greater than 0, and how many at once after a pause, `burst`, 1 by default; every attempt, page, redirect and login request counts, and a request whose turn would come past its deadline fails at once. Each collector has its own limit for an origin; the top-level [`rate_limits`](REQUESTS.md#rate-limits-per-host) limit a host for every collector together, in addition. See [Rate limiting](REQUESTS.md#rate-limiting). |
| `proxy_url` | the environment's | The proxy every request of the collector, its login's included, goes through instead of the one `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY` name: `http://host:port`, `https://host:port` or `socks5://host:port`, without credentials; `direct` for none. See [A collector's own proxy](REQUESTS.md#a-collectors-own-proxy). |
| `proxy_basic_auth_file` | none | With `proxy_url`: the files the `username` and the `password` sent to the proxy are read from, at every request. |
| `fastcgi` | none | Send the requests to a FastCGI application, such as PHP-FPM's status page, in place of an HTTP server, `{}` or with `document_root` and `params`. Not with `proxy_url`, `login` or `tls`. See [FastCGI](REQUESTS.md#fastcgi). |
| `unix_socket` | none | The absolute path of a Unix domain socket every request of the collector, its login's included, is sent over in place of a TCP connection to the URL's host, as `curl --unix-socket` does, such as `/var/run/docker.sock`; the URL's host is still the `Host` header and its path and query the request's. Not with `proxy_url`; no probe parameter, static target or `defaults` sets it. See [Over a Unix socket](REQUESTS.md#over-a-unix-socket). |

For `localfile`, `root` is required. `path`, `max_age` and
`max_response_bytes` are optional, as are `files`, `max_files` and
`max_total_bytes` to
[read a whole directory](LOCALFILE.md#reading-a-directory). Its table is in
[Local files](LOCALFILE.md#a-collector).

For `graphite`, `targets` is required: the Graphite expressions to render.
`from` and `until` set the window, `-15min` to `now` by default, and `path`
defaults to `/render`. Every `http` key about the connection applies:
`query`, `headers`, credentials, `tls`, `retry`, `max_response_bytes`,
`follow_redirects`, `redirect_trusted_hosts`, `enable_http2`,
`new_connection`, `ip_family`, `allowed_schemes`, `accept_status`,
`allowed_targets`, `denied_targets`, `rate_limit`, `proxy_url` and
`proxy_basic_auth_file`. `method` and `body` do not apply. Its table is in
[Graphite](GRAPHITE.md#a-collector).

For `grpc`, `rpc` is required: the method as `package.Service/Method`.
`descriptors`, the source of its message types (`reflection`, `protoset` or
`proto`), is also required, except for the built-in health service.
`message` is the request as JSON, `{}` by default, and `metadata` is its
metadata. The credential keys, `tls`, `retry` with its `codes`,
`max_response_bytes`, the forwarding keys, `allowed_targets` and
`denied_targets` apply as for `http`; the other `http` keys do not. Its
table is in [gRPC](GRPC.md#a-collector).

For `sshfile`, `root`, `username` and `known_hosts_file` are required,
together with `private_key_file` or `password_file`, or both. `path` (or
`files`, `max_files` and `max_total_bytes`), `max_age` and
`max_response_bytes` work as for `localfile`. `passphrase_file`,
`keepalive_interval`, `keepalive_count_max`, `idle_timeout`,
`max_open_connections`, `allowed_targets` and `denied_targets` apply too.
Its table is in [Files over SSH](SSHFILE.md#a-collector).

For `sql`, `driver`, `statement` and `username` are required, and
`service_name` for the `oracle` driver. `args`, `database`, `password_file`,
`private_key_file` (`snowflake` only), `tls`, `options`, `max_rows`,
`max_response_bytes`, `connections`, `allowed_targets` and `denied_targets`
apply too. No `decoder` or `response` setting applies: its rows are read by
a `csv`, `jq`, `yq` or `python` transform. Its table is in
[Databases over SQL](SQL.md#a-collector).

For `dns`, no key is required. `record_type` is `A` by default, or `AAAA`,
`CNAME`, `MX`, `NS`, `TXT`, `SRV` or `PTR`. `server` is the resolver to ask,
the system's by default, and `transport` is `udp` or `tcp`.
`allowed_targets` and `denied_targets` apply too, matched against the name.
The decoder is `json`. Its table is in [DNS](DNS.md#a-collector).

For `tlscert`, no key is required. `tls` sets the roots, a client
certificate and the name to verify. `starttls` (`smtp`, `imap`, `pop3` or
`ftp`) begins TLS after the greeting as that protocol requires, as for
`tcp`. `allowed_targets` and `denied_targets` apply as for `http`. The
decoder is `json`. Its table is in
[TLS certificates](TLSCERT.md#a-collector).

For `ntp`, no key is required, and the type has no keys of its own.
`allowed_targets` and `denied_targets` apply as for `http`, checked on the
address the request is sent to. The decoder is `json`. Its table is in
[NTP](NTP.md#a-collector).

For `kubernetes`, `resource` is required. These keys apply too: `group` and
`version`; `namespaces` or `all_namespaces`; `label_selector` and
`field_selector`; `max_objects`, `max_response_bytes` and `metadata_only`;
`kubelet`, with `kubelet_via_api_server` or `kubelet_ca_file`, for the
statistics of the nodes' kubelets; and `api_server`, with `tls` and
`bearer_token_file`, for a cluster other than the exporter's own.
`allowed_targets` and `denied_targets` do not apply, because there is no
target. The decoder is `json`. Its table is in
[Kubernetes objects](KUBERNETES.md#a-collector).

For `s3file`, `root` is required. `path` (or `files`, `max_files` and
`max_total_bytes`), `max_age` and `max_response_bytes` work as for
`localfile`. `region`, `endpoint`, `path_style`, `access_key_id_file`,
`secret_access_key_file`, `session_token_file`, `allowed_targets` and
`denied_targets` apply too. Its table is in
[Objects in S3](S3FILE.md#a-collector).

For `redis`, no key is required. `info_sections` names the sections of INFO
to read (the default ones when omitted), and `commands` names the read-only
commands whose replies are read in addition. `username` and `password_file`,
`db`, `tls`, `max_response_bytes`, `allowed_targets` and `denied_targets`
apply too. The decoder is `json`. Its table is in
[Redis and Valkey](REDIS.md#a-collector).

For `tcp`, no key is required.

- `send` is the text written once the connection is made, and `terminator`
  is what the answer ends with. Without a terminator, the answer ends when
  the server closes the connection.
- `greeting` is the pattern that a server which speaks first must greet
  with. `starttls` (`smtp`, `imap`, `pop3` or `ftp`) begins TLS after the
  greeting as that protocol requires. `document` returns a JSON document of
  the exchange.
- `send_hex` writes binary bytes in place of `send`, `answer_bytes` ends an
  answer of a known length, `udp` sends one datagram and reads one back, and
  `unix_socket` makes the exchange over a Unix socket.
- `tls`, `max_response_bytes`, `allowed_targets` and `denied_targets` apply
  too.

The answer is a body, read by any decoder, including `binary`. Its table is
in [Text protocols over TCP](TCP.md#a-collector).

For `ldap`, `searches` is required. Each search has its `name`, `base`,
`scope`, `filter`, `attributes`, `count_only` and `max_entries`. `bind_dn`
and `password_file`, `start_tls`, `tls`, `max_response_bytes`,
`allowed_targets` and `denied_targets` apply too. The decoder is `json`. Its
table is in [LDAP directories](LDAP.md#a-collector).

For `sshexec`, `command` is required: the program and its arguments as a
list. `username`, `known_hosts_file` and a private key or a password are
required as for `sshfile`. `success_exit_codes` names the exit statuses that
count as success, `[0]` when omitted. `passphrase_file`, the connection's
keepalive, idle and limit keys, `max_response_bytes`, `allowed_targets` and
`denied_targets` apply too. The command's output is a body, read by any
decoder. Its table is in [Commands over SSH](SSHEXEC.md#a-collector).

For `icmp`, no key is required. `count` is how many echoes a scrape sends,
`interval` the time between them, `reply_timeout` how long to wait for the
last reply, and `payload_bytes` the data each echo carries. `ip_family`,
`allowed_targets` and `denied_targets` apply too. The decoder is `json`. Its
table is in [Ping over ICMP](ICMP.md#a-collector).

For `snmp`, at least one of `get` (the objects read one by one) and `walk`
(the tables read whole) is required. Each object is given by a name of the
collector's choosing and its numeric identifier. `snmp_version` is 1, 2 (for
v2c, the default) or 3. v1 and v2c require `community_file`. v3 requires
`username` and `security_level`, with `auth_protocol`, `auth_password_file`,
`priv_protocol` and `priv_password_file` as its level demands.
`request_timeout`, `retries`, `max_repetitions`, `walk_limit`, `hex`,
`context_name`, `max_response_bytes`, `allowed_targets` and `denied_targets`
apply too. The decoder is `json`. Its table is in
[SNMP agents](SNMP.md#a-collector).

For `smtp`, an opt-in type, no key is required. `starttls_mode` controls
whether TLS is started. `username` with `password_file` or `xoauth2` logs
in, using the mechanism that `auth` names. `mail_from` and `rcpt_to` ask the
server about a sender and recipients, with nothing sent. `hello`, `tls`,
`max_response_bytes`, `allowed_targets` and `denied_targets` apply too. The
decoder is `json`. Its table is in [SMTP](SMTP.md#a-collector).

For `modbus`, an opt-in type, `read` is required: the registers and bits to
read, each with a `name`, its `table`, `address`, `type`, `count` and
`order`. `unit` is the unit id to ask, and `allowed_targets` and
`denied_targets` apply too. The decoder is `json`. Its table is in
[Modbus](MODBUS.md#a-collector).

For `mqtt`, an opt-in type, `topics` is required: the topic filters to
subscribe to, at `qos`. `window` and `max_messages` bound what a scrape
reads. `persistent_session` with `client_id` makes the broker keep the
session between scrapes, and `keep_connection` makes the exporter keep the
connection, for `idle_timeout`. `keep_alive`, `username`, `password_file`,
`tls`, `max_response_bytes`, `allowed_targets` and `denied_targets` apply
too. The decoder is `json`. Its table is in [MQTT](MQTT.md#a-collector).

For `websocket`, an opt-in type, no key is required. `send_text` is a
message sent after the handshake, `read_messages` is how many messages are
read, and `read_window` is how long to wait for them. `subprotocols`,
`headers`, `bearer_token_file`, `tls`, `max_response_bytes`,
`allowed_targets` and `denied_targets` apply too. One message is a body,
read by any decoder, and several messages are a JSON list. Its table is in
[WebSocket](WEBSOCKET.md#a-collector).

For `ftp`, an opt-in type, `root` is required: the directory the collector
may read under. `path` names the file under it. `max_age`, `username`,
`password_file`, `tls`, `max_response_bytes`, `allowed_targets` and
`denied_targets` apply too. The file is read by the decoder that its
extension selects. Its table is in [Files over FTP](FTP.md#a-collector).

For `imap`, an opt-in type, no key is required. `username` with
`password_file` or `xoauth2` logs in, using the mechanism that `auth` names.
`mailboxes` names the mailboxes that `STATUS` asks about, by name or by
pattern, `INBOX` when omitted. `quota` asks for the account's quota, and
`counts` counts the messages of a mailbox that match a search, with the
mailbox opened read-only. `allow_plaintext`, `tls`, `max_response_bytes`,
`allowed_targets` and `denied_targets` apply too. The decoder is `json`. Its
table is in [IMAP](IMAP.md#a-collector).

For `pop3`, an opt-in type, no key is required. `username` with
`password_file` or `xoauth2` logs in, using the mechanism that `auth` names:
`user`, `apop`, `plain`, `login`, `cram-md5`, `xoauth2` or `oauthbearer`.
The number of messages in the mailbox and their size are requested with
`STAT`, with nothing fetched, and `list` asks for the size of the largest
message. `allow_plaintext`, `tls`, `max_response_bytes`, `allowed_targets`
and `denied_targets` apply too. The decoder is `json`. Its table is in
[POP3](POP3.md#a-collector).

For `radius`, an opt-in type, `secret_file` is required: the secret the
server shares with the exporter. `username` with `password_file` is the
account the server is asked to authenticate, using `mode` `pap` or `chap`.
`mode: status-server` asks whether the server is alive and takes no account.
`nas_identifier` names the sender, `resends` and `resend_interval` send the
request again while no reply arrives, and `require_message_authenticator`
fails a reply without one. `allowed_targets` and `denied_targets` apply too.
The decoder is `json`. Its table is in [RADIUS](RADIUS.md#a-collector).

For `whois`, an opt-in type, no key is required. The registry's WHOIS server
is asked about the domain that is the target. The server is the one IANA
names for the top-level domain, or the one `whois_server` names, and
`query_prefix` is sent before the domain. `max_response_bytes`,
`allowed_targets` and `denied_targets` apply too. The decoder is `json`. Its
table is in [WHOIS](WHOIS.md#a-collector).

For `sip`, an opt-in type, no key is required. The SIP server that is the
target is sent an OPTIONS request to see whether it answers, over
`sip_transport` (`udp`, `tcp` or `tls`, the last verified as `tls` says).
`request_uri` is the URI asked, `from_uri` identifies the sender, and
`user_agent` is the request's User-Agent. `username` and `password_file`
are the account used to answer a Digest challenge. `max_response_bytes`,
`allowed_targets` and `denied_targets` apply too. The decoder is `json`. Its
table is in [SIP](SIP.md#a-collector).

For `amqp`, an opt-in type, no key is required. The exporter logs in to the
AMQP 0-9-1 broker that is the target with `username` and `password_file`
(`guest` when omitted), opens its `vhost` (`/` when omitted), and asks each
queue in `queues` for its ready messages and its consumers. `tls` is for a
target written `amqps://`, and `allowed_targets` and `denied_targets` apply
too. The decoder is `json`. Its table is in [AMQP](AMQP.md#a-collector).

For `nats`, an opt-in type, no key is required. The exporter logs in to the
NATS server that is the target with `username` and `password_file`, or with
the token in `bearer_token_file`, and times a PING to it. `jetstream: true`
asks for the account's JetStream state, and `streams` and `consumers` for
each one named. `tls`, `max_response_bytes`, `allowed_targets` and
`denied_targets` apply too. The decoder is `json`. Its table is in
[NATS](NATS.md#a-collector).
<!-- /request-types -->

When a request block sets several keys that it may not set (a key of
another type, a key that its type does not take, a key of `retry` for other
types, an unknown key), the error names the first one in the file: the one
on the earlier line, or further left on the same line. Once that is fixed,
it names the next. The same order applies, on every load, to a check that
refuses two keys for the same reason, for example:

- an ssh collector's negative `keepalive_interval` and `idle_timeout`, or
  its credential files written blank or with a placeholder;
- a kubernetes collector's blank selectors;
- a json login's blank `token` and `expires_in`;
- a login's keys of another kind.

A key the type does not take is refused for that reason, whatever its value.
`starttls: true` in an `ldap` collector is not told that `true` is not one
of `tcp`'s protocols; it is told that the key does not apply, and the error
names `ldap`'s own `start_tls: true`. `start_tls` in a `tcp` or a `tlscert`
collector names `starttls` and its protocols.

<!-- request-types: all -->
A key that belongs to a different type is an error instead of being ignored.
The same applies to `/probe` parameters: a parameter that only another type
accepts gets a `400`. The types other than `http` accept the following.

`localfile` accepts only `path`, `timeout` and `param_<name>`, and its
`target` is optional. When it reads a directory, it accepts `timeout` and
only the `param_<name>` that its
[label values' placeholders](REQUESTS.md#in-label-values) name, never `path`
([Reading a directory](LOCALFILE.md#reading-a-directory)).

`graphite` accepts what `http` accepts except `method` and `body`, and also
`from` and `until`, which no other type accepts.

`grpc` accepts `timeout`, `insecure_skip_verify`, `retry_attempts`,
`retry_backoff`, `header_<name>`, `param_<name>` and `message`, which no
other type accepts.

`sshfile` accepts `path`, `timeout` and `param_<name>`, as `localfile` does,
and requires a `target`, the host.

`s3file` accepts `path`, `timeout` and `param_<name>` too, and requires a
`target`, the bucket.

`sql` accepts `timeout` and `param_<name>`, and requires a `target`, the
database server.

`dns` and `tlscert` accept `timeout` and `param_<name>`, and require a
`target`: the name to resolve, and the server as `host:port`.

`ntp` accepts `timeout` and `param_<name>`, and requires a `target`, the
server as `host:port`, port 123 when omitted.

`kubernetes` accepts `timeout` and `param_<name>`, and takes no `target`. A
probe with one gets a `400`, because the collector lists what its
configuration names.

`redis` accepts `timeout` and `param_<name>`, and requires a `target`, the
server, as `host:port` or a `redis://` or `rediss://` URL.

`ldap` accepts `timeout` and `param_<name>`, and requires a `target`, the
server, as `host:port` or an `ldap://` or `ldaps://` URL.

`tcp` accepts `timeout` and `param_<name>`, and requires a `target`, the
service, as `host:port` with its port, or the host alone for a collector of
[`ports`](TCP.md#several-ports-of-a-host).

`sshexec` accepts `timeout` and `param_<name>`, and requires a `target`, the
host, as `sshfile` does.

`icmp` accepts `timeout` and `param_<name>`, and requires a `target`, the
host to ping, with no port.

`snmp` accepts `timeout` and `param_<name>`, and requires a `target`, the
agent, as `host` or `host:port`, port 161 when omitted.

`smtp`, an opt-in type, accepts `timeout` and `param_<name>`, and
requires a `target`, the mail server, `host`, `host:port`, `smtp://` or
`smtps://`.

`modbus`, an opt-in type, accepts `timeout` and `param_<name>`, and
requires a `target`, the device, `host`, `host:port` or
`modbus://host:port/<unit>`.

`mqtt`, an opt-in type, accepts `timeout` and `param_<name>`, and
requires a `target`, the broker, `host`, `host:port`, `mqtt://` or
`mqtts://`.

`websocket`, an opt-in type, accepts `timeout` and `param_<name>`, and
requires a `target`, the endpoint's `ws://` or `wss://` URL.

`ftp`, an opt-in type, accepts `path`, `timeout` and `param_<name>`, and
requires a `target`, the server, `host`, `host:port`, `ftp://`, `ftpes://`
or `ftps://`.

`imap`, an opt-in type, accepts `timeout` and `param_<name>`, and
requires a `target`, the mail server, `host`, `host:port`, `imap://` or
`imaps://`.

`pop3`, an opt-in type, accepts `timeout` and `param_<name>`, and
requires a `target`, the mail server, `host`, `host:port`, `pop3://` or
`pop3s://`.

`radius`, an opt-in type, accepts `timeout` and `param_<name>`, and
requires a `target`, the RADIUS server, `host` or `host:port`.

`whois`, an opt-in type, accepts `timeout` and `param_<name>`, and
requires a `target`, the domain.

`sip`, an opt-in type, accepts `timeout` and `param_<name>`, and
requires a `target`, the SIP server, `host` or `host:port`.

`amqp`, an opt-in type, accepts `timeout` and `param_<name>`, and
requires a `target`, the AMQP broker, `host`, `host:port` or `amqps://host`.

`nats`, an opt-in type, accepts `timeout` and `param_<name>`, and
requires a `target`, the NATS server, `host`, `host:port` or `tls://host`.
<!-- /request-types -->

`target`, `collector` and each of these parameters may be given once.

- A second value gets a `400` that names the parameter, instead of one of
  the two being used: `timeout=5s&timeout=1h` does not say which was meant.
  `header_<name>` is the exception, because every value of it is forwarded.
- A value may be at most 8 KiB (8192 bytes). A longer one gets a `400` that
  names the parameter and the limit. What a probe names is kept after the
  probe is answered, in the
  [log of repeated failures](LOGGING.md#repeated-failures) and the
  [verbose self-metrics](SELF-METRICS.md#verbose-per-request-self-metrics),
  and this limit is what bounds it there.
- A `header_<name>` value for a header the collector forwards must not
  contain a control character other than tab (including CR and LF), which Go
  refuses to send. It gets a `400` that names the parameter before the
  target is contacted, instead of a `502` that is retried and logged as the
  target's failure.
- A parameter that no request type knows is not read at all, whatever its
  values.
- A whole request, with its request line and headers, may be 64 KiB. The
  exporter answers a longer one with `431`.

#### Choosing request types at build time

By default, a build of the exporter carries every request type, and so does
the published image. A build can instead carry only the types it needs,
which keeps the code of the other types, and the libraries only they use,
out of the binary:

```sh
make build REQUEST_TYPES=http
docker build --build-arg REQUEST_TYPES=http -t exporter:http .
go build -tags "$(sh tools/request-type-tags.sh http)" .
```

`make build` writes the binary to `bin/prometheus-universal-exporter`. Plain
`go build .` writes it to the current directory.

Three features that are not request types come with one, because they share
its libraries:

- the [`protobuf` decoder](#protobuf-responses) is in the builds that have
  `grpc`;
- the `aws` [secret store](AUTHENTICATION.md#secret-stores) and an `http`
  collector's [`aws_sigv4`](AUTHENTICATION.md#signing-requests-for-aws) are
  in those that have `s3file`.

A build without the type refuses a configuration that uses them, with a
message that says which type to build with.

`REQUEST_TYPES` is a comma-separated list of type names. Empty, the default,
means every type. A name that is not a request type fails the build, and so
does a selection that names none. The script turns the list into Go build
tags, `select_request_types` plus `request_type_<name>` per type, so by hand
the equivalent is `go build -tags select_request_types,request_type_http`.

A collector whose type the build left out stops the exporter at startup,
with a message that says the type exists but this build does not include
it, and which types it does include. The startup log line and the
[dry run](#dry-run) report list the types the binary carries, so a
configuration can be checked against the build that will run it. Beyond that
message, the omitted types do not exist for such a build:

- the message for a collector without `type` suggests a type the build has;
- the schema that `--config.schema` prints lists only its types;
- a `/probe` parameter that only an omitted type accepts is one that no
  request type knows, so it is ignored instead of answered with `400`.

Each type built on its own is a build that the test suite runs against, in
CI and with `make test-request-types`
([Development](DEVELOPMENT.md#tests-of-a-build-with-only-some-request-types)).
An http-only build omits every other type, and a build with
`REQUEST_TYPES=localfile` reads files and makes no HTTP requests to targets
at all.

What each type links:

- `grpc`, `sshfile` and `sql` have libraries of their own, which a build
  without them does not link: gRPC and protobuf (about 6 MB of a stripped
  binary), the SSH and SFTP clients (about 1 MB), and the database drivers
  (about 6 MB for the three that are built in by default).
- `dns`, `ntp` and `kubernetes` need nothing outside the standard library,
  and `tlscert` only `golang.org/x/crypto/ocsp`, from a module the default
  build already links.
- `s3file` links the AWS SDK (about 6.7 MB). `sshexec` links the SSH client,
  without the SFTP client that `sshfile` links.
- `redis`, `tcp`, `ldap`, `icmp` and `snmp` need nothing outside the
  standard library either.
- The `sql` type's `oracle`, `clickhouse`, `snowflake` and `vertica` drivers
  are built in only when `SQL_DRIVERS` names them, in addition to
  `REQUEST_TYPES`: `make build SQL_DRIVERS=oracle`, or the image's
  `SQL_DRIVERS` build argument
  ([Building with more drivers](SQL.md#building-with-more-drivers)).

A type can also be opt-in: no build carries it unless asked, including the
default build and the published image. A type is made opt-in with `make
new-request-type OPT_IN=1` ([Adding a request type](DEVELOPMENT.md#adding-a-request-type)),
for a type that few deployments want in the binary. The opt-in types are
[`smtp`](SMTP.md), [`modbus`](MODBUS.md), [`mqtt`](MQTT.md),
[`websocket`](WEBSOCKET.md), [`ftp`](FTP.md), [`imap`](IMAP.md),
[`pop3`](POP3.md), [`radius`](RADIUS.md), [`whois`](WHOIS.md), [`sip`](SIP.md), [`amqp`](AMQP.md) and [`nats`](NATS.md), each written with the standard library alone: about
0.1 MB of a stripped binary each, and about 0.8 MB for the twelve. `OPT_IN_REQUEST_TYPES`, the Makefile's variable and the
Dockerfile's build argument, adds such types, comma-separated, to the types
a build carries, whatever `REQUEST_TYPES` says. The tag alone,
`request_type_<name>`, adds one to a default build. A build without an
opt-in type refuses a collector of that type, with a message that says the
type is opt-in and how to build with it.

```sh
make build OPT_IN_REQUEST_TYPES=mqtt,modbus
docker build --build-arg OPT_IN_REQUEST_TYPES=mqtt,modbus -t exporter:plant .
```

### When a metric cannot be extracted

Each metric sets what happens when its value cannot be produced, for example
when the expression matches nothing, the value is not a number, or a label
expression fails:

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

**`ignore` and `log`** keep the scrape going, so a response carries whatever
could be extracted. That suits a metric that is useful but not essential:
one missing value does not cost you the others. When nothing at all can be
extracted, the probe still succeeds, with an empty body.

How `log` logs:

- It writes one warning per failing rule, however many series failed. A rule
  over a thousand-row table that misses its value on every row logs its
  first error with `"failures":1000`, not a thousand lines.
- A rule that fails the same way on every scrape of a target is logged once
  and then only as a [repeat](LOGGING.md#repeated-failures), and its
  recovery is logged when it works again. "The same way" ignores the
  position in the response: the row, node or item that its first error
  names, and a size that the error measured, may differ on every scrape.
- Several rules with one metric name, such as one for each column or path
  its series come from, are each logged, remembered and recovered
  separately. Their lines identify the rule by its `expression` and its
  `items`, beside the `metric` ([Logging](LOGGING.md)).
- Rules that are alike in name, expression and items, and that only their
  labels distinguish, are one rule to the log, which logs if either of them
  has `log`. (Rules alike in labels too are
  [the same rule](#two-rules-that-are-the-same-rule), which does not load.)

In both modes, the series that a rule had to drop are counted in
`http_exporter_rule_failures_total{collector, metric}`, by the rule's metric
name and so for the rules of one name together. A rule that keeps failing
can therefore be graphed and alerted on.

**`fail`** is for a metric without which the scrape is meaningless. A single
failing rule with `fail` fails the whole probe, even when every other metric
was extracted correctly, because the purpose of the mode is that a response
is either complete or an error, never silently partial. The probe answers
`502 Bad Gateway` with a JSON body that says which collector, which rule and
why:

```json
{"status":"error","stage":"metric","collector":"exchange_rates","metric":"exchange_rate","target":"https://api.frankfurter.dev/v1/latest","error":"metric \"exchange_rate\" value is missing"}
```

Prometheus only looks at the status, which marks the scrape as down (`up`
becomes 0). The body is for whoever runs the probe by hand, and credentials
in the target URL are redacted from it. A failed probe is never cached, so
the next scrape goes back to the target.

Modes are set per metric, so a collector can mix them. A `fail` rule that
succeeds does not fail the probe because a `log` rule beside it failed.

`fail` takes precedence over the collector's
`error_handling.on_transform_error`. That policy governs the transform as a
whole, such as a pre-script that raises or a response the transform cannot
read. `error_mode: fail` is a statement about one metric, so a lenient
`on_transform_error: ignore` does not turn it back into a partial success.

An optional metric (`required: false`, or a collector with
`error_handling.allow_missing_keys: true`) is not failing when its value is
absent. No mode applies to it, including `fail`: it is simply left out.

**What "absent" means.** A value is absent in the same way in every
transform:

- nothing matched: a jq expression or `items` that gave nothing, a regex
  that matched no text, a selector that matched no node, a `prometheus` rule
  that matched no metric of the response, or a `csv` rule over a response
  without a row;
- a null;
- text that is empty or only whitespace: an empty CSV cell, an empty JSON
  string, an empty XML element or HTML cell, or a regex group that captured
  nothing.

Text that is present but is not a number, such as `"up"`, is not absent. It
is a failure to read the value, which `required: false` does not excuse.

Where a rule or a label is not required, an absent value is left out
silently. That hides a column, a field or a cell that the response never
had, as well as one that is merely empty this time.

- A required `csv` rule distinguishes the two: `CSV column "x" is not in the
  response` and `CSV column "x" is empty in row 3`.
- A `csv` label that names a column the response does not have fails its
  rule, required or not, instead of being left off every series
  ([Reading CSV: what to expect](#reading-csv-what-to-expect) lists what
  removes a column).
- A rule that is not required still reports nothing about a value's column
  that has disappeared.

While you write a collector, leave its rules required, so that a missing
column is reported, and read the header the target sends in a
[debug probe](#debug-probes).

**Errors of `css` and `xpath` rules.** The error names the rule's metric
first. Where the failure belongs to one of the things the rule selected, it
also says which: an item of a `css` rule's `items`, or a node of those an
`xpath` rule selected, each counted from 0 in page order. For example:

- `metric "host_load" value is missing for node 3: HTML XPath
  "//td[@class='load']" selected a node without a value` is the fourth cell
  (over XML the expression is named `XPath`);
- `metric "host_load" node 3: value "n/a" is not a number` is its text;
- `metric "host_load" label "host" is missing for node 3` is a required
  label.

A `css` rule says `item 3` in the same places. The message reads the same in
the log, in a debug probe and in the answer of a failed probe.

A probe that runs out of time inside a rule is not that rule's failure, and
no mode applies to it. The probe fails as a whole (see
[Probe deadlines](#probe-deadlines)).

#### When the XPath engine fails on an expression

The XPath engine accepts some expressions at startup that it later cannot
evaluate, once a response contains what they fail on. Examples:

- `//a[contains(@x, 5)]`, as soon as an `a` has an `x`, because the second
  argument must be a string, `'5'`;
- `sum(string(//v))` over text that is not a number;
- `substring(//v, '1')`, `replace(//v, '(', '')` and
  `substring(//v, 0 div 0)`.

That is a failure of the rule whose expression or label it is, handled by
its `error_mode` like any other:

- The rule gives no series for that response, including those of the nodes
  before the one the engine failed at. The other rules give theirs.
- The failure is counted and logged as one failure of the rule, the
  engine's, whatever nodes of the rule failed before the engine did. Their
  failures are not counted in addition, in
  `http_exporter_rule_failures_total` or as missing values, and it is the
  engine's failure that is logged.

The error names the metric, the expression (for a label's expression, with
the label's name) and what the engine reported:

```text
metric "marked" XPath "//a[contains(@x, 5)]" cannot be evaluated: the XPath engine failed on it: contains() function argument type must be string
metric "job_up" label "kind": XPath "substring(../@kind, '1')" cannot be evaluated: the XPath engine failed on it: substring() function first argument type must be number
```

The text after `failed on it:` is the engine's own. Where it starts with
`runtime error:`, as for `substring(//v, 0 div 0)`, a position that is not a
number, the fault is in the engine or in the exporter, not in an argument.
Write the expression another way.

The text after `runtime error:` may change with the response:
`substring(//v, 0 div 0)` over a text of two characters fails with `slice
bounds out of range [9223372036854775807:2]`, and with another length over
another text. The log therefore treats such a failure as the same one
whatever follows, and suppresses its repeats.

With `--log.level=debug`, the stack of a runtime error is logged in a line
`the XPath engine failed with a runtime error`, with the collector and the
metric, under every `error_mode`, including `ignore`. A debug probe's
report, which shows the lines of every level, includes it too. The stack
appears in no error text and in no line of another level.

### When a stage of the probe fails

`error_handling` covers the stages before any metric: fetching (reaching an
`http` target and its HTTP status, or reading a `localfile` file), decoding
the response, and the transform as a whole. It uses the same words as
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
| `log` | carries on without that stage's output, and counts as a success without [`report_failures`](#reporting-failed-probes-as-metrics) | yes, at warning level |
| `ignore` | carries on without that stage's output, and counts as a success without `report_failures` | only at debug level |

A probe that carries on is answered `200` with an empty exposition in the
format that the scrape asked for, exactly like a probe whose rules produced
no series: the text format's `Content-Type`, or OpenMetrics' own with its
closing `# EOF`, compressed when the scrape accepts gzip. An exposition is
identified by its `Content-Type`, so the answer is a valid exposition
although it holds nothing of the collector's. The scrape then succeeds,
which is the purpose of carrying on.

- The failure is still counted in the collector's
  [self-metrics](SELF-METRICS.md).
- The probe counts as a success, unless the collector sets
  [`report_failures`](#reporting-failed-probes-as-metrics), which makes it a
  failed probe that is answered `http_exporter_probe_success 0`.
- Nothing is kept in the [response cache](#response-caching) or exported
  over OTLP.
- Carrying on is not a failure, so with
  [`cache.stale_if_error`](#serving-the-last-good-result-when-the-target-fails)
  the last good result does not stand in for it, again unless the collector
  sets `report_failures`. The answer then holds the two freshness series
  that every answer of such a collector has, with
  `http_exporter_result_stale 0`, and nothing else.

A [static target](STATIC-TARGETS.md) follows the same policies.

- Under `fail`, its scrape fails: `http_exporter_target_up` is `0` and, with
  [`cache.stale_if_error`](#serving-the-last-good-result-when-the-target-fails),
  the last good result is exported in its place.
- Under `log` and `ignore`, the scrape carries on as a probe does: the
  target is up, with nothing of the collector's to export, only its health
  series, and the scrape counts as a success.
- A metric rule with `error_mode: fail` fails the scrape whatever
  `on_transform_error` says, for a probe and a static target alike.

### What a failed probe's caller is told

A failed probe is answered with its stage and its error. That is all
Prometheus needs to mark the scrape as down, and it is what someone running
the probe by hand reads first. However, anyone who may call `/probe` may
name the target and the collector's request parameters, so what the target
answered is not theirs to read. The answer tells them what failed, and the
exporter's log has the full error. The following is withheld from the
answer:

- **Every value of the target's answer that an error quotes.** That includes
  the token that a decoder could not read (`expected float as value, got
  "..."`), a label value or a name from the document, a CSV header, a
  charset that the answer names, the start of a Graphite server's answer
  that is not render JSON, and a `tcp` server's
  [greeting](TCP.md#services-that-speak-first) that did not match. Each is
  replaced by `[withheld; in the exporter's log]`, and the rest of the error
  reads as it does in the log. The names of valid series are not withheld,
  because a successful probe answers them anyway.
- **What a library quotes of the answer**: an XML syntax error's names and
  entities, a TOML token, and a jq program's error on the answer (`cannot
  iterate over: string ("...")`, or a rule's own `error(...)`), which is
  answered as `the jq program failed on the answer: [withheld; in the
  exporter's log]`. Go's HTTP client quotes the first line of an answer that
  is not HTTP, such as an SSH or SMTP banner. That is withheld after
  `malformed HTTP response`, for a target and for a
  [login](AUTHENTICATION.md#when-a-login-fails) alike.
- **A script's traceback**: a [Python](PYTHON.md) transform that raised is
  answered with the exception's type and the line of the script it came
  from: `python transform failed: KeyError at line 3 of the script; the rest
  is in the exporter's log`. Its message and the source lines are derived
  from the data. What the interpreter wrote to stderr is reported as being
  in the log.
- **What a server said about a refusal**:
  - a login provider's answer
    ([When a login fails](AUTHENTICATION.md#when-a-login-fails));
  - a database's message, which is answered by its code
    ([SQL](SQL.md#errors-and-self-metrics));
  - a Kubernetes API server's message and its URL
    ([Permissions](KUBERNETES.md#permissions));
  - an S3 store's message, which names the principal and the account, and
    the message of the AWS SDK's credential chain
    ([S3](S3FILE.md#errors-and-self-metrics));
  - an [`sshexec`](SSHEXEC.md) command's stderr.
- **The operator's own accounts and addresses**: an LDAP bind's DN
  ([LDAP](LDAP.md)), and the addresses on the exporter's side of the
  network. Those are:
  - the address and port that a connection was made from
    (`read tcp [withheld; in the exporter's log]->10.1.2.3:443`);
  - the proxy that a request went through, with everything that a
    connection to it, or a SOCKS proxy's handshake, failed with;
  - the name server that a lookup asked;
  - the path of the [Unix socket](REQUESTS.md#over-a-unix-socket) that a
    request went to.

  The address that the connection went to stays, because the caller named
  it.

An error over the 2,000 bytes to which every failure is cut is cut after the
withheld parts have been removed, so nothing withheld reaches the answer
through the cut. A rule under `error_mode: fail` is answered with its JSON
object, with the same text in `error`. The log, a
[debug probe](#debug-probes)'s report and, with `--web.debug=full`, the
[status page](ENDPOINTS.md#status-page) show each error in full. The
self-metrics count failures by stage, as before.

### Reporting failed probes as metrics

A failed probe is answered `502`, so Prometheus marks the scrape as down and
keeps no sample of it: an uptime check then has only `up` to alert on. With
`report_failures: true` on the collector, a probe is always answered `200`
with an exposition, as
[blackbox_exporter](https://github.com/prometheus/blackbox_exporter)
answers, and a failure is a value:

```yaml
collectors:
  - name: uptime
    report_failures: true
    request:
      type: http
      method: HEAD
      accept_status: [any]   # every status is the answer
    decoder:
      type: none             # no body is read
    transform:
      type: jq
    metrics:
      - name: site_status_code
        expression: $status
```

A probe that succeeds is answered with the collector's series, followed by:

```text
site_status_code 200
http_exporter_probe_success 1
http_exporter_probe_elapsed_seconds 0.012
```

A target that is down, refuses the connection, cannot be resolved, answers a
status the collector does not accept, or fails a stage in any other way is
answered:

```text
http_exporter_probe_success 0
http_exporter_probe_elapsed_seconds 0.003
http_exporter_probe_failed_stage{stage="http"} 1
```

- `http_exporter_probe_success` is `1` or `0`.
- `http_exporter_probe_elapsed_seconds` is how long this probe took, from
  its request to its answer. It is the duration that its
  [`http_exporter_probe_duration_seconds`](SELF-METRICS.md) histogram on
  `/metrics` observes, which is a separate series.
- `http_exporter_probe_failed_stage` names the stage that failed, the same
  one the log and the self-metrics name:
  - `http` for anything that failed in reaching an `http` target: its name,
    its connection or its TLS handshake;
  - `http_status`, `decode`, `transform` and `validation`;
  - `metric` for a rule with `error_mode: fail`;
  - another request type's own fetch stage: `dns`, `tls`, `file`, `grpc` and
    the others ([Request types](#request-types));
  - `panic` for a probe that panicked, in whatever stage. The log has it as
    `probe panicked` with its stack, and the status page and the
    self-metrics count it as failed, as before.

Nothing of the error is in the answer, as nothing of the target's answer is
([What a failed probe's caller is told](#what-a-failed-probes-caller-is-told)).
The log has the error, as always, and so do a
[debug probe](#debug-probes)'s report and the
[status page](ENDPOINTS.md#status-page). The key works the same for every
request type. The three series carry none of the collector's labels
(`transform.labels` and `metrics_prefix` apply to the collector's own
series); Prometheus adds the target's.
[Uptime probes](REQUESTS.md#uptime-probes) combines the key with the rest
of an uptime check of HTTP sites.

How `report_failures` interacts with other settings:

- **`error_handling` `log` or `ignore`** carries on past a failed stage as
  before ([When a stage of the probe fails](#when-a-stage-of-the-probe-fails)),
  but the probe is a failure. The answer says that the stage failed
  (`http_exporter_probe_success 0` with the stage), and so does everything
  else that counts probes, so that nothing disagrees with the answer:
  - the self-metrics count no success for it;
  - the status page shows it as failed, with its error;
  - the last good result stands in for it under `cache.stale_if_error`;
  - its trace is a failed one.

  It is logged as its policy says, at warning level under `log` and at debug
  level under `ignore`, and it is not cached, as before. Without
  `report_failures`, such a probe is a success, as it always was.
- **A rule with `error_mode: fail`** fails the probe in the `metric` stage.
  A rule under `log` or `ignore` that drops its series fails no stage, and
  the probe is a success.
- **[`cache.stale_if_error`](#serving-the-last-good-result-when-the-target-fails)**
  answers the last good result in place of a failed trip, including a trip
  that carried on past a failed stage, with `http_exporter_result_stale 1`,
  plus `http_exporter_probe_success 0` and the stage. The series stand in
  while the target is failing.
- **[Response caching](#response-caching)**: a cached answer is a success.
  The cache keeps only the collector's series; each answer has its own
  elapsed time.
- **[Shared probes](#identical-probes-share-one-request)**: probes that share
  a trip each answer their own elapsed time.
- **[`failure_backoff`](#backing-off-from-failing-targets)**: a probe kept
  from the target fails in its fetch stage.
- **A probe refused by the target policy** (`403`), **one turned away by
  `max_concurrent_probes`** (`503`), one whose caller went away, and a
  caller's mistake (`400`) are answered as without the key. The target was
  not contacted, and they say nothing about it.
- **The self-metrics** count a failure as a failure, in
  `http_exporter_scrapes_total` against `http_exporter_scrape_success_total`
  and the other counters, and in the probe duration histogram. It is
  [logged](LOGGING.md) as before.
- **The [OTLP export](OTLP.md)** exports the collector's series, as before.
  It exports nothing of a failed probe, including one that carried on past a
  failed stage, and never the three series. A stale result that stands in is
  exported as it is without the key. The trace of a failed probe is a failed
  one, which the `errors` sampling keeps, although the probe is answered
  `200`. Its `error.type` is the stage that failed, including `panic`,
  instead of the `502` or `500` it would have been answered with without the
  key. A stale result that stands in keeps `stale`, and a probe that
  `failure_backoff` kept from the target keeps `backoff`.
- **The [status page](ENDPOINTS.md#status-page)** shows that the probe
  failed, with its error, answered `200` with its series.
- **A [debug probe](#debug-probes)** says that a probe would have answered
  `200` with `http_exporter_probe_success 0` in place of the failure or,
  where a stale result stands in, `200 with the last good result, 5s old,
  marked stale (cache.stale_if_error), with http_exporter_probe_success 0
  and the decode stage failed (report_failures)`. It shows the series.
- **[Static targets](STATIC-TARGETS.md)** are not changed by the key. A
  static target's scrape has `http_exporter_target_up` and its own
  self-metrics, and a collector that is both probed and named by static
  targets serves each in its own way.

While `report_failures` is set, the collector's rules must not produce a
series with one of the three names. A rule or a derived family exported
under one, after `metrics_prefix`, is refused when the configuration loads:

```text
collector "uptime" metric "http_exporter_probe_success" is exported as http_exporter_probe_success, which is reserved for the series report_failures adds to every probe's answer; rename the metric, or leave report_failures out
```

A series that a [Python](PYTHON.md) script or a `prometheus` transform
produces under one of the names fails the probe in the `validation` stage.

### Checked when the configuration loads

Everything about a metric that can be known before a scrape is checked at
startup, on reload and by `--dry-run`. An error names the collector, the
metric and the label. A rule without a usable name (`name` omitted, written
`""` or only blanks) is identified by its position among the collector's
rules, counted from 1: `collector "node" metrics rule 2 has no name`. For a
`prometheus` rule, which needs no name, the same form is used: `collector
"node" metrics rule 2 has invalid type "timer"`, where a rule with a name
reads `collector "node" metric "up" has invalid type "timer"`.

The checks are:

**Names**

- A metric name must be a valid Prometheus metric name and must not start
  with `__`. A rule of any transform except `prometheus` must have one.
  Under `prometheus`, a name that looks like a pattern gets the message that
  [a pattern belongs in `expression`](#collectors). Under
  [`name_escaping`](#utf-8-names) `underscores` or `values`, a rule's name,
  and every other name the collector writes, may be any name that is not
  only blanks, and is exported escaped.
- A rule's name must not be longer than `limits.max_metric_name_length` as
  it is exported (after `metrics_prefix`, escaped by `name_escaping`),
  because every series of it would fail the scrape: `collector "c" metric
  "M" is 201 bytes, longer than limits.max_metric_name_length 200, so every
  series of that name would fail validation; shorten the name or raise
  limits.max_metric_name_length`, or, with a prefix, `collector "c" metric
  "M" is exported as "p_M", which is longer than
  limits.max_metric_name_length 200`. This applies to the name of a
  `prometheus` rule and a `python` rule too. Both the scrape and the load
  measure the family name of a histogram or a summary, not its `_bucket`,
  `_sum` or `_count`. A `prometheus` rule without a `name` has nothing to
  measure.
- A rule of a collector with
  [`report_failures`](#reporting-failed-probes-as-metrics) must not be
  exported as `http_exporter_probe_success`,
  `http_exporter_probe_elapsed_seconds` or
  `http_exporter_probe_failed_stage`, and neither may a derived family that
  is not hidden.
- No rule may be named like a series of another rule's histogram or summary:
  `foo_bucket`, `foo_sum` or `foo_count` beside a histogram `foo`, or
  `foo_sum` or `foo_count` beside a summary `foo`.
- Rules with the same metric name must give it the same type. Several rules
  may feed one family, as `jobs{queue="a"}` and `jobs{queue="b"}` read from
  two places, but a family has one type. A `prometheus` rule without a
  `type` keeps the series' own type and is not compared.
- No two rules of a collector may be
  [the same rule](#two-rules-that-are-the-same-rule). Each would produce
  every series the other produces, and every scrape would fail on a
  duplicate series.
- Under `prometheus`, no rule's `expression` may match the `name` of a
  metric that another rule passes on, where the two give its series
  [one name and the same labels](#a-name-and-a-pattern-that-matches-it),
  because each would produce every series of that metric. A rule that is
  only for histograms or only for summaries, by its `type`, is allowed.

**Expressions and rule settings**

- Every expression must compile in its transform's language: jq and yq
  (including `items`, and undefined functions and variables), regular
  expressions, CSS selectors, XPath with the collector's namespaces, and the
  patterns of a `prometheus` transform, `include` and `exclude`.
- A `regex` rule must have a capture group, which is its value, and at most
  one group named `value`. A `regex` label must name a capture group that
  the regex has.
- A rule's [`time_format`](#reading-a-date-or-a-time-as-the-value) must be
  `rfc3339`, `rfc1123` or a layout with a year, a month and a day, without
  surrounding blanks and with `PM` beside the hour of a 12-hour clock. Its
  `time_zone` must be a zone the exporter knows. `time_zone` without
  `time_format`, `time_format` together with `value_map`, and `time_format`
  on a `python` or `prometheus` rule are refused.
- A `prometheus` transform's `rename` targets must be metric names.
  `include`, `exclude` and `rename` apply only to a `prometheus` transform
  without `metrics` rules, and an entry of `include` or `exclude` must not
  be the empty string or [only blanks](#collector-wide-labels).
- A `prometheus` rule's `expression` must not be only blanks, and the rule
  must have a `name` or an `expression`, which is how it says which metrics
  it passes on.
- A label of a `python` rule must not set `value`. The script sets its
  labels itself, and a constant for every series belongs in
  [`transform.labels`](#collector-wide-labels).
- A `python` rule must have a `name`, the script's series it refers to, and
  no `type`, `description`, `required` or `error_mode`, which the script
  determines for its series itself. Each of its labels must set
  `truncate: true`
  ([What a rule of a `python` collector is for](PYTHON.md#what-a-rule-of-a-python-collector-is-for)).
- A rule's [`histogram` block](#histograms-from-a-list-of-values) must be on
  a rule of `type: histogram` of a transform other than `prometheus` and
  `python`. Its `buckets` must be 1 to 100 finite numbers in increasing
  order, each once, without `+Inf`, and such a rule must have no label named
  `le`. `type: histogram` without the block is refused for every transform
  except `prometheus`, with an error that says the block is how to count
  values into a histogram.
- A [`derived`](#derived-series) entry must have a name that no rule of the
  collector and no other entry has, the type `gauge`, `counter` or
  `untyped`, and exactly one of `sum`, `difference` and `ratio`. Each
  operation reads counters, gauges or untyped families that are rules of the
  collector or earlier entries. `by` is allowed only with a `sum` of one
  family, and a `hidden` entry must be read by a later one.
- `labels_from` is a key of a `jq` or `yq` rule, and its expression must
  compile.

**Labels**

- `transform.labels` and `rename_labels` must give label names: classic
  ones, unless `name_escaping` escapes the others. Two renames must not
  target the same label. A key of `transform.labels` or a rule's label that
  `rename_labels` renames or `remove_labels` removes may be any name that is
  not only blanks, because it is never exported.
- No label name, of a rule, `transform.labels`, `rename_labels` or a static
  target, may start with `__`, unless `rename_labels` renames it or
  `remove_labels` removes it. Prometheus reserves such names for its own
  labels: it refuses `__name__` in what it scrapes and drops the others. A
  series whose labels still get such a name, from a Python script or a
  passthrough, fails validation.
- A histogram must not be given a label `le` of its own, nor a summary a
  label `quantile`, by a rule's label, `transform.labels` or a pre-script,
  because its buckets or quantiles carry that label. The scrape fails and
  names the series, as in `metric "h" is a histogram and has a label le of
  its own, which its buckets carry; name the label something else`. On any
  other series, `le` and `quantile` are ordinary labels.
- A rule must name each of its labels once. A second label with the same
  name is refused, as `collector "c" metric "m" has two labels named "env";
  a series has one label of a name, so remove one of them or give it another
  name`, because a series would get the later of the two and the earlier
  would be lost.
- A label name of a rule, of `transform.labels`, or one that
  `transform.rename_labels` renames to must not be longer than
  `limits.max_label_name_length` as `name_escaping` exports it, unless
  `transform.rename_labels` renames it or `transform.remove_labels` removes
  it.
- A `description` must not be longer than `limits.max_help_length`.
- A static label value, of a rule or of `transform.labels`, must not be
  longer than `limits.max_label_value_length`, unless the label is cut at
  the scrape. It is cut where it has `truncate: true`, where any rule of the
  same metric name sets `truncate: true` on a label of that name, where a
  `value_map` maps it to something shorter, or where `remove_labels` drops
  it.
  - A value that contains `{{param_...}}` placeholders is measured with each
    one replaced by its default, or by nothing where it has none.
  - Where a `value_map` of the rule's name maps such a value of a rule's
    label, the value is measured as mapped. A `"*"` entry longer than the
    limit is refused whatever the defaults, because every value the probe
    gives that the map does not list becomes it. A longer entry that the map
    lists is refused only where the defaults give its key. Otherwise it
    loads, and fails the scrape of a probe that gives that key.
  - A `prometheus` rule without a `name` keeps each series' own name, and a
    `value_map` or `truncate: true` of a rule named like one of those series
    applies to it. Its static label is measured as it is, for the names no
    rule has, and also as mapped and cut by each other rule whose name its
    `expression` matches. The match is made as at the scrape: unanchored
    unless the pattern says otherwise, against the name the target wrote,
    before `metrics_prefix` and `name_escaping`.
- The labels that every series of a collector gets (those of
  `transform.labels`, and a rule's static labels) must not number more than
  `limits.max_labels_per_metric`. Each name is counted once, as
  `remove_labels` and `rename_labels` leave them: `collector "c" metric "m"
  gives each of its series 21 labels, its static labels and
  transform.labels, more than limits.max_labels_per_metric 20`.
  - A label that a series might not have is not counted: a value with a
    `{{param_...}}` placeholder, which a probe may fill with nothing; a
    static label with a name that some `value_map` may map to `""`; and a
    label that an `expression` gives.
  - The labels that the exporter adds afterwards, such as a static target's,
    are not counted either.

**Decoders, settings and placeholders**

- An explicit `decoder.type` must be one the transform
  [reads](#collectors).
- A setting must belong to the decoder or transform the collector has:
  `transform.script` to a `python` transform; `transform.libraries` and
  `required_libs` to a collector with a `python` transform or a
  `pre_script`; `response.csv` to the `csv` decoder; `response.graphite` to
  the `graphite` decoder; `response.yaml` to the `yaml` decoder (each of
  these three is also accepted when the decoder is left to each response
  with `auto`); and `response.namespaces` to an `xpath` transform. Anywhere
  else the setting would be ignored, so it is refused, with an error that
  names it and the decoder or transform the collector has instead.
- `response.csv.delimiter` must be one character, and not a double quote or
  a line break. For tab-separated values write `delimiter: "\t"` in double
  quotes, where YAML reads `\t` as the tab character. In single quotes, or
  unquoted, `\t` is a backslash and a `t`, and is refused.
- A `{{param_...}}` placeholder is allowed only where a probe's parameters
  are filled in: in the
  [request](REQUESTS.md#in-the-body-headers-and-query) and in a
  [fixed label value](REQUESTS.md#in-label-values), of `transform.labels` or
  of a rule's static label. In any other setting of a collector
  (`request.bearer_token`, `basic_auth`, `tls.server_name`, a `value_map`, a
  label's name) it would be sent or exported as written, so it is refused,
  with an error that names the field. A Python script, an expression and a
  description are not searched, because the text there belongs to the
  script, the expression or the description.
- A placeholder in a label value must be well formed: `{{param_<name>}}` or
  `{{param_<name>:<default>}}`, without a space after the braces and without
  a filter, which a label value does not take.

**Limits**

- A limit must be a whole number from 0.
  - A negative one, as in `limits.max_metrics: -1`, and one with a fraction,
    as in `max_concurrent_probes: 1.9`, are refused with an error that names
    the key and the value, instead of silently becoming the default or
    losing the fraction.
  - 0, like an omitted limit, means the default.
  - A limit written with a point or an exponent, such as `1e3` or `4.0`, is
    the whole number that it equals, read by its digits.
  - One too large for an int64, such as `1e19`, or even for a floating-point
    number, such as `1e400`, is refused with an error that names the key, the
    number and the range.

  [Sizes](#sizes) follow the same rules.
- `limits.max_output_bytes`, when set, must be at least 38 bytes, which is
  what a [Python script](PYTHON.md#how-scripts-run) that emits no metric
  answers in. A smaller value is refused, in a collector with a script and
  in one without, as `collector "c" limits.max_output_bytes is 26, and a
  Python script that emits no metric answers in 38 bytes, so no transform's
  script could answer within it; set at least 38, or leave it out, or 0, for
  the default, 1MiB`. It used to load and then fail every scrape of a
  collector whose transform is a script.

**YAML values**

- A mapping key that YAML reads as no key at all (`null`, `~`, or nothing
  before the colon, as in `value_map: {null: 0}`) is refused, because the
  entry would be dropped. Quote it, `"null"`, when that text is the key.
- A value that YAML reads as none (nothing after the colon or the dash,
  `null` or `~`) is refused wherever the file takes a value: for a key, an
  entry of a list, or a value of a mapping.
  - YAML gives the exporter nothing for it. A key written `ttl:` used to
    count as omitted, a bare `-` among the `metrics` was no rule at all, with
    nothing to report the missing rule, and `value_map: {up: }` mapped `up`
    to 0.
  - The error names the line and the place, and says what to do: `line 12:
    metrics entry 1 is a dash with nothing after it, which YAML reads as no
    value at all, so the entry would be left out without a word; write a
    metric rule there, or take the entry out`, or `line 7: ttl has nothing
    after its colon, which YAML reads as no value at all; write its value, or
    take the key out`.
  - To leave a key at its default, omit it. A key of text may also be written
    `""`, which counts as omitted ([Editor support](#editor-support)), and a
    block that sets nothing as `{}`.
  - A file's own top-level `x-` keys may hold anything.
- A value must be written as the kind its key takes. YAML reads an unquoted
  `true` or `False` as a boolean and `1`, `1.5e3` or `0x1F` as a number, and
  gives a key of text the text of whatever is written.
  - Where the text is free (a `description`, an `expression`, a label's
    `value`, a header, a query parameter, a body, a path, a `value_map`'s
    keys and a label's `value_map` texts), that is what was meant, so
    `value: 1`, `description: 404` and `value_map: {1: running}` need no
    quotes.
  - A key that takes a name, one of a few words, a host or a single
    character accepts text only. Such keys are a collector's, a rule's and a
    label's `name`, `metrics_prefix`, `name_escaping`, `request.type`,
    `method`, `rpc` and `descriptors`, `decoder.type`, `transform.type`, the
    error policies, a rule's `type`, `error_mode` and `time_zone`,
    `response.csv.delimiter`, `response.graphite.value` and `invalid_lines`,
    an entry of `collector_files`, `redirect_trusted_hosts`, `accept_codes`,
    `retry.codes`, the Python libraries, and a static target's `name` and
    `request.method`. A number or a boolean there used to be read as its
    text, so `name: true` was the metric named `true`. It is now refused:
    `line 9: name is written true, which YAML reads as a boolean, not as
    text; to use that text there, quote it: "true"`.
  - A key that takes one of a few words is told the words, because quoting a
    number would not make it one of them: `line 5: type is written 1, which YAML reads as a number, not as text; write one
  of dns, graphite, grpc, http, icmp, kubernetes, ldap, localfile, ntp, redis, s3file, snmp, sql, sshexec, sshfile, tcp, tlscert`.
  - A boolean is `true` or `false`. `yes`, `no`, `on`, `off`, `y` and `n`
    were booleans in YAML 1.1 and are text now, to an editor and wherever
    the exporter takes text: a label value of `NO`, for Norway, is `NO` and
    not `false`. A key that takes a boolean therefore refuses them, quoted
    or not, where it used to read them as booleans: `line 3: enabled is
    written yes, which YAML reads as text, not as a boolean; write true`.
  - A date written without quotes, `2026-10-06`, is also text wherever text
    is taken. A number or a boolean in quotes is text, which no key of
    numbers or booleans accepts.

The last four checks look at the values YAML gives the exporter.

- With a [merge key](#reusing-settings-with-yaml-anchors), a key the mapping
  sets itself replaces the merged one, and in a list of merges
  (`<<: [*a, *b]`) the first that sets a key is the one read. So
  `limits: {<<: *defaults, max_metrics: 500}` loads even if the anchor's own
  `max_metrics` is `0.5`, because that value is never read there. A value
  that is read is refused, at the line where it is written.
- A key written as an alias, `*k : 7.0` with `&k max_metrics` elsewhere, is
  the key its anchor holds, as YAML reads it. Its value is read and refused
  as that key's value, and named accordingly.

Each of these would otherwise load and then fail every scrape's validation,
or be ignored, whatever the target answered.

### Two rules that are the same rule

Several rules may export one metric name, each reading its series from a
different place or giving them a different label. Two rules that are alike
in everything that determines which series a rule makes are a different
case: they are the same rule, written twice. Each produces every series the
other produces, with one name and one set of labels, and a scrape that has a
series twice fails (`validation failed: duplicate metric series "up"`) on
every scrape, for as long as the configuration runs. The pair is therefore
refused when the configuration loads. The error names both rules by their
positions among the collector's rules, counted from 1, and the metric:

```text
collector "node" metrics rule 1 and rule 3 are the same rule of metric "up": alike in name, expression, items and labels, each makes every series the other makes, and a scrape that has a series twice fails, as a duplicate metric series; take one of the two out, or tell their series apart by a label, as with a static label that has another value in each
```

A `prometheus` rule without a name is identified by its pattern: `… are the
same rule of the metrics that match "^node_": …`. Each copy of a rule is
reported once, against the first.

| Of two rules | Compared | Why |
| --- | --- | --- |
| `name`, `expression`, `items` | yes, as written; `""` is the key left out | they say which series the rule makes and from what |
| `labels` | yes: the same label names, each with the same `value` or the same `expression`, in any order; a `value` as written, [placeholders](REQUESTS.md#in-label-values) and all | a series is its name and its labels |
| `value_map`, `time_format` | yes | they say which texts the rule reads a value from: of two rules that differ in one, each may read what the other cannot — a field that is `up` or a number, a time written one way or another — and a scrape gets one series |
| `scale`, `time_zone`, `description`, `required`, `error_mode` | no | they change the value or the help text of a series, or what happens when there is none, never which series it is |
| a label's `truncate`, `required` and `value_map` | no | the first two change no label's name, and a label's `value_map` is one for [all the rules of a name](#mapping-text-to-values-and-scaling-them) |
| `type` | no | rules of one name have [one type](#checked-when-the-configuration-loads) |

Two rules that differ only in a `scale`, a `description`, `required` or
`error_mode` are therefore the same rule, and are refused: whatever value
each gives, it is a second value for one series. To export one name from two
rules, distinguish their series by a label (a static label with a different
value in each is enough), or by reading a different `expression` or
different `items`.

Under `prometheus`, where a label reads a label of the series the rule
passes on, the order of a rule's labels is compared too when one of them
reads a label that the rule itself sets under another name, because the
order then determines the value.

The comparison is of what the rules write, not of what they mean. Two
expressions that select the same thing in different words, such as `.v` and
`(.v)`, or the `prometheus` expressions `'^up$'` and `'^(up)$'`, are two
rules to it. They load, and the scrape fails on the duplicate series as
before. A `python` collector's rules produce no series (the script does), so
two identical ones change nothing and are not refused. A JSON schema cannot
compare the items of a list in this way, so this is one of the checks that
only the exporter makes ([Editor support](#editor-support)).

### A name and a pattern that matches it

One pair of `prometheus` rules that select the same thing in different words
can be detected from the configuration alone, and is refused when it loads.
A rule without an `expression` passes on the metric that its `name` names. A
rule with an `expression` passes on every metric the expression matches.
Where the expression matches that same name, both pass the metric on:

```yaml
metrics:
  - name: up
    description: Whether the target is up.
  - expression: '.*'
```

Each rule produces every series of `up`, and a scrape that has a series
twice fails (`validation failed: duplicate metric series "up"`) on every
scrape of a target that has the metric. On a target that does not have it,
the first rule produces nothing and reports the metric as missing, unless it
has `required: false`. The first rule, which is about that one metric, thus
contributes a series to no scrape that passes, so the load reports:

```text
collector "node" metrics rule 1 and rule 2 both pass on metric "up": rule 1 passes on the metric of that name, the expression ".*" of rule 2 matches that name, and their labels are alike, so each makes every series of the metric, and a scrape that has a series twice fails, as a duplicate metric series; take one of the two out, write the expression so that it does not match "up", or tell their series apart by a label, as with a static label that has another value in each
```

The expression is matched against the name as it is against a metric of the
target: compiled the same way and matched anywhere in the name, so `up`,
`'^u'`, `'(?i)^UP$'` and `'.*'` all match `up`. The pair is refused
whichever of the two rules is written first, when both of these hold:

- The rule with the expression has no `name`, and so keeps the metric's own
  name, or has that same `name`. With another name, as in
  `{name: alive, expression: '^up$'}`, it exports the metric under that
  name, and the two rules load.
- The two are alike in `labels`, in the sense of
  [two rules that are the same rule](#two-rules-that-are-the-same-rule). A
  label in one that the other does not have, or has with a different `value`
  or `expression`, distinguishes their series as far as the configuration
  can tell, and the rules load.

Nothing else that the rule with the name sets changes this, as in that
section: a `description`, a `scale`, a `type`, `required`, `error_mode` and
a label's `truncate` and `required` do not determine which series a rule
makes. With a `type` other than the metric's own, the scrape fails on a
metric of two types before it reaches the duplicate. Where its `scale` or
`type` cannot apply to the metric, or a label it requires is missing, the
rule fails on the series instead of producing it, so it produces no series
of the metric there either.

Nothing else that the rule with the expression sets keeps the pair from
being refused either, with one exception: a `type` of `histogram` or
`summary` (see below). Such settings can change what a scrape makes of the
pair, though. A rule that sets a `type`, a `scale` or a `required` label can
fail on a series instead of producing it:

- a histogram or a summary keeps its own type and takes no scale;
- a `type` of `histogram` or `summary` applies to no other metric;
- a series might not have the label.

Under `error_mode: log` or `ignore`, the rule then carries on without that
series, and the scrape passes with the one that the rule with the name
produced. What such a pair does therefore depends on the target:

| The rule with the expression sets | Each rule makes the series, and the scrape fails | That rule fails on the series, and the scrape passes | The load |
|---|---|---|---|
| `type: gauge`, `counter` or `untyped` | of a gauge, a counter or an untyped metric: on a duplicate where the type is the metric's own, on `inconsistent types` where it is not | of a histogram or a summary | refuses the pair |
| `type: histogram` or `summary` | of a metric of that type | of any other metric | takes the pair, unless the rule of the name sets that `type` too |
| `scale` | of a gauge, a counter or an untyped metric | of a histogram or a summary | refuses the pair |
| a `required` label | where the series has the label | where it has not | refuses the pair |

`{name: request_duration_seconds}` beside `{expression: '.*', scale: 0.001}`
used to pass on a histogram of that name once, and to fail the scrape of a
target where it is a gauge. `{name: up}` beside
`{expression: '^up$', type: gauge}` failed every scrape of an ordinary
target. Both are refused, whatever the target has. A setting that fails on
some series is not a way to keep a rule away from a metric, because every
series it does not fail on is produced twice. The load names what the rule
sets (its type, its scale, each label it requires) instead of saying that
every series is produced twice:

```text
collector "node" metrics rule 1 and rule 2 both pass on metric "request_duration_seconds": rule 1 passes on the metric of that name, the expression ".*" of rule 2 matches that name, and their labels are alike; the scale of rule 2 does not keep it from the metric, since every series of the metric that rule 2 does not fail on is made by each rule, and a scrape that has a series twice fails, as a duplicate metric series; take one of the two out, write the expression so that it does not match "request_duration_seconds", or tell their series apart by a label, as with a static label that has another value in each
```

Where one of the two rules sets a `type` that the other does not, the
message also says that the scrape fails `or, where the two rules give the
metric different types, as a metric of inconsistent types`. With
`error_mode: fail`, the rule fails the scrape wherever it fails on a series,
so no scrape of the pair passes, and the load reports it like the two rules
above. A `required` label that the rule with the name does not have is a
label that distinguishes their series, and those rules load.

**The exception** is a rule for the histograms alone, or for the summaries
alone: a rule with an expression whose `type` is `histogram` or `summary`,
under `error_mode: log` or `ignore`, is accepted beside a name it matches.
In this example that is the metric `up`, and every histogram:

```yaml
metrics:
  - name: up
  - expression: '.*'
    type: histogram
    error_mode: ignore
```

A `type` of `histogram` or `summary` applies to a metric of that type and to
no other. This rule therefore fails on, and carries on without, every gauge,
counter, untyped metric and summary. It is a rule for the histograms alone
and leaves an ordinary metric to the rule with its name. For a target with a
gauge `up`, a counter and two histograms, the scrape has `up` once and each
histogram once. With `error_mode: ignore` nothing is logged. With `log`, the
default, the scrape is the same, and the rule's failures on the metrics that
are not histograms are logged as a warning.

The pair still produces a series twice where the target's metric of that
name is itself a histogram (or a summary, under `type: summary`). Each rule
then produces it, and the scrape of that target fails with
`validation failed: duplicate metric series "up"`. The type of a metric is
decided by the target, and the load cannot know it. For a name whose metric
is a histogram, write the expression so that it does not match the name, or
remove the rule with the name, because the other rule already produces that
histogram.

Only the `type` decides, whatever else that rule sets. With a `scale` or a
`required` label in addition, the pair still loads: those keep the rule from
more series, not from fewer, and a rule with `type: histogram` and a `scale`
produces no series at all, because a histogram takes no scale. The pair is
refused like the others in two cases:

- The rule with the name sets that same `type`, as in
  `{name: latency, type: histogram}` beside
  `{expression: '.*', type: histogram}`. That rule then produces the metric
  only where the other produces it too: a histogram twice, and anything else
  not at all. With a different `type` in the rule with the name (`gauge`, or
  `summary` beside `histogram`), the rules load, and no scrape has a series
  twice. For a metric of the pattern's type, the rule with the name fails,
  and the other rule produces the one series.
- The rule is under `error_mode: fail`, where it fails the scrape on the
  first metric of another type.

Nothing else is refused. Rules for which the configuration does not show
that they pass on one metric, under one name and with the same labels, load
as before, and fail the scrape that has a series twice with
`duplicate metric series`:

- two rules that both have an expression, such as `'^node_'` and
  `'^node_cpu'`, which both pass on a metric only if the target has one
  that both match;
- a rule with a name beside a rule that exports another metric under that
  name, `{name: up}` and `{name: up, expression: '^node_up$'}`, whose
  series are the same only if the two metrics have the same labels;
- rules whose labels differ as written but end up the same on a target,
  such as a static `job: api` in one rule where the target's series already
  has that label.

Each pair is reported once, with both rules named by their positions among
the collector's rules, counted from 1. Copies of a rule are reported first,
as [the same rule](#two-rules-that-are-the-same-rule), and not again here. A
schema cannot check whether one item's expression matches what another item
names, so this too is a check that only the exporter makes
([Editor support](#editor-support)).

**How mistakes are reported.** Every mistake is reported at once, not one per
run: the mistakes of every collector and every rule, and of every
[collector file](#collector-files), in order, one per line, each quoting the
expression it concerns:

```text
collector "a" metric "x" expression ".foo[": unexpected EOF
collector "a" metric "bad-name": "bad-name" is not a valid Prometheus metric name; use letters, digits, underscores and colons, not starting with a digit, or set the collector's name_escaping to underscores or values to export it escaped
collector "b" metric "z" regex "value=\\d+" has no capture group; the value is the capture group named value, as in '(?P<value>\d+)', or else the first, so wrap the number in one, such as 'requests=(\d+)'
```

A mistake in a collector that a [collector file](#collector-files) defines
names that file, as in `collector file /etc/exporter/collectors.d/payments.yaml:
collector "pay" metric "depth" expression ".queue.depth[": unexpected EOF`.
A rejected reload whose mistakes are all in one collector file is logged
with that file as its `file`. With mistakes in several files, `file` is the
configuration, and the text names each file.

After 20 mistakes, the rest are counted (`and 5 more problems`). `--dry-run`
lists every one as a separate error. A collector whose request, limits or
formats are wrong stops at its first such mistake, because the rest of its
checks depend on them.

A CSS selector that does not compile used to match nothing on every scrape,
without an explanation. It is now refused when the configuration loads. The
expressions are compiled once at that point, and every scrape reuses them.

A file that cannot be read as a configuration is refused with errors
specific to the file. Each error names the line and what was expected there:

```text
line 3: unknown key "requst" in a collector; line 7: "fast" is not a duration; write one such as 500ms, 30s or 1m30s
```

The errors come in file order. On one line, as in a block written `{...}`,
they come in the order of the keys they concern: `defaults:
{bogus: 1, limits: {max_metrics: x}}` reports the unknown key first and then
the value that is not a whole number, every time the file is loaded. A block
is read to its end: `cache: {bogus: 1, ttl: fast, revalidate: maybe}` reports
all three of its problems, followed by the problems in the rest of the file.

Every unknown key is refused, including in a static target's `request`
block, because a misspelt key would otherwise be silently ignored. For the
same reason a file holds exactly one YAML document: the configuration, a
collector file and the static targets file are each refused when a second
document follows a `---`, with an error that names the line it starts on. A
leading `---`, and a final `---` or `...` followed only by comments, are
accepted.

### Editor support

[`configs/config.schema.json`](../configs/config.schema.json) is a JSON Schema of this file.
With the YAML extension for VS Code, or any editor that uses the YAML language
server, start a configuration with this line:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/eenchev/prometheus-universal-exporter/main/configs/config.schema.json
```

The editor then completes keys, shows what each one does, and flags unknown
keys and disallowed values as you type. The example configurations in
`configs/` (`config.example.yaml`, `config.otlp.example.yaml` and
`config.uptime.example.yaml`) start with that line; the ones under
`examples/` do not.

The other files have their own schemas, all in `configs/` beside the
examples and all regenerated with `make schemas` (see
[Development](DEVELOPMENT.md#the-configuration-schema)):

- the [static target file](STATIC-TARGETS.md):
  [`configs/static-targets.schema.json`](../configs/static-targets.schema.json), which
  `configs/static-targets.example.yaml` points editors at in the same way;
- the [target allocator](ALLOCATOR.md)'s static target file, which may have
  `discovered_targets`:
  [`configs/allocator-static-targets.schema.json`](../configs/allocator-static-targets.schema.json);
- the allocator's probe targets file:
  [`configs/probe-targets.schema.json`](../configs/probe-targets.schema.json);
- the [collector file](#collector-files).

`prometheus-universal-exporter --config.schema` prints the schema of the
binary you are running. Its `request.type` values are the request types that
binary was built with.

**Letter case.** A key that takes one of a few words takes them as the schema
writes them, and the exporter refuses any other spelling, as the schema
does:

- `request.type: http`, `transform.type: jq`, `decoder.type`, the
  `error_handling` policies and a rule's `error_mode` (`log`, not `LOG`),
  `request.pagination.type`, `request.descriptors`, and
  `response.graphite`'s `value` and `invalid_lines` are in lower case and
  without surrounding blanks.
- `request.method` is in upper case or lower case, `GET` or `get`.
- The keys whose values are matched in any case where they are used
  (`request.driver`, `request.record_type`, header names, `allowed_schemes`
  and gRPC status codes) take any case in the schema too.

**What only the exporter checks.** The schema is not the final check. Startup
validation also checks what a schema cannot:

- that an expression compiles;
- that a duration of several parts, such as `script_timeout: 400us400us`, is
  not under its key's minimum (a schema reads the digits of a one-part
  duration but cannot add up parts);
- that `failure_backoff.max` is not shorter than `initial`;
- that a duration is not too long to be represented;
- that a [size](#sizes) is under 2^63 bytes;
- that any other whole number, such as `limits.max_metrics`, fits an int64,
  from -2^63 to 2^63 - 1;
- that a collector given one of `tls.cert_file` and `tls.key_file` by its
  file's [defaults](#defaults-for-the-collectors-of-a-file) is given the
  other too;
- that two `request.headers` names are not the same header in different
  case;
- that no two rules of a collector are
  [the same rule](#two-rules-that-are-the-same-rule), or a `prometheus`
  [name and a pattern that matches it](#a-name-and-a-pattern-that-matches-it),
  because a schema cannot compare the items of a list by some of their keys.

**What the schema refuses too.** Where a schema can tell, it refuses what the
exporter refuses:

- a `response.csv.delimiter` of more than one character;
- a size with a fraction and no unit;
- a block that sets keys beside a missing `enabled`;
- a `value_map` key that is empty or has surrounding blanks;
- a label's `expression` of only blanks; an entry of `transform.include` or
  `transform.exclude` that is empty or only blanks; a `prometheus` rule's
  `expression` of only blanks;
- a `value` on a label of a `python` rule, and such a label without
  `truncate: true`; a `python` rule's `type`, `description`, `required` and
  `error_mode`;
- a `required` label whose `value_map` maps a value to `""`;
- a rule without a `name` under any transform except `prometheus`;
- a rule's or a label's `name` that is not a classic name, in a collector
  whose `name_escaping` is not `underscores` or `values`;
- a `grpc` collector that calls a service other than
  `grpc.health.v1.Health` without `descriptors`;
- a `request.headers` name (a collector's or a static target's), or an
  `otlp.headers` name in a block that is switched on, that is not a header
  name or is `Accept-Encoding`;
- a `request.query` name that is empty or contains a control character;
- a `forward_headers` entry that is not a header name or is one that is
  [never forwarded](AUTHENTICATION.md);
- a duration or a size under its key's minimum, written as a number or as
  text of one number and a unit, such as `limits.script_timeout: 500us`,
  `limits.max_script_memory: 16MiB`, `max_response_bytes: 1kB`,
  `otlp.interval: 500ms` in a block that is switched on, a static target
  file's `interval: 0.5s` and `failure_backoff.initial: 0s`;
- a `tls` block with a client certificate and no key, or a key and no
  certificate, except for the file a collector takes from its file's
  `defaults`;
- a request with two credentials, such as `bearer_token` beside
  `bearer_token_file`, or `basic_auth` beside a bearer token; a
  `basic_auth_file` without both its files;
- a `pagination` key that belongs to another way of paging, such as `next`
  beside `type: page`;
- a `grpc` collector's `protoset_file`, `proto_files` or
  `proto_import_paths` beside another source of `descriptors`, or that
  source without its files;
- a `python` transform without a `script`;
- a negative duration, such as `timeout: -5s`, which no key accepts.

**Durations.** A duration is written as Go writes one (`500ms`, `1h30m`,
`1.5s`), for the schemas as for the exporter.

- A `+` may lead it, and a `-` only a zero (`-0s`). Anything written
  negative is negative however small: `-0.4ns`, which rounds to zero, is
  refused by both, as `-1ns` is.
- Only an `otlp` block with `enabled: false`, which is left unchecked,
  accepts a negative duration, for both. "Unchecked" applies to the whole
  block. Switched off, it may hold an endpoint that is not a URL, and also:
  - a `compression` other than `gzip` or `none`, a `metric_names` other than
    `prometheus` or `otel`, and a `scope` without a name;
  - a negative `max_pending_points`, `unready_after_failures` or
    `batch_max_size`, and an `unready_after_failures` over 1000;
  - a `batch_max_bytes` under its minimum.

  Only a value that is not of its key's type at all (text for a number, a
  list for text) is refused there.
- The longest duration is 2^63 - 1 nanoseconds, about 292 years
  (`2562047h47m16.854775807s`). A longer one, such as `2562048h`, is well
  formed, so the schemas accept it, and the exporter refuses it as not a
  duration.
- A duration of zero is written `0s` or `0`. An unquoted `0` is a number to
  YAML, which the exporter reads as the duration it spells, also with a sign
  (`+0`, `-0`), and the schemas accept the number 0 beside the text. No
  other number is a duration, for either: `timeout: 30` lacks its unit and
  is refused, not read as 30 of some unit.
- A schema receives the number, not how it was written. A zero written
  another way (`0.0`, `00`, `0x0`) therefore passes the schemas as the 0 it
  is, while the exporter, which reads what is written, refuses it as not a
  duration.

These two cases, a duration too long to be represented and a zero that is a
number but is not written `0`, are the only differences between the schemas
and the exporter in how a duration is written.

**Whole numbers.** A whole number, such as `limits.max_metrics: 1e3` or
`retry.attempts: 2.0`, may be written with a point or an exponent. A schema
receives the number, and the exporter reads it by its digits, so
`9007199254740993.0` is that number and not the one a floating-point number
rounds it to.

- The exporter refuses a number beyond what an int64 holds, such as `1e19`,
  which a schema accepts. The error names the key, the number and the range.
- It also refuses one beyond what a floating-point number holds, such as
  `1e400`. The YAML reader the exporter uses takes that for text, while an
  editor, reading YAML 1.2, takes it for a number.
- The exporter refuses a fraction however small, although a schema, which
  receives the floating-point number, takes `1.00000000000000000001` for 1.

**Statuses.** An entry of `request.accept_status`, and one of
`request.retry.statuses`, is a status written as a number or as text.

- A number is the status it equals however it is written (`503.0`, `5.03e2`,
  `0x1F7`, `0o767`, `+503` and `0503` are all 503), for both.
- Text is text: a status's three digits, or a class such as `2xx` or `5XX`,
  as the schemas' pattern has it. `"503.0"`, `"0503"`, `"+503"` and
  `" 503 "` are refused by both, and the exporter's error names the form an
  entry takes.
- The few additional spellings the exporter's reader takes for a number,
  including `5_03` and `0b111110111`, are 503 to the exporter, and text that
  is not a status to an editor, which reads YAML 1.2.

**Empty strings.** An optional key written as the empty string counts as
omitted, for the exporter and the schemas alike.

- Where the key takes one of a set of values or text of a pattern, `""`
  gives its default:
  - `decoder.type: ""` is `auto`, `name_escaping: ""` is `fail`, and
    `request.method: ""` is `GET`;
  - an error policy or a rule's `error_mode` written `""` is its default;
  - a rule's `type: ""` is `gauge` or, under a `prometheus` transform, the
    type of the series that it passes through;
  - `otlp.compression: ""` is `gzip`, and `otlp.metric_names: ""` is
    `prometheus`;
  - `metrics_prefix: ""`, `response.graphite.value: ""`,
    `response.graphite.invalid_lines: ""`, and a static target's `name: ""`
    and `request.method: ""` count as those keys omitted.
- A key that belongs to another request type, such as `rpc`, `descriptors`
  or `method`, is omitted when it is written `""`, and so is not counted in
  what that type refuses.
- A rule about a key depends on whether the key is written. A label has a
  `value` or an `expression`, and the one written `""` is the one it does
  not have. A `prometheus` rule has a `name` or an `expression`, and one
  written `""` is one it does not have.
- Blanks are not the empty string. A key written as only blanks, such as
  `" "`, is text like any other, which the key accepts or refuses as it does
  its other values. A label's `value` of blanks is a constant of blanks. Its
  `expression` of blanks is refused, as are an entry of blanks in
  `transform.include` or `transform.exclude` and a `prometheus` rule's
  `expression` of blanks.
- A required key written `""` is as missing as one left out, and is refused
  by both: `transform.type`, `request.type`, a collector's `name`, a label's
  `name`, a rule's `name` under the transforms that need one, a `grpc`
  collector's `rpc`, a `localfile` collector's `root` and a static target's
  `collector`.
- A value that is not text has no empty form. `""` is not a duration and not
  a size, and both refuse it. Both also refuse an empty entry of
  `collector_files`, `request.accept_status`, `request.retry.statuses`,
  `request.allowed_targets`, `request.denied_targets`,
  `request.redirect_trusted_hosts`, `transform.include`,
  `transform.exclude` and the Python libraries, and an empty key of a
  `value_map`, of `request.metadata` and of a static target's `params` and
  `labels`.

**Values without a value, and kinds of value.**

- A key written with no value at all (nothing after its colon, `null` or
  `~`) is a different case, and no key accepts it. To a schema it is none of
  the types the key takes. The exporter refuses it at every key, every entry
  of a list and every value of a mapping, with an error that names the line,
  so that an editor and the exporter flag the same key.
- A key takes the kinds of value that its schema says, for the exporter too.
  - Free text accepts a number and a boolean as the text that they spell
    (`value: 1`, `description: 404`).
  - A key that the schemas restrict to allowed values or a pattern (a name,
    one of a few words, a host) accepts text only. The exporter refuses a
    number or a boolean there and says to quote it, where it used to read
    `name: true` as the name `true`.
  - A boolean is `true` or `false`. The exporter refuses the `yes` and `on`
    that it used to read as booleans.
- A rule's and a label's `name` are text under every `name_escaping`. A name
  that is a number, which `underscores` and `values` allow, is written in
  quotes: `name: "404"`.
- An editor reads the file as YAML 1.2, and the exporter's reader takes a
  few more spellings for a number, including `1_000` and `0b101`. At a key
  that accepts text only, the exporter refuses those as numbers too. That is
  the only difference between the two there.

A static target has no HTTP response to carry an error. For a static target,
`fail` means the scrape serves nothing except `http_exporter_target_up` at
0, and `log` and `ignore` serve what could be extracted.

Every transform may define `transform.pre_script`. It runs once per scrape,
after decoding and before metric extraction. The script receives the decoded
value as `data` and may mutate it, or replace it by assigning to `data`.

- HTML and XML pre-scripts receive the raw document text, which is parsed
  again after the script, so they must leave a string. A dict or a list
  fails the scrape, with an error that names what the script left.
- A `prometheus` transform's pre-script receives the series as
  `{"metrics": [...]}` and must leave them in that form, which is read back
  into series for the rules; see
  [Python](PYTHON.md#reshaping-a-response-instead-of-writing-a-python-transform).
- Python transforms emit metrics with the `metric(...)` API.

A pre-script **must** leave its result in `data`, because that is the
variable the exporter reads back. A script that computes a value under
another name discards it: the transform then runs against the untouched
response, and the collector appears to extract badly instead of being
misconfigured. The exporter therefore refuses to start when a pre-script
never produces `data`, with an error that names the collector, and rejects
such a configuration on reload, leaving the previous one active.

All of these count as producing `data`: replacing it, mutating it by key or
attribute, augmenting it, binding it as a loop or `with` target or with `:=`
(`(data := ...)`), deleting from it (`del data["noise"]`), and calling a
method that mutates it. So does changing a part of `data` under another
name, as the usual clean-up of rows does, and passing `data` to a function
defined in the script:

```python
for row in data["rows"]:
    row["cpu"] = float(row["cpu"].rstrip("%"))    # row is a part of data

def clean(rows):
    rows.sort(key=lambda row: row["name"])
clean(data["rows"])
```

A copy under another name is not `data`: `rows = list(data["rows"])` and
`other = dict(data)` are new objects, and a script that changes only those
is refused.

Reading `data` does not count, however much of it the script reads. A method
call counts only when the method mutates: `data.update(...)` and
`data["rates"].append(...)` produce `data`, while `data.items()`,
`data.get(...)` and `data.copy()` are reads. This matters because the
mistake usually looks like real work: a script that walks `data` thoroughly
and assigns the result to a different name:

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
exporter serves traffic, not at the first scrape. All faults are listed in
one message. A `python` transform emits through `metric(...)` and is not
required to produce `data`.

The check needs the interpreter from `--python.path`, so a configuration
that contains any Python fails to start if that interpreter is unusable. A
configuration with no Python scripts never invokes one. The interpreter is
started once per check (at startup, for `--dry-run` and at each reload),
whatever it finds and however many [collector files](#collector-files) the
configuration reads.

## Reusing settings with YAML anchors

Collectors of one API often share their request, transform and rule
settings. YAML anchors let you write them once: `&name` marks a value,
`*name` repeats it, and `<<: *name` merges a mapping into another. A
top-level key that begins with `x-` belongs to the file: the exporter
ignores it, whatever it holds. That makes it the place for the shared parts:

```yaml
x-weather-request: &weather_request
  type: http
  path: /v1/forecast
  allowed_schemes: [https]
  retry: {attempts: 1, backoff: 2s}

x-location-labels: &location
  - name: latitude
    expression: .latitude | tostring
  - name: longitude
    expression: .longitude | tostring

collectors:
  - name: weather_current
    request:
      <<: *weather_request
      query: {current: temperature_2m, latitude: "{{param_latitude}}", longitude: "{{param_longitude}}"}
    transform: {type: jq}
    metrics:
      - name: weather_temperature_celsius
        expression: .current.temperature_2m
        labels: *location
  - name: weather_daily
    request:
      <<: *weather_request
      query: {daily: temperature_2m_max, latitude: "{{param_latitude}}", longitude: "{{param_longitude}}"}
    transform: {type: jq}
    metrics:
      - name: weather_daily_max_celsius
        expression: .daily.temperature_2m_max[0]
        labels: *location
```

An anchor can also be placed where it is first used, as the Open-Meteo
example's `labels: &location` is on its first metric. An `x-` key is for
content that no collector uses as it is.

- A key written beside `<<:` replaces that key of the merged mapping
  entirely (`query` above, or a `request` set beside a merged collector); it
  is not merged into it. For the limits, retries and target policies that
  most collectors of a file share,
  [defaults](#defaults-for-the-collectors-of-a-file) apply key by key.
- Anchors also work in collector files and the
  [static target file](STATIC-TARGETS.md#the-target-file), each file
  separately: an anchor in one file cannot be used in another.
- Aliases and merge keys work in every mapping of the three files, including
  a collector's `cache` and a static target's `request`. What a merge brings
  in is read as if it were written out: a key the block does not take is
  refused, and a `path` or `body` merged into a target's `request` replaces
  the collector's.
- A key may itself be an alias of an anchored name: with
  `x-names: [&ttl ttl]`, `cache: {*ttl : 1m}` sets `ttl`, as YAML reads it,
  in those blocks as anywhere. An unknown key written this way is refused by
  the name its anchor holds. An alias of `<<` is the text `<<`, not a merge,
  and therefore an unknown key.
- A key is text, a number, a boolean or null. A key written as a list or a
  mapping (`? [a, b]`), or an alias of one, is refused at its line in all
  three files, even in an `x-` block that nothing uses: `line 3: a mapping
  key is a sequence; only scalar keys (text, numbers, booleans, null) can be
  read`.

An `x-` key is ignored only at the top level. Anywhere else it is an unknown
key like any other, and so is a bare `x-`. With
[environment expansion](#environment-variables) on, a `${NAME}` in an `x-`
block that nothing uses is left alone, so its variable does not have to be
set. One in a block that an alias uses is expanded where the block is used.
The published [schemas](#editor-support) accept top-level `x-` keys too.

## Collector files

A long list of collectors is easier to maintain in several files: one per
team, per application or per ConfigMap key. List them under
`collector_files`:

```yaml
collector_files:
  - shared.yaml             # one file
  - collectors.d/*.yaml     # every file the pattern matches
collectors:                 # optional when collector_files supplies them
  - name: local_status
    ...
```

Each collector file holds a `collectors` list and, optionally,
[`defaults`](#defaults-for-the-collectors-of-a-file) for them, and nothing
else:

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
  directory. An absolute path is used as it is. A path must exist; a pattern
  may match nothing, so an empty directory of collector files is accepted. A
  directory is not a file: write `collectors.d/*.yaml`. A file matched by
  two entries is read once. The configuration file itself is never read as a
  collector file, so `*.yaml` next to it is safe. However, another YAML file
  in the same directory, such as a static target file, would be read and
  refused, so a subdirectory or a naming pattern such as
  `collectors-*.yaml` is better practice. The Helm chart mounts its static
  target file in a separate directory, so in the chart's configuration
  directory `*.yaml` matches only the collector files of `config.data`.
- **Only collectors.** Any key in a collector file other than `collectors`
  and `defaults` (`web`, `otlp`, a misspelt `colectors`, or a nested
  `collector_files`) is an error that names the file, the key and its line.
  The exporter-wide settings belong to the configuration alone, so a file of
  collectors can never change them. A file that is empty or has an empty
  `collectors` list is an error too.
- **Order.** The configuration's own collectors come first, then each entry's
  files in the order listed, with a pattern's matches in file name order.
- **Validated together.** The merged collectors are validated exactly as if
  they had been written in the configuration: defaults, expressions, Python
  scripts, and everything in
  [Checked when the configuration loads](#checked-when-the-configuration-loads).
  A mistake found in a collector names the collector file that defines it,
  in the error and, when it rejects a reload, as the log line's `file`. The
  same applies to a fault the interpreter finds in the collector's Python,
  as in `collector file /etc/exporter/collectors.d/payments.yaml: collector
  pay pre_script has a Python syntax error on line 3: invalid syntax`. A
  configuration needs at least one collector across all files; with
  `collector_files`, its own `collectors` key may be omitted.
- **Unique names.** A collector name must be unique across the configuration
  and every collector file; see [Collectors](#collectors).
- **Environment variables.** With `--config.expand-env`, `${NAME}` references
  are expanded in collector files as in the configuration.
- **Reloading.** With `--config.watch`, editing, adding or removing a
  collector file reloads the configuration, even though the configuration
  file itself did not change. The files watched are those the configuration
  file lists, even while it is refused: a configuration that adds a
  collector file with a mistake in it reloads once that file is fixed,
  without another change to the configuration. Starting the watch reloads
  nothing. A reload that would break any rule above is rejected, and the
  last valid configuration stays active.
- **Checking.** `--dry-run` reads the collector files too and lists them under
  `details.collector_files` of its `config` entry.
- **Defaults.** A collector file's `defaults` apply to its own collectors,
  and the configuration's to the configuration's; see
  [Defaults for the collectors of a file](#defaults-for-the-collectors-of-a-file).

`prometheus-universal-exporter --config.collector-file-schema` prints the JSON
Schema of a collector file, published as
[`configs/collector-file.schema.json`](../configs/collector-file.schema.json). Start a collector
file with this line:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/eenchev/prometheus-universal-exporter/main/configs/collector-file.schema.json
```

The editor then checks it the way it checks the configuration, including
that it has no key other than `collectors` and `defaults`.

## Defaults for the collectors of a file

A file of many similar collectors repeats the same limits, retries and
target policies in each. A top-level `defaults` block states them once:
every collector of the same file takes each key of the block that it does
not set itself. The configuration file and each
[collector file](#collector-files) have their own block, which applies to
the collectors of that file only. The configuration's block does not reach
the collectors of a collector file, or the other way round.

```yaml
# collectors.d/payments.yaml
defaults:
  request:
    allowed_targets: ["*.payments.example"]
    retry: {attempts: 2, backoff: 1s}
  limits: {max_metrics: 2000, max_label_value_length: 200}
  error_handling: {on_fetch_error: log}
  failure_backoff: {initial: 1m}
collectors:
  - name: payments_api
    request: {type: http, path: /status}
    transform: {type: jq}
    metrics:
      - name: payments_queue_depth
        expression: .queue.depth
  - name: payments_ledger
    request:
      type: http
      path: /ledger/status
      retry: {attempts: 5}          # backoff 1s still comes from defaults
    limits: {max_metrics: 10000}    # max_label_value_length 200 still applies
    transform: {type: jq}
    metrics:
      - name: payments_ledger_lag_seconds
        expression: .lag
```

- **The keys.** `defaults` sets the keys of `request.tls`, `request.retry`,
  `request.max_response_bytes`, `request.allowed_targets`,
  `request.denied_targets`, `request.proxy_url`,
  `request.proxy_basic_auth_file`, `limits`, `error_handling`,
  `failure_backoff` and `cache`, each written as a collector writes it.
  These are settings that are the same for collectors of different request
  types and decoders or, for the proxy, that depend on where the exporter
  runs. Any other key, whether a collector's (`metrics`, `request.path`,
  `request.type`, a key specific to one request type) or unknown, is refused
  when the configuration loads, with an error that names the key and its
  line. So is a value of the wrong kind.
- **Proxy credentials.** The `proxy_basic_auth_file` of a block that also
  sets `proxy_url` is for that proxy only: a collector that writes its own
  `proxy_url`, another proxy or `direct`, does not take it. Without a
  `proxy_url` in the block, the credentials are for the collectors that
  name their own proxies, and a collector that writes `proxy_url: direct`
  does not take them
  ([A collector's own proxy](REQUESTS.md#a-collectors-own-proxy)). A
  collector that sends its requests to a
  [`unix_socket`](REQUESTS.md#over-a-unix-socket) takes neither.
- **Key by key.** A collector takes every key of the block that it does not
  write itself, at every depth. One that writes `limits.max_metrics` keeps
  the block's other limits, and one that writes `retry.attempts` keeps the
  block's `retry.backoff`.
  - A key the collector writes is its own whatever its value, so
    `cache: {ttl: 0s}` answers every probe of that collector from the
    target. A key written `""` counts as omitted, as everywhere, and is
    taken from the block.
  - A list is one key and is replaced as a whole: a collector's own
    `allowed_targets` is the list it is held to, never combined with the
    block's.
  - A `request.tls` or `failure_backoff` written in `defaults`, even empty,
    turns TLS, or the backoff with its own defaults, on for every collector
    that does not write it. A collector that must never back off writes
    `failure_backoff: {enabled: false}` and takes none of the block's
    `failure_backoff` keys. `enabled: false` in `defaults` itself, which
    would give every collector the same as leaving `failure_backoff` out,
    is refused.
  - A key merged in with `<<:` counts as written where the merge puts it, in
    a collector and in `defaults` alike.
- **Request types.** A key of `request` applies only to the collectors whose
  request type takes it, so collectors of several types can share one file:
  `retry` does not reach a `dns` collector, nor `retry.statuses` a `grpc`
  one. A key that applies to no collector of the file (none of their types
  takes it, or each that does sets it itself or turns the backoff off) would
  be a setting that nothing uses. It is refused, once for a block and its
  keys, as `defaults.request.retry.attempts applies to no collector of
  the file: request.retry.attempts does not apply to request.type "dns" of
  its collectors; remove it from defaults`. So is a `defaults` block in a
  configuration file that defines no collectors of its own.
- **Checked once, and with each collector.**
  - A value that is wrong whatever collector takes it (a negative limit, an
    unknown error policy, more than 10 retry attempts) is refused once, with
    an error that names the block, as `defaults.limits.max_metrics is -1,
    and a limit must not be negative; leave it out, or 0, for the default`,
    preceded by the collector file when it is in one.
  - A value that is wrong for one collector, such as an allowed target that
    is not a bucket name for an `s3file` collector, or a
    `failure_backoff.max` shorter than the `initial` the block gives, is
    refused with that collector. The error ends by saying where the value is
    written: `(failure_backoff.initial comes from the defaults of the
    file)`. A warning about such a value says the same; it is logged at load
    and listed in the [dry run](#dry-run), like the warning that a `POST`
    collector's `retry.attempts` makes no retries.
  - The problems with the keys of the block itself are reported together, in
    file order. While the block is refused, the collectors of its file are
    not checked, because without the values it was meant to give them they
    would be refused for the wrong reasons.
- **The collectors are what count.** A collector is validated, compared on a
  reload and keyed in the [response cache](#response-caching) with the
  values it took, as if it had written them. Moving a value from every
  collector into `defaults` changes no collector. Changing a default changes
  each collector that takes it, which is what
  [What a reload changed](#what-a-reload-changed) lists and what drops their
  cached results.
- **Everything else as before.**
  [Environment variables](#environment-variables) are expanded in `defaults`
  as anywhere in the file. The [target allocator](ALLOCATOR.md) carries the
  files as written, including `defaults`. A
  [static target](STATIC-TARGETS.md) scrapes through the collector with the
  defaults it took. The [debug page](#the-debug-page), which reads one
  collector, accepts no `defaults` key. The [dry run](#dry-run) lists under
  `details.collector_defaults`, for each collector that took any, the keys
  it took. Both [schemas](#editor-support) accept `defaults` with these keys
  only.

## Environment variables

A configuration file is usually committed to version control, and some of
what belongs in it is not: an internal hostname, a tenant identifier, a
token. With `--config.expand-env`, the exporter substitutes `${NAME}`
references from its own environment before it parses the document:

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
  prometheus-universal-exporter --config.file=config.yaml --config.expand-env
```

Expansion is off by default, deliberately. A configuration is full of dollar
signs that are not references (a regex metric rule, a jq expression, a
Python pre-script), and expanding them by default would silently rewrite the
operator's own text. With the flag off, everything in the file means exactly
what it says.

Even with the flag on, only the braced form is a reference. `$VAR` is left
exactly as written, so `expression: '\$([0-9]+)'` and shell-style text in a
pre-script keep working. Write `$$` for a literal dollar: `$${NOT_A_REFERENCE}`
is kept as `${NOT_A_REFERENCE}`.

A reference to a variable that is not set is a startup error, not an empty
string:

```text
config.yaml: environment variable "API_TOKEN" not set; --config.expand-env
requires every ${NAME} it finds to be defined
```

An empty substitution would produce a document that parses and is wrong,
such as a collector with no path or credentials that are silently blank,
and the exporter would serve it. Every missing name is listed at once.

A variable that is set to an empty string is a deliberate choice and is
substituted normally. As the whole of a value, `X-A: ${A}`, it is the value
written `""`, which loads or is refused exactly as `X-A: ""` would be, not a
key with nothing after its colon.

**Where references are expanded.** A reference is expanded where the
document holds it as a value: a value or a key, quoted or not, whole or part
of a longer one. The variable is always exactly that value.

- A token that contains ` #`, or starts with `*`, `&` or `[`, and quotes,
  backslashes and line breaks all arrive as written. The value is written
  back quoted where YAML would otherwise read it differently, and left
  unquoted where it reads the same, so a number stays a number.
- A boolean stays a boolean too. At a key that accepts text only, such as a
  `name`, write the reference in quotes, `name: "${NAME}"`. Otherwise a
  variable set to `true` or `1` is
  [refused](#checked-when-the-configuration-loads) as the boolean or the
  number it then is.
- A reference in a comment is left alone, whether the variable is set or
  not.
- All of this applies in a flow collection too, where a bare reference is
  not valid YAML until it is expanded:
  - alone, as in `regions: [${PRIMARY}, ${SECONDARY}]` or `{token: ${TOKEN}}`;
  - as part of a longer value, as in `{Authorization: Bearer ${TOKEN}}`;
  - beside quoted values, as in `{X-Name: "x", X-Token: ${TOKEN}}`.

  Reading such a file changes no other value in it. A reference in a block
  value, or after a comma in an unquoted description, expands to the
  variable's value exactly as in a file without a flow collection.
- It also applies after an anchor, as in `&base ${BASE}`, and for values
  that YAML reads specially on their own, such as `-` or an empty string: a
  key is always written quoted, and so are an empty value and `-`.
- A variable supplies a value and never structure: `headers: ${ALL_HEADERS}`
  is one string, not a mapping.
- A block value (`|` or `>`) is expanded in every line, also when its header
  specifies the indentation, as in `|2`.
- Two places cannot take every value. A block value takes only a value
  without a line break, and an unquoted value folded over several lines is
  refused; quote the reference there, as in `"${NAME}"`.
- A literal `$${NAME}` in a flow collection must be quoted, because its
  braces are not valid YAML there.
- Errors name the line numbers of the file as you wrote it.

`--config.watch` expands again on every reload, so a reload cannot silently
replace a working configuration with literal references.

The static target document has its own flag, `--static-targets.expand-env`,
which works the same way. `--config.expand-env` does not apply to it, so
either file can take values from the environment while the other is used as
written. See [Static targets](STATIC-TARGETS.md#environment-variables).

Environment references are fixed when the file is read. For a value that
changes per scrape, such as a tenant in the URL path, use a
[path parameter](REQUESTS.md#path-parameters), `{{param_tenant}}`, which the
probe fills in. The two syntaxes never overlap, and a path parameter's
default can itself be an environment reference.

## OpenMetrics

`/probe`, `/self-metrics` and the static targets endpoint answer in
[OpenMetrics](https://github.com/prometheus/OpenMetrics/blob/main/specification/OpenMetrics.md)
1.0.0 when the scraper's `Accept` header prefers it, and otherwise in the
Prometheus text format, version 0.0.4. Prometheus 2 and 3 ask for OpenMetrics
first, so they get it. `curl`, a browser and any client that does not ask
get the text format, as before. No configuration is needed. The answer's
`Content-Type` identifies the format, and the answer carries `Vary: Accept`.

In both formats, the series of a metric are written together under one
`HELP` and `TYPE`. The metrics appear in the order in which each first
appears, and a metric's series in the order in which they were produced (for
a `csv` rule, the order of the rows). Nothing is sorted. This holds whatever
order the series arrive in, for example with two rules of one name with
another rule between them, a script that emits a series of each metric row
after row, or a `prometheus` collector whose rules or `rename` give two of a
target's metrics one name. Each metric's series are answered as one group,
as the text format requires.

The two formats hold the same series, written as each requires, with one
difference in names: the samples of an OpenMetrics counter always end in
`_total`. A counter named `jobs_done_total` is the series `jobs_done_total`
in both, but one named `jobs_done` becomes `jobs_done_total` in OpenMetrics.
Name counters with `_total`, as Prometheus' naming conventions recommend,
and the names are the same whichever format a scraper requests; all of the
exporter's own counters follow this. In OpenMetrics, `untyped` is written as
`unknown`, timestamps are in seconds, `le` and `quantile` values are written
as floats (`1.0`), and the answer ends with `# EOF`. Prometheus reads both
formats the same way.

**Name clashes.** OpenMetrics forbids two families from claiming one name,
which the text format allows. A counter `foo_total` is the OpenMetrics
family `foo`, so beside a gauge `foo` the name would be used twice and
Prometheus would fail the scrape. The same applies to counters `jobs` and
`jobs_total`, and to a histogram `h` beside a gauge `h_count` (the
histogram's own `h_count` sample).

Families that would clash in this way are written as OpenMetrics `unknown`
families under exactly the names their samples have in the text format,
whichever comes first. The counter `foo_total` stays the series
`foo_total`, only without its counter type in OpenMetrics, and the gauge
`foo` stays a gauge. A counter gives way before a histogram or summary does.
A histogram or summary that still clashes is written as the `unknown`
families `h_bucket`, `h_sum` and `h_count` (for a summary, `s`, `s_sum` and
`s_count`). Every series is the same in both formats. Distinct names avoid
all of this and keep the types.

Two kinds of family have no OpenMetrics form of their own type, and are
written so that the answer stays valid:

- A counter named exactly `_total` would be a family with no name. It is
  written as the `unknown` family `_total`.
- A histogram that the target wrote with a `_sum` and no `_count`, or the
  reverse (see [Prometheus input](#character-encodings)), is not an
  OpenMetrics histogram, which has both or neither. It is written as
  `unknown` families `h_bucket` and `h_sum` (or `h_count`), with the same
  series as in the text format.

A counter whose name was escaped by [`name_escaping: values`](#utf-8-names)
is written like any counter, and as Prometheus writes one. Its family is the
escaped name without a trailing `_total`, and its sample is the family with
`_total`, as plain text in both places. `my.errors`, exported as
`U__my_2e_errors`, is the family `U__my_2e_errors` with the sample
`U__my_2e_errors_total`. The escaping doubles every underscore, so
`my.requests_total` is exported as `U__my_2e_requests__total`, which is the
family `U__my_2e_requests_` with that same sample: the family keeps one of
the two underscores. A sample is always its family plus `_total`, which is
what lets an OpenMetrics reader (Prometheus, and this exporter's own
`prometheus` decoder) find the counter's sample in its family.

**Values OpenMetrics does not allow.** The text format lets a target write
values that OpenMetrics does not allow for a family of its type, and a strict
OpenMetrics parser refuses a whole answer because of one of them. The
exporter passes such values on in both formats: a negative counter that a
target wrote is neither refused nor left out. It keeps the OpenMetrics answer
valid in the same way as above. The family is written as `unknown` families
under its sample names, with the same series and values as in the text
format, and loses only a type that it does not conform to. A family has one
type, so the whole family is written this way when one of its series is:

- a counter that is `NaN` or negative (`+Inf` is a valid counter value);
- a histogram:
  - whose `_sum` is `NaN` or negative;
  - with a `_sum` and a bucket with a negative bound, which OpenMetrics
    allows only without a sum;
  - with a bound that is not a number, `le="NaN"`;
  - whose bucket counts decrease from one bound to the next higher, or whose
    highest bucket holds more than its `+Inf` bucket;
  - whose `+Inf` bucket and `_count` differ, as a target scraped between two
    of its updates writes them;
  - or with neither of the two (see [Prometheus input](#character-encodings));
- a summary whose `_sum` is `NaN` or negative; one with a `quantile` outside
  0 to 1, such as percentiles written as `50` and `99`; or one with a negative
  value for a quantile. `NaN`, which a quantile is while nothing has been
  observed, is allowed.

A gauge and an untyped series may have any value. A target's counter
`jobs_total -5` is therefore `# TYPE jobs_total unknown` and `jobs_total -5`
in OpenMetrics, and a histogram `h` whose counts decrease is the `unknown`
families `h_bucket`, `h_sum` and `h_count`. A family whose values its type
allows always keeps the type. [OTLP export](OTLP.md#delivery) follows the
same rule, with gauges.

Order is not one of these cases. A histogram's buckets are written in
ascending order of `le`, with `+Inf` last, and a summary's quantiles in
ascending order, in both formats, whatever order the target wrote them in.

**Created timestamps.** OpenMetrics can give a counter, a histogram and a
summary a `_created` time, the time when it started counting. The exporter
writes none for what it reads from targets, on `/probe` and the static
targets endpoint: it cannot know when a target's counter started, and
OpenMetrics omits `_created` when it is not known. The exporter does know
the start time of its own counters, and writes it on the self-metrics
endpoint when `web.self_metrics.created_timestamps` is set; see
[Created timestamps](SELF-METRICS.md#created-timestamps).

To keep a Prometheus job on the text format, set its `scrape_protocols`:

```yaml
scrape_configs:
  - job_name: universal-exporter
    scrape_protocols: [PrometheusText0.0.4]
```

A Prometheus that asks for its protobuf format first, as it does when it
scrapes native histograms, gets OpenMetrics from `/probe` and the static
targets endpoint, which do not write protobuf. It gets protobuf from the
self-metrics endpoint, whose scrape-time histograms then also carry native
buckets; see [Native histograms](SELF-METRICS.md#native-histograms).

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
answered from memory, and the target is not contacted again. Caching is off
by default. Omitting `cache` or setting `0s` disables it, and a negative
value is rejected at startup. `cache` is always a mapping: `cache: 60s` is
refused, with a message that shows the form to write instead.

The cache is in memory and local to the exporter process. Nothing is written
to disk, replicas do not share entries, and a restart empties it.

**The cache key.** A stored result is only ever returned to an identical
request. The key covers:

- the collector's name and its full effective configuration;
- the `target`;
- every `/probe` parameter the collector's request type accepts (`method`,
  `path`, `timeout`, `body`, `insecure_skip_verify`, `follow_redirects`,
  `enable_http2`, `new_connection`, `ip_family`, `retry_attempts`,
  `retry_backoff`, `message`, `from`, `until` and every `param_<name>`,
  whether it fills the request or only a
  [label value](REQUESTS.md#in-label-values));
- every header forwarded to the target, including a forwarded
  `Authorization` value and the `header_<name>` parameters that are
  forwarded.

Further rules:

- A parameter that changes nothing in what is sent is not part of the key:
  one that no request type knows, which a probe ignores, and a
  `header_<name>` for a header the collector does not forward. Probes that
  differ only in those share an entry, so a caller adding `&x=1`, `&x=2`,
  ... cannot fill the cache with copies of one result.
- The parameters are keyed as the probe reads them, not as they were
  written. `method=get` and `method=GET`, `timeout=5s` and `timeout=5000ms`,
  `insecure_skip_verify=TRUE` and `true` are one request and one entry. A
  parameter [given twice](#request-types) is refused, not keyed.
- Presence and absence differ. A probe that sends no credential, no
  forwarded header or no TLS override cannot read an entry stored by a probe
  that sent one, and two probes with different credentials never share an
  entry.
- The collector's definition is part of the key, so a configuration reload
  retires the entries cached under the previous definition.
- Credentials loaded from files are covered through their configured paths.
  A rotated credential file therefore applies to a cached request once the
  entry expires. Keep `cache` shorter than the rotation interval where that
  matters.

Only fully successful probes are cached; HTTP, decode, transform and
validation failures are not. `limits.max_cache_entries` bounds each
collector's live entries and defaults to 1000. When the limit is reached,
expired entries are dropped first, then the ones closest to expiry. An
expired entry kept to be [revalidated](#revalidating-expired-responses)
counts towards the limit too.

### Serving the last good result when the target fails

A target that is briefly unavailable (restarting, overloaded, behind an
unreliable network) fails its probes. Every series of the collector then
disappears from Prometheus for as long as the outage lasts, which breaks
graphs and fires absent-series alerts. `stale_if_error` keeps each result
for that much longer after `ttl`, and answers a probe whose trip fails with
the last successful result instead of the error:

```yaml
    cache:
      ttl: 30s
      stale_if_error: 5m
```

Within 30 seconds of a successful probe, repeats are answered from memory,
as described above. After that, the target is asked again. If it answers,
the new result is served and stored. If the trip fails and the last good
result of the same probe is less than 5m30s old, that result is answered
with `200`. A trip fails when the target is down, times out or answers an
error status, when its response cannot be decoded or transformed, when the
collector is at `max_concurrent_probes`, or when the collector is
[backing off](#backing-off-from-failing-targets) from the target. After
5m30s, the failure is answered as usual. `ttl` may be `0s`: every probe then
goes to the target, and the last good result is kept only as a fallback.

Two kinds of failure are never answered with a stale result:

- **The target refuses the credential it was sent**: HTTP `401` or `403`, or
  gRPC `UNAUTHENTICATED` or `PERMISSION_DENIED`. A credential forwarded with
  `forward_authorization` is part of the cache key, but a token that has
  just been revoked would otherwise still be answered with the earlier
  result for as long as `stale_if_error` lasts. The probes that
  [`failure_backoff`](#backing-off-from-failing-targets) keeps from the
  target after such a refusal are not answered stale either, because they
  stand for that refusal.
- **The collector's own
  [`allowed_targets` or `denied_targets`](REQUESTS.md#restricting-targets)
  refuse the target**, answered `403`. The target's name now resolves, or a
  redirect now leads, to somewhere the collector may not go, and the refusal
  is the answer.

Such a probe fails as usual, and a static target's scrape publishes no stale
result. A fresh result within `ttl` is still answered from the cache without
asking the target, like any cached result.

A stale answer must never be mistaken for a fresh one. While
`stale_if_error` is set, every answer of the collector therefore carries two
series of the exporter's own:

```text
http_exporter_result_stale 0
http_exporter_result_age_seconds 0
```

- `http_exporter_result_stale` is `1` when the answer is the last good
  result standing in for a failed trip.
- `http_exporter_result_age_seconds` is how long ago the answered result was
  fetched from the target: `0` for a trip just made, and the entry's age for
  a cached answer. On the
  [static targets endpoint](STATIC-TARGETS.md), which serves each target's
  last result until its next scrape, the age is calculated at every read, so
  it shows how old the data is when Prometheus reads it. A target scraped
  every 10 minutes reads from `0` up to about `600` before its next scrape.

Watch these series instead of `up`, which stays `1` while stale results are
answered. `http_exporter_result_stale == 1` selects the targets that are
currently bridged by an old result.

The failure is still logged and counted in the self-metrics as it would have
been. A warning says that the last good result was answered and how old it
is, and `http_exporter_cache_stale_served_total` counts the stale answers. A
stale answer does not count in `http_exporter_scrape_success_total`. A
collector's rules must not produce either series name while `stale_if_error`
is set.

Samples are served without timestamps, so Prometheus stores the old values
at the time of each scrape: a counter stays flat, and a gauge repeats its
last value. Limit `stale_if_error` to outages you would rather bridge than
see: minutes, not hours. A changed collector definition never serves a
result stored under the old one. A [static target](STATIC-TARGETS.md) whose
scrape fails serves the last good result in the same way, marked stale,
while its `http_exporter_target_up` stays `0`.

Cache activity is visible per collector in the self-metrics as
`http_exporter_cache_hits_total`, `http_exporter_cache_misses_total`,
`http_exporter_cache_stale_served_total` and `http_exporter_cache_entries`,
which counts stale entries too. A cache hit counts as a successful scrape,
and cached metrics are still queued for OTLP export.
`http_exporter_scrape_http_status_code` and
`http_exporter_scrape_response_bytes` continue to describe the last real
request to the target.

### Revalidating expired responses

The cache of an `http` or `graphite` collector also keeps, with each result,
the response it was produced from, when that response carried an `ETag` or
a `Last-Modified` header. Once the result is no longer fresh, the next trip
of the same probe asks the target whether the response is still current, as
RFC 9110 describes. It sends `If-None-Match` with the `ETag` exactly as
received, weak (`W/"..."`) or strong, and `If-Modified-Since` with the
`Last-Modified` exactly as received.

- A `304 Not Modified` answer means that the kept response is still current.
  Its body is used again, and the kept headers are updated from the 304's,
  as RFC 9111 § 4.3.4 specifies.
  - Each header that the 304 sends, such as `ETag`, `Last-Modified`,
    `Cache-Control`, `Expires` or `Date`, replaces the kept one. The
    exceptions are `Content-Length`, `Content-Encoding`,
    `Transfer-Encoding`, `Content-Type`, `Content-Range` and the connection's
    headers, which describe a body that the 304 does not have.
  - The trip then continues as if the response had just been downloaded. It
    is decoded and transformed again, with every rule seeing the original
    status (`$status` is `200`, not `304`) and the updated headers, and the
    new result is stored, fresh for `ttl`.
  - Only the download is saved; the series are produced anew on every trip.
- Any other answer is handled like any trip's answer: a new response that
  the collector accepts replaces the kept one, and a failing status fails
  the trip.

A 304 is never a failed status: it stands for the kept response, whose
status the collector accepted. A 304 to a request that was not conditional
on a kept response has nothing to stand for, and fails the probe as
`received HTTP status 304 Not Modified to a request that was not conditional
on a cached response`, unless `request.accept_status` accepts `304`, as
`[304]`, `["3xx"]` and `[any]` do. A request that the collector makes
conditional itself, with `If-None-Match` or `If-Modified-Since` in
`request.headers` or forwarded, is sent as configured, and its answer is not
treated as a revalidation.

An expired entry whose response has a validator is not dropped when its
`ttl` and `stale_if_error` have run out. It answers no probe, but stays for
the next trip to revalidate, until a new result replaces it, a reload
changes or removes the collector, or `limits.max_cache_entries` needs its
place, in which case it is the first to go. It counts in
`http_exporter_cache_entries`. An entry without a validator is dropped when
it expires, as before. The kept response holds the body as downloaded, up to
the collector's response limit, so each kept entry costs about the size of
its response in addition to its series.

Revalidation and
[`stale_if_error`](#serving-the-last-good-result-when-the-target-fails) work
together, in this order:

1. A fresh result answers without a trip.
2. After `ttl`, the trip is conditional when the kept response has a
   validator.
3. A 304 or a new response answers with a fresh result.
4. A trip that fails (the conditional request answered `500`, or timed out)
   is answered with the stale result while it is within `stale_if_error`,
   and with the failure after that, even though the entry is still kept for
   the next revalidation.

Only a response that a single validator represents is revalidated:

- It must have been fetched with `GET` or `HEAD`. That is the method of
  every `graphite` request except one too long for a URL, which is sent as a
  form with `POST`. A matching `If-None-Match` on another method asks the
  target for `412 Precondition Failed`, not a 304.
- A collector with [`request.pagination`](REQUESTS.md#pagination) stores its
  pages merged into one response, which no single page's validator
  represents, so its responses are neither kept nor revalidated.

Every attempt of a conditional request carries the validators: a retry, and
the request sent again after a `401`, or a status in `relogin_on`, made
[`request.login`](AUTHENTICATION.md#logging-in-to-a-target) log in again.

Revalidation is on by default. A collector whose target sends validators it
does not honour can turn it off:

```yaml
    cache:
      ttl: 30s
      revalidate: false
```

`http_exporter_cache_revalidations_total{collector,result}` counts the trips
that revalidated a kept response, with `result="not_modified"` for a 304 and
`result="modified"` for a new response the collector accepted.
`http_exporter_scrape_http_status_code` shows `304` and
`http_exporter_scrape_response_bytes` `0` after a 304, since they describe
what the target sent. A [debug probe](#debug-probes) sends the conditional
request the same probe would, and its report shows `If-None-Match` and
`If-Modified-Since` among the request's headers and the outcome as
`304 Not Modified; the cached body of <n> bytes was reused, with status 200`;
it refreshes nothing, so the entry stays as it was.

## Identical probes share one request

Several Prometheus replicas that scrape the same targets on the same
interval tend to probe at the same moment. When a probe arrives while an
identical one is already waiting on the target, it does not send its own
request. It waits for the request in flight and gets an exact copy of its
answer: the metrics, or the same error. A slow endpoint is then asked once
instead of once per replica, and a rate-limited one is not pushed over its
limit.

"Identical" means the same as for the response cache: the same collector
definition, target, probe parameters and forwarded headers, including
credentials. Two probes that could get different answers therefore never
share a request, and probes that differ only in parameters that change
nothing in what is sent still do. A `param_<name>` that fills a
[label value](REQUESTS.md#in-label-values) is a probe parameter like the
others: two probes that differ in it get different series and never share.

Sharing needs no cache. The cache helps the probes that arrive after one has
finished, and sharing helps the ones that arrive while it is still running.
With a cache, the probes that share a request fill the cache once.

The shared request belongs to no single probe. If the probe that started it
goes away, for example because its Prometheus timed out, the others still
get their answer. The request is cancelled only when every probe waiting on
it has gone.

The trip to the target is counted once in the self-metrics, however many
probes shared it. Every probe still counts in `http_exporter_scrapes_total`
and `http_exporter_scrape_success_total`, and each one that shared another's
request also counts in `http_exporter_probes_coalesced_total`. A failure is
logged once.

Sharing is on by default. A collector whose target must see every probe as a
separate request can turn it off:

```yaml
collectors:
  - name: counts_every_call
    coalesce: false
```

## Limiting concurrent probes

Identical probes share one request, but probes of different targets, or with
different parameters, each make their own, and all of a collector's targets
are often one backend. `max_concurrent_probes` limits how many trips to its
targets a collector makes at once:

```yaml
collectors:
  - name: inventory_api
    max_concurrent_probes: 8   # optional; 32 when omitted or 0
```

A trip is the request or file read, with the decoding and transforms that
follow it. A probe that would exceed the limit is not queued. It is answered
immediately with `503 Service Unavailable` (`collector inventory_api already
has 8 trips to its targets in progress, its max_concurrent_probes; this
probe was not sent`) and counted in `http_exporter_probes_rejected_total`.
`http_exporter_probes_in_flight` shows how close to the limit a collector
runs. Prometheus records the rejected scrape as `up` 0 with that reason, not
as a timeout.

- Probes that make no trip take no slot: one answered from the
  [response cache](#response-caching), one that
  [shares a request](#identical-probes-share-one-request) already in flight,
  and one [kept from a failing target](#backing-off-from-failing-targets).
- A [static target](STATIC-TARGETS.md) shares its collector's limit, but
  waits for a free slot within its scrape budget instead of failing
  immediately, because nothing is waiting on its answer. If no slot frees up
  in time, the scrape fails in the `concurrency` stage.
- The default, 32, is well above what one Prometheus usually sends a single
  backend at once. Lower it for a backend that cannot take many requests at
  a time, and raise it for a collector with many slow targets. A negative
  value is a configuration error.

Per-collector limits leave the process as a whole unbounded: twenty
collectors with a limit of 32 could hold 640 responses, and their series, in
memory at once. `--probe.max-concurrent` limits the trips of all collectors
together, on top of each collector's own limit:

```sh
prometheus-universal-exporter --probe.max-concurrent=64
```

A probe over that limit is answered `503` in the same way, with a message
that names the flag (`the exporter already has 64 trips to targets in
progress, its --probe.max-concurrent; this probe was not sent`). It is
counted in `http_exporter_probes_rejected_exporter_limit_total`, separately
from the probes that its collector's own limit turned away. A static target
scrape waits for a slot. `http_exporter_trips_in_flight` shows how close to
the limit the exporter runs.

Static target scrapes that wait for a slot wait in line. A slot that frees
up goes to the first scrape in line that it can serve, that is, one whose
collector is under its own limit while the exporter is under
`--probe.max-concurrent`. A scrape waiting behind its own full collector
therefore never holds up another collector's. `0`, the default, leaves only
the per-collector limits, and a negative value is a command-line error. Size
the limit to the memory the exporter has, together with
`--python.max-workers` (see [Python](PYTHON.md#how-scripts-run)).

## Backing off from failing targets

A target that is down fails every scrape, and every scrape still goes to it.
A target that does not answer at all holds a trip slot (of
`max_concurrent_probes` and of `--probe.max-concurrent`) until its timeout,
on every scrape. A few dead endpoints can therefore keep the slots full and
turn away the probes of targets that do answer. `failure_backoff` stops
contacting a target that keeps failing, for a while:

```yaml
collectors:
  - name: inventory_api
    failure_backoff:
      after: 3      # consecutive failed scrapes before backing off; at least 1, 3 when omitted
      initial: 30s  # the first wait; 30s when omitted
      max: 10m      # the longest wait; 10m, or initial when that is longer, when omitted
      min_budget: 1s # the least budget a timed-out scrape must have had to count; 1s when omitted
```

After `after` consecutive failed scrapes of a target, its probes and
[static target](STATIC-TARGETS.md) scrapes fail immediately, without
contacting it, for `initial`. When the wait has passed, the next scrape goes
to the target. If it fails, the next wait is twice the last, up to `max`. If
it succeeds, the target is forgotten and every scrape goes to it again.

Only that one scrape goes to the target. While it is in progress, a probe
identical to it [shares it](#identical-probes-share-one-request), and any
other scrape of the target, such as a probe of a collector with
`coalesce: false`, fails immediately, as during the wait. Without
`failure_backoff`, every scrape contacts the target. These are refused at
load: `after` below 1, an `initial` or `min_budget` that is not a positive
duration, and a `max` shorter than `initial`.

`failure_backoff: {enabled: false}` turns the backoff off: every scrape
contacts the target, as without the block. This is how a collector turns off
the backoff that its file's
[defaults](#defaults-for-the-collectors-of-a-file) give it. In a collector
without defaults it is equivalent to omitting the block, and a reload and
the response cache treat it as such. Keys written beside it are still
checked. `enabled` defaults to true.

**What counts as a failed scrape.** A failed scrape is one whose fetch
failed: the connection was refused or reset, it timed out, TLS failed, the
target answered a status the collector does not
[accept](REQUESTS.md#accepting-other-statuses), or the request type's own
fetch failed, such as a file that is not there or a query a database
refused. A target that answered something the decoder or the transform could
not read is up, and counts as a success.

These scrapes count as neither, because they say nothing about the target:
one that [`allowed_targets` or `denied_targets`](REQUESTS.md#restricting-targets)
refused, one that `max_concurrent_probes` or `--probe.max-concurrent` turned
away before it started, and one cancelled because every probe waiting for it
went away.

**The budget floor.** A scrape that timed out (ran out of its budget) does
not count either when that budget was below `min_budget`. Such a scrape
neither counts as a failure nor resets the failures before it.

- A probe's budget comes from its caller: Prometheus's scrape timeout, less
  `--probe.timeout-offset` ([Probe deadlines](#probe-deadlines)). The budget
  is not part of what identifies a target. Without the floor, anyone who can
  reach `/probe` could send a few probes that ask for less time than the
  target needs to answer, and make it back off for every probe and static
  target scrape that shares its state.
- The budgets Prometheus gives are above the floor, so a target that does
  not answer at all still backs off. Raise `min_budget` towards the scrape
  timeout to be stricter about what counts.
- A static target's scrape has its interval as its budget, and the floor
  applies to it in the same way.
- A failure that is not caused by the deadline (a refused connection, TLS, a
  status the collector does not accept, the target's own error) counts
  whatever the budget. So does a timeout of the probe's `timeout` parameter,
  which is part of the key.

A scrape kept from the target fails as its fetch would have, in the fetch
stage, with:

```text
http://10.0.0.7:8080/stats is backing off after 3 consecutive failed scrapes; next try after 2026-10-23T09:14:05Z
```

Everything that applies to a failed fetch therefore applies to it:

- A probe is answered `502` under `error_handling.on_fetch_error: fail`, or
  carries on under `log` and `ignore`
  ([When a stage of the probe fails](#when-a-stage-of-the-probe-fails)), and
  [`cache.stale_if_error`](#serving-the-last-good-result-when-the-target-fails)
  answers it with the last good result.
- A static target's scrape is `up` 0, or serves its last good result marked
  stale.
- Such a scrape takes no trip slot, sends nothing, and leaves
  `http_exporter_scrape_http_status_code` and
  `http_exporter_scrape_response_bytes` describing the last real request.
- Its failure is logged like any failure, and its repeats are one failure
  to the [log](LOGGING.md#repeated-failures), however its count and time
  change. A target that starts to back off is logged once at warning level,
  with its failures and the time of its next try.

**How a target is identified.** A target is identified in the same way that
the [response cache](#response-caching) identifies probes: by the
collector's definition, the target, the probe parameters and the forwarded
headers. Probes with different `param_<name>` values back off separately,
and a static target and the identical probe share one state.

A collector remembers at most 10,000 targets that have failed, and forgets
the one that failed longest ago first, because probe targets come from the
query string. A target is forgotten as soon as a scrape of it succeeds, and
a reload that removes or changes the collector forgets all of them. The
state is in memory, per exporter process.

A [debug probe](#debug-probes) or run always contacts the target and does
not change the backoff. Its report says when the target is backing off, and
until when.

`http_exporter_backoff_skipped_total` counts the scrapes answered without
contacting their target, and `http_exporter_targets_backing_off` counts the
targets from which the collector is currently withholding scrapes
([self-metrics](SELF-METRICS.md#collector-metrics)). Both are `0` without
`failure_backoff`.

## Memory

The exporter's memory consists of the Go heap and the Python workers. The Go
heap holds responses, decoded documents and series while trips are in
progress, and the response cache. Each Python worker is a separate process.
`--probe.max-concurrent` bounds the first.
`--python.max-workers` and
[`limits.max_script_memory`](PYTHON.md#how-scripts-run) bound the second,
and `--python.max-idle-workers` and `--python.idle-timeout` set how many
workers are kept between scrapes, and for how long.

**Expression memory.** A jq or yq expression can build far more than the
response it reads: `[range(40000000)]` holds gigabytes, and a definition
that calls itself never stops building. The probe's time limit does not
bound that. A `jq` or `yq` transform is therefore stopped when the
exporter's memory has grown by more than `limits.max_expression_memory`
while it ran (256 MiB unless set), and the scrape fails as a limit exceeded,
with an error that names the rule:

```text
the transform was stopped at metric "queue_jobs": the exporter's memory grew by more than limits.max_expression_memory, 268435456 bytes, while it ran; make the expression keep less at once, or raise the limit
```

The limit is a [size](#sizes) of at least 1 MiB. It applies to what the
transform holds, not to what it creates and releases.

- Go measures memory for the process, not for one scrape, so what other
  scrapes hold at the same time counts too. A transform is not checked in
  its first 20 milliseconds, within which nearly all finish, and one that
  runs longer may be stopped anywhere between the limit and twice the limit.
  Set the limit well above what the collector's answers need, and lower it
  only on an exporter that is short of memory.
- A [debug probe](#debug-probes) reports the most that a transform's
  expressions held, as `jq transform memory` or `yq transform memory`, with
  `held_bytes` and `limit_bytes` among its logs. Use that to set the limit.
  Because it is the exporter's memory that is measured, probe when little
  else is being scraped.
- A single text, list or object larger than the whole limit stops its
  expression the moment an operator or a function creates it, without
  looking at the heap. An expression that doubles a text at every step is
  therefore stopped when the text first exceeds the limit, while holding
  that text, at most twice the limit, and the ones it was built from. A list
  counts as sixteen bytes per item and an object as forty-eight per key.
- A text, a list or an object that the answer itself holds was not created
  by the expression, however large it is, and reading it stops nothing. A
  copy of it, as `sort` and `tojson` make, does count.
- The failure is in the probe's `validation` stage and is counted in
  `http_exporter_series_limit_exceeded_total`, whatever `error_handling`
  says.
- The jq expressions of [pagination](REQUESTS.md#pagination) and
  [logins](AUTHENTICATION.md#logging-in-to-a-target) run outside the
  transform and are subject to the same limit, on each page and on each of a
  login's answers. One that exceeds it fails the scrape as a limit exceeded,
  in the place where a failed request fails
  (`error_handling.on_fetch_error`), and is counted in the same metric. The
  error names its key, as `request.pagination.next ".." was stopped: the
  exporter's memory grew by more than limits.max_expression_memory, …`.

**Decoded documents.** A decoded document is larger than the response it was
read from, and the more of the response is structure, the larger the factor.
A response of only empty fields, list items or elements, which a faulty or
hostile target can send, takes tens of times its size in memory while its
scrape lasts. The bound on that is
[`max_response_bytes`](REQUESTS.md#compression-and-the-response-size)
(10 MiB unless set) times the scrapes in progress. For targets you do not
run, set it to what their answers need, and set `--probe.max-concurrent` to
what the memory allows.

**Series.** `limits.max_metrics` (10000 by default) bounds a scrape's series
as they are produced, not afterwards. Every transform stops at the first
series over the limit, and the scrape fails with `metric count 10001 exceeds
limit 10000`: one over the limit, because counting further would cost the
memory the limit is meant to save.

- A response of a million lines read with the regex `(\d+)` is refused after
  ten thousand and one series, not after a million. A jq rule produces a
  series, and its labels, as its expression gives each value, with or
  without `items`.
- The prometheus decoder keeps only the series its transform passes on
  (those its rules match, or that `include` and `exclude` select) and stops
  in the same way, so a large exposition of which a collector keeps a few
  series costs only what those few cost. With a `pre_script`, which receives
  every series, the decoder keeps them all, and the transform does the
  counting.
- A python transform's answer is bounded by `limits.max_output_bytes`, and
  its metrics are counted before they are read.
- The failure is in the probe's `validation` stage and is counted in
  `http_exporter_series_limit_exceeded_total`, whatever `error_handling`
  says.

What counts is the series the scrape keeps:

- An item that a rule carries on without, under `error_mode: log` or
  `ignore` (a value that is not a number, a required label that is
  missing), produces no series and uses none of the limit.
- A rule that fails as a whole after it has produced series (a jq program
  that fails part of the way, or an XPath expression the engine fails on)
  gives their share of the limit back. The rules after it have everything
  the limit leaves.
- A prometheus rule that carries on without a series that it cannot produce
  (because of its `type` or its `scale` on a histogram or a summary, or a
  required label that the series lacks) is counted the same way, by the
  series that it produces. The decoder stops only for the series that a rule
  is certain to pass on, and keeps the others that a rule matches; the
  transform counts those that it actually produces from them. If a rule
  matches twenty series and keeps five, the scrape passes under
  `max_metrics: 5`.
- One thing counts although it is not kept: a rule that produces more series
  than the limit has room for ends the scrape there, even if it would have
  failed as a whole later and released them all, because finding that out
  would mean producing every series the response describes.
- A rule with a [`histogram` block](#histograms-from-a-list-of-values)
  produces one series per histogram, counted when its first value arrives,
  and none for the values it counts.

**Go memory limit.** In a container with a memory limit,
`--runtime.memory-limit-ratio` sets the Go memory limit to a share of it.
The container's limit is read at startup from the container's own cgroup, as
`/proc/self/cgroup` names it, under cgroup v2 or v1, with or without a
cgroup namespace. The smallest limit on the way up to the root is used, so a
pod-level limit counts too:

```sh
prometheus-universal-exporter --runtime.memory-limit-ratio=0.8
```

The Go runtime then collects garbage more aggressively as its heap
approaches 80% of the container's limit, instead of growing past it into an
OOM kill. The remainder is left for the Python workers and the runtime's own
overhead. The startup log says what was set.

- Without a container limit, the Go default stays, and `GOMEMLIMIT` in the
  environment takes precedence over the ratio.
- `0`, the default, leaves the limit off. The Helm chart sets `0.8`.
- Any other ratio must be at least `0.1`. A lower one is refused at startup
  and by `--dry-run`, because it would leave the Go heap almost nothing and
  the runtime would spend its time collecting garbage. Leaving room for the
  Python workers takes `0.5` to `0.95`.

`GOGC` in the environment is honoured as by any Go program. It is the
garbage collector's target: how far the heap may grow over the live data
before the next collection, 100 by default. Garbage collection is 13% to 22%
of the exporter's CPU. In measurements with `GOGC=400`, a probe of 5,000
series took 15% to 30% less time, in exchange for a heap that may grow to
about five times the live data between collections instead of two. With a
memory limit set as above, Go collects sooner as the heap approaches it, so
the higher target cannot push the process past the limit. The Helm chart
sets `GOGC` from `goGC.percent` (see
[the chart README](../charts/prometheus-universal-exporter/README.md#garbage-collector)).

## Probe deadlines

Prometheus states how long it will wait for each scrape in the
`X-Prometheus-Scrape-Timeout-Seconds` header: the job's `scrape_timeout`, or
a monitor's `scrapeTimeout`. A probe that takes longer is abandoned, and all
that Prometheus records is `up` 0 and a generic timeout. The reason the
probe was slow never reaches it.

A probe therefore gives itself that long, less `--probe.timeout-offset`
(500ms by default, as blackbox_exporter uses). When the time runs out, it
stops and answers with the reason while Prometheus is still waiting:

```text
collector legacy_text http failed: HTTP request failed: ... context deadline exceeded (the probe ran out of its 9.5s budget: Prometheus's scrape timeout less --probe.timeout-offset)
```

- A scrape timeout above an hour counts as an hour. Whoever can reach
  `/probe` sends the header, and a timeout of years would hold a slot of
  `max_concurrent_probes` for as long as a target hangs.
- A header that is not a positive, finite number (`0`, `-1`, `NaN`, `Inf`,
  text) is not a scrape timeout, and the probe is bounded like one without
  the header. A positive value is a budget however small: `1e-10` ends the
  probe immediately.
- The budget bounds the whole trip: the request or file read, decoding,
  transforms and Python scripts. The `timeout` probe parameter still bounds
  the request alone. Whichever ends first stops the probe.
- A budget that runs out while a metric rule is being evaluated (a jq
  expression that takes too long, or a response with more nodes or rows than
  there is time to read) fails the probe in the `transform` stage. The error
  names the rule that it was at (`the transform was stopped at metric
  "queue_depth"`). It is not that rule's failure: its
  [`error_mode`](#when-a-metric-cannot-be-extracted) does not apply,
  including `ignore` and `log`, and nothing is counted against it. The series
  that the other rules produced are not answered or cached as if they were
  the whole response.
- An offset of half the scrape timeout or more would leave too little time,
  so a probe always keeps at least half.
- Without the header (a probe from `curl`, a script, or anything other than
  Prometheus), the probe gets `--probe.default-timeout`, 30s by default.
  This keeps a target that accepts the connection and never answers from
  holding the probe, and its collector's `max_concurrent_probes` slot,
  indefinitely. Its error names that flag instead. A `timeout` parameter
  bounds the request within that budget and cannot raise it: `timeout=1h`
  still ends after 30s, and the error says the parameter was capped. `0`
  leaves such a probe unbounded, and a negative value is a command-line
  error.
- A probe answered from the [response cache](#response-caching) needs no
  budget. Identical probes that
  [share one request](#identical-probes-share-one-request) share the budget
  of the probe that started it.
- The offset covers writing the answer and the network between the exporter
  and Prometheus. Raise it if Prometheus still times out first. `0` uses the
  whole scrape timeout, and a negative value is a command-line error.

Once the exporter starts writing an answer, the client has 30 seconds to
read it, on every endpoint. An answer can be megabytes in size (a
passed-through exposition, the static targets, the verbose self-metrics, a
[debug report](#debug-probes)). A client that asked for one and stopped
reading would otherwise hold the request, and the answer in memory, for as
long as it kept the connection open. After 30 seconds its connection is
closed, which is logged at `debug` level only. The time that a probe takes
to produce its answer does not count; that is the probe's budget, described
above.

Static targets are unaffected: their scrapes are bounded by their own
`interval` (see [Static targets](STATIC-TARGETS.md#scraping)).

## Watching the configuration

The exporter reads its configuration once at startup, and again on request
(see [Reloading on demand](#reloading-on-demand)). With `--config.watch`, it
also re-reads the configuration file and, when one is configured, the static
target file whenever either changes on disk:

```sh
prometheus-universal-exporter \
  --config.file=config.yaml \
  --config.watch \
  --config.watch-interval=60s
```

`--config.watch-interval` defaults to 60s and must be positive. Zero or a
negative duration together with `--config.watch` is a command-line error
(exit 2), at startup and with [`--dry-run`](#dry-run) alike, instead of a
silently disabled watch. Without `--config.watch` no polling loop runs at
all, and configuration changes take effect on restart.

Changes are detected by modification time, size and permissions, not by
filesystem events. Any time other than the one last read is a change,
including an older one, as `cp -p`, `rsync -t` or a `mv` of a prepared file
leave it. So is a `chmod`, which may have made a file readable or
unreadable. The files are stamped before they are read, so an edit made
during the read is picked up by the next tick. Polling is deliberate:
Kubernetes republishes a mounted ConfigMap by atomically swapping the
`..data` symlink, which replaces the inode that a file-level event watch is
attached to, so such a watch would stop firing after the first change.

A reload that is rejected because a file could not be read is tried again at
every tick until the file can be read. That covers the configuration file, a
collector file or the static target file being missing, being a directory,
or being unreadable because of its permissions or a directory's. The other
of the two files is retried with it when it was rejected only for
disagreeing with it. The retry happens although nothing in the stamps
changed, because a file can become readable through its owner, an ACL or
the permissions of a parent directory, none of which the file's stamp
shows.

- The rejection is logged and counted once, like any other. A retry that
  fails in the same way logs nothing and counts nothing.
- A retry that fails in a different way (the file is now readable but
  invalid) is a separate rejection, logged and counted once. After that, the
  file is again read only when it changes.
- A reload rejected for what a file contains is not tried again until a file
  changes.

The watch covers the [collector files](#collector-files) too: an edited
collector file, a new file that matches a pattern, or a removed file
triggers a reload.

### Descriptor files

The watch also covers the statement files of the sql collectors in force,
`request.statement_file` (see
[A statement in a file](SQL.md#a-statement-in-a-file)). The statement is
read when the configuration loads, so an edited file causes a reload at the
next tick, and a statement the file no longer contains is rejected as the
load would reject it.

It also covers the descriptor files of the grpc collectors in force: a
collector's `request.protoset_file` and `request.proto_files`, and the files
that those `.proto` files import, through any number of files. The
configuration is checked against them when it loads: the collector's `rpc`
must be a method they define, and its `message`, like a static target's
`request.message`, must fit that method's request type. A grpc collector
also re-reads the files itself at its next call once they change (see
[Descriptors](GRPC.md#descriptors)). But nothing would check the
configuration against the new files: a descriptor set replaced by one
without the method would only be noticed through failing probes, with the
last reload still reported as successful. So when one of these files
changes, appears or disappears, the next tick reloads, as it does for the
configuration file, and the configuration is read and checked again against
the files as they are now. There is no flag to turn this on or off; it is
part of `--config.watch`.

When everything still fits, the reload is logged like any other, as
`configuration reloaded` with `"trigger":"watch"`, and counted as a
successful reload. The collectors are defined as before, so nothing is
dropped or restarted: their counters, their cached results, what the
[failure log](LOGGING.md#repeated-failures) remembers about them, and the
schedule of their static targets are kept. A result cached before the change
was produced with the old descriptors and is served until its
[`cache.ttl`](#response-caching) runs out.

When something no longer fits (the service no longer has the method, the
request type no longer has a field that the `message` sets, a `.proto` file
does not compile, an import is gone), the reload is rejected at that tick,
with the reason:

```json
{"level":"ERROR","msg":"configuration reload rejected","trigger":"watch","file":"/etc/exporter/config.yaml","error":"collector \"queue_stats\": the service acme.queue.v1.QueueService in protoset_file /etc/exporter/protos/queue.pb has no method GetStats; it has GetQueue, Watch","retried_when":"the configuration or a file it names changes"}
```

`http_exporter_config_last_reload_successful{file="config"}` then reads `0`
until a reload succeeds, so you can alert on a descriptor that broke a
collector from the tick that found it; see
[Configuration reloads](SELF-METRICS.md#configuration-reloads). The
configuration in force stays in force, but the old descriptors do not: the
collector reads the changed files at its next call whatever happened to the
reload. The calls that no longer fit them therefore fail, for the same
reason the reload was rejected, until the files are fixed. The rejected
reload is logged once and not tried again until a file changes again. The
tick after the files are fixed reloads, and is logged as a reload.

The [static target file](STATIC-TARGETS.md#reloading) is read and checked
again with the configuration when a target in force sets a `request.message`
for a collector that reads descriptor files, because the message is checked
against the same files. The watch does not determine which descriptor file
changed, so such a target file is also read for a file of a collector that
none of its targets uses, and is then found unchanged. A message that the
new files refuse is reported for the target file, on its own line and in its
own series:

```json
{"level":"ERROR","msg":"static target reload rejected","trigger":"watch","file":"/etc/exporter/targets.yaml","error":"target \"orders\": request.message does not fit acme.queue.v1.GetStatsRequest: (line 1:30): unknown field \"include_shards\"","retried_when":"the static target file, a file its check opens or the configuration changes"}
```

The configuration is then rejected with it, with an error that names the
same target, because the two take effect only as a pair that agrees. Both
stay as they were, and both are read again when the file changes once more.

- A target file without such a target is not read for a descriptor file.
  Neither is one whose own last reload was rejected: that one is read again
  for what its line names as `retried_when`.
- Where that line names the configuration (for example because the target
  file names a collector the configuration does not have), it means the
  configuration file, its collector files and, while the configuration is
  rejected too, the files for which the configuration is retried.
- A change to a descriptor file of the configuration in force reloads the
  configuration, which is what rejected the target file, and leaves the
  target file as it is. The descriptor files that the target file's own
  check opened are named separately, as `a file its check opens`, and do
  cause it to be read again.

**Which files are watched.** The set follows the configuration in force: a
reload that removes a grpc collector, or points it at other files, changes
the set from that reload on.

- An imported file is watched where `proto_import_paths` resolved it. The
  place in an earlier import path where it was looked for and not found is
  watched too, because a file that appears there is the one compiled from
  then on.
- A file of the same name in a later import path is never read and not
  watched. Neither are the locations of the well-known files
  (`google/protobuf/*.proto`), which are built in.

**What counts as a change.** A file has changed when its modification time,
its size or its permissions have changed, when it appeared or disappeared,
or when its path leads to another file. The last happens through a symbolic
link, which is how Kubernetes swaps in a new version of a mounted ConfigMap,
or because another file was renamed over it, which the file's device and
inode reveal even when the new file has the time, size and permissions of
the old one.

The reload checks the configuration against the descriptors as the
collectors read them, and a collector's call looks at its files in the same
way as the watch:

- A link pointed at a file with exactly the time and size of the old one,
  or a file renamed over by one with the same time, size and permissions,
  causes a reload, and the next call compiles the new file.
- A file whose permissions alone changed causes a reload too, and the next
  call reads it again. A file made unreadable by a `chmod` then fails the
  reload and that call with `permission denied`. That is also true of an
  unreadable file in an earlier import path: the file of that name in the
  next import path is not read instead, as it is when the file is missing
  there.
- The exporter running as root reads a file whatever its permissions, so
  there a `chmod` only makes the next call compile the files once more,
  successfully.

The device and inode are what the filesystem reports. On a filesystem that
gives an unchanged file a different inode number (a FUSE filesystem mounted
without `use_ino`, once the kernel has dropped the file from its cache, or
after the files are mounted again), the file counts as changed. The next
tick reloads, and the next call compiles the files again, once each time the
number changes. Nothing reads the files to decide otherwise, because that
would cost every call a read. On a filesystem that changed the number at
every look, every tick would reload and every call would compile.

**Cost.** The cost is one look at each of these files per tick. A tick that
finds them unchanged reads no file, compiles nothing and logs nothing.
However many of them changed since the last tick, there is one reload.

- Starting the exporter reloads nothing. The files are stamped when the
  configuration is first read, before it is checked against them, so a file
  edited while the exporter was starting is noticed by the first tick.
- A reload stamps them in the same way, before it validates what it read,
  however `request.type` and `descriptors` are written (`GRPC` is `grpc`).
  The `.proto` files are compiled at that point to learn what they import,
  and the validation uses that same compilation, so a startup and a reload
  compile no file twice unless it fails to compile.
- Without `--config.watch` none of this happens: no descriptor file is
  stamped, and a startup or a reload reads them only to check the
  configuration.

### Rejected reloads

A rejected reload is not tried again until something changes, and is logged
once. More than the configuration can change, though. Loading it opens the
files that it names, and a reload that runs at the moment one of them is
being replaced is rejected because of that file alone: a certificate missing
while a Secret is rotated, or the new certificate in place before its key.
While a configuration is rejected, the watch therefore also covers the files
that it names and the load opens, as well as those of the configuration in
force:

- `otlp.tls.ca_file`, `cert_file` and `key_file`, when `otlp` is enabled;
- `web.basic_auth.username_file` and `password_file`, when it is enabled;
- a sql collector's `request.statement_file`;
- a grpc collector's `request.protoset_file` and `request.proto_files`, and
  the files that those `.proto` files import, through any number of files.
  The configuration does not name the imported files, but the compile reads
  them. Each is watched where `proto_import_paths` resolved it. A missing
  one is watched in every place it was looked for, so a reload rejected for
  a missing import is tried again when the file appears. The place in an
  earlier import path where a file was looked for before it was found counts
  too, because a file that appears there is the one compiled from then on. A
  file of the same name in a later import path is never read and not
  watched. Neither are the well-known files (`google/protobuf/*.proto`),
  which are built in.

The next tick reloads when one of these files appears, disappears or
changes. A change is a different modification time, size or permissions, or
a path that leads to another file. The last happens through a symbolic link,
which is how Kubernetes swaps in a new version of a Secret, or because
another file was renamed over it, which its device and inode reveal.

- There is one reload however many of the files changed, and a reload that
  then succeeds is logged like any other.
- A tick that finds the files unchanged does nothing and logs nothing: it
  reads no file and compiles none.
- The descriptor files are watched as they were before the rejected reload
  read them. One that was fixed while that reload was still running (after
  it was read, before the reload was rejected) is therefore not treated as
  read: the next tick reloads.
- Once the configuration is in force, the watch covers only the
  [descriptor files](#descriptor-files) and no longer the certificates and
  credential files. Nothing in the configuration is checked against those,
  and the export and the exporter's own authentication each read their files
  again when they change.
- A collector's own credential and TLS files are read at each request, not
  when the configuration loads, and never reject a reload.

The [static target file](STATIC-TARGETS.md#reloading) is retried in the same
way. Checking it opens the `request.protoset_file` and `request.proto_files`
of the grpc collectors whose targets set a `request.message`, and the files
that those `.proto` files import, because the message is checked against
them. A reload of the target file at the moment such a file is being
replaced is therefore rejected because of that file alone.

- While the target file is rejected, the watch covers those files too: the
  imported ones as for the configuration, and each as it was before the
  check read it.
- The tick after one of them changes reads the target file again, although
  it is unchanged, and the configuration with it, because they are the
  configuration's [descriptor files](#descriptor-files). A tick that finds
  them unchanged does nothing and logs nothing.
- Once the target file is in force, they stay watched as the configuration's
  descriptor files are: a change to one reads both files again.
- A target file rejected for its own content (it is not YAML, or a target
  has no collector) opens no other file, and is read again when it changes.
- A target's own credential files (`request.bearer_token_file`,
  `request.basic_auth_file`) are read at each scrape and never reject a
  reload.

The watch does not relax any reload rule. These are all still rejected: an
invalid configuration, one that would disable OTLP while a loaded static
target sets `export_via_otlp`, a collector name defined twice, and a
pre-script that stops producing `data`. The last valid configuration stays
active, and the reason is logged with the rejected file's path as `file`
(`configuration reload rejected` or `static target reload rejected`).
`http_exporter_config_last_reload_successful` then reads `0` until a reload
succeeds, so you can alert on a change that did not take effect; see
[Configuration reloads](SELF-METRICS.md#configuration-reloads).

With the watch on, the line of a rejected reload also says, as
`retried_when`, what causes the watch to read that file again. It names only
what applies:

- The file itself: `the configuration changes`, which includes its collector
  files, or `the static target file changes`.
- With it, a file watched while the file is rejected, as above: `the
  configuration or a file it names changes`, or `the configuration, a file
  it names or a file one of those imports changes` when a `.proto` file it
  names imports others.
- The other of the two files, when this one was rejected only because it
  disagrees with the other as in force (for example, a target that names a
  collector the configuration does not have), so that a change to either may
  resolve it: `the static target file, a file its check opens or the
  configuration changes`.
- A file rejected because a file it reads could not be read adds that it is
  tried at every tick: `the configuration changes, and at every tick while a
  file it reads cannot be read`.

Without `--config.watch` nothing is read again until a reload is requested,
and the line has no `retried_when`.

## Reloading on demand

A tool that has just written the configuration can reload the exporter
immediately instead of waiting for the watch, and learn whether the new
configuration was accepted:

```sh
kill -HUP "$(pidof prometheus-universal-exporter)"   # always available

curl -X POST http://exporter:8080/-/reload            # with --web.enable-lifecycle
```

Both reload the configuration with its collector files, and the static
target file, whether or not they changed, under the same rules as the watch.
`POST` (or `PUT`) `/-/reload` answers:

| Status | Meaning |
| --- | --- |
| `200` | Every file was accepted and is in force. |
| `500` | A file was rejected; the body says why, and the previous configuration stays in force. |
| `403` | The exporter was started without `--web.enable-lifecycle`. |
| `405` | Any method other than `POST` or `PUT`. |

The endpoint is off unless `--web.enable-lifecycle` is passed, as in
Prometheus. When `web.basic_auth` is configured, it needs the same
credentials as the other endpoints.

`SIGHUP` needs no flag and is safe to send at any time. One that arrives
while the exporter is still starting (reading its configuration and checking
its Python scripts) does not end the process: it waits, and the exporter
reloads once as soon as it has started.

In both cases the reload is logged with its trigger (`"trigger":"http"`,
`"sighup"` or `"watch"`) and counted in the
[reload self-metrics](SELF-METRICS.md#configuration-reloads). Reloads from
different triggers never interleave: one runs at a time.

### What a reload changed

Each file that a reload puts in force is logged with what it changed, under
`changes`. The `configuration reloaded` line names the collectors added,
removed and changed. The `static targets reloaded` line does the same for
the static targets. Each list is sorted, and a reload without changes says
`no changes`:

```json
{"level":"INFO","msg":"configuration reloaded","trigger":"sighup","collectors":12,"collector_files":3,"changes":"collectors added: queue_depth; collectors removed: legacy_status; collectors changed: weather"}
{"level":"INFO","msg":"static targets reloaded","trigger":"sighup","targets":40,"changes":"no changes"}
```

- A collector is changed when anything in its definition differs, in the
  configuration file or in a collector file, including a key that it takes
  from its file's [defaults](#defaults-for-the-collectors-of-a-file). The
  same condition drops its cached results (see
  [Response caching](#response-caching)).
- A static target is changed when anything in its entry differs, including
  the file's defaults that it takes.
- Each list names ten entries and counts the rest (`collectors added: a01,
  …, a10 and 30 more`), so a reload that replaces a whole file writes a line
  of bounded length.
- The line is the same whatever triggered the reload: the watch, `SIGHUP`,
  `/-/reload` or a bundle from the [target allocator](ALLOCATOR.md).
- A rejected reload puts nothing in force and changes nothing.

The landing page at `/` shows when the last reload put something in force,
and what it changed as a table that combines the changes of both files. It
has a row for each list, with what changed (collectors or static targets),
how (added, removed or changed), how many, and the names: the first 10 and a
count of the rest. A reload that changed nothing says so in a sentence.
Before the first reload, the page says that the exporter has not been
reloaded since it started. The time of the last successful reload is
`http_exporter_config_last_reload_success_timestamp_seconds`
([Configuration reloads](SELF-METRICS.md#configuration-reloads)).

Whatever the trigger, the exporter's state follows the new configuration:

- A removed collector's [self-metrics](SELF-METRICS.md#collector-metrics)
  stop, together with those of its
  [Python workers](SELF-METRICS.md#python-workers), and what was kept about
  it is dropped.
- A changed collector's cached results are dropped, and its
  [static targets](STATIC-TARGETS.md#reloading) are scraped again within ten
  seconds.
- The configuration and the static target file take effect together, in one
  step: nothing ever runs with the new version of one and the old version of
  the other.

**Probes in flight.** A probe or a static target scrape that had read its
collector before the reload continues with the collector it read, and is
answered. When the reload removed that collector or changed its definition,
what such a late probe or scrape would leave under the collector's name is
discarded:

- Its result is not cached. A collector brought back under the name is
  therefore not answered with it within `cache.ttl` or
  `cache.stale_if_error`, and the result does not count toward the
  `limits.max_cache_entries` of the collector now under the name.
- Its failure or success does not count as a failure or a
  [recovery](LOGGING.md#repeated-failures) of that collector. The failure is
  logged at debug level only, marked `"superseded":true`.

A collector that the reload left unchanged caches and logs as before,
whichever configuration its probe had read.

This applies from the moment of the reload. Whether `/-/reload`, `SIGHUP` or
the watch made it, the reload itself drops the cached results of the
collectors it removed or changed and forgets their remembered failures
before it returns, so `/-/reload` answers when that is done.

- A failure that ends in the instant before the reload belongs to the time
  before it: it is logged as such and forgotten with the rest.
- A changed collector's remembered failures are forgotten like a removed
  one's. The first failure of the new definition is therefore logged in full
  as a first failure, and a static target that failed under the old
  definition and succeeds under the new one is not logged as recovered.
- A static target that the reload removed from the static target file, or
  changed there, is treated like one whose collector changed, although the
  collector is unchanged. Its remembered failures are forgotten. The failure
  of a scrape that had read it before the reload is logged at debug level
  only, marked `"superseded":true`. The success of such a scrape is not a
  recovery of the target now under the name.

**Publishing.** A static target's scrape that had read its target before a
reload removed or changed the target, or its collector, publishes nothing on
the
[static targets endpoint](STATIC-TARGETS.md#the-static-targets-endpoint) or
over OTLP, even when a target is back under the name unchanged.

- The endpoint keeps what it has of the target now under the name until that
  target's own first scrape replaces it. That scrape is made within ten
  seconds of the reload, or as soon as the earlier scrape has ended.
- This also applies when reloads in quick succession changed the target, or
  its collector, and changed it back, or removed it and brought it back.
- A target that the reload left unchanged, with its collector, is published
  as before.

**Shared probes.** Identical probes in flight
[share one trip](#identical-probes-share-one-request) only within one stay
of their collector. Suppose reloads changed a collector and changed it back,
or removed it and brought it back, while an earlier probe was at the target.
A new probe of that collector then makes its own trip, and its result is
cached and its failure logged like those of any probe of a collector in
force.

## Metric lint

The metric lint is advice about the series that a collector produces: names
that break Prometheus's
[naming conventions](https://prometheus.io/docs/practices/naming/), labels
whose values look unbounded, and one name that two collectors produce in
different ways. The [dry run](#dry-run) lists what the configuration alone
shows. The [debug report](#debug-probes) and the
[debug page](#the-debug-page) add what the series of a run show.

The lint is only advice. The dry run still passes (exit status `0`), and a
probe, a static target's scrape and a debug run answer the same with or
without it. Nothing else reports it: not startup, not a reload, not a probe.

Each finding names its collector, its rule by a stable id, and the metric by
the name it is exported under, with the collector's `metrics_prefix`:

```text
[counter-total] metric "jobs_done": a counter whose name does not end in _total is written as jobs_done in the Prometheus text format and as jobs_done_total in OpenMetrics, so its name depends on the format a scraper asks for; name it jobs_done_total
[unit-suffix] metric "poll_latency_ms": the unit ms is not a base unit; Prometheus names use seconds, bytes and ratios from 0 to 1: convert the value with scale: 0.001 and name it poll_latency_seconds
```

| Rule | Where | Says |
| --- | --- | --- |
| `counter-total` | dry run, debug | A counter whose name does not end in `_total`; a gauge whose name does. |
| `unit-suffix` | dry run, debug | A unit that is not a base unit; a name that reads as a duration, a time or a size without its unit. |
| `unbounded-label` | debug only | A label whose values in one run look like ids or times, or are more than 100. |
| `name-clash` | dry run, debug | A metric name another collector makes with another type, help or labels. |

The rules read the names that the configuration gives: a rule's `name`, and
the `name` of a [derived entry](#derived-series) that is not `hidden`, each
with the collector's `metrics_prefix`. A derived entry has the `type` it is
written with, or `gauge` without one. In a debug run the rules also read the
names of the series that the run produced. The names that a `python` script
produces, or that a `prometheus` rule without a `name` passes on, are
therefore linted there, although the dry run cannot know them. A finding
that both give is shown once.

### counter-total

The exporter does not add `_total` to a counter's name in the Prometheus
text format. A counter named `jobs_done` is the series `jobs_done` there and
`jobs_done_total` in [OpenMetrics](#openmetrics), and the scraper chooses
the format.

- A rule of `type: counter` whose name does not end in `_total` is reported,
  with the name to give it.
- A rule of `type: gauge` whose name ends in `_total` is reported too: the
  suffix indicates a counter, and `rate()` over a gauge that decreases gives
  meaningless results.
- A rule of `type: untyped` is not reported. Neither is a `prometheus` rule
  without a `type`, which keeps the type of the series.

### unit-suffix

A name is read as its words, which are the parts between underscores, in any
letter case.

- **A unit that is not a base unit.** When such a unit is the last word, or
  the last word before a counter's `_total`, the name is reported with the
  base unit's name and the
  [`scale`](#mapping-text-to-values-and-scaling-them) that converts the
  value:

  | Words | Base unit | `scale` |
  | --- | --- | --- |
  | `ms`, `millis`, `milliseconds` | `seconds` | `0.001` |
  | `us`, `microseconds` | `seconds` | `0.000001` |
  | `ns`, `nanoseconds` | `seconds` | `0.000000001` |
  | `minutes`, `hours`, `days` | `seconds` | `60`, `3600`, `86400` |
  | `kb`, `kilobytes`, `mb`, `megabytes`, `gb`, `gigabytes` | `bytes` | `1000`, `1000000`, `1000000000` |
  | `kib`, `mib`, `gib` | `bytes` | `1024`, `1048576`, `1073741824` |
  | `percent`, `percentage` | `ratio` | `0.01` |

  `us` and `ns` mean a region or a namespace as often as a unit. They are
  read as a unit only in a name that also has a duration word (below):
  `request_latency_us` is reported, `requests_us` is not.
- **A duration or a time without `seconds`.** A name with one of the words
  `duration`, `latency`, `elapsed`, `uptime`, `age`, `time` or `timestamp`
  and without the word `seconds` (or `ratio`) is reported. The finding gives
  the name of a duration in seconds, `request_duration_seconds`, and of a
  point in time as a Unix time in seconds,
  `last_success_time_timestamp_seconds`. A name with `timestamp` is given
  only the second. `timeout` and `timeouts` are not among the words, because
  they count timeouts more often than they measure one. Neither is `times`,
  which counts.
- **A size without `bytes`.** A name with the word `memory` and without the
  word `bytes` (or `ratio`) is reported, with the name in bytes. `size` is
  not among the words: `queue_size`, `pool_size` and `batch_size` count
  items.

The lists are short on purpose: a word that often means something else would
produce advice that nobody reads.

### unbounded-label

Each value of a label is a separate series, which Prometheus keeps for as
long as it keeps the metric. A label whose values keep changing (a request
id, a user, a time) therefore grows the number of series without bound. In a
debug run, each label of each metric is reported once when, among the run's
series:

- it has more than 100 distinct values, or else
- one of its values is a UUID (`5f0c6c2e-8a5b-4c1e-9d3f-2b7a1e0c9d48`), a
  hexadecimal string of 16 or more digits with or without `0x` (a hash or an
  id), a number of 10 or more digits (an id or a Unix time), or a date or an
  RFC 3339 time (`2026-10-24`, `2026-10-24T09:14:05Z`,
  `2026-10-24 09:14:05.123+03:00`).

The finding quotes the first 64 bytes of the value.

- An address with its port, such as `10.0.0.7:9100`, is not reported: a
  target's address is the most common bounded label.
- The shape of a value is not held against a metric that has one series in
  the run, such as
  `build_info{revision="9fceb02d0ae598e95dc970b74767f19372d61af8"}`. One
  series cannot grow its metric's series within a scrape, and the one value
  of an info metric changes only when what it describes changes. Two series
  with such values are reported like any others.
- The dry run sees no values, so it gives none of this advice.

### name-clash

Prometheus keeps one type and one help text for a metric name, and a query
over the series of two collectors that produce the same name mixes them. A
name that a collector's rules produce is therefore reported when a rule of
another collector produces it with:

- another type,
- another help text, where both rules have a `description`, or
- other label names. These are the rules' labels and `transform.labels`,
  after `transform.remove_labels` and `transform.rename_labels`. For a
  derived `sum` with `by` labels, they are its `by` labels and
  `transform.labels`, after the same two settings.

The clash is reported for each of the two collectors, and each report names
the other collector. What one side cannot know is not compared:

- the names that a `python` script produces, or that a `prometheus` rule
  without a `name` passes on;
- the type that a `prometheus` rule without a `type` keeps;
- the labels of a `prometheus` transform's series, which are the target's;
- the labels of the series of a derived entry that uses a series operation,
  which are those of its operands' series.

The dry run compares every collector of the configuration. A debug run
compares the collector it runs with the others in force. A collector written
on the debug page is compared with every configured collector of another
name.

### Silencing a rule

A collector's `lint.ignore` lists, by id, the rules whose advice is not
given for it:

```yaml
collectors:
  - name: legacy
    lint:
      ignore: [unit-suffix, name-clash]
```

An id that belongs to no rule fails the configuration, like any wrong value,
so a misspelt id cannot silently silence nothing. `name-clash` is silenced
only for the collector that lists it: the other collector of the pair still
reports the clash and names it. The debug report says which rules a
collector silences.

## Debugging collectors

When a collector does not produce what it should, run it and read what
happened step by step: the requests, the response, each stage, what was
logged, and the series that a probe would have answered. One flag,
`--web.debug` (the Helm chart's `server.debug`), sets how much is available:

| `--web.debug` | What is served |
|---------------|----------------|
| `off`, the default | Nothing: `/debug` is not there (`404`), and `debug=true` on `/probe` and `debug=<name>` on `/static-targets` are answered `403`. |
| `edit` | The [debug page](#the-debug-page), `/debug`, where a collector — the playground, written there, or one of the configuration by name — is run on a **pasted** response. Nothing is fetched, so nothing a target answers is ever shown. `debug=true` and `debug=<name>` are still answered `403`. |
| `full` | Everything `edit` serves, and what shows a target's answer: on the page, fetching a target through a configured collector or scraping a static target; [`/probe?...&debug=true`](#debug-probes); and [`/static-targets?debug=<name>`](STATIC-TARGETS.md#debugging-a-static-target). The [status page](ENDPOINTS.md#status-page) shows each error whole, with the values of the answer it quotes and what it holds for the operator alone, such as an [`sshexec`](SSHEXEC.md) command's stderr, which below `full` it leaves out as a probe's caller is answered ([What a failed probe's caller is told](#what-a-failed-probes-caller-is-told)). |

Any other value stops the exporter at startup, like any wrong flag (exit
status 2). Every run is made by the same engine and reported in the same
terms, whichever way it was requested: as plain text by the two URLs, and as
JSON by the page, which shows the text report too. Each run is counted in
`http_exporter_debug_runs_total{kind, result}`
([Self-metrics](SELF-METRICS.md#debug-runs)), and in no collector's
self-metrics.

- Use `edit` where the people who can open the exporter's pages may write
  and try collectors but should not see what the targets answer: the page
  runs only on what they paste.
- Turn `full` on while a collector is being fixed against its real target,
  and off again afterwards. A report shows what the target sent, and a
  secret under a header name that does not look like one, or in the body, is
  shown as the target sent it.

[Debugging](AUTHENTICATION.md#debugging-collectors) describes what each
level lets a visitor of the exporter's pages do.

`--web.debug.dangerous` (the chart's `server.debugDangerous`) is valid with
`full` only. It lifts the restrictions that protect the server from the
playground: see [Dangerous debug mode](#dangerous-debug-mode).

### The debug page

`/debug` runs one collector and shows the series that a probe would answer,
the stages, the advice of the [metric lint](#metric-lint), what was logged,
the requests sent, the response read and, folded away, the whole
[report](#debug-probes).

It runs only when *Run* is pressed: not after an edit, and never when the
page opens. On opening, the collector and the target that the page's address
names are filled in and wait for *Run*, because a link on any page can name
them. The landing page links to it, and `/collectors` links each collector
into it, with the collector chosen and, at `full`, with the target that the
collector's form holds when the link is followed.

The collector run is one of two:

- **The playground** is a collector written on the page: the YAML of one
  collector as it is written under `collectors:`, that is, a mapping that
  starts with `name`, without the dash.
  - It is read like a configuration's collectors: the same keys and the same
    checks. A mistake is reported in the words a load would use, with line
    numbers counted from the collector's first line.
  - `${NAME}` references are left as written, whatever `--config.expand-env`
    says.
  - It is never put in force, and its own request block never makes a
    request. It may be of any request type the build has. Its `request.type`
    only determines how its answer is read: its decoder, a `sql` collector's
    rows, or the JSON document that a `tlscert` handshake is turned into.
    The keys that a request needs in order to be made, such as an `sshfile`
    collector's `known_hosts_file` or a `grpc` collector's `proto_files`,
    are not required.
  - It takes no [defaults](#defaults-for-the-collectors-of-a-file).
    `defaults` is a key of a file and is refused here, as `collectors` is. A
    configured collector runs with the keys it took from its file's
    defaults.
- **Configured** is a collector of the configuration in force, chosen by
  name. It runs as configured, including its Python: the script is the
  operator's and reads whatever a target sends on every probe, which is what
  a pasted body stands for.

The editor shows the collector that runs. While no configured collector is
chosen (*Playground* in the list), it holds the playground. That is the
example, an `http` collector, every time the page opens, and then whatever
was written, for as long as the page is open. Nothing of it is kept in the
browser. A configured collector, chosen in the list or by the link from
`/collectors`, is shown instead. It is read-only, because it runs as
configured:

- **As its file writes it.** Its keys are in the order of the file. Its
  `${NAME}` references are shown as written, not expanded. The keys that an
  anchor or a merge key (`<<`) gives it are written out, as the loader reads
  them. The file's comments are left out. A folded block scalar, such as a
  `statement: >-` of several lines, is shown on the lines the file uses, not
  folded into one, like a literal one (`|`). The keys it takes from its
  file's [defaults](#defaults-for-the-collectors-of-a-file) are added where
  it would write them, each marked `# from the file's defaults`.
- **Without its credentials**, at `edit` as at `full`:
  - A value whose key looks like a credential's is shown as `<redacted>`:
    `bearer_token`, the `password` of `basic_auth`, or a header, query
    parameter or login field with such a name, by the same rule that
    withholds a header in a debug report.
  - Every path that a `*_file` key names is shown as `<redacted>` too; no
    page shows these paths.
  - A URL's userinfo and the values of its query parameters named like
    credentials are withheld.
  - Any other text of the request (a header's or a query parameter's value,
    an `http` body, a `grpc` message, a `tcp` send, a `redis` command's
    argument, a `sql` statement) is shown as `<redacted>` as a whole when
    something in it looks like a credential. The rule is the one that masks
    an error's text: `"password": "…"`, `token=…`, `Bearer …`, a URL's
    userinfo, `<password>…</password>`, or JSON written as a string in JSON
    (`\"password\":\"…\"`).
  - The same applies when the text logs in to a text protocol: `AUTH …`,
    `PASS …` or `APOP …` at the start of a line or after a `;`, IMAP's
    `a1 LOGIN user …`, Redis's `HELLO 3 AUTH user …`, or `AUTH` framed as
    RESP (`*2\r\n$4\r\nAUTH\r\n$7\r\n…`). A word that only begins like one,
    such as `author`, does not count. A folded block among these texts is
    then shown on one line.
  - A Python script is shown as written, except for what looks like a
    credential in it, which is masked like an error's text. Keep a script's
    credentials in a file that it reads.

*Edit a copy* makes the playground a copy of that YAML. The copy then runs
instead, under the rules for the playground.

- **What the copy leaves out.** The copy omits what the playground would
  refuse: every key of the request that ends in `_file` or `_files`
  (`tls.ca_file`, `password_file`, `bearer_token_file`, `known_hosts_file`,
  …) and a `login`. Each line is replaced by a comment that says what was
  dropped and why, such as
  `# tls.ca_file dropped: the playground reads no file of the server's (--web.debug.dangerous lets it)`.
  A block left empty, such as a `tls:` that held only `ca_file`, is dropped
  with it, so that the copy loads. A block written in flow style,
  `tls: {ca_file: /etc/ca.pem, server_name: api.example}`, is written out
  with one key per line so that the comment has its own line. A note above
  the editor lists what was left out.
- **Fetching.** A target probed, or a static target scraped, through the
  collector it was copied from is still fetched with the omitted keys.
- **Dangerous debug mode.** In
  [dangerous debug mode](#dangerous-debug-mode) a `login` is kept. The
  paths, which the page withholds, are still dropped, and each comment says
  to write the path in its place.
- **Python** is left in. The note says that a run of the copy is refused
  until the script is removed, as for any playground, except in dangerous
  debug mode.
- **`protobuf` descriptors.** The files of a
  [`protobuf` decoder](#protobuf-responses) (`response.protobuf`) are also
  files of the server. A playground that names them is refused in the same
  way, so run a collector with that decoder by its name.
- **Redacted values.** Any other value shown as `<redacted>` must be left
  out or written again.
- **Request types.** A copy of a collector of any request type runs. A
  `tlscert` collector's copy reads a pasted handshake, or what `tls_cert`
  answers when a target is probed or a static target scraped through it.
- The page remembers which collector the playground was copied from, through
  edits, while it is open, and fetches a target through it.
- *Edit a copy* waits for the collector's YAML: it is disabled while the
  page reads it, and stays disabled when it could not be read. *Back to the
  example*, shown while the playground is a copy, restores the example.

**Line numbers and errors.** The editor numbers its lines in a gutter beside
the text, for a configured collector's YAML and for the playground. It does
not wrap: a long line, such as a statement, scrolls sideways, and the
numbers stay beside their lines however the text is scrolled. When a run of
the playground is refused with a mistake on a line (`line 7: unknown key
"bogus" in a metric rule`), each line that the error names is marked in the
gutter and with a band behind it. `line 7` in the error is a link that
selects that line in the editor. The marks disappear with the next edit.

**Keys.** In the playground, Tab indents by two spaces, or indents the
selected lines, and Shift+Tab outdents them. To leave the editor with the
keyboard, press Esc, then Tab. Ctrl+Enter, or ⌘+Enter on a Mac, runs from
anywhere on the page, like *Run*.

**Colours.** The YAML is coloured as it is typed: keys, strings, numbers,
booleans and `null`, comments, a list's dashes, punctuation, a block
scalar's indicator (`|`, `>-`) and its lines, anchors and aliases (`&a`,
`*a`), tags (`!!str`), and the exporter's own `{{param_<name>}}`
placeholders and `${NAME}` references. Each has its own colour in either
theme. A value that the page withholds, `<redacted>`, is shown muted. The
colouring is a layer behind the text, which remains the browser's own, so
undo, Tab, input methods and paste work as in any text box.

**Folding.** A block (a key with lines indented under it, a list item that
continues below, or a block scalar) folds with the ▾ button beside its line
number.

- The first line of a folded block ends with the number of lines it hides,
  `… 12 lines`. A click on that unfolds it.
- The other lines keep their numbers, so the gutter skips the hidden ones. A
  line that an error names and that is folded away is marked on the line of
  its fold.
- Tab reaches one of the buttons (the one last used, or else the first). The
  arrow keys, Home and End move to the others. Enter or Space folds or
  unfolds.
- *Expand all* and *Collapse all* above the editor unfold or fold every
  block.
- A configured collector's YAML folds in place. The playground cannot be
  edited while one of its blocks is folded,
  because a text box cannot hide its own lines. Folding it therefore shows
  it read-only, like a configured collector's. A click on its text, Enter,
  or *Expand all* unfolds it and puts the caret where it was clicked.
- *Run*, *Copy as*, *Edit a copy* and the lines that an error names always
  use the whole YAML, folded or not.

The collector reads one of three responses:

- **A pasted response**: a body of up to 1 MiB, its `Content-Type`, a status
  (200 unless given) and other headers. No request is made.
  - The body starts as the example's answer with the example playground, and
    empty with a configured collector or a copy of one. A body that has been
    edited is kept whichever collector is chosen.
  - The status must be one that the collector's `request.accept_status`
    accepts, and the body is held to the collector's response limit, as a
    fetched response would be.
  - The page asks for a status and headers only for an `http` or `graphite`
    collector, whose answers are HTTP responses, and for a `Content-Type`
    only where it can choose the decoder. The body's placeholder says what
    to paste: what the target answers, a file's content or, for the types
    that build their own answer, the JSON document that a run's *Response*
    shows.
  - For the playground, these fields follow the `request.type` written in
    the editor as it is typed, in block style (`request:` with `type: sql`
    under it) or in flow style (`request: {type: sql}`). With no type
    written, or a type the build does not have, the page asks as for `http`,
    and the run says what is wrong. Text typed in a field that the type
    hides is kept for when the field is shown again, and is not sent.
  - A response pasted for a `localfile` collector that reads a directory is
    read as one file.
  - A `sql` collector's response is its rows, pasted as JSON in the form
    that a run's *Response* shows: a list of objects with each row's columns
    by name, `[{"state": "open", "jobs": 3}]`. A whole number is read as an
    integer, `true` and `false` as 1 and 0, and `null` leaves the column out
    of its row, as `NULL` does. Anything else is refused with `400`.

  The other headers are rows of a name and a value, starting with one empty
  row. *+ Add header* adds a row and *×* removes one. A blank row is not
  sent. A name may be repeated, in any letter case, as a response may repeat
  a header: the run has each value, in the order of the rows. A row with a
  value and no name, or with a name that is not an HTTP token (letters,
  digits and ``!#$%&'*+.^_`|~-``), is marked on the row with the reason,
  `Header name "Bad Name" is not one HTTP allows.`, and the run is not sent.
  The server checks again what it receives. The rows are kept when another
  collector is chosen.
- **A target** (*Probe a target* on the page), at `full` only. It is fetched
  exactly as `/probe?debug=true` fetches it, through a configured collector:
  the one chosen, or the one the playground was copied from. The fetch uses
  that collector's request block,
  [`allowed_targets` and `denied_targets`](REQUESTS.md#restricting-targets),
  credentials, `accept_status` and
  [`max_concurrent_probes`](#limiting-concurrent-probes), and the probe
  parameters entered on the page.
  - The playground decodes and transforms what the collector it was copied
    from fetched; see below for what the page asks for.
  - A playground that was not copied from a configured collector, such as
    the example, has nothing to fetch through, so *Probe a target* is not
    offered with it.
  - A collector that runs on what another collector fetched can read only
    what a collector of its own request type fetches, because it reads that
    type's answer. A copy whose type was changed is refused (`400`).
- **A static target's scrape**, at `full` only. It is made once, as the
  schedule makes it: with the target's own settings and credentials, its
  interval as the budget and its labels on the series, as
  [`/static-targets?debug=<name>`](STATIC-TARGETS.md#debugging-a-static-target)
  makes it, through the target's own collector. Nothing is published.
  - The target is chosen from a list of the static targets scraped through
    the collector that a target is fetched through: the configured one
    chosen, or the one the playground was copied from. Each is shown as its
    name and its target, `sofia — https://api.open-meteo.com`, with any
    credential in the address withheld as the
    [status page](ENDPOINTS.md#status-page) withholds it.
  - The list is read again whenever that collector changes, so it follows a
    reload of the static target file.
  - With no static target of that collector, or none at all, *Scrape a
    static target* is offered disabled, with a note that says so. The
    example playground, copied from no collector, scrapes none, as it
    fetches none.
  - The panel says whose static targets it lists: *Static targets of
    tls_cert, the collector chosen.*, or *…, the collector this playground
    was copied from.*
  - The scrape is read by the collector chosen (the target's own) or by the
    playground (a copy of it that may have been edited).
  - `/debug?static_target=<name>` opens the page with that target chosen and
    its collector chosen on the left.

What the page asks for follows the request type of the relevant collector.
For a pasted response that is the collector run (for the playground, the
type its editor writes). For a target it is the collector that the target is
probed through. Each type declares its own fields, so a build without a type
shows nothing of it:

| Request type | Target, as the page labels and hints it |
| --- | --- |
| `http` | URL: `https://host:port` |
| `graphite` | Graphite URL: `https://graphite:8080` |
| `grpc` | Server: `host:port, grpc://host:port or grpcs://host:port` |
| `sql` | Database server: `host or host:<the driver's port>`, such as `host:5432`; Account for `snowflake` |
| `dns` | Name to resolve: `example.com`, the server being `request.server` or the system's resolver; Address to resolve for `PTR` |
| `ntp` | NTP server: `host or host:123` |
| `icmp` | Host: `host` |
| `snmp` | Agent: `host or host:161` |
| `tlscert` | Server: `host or host:443`, or the `starttls` protocol's port |
| `sshfile`, `sshexec` | SSH host: `host or host:22` |
| `localfile` | File, or Directory when `request.path` names the file or `files` reads a directory: under its root, optional |
| `kubernetes` | none: the field is left out, and the page says *This collector names its own target.* |
| `s3file` | Bucket: `the bucket, or s3://bucket` |
| `redis` | Server: `host, host:port or rediss://host:port` |
| `tcp` | Server: `host:port` |
| `ldap` | LDAP server: `host, host:port or ldaps://host:port` |

The probe parameters are rows of a name and a value, like the headers.

- There is a row for each of the collector's own `{{param_<name>}}`
  placeholders, with its name filled in and its value empty. An empty value
  takes the default, which is shown greyed in the box (nothing for a
  placeholder without a default).
- *+ Add parameter* adds a row. Its name box suggests the other parameters
  that the type takes from a probe, with `header_<name>` for each header it
  forwards.
- A blank row is not sent. A row with an empty value is sent empty, which a
  `param_<name>` treats as not given.
- The page marks on its row what `/probe` would refuse: a value with no
  name; `target`, `collector` or `debug`, which the page sets itself; and a
  repeated parameter, `Probe parameter path is given 2 times; give it
  once.`, except for a `header_<name>`, each value of which is forwarded.
  The run is then not sent.
- Choosing another collector builds the rows again and keeps what was typed
  for a name that the new collector also takes.
- A collector that takes no parameters, such as a `dns` collector without a
  placeholder, is offered no rows, and the page says *This collector takes
  no probe parameters.*
- The page reads each collector's form again with its YAML from
  `/debug/collector`, so it follows a reload.

The target is probed as `/probe?debug=true` probes it, and the page says
through which collector: *Probed through status_json, the collector chosen.*

*Copy as*, below the parameters, gives the probe in the form used to request
it from the exporter outside the page. It is built from the current form and
rebuilt as the target and the rows change:

- **URL**: the `/probe` URL. It is absolute and resolved against the page's
  own address, so a path prefix in front of the exporter is kept. It has
  `collector`, `target` and each filled parameter row (a repeated
  `header_<name>` once for each row), URL-encoded:
  `http://exporter:8080/probe?collector=status_json&target=http%3A%2F%2Fapi%3A8080&param_city=sofia`.
- **curl**: `curl -s` and the URL in single quotes.
- **scrape_config**: a Prometheus job for the collector and the target, the
  multi-target job of
  [the `localfile` page](LOCALFILE.md#scraping-with-prometheus). The
  parameters are under `params`, the target is moved into the `target`
  parameter and `instance`, and the exporter is scraped at the host and port
  that the page was opened at. Inside a cluster, use its service's address
  instead. Its `metrics_path` is the literal path, `/my pfx/probe` behind a
  prefix with a space, not `/my%20pfx/probe`, because Prometheus escapes the
  path itself. A collector that names its own target, such as a
  `kubernetes` collector, gets the exporter as the job's target and no
  `target` parameter. Every value is a single-quoted YAML scalar.
- **Debug report URL**: the URL with `debug=true`, the
  [debug probe](#debug-probes).

- The buttons are disabled, with a note that says why, while the target is
  empty or a parameter row is wrong.
- With the playground as a copy of a collector, they probe through the
  collector it was copied from, as configured. `/probe` runs only the
  configured collectors, so the playground's edits are not part of what is
  copied, and the page says so.
- A button shows what it copies in a box, so the text can be selected where
  the browser does not let the page use the clipboard, and says *Copied the
  curl command.* when it did.
- With `web.basic_auth` on, curl gets `-u USER`, which asks for the
  password, and the job gets a commented `basic_auth` to fill in. The
  credential is never on the page.
- *Scrape a static target* offers *Copy as* too: its
  `/static-targets?targets=<name>` URL and the curl command for it.

The series of a run are shown as a table, or as the Prometheus text that a
probe would answer. *Table* and *Text* switch between them, and the browser
keeps the choice. The text is the whole answer, byte for byte. The table has
a row for each sample. A histogram's buckets, `_sum` and `_count`, and a
summary's quantiles, each have their own row, with `le` and `quantile` among
the labels. The columns are `name`, one column for each label name that any
row has (in alphabetical order), `value` as the text writes it (`NaN`,
`+Inf`), and `type` from the metric's `# TYPE`:

- **Sorting**: a click on a column's header sorts by it, and a second click
  reverses the order. `value`, `le` and `quantile` sort as numbers, and a
  row without the label sorts last in either direction.
- **Filtering**: the filter box keeps the rows whose name, `label=value` or
  value contains its text, in any letter case, and *Metric* keeps the rows
  of one metric. The page says how many rows are left, `3 of 12 series`.
- **Lint**: a [metric lint](#metric-lint) finding is shown on the rows of
  the metric it names, in a `lint` column, as its rule, which opens to show
  the advice. A series does not record the rule that produced it, so a
  finding is matched by the metric's name: every row of that metric has it.
  A finding about a metric with no row (a rule that produced no series) is
  listed above the table. The *Lint* section below still lists every
  finding.
- **Size**: at most 2000 rows are drawn, so that a large answer leaves the
  page responsive. Beyond that the page says so, and the filter narrows the
  rows. The text has all of them.

What a target sent is put in the table as text, never as HTML. On a narrow
screen the table scrolls sideways in its own box.

With [OTLP](OTLP.md) enabled, a third tab, *OTLP*, shows what the next
export would send of the run's series. It is there for every kind of run: a
pasted response, a fetched target or a static target's scrape, with a
configured collector or with the playground. The export's own code produces
it, so it shows what the export does and is not a description of it:

- **Resource and scope.**
  - A probe's points are under the exporter-wide resource
    (`otlp.service_name` and `otlp.resource_attributes`) with the
    `collector` and `target` attributes of `otlp.probe_attributes`. The
    playground's points are shown as a probe of it would have them.
  - A static target's scrape is under the target's own resource: its
    `otlp.labels_as_resource` are moved to it, and its name is its
    `service.instance.id` with `otlp.instance_id`. This is shown also when
    the target does not set `export_via_otlp`, which the tab then says.
  - With `otlp.exporter_instance` on, every resource also carries its
    attribute, with the value in force, and a note says what it is.
  - The scope is the name and version of `otlp.scope`.
- **Data points.** There is a row for each point, with its Prometheus name
  and its OTLP name (they differ under `otlp.metric_names: otel`), its kind
  (`gauge`, `sum (cumulative, monotonic)`, `histogram (cumulative)` or
  `summary`), its unit, its attributes and its value: a histogram's count,
  sum and each bucket's own count, or a summary's count, sum and quantiles.
  - A row that the export changed has a badge that opens to show why: a
    family exported as gauges because its bucket counts fall or a counter is
    NaN, `_total` removed, the unit `s` from `_seconds`, a label moved to
    the resource, or an attribute added.
  - A click on a column's header sorts the table, and the filter keeps the
    rows that contain its text. It sorts like the series table and every
    other sortable table of the exporter's and the allocator's pages:
    numbers by value, and text alphabetically with the numbers in it by
    value (`node2` before `node10`), ignoring letter case.
- **Left out.** These are the points that the export would leave out: a name
  exported as two kinds or, under `otel` naming, for two Prometheus names.
  They are shown with the warning that the export logs for them. Only the
  run's own points are checked against one another. Those of other probes
  and static targets in the same export, and the exporter's own series, are
  not part of the run.
- **Request body.** This is the `ExportMetricsServiceRequest`, folded away.
  Its points have the run's time in whole milliseconds, like an export's.
  The export sends it on one line, gzipped unless `otlp.compression: none`.
  - It is shown as a tree. Each object and array folds behind a ▸/▾ button,
    which the keyboard reaches too. A folded node shows how many keys or
    items it has and the value of its `name` or `key`,
    `{…} 2 keys · pue-playground`. A folded attribute shows its key and
    value, `{…} service.name = "pue-playground"`, so the attributes of the
    resource and of each point can be read without unfolding them.
  - Keys, strings, numbers, booleans and `null` each have their own colour
    in either theme.
  - The tree opens four levels below the top, down to each resource's
    attributes and the scope's metrics. *Expand all* and *Collapse all* open
    every node or fold all but the top.
  - A node's contents are built when it is first opened. A body of thousands
    of points therefore shows immediately, and *Expand all* goes one level
    at a time without blocking the page. A node with many items, such as an
    array of 200000 points, fills in a couple of thousand items per frame
    while the page stays responsive. A node that cannot be shown says so.
  - *Copy* copies the body as indented JSON, which the tree is built from.
  - A cumulative point starts at its own time, as at a series' first export.
    The export remembers when each series started, and a run neither reads
    nor changes that.
- **Log records**, with [`otlp.logs`](OTLP.md#the-exporters-own-log) on.
  Each line that the run logged is shown as the record that the log export
  would make of it: its time, severity, body and attributes.
  - Records below `otlp.logs.level` are marked as not exported. The
    `ExportLogsServiceRequest` of the rest is shown like the metrics' body,
    with one tree per request when there are more records than one request
    holds.
  - A probe logs a failure as `probe failed`, where a run says `debug run
    failed`, and logs a repeated failure once and then every five minutes.
  - With `otlp.traces` on, each record shows the trace and the span that it
    would be linked to. A run's record is never linked
    ([Linking metrics, logs and traces](OTLP.md#on-the-debug-page)).
- **Spans**, with [`otlp.traces`](OTLP.md#traces-of-slow-requests) on. These
  are the spans that the trace export would make of the run, produced by the
  export's own code: the probe's or the scrape's span, each stage's and each
  request's.
  - They are shown as a tree, with each span's start, duration, a bar of its
    time within the run's time, its status and its attributes.
  - The `ExportTraceServiceRequest` that they would be sent in is shown like
    the metrics' body.
  - The tab says whether a probe or a scrape that took as long and ended in
    the same way as the run would be exported, and by which rule, or why no
    rule would keep it: `otlp.traces.threshold`, `otlp.traces.errors`,
    `otlp.traces.parent_based` (a run has no caller's `traceparent`) and
    `otlp.traces.sample_ratio`, with the randomness of the run's trace id
    compared against the threshold. `otlp.traces.max_per_second` is not
    applied, and the line says so for a trace that is kept.
  - The run's own trace is never exported. With `otlp.traces.propagate`, the
    run's requests carry its `traceparent`, which their request lines show.
    It is unsampled (`00`) and has no `th` in its `tracestate`, because the
    trace is never exported.

Nothing of a run reaches the export: nothing is queued, no start time is
remembered, the export's self-metrics and status are unchanged, and nothing
is sent.

With `otlp.enabled` the page has a separate panel below *Output*, *OTLP
endpoints*. It is present before anything has run, because testing the
endpoint is independent of a run.

- At `full` it offers *Test the metrics endpoint*, with `otlp.logs` also
  *Test the logs endpoint*, and with `otlp.traces` also *Test the traces
  endpoint*.
- A test sends one empty export (`{"resourceMetrics":[]}`,
  `{"resourceLogs":[]}` or `{"resourceSpans":[]}`) to the endpoint that the
  configuration names, with `otlp.headers`, `otlp.tls`, `otlp.compression`
  and `otlp.timeout`. It is sent once and never retried.
- The page shows the status, the time taken, and the full error of the
  connection, of TLS or of a timeout.
- It shows the headers sent. Each header of `otlp.headers` is shown by name
  only, with its value as `<redacted>` whatever the header is called. The
  `Content-Type` and `Content-Encoding` that the export sets are shown as
  sent.
- It shows the answer's headers and body, its `partialSuccess`, and how an
  export would treat the answer: as a success, a retry or a refusal.
- A test counts in none of the export's self-metrics and changes nothing
  that the status page shows.
- One test runs at a time. Another one requested meanwhile is answered
  `429`.
- At `edit` the buttons are disabled.

The playground is restricted beyond reading the response, because what it is
given runs on the server. [`--web.debug.dangerous`](#dangerous-debug-mode)
lifts these restrictions:

- **Python is never run.** A collector with `transform.type: python` or a
  `pre_script` is refused, at `full` as at `edit`. Put it in the
  configuration and run it by name.
- **Its request block is inert**, whatever its type. No run makes a request
  with it, so a `sql`, `tlscert` or `localfile` playground reaches no
  database, host or file. A pasted response is decoded as it is, and a
  target is fetched, or a static target scraped, only through a configured
  collector.
- **No file of the server's.** Every request key that ends in `_file` or
  `_files`, of any type, is refused: `request.bearer_token_file`, the
  `request.tls` files, an `sshfile` collector's `known_hosts_file`, a `grpc`
  collector's `protoset_file`. A `request.login`, which reads its secrets
  from files, is refused too. The refusal names them in the order the
  collector has them. A pasted response needs no credential, and a fetched
  target gets the configured collector's.

**Limits.** A run may take as long as a probe: the timeout on the page, sent
as Prometheus's scrape timeout, within `--probe.default-timeout`, or a
static target's interval. At most 4 runs of the page are in progress at
once; one more is answered `503`.

**Side effects.** A run fills no cache and is answered from none. It reads
only what a debug probe reads of the cache: a configured collector's stale
result and the response it would
[revalidate](#revalidating-expired-responses). It counts in no collector's
self-metrics, except the
[Python worker series](SELF-METRICS.md#python-workers) of a configured
collector's script. It logs one `debug run served` line at `info`, which the
OTLP log export does not take.

The page is behind `web.basic_auth` when that is configured.
[Debugging](AUTHENTICATION.md#debugging-collectors) describes how a run is
protected from being sent by another site.

The page posts each run to `POST /debug/run` as JSON, which a script can do
too:

```sh
curl -s http://exporter:8080/debug/run \
  -H 'Content-Type: application/json' -H 'X-Debug-Run: run' \
  -d '{"collector": "name: x\nrequest: {type: http}\ndecoder: {type: json}\ntransform: {type: jq}\nmetrics: [{name: up, type: gauge, expression: .up}]\n", "body": "{\"up\": 1}"}'
```

The editor reads a configured collector from
`GET /debug/collector?name=<name>`, which answers it as JSON:

- `name`;
- `yaml`, the YAML as the page shows it;
- `from_defaults`, the keys it took from its file's defaults;
- at `full`, `static_targets`: the static targets scraped through it, each
  with a `name` and its `target` as the list shows it, or `[]` for none;
- `form`, what the page asks for to run it: `request_type`; `answer`, the
  body's placeholder; `content_type` and `http_answer`, whether it asks for
  a `Content-Type` and for a status and headers; `target_label` and
  `target_hint`, or `no_target`; `params`, its own probe parameters, each
  with a `name` and its `default`; and `other_params`, the names of the
  others it takes. `params` and `other_params` are each left out when it
  takes none.

A name that the configuration does not have is answered `404` with `error`.

**What a run sends.** A run sends the collector and one response:

- the collector: `collector` (the YAML) or `configured` (a name);
- the response, as one of:
  - `body` with `content_type`, `status` and `headers`;
  - `target` with `params` and, for `collector`, `request_from`: the
    configured collector to fetch through, which the page fills with the one
    the playground was copied from;
  - `static_target`.

`headers` and `params` are objects of names. Each name has a string, or a
list of strings for a name given more than once,
`{"X-Trace": ["one", "two"]}`. A pasted response then has the header once
for each value, with at most 100 values in all. A `header_<name>` parameter
forwards each value. Any other parameter given twice is refused, as `/probe`
refuses it.

**What it answers.** The answer is `200` with:

- `ok`, and `verdict` (what a probe would have answered);
- `series` (Prometheus text);
- `samples`, the same series one sample at a time as the table shows them,
  each with its `name` (a histogram's or summary's with its suffix),
  `metric` (the metric's name, as a lint finding names it), `type`, `labels`
  and `value` (as the text writes it, so `NaN` and `+Inf` are strings);
- `requests`, `response`, `stages` and `notes`;
- `lint`, the findings of the [metric lint](#metric-lint), each with its
  `collector`, `rule`, `metric` and `message`;
- `report`, the text report;
- `otlp`, only with OTLP enabled: what the *OTLP* tab shows.

`otlp` has `metric_names`, `resource` (each a `key` and `value`),
`resource_notes`, `scope`, `points` (each with `prometheus`, `name`, `kind`,
`unit`, `attributes`, `value` and `notes`), `left_out` (each with its
`reason`), `clashes`, `notes` and `body`. It also has:

- with `otlp.logs`, `logs`: `level`, `records` (each with `time`,
  `severity_number`, `severity_text`, `body`, `attributes`, `exported`,
  `note` and, with `otlp.traces`, `trace_id`, `span_id` and `link`), `notes`
  and `body`;
- with `otlp.traces`, `traces`: `threshold`, `errors`, `parent_based`,
  `sample_ratio`, `max_per_second`, `took`, `failed`, `exported`, `reason`
  when it would be exported (as `sampling.reason` names it), `why`, `spans`
  (each with `name`, `kind`, `depth`, `start_ms`, `duration_ms`, `status`
  and `attributes`), `notes` and `body`.

A refused collector is answered `422` with `error`, a malformed run `400`, a
run that is too large `413`, and a fetch or a scrape below `full` `403`.
With `--web.debug.dangerous` an answer also has `dangerous: true`.

The endpoint test is `POST /debug/otlp-test` with `X-Debug-Run` and a JSON
body `{"signal": "metrics"}`, `{"signal": "logs"}` or
`{"signal": "traces"}`. It is answered with `ok`, `signal`, `endpoint`,
`sent`, `body`, `status`, `took`, `failure`, `outcome`, `partial_success`,
`headers`, `response` and `notes`. Each refusal has `error`: `403` below
`full`; `409` with the export off, or for `logs` with `otlp.logs` off or
`traces` with `otlp.traces` off; and `429` while another test runs.

### Dangerous debug mode

`--web.debug.dangerous`, off by default, lets the playground do what a
configured collector does. It is for writing a collector whose script or
files cannot otherwise be tried without putting it in the configuration. It
needs `--web.debug=full` and is refused at startup without it (exit status
2). The exporter logs at `warn` when it starts with it on. The playground
then:

- **Runs Python**, both a `python` transform and a `pre_script`, as a
  configured collector's script runs. It uses the same worker pool and
  [sandbox](PYTHON.md), which blocks opening files and sockets but does not
  stop a script written to escape it. It is held to the same limits
  (`limits.script_timeout`, `max_script_memory`, `max_output_bytes`,
  `--python.max-workers`) and to the run's deadline. Its runs count only in
  the pool-wide [Python worker series](SELF-METRICS.md#python-workers),
  under no collector's name. Its worker is stopped once it has run, which
  those series count as a stop for a reload.
- **Names the server's files**: any request key that ends in `_file` or
  `_files`, and `request.login`, are accepted.
- **Makes its own request**: *Probe a target* with a playground that is not
  a copy of a configured collector fetches the target through the
  playground's own request block, as a probe of a configured collector
  would, with the `allowed_targets`, `denied_targets` and credentials that
  it writes. The playground is then checked as a load checks a request, and
  the files that a request needs are required.
  - This is what makes its files useful: a `localfile` playground reads a
    file under the `root` it names, a `bearer_token_file` or a TLS file is
    read for the request it is sent with, and a `sql` playground connects to
    the database it names.
  - A copy of a configured collector still fetches through that collector,
    whose credentials the copy does not have, and a pasted response still
    makes no request.
  - *Copy as* copies no probe of the playground's own request, because
    `/probe` runs only the configured collectors.

`${NAME}` references are still left as written. The environment is where a
deployment keeps its secrets, and a collector written on the page can name a
file instead.

The page then shows a red banner in place of its usual note, the bar of
every page carries a *Dangerous debug mode* badge, and `/debug/run` answers
`dangerous: true`. Whoever can open the page can run code on the server,
read any file that the exporter's user can read through a playground's
request, and reach any host that the exporter can reach. Use this mode on
your own machine, or in a debugging session that no one else can reach, and
never on a shared deployment
([Debugging](AUTHENTICATION.md#debugging-collectors)).

### Debug probes

At `full`, add `&debug=true` to a probe and read the report in a browser or
with curl:

```sh
curl 'http://exporter:8080/probe?collector=app_json&target=https://api.example.com&debug=true'
```

```text
Debug probe of collector "app_json", target https://api.example.com
Took 184ms. A probe would have answered 502: metric queue_len failed: ...
A debug probe skips the response cache, shares no trip, records no self-metric but http_exporter_debug_runs_total and exports nothing over OTLP.

Requests
  1. GET https://api.example.com/v1/status?tenant=<redacted>&view=full -> 200 OK in 162ms
     Accept: application/json
     Authorization: <redacted>

Response
  Status 200 OK
  Headers
    Content-Type: application/json
  Body: 3174 bytes
    {"workers":[{"name":"a","state":"1"}, ...

Stages
  http         ok            162ms  status 200, 3174 bytes
  decode       ok              3ms  json
  transform    failed         10ms  metric queue_len: metric "queue_len" value is missing

Transform
  ...

Logs
  level=ERROR msg="probe failed" collector=app_json ...

Lint
  [counter-total] metric "jobs_done": a counter whose name does not end in _total is written as jobs_done in the Prometheus text format and as jobs_done_total in OpenMetrics, ...

Metrics a probe would have served
  none
```

The report lists:

- **Requests:** every request that the trip sent, including retries and
  redirects, with its headers and how it ended.
  - A redirect is listed with the headers it was sent. A redirect to a host
    that is neither the request's origin nor in
    [`redirect_trusted_hosts`](REQUESTS.md#what-a-followed-redirect-carries)
    also has a line that names the headers it was not sent: `not sent:
    Authorization, X-Api-Key — the redirect leads to a host that is not the
    origin the request was made to and is not in
    request.redirect_trusted_hosts`.
  - A `grpc` call is listed with its method, metadata and status code. A
    `localfile` read, which sends nothing, is listed as the file it reads. A
    URL longer than 512 bytes is listed by its first 512.
  - For an `http` or `graphite` request that is retried, the list ends with
    how: `Retries: up to 2, 1s apart, on a failed connection and on an
    answer of 404, 408, 425, 429, 5xx that is not accepted.`, from the
    collector's [`retry`](REQUESTS.md#retries) or the static target's.
  - Under [`retry.retry_after`](REQUESTS.md#waiting-as-retry-after-asks) the
    line adds that the `Retry-After` of such an answer replaces the backoff.
    Each retried request has a line with the wait taken and why, `then
    waited 3s, as Retry-After asked`, or with why it was not retried: `not
    retried: its Retry-After asks for 2m0s, longer than the 9.2s left of the
    probe's time`.
  - A request that waited for its turn under
    [`request.rate_limit`](REQUESTS.md#rate-limiting) has a line with how
    long and why: `sent after waiting 500ms for its turn under the rate
    limit of 2 requests per second for http://legacy.example:80`. So has one
    that waited under an entry of
    [`rate_limits`](REQUESTS.md#rate-limits-per-host): `sent after waiting
    200ms for its turn under the rate limit of 5 requests per second of
    rate_limits entry "billing_api" for api.billing.example`.
- **Response:** the status, the headers (the first 100, each value by its
  first 1,024 bytes) and the body, up to 64 KiB, as the target sent them.
  The `Content-Type` is the target's own, not the `charset=utf-8` that the
  rules see after the body is [converted](#character-encodings). A body in
  another encoding is listed with its size as sent and what it was converted
  from, `Body: 3174 bytes in windows-1251, converted to UTF-8 before
  decoding and shown here as UTF-8`, and is shown as the same text in UTF-8,
  because the report is UTF-8. A body that is not text is shown only by its
  length, and a directory as its files.
- **Stages:** each stage with how long it took and how it ended. A stage
  whose `error_handling` carried on says so.
- **Transform:** the series that each metric got, the rules that got none,
  and the rules that carried on without some of their series. Each of the
  last is listed separately with how many and its first error, and with its
  expression where several rules export its metric name. A `prometheus` rule
  without a name is listed as `rule without a name`, and a metric's name
  longer than 200 bytes by its first 200.
- **Logs:** everything that the trip logged at any level, whatever
  `--log.level` is, including what a Python script printed. It also shows
  the memory that the transform used, beside the limit that it is held to:
  - for the expressions of a `jq` or `yq` transform, the peak that they held
    ([`limits.max_expression_memory`](#memory)), measured with garbage
    collections that a normal scrape does not spend;
  - for a Python script, the most address space that its worker has had
    ([`limits.max_script_memory`](PYTHON.md#how-scripts-run)).
- **Lint:** the advice of the [metric lint](#metric-lint) on the collector
  and on the series that the run produced, one finding per line, or `none`,
  and the rules that the collector's `lint.ignore` silences. It changes
  nothing of what a probe answers.
- **Metrics:** the exposition that a probe would have served and, before the
  report, the status that a probe would have answered. Where
  `cache.stale_if_error` would have answered with the last good result, the
  report says so.
- **Backoff:** where [`failure_backoff`](#backing-off-from-failing-targets)
  is backing off from the target, a line under the first two says so, with
  the failures and the time of the next try: `The target is backing off
  after 3 consecutive failed scrapes (failure_backoff): probes and scrapes
  fail without contacting it until 2026-10-23T09:14:05Z. This run contacted
  it all the same, and changes nothing of that.` The debug probe contacts the
  target whatever the backoff says. Its outcome does not start, lengthen or
  end a backoff.

A target can make most of this arbitrarily long: the URL that a redirect led
to, a header's value, the message of a gRPC status, a metric's name. A
report is read by a person and is built whole in memory. Everything in it
except the body therefore has a bound, and whatever is cut says so with its
full length, as a long [error](LOGGING.md#repeated-failures) does:

```text
  2. GET http://10.0.0.7/LLLLLLLL... (921645 bytes) (redirect) -> 200 OK in 3ms
    X-Trace: hhhhhhhh... (1046528 bytes)
    ... (5003 headers)
    queue_depth_aaaaaaaa... (1048577 bytes): 1
```

- **A line** longer than 8,192 bytes is shown by its first 8,192 bytes, cut
  between two characters, followed by `... (N bytes)`, the length of the
  whole line. This applies to every line of the report except the body's:
  the first line with a long target, a log line, or a line of the
  exposition. The bound is larger than the bounds that already apply. An
  error cut at its 2,000 bytes therefore keeps its own length in the line
  that quotes it, and the 4,096 characters that the log keeps of what a
  script printed are shown whole.
- **The body** keeps its own bound, 64 KiB, and its lines are shown as they
  are, however long.
- **A request's URL** is shown by its first 512 bytes and **a metric's
  name** in the Transform section by its first 200, as an error shows them,
  each with its length. How the request ended and how many series the metric
  got therefore remain on the line.
- **A header's value** is shown by its first 1,024 bytes and its length.
  **The headers** of a request or a response are listed up to the first 100,
  in the order of their names, followed by how many there are.
- **A line of the exposition** that is cut is no longer valid exposition, so
  the section then starts with `Lines longer than 8192 bytes are cut below
  (3 of them), so this is not valid exposition as it stands; a probe serves
  them whole.` Only the report is cut. The answer of the probe itself, the
  line that says what a probe would have answered, the response cache, the
  self-metrics and what is exported over OTLP are the same as without a
  debug probe.

What the report redacts (listed below) is withheld before anything is cut,
and no cut ends inside a `<redacted>`. The number of lines is bounded by the
collector's own limits. The Transform section has a line for each metric
name and the exposition one for each series, up to `limits.max_metrics`. A
directory has a line for each file read, up to `request.max_files`, and one
for each file skipped.

A [static target](STATIC-TARGETS.md#debugging-a-static-target) is debugged
at `/static-targets?debug=<name>`, and the [debug page](#the-debug-page)
shows the same report for its runs.

The report always answers `200` with `text/plain`, whatever the probe would
have answered. It takes the same parameters as the probe. For `debug`,
`true`, `1` or an empty value turn it on, and `false` or `0` leave the probe
as it is. Any other value is answered `400`.

A debug probe always goes to the target and leaves nothing behind:

- **Cache.** It is not answered from the response cache and does not fill
  it. It only reads the stale result that a probe would have answered with,
  and the response that a probe would
  [revalidate](#revalidating-expired-responses), and sends the same
  conditional request without refreshing the entry.
- **Sharing.** It does not share an identical probe in flight.
- **Export and self-metrics.** It is not exported over OTLP and not counted
  in the self-metrics, except `http_exporter_debug_runs_total`. That is the
  meaning of the report's line "records no self-metric but
  http_exporter_debug_runs_total". One more thing is counted: a Python
  script that it runs is run by the collector's workers, and the
  [Python worker series](SELF-METRICS.md#python-workers) count what the
  workers did, including a debug probe's runs.
- **Log.** Its failures go to its report, not to the exporter's log. The log
  records only one `probe debug report served` line at `info` (for a debug
  scrape, `static target debug report served`), on stderr only: the
  [log export](OTLP.md#the-exporters-own-log) does not take it.
- **Limits.** The target's
  [`allowed_targets` and `denied_targets`](REQUESTS.md#restricting-targets),
  [`max_concurrent_probes` and `--probe.max-concurrent`](#limiting-concurrent-probes)
  and [the probe's deadline](#probe-deadlines) apply as they do to any
  probe.
- **Panics.** A trip that panics (a bug in a request type, a decoder or a
  transform) is reported as the `500` that a probe would have answered,
  `probe failed: the scrape panicked; the exporter's log has the panic`. The
  panic's message is in the report's log, with a URL's userinfo and anything
  else in it that looks like a credential withheld. The panic is logged with
  its stack, and its slot is released as after any other trip.

Below `--web.debug=full`, `debug=true` is answered `403` before the target
is contacted. When `web.basic_auth` is configured, a debug probe needs the
same credentials as a probe. The report and the page's JSON both redact:

- every query value in the errors that quote a URL. In the requests listed,
  every query value is masked except those that the collector's
  configuration sets literally in its file: a value of `request.query`
  without a placeholder (`view=full` above), a `graphite` collector's
  expressions without placeholders, `from`, `until` and `format=json`, and a
  `kubernetes` listing's `limit` and selectors. These stay masked:
  - a value that a probe parameter or a placeholder gives
    (`tenant={{param_tenant}}` above);
  - a value that a `${NAME}` reference expanded to, which comes from the
    environment;
  - the target's own query, and a static target's own keys;
  - the API server's `continue` token;
  - the value of any parameter whose name looks like a credential's (below);
  - a pair next to a `;`, which a server may split.

  The query is otherwise shown as it was sent: each pair in its position and
  as it was written, including a pair with a `;` or a malformed escape
  ([how a query is masked](LOGGING.md#repeated-failures));
- a URL's userinfo;
- the values of request and response headers whose names look like
  credentials ([the rule](LOGGING.md#repeated-failures) that the logs
  follow). That is a name with a word that contains, in any letter case,
  `auth`, `cookie`, `token`, `secret`, `password`, `passwd`, `passphrase`,
  `passcode`, `key`, `session`, `signature`, `credential` or `jwt`, unless
  the word is an ordinary one that only contains it, such as `author`,
  `authority`, `monkey`, `keyboard` or `tokenizer`. It is also a name with
  `sig`, `pwd`, `pw`, `pass`, `sid`, `hmac` or `otp` as a whole word, as in
  `X-Sig` or `X-Db-Pwd`. A query parameter's name also counts with `code`
  (OAuth's authorization code) or `ticket` (a CAS ticket) as a whole word;
- the value of a path parameter whose name would count for a query
  parameter, `/app;jsessionid=<redacted>`, in every URL shown;
- the URL of a header that carries one (`Location`, `Content-Location`,
  `Refresh` and each `<…>` of `Link`), masked like a request line's: every
  query value and a credential's path parameter. A redirect's
  `Location: /app;jsessionid=<redacted>?code=<redacted>` therefore shows
  where it led without what it carried.

Request bodies are not shown. A secret under any other header name, or in
the response itself, is shown as the target sent it.

## Readiness

`/health` answers `200` for as long as the process runs. `/ready` answers
`200` when the exporter should be sent probes and `503` when it should not,
with one `not ready:` line per reason:

- With `otlp.unready_after_failures` set: that many consecutive OTLP exports
  to the current endpoint failed, including retries (see
  [Delivery](OTLP.md#delivery)). The exporter is ready again once an export
  succeeds. The setting is off by default, because an exporter whose exports
  fail still answers probes, and is at most 1000. It counts only the exports
  of metrics: failing exports of the exporter's own log
  ([`otlp.logs`](OTLP.md#the-exporters-own-log)) and of traces
  ([`otlp.traces`](OTLP.md#traces-of-slow-requests)) never make the exporter
  unready.
- The exporter received `SIGTERM` or `SIGINT` and is waiting for
  `--web.shutdown-delay` to pass before it stops; see
  [Shutting down](#shutting-down). It never becomes ready again.
- In [allocator mode](#configuration-from-the-target-allocator), no bundle
  from the allocator is in force yet: `not ready: no configuration yet:
  waiting for the allocator`. The exporter is ready once the first one is.

Neither endpoint needs credentials, so the reasons never include an error's
text. The log and the [self-metrics](SELF-METRICS.md) have the details.

A rejected reload, of the configuration or of the static target file, leaves
the exporter ready: it keeps answering probes from the configuration in
force. Every replica rejects the same ConfigMap edit, so making them unready
would leave the Service with no endpoints and fail every probe that the
exporter could still answer. The rejection is reported in other ways:

- `http_exporter_config_last_reload_successful` reads `0` until a reload of
  the file is accepted. Alert on this (see
  [Configuration reloads](SELF-METRICS.md#configuration-reloads)).
- The reload logs an `ERROR` line with the reason.
- `/-/reload` answers `500`.

In Kubernetes, a pod that is not ready is removed from its Service, and
Prometheus stops probing through it. `otlp.unready_after_failures` therefore
removes a pod from its Service while its OTLP endpoint fails, which also
stops Prometheus from probing through it. Set it only where OTLP delivery is
the pod's job.

### Health checks

The image has no `curl` or `wget`, so the exporter checks itself.
`--healthcheck` sends one `GET /ready` to the exporter that listens at
`--web.listen-address`. It exits `0` when the answer is `200` and `1`
otherwise, and logs one JSON line to stderr with the reason: the status and
the reasons from `/ready`, or the error of a failed connection.
`--healthcheck=health` requests `/health` instead, for a liveness check,
which stays `200` while the exporter is not ready.

```sh
prometheus-universal-exporter --healthcheck --web.listen-address=:8080
prometheus-universal-exporter --healthcheck=health --web.listen-address=:8080
```

- A listen address with no host, or with a wildcard (`0.0.0.0`, `[::]` or
  `[::ffff:0.0.0.0]`), is requested on `127.0.0.1`. The exporter's `[::]`
  listener answers IPv4 too, and listens on IPv4 only on a host without
  IPv6, where `[::1]` could not be reached.
- A name or a specific address is used as it is, including an IPv6 zone
  (`[fe80::1%eth0]:8080`), and a named port such as `:http` by its number.
- The check gives up after 5 seconds. It starts no server, reads no file and
  loads no configuration, so it takes a few milliseconds.
- The other flags are checked as at a start, so the container's own
  arguments can be given with it, but only the listen address is used.
- It sends no credentials, because `/health` and `/ready` never ask for
  them, even with `web.basic_auth` on, and it ignores `HTTP_PROXY`.

The image's `HEALTHCHECK` runs `--healthcheck` at `:8080`, the address its
`CMD` gives, so `docker ps` shows the container `healthy` while the exporter
is ready. A container started with another `--web.listen-address` needs a
health command of its own, such as `docker run --health-cmd
'/bin/prometheus-universal-exporter --healthcheck --web.listen-address=:9115'`,
and one running the [target allocator](ALLOCATOR.md#running-it) from the image
needs `--no-healthcheck`. Kubernetes ignores `HEALTHCHECK`; the Helm chart's
probes GET the endpoints, or run the check with
[`healthProbes.mode: exec`](../charts/prometheus-universal-exporter/README.md#probes).

## Shutting down

On `SIGTERM` or `SIGINT` the exporter first waits for `--web.shutdown-delay`
(0 by default). During the delay it still answers probes, but `/ready`
answers `503` (`not ready: the exporter is shutting down`), so a load
balancer or a Kubernetes Service takes it out of rotation before it stops
listening. Kubernetes takes a few seconds to remove a terminating pod from
its Service. Without the delay, probes that arrive in that gap are refused,
and Prometheus records failed scrapes on every rollout. The Helm chart sets
the delay to 5 seconds. During the delay, static targets are still scraped
and the [OTLP export](OTLP.md#shutting-down) keeps running.

After the delay, the exporter:

1. stops accepting connections;
2. cuts short the [static target](STATIC-TARGETS.md) scrapes in flight;
3. lets the probes in progress finish for up to `--web.shutdown-timeout`
   (15 seconds by default, long enough for a typical 10s scrape timeout);
4. makes the [last OTLP export](OTLP.md#delivery) when OTLP is enabled. With
   [`otlp.traces`](OTLP.md#traces-of-slow-requests) it is followed by the
   last export of the waiting spans, and with
   [`otlp.logs`](OTLP.md#the-exporters-own-log) by the last export of its
   own log, within the same `otlp.timeout`;
5. exits `0`.

The static target scrapes are not waited for. Their results are served only
by this exporter, which is stopping, so a target that does not answer would
only delay the shutdown. A scrape that is cut short publishes nothing and
logs nothing above `debug`. The target's last result stands, and the last
OTLP export goes without what that scrape would have queued.

Nothing else that the exporter runs on its own delays the shutdown. The
configuration watch stops when the shutdown begins. The
[allocator](ALLOCATOR.md) client, which keeps reporting that the replica is
shutting down, stops once the probes are done.

The log says what the shutdown waits for, and for how long at most:

```json
{"level":"INFO","msg":"shutting down: waiting for 2 probes in progress, at most 15s; a second signal exits at once","probes":2,"other_requests":0,"shutdown_timeout":"15s"}
{"level":"INFO","msg":"shutting down: still waiting for 1 probe in progress, 10s left","probes":1,"other_requests":0,"time_left":"10s"}
```

`other_requests` counts the requests in progress that are not probes, such
as a scrape of the self-metrics. The shutdown waits for those too.

- While it waits, the exporter logs every 5 seconds what it still waits for.
- With nothing in progress it logs `shutting down: no probes in progress;
  exiting`, or that it sends the last OTLP export, and does not wait.
- When the timeout runs out with probes still in progress, it logs an error
  that says it gave up on them and closed their connections. Prometheus
  records those as failed scrapes. The exporter still exits `0`: it stopped
  as requested, and a probe that was cut off is a failed scrape for
  Prometheus, not a failure of the exporter.

Keep the timeout at least as long as the longest scrape timeout of the
monitors that probe the exporter, so that a rollout does not cut probes off:

```sh
prometheus-universal-exporter --web.shutdown-delay=5s --web.shutdown-timeout=30s
```

In Kubernetes the pod must also be allowed to run that long. Its
`terminationGracePeriodSeconds`, 30 by default, must cover a preStop hook's
sleep, the delay, the timeout and the last OTLP export. The Helm chart's
`server.shutdownDelay` and `server.shutdownTimeout` set the flags, its
`preStop` sets the hook, and they raise the grace period to match.

A second `SIGTERM` or `SIGINT` during that time, including the delay, ends
the process immediately, without waiting for the probes or the export. This
covers the second Ctrl-C of an impatient operator and a supervisor that
signals twice.

- The exporter first logs one line, `second signal: exiting now, 1 probe in
  progress left unfinished`.
- The process is then ended by that signal, as an exporter that did not
  catch it would be: a shell reports `130` for `SIGINT` and `143` for
  `SIGTERM`, and Kubernetes reports the same exit codes.
- An exporter started with the signal ignored, as a script's background job
  is with `SIGINT`, exits with that same status instead, a second later.
- `SIGTERM` and `SIGINT` behave the same throughout.
- A `SIGHUP` during that time [reloads](#reloading-on-demand) as at any
  other time and does not end the process.

## Sizes

Every setting that is a number of bytes (`max_response_bytes`,
`limits.max_output_bytes`, `max_total_bytes`) takes either a number of bytes
or a number with a unit:

| Written | Bytes |
| --- | --- |
| `1048576` | 1048576 |
| `1e6`, `1000000.0` | 1000000 |
| `512KiB`, `512 KiB` | 524288 |
| `10MB` | 10000000 |
| `64MiB` | 67108864 |
| `1.5GiB` | 1610612736 |

`kB`, `MB`, `GB` and `TB` are powers of 1000. `KiB`, `MiB`, `GiB` and `TiB`
are powers of 1024. `B` or no unit means bytes. The unit is case
insensitive, and a fraction of a unit is rounded down to whole bytes.
Anything else fails to load, with an error that names the value and its
line:

- text that is not a size, such as `lots`;
- a negative size, such as `-1`;
- a fraction without a unit, such as `1.5`, because there is no half byte;
- space around the size in quotes, such as `" 10MiB"`;
- a size of 2^63 bytes (`8388608TiB`) or more, which is more than a size can
  hold.

The [schema](#editor-support) flags the same spellings except the last: the
range is checked when the configuration loads.

**Minimum sizes.** The response limits, `request.max_response_bytes` and
`limits.max_response_bytes`, are at least 1 KiB when they are set. A size
without a unit is a number of bytes, so a limit under a kibibyte is one
whose unit was left out: `max_response_bytes: 10`, meant as 10 MiB, would
fail every scrape. A smaller limit fails to load, and the schema flags it,
as for `limits.max_output_bytes` below.

`limits.max_output_bytes` bounds an answer of a
[Python worker](PYTHON.md#how-scripts-run) and is 1 MiB when it is left out
or `0`. When it is set, it is at least 38 bytes, the answer of a script that
emits no metric. A smaller value (`26`, `2.6e1`, `"26"`, `26B`, `0.02KiB`)
fails to load. The schema flags it too:

- written as a number, from 1 to 37, however YAML writes the number;
- written as text, in quotes or with a unit, by the digits of its number.
  `20B`, `"26"` and `0.01KiB` are therefore flagged. `0.5B`, which rounds
  down to no byte, is `0`, the default, for both the exporter and the
  schema.

`limits.max_script_memory` (at least 32 MiB when it is set) and
`otlp.batch_max_bytes` (at least 64 KiB in a block that is switched on) are
held to their minimum in the same way.

**Numbers without quotes.** A number of bytes written without quotes is the
whole number it equals, however YAML writes the number. `1e6`, `1E6`, `+1e6`
and `1000000.0` are a million bytes, and `2.5e8` is 250000000. The same is
true at every other key that takes a whole number, such as
`limits.max_metrics`, and for the schema, which receives the number and not
how it was written.

- The number is read by its digits, not as the nearest floating-point
  number, so `9007199254740993.0` is that many bytes.
- What is not a whole number of bytes is refused, by the schema too: a
  number with a fraction (`1.5`, `1e-1`) and `.inf`.
- A whole number that no size can hold is refused for what it is, however it
  is written: `-1e3` and `-1.0` as negative, like `-1000`, and `1e19` and
  `9223372036854775808.0` as too large, like `9223372036854775808`.
- In quotes a number is text, and text must be a whole number of bytes or a
  number with a unit. `"1e6"` and `"1.0"` are refused, by the schema too.
- The schema cannot check a number's range, or digits beyond the seventeen
  or so that a floating-point number holds. A fraction written that far out
  (`1.00000000000000000001`) reaches the schema as the whole number, and is
  refused when the configuration loads.

**Over the limit.** A response over `max_response_bytes` fails the scrape.
When the target declared its size in `Content-Length`, the response is
refused before the body is read, with `response size 5000 exceeds limit
1024`. When it did not, the scrape fails with `response size exceeds limit
1024` once reading passes the limit. The limit counts the decompressed
answer. A `grpc` collector with `descriptors: reflection` holds the files of
its reflection query to the same limit, or to 10 MiB when the limit is
smaller ([gRPC](GRPC.md#descriptors)).

## Dry run

`--dry-run` answers "would this start?" without starting anything. It runs
the same validation as startup, prints a JSON report on stdout, and exits:

| Exit status | Meaning |
| --- | --- |
| `0` | Every check passed; the exporter would start with these files and flags. |
| `1` | At least one check failed; the report says which, and why. |
| `2` | The command line itself could not be parsed, or a flag is invalid — a `--web.listen-address` that is not `host:port`, such as a bare `9115`, a negative duration or limit, a `--config.watch-interval` that is not positive with `--config.watch` — so nothing was checked. |

```sh
prometheus-universal-exporter --dry-run --config.file=config.yaml
prometheus-universal-exporter --dry-run \
  --config.file=config.yaml --static-targets-file=static-targets.yaml
```

It checks everything that startup checks, including that every expression
compiles (see
[Checked when the configuration loads](#checked-when-the-configuration-loads)).

The advice of the [metric lint](#metric-lint) on the configuration is listed
under `details.lint` of the `config` entry, each finding with its
`collector`, `rule`, `metric` and `message`. It is also logged at WARN as
`metric lint`, with the same content as `collector`, `rule`, `metric` and
`advice`. It never fails the check, so a configuration with advice exits
`0`:

```json
"lint": [
  {"collector": "legacy", "rule": "unit-suffix", "metric": "poll_latency_ms", "message": "the unit ms is not a base unit; ..."}
]
```

The `config` entry's `details` also list:

- **`details.deprecations`**: a deprecated spelling. It does not fail the
  check, and it is logged.
- **`details.collector_files`**: the [collector files](#collector-files)
  that the configuration read. A collector name defined twice fails the
  check.
- **`details.collector_defaults`**: each collector that took keys from its
  file's [defaults](#defaults-for-the-collectors-of-a-file), by name, with
  the keys it took in the order of the block, such as
  `{"payments_api": ["request.allowed_targets", "limits.max_metrics"]}`. Its
  values are the collector's, wherever they are written, and
  `request_policies` shows them as they will be applied.
- **`details.request_policies`**: each collector that sets
  `allowed_targets`, `denied_targets`, `accept_status`, `accept_codes`,
  `retry.statuses`, `retry.retry_after`, `pagination`, `login` or
  `rate_limit`, by name, with those settings as they will be applied:
  - `2XX` as `2xx`, and `not_found` as `NOT_FOUND`;
  - `retry.statuses` as `retry_statuses`, including `[]`;
  - `retry.retry_after` as `retry_after: true`;
  - a `pagination` with its defaults, including `max_pages: 10`;
  - a `login` with the `header` and `prefix` that its token is sent with;
  - a `rate_limit` with its `burst`.

  A list that lets through more or less than intended, a pagination that
  asks the target for more pages than intended, or a token sent where it was
  not intended to go is therefore visible before it is deployed. A login is
  shown with the files that its secrets are read from, never with their
  content, and a field or endpoint parameter whose name looks like a
  credential's is shown without its value
  ([Logging in to a target](AUTHENTICATION.md#logging-in-to-a-target)).

  Every `http` and `graphite` collector is listed there too, with how it
  connects: `unix_socket`, the socket that it sends its requests over, or
  `proxy`, the proxy in force. The proxy is its `proxy_url` (its own or from
  its file's `defaults`), `direct`, or `environment` for a collector that
  names none and therefore follows `HTTP_PROXY`, `HTTPS_PROXY` and
  `NO_PROXY`. The files of `proxy_basic_auth_file` are shown, never their
  content, and never what the environment names, which may hold a password
  ([A collector's own proxy](REQUESTS.md#a-collectors-own-proxy)).

**Files read at a scrape.** The dry run also reads the files that an
[`sshfile`](SSHFILE.md#a-collector) collector, and a static target of one,
reads when it connects (its private key, passphrase, password and
`known_hosts` files), as a connection would read them. Startup does not do
this. What a scrape would fail on now is listed under `details.warnings` of
the `config` entry, or of the `static_targets` entry for a target's own
files, and logged. It does not fail the check, because a file may be put in
place after the exporter starts.

The same is done for the files of every type that reads one at a scrape,
both a collector's files and those that a static target sets itself:

- a [`sql`](SQL.md#a-collector) collector: the password, private key and TLS
  files;
- a [`redis`](REDIS.md#static-targets) or [`ldap`](LDAP.md#static-targets)
  collector: the password and TLS files;
- a [`tcp`](TCP.md#static-targets), [`grpc`](GRPC.md#tls),
  [`tlscert`](TLSCERT.md) or, with `transport: tls`,
  [`dns`](DNS.md#dns-over-tls) collector: the TLS files;
- a [`kubernetes`](KUBERNETES.md) collector: the token and TLS files;
- an [`s3file`](S3FILE.md) collector: the credential files;
- an [`snmp`](SNMP.md#versions-and-accounts) collector: the community and
  password files;
- an [`sshexec`](SSHEXEC.md) collector: the key, password and `known_hosts`
  files, as for `sshfile`;
- an `http` or `graphite` collector: its `tls` files, the secret files of
  its [`login`](AUTHENTICATION.md#logging-in-to-a-target) and the files of
  the login's own `tls` block.

The dry run takes the same flags as a real start, and they matter:

- `--config.file` and `--static-targets-file` choose what is checked.
- `--config.expand-env` and `--static-targets.expand-env` decide whether
  `${NAME}` references are expanded in each file. A check run where a
  referenced variable is not set therefore fails, exactly as startup would.
- `--python.path` is the interpreter that the Python scripts are compiled
  with.
- `--config.watch` and `--config.watch-interval` are reported when the watch
  is on.

It never binds a port, starts a watch or contacts a target.

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
      "details": {"collectors": ["app_json", "legacy_text"], "otlp_enabled": false, "config_expand_env": false,
        "request_policies": {"app_json": {"proxy": "environment"}, "legacy_text": {"proxy": "environment"}}}
    },
    {
      "check": "python_scripts",
      "file": "config.yaml",
      "status": "failed",
      "errors": ["collector app_json pre_script must produce its result in a variable named 'data'; assign to data or mutate it in place. ..."]
    },
    {
      "check": "static_targets",
      "file": "static-targets.yaml",
      "status": "failed",
      "errors": ["target \"legacy_eu\" sets export_via_otlp, which needs OTLP export; set otlp.enabled: true and otlp.endpoint, or leave the target to the static targets endpoint"],
      "details": {"targets": ["legacy_eu", "legacy_us"], "static_targets_expand_env": false}
    }
  ]
}
```

| `check` | Present | What it validates |
| --- | --- | --- |
| `config` | always | The configuration file and its collector files load and are valid, and no collector name is defined twice. With [`otlp.exporter_instance`](OTLP.md#the-exporter-instance) on, `details.otlp_exporter_instance` has its `attribute` and its `value` as startup resolves it: the host name, or in allocator mode the `--allocator.member` name, when the file writes none. |
| `python_scripts` | always | Every pre-script and `python` transform compiles, and every pre-script produces `data`. Each faulty script is its own entry in `errors`, which names the [collector file](#collector-files) of a collector defined in one. A configuration without Python needs no interpreter and passes with `"scripts": 0`. |
| `config_watch` | with `--config.watch` | Reports `--config.watch-interval` under `details.interval`. A non-positive interval is a command-line error (exit 2) before the check, so this entry does not fail. |
| `static_targets` | with `--static-targets-file` | The [static target file](STATIC-TARGETS.md) is valid on its own, and against the configuration: every collector exists, and a target with `export_via_otlp` has OTLP export enabled. |

Each entry's `status` is `ok`, `failed` with `errors`, or `skipped` with a
`reason` when it depends on a step that failed. The Python scripts cannot be
read from a configuration that did not load, and a target file that is valid
on its own cannot be paired with such a configuration. A skipped check
counts as not passing, and the report still shows it, so the report is never
shorter because something went wrong. The top-level `status` is `ok` only
when every entry is. `request_types` lists the request types that the binary
was built with (see
[Choosing request types at build time](#choosing-request-types-at-build-time)).

stderr carries one JSON log line per check (`configuration check passed` at
INFO, `skipped` at WARN, `failed` at ERROR with its errors) and a final
`configuration check complete`, so a job's log reads like the exporter's
own. `--log.level` reduces the log and never changes the report. Pipe the
report through `jq` in CI:

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

In Kubernetes, run it as a Job or an init container, not as the exporter
itself. A pod started with `--dry-run` validates and exits instead of
serving, which is why the Helm chart rejects it in `extraArgs`.

## Configuration from the target allocator

With `--allocator.url`, the exporter takes its configuration from the
[target allocator](ALLOCATOR.md) (`pue-allocator`) instead of from its own
files. This is allocator mode.

The allocator holds the configuration, its collector files and the static
targets. It splits the static targets among the exporters that heartbeat to
it, and serves each exporter its bundle: the configuration with its
collector files, and its share of the static targets. The exporter writes
the bundle into `--allocator.cache-dir` and loads it there as it loads
mounted files. `--config.expand-env`, `--static-targets.expand-env`,
`--config.watch` and `SIGHUP` therefore work as without the allocator, on
the files in the cache directory.

```sh
prometheus-universal-exporter \
  --allocator.url=http://pue-allocator:9116 \
  --allocator.cache-dir=/var/cache/pue
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--allocator.url` | none | The allocator's `http://` URL. It turns allocator mode on. |
| `--allocator.cache-dir` | none, required | The directory the bundle is written into and loaded from. A restart starts with the bundle the exporter installed there. |
| `--allocator.member` | the host name | This exporter's name among the allocator's members: in Kubernetes the pod's name, which the host name is. |
| `--allocator.advertise` | an address of the host, listen port | `host:port` Prometheus is told to send this exporter's probes to: the `--web.listen-address` host when it is one address and not a loopback one, or else the host's first address that is not, IPv4 first, with the listen port. An IPv6 address may be written without brackets, `fd00::12:8080`, as a pod's `$(POD_IP):8080` is in an IPv6 cluster. |
| `--allocator.token-file` | none | A file holding a bearer token sent with every request to the allocator, read again for each, so a rotated Secret takes effect at once. |
| `--allocator.capacity` | `1` | This exporter's share of the targets' weight against the other members', from 0.1 to 100 with at most three decimals, said in every heartbeat: a member of capacity 2 is given about twice the weight of one of capacity 1. A change takes effect, as a restart does, once the allocator's member list has held for its `--settle` ([Weights and capacity](ALLOCATOR.md#weights-and-capacity)). |

`--config.file` and `--static-targets-file` cannot be given with
`--allocator.url`, and the other `--allocator.` flags cannot be given
without it. The start is refused with exit status `2` and an error that
names both flags. Files that a configuration names, such as CA files,
descriptor sets, `known_hosts` and credential files, are not in the bundle.
They are mounted on the exporter as without the allocator, at the paths that
the configuration names.

**Heartbeats.** The exporter heartbeats to the allocator at the interval
that the allocator gives in its heartbeat answers (its
`--heartbeat.interval`). The interval is 10 seconds until an answer gives
one, and the shortest one when the answering instances differ
([Members and allocation](ALLOCATOR.md#members-and-allocation)). The
exporter has no setting for it.

- It heartbeats to every address that the URL's host name resolves to, so
  two instances of the allocator behind a headless Service both receive it.
  Each address has its own schedule, so an instance that hangs delays only
  the heartbeats to itself.
- Each heartbeat reports which bundle generation the exporter has in force,
  and whether it refused the last one.

**Bundles.** The exporter requests its bundle with a request that the
allocator holds for up to 30 seconds until the bundle changes.

- A new bundle is written beside the one in force, checked as a reload would
  check it, and put in place by replacing one symbolic link. Nothing that
  reads the directory therefore finds part of one bundle and part of
  another. The exporter then reloads it, with `"trigger":"allocator"` in the
  log.
- Each file, the bundle's directories and the cache directory are synced to
  disk before the link is replaced, and the cache directory again
  afterwards. A crash therefore leaves either the previous bundle or the
  whole new one.
- A bundle that does not load, or that names a file also as a directory of
  another (`a` and `a/b`), is refused. The one in force stays, the refusal
  is logged and reported in the next heartbeat, and the bundle is not
  requested again until the allocator has another.
- A bundle whose reload is refused, when the previous files cannot be put
  back, is a failure to write: the exporter puts them back before it tries
  again.

**Failures.** A request or write that fails is tried again after a wait that
doubles from one second to 30, and a heartbeat does not shorten that wait. A
failure repeated at every try is logged once; for heartbeats, once for each
allocator address. An allocator that has just started answers `503` until
its member list has formed. That is not a failure: the exporter keeps what
it has and asks again when the allocator's `Retry-After` says.

**Shutdown.** On `SIGTERM` or `SIGINT` the exporter tells the allocator
immediately that it is shutting down, before
[`--web.shutdown-delay`](#shutting-down), so that its share of the targets
moves to the others. A heartbeat already on its way is waited for first, so
that none saying otherwise reaches the allocator afterwards. Once the probes
have finished, the exporter waits for a bundle that it is writing to be put
in force or refused before it exits.

**Starting.** An exporter whose cache directory holds a bundle that it
installed, and that loads, starts with it immediately and is ready, whether
or not the allocator answers.

- At the start the exporter removes what a write interrupted by a crash or a
  kill left in the cache directory. Without a bundle that it installed, it
  also removes a `config.yaml` or `static-targets.yaml` there, such as one
  put there by hand, and starts without a configuration. The cache directory
  belongs to the exporter and is not a place to put files for it. Other
  files are left alone.
- An exporter without a bundle, or whose bundle does not load, waits for the
  first one. `/health` answers. `/ready` answers `503` (`not ready: no
  configuration yet: waiting for the allocator`). Every other endpoint
  (`/probe`, the self-metrics, the static targets endpoint, the landing and
  collectors pages, the debug page and `/-/reload`) answers `503` with `no
  configuration yet: waiting for the allocator at <url>`, whatever the
  method.
- `web.basic_auth` comes with the bundle. Until one is in force the exporter
  cannot tell whom to let in, and it lets in no one instead of everyone. The
  cost is that the self-metrics, which would show the allocator's sync
  errors, are not served while it waits; the log says why each sync failed.
  An exporter that starts with a bundle from its cache directory has its
  credential from the start.
- The first bundle makes the exporter ready. When the allocator sends again
  the bundle that did not load, it is checked and reloaded where it lies,
  not written again, and stays there when it is refused.

The [self-metrics](SELF-METRICS.md#target-allocator) show how the exporter
keeps up with the allocator.

`--dry-run` with `--allocator.url` checks the bundle in the cache directory,
which is the one a restart would start with. It fails when there is none, or
when the `config.yaml` there is not one that the exporter installed. It
changes nothing there.

## Related pages

- [PYTHON.md](PYTHON.md) — the Python transform and pre-script API.
- [REQUESTS.md](REQUESTS.md) — the request URL and the `Host` header, redirects, HTTP/2, retries, accepted statuses, pagination, rate limiting, restricting targets, TLS and per-scrape overrides.
- [LOCALFILE.md](LOCALFILE.md) — the `localfile` request type.
- [GRAPHITE.md](GRAPHITE.md) — the `graphite` request type and decoder.
- [GRPC.md](GRPC.md) — the `grpc` request type.
- [SSHFILE.md](SSHFILE.md) — the `sshfile` request type.
- [SQL.md](SQL.md) — the `sql` request type.
- [DNS.md](DNS.md) — the `dns` request type.
- [TLSCERT.md](TLSCERT.md) — the `tlscert` request type.
- [NTP.md](NTP.md) — the `ntp` request type.
- [KUBERNETES.md](KUBERNETES.md) — the `kubernetes` request type.
- [S3FILE.md](S3FILE.md) — the `s3file` request type.
- [REDIS.md](REDIS.md) — the `redis` request type.
- [TCP.md](TCP.md) — the `tcp` request type.
- [LDAP.md](LDAP.md) — the `ldap` request type.
- [SSHEXEC.md](SSHEXEC.md) — the `sshexec` request type.
- [ICMP.md](ICMP.md) — the `icmp` request type.
- [SNMP.md](SNMP.md) — the `snmp` request type.
- [SMTP.md](SMTP.md) — the opt-in `smtp` request type.
- [MODBUS.md](MODBUS.md) — the opt-in `modbus` request type.
- [MQTT.md](MQTT.md) — the opt-in `mqtt` request type.
- [WEBSOCKET.md](WEBSOCKET.md) — the opt-in `websocket` request type.
- [FTP.md](FTP.md) — the opt-in `ftp` request type.
- [IMAP.md](IMAP.md) — the opt-in `imap` request type.
- [POP3.md](POP3.md) — the opt-in `pop3` request type.
- [RADIUS.md](RADIUS.md) — the opt-in `radius` request type.
- [WHOIS.md](WHOIS.md) — the opt-in `whois` request type.
- [SIP.md](SIP.md) — the opt-in `sip` request type.
- [AMQP.md](AMQP.md) — the opt-in `amqp` request type.
- [NATS.md](NATS.md) — the opt-in `nats` request type.
- [AUTHENTICATION.md](AUTHENTICATION.md) — credentials for the target, logging in to it, and credentials for the exporter itself.
- [ENDPOINTS.md](ENDPOINTS.md) — what each endpoint takes and answers.
- [FLAGS.md](FLAGS.md) — every command-line flag.
- [SELF-METRICS.md](SELF-METRICS.md) — the exporter's own metrics.
- [STATIC-TARGETS.md](STATIC-TARGETS.md) — targets the exporter scrapes itself and serves on the static targets endpoint.
- [ALLOCATOR.md](ALLOCATOR.md) — the target allocator, `pue-allocator`.
- [OTLP.md](OTLP.md) — OTLP export.
- [LOGGING.md](LOGGING.md) — the log format.
