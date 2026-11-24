# Authentication

Two credentials are involved, and they are deliberately separate: the one
that Prometheus uses to scrape the exporter, and the one that the exporter
uses to reach the target. The design exists to prevent mixing them, so the
exporter refuses a configuration that tries to.

## Reaching the target

This section applies to `http` and `graphite` collectors, and to `grpc`
collectors, which send the credential as the `authorization` metadata of the
call and forwarded headers as lower-case metadata; see
[gRPC](GRPC.md#authentication). It does not apply to these types:

- An `sshfile` collector authenticates to the host with an SSH private key
  or a password from a file, and verifies the host's key against a
  `known_hosts` file. It sends no headers and follows no redirects: see
  [Files over SSH](SSHFILE.md#authentication).
- A `sql` collector logs in to the database as `request.username` with a
  password from `request.password_file` or, for `snowflake`, a private key
  from `request.private_key_file`, over the TLS that its driver's option and
  the `tls` block set. It forwards nothing from the probe: see
  [Databases over SQL](SQL.md#authentication).

The credential and the collector's headers are for the target's origin: the
scheme, host and port that the request is made to. When the collector
[follows redirects](REQUESTS.md#what-a-followed-redirect-carries):

- A redirect that stays on that origin carries them.
- A redirect to anywhere else (a subdomain, another port, another scheme) is
  sent neither the `Authorization` header nor any other header of the
  collector or the probe, only `Accept`, `Accept-Language` and `User-Agent`,
  and never the request body, unless its host is listed in the collector's
  `request.redirect_trusted_hosts`.
- A TLS client certificate is a credential of the same kind. A redirect over
  `https` to a host that is neither the origin nor listed is refused,
  because the connection would present it.
- Those three headers go wherever a redirect leads, so never put a token in
  them.

Prometheus applies the monitor's authentication when it scrapes the
exporter. To pass that credential on to the discovered target, set
`request.forward_authorization: true` on the selected collector. Each
`monitors` entry supports Secret-backed `auth.type: bearer` and
`auth.type: basic` settings. The exporter never forwards arbitrary incoming
headers.

For target headers that are not secret, configure an allowlist in the
collector and use the chart's monitor `headers` map. The chart encodes these
as `header_<Header-Name>` probe parameters, which the exporter forwards only
when the header is listed in `request.forward_headers`.

Headers that describe the connection to the exporter and not the request are
never forwarded: `Host`, `Connection`, `Content-Length`, `Transfer-Encoding`,
`Trailer`, `TE`, `Upgrade`, and every `Proxy-` header. Neither is
`Accept-Encoding`, which the exporter sets itself to decompress the answer
([Target requests](REQUESTS.md#compression-and-the-response-size)).
`Authorization` has its own setting, `forward_authorization`. Listing one of
them in `forward_headers`, in any letter case, or an entry that is not a
header name, is refused at startup with the reason, because nothing listed
there would ever reach the target:

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

Header values in monitor parameters are not suitable for secrets. Monitor
authentication is disabled by default. Set an entry's `auth.enabled: true`
and use its `auth` block with a Kubernetes Secret for bearer or basic
authentication. An enabled `auth` must set its `type` (`bearer` or `basic`)
and its `secretName`, or the chart fails to render, with an error that names
the monitor. Opt in explicitly per collector before forwarding the incoming
`Authorization` header.

The exporter's endpoints can also be protected with exporter-side Basic
Auth, added to the configuration beside its collectors:

```yaml
# part of the exporter config
web:
  basic_auth:
    enabled: true
    username: exporter
    password: change-me
```

The credential can be read from files instead, so that it stays out of the
configuration. With the Helm chart the configuration is a ConfigMap, which
is not a place for a password:

```yaml
# part of the exporter config
web:
  basic_auth:
    enabled: true
    username: exporter                # or username_file
    password_file: /var/run/prometheus-universal-exporter/web-auth/password
```

`enabled` is the switch, and it must be set explicitly: `true` requires the
credential, and `false` keeps the block's settings without using them. A
block that sets a username or a password without it would protect nothing,
so it is refused when the configuration loads:

```text
line 3: web.basic_auth sets username and password but not enabled; say enabled: true to turn it on, or enabled: false to keep the settings without using them
```

Set one of `username` and `username_file`, and one of `password` and
`password_file`.

- **Comparison.** A request's username and password are checked by comparing
  their SHA-256 digests with those of the configured values, always both and
  in constant time. The time that a refusal takes therefore reveals neither
  the length of the configured password nor which of the two was wrong.
- **Colons.** The username holds no colon. A client's Basic credential ends
  the username at the first colon, so no client could present such a
  username. A colon in `username`, or in the file that `username_file`
  names, is refused when the configuration loads
  (`web.basic_auth.username holds a colon (":"), which ends the username of a
  Basic credential (RFC 7617) ...`). A password may hold one.
- **Whitespace.** Leading and trailing whitespace, such as the newline that
  an editor adds, is removed.
- **Files.** A file that is missing or empty when the configuration loads is
  refused like any other invalid configuration. A file changed on disk is
  read again by the next request, so a rotated Kubernetes Secret takes
  effect without a restart. A file that can no longer be read refuses every
  protected request with `500` instead of letting requests in. The reason is
  logged like a repeated failure: one error, the following requests at debug
  level, an error again every five minutes, and an info line when a request
  reads the file again.
- **The chart** mounts the Secret with `webAuth`, and every monitor that it
  renders then presents that credential: the self-metrics and static targets
  monitors, and each probing monitor without its own `auth`. See the
  [chart README](../charts/prometheus-universal-exporter/README.md#exporter-authentication).

When enabled, Basic Auth is required for `/probe`, the self-metrics path,
the static targets path, the landing page at `/` and the collectors page at
`/collectors` (which list the collectors), and
[`/-/reload`](CONFIGURATION.md#reloading-on-demand). `/health` and `/ready`
remain unauthenticated for Kubernetes probes.

In [allocator mode](ALLOCATOR.md) `web.basic_auth` comes with the
allocator's bundle. An exporter that has no bundle in force yet (it started
without one in its cache directory and the allocator has not answered)
serves none of those endpoints. Each answers `503` with `no configuration
yet: waiting for the allocator at <url>`, including the self-metrics,
because serving anything before the credential is known could serve it to
anyone. An exporter that starts with a bundle from its cache directory
requires the bundle's credential immediately.

Exporter-side Basic Auth and `request.forward_authorization` are mutually
exclusive. Enable one model or the other, so that the incoming
`Authorization` header cannot be confused with the exporter's credential.

This conflict is rejected during startup: the exporter logs
`invalid startup configuration; exiting` and terminates with a non-zero exit
code. An invalid
configuration detected during a file reload is rejected, and the last valid
configuration remains active.

## Signing requests

Some APIs take neither a password nor a token. Instead, each request is
signed with a secret that both sides share, and the server computes the same
signature to verify that the request comes from the account and was not
changed in transit. An `http` collector signs its requests with
`request.signature`, an HMAC:

```yaml
collectors:
  - name: exchange_balance
    request:
      type: http
      path: /api/v3/account
      query:
        recvWindow: "5000"
        timestamp: "{{unix_ms}}"
      signature:
        secret_file: /var/run/secrets/exchange/api-secret
        payload: "{{query}}"
        query_param: signature
    transform:
      type: jq
    metrics:
      - name: exchange_balances
        expression: .balances | length
```

| Key | Default | Notes |
| --- | --- | --- |
| `secret_file` | — | **Required.** The file of the secret, read at each request. It may be a reference to a [secret store](#secret-stores). |
| `secret_encoding` | `text` | How the file writes the secret: `text`, `base64` or `hex`. |
| `algorithm` | `hmac-sha256` | `hmac-sha256`, `hmac-sha1`, `hmac-sha384` or `hmac-sha512`. |
| `payload` | — | **Required.** What is signed (see below). |
| `encoding` | `hex` | How the signature is written: `hex`, `base64` or `base64url`. |
| `header` | — | The header the signature is sent in, after `prefix`. |
| `prefix` | — | What goes before the signature in its header, such as `sha256=`. |
| `query_param` | — | The query parameter the signature is sent in. Exactly one of `header` and `query_param`. |

`payload` is text that contains parts of the request, each as it is sent:
`{{method}}`; `{{path}}`, the path as the URL writes it; `{{query}}`, the
query without its `?`; `{{body}}`; `{{host}}`; `{{url}}`; and
`{{header:Name}}`, the value of a request header, or empty when the request
has none. Anything else in it is signed as written.

- A signature in `query_param` is added after every other parameter, so
  `{{query}}` is the query before it, which is what such APIs sign. It is
  escaped like a parameter's value, including the `+`, `/` and `=` of a
  `base64` signature.
- A timestamp that an API wants signed is a
  [clock placeholder](REQUESTS.md#dates-and-times) in a header or the query,
  `"{{unix_ms}}"`, and the payload includes it through them:
  `"{{header:X-Timestamp}}{{method}}{{path}}{{body}}"`.
- Every request of a trip is signed separately, including a retry and each
  page of a [paginated](REQUESTS.md#pagination) answer. A followed redirect
  is not signed again.
- A secret that cannot be read, or that is not written as `secret_encoding`
  says, fails the scrape before anything is sent. The signature is not a
  secret, and a [debug probe](CONFIGURATION.md#debug-probes) shows it with
  the request.

### Signing requests for AWS

`request.aws_sigv4` signs each request with AWS Signature Version 4, so an
`http` collector reads an AWS API like any other. This example reads the
depth of an SQS queue:

```yaml
collectors:
  - name: sqs_depth
    request:
      type: http
      method: POST
      headers:
        Content-Type: application/x-amz-json-1.0
        X-Amz-Target: AmazonSQS.GetQueueAttributes
      body: '{"QueueUrl": {{param_queue_url|json}}, "AttributeNames": ["ApproximateNumberOfMessages"]}'
      aws_sigv4:
        service: sqs
        region: eu-west-1
    transform:
      type: jq
    metrics:
      - name: sqs_messages
        expression: .Attributes.ApproximateNumberOfMessages
```

```text
/probe?collector=sqs_depth&target=https://sqs.eu-west-1.amazonaws.com&param_queue_url=https://sqs.eu-west-1.amazonaws.com/123456789012/jobs
```

| Key | Default | Notes |
| --- | --- | --- |
| `service` | — | **Required.** The name the API signs under: `sqs`, `monitoring` (CloudWatch), `ce` (Cost Explorer), `ecs`, `lambda`, `execute-api` (API Gateway) and so on. |
| `region` | the SDK's, such as `AWS_REGION` | The region signed for. |
| `access_key_id_file`, `secret_access_key_file` | — | A static access key's files, read at each request, in place of the SDK's chain; both or neither. |
| `session_token_file` | — | A temporary access key's session token, with the other two. |

The credentials are those that the AWS SDK's default chain finds, as for an
[`s3file`](S3FILE.md) collector: a role for the service account (IRSA), EKS
Pod Identity, the instance's role or the environment. The role needs the
API's own permission, here `sqs:GetQueueAttributes`. The signing sets
`Authorization`, `X-Amz-Date` and, for temporary credentials,
`X-Amz-Security-Token`. It is therefore refused beside `basic_auth`,
`bearer_token`, a login sent in `Authorization`, those headers written in
`request.headers`, and `request.signature`.

The key exists in builds that have both the `http` and the `s3file` request
type, including the default build, because it signs with the SDK that
`s3file` links. Any other build refuses it with that reason. Which headers
and body an API requires is defined by the API: its reference gives the
`X-Amz-Target` and the JSON, or the query, of each call.

## Logging in to a target

Some targets take no credential that the operator can put in a file. They
issue a token at a login and require it on every request. An `http` or
`graphite` collector logs in with `request.login` before its request, and
sends the token that it was given, in `Authorization` as a bearer token
unless the login says otherwise.

- The login is made once and its token is kept for the following requests
  (see [How long a token is kept](#how-long-a-token-is-kept)).
- There are four kinds: `json`, `oauth2`, [`jwt`](#a-service-accounts-key)
  and [`steps`](#a-login-of-several-steps).
- A `grpc` collector does not log in: its credential is `bearer_token_file`
  or basic authentication.
- A secret of a login is never written in the configuration. It is read from
  a file at each login, so a rotated Secret is used by the next login.

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
with `Content-Type: application/json`.

- `field_files` is required, with at least one field. `fields` holds the
  fields that are not secret. Each field is in one of the two.
- A file that is missing or empty fails the login with an error that names
  the field and the file, never the content. The whitespace around the
  content is trimmed, as for every
  [credential file](#credentials-from-a-kubernetes-secret).

The token is read from exactly one of three places:

| Key | The token is | Sent as |
| --- | --- | --- |
| `token` | what this jq expression gives of the answer's JSON body, such as `.access_token` or `.data.session.id`, with `$status` and `$headers` as in a rule; it must give text that is not empty | `Authorization: Bearer <token>` |
| `token_header` | the value of this header of the answer, such as `X-Subject-Token` | `Authorization: Bearer <token>` |
| `token_cookie` | the value of the cookie of this name that the answer sets, such as `JSESSIONID`, for an application that keeps a session | `Cookie: JSESSIONID=<value>`, the session sent back |

`header` and `prefix` change how a token is sent: `header: X-Auth-Token`
with `prefix: ""` sends `X-Auth-Token: <token>`. When `header` is left out
it is `Authorization`, and `prefix` is then `"Bearer "`; with another header
the prefix is empty. The header's name must look like a credential's (it
contains `auth`, `token`, `key`, `session`, `cookie` or similar), because
every place that shows a request's headers, above all a debug report,
withholds only the values of such headers. `X-Tenant` is refused when the
configuration loads. A `token_cookie` takes neither key: the cookie is sent
back as it was set.

### OAuth 2.0 client credentials

`type: oauth2` requests a token from a token endpoint with the client
credentials grant (RFC 6749 § 4.4). This is how a service logs in to an API
behind Keycloak, Entra ID, Okta, Auth0 or similar:

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

`client_secret_file` is required, together with one of `client_id` and
`client_id_file`.

- The id and secret are sent as a Basic credential, which every server must
  accept. `auth_style: params` sends them as form parameters instead, for a
  server that requires them there.
- `scopes` are sent as the `scope` parameter, and `endpoint_params` as
  further parameters. `endpoint_params` may not set `grant_type`, `scope`,
  `client_id`, `client_secret` or `refresh_token`, which the grant sets.
- The answer's `access_token` is sent as `Authorization: Bearer <token>`, or
  as `header` and `prefix` say, and its `expires_in` is the token's
  lifetime.

### A user's refresh token

`grant_type: refresh_token` makes an `oauth2` login request a token with the
refresh token grant (RFC 6749 § 6). This is for an API that acts on a user's
own account instead of a service's. A user consents once, in a browser, and
the refresh token from that consent is put in a file, from which every login
gets a new access token. Gmail's API (for a Google Workspace or a personal
Google account) and Microsoft Graph's mail are reached in this way:

```yaml
collectors:
  - name: gmail
    request:
      type: http
      # The label's counts; INBOX unless the probe names another.
      path: /gmail/v1/users/me/labels/{{param_label:INBOX}}
      login:
        type: oauth2
        url: https://oauth2.googleapis.com/token
        grant_type: refresh_token
        client_id_file: /var/run/secrets/gmail/client-id
        client_secret_file: /var/run/secrets/gmail/client-secret
        refresh_token_file: /var/run/secrets/gmail/refresh-token
        scopes: [https://www.googleapis.com/auth/gmail.readonly]
    transform:
      type: jq
    metrics:
      - name: gmail_messages
        description: Messages in the label
        expression: .messagesTotal
        labels:
          - name: label
            expression: .name
      - name: gmail_messages_unread
        expression: .messagesUnread
        labels:
          - name: label
            expression: .name
```

```text
/probe?collector=gmail&target=https://gmail.googleapis.com&param_label=INBOX
```

`refresh_token_file` is required with `grant_type: refresh_token`, and is
refused without it; the default grant is `client_credentials`.

- The request sends `grant_type=refresh_token`, the refresh token, and
  `scopes` as `scope`, which a server that takes none ignores.
  `endpoint_params` may not set `refresh_token` either.
- `client_secret_file` is the client's secret, as for the other grant. When
  it is left out, the client is a public one, such as a desktop or mobile
  app registered without a secret, and sends its id as a form parameter.
  `auth_style: header`, a Basic credential with no secret, is therefore
  refused beside it.

**Rotated refresh tokens.** An endpoint may return a new refresh token with
the access token, to replace the one sent. Microsoft's identity platform
does so at every login, and Okta and Auth0 do with rotation turned on, and
may then refuse the old token.

- The new token is sent at the next login, and at each one after that,
  until the file holds a different token from the one it held when the
  token was replaced. That happens when the operator puts a new token in
  place.
- The token is kept for the endpoint that returned it. A `url` that is a
  path is a different endpoint for each target and each virtual host, and
  each is sent the file's token until it returns one of its own.
- The file is never written, because a Secret is mounted read-only. A
  restart, a second replica, or a reload that changes the `login` block
  therefore sends the file's token again. With an endpoint that refuses a
  refresh token once it has been used, put a new token in the file when the
  exporter restarts, or run one replica of the collector.
- Google does not rotate refresh tokens.

**Getting a refresh token for Gmail.** Create an OAuth client in a Google
Cloud project with the Gmail API enabled, and consent once, requesting
offline access to the scope.

- With Google's OAuth 2.0 Playground, set to use the client's own id and
  secret, the client is a *Web application* with
  `https://developers.google.com/oauthplayground` among its redirect URIs.
- With your own tool that runs the consent in a browser and listens on
  localhost for the answer, the client is a *Desktop app*.
- The Playground, or the tool, shows the refresh token that the consent
  produced.
- A project whose consent screen is in *Testing* gets refresh tokens that
  expire after seven days. Publish it, or keep it internal to a Workspace,
  for a token that lasts.

Mail itself is never read: `gmail.readonly` reads a label's counts, and
nothing in the collector requests a message.

### A service account's key

`type: jwt` is the login of an identity that has no password and no client
secret, only a private key, such as a Google service account or a GitHub
App. The exporter signs a JSON Web Token with the key, stating who it is and
for how long, sends it to `url`, and sends its requests with the token that
`url` returns.

A Google service account reads any Google API that its scopes cover, here a
spreadsheet:

```yaml
collectors:
  - name: sheet
    request:
      type: http
      path: /v4/spreadsheets/{{param_sheet}}/values/A1:B10
      login:
        type: jwt
        url: https://oauth2.googleapis.com/token
        signing_key_file: /var/run/secrets/google/service-account.json
        scopes: [https://www.googleapis.com/auth/spreadsheets.readonly]
    transform:
      type: jq
    metrics:
      - name: sheet_rows
        expression: .values | length
```

```text
/probe?collector=sheet&target=https://sheets.googleapis.com&param_sheet=<spreadsheet id>
```

A GitHub App requests an installation's token with the JWT as the request's
own credential:

```yaml
collectors:
  - name: github_rate
    request:
      type: http
      path: /rate_limit
      login:
        type: jwt
        url: https://api.github.com/app/installations/12345678/access_tokens
        exchange: bearer
        signing_key_file: /var/run/secrets/github/app.private-key.pem
        issuer: "123456"            # the App's id, or its client id
        ttl: 50m                    # an installation's token lasts an hour
    transform:
      type: jq
    metrics:
      - name: github_rate_remaining
        expression: .rate.remaining
```

| Key | Default | Notes |
| --- | --- | --- |
| `signing_key_file` | — | **Required.** The private key, read at every login: PEM, an RSA key (RS256), an elliptic curve key of P-256 or P-384 (ES256, ES384) or an Ed25519 key (EdDSA), not encrypted; or the JSON key file Google Cloud gives for a service account. It may be a reference to a [secret store](#secret-stores). |
| `issuer` | the key file's `client_email` | The JWT's `iss`, who it is from. Required with a PEM key. |
| `subject` | — | The JWT's `sub`, such as the user a service account acts for. |
| `audience` | `url` | The JWT's `aud`, whom it is meant for. |
| `key_id` | the key file's `private_key_id` | The `kid` of the JWT's header. |
| `scopes` | — | The JWT's `scope` claim, the scopes joined by blanks. |
| `claims` | — | Further claims, each text; not those the login sets itself. |
| `lifetime` | `5m` | How long the JWT is valid, at most `1h`. It is issued 30 seconds in the past, for a server whose clock is behind. |
| `exchange` | `form` | `form` posts the JWT as the `assertion` of a JWT bearer grant (RFC 7523), and the token is the answer's `access_token`, kept for its `expires_in`. `bearer` posts with the JWT in `Authorization: Bearer` and no body, and the token is the answer's `token`. |
| `token`, `expires_in` | by `exchange` | jq expressions of the token and of its lifetime in seconds in the answer, for an endpoint that names them otherwise. |

`lifetime` applies to the JWT, not to the token. How long the token is kept
is what the answer's `expires_in` says, or
[`ttl`](#how-long-a-token-is-kept) when the answer gives none, as a GitHub
App's does. A failure says what was wrong with the key file, or the status
that the endpoint answered, and never shows the key or the JWT. The JWT is
always exchanged at `url`: an API that takes a signed JWT itself as the
token is not supported.

### A login of several steps

Some sites cannot log in with one request. Examples: a sign-in page whose
form carries a token against cross-site requests; a form posted with the
password and that token; a ticket read from the page in the answer, traded
for a token; possibly signed with OAuth 1.0a. `type: steps` makes such a
login: a list of requests, made in order. Each reads values from its answer
that the following steps send, and one of them is the token.

```yaml
collectors:
  - name: wellness_daily
    request:
      type: http
      path: /usersummary/daily/{{date@Europe/Sofia}}
      login:
        type: steps
        values:                       # as written: nothing secret
          username: me@example.com
          consumer_key: fc3e99d2-118c-44b8-8ae3-03370dde24c0
        secret_files:                 # each read from its file at every login
          password: /var/run/secrets/wellness/password
          consumer_secret: /var/run/secrets/wellness/consumer_secret
        steps:
          - url: https://sso.example/sso/signin
            query: {service: https://sso.example/sso/embed}
            extract:
              csrf: {regex: 'name="_csrf"\s+value="([^"]+)"'}
          - url: https://sso.example/sso/signin
            query: {service: https://sso.example/sso/embed}
            headers: {User-Agent: "Mozilla/5.0"}
            form:
              username: "{{login.username}}"
              password: "{{login.password}}"
              _csrf: "{{login.csrf}}"
              embed: "true"
            extract:
              ticket: {regex: 'ticket=([^"]+)"'}
          - url: https://api.example/oauth-service/oauth/preauthorized
            query: {ticket: "{{login.ticket}}", accepts-mfa-tokens: "true"}
            oauth1:
              consumer_key: "{{login.consumer_key}}"
              consumer_secret: "{{login.consumer_secret}}"
            extract:
              oauth_token: {form: oauth_token}
              oauth_token_secret: {form: oauth_token_secret}
          - url: https://api.example/oauth-service/oauth/exchange/user/2.0
            method: POST
            oauth1: &signed
              consumer_key: "{{login.consumer_key}}"
              consumer_secret: "{{login.consumer_secret}}"
              token: "{{login.oauth_token}}"
              token_secret: "{{login.oauth_token_secret}}"
            extract:
              access_token: {json: .access_token}
              expires_in: {json: .expires_in}
        token: access_token
        expires_in: expires_in
        keep:                         # the OAuth 1.0a token lasts a year
          oauth_token: 8760h
          oauth_token_secret: 8760h
        refresh_steps:                # it makes the next token, no sign-in
          - url: https://api.example/oauth-service/oauth/exchange/user/2.0
            method: POST
            oauth1: *signed
            extract:
              access_token: {json: .access_token}
              expires_in: {json: .expires_in}
        keep_file: /var/lib/prometheus-universal-exporter/wellness-login.json
    transform:
      type: jq
    metrics:
      - name: wellness_steps
        expression: .totalSteps
```

**Steps.** Each step is one request, with at most 16 steps.

- All steps use one cookie jar, so a cookie that the sign-in page sets is
  sent with the form that follows it, and with nothing else. Each run of
  `steps`, or of `refresh_steps`, starts with an empty jar.
- A step's `url` is absolute or a path from the root, like
  [`login.url`](#where-a-login-goes), and is held to the collector's
  `allowed_schemes`, `allowed_targets` and `denied_targets`. It holds no
  placeholder. A value that the step sends goes in its `query`, whose
  parameters are added to the url's, each replacing one of the same name.
- A step is sent with `GET`, or `POST` when it sends a body, unless `method`
  says `GET`, `POST`, `PUT` or `PATCH`. `form` sends
  `application/x-www-form-urlencoded` and `json` sends a JSON object of
  text; a step uses one of the two.
- `headers` are sent as written, such as a `User-Agent` that a site
  requires. `Host`, `Content-Type`, `Content-Length`, `Transfer-Encoding`,
  `Connection`, `Cookie` and `Accept-Encoding` are set by the step itself;
  with `Accept-Encoding` the answer, which is read for its values, would
  stay compressed.
- A step succeeds on a `2xx` answer, or on one in its `status` list. A login
  follows no redirect, so a `302` whose `Location` holds what is needed is
  listed, `status: [302]`, and read with a header. A step whose `status`
  lists other statuses and that is answered with a redirect it does not list
  fails with an error that says so.

**Values.** What a step sends (its `query`, `headers`, `form`, `json` and
`oauth1` values) may hold `{{login.<name>}}`. That is one of `values`, as
written; one of `secret_files`, read from its file at each login; or one
that an earlier step read. A name used before it is defined is refused when
the configuration loads, with an error that names the step and the key. A
step reads values with `extract`, each from exactly one place:

| Key | The value is |
| --- | --- |
| `regex` | the first group of this regular expression's first match on the body, or the whole match without a group |
| `json` | what this jq expression gives of the body read as JSON, text or a number |
| `form` | this field of the body read as a form, as an OAuth 1.0a token endpoint answers: `oauth_token=...&oauth_token_secret=...` |
| `header` | this header of the answer |
| `cookie` | the cookie of this name the answer sets; its `Max-Age` or `Expires` is the value's expiry, and one the answer deletes, with `Max-Age` below 0 or an `Expires` past, is not read |

A value that is not found, or is empty, fails the login with an error that
names the step and the value, never the answer, which may hold the
password. `token` names the value that is the token, which a step reads. It
is sent like a `json` login's token: in `Authorization` as a bearer token
unless `header` and `prefix` say otherwise. A session cookie that a step
read with `cookie` is sent back with `header: Cookie` and
`prefix: "SESSION="`, its name and an equals sign.

#### Signing a step with OAuth 1.0a

`oauth1` signs a step with OAuth 1.0a's HMAC-SHA1 (RFC 5849), in its
`Authorization` header. It uses the consumer's key and secret and, once a
step has read them, the token and its secret; each is written literally or
is a value. The signature covers the method, the URL, the query and a `form`
body, with a new nonce and the current time. No step may set `Authorization`
beside it.

#### Refreshing with values kept

A login whose full run is costly, such as a sign-in that a site limits to a
few per day or watches for robots, keeps some of its values to make later
tokens with.

- `keep` names those values, each with how long it is used.
  `refresh_steps` are the requests that send them, as `{{login.<name>}}`,
  and read a new token.
- When the token is due, `refresh_steps` are made while every kept value is
  still valid. When one has expired, or the refresh fails, the full `steps`
  are made, with a warning in the log when a refresh failed.
- A kept value expires after its `keep` duration, or earlier at its
  cookie's or its JWT's expiry. `0` uses it for as long as `refresh_steps`
  succeed with it.
- `refresh_steps` read `token` again, and `expires_in` or `expires_at` when
  the login sets one. They may read a kept value again, which then replaces
  it.
- The two go together: `keep` without `refresh_steps`, or the reverse, is
  refused.

#### Keeping the token across restarts

`keep_file` writes the token, its expiry and the kept values to a file after
each login, and a login reads the file first. An exporter that starts again
sends the token that the file holds while it is valid, and makes the next
one with `refresh_steps`, so a restart causes no sign-in.

- The file is written whole, next to itself and renamed over, and is
  readable only by the exporter's user. It holds secrets, so it belongs on
  its own volume, never in the image.
- Its directory must exist and be writable. A file that cannot be written
  leaves the login done and logs a warning.
  [`--dry-run`](CONFIGURATION.md#dry-run) warns about a directory that does
  not exist.
- Several logins may share one file, each under a digest of the collector,
  the login and its destination, which change with the login: a login that
  is changed in any way starts anew.
- A login of which nothing would be used (its token and every kept value
  have expired) is removed from the file when another login is written to
  it.
- Exporters that share the file on a shared volume take each other's
  tokens. A login reads the file again whenever its token is due or refused,
  and takes a token there that differs from the one it last had.

### Where a login goes

Where a login goes is set only by the configuration. No probe parameter sets
or changes `login`. A [static target](STATIC-TARGETS.md) may set its own
`login`, which replaces the collector's for it.

- **An absolute `url`**, such as an identity provider's token endpoint, is
  the same for every target that the collector is asked about, whatever a
  probe says. One token serves all of them.
- **A `url` that is a path** from the root, such as `/api/v2/login`, is put
  on the target's scheme, host and port, so each target is logged in to
  separately and keeps its own token.
  - The rest of the target (its path, query and credentials) plays no part:
    a probe of `https://appliance.example/a/b/` logs in at
    `https://appliance.example/api/v2/login`, as one of
    `https://appliance.example/` does, with the same token.
  - A path relative to the target's own, such as `login` or `../login`, is
    refused at load: it would let the path that a probe names choose where
    the secrets go.
  - The secrets then go to whichever target a probe names, as those of
    `basic_auth_file` do. Restrict the collector's targets with
    [`allowed_targets`](REQUESTS.md#restricting-targets).
  - A target reached by its address with a
    [`Host` header](REQUESTS.md#the-host-header), the collector's or a
    static target's, is logged in to at that virtual host. A probe of
    `https://10.0.0.5/` with `Host: api.example` posts to
    `https://10.0.0.5/api/v2/login` with `Host: api.example`, as the request
    itself is sent, not to the server's default virtual host. An absolute
    `url` names its own host and is sent no `Host` of the target.
- **A `steps` login** goes where each step's `url` says. When a step's `url`
  is a path, it goes to the target, with its `Host`, and the login then
  keeps a token for each target, even when its first step goes to an
  identity provider that every target shares.

The login request is held to the collector's `allowed_schemes`,
`allowed_targets` and `denied_targets`, like the request. A login URL that
they refuse fails the probe in the `login` stage, and is not a refusal of
the target.

- It follows no redirect, whatever `follow_redirects` says, so the secrets
  go only where `url` says. A login answered with a redirect fails, with an
  error saying that `login.url` must be the URL that answers the login
  itself.
- It is made with `login.tls`, its own [tls block](REQUESTS.md#tls) for a
  login server that the target's settings do not fit. Otherwise it uses the
  request's `tls`: a
  [static target's own block](STATIC-TARGETS.md#credentials-and-tls-of-a-target)
  when it names one, or else the collector's `request.tls`. It never uses
  the `insecure_skip_verify` that a probe parameter or a static target sets
  for the target.
- The `login` block is written literally: `url` holds no `{{param_...}}`
  placeholder, no credentials and no fragment.

### How long a token is kept

A token is kept per collector, login, `tls` block that the login was made
with, URL that the login went to, and `Host` that it was sent with. Two
virtual hosts on one address therefore each keep a token, and neither is
sent the other's. The same holds for two static targets that log in with
their own client certificates or CAs. The token is sent on every request
until shortly before it expires.

**Lifetime.** The token's lifetime is:

- for a `json` login, `expires_in`;
- for `oauth2`, the answer's `expires_in`;
- for `token_cookie`, the cookie's `Max-Age` or `Expires`;
- for a `steps` login, the value that `expires_in` names, in seconds, or
  `expires_at`, a time (seconds or milliseconds since 1970, RFC 3339 or an
  HTTP date), or else the expiry of the token's cookie when a step read it as
  a cookie. An `expires_at` before the login began, as happens when a
  lifetime in seconds is read as a time, fails the login.

Where the login gives no lifetime:

- a token that is a JSON Web Token lasts until its `exp` claim, read without
  checking its signature (the target checks that), when it is after the
  login began. A claim already in the past, from a clock behind the
  issuer's, is ignored;
- otherwise `ttl` applies, such as `15m`;
- with neither, the token is kept until the target refuses it.

A token is fetched again once a tenth of its lifetime is left, or 30
seconds, whichever is more, but never before half of it has passed: a token
of an hour at 54 minutes, one of five minutes at four and a half, and one of
a minute at 30 seconds.

**Concurrent probes.** Probes that need a token while it is being fetched
wait for that one login instead of each making one, each within its own
deadline. The login has its own bound, 30 seconds, whatever the deadline of
the probe that started it. A probe with a short timeout that gives up, or
goes away, leaves the login to the probes still waiting, and that is not a
failed login. A login that fails while the kept token has not yet expired
leaves that token in use, and is counted as a failed login. Once the token
has expired, the probe fails.

**Refused tokens.** A target that answers `401` has the token it was sent
dropped, and the request is sent once more with the token of a new login,
whatever its method. That second request does not count as one of
`retry.attempts`, and is made within the probe's deadline. A `401` to it is
the probe's answer, failing in the `http_status` stage.

- A `403` is not taken to mean that the token is invalid, unless
  `relogin_on` says so. It lists the `4xx` statuses that refuse the token,
  in place of `401`, such as `relogin_on: [401, 403]` for a site that
  answers an expired session with `403`. Every kind of login accepts it.
- A `401` from the destination of a
  [followed redirect](REQUESTS.md#what-a-followed-redirect-carries) that was
  not sent the token, such as another origin, does not count either. That
  `401` is the probe's answer, and no new login is made.

At most 1000 tokens are kept, across collectors; the one used longest ago is
dropped first. A reload drops the tokens of a login that it changes or
removes, or whose `tls` block it changes. A secret file is read at each
login, so a rotated secret is used at the next login, and the kept token is
used until then.

### Beside other credentials

A login sent in `Authorization` is refused beside `basic_auth`,
`basic_auth_file`, `bearer_token` or `bearer_token_file`, which send that
header too. A login sent in its own header may be combined with them, for a
gateway that requires a Basic credential and an application behind it that
requires a session.

- A `request.headers` entry for the header that the login sends its token
  in is refused.
- A probe's forwarded `Authorization`
  ([`forward_authorization`](#reaching-the-target)), a forwarded header, or
  a static target's own credential or header, in the login's header, is sent
  instead of the token, and no login is made for that request.

### When a login fails

A failed login fails the probe in the `login` stage, as the log, the probe's
answer and a debug report name it. The error says where the login went,
with its query values masked, and why:

- the status and the start of the answer, at most 256 bytes, with whatever
  looks like a credential masked, and with the secrets that the login read
  from its files and the values that its steps read masked wherever the
  answer repeats them, as a sign-in form answered with its fields filled in
  does;
- or, for `oauth2`, the status, and the error code and description that RFC
  6749 has the server send.

What the login sent (its body, its secrets) is never shown:

```text
login at https://appliance.example/api/v2/login failed: the login was answered with HTTP status 403: {"error": "account locked", "password": "<redacted>"}
```

What the server answered belongs to the operator: an identity provider's
refusal may name the user, the client or the directory that it asked. It is
in the exporter's log and, with
[`--web.debug=full`](CONFIGURATION.md#debugging-collectors), in a debug
probe's report and on the [status page](ENDPOINTS.md#status-page). The
caller of the probe gets only the status and where to find the rest:

```text
login at https://appliance.example/api/v2/login failed: the login was answered with HTTP status 403; what it said is in the exporter's log
```

The caller of an `oauth2` login also gets the error code when it is one that
RFC 6749 § 5.2 registers (`invalid_request`, `invalid_client`,
`invalid_grant`, `unauthorized_client`, `unsupported_grant_type` or
`invalid_scope`), as in `the token endpoint answered HTTP status 401,
invalid_client; what it said is in the exporter's log`.

- Any other code is the server's own text, and the caller gets `the token
  endpoint answered HTTP status 401 with an error code RFC 6749 does not
  register` instead.
- An answer that the OAuth library could not read as a token, whose
  complaint quotes it, is reported as `the token endpoint's answer could not
  be read as a token`.
- A failed connection, or an answer without `access_token`, is reported as
  it is logged.

A failed login is made again by the next probe or scrape that needs the
token. A wrong password is therefore sent at every scrape, and a target that
refuses every token with `401` is logged in to again at every scrape. Where
the site locks an account after a few failed sign-ins, or limits them, give
the collector
[`failure_backoff`](CONFIGURATION.md#backing-off-from-failing-targets),
which counts a failed login as a failed scrape and keeps the next attempts
away from the target, and from the login, for a while.

An error of a `token` or `expires_in` expression says what type it gave,
such as "an object", never the value, which may be the token itself. A
`steps` login's failure names the step, as `login.steps[1]: the login was
answered with HTTP status 403` or
`login.refresh_steps[0].extract.access_token matched nothing in the answer`,
and never shows a value that it read.

**Observing logins.**

- Each collector's logins are counted in
  [`http_exporter_logins_total` and `http_exporter_login_failures_total`](SELF-METRICS.md#collector-metrics),
  and those that a `steps` login's `refresh_steps` made in
  `http_exporter_login_refreshes_total`. A token read from a `keep_file` is
  not a login and is counted in none.
- With [OTLP traces](OTLP.md#traces-of-slow-requests) on, each request of a
  login, including each step of a `steps` login, is a span of the trace of
  the probe or scrape that needed the token. A failed login, logged with the
  stage `login`, is exported with [OTLP logs](OTLP.md#the-exporters-own-log)
  like every log line.
- A [debug probe](CONFIGURATION.md#debugging-collectors) lists each login
  request, marked `(login)`, before the request that it was made for. Its
  headers are shown like every request's, with a credential's value
  withheld, and its body is never shown. No login is shown when the token
  was kept from an earlier one.
- The token belongs to the collector. A debug probe, a debug scrape of a
  static target and a fetch on the debug page use the kept token, and keep
  the one that their login gives for the following probes, but their logins
  are not counted in those self-metrics.
- [`--dry-run`](CONFIGURATION.md#dry-run) lists the login under
  `request_policies`, with its header and prefix filled in, the files that
  its secrets are read from (never their content), and a field or endpoint
  parameter whose name looks like a credential's without its value. A
  `steps` login's `values`, and what each step sends, are shown in the same
  way, and an OAuth 1.0a secret written in a step without its value. A
  secret file that is missing or empty is listed under `warnings`, and so is
  a `keep_file` whose directory does not exist.

### Logins and the target allocator

Each exporter keeps its own tokens, in memory, and logs in when a scrape
first needs one.

- A replica of the [target allocator](ALLOCATOR.md) that scrapes no target
  of a collector, and is sent no probe of it, makes no login.
- A static target that the allocator moves to another replica is logged in
  to there at its first scrape. The replica that it left keeps the token
  unused until the token expires or a reload drops it.
- A `steps` login with `keep_file` on a volume private to each replica
  starts there with its full `steps`. On a volume that the replicas share
  (`ReadWriteMany`), the new replica takes the token and the kept values
  that the other replica wrote, and makes no sign-in.
- Two replicas that write the file at the same time, as while a target
  moves, may each replace the other's line for another login, which then
  logs in again. The file itself is never left half written.

### Secrets from a Kubernetes Secret

With the Helm chart, `targetAuth.type: basic` mounts a Secret's username and
password as the files that the JSON login above reads. Any other Secret,
such as an OAuth client's secret, is mounted with `extraVolumes` and
`extraVolumeMounts` (see the
[chart README](../charts/prometheus-universal-exporter/README.md)), and its
files are named in `field_files`, `client_id_file`, `client_secret_file`,
`refresh_token_file` or `secret_files`. A `keep_file` goes on its own
writable volume, also with `extraVolumes` and `extraVolumeMounts`: an
`emptyDir` to survive restarts of the pod's container, or a
PersistentVolumeClaim to survive restarts of the pod.

## Probing from the browser

`/collectors` lists every collector with a form that probes a target through
it and shows the answer on the page. What the form asks for depends on the
collector:

- **The target** is labelled and hinted as the collector's request type
  names it: *URL* and `https://host:port` for `http`, *SSH host* and
  `host or host:22` for `sshfile`, *Name to resolve* and `example.com` for
  `dns`, as the [debug page](CONFIGURATION.md#the-debug-page) lists them. It
  is marked optional where the type may leave it out, and offered disabled,
  `none: leave it out`, for a `kubernetes` collector, which takes no target.
- **A credential in the collector's configuration** (`basic_auth`,
  `basic_auth_file`, `bearer_token`, `bearer_token_file`, a
  [`login`](#logging-in-to-a-target), a TLS client certificate) is sent by
  the exporter. The page says which kind the collector sends, never its
  value, and there is nothing to enter.
- **`forward_authorization: true`** adds a *Target credential*: a bearer
  token or a username and password. The page sends it to `/probe` as the
  `Authorization` header, which the exporter forwards to the target exactly
  as it forwards a Prometheus monitor's, in place of any credential in the
  configuration. It is never put in the URL: the fields have no name, so
  with JavaScript off the form still probes, without the credential.
- **`forward_headers`** adds a field per header, sent as a `header_<name>`
  probe parameter, as the chart sends a monitor's `headers`. These appear in
  the URL, so they are not for secrets.
- **Request parameters**, the `{{param_<name>}}` placeholders of the path,
  body, headers and query and of the collector's label values, get a field
  each. A field is required unless the placeholder has a default, which is
  shown.

Each form also has a timeout, 10 seconds initially, sent as the scrape
timeout that Prometheus would send. A target that never answers therefore
ends in the exporter's own error after the timeout, instead of a page that
waits forever. A credential left blank is not sent at all.

When the exporter runs with `--web.debug` at `edit` or `full`, each
collector also has a *Debug* link. It opens the
[debug page](CONFIGURATION.md#the-debug-page) with the collector chosen and,
at `full`, the target entered in the form, to show the whole trip. Without
the flag there is no link.

The page itself is behind the exporter's Basic Auth when that is on, and the
browser sends the same credential with each probe. Because exporter Basic
Auth and `forward_authorization` cannot be combined, the `Authorization`
header is never asked to carry both.

### Debugging collectors

`--web.debug` turns on [debugging](CONFIGURATION.md#debugging-collectors) at
one of two levels. The difference between them is what a visitor of the
exporter's pages is shown of the targets:

- **`edit`** serves `/debug`, where a collector is run on a response that
  the visitor pastes. Nothing is fetched and no target is contacted, so the
  page never shows what a target answers: a visitor learns only what the
  collector makes of what they pasted. `/probe?debug=true` and
  `/static-targets?debug=<name>` stay refused (`403`).
- **`full`** adds what shows a target's answer: the page fetching a target
  through a configured collector or scraping a static target, and the two
  debug URLs. A report shows the response as the target sent it: its body,
  and every header whose name does not look like a credential's. A visitor
  therefore sees whatever the targets that the configured collectors can
  reach answer, with those collectors' credentials sent but never shown.
  Turn it on while a collector is being fixed, not permanently.

Both are behind the exporter's Basic Auth like the other pages, and are for
people who may already change the exporter's configuration. What a run can
do is prevented from reaching further than that:

- **Another site's page.** A browser sends the exporter's Basic Auth
  credential with any request to the exporter, including one that a page of
  another site makes.
  - Each run of the page therefore carries an `X-Debug-Run` header and a
    JSON body. Such a page cannot send either without first asking the
    exporter (a CORS preflight), which the exporter never allows. A run
    without the header is answered `403`, and so is one that the browser
    marks as coming from another site (`Sec-Fetch-Site`), as every endpoint
    refuses it ([Requests from other sites](#requests-from-other-sites)).
  - The page fills in the collector and the target that its address names,
    which a link on any page can set, and runs nothing until *Run* is
    pressed, so following such a link fetches nothing.
  - The page cannot be framed. The debug URLs only read, with `GET`, and
    answer `text/plain` with `nosniff`, which a browser never runs as a
    page.
- **The server's files and credentials.** The playground, a collector
  written on the page, may be of any request type, because its request
  block is inert.
  - No run makes a request with it, whatever its type: a pasted response is
    decoded as it is, and a target is reached only through a configured
    collector. A `sql` or `sshfile` playground therefore reaches no database
    or host, and a `localfile` one reads no file. Its type only says how the
    answer is read.
  - A playground that names a file (any request key that ends in `_file` or
    `_files`, such as `bearer_token_file`, a TLS file or `known_hosts_file`,
    and a `login`) is still refused, so that nothing a file holds can reach
    an answer even through a mistake in that rule. Python is refused too
    (below).
  - `${NAME}` references in it are not expanded, so the environment stays
    hidden.
  - A pasted response makes no request at all. A configured collector run by
    name belongs to the operator; on a pasted response it makes no request
    either.
  - The page shows such a collector's YAML, at `edit` as at `full`, with
    every value whose key looks like a credential's, every path that a
    `*_file` key names and a URL's userinfo withheld, and with whatever looks
    like a credential in a Python script masked
    ([The debug page](CONFIGURATION.md#the-debug-page)). It shows how the
    collector is configured, not what it is configured with.
- **Requests to anywhere.** A target is fetched, at `full` only, through a
  collector of the configuration in force (the one chosen, or the one the
  playground was copied from), with its target policy and its credentials,
  as a debug probe of it would be. That reaches nothing that a probe could
  not already reach. Nothing of the browser's request, its `Authorization`
  or cookies, is forwarded.
- **The OTLP endpoint.** The debug page's endpoint test, at `full` only,
  sends one empty export to the endpoint that `otlp` configures, with its
  headers and TLS, and to no other. The visitor cannot name a destination,
  so the test reaches nothing that the export does not reach at every
  interval.
  - At `full` it shows what the endpoint answered (its status, headers and
    the start of its body) and the full error of a failed connection or TLS
    handshake, which can name the endpoint's host, address and certificate.
  - The headers it sent are shown with every `otlp.headers` value withheld,
    whatever the header is called, because an endpoint's API key may have
    any name. The endpoint's URL is shown with its userinfo withheld.
  - One test runs at a time.
  - The *OTLP* tab shows what a run would export, at `edit` too, built from
    what the run already shows.
- **Code.** The playground never runs Python, at either level. A configured
  collector runs its own script, as it does on every probe, on whatever the
  visitor pasted: input chosen by someone else, like a target's answer on
  every probe. An expression of the other transforms runs on the server
  against the response, bounded by the probe's deadline, the collector's
  limits and 4 runs of the page at a time.

`--web.debug.dangerous` lifts the two rules above that restrict the
playground to the response (no file of the server's, and no Python), and
lets it make its own request
([Dangerous debug mode](CONFIGURATION.md#dangerous-debug-mode)). With it,
whoever can open the page runs code on the server:

- A script runs in the workers' [sandbox](PYTHON.md), which prevents a
  script from opening files or sockets by mistake but does not stop one
  written to escape.
- A `localfile` playground reads any file under a root that it chooses, and
  its own request reaches any host that the exporter can reach, with
  whatever credential file it names.

Basic Auth and the checks against other sites' pages still apply, but the
page is then as powerful as a shell in the exporter's container. Use it on
your own machine or in a locked-down debugging session, and never on a
shared deployment. The exporter warns at startup, and every page's bar says
that the mode is on. `${NAME}` references stay unexpanded even then, because
the environment is where a deployment keeps its secrets.

## Requests from other sites

Once the exporter's Basic Auth credential has been typed into a browser, the
browser sends it with every request to the exporter, including those that a
page of another site makes. A link, an image, a redirect or a form posted
from that page is enough; no script is needed. Such a page could make a
logged-in browser:

- probe a target of its choice through a collector that holds a credential,
  which the exporter would send there unless the collector's
  [target policy](REQUESTS.md#restricting-targets) restricts it to its own
  targets;
- post a form to `/-/reload`;
- open the debug page on a target of its choice.

Without Basic Auth, the same page could still reach an exporter on the
visitor's own network, at `localhost` or on an intranet, which the page's
own site cannot reach.

Every endpoint except `/health` and `/ready` therefore refuses, with `403`
and before the credential is checked, a request that the browser marks as
made by another site's page: `Sec-Fetch-Site: cross-site`, or `same-site`,
which another port or subdomain of the same site gets and which is no more
the exporter's than another site is.

- The exporter's own pages are `same-origin`, and an address typed in or a
  bookmark is `none`. Both pass.
- Prometheus, curl and every other client that is not a browser send no
  `Sec-Fetch-Site` and pass too, so scrapes are not affected.
- A page cannot set or remove the header itself.
- One consequence: a link to the exporter on another page, such as a
  dashboard or the Prometheus targets page on another port, is refused when
  followed. Open the address directly instead.

A browser too old to send `Sec-Fetch-Site` (every current browser sends it)
is not refused. Its `Origin` is not compared with the request's own host,
because a reverse proxy in front of the exporter may rewrite the `Host`, and
the comparison would then refuse the exporter's own pages.

## Credentials from a Kubernetes Secret

To use exporter Basic Auth and a Kubernetes Secret for target credentials at
the same time, disable the bridge and configure a mounted credential file.
`targetAuth.enabled` is `false` by default. For basic auth:

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

For the Helm chart, set `targetAuth.enabled: true`, `targetAuth.type: basic`,
`targetAuth.secretName`, `usernameKey`, and `passwordKey`. The exporter reads
the mounted Secret and sends it as HTTP Basic Auth to the underlying
endpoint. For bearer auth, use `type: bearer`, `secretKey`, `fileName`, and
`request.bearer_token_file`. The selected monitor can independently use its
`auth.type: basic` to authenticate its scrape of the exporter;
`request.forward_authorization` must remain `false`.

A credential file (`bearer_token_file`, or the username and password of
`basic_auth_file`) is read at every request. The whitespace around its
content is trimmed, such as the line break that `echo token >file`, or a
Secret made from such a file, leaves at its end. A username file that holds
a colon fails the scrape and sends nothing, with an error that names the
file but not its content (`basic auth username file /var/run/.../username
holds a colon (":"), ...`), just as a username with a colon written in the
configuration is refused at startup (see the next section).

### Credentials in the configuration

A credential written in the configuration or a static target,
`bearer_token` or `basic_auth`, is used as written. One that holds a control
character other than a tab is refused at startup, for a collector and a
static target alike, with an error that names the key and the character but
never the value. Go refuses to send a bearer token with such a character,
which would fail every scrape, and RFC 7617 allows no Basic credential to
hold one. The usual case is the line break at the end of a variable that
[`--config.expand-env`](CONFIGURATION.md#environment-variables) substitutes,
taken from a Secret made from a file that ends with one:

```text
collector "api" request.bearer_token has the control character '\n' in its value, which a header may not hold; a line break at its end is usually one the Secret or the environment variable it came from ends with: remove it, or read the credential from a file with the key's _file form, which trims it
```

A `basic_auth` username that holds a colon is refused at startup too, for a
collector and a static target alike, without the value. The Basic credential
is the username and the password joined by a colon (RFC 7617), and the
target splits it at the first colon. It would therefore read a shorter
username and a password that starts with the rest, and refuse every scrape.
A password may hold a colon.

```text
collector "api" request.basic_auth.username holds a colon (":"), which ends the username of a Basic credential (RFC 7617): the server reads the username up to it and the rest as the start of the password, so a Basic username cannot hold one
```

## Secret stores

Where a secret may not be written to disk, a credential can be read from a
secret store instead of a file: HashiCorp Vault, Google Cloud Secret Manager
or AWS Secrets Manager. The stores are defined once, in `secret_stores`, and
any key that names a credential file accepts a reference to a store instead
of a path:

```yaml
secret_stores:
  - name: vault-prod
    type: vault
    address: https://vault.example.com:8200
    auth: {method: kubernetes, role: exporter}

collectors:
  - name: orders_db
    request:
      type: sql
      driver: postgres
      database: orders
      username: monitor
      password_file: secret://vault-prod/secret/data/orders-db#password
      statement: SELECT count(*) AS orders FROM orders
    transform:
      type: csv
    metrics:
      - name: orders_total
        expression: orders
```

A reference is `secret://<store>/<path>#<field>`: the store's `name`, the
secret's path in the store, and the field to read from a secret that holds
several. `?version=` before the `#` reads a version other than the current
one.

- It works wherever a credential is read from a file at a scrape:
  `password_file`, `bearer_token_file`, the username and password of
  `basic_auth_file`, a login's `field_files` and client secret,
  `community_file`, `secret_file` and similar keys, of a collector and of a
  static target. The value read is trimmed like a file's content.
- It does not work for the files that are opened as files: the `ca_file`,
  `cert_file` and `key_file` of `tls`, `private_key_file`,
  `known_hosts_file`, and descriptor and statement files.
- A reference in one of those keys, a reference that names no store of
  `secret_stores`, and a reference in a key that names no file are refused
  when the configuration loads.
- The exporter's own `web.basic_auth` accepts no reference.

| Key | Default | Notes |
| --- | --- | --- |
| `name` | — | **Required.** What a reference names the store by, and the `store` label of its self-metrics: letters, digits, hyphens and underscores. |
| `type` | — | **Required.** `vault`, `gcp` or `aws`. |
| `cache_ttl` | `5m` | How long a secret read is used before the store is asked again; `0s` asks at every read. |
| `max_stale` | `1h` | How long past `cache_ttl` a secret read before is still used while the store cannot be asked; `0s` for never. |
| `timeout` | `5s` | The most one request to the store takes, at most `1m`. |

**Secrets are kept in memory, never on disk.** A secret is read when a
scrape first needs it and is kept for `cache_ttl`, per secret. The fields of
one secret therefore cost one request, and a scrape within that time costs
none. A rotated secret is picked up when `cache_ttl` has passed.

**A store that is down does not stop the scrapes immediately.** When the
store cannot be reached after `cache_ttl`, the previously read secret is
used for `max_stale` longer, with one warning in the log and each such read
counted in `http_exporter_secret_store_stale_reads_total`. After that, and
for a secret that was never read, the read fails, and so does its scrape,
like a credential file that cannot be read. A store that failed is not asked
for the same secret again for 5 seconds.

With the [target allocator](ALLOCATOR.md), each exporter reads from the
stores itself and keeps its own secrets. The allocator checks the references
and contacts no store.

A request to a store is not part of a scrape's deadline: `timeout` bounds
it. `--dry-run` reads the references that a scrape would read, and warns
about one that cannot be read. [Self-metrics](SELF-METRICS.md#secret-stores)
count each store's requests, failures and stale reads, and the secrets that
it keeps.

### HashiCorp Vault

| Key | Default | Notes |
| --- | --- | --- |
| `address` | — | **Required.** The server's URL. |
| `namespace` | — | The Vault Enterprise namespace. |
| `auth.method` | — | **Required.** `kubernetes`, `token` or `approle`. |
| `auth.role` | — | `kubernetes`, required: the Vault role the service account logs in as. |
| `auth.jwt_file` | the pod's service account token | `kubernetes`: the token exchanged for a Vault token. |
| `auth.token_file` | `VAULT_TOKEN` | `token`: the file of the Vault token, read at each request, so one a Vault Agent renews is used as renewed. |
| `auth.role_id` | — | `approle`, required. |
| `auth.secret_id_file` | `VAULT_SECRET_ID` | `approle`: the file of the secret id. |
| `auth.mount` | `kubernetes`, `approle` | Where the auth method is mounted. |
| `tls` | — | How the server's certificate is verified, and a client certificate, as [`request.tls`](CONFIGURATION.md#collectors) is written. |

In a pod, `kubernetes` reads the service account token that the pod mounts,
so the pod must mount one: `automountServiceAccountToken: true`, the chart's
`serviceAccount.automount`, or a projected token with the audience that the
Vault role expects, named with `auth.jwt_file`. The files of `tls` are read
when the store is first used, not when the configuration loads.

The path is the API's path, the part after `/v1/`. For a KV version 2 mount
it has `data/` after the mount, `secret/data/orders-db`; for version 1 it
does not, `secret/orders-db`. A Vault secret consists of fields, so the
reference names one, `#password`. The token that a `kubernetes` or
`approle` login returns is kept in memory for its lease, and requested again
when Vault refuses it. The token's policy needs `read` on the paths.

### Google Cloud Secret Manager

```yaml
secret_stores:
  - name: gcp
    type: gcp
    project: my-project
collectors:
  - name: api
    request:
      type: http
      bearer_token_file: secret://gcp/api-token
    transform:
      type: jq
    metrics:
      - name: api_up
        expression: .up
```

| Key | Default | Notes |
| --- | --- | --- |
| `project` | — | **Required.** The project whose secrets are read. |
| `credentials_file` | `GOOGLE_APPLICATION_CREDENTIALS` | A service account's key file. Left out, the metadata server gives the token: Workload Identity on GKE, or the machine's service account. |
| `endpoint` | the public API | The API's URL, for a private endpoint. |

The reference names the secret. Its `latest` version is read, or the version
that `?version=` names. The payload is the credential; with `#field` it is a
member of the JSON object that the payload contains. The account needs
`roles/secretmanager.secretAccessor` on the secrets.

### AWS Secrets Manager

```yaml
secret_stores:
  - name: aws
    type: aws
    region: eu-west-1
collectors:
  - name: orders_db
    request:
      type: sql
      driver: postgres
      database: orders
      username: monitor
      password_file: secret://aws/prod/orders-db#password
      statement: SELECT count(*) AS orders FROM orders
    transform:
      type: csv
    metrics:
      - name: orders_total
        expression: orders
```

| Key | Default | Notes |
| --- | --- | --- |
| `region` | the SDK's, such as `AWS_REGION` | The region of the secrets. |
| `endpoint` | the public API | The API's URL, for a VPC endpoint. |

The reference names the secret by its name or its ARN. Its `SecretString`
is the credential; with `#field` it is a member of the JSON object that the
string contains, which is how the console writes a secret of keys and
values. `?version=` names a stage, such as `AWSPREVIOUS`, or a version id.
The credentials are those that the AWS SDK's default chain finds, as for an
[`s3file`](S3FILE.md) collector: a role for the service account (IRSA), EKS
Pod Identity, the instance's role or the environment. The role needs
`secretsmanager:GetSecretValue` on the secrets. The store exists in builds
that have the `s3file` request type, including the default build.
