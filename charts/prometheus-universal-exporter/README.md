# Prometheus Universal Exporter — Helm Chart

Helm chart for deploying the Prometheus Universal Exporter to Kubernetes.

The chart creates the exporter Deployment, Service, ServiceAccount, and ConfigMap. By default, the exporter configuration is managed directly through Helm values.

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

See the exporter documentation for the full collector configuration format.

### 2. Install the exporter

```bash
helm install prometheus-universal-exporter \
  ./charts/prometheus-universal-exporter \
  --namespace monitoring \
  --create-namespace \
  -f values.yaml
```

To update the configuration later:

```bash
helm upgrade prometheus-universal-exporter \
  ./charts/prometheus-universal-exporter \
  --namespace monitoring \
  -f values.yaml
```

Configuration changes automatically roll the exporter Deployment.

### 3. Configure Prometheus

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

To protect the exporter's `/probe`, `/metrics`, and self-metrics endpoints:

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

The Kubernetes `/health` and `/ready` endpoints remain available for health checks.

## Common configuration

### Exporter flags

The exporter's own flags are chart values rather than something to assemble by hand. `server.listenAddress` sets `--web.listen-address` and the container port together, so the listener and the probes cannot drift apart. `server.pythonPath` sets `--python.path`, the interpreter used by the `python` transform; its default, `/usr/local/bin/python3`, is where the exporter image's `python:3.12-slim` base installs Python, and it is worth overriding only for a custom image.

```sh
helm install exporter charts/prometheus-universal-exporter \
  --set server.listenAddress=0.0.0.0:9115 \
  --set server.pythonPath=/usr/bin/python3.11
```

Anything the chart does not render from a named value goes in `extraArgs`, described under [Extra volumes and arguments](#extra-volumes-and-arguments).

### Configuration mount and rollout

The ConfigMap is mounted at `/etc/prometheus-universal-exporter/config.yaml`, and its checksum is part of the Deployment pod template — so a configuration change rolls the Deployment rather than waiting for the kubelet to refresh a mounted file. Set `server.watchConfig` instead when the pods should reload in place.

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
            headers:
              Authorization: Bearer ${API_TOKEN}
```

`env` and `envFrom` take the ordinary Kubernetes shapes and are useful on their own; `expandEnv` is inert without them. Expansion is off by default: only `${NAME}` is substituted and never `$NAME`, but a configuration carrying regexes, jq expressions or Python pre-scripts has dollar signs that are not references, so expanding should be a decision rather than a surprise. A reference whose variable is not set stops the exporter at startup with the variable named, rather than becoming an empty string — a missing Secret key is then a clear failure instead of a collector quietly scraping the wrong thing.

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
  - --log.level=debug
```

`extraVolumes` and `extraVolumeMounts` are passed through untouched, so anything a pod can mount — a ConfigMap, a Secret, a projected volume, an emptyDir — works here, and the entries are appended after the ones the chart makes rather than replacing them. `extraArgs` entries are appended after the chart's own flags, each one a whole argument.

Two collisions are rejected while rendering, because both fail in a way that points somewhere other than the values file:

* An `extraArgs` entry that sets a flag the chart already renders — `--web.listen-address`, `--config.file`, `--python.path` and the rest. Go keeps the last occurrence of a repeated flag, so the entry would quietly win; for the listen address the container port and the probes would still follow `server.listenAddress`, leaving a pod that listens on one port while Kubernetes checks another. The error names the value to set instead.
* An `extraVolumeMounts` entry whose `mountPath` is one the chart already mounts. Mounting over `/etc/prometheus-universal-exporter` replaces it, so the exporter starts with no `config.yaml` and crash-loops with an error about the file rather than about the mount that hid it. To add a file to that directory, mount it at its own path — `/etc/collectors`, say — and point the configuration at it.

An entry that does not begin with `--` is rejected too, since `log.level=debug` as an argument is read as a positional value and ignored.

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
