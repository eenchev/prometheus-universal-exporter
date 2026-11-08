# Ping over ICMP

The `icmp` request type pings a host, as `ping` does: it sends ICMP echo
requests and hands the transform a JSON document of how many replies came
back and how long each took. Use it where a host's reachability is what you
want to know and no port of it is the thing to check — a router, a switch,
a host behind a firewall that answers pings, the gateway of a site — and
alert on the loss and the round trips. Whether a port takes a connection
is a [`tcp`](TCP.md#checking-that-a-port-is-open) collector's
`connect_only`, which goes beside a ping of the same host as a collector
of its own ([With a ping](TCP.md#with-a-ping)).

The echoes go on an unprivileged ICMP socket, so the exporter needs neither
root nor `CAP_NET_RAW` ([Permissions](#permissions)).

The type is opt-in: the default build and the published image leave it out,
so a build carries it only when asked
([Building with icmp](#building-with-icmp)). It needs nothing outside Go's
standard library, and runs on Linux and macOS.

## Building with icmp

`OPT_IN_REQUEST_TYPES` names the opt-in types a build adds to the types it
carries, comma-separated:

```sh
make build OPT_IN_REQUEST_TYPES=icmp              # bin/prometheus-universal-exporter
docker build --build-arg OPT_IN_REQUEST_TYPES=icmp -t exporter:icmp .
go build -tags "$(sh tools/request-type-tags.sh '' '' icmp)" .
```

The tag is `request_type_icmp`. A configuration with an `icmp` collector
is refused by a build without the type, at startup, on a reload and by
`--dry-run`, saying how to build one that has it.

## A collector

```yaml
collectors:
  - name: ping
    request:
      type: icmp
      count: 5
      interval: 200ms
    decoder:
      type: json
    transform:
      type: jq
    metrics:
      - name: ping_loss_ratio
        expression: .loss_ratio
      - name: ping_rtt_seconds
        expression: .rtt_avg_seconds
      - name: ping_rtt_max_seconds
        expression: .rtt_max_seconds
```

A probe of `target=gateway.example.com` sends five echoes 200ms apart and
answers:

```json
{"host":"gateway.example.com","address":"192.0.2.1","family":4,"sent":5,"received":5,
 "loss_ratio":0,"rtts_seconds":[0.0011,0.0012,0.0012,0.0013,0.0019],
 "rtt_min_seconds":0.0011,"rtt_avg_seconds":0.00134,"rtt_max_seconds":0.0019,
 "rtt_stddev_seconds":0.00029,"duplicates":0,"payload_bytes":56,"duration_seconds":0.81}
```

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `icmp`. |
| `count` | `3` | How many echo requests a scrape sends, from 1 to 100. |
| `interval` | `1s` | How long apart they are sent, at least `10ms`. |
| `reply_timeout` | `1s` | How long the last echo's reply is waited for once it is sent, at least `10ms`. |
| `payload_bytes` | `56` | How many bytes of data each echo carries, from 8 to 1232, as `ping -s`. The first 8 are a random token the replies are matched by. |
| `ip_family` | either | `ip4` or `ip6` pings the first address of that family of a name, `prefer_ip4` or `prefer_ip6` that family's first and else the other's; left out, the first address the name resolves to. |
| `all_addresses` | `false` | Ping every address of the name, of the families `ip_family` takes, at once: see [Every address](#every-address). |
| `allowed_targets`, `denied_targets` | none | The hosts the collector may and may not ping, as for [any type that reaches a host](REQUESTS.md#restricting-targets): the name before it is looked up, and the address pinged before a socket is opened. |

No other request key applies, and setting one is a startup error. A scrape
takes about `(count - 1) × interval + reply_timeout`, and ends sooner once
every reply came; keep it within the probe's timeout, which ends it
whatever is left.

The document has `host`, as the target named it; `address`, the address
pinged, and `family`, 4 or 6; `sent` and `received`, and `loss_ratio`, the
share of echoes with no reply, 0 to 1; `rtts_seconds`, the round trip of
each reply, in order from the shortest; `rtt_min_seconds`,
`rtt_avg_seconds`, `rtt_max_seconds` and `rtt_stddev_seconds`, the
population standard deviation, when a reply came, and left out when none
did, so that a rule of them is a [missing
value](CONFIGURATION.md#when-a-metric-cannot-be-extracted) for a host that
answered nothing; `duplicates`, the replies that came twice;
`payload_bytes`; and `duration_seconds`, the whole scrape, the lookup
included.

**A host that answers nothing is no failure.** The scrape answers with
`received` 0 and a loss of 1, which is what a rule alerts on:

```yaml
      - name: ping_up
        expression: 'if .received > 0 then 1 else 0 end'
```

## Every address

A name with several addresses — a pool behind round-robin DNS, a
dual-stack host — is pinged at one of them, so a bad one among them, or a
broken IPv6 path, goes unseen half the time. `all_addresses: true` pings
every address, at once, each with its own socket:

```yaml
    request:
      type: icmp
      count: 3
      interval: 200ms
      all_addresses: true
    metrics:
      - name: ping_loss_ratio
        items: .addresses[]
        expression: .loss_ratio
        labels:
          - name: address
            expression: .address
      - name: ping_any_loss_ratio
        expression: .loss_ratio
```

```json
{"host":"pool.example.com","address":"","family":0,"sent":6,"received":3,"loss_ratio":0.5,
 "rtts_seconds":[0.0011,0.0012,0.0019],"rtt_min_seconds":0.0011,"rtt_avg_seconds":0.0014,"rtt_max_seconds":0.0019,
 "rtt_stddev_seconds":0.00035,"duplicates":0,"payload_bytes":56,"duration_seconds":2.01,"addresses_found":2,
 "addresses":[
  {"address":"192.0.2.1","family":4,"sent":3,"received":3,"loss_ratio":0,"rtts_seconds":[0.0011,0.0012,0.0019],
   "rtt_min_seconds":0.0011,"rtt_avg_seconds":0.0014,"rtt_max_seconds":0.0019,"rtt_stddev_seconds":0.00035,"duplicates":0},
  {"address":"192.0.2.2","family":4,"sent":3,"received":0,"loss_ratio":1,"rtts_seconds":[],"duplicates":0}]}
```

- `addresses` are the addresses pinged, in the order the resolver gave
  them, of the families `ip_family` takes: `ip4` or `ip6` alone, and with
  `prefer_ip4` or `prefer_ip6`, or left out, both, the preferred first.
  Each has the counts, round trips and statistics an address alone has at
  the top of the document. At most 16 are pinged; `addresses_found` is how
  many the name has of those families.
- The top of the document is of all of them together: `sent`, `received`,
  `loss_ratio`, the round trips and their statistics, with `address` `""`
  and `family` 0.
- An address the target lists refuse, or that no socket can be opened
  for, fails the scrape, naming it. A debug probe shows each address's
  echoes as a request of its own, as an exported trace does.

## The target

The probe's `target` is the host: a name, an IPv4 address or an IPv6 one,
with brackets or without, and nothing else. A scheme, a path, a query, a
user or a port is refused with `400`, before anything is sent. A name is
looked up when the scrape is made, and the scrape is labelled
`url="icmp://<host>"`, `http_method="PING"`.

## Static targets

A [static target](STATIC-TARGETS.md) of an `icmp` collector is a host to
ping on the exporter's own schedule, its `target` the host. Its `request`
may set `timeout`, and no other key.

## Permissions

The echoes go on an ICMP datagram socket (`SOCK_DGRAM` of `IPPROTO_ICMP`),
which the system answers the replies to on its own, never a raw socket. On
Linux a process may open one when its group is in the sysctl
`net.ipv4.ping_group_range`, which Docker, containerd and systemd set to
every group (`0 2147483647`); a system that leaves it at the kernel's
`1 0` refuses the socket, and every scrape fails:

```text
opening an ICMP socket to ping 192.0.2.1: socket: permission denied; the system lets no process of the exporter's group open one: on Linux, set the sysctl net.ipv4.ping_group_range to take the group, such as "0 2147483647"
```

The sysctl is of the network namespace, and a safe one to Kubernetes, so a
pod sets it for itself:

```yaml
podSecurityContext:
  sysctls:
    - name: net.ipv4.ping_group_range
      value: "0 2147483647"
```

macOS opens the socket for every process. Over IPv6 the same socket is
opened with `IPPROTO_ICMPV6`, under the same sysctl.

## Limits

A scrape ends once every reply came, `reply_timeout` after the last echo
was sent, or at the probe's deadline, whichever is first: the echoes not
sent by then are not counted. The document is a few hundred bytes and a
round trip a number for each reply, so `max_response_bytes` does not apply.

## Errors and self-metrics

What fails a scrape is being unable to ask: a name that does not resolve
(`looking up gw.example: lookup gw.example: no such host`), one with no
address of the family `ip_family` asks for, an address the target lists
refuse, and a socket the system does not open. An echo the network has no
route for, and one the host or a router answers with an ICMP error, is a
lost echo, counted in `loss_ratio`, not a failure. A failed scrape is
counted and logged as any other type's, in the stage `icmp`; the verbose
self-metrics label it `url="icmp://<host>"`.

## Limitations

- **Echo only.** The type sends echo requests and reads echo replies; it
  does not trace a route, and does not read the TTL a reply came with,
  which a datagram socket does not hand on.
- **Linux and macOS.** A build for another system has the type, and every
  scrape fails, saying so.
- **One address, unless asked.** A name with several addresses is pinged at
  one, the first of its family, but with
  [`all_addresses`](#every-address).

## Troubleshooting

| The scrape says | Do |
| --- | --- |
| `... uses request.type "icmp", which this build of the exporter does not include` | Build with `OPT_IN_REQUEST_TYPES=icmp` ([Building with icmp](#building-with-icmp)); the published image does not carry it. |
| `socket: permission denied; ... net.ipv4.ping_group_range` | Set the sysctl for the exporter's group ([Permissions](#permissions)). |
| `socket: address family not supported by protocol` | The host has no IPv6; set `ip_family: ip4`. |
| `has no IPv6 address, and request.ip_family is ip6` | The name has no address of that family; set `ip_family` to the other, or `prefer_ip6`. |
| every echo lost from a pod, answered from the node | A network policy or a firewall drops ICMP between them; allow ICMP echo from the exporter's pods. |
