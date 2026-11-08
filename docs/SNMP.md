# SNMP agents

The `snmp` request type reads an SNMP agent, as snmp_exporter does: a
switch, a router, a firewall, a UPS, a printer, a server's `snmpd`. A
collector names the objects it reads one by one, such as `sysUpTime.0`, and
the tables it walks whole, such as the interface table, each by a name of
its own and its numeric identifier; the exporter asks the agent over UDP,
with SNMP v1, v2c or v3, and hands the transforms a JSON document of what
it answered, which `jq`, `yq` or `python` turn into series.

MIBs are not read. The names in the document are the collector's, the
numbers the agent's, so the type needs neither net-snmp nor a MIB
directory; a MIB, or `snmptranslate -On`, gives the number of a name once,
when the collector is written ([Finding an object's
number](#finding-an-objects-number)).

The type is opt-in: the default build and the published image leave it out,
so a build carries it only when asked
([Building with snmp](#building-with-snmp)). It needs nothing outside Go's
standard library.

## Building with snmp

`OPT_IN_REQUEST_TYPES` names the opt-in types a build adds to the types it
carries, comma-separated:

```sh
make build OPT_IN_REQUEST_TYPES=snmp              # bin/prometheus-universal-exporter
docker build --build-arg OPT_IN_REQUEST_TYPES=snmp -t exporter:snmp .
go build -tags "$(sh tools/request-type-tags.sh '' '' snmp)" .
```

The tag is `request_type_snmp`. A configuration with an `snmp` collector
is refused by a build without the type, at startup, on a reload and by
`--dry-run`, saying how to build one that has it.

## A collector

A switch's uptime and name, and each interface's state, speed and traffic,
over SNMP v2c:

```yaml
collectors:
  - name: switch
    request:
      type: snmp
      community_file: /etc/exporter/snmp-community
      get:
        sysUpTime: 1.3.6.1.2.1.1.3.0
        sysName: 1.3.6.1.2.1.1.5.0
      walk:
        interfaces:
          ifDescr: 1.3.6.1.2.1.2.2.1.2
          ifOperStatus: 1.3.6.1.2.1.2.2.1.8
          ifHighSpeed: 1.3.6.1.2.1.31.1.1.1.15
          ifHCInOctets: 1.3.6.1.2.1.31.1.1.1.6
          ifHCOutOctets: 1.3.6.1.2.1.31.1.1.1.10
    transform:
      type: jq
    metrics:
      - name: snmp_uptime_seconds
        description: How long the agent has been up
        expression: .get.sysUpTime.value / 100
        labels:
          - name: sys_name
            expression: .get.sysName.value
      - name: if_up
        description: 1 when the interface is up
        items: .walk.interfaces[]
        expression: 'if .ifOperStatus == 1 then 1 else 0 end'
        labels:
          - name: interface
            expression: .ifDescr
      - name: if_speed_bits_per_second
        items: .walk.interfaces[]
        expression: .ifHighSpeed * 1000000
        labels:
          - name: interface
            expression: .ifDescr
      - name: if_in_octets_total
        type: counter
        items: .walk.interfaces[]
        expression: .ifHCInOctets
        labels:
          - name: interface
            expression: .ifDescr
      - name: if_out_octets_total
        type: counter
        items: .walk.interfaces[]
        expression: .ifHCOutOctets
        labels:
          - name: interface
            expression: .ifDescr
```

```text
/probe?collector=switch&target=switch-1.example.com
```

The scrape reads the objects of `get` with a Get, walks each column of
`walk` with GetBulk, and answers:

```json
{
  "get": {
    "sysName": {"type": "OctetString", "value": "switch-1"},
    "sysUpTime": {"type": "TimeTicks", "value": 12345600}
  },
  "walk": {
    "interfaces": [
      {"index": "1", "ifDescr": "Gi0/1", "ifOperStatus": 1, "ifHighSpeed": 1000, "ifHCInOctets": 9123456789, "ifHCOutOctets": 811223344},
      {"index": "2", "ifDescr": "Gi0/2", "ifOperStatus": 2, "ifHighSpeed": 1000, "ifHCInOctets": 0, "ifHCOutOctets": 0}
    ]
  },
  "version": "2c",
  "address": "192.0.2.10:161",
  "requests": 6,
  "duration_seconds": 0.042
}
```

- `get` has each object the agent has, by its name: its `type` and its
  `value`. An object the agent does not have (`noSuchObject`,
  `noSuchInstance`, or v1's `noSuchName`) is left out, so a rule of it is a
  [missing value](CONFIGURATION.md#when-a-metric-cannot-be-extracted)
  rather than a failed scrape.
- `walk` has each table, by its name: its rows, in the order of their
  index, each the `index` and the value of each column the agent has of
  that row. The columns are merged by the index, the numbers after the
  column's identifier, so `ifDescr` comes beside `ifHCInOctets` in one row,
  as snmp_exporter's `lookups` would join them. A row a column lacks has no
  key of that column.
- `version` is `1`, `2c` or `3`; `address` the agent's address the
  requests went to; `engine_id`, of v3, the agent's engine in hexadecimal;
  `requests` how many requests the scrape sent, each sent again counted
  again; `duration_seconds` the whole scrape.

How a value is written:

| SNMP type | In the document |
| --- | --- |
| INTEGER, Counter32, Gauge32, Unsigned32, Counter64 | a number, every digit of it, a Counter64 near 2^64 included |
| TimeTicks | a number of hundredths of a second, as the agent counts them: `/ 100` makes seconds |
| OCTET STRING | text, when it is UTF-8 with no control character but a tab and line ends; otherwise its bytes in hexadecimal by colons, `00:1a:2b:3c:4d:5e`, as a MAC address is written |
| OBJECT IDENTIFIER | dotted: `1.3.6.1.4.1.9.1.1208` |
| IpAddress | dotted: `192.0.2.1` |
| Opaque | the number of a float or a double, as net-snmp writes them; anything else in hexadecimal |
| NULL | `null` |

`hex` names the objects of `get`, and the `table.column`s of `walk`,
whose bytes are written in hexadecimal even when they read as text: a MAC
address whose bytes happen to be printable is otherwise written as the
letters they spell.

```yaml
      hex: [interfaces.ifPhysAddress]
```

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `snmp`. |
| `snmp_version` | `2` | `1`, `2` for v2c, or `3`. See [Versions and accounts](#versions-and-accounts). |
| `community_file` | none | With `1` and `2`, **required**: the file of the community the agent answers to. |
| `username`, `security_level`, `auth_protocol`, `auth_password_file`, `priv_protocol`, `priv_password_file`, `context_name` | none | With `3`: the user and its security. See [SNMPv3](#snmpv3). |
| `get` | none | The objects read one by one: a name of yours, letters, digits and `_`, and the object's numeric identifier, a leading dot or not; one of two arcs, which YAML reads as a number, is quoted, `"1.3"`. At most 256. |
| `walk` | none | The tables read whole: a name of yours, and its columns, each a name and the column's identifier. At most 32 tables, of 64 columns each; a column may not be named `index`. One of `get` and `walk` is required. |
| `hex` | none | Names of `get` and `table.column` of `walk` written in hexadecimal whatever they hold. |
| `max_repetitions` | `25` | With `2` and `3`, how many values a GetBulk of a walk asks for, 1 to 255. Not with `1`, which has no GetBulk. |
| `request_timeout` | `1s` | How long each request waits for its answer before it is sent again, at least `10ms`. |
| `retries` | `1` | How many times a request unanswered is sent again, 0 to 5. |
| `walk_limit` | `10000` | The most values a column of a walk may have, 1 to 1000000. |
| `max_response_bytes` | `10MiB` | The most bytes of the document: see [Limits](#limits). |
| `allowed_targets`, `denied_targets` | none | The agents the collector may and may not reach, as for [any type that reaches a host](REQUESTS.md#restricting-targets), checked on the socket. |

No other request key applies, and setting one is a startup error. The
decoder is `json`, left out or written, and the transform `jq`, `yq` or
`python`.

## Walking tables

Each column of `walk` is walked from its identifier to the end of the
column: with v2c and v3 by GetBulk, `max_repetitions` values a request,
and with v1 by GetNext, one value a request, since v1 has no GetBulk. A
column of 48 interfaces is two GetBulk requests of 25; five columns of it
are ten. An agent that answers a GetBulk `tooBig`, its answer too large for
a datagram, is asked for half as many values, down to one.

The index is what comes after the column's identifier, dotted: `1` for the
first interface of `ifTable`, `192.0.2.1` for a row of `ipAddrTable`, whose
index is the address, `4.192.0.2.1` for a table indexed by a type and an
address. A rule reads it as text, and splits it when it holds several
numbers:

```yaml
      - name: ip_address_info
        items: .walk.addresses[]
        expression: "1"
        labels:
          - name: address
            expression: .index
```

A column that never ends, as an agent whose walk goes round would answer,
is held to `walk_limit`: past it the scrape fails as a limit exceeded. An
answer whose identifier is not past the one before it fails the scrape at
once, naming both.

## The target

The probe's `target` is the agent: `host`, `host:port` or `[v6]:port`, port
161 when it names none, and a bare IPv6 address too. A scheme, a path, a
query or a user is refused with `400`, before anything is sent.
`allowed_targets` and `denied_targets` are matched against the host before
the socket is opened and against the address it is connected to, and the
cloud metadata service is refused without them, as for `http`.

The requests go over UDP, one socket for the scrape. A request unanswered
within `request_timeout` is sent again, `retries` times; a scrape whose
request is never answered fails, saying how many were sent and how long each
waited. An agent of another community, or a v1 or v2c agent asked as a v3
one, is silent rather than refusing, so that failure is often a wrong
community.

## Versions and accounts

`snmp_version` is `1`, `2` for v2c, or `3`, `2` when left out. v1 and v2c
send a community with each request, which the agent answers to;
`community_file` is the file that holds it, read at each scrape, blanks
around it taken off:

```text
$ cat /etc/exporter/snmp-community
s3cr3t-ro
```

The community travels in the clear, so it is a weak password: keep it
read-only on the agent, and let the agent answer the exporter's address
alone. A community in the configuration itself is not taken: the file keeps
it out of the configuration, the debug report and `--dry-run`, and can be
mounted from a Kubernetes Secret.

v1 is for agents that speak nothing else, older printers, UPSes and PDUs:
it has no GetBulk, so a walk is a GetNext for each value, and no Counter64,
so traffic counters are 32 bits wide and wrap quickly on fast links. v2c is
what most agents are read with.

## SNMPv3

v3 replaces the community with a user of the agent and the User-based
Security Model (RFC 3414): the requests are signed, and encrypted, with keys
made from the user's passwords for that agent alone.

```yaml
collectors:
  - name: router
    request:
      type: snmp
      snmp_version: 3
      username: exporter
      security_level: authPriv
      auth_protocol: SHA256
      auth_password_file: /etc/exporter/snmp-auth
      priv_protocol: AES
      priv_password_file: /etc/exporter/snmp-priv
      get:
        sysUpTime: 1.3.6.1.2.1.1.3.0
    transform:
      type: jq
    metrics:
      - name: snmp_uptime_seconds
        expression: .get.sysUpTime.value / 100
```

| Key | Notes |
| --- | --- |
| `username` | **Required.** The user, as the agent has it configured. |
| `security_level` | **Required.** `noAuthNoPriv`, a user alone; `authNoPriv`, each request signed; `authPriv`, signed and encrypted. |
| `auth_protocol` | With `authNoPriv` and `authPriv`, **required**: `MD5`, `SHA` (SHA-1), `SHA224`, `SHA256`, `SHA384` or `SHA512` (RFC 7860). |
| `auth_password_file` | With `authNoPriv` and `authPriv`, **required**: the file of the authentication password, at least 8 characters. |
| `priv_protocol` | With `authPriv`, **required**: `DES`, `AES` (AES-128, RFC 3826), `AES192`, `AES256`, `AES192C` or `AES256C`. |
| `priv_password_file` | With `authPriv`, **required**: the file of the privacy password, at least 8 characters. |
| `context_name` | The context the objects are read in; the default context when left out. |

The names are snmp_exporter's: net-snmp's `snmpget -a` writes the SHA-2
ones with a dash, `SHA-256`, and `-x` the AES ones `AES-192` and `AES-256`.
A key the level does not use is refused at load, as is a v3
key with v1 or v2c and `community_file` with v3.

Each scrape first asks the agent its engine ID, boots and time, which an
unauthenticated Report answers (RFC 3414 § 4); the passwords are then made
keys for that engine, and every request names it, its boots and its time.
A Report that a request was outside the engine's time window, as an agent
rebooted since gives, has the request sent again, once, with the time the
Report brings. The document's `engine_id` is the engine discovery found.

**AES-192 and AES-256 come two ways.** RFC 3826 standardises AES-128 alone,
and agents extend the key of the longer ones in one of two incompatible
ways. `AES192` and `AES256` extend it as `draft-blumenthal-aes-usm-04` does;
`AES192C` and `AES256C` as
`draft-reeder-snmpv3-usm-3desede` does, as Cisco's agents and many others
do. snmp_exporter names them alike. With an authentication
protocol whose hash is long enough, `SHA256` and above for `AES256`, no
extension is needed and the two are one. A wrong choice reads as a
decryption error of the agent's (`usmStatsDecryptionErrors`).

## Static targets

A [static target](STATIC-TARGETS.md) of an `snmp` collector names the
agent as its `target`, which it requires, and may set under its `request`
`timeout`, the whole scrape's budget, `request_timeout` and `retries`, how
long each request waits and how many times it is sent again, a
`request_timeout` of 0 keeping the collector's, and its own
account: `community_file` of a v1 or v2c collector, and `username`,
`auth_password_file` and `priv_password_file` of a v3 one, each as its
collector's security level uses it. What is read is the collector's, so
one collector serves agents that each have their own community or user,
and an agent at the far end of a slow link waits longer than the rest:

```yaml
interval: 1m
targets:
  - name: switch_1
    collector: switch
    target: switch-1.example.com
  - name: branch_switch
    collector: switch
    target: 198.51.100.7
    request:
      community_file: /etc/exporter/branch-community
      request_timeout: 3s
      retries: 2
```

The files hold no placeholder, and no probe parameter sets them, so a probe
can name neither the community nor the user a scrape reads as. A target's
own account and waits are part of its [cache
key](CONFIGURATION.md#response-caching), so targets that differ in them
never share an answer.

## Finding an object's number

A collector names objects by number, which the MIB that defines them gives:

```sh
snmptranslate -On IF-MIB::ifHCInOctets      # .1.3.6.1.2.1.31.1.1.1.6
snmpwalk -v2c -c public -On switch-1 IF-MIB::ifTable
```

The numbers of the objects most collectors read:

| Object | Identifier | What it is |
| --- | --- | --- |
| `sysDescr.0` | `1.3.6.1.2.1.1.1.0` | The agent's description |
| `sysUpTime.0` | `1.3.6.1.2.1.1.3.0` | Hundredths of a second since the agent started |
| `sysName.0` | `1.3.6.1.2.1.1.5.0` | The device's name |
| `ifDescr` | `1.3.6.1.2.1.2.2.1.2` | An interface's description |
| `ifOperStatus` | `1.3.6.1.2.1.2.2.1.8` | 1 up, 2 down, 3 testing, 5 dormant, 7 lower layer down |
| `ifPhysAddress` | `1.3.6.1.2.1.2.2.1.6` | An interface's MAC address |
| `ifName` | `1.3.6.1.2.1.31.1.1.1.1` | An interface's short name |
| `ifHCInOctets`, `ifHCOutOctets` | `1.3.6.1.2.1.31.1.1.1.6`, `.10` | Octets in and out, 64 bits |
| `ifHighSpeed` | `1.3.6.1.2.1.31.1.1.1.15` | Speed in Mbit/s |
| `hrProcessorLoad` | `1.3.6.1.2.1.25.3.3.1.2` | Each processor's load, percent |
| `upsEstimatedMinutesRemaining` | `1.3.6.1.2.1.33.1.2.3.0` | Of a UPS (UPS-MIB) |

## Limits

The scrape's every request ends at the probe's deadline, its `timeout` or
the scrape's, 30 seconds without one, as well as at `request_timeout`: a
scrape of a large table over a slow link needs a deadline for all its
requests. The document is held to `max_response_bytes`, or
`limits.max_response_bytes`, 10 MiB when neither is set: a document past it
fails the scrape as a limit exceeded, which counts in
`http_exporter_series_limit_exceeded_total`, and so does a column past
`walk_limit`. A Get asks for at most 32 objects, and one the agent answers
`tooBig` is split in half.

## Errors and self-metrics

A failed scrape is reported in the `snmp` stage and follows
`error_handling.on_fetch_error`, saying what went wrong: `no answer from
192.0.2.10:161 to 2 requests, each waited for 1s; check the agent is up,
request.community_file or the v3 user, and that it answers the exporter's
address`; `reading the answer of 192.0.2.10:161: ... connection refused; no
agent listens on the port`; `the agent at 192.0.2.10:161 answered the Get of
sysName (1.3.6.1.2.1.1.5.0) with genErr`; and, of v3, the Report the agent
answered with: `the agent knows no such user (usmStatsUnknownUserNames)`,
`the agent found the request's signature wrong (usmStatsWrongDigests);
check request.auth_protocol and the password of request.auth_password_file`,
`the agent could not decrypt the request (usmStatsDecryptionErrors)`, and
`the agent does not take this security level
(usmStatsUnsupportedSecLevels)`. An answer signed wrong fails the scrape
too. An agent the target lists refuse counts in
`http_exporter_targets_refused_total`, answered `403`.

A [debug probe](CONFIGURATION.md#debug-probes) shows each step: of v3 the
discovery and the engine it found, the Get with each object's name and
number, and each table's walk, with what came of each:

```text
Requests
  1. DISCOVER snmp://192.0.2.10:161 -> engine 80001f8804707565, boots 3, time 81234 in 2ms
     Snmp-User: exporter
  2. GET snmp://192.0.2.10:161 -> 2 of 2 objects, in 1 requests in 3ms
     Snmp-Objects: sysName=1.3.6.1.2.1.1.5.0, sysUpTime=1.3.6.1.2.1.1.3.0
  3. WALK snmp://192.0.2.10:161 -> 48 rows of 5 columns, in 10 requests in 31ms
     Snmp-Columns: ifDescr=1.3.6.1.2.1.2.2.1.2, ...
     Snmp-Table: interfaces
```

With [verbose self-metrics](SELF-METRICS.md#verbose-per-request-self-metrics)
a scrape is labelled `url="snmp://host:port"` and `http_method="WALK"` when
the collector walks a table, `http_method="GET"` when it only reads objects.

## Limitations

- **No MIBs.** Objects are named by number; the textual conventions a MIB
  gives, such as an enumeration's names or a `DisplayString`'s hint, are
  the rules' to apply, as `value_map` does.
- **Reads alone.** A collector sends Get, GetNext and GetBulk; it sets
  nothing, and receives no trap.
- **One engine discovery a scrape.** A v3 scrape costs a request more than
  a v2c one, as the engine and its time are asked each time rather than
  kept between scrapes.
- **Columns walked one after another.** Each column is its own walk, so a
  table of many columns is as many walks, each of a GetBulk or more; the
  requests of a scrape are sent one at a time, as an agent's CPU, often a
  switch's management processor, takes them best.
- **No SNMP over TLS or DTLS** (RFC 6353), which few agents speak.

## Troubleshooting

- **`no answer from ... to 2 requests`.** The agent is down, does not let
  the exporter's address ask, or does not know the community: an agent
  drops a request of another community without a word. `snmpget -v2c -c
  <community> <agent> 1.3.6.1.2.1.1.3.0` from the exporter's host tells
  which. A v3 user asked as v2c, and the reverse, is silent too.
- **Some objects missing from `get`.** The agent does not have them: a
  `.0` left off a scalar's identifier (`sysUpTime.0` is
  `1.3.6.1.2.1.1.3.0`, not `1.3.6.1.2.1.1.3`) is the common cause.
- **An empty table.** The identifier is of the table or the entry rather
  than of a column: walk `1.3.6.1.2.1.2.2.1.2`, `ifDescr`, not
  `1.3.6.1.2.1.2.2`, `ifTable`.
- **`usmStatsDecryptionErrors`.** The privacy password or protocol differs
  from the agent's, or the agent extends an AES-192 or AES-256 key the
  other way: try `AES256C` for `AES256`, and the reverse.
- **Traffic counters that drop.** v1's, and `ifInOctets`'s, are 32 bits
  wide and wrap within minutes on a fast link: read `ifHCInOctets` over
  v2c or v3.
