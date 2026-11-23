# Prometheus Universal Exporter — `pop3` Request Type Specification

This is the `pop3` request type's section of § 5.1 of the
[exporter specification](SPECIFICATION-EXPORTER.md), in a document of its
own so that a change to the type is a change to this document. Everything
the request types share — the registry and the rules every type is held to
(§ 5.1), building without a type, decoding, transforms, error policies,
limits, caching, static targets and exposition — is in the exporter
specification, and a section number here, such as § 42.14, is one of its
sections. What a user reads of the type is [POP3.md](POP3.md).

## `pop3`

A `pop3` collector holds a POP3 session (RFC 1939, RFC 2449, RFC 2595,
RFC 5034) with the probe's `target`, in which it asks for the mailbox's
size and reads no message, and hands what the server said to the
transforms as a JSON document. It MUST need nothing outside Go's standard
library, and MUST be opt-in: in no build that does not ask for it with
`request_type_pop3`.

| Key | Default | Rule |
| --- | --- | --- |
| `list` | `false` | It MUST need `username`. |

Of the common part it takes `username`,
`password_file`, `auth`, `xoauth2`, `allow_plaintext`, `tls`,
`max_response_bytes`, `allowed_targets` and `denied_targets`. `auth` MUST
be `user`, `apop`, `plain`, `login`, `cram-md5`, `xoauth2` or
`oauthbearer`; it MUST need `username`, each but `xoauth2` and
`oauthbearer` MUST need `password_file`, and those two the
`xoauth2` block and no `password_file`, the block held to the rules of the
[`smtp` type's](SPECIFICATION-SMTP.md) and a token got and kept as there.
`username` MUST hold no placeholder, and no blank with `apop`. A static
target's request block takes `timeout`, `username`, `password_file` and
`tls`.

- A target MUST be `host`, `host:port`, `pop3://host:port` or
  `pop3s://host:port`, port 110, and 995 for `pop3s://`; anything else, a
  path and a user MUST be refused, a probe's with `400` before anything is
  sent. `pop3s://` MUST be TLS from the first byte.
- The session MUST be: the greeting, which MUST be `+OK`; `CAPA`; on
  `pop3://`, where the server names `STLS`, `STLS`, the handshake with the
  `tls` block's settings, and `CAPA` again; with a `username`, the login,
  `STAT` and, with `list`, `LIST`; `QUIT`. `UIDL`, `RETR`, `TOP`, `DELE`
  and every other command MUST never be sent. A `CAPA` answered `-ERR` MUST NOT fail the
  scrape: the server names no capability.
- Bytes the server sent after its answer to `STLS` and before the
  handshake MUST fail the scrape, and MUST NOT be read as the server's.
- A credential, `apop`'s and `cram-md5`'s included, MUST NOT be sent over a
  connection that is not TLS unless `allow_plaintext` is set: the scrape
  MUST fail before the login.
- Without `auth`, the login MUST be `user` where the server names `USER` or
  answered `CAPA` with `-ERR`, else `plain` where its `SASL` names `PLAIN`,
  else `login` where it names `LOGIN`, else `user`; with an `xoauth2`
  block, `xoauth2`.
- `user` MUST be `USER` with the name and, answered `+OK`, `PASS` with the
  password. `apop` MUST be `APOP` with the name and the MD5 digest, in
  lower-case hexadecimal, of the greeting's last `<...>` followed by the
  password, and MUST fail the scrape, before anything is sent of the
  account, where the greeting has none.
- `plain`, `login`, `cram-md5`, `xoauth2` and `oauthbearer` MUST be `AUTH` with the
  mechanism's name alone, the credential sent in answer to the server's
  challenge: `PLAIN` as RFC 4616 has it with an empty authorization
  identity; `LOGIN` as the name and the password, each in answer to a
  challenge; `CRAM-MD5` as the name and the HMAC-MD5, in lower-case
  hexadecimal, of the challenge keyed by the password (RFC 2195);
  `XOAUTH2` as `user=<username>^Aauth=Bearer <token>^A^A`, a challenge in
  answer to which MUST be answered with an empty line; `OAUTHBEARER` as
  the message of RFC 7628, as `smtp` writes it, a challenge in answer to
  which MUST be answered with the one byte `^A`. A server that answers
  `AUTH XOAUTH2` or `AUTH OAUTHBEARER` with `-ERR`, having been sent no
  credential, MUST be asked once more with the credential in the command,
  which a debug report MUST show as `***`. A challenge once
  the exchange is over MUST be answered with `*`. One of these that the
  server answered `CAPA` without naming MUST fail the scrape before
  anything of the account is sent, saying what the server names.
- A name or a password with a control character MUST fail the scrape
  before anything of the account is sent.
- Any answer to the login but `+OK` MUST fail the scrape, naming the
  command, the account and the server's answer; so MUST a `STAT` answered
  `-ERR`, or with anything but two whole numbers, none negative.
- The text of an `-ERR` MUST be shown, in an error, the log and a debug
  report, with what reads as a credential masked, as the text of any
  server's refusal is: a server may say back the command it refuses.
- The document MUST hold `greeting`, the greeting's text; `tls` and
  `starttls`; `capabilities`, the first word of each line of the last
  `CAPA`, in upper case, sorted, each once, at most 100 bytes each; `sasl`,
  the mechanisms of its `SASL` line, likewise; `apop`, whether the greeting
  has a timestamp; `authenticated`; `auth`, the mechanism the login used;
  `messages` and `size_bytes`, the two numbers of `STAT`; and
  `largest_bytes`. `auth`, `messages` and `size_bytes` MUST be null
  without a login.
- With `list`, `LIST` MUST be sent after `STAT` and its lines read to the
  one that is a dot: `largest_bytes` MUST be the greatest of their sizes,
  and null of a mailbox without a message, as it MUST be without `list`. A
  `LIST` answered `-ERR`, and a line that is no message's number and a
  size that is none or negative, MUST fail the scrape.
- One line of a response MUST be at most 4,096 bytes, the answer to `CAPA`
  at most 200 lines, and the responses together the response limit (§ 20);
  more MUST fail the scrape, the last as a limit exceeded. A response that
  is no POP3 MUST fail the scrape saying so.
- A scrape MUST end at its deadline, and after 30 seconds without one. One
  connection MUST be made for a scrape, through the target policy
  (§ 42.14), and none kept.
- What the server said MUST NOT reach a failed probe's caller (§ 19.4), and
  a password, a digest made of one and a token MUST be in no error, log
  line or debug report: a traced line that carries one MUST show `***` in
  its place.
- The verbose self-metrics MUST label a scrape `pop3://host:port` or
  `pop3s://host:port`, and its `http_method` `STAT` with an account and
  `CAPA` without.
