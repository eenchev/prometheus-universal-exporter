# Prometheus Universal Exporter — `sip` Request Type Specification

This is the `sip` request type's section of § 5.1 of the
[exporter specification](SPECIFICATION-EXPORTER.md), in a document of its
own so that a change to the type is a change to this document. Everything
the request types share — the registry and the rules every type is held to
(§ 5.1), building without a type, decoding, transforms, error policies,
limits, caching, static targets and exposition — is in the exporter
specification, and a section number here, such as § 42.14, is one of its
sections. What a user reads of the type is [SIP.md](SIP.md).

## `sip`

A `sip` collector asks a SIP server (RFC 3261) with an OPTIONS request
whether it answers, and hands the response to the transforms as a JSON
document. It MUST need nothing outside Go's standard library, and MUST be
opt-in, built only under the tag `request_type_sip`.

| Key | Default | Rule |
| --- | --- | --- |
| `sip_transport` | `udp` | `udp`, `tcp` or `tls`, in lower case. |
| `request_uri` | `sip:` and the target's `host:port` | A `sip:` or `sips:` URI of at most 255 bytes, without a blank, a control character, a quote, an angle bracket or a placeholder. |
| `from_uri` | `sip:prometheus-universal-exporter@` and the target's host, `username` in the user's place when it is set | As `request_uri`. |
| `user_agent` | `prometheus-universal-exporter` | At most 128 bytes, no control character. |
| `username`, `password_file` | — | Both or neither; no placeholder; a username without a quote or a backslash. |
| `tls` | — | With `sip_transport: tls` alone, as the other opt-in types that speak TLS take it; refused at load with another transport. |
| `max_response_bytes` | § 20 | The most bytes of a response's body. |
| `allowed_targets`, `denied_targets` | — | As for `http`, on the host's name before the request and on the address it is sent to (§ 26.1). |

- Any other request key MUST be refused at load. A probe MUST take
  `timeout` and `param_<name>` parameters, and a static target's request
  block `timeout`.
- A target MUST be `host`, `host:port` or `[v6]:port`, port 5060 when it
  names none and 5061 with `sip_transport: tls`. A scheme, a path, a query
  or a user MUST be refused, a probe's with `400` before anything is sent
  and a static target's at load.
- A scrape MUST send an OPTIONS request for `request_uri`, with `To` the
  same URI, `From` `from_uri` with a tag, a `Via` of the transport and the
  address it is sent from with a branch that starts `z9hG4bK` and `rport`,
  `Max-Forwards: 70`, a `Call-ID`, `CSeq: 1 OPTIONS`, `User-Agent` and
  `Content-Length: 0`. The branch, the tag and the Call-ID MUST be random.
- Over `udp` the request MUST be sent again, the same datagram, while no
  response comes: after 500 ms, then after twice the last wait, up to 4 s,
  until the deadline. Over `tcp` and `tls` it MUST be sent once. Over
  `tls` the server's certificate MUST be verified as `tls` says before
  anything is sent.
- A message MUST be taken as the response only when the branch of its
  topmost `Via`, its `Call-ID` and its `CSeq` are the request's and its
  status is 200 or more; any other, a provisional response and a datagram
  that is no SIP message among them, MUST be passed over. Header names
  MUST be read in any case, the compact forms of RFC 3261 § 7.3.3 as the
  names they stand for, and a line that starts with a blank as the
  continuation of the header before it. Over a stream a body MUST be read
  by its `Content-Length`, which a message there MUST have, and blank
  lines before a message MUST be passed over.
- A final response MUST be the answer whatever its status, and MUST NOT
  fail the scrape, but for a challenge that is answered.
- A `401` or a `407` MUST be answered once, with a second request under
  the same `Call-ID` and tag, a new branch and the next `CSeq`, when the
  collector has an account: with the Digest credentials (RFC 7616, RFC
  8760) of the strongest challenge among `SHA-256`, `SHA-256-sess`, `MD5`
  and `MD5-sess` that has a nonce and `qop` `auth` or none, in
  `Authorization` to a `401` and `Proxy-Authorization` to a `407`. A
  challenge to those credentials that says `stale=true` MUST be answered
  once more, and a scrape MUST make at most three requests. Without an
  account the challenge MUST be the answer; so MUST a second one that is
  not stale. A challenge with no Digest that can be answered MUST fail the
  scrape saying so. The password MUST NOT be sent, logged or shown.
- The scrape's deadline — the probe's `timeout`, the scrape's own, 30
  seconds when neither is set — MUST bound all its requests. When it comes
  first, the failure MUST wrap the deadline's error and say how many times
  the request was sent, how many messages were ignored and why the last
  was.
- The answer MUST be a JSON document of `host`, `port`, `address`, the
  address the request went to, `transport`, `status_code`, `reason`, `ok`,
  true of a status from 200 to 299, `requests`, `attempts`, the times the
  last request was sent, `response_seconds`, from its first sending to its
  response, `challenged`, `authenticated`, true when the final response is
  no challenge and its request carried credentials, `server`,
  `user_agent`, `allow`, `accept` and `supported`, the members of those
  headers, `headers`, every header by its name in lower case, at most 128
  of them and 32 values of one, `body_bytes`, `tls_version` and
  `tls_cipher_suite`, empty unless over TLS, and `duration_seconds`.
- A response's start line and headers past 64 KiB, and over a stream what
  is no SIP response, MUST fail the scrape; a body past the response limit
  MUST fail it wrapping the limit's error (§ 20).
- The decoder MUST be `json`, as for `grpc`; another decoder, and a
  transform other than `jq`, `yq` and `python`, MUST be refused at load.
- A failure MUST be reported in the `sip` stage. Each request MUST be a
  request of a debug report and of an exported trace, shown as `OPTIONS`
  and `sip://host:port`, `sips://` over TLS, with the status and how often
  it was sent, or the error, as its outcome, and never its credentials.
  The verbose `url` label MUST be the same URL, and `http_method`
  `OPTIONS`.
