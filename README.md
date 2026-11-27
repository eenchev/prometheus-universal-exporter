<img src="docs/logo.svg" alt="prometheus-universal-exporter" width="104">

# prometheus-universal-exporter

[![CI](https://img.shields.io/github/actions/workflow/status/eenchev/prometheus-universal-exporter/ci.yml?branch=main&label=CI&logo=github)](https://github.com/eenchev/prometheus-universal-exporter/actions/workflows/ci.yml)
[![Artifact Hub](https://img.shields.io/endpoint?url=https://artifacthub.io/badge/repository/prometheus-universal-exporter)](https://artifacthub.io/packages/search?repo=prometheus-universal-exporter)
[![Exporter release](https://img.shields.io/github/v/release/eenchev/prometheus-universal-exporter?filter=exporter%2F*&label=exporter&color=fe7d37)](https://github.com/eenchev/prometheus-universal-exporter/releases)
[![Chart release](https://img.shields.io/github/v/release/eenchev/prometheus-universal-exporter?filter=chart%2F*&label=chart&color=fe7d37&logo=helm&logoColor=white)](https://github.com/eenchev/prometheus-universal-exporter/releases)
[![Go](https://img.shields.io/github/go-mod/go-version/eenchev/prometheus-universal-exporter?logo=go&logoColor=white)](go.mod)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

Turn almost any source into a Prometheus target, without writing an exporter for it.

A **collector** says how to query a source, how to parse the answer and which
metrics to make of it. Everything is configuration, so new metrics need no
code, no rebuild and no redeploy.

**What it can read** (opt-in types are added
[at build time](docs/CONFIGURATION.md#choosing-request-types-at-build-time)):

| | Default build | Opt-in build |
| --- | --- | --- |
| **Web and APIs** | HTTP APIs answering JSON, NDJSON, YAML, TOML, INI, XML, CSV, HTML, logfmt, plain text, InfluxDB line protocol, protobuf or Prometheus exposition; gRPC services; Graphite servers; Kubernetes objects of any kind | WebSocket endpoints |
| **Files** | Local files, such as the `.prom` files of batch jobs; files on another host over SSH; objects in Amazon S3 or an S3-compatible store | Files on an FTP server |
| **Data stores** | SQL databases such as MySQL, PostgreSQL and SQL Server; Redis and Valkey; LDAP directories such as OpenLDAP and Active Directory | |
| **Network checks** | Services that answer a line over TCP or UDP, such as memcached, ZooKeeper and HAProxy; open ports; ping (ICMP); DNS records; TLS certificates and their expiry; NTP clocks; SNMP agents (v1, v2c, v3) | Mail servers (SMTP), mailboxes (IMAP, POP3), RADIUS logins, domain registrations (WHOIS), SIP servers |
| **Hosts** | Commands run on another host over SSH | |
| **Messaging and devices** | | MQTT topics, AMQP queues, NATS with JetStream, Kafka consumer lag, Modbus TCP devices |

## Try it

From a checkout, with Go installed, run one of the examples in [`examples/`](examples/):

```sh
make run EXAMPLE=open-meteo PORT=9115
```

It builds `bin/prometheus-universal-exporter` and runs it on 127.0.0.1:9115.
Open <http://127.0.0.1:9115/> for the landing page, and
<http://127.0.0.1:9115/static-targets/status> for the latest scrapes and
probes. `make run` alone lists the examples; `make build` builds into `bin/`.

## How it works

Targets are not written into the configuration. Prometheus discovers the
real service and passes it to the exporter as `target`. The `collector`
parameter selects the server-side configuration that reads it:

```text
/probe?target=http%3A%2F%2Flegacy.example%3A8080&collector=legacy_text
```

One exporter therefore serves many services and many response formats, and
one collector works for every target that answers the same way.

For a fixed list of targets, use [static targets](docs/STATIC-TARGETS.md)
instead. The exporter scrapes them on their own intervals and serves all
results on one endpoint, `/static-targets`, which Prometheus scrapes like
any other exporter. The results can also be exported over OTLP.

## Quick start

Describe a collector:

```yaml
collectors:
  - name: app_json
    metrics_prefix: myapp   # optional; exports myapp_application_requests_total
    request:
      type: http
      path: /status
    transform:
      type: jq
    metrics:
      - name: application_requests_total
        description: Total application requests
        type: counter
        expression: .requests
```

- [`metrics_prefix`](docs/CONFIGURATION.md#prefixing-a-collectors-metrics)
  is optional and is put, with `_`, in front of every metric's name.
- For a list of servers, rows or components, give a metric `items`
  ([Metrics per item](docs/CONFIGURATION.md#metrics-per-item)); a paged API
  is read into one answer ([Pagination](docs/REQUESTS.md#pagination)).
- Collectors can live in separate files, such as one per team
  ([Collector files](docs/CONFIGURATION.md#collector-files)); their names
  must be unique across all files.

To have your editor check the file as you type, start it with this line:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/eenchev/prometheus-universal-exporter/main/configs/config.schema.json
```

The YAML language server then completes keys and flags mistakes against
[`configs/config.schema.json`](configs/config.schema.json). A typo in an
expression stops the exporter at startup, naming the collector and metric.

Run the exporter and probe a target through it:

```sh
go run . --config.file=configs/config.example.yaml

curl 'http://localhost:8080/probe?target=http://127.0.0.1:9000&collector=app_json'
```

The answer is ordinary Prometheus exposition. `configs/config.example.yaml`
is a complete working configuration; more examples are under `examples/` and
in [Configuration](docs/CONFIGURATION.md).

Or run the published image:

```sh
docker run -p 8080:8080 \
  -v "$PWD/config.yaml:/etc/prometheus-universal-exporter/config.yaml:ro" \
  ghcr.io/eenchev/prometheus-universal-exporter:latest
```

## Install with Helm

The chart is published to GitHub Container Registry as a public OCI artifact, so no registry login is needed. The chart does not install the Prometheus Operator CRDs.

```sh
helm install exporter \
  oci://ghcr.io/eenchev/charts/prometheus-universal-exporter \
  --version 2.2.0 \
  --set-file 'config.data.config\.yaml=configs/config.example.yaml'
```

Without `--version`, Helm installs the newest published chart. Pin the version for anything you deploy more than once. To install from a checkout instead:

```sh
helm install exporter charts/prometheus-universal-exporter \
  --set-file 'config.data.config\.yaml=configs/config.example.yaml'
```

The chart creates the Deployment, the Service, the ConfigMap and, on request, `ServiceMonitor` or `PodMonitor` resources with the relabeling the probe pattern needs. With `allocator.enabled` it also runs the [target allocator](docs/ALLOCATOR.md), which distributes the targets across exporter replicas so collection scales horizontally. See the [chart README](charts/prometheus-universal-exporter/README.md) for its values.

## Endpoints and flags

- `/probe` scrapes a target through a collector; `/static-targets` serves the latest results of the static targets, and `/self-metrics` the exporter's own metrics.
- `/` is a landing page, `/collectors` probes a target from the browser, and `/health` and `/ready` are for Kubernetes. `/-/reload` and `/debug` are served only when their flags enable them.

See [Endpoints](docs/ENDPOINTS.md) for each endpoint's parameters and
answers, and [Command-line flags](docs/FLAGS.md) for every flag.

## Documentation

New here? [Getting started](docs/GETTING-STARTED.md) takes you from nothing to Prometheus scraping your first metric in about 15 minutes. [How do I…](docs/HOW-DO-I.md) finds the page for a task, and [Which request type?](docs/CONFIGURATION.md#which-request-type) has a table of what each request type is for.

- [Configuration](docs/CONFIGURATION.md) — collectors, decoders, transforms, metric rules, caching, serving the last good result while a target is down, environment variables and reloading.
- [Endpoints](docs/ENDPOINTS.md) and [command-line flags](docs/FLAGS.md) — every endpoint's parameters and responses, and every flag.
- [Target requests](docs/REQUESTS.md) — the request URL and `Host` header, redirects, HTTP/2, retries, accepted statuses, timings, pagination, restricting targets, TLS, proxies, per-scrape overrides, and [dates and times](docs/REQUESTS.md#dates-and-times) in a request.
- [Uptime probes](docs/REQUESTS.md#uptime-probes) — availability, status, request phases, redirects and certificate expiry of HTTP and HTTPS sites from one collector, as blackbox_exporter's `http` prober reports them. See [`configs/config.uptime.example.yaml`](configs/config.uptime.example.yaml).
- [Local files](docs/LOCALFILE.md) — the `localfile` request type: metrics and status files on disk, one file or a whole directory.
- [Graphite](docs/GRAPHITE.md) — the `graphite` request type: series from a Graphite render API, or carbon lines from a file.
- [gRPC](docs/GRPC.md) — the `grpc` request type: a unary or server-streaming method called with a JSON message, with message types from server reflection, a descriptor set or `.proto` files.
- [Files over SSH](docs/SSHFILE.md) — the `sshfile` request type: files on another host read over SFTP, one file or a whole directory, with the host key always verified.
- [Commands over SSH](docs/SSHEXEC.md) — the `sshexec` request type: one fixed command, such as `df`, run on another host, with its output passed to any transform.
- [Objects in S3](docs/S3FILE.md) — the `s3file` request type: objects in Amazon S3, MinIO, Ceph, R2 or GCS read as files, one object or every match under a prefix.
- [Databases over SQL](docs/SQL.md) — the `sql` request type: read-only statements on PostgreSQL, MySQL, SQL Server and, in builds that ask for them, Oracle, ClickHouse, Snowflake or Vertica.
- [Redis and Valkey](docs/REDIS.md) — the `redis` request type: a server's INFO and the replies of read-only commands, over plaintext or TLS, directly or through Sentinel.
- [Text protocols over TCP](docs/TCP.md) — the `tcp` request type: a line, a conversation or binary bytes sent over TCP, TLS, a Unix socket or UDP, with the answer passed to any transform; or a check of whether ports accept a connection.
- [LDAP directories](docs/LDAP.md) — the `ldap` request type: searches of a directory such as OpenLDAP or Active Directory, returning entries or their count, over plaintext, StartTLS or LDAPS.
- [DNS](docs/DNS.md) — the `dns` request type: the records of a name with their TTLs and the response code, from the system's resolvers or a named server, over UDP, TCP or TLS.
- [TLS certificates](docs/TLSCERT.md) — the `tlscert` request type: the certificates a server presents, directly or after STARTTLS, with their names, issuers and expiry, and whether the chain verifies.
- [NTP](docs/NTP.md) — the `ntp` request type: the clock offset, delay, jitter and stratum of one NTP server, or several compared.
- [Ping over ICMP](docs/ICMP.md) — the `icmp` request type: echo requests on an unprivileged socket, with the number of replies and their round-trip times.
- [SNMP agents](docs/SNMP.md) — the `snmp` request type: objects and tables read from an agent by numeric OID, over v1, v2c or v3 with authentication and encryption.
- [Kubernetes objects](docs/KUBERNETES.md) — the `kubernetes` request type: the objects of any resource, built in or custom, listed from the API server for kube-state-metrics-style metrics, and the kubelets' pod statistics.
- [SMTP](docs/SMTP.md) — the `smtp` request type, in a build that asks for it: a mail server's greeting, extensions, STARTTLS, login and answers to MAIL FROM and RCPT TO, with nothing sent.
- [IMAP](docs/IMAP.md) — the `imap` request type, in a build that asks for it: message counts, unread counts, sizes and quota of a mail account's mailboxes, and search matches, with no message fetched.
- [POP3](docs/POP3.md) — the `pop3` request type, in a build that asks for it: the number and size of the messages in a mailbox, with no message fetched.
- [Modbus](docs/MODBUS.md) — the `modbus` request type, in a build that asks for it: registers, coils and discrete inputs of a PLC, an inverter or a meter over Modbus TCP.
- [MQTT](docs/MQTT.md) — the `mqtt` request type, in a build that asks for it: the retained messages of subscribed topics, and messages published within a window.
- [WebSocket](docs/WEBSOCKET.md) — the `websocket` request type, in a build that asks for it: an optional message sent to an endpoint, with the answer passed to any transform.
- [Files over FTP](docs/FTP.md) — the `ftp` request type, in a build that asks for it: one file from an FTP server, over plaintext, explicit TLS or FTPS.
- [RADIUS](docs/RADIUS.md) — the `radius` request type, in a build that asks for it: a login with PAP or CHAP against a RADIUS server, or a status check, with the reply and its timing.
- [WHOIS](docs/WHOIS.md) — the `whois` request type, in a build that asks for it: a domain's expiry date, registrar, status and name servers from its registry.
- [SIP](docs/SIP.md) — the `sip` request type, in a build that asks for it: an OPTIONS request over UDP, TCP or TLS, with the response status and timing, and optional Digest authentication.
- [AMQP](docs/AMQP.md), [NATS](docs/NATS.md) and [Kafka](docs/KAFKA.md) — the `amqp`, `nats` and `kafka` request types, in a build that asks for them: the ready messages and consumers of each queue of an AMQP 0-9-1 broker such as RabbitMQ; a NATS server's INFO, PING round trip and JetStream streams and consumers; and a Kafka cluster's brokers, topics, partitions and offsets, and its consumer groups' members and lag, as kafka_exporter reports them, with lag in seconds, disk use and topics' settings, and Amazon MSK's IAM login.
- [Authentication](docs/AUTHENTICATION.md) — credentials for the target, logins, signed requests, credentials read from a secret store (HashiCorp Vault, Google Cloud Secret Manager, AWS Secrets Manager), and credentials for the exporter itself.
- [Prometheus Operator](docs/PROMETHEUS-OPERATOR.md) — ServiceMonitor and PodMonitor.
- [Helm chart](charts/prometheus-universal-exporter/README.md) — every chart value.
- [Python](docs/PYTHON.md) — the transform and pre-script API.
- [Self-metrics](docs/SELF-METRICS.md) — what the exporter reports about itself.
- [Static targets](docs/STATIC-TARGETS.md) — targets the exporter scrapes on their own intervals and serves on one endpoint, written out or found by DNS.
- [Target allocator](docs/ALLOCATOR.md) — `pue-allocator`, which serves exporter replicas their configuration and their share of the static targets, and serves Prometheus the probe targets already assigned to a replica.
- [OTLP](docs/OTLP.md) — OTLP export, its retries and compression.
- [Logging](docs/LOGGING.md) — the log format.
- [Exporter specification](docs/SPECIFICATION-EXPORTER.md) — the implementation specification for the exporter.
- [Chart specification](docs/SPECIFICATION-CHART.md) — the implementation specification for the Helm chart.
- The request types' specifications, each type's section of the exporter's: [`amqp`](docs/SPECIFICATION-AMQP.md), [`dns`](docs/SPECIFICATION-DNS.md), [`ftp`](docs/SPECIFICATION-FTP.md), [`graphite`](docs/SPECIFICATION-GRAPHITE.md), [`grpc`](docs/SPECIFICATION-GRPC.md), [`icmp`](docs/SPECIFICATION-ICMP.md), [`imap`](docs/SPECIFICATION-IMAP.md), [`kafka`](docs/SPECIFICATION-KAFKA.md), [`kubernetes`](docs/SPECIFICATION-KUBERNETES.md), [`ldap`](docs/SPECIFICATION-LDAP.md), [`localfile`](docs/SPECIFICATION-LOCALFILE.md), [`modbus`](docs/SPECIFICATION-MODBUS.md), [`mqtt`](docs/SPECIFICATION-MQTT.md), [`nats`](docs/SPECIFICATION-NATS.md), [`ntp`](docs/SPECIFICATION-NTP.md), [`pop3`](docs/SPECIFICATION-POP3.md), [`radius`](docs/SPECIFICATION-RADIUS.md), [`redis`](docs/SPECIFICATION-REDIS.md), [`s3file`](docs/SPECIFICATION-S3FILE.md), [`sip`](docs/SPECIFICATION-SIP.md), [`smtp`](docs/SPECIFICATION-SMTP.md), [`snmp`](docs/SPECIFICATION-SNMP.md), [`sql`](docs/SPECIFICATION-SQL.md), [`sshexec`](docs/SPECIFICATION-SSHEXEC.md), [`sshfile`](docs/SPECIFICATION-SSHFILE.md), [`tcp`](docs/SPECIFICATION-TCP.md), [`tlscert`](docs/SPECIFICATION-TLSCERT.md), [`websocket`](docs/SPECIFICATION-WEBSOCKET.md), [`whois`](docs/SPECIFICATION-WHOIS.md).

For contributors: [development](docs/DEVELOPMENT.md), [dependencies](docs/DEPENDENCIES.md) and [releasing](docs/RELEASING.md).

## License

See [LICENSE](LICENSE).
