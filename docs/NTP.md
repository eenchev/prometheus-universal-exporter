# NTP

The `ntp` request type asks an NTP server for the time, as an SNTP client
does, and hands its reply to the transforms as a JSON document: how far the
exporter's clock is from the server's, how long the round trip took, and
what the server says of itself — its stratum, leap indicator, precision,
root delay and dispersion, and the clock or server it follows. Use it to
watch, from where the exporter runs:

- that a time server answers, and keeps the time: a server that lost its
  upstream says so, and fails the scrape;
- how far a host's clock, the exporter's own, is from a server it should
  agree with;
- which stratum a server is at, and which reference it follows.

It complements node_exporter's `timex` collector, which reports what the
kernel of the host it runs on thinks of its own synchronisation: `ntp` asks
a server over the network, any server the exporter can reach, and measures
the offset itself.

One request is sent, in one UDP datagram, as RFC 4330 (SNTP) and RFC 5905
(NTPv4) describe; nothing is configured but the target.

## A collector

```yaml
collectors:
  - name: clock
    request:
      type: ntp
    transform:
      type: jq
    metrics:
      - name: ntp_offset_seconds
        description: How far the server's clock is ahead of the exporter's
        expression: .offset_seconds
      - name: ntp_delay_seconds
        description: The round trip to the server, without the time it held the request
        expression: .delay_seconds
      - name: ntp_stratum
        description: The server's stratum, labelled with its reference id
        expression: .stratum
        labels:
          - name: reference_id
            expression: .reference_id
      - name: ntp_root_distance_seconds
        description: The server's own distance from its reference clock
        expression: .root_delay_seconds / 2 + .root_dispersion_seconds
```

```text
/probe?collector=clock&target=time.example.com
```

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `ntp`. |
| `allowed_targets`, `denied_targets` | — | The hosts the collector may and may not reach, as for [any type that reaches a host](REQUESTS.md#restricting-targets), checked on the address the request is sent to. |

The type has no keys of its own: the request is an NTPv4 client request,
whatever the server. No other request key applies, and setting one —
`path`, `headers`, `tls` — is a startup error. A probe takes `timeout` and
`param_<name>` parameters for the collector's
[label values](REQUESTS.md#in-label-values); a static target's request block
takes `timeout`.

The decoder is `json`, whether `decoder.type` is left out or set to it; any
other decoder, and a transform that does not read JSON, is refused when the
configuration loads. Use `jq`, `yq` or `python`.

## The target

The target is `host:port`, `host` alone for port 123, an IPv6 address in
brackets: `time.example.com`, `10.0.0.1:123`, `[2001:db8::1]:123`. A
scheme, a path or a user is refused with `400` before anything is sent, a
static target's when it loads. An IPv6 address without brackets is the
address whole, on port 123, as for every type whose target is `host` or
`host:port`: `2001:db8::1:123` is the address `2001:db8::1:123`, since it
is one, and not `2001:db8::1` on port 123, which is `[2001:db8::1]:123`. A name is resolved when the request is
sent, and the request goes to its first address.

## The document

```json
{
  "host": "time.example.com",
  "port": 123,
  "address": "192.0.2.10:123",
  "offset_seconds": -0.000412,
  "delay_seconds": 0.0213,
  "version": 4,
  "leap_indicator": 0,
  "stratum": 2,
  "precision": -24,
  "precision_seconds": 5.96e-08,
  "poll": 3,
  "poll_seconds": 8,
  "root_delay_seconds": 0.0042,
  "root_dispersion_seconds": 0.0157,
  "reference_id": "192.0.2.1",
  "reference_time": 1793102370.4171,
  "reference_time_rfc3339": "2026-10-27T10:39:30.4171Z",
  "transmit_time": 1793102401.0021,
  "transmit_time_rfc3339": "2026-10-27T10:40:01.0021Z",
  "duration_seconds": 0.0218
}
```

- `offset_seconds` is how far the server's clock is ahead of the
  exporter's, negative when it is behind: `((t2 - t1) + (t3 - t4)) / 2`,
  with `t1` the time the request was sent and `t4` the time the reply came,
  by the exporter's clock, and `t2` and `t3` the times the server received
  the request and sent the reply, by its own. `delay_seconds` is the round
  trip without the time the server held the request,
  `(t4 - t1) - (t3 - t2)`, and 0 when that is below 0: a server whose
  clock reads coarser than the round trip, such as one of precision -6
  (15.6ms) on a LAN, can say it held the request longer than the round
  trip took. RFC 5905 floors the delay at the precision of the local
  clock; the exporter's reads to the nanosecond, so its floor is 0.
- `t1` is the exporter's wall clock when the request is sent, and `t4` is
  `t1` and the round trip timed by the monotonic clock, so that a step of
  the wall clock during the round trip changes neither. The differences are
  taken of the 64-bit NTP timestamps as signed numbers, as RFC 5905 takes
  them, so a timestamp of the era that starts on 2036-02-07 is read
  rightly beside one of the era before it.
- `version` is the reply's, 3 or 4; `leap_indicator` is 0, or 1 or 2 when a
  leap second is to be added or removed at the end of the day; `stratum` is
  from 1, a server with a reference clock of its own, to 15.
- `precision` and `poll` are the exponents the server sent, log2 seconds,
  and `precision_seconds` and `poll_seconds` the seconds they stand for.
- `root_delay_seconds` and `root_dispersion_seconds` are the server's round
  trip to its reference clock and the error it may have gathered on the
  way: half the one and the other are its root distance, how far from the
  reference its time may be.
- `reference_id` is, of a stratum 1 server, the code of its reference
  clock, such as `GPS` or `PPS`, its bytes in hexadecimal (`0x…`) when they
  are no printable text; of a higher stratum, the IPv4 address of the
  server it follows, or for one it reaches over IPv6 the first four bytes
  of a hash of that address, which reads as an IPv4 address too.
- `reference_time` is when the server's clock was last set, and
  `transmit_time` when it sent the reply, as Unix seconds with their
  fraction and in RFC 3339 in UTC beside them: `.transmit_time -
  .reference_time` is how long ago the server last heard from its
  reference. A server that has never set its clock sends no reference
  time; `reference_time` is then 0 and `reference_time_rfc3339` empty.
- `address` is the address the request was sent to; `duration_seconds` is
  how long the scrape took, the name's resolution included.

## The request and its reply

The request is a client request of NTPv4, 48 bytes, in which nothing is
set but its version, its mode and its transmit timestamp. The transmit
timestamp is a random number, not the exporter's clock: the server sends it
back as the reply's origin timestamp, which tells the reply to this request
from any other datagram, and nothing of the exporter's clock is told to the
server.

The exporter waits for the reply until the scrape's deadline, and reads only
datagrams from the address the request went to. A datagram is not the reply,
and is ignored, when it is shorter than 48 bytes, when its origin timestamp
is not the request's, when it is not a server's (mode 4, or 5), when it is
of an NTP version other than 3 or 4, or when it has no transmit timestamp.
Such a datagram may be forged by anyone who can send to the exporter, so it
does not end the scrape: the reply may still come. When the deadline comes
first, the scrape fails saying how many datagrams were ignored and why the
last one was:

```text
no reply from 192.0.2.10:123 before the deadline; 1 datagram was ignored, as its origin timestamp is not the request's: context deadline exceeded
```

A scrape cancelled before its deadline, as when every probe waiting for it
has gone away, says `before the scrape was cancelled` instead, and
`context canceled`.

Nothing is sent again: a server that limits its clients' rate answers a
client that asks twice in quick succession with a Kiss-o'-Death instead of
the time. One request is made a scrape, and the deadline, the probe's
`timeout` or the scrape's own, 30 seconds when neither is set, bounds it.

**A server that does not keep the time fails the scrape.** The reply is the
server's, but its offset would be to a clock nobody vouches for, and a rule
over `offset_seconds` would alert on that clock rather than say why. So
these fail the scrape, at once, in the `ntp` stage:

- a Kiss-o'-Death, a reply of stratum 0, named by its code:
  `time.example.com:123 answered a Kiss-o'-Death, RATE: rate exceeded: the
  server asks to be asked less often`. `DENY` and `RSTR` say the server
  refuses the exporter; RFC 5905 lists the others.
- leap indicator 3, the alarm of a server whose clock is not synchronised,
  and stratum 16, which says the same: `time.example.com:123 is not
  synchronised: its leap indicator is 3, the alarm condition`.
- a stratum past 16, which no server has.

A server that is not synchronised is told by the scrape failing — the
collector's series absent, `http_exporter_scrape_success_total` not
counting it — as a server that does not answer is, rather than by a field
a rule must know to look at.

## Examples

How long ago the server last heard from its reference, and whether a leap
second is announced:

```yaml
  - name: clock_reference
    request:
      type: ntp
    transform:
      type: jq
    metrics:
      - name: ntp_reference_age_seconds
        expression: .transmit_time - .reference_time
      - name: ntp_leap_second_announced
        expression: if .leap_indicator == 1 or .leap_indicator == 2 then 1 else 0 end
```

## Static targets

A static target names the server, `host` or `host:port`, and may set
`timeout`:

```yaml
interval: 1m
targets:
  - name: pool
    collector: clock
    target: time.example.com
```

## Errors and self-metrics

A failed request is reported in the `ntp` stage and follows
`error_handling.on_fetch_error`, naming the server: no reply before the
deadline, a refusal the network sent back
(`reading the reply from 192.0.2.10:123: ... connection refused`), a
Kiss-o'-Death or a server that is not synchronised. What a probe's caller
is answered of a failure follows [the rules of every
type](CONFIGURATION.md#what-a-failed-probes-caller-is-told): a Kiss-o'-Death's code
is told when RFC 5905 lists it and withheld otherwise, as a value of the
server's answer. A host the target lists refuse counts in
`http_exporter_targets_refused_total`, answered `403`. Transforms see no
status (`$status` is `null`, Python's `response.status_code` `None`) and
the header `Content-Type: application/json`.

A [debug probe](CONFIGURATION.md#debug-probes) shows the request in its
request section, with how it ended — the stratum, the offset and the
delay, or the error:

```text
Requests
  1. QUERY ntp://time.example.com:123 -> stratum 2, offset -0.000412s, delay 0.0213s in 22ms
     Ntp-Version: 4
```

With [verbose self-metrics](SELF-METRICS.md#verbose-per-request-self-metrics)
a request is labelled `url="ntp://time.example.com:123"` and
`http_method="QUERY"`.

## Limitations

- **One server, one sample.** The offset is that of one request, with the
  error its delay allows, about half of it; a client such as chrony or ntpd
  filters many samples of several servers. Watch the offset over time, or
  against `delay_seconds`, rather than one scrape's.
- **No authentication.** Neither NTP's symmetric keys nor NTS is spoken;
  the random transmit timestamp keeps a third party that does not see the
  request from answering it, and no more.
- **No control queries.** What `ntpq` and `chronyc` read from a server's
  control port — its peers, its own offset from them — is not asked.
- **The exporter's clock is one side of the offset.** A scrape measures the
  host the exporter runs on against the server, not the server against
  the true time: watch a server against a reference you trust, or watch
  `stratum`, `leap_indicator` and the root distance it reports.
