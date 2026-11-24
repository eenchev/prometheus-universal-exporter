# NATS

The `nats` request type asks a NATS server whether it answers and how
fast, over the client protocol, and, with JetStream, how the account's
streams and consumers stand, and hands the answer to the transforms as a
JSON document. Use it where the server's monitoring port (8222) is closed
but a client may connect; where that port is open, an `http` collector
reads more from it.

The type is opt-in: the default build and the published image leave it out,
so a build carries it only when asked
([Building with nats](#building-with-nats)).

## Building with nats

`OPT_IN_REQUEST_TYPES` names the opt-in types a build adds to the types it
carries, comma-separated:

```sh
make build OPT_IN_REQUEST_TYPES=nats              # bin/prometheus-universal-exporter
docker build --build-arg OPT_IN_REQUEST_TYPES=nats -t exporter:nats .
go build -tags "$(sh tools/request-type-tags.sh '' '' nats)" .
```

The tag is `request_type_nats`. A configuration with a `nats` collector
is refused by a build without the type, at startup, on a reload and by
`--dry-run`, saying how to build one that has it.

## A collector

```yaml
collectors:
  - name: nats
    request:
      type: nats
      username: monitor
      password_file: /etc/exporter/nats-password
      jetstream: true
      streams: [ORDERS]
      consumers: [ORDERS/billing]
    transform:
      type: jq
    metrics:
      - name: nats_rtt_seconds
        description: The round trip of a PING
        expression: .rtt_seconds
        labels:
          - name: version
            expression: .info.version
      - name: nats_stream_messages
        description: Messages held by the stream
        items: .streams[] | select(.found)
        expression: .info.state.messages
        labels:
          - name: stream
            expression: .name
      - name: nats_consumer_pending
        description: Messages the consumer has yet to be delivered
        items: .consumers[] | select(.found)
        expression: .info.num_pending
        labels:
          - name: stream
            expression: .stream
          - name: consumer
            expression: .name
```

```text
/probe?collector=nats&target=nats1.example.com
```

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `nats`. |
| `jetstream` | `false` | Ask the account's JetStream state. Required for `streams` and `consumers`. |
| `streams` | — | The streams asked about, by name, at most 100. |
| `consumers` | — | The consumers asked about, each written `stream/consumer`, at most 100. |
| `username`, `password_file` | — | The user logged in as, both or neither. The file is read at each scrape. |
| `bearer_token_file` | — | A token logged in with, in place of a user and a password. The file is read at each scrape. |
| `tls` | — | How the server's certificate is verified, and a client certificate, as for [any type that speaks TLS](REQUESTS.md#tls), when the connection is TLS (see [The target](#the-target)). |
| `max_response_bytes` | `10MiB` | The most bytes of one answer of the JetStream API. |
| `allowed_targets`, `denied_targets` | — | The hosts the collector may and may not reach, as for [any type that reaches a host](REQUESTS.md#restricting-targets), checked on the address the connection is made to. |

No other request key applies, and setting one is a startup error. The
names and the account take no placeholder. A probe takes `timeout` and
`param_<name>` parameters for the collector's
[label values](REQUESTS.md#in-label-values).

The decoder is `json`, whether `decoder.type` is left out or set to it; any
other decoder, and a transform that does not read JSON, is refused when the
configuration loads. Use `jq`, `yq` or `python`.

## The target

The target is the server: `host` or `host:port`, port 4222 when it names
none; `nats://host`, the same; or `tls://host`. A path, a query, a user or
another scheme is refused with `400` before anything is sent, a static
target's when it loads.

A NATS connection starts in plaintext and the server says whether it is to
be TLS. The connection is turned to TLS when the server requires it, and
when the target says `tls://`; a `tls://` target of a server that offers no
TLS fails the scrape, so a credential is not sent in the clear to a server
expected to be secure. To a server that does not require TLS and a target
without `tls://`, the user's password or the token goes as the protocol
sends it, in the clear.

## What a scrape does

A scrape connects, reads the server's `INFO`, turns to TLS as above, logs
in with `CONNECT`, and times a `PING` to its `PONG`. With `jetstream` it
then asks the JetStream API for the account's state, and for each stream
and consumer named, with requests that read and change nothing
(`$JS.API.INFO`, `$JS.API.STREAM.INFO.<stream>`,
`$JS.API.CONSUMER.INFO.<stream>.<consumer>`). No message is published to a
subject of yours, and nothing is consumed.

## The document

```json
{
  "host": "nats1.example.com",
  "port": 4222,
  "address": "192.0.2.20:4222",
  "tls": false,
  "info": {"server_id": "NB3X...", "server_name": "n1", "version": "2.10.18", "max_payload": 1048576, "jetstream": true, "cluster": "east"},
  "connect_seconds": 0.004,
  "rtt_seconds": 0.0006,
  "jetstream": {"memory": 0, "storage": 2048, "streams": 1, "consumers": 1, "limits": {"max_memory": -1, "max_storage": -1}},
  "jetstream_error": "",
  "streams": [
    {"name": "ORDERS", "found": true, "error": "", "info": {"config": {"name": "ORDERS"}, "state": {"messages": 120, "bytes": 4096, "first_seq": 1, "last_seq": 120, "consumer_count": 1}}}
  ],
  "consumers": [
    {"stream": "ORDERS", "name": "billing", "found": true, "error": "", "info": {"num_pending": 7, "num_ack_pending": 2, "num_redelivered": 0, "num_waiting": 1}}
  ],
  "duration_seconds": 0.009
}
```

- `info` is the server's `INFO` as it sent it, without its nonce: its
  name, version, limits and, in a cluster, the cluster's name.
- `rtt_seconds` is the round trip of a `PING` after the login, and
  `connect_seconds` how long it took to connect and log in.
- `jetstream` is the account's JetStream state as the API answers it, and
  each stream's and consumer's `info` is the API's answer for it, as it
  writes it, so a rule reads any field of it. Without `jetstream: true`,
  `jetstream` is `null` and the lists are empty.
- **An account without JetStream is not a failure**: `jetstream` is `null`,
  `jetstream_error` says why, and no stream or consumer is asked about.
- **A stream or a consumer the server does not have is not a failure**:
  its `found` is false and its `info` `null`. One the API refused for
  another reason has `found` false and what it said in `error`.

## Static targets

A [static target](STATIC-TARGETS.md) of a `nats` collector names the
server, and may set `timeout` under its `request`:

```yaml
interval: 1m
targets:
  - name: nats_1
    collector: nats
    target: nats1.example.com
  - name: nats_2
    collector: nats
    target: tls://nats2.example.com
```

## Limits

A scrape is bounded by its deadline, the probe's `timeout` or the scrape's
own, 30 seconds when neither is set. An answer of the JetStream API larger
than the response limit, `request.max_response_bytes` or
`limits.max_response_bytes`, fails the scrape. A collector names at most
100 streams and 100 consumers, and a line of the protocol is read up to
1 MiB.

## Errors and self-metrics

A failed request is reported in the `nats` stage and follows
`error_handling.on_fetch_error`, naming the server: one that cannot be
reached, a TLS handshake that failed, what answered is no NATS server, a
login it did not take
(`nats1.example.com:4222 refused: Authorization Violation`), an answer past
the response limit, or a credential file that cannot be read or is empty.
A host the target lists refuse counts in
`http_exporter_targets_refused_total`, answered `403`. Transforms see no
status (`$status` is `null`, Python's `response.status_code` `None`) and
the header `Content-Type: application/json`.

A [debug probe](CONFIGURATION.md#debug-probes) shows the request with how
it ended:

```text
Requests
  1. PING nats://nats1.example.com:4222 -> PONG after 600µs in 9ms
```

With [verbose self-metrics](SELF-METRICS.md#verbose-per-request-self-metrics)
a request is labelled `url="nats://nats1.example.com:4222"`, `tls://` for a
target written so, and `http_method="PING"`.

## Limitations

- **A user and a password, or a token.** NKeys, credentials files and
  account JWTs, which sign the server's nonce, are not spoken; a server
  that takes nothing else refuses the login.
- **TLS after `INFO`.** A server set to begin with the TLS handshake
  (`handshake_first`) is not spoken to.
- **The client port, not the monitoring port.** Connections, routes,
  subscriptions and the server's memory are `/varz` and its neighbours on
  port 8222, which an `http` collector reads.
- **Streams and consumers are named, not discovered.** The account's
  counts of them are in `jetstream`.
- **One server is asked.** In a cluster, each server is a target; the
  JetStream answers are the cluster's whichever is asked.

## Troubleshooting

- **`refused: Authorization Violation`.** The account or the token is
  wrong, or the server wants an NKey or a credentials file.
- **`jetstream_error` says nothing answers the JetStream API.** JetStream
  is not enabled on the server, or not for the account logged in with.
- **A consumer has `found` false.** Its name is the durable's, written
  after its stream's and a slash; an ephemeral consumer has a name the
  server made, which changes.
- **`offers no TLS, and the target says tls://`.** The server has no TLS
  configured; write the target without the scheme, or give the server a
  certificate.
