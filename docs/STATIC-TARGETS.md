# Static targets

Besides answering `/probe`, the exporter can scrape a fixed list of targets
itself, each on its own interval, and serve their latest results together on
one endpoint, which Prometheus scrapes like any other exporter's metrics. A
target can also be delivered over OTLP.

Use static targets when the list of targets is known and fixed, and a
Prometheus job per target, or service discovery and relabelling for the
probe pattern, would be more machinery than the list needs. For targets that
Prometheus discovers, keep using `/probe`.

## The target file

Pass the list with `--static-targets-file`.
`configs/static-targets.example.yaml` is a complete example.
`configs/static-targets.schema.json`, printed by
`--static-targets-file-schema`, is the file's JSON Schema for editors. Start
a target file with

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
`interval` and `targets` are required, and the file refuses unknown keys.

A target's `target` is checked when the file loads, as its collector's type
reads it. An `http` or `graphite` target is an absolute URL. A target whose
port is 0 or above 65535, which no connection can use, is refused with an error that names the port; this applies to a `grpc`, `sshfile`, `sshexec`, `sql`, `redis`, `tcp`, `ldap`, `tlscert`, `ntp`, `snmp`, `smtp`, `modbus`, `mqtt`, `websocket`, `ftp`, `imap`, `pop3`, `radius`, `sip`, `amqp`, `nats` or `kafka` target too.

Each target names a collector from the exporter configuration and takes
every per-scrape parameter that `/probe` accepts (`method`, `path`, `body`,
`timeout`, `insecure_skip_verify`, `follow_redirects`, `enable_http2`,
`new_connection`, `ip_family` and the `retry` settings), which override the
collector's own request for that target only. Which of these keys a target
may set depends on its collector's
[request type](CONFIGURATION.md#request-types), as the table shows. A key
that the type does not take is refused when the file loads, with an error
that names the target. Of several such keys, including a key of `retry` for
other types, the first in the file is named. (An unknown key is refused as
soon as the file is read, before its collector is looked up.)

| Type | `target` | What a target's `request` may set |
| --- | --- | --- |
| `http` | an absolute URL | `method`, `path`, `body`, `timeout`, `insecure_skip_verify`, `follow_redirects`, `enable_http2`, `new_connection`, `ip_family`, `retry`, `headers`, [`accept_status`](REQUESTS.md#accepting-other-statuses), [`rate_limit`](REQUESTS.md#rate-limiting), its own credentials: `basic_auth`, `basic_auth_file`, `bearer_token`, `bearer_token_file` and [`login`](AUTHENTICATION.md#logging-in-to-a-target), and its own `tls` |
| [`graphite`](GRAPHITE.md#static-targets) | the Graphite server's URL | all `http`'s keys but `method` and `body`, plus its own `targets`, `from` and `until` |
| [`grpc`](GRPC.md#static-targets) | the server | `message`, `metadata`, `timeout`, `insecure_skip_verify`, `retry`, with its `codes`, [`accept_codes`](GRPC.md#errors-and-retries), its own credentials, `basic_auth`, `basic_auth_file`, `bearer_token` and `bearer_token_file`, and its own `tls` |
| [`localfile`](LOCALFILE.md#static-targets) | a file under `request.root`, optional | `path` and `timeout` |
| [`sshfile`](SSHFILE.md#static-targets) | the host | `path`, `timeout`, and its own account and credential files: `username`, `private_key_file`, `passphrase_file` and `password_file` |
| [`sql`](SQL.md#static-targets) | the database server, or a `snowflake` account | `timeout`, its own `username` and `password_file` or `private_key_file`, its own `tls`, and `connections` but `max_targets`, a target whose `connections` change a setting being a target of its own for the collector's limits |
| [`dns`](DNS.md#static-targets) | the name to resolve | `timeout` |
| [`tlscert`](TLSCERT.md#static-targets) | the server | `timeout` |
| [`ntp`](NTP.md#static-targets) | the server | `timeout` |
| [`kubernetes`](KUBERNETES.md#static-targets) | none at all | `timeout` |
| [`s3file`](S3FILE.md#static-targets) | the bucket, required | `path` and `timeout` |
| [`redis`](REDIS.md#static-targets) | the server, required | `timeout`, and its own account and TLS settings: `username` and `password_file`, and `tls` |
| [`tcp`](TCP.md#static-targets) | the service, `host:port`, or a host alone with `ports` or `unix_socket`, required | `timeout`, and its own `tls` |
| [`ldap`](LDAP.md#static-targets) | the server, required | `timeout`, and its own account and TLS settings: `bind_dn` and `password_file`, and `tls` |
| [`sshexec`](SSHEXEC.md#static-targets) | the host, required | `timeout`, and its own account and credential files: `username`, `private_key_file`, `passphrase_file` and `password_file`; nothing of the command |
| [`icmp`](ICMP.md#static-targets) | the host, required | `timeout` |
| [`snmp`](SNMP.md#static-targets) | the agent, required | `timeout`, `request_timeout` and `retries`, and its own account: `community_file`, or of v3 `username`, `auth_password_file` and `priv_password_file` |
| [`smtp`](SMTP.md#static-targets), opt-in | the mail server, required | `timeout`, and its own account and TLS settings: `username` and `password_file`, and `tls` |
| [`modbus`](MODBUS.md#static-targets), opt-in | the device, with its unit when it is not the collector's, required | `timeout` |
| [`mqtt`](MQTT.md#static-targets), opt-in | the broker, required | `timeout`, and its own account and TLS settings: `username` and `password_file`, and `tls` |
| [`websocket`](WEBSOCKET.md#static-targets), opt-in | the endpoint's URL, required | `timeout`, and its own `tls` |
| [`ftp`](FTP.md#static-targets), opt-in | the server, required | `path` and `timeout`, and its own account and TLS settings: `username` and `password_file`, and `tls` |
| [`imap`](IMAP.md#static-targets), opt-in | the mail server, required | `timeout`, and its own account and TLS settings: `username` and `password_file`, and `tls` |
| [`pop3`](POP3.md#static-targets), opt-in | the mail server, required | `timeout`, and its own account and TLS settings: `username` and `password_file`, and `tls` |
| [`radius`](RADIUS.md#static-targets), opt-in | the RADIUS server, required | `timeout` |
| [`whois`](WHOIS.md#static-targets), opt-in | the domain, required | `timeout` |
| [`sip`](SIP.md#static-targets), opt-in | the SIP server, required | `timeout` |
| [`amqp`](AMQP.md#static-targets), opt-in | the AMQP broker, required | `timeout` |
| [`nats`](NATS.md#static-targets), opt-in | the NATS server, required | `timeout` |
| [`kafka`](KAFKA.md#static-targets), opt-in | a broker of the cluster, required | `timeout` |

**Credentials.** An `http` or `graphite` target also takes static `headers`,
and an `http`, `graphite` or `grpc` target its own target credentials, inline
or file-backed, as basic authentication or a bearer token.

- A basic username holds no colon. One written in the target is refused at
  startup, and one read from its file fails the target's scrape
  ([Authentication](AUTHENTICATION.md#credentials-in-the-configuration)).
- A target's own credential is sent instead of the collector's, whose
  `bearer_token_file` or `basic_auth_file` is then not read for that target.
  A missing collector credential file therefore fails only the targets that
  would have sent it.

**Login.** An `http` or `graphite` target may also set its own
[`login`](AUTHENTICATION.md#logging-in-to-a-target), checked like a
collector's. It replaces the collector's login for that target, and the
collector's credential with it when both are sent in `Authorization`. A
target that sets its own credential in the header that the collector's login
sends its token in makes no login. A target's `login` is part of the key
that its answers are cached under, so targets that log in differently share
no answer.

**Rate limit.** An `http` or `graphite` target may set its own
[`rate_limit`](REQUESTS.md#rate-limiting), checked like a collector's, which
replaces the collector's for that target's requests (both keys together).
Those requests have their own limit for each origin that they go to,
separate from the one that the collector's other targets share there. They
also take their turns under the configuration's
[`rate_limits`](REQUESTS.md#rate-limits-per-host), like every collector's
requests. A rate limit changes no answer, so it is not part of the cache
key.

**Proxy.** A target cannot set a
[`proxy_url`](REQUESTS.md#a-collectors-own-proxy). Its requests go through
its collector's proxy, and a target behind another proxy needs its own
collector.

**Statuses.** An `http` or `graphite` target may set its own
[`accept_status`](REQUESTS.md#accepting-other-statuses), and a `grpc` target
its own [`accept_codes`](GRPC.md#errors-and-retries), which replace the
collector's for that target; no probe parameter can do this. A health
endpoint that answers `503` with a useful body on one host only can then
accept it there.

Because the file is operator configuration and not caller input, these
headers are applied directly and are not filtered through the collector's
`request.forward_headers` allowlist.
<!-- /request-types -->

A collector's [`{{param_…}}` placeholders](REQUESTS.md#path-parameters) are
filled by the target's `params`, because there is no probe to supply
`param_<name>`. This covers the placeholders in its path, body, header
values, query values, Graphite targets and gRPC message and metadata values,
and in its [label values](REQUESTS.md#in-label-values), `transform.labels`
and a rule's static `value`:

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

Every placeholder must be filled, by `params` or a default, and every entry
of `params` must fill one. Otherwise the exporter refuses to start, with an
error that names the target, the collector and the parameter. A target's own
`target`, `request.path`, `body` and `headers` are written out in full,
without placeholders. A value of `params` may hold
[clock placeholders](#dates-and-times-in-a-parameter), filled in at each
scrape.

One collector whose labels take a parameter can therefore serve several
targets, each labelled with its own value:

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
written there is exported as text.

A `{{param_…}}` placeholder anywhere that a target writes its own value (its
`target`, `request.path`, `request.body` or a header value) is refused when
the file loads, with an error that names the target and the field. There is
no probe to fill it, and it would reach the target as text.

- `{{` is also refused in an `sshfile`, `sql`, `redis` or `ldap` target's
  account (`username`, `bind_dn` and the credential files) and in a target's
  own `tls` block, which are used as written.
- Braces that do not open `{{param_` are left alone, so a JSON body is
  written as usual.
- The same applies to a value that an environment variable supplies,
  because the file is checked after it is expanded.

**Labels.** `labels` are added to every metric that the target produces,
without overwriting a label that the collector already extracted.

- A label written `""` is left out: it is added to no series, including the
  target's health series, where it used to be exported as `team=""`.
- A target's `le` is left off a histogram and its `quantile` off a summary,
  whose buckets and quantiles already carry that label.
- `static_target` is the endpoint's own label, and `job` and `instance` are
  Prometheus's, set when it scrapes the endpoint, so a target may set none
  of the three. Kept as the series' own labels when the endpoint is scraped,
  a target's `job` would move its series out of the job that scrapes it.
  Give such a label another name, such as `task`.
- A label's name is a classic Prometheus name (letters, digits and `_`),
  whatever the collector's
  [`name_escaping`](CONFIGURATION.md#utf-8-names): the labels are added to
  what the collector exported, after its names were escaped.
- Because the labels are added after the collector's series are checked, a
  label name longer than the collector's
  [`limits.max_label_name_length`](CONFIGURATION.md#long-label-names) is
  refused when the file is loaded. So is a target of a collector whose limit
  is under 13 bytes, the length of the `static_target` label that the
  endpoint adds, beside `collector` and `target`, on the health series.

**Values.** A key, a list entry or a label written with no value at all
(nothing after the colon or the dash, `null` or `~`) is refused, as it is in
the [configuration](CONFIGURATION.md#checked-when-the-configuration-loads),
with an error that names its line. Leave the key out for its default. Such a
key used to be taken as left out, and a `request.path` or `request.body`
written that way as `""`, which replaces the collector's.

A value is written as the kind that its key takes, as in the
[configuration](CONFIGURATION.md#checked-when-the-configuration-loads).

- A target's `name` and `request.method`, and an entry of
  `request.accept_codes` and `request.retry.codes`, are text only. A number
  or a boolean there is refused, with a hint to quote it if that text is
  meant (`name: "1"`).
- A label, a parameter, a header, a path, a body or the `collector` takes a
  number or a boolean as the text it spells, so `labels: {shard: 3}` needs
  no quotes.
- A boolean is `true` or `false`: `export_via_otlp: yes` is refused, with a
  hint to write `true`.

Targets that share a collector, a target or labels can write them once with
a YAML anchor under a top-level `x-` key, which the exporter ignores, and
merge the anchor into each target (see
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

That is
[`examples/open-meteo/static-targets.yaml`](../examples/open-meteo/static-targets.yaml),
shortened. The configuration with its collector is
[`config.yaml`](../examples/open-meteo/config.yaml) next to it. The two run
together as
`prometheus-universal-exporter --config.file=examples/open-meteo/config.yaml --static-targets-file=examples/open-meteo/static-targets.yaml`,
which `make run EXAMPLE=open-meteo` builds and runs.

A merge key works inside a target's `request` too
(`request: {<<: *request, timeout: 2s}`), and a `path` or `body` that it
brings in replaces the collector's, as one written there does.

A target may also set `shard_key`, which the exporter ignores. It is for the
[target allocator](ALLOCATOR.md#static-targets), which hashes it instead of
the target's name to choose the replica that scrapes the target, so targets
that share a key share a replica.

**Allocator keys.** The exporter discovers no targets. A file with the
allocator's `discovered_targets` key
([Discovering targets](ALLOCATOR.md#discovering-targets)) is refused, with
an error that names the allocator, because the exporter alone would scrape
none of what it describes. So is a file with the allocator's weights,
`default_weight` at its top level or `weight` or `weight_label` on a target
([Weights and capacity](ALLOCATOR.md#weights-and-capacity)): the exporter
scrapes every target of its file, and the allocator removes the weights
before it hands a replica its share. The allocator writes each discovered
target out as a separate target before it hands a replica its share. The
file's schema, `configs/static-targets.schema.json`, refuses these keys too.
The allocator's file has its own schema that accepts them,
[`configs/allocator-static-targets.schema.json`](../configs/allocator-static-targets.schema.json).

Check a target file together with its configuration before deploying it:
`prometheus-universal-exporter --dry-run --config.file=config.yaml --static-targets-file=static-targets.yaml`
reports whether each would load, including whether every target names a
collector that exists and whether a target exported over OTLP has an OTLP
export to go to. See [Dry run](CONFIGURATION.md#dry-run).

### Dates and times in a parameter

A value of `params` may hold
[clock placeholders](REQUESTS.md#dates-and-times), which each scrape of the
target fills in, from the same moment as the collector's own. One collector
that asks for a day can then serve targets that each want their own day, in
their own time zone.

```yaml
# the collector:  request: {type: http, path: "/summary/{{param_day}}"}
interval: 1h
targets:
  - name: sofia
    collector: daily_summary
    target: https://api.example
    params: {param_day: "{{date@Europe/Sofia}}"}
  - name: new_york
    collector: daily_summary
    target: https://api.example
    params: {param_day: "{{date@America/New_York}}"}
```

A value may hold text around them, `day-{{date:-1d}}`, and a
[`~last_success`](REQUESTS.md#since-the-last-success) placeholder, which
starts from the last successful scrape of that target.

- A value may hold no `{{param_…}}` placeholder, which nothing would fill,
  and no filter: the collector's placeholder writes the value as it writes
  every value.
- A parameter that fills a label value of the collector may hold no clock
  placeholder, because a label whose value changes with the clock is a new
  series every day, hour or second.
- Each of these is refused when the file loads, with an error that names the
  target and the parameter.
- When the file loads, the value is checked where the collector writes it,
  as it would be written at 13:04:05 on 9 November 2026. A `date_us` value,
  with its slashes, is therefore refused in a Graphite expression as the
  date itself would be.
- A target's own `target`, `request.path`, `body` and `headers` are written
  out as they are, including any clock placeholders.

## Credentials and TLS of a target

One collector can serve several servers that each require their own
password or client certificate: a target names its own account, or its own
`tls` block, instead of the collector's. This is what each type accepts:

| Type | A target's own account | A target's own `tls` |
| --- | --- | --- |
| `http`, `graphite` | `basic_auth`, `basic_auth_file`, `bearer_token`, `bearer_token_file`, `login`, each replacing the collector's as above | yes, or `insecure_skip_verify` alone |
| `grpc` | `basic_auth`, `basic_auth_file`, `bearer_token`, `bearer_token_file` | yes |
| `sshfile`, `sshexec` | `username`, and `private_key_file`, `passphrase_file` and `password_file`, any of the three replacing all three of the collector's | no; the host is verified with the collector's `known_hosts_file` |
| `sql` | `username`, and `password_file` or, for `snowflake`, `private_key_file`, either replacing the collector's credential file | yes |
| `redis` | `password_file`, replacing the collector's, and `username`, which needs the target's own `password_file` | yes |
| `ldap` | `password_file`, replacing the collector's, and `bind_dn`, which needs the target's own `password_file` | yes |
| `tcp` | none: a tcp service is asked no password | yes |
| `smtp`, `imap`, `pop3` | `password_file`, replacing the collector's, and `username`, which needs the target's own `password_file`; under a collector that logs in with a token, `username` alone, the mailbox the collector's token logs in to, and no `password_file` | yes |
| `mqtt`, `ftp` | `password_file`, replacing the collector's, and `username`, which needs the target's own `password_file` | yes |
| `websocket` | none | yes |
| `modbus` | none | no; Modbus TCP has no TLS |
| `snmp` | `community_file` of v1 and v2c; `username`, `auth_password_file` and `priv_password_file` of v3, each replacing the collector's as its security level uses it | no; SNMP has no TLS |
| `radius` | none: the account and the shared secret are the collector's | no; RADIUS is spoken over UDP |
| `sip`, `amqp`, `nats`, `kafka` | none: the account and the `tls` block are the collector's | no |
| `localfile`, `s3file`, `dns`, `tlscert`, `ntp`, `icmp`, `kubernetes`, `whois` | none | no |

A target's credential file may be a reference to a
[secret store](AUTHENTICATION.md#secret-stores) of the configuration,
`password_file: secret://vault-prod/secret/data/db#password`, as a
collector's may.

A target's `tls` replaces the collector's whole block. A key that it leaves
out is not taken from the collector's, so a target with its own client
certificate also names the CA that it verifies the server with.

- A `tls` block, even `tls: {}`, requests TLS, as the collector's block
  does. A target can therefore request TLS that its collector does not, but
  cannot drop the TLS that its collector requests.
- It is checked when the file loads, like the collector's: a client
  certificate needs both `cert_file` and `key_file`. A target with its own
  block whose address requests plaintext (`redis://`, `ldap://` without the
  collector's `start_tls`, `grpc://`) is refused, as is one for which the
  `sql` driver's options switch TLS off.

For `http` and `graphite`, the scheme of the target URL decides whether TLS
is used. `tls: {}` therefore verifies an `https://` target with the system
roots only, and a block on an `http://` target is used, like the
collector's, for the `https` requests that its trips make.

- The target's block is used for every request of its trips: the target's
  request and its pages, each hop of a followed redirect, and its
  [login](AUTHENTICATION.md#logging-in-to-a-target), unless the `login`
  block names its own `tls`.
- Its client certificate is a credential, like the collector's. A redirect
  over `https` to a host that is not the target's origin and not in
  `redirect_trusted_hosts` is refused instead of being shown the certificate
  ([What a followed redirect carries](REQUESTS.md#what-a-followed-redirect-carries)).
- Beside the block, the target's `insecure_skip_verify` is refused: write it
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

Only a static target sets these keys. The file belongs to the operator, and
no `/probe` parameter sets an account, a credential file or a `tls` block,
so a probe always reads as its collector does. A target's own files are read
when its scrape needs them, at each scrape or connection like the
collector's, so a rotated password is used without a reload. `--dry-run`
reads them too and warns, under the target's name, about one that a scrape
would fail on now. A debug report masks a target's password as it masks the
collector's.

A target's account and `tls` block are part of the key of everything that
distinguishes requests:

- the key under which its answers are
  [cached](CONFIGURATION.md#response-caching);
- the key of the trip that identical scrapes and probes share;
- the key by which
  [`failure_backoff`](CONFIGURATION.md#backing-off-from-failing-targets)
  backs off;
- for the types that keep connections, the key of the `sql` pool, the
  `grpc` connection, the `sshfile` connection and the `http`
  [connection pool](REQUESTS.md#connections) that it uses, and of the token
  that an `http` or `graphite` login keeps for it.

Two targets with different accounts, or with different `tls` blocks, never
share an answer, a trip, a backoff, a connection or a login's token, even
with one collector and one server.

## Scraping

Each target is scraped on its own `interval`, as Prometheus scrapes a probe
on its `scrape_interval`. Neither Prometheus's scrapes of the endpoint nor
the OTLP export decide when a target is scraped: the endpoint serves, and the
export delivers, what the last scrape of each target left.

Every running exporter scrapes every target in its file. Run one replica for
a static target file, without a HorizontalPodAutoscaler. Each additional
replica contacts every target again on the same interval, serves its own
results on the endpoint, and exports every target with `export_via_otlp`
again, so the OTLP backend receives duplicate series. To spread many
targets, split them into files served by separate exporters. The Helm chart
warns when it renders static targets with more than one replica or with
autoscaling (see
[Autoscaling](../charts/prometheus-universal-exporter/README.md#autoscaling)).

**The schedule.** A target is first scraped within ten seconds of the
exporter starting, or of the reload that added it or changed it in any way:
its address, its request, its params, its interval, or anything in the
definition of its collector. It therefore appears on the endpoint promptly
even with an interval of an hour.

- After that it keeps a cadence at a point within its interval that its name
  determines, so targets that share an interval are spread over it instead
  of all being scraped at once. The cadence starts at the first such point
  at least one whole interval after the first scrape was due (between one
  interval and two after it), and then continues every interval, however
  long a scrape takes.
- With an interval of ten seconds or less, the first scrape itself is due at
  such a point, so the second scrape comes one interval after it.
- The interval is counted from the time the first scrape was due, not from
  the moment the exporter actually started it, which is always a little
  later. It is counted from when the first scrape began only in two cases:
  for a changed target whose first scrape had to wait for a scrape begun on
  its old definition, and for a first scrape started more than a tenth of
  the interval late, as after the process was held up.
- The first scrape has its interval to end in, like every scrape, so it has
  ended when the cadence starts. A target whose scrapes take most of their
  interval (26 seconds of 30) is therefore not cut short and reported down
  after a start or a reload.

**The deadline.** A scrape must end within its interval; one that does not
fails with an error saying that the scrape ran out of its interval.
`request.timeout` therefore may not be longer than the interval. Retries
(the target's `request.retry`, or else the collector's) whose waits alone
fill the interval (`attempts` × `backoff`) are refused, because the last of
them could never be made. Retries that fit can still be cut short by slow
attempts. The interval is at least `1s`.

**Overlap.** Two scrapes of a target never run at the same time. A scrape
that uses its whole interval, as every scrape of a target that never answers
does, ends just after the target's next turn has come.

- That turn is not given up. It waits for the scrape, starts as soon as the
  scrape has ended, and must itself end by the following turn, so it has
  slightly less than the interval.
- The scrape before it overran only slightly: either a scrape of the steady
  cadence, or the first scrape, which overruns into the cadence by no more
  than the delay with which it was started.
- A target that hangs is therefore tried once per interval, each failure
  logged as `static target scrape failed`, and is seen to answer again
  within one interval.
- A turn is skipped, with a `static target scrape skipped` warning, only
  when it is really lost: the scrape before it was still running when the
  following turn came (a scrape held by something that its deadline does
  not end, or an exporter short of CPU), or no scrape slot became free
  within the interval.

**Concurrency.** At most `concurrency` targets are scraped at once, 8
unless the file says otherwise. A target due while all slots are busy waits
for a slot within its interval, and is skipped, with a warning, if none
becomes free. Each target also waits for a slot of its collector's
`max_concurrent_probes`, which it shares with the probes. A reload that
changes `concurrency` applies to the scrapes that start after it, and to
those waiting.

The slots are shared between the collectors that the file names, so that one
whose targets are slow, or hang until their deadline, cannot hold them all
and leave every other collector's targets skipped turn after turn:

- Each collector may hold its share, `concurrency` divided by the number of
  collectors that the file names, at least one, whenever its targets are
  due.
- Beyond its share, a collector takes a slot only while that leaves one free
  for every other collector that holds none, and at most half the slots. A
  collector whose target becomes due therefore finds a slot immediately,
  however the other collectors' targets behave.
- A freed slot goes to the waiting target whose collector holds the fewest.

A file with one collector uses every slot, as do collectors that are busy
within their shares. With 100 `http` targets and 100 `sql` targets, eight
slots and the database hanging, the `sql` targets hold at most seven slots,
and the `http` targets keep being scraped. With more collectors than slots,
each holds at least one, and one busy collector alone takes half of them.
Set `concurrency` to at least the number of collectors that the file names
so that each has its own share.

**Retries.** Retries come from the collector's `request.retry`, and each key
that a target's own `request.retry` sets replaces the collector's, as the
`retry_attempts` and `retry_backoff` probe parameters each replace one. A
target that sets only `attempts: 3` keeps the collector's `backoff` and
`non_idempotent`.

- An `http` or `graphite` target's `retry.statuses` replaces the
  collector's list, or the default set, as a whole, as the collector's own
  replaces the default ([Retries](REQUESTS.md#retries)). An entry that the
  `accept_status` in force for the target (its own, or else the
  collector's) accepts as a whole is refused, because it could never be
  retried.
- Its `retry.retry_after` replaces the collector's in either direction. One
  target known to send a useful `Retry-After` can therefore have it read
  while the collector's other targets do not
  ([Waiting as Retry-After asks](REQUESTS.md#waiting-as-retry-after-asks)).

**The same path as `/probe`.** Scrapes go through the same fetch, decode and
transform path as `/probe`, so collector limits, the response cache and
[`error_handling`](CONFIGURATION.md#when-a-stage-of-the-probe-fails) all
apply.

- A static target scrape and an identical `/probe` request share cache
  entries.
- Under `log` or `ignore`, a failed stage leaves the target up with nothing
  of the collector's to serve, only its health series, as it answers a probe
  with `200` and an empty exposition.
- With
  [`cache.stale_if_error`](CONFIGURATION.md#serving-the-last-good-result-when-the-target-fails),
  a failed scrape serves the target's last good result, marked by
  `http_exporter_result_stale` 1, while its `http_exporter_target_up` is
  `0`. Its `http_exporter_result_age_seconds` is the age of the data when
  the endpoint is read, computed at every read: it grows between scrapes,
  and starts again from `0` at a scrape that contacts the target.

A collector with
[`failure_backoff`](CONFIGURATION.md#backing-off-from-failing-targets) stops
contacting a target whose scrapes keep failing. Once it is backing off, the
target's scrape fails immediately, without waiting for a slot, as its fetch
would have (`up` 0, or its last good result marked stale), until the wait
has passed. A target that never answers then does not hold a slot of its
collector's `max_concurrent_probes` for its whole interval on every turn. A
static target and the identical `/probe` request share one backoff, as they
share cache entries.

A read of the endpoint does not merge the targets' results each time. The
merged result is kept until a scrape publishes a result or a reload changes
the targets, so reads between two scrapes, as from several Prometheus
replicas, only write what the last merge produced. Ages are still computed
at each read.

Static targets are not reachable through `/probe`.

## Environment variables

A target file carries the addresses and credentials of what is scraped,
which is exactly what should be kept out of a committed file. With
`--static-targets.expand-env`, `${NAME}` references in it are replaced from
the exporter's environment before the file is parsed:

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

It follows the configuration's
[rules](CONFIGURATION.md#environment-variables):

- Only the braced `${NAME}` is a reference, and `$$` writes a literal dollar.
- A variable that is not set stops the exporter at startup, and fails
  `--dry-run`, with every missing name listed.
- A value arrives exactly as the variable holds it, whatever characters it
  has, including a `#`, quotes or line breaks.
- A reload expands the file again.

The flag belongs to this file only. `--config.expand-env` expands the
configuration and its collector files and leaves this file as written, and
this flag leaves them as written, so each is expanded only when requested.

An environment reference is fixed when the file is read. It is not a
`{{param_…}}` placeholder, which a static target cannot use in its own
values (see [The target file](#the-target-file)). To fill a collector's
placeholders, set `params`, whose values may themselves come from the
environment.

## The static targets endpoint

The latest result of every target is served at `--web.static-targets-path`,
`/static-targets` by default. It is in OpenMetrics when the scraper asks for
it, as Prometheus does, and in the text format otherwise (see
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
  one endpoint, and the same metric from two targets remains two series.
- The `target` label is the target as the logs show it: a password in it,
  and the value of a query parameter named like a credential (`token`,
  `api_key`, …), are withheld, and the rest is as written. A target that
  names none, as one of a `localfile` collector that reads the collector's
  own path may, has no `target` label, where it used to have `target=""`.
- `http_exporter_target_up` and
  `http_exporter_target_scrape_duration_seconds` report each target's last
  scrape, so a failing target is visible instead of simply absent. They are
  what [`report_failures`](CONFIGURATION.md#reporting-failed-probes-as-metrics)
  gives a probe, so that key changes nothing here: a target of a collector
  that sets it is served like any other.
- `http_exporter_target_last_success_timestamp_seconds` is when the target
  was last scraped successfully, 0 until it has been, and is kept through
  later failures. Alert on its age to find a target that has produced
  nothing new for too long, however its scrapes went:

  ```promql
  time() - http_exporter_target_last_success_timestamp_seconds > 3 * 3600
  ```
- What the endpoint serves of a target is what its last scrape left:
  - A successful scrape leaves the target's metrics and its health series,
    with `http_exporter_target_up` 1.
  - A failed scrape leaves only the health series, with
    `http_exporter_target_up` 0. The metrics of the previous scrape are no
    longer served, so values that the target did not give are never
    presented as current, and Prometheus marks their series stale.
  - A failed scrape on a collector with
    [`cache.stale_if_error`](CONFIGURATION.md#serving-the-last-good-result-when-the-target-fails)
    leaves the last good result in their place for as long as that setting
    keeps it, marked by `http_exporter_result_stale` 1 and aged by
    `http_exporter_result_age_seconds`, beside `http_exporter_target_up` 0.
    Once it has expired, a failed scrape leaves only the health series.
  - A turn that publishes nothing (a skipped turn, or a scrape cut short by
    a shutdown) changes nothing. The result of the last scrape that ended,
    successful or failed, is still served, and the last success timestamp
    shows how old it is.
- A target that has not been scraped yet is not on the endpoint, and one
  removed from the file disappears from it with the reload.
- The same metric from two targets must have one type. A target whose metric
  another target already serves with a different type has that metric left
  out, with a warning in the log, and the rest of both is served. The same
  applies to a metric named like a series of another target's histogram or
  summary (a gauge `x_sum` beside a histogram `x`, whichever target came
  first), because the endpoint cannot serve two families with one name. When
  the clash is gone, a `static target metric back on the static targets
  endpoint` line says so.
- Reading the endpoint contacts no target. It is as fast as the
  self-metrics, whatever the targets are doing, so Prometheus can scrape it
  on any interval. An interval shorter than the targets' serves the same
  values again, and a longer one misses the values in between.
- `?targets=` narrows a read to the targets that it names, separated by
  commas or with the parameter repeated:
  `/static-targets?targets=legacy_eu,legacy_us` serves those two and nothing
  of the rest. A name that no target has, or a parameter that names none, is
  answered `400` with the reason, so a misspelt or removed target fails the
  scrape instead of silently serving nothing. A named target that has not
  been scraped yet is simply absent, as without the parameter. What is
  served of a target is exactly what the whole endpoint serves of it,
  including a metric left out for its type.
- The path must be one fixed path that no other endpoint uses, including
  `--web.self-metrics-path`; otherwise the exporter does not start.
- It is gzipped when requested, like `/probe`, and behind the exporter's
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

To read only some targets, list them under `params`. Prometheus repeats the
parameter for each, which the endpoint accepts as it accepts commas. With
the Helm chart, `staticTargets.monitor.targets` renders the same for its
monitor. One job per group of targets lets each group be scraped on its own
interval, or with its own timeout:

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

`honor_labels` keeps `static_target`, `target` and the targets' own labels
as they are, instead of prefixing them with `exported_` where Prometheus has
a label of the same name. Prometheus still adds `job` and `instance` for the
endpoint, which is why a target may not set either. With the Helm chart,
`staticTargets.monitor` renders the ServiceMonitor or PodMonitor that does
this; see the
[chart README](../charts/prometheus-universal-exporter/README.md#static-targets).

## Debugging a static target

With `--web.debug=full` (the chart's `server.debug: full`),
`/static-targets?debug=<name>` scrapes the target of that name once, as its
schedule would (with its own request settings and credential, within its
interval). It answers with the same plain-text report as a
[debug probe](CONFIGURATION.md#debug-probes): the requests, the response,
each stage, the logs, the advice of the
[metric lint](CONFIGURATION.md#metric-lint) on the target's collector and on
the series, and the series that the scrape would have published, with the
target's labels and `static_target`, and whether `http_exporter_target_up`
would have been `1` or `0`. The report is bounded like a debug probe's: a
long line, URL, header value or metric name is shown by its start and its
length, and where a line of the series is cut, the report says that the
endpoint serves it whole.

```sh
curl 'http://exporter:8080/static-targets?debug=legacy_eu'
```

A debug scrape publishes nothing: the endpoint keeps serving the target's
last scheduled result, and nothing is counted in the self-metrics except
`http_exporter_debug_runs_total`, or exported over OTLP.

- It skips the response cache like a debug probe, and revalidates the
  response that the scrape would revalidate without refreshing it
  ([Revalidating expired responses](CONFIGURATION.md#revalidating-expired-responses)).
  It waits for a slot of the collector's `max_concurrent_probes` as the
  scrape would.
- A name that no target in force has is answered `404`. `debug` takes one
  name and cannot be combined with `targets` (`400`). Below
  `--web.debug=full` it is answered `403`.
- The [debug page](CONFIGURATION.md#the-debug-page) makes the same scrape,
  with the target chosen from a list of the targets of the chosen
  collector, and can run an edited copy of the collector on what it fetched.
  `/debug?static_target=legacy_eu` opens it on that target.

## Status page

For people who have no Prometheus to query, the exporter serves a status
page at the static targets path followed by `/status`,
`/static-targets/status` by default; the landing page links to it. It shows
the static targets first, then the recent probes and the state of the OTLP
export, which [Status page](ENDPOINTS.md#status-page) in Endpoints describes
together with the page as a whole. The static targets section has a row per
target, with:

- its name, its collector (linked to its section on the
  [collectors page](ENDPOINTS.md)), target and labels, its interval, and
  whether it is exported over OTLP. Once the target has been scraped, its
  name links to the series that the endpoint serves of it,
  `/static-targets?targets=<name>`
  ([above](#the-static-targets-endpoint)). With `--web.debug=full` a *debug*
  link beside it scrapes it once now and reports each stage (`?debug=<name>`,
  [Debugging a static target](#debugging-a-static-target)). The target is
  shown as its `target` label and the log show it, with a credential in its
  URL withheld;
- when its last scrape began, how long ago that was and how long it took;
- the result: `success`, `failed`, `stale result served` (failed, with the
  last good result served marked stale under `cache.stale_if_error`),
  `skipped by backoff` (`failure_backoff` kept the scrape away from the
  target), or `not yet scraped`; and whether the response cache answered it;
- a strip of its last 10 scrapes, oldest first, as for the recent probes:
  ✓ `success`, ✗ `failed`, ~ `stale result served` and … `skipped by
  backoff`, each with its time and result as its tooltip, so a target that
  fails occasionally can be told apart from one that failed once;
- the number of consecutive failed scrapes and, while `failure_backoff`
  keeps scrapes away from the target, until when, or that its next try is in
  progress, with the failures that the backoff counts. A scrape skipped by
  backoff tried nothing and is not counted as a failed scrape;
- the error of the last failed scrape and when that scrape began.
  - It stays shown after the target recovers, so a failure in the night can
    be read in the morning. Once the last scrape has succeeded, it is shown
    muted under the label *Earlier failure, recovered since*, with how long
    ago it was, so that it does not contradict the `success` beside it.
  - It stays unchanged while scrapes are skipped by backoff, so the error
    that made the exporter back off is the one shown.
  - It is the error that the log has, with every URL's credentials withheld
    (its userinfo too where the text only quotes the URL), anything else
    that looks like a credential masked, and cut to 2000 bytes.
  - The values of the target's answer that it quotes, and what is for the
    operator only, such as what an [`sshexec`](SSHEXEC.md) command wrote to
    stderr, are left out, as for a probe's caller
    ([What a failed probe's caller is told](CONFIGURATION.md#what-a-failed-probes-caller-is-told)),
    unless `--web.debug` is `full`: the page is read by whoever may call
    `/probe`.
  - A scrape that panicked shows `the scrape panicked; the exporter's log
    has the panic`; the panic's own text is only in the log.

A summary line above says how many targets there are, how many are failing
and how many have not been scraped yet. The failing targets come first.

- A click on a column's heading sorts the table by it, and a second click
  reverses the order. Interval, Last scrape, Recent (by the scrapes in the
  strip that were not a success) and Failures sort by their numbers, and the
  others alphabetically, `t2` before `t10`.
- The filter above the table keeps the rows whose name, collector, target,
  labels or result contain the typed text, in any letter case, and says how
  many rows match.
- The order and the filter are kept through a refresh. Without JavaScript
  the headings are plain and the filter is not shown. The probe jobs table
  of the [allocator's page](ALLOCATOR.md) sorts and filters with the same
  script.
- The page shows when it was served. Its Refresh button and auto-refresh
  selector fetch it again without reloading it (see
  [Refreshing](ENDPOINTS.md#refreshing) in Endpoints), and
  `--web.status-refresh` sets how often it refreshes itself when its URL
  does not say.

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
   "last_error_at": "2026-10-24T08:58:41Z", "last_error_recovered": false,
   "backoff": "backing off until 2026-10-24T09:02:00Z", "backoff_until": "2026-10-24T09:02:00Z", "backoff_trial": false, "backoff_failures": 3,
   "recent": [{"at": "2026-10-24T08:58:41Z", "duration_seconds": 0.012, "result": "failed"},
              {"at": "2026-10-24T08:59:41Z", "duration_seconds": 0.0001, "result": "skipped by backoff"}]},
  {"name": "billing_eu", "...": "..."}
 ]}
```

A field without a value is left out: the scrape fields of a target that has
not been scraped yet, `last_error` of one that never failed, and `backoff`
of one that is not backing off.

- `consecutive_failures` counts the target's failed scrapes of every kind
  except those skipped by backoff. `backoff_failures` counts the failed
  trips that `failure_backoff` counts (see
  [Backing off from failing targets](CONFIGURATION.md#backing-off-from-failing-targets)).
- `recent` is the strip's scrapes, oldest first, each with when it began,
  how long it took and its result. It is `[]` for a target that has not been
  scraped yet.
- `last_error_recovered`, given only with `last_error`, is `true` when the
  last scrape succeeded after the one that `last_error` describes, and
  `false` otherwise.

What the page keeps of a target is forgotten when a reload, or a new bundle
from the allocator, removes the target, whether or not anything reads the
static targets endpoint. A target brought back under its name is
`not yet scraped`, with no failures and no error, until its first scrape.

The page shows nothing that the endpoint's series and the log do not. It is
protected by `web.basic_auth` like every endpoint, and answers only `GET` and
`HEAD`. While no static target is configured it is still served, for its
recent probes, and its static targets section says that none are configured.

In allocator mode it shows the targets that the allocator gave this replica,
and says so: each replica has its own page. It names the member that the
replica is, linked to the allocator's view of it
(`<--allocator.url>/?member=<name>`,
[A member's targets](ALLOCATOR.md#a-members-targets)), says which bundle is
in force and since when, and when the allocator was last heard from. The
JSON has these as `allocator`:

```json
"allocator": {"member": "pod-0", "member_view": "http://pue-allocator:9116/?member=pod-0",
              "generation": "a1b2c3", "in_force_since": "2026-10-24T08:40:12Z", "last_sync": "2026-10-24T08:59:58Z"}
```

`generation` is that of the bundle that the probes are answered from.
`in_force_since` is left out while the client has not yet learned that the
reload that put the bundle in force has returned. A bundle that was in the
cache directory at the start has been in force since the start. The page
moves with `--web.static-targets-path`, so a `--web.self-metrics-path` on it
is refused.

## Exporting over OTLP

A target with `export_via_otlp: true` is also delivered over
[OTLP](OTLP.md), on `otlp.interval`, in addition to being served on the
endpoint:

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
since the last export, the latest value of each series, with the target's
health metrics. A target scraped more often than `otlp.interval` exports
only its latest values. One scraped less often exports its last result again
only when it is scraped again.

**Resource.** `otlp.service_name` and `otlp.resource_attributes` set the
OTLP resource that the target's metrics arrive under.

- Both fall back to the exporter-wide `otlp` settings, and per-target
  attributes are merged over the exporter-wide ones. An attribute written
  `""` is left out: it sets nothing, so the exporter-wide attribute of that
  name stays as it is.
- Targets with different identities are exported as separate
  `resourceMetrics` entries.
- The `otlp` block is only for a target with `export_via_otlp`, and is
  refused on any other.
- `resource_attributes` may not set `service.name`, which `service_name`
  sets: the resource would carry it twice, so the file is refused with a
  pointer to `service_name`.
- With [`otlp.exporter_instance`](OTLP.md#the-exporter-instance) on, the
  resource also carries the attribute that names the exporter replica that
  scraped the target, whether or not the target has an `otlp` block. A
  target whose `resource_attributes` or `labels_as_resource` name that
  attribute, or whose `instance_id` would set it where it is
  `service.instance.id`, is refused, with a message that names the target.
- Over OTLP every series carries `static_target`, as on the endpoint.
  Targets often share a resource (the same collector, the exporter-wide
  identity), and the label keeps their series apart.

`otlp.labels_as_resource` names labels of the target's `labels` that its
resource carries instead of its data points. `otlp.instance_id: true` gives
the resource the attribute `service.instance.id`, set to the target's name:

```yaml
  - name: plant_berlin
    collector: legacy_text
    target: http://plant.berlin.example:8080
    labels:
      city: berlin
      line: "3"
    export_via_otlp: true
    otlp:
      labels_as_resource: [city]
      instance_id: true
```

Over OTLP the target's series and its health series have `line` and
`static_target` and no `city`, which is an attribute of their resource,
beside `service.instance.id="plant_berlin"`. The endpoint serves `city` on
every series as before. These are refused: a name that is not among the
target's `labels`, a name listed twice, a name that its resource already has
as an attribute (from its own `otlp.resource_attributes` or the
exporter-wide ones), and `instance_id` beside a `service.instance.id`
attribute. See
[Static target labels as resource attributes](OTLP.md#static-target-labels-as-resource-attributes).

`export_via_otlp` needs OTLP export. A target with it while `otlp.enabled`
is `false`, or without an `otlp.endpoint`, makes the exporter log
`invalid static target configuration; exiting` and stop.
`configs/config.otlp.example.yaml` is the example configuration with OTLP
export enabled that goes with `configs/static-targets.example.yaml`.

## Reloading

The file is reloaded on the same terms as the exporter configuration: on
`SIGHUP`, on `POST /-/reload` and, with `--config.watch`, when it changes
(see [Reloading on demand](CONFIGURATION.md#reloading-on-demand) and
[Watching the configuration](CONFIGURATION.md#watching-the-configuration)).
An invalid document, or a configuration change that would disable OTLP
export while a loaded target sets `export_via_otlp`, is rejected, and the
last valid pair stays active.

**Changed targets.** A target that the reload changed starts again (first
scraped within ten seconds, then on its own cadence) once a scrape begun on
the old definition has ended. Two scrapes therefore never overlap, and a
fixed address or credential shows within seconds instead of at the old
cadence's next turn.

- The same applies to every target of a collector that the reload changed
  in any way (a rule, a label, its request), even though the target file is
  unchanged. The endpoint serves what the new definition produces within ten
  seconds, not what the old one produced until the target's next turn.
- A target that did not change, of a collector that did not change, keeps
  its cadence.
- A target removed while its scrape runs publishes nothing. Neither does a
  target that the reload changed, or whose collector it changed or removed:
  that scrape read a definition that no longer exists, and its result would
  stand for the target now under the name.
- This also holds when a later reload has restored the target, so the
  result of a scrape begun before the reloads never replaces that of the
  target's first scrape after them. The restored target starts again like a
  changed one, however soon the second reload follows the first: it is
  first scraped within ten seconds, once the scrape begun before the
  reloads has ended, so it is not missing from the endpoint until its next
  turn.
- Its failures are logged anew as well. What the
  [failure log](LOGGING.md#repeated-failures) remembered of a target that
  the reload removed or changed is forgotten, and a scrape begun before the
  reload neither adds to it nor is logged as the target's recovery.

**Rejected reloads.** A rejected reload is tried again when the file
changes. With `--config.watch` it is also tried again, with the file
unchanged, when a file that its check opened changes: the descriptor files
(`request.protoset_file`, `request.proto_files`, and the files that those
`.proto` files import) of a grpc collector whose target sets a
`request.message`. A reload that ran while such a file was being replaced
therefore succeeds at the tick after the file is in place (see
[Watching the configuration](CONFIGURATION.md#watching-the-configuration)).

- A rejected target file is not read for any other descriptor file. One
  rejected because it names a collector that the configuration does not have
  is read again when it or the configuration file changes, and a descriptor
  file that changes meanwhile reloads only the configuration.
- The same files are watched while the target file is in force. When one
  changes, the next tick reads the target file again with the
  configuration, although neither changed. A `request.message` that the new
  descriptors refuse is then a rejected reload of the target file, logged
  with the target and the reason, instead of only failing scrapes (see
  [Descriptor files](CONFIGURATION.md#descriptor-files)). When the message
  still fits, the reload changes nothing: every target keeps its cadence.
- A target's credential files are read at each scrape, so a missing one
  fails that target's scrapes, not the reload.

The configuration and the target file take effect as one pair. A reload that
changes both (a collector renamed in the configuration and in the targets
that use it) puts both in force in one step. Every scrape uses the
configuration that its target was read with, so no scrape runs a target of
one reload against the collectors of another. A scrape already waiting or
running when the reload comes finishes on the pair it started with. Its
target is then scraped again as above if the reload changed it or its
collector, and what the earlier scrape produced is not published.

**Shutdown.** On `SIGTERM` or `SIGINT` the targets are still scraped
throughout `--web.shutdown-delay`, while their endpoint is still served.
After that no scrape starts, not even one that was waiting for a slot and
gets it then, and those in flight are cut short immediately instead of being
waited for. What they would publish is served only by this exporter, which
is stopping, and a target that does not answer would delay the shutdown for
the whole `--web.shutdown-timeout`. A scrape cut short publishes nothing and
logs no failure, so a restart never reports a target down, over OTLP or
anywhere else, and the target's last result stands. With `export_via_otlp`,
the last OTLP export carries what the scrapes before it queued, not what the
cut-short scrapes would have queued.

## Self-metrics

Static target scrapes are counted in the per-collector
[self-metrics](SELF-METRICS.md), like probes, not in per-target series; the
endpoint's health metrics are the per-target view.
`http_exporter_static_targets` reports how many targets are loaded, and
`http_exporter_static_targets_exported_via_otlp` how many of them are also
exported over OTLP. The file's reloads are reported under
`file="static_targets"` in the
[reload metrics](SELF-METRICS.md#configuration-reloads).
