# Authentication

There are two credentials in play and they are deliberately separate: the one
Prometheus uses to scrape the exporter, and the one the exporter uses to reach
the target. Mixing them is the mistake this design exists to prevent, so the
exporter refuses a configuration that tries.

## Reaching the target

Everything in this section applies to `http` and `graphite` collectors, and
to `grpc` ones, which send the credential as the `authorization` metadata of
the call and forwarded headers as metadata, lower-cased; see
[gRPC](GRPC.md#authentication). An `sshfile` collector authenticates to the
host with an SSH private key or a password from a file, and verifies the
host's key against a `known_hosts` file; nothing here applies to it, since it
sends no headers and follows no redirects: see
[Files over SSH](SSHFILE.md#authentication). Nor does it apply to a `sql`
collector, which logs in to the database as `request.username` with a
password from `request.password_file`, or, for `snowflake`, a private key
from `request.private_key_file`, over the TLS its driver's option and the
`tls` block set, and forwards nothing of the probe's: see
[Databases over SQL](SQL.md#authentication).

The credential and the collector's headers are for the target: its origin,
the scheme, host and port the request is made to. When the collector
[follows redirects](REQUESTS.md#what-a-followed-redirect-carries), a redirect
that stays on that origin carries them, and one that leads anywhere else — a
subdomain, another port, another scheme — is sent neither the `Authorization`
header nor any other header of the collector's or the probe's, only `Accept`,
`Accept-Language` and `User-Agent`, and never the request body, unless its
host is listed in the collector's `request.redirect_trusted_hosts`. A TLS
client certificate is a credential of the same kind: a redirect over `https`
to a host that is neither the origin nor listed is refused, since the
connection would present it. Those three headers go wherever a redirect
leads, so a token never belongs in them.

Monitor authentication is applied by Prometheus when it scrapes the exporter. To pass that credential to the discovered target, set `request.forward_authorization: true` on the selected collector. Each `monitors` entry supports Secret-backed `auth.type: bearer` and `auth.type: basic` settings. The exporter never forwards arbitrary incoming headers.

For non-secret target headers, configure an allowlist in the collector and use the chart's monitor `headers` map. The chart encodes these as `header_<Header-Name>` probe parameters, which the exporter forwards only when the header is listed in `request.forward_headers`. Headers that describe the connection to the exporter rather than the request are never forwarded: `Host`, `Connection`, `Content-Length`, `Transfer-Encoding`, `Trailer`, `TE`, `Upgrade`, and every `Proxy-` header; nor is `Accept-Encoding`, which the exporter sets itself to decompress the answer ([Target requests](REQUESTS.md#compression-and-the-response-size)); `Authorization` has `forward_authorization` of its own. Listing one of them in `forward_headers`, in any case, or an entry that is no header name, is refused at startup, saying why, since nothing listed there would ever reach the target:

```yaml
# exporter config
collectors:
  - name: tenant_status
    request:
      type: http
      path: /status
      forward_authorization: true
      forward_headers: [X-Tenant]
    transform:
      type: jq
    metrics:
      - name: tenant_up
        expression: .up
```

```yaml
# Helm values
monitors:
  - name: application-services
    enabled: true
    type: pod
    headers:
      X-Tenant: team-a
    auth:
      enabled: true
      type: bearer
      secretName: target-api-token
      secretKey: token
```

Header values in monitor parameters are not suitable for secrets. Monitor authentication is disabled by default; set an entry's `auth.enabled: true` and use its `auth` block with a Kubernetes Secret for bearer/basic authentication. An enabled `auth` must say its `type`, `bearer` or `basic`, and its `secretName`, or the chart fails rendering, naming the monitor. Explicitly opt in per collector before forwarding the incoming Authorization header.

The exporter endpoints can also be protected with exporter-side Basic Auth,
added to the configuration beside its collectors:

```yaml
# part of the exporter config
web:
  basic_auth:
    enabled: true
    username: exporter
    password: change-me
```

The credential can be read from files instead, so it stays out of the
configuration — with the Helm chart the configuration is a ConfigMap, which is
no place for a password:

```yaml
# part of the exporter config
web:
  basic_auth:
    enabled: true
    username: exporter                # or username_file
    password_file: /var/run/prometheus-universal-exporter/web-auth/password
```

`enabled` is the switch, and it must be said: `true` requires the credential,
`false` keeps the block's settings without using them. A block that sets a
username or a password without it would protect nothing, so it is refused
when the configuration loads:

```text
line 3: web.basic_auth sets username and password but not enabled; say enabled: true to turn it on, or enabled: false to keep the settings without using them
```

Set one of `username` and `username_file`, and one of `password` and
`password_file`. A request's username and password are checked by comparing
their SHA-256 digests with those of the configured ones, both always and in
constant time, so neither how long the configured password is nor which of
the two was wrong can be told from how long the refusal takes. The username holds
no colon: a client's Basic credential ends the username at the first one, so
no client could present it, and one in `username`, or in the file
`username_file` names, is refused when the configuration loads
(`web.basic_auth.username holds a colon (":"), which ends the username of a
Basic credential (RFC 7617) ...`); a password may hold one. Leading and trailing whitespace, such as the newline an
editor adds, is removed. A file that is missing or empty when the
configuration loads is refused like any other invalid configuration. A file
changed on disk is read again by the next request, so a rotated Kubernetes
Secret takes effect without a restart; one that can no longer be read refuses
every protected request with `500` rather than letting requests in, and logs
why as a repeated failure is: one error, the requests after it at debug level
and again as an error every five minutes, and an info line when a request
reads the file again. The chart mounts the Secret with `webAuth`, and every monitor it renders
then presents that credential: the self-metrics and static targets monitors,
and each probing monitor without an `auth` of its own — see the
[chart README](../charts/prometheus-universal-exporter/README.md#exporter-authentication).

When enabled, Basic Auth is required for `/probe`, the self-metrics path, the static targets path, the landing page at `/` and the collectors page at `/collectors` (which list the collectors), and [`/-/reload`](CONFIGURATION.md#reloading-on-demand). `/health` and `/ready` remain unauthenticated for Kubernetes probes. In
[allocator mode](ALLOCATOR.md) `web.basic_auth` comes with the allocator's
bundle, so an exporter that has none in force yet — it started without one
in its cache directory and the allocator has not answered — serves none of
those endpoints: each answers `503` with `no configuration yet: waiting for
the allocator at <url>`, the self-metrics included, since serving anything
before the credential is known could serve it to anyone. One that starts
with a bundle from its cache directory asks for the bundle's credential at
once. Exporter-side Basic Auth is mutually exclusive with `request.forward_authorization`; enable one model or the other so the incoming Authorization header cannot be confused with the exporter credential.

This conflict is rejected during startup: the exporter logs `invalid startup configuration; exiting` and terminates with a non-zero exit code. Invalid configurations detected during file reload are rejected while the last valid configuration remains active.

## Logging in to a target

Some targets take no credential the operator can put in a file: they hand
out a token at a login, and want it on every request. An `http` or
`graphite` collector logs in with `request.login` before its request, and
sends the token it was given, in `Authorization` as a bearer token unless
the login says otherwise. The login is made once and its token kept for the
requests after it (see [How long a token is kept](#how-long-a-token-is-kept)).
There are two kinds, `json` and `oauth2`. A `grpc` collector does not log
in: its credential is `bearer_token_file` or basic authentication. A secret
of a login is never written in the configuration: it is read from a file at
each login, so a rotated Secret is used by the next one.

### A JSON login

`type: json` posts a JSON object to `url` and reads the token from the
answer:

```yaml
collectors:
  - name: appliance
    request:
      type: http
      path: /api/v2/status
      login:
        type: json
        url: /api/v2/login            # resolved against the target
        fields:
          tenant: acme                # as written: nothing secret
        field_files:                  # each read from its file at every login
          username: /var/run/prometheus-universal-exporter/target-auth/username
          password: /var/run/prometheus-universal-exporter/target-auth/password
        token: .access_token          # jq on the answer's body
        expires_in: .expires_in       # seconds; optional
    transform:
      type: jq
    metrics:
      - name: appliance_up
        expression: .up
```

The body posted is `{"password": "...", "tenant": "acme", "username": "..."}`,
with `Content-Type: application/json`. `field_files` is required, with one
field at least; `fields` holds what is no secret, and a field is in one of
the two. A file that is missing or empty fails the login, naming the field
and the file, never what it holds; the whitespace around what it holds is
trimmed, as for every [credential file](#credentials-from-a-kubernetes-secret).

The token is read from exactly one of three places:

| Key | The token is | Sent as |
| --- | --- | --- |
| `token` | what this jq expression gives of the answer's JSON body, such as `.access_token` or `.data.session.id`, with `$status` and `$headers` as in a rule; it must give text that is not empty | `Authorization: Bearer <token>` |
| `token_header` | the value of this header of the answer, such as `X-Subject-Token` | `Authorization: Bearer <token>` |
| `token_cookie` | the value of the cookie of this name that the answer sets, such as `JSESSIONID`, for an application that keeps a session | `Cookie: JSESSIONID=<value>`, the session sent back |

`header` and `prefix` change how a token is sent: `header: X-Auth-Token`
with `prefix: ""` sends `X-Auth-Token: <token>`. `header` is `Authorization`
when left out, and `prefix` then `"Bearer "`, and nothing in another
header. The header's name must read as a credential's — it has `auth`,
`token`, `key`, `session`, `cookie` or the like in it — since every place
that shows a request's headers, a debug report above all, withholds the
values of those alone; `X-Tenant` is refused when the configuration loads.
A `token_cookie` takes neither: the cookie is sent back as it was set.

### OAuth 2.0 client credentials

`type: oauth2` asks a token endpoint for a token with the client
credentials grant (RFC 6749 § 4.4), the way a service logs in to an API
behind Keycloak, Entra ID, Okta, Auth0 or the like:

```yaml
collectors:
  - name: billing
    request:
      type: http
      path: /v1/usage
      login:
        type: oauth2
        url: https://idp.example/realms/ops/protocol/openid-connect/token
        client_id: metrics-exporter   # or client_id_file
        client_secret_file: /var/run/secrets/billing/client-secret
        scopes: [usage:read]
        endpoint_params:
          audience: https://billing.example
        # auth_style: params          # the id and secret as form parameters
    transform:
      type: jq
    metrics:
      - name: billing_requests
        expression: .requests
```

`client_secret_file` is required, and one of `client_id` and
`client_id_file`. The id and secret are sent as a Basic credential, which
every server must take; `auth_style: params` sends them as parameters of
the form instead, for a server that wants them there. `scopes` are sent as
the `scope` parameter, and `endpoint_params` as further parameters, but not
`grant_type`, `scope`, `client_id` or `client_secret`, which the grant sets.
The answer's `access_token` is sent as `Authorization: Bearer <token>`, or
as `header` and `prefix` say, and its `expires_in` is the token's lifetime.

### Where a login goes

Where a login goes is the configuration's alone. No probe parameter sets or
changes `login`, and a [static target](STATIC-TARGETS.md) may set a `login`
of its own, which replaces the collector's for it:

- **`url` absolute**, such as an identity provider's token endpoint, is the
  same for every target the collector is asked about, whatever a probe
  says; one token serves them all.
- **`url` a path** from the root, such as `/api/v2/login`, is put on the
  target's scheme, host and port, so each target is logged in to on its
  own, and keeps its own token. The rest of the target — its path, query and
  credentials — plays no part: a probe of `https://appliance.example/a/b/`
  logs in at `https://appliance.example/api/v2/login`, as one of
  `https://appliance.example/` does, with the same token. A path relative to
  the target's own, such as `login` or `../login`, is refused at load: it
  would let the path a probe names choose where the secrets go. The secrets
  then go to whichever target a probe names, as `basic_auth_file`'s do:
  restrict the collector's targets with
  [`allowed_targets`](REQUESTS.md#restricting-targets). A target reached by
  its address with a [`Host` header](REQUESTS.md#the-host-header), the
  collector's or a static target's, is logged in to at that virtual host: a
  probe of `https://10.0.0.5/` with `Host: api.example` posts to
  `https://10.0.0.5/api/v2/login` with `Host: api.example`, as the request
  itself is sent, rather than to the server's default virtual host. An
  absolute `url` names its own host and is sent no `Host` of the target's.

The login request is held to the collector's `allowed_schemes`,
`allowed_targets` and `denied_targets` as the request is; a login URL they
refuse fails the probe in the `login` stage, and is no refusal of the
target. It follows no redirect, whatever `follow_redirects` says, so the
secrets go only where `url` says; a login answered with a redirect fails,
saying `login.url` must be the URL that answers the login itself. It is
made with `login.tls`, a [tls block](REQUESTS.md#tls) of its own for a login
server the target's settings do not fit, or else the collector's
`request.tls`, and never with the `insecure_skip_verify` a probe parameter or
a static target sets for the target. The `login` block is written whole:
`url` holds no `{{param_...}}` placeholder, no credentials and no fragment.

### How long a token is kept

A token is kept, by the collector, the login, the URL the login went to and
the `Host` it was sent with, so two virtual hosts on one address keep a token
each and neither is sent the other's, and sent on every request until shortly before it expires. Its lifetime is
`expires_in` for a `json` login, the answer's `expires_in` for `oauth2`, the
cookie's `Max-Age` or `Expires` for `token_cookie`, and, where the answer
says none, `ttl`, such as `15m`; with neither, the token is kept until the
target refuses it. A token is fetched again once a tenth of its life is
left, or 30 seconds, whichever is more, but never before half of it has
passed: a token of an hour at 54 minutes, one of five minutes at four and a
half, one of a minute at 30 seconds.

Probes that need a token while it is being fetched wait for that one login
rather than each making one, each within its own deadline. The login has a
bound of its own, 30 seconds, whatever the deadline of the probe that began
it: a probe with a short timeout that gives up, or goes away, leaves the
login to those still waiting, and is no failed login. A login that
fails while the token kept has not yet expired leaves that token in use, and
is counted as a failed login; once it has expired, the probe fails.

A target that answers `401` has the token it was sent dropped, and the
request is sent once more with the token of a new login, whatever its
method. That second request is not one of `retry.attempts`, and is made
within the probe's deadline; a `401` to it is the probe's answer, failing
in the `http_status` stage. A `403` is not taken for a token gone bad. Nor
is a `401` from where a [followed redirect](REQUESTS.md#what-a-followed-redirect-carries)
led without the token, such as another origin, which is not sent it: that
`401` is the probe's answer, and no new login is made.

At most 1000 tokens are kept, across collectors, the one used longest ago
dropped first; a reload drops the tokens of a login it changes or removes.
A secret file is read at each login, so a rotated secret is used at the
next login; the token kept is used until then.

### Beside other credentials

A login sent in `Authorization` is refused beside `basic_auth`,
`basic_auth_file`, `bearer_token` or `bearer_token_file`, which send that
header too; one sent in a header of its own may stand beside them, for a
gateway that wants a Basic credential and an application behind it that
wants a session. A `request.headers` entry of the header the login sends
its token in is refused. A probe's forwarded `Authorization`
([`forward_authorization`](#reaching-the-target)), a forwarded header, or a
static target's own credential or header, in the login's header, is sent
instead of the token, and no login is made for that request.

### When a login fails

A login that fails fails the probe in the `login` stage, as the log, the
probe's answer and a debug report name it, saying where the login went,
with its query's values masked, and why: the status and the start of the
answer, 256 bytes at most, with what reads as a credential masked, or, for
`oauth2`, the status and the error code and description RFC 6749 has the
server send. What the login sent — its body, its secrets — is never shown:

```text
login at https://appliance.example/api/v2/login failed: the login was answered with HTTP status 403: {"error": "account locked", "password": "<redacted>"}
```

An error of a `token` or `expires_in` expression says what it gave, such as
"an object", never the value, which may be the token itself.

Each collector's logins are counted in
[`http_exporter_logins_total` and `http_exporter_login_failures_total`](SELF-METRICS.md#collector-metrics).
A [debug probe](CONFIGURATION.md#debugging-collectors) lists the login
request, marked `(login)`, before the request it was made for, with its
headers shown as every request's are, a credential's value withheld, and
never its body; it shows no login when the token was kept from an earlier
one. The token is the collector's: a debug probe, a debug scrape of a
static target and a debug page's fetch use the token kept, and keep the one
their login gives for the probes after them, but their logins are not
counted in those self-metrics. [`--dry-run`](CONFIGURATION.md#dry-run) lists the login
under `request_policies`, with its header and prefix filled in, the files
its secrets are read from and never what they hold, and a field or endpoint
parameter whose name reads as a credential's without its value; a secret
file that is missing or empty is listed under `warnings`.

### Secrets from a Kubernetes Secret

With the Helm chart, `targetAuth.type: basic` mounts a Secret's username and
password as the files the JSON login above reads. Any other Secret, an
OAuth client's secret say, is mounted with `extraVolumes` and
`extraVolumeMounts` (see the
[chart README](../charts/prometheus-universal-exporter/README.md)), and its
files named in `field_files`, `client_id_file` or `client_secret_file`.

## Probing from the browser

`/collectors` lists every collector with a form that probes a target through
it and shows the answer in place. What the form asks for follows the
collector:

- **A credential in the collector's configuration** — `basic_auth`,
  `basic_auth_file`, `bearer_token`, `bearer_token_file`, a
  [`login`](#logging-in-to-a-target), a TLS client certificate — is the
  exporter's to send. The page says which kind the
  collector sends, never its value, and there is nothing to enter.
- **`forward_authorization: true`** adds a *Target credential*: a bearer
  token or a username and password. The page sends it to `/probe` as the
  `Authorization` header, which the exporter forwards to the target exactly as
  it forwards a Prometheus monitor's, in place of any credential the
  configuration holds. It is never put in the URL: the fields have no name, so
  with JavaScript off the form still probes, without the credential.
- **`forward_headers`** adds a field per header, sent as a `header_<name>`
  probe parameter as the chart sends a monitor's `headers`. These appear in
  the URL, so they are not for secrets.
- **Request parameters** — the `{{param_<name>}}` placeholders of the path,
  body, headers and query, and of the collector's label values — get a field
  each, required unless the placeholder has a default, which is shown.

Each form also has a timeout, 10 seconds to start with, sent as the
scrape timeout Prometheus would send, so a target that never answers ends
in the exporter's own error after it rather than a page waiting for ever.
A credential left blank is not sent at all.

When the exporter runs with `--web.debug` at `edit` or `full`, each collector
also has a *Debug* link, which opens the
[debug page](CONFIGURATION.md#the-debug-page) with the collector chosen and,
at `full`, the target entered in the form, to see the whole trip. Without the
flag there is no link.

The page itself is behind the exporter's Basic Auth when that is on, and the
browser sends the same credential with each probe. Since exporter Basic Auth
and `forward_authorization` cannot be combined, the `Authorization` header is
never asked to carry both.

### Debugging collectors

`--web.debug` turns on [debugging](CONFIGURATION.md#debugging-collectors) at
one of two levels, and the difference between them is what a visitor of the
exporter's pages is shown of the targets:

- **`edit`** serves `/debug`, where a collector is run on a response the
  visitor pastes. Nothing is fetched and no target is contacted, so the page
  never shows what a target answers: a visitor learns only what the
  collector makes of what they pasted. `/probe?debug=true` and
  `/static-targets?debug=<name>` stay refused (`403`).
- **`full`** adds what shows a target's answer: the page fetching a target
  through a configured collector or scraping a static target, and the two
  debug URLs. A report shows the response as the target sent it — its body,
  and every header whose name does not read as a credential's — so a visitor
  sees whatever the targets the configured collectors can reach answer,
  with those collectors' credentials sent, though never shown. Turn it on
  while a collector is being fixed, not for good.

Both are behind the exporter's Basic Auth like the other pages, and are for
whoever may already change the exporter's configuration. What a run may do
is kept from reaching further than that:

- **Another site's page.** A browser sends the exporter's Basic Auth
  credential with any request to the exporter, one a page of another site
  makes included. Each run of the page therefore carries an `X-Debug-Run`
  header and a JSON body, neither of which such a page can send without
  first asking the exporter (a CORS preflight), which the exporter never
  allows; a run without the header is answered `403`, and one the browser
  marks as coming from another site (`Sec-Fetch-Site`) too. The page cannot
  be framed. The debug URLs only read, with `GET`, and answer `text/plain`
  with `nosniff`, which a browser never runs as a page.
- **The server's files and credentials.** A collector written on the page
  that names a file — `basic_auth_file`, `bearer_token_file`, a TLS file, a
  `login` — is refused, and so is any request type but `http`, since the others read
  files, hosts and databases with the server's keys. `${NAME}` references in
  it are not expanded, so the environment stays unseen. A pasted response
  makes no request at all. A configured collector run by name is the
  operator's own: on a pasted response it makes no request either.
- **Requests to anywhere.** A target is only fetched, at `full`, through a
  collector of the configuration in force, with its target policy and its
  credentials, as a debug probe of it would be — nothing a probe could not
  already reach. Nothing of the browser's request, its `Authorization` or
  cookies, is forwarded.
- **Code.** A collector written on the page never runs Python, at either
  level. A configured collector runs its own script, as it does on every
  probe, on whatever the visitor pasted: input chosen by someone else, as a
  target's answer is on every probe. An expression of the other transforms
  runs on the server against the response, bounded by the probe's deadline,
  the collector's limits and 4 runs of the page at a time.

## Credentials from a Kubernetes Secret

To use exporter Basic Auth and a Kubernetes Secret for target credentials at the same time, disable the bridge and configure a mounted credential file. `targetAuth.enabled` is `false` by default; for basic auth:

```yaml
web:
  basic_auth:
    enabled: true
    username: exporter
    password: change-me

collectors:
  - name: protected_status
    request:
      type: http
      path: /status
      basic_auth_file:
        username: /var/run/prometheus-universal-exporter/target-auth/username
        password: /var/run/prometheus-universal-exporter/target-auth/password
    transform:
      type: jq
    metrics:
      - name: protected_up
        expression: .up
```

For the Helm chart, set `targetAuth.enabled: true`, `targetAuth.type: basic`, `targetAuth.secretName`, `usernameKey`, and `passwordKey`. The mounted Secret is read by the exporter and sent as HTTP Basic Auth to the underlying endpoint. For bearer auth, use `type: bearer`, `secretKey`, `fileName`, and `request.bearer_token_file`. The selected monitor can independently use its `auth.type: basic` to authenticate its scrape of the exporter; `request.forward_authorization` must remain `false`.

A credential file — `bearer_token_file`, or `basic_auth_file`'s username and
password — is read at every request, and the whitespace around what it holds
is trimmed, such as the line break that `echo token >file` or a Secret made
from such a file leaves at its end. A username file that holds a colon fails
the scrape and sends nothing, naming the file but not what it holds (`basic
auth username file /var/run/.../username holds a colon (":"), ...`), as a
username written in the configuration with one is refused at startup (next).

### Credentials in the configuration

A credential written in the configuration or a static target, `bearer_token`
or `basic_auth`, is used as it is written, and one holding a control
character other than a tab is refused at startup, collector and static
target alike, naming the key and the character but never the value: Go
refuses to send a bearer token with one, which would fail every scrape, and
RFC 7617 lets no Basic credential hold one. The usual one is the line break
at the end of a variable that
[`--config.expand-env`](CONFIGURATION.md#environment-variables) substitutes,
taken from a Secret made from a file that ends with one:

```text
collector "api" request.bearer_token has the control character '\n' in its value, which a header may not hold; a line break at its end is usually one the Secret or the environment variable it came from ends with: remove it, or read the credential from a file with the key's _file form, which trims it
```

A `basic_auth` username holding a colon is refused at startup too, collector
and static target alike, without the value: the Basic credential is the
username and the password joined by a colon (RFC 7617), and the target splits
it at the first one, so it would read a shorter username and a password that
starts with the rest, and refuse every scrape. A password may hold a colon.

```text
collector "api" request.basic_auth.username holds a colon (":"), which ends the username of a Basic credential (RFC 7617): the server reads the username up to it and the rest as the start of the password, so a Basic username cannot hold one
```
