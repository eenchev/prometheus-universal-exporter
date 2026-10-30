# Static targets

Besides answering `/probe`, the exporter can scrape a fixed list of targets
itself, each on its own interval, and serve their latest results together on
one endpoint for Prometheus to scrape, like any other exporter's metrics. A
target can also be delivered over OTLP.

Use them when the list of targets is known and fixed, and a Prometheus job per
target, or service discovery and relabelling for the probe pattern, would be
more machinery than the list deserves. For targets Prometheus discovers, keep
`/probe`.

## The target file

Pass the list with `--static-targets-file`.
`configs/static-targets.example.yaml` is a complete example, and
`configs/static-targets.schema.json`, printed by
`--static-targets-file-schema`, is the file's JSON Schema, for editors; start a
target file with

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/eenchev/prometheus-universal-exporter/main/configs/static-targets.schema.json
```

A target file looks like this:

```yaml
interval: 1m            # required: for every target that sets none
concurrency: 8          # how many targets are scraped at once; 8 when unset
targets:
  - name: legacy_eu
    collector: legacy_text
    target: http://legacy.eu.example:8080
    interval: 30s       # this target's own
    request:
      path: /status
      timeout: 5s       # at most the interval
      retry:
        attempts: 2
        backoff: 2s
      headers:
        X-Tenant: team-a
      bearer_token_file: /var/run/prometheus-universal-exporter/target-auth/token
    labels:
      region: eu
  - name: legacy_us
    collector: legacy_text
    target: http://legacy.us.example:8080
    labels:
      region: us
```

<!-- request-types: all -->
`interval` and `targets` are required, and the file refuses keys it does not
know. A target's `target` is checked when the file loads as its collector's
type reads it: an `http` or `graphite` target is an absolute URL, and one
whose port is 0 or past 65535, which no connection can use, is refused
naming the port, as a `grpc`, `sshfile`, `sql`, `tlscert` or `ntp` target's is. Each target names a collector from the exporter configuration and takes
every per-scrape parameter `/probe` accepts — `method`, `path`, `body`,
`timeout`, `insecure_skip_verify`, `follow_redirects`, `enable_http2` and the
`retry` settings — overriding the collector's own request for that target only.
Which of these keys a target may set follows its collector's
[request type](CONFIGURATION.md#request-types), as the table says; a key the
type does not take is refused when the file loads, naming the target, and of
several such keys, a key of `retry` for other types among them, the first in
the file (an unknown key is refused as soon as the file is read, before its
collector is looked at):

| Type | `target` | What a target's `request` may set |
| --- | --- | --- |
| `http` | an absolute URL | `method`, `path`, `body`, `timeout`, `insecure_skip_verify`, `follow_redirects`, `enable_http2`, `retry`, `headers`, [`accept_status`](REQUESTS.md#accepting-other-statuses), [`rate_limit`](REQUESTS.md#rate-limiting), its own credentials: `basic_auth`, `basic_auth_file`, `bearer_token`, `bearer_token_file` and [`login`](AUTHENTICATION.md#logging-in-to-a-target), and its own `tls` |
| [`graphite`](GRAPHITE.md#static-targets) | the Graphite server's URL | all `http`'s keys but `method` and `body`, plus its own `targets`, `from` and `until` |
| [`grpc`](GRPC.md#static-targets) | the server | `message`, `metadata`, `timeout`, `insecure_skip_verify`, `retry`, with its `codes`, [`accept_codes`](GRPC.md#errors-and-retries), its own credentials, `basic_auth`, `basic_auth_file`, `bearer_token` and `bearer_token_file`, and its own `tls` |
| [`localfile`](LOCALFILE.md#static-targets) | a file under `request.root`, optional | `path` and `timeout` |
| [`sshfile`](SSHFILE.md#static-targets) | the host | `path`, `timeout`, and its own account and credential files: `username`, `private_key_file`, `passphrase_file` and `password_file` |
| [`sql`](SQL.md#static-targets) | the database server, or a `snowflake` account | `timeout`, its own `username` and `password_file` or `private_key_file`, its own `tls`, and `connections` but `max_targets`, a target whose `connections` change a setting being a target of its own for the collector's limits |
| [`dns`](DNS.md#static-targets) | the name to resolve | `timeout` |
| [`tlscert`](TLSCERT.md#static-targets) | the server | `timeout` |
| [`ntp`](NTP.md#static-targets) | the server | `timeout` |
| [`kubernetes`](KUBERNETES.md#static-targets) | none at all | `timeout` |
| [`s3file`](S3FILE.md#static-targets), opt-in | the bucket, required | `path` and `timeout` |
| [`redis`](REDIS.md#static-targets), opt-in | the server, required | `timeout`, and its own account and TLS settings: `username` and `password_file`, and `tls` |
| [`tcp`](TCP.md#static-targets), opt-in | the service, `host:port`, required | `timeout`, and its own `tls` |
| [`ldap`](LDAP.md#static-targets), opt-in | the server, required | `timeout`, and its own account and TLS settings: `bind_dn` and `password_file`, and `tls` |
| [`sshexec`](SSHEXEC.md#static-targets), opt-in | the host, required | `timeout`, and its own account and credential files: `username`, `private_key_file`, `passphrase_file` and `password_file`; nothing of the command |

An `http` or `graphite` target also takes static
`headers`, and an `http`, `graphite` or `grpc` target its own target
credentials, inline or file-backed, as basic authentication or a bearer
token; a basic username
holds no colon, which is refused at startup written in the target and fails
the target's scrape read from its file
([Authentication](AUTHENTICATION.md#credentials-in-the-configuration)).
A target's own credential is sent instead of the collector's, whose
`bearer_token_file` or `basic_auth_file` is then not read for that target: a
collector's credential file that is missing fails only the targets that
would have sent it. An `http` or `graphite` target may also set its own
[`login`](AUTHENTICATION.md#logging-in-to-a-target), checked as a
collector's is, which replaces the collector's for that target, and the
collector's credential with it when both are sent in `Authorization`; a
target that sets its own credential in the header the collector's login
sends its token in makes no login. A target's `login` is part of the key
its answers are cached under, so targets that log in differently share no
answer.
An `http` or `graphite` target may set its own
[`rate_limit`](REQUESTS.md#rate-limiting), checked as a collector's is, which
replaces the collector's for that target's requests, both keys together:
they have a limit of their own for each origin they go to, apart from the
one the collector's other targets share there. Its requests take their turns
under the configuration's [`rate_limits`](REQUESTS.md#rate-limits-per-host)
as well, as every collector's do. It changes no answer, so it
is no part of the cache key.
A target cannot set a [`proxy_url`](REQUESTS.md#a-collectors-own-proxy):
its requests go through its collector's proxy, and a target behind another
proxy is scraped by a collector of its own.
An `http` or `graphite` target may set its own
[`accept_status`](REQUESTS.md#accepting-other-statuses), and a `grpc` target
its own [`accept_codes`](GRPC.md#errors-and-retries), replacing the
collector's for that target, which no probe parameter can: a health endpoint
that answers `503` with a useful body on one host only takes it there.
Because the file is operator configuration rather than caller input, these
headers are applied directly and are not filtered through the collector's
`request.forward_headers` allowlist.
<!-- /request-types -->

A collector's [`{{param_…}}` placeholders](REQUESTS.md#path-parameters) — in
its path, body, header values, query values, Graphite targets and gRPC
message and metadata values, and in its [label
values](REQUESTS.md#in-label-values), `transform.labels` and a rule's static
`value` — are filled by the target's
`params`, since there is no probe to supply `param_<name>`:

```yaml
interval: 1m
targets:
  - name: acme_checkout
    collector: graphql_status
    target: https://api.example
    params:
      param_tenant: acme
      param_service: checkout
```

Every placeholder must be filled, by `params` or a default, and every entry of
`params` must fill one; otherwise the exporter refuses to start, naming the
target, the collector and the parameter. A target's own `target`, `request.path`, `body`
and `headers` are written out in full, without placeholders.

So one collector whose labels take a parameter serves several targets, each
labelled with its own value:

```yaml
# the collector:  transform: {type: jq, labels: {tenant: "{{param_tenant}}"}}
interval: 1m
targets:
  - name: acme
    collector: tenant_status
    target: https://api.example
    params: {param_tenant: acme}       # its series carry tenant="acme"
  - name: globex
    collector: tenant_status
    target: https://api.example
    params: {param_tenant: globex}     # and these tenant="globex"
```

A target that leaves the parameter out is refused when the file loads:
`target "acme" uses collector "tenant_status", whose transform.labels.tenant
needs param_tenant, a parameter without a default; a static target has no
probe to supply it, so set it under the target's params, or give the
placeholder a default`. The target's own `labels` are literal: a placeholder
written there is exported as the text it is.

A `{{param_…}}` placeholder anywhere a target writes a value of its own — its
`target`, `request.path`, `request.body` or a header value — is refused when
the file loads, naming the target and the field: there is no probe to fill
it, and it would reach the target as text. So is `{{` in an `sshfile`, `sql`,
`redis` or `ldap` target's account — `username`, `bind_dn` and the credential
files — and in a target's own `tls` block, which are used as written. Braces that do not open
`{{param_` are left alone, so a JSON body is written as usual. The same holds
for a value an environment variable supplies, since the file is checked after
it is expanded.

`labels` are added to every metric the target produces, without overwriting a
label the collector already extracted. A label written `""` is the label left
out: it is added to no series, the target's health series included, where it
used to be exported as `team=""`. A target's `le` is left off a
histogram and its `quantile` off a summary, whose buckets and quantiles carry
that label already. `static_target` is the endpoint's own
label, and `job` and `instance` are Prometheus's, set when it scrapes the
endpoint, so a target may set none of the three: kept as the series' own
labels, as the endpoint is scraped, a target's `job` would move its series out
of the job that scrapes it. Name such a label something else, such as `task`.
A label's name is a classic Prometheus name, letters, digits and `_`,
whatever the collector's [`name_escaping`](CONFIGURATION.md#utf-8-names):
the labels are added to what the collector exported, after its names were
escaped. Since they are added after the collector's series are checked, a
label name longer than the collector's
[`limits.max_label_name_length`](CONFIGURATION.md#long-label-names) is
refused when the file is loaded, and so is a target of a collector whose
limit is under 13 bytes, the length of the `static_target` label the
endpoint adds, beside `collector` and `target` on the health series.

A key, an entry of a list or a label written with no value at all — nothing
after the colon or the dash, `null` or `~` — is refused, as it is in the
[configuration](CONFIGURATION.md#checked-when-the-configuration-loads), naming its line: leave the key
out for its default. It was taken as the key left out, and a `request.path`
or `request.body` so written as one written `""`, which replaces the
collector's.

A value is written as the kind its key takes, as in the
[configuration](CONFIGURATION.md#checked-when-the-configuration-loads). A
target's `name` and `request.method`, and an entry of `request.accept_codes`
and `request.retry.codes`, are text alone: a number or a boolean there is
refused, saying to quote it if that text is meant (`name: "1"`). A label, a
parameter, a header, a path, a body or the `collector` takes one as the text
it spells, so `labels: {shard: 3}` needs no quotes. And a boolean is `true`
or `false`: `export_via_otlp: yes` is refused, saying to write `true`.

Targets that share a collector, a target or labels can write them once with
a YAML anchor under a top-level `x-` key, which the exporter ignores, and
merge it into each target (see
[Reusing settings with YAML anchors](CONFIGURATION.md#reusing-settings-with-yaml-anchors)):

```yaml
x-open-meteo: &open_meteo
  collector: open_meteo_current
  target: https://api.open-meteo.com
interval: 10m
targets:
  - <<: *open_meteo
    name: sofia
    params: {param_latitude: "42.6977", param_longitude: "23.3219"}
    labels: {city: Sofia}
  - <<: *open_meteo
    name: varna
    params: {param_latitude: "43.2141", param_longitude: "27.9147"}
    labels: {city: Varna}
```

That is [`examples/open-meteo/static-targets.yaml`](../examples/open-meteo/static-targets.yaml),
shortened; the configuration with its collector is
[`config.yaml`](../examples/open-meteo/config.yaml) beside it, and the two run
together as
`prometheus-universal-exporter --config.file=examples/open-meteo/config.yaml --static-targets-file=examples/open-meteo/static-targets.yaml`,
which `make run EXAMPLE=open-meteo` builds and runs.

A merge key works inside a target's `request` too — `request: {<<: *request,
timeout: 2s}` — and a `path` or `body` it brings in replaces the collector's
as one written there does.

A target may also set `shard_key`, which the exporter ignores: it is for the
[target allocator](ALLOCATOR.md#static-targets), which hashes it in place of
the target's name to choose the replica that scrapes the target, so targets
that share a key share a replica.

The exporter discovers no targets: a file with the allocator's
`discovered_targets` key ([Discovering targets](ALLOCATOR.md#discovering-targets))
is refused, naming the allocator, since the exporter alone would scrape none
of what it describes. The allocator writes each discovered target out as a
target of its own before it hands a replica its share. The file's schema,
`configs/static-targets.schema.json`, refuses the key too; the allocator's
file has a schema of its own that takes it,
[`configs/allocator-static-targets.schema.json`](../configs/allocator-static-targets.schema.json).

Check a target file together with its configuration before deploying it:
`prometheus-universal-exporter --dry-run --config.file=config.yaml --static-targets-file=static-targets.yaml`
reports whether each would load, including whether every target names a
collector that exists and whether a target exported over OTLP has OTLP export
to go to — see [Dry run](CONFIGURATION.md#dry-run).

## Credentials and TLS of a target

One collector can serve several servers that each take their own password
or client certificate: a target names its own account, or its own `tls`
block, in place of the collector's. This is what each type takes:

| Type | A target's own account | A target's own `tls` |
| --- | --- | --- |
| `http`, `graphite` | `basic_auth`, `basic_auth_file`, `bearer_token`, `bearer_token_file`, `login`, each replacing the collector's as above | yes, or `insecure_skip_verify` alone |
| `grpc` | `basic_auth`, `basic_auth_file`, `bearer_token`, `bearer_token_file` | yes |
| `sshfile` | `username`, and `private_key_file`, `passphrase_file` and `password_file`, any of the three replacing all three of the collector's | no; the host is verified with the collector's `known_hosts_file` |
| `sql` | `username`, and `password_file` or, for `snowflake`, `private_key_file`, either replacing the collector's credential file | yes |
| `redis` | `password_file`, replacing the collector's, and `username`, which needs the target's own `password_file` | yes |
| `ldap` | `password_file`, replacing the collector's, and `bind_dn`, which needs the target's own `password_file` | yes |
| `tcp` | none: a tcp service is asked no password | yes |
| `localfile`, `s3file`, `dns`, `tlscert`, `ntp`, `kubernetes` | none | no |

A target's `tls` replaces the collector's whole block, a key it leaves out
not being taken from the collector's, so a target with a client certificate
of its own names the CA it verifies the server with too. Written, even as
`tls: {}`, it asks for TLS, as the collector's block does; so a target can
ask for TLS its collector does not, but not leave out the TLS its collector
asks for. It is checked when the file loads as the collector's is: a client
certificate needs both `cert_file` and `key_file`, and a target whose
address asks for plaintext — `redis://`, `ldap://` without the collector's
`start_tls`, `grpc://` — is refused with a block of its own, as is one the
`sql` driver's options switch TLS off for.

For `http` and `graphite` the target URL's scheme decides whether TLS is
spoken, so `tls: {}` verifies an `https://` target with the system roots
alone, and a block on an `http://` target is taken, as the collector's is,
for the `https` requests its trips make. The target's block is the one every
request of its trips is made with: the target's request and its pages,
each hop of a followed redirect, and its [login](AUTHENTICATION.md#logging-in-to-a-target),
unless the `login` block names a `tls` of its own. Its client certificate
is a credential as the collector's is: a redirect over `https` to a host
that is not the target's origin and is not in `redirect_trusted_hosts` is
refused rather than shown it
([What a followed redirect carries](REQUESTS.md#what-a-followed-redirect-carries)).
Beside the block, the target's `insecure_skip_verify` is refused: write it
in the block.

```yaml
interval: 1m
targets:
  - name: tenant_a
    collector: redis_memory
    target: tenant-a.cache:6379
    request:
      username: exporter
      password_file: /etc/exporter/redis/tenant-a
  - name: tenant_b
    collector: redis_memory
    target: rediss://tenant-b.cache:6380
    request:
      username: exporter
      password_file: /etc/exporter/redis/tenant-b
      tls:
        ca_file: /etc/exporter/redis/tenant-b-ca.pem
```

Only a static target sets these keys: the file is the operator's, and no
`/probe` parameter sets an account, a credential file or a `tls` block, so a
probe always reads as its collector does. A target's own files are read
when its scrape needs them, at each scrape or connection as the collector's
are, so a rotated password is used without a reload; `--dry-run` reads them
too and warns, under the target's name, of one a scrape would fail on now.
A debug report masks a target's password as it masks the collector's.

A target's account and `tls` block are part of the key of everything that
tells requests apart: the key its answers are
[cached](CONFIGURATION.md#response-caching) under, the key of the trip
identical scrapes and probes share, and the key
[`failure_backoff`](CONFIGURATION.md#backing-off-from-failing-targets)
backs off by, and, for the types that keep connections, the key of the
`sql` pool, the `grpc` connection, the `sshfile` connection and the `http`
[connection pool](REQUESTS.md#connections) it is made on, and of the
token an `http` or `graphite` login keeps for it. Two targets that read as
different accounts, or with different `tls` blocks, never share an answer,
a trip, a backoff, a connection or a login's token, even of one collector
and one server.

## Scraping

Each target is scraped on its own `interval`, as Prometheus scrapes a probe on
its `scrape_interval`. Neither Prometheus's scrapes of the endpoint nor the OTLP
export decide when a target is scraped: the endpoint serves, and the export
delivers, what the last scrape of each target left.

Every running exporter scrapes every target in its file. Run one replica for
a static target file, without a HorizontalPodAutoscaler: each further replica
contacts every target again on the same interval, serves results of its own
on the endpoint, and exports every target with `export_via_otlp` again, so the
OTLP backend receives duplicate series. To spread many targets, split them
into files served by separate exporters. The Helm chart warns when it renders
static targets with more than one replica or with autoscaling (see
[Autoscaling](../charts/prometheus-universal-exporter/README.md#autoscaling)).

A target is first scraped within ten seconds of the exporter starting, or of
the reload that added it or changed it in any way — its address, its request,
its params, its interval, or anything in the definition of its collector — so
it is on the endpoint
promptly even with an interval of an hour. After that it keeps a cadence at a
point within its interval set by its name, so targets sharing an interval are
spread over it rather than all scraped at once: the cadence starts at the first
such point at least one whole interval after the first scrape was due — between
one interval and two after it — and then comes every interval, however long a
scrape takes. With an interval of ten seconds or less the first scrape is
itself due at such a point, so the second scrape comes one interval after it.
The interval is counted from the time the first scrape was due, not from the
moment the exporter came to start it, which is always a little later; only for
a changed target whose first scrape had to wait for a scrape begun on its old
definition, and for a first scrape started more than a tenth of the interval
late, as after the process was held up, is it counted from when the first
scrape began. The first scrape
has its interval to end in, as every scrape
has, so it has ended when the cadence starts, and a target whose scrapes take
most of their interval — 26 seconds of 30 — is not cut short and reported
down after a start or a reload. A scrape
must end within its interval — one that does not fails saying the scrape ran
out of its interval — so `request.timeout` may not be longer, and
retries — the target's `request.retry`, else the collector's — whose waits
alone fill the interval (`attempts` × `backoff`) are refused, since the last of
them could never be made; retries that fit can still be cut short by slow
attempts. The interval is at least `1s`.

Two scrapes of a target never run at once. A scrape that uses its whole
interval — as every scrape of a target that never answers does — ends just
after the target's next turn has come. That turn is not given up: it waits
for the scrape, starts as soon as it has ended, and must itself end by the
turn after it, so it has slightly less than the interval. The scrape before
it ran over by little: a scrape of the steady cadence, or the first scrape,
which runs into the cadence by no more than the moment it was started late.
A target that hangs
is therefore tried once per interval, each failure logged as
`static target scrape failed`, and is seen to answer again within one
interval. A turn is skipped, with a `static target scrape skipped` warning,
only when it is really lost: the scrape before it was still running when the
turn after it came — a scrape held by something its deadline does not end, or
an exporter short of CPU — or no scrape slot came free within the interval.

At most `concurrency` targets are scraped at once,
8 unless the file says otherwise; a target due while all are busy waits for a
slot within its interval, and is skipped, with a warning, if none frees. Each
also waits for a slot of its collector's `max_concurrent_probes`, which it
shares with the probes. A reload that changes `concurrency` applies to the
scrapes that start after it.

Retries come from the collector's `request.retry`, and each key a target's own
`request.retry` sets replaces the collector's, as the `retry_attempts` and
`retry_backoff` probe parameters each replace one: a target setting only
`attempts: 3` keeps the collector's `backoff` and `non_idempotent`. An
`http` or `graphite` target's `retry.statuses` replaces the collector's list,
or the default set, whole, as the collector's own replaces the default
([Retries](REQUESTS.md#retries)); an entry that the `accept_status` in force
for the target, its own or else the collector's, accepts whole is refused,
since it could never be retried. Its `retry.retry_after` replaces the
collector's either way, so one target known to send a `Retry-After` worth
waiting for can have it read while the collector's others do not
([Waiting as Retry-After asks](REQUESTS.md#waiting-as-retry-after-asks)). Scrapes go through the same fetch, decode and
transform path as `/probe`, so collector limits, the response cache and
[`error_handling`](CONFIGURATION.md#when-a-stage-of-the-probe-fails) all
apply. A static target scrape and an identical `/probe` request share cache
entries, and under `log` or `ignore` a failed stage leaves the target up with
nothing of the collector's to serve, only its health series, as it answers a
probe `200` with an empty exposition. With [`cache.stale_if_error`](CONFIGURATION.md#serving-the-last-good-result-when-the-target-fails),
a failed scrape serves the target's last good result, marked by
`http_exporter_result_stale` 1, while its `http_exporter_target_up` is `0`.
Its `http_exporter_result_age_seconds` is how old the data is when the
endpoint is read, worked out at every read: it grows between scrapes, and
starts again from `0` at a scrape that goes to the target.

A collector with [`failure_backoff`](CONFIGURATION.md#backing-off-from-failing-targets)
stops contacting a target whose scrapes keep failing: once it is backing
off, the target's scrape fails at once, without waiting for a slot, as its
fetch would have — `up` 0, or its last good result marked stale — until the
wait has passed. A target that never answers then holds no slot of its
collector's `max_concurrent_probes` for its whole interval on every turn.
A static target and the identical `/probe` request share one backoff, as
they share cache entries.

A read of the endpoint does not merge the targets' results each time: the
merge is kept until a scrape publishes a result or a reload changes the
targets, so reads between two scrapes, as from several Prometheus replicas,
only write what the last one merged. Ages are still worked out at each read.

Static targets are not reachable through `/probe`.

## Environment variables

A target file carries the addresses and credentials of what is scraped, which
is exactly what is worth keeping out of a committed file. Run with
`--static-targets.expand-env` and `${NAME}` references in it are replaced from
the exporter's environment before it is parsed:

```yaml
interval: 1m
targets:
  - name: billing
    collector: billing_status
    target: ${BILLING_URL}
    request:
      bearer_token: ${BILLING_TOKEN}
    labels:
      region: ${REGION}
```

```sh
BILLING_URL=https://billing.internal BILLING_TOKEN=... REGION=eu \
  prometheus-universal-exporter --config.file=config.yaml \
    --static-targets-file=static-targets.yaml --static-targets.expand-env
```

It follows the configuration's [rules](CONFIGURATION.md#environment-variables):
only the braced `${NAME}` is a reference, `$$` writes a literal dollar, a
variable that is not set stops the exporter at startup — and fails
`--dry-run` — with every missing name listed, and a value arrives exactly
as the variable holds it, whatever characters it has, a `#`, quotes or line
breaks included. A reload expands the file again.

The flag is the file's own. `--config.expand-env` expands the configuration
and its collector files and leaves this file as written, and this flag leaves
them as written, so each is expanded only when asked for.

An environment reference is fixed when the file is read; it is not a
`{{param_…}}` placeholder, which a static target cannot use in its own values
(see [The target file](#the-target-file)). To fill a collector's placeholders, set
`params`, whose values may themselves come from the environment.

## The static targets endpoint

The latest result of every target is served at `--web.static-targets-path`,
`/static-targets` by default, in OpenMetrics when the scraper asks for it, as
Prometheus does, and in the text format otherwise (see
[OpenMetrics](CONFIGURATION.md#openmetrics)):

```text
# TYPE application_connections gauge
application_connections{region="eu",static_target="legacy_eu"} 42
application_connections{region="us",static_target="legacy_us"} 17
# HELP http_exporter_target_up Whether the last scrape of this static target succeeded.
# TYPE http_exporter_target_up gauge
http_exporter_target_up{collector="legacy_text",region="eu",static_target="legacy_eu",target="http://legacy.eu.example:8080"} 1
http_exporter_target_up{collector="legacy_text",region="us",static_target="legacy_us",target="http://legacy.us.example:8080"} 1
```

- Every series carries `static_target`, the target's name: the targets share
  one endpoint, and the same metric from two targets stays two series.
- The `target` label is the target as logs show it: a password in it, and the
  value of a query parameter named like a credential (`token`, `api_key`, …),
  are withheld, and the rest is as written. A target that names none, as one
  of a `localfile` collector that reads the collector's own path may, has no
  `target` label, where it used to have `target=""`.
- `http_exporter_target_up` and `http_exporter_target_scrape_duration_seconds`
  report each target's last scrape, so a failing target is visible rather
  than simply absent.
- `http_exporter_target_last_success_timestamp_seconds` is when the target was
  last scraped successfully, 0 until it has been, and is kept through later
  failures. Alert on its age to find a target that has given nothing new for
  too long, however its scrapes went:

  ```promql
  time() - http_exporter_target_last_success_timestamp_seconds > 3 * 3600
  ```
- What the endpoint serves of a target is what its last scrape left:
  - A scrape that succeeded leaves the target's metrics and its health
    series, with `http_exporter_target_up` 1.
  - A scrape that failed leaves the health series only, with
    `http_exporter_target_up` 0: the metrics of the scrape before it are no
    longer served, so values the target did not give are never presented as
    current, and Prometheus marks their series stale.
  - A scrape that failed on a collector with
    [`cache.stale_if_error`](CONFIGURATION.md#serving-the-last-good-result-when-the-target-fails)
    leaves the last good result in their place for as long as that setting
    keeps it, marked by `http_exporter_result_stale` 1 and aged by
    `http_exporter_result_age_seconds`, beside `http_exporter_target_up` 0;
    once it has expired, a failed scrape leaves the health series only.
  - A turn that publishes nothing — one skipped, or a scrape cut short by a
    shutdown — changes nothing: the result of the last scrape that ended,
    whether it succeeded or failed, is still served, and the last success
    timestamp says how old it is.
- A target that has not been scraped yet is not on the endpoint, and one
  removed from the file leaves it with the reload.
- The same metric from two targets must have one type. A target whose metric
  another target already serves with a different type has that metric left
  out, with a warning in the log, and the rest of both is served. The same
  goes for a metric named like a series of another target's histogram or
  summary — a gauge `x_sum` beside a histogram `x`, whichever target came
  first — since the endpoint cannot serve two families of one name. When the
  clash is gone, a `static target metric back on the static targets
  endpoint` line says so.
- Reading the endpoint contacts no target: it is as fast as the self-metrics,
  whatever the targets are doing, so Prometheus can scrape it on any interval.
  An interval shorter than the targets' serves the same values again; a longer
  one misses the values in between.
- `?targets=` narrows a read to the targets it names, separated by commas or
  with the parameter repeated: `/static-targets?targets=legacy_eu,legacy_us`
  serves those two and nothing of the rest. A name no target has, or a
  parameter that names none, is answered `400` saying which, so a misspelt or
  removed target fails the scrape rather than quietly serving nothing. A
  target named but not scraped yet is simply absent, as without the
  parameter. What is served of a target is exactly what the whole endpoint
  serves of it, a metric left out for its type included.
- The path must be one fixed path that no other endpoint uses, including
  `--web.self-metrics-path`; otherwise the exporter does not start.
- It is gzipped when asked, like `/probe`, and behind the exporter's
  [Basic Auth](AUTHENTICATION.md) when that is on.

Scrape it with a plain Prometheus job, keeping the series' own labels:

```yaml
scrape_configs:
  - job_name: static-targets
    metrics_path: /static-targets
    honor_labels: true
    static_configs:
      - targets: ['exporter:8080']
```

To read only some targets, list them under `params`; Prometheus repeats the
parameter for each, which the endpoint accepts as it does commas. With the Helm
chart, `staticTargets.monitor.targets` renders the same for its monitor. One job per
group of targets lets each be scraped on its own interval, or with its own
timeout:

```yaml
scrape_configs:
  - job_name: static-targets-eu
    metrics_path: /static-targets
    honor_labels: true
    params:
      targets: [legacy_eu, billing_eu]
    static_configs:
      - targets: ['exporter:8080']
```

`honor_labels` keeps `static_target`, `target` and the targets' own labels as
they are, rather than prefixed with `exported_` where Prometheus has a label of
the same name. Prometheus still adds `job` and `instance` for the endpoint,
which is why a target may not set either. With the Helm chart, `staticTargets.monitor` renders the
ServiceMonitor or PodMonitor that does this — see the
[chart README](../charts/prometheus-universal-exporter/README.md#static-targets).

## Debugging a static target

With `--web.debug=full` (the chart's `server.debug: full`),
`/static-targets?debug=<name>` scrapes the target of that name once, as its
schedule would — with its own request settings and credential, within its
interval — and answers with the same plain-text report as a
[debug probe](CONFIGURATION.md#debug-probes): the requests, the response,
each stage, the logs, the [metric lint](CONFIGURATION.md#metric-lint)'s
advice on the target's collector and on the series, and the series the
scrape would have published, with the target's labels and `static_target`,
and whether `http_exporter_target_up` would have been `1` or `0`. The report is bounded as a debug probe's is: a
long line, URL, header value or metric name is shown by its start and its
length, and where a line of the series is cut the report says that the
endpoint serves it whole.

```sh
curl 'http://exporter:8080/static-targets?debug=legacy_eu'
```

A debug scrape publishes nothing: the endpoint keeps serving the target's
last scheduled result, and nothing is counted in the self-metrics but
`http_exporter_debug_runs_total` or exported over OTLP. It skips the response cache like a debug probe, revalidating the response the scrape would without refreshing it ([Revalidating expired responses](CONFIGURATION.md#revalidating-expired-responses)), but waits for a
slot of the collector's `max_concurrent_probes` as the scrape would. A name no
target in force has is answered `404`; `debug` takes one name and cannot be
combined with `targets` (`400`); below `--web.debug=full` it is `403`. The
[debug page](CONFIGURATION.md#the-debug-page) makes the same scrape, and can
run another collector on what it fetched.

## Status page

For people who have no Prometheus to ask, the exporter serves a status page
at the static targets path followed by `/status`, `/static-targets/status` by
default; the landing page links it. It shows the static targets first, then
the recent probes and how the OTLP export is going, which
[Status page](ENDPOINTS.md#status-page) in Endpoints describes with the page
as a whole. The static targets section has a row per
target:

- its name, collector, target and labels, its interval, and whether it is
  exported over OTLP. The target is shown as its `target` label and the log
  show it, with a credential in its URL withheld;
- when its last scrape began, how long ago that was and how long it took;
- the result: `success`, `failed`, `stale result served` (failed, and the
  last good result served marked stale under `cache.stale_if_error`),
  `skipped by backoff` (`failure_backoff` kept the scrape from the target), or
  `not yet scraped`; and whether the response cache answered it;
- the consecutive failed scrapes, and while `failure_backoff` keeps scrapes
  from the target, until when, or that its next try is in progress, with the
  failures the backoff counts. A scrape skipped by backoff tried nothing: it
  is not counted as a failed scrape;
- the error of the last failed scrape and when that scrape began. It stays
  shown after the target recovers, so a failure in the night can be read in
  the morning, and while scrapes are skipped by backoff, so the error that
  made the exporter back off is the one shown. It is the error the log has,
  with every URL's credentials withheld, its userinfo too where the text
  only quotes the URL, anything else that reads as a credential masked, and
  cut to 2000 bytes. The values of the target's answer it quotes and what is
  for the operator alone, such as what an [`sshexec`](SSHEXEC.md) command
  wrote to stderr, are left out as a probe's caller is answered them
  ([What a failed probe's caller is told](CONFIGURATION.md#what-a-failed-probes-caller-is-told)),
  unless `--web.debug` is `full`: the page is read by whoever may call
  `/probe`. A scrape that panicked shows `the scrape panicked; the
  exporter's log has the panic`: the panic's own text is only in the log.

A summary line above says how many targets there are, how many are failing
and how many have not been scraped yet; the failing targets come first. The
page shows the moment it was served; its Refresh button and auto-refresh
selector fetch it again without reloading it (see
[Refreshing](ENDPOINTS.md#refreshing) in Endpoints), and
`--web.status-refresh` sets how often it refreshes itself when its URL does
not say.

`?format=json` serves the same as JSON, for a script:

```sh
curl 'http://exporter:8080/static-targets/status?format=json'
```

```json
{"generated_at": "2026-10-24T09:00:00Z", "allocator_mode": false, "targets": 2, "failing": 1, "not_yet_scraped": 0,
 "static_targets": [
  {"name": "legacy_eu", "collector": "legacy", "target": "http://legacy-eu:8080", "labels": {"region": "eu"},
   "interval": "1m0s", "export_via_otlp": false, "result": "skipped by backoff", "failing": true,
   "last_scrape": "2026-10-24T08:59:41Z", "last_scrape_age_seconds": 19.2, "duration_seconds": 0.0001,
   "cached": false, "stale": false, "consecutive_failures": 3,
   "last_error": "fetch http://legacy-eu:8080: received HTTP status 503",
   "last_error_at": "2026-10-24T08:58:41Z",
   "backoff": "backing off until 2026-10-24T09:02:00Z", "backoff_until": "2026-10-24T09:02:00Z", "backoff_trial": false, "backoff_failures": 3},
  {"name": "billing_eu", "...": "..."}
 ]}
```

A field without a value is left out: the scrape fields of a target not yet
scraped, `last_error` of one that never failed, `backoff` of one not backing
off. `consecutive_failures` counts the target's failed scrapes of every kind
but those skipped by backoff, `backoff_failures` the failed trips
`failure_backoff` counts (see
[Backing off from failing targets](CONFIGURATION.md#backing-off-from-failing-targets)).

What the page keeps of a target is forgotten when a reload, or a new bundle
from the allocator, removes the target, whether or not anything reads the
static targets endpoint: a target brought back under its name is
`not yet scraped`, with no failures and no error, until its first scrape.

The page shows nothing the endpoint's series and the log do not: it is
protected by `web.basic_auth` like every endpoint and answers `GET` and `HEAD`
only. While no static target is configured it is served all the same, for its
recent probes, and its static targets section says that none are configured.
In allocator mode it shows the targets the allocator gave this replica, and
says so: each replica has a page of its own. It moves with
`--web.static-targets-path`, so a `--web.self-metrics-path` on it is refused.

## Exporting over OTLP

A target with `export_via_otlp: true` is also delivered over
[OTLP](OTLP.md), on `otlp.interval`, besides being served on the endpoint:

```yaml
interval: 1m
targets:
  - name: legacy_eu
    collector: legacy_text
    target: http://legacy.eu.example:8080
    export_via_otlp: true
    otlp:
      service_name: legacy-app
      resource_attributes:
        deployment.environment: production
```

It is off by default. Each export delivers what the target's scrapes left
since the last one, the latest value of each series, with the target's health
metrics: a target scraped more often than `otlp.interval` exports only its
latest values; one scraped less often exports its last result again only when
scraped again.

`otlp.service_name` and `otlp.resource_attributes` set the OTLP resource the
target's metrics arrive under; both fall back to the exporter-wide `otlp`
settings, and per-target attributes are merged over the exporter-wide ones.
An attribute written `""` is the attribute left out: it sets nothing, so the
exporter-wide attribute of that name stays as it is.
Targets with different identities are exported as separate `resourceMetrics`
entries. The `otlp` block is only for a target with `export_via_otlp`, and
refused on any other. `resource_attributes` may not set `service.name`, which
`service_name` sets: the resource would carry it twice, so the file is
refused pointing at `service_name`. Over OTLP every series carries `static_target`, as on
the endpoint: targets often share a resource — the same collector, the
exporter-wide identity — and the label is what keeps their series apart.

`export_via_otlp` needs OTLP export: a target with it while `otlp.enabled` is
`false`, or without an `otlp.endpoint`, makes the exporter log
`invalid static target configuration; exiting` and stop.
`configs/config.otlp.example.yaml` is the example configuration with OTLP
export enabled that goes with `configs/static-targets.example.yaml`.

## Reloading

The file is reloaded on the same terms as the exporter configuration — on
`SIGHUP`, `POST /-/reload` and, with `--config.watch`, when it changes (see
[Reloading on demand](CONFIGURATION.md#reloading-on-demand) and
[Watching the configuration](CONFIGURATION.md#watching-the-configuration)). An invalid
document, or a configuration change that would disable OTLP export while a
loaded target sets `export_via_otlp`, is rejected, and the last valid pair
stays active. A target the reload changed starts again — first scraped within
ten seconds, then on a cadence of its own — once a scrape begun on the old
definition has ended, so two never overlap, and a fixed address or credential
shows within seconds rather than at the old cadence's next turn. So does every
target of a collector the reload changed in any way — a rule, a label, its
request — although the target file is as it was: the endpoint serves what the
new definition makes within ten seconds, not what the old one made until the
target's next turn. A target that did not change, of a collector that did not
change, keeps its cadence; a
target removed while its scrape runs publishes nothing, and neither does one
the reload changed, or whose collector it changed or removed: that scrape read
a definition that is gone, and its result would stand for the target now
under the name. That holds when a later reload has put the target back as it
was, too, so the result of a scrape begun before the reloads never replaces
that of the target's first scrape after them. The target put back, or changed
back, starts again like one that changed, however soon the second reload
follows the first: it is first scraped within ten seconds, once the scrape
begun before the reloads has ended, so it is not missing from the endpoint
until its next turn. Its failures are logged anew as well: what the
[failure log](LOGGING.md#repeated-failures) remembered of a target the reload
removed or changed is forgotten, and a scrape begun before the reload neither
adds to it nor is logged as the target's recovery.

A rejected reload is tried again when the file changes. With
`--config.watch` it is also tried again, the file untouched, when a file its
check opened changes: the descriptor files (`request.protoset_file`,
`request.proto_files`, and the files those `.proto` files import) of a grpc
collector whose target sets a `request.message`. A reload that ran while such
a file was being replaced
thus goes through at the tick after it is in place (see
[Watching the configuration](CONFIGURATION.md#watching-the-configuration)).
A rejected target file is not read for any other descriptor file: one
rejected because it names a collector the configuration does not have is
read again when it or the configuration file changes, and a descriptor file
changing meanwhile reloads the configuration alone.
The same files are watched while the target file is in force: when one
changes, the next tick reads the target file again with the configuration,
although neither changed, and a `request.message` that the new descriptors
refuse is a rejected reload of the target file, logged with the target and
the reason, rather than only scrapes that fail (see
[Descriptor files](CONFIGURATION.md#descriptor-files)). When the message
still fits, the reload changes nothing: every target keeps its cadence.
A target's credential files are read at each scrape, so one that is missing
fails that target's scrapes, not the reload.

The configuration and the target file take effect as one pair. A reload that
changes both — a collector renamed in the configuration and in the targets
that use it — puts both in force in one step, and every scrape uses the
configuration its target was read with, so no scrape runs a target of one
reload against the collectors of another. A scrape already waiting or running
when the reload comes finishes on the pair it started with; its target is
then scraped again as above if the reload changed it or its collector, and
what the earlier scrape made is not published.

On `SIGTERM` or `SIGINT` the targets keep being scraped through
`--web.shutdown-delay`, while their endpoint is still served. Then no scrape
starts, not one that was waiting for a slot and gets it then either, and
those in flight finish within `--web.shutdown-timeout`; one it
cuts short publishes nothing and logs no failure, so a restart never reports
a target down, over OTLP or anywhere else, and the target's last result
stands.

## Self-metrics

Static target scrapes are counted in the per-collector
[self-metrics](SELF-METRICS.md), as probes are, rather than in per-target
series; the endpoint's health metrics are the per-target view.
`http_exporter_static_targets` reports how many targets are loaded, and
`http_exporter_static_targets_exported_via_otlp` how many of them are also
exported over OTLP. The file's reloads are reported under
`file="static_targets"` in the [reload metrics](SELF-METRICS.md#configuration-reloads).
