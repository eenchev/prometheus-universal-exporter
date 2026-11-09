# Prometheus Universal Exporter — `tlscert` Request Type Specification

This is the `tlscert` request type's section of § 5.1 of the
[exporter specification](SPECIFICATION-EXPORTER.md), in a document of its
own so that a change to the type is a change to this document. Everything
the request types share — the registry and the rules every type is held to
(§ 5.1), building without a type, decoding, transforms, error policies,
limits, caching, static targets and exposition — is in the exporter
specification, and a section number here, such as § 42.14, is one of its
sections. What a user reads of the type is [TLSCERT.md](TLSCERT.md).

## `tlscert`

A `tlscert` collector makes a TLS handshake with the probe's `target` and
hands the certificates the server presented to the transforms as a JSON
document. Nothing MUST be sent over TLS, and nothing in plaintext but the
request for TLS of `starttls`; the connection MUST be closed after the
handshake. Its keys are `tls`, `starttls` and `allowed_targets` /
`denied_targets`:

| Key | Default | Rule |
| --- | --- | --- |
| `tls` | none | `http`'s block, read by the same code: `ca_file` adds roots to the system's, `cert_file` and `key_file` (both or neither, refused at load otherwise) a client certificate, `server_name` the name sent as SNI and verified, the target's host when unset. `insecure_skip_verify` true MUST be refused at load, and by the schema (§ 24.3). The files MUST be read at every handshake, and by `--dry-run`, which MUST warn of one a handshake would fail on. |
| `starttls` | none | `tcp`'s key, one key of the common part with one meaning: `smtp`, `imap`, `pop3` or `ftp`, exactly, anything else refused at load naming the four; the configuration's alone, which no static target or probe MAY set. |
| `allowed_targets`, `denied_targets` | none | As for `http`: the host's name before the connection, and the address the connection is made to. |

- A target MUST be `host`, `host:port` or `[v6]:port`, port 443 when left
  out, or with `starttls` the port registered for the protocol: 25 for
  `smtp`, 143 for `imap`, 110 for `pop3`, 21 for `ftp`; a scheme, path, query or user and a port outside 1–65535 MUST be
  refused, a probe's with `400` before anything is dialed and a static
  target's at load. A probe MUST take `timeout` and `param_<name>`
  parameters, and a static target's request block `timeout`.
- The handshake MUST take whatever chain the server presents, and the
  chain MUST then be verified as a TLS client would: the first certificate
  for `server_name`, the others as intermediates, up to the system's roots
  and `ca_file`'s, at the time of the scrape. A chain that does not verify
  MUST NOT fail the scrape: the document MUST say `verified` false with the
  error's text as `verify_error`. A connection that fails, a handshake that
  fails and a deadline that comes first MUST fail the scrape in the `tls`
  stage, naming `host:port`.
- The answer MUST be a JSON document of `host`, `port`, `server_name`,
  `starttls` (the protocol, `""` without it), `tls_version` and `cipher_suite` as the handshake settled them,
  `verified`, `verify_error`, `duration_seconds`, the first certificate's
  `not_after` and `seconds_until_expiry`, `chain` (`length`,
  `earliest_not_after`, `seconds_until_earliest_expiry`, `earliest_from`)
  and `certificates`, the chain as presented, each with `subject_cn`,
  `subject`, `issuer_cn`, `issuer`, `serial` (lower-case hexadecimal),
  `dns_names`, `ip_addresses`, `email_addresses`, `uris`, `not_before` and
  `not_after` in Unix seconds and as RFC 3339 in UTC (`_rfc3339`),
  `seconds_until_expiry` (negative once expired), `is_ca`,
  `signature_algorithm` and `sha256_fingerprint`; and `ocsp`, of the OCSP
  response the server stapled, which the handshake MUST ask for.
- `ocsp` MUST hold `stapled`, whether one was; `status`, `good`, `revoked`
  or `unknown`, `""` without one or of one that cannot be read; `verified`,
  whether `golang.org/x/crypto/ocsp` checked its signature against the
  server certificate's issuer — the certificate after it in the first
  chain it verified by, or as presented when it verified by none — or a
  responder that issuer delegated to, and that it is of the server's
  certificate; `error`, why not, or why it could not be read, a response
  whose signature does not check read for its status and times all the
  same; `produced_at`, `this_update` and `next_update` in Unix seconds,
  `next_update` 0 when the responder names none, and
  `seconds_until_next_update`, `next_update` less the time of the scrape;
  and of a revoked certificate `revoked_at` and `revocation_reason`, RFC
  5280's name of the reason (`keyCompromise`). A staple MUST NOT fail the
  scrape, whatever it is.
- `chain.length` MUST be the number of certificates presented. When the
  chain verifies, `chain.earliest_not_after` MUST be, over the chains the
  verification built from the server's certificate to a trusted root, the
  latest of each chain's earliest `not_after`, its root included, and
  `earliest_from` MUST be `verified`; a presented certificate on none of
  those chains, such as an expired cross-signed root, MUST NOT count. When
  it does not verify, `earliest_not_after` MUST be the earliest `not_after`
  of the presented certificates and `earliest_from` MUST be `presented`.
  `seconds_until_earliest_expiry` MUST be `earliest_not_after` less the
  time of the scrape. `verified`, `verify_error` and `certificates` MUST be
  of the presented chain either way.
- Without `starttls` the handshake MUST start at the connection's first
  byte. With it the scrape MUST speak the request for TLS `tcp`'s
  `starttls` speaks ([`tcp`](SPECIFICATION-TCP.md)), by the same code: read the greeting, which
  MUST begin `220` (`smtp`, `ftp`), `* OK` in any case (`imap`) or `+OK`
  (`pop3`), then write only the protocol's request for TLS, `EHLO` naming
  the client by the connection's local address as an address literal and,
  when its `250` answer lists `STARTTLS`, `STARTTLS`, answered `220`;
  `a1 STARTTLS`, answered `a1 OK`, the tag in any case; `STLS`, answered
  `+OK`; `AUTH TLS`, answered `234`, after the `120` replies an FTP server
  may greet with before its `220` ([`tcp`](SPECIFICATION-TCP.md)). The greeting and each
  answer MUST be held to
  `limits.max_response_bytes`, a reply past it failing as a limit exceeded
  that names that key, and with the dialogue's reads and writes to the
  scrape's deadline and cancellation. A greeting that does not begin so, a
  server that does not list `STARTTLS`, answers otherwise, closes the
  connection or writes any byte after its yes before the handshake MUST
  fail the scrape in the `tls` stage, naming the step and, where the
  server answered, quoting it with `model.Quoted`, so a probe's caller is
  told it withheld (§ 19.4); the scrape MUST NOT go on without TLS. The
  handshake and the document MUST then be those without `starttls`, the
  document's `starttls` naming the protocol; nothing the server said
  before TLS MAY be in the document.
- The decoder MUST be `json`, as for `grpc`; another decoder, and a
  transform other than `jq`, `yq` and `python`, MUST be refused at load.
- A debug probe's request section MUST show `HANDSHAKE`, `tls://host:port`,
  the name sent as SNI, whether a client certificate is presented and, with
  `starttls`, the protocol as `Tls-Starttls`, with the version, the number
  of certificates and whether the chain verified, or the error, as its
  outcome, which with `starttls` MUST begin with the greeting and each
  command sent and the server's answer, quoted, before the handshake's
  outcome or its error. The verbose `url` label MUST be
  `tls://host:port` and `http_method` MUST be `HANDSHAKE`.
