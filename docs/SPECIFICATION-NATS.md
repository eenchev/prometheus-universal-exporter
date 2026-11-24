# Prometheus Universal Exporter — `nats` Request Type Specification

This is the `nats` request type's section of § 5.1 of the
[exporter specification](SPECIFICATION-EXPORTER.md), in a document of its
own so that a change to the type is a change to this document. Everything
the request types share — the registry and the rules every type is held to
(§ 5.1), building without a type, decoding, transforms, error policies,
limits, caching, static targets and exposition — is in the exporter
specification, and a section number here, such as § 42.14, is one of its
sections. What a user reads of the type is [NATS.md](NATS.md).

## `nats`

A `nats` collector asks a NATS server, the probe's `target`, over the
client protocol, whether it answers and, with JetStream, how the streams
and consumers it names stand, and hands the answer to the transforms as a
JSON document. It MUST need nothing outside Go's standard library, and
MUST be opt-in, built only under the tag `request_type_nats`.

| Key | Default | Rule |
| --- | --- | --- |
| `jetstream` | `false` | Ask the JetStream API. |
| `streams` | — | At most 100 names, none twice; each 1 to 255 bytes with no blank, `.`, `*`, `>`, `/`, `\` or control character. Refused without `jetstream`. |
| `consumers` | — | At most 100, none twice, each `stream/consumer` of two such names. Refused without `jetstream`. |
| `username`, `password_file` | — | Both or neither; the file is read at each scrape, and an empty one fails it. |
| `bearer_token_file` | — | A token, read at each scrape; refused beside `username` or `password_file`. |
| `tls` | — | As for `http` (§ 42.14), used when the connection is TLS; `cert_file` and `key_file` together. |
| `max_response_bytes` | § 20 | The most bytes of one message the server delivers. |
| `allowed_targets`, `denied_targets` | — | As for `http`, on the server's name before the connection and on the address it is made to (§ 26.1). |

- Any other request key MUST be refused at load, and so MUST a placeholder
  in the account's keys. A probe MUST take `timeout` and `param_<name>`
  parameters, and a static target's request block `timeout`.
- A target MUST be `host`, `host:port`, `nats://host[:port]` or
  `tls://host[:port]`, port 4222. Another scheme, a path, a query or a
  user MUST be refused, a probe's with `400` before anything is sent and a
  static target's at load.
- A scrape MUST be one connection. The server's `INFO` MUST be read first;
  what begins otherwise MUST fail the scrape as no NATS server. The
  connection MUST be turned to TLS, before anything is sent, when `INFO`
  says `tls_required` or the target's scheme is `tls`, verifying the
  target's host unless `tls.server_name` names another; a `tls` target of
  a server whose `INFO` says neither `tls_required` nor `tls_available`
  MUST fail the scrape with nothing sent.
- `CONNECT` MUST carry the user and password, or the token as
  `auth_token`, or neither, with `verbose` false, `headers` and
  `no_responders` true, and MUST be followed by a `PING`; an `-ERR` MUST
  fail the scrape with what it said, and so MUST a connection closed in
  place of the `PONG`. A second `PING` MUST be timed to its `PONG` as
  `rtt_seconds`. A `PING` of the server's MUST be answered, and its `+OK`
  and later `INFO` passed over.
- With `jetstream`, the scrape MUST subscribe to an inbox of its own, of
  96 random bits, and ask `$JS.API.INFO`, then
  `$JS.API.STREAM.INFO.<stream>` for each stream and
  `$JS.API.CONSUMER.INFO.<stream>.<consumer>` for each consumer, in the
  order named, each with an empty payload. It MUST publish to no other
  subject.
- An answer to `$JS.API.INFO` that is a status `503`, no responders, or
  the API's `error` MUST leave `jetstream` `null` and set
  `jetstream_error`, MUST NOT fail the scrape, and MUST end the asking:
  `streams` and `consumers` MUST then be `[]`. An `error` in answer to a
  stream or a consumer MUST give its entry `found: false` and `info: null`,
  MUST NOT fail the scrape, and MUST set the entry's `error` to the code
  and description unless the code is `404`.
- A message larger than the response limit MUST fail the scrape wrapping
  the limit's error (§ 20), before it is read; a line longer than 1 MiB,
  and an answer of the API that is no JSON, MUST fail it. What the server
  said MUST be shown with its control characters replaced, credentials
  masked, and at most 200 bytes of it.
- The scrape's deadline — the probe's `timeout`, the scrape's own, 30
  seconds when neither is set — MUST bound the whole scrape.
- The answer MUST be a JSON document of `host`, `port`, `address` (the
  address connected to), `tls`, `info` (the `INFO` as sent, without
  `nonce`), `connect_seconds` (to the first `PONG`), `rtt_seconds`,
  `jetstream` (the answer to `$JS.API.INFO` as sent, or `null`),
  `jetstream_error`, `streams` (each `name`, `found`, `error`, `info`),
  `consumers` (each `stream`, `name`, `found`, `error`, `info`) and
  `duration_seconds`. An `info` of a stream or a consumer MUST be the
  API's answer as sent.
- The decoder MUST be `json`, as for `grpc`; another decoder, and a
  transform other than `jq`, `yq` and `python`, MUST be refused at load.
- A failure MUST be reported in the `nats` stage. The scrape MUST be one
  request of a debug report and of an exported trace, shown as `PING` and
  `nats://host:port`, `tls://` for a target written so, never with the
  password or the token. The verbose `url` label MUST be the same address,
  and `http_method` MUST be `PING`.
- `--dry-run` MUST warn of a password file, a token file, and the tls
  files of a collector that names any, that cannot be read, and MUST NOT
  fail on them.
