# Configuration

The exporter reads one YAML document, named by `--config.file`, and any
[collector files](#collector-files) it lists. It declares the
collectors — how to call a target, how to read what comes back, and which
metrics to publish — and, optionally, the exporter's own settings under `web`
and `otlp`. Target URLs are deliberately not part of it: Prometheus supplies
each one per scrape.

`configs/config.example.yaml` is a complete working document to
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

`decoder.type` chooses how the response is decoded. It is optional and
defaults to `auto`, where the transform selects a deterministic decoder when it
can: `regex` uses text, `csv` uses CSV, `css` uses HTML, and `prometheus` uses
Prometheus exposition. A `graphite` collector reads with the `graphite`
decoder, whatever its transform, and a `grpc` collector with `json`, since a
call is answered as JSON. Other transforms — jq, yq, XPath, Python —
decode each response by what it says it is: an `http` response by its
`Content-Type` header, a `localfile` file by its extension, and by its content
when neither says: an HTML page by its doctype or `<html>` element, in any
case, within the first KiB and after any whitespace, comments and XML
declaration, other markup as XML, JSON by its opening bracket, Prometheus text by its `# TYPE` or
`# HELP` lines, carbon lines (`<path> <value> <timestamp>`, some path with a
dot) as Graphite series, and anything else as text. If the decoded response cannot be used by the selected transform, the
probe fails with a clear mapping error.

What a response says it is, with the decoder left to it:

| `Content-Type` | Extension of a `localfile` file | Decoder |
| --- | --- | --- |
| `application/json`, any type ending in `+json` | `.json` | `json` |
| `application/yaml`, `application/x-yaml`, `text/yaml` | `.yaml`, `.yml` | `yaml` |
| `application/xml`, `text/xml` | `.xml` | `xml` |
| `text/csv` | `.csv` | `csv` |
| `text/html` | `.html`, `.htm` | `html` |
| `application/openmetrics-text`, `text/plain; version=0.0.4` | `.prom` | `prometheus` |
| `text/x-graphite` | `.graphite`, `.carbon` | `graphite` |
| any other, and none | any other, and none | by the content |

The type is read without its parameters and in any case, as the extension is.
Nothing but `text/csv` and `.csv` says CSV, and no content does: a body sent
as `application/csv` or `text/tab-separated-values`, and a file named `.tsv`
or `.txt`, is text, so a `python` script or a pre-script of a `jq` collector
is given one string where it expected rows. Set `decoder.type: csv` for
those. A `csv` transform reads CSV whatever the response says.

Each transform reads what certain decoders produce:

| Transform | Decoders it reads |
| --- | --- |
| `jq`, `yq` | `json`, `yaml`, `graphite` |
| `regex` | `text` |
| `csv` | `csv` |
| `css` | `html` |
| `xpath` | `xml`, `html` |
| `prometheus` | `prometheus` |
| `python` | every decoder |

A `decoder.type` that names a decoder the collector's transform does not read
could never work, so it is refused when the configuration loads, naming both
and what would work:

```text
collector "app" decodes with json, which its regex transform cannot read: it reads text; set decoder.type to text, or use a transform that reads json: jq, yq or python
```

A `jq` or `yq` transform with a `pre_script` is the exception: its rules read
the mapping or list the script leaves in `data`, whatever was decoded, so it
may name any decoder. The same table decides on a scrape, when the decoder
was left to each response.

That fallback means a target that changes its `Content-Type`, or a file renamed
to another extension, is quietly read another way. So a collector that leaves
`decoder.type` unset where the transform implies none is logged at startup and
on every reload, and listed in the [dry run](#dry-run) report:

```text
{"level":"WARN","msg":"configuration warning","file":"config.yaml","warning":"collector \"app_json\" sets no decoder.type, so it decodes by the Content-Type header of each response, and by the content when that does not say; set decoder.type to fix the decoder"}
```

Setting `decoder.type` — to `auto` too, when choosing per response is what you
want — silences it. An explicit decoder is also what reads an ambiguous or
mislabeled endpoint:

```yaml
decoder:
  type: json   # the endpoint says text/plain, but sends JSON
```

Supported decoders are `json`, `yaml`, `xml`, `csv`, `html`,
`prometheus`, `text`, `graphite` and `auto`. The `graphite` decoder reads
Graphite series — a render API answer or carbon lines — for jq, yq and Python;
see [Graphite](GRAPHITE.md). Every collector sets `transform.type`, one of
`jq`, `yq`, `xpath`, `css`, `csv`, `regex`, `prometheus` and `python`; there is
no default, and a collector without one is refused at startup. JSON and YAML expressions
use the embedded jq-compatible engine (the expression language is also used
for yq-compatible transformations). A JSON body is one value and a YAML body
one document: NDJSON, or anything else after the JSON value but whitespace,
fails the scrape saying so, and so does a second YAML document after `---`
(a leading `---`, and a trailing one with nothing after it, are fine), rather
than everything after the first being dropped unseen. A key written twice in
one mapping of a YAML body fails the scrape too, `line 3: mapping key "a"
already defined at line 1`, rather than the later value replacing the earlier
unseen: the error names the first ten such problems and counts the rest. An
error quotes no more than the start of a part of the document, followed by
the length of the whole — the first 64 bytes of a key written twice (`mapping
key "aaaa..."... (100000 bytes) already defined at line 1`), and the first 256
of a key that is a sequence or a mapping, of a value that does not fit its tag
and of an anchor's name — so it stays short whatever the document holds. A
YAML body costs time and memory in proportion to its size whatever it holds —
a mapping of 200,000 keys decodes in about the time it takes to parse, and a
key written 100,000 times is refused as quickly — so `max_response_bytes`
bounds what a target's answer can cost. A response decodes into a
value whose lists and mappings nest 10,000 deep at most, one inside another,
whatever its format, and a document nested deeper fails the scrape in the
`decode` stage, saying so: a JSON one with `arrays and objects nested more than
10000 deep, at line 1, column 10001`, and a YAML one, with or without aliases
and however it is written, with `yaml: the document is nested more than 10000
deep, counting what its aliases stand for; a response may nest 10000 deep at
most`, after `line N:` where the YAML library's parser is what stops at the
level too many and names a line (`yaml: line 3: the document is nested more
than 10000 deep, ...`: the line that level opens on, or the line of the last
key or value before it), the failure being one failure to the
[log of repeated failures](LOGGING.md#repeated-failures) with or without a
line. A YAML document is
counted as a JSON one is, by the sequences and mappings written one inside
another, with an alias counting as what it points at — so anchors that each
hold an alias of the one before are as deep as the chain is long — and a
mapping merged with `<<` counting as it is written, one level inside the
mapping it is merged into. The exporter keeps this bound itself, before the
document is decoded, because the YAML library keeps none on the whole: its
parser bounds indentation and brackets apart, at 10,000 levels each (a
document past either is refused there, in the library's words `exceeded max
depth of 10000`, which the exporter replaces with the message above, the same
mistake reading the same whichever of the two finds it), so brackets inside
indentation parse nested 20,000 deep and more,
and it decodes a chain of aliases on a stack as deep as the chain is long,
which could exhaust the process. A [Python script](PYTHON.md#what-data-is) is
handed a value as deep as it decodes. A document the YAML
library itself
fails on fails the scrape in the `decode` stage like any other that cannot be
decoded (`the YAML library failed on the document: ...`); should the exporter's
own code fail while it decodes one, the error says so and where (`the exporter
failed on the YAML document (yamlkeys.go:<line> decode.(*yamlMap).set): ...; this
is a defect of the exporter and not of the document, please report it`), which
is what to report. XML supports XPath, HTML supports CSS
selectors and XPath (including bare element selectors such as `h1`), text
supports regular expressions, and Prometheus input is parsed before
filtering/renaming. An XML document may nest its elements 512 deep, as an HTML
document may; a deeper one fails the scrape in the `decode` stage, saying so,
whatever its size. The other decoders too quote no more than the start of a
value of the body they cannot read, its first 64 bytes, followed by the
value's length, and then say what they have to say of it: `text format
parsing error in line 7: label value "aaaa..."... (1000000 bytes) contains
unescaped new-line`, `carbon line 3: "app.web01.requests 42 1727000000 x
aaaa..."... (1000040 bytes) has 5 fields; want <path> <value> <timestamp>`,
`CSV header names column "aaaa..."... (70000 bytes) twice, as columns 2 and
3; rename one, ...`, `XML syntax error on line 2: element <aaaaaaaa... (90000
bytes)> closed by </b>`. So an error is a line of a few hundred bytes that
ends with what is wrong and what to change, whatever a value of the body
holds, and so is the warning for the first line a decoder leaves out, a
carbon line that is skipped or a sample line left out. The last resort is a
bound on the whole error, 2,000 bytes: one longer than that — a histogram
series named by a hundred labels, each shown — is cut there and ends with the
length it had, `... (6400 bytes)`. The bound is that of the error of every
stage a probe or a static target's scrape can fail at, in the answer, in the
log and in a debug report alike ([Logging](LOGGING.md#repeated-failures)): a
jq rule that raises `error(.message)`, a script that raises an exception with
its data, a redirect to a URL of a megabyte and a gRPC status message of ten
fail in 2,000 bytes at most, where the error was as long as what it quoted.

### Character encodings

Everything the exporter answers is UTF-8, as Prometheus requires; it refuses
a whole scrape over one label value that is not. A response in another
encoding is converted before it is decoded. The encoding comes from, in this
order: a byte order mark (UTF-8, UTF-16LE or UTF-16BE); the collector's
`response.charset`; the `charset` of the `Content-Type` header; and what the
document declares in itself: for XML, the encoding of the XML declaration;
for a document decoded as HTML, a `<meta>` in its first 1024 bytes and, in a
page without one that declares an encoding, the encoding of an XML
declaration the document starts with, as XHTML has one. Where the two
disagree the `<meta>` is the one read, as in a browser. The XML declaration
of an HTML document counts only at its very first byte: after a blank or an
empty line it is none.

A `<meta>` is found as a browser finds one (the HTML standard's prescan), in
a tag and not in the words `charset=`: it is a `<meta>` tag with a `charset`
attribute, or with `http-equiv="Content-Type"` and a `content` attribute
holding `charset=…`, in any case, quoted or not, and the first tag that
declares an encoding is the one read. A `<meta>` inside a comment, the
`content` of any other `<meta>` — a description that mentions
`charset=windows-1251`, the address of a refresh — and the word in another
element's attributes or in a title declare nothing, and neither does a
declaration that is not whole within the first 1024 bytes. As in a browser, a
`<meta>` or an XML declaration of an HTML document naming UTF-16 is read as
UTF-8, since a page whose declaration could be read is not UTF-16, and
`x-user-defined` as `windows-1252`; a byte order mark or header naming UTF-16
is taken as it says.

A `<meta>` naming an encoding the exporter does not know declares nothing, as
in a browser: the next `<meta>` is looked at, then the XML declaration, and a
page that declares nothing is taken for UTF-8. The same holds for the XML
declaration of an HTML document. The name is the whole value of the
attribute, so `<meta charset=utf-8/>` names `utf-8/`, which is no encoding: a
UTF-8 page is read all the same, but a windows-1251 page whose tag is
`<meta charset=windows-1251/>` has declared nothing, and is read as UTF-8 and
repaired, with the warning below. Quote the value or leave a blank before
`/>`.

The `charset` of a `Content-Type` is read from a header that is not well
formed too: one with a parameter that has no value (`text/html;
charset=windows-1251; q`), and two values a proxy joined with a comma
(`text/html; charset=windows-1251, text/html`), of which the first is read. A
header without a type and a subtype, a `charset` without a value, and one
whose value opens a quote that is not closed (`charset="; q`) name nothing.

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
unknown name in the `Content-Type` header, or in the XML declaration of a
document decoded as XML, fails the `decode` stage, naming it.
Transforms and Python scripts see the converted body, and its `Content-Type`
says `charset=utf-8`. A [debug report](#debugging-a-probe) shows the response
as the target sent it, and says what its body was converted from.

What still is not valid UTF-8 after that — a target that says UTF-8 and is
not — no longer fails the scrape: the invalid bytes in label values and help
text are replaced with `�` (U+FFFD), counted in
`http_exporter_invalid_utf8_total`, and logged as a warning naming the first
metric (`first_metric`; a name longer than 200 bytes by its first 200 and its
length, as an [error](LOGGING.md#repeated-failures) shows one). Setting
`response.charset` fixes it at the source. Each run of
invalid bytes becomes one `�`, however long it is, so names that differ only
in such bytes are repaired into the same text: a page of Cyrillic names in
windows-1251 that declares nothing gives every one-word name the label `�`,
and the series, now duplicates of one, fail the scrape (`duplicate metric
series`) after the warning. The remedy is the same.

Prometheus input is the text exposition format, version 0.0.4, or
OpenMetrics 1.0 text, read by the exporter's own parser. It follows the
reference parser's rules — families from HELP and TYPE lines,
`_sum`/`_count`/`_bucket` grouped into summaries and histograms, quoted UTF-8
names such as `{"my.metric", key="value"} 1` — and accepts three things the
reference parser rejected: a body without a final newline, CRLF line endings,
and trailing blanks on a line. It rejects a histogram or summary count, or a
bucket's, that is negative, NaN, infinite or a fraction such as `1.5`: a count
is a whole number of observations, however it is written (`7`, `7.0`,
`1e3`). A malformed body fails the decode with the offending line
number, for example
`text format parsing error in line 3: expected float as value, got "n/a"`. A
label value that is not valid UTF-8 does not fail it: it is repaired with `�`
and counted, as above.

A label a target writes with an empty value is left off its series:
`m{l="",k="v"}` is read as `m{k="v"}`. To Prometheus the two are one series,
and the exporter exports no label with an empty value, wherever it comes
from — a target's exposition, a rule's expression, a script, a constant
written `""`. The label is off before anything reads the series: a
`prometheus` rule's label that reads `l` finds none, as when the target did
not write it, and a `required` one is missing; a pre-script and a `python`
transform are given the series without it; and the text format, OpenMetrics
and [OTLP](OTLP.md) carry none, with or without a script in between. What
was refused about such a label still is: its name written twice on a
sample, and a histogram's `le` or a summary's `quantile` that is empty,
which is a bound that is no number rather than a label. On a series that
has no buckets or quantiles, `le` and `quantile` are labels like any other
and are left off when empty.

Two series a target tells apart by nothing but such a label are the same
series twice, `m{l=""}` beside `m`, and the scrape fails for the duplicate
as it does for `m` written twice (`duplicate metric series "m"`); it failed
before as well, the check having always read an empty label as none. A
histogram's or a summary's samples written so are one series' samples: a
histogram whose buckets carry `l=""` and whose `_sum` and `_count` do not is
one histogram, where it was two halves that failed the scrape, and two whole
histograms told apart by `l=""` alone fail the decode for the sample they
then have twice (`second h_sum sample for the histogram h`).

A histogram or a summary is passed on as the target wrote it, with its
buckets and quantiles in ascending order, and with values that the text
format allows and OpenMetrics does not, such as a negative `_sum` or bucket
counts that fall (see [OpenMetrics](#openmetrics) for how those are written).
OpenMetrics allows a histogram of buckets alone and a summary of quantiles
alone, and some
text format targets leave `_sum` or `_count` out: such a series is exported
without the line, in either exposition format, never with a `_sum 0` or a
`_count 0` the target did not write. A histogram's count is then the count of
its `+Inf` bucket, which counts every observation, and one with a `_count`
and no `+Inf` bucket is given the bucket.

A histogram's `+Inf` bucket and its `_count` are one number written twice,
and a target that updates the two without a lock is scraped between the
updates now and then: `h_bucket{le="+Inf"} 7` beside `h_count 8`. Such a
histogram does not fail the scrape, which would lose every other family of
the target with it. It is passed on as written: the text format answers
`h_bucket{le="+Inf"} 7` and `h_count 8`, each as it was read. So is a
histogram with neither a `+Inf` bucket nor a `_count`, which is exported
with the buckets and the `_sum` it has and is given neither line. Neither
is an OpenMetrics histogram, so [OpenMetrics](#openmetrics) writes both as
`unknown` families and [OTLP](OTLP.md#delivery) as gauges, with the same
series and values.

What cannot be one series fails the decode, as two samples of one plain
series fail the scrape, and the error names the series and the line it
starts in:

- two buckets with one bound, however it is written — `le="1"` and
  `le="1.0"` — or two values of one quantile
  (`the histogram h, which starts in line 2, has two buckets with the upper
  bound 1`);
- a second `_sum` or `_count` sample of the series.

A sample line that is no part of its family does not fail the decode: it is
left out, and the rest of the answer is served. Under `# TYPE h histogram`, a
sample named `h` itself, or an `h_bucket` without an `le` label, has a value
no histogram series has a place for, and so has a sample named as its summary
without a `quantile` label. Client libraries write such lines and Prometheus
ingests them: Micrometer's older Prometheus registry writes a timer's
percentiles as `x{quantile="0.95"}` under `# TYPE x histogram`, and
VictoriaMetrics' metrics library writes its buckets as
`x_bucket{vmrange="..."}`. The line cannot be passed on as a series of its
own, which would be a second family under a name the histogram `x` writes
its own lines with, so its value is not exported: the histogram is, with the
buckets, `_sum` and `_count` it has, and every other family. The lines left
out of the families the collector passes on are counted in
[`http_exporter_decoder_lines_skipped_total`](SELF-METRICS.md) and logged at
warn level once for a scrape, not once for a line, with the first of them and
what was expected in its place (`sample lines left out ... line 2: expected
h_bucket with an le label, h_sum or h_count as a sample of the histogram h,
got h`); while every scrape leaves the same line out the warning is repeated
sparingly, as a failure's is. A line that cannot be read at all, such as a
value that is no number, still fails the decode. OpenMetrics' `_created`
samples are read and left out, as before, and are not counted.

A body is read as OpenMetrics when its `Content-Type` is
`application/openmetrics-text`, or, when the `Content-Type` does not say
`text/plain; version=0.0.4`, when its last line is `# EOF` — so a file of
OpenMetrics read by a [`localfile`](LOCALFILE.md) collector is too. The
exporter sends no `Accept` header of its own, so a target that negotiates
answers in the text format; set `Accept: application/openmetrics-text` in
`request.headers` to ask for OpenMetrics. Read as OpenMetrics:

- timestamps are seconds, with a fraction, and are kept as milliseconds;
- exemplars (`... 1 # {trace_id="abc"} 0.5`) are skipped;
- a counter `foo`'s sample `foo_total` is the counter `foo_total`, with the
  family's help, as Prometheus names it; a family named `foo_total` is read the
  same way;
- `_created` samples of counters, histograms and summaries are dropped: the
  exporter has no creation time to export;
- `unknown` is `untyped`, `stateset` a gauge, `info` the gauge `foo_info`, and
  a `gaugehistogram` the gauges `foo_bucket` (with `le` as a plain label),
  `foo_gcount` and `foo_gsum`: a histogram's buckets may only go up, a gauge
  histogram's go up and down;
- `# UNIT` lines are ignored, and nothing but blank lines may follow `# EOF`.

All non-Python transforms use the same collector-level metric declaration. Each
entry has `name`, `description`, `type`, `labels`, and a transform-specific
`expression`. The only allowed metric types are `gauge`, `counter`,
`histogram`, `summary`, and `untyped`. A rule reads one value per series, so
it is a `gauge` unless it says otherwise, and `histogram` and `summary` are
refused at startup for every transform but `prometheus`, where a rule passes
through a series that has its buckets or quantiles; a `prometheus` rule
without a `type` keeps each series' own, so a counter stays a counter:

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
- `regex`: a RE2 expression; the capture group named `value`,
  `(?P<value>\d+)`, is the numeric value, or the first capture group when
  none has that name, and labels map to capture-group numbers or names. A
  named value can stand after what a label reads, as in
  `(?m)^(?P<host>\S+) load (?P<value>[\d.]+)$`, where the first group is the
  `host` label's (`expression: host`, or `"1"`); groups are numbered by their
  place in the regex whichever of them is the value, and a label may read the
  value's group too, taking its text as written. A regex without a capture
  group, or with two named `value`, is refused at startup. A match whose
  value group captured nothing — an optional group that took no part, or one
  that matched only blanks — is a missing value for that match.
- `csv`: the expression is the numeric column name and labels map to column
  names. With `response.csv.header: false` there are no names, and columns
  are named by number, from 1: `expression: "2"` reads the second column. A
  name there is refused at startup. A header naming one column twice, or
  leaving a column that holds values unnamed, fails the scrape, naming the
  column, rather than one silently hiding the other; rename one, or read the
  columns by number with `header: false`. An unnamed column empty in every
  row, as a delimiter ending each line leaves, is left out. A row with more
  fields than the header has columns fails the scrape as well when any of
  the extra fields holds a value, naming the line and the column — `CSV line
  5 has a value in column 5, which the header does not name` — since the
  value has no name to be read by: a text with a comma and no quotes around
  it, a delimiter that is not the file's, or a quoted field of tab-separated
  values read without `trim_space` put a row's values in the wrong columns,
  and this is the one sign of it. Extra fields that are all empty, as a
  delimiter ending the line leaves, are left out; a row with fewer fields
  than the header has empty values in the columns it lacks; and with
  `header: false` rows may be of any lengths. A line ends at a line feed, at
  a carriage return with a line feed after it and at a carriage return
  alone, in one file too; inside a quoted field a line feed and a carriage
  return alone are the field's text, and a carriage return with a line feed
  after it is read as the line feed alone. A quote inside a
  field that does not start with one, as in `5" disk`, is read as written; a
  quoted field left open, or with a quote in it that is not doubled, still
  fails the scrape, whatever the other rows hold, rather than be read on over
  the rows after it. The error of a field left open names the line the file
  ends on, in a file whose lines end with a carriage return alone as in one
  of line feeds: the carriage returns after the quote that is never closed
  end lines too. `response.csv.trim_space: true` trims the blanks on both
  sides of every field, with a header row and without one, and lets a quoted
  field begin after blanks and end before them: `a,  "x, y"  ,b` is read as
  `a`, `x, y` and `b`. Without it the blanks after a closing quote fail the
  scrape as a stray quote does. With a
  tab, or another blank that is not a space, as the delimiter an empty field
  stays where it is, and the fields after it in their columns, and a quoted
  field written after spaces is still a quoted field, a tab inside it kept.
  With a space as the delimiter a run of spaces is one delimiter, which is
  how columns aligned with spaces are read (`web01   72  "two words"` is
  three fields), the spaces after a closing quote being the delimiter too;
  an empty field is written as a pair of quotes then, `web01  ""  72`, since
  no blank can stand for it. That is under `trim_space`: without it every
  space is a delimiter of its own, and aligned columns fail at the header,
  for the column a run of spaces leaves unnamed. A response with no
  rows — a header alone, or nothing at all — has no value for any rule, so a
  required rule is [missing its value](#when-a-metric-cannot-be-extracted).
  [Reading CSV: what to expect](#reading-csv-what-to-expect) lists what the
  files tools write do to a rule.
- `css`: the expression selects the HTML element whose text is numeric. Without
  [`items`](#metrics-per-item) a metric is one value, so the expression must
  match at most one element. Several values, and labels read from the page,
  need `items`: select the rows with it, and the value and the labels as cells
  of each row. A selector reads the text of the element it selects and never
  an attribute: a value or a label that stands in one, as in `data-value` or
  in the `datetime` of a `<time>`, is for `xpath`.
  [The text of an HTML element](#the-text-of-an-html-element) says what that
  text is, and [Reading HTML: what to expect](#reading-html-what-to-expect)
  what a page does to a selector.
- `xpath`: the expression selects XML/HTML nodes whose text is numeric; labels
  are XPath expressions evaluated at each node, such as `../@name`, or `@name`
  for an attribute of the node itself. An absolute path in a label starts at
  the document, as it does in a rule's expression, wherever it stands:
  `/status/@site`, `//status/@site`, `concat(/status/@site, '-', @id)` and
  `../x[@ref=//y/@id]` read what they say at every node the rule selects.
  So `//name` in a label is the first `name` of the document, the same for
  every series; a `name` beneath the node is `.//name`, and its child `name`.
  A label that cannot depend on the node is evaluated once for the rule and
  the response, and every series is given its value: one that is one absolute
  path and nothing else, as `//status/@site` or `//row[v > 1]/name`, or one
  call of `count`, `sum`, `string`, `number`, `boolean`, `not`,
  `normalize-space`, `string-length`, `name`, `local-name`, `round`, `floor`
  or `ceiling` with one such path for its argument, as `count(//row)`. Every
  other expression with an absolute path in it is evaluated at each of those
  nodes, and walks the document from the top at each — `concat(/status/@site,
  '-', @id)`, and also `count(//row) + 1`, `//a | //b` and `(//name)[1]`,
  which no node changes: over thousands of nodes that is seconds of a probe,
  so give the part that is the same everywhere a label of its own in one of
  the two shapes, and reach what stands close by with `..` or `ancestor::`.
  An expression is read to its end or refused at startup. The XPath engine
  stops where one complete expression ends and says nothing of what follows,
  so `sum(//a))` would be `sum(//a)` and `//a 'x'` would be `//a`: such an
  expression is refused, with the place — `XPath "sum(//a))": the ")" at byte
  9 closes nothing`, or `what stands from byte 5 on, "'x'", is no part of the
  expression before it`. A second predicate after an expression in
  parentheses, a function call or a literal is refused the same way, since
  the engine reads one predicate there and no more — `(//a)[@x][1]` would be
  `(//a)[@x]` — and the refusal shows the form the engine reads, the
  expression and its first predicate in parentheses of their own:
  `((//a)[@x])[1]`. An expression longer than a few hundred bytes may be
  refused without the place. Also refused are an expression with a NUL
  character in it, which the engine takes for its end, and one whose
  parentheses, predicates and function arguments stand more than 198 deep
  within one another, the deepest that can be seen to be read to its end.
  A rule may select attributes, as
  `//job/@size` does: a label is then relative to the attribute, `.` its
  value, `name()` its name and `../@name` another attribute of its element.
  The label `text()` gives the attribute's value too, as `.` does, so the
  series of such a rule can be told apart by it: to XPath an attribute has no
  text beneath it, but the value is what this label has always given. Only
  that bare label is the value. An attribute has no nodes beneath it, over
  HTML as over XML, so on such a rule `./text()`, `text()[1]`, `node()`,
  `self::*`, `descendant::text()`, `normalize-space(text())`,
  `string(text())` and `substring(text(), 1, 2)` give nothing,
  `contains(text(), '1')` is false and `count(text())` is 0: write them of
  `.`, as in `normalize-space(.)` or `substring(., 1, 2)`.
  This holds over HTML as over XML: `//td/@data-value` with the labels
  `../@data-server` and `../../@id` is one series for each cell, named by its
  cell and its row, and `//time/@datetime` with a
  [`time_format`](#reading-a-date-or-a-time-as-the-value) and the label
  `../../../td[1]` reads the moment of every row of a table. Over HTML the
  text of a node is [the text of an HTML element](#the-text-of-an-html-element).
  Over XML, prefixes in them mean what
  [`response.namespaces`](#xml-namespaces) says. HTML has no namespaces: a
  label that is `@` and one attribute name, on the node or after `../` steps,
  is read by that name as the page writes it, a colon included, as in
  `@xml:lang`, `@og:type`, `@v-on:click` or `../@data-id`
  ([Attribute labels](#attribute-labels)). An expression that
  computes a value instead of selecting nodes — `count(//job)`,
  `sum(//job/@size)`, `string(/status/@load)`, a comparison such as
  `/status/@state = 'ok'` — is one series of that value, a comparison `1` or
  `0`; a label may compute its text too, as in `normalize-space(@name)`. A
  computed value that is not a number, such as `number('n/a')`, is the rule's
  missing value: a NaN an expression computes is the XPath engine saying it
  could not read a number, where `NaN` written in a cell is the value the
  source gives, and is exported
  ([What counts as a number](#what-counts-as-a-number)). `sum()` needs every
  node it adds up to be a number: over a cell of `n/a` or `1,234` the sum has
  no value, and the rule is missing its value, with an error naming the text
  ([Adding up nodes with `sum()`](#adding-up-nodes-with-sum)). A label read
  from an element's text or from an attribute, or computed as a string,
  is trimmed of leading and trailing whitespace, as a `css` label is, so the
  indentation of pretty-printed XML is no part of it. The whitespace inside
  the text is kept, line breaks and tabs included; `normalize-space(.)` makes
  single spaces of it, which `css` has no way to. An attribute read by its
  name on the node itself, `@title`, is trimmed like one read through a path,
  `../@title`, and one that holds nothing but blanks gives no label.
- `prometheus`: the expression matches source metric names; it can remap the
  name, description, type, and selected labels. Without `type` a series keeps
  its own. A rule that matches no metric of the response has no value, so a
  required rule is [missing its value](#when-a-metric-cannot-be-extracted);
  give a rule for a metric the target may leave out `required: false`. The
  expression is a regular expression, matched anywhere in the name the
  target gives a metric; a rule without one matches the metric its `name`
  names. One that is nothing but blanks, such as `expression: " "`, is
  refused at startup, as [an entry of `transform.include`](#collector-wide-labels)
  is: it matched only the names that hold those blanks, so the rule passed
  nothing on, and with `required: false` said nothing. The error says how a
  pattern that does mean a blank is written, `'[ ]'` or `'\x20'` in single
  quotes. Written `""`, the expression is the key left out. A rule needs
  one of the two, since they are how it says which metrics it is about: one
  with neither a `name` nor an `expression` — `- {}`, or a rule of a `type`,
  a `description` or `labels` alone — matched no metric, was reported as
  missing on every scrape as if the target had left a metric out, and with
  `required: false` did nothing without a word. It is refused at startup.
  Such a rule has no name to be named by, so the error names the collector
  and which of its rules it is, counted from 1, as in `collector "node"
  metrics rule 2 has neither a name nor an expression, …`, and says what to
  write: an `expression`, a `name`, or `expression: '.*'` for a rule about
  every metric. A pattern belongs in `expression`, never in `name`: a
  rule's `name` is one metric's, matched whole when the rule has no
  expression, and beside an expression the one name the series it matches
  are exported under. So `name: 'node_.*'`, `name: '^up$'` and
  `name: 'up|node_load1'` are refused as no metric name, and since a name
  with a character a regular expression gives a meaning — `. * + ? ^ $ | (
  ) [ ] { }` or a backslash — is almost surely a pattern under the wrong
  key, the error says so: `… is not a valid Prometheus metric name; use
  letters, digits, underscores and colons, not starting with a digit, or
  set the collector's name_escaping to underscores or values to export it
  escaped; a pattern to match the target's metric names by is a prometheus
  rule's expression, not its name, so if this is one, write it as
  expression, …`. That is under [`name_escaping`](#utf-8-names) `fail`, the
  default. Under `underscores` and `values` a rule's name may be any name,
  as a target's may, so `name: http.server.duration` is the metric the
  target calls that, matched whole and exported escaped, and nothing there
  tells a name from a pattern: `name: 'node_.*'` loads as the metric of
  that very name, which no target has. A metric is passed on by every
  rule that matches it, each making its series, so a rule of a `name` and a
  rule whose expression matches that name both pass that metric on: with
  the same labels they would make every series of it twice, and are
  [refused at startup](#a-name-and-a-pattern-that-matches-it).

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

A label setting both, or neither, is refused at startup. So is one whose
`expression` is nothing but blanks, such as `expression: " "`, with a `value`
beside it or without: blanks are not the key left out, as `""` is, and they
read nothing, so the error says to write the expression or, for a constant,
to set `value` and leave `expression` out. A `value` of blanks is a constant
like any other, exported as written. For a static label on
every metric of a collector, use [`transform.labels`](#collector-wide-labels)
instead.

A `value` may hold `{{param_<name>}}` placeholders, which the probe's
parameters fill as they fill the request's: `value: "api-{{param_tenant}}"`
is `api-acme` on the series of a probe with `&param_tenant=acme`. See
[In label values](REQUESTS.md#in-label-values) for the rules.

Label expressions use the same transform-specific language as the metric
expression. For CSV, each row produces a metric and `expression: server`
selects that row's `server` column.

A jq or yq label is the text of the value its expression gives. A number reads
as it is written, without an exponent from a millionth up to 1e21, so an ID of
`1234567` is the label `1234567` and a ratio of `0.5` is `0.5`; booleans are
`true` and `false`. A label is one value, so an object or an array fails the
metric, handled by its [`error_mode`](#when-a-metric-cannot-be-extracted):
select one of its fields, or make one value of an array with
`.tags | join(",")`.

A label expression that gives a series no value — a selector or path that
matches nothing, a missing attribute or capture group, an empty CSV cell, a
null — leaves the label off that series, as does an empty value, which
Prometheus treats the same way; a label a [Python script](PYTHON.md) gives
as `None` or as the empty string is left off alike, and so is one a target's
own exposition writes as `l=""` under a `prometheus` transform. A `csv` label that names a column the
response does not have at all is not that: no row could give it, so the rule
fails, once for the response and whether the rule or the label is `required`
or not, as its [`error_mode`](#when-a-metric-cannot-be-extracted) says, with
the label, the column and the columns the response does have
([Reading CSV: what to expect](#reading-csv-what-to-expect)). When a series
is wrong without the label, mark it `required`:

```yaml
labels:
  - name: server
    expression: td.name
    required: true    # a row without a name is an error, not an unlabelled series
```

A series missing a required label is a missing value of its metric, handled by
the metric's [`error_mode`](#when-a-metric-cannot-be-extracted): `ignore` and
`log` drop that one series and keep the rest, `fail` fails the probe with an
error naming the label. It is counted in `http_exporter_missing_keys_total`
and, under `ignore` and `log`, in `http_exporter_rule_failures_total`, and it
applies whatever `required` and `error_handling.allow_missing_keys` say
about the value. `required` applies to `expression` labels, and not to
the python transform, whose labels come from its script.

### Reading CSV: what to expect

CSV is written by spreadsheets, databases and scripts, each in its own way.
What a `csv` rule makes of the shapes that are not the plain one:

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

A column the response does not have is one no row has: with a header row,
one the header does not name; with `header: false`, a number past the end of
the longest row; and of the rows a `pre_script` left, a key none of them
has. A row that lacks a column other rows have holds an empty cell there. A
column the header does not have is the one to look into, since nothing is
wrong with the rule: the header was split by another delimiter than
`response.csv.delimiter`; it is written `host, used` and read without
`trim_space`, so the column is named ` used`; the name is in another case
(`Used`); the file starts with a `sep=;` line, which is then the header; or
the file is in an encoding nothing declares, so the names are not the text
the rule has ([Character encodings](#character-encodings)). The message
lists the columns the response does have, the first twelve of them, in order
of their names but for a name that is the rule's in another case or with
blanks around it, which comes first, so that a table of fifty columns shows
it too; and they show which it is: one column named
`host;used`, a name with a blank before it, `Used`, `sep=;`. A value's
column that is not there is the rule's missing value, so `required: false`
silences it, whatever the cause. A label's is not: the rule fails, required
or not, since every series of it would lack the label.

The rows of a CSV answer cost the memory of the cells its lines hold. The
empty cells of the columns a short row lacks are what a rule and a script
read there, not cells kept in every row, so a header of thousands of columns
over lines of one field costs what its bytes do: a header of 2,000 columns
over 2,000 lines of `1`, 15 kB, decodes in under a megabyte. The rows of a
plain file hold some times its size while it is scraped, the most for the
shortest lines: about 23 times for lines of one character, 3.5 times for ten
columns of a monitoring export. That is before `limits.max_metrics` is
looked at; `max_response_bytes` is what bounds it.

A `pre_script` of a `csv` transform is given the rows and leaves the rows:
`data` is a list, each row a dict by the header's names, or with
`header: false` a list of the row's fields. It may drop rows, change cells
and add columns. Anything else it leaves in `data`, and a row that is
neither a dict nor a list, fails the scrape with a message that says what
the script left ([Python](PYTHON.md#what-data-is)), counted as a script
error in `http_exporter_script_errors_total`.

### XML namespaces

In an XPath expression a name without a prefix, such as `entry` or `@unit`,
matches what the document writes without a prefix. A name with one, such as
`m:size`, is matched against the prefixes as the document writes them, which
works until the target picks another prefix for the same namespace, and
cannot tell an `entry` in one default namespace from an `entry` in another.

`response.namespaces` maps prefixes of your choosing to namespace URIs, for
all of the collector's XPath expressions, the metrics' and the labels' alike.
A prefixed name then matches by the URI, whatever prefix the document uses
for it, or none:

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
expressions are its own: one it does not map, the document's `m` here, is
refused when the configuration loads, rather than matching nothing on every
scrape.

The prefix `xml` needs no mapping and takes none: XML reserves it for
`http://www.w3.org/XML/1998/namespace`, which no document declares, so
`@xml:lang`, `../@xml:lang` and `//entry[@xml:lang='en']` work with
`response.namespaces` set or not, whatever the map holds for `xml`.

#### Attribute labels

A label that is `@` and the name of one attribute of the node is read
straight from the node, by the name as the document writes it:

- **In XML**, a plain name — `@name`, `@data-id`: letters, digits, `_`, `.`
  and `-`, not starting with a digit, `.` or `-` — always. A prefixed name,
  `@m:unit`, is read that way too while `response.namespaces` is not set: the
  prefix is then the document's own, as it was before there were namespaces
  to set. With `response.namespaces` set, a prefixed attribute is XPath: its
  prefix means what the map says, `@x:unit` above, and one the map does not
  have, other than `xml`, is refused when the configuration loads.
- **In HTML**, which has no namespaces, any name at all: `@xml:lang`,
  `@og:type`, `@v-on:click`, `@x-on:click.prevent`, `@:href`, `@@click` for
  the attribute `@click`, `@2x`. A colon is a character of the name, and
  `response.namespaces` plays no part. An attribute of a parent is read the
  same way, `../@og:type`, also where the rule selects text nodes, as
  `//td/text()` does, and so is one HTML gives a namespace inside `svg`
  or `math`, `@xlink:href`. Names are as the HTML parser keeps them: in
  lower case, whatever the page wrote, except inside `svg` and `math`, where
  the parser keeps the capitals those languages have, as in `@viewBox`
  ([Reading HTML: what to expect](#reading-html-what-to-expect)). Deeper in
  an expression XPath cannot say such a name; write `@*[name()='og:type']`
  there.

A name is everything after the `@` when it holds nothing XPath takes for an
operator: no `/`, `|`, `[`, `]`, `(`, `)`, `=`, `<`, `>`, `!`, `+`, `*`, `,`,
`$`, quote or blank. Anything else that starts with `@` is XPath like every
other label, and is checked when the configuration loads: `@id | @name` for
the first of two attributes that is there, `@state = 'ok'` for `true` or
`false`; one that is neither a name nor XPath, such as `@[attr]`, is refused
there. With `decoder.type` left unset or at `auto` a label is accepted when
either kind of document would read it. If the answer is then of the kind that
cannot — XML, for a label like `@2x` or `@:href`, names only HTML can
have — the label is not left off: the metric fails, by its
[`error_mode`](#when-a-metric-cannot-be-extracted), saying which label cannot
be read in which kind of document. Setting `response.namespaces` says the
collector reads XML: its labels are then checked as with `decoder.type: xml`
even while the decoder is unset or `auto`, so a prefix the map does not
have, as in `@m:unit` or `../@m:kind`, is refused when the configuration
loads, and so is a name only HTML can have; the error says to set
`decoder.type: html` if the target answers HTML, where such a label is an
attribute's name as written and `response.namespaces` plays no part.

### Collector-wide labels

Three `transform` settings change the labels of every metric a collector
exports, whatever its transform — jq, CSS, XPath, CSV, regex, Python or a
Prometheus passthrough — after its metric rules, in this order:

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

Renames are made at once, from the labels as they were before any of them, so
they never chain: with `a: b` and `b: c`, `b` gets the value `a` had and `c`
the value `b` had. Two renames to the same label are refused at startup.

A `transform.labels` value replaces a label of the same name that a rule, a
script or the target gave a series: with `labels: {site: dc1}`, a series a
rule labelled `site="rack1"` is exported with `site="dc1"`. A value written
`""` is the label left out, as a key written `""` is the key left out
([Editor support](#editor-support)): `labels: {site: ""}` adds no label,
where it used to export `site=""`, and a series that has a `site` of its own
keeps it.

A `transform.labels` value may hold `{{param_<name>}}` placeholders, which
the probe's parameters fill as they fill the request's, so one collector
labels its series with a value only the scrape knows:

```yaml
transform:
  type: jq
  labels:
    tenant: "{{param_tenant}}"      # &param_tenant=acme -> tenant="acme"
    region: "{{param_region:eu}}"   # eu unless the probe says otherwise
```

A probe that leaves out a parameter without a default is answered `400`
before the target is contacted, and a value filled to nothing — an empty
default the probe does not replace — is the label left out, as `""` is. The
rules are in [In label values](REQUESTS.md#in-label-values).

A `prometheus` transform passing metrics through without `metrics` rules can
also pick and rename them: `include` and `exclude` are patterns a metric name
must, or must not, match, and `rename` maps source names to new ones. With
`metrics` rules, the rules choose and name the metrics, so these three are
refused at startup there, as on any other transform, rather than ignored.

An entry of `include` or `exclude` is a regular expression, matched anywhere
in the name the target gives the metric, before `rename` and
`metrics_prefix`: `^node_` picks the names that start with `node_`, and
`load` those that have `load` in them. An entry that is the empty string, or
nothing but blanks, is refused at startup, naming the collector and the key.
The empty pattern matches every name, so `exclude: [""]` dropped every series
and `include: [""]` kept every one; a pattern of blanks matches only the
names that hold those blanks, so `include: ["  "]` exported nothing, with no
error. Neither is what such an entry was written for — an empty string, or
one that picked up spaces, a template that filled in nothing. To match every
name write `'.*'`, and to filter nothing leave the key out; a pattern that
does mean a blank, which a [UTF-8 name](#utf-8-names) may hold, is written
`'[ ]'` or `'\x20'`, in single quotes, where YAML leaves the backslash to the
regular expression.

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
The exporter answers in the classic text format or OpenMetrics (see
[OpenMetrics](#openmetrics)), whose names are limited to
letters, digits, `_` and, for metrics, `:`, as are the names older
Prometheus servers, recording rules and dashboards expect. A name outside
that can come from a `prometheus` transform passing a target through, a Python
script or a pre-script, or be written in the collector itself, as a rule's
name. `name_escaping` says what happens to it:

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

A name the collector writes itself is held to the same, when the
configuration loads: a rule's `name` and the `name` of each of its labels,
under every transform, the keys of `transform.labels`, and what
`transform.rename` and `transform.rename_labels` rename to. Each leaves the
transform beside the names the response gave and is escaped with them. A
rule's label or a key of `transform.labels` that `transform.rename_labels`
renames or `transform.remove_labels` removes is the exception: the renames
and removals come before the names are escaped and the series validated, so
such a name is never exported, and it loads whatever its characters, `a.b`
under `fail` or `__tmp` alike, unless it is nothing but blanks; the name it
is renamed to is held to everything here.

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
of that name, matched whole as it is written, so under `underscores` or
`values` `name: http.server.duration` passes that metric on; a `python`
rule's `name` and its labels' names are the script's, as the script gives
them. What a rule says of its series finds them by the names as written,
before they are escaped: a label's `value_map`, `truncate: true`,
`transform.remove_labels` and the names `rename_labels` renames. Logs, probe
errors and the rule's failures in the self-metrics name the rule as it is
written.

Three things hold whatever `name_escaping` is. A name of nothing but blanks
is no name. A name is not exported beginning with `__`, which Prometheus
reserves: `__up` is refused as written, and under `underscores` so is a
name that it would turn into one, such as `1_min.load`, whose leading digit
becomes `_`, with an error naming both; `values`, which writes `U__` before
every name it escapes, takes it. And a
[static target](STATIC-TARGETS.md)'s `labels` are classic names: they are
added to what the collector exported, after its names were escaped. Two
rules whose names differ as written and are one name once escaped, `a.b`
and `a_b` under `underscores`, load, since the load compares what is
written, and fail each scrape that has both as the duplicate series, or the
metric of two types, that two such names of a target are.

The prefix applies to every metric the collector produces, whatever the
transform: declared metrics, the names a Python script passes to `metric(...)`,
and the source names a `prometheus` transform passes through or renames. A
histogram or summary keeps its family, so `_bucket`, `_sum` and `_count` follow
the prefixed name. Those names are the family's: a metric of its own named
`foo_count` next to a histogram or summary `foo`, as a `rename` can make,
fails the scrape naming both, since the exposition would hold two families
of that name. /probe, OTLP export and static targets all see the same
prefixed names, and the response cache is keyed by the collector's definition,
so a changed prefix never serves metrics cached under the old names. The
exporter's own `http_exporter_*` metrics, including a static target's health
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
longer than `limits.max_metric_name_length` (200 by default), as does a rule's
name over it without a prefix
([Checked when the configuration loads](#checked-when-the-configuration-loads));
a name produced at scrape time, by a Python script or a `prometheus`
transform, is checked against the same limit when the scrape happens.

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
label value. A label that yields one value applies it to every series, and one
that yields none leaves the label off. Any other count than one per series
means values would land on the wrong series — a label that yields nothing for
one server would shift every later label along — so the metric fails under its
`error_mode` instead, the error saying how many values the label gave for how
many series. The metric then fails as a whole, once: none of its series is
exported, one failure is counted and logged for it — the label's, not one
for each value that could not be read as well — and under `fail` the scrape's
error names the pairing mistake. The same holds for whatever else fails the
metric as a whole: its expression failing at any of its values, a label
expression failing, or a label value that is an object or a list. With
`items` there is nothing to pair: a label that yields nothing
for an item is simply absent on that series.

Per item, the value and each label must yield at most one value; two is an
error, since there is no telling which belongs to the series. A value that is
missing or null for one item is that item's missing metric, handled by
`required` and `error_mode` like any other: `log` drops that one series and keeps
the rest. `items` selecting nothing is a missing metric when the metric is
required, and produces nothing when it is not. `$root` works without `items`
too, where it is the same document as `.`. Every jq and yq expression also
has `$status`, the response's HTTP status, and `$headers`, its headers by
lower-case name, for a target whose status or headers say something the body
does not (see [Accepting other statuses](REQUESTS.md#accepting-other-statuses)).

The `css` transform takes `items` too, for HTML tables and lists: `items`
selects the rows, and the expression and each label are selectors within one
row. Without it, the metric is one series: the value is the whole text of the
one element the expression selects, and an expression matching several is a
failure of the rule, handled by its `error_mode`, with an error pointing at
`items`. Its labels can only be static `value` labels: a label reading the page
needs `items`, and is refused at startup without it.

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

Write a selector that holds a `#` in quotes, as above. In YAML a `#` after a
blank starts a comment, so `expression: body #proxy` without them is the
selector `body`, which loads and reads the whole page, and `expression:
#proxy` is no expression at all: the key has nothing after its colon, and
is refused as such.

#### The text of an HTML element

Rules over HTML are written against what a browser's inspector shows, and
`css` and `xpath` read the text of an element accordingly. It is the text of
everything inside the element, comments left out, trimmed of the whitespace
around it, with two exceptions:

- **Scripts and styles.** Text inside a `<script>` or a `<style>` is no part
  of the text of an element around it: `<td>6<script>track(6)</script></td>`
  reads `6`. A rule or a label that selects the `<script>` or the `<style>`
  itself, or a text node inside one, reads its text, so `script#state` and
  `//script[@type='application/ld+json']` still give the data a page keeps in
  a script.
- **Templates.** The content of a `<template>` is not part of the document,
  as it is not in a browser: selectors, `items`, XPath expressions and labels
  match nothing inside one, so a placeholder row such as
  `<tr><td>{{name}}</td></tr>` is no row of its table. The `template` element
  itself is there, empty. A declarative shadow root is the exception: a
  browser renders the content of a `<template>` that has a `shadowrootmode`
  attribute (or the older `shadowroot`), so it is kept, and read as ordinary
  elements inside the `template` element, which `p.shadow` and `//p` find.

Everything else in a page is read as it is written: an element hidden by an
attribute or a style is in the document and has its text, the markup inside
`<noscript>` and `<iframe>` is their text and holds no elements to select,
the fallback inside an `<object>` is elements like any other, and `<pre>`
and `<textarea>` keep their text. Leave out what a rule should not read with
the selector, as in `tr:not([hidden])` or `//tr[not(@hidden)]`.

This is the exporter's reading of the node a rule or a label selects. What an
expression itself asks of the library is the library's: the XPath functions
and comparisons — `string(.)`, `normalize-space(.)`, `sum(//td)`,
`td = '6'` — work on XPath's string value, and `:contains()` looks in all the
text of an element, a script's and a style's included: to `sum(//td)` the
cell above is `6track(6)`, which is no number, and the sum has
[no value](#adding-up-nodes-with-sum). An XML document has no
scripts, styles or templates, only elements of those names, which are read
like any other.

#### Reading HTML: what to expect

A page is read as a browser's parser builds it, which is not always as its
source reads, and `css` and `xpath` differ in what they can say:

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

Label values are capped by `limits.max_label_value_length`, 500 bytes by
default, and a value over the cap fails the whole scrape rather than one series:
a silently shortened value would be a surprise. The failure names the metric,
the label, the value's length in bytes and the cap, and the two ways out:
`metric "statuspage_incident" label "message" value is 812 bytes, longer than
limits.max_label_value_length 500; a label one of the collector's rules gives
can be cut to fit with truncate: true on that label, or raise
limits.max_label_value_length`. The metric and the label are named as the
scrape exposes them, under the collector's `metrics_prefix` and the name
`transform.rename_labels` gave the label, and the label may be one no rule
gives — a directory collector's `file`, a label of a series a `prometheus`
transform passes through as it is — for which the limit is the way out. A
label that carries free text can ask to be cut instead:

```yaml
labels:
  - name: message
    expression: .latest_update
    truncate: true
```

A longer value is then cut to the cap, on a character boundary, and ends in
`…`, which counts towards the cap. A cap of 1 or 2 bytes has no room for the
`…`, which is three, and the value is cut without it; when the value's first
character does not fit either — `日本` under a cap of 2 — nothing is left,
and the label is left off the series, as every label with an empty value is,
where it used to be exported as `message=""`. Truncation applies to declared metrics from
every transform, a `prometheus` rule without a `name` included, before any
`metrics_prefix` is added and before `transform.rename_labels`, so a label
keeps its `truncate: true` under the name a rename gives it, and after a
label's [`value_map`](#mapping-text-to-values-and-scaling-them), for every kind of rule, so the
value exported is the one cut, never a cut value mapped long again. Like a
`value_map`, `truncate: true` goes by the metric's name: set on a label in
one rule, it cuts that label of every series of the rule's name, those of
the other rules of that name included, and those a `prometheus` rule
without a `name` keeps under that name. Text that is not
the UTF-8 it claims is [repaired](#character-encodings) first, so a value cut
to the cap stays within it. A `python` script's labels are cut the same way
when a rule of the collector names the script's metric and the label:
`metrics: [{name: up, labels: [{name: note, expression: note, truncate:
true}]}]` cuts the `note` label of the `up` series the script makes (the
rule makes no series itself, and its `expression` is not read). Without such
a rule the script cuts them, or the limit is raised. Naming a label to cut is
all a `python` rule's label does, so one that sets `value`, and one that
does not set `truncate: true`, is refused at startup: the script's series
would not get the constant, nor the label an `expression` there seems to
read, and nothing would say so. A constant for every series of the collector
goes under [`transform.labels`](#collector-wide-labels), and one for some of
them in the script's `metric(..., labels={...})`. The rule itself names the
script's series and says nothing else: its `type`, `description`, `required`
and `error_mode` are the script's to say and are refused, as a rule without
a `name` is; see
[What a rule of a `python` collector is for](PYTHON.md#what-a-rule-of-a-python-collector-is-for).

A static label whose value a [probe's
parameter](REQUESTS.md#in-label-values) fills is measured as the text it is
filled to, and with `truncate: true` cut as that text is.

The other limits on a series say the same when a scrape fails for them: more
labels than `limits.max_labels_per_metric` (20 by default) as `metric "M" has
24 labels, more than limits.max_labels_per_metric 20; drop labels it does not
need or raise limits.max_labels_per_metric`, a help text over
`limits.max_help_length` as `metric "M" help is 2100 bytes, longer than
limits.max_help_length 2000; shorten it or raise limits.max_help_length`, and
a name over `limits.max_metric_name_length` as `invalid metric name "M":
longer than limits.max_metric_name_length 200` — a name the target or a
script gives, since a rule's own name over it is refused at startup.

### Long label names

A label's name is capped by `limits.max_label_name_length`, 200 bytes by
default, as a metric's name is by `limits.max_metric_name_length`. A name over
the cap fails the whole scrape at validation, whatever gave the series the
label — a series a `prometheus` transform passes through, a script's
`labels={...}`, a rule — and whatever `error_handling` says, as a metric name
over its cap does, and counts in `http_exporter_series_limit_exceeded_total`:
`metric "M" label name "L" is longer than limits.max_label_name_length 200;
rename the label with transform.rename_labels, or raise
limits.max_label_name_length`. A name past 200 bytes is shown by its first 200
and its length, `"lll..."... (10485760 bytes)`, so a target that writes a
label name of megabytes is neither served nor quoted whole. The name is
measured as the scrape exposes it, after `name_escaping`. A series whose
metric name is over its cap as well fails on the metric name, and a label
whose name and value are both over their caps fails on the name. `le` and
`quantile`, which a histogram's buckets and a summary's quantiles carry, are
label names too: a cap under 2 bytes fails every histogram, and one under 8
every summary. A label name written in the configuration — a rule's label, a
key of `transform.labels`, a `transform.rename_labels` target — that is over
the cap as it would be exported is refused at startup; a rule's label or a key
of `transform.labels` that `transform.rename_labels` renames or
`transform.remove_labels` removes is not exported under that name, and is
held neither to the cap, which its new name is, nor to its characters or the
`__` Prometheus reserves ([UTF-8 names](#utf-8-names)). A static target's label over its
collector's cap is refused when the target file is loaded, and so is every
target of a collector whose cap is under 13 bytes: the static targets
endpoint adds `static_target` to each of its series, and `collector` and
`target` to its health series, after the cap is checked. The `file` label a
collector reading a directory adds is checked with the rest, and fails the
scrape under a cap of 4 bytes.

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

[`examples/config.filebeat.json-test.yaml`](../examples/config.filebeat.json-test.yaml)
reads [Filebeat's monitoring endpoint](https://www.elastic.co/guide/en/beats/filebeat/current/http-endpoint.html),
`/stats`, `/` and `/inputs/`, into `filebeat_*` metrics: counters as `_total`,
milliseconds and nanoseconds as seconds, outcomes of one kind as one family
with a label
(`filebeat_output_events_total{outcome="failed"}`), and `error_mode: ignore` on
the sections only some Filebeats report, so they are absent rather than failing
the scrape. With the Kafka output it also reads the Kafka client's own metrics
from `libbeat.outputs`, which other outputs do not have: bytes sent and
received, requests in flight, requests sent and their latency as
`filebeat_output_kafka_request_latency_seconds{quantile="0.99"}` and friends.
Its third collector, `filebeat_inputs`, reads `/inputs/`, an array with one
object per running input, with `items: .[]`: a series per input and metric
with the labels `id` and `input` (the type), such as
`filebeat_input_pipeline_events_total{id="nginx-access",input="filestream",outcome="published"}`
for every input, `filebeat_input_files_active` and the file, message, byte and
event counters for `filestream` inputs, and
`filebeat_input_processing_time_seconds{quantile="0.99"}` for the inputs that
measure it. Filebeat reports that time as percentiles of a sample, not as
buckets, so it is gauges with a `quantile` label rather than a histogram. Every
rule there is `required: false`, as an input reports the metrics of its type
and no others and a Filebeat may answer `[]`: what is not reported has no
series, and nothing is logged.

[`examples/open-meteo/`](../examples/open-meteo/config.yaml) reads the current
weather of a place from [Open-Meteo](https://open-meteo.com), which needs no
API key. Its `config.yaml` takes the place from the probe — `latitude:
"{{param_latitude:42.6977}}"`, Sofia unless the probe says otherwise — asks for
metres per second, and turns the answer into `weather_*` series that carry the
grid point the API answered for, with percentages as ratios and the
observation time as Unix seconds. A repeated probe of one place is answered
from memory for five minutes, and while the API is down the last result is
served, marked stale, for an hour (`cache.ttl`, `cache.stale_if_error`):

```sh
curl 'http://localhost:8080/probe?collector=open_meteo_current&target=https://api.open-meteo.com&param_latitude=43.2141&param_longitude=27.9147'
```

The [`static-targets.yaml`](../examples/open-meteo/static-targets.yaml) beside
it has the exporter scrape three places itself, every ten minutes; see
[Static targets](STATIC-TARGETS.md#the-target-file).

[`examples/config.ecb.xml-test.yaml`](../examples/config.ecb.xml-test.yaml)
reads the [euro foreign exchange reference rates](https://www.ecb.europa.eu/stats/policy_and_exchange_rates/euro_reference_exchange_rates/html/index.en.html)
the European Central Bank publishes every working day, an XML document with
every element in a namespace and the data in attributes. The collector gives
the two namespaces prefixes of its own with
[`response.namespaces`](#xml-namespaces), selects the `rate` attributes
themselves, `//e:Cube[@currency]/@rate`, and reads each one's currency from the
attribute beside it, `../@currency`; a second rule computes its value,
`count(...)`; and a third reads the day the rates are for, the `time`
attribute, as a value with
[`time_format`](#reading-a-date-or-a-time-as-the-value), so no series changes
its labels from one day to the next:

```sh
curl 'http://localhost:8080/probe?collector=ecb_reference_rates&target=https://www.ecb.europa.eu'
```

```text
ecb_euro_reference_rate{currency="USD"} 1.1712
ecb_euro_reference_rates{sender="European Central Bank"} 29
ecb_euro_reference_rates_timestamp_seconds 1.7908992e+09
```

[`examples/config.scrapethissite.html-test.yaml`](../examples/config.scrapethissite.html-test.yaml)
reads an HTML page, [Countries of the World](https://www.scrapethissite.com/pages/simple/)
on scrapethissite.com, a site made for practising scraping, with the `css`
transform. The page holds one `div.country` per country:
[`items`](#metrics-per-item) selects those blocks, and the population, the
area and the country's name are selectors within one block, the name a
[`required`](#collectors) label, so every country is a series of each
metric, named by the page itself:

```sh
curl 'http://localhost:8080/probe?collector=countries_html&target=https://www.scrapethissite.com'
```

```text
country_population{country="Andorra"} 84000
country_area{country="Andorra"} 468
```

[`examples/metar/`](../examples/metar/config.yaml) reads an airport's latest
weather report, its METAR, from the text file the US National Weather Service
keeps for every station in the world — `LBSF 030900Z 12004KT 9999 FEW043
BKN100 14/08 Q1021 NOSIG` — with the `text` decoder and one regex per metric.
The station is a [path parameter](REQUESTS.md#path-parameters),
`{{param_station:LBSF}}`. Every regex but one starts at the report's line with
the station's code in a group a `station` label reads, and has the number
further along in the group named `value`; the one for the report's time
starts at the line before it. A report may end with a forecast (`BECMG
27015G25KT 3000`) and with remarks, whose groups are written like the
observation's own, so each rule says where its group stands and does not look
for it anywhere on the line: the wind directly after the station and the
time, the visibility directly after the wind, the pressure directly after the
temperature. A report with no gust, a variable wind or a wind in metres per
second then gives no such series, and never the forecast's. A report leaves
out what it has nothing to
say about and writes some things in one of two ways, so the rules are
`required: false`, and two rules may give one metric: a temperature is read by
one rule as `14` and by another, with `scale: -1`, as `M05`, and a pressure in
inches of mercury (`A3012`) is scaled to hectopascals beside the rule for
`Q1021`. Knots become metres per second the same way. The line before the
report, `2026/10/03 09:00`, is when it was made, which
[`time_format: "2006/01/02 15:04"`](#reading-a-date-or-a-time-as-the-value)
reads as `metar_observation_timestamp_seconds`:

```sh
curl 'http://localhost:8080/probe?collector=metar&target=https://tgftp.nws.noaa.gov&param_station=EGLL'
```

The [`static-targets.yaml`](../examples/metar/static-targets.yaml) beside it
scrapes three airports every ten minutes, each adding its `city` label to the
`station` the collector reads from the report.

[`examples/config.mempool.json-test.yaml`](../examples/config.mempool.json-test.yaml)
reads three endpoints of [mempool.space](https://mempool.space/docs/api/rest)'s
REST API, which every self-hosted mempool instance serves too, with three
collectors that share one request through a YAML anchor: the recommended fee
rates, one series per key of the answer with
[`items: to_entries[]`](#metrics-per-item) and the keys renamed by a label's
[`value_map`](#mapping-text-to-values-and-scaling-them); the backlog of
unconfirmed transactions, its fees scaled from satoshis to bitcoin; and the
height of the newest block, an answer that is a number and nothing else, read
as text by a regex.

[`examples/config.promdemo.prometheus-test.yaml`](../examples/config.promdemo.prometheus-test.yaml)
passes a Prometheus server's own `/metrics` through, the project's
[public demo server](https://prometheus.demo.prometheus.io) or any other. It
has no metrics rules: [`include` and `exclude`](#collector-wide-labels) keep
some two dozen families out of everything the server exposes, a histogram and
a summary whole among them, and `rename` gives the `process_*` and `go_*`
families, names every Go program exposes, names of the server's own:

```sh
curl 'http://localhost:8080/probe?collector=prometheus_server&target=https://prometheus.demo.prometheus.io'
```

[`examples/config.grafanastatus.json-test.yaml`](../examples/config.grafanastatus.json-test.yaml)
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

### Mapping text to values and scaling them

When a status has an order — up, degraded, down — one number per status is
simpler than a series per status. `value_map` turns the text an expression
gives into the value, and `"*"` maps any value it does not list:

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

The value is looked up as text: a regex capture, a CSS element's or XPath
node's text or a CSV cell without its surrounding blanks, a jq or yq number as
JSON writes it (`7`, `0.5`) and a boolean as `true` or `false`. Case matters.
`"*"` catches numbers too; without it, a value the map does not list is read as
a number, and one that is not a number fails the rule, saying so.

The error names the value as you would read it: text quoted, cut to its
first 64 bytes when it is longer — `metric "state": value "n/a" is not a
number; map text to numbers with value_map` — and an object or an array by
what it is rather than its whole content — `value is an object with 2 keys,
not a number`, `an array of 3 items`, `null` — which usually means the
expression stops one field short. A jq or yq boolean, `true` or `false`, is
read as `1` or `0`; the words as text, in a CSV cell or an HTML element, are
[not numbers](#what-counts-as-a-number).

#### What counts as a number

Text is a number when it is written as one: an integer or a decimal, with or
without digits on either side of the point (`42`, `-17`, `007`, `3.14`, `.5`,
`5.`), an exponent (`1.5e3`, `2E-3`), a leading `+` or `-`, and `NaN`, `Inf`
and `Infinity` in any case, the last two with a sign or without. Blanks around
it do not matter, and an integer too long for a float is read as the nearest
one: `9007199254740993` is `9007199254740992`. A number too small for a float
is `0` (`1e-400`), and one too large is not rounded to infinity but fails its
rule (`value "1e400" is beyond the range of a 64-bit float`). That holds for a
CSV cell, a regex capture, the text of a CSS element or an XPath node, and a
jq or yq string alike.

Anything else is text, and fails its rule as `value "..." is not a number`:
a thousands separator (`1,234`, `1 234`, `1_000`), a decimal comma (`1,5`), a
percent sign or a unit (`85%`, `12.5 MB`), a currency, hexadecimal, octal and
binary (`0x1F`, `0x1p-2`, `0o17`, `0b101`), digits that are not ASCII, and the
words exports write in place of a value: `N/A`, `-`, `null`, and `true`,
`false`, `yes`, `no`, `on` and `off` written as text (a JSON or YAML boolean
is `1` or `0`). An empty cell is not a
failure of this kind but a [missing value](#when-a-metric-cannot-be-extracted).
The remedies are `value_map`, for the texts that stand for a value (`"N/A": 0`,
`"yes": 1`, `"1_000": 1000`), and a `pre_script`, which can rewrite a column
before the rules read it — take the `%` off, turn `1.234,56` into `1234.56`.
`value_map` reads only the texts it lists, so it suits a handful of words and
not a column of decimal commas, which is two lines of a pre-script:

```yaml
transform:
  type: csv
  pre_script: |
    for row in data:
        row["price"] = row["price"].replace(".", "").replace(",", ".")
```

Two kinds of value are numbers before the exporter reads any text, and are
the number their maker made:

- A number an XPath function or operator computes — `number(//v)`,
  `sum(//v)`, `//v * 1` — is converted by the XPath engine, which reads
  `1_000` as 1000 and `0x1p-2` as 0.25. Select the node itself, `//v`, to
  have its text read as above.
- An unquoted value of a YAML response is a number by YAML's own syntax:
  `v: 1_000` is 1000, `v: 0x10` is 16 and `v: 017` is 15. Only a quoted
  YAML string, like a JSON string, is text to a rule.

The numbers of the configuration itself are YAML's: `scale: 1_000` is 1000
there.

`NaN`, `Inf` and `Infinity` are numbers where a response writes them, as the
text of a cell or an element: that is the value the source gives, and it is
exported, so `//v` over `<v>NaN</v>` is a series whose value is NaN. A NaN
an XPath expression computes is another thing: `number(//v)` over
`<v>n/a</v>` is the XPath engine saying it could not read a number, and is
the rule's [missing value](#when-a-metric-cannot-be-extracted), with
`metric "v" XPath "number(//v)" computed NaN, not a number`. The two differ
because the first is what the source says and the second is what the engine
says of text that was no number. A `sum()` over nodes one of which is `NaN`
comes to NaN and is computed, a missing value too; one over infinities of
one sign is that infinity, and is exported.

`scale` multiplies the value, mapped or read as a number — `0.001` for
milliseconds to seconds, `100` for a fraction to a percentage. A scale that is
exactly one over a whole number, as `0.001`, `0.000000001`, `1e-9` or `0.5`
is written, divides by that number instead, so 412 milliseconds are `0.412`
seconds, not the `0.41200000000000003` multiplying by the binary
approximation of `0.001` gives. A scale that only comes close to such a
number, `0.3333333333` or `1.5e-9`, multiplies like any other:

```yaml
- name: api_latency_seconds
  expression: .latency_ms
  scale: 0.001
```

Both work in every transform with rules: `regex`, `css`, `xpath`, `csv`, `jq`
and `yq`.

A label takes a `value_map` too, from text to text — a code to its name — with
`"*"` for any other value; without a match and without `"*"`, the value is kept
as it is, and a value mapped to `""` leaves the label off:

```yaml
- name: worker_up
  items: .workers[]
  expression: 1
  labels:
    - name: state
      expression: .state
      value_map: {"1": running, "2": stopped, "0": "", "*": unknown}
```

A label's `value_map` needs a rule with a `name`, and an expression to map;
it is refused on a static label, on a `python` rule, and, mapping a value to
`""`, on a `required` label. A `prometheus` rule takes `scale`, for plain samples only — a
histogram's or summary's bounds are values too — and not `value_map`, since its
values are numbers already; a `python` script sets its values itself, and
takes neither. `scale` must be a finite number other than 0.

Rules of one `name` make one metric, so a label they share must map alike:
two such rules giving one label different `value_map`s are refused at load,
since the same value would read as two names in one series. Give them one
`value_map`, or different names. The map goes by the series' name: a
`prometheus` rule without a `name` keeps each series' own, and its series of
a name another rule has take that rule's `value_map`, its static labels
included. A label with `truncate: true` is cut after it is mapped, so the
value exported is within the limit (see [Long label values](#long-label-values)).

#### Adding up nodes with `sum()`

The XPath engine's `sum()` leaves out of the sum, without a word, every node
it cannot read as a number, and a number with blanks around it — the cell of
a table printed one cell to a line — is one it cannot read: the sum of a
column was a wrong number, often `0`, and no error. XPath itself has the sum
of text that is no number NaN. So the exporter reads the nodes of a `sum()`
itself, and a sum has a value only when every node it adds up is a number:

- An expression that is one `sum(...)` and nothing else, as
  `sum(//td[@class='bytes'])`, is added up by the exporter: each node's
  text without the blanks around it, read as the engine reads a number. A
  node that is not a number — `n/a`, `1,234`, `12 MB`, an empty cell —
  leaves the rule [missing its value](#when-a-metric-cannot-be-extracted),
  by `required` and `error_mode` as any missing value:
  `metric "bytes" HTML XPath "sum(//td[@class='bytes'])" cannot be computed:
  it adds up text that is not a number, first "n/a" (2 of 14 nodes)` (over
  XML it says `XPath`). Leave
  such nodes out with a predicate, as in
  `sum(//td[@class='bytes'][number(.) = number(.)])`, or select the nodes
  with a rule of their own — where `value_map` or a `pre_script` makes
  numbers of the text — and add the series up in PromQL.
- Where `sum()` is a part of a larger expression, as in
  `sum(//v) div count(//v)` or `round(sum(//v))`, the engine computes it
  with the rest and reads each node as it stands. The rule is missing its
  value when the engine would leave a node out, and there blanks around a
  number count: `metric "mean" XPath "sum(//v) div count(//v)" cannot be
  computed: sum(//v) leaves out text it cannot read as a number, first
  " 12 " (1 of 3 nodes), and blanks around a number count`. XPath 1.0
  cannot trim each node of a set — `normalize-space()` takes one — so
  select the nodes with a rule of their own and add the series up in
  PromQL, or rewrite the text in a `pre_script`.
- A label is held to the same: `sum(../td)` is the sum of all its cells,
  and one with a cell that is no number is a label that cannot be read. A
  label has no missing value, so its series fails, as the metric's
  `error_mode` says, with an error that names the metric, the node and the
  label, and the series of the other nodes are made.

This reaches the calls of `sum()` that are evaluated where the expression
is: outside every predicate. A `sum()` inside a predicate, as in
`//row[sum(v) > 10]`, is evaluated by the engine for each node the
predicate is asked of, and still leaves out what it cannot read. An
argument that is a number or a string and no node-set is the engine's as
well: `sum(count(//v))` is that number, and a string, as in
`sum(string(//v))` or `sum(translate(//v, ',', ''))`, is the number it reads
as, with no blanks around it. A string that is no number is one the engine
fails on, which is
[the rule's failure](#when-the-xpath-engine-fails-on-an-expression):
`metric "v" XPath "sum(string(//v))" cannot be evaluated: the XPath engine
failed on it: sum() function argument type must be a node-set or number`.
Use `number(...)` for one value that may be no number, which is then the
rule's missing value. A prefix before the name is passed over, as the
engine passes over it: `fn:sum(//v)` is `sum(//v)`, alone and as a part of
a larger expression.

The nodes of a sum are read whether or not the engine would come to
evaluate it, as on the right of an `or` whose left is true. Where the
engine fails on reading them, as it does on a predicate it cannot evaluate
over the response, the sum is left to the engine: the rule has its value
when the engine never comes to the call, and fails when it does. A sum
whose nodes are all numbers without blanks is the number it always was.

### Reading a date or a time as the value

A date or a time in a response — when a report was made, a job last ran, a
certificate ends — is worth a series whose value is that moment in Unix
seconds: `time() - app_last_backup_timestamp_seconds` is then how long ago it
was. `time_format` says the text an expression gives is a time and how it is
written:

```yaml
- name: app_last_backup_timestamp_seconds
  expression: .backup.finished_at        # "2026-10-03 09:00:07"
  time_format: "2006-01-02 15:04:05"
  time_zone: Europe/Sofia
```

`time_format` is a name, in either case, or a layout:

- `rfc3339` reads `2026-10-03T09:00:00Z` and `2026-10-03T12:00:00.25+03:00`,
  with or without a fraction of a second;
- `rfc1123` reads the date of an HTTP header, `Sat, 03 Oct 2026 09:00:00 GMT`,
  and the same with a numeric zone, `+0300`;
- a layout is the reference time, `Mon Jan 2 15:04:05 MST 2006`, written the
  way the text writes its times — the convention of Go's `time` package,
  which Promtail and Telegraf take their layouts in too. Each part of the
  reference time has a number of its own, so the layout says where each part
  stands:

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

Quote a layout, so YAML hands it over as the text it is. The dots, the
slashes, a `T` between the date and the time and any other text that is none
of the parts above stand for themselves. A part is a part wherever it stands,
though, and whatever was meant by it: the digits `1` to `5` and `01` to `06`,
inside a longer number too, `15`, `2006` and `002`; the names `Jan`,
`January`, `Mon` and `Monday`; `PM`, `pm` and `MST`; `-07` and `Z07`; and
zeros or nines after a dot or a comma. In `"Q4 2006-01-02"` the `4` is the
minute, so the text may have any number there; the `3` of `"… UTC+3"` is an
hour, and the layout is refused for having no `PM` beside it. When the text
has such digits or words around its time, have the rule's expression give the
time alone, as a regex's capture group does, and write the layout for that.

A fraction of a second needs nothing in the layout: seconds written `05` read
`07`, `07.5` and `07,123456` alike. Write `05` or, to the same effect,
`05.999999999`. Zeros after the seconds stand for exactly that many digits:
`05.000` reads `07.123` and neither `07` nor `07.5`. A year of two digits,
`06`, is one of 1969 to 2068: `69` to `99` are in the 1900s.

A layout is written without blanks before or after it, as the text is read
without its own, and the hour of a 12-hour clock needs `PM` (or `pm`) in the
layout: `"2006-01-02 03:04:05"` would read no afternoon, so it is refused;
write `15` for a 24-hour clock.

The value is the time in Unix seconds, a fraction of a second kept, and
[`scale`](#mapping-text-to-values-and-scaling-them) applies after: `1000`
gives milliseconds. A time already written as a number of Unix seconds needs
no `time_format`, and one in milliseconds only `scale: 0.001`.

A text that names its zone or its offset from UTC is read by it, when the
layout has the zone as one of its parts: `Z07:00`, `-0700`, `-07:00` or `MST`.
One that does not is read in `time_zone`, an IANA name such as `Europe/Sofia`
or `America/New_York`, by the offset that zone has on that day, summer time
included; without `time_zone` it is read as UTC. A zone typed into the layout
as the text has it — the `Z` of `"2006-01-02T15:04:05Z"`, a `UTC` or a `GMT`
— is text to match and no zone, so such a text is read in `time_zone` as
well: write `Z07:00` or `MST` in its place. The zones are built into the
binary, so they are known in an image or on a host that has no zone files. An
abbreviation in the text (`MST` in the layout) is read as one of
`time_zone`'s own — `EET` and `EEST` with `Europe/Sofia` — or as `UTC` or
`GMT`; `GMT+3` and `GMT-5`, `GMT` with a whole number of hours, are that many
hours ahead of UTC and behind it. Any other abbreviation fails the rule, since
an abbreviation alone does not say an offset.

On the night the clocks change, a local time without a zone of its own is one
of two moments, or none. It is then read as Go's `time.Date` reads it, by the
offset before the change or the one after, with no promise of which: in
`Europe/Sofia`, `2026-10-25 03:30`, which the clocks show twice, is read as
the second, in winter time (01:30 UTC), and `2026-03-29 03:30`, which they
skip, as an hour after 02:30 (01:30 UTC). A source that writes UTC or its
offset has no such hour.

The text is read without its surrounding blanks, like `value_map`'s. It works
in every transform with rules: a regex capture, a CSS element's or XPath
node's text, a CSV cell, a jq or yq string. A jq or yq value that is not text
— a number, an object — fails the rule, saying `time_format` reads text. An
empty or absent text is a missing value, as for any rule. Text that is no time
in the format fails the rule, under its
[`error_mode`](#when-a-metric-cannot-be-extracted), naming the text and the
format: `metric "app_last_backup_timestamp_seconds": value "never" is not a
time in time_format "2006-01-02 15:04:05"; write the layout as the text writes
the reference time, Mon Jan 2 15:04:05 MST 2006`.

`time_format` and `value_map` do not go together, since each turns the text
into the value, and neither a `python` nor a `prometheus` rule takes
`time_format`. A layout must have a year, a month and a day: `15:04` alone is
no moment, and `yyyy-mm-dd` or `%Y-%m-%d`, layouts of other conventions, hold
no part of the reference time at all. A sample date, `2026-10-03`, or the
name of another format, `iso8601`, is read as a layout too, its digits as
parts, and refused for what that layout lacks or cannot read, in words that
end with what a layout is. All of this is
[checked when the configuration loads](#checked-when-the-configuration-loads).

Either key written empty, `time_format: ""` or `time_zone: ""`, is the key
left out, as an optional key of free text is throughout the configuration: a
rule with `time_format: ""` reads its value as a number, and may have a
`value_map`; `time_zone: ""` is UTC beside a `time_format` and nothing
without one; and `time_format: ""` beside a `time_zone` that names a zone is
refused as `time_zone` without `time_format`. The
[schema](#editor-support) says the same of each.

### Conditional metrics and labels

There is no `when` key: each transform already says in its own language which
values become series, and a second, shared condition language would repeat it.
The condition goes where the transform reads the value.

**jq and yq.** `select(...)` keeps the items that match, and
`if ... then ... else empty end` gives a value only when a condition holds;
`$status` and `$headers` are there for conditions on the answer itself. A rule
whose condition can leave it without a value needs `required: false`, or no
value is a missing value, logged under `error_mode`:

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

**CSS.** `items` takes any selector, `:has()` included, so the rows kept are
those that hold what the condition looks for:

```yaml
- name: queue_depth
  items: 'tr:has(td.state.active)'
  expression: td.depth
  labels:
    - name: queue
      expression: td.name
```

**Regex.** The pattern is the condition: only text it matches becomes a
series. `(?m)^(\d+) (\w+) up$` reads the lines of services that are up, the
first group the value and the second the `service` label (`expression: "2"`).
Where the label comes first in the line, name the value's group:
`(?m)^(?P<service>\w+) up (?P<value>\d+)$`, with `expression: service`.

**CSV.** A rule reads every row. To keep only some, filter them in a
[pre-script](PYTHON.md), which leaves the rows it keeps in `data`:

```yaml
transform:
  type: csv
  pre_script: |
    data = [row for row in data if row["state"] == "active"]
```

When the filter can leave no row at all, give the rules `required: false`, as
for a jq condition: a response without a row is a missing value of every
required rule.

**Python.** The script calls `metric(...)` for what it wants exported, under
any condition it likes.

**Labels.** A label whose expression gives no value, or `null`, is left off
the series — `if .shared then .owner else null end` in jq — and so is one whose
[`value_map`](#mapping-text-to-values-and-scaling-them) maps its value to `""`.
A `required` label turns its absence into a failure of the rule instead.

**Series that come and go.** A conditional series exists only while its
condition holds. When it stops, Prometheus marks the series stale: `rate()`
and `increase()` have gaps across it, an alert on its value stops firing
rather than resolving, and `absent()` is the only way to see it gone. When the
absence itself means something — a queue that is not active — prefer one series
that is always there, with `1` and `0`:

```yaml
- name: queue_active
  items: .queues[]
  expression: 'if .state == "active" then 1 else 0 end'
  labels:
    - name: queue
      expression: .name
```

`value_map` does the same for text in every transform: `{active: 1, "*": 0}`.
Keep conditions for series whose absence means nothing, such as the depth of a
queue that does not exist yet. The examples here are pinned by a test, so they
stay true.

### Request types

Every collector's `request` block starts with `type`, which is required and
says how the collector reaches its data:

```yaml
request:
  type: http
  path: /api/status
```

There are four types: `http` asks a URL, `localfile` reads a file from the
exporter's own filesystem — see [Local files](LOCALFILE.md) — `graphite`
asks a Graphite render API for series — see [Graphite](GRAPHITE.md) — and
`grpc` calls a unary gRPC method — see [gRPC](GRPC.md). A collector
without `type` stops the exporter at startup with a message saying what to add,
and so does an unknown type.

Each type accepts its own keys. For `http`, `type` is the only required one —
`path` can be left out when the target URL already carries the whole path, and
`method` defaults to `GET`:

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `http`. |
| `method` | `GET` | GET, POST, PUT, PATCH, DELETE or HEAD. |
| `path` | — | Joined onto the target; may use [path parameters](REQUESTS.md#path-parameters). A `%XX` escape in it is sent as written ([Target requests](REQUESTS.md#the-request-url)). |
| `query` | — | Query parameters added to the request, after the target's own query, which is sent as written; values may use [placeholders](REQUESTS.md#in-the-body-headers-and-query). |
| `headers` | — | Sent to the target; values may use [placeholders](REQUESTS.md#in-the-body-headers-and-query). Not `Accept-Encoding`, which is the exporter's own ([Target requests](REQUESTS.md#compression-and-the-response-size)). |
| `body` | — | Request body; may use [placeholders](REQUESTS.md#in-the-body-headers-and-query), encoded with `\|json`, `\|number`, `\|form` or `\|xml`. |
| `basic_auth`, `basic_auth_file` | — | Use one; see [Authentication](AUTHENTICATION.md). |
| `bearer_token`, `bearer_token_file` | — | Use one; not together with basic auth. |
| `forward_authorization`, `forward_headers` | off | See [Authentication](AUTHENTICATION.md). |
| `tls` | verify | See [Target requests](REQUESTS.md#tls). |
| `retry` | none | See [Target requests](REQUESTS.md#retries). |
| `max_response_bytes` | 10 MiB | Response size cap, on the decompressed answer; one whose `Content-Length` is over it is refused before it is read ([Target requests](REQUESTS.md#compression-and-the-response-size)). With `limits.max_response_bytes` set too, the smaller wins; either alone may be above 10 MiB. It bounds the body; the response's headers are bounded apart, at 1 MiB. |
| `follow_redirects`, `enable_http2` | off | See [Target requests](REQUESTS.md#redirects-and-http2). |
| `redirect_trusted_hosts` | none | Hosts, globs and addresses, besides the request's own origin, that a followed redirect may carry the collector's headers, credentials and body to, and present its TLS client certificate to; see [What a followed redirect carries](REQUESTS.md#what-a-followed-redirect-carries). |
| `allowed_schemes` | `http`, `https` | Schemes a target may use: `http`, `https` or both. Any other entry stops the exporter at startup. |
| `accept_status` | every 2xx | Statuses whose answers are decoded, such as `["2xx", 503]`; a status written as a number is the one YAML reads, so `503.0` and `0x1F7` are 503. See [Accepting other statuses](REQUESTS.md#accepting-other-statuses). |
| `allowed_targets`, `denied_targets` | none | Hosts, globs, addresses and networks its requests may and may not reach; see [Restricting targets](REQUESTS.md#restricting-targets). |

For `localfile`, `root` is required, and `path`, `max_age` and
`max_response_bytes` are optional, or `files`, `max_files` and
`max_total_bytes` to [read a whole directory](LOCALFILE.md#reading-a-directory);
its table is in [Local files](LOCALFILE.md#a-collector).

For `graphite`, `targets` is required: the Graphite expressions to render.
`from` and `until` set the window, `-15min` to `now` by default, and `path`
defaults to `/render`. Every `http` key about the connection applies —
`query`, `headers`, credentials, `tls`, `retry`, `max_response_bytes`,
`follow_redirects`, `redirect_trusted_hosts`, `enable_http2`,
`allowed_schemes`, `accept_status`, `allowed_targets` and `denied_targets` —
and `method` and `body` do not; its table is in [Graphite](GRAPHITE.md#a-collector).

For `grpc`, `rpc` is required, the method as `package.Service/Method`, and
so is `descriptors`, where its message types come from — `reflection`,
`protoset` or `proto` — except for the built-in health service. `message` is
the request as JSON, `{}` by default, and `metadata` its metadata; the
credential keys, `tls`, `retry` with its `codes`, `max_response_bytes`, the
forwarding keys, `allowed_targets` and `denied_targets` apply as for `http`, and the other `http` keys do not; its
table is in [gRPC](GRPC.md#a-collector).

A key that belongs to a different type is an error rather than being ignored,
and the same holds for `/probe` parameters: a parameter that only another type
accepts gets a `400`. `localfile` accepts only `path`, `timeout` and
`param_<name>` — when it reads a directory, `timeout` and only the
`param_<name>` its [label values' placeholders](REQUESTS.md#in-label-values)
name, never `path` ([Reading a directory](LOCALFILE.md#reading-a-directory)) —
and its `target` is optional. `graphite` accepts what `http` does but `method` and `body`,
and `from` and `until`, which no other type accepts. `grpc` accepts
`timeout`, `insecure_skip_verify`, `retry_attempts`, `retry_backoff`,
`header_<name>`, `param_<name>` and `message`, which no other type accepts.

`target`, `collector` and each of these parameters is given once. A second
value gets a `400` naming the parameter rather than one of the two being
used: `timeout=5s&timeout=1h` does not say which was meant. `header_<name>` is
the exception, since every value of it is forwarded. A value may be at most
8 KiB (8192 bytes), and a longer one gets a `400` naming the parameter and the
limit: what a probe names is kept after it is answered, in the
[log of repeated failures](LOGGING.md#repeated-failures) and the
[verbose self-metrics](SELF-METRICS.md#verbose-per-request-self-metrics), and
is bounded there by being bounded here. A `header_<name>` value for a header
the collector forwards may not hold a control character other than tab (a CR
or LF among them), which Go refuses to send: it gets a `400` naming the
parameter before the target is contacted, rather than a `502` retried and
logged as the target's failure. A parameter no request type knows is
not read at all, whatever its values. A whole request, its line and headers,
may be 64 KiB; the exporter answers a longer one `431`.

#### Choosing request types at build time

Every build of the exporter carries every request type, and so does the
published image. A build can instead carry only the types it needs, which keeps
the other types' code, and the libraries only they use, out of the binary:

```sh
make build REQUEST_TYPES=http
docker build --build-arg REQUEST_TYPES=http -t exporter:http .
go build -tags "$(sh tools/request-type-tags.sh http)" .
```

`make build` writes the binary to `bin/prometheus-universal-exporter`; plain
`go build .` writes it to the current directory.

`REQUEST_TYPES` is a comma-separated list of type names. Empty, the default,
means every type. A name that is not a request type fails the build, and so does
a selection that names none. The script turns the list into Go build tags —
`select_request_types` plus `request_type_<name>` per type — which is all
`go build -tags select_request_types,request_type_http` needs by hand.

A collector whose type the build left out stops the exporter at startup, saying
the type exists but this build does not include it, and which types it does. The
startup log line and the [dry run](#dry-run) report list the types the binary
carries, so a configuration can be checked against the build that will run it.
To such a build the types it left out do not exist beyond that message: the
message for a collector without `type` shows a type the build has, the schema
`--config.schema` prints lists only its types, and a `/probe` parameter that
only a left-out type accepts is one no request type knows, ignored rather
than answered `400`. Each type built on its own is a build the test suite is
run against, by CI and by `make test-request-types`
([Development](DEVELOPMENT.md#tests-of-a-build-with-only-some-request-types)).
An http-only build leaves `localfile`, `graphite` and `grpc` out, and a build
with `REQUEST_TYPES=localfile` reads files and makes no HTTP requests to
targets at all. `grpc` is the only type with libraries of its own, gRPC and
protobuf, which a build without it does not link: about 6 MB of a stripped
binary.

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
can be extracted, the probe still succeeds with an empty body. `log` writes one
warning per failing rule, however many series failed: a rule over a
thousand-row table that misses its value on every row logs its first error
with `"failures":1000`, not a thousand lines. A rule that fails the same way on
every scrape of a target is logged once and then only as a
[repeat](LOGGING.md#repeated-failures), with its recovery logged when it works
again; the same way, wherever in the response: the row, node or item its
first error names, and a size the error measured, may be another on every
scrape. Several rules of one metric name — one for each column or path its
series come from — are each logged, remembered and recovered by themselves,
and their lines say which rule it is with its `expression`, and its `items`,
beside the `metric` ([Logging](LOGGING.md)); rules alike in name, expression
and items, which their labels tell apart — alike in those too they are
[the same rule](#two-rules-that-are-the-same-rule), which does not load —
are one rule to the log, logged if either of them has `log`. Either way the
series a rule carried on
without are counted in `http_exporter_rule_failures_total{collector,
metric}`, by the rule's metric name and so for the rules of one name
together, so a rule that keeps failing can be graphed and alerted on.

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

A value is absent the same way in every transform: nothing matched, a null, or
text that is empty or only whitespace — an empty CSV cell, an empty JSON
string, an empty XML element or HTML cell, a regex group that captured
nothing. Nothing matched is a jq expression or `items` that gave nothing, a
regex that matched no text, a selector that matched no node, a `prometheus`
rule that matched no metric of the response, and a `csv` rule over a response
without a row. Text that is there but is not a number, such as `"up"`, is not
absent: it is a failure to read the value, which `required: false` does not
excuse.

What is absent is left out in silence where a rule or a label is not
required, and that hides a column, a field or a cell the response never had
as well as one that is empty this time. A `csv` rule that is required tells
the two apart, `CSV column "x" is not in the response` and `CSV column "x"
is empty in row 3`, and a `csv` label naming a column the response does not
have fails its rule, required or not, rather than being left off every
series ([Reading CSV: what to expect](#reading-csv-what-to-expect) lists
what takes a column away); a rule that is not required still says nothing of
a value's column that is gone. While writing a collector leave its rules
required, so that a column that is gone is reported, and read the header the
target sends in a [debug probe](#debugging-a-probe).

The error of a `css` or an `xpath` rule names its metric first, and, where
the failure belongs to one of the things the rule selected, which: an item
of a `css` rule's `items`, a node of those an `xpath` rule selected, each
counted from 0 in the order of the page.
`metric "host_load" value is missing for node 3: HTML XPath
"//td[@class='load']" selected a node without a value` is the fourth cell
(over XML the expression is named `XPath`), `metric "host_load"
node 3: value "n/a" is not a number` its text, and `metric "host_load" label
"host" is missing for node 3` a required label; a `css` rule says `item 3`
in the same places. It reads the same in the log, in a debug probe and in
the answer of a failed probe.

A probe that runs out of time inside a rule is not that rule's failure, and
no mode applies to it: the probe fails as a whole (see
[Probe deadlines](#probe-deadlines)).

#### When the XPath engine fails on an expression

The XPath engine accepts some expressions at startup that it then cannot
evaluate, once a response holds what they stumble over:
`//a[contains(@x, 5)]` as soon as an `a` has an `x` — the second argument
must be a string, `'5'` — `sum(string(//v))` over text that is no number,
`substring(//v, '1')`, `replace(//v, '(', '')`, `//a = true()`. That is the
failure of the rule whose expression or label it is, by its `error_mode`
like any other: the rule gives no series for that response, those of the
nodes before the one the engine failed at included, the other rules give
theirs, and the failure is counted and logged for the rule as every failure
is: as one failure of the rule, the engine's, whatever nodes of the rule
failed before the engine did — their failures are not counted beside it,
in `http_exporter_rule_failures_total` or as missing values, and it is the
engine's failure that is logged. The error names the metric, the
expression — a label's, with the label's name — and what the engine said:

```text
metric "marked" XPath "//a[contains(@x, 5)]" cannot be evaluated: the XPath engine failed on it: contains() function argument type must be string
metric "job_up" label "kind": XPath "substring(../@kind, '1')" cannot be evaluated: the XPath engine failed on it: substring() function first argument type must be number
```

What follows `failed on it:` are the engine's own words. Where they start
with `runtime error:`, as for `//a = true()`, the fault is in the engine or
in the exporter and not in an argument: write the expression another way,
as `count(//a) > 0`. What follows `runtime error:` may change with the
response — `substring(//v, 2, 10)` over a text shorter than that fails with
`slice bounds out of range [:3] with length 2`, and with other numbers over
another text — so the log takes such a failure for the same one whatever
follows, and holds back its repeats. With `--log.level=debug` the stack of
a runtime error is logged, in a line `the XPath engine failed with a runtime
error` with the collector and the metric, under every `error_mode`, `ignore`
included, as it is in a debug probe's report, which shows the lines of
every level; it is in no error text and in no line of another level.

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

A probe that carries on is answered `200` with an empty exposition in the
format the scrape asked for, exactly what a probe whose rules produced no
series is answered: the text format's `Content-Type`, or OpenMetrics' own and
its closing `# EOF`, compressed when the scrape accepts gzip. An exposition is
identified by its `Content-Type`, so the answer says what it is though it
holds nothing of the collector's, and the scrape succeeds, which is what
carrying on is for. The failure is still counted in the collector's
[self-metrics](SELF-METRICS.md), and the probe counts as a success. Nothing
is kept in the [response cache](#response-caching) or exported over OTLP.
Carrying on is not a failure, so with
[`cache.stale_if_error`](#serving-the-last-good-result-when-the-target-fails)
the last good result does not stand in for it: the answer holds the two
freshness series every answer of such a collector has, with
`http_exporter_result_stale 0`, and nothing else.

A [static target](STATIC-TARGETS.md) follows the same policies.
Under `fail` its scrape fails: `http_exporter_target_up` is `0` and, with
[`cache.stale_if_error`](#serving-the-last-good-result-when-the-target-fails),
the last good result is exported in its place. Under `log` and `ignore` the
scrape carries on as a probe does: the target is up, with nothing of the
collector's to export, only its health series, and the scrape counts as a
success. A metric rule with `error_mode: fail` fails the scrape whatever
`on_transform_error` says, on a probe and a static target alike.

### Checked when the configuration loads

Everything about a metric that can be known before a scrape is checked at
startup, on reload and by `--dry-run`, and an error names the collector, the
metric and the label. A rule that has no name to be named by — `name` left
out, written `""` or nothing but blanks — is named by its place among the
collector's rules, counted from 1: `collector "node" metrics rule 2 has no
name`, or, of a `prometheus` rule, which needs none, `collector "node"
metrics rule 2 has invalid type "timer"`, where a rule with a name reads
`collector "node" metric "up" has invalid type "timer"`. What is checked:

- a metric name must be a valid Prometheus metric name, and not start with
  `__`; a rule of any transform but `prometheus` must have one, and under
  `prometheus` a name that reads as a pattern is told that
  [a pattern belongs in `expression`](#collectors). Under
  [`name_escaping`](#utf-8-names) `underscores` or `values` a rule's name,
  and every other name the collector writes, may be any name that is not
  blanks alone, and is exported escaped. A rule's name may be no longer than
  `limits.max_metric_name_length` as it is exported — after `metrics_prefix`,
  escaped by `name_escaping` — since every series of it would fail the
  scrape: `collector "c" metric "M" is 201 bytes, longer than
  limits.max_metric_name_length 200, so every series of that name would fail
  validation; shorten the name or raise limits.max_metric_name_length`, or,
  with a prefix, `collector "c" metric "M" is exported as "p_M", which is
  longer than limits.max_metric_name_length 200`. This holds for a
  `prometheus` rule's and a `python` rule's name too; the scrape measures a
  histogram's or a summary's family name, not its `_bucket`, `_sum` or
  `_count`, and so does the load, and a `prometheus` rule without a `name`
  has nothing to measure;
- every expression must compile in its transform's language — jq and yq
  (including `items`, and undefined functions and variables), regular
  expressions, CSS selectors, XPath with the collector's namespaces, and a
  `prometheus` transform's patterns, `include` and `exclude`;
- a `regex` rule must have a capture group, which is its value, and no more
  than one named `value`; a `regex` label must name a capture group the regex
  has;
- a rule's [`time_format`](#reading-a-date-or-a-time-as-the-value) must be
  `rfc3339`, `rfc1123` or a layout with a year, a month and a day, without
  blanks around it and with `PM` beside the hour of a 12-hour clock, and its
  `time_zone` a zone the exporter knows; `time_zone` without `time_format`,
  `time_format` beside `value_map`, and `time_format` on a `python` or
  `prometheus` rule are refused;
- a `prometheus` transform's `rename` targets must be metric names;
  `include`, `exclude` and `rename` apply only to a `prometheus` transform
  without `metrics` rules, and an entry of `include` or `exclude` is neither
  the empty string nor [nothing but blanks](#collector-wide-labels);
- a label of a `python` rule sets no `value`: the script sets its labels
  itself, and a constant for every series is
  [`transform.labels`](#collector-wide-labels);
- a `python` rule has a `name`, the script's series it names, and no `type`,
  `description`, `required` or `error_mode`, which the script says of its
  series itself, and each of its labels sets `truncate: true`
  ([What a rule of a `python` collector is for](PYTHON.md#what-a-rule-of-a-python-collector-is-for));
- a `prometheus` rule's `expression` is not nothing but blanks, and the rule
  has a `name` or an `expression`, by which it says which metrics it passes
  on;
- `transform.labels` and `rename_labels` must give label names, classic ones
  unless `name_escaping` escapes the others, and two renames may not target
  the same label. A key of `transform.labels` or a rule's label that
  `rename_labels` renames or `remove_labels` removes may be any name that is
  not blanks alone, since it is never exported;
- no label name, of a rule, `transform.labels`, `rename_labels` or a static
  target, may start with `__` — unless `rename_labels` renames it or
  `remove_labels` removes it — which Prometheus keeps for its own labels: it
  refuses `__name__` in what it scrapes and drops the others. A series whose
  labels still get such a name, from a Python script or a passthrough, fails
  validation. So does a histogram given a label `le` of its own, or a summary
  a `quantile` — by a rule's label, `transform.labels` or a pre-script —
  since its buckets or quantiles carry that label: the scrape fails naming the
  series, as in `metric "h" is a histogram and has a label le of its own,
  which its buckets carry; name the label something else`. On any other
  series `le` and `quantile` are labels like any other;
- rules with the same metric name must give it the same type: several rules
  may feed one family, as `jobs{queue="a"}` and `jobs{queue="b"}` read from
  two places, but a family has one type. A `prometheus` rule without a `type`
  keeps the series' own and is not compared;
- no two rules of a collector may be
  [the same rule](#two-rules-that-are-the-same-rule): each would make every
  series the other makes, and every scrape would fail on a duplicate series;
- under `prometheus`, no rule's `expression` may match the `name` another
  rule passes on the metric of, where the two give its series
  [one name and the same labels](#a-name-and-a-pattern-that-matches-it):
  each would make every series of that metric. A rule of the histograms or
  the summaries alone, by its `type`, may;
- no rule may be named as a series of another rule's histogram or summary —
  `foo_bucket`, `foo_sum` or `foo_count` beside a histogram `foo`, `foo_sum` or
  `foo_count` beside a summary `foo`;
- a label name of a rule, of `transform.labels` or that
  `transform.rename_labels` renames to may be no longer than
  `limits.max_label_name_length` as `name_escaping` exports it, unless
  `transform.rename_labels` renames it or `transform.remove_labels` removes
  it;
- a `description` may be no longer than `limits.max_help_length`, and a
  static label value, of a rule or of `transform.labels`, no longer than
  `limits.max_label_value_length` — unless the label is cut at the scrape,
  which it is where it has `truncate: true` or any rule of the same metric
  name sets `truncate: true` on a label of that name, a `value_map` maps it
  to something shorter, or `remove_labels` drops it. A
  value that holds `{{param_...}}` placeholders is measured with each
  replaced by its default, and by nothing where it has none. Where a
  `value_map` of the rule's name maps such a value of a rule's label, it is
  measured as mapped: a `"*"` entry longer than the limit is refused whatever
  the defaults, since every value the probe gives that the map does not list
  becomes it; a longer entry the map lists is refused only where the
  defaults give its key, and otherwise loads, failing the scrape of a probe
  that gives that key. A `prometheus` rule without a `name` keeps each
  series' own name, and a `value_map` or `truncate: true` of a rule named as
  one of those series applies to it: its static label is measured as it is,
  for the names no rule has, and besides as each other rule's name its
  `expression` matches maps and cuts it — matched as at the scrape,
  unanchored unless the pattern says otherwise, against the name the target
  wrote, before `metrics_prefix` and `name_escaping`;
- an explicit `decoder.type` must be one the transform
  [reads](#collectors);
- a setting must belong to the decoder or transform the collector has:
  `transform.script` to a `python` transform, `transform.libraries` and
  `required_libs` to a collector with a `python` transform or a `pre_script`,
  `response.csv` to the `csv` decoder, `response.graphite` to the `graphite`
  decoder — either may also be left to each response with `auto` — and
  `response.namespaces` to an `xpath` transform. Anywhere else the setting
  would be ignored, so it is refused, naming it and the decoder or transform
  the collector has instead;
- `response.csv.delimiter` is one character, and not a double quote or a line
  break. For tab-separated values write `delimiter: "\t"` in double quotes,
  where YAML reads `\t` as the tab character; in single quotes, or
  unquoted, `\t` is a backslash and a `t`, and is refused;
- a `{{param_...}}` placeholder stands only where a probe's parameters are
  filled in: in the [request](REQUESTS.md#in-the-body-headers-and-query) and
  in a [fixed label value](REQUESTS.md#in-label-values), of
  `transform.labels` or of a rule's static label. In any other
  setting of a collector — `request.bearer_token`, `basic_auth`,
  `tls.server_name`, a `value_map`, a label's name — it would be
  sent or exported as written, so it is refused, naming the field. A Python
  script, an expression and a description are not searched: there the text
  is the script's, the expression's or the description's own;
- a placeholder in a label value is well formed: `{{param_<name>}}` or
  `{{param_<name>:<default>}}`, without a space after the braces and without
  a filter, which a label value does not take;
- a limit is a whole number from 0: a negative one, as in
  `limits.max_metrics: -1`, and one with a fraction, as in
  `max_concurrent_probes: 1.9`, are refused naming the key and the value
  rather than quietly becoming the default or losing the fraction. 0, like a
  limit left out, is the default. Written with a point or an exponent, as
  `1e3` or `4.0`, a limit is the whole number it equals, read by its digits,
  and one too large for an int64, as `1e19`, or even for a floating-point
  number, as `1e400`, is refused naming the key, the number and the range. [Sizes](#sizes) are held to the same;
- `limits.max_output_bytes`, when it is set, is at least 38 bytes, which a
  [Python script](PYTHON.md#how-scripts-run) that emits no metric answers
  in: a smaller one is refused, in a collector with a script and in one
  without, as `collector "c" limits.max_output_bytes is 26, and a Python
  script that emits no metric answers in 38 bytes, so no transform's script
  could answer within it; set at least 38, or leave it out, or 0, for the
  default, 1MiB`. It once loaded and failed every scrape of a collector
  whose transform is a script;
- a mapping key YAML reads as no key at all — `null`, `~`, or nothing before
  the colon, as in `value_map: {null: 0}` — is refused, since the entry would
  be dropped; quote it, `"null"`, when that text is the key;
- a value YAML reads as none — nothing after the colon or the dash, `null`
  or `~` — is refused wherever the file takes a value: a key's, an entry of
  a list, a value of a mapping. YAML hands the exporter nothing for it, so a
  key written `ttl:` was the key left out, a bare `-` among the
  `metrics` was no rule at all, with nothing to say that the rule meant
  there is missing, and `value_map: {up: }` mapped `up` to 0. The error
  names the line and the place, and says what to do: `line 12: metrics entry
  1 is a dash with nothing after it, which YAML reads as no value at all, so
  the entry would be left out without a word; write a metric rule there, or
  take the entry out`, or `line 7: ttl has nothing after its colon, which
  YAML reads as no value at all; write its value, or take the key out`. To
  leave a key at its default, leave it out; a key of text may also be
  written `""`, which is the key left out
  ([Editor support](#editor-support)), and a block that sets nothing `{}`.
  A file's own top-level `x-` keys hold what they like;
- a value is written as the kind its key takes. YAML reads an unquoted
  `true` or `False` as a boolean and `1`, `1.5e3` or `0x1F` as a number, and
  hands a key of text the text of whatever is written. Where the text is
  free — a `description`, an `expression`, a label's `value`, a header, a
  query parameter, a body, a path, a `value_map`'s keys and a label's
  `value_map` texts — that is what was meant, and `value: 1`,
  `description: 404` and `value_map: {1: running}` need no quotes. A key
  that takes a name, one of a few words, a host or a single character takes
  text alone: a collector's, a rule's and a label's `name`,
  `metrics_prefix`, `name_escaping`, `request.type`, `method`, `rpc` and
  `descriptors`, `decoder.type`, `transform.type`, the error policies, a
  rule's `type`, `error_mode` and `time_zone`, `response.csv.delimiter`,
  `response.graphite.value` and `invalid_lines`, an entry of
  `collector_files`, `redirect_trusted_hosts`, `accept_codes`, `retry.codes`
  and the Python libraries, and a static target's `name` and
  `request.method`. A number or a boolean there was read as its text, so
  that `name: true` was the metric named `true`, and is refused: `line 9:
  name is written true, which YAML reads as a boolean, not as text; to use
  that text there, quote it: "true"`. A key that takes one of a few words
  is told the words, which quoting a number would not make it one of: `line
  5: type is written 1, which YAML reads as a number, not as text; write one
  of graphite, grpc, http, localfile`. And a boolean is `true` or `false`.
  `yes`, `no`, `on`, `off`, `y` and `n` were booleans to YAML 1.1 and are
  text now, to an editor and wherever the exporter takes text — a label
  value of `NO`, for Norway, is `NO` and not `false` — so a key that takes a
  boolean refuses them, quoted or not, where it used to read them as one:
  `line 3: enabled is written yes, which YAML reads as text, not as a
  boolean; write true`. A date written without quotes, `2026-10-06`, is
  text as well, wherever text is taken, and a number or a boolean in quotes
  is text, which no key of numbers or booleans takes.

The last four look at the values YAML gives the exporter. With a
[merge key](#reusing-settings-with-yaml-anchors), a key the mapping sets
itself replaces the one merged in, and of a list of merges (`<<: [*a, *b]`)
the first that sets a key is the one read, so
`limits: {<<: *defaults, max_metrics: 500}` loads even if the anchor's own
`max_metrics` is `0.5`: that value is never read there. A value that is read
is refused, at the line it is written on. A key written as an alias,
`*k : 7.0` with `&k max_metrics` elsewhere, is the key its anchor holds, as
YAML reads it, and its value is read and refused as that key's, named so.

Each of these would otherwise load and then fail every scrape's validation,
or be ignored, whatever the target answered.

### Two rules that are the same rule

Several rules may export one metric name, each reading its series from
another place or giving them another label. Two rules that are alike in
everything that decides which series a rule makes are something else: the
same rule, written twice. Each makes every series the other makes, of one
name and one set of labels, and a scrape that has a series twice fails —
`validation failed: duplicate metric series "up"` — on every scrape, for as
long as the configuration runs. So the pair is refused when the
configuration loads, naming both rules by their places among the collector's
rules, counted from 1, and the metric:

```text
collector "node" metrics rule 1 and rule 3 are the same rule of metric "up": alike in name, expression, items and labels, each makes every series the other makes, and a scrape that has a series twice fails, as a duplicate metric series; take one of the two out, or tell their series apart by a label, as with a static label that has another value in each
```

A `prometheus` rule without a name is told of by its pattern, `… are the
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

So two rules that differ only in a `scale`, a `description`, `required` or
`error_mode` are the same rule, and are refused: whatever value each gives,
it is a second value for one series. To export one name from two rules,
tell their series apart by a label — a static label with another value in
each is enough — or by reading another `expression` or other `items`.

Labels of one name written twice in a rule keep their order, the last being
the one a series gets. Under `prometheus`, where a label reads a label of
the series the rule passes on, the order of a rule's labels is compared too
when one of them reads a label the rule itself sets under another name,
since the order then decides the value.

The comparison is of what the rules write, not of what they mean. Two
expressions that select the same thing in other words — `.v` and `(.v)`, or
the `prometheus` expressions `'^up$'` and `'^(up)$'` — are two rules to it:
they load, and the scrape fails on the duplicate series as before. A
`python` collector's rules make no series, the script does, so two of them
alike change nothing and are not refused. A JSON schema cannot compare the
items of a list in this way, so this is one of the checks only the exporter
makes ([Editor support](#editor-support)).

### A name and a pattern that matches it

One pair of `prometheus` rules that select the same in other words is
decided by the configuration alone, and is refused when it loads. A rule
without an `expression` passes on the metric its `name` names. A rule with
an `expression` passes on every metric the expression matches. Where the
expression matches that very name, both pass the metric on:

```yaml
metrics:
  - name: up
    description: Whether the target is up.
  - expression: '.*'
```

Each rule makes every series of `up`, and a scrape that has a series twice
fails — `validation failed: duplicate metric series "up"` — on every scrape
of a target that has the metric; of a target that has none, the first rule
makes nothing, and reports the metric missing unless it has
`required: false`. The first rule, which is about that one metric, makes a
series of no scrape that passes, so the load says:

```text
collector "node" metrics rule 1 and rule 2 both pass on metric "up": rule 1 passes on the metric of that name, the expression ".*" of rule 2 matches that name, and their labels are alike, so each makes every series of the metric, and a scrape that has a series twice fails, as a duplicate metric series; take one of the two out, write the expression so that it does not match "up", or tell their series apart by a label, as with a static label that has another value in each
```

The expression is matched against the name as it is against a metric of the
target: compiled the same, and matched anywhere in the name, so `up`,
`'^u'`, `'(?i)^UP$'` and `'.*'` all match `up`. The pair is refused
whichever of the two rules is written first, and when

- the rule with the expression has no `name`, and so keeps the metric's
  own, or has that same `name`: with another, as in
  `{name: alive, expression: '^up$'}`, it exports the metric under that
  name, and the two rules load;
- the two are alike in `labels`, as
  [two rules that are the same rule](#two-rules-that-are-the-same-rule)
  are: a label in one that the other has not, or has with another `value`
  or `expression`, tells their series apart as far as the configuration
  says, and the rules load.

What else the rule of the name says changes nothing of it, as there: a
`description`, a `scale`, a `type`, `required`, `error_mode` and a label's
`truncate` and `required` do not say which series a rule makes. With a
`type` other than the metric's own, the scrape fails on a metric of two
types before it comes to the duplicate. And where its `scale` or `type` cannot
apply to the metric, or a label it requires is missing, the rule fails on
the series instead of making it — it makes no series of the metric there
either.

What else the rule with the expression says does not keep the pair from
being refused either, with one exception, a `type` of `histogram` or
`summary` (below), though it can change what a scrape makes of it. A
rule that sets a `type`, a `scale` or a `required` label can fail on a
series instead of making it: a histogram or a summary keeps its own type
and takes no scale, a `type` of `histogram` or `summary` applies to no
other metric, and a series may not have the label. Under `error_mode: log`
or `ignore` the rule then carries on without that series, and the scrape
passes with the one the rule of the name made. So what such a pair does is
the target's to say:

| The rule with the expression sets | Each rule makes the series, and the scrape fails | That rule fails on the series, and the scrape passes | The load |
|---|---|---|---|
| `type: gauge`, `counter` or `untyped` | of a gauge, a counter or an untyped metric: on a duplicate where the type is the metric's own, on `inconsistent types` where it is not | of a histogram or a summary | refuses the pair |
| `type: histogram` or `summary` | of a metric of that type | of any other metric | takes the pair, unless the rule of the name sets that `type` too |
| `scale` | of a gauge, a counter or an untyped metric | of a histogram or a summary | refuses the pair |
| a `required` label | where the series has the label | where it has not | refuses the pair |

`{name: request_duration_seconds}` beside `{expression: '.*', scale: 0.001}`
passed a histogram of that name on once, and failed the scrape of a target
where it is a gauge; `{name: up}` beside `{expression: '^up$', type: gauge}`
failed every scrape of an ordinary target. Both are refused, whatever the
target has: a setting that fails on some series is no way to keep a rule
from a metric, since every series it does not fail on is made twice. The
load names what the rule sets — its type, its scale, each label it
requires — in place of saying that every series is made twice:

```text
collector "node" metrics rule 1 and rule 2 both pass on metric "request_duration_seconds": rule 1 passes on the metric of that name, the expression ".*" of rule 2 matches that name, and their labels are alike; the scale of rule 2 does not keep it from the metric, since every series of the metric that rule 2 does not fail on is made by each rule, and a scrape that has a series twice fails, as a duplicate metric series; take one of the two out, write the expression so that it does not match "request_duration_seconds", or tell their series apart by a label, as with a static label that has another value in each
```

Where one of the two rules sets a `type` the other does not, it says too
that the scrape fails `or, where the two rules give the metric different
types, as a metric of inconsistent types`. With `error_mode: fail` the rule
fails the scrape wherever it fails on a series, so no scrape of the pair
passes, and the load says of it what it says of the two rules above. A
`required` label that the rule of the name has not is a label that tells
their series apart, and those rules load.

The exception is a rule of the histograms alone, or of the summaries: a
rule with an expression whose `type` is `histogram` or `summary`, under
`error_mode: log` or `ignore`, is taken beside a name it matches. Here that
is the metric `up`, and every histogram:

```yaml
metrics:
  - name: up
  - expression: '.*'
    type: histogram
    error_mode: ignore
```

A `type` of `histogram` or `summary` applies to a metric of that type and
to no other, so this rule fails on, and carries on without, every gauge,
counter, untyped metric and summary: it is a rule of the histograms alone,
and leaves an ordinary metric to the rule of its name. Of a target with a
gauge `up`, a counter and two histograms, the scrape has `up` once and
each histogram once. With `error_mode: ignore` nothing is logged; with
`log`, the default, the scrape is the same, and the rule's failures on the
metrics that are no histograms are logged as a warning.

The pair still makes a series twice where the target's metric of that name
is itself a histogram — a summary, under `type: summary`. Each rule then
makes it, and the scrape of that target fails with
`validation failed: duplicate metric series "up"`: the type of a metric is
the target's to say, and the load cannot tell. For a name whose metric is a
histogram, write the expression so that it does not match the name, or take
the rule of the name out, the other making that histogram already.

Only the `type` decides, whatever else that rule sets. With a `scale` or a
`required` label besides, the pair loads as well: those keep the rule from
more series, not from fewer, and a rule with `type: histogram` and a
`scale` makes no series at all, since a histogram takes no scale. The pair
is refused as the others are

- where the rule of the name sets that same `type`, as in
  `{name: latency, type: histogram}` beside
  `{expression: '.*', type: histogram}`: that rule then makes the metric
  only where the other makes it too, a histogram twice and anything else
  not at all. With another `type` in the rule of the name — `gauge`, or
  `summary` beside `histogram` — the rules load, and no scrape has a series
  twice: of a metric of the pattern's type the rule of the name fails, and
  the other rule makes the one series;
- under `error_mode: fail`, where the rule fails the scrape on the first
  metric of another type.

Nothing else is refused. Rules of which the configuration does not say that
they pass on one metric, under one name and with the same labels, load as
they did, and fail the scrape that has a series twice with
`duplicate metric series`:

- two rules that both have an expression, such as `'^node_'` and
  `'^node_cpu'`, which both pass on a metric only if the target has one
  that both match;
- a rule of a name beside a rule that exports another metric under that
  name, `{name: up}` and `{name: up, expression: '^node_up$'}`, whose
  series are the same only if the two metrics have the same labels;
- rules whose labels differ as written and come to the same on a target,
  such as a static `job: api` in one rule where the target's series has
  that label already.

Each pair is reported once, naming both rules by their places among the
collector's rules, counted from 1, after the copies of a rule, which are
told of as [the same rule](#two-rules-that-are-the-same-rule) and of
nothing else. A schema cannot ask whether one item's expression matches
what another item names, so this too is a check only the exporter makes
([Editor support](#editor-support)).

Every mistake is reported at once, not one per run: the mistakes of every
collector and every rule, and of every [collector file](#collector-files), in
order, one per line, each quoting the expression it is about:

```text
collector "a" metric "x" expression ".foo[": unexpected EOF
collector "a" metric "bad-name": "bad-name" is not a valid Prometheus metric name; use letters, digits, underscores and colons, not starting with a digit, or set the collector's name_escaping to underscores or values to export it escaped
collector "b" metric "z" regex "value=\\d+" has no capture group; the value is the capture group named value, as in '(?P<value>\d+)', or else the first, so wrap the number in one, such as 'requests=(\d+)'
```

A mistake in a collector that a [collector file](#collector-files) defines
names that file, as in `collector file /etc/exporter/collectors.d/payments.yaml:
collector "pay" metric "depth" expression ".queue.depth[": unexpected EOF`, and
a rejected reload whose mistakes are all in one collector file is logged with
that file as its `file`; with mistakes in several files, `file` is the
configuration and the text names each.

Past 20, the rest are counted (`and 5 more problems`); `--dry-run` lists
every one as an error of its own. A collector whose request, limits or
formats are wrong stops at its first such mistake, since the rest of its
checks read them.

A CSS selector that does not compile used to match nothing, on every scrape,
without saying why; it is now refused when the configuration loads. The
expressions are compiled once, then, and every scrape reuses them.

A file that cannot be read as a configuration is refused in its own terms:
each error names the line and what was expected there, as in

```text
line 3: unknown key "requst" in a collector; line 7: "fast" is not a duration; write one such as 500ms, 30s or 1m30s
```

Every unknown key is refused, a static target's `request` block
included, since a misspelt key would otherwise be ignored without a word.
For the same reason a file holds one YAML document: the configuration, a
collector file and the static targets file are each refused when a second
document follows a `---`, naming the line it starts on. A leading `---`, and a
final `---` or `...` with nothing after it but comments, are fine.

### Editor support

[`configs/config.schema.json`](../configs/config.schema.json) is a JSON Schema of this file.
With the YAML extension for VS Code, or any editor that uses the YAML language
server, start a configuration with

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/eenchev/prometheus-universal-exporter/main/configs/config.schema.json
```

and the editor completes keys, shows what each one does, and flags unknown keys
and values that are not allowed as you type. The example configurations in
`configs/`, `config.example.yaml` and `config.otlp.example.yaml`, start with
it; the ones under `examples/` do not.

The [static target file](STATIC-TARGETS.md) has a schema of its own,
[`configs/static-targets.schema.json`](../configs/static-targets.schema.json), which
`configs/static-targets.example.yaml` points editors at the same way. All three
schemas — this one, the [collector file](#collector-files) one and the static
target file one — are in `configs/` with the examples, and are regenerated with
`make schemas` (see [Development](DEVELOPMENT.md#the-configuration-schema)).

`prometheus-universal-exporter --config.schema` prints the schema of the binary
you are running; its `request.type` values are the request types that binary
was built with. The schema describes the canonical spelling and is not the last
word: startup validation also checks what a schema cannot, such as that an
expression compiles, that `otlp.interval` is at least `1s` — a duration is
text to a schema — that a duration is not too long to be held, that a
[size](#sizes) is under 2^63 bytes, that any other whole number, such as
`limits.max_metrics`, is one an int64 holds, from -2^63 to 2^63 - 1, that a
`limits.max_output_bytes` written
in quotes or with a unit is not under [its least](#sizes), and that no two
rules of a collector are
[the same rule](#two-rules-that-are-the-same-rule), nor a `prometheus`
[name and a pattern that matches it](#a-name-and-a-pattern-that-matches-it),
a schema having no way to compare the items of a list by some of their
keys. Where a
schema can tell, it refuses what the exporter refuses: a
`response.csv.delimiter` of more than one character, a size with a fraction
and no unit, a block that sets keys beside a missing `enabled`, a
`value_map` key that is empty or has blanks around it, a label's
`expression` of nothing but blanks, an entry of `transform.include` or
`transform.exclude` that is empty or nothing but blanks, a `prometheus`
rule's `expression` of nothing but blanks, a `value` on a
label of a `python` rule and such a label without `truncate: true`, a
`python` rule's `type`, `description`, `required` and `error_mode`, a
`required` label
whose `value_map` maps a value to `""`, a rule without a `name` under any
transform but `prometheus`, a rule's or a label's `name` that is not a
classic name in a collector whose `name_escaping` is not `underscores` or
`values`, a `grpc` collector that calls
another service than `grpc.health.v1.Health` without `descriptors`, and a
negative duration, such as `timeout: -5s`, which no key takes. A duration is
written as Go writes one — `500ms`, `1h30m`, `1.5s` — to the schemas as to
the exporter: a `+` may lead it, and a `-` only a zero (`-0s`); only in an
`otlp` block with `enabled: false`, which is kept unchecked, do both take a
negative one. Unchecked is of the whole block, to both: switched off, it may
hold a `compression` that is neither `gzip` nor `none`, a negative
`max_pending_points`, `unready_after_failures` or `batch_max_size` and a
`batch_max_bytes` under its least, as it may an endpoint that
is no URL, and only what is not of a key's type at all — text for a number, a
list for text — is refused there. What is written negative is negative
however small: `-0.4ns`,
which rounds to zero, is refused by both as `-1ns` is. The longest duration
is 2^63 - 1 nanoseconds, some 292 years (`2562047h47m16.854775807s`): a
longer one, such as `2562048h`, is well written, so the schemas take it, and
the exporter refuses it as no duration. A duration of zero is written `0s`
or `0`: an unquoted `0` is a number to YAML, which the exporter reads as the
duration it spells, with a sign too (`+0`, `-0`), and the schemas take the
number 0 beside the text. No other number is a duration, to either:
`timeout: 30` lacks its unit and is refused, not read as 30 of anything. A
schema is handed the number and not how it was written, so a zero written
another way — `0.0`, `00`, `0x0` — passes the schemas as the 0 it is, and
the exporter, which reads what is written, refuses it as no duration. These
two, a duration too long to be held and a zero that is a number but is not
written `0`, are all the schemas and the exporter differ on in how a
duration is written.

A whole number, such as `limits.max_metrics: 1e3` or `retry.attempts: 2.0`,
may be written with a point or an exponent: a schema is handed the number,
and the exporter reads it by its digits, so `9007199254740993.0` is that
number and not the one a floating-point number rounds it to. One past what an
int64 holds, such as `1e19`, which a schema takes, the exporter refuses,
naming the key, the number and the range, and so is one past what a
floating-point number holds, such as `1e400`, which the YAML reader the
exporter uses takes for text, and an editor, reading YAML 1.2, for a number; and a fraction the exporter
refuses however small, though a schema, handed the floating-point number,
takes `1.00000000000000000001` for 1. An entry of `request.accept_status` is
a status written as a number or as text: a number is the status it is
however it is written — `503.0`, `5.03e2`, `0x1F7`, `0o767`, `+503` and
`0503` are 503 — to both; text is text, the digits of a status or a class,
so `"503.0"` is refused by both. In quotes, digits with a sign or leading
zeros, `"0503"` or `"+503"`, are the status they read as to the exporter, and
text that is no status to an editor, which flags them. The few more spellings the exporter's
reader takes for a number, `5_03` and `0b111110111` among them, are 503 to
the exporter, and text that is no status to an editor, which reads YAML 1.2.

An optional key written as the empty string is the key left out, to the
exporter and to the schemas alike. Where the key takes one of a set of
values or text of a pattern, `""` is its default: `decoder.type: ""` is
`auto`, `name_escaping: ""` is `fail`, `request.method: ""` is `GET`, an
error policy or a rule's `error_mode` written `""` is the one it defaults
to, a rule's `type: ""` is `gauge`, or under a `prometheus` transform the
type of the series it passes through, `otlp.compression: ""` is `gzip`, and
`metrics_prefix: ""`, `response.graphite.value: ""`,
`response.graphite.invalid_lines: ""` and a static target's `name: ""` and
`request.method: ""` are those keys left out. A key that belongs to another
request type, such as `rpc`, `descriptors` or `method`, is left out when it
is written `""`, and so takes no part in what that type refuses. A rule
about a key goes by the key being written: a label has a `value` or an
`expression`, and the one written `""` is the one it does not have, and a
`prometheus` rule has a `name` or an `expression`, neither of which it has
written `""`. Blanks
are not the empty string: a key written as nothing but blanks, such as
`" "`, is text like any other, which the key takes or refuses as it does its
other values — a label's `value` of blanks is a constant of blanks, and its
`expression` of blanks is refused, as an entry of blanks in
`transform.include` or `transform.exclude` and a `prometheus` rule's
`expression` of blanks are. A key
that is required is as missing written `""` as left out, and refused by
both: `transform.type`, `request.type`, a collector's `name`, a label's
`name`, a rule's `name` under the transforms that need one, a `grpc`
collector's `rpc`, a `localfile` collector's `root` and a static target's
`collector`. A key written with no value at all — nothing after its colon,
`null` or `~` — is another thing, and no key takes it: to a schema it is
none of the types the key takes, and the exporter refuses it at every key,
every entry of a list and every value of a mapping, naming the line, so
that an editor and the exporter flag the same key. And a key takes the
kinds of value its schema says, to the exporter too: free text takes a
number and a boolean as the text they spell (`value: 1`, `description:
404`); a key the schemas hold to allowed values or a pattern — a name, a
word of a few, a host — takes text alone, and the exporter refuses a number
or a boolean there, saying to quote it, where it read `name: true` as the
name `true`; and a boolean is `true` or `false`, the exporter refusing the
`yes` and `on` it read as one. A rule's and a label's `name` are text under
every `name_escaping`: one that is a number, as `underscores` and `values`
allow, is written in quotes, `name: "404"`. An editor reads the file as
YAML 1.2, and the exporter's reader takes a few more spellings for a
number, `1_000` and `0b101` among them: at a key of text alone the exporter
refuses those as numbers too, which is all the two differ on there. What is
not text has no empty form: `""` is no duration and no size, and both refuse it; and both refuse an empty entry of
`collector_files`, `request.accept_status`, `request.allowed_targets`,
`request.denied_targets`, `request.redirect_trusted_hosts`,
`transform.include`, `transform.exclude` and the Python
libraries, and an empty key of a `value_map`, of `request.metadata` and of a
static target's `params` and `labels`.

On a static target there is no HTTP response to carry an error. `fail` there
means the scrape serves nothing except `http_exporter_target_up` at 0, and
`log` and `ignore` serve what could be extracted.

Every transform may define `transform.pre_script`. It runs once per scrape
after decoding and before metric extraction. The script receives the decoded
value as `data` and may mutate it or replace it by assigning to `data`.
HTML/XML pre-scripts receive raw document text, which is parsed again after the
script, so they must leave a string: a dict or a list fails the scrape, naming
what the script left. A `prometheus` transform's pre-script receives the series as
`{"metrics": [...]}` and must leave them in that shape, read back into series
for the rules; see [Python](PYTHON.md#reshaping-a-response-instead-of-writing-a-python-transform). Python transforms emit metrics with the `metric(...)` API.

A pre-script **must** leave its result in `data` — that is the variable the
exporter reads back. A script that computes a value under another name throws it
away: the transform then runs against the untouched response, so the collector
looks like it is extracting badly rather than configured wrongly. The exporter
therefore refuses to start when a pre-script never produces `data`, naming the
collector, and rejects such a configuration on reload with the previous one left
active. Replacing `data`, mutating it by key or attribute, augmenting it,
binding it as a loop or `with` target or with `:=` (`(data := ...)`), deleting
from it (`del data["noise"]`), and calling a method that mutates it all count.

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
configuration with no Python scripts never invokes one. The interpreter is
started once for a check — at startup, for `--dry-run` and at each reload —
whatever it finds and however many [collector files](#collector-files) the
configuration reads.

## Reusing settings with YAML anchors

Collectors of one API often share their request, transform and rule
settings. YAML anchors write them once: `&name` marks a value, `*name` repeats
it, and `<<: *name` merges a mapping into another. A top-level key that begins
with `x-` is the file's own — the exporter ignores it, whatever it holds — so
it is the place for the shared parts:

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

An anchor can also sit where it is first used, as the Open-Meteo example's
`labels: &location` does on its first metric; an `x-` key is for what no
collector uses as it is. A key written beside `<<:` replaces that key of the
merged mapping whole — `query` above, or a `request` set beside a merged
collector — rather than being merged into it. Anchors work in collector
files and the [static target file](STATIC-TARGETS.md#the-target-file) too,
each file on its own: an anchor in one file cannot be used in another.
Aliases and merge keys work in every mapping of the three files, a
collector's `cache` and a static target's `request` included, and what a
merge brings in is read as if it were written out: a key the block does not
take is refused, and a `path` or `body` merged into a target's `request`
replaces the collector's. A key may itself be an alias of an anchored name:
with `x-names: [&ttl ttl]`, `cache: {*ttl : 1m}` sets `ttl`, as YAML reads
it, in those blocks as anywhere, and an unknown key so written is refused by
the name its anchor holds. An alias of `<<` is the text `<<`, not a merge,
and so an unknown key.

An `x-` key is only ignored at the top level; anywhere else it is an unknown
key like any other, and so is a bare `x-`. With
[environment expansion](#environment-variables) on, a `${NAME}` in an `x-`
block nothing uses is left alone, so its variable need not be set; one in a
block an alias uses is expanded where the block is. The published
[schemas](#editor-support) accept top-level `x-` keys too.

## Collector files

A long list of collectors is easier to own in several files — one per team, per
application, or per ConfigMap key. List them under `collector_files`:

```yaml
collector_files:
  - shared.yaml             # one file
  - collectors.d/*.yaml     # every file the pattern matches
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
  directory, such as a static target file, would be read and refused, so a
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
  A mistake found in a collector names the collector file that defines it,
  in the error and, when it rejects a reload, as the log line's `file`; so
  does a fault the interpreter finds in the collector's Python, as in
  `collector file /etc/exporter/collectors.d/payments.yaml: collector pay
  pre_script has a Python syntax error on line 3: invalid syntax`.
  A configuration needs at least one collector across all of them; with
  `collector_files`, its own `collectors` key may be left out.
- **Unique names.** A collector name must be unique across the configuration
  and every collector file; see [Collectors](#collectors).
- **Environment variables.** With `--config.expand-env`, `${NAME}` references
  are expanded in collector files as in the configuration.
- **Reloading.** With `--config.watch`, editing, adding or removing a collector
  file reloads the configuration, even though the configuration file itself did
  not change. The files watched are those the configuration file lists, even
  while it is refused: a configuration that adds a collector file with a
  mistake in it reloads once that file is fixed, without touching the
  configuration again. Starting the watch reloads nothing. A reload that would break any rule above is rejected and the last
  valid configuration stays active.
- **Checking.** `--dry-run` reads the collector files too and lists them under
  `details.collector_files` of its `config` entry.

`prometheus-universal-exporter --config.collector-file-schema` prints the JSON
Schema of a collector file, published as
[`configs/collector-file.schema.json`](../configs/collector-file.schema.json). Start a collector
file with

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/eenchev/prometheus-universal-exporter/main/configs/collector-file.schema.json
```

and the editor checks it the way it checks the configuration, including that it
has no key but `collectors`.

## Environment variables

A configuration file is usually committed, and some of what belongs in it is
not: an internal hostname, a tenant identifier, a token. Pass
`--config.expand-env` and the exporter substitutes `${NAME}` references from its
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
  prometheus-universal-exporter --config.file=config.yaml --config.expand-env
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
config.yaml: environment variable "API_TOKEN" not set; --config.expand-env
requires every ${NAME} it finds to be defined
```

An empty substitution would produce a document that parses and is wrong — a
collector with no path, credentials that are silently blank — and the exporter
would serve it. Every missing name is listed at once. A variable that is set to
an empty string is a deliberate choice and substitutes normally: as the whole
of a value, `X-A: ${A}`, it is the value written `""`, which loads or is
refused just as `X-A: ""` would be, and not a key with nothing after its
colon.

A reference is expanded where the document holds it as a value — a value
or a key, quoted or not, whole or part of a longer one — and the variable is
always exactly that value. A token holding ` #`, one starting with `*`, `&` or
`[`, quotes, backslashes and line breaks all arrive as written: the value is
written back quoted where YAML would otherwise read it differently, and left
unquoted where it reads the same, so a number stays a number. So does a
boolean: at a key that takes text alone, such as a `name`, write the
reference in quotes, `name: "${NAME}"`, or a variable set to `true` or `1`
is [refused](#checked-when-the-configuration-loads) as the boolean or the
number it then is. A reference in
a comment is left alone, set or not. That holds in a flow collection too,
where a bare reference is not YAML until it is expanded — alone, as in
`regions: [${PRIMARY}, ${SECONDARY}]` or `{token: ${TOKEN}}`, as part of a
longer value, as in `{Authorization: Bearer ${TOKEN}}`, and beside quoted
values, as in `{X-Name: "x", X-Token: ${TOKEN}}` — and reading such a file
changes no other value in it: a reference in a block value or after a comma
in an unquoted description expands to the variable's value, exactly as in a
file without one. It holds after an anchor,
as in `&base ${BASE}`, and for values YAML reads specially on their own, such as
`-` or an empty string: a key is always written quoted, and so are an empty
value and `-`. A variable supplies a value and never
structure: `headers: ${ALL_HEADERS}` is one string, not a mapping. A block
value (`|` or `>`) is expanded in every line, also when its header says how
far it is indented, as in `|2`. Two places
cannot take every value: a block value takes one without a line
break, and an unquoted value folded over several lines is refused; quote the
reference there, as in `"${NAME}"`. A literal `$${NAME}` in a flow collection
must be quoted, since its braces are not YAML there. Errors name the file's
own line numbers.

`--config.watch` re-expands on every reload, so a reload cannot quietly
replace a working configuration with literal references.

The static target document has its own flag, `--static-targets.expand-env`,
which works the same way; `--config.expand-env` does not reach it, so either
file can take values from the environment while the other is used as written.
See [Static targets](STATIC-TARGETS.md#environment-variables).

Environment references are fixed when the file is read. For a value that
changes per scrape — a tenant in the URL path — use a
[path parameter](REQUESTS.md#path-parameters), `{{param_tenant}}`, which the probe
fills in. The two syntaxes never overlap, and a path parameter's default can
itself be an environment reference.

## OpenMetrics

`/probe`, `/self-metrics` and the static targets endpoint answer in
[OpenMetrics](https://github.com/prometheus/OpenMetrics/blob/main/specification/OpenMetrics.md)
1.0.0 when the scraper's `Accept` header prefers it, and in the Prometheus text
format, version 0.0.4, otherwise. Prometheus 2 and 3 ask for OpenMetrics first,
so they get it; `curl`, a browser and anything that does not ask get the text
format, as before. Nothing needs configuring. The answer's `Content-Type` says
which it is, and it carries `Vary: Accept`.

In both formats the series of a metric are written together, under one `HELP`
and `TYPE`: the metrics in the order each first appears, and a metric's series
in the order they were made — a `csv` rule's in the order of the rows.
Nothing is sorted. That holds whatever order the series come in: two rules of
one name with another rule between them, a script that emits a series of each
metric row after row, and a `prometheus` collector whose rules or `rename`
give two of a target's metrics one name are all answered with each metric's
series as one group, as the text format requires.

The two formats hold the same series, written the way each requires, with one
difference in names: an OpenMetrics counter's samples always end in `_total`.
A counter named `jobs_done_total` is the series `jobs_done_total` either way,
but one named `jobs_done` becomes `jobs_done_total` in OpenMetrics. Name
counters with `_total`, as Prometheus' naming conventions ask, and the names
are the same whichever format a scraper takes; the exporter's own counters all
are. In OpenMetrics, `untyped` is written as `unknown`, timestamps are in
seconds, `le` and `quantile` values are written as floats (`1.0`), and the
answer ends with `# EOF`; Prometheus reads either the same.

OpenMetrics also forbids two families from claiming one name, which the text
format allows: a counter `foo_total` is the OpenMetrics family `foo`, so beside
a gauge `foo` the name would be used twice and Prometheus would fail the
scrape. The same goes for counters `jobs` and `jobs_total`, or a histogram `h`
beside a gauge `h_count` (the histogram's own `h_count` sample). Families that
would clash like this are written as OpenMetrics `unknown` families under the
very names their samples have in the text format, whichever comes first: the
counter `foo_total` stays the series `foo_total`, only without its counter
type in OpenMetrics, and the gauge `foo` stays a gauge. A counter gives way
before a histogram or summary does; a histogram or summary that still clashes
is written as `unknown` families `h_bucket`, `h_sum` and `h_count` (a summary's
as `s`, `s_sum` and `s_count`). Every series is the same in both formats.
Distinct names avoid all this, and keep the types.

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
is written as any counter is, and as Prometheus writes one: its family is the
escaped name without a `_total` it ends in, and its sample the family with
`_total`, the plain text in both places. `my.errors`, exported as
`U__my_2e_errors`, is the family `U__my_2e_errors` with the sample
`U__my_2e_errors_total`. The escaping doubles every underscore, so
`my.requests_total` is exported as `U__my_2e_requests__total`, which is the
family `U__my_2e_requests_` with that same sample: the family keeps one of
the two underscores. A sample is always its family and `_total`, which is
what lets a reader of OpenMetrics — Prometheus, and this exporter's own
`prometheus` decoder — find the counter's sample in its family.

The text format also lets a target write values that OpenMetrics does not
allow a family of its type, and a strict OpenMetrics parser refuses a whole
answer over one of them. The exporter passes such values on in both formats —
a negative counter a target wrote is not refused, and not left out — and keeps
the OpenMetrics answer valid in the same way: the family is written as
`unknown` families under its sample names, with the same series and values as
in the text format, and loses only a type it does not meet. A family has one
type, so the whole family is written so when one of its series is:

- a counter that is `NaN` or negative. `+Inf` is a counter's value;
- a histogram whose `_sum` is `NaN` or negative; one with a `_sum` and a
  bucket with a negative bound, which OpenMetrics allows only without a sum;
  one with a bound that is not a number, `le="NaN"`; one whose bucket
  counts fall from one bound to the next higher, or whose highest bucket
  holds more than its `+Inf` bucket; one whose `+Inf` bucket and `_count`
  differ, as a target scraped between two of its updates writes them; or one
  with neither of the two (see [Prometheus input](#character-encodings));
- a summary whose `_sum` is `NaN` or negative; one with a `quantile` outside
  0 to 1, such as percentiles written as `50` and `99`; or one with a negative
  value for a quantile. `NaN`, which a quantile is while nothing was observed,
  is allowed.

A gauge and an untyped series may have any value. So a target's counter
`jobs_total -5` is `# TYPE jobs_total unknown` and `jobs_total -5` in
OpenMetrics, and a histogram `h` whose counts fall is the `unknown` families
`h_bucket`, `h_sum` and `h_count`; a family whose values its type allows
always keeps the type. [OTLP export](OTLP.md#delivery) follows the same rule,
with gauges.

Order is not among these: a histogram's buckets are written in ascending order
of `le`, with `+Inf` last, and a summary's quantiles in ascending order, in
both formats, in whatever order the target wrote them.

OpenMetrics can give a counter, a histogram and a summary a `_created` time,
when it started counting. The exporter writes none for what it reads from
targets, on `/probe` and the static targets endpoint: it cannot know when a
target's counter started, and OpenMetrics leaves `_created` out when it is not
known. It does know that of its own counters, and writes it on the
self-metrics endpoint when `web.self_metrics.created_timestamps` is set; see
[Created timestamps](SELF-METRICS.md#created-timestamps).

To keep a Prometheus job on the text format, set its `scrape_protocols`:

```yaml
scrape_configs:
  - job_name: universal-exporter
    scrape_protocols: [PrometheusText0.0.4]
```

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
every `/probe` parameter the collector's request type accepts (`method`,
`path`, `timeout`, `body`, `insecure_skip_verify`, `follow_redirects`,
`enable_http2`, `retry_attempts`, `retry_backoff`, `message`, `from`, `until`
and every `param_<name>`, whether it fills the request or only a [label
value](REQUESTS.md#in-label-values)), and every header forwarded to the target, including
a forwarded `Authorization` value and the `header_<name>` parameters that are
forwarded. A parameter that changes nothing sent is not part of it: one no
request type knows, which a probe ignores, and a `header_<name>` for a header
the collector does not forward. Probes differing only in those share an entry,
so a caller adding `&x=1`, `&x=2`, ... cannot fill the cache with copies of one
result. The parameters are keyed as the probe reads them, not as they were
written: `method=get` and `method=GET`, `timeout=5s` and `timeout=5000ms`,
`insecure_skip_verify=TRUE` and `true` are one request and one entry, and a
parameter [given twice](#request-types) is refused rather than keyed. Presence
and absence differ: a probe that sends
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

Two failures are never answered stale. One is the target refusing the
credential it was sent, HTTP `401` or `403`, or gRPC `UNAUTHENTICATED` or
`PERMISSION_DENIED`. A credential forwarded with `forward_authorization` is
part of the cache key, but a token that has just been revoked would otherwise
still be answered with what it was given before, for as long as
`stale_if_error` lasts. The other is the collector's own
[`allowed_targets` or `denied_targets`](REQUESTS.md#restricting-targets)
refusing the target, answered `403`: its name now resolves, or a redirect now
leads, somewhere the collector may not go, and the refusal is the answer.
Such a probe fails as usual, and a static target's scrape publishes no stale
result. A fresh result within `ttl` is still
answered from the cache without asking the target, as any cached result is.

A stale answer must never pass for a fresh one, so while `stale_if_error` is
set every answer of the collector carries two series of the exporter's own:

```text
http_exporter_result_stale 0
http_exporter_result_age_seconds 0
```

`http_exporter_result_stale` is `1` when the answer is the last good result
standing in for a failed trip. `http_exporter_result_age_seconds` is how long
ago the answered result was fetched from the target — `0` for a trip just
made, the entry's age for a cached answer. On the
[static targets endpoint](STATIC-TARGETS.md), which serves each target's last
result until its next scrape, it is worked out at every read, so it says how
old the data is when Prometheus reads it: a target scraped every 10 minutes
reads from `0` up to about `600` before its next scrape. Watch them rather than `up`, which
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
under the old one. A [static target](STATIC-TARGETS.md) whose scrape
fails serves the last good result the same way, marked stale, while its
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
share one, and probes differing only in parameters that change nothing sent
still do. A `param_<name>` that fills a [label
value](REQUESTS.md#in-label-values) is a parameter of the probe like the
others: two probes that differ in it get different series, and never share.
It needs no cache: the cache helps the probes that come after one
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
`503 Service Unavailable` — `collector inventory_api already has 8 trips to
its targets in progress, its max_concurrent_probes; this probe was not sent` —
and counted in `http_exporter_probes_rejected_total`, while
`http_exporter_probes_in_flight` shows how close to the limit a collector runs.
Prometheus records the rejected scrape as `up` 0 with that reason, rather than
as a timeout.

Probes that make no trip take no slot: one answered from the
[response cache](#response-caching), and one that
[shares a request](#identical-probes-share-one-request) already in flight. A
[static target](STATIC-TARGETS.md) shares its collector's limit, but
waits for a free slot within its scrape budget instead of failing at once,
since nothing is waiting on its answer; if none frees up in time, the scrape
fails in the `concurrency` stage. The default, 32, is well above what one
Prometheus usually sends a single backend at once; lower it for a backend that
cannot take many requests at a time, raise it for a collector with many slow
targets. A negative value is a configuration error.

Each collector's limit leaves the process as a whole unbounded: twenty
collectors of 32 could hold 640 responses, and their series, in memory at
once. `--probe.max-concurrent` bounds the trips of every collector together,
on top of each collector's own limit:

```sh
prometheus-universal-exporter --probe.max-concurrent=64
```

A probe over it is answered `503` in the same way, naming the flag — `the
exporter already has 64 trips to targets in progress, its
--probe.max-concurrent; this probe was not sent` — and counted in
`http_exporter_probes_rejected_exporter_limit_total`, apart from those its
collector's own limit turned away; a static target scrape waits for a slot.
`http_exporter_trips_in_flight` shows how close to the limit the exporter
runs.

Static target scrapes waiting for a slot wait in line: a slot that frees goes
to the first of them it can serve — its collector under its own limit and the
exporter under `--probe.max-concurrent` — so one waiting behind its own full
collector never holds up another collector's. `0`, the default, leaves only the
per-collector limits; a negative value is a command-line error. Size it to the
memory the exporter has, together with `--python.max-workers` (see
[Python](PYTHON.md#how-scripts-run)).

## Memory

The exporter's memory is the Go heap — responses, decoded documents and
series while trips are in progress, and the response cache — and the Python
workers, each a process of its own. `--probe.max-concurrent` bounds the
first, `--python.max-workers` and
[`limits.max_script_memory`](PYTHON.md#how-scripts-run) the second.

`limits.max_metrics` (10000 by default) bounds a scrape's series as they are
made, not after: every transform stops at the first series past it, and the
scrape fails with `metric count 10001 exceeds limit 10000` — one past the
limit, since counting further would cost the memory the limit is there to
save. A response of a million lines read with the regex `(\d+)` is refused
after ten thousand and one series, not after a million; a jq rule makes a
series, and its labels, as its expression gives each value, with `items` and
without. The prometheus
decoder keeps only the series its transform passes on — those its rules match,
or `include` and `exclude` pick — and stops the same way, so a large exposition
of which a collector keeps a few costs what the few cost. With a `pre_script`,
which is given every series, the decoder keeps them all and the transform
counts. A python transform's answer is bounded by `limits.max_output_bytes`,
and its metrics are counted before they are read. The failure is the probe's
`validation` stage, counted in `http_exporter_series_limit_exceeded_total`,
whatever `error_handling` says.

What counts is the series the scrape keeps. An item a rule carries on
without, under `error_mode: log` or `ignore` — a value that is no number, a
required label that is missing — makes no series and takes no room, and a
rule that fails as a whole after it made series, a jq program that fails part
of the way or an XPath expression the engine fails on, gives their room back
with them: the rules after it have all the limit leaves. A prometheus rule
that carries on without a series it cannot make — its `type` or its `scale`
on a histogram or a summary, a required label the series lacks — is counted
the same way, by the series it makes: the decoder stops only for the series
a rule is sure to pass on, keeps the others that a rule matches, and the
transform counts those it makes of them. So of twenty series a rule matches
and keeps five, the scrape passes under `max_metrics: 5`. One thing counts
that is not kept: a rule that makes more series than the limit has room for
ends the scrape there, also when it would have failed as a whole further on
and given them all up, since finding that out would mean making every series
the response describes.

In a container with a memory limit, `--runtime.memory-limit-ratio` sets the Go
memory limit to a share of it, read at startup from the container's own
cgroup, as `/proc/self/cgroup` names it — under cgroup v2 or v1, with or
without a cgroup namespace, and the smallest limit on the way up to the root,
so a pod-level limit counts too:

```sh
prometheus-universal-exporter --runtime.memory-limit-ratio=0.8
```

The Go runtime then collects harder as its heap nears 80% of the container's
limit, instead of growing past it into an OOM kill, and the rest is left to
the Python workers and the runtime's own overhead. The startup log says what
was set. Without a container limit the Go default stays, and `GOMEMLIMIT` in
the environment wins over the ratio. `0`, the default, leaves it off; the Helm
chart sets `0.8`. Any other ratio is at least `0.1`, and one below it is
refused at startup and by `--dry-run`: it would leave the Go heap almost
nothing, so the runtime would spend its time collecting garbage. Leaving
room for the Python workers takes `0.5` to `0.95`.

`GOGC` in the environment is honoured as by any Go program: it is the
garbage collector's target, how far the heap may grow over the live data
before the next collection, 100 by default. Garbage collection is 13% to 22%
of the exporter's CPU, and with `GOGC=400` a probe of 5,000 series took 15%
to 30% less time when measured, for a heap that may grow to about five times
the live data between collections instead of two; with a memory limit set as
above, Go collects sooner as the heap nears it, so the higher target cannot
push the process past the limit. The Helm chart sets it from `goGC.percent`
(see [the chart README](../charts/prometheus-universal-exporter/README.md#garbage-collector)).

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

- A scrape timeout above an hour counts as an hour: whoever reaches `/probe`
  sends the header, and a timeout of years would hold a slot of
  `max_concurrent_probes` for as long as a target hangs.
- A header that is not a positive, finite number — `0`, `-1`, `NaN`, `Inf`,
  text — is no scrape timeout, and the probe is bounded as one without the
  header is. A positive one is a budget however small: `1e-10` ends the probe
  at once.
- The budget bounds the whole trip: the request or file read, decoding,
  transforms and Python scripts. The `timeout` probe parameter still bounds the
  request alone; whichever ends first stops the probe.
- A budget that runs out while a metric rule is being evaluated — a jq
  expression that takes too long, a response of more nodes or rows than there
  is time to read — fails the probe in the `transform` stage, naming the rule
  it was at (`the transform was stopped at metric "queue_depth"`). It is not
  that rule's failure: its [`error_mode`](#when-a-metric-cannot-be-extracted)
  does not apply, `ignore` and `log` included, nothing is counted against it,
  and the series the other rules made are not answered or cached as if they
  were the whole response.
- An offset of half the scrape timeout or more would leave too little, so a
  probe always keeps at least half.
- Without the header — a probe from `curl`, a script, or anything other than
  Prometheus — the probe gets `--probe.default-timeout`, 30s by default, so a
  target that accepts the connection and never answers cannot hold it, and
  its collector's `max_concurrent_probes` slot, for ever. Its error names
  that flag instead. A `timeout` parameter bounds the request within that
  budget and cannot lift it: `timeout=1h` still ends after 30s, and the error says the parameter was capped. `0` leaves
  such a probe unbounded; a negative value is a command-line error.
- A probe answered from the [response cache](#response-caching) needs no budget.
  Identical probes that [share one request](#identical-probes-share-one-request)
  share the budget of the probe that started it.
- The offset covers writing the answer and the network between the exporter
  and Prometheus. Raise it if Prometheus still times out first; `0` uses the
  whole scrape timeout. A negative value is a command-line error.

Once the exporter starts writing an answer, the client has 30 seconds to read
it, on every endpoint. An answer can be megabytes — a passed-through
exposition, the static targets, the verbose self-metrics, a
[debug report](#debugging-a-probe) — and a client that asked for one and
stopped reading would otherwise hold the request, and the answer in memory,
for as long as it kept the connection open. After 30 seconds its connection is
closed, which the log notes at `debug` level only. The time a probe takes to
make its answer does not count: that is the probe's budget above.

Static targets are unaffected: their scrapes are bounded by their own
`interval` (see [Static targets](STATIC-TARGETS.md#scraping)).

## Watching the configuration

The exporter reads its configuration once at startup, and again when asked —
see [Reloading on demand](#reloading-on-demand). Pass `--config.watch` to
have it re-read the configuration file and, when one is configured, the
static target file whenever either changes on disk:

```sh
prometheus-universal-exporter \
  --config.file=config.yaml \
  --config.watch \
  --config.watch-interval=60s
```

`--config.watch-interval` defaults to 60s and must be positive; passing zero or a
negative duration alongside `--config.watch` is a command-line error (exit 2),
at startup and with [`--dry-run`](#dry-run) alike, rather than a silently
disabled watch. Without `--config.watch` no polling loop runs at all,
and configuration changes take effect on restart.

Changes are detected by modification time and size rather than filesystem
events: any other time than the one last read is a change, an older one
included, as `cp -p`, `rsync -t` or a `mv` of a prepared file leave it. The
files are stamped before they are read, so an edit made while they are is
read by the next tick. That is
deliberate: Kubernetes republishes a mounted ConfigMap by atomically swapping
the `..data` symlink, which replaces the inode a file-level event watch is
attached to, so such a watch would stop firing after the first change.

The watch follows the [collector files](#collector-files) too: a collector file
edited, a new file matching a pattern, or a file removed triggers a reload.

### Descriptor files

The watch also follows the descriptor files of the grpc collectors in force:
a collector's `request.protoset_file` and `request.proto_files`, and the
files those `.proto` files import, through however many files. The
configuration is checked against them when it loads — the collector's `rpc`
must be a method they define, and its `message`, like a static target's
`request.message`, must fit that method's request type — and a grpc collector
reads them again by itself at its next call once they change (see
[Descriptors](GRPC.md#descriptors)). Nothing would check the configuration
against the new files, though: a descriptor set replaced by one without the
method would be found by the probes that then fail, with the last reload
still reported as successful. So when one of these files changes, appears or
disappears, the next tick reloads, as it does for the configuration file: the
configuration is read and checked again, against the files as they are now.
No flag turns this on and none turns it off; it is part of `--config.watch`.

When everything still fits, the reload is logged like any other, as
`configuration reloaded` with `"trigger":"watch"`, and counted as a
successful reload. The collectors are defined as they were, so nothing is
dropped or started again: their counters, their cached results, what the
[failure log](LOGGING.md#repeated-failures) remembers of them and the
cadence of their static targets are kept. A result cached before the change
was made with the old descriptors, and is served until its
[`cache.ttl`](#response-caching) runs out.

When something no longer fits — the service no longer has the method, the
request type no longer has a field the `message` sets, a `.proto` file does
not compile, an import is gone — the reload is rejected at that tick, with
the reason:

```json
{"level":"ERROR","msg":"configuration reload rejected","trigger":"watch","file":"/etc/exporter/config.yaml","error":"collector \"queue_stats\": the service acme.queue.v1.QueueService in protoset_file /etc/exporter/protos/queue.pb has no method GetStats; it has GetQueue, Watch","retried_when":"the configuration or a file it names changes"}
```

`http_exporter_config_last_reload_successful{file="config"}` then reads `0`
until a reload succeeds, so a descriptor that broke a collector can be
alerted on from the tick that found it — see
[Configuration reloads](SELF-METRICS.md#configuration-reloads). The
configuration in force stays in force, but not the old descriptors: the
collector reads the changed files at its next call whatever became of the
reload, so the calls that no longer fit them fail, for the reason the reload
was rejected for, until the files are mended. The rejected reload is logged
once, and not tried again until a file changes again; the tick after the
files are mended reloads, and is logged as a reload.

The [static target file](STATIC-TARGETS.md#reloading) is read and checked
again with the configuration when a target in force sets a
`request.message` for a collector that reads descriptor files: the message
is checked against the same files. Which descriptor file changed is not
asked, so such a target file is read also for a file of a collector none of
its targets uses, and is then found as it was. A message the new files refuse
is reported for the target file, on a line of its own and in its own series:

```json
{"level":"ERROR","msg":"static target reload rejected","trigger":"watch","file":"/etc/exporter/targets.yaml","error":"target \"orders\": request.message does not fit acme.queue.v1.GetStatsRequest: (line 1:30): unknown field \"include_shards\"","retried_when":"the static target file, a file its check opens or the configuration changes"}
```

The configuration is then rejected with it, naming the same target, since
the two take effect only as a pair that agrees; both stay as they were, and
both are read again when the file changes once more. A target file without
such a target is not read for a descriptor file, and neither is one whose
own last reload was rejected: that one is read again for what its line names
as `retried_when`. Where that line names the configuration — the target
file names a collector the configuration does not have, say — it means the
configuration file, its collector files and, while the configuration is
rejected too, the files that one is tried again for. A descriptor file of
the configuration in force changing reloads the configuration, which is the
one the target file was rejected by, and leaves the target file as it is;
the descriptor files the target file's own check opened are named apart, as
`a file its check opens`, and do read it again.

Which files are watched follows the configuration in force: a reload that
removes a grpc collector, or points it at other files, changes them from
that reload on. An imported file is watched where the `proto_import_paths`
resolved it, and so is the place in an earlier import path where it was
looked for and not found, since a file appearing there is the one compiled
from then on; a file of the same name in a later import path is never read
and not looked at, and neither are the places of the well-known files
(`google/protobuf/*.proto`), which are built in. A file has changed when its
modification time, its size or its permissions have, when it appeared or
disappeared, or when its path leads to another file: through a symbolic
link, which is how Kubernetes swaps in a new version of a mounted ConfigMap,
or because another file was renamed over it, which the file's device and
inode tell even when the new file has the time, size and permissions of the
old. The reload checks the configuration against the descriptors as the
collectors read them, and a collector's call looks at its files as the watch
does: a link pointed at a file with the very time and size of the old one,
or a file renamed over by one of the same time, size and permissions,
reloads, and the next call compiles the new file; a file whose permissions
alone changed reloads too, and the next call reads it again. One made
unreadable by a `chmod` then fails the reload and that call with
`permission denied` — an unreadable file in an earlier import path too,
rather than the next import path's file of that name being read, as it is
when the file is missing there. The exporter run as root reads a file
whatever its permissions, so there a `chmod` only makes the next call
compile the files once more, and it succeeds.

The device and inode are what the filesystem reports. On one that gives an
unchanged file another inode number — a FUSE filesystem mounted without
`use_ino`, once the kernel has dropped the file from its cache, or the
files mounted again — the file counts as changed: the next tick reloads,
and the next call compiles the files again, once each time the number
changes. Nothing reads the files to tell otherwise, since that would cost
every call a read; on a filesystem that changed the number at every look,
every tick would reload and every call compile.

The cost is one look at each of these files per tick: a tick that finds them
as they were reads no file, compiles nothing and logs nothing. However many
of them changed since the last tick, there is one reload. Starting the
exporter reloads nothing: the files are stamped as the configuration is
first read, before it is checked against them, so one edited while the
exporter was starting is seen by the first tick. A reload stamps them the
same way, before it validates what it read, however `request.type` and
`descriptors` are written (`GRPC` is `grpc`): the `.proto` files are compiled
then, to learn what they import, and that compile is the one the validation
uses, so a startup and a reload compile no file twice unless it does not
compile. Without `--config.watch` none of this is done: no descriptor file
is stamped, and a startup or a reload reads them only to check the
configuration.

### Rejected reloads

A reload that is rejected is not tried again until something changes, and is
logged once. What can change is more than the configuration: loading it opens
the files it names, and a reload that runs in the moment one is being
replaced — a certificate missing while a Secret is rotated, the new
certificate in place before its key — is rejected for that file alone. So
while a configuration is rejected the watch also looks at the files it names
and the load opens, and those of the configuration in force:

- `otlp.tls.ca_file`, `cert_file` and `key_file`, when `otlp` is enabled;
- `web.basic_auth.username_file` and `password_file`, when it is enabled;
- a grpc collector's `request.protoset_file` and `request.proto_files`, and
  the files those `.proto` files import, through however many files, which
  the configuration does not name and the compile reads all the same: each
  where the `proto_import_paths` resolved it, and a missing one in every
  place it was looked for, so a reload rejected for an import that is not
  there is tried again when the file appears. The place in an earlier import
  path where a file was looked for before it was found counts too, since a
  file appearing there is the one compiled from then on; a file of the same
  name in a later import path is never read, and is not looked at. Neither
  are the well-known files (`google/protobuf/*.proto`), which are built in.

When one of them appears, disappears or changes — its modification time, its
size, its permissions, or the file the path leads to, through a symbolic
link, which is how Kubernetes swaps in a new version of a Secret, or by
another file renamed over it, which its device and inode tell — the next
tick reloads, once however many of them changed, and a reload that then
succeeds is logged like any other. A tick that finds them as they were does
nothing and logs nothing: it reads no file and compiles none. The descriptor
files are watched as they were before the rejected reload read them, so one
mended while that reload was still running — after it was read, before the
reload was rejected — is not taken as read: the next tick reloads. Once the
configuration is in force the watch goes on looking at the
[descriptor files](#descriptor-files) alone, and leaves the certificates and
the credential files be: nothing in the configuration is checked against
them, and the export and the exporter's own authentication each read their
files again when they change. A
collector's own credential and TLS files are read at each request, not when
the configuration loads, and never reject a reload.

The [static target file](STATIC-TARGETS.md#reloading) is tried again the
same way. Checking it opens the `request.protoset_file` and
`request.proto_files` of the grpc collectors whose targets set a
`request.message`, and the files those `.proto` files import, which the
message is checked against, so a reload of the target file in the moment
such a file is being replaced is rejected for that file alone. While the
target file is rejected the watch looks at those files too, the imported
ones as for the configuration and each as it was before the check read it,
and the tick after one changes reads the
target file again,
although it is as it was, and the configuration with it, whose
[descriptor files](#descriptor-files) they are; a tick that finds them as
they were does nothing and logs nothing. Once the target file is in force
they go on being watched as the configuration's descriptor files are: one
changing reads both again.
A target file rejected for what it says itself — one that is not YAML, a
target without a collector — opens no other file, and is read again when it
changes. A target's own credential files (`request.bearer_token_file`,
`request.basic_auth_file`) are read at each scrape and never reject a
reload.

The watch does not relax any reload rule. An invalid configuration, one that
would disable OTLP while a loaded static target sets `export_via_otlp`, a
collector name defined twice, and a pre-script that stops producing `data` are
all still rejected, with the last valid configuration
left active and the reason logged, with the rejected file's path as `file`
(`configuration reload rejected` or `static target reload rejected`). `http_exporter_config_last_reload_successful`
then reads `0` until a reload succeeds, so a change that did not take can be
alerted on — see [Configuration reloads](SELF-METRICS.md#configuration-reloads).

With the watch on, the line of a rejected reload also says, as
`retried_when`, what the watch reads that file again for, and names only
what applies: the file itself (`the configuration changes`, which counts its
collector files, or `the static target file changes`); with it a file
watched while the file is rejected, as above (`the configuration or a file
it names changes`, and `the configuration, a file it names or a file one of
those imports changes` when a `.proto` file it names imports others); and
the other of the two files, when this one was
rejected only because it disagrees with the other as in force — a target
naming a collector the configuration does not have, say — so that a change
to either may settle it (`the static target file, a file its check opens or
the configuration changes`). Without `--config.watch` nothing is read again
until a reload is asked for, and the line has no `retried_when`.

## Reloading on demand

A tool that has just written the configuration can reload the exporter at once
instead of waiting for the watch, and find out whether the new configuration
was accepted:

```sh
kill -HUP "$(pidof prometheus-universal-exporter)"   # always available

curl -X POST http://exporter:8080/-/reload            # with --web.enable-lifecycle
```

Both reload the configuration, with its collector files, and the static
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
other endpoints. `SIGHUP` needs no flag, and is safe to send at any time: one
that arrives while the exporter is still starting, reading its configuration
and checking its Python scripts, does not end the process but waits, and the
exporter reloads once as soon as it has started. Either way the reload is logged with
what triggered it — `"trigger":"http"`, `"sighup"` or `"watch"` — and counted
in the [reload self-metrics](SELF-METRICS.md#configuration-reloads). Reloads
from different triggers never interleave: one runs at a time.

Whatever the trigger, the exporter's state follows the new configuration: a
removed collector's [self-metrics](SELF-METRICS.md#collector-metrics) stop,
those of its [Python workers](SELF-METRICS.md#python-workers) with them,
and what was kept about it is dropped, and a changed collector's cached results
are dropped, and its [static targets](STATIC-TARGETS.md#reloading) are scraped
again within ten seconds. The configuration and the static target file take
effect together, in one step: nothing ever runs with the new one of the two
and the old other.

A probe or a static target scrape that had read its collector before the
reload goes on with the collector it read, and is answered. When the reload
removed that collector, or changed its definition, what such a late probe or
scrape leaves under the collector's name goes nowhere: its result is not
cached, so a collector brought back under the name is not answered with it
within `cache.ttl` or `cache.stale_if_error`, and it does not count in the
`limits.max_cache_entries` of the collector now under the name; and its
failure or its success is not taken for a failure or a
[recovery](LOGGING.md#repeated-failures) of that collector, the failure being
logged at debug level only, marked `"superseded":true`. A collector the reload
left as it was caches and logs as it did, whichever configuration its probe
had read.

That holds from the moment the reload is made: the reload itself, whether
`/-/reload`, `SIGHUP` or the watch made it, drops the cached results of the
collectors it removed or changed and forgets their remembered failures before
it returns, so `/-/reload` answers when that is done. A failure that ends in
the instant before is one from before the reload, logged as such and forgotten
with the rest. A changed collector's remembered failures are forgotten as a
removed one's are, so the first failure of the new definition is logged in
full as a first failure, and a static target that failed under the old
definition and succeeds under the new one is not logged as recovered. A
static target that the reload removed from the static target file, or changed
there, is treated as one whose collector changed, though the collector is as
it was: its remembered failures are forgotten, the failure of a scrape that
had read it before the reload is logged at debug level only, marked
`"superseded":true`, and the success of such a scrape is no recovery of the
target now under the name.

A static target's scrape that had read its target before a reload removed or
changed the target, or its collector, publishes nothing on the
[static targets endpoint](STATIC-TARGETS.md#the-static-targets-endpoint) or over OTLP, even when a target is
back under the name as it was: the endpoint keeps what it has of the target
now under the name until that target's own first scrape replaces it. That
scrape is made within ten seconds of the reload, or as soon as the earlier
scrape has ended, also when reloads in quick succession changed the target,
or its collector, and changed it back, or removed it and brought it back. A
target the reload left as it was, with its collector, is published as before.

Identical probes in flight [share one trip](#identical-probes-share-one-request) only
within one stay of their collector: a probe of a collector that reloads
changed and changed back, or removed and brought back, while an earlier probe
was at the target makes a trip of its own, and its result is cached and its
failure logged as any probe's of a collector in force.

## Debugging a probe

When a collector does not give what it should, add `&debug=true` to the probe
and read what happened, step by step, in a browser or with curl:

```sh
curl 'http://exporter:8080/probe?collector=app_json&target=https://api.example.com&debug=true'
```

```text
Debug probe of collector "app_json", target https://api.example.com
Took 184ms. A probe would have answered 502: metric queue_len failed: ...
A debug probe skips the response cache, shares no trip, records no self-metric and exports nothing over OTLP.

Requests
  1. GET https://api.example.com/v1/status?tenant=<redacted> -> 200 OK in 162ms
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

Metrics a probe would have served
  none
```

The report lists:

- **Requests:** every request the trip sent, retries and redirects included,
  with its headers and how it ended. A redirect is listed with the headers it
  was sent, and one to a host that is neither the request's origin nor in
  [`redirect_trusted_hosts`](REQUESTS.md#what-a-followed-redirect-carries)
  with a line of the headers it was not sent, by name: `not sent:
  Authorization, X-Api-Key — the redirect leads to a host that is not the
  origin the request was made to and is not in
  request.redirect_trusted_hosts`. A `grpc` call is listed with its method,
  metadata and status code. A `localfile` read, which sends nothing, is
  listed as the file it reads. A URL longer than 512 bytes is listed by its
  first 512.
- **Response:** the status, the headers — the first 100 of them, each value
  by its first 1,024 bytes — and the body, up to 64 KiB, as the
  target sent them: the `Content-Type` is the target's own, not the
  `charset=utf-8` the rules see after the body is
  [converted](#character-encodings). A body in another encoding is listed with
  its size as sent and what it was converted from —
  `Body: 3174 bytes in windows-1251, converted to UTF-8 before decoding and
  shown here as UTF-8` — and shown as the same text in UTF-8, since the report
  is UTF-8. A body that is not text is shown only by its length, and a
  directory as its files.
- **Stages:** each stage with how long it took and how it ended. A stage whose
  `error_handling` carried on says so.
- **Transform:** the series each metric got, the rules that got none, and the
  rules that carried on without some of their series, each by itself with
  how many and its first error, and with its expression where several rules
  export its metric name. A `prometheus` rule without a name is listed as
  `rule without a name`, and a metric's name longer than 200 bytes by its
  first 200.
- **Logs:** everything the trip logged at any level, whatever `--log.level`
  is, including what a Python script printed.
- **Metrics:** the exposition a probe would have served, and before the
  report, the status a probe would have answered. Where `cache.stale_if_error`
  would have answered with the last good result, the report says so.

A target can make most of that as long as it likes — the URL a redirect led
to, a header's value, the message of a gRPC status, a metric's name — and a
report is read by a person and made in memory whole. So everything in it but
the body has a bound, and what is cut says so with its whole length, as a
long [error](LOGGING.md#repeated-failures) does:

```text
  2. GET http://10.0.0.7/LLLLLLLL... (921645 bytes) (redirect) -> 200 OK in 3ms
    X-Trace: hhhhhhhh... (1046528 bytes)
    ... (5003 headers)
    queue_depth_aaaaaaaa... (1048577 bytes): 1
```

- **A line** longer than 8,192 bytes is shown by its first 8,192 bytes, cut
  between two characters, and `... (N bytes)`, the length of the whole line.
  That holds for every line of the report but the body's, whatever it is: the
  first line with a long target, a log line, a line of the exposition. The
  bound is past what is bounded already, so an error at its 2,000 bytes keeps
  its own length in the line that quotes it, and the 4,096 characters the log
  keeps of what a script printed are shown whole.
- **The body** keeps its own bound, 64 KiB, and its lines are shown as they
  are, however long.
- **A request's URL** is shown by its first 512 bytes and **a metric's name**
  in the Transform section by its first 200, as an error shows them, each
  with its length, so that how the request ended and how many series the
  metric got are still on the line.
- **A header's value** is shown by its first 1,024 bytes and its length, and
  **the headers** of a request or a response are listed up to the first 100,
  in the order of their names, and then how many there are.
- **A line of the exposition** that is cut is no valid exposition any more,
  so the section then starts with `Lines longer than 8192 bytes are cut below
  (3 of them), so this is not valid exposition as it stands; a probe serves
  them whole.` Only the report is cut: the answer of the probe itself, the
  line that says what a probe would have answered, the response cache, the
  self-metrics and what is exported over OTLP are what they are without a
  debug probe.

What the report redacts, listed below, is withheld before anything is cut,
and no cut ends inside a `<redacted>`. The number of lines is
bounded by the collector's own limits: the Transform section has a line for
each metric name and the exposition one for each series, up to
`limits.max_metrics`, and a directory one for each file read, up to
`request.max_files`, and for each file skipped.

The [collectors page](AUTHENTICATION.md#probing-from-the-browser) at
`/collectors` offers the same report with a *Debug report* switch on each
form, shown only when debug probes are enabled. A
[static target](STATIC-TARGETS.md#debugging-a-static-target) is debugged at
`/static-targets?debug=<name>`.

The report always answers `200` with `text/plain`, whatever the probe would
have answered. It takes the same parameters as the probe. For debug, `true`,
`1` or an empty value turn it on and `false` or `0` leave the probe as it is;
any other value is answered `400`.

A debug probe always goes to the target, and leaves nothing behind. It does
not read or fill the response cache, it does not share an identical probe in
flight, and it is not counted in the self-metrics or exported over OTLP,
which is what the report's line "records no self-metric" says. One thing is
counted all the same: a Python script it runs is run by the collector's
workers, and the [Python worker series](SELF-METRICS.md#python-workers)
count what the workers did, a debug probe's runs among them. Its
failures go to its report, not to the exporter's log, which records only one
`probe debug report served` line at `info`. The target's
[`allowed_targets` and `denied_targets`](REQUESTS.md#restricting-targets),
[`max_concurrent_probes` and `--probe.max-concurrent`](#limiting-concurrent-probes)
and [the probe's deadline](#probe-deadlines) apply as they do to any probe.
A trip that panics — a bug, in a request type, a decoder or a transform — is
reported as the `500` a probe would have answered, with the panic's message,
and logged with its stack; its slot is given back as after any other trip.

The report shows what the target answered, so debug probes are off unless the
exporter runs with `--web.enable-probe-debug` (the Helm chart's
`server.probeDebug`). Without the flag `debug=true` is answered `403`. When
`web.basic_auth` is configured, a debug probe needs the same credentials as a
probe. The report redacts:

- every query value, in the requests listed and in the errors that quote a
  URL alike. The query is otherwise shown as it was sent: each pair where it
  stood and as it was written, a pair with a `;` or a malformed escape
  included ([how a query is masked](LOGGING.md#repeated-failures));
- a URL's userinfo;
- the values of request and response headers whose names read as
  credentials ([the rule](LOGGING.md#repeated-failures) the logs follow): any
  name containing, in any case, `auth`, `cookie`, `token`, `secret`,
  `password`, `passwd`, `passphrase`, `passcode`, `key`, `session`,
  `signature`, `credential` or `jwt`, or with `sig`, `pwd`, `pw` or `pass` as
  a whole word of it, as in `X-Sig` or `X-Db-Pwd`.

Request bodies are not shown. A secret under any other header name, or in
the response itself, is shown as the target sent it, so turn the flag on
while a collector is being written or fixed, not for good.

## Readiness

`/health` answers `200` for as long as the process runs. `/ready` answers `200`
when the exporter should be sent probes, and `503` when it should not, with one
`not ready:` line per reason:

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

A rejected reload, of the configuration or of the static target file, leaves
the exporter ready: it keeps answering probes from the configuration in force.
Every replica rejects the same ConfigMap edit, so making them unready would
leave the Service with no endpoints and fail every probe the exporter could
still answer. The rejection shows where it costs nothing:
`http_exporter_config_last_reload_successful` reads `0` until a reload of the
file is accepted, which is what to alert on (see
[Configuration reloads](SELF-METRICS.md#configuration-reloads)), the reload
logs an `ERROR` line with the reason, and `/-/reload` answers `500`.

In Kubernetes, a pod that is not ready is taken out of its Service, and
Prometheus stops probing through it. Setting `otlp.unready_after_failures`
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
scrapes on every rollout. The Helm chart sets it to 5 seconds. Static targets
keep being scraped, and the [OTLP export](OTLP.md#shutting-down) keeps running,
through the delay.

Then it stops accepting connections, lets the
probes in progress finish for up to `--web.shutdown-timeout` (15 seconds by
default, long enough for a typical 10s scrape timeout), makes the [last OTLP export](OTLP.md#delivery) when OTLP is enabled,
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
A `SIGHUP` in that time [reloads](#reloading-on-demand) as at any other, and
does not end the process.

## Sizes

Every setting that is a number of bytes — `max_response_bytes`,
`limits.max_output_bytes`, `max_total_bytes` — takes either a number of bytes
or a number with a unit:

| Written | Bytes |
| --- | --- |
| `1048576` | 1048576 |
| `1e6`, `1000000.0` | 1000000 |
| `512KiB`, `512 KiB` | 524288 |
| `10MB` | 10000000 |
| `64MiB` | 67108864 |
| `1.5GiB` | 1610612736 |

`kB`, `MB`, `GB` and `TB` are powers of 1000, `KiB`, `MiB`, `GiB` and `TiB`
powers of 1024, and `B` or no unit is bytes. The unit is case insensitive, and
a fraction of a unit is rounded down to whole bytes. Anything else fails to
load, naming the value and its line: text that is no size, such as `lots`; a
negative size, such as `-1`; a fraction without a unit, such as `1.5`, since
there is no half byte; space around the size in quotes, such as `" 10MiB"`;
and a size of 2^63 bytes (`8388608TiB`) or more, which is past what a size
can hold. The [schema](#editor-support) flags the same spellings, all but the
last: the range is checked when the configuration loads.

One size has a least. `limits.max_output_bytes`, which bounds an answer of a
[Python worker](PYTHON.md#how-scripts-run) and is 1 MiB when it is left out
or `0`, is at least 38 bytes when it is set, the answer of a script that
emits no metric: a smaller one — `26`, `2.6e1`, `"26"`, `26B`, `0.02KiB` —
fails to load. The schema flags it where it is written as a number, from 1
to 37 however YAML writes the number. Written as text, in quotes or with a
unit, a size is to the schema text that reads as a size, and how many bytes
`20B` or `0.01KiB` comes to it cannot tell: that one is checked when the
configuration loads.

A number of bytes written without quotes is the whole number it equals
however YAML writes the number: `1e6`, `1E6`, `+1e6` and `1000000.0` are a
million bytes and `2.5e8` is 250000000, as they are whole numbers at every
other key that takes one, such as `limits.max_metrics`, and to the schema,
which is handed the number and not how it was written. The number is read by
its digits, not as the floating-point number nearest to it, so
`9007199254740993.0` is that many bytes. What is not a whole number of bytes
is refused as before, by the schema too: one with a fraction (`1.5`, `1e-1`)
and `.inf`. A whole number that no size holds is refused for what it is,
however it is written: `-1e3` and `-1.0` as negative, like `-1000`, and
`1e19` and `9223372036854775808.0` as too large, like `9223372036854775808`.
In quotes a number is text, and text is a
whole number of bytes or a number with a unit: `"1e6"` and `"1.0"` are
refused, by the schema too. The schema cannot tell a number's range, nor
digits past the seventeen or so a floating-point number holds: a fraction
written that far out (`1.00000000000000000001`) reaches it as the whole
number, and is refused when the configuration loads.

A response over `max_response_bytes` fails the scrape with `response size
5000 exceeds limit 1024` when the target said its size in `Content-Length`,
refused before the body is read, and with `response size exceeds limit 1024`
when it did not, once reading passes the limit. The limit counts the answer
decompressed. A `grpc` collector with `descriptors: reflection` holds the
files of its reflection question to the same limit, or to 10 MiB when the
limit is smaller ([gRPC](GRPC.md#descriptors)).

## Dry run

`--dry-run` answers "would this start?" without starting anything. It runs the
same validation startup runs, prints a JSON report on stdout, and exits:

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

It checks everything startup checks, including that every expression compiles
(see [Checked when the configuration loads](#checked-when-the-configuration-loads)).
A deprecated spelling does not fail the check; it is listed under
`details.deprecations` of the `config` entry and logged. The
[collector files](#collector-files) the configuration read are listed under
`details.collector_files`, and a collector name defined twice fails the check.
Each collector that sets `allowed_targets`, `denied_targets`, `accept_status`
or `accept_codes` is listed under `details.request_policies`, by name, with
those lists as they will be applied — `2XX` as `2xx`, `not_found` as
`NOT_FOUND` — so a list that lets through more or less than meant shows before
it is deployed.

It takes the same flags a real start does, and they matter: `--config.file` and
`--static-targets-file` choose what is checked, `--config.expand-env` and
`--static-targets.expand-env` decide whether `${NAME}` references are expanded
in each — so a check run where a referenced
variable is not set fails, exactly as startup would — `--python.path` is the
interpreter the Python scripts are compiled with, and `--config.watch` with
`--config.watch-interval` are reported when the watch is on. It never binds a
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
      "details": {"collectors": ["app_json", "legacy_text"], "otlp_enabled": false, "config_expand_env": false}
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
| `config` | always | The configuration file and its collector files load and are valid, and no collector name is defined twice. |
| `python_scripts` | always | Every pre-script and `python` transform compiles, and every pre-script produces `data`. Each faulty script is its own entry in `errors`, which names the [collector file](#collector-files) of a collector defined in one. A configuration without Python needs no interpreter and passes with `"scripts": 0`. |
| `config_watch` | with `--config.watch` | Reports `--config.watch-interval` under `details.interval`. A non-positive interval is a command-line error (exit 2) before the check, so this entry does not fail. |
| `static_targets` | with `--static-targets-file` | The [static target file](STATIC-TARGETS.md) is valid on its own, and against the configuration: every collector exists, and a target with `export_via_otlp` has OTLP export enabled. |

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
- [LOCALFILE.md](LOCALFILE.md) — the `localfile` request type.
- [GRAPHITE.md](GRAPHITE.md) — the `graphite` request type and decoder.
- [GRPC.md](GRPC.md) — the `grpc` request type.
- [AUTHENTICATION.md](AUTHENTICATION.md) — credentials for the target and for the exporter itself.
- [SELF-METRICS.md](SELF-METRICS.md) — the exporter's own metrics.
- [STATIC-TARGETS.md](STATIC-TARGETS.md) — targets the exporter scrapes itself and serves on the static targets endpoint.
- [OTLP.md](OTLP.md) — OTLP export.
