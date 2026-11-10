# Prometheus Universal Exporter — `imap` Request Type Specification

This is the `imap` request type's section of § 5.1 of the
[exporter specification](SPECIFICATION-EXPORTER.md), in a document of its
own so that a change to the type is a change to this document. Everything
the request types share — the registry and the rules every type is held to
(§ 5.1), building without a type, decoding, transforms, error policies,
limits, caching, static targets and exposition — is in the exporter
specification, and a section number here, such as § 42.14, is one of its
sections. What a user reads of the type is [IMAP.md](IMAP.md).

## `imap`

An `imap` collector holds an IMAP session (RFC 3501, RFC 9051) with the
probe's `target`, in which it selects no mailbox, and hands what the server
said to the transforms as a JSON document. It MUST need nothing outside
Go's standard library, and MUST be opt-in: in no build that does not ask
for it with `request_type_imap`.

| Key | Default | Rule |
| --- | --- | --- |
| `mailboxes` | `[INBOX]`, with an account | At most 50, none empty or with a control character; each MAY hold placeholders. One with `*` or `%` is a pattern. It MUST need `username`. |
| `quota` | `false` | It MUST need `username`. |
| `allow_plaintext` | `false` | — |

Of the common part it takes `username`, `password_file`, `tls`,
`max_response_bytes`, `allowed_targets` and `denied_targets`; `username` and
`password_file` MUST be set together, and MUST hold no placeholder. A static
target's request block takes `timeout`, `username`, `password_file` and
`tls`.

- A target MUST be `host`, `host:port`, `imap://host:port` or
  `imaps://host:port`, port 143, and 993 for `imaps://`; anything else, a
  path and a user MUST be refused, a probe's with `400` before anything is
  sent. `imaps://` MUST be TLS from the first byte.
- The session MUST be: the greeting, which MUST be `* OK`; `CAPABILITY`;
  on `imap://`, where the server names `STARTTLS`, `STARTTLS`, the
  handshake with the `tls` block's settings, and `CAPABILITY` again; with a
  `username`, the login, `CAPABILITY` again, a `STATUS` for each mailbox
  and, with `quota` where the server names `QUOTA`, `GETQUOTAROOT` of the
  first mailbox; `LOGOUT`. `SELECT`, `EXAMINE` and every command that reads
  or changes a message MUST never be sent.
- Bytes the server sent after its answer to `STARTTLS` and before the
  handshake MUST fail the scrape, and MUST NOT be read as the server's.
- A password MUST NOT be sent over a connection that is not TLS unless
  `allow_plaintext` is set: the scrape MUST fail before the login.
- The login MUST be `AUTHENTICATE PLAIN` where the server names
  `AUTH=PLAIN`, as RFC 4616 has it with an empty authorization identity, in
  the command where the server names `SASL-IR` and in answer to its
  continuation request otherwise; else `LOGIN` with the name and the
  password as quoted strings, which MUST be refused, before anything of
  them is sent, where the server names `LOGINDISABLED` or either holds a
  character outside printable ASCII. Any answer but `OK` MUST fail the
  scrape, naming the command, the account and the server's answer.
- A mailbox named without `*` and `%` MUST be asked about by its name. One
  named by a pattern MUST be listed with `LIST "" <pattern>`, and each
  mailbox of the list without `\Noselect` and `\NonExistent` asked about,
  with the flags the list gave it. A mailbox MUST be asked about once. A
  scrape that would ask about more than 500 MUST fail.
- `STATUS` MUST ask for `MESSAGES`, `UNSEEN`, `UIDNEXT` and `UIDVALIDITY`,
  for `RECENT` where the server names `IMAP4rev1`, and for `SIZE` where it
  names `STATUS=SIZE` or `IMAP4rev2`. A `STATUS` answered `NO` or `BAD` MUST
  be a mailbox of the document with `ok` false, the answer's text and null
  counts, and MUST NOT fail the scrape; `CAPABILITY`, `STARTTLS` and `LIST`
  answered so MUST fail it.
- A mailbox's name MUST be written in a command in modified UTF-7
  (RFC 3501 § 5.1.3), as a quoted string with `\` and `"` escaped, and MUST
  be read from a response as an atom, a quoted string or a literal, decoded
  from modified UTF-7 where it is that and kept as it came where it is not.
  A placeholder's value written into a name MUST hold no control character,
  and one that does MUST be refused before anything is sent.
- The document MUST hold `greeting`, the greeting's text; `tls` and
  `starttls`; `capabilities`, those of the last `CAPABILITY`, in upper
  case, sorted, each once and as the server wrote it, at most 200 of at
  most 100 bytes each; `authenticated`; `mailboxes`, each with `name`,
  `ok`, `text`, `messages`, `unseen`, `recent`, `uidnext`, `uidvalidity`
  and `size`, null where the server gave none, and `flags`; and `quota`,
  each resource of each root with `root`, `resource`, `usage` and `limit`,
  and for `STORAGE` `usage_bytes` and `limit_bytes`, 1024 times those.
- One line of a response, its literals counted, MUST be at most 64 KiB, and
  the responses together the response limit (§ 20); more MUST fail the
  scrape, the latter as a limit exceeded. A response that is no IMAP MUST
  fail the scrape saying so.
- A scrape MUST end at its deadline, and after 30 seconds without one. One
  connection MUST be made for a scrape, through the target policy
  (§ 42.14), and none kept.
- What the server said MUST NOT reach a failed probe's caller (§ 19.4), and
  a password MUST be in no error, log line or debug report: a traced
  command that carries one MUST show `***` in its place.
- The verbose self-metrics MUST label a scrape `imap://host:port` or
  `imaps://host:port`, and its `http_method` `STATUS` with an account and
  `CAPABILITY` without.
