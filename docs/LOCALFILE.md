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
| `max_response_bytes` | limit | The most that is read of one file; a larger file fails the scrape. |
| `files` | — | Read a whole directory instead of one file: the [file name patterns](#reading-a-directory) to read. Not with `path`. |
| `max_files` | `100` | With `files`: the most files one scrape reads. |
| `max_total_bytes` | `64MiB` | With `files`: the most one scrape reads across every file. A [size](CONFIGURATION.md#sizes), with or without a unit. |

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

## Reading a directory

With `files`, a collector reads every file of a directory whose name matches
one of the patterns, the way node_exporter's textfile collector reads its
directory:

```yaml
collectors:
  - name: textfiles
    request:
      type: localfile
      root: /var/lib/node_exporter/textfile_collector
      files: ["*.prom"]
      max_age: 1h
    transform:
      type: prometheus
```

Each file is decoded, transformed and checked on its own, as if it were the
only one, and its series get a `file` label with its name. Next to them the
answer carries, for every file read:

```text
backup_last_success_timestamp_seconds{file="backup.prom"} 1.7901e+09
jobs_total{file="batch.prom",queue="default"} 7
localfile_mtime_seconds{file="backup.prom"} 1.7901e+09
localfile_mtime_seconds{file="batch.prom"} 1.7901e+09
localfile_mtime_seconds{file="broken.prom"} 1.7901e+09
localfile_scrape_error{file="backup.prom"} 0
localfile_scrape_error{file="batch.prom"} 0
localfile_scrape_error{file="broken.prom"} 1
localfile_files_skipped 0
```

| Series | Meaning |
| --- | --- |
| `localfile_mtime_seconds{file}` | When the file was last modified, as node_exporter's `node_textfile_mtime_seconds`. Reported for every file whose modification time could be read, including one that failed. |
| `localfile_scrape_error{file}` | `1` when the file was left out — it could not be read, decoded or transformed, or it broke a rule below — and `0` otherwise. |
| `localfile_files_skipped` | Matching files not read because the directory had more than `max_files`. |

**One bad file does not sink the rest.** A file that fails at any stage is left
out alone: its series are missing, its `localfile_scrape_error` reads `1`, and
the reason is logged as a warning naming the file and the stage. Every other
file's series are answered, and the probe succeeds. The collector's
self-metrics count the failure as usual — a malformed file in
`http_exporter_parse_errors_total`, an oversized one in
`http_exporter_series_limit_exceeded_total`. That includes a metric rule with
`error_mode: fail`: it fails its file, not the probe. Alert on it:

```promql
localfile_scrape_error == 1
time() - localfile_mtime_seconds > 3600
```

A file is also left out when:

- a series of it already has a `file` label, which would collide with the one
  added, or is named like one of the series above;
- a metric of it has a different type than the same metric in a file read
  before it, since one answer declares a metric's type once. Files are read in
  name order, so the first by name wins.

A directory that is missing, or a target that names a file rather than a
directory, fails the probe in the `file` stage, following
`error_handling.on_fetch_error`. An empty directory, or one with nothing
matching, is an answer with no series but `localfile_files_skipped`.

### Which files

The patterns use [Go's `path.Match` syntax](https://pkg.go.dev/path#Match) —
`*`, `?`, `[a-z]` — and match names in the directory, not below it:
subdirectories are not read, and a pattern cannot contain `/`. A name starting
with a dot matches only a pattern that starts with one, as in a shell, so the
hidden temporary files many writers rename into place are not read half-written.
With `*.prom`, a temporary `batch.prom.$$` is not read either.

The directory is `root`, or the directory under it the probe's or scheduled
target's `target` names; `/probe?collector=textfiles&target=nightly` reads
`root/nightly`. There is no file to name, so the `path` and `param_<name>`
probe parameters, and `request.path` in a scheduled target, are refused.

Any file a collector can decode can be read this way, not only `.prom`. Each
file's decoder is chosen as for one file: from its extension with
`decoder.type: auto`, or the collector's `decoder.type`. The collector's one transform applies to every file, so a
directory read by one collector should hold files of one shape — `*.prom`
passed through, `*.json` status files with `jq`, `*.txt` or `*.log` lines with
`regex`:

```yaml
  - name: status_files
    request:
      type: localfile
      root: /var/lib/app/status
      files: ["*.json"]
    transform:
      type: jq
    metrics:
      - name: app_queue_depth
        type: gauge
        expression: .queue.depth
```

### Limits

A directory is easy to fill, so reading one is bounded:

- **`max_files`**, 100 by default. Files are taken in name order, and the rest
  are skipped, counted in `localfile_files_skipped` and logged as a warning
  naming how many matched and the first one skipped. They get no series of
  their own, so the `file` label never has more than `max_files` values.
- **The response limit for each file**, `max_response_bytes` or the
  collector's `limits.max_response_bytes`, 10 MiB by default. A larger file is
  refused from its size, before it is opened, so a 1 GiB file costs a `stat`,
  not a read.
- **`max_total_bytes`** across the files of one scrape, 64 MiB by default. A
  file that would go past it is refused in the same way, and later, smaller
  files are still read.

- **The listing**, at most ten times `max_files` entries, and at least 1000,
  whatever they are. A directory with more is listed only that far, in the
  order the filesystem returns entries, and a warning says so; the files
  considered are those found by then. Keep other files out of the directory,
  or raise `max_files`.

Which files are read is decided from their sizes, in name order, before any is
read. A file that grows after that, past what `max_total_bytes` leaves it, fails
alone rather than taking the scrape past the total.

A refused file is a failed file: it is logged, its `localfile_scrape_error`
reads `1` and its `localfile_mtime_seconds` is reported. `max_age` applies to
each file, and `limits.max_metrics` to the whole answer.

Files are read four at a time, and answered in name order whichever finished
first. Reading the directory is one read for the
[bounds on reads](#what-it-takes-from-node_exporter): it takes one of the
collector's four pending-read slots and ends with the probe's `timeout` or
deadline. **A deadline does not lose what was read.** The files read by then
are answered; a file still being read, or not reached, fails alone —
`localfile_scrape_error` `1`, its mtime reported when it was taken, and a log
line saying it was not read before the deadline. No further file is started.
Only a directory that could not even be listed in time fails the probe.

## Formats

With `decoder.type` unset or `auto`, the file's extension chooses the
decoder, and anything else is recognised from its content. Unset, and not
implied by the transform, it is logged as a configuration warning at startup:
set it to the decoder the files need, or to `auto` to keep choosing by
extension.

| Extension | Decoded as |
| --- | --- |
| `.prom` | Prometheus text, as node_exporter's textfile collector expects |
| `.json` | JSON |
| `.yaml`, `.yml` | YAML |
| `.xml` | XML |
| `.csv` | CSV |
| `.html`, `.htm` | HTML |

`decoder.type` overrides the choice, as for `http`. A file
declares no encoding: one in anything but UTF-8 needs `response.charset`, such
as `windows-1252`, unless it starts with a byte order mark (see
[Character encodings](CONFIGURATION.md#character-encodings)). A
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

A collector with `path` reads one file per probe; one with `files` reads a
[whole directory](#reading-a-directory) as node_exporter does, each file checked
on its own and labelled with its name, so a broken file is named and fails
alone.

## Errors and self-metrics

A failed read is reported in the `file` stage — `collector textfile file
failed: file /var/lib/.../batch.prom does not exist` — and follows
`error_handling.on_fetch_error`, the policy for failing to obtain a response, as
for an `http` collector. A
file over the size limit counts in `http_exporter_series_limit_exceeded_total`.
`http_exporter_scrape_http_status_code` reads `200` after a successful read.

With [verbose self-metrics](SELF-METRICS.md#verbose-per-request-self-metrics),
a file read is labelled with its `file://` URL, path parameters kept as their
placeholders, and `http_method="READ"`; a directory read with the directory's
URL, ending in `/`:

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
