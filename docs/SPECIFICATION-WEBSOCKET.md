# Prometheus Universal Exporter — `websocket` Request Type Specification

This is the `websocket` request type's section of § 5.1 of the
[exporter specification](SPECIFICATION-EXPORTER.md), in a document of its
own so that a change to the type is a change to this document. Everything
the request types share — the registry and the rules every type is held to
(§ 5.1), building without a type, decoding, transforms, error policies,
limits, caching, static targets and exposition — is in the exporter
specification, and a section number here, such as § 42.14, is one of its
sections. What a user reads of the type is [WEBSOCKET.md](WEBSOCKET.md).

## `websocket`

A `websocket` collector makes a WebSocket connection (RFC 6455) to the
probe's `target`, optionally sends one text message, reads the messages
the collector asks for, and closes. It MUST need nothing outside Go's
standard library, MUST be opt-in (`request_type_websocket`), and MUST keep
no connection beyond a scrape.

| Key | Default | Rule |
| --- | --- | --- |
| `send_text` | — | UTF-8. MAY hold placeholders, written with the filters of an `http` body. |
| `read_messages` | `1` | 1 to 10000. |
| `read_window` | `5s` | Not negative. |
| `subprotocols` | — | HTTP tokens, none twice. |

Of the common part it takes `headers`, whose values MAY hold placeholders
and whose names may not, and which MUST NOT set `Host`, `Upgrade`,
`Connection` or a `Sec-WebSocket-` header, nor `Authorization` beside
`bearer_token_file`;
`bearer_token_file`, `tls`, `max_response_bytes`, `allowed_targets` and
`denied_targets`. A static target's request block takes `timeout` and
`tls`.

- A target MUST be `ws://host[:port][/path][?query]` or `wss://`, ports 80
  and 443, or the same without a scheme, which MUST be `wss` for a
  collector with a `tls` block and `ws` otherwise. A user, a fragment, a
  blank in the path and another scheme MUST be refused, a probe's with
  `400` before anything is sent.
- The handshake MUST be a `GET` of the target's path and query with
  `Upgrade: websocket`, `Connection: Upgrade`, `Sec-WebSocket-Version: 13`,
  a `Sec-WebSocket-Key` of 16 random bytes, the collector's headers, its
  bearer token and its subprotocols, and no extension. It MUST fail the
  scrape unless the answer is a 101 with `Upgrade: websocket`, `Connection`
  holding `Upgrade`, the `Sec-WebSocket-Accept` the key makes, no
  extension, and no subprotocol but one offered; when the answer's
  first line is not an HTTP status line, as soon as it has come; and when
  its status line and headers pass 64 KiB. What the server said — that
  line, an extension or a subprotocol it chose — MUST NOT be told to a
  probe's caller (§ 19.4); the status of an answer that is no 101 MUST be.
- Every frame sent MUST be masked with a random key. A frame received
  that is masked, has a reserved bit set, is a control frame longer than
  125 bytes or not final, is of an unknown opcode, continues no message or
  begins one inside another, and a text message that is not UTF-8, MUST
  fail the scrape. A ping MUST be answered with a pong of its payload. A
  message's frames MUST be put together.
- Reading MUST end when `read_messages` have come, `read_window` has
  passed since the handshake and the message sent, or the server closed;
  the window MUST end within nine tenths of the time the scrape has left.
  A close frame of status 1000 MUST then be sent, and the connection
  closed.
- With `read_messages: 1` the answer MUST be the message's payload as it
  came, of `Content-Type: application/octet-stream` when it is binary and
  for the decoder to detect otherwise; no message MUST fail the scrape,
  naming the window. With more, the answer MUST be a JSON list, empty when
  none came, of the messages in order, each with `payload` or, of a binary
  one, `payload_base64`, `json` when a text payload is JSON, and `binary`;
  the decoder MUST then be `json`.
- The payloads of the messages together, and the list, MUST be at most the
  response limit, more failing as a limit exceeded. The deadline MUST
  bound the whole scrape, 30 seconds when none is set.
- The verbose `url` label and what a log shows of a target MUST leave the
  query out, which may hold a credential; `http_method` MUST be `SEND` for
  a collector with `send_text` and `READ` otherwise. `$tls` MUST describe
  a `wss` connection's certificate.
