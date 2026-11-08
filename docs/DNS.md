# DNS

The `dns` request type resolves one name and hands its records to the
transforms as a JSON document. Use it to watch what a name resolves to from
where the exporter runs:

- that a service's name still has its records, and how many — the A records
  behind a load balancer, the MX records of a mail domain;
- that an internal resolver answers, and how long it takes;
- that a record holds what it should: an SPF or verification TXT record, the
  port of an SRV record, the name a PTR record gives an address;
- how long a record is cached for, a zone's SOA serial on each of its
  servers, its CAA records, its DNSSEC keys, and whether the resolver
  validated the answer (the AD bit);
- what a server answers when it fails: `SERVFAIL` and `REFUSED` are data,
  not a failed scrape.

It needs nothing but a resolver to ask: the system's, or a server the
collector names.

## A collector

```yaml
collectors:
  - name: mail_exchangers
    request:
      type: dns
      record_type: MX
      server: 10.0.0.53:53
    transform:
      type: jq
    metrics:
      - name: dns_records
        description: Records the name has of the record type
        expression: .count
      - name: dns_name_found
        description: Whether the name exists and has records of the type
        expression: if .not_found then 0 else 1 end
      - name: dns_lookup_duration_seconds
        description: How long the lookup took
        expression: .duration_seconds
      - name: dns_mx_preference
        description: The preference of each mail exchanger
        items: .answers[]
        expression: .preference
        labels:
          - name: host
            expression: .host
```

```text
/probe?collector=mail_exchangers&target=example.com
```

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `dns`. |
| `record_type` | `A` | The record type asked for: `A`, `AAAA`, `CNAME`, `MX`, `NS`, `TXT`, `SRV`, `PTR`, `SOA`, `CAA`, `DNSKEY` or `RRSIG`, in any case and with blanks around it or not. |
| `server` | the system's resolver | The server every question is sent to: `host` or `host:port`, port 53 when it is left out, an IPv6 address in brackets with a port. Left out, the servers of `/etc/resolv.conf`. |
| `transport` | `udp` | `udp`, which asks again over TCP when an answer is too long for UDP, as every resolver does, `tcp`, which asks over TCP alone, or `tls`, DNS over TLS, of `server` alone: see [DNS over TLS](#dns-over-tls). |
| `tls` | — | With `transport: tls` alone: the [`tls` block](REQUESTS.md#tls) the server's certificate is verified with, for `tls.server_name` or else the server's host. |
| `allowed_targets`, `denied_targets` | — | The names the collector may and may not resolve: see [Target lists](#target-lists). |

No other request key applies, and setting one — `path`, `headers` —
is a startup error. The record type, the server and the transport are the
collector's: no probe parameter changes them, so a probe cannot send the
exporter's questions to a server the configuration does not name. A probe
takes `timeout` and `param_<name>` parameters for the collector's
[label values](REQUESTS.md#in-label-values); a static target's request block
takes `timeout`.

The decoder is `json`, whether `decoder.type` is left out or set to it; any
other decoder, and a transform that does not read JSON (`regex`, `csv`,
`css`, `prometheus`), is refused when the configuration loads. Use `jq`,
`yq` or `python`.

### Coming from 2.0

Exporter 2.0 asked Go's resolver; later releases ask the question
themselves ([The question](#the-question)), and two things behave
differently:

- **`SERVFAIL`, `REFUSED` and the other failures are data.** 2.0 failed the
  scrape on them, so the target went down and `on_fetch_error` applied; now
  the scrape succeeds with `rcode` saying what the server answered. A
  collector that relied on the failure keeps an alertable series with a
  rule of its own:

  ```yaml
      - name: dns_answered
        description: 1 when the server answered NOERROR or NXDOMAIN
        expression: 'if .rcode == "NOERROR" or .rcode == "NXDOMAIN" then 1 else 0 end'
  ```

  and an alert on `dns_answered == 0` in place of one on the target's
  scrape failing.
- **`/etc/hosts` is not read.** A name 2.0 answered from the exporter's
  `/etc/hosts`, or a Kubernetes pod's `hostAliases`, is asked of the server
  like any other, and is `not_found` unless the server has it.

Nothing else needs changing: the document gains `rcode`, the flags and each
record's `ttl`, which rules written for 2.0 do not read.

## The target

The target is the name to resolve, such as `example.com` or
`_http._tcp.example.com` for an SRV record: a name alone, of letters,
digits, `.`, `-` and `_`, at most 253 characters with every label between
the dots from 1 to 63. It is matched in any case, and a trailing dot is
dropped. A URL, a name with a port or a path, and an IP address are refused
with `400` before anything is asked, a static target's when it loads.

With `record_type: PTR` the target is instead the IP address whose names
are asked for, IPv4 or IPv6, such as `192.0.2.1` or `2001:db8::1`; the
exporter asks for the PTR records of its `in-addr.arpa` or `ip6.arpa` name.

The name is always asked as an absolute one: the search domains of
`/etc/resolv.conf` are never appended to it, so `db` is asked as `db.`, and
a name inside a Kubernetes cluster is written out in full,
`api.default.svc.cluster.local`.

## The document

```json
{
  "name": "example.com",
  "record_type": "MX",
  "server": "10.0.0.53:53",
  "transport": "udp",
  "rcode": "NOERROR",
  "not_found": false,
  "authoritative": false,
  "recursion_available": true,
  "authenticated_data": false,
  "count": 2,
  "duration_seconds": 0.0042,
  "answers": [
    {"host": "mx1.example.com", "preference": 10, "ttl": 3600},
    {"host": "mx2.example.com", "preference": 20, "ttl": 3600}
  ]
}
```

- `name` is the target as it was asked, in lower case and without the
  trailing dot; `server` is `request.server` as `host:port`, `""` for the
  system's resolver.
- `answers` holds one object a record, with the fields of its type and its
  `ttl`, the seconds the server said it may be cached for:

  | Record type | Fields |
  | --- | --- |
  | `A`, `AAAA` | `address` |
  | `CNAME` | `target`, the name the alias's CNAME record points to |
  | `MX` | `host`, `preference` |
  | `NS` | `host` |
  | `TXT` | `text`, the record's strings joined into one |
  | `SRV` | `target`, `port`, `priority`, `weight` |
  | `PTR` | `name` |
  | `SOA` | `host`, the zone's primary server, `mailbox`, its contact as a name, `serial`, `refresh`, `retry`, `expire`, `minimum` |
  | `CAA` | `flags`, `tag` (`issue`, `issuewild`, `iodef`), `value` |
  | `DNSKEY` | `flags` (257 a key-signing key, 256 a zone-signing key), `protocol`, `algorithm`, `key_tag`, as DS records and signatures name the key |
  | `RRSIG` | `type_covered`, the type of the records it signs, `algorithm`, `labels`, `original_ttl`, `key_tag` and `signer`, the key that signed it, `inception` and `expiration`, seconds since 1970, and `seconds_until_expiration`: see [DNSSEC signatures](#dnssec-signatures) |

  Names are given without their trailing dot. The records are in an order of
  their own — by preference or priority first where the type has one, then
  by their data — so that two lookups of the same records give the same
  document and the same series, though a resolver shuffles records of the
  same priority.
- `count` is the number of records, `answers | length`.
- `rcode` is the answer's response code: `NOERROR`, `NXDOMAIN`, `SERVFAIL`,
  `REFUSED`, `FORMERR`, `NOTIMP`, or another by name or as `RCODE<n>`.
  Whatever it is, the scrape succeeds with the document, so a rule can say
  that a server fails: `if .rcode == "SERVFAIL" then 1 else 0 end`.
- `not_found` is `true` when the name does not exist (`NXDOMAIN`), and when
  it exists without records of the type (`NOERROR` and no record); `rcode`
  tells the two apart. `answers` is then empty and `count` 0. For `CNAME`
  that is any name that is no alias, whatever other records it has: a name
  with addresses and one with a TXT record alone are `not_found` as one
  that does not exist is. A `SERVFAIL` or `REFUSED` is not `not_found`.
- `authoritative`, `recursion_available` and `authenticated_data` are the
  answer's AA, RA and AD bits: whether the server is authoritative for the
  name, offers recursion, and, a validating resolver, validated the answer
  with DNSSEC.
- `duration_seconds` is how long the lookup took, the retries of the
  resolver included.

## The question

The exporter asks the question itself, with a client of its own: one
question of the record type, class `IN`, recursion desired, the AD bit set
so that a validating resolver reports it (RFC 6840), and an EDNS(0) record
offering answers of 1232 bytes over UDP, as resolvers do since DNS Flag Day
2020; a server that answers it `FORMERR`, knowing no EDNS, is asked again
without it. Over `udp` the question is sent again every 2 seconds while no
answer comes, and asked again over TCP when the answer is marked truncated;
over `tcp` it is asked over TCP alone. For `PTR` the question is of the
address's `in-addr.arpa` or `ip6.arpa` name.

It goes to `server`, or without one to the `nameserver` lines of
`/etc/resolv.conf` in their order, on port 53, `127.0.0.1` and `::1` when
the file names none; the next is asked when one fails, does not answer
within 5 seconds, or answers `SERVFAIL`, `REFUSED`, `NOTIMP` or `FORMERR`;
the last of those answers is the document when no server answers better,
kept over a later server's failure, and the scrape fails only when no server
answers at all. The file's
`search`, `options` (`timeout`, `attempts`, `rotate`, `ndots`) and
`/etc/hosts` are not read: a name in `/etc/hosts` is asked of the server
like any other.

The answer is the server's records of the type of the name, or of the
names its CNAME records lead to, followed to the end of the chain however
the server orders them, as a resolver gives them; for `CNAME` it is the
name's own CNAME record alone, so an alias of an alias gives the next name
of the chain, not its end. Nothing is validated by the exporter:
`authenticated_data` is the resolver's word.

A server that does not answer within the deadline, and an answer that is no
answer to the question — another id, name or type — or whose records do not
parse fail the scrape, in the `dns` stage, saying what was asked of which
server: `resolving A www.example.com on 10.0.0.53:53: cannot unmarshal DNS
message`. A lookup cut short by the scrape's deadline says so and counts as
a timeout.

## DNSSEC signatures

A zone signed with DNSSEC stops resolving, for every validating resolver,
the moment its signatures expire: a signer that stopped re-signing is an
outage with a date on it. `record_type: RRSIG` asks for the name's
signatures, with the DO bit set (RFC 3225) so that the server gives them,
and answers each one: the type of the records it covers, the key that made
it (`key_tag`, as a `DNSKEY` answer names the keys, and `signer`), and when
it was made and expires. The wire writes those times as 32-bit serial
numbers; the exporter places each at the time nearest to now (RFC 4034
§ 3.1.5), so they stay right past 2106.

```yaml
  - name: zone_signatures
    request:
      type: dns
      record_type: RRSIG
      server: ns1.example.com
    transform:
      type: jq
    metrics:
      - name: dns_signature_seconds_until_expiry
        description: How long each signature of the name has left
        items: .answers[]
        expression: .seconds_until_expiration
        labels:
          - name: type
            expression: .type_covered
          - name: key_tag
            expression: .key_tag | tostring
```

Ask the zone's own servers, or a resolver that passes signatures on: a
resolver that does not, and an old server that refuses the DO bit's OPT
record with `FORMERR`, answer no signature. An RRSIG question is not asked
again without the OPT record, which would get none; its `FORMERR` is the
document's `rcode`. Nothing is validated: the signatures are reported as
the server gave them.

## DNS over TLS

`transport: tls` asks `server` over TLS, as RFC 7858 has it: the question
and its answer as over TCP, within a TLS connection, made for each scrape.
The server's port is 853 when `server` names none. Its certificate is
verified against the system's roots and `tls.ca_file`, for `tls.server_name`
or else the server's host, with the [`tls` block](REQUESTS.md#tls) of any
other type; a client certificate is `cert_file` and `key_file`. `tls` needs
`server`: the system's resolvers are asked over UDP and TCP, and a `tls`
block without `transport: tls` is refused.

```yaml
  - name: dot_resolver
    request:
      type: dns
      record_type: A
      transport: tls
      server: dns.example.com
      tls:
        ca_file: /etc/exporter/dns-ca.pem
    transform:
      type: jq
    metrics:
      - name: dns_dot_lookup_seconds
        expression: .duration_seconds
```

## Target lists

`allowed_targets` and `denied_targets` take the hosts, globs, addresses and
networks they take for [any other type](REQUESTS.md#restricting-targets), and
are matched against the name to resolve, by name alone: a lookup connects to
nothing at the name, so what it resolves to is the answer, not a host the
exporter reaches. A name that matches `denied_targets`, and with
`allowed_targets` one that matches none of its names, is refused with `403`
before the server is asked; a network in `allowed_targets` allows no name.
A PTR target, an address, is matched by address, the cloud metadata
addresses refused as every collector refuses them.

The server is the configuration's and is not held to the lists.

## Examples

How many addresses a name has, and one series an address:

```yaml
  - name: service_addresses
    request:
      type: dns
      record_type: A
    transform:
      type: jq
    metrics:
      - name: dns_address_records
        expression: .count
      - name: dns_address
        items: .answers[]
        expression: "1"
        labels:
          - name: address
            expression: .address
```

Whether a domain's SPF record is in place:

```yaml
  - name: spf
    request:
      type: dns
      record_type: TXT
    transform:
      type: jq
    metrics:
      - name: dns_spf_record_present
        expression: '[.answers[].text | select(startswith("v=spf1"))] | length'
```

How long the records of a name may be cached, the least of their TTLs:

```yaml
  - name: record_ttl
    request:
      type: dns
      record_type: A
    transform:
      type: jq
    metrics:
      - name: dns_record_ttl_seconds
        expression: '[.answers[].ttl] | min // 0'
      - name: dns_server_failing
        expression: 'if .rcode == "SERVFAIL" or .rcode == "REFUSED" then 1 else 0 end'
```

A zone's SOA serial as each of its servers has it, one collector each, so
that a secondary behind the primary shows:

```yaml
  - name: zone_serial_ns1
    request:
      type: dns
      record_type: SOA
      server: ns1.example.com
    transform:
      type: jq
    metrics:
      - name: dns_zone_serial
        expression: .answers[0].serial // 0
        labels:
          - name: server
            expression: .server
```

## Static targets

A static target names the name to resolve, and may set `timeout`:

```yaml
interval: 5m
targets:
  - name: example_mx
    collector: mail_exchangers
    target: example.com
```

## Errors and self-metrics

A failed lookup is reported in the `dns` stage and follows
`error_handling.on_fetch_error`. A name the target lists refuse counts in
`http_exporter_targets_refused_total`, answered `403`. Transforms see no
status (`$status` is `null`, Python's `response.status_code` `None`) and the
header `Content-Type: application/json`.

A [debug probe](CONFIGURATION.md#debug-probes) shows the lookup in its
request section, with how it ended — the number of records, `not found`, a
response code other than `NOERROR` and `NXDOMAIN`, such as `SERVFAIL`, or
the error:

```text
Requests
  1. QUERY dns://10.0.0.53:53/example.com -> 2 records in 4ms
     Dns-Server: 10.0.0.53:53
     Dns-Transport: udp
     Dns-Type: MX
```

With [verbose self-metrics](SELF-METRICS.md#verbose-per-request-self-metrics)
a lookup is labelled with its DNS URI, as RFC 4501 writes one —
`url="dns://10.0.0.53:53/example.com?type=MX"`, or
`url="dns:example.com?type=MX"` with the system's resolver — and
`http_method="QUERY"`.
