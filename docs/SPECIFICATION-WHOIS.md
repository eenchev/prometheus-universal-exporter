# Prometheus Universal Exporter — `whois` Request Type Specification

This is the `whois` request type's section of § 5.1 of the
[exporter specification](SPECIFICATION-EXPORTER.md), in a document of its
own so that a change to the type is a change to this document. Everything
the request types share — the registry and the rules every type is held to
(§ 5.1), building without a type, decoding, transforms, error policies,
limits, caching, static targets and exposition — is in the exporter
specification, and a section number here, such as § 42.14, is one of its
sections. What a user reads of the type is [WHOIS.md](WHOIS.md).

## `whois`

A `whois` collector asks a WHOIS server (RFC 3912) for the registration of
the domain that is the probe's `target` and hands it to the transforms as a
JSON document. It MUST need nothing outside Go's standard library, and
MUST be opt-in, built only under the tag `request_type_whois`.

| Key | Default | Rule |
| --- | --- | --- |
| `whois_server` | — | `host`, `host:port` or `[v6]:port`, port 43 when left out; anything else refused at load. |
| `query_prefix` | — | At most 64 characters, no control character. |
| `max_response_bytes` | § 20 | The most bytes of one answer. |
| `allowed_targets`, `denied_targets` | — | As for `http`, on each server a question goes to: its name before the question and the address its connection is made to (§ 26.1). They are not asked of the domain. |

- Any other request key MUST be refused at load. A probe MUST take
  `timeout` and `param_<name>` parameters, and a static target's request
  block `timeout`.
- A target MUST be a domain name of two labels or more, whose case and
  trailing dot MUST NOT matter. A scheme, port, path, query, user, blank,
  IP address or character outside ASCII MUST be refused, a probe's with `400` before anything is sent
  and a static target's at load.
- A question MUST be one TCP connection: `query_prefix`, the domain and
  CRLF are sent, and the answer is read until the server closes. An answer
  past the response limit MUST fail the scrape wrapping the limit's error
  (§ 20), and a connection closed with no answer MUST fail it. The scrape's
  deadline — the probe's `timeout`, the scrape's own, 30 seconds when
  neither is set — MUST bound all its questions.
- With `whois_server` that server MUST be asked, and no other. Without it,
  `whois.iana.org:43` MUST be asked for the domain's top-level domain, and
  the server its answer names, by `refer` or else `whois`, asked for the
  domain; an answer that names none MUST fail the scrape, naming
  `request.whois_server`. What it named MUST be kept, per top-level domain,
  for at most a day and for at most 512 of them, and the server it named
  MUST be held to the target lists each time it is used.
- The answer's lines MUST be read as fields: `key: value`, the key before
  the first colon that ends one, at most 64 bytes and starting with a
  letter; `[key] value`; and, under a key with no value, the lines further
  in that are no field themselves as that key's values. A line of at most
  three words and 40 bytes that is no field, starts with a letter and has
  no punctuation MUST be read as such a key, written without its colon. A
  field further in under either MUST be kept under its own key and under
  the two together, the heading's and its own by a space. Keys MUST be
  lower-cased, their blanks single and trailing dots dropped. Lines that
  start with `%`, `#` or `;` MUST be passed over, and nothing MUST be read
  from a line that starts with `>>>` on. At most 256 keys and 32 values of
  a key MUST be kept.
- The answer MUST be a JSON document of `domain`; `server`, the server that
  answered, as `host:port`; `referred_by`, the server that named it, `""`
  with `whois_server`; `registered`; `registrar`; `registrar_iana_id`;
  `created`, `updated` and `expires`, as RFC 3339 in UTC, each with its
  `_timestamp_seconds`; `status`, each value without a URL after it;
  `name_servers`, in lower case, without addresses; `dnssec`; `fields`;
  `text`, the answer as it came; and `duration_seconds`.
- A date MUST be read from the first of the keys known for it that holds
  one that can be read, a registry's own before a registrar's, in UTC when
  it names no zone. One that cannot be read, and one no key holds, MUST be
  `null`, the date and its seconds both. A date of numbers that begins
  with the day and is not dotted MUST NOT be read.
- `registered` MUST be false only when the answer holds no date read, no
  registrar and no name server, and says the registry has no such domain;
  such an answer MUST NOT fail the scrape.
- The decoder MUST be `json`, as for `grpc`; another decoder, and a
  transform other than `jq`, `yq` and `python`, MUST be refused at load.
- A failure MUST be reported in the `whois` stage. Each question MUST be a
  request of a debug report and of an exported trace, shown as `QUERY` and
  `whois://server/question` with the bytes answered, or the error, as its
  outcome. The verbose `url` label MUST be `whois://` and the domain, and
  `http_method` MUST be `QUERY`.
