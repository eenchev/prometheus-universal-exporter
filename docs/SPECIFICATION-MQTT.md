# Prometheus Universal Exporter — `mqtt` Request Type Specification

This is the `mqtt` request type's section of § 5.1 of the
[exporter specification](SPECIFICATION-EXPORTER.md), in a document of its
own so that a change to the type is a change to this document. Everything
the request types share — the registry and the rules every type is held to
(§ 5.1), building without a type, decoding, transforms, error policies,
limits, caching, static targets and exposition — is in the exporter
specification, and a section number here, such as § 42.14, is one of its
sections. What a user reads of the type is [MQTT.md](MQTT.md).

## `mqtt`

An `mqtt` collector subscribes to topics of an MQTT 3.1.1 broker and hands
the messages that came to the transforms as a JSON document. It MUST need
nothing outside Go's standard library, MUST be opt-in
(`request_type_mqtt`), and MUST publish nothing: the only packets it sends
are CONNECT, SUBSCRIBE, PUBACK, PINGREQ and DISCONNECT.

| Key | Default | Rule |
| --- | --- | --- |
| `topics` | — | **Required.** 1 to 64 topic filters, none twice; `#` only as the whole last level, `+` only as a whole level. Each MAY hold placeholders. |
| `qos` | `0` | 0 or 1. |
| `window` | `1s` | Not negative. |
| `max_messages` | `1000` | 1 to 100000. |
| `client_id` | made for each connection | At most 256 bytes, no control character or placeholder. |
| `persistent_session` | `false` | MUST need `client_id`; not beside `keep_connection`. |
| `keep_connection` | `false` | |
| `keep_alive` | `30s` | Not negative; told to the broker in whole seconds, a part of one counted as one, and 65535 at most. |

Of the common part it takes `username`, `password_file` (only beside
`username`), `tls`, `max_response_bytes`, `allowed_targets`,
`denied_targets` and `idle_timeout` (only beside `keep_connection`, not
negative, 5m when left out, 0 never closing an idle connection); a static target's request block takes
`timeout`, `username`, `password_file` and `tls`.

- A target MUST be `host`, `host:port`, `mqtt://host:port` or
  `mqtts://host:port`, port 1883, and 8883 for `mqtts://`. `mqtts://`, and
  a target without a scheme of a collector with a `tls` block, MUST be
  TLS. Anything else MUST be refused, a probe's with `400` before anything
  is sent.
- A placeholder's value written into a topic MUST be one level: empty, or
  with a `/`, `+`, `#` or control character, it MUST fail the scrape before
  anything is sent.
- A connection MUST be made with protocol level 4, the keep alive, the
  client identifier, the collector's own or `pue-` and 16 random
  hexadecimal digits, and clean session set unless `persistent_session`.
  A CONNACK that refuses MUST fail the scrape with its code's meaning,
  a code MQTT 3.1.1 does not define not being told to a probe's caller
  (§ 19.4).
  The topics MUST then be subscribed at `qos` in one SUBSCRIBE, unless the
  broker said a session was present; a subscription the broker refuses
  MUST fail the scrape naming its topic. A message of QoS 1 MUST be
  acknowledged.
- Without `keep_connection`, a scrape MUST make its own connection, read
  until `window` has passed since it subscribed or `max_messages` have
  come, send DISCONNECT and close. The window MUST end within nine tenths
  of the time the scrape has left, and one longer than half `keep_alive`
  MUST be pinged through. `messages` MUST be every message in the
  order it came.
- With `keep_connection`, the first scrape of a collector, broker, account
  and topics MUST make the connection and wait `window`; later scrapes
  MUST answer at once from what came on it, one that finds it ended
  MUST make it again, and one that waits for another scrape to make it
  MUST give up at its own deadline. The exporter MUST keep the last message of each
  topic, of at most `max_messages` topics and the response limit in bytes
  as the document holds them,
  counting what it could not keep as `dropped`; MUST send a PINGREQ every
  half `keep_alive`, and give the connection up when the broker sent
  nothing for one and a half; MUST close it with DISCONNECT once no scrape
  has used it for `idle_timeout`, when a configuration no longer has its
  collector with `keep_connection`, and at shutdown; and MUST keep at most
  256 such connections, a scrape that would make another failing.
  `messages` MUST be sorted by topic, each with `count`, the messages of
  its topic that came.
- The answer MUST be a JSON document of `messages`, each with `topic`,
  `payload` when it is UTF-8 and `payload_base64` when not, `json` when the
  payload is JSON, `qos`, `retained` and `age_seconds`; and `connection`,
  with `kept`, `new`, `session_present`, `age_seconds`, `received` and
  `dropped`. The decoder MUST be `json`. `$tls` MUST describe a TLS
  connection's certificate.
- A packet larger than the response limit, messages that together pass it
  on a scrape's own connection, and a document that passes it MUST fail
  the scrape as a limit exceeded. The deadline MUST bound a scrape, 30
  seconds when none is set.
- The password MUST be in no error, log line, trace or debug report. The
  verbose `url` label MUST be `mqtt://host:port` or `mqtts://host:port`
  and `http_method` `SUBSCRIBE`. The connections kept and made MUST be the
  collector's `http_exporter_mqtt_connections` and
  `http_exporter_mqtt_connects_total` (§ 22).
