# Databases over SQL

The `sql` request type runs one statement on a database and turns the rows it
returns into metrics. Use it for what a database knows that no exporter of its
own shows:

- the jobs of a queue table by state, the age of the oldest unprocessed row;
- the time of the last successful import, written by the importer itself;
- the counts a business dashboard shows, from the application's own tables.

Three drivers are built in with the type: `mysql` (MySQL and MariaDB),
`postgres` (PostgreSQL and the databases that speak its protocol) and
`sqlserver` (Microsoft SQL Server and Azure SQL). Four more — `oracle`,
`clickhouse`, `snowflake` and `vertica` — are in a build that asks for them,
since they bring large dependencies of their own (see [Building with more
drivers](#building-with-more-drivers)).

## A collector

```yaml
collectors:
  - name: jobs
    request:
      type: sql
      driver: postgres
      database: app
      username: metrics
      password_file: /etc/exporter/db/password
      statement: SELECT state, count(*) AS jobs FROM jobs WHERE queue = $1 GROUP BY state
      args: ["{{param_queue:default}}"]
    transform:
      type: csv
    metrics:
      - name: app_jobs
        type: gauge
        expression: jobs
        labels:
          - name: state
            expression: state
```

```text
/probe?collector=jobs&target=db1.example.com&param_queue=mail
```

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `sql`. |
| `driver` | — | **Required.** `mysql`, `postgres` or `sqlserver`; `oracle`, `clickhouse`, `snowflake` or `vertica` in a build that has them. In any case, blanks around it aside. |
| `statement` | — | **Required.** The one statement run, with the driver's own placeholders for `args` (see [The statement](#the-statement)). Not blanks alone. |
| `args` | — | The values bound to the statement's placeholders, in order, as text. May use [placeholders](REQUESTS.md#path-parameters), `{{param_name}}` and `{{param_name:default}}`. |
| `database` | the account's | The database the statement runs on. May use placeholders, filled with letters, digits and `_ - . $` only. Not for `oracle`. Not blanks alone. |
| `service_name` | — | **Required for `oracle`**, and for no other driver: the service of the database. |
| `username` | — | **Required.** The account the statement runs as. Not blanks alone. |
| `password_file` | — | The file of the account's password. Left out, no password is sent. |
| `private_key_file` | — | `snowflake` only: the RSA key of key-pair authentication, instead of `password_file` (see [Authentication](#authentication)). |
| `tls` | — | `ca_file`, `cert_file` and `key_file`, `server_name`, `insecure_skip_verify`, as for [any request](REQUESTS.md#tls): the block written, even empty, switches TLS on, verified, unless the driver's TLS option says otherwise (see [Drivers](#drivers)). |
| `options` | — | The driver's own settings, from a short list for each (see [Drivers](#drivers)); any other is a startup error. |
| `max_rows` | `10000` | The most rows the statement may return; one more fails the scrape, and the statement is ended there, the rest of its rows not read (the driver may close its connection to do so). |
| `max_response_bytes` | limit | The most bytes the rows may hold: each value, text and binary data by its length and every other value as 8, with its column's name's length, which the rows the transforms read have in each row the column has a value in; a `NULL`, left out of its row, counts nothing. A driver reads a row before the limit sees it: how much a row larger than the limit costs is under [Limitations](#limitations). |
| `connections` | — | How connections to a target are kept (see [Connections](#connections)). |
| `allowed_targets`, `denied_targets` | — | The servers the collector may and may not reach, as for [any type that reaches a host](REQUESTS.md#restricting-targets), checked on every connection. |

No other request key applies, and setting one — `method`, `headers`, `retry`,
`passphrase_file` — is a startup error. The file keys name files on the
exporter's own filesystem, such as a mounted Secret, and take no placeholders.
They are read each time a connection is made, not when the configuration
loads: a password replaced on disk is what the next connection sends, and a
file that is missing or empty fails the scrape, naming it.
[`--dry-run`](CONFIGURATION.md#dry-run) reads them, as a connection would, and
warns of each a scrape would fail on now.

## The target

A probe's target, and a static target's, is the database server: `host` or
`host:port`, an IPv6 address in brackets (`[2001:db8::1]:5432`), the driver's
port when it names none. A target that ends in a colon with no port after it,
`db1.example.com:` or `[::1]:`, is refused, a static target's when the file
loads and a probe's on the scrape: `target "db1.example.com:" ends in a colon
with no port after it; write the port after the colon, or leave the colon out
for the driver's port`.

| Driver | Port |
| --- | --- |
| `postgres` | 5432 |
| `mysql` | 3306 |
| `sqlserver` | 1433 |
| `oracle` | 1521 |
| `clickhouse` | 9000 native, 9440 native with TLS, 8123 http, 8443 http with TLS |
| `vertica` | 5433 |
| `snowflake` | — the target is the account identifier |

A `snowflake` target is the account identifier, `myorg-myaccount` or a locator
such as `xy12345.eu-west-1`, reached at `<account>.snowflakecomputing.com`
over HTTPS. A connection to it that the target lists refuse fails the scrape
at once with their refusal, as with every driver, where the driver itself
would retry it until the scrape's deadline.

Nothing else may come with a target: no scheme, no user, no database, no
options — `postgres://u:p@db/app?sslmode=disable` is answered `400`. They are
the collector's keys, so a probe can reach only the databases and accounts the
configuration names. A collector's `allowed_targets` and `denied_targets` hold
the server's name, and every address a connection reaches, which the
exporter's own dialer checks for every driver but `vertica` (see
[Limitations](#limitations)).

## The statement

`statement` is one statement, written in the database's own SQL, with the
driver's own placeholders for the values in `args`:

| Driver | Placeholders |
| --- | --- |
| `postgres` | `$1`, `$2`, … |
| `mysql`, `clickhouse`, `snowflake`, `vertica` | `?` |
| `sqlserver` | `@p1`, `@p2`, … |
| `oracle` | `:1`, `:2`, … |

`args` are sent to the database as values, never as SQL: `postgres`,
`mysql`, `sqlserver`, `oracle`, `snowflake` and `vertica` bind them to the
placeholders at the server, while `clickhouse`'s driver writes each into the
statement before sending it, quoted as a literal of its type, as `vertica`'s
does in a `COPY ... FROM LOCAL` statement. Either way a probe parameter in one
cannot change the statement, whatever it holds. A placeholder in `statement` itself is a
startup error: a value from the probe goes in `args`.

**Read-only.** The statement runs read-only where the database can make it
so:

- `postgres`, `mysql` and `vertica`: in a transaction begun `READ ONLY`,
  rolled back afterwards;
- `oracle`: after `SET TRANSACTION READ ONLY`, in a transaction rolled back
  afterwards;
- `clickhouse`: with the setting `readonly=1`.

SQL Server and Snowflake have no such transaction (their drivers refuse one):
the statement runs as it is, and the account must be one that can only read.
Give every collector an account that can read what its statements read and
nothing more, whatever its database: read-only is a second line, not the
first.

**One statement.** `postgres` sends every statement with the extended
protocol, bound even without `args`, so the server refuses two joined with
`;`; `mysql` never allows several statements in one. `vertica`'s driver would
split a statement at each `;` and run the parts one after another, a `COMMIT`
among them ending the read-only transaction, so a `vertica` statement of
several parts — a `;` followed by anything but blanks and comments, outside
quotes, quoted identifiers, comments and dollar quotes — is a startup error.
The exporter decides this itself as well as by the driver's own splitting:
since vertica-sql-go 1.3.9 the driver does not split at a `;` it takes to
stand inside a `BEGIN ... END`, `CASE`, `IF`, `LOOP` or `WHILE` block, and
sends such text to the server whole, which runs every statement in it — even
`SELECT 1 AS begin; COMMIT`. A `//` is not a comment to either, so a `;`
followed by `// ...` is refused too. The exporter reads the text every way
the server may, which is not always the driver's way, and refuses it if any
reading finds several statements: a `$` inside a word is part of it, not a
dollar quote (`SELECT a$b$; COMMIT; SELECT $b$x$b$` is three statements); a
`\` escapes the next character in an `E'...'`, and in every `'...'` where
`standard_conforming_strings` is off (`SELECT E'\''; COMMIT; --'` is two);
block comments may nest; and only ASCII white space is blank. A statement
the driver's lexer fails on — a `?` right after a quote at the start of the
text or of a line after a `--` comment, as in `'?'` — is a startup error
too, since the driver would fail on it at every scrape.
The other drivers send what is written, and a statement of several parts is the database's to run
or refuse: another reason for an account that can only read.

**Deadlines.** The statement runs within the probe's deadline, its `timeout`
parameter or Prometheus's scrape timeout (see [Probe
deadlines](CONFIGURATION.md#probe-deadlines)), connecting included. When the
deadline comes the scrape fails, `running the statement: the scrape's
deadline came first`, and the driver stops the statement: `postgres` and
`sqlserver` ask the server to cancel it, `clickhouse` and `snowflake` abort
it, `oracle` breaks it off, and `mysql` closes the connection, whose statement
the server ends when it notices. `clickhouse` on `http` aborts it by ending
its request, and sends every request with the setting
`cancel_http_readonly_queries_on_client_close=1`, which has the server stop
the statement then. `vertica`'s connection is given up at the deadline, and
the server is not asked to stop the statement (see
[Limitations](#limitations)). `postgres` also sets `statement_timeout` to
the time the scrape has left, in the transaction, so the server ends a
statement nobody waits for even if the cancel is lost. For the others, an
account with a limit of its own (`MAX_EXECUTION_TIME` or `max_statement_time`
in MySQL and MariaDB, `STATEMENT_TIMEOUT_IN_SECONDS` in Snowflake, a resource
governor in SQL Server, a resource pool's `RUNTIMECAP` in Vertica) is the
server's side of the same bound.

A driver that fails on what a server sends — a panic, on an answer it reads
without checking — fails the connection, the statement or the keepalive ping
it was on, never the exporter, and the connection is closed. That holds
outside any scrape too: for a driver that fails so closing a connection past
its `max_lifetime` or `idle_timeout`, or rolling back the transaction of a
scrape whose deadline came. One that fails so while connecting holds no
place at `max_per_target` after it.

## The rows

The rows reach the transforms as a list of objects, one a row, of its columns
by name:

```json
[{"state": "open", "jobs": 3}, {"state": "done", "jobs": 40}]
```

- A `csv` transform reads a column by its name, one series a row, as it reads
  the columns of a CSV file with a header: `expression: jobs` above.
- `jq` and `yq` read the list: `items: .[]` and `expression: .jobs`.
- `python` is handed the list as `data`.

No other transform reads rows, and `decoder.type` and the `response` block do
not apply: the driver reads the values, and there is no body to decode.

| The column holds | The transforms see |
| --- | --- |
| an integer or a floating-point number | the number |
| a decimal (`NUMERIC`, `DECIMAL`, `NUMBER`, `MONEY`; Snowflake's `NUMBER`, which its driver names `FIXED`, and `DECFLOAT`), which drivers give as text | the number it writes, or its text when it writes none |
| a boolean | `1` or `0` |
| a date or a timestamp (`DATE`, `DATETIME`, `TIMESTAMP`) | its Unix time in seconds, with the fraction |
| a time of day (`TIME`, `TIMETZ`, ClickHouse's `Time` and `Time64`) | the seconds since midnight, with the fraction, as its clock reads where it was written: `13:45:30.5` is `49530.5`; a MySQL `TIME`, a duration, may be negative or past 24 hours, `-01:00:00` is `-3600` |
| text | the text |
| binary data | its text when it is UTF-8; otherwise the scrape fails, naming the column |
| `NULL` | nothing: the column is left out of its row |

A `NULL` is a value the row does not have: a `jq` rule finds no key, and a
`csv` rule reading the column has no value in that row — a label read from it
is left off that row's series — which its `error_mode` decides for a value. A
column `NULL` in every row is still one the statement returned: a `csv` rule
reads it as it reads one `NULL` in some rows, not as a column the response
lacks (see [When a metric cannot be extracted](CONFIGURATION.md#when-a-metric-cannot-be-extracted)). A
column without a name — `SELECT count(*) FROM jobs` in SQL Server — and two
columns of one name fail the scrape: name each with `AS`. A statement that
returns no row leaves every rule without a value, as a response with no item
does.

## Authentication

The account is `username`, its password the content of `password_file`, its
blanks around it taken off, as every credential file's are. A password file
that is empty, or holds a line break inside the password, fails the scrape.
The account and the password reach the driver as written, whatever they
hold: `:`, `@`, `/`, `%` and blanks are theirs, not a separator.

`snowflake` also takes key-pair authentication: `private_key_file` names an
RSA private key in PKCS#8 (`-----BEGIN PRIVATE KEY-----`) or PKCS#1, not
encrypted, whose public key is set on the Snowflake user. The exporter cannot
read an encrypted key: write it unencrypted, `openssl pkcs8 -topk8 -nocrypt
-in rsa_key.p8 -out rsa_key_plain.p8`, and keep the file readable by the
exporter alone. A `snowflake` collector needs `password_file` or
`private_key_file`, and not both.

No other authentication is offered: a password is never written in the
configuration itself, and Kerberos, IAM and Azure AD tokens are not used.

## Drivers

Each driver takes a short list of `options`, and the option that switches its
TLS. With that option left out, a collector that writes a `tls` block has
TLS, the server's certificate verified against `tls.server_name` or the
target's host, and one that writes none has no TLS. A `tls` block, even an
empty one, counts: `tls: {}` asks for TLS verified against the system's
roots, and beside an option that switches TLS off it is refused like any
other.

| Driver | Options | TLS |
| --- | --- | --- |
| `postgres` | `sslmode`: `disable`, `require` (TLS, the certificate not verified) or `verify-full`; `application_name`, `prometheus-universal-exporter` by default | `sslmode` |
| `mysql` | `tls`: `false`, `true` or `skip-verify` (TLS, the certificate not verified) | `tls` |
| `sqlserver` | `encrypt`: `disable` (no TLS at all), `false` (the login alone, its certificate not verified), `true` or `strict` (TDS 8.0, TLS first) | `encrypt`; `true` with a `tls` block, `false` without |
| `oracle` | `ssl`: `false` or `true` | `ssl`; `tls.server_name` is refused, since the driver checks the certificate against the target's host itself |
| `clickhouse` | `protocol`: `native` or `http`; `compress`: `none`, `lz4` or `zstd`; `secure`: `false` or `true` | `secure` |
| `snowflake` | `warehouse`, `role`, `schema` | always TLS, verified |
| `vertica` | `tlsmode`: `none`, `server` (TLS, the certificate not verified) or `server-strict` | `tlsmode` |

A `tls` key beside a TLS option that switches TLS off is a startup error:
it would be ignored. So are `tls.ca_file` and `tls.server_name`, which say how
the server's certificate is verified, beside a value that does not verify it
(`sslmode: require`, `tls: skip-verify`, `tlsmode: server`): use the
verifying value (`verify-full`, `true`, `server-strict`), or leave them out;
a client certificate, `cert_file` and `key_file`, is presented either way.
The TLS files are read each time a connection is made, and that connection
uses what was read for it, so a file replaced is used from the next
connection on; `vertica`'s driver, which takes its TLS settings by a name
registered with it and keeps every name for good, is given one name for
each connection being made at once, used again by later ones, so it keeps
no more than that however often the files change.
SQL Server encrypts the login alone with a certificate
it makes itself unless given one, and its own tools do not verify that
certificate: neither does the exporter, with `encrypt: false` or left out,
unless a `tls` block says how to (`ca_file`, `server_name`, or none for the
system's roots). Unless `encrypt` is
`disable`, the login is never sent unencrypted: a server that answers it does
not support encryption fails the scrape, `the server answered that it does
not support encryption, so the login would be sent unencrypted`; enable
encryption on the server, or write `encrypt: disable`; and a prelogin answer
with an empty `ENCRYPTION` option, which no SQL Server sends, fails the
connection whatever `encrypt` is. `oracle` with `ssl: true` makes every
connection TLS: a listener that redirects the connection to an address with
`PROTOCOL=TCP` has it fail, `the oracle driver began a connection without
TLS`, before anything is sent on it; have the listener redirect to a `TCPS`
address. `clickhouse` with
`secure: true` makes the TLS handshake before anything else on the native
protocol as on `http`. Each connection is configured from the collector alone:
the `PG*` environment variables and `.pgpass` file `postgres` would read,
for one, change nothing — but `PGSERVICE`, which the driver reads whatever
it is told, and which fails every `postgres` connection until it is unset —
nor do `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY`:
every driver, `clickhouse` on `http` and `snowflake` too, connects to the
server directly, never through a [proxy](REQUESTS.md#proxies). SQL Server's named instances are reached by their
port, not by name: finding one by name would ask the SQL Server Browser over
UDP, and the exporter's dialer makes TCP connections alone, refusing the
driver's `udp` connection with `the driver asked for a udp connection`.

## Connections

A collector keeps its connections to a target in a pool, one for each target
and each set of connection settings — account, credential files, database,
options, TLS settings, `connections`, target lists — shared by its scrapes of
the target and with no other collector. A connection is made at the first
scrape that needs one and kept for the next.

The limits are of targets, not pools. A target is the server, `host:port`, or
the Snowflake account, with the collector's connection settings: the pools of
the databases and accounts a collector reaches on one server are one target's,
counted once in `max_targets` and together in `max_per_target`. A [static
target](#static-targets) whose `connections` give it other settings is a
target of its own, with its own limits, though it names a server the probes
reach.

```yaml
    request:
      connections:
        max_per_target: 2
        max_idle_per_target: 1
        max_targets: 50
        idle_timeout: 5m
        max_lifetime: 1h
        keepalive:
          interval: 30s
          check: tcp
```

| Key | Default | Notes |
| --- | --- | --- |
| `max_per_target` | `2` | The most connections open to one target at once, in use or idle, across its databases and accounts, a `keepalive.check: ping` holding one while it pings. A scrape that would make one more first closes an idle connection of another of the target's pools. A scrape that finds all of them in use waits for one within its deadline, and fails at it: `collector "jobs" has request.connections.max_per_target, 2, connections to db1.example.com:5432, and none was free before the scrape's deadline`. It bounds the target's pools too, one for each database and account its scrapes reach, a pool being of no use without a connection: a scrape that needs another closes the target's pool used longest ago that no scrape is using, and fails, counted as a limit failure, when every one is in use. `0` sets no limit to the connections, and keeps at most 64 pools for a target. |
| `max_idle_per_target` | `1` | How many connections to a target are kept idle between scrapes, across its databases and accounts: a connection given back while the target's pools keep that many idle is closed. `0` keeps none: each scrape connects anew. |
| `max_targets` | `50` | The most targets the collector keeps connections to. A scrape of another target at the limit closes the pools of the target used longest ago that no scrape is using, and fails when every target is in use: `collector "jobs" has request.connections.max_targets, 50, targets with connections open, each in use by a scrape`. `0` sets no limit. |
| `idle_timeout` | `5m` | How long an idle connection is kept; `0` keeps it as long as the collector is configured. A `keepalive.check: ping` is no use: a connection no scrape has had for `idle_timeout` is closed by the keepalive, at its next interval, instead of pinged. |
| `max_lifetime` | `1h` | How long a connection is used before it is closed and made again; `0` uses it as long as it works. |
| `keepalive.interval` | `30s` | How often a kept connection is checked; at least `1s`, `0` checks none. |
| `keepalive.check` | `tcp` | `tcp`: TCP keepalives on every connection at the interval, so one a firewall or a dead host dropped is found broken. `ping`: every interval, each idle connection of the pool is pinged, and a connection the driver then reports broken is closed. Each ping takes one of the target's places at `max_per_target`, without waiting, so that no more connections are pinged than there are places free: when the scrapes hold every place the pings are skipped for that interval. They never make a connection, and are given the interval to answer in: one that does not answer by then fails, counted, and frees its place; the connections have no TCP keepalives, as with an interval of `0`, even with a driver that would switch them on (`mysql`). |

A connection that broke is found by the scrape that takes it, or by a
keepalive, and made again. A reload that removes a collector, or changes any
of its connection settings, closes its pools: the next scrape makes a pool of
the new settings, while the scrapes using an old pool then — a statement
still running, a scrape waiting at `max_per_target` — finish on it, and it is
closed when the last is done; their connections are counted as the
collector's while it is configured, and no longer once a reload removes it.
The exporter's shutdown closes every pool the same way.

The end of a statement, closing its rows and rolling its transaction back, is
bounded by the scrape's deadline (or a tenth of a second when the deadline
has passed by then): a driver may wait on it without a deadline of its own,
as `mysql`'s rollback waits for the server's answer. When the deadline comes
first, the scrape answers the rows it has read, the connection's network
connection is closed, and the connection is closed, never kept; it holds its
place at `max_per_target` until it is closed. A `vertica` connection, which
the driver dials by itself, cannot be closed under the driver: it holds its
place until the driver's end returns.

A transaction is rolled back by the scrape, at the statement's end, even
when the deadline comes while it is open: never by Go's `database/sql` on
its own the moment the deadline comes, which would close the connection
while the scrape still has it. A connection whose transaction was still open
when the deadline came, or when the statement was ended before its rows were
all read, is then closed rather than kept, unless its driver resets the
connection's session and says whether it is still usable, as `postgres` and
`mysql` do.

## Static targets

A [static target](STATIC-TARGETS.md) of a `sql` collector names the server as
its `target`, and may set under its `request` its `timeout`, its own account
and credential file — `username`, and `password_file` or, for `snowflake`,
`private_key_file`, either replacing the collector's credential file, and
none of them holding a placeholder, which is refused when the file loads
since no probe fills it — its own `tls` block, and
`connections`, whose keys each replace the collector's but `max_targets`,
which is the collector's across its targets; a target whose `connections`
change a setting is a target of its own for the limits, counted once more in
`max_targets`. `params` fills the collector's
placeholders, in `args` and `database`:

```yaml
interval: 1m
targets:
  - name: billing_jobs
    collector: jobs
    target: billing-db.example.com
    params:
      param_queue: invoices
  - name: legacy_jobs
    collector: jobs
    target: legacy-db.example.com:5433
    request:
      username: legacy_metrics
      password_file: /etc/exporter/db/legacy-password
      tls:
        ca_file: /etc/exporter/db/legacy-ca.pem
      connections:
        max_per_target: 1
```

A target's `tls` replaces the collector's whole block for that target, a key
it leaves out not being taken from the collector's, and is held when the
file loads to what the driver can honour with the collector's `options`, as
the collector's is: a block beside an option that switches TLS off, or a
`ca_file` or `server_name` beside one that verifies nothing, is refused.
Written, even as `tls: {}`, it asks for TLS as the collector's would; a
target cannot turn off the TLS its collector asks for. Its files are read
when a connection is made, and by [`--dry-run`](CONFIGURATION.md#dry-run),
under the target's name. A target's account, credential files and block are
part of the key of its [pool](#connections) and of its
[cache key](CONFIGURATION.md#response-caching), so targets that connect
differently share no connection and no answer.

## Errors and self-metrics

A failed statement is reported in the `sql` stage and follows
`error_handling.on_fetch_error`, and names the server unless its error does:
`running the statement: ERROR: relation "jobs" does not exist (SQLSTATE 42P01)
(on db1.example.com:5432)`. A statement over `max_rows`, or rows over the
response limit, count in `http_exporter_series_limit_exceeded_total`; a server
the target lists refuse counts in `http_exporter_targets_refused_total`,
answered `403`.

Each `sql` collector has, with no per-request series (see
[Self-metrics](SELF-METRICS.md)):

| Family | Type | What |
| --- | --- | --- |
| `http_exporter_sql_connections` | gauge | Connections its pools have open, in use or idle, to every target, those of pools a reload closed while scrapes were using them among them. |
| `http_exporter_sql_idle_connections` | gauge | Connections its pools keep idle. |
| `http_exporter_sql_targets` | gauge | Targets it has pools open for, each server or account once whatever databases and accounts it reached there, and a static target with settings of its own once more. |
| `http_exporter_sql_connects_total` | counter | Connections it made; one that keeps rising is connections made again and again. |
| `http_exporter_sql_keepalive_failures_total` | counter | Failed pings, with `keepalive.check: ping`. |
| `http_exporter_sql_limit_waits_total` | counter | Scrapes that waited at `max_per_target`. |
| `http_exporter_sql_limit_failures_total` | counter | Scrapes that failed at `max_per_target` or `max_targets`. |
| `http_exporter_sql_rows` | gauge | Rows its last successful statement returned. |

A [debug probe](CONFIGURATION.md#debug-probes) shows the statement in its
request section — the driver, the server and the database, the account,
whether the connection was made for the probe or kept, and how many values
were bound — and never the password, the values or the statement's text; its
response section shows the rows as the JSON above:

```text
Requests
  1. QUERY postgres://db1.example.com:5432/app -> returned 2 rows of the columns state, jobs
     Sql-Args: 1
     Sql-Connection: reused
     Sql-Database: app
     Sql-Driver: postgres
     Sql-User: metrics
```

With [verbose self-metrics](SELF-METRICS.md#verbose-per-request-self-metrics)
a statement is labelled with that `driver://host:port/database` URL, the
database's placeholders kept as they are, and `http_method="QUERY"`.

## Building with more drivers

The default build and the image carry `mysql`, `postgres` and `sqlserver`.
`oracle`, `clickhouse`, `snowflake` and `vertica` are built in when asked for,
by `SQL_DRIVERS`, a comma-separated list:

```sh
make build SQL_DRIVERS=oracle,snowflake            # bin/prometheus-universal-exporter
docker build --build-arg SQL_DRIVERS=clickhouse .
```

`SQL_DRIVERS` goes with `REQUEST_TYPES` (see [Choosing request types at build
time](CONFIGURATION.md#choosing-request-types-at-build-time)) when that names
`sql`, and is refused with a `REQUEST_TYPES` that leaves `sql` out. A build
without the `sql` type links no driver at all. The drivers are most of the
type's size: built with `-trimpath -ldflags="-s -w"`, as the image is, the
exporter is 23.7 MB without `sql`, 29.8 MB with its three default drivers and
76.0 MB with all seven, Snowflake's the largest by far. A collector naming a driver the
build does not have is refused when the configuration loads:

```text
collector "warehouse" uses sql driver "snowflake", which this build does not include; it was built with: mysql, postgres, sqlserver. Build with SQL_DRIVERS=snowflake (make build SQL_DRIVERS=snowflake, or the image's SQL_DRIVERS build argument)
```

`--version`, the startup log and `http_exporter_build_info`'s `sql_drivers`
label name the drivers a build has; of a build without the `sql` type,
`--version` and the label say nothing of drivers.

## Limitations

- **SQL Server and Snowflake run the statement as it is**, without a
  read-only transaction; their accounts must be ones that can only read.
- **A Snowflake result must come in one chunk.** Snowflake sends the first
  part of a result with the answer and leaves a larger result's other chunks
  at presigned cloud storage URLs, which the exporter does not connect to:
  they are not the account's host, and the target lists cannot allow them.
  Such a statement fails at once — `running the statement: the statement's
  result came from Snowflake in more than one chunk, ...; return fewer rows
  or columns, with WHERE, GROUP BY or LIMIT, so that the whole result comes
  in one chunk` — rather than at the scrape's deadline. Aggregate in the
  statement, so that it returns the few rows a metric needs. The exporter
  fails it when the answer naming the chunks comes, before gosnowflake sees
  it, so that the driver never starts fetching them: gosnowflake (v1.19.1)
  can miss the signal of a chunk download that failed and wait for the chunk
  forever, past the scrape's deadline, holding the statement's connection
  and its place at `max_per_target`. To tell, the exporter reads each answer
  to a statement whole before the driver does, the result's first chunk
  with it, which is held twice for a moment: the exporter's copy and the
  driver's. That read is held to the response limit (below).
- **gosnowflake's own behaviour.** Its platform detection, which would ask
  the cloud metadata services and AWS STS which platform it runs on, is
  turned off (`SNOWFLAKE_DISABLE_PLATFORM_DETECTION=true`, set by the
  exporter). The build leaves out its native "minicore" library
  (`minicore_disabled`, which `SQL_DRIVERS=snowflake` adds), so it writes no
  library to a temporary directory or `~/.cache/snowflake/minicore` at start.
  What remains is the driver's: at start it creates an empty
  `~/.cache/snowflake` (or `$XDG_CACHE_DIR/snowflake`, or
  `$SF_TEMPORARY_CREDENTIAL_CACHE_DIR`) where it can, and stores nothing in
  it, the exporter keeping no credential there; at the first connection it
  looks for an `sf_client_config.json` beside the executable and in the home
  directory, or at `$SF_CLIENT_CONFIG_FILE`, and logs as one says.
- **Vertica's driver dials by itself**, taking neither a dialer nor a
  context: the target's name and every address it resolves to are checked
  against the target lists before the connection, not the connection's
  address after it, and a name the exporter cannot look up fails the scrape
  then, `has to be looked up here to hold its addresses to the target lists
  before the driver, which dials by itself, connects`, since the driver would
  look it up itself; its TCP keepalives are the driver's own; and a connection
  not made by the scrape's deadline is left to finish, then closed. The
  driver reads with no deadline and would stop a statement only by a cancel
  request sent over a connection it dials itself — after a lookup of its own,
  with no timeout, past the target lists, and unencrypted whatever `tlsmode`
  says. So the exporter never lets it send one: at the scrape's deadline the
  scrape fails, `the vertica server had not answered when the scrape's
  deadline came`, and the connection is given up, closed when the driver's
  call returns, while the server runs the statement to its end. Bound it at
  the server, with the resource pool's `RUNTIMECAP`.
- **SQL Server routing is refused.** A server that answers the login by
  sending the connection to another server — Azure SQL's `Redirect`
  connection policy, read-only routing in an availability group — names a
  host the target is not, and the exporter's dialer refuses the connection
  to it: the scrape fails with `the connection is to a host the request did
  not check`. Use Azure SQL's `Proxy` connection policy, or make the server
  the connection is routed to the target.
- **Vertica's driver logs by itself**: its warnings and errors go into the
  exporter's log, lines with `sql_driver` `vertica`, at their own level, not
  to stdout or the file `VERTICA_SQL_GO_LOG_FILE` names;
  `VERTICA_SQL_GO_LOG_LEVEL` still says how much it logs.
- **Snowflake's driver logs by itself** too: its errors go into the
  exporter's log, lines with `sql_driver` `snowflake`, at their own level
  and with the passwords and tokens it masks masked, not to stderr; an
  `sf_client_config.json` it finds (above) sends them where it says instead.
  The error it logs for a statement whose result came in more than one
  chunk, which the exporter fails (above) — `error: 262000 (08006): the
  statement's result came from Snowflake in more than one chunk, ...` — and
  the one for an answer larger than the response limit (below) — `error:
  262000 (08006): the statement's result came from Snowflake larger than the
  exporter's response limit` — are logged at debug: the scrape's own failure
  says what happened.
- **A row is read before `max_response_bytes` sees it.** The limit counts
  the values as a driver hands them on, and a driver reads a row, at least,
  whole before it hands any of it on. Where a driver can be told, the
  exporter bounds what it reads by the limit:
  - `postgres` refuses a message from the server, a row with all its values
    among them, that is larger than the limit and 1 MiB beside it (for a
    row's lengths and `NULL`s, a wide result's description and a long error
    message) from its length alone, before reading it, and closes the
    connection: `the server sent a message larger than the response limit
    of 1048576 bytes and the 1048576 bytes the protocol is allowed beside
    it, as a row too large for the limit is; return smaller values, or raise
    request.max_response_bytes or limits.max_response_bytes`. Unbounded,
    pgx would read a message of up to 1 GiB.
  - `sqlserver` sets `TEXTSIZE` to twice the limit and two bytes on each
    connection, when it is made and before each use of it, which costs a
    round trip: the server cuts a longer `varchar(max)`, `nvarchar(max)`,
    `varbinary(max)`, `text`, `ntext` or `image` value there, and since a cut
    value is still over the limit as the exporter reads it (`nvarchar` text
    is two bytes a character as sent), the scrape fails by the limit and
    never hands a cut value on. Other values are 8000 bytes at most, but `xml`
    and CLR types such as `geography`, which `TEXTSIZE` does not cover, and
    which the driver reads whole.
  - `mysql` reads a row whole, as long as the server sends it, which the
    server's `max_allowed_packet` bounds (64 MiB by default, 1 GiB at most);
    the driver cannot be told less, its own `maxAllowedPacket` bounding what
    it sends alone. Nor can the exporter read the packets' lengths on the
    connection it hands the driver, as it does `postgres`'s messages: with
    `tls`, the driver starts TLS itself on that connection, and the packets
    are inside it, where only the driver sees them; a bound that held without
    TLS and not with it is not given.
  - `snowflake`'s answer to a statement, which holds the result's first
    chunk, the only one the exporter takes (above), is read by the exporter
    before the driver sees it, and that read is held to the limit: an answer
    whose `Content-Length` passes the limit is refused unread, one without
    a `Content-Length` once a byte past the limit has been read, and a
    gzipped one, however small, once a byte past the limit has been
    decompressed, without decompressing further, so that a gzip bomb costs
    no more than the limit. The scrape fails at once, as a limit, not the
    driver's error, and the driver does not retry it: `running the
    statement: the statement's answer from Snowflake was larger than the
    response limit of 1048576 bytes (decompressed, when it came gzipped),
    and was refused; return fewer rows or columns, with WHERE, GROUP BY or
    LIMIT, or raise request.max_response_bytes or limits.max_response_bytes`.
    A gzipped answer within the limit is handed to the driver decompressed,
    in place of the gzipped one. So the exporter holds one copy of an answer
    of at most the limit, and the driver another while it decodes it: what
    the driver builds of that copy as it decodes it (its rows, a string for
    each value) is the driver's, and the limit on the rows' values then
    applies as for every driver. The login and the driver's other requests,
    whose answers are not results, are not bounded.
  - `clickhouse` and `vertica` read a row, or a block of rows as their
    protocol sends it (`clickhouse`'s blocks), whole, with no bound the
    exporter sets.
- **No connection string.** A setting a driver has that no key or option
  above names cannot be set.

## Troubleshooting

| The scrape says | Do |
| --- | --- |
| `... uses sql driver "...", which this build does not include` | Build with `SQL_DRIVERS` naming it (see [Building with more drivers](#building-with-more-drivers)). |
| `connecting to postgres at ...: ... password authentication failed` | The account or its password; check `password_file`. |
| `request.password_file ... is empty` | The file is there and empty: the Secret's key, or its mount. |
| `running the statement: the statement's result came from Snowflake in more than one chunk, ...` | Return fewer rows or columns (see [Limitations](#limitations)). |
| `running the statement: the statement's answer from Snowflake was larger than the response limit ...` | Return fewer rows or columns, or raise `max_response_bytes` (see [Limitations](#limitations)). |
| `running the statement: ... read-only transaction`, `... READ ONLY transaction` | The statement writes; it ran read-only, as it always does where the database can. |
| `running the statement: the scrape's deadline came first` | The statement takes longer than the probe may; make it cheaper, or raise the probe's `timeout`. |
| `column 1 of the statement's result has no name` | Name the column with `AS`. |
| `the statement's result names column "..." twice` | Give one of them another name with `AS`. |
| `... holds binary data that is no UTF-8 text` | Return the column as text in the statement, its hexadecimal digits say. |
| `the statement returned more than request.max_rows` | Return fewer rows, with `WHERE`, `GROUP BY` or `LIMIT`, or raise `max_rows`. |
| `... has request.connections.max_per_target, ..., and none was free` | The collector's scrapes of the target hold every connection it may open; raise the limit, or the scrape's timeout. |
| `... pools open to ..., one for each database and account scraped there, each in use by a scrape` | Probes reach more of the target's databases or accounts at once than it keeps pools for, `max_per_target` or 64 without one; raise `max_per_target`, or probe fewer at once. |
| `request.private_key_file ... is encrypted` | Write the key unencrypted (see [Authentication](#authentication)). |
