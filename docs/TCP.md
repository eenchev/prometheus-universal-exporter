# Text protocols over TCP

The `tcp` request type reads a service that answers a line sent over a TCP
connection with its statistics, as text: it connects, writes the line the
collector names, reads the answer until it ends with a terminator or the
server closes the connection, and hands the answer to the decoder and the
transform as an `http` collector's body is handed to them. Use it where a
service's numbers come that way and a dedicated exporter for it is one more
thing to run:

- memcached's `stats`;
- ZooKeeper's four-letter words, such as `mntr`;
- HAProxy's runtime API on a TCP socket, `show stat`, which answers CSV;
- NUT's `upsd`, `LIST VAR <ups>`, and Beanstalkd's `stats`.

Whatever reads a body reads the answer: `regex` for lines of text, `csv`,
`prometheus` for a service that writes exposition, `jq` for one that answers
JSON, `yq`, `python`.

The type is opt-in: the default build and the published image leave it out,
so a build carries it only when asked
([Building with tcp](#building-with-tcp)). It needs nothing outside Go's
standard library.

## Building with tcp

`OPT_IN_REQUEST_TYPES` names the opt-in types a build adds to the types it
carries, comma-separated:

```sh
make build OPT_IN_REQUEST_TYPES=tcp              # bin/prometheus-universal-exporter
make build OPT_IN_REQUEST_TYPES=redis,tcp        # more than one opt-in type
docker build --build-arg OPT_IN_REQUEST_TYPES=tcp -t exporter:tcp .
go build -tags "$(sh tools/request-type-tags.sh '' '' tcp)" .
```

The tag is `request_type_tcp`, which is all `go build -tags
request_type_tcp .` needs by hand: every type of the default build, and
`tcp`. `OPT_IN_REQUEST_TYPES` goes with `REQUEST_TYPES` (see [Choosing
request types at build time](CONFIGURATION.md#choosing-request-types-at-build-time)):
`REQUEST_TYPES=http OPT_IN_REQUEST_TYPES=tcp` builds the two, and
`REQUEST_TYPES` may name `tcp` itself, `REQUEST_TYPES=tcp` building it
alone.

A configuration with a `tcp` collector is refused by a build without the
type, at startup, on a reload and by `--dry-run`, saying how to build one
that has it:

```text
collector "memcached" uses request.type "tcp", which this build of the exporter does not include; it was built with: dns, graphite, grpc, http, kubernetes, localfile, sql, sshfile, tlscert. tcp is an opt-in type, which the default build and the published image leave out: build with OPT_IN_REQUEST_TYPES=tcp (make build, or the Dockerfile's build argument), which adds the tag request_type_tcp
```

## A collector

```yaml
collectors:
  - name: memcached
    request:
      type: tcp
      send: "stats\r\n"
      terminator: "END\r\n"
    transform:
      type: regex
    metrics:
      - name: memcached_current_connections
        description: Connections open to the server
        expression: '(?m)^STAT curr_connections (\d+)\r$'
      - name: memcached_items
        description: Items stored
        expression: '(?m)^STAT curr_items (\d+)\r$'
      - name: memcached_memory_used_bytes
        expression: '(?m)^STAT bytes (\d+)\r$'
      - name: memcached_memory_limit_bytes
        expression: '(?m)^STAT limit_maxbytes (\d+)\r$'
      - name: memcached_get_hits_total
        type: counter
        expression: '(?m)^STAT get_hits (\d+)\r$'
      - name: memcached_get_misses_total
        type: counter
        expression: '(?m)^STAT get_misses (\d+)\r$'
      - name: memcached_evictions_total
        type: counter
        expression: '(?m)^STAT evictions (\d+)\r$'
```

```text
/probe?collector=memcached&target=memcached-0.cache:11211
```

memcached ends each line with `\r\n`, so a rule's `$` comes after a `\r`:
with `(?m)`, `$` matches before a line feed alone.

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `tcp`. |
| `send` | nothing | The text written once the connection is made, such as `"stats\r\n"`. Write it in double quotes, in which YAML reads `\r\n` as a carriage return and a line feed; in single quotes or none, `\r\n` is four characters. May hold `{{param_name}}` placeholders. At most 64 KiB. Left out, nothing is written, and the answer is what the server says first. See [What a scrape sends](#what-a-scrape-sends). |
| `terminator` | none | What the answer ends with, such as `"END\r\n"`: reading stops once it has come. Left out, reading stops when the server closes the connection. At most 64 KiB, and no placeholder. See [Reading the answer](#reading-the-answer). |
| `tls` | none | Speak TLS, verifying the server: see [TLS](#tls). |
| `max_response_bytes` | `10MiB` | The most bytes of the answer: see [Limits](#limits). |
| `allowed_targets`, `denied_targets` | none | The services the collector may and may not reach, as for [any type that reaches a host](REQUESTS.md#restricting-targets), checked on the connection. |

No other request key applies, and setting one is a startup error. A key
written `""` is the key left out. The decoder is `decoder.type`, or what the
transform implies — `text` for `regex`, `csv` for `csv`, `prometheus` for
`prometheus` — or, for `jq`, `yq` and `python` without one, what the answer
reads as, JSON when it is JSON: the answer has no `Content-Type` to go by.

## The target

The probe's `target` is the service: `host:port` or `[v6]:port`. The port
is required, since no port is every such service's. A scheme, a path, a
query or a user in the target is refused with `400`, before anything is
sent. `allowed_targets` and `denied_targets` are matched against the host
before the connection is made and against the address it is made to, and
the cloud metadata service is refused without them, as for `http`.

## TLS

A `tls` block makes the scrape speak TLS, the server's certificate verified
against the system's roots and `tls.ca_file`, for the name
`tls.server_name` or else the target's host. The block is
[the one `http` takes](REQUESTS.md#tls): `cert_file` and `key_file` present
a client certificate, both or neither, and `insecure_skip_verify` skips the
verification. There is no scheme for TLS in the target: the block alone says
it.

```yaml
request:
  type: tcp
  send: "show stat\n"
  tls:
    ca_file: /etc/exporter/haproxy-ca.pem
```

## What a scrape sends

A scrape makes one connection, speaks TLS over it when the collector has a
`tls` block, writes `send` when it is set, reads the answer and closes the
connection; the next scrape makes its own. `send` is written as it is,
nothing added: a protocol whose commands end with a line feed or `\r\n`
needs it written at the end of `send`, and ZooKeeper's four-letter words take
none. The write's end of the connection stays open while the answer is read,
since some servers stop answering a client that has closed it.

A placeholder in `send` is filled from the probe's `param_<name>`
parameter, as a path's is ([Path parameters](REQUESTS.md#path-parameters)),
with its default when the probe gives none. A value holding a carriage
return or a line feed is refused, answered `400` before the connection is
made, since it would end the line `send` writes and start another, a command
of the probe's choosing; any other value is written as it is. A probe
parameter that no placeholder of `send` or of a label value names is refused,
as for every type.

```yaml
  - name: haproxy_proxy
    request:
      type: tcp
      send: "show stat {{param_proxy:-1}} -1 -1\n"
```

asks HAProxy for the proxy the probe names, `&param_proxy=web`, and for every
proxy without one. The terminator holds no placeholder: it is matched as it
is written.

| Service | `send` | `terminator` |
| --- | --- | --- |
| memcached | `"stats\r\n"` | `"END\r\n"` |
| ZooKeeper | `mntr`, `ruok`, `srvr` | none: the server closes |
| HAProxy's runtime API | `"show stat\n"`, `"show info\n"` | none: the server closes |
| NUT `upsd` | `"LIST VAR ups\n"` | `"END LIST VAR ups\n"` |
| Beanstalkd | `"stats\r\nquit\r\n"` | none: the server closes after `quit` |

## Reading the answer

With `terminator`, reading stops once the answer read so far ends with it;
the terminator stays in the body, where a rule can match the last line by
it. A server that closes the connection before the terminator came cut its
answer short, and the scrape fails, saying how many bytes came. A
terminator is matched at the end of what was read, so a server that writes
more after it in the same answer is read on until the deadline: name the
end of the whole answer.

Without `terminator`, reading stops when the server closes the connection,
as ZooKeeper does after a four-letter word and HAProxy's runtime API after a
command. A server that keeps the connection open, as memcached does, waiting
for the next command, holds the scrape until its deadline, which fails it:
give such a service its `terminator`.

Either way the answer is the body: as the server wrote it, in no JSON
envelope, with no status (`$status` is `null`, Python's
`response.status_code` `None`) and no headers.

## Examples

[`examples/tcp/`](../examples/tcp/config.yaml) holds complete collectors for
memcached, ZooKeeper and HAProxy, with more of each one's statistics than
below, and static targets for them.

ZooKeeper's `mntr`, each number it reports as a series of one family by its
name, and whether the server is the ensemble's leader. The server answers
the four-letter words its `4lw.commands.whitelist` names, `mntr` among them:

```yaml
  - name: zookeeper
    request:
      type: tcp
      send: mntr
    transform:
      type: regex
    metrics:
      - name: zookeeper_mntr
        description: Each number mntr reports, by its name
        expression: '(?m)^zk_(?P<name>\w+)\t(?P<value>-?[0-9.]+)$'
        labels:
          - name: name
            expression: name
      - name: zookeeper_leader
        description: 1 on the ensemble's leader
        expression: '(?m)^zk_server_state\t(\w+)$'
        value_map:
          leader: 1
          "*": 0
```

```text
/probe?collector=zookeeper&target=zk-0.zookeeper:2181
```

HAProxy's runtime API on a TCP socket, `stats socket ipv4@0.0.0.0:9999 level
user` in its configuration, each proxy's and server's sessions and whether
it is up, from `show stat`'s CSV. Its header line begins `# pxname`, which
is the first column's name:

```yaml
  - name: haproxy
    request:
      type: tcp
      send: "show stat\n"
    transform:
      type: csv
    metrics:
      - name: haproxy_current_sessions
        expression: scur
        labels:
          - name: proxy
            expression: "# pxname"
          - name: server
            expression: svname
      - name: haproxy_sessions_total
        type: counter
        expression: stot
        labels:
          - name: proxy
            expression: "# pxname"
          - name: server
            expression: svname
      - name: haproxy_up
        description: 1 when the proxy or server is UP or OPEN
        expression: status
        value_map:
          UP: 1
          OPEN: 1
          "*": 0
        labels:
          - name: proxy
            expression: "# pxname"
          - name: server
            expression: svname
```

## Static targets

A [static target](STATIC-TARGETS.md) of a `tcp` collector names the service,
`host:port`, as its `target`, which it requires, and may set under its
`request` only `timeout`. What is sent and the terminator are the
collector's, and `params` fills its placeholders:

```yaml
interval: 30s
targets:
  - name: memcached_0
    collector: memcached
    target: memcached-0.cache:11211
  - name: zookeeper_0
    collector: zookeeper
    target: zk-0.zookeeper:2181
    request:
      timeout: 5s
```

## Limits

The answer is read against `max_response_bytes`, or
`limits.max_response_bytes`, 10 MiB when neither is set, as it arrives: an
answer that grows past it fails the scrape when it does, without waiting
for the rest, as a limit exceeded, which counts in
`http_exporter_series_limit_exceeded_total`.

The connection, the TLS handshake, the write and the read end at the
probe's deadline, its `timeout` or the scrape's, 30 seconds without one:
a server that stops answering, or keeps the connection open without the
terminator, fails the scrape when it comes, with the deadline's error.

## Errors and self-metrics

A failed scrape is reported in the `tcp` stage and follows
`error_handling.on_fetch_error`, saying what went wrong:
`connecting to memcached-0.cache:11211: ...`, `reading from
memcached-0.cache:11211: no answer ending with request.terminator "END\r\n"
came, 12 bytes read: ...`, `the server closed the connection after 12 bytes,
and the answer does not end with request.terminator "END\r\n"`. A service
the target lists refuse counts in `http_exporter_targets_refused_total`,
answered `403`.

A [debug probe](CONFIGURATION.md#debug-probes) shows in its request section
the bytes sent, quoted so that a carriage return and a line feed are seen,
and how many bytes were read and how reading stopped; its response section
shows the answer:

```text
Requests
  1. SEND tcp://memcached-0.cache:11211 -> read 1342 bytes, the answer ended with the terminator "END\r\n" in 1ms
     Tcp-Send: "stats\r\n"
```

With [verbose self-metrics](SELF-METRICS.md#verbose-per-request-self-metrics)
a scrape is labelled `url="tcp://host:port"`, `tls://` over TLS, and
`http_method="SEND"` when the collector writes `send`, `http_method="READ"`
when it only reads.

## Limitations

- **One line, one answer.** A scrape writes `send` once and reads one
  answer; a dialogue, a login, or an answer read after a second command is
  not spoken. Write every command into `send` where the server takes them
  together, as Beanstalkd's `stats\r\nquit\r\n`.
- **Text matched as written.** The terminator is matched as bytes at the end
  of the answer, with no placeholder, and no pattern.
- **One connection per scrape.** No connection is kept between scrapes, so
  each scrape costs a TCP handshake, and a TLS one with TLS.
- **No UDP, no Unix sockets.** A service reached only over a Unix socket,
  as HAProxy's runtime API often is, needs a TCP socket configured beside it.

## Troubleshooting

| The scrape says | Do |
| --- | --- |
| `... uses request.type "tcp", which this build of the exporter does not include` | Build with `OPT_IN_REQUEST_TYPES=tcp` (see [Building with tcp](#building-with-tcp)); the published image does not carry it. |
| `target "..." is not host:port; a tcp target names its port` | Add the port: `memcached-0.cache:11211`. |
| `... no answer ending with request.terminator "END\r\n" came, ...` | The terminator: written in double quotes, so that `\r\n` is a carriage return and a line feed, and the end of the whole answer. Or `send`: the server did not take it, and waits for more, often a line feed it lacks. |
| `... the server kept the connection open, ...; without request.terminator ...` | The server does not close after answering: set `terminator`. |
| `the server closed the connection after 0 bytes, ...` | The server refused what was sent: a ZooKeeper whose `4lw.commands.whitelist` does not name the word answers nothing and closes. |
| `... the answer of ... is larger than the response limit ...` | Raise `max_response_bytes`, or send a command that answers less. |
| `request.send: the value of param_<name> holds a carriage return or a line feed ...` | The probe's parameter: a value may not end the line. |
| `TLS handshake with ...: ... certificate signed by unknown authority` | Name the server's CA in `tls.ca_file`. |
