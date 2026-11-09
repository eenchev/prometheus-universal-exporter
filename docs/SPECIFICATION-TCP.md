# Prometheus Universal Exporter — `tcp` Request Type Specification

This is the `tcp` request type's section of § 5.1 of the
[exporter specification](SPECIFICATION-EXPORTER.md), in a document of its
own so that a change to the type is a change to this document. Everything
the request types share — the registry and the rules every type is held to
(§ 5.1), building without a type, decoding, transforms, error policies,
limits, caching, static targets and exposition — is in the exporter
specification, and a section number here, such as § 42.14, is one of its
sections. What a user reads of the type is [TCP.md](TCP.md).

## `tcp`

A `tcp` collector reads a service that answers a line of text sent over a
TCP connection, such as memcached's `stats`, a ZooKeeper four-letter word or
HAProxy's runtime API: it writes the line and hands the answer, as the
server wrote it, to the decoder and the transform as an `http` collector's
body is handed to them. Its keys:

| Key | Default | Rule |
| --- | --- | --- |
| `greeting` | none | A regular expression (Go's `regexp`, RE2) the server's greeting must match before `send` is written. At most 64 KiB; no placeholder. |
| `send` | none | The text written once the connection is made, or once the greeting has come; nothing is written without it. At most 64 KiB. `{{param_name}}` placeholders, without filters. |
| `terminator` | none | What the answer ends with. At most 64 KiB; no placeholder. |
| `document` | `false` | `true`: the answer is a JSON document of the exchange in place of the server's text. |
| `starttls` | none | `smtp`, `imap`, `pop3` or `ftp`: TLS is begun on the plaintext connection, after the greeting, by that protocol's request for it. |
| `tls` | none | As `http`'s; written, even empty (`tls: {}`), the scrape speaks TLS, from the start, or, with `starttls`, once begun. |
| `send_hex` | none | `send` as pairs of hexadecimal digits, blanks between them or none; no placeholder. At most 64 KiB of bytes. |
| `answer_bytes` | none | How many bytes the answer is; reading stops once that many came. |
| `udp` | `false` | `true`: one UDP datagram is sent and the first back is the answer. |
| `steps` | none | A conversation, in place of `send`, `send_hex`, `terminator` and `answer_bytes`, each refused beside it, as is `udp`: up to 16 entries of `send`, required, not `""`, held as `send` is, and `until`, a regular expression held as `greeting` is, required of each but the last. |
| `unix_socket` | none | The absolute path of a Unix stream socket the exchange is made over; of the common part, as `http`'s. |
| `connect_only` | `false` | `true`: the scrape connects, makes the TLS handshake with `tls`, and closes, writing and reading nothing; the answer is a JSON document of whether the port was open. |
| `connect_timeout` | `5s` | With `connect_only`, how long the connection and its handshake may take; at least `10ms`. |
| `ports` | none | With `connect_only`, up to 64 distinct ports from 1 to 65535 of the target's host, dialed at once. |
| `max_response_bytes`, `allowed_targets`, `denied_targets` | as `http` | |

- `send_hex` MUST be refused at load beside `send`, with a placeholder, with
  a character that is no hexadecimal digit or blank (naming it), with an odd
  number of digits, with no digit, and past 64 KiB of bytes; a scrape MUST
  write its bytes as `send` is written, and its `http_method` MUST be
  `SEND`. `answer_bytes` MUST be refused below 0 and beside `terminator`; a
  scrape of it MUST read exactly that many bytes, the answer ending there
  though the server keeps the connection open, MUST fail, saying how many
  bytes came, when the server closes the connection sooner, and MUST fail as
  a limit exceeded before reading when the length is past the response
  limit.
- `udp: true` MUST be refused at load without `send` or `send_hex`, and
  beside `greeting`, `terminator`, `answer_bytes`, `tls`, `starttls` and
  `unix_socket`. A scrape MUST send one datagram on a socket connected to
  the target's address, through the target policy as a TCP connection is,
  and MUST take the first datagram read as the answer, a datagram past the
  response limit failing the scrape as a limit exceeded; a port answered
  with ICMP port unreachable MUST fail the scrape when the refusal comes,
  not at the deadline. Its `url` label MUST be `udp://host:port`.
- `unix_socket` MUST be checked as `http`'s is (§ 42.15b), MUST be settable by no probe parameter, static target or
  `defaults` block, and its scrape MUST connect to the socket rather than
  the target, the target being a host with a port or without, held by the
  target lists to its name as written, never looked up. The `url` label
  MUST be `unix://` and the socket's path, `tls+unix://` with TLS, and a
  probe's caller MUST be told the socket's path withheld when the connection
  fails.
- `greeting`, `send` and `terminator` written `""` MUST be the keys left
  out. Any of them longer than 64 KiB, a placeholder in `terminator` or
  `greeting`, a `greeting` Go's `regexp` does not compile, `terminator` with
  `greeting` and without `send` or `send_hex`, `answer_bytes` likewise, and the
  two with `starttls` and neither of those, and a placeholder of `send` that does not
  parse or names a filter MUST be refused at load, as MUST a `tls` block
  with one of `cert_file` and `key_file` alone. No probe parameter MAY set
  `greeting`.
- With `greeting`, a scrape MUST read the greeting after the connection and
  the TLS handshake and before it writes anything: the server's first line,
  through its line feed, or, when that line begins with three ASCII digits
  and `-`, every line up to and including the first that begins with the
  same three digits and is not followed by `-`. The greeting MUST be held
  to the response limit as it arrives, a greeting that has reached the
  limit without ending failing the scrape as a limit exceeded at once,
  without waiting for another byte or the deadline, and its read MUST end at the
  scrape's deadline. The pattern MUST be compiled when the collector loads
  and not again at each scrape. The pattern MUST be matched against the whole greeting
  as read, line ends included; a greeting that does not match MUST fail
  the scrape without writing `send`, the error naming the target, quoting
  the greeting with `model.Quoted`, so that a probe's caller is told it
  withheld (§ 19.4), and the pattern. A server that closes the connection
  before its greeting ended MUST fail the scrape, saying it sent no
  greeting or quoting the part it sent alike, and one that has not greeted
  by the deadline MUST fail it with the deadline's error. Bytes the server
  wrote after the greeting MUST be the start of the answer. Without `send`
  the greeting MUST be the answer, nothing being read after it.
- Every answer MUST carry the scrape's timings as headers, in seconds,
  written as decimal numbers: `X-Tcp-Connect-Seconds` always,
  `X-Tcp-Tls-Seconds` with TLS, `X-Tcp-Greeting-Seconds` and
  `X-Tcp-Greeting`, a value for each line of the greeting without its line
  end, with `greeting` or `starttls`, `X-Tcp-Starttls-Seconds` with
  `starttls`, and `X-Tcp-Answer-Seconds` when an answer was read
  after the greeting or without one; without `document` the body MUST stay
  the answer alone.
- With `document: true` the body MUST be a JSON object, with the
  `Content-Type` `application/json`: `greeting`, the greeting as read, with
  `greeting`; `answer`, what was read after it or the whole answer without
  it, `""` when nothing was, whenever an answer was read; `starttls`, the
  protocol, with `starttls`; and, as numbers of
  seconds, `connect_seconds` always, `tls_seconds` with TLS,
  `greeting_seconds` with `greeting` or `starttls`, `starttls_seconds`, from
  the end of the greeting until the server's yes to the request for TLS,
  with `starttls`, `answer_seconds` when an answer was
  read, and `duration_seconds`, the exchange from the dial to the end of the
  read. A key of a step not taken MUST be left out, not written `null` or
  `0`. A greeting or an answer that is not UTF-8 MUST fail the scrape,
  naming which and the target. The answer MUST be held, as it arrives, to
  what the greeting left of the response limit, and the JSON document to
  the limit; either past it MUST fail the scrape as a limit exceeded. The decoder MUST be `json`, left out or
  written, with no warning of a decoder left to each response; any other
  decoder, and a transform other than `jq`, `yq` and `python`, MUST be
  refused at load, naming the document.
- `starttls` MUST be one of `smtp`, `imap`, `pop3` and `ftp`, exactly, and
  `terminator` with `starttls` and without `send` MUST be refused at load.
  With `starttls` a scrape MUST read the greeting, as with `greeting` and
  whether `greeting` is set or not, on the plaintext connection, and MUST
  fail without writing anything when it does not begin with `220` (`smtp`,
  `ftp`), `* OK` in any case (`imap`) or `+OK` (`pop3`), matching
  `greeting` against it too when that is set. For `ftp` the `120` replies a
  server may say before its `220` (RFC 959), of one line or several, MUST
  be read past, held to the limit with the greeting and to the deadline:
  the greeting checked, matched against `greeting` and held by the answer
  is the reply after them, and the debug report MUST show them before it.
  It MUST then write only the
  protocol's request for TLS: for `smtp`, `EHLO` naming the client by the
  connection's local address as an address literal (`[192.0.2.7]`,
  `[IPv6:2001:db8::7]`), its answer read as a greeting is and required to
  be `250` and to list `STARTTLS` on a line after its first, in any case,
  then `STARTTLS`, answered `220`; for `imap`, `a1 STARTTLS`, its answer
  the lines up to the first beginning `a1 ` in any case, which MUST be `a1
  OK`, the tag in any case: a server that echoes the tag as `A1` MUST NOT
  be left waiting on until the deadline; for
  `pop3`, `STLS`, answered `+OK`; for `ftp`, `AUTH TLS`, answered `234`.
  Each answer MUST be held to the response limit, the deadline and
  cancellation as the greeting is. A server that does not list `STARTTLS`,
  answers otherwise (a refusal such as `454`, `NO`, `BAD`, `-ERR` or `534`,
  or what is no answer of the protocol), closes the connection or writes
  any byte after its yes before the handshake MUST fail the scrape in the
  `tcp` stage, naming the command and, where the server answered, quoting
  the answer with `model.Quoted`; the scrape MUST NOT write `send` or any
  other byte in plaintext, nor go on without TLS. The handshake MUST then
  be made with the `tls` block's settings, or, without one, verifying with
  the system roots for the target's host, and `send` written and the answer
  read over TLS; without `send` the scrape MUST end after the handshake,
  the greeting being the answer. No static target or probe MAY set
  `starttls`. `starttls` is a key of the common part, which `tlscert` takes
  too with the same values and dialogue ([`tlscert`](SPECIFICATION-TLSCERT.md)).
- A placeholder of `send` MUST be filled from the probe's parameter, or its
  default, written as it is. A value MUST hold only ASCII letters, digits
  and `. _ - : / @ + = , %`; a value holding any other character, a blank,
  `;`, `|`, `&`, a quote, `#`, a control character or a character outside
  ASCII, MUST be refused with `400` before the connection is made, the
  refusal naming the parameter and the character, so that a probe cannot
  end the line, add a `;`-separated command (HAProxy's runtime API runs
  every one on a line) or add an argument. A default holding such a
  character MUST be refused at load. A probe
  parameter that no placeholder of `send` or of a label value names MUST be
  refused, the refusal naming `request.send`. A `send` that its
  placeholders' values and defaults leave empty MUST be refused with `400`
  before the connection is made, naming its placeholders, and a static
  target leaving it empty at load; the scrape MUST NOT write nothing in its
  place.
- A scrape MUST make one connection, speak TLS over it when a `tls` block is
  written, an empty one (`tls: {}`) as much as one with keys, from the start
  without `starttls`, verifying the
  certificate with the system roots and `ca_file`'s,
  write `send` when it is set, nothing added to it, read the answer and
  close the connection; it MUST NOT close its writing half before the
  answer is read. With `terminator`, reading MUST stop once the bytes read
  end with it, the terminator staying in the body, and a server that closes
  the connection before then MUST fail the scrape, saying how many bytes
  came. Without `terminator`, reading MUST stop when the server closes the
  connection.
- The answer MUST be held to the response limit as it arrives: once its
  bytes are past the limit the scrape MUST fail as a limit exceeded, without
  reading on; an answer of exactly the limit MUST be taken.
- The connection, the TLS handshake, the greeting, the request for TLS, the
  write and the read MUST end at the scrape's deadline, 30 seconds without one, or when it is cancelled, and a
  scrape ended so MUST fail with the deadline's error, saying, when reading
  was what the deadline ended, the terminator waited for, or, without one,
  that the server kept the connection open.
- The answer MUST be the body, with no status, as a local file's has none,
  and, without `document`, no `Content-Type`; the decoder MUST be `decoder.type`, or the one the
  transform implies, or what the body reads as. A failed scrape MUST be
  reported in the `tcp` stage.
- The target MUST be `host:port` or `[v6]:port`, the port required, with no
  scheme, user, path or query, which MUST be answered `400` before anything
  is sent, as a static target's is refused at load. It is required. The
  target lists MUST be applied to the host before the connection and to the
  address connected to, as `http`'s are, a refused one answered `403`,
  counted in `http_exporter_targets_refused_total`, and never connected to.
- A debug probe's request section MUST show the scrape by its method and
  `tcp://host:port` (`tls://` over TLS, from the start or begun with
  `starttls`), the bytes sent quoted, with a
  carriage return and a line feed written `\r` and `\n`, in a `Tcp-Send`
  header, with `greeting` the pattern quoted in a `Tcp-Greeting` header,
  with `starttls` the protocol in a `Tcp-Starttls` header,
  and as its outcome the greeting read, quoted, when there is one, with
  `starttls` each command of the request for TLS and the server's answer,
  quoted, and the TLS version begun, the
  bytes read and how reading stopped, or the error, which, when the
  handshake after the request for TLS fails, MUST follow the greeting and
  the commands and answers as a success's outcome does. A report MUST write a
  control character of a header's value, other than a tab, escaped as Go
  writes it in a quoted string (`\r`, `\x1b`), and every other character
  of a value as it is. The verbose `url` label MUST be `tcp://host:port` or
  `tls://host:port`, and `http_method` MUST be `SEND` for a collector with
  `send` and `READ` without.
- `--dry-run` MUST read the `tls` block's files when the block is set and
  list what a scrape would fail on as warnings of the `config` check.
- A static target MAY set `timeout` and its own `tls` block (§ 42.14); a
  probe MAY set `timeout` and `param_<name>`.
- With `steps`, after the greeting and the request for TLS when set, each
  step's `send`, its placeholders filled as `send`'s, MUST be written in
  order, and its answer read until what came of it matches its `until`, or,
  the last without `until`, until the server closes the connection: one
  that closes it before the answer matches, or the deadline first, MUST
  fail the scrape, quoting what came and naming the step. The steps'
  answers together, with the greeting a document holds, MUST be held to the
  response limit. The answer MUST be their answers one after another, and
  the document of `document` MUST hold `steps`, each step's `send`,
  `answer` and `seconds`. The method MUST be `SEND`; a debug report MUST
  show each step's send, quoted, in `Tcp-Steps`, and the outcome the steps
  taken and the bytes read.
- `connect_only: true` MUST be refused at load beside `send`, `send_hex`,
  `steps`, `greeting`, `terminator`, `answer_bytes`, `starttls`, `udp` and
  `unix_socket`, each named; `connect_timeout` and `ports` MUST be refused
  without it, `connect_timeout` under `10ms`, and `ports` of more than 64,
  of a port outside 1 to 65535, naming its index, and of a port twice. Its
  decoder MUST be `json` and its transform `jq`, `yq` or `python`, as with
  `document`, which MAY be written beside it.
- A scrape of `connect_only` MUST dial the target, holding the connection
  to the target lists as every connection is, make the TLS handshake when a
  `tls` block is set, and close the connection, writing nothing to it and
  reading nothing of it but the handshake's. Its body MUST be a JSON object
  with `open`, true when the connection was made and, with `tls`, the
  handshake too; `reason` and `error` when it is false, `reason` one of
  `refused`, `timeout`, `unreachable`, `reset`, `tls` and `error`; when the
  connection was made, `address`, the remote address it reached, and
  `connect_seconds`, even when the handshake failed; with `tls` and a
  handshake made, `tls_seconds` and `tls_version` as Go names it (`TLS
  1.3`); and `duration_seconds`; a key that does not apply MUST be left
  out. Its `Content-Type` MUST be `application/json`.
- A port refused, not answering by the connection's deadline, unreachable,
  reset, or whose handshake fails, MUST be answered `"open": false`, not
  fail the scrape. A target the target lists refuse MUST fail the scrape,
  never connected to, and so MUST a name that does not resolve, other than
  by a lookup's timeout, and a scrape cut short before the connection's
  deadline.
- The connection and its handshake MUST end at `connect_timeout` from when
  the scrape begins to connect, `5s` when it is left out, or, when sooner, at nine tenths
  of the time the scrape has left to its deadline, so that the document is
  answered within it.
- With `ports`, the target MUST be a host alone, a name, an IPv4 address,
  or an IPv6 address in brackets or not; one with a port MUST be refused
  with `400`. Each port MUST be dialed at once, as a scrape of one port is,
  and the body MUST hold `ports`, an object of each port's document keyed by
  its number in decimal, `open`, whether every port was open, and
  `open_ports`, how many were, with `duration_seconds`. A failure of any
  port that fails a scrape MUST fail it.
- The `http_method` of `connect_only` MUST be `CONNECT`. A debug report
  MUST show the scrape, and with `ports` each port as a request of its own
  whose `url` is `tcp://host:port` or `tls://host:port`, with its
  connection's phases, a `Tcp-Connect-Timeout` header of the time the
  connection was given, and as its outcome `open, connected to <address>`,
  with `, began <version>` after a handshake, or `not open, <reason>:
  <error>`; a span MUST tell `tcp.open` and, when not open,
  `tcp.closed.reason`. The scrape's `url` label with `ports` MUST be
  `tcp://host` or `tls://host`, an IPv6 address in brackets.
