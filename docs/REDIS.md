# Redis and Valkey

The `redis` request type reads a Redis or Valkey server, which speak the same
protocol: its `INFO`, and the replies of read-only commands the collector
lists, such as the length of a queue, handed to `jq`, `yq` or `python` as one
JSON document. Use it where a server's own numbers, or the size of what an
application keeps in it, are what you want as metrics, and a dedicated
exporter for it is one more thing to run:

- memory used against `maxmemory`, clients connected and blocked, hits and
  misses, evictions, from `INFO`;
- the backlog of a job queue kept in a list, a sorted set or a stream, with
  `LLEN`, `ZCOUNT` or `XINFO GROUPS`;
- the replicas of a primary and how far behind each is, from
  `INFO replication`.

The type is opt-in: the default build and the published image leave it out,
so a build carries it only when asked
([Building with redis](#building-with-redis)). It needs nothing outside Go's
standard library.

## Building with redis

`OPT_IN_REQUEST_TYPES` names the opt-in types a build adds to the types it
carries, comma-separated:

```sh
make build OPT_IN_REQUEST_TYPES=redis            # bin/prometheus-universal-exporter
make build OPT_IN_REQUEST_TYPES=s3file,redis     # both opt-in types
docker build --build-arg OPT_IN_REQUEST_TYPES=redis -t exporter:redis .
go build -tags "$(sh tools/request-type-tags.sh '' '' redis)" .
```

The tag is `request_type_redis`, which is all `go build -tags
request_type_redis .` needs by hand: every type of the default build, and
`redis`; `-tags request_type_s3file,request_type_redis` adds both opt-in
types. `OPT_IN_REQUEST_TYPES` goes with `REQUEST_TYPES` (see [Choosing
request types at build time](CONFIGURATION.md#choosing-request-types-at-build-time)):
`REQUEST_TYPES=http OPT_IN_REQUEST_TYPES=redis` builds the two, and
`REQUEST_TYPES` may name `redis` itself, `REQUEST_TYPES=redis` building it
alone.

A configuration with a `redis` collector is refused by a build without the
type, at startup, on a reload and by `--dry-run`, saying how to build one
that has it:

```text
collector "redis" uses request.type "redis", which this build of the exporter does not include; it was built with: dns, graphite, grpc, http, kubernetes, localfile, sql, sshfile, tlscert. redis is an opt-in type, which the default build and the published image leave out: build with OPT_IN_REQUEST_TYPES=redis (make build, or the Dockerfile's build argument), which adds the tag request_type_redis
```

## A collector

```yaml
collectors:
  - name: redis
    request:
      type: redis
      password_file: /var/run/secrets/redis/password
      info_sections: [clients, memory, stats, keyspace]
    transform:
      type: jq
    metrics:
      - name: redis_connected_clients
        description: Clients connected to the server
        expression: .info.clients.connected_clients
      - name: redis_memory_used_bytes
        description: Memory the server has allocated
        expression: .info.memory.used_memory
      - name: redis_memory_max_bytes
        description: The server's maxmemory, 0 for no limit
        expression: .info.memory.maxmemory
      - name: redis_memory_used_ratio
        description: Memory used as a share of maxmemory, 0 without a limit
        expression: '.info.memory | if .maxmemory > 0 then .used_memory / .maxmemory else 0 end'
      - name: redis_keyspace_hits_total
        type: counter
        expression: .info.stats.keyspace_hits
      - name: redis_keyspace_misses_total
        type: counter
        expression: .info.stats.keyspace_misses
      - name: redis_keyspace_hit_ratio
        description: Hits as a share of lookups since the server started
        expression: '.info.stats | (.keyspace_hits + .keyspace_misses) as $all | if $all > 0 then .keyspace_hits / $all else 0 end'
      - name: redis_db_keys
        description: Keys of each database
        items: '.info.keyspace | to_entries[]'
        expression: .value.keys
        labels:
          - name: db
            expression: .key
```

```text
/probe?collector=redis&target=redis-master.cache:6379
```

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `redis`. |
| `username` | the default user | An [ACL user](https://redis.io/docs/latest/operate/oss_and_stack/management/security/acl/), logged in as with `AUTH username password`. Only with `password_file`. |
| `password_file` | none | The file of the password, read at every scrape, its surrounding whitespace taken off. Without it no `AUTH` is sent, for a server that wants none. |
| `db` | `0` | The logical database, selected with `SELECT` after the login; what the keys of `commands` are read from. |
| `info_sections` | INFO's default sections | The sections of `INFO` read, such as `[server, memory, keyspace]`, each with an `INFO` of its own, which every version of Redis takes. Left out, `INFO` alone, the server's default sections, unless `commands` is set: see [What a scrape reads](#what-a-scrape-reads). |
| `commands` | none | Read-only commands whose replies the document holds besides: see [Commands](#commands). |
| `tls` | none | Speak TLS, verifying the server: see [TLS](#tls). |
| `max_response_bytes` | `10MiB` | The most bytes of the server's replies to one scrape's commands, together: see [Limits](#limits). |
| `allowed_targets`, `denied_targets` | none | The servers the collector may and may not reach, as for [any type that reaches a host](REQUESTS.md#restricting-targets), checked on the connection. |

No other request key applies, and setting one is a startup error. No
password is written in the configuration: there is no `password` key. The
decoder is `json`, whether `decoder.type` is left out or set to it; any other
decoder, and a transform that does not read JSON, is refused when the
configuration loads. Use `jq`, `yq` or `python`.

## The target

The probe's `target` is the server: `host`, `host:port`, `[v6]:port`, or a
`redis://` or `rediss://` URL of one, port 6379 when it names none. It is
required. A user, a password, a database or a query in the target is refused
with `400` — the database is `db`, the account `username` — and so is any
other scheme. `allowed_targets` and `denied_targets` are matched against the
host before the connection is made and against the address it is made to,
and the cloud metadata service is refused without them, as for `http`.

## TLS

`rediss://`, or a `tls` block, makes the scrape speak TLS, the server's
certificate verified against the system's roots and `tls.ca_file`, for the
name `tls.server_name` or else the target's host. The block is
[the one `http` takes](REQUESTS.md#tls): `cert_file` and `key_file` present a
client certificate, to a server started with `tls-auth-clients yes`, and
`insecure_skip_verify` skips the verification. A bare `host:port` with a
`tls` block speaks TLS; `redis://` with one is refused, the two saying
opposite things.

```yaml
request:
  type: redis
  tls:
    ca_file: /etc/exporter/redis-ca.pem
  password_file: /var/run/secrets/redis/password
```

## What a scrape reads

A scrape makes one connection, logs in with `AUTH` when `password_file` is
set, selects `db` with `SELECT` when it is not `0`, and sends, each answered
before the next:

- `INFO <section>` for each of `info_sections`, or `INFO` alone when neither
  `info_sections` nor `commands` is set;
- each of `commands`, in order.

So a collector of `commands` alone sends no `INFO`; give it `info_sections`
too for both. The connection is closed when the scrape is over, the next
scrape making its own: a scrape is a handful of commands at most every few
seconds, and a connection kept between them would need a keepalive, an idle
timeout and a limit of its own for no gain worth them.

The answer is a JSON document:

```json
{
  "info": {
    "memory": {"used_memory": 1048576, "maxmemory": 4194304, "used_memory_human": "1.00M", "mem_fragmentation_ratio": 1.25},
    "keyspace": {"db0": {"keys": 12, "expires": 3, "avg_ttl": 1500}},
    "replication": {"role": "master", "connected_slaves": 1, "slave0": {"ip": "10.0.0.2", "port": 6379, "state": "online", "offset": 1234, "lag": 0}, "master_repl_offset": 1240}
  },
  "commands": {"queue": 42}
}
```

- `info` holds each section by its heading in lower case, `_` for a blank,
  each an object of its `name:value` lines. It is there when `INFO` was sent.
- A value that is a number is a JSON number, written exactly as the server
  wrote it, so a 64-bit counter keeps every digit; `1.00M`, `55.12%`,
  `7.2.4` and `00000000` stay text. A value made of `name=value` pairs joined
  by commas — a keyspace line, a replica's `slave0` line, every
  `cmdstat_<command>` line of `commandstats` — is an object of them, each
  value read the same way.
- `commands` holds each command's reply under its `name`. It is there when
  `commands` was set.

A section named in `info_sections` that the server does not have, which it
answers with nothing, fails the scrape naming it: `INFO everything` lists
those it has.

## Commands

`commands` lists read-only commands, each with a `name` its reply is put
under, letters, digits and `_`, and the command and its arguments as a list:

```yaml
commands:
  - name: emails_queued
    command: [LLEN, "queue:emails"]
  - name: delayed_due
    command: [ZCOUNT, delayed, "-inf", "{{param_now:+inf}}"]
  - name: groups
    command: [XINFO, GROUPS, events]
```

The command and its subcommand are matched in any case. An argument is sent
as one bulk string, exactly as written, so a key with a blank or a quote in
it needs no escaping, and YAML's numbers, `[ZCOUNT, q, 0, 100]`, are sent as
the text they are written as. An argument may hold `{{param_name}}`
placeholders, filled from the probe's `param_<name>` parameters, as a path's
are ([Path parameters](REQUESTS.md#path-parameters)), with any value; the
command's name and subcommand may not.

Only these commands are taken, with the arguments after the name, or the
name and subcommand, that each takes; anything else is refused when the
configuration loads:

| Command | Arguments | Reply in the document |
| --- | --- | --- |
| `DBSIZE`, `LASTSAVE`, `SLOWLOG LEN` | none | a number |
| `ROLE` | none | a list |
| `CLUSTER INFO` | none | an object of its lines, read as INFO's values are |
| `GET`, `STRLEN`, `TYPE`, `TTL`, `PTTL`, `LLEN`, `SCARD`, `ZCARD`, `HLEN`, `XLEN`, `MEMORY USAGE`, `OBJECT ENCODING`, `OBJECT FREQ`, `OBJECT IDLETIME`, `OBJECT REFCOUNT` | a key | a number, or text, or `null` for a key that is not there |
| `HGETALL`, `XINFO STREAM` | a key | an object of the names and values |
| `XINFO GROUPS` | a key | a list of objects, one for each consumer group |
| `XINFO CONSUMERS` | a key and a group | a list of objects, one for each consumer |
| `LINDEX`, `SISMEMBER`, `ZSCORE`, `HGET`, `HEXISTS`, `XPENDING` | a key and one more | as the server answers |
| `ZCOUNT`, `ZLEXCOUNT` | a key, a minimum and a maximum | a number |
| `EXISTS`, `PFCOUNT` | one key or more | a number |
| `HMGET` | a key and one field or more | a list |

These read, and answer no more than what they read: no command writes, lists
keys (`KEYS`, `SCAN`), dumps a collection without a bound (`LRANGE`,
`SMEMBERS`), blocks (`BLPOP`, `XREAD`), or shows the server's settings, a
password among them, or its other clients (`CONFIG GET`, `CLIENT LIST`). A
reply is mapped kind by kind: an integer as a number, a string as text, a
nil as `null`, an array as a list; a string reply is never read as a number,
so a counter kept with `INCR` and read with `GET` is `"17"`, which `jq`'s
`tonumber` reads. An error reply, such as `WRONGTYPE`, fails the scrape with
the command and the server's words.

## Examples

A job queue kept in a list, one collector for every queue, the queue named by
the probe:

```yaml
  - name: queue_length
    request:
      type: redis
      commands:
        - name: length
          command: [LLEN, "queue:{{param_queue}}"]
    transform:
      type: jq
    metrics:
      - name: redis_queue_length
        description: Jobs waiting in the queue
        expression: .commands.length
        labels:
          - name: queue
            value: "{{param_queue}}"
```

```text
/probe?collector=queue_length&target=redis-master.cache:6379&param_queue=emails
```

The replicas of a primary, and how far behind each is, in seconds since it
last answered and in bytes of the replication stream, from
`INFO replication`; on a replica, how long ago it heard from its primary:

```yaml
  - name: replication
    request:
      type: redis
      info_sections: [replication]
    transform:
      type: jq
    metrics:
      - name: redis_connected_replicas
        expression: .info.replication.connected_slaves // 0
      - name: redis_replica_lag_seconds
        description: Seconds since each replica last acknowledged the primary
        items: '.info.replication | to_entries[] | select(.key | test("^slave[0-9]+$")) | .value'
        expression: .lag
        labels:
          - name: replica
            expression: '"\(.ip):\(.port)"'
      - name: redis_replica_behind_bytes
        description: Bytes of the replication stream each replica has yet to acknowledge
        items: '.info.replication as $r | $r | to_entries[] | select(.key | test("^slave[0-9]+$")) | .value | .behind = $r.master_repl_offset - .offset'
        expression: .behind
        labels:
          - name: replica
            expression: '"\(.ip):\(.port)"'
      - name: redis_master_last_io_seconds
        description: On a replica, seconds since its primary last sent anything
        expression: .info.replication.master_last_io_seconds_ago
        required: false
```

The consumer groups of a stream, each one's pending messages:

```yaml
  - name: stream_groups
    request:
      type: redis
      db: 2
      commands:
        - name: groups
          command: [XINFO, GROUPS, events]
    transform:
      type: jq
    metrics:
      - name: redis_stream_group_pending
        items: .commands.groups[]
        expression: .pending
        labels:
          - name: group
            expression: .name
```

## Static targets

A [static target](STATIC-TARGETS.md) of a `redis` collector names the server
as its `target`, which it requires, and may set under its `request` only
`timeout`. The account, the database and the commands are the
collector's, and `params` fills its placeholders:

```yaml
interval: 30s
targets:
  - name: cache_primary
    collector: redis
    target: redis-master.cache:6379
  - name: emails_queue
    collector: queue_length
    target: rediss://queues.internal:6380
    params:
      param_queue: emails
```

## Limits

Everything the server answers a scrape is read against `max_response_bytes`,
or `limits.max_response_bytes`, 10 MiB when neither is set, counted in the
bytes of the replies as they come off the connection. A string whose
declared length is past what is left fails the scrape before a byte of it is
read, and no more is held for one than the server has sent of it, so a
broken or hostile server's `$1000000000` costs the bytes that came, never a
gigabyte; an array whose elements could not fit fails the same way. Either
counts in `http_exporter_series_limit_exceeded_total` as a limit exceeded.
Arrays nest at most 16 deep and a line of a reply, but a string's data, is
at most 64 KiB; a server past either, or one that answers no RESP2 at all,
fails the scrape saying it is no RESP2 reply.

The document the replies make is held to the same limit: JSON writes a
control character as six bytes (`\u0001`) and a quote or a backslash as two,
so replies within the limit can make a document past it, which fails the
scrape as a limit exceeded too, saying how large the document was.

The connection, the TLS handshake and every command end at the probe's
deadline, its `timeout` or the scrape's, 30 seconds without one: a server
that stops answering fails the scrape when it comes, with the deadline's
error and the command it was waiting on.

## Errors and self-metrics

A failed scrape is reported in the `redis` stage and follows
`error_handling.on_fetch_error`, naming the command and what went wrong:
`AUTH exporter ***: the server answered: WRONGPASS invalid username-password
pair or user is disabled.`, `connecting to redis-master.cache:6379: ...`. A
server the target lists refuse counts in
`http_exporter_targets_refused_total`, answered `403`. Transforms see no
status (`$status` is `null`, Python's `response.status_code` `None`) and
the header `Content-Type: application/json`.

A [debug probe](CONFIGURATION.md#debug-probes) shows each command in its
request section — `AUTH` with its password as `***`, `SELECT`, each `INFO`
and command — and the kind of each reply, or the error:

```text
Requests
  1. AUTH redis://redis-master.cache:6379/2 -> the simple string OK in 1ms
     Redis-Command: AUTH exporter ***
  2. SELECT redis://redis-master.cache:6379/2 -> the simple string OK in 1ms
     Redis-Command: SELECT 2
  3. XINFO redis://redis-master.cache:6379/2 -> an array of 2 elements in 1ms
     Redis-Command: XINFO GROUPS events
```

With [verbose self-metrics](SELF-METRICS.md#verbose-per-request-self-metrics)
a scrape is labelled `url="redis://host:port/db"`, `rediss://` over TLS, and
`http_method="INFO"` when it sends `INFO` alone, `http_method="COMMANDS"`
when it sends the collector's commands alone, and
`http_method="INFO+COMMANDS"` when it sends `info_sections`' `INFO`s and the
commands.

## Limitations

- **RESP2 only.** The client never sends `HELLO 3`, and every Redis and
  Valkey server answers RESP2 to a client that does not.
- **No Sentinel or cluster routing.** A collector reads the server its
  target names; a cluster's nodes are each a target, and a command on a key
  another node holds fails with the `MOVED` error.
- **Text values only.** The document is JSON, whose strings are text, so a
  reply holding bytes that are not UTF-8 — a serialized object, a
  compressed value, a counter written in binary, or a hash field named so —
  fails the scrape, naming the command, rather than being put in the
  document with U+FFFD in place of those bytes. Read such a key's size or
  kind instead, with `STRLEN`, `TYPE` or `OBJECT ENCODING`.
- **One connection per scrape.** No connection is kept between scrapes, so
  each scrape costs a TCP handshake, and a TLS one with TLS.
- **No `CONFIG GET`, `CLIENT LIST` or `LATENCY`.** What they show beyond
  `INFO` holds secrets or other clients' addresses.

## Troubleshooting

| The scrape says | Do |
| --- | --- |
| `... uses request.type "redis", which this build of the exporter does not include` | Build with `OPT_IN_REQUEST_TYPES=redis` (see [Building with redis](#building-with-redis)); the published image does not carry it. |
| `INFO: the server answered: NOAUTH Authentication required.` | The server wants a password: set `password_file`. |
| `AUTH ***: the server answered: WRONGPASS ...` | The password, or the user: an ACL user needs `username`. |
| `AUTH ***: the server answered: ERR AUTH <password> called without any password configured ...` | The server wants none: take `password_file` out. |
| `GET session:1: the reply holds bytes that are not UTF-8 text, ...` | The key holds binary data: read its size or kind (see [Limitations](#limitations)). |
| `the document of the replies is ... bytes as JSON, larger than the response limit ...` | The replies hold control characters JSON escapes: raise `max_response_bytes`, or ask for less. |
| `... sends CONFIG, which is not among the read-only commands a redis collector may send` | Only the [listed commands](#commands) are taken. |
| `INFO keyspace ...: the server answered no section` | The section's name: `INFO everything` lists those the server has. |
| `LLEN ...: the server answered: WRONGTYPE ...` | The key holds another kind of value: `TYPE` says which. |
| `... the replies are larger than the response limit ...` | Raise `max_response_bytes`, or ask for fewer sections or smaller replies. |
| `TLS handshake with ...: ... certificate signed by unknown authority` | Name the server's CA in `tls.ca_file`. |
| `... a reply begins with "H", which no RESP2 reply does` | The target is not a Redis server: the port, or `rediss://` for a server that speaks TLS. |
