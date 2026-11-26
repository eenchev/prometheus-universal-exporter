# Text protocols over TCP

The `tcp` request type reads a service that answers a line sent over a TCP
connection with its statistics, as text. It connects, writes the line that
the collector names, reads the answer until it ends with a terminator or the
server closes the connection, and hands the answer to the decoder and the
transform as an `http` collector's body is handed to them. Use it where a
service exposes its numbers this way and a dedicated exporter would be one
more thing to run:

- memcached's `stats`;
- ZooKeeper's four-letter words, such as `mntr`;
- HAProxy's runtime API on a TCP socket, `show stat`, which answers CSV;
- NUT's `upsd`, `LIST VAR <ups>`, and Beanstalkd's `stats`;
- whether a service that speaks first (SMTP, FTP, POP3, IMAP, SSH) greets as
  it should, and how long it took
  ([Services that speak first](#services-that-speak-first));
- whether a mail or FTP server that begins TLS on request, with `STARTTLS`,
  does, with a certificate that verifies ([STARTTLS](#starttls));
- a service that answers over UDP, such as a game server's query
  ([Over UDP](#over-udp)), or on a Unix socket, as HAProxy's runtime API
  often is ([Over a Unix socket](#over-a-unix-socket));
- a binary protocol, such as Modbus TCP, written in hexadecimal and read by
  a layout of its fields ([Binary protocols](#binary-protocols));
- whether a port, or each of a host's ports, takes a connection, sending
  nothing ([Checking that a port is open](#checking-that-a-port-is-open)).

Anything that reads a body can read the answer: `regex` for lines of text,
`csv`, `prometheus` for a service that writes exposition, `jq` for one that
answers JSON, `yq`, `python`, and `binary` for a binary answer.

It needs nothing outside Go's standard library.

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
with `(?m)`, `$` matches only before a line feed. memcached counts hits and
misses, not their ratio, so the ratio is
[derived](CONFIGURATION.md#derived-series) from the two, over their hidden
sum; a server that has had no get yet has no ratio.

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `tcp`. |
| `connect_only` | `false` | `true` connects, makes the TLS handshake with `tls`, and closes, writing and reading nothing: the answer is a JSON document of whether the port was open. Not with `send`, `send_hex`, `steps`, `greeting`, `terminator`, `answer_bytes`, `starttls`, `udp` or `unix_socket`. See [Checking that a port is open](#checking-that-a-port-is-open). |
| `connect_timeout` | `5s` | With `connect_only`, how long the connection and its handshake may take before the port counts as not open, at least `10ms`, and at most nine tenths of the time the scrape has left. |
| `ports` | none | With `connect_only`, the ports of the target's host dialed at once, such as `[22, 443, 5432]`, the target then being the host alone. At most 64. See [Several ports of a host](#several-ports-of-a-host). |
| `document` | `false` | `true` hands the transforms a JSON document of the exchange, the greeting, the answer and each step's seconds, in place of the server's text; the decoder is then `json`. See [The exchange as a document](#the-exchange-as-a-document). |
| `greeting` | none | A regular expression the server's greeting must match, such as `'^220[ -]'`: the greeting is read before `send` is written, and one that does not match fails the scrape. Without `send` or `send_hex` the greeting is the answer, and `terminator` and `answer_bytes` are refused. No placeholder; at most 64 KiB. See [Services that speak first](#services-that-speak-first). |
| `starttls` | none | `smtp`, `imap`, `pop3` or `ftp`: begin TLS on the plaintext connection, after the greeting, as that protocol asks for it, and write `send` over TLS. See [STARTTLS](#starttls). |
| `send` | nothing | The text written once the connection is made, such as `"stats\r\n"`. Write it in double quotes, in which YAML reads `\r\n` as a carriage return and a line feed; in single quotes or none, `\r\n` is four characters. May hold `{{param_name}}` placeholders. At most 64 KiB. Left out, nothing is written, and the answer is what the server says first. See [What a scrape sends](#what-a-scrape-sends). |
| `send_hex` | nothing | The bytes written, as pairs of hexadecimal digits, with blanks between them or none: `"00 01 00 00 00 06 01 03 00 00 00 02"`. In place of `send`, for a protocol whose requests are binary; no placeholder. At most 64 KiB. See [Binary protocols](#binary-protocols). |
| `steps` | none | A conversation of several exchanges, each a `send` and the `until` its answer matches, in place of `send` and of where the answer ends. See [A conversation](#a-conversation). |
| `terminator` | none | What the answer ends with, such as `"END\r\n"`: reading stops once it has come. Left out, reading stops when the server closes the connection. At most 64 KiB, and no placeholder. See [Reading the answer](#reading-the-answer). |
| `answer_bytes` | none | How many bytes the answer is: reading stops once that many have come, for a server that keeps the connection open after an answer of a known length. Not with `terminator`. See [Binary protocols](#binary-protocols). |
| `udp` | `false` | `true` sends `send` or `send_hex` as one UDP datagram and takes the first datagram back as the answer. See [Over UDP](#over-udp). |
| `unix_socket` | none | The absolute path of a Unix socket the exchange is made over, in place of a connection to the target. See [Over a Unix socket](#over-a-unix-socket). |
| `tls` | none | Speak TLS, verifying the server: see [TLS](#tls). With `starttls`, the settings of the TLS it begins. |
| `max_response_bytes` | `10MiB` | The most bytes of the answer: see [Limits](#limits). |
| `allowed_targets`, `denied_targets` | none | The services the collector may and may not reach, as for [any type that reaches a host](REQUESTS.md#restricting-targets), checked on the connection. |

No other request key applies, and setting one is a startup error. A key
written `""` is the same as leaving it out.

The decoder is `decoder.type`, or the one that the transform implies (`text`
for `regex`, `csv` for `csv`, `prometheus` for `prometheus`), or, for `jq`,
`yq` and `python` without one, what the answer reads as, JSON when it is
JSON: the answer has no `Content-Type` to go by.

## The target

The probe's `target` is the service: `host:port` or `[v6]:port`.

- The port is required, because no port is common to every such service.
- A collector with [`unix_socket`](#over-a-unix-socket) takes a host without
  a port, because its target only names the service.
- A collector with [`ports`](#several-ports-of-a-host) takes the host alone,
  `[v6]` or a bare IPv6 address, because the ports come from the list.
- A scheme, a path, a query or a user in the target is refused with `400`,
  before anything is sent.
- `allowed_targets` and `denied_targets` are matched against the host before
  the connection is made and against the address that it is made to, and the
  cloud metadata service is refused without them, as for `http`.

## TLS

A `tls` block makes the scrape use TLS, with the server's certificate
verified against the system's roots and `tls.ca_file`, for the name
`tls.server_name` or else the target's host. The block is
[the one that `http` takes](REQUESTS.md#tls): `cert_file` and `key_file`
present a client certificate (both or neither), and `insecure_skip_verify`
skips the verification.

There is no scheme for TLS in the target: only the block requests it, and a
`tls` block, even an empty one, makes the scrape use TLS. `tls: {}` requests
TLS verified against the system's roots, never plaintext, so the label of
such a scrape is `tls://host:port`. With [`starttls`](#starttls), the block
holds the settings of the TLS begun after the greeting instead of from the
start.

```yaml
request:
  type: tcp
  send: "show stat\n"
  tls:
    ca_file: /etc/exporter/haproxy-ca.pem
```

## What a scrape sends

A scrape makes one connection and:

1. uses TLS over it when the collector has a `tls` block, even an empty one;
2. reads the server's greeting when the collector sets
   [`greeting`](#services-that-speak-first) or [`starttls`](#starttls);
3. begins TLS with `starttls`;
4. writes `send` when it is set;
5. reads the answer and closes the connection.

The next scrape makes its own connection. `send` is written as it is, with
nothing added: a protocol whose commands end with a line feed or `\r\n` needs
it written at the end of `send`, and ZooKeeper's four-letter words take
none. The writing end of the connection stays open while the answer is read,
because some servers stop answering a client that has closed it.

A placeholder in `send` is filled from the probe's `param_<name>` parameter,
as a path's is ([Path parameters](REQUESTS.md#path-parameters)), with its
default when the probe gives none.

- The value may hold only letters, digits and `. _ - : / @ + = , %`. Any
  other character (a blank, a `;`, `|`, `&`, a quote, a `#`, a line break or
  another control character, or a letter outside ASCII) is refused with
  `400`, naming the parameter and the character, before the connection is
  made.
- Refusing only line breaks would not be enough, because these servers split
  a line on more than its end. HAProxy's runtime API runs each
  `;`-separated command of a line, so the example below with
  `param_proxy=-1;disable server app/app1` would disable a server.
  memcached, ZooKeeper and NUT split a command's arguments on blanks, so a
  value with a blank adds an argument.
- The allowed characters are those that name a proxy, a server, a UPS or a
  key, and none of them separates or quotes, so a value is always one
  argument of the command that `send` writes.
- A default is held to the same characters, checked when the collector
  loads.
- A probe parameter that no placeholder of `send` or of a label value names
  is refused, as for every type.

A `send` that its placeholders leave empty, `"{{param_cmd:}}"` with no
`param_cmd` on the probe, is refused too, with `400` naming the parameters,
before the connection is made: `request.send is empty once param_cmd is
filled in, ...`. Writing nothing would turn the scrape into a read of what
the server says first, which a server waiting for a command ends only at the
deadline, even though the collector says it sends. A static target that
leaves it empty is refused at load. Give the placeholder a non-empty
default, or write the command's fixed part around it.

```yaml
  - name: haproxy_proxy
    request:
      type: tcp
      send: "show stat {{param_proxy:-1}} -1 -1\n"
```

asks HAProxy for the proxy that the probe names, `&param_proxy=web`, or for
every proxy without one. The terminator holds no placeholder: it is matched
as it is written.

| Service | `send` | `terminator` |
| --- | --- | --- |
| memcached | `"stats\r\n"` | `"END\r\n"` |
| ZooKeeper | `mntr`, `ruok`, `srvr` | none: the server closes |
| HAProxy's runtime API | `"show stat\n"`, `"show info\n"` | none: the server closes |
| NUT `upsd` | `"LIST VAR ups\n"` | `"END LIST VAR ups\n"` |
| Beanstalkd | `"stats\r\nquit\r\n"` | none: the server closes after `quit` |

## Reading the answer

With `terminator`, reading stops once the answer read so far ends with it.
The terminator stays in the body, where a rule can match the last line by
it. A server that closes the connection before the terminator has come has
cut its answer short, and the scrape fails, saying how many bytes came. A
terminator is matched at the end of what was read, so a server that writes
more after it in the same answer is read until the deadline: name the end of
the whole answer.

Without `terminator`, reading stops when the server closes the connection,
as ZooKeeper does after a four-letter word and HAProxy's runtime API after a
command. A server that keeps the connection open waiting for the next
command, as memcached does, holds the scrape until its deadline, which
fails it: give such a service its `terminator`.

Either way, the answer is the body, as the server wrote it: in no JSON
envelope unless [`document`](#the-exchange-as-a-document) requests one, with
no status (`$status` is `null`, and Python's `response.status_code` is
`None`) and no headers from the server. Its only headers are the exporter's
[timings](#timings-as-headers).

## A conversation

Some services provide what a metric needs only after more than one exchange:
SMTP lists its extensions in its answer to `EHLO`, and a polite client then
says `QUIT`; POP3 answers `CAPA`, and IMAP `CAPABILITY`, after their
greetings. `steps` is such a conversation: each step is a `send` and an
`until`, a regular expression that its answer matches once it has fully
arrived:

```yaml
    request:
      type: tcp
      greeting: '^220 '
      steps:
        - send: "EHLO {{param_client:exporter.example}}\r\n"
          until: '(?m)^250 '
        - send: "QUIT\r\n"
    transform:
      type: regex
    metrics:
      - name: smtp_size_limit_bytes
        expression: '(?m)^250[ -]SIZE (\d+)\r$'
```

- The steps run after the greeting and `starttls`, when the collector has
  them, and each `send` is written in order. A step's answer is read until
  what has arrived matches its `until`, across as many reads as needed, and
  the next step is written after it. The last step may leave out `until`:
  its answer is read until the server closes the connection, as `QUIT`'s is.
- The answer that the rules read is the steps' answers, one after another.
  With [`document`](#the-exchange-as-a-document), `steps` also holds each
  step's `send`, `answer` and `seconds`.
- A step's `send` follows the rules of `send`, including `{{param_name}}`
  placeholders, and `until` those of `greeting`: a pattern with no
  placeholder. Every step except the last needs `until`.
- An answer that never matches its `until` fails the scrape at the deadline,
  quoting what arrived; a server that closes the connection before it
  matches fails it immediately. The answers are held to the response limit
  together.
- `steps` replaces `send`, `send_hex`, `terminator` and `answer_bytes`, and
  is not available over UDP. There are at most 16 steps.

## Over UDP

With `udp: true`, `send` or `send_hex` goes to `host:port` as one UDP
datagram, and the first datagram that comes back is the answer, read by any
decoder, like a TCP answer. A UDP server speaks only when spoken to, so a
collector with `udp` must write something; `greeting`, `terminator`,
`answer_bytes`, `tls`, `starttls` and `unix_socket` are refused beside it.

```yaml
collectors:
  - name: memcached_udp
    request:
      type: tcp
      udp: true
      # memcached's UDP frame header: request id, sequence, count, reserved.
      send_hex: "00 01 00 00 00 01 00 00 73 74 61 74 73 0d 0a"   # stats\r\n
    decoder:
      type: text
    transform:
      type: regex
    metrics:
      - name: memcached_curr_items
        expression: 'STAT curr_items (\d+)'
```

The socket is connected to the target's address, so only its datagrams are
read, and the target lists are checked on it as on a TCP connection. A port
where no server listens answers with ICMP port unreachable, which fails the
scrape immediately (`connection refused`) instead of at its deadline; a
server that drops the datagram, or whose answer is lost, fails at the
deadline. A datagram beyond the response limit fails the scrape as an
exceeded limit. The scrape is labelled `url="udp://host:port"`.

## Over a Unix socket

`unix_socket` makes the exchange over a Unix domain socket on the
exporter's side instead of a TCP connection: HAProxy's runtime API on
`/var/run/haproxy.sock`, or a service's control socket. It is the absolute
path of the socket, checked as an
[`http` collector's](REQUESTS.md#over-a-unix-socket) is, and set only in the
configuration: no probe parameter, static target or `defaults` block sets
it, so whoever can reach `/probe` never names the file that the exporter
connects to.

```yaml
collectors:
  - name: haproxy_info
    request:
      type: tcp
      unix_socket: /var/run/haproxy.sock
      send: "show info\n"
    decoder:
      type: text
    transform:
      type: regex
    metrics:
      - name: haproxy_uptime_seconds
        expression: 'Uptime_sec: (\d+)'
```

The target names the service and is never dialed: a host, with or without a
port, such as `target=haproxy`. `allowed_targets` and `denied_targets`
apply to its name as written, which is never looked up, so a name that the
list does not name is refused even when the list holds networks. Everything
else works as in a TCP exchange: `greeting`, `starttls`, `tls` over the
socket, `send`, `terminator` and `answer_bytes`. The scrape is labelled
`url="unix:///var/run/haproxy.sock"`, or `tls+unix://` with TLS. A socket
that cannot be reached fails the scrape with an error that names its path
in the exporter's log, and a probe's caller gets the error with the path
withheld.

## Binary protocols

`send_hex` writes the bytes of a binary request, and the
[`binary` decoder](CONFIGURATION.md#binary-answers) reads the answer's
fields by a layout. Many binary servers keep the connection open after an
answer that ends with no terminator; `answer_bytes` ends it at the length
that the protocol gives. This example is a Modbus TCP read of two holding
registers from unit 1, whose answer is 9 bytes plus 2 for each register:

```yaml
collectors:
  - name: modbus
    request:
      type: tcp
      send_hex: "0001 0000 0006 01 03 0000 0002"   # read 2 registers from 0
      answer_bytes: 13
    decoder:
      type: binary
    response:
      binary:
        fields:
          - {type: skip, length: 7}     # the MBAP header and the unit
          - {name: function, type: u8}
          - {name: bytes, type: u8}
          - {name: registers, type: u16, count: 2}
    transform:
      type: jq
    metrics:
      - name: modbus_register
        expression: .registers[0]
```

A server that closes the connection before `answer_bytes` have arrived
fails the scrape, saying how many bytes came; one that sends fewer and keeps
the connection open fails at the deadline. An `answer_bytes` beyond the
response limit fails the scrape before anything is read. A Modbus exception
(the function with its high bit set, `0x83`, and one byte of its code) is 9
bytes, fewer than `answer_bytes`, so the server's refusal of a read fails
the scrape at its deadline as an answer that did not arrive whole.

## Checking that a port is open

`connect_only: true` makes the scrape a check that a port accepts a
connection, like a monitoring system's port check. It dials the target,
makes the TLS handshake when the collector has a `tls` block, and closes,
writing and reading nothing, so it suits any service, whatever it speaks.
The answer is a JSON document of what it found:

```yaml
collectors:
  - name: port
    request:
      type: tcp
      connect_only: true
    transform:
      type: jq
    metrics:
      - name: port_open
        description: 1 when the port took a connection
        expression: 'if .open then 1 else 0 end'
      - name: port_connect_seconds
        expression: .connect_seconds
```

```text
/probe?collector=port&target=db.example:5432
```

```json
{"open":true,"address":"192.0.2.10:5432","connect_seconds":0.0011,"duration_seconds":0.0013}
{"open":false,"reason":"refused","error":"dial tcp 192.0.2.10:5432: connect: connection refused","duration_seconds":0.0004}
```

- `open` is whether the connection was made and, with `tls`, whether the
  handshake succeeded too.
- `reason` and `error` say why a port is not open:
  - `refused`: the host answered that nothing listens there;
  - `timeout`: no answer within `connect_timeout`, as for a port that a
    firewall drops, or a lookup of the name that took as long;
  - `unreachable`: the network or the host has no route;
  - `reset`: the connection was reset as it was made;
  - `tls`: the connection was made but the handshake failed, as it does for
    a certificate that does not verify;
  - `error`: anything else.

  Both are left out of an open port's document.
- `address` is the address that the connection reached, as chosen by the
  name's lookup, and `connect_seconds` how long making it took, including the
  lookup, when the connection was made, even if the handshake then failed.
  `tls_seconds` and `tls_version`, such as `TLS 1.3`, describe the
  handshake, with `tls`.
- `duration_seconds` is the whole scrape.

**A port that is not open is an answer, not a failed scrape.** The scrape
answers `"open": false`, which a rule turns into 0, so the series is 0 or 1
at every scrape and an alert is on `port_open == 0` instead of on a missing
series, as [DNS](DNS.md) answers a `SERVFAIL`. What fails the scrape is
being unable to ask: a target that the [target lists](#the-target) refuse,
which is never connected to, a name that does not resolve, and a probe cut
short.

`connect_timeout`, `5s` when left out and at least `10ms`, is how long the
connection and its handshake may take before the port counts as not open,
with `reason` `timeout`. Within the probe's deadline it is at most nine
tenths of the time the scrape has left, so the document is answered before
the deadline ends the scrape: a probe with `timeout=2s` waits at most
`1.8s`.

`connect_only` does not combine with any key of what is sent or read
(`send`, `send_hex`, `steps`, `greeting`, `terminator`, `answer_bytes`,
`starttls`), nor with `udp`, which has no connection to make, nor with
`unix_socket`; each is refused at load. `document` may be written and changes
nothing: the decoder is `json`, and the transform `jq`, `yq` or `python`. A
debug probe shows the scrape as `CONNECT`, with its outcome `open, connected
to 192.0.2.10:5432` or `not open, refused: ...`.

### Several ports of a host

`ports` lists the ports of the target's host to dial, all at once, each with
its own connection, so one target covers a whole host instead of one target
per port. The target is then the host alone, `db.example`, `192.0.2.10` or
`[2001:db8::1]`; one with a port is refused, because the ports come from the
list. There are at most 64, each listed once:

```yaml
collectors:
  - name: host_ports
    request:
      type: tcp
      connect_only: true
      ports: [22, 443, 5432]
    transform:
      type: jq
    metrics:
      - name: port_open
        description: 1 when the port took a connection
        items: '.ports | to_entries[]'
        expression: 'if .value.open then 1 else 0 end'
        labels:
          - name: port
            expression: .key
      - name: ports_open
        description: How many of the ports took a connection
        expression: .open_ports
```

```json
{"open":false,"open_ports":2,"duration_seconds":0.0021,"ports":{
  "22":{"open":true,"address":"192.0.2.10:22","connect_seconds":0.0010},
  "443":{"open":true,"address":"192.0.2.10:443","connect_seconds":0.0012},
  "5432":{"open":false,"reason":"refused","error":"dial tcp 192.0.2.10:5432: connect: connection refused"}}}
```

`ports` holds each port's document, keyed by its number, with the same
fields as the document of one port. `open` is whether every port was open,
and `open_ports` how many were. Each port's connection is held to the target
lists, and a `tls` block makes the handshake on every one. A debug probe,
and a trace, show each port as its own `CONNECT` request,
`tcp://db.example:22` and so on, made at once; the scrape's label is the
host, `tcp://db.example`.

### With a ping

[`icmp`](ICMP.md) pings a host. A port check and a ping of the same host are
two collectors, the ping distinguishing a host that is down from one whose
service is down, and a [static target](STATIC-TARGETS.md) of each covers a
host:

```yaml
collectors:
  - name: ping
    request:
      type: icmp
      count: 3
    transform:
      type: jq
    metrics:
      - name: ping_up
        expression: 'if .received > 0 then 1 else 0 end'
      - name: ping_rtt_avg_seconds
        expression: .rtt_avg_seconds
  - name: host_ports
    request:
      type: tcp
      connect_only: true
      ports: [22, 443, 5432]
    transform:
      type: jq
    metrics:
      - name: port_open
        items: '.ports | to_entries[]'
        expression: 'if .value.open then 1 else 0 end'
        labels:
          - name: port
            expression: .key
```

```yaml
interval: 1m
targets:
  - name: db_ping
    collector: ping
    target: db.example
  - name: db_ports
    collector: host_ports
    target: db.example
```

## Services that speak first

SMTP, FTP, POP3, IMAP and SSH servers greet a client before it says
anything, and a client that writes first is out of turn: an SMTP server may
refuse it as it would refuse a spammer. `greeting` makes the scrape wait for
the greeting, check it, and only then write `send`:

```yaml
request:
  type: tcp
  greeting: '^220[ -]'   # what an SMTP server greets with when it serves
  send: "QUIT\r\n"
```

A scrape with `greeting` connects, uses TLS when the collector has a `tls`
block, reads the greeting, matches it against the pattern, then writes
`send` and reads the answer like any scrape. Without `send` nothing is
written: the greeting is the answer, the body that the rules read, and
nothing is read after it. A greeting-only check of a server that then waits
for the client, as an SMTP or SSH server does, therefore ends as soon as it
has greeted, and `terminator` is refused at load, because there is nothing
for it to end.

The greeting is:

- the server's first line, through the line feed that ends it (`\r\n` or
  `\n` alone);
- or, when that line begins with three digits and a `-`, as an SMTP or FTP
  reply over several lines does (`220-mx.example.com ESMTP`), every line up
  to and including the first that begins with the same three digits and no
  `-` (`220 ready`, or `220` alone). The lines between may say anything,
  because FTP allows them free text.

The pattern is a regular expression in [Go's syntax](https://pkg.go.dev/regexp/syntax)
(RE2, so no backreference or lookaround). It is matched against the whole
greeting as it was read, line ends included:

- `^` is the start of the greeting and, without `(?m)`, `$` is its end,
  after the last line feed.
- With `(?m)`, `$` matches before a line feed, and so after the `\r` of a
  line ending in `\r\n`, as SMTP, FTP, POP3, IMAP and SSH lines do:
  `(?m)^220 ready$` does not match `220 ready\r\n`. Write `\r?$` to end a
  line either way: `(?m)^220 ready\r?$`.
- `^220[ -]` takes an SMTP greeting on one line or over several; `^220 `
  takes only a greeting on one line.

Write the pattern in single quotes, in which YAML keeps a `\` as it is. It
holds no placeholder, and no probe parameter sets it: the greeting is the
configuration's check of the server.

A greeting that does not match fails the scrape in the `tcp` stage, and
`send` is never written:

```text
collector smtp tcp failed: the greeting of mx.example.com:25, "554 mx.example.com no service\r\n", does not match request.greeting "^220[ -]"
```

A probe's caller is told the pattern, with the greeting
[withheld](CONFIGURATION.md#what-a-failed-probes-caller-is-told) because it
is what the server said: `the greeting of mx.example.com:25, [withheld; in
the exporter's log], does not match request.greeting "^220[ -]"`. The log
and a debug probe's report show it.

The scrape also fails, saying so, when the server closes the connection
before or in the middle of its greeting, and when it has not greeted, or not
ended its greeting with a line feed, by the scrape's deadline. The greeting
is held to the [response limit](#limits) as the answer is, and the answer
after it is held to the limit again.

Over TLS the greeting comes after the handshake, as with SMTPS on port 465,
IMAPS on 993 and POP3S on 995, which speak TLS from the start. A server that
starts in plaintext and turns to TLS when asked is read with
[`starttls`](#starttls), and the greeting then comes before the handshake.

### The exchange as a document

`document: true` hands the transforms a JSON document of the exchange
instead of the server's text, so plain `jq` rules can read the greeting, the
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
  `greeting`. `answer` is what was read after it or, without `greeting`, the
  whole answer, `""` when the server wrote nothing. A greeting-only check,
  with no `send`, has a `greeting` and no `answer`.
- `starttls` is the protocol whose request began TLS, with `starttls`.
- The timings, in seconds:
  - `connect_seconds`: making the connection, the name's lookup included;
  - `tls_seconds`: the TLS handshake, with `tls` or `starttls`;
  - `greeting_seconds`: from the connection, or the handshake, until the
    greeting ended, with `greeting` or `starttls`;
  - `starttls_seconds`: from the end of the greeting until the server said
    to begin TLS, with `starttls`;
  - `answer_seconds`: from the write of `send`, or from when reading began
    without it, until reading stopped, when an answer was read;
  - `duration_seconds`: the whole exchange, from the connection to the end
    of the read.
- A step that the scrape did not take is left out, not written `null` or
  `0`, as a [DNS](DNS.md) record's fields of another type are. A rule
  reading `.tls_seconds` of a collector without `tls` finds nothing, which
  is a missing value.
- JSON text is UTF-8, so a greeting or an answer that holds bytes that are
  not UTF-8 fails the scrape (`the answer of host:port holds bytes that are
  not UTF-8 text, ...`), as a [Redis](REDIS.md) reply does, instead of
  reaching the rules with its bytes replaced. Leave `document` out to read
  them as they came.
- The document is held to the [response limit](#limits), since it is the
  body. The greeting and the answer share the limit as they are read, the
  answer getting what the greeting left. The document they make must be
  within the limit too; in it, JSON writes a control character in six bytes
  (`\u0001`) and adds its keys and timings. Going past either fails the
  scrape as an exceeded limit: `the greeting and the answer of host:port are
  larger together than the response limit ...`, or `the JSON document
  request.document makes of what host:port said is 2412 bytes, larger than
  the response limit ...`.

The decoder is `json`, whether it is left out or written. Any other decoder
is refused at load, and so is a transform other than `jq`, `yq` and
`python`:

```text
collector "smtp" decodes with text, but it is answered with the JSON document request.document makes of what a tcp server says; leave decoder.type out, or set json
```

### Timings as headers

Every `tcp` answer, with `document` or without, carries the same timings, in
seconds, and the greeting, as headers. A `tcp` answer has no headers of its
own, so these name nothing the server sent, and a collector that reads only
the body sees the same answer as before.

| Header | When | What |
| --- | --- | --- |
| `X-Tcp-Connect-Seconds` | always | `connect_seconds`. |
| `X-Tcp-Tls-Seconds` | with `tls` or `starttls` | `tls_seconds`. |
| `X-Tcp-Greeting-Seconds` | with `greeting` or `starttls` | `greeting_seconds`. |
| `X-Tcp-Greeting` | with `greeting` or `starttls` | The greeting, a value for each of its lines, without its line end. |
| `X-Tcp-Starttls-Seconds` | with `starttls` | `starttls_seconds`. |
| `X-Tcp-Answer-Seconds` | when an answer is read | `answer_seconds`. None for a greeting-only check. |

A `jq` or `yq` rule reads them as `$headers`, by lower-case name, as for an
`http` answer: `$headers["x-tcp-connect-seconds"] | tonumber`. A Python
script reads `response.header("X-Tcp-Connect-Seconds")`, or
`response.headers["X-Tcp-Greeting"]` for the greeting's lines as a list. The
headers are for a collector that reads the server's text itself, as an
answer in JSON or one that a Python script parses. A `regex` rule reads the
body alone; `document: true` is the way to the timings without a script.

A debug probe's report shows the greeting step in its request section (the
pattern as `Tcp-Greeting` before `Tcp-Send`, and the greeting read before
how reading the answer stopped) and the headers in its response section:

```text
Requests
  1. SEND tcp://mx.example.com:25 -> greeted with "220 mx.example.com ESMTP\r\n", which matched request.greeting; read 15 bytes, the server closed the connection in 41ms
     Tcp-Greeting: "^220[ -]"
     Tcp-Send: "QUIT\r\n"
```

### Checking SMTP, FTP, POP3, IMAP and SSH

Each collector below checks that the service greets as it should and
answers a command that ends the session, and turns the timings of its
[document](#the-exchange-as-a-document) into series. A server that greets
otherwise, or not at all, fails the probe, and Prometheus's `up` for the
probe shows `0`.

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

IMAP, port 143, greeting `* OK`. `LOGOUT`, tagged `a`, is answered with an
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
greeting, and the client is to speak next. The check therefore sends
nothing, and the document has the greeting and no answer. The greeting below
takes only OpenSSH 9.6 and later, so an older server fails the probe; the
series name the software:

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
which a `regex` rule reads as it came. Here it reads the version as a number:

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

## STARTTLS

SMTP on 25 and 587, IMAP on 143, POP3 on 110 and FTP on 21 start in
plaintext and begin TLS when the client asks. `starttls` names the protocol
whose request the scrape speaks. The scrape reads the greeting, asks for
TLS, makes the handshake, and only then writes `send`, over TLS:

| `starttls` | The greeting must begin | Sent before TLS | The server's yes |
| --- | --- | --- | --- |
| `smtp` | `220` | `EHLO [address]`, then `STARTTLS` once the answer, `250`, lists it | `220` |
| `imap` | `* OK` | `a1 STARTTLS` | `a1 OK`, the tag in any case, after any untagged lines |
| `pop3` | `+OK` | `STLS` | `+OK` |
| `ftp` | `220` | `AUTH TLS` | `234` |

- **The greeting** is read as with
  [`greeting`](#services-that-speak-first), over several lines for SMTP and
  FTP, whether `greeting` is set or not. It must begin as the table says.
  Anything else, such as an SMTP server's `554` or an IMAP server's
  `* PREAUTH` (which has no `STARTTLS` to offer), fails the scrape before
  anything is sent. `greeting`, when set, is matched against it too.
- **An FTP server may first say when it will be ready**, with `120`, in one
  reply or several, as RFC 959 allows. With `starttls: ftp` those replies
  are read past, held to the limit and the deadline with the greeting. The
  greeting is then the `220` after them: it is what must begin `220`, what
  `greeting` is matched against, and what the answer and its `greeting`
  hold. A debug probe's report shows the replies read past. Without
  `starttls` nothing is read past: the first reply is the greeting.
- **An SMTP client names itself in `EHLO`.** The exporter gives its own
  address on the connection, `[10.1.2.3]` or `[IPv6:2001:db8::7]`, as RFC
  5321 says a client without a name that the server can look up should do;
  a pod's or a machine's host name is not such a name. The answer must be
  `250` and list `STARTTLS`, in any case, on one of the lines after the
  first, which names the server.
- **IMAP's capabilities are not asked for.** A server without `STARTTLS`
  answers `NO` or `BAD`, which fails the scrape as plainly, with one command
  less in plaintext. IMAP tags are case-sensitive, but a server that answers
  `A1 OK` for `a1` has answered the command, so the tag is matched in any
  case. Otherwise the answer would be taken for an untagged line and the
  scrape would wait for another until its deadline.
- **The handshake** takes the `tls` block's settings (`ca_file`, a client
  certificate, `server_name`, `insecure_skip_verify`), as TLS from the start
  does ([TLS](#tls)). Without a block it verifies the server against the
  system's roots for the target's host, and never skips TLS. The label is
  `tls://host:port`.
- **Only the commands of the table are written before the handshake.**
  `send`, which a probe's values may fill, goes over TLS only. Without
  `send` the scrape ends with the handshake, which checks that the server
  begins TLS with a certificate that verifies; the greeting is then the
  answer, and `terminator` is refused at load.
- **A server that does not begin TLS fails the scrape.** One that does not
  list `STARTTLS`, refuses it (SMTP's `454`, IMAP's `NO`, POP3's `-ERR`,
  FTP's `534`) or answers anything else fails the scrape in the `tcp`
  stage. The error names the step and quotes the server's answer, which a
  probe's caller is told
  [withheld](CONFIGURATION.md#what-a-failed-probes-caller-is-told) and the
  log and a debug probe's report show. The scrape never goes on in
  plaintext, so a network that strips `STARTTLS` from the server's answers
  causes a failed scrape, not a plaintext one.
- **A server that writes more after its yes**, before the handshake, fails
  the scrape. Those bytes came in plaintext, where anything on the way could
  have written them, and would otherwise be read as the start of what the
  server says over TLS.
- **Each answer** is held to the [response limit](#limits) and the scrape's
  deadline, as the greeting is.
- **`starttls` belongs to the collector alone**: no static target or probe
  sets it. A static target's `tls` block replaces the collector's settings
  for the handshake, as it does for TLS from the start.

The request for TLS adds its own timings: `starttls_seconds` in the
[document](#the-exchange-as-a-document) and `X-Tcp-Starttls-Seconds`, from
the end of the greeting until the server said yes, beside `tls_seconds` for
the handshake. The document's `starttls` names the protocol.

To watch the certificate that such a server presents (its expiry and whether
it verifies) instead of speaking to it over TLS, use the
[`tlscert`](TLSCERT.md#starttls) type of the default build. It takes the
same `starttls`, with the same values and dialogue.

SMTP submission, port 587, checked through `QUIT` over TLS:

```yaml
  - name: smtp_submission
    request:
      type: tcp
      starttls: smtp
      send: "QUIT\r\n"
      document: true
    transform:
      type: jq
    metrics:
      - name: smtp_up
        description: 1 when the server began TLS and answered QUIT with 221
        expression: 'if (.answer | startswith("221")) then 1 else 0 end'
      - name: tcp_starttls_seconds
        expression: .starttls_seconds
      - name: tcp_tls_seconds
        expression: .tls_seconds
```

```text
/probe?collector=smtp_submission&target=mx.example.com:587
```

IMAP, port 143, the handshake alone, with the server's CA:

```yaml
  - name: imap_starttls
    request:
      type: tcp
      starttls: imap
      document: true
      tls:
        ca_file: /etc/exporter/mail-ca.pem
    transform:
      type: jq
    metrics:
      - name: imap_starttls_up
        description: 1 when the server began TLS with a certificate that verifies
        expression: '1'
      - name: tcp_tls_seconds
        expression: .tls_seconds
```

POP3, port 110, `QUIT` over TLS:

```yaml
  - name: pop3_starttls
    request:
      type: tcp
      starttls: pop3
      send: "QUIT\r\n"
      document: true
    transform:
      type: jq
    metrics:
      - name: pop3_up
        expression: 'if (.answer | startswith("+OK")) then 1 else 0 end'
      - name: tcp_starttls_seconds
        expression: .starttls_seconds
```

A failed request for TLS reads:

```text
collector smtp_submission tcp failed: the server mx.example.com:587 did not begin TLS: it answered STARTTLS with "454 4.7.0 TLS not available due to local problem\r\n", not 220
```

A debug probe's report names the protocol as `Tcp-Starttls` and shows, after
the greeting, each command sent and the server's answer, quoted, and then
the TLS version begun or, when the handshake fails, its error:

```text
Requests
  1. SEND tls://mx.example.com:587 -> greeted with "220 mx.example.com ESMTP\r\n"; sent "EHLO [10.1.2.3]\r\n", answered "250-mx.example.com\r\n250 STARTTLS\r\n"; sent "STARTTLS\r\n", answered "220 2.0.0 Ready\r\n"; began TLS 1.3; read 15 bytes, the server closed the connection in 52ms
     Tcp-Send: "QUIT\r\n"
     Tcp-Starttls: smtp
```

`start_tls: true`, the StartTLS of [`ldap`](LDAP.md#tls), is not a `tcp`
key. Written in a `tcp` or a `tlscert` collector, it is refused, and the
error names `starttls` and its protocols instead.

## Examples

[`examples/tcp/`](../examples/tcp/config.yaml) holds complete collectors for
memcached, ZooKeeper and HAProxy, with more of each one's statistics than
below, and static targets for them. It also reads memcached's `stats slabs`,
each statistic by slab class, and ZooKeeper's `ruok`, whether the server
answers `imok`.

ZooKeeper's `mntr`: each number that it reports becomes a series of one
family by its name, plus whether the server is the ensemble's leader. The
server answers the four-letter words that its `4lw.commands.whitelist`
names, `mntr` among them:

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

HAProxy's runtime API on a TCP socket (`stats socket ipv4@0.0.0.0:9999 level
user` in its configuration): each proxy's and server's sessions and whether
it is serving, from the CSV of `show stat`. Its header line begins
`# pxname`, which is the first column's name.

Whether a frontend or a server serves is the first word of its status. That
is more than a `value_map`, which matches whole texts, can read: HAProxy 2.8
writes `UP 1/3` for a server going down; `NOLB`, `DRAIN` and `no check` for
servers that serve; `DOWN 1/2`, `MAINT`, `MAINT (via app/app1)` and
`DOWN (agent)` for ones that do not; and `STOP` for a stopped frontend. A
`pre_script` ([Python](PYTHON.md#what-data-is)) adds a column, `up`, for
the rule to read: `0` for a status that is empty or begins `DOWN`, `MAINT`
or `STOP`, and `1` for any other:

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

A [static target](STATIC-TARGETS.md) of a `tcp` collector names the service
as its `target`, which is required: `host:port`, or the host alone for a
collector with [`ports`](#several-ports-of-a-host). Under its `request` it
may set `timeout` and `tls`. What is sent, the terminator, `starttls`,
`connect_only` and `ports` belong to the collector, and `params` fills its
placeholders:

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

A target's `tls` replaces the collector's whole block for that target: a key
that it leaves out is not taken from the collector's. It is checked when the
file loads, as the collector's is.

- Written, even as `tls: {}`, it makes the scrape speak TLS. One collector
  can therefore read services in plaintext and behind TLS, each with its own
  CA or client certificate. A target cannot turn off the TLS that its
  collector asks for.
- Its files are read at each connection and by
  [`--dry-run`](CONFIGURATION.md#dry-run), which warns about them under the
  target's name. They hold no placeholder, and no probe parameter sets them.
- A target's block is part of its
  [cache key](CONFIGURATION.md#response-caching), so targets with different
  blocks never share an answer or a trip to the service.

## Limits

The answer is read against `max_response_bytes`, or
`limits.max_response_bytes`, 10 MiB when neither is set, as it arrives. An
answer that grows past it fails the scrape at that point, without waiting
for the rest, as an exceeded limit, which counts in
`http_exporter_series_limit_exceeded_total`.

The greeting is held to the limit too, on its own. A greeting that grows
past it fails the scrape at that point, and so does one that reaches it
without having ended, at once, even if the server sends nothing more. With
[`document`](#the-exchange-as-a-document) the greeting and the answer share
the limit, and the JSON document they make is held to it as well.

The connection, the TLS handshake, the greeting, the request for TLS with
`starttls`, the write and the read all end at the probe's deadline: its
`timeout`, or the scrape's, 30 seconds without one. A server that stops
answering, or keeps the connection open without the terminator, fails the
scrape at the deadline, with the deadline's error.

With [`connect_only`](#checking-that-a-port-is-open), the connection and the
handshake end at `connect_timeout` instead, within the deadline, and a port
that has not answered by then is `"open": false`, not a failure. Its
document is a few hundred bytes for each port, which `max_response_bytes`
does not limit.

## Errors and self-metrics

A failed scrape is reported in the `tcp` stage and follows
`error_handling.on_fetch_error`. The error says what went wrong, for
example:

- `connecting to memcached-0.cache:11211: ...`;
- `reading from memcached-0.cache:11211: no answer ending with
  request.terminator "END\r\n" came, 12 bytes read: ...`;
- `the server closed the connection after 12 bytes, and the answer does not
  end with request.terminator "END\r\n"`;
- with `greeting`: `the greeting of mx.example.com:25, "554 ...", does not
  match request.greeting "^220[ -]"`, `the server mx.example.com:25 closed
  the connection without a greeting; request.greeting expects one`, and
  `waiting for the greeting of mx.example.com:25, as request.greeting
  expects, 0 bytes read: ...`;
- with `starttls`: the step that failed ([STARTTLS](#starttls)).

A service that the target lists refuse counts in
`http_exporter_targets_refused_total` and is answered `403`.

A [debug probe](CONFIGURATION.md#debug-probes) shows in its request section
the bytes sent, quoted so that a carriage return and a line feed can be
seen, and how many bytes were read and how reading stopped, after the
greeting step with `greeting` ([Timings as headers](#timings-as-headers)).
Its response section shows the answer and its headers. A control character
in `X-Tcp-Greeting`, which holds what the server greeted with, is written
as `\r` or `\x1b`, so that it does not rewrite the report on a terminal:

```text
Requests
  1. SEND tcp://memcached-0.cache:11211 -> read 1342 bytes, the answer ended with the terminator "END\r\n" in 1ms
     Tcp-Send: "stats\r\n"
```

With [verbose self-metrics](SELF-METRICS.md#verbose-per-request-self-metrics),
a scrape is labelled:

- `url="tcp://host:port"`, `tls://` over TLS, `udp://` over UDP, and
  `unix://` with the socket's path over a Unix socket;
- `http_method="SEND"` when the collector writes `send` or `send_hex`,
  `http_method="READ"` when it only reads, and `http_method="CONNECT"` with
  [`connect_only`](#checking-that-a-port-is-open), for which a port that is
  not open is an answer, not a failure.

## Limitations

- **A conversation, not a session.** A scrape reads a greeting when asked,
  begins TLS with `starttls`, and either writes `send` once and reads one
  answer or speaks up to 16 [`steps`](#a-conversation), each answered before
  the next. A login whose answer decides what is sent next is not spoken.
  `starttls` speaks only the requests for TLS of SMTP, IMAP, POP3 and FTP,
  not those of LMTP, XMPP, LDAP (the [`ldap`](LDAP.md#tls) type's
  `start_tls`), PostgreSQL or MySQL. Where the server takes several commands
  together, write them all into `send`, as with Beanstalkd's
  `stats\r\nquit\r\n`.
- **Text matched as written.** The terminator is matched as bytes at the end
  of the answer, with no placeholder and no pattern.
- **The greeting is the first line.** An SSH server may send lines of its
  own before its `SSH-2.0-` line (RFC 4253, section 4.2). The greeting read
  is then the first of those lines, which `^SSH-2\.0-` does not match. Such
  a server needs a pattern for what it writes first.
- **One connection per scrape.** No connection is kept between scrapes, so
  each scrape costs a TCP handshake, and a TLS one with TLS. See [One
  connection per scrape](REQUESTS.md#one-connection-per-scrape) for why.
- **One datagram each way over UDP.** A UDP exchange sends one datagram and
  takes one back. An answer split over several datagrams is read as its
  first datagram, and a datagram lost on the way fails the scrape at its
  deadline, with no second try.
- **Stream sockets only.** A Unix datagram socket, as some services' control
  sockets are, cannot be reached.

## Troubleshooting

| The scrape says | Do |
| --- | --- |
| `target "..." is not host:port; a tcp target names its port` | Add the port: `memcached-0.cache:11211`. |
| `the greeting of ..., [withheld; in the exporter's log], does not match request.greeting ...` | The server greeted otherwise: the exporter's log, or a [debug probe](CONFIGURATION.md#debug-probes), shows what it said. Over several lines the pattern is matched against them all: `^220[ -]` takes `220-` on the first. |
| `waiting for the greeting of ..., as request.greeting expects, 0 bytes read` | The server says nothing first: it is not a service that greets, or it greets only over TLS, which `tls` asks for. |
| `the server ... does not offer STARTTLS in its answer to EHLO, ...` | The server has no TLS on that port: use the port where it speaks TLS from the start, 465, with `tls` and no `starttls`, or configure TLS on the server. |
| `the server ... did not begin TLS: it answered ... with [withheld; in the exporter's log], not ...` | The server refused: the log, or a debug probe, shows its words. |
| `the greeting of ..., [withheld; in the exporter's log], does not begin with 220, which request.starttls smtp needs ...` | The server will not serve the client, or speaks another protocol than `starttls` names. |
| `... no answer ending with request.terminator "END\r\n" came, ...` | The terminator: written in double quotes, so that `\r\n` is a carriage return and a line feed, and the end of the whole answer. Or `send`: the server did not take it, and waits for more, often a line feed it lacks. |
| `... the server kept the connection open, ...; without request.terminator ...` | The server does not close after answering: set `terminator`. |
| `the server closed the connection after 0 bytes, ...` | The server refused what was sent: a ZooKeeper whose `4lw.commands.whitelist` does not name the word answers nothing and closes. |
| `... the answer of ... is larger than the response limit ...` | Raise `max_response_bytes`, or send a command that answers less. |
| `request.send is empty once param_<name> is filled in, ...` | The probe gave no value for a placeholder whose default is empty, and nothing else is in `send`: give the parameter, or a default. |
| `the JSON document request.document makes of what ... said is ... bytes, larger than the response limit ...` | Raise `max_response_bytes`: the document counts the greeting and the answer together, a control character six times. |
| `request.send: the value of param_<name> holds a carriage return or a line feed ...` | The probe's parameter: a value may not end the line. |
| `TLS handshake with ...: ... certificate signed by unknown authority` | Name the server's CA in `tls.ca_file`. |
