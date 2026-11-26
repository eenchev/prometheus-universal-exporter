# Prometheus Universal Exporter — Helm Chart

Helm chart for deploying the Prometheus Universal Exporter to Kubernetes.

The exporter turns a source that was never meant for Prometheus into a Prometheus target. The default image reads an HTTP API, a gRPC service, a Graphite server, a file in the pod or on another host over SSH, a SQL database, the records of a DNS name, the TLS certificate a server presents, the clock of an NTP server, Kubernetes objects listed from the API server, objects in S3 or an S3-compatible store, the INFO and read-only commands of a Redis or Valkey server, the statistics of a service that answers a line sent over TCP, such as memcached or ZooKeeper, the output of a command run on another host over SSH, the searches of an LDAP directory, the echo replies of a host pinged over ICMP, and the objects and tables of an SNMP agent. An image built with the opt-in request types also reads a mail server, a Modbus device, an MQTT broker, a WebSocket endpoint, a file on an FTP server, the mailboxes of a mail account, a mailbox over POP3, a login against a RADIUS server, the registration of a domain, a SIP server that answers, the queues of an AMQP broker, and a NATS server and its JetStream. It reads answers in JSON, NDJSON, YAML, TOML, INI, XML, CSV, HTML, logfmt, plain text, InfluxDB line protocol, binary layouts, protobuf messages, Graphite's render API and Prometheus exposition. A collector in the configuration defines how to call the source, how to read the answer and which metrics to publish. Targets are not written into the configuration: Prometheus discovers the real service and passes it to the exporter's `/probe` endpoint, so one deployment serves many services and many response formats.

The chart creates the exporter Deployment, Service, ServiceAccount and ConfigMap. By default, the exporter configuration is managed through Helm values.

## Features

* A Prometheus `/probe` endpoint that takes the discovered `target` and a named `collector`
* Collectors that configure the target request: method, path, headers, request body, timeout, retries and TLS
* `grpc`, `graphite`, `dns`, `tlscert`, `ntp`, `s3file`, `redis`, `tcp`, `ldap`, `sshexec`, `icmp` and `snmp` collectors, and, in an image built with them, `smtp`, `modbus`, `mqtt`, `websocket`, `ftp`, `imap`, `pop3`, `radius`, `whois`, `sip`, `amqp` and `nats` ones
* HTTP logins, a JSON POST for a token or OAuth, and pagination across an API's pages
* Decoders for JSON, NDJSON, YAML, TOML, INI, XML, CSV, HTML, logfmt, plain text, InfluxDB line protocol, protobuf messages and Prometheus exposition, with auto-detection
* Transforms with jq, yq, XPath, CSS selectors, regex, Prometheus filtering and Python
* Python pre-scripts and full Python transforms, with a bundled third-party library set and no runtime installs
* Declared metrics with types, descriptions, explicit labels and per-metric error policy
* Per-scrape overrides through `/probe` parameters, which monitors render as `params`
* `ServiceMonitor` and `PodMonitor` resources, including a separate monitor for the exporter's own metrics
* `Probe` resources for uptime checks of URLs and Ingresses, as blackbox_exporter is declared
* Exporter Basic Auth, Secret-backed monitor authentication, and Secret-backed target credentials
* Target credentials read from HashiCorp Vault, Google Cloud Secret Manager or AWS Secrets Manager, with nothing mounted
* SSH keys, passwords and known hosts for `sshfile` and `sshexec` collectors, from a Secret and the values
* Database passwords, Snowflake private keys and TLS files for `sql` collectors, from a Secret
* `list` permissions on the resources `kubernetes` collectors read, for the exporter's own ServiceAccount
* Static targets the exporter scrapes itself and delivers over OTLP
* Response caching per collector
* Configuration watching and in-place reload, and `${NAME}` expansion from the environment
* Self-metrics, including the standard `go_` and `process_` series and optionally per-request series
* A target allocator that splits static targets over the exporter's pods
* A HorizontalPodAutoscaler that can scale on how busy the pods are, or on static targets per pod
* A debug page, `/debug`, that runs a collector on a pasted or fetched answer and shows every stage (`server.debug`)

## Prerequisites

* Kubernetes cluster
* Helm 3
* Prometheus Operator if you want to use `ServiceMonitor`, `PodMonitor` or `Probe`

## Quick start

### 1. Create a values file

Create `values.yaml`:

```yaml
config:
  data:
    config.yaml: |
      collectors:
        - name: example
          request:
            type: http
            path: /status
          transform:
            type: regex
          metrics:
            - name: example_status
              description: Example status value
              type: gauge
              error_mode: log
              labels: []
              expression: 'status:\s+(\d+(?:\.\d+)?)'
```

The chart creates a ConfigMap with this configuration and mounts it into the exporter.

Every collector key works here as it does in a standalone configuration file.
For example, `metrics_prefix: example` on the collector above exports
`example_example_status`: the prefix is joined with `_` to every metric the
collector exports. The exporter validates the prefix at startup, so an
invalid one fails the rollout instead of producing badly named series.

See the exporter documentation for the full collector configuration format, and
[Prefixing a collector's metrics](https://github.com/eenchev/prometheus-universal-exporter/blob/main/docs/CONFIGURATION.md#prefixing-a-collectors-metrics)
for the prefix rules.

### 2. Install the exporter

The chart is published to GitHub Container Registry as an OCI artifact:

```bash
helm install prometheus-universal-exporter \
  oci://ghcr.io/eenchev/charts/prometheus-universal-exporter \
  --version 2.2.0 \
  --namespace monitoring \
  --create-namespace \
  -f values.yaml
```

Without `--version`, Helm installs the newest published chart:

```bash
helm install prometheus-universal-exporter \
  oci://ghcr.io/eenchev/charts/prometheus-universal-exporter \
  --namespace monitoring \
  --create-namespace \
  -f values.yaml
```

Pin the version in anything you deploy more than once. An unpinned install takes whatever is newest when it runs, so the same command run twice can produce two different releases.

The package is public, so no registry login is needed.

To install from a checkout of this repository instead, point Helm at the chart directory:

```bash
helm install prometheus-universal-exporter \
  ./charts/prometheus-universal-exporter \
  --namespace monitoring \
  --create-namespace \
  -f values.yaml
```

### 3. Upgrade

```bash
helm upgrade prometheus-universal-exporter \
  oci://ghcr.io/eenchev/charts/prometheus-universal-exporter \
  --version 2.2.0 \
  --namespace monitoring \
  -f values.yaml
```

A configuration change rolls the exporter Deployment automatically.

### 4. Configure Prometheus

If Prometheus Operator is installed, enable a `ServiceMonitor` or `PodMonitor`:

```yaml
monitors:
  - name: application-services
    enabled: true
    type: service
    collector: example
```

For example, a complete `values.yaml` could look like:

```yaml
config:
  data:
    config.yaml: |
      collectors:
        - name: example
          request:
            type: http
            path: /status
          transform:
            type: regex
          metrics:
            - name: example_status
              description: Example status value
              type: gauge
              error_mode: log
              labels: []
              expression: 'status:\s+(\d+(?:\.\d+)?)'

monitors:
  - name: application-services
    enabled: true
    type: service
    collector: example
```

The chart creates the monitor and routes Prometheus requests through the exporter at the Service's full name, `<release>-prometheus-universal-exporter.<namespace>.svc:8080`, so a Prometheus in another namespace can reach it.

A monitor must name a `collector`. When the chart holds the configuration, that collector must be defined in `config.data`, in `config.yaml` or in a collector file among its keys. Otherwise every probe would be answered `400`, so rendering fails instead. When `config.yaml` lists collector files by absolute path, which may be outside the chart's ConfigMap, a collector not found in `config.data` is left for the exporter to check.

> Probe monitors of either type send Prometheus to the exporter's Service, so they need `service.enabled: true`. Rendering fails when it is off.

Each monitor is named `<release>-prometheus-universal-exporter-<name>`, so `name` must be a DNS-1123 label: lower-case letters, digits and `-`, starting and ending with a letter or digit. It must be unique among the entries and must not be `self` or `static-targets`, the names of the chart's own monitors. Anything else fails rendering.

Without `targetSelector`, a monitor selects the targets labelled `app.kubernetes.io/name: target`. With it, the monitor uses its `matchLabels` or `matchExpressions`.

`interval` and `scrapeTimeout`, here and on the self-metrics and static targets monitors, are Prometheus durations as the Prometheus Operator accepts them: whole numbers of `y`, `w`, `d`, `h`, `m`, `s` and `ms`, such as `30s`, `1m30s` or `1500ms`. A fraction such as `1.5m`, or the units `us` and `ns`, fail rendering. So does a `scrapeTimeout` longer than its `interval`, because Prometheus rejects such a scrape and the monitor would scrape nothing. A probing monitor may leave either out. The chart then renders neither, and the Prometheus Operator's default applies.

A monitor looks for its targets in its own namespace, which is the release's unless `namespaceOverride` is set, and probes each target on the port named `http`. Each entry can change both:

- `port` names the port whose address becomes the probe's target: a port of the selected Services for `type: service`, or a container port of the selected pods for `type: pod`.
- `namespaceSelector` is the Prometheus Operator's field: `matchNames` lists the namespaces to search, and `any: true` searches all of them.

```yaml
monitors:
  - name: payments
    enabled: true
    type: service
    collector: example
    port: metrics
    namespaceSelector:
      matchNames: [payments, checkout]
    targetSelector:
      matchLabels:
        app.kubernetes.io/part-of: payments
```

`port` is a port's name, never its number, because that is what the Prometheus Operator's `port` field takes. With `port: "9115"` the Operator would look for a port named `9115`, find none, and leave the monitor without targets. Rendering therefore fails on a number, written as a number or as a string, with a message that says to name the port. It also fails on any other value that is not a valid port name.

A name consists of lower-case letters, digits and `-`, with at least one letter and no `-` first or last. It can be up to 63 characters for `type: service`, and up to 15 characters with no `--` for `type: pod`. The lengths and the characters are Kubernetes' rules for the names of Service ports and container ports. The required letter is Kubernetes' rule for container ports only: for `type: service` it is the chart's own rule, made so that a number written as a name (`"9115"`) is not rendered as a port name that matches nothing. Kubernetes itself allows a Service port to be named in digits alone (`name: "8080"`), and a Service with such a port cannot be monitored through `port` until its port has a name with a letter.

A target whose port has no name gets one where the port is declared. For `type: service`, that is in the Service:

```yaml
spec:
  ports:
    - name: metrics
      port: 9115
```

For `type: pod`, it is in the pod's container:

```yaml
spec:
  containers:
    - name: app
      ports:
        - name: metrics
          containerPort: 9115
```

The chart renders no port number. The Operator's fields for a number differ between the two kinds and between its releases: a PodMonitor's `portNumber` is unknown to the CRDs of releases before 0.79, and `targetPort` is deprecated on a PodMonitor and, on a ServiceMonitor, means a container port of the pods behind the Service rather than the Service's own port. Every release accepts a name.

While `selfMetrics.enabled` is on, the exporter's own metrics get a monitor too, `<release>-prometheus-universal-exporter-self`: a `ServiceMonitor`, or a `PodMonitor` with `selfMetrics.type: pod`. The chart renders it when it renders any other monitor (a probe monitor, a [`Probe`](#uptime-probes), the [static targets](#static-targets) monitor, a probe job's `ScrapeConfig` or the allocator's `ServiceMonitor`) or when the cluster serves that kind. An install without the Prometheus Operator therefore does not fail on it.

## Configure the target request

The collector defines the request sent to the target.

A monitor can override request settings for an individual scrape:

```yaml
monitors:
  - name: application-services
    enabled: true
    type: service
    collector: example
    params:
      method: [POST]
      path: [/api/status]
      timeout: [5s]
      body: [raw request body]
```

Supported parameters include:

* `method`
* `path`
* `timeout`
* `body`
* `insecure_skip_verify`
* `follow_redirects`
* `enable_http2`
* `new_connection`
* `ip_family`
* retry settings
* `param_<name>`, which fills a `{{param_<name>}}` placeholder in the collector's `request.path`, or in one of its label values
* `from` and `until`, the render window of a [`graphite`](../../docs/GRAPHITE.md) collector
* `message`, the request message of a [`grpc`](../../docs/GRPC.md) collector

`collector` and `target` are not among them. The chart renders the collector parameter from the monitor's `collector`, which it checks against `config.data`, and the target parameter from each discovered target's address. A `params` entry for either would render a second key of the same name, so it fails rendering with a message that points to `collector` and `targetSelector`.

For example, a collector with `path: /api/{{param_tenant}}/status` scraped by a
monitor with `params: {param_tenant: [acme]}` requests `/api/acme/status`. A
placeholder can have a default after a colon, `{{param_tenant:acme}}`. A
placeholder with no default that the monitor does not supply fails the scrape
with `400`. See [Target requests](../../docs/REQUESTS.md#path-parameters). A
collector's label can take the same parameter, `transform: {labels: {tenant:
"{{param_tenant}}"}}`, so each monitor's series carry the value it sends
([In label values](../../docs/REQUESTS.md#in-label-values)).

A [`graphite`](../../docs/GRAPHITE.md) collector is monitored the same way. The
monitor selects the Graphite Service, whose address becomes the `target`. Its
`params` fill the placeholders of the collector's `request.targets`, such as
`param_env: [staging]`, or set the window, such as `from: [-1h]`. `method`
and `body` do not apply to it and are answered with `400`.

A [`grpc`](../../docs/GRPC.md) collector is monitored the same way too. The
monitor selects the Service of the gRPC server and names its gRPC port with
`port`, such as `port: grpc`, because the default is the port named `http`.
That port's `host:port` address becomes the `target`, which is called in
plaintext unless the collector sets `request.tls`. The monitor's `params` fill
the placeholders of the collector's `request.message` and `metadata`, such as
`param_queue: [orders]`, or replace the message, such as
`message: ['{"queue": "orders"}']`. `method`, `path` and `body` do not apply
to it and are answered with `400`. A `descriptors: protoset` or `proto`
collector reads its files from the exporter's filesystem, so mount them from a
ConfigMap with `extraVolumes` and `extraVolumeMounts`.

## Authentication

### Target authentication

If the target requires authentication, use a Kubernetes Secret instead of putting credentials in the configuration.

For basic authentication:

```yaml
targetAuth:
  enabled: true
  type: basic
  secretName: target-basic-auth
  usernameKey: username
  passwordKey: password
```

Reference the mounted credentials from the collector:

```yaml
request:
  basic_auth_file:
    username: /var/run/prometheus-universal-exporter/target-auth/username
    password: /var/run/prometheus-universal-exporter/target-auth/password
```

Bearer authentication is also supported.

`targetAuth.mountPath` must be a path of its own. Rendering fails when it is:

- the configuration directory, `webAuth.mountPath`, `sshAuth.mountPath`, `sqlAuth.mountPath` or the path of an `extraVolumeMounts` entry;
- inside or above the configuration directory or another credential's directory; or
- the parent of an `extraVolumeMounts` entry.

A mount point cannot be created inside a read-only Secret, ConfigMap or projected volume, which is why nested paths are refused.

### Monitor authentication

A monitor's `auth` is the credential Prometheus presents to the exporter when it scrapes through that monitor. It is read from a Secret in the monitor's namespace and is off by default. With `enabled: true`, it must set `type` (`bearer` or `basic`) and `secretName`. When either is missing, rendering fails and names the monitor, rather than rendering a monitor that scrapes with no credential or the wrong one. The Secret's keys default to `token` for bearer, and to `username` and `password` for basic:

```yaml
monitors:
  - name: application-services
    enabled: true
    type: service
    collector: example
    auth:
      enabled: true
      type: bearer
      secretName: scrape-token   # key: token, or set secretKey
```

To pass that credential on to the target, set `request.forward_authorization: true` on the collector. See [Authentication](../../docs/AUTHENTICATION.md).

### Exporter authentication

To protect the exporter's `/probe` and self-metrics endpoints, add `web.basic_auth` to the configuration. `config.data."config.yaml"` is the whole configuration file, so it keeps its collectors beside `web`. The chart's default collector stands in for yours here:

```yaml
config:
  data:
    config.yaml: |
      web:
        basic_auth:
          enabled: true
          username: exporter
          password: change-me
      collectors:
        - name: example
          request:
            type: http
            path: /status
          transform:
            type: regex
          metrics:
            - name: example_status
              expression: 'status:\s+(\d+(?:\.\d+)?)'
```

A password written this way ends up in the chart's ConfigMap, and the monitors need the credential too. The chart also cannot see it, so an Ingress or a `NodePort` or `LoadBalancer` Service additionally needs `exposure.allowWithoutWebAuth` (see [Exposing the exporter](#exposing-the-exporter)). Instead, mount the credential from a Secret with `webAuth` and point the configuration at the files. Every monitor the chart renders then presents it:

```yaml
webAuth:
  enabled: true
  secretName: exporter-auth        # keys: username, password
config:
  data:
    config.yaml: |
      web:
        basic_auth:
          enabled: true
          username_file: /var/run/prometheus-universal-exporter/web-auth/username
          password_file: /var/run/prometheus-universal-exporter/web-auth/password
      collectors:
        - name: example
          request:
            type: http
            path: /status
          transform:
            type: regex
          metrics:
            - name: example_status
              expression: 'status:\s+(\d+(?:\.\d+)?)'
monitors:
  - name: targets
    enabled: true
    type: service
    collector: example
```

`web.basic_auth` protects `/probe`, the self-metrics endpoint and the static targets endpoint alike. With `webAuth.enabled`, every monitor the chart renders therefore sends the `webAuth` Secret's credential as `basicAuth`: the self-metrics monitor, the static targets monitor, and each probing monitor without an `auth` of its own. A probing monitor whose `auth.enabled` is true sends its own credential instead.

`webAuth.usernameKey` and `webAuth.passwordKey` name the Secret's keys, `username` and `password` by default. The files are always named `username` and `password` under `webAuth.mountPath`. The exporter re-reads a file when it changes, so a rotated Secret takes effect without a restart.

`webAuth.mountPath` must be a path of its own, under the same rules as `targetAuth.mountPath`: the configuration directory, `targetAuth.mountPath`, `sshAuth.mountPath`, `sqlAuth.mountPath` or an `extraVolumeMounts` entry at the same path is rejected while rendering, and so is a path inside or above the configuration directory or another credential's directory, or an `extraVolumeMounts` entry inside it.

`webAuth.enabled` is also what lets the chart render an Ingress or a `NodePort` or `LoadBalancer` Service, once the configuration reads its files. See [Exposing the exporter](#exposing-the-exporter).

The `/health` and `/ready` endpoints stay available without credentials for Kubernetes health checks. The readiness probe uses `/ready`. It reports the pod as not ready while it shuts down and, with `otlp.unready_after_failures` set, while its OTLP exports keep failing. A rejected configuration reload leaves the pod ready, serving the configuration in force. See [Readiness](../../docs/CONFIGURATION.md#readiness).

## Service account

By default the chart creates a ServiceAccount named `<fullname>`, or `serviceAccount.name` when it is set, and the exporter's pods run as it. To run them as an existing account, for example one managed by your cluster's policies or by another team, set `serviceAccount.create: false` and name it:

```yaml
serviceAccount:
  create: false
  name: metrics-exporter
```

The chart then renders no ServiceAccount, and the pods run as `metrics-exporter`. The name is required. An empty `serviceAccount.name` with `create: false` fails rendering: the values schema refuses it and, when the schema is skipped, so do the templates. Otherwise the pods would run as the namespace's `default` account, which every workload without its own account shares and which `kubernetesAccess` would then grant `list` to. Write `name: default` to choose that account on purpose.

Whichever account the pods run as, they mount no service account token unless `serviceAccount.automount` is true or `kubernetesAccess` is enabled. With `kubernetesAccess`, the pods mount the token, and the chart binds its Roles, or its ClusterRole, to the account the pods run as, including a named existing account. See [Kubernetes collectors](#kubernetes-collectors). The chart sets `automountServiceAccountToken` on the pods, and on the ServiceAccount only when it creates it.

`serviceAccount.annotations` are added to the ServiceAccount the chart creates. Examples are the IAM role an EKS pod assumes (IRSA), shown under [S3 object collectors](#s3-object-collectors), and the Google service account of GKE's Workload Identity, `iam.gke.io/gcp-service-account`. The chart never changes an account it did not create, so annotations together with `create: false` fail rendering with a message that says so. Put them on the existing account itself.

With a `kubernetes` discovery source, the allocator runs as its own account, `<fullname>-allocator`. Without one, it runs as the exporter's account. `allocator.serviceAccount.name` runs it as an existing account instead, whatever its sources. See [Target allocator](#target-allocator).

## SSH file collectors

An `sshfile` collector reads a file over SSH (SFTP). It authenticates with a private key or a password read from files it names (`private_key_file`, `passphrase_file`, `password_file`) and checks the server's host key against `known_hosts_file`. `sshAuth` mounts all of these from Kubernetes objects into one read-only directory, `sshAuth.mountPath` (`/var/run/prometheus-universal-exporter/ssh` by default), under fixed names:

| File | From |
| --- | --- |
| `private_key` | The Secret's `sshAuth.privateKeyKey`. |
| `passphrase` | The Secret's `sshAuth.passphraseKey`, the private key's passphrase; only beside `privateKeyKey`. |
| `password` | The Secret's `sshAuth.passwordKey`. |
| `known_hosts` | The lines of `sshAuth.knownHosts`, or the Secret's `sshAuth.knownHostsSecretKey`. |

Create the Secret from the key. To keep the host keys in the Secret too, add a `known_hosts` file written by `ssh-keyscan`:

```bash
ssh-keyscan -t ed25519 sftp.example.com > known_hosts
kubectl create secret generic sftp-credentials \
  --namespace monitoring \
  --from-file=id_ed25519=./id_ed25519 \
  --from-file=known_hosts=./known_hosts
```

```yaml
sshAuth:
  enabled: true
  secretName: sftp-credentials
  privateKeyKey: id_ed25519
  knownHostsSecretKey: known_hosts
```

Alternatively, leave `known_hosts` out of the Secret and paste the lines that `ssh-keyscan -t ed25519 sftp.example.com` prints into the values. Host keys are public, so the chart renders them into its own ConfigMap, `<release>-prometheus-universal-exporter-ssh-known-hosts`:

```yaml
sshAuth:
  enabled: true
  secretName: sftp-credentials
  privateKeyKey: id_ed25519
  knownHosts: |
    sftp.example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMbWeyhTNTo7zGLaBVnPBDqg4tqfCW7DfdUAsdthK+Ew
```

Then point the collector at the files:

```yaml
config:
  data:
    config.yaml: |
      collectors:
        - name: sftp_textfile
          request:
            type: sshfile
            username: metrics
            private_key_file: /var/run/prometheus-universal-exporter/ssh/private_key
            known_hosts_file: /var/run/prometheus-universal-exporter/ssh/known_hosts
            root: /var/lib/node_exporter/textfile_collector
            path: batch.prom
          transform:
            type: prometheus
```

Probe it with the server as the target: `/probe?collector=sftp_textfile&target=ssh://sftp.example.com:22`. The other keys of an `sshfile` request (password authentication, directories, keepalives and how long a connection is kept) are described in [SSH files](https://github.com/eenchev/prometheus-universal-exporter/blob/main/docs/SSHFILE.md).

With `sshAuth.enabled`, rendering fails, with a message that names what to set, when:

- `secretName` is empty;
- neither `privateKeyKey` nor `passwordKey` is set;
- `passphraseKey` is set without `privateKeyKey`;
- known hosts are given by neither `knownHosts` nor `knownHostsSecretKey`, or by both, because the single `known_hosts` file can come from only one of them; or
- `knownHosts` holds no entry.

The chart recognises an entry by its shape alone: a line that is not a `#` comment and has at least three fields (a host, a key type and a key). Whether the key is valid is for the exporter to check. The values schema refuses the same cases, and the templates refuse them again when the schema is skipped.

The files have mode `0400`. The kubelet makes them group-owned by the pod's `fsGroup` and readable by that group. The exporter runs as user and group 65532 with `fsGroup` 65532, so it can read them and no other user can. A `podSecurityContext` without that `fsGroup` leaves them unreadable to the exporter.

The inline `knownHosts` lines are part of the pods' `checksum/config`, so changing them rolls the Deployment, as a configuration change does. A changed Secret reaches the mounted files without a restart, because the kubelet updates them in place. `sshAuth.mountPath` must be a path of its own, as `targetAuth.mountPath` must. With `networkPolicy.egress` rules, add one for the SSH servers' port, 22 by default.

## SSH exec collectors

An `sshexec` collector runs one fixed command on a host over SSH, such as `df -P` or a script on that host, and reads its output as a body. It uses the same keys and connection as an `sshfile` collector, so [`sshAuth`](#ssh-file-collectors) mounts its private key or password and its `known_hosts` file the same way.

```yaml
sshAuth:
  enabled: true
  secretName: exporter-ssh
  privateKeyKey: id_ed25519
  knownHostsSecretKey: known_hosts
config:
  data:
    config.yaml: |
      collectors:
        - name: disk
          request:
            type: sshexec
            command: [df, -P, -k, /]
            username: metrics
            private_key_file: /var/run/prometheus-universal-exporter/ssh/private_key
            known_hosts_file: /var/run/prometheus-universal-exporter/ssh/known_hosts
          transform:
            type: regex
          metrics:
            - name: node_filesystem_available_kilobytes
              expression: '(?m)^\S+\s+\d+\s+\d+\s+(\d+)\s+\d+%\s+/$'
```

Probe it with the host as the target: `/probe?collector=disk&target=db1.example.com`. Give the key its own account on the host, restricted to the commands the collectors run by a forced command in its `authorized_keys`, as [Commands over SSH](https://github.com/eenchev/prometheus-universal-exporter/blob/main/docs/SSHEXEC.md#security) shows. That page also covers the command, its quoting and the exit statuses that count as success. With `networkPolicy.egress` rules, add one for the SSH servers' port, 22 by default.

## SQL collectors

An `sql` collector runs one statement on a database and reads the rows it returns. It reads the password, a Snowflake private key, the TLS CA bundle and the client certificate and key from files it names (`password_file`, `private_key_file`, `tls.ca_file`, `tls.cert_file`, `tls.key_file`). `sqlAuth` mounts them from one Secret into one read-only directory, `sqlAuth.mountPath` (`/var/run/prometheus-universal-exporter/sql` by default), under fixed names:

| File | From |
| --- | --- |
| `password` | The Secret's `sqlAuth.passwordKey`. |
| `private_key` | The Secret's `sqlAuth.privateKeyKey`, an unencrypted key for Snowflake key-pair authentication. |
| `ca.crt` | The Secret's `sqlAuth.caKey`, the CA bundle the database's certificate is checked against. |
| `client.crt` | The Secret's `sqlAuth.certKey`, a client certificate; only beside `keyKey`. |
| `client.key` | The Secret's `sqlAuth.keyKey`, the client certificate's key; only beside `certKey`. |

Create the Secret from the password and the CA bundle:

```bash
kubectl create secret generic postgres-exporter \
  --namespace monitoring \
  --from-literal=password='s3cret' \
  --from-file=ca.crt=./ca.crt
```

```yaml
sqlAuth:
  enabled: true
  secretName: postgres-exporter
  passwordKey: password
  caKey: ca.crt
```

Then point the collector at the files:

```yaml
config:
  data:
    config.yaml: |
      collectors:
        - name: pg_connections
          request:
            type: sql
            driver: postgres
            database: app
            username: exporter
            password_file: /var/run/prometheus-universal-exporter/sql/password
            tls: {ca_file: /var/run/prometheus-universal-exporter/sql/ca.crt}
            statement: "SELECT state, count(*) AS connections FROM pg_stat_activity GROUP BY 1"
          transform: {type: csv}
          metrics:
            - {name: pg_connections, type: gauge, expression: connections, labels: [{name: state, expression: state}]}
```

Probe it with the database server as the target: `/probe?collector=pg_connections&target=postgres.databases.svc:5432`.

For Snowflake key-pair authentication, in an image built with the Snowflake driver, put the private key in the Secret instead of a password. The exporter cannot read an encrypted key, so there is no passphrase. Write the key unencrypted first, as [SQL](https://github.com/eenchev/prometheus-universal-exporter/blob/main/docs/SQL.md#authentication) shows, with `openssl pkcs8 -topk8 -nocrypt -in rsa_key.p8 -out rsa_key_plain.p8`:

```yaml
sqlAuth:
  enabled: true
  secretName: snowflake-exporter
  privateKeyKey: rsa_key_plain.p8
```

Then name `private_key_file: /var/run/prometheus-universal-exporter/sql/private_key` in the collector. The statement's placeholders take their values from `args`. The drivers and the other keys of an `sql` request are described in [SQL](https://github.com/eenchev/prometheus-universal-exporter/blob/main/docs/SQL.md).

With `sqlAuth.enabled`, rendering fails, with a message that names what to set, when:

- `secretName` is empty;
- none of `passwordKey`, `privateKeyKey`, `caKey`, `certKey` and `keyKey` is set; or
- `certKey` is set without `keyKey`, or `keyKey` without `certKey`, because a client certificate is presented with its key.

The values schema refuses the same cases, and the templates refuse them again when the schema is skipped. A `sqlAuth.passphraseKey` is refused too, whether `sqlAuth` is enabled or not: the exporter reads only an unencrypted key, so remove the passphrase as shown above.

The files have mode `0400`, and the exporter reads them through the pod's `fsGroup`, as it does the files of the [SSH file collectors](#ssh-file-collectors). A changed Secret reaches the mounted files without a restart. `sqlAuth.mountPath` must be a path of its own, as `targetAuth.mountPath` must. With `networkPolicy.egress` rules, add one for the databases' ports: 5432 for PostgreSQL, 3306 for MySQL, 1433 for SQL Server, 443 for Snowflake:

```yaml
networkPolicy:
  enabled: true
  egress:
    - to:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: databases
      ports:
        - port: 5432
```

`sqlAuth` mounts one set of credentials. Collectors for several databases can share it when one Secret holds everything they need, for example one CA bundle and one password. For databases with their own credentials, mount each additional Secret through `extraVolumes` and `extraVolumeMounts` at its own path, and name its files in the collectors. A Secret volume without `items` holds every key of the Secret as a file of that name:

```yaml
extraVolumes:
  - name: reporting-db
    secret:
      secretName: reporting-db-exporter
      defaultMode: 0400
extraVolumeMounts:
  - name: reporting-db
    mountPath: /var/run/prometheus-universal-exporter/sql-reporting
    readOnly: true
```

## S3 object collectors

An `s3file` collector reads objects from Amazon S3 or an S3-compatible store.

The collector's credentials need no mount from the chart. On EKS, give the pods the IAM role of their service account (IRSA). The AWS SDK's default chain takes it from the token and role that EKS provides to the pods, so there is nothing to rotate:

```yaml
image:
  repository: registry.example.com/prometheus-universal-exporter-s3
  tag: "1.4.0"
serviceAccount:
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/metrics-reader
config:
  data:
    config.yaml: |
      collectors:
        - name: textfiles
          request:
            type: s3file
            root: /textfiles/prod
            files: ["*.prom"]
            region: eu-west-1
          transform:
            type: prometheus
```

Probe it with the bucket as the target: `/probe?collector=textfiles&target=acme-metrics`.

- The annotation goes on the ServiceAccount the chart creates. With an existing account (`serviceAccount.create: false`), annotate that account instead (see [Service account](#service-account)).
- Outside EKS, pass the two parts of an access key from a Secret as `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` through `env` or `envFrom`. Alternatively, mount the Secret through `extraVolumes` and `extraVolumeMounts` and name its files with the collector's `access_key_id_file` and `secret_access_key_file`, which are read at every request.
- For a store other than AWS (MinIO, Ceph, R2, Google Cloud Storage), set the collector's `endpoint`, and `path_style` where the store needs it.
- With `networkPolicy.egress` rules, add one for the store's port, 443 for AWS.

The request's keys are described in [Objects in S3](https://github.com/eenchev/prometheus-universal-exporter/blob/main/docs/S3FILE.md).

## Secret stores

A credential the exporter reads from a [secret store](../../docs/AUTHENTICATION.md#secret-stores) (HashiCorp Vault, Google Cloud Secret Manager or AWS Secrets Manager) needs no mounted Secret. The store and the references to it are part of the configuration, and the chart gives the pods what the store's login needs.

For Vault's Kubernetes auth, the pods must mount their service account token, which they do not by default:

```yaml
serviceAccount:
  automount: true
config:
  data:
    config.yaml: |
      secret_stores:
        - name: vault
          type: vault
          address: https://vault.vault.svc:8200
          auth: {method: kubernetes, role: exporter}
      collectors:
        - name: api
          request:
            type: http
            bearer_token_file: secret://vault/secret/data/api#token
          transform:
            type: jq
          metrics:
            - name: api_up
              expression: .up
```

The Vault role is bound to the ServiceAccount the pods run as: `<fullname>` in the release's namespace, unless `serviceAccount.name` names another. To leave the API token unmounted, project a token with Vault's audience through `extraVolumes` and `extraVolumeMounts` and name it with the store's `auth.jwt_file`. A Vault token or an AppRole's secret id comes from a Secret as `VAULT_TOKEN` or `VAULT_SECRET_ID`, through `env` or `envFrom`.

For AWS Secrets Manager, annotate the ServiceAccount with the IAM role, as for [S3 object collectors](#s3-object-collectors). For Google Cloud Secret Manager, annotate it with the Workload Identity binding, `iam.gke.io/gcp-service-account`. Neither needs the token mounted.

With `networkPolicy.egress` rules, allow the store too: Vault's address, or the cloud's API and its metadata endpoint. With `allocator.enabled`, each exporter pod reads from the stores itself, and the allocator contacts none of them.

## Redis collectors

A `redis` collector reads a Redis or Valkey server's INFO and the replies of read-only commands, such as a queue's `LLEN`.

When the server requires a password, the collector names a file with `password_file`, which is read at every scrape. Mount it from a Secret through `extraVolumes` and `extraVolumeMounts`:

```yaml
extraVolumes:
  - name: redis-exporter
    secret:
      secretName: redis-exporter
      defaultMode: 0400
extraVolumeMounts:
  - name: redis-exporter
    mountPath: /var/run/prometheus-universal-exporter/redis
    readOnly: true
```

Then name `password_file: /var/run/prometheus-universal-exporter/redis/password` in the collector, with `username` for an ACL user. Probe it with the server as the target, `/probe?collector=redis&target=redis-master.cache:6379`, or use `rediss://` for TLS, with the CA and a client certificate mounted as [TLS files](#tls-files) shows. With `networkPolicy.egress` rules, add one for the servers' port, 6379 by default. The request's keys are described in [Redis and Valkey](https://github.com/eenchev/prometheus-universal-exporter/blob/main/docs/REDIS.md).

## TCP collectors

A `tcp` collector sends a line, such as memcached's `stats`, over a TCP connection and reads the answer. It is for services that report statistics this way: memcached, ZooKeeper's four-letter words, HAProxy's runtime API on a TCP socket.

Probe it with the service as the target, always with its port: `/probe?collector=memcached&target=memcached.cache:11211`. With `networkPolicy.egress` rules, add one for each service's port. For TLS, the collector names its CA and client certificate files in `tls`, mounted as [TLS files](#tls-files) shows. The request's keys are described in [Text protocols over TCP](https://github.com/eenchev/prometheus-universal-exporter/blob/main/docs/TCP.md).

### TLS files

A `tcp`, `redis` or `ldap` collector that uses TLS verifies the server against the system's roots and the CA in `tls.ca_file`. When the server asks for a client certificate, it presents the one in `tls.cert_file` and `tls.key_file`, which are set together or not at all. The files are read from the exporter's pod, so put them in a Secret as `ca.crt` and, for a client certificate, `tls.crt` and `tls.key` (the keys cert-manager writes), and mount it through `extraVolumes` and `extraVolumeMounts`. This example reads HAProxy's runtime API over TLS with a client certificate:

```yaml
extraVolumes:
  - name: haproxy-tls
    secret:
      secretName: haproxy-tls   # keys: ca.crt, tls.crt, tls.key
      defaultMode: 0400
extraVolumeMounts:
  - name: haproxy-tls
    mountPath: /var/run/prometheus-universal-exporter/haproxy-tls
    readOnly: true
config:
  data:
    config.yaml: |
      collectors:
        - name: haproxy
          request:
            type: tcp
            send: "show info\n"
            tls:
              ca_file: /var/run/prometheus-universal-exporter/haproxy-tls/ca.crt
              cert_file: /var/run/prometheus-universal-exporter/haproxy-tls/tls.crt
              key_file: /var/run/prometheus-universal-exporter/haproxy-tls/tls.key
          transform:
            type: regex
          metrics:
            - name: haproxy_current_connections
              expression: '(?m)^CurrConns: (\d+)$'
```

Probe it with the runtime API's address: `/probe?collector=haproxy&target=haproxy.edge:9999`.

- A `redis` or `ldap` collector takes the same `tls` block, beside a `password_file` from its own Secret or the same one.
- A CA alone (a Secret with only `ca.crt`) verifies the server without a client certificate.
- The files are read at the first scrape and again when they change, so a renewed Secret takes effect once the kubelet updates the mount, without a restart.
- As for any extra mount, the `mountPath` must be a path of its own, not at or inside the configuration directory or a credential's directory.

One collector can serve servers that each have their own Secret. A [static target](#static-targets) of a `redis`, `ldap`, `tcp`, `sql`, `grpc`, `http` or `graphite` collector can name its own `tls` block, and a `redis` or `ldap` target its own `password_file` with its `username` or `bind_dn`, in place of the collector's. Mount each Secret at its own path and name its files in the target ([Credentials and TLS of a target](https://github.com/eenchev/prometheus-universal-exporter/blob/main/docs/STATIC-TARGETS.md#credentials-and-tls-of-a-target)).

## LDAP collectors

An `ldap` collector searches an LDAP directory (OpenLDAP, Active Directory or any other LDAPv3 server) and reads the entries each search finds, or only their count.

The collector binds as the account in `bind_dn` and reads that account's password from the file named by `password_file` at every scrape. Mount the file from a Secret through `extraVolumes` and `extraVolumeMounts`:

```yaml
extraVolumes:
  - name: ldap-exporter
    secret:
      secretName: ldap-exporter
      defaultMode: 0400
extraVolumeMounts:
  - name: ldap-exporter
    mountPath: /var/run/prometheus-universal-exporter/ldap
    readOnly: true
```

Then name `password_file: /var/run/prometheus-universal-exporter/ldap/password` beside `bind_dn` in the collector. Probe it with the server as the target, `/probe?collector=directory&target=ldap.example.org`, or use `ldaps://` for TLS from the start. The CA and a client certificate are mounted as [TLS files](#tls-files) shows. With `networkPolicy.egress` rules, add one for the servers' port: 389, or 636 for LDAPS. The request's keys are described in [LDAP directories](https://github.com/eenchev/prometheus-universal-exporter/blob/main/docs/LDAP.md).

## ICMP and SNMP collectors

An `icmp` collector pings a host on an unprivileged ICMP socket. Linux opens such a socket only for a process whose group is within the sysctl `net.ipv4.ping_group_range`. The exporter runs as group `65532`. containerd and Docker set the sysctl to allow every group. Where the runtime does not, set it on the pod: the sysctl belongs to the pod's network namespace, and Kubernetes treats it as safe ([Permissions](../../docs/ICMP.md#permissions)):

```yaml
podSecurityContext:
  sysctls:
    - name: net.ipv4.ping_group_range
      value: "0 2147483647"
```

The `podSecurityContext` in the values is merged with the chart's, so the pod keeps running as that user and group. An `snmp` collector needs nothing from the chart beyond its community or v3 password files, mounted from a Secret through `extraVolumes` and `extraVolumeMounts` in the same way as an [LDAP collector's](#ldap-collectors) password. See [SNMP agents](../../docs/SNMP.md).

## Kubernetes collectors

A `kubernetes` collector lists objects from the API server of the cluster the exporter runs in, using the exporter's own ServiceAccount. By default the pods mount no token and the account can list nothing. `kubernetesAccess` grants `list` on what the collectors read, and nothing else, except for the `get` on the kubelets' statistics that `kubernetesAccess.kubelet` adds (see below):

```yaml
kubernetesAccess:
  enabled: true
  namespaces: [shop]
  rules:
    - apiGroups: [apps]
      resources: [deployments]
    - apiGroups: [""]
      resources: [pods]
config:
  data:
    config.yaml: |
      collectors:
        - name: deployments
          request:
            type: kubernetes
            group: apps
            resource: deployments
            namespaces: [shop]
          transform:
            type: jq
          metrics:
            - name: kube_deployment_status_replicas_available
              items: .items[]
              expression: .status.availableReplicas // 0
              labels:
                - name: deployment
                  expression: .metadata.name
```

Probe it with no target: `/probe?collector=deployments`.

With `enabled`, the pods and the ServiceAccount mount the token, whatever `serviceAccount.automount` says. The chart renders a Role and a RoleBinding named `<fullname>` in each namespace of `namespaces`, or in the release's namespace when the list is empty. They allow `list` on each rule's `apiGroups` (`""` for the core group) and `resources`.

A collector with `all_namespaces`, or one for a cluster-scoped resource such as `nodes`, needs `clusterWide: true`, which renders a ClusterRole and a ClusterRoleBinding named `<namespace>-<fullname>` instead. Setting `clusterWide` and `namespaces` together fails rendering.

Rendering also fails with no rules, with a rule containing `*`, and with a rule that grants `secrets` of the core group. `list` on Secrets lets the account read their values, which a collector that reads only their metadata (`metadata_only`) never needs. Grant such a collector its `list` through a Role of your own, knowing what it allows.

A collector for the nodes' kubelets (`kubelet: stats/summary`, see [Kubelet statistics](https://github.com/eenchev/prometheus-universal-exporter/blob/main/docs/KUBERNETES.md#kubelet-statistics)) needs `get` on `nodes/stats`. `kubelet: stats` adds it to the ClusterRole, beside a rule for `nodes`. With `kubelet_via_api_server` the collector needs `get` on `nodes/proxy` instead, which `kubelet: proxy` adds; note that this lets the account do everything a kubelet does. Either setting needs `clusterWide` and, with `networkPolicy.egress` rules, a rule for the kubelets' port, 10250, on the nodes' addresses:

```yaml
kubernetesAccess:
  enabled: true
  clusterWide: true
  kubelet: stats
  rules:
    - apiGroups: [""]
      resources: [nodes]
```
The account may need more than `list` for its collectors. `extraRules` adds RBAC rules of your own, each with its `apiGroups`, `resources`, `verbs` and optional `resourceNames`. They are rendered as written, after the chart's rules in the same Role or ClusterRole, so they apply in the same namespaces, or cluster-wide with `clusterWide`. They are subject to the same checks as `rules`: a `*` in any of the three lists, or the core group's `secrets`, fails rendering, unless `allowBroadExtraRules: true` grants them as written. Set that only when you accept what it allows:

```yaml
kubernetesAccess:
  enabled: true
  rules:
    - apiGroups: [""]
      resources: [pods]
  extraRules:
    - apiGroups: [""]
      resources: [configmaps]
      verbs: [get]
      resourceNames: [cluster-info]
```

With `serviceAccount.create: false`, the bindings name the account the pods run as, `serviceAccount.name` ([Service account](#service-account)). While `kubernetesAccess` is enabled, no `extraVolumeMounts` entry may be at or below the token's directory, `/var/run/secrets/kubernetes.io/serviceaccount`. With `networkPolicy.egress` rules, add one for the API server: the address and port of the `kubernetes` Service's endpoints, 443 or 6443 on most clusters. The request's keys are described in [Kubernetes objects](https://github.com/eenchev/prometheus-universal-exporter/blob/main/docs/KUBERNETES.md).

## Common configuration

### Exporter flags

The exporter's flags are set through chart values. Every flag a serving exporter takes has one:

| Value | Flag | Default |
| --- | --- | --- |
| `server.listenAddress` | `--web.listen-address` | `:8080` |
| `selfMetrics.path` | `--web.self-metrics-path` | `/self-metrics` |
| `server.pythonPath` | `--python.path` | `/usr/local/bin/python3` |
| `server.logLevel` | `--log.level` | `info` |
| `server.probeTimeoutOffset` | `--probe.timeout-offset` | unset: the exporter's `500ms` |
| `server.probeDefaultTimeout` | `--probe.default-timeout` | unset: the exporter's `30s` |
| `server.probeMaxConcurrent` | `--probe.max-concurrent` | unset: no limit across collectors |
| `server.pythonMaxWorkers` | `--python.max-workers` | unset: no limit across scripts |
| `server.pythonMaxIdleWorkers` | `--python.max-idle-workers`: `0` keeps none idle | unset: the exporter's `1` per script |
| `server.pythonIdleTimeout` | `--python.idle-timeout`: whole hours, minutes and seconds, at least `1s` | unset: the exporter's `2m` |
| `server.shutdownTimeout` | `--web.shutdown-timeout` | unset: the exporter's `15s` |
| `server.shutdownDelay` | `--web.shutdown-delay` | `5s` |
| `server.watchConfig` / `server.watchConfigInterval` | `--config.watch` / `--config.watch-interval`, a positive Go duration such as `60s` or `1m30s` | off / `60s` |
| `server.expandEnv` | `--config.expand-env` | off |
| `server.enableLifecycle` | `--web.enable-lifecycle` | off |
| `server.debug` | `--web.debug`: `off`, `edit` or `full` | off |
| `server.debugDangerous` | `--web.debug.dangerous` when true, which needs `server.debug: full`: the playground may run Python, read the pod's files and make its own requests | `false` |
| `server.gzipLevel` | `--web.gzip-level`: `0` to `9` | unset: the exporter's `1` |
| `server.statusRefresh` | `--web.status-refresh`: `off`, or `5s` to `24h` | unset: the exporter's `off` |
| `server.allocatorCapacity` | `--allocator.capacity`, with `allocator.enabled` alone: `0.1` to `100` with at most three decimals | unset: the exporter's `1` |
| `staticTargets.enabled` | `--static-targets-file` | off |
| `staticTargets.path` | `--web.static-targets-path` | `/static-targets` |
| `staticTargets.expandEnv` | `--static-targets.expand-env`, with `staticTargets.enabled` | off |
| `goMemLimit.enabled` / `goMemLimit.ratio` | `--runtime.memory-limit-ratio` | on / `0.8` |
| `config` | `--config.file`, without the allocator | the chart's ConfigMap |
| `allocator.enabled` | `--allocator.url`, `--allocator.cache-dir`, `--allocator.member`, `--allocator.advertise`; see [Target allocator](#target-allocator) | off |
| `allocator.tokenSecret` | `--allocator.token-file`, with `allocator.enabled` | a generated Secret |
| `allocator.staticTargets.expandEnv` | `--static-targets.expand-env`, with `allocator.enabled` | off |

`server.listenAddress` also sets the container port, so the listener and the probes cannot drift apart. Its host may be empty, as in `:8080`, a wildcard such as `0.0.0.0:8080` or `[::]:8080`, or a pod address. A loopback host (`127.x.x.x`, `localhost` or `[::1]`) fails rendering, because neither the kubelet's probes nor the Service can reach it.

`server.pythonPath` is the interpreter the `python` transform uses. Its default is where the exporter image's `python:3.12-slim` base installs Python, so override it only for a custom image. `server.logLevel` is one of `debug`, `info`, `warn` and `error`; anything else fails rendering.

`server.probeTimeoutOffset` is how much of Prometheus's scrape timeout (a monitor's `scrapeTimeout`) a probe leaves unused. A slow target or a hung file read is then answered with the exporter's own error before Prometheus gives up (see [Probe deadlines](../../docs/CONFIGURATION.md#probe-deadlines)). It takes a Go duration of zero or more. When it is empty, the flag is not rendered, so the exporter's default applies and an image older than the flag still starts. Set it only with an image that has the flag.

`server.probeDefaultTimeout` bounds a probe that arrives without a scrape timeout header, for example from curl, a script or the exporter's collectors page with JavaScript off. A `timeout` parameter bounds the request within it and cannot raise it. Prometheus always sends a scrape timeout, so its scrapes are unaffected. The value is a Go duration of zero or more, where `0` leaves such a probe unbounded. Like `probeTimeoutOffset`, it is rendered only when set.

`server.gzipLevel` is the gzip level for the responses of `/probe`, the self-metrics endpoint and the static targets endpoint to a scrape that accepts gzip, as Prometheus's scrapes do (see [Endpoints](../../docs/ENDPOINTS.md)):

- `1`, the exporter's default, is the fastest.
- `9` produces the smallest responses, for a scrape over a slow link.
- `0` sends responses uncompressed, which Prometheus also accepts, for when the network is cheaper than the pod's CPU.

It takes a whole number from `0` to `9`, as a number or a string. Anything else fails rendering, as does `--web.gzip-level` in `extraArgs`. The default, empty, renders no flag.

`server.statusRefresh` is how often the status page, at the static targets path followed by `/status`, refreshes itself when its URL has no `?refresh=` (see [Status page](../../docs/ENDPOINTS.md#status-page)). It is `off`, or whole hours, minutes and seconds from `5s` to `24h`, such as `30s` or `1m`, for a page left open on a wall screen. Anything else fails rendering, as does `--web.status-refresh` in `extraArgs`. The default, empty, renders no flag, and the page does not refresh by itself until someone picks an interval on it.

`server.probeMaxConcurrent` limits the trips to targets (probes and static target scrapes) of all collectors together, on top of each collector's `max_concurrent_probes`. Each trip holds a response and its series in memory, so this limits what a burst of probes can cost the pod.

`server.pythonMaxWorkers` limits the Python workers of all collectors together. Each worker is a separate process with its own memory, so pair this value with a collector's `limits.max_script_memory` (see [Python](../../docs/PYTHON.md)).

Both values take a whole number from 0 to 2147483647, where `0` means no limit, and are rendered only when set. The number may be written as a number or as a string of digits with no leading zero (the exporter's flag reads `010` as eight and refuses `08`). The exporter itself accepts a larger number on a 64-bit platform; the chart does not. Size both to `resources.limits.memory`.

`server.pythonMaxIdleWorkers` and `server.pythonIdleTimeout` set how many of each script's workers stay idle between scrapes (one by default) and for how long (two minutes by default). Each idle worker is a process of about 11 MB, or 17 MB with lxml, so ten Python collectors keep about 110 to 170 MB of the pod's memory in idle workers by default. With `0`, no worker stays idle and every scrape starts an interpreter. Keep the timeout longer than the Python collectors' scrape interval, or every scrape starts one too. The idle timeout is whole hours, minutes and seconds of at least `1s`, as the exporter accepts it; anything else fails rendering. `server.pythonMaxIdleWorkers` is a whole number in the same form as above. Both are rendered only when set (see [Idle workers and memory](../../docs/PYTHON.md#how-scripts-run)).

```sh
helm install exporter charts/prometheus-universal-exporter \
  --set server.listenAddress=0.0.0.0:9115 \
  --set server.pythonPath=/usr/bin/python3.11 \
  --set server.logLevel=debug \
  --set server.probeTimeoutOffset=1s
```

### Shutting down

`server.shutdownTimeout` is how long a stopping pod waits for probes in progress before closing them. It is rendered as `--web.shutdown-timeout` (see [Shutting down](../../docs/CONFIGURATION.md#shutting-down)). Keep it at least as long as the monitors' `scrapeTimeout`, or a rollout cuts probes off and Prometheus records failed scrapes. It takes whole hours, minutes and seconds, such as `30s` or `1m30s`, so that the chart can calculate the grace period. Like `probeTimeoutOffset`, it is rendered only when set.

`server.shutdownDelay`, `5s` by default, is how long a stopping pod keeps answering probes before that, while `/ready` answers `503`. It is rendered as `--web.shutdown-delay`. Kubernetes takes a few seconds to remove a terminating pod from its Service. Probes sent to the pod in that gap would otherwise be refused, and Prometheus would record failed scrapes during every rollout. Raise the delay on a large cluster where endpoint updates are slow. `0s` turns it off, and an empty value leaves the flag out for an image that does not have it.

Kubernetes kills a pod `terminationGracePeriodSeconds` after asking it to stop, 30 seconds by default. A stopping exporter needs the sleep of its [`preStop`](#shutting-down) hook, when it has one, plus its shutdown delay, its shutdown timeout and about 10 seconds more for the last OTLP export and for exiting. With the defaults that is 30 seconds, which Kubernetes' default already covers. When `terminationGracePeriodSeconds` is unset, the chart renders that sum whenever it is more than 30. When it is set, it must be at least that sum, or rendering fails:

```sh
helm install exporter charts/prometheus-universal-exporter \
  --set server.shutdownTimeout=1m   # renders terminationGracePeriodSeconds: 75
```

`preStop` is the exporter container's preStop hook, and `allocator.preStop`
the allocator's. Each is written as a Kubernetes lifecycle handler with
exactly one of `exec` (a command), `httpGet` (a path and a port) or `sleep`
(its `seconds`). `tcpSocket`, which Kubernetes does not run as a hook, and
more than one handler fail rendering.

Kubernetes runs the hook before telling the container to stop, within the
same grace period. The chart therefore adds a `sleep`'s seconds to the grace
period it renders, and to the minimum a configured one must have. The chart
cannot know how long an `exec` or `httpGet` hook takes, so raise
`terminationGracePeriodSeconds` to cover it.

The exporter needs no hook to leave its Service before it stops, because
`server.shutdownDelay` does that. Use a hook for anything else that must
happen first, such as a sidecar's drain or a mesh that waits for one:

```yaml
preStop:
  sleep:
    seconds: 10        # with the defaults, renders terminationGracePeriodSeconds: 40
allocator:
  terminationGracePeriodSeconds: 60
  preStop:
    sleep:
      seconds: 15      # at least 15 + 15 = 30 without discovery, 33 with it
```

The allocator's pods stop within `allocator.terminationGracePeriodSeconds`.
When it is null and the allocator has a discovery source, the chart renders
the larger of 30 and the sum of its preStop's sleep, a peer check interval
and timeout (rounded up), and 15 seconds for its server's 10-second shutdown
and its exit. Without a discovery source, the chart renders that sum only
when it is more than Kubernetes' 30. When the value is set, it must be at
least that sum, or rendering fails.

Raise `terminationGracePeriodSeconds` further if `otlp.timeout` is longer than its default of 5 seconds. With [`otlp.logs`](#otlp-export), the last export of the exporter's own log follows the metrics' export within the same `otlp.timeout` and takes at least a second, which the 10 seconds cover at the default.

`server.enableLifecycle` enables `/-/reload`. A `POST` or `PUT` to it reloads the configuration immediately and answers `200` when the configuration was accepted, or `500` with the reason when it was not. See [Reloading on demand](../../docs/CONFIGURATION.md#reloading-on-demand). A chart-managed ConfigMap does not need it, because a change rolls the Deployment. It is for a ConfigMap updated in place with `config.enabled: false`. Like `probeTimeoutOffset`, the flag is rendered only when set, so older images still start.

`server.debug` sets how far collectors can be debugged. See [Debugging collectors](../../docs/CONFIGURATION.md#debugging-collectors).

- `edit` enables `/debug`, a page that runs a collector written there, or a configured one, on a pasted response. It shows each stage of the trip and the series a probe would serve. Nothing is put in force and nothing is fetched, and a collector written on the page never runs Python or reads a file of the pod.
- `full` adds everything that shows a target's answer: fetching a target through a configured collector or scraping a static target on that page, `/probe?...&debug=true` and `/static-targets?debug=<name>`. The status page also shows each error in full, including the values of the answer it quotes and details meant for the operator alone, such as an `sshexec` command's stderr. Turn `full` on while you fix a collector, and off again afterwards.

Empty and `off` render no flag. Anything else fails rendering, as does `--web.debug` in `extraArgs`.

`server.debugDangerous: true` renders `--web.debug.dangerous` (see [Dangerous debug mode](../../docs/CONFIGURATION.md#dangerous-debug-mode)). Rendering fails unless `server.debug` is `full`, because the exporter would refuse to start, and it also fails with `--web.debug.dangerous` in `extraArgs`. **This mode is dangerous.** The debug page's playground then runs Python in a sandbox that does not stop a script written to escape it. It reads any file in the pod with the exporter's permissions, including mounted Secrets and the service account token, and makes its own requests to any host the pod can reach. Anyone who can open the page can do the same. Turn it on only for a debugging session that no one else can reach, such as a port-forward to your own release, and never in a shared deployment. The install notes show a warning while it is on.

The one-shot flags (`--dry-run`, `--config.schema`, `--config.collector-file-schema`, `--static-targets-file-schema`, `--version`, `--healthcheck`) print or check something and exit, so they have no values. Run them as a separate command or, for `--healthcheck`, with [`healthProbes.mode: exec`](#probes). A flag that an exporter image has but this chart version does not know yet goes in `extraArgs`, described under [Extra volumes and arguments](#extra-volumes-and-arguments).

### Configuration mount and rollout

The ConfigMap is mounted at `/etc/prometheus-universal-exporter/config.yaml`, and its checksum is part of the Deployment's pod template. A configuration change therefore rolls the Deployment instead of waiting for the kubelet to refresh a mounted file. The checksum also covers the inline [`sshAuth.knownHosts`](#ssh-file-collectors) lines. Set `server.watchConfig` when the pods should reload in place instead.

### Collector files

Every key of `config.data` becomes a file in the mounted configuration directory, next to `config.yaml`. This is where [collector files](../../docs/CONFIGURATION.md#collector-files) go: add each one as a key and list them under `collector_files`, relative to `config.yaml`. A key is a ConfigMap key: letters, digits, `-`, `_` and `.`, at most 253 characters, and not `.` or `..`. The directory is flat, so a key such as `collectors/payments.yaml` fails at rendering rather than at install. Name it `collectors-payments.yaml`, or mount a directory of your own as described below:

```yaml
config:
  data:
    config.yaml: |
      collector_files:
        - collectors-*.yaml
      web:
        self_metrics:
          verbose: true
    collectors-payments.yaml: |
      collectors:
        - name: payments_api
          request:
            type: http
            path: /status
          transform:
            type: jq
          metrics:
            - name: payments_queue_depth
              expression: .queue.depth
    collectors-search.yaml: |
      collectors:
        - name: search_api
          request:
            type: http
            path: /health
          transform:
            type: regex
          metrics:
            - name: search_up
              expression: 'up=(\d+)'
```

A collector file holds `collectors` and, optionally, a [`defaults`](../../docs/CONFIGURATION.md#defaults-for-the-collectors-of-a-file) block that supplies the keys its collectors do not set. It holds nothing else, and the chart mounts it as written.

- Collector names must be unique across `config.yaml` and every file, or the pod refuses to start.
- A pattern such as `*.yaml` matches every key except `config.yaml`, which the exporter never reads as a collector file. The static target document is mounted in its own directory, `/etc/prometheus-universal-exporter-static-targets`, so no pattern here matches it.
- A file changes the ConfigMap checksum as `config.yaml` does, so a change rolls the Deployment or, with `server.watchConfig`, is reloaded in place.
- Collector files kept in a separate ConfigMap can be mounted with `extraVolumes` and `extraVolumeMounts` at their own path, such as `/etc/collectors`, and listed by absolute path: `/etc/collectors/*.yaml`.

Every file reaches the pod exactly as given, whatever its name and whatever the indentation of its first line. A file is rendered into the ConfigMap as readable text. A file that YAML text cannot hold unchanged (one with carriage returns or control characters, or ending in blank lines or trailing spaces) is rendered as base64 under the ConfigMap's `binaryData` instead, and is the same file in the pod.

### Default labels and annotations

`defaultLabels` and `defaultAnnotations` are applied to every object the chart creates. Metadata set on a particular object overrides a default of the same name.

```yaml
defaultLabels:
  team: platform
defaultAnnotations:
  owner: observability@example.com
```

### Resources

```yaml
resources:
  requests:
    cpu: 100m
    memory: 128Mi
  limits:
    cpu: 500m
    memory: 512Mi
```

`goMemLimit`, on by default, renders `--runtime.memory-limit-ratio` with `goMemLimit.ratio`, `0.8`. The exporter reads the container's memory limit from its cgroup and sets the Go memory limit to that share of it. The Go runtime then collects garbage more aggressively as its heap approaches the limit, instead of growing past the container's limit into an OOM kill.

The remaining share is for memory the Go heap does not count: the Python workers, which are separate processes, and the runtime's overhead. Lower the ratio for a configuration with many Python workers, and limit those with `server.pythonMaxWorkers` and `limits.max_script_memory`. Without a memory limit in `resources`, the exporter keeps the Go default. `GOMEMLIMIT` set in `env` takes precedence over the ratio.

The ratio is from 0.1 to 1, as the exporter accepts it. Below 0.1 the Go memory limit leaves the heap almost nothing and the runtime spends its time collecting garbage, so `ratio: 0.05` fails rendering. `0.5` to `0.95` is the range that leaves room for the Python workers. Set `goMemLimit.enabled: false` for no Go memory limit. The values schema refuses an invalid ratio first, with its own message. When the schema is skipped (`--skip-schema-validation`), the templates refuse it and write the number out in full, so their message for `ratio: 1e-5` says `0.00001` rather than `1e-05`. A string is checked as written and so takes no exponent.

### Garbage collector

`goGC.percent` sets the Go garbage collector's target, the `GOGC` environment variable of the exporter container: how far the heap may grow over the live data before the next collection, in percent. The default, empty, leaves the variable out, so Go's own default of 100 applies.

Garbage collection is 13% to 22% of the exporter's CPU in every profile of a probe, so collecting less often saves a good part of it. Measured on 2 CPUs with `GOGC=400` instead of the default, a Prometheus pass-through probe of 5,000 series took 14.2 ms instead of 18.1 ms, a jq probe over 5,000 items 58 ms instead of 78 ms, and a csv probe 11.9 ms instead of 16.8 ms: 15% to 30% less. The cost is memory. Between collections the heap may grow to about (1 + percent/100) times the live data, which is five times at 400 where the default allows two.

The Go memory limit above is what makes a higher target safe. With a memory limit in `resources` and `goMemLimit` on, Go collects sooner as the heap approaches the limit, whatever `GOGC` says, so a higher target cannot push the process past it. Without a container memory limit there is no such ceiling, and the heap grows as far as the target allows. With a memory limit, 200 to 400 is a good starting point:

```yaml
resources:
  limits:
    memory: 512Mi
goMemLimit:
  enabled: true
  ratio: 0.8
goGC:
  percent: 400
```

The value is a whole number from 1 to 10000, as a number or a string, or `"off"`, which collects nothing until the heap is near the Go memory limit.

- `off` fails rendering unless a limit is in force: `goMemLimit.enabled` with `resources.limits.memory` set, or your own `GOMEMLIMIT` in `env`, which takes precedence over the ratio. A number needs no limit.
- `off` needs its quotes in a values file. YAML reads a bare `off` as `false`, which the values schema refuses.
- `GOGC` in `env` together with `goGC.percent` fails rendering, because the container would get the variable from both. `GOGC` in `env` alone is rendered as written, and the chart does not check it.
- Changing the value changes the pod template, so it rolls the pods like any other change to the Deployment.

### Probes

The chart checks `/health` for liveness and `/ready` for readiness on the `http` port. `livenessProbe` and `readinessProbe` set the timings. The check itself is fixed by the chart, so setting `httpGet`, `exec`, `tcpSocket` or `grpc` fails rendering. `terminationGracePeriodSeconds` is accepted on `livenessProbe` only: Kubernetes refuses it on a readiness probe, so the values schema does too. The liveness probe is lenient by default, because a pod busy with a burst of probes is slow rather than dead, and a restart would lose its cache and Python workers:

```yaml
livenessProbe:
  periodSeconds: 10
  timeoutSeconds: 3
  failureThreshold: 5
readinessProbe:
  periodSeconds: 10
  timeoutSeconds: 3
  failureThreshold: 3
```

With `healthProbes.mode: exec`, the kubelet instead runs the exporter's own [health check](../../docs/CONFIGURATION.md#health-checks) in the container: `/bin/prometheus-universal-exporter --healthcheck=health` for liveness and `--healthcheck=ready` for readiness. Each gets the `--web.listen-address` the container is given, so the check follows `server.listenAddress`. It asks the same endpoints from inside the pod, for a cluster where the kubelet cannot reach the pod's address.

Each run starts a process in the container, which a `GET` does not, so the default `http` mode is cheaper where it works. `exec` needs the exporter at `/bin/prometheus-universal-exporter`, which is where the exporter's image has it. The timings above still apply. The check gives up after 5 seconds by itself, and the kubelet gives up at `timeoutSeconds`.

```yaml
healthProbes:
  mode: exec
```

### Replicas

```yaml
replicaCount: 2
```

Probes scale with replicas, because Prometheus sends each probe to one pod through the Service. Static targets do not: every replica scrapes every static target on its own schedule and serves its own results. Run [static targets](#static-targets) with `replicaCount: 1` (the chart's notes warn otherwise), or split them across releases.

A change rolls the pods according to `strategy`, which is `RollingUpdate` by default with `maxUnavailable: 0` and `maxSurge: 1`. The old pod stays until its replacement is ready, so for a moment there is one extra pod. For no overlap, set `strategy.type: Recreate`, which stops the old pods first. The chart then leaves `strategy.rollingUpdate` out of the Deployment, because Kubernetes refuses it there, so this one line is enough:

```yaml
strategy:
  type: Recreate
```

With `RollingUpdate`, setting both `maxSurge` and `maxUnavailable` to 0 (as `0` or `0%`) fails rendering, because Kubernetes refuses a rolling update that can neither add a pod nor remove one. Set one of them above 0. A count left out of `strategy.rollingUpdate` is not rendered, and Kubernetes uses its default of 25% for it.

To keep replicas apart, spread them across zones or nodes. A constraint without a `labelSelector` gets one that selects this release's pods:

```yaml
topologySpreadConstraints:
  - maxSkew: 1
    topologyKey: topology.kubernetes.io/zone
    whenUnsatisfiable: ScheduleAnyway
priorityClassName: monitoring
```

### Autoscaling

`autoscaling` renders an `autoscaling/v2` HorizontalPodAutoscaler, which then sets the Deployment's replica count. The chart leaves `replicaCount` out of the Deployment so that an upgrade does not reset it. By default it scales on CPU utilization, measured against `resources.requests`. You can add `targetMemoryUtilizationPercentage`, the exporter's own load (see below) and further `metrics`, and set scale-up and scale-down policies in `behavior`. A `maxReplicas` below `minReplicas`, or nothing to scale on, fails rendering.

```yaml
autoscaling:
  enabled: true
  minReplicas: 2
  maxReplicas: 6
  targetCPUUtilizationPercentage: 75
```

Each replica keeps its own [response cache](../../docs/CONFIGURATION.md#response-caching) and shares in-flight requests only among its own identical probes, and the Service spreads probes across replicas. As replicas are added, the cache therefore answers a smaller share of probes and more of them reach the targets. With a 30s `ttl` and four replicas, a target probed every 15s may be asked up to four times per `ttl` instead of once, and a result kept for `stale_if_error` is served only by the replica that stored it. Where the cache is what protects a target, keep the replica count low, or give Prometheus's scrapes of one target a stable replica: a Service with `sessionAffinity: ClientIP` sends every probe from one Prometheus to one pod.

CPU is a poor measure for an exporter that mostly waits on its targets. Two values scale on the exporter's own load instead:

- `targetBusy` scales on how busy the pods are: the average number of probes and static target scrapes in progress per pod, from the exporter's `http_exporter_probes_seconds_total` and `http_exporter_static_targets_scrape_seconds_total`. It is a number above 0 or a quantity such as `"1500m"`.
- `targetStaticTargetsPerPod` scales on the static targets per pod, from `http_exporter_static_targets`, with the [target allocator](#target-allocator). It is a whole number. Without the allocator every pod scrapes every static target, so the number per pod does not fall as pods are added, and setting the value fails rendering.

Each renders a `Pods` metric with an `AverageValue` target, named by `busyMetric` (`http_exporter_busy`) and `staticTargetsMetric` (`http_exporter_static_targets`). A metrics adapter must serve these metrics from Prometheus, and Prometheus must scrape the self-metrics of every pod, as the [self-metrics monitor](#4-configure-prometheus) does. [Autoscaling](../../docs/SELF-METRICS.md#autoscaling) in the self-metrics documentation has the prometheus-adapter rules for the two names, and a KEDA ScaledObject that does the same without an adapter.

```yaml
allocator:
  enabled: true
autoscaling:
  enabled: true
  minReplicas: 2
  maxReplicas: 10
  targetCPUUtilizationPercentage: null
  targetBusy: 3
  targetStaticTargetsPerPod: 200
```

Autoscaling suits a release that serves probes, or one whose static targets the [target allocator](#target-allocator) distributes across the pods. It does not suit [static targets](#static-targets) without the allocator. Every replica scrapes every static target, so each replica the autoscaler adds sends another full set of requests to every target, which increases the load it was meant to spread, and each replica serves its own results. A target with `export_via_otlp` is exported over OTLP by every replica, so the OTLP backend receives duplicate series under the same resource. The chart's notes warn when static targets are rendered with autoscaling and name the targets exported over OTLP. Put static targets in their own release, with `replicaCount: 1` and autoscaling off.

`podDisruptionBudget` renders a PodDisruptionBudget, so a node drain does not take down every replica at once. Set one of `minAvailable` or `maxUnavailable`, as a count or a percentage. Setting both fails rendering, and setting neither gives `maxUnavailable: 1`.

Two settings block node drains, and the chart's notes warn about both:

- A `minAvailable` that covers every replica, as a count of `replicaCount` or more or as `100%`. With one replica, `minAvailable: 1` blocks a drain until the pod is deleted by hand.
- `maxUnavailable: 0`. It is valid Kubernetes and is rendered as set, but it allows no voluntary eviction, so a drain of a node running the exporter waits indefinitely.

```yaml
replicaCount: 3
podDisruptionBudget:
  enabled: true
  maxUnavailable: 1
```

### Service

The exporter Service is enabled by default:

```yaml
service:
  enabled: true
  type: ClusterIP
  port: 8080
```

`service.type` is `ClusterIP`, `NodePort` or `LoadBalancer`. `ExternalName` is not accepted: it is a DNS alias with no endpoints, through which neither Prometheus nor the monitors could reach the exporter.

`NodePort` and `LoadBalancer` make the exporter reachable from outside the cluster, so they require the exporter to authenticate its callers. See [Exposing the exporter](#exposing-the-exporter).

`service.sessionAffinity: ClientIP` sends every probe from one Prometheus to one pod, so with several replicas its probes keep reaching that pod's [cache](#autoscaling). The default, empty, leaves Kubernetes' `None`.

### Ingress

```yaml
webAuth:
  enabled: true
  secretName: exporter-auth        # keys: username, password
ingress:
  enabled: true
  className: nginx
  hosts:
    - host: exporter.example.com
      paths:
        - path: /
          pathType: Prefix
```

The Ingress routes to the exporter's Service, so it needs `service.enabled: true`; rendering fails with the Service off. It makes the exporter reachable from outside the cluster, so the exporter must authenticate its callers. This example uses `webAuth`, and the configuration's `web.basic_auth` must point at its files as in [Exporter authentication](#exporter-authentication), or rendering fails. See [Exposing the exporter](#exposing-the-exporter). The Ingress terminates TLS, configured with `ingress.tls`; the exporter serves plain HTTP.

### Exposing the exporter

`/probe` sends a request on its caller's behalf to any target that `allowed_targets` and `denied_targets` permit, and answers with what the collector read. An exporter that anyone can reach without a credential is therefore an open proxy into every network its pod can reach, and it discloses what its collectors read. For this reason rendering fails when the exporter is reachable from outside the cluster (through `ingress.enabled`, or `service.type` `NodePort` or `LoadBalancer`) while `webAuth.enabled` is false. The message names the setting that exposes the exporter.

`webAuth` only mounts the Secret. The exporter authenticates callers only when the configuration's `web.basic_auth` reads the mounted files; a configuration that does not still serves `/probe` to anyone. So with `webAuth.enabled` and the exporter exposed, when the chart renders the configuration, rendering also fails unless `config.data."config.yaml"` has `web.basic_auth` with `enabled: true` and `password_file` set to `<webAuth.mountPath>/password` (`/var/run/prometheus-universal-exporter/web-auth/password` by default), as in [Exporter authentication](#exporter-authentication). The message names that path.

The chart checks only `config.yaml`, because `web` is read from the main file alone. A `config.yaml` that is not valid YAML is left for the exporter, which refuses it. With `config.enabled: false` the chart cannot see the configuration and checks nothing. Pointing the external ConfigMap's `web.basic_auth` at the files is then your responsibility, and `exposure.allowWithoutWebAuth` is how you state that callers are authenticated another way.

`webAuth` is the only place where the chart can see that the exporter authenticates its callers. The chart does not read a `web.basic_auth` with an inline password in `config.data."config.yaml"`, or one in an external ConfigMap. Authentication at the ingress controller, such as ingress-nginx's `nginx.ingress.kubernetes.io/auth-type` annotations, is handled by the controller. In either case, state it with `exposure.allowWithoutWebAuth`:

```yaml
exposure:
  allowWithoutWebAuth: true   # callers authenticate at the ingress controller
ingress:
  enabled: true
  className: nginx
  annotations:
    nginx.ingress.kubernetes.io/auth-type: basic
    nginx.ingress.kubernetes.io/auth-secret: exporter-ingress-auth
  hosts:
    - host: exporter.example.com
      paths:
        - path: /
          pathType: Prefix
```

The same value lets you expose an exporter without a credential on purpose, for example behind a load balancer that only a private network can reach. A `ClusterIP` Service, the default, is reachable only from inside the cluster and needs neither. Limit who can reach it there with a [NetworkPolicy](#networkpolicy).

### NetworkPolicy

Enable a NetworkPolicy to restrict which workloads can reach the exporter and, with egress rules, which targets it can reach:

```yaml
networkPolicy:
  enabled: true
```

On its own, this allows ingress only to the `http` port, from anywhere, which is what Prometheus scrapes. It leaves egress open, because the chart does not know the targets a probe will name. `ingress` rules replace that default. `egress` rules limit egress to what they allow, plus DNS (port 53 over UDP and TCP) unless `allowDNS: false`:

```yaml
networkPolicy:
  enabled: true
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: monitoring
      ports:
        - port: http
  egress:
    - to:
        - ipBlock:
            cidr: 10.0.0.0/8
```

## Using an external ConfigMap

By default, the chart manages the exporter ConfigMap:

```yaml
config:
  enabled: true
```

If the configuration is managed outside Helm, disable the chart-managed ConfigMap:

```yaml
config:
  enabled: false
```

With an external ConfigMap, you can enable configuration watching:

```yaml
server:
  watchConfig: true
  watchConfigInterval: 60s
```

The exporter then reloads the configuration when the mounted ConfigMap changes, without a Deployment rollout.

Static targets are not part of that ConfigMap. With `staticTargets.enabled`, the chart renders `staticTargets.data` into its own ConfigMap, `<release>-prometheus-universal-exporter-static-targets`, whether or not `config.enabled` is on.

### Environment variables in the configuration

`server.expandEnv` adds `--config.expand-env`, which substitutes `${NAME}` references in the configuration and its collector files from the container's environment before they are parsed. `staticTargets.expandEnv` adds `--static-targets.expand-env`, which does the same for the static target document, independently. A hostname, tenant or token can then come from a Secret instead of the ConfigMap the chart renders:

```yaml
server:
  expandEnv: true

env:
  - name: API_TOKEN
    valueFrom:
      secretKeyRef:
        name: exporter-secrets
        key: api-token

envFrom:
  - secretRef:
      name: exporter-secrets

config:
  data:
    config.yaml: |
      collectors:
        - name: example
          request:
            type: http
            headers:
              Authorization: Bearer ${API_TOKEN}
```

For a static target's address or credentials, set `staticTargets.expandEnv` the same way:

```yaml
staticTargets:
  enabled: true
  expandEnv: true
  data: |
    interval: 1m
    targets:
      - name: billing
        collector: example
        target: ${BILLING_URL}
        request:
          bearer_token: ${BILLING_TOKEN}
```

`env` and `envFrom` take the ordinary Kubernetes forms and are useful on their own; `expandEnv` does nothing without them. Expansion is off by default. Only `${NAME}` is substituted, never `$NAME`, but a configuration with regexes, jq expressions or Python pre-scripts contains dollar signs that are not references, so expansion should be a deliberate choice. A reference to a variable that is not set stops the exporter at startup and names the variable, instead of becoming an empty string. A missing Secret key is then a clear failure rather than a collector silently scraping the wrong thing.

Behind an egress proxy, set `HTTPS_PROXY`, `HTTP_PROXY` and `NO_PROXY` the same way. Target requests and OTLP exports use them; no configuration key does. See [Proxies](../../docs/REQUESTS.md#proxies). Keep the cluster's own names in `NO_PROXY` so that in-cluster targets and the OpenTelemetry Collector are reached directly:

```yaml
env:
  - name: HTTPS_PROXY
    value: http://proxy.corp.example:3128
  - name: NO_PROXY
    value: .svc,.cluster.local,10.0.0.0/8
```

### OTLP export

OTLP export is part of the configuration, not the values: set the `otlp` block inside `config.data.config.yaml` (see [OTLP export](../../docs/OTLP.md)).

- Its `logs` block also forwards the exporter's own log to the same collector at its `/v1/logs` path, in addition to the pod's stdout and stderr that `kubectl logs` reads (see [The exporter's own log](../../docs/OTLP.md#the-exporters-own-log)).
- Its `traces` block exports, at the collector's `/v1/traces` path, a trace of each probe and static target scrape that took `threshold` or longer. With `sample_ratio` it also exports a share of the rest, at most `max_per_second` per second (see [Traces of slow requests](../../docs/OTLP.md#traces-of-slow-requests) and [Which traces are exported](../../docs/OTLP.md#which-traces-are-exported)).
- Traces are linked to the log records and the self-metrics' exemplars of the same probe and, with `propagate`, sent on to the targets (see [Linking metrics, logs and traces](../../docs/OTLP.md#linking-metrics-logs-and-traces)).

Keep a collector's token in a Secret. `env` and `server.expandEnv` put it in the headers that every export sends:

```yaml
server:
  expandEnv: true

env:
  - name: OTLP_TOKEN
    valueFrom:
      secretKeyRef:
        name: otel-collector-credentials
        key: token

config:
  data:
    config.yaml: |
      otlp:
        enabled: true
        endpoint: http://otel-collector.observability:4318/v1/metrics
        headers:
          Authorization: Bearer ${OTLP_TOKEN}
        logs:
          enabled: true
          level: info
        traces:
          enabled: true
          threshold: 2s
          # Also a share of the other traces, at most 20 a second in all:
          # sample_ratio: 0.01
          # max_per_second: 20
          # Send each request to a target the trace's traceparent:
          # propagate: true
      collectors:
        - name: example
          request:
            type: http
          transform:
            type: regex
          metrics:
            - name: example_value
              expression: 'value=(\d+)'
```

A failing log or trace export never makes the pod unready. The exporter's [status page](../../docs/ENDPOINTS.md#otlp-export) and the `http_exporter_otlp_log_*`, `http_exporter_otlp_trace_exports_total` and `http_exporter_otlp_spans_*` [self-metrics](../../docs/SELF-METRICS.md#otlp-export-status) show how the exports are doing. Behind an egress proxy, keep the collector's name in `NO_PROXY`, as [above](#environment-variables-in-the-configuration).

Every replica exports under the same `service.name`, so a backend cannot tell from the resource which pod sent a point or a log line. The `exporter_instance` block adds an attribute that names the replica, `pue.exporter.instance` by default, to every resource the export sends (see [The exporter instance](../../docs/OTLP.md#the-exporter-instance)). Its value defaults to the pod's name. That is the pod's host name and, with the [allocator](#target-allocator), also the `--allocator.member` name the chart passes, `$(POD_NAME)`:

```yaml
allocator:
  enabled: true

config:
  data:
    config.yaml: |
      otlp:
        enabled: true
        endpoint: http://otel-collector.observability:4318/v1/metrics
        exporter_instance:
          enabled: true
      collectors:
        - name: example
          request:
            type: http
          transform:
            type: regex
          metrics:
            - name: example_value
              expression: 'value=(\d+)'
```

With the allocator, a static target that moves to another replica continues under the other pod's name, as a new resource. See the caveat in [The exporter instance](../../docs/OTLP.md#the-exporter-instance).

### Exporter resource metrics

The exporter publishes the standard `go_` and `process_` series for its own CPU and memory, under the names existing Go dashboards use. They are on by default. To leave them out, set `web.self_metrics.resource_metrics_enabled: false` inside `config.data.config.yaml`:

```yaml
config:
  data:
    config.yaml: |
      web:
        self_metrics:
          resource_metrics_enabled: false
      collectors:
        - name: example
          request:
            type: http
          ...
```

The self-metrics monitor scrapes them with everything else on that endpoint. They carry no per-collector or per-target labels, so they add a fixed number of series, not one per target. Reading them does not stop the world: the memory statistics come from `runtime/metrics` and the garbage collection pauses from `runtime/debug.ReadGCStats`, so scraping them is cheap. See [Resource metrics](../../docs/SELF-METRICS.md#resource-metrics) for every series and its source.

## Extra volumes and arguments

The chart mounts one ConfigMap, the one it renders, and passes the flags it derives from the values above. `extraVolumes`, `extraVolumeMounts` and `extraArgs` cover everything else, in the ordinary Kubernetes and command-line forms. For example, they mount a ConfigMap or Secret that the chart does not create beside the standard one:

```yaml
extraVolumes:
  - name: extra-collectors
    configMap:
      name: my-collectors
  - name: internal-ca
    secret:
      secretName: internal-ca

extraVolumeMounts:
  - name: extra-collectors
    mountPath: /etc/collectors
    readOnly: true
  - name: internal-ca
    mountPath: /etc/ssl/internal
    readOnly: true

extraArgs:
  - --some.new-flag=value
```

`extraVolumes` and `extraVolumeMounts` are passed through unchanged, so anything a pod can mount works here: a ConfigMap, a Secret, a projected volume, an emptyDir. The entries are appended after the chart's own, not substituted for them. They mount into the exporter's pods; the [target allocator](#target-allocator)'s pods take `allocator.extraVolumes` and `allocator.extraVolumeMounts` instead. `extraArgs` entries are appended after the chart's own flags, each entry one whole argument.

Three mistakes are rejected while rendering, because each would otherwise fail in a way that does not point back to the values file:

* **An `extraArgs` entry that sets a flag the chart already renders**, such as `--web.listen-address`, `--config.file`, `--python.path`, `--log.level` or `--probe.timeout-offset`. Go keeps the last occurrence of a repeated flag, so the entry would silently win. For the listen address, the container port and the probes would still follow `server.listenAddress`, leaving a pod that listens on one port while Kubernetes checks another. The error names the value to set instead.
* **An `extraVolumeMounts` entry whose `mountPath` the chart already mounts, or that another entry uses.** The chart's paths are the configuration directory, the static target directory `/etc/prometheus-universal-exporter-static-targets` while `staticTargets.enabled` is on, and `targetAuth.mountPath`, `webAuth.mountPath`, `sshAuth.mountPath` and `sqlAuth.mountPath` while they are enabled. Mounting over `/etc/prometheus-universal-exporter` replaces it, so the exporter starts with no `config.yaml` and crash-loops with an error about the file, not about the mount that hid it. To add a file to that directory, mount it at its own path, such as `/etc/collectors`, and point the configuration at it. Paths are compared without a trailing slash, so `/etc/prometheus-universal-exporter/` is the same path.
* **A mount below a directory the chart mounts from a ConfigMap or a Secret.** This covers an `extraVolumeMounts` entry or a credential's `mountPath` inside the configuration directory (such as a `subPath` file at `/etc/prometheus-universal-exporter/extra.yaml`), inside the static target directory, or inside `targetAuth.mountPath`, `webAuth.mountPath`, `sshAuth.mountPath` or `sqlAuth.mountPath` while they are enabled. It also covers a credential's `mountPath` above the configuration or static target directory. Kubernetes mounts those volumes read-only, so the mount point cannot be created in them and the container never starts (`CreateContainerError`). The error names the value and the directory to mount outside of. To add a file to the configuration directory, add it to `config.data`.

A [`localfile`](../../docs/LOCALFILE.md) collector reads files the same way: mount the directory it names as `request.root` with these values, read-only, as shown in [Local files in Kubernetes](../../docs/LOCALFILE.md#in-kubernetes).

`--dry-run`, `--config.schema`, `--config.collector-file-schema`, `--static-targets-file-schema`, `--version`, `--healthcheck` and `--help` are rejected in `extraArgs` as well. Each prints or checks something and exits, so a pod started with one would restart indefinitely instead of serving. Run them as a separate command, a Job or an init container instead. See [Dry run](../../docs/CONFIGURATION.md#dry-run).

An entry that does not begin with `--` is rejected too, because an argument such as `some.new-flag=value` is read as a positional value and ignored.

## Static targets

The exporter can scrape a fixed list of targets itself, each on its own
interval, and serve their latest results on one endpoint for Prometheus to
scrape. A target with `export_via_otlp: true` is also exported over OTLP.
See [Static targets](../../docs/STATIC-TARGETS.md) for the document format.

Run static targets with `replicaCount: 1` and [autoscaling](#autoscaling) off.
Every replica scrapes every static target. With three replicas, each target
is therefore contacted three times per interval, each replica serves its own
results through the one Service, and a target with `export_via_otlp` is
exported over OTLP three times, as duplicate series. To spread many static
targets, split them across releases, each with its own document, or let the
[target allocator](#target-allocator) distribute them across the replicas.

```yaml
staticTargets:
  enabled: true
  data: |
    interval: 1m
    targets:
      - name: legacy_eu
        collector: legacy_text
        target: http://legacy.eu.example:8080
        request:
          path: /status
        labels:
          region: eu
```

| Value | Default | Purpose |
| --- | --- | --- |
| `staticTargets.enabled` | `false` | Render `data` into the ConfigMap `<fullname>-static-targets`, mount it at `/etc/prometheus-universal-exporter-static-targets` and pass it with `--static-targets-file`. |
| `staticTargets.fileName` | `static-targets.yaml` | Its file name in `/etc/prometheus-universal-exporter-static-targets`, which is its key in that ConfigMap: letters, digits, `-`, `_` and `.`. The directory is not the configuration directory, so no `collector_files` pattern matches the document. |
| `staticTargets.path` | `/static-targets` | Rendered as `--web.static-targets-path`, always: where the targets' latest results are served, and, followed by `/status`, the [status page](https://github.com/eenchev/prometheus-universal-exporter/blob/main/docs/ENDPOINTS.md#status-page) of the static targets and the recent probes. It may not be `selfMetrics.path` or another endpoint's, and `selfMetrics.path` may not be the status page's; either fails rendering. |
| `staticTargets.data` | `""` | The static target document, required with `enabled`, also with `config.enabled: false`. A change rolls the Deployment. |
| `staticTargets.expandEnv` | `false` | Rendered as `--static-targets.expand-env` with `enabled`: expand `${NAME}` references in `data` from the container's environment. Independent of `server.expandEnv`. |
| `staticTargets.monitor.enabled` | `true` | Render the monitor that scrapes the endpoint, when `staticTargets.enabled` is. It needs the Prometheus Operator's CRDs. |
| `staticTargets.monitor.type` | `service` | `service` for a ServiceMonitor, `pod` for a PodMonitor. |
| `staticTargets.monitor.interval` / `scrapeTimeout` | `30s` / `10s` | How often Prometheus reads the endpoint. The exporter scrapes the targets on the document's own intervals whatever this is. A `scrapeTimeout` longer than `interval` fails rendering. |
| `staticTargets.monitor.labels` / `annotations` | `{}` | Added to the monitor. |
| `staticTargets.monitor.relabelings` / `metricRelabelings` | `[]` | Passed to the monitor's endpoint. |
| `staticTargets.monitor.targets` | `[]` | Names of the static targets the monitor reads, rendered as the endpoint's `targets` parameter; empty reads every target. A name that is not a target in `data` fails rendering. |

The monitor sets `honorLabels: true`, so the series keep their own
`static_target` and `target` labels and the targets' labels instead of
Prometheus's. This is why a target cannot set `job` or `instance`: the
exporter refuses them, because they would replace the labels Prometheus gives
the endpoint's series. With `webAuth.enabled`, the monitor presents the
exporter's credential, as the self-metrics monitor does.

A target with `export_via_otlp` needs `otlp.enabled: true` in the exporter
configuration. When the chart manages the configuration, rendering fails if a
target sets it while OTLP export is off, so the problem is caught before a pod
fails to start.

## Uptime probes

An uptime check of HTTP and HTTPS sites, which is what blackbox_exporter's
`http` prober does, takes one collector
([Uptime probes](../../docs/REQUESTS.md#uptime-probes)). The sites to check
are listed in a Prometheus Operator `Probe`, as they are for
blackbox_exporter. Each `probes` entry renders one `Probe` named
`<release>-prometheus-universal-exporter-<name>`. Its prober is the
exporter's Service, `<fullname>.<namespace>.svc:<service.port>`, at `/probe`,
with the entry's `collector` sent as the `collector` parameter. The
Prometheus Operator sends each target as the `target` parameter and labels
its series with it as `instance`, so the entry needs no relabeling. A
complete `values.yaml`:

```yaml
config:
  data:
    config.yaml: |
      collectors:
        - name: site
          report_failures: true
          request:
            type: http
            method: HEAD
            follow_redirects: true
            new_connection: true
            accept_status: [any]
          decoder:
            type: none
          transform:
            type: jq
          metrics:
            - name: site_up
              expression: 'if $status >= 200 and $status < 300 then 1 else 0 end'
            - name: site_http_status_code
              expression: $status
            - name: site_http_duration_seconds
              items: '$timings | to_entries[] | select(.value != null)'
              expression: .value
              labels:
                - name: phase
                  expression: .key
            - name: site_tls_cert_expiry_timestamp_seconds
              expression: $tls.not_after
              required: false

probes:
  - name: sites
    collector: site
    interval: 1m
    scrapeTimeout: 30s
    jobName: site
    targets:
      - https://www.example.com
      - https://shop.example.com/health
    targetLabels:
      team: web
```

For a release `exporter` in the namespace `monitoring`, this renders the
following Probe (labels and annotations omitted):

```yaml
apiVersion: monitoring.coreos.com/v1
kind: Probe
metadata:
  name: exporter-prometheus-universal-exporter-sites
spec:
  jobName: "site"
  prober:
    url: "exporter-prometheus-universal-exporter.monitoring.svc:8080"
    scheme: http
    path: /probe
  interval: 1m
  scrapeTimeout: 30s
  params:
    - name: collector
      values: ["site"]
  targets:
    staticConfig:
      static:
        - https://www.example.com
        - https://shop.example.com/health
      labels:
        team: web
```

Instead of `targets`, an entry can set `ingress`, which probes every host and
path of the Ingresses its `selector` matches, in the Probe's own namespace or
in the namespaces its `namespaceSelector` gives (`matchNames`, or
`any: true`). An entry must have exactly one of the two. With both, the
Prometheus Operator would probe the static targets and ignore the Ingresses,
so both together fail rendering, and so does neither. `selector` is required,
because an empty one selects every Ingress; write `selector: {}` if that is
what you mean.

```yaml
probes:
  - name: ingresses
    collector: site
    ingress:
      selector:
        matchLabels:
          probe: "true"
      namespaceSelector:
        any: true
```

The collector is sent in `spec.params`, never in `spec.module`. The
Prometheus Operator sends `spec.module` as `module=`, a parameter the
exporter does not read. The other keys of an entry:

- `params` adds other `/probe` parameters, each a list, such as
  `timeout: [10s]`. `collector` and `target` are set by the chart and fail
  rendering.
- `relabelings` is rendered as the targets' `relabelingConfigs`, and
  `metricRelabelings` as the Probe's.
- `labels` and `annotations` are put on the Probe itself, for a Prometheus
  `probeSelector`.
- `targetLabels` labels the series of the static targets. It is refused
  together with `ingress`, whose targets take no labels in a Probe; label
  those with `relabelings` (`action: replace`, `targetLabel` and
  `replacement`).
- `interval` and `scrapeTimeout` are Prometheus durations, with the timeout
  no longer than the interval. They are left out when unset, so
  Prometheus's defaults apply.
- `jobName` sets the `job` label, which is `probe/<namespace>/<Probe name>`
  without it.

The checks for a monitor entry apply here too:

- The name must be a DNS-1123 label, unique among the entries.
- The collector is required and, when the chart holds the configuration,
  must be one that `config.data` defines.
- The Probes need `service.enabled`.
- The target allocator, whose probe targets are `allocator.probeJobs`,
  refuses a `probes` entry.

With `webAuth.enabled`, a Probe without its own `auth` presents the `webAuth`
Secret, as a monitor does, because the exporter's Basic Auth protects
`/probe`. A Probe's own `auth` takes the same keys as a
[monitor's](#monitor-authentication).

`Probe` is a Prometheus Operator kind, like the monitors. With `probes`
empty, the default, nothing is rendered and the cluster needs no Probe CRD.
A rendered Probe also renders the self-metrics monitor, as any other monitor
does.

## Target allocator

With several exporter replicas, the target allocator distributes the work
among them. It gives each pod its share of the static targets, and publishes
the probe targets to Prometheus already assigned to one pod. Each target is
then scraped by one replica, and a pod's connection pools and cache serve the
same targets every time. The allocator is `pue-allocator`, a binary in the
exporter's image. See [Target allocator](../../docs/ALLOCATOR.md) for how it
allocates, its input files and its API.

```yaml
replicaCount: 4
allocator:
  enabled: true
  staticTargets:
    data: |
      interval: 1m
      targets:
        - name: legacy_eu
          collector: legacy_text
          target: http://legacy.eu.example:8080
  probeJobs:
    - name: websites
      collector: html_status
      params:
        timeout: 10s
      targets:
        - https://shop.example
        - https://docs.example
```

A target's or a probe job's `weight` is its cost to a pod relative to the
other targets. `server.allocatorCapacity` (`--allocator.capacity`, 1 by
default) is the capacity of the exporter pods relative to the allocator's
other members. The allocator distributes the weight in proportion to the
capacity (see [Weights and capacity](../../docs/ALLOCATOR.md#weights-and-capacity)).

`allocator.enabled` renders the following.

**The allocator.** A Deployment `<fullname>-allocator` with
`allocator.replicas` pods (2 by default), a headless Service of the same name
on port 9116, a PodDisruptionBudget (`allocator.pdb.enabled`) and a
NetworkPolicy (`allocator.networkPolicy.enabled`).

* Its pods are scheduled by `allocator`'s own `nodeSelector`, `tolerations`,
  `affinity`, `priorityClassName` and `topologySpreadConstraints`, not the
  exporter's, and are spread across nodes by default where possible.
* The allocator mounts the configuration (`config.data`, or your ConfigMap
  with `config.enabled: false`) and two ConfigMaps of its own:
  `<fullname>-allocator-static-targets` from `allocator.staticTargets.data`,
  and `<fullname>-allocator-probe-targets` from `allocator.probeJobs`. A
  change to any of them rolls the allocator, not the exporter.
* A new allocator pod counts as available `minReadySeconds` after it is
  ready. Its `/ready` answers once it has read its files, but it needs longer
  before it hands out targets. Its first member list takes effect no sooner
  than `max(allocator.settle, allocator.heartbeatInterval)` after it starts,
  and no sooner than the interval at which the exporter pods heartbeat the
  old pods, counted from when the first exporter pod finds it. The exporter
  pods then learn of it at their next heartbeat, one heartbeat interval
  later.
* The chart cannot know the old pods' interval when a rollout changes
  `heartbeatInterval`. It therefore sets `minReadySeconds` to
  `max(allocator.settle, 2m)` plus `allocator.heartbeatInterval`, where 2m
  is the longest interval an allocator announces. That is 130 seconds with
  the default `settle` and `heartbeatInterval`, for one pod as for several.
  A rollout then does not replace the old allocators before a new one hands
  out targets, even when it shortens the interval. The rest of the 2m covers
  the time the exporter pods' lookups take to find the new pod when the old
  interval was shorter. The old pods keep serving in the meantime.

**The exporter pods in allocator mode.** They have no configuration or static
target file. They get `--allocator.url` pointing at the allocator's Service,
the pod's name as the member name, the pod's IP and port as the address
Prometheus probes, and a cache directory (an `emptyDir`) where the pod keeps
the last configuration and share it was given.

* The `emptyDir` survives a container restart, so a restart while the
  allocator is unavailable resumes from it. It does not survive the pod's
  replacement: a new pod starts empty and waits for the allocator.
* The credential Secrets (`targetAuth`, `webAuth`, `sshAuth`, `sqlAuth`) and
  `extraVolumes` stay mounted as before, because the configuration names
  their files by path.
* The pods are not ready, and answer `/probe` with `503`, until they have a
  configuration.

**A Prometheus Operator `ScrapeConfig` per probe job**
(`allocator.scrapeConfig`). It reads the job's targets from the allocator's
`/v1/sd/<job>` every `refreshInterval` (30s) and probes each target at the pod
the allocator chose. It needs the ScrapeConfig CRD, and your Prometheus's
`scrapeConfigSelector` must select it, for example by
`allocator.scrapeConfig.labels`. No relabeling is needed: the allocator's
answer sets the address, the path, the parameters and `instance`.

**The static targets monitor** (`staticTargets.monitor`), when the allocator
has static targets. It reads every pod's share.

**A `ServiceMonitor` for the allocator**, with
`allocator.serviceMonitor.enabled`. It is named `<fullname>-allocator` and
scrapes the allocator's own `/metrics` through its Service, and only that
Service, presenting the allocator's token as `Bearer` credentials. It needs
the ServiceMonitor CRD. The allocator's NetworkPolicy admits the
`allocator.networkPolicy.prometheus` peers, so list your Prometheus there.

### The allocator's token

The allocator always requires a bearer token. It refuses every request except
its health probes without it; the exporter pods send it, and the
ScrapeConfigs and the allocator's ServiceMonitor present it. Without a token,
any pod the allocator's NetworkPolicy admits (by default a pod labelled
`app.kubernetes.io/name: prometheus` in any namespace) could heartbeat as a
member, receive a share of the targets, and have Prometheus probe it with the
exporter's credential.

With `allocator.tokenSecret.name` empty, the default, the chart renders a
Secret `<fullname>-allocator-token` that holds a random token under
`allocator.tokenSecret.key` (`token`). An upgrade reads the token back from
that Secret, so it stays the same. `helm template` and a client-side
`--dry-run` cannot read the cluster, so each of their renders generates a new
token. When such a render is applied, the Secret changes. The allocator and
the exporter pods read the new token from their mounted file once the kubelet
updates it, within about a minute, and requests are refused between the two
updates until both have it. To keep one token whatever renders the chart,
create the Secret yourself and name it:

```sh
kubectl create secret generic pue-allocator-token --from-literal=token="$(head -c 32 /dev/urandom | base64)"
```

```yaml
allocator:
  tokenSecret:
    name: pue-allocator-token
```

The chart then generates no token. The allocator does not compare a member's
advertised address with the address its heartbeat comes from. A service mesh
sidecar or a proxy in between would make them differ for every pod, and the
token already keeps out a pod that does not have it.

A probe job takes `name`, `collector`, `targets`, `params`, `labels`,
`shard_key`, `discovery` and `target_template`, the keys of the allocator's
probe targets file. The allocator decodes that file strictly, so any other
key, such as `target`, and a job with neither a target nor a discovery source
fail rendering instead of stopping the allocator at startup.

A large fleet does not have to be listed. A probe job's `discovery`, and the
`discovered_targets` of `allocator.staticTargets.data`, name `file_sd`,
`http_sd`, `kubernetes` or `dns_sd` sources. The allocator allocates the
targets they find like written ones
([Discovering targets](../../docs/ALLOCATOR.md#discovering-targets)).

```yaml
allocator:
  enabled: true
  probeJobs:
    - name: databases
      collector: pg_health
      discovery:
        - kubernetes:
            namespaces: [db]
            label_selector: app.kubernetes.io/name=postgres
            port: metrics
          labels:
            namespace: __meta_kubernetes_namespace
```

With a `kubernetes` source, the allocator runs as its own ServiceAccount,
`<fullname>-allocator`, with its token mounted, while the exporter's account
keeps following `serviceAccount.automount`. With `allocator.rbac.create`, the
default, the chart lets that account `list` EndpointSlices, which is all the
allocator does with the API. It renders a Role and a RoleBinding in each
namespace a source lists (the release's namespace for a source that lists
none), and a ClusterRole and a ClusterRoleBinding
`<namespace>-<fullname>-allocator` for a source with `all_namespaces`.
Without a `kubernetes` source none of this is rendered, and the allocator
runs as the exporter's account, with `serviceAccount.automount` deciding
whether its token is mounted.

`allocator.serviceAccount.name` runs the allocator as an existing account
instead, with or without a `kubernetes` source. The chart then creates no
`<fullname>-allocator` account:

```yaml
allocator:
  enabled: true
  serviceAccount:
    name: pue-discovery
```

With a `kubernetes` source, the allocator's pods mount that account's token,
and `allocator.rbac.create` binds the Roles and the ClusterRole above to it.
With `allocator.rbac.create: false` the chart renders none of them, and
granting the account `list` on EndpointSlices is up to you. Without a
`kubernetes` source the allocator calls no API, so `serviceAccount.automount`
decides its token as before.

With any discovery source, in a probe job or in the `discovered_targets` of
`allocator.staticTargets.data`, the allocator's pods run as peers. If each
instance queried its sources on its own timer, the instances could hold
different targets for a refresh interval, or for as long as one of them
cannot reach a source. The exporter pods heartbeat every instance and fetch
their bundle from whichever answers, so they would flip between the two
views, and so would Prometheus's service discovery answers.

To prevent that, each pod runs with
`--peers=<fullname>-allocator.<namespace>.svc:9116`, the headless Service the
exporter pods heartbeat to, and `--peers.self=$(POD_IP)`, its pod IP from the
downward API. The live pod with the lowest IP is the leader and is the only
one that queries the sources. The others serve the targets it found, which
they fetch from it every `allocator.peerCheckInterval` (2 seconds by
default). Every pod therefore serves the same bundles and the same targets.

When the leader goes away, the pod with the next IP takes over and serves the
targets it last fetched until its own sources answer. It takes over at its
next check when the leader shuts down, and after
`allocator.peerFailureThreshold` failed checks (3 by default) when the leader
stops answering. A stopping pod first keeps serving for one check interval
and one check timeout, so that its peers see it leave. The pods'
`terminationGracePeriodSeconds` is that time, rounded up, plus 15 seconds, or
30 when that is less, as it is with the default timings.
`allocator.terminationGracePeriodSeconds` sets it and must be at least that
value, and the sleep of `allocator.preStop` adds to both (see
[Shutting down](#shutting-down)).

`allocator.replicas` is 2 by default, as without discovery, and any number is
safe. The Deployment rolls out with the default strategy, one pod at a time,
under the disruption budget of `maxUnavailable: 1`. The allocator's
NetworkPolicy also admits the allocator's own pods, which check each other
and fetch the leader's targets. A network split that keeps the pods from
reaching each other gives each side a leader, and the targets can then flip
between the two until the split heals. See
[Instances with discovery](../../docs/ALLOCATOR.md#instances-with-discovery).

Every pod serves the same allocation. With
`allocator.serviceMonitor.enabled`, Prometheus therefore has the series of
the allocation once per pod, distinguished by `pod` and `instance`, and a
query takes their `max`. `allocator.leaderOnly: true` runs the peers with
`--peers.leader-only`. Only the leader's `/metrics` then has the members, the
allocation and the discovery sources. Every other pod's `/metrics` has only
the peers' metric families, including `pue_allocator_is_leader` at 0. Each
pod is still monitored, a release with no leader is visible, and the
allocation is one pod's series. When another pod becomes the leader, the
series come from it, with its `pod` and `instance`
([Leader-only metrics](../../docs/ALLOCATOR.md#leader-only-metrics)).
`leaderOnly` needs a discovery source: `true` without one fails rendering,
because the pods are then not peers and each serves everything.

A `file_sd` source reads files in the allocator's pod, and so are the
`bearer_token_file` and `tls` files of an `http_sd` source. The exporter's
`extraVolumes` and `extraVolumeMounts` mount into the exporter's pods only.
`allocator.extraVolumes` and `allocator.extraVolumeMounts`, in the same form,
mount into the allocator's pods only. For example, a ConfigMap of file_sd
files:

```yaml
allocator:
  enabled: true
  extraVolumes:
    - name: file-sd
      configMap:
        name: websites-sd
  extraVolumeMounts:
    - name: file-sd
      mountPath: /etc/pue-sd
      readOnly: true
  probeJobs:
    - name: websites
      collector: html_status
      target_template: https://{{address}}/health
      discovery:
        - file_sd:
            files: [/etc/pue-sd/*.json]
```

A ConfigMap volume is updated in place when the ConfigMap changes, and the
source re-reads its files every `refresh_interval`, so a change to them needs
no rollout. A `subPath` mount is never updated.

As for the exporter, rendering fails for a `mountPath` that the allocator's
pod already mounts, for one inside such a path (those volumes are read-only),
and for a path two entries share. The allocator's own mounts are:

- the configuration directory;
- `/etc/prometheus-universal-exporter-static-targets`, with
  `allocator.staticTargets.data`;
- `/etc/prometheus-universal-exporter-probe-targets`, with
  `allocator.probeJobs`;
- the token's directory; and
- the service account token's directory,
  `/var/run/secrets/kubernetes.io/serviceaccount`, while it is mounted (with
  a `kubernetes` source or `serviceAccount.automount`).

`allocator.staticTargets.expandEnv` renders `--static-targets.expand-env` on
the exporter pods. They then expand `${NAME}` references in their share of
`allocator.staticTargets.data` from their own environment (`env` and
`envFrom`), as `staticTargets.expandEnv` does without the allocator.
`staticTargets.expandEnv` together with the allocator fails rendering. The
allocator checks and serves the document as written, so a reference must be
somewhere the unexpanded text passes its checks: in a credential, a header, a
label or a URL's path. It cannot be in a target's scheme or host, which the
allocator refuses.

| Value | Default | Purpose |
| --- | --- | --- |
| `allocator.enabled` | `false` | Deploy the allocator and run the exporter in allocator mode. |
| `allocator.replicas` | empty: `2` | Allocator pods, from 1. They keep no state. With a discovery source they are peers, run with `--peers` and `--peers.self`: the leader alone asks the sources and the others serve what it found. |
| `allocator.image` | the exporter's | `repository`, `tag`, `pullPolicy`; each empty field takes `image`'s. |
| `allocator.resources` | 50m/64Mi, 500m/256Mi | The allocator's requests and limits. |
| `allocator.settle` | `15s` | `--settle`: how long a changed member list holds before targets move, so a pod restarting quickly keeps its share. A Go duration from zero to `1h`; anything else fails rendering. Empty leaves the flag out. It also sets the Deployment's `minReadySeconds`, `max(settle, 2m)` plus `heartbeatInterval`. |
| `allocator.heartbeatInterval` | `""`: `10s` | `--heartbeat.interval`: how often the exporter pods heartbeat, which the allocator tells them in every heartbeat answer, so the exporter pods have no setting of their own; a pod not heard from for three intervals stops being a member, and its targets move `settle` after that. A Go duration from `1s` to `2m`; anything else fails rendering. Empty leaves the flag out and the allocator's default, `10s`, in force. A shorter interval finds a pod that has stopped sooner, for more heartbeats. It also sets the Deployment's `minReadySeconds`, `max(settle, 2m)` plus the interval. |
| `allocator.configWatchInterval` | `""`: `60s` | `--config.watch-interval`: how often the allocator reads the configuration and the target ConfigMaps again for changes, which reach the exporter pods at once after; the kubelet takes up to about a minute more to update a mounted ConfigMap. A Go duration from zero to `24h`, `0s` reading them once at the start; anything else fails rendering. Empty leaves the flag out. |
| `allocator.discoveryRefreshInterval` | `""`: each kind's own | `--discovery.refresh-interval`: how often the allocator asks a discovery source that sets no `refresh_interval`, whatever its kind. A Go duration from `1s` to `24h`; anything else fails rendering. Empty leaves the flag out and each kind its own default: `1m` for `file_sd` and `http_sd`, `30s` for `kubernetes` and `dns_sd`. [How fast changes reach the exporters](../../docs/ALLOCATOR.md#how-fast-changes-reach-the-exporters) lists every delay and its setting, Prometheus's `allocator.scrapeConfig.refreshInterval` among them. |
| `allocator.balanceFactor` | unset | `--allocation.balance-factor`: how far above its fair share of the targets' weight a member's load may go before a target its hash gives it goes to the next member; 1 holds every member to its fair share, 10 lets hashing place everything for up to 10 members. A number from `1` to `10` with at most three decimals, or rendering fails. Empty leaves the flag out and the allocator's default, `1.25`, in force. |
| `allocator.maxMembers` | `1000` | `--members.max`: how many live members the allocator takes; the heartbeat of a new pod beyond them is refused until one stops heartbeating. Keep it above the most exporter pods the autoscaler runs. |
| `allocator.logLevel` | `info` | `debug`, `info`, `warn` or `error`. |
| `allocator.leaderOnly` | `false` | With a discovery source, where the pods are peers: `--peers.leader-only`, only the leader serves the whole of `/metrics` and of the status page, and the other pods the peers' metrics alone, so the ServiceMonitor has one pod's series of the allocation. `true` without a discovery source fails rendering. |
| `allocator.peerCheckInterval` | `""`: `2s` | With a discovery source: `--peers.check-interval`, how often each pod resolves the headless Service, checks the other pods and, as a follower, fetches the leader's snapshot, so how soon a follower serves what the leader found. A Go duration from `500ms` to `1m`. |
| `allocator.peerCheckTimeout` | `""`: `1s` | With a discovery source: `--peers.check-timeout`, how long a check of another pod may take, from `100ms` to less than `peerCheckInterval`. |
| `allocator.peerFailureThreshold` | `null`: `3` | With a discovery source: `--peers.failure-threshold`, the failed checks in a row, from 1 to 20, after which a pod stops being live: a leader that stops answering is replaced within that many check intervals and check timeouts, 9 seconds with the defaults; one that shuts down cleanly at the next check. |
| `allocator.peerSnapshotTimeout` | `""`: `30s` | With a discovery source: `--peers.snapshot-timeout`, how long a follower's fetch of the leader's snapshot may take, from `peerCheckTimeout` to `5m`. |
| `allocator.tokenSecret.name` / `key` | `""` / `token` | The Secret holding the shared bearer token, which the allocator always requires, the exporter pods send and the ScrapeConfigs present; empty, the chart generates one (see [The allocator's token](#the-allocators-token)). |
| `allocator.staticTargets.data` | `""` | The static target document, as `staticTargets.data`; a target may add `shard_key`. |
| `allocator.staticTargets.expandEnv` | `false` | `--static-targets.expand-env` on the exporter pods, which expand `${NAME}` references in their share of the document. |
| `allocator.probeJobs` | `[]` | The probe jobs: `name`, a DNS-1123 label that also names the job's ScrapeConfig `<fullname>-<name>`, `collector`, one the configuration defines, `targets`, at least one unless `discovery` finds them, and optionally `params`, `labels`, `shard_key` (`target` or `address`), `discovery` and `target_template`; no other key. |
| `allocator.rbac.create` | `true` | With a `kubernetes` discovery source, let the allocator's ServiceAccount list EndpointSlices where the sources list them. |
| `allocator.serviceAccount.name` | `""` | An existing ServiceAccount the allocator's pods run as, with or without a `kubernetes` source; the chart then creates none. Empty, `<fullname>-allocator` with a `kubernetes` source, the exporter's account without one. |
| `allocator.extraVolumes` / `extraVolumeMounts` | `[]` | Volumes and mounts of the allocator's pods only, as `extraVolumes` and `extraVolumeMounts` are the exporter's: a `file_sd` source's files, an `http_sd` source's token and TLS files. |
| `allocator.networkPolicy.enabled` | `true` | Admit ingress to the allocator only from the exporter pods and from `prometheus`, and with a discovery source from the allocator's own pods. |
| `allocator.networkPolicy.prometheus` | Prometheus Operator pods | NetworkPolicy peers that fetch the probe targets: by default pods labelled `app.kubernetes.io/name: prometheus` in any namespace. Set your Prometheus's labels and namespace; a Prometheus Agent is `app.kubernetes.io/name: prometheus-agent`. |
| `allocator.pdb.enabled` | `true` | A PodDisruptionBudget of `maxUnavailable: 1` for the allocator. |
| `allocator.podLabels` / `podAnnotations` | `{}` | Labels and annotations of the allocator's pods only; a label the chart sets fails rendering, and `checksum/config` stays the chart's. |
| `allocator.terminationGracePeriodSeconds` | `null` | The allocator pods' grace period, at least `allocator.preStop`'s sleep, a peer check interval and timeout with discovery, and 15 seconds; see [Shutting down](#shutting-down). |
| `allocator.preStop` | `null` | The allocator container's preStop hook, a Kubernetes lifecycle handler: an exec, an HTTP GET or a sleep; see [Shutting down](#shutting-down). |
| `allocator.priorityClassName` | `""` | The allocator pods' PriorityClass. |
| `allocator.nodeSelector` / `tolerations` / `affinity` | empty | Scheduling of the allocator's pods, independent of the exporter's. |
| `allocator.topologySpreadConstraints` | `kubernetes.io/hostname`, `ScheduleAnyway` | The allocator pods' topology spread constraints; one without a `labelSelector` spreads the allocator's pods. `[]` renders none. |
| `allocator.serviceMonitor.enabled` | `false` | A ServiceMonitor of the allocator's `/metrics`. |
| `allocator.serviceMonitor.interval` / `scrapeTimeout` | `""` / `""` | Prometheus durations; empty leaves the Prometheus Operator's defaults. A `scrapeTimeout` longer than `interval` fails rendering. |
| `allocator.serviceMonitor.labels` | `{}` | Added to the ServiceMonitor, for a Prometheus `serviceMonitorSelector`. |
| `allocator.scrapeConfig.enabled` | `true` | Render a ScrapeConfig per probe job. |
| `allocator.scrapeConfig.refreshInterval` / `interval` / `scrapeTimeout` | `30s` / `""` / `""` | How often Prometheus reads the targets, and the probes' interval and timeout; empty leaves Prometheus's defaults. A `scrapeTimeout` longer than `interval` fails rendering. |
| `allocator.scrapeConfig.labels` | `{}` | Added to each ScrapeConfig. |

The chart refuses the following while rendering:

- `allocator.enabled` together with `staticTargets.enabled`. Move the
  document to `allocator.staticTargets.data`.
- `allocator.enabled` together with an enabled `monitors` entry, because
  probe targets then come from `allocator.probeJobs`. Move its targets to a
  probe job.
- `staticTargets.monitor.targets` with the allocator, because each pod holds
  only its share.
- `staticTargets.expandEnv` with the allocator. Set
  `allocator.staticTargets.expandEnv` instead.
- A probe job whose collector the configuration does not define, that lists
  no target, or that has a key the allocator does not take.
- An `env` entry named `POD_NAME` or `POD_IP`, which the chart sets.

`--allocator.advertise` is `$(POD_IP):<port>`, which suits a cluster whose
pods have IPv4 addresses. With `networkPolicy.egress` rules, the exporter's
NetworkPolicy also lets its pods reach the allocator.

### The allocator's status page

Each allocator pod serves a status page at `/` on port 9116. It shows the
instance, its peers and which pod leads, the members, how the targets are
distributed among them, and the discovery sources
([Status page](../../docs/ALLOCATOR.md#status-page)). The chart exposes the
port inside the cluster only. To reach a pod's page, use `kubectl
port-forward`, which the allocator's NetworkPolicy does not block, and open
the forwarded port, 9116 on your machine, in a browser:

```sh
kubectl port-forward svc/<fullname>-allocator 9116
```

That reaches one of the pods. The browser asks for a user name and a
password. Enter any user name, and the allocator's token (or the token of
your `allocator.tokenSecret`) as the password:

```sh
kubectl get secret <fullname>-allocator-token -o jsonpath='{.data.token}' | base64 -d
```

With a discovery source the pods are peers. Every pod's page names the leader
by its pod IP, as `pue_allocator_leader{peer}` does on every pod's
`/metrics`. The pod with that IP is the leader, and its page is the one to
read. With `allocator.leaderOnly`, the other pods' pages show only the pod
and its peers:

```sh
kubectl get pod -l app.kubernetes.io/name=<name>-allocator --field-selector status.podIP=<leader IP> -o name
kubectl port-forward pod/<that pod> 9116
```

A member's view links to that exporter's own status page and to the series of
each of its static targets, at the pod IP the member advertises. A
port-forwarded browser cannot reach that address. Forward the exporter's pod
instead and open the same path, `/static-targets/status` by default, on the
forwarded port, which is the port of `server.listenAddress`, 8080 by default:

```sh
kubectl port-forward pod/<member> 8080
```

## Using the chart as a dependency

The chart can be a dependency of your own chart. Its values then go under its name in the parent's values, and a `condition` turns it on and off:

```yaml
# Chart.yaml of the parent chart
dependencies:
  - name: prometheus-universal-exporter
    version: 2.2.0
    repository: oci://ghcr.io/eenchev/charts
    condition: prometheus-universal-exporter.enabled
```

```yaml
# values.yaml of the parent chart
global:
  team: observability
prometheus-universal-exporter:
  enabled: true
  replicaCount: 2
```

Helm passes a dependency the parent's `global` values and the `enabled` value its condition reads, along with its own values. The values schema accepts those two keys and the chart reads neither. Any other key the chart does not know still fails rendering.

## Values

Every value has a default, and `values.yaml` documents each one in place. Helm checks `values.schema.json` on install, upgrade and `helm template`, so a misspelled key or a wrong type fails there instead of producing a pod that starts and behaves unexpectedly.

**Null blocks.** Helm reads a null as "remove this key and the chart's defaults under it". A block left empty in a values file, such as `ingress:` with every key under it commented out, is a null. For a feature's own block (`allocator`, `webAuth`, `targetAuth`, `sshAuth`, `sqlAuth`, `kubernetesAccess`, `healthProbes`, `goGC`), a null means the feature is off or at its default. Any other block whose keys the chart reads fails rendering with a message that names it: `ingress is null, which removes every default the chart has under it: …`. Delete the key to keep its defaults, or write the keys you mean under it.

**Whole numbers.** A whole number is rendered as a whole number however it is written: `replicaCount: 2000000`, `2e6` and `2000000.0` in a values file all render `replicas: 2000000`, never `2e+06`. The whole numbers the chart renders itself have a range. A fraction or a number outside the range fails rendering; the schema refuses it and, when the schema is skipped, so do the templates. The ranges are:

- 0 to 2147483647 for `replicaCount`, `terminationGracePeriodSeconds`, `allocator.terminationGracePeriodSeconds`, a `preStop` hook's `sleep.seconds`, `server.probeMaxConcurrent`, `server.pythonMaxWorkers`, `server.pythonMaxIdleWorkers` and a `podDisruptionBudget` count;
- 1 to 2147483647 for the replicas and utilization targets of `autoscaling`;
- 1 to 65535 for `service.port`.

A `podDisruptionBudget` count given as a string is rendered without quotes, so it must be what Kubernetes accepts on a budget, and otherwise fails rendering with a message that names the key. It is a whole number from 0 to 2147483647 or a percentage from `0%` to `100%`, written with no leading zero, because `"010"` would render as YAML's octal number, eight.

The probes' timings and the `strategy.rollingUpdate` counts are passed through as written. The values schema bounds them as Kubernetes does, so `periodSeconds: 1e30` fails at `helm template` instead of when Kubernetes refuses the Deployment:

- Each timing is a whole number up to 2147483647, from 0 for `initialDelaySeconds` and from 1 for the others. A liveness probe's `successThreshold` is 1, the only value Kubernetes accepts there.
- `maxSurge` and `maxUnavailable` are a whole number from 0 to 2147483647 or a percentage written with no leading zero, up to `100%` for `maxUnavailable`. They are never a string of digits alone, which would reach Kubernetes as a string it refuses.

Whole numbers inside the other Kubernetes structures the chart passes through, such as a security context's `runAsUser` or `fsGroup` (for example OpenShift's `1000680000`), `resources`, tolerations and relabelings, are also written out in digits. Kubernetes bounds those.

| Value | Type | Default | What it sets |
| --- | --- | --- | --- |
| `replicaCount` | integer | `1` | Deployment replicas. |
| `image.repository` / `image.tag` / `image.pullPolicy` | string | GHCR, `""`, `IfNotPresent` | The exporter image. An empty `tag` deploys the chart's `appVersion`, the exporter release the chart version was validated against; set it to run another. |
| `imagePullSecrets` | array | `[]` | Secrets for a private registry. |
| `nameOverride` / `fullnameOverride` / `namespaceOverride` | string | `""` | Naming and namespace of the created objects. Objects are named `<release>-prometheus-universal-exporter`, or after the release alone when its name holds the chart's, so two releases in one namespace do not collide; `fullnameOverride` names them outright. |
| `defaultLabels` / `defaultAnnotations` | map | `{}` | Metadata applied to every object the chart creates. |
| `serviceAccount` | object | created | `create`, `automount`, `name`, `annotations`; `name` is required, and `annotations` refused, with `create: false`. See [Service account](#service-account). |
| `kubernetesAccess` | object | disabled | The exporter's own access to the Kubernetes API, for `kubernetes` collectors: `enabled`, `rules` (`apiGroups` and `resources` the ServiceAccount may `list`), `namespaces` (the release's when empty) or `clusterWide`, `kubelet` (`stats` or `proxy`, `get` on `nodes/stats` or `nodes/proxy`, with `clusterWide`), `extraRules` (rules of your own, with their verbs) and `allowBroadExtraRules`; see [Kubernetes collectors](#kubernetes-collectors). |
| `service` | object | enabled, ClusterIP, 8080 | The exporter Service: `enabled`, `type` (`ClusterIP`, `NodePort` or `LoadBalancer`, the last two with `webAuth` or `exposure.allowWithoutWebAuth`), `port`, `sessionAffinity`, `annotations`. |
| `neg` | object | disabled | GKE Network Endpoint Group annotations on the Service. |
| `ingress` | object | disabled | Class, hosts, paths, TLS and annotations. Needs `webAuth` or `exposure.allowWithoutWebAuth`; see [Exposing the exporter](#exposing-the-exporter). |
| `exposure.allowWithoutWebAuth` | boolean | `false` | Lets `ingress.enabled`, and `service.type` `NodePort` or `LoadBalancer`, render without `webAuth`, for callers authenticated where the chart cannot see it; see [Exposing the exporter](#exposing-the-exporter). |
| `server` | object | see [Exporter flags](#exporter-flags) | Exporter flags: `listenAddress`, `pythonPath`, `logLevel`, `probeTimeoutOffset`, `probeDefaultTimeout`, `probeMaxConcurrent`, `pythonMaxWorkers`, `pythonMaxIdleWorkers`, `pythonIdleTimeout`, `shutdownTimeout`, `shutdownDelay`, `enableLifecycle`, `debug`, `gzipLevel`, `statusRefresh`, `allocatorCapacity`, `watchConfig`, `watchConfigInterval`, `expandEnv`. |
| `terminationGracePeriodSeconds` | integer | unset | The pod's grace period; see [Shutting down](#shutting-down). |
| `preStop` | object | unset | The exporter container's preStop hook, a Kubernetes lifecycle handler: an exec, an HTTP GET or a sleep; see [Shutting down](#shutting-down). |
| `env` / `envFrom` | array | `[]` | Container environment, in the Kubernetes shapes. |
| `extraArgs` | array | `[]` | Extra command-line flags. |
| `extraVolumes` / `extraVolumeMounts` | array | `[]` | Volumes and mounts beyond the chart's own. |
| `config` | object | enabled | `enabled`, and `data` holding `config.yaml` and any [collector files](#collector-files), each rendered as it is given. |
| `staticTargets` | object | disabled | Static targets rendered into a ConfigMap of their own. |
| `allocator` | object | disabled | The target allocator and the exporter in allocator mode: `enabled`, `replicas`, `image`, `resources`, `settle`, `heartbeatInterval`, `configWatchInterval`, `discoveryRefreshInterval`, `maxMembers`, `logLevel`, `leaderOnly`, `peerCheckInterval`, `peerCheckTimeout`, `peerFailureThreshold`, `peerSnapshotTimeout`, `tokenSecret`, `staticTargets`, `probeJobs`, `extraVolumes`, `extraVolumeMounts`, `networkPolicy`, `pdb`, `podLabels`, `podAnnotations`, `priorityClassName`, `nodeSelector`, `tolerations`, `affinity`, `topologySpreadConstraints`, `serviceMonitor`, `scrapeConfig`, `rbac`, `serviceAccount`; see [Target allocator](#target-allocator). |
| `monitors` | array | `[]` | `ServiceMonitor` and `PodMonitor` resources, one per entry: `name` a unique DNS-1123 label other than `self` and `static-targets`, `interval` and `scrapeTimeout` Prometheus durations with the timeout no longer than the interval, `port` (`http` by default), a port's name and never its number, and `namespaceSelector` for where the targets are, `params` without `collector` or `target`, `auth` with a `type` and `secretName` when enabled; see [Configure Prometheus](#4-configure-prometheus) and [Monitor authentication](#monitor-authentication). |
| `probes` | array | `[]` | Prometheus Operator `Probe` resources, one per entry, whose prober is the exporter's Service at `/probe`: `name` a unique DNS-1123 label, `collector` (required, and one `config.data` defines), `targets` (URLs) or `ingress` (`selector`, `namespaceSelector`) but not both, `targetLabels` (with `targets` only), `interval` and `scrapeTimeout`, `jobName`, `labels`, `annotations`, `relabelings`, `metricRelabelings`, `params` without `collector` or `target`, and `auth` as a monitor's; see [Uptime probes](#uptime-probes). |
| `selfMetrics` | object | enabled | The monitor for the exporter's own endpoint, `type` `service` or `pod`, and its path; see [Configure Prometheus](#4-configure-prometheus). |
| `resources` | object | 100m/128Mi, 500m/512Mi | Requests and limits. |
| `goMemLimit` | object | enabled, `0.8` | `--runtime.memory-limit-ratio`: the Go memory limit as a share of the container's; see [Resources](#resources). |
| `goGC` | object | unset | `percent`, the Go garbage collector's target, rendered as the container's `GOGC` environment variable: a whole number from 1 to 10000, or `"off"` with a Go memory limit in force; empty keeps Go's default, 100. See [Garbage collector](#garbage-collector). |
| `healthProbes` | object | `mode: http` | `mode`: `http` GETs `/health` and `/ready` on the `http` port, `exec` runs the exporter's `--healthcheck` in the container; see [Probes](#probes). |
| `livenessProbe` / `readinessProbe` | object | see [Probes](#probes) | The probes' timings; `terminationGracePeriodSeconds` on the liveness probe only. |
| `autoscaling` | object | disabled | `enabled`, `minReplicas`, `maxReplicas`, `targetCPUUtilizationPercentage`, `targetMemoryUtilizationPercentage`, `targetBusy`, `busyMetric`, `targetStaticTargetsPerPod`, `staticTargetsMetric`, `metrics`, `behavior`; see [Autoscaling](#autoscaling). |
| `podDisruptionBudget` | object | disabled | `enabled`, `minAvailable` or `maxUnavailable`, `unhealthyPodEvictionPolicy`; see [Replicas](#replicas). |
| `strategy` | object | RollingUpdate | Deployment strategy and its `rollingUpdate` settings, which are rendered with `RollingUpdate` only; see [Replicas](#replicas). |
| `podSecurityContext` / `securityContext` | object | hardened | Pod and container security context; the pod runs as user, group and `fsGroup` 65532. |
| `podLabels` / `podAnnotations` | map | `{}` | Labels and annotations of the pods only, over `defaultLabels` and `defaultAnnotations`. A label the chart sets itself, such as `app.kubernetes.io/name`, fails rendering; `checksum/config` stays the chart's. |
| `priorityClassName` | string | `""` | The pods' PriorityClass. |
| `nodeSelector` / `tolerations` / `affinity` | map/array/object | empty | Scheduling. |
| `topologySpreadConstraints` | array | `[]` | Pod topology spread constraints; one without a `labelSelector` spreads this release's pods. See [Replicas](#replicas). |
| `networkPolicy` | object | disabled | `ingress` and `egress` rules, and `allowDNS` with egress rules; see [NetworkPolicy](#networkpolicy). |
| `targetAuth` | object | disabled | Secret-backed credentials mounted for the exporter to send to the target. |
| `webAuth` | object | disabled | A Secret's username and password mounted as files for the exporter's own Basic Auth, and presented by every monitor without `auth` of its own; see [Exporter authentication](#exporter-authentication). |
| `sshAuth.enabled` | boolean | `false` | Mount SSH credentials and known hosts for `sshfile` collectors; see [SSH file collectors](#ssh-file-collectors). |
| `sshAuth.secretName` | string | `""` | The Secret holding the SSH credentials; required when enabled. |
| `sshAuth.privateKeyKey` | string | `""` | The Secret's key mounted as `private_key`. This or `passwordKey`, or both, when enabled. |
| `sshAuth.passphraseKey` | string | `""` | The Secret's key mounted as `passphrase`, the private key's passphrase; only with `privateKeyKey`. |
| `sshAuth.passwordKey` | string | `""` | The Secret's key mounted as `password`. |
| `sshAuth.knownHosts` | string | `""` | `known_hosts` lines, such as `ssh-keyscan` prints, rendered into a ConfigMap and mounted as `known_hosts`; at least one entry. Not with `knownHostsSecretKey`. |
| `sshAuth.knownHostsSecretKey` | string | `""` | The Secret's key mounted as `known_hosts`. Not with `knownHosts`; one of the two when enabled. |
| `sshAuth.mountPath` | string | `/var/run/prometheus-universal-exporter/ssh` | Where the files are mounted, read-only, mode `0400`; a path of its own. |
| `sqlAuth.enabled` | boolean | `false` | Mount database credentials and TLS files for `sql` collectors; see [SQL collectors](#sql-collectors). |
| `sqlAuth.secretName` | string | `""` | The Secret holding the database credentials; required when enabled. |
| `sqlAuth.passwordKey` | string | `""` | The Secret's key mounted as `password`. |
| `sqlAuth.privateKeyKey` | string | `""` | The Secret's key mounted as `private_key`, an unencrypted key for Snowflake key-pair authentication. |
| `sqlAuth.caKey` | string | `""` | The Secret's key mounted as `ca.crt`. |
| `sqlAuth.certKey` | string | `""` | The Secret's key mounted as `client.crt`; only with `keyKey`. |
| `sqlAuth.keyKey` | string | `""` | The Secret's key mounted as `client.key`; only with `certKey`. At least one of the keys when enabled. |
| `sqlAuth.mountPath` | string | `/var/run/prometheus-universal-exporter/sql` | Where the files are mounted, read-only, mode `0400`; a path of its own. |

## Other options

The chart also supports:

* Multiple `ServiceMonitor` and `PodMonitor` resources
* Prometheus relabeling and metric relabeling
* Monitor authentication
* Custom headers
* Custom labels and annotations
* Pod affinity, tolerations, topology spread and priority
* RollingUpdate or Recreate deployment strategies
* GKE Network Endpoint Groups
* Custom exporter listen address
* Custom Python interpreter
* Verbose self-metrics
* Resource metrics for the exporter's own CPU and memory
* Collector caching
* Extra volumes and volume mounts
* Extra command-line flags

See `values.yaml` for all available Helm options.

The requirements this chart is built to are in
[../../docs/SPECIFICATION-CHART.md](../../docs/SPECIFICATION-CHART.md).

## Security

The chart runs the exporter with security-focused defaults:

* Non-root container, as user and group 65532, set by number in the image and in `podSecurityContext` so `runAsNonRoot` can verify it
* Read-only root filesystem
* All Linux capabilities dropped
* Privilege escalation disabled
* No Ingress, `NodePort` or `LoadBalancer` Service without the exporter's own authentication (`webAuth`), unless `exposure.allowWithoutWebAuth` states that callers are authenticated elsewhere; see [Exposing the exporter](#exposing-the-exporter)
* No Kubernetes API permissions and no mounted service account token, whichever account the pod runs as. The exception is `kubernetesAccess`, which grants `kubernetes` collectors `list` on the resources its rules name, never on the core group's Secrets, and, with `kubernetesAccess.kubelet`, `get` on `nodes/stats` or `nodes/proxy`

## Uninstall

```bash
helm uninstall prometheus-universal-exporter \
  --namespace monitoring
```
