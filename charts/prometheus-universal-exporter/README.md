# Prometheus Universal Exporter chart

Install with `helm install exporter ./charts/prometheus-universal-exporter`. Replace the default ConfigMap with `--set-file config.data.config.yaml=config.yaml` or values supplied by your deployment system.

The chart creates a Deployment, Service, ServiceAccount, and ConfigMap. A ConfigMap checksum annotation triggers a rollout when collector configuration changes. The exporter also checks the file periodically and keeps the last valid configuration when a reload is invalid. `namespaceOverride`, `strategy`, `resources`, `tolerations`, and `affinity` are available directly in values.

`serviceMonitor.enabled` and `podMonitor.enabled` are opt-in because the Prometheus Operator CRDs are external dependencies. For a single monitor choice, use the explicit selector:

```yaml
monitor:
  enabled: true
  type: service # pod or service
```

This renders either a ServiceMonitor or a PodMonitor. The existing `serviceMonitor.enabled` and `podMonitor.enabled` flags remain available for compatibility and explicit per-resource control; do not enable both mechanisms for both monitor kinds at the same time, or duplicate monitor resources may be created. Set `serviceMonitor.collector` or `podMonitor.collector` to select one server-side collector. Each target endpoint selects one collector; use additional monitor resources/endpoints for others. Relabeling passes the discovered address as `target`, preserves it as `instance`, and routes the scrape to the exporter Service. When enabled, `selfMetrics.enabled` creates a second monitor resource selecting the exporter itself and scraping `selfMetrics.path`.

Both monitor values support `headers` and Secret-backed `auth`. Monitor authentication is disabled by default; set the selected monitor's `auth.enabled: true` and choose `auth.type: bearer` or `auth.type: basic` to render the Prometheus Operator `authorization` or `basicAuth` configuration. Header entries are rendered as `header_<name>` endpoint parameters and are forwarded to the target only when the collector lists the canonical name in `request.forward_headers`. To pass the monitor's Authorization header through to the target, the collector must also set `request.forward_authorization: true`. Do not place secrets in `headers`; use a Kubernetes Secret through `auth`.

Exporter endpoint protection is configured in the mounted exporter configuration, not in the monitor values:

```yaml
web:
  basic_auth:
    enabled: true
    username: exporter
    password: change-me
```

This protects `/probe`, `/metrics`, and `selfMetrics.path`; `/health` and `/ready` remain open for Kubernetes probes. It cannot be enabled with any collector using `request.forward_authorization`.

For the non-bridge model, enable `targetAuth` and reference the mounted token in the collector:

```yaml
targetAuth:
  enabled: true
  secretName: target-api-token
  secretKey: token

# In config.data.config.yaml:
request:
  bearer_token_file: /var/run/prometheus-universal-exporter/target-auth/token
```

The exporter then uses its own configured Basic Auth for incoming scrapes and the Kubernetes Secret token for the target request. These credentials are independent.

The chart defaults to a non-root, read-only-root-filesystem container, drops Linux capabilities, and does not install Kubernetes API permissions. `ingress.enabled` creates an Ingress, while `neg.enabled` adds the GKE NEG service annotation. Configure `networkPolicy` in an environment-specific values file if target access must be restricted.
