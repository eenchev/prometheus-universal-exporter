# Local files

The `localfile` request type reads a file from the exporter's own filesystem
instead of calling a URL. Everything after the read is the same as for `http`:
the file is decoded, transformed and exposed by the same collector rules, with
the same limits, caching and error policies.

Use it for what writes metrics, or a status, to disk rather than serving it:

- the `.prom` files a cron job or batch run leaves for node_exporter's textfile
  collector;
- a JSON, YAML, CSV or XML status file an appliance, an agent or a deployment
  writes;
- a small file under `/proc` or `/sys` that is mounted into the container.

## A collector

```yaml
collectors:
  - name: textfile
    request:
      type: localfile
      root: /var/lib/node_exporter/textfile_collector
      path: batch.prom
      max_age: 1h
    transform:
      type: prometheus
```

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `localfile`. |
| `root` | — | **Required.** The absolute directory the collector may read under. Nothing outside it can be read. `/` is refused. |
| `path` | — | The file, relative to `root` and to the target. May use [path parameters](REQUESTS.md#path-parameters). |
| `max_age` | off | Refuse a file last modified longer ago than this. |
| `max_response_bytes` | limit | The most that is read; a larger file fails the scrape. |

No other request key applies, and setting one — `method`, `headers`, a
credential — is a startup error, as it is for any key that belongs to another
request type. `root` is not checked for existence when the configuration loads,
so a volume that is mounted after the exporter starts works; a scrape before
then fails with a clear error.

## Which file is read

The file is `root` / target / `path`:

| Probe | File read |
| --- | --- |
| `/probe?collector=textfile` | `root/batch.prom` |
| `/probe?collector=textfile&target=nightly` | `root/nightly/batch.prom` |
| `/probe?collector=textfile&path=backup.prom` | `root/backup.prom` |

`target` is optional. It names a file or a directory under `root`, and may be
written relative to `root`, as an absolute path inside it, or as a `file://`
URL of one: `nightly`, `/var/lib/node_exporter/textfile_collector/nightly` and
`file:///var/lib/node_exporter/textfile_collector/nightly` are the same target.
With a target that names the file, `path` can be left out of the collector:

```yaml
  - name: any_textfile
    request:
      type: localfile
      root: /var/lib/node_exporter/textfile_collector
    transform:
      type: prometheus
```

```text
/probe?collector=any_textfile&target=backup.prom
```

A target, `path` or path parameter that would lead outside `root` — `..`, an
absolute path elsewhere, a symbolic link pointing out — is refused: a target
with `400 Bad Request` before anything is read, a `path` or path parameter as a
failed scrape. A path parameter fills exactly one file or directory name and
may not contain `/`.

The `/probe` parameters that apply are `path`, `timeout` and `param_<name>`.
Any parameter that belongs only to `http`, such as `method` or `header_<name>`,
is refused with `400`.

## Scraping with Prometheus

Prometheus asks the exporter; the file never needs a URL of its own. With a
fixed `path`, no target is needed at all:

```yaml
scrape_configs:
  - job_name: textfile
    metrics_path: /probe
    params:
      collector: [textfile]
    static_configs:
      - targets: ['exporter.example:8080']
```

To scrape several files through one collector, list them as targets and move
each into the `target` parameter, as for any probe:

```yaml
scrape_configs:
  - job_name: status_files
    metrics_path: /probe
    params:
      collector: [any_textfile]
    static_configs:
      - targets: [backup.prom, rotate.prom]
    relabel_configs:
      - source_labels: [__address__]
        target_label: __param_target
      - source_labels: [__param_target]
        target_label: instance
      - target_label: __address__
        replacement: exporter.example:8080
```

## Scheduled targets over OTLP

A [scheduled target](OTLP.md#scheduled-targets) of a `localfile` collector
reads the file on the exporter's own timer and sends the result over OTLP.
`target` is optional, and only `path` and `timeout` may be set under its
`request`:

```yaml
targets:
  - name: nightly_backup
    collector: any_textfile
    target: backup.prom
    labels:
      job: backup
  - name: batch
    collector: textfile
```

## Formats

With `response.format` left at `auto`, the file's extension chooses the
decoder, and anything else is recognised from its content:

| Extension | Decoded as |
| --- | --- |
| `.prom` | Prometheus text, as node_exporter's textfile collector expects |
| `.json` | JSON |
| `.yaml`, `.yml` | YAML |
| `.xml` | XML |
| `.csv` | CSV |
| `.html`, `.htm` | HTML |

`response.format` or `decoder.type` overrides the choice, as for `http`. A
`.prom` file with `transform.type: prometheus` is passed through, and
`include`, `exclude`, `rename` and `labels` of that transform apply as usual.

Transforms and Python scripts see the file as a response with status `200` and
three headers: `Content-Type` from the extension, `Content-Length`, and
`Last-Modified`, the file's modification time. A pre-script can use the last to
publish the file's age.

## What it takes from node_exporter

Reading files written by other programs has pitfalls that node_exporter's
textfile collector learned about first. The `localfile` type builds the lessons
in:

- **One directory, chosen by the operator.** A scrape can only read under
  `root`, whatever a probe asks for. The confinement is enforced by the
  operating system through Go's `os.Root`, so `..` and symbolic links cannot
  step outside it. A symbolic link is followed only when it is relative and
  stays inside `root`; an absolute link is refused even when it points inside,
  because where it leads depends on where `root` is mounted. Give each
  collector the narrowest directory that holds its files.
- **Only regular files.** A directory, device, socket or named pipe is refused
  without being read, and files are opened non-blocking, so a named pipe can
  never hang a scrape the way it would hang a plain `cat`.
- **No torn reads.** A writer that rewrites a file in place can be caught
  half-way. A file whose size or modification time changes while it is read is
  read again, and a scrape fails rather than export a mix of two versions if it
  changes a second time. Write the file atomically instead — to a temporary file
  in the same directory, renamed into place:

  ```sh
  generate_metrics > /var/lib/node_exporter/textfile_collector/batch.prom.$$ &&
    mv /var/lib/node_exporter/textfile_collector/batch.prom.$$ \
       /var/lib/node_exporter/textfile_collector/batch.prom
  ```

  The collector reads only the file it names, so the temporary file is never
  read half-written.
- **Stale files are visible.** A job that has stopped leaves its last values in
  place, and they would be exported forever. `max_age` turns that into a failed
  scrape — a `502` that Prometheus records as `up` 0 for the probe — and the
  modification time is also available to transforms.
- **Bounded reads.** A read stops at the collector's response limit, and a
  probe returns when its `timeout` or Prometheus's scrape timeout ends even if
  the file lives on a network filesystem that has stopped answering (see
  [Probe deadlines](CONFIGURATION.md#probe-deadlines)). A read cannot be
  cancelled, though, so the read itself goes on; at most four of a collector's
  reads may still be running at once. Once all four are held by reads that have
  not returned, a probe fails at once — `collector "textfile" already has 4 file
  reads that have not returned` — rather than adding another, and reads resume
  as the filesystem answers.
- **Least privilege.** The published image runs as the unprivileged user
  `exporter`, so the files must be readable by it; a file it may not read fails
  the scrape with `permission denied` rather than being skipped silently.

Unlike node_exporter, a probe reads one file, not a whole directory merged into
one exposition: merging files would hide which one a broken series came from,
and fail every file's metrics when one is malformed. Scrape each file as its own
target instead, as above.

## Errors and self-metrics

A failed read is reported in the `file` stage — `collector textfile file
failed: file /var/lib/.../batch.prom does not exist` — and follows
`error_handling.on_fetch_error`, the policy for failing to obtain a response, as
for an `http` collector. A
file over the size limit counts in `http_exporter_series_limit_exceeded_total`.
`http_exporter_scrape_http_status_code` reads `200` after a successful read.

With [verbose self-metrics](SELF-METRICS.md#verbose-per-request-self-metrics),
a file read is labelled with its `file://` URL, path parameters kept as their
placeholders, and `http_method="READ"`:

```text
http_exporter_scrapes_total{collector="textfile",http_method="READ",url="file:///var/lib/node_exporter/textfile_collector/batch.prom"} 12
```

## In Kubernetes

Mount the directory into the exporter's pod read-only with the chart's
`extraVolumes` and `extraVolumeMounts`. A volume shared with the job that
writes the files, such as a PersistentVolumeClaim, works wherever the pod runs;
a `hostPath` reads the directory of whichever node the pod is scheduled on, so
pin the pod to that node:

```yaml
extraVolumes:
  - name: textfile
    hostPath:
      path: /var/lib/node_exporter/textfile_collector
      type: Directory
extraVolumeMounts:
  - name: textfile
    mountPath: /var/lib/node_exporter/textfile_collector
    readOnly: true
```

The chart's read-only root filesystem is unaffected, since the exporter only
reads.
