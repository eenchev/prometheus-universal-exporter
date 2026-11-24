# AMQP

The `amqp` request type asks an AMQP 0-9-1 broker, RabbitMQ and the brokers
that speak its protocol, how deep its queues are, over AMQP itself, and
hands the answer to the transforms as a JSON document: each queue's ready
messages and consumers, and what the broker says of itself. Use it where
the broker's management API is closed or not installed but a client may
connect; where the API is open, an `http` collector reads more from it.

The type is opt-in: the default build and the published image leave it out,
so a build carries it only when asked
([Building with amqp](#building-with-amqp)).

## Building with amqp

`OPT_IN_REQUEST_TYPES` names the opt-in types a build adds to the types it
carries, comma-separated:

```sh
make build OPT_IN_REQUEST_TYPES=amqp              # bin/prometheus-universal-exporter
docker build --build-arg OPT_IN_REQUEST_TYPES=amqp -t exporter:amqp .
go build -tags "$(sh tools/request-type-tags.sh '' '' amqp)" .
```

The tag is `request_type_amqp`. A configuration with an `amqp` collector
is refused by a build without the type, at startup, on a reload and by
`--dry-run`, saying how to build one that has it.

## A collector

```yaml
collectors:
  - name: rabbit
    request:
      type: amqp
      username: monitor
      password_file: /etc/exporter/amqp-password
      queues: [orders, emails]
    transform:
      type: jq
    metrics:
      - name: amqp_queue_messages_ready
        description: Messages ready for delivery in the queue
        items: .queues[] | select(.exists)
        expression: .messages
        labels:
          - name: queue
            expression: .name
      - name: amqp_queue_consumers
        description: Consumers of the queue
        items: .queues[] | select(.exists)
        expression: .consumers
        labels:
          - name: queue
            expression: .name
      - name: amqp_queue_exists
        description: Whether the broker has the queue
        items: .queues[]
        expression: .exists
        labels:
          - name: queue
            expression: .name
```

```text
/probe?collector=rabbit&target=rabbit1.example.com
```

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `amqp`. |
| `queues` | — | The queues asked about, by name, at most 100, each at most 255 bytes. Left out, the scrape logs in and asks about none, which still says whether the broker takes the login and how long it took. |
| `vhost` | `/` | The virtual host opened, at most 255 bytes. |
| `username`, `password_file` | `guest` and `guest` | The account logged in with, both or neither. The file is read at each scrape. A broker out of the box takes `guest` from its own host alone. On RabbitMQ the account needs access to the virtual host and no permission in it. |
| `tls` | — | For an `amqps://` target, how the broker's certificate is verified, and a client certificate, as for [any type that speaks TLS](REQUESTS.md#tls). |
| `allowed_targets`, `denied_targets` | — | The hosts the collector may and may not reach, as for [any type that reaches a host](REQUESTS.md#restricting-targets), checked on the address the connection is made to. |

No other request key applies, and setting one is a startup error. The
queues, the virtual host and the account take no placeholder. A probe takes
`timeout` and `param_<name>` parameters for the collector's
[label values](REQUESTS.md#in-label-values).

The decoder is `json`, whether `decoder.type` is left out or set to it; any
other decoder, and a transform that does not read JSON, is refused when the
configuration loads. Use `jq`, `yq` or `python`.

## The target

The target is the broker: `host` or `host:port`, port 5672 when it names
none; `amqp://host`, the same; or `amqps://host` for TLS, port 5671. A
path, a query, a user or another scheme is refused with `400` before
anything is sent, a static target's when it loads; the virtual host is the
collector's `vhost`, not a path of the target.

## What a scrape does

A scrape opens one connection, logs in with the PLAIN mechanism, opens the
virtual host, and declares each queue *passively*: the broker creates
nothing and answers how many messages are ready in the queue and how many
consumers it has. It then closes the connection as the protocol has it.
Nothing is published, consumed or acknowledged.

A broker answers a queue it does not have by closing the channel, so each
queue after such a one is asked on a new channel of the same connection.

## The document

```json
{
  "host": "rabbit1.example.com",
  "port": 5672,
  "address": "192.0.2.10:5672",
  "tls": false,
  "vhost": "/",
  "product": "RabbitMQ",
  "version": "4.0.2",
  "platform": "Erlang/OTP 26.2.5",
  "cluster_name": "rabbit@rabbit1",
  "server_properties": {"product": "RabbitMQ", "version": "4.0.2"},
  "mechanisms": ["PLAIN", "AMQPLAIN"],
  "connect_seconds": 0.012,
  "queues": [
    {"name": "orders", "exists": true, "messages": 42, "consumers": 3, "error": ""},
    {"name": "emails", "exists": false, "messages": null, "consumers": null, "error": ""}
  ],
  "duration_seconds": 0.015
}
```

- `queues` has an entry for each queue named, in the order they are named.
  `messages` is the messages **ready** for delivery: those delivered to a
  consumer and not yet acknowledged are not among them, as the protocol
  does not say them.
- **A queue the broker does not have is not a failure**: its `exists` is
  false and its counts `null`, so a rule decides what a missing queue
  means. A queue the broker would not answer about for another reason, an
  account without access to it or a queue another connection holds
  exclusively, has `exists` false and what the broker said in `error`.
- `product`, `version`, `platform` and `cluster_name` are the broker's
  properties of those names, and `server_properties` every one of them
  that is text.
- `connect_seconds` is how long it took to connect, log in and open the
  virtual host.

## Static targets

A [static target](STATIC-TARGETS.md) of an `amqp` collector names the
broker, and may set `timeout` under its `request`:

```yaml
interval: 1m
targets:
  - name: rabbit_1
    collector: rabbit
    target: rabbit1.example.com
  - name: rabbit_2
    collector: rabbit
    target: amqps://rabbit2.example.com
```

## Limits

A scrape is bounded by its deadline, the probe's `timeout` or the scrape's
own, 30 seconds when neither is set. A collector names at most 100 queues,
and a frame of the broker's is read up to 1 MiB.

## Errors and self-metrics

A failed request is reported in the `amqp` stage and follows
`error_handling.on_fetch_error`, naming the broker: one that cannot be
reached, a TLS handshake that failed, what answered is no AMQP 0-9-1
broker, a login it did not take
(`rabbit1.example.com:5672 refused the login of monitor: 403
ACCESS_REFUSED ...`), a virtual host it does not have or the account may
not open, or a password file that cannot be read or is empty. A host the
target lists refuse counts in `http_exporter_targets_refused_total`,
answered `403`. Transforms see no status (`$status` is `null`, Python's
`response.status_code` `None`) and the header
`Content-Type: application/json`.

A [debug probe](CONFIGURATION.md#debug-probes) shows the request with how
it ended:

```text
Requests
  1. DECLARE-PASSIVE amqp://rabbit1.example.com:5672 -> 2 queues asked of RabbitMQ 4.0.2
     Amqp-Vhost: /
```

With [verbose self-metrics](SELF-METRICS.md#verbose-per-request-self-metrics)
a request is labelled `url="amqp://rabbit1.example.com:5672"`, `amqps://`
over TLS, and `http_method="DECLARE-PASSIVE"`.

## Limitations

- **AMQP 0-9-1 only.** AMQP 1.0, which Azure Service Bus, ActiveMQ Artemis
  and RabbitMQ's 1.0 listener speak, is another protocol and is not spoken.
- **PLAIN logins only.** A broker that offers only `EXTERNAL` or another
  mechanism fails the scrape, saying which it offers. A client certificate
  can be presented for TLS, but the login is still the account's.
- **Ready messages and consumers, no more.** Unacknowledged messages,
  rates, memory and the list of queues are the management API's; the
  protocol has no method for them. Queues are named, not discovered.
- **The password goes as the protocol sends it**, in the clear on an
  `amqp://` connection. Use `amqps://` across a network that is not yours.

## Troubleshooting

- **The broker closes the connection at the login.** The account or its
  password is wrong, or the account is `guest` from another host, which
  RabbitMQ refuses unless `loopback_users` is changed. Make an account for
  monitoring.
- **`refused the virtual host "/": 530 NOT_ALLOWED`.** The account has no
  access to the virtual host; grant it, or set `vhost` to the one it has.
- **A queue has `exists` false and an `error` of `405 RESOURCE_LOCKED`.**
  The queue is exclusive to another connection, and only that connection
  may ask about it.
- **`does not speak AMQP 0-9-1`.** The port is another protocol's: the
  management API's 15672, or an AMQP 1.0 listener.
