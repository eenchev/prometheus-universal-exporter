# Python transforms

Python is a transform, not a decoder. Configure it under the same
`transform` block as every other collector; the selected response decoder
first parses the response when applicable, then the Python script receives
`response.status_code`, `response.headers`, `response.body`, `response.text`,
`target`, `collector`, and decoded `data`. `response.status_code` is the HTTP
status, a `grpc` call's status code — `0` for `OK`, or one of its
`accept_codes` — or `None` for a local file, which has no status. Scripts emit metrics with
`metric(...)` and may call `fail(...)`, and read sizes, durations, times,
percentages and Kubernetes quantities written as text with the
[value-parsing helpers](#value-parsing-helpers), `parse_bytes(...)`,
`parse_duration(...)`, `parse_time(...)`, `parse_percent(...)` and
`parse_quantity(...)`.

## What a script has

A script, a `python` transform's and a `pre_script`, starts with these
names, and whatever it imports:

| Name | What it is |
| --- | --- |
| `data` | The response as its decoder read it ([What `data` is](#what-data-is)). A `pre_script` changes it, or binds the name to another value, for the rules to read. |
| `response` | The answer: `status_code`, `ok` (whether the status is a success: an HTTP 2xx or a `grpc` call's `0`; `None` without a status), `headers`, `header(name, default=None)`, `body` and `text`, `json()` and `yaml()`, and, of an `http` or `graphite` request, `timings`, `tls`, `url`, `redirects`, `http_version` and `remote_ip`. |
| `target`, `collector` | The target as the probe gave it, credentials included, and the collector's name. |
| `params` | The [probe's parameters](REQUESTS.md#in-expressions), a dict of each `param_<name>` that was given, by its name, as text; empty when there is none. jq's `$params`. |
| `scrape_time` | When the scrape began, in seconds since the epoch, a float as `time.time()` gives one; the same moment the request's [clock placeholders](REQUESTS.md#dates-and-times) were filled from. jq's `$scrape_time`. |
| `last_success` | When the last scrape of the same collector, target and parameters that succeeded began, in the same seconds; `None` at the first scrape after a start, and for a new target. jq's `$last_success`. |
| `steps` | Of an `http` collector with [`request.then`](REQUESTS.md#chained-requests), each request of the chain by step name, a dict of its `status`, `headers`, `duration_seconds` and `url`, a fan-out step a list of them; `None` without a chain. jq's `$steps` ([Steps of a chain](CONFIGURATION.md#steps-of-a-chain)). |
| `state` | With `transform.state: true`, a dict kept between the script's runs ([State between runs](#state-between-runs)). |
| `metric(...)`, `fail(message)` | Make a series; fail the scrape with a message. |
| `log(message, level="info")` | Write a line of the exporter's log, at `debug`, `info`, `warning` or `error`. |
| `metrics` | The list `metric(...)` appends to, of a `python` transform. |
| `parse_bytes`, `parse_duration`, `parse_time`, `parse_percent`, `parse_quantity` | The [value-parsing helpers](#value-parsing-helpers). |
| `json`, `sys`, `os`, `io`, `contextlib`, `builtins` | The modules of those names, as if imported. |

`scrape_time - last_success` is how long ago the last good scrape was,
which makes a rate of two totals, or says that a gap was longer than it
should be:

```python
if last_success is not None:
    metric(name="app_seconds_since_last_scrape", value=scrape_time - last_success)
metric(name="app_report_age_seconds", value=scrape_time - parse_time(data["generated_at"]))
```

The last success is kept in memory, so a restart forgets it. A scrape that
fails, one answered from the [cache](CONFIGURATION.md#response-caching)
and a [debug probe](CONFIGURATION.md#debug-probes) keep the one before.

`response.headers` is a dict of each header's values as a list, by the
header's canonical name (`Content-Type`, `X-Mode`), as the response carried
them: `response.headers["X-Mode"]` is `["a", "b"]` for a header sent twice.
`response.header("x-mode")` is the form jq and yq rules read as `$headers`:
the values joined by `, `, `"a, b"`, whatever the name's case, or `None`
without the header, or a default, `response.header("x-mode", "")`.

An `http` or `graphite` collector's script also has what the request was,
besides its answer, as jq and yq rules have it
([Timings, TLS and the final URL](REQUESTS.md#timings-tls-and-the-final-url)):

- `response.timings`: a dict of the request's phases in seconds, `None` for
  a phase that it did not have, as in `response.timings["first_byte"]`;
- `response.tls`: a dict of the TLS connection and the target's certificate,
  as in `response.tls["not_after"]`, or `None` over plain HTTP;
- `response.url`: where the request went after its redirects, with its
  credentials withheld;
- `response.redirects`: how many redirects it followed;
- `response.http_version`: the version of HTTP that the answer came in,
  `"1.0"`, `"1.1"` or `"2"`;
- `response.remote_ip`: the IP address that the request's connection was
  made to, `"203.0.113.7"`, or `None` through a proxy or over a Unix socket.

All six are `None` for every other request type.

`target` is the target exactly as the probe or the static target gave it,
credentials included: `https://user:password@host/api?token=abc` reaches the
script as written, not redacted as the exporter's logs show it. Do not print
it or put it in a label as it is. What a script prints is logged at debug
level and shown in [debug reports](CONFIGURATION.md#debug-probes), and a
label is kept by Prometheus. When a script needs the host, take it apart
first, for example `urllib.parse.urlsplit(target).hostname`.

The worker is started with the exporter's environment, so `os.environ` in a
script holds whatever the exporter was given, a secret passed in a variable
for [`--config.expand-env`](CONFIGURATION.md#environment-variables) among it.
The same care applies: a script that prints the environment writes it to the
debug log, and one that puts a variable in a label hands it to Prometheus.

For example:

```yaml
transform:
  type: python
  script: |
    import re
    for line in response.text.splitlines():
        match = re.match(r"Worker (\S+) CPU: (\d+)%", line)
        if match:
            metric(name="vendor_worker_cpu", type="gauge",
                   value=float(match.group(2)),
                   labels={"worker": match.group(1)})
metrics: []
```

`metrics: []` is explicit for Python because the script creates the metric
definitions dynamically through `metric(...)`.

### What a rule of a `python` collector is for

A rule under `metrics` makes no series there. It names a series that the
script makes, by the `name` that the script gives it, and that is all it
says.

- A [debug probe](CONFIGURATION.md#debug-probes)'s report lists the rule
  among those that gave no series when the script made none of that name.
- The rule has its [`http_exporter_rule_failures_total`](SELF-METRICS.md)
  series, which stays `0`, because a rule that makes no series fails none.
- Each of its labels names a label of that series to
  [cut to `limits.max_label_value_length`](CONFIGURATION.md#long-label-values)
  with `truncate: true`. The label's `expression` must be there and is not
  read, and the rule's own is not read either.

Everything else that a rule says under another transform, the script says
itself, so it is refused at startup instead of ignored:

- `type` and `description`, which are
  `metric(..., type="counter", help="...")`;
- `required` and `error_mode`: a script that cannot do without something
  calls `fail("...")`, and
  [`error_handling.on_transform_error`](CONFIGURATION.md#when-a-stage-of-the-probe-fails)
  says what the collector does then;
- `items`, `scale`, `value_map`, `time_format` and `time_zone`;
- a label's `value`, `value_map` and `required`;
- a label without `truncate: true`, and a rule without a `name`.

A key written `""` is the same as leaving it out.

A name passed to `metric(...)`, and a label name, that is not a classic
Prometheus name — `http.server.duration`, `service.name` — fails the scrape
unless the collector sets `name_escaping`; see
[UTF-8 names](CONFIGURATION.md#utf-8-names). With `name_escaping:
underscores` or `values` a rule may name such a series, and such a label of
it to cut, by the names the script gives them: the names are escaped after
the rule has found its series. Without it such a rule is refused at startup,
as the series it names would fail every scrape.

`labels` is a mapping of label names to values. A value that is not a string
is written the way a [jq label](CONFIGURATION.md#collectors) is: `1234567`
as `1234567`, `0.5` as `0.5`, `True` as `true`, and `None` leaves the label
off. So does the empty string, `""`, as a rule's label that comes out empty
is left off under every other transform: Prometheus reads `m{l=""}` as the
series `m`. Two series a script tells apart by such a label alone are
therefore one series twice, and the scrape fails for the duplicate. A list
or a dict is not one value and fails the script, saying so; join it into one
first, with `",".join(tags)`.

The launcher blocks:

- `socket`, `ssl`, `subprocess`, `ctypes`, `multiprocessing`, `threading`,
  `mmap`, `pty`, `pathlib`, `shutil`, `tempfile` and `urllib.request`, and
  the C modules beneath them, such as `_socket` and `_posixsubprocess`;
- shell execution;
- opening files, through `open`, `io.FileIO` or `os`, except for reading
  time zone data;
- package installation.

A script may not import `posix`, `_io`, `_thread`, `select`, `selectors`,
`fcntl`, `termios` or `importlib` itself, though the standard library that
it imports may, so `dataclasses` and the like still work.

Time zone data is the one thing that a script may read: the system's zone
files (`zoneinfo.TZPATH`, `/usr/share/zoneinfo` and the like, and
`/etc/localtime`), `python-dateutil`'s bundled copy, and the `tzdata`
package's copy. `zoneinfo.ZoneInfo("Europe/Berlin")` and
`dateutil.tz.gettz("Europe/Berlin")` therefore work; the image ships the
system's files. (From Python 3.12, `zoneinfo` loads `sysconfig`, which
imports `threading`; the worker loads `zoneinfo` before the sandbox is in
place, so scripts can import it while `threading` stays blocked.) Any other
file, or one reached from those directories by `..` or a symlink out of
them, is still refused.

A script that fails inside a library is shown the library's frames with
their source lines, which the traceback reads through `linecache` and
`tokenize.open`. Those read a module's `.py` source, as the importer does,
and time zone data, and refuse any other file. The worker never writes
bytecode caches either (it runs Python with `-B`), so importing a module
whose `.pyc` is missing or out of date works in a read-only directory as in
a writable one.

The sandbox keeps a script from doing by mistake what it should not. It is
not a wall against a script written to get out, which Python cannot offer
from inside the interpreter. Treat collector configuration as you treat the
exporter's code, and rely on the container for isolation: the chart runs it
as a non-root user with a read-only root file system. Python has no
supported network API, and `requests` and `httpx` are unnecessary.
`script_timeout` and the metric and output limits apply. Declared
`libraries` are validated against the supported names (`lxml`, `PyYAML` and
`python-dateutil`, or their import names `yaml` and `dateutil`). They are
never installed during a scrape, and the image has no pip to install them
with.

The worker runs the interpreter that `--python.path` names, `python3` by
default, which in the image is Python 3.12 (the Dockerfile's
`PYTHON_VERSION`). The sandbox depends on what the standard library imports,
and when, which changes between releases, so 3.12 is the release that it is
built and tested on. Where Python 3.13 is installed, the test suite also runs
the sandbox, a library's traceback, and how a script's error is cut on it.
Other releases may work, untested.

### Value-parsing helpers

A script and a pre-script have five functions besides `metric` and `fail`:
the [value-parsing helpers](CONFIGURATION.md#value-parsing-helpers) that jq
and yq expressions call as filters.

- `parse_bytes("1.00M")` is `1048576.0`, `parse_duration("1h2m")` is
  `3720.0`, `parse_time("2026-10-03T09:00:00Z")` is `1791018000.0`,
  `parse_percent("55.12%")` is `0.5512`, and `parse_quantity("250m")` is
  `0.25`.
- Each takes one value and gives a `float`, read and rounded exactly as the
  jq function of its name reads it, from the same forms; the two are tested
  against the same vectors.
- `parse_bytes`, `parse_duration` and `parse_quantity` give an `int` or a
  `float` back as it is, taking it to be bytes, seconds or a quantity
  already.
- Text that a helper cannot read raises a `ValueError`, and anything else
  that is not text a `TypeError`. The script may catch it; otherwise it fails
  the script as any exception does, with a traceback showing the script's
  line and the helper's message:

```python
for row in data:
    try:
        size = parse_bytes(row["size"])
    except ValueError:
        continue          # a row the target wrote "n/a" in
    metric(name="volume_size_bytes", value=size, labels={"volume": row["name"]})
```

## What `data` is

`data` is the response as its decoder read it:

| Decoder | `data` |
| --- | --- |
| `json`, `yaml` | The document: dicts, lists, strings, numbers, booleans and `None`. |
| `ndjson` | The list of the values of the lines, each as `json` gives it. |
| `logfmt` | The list of the records, a dict per line of its keys and their values as strings, `None` for a key written alone. |
| `toml` | The document, as for `json`, with each date and time a string ([NDJSON, logfmt, TOML and INI](CONFIGURATION.md#ndjson-logfmt-toml-and-ini)). |
| `ini` | A dict of each section's dict of strings, and of each key before the first section to its string. |
| `influx` | The list of the points, a dict per line of its `measurement`, its `tags`, a dict of strings, its `fields`, a dict of floats, ints, booleans and strings, and its `timestamp`, an int or `None` ([InfluxDB line protocol](CONFIGURATION.md#influxdb-line-protocol)). |
| `protobuf` | A dict of the message's fields by their `.proto` names, as a `grpc` answer is, a 64-bit integer a string ([Protobuf responses](CONFIGURATION.md#protobuf-responses)); `response.body` is the bytes as they came. |
| `binary` | A dict of each field's name to its int, float, str or list of numbers, as `response.binary` lays them out ([Binary answers](CONFIGURATION.md#binary-answers)); `response.body` is the bytes. |
| `graphite` | The [series document](GRAPHITE.md#the-series-document), `{"series": [...]}`. |
| `csv` | A list of rows: a dict per row, by header, or with `response.csv.header: false` a list of the row's fields. |
| `prometheus` | `{"metrics": [...]}`, a dict per series with `name`, `type`, `help`, `labels` and `value` — or, for a histogram, `buckets` (each `{"le": ..., "count": ...}`, the `+Inf` bucket as `float("inf")`), `sum` and `count`, and for a summary `quantiles` (each `{"quantile": ..., "value": ...}`), `sum` and `count` — and `timestamp`, in milliseconds, when the series has one. A histogram or summary the target wrote without a `_sum` or a `_count` has no `sum` or `count` key: read one with `series.get("sum")`. A histogram's `count` and the `count` of its `+Inf` bucket are each what the target wrote, and [may differ](CONFIGURATION.md#character-encodings) for a target scraped between two of its updates. A `count`, the series' or a bucket's, must stay a whole number from 0: `2.5` fails the scrape. |
| `text`, `html`, `xml` | The body as a string; parse HTML and XML with [lxml](#parsing-html-with-lxml). |
| `none` | `None`; `response.body` and `response.text` are the body as the target sent it, not converted from its encoding ([No decoder](CONFIGURATION.md#no-decoder)). |

`NaN` and the infinities arrive as the floats `float("nan")` and
`float("inf")`, and may be given back the same way, in `data` from a
pre-script and as a `metric(...)` value. Integers of any length arrive as
Python `int`s and come back exact: an ID such as `1500000000000000001` that a
pre-script leaves in `data` keeps every digit in a label, rather than being
rounded to the nearest float.

`data` arrives nested as deep as its decoder read it, and a pre-script may
leave it that deep.

- A response decodes into a value whose lists and dicts nest at most 10,000
  levels, one inside another, whatever its format. A document nested deeper
  fails the scrape in the `decode` stage before any script runs.
- A response that decodes is never refused on its way to a script for its
  depth, whichever Python runs the script.
- What a pre-script leaves nested deeper than 10,000 levels, and a list or a
  dict that holds itself, fails the scrape with `python pre-script failed:
  RecursionError: data is nested more than 10000 deep, or a list or a dict
  in it holds itself; the exporter reads what a script leaves in data nested
  10000 deep at most, as deep as it decodes a response`.
- A list or a dict that holds itself is found where the worker comes back to
  it, at the same cost whether it is in itself once or twice, and not by
  following it ten thousand levels down.

What goes back is a `str`, an `int`, a `float`, a `bool`, `None`, a list, a
tuple or a dict of those. A set, `bytes`, a date or a `Decimal` left in
`data`, or in an entry appended to `metrics`, fails the script saying which
it is and what to make of it: `python pre-script failed: TypeError: what the
script left in data holds a set, which cannot be handed back to the
exporter: ...; make it one of those, as list(a_set), str(a_date) or
float(a_decimal) do`.

`data` goes back to the exporter as JSON, which cannot say that a list or a
dict is one that it has written already. One that is in `data` twice is
written twice and read back as two: `x = [1, 2]; data = [x, x]` leaves
`[[1, 2], [1, 2]]`. What a script built small can therefore be long when
written out. A list that holds another twice, which holds a third twice, and
so on forty times, is forty lists to the script but a million million values
as JSON, and the worker does not write it out to find that out. `data` that
holds a list or a dict more than once, and is longer than
`limits.max_output_bytes` whatever is in it (each value is at least a byte,
and a string or a key as many bytes as it is long), fails the scrape with

```text
python pre-script failed: OverflowError: what the script left in data is longer than limits.max_output_bytes (1048576 bytes) written out, a list or a dict that is there more than once being written each time; leave less there, or raise limits.max_output_bytes
```

and the worker carries on. It has gone through each list and dict once, in
the time and the memory that the script took to build them or, where the
script built a great deal, in a multiple of the limit. The `metrics` of a
transform are held to the same rule:

- lists like these where a metric has one value fail the scrape as a list of
  two numbers there does (`metric "m" label "l" is an array of 2 values, not
  a single value; ...`);
- under a key of your own they are passed over;
- metrics that hold one entry so often that they are longer than the limit
  fail with the same `OverflowError`, `... left in metrics ...`.

A string is written each time it is there too, and one long string held many
times is the most common way to a long answer: `data = [text] * 5000`, a
dict with the same text for every key, or a `blob` label or a `help` that
every one of thousands of metrics carries. A hundred thousand characters held
five thousand times are 500 MB as JSON from 100 kB of the script's memory.

The worker does not make that either. While it looks through an answer, it
adds up how long the strings in it are, each as often as it is there. An
answer whose strings alone come to more than `limits.max_output_bytes`, and
which is therefore longer than the limit whatever else is in it, fails the
scrape with the same `OverflowError`, naming `data` or `metrics`, before
anything is written. That takes a few milliseconds and the memory that the
script took, and the worker carries on. It goes for one long text alone as
well, and for an answer of many different texts that are too long together:
`leave less there, or raise limits.max_output_bytes` is what to do about
each. The strings that are values count in this way, each as long as it is
in characters.

A key is written each time its dict is, so one long string that is the key
of every row, or the name of a label on every metric, is written as often:
`data = [{text: 1} for _ in range(5000)]`, or
`metric("m", labels={text: "v"})` five thousand times. Keys count with the
strings, but the worker does not add them up for every answer. That would
cost every answer half again of the look through it, for keys that are a few
characters each in nearly every answer. Instead, it adds up the keys of a
few dicts, and lets each stand for the values around it:

- In an answer that it writes as it is, it goes by levels: the answer is the
  first level, and a level is everything that the lists and dicts of the
  level above hold. In each level that has four values or more, it looks at
  the fourth value and at every sixty-first after it, and adds up the keys
  of those that are dicts. Their total is multiplied by the level's number of
  values divided by the number looked at: four times in a level of four values,
  sixty-four times in one of sixty-four, and about sixty-one times in a long
  one.
- In an answer that it copies before writing, because of a `NaN` or a value
  of another type in it, it goes by the dicts that its copy is made of, in
  the order in which it finished them: a dict after the dicts inside it, and
  the answer itself last. It adds up the keys of the fourth dict and of every
  sixty-first after it, and multiplies their total by the copy's number of
  dicts divided by the number looked at.

Only where it found keys in this way, and those keys, counted so, would with
the strings be longer than the limit or within 64 characters of it, does it
add up the keys of every dict, each as often as it is written. The answer
then fails with the same `OverflowError` when its keys and its strings
together come to more than `limits.max_output_bytes`.

- An answer nested deeper than the copy follows is written key by key, and
  each key is counted as it is written.
- A key counts when it is a string (a `str`, or a value of a class made from
  it, such as a member of an enum of strings), as long as it is in
  characters. A key that is a number, a boolean or `None` counts nothing,
  and neither do numbers and the punctuation between values.
- The answer's own keys are among those counted: `ok`, `log` and `data`, and
  for each metric `name`, `type`, `value`, `labels`, `help` and `timestamp`.
  Metrics as `metric()` makes them therefore fail in this way too, instead of
  with the exporter's error below, once their names, label names, label
  values and help texts, with those keys, come to more than the limit. Under
  a limit of 1 MiB, that is from 2.4 times the limit written out for metrics
  of a one-letter name and one label, from 1.7 times for metrics of four
  labels and a help text, and from 1.4 times for metrics of eight labels and
  a long help text. Between the limit and that, they are written, and the
  exporter refuses them.

The look changes nothing of what the script left. It takes a value as a dict,
and a key as a string, by its type, not by what the value says its class or
its length is, and it runs none of a class's own methods. A mapping or a key
of a class of your own, or one that stands in for another object, is written
as it always was, wherever it stands in the answer.

An answer is therefore refused only when it is certainly longer than the
limit, and the same answer always ends the same way. But not every answer
that long keys make too long is refused.

- The look does not pass the first three values of a level, any value of a
  level of fewer than four, the values between one that it looks at and the
  sixty-first after it, or, in an answer that the worker copies, the first
  three dicts that it finishes.
- Long keys that are only there are not counted: in the first three rows,
  say, or in the first column of rows of exactly sixty-one columns, where
  the sixty-first value after the fourth of one row is the fourth of the
  next, so that the look passes the fourth column of every row and no other.
- Nor are long keys counted that are in so few of the dicts that the look
  does pass that, counted for the values around them, they do not reach the
  limit.

Such an answer may be written and then refused by the exporter, as is any
answer that the worker does not refuse: it is not measured by the worker but
written. One longer than `limits.max_output_bytes` (its strings and the keys
that the worker counted within the limit, and the whole over it with its
numbers, what stands between them, or keys that the look did not pass) fails
the scrape with `python pre-script output exceeds limit`
(`python transform output exceeds limit`) and costs its worker, as below.

What the script itself does with data that deep is bounded by its
interpreter.

- A function that calls itself for each level stops with a `RecursionError`
  at the recursion limit, `sys.getrecursionlimit()`, which is 1000: walk deep
  data in a loop instead.
- Raising the limit with `sys.setrecursionlimit` lasts as long as the worker
  and is at the script's own risk, because an interpreter that recurses
  deeper than its stack holds ends without an error, and the worker with it.
- The standard library's own walks stop too, at a depth that depends on the
  Python release. `json.dumps(data)`, `json.loads(response.text)`,
  `response.json()` and `repr(data)` stop at the recursion limit up to
  Python 3.11, a little under 1,000 levels. In 3.12 (the image's) and 3.13
  they stop at a limit of the interpreter's own on nested calls in C, which a
  script cannot change, a little under 10,000 levels (9,997 for `json`).
  From 3.14 they stop at what the stack holds.
- `copy.deepcopy(data)` stops at about half the recursion limit on all of
  them.

`metric(name, type="gauge", value=0, labels=None, help=None,
timestamp=None)` takes a `value` as the other transforms do: a number, a
numeric string such as `"12"`, or a boolean, as `1` or `0`.

- A number of another kind that says what float it is (a `decimal.Decimal`,
  a `fractions.Fraction` or a numpy number) is that float. Anything else,
  `None` included, fails the script, naming the metric, and so does a whole
  number too large for a float.
- A `timestamp` is milliseconds since the Unix epoch, and may be a float, as
  `scrape_time * 1000` is. It is cut to whole milliseconds, and one beyond
  what 64 bits of milliseconds hold fails the script. Given to
  `metric(...)`, a `datetime` with a time zone is its moment, while one
  without a zone fails the script, and so does a number that reads as
  seconds, as `time.time()` gives them.
- A label value of `bytes` is its text, read as UTF-8.
- A `help` given with any series of a name is the metric's.
- `name`, `type` and `help` are strings, and `None` for the type or the help
  means none given. Anything else fails the script, naming the metric and
  the argument, as in `metric 'jobs' help 5 is not a string`.
- A list, a dict, a tuple or a set given where `metric(...)` takes one value
  is named by what it is and how many items it has, not written out:
  `metric 'jobs' help a list of 3 items is not a string`. The error is
  therefore the same, and as short, for a list of three numbers, one of a
  thousand, and one nested deeper than Python itself can write.

A dict appended to `metrics` by hand,
`{"name": ..., "value": ..., "labels": {...}}`, is checked as `metric(...)`
checks its arguments. Its value and timestamp are read the same way, with
`None` failing; its label values are written the same way, with `None` and
the empty string leaving the label off; and a missing `type` is `gauge`. A
name, type or help that is not a string, labels that are not a mapping, and
an entry of `metrics` that is not a dict fail the scrape the same way,
naming the metric and what is wrong with it: `metric "jobs" labels are an
array of 1 item, not a mapping of label names to values`.

An entry of `metrics` is flat, as `metric(...)` makes it: `name`, `type`,
`value`, `help` and `timestamp` hold one value each, and `labels` a dict of
one value for each label.

- A list or a dict where one value belongs is the script's mistake, and
  fails the scrape as a script failure. The error names the metric, the part
  of it, and what stands there by its kind and the number of its items:
  `python transform: metric "jobs" label "queue" is an array of 3 values,
  not a single value; select one, or join them with join(",")`,
  `metric "jobs" help an array of 3 items is not a string`, or
  `metric "jobs" value an object with 1 key is not a number`.
- The failure is the same whatever the list or the dict holds: three
  numbers, lists nested thousands of levels deep (deeper than any response
  decodes), or itself. None of them fails as the interpreter's
  `RecursionError`, and the worker serves the next scrape.
- A key of the script's own in an entry, one that no metric has, is not
  read, whatever it holds.

`response.text`, `response.body` and, where the decoder gives a script the
body as text, `data` are one string, not three copies: a large response costs
its size once to hand to a script.

## State between runs

A script's names are gone when it ends. `transform.state: true` gives the
collector's scripts one that stays: `state`, a dict, which holds at each
run what the run before left in it.

```yaml
transform:
  type: python
  state: true
  script: |
    total = data["requests_total"]
    before = state.get("requests_total")
    if before is not None and total >= before:
        metric(name="app_requests_since_last_scrape", value=total - before)
    state["requests_total"] = total
```

- The first run finds `state` empty, so read it with `state.get(...)`.
- Each target has its own state, each set of probe parameters of a target
  its own, and a `pre_script` and a `python` transform each their own.
- It is kept as JSON: text, numbers, booleans, `None`, lists and dicts with
  text keys, of at most 64 KiB written out. What JSON has no form of comes
  back as what it has: a tuple as a list, and a dict's key that is a
  number as its text, so `state[1]` is `state["1"]` at the next run. A script that leaves anything
  else in it, more than that, or `state` bound to something that is no
  dict, fails.
- What a script that failed left is not kept, and neither is what a
  [debug probe](CONFIGURATION.md#debug-probes) left, which reads the state
  and changes nothing. A scrape answered from the
  [cache](CONFIGURATION.md#response-caching) runs no script.
- **It is memory, not storage.** A restart empties it, and each replica has
  its own, so a target that the [target allocator](ALLOCATOR.md) moves to
  another exporter starts there from an empty state. The exporter keeps the
  states of the 4,096 scripts and targets used most recently, 64 MiB of them
  at most. Write the script so that an empty state costs one scrape's answer
  and no more.
- Two scrapes of one target at the same moment each read the state as it
  was, and the one that ends last is the one kept.
- A [reload](CONFIGURATION.md#reloading-on-demand) keeps the states of a collector
  it keeps, of one whose script it changed too, so an edited script finds
  what the one before it left. It drops those of a collector it removes or
  takes the key from: one added again under the name starts empty.

The key is refused on a collector with no Python script.

The [self-metrics](SELF-METRICS.md#collector-metrics) of a collector with
the key say what is kept of it: `http_exporter_python_states` (how many
states), `http_exporter_python_state_bytes` (their size), and
`http_exporter_python_states_dropped_total` (the states that the exporter
dropped to stay within its bounds, each of which cost a script its memory).

## How scripts run

Scripts run in long-lived Python workers, not in a new interpreter per scrape.
Starting CPython and importing lxml or dateutil takes tens to hundreds of
milliseconds; running a typical script takes well under one. A worker pays the
start once and then serves scrape after scrape.

- **One collector per worker.** A worker only runs one collector's scripts, so
  nothing one collector's script does to a module can affect another's. Each run
  gets fresh globals, so a variable from the last scrape is gone; module state,
  such as an attribute set on an imported module, lasts for the worker's life.
  A changed script, after a reload, gets new workers.
- **Timeouts.** `limits.script_timeout` (100 ms by default) bounds running
  the script. Its clock starts when the worker has read the response and is
  about to run the script.
  - When set, it is at least 1 ms. Below that, the time is spent more on the
    pipes to the worker and the machine's load than on the script, and even
    a script that does nothing would time out by chance, so a smaller value
    fails to load.
  - It does not count starting the interpreter, nor handing the worker the
    response, which takes longer the larger the response is: a one-line
    script reading a 10 MiB body does not time out because of the body's
    size. Handing the response over is bounded by
    [the probe's deadline](CONFIGURATION.md#probe-deadlines), as the whole
    probe is, and a worker that has not taken a request after 30 seconds is
    given up on.
  - A script that overruns fails the scrape with a timeout error,
    `python transform timed out after 100ms`, and its worker is killed; the
    next scrape starts another.
  - When the probe's deadline, not `script_timeout`, is what ends a script,
    the error says so: `python transform was stopped after 1.2s because its
    probe or scrape ran out of time, not because of limits.script_timeout
    (30s)`. The limit to raise is then the probe's, and the run is counted
    with the outcome `deadline`, not `timeout`.
- **Starting.** An interpreter has ten seconds to start: to import the
  collector's libraries and say that it is ready.
  - One still running after that is stopped, and the scrape fails with
    `python transform failed: the interpreter did not start within 10s: it
    was still running and had not said it was ready, so it was stopped; it
    did not crash: look at how busy the machine is and at how long the
    libraries the collector declares take to import`, followed by what it
    had written to stderr, if anything.
  - One that exited before it was ready fails the scrape with `the
    interpreter did not start: the interpreter exited: ` and what it wrote
    to stderr, which is where Python says why (a module that is not
    installed, for example). Only this case is a crash to look into.
  - An error shows the last 1,500 bytes of stderr, after `... ` when there
    was more: the end is where Python says why it stopped. The error of a
    worker that died in a run goes on after it to the hint about
    `limits.max_script_memory`.
  - Either failure to start is counted in
    `http_exporter_python_worker_start_failures_total`, and the next scrape
    starts another interpreter.
  - What the interpreter wrote to stderr is for the operator (a script may
    print the data there), so a probe's caller gets the error without it,
    ending `what it wrote to stderr is in the exporter's log`.
- **Declared libraries are preloaded.** The libraries in `libraries` are
  imported when the worker starts, so their import time is not counted
  against the script, and a library that itself needs a module that the
  sandbox blocks, such as `threading`, still loads.
- **Errors.** A script that raises, or calls `fail(...)` or `sys.exit()`,
  fails that scrape with the Python error, and the worker carries on.
  - A worker that crashes, or answers with more than
    `limits.max_output_bytes`, is replaced.
  - An answer that the worker does not write, because a list or a dict is in
    it too often or in itself, or because its strings, alone or with its
    keys, are longer than the limit, is the script's error, and the worker
    carries on. An exception is never such an answer, however long its text
    is (below).
  - A worker that died while it sat idle (killed by the kernel for memory,
    or by a signal that a script armed and left behind, such as
    `signal.alarm`) fails no scrape. It is found dead when it is next taken,
    counted as a `crash`, and another worker runs the script.

  The error is the traceback of your script alone: its five innermost
  frames, so the failing line is always there, each with the line of the
  script that it ran, then the exception. The worker's own frames, including
  those of `metric(...)` and `fail(...)`, are left out:

  ```text
  python transform failed: Traceback (most recent call last):
    File "<collector-python>", line 5, in <module>
      metric(name="rate", value=rate(row))
                                ^^^^^^^^^
    File "<collector-python>", line 2, in rate
      return row["requests"] / row["seconds"]
             ~~~~~~~~~~~~~~~~^~~~~~~~~~~~~~~~
  ZeroDivisionError: division by zero
  ```

  That is the error the exporter's log, a debug probe's report and, with
  `--web.debug=full`, the status page show. A probe's caller is answered the
  exception's type and the line of the innermost frame of the script, without
  the message or the source lines, which are what the script made of the
  data: `python transform failed: ZeroDivisionError at line 2 of the script;
  the rest is in the exporter's log`. An error the worker raises after the
  script, which has no frame, is answered by its type alone; `fail(...)`'s
  message, which names no exception, is answered as
  `[withheld; in the exporter's log]`. See
  [What a failed probe's caller is told](CONFIGURATION.md#what-a-failed-probes-caller-is-told).

  A traceback of more than 1,500 bytes is shown by what says most, so that
  the error, held to 2,000 bytes like any other
  ([Logging](LOGGING.md#repeated-failures)), still ends with the exception:

  - each line by its first 200 bytes and its length;
  - the exception's own line and, of a message of several lines, the first
    lines that fit in 750 bytes, with how many lines it had;
  - before it, the last frames that fit in the rest, after how many lines
    stood before them.

  So `raise Exception(text)` with a response of ten megabytes fails with its
  frame and the exception's line by its start:

  ```text
  python transform failed: Traceback (most recent call last):
    File "<collector-python>", line 1, in <module>
      raise Exception(data['message'])
  Exception: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa... (10485571 bytes)
  ```

  and four hundred exceptions raised from one another fail with `python
  transform failed: ... (3613 lines)` and then the end of the traceback: the
  last exception with its frames, and as many of those before it as fit.
  The same failure with a longer message, or a longer chain that ends the
  same way, is one failure to the log. The lines of markers under a source
  line (`^^^^`, `~~~~`) come from the `traceback` module, and Python 3.13
  draws them under more lines than 3.12, under a call such as `fail(...)`
  too. They count as lines like any other, so on 3.13 a long traceback may
  show one frame fewer, and a failure's text differs between the two.

  An exception's text does not count as output, and
  `limits.max_output_bytes` never turns it into `python transform output
  exceeds limit`. A traceback of more than 16,384 characters, or one whose
  line would not fit the limit, is cut by the worker itself, before it
  writes it, to exactly what is shown of it above.

  So `raise ValueError(response.text)` with a response of two megabytes,
  `fail(text)`, a `KeyError` of a large key, or a `SyntaxError` on a line of
  megabytes fails under the default limit of 1 MiB with the frame and the
  exception's line, as above. It is counted as the script's error
  (`script_error`, not `output_limit`), recognised as the same failure
  whatever the length, and the worker stays in service. The worker neither
  joins nor copies such a message to cut it, whatever its characters are: it
  is in memory as often as Python's `traceback` module makes it, two or three
  times depending on the version, where writing it whole took five.

  Where the worker cannot make the text of an error at all, the scrape
  still fails as the script's, with the type of its exception and of what
  kept the text from being written: `python transform failed: ValueError:
  (the text of this error could not be written: MemoryError)`. That one is
  a message `limits.max_script_memory` does not hold twice, which is how
  often the `traceback` module needs it: raise less of the data in the
  exception, or raise the limit. The worker stays in service after it.

  What is shown of an error fits any limit of 16 KiB or more, whatever its
  characters are: written out, it stays under 11 kB even where JSON writes
  every byte of it as six characters. Under a smaller limit that does not
  hold it, the error is what fits: what would be shown in half the 1,500
  bytes, and half again, which leaves out frames and lines of the message
  before the exception's own line. Under a limit too small for that line by
  its first 200 bytes, it is as many of its first characters as fit. The
  `MemoryError` of `limits.max_script_memory` is cut the same way.
- **Output.** `print` inside a script is captured per run and never mixes
  with the metrics.
  - The first 4 KiB of it is logged at debug level, as
    `python transform printed` or `python pre-script printed`, with the
    collector, so `--log.level=debug` shows it while a script is being
    written. The rest is dropped, and counts no further against
    `limits.max_output_bytes`.
  - `log("queue stalled", level="warning")` writes a line at a level of its
    own, as `python transform logged`, with the collector, the target and
    the message, for what should be seen without debug logging. A run writes
    at most 20 lines, each of at most 1,024 characters, whatever it prints.
  - A script that fails has the lines that it logged before written with its
    failure, and the last 1,024 characters of what it printed as `printed`
    in the line that its failure is logged in, with anything that reads as a
    credential masked. They often explain the failure, need no second run at
    debug level, and are written as sparingly as a repeating failure is.
- **The size of an answer.** `limits.max_output_bytes`, a
  [size](CONFIGURATION.md#sizes), bounds what a worker writes back for one
  run: the metrics that a script emitted, or the data that a pre-script
  left, with what it printed. It is 1 MiB when left out or `0`.

  When set, it is at least 38 bytes: the answer of a transform's script that
  emits no metric and prints nothing. With less, such a script could do
  nothing but fail, and a pre-script could leave nothing in `data` that is
  longer than `None` when written (under 27 bytes no worker even starts). A
  smaller limit is therefore refused when the configuration loads, by
  `--dry-run` and on a reload, in a collector with a script, with a
  pre-script, and with neither: `collector "c" limits.max_output_bytes is
  26, and a Python script that emits no metric answers in 38 bytes, so no
  transform's script could answer within it; set at least 38, or leave it
  out, or 0, for the default, 1MiB`.
- **The size of a request.** What the exporter hands a worker (the response,
  its body once, and `data`) is written out as one line of JSON. That line
  may be no longer than 32 times the response limit
  (`limits.max_response_bytes`), plus `limits.max_output_bytes`, plus the
  script. Only data that a decoder made far larger than the response comes
  near that. Such a request is refused while it is being written, before any
  worker reads it: `python transform did not run: the response and the data
  decoded from it come to more than N bytes written out for the worker, 32
  times the response limit (10485760 bytes) and limits.max_output_bytes
  (1048576 bytes) together, and the script; ...`. CSV rows by header are
  sent as the header's columns once and each row's cells, as long as the
  body, and the worker makes the dicts that a script reads from them, so a
  long header over many short lines costs the request no more than the body.
- **Memory.** `limits.max_script_memory`, such as `256MiB`, bounds the
  address space of each of the collector's workers through `RLIMIT_AS`, set
  after the libraries are loaded.
  - Everything that the worker's process has mapped counts against it: the
    interpreter itself (about 17 MiB of address space for Python 3.12 before
    any library), the libraries preloaded from `libraries`, and what the
    script allocates. A script therefore has the limit minus the interpreter
    and its libraries.
  - Workers run with one malloc arena (`MALLOC_ARENA_MAX=1` in their
    environment). Otherwise glibc would reserve 64 MiB of address space for
    the worker's second thread, the one that watches the exporter, and that
    reservation would be taken from the limit although nothing uses it. An
    exporter started with `MALLOC_ARENA_MAX` set passes its own value on
    instead.
  - A script that needs more fails the scrape with `MemoryError: the script
    ran out of memory under limits.max_script_memory`, and the worker
    carries on.
  - An interpreter built with its library inside it, as one compiled from
    source often is, holds some 40 MiB before any script runs, where a
    distribution's holds 20. Under a limit that it is already past, every
    scrape fails, saying so: `limits.max_script_memory (33554432 bytes) is
    less than the 44371968 bytes this Python interpreter holds before a
    script runs, so no script can run under it; set at least 59MiB, or 0
    for no limit`. A limit under what the bare interpreter holds is refused
    sooner, when the configuration loads and by `--dry-run`, in the same
    words.
  - A [debug probe](CONFIGURATION.md#debug-probes) says the most address
    space that the script's worker has had, beside the limit, as
    `python transform memory` with `worker_peak_bytes` and `limit_bytes` (0
    for no limit), which is what to set the limit from.
  - A library written in C, such as lxml, that cannot allocate may end the
    interpreter instead. The error then says that the worker died and that
    it ran under the limit, so raise the limit if the script needs more.
  - It is at least 32MiB; `0`, the default, leaves it unbounded. It is
    enforced on Linux, where the exporter's image runs.
- **Workers across collectors.** Each script has workers of its own, so many
  Python collectors can run many interpreters. `--python.max-workers` bounds
  the workers alive at once, starting, busy or idle, of every collector
  together.
  - A run that finds no worker free for its script stops the idle worker
    that has been unused longest, of any script, and starts its own in its
    place. When every worker is busy, it waits in line for one within its
    probe's deadline, and otherwise fails, saying so.
  - Each worker that becomes free lets the first run in line through,
    instead of every waiting run racing for it.
  - A worker stopped this way is counted with the reason `evicted`.
  - `0`, the default, leaves the workers bounded only per script.
- **Lifetime.** A worker is reused up to 1,000 times.
  - After a burst of concurrent scrapes, at most
    `--python.max-idle-workers` of a collector's workers (one by default)
    stay idle for its next scrape, and the rest stop, counted as `surplus`.
  - An idle worker stops once it has been unused for longer than
    `--python.idle-timeout`, two minutes by default. This is checked every
    minute, or every half of the timeout when that is shorter, so a
    collector that nobody scrapes any more does not keep its interpreters.
  - A reload that changes or removes a script stops its idle workers at
    once, and a busy one when its run ends. The workers of a collector that
    a reload removed are not kept for a collector added again under its
    name, which starts its own.
  - Workers exit with the exporter. An idle worker exits when the exporter's
    end of its request pipe closes, however the exporter ended. A worker
    busy in a script is not reading that pipe, and an exporter that was
    killed stops nobody, so every worker also checks its parent process once
    a second. It ends within about that second of the exporter being gone,
    even in the middle of a script that would never have returned. The check
    keeps running through a script that uses all of
    `limits.max_script_memory`.
- **Idle workers and memory.** Each idle worker is a CPython process of
  about 11 MB resident, 17 MB with lxml imported. What idle workers cost is
  therefore the Python collectors, times `--python.max-idle-workers`, times
  that: ten Python collectors keep 110 to 170 MB with the default of one,
  and kept 440 to 680 MB at four each, the earlier default.
  - A collector scraped by one Prometheus needs one worker, which serves
    scrape after scrape. More stay idle only to serve the next burst of
    concurrent scrapes (several Prometheus replicas, or static targets of
    one collector that are due together) without starting interpreters.
    Raise `--python.max-idle-workers` to the number of scrapes that a
    collector has in progress at once when those bursts are regular.
  - `0` keeps none idle: every run starts an interpreter, which takes tens
    of milliseconds, more with libraries to import, and nothing is left
    between scrapes. Runs of one script still run at the same time, each in
    a worker of its own.
  - The idle timeout, from `1s` with no upper bound, should outlast the
    longest scrape interval of the Python collectors. An idle worker that
    stops before the next scrape makes every scrape start one, which the
    `idle` stops in the [worker series](SELF-METRICS.md#python-workers)
    show, rising with every scrape. The default of two minutes is twice a
    scrape interval of a minute, so a collector scraped every minute or
    more often keeps its worker through a late or a missed scrape. Lengthen
    it for collectors scraped less often, or lower it, and
    `--python.max-idle-workers`, to free the memory sooner.
  - Both are set for the whole exporter, as `--python.max-workers` is, with
    no `limits` key in a collector: they are about what the exporter's
    machine spends on interpreters, not about what a script needs.
- **Metrics.** With `web.self_metrics.verbose`, the exporter publishes each
  collector's workers by state (starting, idle, busy), how many started or
  failed to, why they stopped, and how script runs ended, and the same for
  the execution pool as a whole. See
  [Python workers](SELF-METRICS.md#python-workers).

## Parsing HTML with lxml

The image bundles `lxml`, and `lxml.html` is the HTML parser for Python
scripts:

```yaml
transform:
  type: python
  libraries:
    - lxml
  script: |
    import lxml.html
    doc = lxml.html.fromstring(response.text)
    for row in doc.xpath('//table[@id="servers"]//tr[td]'):
        name, cpu = [cell.text_content().strip() for cell in row.xpath('./td')]
        metric(name="server_cpu", value=float(cpu), labels={"server": name})
metrics: []
```

Select elements with XPath. `lxml`'s CSS selector support needs the separate
`cssselect` package, which is not bundled; the native `css` transform covers
CSS selection without Python.

BeautifulSoup is not bundled. It could not run in the sandbox — it imports
`logging`, which imports the blocked `threading` module — so a collector that
declares `beautifulsoup4` or `bs4` fails validation with a pointer to
`lxml.html`. Replace `BeautifulSoup(response.text, "html.parser")` with
`lxml.html.fromstring(response.text)`, `.find_all(...)`/`.select(...)` with
`.xpath(...)`, and `.get_text()` with `.text_content()`.

## Reshaping a response instead of writing a Python transform

When a response only needs parsing, prefer a pre-script that returns a mapping
or a sequence over `transform.type: python`. A structured pre-script result
becomes the decoded response for the `jq` and `yq` transforms whatever
the endpoint actually returned, so the metrics are declared exactly like any
other collector's:

```yaml
transform:
  type: jq
  pre_script: |
    import re
    data = {"workers": [{"name": m.group(1), "cpu": int(m.group(2))}
                        for m in re.finditer(r"Worker (\S+) CPU: (\d+)%", response.text)]}
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

Python parses, the metric declaration stays uniform, and `error_mode`,
`required`, `description`, and `type` behave as they do everywhere else — none
of which apply to metrics emitted from a `python` transform, whose rules
[do not take them](#what-a-rule-of-a-python-collector-is-for). Reserve
`transform.type: python` for collectors whose metric *names* are not known until
the response is read; those still emit through `metric(...)` and may omit the
`metrics` array entirely.

The promotion is deliberately limited to the transforms that read structured
data. `csv`, `regex`, `css`, `xpath`, and `prometheus` keep receiving their own
decoded format, and a pre-script that returns a string still leaves the format
alone, so HTML and XML output is reparsed as before. A pre-script of a `css` or
`xpath` transform must leave `data` a string of markup: a dict or a list fails
the scrape saying so, rather than parsing into a document none of the rules
matches. To hand the rules structured data, use a `jq` or `yq` transform.

A pre-script of a `prometheus` transform gets `{"metrics": [...]}`,
[as above](#what-data-is), and must leave `data` in the same shape. It may
drop series, change their values and labels, or add series, which the
transform's rules then read as they read the exposition.

- A series without a `type` is `untyped`. A histogram has `buckets`, and a
  summary `quantiles`.
- A series' `type` and `help` are strings, and its `labels` a dict of label
  names to values; `None` for one of them means none given, as the key left
  out does. Anything else there (a number for the help, a list for the type,
  a list of pairs for the labels) fails the scrape as the script's failure,
  counted in `http_exporter_script_errors_total`, naming the series, the
  key and what stands there: `python pre-script: data["metrics"][0]:
  up_thing labels are an array of 1 item, not a mapping of label names to
  values`, `up_thing help 5 is not a string`, `up_thing type an array of 1
  item is not a string; give "gauge", "counter", "untyped", "histogram" or
  "summary"`. A type that is text but none of the five is refused when the
  series are validated, as `metric "up_thing" has invalid type "counterr"`.
- A label value is written as `metric(...)` writes one, and `None` or the
  empty string leaves the label off the series that the rules read. A label
  that the target's own exposition gave as `l=""` is not among a series'
  `labels` to begin with: the decoder leaves it off, with or without a
  pre-script, so a script that passes `data` on as it got it changes nothing
  of what is scraped
  ([Prometheus input](CONFIGURATION.md#character-encodings)).
- A histogram or summary left without `sum` or `count`, or with `None` for
  it, is exported without a `_sum` or a `_count`, like one that the target
  wrote without it, and a histogram's count is then its `+Inf` bucket's.
- A histogram is checked as one read from the target is: no bound, nor a
  summary's quantile, may appear twice, which fails the pre-script, naming
  the series.
- A histogram's `count` and the `count` of its `+Inf` bucket are read back as
  the script left them, and each is exported as it is. A script that changes
  one of the two should change the other as well. Otherwise it leaves a
  histogram whose two numbers differ, which the text format writes as they
  are and [OpenMetrics](CONFIGURATION.md#openmetrics) as `unknown`
  families. A histogram left with neither is exported with the buckets that
  it has, without a `+Inf` bucket and without a `_count`.
- A histogram left with nothing at all (no `buckets`, no `sum` and no
  `count`), or a summary with no `quantiles`, no `sum` and no `count`, fails
  the scrape instead of being exported as a `# TYPE` line with no sample.
  That is what a misspelled key leaves, and the error names the series, the
  keys that such a series has, and the keys that this one has (`the
  histogram made has no buckets, no sum and no count; a histogram series has
  the keys "buckets", "sum" and "count", and this one has the keys "bucket",
  "cnt", "name", "type"`).

A pre-script of a `csv` transform gets the rows, [as above](#what-data-is),
and must leave rows: `data` stays a list, and each row a dict, read by its
keys as by the header's names, or a list, read by number from 1. The script
may drop rows (`data = data[1:]` for a header line of a file read with
`header: false`), change cells, and add columns for the rules to name.

- Anything else left in `data` (a dict, a string, `None`) fails the scrape
  in the transform stage, saying what the script left: `python pre-script of
  a csv transform left data as an object with 1 key; it must leave a list of
  rows, each row a dict by column name or a list by column number` (`null`
  for `None`, a string in quotes, a number as written).
- A row that is neither a dict nor a list fails it too, by its number,
  counted from 1: `python pre-script of a csv transform left row 2 as "db1";
  a row must be a dict by column name or a list by column number`.
- Both are the script's failure, counted in
  `http_exporter_script_errors_total`, as an exception that it raised is.
- The rows need not have the same keys. A key that a row lacks, when
  another row has it, is an empty cell in that row, and a column that no row
  has is one that the response does not have
  ([Reading CSV](CONFIGURATION.md#reading-csv-what-to-expect)).
- A CSV row that a pre-script changed may hold numbers and `None`. A label
  read from a number is written as `metric(...)` writes one, `1234567` as
  `1234567`, and `None` leaves the label off.

Errors are classified as HTTP, decode, transform, missing data, validation, or resource-limit failures. `error_handling` accepts `fail`, `log`, and `ignore`; `allow_missing_keys` controls required extraction results. Limits default to conservative values and are enforced immediately before exposition.

CSV responses can use a native CSV transform without CSS or Python:

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

The entire `response` block may be omitted. The exporter infers CSV for the
`csv` transform, and header-based CSV parsing is enabled by default. Use
`response.csv` only when changing CSV behavior, such as selecting a custom
delimiter or disabling the header row. Likewise, a regex transform implies
`decoder.type: text`.

A Python transform implies no decoder. Without `decoder.type`, its
collector decodes each response by its `Content-Type` header (a `localfile`
collector by the file's extension), and by the content when that does not
say, and every configuration load warns about it. Set `decoder.type` (for
example, `text` or `json`) to fix what the script reads.

`error_mode` applies after decoding, when an individual metric is extracted.
Decode failures and response/transform incompatibilities are collector-level
errors controlled by `error_handling`.
