# WebSocket

The `websocket` request type reads messages from a WebSocket endpoint: a
scrape makes the opening handshake, sends one text message when the
collector has one — a subscription, a query — reads the messages the
collector asks for, and closes. Use it for services that publish their
state on a socket and have no HTTP endpoint for it: a status feed, a
ticker, a GraphQL subscription's first result.

It stays a pull: a scrape connects, reads and closes, and no connection
outlives it.

The type is opt-in: the default build and the published image leave it out,
so a build carries it only when asked
([Building with websocket](#building-with-websocket)).

## Building with websocket

`OPT_IN_REQUEST_TYPES` names the opt-in types a build adds to the types it
carries, comma-separated:

```sh
make build OPT_IN_REQUEST_TYPES=websocket              # bin/prometheus-universal-exporter
docker build --build-arg OPT_IN_REQUEST_TYPES=websocket -t exporter:websocket .
go build -tags "$(sh tools/request-type-tags.sh '' '' websocket)" .
```

The tag is `request_type_websocket`. A configuration with an `websocket` collector
is refused by a build without the type, at startup, on a reload and by
`--dry-run`, saying how to build one that has it. The type is written with
the standard library alone, and adds no dependency.

## A collector

```yaml
collectors:
  - name: queue
    request:
      type: websocket
      send_text: '{"op":"stats","queue":{{param_queue|json}}}'
      bearer_token_file: /etc/exporter/feed-token
    decoder:
      type: json
    transform:
      type: jq
    metrics:
      - name: queue_depth
        expression: .depth
        labels:
          - name: queue
            value: "{{param_queue}}"
```

```text
/probe?collector=queue&param_queue=orders&target=wss://feed.example.com/ws
```

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `websocket`. |
| `send_text` | — | A text message sent once the handshake is made. May contain `{{param_name}}` placeholders, written with a filter as an [http body's](REQUESTS.md#in-the-body-headers-and-query) are: `\|json`, `\|number`, `\|form`, `\|xml` or `\|raw`. |
| `read_messages` | `1` | How many messages are read (see [The answer](#the-answer)). |
| `read_window` | `5s` | How long they are waited for. |
| `subprotocols` | — | The subprotocols offered, such as `[graphql-transport-ws]`. |
| `headers` | — | Headers of the handshake; values may contain placeholders. `Host`, `Upgrade`, `Connection` and the `Sec-WebSocket-` headers are the handshake's own. |
| `bearer_token_file` | — | A token sent as `Authorization: Bearer`, read at each scrape. |
| `tls` | — | How the server's certificate is verified, and a client certificate, as for [any type that speaks TLS](REQUESTS.md#tls). |
| `max_response_bytes` | the collector's limit | The most the messages read may be, together. |
| `allowed_targets`, `denied_targets` | — | The hosts the collector may and may not reach, as for [any type that reaches a host](REQUESTS.md#restricting-targets). |

A probe takes `timeout` and `param_<name>` parameters, which fill the
placeholders of `send_text`, the header values and the collector's
[label values](REQUESTS.md#in-label-values).

## The target

The target is the endpoint's URL: `ws://host:port/path?query` or `wss://`,
ports 80 and 443. A target without a scheme is `ws://`, or `wss://` when
the collector has a `tls` block. A user, a fragment or another scheme is
refused with `400` before anything is sent. Logs and labels show the URL
without its query, which may hold a token.

## The answer

With `read_messages: 1`, the default, the answer is the first message, as
the server sent it, for whatever decoder reads it: JSON for `jq`, text for
`regex`, the Prometheus text format for `prometheus`. Set `decoder.type`
when the content does not tell. No message within `read_window` fails the
scrape.

With more, the answer is a JSON list of the messages that came before the
window passed or the server closed, fewer than asked or none included:

```json
[
  {"payload": "{\"price\":101.5}", "json": {"price": 101.5}, "binary": false},
  {"payload_base64": "AAE=", "binary": true}
]
```

`payload` is a text message, `json` the same parsed when it is JSON, and
`payload_base64` a binary one. The decoder is then `json`.

A server that greets with a message before it answers, as many
subscription protocols do, needs `read_messages` to count the greeting:
read two, and take `.[1].json`.

## Static targets

A [static target](STATIC-TARGETS.md) of a `websocket` collector is an
endpoint's URL, written as a probe's target is. Its `request` may set
`timeout`, and `tls` for a server with a certificate authority of its own.

## Limits

A scrape ends at its deadline, the probe's `timeout` or the scrape
timeout, and after 30 seconds when it has neither; `read_window` ends
within nine tenths of the time left. The messages read are held to
`max_response_bytes` together, and more fails the scrape as a limit
exceeded.

## Errors and self-metrics

The scrape fails when the handshake is not answered with a 101 as RFC 6455
has it, naming the status the server gave; when the server chooses a
subprotocol that was not offered; and when it sends a frame the protocol
does not allow.

```text
the handshake with wss://feed.example.com:443/ws: the server answered HTTP status 401, not 101: it does not serve a WebSocket there, or refuses this client
```

With `web.self_metrics.verbose`, a scrape is labelled with the endpoint's
URL without its query, and `SEND` or `READ`.

## Limitations

- One message is sent, as text. A protocol that needs an exchange of
  several, such as an acknowledgement before the subscription, is beyond
  it.
- No extension is offered, so no message is compressed
  (`permessage-deflate`).
- A redirect in answer to the handshake is not followed, and no proxy is
  used.
- `basic_auth` and `login` are not taken; send a header.

## Troubleshooting

- **`no message came ... within request.read_window`**: the server sends
  only when asked; set `send_text`, or a longer window.
- **`HTTP status 400` or `403`**: many servers check the `Origin` header;
  set it in `headers`.
- **`HTTP status 200`**: the path serves a page, not a socket; check the
  target's path.
