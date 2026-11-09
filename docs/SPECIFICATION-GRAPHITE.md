# Prometheus Universal Exporter — `graphite` Request Type Specification

This is the `graphite` request type's section of § 5.1 of the
[exporter specification](SPECIFICATION-EXPORTER.md), in a document of its
own so that a change to the type is a change to this document. Everything
the request types share — the registry and the rules every type is held to
(§ 5.1), building without a type, decoding, transforms, error policies,
limits, caching, static targets and exposition — is in the exporter
specification, and a section number here, such as § 42.14, is one of its
sections. What a user reads of the type is [GRAPHITE.md](GRAPHITE.md).

## `graphite`

A `graphite` collector asks a Graphite render API — graphite-web, carbonapi or
anything answering `/render` alike — for the series of one or more Graphite
expressions. The probe's `target` is the Graphite server, required, and
checked as an `http` target is. The exporter MUST NOT accept pushed carbon
lines: it only pulls. Its keys:

| Key | Default | Rule |
| --- | --- | --- |
| `targets` | none | **Required**, at least one. Graphite expressions, each sent as a `target` query parameter. |
| `from` | `-15min` | The window's start, sent as `from`. Wide enough to hold a point of a series stored at one- or five-minute resolution. |
| `until` | `now` | The window's end, sent as `until`. |
| `path` | `/render` | As `http`'s. |
| `query`, `headers`, `basic_auth` / `basic_auth_file`, `bearer_token` / `bearer_token_file`, `login`, `forward_authorization`, `forward_headers`, `tls`, `retry`, `max_response_bytes`, `follow_redirects`, `redirect_trusted_hosts`, `enable_http2`, `new_connection`, `ip_family`, `allowed_schemes`, `accept_status`, `allowed_targets`, `denied_targets`, `rate_limit`, `proxy_url`, `proxy_basic_auth_file` | as `http` | With `http`'s rules. |

- The request MUST be `http`'s in everything but its URL — the same transport,
  credentials, retries, limits and proxy — and a `GET`; `method` and `body`
  MUST be rejected. The method MUST be left unset in the configuration, so a
  configuration validated again does not read as one that sets it.
- The URL MUST be the target with `path`, `query`, then every expression as a
  `target` parameter, `from`, `until` and `format=json`. `request.query` MUST
  NOT set `target`, `from`, `until` or `format`, in any case. A pair of the
  target's own query naming one of the four, in any case and however the name
  is escaped, MUST be dropped, so a probe's target can neither add an
  expression nor change the window or the format; a pair in which a `;` sets
  one MUST be dropped too.
- When the expressions a request carries, as configured and URL-encoded with
  `&target=` each, exceed 2048 bytes, the request MUST instead be a `POST` of
  the same parameters as an `application/x-www-form-urlencoded` body, with no
  query in the URL, retried as a `GET` would be. The `http_method` label MUST
  say which method, decided by the expressions alone, so it never changes
  between scrapes of one collector or static target.
- Every expression MUST be checked at load: not empty, no control character,
  quotes closed and brackets balanced, a quoted bracket not counted and a
  backslash inside quotes escaping the character after it; an expression
  listed twice, surrounding whitespace aside, MUST be refused naming both.
  Graphite functions MUST NOT be checked, since render APIs differ in them.
  `from` and `until` MUST NOT hold whitespace.
- An expression MAY hold `{{param_<name>}}` placeholders (§ 42.10a), filled
  as in the path, defaults, missing and unused parameters included. A value
  MUST hold only letters, digits and `_ - . : @ % + ~`; any other value MUST be
  refused with `400` before Graphite is contacted, since an expression has no
  escaping and a quote, comma, bracket or glob could change it.
- It MUST accept the `/probe` parameters `path`, `timeout`,
  `insecure_skip_verify`, `follow_redirects`, `enable_http2`, `new_connection`,
  `ip_family`, `retry_attempts`, `retry_backoff`, `header_<name>`, `param_<name>`, and `from` and `until`,
  which replace the window for the probe; an empty one, or one holding
  whitespace, MUST be answered `400`, and so must either for a collector of
  another type. A static target using a `graphite` collector MAY set `path`,
  `timeout`, `insecure_skip_verify`, `follow_redirects`, `enable_http2`,
  `new_connection`, `ip_family`, `retry`, `headers`, the basic and bearer credential keys, `accept_status`,
  `login`, `rate_limit`, and `targets`, `from` and `until`, which replace the collector's. A static target's
  `targets` MUST be checked like the collector's, repeats included, and MUST
  NOT hold placeholders. Its `from` and `until` MUST enter its cache key as
  the probe parameters of the same name, so it shares cached results with a
  probe asking for the same window; its `targets`, which no probe can set,
  MUST enter it in a section a probe's key never has, so two targets asking
  one server for different series never share a cached result, and a probe
  never reads one.
- `decoder.type` left unset or `auto` MUST be `graphite` (§ 15a); `json`
  MAY be set to read the answer as Graphite sent it.
