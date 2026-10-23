# LDAP directories

The `ldap` request type searches an LDAP directory — OpenLDAP, Active
Directory, 389 Directory Server or any other server of LDAPv3 — and hands
what its searches find to `jq`, `yq` or `python` as one JSON document: the
entries of each search, with the attributes asked for, or only how many
there are. Use it where a directory's own numbers, or the size of what it
holds, are what you want as metrics, and a dedicated exporter for it is one
more thing to run:

- an OpenLDAP server's connections, operations and bytes sent, from the
  counters of its `cn=Monitor` backend;
- the accounts of Active Directory that are disabled, locked or about to
  expire, counted with a filter;
- the members of a group, such as the administrators, so that one more is
  seen.

The type is opt-in: the default build and the published image leave it out,
so a build carries it only when asked
([Building with ldap](#building-with-ldap)). It needs nothing outside Go's
standard library.

## Building with ldap

`OPT_IN_REQUEST_TYPES` names the opt-in types a build adds to the types it
carries, comma-separated:

```sh
make build OPT_IN_REQUEST_TYPES=ldap                # bin/prometheus-universal-exporter
make build OPT_IN_REQUEST_TYPES=s3file,redis,ldap   # every opt-in type
docker build --build-arg OPT_IN_REQUEST_TYPES=ldap -t exporter:ldap .
go build -tags "$(sh tools/request-type-tags.sh '' '' ldap)" .
```

The tag is `request_type_ldap`, which is all `go build -tags
request_type_ldap .` needs by hand: every type of the default build, and
`ldap`. `OPT_IN_REQUEST_TYPES` goes with `REQUEST_TYPES` (see [Choosing
request types at build time](CONFIGURATION.md#choosing-request-types-at-build-time)):
`REQUEST_TYPES=http OPT_IN_REQUEST_TYPES=ldap` builds the two, and
`REQUEST_TYPES` may name `ldap` itself, `REQUEST_TYPES=ldap` building it
alone.

A configuration with an `ldap` collector is refused by a build without the
type, at startup, on a reload and by `--dry-run`, saying how to build one
that has it:

```text
collector "openldap" uses request.type "ldap", which this build of the exporter does not include; it was built with: dns, graphite, grpc, http, kubernetes, localfile, sql, sshfile, tlscert. ldap is an opt-in type, which the default build and the published image leave out: build with OPT_IN_REQUEST_TYPES=ldap (make build, or the Dockerfile's build argument), which adds the tag request_type_ldap
```

## A collector

An OpenLDAP server's [`cn=Monitor`](https://www.openldap.org/doc/admin26/monitoringslapd.html)
backend, its counters and its operations, read by an account the backend's
access rules let read it:

```yaml
collectors:
  - name: openldap
    request:
      type: ldap
      bind_dn: cn=monitor,dc=example,dc=org
      password_file: /var/run/secrets/ldap/password
      searches:
        - name: counters
          base: cn=Monitor
          filter: (objectClass=monitorCounterObject)
          attributes: [monitorCounter]
        - name: operations
          base: cn=Operations,cn=Monitor
          scope: one
          filter: (objectClass=monitorOperation)
          attributes: [monitorOpInitiated, monitorOpCompleted]
    transform:
      type: jq
    metrics:
      - name: openldap_connections_current
        description: Connections open now
        expression: '.searches.counters.entries[] | select(.dn == "cn=Current,cn=Connections,cn=Monitor") | .attributes.monitorCounter[0] | tonumber'
      - name: openldap_connections_total
        type: counter
        description: Connections accepted since the server started
        expression: '.searches.counters.entries[] | select(.dn == "cn=Total,cn=Connections,cn=Monitor") | .attributes.monitorCounter[0] | tonumber'
      - name: openldap_statistics_total
        type: counter
        description: Bytes, PDUs, referrals and entries sent since the server started
        items: '.searches.counters.entries[] | select(.dn | endswith(",cn=Statistics,cn=Monitor"))'
        expression: '.attributes.monitorCounter[0] | tonumber'
        labels:
          - name: statistic
            expression: '.dn | capture("^cn=(?<name>[^,]+)").name | ascii_downcase'
      - name: openldap_operations_completed_total
        type: counter
        description: Operations completed since the server started, by kind
        items: .searches.operations.entries[]
        expression: '.attributes.monitorOpCompleted[0] | tonumber'
        labels:
          - name: operation
            expression: '.dn | capture("^cn=(?<name>[^,]+)").name | ascii_downcase'
```

```text
/probe?collector=openldap&target=ldap.example.org
```

`monitorCounter`, `monitorOpInitiated` and `monitorOpCompleted` are
operational attributes, which a server sends only when they are asked for
by name, or with `+`.

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `ldap`. |
| `bind_dn` | none: an anonymous bind | The DN of the account the scrape binds as with a simple bind, such as `cn=exporter,ou=services,dc=example,dc=org`. Only with `password_file`. See [Binding](#binding). |
| `password_file` | none | The file of the account's password, read at every scrape, its surrounding whitespace taken off. Only with `bind_dn`. |
| `start_tls` | `false` | Begin TLS with the StartTLS operation on a plaintext connection: see [TLS](#tls). |
| `searches` | — | **Required.** The searches whose results the document holds: see [Searches](#searches). |
| `tls` | none | Verify the server, and present a client certificate: see [TLS](#tls). |
| `max_response_bytes` | `10MiB` | The most bytes of the server's messages to one scrape, every page of every search together: see [Limits](#limits). |
| `allowed_targets`, `denied_targets` | none | The servers the collector may and may not reach, as for [any type that reaches a host](REQUESTS.md#restricting-targets), checked on the connection. |

No other request key applies, and setting one is a startup error. No
password is written in the configuration: there is no `password` key. The
decoder is `json`, whether `decoder.type` is left out or set to it; any other
decoder, and a transform that does not read JSON, is refused when the
configuration loads. Use `jq`, `yq` or `python`.

## Binding

Each scrape binds before its searches, with a simple bind: as `bind_dn`
with the password of `password_file`, or, without them, anonymously, with
an empty DN and an empty password, which also tells a server that the
client speaks LDAPv3. `bind_dn` and `password_file` go together, each
refused without the other, and a password file that is empty, or blanks
alone, fails the scrape before anything is sent: a simple bind with a DN
and an empty password is an *unauthenticated* bind (RFC 4513 § 5.1.2),
which many servers take without checking anything and answer `success`, so
the searches after it would run as nobody, finding less than they should,
with nothing to say why. A password the server refuses fails the scrape
with `invalidCredentials` and the server's words, never the password.

There is no SASL bind: no Kerberos, no `EXTERNAL` with a client
certificate. A client certificate in `tls` is presented in the TLS
handshake, which a server may require, but the bind is still the simple
one; send it over TLS ([TLS](#tls)), since a simple bind's password is
otherwise sent as it is.

## The target

The probe's `target` is the server: `host`, `host:port`, `[v6]:port`, or an
`ldap://` or `ldaps://` URL of one, port 389, or 636 for TLS from the start,
when it names none. It is required. A user, a password, a DN or a query in
the target is refused with `400` — the base is each search's, the account
`bind_dn` — and so is any other scheme. `allowed_targets` and
`denied_targets` are matched against the host before the connection is
made and against the address it is made to, and the cloud metadata service
is refused without them, as for `http`.

## TLS

A scrape speaks TLS from the start with an `ldaps://` target, or with a
bare `host:port` when the collector has a `tls` block; it begins TLS on a
plaintext connection, port 389, with the StartTLS operation when the
collector sets `start_tls`, before it binds, with `tls` or without. Either
way the server's certificate is verified against the system's roots and
`tls.ca_file`, for the name `tls.server_name` or else the target's host.
The block is [the one `http` takes](REQUESTS.md#tls): `cert_file` and
`key_file` present a client certificate, and `insecure_skip_verify` skips
the verification.

```yaml
request:
  type: ldap
  start_tls: true
  tls:
    ca_file: /etc/exporter/ldap-ca.pem
  bind_dn: cn=exporter,ou=services,dc=example,dc=org
  password_file: /var/run/secrets/ldap/password
  searches:
    - name: people
      base: ou=people,dc=example,dc=org
      count_only: true
```

`ldap://` with a `tls` block and no `start_tls` is refused, the two saying
opposite things, and so is `ldaps://` with `start_tls`, TLS being begun
twice. A server that refuses StartTLS fails the scrape, with the server's
words, before the password is sent.

## Searches

`searches` lists the searches a scrape makes, in order, each answered
before the next is sent:

| Key | Default | Notes |
| --- | --- | --- |
| `name` | — | **Required.** The name the result is put under in the document, letters, digits and `_`, not starting with a digit, unique among the collector's searches. |
| `base` | `""`, the root DSE | The DN the search starts at, such as `ou=people,dc=example,dc=org`; `""` with `scope: base` reads the root DSE, the server's `namingContexts` and `supportedControl` among its attributes. |
| `scope` | `sub` | `base`, the base entry alone; `one`, the entries right under it; `sub`, the base and everything under it. |
| `filter` | `(objectClass=*)` | The filter the entries are matched with, as [RFC 4515](https://www.rfc-editor.org/rfc/rfc4515) writes one, in parentheses: `&`, `\|`, `!`, `=`, `=*`, substrings with `*`, `>=`, `<=`, `~=` and extensible match, such as `(userAccountControl:1.2.840.113556.1.4.803:=2)`; a `(`, `)`, `*` or `\` in a value is written `\28`, `\29`, `\2a` or `\5c`. |
| `attributes` | none: the DNs alone | The attributes each entry is read with, such as `[cn, mail]`, by name or OID, `*` for every user attribute and `+` for every operational one. Left out, the search asks for `1.1`, no attribute at all. |
| `count_only` | `false` | Count the entries, holding none of them: the result has no `entries`. Not with `attributes` or `max_entries`. |
| `max_entries` | `1000` | The most entries a search that is not `count_only` may match; one more fails the scrape, asking for a narrower filter or `count_only`. |

A search is read a page of 500 entries at a time with the Simple Paged
Results control ([RFC 2696](https://www.rfc-editor.org/rfc/rfc2696)), sent
as not critical: a server that has it, as OpenLDAP, Active Directory and 389
Directory Server do, answers page after page, so a search finds more than
the server's size limit for one answer, Active Directory's 1000 among them;
a server without it answers every entry in one go. Continuation references
a search answers are counted, never followed.

The answer is a JSON document:

```json
{
  "searches": {
    "admins": {
      "count": 1,
      "references": 0,
      "entries": [
        {"dn": "cn=admins,ou=groups,dc=example,dc=org", "attributes": {"member": ["uid=ada,ou=people,dc=example,dc=org", "uid=alan,ou=people,dc=example,dc=org"]}}
      ]
    },
    "disabled": {"count": 12, "references": 0}
  }
}
```

- `count` is the number of entries the search matched, across every page.
- `references` is the number of continuation references it answered.
- `entries`, unless the search is `count_only`, holds each entry in the
  order the server sent them: its `dn`, and its `attributes`, each a list
  of its values as text, an attribute with one value a list of one.
  Attributes are named as the server writes them, which need not be how
  `attributes` asks for them: most servers write the schema's name,
  `objectClass` for an `objectclass` asked for, and a `jq` path matches
  case, so read the name a [debug probe](CONFIGURATION.md#debug-probes)
  shows in the document.
  An attribute an entry does not have is not in its `attributes`, and
  `{}` is an entry with none of those asked for, or a search asking for
  none.

A value that is not UTF-8 text fails the scrape, naming the attribute and
the entry: the document is JSON, whose strings are text, and a value put
in it would have each such byte replaced with U+FFFD. Leave binary
attributes, such as Active Directory's `objectGUID` and `objectSid` or a
person's `jpegPhoto`, out of `attributes` — and out of what `*` asks for, by
naming the attributes instead.

### Placeholders

A search's `base` and `filter` may hold `{{param_name}}` placeholders,
filled from the probe's `param_<name>` parameters, as a path's are
([Path parameters](REQUESTS.md#path-parameters)). A placeholder may stand
in a value alone: in the filter, an assertion's value, filled escaped as
RFC 4515 escapes one, so a probe's `*)(uid=*` is matched as those eight
characters, `\2a\29\28uid=\2a`, and widens nothing; in the base, an
attribute's value, filled escaped as RFC 4514 escapes one, so a probe's
`people,dc=other` stays one RDN's value, `people\,dc\=other`. A placeholder
in an attribute's name, in the filter's structure, as a whole DN, or in any
other key — `name`, `scope`, `attributes`, `bind_dn` — is refused when the
configuration loads.

```yaml
searches:
  - name: user
    base: ou={{param_unit:people}},dc=example,dc=org
    filter: (&(objectClass=person)(uid={{param_user}}))
    count_only: true
```

## Examples

The accounts of Active Directory that are disabled — those whose
`userAccountControl` has the bit `ACCOUNTDISABLE`, 2 — and those locked out,
counted, with the bitwise AND matching rule of Active Directory, by an
account that may read the users:

```yaml
  - name: ad_accounts
    request:
      type: ldap
      bind_dn: CN=prometheus,OU=Service Accounts,DC=corp,DC=example,DC=com
      password_file: /var/run/secrets/ldap/password
      searches:
        - name: disabled
          base: DC=corp,DC=example,DC=com
          filter: (&(objectCategory=person)(objectClass=user)(userAccountControl:1.2.840.113556.1.4.803:=2))
          count_only: true
        - name: locked
          base: DC=corp,DC=example,DC=com
          filter: (&(objectCategory=person)(objectClass=user)(lockoutTime>=1))
          count_only: true
    transform:
      type: jq
    metrics:
      - name: ad_accounts_disabled
        description: User accounts that are disabled
        expression: .searches.disabled.count
      - name: ad_accounts_locked
        description: User accounts locked out by failed logins
        expression: .searches.locked.count
```

```text
/probe?collector=ad_accounts&target=ldaps://dc1.corp.example.com
```

The members of a group, read from its `member` attribute, the group named by
the probe:

```yaml
  - name: group_members
    request:
      type: ldap
      bind_dn: cn=exporter,ou=services,dc=example,dc=org
      password_file: /var/run/secrets/ldap/password
      searches:
        - name: group
          base: cn={{param_group}},ou=groups,dc=example,dc=org
          scope: base
          attributes: [member]
    transform:
      type: jq
    metrics:
      - name: ldap_group_members
        description: Members of the group
        expression: '[.searches.group.entries[].attributes.member // [] | length] | add // 0'
        labels:
          - name: group
            value: "{{param_group}}"
```

```text
/probe?collector=group_members&target=ldap.example.org&param_group=admins
```

Active Directory answers a `member` of more than 1500 values in ranges,
`member;range=0-1499`, which the document holds under that name; count
such a group's members with a `count_only` search of the users whose
`memberOf` is the group instead,
`(memberOf=CN=Admins,OU=Groups,DC=corp,DC=example,DC=com)`.

## Static targets

A [static target](STATIC-TARGETS.md) of an `ldap` collector names the server
as its `target`, which it requires, and may set under its `request` only
`timeout`. The account, the TLS settings and the searches are the
collector's, and `params` fills its placeholders:

```yaml
interval: 60s
targets:
  - name: openldap_primary
    collector: openldap
    target: ldap.example.org
  - name: admins
    collector: group_members
    target: ldaps://ldap.example.org
    params:
      param_group: admins
```

## Limits

Everything the server answers a scrape is read against
`max_response_bytes`, or `limits.max_response_bytes`, 10 MiB when neither is
set, counted in the bytes of its messages as they come off the connection:
the bind's and StartTLS's answers, and every page of every search, a
`count_only` search's among them, whose entries cost their DNs' bytes and a
few more each, since it asks for no attribute. A message whose declared
length is past what is left fails the scrape before a byte of it is read,
and no more is held for one than the server has sent of it, so a broken or
hostile server's length of a gigabyte costs the bytes that came, never a
gigabyte. Either counts in `http_exporter_series_limit_exceeded_total` as a
limit exceeded. An entry of a `count_only` search takes its DN's length and
some 15 bytes more, so counting a million entries of 50-byte DNs reads some
65 MiB: raise `max_response_bytes` for such a count, or count fewer, in a
narrower base.

A `count_only` search holds no entry, so `max_entries` does not apply to
it; a search that holds its entries may match at most `max_entries` of them,
1000 by default, and fails the scrape at the next one, before the rest are
read, asking for a narrower base or filter, a larger `max_entries` or
`count_only`.

A message is decoded no deeper than the protocol's own structures go, each
element's length checked against what encloses it, and LDAP's encoding is
held to: a length of more than four bytes, an indefinite length, a tag of
more than one byte and a value that is not a primitive string are refused,
and so is a message that is not the answer the scrape is waiting for. Any
of them fails the scrape saying it is not an LDAP message. A filter nests
`&`, `|` and `!` at most 32 deep.

The document the entries make is held to the same limit: JSON writes a
control character as six bytes (`\u0001`) and a quote or a backslash as two,
so messages within the limit can make a document past it, which fails the
scrape as a limit exceeded too, saying how large the document was.

The connection, the TLS handshake and every operation end at the probe's
deadline, its `timeout` or the scrape's, 30 seconds without one, and each
search tells the server the seconds left, as its time limit: a server that
stops answering fails the scrape when it comes, with the deadline's error
and the operation it was waiting on.

## Errors and self-metrics

A failed scrape is reported in the `ldap` stage and follows
`error_handling.on_fetch_error`, naming the operation and what went wrong.
An operation the server answers with a result other than `success` fails the
scrape with the result's name and number and the server's diagnostic
message:

```text
BIND cn=exporter,dc=example,dc=org ***: the server answered invalidCredentials (49): 80090308: LdapErr: DSID-0C09044E, comment: AcceptSecurityContext error, data 52e, v4563
SEARCH people base="ou=people,dc=example,dc=org" scope=sub filter=(objectClass=person) attributes=1.1: the server answered noSuchObject (32)
```

A server the target lists refuse counts in
`http_exporter_targets_refused_total`, answered `403`. Transforms see no
status (`$status` is `null`, Python's `response.status_code` `None`) and the
header `Content-Type: application/json`.

A [debug probe](CONFIGURATION.md#debug-probes) shows each operation in its
request section — `STARTTLS`, `BIND` with its DN and its password as `***`,
or `BIND anonymous`, and each page of each `SEARCH` with its base, scope,
filter and attributes, a placeholder's value escaped in it — and how each
ended:

```text
Requests
  1. STARTTLS ldap://ldap.example.org:389 -> success, TLS 1.3 in 4ms
     Ldap-Operation: STARTTLS
  2. BIND ldap://ldap.example.org:389 -> success in 1ms
     Ldap-Operation: BIND cn=exporter,ou=services,dc=example,dc=org ***
  3. SEARCH ldap://ldap.example.org:389 (page 1) -> success, 500 entries, more to come in 12ms
     Ldap-Operation: SEARCH people base="ou=people,dc=example,dc=org" scope=sub filter=(objectClass=person) attributes=1.1
  4. SEARCH ldap://ldap.example.org:389 (page 2) -> success, 212 entries in 6ms
     Ldap-Operation: SEARCH people base="ou=people,dc=example,dc=org" scope=sub filter=(objectClass=person) attributes=1.1
```

With [verbose self-metrics](SELF-METRICS.md#verbose-per-request-self-metrics)
a scrape is labelled `url="ldap://host:port"`, `ldaps://` for TLS from the
start, and `http_method="SEARCH"`.

## Limitations

- **Simple bind only.** No SASL: no Kerberos (GSSAPI), no NTLM, no
  `EXTERNAL` bind with a client certificate.
- **No referrals followed.** A search whose base another server holds, which
  the server answers with `referral`, fails the scrape; continuation
  references are counted, not followed. Make the target the server that
  holds the base, or for Active Directory a global catalog, port 3268.
- **Text values only.** A value that is not UTF-8 fails the scrape: leave
  binary attributes out of `attributes`.
- **Ranged attributes are not followed.** Active Directory's
  `member;range=0-1499` holds the first values of a large attribute alone.
- **One connection per scrape.** No connection is kept between scrapes, so
  each costs a TCP handshake, a TLS one with TLS, and a bind.

## Troubleshooting

| The scrape says | Do |
| --- | --- |
| `... uses request.type "ldap", which this build of the exporter does not include` | Build with `OPT_IN_REQUEST_TYPES=ldap` (see [Building with ldap](#building-with-ldap)); the published image does not carry it. |
| `BIND ... ***: the server answered invalidCredentials (49): ...` | The password, or the DN: Active Directory's `data 52e` says the password is wrong, `data 533` that the account is disabled. |
| `SEARCH ...: the server answered insufficientAccessRights (50): ...` | The account may not search there: Active Directory refuses an anonymous search; set `bind_dn`. |
| `SEARCH ...: the server answered noSuchObject (32)` | The base is not an entry of the server's: check it, and that the server holds it. |
| `... referral (10); the exporter follows no referral` | Make the target the server that holds the base. |
| `... sizeLimitExceeded (4); the server's own size limit ...` | The server limits what the account may read even page by page, as OpenLDAP's `sizelimit size.pr` does: narrow the filter, or raise the limit. |
| `... matches more than its max_entries, 1000; ...` | Narrow the base or filter, raise `max_entries`, or set `count_only`. |
| `... the attribute objectGUID of ... holds bytes that are not UTF-8 text ...` | Leave the binary attribute out of `attributes`. |
| `... the server's messages are larger than the response limit ...` | Raise `max_response_bytes`, ask for fewer attributes, or narrow the searches. |
| `STARTTLS: the server answered protocolError (2): ...` | The server has no TLS on that port: use `ldaps://`, or configure TLS on the server. |
| `TLS handshake with ...: ... certificate signed by unknown authority` | Name the server's CA in `tls.ca_file`. |
| `... a message begins with 0x48, which no LDAP message does; is the target an LDAP server?` | The target is not an LDAP server: the port, or `ldaps://` for a server that speaks TLS from the start. |
