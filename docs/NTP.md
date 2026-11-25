# NTP

The `ntp` request type asks an NTP server for the time, as an SNTP client
does, and hands its reply to the transforms as a JSON document. The
document says how far the exporter's clock is from the server's, how long
the round trip took, and what the server says of itself: its stratum, leap
indicator, precision, root delay and dispersion, and the clock or server
that it follows. Use it to watch, from where the exporter runs:

- that a time server answers, and keeps the time: a server that lost its
  upstream says so, and fails the scrape;
- how far a host's clock, the exporter's own, is from a server it should
  agree with;
- which stratum a server is at, and which reference it follows.

It complements node_exporter's `timex` collector, which reports what the
kernel of its own host thinks of its synchronisation. `ntp` asks a server
over the network, any server that the exporter can reach, and measures the
offset itself.

One request is sent, in one UDP datagram, as RFC 4330 (SNTP) and RFC 5905
(NTPv4) describe. With `samples`, a few are sent, and the one with the least
delay is the answer. A target may name several servers, asked at the same
time, and the document compares their offsets.

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
| `samples` | `1` | How many requests a scrape sends each server, from 1 to 8 (see [Several samples](#several-samples)). |
| `sample_interval` | `2s` | How long apart they are sent, at least `1s`. |
| `allowed_targets`, `denied_targets` | — | The hosts the collector may and may not reach, as for [any type that reaches a host](REQUESTS.md#restricting-targets), checked on the address the request is sent to. |

The request is an NTPv4 client request, whatever the server. No other request key applies, and setting one —
`path`, `headers`, `tls` — is a startup error. A probe takes `timeout` and
`param_<name>` parameters for the collector's
[label values](REQUESTS.md#in-label-values); a static target's request block
takes `timeout`.

The decoder is `json`, whether `decoder.type` is left out or set to it. Any
other decoder, and a transform that does not read JSON (`regex`, `csv`,
`css`, `prometheus`), is refused when the configuration loads; use `jq`,
`yq` or `python`.

## The target

The target is `host:port`, or `host` alone for port 123, with an IPv6
address in brackets: `time.example.com`, `10.0.0.1:123`,
`[2001:db8::1]:123`.

- A scheme, a path or a user is refused with `400` before anything is sent,
  and a static target's when it loads.
- An IPv6 address without brackets is taken as a whole address, on port
  123, as for every type whose target is `host` or `host:port`.
  `2001:db8::1:123` is therefore the address `2001:db8::1:123`, since that
  is a valid address, not `2001:db8::1` on port 123, which is written
  `[2001:db8::1]:123`.
- A name is resolved when the request is sent, and the request goes to its
  first address.
- A target may name up to 8 servers, separated by commas and none twice:
  `time1.example.com,time2.example.com:123`. See
  [Several servers](#several-servers).

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

- `offset_seconds` is how far the server's clock is ahead of the exporter's,
  negative when it is behind: `((t2 - t1) + (t3 - t4)) / 2`. Here `t1` is
  the time the request was sent and `t4` the time the reply came, by the
  exporter's clock, and `t2` and `t3` are the times the server received the
  request and sent the reply, by its own clock.
- `delay_seconds` is the round trip without the time that the server held
  the request, `(t4 - t1) - (t3 - t2)`, and 0 when that is below 0. A server
  whose clock reads more coarsely than the round trip, such as one of
  precision -6 (15.6ms) on a LAN, can say that it held the request longer
  than the round trip took. RFC 5905 floors the delay at the precision of
  the local clock; the exporter's clock reads to the nanosecond, so its
  floor is 0.
- `t1` is the exporter's wall clock when the request is sent, and `t4` is
  `t1` plus the round trip timed by the monotonic clock, so that a step of
  the wall clock during the round trip changes neither. The differences of
  the 64-bit NTP timestamps are taken as signed numbers, as RFC 5905 takes
  them, so a timestamp of the era that starts on 2036-02-07 is read
  correctly beside one of the era before it.
- `version` is the reply's, 3 or 4; `leap_indicator` is 0, or 1 or 2 when a
  leap second is to be added or removed at the end of the day; `stratum` is
  from 1, a server with a reference clock of its own, to 15.
- `precision` and `poll` are the exponents the server sent, log2 seconds,
  and `precision_seconds` and `poll_seconds` the seconds they stand for.
- `root_delay_seconds` and `root_dispersion_seconds` are the server's round
  trip to its reference clock and the error that it may have gathered on the
  way. Half the first plus the second is its root distance: how far from the
  reference its time may be.
- `reference_id` is:
  - for a stratum 1 server, the code of its reference clock, such as `GPS`
    or `PPS`, or its bytes in hexadecimal (`0x…`) when they are not
    printable text;
  - for a higher stratum, the IPv4 address of the server that it follows,
    or, for one that it reaches over IPv6, the first four bytes of a hash of
    that address, which also reads as an IPv4 address.
- `reference_time` is when the server's clock was last set, and
  `transmit_time` is when it sent the reply, as Unix seconds with their
  fraction, with RFC 3339 times in UTC beside them.
  `.transmit_time - .reference_time` is how long ago the server last heard
  from its reference. A server that has never set its clock sends no
  reference time; `reference_time` is then 0 and `reference_time_rfc3339`
  empty.
- `address` is the address the request was sent to; `duration_seconds` is
  how long the scrape took, the name's resolution included.

## The request and its reply

The request is an NTPv4 client request of 48 bytes, in which nothing is set
but its version, its mode and its transmit timestamp. The transmit
timestamp is a random number, not the exporter's clock. The server sends it
back as the reply's origin timestamp, which tells the reply to this request
apart from any other datagram, and nothing about the exporter's clock is
told to the server.

The exporter waits for the reply until the scrape's deadline, and reads only
datagrams from the address that the request went to. A datagram is not the
reply, and is ignored, when:

- it is shorter than 48 bytes;
- its origin timestamp is not the request's;
- it is not a server's (mode 4, or 5);
- it is of an NTP version other than 3 or 4;
- or it has no transmit timestamp.

Anyone who can send to the exporter may forge such a datagram, so it does
not end the scrape: the reply may still come. When the deadline comes first,
the scrape fails, saying how many datagrams were ignored and why the last
one was:

```text
no reply from 192.0.2.10:123 before the deadline; 1 datagram was ignored, as its origin timestamp is not the request's: context deadline exceeded
```

A scrape cancelled before its deadline, as when every probe waiting for it
has gone away, says `before the scrape was cancelled` instead, and
`context canceled`.

Nothing is sent again: a server that limits its clients' rate answers a
client that asks twice in quick succession with a Kiss-o'-Death instead of
the time. One request is made a scrape, `samples` with that key, and the
deadline, the probe's `timeout` or the scrape's own, 30 seconds when
neither is set, bounds it.

**A server that does not keep the time fails the scrape.** The reply is the
server's, but its offset would be to a clock that nobody vouches for, and a
rule over `offset_seconds` would alert on that clock instead of saying why.
These cases therefore fail the scrape at once, in the `ntp` stage:

- a Kiss-o'-Death, a reply of stratum 0, named by its code:
  `time.example.com:123 answered a Kiss-o'-Death, RATE: rate exceeded: the
  server asks to be asked less often`. `DENY` and `RSTR` say the server
  refuses the exporter; RFC 5905 lists the others.
- leap indicator 3, the alarm of a server whose clock is not synchronised,
  and stratum 16, which says the same: `time.example.com:123 is not
  synchronised: its leap indicator is 3, the alarm condition`.
- a stratum past 16, which no server has.

A server that is not synchronised is reported by the scrape failing (the
collector's series absent, and `http_exporter_scrape_success_total` not
counting it), as for a server that does not answer, not by a field that a
rule must know to look at.

## Several samples

One request's offset is off by up to half its round trip, and a round trip
a queue somewhere lengthened makes it worse. `samples` sends a server that
many requests, `sample_interval` apart, and takes the reply of the least
delay as its answer, as NTP's clock filter does:

```yaml
    request:
      type: ntp
      samples: 4
      sample_interval: 2s
    metrics:
      - name: ntp_offset_seconds
        expression: .offset_seconds
      - name: ntp_jitter_seconds
        expression: .jitter_seconds
```

- The document is that reply's, with `samples`, how many requests were
  sent, `samples_answered`, how many were answered, and `jitter_seconds`,
  the population standard deviation of their offsets, 0 of one.
- A request that is not answered before the next is due is unanswered, and
  the next is sent; the last waits until the scrape's deadline. A server
  that answers a Kiss-o'-Death, or is not synchronised, ends its samples
  and fails as one request does, and so does one that answers none.
- The requests are `sample_interval` apart, at least `1s`, which a server
  that limits its clients' rate allows: a scrape takes about
  `(samples - 1) × sample_interval`, which must fit its timeout.
- A debug probe shows each request, and an exported trace has a span of
  each.

## Several servers

A target of several servers, by commas, asks each at once, each with its
`samples`, and the document compares them, so one that disagrees with the
others shows:

```text
/probe?collector=clock&target=time1.example.com,time2.example.com,time3.example.com
```

```json
{
  "servers": [
    {"host": "time1.example.com", "port": 123, "offset_seconds": 0.0012, "delay_seconds": 0.011, "stratum": 2, "samples": 1, "samples_answered": 1, "jitter_seconds": 0, "...": "..."},
    {"host": "time2.example.com", "port": 123, "offset_seconds": 0.0009, "...": "..."},
    {"host": "time3.example.com", "port": 123, "offset_seconds": 0, "stratum": 0, "...": "...", "error": "time3.example.com:123 is not synchronised: its stratum is 16"}
  ],
  "servers_answered": 2,
  "offset_median_seconds": 0.00105,
  "offset_spread_seconds": 0.0003,
  "duration_seconds": 0.02
}
```

- `servers` are each server's document, as one server's is, in the
  target's order; one that answered no time has its `host`, `port` and
  `error`, and its other fields empty.
- `servers_answered` is how many answered, and `offset_median_seconds` and
  `offset_spread_seconds`, the greatest offset less the least, are of
  theirs, left out when none did. The scrape fails only when no server
  answers, with the first's failure.
- A rule over a server reads `.servers[]`, labelled by `.host`:

```yaml
      - name: ntp_server_offset_seconds
        items: .servers[] | select(.error == null)
        expression: .offset_seconds
        labels:
          - name: server
            expression: .host
      - name: ntp_servers_offset_spread_seconds
        expression: .offset_spread_seconds
```

The verbose `url` label names every server, `ntp://time1.example.com:123,time2.example.com:123`.

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
`error_handling.on_fetch_error`, naming the server. Causes include no reply
before the deadline, a refusal that the network sent back
(`reading the reply from 192.0.2.10:123: ... connection refused`), a
Kiss-o'-Death, and a server that is not synchronised.

What a probe's caller is told about a failure follows
[the rules of every type](CONFIGURATION.md#what-a-failed-probes-caller-is-told):
a Kiss-o'-Death's code is told when RFC 5905 lists it, and withheld
otherwise, as a value of the server's answer. A host that the target lists
refuse counts in `http_exporter_targets_refused_total` and is answered
`403`. Transforms see no status (`$status` is `null`, and Python's
`response.status_code` is `None`) and the header
`Content-Type: application/json`.

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

- **A few samples, not a clock filter.** The offset is that of the request
  with the least delay, out of at most 8 per scrape, with the error that its
  delay allows, about half of it. A client such as chrony or ntpd filters
  many samples over hours. Watch the offset over time, or against
  `delay_seconds`, instead of one scrape's.
- **No authentication.** Neither NTP's symmetric keys nor NTS is spoken;
  the random transmit timestamp keeps a third party that does not see the
  request from answering it, and no more.
- **No control queries.** What `ntpq` and `chronyc` read from a server's
  control port — its peers, its own offset from them — is not asked.
- **The exporter's clock is one side of the offset.** A scrape measures the
  exporter's host against the server, not the server against the true time.
  Watch a server against a reference that you trust, or watch `stratum`,
  `leap_indicator` and the root distance that it reports.
