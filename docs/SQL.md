# Databases over SQL

The `sql` request type runs a statement on a database, or several in order
([Several statements](#several-statements)), and turns the rows they return
into metrics. Use it for what a database knows that no dedicated exporter
shows:

- the jobs of a queue table by state, the age of the oldest unprocessed row;
- the time of the last successful import, written by the importer itself;
- the counts that a business dashboard shows, from the application's own
  tables.

Three drivers are built in with the type: `mysql` (MySQL and MariaDB),
`postgres` (PostgreSQL and the databases that speak its protocol) and
`sqlserver` (Microsoft SQL Server and Azure SQL). Four more, `oracle`,
`clickhouse`, `snowflake` and `vertica`, are only in a build that asks for
them, because they bring large dependencies (see
[Building with more drivers](#building-with-more-drivers)).

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
| `statement` | — | **Required**, unless `statement_file` names the file of it or `statements` lists several. The one statement run, with the driver's own placeholders for `args` (see [The statement](#the-statement)). Not blanks alone. |
| `statements` | — | Several statements, each with its `name`, `statement`, `args` and `arg_types`, in place of `statement`, `statement_file`, `args` and `arg_types`, run in order on one connection (see [Several statements](#several-statements)). At most 16. |
| `statement_file` | — | The absolute path of a file holding the statement, in place of `statement` (see [A statement in a file](#a-statement-in-a-file)). |
| `args` | — | The values bound to the statement's placeholders, in order, as text unless `arg_types` says otherwise. May use [placeholders](REQUESTS.md#path-parameters), `{{param_name}}` and `{{param_name:default}}`. |
| `arg_types` | all `text` | The type each of `args` is bound as, one for each, in their order: `text`, `int`, `float` or `bool` (see [Typed args](#typed-args)). |
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

No other request key applies, and setting one (`method`, `headers`,
`retry`, `passphrase_file`) is a startup error. The file keys name files on
the exporter's own filesystem, such as a mounted Secret, and take no
placeholders. They are read each time a connection is made, not when the
configuration loads: the next connection sends a password replaced on disk,
and a file that is missing or empty fails the scrape with an error that
names it. [`--dry-run`](CONFIGURATION.md#dry-run) reads them as a connection
would, and warns about each one that a scrape would fail on now.

## The target

A probe's target, and a static target's, is the database server: `host` or
`host:port`, or an IPv6 address in brackets (`[2001:db8::1]:5432`). Without
a port, the driver's default port is used. A target that ends in a colon
with no port after it, `db1.example.com:` or `[::1]:`, is refused (a static
target's when the file loads, a probe's on the scrape): `target
"db1.example.com:" ends in a colon with no port after it; write the port
after the colon, or leave the colon out for the driver's port`.

| Driver | Port |
| --- | --- |
| `postgres` | 5432 |
| `mysql` | 3306 |
| `sqlserver` | 1433 |
| `oracle` | 1521 |
| `clickhouse` | 9000 native, 9440 native with TLS, 8123 http, 8443 http with TLS |
| `vertica` | 5433 |
| `snowflake` | — the target is the account identifier |

A `snowflake` target is the account identifier, `myorg-myaccount` or a
locator such as `xy12345.eu-west-1`, reached at
`<account>.snowflakecomputing.com` over HTTPS. A connection to it that the
target lists refuse fails the scrape immediately with their refusal, as with
every driver, where the driver itself would retry it until the scrape's
deadline.

Nothing else may come with a target: no scheme, no user, no database, no
options. `postgres://u:p@db/app?sslmode=disable` is answered `400`. These
are the collector's keys, so a probe can reach only the databases and
accounts that the configuration names. A collector's `allowed_targets` and
`denied_targets` apply to the server's name and to every address that a
connection reaches, which the exporter's own dialer checks for every driver
except `vertica` (see [Limitations](#limitations)).

## The statement

`statement` is one statement, written in the database's own SQL, with the
driver's own placeholders for the values in `args`:

| Driver | Placeholders |
| --- | --- |
| `postgres` | `$1`, `$2`, … |
| `mysql`, `clickhouse`, `snowflake`, `vertica` | `?` |
| `sqlserver` | `@p1`, `@p2`, … |
| `oracle` | `:1`, `:2`, … |

`args` are sent to the database as values, never as SQL. `postgres`,
`mysql`, `sqlserver`, `oracle`, `snowflake` and `vertica` bind them to the
placeholders at the server, while the `clickhouse` driver writes each into
the statement before sending it, quoted as a literal of its type. Either
way, a probe parameter in one cannot change the statement, whatever it
holds. A placeholder in `statement` itself is a startup error: a value from
the probe goes in `args`.

`vertica`'s driver also writes the values into the text of a statement that
it does not prepare, doubling a `'` but leaving a `\` as it is. In an
`E'...'`, or with `standard_conforming_strings` off, that `\` escapes the
closing quote, so a value ending in `\` would end the string at the next
quote and the rest would be SQL. A `vertica` statement that the driver
would not prepare is therefore a startup error:

- any `COPY`. A `COPY ... FROM LOCAL` is the one that the driver sends
  unprepared, and it would read a file of the exporter's host into the
  database at every scrape. Every `COPY` loads rows into a table, which no
  metrics query does and the read-only transaction is there to refuse. The
  exporter finds the `COPY` both as the driver does and as the server does,
  in any letter case. The driver takes the first word outside quotes,
  comments and dollar quotes, with block comments not nested: to it,
  `/* /* */ COPY t FROM LOCAL 'f' */ SELECT ?` is a `COPY ... FROM LOCAL`,
  although it is a `SELECT` to the server;
- a function's DDL or an `EXECUTE` grant on one (`CREATE`, `ALTER` or
  `DROP FUNCTION`, `GRANT` or `REVOKE EXECUTE ON FUNCTION`), which the
  driver sends unprepared, with no value bound.

Every other statement is prepared at the server, with the exporter setting
the driver's `use_prepared_statements=1`, and its values are bound to it.

**Read-only.** The statement runs read-only where the database can make it
so:

- `postgres`, `mysql` and `vertica`: in a transaction begun `READ ONLY`,
  rolled back afterwards;
- `oracle`: after `SET TRANSACTION READ ONLY`, in a transaction rolled back
  afterwards;
- `clickhouse`: with the setting `readonly=1`;
- `sqlserver`, which has no read-only transaction (its driver refuses one):
  in a transaction that is always rolled back. That undoes what the
  statement wrote (rows, and the DDL that SQL Server keeps in a transaction),
  but not what commits by itself, such as a `COMMIT` in the statement.

Snowflake has no read-only transaction either: the statement runs as it is.
Give every collector an account that can read what its statements read and
nothing more, whatever the database, and above all for SQL Server and
Snowflake: read-only is a second line of defence, not the first.

**One statement.** `postgres` sends every statement with the extended
protocol, bound even without `args`, so the server refuses two statements
joined with `;`. `mysql` never allows several statements in one.

`vertica`'s driver would split a statement at each `;` and run the parts one
after another, and a `COMMIT` among them would end the read-only
transaction. A `vertica` statement of several parts (a `;` followed by
anything other than blanks and comments, outside quotes, quoted identifiers,
comments and dollar quotes) is therefore a startup error.

- The exporter decides this itself, as well as by the driver's own
  splitting. Since vertica-sql-go 1.3.9, the driver does not split at a `;`
  that it considers to be inside a `BEGIN ... END`, `CASE`, `IF`, `LOOP` or
  `WHILE` block. It sends such text to the server whole, and the server runs
  every statement in it, even `SELECT 1 AS begin; COMMIT`.
- A `//` is not a comment to either, so a `;` followed by `// ...` is
  refused too.
- The exporter reads the text in every way that the server may, which is not
  always the driver's way, and refuses it if any reading finds several
  statements:
  - a `$` inside a word is part of the word, not a dollar quote
    (`SELECT a$b$; COMMIT; SELECT $b$x$b$` is three statements);
  - a `\` escapes the next character in an `E'...'`, and in every `'...'`
    where `standard_conforming_strings` is off
    (`SELECT E'\''; COMMIT; --'` is two);
  - block comments may nest;
  - only ASCII white space counts as blank.
- A statement that the driver's lexer fails on is a startup error too,
  because the driver would fail on it at every scrape. An example is a `?`
  right after a quote at the start of the text, or at the start of a line
  after a `--` comment, as in `'?'`.

The other drivers send what is written, and a statement of several parts is
for the database to run or refuse: another reason for an account that can
only read.

**Deadlines.** The statement runs within the probe's deadline, its `timeout`
parameter or Prometheus's scrape timeout (see
[Probe deadlines](CONFIGURATION.md#probe-deadlines)), including the
connection. When the deadline comes, the scrape fails with `running the
statement: the scrape's deadline came first`, and the driver stops the
statement:

- `postgres` and `sqlserver` ask the server to cancel it;
- `clickhouse` and `snowflake` abort it, and `oracle` breaks it off;
- `mysql` closes the connection, and the server ends its statement when it
  notices;
- `clickhouse` on `http` aborts it by ending its request, and sends every
  request with the setting `cancel_http_readonly_queries_on_client_close=1`,
  which makes the server stop the statement then;
- `vertica`'s connection is given up at the deadline, and the server is not
  asked to stop the statement (see [Limitations](#limitations)).

The server is also told how much time the scrape has left, in the
transaction before the statement, so that it ends a statement that nobody is
waiting for even if the cancel is lost, or, with `mysql`, never sent:

- `postgres`: `SET LOCAL statement_timeout`, in milliseconds.
- `mysql`: `SET SESSION max_execution_time`, in milliseconds, on MySQL 5.7.8
  and later, where it bounds a `SELECT`, and
  `SET SESSION max_statement_time`, in seconds, on MariaDB 10.1.1 and later.
  The server's kind and version are queried once per connection,
  `SELECT VERSION()`, when its first statement runs.
  - A server older than its variable, or whose version reads as neither
    MySQL's nor MariaDB's, is set nothing, instead of having every scrape
    refused over a variable it does not know.
  - A server can report a version that has the variable and then refuse it
    as unknown (error 1193), as a proxy speaking for another server may. The
    statement is then run without it, and the variable is not set again on
    that connection.
- `sqlserver`: `SET LOCK_TIMEOUT`, in milliseconds, which bounds how long the
  statement waits for a lock that another session holds, where most of a
  metrics query's waiting happens; the cancel at the deadline bounds the
  rest.

A statement that the server ends at that limit fails like one that the
deadline ended, `running the statement: the scrape's deadline came first`,
although the server's clock may end it a moment before the scrape's context
ends. For the other drivers, an account with its own limit
(`STATEMENT_TIMEOUT_IN_SECONDS` in Snowflake, a resource pool's `RUNTIMECAP`
in Vertica, `max_execution_time` in ClickHouse's profile) is the server's
side of the same bound.

A driver that fails on what a server sends (a panic, on an answer it reads
without checking) fails the connection, the statement or the keepalive ping
that it was on, never the exporter, and the connection is closed. That also
holds outside any scrape: for a driver that fails in this way while closing
a connection past its `max_lifetime` or `idle_timeout`, or while rolling back
the transaction of a scrape whose deadline came. One that fails in this way
while connecting holds no place at `max_per_target` afterwards.

### A statement in a file

A long statement reads, and is reviewed, better in its own file.
`statement_file` names the file, instead of `statement`:

```yaml
    request:
      type: sql
      driver: postgres
      username: metrics
      password_file: /etc/exporter/db/password
      statement_file: /etc/exporter/sql/jobs.sql
      args: ["{{param_queue:default}}"]
```

The file is read when the configuration loads, and again at each reload.
With [`--config.watch`](CONFIGURATION.md#watching-the-configuration), an edit
of it reloads at the next tick, as an edit of the configuration does, so the
statement in force is always the one on disk that the configuration was
checked with. A collector whose statement file changed counts as changed in
the reload's log line, and its
[cached results](CONFIGURATION.md#response-caching) are not reused. The file
is checked as the configuration loads, and a file that does not meet these
rules fails the load or the reload, with the reason:

- `statement_file` is an absolute path, written as it is: no blanks around
  it, and no placeholder, which nothing fills in a path read at load time. A
  relative path would be read from whatever directory the exporter started
  in.
- The file exists, is readable by the exporter, and is a regular file. A
  directory, a named pipe (which would hold the load until something wrote
  to it, so it is refused without being opened), a device or a socket is
  refused. A symbolic link to a file is followed, which is how a mounted
  ConfigMap's files are reached.
- It is at most 1 MiB, and UTF-8 text with no NUL byte.
- It holds a statement. A UTF-8 byte order mark at its start and the blanks
  around the statement are removed, and a file of only blanks is refused.
- The statement has no `{{param_...}}` placeholder, as a statement written
  in place may not: a probe's values go in `args`.
- It is held to the same rules as a statement written in place: one
  statement, and for `vertica` one that its driver prepares.

`statement` and `statement_file` are not written together; a `statement`
written `""` is the same as leaving the key out, as for every key.

- A static target uses its collector's statement and sets no file of its
  own.
- The [target allocator](ALLOCATOR.md) checks the key but does not open the
  file, which travels as a path. Mount it on every replica at the same path,
  where each reads and checks it when it loads the bundle.
- The [debug page's playground](CONFIGURATION.md#debugging-collectors) reads
  no file of the server's, and a copy of a collector with a statement file
  leaves the key out.

### Typed args

An arg is bound as text, which most databases convert where the statement
needs a number; PostgreSQL converts it by the type that the placeholder
takes. Where a database does not (MySQL's `LIMIT ?`, for which MySQL
versions before 8.0.22 refuse text, or a comparison of a number with text
that compares them as text), `arg_types` binds the arg as its type:

```yaml
      statement: SELECT id, state FROM jobs WHERE priority >= ? AND paused = ? ORDER BY id LIMIT ?
      args: ["{{param_priority:0.5}}", "false", "{{param_limit:100}}"]
      arg_types: [float, bool, int]
```

| Type | The arg holds | Bound as |
| --- | --- | --- |
| `text` | anything | the text, as written |
| `int` | a whole number in decimal digits, a sign allowed: `-12`, `+7` | a 64-bit integer |
| `float` | a finite decimal number: `0.25`, `1e-3`, `.5` — no `Inf`, `NaN`, hexadecimal float (`0x1p-2`) or `_` | a 64-bit floating-point number |
| `bool` | `true` or `false`, in any case | a boolean |

`arg_types` gives a type for every arg, by position, with `text` for one
bound as text. Another count, or a type other than these four in lower
case, is a startup error. An arg that does not match its type fails as early
as possible:

- one with no placeholder, or whose placeholders all have defaults, when the
  configuration loads, filled with the defaults;
- one that a probe fills, before the server is asked, answered `400`:
  `request.args[2] is "ten", which is no whole number …`;
- one that a static target's `params` fill, when the target file loads.

A parameter left empty counts as not given, so the default fills it. A
database without a boolean type, such as Oracle before 23ai, may refuse a
`bool`: bind `1` or `0` as an `int` there.

### Several statements

A collector runs one statement, so five related metrics of a database need
five collectors, five logins and five round trips per scrape. `statements`
runs several, each with a name, in order, on one connection, and the
document holds each one's rows under its name:

```yaml
    request:
      type: sql
      driver: postgres
      username: metrics
      statements:
        - name: jobs
          statement: SELECT state, count(*) AS n FROM jobs WHERE queue = $1 GROUP BY state
          args: ["{{param_queue:mail}}"]
        - name: oldest
          statement: SELECT extract(epoch FROM now() - min(created)) AS age FROM jobs
        - name: workers
          statement: SELECT host, busy FROM workers LIMIT $1
          args: ["50"]
          arg_types: [int]
    transform:
      type: jq
    metrics:
      - name: jobs
        items: .jobs[]
        expression: .n
        labels:
          - name: state
            expression: .state
      - name: oldest_job_age_seconds
        expression: .oldest[0].age
      - name: workers_busy
        expression: '[.workers[] | select(.busy == 1)] | length'
```

```json
{"jobs": [{"state": "queued", "n": 12}], "oldest": [{"age": 341.5}], "workers": [{"host": "w1", "busy": 1}]}
```

`busy` is a boolean column, which the rows hold as `1` or `0`
([The rows](#the-rows)). Compare it explicitly: jq treats `0` as true, so
`select(.busy)` would keep every row.

- Each entry has a `name` (letters, digits and `_`, not beginning with a
  digit, and unique in the collector), by which a rule reads its rows,
  `.jobs`. It also has its `statement`, written like `statement`, with the
  driver's placeholders for its own `args`, and its `args` and `arg_types`,
  as in [Typed args](#typed-args), each with its own placeholders. There are
  at most 16 entries, and `statement`, `statement_file`, `args` and
  `arg_types` are refused beside them.
- They run in the order written, on one connection of the pool, each in its
  own transaction, rolled back, with its own time limits, like a single
  statement ([The statement](#the-statement)). Each sees the database as it
  is when it begins.
- `max_rows` applies to each statement; the response limit applies to their
  rows together. The first statement that fails fails the scrape, with an
  error that names it, `request.statements[1], oldest: …`, and the
  statements after it do not run.
- The document is an object, which `jq`, `yq` and `python` (`data["jobs"]`)
  read. A `csv` transform, which reads one statement's rows by column, is
  refused at load. `http_exporter_sql_rows` counts the rows of all of them.
- A debug probe shows each statement as its own request, with its name in
  `Sql-Statement`: the first on the connection as it was obtained, and the
  rest on it as `reused`.

## The rows

The rows reach the transforms as a list of objects, one per row, with its
columns by name:

```json
[{"state": "open", "jobs": 3}, {"state": "done", "jobs": 40}]
```

- A `csv` transform reads a column by its name, one series per row, as it
  reads the columns of a CSV file with a header: `expression: jobs` above.
- `jq` and `yq` read the list: `items: .[]` and `expression: .jobs`.
- `python` receives the list as `data`.

No other transform reads rows, and `decoder.type` and the `response` block
do not apply: the driver reads the values, and there is no body to decode.

| The column holds | The transforms see |
| --- | --- |
| an integer or a floating-point number | the number |
| a decimal (`NUMERIC`, `DECIMAL`, `NUMBER`, `MONEY`; Snowflake's `NUMBER`, which its driver names `FIXED`, and `DECFLOAT`), which drivers give as text | the number it writes, or its text when it writes none: PostgreSQL writes `money` with its currency, `$12.34`, so cast it there, `amount::numeric` |
| a boolean | `1` or `0` |
| a date or a timestamp (`DATE`, `DATETIME`, `TIMESTAMP`) | its Unix time in seconds, with the fraction; PostgreSQL's `infinity` and `-infinity`, which are no time, the text |
| a time of day (`TIME`, `TIMETZ`, ClickHouse's `Time` and `Time64`) | the seconds since midnight, with the fraction, as its clock reads where it was written: `13:45:30.5` is `49530.5`; a MySQL `TIME`, a duration, may be negative or past 24 hours, `-01:00:00` is `-3600` |
| text | the text |
| binary data | its text when it is UTF-8; otherwise the scrape fails, naming the column |
| `NULL` | nothing: the column is left out of its row |

A `NULL` is a value that the row does not have. A `jq` rule finds no key, and
a `csv` rule reading the column has no value in that row (a label read from
it is left off that row's series), which its `error_mode` decides for a
value.

- A column that is `NULL` in every row is still a column that the statement
  returned. A `csv` rule reads it as it reads one `NULL` in some rows, not as
  a column that the response lacks (see
  [When a metric cannot be extracted](CONFIGURATION.md#when-a-metric-cannot-be-extracted)).
- A column without a name (`SELECT count(*) FROM jobs` in SQL Server) and two
  columns with one name fail the scrape: name each with `AS`.
- A statement that returns no row leaves every rule without a value, like a
  response with no item.

## Authentication

The account is `username`, and its password is the content of
`password_file`, with surrounding blanks removed, as for every credential
file. A password file that is empty, or that holds a line break inside the
password, fails the scrape. The account and the password reach the driver
as written, whatever they hold: `:`, `@`, `/`, `%` and blanks are part of
them, not separators.

`snowflake` also supports key-pair authentication: `private_key_file` names
an unencrypted RSA private key in PKCS#8 (`-----BEGIN PRIVATE KEY-----`) or
PKCS#1, whose public key is set on the Snowflake user. The exporter cannot
read an encrypted key, so write it unencrypted
(`openssl pkcs8 -topk8 -nocrypt -in rsa_key.p8 -out rsa_key_plain.p8`) and
keep the file readable only by the exporter. A `snowflake` collector needs
`password_file` or `private_key_file`, and not both.

No other authentication is offered: a password is never written in the
configuration itself, and Kerberos, IAM and Azure AD tokens are not used.

## Drivers

Each driver takes a short list of `options`, including the option that
switches its TLS. With that option left out, a collector that writes a `tls`
block uses TLS, with the server's certificate verified against
`tls.server_name` or the target's host, and one that writes none uses no
TLS. A `tls` block counts even when empty: `tls: {}` requests TLS verified
against the system's roots, and beside an option that switches TLS off it is
refused like any other.

| Driver | Options | TLS |
| --- | --- | --- |
| `postgres` | `sslmode`: `disable`, `require` (TLS, the certificate not verified) or `verify-full`; `application_name`, `prometheus-universal-exporter` by default | `sslmode` |
| `mysql` | `tls`: `false`, `true` or `skip-verify` (TLS, the certificate not verified) | `tls` |
| `sqlserver` | `encrypt`: `disable` (no TLS at all), `false` (the login alone, its certificate not verified), `true` or `strict` (TDS 8.0, TLS first) | `encrypt`; `true` with a `tls` block, `false` without |
| `oracle` | `ssl`: `false` or `true` | `ssl`; `tls.server_name` is refused, since the driver checks the certificate against the target's host itself |
| `clickhouse` | `protocol`: `native` or `http`; `compress`: `none`, `lz4` or `zstd`; `secure`: `false` or `true` | `secure` |
| `snowflake` | `warehouse`, `role`, `schema` | always TLS, verified |
| `vertica` | `tlsmode`: `none`, `server` (TLS, the certificate not verified) or `server-strict` | `tlsmode` |

**TLS options and blocks.** A `tls` key beside a TLS option that switches
TLS off is a startup error, because it would be ignored. So are
`tls.ca_file` and `tls.server_name`, which say how the server's certificate
is verified, beside a value that does not verify it (`sslmode: require`,
`tls: skip-verify`, `tlsmode: server`): use the verifying value
(`verify-full`, `true`, `server-strict`), or leave them out. A client
certificate, `cert_file` and `key_file`, is presented either way.

- The TLS files are read each time a connection is made, and that connection
  uses what was read for it, so a replaced file is used from the next
  connection on. `vertica`'s driver takes its TLS settings by a name
  registered with it, and keeps every name forever. It is therefore given
  one name for each connection being made at the same time, reused by later
  ones, so it keeps no more names than that, however often the files change.
- SQL Server encrypts only the login, with a certificate that it creates
  itself unless given one, and its own tools do not verify that certificate.
  Neither does the exporter, with `encrypt: false` or left out, unless a
  `tls` block says how (`ca_file`, `server_name`, or neither for the system's
  roots).
- Unless `encrypt` is `disable`, the login is never sent unencrypted. A
  server that answers that it does not support encryption fails the scrape,
  `the server answered that it does not support encryption, so the login
  would be sent unencrypted`; enable encryption on the server, or write
  `encrypt: disable`. A prelogin answer with an empty `ENCRYPTION` option,
  which no SQL Server sends, fails the connection whatever `encrypt` is.
- `oracle` with `ssl: true` makes every connection TLS. A listener that
  redirects the connection to an address with `PROTOCOL=TCP` makes it fail,
  `the oracle driver began a connection without TLS`, before anything is sent
  on it; have the listener redirect to a `TCPS` address.
- `clickhouse` with `secure: true` makes the TLS handshake before anything
  else, on the native protocol as on `http`.

**No environment.** Each connection is configured from the collector alone.
The `PG*` environment variables and the `.pgpass` file that `postgres` would
read change nothing, except `PGSERVICE`, which the driver reads regardless
and which fails every `postgres` connection until it is unset. Neither do
`HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY`: every driver, including
`clickhouse` on `http` and `snowflake`, connects to the server directly,
never through a [proxy](REQUESTS.md#proxies).

SQL Server's named instances are reached by their port, not by name: finding
one by name would query the SQL Server Browser over UDP, and the exporter's
dialer makes only TCP connections, refusing the driver's `udp` connection
with `the driver asked for a udp connection`.

## Connections

A collector keeps its connections to a target in a pool. There is one pool
for each target and each set of connection settings (account, credential
files, database, options, TLS settings, `connections`, target lists). It is
shared by the collector's scrapes of the target, and with no other
collector. A connection is made at the first scrape that needs one, and kept
for the next.

The limits apply to targets, not pools. A target is the server, `host:port`,
or the Snowflake account, together with the collector's connection
settings. The pools of the databases and accounts that a collector reaches
on one server belong to one target: they are counted once in `max_targets`
and together in `max_per_target`. A [static target](#static-targets) whose
`connections` give it other settings is a target of its own, with its own
limits, even though it names a server that the probes reach.

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

**Broken connections.** A connection that broke is detected by the scrape
that takes it, or by a keepalive, and made again. The same goes for one that
the server ended while it was idle, through PostgreSQL's
`idle_session_timeout`, an administrator's `pg_terminate_backend`, or
MySQL's `wait_timeout`. The scrape that finds it closes it and connects
again, because nothing of its statement was sent, and fails only if the new
connection does. A server that ends idle sessions sooner than the scrape
interval costs a connection every scrape; give the account a longer
timeout, or set `max_idle_per_target: 0`.

**Reloads.** A reload that removes a collector, or changes any of its
connection settings, closes its pools. The next scrape makes a pool with
the new settings. The scrapes that are using an old pool at that moment (a
statement still running, a scrape waiting at `max_per_target`) finish on
it, and it is closed when the last one is done. Their connections count as
the collector's while it is configured, and no longer once a reload removes
it. The exporter's shutdown closes every pool in the same way.

**The end of a statement.** Closing a statement's rows and rolling back its
transaction is bounded by the scrape's deadline, or by a tenth of a second
when the deadline has already passed. A driver may wait on it without a
deadline of its own, as `mysql`'s rollback waits for the server's answer.

- When the deadline comes first, the scrape answers with the rows that it
  has read. The connection's network connection is closed and the
  connection is discarded, never kept; it holds its place at
  `max_per_target` until it is closed.
- A `vertica` connection, which the driver dials itself, cannot be closed
  under the driver: it holds its place until the driver's end returns.

A transaction is rolled back by the scrape, at the statement's end, even
when the deadline comes while it is open. It is never rolled back by Go's
`database/sql` on its own the moment the deadline comes, which would close
the connection while the scrape still has it. A connection whose
transaction was still open when the deadline came, or whose statement was
ended before all its rows were read, is then closed instead of kept. The
exception is a driver that resets the connection's session and reports
whether it is still usable, as `postgres` and `mysql` do.

## Static targets

A [static target](STATIC-TARGETS.md) of a `sql` collector names the server
as its `target`, and may set under its `request`:

- its `timeout`;
- its own account and credential file: `username`, and `password_file` or,
  for `snowflake`, `private_key_file`, either one replacing the collector's
  credential file. None of them may hold a placeholder, which is refused
  when the file loads because no probe fills it;
- its own `tls` block;
- `connections`, whose keys each replace the collector's, except
  `max_targets`, which applies to the collector across its targets. A target
  whose `connections` change a setting is a separate target for the limits,
  counted once more in `max_targets`.

`params` fills the collector's placeholders, in `args` and `database`:

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

A target's `tls` replaces the collector's whole block for that target; a
key that it leaves out is not taken from the collector's.

- When the file loads, the block is checked against what the driver can
  honour with the collector's `options`, as the collector's block is. A
  block beside an option that switches TLS off, or a `ca_file` or
  `server_name` beside one that verifies nothing, is refused.
- A written block, even `tls: {}`, requests TLS as the collector's would. A
  target cannot turn off the TLS that its collector requests.
- Its files are read when a connection is made, and by
  [`--dry-run`](CONFIGURATION.md#dry-run), under the target's name.
- A target's account, credential files and block are part of the key of its
  [pool](#connections) and of its
  [cache key](CONFIGURATION.md#response-caching), so targets that connect
  differently share no connection and no answer.

## Errors and self-metrics

A failed statement is reported in the `sql` stage, follows
`error_handling.on_fetch_error`, and names the server unless its error
already does: `running the statement: ERROR: relation "jobs" does not exist
(SQLSTATE 42P01) (on db1.example.com:5432)`. A statement over `max_rows`, or
rows over the response limit, count in
`http_exporter_series_limit_exceeded_total`. A server that the target lists
refuse counts in `http_exporter_targets_refused_total`, and is answered
`403`.

An error that the database itself returned (a refused login, a transaction
it would not begin, a failed statement, an error in the middle of the rows)
is for the operator only. Its message may quote a value of a row, the
statement, the account and the address that the exporter connected from, or
the names of a schema. The exporter's log shows the whole message, and so,
with `--web.debug=full`, do a debug probe's report and the
[status page](ENDPOINTS.md#status-page).

The caller of `/probe` gets what the exporter was doing, and the server's
error code as the database documents it:

- `SQLSTATE` for `postgres` and `vertica`;
- the error number for `mysql` (with its `SQLSTATE`), `sqlserver`, and
  `snowflake` (with its `SQLSTATE`);
- the code for `clickhouse`;
- the `ORA-` number for `oracle`.

For example:

```text
collector jobs sql failed: running the statement: the database answered SQLSTATE 42P01; its message is in the exporter's log (on db1.example.com:5432)
collector jobs sql failed: connecting: connecting to mysql at db1.example.com:3306: the database answered error 1045 (SQLSTATE 28000); its message is in the exporter's log
```

A failure that is not the database's answer, such as a refused connection,
a failed TLS handshake or the scrape's deadline, is reported as it is. So is
a failure of `snowflake`'s driver itself, whose errors are numbered from
260000.

Each `sql` collector has these self-metrics, and no per-request series (see
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
request section: the driver, the server and the database, the account,
whether the connection was made for the probe or kept, and how many values
were bound. It never shows the password, the values or the statement's
text. Its response section shows the rows as the JSON above:

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
a statement is labelled with that `driver://host:port/database` URL, with
the database's placeholders kept as they are, and `http_method="QUERY"`.

## Building with more drivers

The default build and the image include `mysql`, `postgres` and
`sqlserver`. `oracle`, `clickhouse`, `snowflake` and `vertica` are built in
when requested with `SQL_DRIVERS`, a comma-separated list:

```sh
make build SQL_DRIVERS=oracle,snowflake            # bin/prometheus-universal-exporter
docker build --build-arg SQL_DRIVERS=clickhouse .
```

`SQL_DRIVERS` goes with `REQUEST_TYPES` (see
[Choosing request types at build time](CONFIGURATION.md#choosing-request-types-at-build-time))
when that names `sql`, and is refused with a `REQUEST_TYPES` that leaves out
`sql`. A build without the `sql` type links no driver at all. The drivers
are most of the type's size. Built with `-trimpath -ldflags="-s -w"`, as the
image is, the exporter is 23.7 MB without `sql`, 29.8 MB with its three
default drivers, and 76.0 MB with all seven, Snowflake's driver being by far
the largest. A collector that names a driver that the build does not have is
refused when the configuration loads:

```text
collector "warehouse" uses sql driver "snowflake", which this build does not include; it was built with: mysql, postgres, sqlserver. Build with SQL_DRIVERS=snowflake (make build SQL_DRIVERS=snowflake, or the image's SQL_DRIVERS build argument)
```

`--version`, the startup log and the `sql_drivers` label of
`http_exporter_build_info` name the drivers that a build has. For a build
without the `sql` type, `--version` and the label say nothing about drivers.

## Limitations

- **SQL Server and Snowflake have no read-only transaction.** SQL Server's
  statement runs in a transaction that is rolled back after it, which does
  not undo a `COMMIT` that the statement makes itself, and Snowflake's runs
  as it is. Their accounts must be able only to read.
- **A Snowflake result must come in one chunk.** Snowflake sends the first
  part of a result with the answer, and leaves the other chunks of a larger
  result at presigned cloud storage URLs. The exporter does not connect to
  those: they are not the account's host, and the target lists cannot allow
  them.
  - Such a statement fails immediately, instead of at the scrape's deadline:
    `running the statement: the statement's result came from Snowflake in
    more than one chunk, ...; return fewer rows or columns, with WHERE, GROUP
    BY or LIMIT, so that the whole result comes in one chunk`. Aggregate in
    the statement, so that it returns the few rows that a metric needs.
  - The exporter fails it when the answer naming the chunks arrives, before
    gosnowflake sees it, so that the driver never starts fetching them.
    gosnowflake (v1.19.1) can miss the signal of a failed chunk download. It
    would then wait for the chunk forever, past the scrape's deadline,
    holding the statement's connection and its place at `max_per_target`.
  - To detect this, the exporter reads each answer to a statement whole,
    including the result's first chunk, before the driver does, so it is
    briefly held twice: the exporter's copy and the driver's. That read is
    held to the response limit (below).
- **gosnowflake's own behaviour.** Its platform detection, which would ask
  the cloud metadata services and AWS STS which platform it runs on, is
  turned off (`SNOWFLAKE_DISABLE_PLATFORM_DETECTION=true`, set by the
  exporter). The build leaves out its native "minicore" library
  (`minicore_disabled`, which `SQL_DRIVERS=snowflake` adds), so it writes no
  library to a temporary directory or to `~/.cache/snowflake/minicore` at
  start. What remains is the driver's own behaviour: at start it creates an
  empty `~/.cache/snowflake` (or `$XDG_CACHE_DIR/snowflake`, or
  `$SF_TEMPORARY_CREDENTIAL_CACHE_DIR`) where it can, and stores nothing in
  it, because the exporter keeps no credential there. At the first
  connection it looks for an `sf_client_config.json` next to the executable
  and in the home directory, or at `$SF_CLIENT_CONFIG_FILE`, and logs as
  that file says.
- **Vertica's driver dials by itself**, taking neither a dialer nor a
  context.
  - The target's name and every address it resolves to are checked against
    the target lists before the connection, not the connection's address
    after it. A name that the exporter cannot look up fails the scrape then,
    `has to be looked up here to hold its addresses to the target lists
    before the driver, which dials by itself, connects`, because the driver
    would look it up itself.
  - Its TCP keepalives are the driver's own, and a connection that is not
    made by the scrape's deadline is left to finish and then closed.
  - The driver reads with no deadline. It would stop a statement only by a
    cancel request sent over a connection that it dials itself, after its
    own lookup, with no timeout, bypassing the target lists, and
    unencrypted whatever `tlsmode` says. The exporter therefore never lets
    it send one. At the scrape's deadline the scrape fails, `the vertica
    server had not answered when the scrape's deadline came`, and the
    connection is given up and closed when the driver's call returns, while
    the server runs the statement to its end. Bound it at the server, with
    the resource pool's `RUNTIMECAP`.
- **Vertica's driver does what the server asks of the exporter's files.**
  vertica-sql-go answers the server's file-transfer requests on any
  statement, prepared or not. The exporter cannot stop it, because the
  driver dials by itself and takes no connection that the exporter could
  watch.
  - A request to load a file makes it open whatever path the server names
    and send its contents.
  - A request to check files makes it report which paths exist and how large
    they are.
  - A request to load from standard input makes it read the exporter's.

  As a result, a server that answers at a `vertica` target can read any file
  the exporter's process can read: its credential files, a Kubernetes service
  account token, its configuration. Point `vertica` collectors only at
  servers you trust, in `allowed_targets` (see
  [Restricting targets](REQUESTS.md#restricting-targets)), never at targets
  that a probe's caller may choose freely, and run the exporter with no more
  files readable than it needs. The driver is opt-in (`SQL_DRIVERS=vertica`),
  so a build without it has no such driver.
- **SQL Server routing is refused.** A server can answer the login by
  redirecting the connection to another server (Azure SQL's `Redirect`
  connection policy, or read-only routing in an availability group). That
  names a host that the target is not, and the exporter's dialer refuses the
  connection to it: the scrape fails with `the connection is to a host the
  request did not check`. Use Azure SQL's `Proxy` connection policy, or make
  the server that the connection is routed to the target.
- **Vertica's driver logs by itself.** Its warnings and errors go into the
  exporter's log, as lines with `sql_driver` `vertica`, at their own level,
  not to stdout or to the file that `VERTICA_SQL_GO_LOG_FILE` names.
  `VERTICA_SQL_GO_LOG_LEVEL` still sets how much it logs.
- **Snowflake's driver logs by itself** too. Its errors go into the
  exporter's log, not to stderr, as lines with `sql_driver` `snowflake`, at
  their own level, and with the passwords and tokens that it masks masked.
  An `sf_client_config.json` that it finds (above) redirects them as it
  says. Two of its errors are logged at debug level, because the scrape's own
  failure explains what happened:
  - for a statement whose result came in more than one chunk, which the
    exporter fails (above): `error: 262000 (08006): the statement's result
    came from Snowflake in more than one chunk, ...`;
  - for an answer larger than the response limit (below): `error: 262000
    (08006): the statement's result came from Snowflake larger than the
    exporter's response limit`.
- **A row is read before `max_response_bytes` sees it.** The limit counts
  the values as a driver hands them on, and a driver reads a row, at least,
  whole before it hands any of it on. Where a driver can be told, the
  exporter bounds what it reads by the limit:
  - `postgres` refuses a message from the server that is larger than the
    limit plus 1 MiB (the extra is for a row's lengths and `NULL`s, a wide
    result's description, and a long error message). That includes a row
    with all its values. It refuses it by its length alone, before reading
    it, and closes the connection: `the server sent a message larger than
    the response limit of 1048576 bytes and the 1048576 bytes the protocol
    is allowed beside it, as a row too large for the limit is; return
    smaller values, or raise request.max_response_bytes or
    limits.max_response_bytes`. Unbounded, pgx would read a message of up to
    1 GiB.
  - `sqlserver` sets `TEXTSIZE` to twice the limit plus two bytes on each
    connection, when it is made and before each use, which costs a round
    trip. The server cuts a longer `varchar(max)`, `nvarchar(max)`,
    `varbinary(max)`, `text`, `ntext` or `image` value there. A cut value is
    still over the limit as the exporter reads it (`nvarchar` text is two
    bytes per character as sent), so the scrape fails by the limit and never
    hands on a cut value. Other values are at most 8000 bytes, except `xml`
    and CLR types such as `geography`, which `TEXTSIZE` does not cover and
    which the driver reads whole.
  - `mysql` reads a row whole, as long as the server sends it, which the
    server's `max_allowed_packet` bounds (64 MiB by default, 1 GiB at most).
    The driver cannot be told less: its own `maxAllowedPacket` bounds only
    what it sends. Nor can the exporter read the packets' lengths on the
    connection that it gives the driver, as it does for `postgres`'s
    messages. With `tls`, the driver starts TLS itself on that connection,
    and the packets are inside it, where only the driver sees them. A bound
    that held without TLS but not with it is not offered.
  - `snowflake`'s answer to a statement holds the result's first chunk (the
    only one that the exporter takes, above). The exporter reads it before
    the driver sees it, and that read is held to the limit:
    - an answer whose `Content-Length` exceeds the limit is refused unread;
    - one without a `Content-Length` is refused once a byte beyond the limit
      has been read;
    - a gzipped one, however small, is refused once a byte beyond the limit
      has been decompressed, without decompressing further, so that a gzip
      bomb costs no more than the limit.

    The scrape fails immediately, as a limit, not with the driver's error,
    and the driver does not retry it: `running the statement: the
    statement's answer from Snowflake was larger than the response limit of
    1048576 bytes (decompressed, when it came gzipped), and was refused;
    return fewer rows or columns, with WHERE, GROUP BY or LIMIT, or raise
    request.max_response_bytes or limits.max_response_bytes`. A gzipped
    answer within the limit is passed to the driver decompressed. The
    exporter therefore holds one copy of an answer of at most the limit, and
    the driver another while it decodes it. What the driver builds from that
    copy as it decodes it (its rows, a string for each value) is the
    driver's, and the limit on the rows' values then applies as for every
    driver. The login and the driver's other requests, whose answers are not
    results, are not bounded.
  - `clickhouse` and `vertica` read a row, or a block of rows as their
    protocol sends it (`clickhouse`'s blocks), whole, with no bound set by
    the exporter.
- **No connection string.** A setting a driver has that no key or option
  above names cannot be set.

## Troubleshooting

| The scrape says | Do |
| --- | --- |
| `... uses sql driver "...", which this build does not include` | Build with `SQL_DRIVERS` naming it (see [Building with more drivers](#building-with-more-drivers)). |
| `connecting to postgres at ...: ... password authentication failed` | The account or its password; check `password_file`. |
| `... the database answered SQLSTATE 28P01; its message is in the exporter's log`, `... error 1045 ...`, `... error 18456 ...`, `... ORA-01017 ...` | The database refused the login: the account or its password; check `password_file`. Any other code: the message is in the exporter's log. |
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
