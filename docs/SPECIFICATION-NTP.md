# Prometheus Universal Exporter — `ntp` Request Type Specification

This is the `ntp` request type's section of § 5.1 of the
[exporter specification](SPECIFICATION-EXPORTER.md), in a document of its
own so that a change to the type is a change to this document. Everything
the request types share — the registry and the rules every type is held to
(§ 5.1), building without a type, decoding, transforms, error policies,
limits, caching, static targets and exposition — is in the exporter
specification, and a section number here, such as § 42.14, is one of its
sections. What a user reads of the type is [NTP.md](NTP.md).

## `ntp`

An `ntp` collector sends one SNTP client request (RFC 4330, the packet of
NTPv4, RFC 5905) to the probe's `target` and hands the server's reply to
the transforms as a JSON document. It MUST need nothing outside Go's
standard library, and MUST be in the default build. Its keys are
`samples`, how many requests a scrape sends each server, from 1 to 8, 1
when left out, and `sample_interval`, how long apart, at least 1s, 2s when
left out, anything else refused at load; of the common part it takes
`allowed_targets` and `denied_targets`,
as for `http`, the host's name before the request and the address the UDP
socket is connected to before anything is sent (§ 26.1).

- A target MUST be `host`, `host:port` or `[v6]:port`, port 123 when left
  out; an IPv6 address without brackets MUST be taken whole as the host,
  on port 123, as every `host[:port]` target is read. A scheme, path,
  query or user and a port outside 1–65535 MUST be
  refused, a probe's with `400` before anything is sent and a static
  target's at load. A target MAY name up to 8 servers by commas, each held
  to those rules; more, an empty one, and one named twice MUST be refused
  likewise, and the target lists MUST hold each. A probe MUST take
  `timeout` and `param_<name>` parameters, and a static target's request
  block `timeout`.
- A scrape MUST send each server `samples` requests, `sample_interval`
  apart, each as one request is below, waiting for the reply of each but
  the last until the next is due, and of the last until the deadline: one
  unanswered then is unanswered, and one that fails otherwise, or a reply
  that fails the scrape below, MUST end the server's requests with that
  failure. The server's answer MUST be the reply of the least delay, with
  `samples`, `samples_answered` and `jitter_seconds`, the population
  standard deviation of the answered offsets, in its document; a server
  that answered none MUST fail with the last request's failure. Each
  request MUST be one of a debug report and of an exported trace.
- A target of several servers MUST ask each at once, and its document
  MUST be of `servers`, each server's document in the target's order, a
  server that failed with its `host`, `port` and `error` alone;
  `servers_answered`; `offset_median_seconds` and `offset_spread_seconds`,
  the greatest offset less the least, of those that answered, left out when
  none did; and `duration_seconds`. It MUST fail only when no server
  answered, with the first's failure.
- A scrape MUST send one request to a server of one sample, of 48 bytes, version 4, mode 3, whose
  only field set besides is the transmit timestamp, which MUST be a random
  number and not the exporter's clock. Nothing MUST be sent again within
  the scrape. The scrape's deadline — the probe's `timeout`, the scrape's
  own, 30 seconds when neither is set — MUST bound the whole of it.
- A datagram MUST be taken as the reply only when it comes from the address
  the request was sent to, is at least 48 bytes, carries the request's
  transmit timestamp as its origin timestamp, is of mode 4 or 5 and of
  version 3 or 4, and has a transmit timestamp. Any other datagram MUST be
  ignored and the reply waited for; when the deadline comes first, the
  scrape MUST fail wrapping `context.DeadlineExceeded`, saying how many
  datagrams were ignored and why the last one was. A scrape cancelled
  before its deadline MUST fail wrapping `context.Canceled` and saying
  `before the scrape was cancelled`, not `before the deadline`. An error of the socket,
  such as the refusal of an ICMP port unreachable, MUST fail it at once.
- The reply MUST fail the scrape in the `ntp` stage, naming `host:port`,
  when it is a Kiss-o'-Death (stratum 0), naming its code; when its leap
  indicator is 3 or its stratum 16, saying the server is not synchronised;
  and when its stratum is past 16. A Kiss-o'-Death code RFC 5905 lists
  MUST be told to a probe's caller; any other, and a stratum past 16, are
  values of the answer and MUST be withheld from the caller (§ 19.4).
- The offset MUST be `((t2 - t1) + (t3 - t4)) / 2` and the delay
  `(t4 - t1) - (t3 - t2)`, or 0 when that is below 0, `t1` the exporter's wall clock when the request
  was sent and `t4` `t1` with the round trip timed by the monotonic clock
  added, each difference taken of the 64-bit NTP timestamps as a signed
  number, so that timestamps on either side of the end of an NTP era (2036)
  give the difference they are. The server's timestamps MUST be read as
  times in the era that puts them within 68 years of the exporter's clock.
- The answer MUST be a JSON document of `host`, `port`, `address` (the
  address the request was sent to), `offset_seconds`, `delay_seconds`,
  `version`, `leap_indicator`, `stratum`, `precision` and `poll` (log2
  seconds, as sent) with `precision_seconds` and `poll_seconds`,
  `root_delay_seconds`, `root_dispersion_seconds`, `reference_id`,
  `reference_time` and `transmit_time` in Unix seconds with their fraction
  and as RFC 3339 in UTC (`_rfc3339`), and `duration_seconds`. The reference
  id MUST be, of stratum 1, its printable ASCII without trailing NULs, or
  `0x` and its bytes in hexadecimal when they are not printable; of a higher
  stratum, a dotted IPv4 address. A reference timestamp of 0 MUST be given
  as `reference_time` 0 and `reference_time_rfc3339` `""`.
- The decoder MUST be `json`, as for `grpc`; another decoder, and a
  transform other than `jq`, `yq` and `python`, MUST be refused at load.
- A debug probe's request section MUST show `QUERY`, `ntp://host:port` and
  the version sent, with the stratum, offset and delay, or the error, as
  its outcome. The verbose `url` label MUST be `ntp://host:port`, of several
  servers each `host:port` by commas after `ntp://`, and `http_method` MUST
  be `QUERY`.
