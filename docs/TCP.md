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
  first](#services-that-speak-first));
- whether a mail or FTP server that begins TLS on request, with `STARTTLS`,
  does, with a certificate that verifies ([STARTTLS](#starttls)).

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
| `starttls` | none | `smtp`, `imap`, `pop3` or `ftp`: begin TLS on the plaintext connection, after the greeting, as that protocol asks for it, and write `send` over TLS. See [STARTTLS](#starttls). |
| `send` | nothing | The text written once the connection is made, such as `"stats\r\n"`. Write it in double quotes, in which YAML reads `\r\n` as a carriage return and a line feed; in single quotes or none, `\r\n` is four characters. May hold `{{param_name}}` placeholders. At most 64 KiB. Left out, nothing is written, and the answer is what the server says first. See [What a scrape sends](#what-a-scrape-sends). |
| `terminator` | none | What the answer ends with, such as `"END\r\n"`: reading stops once it has come. Left out, reading stops when the server closes the connection. At most 64 KiB, and no placeholder. See [Reading the answer](#reading-the-answer). |
| `tls` | none | Speak TLS, verifying the server: see [TLS](#tls). With `starttls`, the settings of the TLS it begins. |
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
plaintext, so the label of such a scrape is `tls://host:port`. With
[`starttls`](#starttls) the block holds the settings of the TLS begun after
the greeting rather than from the start.

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
collector sets [`greeting`](#services-that-speak-first) or
[`starttls`](#starttls), begins TLS with `starttls`, writes `send` when
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

A `send` that its placeholders leave empty, `"{{param_cmd:}}"` with no
`param_cmd` on the probe, is refused too, answered `400` naming the
parameters before the connection is made: `request.send is empty once
param_cmd is filled in, ...`. Writing nothing would turn the scrape into a
read of what the server says first, which a server waiting for a command
ends only at the deadline, while the collector says it sends. A static
target that leaves it empty is refused at load. Give the placeholder a
default that is not empty, or write the command's fixed part around it.

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
greeting and, without `(?m)`, `$` its end, after the last line feed. With
`(?m)`, `$` matches before a line feed, and so after the `\r` of a line
ending in `\r\n`, as SMTP, FTP, POP3, IMAP and SSH lines do:
`(?m)^220 ready$` does not match `220 ready\r\n`. Write `\r?$` to end a
line either way: `(?m)^220 ready\r?$`.
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
IMAPS on 993 and POP3S on 995, which speak TLS from the start. A server
that starts in plaintext and turns to TLS when asked is read with
[`starttls`](#starttls), the greeting then coming before the handshake.

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
- `starttls` is the protocol whose request began TLS, with `starttls`.
- `connect_seconds` is how long making the connection took, the name's
  lookup included; `tls_seconds` the TLS handshake, with `tls` or
  `starttls`; `greeting_seconds` from the connection, or the handshake,
  until the greeting ended, with `greeting` or `starttls`;
  `starttls_seconds` from the greeting's end until the server said to begin
  TLS, with `starttls`; `answer_seconds` from the write of
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
- The document is held to the [response limit](#limits), as the body it
  is: the greeting and the answer share the limit as they are read, the
  answer getting what the greeting left, and the document they make, in
  which JSON writes a control character in six bytes (`\u0001`) and adds
  its keys and timings, must be within it too. Either past it fails the
  scrape as a limit exceeded: `the greeting and the answer of host:port are
  larger together than the response limit ...`, or `the JSON document
  request.document makes of what host:port said is 2412 bytes, larger than
  the response limit ...`.

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
| `X-Tcp-Tls-Seconds` | with `tls` or `starttls` | `tls_seconds`. |
| `X-Tcp-Greeting-Seconds` | with `greeting` or `starttls` | `greeting_seconds`. |
| `X-Tcp-Greeting` | with `greeting` or `starttls` | The greeting, a value for each of its lines, without its line end. |
| `X-Tcp-Starttls-Seconds` | with `starttls` | `starttls_seconds`. |
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

## STARTTLS

SMTP on 25 and 587, IMAP on 143, POP3 on 110 and FTP on 21 start in
plaintext and begin TLS when the client asks. `starttls` names the
protocol whose request the scrape speaks, and the scrape reads the greeting,
asks for TLS, makes the handshake and only then writes `send`, over TLS:

| `starttls` | The greeting must begin | Sent before TLS | The server's yes |
| --- | --- | --- | --- |
| `smtp` | `220` | `EHLO [address]`, then `STARTTLS` once the answer, `250`, lists it | `220` |
| `imap` | `* OK` | `a1 STARTTLS` | `a1 OK`, the tag in any case, after any untagged lines |
| `pop3` | `+OK` | `STLS` | `+OK` |
| `ftp` | `220` | `AUTH TLS` | `234` |

- The greeting is read as with [`greeting`](#services-that-speak-first),
  over several lines for SMTP and FTP, whether `greeting` is set or not,
  and must begin as the table says: anything else, an SMTP server's `554`,
  or an IMAP server's `* PREAUTH`, which has no `STARTTLS` to offer, fails
  the scrape before anything is sent. `greeting`, when set, is matched
  against it too. An FTP server may first say when it will be ready,
  `120`, in one reply or several, as RFC 959 allows: with `starttls: ftp`
  those are read past, held to the limit and the deadline with the
  greeting, and the greeting — what must begin `220`, what `greeting` is
  matched against, and what the answer and its `greeting` hold — is the
  `220` after them. A debug probe's report shows them. Without `starttls`
  nothing is read past: the first reply is the greeting.
- An SMTP client names itself in `EHLO`; the exporter gives its own address
  on the connection, `[10.1.2.3]` or `[IPv6:2001:db8::7]`, as RFC 5321 has
  a client without a name the server can look up do, which a pod's or a
  machine's host name is not. The answer must be `250` and list `STARTTLS`
  on one of the lines after the first, which names the server, in any
  case. IMAP's capabilities are not asked for:
  a server without `STARTTLS` answers `NO` or `BAD`, which fails the scrape
  as plainly, with one command less in plaintext. IMAP tags are
  case-sensitive, but a server that answers `A1 OK` for `a1` has answered
  the command: the tag is matched in any case, rather than the answer being
  taken for an untagged line and the scrape waiting for another until its
  deadline.
- The handshake takes the `tls` block's settings, `ca_file`, a client
  certificate, `server_name`, `insecure_skip_verify`, as TLS from the start
  does ([TLS](#tls)); without a block it verifies the server against the
  system's roots for the target's host, never skipping TLS. The label is
  `tls://host:port`.
- Nothing but the commands of the table is written before the handshake:
  `send`, which a probe's values may fill, goes over TLS alone. Without
  `send` the scrape ends with the handshake, which checks that the server
  begins TLS with a certificate that verifies; the greeting is the answer,
  and `terminator` is refused at load then.
- A server that does not list `STARTTLS`, refuses it — SMTP's `454`, IMAP's
  `NO`, POP3's `-ERR`, FTP's `534` — or answers anything else fails the
  scrape in the `tcp` stage, naming the step and quoting the server's
  answer, which a probe's caller is told
  [withheld](CONFIGURATION.md#what-a-failed-probes-caller-is-told), and the
  log and a debug probe's report show. The scrape never goes on in
  plaintext, so a network that strips `STARTTLS` from the server's answers
  makes a failed scrape, not a plaintext one.
- A server that writes more after its yes, before the handshake, fails the
  scrape: those bytes came in plaintext, where anything on the way could
  have written them, and would otherwise be read as the start of what the
  server says over TLS.
- Each answer is held to the [response limit](#limits) and the scrape's
  deadline, as the greeting is.
- `starttls` is the collector's alone: no static target or probe sets it. A
  static target's `tls` block replaces the collector's settings for the
  handshake, as it does for TLS from the start.

The timings gain the request's: `starttls_seconds` in the
[document](#the-exchange-as-a-document) and `X-Tcp-Starttls-Seconds`, from
the end of the greeting until the server said yes, beside `tls_seconds`,
the handshake's; the document's `starttls` names the protocol.

To watch the certificate such a server presents, its expiry and whether
it verifies, rather than to speak to it over TLS, the
[`tlscert`](TLSCERT.md#starttls) type of the default build takes the same
`starttls`, with the same values and dialogue.

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

and a debug probe's report names the protocol as `Tcp-Starttls` and shows
each command sent and the server's answer, quoted, after the greeting, and
the TLS version begun, or, when the handshake after them fails, its error:

```text
Requests
  1. SEND tls://mx.example.com:587 -> greeted with "220 mx.example.com ESMTP\r\n"; sent "EHLO [10.1.2.3]\r\n", answered "250-mx.example.com\r\n250 STARTTLS\r\n"; sent "STARTTLS\r\n", answered "220 2.0.0 Ready\r\n"; began TLS 1.3; read 15 bytes, the server closed the connection in 52ms
     Tcp-Send: "QUIT\r\n"
     Tcp-Starttls: smtp
```

`start_tls: true`, [`ldap`](LDAP.md#tls)'s StartTLS, is not a `tcp` key:
written in a `tcp` or a `tlscert` collector it is refused with
`starttls` and its protocols named instead.

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
`request` `timeout` and `tls`. What is sent, the terminator and `starttls`
are the collector's, and `params` fills its placeholders:

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
past it fails the scrape when it does, and so does one that reaches it
without having ended, at once, though the server sends nothing more. With
[`document`](#the-exchange-as-a-document) the greeting and the answer share
the limit, and the JSON document they make is held to it as well.

The connection, the TLS handshake, the greeting, the request for TLS with
`starttls`, the write and the read end at the
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
read: ...`, and, with `starttls`, the step that failed
([STARTTLS](#starttls)). A service
the target lists refuse counts in `http_exporter_targets_refused_total`,
answered `403`.

A [debug probe](CONFIGURATION.md#debug-probes) shows in its request section
the bytes sent, quoted so that a carriage return and a line feed are seen,
and how many bytes were read and how reading stopped, after the greeting
step with `greeting` ([Timings as headers](#timings-as-headers));
its response section shows the answer and its headers, a control character
in `X-Tcp-Greeting`, which holds what the server greeted with, written as
`\r` or `\x1b`, so that it does not rewrite the report on a terminal:

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

- **One line, one answer.** A scrape reads a greeting when asked, begins
  TLS with `starttls`, writes `send` once and reads one answer; a dialogue,
  a login, or an answer read after a second command is not spoken.
  `starttls` speaks SMTP's, IMAP's, POP3's and FTP's request for TLS alone:
  LMTP's, XMPP's, LDAP's (the [`ldap`](LDAP.md#tls) type's `start_tls`),
  PostgreSQL's and MySQL's are not. Write every command into `send` where the server takes them
  together, as Beanstalkd's `stats\r\nquit\r\n`.
- **Text matched as written.** The terminator is matched as bytes at the end
  of the answer, with no placeholder, and no pattern.
- **The greeting is the first line.** An SSH server may send lines of its
  own before its `SSH-2.0-` line (RFC 4253, section 4.2); the greeting read
  is the first of them, which `^SSH-2\.0-` does not match. Such a server
  needs a pattern for what it writes first.
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
