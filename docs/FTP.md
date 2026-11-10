# FTP

The `ftp` request type reads one file from an FTP server and hands it to
the decoder its extension names, as [`sshfile`](SSHFILE.md) reads one over
SFTP and [`localfile`](LOCALFILE.md) one from the exporter's own disk. It
is for the systems that still drop their files on an FTP server: a CSV a
batch job leaves every night, a status file an appliance exports.

It only reads: the commands that store, rename or delete are not in the
code. Plaintext FTP, explicit TLS (`AUTH TLS`) and TLS from the first byte
are spoken.

The type is opt-in: the default build and the published image leave it out,
so a build carries it only when asked
([Building with ftp](#building-with-ftp)).

## Building with ftp

`OPT_IN_REQUEST_TYPES` names the opt-in types a build adds to the types it
carries, comma-separated:

```sh
make build OPT_IN_REQUEST_TYPES=ftp              # bin/prometheus-universal-exporter
docker build --build-arg OPT_IN_REQUEST_TYPES=ftp -t exporter:ftp .
go build -tags "$(sh tools/request-type-tags.sh '' '' ftp)" .
```

The tag is `request_type_ftp`. A configuration with an `ftp` collector
is refused by a build without the type, at startup, on a reload and by
`--dry-run`, saying how to build one that has it. The type is written with
the standard library alone, and adds no dependency.

## A collector

```yaml
collectors:
  - name: meters
    request:
      type: ftp
      root: /exports
      path: "{{param_site}}/meters.csv"
      max_age: 26h
      username: exporter
      password_file: /etc/exporter/ftp-password
      tls:
        ca_file: /etc/exporter/ftp-ca.pem
    transform:
      type: jq
    metrics:
      - name: meter_energy_kwh
        items: .[]
        expression: .kwh
        labels:
          - name: meter
            expression: .meter
```

```text
/probe?collector=meters&param_site=berlin&target=ftpes://files.example.com
```

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `ftp`. |
| `root` | — | **Required.** The directory the collector may read under, absolute. |
| `path` | — | The file, relative to `root`. May contain `{{param_name}}` placeholders, each value one path element. A probe's `path` parameter replaces it. |
| `max_age` | — | Fail the scrape when the file was last modified longer ago, by the server's `MDTM`. |
| `username`, `password_file` | `anonymous` | The account. The file is read at each scrape. |
| `tls` | — | How the server's certificate is verified, and a client certificate, as for [any type that speaks TLS](REQUESTS.md#tls). |
| `max_response_bytes` | the collector's limit | The largest file read. |
| `allowed_targets`, `denied_targets` | — | The hosts the collector may and may not reach, as for [any type that reaches a host](REQUESTS.md#restricting-targets). |

The file is named as a [`localfile`](LOCALFILE.md) collector's is: a
placeholder's value may hold no `/`, and neither `path` nor the `path`
parameter may lead outside `root`. The decoder is chosen by the file's
extension, as for a local file — `.json`, `.csv`, `.yaml`, `.xml`, `.prom`
and the others [`decoder.type: auto`](LOCALFILE.md#formats) knows — and by
`decoder.type` when it is set.

## The target

| Target | Connection |
| --- | --- |
| `host`, `host:port`, `ftp://host:port` | Plaintext, port 21. |
| `ftpes://host:port` | Explicit TLS, port 21: `AUTH TLS` before the account is sent. A target without a scheme is this too when the collector has a `tls` block. |
| `ftps://host:port` | TLS from the first byte, port 990. |

A path, a user or another scheme in the target is refused with `400`
before anything is sent: the file is `request.path`'s, and the account
`request.username`'s. `ftp://` is plaintext, the password with it.

## What a scrape does

One control connection: log in, `TYPE I`, `SIZE` and `MDTM` for the file,
a passive data connection, `RETR`, `QUIT`. The data connection is always
passive (`EPSV`, and `PASV` for a server without it), made by the exporter
to the server the control connection reached, whatever address the
server's answer names — which is also what gets through NAT — and through
the collector's target lists. Under TLS the data connection is TLS too,
and resumes the control connection's session, as vsftpd and ProFTPD
require.

The answer has `Last-Modified` when the server answered `MDTM`, and the
file's certificate is the control connection's for
[`$tls`](REQUESTS.md#timings-tls-and-the-final-url).

## Static targets

A [static target](STATIC-TARGETS.md) of an `ftp` collector is a server,
written as a probe's target is. Its `request` may set `path` and `timeout`,
and `username`, `password_file` and `tls` for a server with an account or
a certificate authority of its own.

## Limits

A scrape ends at its deadline, the probe's `timeout` or the scrape
timeout, and after 30 seconds when it has neither. A file larger than
`max_response_bytes` fails the scrape as a limit exceeded: by its `SIZE`
before anything is transferred, and by what was read from a server without
`SIZE`. Two connections are made for each scrape, and none is kept.

## Errors and self-metrics

A server's refusal fails the scrape with its reply:

```text
the server answered RETR /exports/berlin/meters.csv with 550 Failed to open file.
the server answered PASS *** with 530 Login incorrect.
```

The password is in no error, log line or debug report. With
`web.self_metrics.verbose`, a scrape is labelled with the file's URL,
`ftp://host:port/exports/{{param_site}}/meters.csv`, and `RETR`.

## Limitations

- One file a scrape. A directory's files are not listed or read together,
  as `localfile` and `sshfile` read them with `files`.
- Passive mode only: the exporter never listens for the server.
- Binary transfer only, with no restart of a transfer cut short.
- `max_age` needs a server that answers `MDTM`; without it the scrape
  fails, saying so.
- SFTP is another protocol: use [`sshfile`](SSHFILE.md).

## Troubleshooting

- **`AUTH TLS` answered with 500 or 502**: the server has no explicit
  TLS. Use `ftp://`, or `ftps://` if it serves TLS on port 990.
- **`522 SSL connection failed; session reuse required`**, or a data
  connection that fails under TLS: something between the exporter and the
  server ends the TLS session, as an FTP-aware firewall does.
- **`the data connection: ... refused` or a timeout after `EPSV`**: the
  server's passive ports are not open to the exporter.
- **`550` for a file that is there**: the account's directory is not
  where `root` says; many servers confine an account to its home, where
  `/` is that home.
