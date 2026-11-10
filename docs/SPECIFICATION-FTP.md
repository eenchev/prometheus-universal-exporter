# Prometheus Universal Exporter — `ftp` Request Type Specification

This is the `ftp` request type's section of § 5.1 of the
[exporter specification](SPECIFICATION-EXPORTER.md), in a document of its
own so that a change to the type is a change to this document. Everything
the request types share — the registry and the rules every type is held to
(§ 5.1), building without a type, decoding, transforms, error policies,
limits, caching, static targets and exposition — is in the exporter
specification, and a section number here, such as § 42.14, is one of its
sections. What a user reads of the type is [FTP.md](FTP.md).

## `ftp`

An `ftp` collector reads one file from an FTP server (RFC 959) and hands
it to the decoder its extension names, as `localfile` and `sshfile` hand
theirs. It MUST need nothing outside Go's standard library, MUST be opt-in
(`request_type_ftp`), and MUST send no command but `AUTH`, `USER`, `PASS`,
`PBSZ`, `PROT`, `TYPE`, `SIZE`, `MDTM`, `EPSV`, `PASV`, `RETR` and `QUIT`.

It has no key of its own. Of the common part it takes `root` (required,
absolute, stored cleaned), `path` (relative, under `root`, placeholders
allowed), `max_age`, `max_response_bytes`, `username`, `password_file`
(only beside `username`), `tls`, `allowed_targets` and `denied_targets`. A
probe takes `path`, `timeout` and `param_<name>`; a static target's request
block takes `path`, `timeout`, `username`, `password_file` and `tls`.

- A target MUST be `host`, `host:port` or `ftp://host:port`, port 21, in
  plaintext; `ftpes://host:port`, explicit TLS, which a target without a
  scheme MUST be too for a collector with a `tls` block; or
  `ftps://host:port`, port 990, TLS from the first byte. A path, a user
  and another scheme MUST be refused, a probe's with `400` before anything
  is sent.
- The file MUST be named as a `localfile` collector's is (§ 5.1): a
  placeholder's value one path element, and neither the path nor the
  `path` parameter leading outside `root`. A path with a line break or a
  NUL MUST be refused before anything is sent.
- A scrape MUST: read the greeting, which MUST be a 220; for explicit TLS
  send `AUTH TLS` and make the handshake before the account is sent; log
  in with `USER` and, when asked, `PASS`, as `anonymous` when the collector
  has no `username`; under TLS send `PBSZ 0` and `PROT P`; send `TYPE I`;
  ask `SIZE` and `MDTM`, whose refusal is no failure; open a passive data
  connection; send `RETR`; read the file to the data connection's end; read
  the transfer's final reply, which MUST be a 226 or a 250; and send
  `QUIT`.
- The data connection MUST be passive, asked with `EPSV` and, where that
  is refused, `PASV`, and MUST be made to the address the control
  connection reached, whatever address the answer names, through the
  collector's target lists. Under TLS it MUST be TLS too, offering the
  control connection's session for resumption.
- A file whose `SIZE` or whose bytes read pass the response limit MUST
  fail the scrape as a limit exceeded, the first before a data connection
  is made. With `max_age`, a file `MDTM` says is older MUST fail the
  scrape, and so MUST a server that does not answer `MDTM`.
- The answer MUST be the file's bytes, with the `Content-Type` its
  extension names (§ 6), `Content-Length`, and `Last-Modified` when the
  server answered `MDTM`. `$tls` MUST describe the control connection's
  certificate.
- A failure MUST tell a probe's caller a reply's code and not its text,
  nor a line that is no reply, which are the answer's (§ 19.4).
- One reply MUST be at most 64 KiB, a longer one failing the scrape when
  that much has come. The deadline MUST bound the whole scrape, 30 seconds
  when none is set. The password MUST be in no error, log line, trace or debug report, which
  MUST show `PASS ***`. The verbose `url` label MUST be the file's URL,
  `ftp://`, `ftpes://` or `ftps://host:port/<root>/<path>`, with the
  path's placeholders as written, and `http_method` `RETR`.
