# Prometheus Operator

The exporter is scraped like any other exporter, with one addition: the real
target travels in the `target` query parameter, so every monitor needs the
relabeling that moves the discovered address into it.

The chart's generated ServiceMonitor and PodMonitor show the required
relabeling, here as rendered for a release `exporter` in the namespace
`monitoring`:

```yaml
params:
  collector: [legacy_text]
relabelings:
  - sourceLabels: [__address__]
    targetLabel: __param_target
  - sourceLabels: [__param_target]
    targetLabel: instance
  - targetLabel: __address__
    replacement: exporter-prometheus-universal-exporter.monitoring.svc:8080
```

The last rule sends every scrape to the exporter's Service, addressed as
`<fullname>.<namespace>.svc:<service.port>`, so it reaches the exporter from
any namespace Prometheus runs in.

One ServiceMonitor endpoint selects one collector. Use multiple endpoints or monitor resources for multiple collector configurations. The same pattern works for PodMonitor.

Each Helm `monitors` entry can set `interval` and `scrapeTimeout` for the Prometheus scrape. Its `params` map can override the selected collector's request method, path, timeout, or raw body for that scrape:

```yaml
monitors:
  - name: write-status
    enabled: true
    type: service
    collector: legacy_text
    interval: 30s
    scrapeTimeout: 10s
    params:
      method: [POST]
      path: [/api/status]
      timeout: [5s]
      body: [raw request body]
      retry_attempts: ["2"]
      retry_backoff: ["2s"]
      param_tenant: [acme]   # fills {{param_tenant}} in the collector's request.path, or in a label value
```

The body is opaque text and does not need to be JSON. Without a `timeout` parameter, the exporter uses the incoming Prometheus scrape context as the target request timeout.

`params` cannot set `collector` or `target`: the chart renders the first from the entry's `collector`, which it checks against the configuration, and the second from each discovered target's address, so either would be a second key of the same name, and rendering fails. `interval` and `scrapeTimeout` are Prometheus durations — whole numbers of `y`, `w`, `d`, `h`, `m`, `s` and `ms`, such as `1m30s` — and the entry's `name`, which names the monitor `<fullname>-<name>`, is a unique DNS-1123 label. A `scrapeTimeout` longer than its `interval` fails rendering, since Prometheus refuses such a scrape.

A monitor probes its targets on the port named `http` and finds them in its own namespace. An entry's `port` names another port — of the selected Services for `type: service`, of the selected pods' containers for `type: pod` — and its `namespaceSelector`, the Prometheus Operator's, other namespaces:

```yaml
monitors:
  - name: queues
    enabled: true
    type: service
    collector: queue_depth
    port: grpc
    namespaceSelector:
      matchNames: [payments, search]   # or any: true
```

`port` is the port's name, never its number, as the Prometheus Operator's `port` field is: `port: "9115"` would match no port, so the chart refuses a number while rendering and says to name the port — in the Service's `spec.ports` for `type: service`, in the pod's `containers[].ports` for `type: pod` — and give that name.

## Related pages

- [REQUESTS.md](REQUESTS.md) — the request parameters a monitor's `params` map can set.
- [AUTHENTICATION.md](AUTHENTICATION.md) — monitor authentication and forwarding it to the target.
- [../charts/prometheus-universal-exporter/README.md](../charts/prometheus-universal-exporter/README.md) — every `monitors` value the chart accepts.
