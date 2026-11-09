# Prometheus Universal Exporter — `grpc` Request Type Specification

This is the `grpc` request type's section of § 5.1 of the
[exporter specification](SPECIFICATION-EXPORTER.md), in a document of its
own so that a change to the type is a change to this document. Everything
the request types share — the registry and the rules every type is held to
(§ 5.1), building without a type, decoding, transforms, error policies,
limits, caching, static targets and exposition — is in the exporter
specification, and a section number here, such as § 42.14, is one of its
sections. It holds § 9a too, how a gRPC answer becomes the JSON document the
transforms read. What a user reads of the type is [GRPC.md](GRPC.md).

## `grpc`

A `grpc` collector calls one unary gRPC method, or one server streaming
method with `stream_messages`, on the probe's `target`, the server, and hands
the answer to the transforms as JSON (§ 9a). The call MUST
be described in configuration alone, with message types built at run time
from descriptors, so no service needs generated code or a new build. Its
keys:

| Key | Default | Rule |
| --- | --- | --- |
| `rpc` | none | **Required.** `package.Service/Method`; a leading `/` MUST be dropped. MUST match `^([A-Za-z_][A-Za-z0-9_]*\.)*[A-Za-z_][A-Za-z0-9_]*/[A-Za-z_][A-Za-z0-9_]*$` and hold no placeholder. |
| `message` | `{}` | The request message in the protobuf JSON mapping. MAY hold placeholders (§ 42.10a) with the filters `json`, `number` and `raw`; `form` and `xml` MUST be refused. |
| `metadata` | none | Keys MUST be `[0-9a-z_.-]+`; `-bin` keys, `grpc-` and `:` keys, `content-type`, `te`, `host`, `connection` and `user-agent` MUST be refused, and `authorization` when a credential key is set. Values MAY hold placeholders, filled with the header rule; a literal value with a control character MUST be refused. |
| `descriptors` | none | `reflection`, `protoset` or `proto`, in lower case and without blanks around it; any other spelling MUST be refused naming the three. **Required**, but for a method of `grpc.health.v1.Health`, whose types MUST be built in: `Check`, `List`, every service's status in one call, and `Watch`, with `stream_messages`. |
| `protoset_file` | none | Required with, and only with, `protoset`. |
| `proto_files` | none | Required with, and only with, `proto`. |
| `proto_import_paths` | each file's directory | Only with `proto`. |
| `basic_auth` / `basic_auth_file`, `bearer_token` / `bearer_token_file`, `forward_authorization`, `forward_headers`, `tls`, `max_response_bytes` | as `http` | With `http`'s rules; credentials MUST be sent as the `authorization` metadata, their files read at every call. A `forward_headers` name gRPC reserves MUST be refused. |
| `retry` | none | `attempts` and `backoff` as `http`'s; `codes`, the gRPC status codes retried by name, case-insensitive, `[UNAVAILABLE]` when left out. An unknown name and `OK` MUST be refused. `non_idempotent`, `statuses` and `retry_after` MUST be refused, and `codes` for every other type, in a collector and a static target. |
| `stream_messages` | none | From 1 to 1000; set, the method MUST be server streaming, and unset it MUST be unary, either refused naming the key otherwise. The stream MUST be read until the server ends it or that many messages have come, when the call MUST end it; the answer MUST be `{"messages": [...], "ended": ...}`, each message rendered as a unary answer, `ended` true when the server ended the stream. A status other than `OK` ending it MUST be the call's status, as a unary call's; the messages MUST be held to the response limit together as JSON, a limit exceeded past it naming the message. |
| `accept_codes` | none | Status codes other than `OK`, by name as `retry.codes`, whose calls are answers: such a call MUST NOT be retried, and MUST be answered as an empty JSON object with the code as its status (`$status`, `response.status_code`, `http_exporter_scrape_grpc_status_code`) and the status message as the `grpc-message` header, `grpc-status` holding the code. An unknown name and `OK` MUST be refused, and the key for every other type. |

- A target MUST be `host:port`, `dns:///host:port`, `grpc://host:port` or
  `grpcs://host:port`, with a port from 1 to 65535; any other scheme, a path,
  a query or a user MUST be refused, a probe's with `400` before anything is
  called and a static target's at load. A bare target MUST be plaintext;
  `grpcs://` or a written `tls` block MUST make the call TLS, an empty one
  (`tls: {}`) as much as one with keys, verified with the system roots, and
  `grpc://` with a written `tls` block MUST be refused.
- A target's host MUST be handed to grpc-go as a DNS name or address
  (`dns:///host:port`) in every form of target, so that a host named like one
  of grpc-go's resolvers, such as `unix-abstract` or `passthrough`, is that
  name looked up over DNS, and behind a proxy the name of the proxy's
  `CONNECT`, and never selects a resolver or a unix socket. The host `unix`,
  in any case, MUST be refused as a malformed target is, with a message that
  says grpc-go would dial it as a unix socket: grpc-go dials the address
  `unix:<port>`, which is what a proxied target stays, as a unix socket
  whatever produced it. Nothing MUST be dialled for a refused target.
- At load, each placeholder of the message MUST take its default, or a
  stand-in of its filter's kind when it has none, and the message MUST then be
  JSON.
- With `protoset` or `proto`, the service and method MUST be looked up at load
  and by `--dry-run`: a missing service or method, naming the methods the
  service has, and a client or bidirectional streaming method, a server
  streaming method without `stream_messages` and a unary one with it, naming
  its kind, MUST be refused; a message whose placeholders all have defaults,
  and a static target's message, MUST be checked against the input type,
  unknown fields refused. A descriptor set MUST be read with its imports, an
  import it lacks taken from the types built into the exporter when they hold
  it, and refused naming it otherwise. `.proto` files MUST be compiled with the
  well-known types built in, imports resolved only in the import paths, each
  file named by its path from the import path it is under, and a compile
  error MUST name the file and line. Both MUST be read again at a call when a
  file they were read from changed on disk: the descriptor set; a `.proto`
  file named, or one imported, through however many files; and a file that
  has appeared in an import path before the one a file was found in, which
  MUST be the one compiled from then on. A file MUST be stamped before it is
  read, so that one replaced while the files were being read is read again.
  The check at a call MUST tell every change the configuration watch tells
  (§ 24.1), and the watch every change it tells: a file whose modification
  time, size or permissions changed, that appeared or disappeared, or whose
  path leads to another file — a swapped `..data` link, even to a file with
  the time and size of the old, or another file renamed over it with the
  same time, size and permissions, which only its identity tells — MUST be
  read again at the next call. One no longer readable MUST fail that call
  with the system's error (`permission denied`), an unreadable file in an
  earlier import path included, which MUST NOT be passed over for the next
  import path's file of that name as a missing one is; where the exporter
  can read it whatever its permissions, as root, the call MUST read the files
  again and MAY succeed. Telling which file a path leads to MUST NOT cost
  that check a look at each component of the path at every call: one look at
  the file, as the time and size take, MUST tell it where the system says
  which file it is, its device and inode, which the watch MUST stamp too.
  Neither MUST read a file to tell whether it changed: a filesystem that
  gives an unchanged file another inode number makes it a changed file, read
  again at the next call and reloaded for at the next tick.
  With the configuration watch on, a change to one of these files MUST also
  reload the configuration at the next tick, which checks it against them
  (§ 24.1).
- With `reflection`, the target's `grpc.reflection.v1` service MUST be asked,
  and `v1alpha` when the server does not implement v1, for the file defining
  the service and every file it imports, asking by name for any the answer
  left out. An answer MUST be kept per connection and service for 10 minutes,
  and MUST then be forgotten by a timer, whether or not another call comes.
  At most 1000 answers, of at most 128 MiB of files as the server sent them
  all together, MUST be kept, the one a probe used longest ago forgotten
  first to make room; an answer larger than 128 MiB alone MUST serve the
  probes that asked for it and MUST NOT be kept.
  Probes missing the same answer together MUST share one question, which MUST
  run with its own timeout, detached from the deadline and cancellation of the
  probe that started it, so that probe ending does not fail the others; each
  probe MUST wait for the answer only as long as its own deadline allows.
  The question MUST carry the call's metadata and credentials.
  The question MUST take at most the collector's response bound (§ 20) of
  files, or 10 MiB when the bound is smaller, counted as the bytes they arrive
  as, and at most 4096 files, the bounds holding the files of every answer to
  it together; past either it MUST fail, counted in
  `http_exporter_series_limit_exceeded_total`, saying which bound it passed
  and naming `descriptors: protoset` or `proto`, and for the bytes
  `max_response_bytes`. Probes sharing a question MUST share the response
  bound of the collector whose probe started it. A failed question MUST NOT be
  kept: the next probe asks again. The imports of the files received, and of a
  descriptor set, MUST be followed without a frame of the stack per file, so
  that no chain of imports can overflow it.
  A call failing with `UNIMPLEMENTED`, or whose answer does not decode, MUST
  drop the answer, ask again and call again once, apart from the retries. A
  server without reflection MUST fail saying so and naming `protoset` and
  `proto`; a reflection call ending with a status MUST be reported as a call
  with that status, and MUST be retried as a call with that status is: a
  status `retry.codes` lists, after `backoff`, the question and the call
  sharing the `attempts`.
- A message that does not fit the input type MUST fail the probe at the
  `message` stage before the target is called, naming the field; a call
  ending with a status other than `OK` MUST fail at the `grpc` stage with the
  status's name and message, logged with a `grpc_code` attribute. The status
  codes listed in `retry.codes` MUST be retried up to `attempts` times, after
  `backoff`, within the probe's budget. A retry MUST NOT be answered from the
  connection's earlier failure: when the connection is waiting to try again,
  the retry MUST end the wait and connect at once, waiting up to one second
  for the connection before it calls, so that a retry of a refused connection
  dials the target again, and MUST end the wait again every quarter of that
  second while no connection is made, as a call does (below). The probe's
  budget or `timeout` MUST be the call's deadline. An answer over the
  response limit MUST fail with `RESOURCE_EXHAUSTED` and count as a limit.
  An answer's headers, and its trailers, MUST each be bounded as an `http`
  response's headers are (§ 4), at 1 MiB, the bound advertised to the server
  as the connection's largest header list; an answer whose headers or
  trailers the client reads past it, the call's or the reflection question's,
  MUST fail
  as `INTERNAL`, counted as a limit, with the error that says the headers
  were too large, and MUST NOT be retried, whatever `retry.codes` and
  `accept_codes` list.
- Metadata MUST be the collector's, a static target's own over it, the
  credentials as `authorization`, and the forwarded headers lower-cased over
  all of them, a reserved name left out.
- Connections MUST be kept per target and TLS settings and reused, closed
  after 5 minutes unused, and made again when a TLS file changes on disk,
  without cancelling a call still using the old one. `--dry-run` MUST read
  the `tls` block's files of a collector, and of a static target's own
  block, and warn of one a connection would fail on. A connection MUST be
  closed by a timer once no call or reflection question has held it for 5
  minutes, whether or not another call comes, since grpc-go goes on
  reconnecting a connection whose server is gone. At most 1000 connections
  MUST be kept: a new one past them MUST close the one no call holds that
  was used longest ago. A connection a call holds MUST NOT be closed for the
  bound; while calls hold more than 1000, as many MUST be kept, and the
  excess MUST be closed as their calls end. An attempt to connect —
  the TCP connection, the TLS handshake and the server's answer to the
  HTTP/2 preface — MUST be given up after 20 seconds, a call still waiting
  for it then failing as `UNAVAILABLE`. A connection that failed
  to connect MUST wait at most 5 seconds before trying again, and a call that
  finds it waiting MUST end the wait and try at once, within its deadline, so
  a server that came back is reached at the next probe. A call cannot tell a
  connection that is waiting from one that is trying at that moment, whose
  wait there is none of to end: so for as long as the connection reports its
  failure, within the second the call waits for it, the call MUST end the
  wait again every quarter of a second, and an attempt begun before the
  server came back, which then fails, MUST NOT fail the call with it. A
  target that stays down is thereby dialed at most four times in that
  second. A connection that stopped answering without being closed MUST be
  replaced: when the probe's own deadline ends a call, or the wait for a
  reflection answer, on a connection that is established, the connection
  MUST be dropped, so the next call to the target makes a new one. A
  `DEADLINE_EXCEEDED` the server sent, and a deadline that ends while the
  connection is still being made, MUST NOT drop it. Dropping MUST NOT cancel
  the other calls using the connection, nor
  a reflection question in flight on it: it MUST be closed when the last of
  them has ended, and a call made after the drop MUST NOT wait for a
  reflection question asked on the dropped connection. Client keepalive pings
  MUST NOT be sent.
- It MUST accept the `/probe` parameters `timeout`, `insecure_skip_verify`,
  `retry_attempts`, `retry_backoff`, `header_<name>`, `param_<name>` and
  `message`, which replaces `request.message`, placeholders and all; a
  parameter only another type accepts MUST be answered `400`. A static target
  MAY set `message`, `metadata`, `timeout`, `insecure_skip_verify`, `retry`,
  `accept_codes`, the basic and bearer credential keys and its own `tls`
  block (§ 42.14), which a `grpc://` target MUST be refused with at load;
  its message and
  metadata MUST NOT
  hold placeholders, its message MUST be JSON, and with descriptors read at
  load fit the input type. Its message MUST enter its cache key as the probe
  parameter `message`, and its metadata and retry codes a section a probe's
  key never has.
- `decoder.type` left unset or `auto` MUST be `json`, and any other decoder
  MUST be refused; the transform MUST be `jq`, `yq` or `python`.
- The verbose `url` label MUST be `grpc://host:port/package.Service/Method`,
  `grpcs://` over TLS, and `http_method` MUST be `POST`.

## 9a. gRPC answers as JSON

A `grpc` call answered `OK` (§ 5.1) MUST become a fetch result with status
200, `Content-Type: application/json`, the answer's headers and trailers,
binary ones aside, as its headers, and as its body the answer in the protobuf
JSON mapping, compacted so the same answer is always the same bytes, with:

- fields at their zero value written, so a rule never misses a value proto3
  would leave out;
- field names as the `.proto` file writes them;
- 64-bit integers as strings, enums as their names, and `Timestamp` and
  `Duration` as the mapping writes them;
- a `google.protobuf.Any` as the message it carries, with its `@type`, its
  type looked up among the method's own types, from whichever descriptors
  described it, and then the types built into the exporter; a type in
  neither MUST fail the answer, naming it.

A numeric string MUST be read by metric values as a number.

The JSON MUST be bounded by `max_response_bytes` as the message is: an answer
whose JSON is over the limit MUST fail as an answer over the limit, the call
itself `OK`, its error saying how large the JSON is and the limit. Writing
every zero value can make the JSON far larger than the message: an empty
element of a repeated field is two bytes on the wire and every one of its
fields as JSON. The JSON's size MUST therefore be counted from the message as
it arrived, before it is decoded, and an answer whose count is over the limit
MUST be refused then, its error saying the answer is at least that many bytes
as JSON: neither decoding nor rendering an answer that is refused may cost
more than reading it does. The count MUST NOT be more than the JSON of a
message that carries no single-valued field and no map key twice, as no
encoder writes one, so that such an answer within the limit is never refused
by it; it MUST be exact but for floating-point numbers, which count as a
digit, and the well-known types written in a form of their own, which count
at their shortest. An answer the count lets through MUST still fail when its
rendered JSON is over the limit.

---
