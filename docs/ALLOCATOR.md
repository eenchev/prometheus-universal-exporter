# Target allocator

`pue-allocator` is a small server that runs beside several exporter replicas
and decides which replica does what. It serves every replica started in
allocator mode (`--allocator.url`) the same configuration and its own share of
the static targets, as one versioned bundle, and it serves Prometheus the
probe targets through HTTP service discovery, each already pointed at a
replica. Every replica holds the whole configuration, so any replica can
answer any probe: routing a probe to "its" replica only saves connection
pools and cache.

The allocator only decides. Every scrape and every probe still goes straight
between Prometheus, the replicas and the targets, and when the allocator is
down the replicas keep their last bundle and Prometheus its last service
discovery answer.

It keeps nothing on disk. The same input files and the same heartbeats give
the same bundles and the same answers, so a restarted allocator, or a second
copy beside the first, answers as the first did.

## Running it

The allocator is built beside the exporter: `make build` leaves it in
`bin/pue-allocator`, the release archives carry it, and the container image
has it at `/bin/pue-allocator`, beside the exporter (run the image with
`--entrypoint /bin/pue-allocator`, or the pod with `command:
["/bin/pue-allocator"]`). It is built with the same request types as the
exporter, since it checks the configuration it serves with the exporter's
loader: an allocator built with only some request types refuses a
configuration that uses another, as such an exporter does.

```sh
pue-allocator \
  --config.file=/etc/pue/config.yaml \
  --static-targets-file=/etc/pue/static-targets.yaml \
  --probe-targets-file=/etc/pue/probe-targets.yaml
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--web.listen-address` | `:9116` | Where the API, `/metrics`, `/healthz` and `/ready` are served, over plain HTTP. |
| `--config.file` | `/etc/prometheus-universal-exporter/config.yaml` | The configuration the replicas are served, with the collector files it lists. |
| `--static-targets-file` | none | The static target file, split among the replicas. |
| `--probe-targets-file` | none | The probe targets file, served to Prometheus at `/v1/sd/<job>`. |
| `--web.token-file` | none | A file holding a bearer token every request but `/healthz` and `/ready` must carry, read again when it changes, so a rotated Secret takes effect without a restart. |
| `--config.watch-interval` | `1m` | How often the input files are read again for changes; `0` reads them once. |
| `--settle` | `15s` | How long a changed member list must hold before targets move. |
| `--members.max` | `1000` | How many live members the allocator takes; a heartbeat of a new member beyond them is answered `429`. |
| `--log.level` | `info` | `debug`, `info`, `warn` or `error`. Every line is JSON, as the exporter's. |
| `--version` | | Print the version and the request types of the build, and exit. |

Input files that are refused at the start stop the allocator with exit
status 1, and a malformed command line with 2. `SIGTERM` or `SIGINT` shuts it
down: `/ready` answers `503`, every long poll is answered at once, and the
server stops within ten seconds.

## Inputs

The allocator reads three files and checks them as the exporter checks its
own, with the exporter's loader, before it serves anything made from them.
They are read again every `--config.watch-interval`; a change is put in
force only when all three are accepted together, and files that are refused
leave the last good ones in force, logged and counted once for as long as
the refusal stays the same (`pue_allocator_reloads_total{result="failure"}`).
The files are read before and after they are checked, and a change caught in
between is read again at the next check, so what the replicas are served is
exactly what was checked.

### The configuration

`--config.file` is a configuration in the exporter's own format
([Configuration](CONFIGURATION.md)), with its collectors, collector files,
limits, OTLP and every other setting a configuration holds. The replicas
are served it, and its collector files, as written. Beyond the exporter's
own checks:

- Each `collector_files` entry must be a relative path, or pattern, inside the
  configuration's directory: the bundle carries a collector file at its path
  relative to that directory, and the replica writes the bundle into a
  directory of its own. An absolute entry, or one that leaves the directory,
  is refused.
- No collector file may land on the bundle's own files, `config.yaml` and
  `static-targets.yaml`, and no pattern may match `static-targets.yaml`
  (`*.yaml` beside the configuration would): on the replica the share of the
  static targets lies beside the configuration. Keep collector files in a
  directory of their own, such as `collectors/*.yaml`.
- Names beginning with `..` are the replica's cache directory's own
  (`..data`, `..gen-<generation>`): a collector file whose path begins with
  one is refused, and so is an entry of a directory and more whose directory
  could match one — it begins with `..`, with a pattern character, or with a
  dot and one, as `*/x.yaml` does — since on the replica it would match
  `..data/config.yaml` too. Begin such an entry with a directory's name.
- Files the configuration names by path — a grpc collector's descriptor
  files, `otlp.tls` files, `web.basic_auth` files, credential files — travel
  as paths, never as content. They must be mounted on every replica at the
  same path, as without the allocator, and need not be mounted on the
  allocator: it checks the configuration without opening them. The checks
  that read them are the replicas' when they load the bundle — a missing or
  unreadable file, a descriptor set without the collector's method, a
  message that does not fit it — and a replica that refuses a bundle for
  one keeps its last good bundle and reports the refusal in its heartbeats,
  which the allocator logs. So a grpc collector with `descriptors: protoset`
  or `proto` is checked by the allocator for everything but its method and
  messages, and so is a static target's `message` for it; the built-in
  health service is checked in full.
- The allocator expands no `${NAME}` reference: it checks the files as
  written and serves them so, and a replica started with
  `--config.expand-env` expands them from its own environment. A reference
  in a value the loader checks, a number say, is therefore refused by the
  allocator; write such values out.
- Python scripts are not run by the allocator; each replica checks its
  collectors' scripts when it loads the bundle, and reports a bundle it
  refuses in its heartbeats, which the allocator logs once.

### Static targets

`--static-targets-file` is a [static target file](STATIC-TARGETS.md) as the
exporter reads it, checked against the configuration. A target may set
`shard_key`: the allocator then allocates it by that key in place of its
name, so every target of one key lands on one replica and shares its pools.

```yaml
interval: 1m
targets:
  - name: db1_queries
    collector: pg_queries
    target: db1.internal:5432
    shard_key: db1
  - name: db1_locks
    collector: pg_locks
    target: db1.internal:5432
    shard_key: db1
```

A replica's share is a static target file of its own: the file's top-level
keys (`interval`, `concurrency`) as written, and the targets of the share in
the file's order, each with its name written out, so a target left to its
default name, `<collector>_<index>`, keeps the name the whole file gives it.
Aliases and merge keys are written out as what they stand for, and the
file's `x-` keys stay behind.

### Probe targets

`--probe-targets-file` lists probe jobs: targets probed with one collector,
with the parameters and labels Prometheus should send and attach.

```yaml
probe_jobs:
  - name: websites
    collector: html_status
    params:              # sent as query parameters on every probe
      timeout: 10s
    labels:
      team: web
    targets:
      - https://shop.example
      - https://docs.example
  - name: databases
    collector: pg_health
    shard_key: address   # keep every probe of one server on one replica
    targets:
      - db1.example:5432
      - db2.example:5432
```

- `name` is required and unique, letters, digits, `_`, `.` and `-`: it is the
  job's path, `/v1/sd/<name>`.
- `collector` must be a collector of the configuration.
- `targets` must not be empty, each listed once, and each is checked as a
  static target's `target` is for the collector's request type: an `http`
  target is an absolute URL with a port that can be used.
- `params` are probe parameters (`timeout`, `path`, `param_<name>` and the
  others the collector's request type takes), checked as a probe checks
  them; a parameter of another request type, a malformed one, `target`,
  `collector` and `debug` are refused, and a name must make a label name.
- `labels` go on every target of the job; one written `""` is left out.
  `instance`, which the allocator sets, and names starting with `__` are
  refused.
- `shard_key` is `target`, the default, which allocates each probe by its
  target as written, or `address`, which allocates it by the host and port it
  reaches: a URL's host and port, with `:80` or `:443` filled in for an `http`
  or `https` URL that names none, compared without case, or anything else as
  written.

The file may keep YAML anchors under top-level `x-` keys, as the exporter's
files may.

## Members and allocation

A replica in allocator mode heartbeats to the allocator every 10s with its
id, the address Prometheus should send its probes to, and the generation of
the bundle it has in force. The replicas heard from within the last 30s, and
not shutting down, are the live members; a replica that says it is shutting
down stops being live at once, so no probe is routed to it any more, and
leaves the member list as any other change does, after `--settle`.

A change to the live members takes effect once it has held for `--settle`:
a replica that restarts and is back within it keeps its share, and nothing
moves. The first member list forms once the live members have not changed
for `--settle`, each replica heard from for the first time starting the wait
again, and no sooner than `--settle` or a heartbeat interval, whichever is
longer, after the allocator started, so a restarted allocator has heard from
every replica before it hands out shares. Until then the bundle and service
discovery endpoints answer `503`, and replicas and Prometheus keep what they
have.

The allocator takes `--members.max` live members, 1000 by default. A heartbeat
names any member it likes, so without a bound every new name would add one;
beyond it the heartbeat of a member the allocator does not know is answered
`429`, with `Retry-After: 30`, logged once while the allocator stays full, and
counted in `pue_allocator_members_refused_total`. The members it has keep
heartbeating, and a replica whose heartbeats stop frees its place when its
30s run out, though it stays in the member list until `--settle` after that.

Every target, static or probe, belongs to the member with the highest
rendezvous score, the first 8 bytes of SHA-256 of the member's id, a NUL and
the target's shard key. Adding a replica moves only the targets it now wins,
about one in N+1; removing one moves only its own.

**Handover.** A static target that moves from a replica still in the member
list stays in that replica's share for one of the target's intervals, its own
or the file's, while its new owner already scrapes it, so its series has no
gap; then the old owner gives it up. A target whose old owner left has
nobody to drain it.

A probe target moves when Prometheus next refreshes the job, and keeps its
series, since its `instance` is its target. A probe is routed among the
members of the member list that are live (all live members if none is), and
among those only to the members whose heartbeat reports a bundle whose
configuration has the job's collector, when any does: a collector just added
draws no `unknown collector` answers while the new bundle spreads.

## API

Plain HTTP with JSON. With `--web.token-file`, every endpoint but `/healthz`
and `/ready` answers `401` to a request without `Authorization: Bearer
<token>`. The file is read again when its modification time or size
changes, so a rotated token is the one required from the next request. A
file that is empty or missing at the start stops the allocator; one that
becomes empty or unreadable later refuses every request with `401` until it
holds a token again, logged once.

| Endpoint | Caller | Answer |
| --- | --- | --- |
| `POST /v1/heartbeat` | replica | The generation of the bundle the replica is to have. |
| `GET /v1/bundle?member=<id>` | replica | The replica's bundle, with long polling. |
| `GET /v1/sd/<job>` | Prometheus | The job's probe targets in the HTTP service discovery format. |
| `GET /metrics` | Prometheus | The allocator's own metrics. |
| `GET /healthz` | kubelet | `200` while the process serves. |
| `GET /ready` | kubelet | `200` once the input files are loaded, which they are from the start; `503` while shutting down. |

### Heartbeats

```json
{"member": "exporter-0", "advertise": "10.0.3.7:9115", "generation": "8117cfab…",
 "shutting_down": false, "refused": {"generation": "…", "error": "…"}}
```

`member` and `advertise`, a `host:port` with a host and a decimal port from 1
to 65535, are required; anything else is answered `400`. A body over 64 KiB
is answered `413`, and the heartbeat of a new member beyond `--members.max`
`429`. The answer is `{"generation": "…"}`, the generation of the
member's bundle, or `""` while the member list has not formed. A replica
whose bundle differs fetches it.

### Bundles

A bundle is a tree of files, keyed by path:

```json
{"generation": "8117cfab0434defd499420fa39cba0e2",
 "files": {"config.yaml": "…", "collectors/team-a.yaml": "…", "static-targets.yaml": "…"}}
```

- `config.yaml` is the configuration, and each collector file is at its path
  relative to the configuration's directory, both as written.
- `static-targets.yaml` is the member's share, draining targets included.
  It is left out when the share is empty (a static target file must hold a
  target), and so for a replica that is not yet in the member list, which
  gets the configuration alone and can answer probes while it waits.
- `generation` is the digest of the files: the same files give the same
  generation, on every allocator.

The answer carries the generation as its `ETag`, quoted. A request whose
`If-None-Match` names the generation the replica has is answered `304` when
the bundle is still that one. With `wait=<duration>`, such a request waits,
for at most `60s` however long it asks, until the bundle changes and is then
answered at once with the new one, or with `304` when the wait runs out or
the allocator shuts down. Until the member list has formed a bundle request
is answered `503`, with `Retry-After`: at once without a wait, and with one
when the wait runs out, unless the list forms first and the bundle is the
answer. A replica
takes the `503` as no failure: it keeps its bundle and asks again after
`Retry-After`. Its `If-None-Match` is the generation, quoted, as the `ETag`
is; a bare one is taken too.

### Service discovery

`GET /v1/sd/websites` for the job above:

```json
[
  {"targets": ["10.0.3.7:9115"],
   "labels": {"__metrics_path__": "/probe",
              "__param_collector": "html_status",
              "__param_target": "https://shop.example",
              "__param_timeout": "10s",
              "__meta_pue_allocator_member": "exporter-0",
              "instance": "https://shop.example",
              "team": "web"}}
]
```

One group per target, at the address its replica advertises. An unknown job
is answered `404`; `503` while the member list has not formed or no replica
is live, so Prometheus keeps its last good targets rather than dropping
them. The answer sets no `job` label: as with any service discovery, the job
is the scrape configuration's `job_name`, so name it after the probe job.
`__meta_pue_allocator_member` names the replica, for relabeling; Prometheus
drops it after relabeling, so a probe that moves keeps its series.

```yaml
scrape_configs:
  - job_name: websites
    http_sd_configs:
      - url: http://pue-allocator.internal:9116/v1/sd/websites
        refresh_interval: 30s
        authorization:
          credentials_file: /etc/prometheus/pue-allocator-token
```

## Metrics

| Metric | Type | Meaning |
| --- | --- | --- |
| `pue_allocator_members` | gauge | Members of the member list allocation uses. |
| `pue_allocator_targets{kind}` | gauge | Targets of the input files in force, `static` or `probe`. |
| `pue_allocator_assigned{member,kind}` | gauge | Static targets each member owns, without those it drains, and probe targets routed to it. |
| `pue_allocator_generation_info{member,generation}` | gauge | `1`, with the generation of each member's bundle. |
| `pue_allocator_reloads_total{result}` | counter | Changed input files put in force (`success`) or refused (`failure`). |
| `pue_allocator_member_changes_total` | counter | Changes of the member list after the first. |
| `pue_allocator_members_refused_total` | counter | Heartbeats of new members refused because `--members.max` members were live. |

## On Kubernetes

The Helm chart deploys the allocator with `allocator.enabled`, beside the
exporter, and switches the replicas to allocator mode; see
[Target allocator](../charts/prometheus-universal-exporter/README.md#target-allocator)
in the chart README. It runs `allocator.replicas` allocators, two by default,
from the exporter's image, behind a headless Service on port 9116: they need
no leader, since each hears every heartbeat, a replica heartbeating to every
address the Service's name resolves to. The configuration (`config.data`)
and `allocator.staticTargets.data` and `allocator.probeJobs`, the static
targets and the probe jobs, are mounted into the allocator only; a change to
them rolls the allocator, and reaches the replicas as a new bundle without a
restart. The replicas get `--allocator.url`, their pod's name as the member
and their pod's IP as the address to advertise, keep their bundle in an
`emptyDir`, and keep the credential Secrets mounted, which the allocator does
not need. The `emptyDir` survives a container restart but not the pod's
replacement: a new pod starts with no bundle, not ready, and waits for the
allocator. A restarted container starts with the bundle its exporter
installed there, and with nothing else: a `config.yaml` put into the
directory by other means is removed. Each probe job
becomes a Prometheus Operator `ScrapeConfig` reading `/v1/sd/<job>`.

## Without Kubernetes

Run `pue-allocator` on one or two hosts, start each exporter with
`--allocator.url=http://allocator.internal:9116`, and point Prometheus's
`http_sd_configs` at the same address, one scrape configuration per probe
job. systemd units, Compose and plain VMs need nothing else.
