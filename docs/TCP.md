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
- NUT's `upsd`, `LIST VAR <ups>`, and Beanstalkd's `stats`;
- whether a service that speaks first — SMTP, FTP, POP3, IMAP, SSH — greets
  as it should, and how long it took ([Services that speak
  first](#services-that-speak-first)).

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
collector "memcached" uses request.type "tcp", which this build of the exporter does not include; it was built with: dns, graphite, grpc, http, kubernetes, localfile, ntp, sql, sshfile, tlscert. tcp is an opt-in type, which the default build and the published image leave out: build with OPT_IN_REQUEST_TYPES=tcp (make build, or the Dockerfile's build argument), which adds the tag request_type_tcp
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
    derived:
      - name: memcached_gets
        hidden: true
        sum: [memcached_get_hits_total, memcached_get_misses_total]
      - name: memcached_get_hit_ratio
        ratio: [memcached_get_hits_total, memcached_gets]
```

```text
/probe?collector=memcached&target=memcached-0.cache:11211
```

memcached ends each line with `\r\n`, so a rule's `$` comes after a `\r`:
with `(?m)`, `$` matches before a line feed alone.
memcached counts hits and misses, not their ratio, so the ratio is
[derived](CONFIGURATION.md#derived-series) from the two, over their hidden
sum; a server that has had no get yet has no ratio.

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `tcp`. |
| `document` | `false` | `true` hands the transforms a JSON document of the exchange, the greeting, the answer and each step's seconds, in place of the server's text; the decoder is then `json`. See [The exchange as a document](#the-exchange-as-a-document). |
| `greeting` | none | A regular expression the server's greeting must match, such as `'^220[ -]'`: the greeting is read before `send` is written, and one that does not match fails the scrape. Without `send` the greeting is the answer. No placeholder; at most 64 KiB. See [Services that speak first](#services-that-speak-first). |
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
it, and a `tls` block, even an empty one, makes the scrape speak TLS:
`tls: {}` asks for TLS verified against the system's roots, never for
plaintext, so the label of such a scrape is `tls://host:port`.

```yaml
request:
  type: tcp
  send: "show stat\n"
  tls:
    ca_file: /etc/exporter/haproxy-ca.pem
```

## What a scrape sends

A scrape makes one connection, speaks TLS over it when the collector has a
`tls` block, even an empty one, reads the server's greeting when the
collector sets [`greeting`](#services-that-speak-first), writes `send` when
it is set, reads the answer and closes the
connection; the next scrape makes its own. `send` is written as it is,
nothing added: a protocol whose commands end with a line feed or `\r\n`
needs it written at the end of `send`, and ZooKeeper's four-letter words take
none. The write's end of the connection stays open while the answer is read,
since some servers stop answering a client that has closed it.

A placeholder in `send` is filled from the probe's `param_<name>`
parameter, as a path's is ([Path parameters](REQUESTS.md#path-parameters)),
with its default when the probe gives none. The value may hold only
letters, digits and `. _ - : / @ + = , %`; any other character, a blank, a
`;`, `|`, `&`, a quote, a `#`, a line break or another control character,
or a letter outside ASCII, is refused, answered `400` naming the parameter
and the character before the connection is made. Refusing line breaks alone
would not do: these servers split a line on more than its end. HAProxy's
runtime API runs each `;`-separated command of a line, so the example below
with `param_proxy=-1;disable server app/app1` would disable a server, and
memcached, ZooKeeper and NUT split a command's arguments on blanks, so a
value with a blank adds an argument. The characters allowed are those that
name a proxy, a server, a UPS or a key, and none of them separates or quotes,
so a value is always one argument of the command `send` writes. A default
is held to the same characters, checked when the collector loads. A probe
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
envelope unless [`document`](#the-exchange-as-a-document) asks for one,
with no status (`$status` is `null`, Python's `response.status_code`
`None`) and no headers of the server's: the only headers it carries are
the exporter's [timings](#timings-as-headers).

## Services that speak first

SMTP, FTP, POP3, IMAP and SSH servers greet a client before it says
anything, and a client that writes first is out of turn: an SMTP server may
refuse it as a spammer would be refused. `greeting` makes the scrape wait
for the greeting, check it, and only then write `send`:

```yaml
request:
  type: tcp
  greeting: '^220[ -]'   # what an SMTP server greets with when it serves
  send: "QUIT\r\n"
```

A scrape with `greeting` connects, speaks TLS when the collector has a
`tls` block, reads the greeting, matches it against the pattern, then writes
`send` and reads the answer as any scrape does. Without `send` nothing is
written: the greeting is the answer, the body the rules read, and nothing
is read after it, so a greeting-only check of a server that then waits for
the client, as an SMTP or SSH server does, ends as soon as it has greeted;
`terminator` is refused at load then, since there is nothing for it to
end.

The greeting is:

- the server's first line, through the line feed that ends it, `\r\n` or
  `\n` alone;
- or, when that line begins with three digits and a `-`, as an SMTP or FTP
  reply over several lines does (`220-mx.example.com ESMTP`), every line up
  to and including the first that begins with the same three digits and no
  `-` (`220 ready`, or `220` alone). The lines between may say anything:
  FTP allows them free text.

The pattern is a regular expression in [Go's syntax](https://pkg.go.dev/regexp/syntax)
(RE2, so no backreference or lookaround), matched against the whole
greeting as it was read, line ends included: `^` is the start of the
greeting and, without `(?m)`, `$` its end, after the last line feed.
`^220[ -]` takes an SMTP greeting on one line and over several; `^220 `
takes only one on one line. Write it in single quotes, in which YAML keeps
a `\` as it is. It holds no placeholder, and no probe parameter sets it:
the greeting is the configuration's check of the server.

A greeting that does not match fails the scrape in the `tcp` stage, and
`send` is never written:

```text
collector smtp tcp failed: the greeting of mx.example.com:25, "554 mx.example.com no service\r\n", does not match request.greeting "^220[ -]"
```

A probe's caller is told the pattern, with the greeting
[withheld](CONFIGURATION.md#what-a-failed-probes-caller-is-told), since it
is what the server said: `the greeting of mx.example.com:25, [withheld; in
the exporter's log], does not match request.greeting "^220[ -]"`. The log
and a debug probe's report show it. A server that closes the connection
before it has greeted, or in the middle of its greeting, fails the scrape
saying so, and so does one that has not greeted, or not ended its
greeting with a line feed, by the scrape's deadline. The greeting is held
to the [response limit](#limits) as the answer is, and the answer to it
again.

Over TLS the greeting comes after the handshake, as with SMTPS on port 465,
IMAPS on 993 and POP3S on 995, which speak TLS from the start. A protocol
that starts in plaintext and turns to TLS after a command, SMTP's
`STARTTLS`, is not spoken.

### The exchange as a document

`document: true` hands the transforms a JSON document of the exchange in
place of the server's text, so plain `jq` rules read the greeting, the
answer and how long each step took, with no script:

```json
{
  "greeting": "220-mx.example.com ESMTP\r\n220 ready\r\n",
  "answer": "221 2.0.0 Bye\r\n",
  "connect_seconds": 0.0012,
  "greeting_seconds": 0.0381,
  "answer_seconds": 0.0007,
  "duration_seconds": 0.0401
}
```

- `greeting` is the greeting as it was read, line ends included, with
  `greeting`; `answer` is what was read after it, or, without `greeting`,
  the whole answer, `""` when the server wrote nothing. A greeting-only
  check, with no `send`, has a `greeting` and no `answer`.
- `connect_seconds` is how long making the connection took, the name's
  lookup included; `tls_seconds` the TLS handshake, with `tls`;
  `greeting_seconds` from the connection, or the handshake, until the
  greeting ended, with `greeting`; `answer_seconds` from the write of
  `send`, or from when reading began without it, until reading stopped,
  when an answer was read; and `duration_seconds` the whole exchange, from
  the connection to the end of the read.
- A step the scrape did not take is left out, not written `null` or `0`,
  as a [DNS](DNS.md) record's fields of another type are: a rule reading
  `.tls_seconds` of a collector without `tls` finds nothing, which is a
  missing value.
- JSON text is UTF-8, so a greeting or an answer holding bytes that are not
  fails the scrape, `the answer of host:port holds bytes that are not UTF-8
  text, ...`, as a [Redis](REDIS.md) reply does, rather than reaching the
  rules with its bytes replaced. Leave `document` out to read them as they
  came.

The decoder is `json`, left out or written; any other is refused at load,
and so is a transform other than `jq`, `yq` and `python`:

```text
collector "smtp" decodes with text, but it is answered with the JSON document request.document makes of what a tcp server says; leave decoder.type out, or set json
```

### Timings as headers

Every `tcp` answer, with `document` or without, carries the same timings,
in seconds, and the greeting, as headers: a `tcp` answer has none of its
own, so these name nothing the server sent, and a collector reading only
the body sees the answer it always did.

| Header | When | What |
| --- | --- | --- |
| `X-Tcp-Connect-Seconds` | always | `connect_seconds`. |
| `X-Tcp-Tls-Seconds` | with `tls` | `tls_seconds`. |
| `X-Tcp-Greeting-Seconds` | with `greeting` | `greeting_seconds`. |
| `X-Tcp-Greeting` | with `greeting` | The greeting, a value for each of its lines, without its line end. |
| `X-Tcp-Answer-Seconds` | when an answer is read | `answer_seconds`. None for a greeting-only check. |

A `jq` or `yq` rule reads them as `$headers`, by lower-case name, as for an
`http` answer: `$headers["x-tcp-connect-seconds"] | tonumber`; a Python
script reads `response.header("X-Tcp-Connect-Seconds")`, or
`response.headers["X-Tcp-Greeting"]`, the greeting's lines as a list. They
are for a collector that reads the server's text itself, an answer in JSON
or one a Python script parses; a `regex` rule reads the body alone, and
`document: true` is the way to the timings without a script.

A debug probe's report shows the greeting step in its request section, the
pattern as `Tcp-Greeting` before `Tcp-Send`, and the greeting read before
how reading the answer stopped, and the headers in its response section:

```text
Requests
  1. SEND tcp://mx.example.com:25 -> greeted with "220 mx.example.com ESMTP\r\n", which matched request.greeting; read 15 bytes, the server closed the connection in 41ms
     Tcp-Greeting: "^220[ -]"
     Tcp-Send: "QUIT\r\n"
```

### Checking SMTP, FTP, POP3, IMAP and SSH

Each collector below checks that the service greets as it should and
answers a command that ends the session, and turns the timings of its
[document](#the-exchange-as-a-document) into series; a server that greets
otherwise, or not at all, fails the probe, which Prometheus's `up` for the
probe shows as `0`.

SMTP, port 25 or 587: a greeting with `220`, on one line or several, and
`221` to `QUIT`, after which the server closes the connection:

```yaml
  - name: smtp
    request:
      type: tcp
      greeting: '^220[ -]'
      send: "QUIT\r\n"
      document: true
    transform:
      type: jq
    metrics:
      - name: smtp_up
        description: 1 when the server greeted with 220 and answered QUIT with 221
        expression: 'if (.answer | startswith("221")) then 1 else 0 end'
      - name: tcp_connect_seconds
        expression: .connect_seconds
      - name: tcp_greeting_seconds
        expression: .greeting_seconds
      - name: tcp_answer_seconds
        expression: .answer_seconds
      - name: tcp_duration_seconds
        expression: .duration_seconds
```

```text
/probe?collector=smtp&target=mx.example.com:25
```

FTP, port 21, which greets as SMTP does, with `220`, and answers `QUIT`
with `221`:

```yaml
  - name: ftp
    request:
      type: tcp
      greeting: '^220[ -]'
      send: "QUIT\r\n"
      document: true
    transform:
      type: jq
    metrics:
      - name: ftp_up
        expression: 'if (.answer | startswith("221")) then 1 else 0 end'
      - name: tcp_connect_seconds
        expression: .connect_seconds
      - name: tcp_greeting_seconds
        expression: .greeting_seconds
```

POP3, port 110, `+OK` to greet and to `QUIT`. POP3S on 995 is the same
with `tls: {}`, and its handshake's time is `.tls_seconds`:

```yaml
  - name: pop3
    request:
      type: tcp
      greeting: '^\+OK'
      send: "QUIT\r\n"
      document: true
    transform:
      type: jq
    metrics:
      - name: pop3_up
        expression: 'if (.answer | startswith("+OK")) then 1 else 0 end'
      - name: tcp_connect_seconds
        expression: .connect_seconds
      - name: tcp_greeting_seconds
        expression: .greeting_seconds
```

IMAP, port 143, greeting `* OK`; `LOGOUT`, tagged `a`, is answered with an
untagged `* BYE` and `a OK`, and the server closes. IMAPS on 993 is the
same with `tls: {}`:

```yaml
  - name: imap
    request:
      type: tcp
      greeting: '^\* OK'
      send: "a LOGOUT\r\n"
      document: true
    transform:
      type: jq
    metrics:
      - name: imap_up
        expression: 'if (.answer | test("(?m)^a OK")) then 1 else 0 end'
      - name: tcp_connect_seconds
        expression: .connect_seconds
      - name: tcp_greeting_seconds
        expression: .greeting_seconds
```

SSH, port 22: the server's banner, `SSH-2.0-` and its software, is its
greeting, and the client is to speak next, so the check sends nothing and
the document has the greeting and no answer. The greeting below takes
OpenSSH 9.6 and later alone, so an older server fails the probe; the series
name the software:

```yaml
  - name: ssh
    request:
      type: tcp
      greeting: '^SSH-2\.0-OpenSSH_(9\.([6-9]|[1-9][0-9])|[1-9][0-9]+\.)'
      document: true
    transform:
      type: jq
    metrics:
      - name: ssh_banner_info
        description: 1, the software the server's banner names as a label
        expression: '1'
        labels:
          - name: software
            expression: '.greeting | capture("^SSH-2\\.0-(?<software>\\S+)").software'
      - name: tcp_connect_seconds
        expression: .connect_seconds
      - name: tcp_greeting_seconds
        expression: .greeting_seconds
```

```text
/probe?collector=ssh&target=bastion.example.com:22
```

Without `document`, a greeting-only check's body is the greeting itself,
which a `regex` rule reads as it came; here the version as a number:

```yaml
  - name: ssh_version
    request:
      type: tcp
      greeting: '^SSH-2\.0-'
    transform:
      type: regex
    metrics:
      - name: ssh_openssh_version
        expression: '^SSH-2\.0-OpenSSH_(\d+\.\d+)'
```

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
it is serving, from `show stat`'s CSV. Its header line begins `# pxname`,
which is the first column's name. Whether a frontend or a server serves is
the first word of its status, more than a `value_map`, which matches whole
texts, can read: HAProxy 2.8 writes `UP 1/3` for a server going down,
`NOLB`, `DRAIN` and `no check` for servers that serve, and `DOWN 1/2`,
`MAINT`, `MAINT (via app/app1)` and `DOWN (agent)` for ones that do not,
and `STOP` for a stopped frontend. A `pre_script`
([Python](PYTHON.md#what-data-is)) adds a column, `up`, for the rule to
read, `0` for a status that begins `DOWN`, `MAINT` or `STOP`, or is empty,
and `1` for any other:

```yaml
  - name: haproxy
    request:
      type: tcp
      send: "show stat\n"
    transform:
      type: csv
      pre_script: |
        def up(status):
            if not status or status.startswith(("DOWN", "MAINT", "STOP")):
                return 0
            return 1
        data = [dict(row, up=up(row.get("status"))) for row in data]
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
        description: 1 when the proxy or server is serving
        expression: up
        labels:
          - name: proxy
            expression: "# pxname"
          - name: server
            expression: svname
```

## Static targets

A [static target](STATIC-TARGETS.md) of a `tcp` collector names the service,
`host:port`, as its `target`, which it requires, and may set under its
`request` `timeout` and `tls`. What is sent and the terminator are the
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
  - name: zookeeper_secure
    collector: zookeeper
    target: zk-secure.zookeeper:2281
    request:
      tls:
        ca_file: /etc/exporter/zookeeper/ca.pem
        cert_file: /etc/exporter/zookeeper/client.pem
        key_file: /etc/exporter/zookeeper/client-key.pem
```

A target's `tls` replaces the collector's whole block for that target, a key
it leaves out not being taken from the collector's, and is checked when the
file loads as the collector's is. Written, even as `tls: {}`, it makes the
scrape speak TLS, so one collector reads services in plaintext and behind
TLS, each with its own CA or client certificate; a target cannot turn off
the TLS its collector asks for. Its files are read at each connection, and
by [`--dry-run`](CONFIGURATION.md#dry-run), which warns of them under the
target's name. They hold no placeholder, and no probe parameter sets them. A
target's block is part of its [cache key](CONFIGURATION.md#response-caching),
so targets with different blocks never share an answer, nor a trip to the
service.

## Limits

The answer is read against `max_response_bytes`, or
`limits.max_response_bytes`, 10 MiB when neither is set, as it arrives: an
answer that grows past it fails the scrape when it does, without waiting
for the rest, as a limit exceeded, which counts in
`http_exporter_series_limit_exceeded_total`.

The greeting is held to the limit too, on its own: a greeting that grows
past it fails the scrape when it does.

The connection, the TLS handshake, the greeting, the write and the read end at the
probe's deadline, its `timeout` or the scrape's, 30 seconds without one:
a server that stops answering, or keeps the connection open without the
terminator, fails the scrape when it comes, with the deadline's error.

## Errors and self-metrics

A failed scrape is reported in the `tcp` stage and follows
`error_handling.on_fetch_error`, saying what went wrong:
`connecting to memcached-0.cache:11211: ...`, `reading from
memcached-0.cache:11211: no answer ending with request.terminator "END\r\n"
came, 12 bytes read: ...`, `the server closed the connection after 12 bytes,
and the answer does not end with request.terminator "END\r\n"`, and, with
`greeting`, `the greeting of mx.example.com:25, "554 ...", does not match
request.greeting "^220[ -]"`, `the server mx.example.com:25 closed the
connection without a greeting; request.greeting expects one`, `waiting for
the greeting of mx.example.com:25, as request.greeting expects, 0 bytes
read: ...`. A service
the target lists refuse counts in `http_exporter_targets_refused_total`,
answered `403`.

A [debug probe](CONFIGURATION.md#debug-probes) shows in its request section
the bytes sent, quoted so that a carriage return and a line feed are seen,
and how many bytes were read and how reading stopped, after the greeting
step with `greeting` ([Timings as headers](#timings-as-headers));
its response section shows the answer and its headers:

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

- **One line, one answer.** A scrape reads a greeting when asked, writes
  `send` once and reads one answer; a dialogue, a login, `STARTTLS`, or an
  answer read after a second command is not spoken. Write every command into `send` where the server takes them
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
| `the greeting of ..., [withheld; in the exporter's log], does not match request.greeting ...` | The server greeted otherwise: the exporter's log, or a [debug probe](CONFIGURATION.md#debug-probes), shows what it said. Over several lines the pattern is matched against them all: `^220[ -]` takes `220-` on the first. |
| `waiting for the greeting of ..., as request.greeting expects, 0 bytes read` | The server says nothing first: it is not a service that greets, or it greets only over TLS, which `tls` asks for. |
| `... no answer ending with request.terminator "END\r\n" came, ...` | The terminator: written in double quotes, so that `\r\n` is a carriage return and a line feed, and the end of the whole answer. Or `send`: the server did not take it, and waits for more, often a line feed it lacks. |
| `... the server kept the connection open, ...; without request.terminator ...` | The server does not close after answering: set `terminator`. |
| `the server closed the connection after 0 bytes, ...` | The server refused what was sent: a ZooKeeper whose `4lw.commands.whitelist` does not name the word answers nothing and closes. |
| `... the answer of ... is larger than the response limit ...` | Raise `max_response_bytes`, or send a command that answers less. |
| `request.send: the value of param_<name> holds a carriage return or a line feed ...` | The probe's parameter: a value may not end the line. |
| `TLS handshake with ...: ... certificate signed by unknown authority` | Name the server's CA in `tls.ca_file`. |
