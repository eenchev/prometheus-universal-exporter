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
* Static targets the exporter scrapes itself and delivers over OTLP
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
  --version 1.5.0 \
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
  --version 1.5.0 \
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

The chart creates the required monitor and routes Prometheus requests through the exporter, at the Service's full name, `<release>-prometheus-universal-exporter.<namespace>.svc:8080`, so a Prometheus in another namespace reaches it.

A monitor must name a `collector`, and, when the chart holds the configuration, one that `config.data` defines, in `config.yaml` or a collector file among its keys: otherwise every probe it sends would be answered `400`, so rendering fails first. When `config.yaml` lists collector files by absolute path, which may lie outside the chart's ConfigMap, a collector not found in `config.data` is left to the exporter to check.

> Probe monitors, of either type, send Prometheus to the exporter's Service, so they need `service.enabled: true`; with it off, rendering fails.

Each monitor is named `<release>-prometheus-universal-exporter-<name>`, so `name` is a DNS-1123 label — lower-case letters, digits and `-`, starting and ending with a letter or digit — unique among the entries, and neither `self` nor `static-targets`, the names of the chart's own monitors; anything else fails rendering. Without `targetSelector` a monitor selects the targets labelled `app.kubernetes.io/name: target`; with it, its `matchLabels` or `matchExpressions`. `interval` and `scrapeTimeout`, here and on the self-metrics and static targets monitors, are Prometheus durations, as the Prometheus Operator takes them: whole numbers of `y`, `w`, `d`, `h`, `m`, `s` and `ms`, such as `30s`, `1m30s` or `1500ms`; a fraction such as `1.5m`, or `us` and `ns`, fail rendering.

The exporter's own metrics get a monitor too, `<release>-prometheus-universal-exporter-self`, while `selfMetrics.enabled` is on: a `ServiceMonitor`, or a `PodMonitor` with `selfMetrics.type: pod`. It is rendered when the chart renders any other monitor — a probe monitor or the [static targets](#static-targets) monitor — or when the cluster serves that kind, so an install without the Prometheus Operator does not fail on it.

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
* `from` and `until`, the render window of a [`graphite`](../../docs/GRAPHITE.md) collector
* `message`, the request message of a [`grpc`](../../docs/GRPC.md) collector

`collector` and `target` are not among them: the chart renders the collector parameter from the monitor's `collector`, where it is checked against `config.data`, and the target parameter from each discovered target's address. A `params` entry of either would render a second key of the same name, so it fails rendering, pointing to `collector` and `targetSelector`.

For example, a collector with `path: /api/{{param_tenant}}/status` scraped by a
monitor with `params: {param_tenant: [acme]}` requests `/api/acme/status`. A
placeholder may have a default after a colon, `{{param_tenant:acme}}`; one with
no default that the monitor does not supply fails the scrape with `400`. See
[Target requests](../../docs/REQUESTS.md#path-parameters).

A [`graphite`](../../docs/GRAPHITE.md) collector is monitored the same way: a
monitor selects the Graphite Service, whose address becomes the `target`, and
its `params` fill the placeholders of the collector's `request.targets`, such
as `param_env: [staging]`, or set the window, such as `from: [-1h]`. `method`
and `body` do not apply to it and are answered with `400`.

A [`grpc`](../../docs/GRPC.md) collector is monitored the same way too: a
monitor selects the Service of the gRPC server, on its gRPC port, whose
`host:port` address becomes the `target`, called in plaintext unless the
collector sets `request.tls`. Its `params` fill the placeholders of the
collector's `request.message` and `metadata`, such as `param_queue: [orders]`,
or replace the message, such as `message: ['{"queue": "orders"}']`. `method`,
`path` and `body` do not apply to it and are answered with `400`. A
`descriptors: protoset` or `proto` collector reads its files from the
exporter's filesystem: mount them from a ConfigMap with `extraVolumes` and
`extraVolumeMounts`.

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

A password written there ends up in the chart's ConfigMap, and the monitors need the credential too. Mount it from a Secret with `webAuth` instead and point the configuration at the files; every monitor the chart renders then presents it:

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
```

With `webAuth.enabled`, every monitor the chart renders sends the `webAuth` Secret's credential as `basicAuth`, since `web.basic_auth` protects `/probe` and the self-metrics and static targets endpoints alike: the self-metrics and static targets monitors, and each probing monitor without an `auth` of its own. A probing monitor whose `auth.enabled` is true sends its own credential instead. `webAuth.usernameKey` and `webAuth.passwordKey` name the Secret's keys, `username` and `password` by default; the files are always named `username` and `password` under `webAuth.mountPath`. The exporter reads a file again when it changes, so a rotated Secret takes effect without a restart. An `extraVolumeMounts` entry at `webAuth.mountPath` is rejected while rendering.

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
| `server.probeMaxConcurrent` | `--probe.max-concurrent` | unset: no limit across collectors |
| `server.pythonMaxWorkers` | `--python.max-workers` | unset: no limit across scripts |
| `server.shutdownTimeout` | `--web.shutdown-timeout` | unset: the exporter's `15s` |
| `server.shutdownDelay` | `--web.shutdown-delay` | `5s` |
| `server.watchConfig` / `server.watchConfigInterval` | `--config.watch` / `--config.watch-interval`, a positive Go duration such as `60s` or `1m30s` | off / `60s` |
| `server.expandEnv` | `--config.expand-env` | off |
| `server.enableLifecycle` | `--web.enable-lifecycle` | off |
| `server.probeDebug` | `--web.enable-probe-debug` | off |
| `staticTargets.enabled` | `--static-targets-file` | off |
| `staticTargets.expandEnv` | `--static-targets.expand-env`, with `staticTargets.enabled` | off |
| `config` | `--config.file` | the chart's ConfigMap |

`server.listenAddress` sets the container port too, so the listener and the probes cannot drift apart. Its host may be empty, as in `:8080`, a wildcard such as `0.0.0.0:8080` or `[::]:8080`, or a pod address; a loopback host — `127.x.x.x`, `localhost` or `[::1]` — fails rendering, since neither the kubelet's probes nor the Service reach it. `server.pythonPath` is the interpreter used by the `python` transform; its default is where the exporter image's `python:3.12-slim` base installs Python, and it is worth overriding only for a custom image. `server.logLevel` is one of `debug`, `info`, `warn` and `error`, and anything else fails rendering.

`server.probeTimeoutOffset` is how much of Prometheus's scrape timeout — a monitor's `scrapeTimeout` — a probe leaves unused, so a slow target or a hung file read is answered with the exporter's own error before Prometheus gives up (see [Probe deadlines](../../docs/CONFIGURATION.md#probe-deadlines)). It takes a Go duration of zero or more. Left empty, the flag is not rendered at all, so the exporter's default applies and an image older than the flag still starts; set it only with an image that has it.

`server.probeDefaultTimeout` bounds a probe without a scrape timeout header, as from curl, a script or the exporter's collectors page with JavaScript off; a `timeout` parameter bounds the request within it and cannot lift it. Prometheus always sends a scrape timeout, so its scrapes are unaffected. It takes a Go duration of zero or more, `0` leaving such a probe unbounded, and like `probeTimeoutOffset` it is rendered only when set.

`server.probeMaxConcurrent` bounds the trips to targets, probes and static target scrapes of every collector together, on top of each collector's `max_concurrent_probes`; each holds a response and its series in memory, so this bounds what a burst of probes can cost the pod. `server.pythonMaxWorkers` bounds the Python workers of every collector together, each a process with memory of its own; pair it with a collector's `limits.max_script_memory` (see [Python](../../docs/PYTHON.md)). Both take a whole number, `0` for no limit, and are rendered only when set. Size them to `resources.limits.memory`.

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

Kubernetes kills a pod `terminationGracePeriodSeconds` after asking it to stop, 30 seconds unless set. A stopping exporter needs its shutdown delay, its shutdown timeout and about 10 seconds more, for the last OTLP export and exiting — 30 seconds with the defaults, which Kubernetes' default already covers. Left unset, `terminationGracePeriodSeconds` is rendered as that sum whenever it is more than 30; set, it must be at least that, or rendering fails:

```sh
helm install exporter charts/prometheus-universal-exporter \
  --set server.shutdownTimeout=1m   # renders terminationGracePeriodSeconds: 75
```

Raise `terminationGracePeriodSeconds` further if `otlp.timeout` is longer than its default of 5 seconds.

`server.enableLifecycle` enables `POST /-/reload`, which reloads the configuration at once and answers `200` when it was accepted or `500` with the reason when it was not — see [Reloading on demand](../../docs/CONFIGURATION.md#reloading-on-demand). A chart-managed ConfigMap does not need it, since a change rolls the Deployment; it is for a ConfigMap updated in place with `config.enabled: false`. Like `probeTimeoutOffset`, the flag is only rendered when set, so older images still start.

`server.probeDebug` enables `/probe?debug=true`, which answers a probe with a plain-text report of its trip instead of its metrics, a *Debug report* switch on each form of `/collectors`, and `/static-targets?debug=<name>` for one static target — see [Debugging a probe](../../docs/CONFIGURATION.md#debugging-a-probe). The report shows what the target answered, so it is off by default; turn it on while a collector is being written or fixed, and off again. Rendered only when `true`.

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

A collector file holds `collectors` and nothing else, and a collector name must be unique across `config.yaml` and every file, or the pod refuses to start. Name the keys so a pattern matches them and nothing else: `*.yaml` would also match the static target file the chart puts in the same directory. A file changes the ConfigMap checksum like `config.yaml` does, so it rolls the Deployment, or, with `server.watchConfig`, is reloaded in place. Collector files kept in a ConfigMap of their own can be mounted with `extraVolumes` and `extraVolumeMounts` at their own path, such as `/etc/collectors`, and listed by absolute path: `/etc/collectors/*.yaml`.

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

`goMemLimit`, on by default, renders `--runtime.memory-limit-ratio` with `goMemLimit.ratio`, `0.8`: the exporter reads the container's memory limit from its cgroup and sets the Go memory limit to that share of it, so the Go runtime collects harder as its heap nears it instead of growing past the container's limit into an OOM kill. The rest is left to what the Go heap does not count — the Python workers, which are processes of their own, and the runtime's overhead; lower the ratio for a configuration with many Python workers, and bound those with `server.pythonMaxWorkers` and `limits.max_script_memory`. Without a memory limit in `resources`, the exporter keeps the Go default, and `GOMEMLIMIT` set in `env` wins over the ratio.

### Probes

The chart checks `/health` for liveness and `/ready` for readiness on the `http` port. `livenessProbe` and `readinessProbe` set their timings; the check itself is the chart's, and setting `httpGet`, `exec`, `tcpSocket` or `grpc` fails rendering. `terminationGracePeriodSeconds` is accepted on `livenessProbe` only: Kubernetes refuses it on a readiness probe, so the values schema does too. The liveness probe has some slack by default, since a pod busy with a burst of probes is slow rather than dead, and a restart would lose its cache and Python workers:

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

### Replicas

```yaml
replicaCount: 2
```

Probes scale with replicas, since Prometheus sends each to one pod through the Service. Static targets do not: every replica scrapes every static target on its own schedule and serves its own results, so run [static targets](#static-targets) with `replicaCount: 1` (the chart's notes warn otherwise), or split them over releases.

To keep replicas apart, spread them over zones or nodes. A constraint without a `labelSelector` gets one selecting this release's pods:

```yaml
topologySpreadConstraints:
  - maxSkew: 1
    topologyKey: topology.kubernetes.io/zone
    whenUnsatisfiable: ScheduleAnyway
priorityClassName: monitoring
```

### Autoscaling

`autoscaling` renders an `autoscaling/v2` HorizontalPodAutoscaler, which then sets the Deployment's replica count itself; `replicaCount` is left out of the Deployment so an upgrade does not put it back. It scales on CPU utilization by default, measured against `resources.requests`; `targetMemoryUtilizationPercentage` and further `metrics` can be added, and `behavior` takes the scale-up and scale-down policies. `maxReplicas` below `minReplicas`, or nothing to scale on, fails rendering.

```yaml
autoscaling:
  enabled: true
  minReplicas: 2
  maxReplicas: 6
  targetCPUUtilizationPercentage: 75
```

Each replica keeps its own [response cache](../../docs/CONFIGURATION.md#response-caching) and shares only its own identical probes in flight, and the Service spreads probes across replicas. So as replicas are added, the cache answers a smaller share of probes and more of them reach the targets: with a 30s `ttl` and four replicas, a target probed every 15s may be asked up to four times per `ttl` instead of once, and a result kept for `stale_if_error` is only served by the replica that stored it. Where the cache is what spares a target, keep the replica count low, or give Prometheus's scrapes of one target a stable replica — a Service with `sessionAffinity: ClientIP` sends every probe from one Prometheus to one pod.

Autoscaling suits a release that serves probes. It does not suit [static targets](#static-targets): every replica scrapes every static target, so each replica the autoscaler adds is another full set of requests to every target — the load it was scaling out from grows with it — and each replica serves its own results. A target with `export_via_otlp` is exported over OTLP by every replica, so the OTLP backend receives duplicate series under the same resource. The chart's notes warn when static targets are rendered with autoscaling, naming the targets exported over OTLP. Put static targets in a release of their own, with `replicaCount: 1` and autoscaling off.

`podDisruptionBudget` renders a PodDisruptionBudget, so a node drain does not take every replica down at once. Set one of `minAvailable` or `maxUnavailable`, a count or a percentage; both fail rendering, and neither gives `maxUnavailable: 1`. With one replica, `minAvailable: 1` would block a drain until the pod is deleted by hand. `maxUnavailable: 0` is valid Kubernetes and is rendered as set, but it allows no voluntary eviction at all, so a drain of a node running the exporter waits for ever; the chart's notes warn about it.

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

`service.type` is `ClusterIP`, `NodePort` or `LoadBalancer`; not `ExternalName`, a DNS alias with no endpoints, through which neither Prometheus nor the monitors would reach the exporter.

`service.sessionAffinity: ClientIP` sends every probe from one Prometheus to one pod, so with several replicas its probes keep finding that pod's [cache](#autoscaling); empty, the default, leaves Kubernetes' `None`.

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

Enable a NetworkPolicy to restrict which workloads can reach the exporter, and, with egress rules, which targets it can reach:

```yaml
networkPolicy:
  enabled: true
```

On its own this limits ingress to the `http` port, from anywhere, which Prometheus scrapes, and leaves egress open, since the targets a probe names are not known to the chart. `ingress` rules replace that default. `egress` rules limit egress to what they allow, plus DNS, port 53 over UDP and TCP, unless `allowDNS: false`:

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

When using an external ConfigMap, you can enable configuration watching:

```yaml
server:
  watchConfig: true
  watchConfigInterval: 60s
```

This allows the exporter to reload the configuration when the mounted ConfigMap changes without requiring a Deployment rollout.

### Environment variables in the configuration

`server.expandEnv` adds `--config.expand-env`, which substitutes `${NAME}` references in the configuration and its collector files from the container's environment before they are parsed; `staticTargets.expandEnv` adds `--static-targets.expand-env`, which does the same for the static target document, independently. That lets a hostname, tenant or token come from a Secret rather than from the ConfigMap the chart renders:

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

## Static targets

The exporter can scrape a fixed list of targets itself, each on its own
interval, and serve their latest results together on one endpoint for
Prometheus to scrape; a target with `export_via_otlp: true` is also delivered
over OTLP. See [Static targets](../../docs/STATIC-TARGETS.md) for the document.

Run static targets with `replicaCount: 1` and [autoscaling](#autoscaling) off.
Every replica scrapes every static target, so with three replicas each target
is contacted three times per interval, each replica serves results of its own
through the one Service, and a target with `export_via_otlp` is exported over
OTLP three times, as duplicate series.
To spread many static targets, split them over releases, each with a document
of its own.

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
| `staticTargets.enabled` | `false` | Render `data` into the ConfigMap and pass it with `--static-targets-file`. |
| `staticTargets.fileName` | `static-targets.yaml` | Its file name in the mounted configuration directory, which is its key in the ConfigMap: letters, digits, `-`, `_` and `.`, and not a key of `config.data`. |
| `staticTargets.path` | `/static-targets` | Rendered as `--web.static-targets-path`, always: where the targets' latest results are served. It may not be `selfMetrics.path` or another endpoint's. |
| `staticTargets.data` | `""` | The static target document. A change rolls the Deployment. Required with `config.enabled`; with `config.enabled: false`, which renders no ConfigMap, leave it empty and put the file into your own ConfigMap under `fileName`. |
| `staticTargets.expandEnv` | `false` | Rendered as `--static-targets.expand-env` with `enabled`: expand `${NAME}` references in `data` from the container's environment. Independent of `server.expandEnv`. |
| `staticTargets.monitor.enabled` | `true` | Render the monitor that scrapes the endpoint, when `staticTargets.enabled` is. It needs the Prometheus Operator's CRDs. |
| `staticTargets.monitor.type` | `service` | `service` for a ServiceMonitor, `pod` for a PodMonitor. |
| `staticTargets.monitor.interval` / `scrapeTimeout` | `30s` / `10s` | How often Prometheus reads the endpoint. The exporter scrapes the targets on the document's own intervals whatever this is. |
| `staticTargets.monitor.labels` / `annotations` | `{}` | Added to the monitor. |
| `staticTargets.monitor.relabelings` / `metricRelabelings` | `[]` | Passed to the monitor's endpoint. |
| `staticTargets.monitor.targets` | `[]` | Names of the static targets the monitor reads, rendered as the endpoint's `targets` parameter; empty reads every target. A name that is not a target in `data` fails rendering. |

The monitor sets `honorLabels: true`, so the series keep their own
`static_target` and `target` labels, and the targets' labels, rather than
Prometheus's. That is why a target may not set `job` or `instance`: the
exporter refuses them, since they would replace the ones Prometheus gives the
endpoint's series. With `webAuth.enabled` it presents the exporter's credential, as
the self-metrics monitor does.

A target with `export_via_otlp` needs `otlp.enabled: true` in the exporter
configuration. When the chart manages the configuration, rendering fails if a
target sets it while OTLP export is off, rather than the pod failing to start.

## Values

Every value has a default, and `values.yaml` documents each one in place. `values.schema.json` is checked by Helm on install, upgrade and `helm template`, so a misspelled key or a wrong type fails there rather than on a pod that starts and behaves unexpectedly.

| Value | Type | Default | What it sets |
| --- | --- | --- | --- |
| `replicaCount` | integer | `1` | Deployment replicas. |
| `image.repository` / `image.tag` / `image.pullPolicy` | string | GHCR, `""`, `IfNotPresent` | The exporter image. An empty `tag` deploys the chart's `appVersion`, the exporter release the chart version was validated against; set it to run another. |
| `imagePullSecrets` | array | `[]` | Secrets for a private registry. |
| `nameOverride` / `fullnameOverride` / `namespaceOverride` | string | `""` | Naming and namespace of the created objects. Objects are named `<release>-prometheus-universal-exporter`, or after the release alone when its name holds the chart's, so two releases in one namespace do not collide; `fullnameOverride` names them outright. |
| `defaultLabels` / `defaultAnnotations` | map | `{}` | Metadata applied to every object the chart creates. |
| `serviceAccount` | object | created | `create`, `automount`, `name`, `annotations`. |
| `service` | object | enabled, ClusterIP, 8080 | The exporter Service: `enabled`, `type` (`ClusterIP`, `NodePort` or `LoadBalancer`), `port`, `sessionAffinity`, `annotations`. |
| `neg` | object | disabled | GKE Network Endpoint Group annotations on the Service. |
| `ingress` | object | disabled | Class, hosts, paths, TLS and annotations. |
| `server` | object | see [Exporter flags](#exporter-flags) | Exporter flags: `listenAddress`, `pythonPath`, `logLevel`, `probeTimeoutOffset`, `probeDefaultTimeout`, `probeMaxConcurrent`, `pythonMaxWorkers`, `shutdownTimeout`, `shutdownDelay`, `enableLifecycle`, `probeDebug`, `watchConfig`, `watchConfigInterval`, `expandEnv`. |
| `terminationGracePeriodSeconds` | integer | unset | The pod's grace period; see [Shutting down](#shutting-down). |
| `env` / `envFrom` | array | `[]` | Container environment, in the Kubernetes shapes. |
| `extraArgs` | array | `[]` | Extra command-line flags. |
| `extraVolumes` / `extraVolumeMounts` | array | `[]` | Volumes and mounts beyond the chart's own. |
| `config` | object | enabled | `enabled`, and `data` holding `config.yaml` and any [collector files](#collector-files). |
| `staticTargets` | object | disabled | Static targets rendered into the ConfigMap. |
| `monitors` | array | `[]` | `ServiceMonitor` and `PodMonitor` resources, one per entry: `name` a unique DNS-1123 label other than `self` and `static-targets`, `interval` and `scrapeTimeout` Prometheus durations, `params` without `collector` or `target`; see [Configure Prometheus](#4-configure-prometheus). |
| `selfMetrics` | object | enabled | The monitor for the exporter's own endpoint, `type` `service` or `pod`, and its path; see [Configure Prometheus](#4-configure-prometheus). |
| `resources` | object | 100m/128Mi, 500m/512Mi | Requests and limits. |
| `goMemLimit` | object | enabled, `0.8` | `--runtime.memory-limit-ratio`: the Go memory limit as a share of the container's; see [Resources](#resources). |
| `livenessProbe` / `readinessProbe` | object | see [Probes](#probes) | The probes' timings; `terminationGracePeriodSeconds` on the liveness probe only. |
| `autoscaling` | object | disabled | `enabled`, `minReplicas`, `maxReplicas`, `targetCPUUtilizationPercentage`, `targetMemoryUtilizationPercentage`, `metrics`, `behavior`; see [Autoscaling](#autoscaling). |
| `podDisruptionBudget` | object | disabled | `enabled`, `minAvailable` or `maxUnavailable`, `unhealthyPodEvictionPolicy`; see [Replicas](#replicas). |
| `strategy` | object | RollingUpdate | Deployment strategy and its `rollingUpdate` settings. |
| `podSecurityContext` / `securityContext` | object | hardened | Pod and container security context; the pod runs as user, group and `fsGroup` 65532. |
| `podLabels` / `podAnnotations` | map | `{}` | Labels and annotations of the pods only, over `defaultLabels` and `defaultAnnotations`. A label the chart sets itself, such as `app.kubernetes.io/name`, fails rendering; `checksum/config` stays the chart's. |
| `priorityClassName` | string | `""` | The pods' PriorityClass. |
| `nodeSelector` / `tolerations` / `affinity` | map/array/object | empty | Scheduling. |
| `topologySpreadConstraints` | array | `[]` | Pod topology spread constraints; one without a `labelSelector` spreads this release's pods. See [Replicas](#replicas). |
| `networkPolicy` | object | disabled | `ingress` and `egress` rules, and `allowDNS` with egress rules; see [NetworkPolicy](#networkpolicy). |
| `targetAuth` | object | disabled | Secret-backed credentials mounted for the exporter to send to the target. |
| `webAuth` | object | disabled | A Secret's username and password mounted as files for the exporter's own Basic Auth, and presented by every monitor without `auth` of its own; see [Exporter authentication](#exporter-authentication). |

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
* No Kubernetes API permissions, and no service account token mounted, whichever account the pod runs as

## Uninstall

```bash
helm uninstall prometheus-universal-exporter \
  --namespace monitoring
```
