# IMAP

The `imap` request type logs in to a mail server as a mail client does and
asks how much mail each mailbox holds: the greeting, `CAPABILITY`,
`STARTTLS`, a login, then `STATUS` for each mailbox the collector names.
No message is read, marked or moved, and no mailbox is opened unless the
collector [counts messages by a search](#counting-messages-by-a-search),
which opens one read-only. What the server said is handed to the transforms as one JSON document. Use it to
watch:

- how many messages a mailbox holds and how many are unread, such as a
  support inbox nobody is answering or a queue a job should be emptying;
- how many messages match a search: unanswered for more than a day,
  flagged, from one sender;
- that a mail server greets, offers TLS and takes the account, before the
  people who use it find out;
- the folders of an account, each with its counts, and the account's quota;
- the certificate the server presents, with
  [`$tls`](REQUESTS.md#timings-tls-and-the-final-url).

It needs the account's name and password, or an OAuth token where the
server takes nothing else ([Gmail](#gmail)). The
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
| `auth` | `plain`, or `login` where the server offers no `AUTH=PLAIN` | The mechanism: `plain`, `login` or `xoauth2`. |
| `xoauth2` | — | Where the access token of `AUTHENTICATE XOAUTH2` comes from, the block the [`smtp` type takes](SMTP.md#xoauth2); it implies `auth: xoauth2`, and takes the place of `password_file`. |
| `mailboxes` | `[INBOX]` | Up to 50 mailboxes, each by its name, such as `Archive/2026`, or by a pattern: `*` is any text and `%` any text within one level, so `*` is every mailbox of the account. A pattern is listed with `LIST`, and each mailbox of the list is asked about. May contain `{{param_name}}` placeholders. |
| `quota` | `false` | Also ask for the quota of the first mailbox, with `GETQUOTAROOT`, where the server has `QUOTA`. |
| `counts` | — | Up to 20 [counts of the messages that match a search](#counting-messages-by-a-search). |
| `allow_plaintext` | `false` | Log in over a connection that is not TLS, to a server that has none. The password is then sent as it is. |
| `tls` | — | How the server's certificate is verified, and a client certificate, as for [any type that speaks TLS](REQUESTS.md#tls). |
| `max_response_bytes` | the collector's limit | The most the server's responses may be, together. |
| `allowed_targets`, `denied_targets` | — | The hosts the collector may and may not reach, as for [any type that reaches a host](REQUESTS.md#restricting-targets). |

A probe takes `timeout` and `param_<name>` parameters, which fill the
placeholders of `mailboxes`, `counts` and the collector's
[label values](REQUESTS.md#in-label-values). A value written into a
mailbox's name may hold no control character, so a probe adds no command.
The decoder is `json`; use `jq`, `yq` or `python`.

Without `auth`, the login is `AUTHENTICATE PLAIN` where the server offers
it and `LOGIN` otherwise. A password or a token is never sent over a
connection that is not TLS: a
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
  ],
  "counts": [
    {"name": "waiting_a_day", "mailbox": "INBOX", "ok": true, "text": "SEARCH completed (Success)", "count": 3}
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

## Counting messages by a search

`counts` asks the server how many messages of a mailbox match a search,
which is what "is this inbox being answered" needs:

```yaml
request:
  type: imap
  username: support@example.com
  password_file: /etc/exporter/imap-password
  counts:
    - name: waiting_a_day
      search: UNANSWERED BEFORE {{date_imap:-1d}}
    - name: flagged
      search: FLAGGED
    - name: from_billing
      mailbox: Archive/2026
      search: FROM "billing@example.com" SINCE {{date_imap:-7d}}
metrics:
  - name: imap_messages_matching
    items: .counts[] | select(.ok)
    expression: .count
    labels:
      - name: search
        expression: .name
```

| Key | Default | Notes |
| --- | --- | --- |
| `name` | — | **Required.** What a rule reads the count by: letters, digits and underscores, each name once. |
| `mailbox` | `INBOX` | The mailbox searched, by its name; no pattern. May contain `{{param_name}}` placeholders. |
| `search` | — | **Required.** The criteria of an IMAP `SEARCH`, in printable ASCII; several must all match. |

Criteria most servers take: `ALL`, `UNSEEN`, `SEEN`, `UNANSWERED`,
`FLAGGED`, `DELETED`, `FROM "text"`, `TO "text"`, `SUBJECT "text"`,
`BEFORE date`, `SINCE date`, `LARGER bytes`, `SMALLER bytes`,
`KEYWORD flag`, and `NOT x` and `OR x y` of those. A date is written as
`9-Nov-2026`, which the [clock placeholder](REQUESTS.md#dates-and-times)
`{{date_imap}}` writes: `{{date_imap:-1d}}` is yesterday. `BEFORE` and
`SINCE` compare the day a message arrived, not its hour. Gmail also takes
`X-GM-RAW "older_than:2d is:unread"`, its own search box.

The mailbox is opened with `EXAMINE`, which is read-only: the server marks
nothing read and changes no flag, and the exporter fetches no message. A
`{{param_name}}` placeholder in a search must stand within double quotes,
as in `FROM "{{param_sender}}"`, and its value may hold no double quote or
backslash, so a probe changes the text searched for and never the
criteria.

A mailbox the server does not open and a search it refuses, as it does
criteria it cannot read, are answers: the count is in the document with
`ok` false, what the server said in `text` and `count` `null`, and the
scrape succeeds. A server without `ESEARCH` lists every match for the
exporter to count, which for a search matching very many messages can
reach `max_response_bytes`; narrow the search or raise the limit.

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
password. It logs in with `XOAUTH2`, an access token the collector gets
with a refresh token:

```yaml
request:
  type: imap
  username: monitor@example.com
  xoauth2:
    token_url: https://oauth2.googleapis.com/token
    client_id: 1234567890-abc.apps.googleusercontent.com
    client_secret_file: /etc/exporter/gmail-client-secret
    refresh_token_file: /etc/exporter/gmail-refresh-token
    scopes: [https://mail.google.com/]
```

The block's keys, how long a token is kept and how the refresh token is
made are those of the [`smtp` type's `xoauth2`](SMTP.md#xoauth2); one
refresh token with the scope `https://mail.google.com/` serves both.

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
the scrape as a limit exceeded. A scrape asks about at most 500 mailboxes
and 20 counts, each a command of its own. One connection is made for each scrape; none is
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
that carry it, and a token is shown the same way.

With `web.self_metrics.verbose`, a scrape is labelled with
`imap://host:port` and, as `http_method`, how far the session goes:
`CAPABILITY`, or `STATUS` with an account.

## Limitations

- No message is read: there is no metric of a message's subject, sender
  or age, only how many match a search.
- A search is sent without a charset, so its text is ASCII.
- The login is `PLAIN`, `LOGIN` or `XOAUTH2`. `OAUTHBEARER`, `CRAM-MD5`,
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
  wants another mechanism, often OAuth: set `xoauth2`.
- **A count with `ok` false and `Could not parse command`**: the server
  does not read the criteria. Dates are `9-Nov-2026`, text with a space is
  in double quotes, and `OR` takes exactly two criteria.
- **A mailbox with `ok` false and `[NONEXISTENT]`**: the name is not the
  server's. List them with `mailboxes: ["*"]` in a
  [debug run](CONFIGURATION.md#debug-probes), and mind the hierarchy
  separator, `/` or `.`.
- **`x509: certificate signed by unknown authority`**: name the server's
  authority in `tls.ca_file`.
