# SIP

The `sip` request type asks a SIP server (a proxy, a registrar or a PBX)
whether it answers, the way SIP devices ask each other. It sends one
`OPTIONS` request (RFC 3261) over UDP, TCP or TLS, and hands the response to
the transforms as a JSON document, with its status, how long it took, and
what the server says of itself. Use it to watch, from where the exporter
runs:

- that a SIP server answers, and how fast;
- which status it answers with, and that it still takes an account;
- over TLS, that its certificate verifies.

No call is placed and nothing is registered.

The type is opt-in: the default build and the published image leave it out
(see [Building with sip](#building-with-sip)).

## Building with sip

`OPT_IN_REQUEST_TYPES` lists, comma-separated, the opt-in types that a
build adds:

```sh
make build OPT_IN_REQUEST_TYPES=sip              # bin/prometheus-universal-exporter
docker build --build-arg OPT_IN_REQUEST_TYPES=sip -t exporter:sip .
go build -tags "$(sh tools/request-type-tags.sh '' '' sip)" .
```

The build tag is `request_type_sip`. A build without the type refuses a
configuration with a `sip` collector at startup, on a reload and with
`--dry-run`, and the error says how to build one that has it. The type uses
only the standard library and adds no dependency.

## A collector

```yaml
collectors:
  - name: sip_ping
    request:
      type: sip
    transform:
      type: jq
    metrics:
      - name: sip_up
        description: Whether the server answered with a success
        expression: .ok
      - name: sip_status_code
        description: The status of the response
        expression: .status_code
      - name: sip_response_seconds
        description: How long the response took
        expression: .response_seconds
```

```text
/probe?collector=sip_ping&target=pbx.example.com
```

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `sip`. |
| `sip_transport` | `udp` | `udp`, `tcp` or `tls` (see [The transport](#the-transport)). |
| `request_uri` | `sip:` and the target | The URI the request asks, and its `To`: `sip:1000@pbx.example.com` asks for a user. At most 255 bytes. |
| `from_uri` | `sip:prometheus-universal-exporter@` and the target's host | Who the request says is asking, its `From`. With an account, `username` is the user in the default. |
| `user_agent` | `prometheus-universal-exporter` | The request's `User-Agent`, at most 128 bytes. |
| `username`, `password_file` | — | The account a Digest challenge is answered with (see [Authentication](#authentication)); both or neither. The file is read at each scrape. |
| `tls` | — | With `sip_transport: tls`, how the server's certificate is verified, and a client certificate, as for [any type that speaks TLS](REQUESTS.md#tls). |
| `max_response_bytes` | `10MiB` | The most bytes of a response's body. |
| `allowed_targets`, `denied_targets` | — | The hosts the collector may and may not reach, as for [any type that reaches a host](REQUESTS.md#restricting-targets), checked on the address the request is sent to. |

No other request key applies, and setting one is a startup error. The URIs
and the account take no placeholder. A probe takes `timeout` and
`param_<name>` parameters for the collector's
[label values](REQUESTS.md#in-label-values).

The decoder is `json`, whether `decoder.type` is left out or set to it. Any
other decoder, and a transform that does not read JSON (`regex`, `csv`,
`css`, `prometheus`), is refused when the configuration loads; use `jq`,
`yq` or `python`.

## The transport

- `udp`, the default, is what most SIP is spoken over. A datagram may be
  lost, so the request is sent again while no response comes, as RFC 3261
  has a client do: after half a second, then after twice as long each time,
  up to four seconds, until the scrape's deadline.
- `tcp` sends the request once over a connection of the scrape's own.
- `tls` does the same over TLS, and the server's certificate is verified
  as the `tls` block says, against the system's authorities when it names
  none. The document names the version and the cipher suite.

## The target

The target is `host:port`, `host` alone for port 5060, or 5061 with
`sip_transport: tls`, an IPv6 address in brackets: `pbx.example.com`,
`10.0.0.1:5080`, `[2001:db8::1]:5060`. A SIP URI, a scheme, a path or a
user is refused with `400` before anything is sent, a static target's when
it loads: the user asked for is `request_uri`'s. A name is resolved when
the request is sent, and the request goes to its first address; SRV and
NAPTR records are not looked up.

## The document

```json
{
  "host": "pbx.example.com",
  "port": 5060,
  "address": "192.0.2.10:5060",
  "transport": "udp",
  "status_code": 200,
  "reason": "OK",
  "ok": true,
  "requests": 1,
  "attempts": 1,
  "response_seconds": 0.0123,
  "challenged": false,
  "authenticated": false,
  "server": "Asterisk PBX 20.5.0",
  "user_agent": "",
  "allow": ["INVITE", "ACK", "CANCEL", "OPTIONS", "BYE"],
  "accept": ["application/sdp"],
  "supported": ["replaces", "timer"],
  "headers": {"cseq": ["1 OPTIONS"], "server": ["Asterisk PBX 20.5.0"]},
  "body_bytes": 0,
  "tls_version": "",
  "tls_cipher_suite": "",
  "duration_seconds": 0.0131
}
```

- `status_code` and `reason` are the response's, and `ok` is true of a
  status from 200 to 299.
- **A response is an answer, whatever its status.** A `404` or a `405` comes
  from a server that is up and does not like the question: the scrape
  succeeds and `ok` is false, so a rule decides what counts as up. Many
  servers answer an OPTIONS for no user with `200`, and some with `404`.
  Set `request_uri` to a user that the server knows, or read
  `.status_code < 500`.
- `attempts` is how many times the last request was sent, more than once
  over UDP alone, and `response_seconds` how long after it was first sent
  its response came. `requests` is how many requests the scrape made: 2
  when it answered a challenge.
- `challenged` says the server asked for credentials, and `authenticated`
  that it took the collector's.
- `server` and `user_agent` are the response's headers of those names,
  and `allow`, `accept` and `supported` the members of those.
- `headers` is every header of the response by its name in lower case, a
  compact name written out (`i` is `call-id`), each with its values.
- `tls_version` and `tls_cipher_suite` are the connection's over TLS, and
  empty over the other transports.

## Authentication

A server that wants to know who is asking answers `401` or `407` with a
Digest challenge. With `username` and `password_file`, the scrape answers
it once: a second request, with the digest of the account's password over
the server's nonce. MD5 and SHA-256 are answered, their `-sess` forms too,
with `qop=auth` or without (RFC 8760); the strongest the server offers is
taken. The password itself is never sent.

- Without an account the `401` is the answer: `challenged` is true and the
  scrape succeeds, which is enough to know the server is up.
- With an account the server does not take, the second `401` is the
  answer, with `authenticated` false. A challenge that says only that the
  nonce went stale is answered once more.
- A challenge that cannot be answered, of a scheme other than Digest or of
  an algorithm that is not spoken, fails the scrape, saying which.

## The request and its response

```text
OPTIONS sip:pbx.example.com:5060 SIP/2.0
Via: SIP/2.0/UDP 192.0.2.50:41234;branch=z9hG4bK1f0c…;rport
Max-Forwards: 70
From: <sip:prometheus-universal-exporter@pbx.example.com>;tag=5d3a…
To: <sip:pbx.example.com:5060>
Call-ID: 8c1e…
CSeq: 1 OPTIONS
Accept: application/sdp
User-Agent: prometheus-universal-exporter
Content-Length: 0
```

The `Via` names the address the request is sent from, with `rport`, so a
server on the other side of address translation answers where the request
came from. The server must be willing to answer the exporter's address:
one that ignores requests from addresses it does not know reads here as no
response.

A message is the response only when it answers this request, by the branch
of its `Via`, its `Call-ID` and its `CSeq`. Any other message is ignored, and
so is a provisional response (`100 Trying`), while the final one is waited
for. When the deadline (the probe's `timeout`, or the scrape's own, 30
seconds when neither is set) comes first, the scrape fails, saying how many
times the request was sent, how many messages were ignored, and why the
last one was:

```text
no response from 192.0.2.10:5060 before the deadline, the request sent 7 times: context deadline exceeded
```

## Static targets

A [static target](STATIC-TARGETS.md) of a `sip` collector names the
server, `host` or `host:port`, and may set `timeout` under its `request`:

```yaml
interval: 1m
targets:
  - name: pbx_1
    collector: sip_ping
    target: pbx1.example.com
```

## Limits

A scrape is bounded by its deadline. A response's start line and headers
are held to 64 KiB and its body to the response limit,
`request.max_response_bytes` or `limits.max_response_bytes`; a body past
it fails the scrape. The document keeps at most 128 headers and 32 values
of one.

## Errors and self-metrics

A failed request is reported in the `sip` stage and follows
`error_handling.on_fetch_error`, naming the server. Causes include:

- no response before the deadline;
- a refusal that the network sent back
  (`reading the response of 192.0.2.10:5060: ... connection refused`);
- a connection closed before an answer;
- an answer over a stream that is not a SIP response;
- a TLS handshake that failed;
- a credential file that cannot be read or is empty;
- a challenge that cannot be answered.

A host that the target lists refuse counts in
`http_exporter_targets_refused_total` and is answered `403`. Transforms see
no status (`$status` is `null`, and Python's `response.status_code` is
`None`) and the header `Content-Type: application/json`; the response's
status is the document's `status_code`.

A [debug probe](CONFIGURATION.md#debug-probes) shows each request with how
it ended:

```text
Requests
  1. OPTIONS sip://pbx.example.com:5060 -> 401 Unauthorized, sent once in 9ms
     Sip-Transport: udp
  2. OPTIONS sip://pbx.example.com:5060 -> 200 OK, sent once in 11ms
     Sip-Transport: udp
     Authorization: Digest
```

With [verbose self-metrics](SELF-METRICS.md#verbose-per-request-self-metrics)
a request is labelled `url="sip://pbx.example.com:5060"`, `sips://` over
TLS, and `http_method="OPTIONS"`.

## Limitations

- **OPTIONS only.** No INVITE, so no call is placed and no media is
  tested, and no REGISTER, so the account is not registered: it only
  answers a challenge to the OPTIONS.
- **The address is the target's.** SRV and NAPTR records are not looked up,
  and a response is read on the socket that the request left from, so a
  server that answers from another address is not heard over UDP.
- **Digest alone.** MD5 and SHA-256 with `qop=auth` or none; `auth-int`
  alone, AKA and other schemes are not answered.
- **No WebSocket or SCTP transport.**

## Troubleshooting

- **No response, nothing ignored.** A firewall drops UDP 5060, the server
  listens on another port or transport, or it ignores requests from
  addresses it does not know. Try `sip_transport: tcp`.
- **`ok` is false with `404`, `405` or `403`.** The server is up and
  refuses the question. Ask for a user it knows with `request_uri`, or
  let the rule take the status as up.
- **`challenged` is true and `authenticated` false with an account.** The
  server did not take the credentials: the password, the user, or a realm
  the account is not of.
- **`TLS handshake with …` fails.** The certificate does not verify
  against `tls.ca_file` or the system's authorities, or is not for the
  target's name; `tls.server_name` names the one to verify.
