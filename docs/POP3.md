# POP3

The `pop3` request type logs in to a mail server as a client that fetches
mail over POP3 does and asks how much mail the mailbox holds: the greeting,
`CAPA`, `STLS`, a login, then `STAT`. No message is fetched or deleted. What the server said is handed to the transforms as one JSON
document. Use it to watch:

- how many messages wait in a mailbox and how large they are together,
  such as a mailbox a job should be emptying;
- that a POP3 server greets, offers TLS and takes the account, by the
  login its clients use;
- the certificate the server presents, with
  [`$tls`](REQUESTS.md#timings-tls-and-the-final-url).

A server that also speaks IMAP says more to the [`imap` type](IMAP.md):
each folder's counts, unread messages and searches. POP3 knows one mailbox
and two numbers.

The type is opt-in: the default build and the published image leave it out
(see [Building with pop3](#building-with-pop3)).

## Building with pop3

`OPT_IN_REQUEST_TYPES` lists, comma-separated, the opt-in types that a
build adds:

```sh
make build OPT_IN_REQUEST_TYPES=pop3              # bin/prometheus-universal-exporter
docker build --build-arg OPT_IN_REQUEST_TYPES=pop3 -t exporter:pop3 .
go build -tags "$(sh tools/request-type-tags.sh '' '' pop3)" .
```

The build tag is `request_type_pop3`. A build without the type refuses a
configuration with a `pop3` collector at startup, on a reload and with
`--dry-run`, and the error says how to build one that has it. The type uses
only the standard library and adds no dependency.

## A collector

```yaml
collectors:
  - name: mailbox
    request:
      type: pop3
      username: orders@example.com
      password_file: /etc/exporter/pop3-password
    transform:
      type: jq
    metrics:
      - name: pop3_mailbox_messages
        description: Messages waiting in the mailbox
        expression: .messages
      - name: pop3_mailbox_size_bytes
        description: The size of the messages waiting, together
        expression: .size_bytes
```

```text
/probe?collector=mailbox&target=pop3s://mail.example.com
```

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `pop3`. |
| `username`, `password_file` | — | The account the session logs in to. The file is read at each scrape. Left out, the session does not log in, and the document holds the greeting and the capabilities alone. |
| `auth` | the server's (see [Logging in](#logging-in)) | The mechanism: `user`, `apop`, `plain`, `login`, `cram-md5`, `xoauth2` or `oauthbearer`. |
| `xoauth2` | — | Where the access token of `xoauth2` and `oauthbearer` comes from, the block the [`smtp` type takes](SMTP.md#xoauth2); it implies `auth: xoauth2`, and takes the place of `password_file`. |
| `list` | `false` | Also ask `LIST`, for the size of the largest message: `largest_bytes` in the document. The answer has a line for each message. |
| `allow_plaintext` | `false` | Log in over a connection that is not TLS, to a server that has none. |
| `tls` | — | How the server's certificate is verified, and a client certificate, as for [any type that speaks TLS](REQUESTS.md#tls). |
| `max_response_bytes` | the collector's limit | The most the server's responses may be, together. |
| `allowed_targets`, `denied_targets` | — | The hosts the collector may and may not reach, as for [any type that reaches a host](REQUESTS.md#restricting-targets). |

A probe takes `timeout` and `param_<name>` parameters, which fill the
collector's [label values](REQUESTS.md#in-label-values). The decoder is `json`, whether `decoder.type` is left out or set to it. Any
other decoder, and a transform that does not read JSON (`regex`, `csv`,
`css`, `prometheus`), is refused when the configuration loads; use `jq`,
`yq` or `python`.

## Logging in

`auth` names how the session logs in:

| `auth` | What is sent | Needs |
| --- | --- | --- |
| `user` | `USER` and `PASS`, the password as it is | `password_file` |
| `apop` | `APOP`, an MD5 digest of the greeting's timestamp and the password | `password_file`, and a server whose greeting has a timestamp |
| `plain` | `AUTH PLAIN` (SASL) | `password_file` |
| `login` | `AUTH LOGIN` (SASL) | `password_file` |
| `cram-md5` | `AUTH CRAM-MD5` (SASL), an HMAC of the server's challenge | `password_file` |
| `xoauth2` | `AUTH XOAUTH2` (SASL), an OAuth 2.0 access token | `xoauth2` |
| `oauthbearer` | `AUTH OAUTHBEARER` (SASL, RFC 7628), the same token | `xoauth2` |

The token of `xoauth2` and `oauthbearer` is sent in answer to the server's
`+`, as Microsoft 365 takes it; a server that refuses the mechanism's name
alone is asked once more with the token in the `AUTH` command, as Google
writes Gmail's exchange.

Left out, the login is `user` where the server's `CAPA` names `USER`, or
answers nothing, as an old server's does; else `plain`, else `login`, of
the mechanisms it names. A collector with an `xoauth2` block logs in with
`xoauth2`. A SASL mechanism `auth` names must be one the server's `CAPA`
names, and the scrape fails saying which it does name otherwise.

A credential is never sent over a connection that is not TLS: a server that
offers no `STLS` fails the scrape before the login, unless
`allow_plaintext` says the server has none. That holds for `apop` and
`cram-md5` too, which send no password but let whoever reads the session
test guesses of it.

### Gmail and Microsoft 365

`pop.gmail.com` takes an
[app password](https://support.google.com/accounts/answer/185833) on port
995, with POP turned on in the account's settings:

```yaml
request:
  type: pop3
  username: monitor@example.com
  password_file: /etc/exporter/gmail-app-password
```

```text
/probe?collector=gmail&target=pop3s://pop.gmail.com
```

An account that takes OAuth alone, as a Workspace account can be set to and
a Microsoft 365 mailbox is, logs in with `xoauth2`, an access token the
collector gets with a refresh token:

```yaml
request:
  type: pop3
  username: monitor@example.com
  xoauth2:
    token_url: https://oauth2.googleapis.com/token
    client_id: 1234567890-abc.apps.googleusercontent.com
    client_secret_file: /etc/exporter/gmail-client-secret
    refresh_token_file: /etc/exporter/gmail-refresh-token
    scopes: [https://mail.google.com/]
```

For Microsoft 365 the target is `pop3s://outlook.office365.com`, the
endpoint `https://login.microsoftonline.com/<tenant>/oauth2/v2.0/token` and
the scope `https://outlook.office.com/POP.AccessAsUser.All`. The block's
keys, how long a token is kept and how the refresh token is made are those
of the [`smtp` type's `xoauth2`](SMTP.md#xoauth2), and so is
[an application's own token](SMTP.md#an-applications-own-token), for a
service account.

Gmail hands a POP3 client the messages its POP setting covers, which is not
what the inbox shows; its [`imap`](IMAP.md#gmail) counts are the inbox's.

## The target

The target is `host`, `host:port`, `pop3://host:port` or
`pop3s://host:port`. The port is 110, and 995 for `pop3s://`, whose session
is TLS from the first byte; on `pop3://` TLS is begun with `STLS` where the
server offers it. A path, a user or another scheme is refused with `400`
before anything is sent. Logs and labels show the target as
`pop3://host:port` or `pop3s://host:port`.

## The document

```json
{
  "greeting": "POP3 server ready <1896.697170952@mail.example.com>",
  "tls": true,
  "starttls": false,
  "capabilities": ["PIPELINING", "SASL", "TOP", "UIDL", "USER"],
  "sasl": ["LOGIN", "PLAIN", "XOAUTH2"],
  "apop": true,
  "authenticated": true,
  "auth": "user",
  "messages": 12,
  "size_bytes": 123456,
  "largest_bytes": 98765
}
```

`capabilities` are the first word of each line of the server's `CAPA`, in
upper case, and `sasl` the mechanisms its `SASL` line names; both are empty
for a server that knows no `CAPA`. `apop` says whether the greeting has the
timestamp `APOP` needs. `auth` is the mechanism the login used. `messages`
and `size_bytes` are what `STAT` answered, the messages in the mailbox and
their bytes together; with `auth` they are `null` when the collector has no
account. `largest_bytes` is the size of the largest message, `null` unless
the collector sets `list`, and for a mailbox without a message.

## Static targets

A [static target](STATIC-TARGETS.md) of a `pop3` collector is a mail
server, written as a probe's target is. Its `request` may set `timeout`,
and `username`, `password_file` and `tls` for a server with an account or a
certificate authority of its own; a target that sets `username` logs in
with its own `password_file`, and a `tls` block replaces the collector's
whole. A target of a collector that logs in with a token sets no
`password_file`; its `username` alone names the mailbox the collector's
token logs in to, which is how one
[application's token](SMTP.md#an-applications-own-token) watches many.

## Limits

A scrape ends at its deadline, the probe's `timeout` or the scrape
timeout, and after 30 seconds when it has neither. One line of a response
may be 4 KiB, a `CAPA` answer 200 lines, and the responses together
`max_response_bytes`, the lines of `LIST` among them, about 10 bytes a
message; more fails the scrape, the last as a limit exceeded.
One connection is made for each scrape; none is kept.

## Errors and self-metrics

The scrape fails, naming the server and its answer, when the greeting is
not `+OK`, `STLS` is refused, TLS cannot be begun, the certificate does not
verify, the login is refused, or `STAT` or `LIST` is:

```text
the server mail.example.com:995 refused PASS for orders@example.com with -ERR [AUTH] Username and password not accepted.
```

That is the exporter's log; whoever called `/probe` is answered `-ERR` and
`[withheld; in the exporter's log]` for the server's words, as for
[any failed probe](CONFIGURATION.md#what-a-failed-probes-caller-is-told). A
password, a digest and a token are in no error, log line or debug report: a
[debug probe](CONFIGURATION.md#debug-probes) shows each command and its
answer, with `PASS ***`, `APOP name ***` and `***` for the lines that carry
them. A server that says back in its `-ERR` the command it refuses has
what reads as a credential there masked, as `password=<redacted>`.

With `web.self_metrics.verbose`, a scrape is labelled with
`pop3://host:port` and, as `http_method`, how far the session goes: `CAPA`,
or `STAT` with an account.

## Limitations

- A POP3 server lets one session at a time into a mailbox. A scrape holds
  it for a moment, and fails with the server's `-ERR [IN-USE]` while a mail
  client, or another scrape of the same account, is in it; that client
  fails the same way if it comes while a scrape is.
- No message is fetched: there is no age of the oldest message. The
  [`imap` type](IMAP.md) counts by a search and says when the oldest
  match arrived.
- `NTLM`, `GSSAPI` and `SCRAM` logins are not spoken.
- A SASL credential is sent in answer to the server's challenge, one
  round trip more than the form that puts it in the `AUTH` command, which
  not every server takes.

## Troubleshooting

- **`the account's credential is not sent over a connection that is not
  TLS`**: the server offered no `STLS`. Use `pop3s://` on 995;
  `allow_plaintext` only for a server that has no TLS at all.
- **`does not offer the SASL mechanism …`**: `auth` names a mechanism the
  server's `CAPA` does not. The error lists those it does; `user` and
  `apop` need none.
- **`holds no timestamp, so the server takes no APOP`**: the server does
  not speak APOP; use `user` or a SASL mechanism.
- **`-ERR [AUTH]` from Gmail**: the account's own password is not taken;
  use an app password, and check that POP is on for the account.
- **`-ERR [IN-USE]`**: another session has the mailbox. Scrape less often,
  or from one exporter.
- **`x509: certificate signed by unknown authority`**: name the server's
  authority in `tls.ca_file`.
