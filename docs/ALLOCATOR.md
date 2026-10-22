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
copy beside the first, answers as the first did. That holds while the inputs
are files alone: a [discovery source](#discovering-targets) is asked by each
copy on its own timer, and two copies can find different targets for a
refresh interval or, when one cannot reach the source, for as long as that
lasts, so an allocator that discovers runs as one copy
([One copy with discovery](#one-copy-with-discovery)).

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
| `--discovery.max-targets` | `10000` | How many targets one answer of a [discovery source](#discovering-targets) may hold, for a source that sets no `max_targets`; an answer with more is refused. |
| `--log.level` | `info` | `debug`, `info`, `warn` or `error`. Every line is JSON, as the exporter's. |
| `--version` | | Print the version and the request types of the build, and exit. |
| `--static-targets-file-schema` | | Print the JSON Schema of the static target file, with `discovered_targets`, for editors, and exit. |
| `--probe-targets-file-schema` | | Print the JSON Schema of the probe targets file, for editors, and exit. |

Input files that are refused at the start stop the allocator with exit
status 1, and a malformed command line with 2. Files that change while the
start reads them, as a ConfigMap being swapped does, are read again a second
later, up to five times in all, before the start gives up with exit status 1. `SIGTERM` or `SIGINT` shuts it
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
  `--config.expand-env` expands the configuration and its collector files
  from its own environment, and one started with
  `--static-targets.expand-env` its share of the static targets. A reference
  in a value the loader checks, a number say, or a static target's scheme or
  host, is therefore refused by the allocator; write such values out.
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

Beside `targets`, the allocator's static target file may have
`discovered_targets`, entries written once for every target a discovery
source finds ([Discovering targets](#discovering-targets)); `targets` may
then be left out.

The exporter's schema of the file, `configs/static-targets.schema.json`,
refuses `discovered_targets`, as the exporter does. The allocator's file has
a schema of its own, [`configs/allocator-static-targets.schema.json`](../configs/allocator-static-targets.schema.json),
printed by `pue-allocator --static-targets-file-schema`: the exporter's,
with `discovered_targets`. Start the file with

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/eenchev/prometheus-universal-exporter/main/configs/allocator-static-targets.schema.json
```

and an editor with the YAML language server completes and checks it as it
does the exporter's files ([Editor support](CONFIGURATION.md#editor-support)).

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
- `targets` must not be empty, unless the job has `discovery`, each listed
  once, and each is checked as a static target's `target` is for the
  collector's request type: an `http` target is an absolute URL with a port
  that can be used.
- `discovery` lists sources whose targets are probed besides `targets`, and
  `target_template` makes a target of each address they find
  ([Discovering targets](#discovering-targets)).
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
files may. Its schema, [`configs/probe-targets.schema.json`](../configs/probe-targets.schema.json),
printed by `pue-allocator --probe-targets-file-schema`, is for editors:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/eenchev/prometheus-universal-exporter/main/configs/probe-targets.schema.json
```

Both of the allocator's schemas are generated from the structs it decodes,
as the exporter's are, and regenerated with `make schemas`; the allocator's
checks at load remain the last word, checking too what no schema can, such
as that a collector is one of the configuration's.

## Discovering targets

A large fleet need not be listed by hand. A probe job, or an entry of the
static target file's `discovered_targets`, may name discovery sources, and
the targets they find are allocated as written ones are: by the same
rendezvous hashing, into the same bundles and service discovery answers.

```yaml
probe_jobs:
  - name: websites
    collector: html_status
    labels:
      team: web
    targets:
      - https://shop.example         # written targets stay
    target_template: https://{{address}}/health
    discovery:
      - name: cmdb
        http_sd:
          url: https://cmdb.internal/prometheus/websites
          bearer_token_file: /etc/pue/cmdb-token
      - file_sd:
          files: [sd/websites-*.json]
```

```yaml
interval: 1m
targets:
  - name: legacy
    collector: node
    target: http://legacy.internal:9100/metrics
discovered_targets:
  - name: node                       # targets are named node_<address>
    collector: node
    target_template: http://{{address}}/metrics
    interval: 30s
    shard_key: address               # optional: allocate by the address
    labels:
      team: infra
    discovery:
      - kubernetes:
          namespaces: [monitoring]
          label_selector: app.kubernetes.io/name=node-exporter
          port: metrics
        labels:
          node: __meta_kubernetes_node_name
```

A `discovered_targets` entry is a static target without `target` and with
`name`, `discovery`, and optionally `target_template` and `shard_key`; every
other key (`collector`, `interval`, `request`, `labels`, `params`,
`export_via_otlp`, `otlp`) is the static target's own, written into every
target the entry makes. It is checked at load as such a target, with the
address `192.0.2.1:9100` in place of a discovered one, so an `http` collector
without a `target_template` making a URL is refused then. Each target it
makes is named `<name>_<address>`, the address escaped so that no two
addresses share a name: a letter or digit stays, `.` is written `_`, and
`:`, `-`, `[` and `]` are written `_3A`, `_2D`, `_5B` and `_5D`, `_` and
their byte in upper-case hex. So `10.0.0.5:9100` makes `node_10_0_0_5_3A9100`,
`node-1.example.com:9100` makes `node_node_2D1_example_com_3A9100`, and
`[fd00::1]:9100` makes `node__5Bfd00_3A_3A1_5D_3A9100`. An address of any
other character, an upper-case letter or `_` say, or one whose escaped form
would be longer than 128 characters, is written instead as its first 48
characters, lower-cased and every one but a letter or digit written `_`,
then `_X` and the first 16 hex digits of its SHA-256:
`Web_1.example:80` makes `node_web_1_example_80_X` and 16 hex digits. A
target whose name the file already gives a written target takes that second
form too. A name depends on the entry, the address and the file alone, never
on what else discovery finds, so a target keeps its name, and its
`static_target` label and its series, from one discovery to the next. For
the same reason no entry's `name` may be another entry's followed by `_`
(`node` and `node_eu`, say), which is refused at load. `shard_key` may only be `address`, which
allocates each target by its address, so the targets of several entries at
one address share a replica; left out, a target is allocated by its name.
The exporter refuses `discovered_targets` ([Static targets](STATIC-TARGETS.md)):
only the allocator discovers.

`target_template` makes the target from the address a source found,
`{{address}}` standing for it; without one the address is the target. A
discovered target is checked as a written one is; one that is refused, or
that is listed already, is left out, and what is left out is logged once
while it stays the same.

### Sources

Each source sets exactly one of `file_sd`, `http_sd` and `kubernetes`, and
may set:

- `name`, letters, digits, `_`, `.` and `-`, unique in its list; by default
  its kind and its place, such as `file_sd_1`. It names the source in the
  metrics and the logs.
- `labels`, labels set from the source's own: `zone: __meta_zone` gives each
  target the label `zone` with the value of its `__meta_zone`. A name
  starting with `__`, `job`, `instance` and `static_target` are refused.
- `max_targets`, how many targets one answer may hold, `--discovery.max-targets`
  by default.

**`file_sd`** reads Prometheus file_sd files, JSON (`.json`) or YAML (`.yml`,
`.yaml`) lists of `{targets: [...], labels: {...}}`, from `files`, paths or
patterns relative to the directory of the file that names them. They are
read again every `refresh_interval`, `1m` by default.

**`http_sd`** asks `url`, an `http` or `https` URL without credentials, every
`refresh_interval`, `1m` by default, for the same JSON list, as Prometheus's
HTTP service discovery does, sending `X-Prometheus-Refresh-Interval-Seconds`.
`timeout`, no longer than the refresh interval, bounds the request; left
out, it is `10s`, or the refresh interval when that is shorter; `bearer_token_file` holds a token sent as `Authorization:
Bearer`, read again at every refresh; `tls` is the exporter's `tls` block
(`ca_file`, `cert_file`, `key_file`, `server_name`, `insecure_skip_verify`).
Redirects are not followed, and an answer that is not `200` fails.

**`kubernetes`** lists the `discovery.k8s.io/v1` EndpointSlices of the
cluster the allocator runs in, every `refresh_interval`, `30s` by default,
with plain HTTPS requests to the API server: at `KUBERNETES_SERVICE_HOST` and
`KUBERNETES_SERVICE_PORT`, verified with the service account's `ca.crt`, and
with its token, read again at every listing so a rotated token is used. It
polls rather than watches, which keeps it small and is enough for targets
that move at the pace of a rollout. `namespaces` lists the namespaces to
list, by default the allocator's own; `all_namespaces: true` lists them all,
which needs a ClusterRole. `label_selector` is a Kubernetes label selector,
checked at load. `port` is the endpoint port the targets are at, by name, as
the Service names it, or by number. Each ready endpoint's address at that
port is a target, and with `include_not_ready: true` every endpoint's;
a slice without the port has none. `address_type` is the `addressType` of
the slices taken: `IPv4`, the default, `IPv6`, or `any`, which takes every
slice, `FQDN` ones included. A dual-stack Service has a slice of each family
holding the same pods, so taking both would probe each pod twice. The
slices are asked for 100 at a time, following the API server's `continue`
token, so that a page of large slices stays within the 16 MiB an answer may
hold: 100 slices of the EndpointSlice controller's 100 endpoints are about
10 MiB. A page larger than 16 MiB, as a cluster that raises
`--max-endpoints-per-slice` makes, is asked for again, with the same
`continue` token, with half as many slices, down to one, and the rest of
the listing keeps the smaller size; it is logged once at debug level, with
the size the listing fell to. Each listing starts at 100 again, so nothing
outlives it, at the cost of the refused requests, at most seven, each read
no further than 16 MiB. Only a single slice larger than 16 MiB cannot be
listed, and fails the source. `timeout` is as `http_sd`'s, and bounds the
whole listing, every page of every namespace and every page asked for
again, not each request alone. Each target
has the meta labels `__meta_kubernetes_namespace`,
`__meta_kubernetes_endpointslice_name`,
`__meta_kubernetes_endpointslice_address_type`, `__meta_kubernetes_service_name`
(from the slice's `kubernetes.io/service-name` label),
`__meta_kubernetes_pod_name`, `__meta_kubernetes_node_name`,
`__meta_kubernetes_endpoint_hostname`, `__meta_kubernetes_endpoint_port_name`,
`__meta_kubernetes_endpoint_ready` and
`__meta_kubernetes_endpointslice_label_<name>` for each label of the slice,
when they have a value. The allocator needs only `list` on
`endpointslices` in the API group `discovery.k8s.io`, since it neither gets
one slice nor watches; the chart grants that alone
([On Kubernetes](#on-kubernetes)).

There is no `relabel_configs`: the `labels` mapping takes the place of the
relabeling that would copy a meta label to a target label, and a probe
job's service discovery answer carries the `__meta_` labels for
Prometheus's own relabeling.

### Labels

A probe target's group in the [service discovery answer](#service-discovery)
has, besides the labels the allocator sets, those its source gave it: the
`__meta_` labels, which Prometheus drops after relabeling, and those of a
label name but `job` and `instance`; never another label starting with `__`,
which would steer the probe itself. The job's `labels` win over them, and
one the job writes `""` takes a discovered label away.

A static target an entry makes has the labels its source gave it but every
one starting with `__`, and `job`, `instance` and `static_target`; the
entry's `labels` win over them.

### Failures and changes

Each source answers on its own. A source that fails keeps the targets of
its last good answer; one that has never answered adds none, and the
allocator forms its member list and serves the rest meanwhile. An answer
with more targets than the bound, larger than 16 MiB, or that is not a list
of target groups is a failure. A failure is logged, and counted in
`pue_allocator_discovery_refreshes_total{result="failure"}`, once while its
text stays the same.

What a source finds is put in force as a reload is: a bundle changes only
when its share does, a static target no longer found leaves every share at
once, and one whose owner changes is handed over. A reload of the input
files keeps every source whose owner and settings it leaves alone running,
with what it found, so a changed `shard_key` moves discovered targets with
[handover](#members-and-allocation) rather than dropping them. A source whose
settings change, its `refresh_interval` or its `label_selector` say, is
replaced by one of the new settings, which starts with what the old one found
when it is of the same kind, has the same owner and keeps its name: the old
targets stay in force until the new source first answers, and a failure
before then keeps them, as any failure does, and is counted. A source
renamed, or of another kind, starts with no targets.

### One copy with discovery

Each allocator copy asks its sources on its own timer, so two copies can hold
different targets, and so different bundles and service discovery answers,
for up to a refresh interval, and for as long as one of them cannot reach a
source. An exporter replica heartbeats every copy and fetches its bundle from
whichever answers, and Prometheus asks whichever its request reaches, so with
two copies a replica's bundle and a job's targets would flip between them.
An allocator with a discovery source therefore runs as one copy. It keeps no
state and is back within seconds of a restart, while the replicas keep their
bundle in force and Prometheus its last service discovery answer, so one
copy loses nothing but changes made while it is away. The chart runs one
when a source is configured ([On Kubernetes](#on-kubernetes)); without the
chart, run one. An allocator of input files alone may run as several copies.
A copy being stopped stops taking requests at `SIGTERM`, so a replacement
started meanwhile, as one is for an evicted pod, does not serve beside it.

## Members and allocation

A replica in allocator mode heartbeats to the allocator every 10s with its
id, the address Prometheus should send its probes to, and the generation of
the bundle it has in force. A heartbeat the replica gave up waiting for can
still reach the allocator after a later one, so each carries the name of the
replica's process, random and made at its start, and a sequence number
counting that process's heartbeats from 1: the allocator applies a heartbeat
only when its sequence is greater than that of the last it applied of the
same process, and so a late heartbeat neither undoes a later one nor puts a
replica that said it is shutting down back among the live members. A
restarted replica, a new process of the same id, is taken at its first
heartbeat, and a late heartbeat of the process it replaced is not. The replicas heard from within the last 30s, and
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
gap; then the old owner gives it up. That holds whether the member list
changed or a reload of the input files moved the target, by a changed
`shard_key` say. A target whose old owner left has nobody to drain it, and
one the reload removed is in no share at once.

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
{"member": "exporter-0", "advertise": "10.0.3.7:9115",
 "instance": "J7KTZ3TQWK76RFHIUQJRRUD646", "sequence": 42, "generation": "8117cfab…",
 "shutting_down": false, "refused": {"generation": "…", "error": "…"}}
```

`member`, `advertise`, a `host:port` with a host and a decimal port from 1
to 65535, `instance`, the replica process's name of 1 to 64 bytes, and
`sequence`, from 1, are required; anything else is answered `400`. A
heartbeat whose sequence is not greater than the last applied of its member
and instance, or whose instance is the one its member's current instance
replaced, is answered as any other but changes nothing. A member that said
it is shutting down is remembered for 30s after its last heartbeat, though
not live, so a late heartbeat of it is still known for one. A replica
reports a refused bundle until it puts a later one in force; the allocator
logs a refusal once, and again when the replica refuses a bundle after
having reported none. A body over 64 KiB
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
  generation, on every allocator. Discovered targets are part of the files,
  so allocators that discover agree only on what they have found alike
  ([One copy with discovery](#one-copy-with-discovery)).

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
| `pue_allocator_targets_per_member{kind}` | gauge | Those allocated to the members, by `kind`, divided by the number of members: what each replica has on average; `0` with no member. |
| `pue_allocator_generation_info{member,generation}` | gauge | `1`, with the generation of each member's bundle. |
| `pue_allocator_reloads_total{result}` | counter | Changed input files put in force (`success`) or refused (`failure`). |
| `pue_allocator_member_changes_total` | counter | Changes of the member list after the first. |
| `pue_allocator_members_refused_total` | counter | Heartbeats of new members refused because `--members.max` members were live. |
| `pue_allocator_discovered_targets{kind,owner,source}` | gauge | Targets of each discovery source's last good answer, by `kind` (`probe` or `static`), `owner` (the probe job or the `discovered_targets` entry) and `source` (its name); `0` before it answers. |
| `pue_allocator_discovery_refreshes_total{kind,owner,source,result}` | counter | Answers of each discovery source taken (`success`), and its failures (`failure`), one counted once while its text stays the same. |
| `pue_allocator_discovery_last_success_timestamp_seconds{kind,owner,source}` | gauge | When each discovery source last answered, in Unix seconds; `0` before it does. |

## On Kubernetes

The Helm chart deploys the allocator with `allocator.enabled`, beside the
exporter, and switches the replicas to allocator mode; see
[Target allocator](../charts/prometheus-universal-exporter/README.md#target-allocator)
in the chart README. It runs `allocator.replicas` allocators, two by default,
from the exporter's image, behind a headless Service on port 9116: they need
no leader, since each hears every heartbeat, a replica heartbeating to every
address the Service's name resolves to. With a discovery source, in a probe
job or in the static targets, it runs one allocator, and refuses
`allocator.replicas` above 1, rolling it out with the `Recreate` strategy so
the old pod is gone before the new one starts
([One copy with discovery](#one-copy-with-discovery)); its disruption budget
of `maxUnavailable: 1` still lets a node drain evict it. The configuration (`config.data`)
and `allocator.staticTargets.data` and `allocator.probeJobs`, the static
targets and the probe jobs, are mounted into the allocator only; a change to
them rolls the allocator, and reaches the replicas as a new bundle without a
restart. The replicas get `--allocator.url`, their pod's name as the member
and their pod's IP as the address to advertise, keep their bundle in an
`emptyDir`, and keep the credential Secrets mounted, which the allocator does
not need. The allocator always runs with `--web.token-file`, and the replicas
and Prometheus send its token: from the Secret `allocator.tokenSecret.name`
names, or, by default, from one the chart generates and keeps across
upgrades (see
[The allocator's token](../charts/prometheus-universal-exporter/README.md#the-allocators-token)),
since the default NetworkPolicy admits any Prometheus-labelled pod of the
cluster, which without the token could heartbeat as a replica and be handed
targets. The `emptyDir` survives a container restart but not the pod's
replacement: a new pod starts with no bundle, not ready, and waits for the
allocator. A restarted container starts with the bundle its exporter
installed there, and with nothing else: a `config.yaml` put into the
directory by other means is removed. Each probe job
becomes a Prometheus Operator `ScrapeConfig` reading `/v1/sd/<job>`. A
`kubernetes` discovery source, in a probe job or in the static targets, gives
the allocator a ServiceAccount of its own, with its token mounted, and a
Role in each namespace it lists, or a ClusterRole for `all_namespaces`,
allowing `list` on EndpointSlices.

The files a `file_sd` source reads, and an `http_sd` source's
`bearer_token_file` and `tls` files, must be in the allocator's pod:
`allocator.extraVolumes` and `allocator.extraVolumeMounts` mount them there,
as `extraVolumes` and `extraVolumeMounts` mount files into the exporter's.
With the file_sd files in a ConfigMap `websites-sd`:

```yaml
allocator:
  enabled: true
  extraVolumes:
    - name: file-sd
      configMap:
        name: websites-sd
  extraVolumeMounts:
    - name: file-sd
      mountPath: /etc/pue-sd
      readOnly: true
  probeJobs:
    - name: websites
      collector: html_status
      target_template: https://{{address}}/health
      discovery:
        - file_sd:
            files: [/etc/pue-sd/*.json]
```

The paths are absolute: a relative one is taken against the directory of the
file that names it, which in the chart is a ConfigMap of the chart's own. The
kubelet updates a ConfigMap volume in place, not one mounted with `subPath`,
and the source reads the files again every `refresh_interval`, so a change to
the ConfigMap reaches the allocator without a rollout.

Since each replica scrapes only its share of the static targets, adding
replicas spreads them thinner, and the chart's `autoscaling` can scale the
replicas on the static targets per pod, `autoscaling.targetStaticTargetsPerPod`,
or on how busy they are, `autoscaling.targetBusy`
([Autoscaling](SELF-METRICS.md#autoscaling)).

## Without Kubernetes

Run `pue-allocator` on one or two hosts, one with a discovery source
([One copy with discovery](#one-copy-with-discovery)), start each exporter with
`--allocator.url=http://allocator.internal:9116`, and point Prometheus's
`http_sd_configs` at the same address, one scrape configuration per probe
job. systemd units, Compose and plain VMs need nothing else.
