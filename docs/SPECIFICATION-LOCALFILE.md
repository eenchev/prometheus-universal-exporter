# Prometheus Universal Exporter — `localfile` Request Type Specification

This is the `localfile` request type's section of § 5.1 of the
[exporter specification](SPECIFICATION-EXPORTER.md), in a document of its
own so that a change to the type is a change to this document. Everything
the request types share — the registry and the rules every type is held to
(§ 5.1), building without a type, decoding, transforms, error policies,
limits, caching, static targets and exposition — is in the exporter
specification, and a section number here, such as § 42.14, is one of its
sections. What a user reads of the type is [LOCALFILE.md](LOCALFILE.md).

## `localfile`

A `localfile` collector reads a file from the exporter's own filesystem. It
exists for what writes metrics or a status to disk rather than serving it —
above all the Prometheus text files batch jobs leave for node_exporter's
textfile collector — and follows that collector's practice.

| Key | Default | Rule |
| --- | --- | --- |
| `root` | none | **Required.** An absolute directory, stored cleaned. The filesystem root MUST be refused. Its existence is not checked at load, so a volume mounted later works. |
| `path` | none | Relative to `root` and the target; MUST NOT be absolute or lead outside with `..`. May carry path parameters (§ 42.10a). |
| `max_age` | off | Non-negative duration. A file last modified longer ago MUST fail the scrape, naming its age and the limit. |
| `max_response_bytes` | limit | As for `http`; with `files`, the limit of each file. |
| `files` | none | A list of file name patterns, `path.Match` syntax. Setting it makes the collector read a directory (§ 5.1a). MUST NOT be combined with `path`; a pattern MUST NOT be empty, contain `/`, `\` or NUL, or be malformed. |
| `max_files` | 100 | With `files` only; not negative. |
| `max_total_bytes` | 64 MiB | With `files` only; not negative. A size (§ 5.0a). |

`max_files` or `max_total_bytes` without `files` MUST be rejected, naming the
key, the first in the file when both are set.

The file read MUST be `root` / target / `path`:

- `target` MAY be left out, by a probe and by a static target. When given it
  MUST be a path relative to `root`, an absolute path inside `root`, or a
  `file://` URL of one, and a target leading outside `root` MUST be refused —
  a probe with `400` before anything is read, a static target at load. A
  `file://` URL MUST carry an absolute path, and its path MUST be
  percent-decoded before it is read and before it is held to these rules, so
  `file:///srv/a%20b.prom` names the file `a b.prom` and a `%` of a file's
  own name is written `%25`; a `%` that does not begin an escape MUST be
  refused the same way, saying so. The two plain forms are paths, not URLs,
  and MUST be taken as written.
- A `path` probe parameter replaces `request.path` and is held to the same
  rules. A path parameter value MUST be a single file or directory name: `/`,
  `\`, NUL, `.` and `..` MUST be refused, in a probe's value as a failed
  scrape and in a placeholder's default at load (§ 42.10a).
- When neither names a file the scrape MUST fail saying so.

Every read MUST be confined to `root` by the operating system (Go's
`os.Root`), so that neither `..` nor a symbolic link can reach outside it
whatever the probe asks for. A symbolic link MAY be followed only while it
stays inside `root`; an absolute link MUST be refused. Only regular files MUST
be read: a directory, device, socket or named pipe MUST be refused before it is
read, and files MUST be opened non-blocking so a named pipe cannot hang a
scrape. The read MUST stop at the collector's response limit and fail beyond
it, counted as a limit error. A file whose size or modification time changes
while it is read MUST be read again, and the scrape MUST fail, advising an
atomic rename, if it changes a second time. A read MUST NOT hold a probe past
its `timeout`, budget (§ 3.2a) or context, even when the filesystem does not
answer. Because a read cannot be cancelled, the reads of one collector that are
still in progress after their probe has given up on them — abandoned reads —
MUST be capped, at four. A probe that finds the cap reached MUST fail at once,
without starting a read, saying the filesystem is not answering, and an
abandoned read MUST stop counting when it returns. A read whose probe is still
waiting for it MUST NOT count: probes of a filesystem that answers MUST NOT be
refused for being many, whatever their number. The reads in progress MAY
therefore pass the cap by the probes that were waiting when the filesystem
stopped answering, which `limits.max_concurrent_probes` bounds.

A successful read MUST be presented to the shared pipeline as a response with
status `200` and the headers `Content-Type`, chosen from the extension
(`.prom` as Prometheus text version 0.0.4, `.json`, `.yaml`/`.yml`, `.xml`,
`.csv`, `.html`/`.htm`) so `decoder.type: auto` picks the decoder,
`Content-Length`, and `Last-Modified`, the file's modification time. A failed
read MUST be reported in the `file` stage and follow `on_fetch_error`. A file
that does not exist, and one the exporter may not read, MUST each be named as
such.

It MUST accept these `/probe` parameters: `path`, `timeout` and
`param_<name>`. A static target using a `localfile` collector MAY set
`path` and `timeout`. Its verbose self-metrics MUST carry the file's `file://`
URL, its path escaped as a URL's path is (a space as `%20`, a `?` as `%3F`, a
`#` as `%23`), with path parameters as their placeholders, as `url`, and a
debug report's request line MUST show that URL; `READ` MUST be
`http_method`, so they are never mistaken for the per-collector series, which
carry no method.

A collector with `path`, or none, reads one file per probe. Merging files MUST
happen only as § 5.1a specifies, where each file is checked on its own and
named, never as one concatenated exposition, which would hide which file a
broken series came from and fail every file's metrics when one is malformed.

### 5.1a Reading a directory

A `localfile` collector with `files` MUST read the directory `root` / target —
`root` when the target is left out — and every entry directly in it whose name
matches one of the patterns. Subdirectories MUST NOT be read, and a name
starting with `.` MUST match only a pattern starting with `.`. A missing
directory, or a target naming something other than a directory, MUST fail the
scrape in the `file` stage under `on_fetch_error`. There is no file to name, so
the `path` and `param_<name>` probe parameters MUST be refused with `400`, and
`request.path` in a static target at load, each saying why. A collector whose
label values hold placeholders (§ 42.10c) has those for a `param_<name>` to
fill: its probes MUST be held to the placeholders' own rule instead — a
parameter a label names accepted, for the series of every file, and any other
refused with `400` as unused — and `path` MUST stay refused.

The directory MUST be listed in batches, and the listing MUST stop after ten
times `max_files` entries, and at least 1000, whatever they are, logging a
warning with how many were listed; only the files found by then are
considered.

Matching files MUST be taken in name order, at most `max_files` of them; the
rest MUST NOT be read or looked at, and MUST be counted and logged as a warning
with how many matched and the first skipped. Every file taken MUST be read by
the rules for one file above — confined to `root`, only regular files,
non-blocking, re-read when it changes under the read — and additionally:

- Which files are read MUST be decided from their sizes, in name order, before
  any is read, so the choice does not depend on which read finishes first.
- A file larger than the response limit MUST be refused from its size, before
  it is opened.
- A file that would take the bytes read in this scrape past `max_total_bytes`
  MUST be refused in the same way; later files that fit MUST still be read.
- What `max_total_bytes` leaves after the files chosen MUST be shared among
  them as room to grow, and a file that grows past its size plus its share
  before it is read MUST fail alone, so a scrape never reads more than
  `max_total_bytes`.
- `max_age` MUST apply to each file.

The files chosen MUST be read four at a time, and answered in name order
whichever finished first.

The whole directory read MUST be one read for the cap on abandoned reads,
counted only when its probe has given up on it, and MUST end with the probe's
`timeout`, budget or context. When it ends before the read is done, the files
read by then MUST be answered, and each file still being read or not yet
reached MUST fail alone with an error saying it was not read before the
deadline, its modification time reported when it had been taken; no further
file MUST be started. Only a directory not listed in time MUST fail the probe.
An answer cut short this way MUST NOT be cached, since the cache would go on
serving the files not reached as failed. When the exporter's shutdown cut it
short, on a static target, it MUST be dropped like any static target scrape the
shutdown aborts: nothing published and no failure logged.

Each file read MUST then be decoded, transformed and validated on its own, as a
response of its own with the headers a single file gets, so its decoder follows
its extension under `decoder.type: auto`. Its series MUST be given a label
`file` with its name. A file MUST be left out, alone, when it could not be read
or was refused, when decoding, the transform or validation fails — including a
metric rule with `error_mode: fail` — when a series of it already has a `file`
label or bears one of the names below, or when one of its metrics has another
type than the same metric in a file taken before it. A left-out file MUST NOT
fail the probe: it MUST be logged as a warning with the file, the stage and the
error, and counted in the collector's self-metrics as the stage's failure
would be, and every other file's series MUST be answered. The series of a
family MUST stay one contiguous block, declared once.

The answer MUST also carry, as gauges with help text:

```text
localfile_mtime_seconds{file}   modification time, for every file whose time was read
localfile_scrape_error{file}    1 for a left-out file, 0 for every other file taken
localfile_files_skipped         the files beyond max_files
```

A file beyond `max_files` MUST NOT get a series of its own, so the `file`
label never has more than `max_files` values. `limits.max_metrics` MUST apply to the whole answer. The verbose
`url` label MUST be the directory's `file://` URL ending in `/`. Static
targets MUST read directories the same way.
