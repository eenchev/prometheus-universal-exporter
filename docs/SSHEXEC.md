# Commands over SSH

The `sshexec` request type runs one fixed command on a remote host over SSH
and hands what the command writes to its standard output to the decoder and
the transform, as an `http` collector's body is handed to them, and as a
[`tcp`](TCP.md) collector's answer is: raw, with nothing wrapped around it.
Use it where the numbers are what a command prints and a dedicated exporter
on the host is one more thing to install and run:

- `df -P` for the space left on a file system, `uptime` for the load;
- `systemctl is-active` or `systemctl show` for a unit's state and restarts;
- a script of the host's own that counts something, such as the lines of a
  log that match, with `grep -c`.

Whatever reads a body reads the output: `regex` for lines of text, `csv` or
`text` for columns, `prometheus` for a script that writes exposition, `jq`
for one that writes JSON, `yq`, `python`.

The connection is [`sshfile`](SSHFILE.md)'s: the same keys, the host's key
always verified against `known_hosts_file`, the same authentication, and
the same [persistent connections](SSHFILE.md#persistent-connections),
kept to a host and shared by the scrapes that reach it with the same
settings. Each scrape runs the command in a session of its own on that
connection. The host needs nothing but an SSH server and an account that
may run the command; read [Security](#security) before you give it one.

The type is opt-in: the default build and the published image leave it out,
so a build carries it only when asked
([Building with sshexec](#building-with-sshexec)). It needs Go's SSH client,
`golang.org/x/crypto/ssh`, and not the SFTP client `sshfile` links: a build
of `sshexec` alone, or of `sshfile` alone, carries only what it needs.

## Building with sshexec

`OPT_IN_REQUEST_TYPES` names the opt-in types a build adds to the types it
carries, comma-separated:

```sh
make build OPT_IN_REQUEST_TYPES=sshexec              # bin/prometheus-universal-exporter
make build OPT_IN_REQUEST_TYPES=sshexec,tcp          # more than one opt-in type
docker build --build-arg OPT_IN_REQUEST_TYPES=sshexec -t exporter:sshexec .
go build -tags "$(sh tools/request-type-tags.sh '' '' sshexec)" .
```

The tag is `request_type_sshexec`, which is all `go build -tags
request_type_sshexec .` needs by hand: every type of the default build, and
`sshexec`. `OPT_IN_REQUEST_TYPES` goes with `REQUEST_TYPES` (see [Choosing
request types at build time](CONFIGURATION.md#choosing-request-types-at-build-time)):
`REQUEST_TYPES=http OPT_IN_REQUEST_TYPES=sshexec` builds the two, and
`REQUEST_TYPES=sshexec` builds it alone.

A configuration with an `sshexec` collector is refused by a build without
the type, at startup, on a reload and by `--dry-run`, saying how to build
one that has it.

## A collector

```yaml
collectors:
  - name: disk
    request:
      type: sshexec
      command: [df, -P, -k, /]
      username: metrics
      private_key_file: /etc/exporter/ssh/id_ed25519
      known_hosts_file: /etc/exporter/ssh/known_hosts
    transform:
      type: regex
    metrics:
      - name: node_filesystem_used_kilobytes
        expression: '(?m)^\S+\s+\d+\s+(\d+)\s+\d+\s+\d+%\s+/$'
      - name: node_filesystem_available_kilobytes
        expression: '(?m)^\S+\s+\d+\s+\d+\s+(\d+)\s+\d+%\s+/$'
```

```text
/probe?collector=disk&target=db1.example.com
```

`df -P` writes a header and one line for the file system, in columns, so
each rule reads a column of the line mounted on `/`.

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `sshexec`. |
| `command` | — | **Required.** The command, as a list: the program and its arguments, `[df, -P, /]`. Never a line a shell reads; see [The command](#the-command). The program is an absolute path, `/usr/local/bin/check`, or a bare name the account's `PATH` finds, `df`. An argument may hold `{{param_name}}` placeholders; the program takes none. At most 64 KiB. |
| `success_exit_codes` | `[0]` | The exit statuses that are a success, such as `[0, 1]` for `grep`, whose 1 says it found nothing. Any other fails the scrape. Each from 0 to 255; the list replaces `[0]`, so name 0 in it when 0 is a success too. |
| `max_response_bytes` | limit | The most the command may write to stdout; see [Limits](#limits). |
| `username` | — | **Required.** The account the command runs as. Not blanks alone. |
| `private_key_file`, `passphrase_file`, `password_file` | — | The account's credentials, as [`sshfile`'s](SSHFILE.md#authentication): a private key, a password, or both, the key tried first. One of `private_key_file` and `password_file` is required. |
| `known_hosts_file` | — | **Required.** The known_hosts file the host's key is verified against, as [`sshfile`'s](SSHFILE.md#host-keys). There is no way to skip the check. |
| `keepalive_interval`, `keepalive_count_max`, `idle_timeout`, `max_open_connections` | `30s`, `3`, `5m`, `0` | The connection kept to a host, as [`sshfile`'s](SSHFILE.md#persistent-connections). |
| `allowed_targets`, `denied_targets` | — | The hosts the collector may and may not reach, as for [any type that reaches a host](REQUESTS.md#restricting-targets), checked on every connection. |

No other request key applies, and setting one — `method`, `headers`,
`root` — is a startup error. A key written `""` is the key left out. The
file keys are read when a connection is made, and by
[`--dry-run`](CONFIGURATION.md#dry-run), as `sshfile`'s are. The decoder is
`decoder.type`, or what the transform implies — `text` for `regex`, `csv`
for `csv`, `prometheus` for `prometheus` — or, for `jq`, `yq` and `python`
without one, what the output reads as, JSON when it is JSON: the output has
no `Content-Type` to go by, and no status (`$status` is `null`, Python's
`response.status_code` `None`).

## The target

The probe's `target` is the host, as an `sshfile` collector's is:
`db1.example.com`, `db1.example.com:2222` or `ssh://db1.example.com:2222`,
port 22 when it is left out, an IPv6 address in brackets. It is required,
and a probe without one is answered `400`. A target with a user, a path or
a query is refused with `400`: the account is `username`, and the command
`command`. The target is shown in logs and labels as it is written, since
it holds no credential.

## The command

The command is a list, and every word of it reaches the program as the one
word it is in the list. sshd runs a command by handing the command line to
the account's shell, `$SHELL -c '<line>'`, so the exporter writes the line
for a POSIX shell: every word in single quotes, a `'` in it written `'\''`.

```yaml
command: [printf, "%s\n", "it's $HOME", "*", "; reboot"]
```

is sent as

```text
'printf' '%s\n' 'it'\''s $HOME' '*' '; reboot'
```

and prints the three arguments as they are written: no variable is
expanded, no glob matched, no `;`, `|`, `&`, `>` or backquote read as the
shell's. There is no way to write a pipe, a redirection or a variable into
the command; write a script on the host that does what it must, and run
that. The quoting is that of a POSIX shell — `sh`, `bash`, `dash`, `ksh`,
`zsh`; an account whose shell is another, such as `fish` or `csh`, reads it
otherwise and should be given `/bin/sh`.

The program is an absolute path, or a bare name the account's `PATH` finds.
A relative path with a `/`, `bin/check` or `./check`, is refused when the
configuration loads, since it would be found from the account's home
directory, which the configuration does not see. The program takes no
placeholder, and neither does a static target change it: the configuration
alone says what runs.

An argument may hold [`{{param_name}}`](REQUESTS.md#path-parameters)
placeholders, filled from the probe's `param_<name>` parameters, with a
default as `{{param_unit:nginx.service}}`:

```yaml
command: [systemctl, is-active, "{{param_unit:nginx.service}}"]
```

```text
/probe?collector=unit&target=web1.example.com&param_unit=postgresql.service
```

A placeholder's value, a probe's or a default, may hold only letters,
digits and `. _ - : / @ + = , %`, the characters [`tcp`'s
`send`](TCP.md#what-a-scrape-sends) takes. Anything else — a blank, a `;`,
a quote, a `$`, a line break — is refused before anything is sent, a
probe's value with `400` naming the argument and the character, a default
when the configuration loads:

```text
request.command[2]: the value of param_unit holds ';' (U+003B); a value written into an argument of request.command may hold only letters, digits and . _ - : / @ + = , %, so that it stays the one argument it is written in, whatever splits the command line on the host
```

The quoting alone keeps a value one word for the shell. The narrow set is
there for what comes after it: the program, which may read a value as an
option or a list, and a [forced command](#security)'s wrapper, which may
split the command line again on blanks.

Two values of those characters are refused besides, a probe's with `400`
naming the argument, the parameter and why, a default when the
configuration loads:

- **A value that begins its argument with `-`**, which the program would
  read as an option, whatever the placeholder was meant to name: with
  `command: [wc, -l, "{{param_file}}"]`, `param_file=--files0-from=/etc/passwd`
  would have `wc` read the names of the files to count from `/etc/passwd`,
  and complain of each line of it. A `-` after what the configuration
  wrote is part of the value: `--unit={{param_unit}}` takes `-x`.
- **A value with `..` as a path component** — `..` itself, or one that
  begins with `../`, ends with `/..` or holds `/../` — which would name a
  file outside the directory the argument is written under, as
  `/var/log/app/{{param_file}}` is. A `..` inside a name, `a..b`, is no
  component.

```text
request.command[2]: the value of param_file begins with -, and begins the argument, which the program would read as an option; a value that begins an argument of request.command may not begin with -
```

Where the program takes options, write `--` before the arguments a
placeholder fills, as the end of its options: `[wc, -l, --,
"{{param_file}}"]`. The refusal stays, and a program that reads `--` so is
safe from an option however its arguments are written.

No environment variable is sent. OpenSSH accepts only the variables its
`AcceptEnv` names, none by default, so a request to set one would fail on
some hosts and be ignored on others; a script that needs a variable sets it
itself.

## Exit status

The command's exit status decides whether the scrape succeeded: 0 does,
and every other fails it, unless `success_exit_codes` lists it, which
replaces 0. `grep -c` exits with 1 when it counts nothing, which is an
answer, not a failure:

```yaml
command: [grep, -c, ERROR, /var/log/app/current.log]
success_exit_codes: [0, 1]
```

A failure says the status, and the start of what the command wrote to
stderr, which the exporter keeps the first 2 KiB of and shows the first
1 KiB of, with what reads as a credential in it masked as in any text the
exporter did not write:

```text
the command on db1.example.com:22 exited with 2; its stderr began: grep: /var/log/app/current.log: No such file or directory
```

That text is the operator's: the exporter's log shows it, and with
`--web.debug=full` so do a [debug probe](#errors-and-self-metrics)'s report
and the [status page](ENDPOINTS.md#status-page). The caller of `/probe` is
answered the status alone, and that stderr is in the log, since stderr may
quote what a file on the host holds; below `--web.debug=full` the status
page, which whoever may call `/probe` can read, shows the same:

```text
collector disk ssh failed: the command on db1.example.com:22 exited with 2; what it wrote to stderr is in the exporter's log
```

A command ended by a signal says which: `the command on db1.example.com:22
was killed by signal TERM; ...`. One whose session ends without an exit
status fails saying so. The output of a failed command is not decoded.

## Examples

The load average, from `uptime`, whose line ends with three numbers:

```yaml
  - name: load
    request:
      type: sshexec
      command: [uptime]
      username: metrics
      private_key_file: /etc/exporter/ssh/id_ed25519
      known_hosts_file: /etc/exporter/ssh/known_hosts
    transform:
      type: regex
    metrics:
      - name: node_load1
        expression: 'load averages?: ([\d.]+),? [\d.]+,? [\d.]+'
      - name: node_load5
        expression: 'load averages?: [\d.]+,? ([\d.]+),? [\d.]+'
      - name: node_load15
        expression: 'load averages?: [\d.]+,? [\d.]+,? ([\d.]+)'
```

A unit's state, from `systemctl is-active`, which prints `active` and exits
with 0 for a running unit, and prints its state and exits with 3 otherwise;
both are an answer, and a `value_map` makes the state a number:

```yaml
  - name: unit
    request:
      type: sshexec
      command: [systemctl, is-active, "{{param_unit:nginx.service}}"]
      success_exit_codes: [0, 3]
      username: metrics
      private_key_file: /etc/exporter/ssh/id_ed25519
      known_hosts_file: /etc/exporter/ssh/known_hosts
    transform:
      type: regex
      labels:
        unit: "{{param_unit:nginx.service}}"
    metrics:
      - name: systemd_unit_active
        type: gauge
        expression: '^(\w+)'
        value_map: {active: 1, "*": 0}
```

A script of the host's own, which counts the failed logins of the last hour
and writes them as exposition, so the `prometheus` transform reads it as
it is:

```sh
#!/bin/sh
# /usr/local/bin/count-failed-logins
set -eu
n=$(journalctl -u ssh --since -1h --no-pager -q | grep -c 'Failed password' || true)
printf '# TYPE ssh_failed_logins gauge\nssh_failed_logins %s\n' "$n"
```

```yaml
  - name: failed_logins
    request:
      type: sshexec
      command: [/usr/local/bin/count-failed-logins]
      username: metrics
      private_key_file: /etc/exporter/ssh/id_ed25519
      known_hosts_file: /etc/exporter/ssh/known_hosts
    transform:
      type: prometheus
```

The script counts with `grep -c`, whose exit status of 1 for no match `||
true` keeps from failing the script; a collector that ran `grep -c` itself
would list 1 in `success_exit_codes`. `examples/sshexec` has these
collectors, with a static target file, ready for `make run
EXAMPLE=sshexec`.

## Security

A command run over SSH runs with the account's rights, and the account's key
is in the exporter's hands. Give the exporter as little as the commands
need:

- **A dedicated account**, used by the exporter alone, that owns nothing
  and may read only what the commands read: not `root`, not a person's
  account, not one in `sudo` or `wheel`. Give it a real login shell,
  `/bin/sh`: sshd runs every command with the account's shell, `$SHELL -c`,
  a forced command too, so an account whose shell is `/usr/sbin/nologin` or
  `/bin/false` runs nothing, and every scrape fails with `This account is
  currently not available`. What keeps the account from a shell of its own
  is the key's `restrict` and forced command below, not its shell.
- **A key of its own**, made for the exporter, with no passphrase or with
  one in `passphrase_file`, and restricted in the account's
  `~/.ssh/authorized_keys` with `restrict`, which turns off forwarding, a
  terminal and `~/.ssh/rc`, and a `from=` naming the exporter's addresses:

  ```text
  restrict,from="10.0.8.0/24" ssh-ed25519 AAAA... prometheus-exporter
  ```

- **A forced command.** `command="..."` on the key's line makes sshd run
  that command, whatever the client asked: the exporter's `command` is then
  **ignored by the server**, and only handed to the forced command in the
  variable `SSH_ORIGINAL_COMMAND`, the command line as the exporter sent it,
  each word in single quotes. A key with a forced command can run that
  command and nothing else, even when the key is stolen:

  ```text
  restrict,command="/usr/bin/uptime" ssh-ed25519 AAAA... prometheus-exporter
  ```

  For a key that runs more than one command, a small wrapper allows the
  command lines the collectors send, compared whole, and refuses the rest.
  The line to compare is the one a [debug probe](#errors-and-self-metrics)
  shows as `Ssh-Command`:

  ```sh
  #!/bin/sh
  # /usr/local/bin/exporter-commands, the forced command of the exporter's key
  case "$SSH_ORIGINAL_COMMAND" in
    "'df' '-P' '-k' '/'") exec df -P -k / ;;
    "'uptime'")           exec uptime ;;
    "'systemctl' 'is-active' 'nginx.service'")      exec systemctl is-active nginx.service ;;
    "'systemctl' 'is-active' 'postgresql.service'") exec systemctl is-active postgresql.service ;;
    *) echo "refused: $SSH_ORIGINAL_COMMAND" >&2; exit 126 ;;
  esac
  ```

  ```text
  restrict,command="/usr/local/bin/exporter-commands" ssh-ed25519 AAAA... prometheus-exporter
  ```

  The wrapper runs what it allows by its own words, never by `eval` or `sh
  -c "$SSH_ORIGINAL_COMMAND"`, which would run whatever the client sent.
  A placeholder's values must then be listed, as the units are above.
- **No shell metacharacter reaches the host unquoted.** The command is a
  list the exporter quotes word by word ([The command](#the-command)), the
  program and its arguments are the configuration's, and a probe changes
  only what a placeholder stands for, held to the characters of a name.
  Nothing a probe or a static target sends can add a command, a pipe or a
  redirection. Who may change the configuration may run any command the
  account may; guard it as you guard the account.
- **No option and no `..` from a probe.** A value that would begin its
  argument with `-`, an option to the program, or that has `..` as a path
  component is refused with `400` ([The command](#the-command)). Write
  `--` before the arguments a placeholder fills in a command that takes
  options, `[wc, -l, --, "{{param_file}}"]`, so that its program reads no
  option there whatever the rules let through.
- **stderr stays with the operator.** A failed command's stderr is in the
  exporter's log, and with `--web.debug=full` in a debug probe's report and
  on the status page, not in what `/probe` answers ([Exit status](#exit-status)): a command that complains
  of a file quotes it, and the caller of a probe may be anyone the probe
  endpoint answers.
- **Opt-in.** The type is in no build that does not ask for it, so an
  exporter built without it cannot be configured to run commands anywhere.

The host's key is always verified against `known_hosts_file`, so the
account's credentials are given to no host that only claims to be the one
scraped (see [Host keys](SSHFILE.md#host-keys)).

## Static targets

A [static target](STATIC-TARGETS.md) of an `sshexec` collector names the
host as its `target`, which it requires, and may set under its `request`
`timeout` and its own account and credentials: `username`,
`private_key_file`, `passphrase_file` and `password_file`, as an `sshfile`
target's (see [Static targets](SSHFILE.md#static-targets)): a target that
sets any of the three credential files replaces all three of the
collector's with its own. Nothing of the command: `command` and
`success_exit_codes` are the collector's, `known_hosts_file` and the
connection settings too, and `params` fills its placeholders.

```yaml
interval: 1m
targets:
  - name: db1_disk
    collector: disk
    target: db1.example.com
  - name: legacy_disk
    collector: disk
    target: legacy.example.com:2222
    request:
      username: monitor
      password_file: /etc/exporter/ssh/legacy-password
```

## Limits

The command's stdout is read against `max_response_bytes`, or
`limits.max_response_bytes`, 10 MiB when neither is set, as it arrives: a
command that writes past it fails the scrape when it does, as a limit
exceeded, which counts in `http_exporter_series_limit_exceeded_total`, and
is sent `KILL` and its session closed rather than read to its end. Of
stderr the first 2 KiB are kept and the rest read and dropped, so a command
that writes much there is not held up.

Making the connection, opening the session and running the command end at
the probe's deadline, its `timeout` or the scrape's, 30 seconds without
one. At the deadline the scrape fails at once, and the command is sent
`KILL` and its session closed. For a command the client asked for,
OpenSSH 8.1 and later passes the signal on, and the command ends.

**A forced command is not ended**, nor any command on an sshd older than
8.1. OpenSSH refuses a signal to a session whose command is forced
(`command="..."` in `authorized_keys`, or `ForceCommand`), and keeps a
session open, its close unanswered, for as long as its command lives. The exporter gives such a session two seconds,
and then retires its connection: no scrape opens a session on it any
more, the next scrapes connect again, and the scrapes whose commands run
on it meanwhile finish, each by its own deadline and two seconds; the
connection is closed when the last of them has ended. The exporter logs a
warning once for each connection it retires. The command itself runs on,
on the host, until it ends, so bound it there: run it under `timeout`, in
the forced command's wrapper, `exec timeout 20 df -P -k /`, or in the
collector's command, `[timeout, 20, df, -P, -k, /]`, with a bound below
the probe's deadline.

A connection lost after the command was sent is not tried again: the host
may have run the command, which then would run twice. The scrape fails
saying so, and the next scrape connects again. Only opening the session,
before the command is sent, is tried again on a new connection when the
kept one is found lost.

Each scrape opens one session on the kept connection, so scrapes that
overlap, of one collector or of several sharing a connection, each hold a
session while their command runs. OpenSSH's `MaxSessions`, 10 by default,
bounds them; a host that refuses a session fails the scrape saying so.

## Errors and self-metrics

A failed scrape is reported in the `ssh` stage and follows
`error_handling.on_fetch_error`, naming the host: making the connection
fails as an `sshfile` connection does (see [Troubleshooting](SSHFILE.md#troubleshooting)),
and the command as [Exit status](#exit-status) says. A host the target
lists refuse counts in `http_exporter_targets_refused_total`, answered
`403`, and a value of a placeholder that is refused is answered `400`.

The connections kept are counted as `sshfile`'s are, in families of the
type's own: `http_exporter_sshexec_connections`, the connections a
collector's scrapes use that are open now,
`http_exporter_sshexec_connects_total`, those they have made, and
`http_exporter_sshexec_limit_waits_total` and
`http_exporter_sshexec_limit_failures_total`, the scrapes that waited for
room at `max_open_connections` and those that failed there (see
[Self-metrics](SELF-METRICS.md)).

A [debug probe](CONFIGURATION.md#debug-probes) shows the command in its
request section — the host as an `ssh://` URL, the account, the command
line as it was sent, quoted, and whether the connection was made for the
probe or kept — and how it ended: its exit status, the bytes read, and the
start of stderr; its response section shows the output. Never a
credential:

```text
Requests
  1. EXEC ssh://db1.example.com:22 -> exited with 0, read 112 bytes in 38ms
     Ssh-Command: 'df' '-P' '-k' '/'
     Ssh-Connection: reused
     Ssh-User: metrics
```

With [verbose self-metrics](SELF-METRICS.md#verbose-per-request-self-metrics)
a scrape is labelled `url="ssh://host:port"` and `http_method="EXEC"`.

## Limitations

- **One command, no shell.** The command is a program and its arguments; a
  pipe, a redirection, a variable or `&&` is a script's to do on the host.
- **No environment.** `env` is not a key: servers refuse to set most
  variables (OpenSSH's `AcceptEnv`), and a script sets what it needs.
- **No terminal, no input.** The command runs without a terminal, and its
  stdin is at its end at once: a command that reads it, as `cat` with no
  file does, reads nothing and goes on, and one that asks a question reads
  no answer.
- **A POSIX shell.** The quoting is a POSIX shell's; an account whose shell
  reads quotes otherwise, `fish` or `csh`, runs the command differently.
- **Keyboard-interactive is answered with the password alone**, as for
  `sshfile`, so a host that asks for a one-time code cannot be scraped.

## Troubleshooting

| The scrape says | Do |
| --- | --- |
| `... uses request.type "sshexec", which this build of the exporter does not include` | Build with `OPT_IN_REQUEST_TYPES=sshexec` (see [Building with sshexec](#building-with-sshexec)); the published image does not carry it. |
| `the command on ... exited with 127; its stderr began: sh: 1: ...: not found` | The program is not on the account's `PATH`: write its absolute path. |
| `the command on ... exited with 126` | The program is not executable by the account, or a forced command's wrapper refused the line: compare the debug probe's `Ssh-Command` with the wrapper's list. |
| `the command on ... exited with 1, and wrote nothing to stderr` | A command whose 1 is an answer, as `grep`'s is: list it in `success_exit_codes`. |
| `request.command[N]: the value of param_<name> holds ...` | The probe's parameter: a value may hold only a name's characters. |
| `request.command[N]: the value of param_<name> begins with -, ...` | A value may not begin its argument with `-`, which the program would read as an option. |
| `request.command[N]: the value of param_<name> has .. as a path component ...` | A value may not climb out of the directory its argument names. |
| `the command on ... exited with 1; what it wrote to stderr is in the exporter's log, ...` | The probe's answer leaves stderr out: read the exporter's log, a debug probe's report or the status page. |
| `the command on ... exited with 1; its stderr began: This account is currently not available.` | The account's shell is `nologin` or `false`; sshd runs every command with it. Give the account `/bin/sh` (see [Security](#security)). |
| `the connection was lost once the command was sent, so it is not sent again; it may have run` | The connection broke between the command and its answer; the next scrape connects again. A command that must not run twice cannot be told apart from one that did not run. |
| `request.command[0] "..." is a relative path` | Write the program as an absolute path or a bare name. |
| `... the command's output is larger than the response limit ...` | Raise `max_response_bytes`, or have the command write less. |
| `running the command on ...: context deadline exceeded` | The command took longer than the probe's deadline: raise its `timeout`, or make the command faster. Under a forced command, bound it on the host with `timeout` (see [Limits](#limits)). |
| `opening a session: ...` | The host refused a session on the connection: its `MaxSessions`, or an account that may not run commands. |
| `... is not in request.known_hosts_file`, `refused user ...` | The host key or the authentication, as for [`sshfile`](SSHFILE.md#troubleshooting). |
