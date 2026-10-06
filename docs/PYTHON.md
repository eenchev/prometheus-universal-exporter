# Python transforms

Python is a transform, not a decoder. Configure it under the same
`transform` block as every other collector; the selected response decoder
first parses the response when applicable, then the Python script receives
`response.status_code`, `response.headers`, `response.body`, `response.text`,
`target`, `collector`, and decoded `data`. `response.status_code` is the HTTP
status, a `grpc` call's status code — `0` for `OK`, or one of its
`accept_codes` — or `None` for a local file, which has no status. Scripts emit metrics with
`metric(...)` and may call `fail(...)`.

`response.headers` is a dict of each header's values as a list, by the
header's canonical name (`Content-Type`, `X-Mode`), as the response carried
them: `response.headers["X-Mode"]` is `["a", "b"]` for a header sent twice.
`response.header("x-mode")` is the form jq and yq rules read as `$headers`:
the values joined by `, `, `"a, b"`, whatever the name's case, or `None`
without the header, or a default, `response.header("x-mode", "")`.

`target` is the target exactly as the probe or the static target gave it,
credentials included: `https://user:password@host/api?token=abc` reaches the
script as written, not redacted as the exporter's logs show it. Do not print
it or put it in a label as it is. What a script prints is logged at debug
level and shown in [debug reports](CONFIGURATION.md#debugging-a-probe), and a
label is kept by Prometheus. When a script needs the host, take it apart
first, for example `urllib.parse.urlsplit(target).hostname`.

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

A rule under `metrics` makes no series there: it names one the script makes,
by the `name` the script gives it, and that is all it says. A
[debug probe](CONFIGURATION.md#debugging-a-probe)'s report lists the rule
among those that gave no series when the script made none of that name; the
rule has its [`http_exporter_rule_failures_total`](SELF-METRICS.md) series,
which stays `0`, since a rule that makes no series fails none; and each of
its labels names a label of that series to
[cut to `limits.max_label_value_length`](CONFIGURATION.md#long-label-values)
with `truncate: true` (the label's `expression` must be there and is not
read, and the rule's own is not read either). Everything else a rule says
under another transform the script says itself, so it is refused at startup
rather than ignored: `type` and `description`, which are
`metric(..., type="counter", help="...")`; `required` and `error_mode`, for
which a script that cannot do without something calls `fail("...")`, and
[`error_handling.on_transform_error`](CONFIGURATION.md#when-a-stage-of-the-probe-fails)
says what the collector does then; `items`, `scale`, `value_map`,
`time_format` and `time_zone`; a label's `value`, `value_map` and `required`;
a label without `truncate: true`; and a rule without a `name`. A key written
`""` is the key left out.

A name passed to `metric(...)`, and a label name, that is not a classic
Prometheus name — `http.server.duration`, `service.name` — fails the scrape
unless the collector sets `name_escaping`; see
[UTF-8 names](CONFIGURATION.md#utf-8-names).

`labels` is a mapping of label names to values. A value that is not a string
is written the way a [jq label](CONFIGURATION.md#collectors) is: `1234567`
as `1234567`, `0.5` as `0.5`, `True` as `true`, and `None` leaves the label
off. So does the empty string, `""`, as a rule's label that comes out empty
is left off under every other transform: Prometheus reads `m{l=""}` as the
series `m`. Two series a script tells apart by such a label alone are
therefore one series twice, and the scrape fails for the duplicate. A list
or a dict is not one value and fails the script, saying so; join it into one
first, with `",".join(tags)`.

The launcher blocks `socket`, `ssl`, `subprocess`, `ctypes`, `multiprocessing`, `threading`, `mmap`, `pty`, `pathlib`, `shutil`, `tempfile` and `urllib.request`, and the C modules beneath them, such as `_socket` and `_posixsubprocess`; shell execution; opening files, through `open`, `io.FileIO` or `os`, but for reading time zone data; and package installation. A script may not import `posix`, `_io`, `_thread`, `select`, `selectors`, `fcntl`, `termios` or `importlib` itself, though the standard library it imports may, so `dataclasses` and the like still work. Time zone data is the one thing a script may read: the system's zone files (`zoneinfo.TZPATH`, `/usr/share/zoneinfo` and the like, and `/etc/localtime`), `python-dateutil`'s bundled copy and the `tzdata` package's, so `zoneinfo.ZoneInfo("Europe/Berlin")` and `dateutil.tz.gettz("Europe/Berlin")` work; the image ships the system's. (From Python 3.12, `zoneinfo` loads `sysconfig`, which imports `threading`; the worker loads `zoneinfo` before the sandbox is in place, so scripts can import it while `threading` stays blocked.) Any other file, or one reached from those directories by `..` or a symlink out of them, is still refused. The worker never writes bytecode caches either (it runs Python with `-B`), so importing a module whose `.pyc` is missing or out of date works in a writable directory as in a read-only one. The sandbox keeps a script from doing by mistake what it should not; it is not a wall against a script written to get out, which Python cannot offer from inside the interpreter. Treat collector configuration as you treat the exporter's code, and rely on the container — the chart runs it as a non-root user with a read-only root file system — for isolation. Python has no supported network API; `requests` and `httpx` are unnecessary. `script_timeout` and metric/output limits apply. Declared `libraries` are validated against the supported names (`lxml`, `PyYAML`, and `python-dateutil`, or their import names `yaml` and `dateutil`); they are never installed during a scrape, and the image has no pip to install them with.

## What `data` is

`data` is the response as its decoder read it:

| Decoder | `data` |
| --- | --- |
| `json`, `yaml` | The document: dicts, lists, strings, numbers, booleans and `None`. |
| `graphite` | The [series document](GRAPHITE.md#the-series-document), `{"series": [...]}`. |
| `csv` | A list of rows: a dict per row, by header, or with `response.csv.header: false` a list of the row's fields. |
| `prometheus` | `{"metrics": [...]}`, a dict per series with `name`, `type`, `help`, `labels` and `value` — or, for a histogram, `buckets` (each `{"le": ..., "count": ...}`, the `+Inf` bucket as `float("inf")`), `sum` and `count`, and for a summary `quantiles` (each `{"quantile": ..., "value": ...}`), `sum` and `count` — and `timestamp`, in milliseconds, when the series has one. A histogram or summary the target wrote without a `_sum` or a `_count` has no `sum` or `count` key: read one with `series.get("sum")`. A histogram's `count` and the `count` of its `+Inf` bucket are each what the target wrote, and [may differ](CONFIGURATION.md#character-encodings) for a target scraped between two of its updates. A `count`, the series' or a bucket's, must stay a whole number from 0: `2.5` fails the scrape. |
| `text`, `html`, `xml` | The body as a string; parse HTML and XML with [lxml](#parsing-html-with-lxml). |

`NaN` and the infinities arrive as the floats `float("nan")` and
`float("inf")`, and may be given back the same way, in `data` from a
pre-script and as a `metric(...)` value. Integers of any length arrive as
Python `int`s and come back exact: an ID such as `1500000000000000001` that a
pre-script leaves in `data` keeps every digit in a label, rather than being
rounded to the nearest float.

`data` arrives nested as deep as its decoder read it, and a pre-script may
leave it that deep: a response decodes into a value whose lists and dicts
nest 10,000 levels at most, one inside another, whatever its format, a
document nested deeper failing the scrape in the `decode` stage before any
script runs. A response that decodes is never refused on its way to a script
for its depth, whichever Python runs the script. What a pre-script leaves
nested deeper than 10,000 levels, and a list or a dict that holds itself,
fails the scrape with `python pre-script failed: RecursionError: data is
nested more than 10000 deep, or a list or a dict in it holds itself; the
exporter reads what a script leaves in data nested 10000 deep at most, as
deep as it decodes a response`. A list or a dict that holds itself is found
where the worker comes back to it, at the same cost whether it is in itself
once or twice, and not by following it ten thousand levels down.

`data` goes back to the exporter as JSON, which cannot say that a list or a
dict is one it has written already. One that is in `data` twice is written
twice and read back as two: `x = [1, 2]; data = [x, x]` leaves
`[[1, 2], [1, 2]]`. What a script built small can therefore be long written
out. A list that holds another twice, which holds a third twice, and so on
forty times, is forty lists to the script and a million million values as
JSON, and the worker does not write it out to find that out: `data` that
holds a list or a dict more than once and is longer than
`limits.max_output_bytes` whatever is in it — each value is a byte at least,
a string and a key as many as it is long — fails the scrape with

```text
python pre-script failed: OverflowError: what the script left in data is longer than limits.max_output_bytes (1048576 bytes) written out, a list or a dict that is there more than once being written each time; leave less there, or raise limits.max_output_bytes
```

and the worker carries on, having gone through each list and dict once, in
the time and the memory the script took to build them or, where the script
built a great deal, in a multiple of the limit. The `metrics` of a transform
are held to the same: lists like these where a metric has one value fail the
scrape as a list of two numbers there does (`metric "m" label "l" is an array
of 2 values, not a single value; ...`), under a key of your own they are
passed over, and metrics that hold one entry so often that they are longer than the
limit fail with the same `OverflowError`, `... left in metrics ...`. An
answer that holds no list or dict twice is not measured by the worker,
however long: it is written, and one longer than `limits.max_output_bytes`
fails the scrape with `python pre-script output exceeds limit` (`python
transform output exceeds limit`) and costs its worker, as below. So does one
that is longer only for a string it holds many times: a text is written each
time too, and a million references to a megabyte of it are a million
megabytes, which `limits.script_timeout` and `limits.max_script_memory` end.

What the script itself does with data that deep is bounded by its
interpreter. A function that calls itself for each level stops with a
`RecursionError` at the recursion limit, `sys.getrecursionlimit()`, which is
1000: go down deep data in a loop instead. Raising the limit with
`sys.setrecursionlimit` lasts as long as the worker and is at the script's
own risk, since an interpreter that recurses deeper than its stack holds ends
without an error, and the worker with it. The standard library's own walks
stop too, at a depth that depends on the Python release: `json.dumps(data)`,
`json.loads(response.text)`, `response.json()` and `repr(data)` at the
recursion limit up to Python 3.11, a little under 1,000 levels; at a limit of
the interpreter's own on nested calls in C, which a script cannot change, in
3.12 — the image's — and 3.13, a little under 10,000 levels (9,997 for
`json`); and at what the stack holds from 3.14. `copy.deepcopy(data)` stops at
about half the recursion limit on all of them.

`metric(...)` takes a `value` as the other transforms do: a number, a numeric
string such as `"12"`, or a boolean, as `1` or `0`; anything else, `None`
included, fails the script naming the metric. A `timestamp` is milliseconds
since the Unix epoch, and may be a float, as `time.time() * 1000` is; it is
cut to whole milliseconds, and one beyond what 64 bits of milliseconds hold
fails the script. `name`, `type` and `help` are strings, and `None` for the
type or the help is none given; anything else fails the script naming the
metric and the argument, as in `metric 'jobs' help 5 is not a string`. A
list, a dict, a tuple or a set given where `metric(...)` takes one value is
named by what it is and how many items it has, not written out — `metric
'jobs' help a list of 3 items is not a string` — so the error is the same,
and as short, for a list of three numbers, one of a thousand, and one nested
deeper than Python itself can write.

A dict appended to `metrics` by hand, `{"name": ..., "value": ..., "labels":
{...}}`, is checked as `metric(...)` checks its arguments: its value and
timestamp are read the same way, `None` failing, its label values are
written the same way, `None` and the empty string leaving the label off, and
a missing `type` is `gauge`. A name, type or help that is not a string, labels that are not a
mapping, and an entry of `metrics` that is not a dict fail the scrape the same
way, naming the metric and what is wrong with it — `metric "jobs" labels are
an array of 1 item, not a mapping of label names to values`.

An entry of `metrics` is flat, as `metric(...)` makes it: `name`, `type`,
`value`, `help` and `timestamp` hold one value each, and `labels` a dict of
one value for each label. A list or a dict where one value belongs is the
script's mistake and fails the scrape as a script failure, naming the metric,
the part of it and what stands there by its kind and the number of its items:
`python transform: metric "jobs" label "queue" is an array of 3 values, not a
single value; select one, or join them with join(",")`, `metric "jobs" help an
array of 3 items is not a string`, `metric "jobs" value an object with 1 key
is not a number`. The failure is the same whatever the list or the dict
holds: three numbers, lists nested thousands of levels deep — deeper than
any response decodes — or itself. None of them fails as the interpreter's
`RecursionError`, and the worker serves the next scrape. A key of the
script's own in an entry, one no metric has, is not read, whatever it holds.

`response.text`, `response.body` and, where the decoder gives a script the
body as text, `data` are one string, not three copies: a large response costs
its size once to hand to a script.

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
- **Timeouts.** `limits.script_timeout` (100 ms by default) bounds running the
  script: its clock starts when the worker has read the response and is about
  to run the script. It does not count starting the interpreter, nor handing
  the worker the response, which takes longer the larger the response is — a
  one-line script reading a 10 MiB body does not time out for the body's size.
  A script that overruns fails the scrape with a timeout error,
  `python transform timed out after 100ms`, and its worker is killed; the next
  scrape starts another. Handing the response over is bounded by
  [the probe's deadline](CONFIGURATION.md#probe-deadlines), as the whole probe
  is, and a worker that has not taken a request after 30 seconds is given up
  on. When the probe's deadline, not `script_timeout`, is what ends a script,
  the error says that — `python transform was stopped after 1.2s because its
  probe or scrape ran out of time, not because of limits.script_timeout (30s)`
  — so the limit to raise is the probe's, and the run is counted with
  the outcome `deadline`, not `timeout`.
- **Starting.** An interpreter has ten seconds to start in: to import the
  collector's libraries and say it is ready. One still running after that is
  stopped, and the scrape fails with `python transform failed: the
  interpreter did not start within 10s: it was still running and had not
  said it was ready, so it was stopped; it did not crash: look at how busy
  the machine is and at how long the libraries the collector declares take
  to import`, followed by what it had written to stderr, if anything. One
  that exited before it was ready fails the scrape with `the interpreter did
  not start: the interpreter exited: ` and what it wrote to stderr, which is
  where Python says why — a module that is not installed, for one. Only
  that is a crash to look for. Either start is counted in
  `http_exporter_python_worker_start_failures_total`, and the next scrape
  starts another interpreter.
- **Declared libraries are preloaded.** The libraries in `libraries` are
  imported when the worker starts, so their import time is not counted against
  the script, and a library that itself needs a module the sandbox blocks, such
  as `threading`, still loads.
- **Errors.** A script that raises, calls `fail(...)` or `sys.exit()` fails that
  scrape with the Python error; the worker carries on. A worker that crashes, or
  answers with more than `limits.max_output_bytes`, is replaced; an answer it
  does not write because a list or a dict is in it too often, or in itself,
  is the script's error, and the worker carries on. A worker that
  died while it sat idle — killed by the kernel for memory, or by a signal a
  script armed and left behind, such as `signal.alarm` — fails no scrape: it
  is found dead when it is next taken, counted as a `crash`, and another runs
  the script. The error is
  the traceback of your script alone — its five innermost frames, so the
  failing line is always there, each with the line of the script it ran,
  then the exception; the worker's own frames, `metric(...)`'s and
  `fail(...)`'s included, are left out:

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
- **Output.** `print` inside a script is captured per run and never mixes with
  the metrics. The first 4 KiB of it is logged at debug level, as `python
  transform printed` or `python pre-script printed` with the collector, so
  `--log.level=debug` shows it while a script is being written; the rest is
  dropped, and counts against `limits.max_output_bytes` no further.
- **Memory.** `limits.max_script_memory`, such as `256MiB`, bounds the address
  space of each of the collector's workers through `RLIMIT_AS`, set after the
  libraries are loaded. Everything the worker's process has mapped counts
  against it: the interpreter itself (about 17 MiB of address space for
  Python 3.12 before any library), the libraries preloaded from
  `libraries`, and what the script allocates — so a script has the limit
  less the interpreter and its libraries. Workers run with one malloc arena
  (`MALLOC_ARENA_MAX=1` in their environment): glibc would otherwise reserve
  64 MiB of address space for the worker's second thread, the one that
  watches the exporter, and that reservation would be taken from the limit
  though nothing uses it. An exporter started with `MALLOC_ARENA_MAX` set
  passes its own value on instead. A script
  that needs more fails the scrape with `MemoryError: the script ran out of
  memory under limits.max_script_memory`, and the worker carries on. A
  library written in C, such as lxml, that cannot allocate may end the
  interpreter instead; the error then says the worker died and that it ran
  under the limit, so raise it if the script needs more. It is at
  least 32MiB; `0`, the default, leaves it unbounded. It is enforced on Linux,
  where the exporter's image runs.
- **Workers across collectors.** Each script has workers of its own, so many
  Python collectors can run many interpreters. `--python.max-workers` bounds the
  workers alive at once, starting, busy or idle, of every collector together. A
  run that finds none free for its script stops the idle worker unused for
  longest, of any script, and starts its own in its place, or, when every
  worker is busy, waits in line for one within its probe's deadline and
  otherwise fails saying so. Each worker that frees lets the first run in line
  through, rather than every waiting run racing for it. A worker stopped this way is counted with the reason `evicted`.
  `0`, the default, leaves the workers bounded only per script.
- **Lifetime.** A worker is reused up to 1,000 times, at most four stay idle per
  collector after a burst of scrapes, and an idle one stops after five minutes
  — checked every minute, so a collector nobody scrapes any more does not keep
  its interpreters. A reload that changes or removes a script stops its idle
  workers at once, and a busy one when its run ends.
  Workers exit with the exporter. An idle worker exits when the exporter's end
  of its request pipe closes, however the exporter ended. A worker busy in a
  script is not reading that pipe, and an exporter that was killed stops
  nobody, so every worker also watches its parent process, once a second, and
  ends within about that second of the exporter being gone, even in the middle
  of a script that would never have returned. The watch keeps running through
  a script that uses all of `limits.max_script_memory`.
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

A pre-script of a `prometheus` transform gets `{"metrics": [...]}`, [as above](#what-data-is),
and must leave `data` in the same shape: it may drop series, change their
values and labels, or add series, which the transform's rules then read as
they read the exposition. A series without a `type` is `untyped`; a histogram
has `buckets`, and a summary `quantiles`. A series' `type` and `help` are
strings and its `labels` a dict of label names to values; `None` for one of
them is none given, as the key left out is. Anything else there — a number
for the help, a list for the type, a list of pairs for the labels — fails
the scrape as the script's failure, counted in
`http_exporter_script_errors_total`, naming the series, the key and what
stands there: `python pre-script: data["metrics"][0]: up_thing labels are an
array of 1 item, not a mapping of label names to values`, `up_thing help 5 is
not a string`, `up_thing type an array of 1 item is not a string; give
"gauge", "counter", "untyped", "histogram" or "summary"`. A type that is text
and none of the five is refused when the series are validated, as
`metric "up_thing" has invalid type "counterr"`. A label value is written as
`metric(...)` writes one, and `None` or the empty string leaves the label
off the series the rules read. A label the target's own exposition gave as
`l=""` is not among a series' `labels` to begin with: the decoder leaves it
off, with a pre-script or without one, so a script that passes `data` on as
it got it changes nothing of what is scraped
([Prometheus input](CONFIGURATION.md#character-encodings)). A histogram or summary left without
`sum` or `count`, or with `None` for it, is exported without a `_sum` or a
`_count`, as one the target wrote without it, and a histogram's count is then
its `+Inf` bucket's. A histogram is checked as one read from the target is:
no bound, nor a summary's quantile, may appear twice, which fails the
pre-script, naming the series. Its `count` and the `count` of its `+Inf`
bucket are read back as the script left them, each exported as it is: a
script that changes one of the two changes the other as well, or leaves a
histogram whose two numbers differ, which the text format writes as they are
and [OpenMetrics](CONFIGURATION.md#openmetrics) as `unknown` families. One
left with neither is exported with the buckets it has, without a `+Inf`
bucket and without a `_count`. A histogram left with nothing at all — no
`buckets`, no `sum` and no `count` — or a summary with no `quantiles`, no
`sum` and no `count`, fails the scrape rather than being exported as a
`# TYPE` line with no sample: that is what a misspelled key leaves, and the
error names the series, the keys such a series has and the keys this one has
(`the histogram made has no buckets, no sum and no count; a histogram series
has the keys "buckets", "sum" and "count", and this one has the keys
"bucket", "cnt", "name", "type"`).

A pre-script of a `csv` transform gets the rows, [as above](#what-data-is),
and must leave rows: `data` stays a list, each row a dict, read by its keys
as by the header's names, or a list, read by number from 1. The script may
drop rows (`data = data[1:]` for a header line of a file read with
`header: false`), change cells, and add columns for the rules to name.
Anything else left in `data` — a dict, a string, `None` — fails the scrape in
the transform stage, saying what the script left: `python pre-script of a csv
transform left data as an object with 1 key; it must leave a list of rows,
each row a dict by column name or a list by column number` (`null` for
`None`, a string in quotes, a number as written). A row that is neither a
dict nor a list fails it too, by its number, counted from 1: `python
pre-script of a csv transform left row 2 as "db1"; a row must be a dict by
column name or a list by column number`. Both are the script's failure,
counted in `http_exporter_script_errors_total` as an exception it raised is.
The rows need not have the same
keys: a key a row lacks, when another row has it, is an empty cell in that
row, and a column no row has is one the response does not have
([Reading CSV](CONFIGURATION.md#reading-csv-what-to-expect)). A CSV row a
pre-script changed may hold numbers and `None`: a label read from
a number is written as `metric(...)` writes one, `1234567` as `1234567`, and
`None` leaves the label off.

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
delimiter or disabling the header row. Likewise, `decoder.type: text` is
unnecessary for a regex or Python transform unless an explicit decoder is
needed for an ambiguous endpoint.

`error_mode` applies after decoding, when an individual metric is extracted.
Decode failures and response/transform incompatibilities are collector-level
errors controlled by `error_handling`.
