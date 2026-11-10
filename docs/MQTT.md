# MQTT

The `mqtt` request type reads messages from an MQTT broker: a scrape
connects, subscribes to the collector's topics, and hands the transforms
the messages that came, as a JSON document. Use it for what devices and
services publish — sensor readings, a status topic, a broker's `$SYS`
statistics — without a bridge in between.

It stays a pull. Prometheus scrapes the exporter, the exporter asks the
broker, and the answer is what the broker sent on the collector's
subscriptions; nothing is published, and nothing reaches the exporter that
a collector did not subscribe to. MQTT 3.1.1, which every broker speaks.

The type is opt-in: the default build and the published image leave it out,
so a build carries it only when asked
([Building with mqtt](#building-with-mqtt)).

## Building with mqtt

`OPT_IN_REQUEST_TYPES` names the opt-in types a build adds to the types it
carries, comma-separated:

```sh
make build OPT_IN_REQUEST_TYPES=mqtt              # bin/prometheus-universal-exporter
docker build --build-arg OPT_IN_REQUEST_TYPES=mqtt -t exporter:mqtt .
go build -tags "$(sh tools/request-type-tags.sh '' '' mqtt)" .
```

The tag is `request_type_mqtt`. A configuration with an `mqtt` collector
is refused by a build without the type, at startup, on a reload and by
`--dry-run`, saying how to build one that has it. The type is written with
the standard library alone, and adds no dependency.

## A collector

```yaml
collectors:
  - name: sensors
    request:
      type: mqtt
      topics: [sensors/+/temperature]
      username: exporter
      password_file: /etc/exporter/mqtt-password
    transform:
      type: jq
    metrics:
      - name: sensor_temperature_celsius
        items: .messages[]
        expression: .json.celsius
        labels:
          - name: sensor
            expression: .topic | split("/")[1]
      - name: sensor_reading_age_seconds
        items: .messages[]
        expression: .age_seconds
        labels:
          - name: sensor
            expression: .topic | split("/")[1]
```

```text
/probe?collector=sensors&target=mqtts://broker.example.com
```

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `mqtt`. |
| `topics` | — | **Required.** Up to 64 topic filters, with `+` for one level and `#` for the rest: `sensors/+/temperature`, `plant/#`. May contain `{{param_name}}` placeholders. |
| `qos` | `0` | The QoS subscribed at, 0 or 1. |
| `window` | `1s` | How long a scrape reads after subscribing. |
| `max_messages` | `1000` | Reading ends once this many messages have come, before the window has passed. |
| `username`, `password_file` | — | The account. The file is read at each connection. |
| `client_id` | made for each connection | The client identifier. |
| `persistent_session` | `false` | The broker keeps the session between scrapes (see [Between scrapes](#between-scrapes)). |
| `keep_connection` | `false` | The exporter keeps the connection between scrapes. |
| `idle_timeout` | `5m` | With `keep_connection`, how long a connection no scrape used is kept. |
| `keep_alive` | `30s` | The keep alive told to the broker, which counts it in whole seconds. |
| `tls` | — | How the broker's certificate is verified, and a client certificate, as for [any type that speaks TLS](REQUESTS.md#tls). |
| `max_response_bytes` | the collector's limit | The most the messages of a scrape may be, together. |
| `allowed_targets`, `denied_targets` | — | The hosts the collector may and may not reach, as for [any type that reaches a host](REQUESTS.md#restricting-targets). |

A probe takes `timeout` and `param_<name>` parameters, which fill the
placeholders of `topics` and the collector's
[label values](REQUESTS.md#in-label-values). A value written into a topic
is one level of it: one with a `/`, `+` or `#` is refused, so a probe reads
no topic the collector did not write. The decoder is `json`; use `jq`, `yq`
or `python`.

## The target

The target is `host`, `host:port`, `mqtt://host:port` or
`mqtts://host:port`: port 1883, and 8883 for `mqtts://`, which is TLS. A
target without a scheme is TLS too when the collector has a `tls` block. A
path, a user or another scheme is refused with `400` before anything is
sent. MQTT over WebSocket is not spoken.

`mqtt://` is plaintext, the password with it, as MQTT sends it: use
`mqtts://` wherever the network is not yours.

## The document

```json
{
  "messages": [
    {"topic": "sensors/boiler/temperature", "payload": "{\"celsius\":71.5}", "json": {"celsius": 71.5}, "qos": 0, "retained": true, "age_seconds": 0.94}
  ],
  "connection": {"kept": false, "new": true, "session_present": false, "age_seconds": 1.01, "received": 1, "dropped": 0}
}
```

`payload` is the message as text, and `json` the same parsed when it is
JSON, a bare number such as `21.5` included. A payload that is not UTF-8
is in `payload_base64` in their place. `retained` says the broker sent the
message as the topic's retained one, on subscribing. `age_seconds` is how
long ago the exporter received it.

## Between scrapes

Three ways of reading, by what the collector sets.

**Neither key (the default).** Each scrape connects, subscribes, reads
for `window`, and disconnects. It sees each topic's retained message, which
the broker sends on subscribing, and whatever is published within the
window. Nothing is connected between scrapes. This suits topics whose
publishers set the retain flag, which is what the flag is for; set
`max_messages` to the number of topics and a scrape ends as soon as it has
them all.

**`persistent_session: true`**, with a `client_id` and `qos: 1`. Each
scrape still connects and disconnects, but the broker keeps the session:
what is published at QoS 1 while no scrape is connected is queued by the
broker and delivered to the next one. `messages` then holds every message
since the last scrape, in order, for a transform that counts or sums them.
The broker's queue has a limit of its own, and the session outlives the
collector: a broker keeps queueing for a `client_id` that never returns
until its session expiry clears it.

**`keep_connection: true`.** The first scrape makes the connection and
waits `window` for the first messages; the connection then stays, and the
exporter remembers the last message of each topic that comes on it, of up
to `max_messages` topics. A scrape answers at once from what is
remembered: `messages` is one entry a topic, sorted by topic, each with
`count`, how many messages of the topic came on this connection. This suits
topics published without the retain flag, more often than a scrape's
window would catch.

```yaml
request:
  type: mqtt
  topics: [telemetry/#]
  keep_connection: true
  idle_timeout: 10m
```

A kept connection is one for each collector, broker, account and set of
topics, placeholders filled; it is pinged twice in each `keep_alive`, made
again by the next scrape when it ended, and closed when no scrape used it
for `idle_timeout`, when a reload removes its collector, and at shutdown.
`connection.new` is `true` on the scrape that made it, `received` counts
its messages, and `dropped` those of topics beyond `max_messages` or
beyond `max_response_bytes`. What is remembered is in memory alone: a
restart begins again. The exporter keeps at most 256 such connections.

With several replicas of the exporter, leave `client_id` out: a broker
takes one connection of a client identifier at a time, and two replicas
with the same one cut each other off, scrape after scrape. For the same
reason `persistent_session`, which needs a fixed `client_id`, is for one
exporter.

## Static targets

A [static target](STATIC-TARGETS.md) of an `mqtt` collector is a broker,
written as a probe's target is. Its `request` may set `timeout`, and
`username`, `password_file` and `tls` for a broker with an account or a
certificate authority of its own.

## Limits

A scrape ends at its deadline, the probe's `timeout` or the scrape
timeout, and after 30 seconds when it has neither; `window` ends within
nine tenths of the time left, so keep it under the scrape timeout. The
messages of a scrape together, a single packet and the document are held
to `max_response_bytes`, and more fails the scrape as a limit exceeded.

## Errors and self-metrics

A broker that refuses the connection or a subscription fails the scrape,
saying which:

```text
CONNECT of mqtts://broker.example.com:8883: the broker refused the connection: not authorized
SUBSCRIBE of mqtts://broker.example.com:8883: the broker refused the subscription to "plant/#"; the account may not read it
```

A scrape in which no message came succeeds, with `messages` empty: make
the metric a required one, or alert on its absence. The password is in no
error, log line or debug report.

With `web.self_metrics.verbose`, a scrape is labelled with
`mqtt://host:port` or `mqtts://host:port` and `SUBSCRIBE`. A collector's
kept connections are `http_exporter_mqtt_connections`, and those it made
`http_exporter_mqtt_connects_total`; a counter that keeps rising is a
connection that does not last.

## Limitations

- MQTT 3.1.1. MQTT 5's properties, shared subscriptions and reason codes
  are not spoken; a v5 broker serves v3.1.1 clients.
- QoS 0 and 1. A subscription at QoS 2 is not made.
- Nothing is published, a last will included.
- MQTT over WebSocket is not spoken.

## Troubleshooting

- **`messages` is empty**: the topics have no retained message, and
  nothing was published within `window`. Have the publisher set the retain
  flag, or use `keep_connection`.
- **`the broker closed the connection; another client with the same
  client_id takes a connection over`**: two clients share the collector's
  `client_id`; leave it out, or give each its own.
- **`identifier rejected`**: the broker wants a client identifier of its
  own form, or shorter; set `client_id`.
- **`http_exporter_mqtt_connects_total` keeps rising**: the broker ends
  the connection; see its log. A `keep_alive` longer than the broker
  allows, or a load balancer that drops idle connections sooner than half
  of it, are the usual reasons.
