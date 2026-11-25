# Target requests

This page describes the request that an `http` collector (one with
`request.type: http`, see
[request types](CONFIGURATION.md#request-types)) makes to the discovered
target. It is not about the scrape that Prometheus makes of the exporter.

- A `graphite` collector makes the same request, with a URL built for the
  Graphite render API ([Graphite](GRAPHITE.md)).
- A `grpc` collector makes a gRPC call instead, under its own rules, which
  are described in [gRPC](GRPC.md). Its credentials, TLS keys and probe
  placeholders work as they do here.
- A token that the target issues at its own login, an OAuth 2.0 token of the
  client credentials grant, or one of a user's refresh token, is fetched
  with `request.login`. It is described with the other target credentials in
  [Logging in to a target](AUTHENTICATION.md#logging-in-to-a-target).

Each setting is on a collector's `request` block. Most can be overridden for
a single scrape through a `/probe` query parameter, which is what a
monitor's `params` map renders into.

## Contents

- [The request URL](#the-request-url)
  - [The Host header](#the-host-header)
  - [Compression and the response size](#compression-and-the-response-size)
- [Path parameters](#path-parameters)
  - [In the body, headers and query](#in-the-body-headers-and-query)
  - [In label values](#in-label-values)
  - [In expressions](#in-expressions)
  - [Dates and times](#dates-and-times)
  - [Since the last success](#since-the-last-success)
- [Redirects and HTTP/2](#redirects-and-http2)
  - [What a followed redirect carries](#what-a-followed-redirect-carries)
- [Restricting targets](#restricting-targets)
- [Connections](#connections)
  - [Address families](#address-families)
  - [Proxies](#proxies)
  - [A collector's own proxy](#a-collectors-own-proxy)
  - [Over a Unix socket](#over-a-unix-socket)
  - [FastCGI](#fastcgi)
- [Retries](#retries)
  - [Waiting as Retry-After asks](#waiting-as-retry-after-asks)
- [Accepting other statuses](#accepting-other-statuses)
- [Timings, TLS and the final URL](#timings-tls-and-the-final-url)
- [Uptime probes](#uptime-probes)
  - [WebSocket endpoints](#websocket-endpoints)
  - [Compared with blackbox_exporter](#compared-with-blackbox_exporter)
- [One connection per scrape](#one-connection-per-scrape)
- [Pagination](#pagination)
- [Chained requests](#chained-requests)
  - [The keys of a step](#the-keys-of-a-step)
  - [Values and placeholders](#values-and-placeholders)
  - [One target only](#one-target-only)
  - [The answer](#the-answer)
  - [Time, retries and the response limit](#time-retries-and-the-response-limit)
  - [Credentials, logins and cookies](#credentials-logins-and-cookies)
  - [What a chain does not take](#what-a-chain-does-not-take)
  - [Errors, self-metrics and the debug report](#errors-self-metrics-and-the-debug-report)
- [Rate limiting](#rate-limiting)
  - [Rate limits per host](#rate-limits-per-host)
- [TLS](#tls)

## The request URL

The URL requested is the probe's `target` with the collector's
`request.path` joined onto it and `request.query` added after the target's
own query.

**Scheme.** A target without a scheme is `http`. This is the normal case:
Prometheus service discovery produces `__address__` as a bare `host:port`,
and the chart's monitors pass it through as the target. `10.0.0.5:8080`,
`legacy.example:8080` and `[fd00::5]:9000` therefore all mean `http://`.

- A target that needs HTTPS says so, `https://secure.example:8443`, or a
  relabeling rule adds the scheme.
- A scheme comes before the path, the query and the fragment, so a `://`
  later in the target belongs to them:
  `app.example:8080/login?next=http://other/` has no scheme and is `http`.
- `allowed_schemes` applies to the result, so a collector that allows only
  `https` rejects a bare target instead of upgrading it.
- A static target's scheme is checked when the file loads, so a scheme that
  the collector does not allow stops the exporter instead of failing every
  scrape.
- `allowed_schemes` itself holds `http`, `https` or both, in any letter
  case. Any other entry (`htps`, `ftp`, `https://`, one with a space around
  it, an empty one) would refuse every target, and stops the exporter at
  startup instead.

**Host.** The target's host is an IP address or a name written in ASCII:
letters, digits, `.`, `-` and `_`. An internationalised name is written in
its `xn--` form, `xn--bcher-kva.example` for `bücher.example`. A target
whose host has any other character is refused with `403` before anything is
sent ([Restricting targets](#restricting-targets) explains why).

**Path.** `request.path` is optional. Without it, and without a `path` probe
parameter, the target is requested exactly as given:
`http://legacy.example:8080` requests `/`, and
`http://legacy.example:8080/api/status` requests `/api/status`.

- A target that has a path keeps it, and `request.path` is appended after
  it, with the target's escapes kept as written: `http://h/a%2Fb` with
  `path: /x` requests `/a%2Fb/x`, not `/a/b/x`.
- A trailing slash is kept, once: `path: /` requests the root, `/`, whether
  or not the target ends in `/`.
- Nothing warns about a missing path. A target that serves nothing at `/`
  fails the probe with its own status, or returns a page that the metric
  rules cannot read.

A path is only a path. It is joined onto the target and cleaned (`a//b` and
`a/../b` become `a/b` and `b`). It cannot hold a query or a fragment, which
would be sent escaped as `%3F` and `%23` and would request a path that does
not exist. `request.path: /status?format=json` is refused at startup. A
`path` probe parameter or a static target's `request.path` that holds `?` or
`#` is refused too, with a `400` or at load. Put query parameters under
`request.query`:

```yaml
request:
  type: http
  path: /status
  query:
    format: json
```

**Escapes in a path.** A `%` followed by two hexadecimal digits in
`request.path`, in a `path` probe parameter or in a static target's
`request.path` is an escape that you wrote, and is sent as written.

- `path: /api/v4/projects/group%2Fproject` requests exactly that. The `%2F`
  is neither decoded into a second segment nor escaped again into `%252F`.
- Everything else is escaped as a path requires: a space as `%20`, `é` as
  `%C3%A9`. A `%` that begins no escape is a percent sign and is sent as
  `%25`: `/100%/x` requests `/100%25/x`.
- To send the three characters `%2F` literally, write `%252F`.
- An escaped `.` or `..` (`%2E%2E`) is sent as written too and is not
  resolved as a parent directory.
- In the `path` probe parameter, the escape is what the exporter reads after
  it decodes the probe's own URL. A monitor therefore writes
  `path=/api/group%252Fproject` on the wire, which the `params` of a
  Prometheus scrape configuration do for it.

**The target's own query.** A query that the target carries is sent byte for
byte as it was written. A bare key stays bare (`?debug`), a pair with a `;`
in it stays (`?a=1;b=2`), and the pairs keep their order and their escapes
(`?z=a%20b&a=1`). Only what could not be sent at all is escaped, in place: a
space as `%20`, a double quote, `<`, `>` and characters outside ASCII.

`request.query` is appended after it, encoded, in name order. A name that
both carry is sent twice, the target's first: `http://h/x?format=xml` with
`query: {format: json}` requests `/x?format=xml&format=json`. Which of the
two a server reads is the server's choice, so leave the name out of one of
them. (A `graphite` collector's own parameters are the exception: see
[Graphite](GRAPHITE.md#a-collector).)

### The Host header

A `Host` under `request.headers`, or a static target's, is the host that the
request is sent with. A virtual host can therefore be reached through an IP
address or an ingress:

```yaml
request:
  type: http
  headers:
    Host: status.internal.example
```

```text
/probe?target=10.0.0.5:8080&collector=legacy_text
  -> GET http://10.0.0.5:8080/status   Host: status.internal.example
```

For HTTPS, the certificate is still checked against the address that the
target names; set [`tls.server_name`](#tls) to the virtual host too.

These are refused at startup, for a collector and a static target alike:

- a header value that holds a control character other than tab, such as a
  line break pasted into a token, because it could never be sent;
- a `bearer_token` or a `basic_auth` credential that holds one, and a
  `basic_auth` username that holds a colon, which a target would read as the
  end of the username
  ([Authentication](AUTHENTICATION.md#credentials-in-the-configuration));
- a header name that is not valid, such as one with a space;
- two names that are the same header in different letter case, `X-Tenant`
  and `x-tenant`, of which a scrape would otherwise send either one.

### Compression and the response size

The exporter itself asks every target for gzip and decompresses the answer,
so `Accept-Encoding` belongs to the exporter. One under `request.headers`, a
collector's or a static target's, is refused at startup, because with it set
Go would hand the decoder the compressed bytes. One that the probe carries
is never forwarded (Prometheus sends `Accept-Encoding: gzip` on every
scrape), and listing it in `forward_headers` is refused at startup.

`max_response_bytes` counts the answer as the decoder gets it, decompressed.

- An answer whose `Content-Length` is over the limit is refused before its
  body is read: `response size 5000 exceeds limit 1024`.
- An answer without a `Content-Length` is refused once reading passes the
  limit, `response size exceeds limit 1024`, because its size is then
  unknown.
- The `Content-Length` of a `HEAD` answer is the size of a body that it does
  not have, and is not held against it.
- There is no "no limit". The largest value that can be written,
  `9223372036854775807`, is held one byte lower and reads the whole answer.
- There is no limit under 1 KiB either. A size without a unit is a number of
  bytes, and `max_response_bytes: 10`, written for 10 MiB, would fail every
  scrape. A limit from 1 to 1023 bytes, this one or
  `limits.max_response_bytes`, is therefore refused at startup:

```text
collector "api" request.max_response_bytes is 10 bytes, under the least, 1KiB; a size without a unit is a number of bytes, so write the unit, such as 10MiB, or leave it out, or 0, for the default, 10MiB
```

`max_response_bytes` bounds the body. The response's status line and headers
have their own bound, 1 MiB, which no setting changes. A target that sends
more fails the scrape as a limit does (`the response headers are larger than
1048576 bytes, the most the exporter reads of a response's headers`) and is
not retried, because it would send them again.

Over HTTP/2 ([`enable_http2`](#redirects-and-http2)) the bound is the same,
but the error is that one, with its single attempt, only when the headers
arrive whole before they are found too large, as headers that repeat one
value do.

- Headers that pass the bound while more are still arriving make Go's HTTP/2
  client close the connection. What it then reports does not mention
  headers; it is what it reports for any answer that breaks the protocol.
- The exporter cannot tell the two cases apart, so it does not call it a
  limit. Every request that still waits for its answer on that connection
  gets the same error, whatever its own answer would have been.
- A request that gets this error is therefore retried once, as one of its
  [`retry.attempts`](#retries), on a connection made for it alone and closed
  after its answer. A request that only shared the connection gets its
  answer there, even when the request whose headers closed the connection is
  retried at the same moment.
- A request that gets the error a second time is not retried again, however
  many attempts are left. Its scrape fails as a failed request: `connection
  error: PROTOCOL_ERROR: the HTTP/2 connection was closed over what the
  target sent, as the connection before it was, so the request is not
  retried again; an answer whose headers are larger than 1048576 bytes, the
  most the exporter reads of a response's headers, ends this way over
  HTTP/2, so look at the size of the target's response headers first`.
- With `retry.attempts: 0`, the default, there is no retry, as for any
  failure, and the error says that the connection was closed `in answer to
  this request or to another on the same connection`. A collector that
  shares a target with one whose headers may be over the bound needs
  `retry.attempts` of at least 1 to get past it.

## Path parameters

A collector's `request.path` can carry placeholders that the scrape fills
in. One collector can then serve targets whose paths differ only by a value
that the scrape knows, such as a tenant, a region or an API version:

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

A placeholder is `{{param_<name>}}`, and it is filled by the probe parameter
of exactly the same name, `param_<name>`. Names are letters, digits and
underscores after `param_`. The prefix keeps them separate from the probe's
own parameters (`target`, `collector`, `path`, `method` and the rest), so no
name is reserved.

**Defaults.** Anything after a colon is the default: `{{param_version:2}}`
binds `2` when the probe does not supply `param_version`. The default is
optional.

- A placeholder with no default that the probe does not supply fails the
  probe with `400 Bad Request`, which names the parameter, before the target
  is contacted. A request sent with the placeholder unfilled would reach a
  path that nobody configured.
- An empty value (`param_tenant=`) counts as not supplied, so it takes the
  default, or fails if there is none.
- An explicit empty default, `{{param_suffix:}}`, is allowed and binds
  nothing.

**Escaping.** A value is always one path segment.

- It is escaped, so `a/b` is sent as `a%2Fb` and does not become two
  segments. `?`, `#`, spaces and non-ASCII characters are escaped in the
  same way.
- A `%` in a value is always a percent sign, so `50%2F` is sent as
  `50%252F`. An escape written in the path around the placeholder is
  [sent as written](#the-request-url).
- The values `.` and `..` are rejected with `400`, because a server that
  resolves them would serve a different path from the one configured.
- Defaults are escaped in the same way, and a default of `.` or `..` stops
  the exporter at startup, because no probe could use it.
- A `localfile` or `sshfile` path holds a default to the same rules as a
  value, so a default with `/` or `\` in it
  [stops the exporter too](LOCALFILE.md#which-file-is-read).

**Mistakes are errors, not fallbacks.** A `param_` parameter that the
collector does not use, in its path or in another place that takes
placeholders, is rejected with `400`. So is one given twice. The first is
almost always a misspelling: with `{{param_tenant:acme}}`, a probe that
sends `param_tenat=globex` would otherwise succeed against the default
tenant and report `acme`'s numbers as `globex`'s.

**Scope.** Placeholders are bound in the collector's `request.path`, for an
`http` collector in its
[body, header values and query values](#in-the-body-headers-and-query), and
in the collector's [fixed label values](#in-label-values).

- A `path` probe parameter replaces the whole path and is used exactly as
  given, and a `body` probe parameter replaces the body in the same way. A
  `param_` parameter that only they would have used then has nothing to fill
  and is rejected. One that also fills a label value still has that to fill,
  and is accepted.
- `{{` always opens a placeholder in `request.path`. Anything that is not a
  well-formed `{{param_<name>}}` or `{{param_<name>:<default>}}` stops the
  exporter at startup, with an error that names the collector.

**Environment variables.** Placeholders use `{{…}}` so that they never
collide with the `${NAME}` references that
[`--config.expand-env`](CONFIGURATION.md#environment-variables) substitutes.
The environment is read once, when the file is loaded; path parameters are
bound on every probe. The two can be combined, so a default can come from
the environment:

```yaml
path: /api/{{param_tenant:${DEFAULT_TENANT}}}/status
```

With `--config.expand-env` off, that reference is left unexpanded in the
default, and the exporter refuses to start instead of binding
`${DEFAULT_TENANT` and leaving a stray brace in the path.

**Caching and self-metrics.** Every probe parameter is part of the response
cache key, so two tenants never share a cached result. The verbose
self-metrics label a request with the placeholder, not the value:
`url="http://legacy.us.example:8080/api/{{param_tenant}}/v{{param_version:2}}/status"`.
The value is a tenant or an account, which labels already keep out of the
query string, and one series per value would be unbounded.

**Static targets.** A [static target](STATIC-TARGETS.md) is scraped on the
exporter's own timer, with no probe to supply a value, so it gives its
values under `params`:

```yaml
targets:
  - name: acme_checkout
    collector: graphql_status
    target: https://api.example
    params:
      param_tenant: acme
      param_service: checkout
```

Every placeholder of the collector, in its request or in a
[label value](#in-label-values), must then be filled, by `params` or by a
default, and every entry of `params` must fill one. Otherwise the exporter
refuses to start, with an error that names the target, the collector and the
parameter. The target's own `request` block (its `path`, `body` and
`headers`) is literal and cannot use placeholders, and neither can its own
`labels`.

From a Prometheus Operator monitor, the values go in `params` like any other
probe parameter:

```yaml
params:
  collector: [legacy_text]
  param_tenant: [acme]
```

### In the body, headers and query

The same placeholders work in an `http` collector's `request.body`, in the
values of `request.headers` and in the values of `request.query`. They are
filled by the same probe parameters, with the same defaults, and refused
with the same `400` when one is missing or unused. One collector can then
talk to a GraphQL or JSON-RPC endpoint, or to an API that takes its tenant
in a header:

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

**Each place has its encoding.** A value is written in the form that its
place requires, so a probe parameter fills in a value and cannot change the
structure of the request:

| Where | Written as |
| --- | --- |
| Header value | As given; a value with a control character, such as a line break, is refused with `400`, so it can never end the header and start another. |
| Query value | Encoded as a query value: `us&x=1` stays one value and adds no parameter. |
| Body, `{{param_x\|json}}` | A JSON string, quoted and escaped: `a "b"` becomes `"a \"b\""`. |
| Body, `{{param_x\|number}}` | A JSON number, refused with `400` unless it is one: `12; DROP` is not. |
| Body, `{{param_x\|form}}` | URL form encoded, for `application/x-www-form-urlencoded` bodies. |
| Body, `{{param_x\|xml}}` | Escaped XML text, for SOAP and other XML bodies. |
| Body, `{{param_x}}` or `{{param_x\|raw}}` | Exactly as given. Use it only where the template itself is the structure and the value comes from your own monitors. |
| gRPC `message`, `metadata` value | The body's filters in the message, which is JSON, so only `\|json`, `\|number` and `\|raw`; a metadata value as a header value. See [gRPC](GRPC.md#placeholders). |
| Graphite target | As given, and only letters, digits and `_ - . : @ % + ~`; anything else — a quote, a comma, a bracket, a glob — is refused with `400`, since Graphite expressions have no escaping. See [Graphite](GRAPHITE.md#placeholders). |

The filter follows the default, if there is one:
`{{param_limit:10|number}}`. Filters exist only in the body. A header or
query value has one encoding and always gets it, so a filter there stops the
exporter at startup.

A default is held to the same rules as a value, when the configuration
loads. A default that its place refuses (`{{param_limit:ten|number}}`, a
header default with a line break in it, a glob as the default of a Graphite
expression) stops the exporter at startup, with an error that names the
collector, the field and the parameter. If it were loaded, it would answer
`400` to every probe that left the parameter out, and blame the probe for
the configuration's mistake.

**The body's own braces.** In a body, a header value or a query value, `{{`
opens a placeholder only when `param_` follows it, because a body may
contain its own braces. `{{ param_x }}` with spaces is refused at startup
instead of being sent as text. A brace before a placeholder is a brace:
`{{{param_x}}}` is sent as `{`, the value and `}`, which a list in a
Graphite expression needs, `app.{{{param_host}},db}.cpu`. Header names and
query names cannot hold placeholders.

**Only there.** Placeholders are filled in the places above, in a `sql`
collector's `database` and `args` ([SQL](SQL.md)), in the collector's
[fixed label values](#in-label-values), and nowhere else. A `{{param_...}}`
in any other setting of a collector would be used as written: the token
would be sent with the braces in it, and the value mapped to
`{{param_region}}`. Such settings are `bearer_token`, `basic_auth`, a
credential or TLS file, `tls.server_name`, a `value_map`, a label's name, a
`rename_labels` target, and a prometheus transform's `include` and
`exclude`. A placeholder there therefore stops the exporter at startup, with
an error that names the field:

```text
collector "api" request.bearer_token has a {{param_...}} placeholder, which is not filled in there and would be used as written; a probe's parameters fill placeholders only in the request's path, body, header and query values, a grpc message and metadata values, a graphite collector's targets, a sql collector's database and args, and the label values of transform.labels and of a metric rule's static label
```

To send a token that the probe supplies, put it in a header value:
`headers: {Authorization: "Bearer {{param_token}}"}`.

Three kinds of field are written in their own language and are not searched
for placeholders, because the same characters belong to that language there:

- Python, in `transform.script` and `transform.pre_script`, where an
  f-string writes a literal brace as two (`f"{{param_x}}"`);
- a metric's `items` and `expression` and a label's `expression`, where a jq
  string or a regex may hold the text;
- a metric's `description`, which may say that the path takes
  `{{param_tenant}}`.

None of them is filled in either: a placeholder written in an expression is
evaluated as the text it is.

The values are part of the response cache key, like every probe parameter.
The verbose self-metrics never carry them: the `url` label has no query
string, and the body and headers are not labels at all.

### In label values

A placeholder is also filled in the two places where a collector writes a
fixed label value: the values of
[`transform.labels`](CONFIGURATION.md#collector-wide-labels), and the
`value` of a metric rule's static label. One collector can then label its
series with a value that only the scrape knows, such as the tenant it was
asked about or the region that a monitor stands for:

```yaml
collectors:
  - name: tenant_status
    request: {type: http, path: "/api/{{param_tenant}}/status"}
    transform:
      type: jq
      labels:
        tenant: "{{param_tenant}}"
        region: "{{param_region:eu}}"
    metrics:
      - name: status_up
        expression: .up
        labels:
          - {name: source, value: "api-{{param_tenant}}"}
```

```text
/probe?target=https://api.example&collector=tenant_status&param_tenant=acme
  -> GET https://api.example/api/acme/status
     status_up{region="eu",source="api-acme",tenant="acme"} 1
```

<!-- request-types: all -->
One parameter may fill the request and the labels alike, as `param_tenant`
does here, or a label alone, in a collector whose request has no placeholder
at all. It works with every request type — `http`, `grpc`, `graphite`,
`localfile`, `sshfile`, `sql`, `dns`, `tlscert`, `ntp`, `kubernetes`,
`s3file`, `redis`, `tcp`, `ldap`, `sshexec`, `icmp` and `snmp`, and the opt-in `smtp`, `modbus`, `mqtt`, `websocket`, `ftp`, `imap`, `pop3`, `radius`, `whois`, `sip`, `amqp` and `nats` — and under every transform: `transform.labels` are applied
after every transform, a `python` one included, and a rule's `value` is
filled wherever a rule's label has one, which is every transform but
`python`, whose rules [set no label
value](CONFIGURATION.md#long-label-values). A `python` script reads the
parameters itself, as [`params`](#in-expressions).

<!-- /request-types -->

**Written as in a header value.** A label value holds `{{param_<name>}}` or
`{{param_<name>:<default>}}`, any number of them, among the value's own
text: `rack: "{{param_dc}}/{{param_rack:r1}}"`. `{{` opens a placeholder
only when `param_` follows it, so any other braces are text and are exported
as written. A brace before a placeholder is a brace, so `"{{{param_dc}}}"`
filled with `ams` is `{ams}`. Two things stop the exporter at startup, with
an error that names the collector and the label: `{{ param_x }}` with a
space after the braces, and a filter, because a label value is written in
one way, as it is given:

```text
collector "tenant_status" transform.labels.tenant placeholder {{param_tenant|json}} has a filter; a label value is written one way, as it is given, so write the placeholder without a |, which a default cannot hold either
```

A label's name cannot hold a placeholder. Neither can the `value` of a label
that also sets `expression`, which is refused whatever its value says. A
label with a placeholder in its `value` takes neither `required`, which is
for a label that an expression reads from the response, nor its own
`value_map`, like every label with a `value`. Either have the probe send the
value that is wanted, or set the `value_map` on a label of that name that
another rule of the same metric name reads with an expression: that maps the
label for every rule of that name.

**As given.** Nothing of a value is escaped when it is filled in, and
nothing is refused for the characters it holds. The exposition formats
escape a quote, a backslash and a line break where they write a label, as
for any label. The only value refused is text that is not valid UTF-8, which
no label value may be: such a probe is answered `400` with an error that
names the parameter.

**The probe's rules are the request's.** A placeholder with no value and no
default answers `400`, with an error that names the parameter and the label,
before the target is contacted:

```text
transform.labels.tenant needs param_tenant, which the probe did not supply and which has no default; add &param_tenant=<value> to the probe, or give it a default as {{param_tenant:<default>}}
```

An empty value (`param_tenant=`) counts as not supplied, so it takes the
default, or fails if there is none. An explicit empty default,
`{{param_suffix:}}`, binds nothing. A label whose whole value is then empty
is left off the series, like a label with an empty value everywhere else:

- For `transform.labels`, the label is left out. A series that has its own
  label of that name keeps it, where a non-empty value would replace it.
- For a rule's label, the rule does not set the label. The series has none,
  and a series that a `prometheus` rule passes on keeps the label of that
  name that the target gave it.

**Used, and unused.** A parameter that fills a label is used, so it is not
refused as unused even when the request names no placeholder. A `path`,
`body` or `message` probe parameter that replaces a templated part of the
request removes the request's use of a parameter, never a label's. A
parameter that no place uses is still answered `400`, as is one given twice.

**The same as the text written.** Everything that reads a label's value
treats a filled value exactly like the same text written in the
configuration:

- `limits.max_label_value_length`. A longer value fails the scrape in the
  validation, unless the label is cut:
  [`truncate: true`](CONFIGURATION.md#long-label-values) on it, or on the
  label of that name in another rule of the metric's name, cuts it, after
  any `value_map`;
- `remove_labels` and `rename_labels` and their order;
- a `value_map` of the rule's name;
- `name_escaping`;
- `limits.max_labels_per_metric`;
- the check for a series made twice.

**When the configuration loads**, rules are compared as they are written
([Two rules that are the same rule](CONFIGURATION.md#two-rules-that-are-the-same-rule)).
Two rules that differ only by `value: "{{param_a}}"` and
`value: "{{param_b}}"` are two rules, and load. A probe that gives both
parameters the same value makes them the same series, and that scrape fails
like any scrape with a duplicate series, `validation failed: duplicate
metric series "status_up"`.

The only length check that can be made at load time is made on the value as
a probe without parameters fills it: each placeholder is replaced by its
default, or by nothing where it has none. A value that is too long by its
own text and defaults would fail every probe that leaves the parameters out:

```text
collector "tenant_status" transform.labels "tenant" is 612 bytes once its placeholders take their defaults, longer than limits.max_label_value_length 500, so every series of a probe that gives them no other value would fail validation; shorten the text or the defaults, or raise the limit
```

Where a `value_map` of the rule's name maps a rule's label, the value is
measured after the map. A `"*"` entry longer than the limit is what every
value that the probe gives and the map does not list becomes. It is
therefore refused whatever the defaults are, unless the label is cut at the
scrape (`truncate: true` on it, or on the label of that name in any rule of
the metric's name):

```text
collector "tenant_status" metric "status_up" label "tenant" value holds {{param_...}} placeholders, and the value_map of the metric's name maps every value it does not list, by its "*" entry, to 612 bytes, longer than limits.max_label_value_length 500, so every series of a probe that gives a value not listed would fail validation; shorten the "*" entry or take it out, set truncate: true on the label, or raise the limit
```

A longer entry that the map lists is refused only where the defaults give
its key, because every probe that leaves the parameters out then gives it.
Otherwise the configuration loads, and a probe that gives that key fails its
scrape in the validation, like a probe that gives any long value.

**Static targets.** The `params` of a [static target](STATIC-TARGETS.md)
fill a label's placeholders exactly as they fill the request's, and the file
is held to both when it loads: every placeholder of the collector, in the
request or in a label, must be filled by `params` or a default, and every
entry of `params` must fill one. The target's own `labels` stay literal.

**Caching.** Every probe parameter is part of the
[response cache](CONFIGURATION.md#response-caching) key, and of what makes
two probes [identical](CONFIGURATION.md#identical-probes-share-one-request),
whether it fills the request or only a label. Two probes that differ only in
a label's parameter never get each other's series: not from the cache, not
as the stale result that `cache.stale_if_error` answers a failed trip with,
and not by sharing one request while both are in flight.

**Cardinality.** Each distinct value is a separate set of series in
Prometheus. The values should therefore come from the `params` of your own
monitors (a known list of tenants or regions) and not from whoever can reach
`/probe`. The exporter itself keeps a value only where it keeps any probe's
parameters and series, each within a bound:

- in the response cache, which `limits.max_cache_entries` bounds;
- in the [failure log](LOGGING.md#repeated-failures), which remembers a
  failing probe, with its parameters, until it recovers or has not failed
  for an hour, for at most 10,000 failing things;
- with the [OTLP export](OTLP.md) on, in the data points that wait for the
  next export, which [`otlp.max_pending_points`](OTLP.md#delivery) bounds;
- with the OTLP export on, in the start times of the exported counters,
  histograms and summaries. Each is kept until its series has not been
  exported for an hour. There are at most twice `otlp.max_pending_points` of
  them, each identified by at most 256 bytes of its series, or by a digest
  of a longer series.

The verbose self-metrics never carry a value. A collector whose label values
hold no placeholder pays nothing for the feature. One that has some makes a
copy of its labels for each probe.

### In expressions

A `jq` or `yq` expression of a metric rule reads the probe's parameters as
`$params`, an object with each `param_<name>` that the probe gave, by name:

```yaml
metrics:
  - name: queue_jobs
    items: .queues[] | select(.name == $params.queue)
    expression: .jobs
    labels:
      - name: region
        expression: $params.region // "eu"
```

```text
/probe?collector=queues&target=https://broker.example&param_queue=mail
```

A value is text. A parameter that the probe did not give, or gave empty, is
`null`, so an expression states what happens without it, as `// "eu"` does
above.

- A collector accepts the parameters that its expressions read by name,
  `$params.queue` or `$params["queue"]`, in addition to those that its
  placeholders name. Any other is refused as
  [a mistake](#path-parameters).
- A collector whose expression reads `$params` in another way, as a whole or
  by a computed name, accepts any parameter.
- A [static target's `params`](STATIC-TARGETS.md) are read in the same way,
  and probes that differ in a parameter are cached separately.
- `$params` is `null` in the expressions of pagination and logins.

A Python script (a `python` transform's or a `pre_script`) reads the same
parameters as `params`, a dict with each one by name:

```yaml
transform:
  type: python
  script: |
    for queue in data["queues"]:
        if params.get("queue") in (None, queue["name"]):
            metric(name="queue_jobs", value=queue["jobs"], labels={"queue": queue["name"]})
```

A collector accepts the parameters that its script reads as
`params["queue"]` or `params.get("queue")`, and any parameter when the
script uses `params` in another way, as a whole or with a computed name.

### Dates and times

Many APIs need a date or a time in the request: the day of a daily summary,
the start of a history window, the hour of an electricity price. A clock
placeholder writes the time of the scrape. It can stand wherever a
`{{param_<name>}}` placeholder can, in any request type:

```yaml
collectors:
  - name: daily_summary
    request:
      type: http
      path: /usersummary/daily/{{date@Europe/Sofia}}
      query:
        from: "{{datetime:-1d/d}}"
        to: "{{datetime/d}}"
```

A scrape at 22:30 UTC on 9 November 2026 requests
`/usersummary/daily/2026-11-10?from=2026-11-08T00:00:00Z&to=2026-11-09T00:00:00Z`
(the query values are encoded like every query value): in Sofia, it is
already the 10th.

A clock placeholder is the name of its format, followed by these optional
parts in this order: a [last success](#since-the-last-success) to start
from, an offset, a truncation and a time zone:
`{{name[~last_success(fallback)][:offset][/unit][@zone]}}`.

| Name | Writes, at 22:30:15.25 UTC on 9 November 2026 |
| --- | --- |
| `date` | `2026-11-09` |
| `date_compact` | `20261109` |
| `date_eu` | `09.11.2026` |
| `date_us` | `11/09/2026` |
| `date_imap` | `9-Nov-2026`, as an IMAP search writes a date |
| `month` | `2026-11` |
| `year` | `2026` |
| `time` | `22:30:15` |
| `datetime` | `2026-11-09T22:30:15Z`, RFC 3339 |
| `datetime_ms` | `2026-11-09T22:30:15.250Z`, RFC 3339 with milliseconds |
| `unix` | `1794263415`, seconds since 1970 |
| `unix_ms` | `1794263415250`, milliseconds since 1970 |

**Offset.** After a colon, `+` or `-` and one or more amounts of `w`
(weeks), `d` (days), `h`, `m` and `s`: `{{date:-1d}}` is yesterday,
`{{date:+1w}}` is a week from today, and `{{datetime:-1d12h}}` is a day and
a half ago. Weeks and days are calendar days in the zone, so `-1d` is
yesterday's date even on the day when summer time starts or ends. Hours,
minutes and seconds are elapsed time. An amount is at most 100000.

**Truncation.** After a slash, `d`, `h` or `m`: the start of the day, hour
or minute that the moment falls in, in the zone, applied after the offset.
`{{datetime:-1d/d}}` is midnight at the start of yesterday, and `{{unix/h}}`
is the start of this hour, in seconds.

**Time zone.** After an at sign, an IANA name such as `Europe/Sofia` or
`America/New_York`, `UTC`, or `Local`, the zone that the exporter runs in.
It is UTC when left out. The zone decides which day it is and how `datetime`
writes its offset: `{{datetime@Europe/Sofia}}` is
`2026-11-10T00:30:15+02:00`. The exporter includes the time zone database,
so every name works in a container without one.

**Filters.** In a body, a clock placeholder takes a filter as a
`{{param_...}}` placeholder does: `"day": {{date|json}}` writes
`"day": "2026-11-09"`, and `"since": {{unix_ms:-1h|number}}` writes a
number. A format that its place cannot write is refused when the
configuration loads, as a default would be: `{{date|number}}`, and a
`date_us` in a `localfile` path, whose slashes would be directories.

**One moment per scrape.** Every clock placeholder of a scrape uses one
moment, read when the scrape starts. `{{date}}` in the path and
`{{datetime}}` in a query value are of the same day even a second before
midnight, and a retry and a later page send the values that the first
request sent.

**Not a probe parameter.** No probe gives a clock placeholder a value, the
collectors page asks for none, and a probe parameter named `date` has
nothing to do with the request.

- A [static target](STATIC-TARGETS.md#dates-and-times-in-a-parameter) may
  write clock placeholders into the values of its own `params`.
- A clock placeholder may not stand in a [label value](#in-label-values): a
  label that changes with the clock is a new series every day, hour or
  second.
- Like a `{{param_...}}` placeholder, it is recognised outside a path only
  when the name follows the two braces directly, so `{{ date }}` and
  `{{dates}}` in a body are text.

**Caching.** A [cached](CONFIGURATION.md#response-caching) answer is kept
for its `cache.ttl` whatever the clock says. A collector whose request asks
for today's date, cached for an hour, may therefore answer with yesterday's
data for the first hour of the day. Keep the TTL short compared with the
unit that the request changes by.

A cached answer is not
[revalidated](CONFIGURATION.md#revalidating-expired-responses) when its
request holds a clock placeholder, the collector's or a static target's. The
validators of yesterday's answer say nothing about today's, and a target
asked with `If-Modified-Since` would answer today's request with `304`,
which would keep yesterday's answer for it. The `url` label of the verbose
self-metrics keeps a placeholder of the path as it is written.

### Since the last success

An API that answers with what happened in a window of time (activities,
events, log lines, orders) is best asked for each stretch of time once: from
where the last successful scrape ended, to now. `~last_success`, after a
clock placeholder's name, starts the placeholder from the time of the last
successful scrape of the same collector, target and parameters, instead of
from the time of the current scrape:

```yaml
collectors:
  - name: new_events
    request:
      type: http
      path: /api/events
      query:
        from: "{{datetime~last_success(-1h)}}"
        to: "{{datetime}}"
```

The first scrape asks for the last hour, which is the fallback in the
parentheses. Each scrape after a successful one asks from the `to` that the
successful one sent. The time of a scrape is read once, when it starts, so
the windows meet with no gap and no overlap. A scrape that fails, or that
carries on past a failed stage under `error_handling`, moves nothing, and
the next scrape asks for its time again.

- **The fallback is required**: `-` and amounts like an offset's,
  `~last_success(-1h)`, `~last_success(-1d)`. It is where the window starts
  while there is no last success: on the first scrape of a target, of a new
  value of a parameter, and after the exporter starts, because the times are
  kept in memory. A reload keeps them.
- **The other parts apply after it.** `{{datetime~last_success(-1h):-5m}}`
  starts five minutes before the last success, which gives an overlap for an
  API whose events arrive late. `{{date~last_success(-1d)@Europe/Sofia}}` is
  the day of the last success, in Sofia. Truncate both ends in the same way,
  or the windows overlap or leave gaps: a `from` of
  `{{datetime~last_success(-1h)/m}}` meets a `to` of `{{datetime/m}}`.
- **Each target has its own.** A probe's last success is that of its
  collector, target, `path` and `param_<name>` parameters. A static target
  has its own, and a parameter value of the target may hold `~last_success`
  too. A [debug](AUTHENTICATION.md#debugging-collectors) run starts from the
  last success and stores none. An answer from the
  [response cache](CONFIGURATION.md#response-caching) is not a scrape and
  moves nothing.
- **One asker for each window.** A window is given once, to whichever scrape
  succeeded. Two Prometheus servers that probe the same target each get
  every other window. Each of several exporter replicas behind one Service
  keeps its own last success, so their windows overlap. In either case, make
  the target a [static target](STATIC-TARGETS.md): the exporter asks on its
  own schedule, once, and every Prometheus reads the same answer. With the
  [target allocator](ALLOCATOR.md), a static target that moves to another
  replica starts there from its fallback, as after a restart.
- **What a success is.** A scrape succeeds when it is answered with metrics.
  A `HEAD` of `/probe`, which runs the probe and reads no body, therefore
  moves the window. So does a scrape in which a rule failed under
  `error_mode: log` or `ignore` and its metric was left out. Give a rule
  whose window must not be lost
  [`error_mode: fail`](CONFIGURATION.md#when-a-metric-cannot-be-extracted),
  which fails the scrape and keeps the window for the next one.
- **There is no cap.** A target that failed for a day is asked for the whole
  day at its next success. The exporter keeps the last success of at most
  10,000 collectors, targets and parameters. Beyond that it forgets the one
  stored longest ago, which then starts from its fallback.

## Redirects and HTTP/2

Three transport settings are on the collector request, all off by default:

```yaml
request:
  follow_redirects: false
  enable_http2: false
  new_connection: false
```

`follow_redirects` decides whether a redirect status on the target request
is followed. At `false`, the exporter returns the redirect response itself,
so the collector sees the 3xx status and, with the default
`on_fetch_error: fail`, the probe fails. That is deliberate: a target that
has moved should be noticed, not silently scraped somewhere else. Set it to
`true` for endpoints that legitimately redirect, such as an API whose
documented host forwards to another.

Whether a redirect is followed is decided before anything is sent to its
destination, as for the first URL.

- It must satisfy the collector's `allowed_schemes`. A collector that allows
  only `https` refuses a redirect to `http://` (`redirect to http://…
  refused: target scheme "http" is not allowed; request.allowed_schemes
  allows https`) instead of reading the answer in plain text.
- It must satisfy
  [`allowed_targets` and `denied_targets`](#restricting-targets) too.
- A request is stopped at its tenth redirect, which fails the probe.

### What a followed redirect carries

A target that redirects chooses where the next request goes, so what that
request carries depends on its destination. The *origin* of a request is the
scheme, host and port of its first URL (the target with the collector's
`path`). A port that is left out is the scheme's default, 80 or 443. A
redirect is *trusted* when it stays on that origin, or when its host is
listed in `request.redirect_trusted_hosts`:

| The redirect leads to | Headers and credentials | Request body |
| --- | --- | --- |
| the origin the request was made to | everything the first request carried | sent again after a `307` or `308` |
| a host in `redirect_trusted_hosts`, on any port | everything the first request carried | sent again after a `307` or `308` |
| anywhere else: another host, a subdomain, another port, another scheme | only `Accept`, `Accept-Language` and `User-Agent` | never: a redirect that would send it is refused |

The most common redirect, from `http://` to `https://` on one host, leads to
another origin, and gets none of the collector's credentials unless the host
is listed.

**How the host is compared.** The host is compared as the URL writes it,
ignoring letter case, because that is the name by which the request is
routed. Nothing is normalised first.

- `api.example.com.`, with a final dot, is a different host from
  `api.example.com`: a resolver looks up a rooted name separately, bypassing
  the hosts file and the search domains.
- `127.0.0.1` is not `localhost`, and a port written `:080` is not port 80.
- A redirect to a host written with a character outside ASCII,
  `bücher.example` or a look-alike of the origin's own name, is not followed
  at all, as a target written that way is
  [refused](#restricting-targets). An internationalised name is trusted, and
  reached, in its `xn--` form, so list it in that form and have the target
  redirect to it in that form.

**What "everything" is.** It is the collector's `headers`, the headers
[forwarded](AUTHENTICATION.md#reaching-the-target) from the probe, the
`Authorization` that its credentials produce, and a `Cookie`. Any other
destination gets none of them, whatever a header is called (an API key in
its own header is as much a credential as `Authorization`), and no `Host`
that the collector set. The exporter never adds a `Referer`, which would
tell the next host the URL it came from. One set in `request.headers` is a
header of the collector like any other.

`Accept`, `Accept-Language` and `User-Agent` go to every redirect
destination, with the values that the first request had. Never put a token,
or anything else that identifies the collector, in those three: not in
`request.headers`, and not by forwarding a probe's header into them. The
first URL's path and the collector's `query` are not carried to another
host, because a `Location` names the whole URL of the next request. The
first host sees them, however, and the first host chooses the `Location`.

Each redirect of a chain is judged separately against the first URL and the
list. A request that is sent to an untrusted host and then back to its
origin therefore carries everything again once it is back.

**The body.** A `301`, `302` or `303` turns a `POST`, `PUT`, `PATCH` or
`DELETE` into a `GET` without a body, and without the `Content-Type` and the
other headers that describe one. That redirect has nothing to withhold and
is followed. A `307` or `308` sends the request again as it was. With a body
(the collector's `body`, a `body` probe parameter, or the form that a
[`graphite`](GRAPHITE.md) collector posts for long expressions) it is
followed only to a trusted destination. Otherwise the probe fails:

```text
redirect to https://other.example/x refused: it would send the request body to other.example, which is not the origin the request was made to and is not listed in request.redirect_trusted_hosts
```

**The client certificate.** The collector's TLS client certificate,
[`tls.cert_file` and `tls.key_file`](#tls), is a credential too, and every
TLS connection that the collector makes presents it. So is a static target's
own certificate, in the `tls` block that replaces the collector's for it:
every TLS connection of that target's requests presents it, and its refusal
says `the static target's TLS client certificate`. A redirect over `https`
to an untrusted destination is therefore refused before a connection is made
there and, like the refusal of a body, is not retried:

```text
redirect to https://other.example/x refused: it would present the collector's TLS client certificate to other.example, which is not the origin the request was made to and is not listed in request.redirect_trusted_hosts
```

A trusted destination is presented the certificate. A redirect over plain
`http`, where there is nothing to present, is followed. The other `tls`
settings are not credentials and apply to every hop of a request:

- `ca_file` and `insecure_skip_verify` decide how each host that a redirect
  leads to is verified.
- A `server_name` written for the target is also the name that every `https`
  host of a redirect is verified against. A redirect to another `https` host
  therefore fails verification unless that host holds a certificate for that
  name.

**The list.** List the hosts that a target is known to redirect to and that
may be given the collector's credentials:

```yaml
request:
  type: http
  follow_redirects: true
  bearer_token_file: /etc/exporter/api-token
  redirect_trusted_hosts:
    - api-eu.example.com   # a host name
    - "*.cdn.example.com"  # a glob: * and ? match any characters, dots too
    - 192.0.2.10           # an address, for a redirect that names the address
```

An entry is a host name, a glob of one or an IP address, written as
[`allowed_targets`](#restricting-targets) writes them and without a scheme,
a port or a path. `"*"` alone trusts every host, wherever the target sends
the request.

- A listed host is trusted on every port and over every scheme that
  `allowed_schemes` allows. A collector whose credentials must not travel in
  plain text therefore allows only `https`.
- Entries are compared with the host that the redirect's URL names, as
  written there and ignoring letter case. Nothing is looked up, so an
  address does not trust a name that resolves to it, and a network cannot be
  listed.
- An entry with a final dot matches the host written with one, and no other.
  An address with a zone, `fe80::1%eth0`, matches only that zone.
- An entry is written in ASCII, as in `allowed_targets`. One with a
  character outside ASCII stops the load, and no entry, including `"*"`,
  matches a host written with one, to which no redirect is followed.

A star matches dots too, so a glob can trust more than it appears to:

- `*example.com` matches `evilexample.com`; write `*.example.com`.
- `api.example.*` matches `api.example.evil.net`, so do not put the star
  last.
- A glob is matched against host names, not addresses. `10.*` would trust
  `10.evil.example`, so a glob made only of digits, dots and wildcards is
  refused, and addresses are listed one by one.
- Wildcards alone are refused too, in any spelling except `"*"`: `**`,
  `*.*`.

An empty entry, a network, a port and a URL are refused when the
configuration loads: `request.redirect_trusted_hosts entry
"api.example.com:8443" has a port; a host is trusted on every port, so give
the host alone: api.example.com`. The list belongs to the collector only: no
probe parameter and no static target sets or extends it. It applies whenever
redirects are followed, including when a probe switched that on with
`follow_redirects=true`.

**Credentials in a URL.** A credential written into the target URL itself,
`https://user:password@host/`, belongs to that URL. It follows a redirect
whose `Location` is a path, which stays on the URL's host, and no redirect
that names a host, trusted or not. A user and password that a `Location`
writes into its own URL come from the target, not from the collector. An
untrusted destination is sent none of it. A trusted one is sent them as
basic auth only when the request has no `Authorization` of its own;
otherwise that `Authorization` is sent. A `Host` in `request.headers` names
a virtual host of the origin: it is kept across a redirect to a path, and
sent to no other host, listed or not.

**Errors.** When a host that was not sent the collector's headers answers
with a status that fails the probe, most often `401` or `403`, the error
says so and names the setting: `received HTTP status 401 from other.example,
where a redirect led: the collector's headers and credentials were not sent
to that host, which is not the origin the request was made to; list it in
request.redirect_trusted_hosts if it is to be sent them`. A credential in
the target URL counts as one that was not sent.

Where the redirect led to the target's own host under another scheme or on
another port, the messages name both origins, because the host alone would
not show the difference: `received HTTP status 401 from
https://api.example.com, where a redirect led: the collector's headers and
credentials were not sent there, since https://api.example.com is not the
origin the request was made to, http://api.example.com; list api.example.com
in request.redirect_trusted_hosts if it is to be sent them`.

A [debug probe](CONFIGURATION.md#debug-probes) lists every redirect with the
headers it was sent and, under an untrusted one, the names of the headers
that were not sent, with a credential of the target URL as `Authorization
(the target URL's credentials)`.

`enable_http2` decides whether the target request may negotiate HTTP/2.
HTTP/2 is negotiated through ALPN over TLS, so this affects only HTTPS
targets; cleartext HTTP/2 is never attempted. The default, `false`, is the
protocol that the exporter has always used.

`new_connection` sends every request on its own connection, closed after its
answer, instead of on one kept from an earlier request: see
[Connections](#connections).

All three are overridable per scrape:

```yaml
params:
  follow_redirects: ["true"]
  enable_http2: ["true"]
  new_connection: ["true"]
```

Each accepts exactly `true` or `false`. Anything else returns HTTP 400
before the target is contacted, so a typo cannot silently fall back to a
default. An absent parameter leaves the collector's setting in force. The
parameters are part of the response cache key, so a scrape that asks for
different transport behaviour never reads another scrape's cached result.
Static targets accept all three in their own `request` block.
`redirect_trusted_hosts` cannot be overridden: a probe that names it is
answered as though it had not.

These replace the earlier undocumented `request.redirect_policy`. A
configuration that still sets it now fails to load with an unknown-field
error instead of silently changing behaviour.

## Restricting targets

Whoever can reach `/probe` chooses the `target`, and a target that redirects
chooses where a followed redirect goes. `allowed_targets` and
`denied_targets` bound:

- where a collector's requests may go, for `http`, `graphite`, `grpc`,
  [`sshfile`](SSHFILE.md), [`sql`](SQL.md#the-target),
  [`tlscert`](TLSCERT.md), [`ntp`](NTP.md), [`icmp`](ICMP.md#the-target),
  [`redis`](REDIS.md#the-target), [`tcp`](TCP.md#the-target),
  [`ldap`](LDAP.md#the-target), [`sshexec`](SSHEXEC.md#the-target) and
  [`snmp`](SNMP.md#the-target) collectors;
- which names a [`dns`](DNS.md#target-lists) collector may resolve, matched
  by name only;
- which buckets an [`s3file`](S3FILE.md#which-object-is-read) collector may
  read, by name or glob, because its target is a bucket.

```yaml
request:
  type: http
  allowed_targets:
    - "*.example.com"      # a glob: * and ? match any characters, dots too
    - api.partner.net      # a host name
    - 203.0.113.0/24       # a network: every address the target resolves to must be in one
  denied_targets:
    - 192.0.2.10           # an address
    - 10.0.0.0/8
    - 127.0.0.0/8
    - "::1"
```

Every `http`, `graphite`, `grpc`, `sshfile`, `sql`, `tlscert`, `ntp`, `icmp`, `redis`, `tcp`, `ldap`, `sshexec`, `snmp`, `smtp`, `modbus`, `mqtt`, `websocket`, `ftp`, `imap`, `pop3`, `radius`, `whois`, `sip`, `amqp` and `nats` collector, and a
`dns` collector [asking for PTR records](DNS.md#target-lists) of an address,
refuses the cloud metadata service even with neither list set. That is
`169.254.169.254`, where AWS, GCP, Azure and most others answer, and AWS's
`fd00:ec2::254`. The service hands out the credentials of the machine that
the exporter runs on to whoever can name it as a probe's target. A collector
that must reach it lists its address, or a network that contains it, in
`allowed_targets`; a name that resolves to it is not enough.

A [`kubernetes`](KUBERNETES.md#no-target) collector takes neither list and
no target: it lists what its configuration names, from the API server that
the configuration names.

Each entry is a host name, a glob of one, an IP address or a CIDR network,
without a scheme, port or path.

- A name is letters, digits, dots, hyphens and underscores, so `my_service`
  and `*.svc_local` can be listed.
- An entry is written in ASCII, and an internationalised name in its `xn--`
  form (`xn--bcher-kva.example` for `bücher.example`). An entry with any
  other character stops the load: `request.allowed_targets entry
  "bücher.example" has U+00FC 'ü', a character outside ASCII: write an
  internationalised name in its ASCII form, as xn--bcher-kva.example for
  bücher.example`.
- A target is refused when its host is denied by name, or when any address
  it resolves to is in a denied network. When `allowed_targets` is set, it
  is also refused unless its host is allowed by name, or every address it
  resolves to is in an allowed network. `denied_targets` wins.
- Names are compared ignoring letter case. `*.example.com` matches
  `a.b.example.com`, but not `example.com` itself.
- An IPv6 address with a zone, `fe80::1%eth0`, is checked as the address
  without it, because the zone only names the interface that reaches it.
  `denied_targets: [fe80::/10]` therefore refuses it, and the metadata
  service's `fd00:ec2::254%eth0` is refused like `fd00:ec2::254`.
- An address in `64:ff9b::/96`, which a NAT64 gateway forwards to the IPv4
  address in its last four bytes, is also denied by a rule against that IPv4
  address. `denied_targets: [10.0.0.0/8]` refuses `64:ff9b::a00:1`, and the
  metadata service is refused as `64:ff9b::a9fe:a9fe`. A network with its
  own NAT64 prefix needs that prefix denied by hand.

**When names are checked.** Names are checked before the request and again
for every redirect that is followed. So is a target written as an address,
which needs no lookup, so it is refused before anything is sent.

A host is checked as it will be dialed. It is an IP address or a name. A
name is letters, digits, `.`, `-` and `_`, and is dialed as written, so it
is checked as written, in lower case and without a final dot. `my_service`,
`db--primary.internal` and `-edge.internal` are not names that a registrar
would sell, but Docker Compose, Kubernetes and a hosts file produce such
names, and they are reached and matched like any other.

The name that the lists judge is therefore the name that the request is made
to. It is what is looked up and dialed, what the `Host` header (the
`:authority` of HTTP/2) and the TLS handshake name, and what a proxy reads
in the request line or the `CONNECT`. Capitals and a final dot do not make a
different name. Only the collector's own settings replace it: a `Host`
header that it sets or forwards, and `tls.server_name`.

**Hosts outside ASCII.** A host with a character outside ASCII is refused
with `403`, before anything is sent or looked up, whatever the lists are and
even with none set: `target bücher.example refused: it has U+00FC 'ü', a
character outside ASCII: write an internationalised name in its ASCII form,
as xn--bcher-kva.example for bücher.example`. Write an internationalised
name in its `xn--` form, in the target and in the lists alike.

The reason is that the HTTP client does not send a host written outside
ASCII under one name. It dials, and names in the TLS handshake, the form
that the name maps to: `ｏrigin.test`, with a full-width `ｏ`, and
`origin。test`, with an ideographic full stop, are both `origin.test`. But
it writes the `Host` header, and the request line that a proxy reads, in the
unmapped form, `xn--rigin-qr33a.test` and `xn--origintest-sh3i`.

- A list that allows `origin.test` would therefore let through a request
  that a proxy fetches from, or a server of many sites answers from, a name
  that nobody allowed.
- A list that denies a name would be bypassed by another spelling of it.
- Over HTTP/2 the client would not send such a request at all, and would
  dial the target again and again instead.

A host written in full-width digits (`１２７.０.０.１`) is refused in the
same way. So is a redirect to any such host, and a `grpc` target that names
one.

**Other invalid characters.** A host with any other character that no name
has is refused in the same way: `target intern%61l.example refused: it has
"%", a character that no host name or address has`. No name is written with
a `%`, a `,` or a `;`, and such a host is not harmless behind a
[proxy](#proxies), which may read it differently from the lists. A URL
carries a `%` in its host as `%25`, so `http://intern%2561l.example/` names
the host `intern%61l.example`. That matches no entry for `internal.example`,
but a proxy that decodes it once more fetches from `internal.example`. The
same spelling of `169.254.169.254` would reach the metadata service. A
redirect to such a host is not followed, and a `grpc` target that names one
is refused in the same way.

**Older IPv4 forms.** An IPv4 address in one of the older forms that
resolvers and proxies still read is checked as the address it is
(`127.0.0.1` in these examples), not as a name, before anything is sent: one
number (`2130706433`), fewer than four parts (`127.1`), or parts in
hexadecimal or octal (`0x7f.0.0.1`, `0177.0.0.1`). A connection to a host
that the request did not check, when no proxy is in between, is refused too.

**Addresses.** Addresses are checked against the address that each
connection is actually made to. The name is therefore looked up once, by the
connection, and a name that resolves differently from one lookup to the next
cannot bypass the check.

Behind a [proxy](#proxies) the exporter connects to the proxy, so it looks
up the target's name itself before the request and checks those addresses.

- A name that it cannot look up is left to the proxy to resolve, as for curl
  or any other client behind a proxy. An exporter with no outside DNS of its
  own therefore still reaches what its proxy reaches, and the name rules of
  both lists still apply.
- Only a collector whose own lists hold an address or a network fails then,
  because its rules could not be applied to an address that is unknown here:
  `the request goes through a proxy, so metrics.partner.example has to be
  looked up here to hold it to the addresses and networks in
  request.allowed_targets and denied_targets, and the lookup failed`. List
  names instead, or give the exporter a resolver that knows the targets.

This limits the protection behind a proxy, including the default refusal of
the cloud metadata service. The exporter checks the name, the address when
the target is written as one, and the addresses that its own lookup returns.
It does not see the address that the proxy resolves the name to. A name that
the exporter cannot resolve, or resolves differently from the proxy, reaches
whatever the proxy reaches for it, which is the metadata service of the
proxy's machine if a name there leads to it. Where that matters, restrict
the collector by name with `allowed_targets`, and deny `169.254.169.254` at
the proxy.

Whether a request goes through a proxy is decided in the same way as the
connection pool that sends it decides: from the collector's
[`proxy_url`](#a-collectors-own-proxy), or from the environment as the pool
read it. The proxy's own address comes from the configuration and is not
checked against the lists.

A `grpc` collector does the same. Its connections are checked as they are
made, and a refused one is reported as the refusal and answered `403`, as
for `http`, not as the `UNAVAILABLE` that gRPC would make of it. A refused
request is not retried.

A refused probe is answered `403 Forbidden` (`collector web refused the
target: target 10.1.2.3 refused: its address 10.1.2.3 is in 10.0.0.0/8 in
request.denied_targets`) without the target being contacted, whatever
`error_handling.on_fetch_error` says, and is counted in
`http_exporter_targets_refused_total`. A refused static target fails in the
`target_policy` stage.

## Connections

Connections to targets are kept and reused, so an HTTPS target costs one TLS
handshake instead of one per scrape.

**Pools.** Collectors and scrapes share one connection pool when they have
all of the following in common:

- the TLS settings, that is, the whole `tls` block: `ca_file`, `cert_file`,
  `key_file`, `insecure_skip_verify` and `server_name`;
- `enable_http2`;
- `allowed_targets` and `denied_targets`;
- the [proxy](#a-collectors-own-proxy) and proxy credentials, or the
  [Unix socket](#over-a-unix-socket).

A scrape that overrides `insecure_skip_verify` or `enable_http2` uses the
pool of its own settings. So does a static target with its own `tls` block,
so that no connection made with one client certificate carries a request
that must present another. Collectors with different target lists never
share a connection, because a connection is checked against the lists once,
when it is made.

A certificate or key replaced on disk is picked up by the next request. An
idle connection is closed after 90 seconds. A pool that nothing has used for
five minutes, such as one left behind by a reload, is closed with its
connections. OTLP exports keep their connection to the collector in the same
way.

**A connection per request.** A kept connection answers with what it was
made with: the address that the name had when it was looked up, and the
certificate that the target presented then. A target that has moved to
another address, or that serves a new certificate, or one that no longer
verifies, is not noticed until the connection closes. A request sent on a
kept connection also has no lookup, connection or handshake to
[time](#timings-tls-and-the-final-url).

`new_connection: true` sends every request of the collector, including each
[retry](#retries) and each request that a followed
[redirect](#redirects-and-http2) makes, on a connection made for it. No
other request uses that connection, and it is closed when its answer has
been read, as with blackbox_exporter's probes. Each request then looks up
the name, connects, and makes and verifies the TLS handshake again. That is
what an [uptime check](#uptime-probes) needs, at the cost of a handshake per
request. A scrape overrides it with the `new_connection` parameter, and a
static target in its own `request` block. A login's request is sent on a
kept connection in either case.

### Address families

A name with both IPv4 and IPv6 addresses is connected to over either. Go
tries the family of the first address that the lookup returned, then the
other a moment later, and uses the first connection made. An `http` or
`graphite` collector chooses the family with `ip_family`:

```yaml
request:
  ip_family: ip6
```

| `ip_family` | Connects over |
| --- | --- |
| left out | either family, as Go connects |
| `ip4`, `ip6` | that family alone. A target with no address of it — a name with none, or an address of the other family written as the target — fails: `www.example.com has no IPv6 address, and request.ip_family is ip6`. |
| `prefer_ip4`, `prefer_ip6` | that family first, and the other when the first has no address, or none of its addresses takes the connection. Under a deadline, the probe's or the collector's `timeout`, the first family is given half of the time left, so an address that never answers leaves the other its half within the same probe. |

An uptime check of a site's IPv6 is therefore `ip_family: ip6`. A site that
is reachable only over IPv4 fails it, where Go would have answered over
IPv4.

- It applies to every request of the collector: each [retry](#retries), each
  request that a followed [redirect](#redirects-and-http2) makes, and each
  page. It does not apply to its
  [login](AUTHENTICATION.md#logging-in-to-a-target), which connects over
  either family.
- A scrape overrides it with the `ip_family` parameter,
  `params: {ip_family: [ip4]}`, which takes the four values and no other
  (`400` otherwise). A static target sets it in its own `request` block.
- Each family has its own [connection pool](#connections), so a connection
  made over IPv4 never carries a request that asked for IPv6.

The family is that of the exporter's connection to the target, so the
setting is refused where there is no such connection:

- beside [`unix_socket`](#over-a-unix-socket), whose connection has no
  address family;
- beside a [`proxy_url`](#a-collectors-own-proxy) other than `direct`, and
  for a request that the [environment's proxy](#proxies) would carry, which
  fails before the proxy is asked anything. A proxy looks up the target's
  name and connects to it over the family that the proxy chooses, and the
  exporter's own connection, to the proxy, says nothing about the target's.
  Reach such a target with `proxy_url: direct`, or name it in `NO_PROXY`.

A probe parameter or a static target's `ip_family` for a collector with its
own socket or proxy is refused, as the key is.

### Proxies

Target requests and OTLP exports go through the proxy that the environment
names, as with curl:

- `HTTPS_PROXY` for `https` URLs and `HTTP_PROXY` for `http` URLs;
- `NO_PROXY` for what to reach directly: a comma-separated list of hosts,
  domains (`.internal.example` or `internal.example` covers every host under
  it), IP addresses and CIDR ranges, optionally with a port, or `*` for
  everything.

The lower-case spellings work too. Requests to `localhost` and loopback
addresses always go direct. For an `https` URL the proxy is asked to tunnel
the connection (`CONNECT`), so TLS still runs end to end with the target,
and its [TLS settings](#tls) apply unchanged.

```sh
HTTPS_PROXY=http://proxy.corp.example:3128 \
NO_PROXY=.svc,.cluster.local,10.0.0.0/8 \
  prometheus-universal-exporter --config.file=config.yaml
```

A proxy URL may carry credentials, `http://user:password@proxy:3128`, which
are sent as `Proxy-Authorization`. The environment applies to the whole
exporter and is read once, when the exporter first connects. An `http` or
`graphite` collector may name its own proxy instead
([below](#a-collectors-own-proxy)). In Kubernetes, set the variables through
the chart's `env`, with credentials from a Secret.

Which request types use the proxy:

- `localfile` collectors read files and are not affected.
- `sshfile`, `sshexec`, `sql`, `redis`, `tcp`, `ldap` and `tlscert`
  collectors connect to their hosts directly and are not affected.
- `dns`, `ntp`, `icmp` and `snmp` collectors are not affected: no HTTP proxy
  carries their datagrams.
- `grpc` and `kubernetes` collectors go through the proxy. So do `s3file`
  collectors, through the AWS SDK's own client, which reads the same
  variables.

A proxy that refuses to tunnel to an `https` target fails the request with
`the proxy refused the tunnel to api.example:443: 403 Forbidden`. The caller
of a probe is told this without the proxy's words. The caller is told
nothing about a failed connection to the proxy (its address, its name, its
certificate) beyond `proxyconnect tcp: [withheld; in the exporter's log]`,
and nothing about a SOCKS proxy's failure (its address and what its
handshake said, such as `username/password authentication failed`) beyond
`socks connect tcp: [withheld; in the exporter's log]`. The proxy is part of
where the exporter runs, not what the caller asked for. The log and a debug
report have the whole error.

A plain `http` target's request is sent to the proxy whole. What the proxy
answers it with, such as its own `407` or `502`, is therefore the answer, as
if it came from the target.

### A collector's own proxy

An `http` or `graphite` collector can name the proxy that its requests go
through, instead of the environment's. For example, a partner API may be
reached through a proxy in its own network while everything else goes direct
or through the usual proxy. A collector can also go direct although the
environment names a proxy:

```yaml
collectors:
  - name: partner_api
    request:
      type: http
      proxy_url: http://egress.partner.example:3128
      proxy_basic_auth_file:
        username: /run/secrets/egress/username
        password: /run/secrets/egress/password
  - name: in_cluster
    request:
      type: http
      proxy_url: direct     # no proxy, whatever HTTPS_PROXY says
```

- **`proxy_url`** is `http://host:port`, `https://host:port` or
  `socks5://host:port` (also `socks5h://`), and nothing else: no path, no
  query, no credentials, no placeholder. `direct` reaches the targets
  without a proxy. When it is left out, the environment's proxy applies, as
  above.
- **Everything that the collector sends goes through it**: its requests,
  their retries, pages and followed redirects, and its
  [login](AUTHENTICATION.md#logging-in-to-a-target). A collector has no
  `NO_PROXY`: `localhost` goes through the proxy too.
- **`http://`** is contacted in plain text. An `http` target's request is
  sent whole, and an `https` target's through a `CONNECT` tunnel, with TLS
  end to end with the target and its [TLS settings](#tls) unchanged.
- **`https://`** is the same over TLS to the proxy. The proxy is verified
  against the system's roots (on Linux those that `SSL_CERT_FILE` and
  `SSL_CERT_DIR` name, when set), without a client certificate. The
  collector's `tls` block belongs to the target, and its client certificate,
  CA and `server_name` are not used with the proxy.
- **`socks5://`** is asked to connect to the target by name, which the proxy
  resolves.
- **Credentials** come from the files of `proxy_basic_auth_file`, its
  `username` and `password`. They are read at every request, so a rotated
  Secret is picked up, and sent as `Proxy-Authorization` (`http` and
  `https`) or as the SOCKS username and password. A `proxy_url` with
  credentials in it is refused when the configuration loads, without quoting
  them. So are credentials without a `proxy_url` to send them to. A
  credential file that cannot be read, or is empty, fails the request
  immediately, without its retries, as a
  [`basic_auth_file`](AUTHENTICATION.md) does.
- **Target lists** apply to the target, as behind the environment's proxy:
  to its name, and to the addresses that the exporter resolves it to
  ([Restricting targets](#restricting-targets)). The proxy's own address
  comes from the configuration and is not held to `allowed_targets` and
  `denied_targets`.
- **Connections.** Collectors with different proxies, or with different
  credentials for one proxy, never share a connection
  ([Connections](#connections)).
- **No [`ip_family`](#address-families)** beside a `proxy_url` other than
  `direct`: the proxy chooses the family over which it connects to the
  target.
- **`--dry-run`** lists, for each `http` and `graphite` collector, the proxy
  in force under `request_policies` as `proxy`: its `proxy_url` (its own or
  from its file's `defaults`), `direct`, or `environment` when it names none
  and follows `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY`. It shows the files
  of `proxy_basic_auth_file`, never their content. What the environment
  names is not shown, because a proxy URL there may hold a password.
- **Configuration only.** No probe parameter sets the proxy, and neither
  does a static target: a target behind another proxy needs its own
  collector. A file's
  [`defaults`](CONFIGURATION.md#defaults-for-the-collectors-of-a-file) may
  give its `http` and `graphite` collectors their proxy and the files of its
  credentials.
  - The credentials of a `defaults` block that names the proxy are for that
    proxy only. A collector that writes its own `proxy_url` (another proxy,
    or `direct`) does not take them, and writes its own credentials if its
    proxy requires some, as `partner_api` below does. `--dry-run` does not
    list them among the keys it took.
  - Credentials in `defaults` without a `proxy_url` are for the collectors
    that name their own proxies. Every collector that does takes them; one
    that goes `direct` does not.
  - Credentials that no collector takes are refused, like any key of
    `defaults` that applies to no collector.

  ```yaml
  defaults:
    request:
      proxy_url: http://proxy.corp.example:3128
      proxy_basic_auth_file:          # for proxy.corp.example alone
        username: /run/secrets/corp-proxy/username
        password: /run/secrets/corp-proxy/password
  collectors:
    - name: billing                   # through proxy.corp.example, with its credentials
      request: {type: http}
    - name: partner_api               # its own proxy, and its own credentials for it
      request:
        type: http
        proxy_url: http://egress.partner.example:3128
        proxy_basic_auth_file:
          username: /run/secrets/egress/username
          password: /run/secrets/egress/password
  ```

### Over a Unix socket

An `http` collector can send its requests over a Unix domain socket instead
of a TCP connection to the URL's host, as `curl --unix-socket` does.
Examples are Docker's API on `/var/run/docker.sock`, a php-fpm or nginx
status page served on a socket, or the socket of a systemd service:

```yaml
collectors:
  - name: docker
    request:
      type: http
      unix_socket: /var/run/docker.sock
      path: /v1.43/info
```

A probe of `target=http://docker` sends `GET /v1.43/info` with
`Host: docker` to the socket.

- **`unix_socket`** is the absolute path of the socket.
  - These are refused when the configuration loads: a relative path, a path
    of only blanks, and a path with a placeholder or a control character.
    `""` is the same as leaving the key out.
  - A path longer than the system's socket addresses hold (107 bytes on
    Linux, 103 on macOS) is refused too, because it could never be connected
    to. The message gives its length and the limit. The schema leaves the
    length to the exporter, because it differs between systems.
  - Linux's abstract sockets, written with a leading `@`, are not absolute
    paths and are refused.
  - The key belongs to the `http` type and the
    [`tcp`](TCP.md#over-a-unix-socket) type. A collector of another type
    that sets it is refused, like any key of another type.
- **The URL keeps its meaning.** Its host is the `Host` header, and its path
  and query are the request's, joined with `path` and `query` as usual. The
  host is never looked up or dialed, so any name works, such as
  `http://docker` or `http://localhost`.
- **`https://`** URLs are requested with TLS over the socket. The URL's host
  is the server name that the certificate is verified for, with the
  collector's [TLS settings](#tls).
- **Everything that the collector sends goes to the socket**: its requests,
  their retries, pages and followed redirects (whatever host a redirect
  names), and its [login](AUTHENTICATION.md#logging-in-to-a-target).
- **No proxy.** The socket takes the place of a proxy. `proxy_url` and
  `proxy_basic_auth_file` beside `unix_socket` are refused, the
  environment's proxy does not apply, and the collector takes neither from
  its file's
  [`defaults`](CONFIGURATION.md#defaults-for-the-collectors-of-a-file).
  `--dry-run` lists the collector under `request_policies` with its
  `unix_socket` and no `proxy`. [`ip_family`](#address-families) beside
  `unix_socket` is refused too, because the socket's connection has no
  address family.
- **Configuration only.** No probe parameter sets the socket, and neither
  does a static target or `defaults`. Whoever can reach `/probe` names the
  URL, whose host only goes into the request, and never the file that the
  exporter connects to. The socket's path is trusted like the rest of the
  configuration and is not held to `allowed_targets` and `denied_targets`.
- **Target lists** apply to the URL's host as written: a name is matched
  against the names and globs, and an address written as the host against
  the addresses and networks. The name is not looked up, because the
  connection goes to the socket and not to an address. A name that
  `allowed_targets` does not list by name is therefore refused even when the
  list holds networks.
- **Connections.** Collectors with different sockets never share a
  connection, and none shares one with a collector that dials TCP
  ([Connections](#connections)).
- **Errors.** A socket that cannot be reached fails the request with an
  error that names its path, `dial unix /var/run/docker.sock: connect:
  permission denied`, in the exporter's log. The caller of a probe gets the
  error with the path withheld
  ([What a failed probe's caller is told](CONFIGURATION.md#what-a-failed-probes-caller-is-told)).

The exporter needs read and write access to the socket file. In Kubernetes,
mount the socket's directory from the node with the chart's `extraVolumes`
and `extraVolumeMounts`, and give the pod the socket's group in
`podSecurityContext.supplementalGroups` when the exporter's user, 65532,
does not own it:

```yaml
extraVolumes:
  - name: docker
    hostPath: {path: /var/run/docker.sock, type: Socket}
extraVolumeMounts:
  - name: docker
    mountPath: /var/run/docker.sock
podSecurityContext:
  supplementalGroups: [998]   # the group the socket belongs to on the node
```

Access to Docker's socket gives control of the node's containers. Give it
only to an exporter whose `/probe` can be reached only by people you trust,
and prefer a read-only socket proxy in front of it where you have one.

### FastCGI

An `http` collector can use FastCGI instead of HTTP with its target. This is
for an application that answers only on a FastCGI port or socket: PHP-FPM's
status and ping pages, which PHP-FPM serves to its FastCGI clients and to no
browser, or the status script of any FastCGI application.

```yaml
collectors:
  - name: php_fpm
    request:
      type: http
      fastcgi: {}                 # the status page needs no document root
      path: /status
      query: {json: ""}
    decoder:
      type: json
    transform:
      type: jq
    metrics:
      - name: phpfpm_active_processes
        expression: '."active processes"'
      - name: phpfpm_listen_queue
        expression: '."listen queue"'
```

A probe of `target=http://php-fpm:9000` asks PHP-FPM on port 9000 for
`/status?json`, as `cgi-fcgi -bind -connect php-fpm:9000` does with
`SCRIPT_NAME=/status SCRIPT_FILENAME=/status QUERY_STRING=json`. PHP-FPM
answers with its status when `pm.status_path` is `/status`. With
[`unix_socket`](#over-a-unix-socket), such as `/run/php/php-fpm.sock`, the
request goes to the socket and the URL's host only names it.

- **The request is the collector's.**
  - The decoded path of its URL is the script, `SCRIPT_NAME`. With
    `document_root` in front, it is the script's file, `SCRIPT_FILENAME`
    (without `document_root`, the path alone).
  - Its query is `QUERY_STRING`, and its method is `REQUEST_METHOD`.
  - Each header is an `HTTP_` parameter (`X-Pool` is `HTTP_X_POOL`).
    `Content-Type` and the body's length are `CONTENT_TYPE` and
    `CONTENT_LENGTH`. Its body is the application's stdin.
  - `Proxy` is not sent as a parameter, because `HTTP_PROXY` sets a proxy in
    many applications.
  - `GATEWAY_INTERFACE`, `SERVER_NAME`, `SERVER_PORT`, `REQUEST_URI`,
    `DOCUMENT_URI`, `DOCUMENT_ROOT`, `SERVER_ADDR` and `REMOTE_ADDR` are set
    too.
- **`params`** adds parameters, or replaces those above by name, such as
  `SCRIPT_FILENAME: /usr/share/php/status.php`. A name is letters, digits
  and `_`, and does not start with a digit. A value holds no placeholder and
  no control character.
- **The answer is read as an HTTP answer.** The application's `Status`
  header is the status, or `200` without one. `accept_status`, retries,
  decoders and rules therefore read it as they read an HTTP server's answer.
  What the application wrote to its stderr before its headers (for example
  PHP-FPM's `Primary script unknown` for a path that it has no script for)
  is the `X-Fastcgi-Stderr` header, limited to its first 4 KiB. A jq rule
  reads it as `$headers["x-fastcgi-stderr"]`.
- **Plaintext, to the application.** A target is an `http://` URL, or a bare
  `host:port` as Prometheus's discovery produces it, which is read as
  `http://`. The port is the application's FastCGI port, or `80` when the
  target names none.
  - `https` targets, `tls`, `proxy_url`, `proxy_basic_auth_file` and `login`
    are refused beside `fastcgi`, and the environment's proxy does not
    apply.
  - A redirect to an `https://` URL is not followed. It fails the request
    instead of leaving FastCGI for an HTTPS request to the host that it
    names.
  - `allowed_targets` and `denied_targets` apply to the connection as they
    apply to an HTTP request's.
- **One connection per request.** The application closes the connection once
  it has answered, which is how it answers a FastCGI client that does not
  ask it to keep the connection open.
- **Errors.** A target that answers with no FastCGI record, such as an HTTP
  server, fails the request: `the answer is no FastCGI record, version 72;
  is the target a FastCGI application's port?`. An application that closes
  the connection before it has ended the request fails it too.

## Retries

Retries can be configured in the collector and overridden for one scrape:

```yaml
request:
  retry:
    attempts: 2   # retries after the initial request
    backoff: 2s   # fixed delay between attempts
```

The exporter retries transport failures and the HTTP statuses that
`retry.statuses` lists. By default these are the transient ones: `408`,
`425`, `429` and `5xx`. Other HTTP statuses are returned immediately.

- Two failures are not retried, because the same request would fail in the
  same way: a target that the collector's lists
  [refuse](#restricting-targets), and response headers over
  [their bound](#compression-and-the-response-size).
- An HTTP/2 connection closed for a protocol error before the answer's
  headers were read is retried once, on a new connection, because the error
  may be for the answer of another request on that connection. The second
  such error ends the request.
- A connection that breaks while the body is being read (reset, or closed
  before the length it promised) counts as a transport failure and is
  retried too, unless the probe's time has run out. A
  [debug probe](CONFIGURATION.md#debug-probes) shows such an attempt as its
  status followed by `then the body broke off`.
- A `401` to a collector that
  [logs in](AUTHENTICATION.md#logging-in-to-a-target) is sent once more with
  the token of a new login, whatever the method. That does not count as an
  attempt of `retry.attempts`, and it is made within the probe's deadline.

Only requests with an idempotent method are retried (`GET`, `HEAD`,
`OPTIONS`, `TRACE`, `PUT` and `DELETE`), because sending a `POST` or `PATCH`
again may repeat its effect. When a target is known to handle a repeated
`POST` safely, allow it:

```yaml
request:
  method: POST
  retry:
    attempts: 2
    non_idempotent: true   # retry this POST too
```

Without it, a `POST` collector with `retry.attempts` is logged as a
configuration warning at startup, and its failed requests are not retried.

- The retry count and the fixed delay can be overridden for one scrape with
  the `retry_attempts` and `retry_backoff` probe parameters.
- Retries share the timeout of the scrape or target, so the retry loop
  cannot extend the configured deadline.
- `attempts` is at most 10, in the configuration, in a static target and in
  the `retry_attempts` parameter. Whoever can reach `/probe` chooses the
  parameter, and without a bound one probe of a failing target could send it
  requests in a tight loop until its deadline.

When the deadline, or a shutdown, cuts short the wait before a retry, the
probe still reports what the target last answered. A `503` remains a failed
`http_status` stage with the target's body in the log and
`http_exporter_scrape_http_status_code` at 503, and the error adds that the
probe ran out of its budget. After a failed connection, the error is that
connection error, with a note that the wait before the retry was cut short.

A status that the collector [accepts](#accepting-other-statuses) is its
answer and is not retried, even a `503`.

`retry.statuses`, for `http` and `graphite` collectors, sets which statuses
are retried. It is written like
[`accept_status`](#accepting-other-statuses): statuses from 100 to 599 and
classes such as `5xx`. An entry that is neither is refused with the same
message. A list replaces the default set instead of adding to it, so include
the default statuses that should still be retried. This example is for a
service that answers `404` while it is still starting, and whose `5xx` are
retried as before:

```yaml
request:
  retry:
    attempts: 3
    backoff: 2s
    statuses: [404, 408, 425, 429, "5xx"]   # 404 added to the default
```

This example is for a target whose `500` and `501` mean that the request
itself is wrong, which another attempt cannot change, and whose gateway
errors are worth a retry:

```yaml
request:
  retry:
    attempts: 2
    statuses: [429, 502, 503, 504]   # 500 and 501 are no longer retried
```

- An empty list, `statuses: []`, retries no status at all, but a connection
  that failed, or broke while its body was read, is still retried.
  `attempts: 0` is what turns retries off completely.
- An entry whose every status the collector accepts is refused when the
  configuration loads, because an accepted status is the answer and that
  entry could never be retried: `retry.statuses: [503]` with
  `accept_status: ["2xx", 503]`, and `retry.statuses: [200]` with no
  `accept_status`, which accepts every 2xx.
- A class of which only some statuses are accepted is allowed, and the
  others are retried: with `retry.statuses: ["5xx"]` and
  `accept_status: ["2xx", 503]`, a `503` is the answer and any other `5xx`
  is retried.
- Any other status may be listed, including a 2xx that `accept_status`
  leaves out and a 3xx that is not followed.
- `retry.statuses` without `attempts` changes nothing until a static
  target's `retry.attempts`, or the `retry_attempts` probe parameter, sets
  some, as with `backoff`.
- A [static target](STATIC-TARGETS.md) may replace it with its own
  `retry.statuses`. No probe parameter sets it.
- `--dry-run` lists a collector's `retry.statuses` under `request_policies`,
  and a [debug probe](CONFIGURATION.md#debug-probes) says, below its
  requests, how often and on which statuses its request is retried.

A `grpc` collector retries by gRPC status code instead, with `retry.codes`
(`[UNAVAILABLE]` by default), and has no `non_idempotent`, `statuses` or
`retry_after`. `retry.codes` is refused for every other type, and
`retry.statuses` and `retry.retry_after` are refused for `grpc`. See
[gRPC](GRPC.md#errors-and-retries).

### Waiting as Retry-After asks

A target that rate-limits or is briefly down often says when to come back,
in a `Retry-After` header on its `429` or `503`. The exporter ignores the
header unless a collector enables it with `retry.retry_after`, for `http`
and `graphite` collectors. Without it every retry waits for `backoff`,
whatever the target says:

```yaml
request:
  retry:
    attempts: 2
    backoff: 1s          # still the wait after a failed connection
    retry_after: true    # a retried 429 or 503 waits what its Retry-After asks
```

With it, an answer that is retried (its status is listed in
`retry.statuses`, it is not accepted, and an attempt is left) waits for the
time that its `Retry-After` gives instead of `backoff`. A target that
answers

```
HTTP/1.1 429 Too Many Requests
Retry-After: 3
```

is asked again 3 seconds later.

- The value is a number of seconds, digits only, or an HTTP date such as
  `Wed, 21 Oct 2026 07:28:00 GMT`. The exporter waits until that date by its
  own clock, not by the answer's `Date` header. A date in the past means no
  wait.
- A value that is neither (`soon`, `-1`, `1.5`, `+3`) is ignored, and the
  wait is `backoff`, as without the header.
- A status that is not retried is the answer, whatever its `Retry-After`. So
  is the answer of the last attempt.
- The wait after a failed connection, which has no answer to carry the
  header, is still `backoff`.
- The `401` that is resent with the token of a new
  [login](AUTHENTICATION.md#logging-in-to-a-target) is not affected.

The probe's deadline bounds the wait, and there is no other bound. A
`Retry-After` that asks for longer than the probe has left is not waited for
at all, because the retry could not be sent in time and the probe would fail
in the same way later. The answer is then immediately the probe's answer: a
failed `http_status` stage whose error says why it was not retried:

```
received HTTP status 503, not retried: its Retry-After asks for 2m0s, longer than the 9.2s left of the probe's time
```

A [static target](STATIC-TARGETS.md) may set `retry.retry_after` to either
value, which replaces the collector's. No probe parameter sets it.
`--dry-run` lists it under `request_policies` as `retry_after: true`. A
[debug probe](CONFIGURATION.md#debug-probes) says in its retry line that the
`Retry-After` of a retried answer replaces the backoff. Below each request
that was retried it shows the wait taken and why, `then waited 3s, as
Retry-After asked`, or `then waited 1s, retry.backoff; the answer has no
Retry-After`, and below the last one why it was not retried.

## Accepting other statuses

A response is decoded when its status is 2xx. Any other status fails the
scrape in the `http_status` stage. Some targets answer with a useful body
under another status: a health endpoint that reports its checks with `503`,
or an API that answers `404` with a JSON body for an empty queue.
`accept_status` lists the statuses whose answers are decoded, for `http` and
`graphite` collectors:

```yaml
request:
  type: http
  path: /health
  accept_status: ["2xx", 503]   # statuses from 100 to 599, or classes such as 2xx
transform:
  type: jq
metrics:
  - name: app_healthy
    expression: 'if $status == 200 then 1 else 0 end'
  - name: app_check_up
    items: .checks[]
    expression: 'if .ok then 1 else 0 end'
    labels:
      - name: check
        expression: .name
```

Listing statuses replaces the default, so write `2xx` to keep the successful
ones.

- A status written as a number is the status that YAML reads, however it is
  written: `503.0`, `5.03e2`, `0x1F7`, `+503` and `0503` are all 503, as
  they are for the [schema](CONFIGURATION.md#editor-support).
- In quotes it is text, and is held to what the schema accepts: the three
  digits of a status, `"503"`, or a class, `"5xx"` or `"5XX"`.
- These are refused, with an error that names the collector or the static
  target and the entry: text with a sign, leading zeros or blanks (`"+503"`,
  `"0503"` or `" 503 "`), `"503.0"`, a number with a fraction such as
  `503.5`, and a number outside 100 to 599 such as `6e2`.
- An accepted status is not [retried](#retries), and an entry of
  `retry.statuses` that is accepted as a whole is refused.

**Reading the status and headers.** jq and yq rules read the status as
`$status` and the headers as `$headers`, an object of lower-case header
names, each with its values joined by `, `: `$headers["retry-after"]`. A
Python script has them as `response.status_code` and `response.headers`,
which keeps each header's values as a list under its canonical name
(`response.headers["Retry-After"]`). `response.header("retry-after")` joins
them with `, ` as `$headers` does. For a `grpc` collector `$status` is the
call's status code, `0` for `OK` (see
[`accept_codes`](GRPC.md#errors-and-retries)). For a `localfile` or
`sshfile` collector, which has no status, it is `null`.

`accept_status: [any]` accepts every status from 100 to 599. It is for a
collector that reports whatever the target answers instead of failing on
some of it, such as an uptime check whose rules compare `$status`:

```yaml
request:
  type: http
  method: HEAD
  accept_status: [any]   # every status from 100 to 599 is decoded
```

With [`decoder.type: none`](CONFIGURATION.md#no-decoder) beside it, no body
is decoded either, so an empty or HTML answer causes no failure.

- `any` is written alone and in lower case. Beside another entry
  (`[any, 503]`) or as `ANY`, it is refused when the configuration loads, as
  the schema refuses it.
- A status above 599, which some servers send, still fails in the
  `http_status` stage.
- Because an accepted status is not retried, `retry.statuses` with entries
  is refused beside `[any]` (an empty list is allowed), and a retry is made
  only on a failed connection. `retry.statuses` itself takes no `any`.
- A redirect that `follow_redirects` follows is followed as before, and the
  status of the answer that it leads to is the one accepted.
- A [static target](STATIC-TARGETS.md) may set its own `[any]`, or replace
  the collector's `[any]` with a list.

A `101 Switching Protocols` has no body: what follows it on the connection
is the protocol that the server switched to. Its answer is only the status
and the headers. The connection is closed immediately, unread, and never
kept for another request, so the frames of a
[WebSocket endpoint](#websocket-endpoints) are not read as a body until the
probe's deadline.

## Timings, TLS and the final URL

In addition to `$status` and `$headers`, jq and yq rules of an `http` or
`graphite` collector can read facts about the request other than its answer.
A Python script has the same on its `response`:

| jq and yq | Python | What it is |
| --- | --- | --- |
| `$timings` | `response.timings` | How long the request's phases took, in seconds |
| `$tls` | `response.tls` | The TLS connection the request was sent on, and the target's certificate |
| `$url` | `response.url` | Where the request went, after the redirects it followed |
| `$redirects` | `response.redirects` | How many redirects it followed, `0` for none |
| `$http_version` | `response.http_version` | The version of HTTP the answer came in: `"1.0"`, `"1.1"` or `"2"` |
| `$remote_ip` | `response.remote_ip` | The IP address the request's connection was made to |

```yaml
transform:
  type: jq
metrics:
  - name: app_request_seconds
    items: '$timings | to_entries[] | select(.value != null)'
    expression: .value
    labels:
      - name: phase
        expression: .key
```

`$timings` is an object with five numbers of seconds, measured on the
request with Go's `net/http/httptrace`:

| Key | The time |
| --- | --- |
| `dns` | to look the target's name up |
| `connect` | to connect to it, from the first address tried to the one that took the connection |
| `tls` | for the TLS handshake |
| `first_byte` | from the request's start, when it asked for a connection, to the first byte of the answer |
| `total` | from the request's start to the end of reading the body |

A phase that the request did not have is `null`, not `0`:

- On a connection kept from an earlier request there is no lookup,
  connection or handshake. [`new_connection`](#connections) gives every
  request all three.
- Over plain HTTP there is no handshake.
- To an IP address or over a [Unix socket](#over-a-unix-socket) there is no
  lookup.
- Through a [proxy](#proxies) the lookup and the connection are the proxy's,
  and the handshake is the target's through the tunnel. A connection to an
  `https://` proxy is made by the exporter itself, and its handshake is not
  timed.

A request is timed only when something can read its timings: when a jq or yq
text of the collector names `$timings` or `$remote_ip` (which the same trace
provides), when the collector runs a Python script, and in a
[debug probe](CONFIGURATION.md#debug-probes) or on the debug page, whose
report shows them. The trace costs every request a few allocations, which a
collector that reads none of it does not pay.

The values belong to one request: the one whose status is `$status`.

- For a [retried](#retries) request, that is the last attempt.
- For a [redirect](#redirects-and-http2), it is the request that the
  redirect led to, not the one redirected. A redirect to another host
  therefore has its own lookup, connection and handshake.
- For a [paginated](#pagination) collector, it is the first page, or the
  page whose status failed the scrape.
- A
  [revalidated response](CONFIGURATION.md#revalidating-expired-responses)
  has the values of the conditional request, whose `304` is answered with
  the cached status and body.
- A probe that the [response cache](CONFIGURATION.md#response-caching)
  answers runs no rule. Its series are those of the scrape that filled the
  cache, including the timings.

`$tls` is `null` over plain HTTP. Otherwise it is an object that describes
the connection on which the answer of the same request came, and the
certificate that the target presented on it. It contains nothing secret:

| Key | What it is |
| --- | --- |
| `tls_version` | The TLS version, `"TLS 1.2"` or `"TLS 1.3"` |
| `cipher_suite` | The cipher suite, by its IANA name, `"TLS_AES_128_GCM_SHA256"` |
| `server_name` | The name sent as SNI: the URL's host or `tls.server_name`; `""` when none was sent, as to an IP address |
| `verified` | Whether the exporter verified the chain: `false` under `insecure_skip_verify`, which is the only way a chain that does not verify is answered |
| `not_before`, `not_after` | The target's certificate's validity, in Unix seconds |
| `subject_cn`, `issuer_cn` | The common names of its subject and its issuer |

The names and values are those of the document of the
[`tlscert` type](TLSCERT.md#the-document), so a rule reads the two in the
same way. `$tls.not_after` is when the certificate that the target serves
expires, which is what an uptime check alerts on. A chain's intermediates
and roots, and a certificate on a port that does not speak HTTP, are covered
by the `tlscert` type. Through an `https://` proxy, a plain HTTP request has
no `$tls`: the connection it came on is the proxy's.

`$url` is the URL of the same request: the target's or, with
[`follow_redirects`](#redirects-and-http2), the URL that the redirects led
to. `$redirects` is how many redirects were followed to get there: `0`
without `follow_redirects` or when the target answered directly.

`$url` is shown as a target is shown in a log line:

- Its userinfo and its fragment are left out.
- The value of a query parameter whose name looks like a credential's is
  `<redacted>`: `token`, `api_key`, `password`, `sig`, `code` and `ticket`
  (which the redirect back from a login carries), and similar names
  ([the rule](LOGGING.md#repeated-failures)).
- The value of a path parameter with such a name, after a `;` or an escaped
  `%3B`, is `<redacted>` too. A servlet's session is carried in the path in
  this way when cookies are off: `/app;jsessionid=<redacted>`.
- A debug report shows `$url` as the rules read it, with its escapes kept,
  and with every query value that the configuration did not set masked as
  well.

Every other parameter, and every segment of the path, is kept as it was
sent. A label of `$url` is therefore only as safe as that rule:

- An order number, a tenant or an e-mail address in a parameter with another
  name is kept.
- A URL that carries a parameter chosen by a redirect, such as a login
  page's `?next=`, produces a series for every value.
- A secret that is a whole segment of the path, such as a bot's token in
  `/bot123456:AAE…/getMe`, has no name to recognise it by and is kept.

Where that matters, keep only the URL's path,
`$url | split("?")[0] | split(";")[0]`, which ends it before its query and
its first path parameter, and put `$url` in a label only for sites whose
paths carry no secret.

`$http_version` is the version of HTTP in which the answer of the same
request came, `"1.0"`, `"1.1"` or `"2"`, as blackbox_exporter's
`probe_http_version` writes it. It is `"2"` only where
[`enable_http2`](#redirects-and-http2) let the request negotiate it.

`$remote_ip` is the IP address that the request's connection was made to,
whether the connection was kept from an earlier request or made for this
one: `"203.0.113.7"` or `"2001:db8::7"`. It is the address that the name
resolved to, in the family that the connection used
([Address families](#address-families)).

- It is `null` through a [proxy](#proxies), because the connection is made
  to the proxy's address, which is not an address of the target. It is also
  `null` over a [Unix socket](#over-a-unix-socket), which has no address.
- A name with several addresses, or one that moves, gives a value for each.
  `$remote_ip` in a label therefore produces a series for every address at
  which the site was reached. The [uptime recipe](#uptime-probes) keeps only
  the family, `4` or `6`.

`$timings`, `$tls`, `$url`, `$redirects`, `$http_version` and `$remote_ip`
are `null` for every other request type, and on a response pasted on the
[debug page](CONFIGURATION.md#the-debug-page). Every jq and yq program has
the variables, so a rule that reads them loads whatever the collector's
type. A pagination's `next` and a login's `token` expression read them as
`null` too. The report of a
[debug probe](CONFIGURATION.md#debug-probes) and the debug page show their
values under the response's status, in that order, with the query values of
`$url` masked as the report's request lines mask them.

## Uptime probes

An uptime check of HTTP and HTTPS sites, which is the job of the `http`
prober of
[blackbox_exporter](https://github.com/prometheus/blackbox_exporter), is one
collector:
[`configs/config.uptime.example.yaml`](../configs/config.uptime.example.yaml).

```yaml
collectors:
  - name: site
    report_failures: true
    request:
      type: http
      method: HEAD
      follow_redirects: true
      new_connection: true
      accept_status: [any]
    decoder:
      type: none
    transform:
      type: jq
    metrics:
      - name: site_up
        expression: 'if $status >= 200 and $status < 300 then 1 else 0 end'
      - name: site_http_status_code
        expression: $status
      - name: site_http_duration_seconds
        items: '$timings | to_entries[] | select(.value != null)'
        expression: .value
        labels:
          - name: phase
            expression: .key
      - name: site_http_redirects
        expression: $redirects
      - name: site_http_version
        expression: '$http_version | tonumber'
      - name: site_ip_protocol
        expression: '$remote_ip | if . == null then null elif contains(":") then 6 else 4 end'
        required: false   # none through a proxy
      - name: site_http_ssl
        expression: 'if $tls == null then 0 else 1 end'
      - name: site_tls_cert_expiry_timestamp_seconds
        expression: $tls.not_after
        required: false   # none over plain HTTP
      - name: site_tls_version_info
        items: '$tls | select(. != null)'
        expression: "1"
        required: false
        labels:
          - name: version
            expression: .tls_version
          - name: cipher_suite
            expression: .cipher_suite
```

What each key does:

- [`report_failures: true`](CONFIGURATION.md#reporting-failed-probes-as-metrics)
  answers a failed probe (a name that does not resolve, a refused
  connection, a timeout, a certificate that does not verify) with `200`,
  `http_exporter_probe_success 0` and the stage that failed, instead of
  `502` and no sample.
- `method: HEAD` requests no body. A site that refuses `HEAD` answers `405`.
  Use `GET` there; its body is read, up to `max_response_bytes`, and
  discarded.
- [`follow_redirects: true`](#redirects-and-http2) follows the site's
  redirects, so the status is that of the final destination, and
  `$redirects` counts them.
- [`new_connection: true`](#connections) makes every probe, and every
  request of its redirects, connect anew. Each one looks up the name,
  connects and verifies the certificate that the site serves now, and times
  all three. On a connection kept from the last probe, a site that moved to
  another address, or that now serves a certificate that does not verify,
  would still answer `site_up 1`, with the old certificate in `$tls`, until
  the connection closed.
- [`accept_status: [any]`](#accepting-other-statuses) makes every status the
  answer, so a `503` is `site_up 0` with its status instead of a failed
  probe.
- [`decoder.type: none`](CONFIGURATION.md#no-decoder) reads no body. The
  rules read `$status` and the variables of
  [Timings, TLS and the final URL](#timings-tls-and-the-final-url), never
  the document.
- `required: false` on the TLS rules: over plain HTTP `$tls` is `null`, and
  those series are left out instead of being reported as missing values. The
  same applies to `site_ip_protocol` through a proxy, where `$remote_ip` is
  `null`.
- `site_ip_protocol` is the family of the address at which the site was
  reached, read from [`$remote_ip`](#timings-tls-and-the-final-url), and not
  the address itself. A site behind several addresses, or one whose address
  changes, would produce a series for each address in a label. Where the
  address is needed, an info series for it takes one more rule,
  `expression: "1"` with a label of `$remote_ip`, at that cost.

Prometheus names each site as the probe's target:

```yaml
scrape_configs:
  - job_name: site
    metrics_path: /probe
    params:
      collector: [site]
    static_configs:
      - targets:
          - https://www.example.com
          - https://shop.example.com/health
          - http://intranet.example.internal
    relabel_configs:
      - source_labels: [__address__]
        target_label: __param_target
      - source_labels: [__param_target]
        target_label: instance
      - target_label: __address__
        replacement: exporter.example:8080
```

With the Prometheus Operator, the sites are a `Probe`, as for
blackbox_exporter, and the chart renders one for each of its `probes`
entries. See [Probes](PROMETHEUS-OPERATOR.md#probes), and the chart README's
[Uptime probes](../charts/prometheus-universal-exporter/README.md#uptime-probes)
for a complete values file.

These are the series of one probe of `https://example.com`, which redirected
once, to `https://www.example.com/`:

```text
site_up 1
site_http_status_code 200
site_http_duration_seconds{phase="connect"} 0.011
site_http_duration_seconds{phase="dns"} 0.004
site_http_duration_seconds{phase="first_byte"} 0.071
site_http_duration_seconds{phase="tls"} 0.024
site_http_duration_seconds{phase="total"} 0.072
site_http_redirects 1
site_http_version 1.1
site_ip_protocol 6
site_http_ssl 1
site_tls_cert_expiry_timestamp_seconds 1767225599
site_tls_version_info{cipher_suite="TLS_AES_128_GCM_SHA256",version="TLS 1.3"} 1
http_exporter_probe_success 1
http_exporter_probe_elapsed_seconds 0.149
```

| Series | What it says |
| --- | --- |
| `site_up` | `1` when the site answered with a `2xx` status, after its redirects; `0` for any other status. Absent from a probe that failed. |
| `site_http_status_code` | The status of the answer, after the redirects. |
| `site_http_duration_seconds{phase}` | How long each phase of the answered request took ([`$timings`](#timings-tls-and-the-final-url)): `dns`, `connect` and `tls` each on its own, and `first_byte` and `total` from the request's start. A phase the request did not have, such as `tls` over plain HTTP, or `dns` to an IP address, has no series; with `new_connection` every probe has the other two. |
| `site_http_redirects` | How many redirects were followed. |
| `site_http_version` | The version of HTTP the site answered in, after its redirects: `1.1`, `2`, or `1` for HTTP/1.0. |
| `site_ip_protocol` | `4` or `6`: the IP version of the address the answered request's connection was made to. Absent through a proxy. |
| `site_http_ssl` | `1` when the answered request was made over TLS. |
| `site_tls_cert_expiry_timestamp_seconds` | When the certificate the site served on this probe's connection expires, in Unix seconds; alert on it less `time()`. |
| `site_tls_version_info{version,cipher_suite}` | The TLS version of the connection, `TLS 1.2` or `TLS 1.3`, and the cipher suite it settled on, by its IANA name, so an alert can name a site still on an old version or a weak suite. |
| `http_exporter_probe_success` | `1` when the probe got an answer, whatever its status; `0` when it failed, with `http_exporter_probe_failed_stage{stage}` naming the stage. |
| `http_exporter_probe_elapsed_seconds` | How long the whole probe took, the redirects included. |

A site is down when either series says so: `http_exporter_probe_success ==
0` (no answer at all) or `site_up == 0` (an answer that is not a `2xx`).
Both carry the job's and the target's labels, so
`http_exporter_probe_success == 0 or site_up == 0` lists the sites that are
down.

### WebSocket endpoints

A WebSocket endpoint is up when it accepts the handshake (RFC 6455): a `GET`
that asks to upgrade, which the endpoint answers with `101 Switching
Protocols` and the `Sec-WebSocket-Accept` of the key. That is a separate
`http` collector, with the handshake's headers as its request headers:

```yaml
collectors:
  - name: websocket
    report_failures: true
    request:
      type: http
      method: GET
      headers:
        Connection: Upgrade
        Upgrade: websocket
        Sec-WebSocket-Version: "13"
        Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==   # any 16 bytes in base64
      new_connection: true
      accept_status: [101]
    decoder:
      type: none
    transform:
      type: jq
    metrics:
      - name: websocket_up
        expression: 'if $status == 101 and $headers["sec-websocket-accept"] == "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" then 1 else 0 end'
      - name: websocket_http_status_code
        expression: $status
```

- The four headers are sent as written. `Connection` and `Upgrade` are
  hop-by-hop headers, which a collector's `headers` may set (a probe's
  `header_` parameters never forward them). A request that asks for a
  WebSocket is sent over HTTP/1.1, even under
  [`enable_http2`](#redirects-and-http2) to a server that speaks HTTP/2,
  because HTTP/2 has no `Upgrade`.
- The key is any 16 bytes in base64, the same on every probe. It determines
  the `Sec-WebSocket-Accept` that the endpoint answers with: the base64 of
  the SHA-1 of the key followed by `258EAFA5-E914-47DA-95CA-C5AB0DC85B11`.
  The key above is RFC 6455's own example, whose answer is
  `s3pPLMBiTxaQ9kYGzzhZRbK+xOo=`. For another key,
  `printf '%s' "<key>258EAFA5-E914-47DA-95CA-C5AB0DC85B11" | openssl sha1
  -binary | base64` gives it. A rule that compares it distinguishes a
  WebSocket server from something that answers `101` without having read the
  key.
- [`accept_status: [101]`](#accepting-other-statuses) accepts the `101` as
  the answer and fails the probe on any other status, in the `http_status`
  stage, which `report_failures` reports. With `accept_status: [any]`,
  another status is `websocket_up 0` with its status instead.
- The `101` is the answer: only the headers. The connection is closed
  immediately, unread, so the probe ends as soon as the handshake is
  answered, however long the endpoint would keep sending.
- An endpoint behind `wss://` is the same with an `https://` target; the
  handshake is made over TLS.

The exporter checks only the handshake. It sends and reads no message.

### Compared with blackbox_exporter

This table lists the series of blackbox_exporter's `http` prober and its
address-family settings, with the equivalent here:

| blackbox_exporter | Here | Differences |
| --- | --- | --- |
| `probe_success` | `http_exporter_probe_success`, and `site_up` | `probe_success` is `0` for a status outside `valid_status_codes`, `2xx` by default; here a probe with an answer succeeds, and `site_up` judges its status, so the two together say what `probe_success` does. |
| `probe_duration_seconds` | `http_exporter_probe_elapsed_seconds` | The same: the whole probe. |
| `probe_http_status_code` | `site_http_status_code` | The same: the status after the redirects. |
| `probe_http_duration_seconds{phase}` | `site_http_duration_seconds{phase}` | blackbox_exporter's phases, `resolve`, `connect`, `tls`, `processing` and `transfer`, are consecutive and summed over every request of the redirects. Here they are of the answered request alone; `dns`, `connect` and `tls` are its `resolve`, `connect` and `tls`, and `first_byte` and `total` run from its start, so `processing` is `first_byte` less the three before it and `transfer` is `total` less `first_byte`. Both connect anew for every request, blackbox_exporter always and this collector by its `new_connection`, so both time a lookup, a connection and a handshake on every probe. A phase that did not happen, `tls` over plain HTTP or `dns` to an IP address, has no series, where blackbox_exporter reports `0`. |
| `probe_ssl_earliest_cert_expiry` | `site_tls_cert_expiry_timestamp_seconds` | blackbox_exporter's is the earliest expiry of the chain the site presented; here it is the site's own certificate's. Both are of the certificate the site presents now, on the probe's own connection. A [`tlscert`](TLSCERT.md) collector reads the whole chain, `chain.earliest_not_after`. |
| `probe_http_redirects` | `site_http_redirects` | The same. |
| `probe_http_version` | `site_http_version` | The same: `1.1` or `2`, of the last request. |
| `probe_ip_protocol` | `site_ip_protocol` | The same, `4` or `6`, but of the address the answered request's connection was made to, where blackbox_exporter's is of the address it resolved the target's name to; the two differ only after a redirect to another host. Through a proxy it has no series here. `probe_ip_addr_hash` has no counterpart: `$remote_ip` is the address itself. |
| `probe_http_ssl` | `site_http_ssl` | The same: whether the last request was made over TLS. |
| `probe_tls_version_info{version}` | `site_tls_version_info{version}` | The same, `TLS 1.3`, with the cipher suite as a label beside it. |
| `preferred_ip_protocol`, `ip_protocol_fallback` (settings of the module) | [`ip_family`](#address-families) (a key of the request, and a probe parameter) | `preferred_ip_protocol: ip4` with `ip_protocol_fallback: true` is `prefer_ip4`, and with `false` it is `ip4`; `ip6` alike. blackbox_exporter prefers `ip6` by default, and falls back only when the name has no address of the family it prefers; `prefer_ip6` falls back too when none of those addresses takes the connection. Left out here, a site is connected to over either family, as Go connects. |

Other series of blackbox_exporter have no rule above, but can be read from
what is available: `probe_http_content_length` is
`$headers["content-length"] | tonumber`, and
`probe_http_last_modified_timestamp_seconds` is
`$headers["last-modified"] | parse_time` (see the
[value-parsing helpers](CONFIGURATION.md#value-parsing-helpers)).

What this collector does not do:

- No ICMP: an `http` collector does not ping. A host's reachability is a
  connection to one of its ports, or the echoes of the [`icmp`](ICMP.md)
  type.
- No raw TCP: a port that speaks something other than HTTP is for the
  [`tcp`](TCP.md) type, and a name's records for the [`dns`](DNS.md) type.
- One certificate: the chain's intermediates and roots, a chain that does
  not verify and the reason, and a TLS port that does not speak HTTP are for
  the [`tlscert`](TLSCERT.md) type.
- No body check: blackbox_exporter's `fail_if_body_matches_regexp` is a
  `GET` with a decoder and a rule that reads the body, in a separate
  collector.

## One connection per scrape

`ldap`, `redis` and `tcp` collectors make a connection for each scrape and
close it when the scrape is over. They do not keep one between scrapes, as
`http`, `grpc`, `sql`, `sshfile` and `sshexec` do.

A scrape of these types sends a few requests, at most seconds apart. A
connection kept between scrapes, which are a minute or more apart, would
need its own keepalive, idle timeout and limit, like the kept connections of
the other types, and a way to distinguish a connection that the server
closed while idle from one that failed. A scrape would gain nothing visible
from this: each scrape costs a TCP handshake, a TLS handshake over TLS, and
for `ldap` a bind, which is a few milliseconds compared with the scrape's
interval. Exported traces show the cost as the `connection.*` spans of the
scrape's first request
([Traces of slow requests](OTLP.md#traces-of-slow-requests)).

## Pagination

An API that pages its answer gives one request only the first page. With
`request.pagination` an `http` collector also fetches the following pages
and merges them into one JSON document: the first page, in which the array
that holds its items, `items`, holds the items of every page in order. The
decoder, the rules and the limits then read that document as the answer, so
a collector written for one page reads every page unchanged.

```yaml
request:
  type: http
  path: /orgs/acme/repos
  query:
    per_page: "100"
  bearer_token_file: /etc/secrets/github-token
  pagination:
    type: link    # follow the Link header's rel="next"
    items: .      # each page is an array of repositories
    max_pages: 20
transform:
  type: jq
metrics:
  - name: github_repos
    expression: length
  - name: github_repo_stars
    items: .[]
    expression: .stargazers_count
    labels:
      - name: repo
        expression: .name
```

GitHub answers the first page of `/orgs/acme/repos` with

```text
Link: <https://api.github.com/organizations/1/repos?per_page=100&page=2>; rel="next",
      <https://api.github.com/organizations/1/repos?per_page=100&page=7>; rel="last"
```

and the collector requests page 2, then each page that its `Link` names,
until a page names none. `github_repos` is the number of repositories on all
pages, not only on the first.

`type` sets how the next page is found:

| `type` | The next page | Needs |
| --- | --- | --- |
| `link` | The URL of the `Link` header's `rel="next"` (RFC 8288), absolute or relative to the page's URL. | — |
| `next_url` | The URL the jq expression `next` reads from the page, absolute or relative to the page's URL. | `next` |
| `cursor` | The first page's URL with the value `next` reads from the page in the query parameter `param`. | `next`, `param` |
| `page` | A page number in `param`: `start`, 1 by default, on the first page, one more on each. An API that numbers its pages from 0 is paged with `start: 0`. | `param` |
| `offset` | An offset in `param`: `start`, 0 by default, on the first page, moved on by the number of items each page held, or by `size`. `size: 0` is `size` left out. | `param` |

```yaml
pagination:              # {"data":{"hosts":[...]},"links":{"next":"/api/hosts?page=2"}}
  type: next_url
  next: .links.next
  items: .data.hosts
```

```yaml
pagination:              # {"items":[...],"meta":{"next_cursor":"dXNlcjo0Mg=="}}
  type: cursor
  next: .meta.next_cursor
  param: cursor          # ...&cursor=dXNlcjo0Mg==
  items: .items
```

```yaml
pagination:              # /tickets?page=1, ?page=2, ... until a page has no tickets
  type: page
  param: page
  items: .tickets
```

```yaml
pagination:              # /events?offset=0, ?offset=50, ... with limit: "50" in request.query
  type: offset
  param: offset
  size: 50
  items: .events
```

- `items` is a jq path, such as `.items`, `.data.results` or
  `.data[0].rows`, or `.` for a page that is an array. An expression that
  names no place, such as `.items | length`, is refused when the
  configuration loads. A page whose items array is missing or `null` has no
  items.
- `next` is any jq expression over the page, with `$headers` and `$status`
  as in a [rule](#accepting-other-statuses), so a cursor sent in a header is
  `$headers["x-next-cursor"]`. It gives the next URL or cursor as text (a
  cursor may also be a number), and `null`, `false` or `""` when there is no
  next page.
- `param` replaces a parameter of that name in the target's own query.
  `request.query` may not set it, because the pagination sets it on every
  page.

**Where the pages end.** The pages end at the first page that names no next
one: no `rel="next"`, a `next` that gives `null`, `false` or `""`, or, for
`page` and `offset`, a page whose items array is empty or missing. They also
end at `max_pages`, which is 10 by default and at most 1000, including the
first page.

A trip that stops at `max_pages` while the last page named another one, or
for `page` and `offset` held items, is not a failure. It is answered with
the pages it fetched, counted in
`http_exporter_pagination_max_pages_reached_total`, and logged at warn
level, once and then sparingly while it repeats, until a trip fetches every
page again. Every page fetched is counted in
`http_exporter_pages_fetched_total` (see
[self-metrics](SELF-METRICS.md)).

**Each page is the collector's request.** It has the same method, body,
headers, credentials, TLS settings, retries and redirect rules, and the same
[`allowed_targets` and `denied_targets`](#restricting-targets), checked on
each page's connections. The target names the next page, and that page is
sent the collector's credentials. A next page that leaves the target's
origin (its scheme, host or port) therefore fails the trip before it is
requested. So does one that names a page already fetched, which would loop.

**Behind a `Host` header.** A target reached by its address with a
[`Host` header](#the-host-header), the collector's or a static target's,
writes its links with that `Host`: `https://10.0.0.5/items` with
`Host: api.example` is answered with
`Link: <https://api.example/items?page=2>; rel="next"`.

- A next page on the virtual host of the `Host` (the target's scheme, and
  the port of the `Host`, or the scheme's default when it names none) is
  also on the target's origin. It is fetched as the first page was: from
  `https://10.0.0.5/items?page=2`, with `Host: api.example`.
- `api.example` is never looked up. What it resolves to may be another
  server, which the target's `allowed_targets` and `denied_targets` were not
  checked against, and it would be sent the collector's credentials. At the
  target's address the page stays on the host that the policy allowed, at
  the virtual host that the configuration chose.
- A next page on another port or scheme of the virtual host, or on any other
  host, still fails the trip.

**Limits.** The probe's `timeout`, and the scrape's deadline, cover all
pages together. The response limit (`max_response_bytes`) applies to each
page, to the pages together, and to the merged document.

**JSON only.** The pages are read as JSON. A collector with
`request.pagination` decodes with `json`, which is what an omitted
`decoder.type` becomes; any other decoder is refused when the configuration
loads.

- A first page that is not JSON, or that is answered with a status that the
  collector does not [accept](#accepting-other-statuses), is the answer as
  it would be without pagination, and fails in the `decode` or the
  `http_status` stage.
- A later page with such a status fails the trip in the `http_status` stage
  with that page's status and body, and the error names the page: `received
  HTTP status 503 on page 3 of request.pagination,
  https://api.example.com/tickets?page=3`.
- A later page that is not JSON, items that are not an array, and a `next`
  that gives something other than text or a number fail the trip in the
  fetch stage, with an error that names the page.
- An answer of one page is the page as it came, byte for byte. An answer of
  several pages is the merged document written again as JSON, with the other
  keys, the status and the headers of the first page.

A [debug probe](CONFIGURATION.md#debug-probes) lists every page's request
with its number, `(page 2)`, and says how many pages the response is merged
from and whether the pages stopped at `max_pages`. A static target of the
collector pages through its own target and path in the same way.
`request.pagination` belongs to the collector: a static target cannot set
it, and a probe parameter cannot change it.

## Chained requests

Some APIs need more than one request for one answer: a request reads an
ID, and a second request sends it; or a list request reads names, and one
more request is made for each name. With `request.then`, an `http`
collector makes up to 13 requests per trip, all to its one target: its own
request, which is the chain's first and is named `first`, then up to 12
steps in order. A request can send values read from the answers before
it.

```yaml
collectors:
  - name: gitlab_pipeline
    request:
      type: http
      path: /api/v4/projects/{{param_project}}
      extract:
        id: {json: .id}
      then:
        - name: pipelines
          path: /api/v4/projects/{{chain.id}}/pipelines
          query:
            ref: main
            per_page: "1"
    transform:
      type: jq
    metrics:
      - name: gitlab_last_pipeline_success
        description: 1 when the last pipeline of main succeeded.
        expression: 'if .[0].status == "success" then 1 else 0 end'
```

Probed with `param_project=group%2Fapp`, the collector asks
`/api/v4/projects/group%2Fapp`, reads the project's numeric `id` from the
answer, then asks `/api/v4/projects/42/pipelines?per_page=1&ref=main`. The
last step's answer is the response, which the decoder and the rules read
as any `http` answer.

A step with `for_each` is a fan-out: it makes one request per entry of a
list read earlier. This collector reads every Jenkins job, then each job's
last build:

```yaml
collectors:
  - name: jenkins_jobs
    request:
      type: http
      path: /api/json
      query: {tree: "jobs[name]"}
      extract:
        jobs: {json: '[.jobs[].name]'}
      then:
        - name: last_build
          for_each: jobs
          path: /job/{{each}}/lastBuild/api/json
          concurrency: 4
          max_items: 200
          on_item_error: skip
    answer: all
    transform:
      type: jq
    metrics:
      - name: jenkins_last_build_seconds
        description: Duration of each job's last build.
        items: .last_build[] | select(.body)
        expression: .body.duration / 1000
        labels:
          - name: job
            expression: .each
```

With `answer: all`, the response is one JSON document of every answer,
keyed by step name:

```json
{"first": {"status": 200, "body": {"jobs": [{"name": "api"}, {"name": "web"}]}},
 "last_build": [{"each": "api", "status": 200, "body": {"duration": 81000}},
                {"each": "web", "error": "received HTTP status 404"}]}
```

### The keys of a step

| Key | Default | What it does |
| --- | --- | --- |
| `name` | required | Unique in the collector: lower-case letters, digits and `_`, not `first`. It names the step in errors, self-metrics and the document. |
| `method` | `GET`, or `POST` with a body | `GET`, `HEAD`, `POST`, `PUT` or `PATCH`. |
| `path` | the first request's path | A path on the target, joined to it as `request.path` is. It names no scheme and no host. |
| `url` | none | One placeholder of a link a step read, alone, such as `"{{chain.next}}"`, in place of `path`. |
| `query` | none | The step's query parameters, after the query the target's URL has, which every step with a `path` sends. The collector's `request.query` is the first request's alone. With `url`, they are added to the link's query. |
| `headers` | the collector's | Added to the collector's headers, each replacing one of the same name. Not `Authorization`, nor the header a login sends its token in. |
| `body`, `form`, `json` | none | At most one: a body written as `request.body` is, a form, or a JSON object of text values. |
| `accept_status` | the collector's | The statuses that count as an answer for this step. |
| `forwarded_headers` | `true` | `false` sends the step none of the forwarded headers — the `header_<name>` probe parameters and those `request.forward_headers` lets through — but a forwarded `Authorization`, the collector's credential, and a forwarded `Host`, which keeps the step on the first request's virtual host. |
| `extract` | none | The values read from this step's answer. |
| `for_each` | none | The name of a list value read earlier: one request per entry. |
| `concurrency` | `4` | With `for_each`: how many of its requests run at once, 1 to 16. |
| `max_items` | `100` | With `for_each`: how many entries are asked for, 1 to 1000. The rest are left out, counted and logged; the trip still succeeds. |
| `on_item_error` | `fail` | With `for_each`: `fail` ends the trip at an item that fails; `skip` keeps the item in the document as an error and goes on. |

Every step uses the collector's TLS settings, proxy, `ip_family`,
`new_connection`, retries, rate limit and credentials. A step sets none of
them itself.

### Values and placeholders

`request.extract`, and a step's `extract`, read named values from an
answer, at most 16 per step. A value is read as a [steps
login](AUTHENTICATION.md) reads one, from exactly one place:

| Key | The value is |
| --- | --- |
| `regex` | the first group of the first match on the body, or the whole match without a group |
| `json` | what a jq expression gives of the body read as JSON: text, a number, or a list, which `for_each` goes over |
| `form` | a field of the body read as a form |
| `header` | a header of the answer |
| `cookie` | the value of a cookie the answer sets |

A value that is not found, or is empty, fails the step that reads it:
`step "first": value "id" was not found in the answer`. With `optional:
true` it is the empty text instead. With `secret: true` it is shown as
`***` wherever the exporter shows it: in the debug report and the
workflow diagram, in a step's URL and headers, in an error, and in a
request's span. No value is ever written to the log, secret or not, but
for the URL of a request at debug level, in which only a secret one is
masked.

A later step sends a value as `{{chain.<name>}}`, in its `path`, `query`,
`headers`, `body`, `form` and `json` values, and its `url`. A value is
escaped as its place needs: as one path segment in `path`, as a query value
in `query`, as written in a header, and in `body` as its filter says, such
as `{{chain.id|json}}`. A list value is written as its JSON text. In a
fan-out step, `{{each}}` is the entry, `{{each.<field>}}` a field of an
entry that is an object, and `{{each_index}}` its place, from 0. The values
a fan-out step reads are lists, one entry per item. `{{param_<name>}}` and
the [clock placeholders](#dates-and-times) work in every step.

A fan-out over an empty list makes no request, and is a step that went
through. A `for_each` value that is not a list fails the step.

The load refuses a value that a step sends before a step reads it, a value
that no step sends or fans out over, and `{{each}}` outside a fan-out step.
A value lives for one trip.

### One target only

Every request of a chain goes to the target's origin: the scheme, host and
port of the first request's URL.

- A step's `path` names no scheme and no host; `//x.com/a` is refused when
  the configuration loads.
- A step's `url` is a link read from an answer, resolved against the URL of
  that answer. A link to another origin fails the step in the
  `target_policy` stage: `step "detail": the link "https://x.com/a" leads
  to another origin than the target's, https://api.example; a chain asks
  one target only`. The one exception is a link that names the virtual
  host the request's `Host` header asks for, which is asked of the
  target's address with that `Host`, as a [next page](#pagination) is.
- In a collector with `request.then`, every request, the first included,
  follows a [redirect](#redirects-and-http2) only to the target's origin,
  and `request.redirect_trusted_hosts` is refused.
- A target given by its address with a `Host` header keeps that `Host` on
  every step.
- `allowed_targets` and `denied_targets` are checked for the target and
  apply to every step.
- A `request.login` still goes to its own `url`, such as a sign-on host. It
  is authentication, not a step of the chain.

### The answer

`answer: last`, the default, makes the last step's answer the response:
its status, headers and body, read by any decoder. The earlier steps only
give values. A fan-out step cannot be the last step under `answer: last`.

`answer: all` makes one JSON document of every answer. The decoder is
`json`; another is refused. Each answer must be JSON, or the step fails. A
step is `{"status": …, "body": …}`, a `HEAD` step's body is `null`, and a
fan-out step is a list in item order of `{"each": …, "status": …, "body":
…}` or `{"each": …, "error": "…"}`. `$status` and `$headers` are the first
request's.

### Time, retries and the response limit

The whole chain shares one budget: the probe's deadline, or a static
target's interval. A probe's `timeout` bounds the chain, not each request.
A fan-out that the deadline cuts short fails saying how far it got: `step
"last_build": the trip's deadline came after 12 of 40 items`. For a fan-out
of more than a few dozen items, scrape a [static
target](STATIC-TARGETS.md) with `cache.ttl` rather than a probe, which
Prometheus's scrape timeout bounds.

Retries are per request. A `POST`, `PUT` or `PATCH` step is retried only
with `retry.non_idempotent`. The collector's rate limits count every
request. `max_response_bytes` bounds each answer, and the document of
`answer: all` as it is built.

### Credentials, logins and cookies

Every step sends the collector's credentials, signed with
`request.signature` or `request.aws_sigv4` when the collector signs. A
`request.login` sends every step the token it keeps, as it does for one
request, and logs in only when that token has expired. A `401`, or a
status in `login.relogin_on`, at any step logs in again and sends that
request once more, at most once per trip. The steps of a trip share a
cookie jar: a cookie that one answer sets is sent with the later steps. The
jar starts empty at each trip.

### What a chain does not take

`request.then` is refused beside `request.pagination`,
`request.redirect_trusted_hosts` and `cache.revalidate: true`, and in any
request type but `http`. A chained collector's cached result is never
revalidated: one answer's validators cannot speak for the others. A static
target cannot set `then`, `extract` or `answer`.

A probe's parameters that change the request, such as `path`, `method` and
`body`, change the first request alone, and `path` also the steps that
leave `path` and `url` out, which ask the first request's path.
`param_<name>` values fill every step. Forwarded headers, `header_<name>`
among them, go with every step but one that sets `forwarded_headers:
false`, such as a step of an API that refuses a header the first request
needs. The cache, shared trips and `failure_backoff` work on the whole
chain, which is one trip.

### Errors, self-metrics and the debug report

A failed step fails the trip in the stage a request's failure has, and the
error starts with the step, and for a fan-out item with its place: `step
"last_build" item 3 of 40 ("web"): received HTTP status 404`. The log line
adds `step`, `item` and `items`. Under `on_item_error: skip`, the items
that failed are summed in one warning, `step "last_build": 3 of 40 items
failed and were skipped`, logged once and then sparingly while it repeats. With `report_failures`,
`http_exporter_probe_failed_stage` has a `step` label.

The [self-metrics](SELF-METRICS.md#chained-requests) count each step by
name. jq and yq read each request as `$steps`, and a Python script as
`steps` (see [CONFIGURATION](CONFIGURATION.md#steps-of-a-chain)).

A [debug probe](CONFIGURATION.md#debug-probes) lists the chain, one line
per step, with the values each read; a value marked `secret` shows as
`***`. Each of its requests names its step, and a fan-out item its place:
`GET https://ci.example/job/web/lastBuild/api/json (step last_build, item
2) -> 404 Not Found`. The debug page and the status page draw it as a workflow: a block
per step, with arrows between them. A step that went through is green; one
that failed or timed out is red, with its error inside; a fan-out is a
stacked block, amber when its failed items were skipped; a step not reached
is grey and dashed. Each block also says its state in words. On the debug
page, each block links to its step's details, and a pasted response takes
one text area per step for the answers of the steps; `--dry-run` lists each
step with the values it reads and sends.

## Rate limiting

A target that misbehaves when it is asked too often, such as an old
appliance or a legacy application server, can be limited to a set rate,
however many probes, static targets, retries and pages ask it at once:

```yaml
request:
  rate_limit:
    requests_per_second: 2   # on average; 0.5 is one request every 2 seconds
    burst: 1                 # requests sent at once after a pause; 1 when left out
```

`request.rate_limit` belongs to `http` and `graphite` collectors. A
collector of any other type that sets it is refused when the configuration
loads, like any key that its type does not take. `requests_per_second` is
required and is a number greater than 0. `burst`, when set, is at least 1.

**Scope.** The limit is kept for each origin (scheme, host and port) that
the collector's requests go to. A port that is left out is the scheme's
default, so `http://legacy.example` and `http://legacy.example:80` share the
limit.

- Each collector has its own limit. Two collectors that ask the same host
  are limited separately, and one collector's requests never wait for
  another's. For a limit that they share, see
  [Rate limits per host](#rate-limits-per-host).
- Every request sent counts, against the origin it is sent to: each attempt
  of a [retry](#retries), each [page](#pagination), each hop of a
  [followed redirect](#redirects-and-http2), and each request of a
  [login](AUTHENTICATION.md#logging-in-to-a-target). For the last two, the
  origin is the target's own when they stay on it.

**Waiting.** A request takes its turn from a bucket that holds `burst` turns
and refills at `requests_per_second`. With a turn available it is sent
immediately; otherwise it waits for the next turn. A request whose turn
would come at or after its deadline (the probe's or the scrape's time, or a
login's own) is not made to wait. It fails immediately, because the wait
could only end in the same failure later, and it is not retried, because a
retry would meet the same limit closer to the deadline:

```
rate limit of 2 requests per second for http://legacy.example:80 would delay the request past its deadline: it would wait 1.5s for its turn, and 800ms is left; ask the target less often, raise request.rate_limit, or the timeout
```

A wait that is cut short (the probe's client has gone, or the exporter is
shutting down) returns its turn. The requests that waited are counted in
`http_exporter_rate_limit_waits_total`, and those refused in
`http_exporter_rate_limit_refused_total`, by collector only
([Self-metrics](SELF-METRICS.md)). A
[debug probe](CONFIGURATION.md#debug-probes) shows, under each request that
waited, how long and why: `sent after waiting 500ms for its turn under the
rate limit of 2 requests per second for http://legacy.example:80`. Its own
requests take their turns like any others and are not counted.

A [static target](STATIC-TARGETS.md) of the collector may set its own
`rate_limit`, which replaces the collector's for its requests: both keys
together, with `burst` 1 when it is left out. Its requests then have their
own limit for each origin, separate from the one that the collector's other
targets share there, so a target with a lower rate is not slowed further by
them. No probe parameter sets or changes the limit.

The buckets of origins that have not been used for a while are forgotten
once they have refilled, when they hold nothing that a new bucket would not.
At most 10,000 are kept, and beyond that the least recently used is
forgotten first. A change of the limit starts it from a full bucket.

### Rate limits per host

`request.rate_limit` belongs to one collector. Several collectors that call
one API therefore share nothing, and together they may ask it faster than
its quota allows. The top-level `rate_limits` list sets limits that the
requests of every collector to a host share:

```yaml
rate_limits:
  - name: billing_api                 # required; the rule label of the self-metrics
    hosts:
      - api.billing.example           # exact, in any case
      - "*.billing.example"           # its subdomains, not billing.example itself
      - 10.20.0.15                    # an IP address
    rate_limit:
      requests_per_second: 5          # as request.rate_limit writes it
      burst: 10
```

Every `http` and `graphite` request to one of an entry's hosts takes a turn
from the entry's bucket, whichever collector, probe or static target sends
it. There is one bucket for the entry, whichever of its hosts a request goes
to. Requests are counted as for `request.rate_limit`: each attempt of a
retry, each page, each hop of a followed redirect that goes to the host, and
each request of a login.

The request takes this turn in addition to the one under its collector's own
`rate_limit`, or its static target's: first that one, then the entry's. A
request refused by one returns the turn that it took under the other,
because it is not sent. Waiting works as for `request.rate_limit`: a request
waits for its turn unless the turn would come at or after its deadline, and
then fails immediately, without a retry, with an error that names the entry:

```
rate limit of 5 requests per second of rate_limits entry "billing_api" for api.billing.example would delay the request past its deadline: it would wait 1.2s for its turn, and 800ms is left; ask the entry's hosts less often, raise the entry's rate_limit, or the timeout
```

**Hosts.** An entry's hosts are:

- host names, matched in any letter case and with or without a final dot;
- IP addresses, matched however they are written, an IPv6 address with or
  without brackets;
- wildcards `*.example.com`, which match every subdomain of `example.com` at
  any depth (`api.example.com` and `a.b.example.com`) but not `example.com`
  itself. List both to cover both.

The following rules apply:

- An IPv4 address and the same address mapped into IPv6 are one host, and Go
  dials them identically: `10.0.0.1` matches `http://[::ffff:10.0.0.1]/`,
  and `::ffff:10.0.0.1` matches `http://10.0.0.1/`.
- An entry's IPv6 address has no zone, and is refused at load with one
  (`fe80::1%eth0`), because a zone names an interface of the exporter's
  machine and not the host. A request's zone is ignored, so `fe80::1`
  matches `http://[fe80::1%25eth0]/` through every interface.
- An internationalised name is written in its `xn--` form,
  `xn--bcher-kva.example` for `bücher.example`, and matches that form in any
  letter case and with or without a final dot. A host with a character
  outside ASCII is refused at load, as in `allowed_targets`, and is not
  converted. A request whose host has one is
  [refused](#restricting-targets) before it is sent and takes no turn, so
  the `xn--` form is the only form in which a limited request names such a
  host.
- A host is written alone, without a scheme, port or path. An entry applies
  to every port and scheme of its hosts, because a quota is kept for a host
  and not for one of its ports.
- A request matches by the host of its URL, not by the address that it
  resolves to, so a host reached by two names needs both listed.
- The first entry whose hosts match a request applies, in the order of the
  list. An exact name listed before a wildcard that covers it therefore
  takes its requests out of the wildcard's entry.
- A host may be listed once, in one entry. The same host twice is refused
  when the configuration loads. So is a host that a wildcard of an earlier
  entry covers (`api.example.com` or `*.api.example.com` after an entry with
  `*.example.com`), because the earlier entry would always match it first
  and the later one would never apply. The error names both entries; list
  the narrower entry first:

```
rate_limits entry "exact" lists "api.example.com", which rate_limits entry "wild"'s "*.example.com" covers already; the first entry that matches applies, so rate_limits entry "exact" would never apply to it: list the narrower entry before the broader one
```

Within one entry, a host that the entry's own wildcard covers is accepted,
because the entry applies to it in either case. Also refused are an entry
without a `name`, two entries with one name, an entry without hosts, and a
`rate_limit` that `request.rate_limit` would refuse. `burst` is 1 when left
out.

**Replicas.** An entry's bucket is kept by each exporter process and is not
shared between processes. Several replicas, behind the
[target allocator](ALLOCATOR.md) or otherwise, each have their own bucket
for an entry. N replicas together may therefore ask its hosts up to N ×
`requests_per_second`, with N × `burst` at once. Either give each replica
the host's quota divided by the number of replicas, or have the allocator
keep the targets that ask the host on one replica:
[static targets](ALLOCATOR.md#static-targets) of one `shard_key` are placed
on one replica, and a [probe job](ALLOCATOR.md#probe-targets) with
`shard_key: address` keeps every probe of one host and port on one replica.
A probe that Prometheus sends to another replica, which any replica answers,
takes its turn there.

Only `http` and `graphite` requests are limited, as with
`request.rate_limit`. The requests of a `grpc`, `sql`, `redis` or other
collector take no turn, whatever host they go to. No probe parameter sets or
changes an entry.

The requests of a [debug probe](CONFIGURATION.md#debug-probes) take their
turns like any others and are not counted. Its report shows, under a request
that waited, the entry that it waited for. The requests that an entry made
wait are counted in
`http_exporter_host_rate_limit_waits_total{rule="billing_api"}`, and those
that it refused in `http_exporter_host_rate_limit_refused_total`, by entry,
with no collector label
([Self-metrics](SELF-METRICS.md#rate-limits-per-host)).

A reload that leaves an entry unchanged keeps its bucket, with the turns
that it has promised to waiting requests. A reload that changes it (its
hosts or its `rate_limit`) starts it from a full bucket, as a changed
`request.rate_limit` does. An entry's counts are kept while an entry of its
name is in force, and forgotten when a reload removes it.

## TLS

The collector can configure TLS verification and trust material for the
target:

```yaml
request:
  tls:
    ca_file: /etc/prometheus/tls/ca.crt
    cert_file: /etc/prometheus/tls/client.crt
    key_file: /etc/prometheus/tls/client.key
    insecure_skip_verify: false
    server_name: status.internal.example
```

`server_name` is the name that the target's certificate is checked against,
and that is sent as SNI. Use it when the target is addressed by something
that the certificate does not name, such as an IP address or a Service's
cluster name, as with a [`Host` header](#the-host-header). When it is unset,
it is the target's host. It also applies to every host that a
[followed redirect](#what-a-followed-redirect-carries) leads to, as
`ca_file` and `insecure_skip_verify` do. The client certificate is presented
to the target's origin and to the hosts in `redirect_trusted_hosts`, and a
redirect over `https` to any other host is refused.

A client certificate needs both `cert_file` and `key_file`; one without the
other stops the exporter at startup. The files themselves are read at the
first request, and again when they change, so a Secret mounted later is
picked up. [`--dry-run`](CONFIGURATION.md#dry-run) reads them and warns
about one that a request would fail on, as it does for the files of a
`login`'s own `tls` block.

Other request types take the same block:

- A [`tlscert`](TLSCERT.md) collector takes it, read by the same code,
  except for `insecure_skip_verify`: it reports a chain that does not verify
  instead of refusing it.
- A [`kubernetes`](KUBERNETES.md#outside-the-cluster) collector takes it
  only beside `api_server`, also without `insecure_skip_verify`, because it
  sends the API server a token.
- A [`redis`](REDIS.md#tls) collector takes the whole block, and uses TLS
  when it is set or the target is a `rediss://` URL.
- A [`tcp`](TCP.md#tls) collector takes the whole block too, and uses TLS
  when it is set.
- An [`ldap`](LDAP.md#tls) collector does the same, with `ldaps://`, or
  after StartTLS with `start_tls`.

A [static target](STATIC-TARGETS.md#credentials-and-tls-of-a-target) of an
`http` or `graphite` collector may name its own `tls` block, which replaces
the collector's block as a whole for every request of that target: its
pages, its redirects and its login. `tls: {}` there verifies with the system
roots only. The URL's scheme, not the block, decides whether TLS is used. No
probe parameter sets it.

For a one-off scrape, the `insecure_skip_verify` probe parameter overrides
the collector setting. Set it to `true` only for endpoints where certificate
verification is intentionally unavailable. It disables verification of the
server certificate and should not be used as a general workaround.

```yaml
params:
  insecure_skip_verify: ["true"]
```
