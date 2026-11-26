# Kafka

The `kafka` request type asks a Kafka cluster, over Kafka's own protocol,
how it stands:

- its brokers;
- its topics, with each partition's leader, replicas and offsets;
- its consumer groups, with their state, members and lag.

It hands the answer to the transforms as one JSON document.

It reads no message and writes nothing. Every request it sends is an admin
request that a monitoring tool sends, and `examples/kafka` writes the series
of [kafka_exporter](https://github.com/danielqsj/kafka_exporter) from it.

The type is opt-in: the default build and the published image leave it out
(see [Building with kafka](#building-with-kafka)).

## Building with kafka

`OPT_IN_REQUEST_TYPES` lists, comma-separated, the opt-in types that a
build adds:

```sh
make build OPT_IN_REQUEST_TYPES=kafka              # bin/prometheus-universal-exporter
docker build --build-arg OPT_IN_REQUEST_TYPES=kafka -t exporter:kafka .
go build -tags "$(sh tools/request-type-tags.sh '' '' kafka)" .
```

The build tag is `request_type_kafka`. A build without the type refuses a
configuration with a `kafka` collector at startup, on a reload and with
`--dry-run`, and the error says how to build one that has it. The type uses
only the standard library and adds no dependency: the protocol and the SASL
logins are written in the exporter.

## A collector

```yaml
collectors:
  - name: kafka
    request:
      type: kafka
      username: monitor
      password_file: /etc/exporter/kafka-password
      sasl_mechanism: SCRAM-SHA-512
      exclude_groups: ['console-consumer-.*']
    transform:
      type: jq
    metrics:
      - name: kafka_brokers
        description: Brokers in the cluster
        expression: .brokers | length
      - name: kafka_topic_under_replicated_partitions
        description: Partitions with fewer replicas in sync than they have
        items: .topics[]
        expression: .under_replicated
        labels:
          - name: topic
            expression: .name
      - name: kafka_consumergroup_lag_sum
        description: Messages a group has yet to consume, by topic
        items: .groups[] | .topics[] | select(.lag != null)
        expression: .lag
        labels:
          - name: consumergroup
            expression: .group
          - name: topic
            expression: .topic
```

```text
/probe?collector=kafka&target=kafka1.example.com:9092
```

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `kafka`. |
| `username`, `password_file` | — | The account of a PLAIN or SCRAM login, both or neither. The file is read at each scrape. |
| `bearer_token_file` | — | The token of an OAUTHBEARER login, in place of an account. The file is read at each scrape. |
| `sasl_mechanism` | see notes | `PLAIN`, `SCRAM-SHA-256`, `SCRAM-SHA-512` or `OAUTHBEARER`. Left out, `PLAIN` with a username, `OAUTHBEARER` with a token file, and no login without either. |
| `include_topics`, `exclude_topics` | every topic | Regular expressions ([RE2](https://github.com/google/re2/wiki/Syntax)), at most 100 each. A topic is reported when its whole name matches one of `include_topics`, or that key is left out, and none of `exclude_topics`. |
| `internal_topics` | `false` | Report Kafka's own topics too, such as `__consumer_offsets`. Without it they are left out whatever `include_topics` says. |
| `include_groups`, `exclude_groups` | every group | The same, for consumer groups. |
| `tls` | — | How each broker's certificate is verified, and a client certificate, as for [any type that speaks TLS](REQUESTS.md#tls), when the target says `tls://`. |
| `max_response_bytes` | `10MiB` | The most bytes the brokers' answers come to, together, in one scrape. |
| `allowed_targets`, `denied_targets` | — | The hosts the collector may and may not reach, as for [any type that reaches a host](REQUESTS.md#restricting-targets). They apply to every broker, by the name the cluster gives it and by the address it is reached at. |

No other request key applies, and setting one is a startup error. The
patterns, the account and the token's file take no placeholder. A probe
takes `timeout` and `param_<name>` parameters for the collector's
[label values](REQUESTS.md#in-label-values).

A pattern matches a whole name: `orders` matches the topic `orders` and not
`orders.eu`, which `orders.*` matches. A pattern that does not compile, an
empty one or one longer than 1,000 characters is refused when the
configuration loads.

The decoder is `json`, whether `decoder.type` is left out or set to it. Any
other decoder, and a transform that does not read JSON (`regex`, `csv`,
`css`, `prometheus`), is refused when the configuration loads; use `jq`,
`yq` or `python`.

## The target

The target is a broker of the cluster, to begin with: `host` or
`host:port`, port 9092 when it names none; `kafka://host:port`, the same; or
`tls://host:port` for a listener that speaks TLS. A path, a query, a user or
another scheme is refused with `400` before anything is sent, and a static
target's is refused when it loads.

The scrape reaches every other broker at the address the cluster's metadata
gives it: the listener's advertised host and port. Those must be reachable
from the exporter, as from any Kafka client.

- Over TLS, every connection is TLS, and each broker's certificate is
  verified for its own host, or for `tls.server_name` when that is set.
- Without TLS, a PLAIN login's password and an OAUTHBEARER token go in the
  clear, as Kafka's `SASL_PLAINTEXT` listeners take them. A SCRAM login
  never sends the password.

## What a scrape does

1. It connects to the target and asks which versions of each request the
   broker speaks (`ApiVersions`). It then logs in (`SaslHandshake`,
   `SaslAuthenticate`) when the collector has an account or a token.
2. It asks for the cluster's metadata (`Metadata`): the brokers, the
   controller, the topics, and each partition's leader, replicas, replicas
   in sync and offline replicas.
3. It asks every broker for the consumer groups that it coordinates
   (`ListGroups`). It then asks the broker that listed each chosen group,
   its coordinator, for the group's state and members (`DescribeGroups`)
   and for its committed offsets (`OffsetFetch`).
4. It asks each partition's leader for the partition's newest and oldest
   offsets (`ListOffsets`). It does so after the committed offsets, so that
   a lag read at once is not negative.

Each broker is reached on its own connection, logged in like the first.
The brokers are asked at once, and every connection is closed when the
scrape ends.

Each request is sent at the version a Kafka 2.1 client sends: Metadata v7,
ListOffsets v4, OffsetFetch v5, DescribeGroups v2, ListGroups v2 and
ApiVersions v2. Kafka 4 keeps those versions and removed
the older ones, so one version of each reaches every broker from 2.1 on, and
Redpanda and the cloud services that speak Kafka's protocol too.

## The document

```json
{
  "address": "192.0.2.10:9092",
  "tls": false,
  "cluster_id": "MkU3OEVBNTcwNTJENDM2Qg",
  "controller": 1,
  "brokers": [
    {"id": 1, "host": "kafka1.example.com", "port": 9092, "rack": "eu-1a", "controller": true, "leader_of": 41, "replicas": 120}
  ],
  "topics": [
    {"name": "orders", "internal": false, "error": "", "partition_count": 3, "replication_factor": 3,
     "offline": 0, "under_replicated": 1, "not_preferred_leader": 1, "messages": 160,
     "partitions": [
       {"partition": 0, "leader": 1, "replicas": [1, 2, 3], "isr": [1, 2, 3], "offline_replicas": [],
        "preferred_leader": true, "under_replicated": false, "offline": false, "newest": 100, "oldest": 10, "error": ""}
     ]}
  ],
  "groups": [
    {"group": "billing", "protocol_type": "consumer", "state": "Stable", "members": 2, "coordinator": 2, "error": "",
     "topics": [
       {"group": "billing", "topic": "orders", "committed_sum": 145, "lag": 10,
        "partitions": [{"partition": 0, "committed": 90, "lag": 10, "error": ""}]}
     ]}
  ],
  "errors": [],
  "duration_seconds": 0.021
}
```

**Brokers.**

- `leader_of` and `replicas` count the partitions that a broker leads and
  holds a replica of, over every topic, chosen or not.
- `rack` is `null` for a broker without one.

**Topics.** These are the chosen topics, by name.

- `offline` counts the partitions without a leader.
- `under_replicated` counts the partitions with fewer replicas in sync than
  they have.
- `not_preferred_leader` counts the partitions led by a broker other than
  the first of their replicas, which a leader election moves back.
- `messages` is the newest offset minus the oldest over the partitions.
  This is what the topic holds, less what compaction removed. It is `null`
  when a partition's offsets are missing.
- A partition's `newest` and `oldest` are `null` while it is offline, or
  when its leader could not be asked; `error` then says why.
- A topic that the metadata refused, such as one the account may not
  describe, has its `error`, for example `TOPIC_AUTHORIZATION_FAILED`, and
  no partitions.

**Groups.** These are the chosen groups, by name, with every topic of
theirs that is chosen.

- `lag` is the newest offset minus the committed one, and a topic's `lag`
  is the sum over its partitions. A committed offset past the newest, which
  a commit made between the two requests can be, reads as no lag.
- A partition that the group has committed nothing to has `committed: -1`
  and `lag: null`, and counts in neither sum. A topic's `lag` is `null`
  when none of its partitions has a lag.
- `coordinator` is the ID of the broker that listed the group.
- A group whose coordinator could not be asked, or that it refused to
  describe, such as one the account may not describe, has its `error`, and
  `state` and `members` are `null`. Its committed offsets and lag are kept
  when the coordinator gave them.

**Errors.** A scrape that reaches the target but not every other broker does
not fail. What the missing broker would have said is missing from the
document. `errors` lists each broker that could not be asked and each
group that could not be described, as a sentence naming the broker or the
group and the request. A partition whose offsets are missing says why in
its own `error`. The groups that a missing
broker coordinates are missing too, since only it lists them. A rule such
as `.errors | length` tells a partial answer apart.

## Static targets

A [static target](STATIC-TARGETS.md) of a `kafka` collector names a broker
of its cluster, and may set `timeout` under its `request`. One target is
one cluster:

```yaml
interval: 30s
targets:
  - name: kafka_eu
    collector: kafka
    target: kafka1.eu.example.com:9092
    labels:
      cluster: eu
  - name: kafka_us
    collector: kafka
    target: tls://kafka1.us.example.com:9093
    labels:
      cluster: us
```

## Limits

A scrape is bounded by these:

- **Its deadline:** the probe's `timeout`, or the scrape's own, 30 seconds
  when neither is set.
- **The response limit,** `request.max_response_bytes` or
  `limits.max_response_bytes`: the brokers' answers together may come to no
  more than this, or the scrape fails.
- **Partitions and groups:** at most 10,000 partitions of the chosen topics
  and 1,000 chosen groups are reported. More fail the scrape, saying to
  narrow the patterns.
- **SCRAM's iterations:** a login hashes the password at most 100,000 times,
  as a broker asks. Kafka itself asks at most 16,384.

## Errors and self-metrics

A failed request is reported in the `kafka` stage and follows
`error_handling.on_fetch_error`, naming the broker. Causes include:

- a target that cannot be reached, or a TLS handshake that failed;
- an answer that is not Kafka's protocol;
- a broker older than Kafka 2.1:
  `kafka1:9092 speaks Metadata v0 to v5, not v7, which the kafka type sends; it needs Kafka 2.1 or later`;
- a login that it refused:
  `kafka1:9092 refused the SCRAM-SHA-512 login of "monitor": Authentication failed during authentication due to invalid credentials with SASL mechanism SCRAM-SHA-512`;
- a mechanism that it does not take, naming those it does;
- answers past the response limit, or too many partitions or groups;
- a credential file that cannot be read or is empty.

A target that the target lists refuse fails the scrape, is answered `403`
and counts in `http_exporter_targets_refused_total`. Another broker that they
refuse is one of the document's `errors`, and the scrape goes on without
it.
Transforms see no status (`$status` is `null`, and Python's
`response.status_code` is `None`) and the header
`Content-Type: application/json`.

A [debug probe](CONFIGURATION.md#debug-probes) shows the scrape with how it
ended:

```text
Requests
  1. METADATA kafka://kafka1.example.com:9092 -> 3 brokers, 12 topics, 4 groups in 21ms
```

With [verbose self-metrics](SELF-METRICS.md#verbose-per-request-self-metrics)
a request is labelled `url="kafka://kafka1.example.com:9092"`, `tls://` for
a target written so, and `http_method="METADATA"`.

## kafka_exporter's series

[`examples/kafka`](../examples/kafka/config.yaml) writes the series of
kafka_exporter from the document, with the same names and labels:

- `kafka_brokers` and `kafka_broker_info`;
- `kafka_topic_partitions`;
- each partition's offsets, replicas, leader and replication
  (`kafka_topic_partition_*`);
- each group's offsets, lag and members (`kafka_consumergroup_*`), `-1`
  for a partition without a committed offset as it writes it.

Its dashboards and alerts read them as they read its own. Its
`--topic.filter`, `--topic.exclude`, `--group.filter` and `--group.exclude`
are `include_topics`, `exclude_topics`, `include_groups` and
`exclude_groups`. Its `--offset.show-all=false` is a rule's
`select(.members > 0)`.

One exporter reads many clusters, each a target, and the
[target allocator](ALLOCATOR.md) spreads them over replicas, where
kafka_exporter runs a process for each cluster. Some of what it does is not
done here: see [Limitations](#limitations).

## Limitations

- **No Kerberos (GSSAPI) login.** It would need a Kerberos library. A
  cluster that takes nothing else cannot be read.
- **No AWS IAM login** (`AWS_MSK_IAM`, MSK's IAM listeners on port 9098).
  An MSK cluster's SCRAM listener, port 9096, is read with
  `SCRAM-SHA-512`.
- **Kafka 2.1 or later.** An older broker is refused, saying so.
- **Offsets kept in ZooKeeper are not read.** Only consumers older than
  Kafka 0.9 commit there, and Kafka 4 has no ZooKeeper.
- **Lag is in messages, not in time.** A rate of the newest offsets, in
  PromQL, turns it into time.
- **No disk use and no topic configuration.** The brokers' log directories
  and a topic's retention are not asked for.
- **The groups of Kafka 4's new consumer protocol** (KIP-848, `group.protocol=consumer`)
  are described by a request the type does not send. Their committed
  offsets and lag are read, but their `state` and `members` may be `null`,
  with an `error`.

## Troubleshooting

- **`closed the connection while reading the answer to ApiVersions … a
  target of one is written tls://host:port`.** The listener speaks TLS;
  write the target with `tls://`.
- **`closed the connection while reading the answer to Metadata`.** The
  broker takes only logged-in clients; set the account or the token, and
  `sasl_mechanism`.
- **`takes no SASL login on this listener`.** The listener is Kafka's
  `PLAINTEXT` or `SSL` one, which takes no login; leave the account and
  the token out, or reach the `SASL_PLAINTEXT` or `SASL_SSL` listener,
  which is often on another port.
- **`refused the OAUTHBEARER login of the token: {"status":"invalid_token"}`.**
  The broker does not take the token: it has expired, or it was issued
  for another audience or by an issuer the broker does not trust. The
  token file is read at each scrape, so a renewed token is used at the
  next.
- **`does not take the SASL mechanism PLAIN …; it takes SCRAM-SHA-512`.**
  Set `sasl_mechanism` to one it takes.
- **`errors` says `connecting to kafka-0.kafka-headless:9092`.** The
  brokers advertise names that the exporter cannot resolve or reach: run it
  where the brokers' advertised listeners are reachable, or give the
  cluster a listener that advertises such names.
- **A group is missing.** Only its coordinator lists it: when that broker
  cannot be reached, `errors` names it. Kafka also forgets a group that
  has no members once its committed offsets expire
  (`offsets.retention.minutes`, a week by default).
- **`lag` is `null`.** The group has committed no offset of the partition,
  or its leader could not be asked; the partition's `error` says which.
