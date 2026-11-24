# Prometheus Universal Exporter — `amqp` Request Type Specification

This is the `amqp` request type's section of § 5.1 of the
[exporter specification](SPECIFICATION-EXPORTER.md), in a document of its
own so that a change to the type is a change to this document. Everything
the request types share — the registry and the rules every type is held to
(§ 5.1), building without a type, decoding, transforms, error policies,
limits, caching, static targets and exposition — is in the exporter
specification, and a section number here, such as § 42.14, is one of its
sections. What a user reads of the type is [AMQP.md](AMQP.md).

## `amqp`

An `amqp` collector asks an AMQP 0-9-1 broker, the probe's `target`, about
the queues it names and hands the answer to the transforms as a JSON
document. It MUST need nothing outside Go's standard library, and MUST be
opt-in, built only under the tag `request_type_amqp`.

| Key | Default | Rule |
| --- | --- | --- |
| `queues` | — | At most 100 names, each of 1 to 255 bytes with no control character, none twice. |
| `vhost` | `/` | At most 255 bytes, no control character. |
| `username`, `password_file` | `guest`, `guest` | Both or neither; the file is read at each scrape, and an empty one fails it. |
| `tls` | — | As for `http` (§ 42.14), used for an `amqps://` target; `cert_file` and `key_file` together. |
| `allowed_targets`, `denied_targets` | — | As for `http`, on the broker's name before the connection and on the address it is made to (§ 26.1). |

- Any other request key MUST be refused at load, and so MUST a placeholder
  in `queues`, `vhost`, `username` or `password_file`. A probe MUST take
  `timeout` and `param_<name>` parameters, and a static target's request
  block `timeout`.
- A target MUST be `host`, `host:port`, `amqp://host[:port]` or
  `amqps://host[:port]`, port 5672, and 5671 for `amqps`. Another scheme, a
  path, a query or a user MUST be refused, a probe's with `400` before
  anything is sent and a static target's at load. An `amqps` target MUST be
  spoken to over TLS from the first byte, and no other.
- A scrape MUST be one connection: the protocol header of 0-9-1,
  `Connection.Start-Ok` with the PLAIN mechanism and the capability
  `authentication_failure_close`, `Connection.Tune-Ok` with the broker's
  limits and no heartbeat, `Connection.Open` of `vhost`, and, for each
  queue in the order named, `Queue.Declare` with the passive bit alone on a
  channel. It MUST send no other method but those that open and close a
  channel and close the connection: nothing MUST be published, consumed,
  acknowledged or declared other than passively.
- A queue's `Declare-Ok` MUST give its entry `exists: true` and its
  `messages` and `consumers`. A `Channel.Close` in its place MUST give
  `exists: false` with both `null`, MUST NOT fail the scrape, and MUST set
  the entry's `error` to the reply code and text unless the code is `404`;
  the queues after it MUST be asked on another channel.
- A broker that offers no PLAIN mechanism, a `Connection.Close` in answer
  to the login or to the virtual host, a connection closed at the login,
  and an answer that is no AMQP 0-9-1 MUST each fail the scrape, saying
  which. What the broker said MUST be shown with its control characters
  replaced, credentials masked, and at most 200 bytes of it. A frame larger
  than 1 MiB MUST fail the scrape.
- The scrape's deadline — the probe's `timeout`, the scrape's own, 30
  seconds when neither is set — MUST bound the whole scrape.
- The answer MUST be a JSON document of `host`, `port`, `address` (the
  address connected to), `tls`, `vhost`, `product`, `version`, `platform`
  and `cluster_name` (the broker's properties of those names, `""` for one
  it does not send), `server_properties` (each of its properties that is
  text), `mechanisms`, `connect_seconds` (to the virtual host's opening),
  `queues` (each `name`, `exists`, `messages`, `consumers`, `error`) and
  `duration_seconds`. `queues` MUST be `[]` for a collector that names
  none.
- The decoder MUST be `json`, as for `grpc`; another decoder, and a
  transform other than `jq`, `yq` and `python`, MUST be refused at load.
- A failure MUST be reported in the `amqp` stage. The scrape MUST be one
  request of a debug report and of an exported trace, shown as
  `DECLARE-PASSIVE` and `amqp://host:port`, `amqps://` over TLS, with the
  virtual host and never the password. The verbose `url` label MUST be the
  same address, and `http_method` MUST be `DECLARE-PASSIVE`.
- `--dry-run` MUST warn of a password file, and of the tls files of a
  collector that names any, that cannot be read, and MUST NOT fail on them.
