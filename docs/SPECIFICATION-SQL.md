# Prometheus Universal Exporter — `sql` Request Type Specification

This is the `sql` request type's section of § 5.1 of the
[exporter specification](SPECIFICATION-EXPORTER.md), in a document of its
own so that a change to the type is a change to this document. Everything
the request types share — the registry and the rules every type is held to
(§ 5.1), building without a type, decoding, transforms, error policies,
limits, caching, static targets and exposition — is in the exporter
specification, and a section number here, such as § 42.14, is one of its
sections. What a user reads of the type is [SQL.md](SQL.md).

## `sql`

A `sql` collector runs one statement on a database, the probe's `target`,
and MUST hand the rows it returns to the transforms as the values of a list
of objects, one a row, of its columns by name. Its keys:

| Key | Default | Rule |
| --- | --- | --- |
| `driver` | none | **Required**: `mysql`, `postgres`, `sqlserver`, `oracle`, `clickhouse`, `snowflake` or `vertica`, matched without regard to case or surrounding whitespace, by the loader and the schema alike. |
| `statement` | none | **Required**, not blanks alone. |
| `args` | none | Text, each MAY hold `{{param_...}}` placeholders. |
| `database` | none | Not blanks alone; MAY hold placeholders; not with `oracle`. |
| `service_name` | none | **Required** with `oracle`, refused with any other driver. |
| `username` | none | **Required**, not blanks alone, no control character. |
| `password_file` | none | A file read when a connection is made. |
| `private_key_file` | none | `snowflake` only, not beside `password_file`; one of the two MUST be set for `snowflake`. |
| `tls` | none | The `tls` block of § 4. |
| `options` | none | The driver's allow-listed options and values (below). |
| `max_rows` | `10000` | Not negative. |
| `max_response_bytes` | limit | § 20. |
| `connections` | defaults | Below. |
| `allowed_targets`, `denied_targets` | none | § 26.1. |

- **Drivers.** `mysql` (go-sql-driver/mysql), `postgres` (pgx, through its
  `database/sql` adapter) and `sqlserver` (go-mssqldb) MUST be built in with
  the type. `oracle` (go-ora), `clickhouse` (clickhouse-go), `snowflake`
  (gosnowflake) and `vertica` (vertica-sql-go) MUST be built in only with
  their `sql_driver_<name>` tag as well as the type's. Each driver MUST live
  in `internal/fetch/sqldriver_<name>.go`, the one file that imports its
  module, under the type's constraint and, for an opt-in driver, its tag. A
  collector naming a driver the build left out MUST be refused at load:
  `collector "x" uses sql driver "oracle", which this build does not include;
  it was built with: mysql, postgres, sqlserver. Build with SQL_DRIVERS=oracle
  (make build SQL_DRIVERS=oracle, or the image's SQL_DRIVERS build
  argument)`. `--version`, the startup log line and
  `http_exporter_build_info` (§ 22.0d) MUST name the built drivers; of a
  build without the type, `--version` MUST say nothing of drivers.
- **Target.** The target MUST be the server alone: `host` or `host:port`, an
  IPv6 address in brackets, the driver's port when it names none (5432
  postgres, 3306 mysql, 1433 sqlserver, 1521 oracle, 5433 vertica, 9000
  clickhouse native, 9440 with `secure`, 8123 http, 8443 with `secure`); a
  `snowflake` target MUST be an account identifier, reached at
  `<account>.snowflakecomputing.com:443`. A target holding a scheme, a user,
  a path or a query MUST be refused, `400` for a probe, so that no probe
  names a database, an account or an option the configuration does not. A
  target ending in a colon with no port after it MUST be refused, a static
  target's at load and a probe's on the scrape, before any connection. An
  IPv6 address MUST keep its brackets wherever the target is written again,
  with a port or without. The target is required. The policy (§ 26.1) MUST hold the host's name before a
  connection and, through the exporter's dialer, every address a connection
  reaches, for every driver whose driver takes a dialer; for `vertica`,
  which takes none, the name and every address it resolves to MUST be held
  before the connection, a name whose addresses cannot be looked up MUST
  fail the scrape before it, as the driver would look it up itself, and the
  documentation MUST say so. The `vertica` driver answers a server's
  file-transfer requests (load a file, check files, load from standard
  input) on any statement, which the exporter, not holding the connection,
  cannot refuse: the documentation MUST say that a server at a `vertica`
  target can read any file the exporter's process can, and that such
  collectors are to be held by `allowed_targets` to servers the operator
  trusts. The exporter's dialer MUST make TCP
  connections alone, refusing any other network a driver asks for (UDP to
  the SQL Server Browser) with a sentence saying so. `snowflake`
  MUST make no request to a host other than the account's: its platform
  detection (cloud metadata services, AWS STS) MUST be off, and a result's
  chunks after the first, at cloud storage URLs, MUST NOT be fetched; such a
  result MUST fail the scrape at once, not at the deadline, saying it came in
  more than one chunk and to return fewer rows or columns. The exporter MUST
  fail it from the account's answer naming the chunks, before gosnowflake
  sees the answer, so that the driver's chunk downloader never starts: it
  can miss the signal of a failed download and wait forever, past the
  scrape's deadline, keeping the connection and its place at
  `max_per_target`. A connection to
  the account that the policy refuses MUST fail the connection, or the
  statement it was made for, at once with the policy's refusal, though the
  driver would retry it until the deadline. A build with
  `snowflake` MUST carry gosnowflake's `minicore_disabled` tag, which
  `tools/request-type-tags.sh` MUST give with `sql_driver_snowflake`.
- **Credentials.** `password_file` and `private_key_file` MUST be read each
  time a connection is made, never at load; a file missing, unreadable or
  empty, a password holding a control character, and a key that is no RSA
  key in PKCS#8 or PKCS#1, or is encrypted, MUST fail the scrape naming the
  key and the file and never showing its content. The account and the
  password MUST reach the driver as written, whatever they hold (`:`, `@`,
  `/`, `%`, blanks); for `oracle`, whose go-ora would end the account at a
  `:` its own URL builder leaves unescaped, the exporter MUST build the URL
  itself. The TLS files MUST be read each time a connection is made, and
  that connection MUST use what was read for it; for `vertica`, whose
  driver takes its TLS settings by a registered name and keeps every name
  for good, a name MUST be held by one connection while it is made and
  then used again, so that the driver keeps no more names than connections
  were made at once. No inline password MUST be offered, and `passphrase_file` MUST be refused as a key of another
  type. `--dry-run` MUST read those files, and the TLS files, of every `sql`
  collector, and a static target's own credential file and TLS files, and
  warn of what a scrape would fail on, as for `sshfile`.
- **Options and TLS.** Each driver MUST take only its own options, refusing
  any other naming the ones it takes, and only the listed values of an
  option that has them: `postgres` `sslmode` (`disable`, `require`,
  `verify-full`) and `application_name`; `mysql` `tls` (`false`, `true`,
  `skip-verify`); `sqlserver` `encrypt` (`disable`, `false`, `true`,
  `strict`); `oracle` `ssl` (`false`, `true`); `clickhouse` `protocol`
  (`native`, `http`), `compress` (`none`, `lz4`, `zstd`) and `secure`
  (`false`, `true`); `snowflake` `warehouse`, `role` and `schema`; `vertica`
  `tlsmode` (`none`, `server`, `server-strict`). `sslmode`, `tls`, `encrypt`,
  `ssl`, `secure` and `tlsmode` say whether TLS is used; left out, a
  collector with a written `tls` block, an empty one (`tls: {}`) as much as
  one with keys, MUST use TLS, verified against `tls.server_name` or the
  target's host, and one without none
  (`snowflake` always uses TLS). A value of the option that does not verify
  the certificate MUST skip the verification; a written `tls` block, even an
  empty one, beside a value that uses no TLS, `tls.ca_file` or `tls.server_name` beside a value that does
  not verify the certificate (`postgres` `require`, `mysql` `skip-verify`,
  `vertica` `server`), saying to use the verifying value or leave them out,
  and `tls.server_name` for `oracle`, which sets the name itself, MUST be
  refused at load; `cert_file` and `key_file` MAY be set beside a value that
  does not verify. `sqlserver` with `encrypt` `false` or left
  out and no `tls` block MUST NOT verify the certificate of the login's TLS,
  and with a written `tls` block MUST verify it as the keys say, with the
  system roots for an empty one; unless `encrypt` is
  `disable`, a server answering that it does not support encryption MUST
  fail the scrape, saying to enable encryption on the server or write
  `encrypt: disable`, before the login is sent; and, unless `encrypt` is
  `strict`, whose answer comes inside TLS, a prelogin answer with an empty
  `ENCRYPTION` option, which the driver reads without checking, MUST fail
  the connection with a sentence saying so. `oracle` with `ssl` `true` MUST
  begin every connection with a TLS handshake: one go-ora would begin
  without TLS, as after a listener's redirect to an address with
  `PROTOCOL=TCP`, MUST be closed before anything is written on it, failing
  the connection with a sentence saying so. `clickhouse` with `secure`
  `true` MUST make the TLS handshake, with the collector's TLS settings, on
  the connection the exporter dials before the driver writes to it, on the
  native protocol as on `http`. A connection MUST be configured from the
  collector alone: environment variables and files a driver would read
  (every `PG*` variable pgx reads, and `.pgpass`) MUST change nothing,
  `PGSERVICE` aside, which pgx reads whatever it is told and which MUST
  fail the connection saying to unset it, and a driver MUST
  NOT connect through a proxy: `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY`
  MUST change nothing for `clickhouse` on `http` or for `snowflake`.
- **Read-only and one statement.** The statement MUST run in a read-only
  transaction where the driver offers one — `postgres`, `mysql` and `vertica`
  through `database/sql`'s read-only transaction, `oracle` after `SET
  TRANSACTION READ ONLY` — rolled back afterwards, and for `clickhouse` with
  the setting `readonly=1`. `sqlserver`, whose driver refuses a read-only
  transaction, MUST run it in a transaction always rolled back afterwards,
  and `snowflake` MUST run it as it is; the documentation MUST require an
  account that can only read of both, and MUST say that the rollback undoes
  no commit the statement makes itself. `args` MUST be
  sent as values, never as SQL: bound to the statement's native placeholders
  at the server by every driver but `clickhouse`, whose driver writes each
  into the statement quoted as a literal of its type, and the documentation
  MUST say which. `statement_file` MUST name, in place of `statement`, the
  file the statement is read from when the configuration loads and at each
  reload; both written with something other than `""` MUST be refused,
  `""` being the key left out, and neither but on the debug page's
  playground, which makes no request. It MUST be refused at load: with
  blanks alone or around it, with a placeholder, with a NUL, or relative;
  a file missing, unreadable or no regular file — a named pipe MUST be
  refused without being opened —, over 1 MiB, not UTF-8 or holding a NUL
  byte, of blanks alone once a UTF-8 byte order mark and the blanks around
  it are taken off, or holding a `{{param_...}}` placeholder; and a
  statement the driver's own check refuses (`vertica`) MUST be refused
  naming the file. A symbolic link MUST be followed. The text read MUST be
  part of the collector's definition, so that a reload names the collector
  changed and its cached results are not reused; a load that opens no file
  (the target allocator's) MUST check the key without reading the file.
  `arg_types` MUST give each arg, by its place, the type it
  is bound as — `text`, the default, as written; `int`, Go's `int64` of a
  decimal whole number, a sign allowed; `float`, a finite `float64` of a
  decimal number, no hexadecimal float, underscore, `Inf`, `NaN` or number
  past the range; `bool`, `true` or `false` in any case — handed to the
  driver as that Go value. It MUST be refused at load without `args`, with
  another count of types than of args, or with a type other than those four
  as written; an arg of a type but `text` with no placeholder, or whose
  placeholders all have defaults, MUST be refused at load when it, filled
  with the defaults, is not of its type; and one a probe's parameters fill
  MUST be checked when they are, a value not of its type answered `400`
  before the server is asked and refused in a static target's `params` when
  its file loads. `statements` MUST run, in place of `statement`,
  `statement_file`, `args` and `arg_types`, which beside it MUST be refused
  at load, up to 16 entries of a `name` — letters, digits and `_`, not
  beginning with a digit, unique among the collector's —, a `statement`
  and its own `args` and `arg_types`, held to the rules of the one
  statement's, each placeholder of a `statement` refused naming its entry:
  in order, on one connection of the pool, each in a transaction of its
  own ended before the next begins, as the one statement's; `max_rows` MUST
  hold each, and the response limit their rows together. The first that
  fails MUST fail the scrape, `request.statements[<i>], <name>:` before its
  error, the rest not run. The document MUST be an object of each one's rows
  by its name, a `csv` transform MUST be refused at load beside it, and
  `http_exporter_sql_rows` MUST count all of their rows; a debug report MUST
  show each as a request, `Sql-Statement` its name, the connection of each
  after the first `reused`. A `vertica` statement its driver would not prepare, and
  so would write the values into, doubling a `'` and leaving a `\` as it
  is, MUST be refused at load: any `COPY`, its first word as the driver
  reads the text (outside quotes, quoted identifiers, `--` and unnested
  `/* */` comments and dollar quotes, as written and as its lexer rewrites
  it) or as the server does (after blanks and comments, nesting or not),
  the message of a `COPY ... FROM LOCAL` saying that it reads a file of the
  exporter's host at every scrape and is sent with its values written into
  it; and a statement the driver takes for a function's DDL or an `EXECUTE`
  grant on one (`CREATE`, `ALTER` or `DROP FUNCTION`, `GRANT` or `REVOKE
  EXECUTE ON FUNCTION`, by its own `UDSFAnalyzer`). The driver's URL MUST
  set `use_prepared_statements=1`, which no option overrides;
  `postgres` MUST send every statement with the extended protocol, `mysql`
  MUST NOT allow several statements in one, and a `vertica` statement holding
  a `;` followed by anything but blanks and comments, outside quotes, quoted
  identifiers, comments and dollar quotes, which its driver would run as
  several, MUST be refused at load — decided by the exporter's own reading,
  which knows no blocks, as well as by the driver's splitting, since the
  driver sends a statement whose `;` it takes to stand in a `BEGIN ... END`
  or similar block to the server whole, and the server runs every part. The
  exporter's reading MUST count the most statements of any way the server
  may read the text as well as the driver's: a `$` inside a word as part of
  it, not the start of a dollar quote; a `\` in an `E'...'`, and in every
  `'...'` with `standard_conforming_strings` off, escaping the next
  character; block comments nesting or not; and only ASCII white space as
  blank. A `vertica` statement the driver's lexer fails on (a `?` right
  after a quote it begins a string at, which panics it) MUST be refused at
  load, never panic the load. A `{{param_...}}` placeholder in
  `statement` MUST be refused at load; one in `database` MUST be filled only
  with letters, digits and `_ - . $`, refused otherwise before anything is
  sent.
- **Deadline.** The connection and the statement MUST run within the probe's
  deadline, which the driver cancels the statement at; a statement still
  running then MUST fail the scrape saying the deadline came first.
  `clickhouse` on `http` MUST send the setting
  `cancel_http_readonly_queries_on_client_close=1` with every request, so
  that the server stops a statement whose request the driver ended.
  `vertica`'s every call on a connection MUST return by its context's end,
  the connection then given up and closed once the driver's call returns,
  and the driver MUST NOT be given a context that ends, so that it sends no
  cancel request of its own (over a connection it dials itself, past the
  target lists and unencrypted); the documentation MUST say that the server
  then runs the statement to its end. A driver that panics MUST fail the
  connection, the statement or the keepalive ping it panicked in, never the
  process, in `database/sql`'s goroutine that opens connections too, and a
  panic while connecting MUST leave no place at `max_per_target` taken.
  Every call `database/sql` makes on a connection, its transactions and
  statements — in its goroutines that close a connection past its lifetime
  or idle time and roll back a transaction whose context ended, too — MUST
  turn a panic into an error joined with `driver.ErrBadConn`, so that the
  connection is closed; the connection that does so MUST be what the
  driver's is to `database/sql`, running statements, resetting its session
  and saying whether it is valid exactly when the driver's does, and doing
  as `database/sql` would without the rest, and the exporter's own use of
  the driver's connection (`Conn.Raw`) MUST reach the driver's.
  `postgres` MUST also set `statement_timeout` to the time left, in the
  transaction; `mysql` MUST set `max_execution_time` to it on MySQL 5.7.8
  and later and `max_statement_time` on MariaDB 10.1.1 and later, by the
  version the server gives when asked once on each connection (`SELECT
  VERSION()`, MariaDB's `5.5.5-` prefix skipped), and nothing on an older
  server or one whose version reads as neither, so that no scrape is
  refused over a variable the server does not know; a server that refuses
  the variable as unknown (MySQL's error 1193) MUST have the statement run
  without it, in a new read-only transaction, and MUST NOT be set it again
  on that connection, any other refusal failing the scrape; and `sqlserver` MUST
  set `LOCK_TIMEOUT` to it, in the transaction: each in milliseconds, a
  millisecond at least, MariaDB's in seconds to the millisecond. The server
  ending the statement at its limit (postgres's SQLSTATE 57014 statement
  timeout, MySQL's error 3024, MariaDB's 1969, SQL Server's 1222) MUST be
  reported as the deadline coming first though the scrape's own deadline is a moment away. `postgres`'s cancel request MUST go through the exporter's dialer,
  under the policy, to the address the statement's connection reached, and
  to no other.
- **Rows.** Integers and floating-point numbers MUST be numbers; the text of
  a column of a numeric type, a decimal (Snowflake's `FIXED` and `DECFLOAT`
  included), the number it writes, or its text
  when it writes none; a boolean 1 or 0; a date or a timestamp its Unix time
  in seconds with the fraction; a time of day (`TIME`, `TIMETZ`,
  ClickHouse's `Time` and `Time64`), whether the driver gives it as a time,
  as text or as a duration, the seconds since midnight with the fraction, as
  its clock reads where it was written (a MySQL `TIME`, a duration, MAY be
  negative or past 24 hours); text and UTF-8 binary data text; binary data
  that is no UTF-8 MUST fail the scrape naming the column; and `NULL` MUST
  leave the column out of its row; a column `NULL` in every row MUST still
  be one the response has, a `csv` rule reading it as a column `NULL` in
  some rows. A column without a name and a name given to two columns MUST
  fail the scrape. More than `max_rows` rows, and rows whose values hold more
  than the response limit — text and binary values by their length, every
  other value as 8 bytes, each with its column's name's length, a `NULL`
  nothing — MUST fail the scrape as a limit, the statement
  ended before its rows are closed so that the driver reads none of the
  rest. A driver reads a row whole before the limit sees any of it, so the
  limit MUST bound what a driver reads of the server at once where the
  driver can be told: `postgres` MUST refuse a message from the server
  larger than the limit and 1 MiB beside it from its length, before reading
  it, failing the scrape as a limit in the limit's words (`the server sent a
  message larger than the response limit of N bytes ...`), where pgx would
  read one of up to 1 GiB; `sqlserver` MUST set `TEXTSIZE` on each
  connection, when it is made and before each use, to twice the limit and two
  bytes, so that the server cuts a longer `varchar(max)`, `nvarchar(max)`,
  `varbinary(max)`, `text`, `ntext` or `image` value, a cut value being over
  the limit as read and so never handed on; `snowflake`'s answer to a
  statement, or to the driver's ask for a statement's result, which the
  exporter reads before the driver (below), MUST be read to the limit and no
  further: one whose `Content-Length` passes the limit refused unread, one
  without once a byte past the limit is read, and a gzipped one once a byte
  past the limit is decompressed, without decompressing further. Such an
  answer MUST fail the statement at once as a limit, in the limit's words
  (`the statement's answer from Snowflake was larger than the response
  limit of N bytes ...`), the driver given an answer it neither retries nor
  reads a result from, and the exporter MUST keep no more than one copy of
  at most the limit of an answer it has read, a gzipped one within the limit
  handed to the driver decompressed and its gzipped copy let go of. What no driver can be told — a `mysql` row, up to
  the server's `max_allowed_packet`; a SQL Server `xml` or CLR-type value;
  `clickhouse`'s and `vertica`'s rows and blocks of rows; what gosnowflake
  builds as it decodes the bounded answer — is read before the limit
  applies, and SQL.md MUST say so. The rows MUST be read by a `csv` transform by column name, one row
  at a time as a CSV file's with a header, and by `jq`, `yq` and `python` as
  the list; `decoder.type` other than its default, any other transform and
  any `response` setting MUST be refused at load, with no warning that the
  decoder is left to each response. The answer has no status.
- **Connections.** A collector MUST keep its connections to a target in a
  pool of its own, one for each target and connection settings — account,
  credential files, database, options, TLS settings, `connections`, target
  lists — shared with no other collector. `connections` holds
  `max_per_target` (`2`, `0` for no limit), `max_idle_per_target` (`1`, `0`
  for none), `max_targets` (`50`, `0` for no limit), `idle_timeout` (`5m`,
  `0` keeps), `max_lifetime` (`1h`, `0` for ever) and `keepalive`, its
  `interval` (`30s`, `0` or at least `1s`) and `check` (`tcp` or `ping`); no
  count or duration MAY be negative. The limits MUST be of targets, not
  pools: a target is the server, `host:port`, or the Snowflake account, with
  the connection settings, so that the pools of the databases and accounts a
  collector reaches on one server are one target's, and a static target
  whose `connections` change a setting is a target of its own. The
  connections open to a target, in use or idle, across its pools, MUST NOT
  be more than `max_per_target`: a scrape that would make one more MUST
  first close an idle connection of another of the target's pools, and a
  scrape that finds `max_per_target` connections in use MUST wait within its
  deadline, counted as a limit wait, and fail naming the limit when the
  deadline comes first, counted as a limit failure. A target MUST keep no
  more pools than `max_per_target`, and no more than 64 without one: a
  scrape that needs another pool at the limit MUST close the target's pool
  used longest ago that no scrape uses, and fail, counted as a limit
  failure, when every one is in use. The connections kept idle MUST be held
  to `max_idle_per_target` across the target's pools: a connection given
  back while they keep that many MUST be closed. A scrape of another
  target at `max_targets` MUST close the pools of the target used longest
  ago that no scrape uses, and fail, counted as a limit failure, when every
  target is in use. `check: tcp` MUST set TCP keepalives at the
  interval on the connections the exporter dials, and any other check or an
  interval of `0` none, though the driver would switch them on (`mysql`);
  `check: ping` MUST ping
  every idle connection of each pool every interval, a failed ping counted,
  and a ping MUST NOT count as a use to `idle_timeout`: a connection no
  scrape has had for `idle_timeout` MUST be closed, not pinged. Each ping
  MUST hold one of the target's places at `max_per_target` while it pings,
  taken without waiting, the connections pinged no more than the places
  free, and the pings MUST be skipped for the interval when the scrapes
  hold every place; they MUST NOT make a connection, and MUST be given the
  interval to answer in, one that does not answer by then counted as a
  failed ping. A static target MAY set each key but `max_targets`, which
  MUST be refused there, by the loader and the schema, replacing the
  collector's key by key. A reload MUST close the pools of a collector it
  removes or whose connection settings it changes, and the shutdown every
  pool: a closed pool MUST no longer be used by a new scrape, while the
  scrapes using it, running a statement or waiting at `max_per_target`, MUST
  finish on it, the pool closed when the last is done; their connections
  MUST be counted as the collector's while it is configured, and not after a
  later reload removes it. A driver's closing of a connection MUST NOT be
  waited for under a lock other scrapes or the self-metrics take, nor by a
  scrape past its deadline. The end of a statement (closing its rows,
  rolling back its transaction) MUST be bounded by the scrape's deadline,
  or a grace of 100 ms when the deadline has passed when it begins: when
  the bound comes first, the scrape MUST answer the rows it read, the
  connection MUST be closed and never given back to its pool, its network
  connections closed where the exporter dialed them, and it MUST hold its
  place at `max_per_target` until it is closed. A statement's transaction
  MUST be rolled back by the scrape at the statement's end, and MUST NOT be
  rolled back, nor its connection closed, by database/sql on its own when
  the scrape's context ends, while the scrape still holds the connection;
  the transaction's BEGIN MUST still be bounded by the scrape's deadline. A
  connection whose transaction was open when the scrape's context ended (its
  deadline, or the statement ended before its rows were all read) MUST then
  be closed, not given back, unless its driver's connection both resets its
  session and says whether it is valid.
- **Static targets** MAY set `timeout`, `username`, `password_file`,
  `private_key_file` (either credential file replacing the collector's),
  `tls` and `connections`; `params` fills the placeholders of `args` and
  `database`. A
  `{{` in a static target's `username`, `password_file` or
  `private_key_file` MUST be refused at load, by the loader and the schema,
  as `sshfile`'s are. A target's `tls` (§ 42.14) MUST be held at load to
  what the driver honours with the collector's `options`, as the
  collector's is, and MUST be part of the key of the target's pool.
- A driver's own log (`vertica`'s and `snowflake`'s) MUST go into the
  exporter's log at its level, with `sql_driver` naming the driver, never to
  stdout, stderr or a file of the driver's own unless gosnowflake's client
  configuration file (`sf_client_config.json`) names one. gosnowflake's
  error for a statement the exporter fails for a result in more than one
  chunk MUST be logged at debug, the scrape's failure saying what happened;
  its other errors MUST stay errors. A server's answer
  routing a `sqlserver` connection to another host MUST be refused by the
  exporter's dialer, and the documentation MUST say so.
- An error the database answered — the server's own error, wherever the
  exporter met it: connecting and authenticating, beginning the read-only
  transaction and its setup statements, the statement, and reading the rows
  — MUST be the operator's alone: the log, and with `--web.debug=full` a
  debug probe's report and the status page, MUST show its whole text, and a
  probe's caller MUST be answered, in its place, `the database answered
  <code>; its message is in the exporter's log`, the rest of the failure's
  text (the step, the server) as it is. The code MUST be the one the
  database documents: `SQLSTATE <state>` for `postgres` (pgconn's
  `PgError`) and `vertica` (`VError`), `error <number>` for `mysql`
  (`MySQLError`, followed by ` (SQLSTATE <state>)` when the server sent
  one) and `sqlserver` (`mssql.Error`), `code <number>` for `clickhouse`
  (`proto.Exception`), `ORA-<5 digits>` for `oracle` (go-ora's
  `network.OracleError`), and `error <6 digits>` followed by ` (SQLSTATE
  <state>)` when there is one for `snowflake` (`SnowflakeError`, but for
  the driver's own, numbered from 260000 to 279999). A failure that is not
  the server's answer — a dial, TLS, policy or protocol failure, the
  scrape's deadline, the response limit — MUST be answered as before. The
  marked error MUST still be what it was to `errors.Is` and `errors.As`, so
  that the server's statement timeout is still the scrape's deadline and a
  message over the limit still the limit's failure, and the server MUST be
  named after the failure unless the caller's text names it.
- A failed statement MUST be reported in the `sql` stage, naming the server.
  A debug probe's request section MUST show `QUERY`,
  `driver://host:port/database` (the service for `oracle`), the database
  escaped as a URL's path is, as `localfile`'s file is, the account,
  whether the connection was new or kept, and the number of bound values,
  and never a credential, a bound value or the statement's text; the
  outcome MUST name the rows and the columns. The verbose `url` label MUST
  be that URL, with the database's placeholders as written, and
  `http_method` MUST be `QUERY`.
