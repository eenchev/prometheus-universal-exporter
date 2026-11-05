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
copy beside the first, answers as the first did. What a
[discovery source](#discovering-targets) finds is input too, and two copies
asking it each on its own timer could find different targets, so copies
with a discovery source run as peers: one leads and asks the sources, and
the others take what it found
([Copies with discovery](#copies-with-discovery)).

## Running it

The allocator is built beside the exporter: `make build` leaves it in
`bin/pue-allocator`, the release archives carry it, and the container image
has it at `/bin/pue-allocator`, beside the exporter (run the image with
`--entrypoint /bin/pue-allocator`, or the pod with `command:
["/bin/pue-allocator"]`). Run that way under Docker, give it
`--no-healthcheck` too: the image's `HEALTHCHECK` asks the exporter's
`/ready` on port 8080 ([Health checks](CONFIGURATION.md#health-checks)), which
the allocator does not serve. It is built with the same request types as the
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
| `--web.listen-address` | `:9116` | Where the API, the [status page](#status-page), `/metrics`, `/healthz` and `/ready` are served, over plain HTTP. |
| `--config.file` | `/etc/prometheus-universal-exporter/config.yaml` | The configuration the replicas are served, with the collector files it lists. |
| `--static-targets-file` | none | The static target file, split among the replicas. |
| `--probe-targets-file` | none | The probe targets file, served to Prometheus at `/v1/sd/<job>`. |
| `--web.token-file` | none | A file holding a bearer token every request but `/healthz` and `/ready` must carry, read again when it changes, so a rotated Secret takes effect without a restart. The status page, `/v1/status` and `/metrics` also take it as the password of Basic auth, for a browser ([Status page](#status-page)). |
| `--config.watch-interval` | `1m` | How often the input files are read again for changes; `0` reads them once. |
| `--settle` | `15s` | How long a changed member list must hold before targets move. |
| `--heartbeat.interval` | `10s` | How often the replicas heartbeat, from `1s` to `2m`, which the allocator tells them in every heartbeat answer; a replica not heard from for three intervals, the member TTL, stops being a member ([Members and allocation](#members-and-allocation)). |
| `--members.max` | `1000` | How many live members the allocator takes; a heartbeat of a new member beyond them is answered `429`. |
| `--discovery.max-targets` | `10000` | How many targets one answer of a [discovery source](#discovering-targets) may hold, for a source that sets no `max_targets`; an answer with more is refused. |
| `--peers` | none | The allocator copies to run beside, as `host:port` entries, comma-separated: a name, resolved to every address it has every 2s, or an IP address. List every copy, this one included, so each finds its own address among them. The copies are then peers, of which only the leader asks the discovery sources ([Copies with discovery](#copies-with-discovery)). |
| `--peers.self` | none | This copy's address as its peers reach it: an IP address, taking the port of `--web.listen-address`, or `ip:port`. Left out, the copy finds itself among its peers' addresses by asking them. Only with `--peers`. |
| `--peers.leader-only` | off | Only the leader serves the whole of `/metrics` and of the status page; a follower serves the peers' metrics and its own peer state alone ([Leader-only metrics](#leader-only-metrics)). Only with `--peers`. |
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
- [`otlp.exporter_instance`](OTLP.md#the-exporter-instance), which names the
  replica on every OTLP resource it sends, takes its value, when the
  configuration writes none, on each replica: its `--allocator.member` name,
  the id it heartbeats with. The allocator leaves it as written.

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

Each source sets exactly one of `file_sd`, `http_sd`, `kubernetes` and
`dns_sd`, and may set:

- `name`, letters, digits, `_`, `.` and `-`, unique in its list; by default
  its kind and its place, such as `file_sd_1`. It names the source in the
  metrics and the logs.
- `labels`, labels set from the source's own: `zone: __meta_zone` gives each
  target the label `zone` with the value of its `__meta_zone`. A name
  starting with `__`, `job`, `instance` and `static_target` are refused.
- `max_targets`, how many targets one answer may hold, `--discovery.max-targets`
  by default; it bounds the leader's snapshot of the source on a follower
  too ([Copies with discovery](#copies-with-discovery)).

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
(`ca_file`, `cert_file`, `key_file`, `server_name`, `insecure_skip_verify`),
for an `https` URL only: a `tls` block, even an empty one, beside an `http`
URL is refused at load, since the request would never use it. Redirects are not followed, and an answer that is not `200` fails.
A failure's text, as the log, the status page and `/v1/status` show it,
gives the URL with the values of query parameters whose names read as
credentials' masked, `?access_token=<redacted>`, as the exporter's logs show
a target ([Logging](LOGGING.md#repeated-failures)); a `url` refused at load
is shown so too, and one that is no URL at all not at all.

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

**`dns_sd`** looks `names` up in the DNS every `refresh_interval`, `30s` by
default, as Prometheus's `dns_sd_configs` does. `type` is the record type
asked for: `SRV`, the default, `A` or `AAAA`. Each SRV record is a target at
its target host, in lower case and without its trailing dot, and its port;
a record whose target is `.`, which says the service is not offered there,
is none. Each A or AAAA record is a target at its address and `port`, which
`A` and `AAAA` require and `SRV` refuses, an IPv6 address written in
brackets (`[2001:db8::1]:9100`). A name is written once in the list, a DNS
name without a port or a scheme, and asked as an absolute name, so the
system's search domains are never appended to it. The names go to the
system's nameservers, or to `server`, `host` or `host:port` (port `53` by
default), when it is set. `timeout`, as `http_sd`'s, bounds the lookups of
all the names together. A name that does not exist, or has no records of
the type, gives no targets and is no failure, so one service gone leaves the
others; any other failure of any name, a server failure, a refusal or no
answer in time, fails the whole answer, which keeps the targets of the last
good one ([Failures and changes](#failures-and-changes)). A target that
several names give, or one name twice, is taken once, with the labels of
the first name in `names` that gives it, and the targets are sorted by
address, so the same records give the same targets whatever order the
server sends them in. Each target has the meta label `__meta_dns_name`, the
name asked, and an SRV target `__meta_dns_srv_record_target` and
`__meta_dns_srv_record_port`, its record's target, as written in the
address, and port.

```yaml
probe_jobs:
  - name: nodes
    collector: node_health
    target_template: http://{{address}}/health
    discovery:
      - dns_sd:
          names: [_metrics._tcp.eu.example.com, _metrics._tcp.us.example.com]
          server: 10.0.0.53:53         # optional: the system's resolver otherwise
        labels:
          service: __meta_dns_name
      - name: legacy
        dns_sd:
          names: [legacy.example.com]
          type: A
          port: 9100
          refresh_interval: 1m
```

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
An entry's `otlp.labels_as_resource` is checked at load, with the entry's
own `labels`, before discovery has found anything: it names labels the entry
writes, not labels a source gives.

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

### Copies with discovery

Each allocator copy asking its sources on its own timer could hold
different targets, and so different bundles and service discovery answers,
for up to a refresh interval, and for as long as one of them cannot reach a
source; an exporter replica heartbeats every copy and fetches its bundle
from whichever answers, and Prometheus asks whichever its request reaches,
so a replica's bundle and a job's targets would flip between them. So copies
with a discovery source run as peers, each given `--peers`: the name every
copy's address resolves to, as the chart's headless Service, or a list of
their addresses. Without `--peers` a copy asks its sources itself, which is
right for one copy alone; copies of input files alone need no peers.

Every 2s each copy resolves `--peers` to its addresses and asks each other
copy at `GET /v1/peer`, with the API's token, within 1s. A copy that answered is
live until three such checks in a row have failed, so one slow answer does
not move the leader; one that answers `503`, as a copy shutting down does,
stops being live at once. A copy knows its own address from `--peers.self`,
or else by asking: the address answering with its own process's name is
itself. The leader is the live copy of the lowest address, itself included:
IPv4 addresses before IPv6, each by its bytes, so `10.0.0.9` before
`10.0.0.10`, then by port. Every copy that sees the same live copies so
chooses the same leader.

A copy whose own address is not known, because `--peers` leaves it out and
no `--peers.self` gives it, or because the copy cannot reach itself, cannot
place itself in that order. It follows only a copy that discovers, which
`/v1/peer` tells, as it tells whether the copy knows its own address: the
lowest such copy that knows its own address, else the one of the lowest
process name among those that do not, unless it leads itself and its own
name is lower still; with no copy discovering it leads. Two such copies so
settle on one leader within two checks, rather than each following the
other with neither asking the sources. After three checks without finding
its own address a copy logs an error once: list every copy in `--peers`,
this one included, or give `--peers.self`.

Only the leader asks the discovery sources. Every other copy, a follower,
fetches the leader's snapshot every 2s at `GET /v1/peer/snapshot`, what each
of its sources last found, and puts it in force as a source's answer would
be, in place of asking the sources itself. The copies so expand the same
input files with the same discovered targets, and serve the same bundles, of
the same generations, and the same service discovery answers. A follower
keeps what it holds of a source that has not answered on the leader.

A snapshot writes each source's targets as the target groups it answered, a
group's labels once rather than once for each of its targets, so it is about
the size of the sources' answers; it may be up to 16 MiB for each of the
follower's sources, the bound of one answer, and a fetch may take 30s, where
a check takes 1s. The fetch runs beside the checks, one at a time, so a
leader that hangs while it writes its snapshot delays none of them: the
follower goes on checking every 2s, and when the leader stops being live it
leads, or follows the next copy, at once, cancelling the fetch. A source's targets in the snapshot are checked as its own
answer is, and held to the follower's `max_targets` of the source, or
`--discovery.max-targets`: more targets than that, an empty target or a
label of no label name refuse them, as the source's failure, logged and
counted in `pue_allocator_discovery_refreshes_total{result="failure"}`, and
the source keeps what the follower holds of it.

A snapshot names each source by its owner, the probe job or
`discovered_targets` entry, its kind and its name, not by its settings, so
during a rolling update that changes a source's settings the new copies
still take the old leader's targets of it. A source of the follower's input
files that the leader's lack altogether, as a new one does while a changed
ConfigMap has reached the follower's pod and not yet the leader's, the
follower asks itself, as a leader would, until the leader's snapshot has
it, so its targets are not missing from the follower's bundles meanwhile.
A fetch names, in `If-None-Match`, the digest of what the follower holds,
not of the snapshot it took last, and the leader answers `304` only when the
two are the same: whatever made them differ since, a changed ConfigMap that
reached the leader's pod first or the follower's own sources answering while
it led for a moment, the next fetch brings the snapshot and puts it right. A
follower whose input files differ from the leader's in their discovery
sources holds other targets after taking the snapshot;
`pue_allocator_snapshot_age_seconds` then grows, and after three such
fetches in a row the follower logs so once, until the two agree again.

When the leader goes away, the copy of the next address leads from its next
check: at once when the leader is shutting down, and after three failed
checks when it stops answering. It starts asking the sources, and serves the
targets of the snapshot it took last until each source first answers, as a
source whose settings a reload changed does, so the targets never leave the
bundles. A copy that comes to lead without ever having taken a snapshot, as
a new copy of the lowest address does, first takes that of the lowest other
live copy. A copy that sees a lower copy come back stops asking its sources and
follows it. A follower that cannot fetch the leader's snapshot keeps serving
the one it took last, never nothing; after three failures in a row it logs
so once, and `pue_allocator_snapshot_age_seconds` tells how old its snapshot
is ([Metrics](#metrics)).

A copy shutting down answers `/v1/peer` with `503` from the first signal,
and keeps serving for 3s more, a round of every peer's checks, before its
server stops: `/ready` answers `503`, a bundle long poll is answered at
once, and heartbeats, bundles, service discovery answers and its snapshot
are served as before. Every peer so sees it shutting down at its next
check, and the next copy leads at once rather than after three refused
connections. Copies being replaced one at a time, as a rolling update does,
so keep the targets. Peers are not a consensus: a network split that leaves two groups
of copies unable to reach each other gives each group a leader, each asking
the sources on its own timer, and while the split lasts the replicas'
bundles and the job's targets can flip between the two groups' findings, as
they would between copies without peers. It heals at the next check after
the copies reach each other again, the higher leader then following the
lower. `pue_allocator_leader{peer}` shows the leader each copy follows, and
copies that disagree on it are split.

#### Leader-only metrics

Every copy serves the same allocation, and so the same series of it in
`/metrics`: a Prometheus scraping every copy, as the chart's ServiceMonitor
does, has one series of `pue_allocator_assigned` for each copy, told apart
only by the `instance` and `pod` labels the scrape adds, and a query has to
take their `max`. With `--peers.leader-only` only the leader serves the whole
of `/metrics`. A follower answers `200` with the peers' families alone,
`pue_allocator_is_leader` (`0`), `pue_allocator_leader{peer}`,
`pue_allocator_peers`, `pue_allocator_peer_up{peer}`,
`pue_allocator_leader_changes_total`, `pue_allocator_snapshot_age_seconds`
and `pue_allocator_peer_failures_total`, and none of the members, the
allocation or the discovery sources. Each follower so stays observable,
and a cluster whose copies all answer `pue_allocator_is_leader 0`, whose sum
is then 0, has no leader; the allocation's series are the leader's alone,
one of each. When another copy comes to lead they come from it, with its
`instance` and `pod`: a query that leaves those labels out, or takes the
`max` without them, as the examples do, reads on across the change, which
shows as a gap of a scrape or two and a counter starting again from the new
leader's count. Before its first check a copy counts as a follower.

A follower's [status page](#status-page) is likewise short: it names the
leader by its address and shows the copy itself and its peers, with the
peers' series, and leaves the members, the spread and the discovery sources
to the leader's page; `/v1/status` gives `"partial": true` with `members`,
`spread` and `sources` `null`. The flag is refused without `--peers`: a copy
alone is its own leader.

## Members and allocation

A replica in allocator mode heartbeats to the allocator every heartbeat
interval with its id, the address Prometheus should send its probes to, and
the generation of the bundle it has in force. The interval is the
allocator's `--heartbeat.interval`, 10s by default, and only the allocator's:
every heartbeat answer says it, and the replica heartbeats at it from the
next heartbeat on, so the two sides cannot disagree, and the exporter has no
setting of it. Until an answer says one, as before its first, the replica
heartbeats every 10s. It keeps an interval said outside `1s` to `2m` within
them, so a bogus answer can neither make it heartbeat without pause nor
fall silent. A replica heartbeats every copy of the allocator
([On Kubernetes](#on-kubernetes)), each on a schedule of its own: a copy
that is slow to answer, or hangs, delays only the heartbeats to itself, and
the others hear the replica every interval. A heartbeat to a copy, and the
lookup of the copies' addresses, made every interval, are given up on after
5s, or the interval when that is shorter; a lookup that fails keeps the
addresses it resolved to last. When the copies say different intervals,
as during a rollout that changes it, it takes the shortest, so each copy
hears it within its own interval, and a copy that does not answer, or says
none, changes nothing. A heartbeat the replica gave up waiting for can
still reach the allocator after a later one, so each carries the name of the
replica's process, random and made at its start, and a sequence number
counting that process's heartbeats from 1: the allocator applies a heartbeat
only when its sequence is greater than that of the last it applied of the
same process, and so a late heartbeat neither undoes a later one nor puts a
replica that said it is shutting down back among the live members. A
restarted replica, a new process of the same id, is taken at its first
heartbeat, and a late heartbeat of the process it replaced is not. The
replicas heard from within the member TTL, three heartbeat intervals (30s
by default), and not shutting down, are the live members, so a replica
outlives two heartbeats lost in a row; a replica that says it is shutting
down stops being live at once, so no probe is routed to it any more, and
leaves the member list as any other change does, after `--settle`.

A change to the live members takes effect once it has held for `--settle`:
a replica that restarts and is back within it keeps its share, and nothing
moves. The first member list forms once the live members have not changed
for `--settle`, each replica heard from for the first time starting the wait
again, and no sooner than `--settle` or a heartbeat interval, whichever is
longer, after the allocator started, so a restarted allocator has heard from
every replica before it hands out shares. The heartbeat interval there is
the longest of the allocator's own and those its replicas say, in each
heartbeat, they were heartbeating at: a copy started with a shorter
`--heartbeat.interval` than the copies before it said, as during a rollout
that shortens it, hears a replica first at the replica's next heartbeat on
the old interval, and a replica looks up the copies' addresses every
interval, so every replica has reached it within the old interval of its
start; its first heartbeat says that interval, and the first member list
waits it.

`--heartbeat.interval` and `--settle` are independent, and no value of one
is refused for the other. A shorter interval finds a replica that has
stopped sooner, at the cost of more heartbeats: a replica whose heartbeats
stop leaves the live members after the member TTL, and its static targets
move `--settle` after that. A replica that restarts under the same id
within the member TTL is never missed, whatever `--settle` is; `--settle`
is what keeps a replica that is gone for longer, but back within it, from
moving its share. An interval longer than `--settle` makes the first member
list wait the interval rather than `--settle`. The copies of the allocator
check each other every 2s whatever the interval
([Copies with discovery](#copies-with-discovery)): it is the replicas'
timing alone. Until then the bundle and service
discovery endpoints answer `503`, and replicas and Prometheus keep what they
have.

The allocator takes `--members.max` live members, 1000 by default. A heartbeat
names any member it likes, so without a bound every new name would add one;
beyond it the heartbeat of a member the allocator does not know is answered
`429`, with `Retry-After` the member TTL in whole seconds, 30 by default,
logged once while the allocator stays full, and
counted in `pue_allocator_members_refused_total`. The members it has keep
heartbeating, and a replica whose heartbeats stop frees its place when its
member TTL runs out, though it stays in the member list until `--settle`
after that.

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

Exported over OTLP with
[`otlp.exporter_instance`](OTLP.md#the-exporter-instance), a static target's
data goes on, after a move, under the new owner's name: a new resource to a
backend that keys series on resource attributes.

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
holds a token again, logged once. A browser cannot send a bearer token, so
`GET` and `HEAD` of the status page, `/v1/status` and `/metrics`, what a person reads,
also takes the token as the password of HTTP Basic auth, of any user name,
and their `401` asks for it, so the browser prompts; no other endpoint takes
Basic auth, since a browser sends the credentials it was given with any
request to the allocator, another site's page's included, and a heartbeat
must not be one of them. The token is never taken from the URL.

| Endpoint | Caller | Answer |
| --- | --- | --- |
| `POST /v1/heartbeat` | replica | The generation of the bundle the replica is to have, and the heartbeat interval. |
| `GET /v1/bundle?member=<id>` | replica | The replica's bundle, with long polling. |
| `GET /v1/sd/<job>` | Prometheus | The job's probe targets in the HTTP service discovery format. |
| `GET /v1/peer` | another copy | `{"instance": "…", "discovering": true, "address_known": true}`: the copy's process, whether it asks its discovery sources, and whether it knows its own address (or runs without `--peers`); `503` while it shuts down. |
| `GET /v1/peer/snapshot` | another copy | Every discovery source of the copy's input files, whether it has answered and the target groups it last found, with the digest of it as its `ETag`, `304` to an `If-None-Match` naming it. |
| `GET /` | a person | The [status page](#status-page). |
| `GET /v1/status` | a person, a script | What the status page shows, as JSON. |
| `GET /metrics` | Prometheus | The allocator's own metrics. |
| `GET /healthz` | kubelet | `200` while the process serves. |
| `GET /ready` | kubelet | `200` once the input files are loaded, which they are from the start; `503` while shutting down. |

### Heartbeats

```json
{"member": "exporter-0", "advertise": "10.0.3.7:9115",
 "instance": "J7KTZ3TQWK76RFHIUQJRRUD646", "sequence": 42, "generation": "8117cfab…",
 "shutting_down": false, "refused": {"generation": "…", "error": "…"}, "interval_seconds": 10}
```

`member`, `advertise`, a `host:port` with a host and a decimal port from 1
to 65535, `instance`, the replica process's name of 1 to 64 bytes, and
`sequence`, from 1, are required; anything else is answered `400`. A
heartbeat whose sequence is not greater than the last applied of its member
and instance, or whose instance is the one its member's current instance
replaced, is answered as any other but changes nothing. A member that said
it is shutting down is remembered for the member TTL after its last heartbeat, though
not live, so a late heartbeat of it is still known for one. A replica
reports a refused bundle until it puts a later one in force; the allocator
logs a refusal once, and again when the replica refuses a bundle after
having reported none. `interval_seconds` is the interval the replica was
heartbeating at when it sent the heartbeat, which the first member list
waits for ([Members and allocation](#members-and-allocation)). A body over 64 KiB
is answered `413`, and the heartbeat of a new member beyond `--members.max`
`429`. The answer is `{"generation": "…", "heartbeat_interval_seconds": 10}`:
the generation of the member's bundle, or `""` while the member list has not
formed, and `--heartbeat.interval` in seconds. A replica whose bundle
differs fetches it, and heartbeats at the interval from its next heartbeat
on ([Members and allocation](#members-and-allocation)).

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
  so allocators that discover agree on them by taking the leader's
  ([Copies with discovery](#copies-with-discovery)).

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

## Status page

`GET /` is a page for a person: what this copy holds, without a Prometheus
to ask. It shows

- this copy: its address (among its peers, or `--web.listen-address`), its
  process's instance, version and revision, when it started and whether it
  is ready;
- with `--peers`, whether it leads or follows, which copy leads, and
  whether it runs with `--peers.leader-only`, and
  every copy its peers' addresses give, itself included, with whether it is
  live, discovers and knows its own address, when it was last checked and
  the checks it failed in a row; how old the copy's snapshot is and whether
  it holds the leader's discovered targets; and the series of the peers'
  metrics as `/metrics` has them ([Metrics](#metrics));
- the members: the heartbeat interval and the member TTL; each one's
  address, how long ago its last heartbeat came, marked when a heartbeat is
  overdue, more than one and a half intervals after the last,
  the bundle it has in force beside the one it is to have, a refusal it
  reports, whether it is in the member list, joining, silent or shutting
  down, and the static targets it keeps draining;
- how the targets are spread: for each member, the static targets it owns,
  those it keeps draining, the probe targets service discovery routes to it,
  by probe job and in all, and the total, with a bar of each; then the
  totals, the average per member and the imbalance, the most any member has
  divided by the average, `1.00×` for targets spread evenly;
- the discovery sources: each one's kind, owner, result, with the error
  while it fails, its targets, when it was last refreshed, and whether what
  it holds is this copy's own discovery or the leader's snapshot.

The page refreshes itself as the exporter's status page does: Refresh, and
an auto-refresh of Off, 5s, 10s, 30s, 1m and on, kept in the URL as
`?refresh=30s`, fetching the page's content again (`?format=fragment`) and
skipping its turn while the tab is hidden. It has the exporter's pages' bar,
with their theme picker, and needs nothing from outside the allocator.

`GET /v1/status` answers the same as JSON: `generated_at`; `copy`
(`address`, `instance`, `version`, `revision`, `started_at`,
`uptime_seconds`, `ready`); `peers`, `null` without `--peers` (`role`,
`leader` or `follower`, `deciding` before the first check; `leader`, the
leader's address; `leader_only`; `live_copies`, `leader_changes`, `snapshot_age_seconds`,
`snapshot_taken`, `holds_leader_state`, the failures, `copies`, each with
its `address`, `self`, `leader`, `live`, `instance`, `discovering`,
`address_known`, `last_check` and `failures`, and `metrics`, each a
`series` and its `value`); `partial`, true on a follower under
`--peers.leader-only`, whose `members`, `spread` and `sources` are `null`;
`members` (`formed`, `in_list`, `live`, `heartbeat_interval_seconds`,
`member_ttl_seconds` and
`list`, each with `id`, `address`, `last_heartbeat`, `late`, `live`, `in_list`,
`shutting_down`, `generation`, `current`, `up_to_date`, `refused` and
`draining`); `spread` (`jobs`, and `members`, `total`, `average` and
`imbalance`, each with `static`, `draining`, `probe`, `probe_jobs` by job
and `total`); and `sources` (`name`, `kind`, `owner`, `type`, `result`,
`ok`, `failing` or `pending`, `error`, `targets`, `last_refresh`,
`last_success` and `from`, `own` or `snapshot`). Times are RFC 3339 in UTC,
each with its age in seconds beside it.

With `--web.token-file` a browser asks for a user name and a password on
the first visit: give any user name, and the token as the password
([API](#api)). On Kubernetes, reach the page through `kubectl port-forward`
([On Kubernetes](#on-kubernetes)).

### A member's targets

A member's id in the members and in the spread links to its view,
`GET /?member=<id>`: what that one exporter replica has, to answer "which
targets does this pod scrape?" without reading its bundle. It shows the
member as the members do, then

- the static targets it owns, by name, each with its target, collector,
  labels and interval;
- the static targets it keeps draining, each with the member it moved to,
  which links to that member's view, and how much longer it drains;
- the probe targets service discovery routes to it, counted by probe job,
  then each with its job and collector.

The counts are the spread's for the member. A target is shown as the
exporter's pages show it, its credentials withheld: a URL's user and
password as `redacted`, and the values of query and path parameters whose
names read as credentials', such as `token` or `access_token`, as
`<redacted>`; a static target's `params` and `request` block are not shown
at all. A filter box above the lists hides, as it is typed, the rows that
do not hold its text, in any case, in their name, target, collector, a
label as `name=value`, job or the member they moved to, each on its own, so
text running across two of them, or in another column, as an interval,
matches nothing, with no page load; Search all, or Enter,
asks the allocator with `?filter=<text>`, which applies the same match to
every target of the member before the bound below. A filter of spaces
alone is no filter. On a phone each row's cells stack, each beside its
name. Each list holds at most
1000 targets, so a member of a share of tens of thousands stays a page a
browser draws at once; a list cut short says how many are left out, and
the filter finds them. The view refreshes itself as the page does, the
filter kept, and its bar's JSON link is `GET /v1/status?member=<id>`, the
same as JSON: `generated_at`, `member`, `filter`, `limit` (1000),
`partial`, `leader`, `status` (the member as `members.list` has it),
`static` and `draining` (`total`, `matching` and `list`, each with `name`,
`collector`, `target`, `labels`, `interval`, and while draining `moved_to`
and `drains_for_seconds`), and `probes` (`jobs`, each with `job`,
`collector` and `targets`; `total`, `matching` and `list`, each with `job`,
`collector` and `target`). A member the allocator does not know is
answered `404` as JSON, and the page, and its fragment, say so with `200`,
so a view left open, refreshing itself, of a member that has gone shows
that it has, its filter box hidden; `?member=` without an id is answered
`400`. Under
`--peers.leader-only` a follower serves no member's view, as it serves no
members: the page names the leader, whose page has it, and the JSON is
`partial` with the leader's address.

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
| `pue_allocator_peers` | gauge | With `--peers`: the live copies, this one included. |
| `pue_allocator_peer_up{peer}` | gauge | With `--peers`: `1` for each other copy that is live, by its address, `0` for one that has stopped answering, until its name no longer resolves to it. |
| `pue_allocator_is_leader` | gauge | With `--peers`: `1` while this copy leads and asks the discovery sources, `0` while it follows. |
| `pue_allocator_leader{peer}` | gauge | With `--peers`: `1`, with the address of the copy this one takes for the leader, `self` while it leads without knowing its own address. Copies that disagree on it are split. |
| `pue_allocator_leader_changes_total` | counter | With `--peers`: changes of the leader after the first choice. |
| `pue_allocator_snapshot_age_seconds` | gauge | With `--peers`: seconds since this copy last held what the leader's snapshot holds, having fetched it, or since its start before the first time; `0` while it leads. It grows while the snapshot cannot be fetched, and while the copies' input files differ in their discovery sources. |
| `pue_allocator_peer_failures_total{request}` | counter | With `--peers`: requests to the other copies that failed, `check` or `snapshot`. |

With `--peers.leader-only` a follower serves the families of `--peers`
alone ([Leader-only metrics](#leader-only-metrics)).

With `--peers` only the leader asks the discovery sources, so only its
`pue_allocator_discovery_refreshes_total` and
`pue_allocator_discovery_last_success_timestamp_seconds` move, and only it
logs a source's failures, but for the leader's snapshot of a source that a
follower refuses; a follower's `pue_allocator_discovered_targets`
counts the targets of the snapshot it took.

## On Kubernetes

The Helm chart deploys the allocator with `allocator.enabled`, beside the
exporter, and switches the replicas to allocator mode; see
[Target allocator](../charts/prometheus-universal-exporter/README.md#target-allocator)
in the chart README. It runs `allocator.replicas` allocators, two by default,
from the exporter's image, behind a headless Service on port 9116: each
hears every heartbeat, a replica heartbeating to every address the Service's
name resolves to, so for the members and the allocation they need no
leader. With a discovery source, in a probe job or in the static targets,
each is run with `--peers` naming that Service and `--peers.self` its pod's
IP, so the pods are peers and only the leader asks the sources
([Copies with discovery](#copies-with-discovery)), and the allocator's
NetworkPolicy admits the allocator's own pods; they are rolled out as any
Deployment is, one at a time, under a disruption budget of
`maxUnavailable: 1`. The configuration (`config.data`)
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
allocator, answering every endpoint but `/health` and `/ready` with `503`
until the first bundle brings its `web.basic_auth`, the self-metrics
included (see
[Configuration from the target allocator](CONFIGURATION.md#configuration-from-the-target-allocator)). A restarted container starts with the bundle its exporter
installed there, and with nothing else: a `config.yaml` put into the
directory by other means is removed. Each probe job
becomes a Prometheus Operator `ScrapeConfig` reading `/v1/sd/<job>`. A
`kubernetes` discovery source, in a probe job or in the static targets, gives
the allocator a ServiceAccount of its own, with its token mounted, and a
Role in each namespace it lists, or a ClusterRole for `all_namespaces`,
allowing `list` on EndpointSlices; `allocator.serviceAccount.name` runs the
allocator as an existing account instead, to which those Roles are bound.

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

`allocator.leaderOnly` runs the peers with `--peers.leader-only`, so the
allocator's ServiceMonitor, which scrapes every pod, has one pod's series of
the allocation ([Leader-only metrics](#leader-only-metrics)). Each allocator
pod's [status page](#status-page) is reached with `kubectl port-forward`, the
leader's by the pod IP every pod's page names; see
[The allocator's status page](../charts/prometheus-universal-exporter/README.md#the-allocators-status-page).

Since each replica scrapes only its share of the static targets, adding
replicas spreads them thinner, and the chart's `autoscaling` can scale the
replicas on the static targets per pod, `autoscaling.targetStaticTargetsPerPod`,
or on how busy they are, `autoscaling.targetBusy`
([Autoscaling](SELF-METRICS.md#autoscaling)).

## Without Kubernetes

Run `pue-allocator` on one or two hosts, with a discovery source each given
`--peers` with both hosts' addresses, or a name resolving to both
([Copies with discovery](#copies-with-discovery)), start each exporter with
`--allocator.url=http://allocator.internal:9116`, and point Prometheus's
`http_sd_configs` at the same address, one scrape configuration per probe
job. systemd units, Compose and plain VMs need nothing else.
