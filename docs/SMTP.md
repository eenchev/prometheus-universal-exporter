# SMTP

The `smtp` request type talks to a mail server as a client about to send a
message does, and stops before the message: it reads the greeting, says
`EHLO`, begins TLS with `STARTTLS`, logs in with `AUTH` when the collector
has an account, and, when it names a sender and recipients, asks the server
about them with `MAIL FROM` and `RCPT TO`. No `DATA` follows, so nothing is
ever sent. What the server said at each step is handed to the transforms as
one JSON document. Use it to watch:

- that a mail server greets, offers TLS and takes the account your
  applications send with, before they find out;
- what a server advertises: its size limit, its `AUTH` mechanisms, whether
  `STARTTLS` is on offer;
- whether a server would take mail for an address, by its answer to
  `RCPT TO`;
- the certificate the server presents, with
  [`$tls`](REQUESTS.md#timings-tls-and-the-final-url).

The [`tcp` type](TCP.md) can hold the same conversation as `steps` of text;
this type knows the protocol, so the answers come as numbers and labels,
and it can log in.

The type is opt-in: the default build and the published image leave it out,
so a build carries it only when asked
([Building with smtp](#building-with-smtp)).

## Building with smtp

`OPT_IN_REQUEST_TYPES` names the opt-in types a build adds to the types it
carries, comma-separated:

```sh
make build OPT_IN_REQUEST_TYPES=smtp              # bin/prometheus-universal-exporter
docker build --build-arg OPT_IN_REQUEST_TYPES=smtp -t exporter:smtp .
go build -tags "$(sh tools/request-type-tags.sh '' '' smtp)" .
```

The tag is `request_type_smtp`. A configuration with an `smtp` collector
is refused by a build without the type, at startup, on a reload and by
`--dry-run`, saying how to build one that has it. The type is written with
the standard library alone, and adds no dependency.

## A collector

```yaml
collectors:
  - name: mail
    request:
      type: smtp
      username: monitor@example.com
      password_file: /etc/exporter/smtp-password
      mail_from: monitor@example.com
      rcpt_to: [postmaster@example.com]
    transform:
      type: jq
    metrics:
      - name: smtp_tls
        description: Whether the session is over TLS
        expression: .tls
      - name: smtp_size_limit_bytes
        description: The largest message the server takes
        expression: .size
      - name: smtp_auth_mechanism_offered
        items: .auth_mechanisms[]
        expression: "1"
        labels:
          - name: mechanism
            expression: .
      - name: smtp_recipient_accepted
        description: Whether the server would take mail for the address
        items: .rcpt_to[]
        expression: .accepted
        labels:
          - name: address
            expression: .address
          - name: code
            expression: .code
```

```text
/probe?collector=mail&target=smtp.example.com:587
```

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `smtp`. |
| `starttls_mode` | `auto` | `auto` begins TLS with `STARTTLS` where the server offers it; `required` fails the scrape where it does not; `never` leaves the session in plaintext. Not for an `smtps://` target. |
| `hello` | the exporter's address, as `[192.0.2.7]` | The name given in `EHLO`. |
| `username`, `password_file` | — | The account `AUTH` logs in with. The file is read at each scrape. Left out, the session does not log in. |
| `auth` | `plain`, or `login` where the server offers that alone | The mechanism: `plain`, `login` or `xoauth2`. |
| `xoauth2` | — | Where the access token of `AUTH XOAUTH2` comes from (see [XOAUTH2](#xoauth2)); it implies `auth: xoauth2`. |
| `mail_from` | — | The sender `MAIL FROM` names, or `<>` for the null sender. May contain `{{param_name}}` placeholders. |
| `rcpt_to` | — | Up to 20 recipients, each asked with a `RCPT TO` of its own, after `mail_from`. May contain placeholders. |
| `tls` | — | How the server's certificate is verified, and a client certificate, as for [any type that speaks TLS](REQUESTS.md#tls). |
| `max_response_bytes` | the collector's limit | The most the server's replies may be, together. |
| `allowed_targets`, `denied_targets` | — | The hosts the collector may and may not reach, as for [any type that reaches a host](REQUESTS.md#restricting-targets). |

A probe takes `timeout` and `param_<name>` parameters, which fill the
placeholders of `mail_from`, `rcpt_to` and the collector's
[label values](REQUESTS.md#in-label-values). A value written into an address
may hold no blank, angle bracket or control character, so a probe adds no
command. The decoder is `json`; use `jq`, `yq` or `python`.

A password or a token is never sent over a connection that is not TLS: a
server that offers no `STARTTLS` fails the scrape before `AUTH`, unless
`starttls_mode: never` says the server has none.

## The target

The target is `host`, `host:port`, `smtp://host:port` or
`smtps://host:port`. The port is 25, and 465 for `smtps://`, whose session
is TLS from the first byte; write the port for submission,
`smtp.example.com:587`. A path, a user or another scheme is refused with
`400` before anything is sent. Logs and labels show the target as
`smtp://host:port` or `smtps://host:port`.

## The document

```json
{
  "greeting": {"code": 220, "text": "smtp.example.com ESMTP ready"},
  "tls": true,
  "starttls": true,
  "extensions": {"SIZE": "35882577", "8BITMIME": true, "AUTH": "LOGIN PLAIN XOAUTH2", "PIPELINING": true},
  "auth_mechanisms": ["LOGIN", "PLAIN", "XOAUTH2"],
  "size": 35882577,
  "auth": {"mechanism": "PLAIN", "code": 235},
  "mail_from": {"address": "monitor@example.com", "code": 250, "accepted": true, "text": "2.1.0 OK"},
  "rcpt_to": [
    {"address": "postmaster@example.com", "code": 250, "accepted": true, "text": "2.1.5 OK"}
  ]
}
```

`extensions` are those of the `EHLO` within TLS when TLS was begun, each
with its parameters as text, or `true` when it has none. `size` is `SIZE`'s
limit, `null` when the server names none. `auth` and `mail_from` are `null`
when the session did not get that far, and `rcpt_to` is empty. `accepted`
is whether the code is a 2xx.

A recipient the server refuses is an answer: it is in the document with its
code and text, and the scrape succeeds. So is a refused `MAIL FROM`, after
which no `RCPT TO` is sent.

## Gmail

`smtp.gmail.com` takes all three mechanisms, on port 587 with `STARTTLS`
and on 465 with TLS from the first byte.

With an [app password](https://support.google.com/accounts/answer/185833),
which an account with 2-Step Verification can make, `PLAIN` and `LOGIN`
work as with any server:

```yaml
request:
  type: smtp
  username: monitor@example.com
  password_file: /etc/exporter/gmail-app-password   # the 16 letters
  # auth: login                                     # PLAIN is the default
```

```text
/probe?collector=gmail&target=smtp.gmail.com:587
/probe?collector=gmail&target=smtps://smtp.gmail.com
```

Without one, as for a Workspace account whose administrator allows OAuth
alone, use `XOAUTH2`.

### XOAUTH2

`AUTH XOAUTH2` sends the account's name and an OAuth 2.0 access token.
Gmail's last an hour, so the collector either reads one that something else
keeps current, or gets its own with a refresh token:

```yaml
request:
  type: smtp
  username: monitor@example.com
  xoauth2:
    token_url: https://oauth2.googleapis.com/token
    client_id: 1234567890-abc.apps.googleusercontent.com
    client_secret_file: /etc/exporter/gmail-client-secret
    refresh_token_file: /etc/exporter/gmail-refresh-token
    scopes: [https://mail.google.com/]
```

| Key | Notes |
| --- | --- |
| `token_file` | A file holding an access token, read at each scrape. Not beside the keys below. |
| `token_url` | The token endpoint of the refresh token grant, `https://`. |
| `client_id`, `client_id_file` | The OAuth client's id, written or in a file. One of them. |
| `client_secret_file` | The client's secret; left out for a public client. |
| `refresh_token_file` | The refresh token, read when a token is asked for. |
| `scopes` | Sent as `scope`. For Gmail, `https://mail.google.com/`. |

The token the endpoint answers with is kept until a minute before it
expires, so the endpoint is asked about once an hour, and again when the
server refuses the token kept. The request to the endpoint is made
with the system's certificates and the proxy the environment names;
`request.tls` and the collector's target lists are the mail server's. The refresh token is made once, outside the exporter, by a
consent for the scope with a client of your Google Cloud project; a client
left in "Testing" has its refresh tokens expire after seven days.

Gmail's submission servers answer `RCPT TO` with 250 for any address that
is well formed, and report what cannot be delivered later, by mail: a
recipient check says something about a mailbox only when asked of the
servers that take the domain's mail, its MX hosts, on port 25.

## Static targets

A [static target](STATIC-TARGETS.md) of an `smtp` collector is a mail
server, written as a probe's target is. Its `request` may set `timeout`,
and `username`, `password_file` and `tls` for a server with an account or a
certificate authority of its own; a target that sets `username` logs in
with its own `password_file`, and a `tls` block replaces the collector's
whole.

## Limits

A scrape ends at its deadline, the probe's `timeout` or the scrape
timeout, and after 30 seconds when it has neither. One reply may be 64 KiB,
and the replies together `max_response_bytes`; more fails the scrape as a
limit exceeded. One connection is made for each scrape and closed with
`QUIT`; none is kept.

## Errors and self-metrics

The scrape fails, naming the server and its reply, when the greeting is not
a 220, `EHLO` or `STARTTLS` is refused, TLS cannot be begun where it must
be, the certificate does not verify, or `AUTH` is refused:

```text
the server smtp.example.com:587 refused AUTH PLAIN for monitor@example.com with 535 5.7.8 Username and Password not accepted.
```

That is the exporter's log; whoever called `/probe` is answered the code
and `[withheld; in the exporter's log]` for the server's words, as for
[any failed probe](CONFIGURATION.md#what-a-failed-probes-caller-is-told). A password and a token are in no error, log line or debug report: a
[debug probe](CONFIGURATION.md#debug-probes) shows each command and its reply, with
`AUTH PLAIN ***` for the ones that carry a credential.

With `web.self_metrics.verbose`, a scrape is labelled with
`smtp://host:port` and, as `http_method`, how far the session goes: `EHLO`,
`AUTH`, `MAIL` or `RCPT`.

## Limitations

- Nothing is sent: there is no `DATA`, and so no check that a message is
  delivered. Watch delivery from the receiving side.
- `AUTH` is `PLAIN`, `LOGIN` or `XOAUTH2`. `CRAM-MD5`, `OAUTHBEARER`,
  `NTLM` and `GSSAPI` are not spoken.
- A recipient check is what the server answers at `RCPT TO`, which many
  servers answer with 250 for every address, so as not to tell which
  exist.
- `VRFY` and `EXPN` are not sent.

## Troubleshooting

- **`the account's credential is not sent over a connection that is not
  TLS`**: the server offered no `STARTTLS`. Use port 587, or `smtps://` on
  465; `starttls_mode: never` only for a server that has no TLS at all.
- **`535 5.7.8 Username and Password not accepted` from Gmail**: the
  account's own password is not taken; use an app password or `xoauth2`.
- **`the token endpoint ... answered HTTP status 400, invalid_grant`**: the
  refresh token was revoked or has expired; make a new one.
- **`greeted with 554`**, or a timeout on port 25: the server, or the
  network between, refuses this client; many networks block port 25
  outbound.
- **`x509: certificate signed by unknown authority`**: name the server's
  authority in `tls.ca_file`.
