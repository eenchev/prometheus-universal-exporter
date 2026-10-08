# Target requests

Everything on this page describes the request an `http` collector — one with
`request.type: http`, see [request types](CONFIGURATION.md#request-types) —
makes to the discovered target, and a `graphite` collector's too, which is the
same request with a URL built for the Graphite render API
([Graphite](GRAPHITE.md)), not the scrape Prometheus makes of the exporter. A
`grpc` collector makes a gRPC call instead, with rules of its own, described in
[gRPC](GRPC.md); its credentials, TLS keys and probe placeholders work as
they do here. Each setting
lives on a collector's `request` block, and most can be overridden for a single
scrape through a `/probe` query parameter — which is what a monitor's `params`
map renders into.

## The request URL

The URL requested is the probe's `target` with the collector's `request.path`
joined onto it and `request.query` added after the target's own query.

A target without a scheme is `http`. That is the normal case rather than an
exception: Prometheus service discovery produces `__address__` as a bare
`host:port`, and the chart's monitors pass it through as the target. So
`10.0.0.5:8080`, `legacy.example:8080` and `[fd00::5]:9000` all mean `http://`.
A target that needs HTTPS says so, `https://secure.example:8443`, or a
relabeling rule adds the scheme. A scheme stands before the path, the query
and the fragment begin, so a `://` further on is theirs:
`app.example:8080/login?next=http://other/` has none and is `http`.
`allowed_schemes` applies to the result, so a
collector that allows only `https` rejects a bare target instead of upgrading
it. A static target's scheme is checked when the file loads, so one the
collector does not allow stops the exporter instead of failing every scrape.
`allowed_schemes` itself holds `http`, `https` or both, in any case: any other
entry — `htps`, `ftp`, `https://`, one with a space around it, an empty one —
would refuse every target, and stops the exporter at startup instead.

The target's host is an IP address or a name written in ASCII: letters,
digits, `.`, `-` and `_`. An internationalised name is written in its `xn--`
form, `xn--bcher-kva.example` for `bücher.example`; a target whose host has
any other character is refused, `403`, before anything is sent
([Restricting targets](#restricting-targets) says why).

`request.path` is optional. Without it, and without a `path` probe parameter,
the target is requested exactly as given: `http://legacy.example:8080` requests
`/`, and `http://legacy.example:8080/api/status` requests `/api/status`. A target
that carries a path keeps it, and `request.path` is appended after it, the
target's escapes kept as written: `http://h/a%2Fb` with `path: /x` requests
`/a%2Fb/x`, not `/a/b/x`. A trailing slash is kept, once: `path: /` requests the
root, `/`, whether or not the target ends in `/`. Nothing
warns about a missing path — a target that serves nothing at `/` fails the probe
with its own status, or returns a page the metric rules cannot read.

A path is a path: it is joined onto the target and cleaned (`a//b` and `a/../b`
become `a/b` and `b`), and it cannot hold a query or a fragment, which would
be sent escaped as `%3F` and `%23` and ask the target for a path that does not
exist. `request.path: /status?format=json` is refused at startup, and so is a
`path` probe parameter or a static target's `request.path` holding `?` or `#`,
with a `400` or at load. Put query parameters under `request.query`:

```yaml
request:
  type: http
  path: /status
  query:
    format: json
```

**Escapes in a path.** A `%` followed by two hexadecimal digits in
`request.path`, in a `path` probe parameter or in a static target's
`request.path` is an escape you wrote, and is sent as written:
`path: /api/v4/projects/group%2Fproject` requests exactly that, the `%2F`
neither decoded into a second segment nor escaped again into `%252F`.
Everything else is escaped as a path needs — a space as `%20`, `é` as
`%C3%A9` — and a `%` that begins no escape is a percent sign, sent as `%25`:
`/100%/x` requests `/100%25/x`. To send the three characters `%2F` literally,
write `%252F`. An escaped `.` or `..` (`%2E%2E`) is sent as written too and
is not resolved as a parent directory. In the `path` probe parameter the
escape is what the exporter reads after decoding the probe's own URL, so a
monitor writes `path=/api/group%252Fproject` on the wire, which the `params`
of a Prometheus scrape configuration do for it.

**The target's own query.** A query the target carries is sent byte for byte
as it was written — a bare key stays bare (`?debug`), a pair with a `;` in it
stays (`?a=1;b=2`), and the pairs keep their order and their escapes
(`?z=a%20b&a=1`). Only what could not be sent at all is escaped, where it
stands: a space as `%20`, a double quote, `<`, `>` and characters outside
ASCII. `request.query` is appended after it, encoded, in name
order. A name both carry is sent twice, the target's first:
`http://h/x?format=xml` with `query: {format: json}` requests
`/x?format=xml&format=json`, and which of the two a server reads is the
server's choice — leave the name out of one of them. (A `graphite` collector's
own parameters are the exception: see [Graphite](GRAPHITE.md#a-collector).)

### The Host header

A `Host` under `request.headers`, or a static target's, is the host the request
is sent with, so a virtual host is reached through an IP address or an ingress:

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

For HTTPS, the certificate is still checked against the address the target
names; set [`tls.server_name`](#tls) to the virtual host too. A header value
holding a control character other than tab — a line break pasted into a
token — is refused at startup, collector and static target alike, since it
could never be sent. So is a header name that is not one, such as one with a
space, and two names that are the same header in different case, `X-Tenant`
and `x-tenant`, of which a scrape would otherwise send either.

### Compression and the response size

The exporter asks every target for gzip itself and decompresses the answer, so
`Accept-Encoding` is its own: one under `request.headers`, a collector's or a
static target's, is refused at startup, since with it set Go would hand the
decoder the compressed bytes. One the probe carries is never forwarded, even
listed in `forward_headers` — Prometheus sends `Accept-Encoding: gzip` on every
scrape.

`max_response_bytes` counts the answer as the decoder gets it, decompressed.
An answer whose `Content-Length` is over the limit is refused before its body
is read — `response size 5000 exceeds limit 100` — and one without a
`Content-Length` once reading passes the limit, `response size exceeds limit
100`, its size then unknown. A `HEAD` answer's `Content-Length` is the size of
a body it does not have, and is not held against it. There is no "no limit":
the largest value that can be written, `9223372036854775807`, is held one
byte lower and reads the whole answer.

`max_response_bytes` bounds the body. The response's status line and headers
have a bound of their own, 1 MiB, which no setting changes: a target that
sends more fails the scrape as a limit does — `the response headers are larger
than 1048576 bytes, the most the exporter reads of a response's headers` —
and is not retried, since it would send them again.

Over HTTP/2 ([`enable_http2`](#redirects-and-http2)) the bound is the same,
but the error is that one, with its single attempt, only when the headers
arrive whole before they are found too large, as headers that repeat one value
do. Headers that pass the bound while more are still arriving make Go's HTTP/2
client close the connection, and what it reports then says nothing of headers
and is what it reports for any answer that breaks the protocol. The exporter
cannot tell the two apart, so it does not call it a limit, and every request
still waiting for its answer on that connection is handed the same error,
whatever its own answer would have been. So a request that draws it is
retried once, as one of its [`retry.attempts`](#retries), on a connection made
for it alone and closed after its answer: a request that only shared the
connection gets its answer there, also when the request whose headers closed
the connection is retried at the same moment. One that
draws the error a second time is not retried again, however many attempts are
left, and its scrape fails as a failed request: `connection error:
PROTOCOL_ERROR: the HTTP/2 connection was closed over what the target sent,
as the connection before it was, so the request is not retried again; an
answer whose headers are larger than 1048576 bytes, the most the exporter
reads of a response's headers, ends this way over HTTP/2, so look at the size
of the target's response headers first`. With `retry.attempts: 0`, the
default, there is no retry, as for any failure, and the error says that the
connection was closed `in answer to this request or to another on the same
connection`: a collector that shares a target with one whose headers may be
over the bound needs `retry.attempts` of at least 1 to get past it.

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
non-ASCII characters are escaped likewise; so is a `%`, which in a value is
always a percent sign, `50%2F` sent as `50%252F`, whereas an escape written in
the path around the placeholder is [sent as written](#the-request-url). The
values `.` and `..` are rejected
with `400`, since a server resolving them would serve a different path than the
one configured. Defaults are escaped the same way, and a default of `.` or
`..` stops the exporter at startup, since no probe could use it. A `localfile`
path holds a default to what it holds a value to, so there one with `/` or
`\` in it [stops it too](LOCALFILE.md#which-file-is-read).

**Mistakes are errors, not fallbacks.** A `param_` parameter the collector
does not use, in its path or in another place that takes placeholders, is
rejected with `400`, and so is one given twice. The first is
almost always a misspelling: with `{{param_tenant:acme}}`, a probe sending
`param_tenat=globex` would otherwise succeed against the default tenant and
report `acme`'s numbers as `globex`'s.

**Scope.** Placeholders are bound in the collector's `request.path`, for
an `http` collector in its [body, header values and query
values](#in-the-body-headers-and-query), and in the collector's [fixed label
values](#in-label-values). A `path` probe parameter replaces the
path wholesale and is used exactly as given, and a `body` probe parameter
replaces the body the same way, so a `param_` parameter only they would have
used has nothing to fill and is rejected; one that also fills a label value
still has that to fill, and is accepted. `{{` always opens a placeholder in
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

Every placeholder of the collector, in its request or in a [label
value](#in-label-values), must then be filled, by `params`
or by a default, and every entry of `params` must fill one; otherwise the
exporter refuses to start, naming the target, the collector and the parameter.
The target's own `request` block — its `path`, `body` and `headers` — is
literal and cannot use placeholders, and neither can its own `labels`.

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
| gRPC `message`, `metadata` value | The body's filters in the message, which is JSON, so only `\|json`, `\|number` and `\|raw`; a metadata value as a header value. See [gRPC](GRPC.md#placeholders). |
| Graphite target | As given, and only letters, digits and `_ - . : @ % + ~`; anything else — a quote, a comma, a bracket, a glob — is refused with `400`, since Graphite expressions have no escaping. See [Graphite](GRAPHITE.md#placeholders). |

The filter follows the default, if there is one: `{{param_limit:10|number}}`.
Filters exist only in the body; a header or query value has one encoding and
always gets it, so a filter there stops the exporter at startup.

A default is held to the same rules as a value, when the configuration loads:
one its place refuses — `{{param_limit:ten|number}}`, a header default with a
line break in it, a glob as the default of a Graphite expression — stops the
exporter at startup, naming the collector, the field and the parameter. Loaded,
it would answer `400` to every probe that left the parameter out, blaming the
probe for the configuration's mistake.

**Braces of the body's own.** In a body, a header value or a query value,
`{{` opens a placeholder only when `param_` follows it, since a body may well
contain braces of its own; `{{ param_x }}` with spaces is refused at startup
rather than sent as text. A brace before a placeholder is a brace:
`{{{param_x}}}` is sent as `{`, the value and `}`, as a list of a Graphite
expression needs, `app.{{{param_host}},db}.cpu`. Header names and query
names cannot hold placeholders.

**Only there.** Placeholders are filled in the places above, in the
collector's [fixed label values](#in-label-values), and nowhere else.
A `{{param_...}}` in any other setting of a collector — `bearer_token`,
`basic_auth`, a credential or TLS file, `tls.server_name`, a `value_map`, a
label's name, a `rename_labels` target, a prometheus transform's `include`
and `exclude` — would be used as written, the token sent with the braces in
it and the value mapped to `{{param_region}}`, so it stops the exporter at
startup, naming the field:

```text
collector "api" request.bearer_token has a {{param_...}} placeholder, which is not filled in there and would be used as written; a probe's parameters fill placeholders only in the request's path, body, header and query values, a grpc message and metadata values, a graphite collector's targets, and the label values of transform.labels and of a metric rule's static label
```

To send a token the probe supplies, put it in a header value:
`headers: {Authorization: "Bearer {{param_token}}"}`.

Three kinds of field are written in a language of their own and are not
searched for placeholders, since there the same characters are that
language's: Python, `transform.script` and `transform.pre_script`, where an
f-string writes a literal brace as two (`f"{{param_x}}"`); a metric's
`items` and `expression` and a label's `expression`, where a jq string or a
regex may hold the text; and a metric's `description`, which may well say
that the path takes `{{param_tenant}}`. None of them is filled in either: a
placeholder written in an expression is evaluated as the text it is.

The values are part of the response cache key like every probe parameter, and
the verbose self-metrics never carry them: the `url` label has no query string,
and the body and headers are not labels at all.

### In label values

A placeholder is also filled in the two places a collector writes a fixed
label value: the values of
[`transform.labels`](CONFIGURATION.md#collector-wide-labels), and the `value`
of a metric rule's static label. One collector can then label its series
with a value only the scrape knows — the tenant it was asked about, the
region a monitor stands for:

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

One parameter may fill the request and the labels alike, as `param_tenant`
does here, or a label alone, in a collector whose request has no placeholder
at all. It works with every request type — `http`, `grpc`, `graphite` and
`localfile` — and under every transform: `transform.labels` are applied
after every transform, a `python` one included, and a rule's `value` is
filled wherever a rule's label has one, which is every transform but
`python`, whose rules [set no label
value](CONFIGURATION.md#long-label-values). A `python` script is not given
the parameters; what it labels its series with is its own.

**Written as in a header value.** `{{param_<name>}}` or
`{{param_<name>:<default>}}`, any number of them in one value, among text of
the value's own: `rack: "{{param_dc}}/{{param_rack:r1}}"`. `{{` opens a
placeholder only when `param_` follows it, so any other braces are text, and
are exported as written; a brace before a placeholder is a brace, so
`"{{{param_dc}}}"` filled with `ams` is `{ams}`. Two things stop the
exporter at startup, naming the
collector and the label: `{{ param_x }}` with a space after the braces, and
a filter, since a label value is written one way, as it is given:

```text
collector "tenant_status" transform.labels.tenant placeholder {{param_tenant|json}} has a filter; a label value is written one way, as it is given, so write the placeholder without a |, which a default cannot hold either
```

A label's name cannot hold a placeholder, nor can the `value` of a label that
also sets `expression`, which is refused whatever its value says. A label
with a placeholder in its `value` takes neither `required`, which is for a
label an expression reads from the response, nor a `value_map` of its own,
as no label with a `value` does: have the probe send the value wanted, or
set the `value_map` on a label of that name that another rule of the same
metric name reads with an expression, which maps the label for every rule of
that name.

**As given.** Nothing of a value is escaped when it is filled in, and nothing
is refused for the characters it holds: the exposition formats escape a
quote, a backslash and a line break where they write a label, as for any
label. The one value refused is text that is not valid UTF-8, which no label
value may be: such a probe is answered `400` naming the parameter.

**The probe's rules are the request's.** A placeholder with no value and no
default answers `400` naming the parameter and the label, before the target
is contacted:

```text
transform.labels.tenant needs param_tenant, which the probe did not supply and which has no default; add &param_tenant=<value> to the probe, or give it a default as {{param_tenant:<default>}}
```

An empty value (`param_tenant=`) counts as not supplied, so it takes the
default, or fails if there is none. An explicit empty default,
`{{param_suffix:}}`, binds nothing — and a label whose whole value comes to
nothing is left off the series, as a label with an empty value is everywhere.
For `transform.labels` that is the label left out: a series that has a label
of that name of its own keeps it, where a value that is not empty replaces
it. For a rule's label it is the label the rule does not set: the series
simply has none, and one a `prometheus` rule passes on keeps the label of
that name the target gave it.

**Used, and unused.** A parameter that fills a label is used, so it is not
refused as unused even when the request names no placeholder, and a `path`,
`body` or `message` probe parameter that replaces a templated part of the
request takes away the request's use of a parameter, never a label's. A
parameter that no place uses is still answered `400`, as is one given twice.

**The same as the text written.** Everything that reads a label's value
treats a filled value exactly as it would treat the same text written in the
configuration: `limits.max_label_value_length` — a longer value fails the
scrape in the validation, unless the label is cut:
[`truncate: true`](CONFIGURATION.md#long-label-values) on it, or on the
label of that name in another rule of the metric's name, cuts it, after any
`value_map` —
`remove_labels` and `rename_labels` and their order, a `value_map` of the
rule's name, `name_escaping`, `limits.max_labels_per_metric`, and the check
for a series made twice.

**When the configuration loads**, rules are compared as they are written
([Two rules that are the same rule](CONFIGURATION.md#two-rules-that-are-the-same-rule)):
two rules alike but for `value: "{{param_a}}"` and `value: "{{param_b}}"`
are two rules, and load. A probe that gives both parameters the same value
makes them the same series, and that scrape fails as any scrape with a
series twice does, `validation failed: duplicate metric series "status_up"`.
The one check of a value's length that can be made then is made on the value
as a probe that gives no parameter fills it — each placeholder replaced by
its default, and by nothing where it has none — since a value too long by its
own text and defaults fails every probe that leaves the parameters out:

```text
collector "tenant_status" transform.labels "tenant" is 612 bytes once its placeholders take their defaults, longer than limits.max_label_value_length 500, so every series of a probe that gives them no other value would fail validation; shorten the text or the defaults, or raise the limit
```

Where a `value_map` of the rule's name maps a rule's label, the value is
measured as the map makes it. A `"*"` entry longer than the limit is what
every value the probe gives that the map does not list becomes, so it is
refused whatever the defaults are, unless the label is cut at the scrape —
`truncate: true` on it, or on the label of that name in any rule of the
metric's name:

```text
collector "tenant_status" metric "status_up" label "tenant" value holds {{param_...}} placeholders, and the value_map of the metric's name maps every value it does not list, by its "*" entry, to 612 bytes, longer than limits.max_label_value_length 500, so every series of a probe that gives a value not listed would fail validation; shorten the "*" entry or take it out, set truncate: true on the label, or raise the limit
```

A longer entry the map lists is refused only where the defaults give its
key, since every probe that leaves the parameters out then gives it;
otherwise the configuration loads, and a probe that gives that key fails its
scrape in the validation, as a probe that gives any long value does.

**Static targets.** A [static target](STATIC-TARGETS.md)'s `params` fill a
label's placeholders exactly as they fill the request's, and the file is
held to both when it loads: every placeholder of the collector, in the
request or in a label, filled by `params` or a default, and every entry of
`params` filling one. The target's own `labels` stay literal.

**Caching.** Every probe parameter is part of the [response
cache](CONFIGURATION.md#response-caching) key, and of what makes two probes
[identical](CONFIGURATION.md#identical-probes-share-one-request), whether it
fills the request or only a label. Two probes that differ only in a label's
parameter never get each other's series: not from the cache, not as the
stale result `cache.stale_if_error` answers a failed trip with, and not by
sharing one request while both are in flight.

**Cardinality.** Each distinct value is a set of series of its own in
Prometheus, so the values should come from your own monitors' `params` — a
known list of tenants or regions — and not from whoever can reach `/probe`.
The exporter itself keeps a value only where it keeps any probe's
parameters and series, each within a bound: in the response cache, which
`limits.max_cache_entries` bounds; in the
[failure log](LOGGING.md#repeated-failures), which remembers a probe that
fails, parameters and all, until it recovers or has not failed for an hour,
10,000 failing things at most; and, with the [OTLP export](OTLP.md) on, in
the data points that wait for the next export, which
[`otlp.max_pending_points`](OTLP.md#delivery) bounds, and in the start
times of the counters, histograms and summaries exported, each kept until
its series has not been exported for an hour, twice `otlp.max_pending_points`
of them at most. Its verbose self-metrics never carry a value. A collector
whose label values hold no placeholder pays nothing for the feature; one
that has some makes a copy of its labels for each probe.

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

Whether a redirect is followed at all is decided before anything is sent to
where it leads, as for the first URL. It keeps to the collector's
`allowed_schemes`: a collector that allows only `https` refuses a redirect to
`http://` — `redirect to http://… refused: target scheme "http" is not allowed;
request.allowed_schemes allows https` — rather than read the answer in plain
text. It keeps to [`allowed_targets` and `denied_targets`](#restricting-targets)
too, and a request is stopped at its tenth redirect, which fails the probe.

### What a followed redirect carries

A target that redirects chooses where the next request goes, so what that
request carries depends on where it leads. The *origin* of a request is the
scheme, host and port of its first URL — the target with the collector's
`path` — a port left out being the scheme's own, 80 or 443. A redirect is
*trusted* when it stays on that origin, or when its host is listed in
`request.redirect_trusted_hosts`:

| The redirect leads to | Headers and credentials | Request body |
| --- | --- | --- |
| the origin the request was made to | everything the first request carried | sent again after a `307` or `308` |
| a host in `redirect_trusted_hosts`, on any port | everything the first request carried | sent again after a `307` or `308` |
| anywhere else: another host, a subdomain, another port, another scheme | only `Accept`, `Accept-Language` and `User-Agent` | never: a redirect that would send it is refused |

The commonest redirect of all, from `http://` to `https://` on one host,
leads to another origin, and gets none of the collector's credentials unless
the host is listed.

The host is compared as the URL writes it, without case, since that is the
name the request is routed by: nothing is tidied first. `api.example.com.`,
with a final dot, is another host than `api.example.com` — a resolver looks a
rooted name up apart, past the hosts file and the search domains — `127.0.0.1`
is not `localhost`, and a port written `:080` is not port 80. A redirect to a
host written with a character outside ASCII, `bücher.example` or a look-alike
of the origin's own name, is not followed at all, as a target written so is
[refused](#restricting-targets); an internationalised name is trusted, and
reached, in its `xn--` form, so list it that way and have the target redirect
to it that way.

"Everything" is the collector's `headers`, the headers
[forwarded](AUTHENTICATION.md#reaching-the-target) from the probe, the
`Authorization` its credentials make and a `Cookie`. Anywhere else gets none
of them, whatever a header is called — an API key in a header of its own is a
credential as much as `Authorization` is — and no `Host` the collector set.
The exporter never adds a `Referer`, which would tell the next host the URL
it came from; one set in `request.headers` is a header of the collector's
like any other.

`Accept`, `Accept-Language` and `User-Agent` go to every redirect
destination, with the values the first request had. Never put a token, or
anything else that identifies the collector, in those three: not in
`request.headers`, and not by forwarding a probe's header into them. The
first URL's path and the collector's `query` are not carried to another host
— a `Location` names the whole URL of the next request — but the first host
sees them, and it is the first host that chooses the `Location`.

Each redirect of a chain is judged on its own against the first URL and the
list, so a request that is sent away to a host that is not trusted and then
back to its origin carries everything again once it is back.

A `301`, `302` or `303` turns a `POST`, `PUT`, `PATCH` or `DELETE` into a
`GET` without a body, and without the `Content-Type` and the other headers
that describe one; that redirect has nothing to keep back and is followed. A
`307` or `308` has the request sent again as it was. With a body — the
collector's `body`, a `body` probe parameter, or the form a
[`graphite`](GRAPHITE.md) collector posts for long expressions — it is
followed only to a trusted destination; otherwise the probe fails:

```text
redirect to https://other.example/x refused: it would send the request body to other.example, which is not the origin the request was made to and is not listed in request.redirect_trusted_hosts
```

The collector's TLS client certificate, [`tls.cert_file` and
`tls.key_file`](#tls), is a credential too, and every TLS connection the
collector makes presents it. A redirect over `https` to a destination that is
not trusted is therefore refused before a connection is made there, and, like
the refusal of a body, not retried:

```text
redirect to https://other.example/x refused: it would present the collector's TLS client certificate to other.example, which is not the origin the request was made to and is not listed in request.redirect_trusted_hosts
```

A trusted destination is presented the certificate, and a redirect over plain
`http`, where there is nothing to present, is followed. The other `tls`
settings are no credentials and hold for every hop of a request: `ca_file`
and `insecure_skip_verify` decide how each host a redirect leads to is
verified, and a `server_name` written for the target is also the name every
`https` host of a redirect is verified against, so a redirect to another
`https` host fails verification unless that host holds a certificate for
that name.

List the hosts a target is known to redirect to, and that may be given the
collector's credentials:

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
[`allowed_targets`](#restricting-targets) writes them and without a scheme, a
port or a path; `"*"` alone trusts every host, wherever the target sends the
request. A listed host is trusted on every port and over every scheme `allowed_schemes` allows, so a
collector whose credentials must not travel in plain text allows only
`https`. Entries are compared with the host the redirect's URL names, as it
is written there and without case: nothing is looked up, so an address does
not trust a name that resolves to it, and a network cannot be listed. An
entry with a final dot matches the host written with one, and no other; an
address with a zone, `fe80::1%eth0`, matches that zone alone. An entry is
written in ASCII, as `allowed_targets` writes its own: one with a character
outside it stops the load, and no entry, `"*"` included, matches a host
written with one, which no redirect is followed to.

A star matches dots too, so a glob can trust more than it seems to:
`*example.com` matches `evilexample.com` — write `*.example.com` — and
`api.example.*` matches `api.example.evil.net`, so do not put the star last.
A glob is matched against host names, not addresses: `10.*` would trust
`10.evil.example`, so a glob made of digits, dots and wildcards alone is
refused, and addresses are listed one by one. Wildcards alone are refused
too, in any spelling but `"*"`: `**`, `*.*`.

An empty entry, a network, a port and a URL are refused when the
configuration loads — `request.redirect_trusted_hosts entry
"api.example.com:8443" has a port; a host is trusted on every port, so give
the host alone: api.example.com`. The list is the collector's alone: no probe
parameter and no static target sets or extends it, and it applies whenever
redirects are followed, also when a probe switched that on with
`follow_redirects=true`.

A credential written into the target URL itself, `https://user:password@host/`,
belongs to that URL: it follows a redirect whose `Location` is a path, which
stays on the URL's host, and no redirect that names a host, trusted or not.
A user and password that a `Location` writes into its own URL are the
target's word, not the collector's: a destination that is not trusted is
sent none of it, and a trusted one is sent them as basic auth only when the
request has no `Authorization` of its own, which otherwise is the one sent.
A `Host` in `request.headers` names a virtual host of the origin: it is kept
across a redirect to a path, and sent to no other host, listed or not.

When a host that was not sent the collector's headers answers with a status
that fails the probe, most often `401` or `403`, the error says so and names
the setting: `received HTTP status 401 from other.example, where a redirect
led: the collector's headers and credentials were not sent to that host,
which is not the origin the request was made to; list it in
request.redirect_trusted_hosts if it is to be sent them`. A credential in
the target URL counts as one that was not sent. Where the redirect led to
the target's own host under another scheme or on another port, the messages
name both origins, since the host alone would not show the difference:
`received HTTP status 401 from https://api.example.com, where a redirect
led: the collector's headers and credentials were not sent there, since
https://api.example.com is not the origin the request was made to,
http://api.example.com; list api.example.com in
request.redirect_trusted_hosts if it is to be sent them`. A
[debug probe](CONFIGURATION.md#debugging-a-probe) lists every redirect with
the headers it was sent and, under one that was not trusted, the names of the
headers that were not, a credential of the target URL as `Authorization (the
target URL's credentials)`.

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
both in their own `request` block. `redirect_trusted_hosts` is not
overridable: a probe naming it is answered as though it had not.

These replace the earlier undocumented `request.redirect_policy`. A
configuration still setting it now fails to load with an unknown-field error
rather than silently changing behaviour.

## Restricting targets

Whoever can reach `/probe` chooses the `target`, and a target that redirects
chooses where a followed redirect goes. `allowed_targets` and
`denied_targets` bound where a collector's requests may go, for `http`,
`graphite` and `grpc` collectors:

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

Every `http`, `graphite` and `grpc` collector refuses the cloud metadata
service — `169.254.169.254`, where AWS, GCP, Azure and most others answer,
and AWS's `fd00:ec2::254` — even with neither list set: it hands out the
credentials of the machine the exporter runs on, to whoever can name it as a
probe's target. A collector that must reach it lists its address, or a network
holding it, in `allowed_targets`; a name that resolves to it is not enough.

Each entry is a host name, a glob of one, an IP address or a CIDR network,
without a scheme, port or path; a name is letters, digits, dots, hyphens and
underscores, so `my_service` and `*.svc_local` can be listed. An entry is
written in ASCII, an internationalised name in its `xn--` form
(`xn--bcher-kva.example` for `bücher.example`): one with any other character
stops the load, `request.allowed_targets entry "bücher.example" has U+00FC
'ü', a character outside ASCII: write an internationalised name in its ASCII
form, as xn--bcher-kva.example for bücher.example`. A target is refused when its host is denied
by name, or any address it resolves to is in a denied network; and, when
`allowed_targets` is set, unless its host is allowed by name, or every address
it resolves to is in an allowed network. `denied_targets` wins. Names are
compared without case; `*.example.com` matches `a.b.example.com`, not
`example.com` itself. An IPv6 address with a zone, `fe80::1%eth0`, is checked
as the address without it: the zone names the interface that reaches it, so
`denied_targets: [fe80::/10]` refuses it, and the metadata service's
`fd00:ec2::254%eth0` is refused as `fd00:ec2::254` is.

Names are checked before the request and again for every redirect followed,
and so is a target written as an address, which needs no lookup, so it is
refused before anything is sent. A host is checked as it will be dialed. It is
an IP address or a name, and a name is letters, digits, `.`, `-` and `_`,
dialed as written, so it is checked as written, in lower case and without a
final dot: `my_service`,
`db--primary.internal` and `-edge.internal` are not names a registrar would
sell, but they are names Docker Compose, Kubernetes and a hosts file hand
out, and they are reached and matched like any other. The name the lists
judge is so the name the request is made to: it is what is looked up and
dialed, what the `Host` header (the `:authority` of HTTP/2) and the TLS
handshake name, and what a proxy reads in the request line or the `CONNECT`,
apart from capitals and a final dot, which are the same name. Only the
collector's own word replaces it: a `Host` header it sets or forwards, and
`tls.server_name`.

A host with a character outside ASCII is refused, with `403` and before
anything is sent or looked up, whatever the lists are and with none set:
`target bücher.example refused: it has U+00FC 'ü', a character outside ASCII:
write an internationalised name in its ASCII form, as xn--bcher-kva.example
for bücher.example`. Write an internationalised name in its `xn--` form, in
the target and in the lists alike. The HTTP client does not send a host
written outside ASCII under one name: it dials, and names in the TLS
handshake, the form the name maps to — `ｏrigin.test`, with a full-width `ｏ`,
and `origin。test`, with an ideographic full stop, are both `origin.test` —
and writes the `Host` header and the request line a proxy reads in the
unmapped one, `xn--rigin-qr33a.test` and `xn--origintest-sh3i`. A list that
allows `origin.test` would so let a request through that a proxy fetches
from, or a server of many sites answers from, a name nobody allowed, and one
that denies a name would be passed under another spelling of it; over HTTP/2
the client would not send such a request at all, and dial the target again
and again instead. A host written in full-width digits (`１２７.０.０.１`) is
refused the same way, and so is a redirect to any such host, and a `grpc`
target naming one.

A host with any other character that no name has is refused in the same way:
`target intern%61l.example refused: it has "%", a character that no host name
or address has`. No name is written with a `%`, a `,` or a `;`, and such a host
is not harmless behind a [proxy](#proxies), which may read it differently
from the lists: a URL carries a `%` in its host as `%25`, so
`http://intern%2561l.example/` names the host `intern%61l.example`, which
matches no entry for `internal.example` — and which a proxy that decodes it
once more fetches from `internal.example`. The same spelling of
`169.254.169.254` would reach the metadata service. A redirect to such a host
is not followed, and a `grpc` target naming one is refused the same way. An
IPv4 address in one of the older forms resolvers and
proxies still read — one number (`2130706433`), fewer than four parts
(`127.1`), parts in hexadecimal or octal (`0x7f.0.0.1`, `0177.0.0.1`) — is
checked as the address it is, `127.0.0.1` here, before anything is sent,
rather than as a name. A connection to a host the request did not check, when
no proxy is in between, is refused too.
Addresses are checked against the one each connection is actually made to, so
the name is looked up once, by the connection, and a name that resolves
somewhere else from one lookup to the next cannot slip through. Behind a
[proxy](#proxies) the exporter connects to the proxy, so it looks the target's
name up itself before the request and checks those addresses. A name it
cannot look up is left to the proxy to resolve, as it is for curl or any other
client behind one — an exporter with no outside DNS of its own still reaches
what its proxy reaches — and the name rules of both lists still apply. Only a
collector whose own lists hold an address or a network fails then, since its
rules could not be held to an address nobody here knows: `the request goes
through a proxy, so metrics.partner.example has to be looked up here to hold
it to the addresses and networks in request.allowed_targets and
denied_targets, and the lookup failed`. List names instead, or give the
exporter a resolver that knows the targets.

This bounds the protection behind a proxy, the default refusal of the cloud
metadata service included. The exporter checks the name, the address when the
target is written as one, and the addresses its own lookup returns; it does
not see the address the proxy resolves the name to. A name the exporter
cannot resolve, or resolves differently than the proxy does, reaches whatever
the proxy reaches for it — the metadata service of the proxy's machine, if a
name there leads to it. Where that matters, restrict the collector by name
with `allowed_targets`, and deny `169.254.169.254` at the proxy.

Whether a
request goes through a proxy is decided as the connection pool sending it
decides, from the environment as it read it. A `grpc` collector does the same:
its connections are checked as they are made, and one refused is reported as
the refusal, answered `403`, as for `http`, rather than as the `UNAVAILABLE`
gRPC makes of it. A refused request is not retried.

A refused probe is answered `403 Forbidden` — `collector web refused the
target: target 10.1.2.3 refused: its address 10.1.2.3 is in 10.0.0.0/8 in
request.denied_targets` — without the target being contacted, whatever
`error_handling.on_fetch_error` says, and is counted in
`http_exporter_targets_refused_total`. A refused static target fails in the
`target_policy` stage.

## Connections

Connections to targets are kept and reused, so an HTTPS target pays for one TLS
handshake rather than one per scrape. Every collector and scrape with the same
TLS settings — `tls.ca_file`, `cert_file`, `key_file` and
`insecure_skip_verify` — the same `enable_http2` and the same
`allowed_targets` and `denied_targets` shares one connection pool; a scrape
overriding `insecure_skip_verify` or `enable_http2` uses the pool of its own
settings. Collectors with different target lists never share a connection,
since a connection is checked against the lists once, when it is made.
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
`425`, `429`, and `5xx`). Other HTTP statuses are returned immediately. Two
failures are not retried, since the same request would fail the same way: a
target the collector's lists [refuse](#restricting-targets) and response
headers over [their bound](#compression-and-the-response-size). An HTTP/2
connection closed for a protocol error before the answer's headers were read
is retried once, on a new connection, since the error may be for another
request's answer on the connection; the second such error ends the request.
A
connection that breaks while the body is being read — reset, or closed before
the length it promised — counts as a transport failure and is retried too,
unless the probe's time is what ran out; a [debug probe](CONFIGURATION.md#debugging-a-probe)
shows such an attempt as its status followed by `then the body broke off`.

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
`attempts` is at most 10, in the configuration, a static target and the
`retry_attempts` parameter alike: whoever can reach `/probe` chooses the
parameter, and without a bound one probe of a failing target could send it
requests in a tight loop until its deadline.

When the deadline, or a shutdown, cuts short the wait before a retry, the
probe still reports what the target last answered: a `503` stays a failed
`http_status` stage with the target's body in the log and
`http_exporter_scrape_http_status_code` at 503, and the error adds that the
probe ran out of its budget. After a connection that failed, the error is that
connection error, noting that the wait before retrying was cut short.

A status the collector [accepts](#accepting-other-statuses) is its answer and
is not retried, even a `503`.

A `grpc` collector retries by gRPC status code instead, with `retry.codes`,
`[UNAVAILABLE]` by default, and has no `non_idempotent`; `retry.codes` is
refused for every other type. See [gRPC](GRPC.md#errors-and-retries).

## Accepting other statuses

A response is decoded when its status is 2xx; any other fails the scrape in
the `http_status` stage. Some targets answer with a useful body under another
status — a health endpoint that reports its checks with `503`, an API that says
`404` with a JSON body for an empty queue. `accept_status` lists the statuses
whose answers are decoded, for `http` and `graphite` collectors:

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
ones. A status written as a number is the status YAML reads it as, however it
is written: `503.0`, `5.03e2`, `0x1F7`, `+503` and `0503` are all 503, as they
are to the [schema](CONFIGURATION.md#editor-support). In quotes it is text,
the digits of a status or a class: `"503"`, and `"0503"` and `"+503"`, which
are 503 too though the schema flags them, while `"503.0"` is refused, and so are a number
with a fraction, such as `503.5`, and one outside 100 to 599, such as `6e2`,
naming the collector, or the static target, and the entry. An accepted status is not [retried](#retries). jq and yq rules read the
status as `$status` and the headers as `$headers`, an object of lower-case
header names, each with its values joined by `, `:
`$headers["retry-after"]`. A Python script has them as `response.status_code`
and `response.headers`, which keeps each header's values as a list under its
canonical name (`response.headers["Retry-After"]`); `response.header("retry-after")`
joins them with `, ` as `$headers` does. For a `grpc` collector `$status` is the call's status
code, `0` for `OK` (see [`accept_codes`](GRPC.md#errors-and-retries)); for a
`localfile` collector, which has no status, it is `null`.

## TLS

The collector can configure target TLS verification and trust material:

```yaml
request:
  tls:
    ca_file: /etc/prometheus/tls/ca.crt
    cert_file: /etc/prometheus/tls/client.crt
    key_file: /etc/prometheus/tls/client.key
    insecure_skip_verify: false
    server_name: status.internal.example
```

`server_name` is the name the target's certificate is checked against, and
sent as SNI, when the target is addressed by something the certificate does
not name — an IP address, a Service's cluster name — as with a
[`Host` header](#the-host-header). Unset, it is the target's host. It holds
for every host a [followed redirect](#what-a-followed-redirect-carries)
leads to as well, as `ca_file` and `insecure_skip_verify` do; the client
certificate is presented to the target's origin and to the hosts in
`redirect_trusted_hosts`, and a redirect over `https` to any other host is
refused.

A client certificate needs both `cert_file` and `key_file`; one without the
other stops the exporter at startup. The files themselves are read at the first
request, and again when they change, so a Secret mounted later is picked up.

For a one-off scrape, the `insecure_skip_verify` probe parameter overrides the
collector setting. Set it to `true` only for endpoints where certificate
verification is intentionally unavailable; it disables server certificate
verification and should not be used as a general workaround.

```yaml
params:
  insecure_skip_verify: ["true"]
```
