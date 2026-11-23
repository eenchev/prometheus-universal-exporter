# Prometheus Universal Exporter — `radius` Request Type Specification

This is the `radius` request type's section of § 5.1 of the
[exporter specification](SPECIFICATION-EXPORTER.md), in a document of its
own so that a change to the type is a change to this document. Everything
the request types share — the registry and the rules every type is held to
(§ 5.1), building without a type, decoding, transforms, error policies,
limits, caching, static targets and exposition — is in the exporter
specification, and a section number here, such as § 42.14, is one of its
sections. What a user reads of the type is [RADIUS.md](RADIUS.md).

## `radius`

A `radius` collector sends one RADIUS request (RFC 2865) to the probe's
`target` over UDP and hands the server's reply to the transforms as a JSON
document. It MUST need nothing outside Go's standard library, and MUST be
opt-in, built only under the tag `request_type_radius`.

| Key | Default | Rule |
| --- | --- | --- |
| `secret_file` | — | Required of a request that is made. The shared secret, read at each scrape; an unreadable or empty file MUST fail the scrape. |
| `mode` | `pap` | `pap`, `chap` or `status-server`; anything else refused at load. |
| `username`, `password_file` | — | Required unless `mode` is `status-server`, which MUST refuse both at load. The file is read at each scrape; an unreadable or empty one, and with `pap` a password past 128 bytes, MUST fail the scrape. |
| `nas_identifier` | `prometheus-universal-exporter` | At most 253 bytes, no control character. |
| `resends` | `2` | 0 to 5. |
| `resend_interval` | `2s` | At least 100ms. |
| `require_message_authenticator` | `false` | |
| `allowed_targets`, `denied_targets` | — | As for `http`, the host's name before the request and the address the UDP socket is connected to before anything is sent (§ 26.1). |

- Any other request key, and a placeholder in `secret_file`, `username` or
  `password_file`, MUST be refused at load. A probe MUST take `timeout` and
  `param_<name>` parameters, and a static target's request block `timeout`.
- A target MUST be `host`, `host:port` or `[v6]:port`, port 1812 when left
  out. A scheme, path, query or user, an IPv6 address without brackets and
  a port outside 1–65535 MUST be refused, a probe's with `400` before
  anything is sent and a static target's at load.
- The request MUST be an Access-Request, or with `mode: status-server` a
  Status-Server (RFC 5997), with a random identifier and Request
  Authenticator. Its first attribute MUST be a Message-Authenticator
  (RFC 3579), HMAC-MD5 of the packet under the shared secret; it MUST carry
  the NAS-Identifier; and an Access-Request MUST carry the User-Name and,
  with `pap`, the User-Password hidden as RFC 2865 § 5.2 has it, or with
  `chap` a CHAP-Password whose challenge is the Request Authenticator. A
  Status-Server MUST carry no account.
- While no reply comes, the request MUST be sent again every
  `resend_interval`, `resends` times at most, each time the same packet;
  the last MUST wait until the scrape's deadline — the probe's `timeout`,
  the scrape's own, 30 seconds when neither is set.
- A datagram MUST be taken as the reply only when it comes from the address
  the request was sent to, its length fits, it answers the request's
  identifier, its code is Access-Accept, Access-Reject, Access-Challenge or
  Accounting-Response, its Response Authenticator is the MD5 the shared
  secret makes of it, and its Message-Authenticator, when it has one,
  verifies likewise. Any other datagram MUST be ignored and the reply
  waited for; when the deadline comes first, the scrape MUST fail wrapping
  `context.DeadlineExceeded`, saying how many times the request was sent,
  how many datagrams were ignored and why the last one was. A scrape
  cancelled before its deadline MUST fail wrapping `context.Canceled`,
  saying `before the scrape was cancelled`. An error of the socket MUST
  fail it at once.
- With `require_message_authenticator` a reply that verifies and carries no
  Message-Authenticator MUST fail the scrape, naming the key.
- A verified reply MUST be an answer whatever its code: an Access-Reject
  MUST NOT fail the scrape.
- The answer MUST be a JSON document of `host`, `port`, `address` (the
  address the request was sent to), `mode`, `code`, `reply`
  (`access-accept`, `access-reject`, `access-challenge` or
  `accounting-response`), `accepted` (true of an Access-Accept alone, and of a
  Status-Server's Accounting-Response),
  `attempts` (how many times the request was sent), `response_seconds`
  (from the first request sent to the reply), `reply_messages` (each
  Reply-Message, in order), `message_authenticator` (whether the reply
  carried one), `session_timeout_seconds` (the Session-Timeout, `null`
  without one), `attribute_types` (the types of the reply's attributes, in
  order) and `duration_seconds`. Nothing of the secret or the password MUST
  be in it, in a failure or in a debug report.
- The decoder MUST be `json`, as for `grpc`; another decoder, and a
  transform other than `jq`, `yq` and `python`, MUST be refused at load.
- A failure MUST be reported in the `radius` stage. A debug probe's request
  section MUST show the packet sent, `radius://host:port` and the mode,
  with the reply and how many times the request was sent, or the error, as
  its outcome. The verbose `url` label MUST be `radius://host:port`, and
  `http_method` MUST be `ACCESS-REQUEST`, or `STATUS-SERVER` for that mode.
- `--dry-run` MUST warn of a secret or password file a scrape would fail
  on.
