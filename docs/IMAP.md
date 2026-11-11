# IMAP

The `imap` request type logs in to a mail server as a mail client does and
asks how much mail each mailbox holds: the greeting, `CAPABILITY`,
`STARTTLS`, a login, then `STATUS` for each mailbox the collector names.
No mailbox is opened, so no message is read, marked or moved. What the
server said is handed to the transforms as one JSON document. Use it to
watch:

- how many messages a mailbox holds and how many are unread, such as a
  support inbox nobody is answering or a queue a job should be emptying;
- that a mail server greets, offers TLS and takes the account, before the
  people who use it find out;
- the folders of an account, each with its counts, and the account's quota;
- the certificate the server presents, with
  [`$tls`](REQUESTS.md#timings-tls-and-the-final-url).

It needs the account's name and password, and no API or OAuth client. The
[`smtp` type](SMTP.md) watches the sending side of the same server.

The type is opt-in: the default build and the published image leave it out,
so a build carries it only when asked
([Building with imap](#building-with-imap)).

## Building with imap

`OPT_IN_REQUEST_TYPES` names the opt-in types a build adds to the types it
carries, comma-separated:

```sh
make build OPT_IN_REQUEST_TYPES=imap              # bin/prometheus-universal-exporter
docker build --build-arg OPT_IN_REQUEST_TYPES=imap -t exporter:imap .
go build -tags "$(sh tools/request-type-tags.sh '' '' imap)" .
```

The tag is `request_type_imap`. A configuration with an `imap` collector
is refused by a build without the type, at startup, on a reload and by
`--dry-run`, saying how to build one that has it. The type is written with
the standard library alone, and adds no dependency.

## A collector

```yaml
collectors:
  - name: mailbox
    request:
      type: imap
      username: support@example.com
      password_file: /etc/exporter/imap-password
      mailboxes: [INBOX, "Archive/*"]
      quota: true
    transform:
      type: jq
    metrics:
      - name: imap_mailbox_messages
        description: Messages in the mailbox
        items: .mailboxes[] | select(.ok)
        expression: .messages
        labels:
          - name: mailbox
            expression: .name
      - name: imap_mailbox_unseen
        description: Messages in the mailbox that are not read
        items: .mailboxes[] | select(.ok)
        expression: .unseen
        labels:
          - name: mailbox
            expression: .name
      - name: imap_quota_usage_bytes
        items: .quota[] | select(.resource == "STORAGE")
        expression: .usage_bytes
```

```text
/probe?collector=mailbox&target=imaps://mail.example.com
```

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `imap`. |
| `username`, `password_file` | — | The account the session logs in to. The file is read at each scrape. Left out, the session does not log in, and the document holds the greeting and the capabilities alone. |
| `mailboxes` | `[INBOX]` | Up to 50 mailboxes, each by its name, such as `Archive/2026`, or by a pattern: `*` is any text and `%` any text within one level, so `*` is every mailbox of the account. A pattern is listed with `LIST`, and each mailbox of the list is asked about. May contain `{{param_name}}` placeholders. |
| `quota` | `false` | Also ask for the quota of the first mailbox, with `GETQUOTAROOT`, where the server has `QUOTA`. |
| `allow_plaintext` | `false` | Log in over a connection that is not TLS, to a server that has none. The password is then sent as it is. |
| `tls` | — | How the server's certificate is verified, and a client certificate, as for [any type that speaks TLS](REQUESTS.md#tls). |
| `max_response_bytes` | the collector's limit | The most the server's responses may be, together. |
| `allowed_targets`, `denied_targets` | — | The hosts the collector may and may not reach, as for [any type that reaches a host](REQUESTS.md#restricting-targets). |

A probe takes `timeout` and `param_<name>` parameters, which fill the
placeholders of `mailboxes` and the collector's
[label values](REQUESTS.md#in-label-values). A value written into a
mailbox's name may hold no control character, so a probe adds no command.
The decoder is `json`; use `jq`, `yq` or `python`.

The login is `AUTHENTICATE PLAIN` where the server offers it and `LOGIN`
otherwise. A password is never sent over a connection that is not TLS: a
server that offers no `STARTTLS` fails the scrape before the login, unless
`allow_plaintext` says the server has none.

## The target

The target is `host`, `host:port`, `imap://host:port` or
`imaps://host:port`. The port is 143, and 993 for `imaps://`, whose session
is TLS from the first byte; on `imap://` TLS is begun with `STARTTLS` where
the server offers it. A path, a user or another scheme is refused with
`400` before anything is sent. Logs and labels show the target as
`imap://host:port` or `imaps://host:port`.

## The document

```json
{
  "greeting": "Gimap ready for requests",
  "tls": true,
  "starttls": false,
  "capabilities": ["IDLE", "IMAP4REV1", "QUOTA", "STATUS=SIZE"],
  "authenticated": true,
  "mailboxes": [
    {"name": "INBOX", "ok": true, "text": "Success", "messages": 12, "unseen": 3, "recent": 0,
     "uidnext": 44, "uidvalidity": 7, "size": 123456, "flags": []},
    {"name": "Archive/2026", "ok": true, "text": "Success", "messages": 840, "unseen": 0, "recent": 0,
     "uidnext": 841, "uidvalidity": 9, "size": null, "flags": ["\\HasNoChildren"]}
  ],
  "quota": [
    {"root": "", "resource": "STORAGE", "usage": 1024, "limit": 15728640,
     "usage_bytes": 1048576, "limit_bytes": 16106127360}
  ]
}
```

`capabilities` are those the server names after the login, in upper case.
A mailbox's counts are what `STATUS` answered; `size`, its bytes, is `null`
unless the server counts it (`STATUS=SIZE`). `flags` are those `LIST` gave
a mailbox found by a pattern. `uidnext` grows by one for each message that
arrives, so its increase is the mail received, whatever was deleted since.
A `STORAGE` quota is counted in units of 1024 bytes, which `usage_bytes`
and `limit_bytes` give in bytes.

A mailbox the server refuses `STATUS` for is an answer: it is in the
document with `ok` false, what the server said in `text` and its counts
`null`, and the scrape succeeds. A mailbox that cannot be selected, such as
Gmail's `[Gmail]`, is left out of a pattern's list. A name in another
alphabet is read and written as it is, `Entwürfe`, whatever the server's
encoding of it.

## Gmail

`imap.gmail.com` takes an
[app password](https://support.google.com/accounts/answer/185833), which an
account with 2-Step Verification can make, on port 993:

```yaml
request:
  type: imap
  username: monitor@example.com
  password_file: /etc/exporter/gmail-app-password   # the 16 letters
  mailboxes: [INBOX, "[Gmail]/Spam"]
```

```text
/probe?collector=gmail&target=imaps://imap.gmail.com
```

Gmail's labels are its mailboxes, so `mailboxes: ["*"]` counts every label.
A Workspace account whose administrator allows OAuth alone has no app
password; read its mailbox through the Gmail API instead, with an
[`http` collector and an `oauth2` login](AUTHENTICATION.md).

## Static targets

A [static target](STATIC-TARGETS.md) of an `imap` collector is a mail
server, written as a probe's target is. Its `request` may set `timeout`,
and `username`, `password_file` and `tls` for a server with an account or a
certificate authority of its own; a target that sets `username` logs in
with its own `password_file`, and a `tls` block replaces the collector's
whole.

## Limits

A scrape ends at its deadline, the probe's `timeout` or the scrape
timeout, and after 30 seconds when it has neither. One line of a response
may be 64 KiB, and the responses together `max_response_bytes`; more fails
the scrape as a limit exceeded. A scrape asks about at most 500 mailboxes,
each a command of its own. One connection is made for each scrape; none is
kept.

## Errors and self-metrics

The scrape fails, naming the server and its answer, when the greeting is
not `* OK`, `CAPABILITY`, `STARTTLS` or `LIST` is refused, TLS cannot be
begun, the certificate does not verify, or the login is refused:

```text
the server imap.example.com:993 refused AUTHENTICATE PLAIN for support@example.com with NO [AUTHENTICATIONFAILED] Invalid credentials (Failure)
```

That is the exporter's log; whoever called `/probe` is answered `NO` and
`[withheld; in the exporter's log]` for the server's words, as for
[any failed probe](CONFIGURATION.md#what-a-failed-probes-caller-is-told). A
password is in no error, log line or debug report: a
[debug probe](CONFIGURATION.md#debug-probes) shows each command and its
answer, with `AUTHENTICATE PLAIN ***` and `LOGIN "name" ***` for the ones
that carry it.

With `web.self_metrics.verbose`, a scrape is labelled with
`imap://host:port` and, as `http_method`, how far the session goes:
`CAPABILITY`, or `STATUS` with an account.

## Limitations

- No mailbox is opened: there is no search, so no count of messages by
  sender, age or flag, and no message is read.
- The login is `PLAIN` or `LOGIN`. `XOAUTH2`, `OAUTHBEARER`, `CRAM-MD5`,
  `NTLM` and `GSSAPI` are not spoken.
- `unseen` is what the server counts as not `\Seen`, for the whole mailbox;
  Gmail counts by message, not by conversation as its web page does.
- The quota is that of the first mailbox's root. Many servers have no
  `QUOTA`, and the list is then empty.

## Troubleshooting

- **`the account's password is not sent over a connection that is not
  TLS`**: the server offered no `STARTTLS`. Use `imaps://` on 993;
  `allow_plaintext` only for a server that has no TLS at all.
- **`NO [AUTHENTICATIONFAILED]` from Gmail**: the account's own password is
  not taken; use an app password, and check that IMAP is on for the account.
- **`takes no LOGIN (LOGINDISABLED) and offers no AUTH=PLAIN`**: the server
  wants a mechanism this type does not speak, often OAuth.
- **A mailbox with `ok` false and `[NONEXISTENT]`**: the name is not the
  server's. List them with `mailboxes: ["*"]` in a
  [debug run](CONFIGURATION.md#debug-probes), and mind the hierarchy
  separator, `/` or `.`.
- **`x509: certificate signed by unknown authority`**: name the server's
  authority in `tls.ca_file`.
