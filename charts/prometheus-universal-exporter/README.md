# Prometheus Universal Exporter chart

The chart is versioned and released independently of the exporter: `version` in `Chart.yaml` is the chart's own version and is the source of truth for a release, while `appVersion` records the exporter release the chart was validated against. Chart releases are tagged `chart/prometheus-universal-exporter-<version>`, separate from the exporter's own `exporter/prometheus-universal-exporter-v<version>` tags; the release workflow refuses a tag that does not match `Chart.yaml`. Released charts are published to `oci://ghcr.io/eenchev/charts/prometheus-universal-exporter`.

Install with `helm install exporter ./charts/prometheus-universal-exporter`. Replace the default ConfigMap with `--set-file 'config.data.config\.yaml=config.yaml'` or values supplied by your deployment system. Quote the argument and escape the dot: Helm splits `--set` keys on unescaped dots, so the unquoted form sets a nested `config.data.config.yaml` path instead of the single `config.yaml` key the exporter reads.

The chart creates a Deployment, Service (enabled by default), ServiceAccount, and ConfigMap. Set `service.enabled: false` to omit the Service. A ConfigMap checksum annotation triggers a rollout when collector configuration changes. The exporter also checks the file periodically and keeps the last valid configuration when a reload is invalid. `namespaceOverride`, `strategy`, `resources`, `tolerations`, and `affinity` are available directly in values.

`defaultLabels` and `defaultAnnotations` are applied to every chart-created Kubernetes object, including Pod template metadata and optional monitor, ingress, and NetworkPolicy resources. Resource-specific `labels` and `annotations` are applied afterward and override a same-named default; generated chart labels and required annotations remain authoritative where necessary.

```yaml
defaultLabels:
  app.kubernetes.io/part-of: observability
defaultAnnotations:
  owner.example.com/team: platform
```

The default `RollingUpdate` strategy also works with `replicaCount: 1`: `maxUnavailable: 0` keeps the old Pod available and `maxSurge: 1` permits one extra Pod, so the rollout can temporarily run two Pods until the replacement is ready. Use `strategy.type: Recreate` if overlap is undesirable. Percentage values are also supported by Kubernetes, but explicit values make the single-replica behavior clear.

The optional Prometheus Operator monitors are configured as an array because the chart can create multiple target monitors. The Prometheus Operator CRDs are external dependencies, so the array is empty by default:

```yaml
monitors:
  - name: application-services
    enabled: true
    type: service # pod or service
    collector: example
```

Each array item creates one named ServiceMonitor or PodMonitor. All scrape settings, selectors, headers, authentication, relabelings, and metric relabelings live under that item. Each target endpoint selects one collector. The chart adds the target-routing relabelings that pass the discovered address as `target`, preserve it as `instance`, and route the scrape to the exporter Service. Values in an item's `relabelings` are appended to those built-ins. When enabled, `selfMetrics.enabled` creates one self-health monitor for each monitor type used by the array. ServiceMonitor and PodMonitor entries use the exporter Service for routing, so keep `service.enabled: true` when using the chart-generated monitors.

Each entry's `interval` and `scrapeTimeout` configure the Prometheus scrape. The optional `params` map is rendered as `/probe` query parameters and can override collector request settings for that scrape:

```yaml
params:
  method: [POST]
  path: [/api/status]
  timeout: [5s]
  body: [raw request body]
```

The `body` value is an opaque string and is not required to be JSON. Values in `params` must be lists because that is the Prometheus Operator format. When no `timeout` parameter is supplied, the exporter uses the incoming scrape request context and does not apply a separate collector timeout.

`relabelings` and `metricRelabelings` accept the native Prometheus Operator structures. `relabelings` run during target relabeling; `metricRelabelings` run on scraped samples. Self-health monitor relabelings can be set separately with `selfMetrics.relabelings` and `selfMetrics.metricRelabelings`.

Monitor authentication is disabled by default; set an item's `auth.enabled: true` and choose `auth.type: bearer` or `auth.type: basic` to render the Prometheus Operator `authorization` or `basicAuth` configuration. Header entries are rendered as `header_<name>` endpoint parameters and are forwarded to the target only when the collector lists the canonical name in `request.forward_headers`. To pass the monitor's Authorization header through to the target, the collector must also set `request.forward_authorization: true`. Do not place secrets in `headers`; use a Kubernetes Secret through `auth`.

For example:

```yaml
monitors:
  - name: application-services
    enabled: true
    type: service
    collector: example
    relabelings:
      - sourceLabels: [__meta_kubernetes_service_label_team]
        targetLabel: team
    metricRelabelings:
      - sourceLabels: [__name__]
        regex: exporter_debug_.+
        action: drop
```

Exporter endpoint protection is configured in the mounted exporter configuration, not in the monitor values:

```yaml
web:
  basic_auth:
    enabled: true
    username: exporter
    password: change-me
```

This protects `/probe`, `/metrics`, and `selfMetrics.path`; `/health` and `/ready` remain open for Kubernetes probes. It cannot be enabled with any collector using `request.forward_authorization`.

`targetAuth.enabled` is `false` by default, so no target credential Secret is mounted. To mount basic authentication from a Kubernetes Secret:

```yaml
targetAuth:
  enabled: true
  type: basic
  secretName: target-basic-auth
  usernameKey: username
  passwordKey: password
  mountPath: /var/run/prometheus-universal-exporter/target-auth
  usernameFileName: username
  passwordFileName: password

# In config.data.config.yaml:
request:
  basic_auth_file:
    username: /var/run/prometheus-universal-exporter/target-auth/username
    password: /var/run/prometheus-universal-exporter/target-auth/password
```

For bearer authentication, set `type: bearer`, `secretKey: token`, and `fileName: token`, then reference `request.bearer_token_file`. The exporter then uses its own configured Basic Auth for incoming scrapes and the mounted Kubernetes Secret for the target request. These credentials are independent.

Each `monitors` entry's `params` map becomes `/probe` query parameters, so per-scrape request overrides need no chart changes. Alongside `method`, `path`, `timeout`, `body`, `insecure_skip_verify` and the retry settings, `follow_redirects: ["true"]` and `enable_http2: ["true"]` override the collector's redirect and HTTP/2 behaviour for that monitor. Both default to false on the collector; values must be lists, and each must be exactly `"true"` or `"false"` or the exporter answers 400.

The exporter's process settings are values, not hardcoded arguments:

```yaml
server:
  listenAddress: ":8080"
  pythonPath: /usr/local/bin/python3
```

`server.listenAddress` becomes `--web.listen-address` and also sets the container port, so an override such as `0.0.0.0:9115` moves the listener and the port together. The port keeps the name `http`, which is what the Service, Ingress, ServiceMonitor, and PodMonitor reference, so nothing else needs changing; `service.port` stays independent. A value without a valid TCP port fails `helm template` with an explicit message.

`server.pythonPath` becomes `--python.path`, the interpreter used by the `python` transform. The default matches the exporter image, which is based on `python:3.12-slim` and installs Python at `/usr/local/bin/python3`. Override it when you run a custom image with the interpreter somewhere else.

Set `otlpTargets.enabled` to have the exporter scrape a fixed list of targets itself and deliver only those metrics over OTLP, with no Prometheus involved. `otlpTargets.data` holds the scheduled target document; it is rendered into the same ConfigMap as the exporter configuration, so changing it rolls the Deployment through the existing checksum annotation, and the chart passes `--otlp.targets-file` automatically.

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
        otlp:
          service_name: legacy-app
```

Scheduled targets require OTLP export. When the chart manages the configuration, enabling them without `otlp.enabled: true` in `config.data.config.yaml` fails `helm template` with an explicit message rather than producing a Deployment that crash-loops. With an external ConfigMap (`config.enabled: false`) the chart cannot check, and the exporter reports the same requirement at startup. See [docs/OTLP.md](../../docs/OTLP.md) for the target document format.

`server.watchConfig` passes `--config.watch` so the exporter re-reads its configuration in place when the mounted files change, with `server.watchConfigInterval` (default `60s`, validated at render time) controlling how often it checks. It is off by default because the chart's ConfigMap checksum annotation already rolls the Deployment whenever chart-managed configuration changes — the pod restarts with the new configuration and an in-place reload would never be reached. Turn it on when `config.enabled` is `false` and an external ConfigMap is updated without triggering a rollout.

Verbose self-metrics are configured in the exporter configuration rather than in chart values: set `web.self_metrics.verbose: true` inside `config.data.config.yaml` to republish every exporter self-metric broken down by collector, request URL and method, labelled `collector`, `http_method` and `url`. The per-collector series are unchanged and stay alongside them, so constrain the label when querying one shape (`{http_method=""}` for the collector totals, `{http_method!=""}` for the requests). They are off by default because a request URL is an unbounded label value and each combination carries a whole metric family; tracking is capped at 1000 combinations, `http_exporter_request_series_tracked` reports how many are in use and `http_exporter_request_series_capped` reports when that cap is reached. Scheduled OTLP targets are listed from startup; a `/probe` request appears the first time it is served, because its URL comes from the probe's `target` parameter. If you scrape the exporter with the self-metrics monitor, consider the extra cardinality before enabling it across a large target set.

Collector caching is configured in the exporter configuration, not in chart values. Add `cache: 60s` to a collector in `config.data.config.yaml` to answer repeated identical probes from memory. The cache is per-process and in-memory, so each replica keeps its own entries and a rollout empties them; with several replicas, expect up to one target request per replica per interval.

The chart defaults to a non-root, read-only-root-filesystem container, drops Linux capabilities, and does not install Kubernetes API permissions. `ingress.enabled` creates an Ingress, while `neg.enabled` adds the GKE NEG service annotation. Configure `networkPolicy` in an environment-specific values file if target access must be restricted.
