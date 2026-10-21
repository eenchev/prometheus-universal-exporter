# DNS

The `dns` request type resolves one name and hands its records to the
transforms as a JSON document. Use it to watch what a name resolves to from
where the exporter runs:

- that a service's name still has its records, and how many — the A records
  behind a load balancer, the MX records of a mail domain;
- that an internal resolver answers, and how long it takes;
- that a record holds what it should: an SPF or verification TXT record, the
  port of an SRV record, the name a PTR record gives an address.

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
| `record_type` | `A` | The record type asked for: `A`, `AAAA`, `CNAME`, `MX`, `NS`, `TXT`, `SRV` or `PTR`, in any case and with blanks around it or not. |
| `server` | the system's resolver | The server every question is sent to: `host` or `host:port`, port 53 when it is left out, an IPv6 address in brackets with a port. Left out, the servers of `/etc/resolv.conf`. |
| `transport` | `udp` | `udp`, which asks again over TCP when an answer is too long for UDP, as every resolver does, or `tcp`, which asks over TCP alone. |
| `allowed_targets`, `denied_targets` | — | The names the collector may and may not resolve: see [Target lists](#target-lists). |

No other request key applies, and setting one — `path`, `headers`, `tls` —
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
  "not_found": false,
  "count": 2,
  "duration_seconds": 0.0042,
  "answers": [
    {"host": "mx1.example.com", "preference": 10},
    {"host": "mx2.example.com", "preference": 20}
  ]
}
```

- `name` is the target as it was asked, in lower case and without the
  trailing dot; `server` is `request.server` as `host:port`, `""` for the
  system's resolver.
- `answers` holds one object a record, with the fields of its type alone:

  | Record type | Fields |
  | --- | --- |
  | `A`, `AAAA` | `address` |
  | `CNAME` | `target`, the name the alias leads to at the end of its chain |
  | `MX` | `host`, `preference` |
  | `NS` | `host` |
  | `TXT` | `text`, the record's strings joined into one |
  | `SRV` | `target`, `port`, `priority`, `weight` |
  | `PTR` | `name` |

  Names are given without their trailing dot. The records are in an order of
  their own — by preference or priority first where the type has one, then
  by their data — so that two lookups of the same records give the same
  document and the same series, though a resolver shuffles records of the
  same priority.
- `count` is the number of records, `answers | length`.
- `not_found` is `true` when the name does not exist (NXDOMAIN), and when it
  exists without records of the type: the resolver does not tell the two
  apart. `answers` is then empty and `count` 0, and the scrape succeeds, so
  a rule can say a record is missing. A name that is no alias asked for its
  `CNAME` is not `not_found`: it has no `CNAME` record to show, and its
  `answers` are empty.
- `duration_seconds` is how long the lookup took, the retries of the
  resolver included.

## What the resolver gives, and what it does not

The lookup is Go's own resolver, built into the exporter, which needs no
library and no C resolver. It gives the records' data and tells a name not
found from a failure. It does **not** give:

- **TTLs.** No record's time to live is known, so none is in the document.
- **Response codes** beyond not found. `SERVFAIL`, `REFUSED`, a malformed
  answer and a server that does not answer within the deadline are all
  failures of the scrape, in the `dns` stage, saying what was asked of which
  server: `resolving A www.example.com on 10.0.0.53:53: server misbehaving`.
  A lookup cut short by the scrape's deadline says so and counts as a
  timeout.
- **Records of other types**: SOA, CAA, DNSKEY and the like cannot be asked
  for.
- **DNSSEC**: nothing is validated, and the AD bit is not reported.

Records whose names are not valid DNS names are left out by the resolver;
the others are answered. The resolver reads `/etc/hosts` before it asks a
server for `A`, `AAAA`, `CNAME` and `PTR`, as it does for any other lookup of
the exporter, `server` or not: a name there is answered from it.

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

A [debug probe](CONFIGURATION.md#debugging-a-probe) shows the lookup in its
request section, with how it ended — the number of records, `not found`, or
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
