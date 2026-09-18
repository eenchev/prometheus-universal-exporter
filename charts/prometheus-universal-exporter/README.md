# Prometheus Universal Exporter chart

Install with `helm install exporter ./charts/prometheus-universal-exporter`. Replace the default ConfigMap with `--set-file config.data.config.yaml=config.yaml` or values supplied by your deployment system.

The chart creates a Deployment, Service, ServiceAccount, and ConfigMap. A ConfigMap checksum annotation triggers a rollout when collector configuration changes. The exporter also checks the file periodically and keeps the last valid configuration when a reload is invalid. `namespaceOverride`, `strategy`, `resources`, `tolerations`, and `affinity` are available directly in values.

The default `RollingUpdate` strategy also works with `replicaCount: 1`: `maxUnavailable: 25%` becomes zero unavailable replicas and `maxSurge: 25%` permits one extra Pod, so the old ready Pod remains until the replacement is ready. This can temporarily run two Pods. Use `strategy.type: Recreate` if overlap is undesirable.

The optional Prometheus Operator monitors are configured as an array because the chart can create multiple target monitors. The Prometheus Operator CRDs are external dependencies, so the array is empty by default:

```yaml
monitors:
  - name: application-services
    enabled: true
    type: service # pod or service
    collector: example
```

Each array item creates one named ServiceMonitor or PodMonitor. All scrape settings, selectors, headers, authentication, relabelings, and metric relabelings live under that item. Each target endpoint selects one collector. The chart adds the target-routing relabelings that pass the discovered address as `target`, preserve it as `instance`, and route the scrape to the exporter Service. Values in an item's `relabelings` are appended to those built-ins. When enabled, `selfMetrics.enabled` creates one self-health monitor for each monitor type used by the array.

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

`targetAuth` is `null` by default, so no target credential Secret is mounted. To mount basic authentication from a Kubernetes Secret:

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

The chart defaults to a non-root, read-only-root-filesystem container, drops Linux capabilities, and does not install Kubernetes API permissions. `ingress.enabled` creates an Ingress, while `neg.enabled` adds the GKE NEG service annotation. Configure `networkPolicy` in an environment-specific values file if target access must be restricted.
