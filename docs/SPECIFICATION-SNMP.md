# Prometheus Universal Exporter — `snmp` Request Type Specification

This is the `snmp` request type's section of § 5.1 of the
[exporter specification](SPECIFICATION-EXPORTER.md), in a document of its
own so that a change to the type is a change to this document. Everything
the request types share — the registry and the rules every type is held to
(§ 5.1), building without a type, decoding, transforms, error policies,
limits, caching, static targets and exposition — is in the exporter
specification, and a section number here, such as § 42.14, is one of its
sections. What a user reads of the type is [SNMP.md](SNMP.md).

## `snmp`

An `snmp` collector reads an SNMP agent's objects one by one and walks its
tables, and hands the transforms a JSON document of what it answered. It
MUST import nothing outside the standard library, MIBs included: its objects are
named by the collector and numbered by their identifiers. Its keys:

| Key | Default | Rule |
| --- | --- | --- |
| `snmp_version` | `2` | 1, 2 for v2c, or 3. |
| `community_file` | none | With 1 and 2, required: the file of the community. |
| `username` | none | With 3, required: the user, of the common part. |
| `security_level` | none | With 3, required: `noAuthNoPriv`, `authNoPriv` or `authPriv`. |
| `auth_protocol`, `auth_password_file` | none | With `authNoPriv` and `authPriv`, required: `MD5`, `SHA`, `SHA224`, `SHA256`, `SHA384` or `SHA512`, and the file of the password. |
| `priv_protocol`, `priv_password_file` | none | With `authPriv`, required: `DES`, `AES`, `AES192`, `AES256`, `AES192C` or `AES256C`, and the file of the password. |
| `context_name` | `""` | With 3, the context. |
| `get` | none | Objects by name and identifier, at most 256. |
| `walk` | none | Tables by name, each columns by name and identifier, at most 32 tables of 64 columns. |
| `hex` | none | Names of `get` and `table.column` of `walk` written as hexadecimal. |
| `max_repetitions` | `25` | With 2 and 3, the values a GetBulk asks for, 1 to 255. |
| `request_timeout` | `1s` | How long a request waits for its answer, at least `10ms`. |
| `retries` | `1` | How many times one unanswered is sent again, 0 to 5. |
| `walk_limit` | `10000` | The most values of a column, 1 to 1000000. |
| `max_response_bytes`, `allowed_targets`, `denied_targets` | as `http` | |

- `get` or `walk` MUST name at least one object. A name of `get`, of a
  table and of a column MUST be letters, digits and `_`, not beginning with
  a digit; a column MUST NOT be named `index`; an identifier MUST be
  numbers by dots, a leading dot taken, of 2 to 128 arcs, the first 0, 1 or
  2, the second below 40 under 0 and 1, each at most 4294967295, and an
  identifier an agent sends whose arcs, the first two together among them,
  are past it MUST fail the scrape; an object
  of `get` named twice and a name of `hex` of no object MUST be refused at
  load.
- A key of one version's account MUST be refused with another's: v3's keys
  with 1 and 2, `community_file` with 3, a protocol or password file a
  security level does not use, and `max_repetitions` with 1. A password MUST
  be at least 8 characters and a community not empty, read from its file at
  each scrape, blanks around it taken off; one that is not MUST fail the
  scrape naming the key, and `--dry-run` MUST warn of it.
- The target MUST be `host`, `host:port` or `[v6]:port`, port 161 when it
  names none, a bare IPv6 address too; a scheme, a path, a query or a user
  MUST be refused before anything is sent. Its `url` label MUST be
  `snmp://host:port`, its `http_method` `WALK` with `walk` and `GET`
  without. The target lists MUST hold the name before the socket is opened
  and the address when it is connected, as an `ntp` request's.
- The requests MUST go over one UDP socket of the scrape. A request
  unanswered within `request_timeout`, or the scrape's deadline when sooner,
  MUST be sent again, `retries` times, and a scrape with none answered MUST
  fail saying how many were sent and how long each waited, or, when the
  scrape's deadline came, with the deadline's error, sending nothing more; a
  datagram that is not the answer, by its request-id, of v3 its msgID, MUST
  be ignored, and an answer to any sending of the request, an earlier one's
  coming late included, MUST be taken. A
  read the system refuses, as an ICMP port unreachable makes it, MUST fail
  the scrape saying no agent listens.
- `get` MUST be asked with GetRequest, at most 32 objects a request in the
  order of their names; an object the agent answers `noSuchObject` or
  `noSuchInstance` MUST be left out of the document; v1's `noSuchName` MUST
  take the object its error-index names out of the request and ask the rest
  again; `tooBig` MUST halve the request; any other error-status MUST fail
  the scrape naming the object and the status, and an answer of another
  number of values, or another identifier in an object's place, MUST fail
  it.
- Each column of `walk` MUST be walked from its identifier: with v1 by
  GetNext, one value a request, until `noSuchName`; with 2 and 3 by GetBulk
  of `max_repetitions`, halved on `tooBig` down to 1, until `endOfMibView`
  or a value outside the column. A value not past the one before it MUST
  fail the scrape, and a column past `walk_limit` values MUST fail it as a
  limit exceeded. The columns of a table MUST be merged into rows by the
  index, the identifier after the column's, each row having `index`, the
  index dotted, and the values of the columns the agent has of it, the rows
  in the order of their index as identifiers.
- A value MUST be written: an INTEGER, Counter32, Gauge32, TimeTicks
  (hundredths), Unsigned32 and Counter64 as a number of every digit; an
  OCTET STRING as text when it is UTF-8 of no control character but a tab
  and line ends and not named in `hex`, and otherwise as its bytes in two
  hexadecimal digits each by colons; an OBJECT IDENTIFIER and an IpAddress
  dotted; an Opaque of a finite float or double (`0x9f78`, `0x9f79`) as the
  number and any other as hexadecimal; a NULL as null. A value of another
  tag MUST fail the scrape.
- The body MUST be a JSON object with `get`, each object's `type` and
  `value`, `walk`, each table's rows, `version` (`1`, `2c` or `3`),
  `address`, the address the requests went to, of v3 `engine_id` in
  hexadecimal, `requests`, the requests sent, again ones counted, and
  `duration_seconds`, with the `Content-Type` `application/json` and no
  status, held to the response limit as a limit exceeded.
- With 3, a scrape MUST first discover the engine (RFC 3414 § 4): an
  unauthenticated, reportable request naming no engine, no time and no
  user, whose Report brings the engine's ID, boots and time; an answer with
  no engine ID MUST fail the scrape. The passwords MUST be made keys and
  localized to the engine as RFC 3414 § A.2 does, with the authentication
  protocol's hash; each request MUST then name the engine, its boots and
  its time as discovery read it plus the seconds since, and the context.
- With `authNoPriv` and `authPriv` each request MUST be signed with HMAC of
  the protocol over the message, its authentication parameters zeros,
  truncated to 12 bytes for MD5 and SHA, 16, 24, 32 and 48 for SHA224 to
  SHA512 (RFC 7860), and an answer MUST be signed alike and be checked,
  one signed wrong failing the scrape and one unsigned failing it too, as
  one in the clear to a request of `authPriv` MUST. With
  `authPriv` the scoped PDU MUST be encrypted: DES-CBC under the first 8
  bytes of the privacy key, its IV the next 8 XOR a salt of the boots and a
  counter, padded to whole blocks (RFC 3414 § 8); AES-CFB-128 under its
  first 16, 24 or 32, its IV the boots, the time and a salt of 8 bytes
  (RFC 3826); a key too short for AES192 and AES256 extended by the hash of
  itself, `draft-blumenthal-aes-usm-04`, and for AES192C and AES256C by the
  key localized from it, `draft-reeder-snmpv3-usm-3desede`. An answer MUST
  be decrypted with its own boots, time and salt.
- A Report MUST fail the scrape naming its counter — an unsupported
  security level, an unknown user, a wrong digest, a decryption error, an
  unknown engine or context — but one of `usmStatsNotInTimeWindows` after
  discovery, which MUST take the engine's boots and time from the Report
  and send the request again, once, not counted as a retry.
- A debug report MUST show, of v3, `DISCOVER` with `Snmp-User` and as its
  outcome the engine in hexadecimal, its boots and time; the `get` as one
  `GET` with `Snmp-Objects`, each name and identifier, and as its outcome
  how many objects the agent had of how many, in how many requests; and
  each table as a `WALK` with `Snmp-Table` and `Snmp-Columns`, its outcome
  the rows and columns and the requests.
- A static target MAY set `timeout`, `request_timeout` and `retries`, held
  as the collector's are, and its own account as its collector's version
  and level use it: `community_file` of 1 and 2, `username`,
  `auth_password_file` and `priv_password_file` of 3, none holding a
  placeholder; each MUST be part of the target's cache key. A probe MAY set
  `timeout` and `param_<name>`, which the label values take.
