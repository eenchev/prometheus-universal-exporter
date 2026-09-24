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

The launcher blocks `socket`, `subprocess`, `ctypes`, `multiprocessing`, `threading`, shell execution, and package installation. Python has no supported network API; `requests` and `httpx` are unnecessary. `script_timeout` and metric/output limits apply. Declared `libraries` are validated against the supported names (`lxml`, `PyYAML`, and `python-dateutil`, or their import names `yaml` and `dateutil`); they are never installed during a scrape, and the image has no pip to install them with.

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
  the metrics.
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
