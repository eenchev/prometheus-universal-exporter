# Prometheus Universal Exporter — Helm Chart

Helm chart for deploying the Prometheus Universal Exporter to Kubernetes.

The exporter turns an HTTP endpoint that was never meant for Prometheus into a Prometheus target. Point it at a service that answers with JSON, YAML, XML, CSV, HTML, plain text or Prometheus exposition; a collector in the configuration says how to call it, how to read the response and which metrics to publish. Targets are never written into the configuration — Prometheus discovers the real service and passes it to the exporter's `/probe` endpoint, so one deployment serves many services and many response shapes.

The chart creates the exporter Deployment, Service, ServiceAccount, and ConfigMap. By default, the exporter configuration is managed directly through Helm values.

## Features

* A Prometheus `/probe` endpoint that takes the discovered `target` and a named `collector`
* Collectors that configure the target request: method, path, headers, request body, timeout, retries and TLS
* Decoders for JSON, YAML, XML, CSV, HTML, plain text and Prometheus exposition, with auto-detection
* Transforms with jq, yq, XPath, CSS selectors, regex, Prometheus filtering and Python
* Python pre-scripts and full Python transforms, with a bundled third-party library set and no runtime installs
* Declared metrics with types, descriptions, explicit labels and per-metric error policy
* Per-scrape overrides through `/probe` parameters, which monitors render as `params`
* `ServiceMonitor` and `PodMonitor` resources, including a separate monitor for the exporter's own metrics
* Exporter Basic Auth, Secret-backed monitor authentication, and Secret-backed target credentials
* Scheduled targets the exporter scrapes itself and delivers over OTLP
* Response caching per collector
* Configuration watching and in-place reload, and `${NAME}` expansion from the environment
* Self-metrics, optionally including per-request series and the standard `go_` and `process_` series

## Prerequisites

* Kubernetes cluster
* Helm 3
* Prometheus Operator if you want to use `ServiceMonitor` or `PodMonitor`

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

The chart automatically creates a ConfigMap containing this configuration and mounts it into the exporter.

Every collector key works here as it does in a standalone configuration file.
For example, `metrics_prefix: example` on the collector above would export
`example_example_status`; a prefix is joined with `_` to every metric the
collector exports, and is validated when the exporter starts, so a bad one fails
the rollout instead of producing oddly named series.

See the exporter documentation for the full collector configuration format, and
[Prefixing a collector's metrics](https://github.com/eenchev/prometheus-universal-exporter/blob/main/docs/CONFIGURATION.md#prefixing-a-collectors-metrics)
for the prefix rules.

### 2. Install the exporter

The chart is published to GitHub Container Registry as an OCI artifact:

```bash
helm install prometheus-universal-exporter \
  oci://ghcr.io/eenchev/charts/prometheus-universal-exporter \
  --version 0.2.1 \
  --namespace monitoring \
  --create-namespace \
  -f values.yaml
```

Omitting `--version` installs the newest published chart:

```bash
helm install prometheus-universal-exporter \
  oci://ghcr.io/eenchev/charts/prometheus-universal-exporter \
  --namespace monitoring \
  --create-namespace \
  -f values.yaml
```

Pin the version in anything you deploy more than once. An unpinned install takes whatever is newest at the moment it runs, so the same command run twice can produce two different releases — which is the one thing you do not want to discover while rolling back.

No registry login is needed: the package is public.

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
  --version 0.2.1 \
  --namespace monitoring \
  -f values.yaml
```

Configuration changes automatically roll the exporter Deployment.

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

The chart creates the required monitor and routes Prometheus requests through the exporter.

> Keep `service.enabled: true` when using chart-generated `ServiceMonitor` or `PodMonitor` resources.

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
* retry settings
* `param_<name>`, which fills a `{{param_<name>}}` placeholder in the collector's `request.path`

For example, a collector with `path: /api/{{param_tenant}}/status` scraped by a
monitor with `params: {param_tenant: [acme]}` requests `/api/acme/status`. A
placeholder may have a default after a colon, `{{param_tenant:acme}}`; one with
no default that the monitor does not supply fails the scrape with `400`. See
[Target requests](../../docs/REQUESTS.md#path-parameters).

## Authentication

### Target authentication

If the target requires authentication, use a Kubernetes Secret rather than putting credentials directly in the configuration.

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

### Exporter authentication

To protect the exporter's `/probe` and self-metrics endpoints:

```yaml
config:
  data:
    config.yaml: |
      web:
        basic_auth:
          enabled: true
          username: exporter
          password: change-me
```

A password written there ends up in the chart's ConfigMap. To keep it in a Secret instead, mount the Secret with `webAuth` and point the configuration at the files. The same Secret can give the monitors their credential:

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
monitors:
  - name: targets
    enabled: true
    type: service
    collector: example
    auth:
      enabled: true
      type: basic
      secretName: exporter-auth
```

The self-metrics monitor uses the `webAuth` Secret by itself, since `web.basic_auth` protects the self-metrics endpoint too; the probing monitors take it through their `auth`, as above, whose keys default to `username` and `password`. `webAuth.usernameKey` and `webAuth.passwordKey` name the Secret's keys, `username` and `password` by default; the files are always named `username` and `password` under `webAuth.mountPath`. The exporter reads a file again when it changes, so a rotated Secret takes effect without a restart. An `extraVolumeMounts` entry at `webAuth.mountPath` is rejected while rendering.

The Kubernetes `/health` and `/ready` endpoints remain available for health checks. The readiness probe uses `/ready`, which reports the pod not ready while a reload of its configuration is rejected, and, with `otlp.unready_after_failures` set, while its OTLP exports keep failing; see [Readiness](../../docs/CONFIGURATION.md#readiness).

## Common configuration

### Exporter flags

The exporter's own flags are chart values rather than something to assemble by hand. Every flag a serving exporter takes has one:

| Value | Flag | Default |
| --- | --- | --- |
| `server.listenAddress` | `--web.listen-address` | `:8080` |
| `selfMetrics.path` | `--web.self-metrics-path` | `/self-metrics` |
| `server.pythonPath` | `--python.path` | `/usr/local/bin/python3` |
| `server.logLevel` | `--log.level` | `info` |
| `server.probeTimeoutOffset` | `--probe.timeout-offset` | unset: the exporter's `500ms` |
| `server.probeDefaultTimeout` | `--probe.default-timeout` | unset: the exporter's `30s` |
| `server.shutdownTimeout` | `--web.shutdown-timeout` | unset: the exporter's `5s` |
| `server.shutdownDelay` | `--web.shutdown-delay` | `5s` |
| `server.watchConfig` / `server.watchConfigInterval` | `--config.watch` / `--config.watch-interval` | off / `60s` |
| `server.expandEnv` | `--config.export-env` | off |
| `server.enableLifecycle` | `--web.enable-lifecycle` | off |
| `otlpTargets.enabled` | `--otlp.targets-file` | off |
| `config` | `--config.file` | the chart's ConfigMap |

`server.listenAddress` sets the container port too, so the listener and the probes cannot drift apart. `server.pythonPath` is the interpreter used by the `python` transform; its default is where the exporter image's `python:3.12-slim` base installs Python, and it is worth overriding only for a custom image. `server.logLevel` is one of `debug`, `info`, `warn` and `error`, and anything else fails rendering.

`server.probeTimeoutOffset` is how much of Prometheus's scrape timeout — a monitor's `scrapeTimeout` — a probe leaves unused, so a slow target or a hung file read is answered with the exporter's own error before Prometheus gives up (see [Probe deadlines](../../docs/CONFIGURATION.md#probe-deadlines)). It takes a Go duration of zero or more. Left empty, the flag is not rendered at all, so the exporter's default applies and an image older than the flag still starts; set it only with an image that has it.

`server.probeDefaultTimeout` bounds a probe that names no deadline — no scrape timeout header and no `timeout` parameter, as from curl, a script or the exporter's collectors page with JavaScript off. Prometheus always sends a scrape timeout, so its scrapes are unaffected. It takes a Go duration of zero or more, `0` leaving such a probe unbounded, and like `probeTimeoutOffset` it is rendered only when set.

```sh
helm install exporter charts/prometheus-universal-exporter \
  --set server.listenAddress=0.0.0.0:9115 \
  --set server.pythonPath=/usr/bin/python3.11 \
  --set server.logLevel=debug \
  --set server.probeTimeoutOffset=1s
```

### Shutting down

`server.shutdownTimeout` is how long a stopping pod waits for the probes in progress before closing them, rendered as `--web.shutdown-timeout` (see [Shutting down](../../docs/CONFIGURATION.md#shutting-down)). Keep it at least as long as the monitors' `scrapeTimeout`, or a rollout cuts probes off and Prometheus records failed scrapes. It takes whole hours, minutes and seconds — `30s`, `1m30s` — so the chart can work out the grace period, and like `probeTimeoutOffset` it is rendered only when set.

`server.shutdownDelay`, `5s` by default, is how long a stopping pod keeps answering probes before that, with `/ready` answering `503`, rendered as `--web.shutdown-delay`. Kubernetes takes a few seconds to take a terminating pod out of its Service, and probes sent to it in that gap would otherwise be refused, so Prometheus would record failed scrapes during every rollout. Raise it on a large cluster where endpoint updates are slow; `0s` turns it off, and empty leaves the flag out for an image older than it.

Kubernetes kills a pod `terminationGracePeriodSeconds` after asking it to stop, 30 seconds unless set. A stopping exporter needs its shutdown delay, its shutdown timeout and about 10 seconds more, for the last OTLP export and exiting — 20 seconds with the defaults. Left unset, `terminationGracePeriodSeconds` is rendered as that sum whenever it is more than 30; set, it must be at least that, or rendering fails:

```sh
helm install exporter charts/prometheus-universal-exporter \
  --set server.shutdownTimeout=1m   # renders terminationGracePeriodSeconds: 75
```

Raise `terminationGracePeriodSeconds` further if `otlp.timeout` is longer than its default of 5 seconds.

`server.enableLifecycle` enables `POST /-/reload`, which reloads the configuration at once and answers `200` when it was accepted or `500` with the reason when it was not — see [Reloading on demand](../../docs/CONFIGURATION.md#reloading-on-demand). A chart-managed ConfigMap does not need it, since a change rolls the Deployment; it is for a ConfigMap updated in place with `config.enabled: false`. Like `probeTimeoutOffset`, the flag is only rendered when set, so older images still start.

The one-shot flags — `--dry-run`, `--config.schema`, `--config.collector-file-schema`, `--version` — print something and exit, so they have no values: run them as a separate command. A flag an exporter image has that this chart version does not know yet goes in `extraArgs`, described under [Extra volumes and arguments](#extra-volumes-and-arguments).

### Configuration mount and rollout

The ConfigMap is mounted at `/etc/prometheus-universal-exporter/config.yaml`, and its checksum is part of the Deployment pod template — so a configuration change rolls the Deployment rather than waiting for the kubelet to refresh a mounted file. Set `server.watchConfig` instead when the pods should reload in place.

### Collector files

Every key of `config.data` becomes a file in the mounted configuration directory, next to `config.yaml`. That is where [collector files](../../docs/CONFIGURATION.md#collector-files) go: add each as a key and list them under `collector_files`, relative to `config.yaml`:

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

A collector file holds `collectors` and nothing else, and a collector name must be unique across `config.yaml` and every file, or the pod refuses to start. Name the keys so a pattern matches them and nothing else: `*.yaml` would also match the scheduled target file the chart puts in the same directory. A file changes the ConfigMap checksum like `config.yaml` does, so it rolls the Deployment, or, with `server.watchConfig`, is reloaded in place. Collector files kept in a ConfigMap of their own can be mounted with `extraVolumes` and `extraVolumeMounts` at their own path, such as `/etc/collectors`, and listed by absolute path: `/etc/collectors/*.yaml`.

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

### Replicas

```yaml
replicaCount: 2
```

### Service

The exporter Service is enabled by default:

```yaml
service:
  enabled: true
  type: ClusterIP
  port: 8080
```

### Ingress

```yaml
ingress:
  enabled: true
  className: nginx
  hosts:
    - host: exporter.example.com
      paths:
        - path: /
          pathType: Prefix
```

### NetworkPolicy

Enable and configure a NetworkPolicy when you need to restrict which workloads can access the exporter or which targets it can reach:

```yaml
networkPolicy:
  enabled: true
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

When using an external ConfigMap, you can enable configuration watching:

```yaml
server:
  watchConfig: true
  watchConfigInterval: 60s
```

This allows the exporter to reload the configuration when the mounted ConfigMap changes without requiring a Deployment rollout.

### Environment variables in the configuration

`server.expandEnv` adds `--config.export-env`, which substitutes `${NAME}` references in the configuration and in the scheduled target document from the container's environment before either is parsed. That lets a hostname, tenant or token come from a Secret rather than from the ConfigMap the chart renders:

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

`env` and `envFrom` take the ordinary Kubernetes shapes and are useful on their own; `expandEnv` is inert without them. Expansion is off by default: only `${NAME}` is substituted and never `$NAME`, but a configuration carrying regexes, jq expressions or Python pre-scripts has dollar signs that are not references, so expanding should be a decision rather than a surprise. A reference whose variable is not set stops the exporter at startup with the variable named, rather than becoming an empty string — a missing Secret key is then a clear failure instead of a collector quietly scraping the wrong thing.

Behind an egress proxy, set `HTTPS_PROXY`, `HTTP_PROXY` and `NO_PROXY` the same way. Target requests and OTLP exports use them, and nothing in the configuration does; see [Proxies](../../docs/REQUESTS.md#proxies). Keep the cluster's own names in `NO_PROXY` so in-cluster targets and the OpenTelemetry Collector are reached directly:

```yaml
env:
  - name: HTTPS_PROXY
    value: http://proxy.corp.example:3128
  - name: NO_PROXY
    value: .svc,.cluster.local,10.0.0.0/8
```

### Exporter resource metrics

Set `web.self_metrics.resource_metrics_enabled: true` inside `config.data.config.yaml` to publish the standard `go_` and `process_` series describing the exporter's own CPU and memory, under the names an existing Go dashboard already uses:

```yaml
config:
  data:
    config.yaml: |
      web:
        self_metrics:
          resource_metrics_enabled: true
      collectors:
        - name: example
          request:
            type: http
          ...
```

They are scraped by the self-metrics monitor along with everything else on that endpoint. They carry no per-collector or per-target labels, so they add a fixed number of series rather than one per target. Off by default: reading them briefly stops the world on every scrape, which an exporter scraped frequently by several Prometheus servers should not pay for unless the numbers are wanted.

## Extra volumes and arguments

The chart mounts one ConfigMap — the one it renders — and passes the flags it derives from the values above. `extraVolumes`, `extraVolumeMounts` and `extraArgs` cover everything beyond that, in the ordinary Kubernetes and command-line shapes, so a ConfigMap or Secret the chart does not create can be mounted alongside the standard one:

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

`extraVolumes` and `extraVolumeMounts` are passed through untouched, so anything a pod can mount — a ConfigMap, a Secret, a projected volume, an emptyDir — works here, and the entries are appended after the ones the chart makes rather than replacing them. `extraArgs` entries are appended after the chart's own flags, each one a whole argument.

Two collisions are rejected while rendering, because both fail in a way that points somewhere other than the values file:

* An `extraArgs` entry that sets a flag the chart already renders — `--web.listen-address`, `--config.file`, `--python.path`, `--log.level`, `--probe.timeout-offset` and the rest. Go keeps the last occurrence of a repeated flag, so the entry would quietly win; for the listen address the container port and the probes would still follow `server.listenAddress`, leaving a pod that listens on one port while Kubernetes checks another. The error names the value to set instead.
* An `extraVolumeMounts` entry whose `mountPath` is one the chart already mounts. Mounting over `/etc/prometheus-universal-exporter` replaces it, so the exporter starts with no `config.yaml` and crash-loops with an error about the file rather than about the mount that hid it. To add a file to that directory, mount it at its own path — `/etc/collectors`, say — and point the configuration at it.

A [`localfile`](../../docs/LOCALFILE.md) collector reads files the same way: mount the directory it names as `request.root` with these values, read-only, as shown in [Local files in Kubernetes](../../docs/LOCALFILE.md#in-kubernetes).

`--dry-run`, `--config.schema`, `--config.collector-file-schema`, `--version` and `--help` are rejected as well: each prints something and exits, so a pod started with one would restart for ever instead of serving. Run them as a separate command, a Job or an init container instead — see [Dry run](../../docs/CONFIGURATION.md#dry-run).

An entry that does not begin with `--` is rejected too, since `some.new-flag=value` as an argument is read as a positional value and ignored.

## Scheduled OTLP targets

The exporter can optionally scrape targets itself and send the metrics directly over OTLP.

Enable scheduled targets:

```yaml
otlpTargets:
  enabled: true
  data: |
    targets:
      - name: legacy_eu
        collector: legacy_text
        target: http://legacy.eu.example:8080
        request:
          path: /status
        labels:
          region: eu
```

OTLP export must also be enabled in the exporter configuration.

See `docs/OTLP.md` for the scheduled-target configuration format.

## Values

Every value has a default, and `values.yaml` documents each one in place. `values.schema.json` is checked by Helm on install, upgrade and `helm template`, so a misspelled key or a wrong type fails there rather than on a pod that starts and behaves unexpectedly.

| Value | Type | Default | What it sets |
| --- | --- | --- | --- |
| `replicaCount` | integer | `1` | Deployment replicas. |
| `image.repository` / `image.tag` / `image.pullPolicy` | string | GHCR, `latest`, `IfNotPresent` | The exporter image. Pin `tag` in production. |
| `imagePullSecrets` | array | `[]` | Secrets for a private registry. |
| `nameOverride` / `fullnameOverride` / `namespaceOverride` | string | `""` | Naming and namespace of the created objects. |
| `defaultLabels` / `defaultAnnotations` | map | `{}` | Metadata applied to every object the chart creates. |
| `serviceAccount` | object | created | `create`, `automount`, `name`, `annotations`. |
| `service` | object | enabled, ClusterIP, 8080 | The exporter Service. |
| `neg` | object | disabled | GKE Network Endpoint Group annotations on the Service. |
| `ingress` | object | disabled | Class, hosts, paths, TLS and annotations. |
| `server` | object | see [Exporter flags](#exporter-flags) | Exporter flags: `listenAddress`, `pythonPath`, `logLevel`, `probeTimeoutOffset`, `probeDefaultTimeout`, `shutdownTimeout`, `shutdownDelay`, `enableLifecycle`, `watchConfig`, `watchConfigInterval`, `expandEnv`. |
| `terminationGracePeriodSeconds` | integer | unset | The pod's grace period; see [Shutting down](#shutting-down). |
| `env` / `envFrom` | array | `[]` | Container environment, in the Kubernetes shapes. |
| `extraArgs` | array | `[]` | Extra command-line flags. |
| `extraVolumes` / `extraVolumeMounts` | array | `[]` | Volumes and mounts beyond the chart's own. |
| `config` | object | enabled | `enabled`, and `data` holding `config.yaml` and any [collector files](#collector-files). |
| `otlpTargets` | object | disabled | Scheduled targets rendered into the ConfigMap. |
| `monitors` | array | `[]` | `ServiceMonitor` and `PodMonitor` resources. |
| `selfMetrics` | object | enabled | The monitor for the exporter's own endpoint, and its path. |
| `resources` | object | 100m/128Mi, 500m/512Mi | Requests and limits. |
| `strategy` | object | RollingUpdate | Deployment strategy and its `rollingUpdate` settings. |
| `podSecurityContext` / `securityContext` | object | hardened | Pod and container security context. |
| `nodeSelector` / `tolerations` / `affinity` | map/array/object | empty | Scheduling. |
| `networkPolicy` | object | disabled | `ingress` and `egress` rules. |
| `targetAuth` | object | disabled | Secret-backed credentials mounted for the exporter to send to the target. |
| `webAuth` | object | disabled | A Secret's username and password mounted as files for the exporter's own Basic Auth; see [Exporter authentication](#exporter-authentication). |

## Other options

The chart also supports:

* Multiple `ServiceMonitor` and `PodMonitor` resources
* Prometheus relabeling and metric relabeling
* Monitor authentication
* Custom headers
* Custom labels and annotations
* Pod affinity and tolerations
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

* Non-root container
* Read-only root filesystem
* All Linux capabilities dropped
* Privilege escalation disabled
* No Kubernetes API permissions

## Uninstall

```bash
helm uninstall prometheus-universal-exporter \
  --namespace monitoring
```
