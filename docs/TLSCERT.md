# TLS certificates

The `tlscert` request type makes a TLS handshake with a server and hands the
certificates it presents to the transforms as a JSON document: for each, its
subject, names, issuer, serial and validity, and for the chain whether it
verifies. Use it to know before a certificate expires:

- the certificate of a web server, an API or an ingress, however it was
  issued and wherever it is installed;
- the certificate of a TLS service that speaks no HTTP — an LDAPS or SMTPS
  server, a database or a message broker on its TLS port;
- the certificate of a mail or FTP server that begins TLS when asked, with
  [STARTTLS](#starttls), on SMTP's 25 or 587, IMAP's 143, POP3's 110 or
  FTP's 21;
- a certificate nobody trusts, such as an internal one, whose expiry matters
  as much.

Nothing is sent over TLS: the handshake is the scrape, and the connection
is closed when it is over.

## A collector

```yaml
collectors:
  - name: certificates
    request:
      type: tlscert
      tls:
        ca_file: /etc/exporter/tls/internal-ca.pem
    transform:
      type: jq
    metrics:
      - name: tls_certificate_expiry_seconds
        description: Seconds until the server's certificate expires
        expression: .seconds_until_expiry
      - name: tls_certificate_expires_within_14_days
        description: Whether the server's certificate expires within 14 days
        expression: if .seconds_until_expiry < 14 * 86400 then 1 else 0 end
      - name: tls_certificate_verified
        description: Whether the chain verifies against the trusted roots
        expression: if .verified then 1 else 0 end
      - name: tls_chain_earliest_expiry_timestamp_seconds
        description: When the chain stops verifying, or its first certificate expires
        expression: .chain.earliest_not_after
      - name: tls_certificate_info
        description: The server's certificate, by its subject and issuer
        expression: "1"
        labels:
          - name: subject
            expression: .certificates[0].subject_cn
            truncate: true
          - name: issuer
            expression: .certificates[0].issuer_cn
            truncate: true
```

```text
/probe?collector=certificates&target=www.example.com:443
```

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `tlscert`. |
| `tls.ca_file` | — | Further roots the chain is verified against, besides the system's. |
| `tls.server_name` | the target's host | The name sent as SNI and the certificate is verified against, when the target is addressed by something else, such as an IP address. |
| `tls.cert_file`, `tls.key_file` | — | A client certificate, presented to a server that asks for one; both or neither. |
| `starttls` | — | `smtp`, `imap`, `pop3` or `ftp`: begin TLS on a plaintext connection as that protocol asks for it ([STARTTLS](#starttls)). |
| `allowed_targets`, `denied_targets` | — | The hosts the collector may and may not reach, as for [any type that reaches a host](REQUESTS.md#restricting-targets), checked on the connection. |

The `tls` block is the one `http`, `graphite`, `grpc`, `sql`, `redis`, `tcp`, `ldap` and,
with `api_server`, `kubernetes` take ([TLS](REQUESTS.md#tls)), read by the same
code, but for
`insecure_skip_verify`, which is refused, by the exporter and by the
configuration's schema: no certificate is refused anyway.
Its files are read at every handshake, so a Secret mounted after the
exporter starts is used, and [`--dry-run`](CONFIGURATION.md#dry-run) reads
them as a handshake would, warning of a CA file that is missing or holds no
certificate and a client certificate that does not load. No other request
key applies, and setting one is a startup error. A probe takes `timeout` and
`param_<name>` parameters for the collector's
[label values](REQUESTS.md#in-label-values); a static target's request block
takes `timeout`.

The decoder is `json`, whether `decoder.type` is left out or set to it; any
other decoder, and a transform that does not read JSON, is refused when the
configuration loads. Use `jq`, `yq` or `python`.

## The target

The target is `host:port`, `host` alone for port 443, or for the protocol's
own port with [`starttls`](#starttls), an IPv6 address in brackets:
`www.example.com`, `ldap.example.com:636`, `[2001:db8::1]:8443`.
A scheme, a path or a user is refused with `400` before anything is dialed,
a static target's when it loads.

Without `starttls` the handshake is TLS from its first byte; with it, it
follows the protocol's request for TLS ([STARTTLS](#starttls)). A server
that turns to TLS in a way of its own, as PostgreSQL does, is not read:
scrape the port where it speaks TLS at once, where it has one. The exporter
offers TLS 1.2 and 1.3, as it does to every target; a server that speaks
only an older version fails the handshake.

## STARTTLS

SMTP, IMAP, POP3 and FTP servers start in plaintext and begin TLS when the
client asks. `starttls` names the protocol: the scrape reads the greeting,
asks for TLS in the protocol's words, makes the handshake once the server
has said yes, and reads the certificates as on a TLS port. It is the
dialogue the [`tcp` type's `starttls`](TCP.md#starttls) speaks, with the
same key and values:

| `starttls` | Port of a target without one | The greeting must begin | Sent before TLS | The server's yes |
| --- | --- | --- | --- | --- |
| `smtp` | 25 | `220` | `EHLO [address]`, then `STARTTLS` once the answer, `250`, lists it | `220` |
| `imap` | 143 | `* OK` | `a1 STARTTLS` | `a1 OK`, after any untagged lines |
| `pop3` | 110 | `+OK` | `STLS` | `+OK` |
| `ftp` | 21 | `220` | `AUTH TLS` | `234` |

- A target without a port is at the port registered for the protocol, the
  table's. SMTP's is 25, where other servers deliver a domain's mail to its
  MX; write `:587` for submission, where users' clients send theirs, which
  is often another certificate.
- The greeting is read over several lines for SMTP and FTP, and must begin
  as the table says: anything else, an SMTP server's `554`, or an IMAP
  server's `* PREAUTH`, which has no `STARTTLS` to offer, fails the scrape
  before anything is sent.
- An SMTP client names itself in `EHLO`; the exporter gives its own address
  on the connection, `[10.1.2.3]` or `[IPv6:2001:db8::7]`, as RFC 5321 has
  a client without a name the server can look up do. The answer must be
  `250` and list `STARTTLS` on one of its lines, in any case.
- Nothing but the commands of the table is written, and nothing at all over
  TLS: once the handshake is over the connection is closed, without `QUIT`.
- A server that does not list `STARTTLS`, refuses it — SMTP's `454`, IMAP's
  `NO`, POP3's `-ERR`, FTP's `534` — or answers anything else fails the
  scrape in the `tls` stage, naming the step and quoting the server's
  answer, which a probe's caller is told
  [withheld](CONFIGURATION.md#what-a-failed-probes-caller-is-told), and the
  log and a debug probe's report show. The scrape never goes on in
  plaintext, and a server that writes more after its yes, before the
  handshake, fails it, since those bytes came in plaintext, where anything
  on the way could have written them.
- The greeting and each answer are held to `limits.max_response_bytes` and
  the scrape's deadline, which bounds the handshake too.
- The handshake is the one without `starttls`: the `tls` block's settings,
  `server_name` or the target's host, and a chain that does not verify
  reported, not refused. The label is `tls://host:port`, and the
  document's `starttls` names the protocol.
- `starttls` is the collector's alone: no static target or probe sets it.

The certificates of a mail server: its MX on 25 and its submission port,
587, through one collector, IMAP on 143 and POP3 on 110, each with the days
left and an alert-ready flag for one that expires within 14 days:

```yaml
  - name: smtp_certificate
    request:
      type: tlscert
      starttls: smtp
    transform:
      type: jq
    metrics:
      - name: smtp_certificate_days_left
        expression: .seconds_until_expiry / 86400 | floor
      - name: smtp_certificate_expiring
        expression: if .seconds_until_expiry < 14 * 86400 then 1 else 0 end
      - name: smtp_certificate_verified
        expression: if .verified then 1 else 0 end
  - name: imap_certificate
    request:
      type: tlscert
      starttls: imap
    transform:
      type: jq
    metrics:
      - name: imap_certificate_days_left
        expression: .seconds_until_expiry / 86400 | floor
      - name: imap_certificate_expiring
        expression: if .seconds_until_expiry < 14 * 86400 then 1 else 0 end
  - name: pop3_certificate
    request:
      type: tlscert
      starttls: pop3
    transform:
      type: jq
    metrics:
      - name: pop3_certificate_days_left
        expression: .seconds_until_expiry / 86400 | floor
      - name: pop3_certificate_expiring
        expression: if .seconds_until_expiry < 14 * 86400 then 1 else 0 end
```

```text
/probe?collector=smtp_certificate&target=mx.example.com
/probe?collector=smtp_certificate&target=mail.example.com:587
/probe?collector=imap_certificate&target=mail.example.com
/probe?collector=pop3_certificate&target=mail.example.com
```

## The document

```json
{
  "host": "www.example.com",
  "port": 443,
  "server_name": "www.example.com",
  "starttls": "",
  "tls_version": "TLS 1.3",
  "cipher_suite": "TLS_AES_128_GCM_SHA256",
  "verified": true,
  "verify_error": "",
  "duration_seconds": 0.031,
  "not_after": 1767225599,
  "seconds_until_expiry": 6220800,
  "chain": {"length": 2, "earliest_not_after": 1767225599, "seconds_until_earliest_expiry": 6220800, "earliest_from": "verified"},
  "certificates": [
    {
      "subject_cn": "www.example.com",
      "subject": "CN=www.example.com,O=Example",
      "issuer_cn": "Example Issuing CA",
      "issuer": "CN=Example Issuing CA,O=Example",
      "serial": "3f2a9c",
      "dns_names": ["www.example.com", "example.com"],
      "ip_addresses": [],
      "email_addresses": [],
      "uris": [],
      "not_before": 1759276800,
      "not_before_rfc3339": "2025-10-01T00:00:00Z",
      "not_after": 1767225599,
      "not_after_rfc3339": "2025-12-31T23:59:59Z",
      "seconds_until_expiry": 6220800,
      "is_ca": false,
      "signature_algorithm": "SHA256-RSA",
      "sha256_fingerprint": "9f86d0..."
    }
  ]
}
```

- `certificates` is the chain as the server presented it, its own
  certificate first. Each certificate's subject alternative names are
  `dns_names`, `ip_addresses`, `email_addresses` and `uris`; `serial` is
  hexadecimal, in lower case; times are Unix seconds, and RFC 3339 in UTC
  beside them; `seconds_until_expiry` is `not_after` less the time of the
  scrape, negative once the certificate has expired.
- `not_after` and `seconds_until_expiry` at the top are the first
  certificate's, the server's own, for the rule that wants nothing else.
- `chain.length` is how many certificates were presented.
  `chain.earliest_not_after` and `seconds_until_earliest_expiry` are when
  the chain stops verifying. Of a chain that verifies, they are of the
  chains it verifies by, each from the server's certificate to a trusted
  root and ending when the first of its certificates expires, the root
  included: a client trusts the server while any one of them holds, so it
  is the latest of those ends. A certificate the server sends that no
  client needs is left out, such as an expired cross-signed root (Let's
  Encrypt's servers sent DST Root CA X3 long after it expired), which would
  otherwise hold the expiry in the past and an alert on it firing for good.
  Of a chain that does not verify, they are of the first of the presented
  certificates to expire. `chain.earliest_from` says which: `verified` or
  `presented`. `certificates` is the presented chain either way.
- `verified` says whether the chain verifies as a TLS client would verify
  it: the server's certificate for `server_name`, or the target's host, the
  others as intermediates, up to the system's roots and those of
  `tls.ca_file`, at the time of the scrape. `verify_error` says why not:
  `x509: certificate signed by unknown authority`, `x509: certificate has
  expired or is not yet valid: ...`, `x509: certificate is valid for
  a.example.com, not b.example.com`.
- `tls_version` and `cipher_suite` are what the handshake settled on.
- `starttls` is the protocol whose request began TLS, `smtp`, `imap`,
  `pop3` or `ftp`, and `""` without [`starttls`](#starttls). Nothing the
  server said before TLS is in the document.
- `duration_seconds` is how long the connection and the handshake took,
  and with `starttls` the greeting and the request for TLS before it.

**A chain that does not verify does not fail the scrape.** The expiry of a
certificate nobody trusts is still worth knowing, so the document is
answered with `verified` false and the reason. What fails the scrape, in the
`tls` stage, is a handshake that does not happen: nothing listening, a
server that does not answer within the deadline, one that speaks no TLS, or
one that refuses the handshake, as one that requires a client certificate
the collector does not have may; with `starttls`, a server that does not
begin TLS ([STARTTLS](#starttls)).

## Examples

Days left on the certificate, labelled with its subject, and an alert-ready
flag for one that expires within 14 days:

```yaml
  - name: certificate_days
    request:
      type: tlscert
    transform:
      type: jq
    metrics:
      - name: tls_certificate_days_left
        expression: .seconds_until_expiry / 86400 | floor
        labels:
          - name: subject
            expression: .certificates[0].subject_cn
      - name: tls_certificate_expiring
        expression: if .seconds_until_expiry < 14 * 86400 then 1 else 0 end
```

## Static targets

A static target names `host:port`, and may set `timeout`; `starttls` is
the collector's:

```yaml
interval: 1h
targets:
  - name: www
    collector: certificates
    target: www.example.com:443
```

## Errors and self-metrics

A failed handshake is reported in the `tls` stage and follows
`error_handling.on_fetch_error`, naming the server:
`TLS handshake with www.example.com:443: ...`, or, with `starttls`, the
step that failed, such as `the server mx.example.com:25 did not begin TLS:
it answered STARTTLS with "454 4.7.0 TLS not available\r\n", not 220`. A host the target lists
refuse counts in `http_exporter_targets_refused_total`, answered `403`.
Transforms see no status (`$status` is `null`, Python's
`response.status_code` `None`) and the header
`Content-Type: application/json`.

A [debug probe](CONFIGURATION.md#debug-probes) shows the handshake in
its request section — the server, the name sent as SNI, whether a client
certificate was presented, and how it ended:

```text
Requests
  1. HANDSHAKE tls://www.example.com:443 -> TLS 1.3, 2 certificates, verified in 31ms
     Tls-Client-Certificate: no
     Tls-Server-Name: www.example.com
```

With `starttls` the section names the protocol in `Tls-Starttls`, and its
outcome shows the greeting and each command and answer, quoted, before the
handshake's:

```text
Requests
  1. HANDSHAKE tls://mail.example.com:110 -> greeted with "+OK Dovecot ready.\r\n"; sent "STLS\r\n", answered "+OK Begin TLS negotiation now.\r\n"; TLS 1.3, 2 certificates, verified in 45ms
     Tls-Client-Certificate: no
     Tls-Server-Name: mail.example.com
     Tls-Starttls: pop3
```

With [verbose self-metrics](SELF-METRICS.md#verbose-per-request-self-metrics)
a handshake is labelled `url="tls://www.example.com:443"` and
`http_method="HANDSHAKE"`.
