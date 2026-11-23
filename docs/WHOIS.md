# WHOIS

The `whois` request type reads a domain's registration from its registry's
WHOIS server (RFC 3912) and hands it to the transforms as a JSON document:
when the registration expires, its registrar, its status and its name
servers. Use it to alert before a domain expires, and to see a registrar or
a name server change.

The type is opt-in: the default build and the published image leave it out,
so a build carries it only when asked
([Building with whois](#building-with-whois)).

## Building with whois

`OPT_IN_REQUEST_TYPES` names the opt-in types a build adds to the types it
carries, comma-separated:

```sh
make build OPT_IN_REQUEST_TYPES=whois              # bin/prometheus-universal-exporter
docker build --build-arg OPT_IN_REQUEST_TYPES=whois -t exporter:whois .
go build -tags "$(sh tools/request-type-tags.sh '' '' whois)" .
```

The tag is `request_type_whois`. A configuration with a `whois` collector
is refused by a build without the type, at startup, on a reload and by
`--dry-run`, saying how to build one that has it.

## A collector

```yaml
collectors:
  - name: domain
    cache:
      ttl: 12h
    request:
      type: whois
    transform:
      type: jq
    metrics:
      - name: domain_expiry_timestamp_seconds
        description: When the domain's registration expires
        expression: .expires_timestamp_seconds
        labels:
          - name: registrar
            expression: .registrar
      - name: domain_registered
        description: Whether the registry knows the domain
        expression: .registered
```

```text
/probe?collector=domain&target=example.com
```

**Cache the collector for hours.** Registries limit how often one address
may ask, some to a few questions a day, and answer an address that asks
more with an error, an empty answer or nothing; a registration changes
rarely. `cache.ttl` keeps one answer for every scrape within it
([Response caching](CONFIGURATION.md#response-caching)).

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `whois`. |
| `whois_server` | — | The WHOIS server asked, `host` or `host:port`, port 43 when it names none (see [The server asked](#the-server-asked)). |
| `query_prefix` | — | Text written before the domain in the question, at most 64 characters, for a server that wants more than the name: `"-T dn,ace "` for DENIC (`.de`). |
| `max_response_bytes` | `10MiB` | The most bytes of one answer. |
| `allowed_targets`, `denied_targets` | — | The hosts the collector may and may not reach, as for [any type that reaches a host](REQUESTS.md#restricting-targets), checked on each WHOIS server a question goes to, not on the domain. |

No other request key applies, and setting one is a startup error. A probe
takes `timeout` and `param_<name>` parameters for the collector's
[label values](REQUESTS.md#in-label-values).

The decoder is `json`, whether `decoder.type` is left out or set to it; any
other decoder, and a transform that does not read JSON, is refused when the
configuration loads. Use `jq`, `yq` or `python`.

## The target

The target is the domain, `example.com`, as it is registered: the name
under its top-level domain, not a host under it such as `www.example.com`,
which a registry does not know. An internationalised name is written in
its ASCII form, `xn--bcher-kva.example` for `bücher.example`. A scheme, a port, a path, a user or an IP address is
refused with `400` before anything is sent, a static target's when it
loads.

## The server asked

Left to itself, a scrape asks IANA's server, `whois.iana.org`, which
server answers for the domain's top-level domain, and asks that one for the
domain. What IANA said of a top-level domain is kept for a day, so a scrape
after the first asks the registry alone.

`whois_server` names the server and skips IANA: for a registry IANA lists
no server for, a registrar's server with more detail, or a mirror of your
own. A collector with it serves the domains of that server alone.

Each question is one TCP connection: the domain and a line end are sent,
and the answer is read until the server closes.

## The document

```json
{
  "domain": "example.com",
  "server": "whois.verisign-grs.com:43",
  "referred_by": "whois.iana.org:43",
  "registered": true,
  "registrar": "Example Registrar, Inc.",
  "registrar_iana_id": "376",
  "created": "1995-08-14T04:00:00Z",
  "created_timestamp_seconds": 808372800,
  "updated": "2026-08-14T07:01:34Z",
  "updated_timestamp_seconds": 1786690894,
  "expires": "2027-08-13T04:00:00Z",
  "expires_timestamp_seconds": 1818129600,
  "status": ["clientTransferProhibited"],
  "name_servers": ["a.iana-servers.net", "b.iana-servers.net"],
  "dnssec": "signedDelegation",
  "fields": {"domain name": ["EXAMPLE.COM"], "registry expiry date": ["2027-08-13T04:00:00Z"]},
  "text": "   Domain Name: EXAMPLE.COM\r\n...",
  "duration_seconds": 0.31
}
```

WHOIS has no format: each registry writes its own keys and its own dates.
The document is what could be read of the answer.

- `expires`, `created` and `updated` are the dates, in RFC 3339 in UTC,
  with each as Unix seconds beside it, read from the keys registries write
  them under (`Registry Expiry Date`, `Expiry date`, `paid-till`,
  `expires`, `[Expires on]`, …) and the forms they write them in. A date
  that names no zone is taken as UTC.
- **A date that could not be read is `null`**, and a rule that reads it
  fails as any rule over a missing value does, rather than the domain
  looking as if it never expired. Some registries publish no expiry date
  at all (`.de`, `.eu`, `.nl` among them). A date of numbers that begins
  with the day, `03/04/2027`, is not guessed at.
- `registered` is false when the answer holds no date, registrar or name
  server and says the registry knows no such domain (`No match`,
  `NOT FOUND`, …). Such an answer is not a failure.
- `status` is the status values, each without the URL that may follow it,
  and `name_servers` the name servers, in lower case.
- `fields` is every `key: value` line of the answer by its key in lower
  case, a key written several times with each of its values, up to the
  line that says when the database was last updated. Lines written under a
  heading, further in, are the heading's values, and a field under one is
  under its own key and under the two together: the `Organization` under
  a `Registrar` heading is `organization` and `registrar organization`. A rule reads what the
  document has no field for from it: `.fields["registrant country"][0]`.
- `text` is the answer as it came, for a `python` transform or a `jq`
  `capture` to read what no line holds as a key and a value.
- `server` is the server that answered, and `referred_by` the server that
  named it, empty with `whois_server`.

## Static targets

A [static target](STATIC-TARGETS.md) of a `whois` collector names the
domain, and may set `timeout` under its `request`. Give the scrapes an
interval of hours, as the collector's cache has:

```yaml
interval: 12h
targets:
  - name: example_com
    collector: domain
    target: example.com
```

## Limits

A scrape is bounded by its deadline, the probe's `timeout` or the scrape's
own, 30 seconds when neither is set, which covers both questions. An answer
larger than the response limit, `request.max_response_bytes` or
`limits.max_response_bytes`, fails the scrape. The document holds at most
256 keys in `fields`, and 32 values of one key.

## Errors and self-metrics

A failed request is reported in the `whois` stage and follows
`error_handling.on_fetch_error`, naming the server: one that cannot be
reached, one that closed the connection without an answer, an answer past
the response limit, or a top-level domain IANA names no server for
(`whois.iana.org:43 names no WHOIS server for .example; set
request.whois_server to the registry's, if it has one`). A host the target
lists refuse counts in `http_exporter_targets_refused_total`, answered
`403`. Transforms see no status (`$status` is `null`, Python's
`response.status_code` `None`) and the header
`Content-Type: application/json`.

A [debug probe](CONFIGURATION.md#debug-probes) shows each question with how
it ended:

```text
Requests
  1. QUERY whois://whois.iana.org:43/com -> 2381 bytes in 95ms
  2. QUERY whois://whois.verisign-grs.com:43/example.com -> 3254 bytes in 210ms
```

With [verbose self-metrics](SELF-METRICS.md#verbose-per-request-self-metrics)
a request is labelled `url="whois://example.com"` and `http_method="QUERY"`.

## Limitations

- **WHOIS, not RDAP.** The structured successor of WHOIS is JSON over
  HTTPS, which an `http` collector reads as it is:
  `https://rdap.org/domain/example.com`. Prefer it for the registries that
  have it; `whois` is for those that do not, and for one way of asking
  them all.
- **The registry is asked, not the registrar.** A registry that refers to
  its registrar's server for the details is not followed there; name that
  server with `whois_server` to ask it.
- **Rate limits are the registry's.** An answer that says the limit was
  exceeded reads as a registration with no dates: the rule over the date
  fails, and `text` says why.
- **Domains only.** Addresses, networks and AS numbers are not asked for.

## Troubleshooting

- **`expires_timestamp_seconds` is `null`.** Run a
  [debug probe](CONFIGURATION.md#debug-probes) and read `text`: the
  registry publishes no expiry date, limits the exporter's rate, or writes
  the date under a key or in a form the type does not read, which
  `fields` or `text` and a rule of your own can.
- **`registered` is false for a domain that exists.** The target is a host
  under the domain, or the server asked is not the registry's.
- **The registry closes the connection without an answer.** It limits the
  exporter's address; raise `cache.ttl` and the interval.
