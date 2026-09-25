# Prometheus Operator

The exporter is scraped like any other exporter, with one addition: the real
target travels in the `target` query parameter, so every monitor needs the
relabeling that moves the discovered address into it.

The chart's generated ServiceMonitor and PodMonitor show the required relabeling:

```yaml
params:
  collector: [legacy_text]
relabelings:
  - sourceLabels: [__address__]
    targetLabel: __param_target
  - sourceLabels: [__param_target]
    targetLabel: instance
  - targetLabel: __address__
    replacement: generic-http-exporter:8080
```

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
      param_tenant: [acme]   # fills {{param_tenant}} in the collector's request.path
```

The body is opaque text and does not need to be JSON. Without a `timeout` parameter, the exporter uses the incoming Prometheus scrape context as the target request timeout.

`params` cannot set `collector` or `target`: the chart renders the first from the entry's `collector`, which it checks against the configuration, and the second from each discovered target's address, so either would be a second key of the same name, and rendering fails. `interval` and `scrapeTimeout` are Prometheus durations — whole numbers of `y`, `w`, `d`, `h`, `m`, `s` and `ms`, such as `1m30s` — and the entry's `name`, which names the monitor `<fullname>-<name>`, is a unique DNS-1123 label.

## Related pages

- [REQUESTS.md](REQUESTS.md) — the request parameters a monitor's `params` map can set.
- [AUTHENTICATION.md](AUTHENTICATION.md) — monitor authentication and forwarding it to the target.
- [../charts/prometheus-universal-exporter/README.md](../charts/prometheus-universal-exporter/README.md) — every `monitors` value the chart accepts.
