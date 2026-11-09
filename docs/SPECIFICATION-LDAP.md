# Prometheus Universal Exporter — `ldap` Request Type Specification

This is the `ldap` request type's section of § 5.1 of the
[exporter specification](SPECIFICATION-EXPORTER.md), in a document of its
own so that a change to the type is a change to this document. Everything
the request types share — the registry and the rules every type is held to
(§ 5.1), building without a type, decoding, transforms, error policies,
limits, caching, static targets and exposition — is in the exporter
specification, and a section number here, such as § 42.14, is one of its
sections. What a user reads of the type is [LDAP.md](LDAP.md).

## `ldap`

An `ldap` collector searches an LDAP directory, any server of LDAPv3, and
hands the transforms what its searches find as one JSON document. Its keys:

| Key | Default | Rule |
| --- | --- | --- |
| `bind_dn` | none | The DN of a simple bind. A DN as RFC 4514 writes one, not blanks alone, no placeholder; only with `password_file`. Without it the bind is anonymous. |
| `password_file` | none | The file of the bind's password. Not blanks alone; only with `bind_dn`. |
| `sasl_external` | `false` | A SASL `EXTERNAL` bind in place of the simple one: only with `tls.cert_file` and `key_file`, and MUST be refused beside `bind_dn` or `password_file`; a static target of such a collector MUST be refused its own `bind_dn` or `password_file`, and a `tls` block of its own without `cert_file`. |
| `start_tls` | `false` | Begin TLS with the StartTLS extended operation on a plaintext connection. |
| `searches` | — | Required, one or more: entries of `name`, `base`, `scope`, `filter`, `attributes`, `count_only` and `max_entries`. |
| `tls` | none | As `http`'s. |
| `max_response_bytes`, `allowed_targets`, `denied_targets` | as `http` | |

- The client MUST speak LDAPv3 in BER as RFC 4511 restricts it and MUST
  link no LDAP client library.
- `password_file` MUST be read at every scrape, not at load, its
  surrounding whitespace taken off; a file that is missing, unreadable or
  empty MUST fail the scrape naming the key and the file, never its
  content, before anything is sent, since a simple bind with a DN and an
  empty password is an unauthenticated bind. `--dry-run` MUST read it, and
  the `tls` block's files when the block is set, and list what a scrape
  would fail on as warnings of the `config` check.
- A search's `name` MUST be letters, digits and `_`, not starting with a
  digit, and unique among the collector's searches; its `base` a DN, `""`
  the root DSE; its `scope` `base`, `one` or `sub`, `sub` when left out; its
  `filter` an RFC 4515 filter in parentheses — and, or, not, equality,
  substrings, `>=`, `<=`, presence, approximate and extensible match —
  `(objectClass=*)` when left out, nesting `&`, `|` and `!` at most 32
  deep, and a substring match with no substring between its wildcards,
  `(cn=**)`, which RFC 4511 does not allow, MUST be refused, saying to
  write `(cn=*)` for presence; each of its `attributes` an attribute's name
  or OID with options, `*` or `+`, each named once in any case. `count_only` MUST NOT go with
  `attributes` or `max_entries`, and `max_entries` MUST NOT be negative,
  `0` being the default of 1000. Each MUST be refused at load otherwise.
- A placeholder MAY stand in a search's `base` and `filter` alone, and
  there only in a value: an attribute's value of the base, filled escaped
  as RFC 4514 escapes one, and an assertion's value of the filter, filled
  escaped as RFC 4515 escapes one, so no probe's value can change the
  base's RDNs or the filter's structure; a placeholder in a DN's attribute
  type, a filter's attribute or structure, or any other key MUST be refused
  at load. A substring match that placeholders filled with empty values
  leave with no substring, `(cn=*{{param_x:}}*)`, MUST be sent as the
  presence filter `(cn=*)`, as `(cn={{param_x:}}*)` fills in to, and never
  as a SubstringFilter of no substring.
- A scrape MUST make one connection, begin TLS on it when `start_tls` is
  set, send a simple bind, as `bind_dn` or anonymous, or with
  `sasl_external` a bind of SASL credentials of the mechanism `EXTERNAL`
  alone, no DN, password or authorization identity, shown as `BIND SASL
  EXTERNAL`, and then each search
  in order, each answered before the next is sent, and end with an unbind.
  A search MUST ask for its `attributes`, or for `1.1` when it has none, and
  MUST carry the Simple Paged Results control (RFC 2696), not critical,
  asking for pages of 500 entries and sending each page after the first
  the cookie the last one ended with, until a page ends with none or with
  no such control; it MUST send no size limit and, as its time limit, the
  seconds left until the scrape's deadline. A continuation reference MUST
  be counted and MUST NOT be followed. An operation answered with a result
  other than `success` MUST fail the scrape with the operation, the
  result's name and number and the server's diagnostic message, the bind's
  password written `***`; a probe's caller MUST be answered a failed bind
  with its DN withheld (`BIND [withheld; in the exporter's log] ***`), the
  DN being the operator's account, and an anonymous bind as `BIND
  anonymous`; a notice of disconnection MUST fail it with the server's
  reason.
- The answer MUST be a JSON document of `searches`, an object of each
  search's result under its name: `count`, the entries it matched across
  every page, `references`, the continuation references it answered, and,
  unless it is `count_only`, `entries`, a list of each entry's `dn` and
  `attributes`, an object of each attribute as the server named it, each a
  list of its values as text. A `count_only` search MUST hold none of its
  entries. A search that is not `count_only` matching more entries than its
  `max_entries` MUST fail the scrape at the next one. A DN, an attribute's
  name or a value that is not UTF-8 MUST fail the scrape naming the
  attribute and the entry, and MUST NOT be put in the document with U+FFFD
  in place of its bytes.
- An attribute an entry holds with a range option, `member;range=0-1499`
  as Active Directory answers one of more values than it sends at once,
  the option among the name's in any case, MUST be followed, after the
  search's pages, with a search of the entry alone, scope base, filter
  `(objectClass=*)`, of the next range, `member;range=1500-*`, until a
  range whose end is `*` or an answer with none, at most 1000 searches of
  an attribute; a range whose start is not the one asked for MUST fail the
  scrape. Every value MUST be under the attribute's name without the range
  option, `member`, in the order answered. Each such search MUST be a
  request of a debug report and an exported trace, `SEARCH` with its base
  and the range asked for, its outcome the values it answered.
- The messages of a scrape MUST be read against the response limit
  together, counted in their bytes as they come off the connection, a
  `count_only` search's among them. A message's declared length past what
  the limit has left MUST fail the scrape before a byte of its content is
  read, and no more MUST be held for one than the server has sent of it;
  each failure past the limit MUST be a limit exceeded. A length of more
  than four bytes, an indefinite length, an element longer than what
  encloses it, a tag of more than one byte, a constructed string, and a
  message of an ID the scrape is not waiting for MUST fail the scrape
  saying it is no LDAP message. The document MUST be held to the response
  limit as the messages are, one whose JSON is larger failing the scrape as
  a limit exceeded, saying the document's size.
- The target MUST be `host`, `host:port`, `[v6]:port`, or an `ldap://` or
  `ldaps://` URL of one, port 389, or 636 for TLS from the start, when it
  names none, with no user, path, query or other scheme, which MUST be
  answered `400` before anything is sent, as a static target's is refused
  at load. It is required. `ldaps://`, or a written `tls` block without
  `start_tls`, an empty one (`tls: {}`) as much as one with keys, MUST
  make the scrape speak TLS from the start, port 636 for a bare host, so
  that the bind's password never goes out in plaintext, and
  `start_tls` MUST make it send StartTLS before the bind, either verifying
  the certificate against `tls.server_name` or the target's host; a
  refused StartTLS MUST fail the scrape. `ldap://` with a written `tls`
  block and no `start_tls`, and `ldaps://` with `start_tls`, MUST be refused.
  `starttls`, the protocol `tcp` and `tlscert` begin TLS in, MUST be refused
  in an `ldap` collector, however it is written, as a key the type does not
  take with the type's own key named after it: `an ldap collector asks for
  StartTLS on its plaintext connection with its own key, start_tls: true`;
  and `start_tls` in a `tcp` or a `tlscert` collector MUST be refused naming
  `starttls` and its four protocols. The
  target lists MUST be applied to the host before the connection and to the
  address connected to, as `http`'s are, a refused one answered `403`,
  counted in `http_exporter_targets_refused_total`, and never connected to.
- The connection, the TLS handshake and every read and write MUST end at
  the scrape's deadline, 30 seconds without one, or when it is cancelled,
  and a scrape ended so MUST fail with the deadline's error.
- A failed scrape MUST be reported in the `ldap` stage. The decoder MUST
  be `json`, as for `grpc`; another decoder, and a transform other than
  `jq`, `yq` and `python`, MUST be refused at load.
- A debug probe's request section MUST show each operation, `STARTTLS`,
  `BIND` and each page of each `SEARCH`, by its name and `ldap://host:port`
  (`ldaps://` for TLS from the start), with an `Ldap-Operation` header of
  the bind's DN and its password as `***`, or `BIND anonymous`, and of the
  search's name, base, scope, filter and attributes, and how each ended as
  its outcome. The verbose `url` label MUST be `ldap://host:port` or
  `ldaps://host:port`, and `http_method` MUST be `SEARCH`.
- A static target MAY set `timeout`, its own account, `bind_dn` and
  `password_file`, and its own `tls` block (§ 42.14); a probe MAY set
  `timeout` and `param_<name>`. A target's `password_file` MUST replace the
  collector's, as the password of the collector's `bind_dn`, or of the
  target's own, which MUST replace the collector's; a target's `bind_dn`
  without its own `password_file`, a `password_file` alone of a target
  whose collector binds anonymously, and an `ldap://` target with its own
  `tls` block and no `start_tls` on its collector MUST be refused at load.

A collector MAY set `cache`, a mapping of `ttl`, the time to live of a cached
collector result, and `stale_if_error`, how much longer a result stands in for
a trip that fails. Omitting `cache`, or setting both to `0s`, MUST disable
caching for that collector. Section 42.13 defines the caching contract.

Every collector uses the same `transform` and `metrics` structure. The decoder
only selects how the response is parsed; it does not contain executable
collector logic. Python is a transform (`transform.type: python`) and its
script and optional library declarations live under `transform`, just like
`pre_script`. A Python transform emits its metric definitions dynamically
through `metric(...)`, so it MAY omit the `metrics` array entirely; an empty
`metrics: []` MUST NOT be required. A collector that needs Python only to parse
its response SHOULD instead reshape the response in `transform.pre_script` and
declare its metrics in the ordinary `metrics` array, as section 18 defines.

Collector names SHOULD use Prometheus-label-safe/simple names such as:

```text
elasticsearch_cluster
legacy_application
vendor_status
```

---
