# Prometheus Universal Exporter — `sshfile` Request Type Specification

This is the `sshfile` request type's section of § 5.1 of the
[exporter specification](SPECIFICATION-EXPORTER.md), in a document of its
own so that a change to the type is a change to this document. Everything
the request types share — the registry and the rules every type is held to
(§ 5.1), building without a type, decoding, transforms, error policies,
limits, caching, static targets and exposition — is in the exporter
specification, and a section number here, such as § 42.14, is one of its
sections. What a user reads of the type is [SSHFILE.md](SSHFILE.md).

## `sshfile`

An `sshfile` collector reads a file, or a directory of files, from the
probe's `target`, a remote host, over SFTP on an SSH connection, and MUST
present what it read exactly as `localfile` presents a file of its own: the
same path rules, the same reading of one file and of a directory (§ 5.1a),
the same response. Its keys:

| Key | Default | Rule |
| --- | --- | --- |
| `root` | none | **Required.** As `localfile`'s, a directory on the host, but `/` MUST be taken, and a backslash MUST be refused. |
| `path`, `files`, `max_files`, `max_total_bytes`, `max_age`, `max_response_bytes` | as `localfile` | With `localfile`'s rules; `path` without a backslash. |
| `username` | none | **Required**, not blanks alone. |
| `private_key_file` | none | A private key, OpenSSH, PKCS#1 or SEC 1, encrypted or not, or unencrypted PKCS#8. |
| `passphrase_file` | none | Only with `private_key_file`. |
| `password_file` | none | One of `private_key_file` and `password_file` MUST be set; both MAY be, the key tried first. |
| `known_hosts_file` | none | **Required.** |
| `keepalive_interval` | `30s` | `0`, which sends no keepalive, or at least `1s`. |
| `keepalive_count_max` | `3` | At least 1. |
| `idle_timeout` | `5m` | Not negative; `0` keeps an unused connection open. |
| `max_open_connections` | `0` | Not negative; `0` sets no limit. |
| `allowed_targets`, `denied_targets` | none | § 26.1. |

- A placeholder in `username` or any file key MUST be refused at load, and
  so MUST `username` and any file key written with blanks alone, a
  collector's and a static target's: `""` is the key left out. The
  credential files and `known_hosts_file` MUST be read when a connection is
  made, as the other types' credential files are read at a request, not at
  load; a file that is missing, unreadable, empty, not a key, an encrypted key
  without `passphrase_file`, or a `known_hosts_file` with no host key line or
  a line that is not one MUST fail the scrape naming the key and the file,
  never showing its content; an encrypted PKCS#8 key MUST be named as such,
  with the `ssh-keygen` command that converts it. Surrounding whitespace MUST
  be taken off a password and a passphrase as off every credential file.
  `--dry-run` MUST read those files of every `sshfile` collector, and of
  every static target that sets credential files of its own, as a connection
  would, and list what a scrape would fail on as warnings of the `config` or
  `static_targets` check, logged, without failing the check; startup and
  reloads MUST NOT read them.
- `password_file` MUST be offered as the `password` method, and as the
  answers of the `keyboard-interactive` method only to a host that neither
  takes nor has refused the `password` method, after every other method of
  the connection the host takes: a wrong password MUST be given once per
  connection. A question the host does not echo MUST be answered with the
  password, one it echoes with `username`, in 3 rounds of questions at most,
  a round without a question not counted; a host asking more MUST be refused
  saying so. The documentation MUST say that a one-time code is answered
  with the password, so a host asking for one cannot be scraped.
- The target MUST be `host`, `host:port` or `ssh://` followed by either, port
  22 when left out, an IPv6 address in brackets or bare. It is required: a
  probe without one MUST be answered `400`, a static target without one
  refused at load. A target with a user, a path, a query or another scheme,
  a port outside 1 to 65535, or a host that is no host (§ 26.1) MUST be
  refused, a probe's with `400`. The host MUST be held to the collector's
  `allowed_targets` and `denied_targets` and the cloud metadata addresses
  (§ 26.1): its name before anything is sent, and the address of every
  connection when it is made, through the same check of a connection as an
  `http` request's.
- The host's key MUST always be verified against `known_hosts_file`, plain
  and hashed lines and `[host]:port` alike, with the target's host as the
  name looked up; there MUST be no setting that skips it. The host key
  algorithms asked of the host MUST begin with those of the keys the file
  holds for it, so a host that offers several keys presents one the file
  has, and go on with every other the client takes, so a host that offers
  none of those presents another and is refused as not presenting the listed
  key; when an `@cert-authority` line holds for the host, or one names every
  host with a bare `*` and the file holds no plain line of the host, the
  certificate algorithms MUST come first; a bare `*` line MUST NOT put them
  before the types of the host's own lines, as OpenSSH's does not. A host
  certificate that authority signed MUST be taken, a bare `*` line's on
  every port. A host certificate whose authority no line trusts for the host
  MUST be checked again as the plain key it certifies, and taken when a line
  of the host holds that key, unless its authority is `@revoked`. A host the
  file does not list, one whose key is not the listed one, one presenting a
  certificate of an authority not trusted for it, and one whose key or
  certificate authority the file marks `@revoked`, MUST be refused before
  authenticating, each with its own message naming
  `request.known_hosts_file` and the presented key's type and SHA-256
  fingerprint, the first saying how to get the line with `ssh-keyscan` and
  that its fingerprint is to be checked first, the third naming the
  authority and `@cert-authority` and saying whether no line holds the key
  it certifies, the host's line holds another, naming the key certified, or
  the file marks it `@revoked`, naming it, the fourth `@revoked` and, for an
  authority, the authority. The documentation MUST say how a line names
  hosts on a port other than 22 (`[*]:port`). A refused authentication MUST
  name the user, the credential files and the SSH methods tried.
- The file read MUST be `root` / `path`, with the `path` probe parameter and
  path parameters held to `localfile`'s rules; the directory read MUST be
  `root`, the `path` and `param_<name>` probe parameters refused as for a
  `localfile` directory. A backslash MUST be refused in `request.path` and a
  static target's at load, and in the `path` probe parameter with `400`
  naming it, and a symbolic link whose target has one MUST NOT be followed.
  Every name MUST be confined to `root` twice:
  lexically, as `localfile`'s, and on the host, where `root` and then the
  name are resolved component by component, every symbolic link on the way
  followed with `lstat` and `readlink`, and a name that resolves outside the
  resolved `root` refused as leading outside `request.root` through a
  symbolic link, before it is opened; the resolved path MUST be what is
  opened. A link loop MUST be refused after 40 links. The documentation MUST
  say, among the type's limitations, what this leaves to the server: a link
  changed between the check and the open, since SFTP has no open relative to
  a directory.
- A directory MUST be listed in SFTP's batches, the next batch asked of the
  host only when the entries before it have been looked at, so that the
  listing stops at its bound (§ 5.1a) having received at most one batch past
  it, never the whole directory. It MUST be listed on an SSH session of its
  own, opened on the connection for the listing and closed when the listing
  ends — at the end of the directory, at the bound, on an error or when the
  read is abandoned — before the directory's files are read; a listing that
  does not wait for the host's answer MUST wait, at most 2 seconds and never
  past the probe's deadline, for the host to close its side of the session
  (the end of the session's output) before the read goes on, so a scrape
  right after it is not refused a session the host still counts, and MUST
  then close the session, the host having closed its side or not, having
  read at most 512 KiB of what the host sent meanwhile. A listing waiting
  for the host's answer, to the opening of its session or to a request,
  MUST NOT be waited for. Once a host has not closed a listing's session
  within the 2 seconds, the later listings on that connection MUST NOT wait
  for it, and a session the host then refuses MUST fail the read saying the
  host did not close an earlier listing's session. A host answering 16
  requests for entries in a row with no entry but . and .. MUST fail the
  read saying so. A listing's session that ends under it (the end of its
  output) MUST be taken for the connection lost, the connection dropped and
  the read made again once on a new one, only when the connection is not up:
  a `keepalive@openssh.com` request MUST be sent on it, and when the host
  answers it, refusing it or not, within 2 seconds and before the probe's
  deadline, the listing MUST fail saying the host's SFTP server ended the
  listing's session, the connection MUST be kept, and the listing MUST NOT
  be tried again. A host that
  refuses that session MUST fail the read saying that its limit on sessions,
  OpenSSH's `MaxSessions`, must allow one more for each directory listed at
  once on the connection; the documentation MUST say so. What the host sends
  MUST be held to bounds before anything is allocated for it: a packet over
  256 KiB, a count of entries or of extended attributes larger than the rest
  of the packet holds, a string or attributes cut short, a name that is
  empty or holds `/` or a NUL, an answer of another type or to another
  request MUST fail the listing saying what was wrong. `.` and `..` MUST be
  left out, and an entry's name, type, mode, size and modification time,
  and the errors of a directory that does not exist or may not be read,
  MUST be those pkg/sftp's listing gives.
- One SSH connection, with one SFTP session, MUST be kept per host and port,
  username, credential files, `known_hosts_file`, keepalive settings and
  target lists, and collector for one with `max_open_connections`, made at
  the first scrape that needs it and shared by every scrape that reaches it,
  at once or later, of one collector or of several.
  Scrapes asking for a connection while it is being made MUST wait for that
  one rather than make their own, each within its own deadline; the making
  MUST NOT end with the deadline of the scrape that started it, but within
  the larger of that deadline and 30 seconds, and a connection made when no
  scrape waits for it any more MUST be kept. A scrape whose deadline comes
  while it waits MUST say how far the making had got: the TCP connection,
  the SSH handshake or the start of the SFTP session. A connection MUST be
  made again at the next scrape when one of its files has changed on disk,
  or when it has ended, and the making of one so replaced MUST be cancelled
  once no scrape waits for it; a scrape whose read loses its connection MUST
  connect again once, within its deadline.
- Every `keepalive_interval` a `keepalive@openssh.com` request wanting a
  reply MUST be sent; a reply of any kind is an answer. When
  `keepalive_count_max` in a row have no answer within an interval, the
  connection MUST be closed, whatever reads hold it, and the next scrape
  MUST make a new one. A connection no read has held for `idle_timeout`
  MUST be closed; with `0` it MUST be kept. A positive `keepalive_interval`
  under `1s` MUST be refused at load: each keepalive is given the interval to
  be answered in.
- A collector with `max_open_connections` above 0 MUST hold at most that many
  connections open at once, those being made and those a read still holds
  among them, and MUST share none with another collector. A scrape that needs
  a new one at the limit MUST first close the collector's connection that no
  read holds and was used longest ago; when there is none, it MUST cancel the
  making of one no scrape waits for any more, and wait for it to end; when
  reads hold, or scrapes wait for, every one, it MUST wait for one to be free
  within its deadline, and fail at it naming `request.max_open_connections`,
  its value, and how many of the connections are held by a read and how many
  being made. A changed limit MUST be held from the next connection the
  collector makes.
- Each connection MUST note the collectors whose scrapes used it. When the
  per-collector state follows a configuration (§ 24), a collector that is
  gone, or whose settings — its own, or a static target's of it — no longer
  make the connection, MUST stop being one of them, and a connection left
  with none MUST be closed once no read holds it, and the making of one
  MUST be cancelled once no scrape waits for it, at the reload or when the
  last scrape waiting for it stops waiting. The exporter's shutdown MUST
  close every connection and cancel the making of every one being made,
  whether the cache still keeps it or a scrape waits for one it forgot.
- Making the connection, the handshake with authentication, the start of the
  SFTP session and the read MUST happen within the probe's `timeout`, budget
  (§ 3.2a) or context. A probe whose deadline comes during a read MUST
  return at once, close the files the read has open, and drop the connection
  when the read has not returned two seconds later, so that a host that stops
  answering never holds a probe past its deadline nor a read forever. A
  directory being listed MUST instead be ended at once by closing the session
  it is listed on, its connection kept. A directory read cut short MUST
  answer what was read, as § 5.1a says.
- A read MUST be presented as a `localfile` read is: no status (`$status`
  `null`, `response.status_code` `None`), `Content-Type` by the extension,
  `Content-Length` and `Last-Modified`, and a directory as § 5.1a's answer
  with the same `localfile_*` series. A failed read MUST be reported in the
  `ssh` stage, follow `on_fetch_error`, and name the host once. A file that does
  not exist, one outside `root`, one too large (a limit error) and one older
  than `max_age` MUST each be named as such.
- It MUST accept the `/probe` parameters `path`, `timeout` and
  `param_<name>`. A static target MAY set `path`, `timeout`, `username`,
  `private_key_file`, `passphrase_file` and `password_file`; a target that
  sets any of the three files MUST use its own three in place of the
  collector's, held to the collector's rules for them, and its `username`
  replaces the collector's. These MUST enter its cache key in a section a
  probe's key never has.
- A debug probe's request section MUST show each read: `READ`, the host and
  the file or directory as `sftp://host:port/path`, the path escaped as a
  URL's path is, as `localfile`'s is, the user, and whether the
  connection was made for the probe or kept from an earlier scrape, and no
  credential. The verbose `url` label MUST be that URL, with path parameters
  as their placeholders, ending in `/` for a directory, and `http_method`
  MUST be `READ`.
