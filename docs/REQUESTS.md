# Target requests

Everything on this page describes the request an `http` collector — one with
`request.type: http`, see [request types](CONFIGURATION.md#request-types) —
makes to the discovered target, not the scrape Prometheus makes of the exporter. Each setting
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

**Scope.** Placeholders are bound only in the collector's `request.path`. A
`path` probe parameter replaces that path wholesale and is used exactly as
given, so a `param_` parameter sent alongside it has nothing to fill and is
rejected. `{{` always opens a placeholder in `request.path`; anything that is not
a well-formed `{{param_<name>}}` or `{{param_<name>:<default>}}` stops the
exporter at startup, naming the collector.

**Environment variables.** Placeholders use `{{…}}` precisely so they never meet
the `${NAME}` references [`--config.export-env`](CONFIGURATION.md#environment-variables)
substitutes. The environment is read once, when the file is loaded; path
parameters are bound on every probe. The two compose, so a default can come from
the environment:

```yaml
path: /api/{{param_tenant:${DEFAULT_TENANT}}}/status
```

With `--config.export-env` off, that reference is left in the default
unexpanded, and the exporter refuses to start rather than bind `${DEFAULT_TENANT`
and leave a stray brace in the path.

**Caching and self-metrics.** Every probe parameter is part of the response cache
key, so two tenants never share a cached result. The verbose self-metrics label
a request with the placeholder, not the value —
`url="http://legacy.us.example:8080/api/{{param_tenant}}/v{{param_version:2}}/status"` —
because the value is a tenant or an account, which labels already keep out of
the query string, and one series per value would be unbounded.

**Scheduled targets.** A [scheduled target](OTLP.md) is scraped on the
exporter's own timer, with no probe to supply a value. Its own `request.path`
cannot use placeholders, and it can use a collector whose path has them only if
every placeholder has a default; otherwise the exporter refuses to start, naming
both.

From a Prometheus Operator monitor, the values go in `params` like any other
probe parameter:

```yaml
params:
  collector: [legacy_text]
  param_tenant: [acme]
```

## Redirects and HTTP/2

Two transport settings live on the collector request, both off by default:

```yaml
request:
  follow_redirects: false
  enable_http2: false
```

`follow_redirects` decides whether a redirect status on the target request is
followed. Left at `false`, the exporter returns the redirect response itself, so
the collector sees the 3xx status and — with the default `on_http_error: fail` —
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
behaviour never reads another scrape's cached result. Scheduled targets accept
both in their own `request` block.

These replace the earlier undocumented `request.redirect_policy`. A
configuration still setting it now fails to load with an unknown-field error
rather than silently changing behaviour.

## Retries

Retries can be configured in the collector and overridden for one scrape:

```yaml
request:
  retry:
    attempts: 2   # retries after the initial request
    backoff: 2s   # fixed delay between attempts
```

The exporter retries transport failures and transient HTTP responses (`408`,
`425`, `429`, and `5xx`). Other HTTP statuses are returned immediately. The
retry count and fixed delay can be overridden for one scrape with the
`retry_attempts` and `retry_backoff` probe parameters. Retries share the scrape/target
timeout, so the retry loop cannot extend the configured deadline indefinitely.

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
