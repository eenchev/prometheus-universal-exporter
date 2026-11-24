# Files over SSH

The `sshfile` request type is [`localfile`](LOCALFILE.md) for another
machine. It reads a file, or a directory of files, from a remote host over
SFTP, and decodes, transforms and checks it exactly as `localfile` does with
a file of the exporter's own. Use it where the files are written on a host
that the exporter cannot mount:

- the `.prom` files a cron job leaves for node_exporter's textfile collector,
  on a host where you would rather not run node_exporter, or not open its port;
- the JSON or XML status file an appliance writes and offers over SFTP alone;
- the output of a batch job on a host that is reachable only over SSH.

The host needs nothing but an SSH server with its SFTP subsystem, which
OpenSSH enables by default, and an account that can read the files. To run
a command on the host and read its output instead, use
[`sshexec`](SSHEXEC.md), which connects as `sshfile` does, with the same
keys.

## A collector

```yaml
collectors:
  - name: backups
    request:
      type: sshfile
      root: /var/lib/node_exporter/textfile_collector
      path: backup.prom
      username: metrics
      private_key_file: /etc/exporter/ssh/id_ed25519
      known_hosts_file: /etc/exporter/ssh/known_hosts
      max_age: 26h
    transform:
      type: prometheus
```

```text
/probe?collector=backups&target=db1.example.com
```

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `sshfile`. |
| `root` | — | **Required.** The absolute directory on the host the collector may read under, with `/` between its names and no backslash. Nothing outside it can be read. `/` is taken, for an account confined to its own tree (a chroot), whose `/` is all it sees. |
| `path` | — | The file, relative to `root`, with `/` between its names and no backslash. May use [path parameters](REQUESTS.md#path-parameters). |
| `files` | — | Read a whole directory instead of one file: the [file name patterns](#reading-a-directory) to read. Not with `path`. |
| `max_files` | `100` | With `files`: the most files one scrape reads. |
| `max_total_bytes` | `64MiB` | With `files`: the most one scrape reads across every file. |
| `max_age` | off | Refuse a file last modified longer ago than this. |
| `max_response_bytes` | limit | The most that is read of one file; a larger file fails the scrape. |
| `username` | — | **Required.** The account the file is read as. Not blanks alone. |
| `private_key_file` | — | The account's private key, tried first. |
| `passphrase_file` | — | The passphrase of an encrypted `private_key_file`; only with it. |
| `password_file` | — | The account's password, tried after the key. One of `private_key_file` and `password_file` is required; both may be set. |
| `known_hosts_file` | — | **Required.** The known_hosts file the host's key is verified against. There is no way to skip the check. |
| `keepalive_interval` | `30s` | How often a keepalive is sent on the connection kept to a host, and how long each is given to be answered. At least `1s`; `0` sends none. |
| `keepalive_count_max` | `3` | How many keepalives in a row may go unanswered before the connection is closed. At least 1. |
| `idle_timeout` | `5m` | How long a connection nothing uses is kept; `0` keeps it as long as the collector is configured. |
| `max_open_connections` | `0` | The most connections the collector holds open at once, across its hosts; `0` sets no limit. See [Persistent connections](#persistent-connections). |
| `allowed_targets`, `denied_targets` | — | The hosts the collector may and may not reach, as for [any type that reaches a host](REQUESTS.md#restricting-targets), checked on every connection. |

No other request key applies, and setting one (`method`, `headers`, a
`bearer_token`) is a startup error.

- The file keys name files on the exporter's own filesystem, such as a
  mounted Secret, and take no placeholders. `username` and the file keys are
  refused when written with only blanks (`""` is the same as leaving the key
  out).
- The files are read when a connection is made, not when the configuration
  loads, as the other types read their credential files at a request. A key
  or a `known_hosts` file that is missing or unreadable fails the scrape,
  naming the file, and a Secret mounted after the exporter starts works.
- [`--dry-run`](CONFIGURATION.md#dry-run) does read them, as a connection
  would, and warns, without failing the check, about each one that a scrape
  would fail on now: a file missing or unreadable, a key that is not a key
  or is encrypted without its passphrase, an empty password, or a
  `known_hosts` file without a line or with a line that is not valid. It
  checks a static target's own credential files too. Startup and reloads do
  not read them.

## Which file is read

The probe's `target` is the host: `db1.example.com`, `db1.example.com:2222`
or `ssh://db1.example.com:2222`, port 22 when it is left out, an IPv6 address
in brackets. It is required, and a probe without one is answered `400`. A
target with a user, a path or a query is refused with `400`: the account is
`username`, and the file `root` / `path`.

| Probe | File read |
| --- | --- |
| `/probe?collector=backups&target=db1.example.com` | `backup.prom` in `root` on db1 |
| `/probe?collector=backups&target=db1.example.com&path=rotate.prom` | `rotate.prom` in `root` on db1 |

The `/probe` parameters that apply are `path`, `timeout` and `param_<name>`.
A `path` or path parameter follows `localfile`'s rules: it is relative to
`root`, and a path parameter is a single file or directory name.

A backslash is refused in `path`, in the `path` parameter (`400`, naming it)
and in a static target's `path`. Names are checked with `/` as the only
separator, but a Windows SFTP server would take a backslash as a separator
too, so `..\..\x` would leave `root` there.

A placeholder lets one collector read one file of several:

```yaml
  - name: job_status
    request:
      type: sshfile
      root: /srv/jobs/status
      path: "{{param_job}}.json"
      username: metrics
      private_key_file: /etc/exporter/ssh/id_ed25519
      known_hosts_file: /etc/exporter/ssh/known_hosts
    transform:
      type: jq
    metrics:
      - name: job_last_success_timestamp_seconds
        type: gauge
        expression: .last_success
```

```text
/probe?collector=job_status&target=batch1.example.com&param_job=nightly
```

**Confined to `root`.** A read cannot leave `root`, whatever the probe asks
for.

- `..` is refused before anything is sent.
- Every symbolic link on the way to the file is followed by the exporter, on
  the host, before the file is opened, and a path that leads outside `root`
  is refused (`file /srv/jobs/status/x.json leads outside request.root
  through a symbolic link`). A link that stays inside `root` is followed,
  whether absolute or relative; a link whose target has a backslash is not.
- A backslash is refused in `root` too.

The check and the open are two separate steps, though (see
[Limitations](#limitations)): read as an account that can read nothing else
it should not, not as `root` or an administrator.

## Authentication

The account authenticates with a private key, a password, or both, the key
tried first:

- **A private key**, `private_key_file`, in OpenSSH's format as `ssh-keygen`
  writes it, encrypted or not, or PKCS#1 or SEC 1 PEM, encrypted or not, or
  unencrypted PKCS#8 PEM. An encrypted key needs `passphrase_file`, the file
  of its passphrase. An encrypted PKCS#8 key (`BEGIN ENCRYPTED PRIVATE KEY`,
  as `openssl genpkey` with a cipher writes it) cannot be read: convert it to
  OpenSSH's format with `ssh-keygen -p -f <key>`, which asks for its
  passphrase and a new one. Make a key for the exporter alone, and allow it
  only what it needs on the host:

  ```sh
  ssh-keygen -t ed25519 -N '' -C prometheus-exporter -f id_ed25519
  ```

  In the host account's `~/.ssh/authorized_keys`, restrict the key to SFTP
  with `restrict`, and point OpenSSH's `ForceCommand internal-sftp` at it in
  a `Match User` block for an account that is to do nothing else.
- **A password**, `password_file`. Its surrounding whitespace, including a
  trailing newline, is left out, as for the other types' credential files.
  - It is offered as SSH's `password` method or, to a host that does not
    take that method, as the answer to `keyboard-interactive` questions,
    which a host that leaves passwords to PAM asks
    (`PasswordAuthentication no`, `KbdInteractiveAuthentication yes`).
  - A host that takes both, as OpenSSH with PAM does by default, is given the
    password once, by the `password` method. A wrong password therefore
    counts once against its `MaxAuthTries`, a PAM lockout or fail2ban, not
    twice.
  - Every question that the host does not echo is answered with the
    password, and one that it echoes with `username`, in at most three
    rounds of questions. A round without a question is not counted.
  - A one-time code is also a question that the host does not echo, so a
    host that asks for one (two-factor authentication) is given the password
    instead and cannot be scraped.

The files are read when a connection is made, so a rotated key or password
is used by the next connection. A connection is made again at the next
scrape when one of its files has changed on disk; one still being made then
is given up once no scrape waits for it.

A failed authentication says what was tried, the files and the SSH methods:
`the host db1.example.com:22 refused user "metrics": authentication failed
with the private key in /etc/exporter/ssh/id_ed25519 (the SSH methods tried:
publickey); check request.username and the credential files`. When
`keyboard-interactive` was among them, it adds that the host's questions
must ask for the password alone. A host that asked more than three rounds
of them is refused, saying so.

## Host keys

The host's key is always verified against `known_hosts_file`. There is no
option to skip it, because a scrape that trusts any key would hand the
account's credentials to whoever answers in the host's place. The file holds
lines as OpenSSH's `known_hosts` does: plain or hashed names, and
`[host]:port` for a port other than 22. The target is looked up in it as it
is written, in lower case: a host scraped as `db1.example.com` needs a line
for `db1.example.com`, not only for its address.

To get a host's line, ask it with `ssh-keyscan`, and check the key's
fingerprint against the host's own before you trust it — over its console, or
a channel you already trust:

```sh
ssh-keyscan -p 22 -t ed25519 db1.example.com >> known_hosts
ssh-keygen -lf known_hosts                       # the fingerprints it holds
ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub # on the host itself
```

A host offers several keys (ECDSA, Ed25519 and RSA), and an SSH client asks
for one by its own order of preference. The exporter asks:

- first for the types that the file holds for the host, so a file with only
  the host's Ed25519 line works against a host that would otherwise have
  presented its ECDSA key;
- then for every other type. A host that no longer offers the type of its
  line, because its key was replaced by one of another type, then presents
  the key that it has. It is refused as presenting a key that is not the one
  the file holds, with that key's fingerprint, instead of with SSH's
  `no common algorithm for host key`.

A host certificate is accepted when the file trusts its authority with an
`@cert-authority` line for the host, such as
`@cert-authority *.example.com ssh-ed25519 AAAA...`. For such a host, the
certificate types are asked for first.

- A line names a host on a port other than 22 as `[host]:port`, so write a
  pattern for several hosts on such a port as `[*.example.com]:2222` or
  `[*]:2222`. A line naming every host with a bare `*` trusts its authority
  on every port, as OpenSSH's does.
- Such a line makes the certificate types come first only for a host that
  the file has no line of. A host that the file has a line of is asked for
  that line's key type first, as OpenSSH asks, so a host that also presents
  a certificate of another authority is accepted by its line.
- A host whose certificate is signed by an authority that the file does not
  trust for it is still accepted when the file holds the line of the plain
  key that the certificate is for, as OpenSSH accepts it ("no matching CA
  found, retry with plain key").
- `@revoked` lines are honoured, both for a host's key and for an
  authority's.

| What went wrong | What the scrape says |
| --- | --- |
| The file has no line for the host | ``the host db1.example.com:22 is not in request.known_hosts_file ...; add its line, which `ssh-keyscan -p 22 db1.example.com` prints, after checking that the key's fingerprint, ssh-ed25519 SHA256:..., is the host's own (ssh-keygen -lf on the host's /etc/ssh/ssh_host_*_key.pub)`` |
| The host presented another key | `the host db1.example.com:22 presented the key ..., which is not the one request.known_hosts_file ... holds for it; either the host's key changed and its line must be replaced, or something stands between the exporter and the host` |
| The host presented a certificate of an authority not trusted for it | `the host db1.example.com:2222 presented a host certificate, ..., signed by the authority ssh-ed25519 SHA256:..., which no @cert-authority line of request.known_hosts_file ... trusts for the host, and no line of the file holds the key it certifies; add an @cert-authority line of the authority that signs the host's certificates, naming the host as it is scraped, [host]:port or a pattern such as [*]:port for a port other than 22, or the host's own line`; when the host's line holds another key, `... trusts for the host, of the key ssh-ed25519 SHA256:..., which is not the one the file holds for the host; add an @cert-authority line ..., or, when the host's key changed, replace its line`; when the file marks the key it certifies `@revoked`, `... trusts for the host, of the key ssh-ed25519 SHA256:..., which the file marks @revoked` |
| The key, or the certificate's authority, is `@revoked` | `the host db1.example.com:22 presented the key ..., which request.known_hosts_file ... marks @revoked`, or `... presented a host certificate, ..., signed by the authority ..., which request.known_hosts_file ... marks @revoked` |
| The file holds no line at all | `request.known_hosts_file ... holds no host key` |
| A line is not a known_hosts line | `request.known_hosts_file ... cannot be read as known_hosts lines: ...` |

## Persistent connections

A connection to a host is kept, with its SFTP session, and shared by every
scrape that reaches the host with the same account, credential files,
known_hosts file, keepalive settings and target policy, whether of one
collector or of several. It is made at the first scrape that needs it, so
the handshake and the authentication are paid once instead of at every
scrape.

The scrapes that reach it while it is being made wait for it, each within
its own deadline. When the first gives up, the others still get it. When
every one has given up, it is still made, given the larger of the first
scrape's deadline and 30 seconds, and kept for the next scrape.

- **Keepalives.** A connection can die without a word, when a host loses
  power or a firewall drops its state. Every `keepalive_interval` the exporter
  sends a `keepalive@openssh.com` request, which any SSH server answers; when
  `keepalive_count_max` in a row go unanswered within an interval, the
  connection is closed, and the next scrape makes a new one.
- **Idle connections** are closed after `idle_timeout`, 5 minutes by default.
  With `idle_timeout: 0` a connection stays as long as a collector of the
  configuration uses it; keepalives still close one that has died.
- **A broken connection** is noticed by the next scrape, which connects
  again, once, within its own deadline. So is one that the host closed
  during a read. A directory listing's own session (below) that the host's
  SFTP server ends while the connection stays up is not a broken connection.
  The exporter checks the connection with a `keepalive@openssh.com` request,
  waiting at most 2 seconds for its answer. When it answers, the read fails,
  saying that the SFTP server ended the listing's session; the connection is
  kept, and the listing is not tried again.
- **Reloads.** A reload that removes a collector, or changes its account,
  credential files, known_hosts file, keepalive settings or target lists,
  closes the connections that only that collector used, once no read holds
  them. It gives up the connections being made for it that no scrape waits
  for, or does so once the last scrape waiting for one stops waiting.
- **Shutdown** closes every connection, and gives up those being made, a
  scrape waiting for one or not.
- **A limit.** With `max_open_connections`, a collector holds at most that
  many connections open at once, across its hosts. This suits a collector
  that reads many hosts, against the hosts' `MaxStartups` or the exporter's
  file descriptors. Its connections are then its own, shared with no other
  collector, so the limit counts only them, including those being made. A
  scrape that needs a new connection at the limit:
  1. closes the collector's connection that no read holds and that was used
     longest ago;
  2. when there is none, gives up a connection being made that no scrape
     waits for any more (one to a host that took the TCP connection and
     never answered, whose scrape gave up), and waits for its making to end;
  3. when reads hold, or scrapes wait for, every connection, waits for one
     to be free, within its deadline. At the deadline it fails, saying what
     holds them: `collector "textfiles" has request.max_open_connections, 4,
     connections (3 open and held by a read, 1 being made for a scrape
     waiting for it), and none was free before the scrape's deadline`.

  A reload that lowers the limit closes nothing at once; the next connection
  that the collector makes brings it down to the limit. `0`, the default,
  sets no limit.

`http_exporter_sshfile_connections` counts the connections that a
collector's scrapes use that are open now, and
`http_exporter_sshfile_connects_total` those that they have made since the
exporter started. A connection made again and again (because a host drops
idle connections early, keepalives time out, or `max_open_connections` is
too low for the hosts read) shows as a counter that keeps rising (see
[Self-metrics](SELF-METRICS.md)). A collector's scrapes that waited for room
at its `max_open_connections` count in
`http_exporter_sshfile_limit_waits_total`, and those that failed there in
`http_exporter_sshfile_limit_failures_total`. A connection made after the
shutdown or the reload that gave it up is counted nowhere.

## Deadlines

Making a connection (the handshake and authentication), the start of the
SFTP session and the read all happen within the probe's deadline: its
`timeout` parameter or Prometheus's scrape timeout (see
[Probe deadlines](CONFIGURATION.md#probe-deadlines)).

- SFTP reads cannot be cancelled, so a probe whose deadline comes during a
  read returns at once and closes the file. When the read has not returned
  two seconds later, the connection is dropped, which ends it, and the next
  scrapes connect again.
- A directory being listed is ended at once instead, by closing the session
  that it is listed on, and the connection is kept.

A host that stops answering therefore never holds a probe past its deadline.

## Reading a directory

With `files`, the collector reads every file of `root` whose name matches
one of the patterns, as a
[`localfile` collector reads a directory](LOCALFILE.md#reading-a-directory).
Each file is decoded and checked on its own and labelled `file`, with
`localfile_mtime_seconds`, `localfile_scrape_error` and
`localfile_files_skipped` beside them. These have the same names as
`localfile`'s, so one alert covers both. The textfile directory of a host,
read over SSH:

```yaml
  - name: textfiles
    request:
      type: sshfile
      root: /var/lib/node_exporter/textfile_collector
      files: ["*.prom"]
      max_age: 1h
      username: metrics
      private_key_file: /etc/exporter/ssh/id_ed25519
      known_hosts_file: /etc/exporter/ssh/known_hosts
    transform:
      type: prometheus
```

```text
/probe?collector=textfiles&target=db1.example.com
```

The limits are `localfile`'s: `max_files`, the response limit for each file,
`max_total_bytes`, files read four at a time, what was read by the deadline
answered, and the listing stopped after ten times `max_files` entries, at
least 1000.

SFTP lists a directory in batches that the server chooses, a hundred or so
entries each. The exporter asks for the next batch only when it has looked
at the one before, and stops asking at that bound, so a directory of a
million entries costs a scrape what its first thousands do, never the whole
listing. A server that answers request after request with no entry but `.`
and `..` (or none) is asked 16 times in a row and no more; then the read
fails, saying so.

A directory is listed on an SSH session of its own, opened on the kept
connection beside its SFTP session for as long as the directory is listed,
and closed before its files are read.

- The scrape waits, at most 2 seconds and never past its deadline, for the
  host to close its side of the session. The host counts the session until
  then, and would otherwise refuse the next scrape's listing a session that
  it has not freed yet.
- The exporter then closes the session itself. Of what the host still sends
  meanwhile, it reads at most 512 KiB, two packets' worth, before closing it
  at once.
- A host that did not close a session within those 2 seconds (its SFTP
  server does not end when its input ends) is not waited for again on that
  connection, so its scrapes pay the wait once, not each time.
- A listing abandoned while it waits for the host's answer, to the opening
  of the session or to a request, is not waited for.

A host limits the sessions of one connection (OpenSSH's `MaxSessions`, 10 by
default), so it must allow one more for each directory listed at the same
time on the connection: scrapes of several collectors sharing it, or
overlapping scrapes of one collector. A host that refuses the session fails
the read, saying so. The directory is always `root`, because the target is
the host: the `path` and `param_<name>` probe parameters are refused.

## An appliance's status file

An appliance that writes its status as JSON and offers SFTP alone, scraped
with a password from a mounted Secret:

```yaml
  - name: appliance_status
    request:
      type: sshfile
      root: /status
      path: health.json
      username: monitor
      password_file: /etc/exporter/appliance/password
      known_hosts_file: /etc/exporter/appliance/known_hosts
      max_age: 10m
    transform:
      type: jq
    metrics:
      - name: appliance_temperature_celsius
        type: gauge
        expression: .sensors.temperature
      - name: appliance_fan_ok
        type: gauge
        expression: .fan.state
        value_map: {ok: 1, "*": 0}
```

## Static targets

A [static target](STATIC-TARGETS.md) of an `sshfile` collector names the
host as its `target`, which is required. Under its `request` it may set
`path` and `timeout`, and its own account and credentials: `username`,
`private_key_file`, `passphrase_file` and `password_file`. A target that sets
any of the three credential files replaces all three of the collector's with
its own, so it names a key or a password of its own; `username` replaces
only the collector's `username`. `root`, `known_hosts_file` and the
keepalive settings belong to the collector, and `params` fills its
placeholders:

```yaml
interval: 1m
targets:
  - name: db1_backups
    collector: backups
    target: db1.example.com
  - name: legacy_backups
    collector: backups
    target: legacy.example.com:2222
    request:
      username: backup
      password_file: /etc/exporter/ssh/legacy-password
```

## Errors and self-metrics

A failed read is reported in the `ssh` stage and follows
`error_handling.on_fetch_error`, as a failed `localfile` read does. The error
names the host, `file /var/lib/node_exporter/textfile_collector/backup.prom
does not exist (on db1.example.com:22)`, unless it already names it, as
every error of making the connection does.

A file over the size limit counts in
`http_exporter_series_limit_exceeded_total`, and a host that the target
lists refuse counts in `http_exporter_targets_refused_total` and is answered
`403`. Transforms see the file as a `localfile` file: no status (`$status`
is `null`, and Python's `response.status_code` is `None`), `Content-Type`
from the extension, `Content-Length` and `Last-Modified`.

A [debug probe](CONFIGURATION.md#debug-probes) shows the read in its request
section (the host and the file as an `sftp://` URL, the account, and whether
the connection was made for the probe or kept from an earlier scrape). It
never shows a credential:

```text
Requests
  1. READ sftp://db1.example.com:22/var/lib/node_exporter/textfile_collector/backup.prom -> read in 41ms
     Ssh-Connection: reused
     Ssh-User: metrics
```

With [verbose self-metrics](SELF-METRICS.md#verbose-per-request-self-metrics)
a read is labelled with that `sftp://` URL, path parameters kept as their
placeholders, and `http_method="READ"`.

## Limitations

- **A link swapped between the check and the open.** Every symbolic link on
  the way to a file is checked on the host before the file is opened, and
  the file is then opened by its resolved name: SFTP has no way to open a
  file relative to a directory already opened, as `openat` does. A link
  changed on the host between the two steps is followed by the server, and
  can lead outside `root`. Read as an account that can read nothing else it
  should not.
- **Keyboard-interactive is answered with the password alone**, so a host
  that asks for a one-time code cannot be scraped (see
  [Authentication](#authentication)).

## Troubleshooting

| The scrape says | Do |
| --- | --- |
| `... is not in request.known_hosts_file` | Add the host's line with `ssh-keyscan`, after checking its fingerprint. The line must name the host as the target writes it, `[host]:port` for another port. |
| `... presented the key ..., which is not the one ...` | The host's key changed, or something is in between. Find out which before replacing the line. A host whose key was replaced by one of another type, its old type no longer offered, says this too, with the new key's type and fingerprint. |
| `refused user ...: authentication failed with ...` | The account, the key's line in `authorized_keys`, or the password. |
| `request.private_key_file ... is encrypted` | Add `passphrase_file`, or use a key without a passphrase. |
| `request.private_key_file ... is an encrypted PKCS#8 key, which cannot be read` | Convert it with `ssh-keygen -p -f <key>`, or write it unencrypted. |
| `... (the SSH methods tried: keyboard-interactive) ...` | The host takes passwords through PAM; check the password, and that the host asks for nothing else. |
| `... asked keyboard-interactive questions in more than 3 rounds ...` | The host asks for more than the password, such as a one-time code; scrape with a key, or an account without it. |
| `... signed by the authority ..., which no @cert-authority line ... trusts for the host` | Add the authority's `@cert-authority` line, naming the host as the target writes it — `[host]:port`, or `[*]:port`, for a port other than 22 — or the host's own line. |
| `... did not start an SFTP session` | The host's SSH server has no `Subsystem sftp`, or the account may not use it. |
| `the host refused an SSH session to list the directory in (...); it did not close the session of an earlier listing ...` | The host's SFTP server does not end when the end of its input tells it the listing is done, so the host still counts the sessions of earlier listings: fix the server, or raise `MaxSessions`. |
| `the host's SFTP server ended the session the directory was listed on before the listing was done, ...` | The host's SFTP server exited in the middle of the listing, the SSH connection staying up: its log on the host (`sftp-server` or `internal-sftp`, through `LogLevel` or syslog) says why. |
| `the host's SFTP server answered 16 requests for directory entries in a row with no entry but . and .. ...` | The host's SFTP server does not list the directory as SFTP servers do; it answers neither entries nor the end of the directory. |
| `the host refused an SSH session to list the directory in ...` | The host's limit on sessions per connection, `MaxSessions` in OpenSSH's `sshd_config`, leaves no room for the listing's session: raise it, or have fewer collectors list directories on the host at once. |
| `request.root ... does not exist`, `file ... does not exist` | The path on the host, or the account's view of it under a chroot. |
| `file ... leads outside request.root through a symbolic link` | The file is a link out of `root`; point `root` at the real directory, or copy the file in. |
| `connecting to ...: ... deadline exceeded` | The host, its port or a firewall; raise the probe's `timeout` for a slow link. |
| `the SSH handshake with db1.example.com:22 did not finish: context deadline exceeded` | The port took the connection and said nothing: an SSH server that is stuck or at its `MaxStartups`, or a firewall or a proxy that takes connections itself. Check the port with `ssh -v`. |
| `... has request.max_open_connections, ..., connections (... held by a read ...)` | The collector's reads hold, or its scrapes wait for, every connection it may open; raise the limit, or the scrape's timeout. |
