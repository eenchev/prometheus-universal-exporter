# Prometheus Universal Exporter — `redis` Request Type Specification

This is the `redis` request type's section of § 5.1 of the
[exporter specification](SPECIFICATION-EXPORTER.md), in a document of its
own so that a change to the type is a change to this document. Everything
the request types share — the registry and the rules every type is held to
(§ 5.1), building without a type, decoding, transforms, error policies,
limits, caching, static targets and exposition — is in the exporter
specification, and a section number here, such as § 42.14, is one of its
sections. What a user reads of the type is [REDIS.md](REDIS.md).

## `redis`

A `redis` collector reads a Redis or Valkey server, which speak the same
protocol: its INFO, and the replies of read-only commands its configuration
lists, handed to the transforms as one JSON document. Its keys:

| Key | Default | Rule |
| --- | --- | --- |
| `username` | none | An ACL user, logged in as with `AUTH username password`. No blank, control character or placeholder; only with `password_file`. |
| `password_file` | none | The file of the password `AUTH` sends. Not blanks alone. Without it no `AUTH` is sent. |
| `db` | `0` | The logical database, selected with `SELECT` after the login; at least 0. `0`, the database a connection starts in, MUST send no `SELECT`. |
| `info_sections` | INFO's default sections | Sections of INFO, each lower-case letters and `_`, each named once. |
| `commands` | none | Entries of `name` and `command`, the reply of each put under its name. |
| `sentinel` | none | `master`, required, not blanks, with no blank at an end, control character or placeholder; `username`, only with `password_file`, and `password_file`, not blanks alone: the Sentinel's own account. |
| `tls` | none | As `http`'s; written, even empty (`tls: {}`), the scrape speaks TLS. |
| `max_response_bytes`, `allowed_targets`, `denied_targets` | as `http` | |

- The client MUST speak RESP2 and MUST NOT send `HELLO`; it MUST link no
  Redis client library.
- `password_file` MUST be read at every scrape, not at load, its
  surrounding whitespace taken off; a file that is missing, unreadable or
  empty MUST fail the scrape naming the key and the file, never its
  content. `--dry-run` MUST read it, and the `tls` block's files when the
  block is set, and list what a scrape would fail on as warnings of the
  `config` check.
- A scrape MUST send, in order, `AUTH` when `password_file` is set,
  `SELECT` when `db` is not `0`, then `INFO <section>` for each entry of
  `info_sections`, one command each, or `INFO` alone when `info_sections`
  and `commands` are both unset, and then each entry of `commands`. A
  collector with `commands` and no `info_sections` MUST send no `INFO`.
  Each command MUST be answered before the next is sent. An error reply to
  any of them MUST fail the scrape with the command and the server's text,
  `AUTH`'s password written `***`.
- A `commands` entry's `name` MUST be letters, digits and `_`, not starting
  with a digit, and unique among the collector's entries. Its `command`
  MUST name one of these, by name or by name and subcommand, in any case,
  with the arguments after it each allows, and MUST be refused at load
  otherwise, the refusal listing them: `DBSIZE`, `LASTSAVE`, `ROLE`,
  `SLOWLOG LEN`, `CLUSTER INFO` (none); `GET`, `STRLEN`, `TYPE`, `TTL`,
  `PTTL`, `LLEN`, `SCARD`, `ZCARD`, `HLEN`, `HGETALL`, `XLEN`,
  `XINFO STREAM`, `XINFO GROUPS`, `MEMORY USAGE`, `OBJECT ENCODING`,
  `OBJECT FREQ`, `OBJECT IDLETIME`, `OBJECT REFCOUNT` (a key); `LINDEX`,
  `SISMEMBER`, `ZSCORE`, `HGET`, `HEXISTS`, `XPENDING`, `XINFO CONSUMERS`
  (two); `ZCOUNT`, `ZLEXCOUNT` (three); `EXISTS`, `PFCOUNT` (one or more);
  `HMGET` (two or more). No command that writes, lists keys, dumps a
  collection without a bound, blocks, or shows the server's settings or
  other clients (`CONFIG GET`, `CLIENT LIST`) MAY be among them. An
  argument after the command's name and subcommand MAY hold
  `{{param_name}}` placeholders, filled from the probe and sent as one
  bulk string whatever it holds; a placeholder in the name or the
  subcommand MUST be refused at load.
- The answer MUST be a JSON document of `info`, present when INFO was
  sent, an object of each section by its heading in lower case with `_`
  for a blank, each an object of its `name:value` lines; and `commands`,
  present when commands were sent, each reply under its entry's name. A
  value of INFO that is a JSON number MUST be one, written as the server
  wrote it, whatever its size; one made of `name=value` pairs joined by
  commas, such as a keyspace line, MUST be an object of them, each value
  read the same way; any other MUST be its text. A command's reply MUST
  be: an integer a number, a simple or bulk string its text, a nil `null`,
  an array a list; `HGETALL`'s and `XINFO STREAM`'s an object of their
  names and values, `XINFO GROUPS`'s and `XINFO CONSUMERS`'s a list of such
  objects, and `CLUSTER INFO`'s an object of its lines read as INFO's are.
  A section named in `info_sections` that the server answers with no
  section MUST fail the scrape naming it.
- The replies of a scrape MUST be read against the response limit
  together, counted in their bytes as they come off the connection. A bulk
  string's declared length past what the limit has left MUST fail the
  scrape before a byte of it is read, and no more MUST be held for one than
  the server has sent of it; an array's declared length MUST fail it when
  its elements, at three bytes each, could not fit; arrays MUST NOT nest
  deeper than 16, nor a line other than a bulk string's data be longer than
  64 KiB. Each failure past the limit MUST be a limit exceeded; a reply
  that is no RESP2 reply MUST fail the scrape saying so.
- With `sentinel`, the target MUST be read as a Sentinel: on a connection
  of its own, logged in with `sentinel.username` and
  `sentinel.password_file` (read at every scrape, as `password_file` is,
  and by `--dry-run`), never the collector's, the scrape MUST send
  `SENTINEL GET-MASTER-ADDR-BY-NAME <master>` then
  `SENTINEL MASTER <master>`, close that connection, and read the server
  at the address answered as it reads a target, with the collector's
  account. A nil or empty answer MUST fail the scrape saying the Sentinel
  knows no primary of that name; an answer that is no address and port
  MUST fail it saying so. The primary MUST be held to the target lists
  as the target is, before it is dialed and at the address dialed, and
  one refused MUST never be connected to. Over TLS both connections MUST
  speak it, the primary's certificate verified for `tls.server_name` or
  else the address answered. The document MUST hold `sentinel`, an
  object of `SENTINEL MASTER`'s fields, each value read as an INFO value
  is, and `address`, the `host:port` read. The Sentinel's replies and the
  primary's MUST be held to the response limit together.
- The target MUST be `host`, `host:port`, `[v6]:port`, or a `redis://` or
  `rediss://` URL of one, port 6379 when it names none, with no user,
  path, query or other scheme, which MUST be answered `400` before
  anything is sent, as a static target's is refused at load. It is
  required. `rediss://`, or a written `tls` block, an empty one (`tls: {}`)
  as much as one with keys, MUST make the scrape speak TLS, verifying the
  certificate against `tls.server_name` or the target's host, with the
  system roots and `ca_file`'s; `redis://` with a written `tls` block MUST
  be refused. The target lists
  MUST be applied to the host before the connection and to the address
  connected to, as `http`'s are, a refused one answered `403`, counted in
  `http_exporter_targets_refused_total`, and never connected to.
- Each scrape MUST make its own connection and close it when the scrape
  is over. The connection, the TLS handshake and every read and write MUST
  end at the scrape's deadline, 30 seconds without one, or when it is
  cancelled, and a scrape ended so MUST fail with the deadline's error.
- A failed scrape MUST be reported in the `redis` stage. The decoder MUST
  be `json`, as for `grpc`; another decoder, and a transform other than
  `jq`, `yq` and `python`, MUST be refused at load.
- A debug probe's request section MUST show each command sent, `AUTH` and
  `SELECT` among them, by its name and `redis://host:port/db` (`rediss://`
  over TLS), with the command's words in a `Redis-Command` header,
  `AUTH`'s password as `***`, and the reply's kind, or the error, as its
  outcome. The verbose `url` label MUST be `redis://host:port/db` or
  `rediss://host:port/db`, and `http_method` MUST be `INFO` for a
  collector without `commands`, `COMMANDS` for one with `commands` and no
  `info_sections`, and `INFO+COMMANDS` for one with both.
- A reply holding bytes that are not UTF-8 — a bulk or simple string, a
  name of a reply's pairs, `CLUSTER INFO`'s or `INFO`'s text — MUST fail the
  scrape, naming the command, and MUST NOT be put in the document with
  U+FFFD in place of those bytes.
- The document MUST be held to the response limit as the replies are: one
  whose JSON is larger than the limit MUST fail the scrape as a limit
  exceeded, saying the document's size.
- A static target MAY set `timeout`, its own account, `username` and
  `password_file`, and its own `tls` block (§ 42.14); a probe MAY set
  `timeout` and `param_<name>`. A target's `password_file` MUST replace the
  collector's, as the password of the collector's `username`, or of the
  target's own, which MUST replace the collector's; a target's `username`
  without its own `password_file`, and a `redis://` target with its own
  `tls` block, MUST be refused at load.
