# Prometheus Universal Exporter — `dns` Request Type Specification

This is the `dns` request type's section of § 5.1 of the
[exporter specification](SPECIFICATION-EXPORTER.md), in a document of its
own so that a change to the type is a change to this document. Everything
the request types share — the registry and the rules every type is held to
(§ 5.1), building without a type, decoding, transforms, error policies,
limits, caching, static targets and exposition — is in the exporter
specification, and a section number here, such as § 42.14, is one of its
sections. What a user reads of the type is [DNS.md](DNS.md).

## `dns`

A `dns` collector resolves the probe's `target`, a DNS name, and hands its
records to the transforms as a JSON document. The question MUST be the
exporter's own, sent with a DNS client that imports the standard library
alone, not a resolver's lookup, so that the answer's TTLs, response code and
flags are known. Its keys:

| Key | Default | Rule |
| --- | --- | --- |
| `record_type` | `A` | One of `A`, `AAAA`, `CNAME`, `MX`, `NS`, `TXT`, `SRV`, `PTR`, `SOA`, `CAA`, `DNSKEY`, `RRSIG`, matched without regard to case or surrounding whitespace and stored in upper case; any other MUST be refused at load. |
| `server` | the system's resolver | `host`, `host:port` or `[v6]:port`, port 53 when left out. A scheme, path, query or user, a port outside 1–65535 and blanks alone MUST be refused at load. Every question MUST be sent to it when it is set. |
| `transport` | `udp` | `udp`, `tcp` or `tls`, exactly; anything else MUST be refused. `udp` MUST ask again over TCP for an answer marked truncated; `tcp` MUST ask over TCP alone; `tls` MUST ask over TLS (RFC 7858), port 853 when `server` names none, verifying the certificate with the `tls` block for `tls.server_name` or else the server's host, and MUST be refused without `server`. |
| `tls` | none | As `http`'s; with `transport: tls` alone, and refused without it; a defaults block's `tls` MUST apply to a collector with `transport: tls` alone. Its files MUST be read at each scrape, and `--dry-run` MUST read them, warning of one a scrape would fail on. |
| `allowed_targets`, `denied_targets` | none | Matched against the name; see below. |

- The record type, server and transport MUST be the collector's alone: no
  probe parameter or static target key may change them. A probe MUST take
  `timeout` and `param_<name>` parameters, and a static target's request
  block `timeout`.
- A target MUST be a name of letters, digits, `.`, `-` and `_`, at most 253
  characters, each label from 1 to 63, matched without regard to case and a
  trailing dot; for `PTR` it MUST instead be an IPv4 or IPv6 address, without
  a zone. A URL, a name with a port, path, query or user, an IP address for
  any other record type, and an address for `PTR` that is none MUST be
  refused, a probe's with `400` before anything is asked and a static
  target's at load.
- The name MUST be asked as an absolute name, so that no search domain of
  the system's configuration is appended to it.
- The answer MUST be a JSON document of `name` (lower case, no trailing
  dot), `record_type`, `server` (`host:port`, `""` for the system's
  resolver), `transport`, `rcode`, the response code by name (`NOERROR`,
  `FORMERR`, `SERVFAIL`, `NXDOMAIN`, `NOTIMP`, `REFUSED`, the names of RFC
  2136's codes, `RCODE<n>` for another), `not_found`, `authoritative`,
  `recursion_available` and `authenticated_data`, the answer's AA, RA and
  AD bits, `count`, `duration_seconds` and `answers`, one object a record
  with its `ttl` and the fields of its type alone:
  `address` (A, AAAA); `target` (CNAME, the name the CNAME record of the
  name points to); `host` and `preference` (MX); `host` (NS);
  `text`, the record's strings joined (TXT); `target`, `port`, `priority`
  and `weight` (SRV); `name` (PTR); `host`, `mailbox`, `serial`,
  `refresh`, `retry`, `expire` and `minimum` (SOA); `flags`, `tag` and
  `value` (CAA); `flags`, `protocol`, `algorithm` and `key_tag`, computed
  as RFC 4034 appendix B does (DNSKEY); `type_covered`, by name or
  `TYPE<n>`, `algorithm`, `labels`, `original_ttl`, `key_tag`, `signer`,
  `inception`, `expiration` and `seconds_until_expiration`, the times placed
  nearest to now as RFC 1982's serial numbers are (RRSIG). An `RRSIG`
  question MUST set the DO bit of its OPT record, and MUST NOT be asked
  again without the record on `FORMERR`; no other question MUST set it. Names MUST be given without their
  trailing dot, and the records MUST be in an order of their own — by
  preference or priority first, then by their data — so that two lookups of
  the same records give the same document.
- Every answer to the question MUST be the document, whatever its response
  code. `not_found` MUST be true for `NXDOMAIN`, and for `NOERROR` without
  records of the type, `answers` empty and `count` 0; for `CNAME` that MUST
  be every name without a CNAME record, whatever other records it has. A
  server that does not answer, an answer of another id, name or type, and
  one whose records do not parse MUST fail the scrape in the `dns` stage,
  naming the record type, the name and the server asked, not the system's;
  a lookup cut short by the scrape's deadline MUST wrap the context's
  error.
- The question MUST be one of the record type, class `IN`, with RD and AD
  set and an EDNS(0) OPT record offering 1232 bytes; a `FORMERR` answer to
  it MUST be asked again once without the OPT record. For `PTR` it MUST be
  of the address's `in-addr.arpa` or `ip6.arpa` name. Over `udp` it MUST be
  sent again every 2 seconds while no answer comes and asked again over TCP
  for an answer marked truncated; over `tcp`, over TCP alone. It MUST go to
  `server`, or without it to the `nameserver` lines of `/etc/resolv.conf`
  in order, port 53, `127.0.0.1` and `::1` when there are none, the next
  asked when one fails, does not answer within 5 seconds, or answers
  `SERVFAIL`, `REFUSED`, `NOTIMP` or `FORMERR`; the last such answer MUST
  be the document when no server answers better, kept over a later
  server's failure, and the scrape MUST fail only when none answers. `/etc/hosts` and the file's other lines MUST NOT
  be read.
- The records MUST be those of the record type whose owner is the name, or
  but for `CNAME` a name the answer's CNAME records lead to from it, in any
  order, at most 16 deep; for `CNAME` the name's own CNAME record alone.
- The target lists MUST be matched against the name by name alone, never
  by what it resolves to: a denied name, and with `allowed_targets` one that
  matches none of its names, MUST be refused with `403` before the server is
  asked, so a network in `allowed_targets` allows no name. A `PTR` target
  MUST be matched as an address, the cloud metadata addresses refused unless
  `allowed_targets` lists them. The server is not held to the lists.
- The decoder MUST be `json`, as for `grpc`; another decoder, and a
  transform other than `jq`, `yq` and `python`, MUST be refused at load.
- A debug probe's request section MUST show `QUERY`, the DNS URI without
  its query, and the record type, server and transport, with the number of
  records, `not found`, a response code other than `NOERROR` and
  `NXDOMAIN`, or the error as its outcome. The verbose `url` label
  MUST be the DNS URI of RFC 4501, `dns:name?type=T` with the system's
  resolver and `dns://host:port/name?type=T` with `server`, and
  `http_method` MUST be `QUERY`.
