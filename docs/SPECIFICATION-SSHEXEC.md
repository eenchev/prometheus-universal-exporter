# Prometheus Universal Exporter — `sshexec` Request Type Specification

This is the `sshexec` request type's section of § 5.1 of the
[exporter specification](SPECIFICATION-EXPORTER.md), in a document of its
own so that a change to the type is a change to this document. Everything
the request types share — the registry and the rules every type is held to
(§ 5.1), building without a type, decoding, transforms, error policies,
limits, caching, static targets and exposition — is in the exporter
specification, and a section number here, such as § 42.14, is one of its
sections. What a user reads of the type is [SSHEXEC.md](SSHEXEC.md).

## `sshexec`

An `sshexec` collector runs one command the configuration fixes on the
probe's `target`, a remote host, over SSH, and hands what the command wrote
to stdout, as it wrote it, to the decoder and the transform as an `http`
collector's body is handed to them. Its
connection MUST be `sshfile`'s, kept apart from `sshfile`'s connections.
Its keys:

| Key | Default | Rule |
| --- | --- | --- |
| `command` | none | **Required.** A list: the program, then its arguments. The program an absolute path or a bare name, with no `/` otherwise, no placeholder and no control character, not blanks alone. No word with a NUL. At most 64 KiB together. `{{param_name}}` placeholders in the arguments, without filters. |
| `success_exit_codes` | `[0]` | The exit statuses that are a success, each from 0 to 255; a list replaces `[0]`. |
| `username`, `private_key_file`, `passphrase_file`, `password_file`, `known_hosts_file`, `keepalive_interval`, `keepalive_count_max`, `idle_timeout`, `max_open_connections` | as `sshfile` | With `sshfile`'s rules. |
| `max_response_bytes`, `allowed_targets`, `denied_targets` | as `http` | |

- The keys the two types share MUST be keys of the request block's common
  part, with one description in the schema naming both types, and their
  errors MUST name the collector's type where `sshfile`'s name `sshfile`.
- A scrape MUST send the command as one line in which every word of the
  list is written in single quotes, a `'` in it as `'\''`, so that a POSIX
  shell, which sshd hands the line to, makes each the one word it is in the
  list, whatever it holds: a test MUST run the line through `/bin/sh` and
  find an argument with blanks, quotes, `$`, a backquote, a glob and a `;`
  unchanged.
- A placeholder of an argument MUST be filled from the probe's parameter, or
  its default, and its value MUST hold only ASCII letters, digits and `. _ -
  : / @ + = , %`, as `tcp`'s `send`; a value holding any other character
  MUST be refused with `400` before anything is sent, naming the argument,
  the parameter and the character, and a default holding one MUST be
  refused at load. A value that would begin its argument with `-`, nothing
  of the argument written before it, and a value with `..` as a path
  component — `..` itself, or one beginning with `../`, ending with `/..`
  or holding `/../` — MUST be refused alike, naming the argument, the
  parameter and why; a `-` after what the configuration wrote in the
  argument, and a `..` within a name, MUST be taken. An argument whose
  values make `..` a path component that no one of them is — `.` and `.`
  in two placeholders side by side, `.` beside a dot the configuration
  wrote — MUST be refused alike, and defaults that do at load; a `..` the
  configuration writes itself MUST be taken. `tcp`'s `send` MUST NOT
  take these rules. A probe parameter that no placeholder of an
  argument or of a label value names MUST be refused, the refusal naming
  `request.command`.
- A scrape MUST open one SSH session on the connection kept to the host,
  run the command in it with no environment, terminal or input, and read its
  stdout against the response limit as it arrives: once past the limit the
  scrape MUST fail as a limit exceeded, the command sent `KILL` and its
  session closed, and a session that has not ended two seconds later MUST
  retire its connection as one at a deadline does (below). Of stderr the first 2 KiB MUST be kept, the rest read and
  dropped. A session that cannot be opened because the connection was lost
  MUST be opened once more on a new connection; once the exec request has
  been written the command MUST NOT be sent again, even when the
  connection is lost before the host answers it, and the scrape MUST fail
  saying the command may have run. The client MUST send the end of stdin at
  once, so a command reading it reads nothing and does not wait.
- The exit status MUST decide: 0, or with `success_exit_codes` one it lists,
  is a success, whose stdout is the body; any other status, a signal, or a
  session ended without a status MUST fail the scrape, saying the host and
  the status (`exited with N`, and, with `success_exit_codes`, that it does
  not list it) or the signal (`was killed by signal NAME`), and the start of
  stderr, at most 1 KiB of it, with what reads as a credential masked as in
  any text the target wrote, or that it wrote nothing there. The start of
  stderr MUST be in the log, and with `--web.debug=full` in a debug probe's
  report and on the status page, alone: the `502` a probe is answered MUST
  say the host and the status or the signal, and that stderr is in the
  exporter's log, and MUST NOT hold any of stderr; below `--web.debug=full`
  the status page MUST show the error as the probe was answered it.
- At the scrape's deadline, 30 seconds without one, or when it is
  cancelled, the scrape MUST fail at once, send the command `KILL` and
  close its session. When the session has not ended two seconds later, as
  OpenSSH leaves a forced command's, which it refuses signals and keeps
  open while the command lives, the connection MUST be retired, not
  closed: no scrape MUST open a session on it any more, the scrapes whose
  sessions run on it MUST be let finish, and it MUST be closed when the
  last of those has ended; a warning MUST be logged once for each
  connection retired.
- The answer MUST be the body, with no status and no `Content-Type`; the
  decoder MUST be `decoder.type`, or the one the transform implies, or what
  the body reads as. A failed scrape MUST be reported in the `ssh` stage.
- The target MUST be `sshfile`'s, checked and held to the target lists as
  `sshfile`'s is, a refused host answered `403` and counted in
  `http_exporter_targets_refused_total`.
- A debug probe's request section MUST show `EXEC`, the host as
  `ssh://host:port`, the user, the command line as it was sent in an
  `Ssh-Command` header, and whether the connection was made for the probe
  or kept, and as its outcome the exit status or the signal, the bytes read
  and the start of stderr, masked, or the error; no credential. The verbose
  `url` label MUST be `ssh://host:port`, and `http_method` MUST be `EXEC`.
- `--dry-run` MUST read the collector's files, and a static target's own
  credential files, as `sshfile`'s.
- A static target MAY set `timeout`, `username`, `private_key_file`,
  `passphrase_file` and `password_file`, with `sshfile`'s rules, and nothing
  of the command; a probe MAY set `timeout` and `param_<name>`.
- The documentation MUST have a security section: a dedicated account, a
  key restricted in `authorized_keys`, a forced command, saying that the
  server then ignores the configured command and hands it to the forced
  command in `SSH_ORIGINAL_COMMAND`, with a wrapper that allows a short list
  of command lines, and that no shell metacharacter reaches the host
  unquoted; that the account needs a real login shell, since sshd runs
  every command, a forced one too, with `$SHELL -c`, and an account whose
  shell is `nologin` or `false` fails every scrape; that a placeholder's
  value may not begin its argument with `-` or have `..` as a path
  component, recommending `--` before the arguments placeholders fill in a
  command that takes options; that stderr is not answered to a probe's
  caller; that a forced command is not ended at the deadline, recommending
  `timeout` in the wrapper or the command; and it MUST say why `env` is not
  supported.
