# Target requests

Everything on this page describes the request an `http` collector — one with
`request.type: http`, see [request types](CONFIGURATION.md#request-types) —
makes to the discovered target, and a `graphite` collector's too, which is the
same request with a URL built for the Graphite render API
([Graphite](GRAPHITE.md)), not the scrape Prometheus makes of the exporter. Each setting
lives on a collector's `request` block, and most can be overridden for a single
scrape through a `/probe` query parameter — which is what a monitor's `params`
map renders into.

## The request URL

The URL requested is the probe's `target` with the collector's `request.path`
joined onto it and `request.query` merged into it.

A target without a scheme is `http`. That is the normal case rather than an
exception: Prometheus service discovery produces `__address__` as a bare
`host:port`, and the chart's monitors pass it through as the target. So
`10.0.0.5:8080`, `legacy.example:8080` and `[fd00::5]:9000` all mean `http://`.
A target that needs HTTPS says so, `https://secure.example:8443`, or a
relabeling rule adds the scheme. `allowed_schemes` applies to the result, so a
collector that allows only `https` rejects a bare target instead of upgrading
it.

`request.path` is optional. Without it, and without a `path` probe parameter,
the target is requested exactly as given: `http://legacy.example:8080` requests
`/`, and `http://legacy.example:8080/api/status` requests `/api/status`. A target
that carries a path keeps it, and `request.path` is appended after it. Nothing
warns about a missing path — a target that serves nothing at `/` fails the probe
with its own status, or returns a page the metric rules cannot read.

## Path parameters

A collector's `request.path` can carry placeholders the scrape fills in, so one
collector serves targets whose paths differ only by a value the scrape knows — a
tenant, a region, an API version:

```yaml
collectors:
  - name: legacy_text
    request:
      type: http
      path: /api/{{param_tenant}}/v{{param_version:2}}/status
```

```text
/probe?target=http://legacy.us.example:8080&collector=legacy_text&param_tenant=acme
  -> GET http://legacy.us.example:8080/api/acme/v2/status
```

A placeholder is `{{param_<name>}}`, and it is filled by the probe parameter of
exactly the same name, `param_<name>`. Names are letters, digits and
underscores after `param_`. The prefix keeps them out of the way of the probe's
own parameters — `target`, `collector`, `path`, `method` and the rest — so no
name is off limits.

**Defaults.** Anything after a colon is the default: `{{param_version:2}}` binds
`2` when the probe does not supply `param_version`. The default is optional. A
placeholder with no default that the probe does not supply fails the probe with
`400 Bad Request` naming the parameter, before the target is contacted — a
request sent with the placeholder unfilled would reach a path nobody configured.
An empty value (`param_tenant=`) counts as not supplied, so it takes the
default, or fails if there is none. An explicit empty default, `{{param_suffix:}}`,
is allowed and binds nothing.

**Escaping.** A value is always one path segment. It is escaped, so `a/b` is
sent as `a%2Fb` rather than becoming two segments, and `?`, `#`, spaces and
non-ASCII characters are escaped likewise. The values `.` and `..` are rejected
with `400`, since a server resolving them would serve a different path than the
one configured. Defaults are escaped the same way.

**Mistakes are errors, not fallbacks.** A `param_` parameter the collector's path
does not use is rejected with `400`, and so is one given twice. The first is
almost always a misspelling: with `{{param_tenant:acme}}`, a probe sending
`param_tenat=globex` would otherwise succeed against the default tenant and
report `acme`'s numbers as `globex`'s.

**Scope.** Placeholders are bound in the collector's `request.path` and, for
an `http` collector, in its [body, header values and query
values](#in-the-body-headers-and-query). A `path` probe parameter replaces the
path wholesale and is used exactly as given, and a `body` probe parameter
replaces the body the same way, so a `param_` parameter only they would have
used has nothing to fill and is rejected. `{{` always opens a placeholder in
`request.path`; anything that is not a well-formed `{{param_<name>}}` or
`{{param_<name>:<default>}}` stops the exporter at startup, naming the
collector.

**Environment variables.** Placeholders use `{{…}}` precisely so they never meet
the `${NAME}` references [`--config.expand-env`](CONFIGURATION.md#environment-variables)
substitutes. The environment is read once, when the file is loaded; path
parameters are bound on every probe. The two compose, so a default can come from
the environment:

```yaml
path: /api/{{param_tenant:${DEFAULT_TENANT}}}/status
```

With `--config.expand-env` off, that reference is left in the default
unexpanded, and the exporter refuses to start rather than bind `${DEFAULT_TENANT`
and leave a stray brace in the path.

**Caching and self-metrics.** Every probe parameter is part of the response cache
key, so two tenants never share a cached result. The verbose self-metrics label
a request with the placeholder, not the value —
`url="http://legacy.us.example:8080/api/{{param_tenant}}/v{{param_version:2}}/status"` —
because the value is a tenant or an account, which labels already keep out of
the query string, and one series per value would be unbounded.

**Static targets.** A [static target](STATIC-TARGETS.md) is scraped on the
exporter's own timer, with no probe to supply a value, so it gives its values
under `params`:

```yaml
targets:
  - name: acme_checkout
    collector: graphql_status
    target: https://api.example
    params:
      param_tenant: acme
      param_service: checkout
```

Every placeholder of the collector's request must then be filled, by `params`
or by a default, and every entry of `params` must fill one; otherwise the
exporter refuses to start, naming the target, the collector and the parameter.
The target's own `request` block — its `path`, `body` and `headers` — is
literal and cannot use placeholders.

From a Prometheus Operator monitor, the values go in `params` like any other
probe parameter:

```yaml
params:
  collector: [legacy_text]
  param_tenant: [acme]
```

### In the body, headers and query

The same placeholders work in an `http` collector's `request.body`, in the
values of `request.headers` and in the values of `request.query`, filled by the
same probe parameters, with the same defaults, and refused with the same `400`
when one is missing or unused. One collector can then speak to a GraphQL or
JSON-RPC endpoint, or an API that takes its tenant in a header:

```yaml
collectors:
  - name: graphql_status
    request:
      type: http
      method: POST
      path: /graphql
      headers:
        Content-Type: application/json
        X-Tenant: "{{param_tenant}}"
      query:
        region: "{{param_region:eu}}"
      body: '{"query": "query($s: String!, $n: Int!) { status(service: $s, limit: $n) { up } }", "variables": {"s": {{param_service|json}}, "n": {{param_limit:10|number}}}}'
```

```text
/probe?target=https://api.example&collector=graphql_status&param_tenant=acme&param_service=checkout
  -> POST https://api.example/graphql?region=eu
     X-Tenant: acme
     {"query": "…", "variables": {"s": "checkout", "n": 10}}
```

**Each place has its encoding.** A value is written the way its place needs,
so a probe parameter fills a value in rather than changing the request's
structure:

| Where | Written as |
| --- | --- |
| Header value | As given; a value with a control character, such as a line break, is refused with `400`, so it can never end the header and start another. |
| Query value | Encoded as a query value: `us&x=1` stays one value and adds no parameter. |
| Body, `{{param_x\|json}}` | A JSON string, quoted and escaped: `a "b"` becomes `"a \"b\""`. |
| Body, `{{param_x\|number}}` | A JSON number, refused with `400` unless it is one: `12; DROP` is not. |
| Body, `{{param_x\|form}}` | URL form encoded, for `application/x-www-form-urlencoded` bodies. |
| Body, `{{param_x\|xml}}` | Escaped XML text, for SOAP and other XML bodies. |
| Body, `{{param_x}}` or `{{param_x\|raw}}` | Exactly as given. Use it only where the template itself is the structure and the value comes from your own monitors. |
| Graphite target | As given, and only letters, digits and `_ - . : @ % + ~`; anything else — a quote, a comma, a bracket, a glob — is refused with `400`, since Graphite expressions have no escaping. See [Graphite](GRAPHITE.md#placeholders). |

The filter follows the default, if there is one: `{{param_limit:10|number}}`.
Filters exist only in the body; a header or query value has one encoding and
always gets it, so a filter there stops the exporter at startup.

**Braces of the body's own.** In a body, a header value or a query value,
`{{` opens a placeholder only when `param_` follows it, since a body may well
contain braces of its own; `{{ param_x }}` with spaces is refused at startup
rather than sent as text. Header names and query names cannot hold
placeholders.

The values are part of the response cache key like every probe parameter, and
the verbose self-metrics never carry them: the `url` label has no query string,
and the body and headers are not labels at all.

## Redirects and HTTP/2

Two transport settings live on the collector request, both off by default:

```yaml
request:
  follow_redirects: false
  enable_http2: false
```

`follow_redirects` decides whether a redirect status on the target request is
followed. Left at `false`, the exporter returns the redirect response itself, so
the collector sees the 3xx status and — with the default `on_fetch_error: fail` —
the probe fails. That is deliberate: a target that has moved is worth noticing
rather than quietly scraping somewhere else. Set it to `true` for endpoints that
legitimately redirect, such as an API whose documented host forwards to another.

`enable_http2` decides whether the target request may negotiate HTTP/2. HTTP/2
is negotiated through ALPN over TLS, so this only affects HTTPS targets;
cleartext HTTP/2 is never attempted. The default of `false` is the protocol the
exporter has always used.

Both are overridable per scrape:

```yaml
params:
  follow_redirects: ["true"]
  enable_http2: ["true"]
```

Each accepts exactly `true` or `false`; anything else returns HTTP 400 before
the target is contacted, so a typo cannot quietly fall back to a default. An
absent parameter leaves the collector's setting in force, and both parameters
are part of the response cache key, so a scrape asking for different transport
behaviour never reads another scrape's cached result. Static targets accept
both in their own `request` block.

These replace the earlier undocumented `request.redirect_policy`. A
configuration still setting it now fails to load with an unknown-field error
rather than silently changing behaviour.

## Connections

Connections to targets are kept and reused, so an HTTPS target pays for one TLS
handshake rather than one per scrape. Every collector and scrape with the same
TLS settings — `tls.ca_file`, `cert_file`, `key_file` and
`insecure_skip_verify` — and the same `enable_http2` shares one connection pool;
a scrape overriding either of the last two uses the pool of its own settings.
A certificate or key replaced on disk is picked up by the next request. An idle
connection is closed after 90 seconds, and a pool nothing has used for five
minutes, such as one a reload left behind, is closed with it. OTLP exports keep
their connection to the collector the same way.

### Proxies

Target requests and OTLP exports go through the proxy the environment names,
as with curl: `HTTPS_PROXY` for `https` URLs, `HTTP_PROXY` for `http` ones, and
`NO_PROXY` for what to reach directly — a comma-separated list of hosts,
domains (`.internal.example` or `internal.example` covers every host under it),
IP addresses and CIDR ranges, optionally with a port, or `*` for everything.
The lower-case spellings work too. Requests to `localhost` and loopback
addresses always go direct. For an `https` URL the proxy is asked to tunnel the
connection (`CONNECT`), so TLS still runs end to end with the target and its
[TLS settings](#tls) apply unchanged.

```sh
HTTPS_PROXY=http://proxy.corp.example:3128 \
NO_PROXY=.svc,.cluster.local,10.0.0.0/8 \
  prometheus-universal-exporter --config.file=config.yaml
```

A proxy URL may carry credentials, `http://user:password@proxy:3128`, sent as
`Proxy-Authorization`. There is no proxy setting in the configuration: the
environment applies to the whole exporter, and is read once, when the exporter
first connects. In Kubernetes, set the variables through the chart's `env`,
with credentials from a Secret. `localfile` collectors read files and are not
affected.

## Retries

Retries can be configured in the collector and overridden for one scrape:

```yaml
request:
  retry:
    attempts: 2   # retries after the initial request
    backoff: 2s   # fixed delay between attempts
```

The exporter retries transport failures and transient HTTP responses (`408`,
`425`, `429`, and `5xx`). Other HTTP statuses are returned immediately.

Only requests whose method is idempotent are retried — `GET`, `HEAD`,
`OPTIONS`, `TRACE`, `PUT` and `DELETE` — since sending a `POST` or `PATCH`
again may repeat what it did. When a target is known to handle a repeated
`POST` safely, allow it:

```yaml
request:
  method: POST
  retry:
    attempts: 2
    non_idempotent: true   # retry this POST too
```

Without it, a `POST` collector with `retry.attempts` is logged as a
configuration warning at startup, and its failed requests are not retried. The
retry count and fixed delay can be overridden for one scrape with the
`retry_attempts` and `retry_backoff` probe parameters. Retries share the scrape/target
timeout, so the retry loop cannot extend the configured deadline indefinitely.

When the deadline, or a shutdown, cuts short the wait before a retry, the
probe still reports what the target last answered: a `503` stays a failed
`http_status` stage with the target's body in the log and
`http_exporter_scrape_http_status_code` at 503, and the error adds that the
probe ran out of its budget. After a connection that failed, the error is that
connection error, noting that the wait before retrying was cut short.

## TLS

The collector can configure target TLS verification and trust material:

```yaml
request:
  tls:
    ca_file: /etc/prometheus/tls/ca.crt
    cert_file: /etc/prometheus/tls/client.crt
    key_file: /etc/prometheus/tls/client.key
    insecure_skip_verify: false
```

For a one-off scrape, the `insecure_skip_verify` probe parameter overrides the
collector setting. Set it to `true` only for endpoints where certificate
verification is intentionally unavailable; it disables server certificate
verification and should not be used as a general workaround.

```yaml
params:
  insecure_skip_verify: ["true"]
```
