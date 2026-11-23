# RADIUS

The `radius` request type asks a RADIUS server (RFC 2865) to authenticate
an account, as a VPN gateway or a wireless controller does, and hands the
server's reply to the transforms as a JSON document: whether the account
was accepted, how long the reply took, and how many times the request had
to be sent. Use it to watch, from where the exporter runs:

- that a RADIUS server answers, and how fast;
- that a login works end to end, through the server and the directory
  behind it, with an account kept for the purpose;
- that a server is alive without any account, with `mode: status-server`.

The type is opt-in: the default build and the published image leave it out,
so a build carries it only when asked
([Building with radius](#building-with-radius)).

## Building with radius

`OPT_IN_REQUEST_TYPES` names the opt-in types a build adds to the types it
carries, comma-separated:

```sh
make build OPT_IN_REQUEST_TYPES=radius              # bin/prometheus-universal-exporter
docker build --build-arg OPT_IN_REQUEST_TYPES=radius -t exporter:radius .
go build -tags "$(sh tools/request-type-tags.sh '' '' radius)" .
```

The tag is `request_type_radius`. A configuration with a `radius` collector
is refused by a build without the type, at startup, on a reload and by
`--dry-run`, saying how to build one that has it.

## A collector

```yaml
collectors:
  - name: radius_login
    request:
      type: radius
      secret_file: /etc/exporter/radius-secret
      username: monitor
      password_file: /etc/exporter/radius-password
    transform:
      type: jq
    metrics:
      - name: radius_login_accepted
        description: Whether the server accepted the account
        expression: .accepted
      - name: radius_response_seconds
        description: How long after the first request the reply came
        expression: .response_seconds
      - name: radius_attempts
        description: How many times the request was sent
        expression: .attempts
```

```text
/probe?collector=radius_login&target=radius.example.com
```

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `radius`. |
| `secret_file` | — | **Required.** The file that holds the secret the server shares with this client, read at each scrape. |
| `mode` | `pap` | `pap`, `chap` or `status-server` (see [The mode](#the-mode)). |
| `username`, `password_file` | — | The account, required unless the mode is `status-server`, which takes neither. The file is read at each scrape. |
| `nas_identifier` | `prometheus-universal-exporter` | The NAS-Identifier the request names its sender by, at most 253 bytes. |
| `resends` | `2` | How many more times the request is sent while no reply comes, from 0 to 5. |
| `resend_interval` | `2s` | How long a reply is waited for before the request is sent again, at least `100ms`. |
| `require_message_authenticator` | `false` | Fail the scrape when the reply carries no Message-Authenticator. |
| `allowed_targets`, `denied_targets` | — | The hosts the collector may and may not reach, as for [any type that reaches a host](REQUESTS.md#restricting-targets), checked on the address the request is sent to. |

No other request key applies, and setting one is a startup error. The
account and the secret take no placeholder. A probe takes `timeout` and
`param_<name>` parameters for the collector's
[label values](REQUESTS.md#in-label-values).

The server must know the exporter as a client: add the address the exporter
sends from to the server's clients, with the secret of `secret_file`. A
server ignores a request from a client it does not know, which reads here as
no reply.

The decoder is `json`, whether `decoder.type` is left out or set to it; any
other decoder, and a transform that does not read JSON, is refused when the
configuration loads. Use `jq`, `yq` or `python`.

## The mode

- `pap` sends the password hidden with the shared secret (User-Password),
  which the server recovers and checks against whatever it keeps: a file,
  a directory, a hash. It works with every backend, and is the default. A
  password is at most 128 bytes.
- `chap` sends a digest of the password over a challenge (CHAP-Password),
  so the server must keep the password itself to check it.
- `status-server` sends a Status-Server (RFC 5997), which asks only
  whether the server is alive. It needs no account and leaves no failed
  login in the server's log; a server that does not support it does not
  answer.

EAP, which 802.1X uses, and MS-CHAP are not spoken.

## The target

The target is `host:port`, `host` alone for port 1812, an IPv6 address in
brackets: `radius.example.com`, `10.0.0.1:1812`, `[2001:db8::1]:1812`. A
scheme, a path or a user is refused with `400` before anything is sent, a
static target's when it loads. A name is resolved when the request is sent,
and the request goes to its first address, over UDP.

## The document

```json
{
  "host": "radius.example.com",
  "port": 1812,
  "address": "192.0.2.10:1812",
  "mode": "pap",
  "code": 2,
  "reply": "access-accept",
  "accepted": true,
  "attempts": 1,
  "response_seconds": 0.0041,
  "reply_messages": [],
  "message_authenticator": true,
  "session_timeout_seconds": 3600,
  "attribute_types": [80, 27],
  "duration_seconds": 0.0052
}
```

- `reply` is `access-accept`, `access-reject` or `access-challenge`, and
  `code` its number; `accepted` is true of an Access-Accept alone.
- **A reply is an answer, whatever it says.** An Access-Reject is a server
  that is up and refused the account: the scrape succeeds and `accepted` is
  false, so a rule decides what a refusal means, and an account that should
  be refused can be watched too.
- `attempts` is how many times the request was sent, and
  `response_seconds` how long after the first the reply came.
- `reply_messages` is the text the server sent to be shown to the user,
  `session_timeout_seconds` the session it grants, `null` when it names
  none, and `attribute_types` the types of the reply's attributes, in
  order.
- `message_authenticator` says the reply carried a Message-Authenticator.

## The request and its reply

Every request carries a Message-Authenticator (RFC 3579), its first
attribute, which servers patched against forged replies (CVE-2024-3596)
require. A request no reply comes to within `resend_interval` is sent again,
the same packet, `resends` times; the last waits until the scrape's
deadline, the probe's `timeout` or the scrape's own, 30 seconds when
neither is set.

A datagram is the reply only when it answers the request's identifier and
its Response Authenticator is what the shared secret makes, and its
Message-Authenticator too when it has one. Any other datagram is ignored,
as one from elsewhere would be, and the reply waited for. When the deadline
comes first, the scrape fails saying how many times the request was sent,
how many datagrams were ignored and why the last one was:

```text
no reply from 192.0.2.10:1812 before the deadline, the request sent 3 times; 1 datagram was ignored, as its Response Authenticator is not what the shared secret makes, so it is not this server's reply, or request.secret_file is not the secret the server has for this client: context deadline exceeded
```

With `require_message_authenticator: true` a reply that verifies and has no
Message-Authenticator fails the scrape, naming the key. Set it for servers
known to send one.

## Static targets

A [static target](STATIC-TARGETS.md) of a `radius` collector names the
server, `host` or `host:port`, and may set `timeout` under its `request`:

```yaml
interval: 1m
targets:
  - name: radius-1
    collector: radius_login
    target: radius1.example.com
```

## Limits

A scrape is bounded by its deadline. A reply is one datagram, of at most
4096 bytes, so no response limit applies.

## Errors and self-metrics

A failed request is reported in the `radius` stage and follows
`error_handling.on_fetch_error`, naming the server: no reply before the
deadline, a refusal the network sent back
(`reading the reply from 192.0.2.10:1812: ... connection refused`), a
credential file that cannot be read or is empty, or a reply without the
Message-Authenticator the collector requires. A host the target lists refuse
counts in `http_exporter_targets_refused_total`, answered `403`. Transforms
see no status (`$status` is `null`, Python's `response.status_code` `None`)
and the header `Content-Type: application/json`.

A [debug probe](CONFIGURATION.md#debug-probes) shows the request with how it
ended:

```text
Requests
  1. ACCESS-REQUEST radius://radius.example.com:1812 -> access-accept after 1 of 3 sent in 4ms
     Radius-Mode: pap
```

With [verbose self-metrics](SELF-METRICS.md#verbose-per-request-self-metrics)
a request is labelled `url="radius://radius.example.com:1812"` and
`http_method="ACCESS-REQUEST"`, or `STATUS-SERVER`.

## Limitations

- **PAP, CHAP and Status-Server only.** EAP, MS-CHAP and a reply to an
  Access-Challenge are not spoken: a challenge is reported as the reply it
  is.
- **UDP only.** RADIUS over TCP or TLS (RadSec) is not spoken.
- **No accounting.** No Accounting-Request is sent.
- **PAP hides the password with MD5 and the shared secret**, as the
  protocol defines; use an account that can do nothing but log in, and a
  long random secret.

## Troubleshooting

- **No reply, nothing ignored.** The server does not know the exporter's
  address as a client, a firewall drops UDP 1812, or, with
  `mode: status-server`, the server does not support Status-Server.
- **No reply, a datagram ignored for its Response Authenticator.** The
  server answered, with another secret: `secret_file` is not the secret
  the server has for this client.
- **`accepted` is false.** The server refused the account; `reply_messages`
  may say why, and the server's log does. With a wrong shared secret a
  server cannot recover a PAP password and rejects it too, or ignores the
  request for its Message-Authenticator.
