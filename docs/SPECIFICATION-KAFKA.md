# Prometheus Universal Exporter — `kafka` Request Type Specification

This is the `kafka` request type's section of § 5.1 of the
[exporter specification](SPECIFICATION-EXPORTER.md), in a document of its
own so that a change to the type is a change to this document. Everything
the request types share — the registry and the rules every type is held to
(§ 5.1), building without a type, decoding, transforms, error policies,
limits, caching, static targets and exposition — is in the exporter
specification, and a section number here, such as § 42.14, is one of its
sections. What a user reads of the type is [KAFKA.md](KAFKA.md).

## `kafka`

A `kafka` collector asks a Kafka cluster, through the broker that is the
probe's `target`, over Kafka's protocol, for its brokers, its topics'
partitions and offsets, and its consumer groups' state, members and
committed offsets, and hands the answer to the transforms as a JSON
document. It MUST need nothing outside Go's standard library, and MUST be
opt-in, built only under the tag `request_type_kafka`. It MUST read no
message and send no request that changes the cluster.

| Key | Default | Rule |
| --- | --- | --- |
| `sasl_mechanism` | see rule | `PLAIN`, `SCRAM-SHA-256`, `SCRAM-SHA-512` or `OAUTHBEARER`, or `""`, the key left out. Left out, `PLAIN` with a `username`, `OAUTHBEARER` with a `bearer_token_file`, and no login without either. `OAUTHBEARER` without `bearer_token_file`, and another mechanism without `username`, MUST be refused. |
| `username`, `password_file` | — | Both or neither; the file is read at each scrape, and an empty one fails it. |
| `bearer_token_file` | — | A token, read at each scrape; refused beside `username` or `password_file`, and with a placeholder. |
| `include_topics`, `exclude_topics`, `include_groups`, `exclude_groups` | — | At most 100 regular expressions each, each 1 to 1,000 bytes and one that compiles (RE2). A name is chosen when it matches one of the include list whole, or that list is left out, and none of the exclude list. |
| `internal_topics` | `false` | Choose the topics the metadata marks internal too. |
| `tls` | — | As for `http` (§ 42.14), used when the target's scheme is `tls`, on every connection; `cert_file` and `key_file` together. |
| `max_response_bytes` | § 20 | The most bytes the answers of one scrape come to, together. |
| `allowed_targets`, `denied_targets` | — | As for `http` (§ 26.1), on the target's name, on each other broker's name before its connection, and on every address a connection is made to. |

- Any other request key MUST be refused at load, and so MUST a placeholder
  in the account's keys. A probe MUST take `timeout` and `param_<name>`
  parameters, and a static target's request block `timeout`.
- A target MUST be `host`, `host:port`, `kafka://host[:port]` or
  `tls://host[:port]`, port 9092. Another scheme, a path, a query or a
  user MUST be refused, a probe's with `400` before anything is sent and a
  static target's at load.
- Every request MUST be sent at one version: Metadata 7, ListOffsets 4,
  OffsetFetch 5, DescribeGroups 2, ListGroups 2,
  ApiVersions 2, SaslHandshake 1, and SaslAuthenticate 1 where the broker
  speaks it, 0 otherwise. Each request MUST carry the client ID
  `prometheus-universal-exporter`, and each answer's correlation ID MUST be
  the request's.
- Every connection MUST first send ApiVersions. A broker that refuses it,
  or that does not speak each version above that the scrape sends, MUST
  fail what was to be asked of it, the refusal naming the request, the
  versions the broker speaks and Kafka 2.1. A first answer whose size is
  over 64 KiB MUST be read as no Kafka broker. A connection closed in place
  of that answer MUST fail saying that a TLS listener is written
  `tls://host:port`.
- With a mechanism, every connection MUST log in after ApiVersions, by
  SaslHandshake and SaslAuthenticate. A mechanism that the broker does not
  take MUST fail it, naming those it does. PLAIN MUST send the user and the
  password (RFC 4616), OAUTHBEARER the token (RFC 7628), and SCRAM MUST
  follow RFC 5802 with SHA-256 or SHA-512: a nonce of 24 random bytes, a
  user's `=` and `,` escaped. It MUST refuse a server nonce that does not
  extend the client's, more than 100,000 iterations, and a server signature
  that does not match. A refused login MUST say what the broker said and
  MUST NOT show the password or the token.
- A scrape MUST ask the target the cluster's metadata for every topic,
  without creating one. It MUST then ask every broker the metadata lists
  for the groups it coordinates, then the broker that listed each chosen
  group for its description and its committed offsets of every topic, and
  MUST NOT send FindCoordinator. Only after those MUST it ask each leader
  of a chosen topic's partition that has a leader for the partition's
  newest (-1) and oldest (-2) offsets, uncommitted records read. Each
  broker MUST be asked on a connection of its own, at the address the
  metadata gives it; a broker whose address no connection can use MUST
  fail what is asked of it, as a broker that cannot be reached does. The
  brokers MAY be asked at once, and every connection MUST be closed when
  the scrape ends.
- The target that cannot be reached, refused, or that fails its login or
  its metadata MUST fail the scrape. A failure of another broker MUST NOT
  fail it: what the broker would have answered MUST be missing, the
  partitions it leads MUST have their `error`, its groups their `error`,
  and `errors` MUST hold a sentence for it naming the broker. So MUST a
  group that its coordinator refused to describe or to give the offsets
  of, which keeps its `error`. The
  scrape's deadline running out, and the response limit, MUST fail the
  scrape whatever was being asked.
- The answers of a scrape together larger than the response limit MUST
  fail it wrapping the limit's error (§ 20), before the answer that passes
  it is read. More than 10,000 partitions of the chosen topics, or more
  than 1,000 chosen groups, MUST fail it, saying to narrow the patterns. An
  answer that does not decode, or whose arrays claim more items than its
  bytes hold, MUST fail what was asked as no Kafka protocol. What a broker
  said MUST be shown with its control characters replaced, credentials
  masked, and at most 200 bytes of it.
- The scrape's deadline — the probe's `timeout`, the scrape's own, 30
  seconds when neither is set — MUST bound the whole scrape.
- The answer MUST be a JSON document of these, each list sorted by its
  name, ID or index:
  - `address` (the address of the target's connection), `tls`,
    `cluster_id`, `controller`;
  - `brokers`: each `id`, `host`, `port`, `rack`, `controller`, and
    `leader_of` and `replicas`, counted over every topic;
  - `topics`, the chosen ones: each `name`, `internal`, `error`,
    `partition_count`, `replication_factor` (the most replicas of a
    partition), `offline`, `under_replicated`, `not_preferred_leader` (a
    leader other than the first replica, offline partitions not counted),
    `messages` (newest minus oldest summed, `null` when a partition lacks
    them), and `partitions`: each `partition`, `leader` (-1 for none),
    `replicas`, `isr`, `offline_replicas`, `preferred_leader`,
    `under_replicated` (fewer in sync than replicas), `offline`, `newest`,
    `oldest` and `error`;
  - `groups`, the chosen ones: each `group`, `protocol_type`, `state`,
    `members`, `coordinator` (the broker that listed it), `error`, and
    `topics`, the chosen topics it committed offsets of: each `group`,
    `topic`, `committed_sum` (of the offsets of 0 or more), `lag` (`null`
    when no partition has one), and `partitions`: each `partition`,
    `committed` (-1 for none), `lag` and `error`;
  - `errors` and `duration_seconds`.
- A partition's `lag` MUST be its newest offset minus the committed one,
  and 0 when that is negative. It MUST be `null` with no committed offset or
  no newest offset.
- The decoder MUST be `json`, as for `grpc`; another decoder, and a
  transform other than `jq`, `yq` and `python`, MUST be refused at load.
- A failure MUST be reported in the `kafka` stage. The scrape MUST be one
  request of a debug report and of an exported trace, shown as `METADATA`
  and `kafka://host:port`, `tls://` for a target written so, with the
  brokers, topics and groups it found, never with the password or the
  token. The verbose `url` label MUST be the same address, and
  `http_method` MUST be `METADATA`.
- `--dry-run` MUST warn of a password file, a token file, and the tls
  files of a collector that names any, that cannot be read, and MUST NOT
  fail on them.
