# Python transforms

Python is a transform, not a decoder. Configure it under the same
`transform` block as every other collector; the selected response decoder
first parses the response when applicable, then the Python script receives
`response.status_code`, `response.headers`, `response.body`, `response.text`,
`target`, `collector`, and decoded `data`. Scripts emit metrics with
`metric(...)` and may call `fail(...)`.

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

A name passed to `metric(...)`, and a label name, that is not a classic
Prometheus name — `http.server.duration`, `service.name` — fails the scrape
unless the collector sets `name_escaping`; see
[UTF-8 names](CONFIGURATION.md#utf-8-names).

`labels` is a mapping of label names to values. A value that is not a string
is written the way a [jq label](CONFIGURATION.md#collectors) is: `1234567`
as `1234567`, `0.5` as `0.5`, `True` as `true`, and `None` leaves the label
off. A list or a dict is not one value and fails the script, saying so; join
it into one first, with `",".join(tags)`.

The launcher blocks `socket`, `ssl`, `subprocess`, `ctypes`, `multiprocessing`, `threading`, `mmap`, `pty`, `pathlib`, `shutil`, `tempfile` and `urllib.request`, and the C modules beneath them, such as `_socket` and `_posixsubprocess`; shell execution; opening files, through `open`, `io.FileIO` or `os`; and package installation. A script may not import `posix`, `_io`, `_thread`, `select`, `selectors`, `fcntl`, `termios` or `importlib` itself, though the standard library it imports may, so `dataclasses` and the like still work. The sandbox keeps a script from doing by mistake what it should not; it is not a wall against a script written to get out, which Python cannot offer from inside the interpreter. Treat collector configuration as you treat the exporter's code, and rely on the container — the chart runs it as a non-root user with a read-only root file system — for isolation. Python has no supported network API; `requests` and `httpx` are unnecessary. `script_timeout` and metric/output limits apply. Declared `libraries` are validated against the supported names (`lxml`, `PyYAML`, and `python-dateutil`, or their import names `yaml` and `dateutil`); they are never installed during a scrape, and the image has no pip to install them with.

## What `data` is

`data` is the response as its decoder read it:

| Decoder | `data` |
| --- | --- |
| `json`, `yaml` | The document: dicts, lists, strings, numbers, booleans and `None`. |
| `graphite` | The [series document](GRAPHITE.md#the-series-document), `{"series": [...]}`. |
| `csv` | A list of rows: a dict per row, by header, or with `response.csv.header: false` a list of the row's fields. |
| `prometheus` | `{"metrics": [...]}`, a dict per series with `name`, `type`, `help`, `labels` and `value` — or, for a histogram, `buckets` (each `{"le": ..., "count": ...}`, the `+Inf` bucket as `float("inf")`), `sum` and `count`, and for a summary `quantiles` (each `{"quantile": ..., "value": ...}`), `sum` and `count` — and `timestamp`, in milliseconds, when the series has one. |
| `text`, `html`, `xml` | The body as a string; parse HTML and XML with [lxml](#parsing-html-with-lxml). |

`NaN` and the infinities arrive as the floats `float("nan")` and
`float("inf")`, and may be given back the same way, in `data` from a
pre-script and as a `metric(...)` value.

`metric(...)` takes a `value` as the other transforms do: a number, a numeric
string such as `"12"`, or a boolean, as `1` or `0`; anything else, `None`
included, fails the script naming the metric. A `timestamp` is milliseconds
since the Unix epoch, and may be a float, as `time.time() * 1000` is; it is
cut to whole milliseconds, and one beyond what 64 bits of milliseconds hold
fails the script.

A dict appended to `metrics` by hand, `{"name": ..., "value": ..., "labels":
{...}}`, is checked as `metric(...)` checks its arguments: its value and
timestamp are read the same way, `None` failing, its label values are
written the same way, `None` leaving the label off, and a missing `type` is
`gauge`.

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
  script, not starting the interpreter. A script that overruns fails the scrape
  with a timeout error, and its worker is killed; the next scrape starts another.
- **Declared libraries are preloaded.** The libraries in `libraries` are
  imported when the worker starts, so their import time is not counted against
  the script, and a library that itself needs a module the sandbox blocks, such
  as `threading`, still loads.
- **Errors.** A script that raises, calls `fail(...)` or `sys.exit()` fails that
  scrape with the Python error; the worker carries on. A worker that crashes, or
  answers with more than `limits.max_output_bytes`, is replaced.
- **Output.** `print` inside a script is captured per run and never mixes with
  the metrics. The first 4 KiB of it is logged at debug level, as `python
  transform printed` or `python pre-script printed` with the collector, so
  `--log.level=debug` shows it while a script is being written; the rest is
  dropped, and counts against `limits.max_output_bytes` no further.
- **Lifetime.** A worker is reused up to 1,000 times, at most four stay idle per
  collector after a burst of scrapes, and an idle one stops after five minutes
  — checked every minute, so a collector nobody scrapes any more does not keep
  its interpreters. A reload that changes or removes a script stops its idle
  workers at once, and a busy one when its run ends.
  Workers exit with the exporter.
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
of which apply to metrics emitted from a `python` transform. Reserve
`transform.type: python` for collectors whose metric *names* are not known until
the response is read; those still emit through `metric(...)` and may omit the
`metrics` array entirely.

The promotion is deliberately limited to the transforms that read structured
data. `csv`, `regex`, `css`, `xpath`, and `prometheus` keep receiving their own
decoded format, and a pre-script that returns a string still leaves the format
alone, so HTML and XML output is reparsed as before.

A pre-script of a `prometheus` transform gets `{"metrics": [...]}`, [as above](#what-data-is),
and must leave `data` in the same shape: it may drop series, change their
values and labels, or add series, which the transform's rules then read as
they read the exposition. A series without a `type` is `untyped`; a histogram
needs `buckets`, `sum` and `count`, and a summary `quantiles`, `sum` and
`count`. Anything else fails the pre-script, naming the series.

A CSV row a pre-script changed may hold numbers and `None`: a label read from
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
