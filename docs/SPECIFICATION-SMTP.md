# Prometheus Universal Exporter — `smtp` Request Type Specification

This is the `smtp` request type's section of § 5.1 of the
[exporter specification](SPECIFICATION-EXPORTER.md), in a document of its
own so that a change to the type is a change to this document. Everything
the request types share — the registry and the rules every type is held to
(§ 5.1), building without a type, decoding, transforms, error policies,
limits, caching, static targets and exposition — is in the exporter
specification, and a section number here, such as § 42.14, is one of its
sections. What a user reads of the type is [SMTP.md](SMTP.md).

## `smtp`

An `smtp` collector holds an SMTP session (RFC 5321) with the probe's
`target` up to, and never including, `DATA`, and hands what the server said
to the transforms as a JSON document. It MUST need nothing outside Go's
standard library, and MUST be opt-in: in no build that does not ask for it
with `request_type_smtp`.

| Key | Default | Rule |
| --- | --- | --- |
| `starttls_mode` | `auto` | `auto`, `required` or `never`; anything else MUST be refused at load. |
| `hello` | the connection's local address as an address literal | No blank, control character or placeholder. |
| `auth` | `plain` where offered, else `login` | `plain`, `login` or `xoauth2`. It MUST need `username`; `plain` and `login` MUST need `password_file`, and `xoauth2` the `xoauth2` block and no `password_file`. |
| `xoauth2` | — | `token_file`, or `token_url` (https, no credentials) with `refresh_token_file` and one of `client_id` and `client_id_file`, and optionally `client_secret_file` and `scopes`; both sources together MUST be refused. It implies `auth: xoauth2`. |
| `mail_from` | — | An address, `local@domain`, or `<>`; MAY hold placeholders. |
| `rcpt_to` | — | At most 20 addresses, never `<>`, only beside `mail_from`; each MAY hold placeholders. |

Of the common part it takes `username`, `password_file`, `tls`,
`max_response_bytes`, `allowed_targets` and `denied_targets`; a static
target's request block takes `timeout`, `username`, `password_file` and
`tls`.

- A target MUST be `host`, `host:port`, `smtp://host:port` or
  `smtps://host:port`, port 25, and 465 for `smtps://`; anything else, a
  path and a user MUST be refused, a probe's with `400` before anything is
  sent. `smtps://` MUST be TLS from the first byte, and MUST be refused for
  a collector whose `starttls_mode` is `required` or `never`.
- The session MUST be: the greeting, which MUST be a 220; `EHLO`; where
  the server offers `STARTTLS` and the mode is not `never`, `STARTTLS`, the
  handshake with the `tls` block's settings, and `EHLO` again, whose
  extensions replace the first's; `AUTH` when the collector has a
  `username`; `MAIL FROM` and, when it is accepted, each `RCPT TO`, then
  `RSET`; `QUIT`. `DATA` MUST never be sent. `required` MUST fail the
  scrape where `STARTTLS` is not offered.
- Bytes the server sent after its answer to `STARTTLS` and before the
  handshake MUST fail the scrape, and MUST NOT be read as the server's.
- A credential MUST NOT be sent over a connection that is not TLS unless
  the mode is `never`: the scrape MUST fail before `AUTH`.
- `PLAIN` MUST be sent as RFC 4616 has it with an empty authorization
  identity, `LOGIN` as the username and the password each in answer to a
  334, and `XOAUTH2` as `user=<username>^Aauth=Bearer <token>^A^A`; a 334
  in answer to `XOAUTH2` MUST be answered with an empty line. A mechanism
  the server does not offer, and any answer but 235, MUST fail the scrape,
  naming the mechanism, the account and the server's reply.
- An access token of the refresh token grant MUST be asked of `token_url`
  with `grant_type=refresh_token`, the files read then, and MUST be kept
  until a minute before `expires_in` and forgotten when the server refuses
  it; one of `token_file` MUST be read at every scrape.
- A placeholder's value written into an address MUST hold no blank, angle
  bracket or control character, and the address filled MUST be an address;
  otherwise the scrape MUST fail before anything is sent.
- A refused `MAIL FROM` and a refused `RCPT TO` are answers: they MUST be
  in the document, and MUST NOT fail the scrape.
- The answer MUST be a JSON document of `greeting` (`code`, `text`), `tls`,
  `starttls`, `extensions` (each keyword in upper case with its parameters
  as text, or `true`), `auth_mechanisms` sorted, `size` or `null`, `auth`
  (`mechanism`, `code`) or `null`, `mail_from` (`address`, `code`,
  `accepted`, `text`) or `null`, and `rcpt_to`, a list of the same. The
  decoder MUST be `json`. `$tls` MUST describe the session's certificate.
- One reply MUST be at most 64 KiB and the replies together the response
  limit, more failing as a limit exceeded. The deadline MUST bound the
  whole scrape, 30 seconds when none is set.
- A password, a token and the line that carries one MUST be in no error,
  log line, trace or debug report, which MUST show the command masked. The
  verbose `url` label MUST be `smtp://host:port` or `smtps://host:port`,
  and `http_method` `EHLO`, `AUTH`, `MAIL` or `RCPT`, the furthest the
  collector goes.
