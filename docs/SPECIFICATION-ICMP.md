# Prometheus Universal Exporter — `icmp` Request Type Specification

This is the `icmp` request type's section of § 5.1 of the
[exporter specification](SPECIFICATION-EXPORTER.md), in a document of its
own so that a change to the type is a change to this document. Everything
the request types share — the registry and the rules every type is held to
(§ 5.1), building without a type, decoding, transforms, error policies,
limits, caching, static targets and exposition — is in the exporter
specification, and a section number here, such as § 42.14, is one of its
sections. What a user reads of the type is [ICMP.md](ICMP.md).

## `icmp`

An `icmp` collector pings a host, as ping does, and hands the transforms a
JSON document of the replies. Its keys:

| Key | Default | Rule |
| --- | --- | --- |
| `count` | `3` | Echo requests a scrape sends, 1 to 100. |
| `interval` | `1s` | How long apart they are sent, at least `10ms`. |
| `reply_timeout` | `1s` | How long the last one's reply is waited for, at least `10ms`. |
| `payload_bytes` | `56` | Bytes of data each carries, 8 to 1232. |
| `ip_family`, `allowed_targets`, `denied_targets` | as `http` | |

- A key outside its bounds, and an `ip_family` other than `ip4`, `ip6`,
  `prefer_ip4` and `prefer_ip6`, MUST be refused at load. The target MUST be
  a host, a name or an address, an IPv6 one with brackets or without; a
  scheme, a path, a query, a user or a port MUST be refused before anything
  is sent. Its `url` label MUST be `icmp://` and the host, its
  `http_method` `PING`.
- A name MUST be looked up when the scrape is made, and the first address of
  the family `ip_family` asks for pinged: `ip4` and `ip6` of that family
  alone, failing the scrape, naming the host and the family, when it has
  none; `prefer_ip4` and `prefer_ip6` of that family first; left out, the
  first address. The target lists MUST hold the name before the lookup and
  the address before a socket is opened, a refused one being refused
  whether or not the system opens ICMP sockets, the socket then being
  connected to that address alone.
- The echoes MUST go on an unprivileged ICMP datagram socket (`SOCK_DGRAM`
  of `IPPROTO_ICMP` or `IPPROTO_ICMPV6`), connected to the address, never a
  raw socket; a socket the system refuses for permission MUST fail the
  scrape naming `net.ipv4.ping_group_range`. An echo MUST be type 8 with
  its checksum, or 128 for ICMPv6, its sequence the echo's number from 1,
  its data a random 8-byte token of the scrape and bytes after it; a reply
  MUST be taken only when it is type 0 or 129, code 0, of a sequence sent and
  of the echo's data, after an IPv4 header where the system reads one, a
  second reply to one echo being counted as a duplicate.
- The echoes MUST be sent `interval` apart, and the scrape MUST end once
  every reply came, `reply_timeout` after the last was sent, or at its
  deadline. A host that answered none MUST be no failure: the document is
  answered with `received` 0. An echo the network has no route for MUST
  count as lost, not fail the scrape.
- The document MUST be a JSON object with `host`, `address`, `family` (4 or
  6), `sent`, `received`, `loss_ratio`, `rtts_seconds`, the round trips from
  the shortest, `rtt_min_seconds`, `rtt_avg_seconds`, `rtt_max_seconds` and
  `rtt_stddev_seconds` (the population deviation) when a reply came and
  left out otherwise, `duplicates`, `payload_bytes` and `duration_seconds`,
  with the `Content-Type` `application/json` and no status.
- With `all_addresses`, every address of the name of the families
  `ip_family` takes, `prefer_ip4` and `prefer_ip6` both, that family's
  first, MUST be pinged, the first 16 in that order, at once, each as an
  address alone is, its own request in a debug report and its span. The
  document MUST have `addresses`, each one's `address`, `family`, `sent`,
  `received`, `loss_ratio`, `rtts_seconds`, the statistics when a reply
  came and `duplicates`, in that order, and `addresses_found`, how many
  the name has of those families; its top MUST then have `address` `""`
  and `family` 0, and the counts, the round trips and their statistics of
  all of them together. An address whose ping fails MUST fail the scrape,
  naming it. Without it, the document MUST have neither key.
