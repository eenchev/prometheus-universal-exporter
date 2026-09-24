# Authentication

There are two credentials in play and they are deliberately separate: the one
Prometheus uses to scrape the exporter, and the one the exporter uses to reach
the target. Mixing them is the mistake this design exists to prevent, so the
exporter refuses a configuration that tries.

## Reaching the target

Monitor authentication is applied by Prometheus when it scrapes the exporter. To pass that credential to the discovered target, set `request.forward_authorization: true` on the selected collector. Each `monitors` entry supports Secret-backed `auth.type: bearer` and `auth.type: basic` settings. The exporter never forwards arbitrary incoming headers.

For non-secret target headers, configure an allowlist in the collector and use the chart's monitor `headers` map. The chart encodes these as `header_<Header-Name>` probe parameters, which the exporter forwards only when the header is listed in `request.forward_headers`:

```yaml
# exporter config
collectors:
  - name: tenant_status
    request:
      type: http
      path: /status
      forward_authorization: true
      forward_headers: [X-Tenant]
```

```yaml
# Helm values
monitors:
  - name: application-services
    enabled: true
    type: pod
    headers:
      X-Tenant: team-a
    auth:
      enabled: true
      type: bearer
      secretName: target-api-token
      secretKey: token
```

Header values in monitor parameters are not suitable for secrets. Monitor authentication is disabled by default; set an entry's `auth.enabled: true` and use its `auth` block with a Kubernetes Secret for bearer/basic authentication. Explicitly opt in per collector before forwarding the incoming Authorization header.

The exporter endpoints can also be protected with exporter-side Basic Auth:

```yaml
web:
  basic_auth:
    enabled: true
    username: exporter
    password: change-me
```

The credential can be read from files instead, so it stays out of the
configuration — with the Helm chart the configuration is a ConfigMap, which is
no place for a password:

```yaml
web:
  basic_auth:
    enabled: true
    username: exporter                # or username_file
    password_file: /var/run/prometheus-universal-exporter/web-auth/password
```

Set one of `username` and `username_file`, and one of `password` and
`password_file`. Leading and trailing whitespace, such as the newline an
editor adds, is removed. A file that is missing or empty when the
configuration loads is refused like any other invalid configuration. A file
changed on disk is read again by the next request, so a rotated Kubernetes
Secret takes effect without a restart; one that can no longer be read refuses
every protected request with `500` and logs why, rather than letting requests
in. The chart mounts the Secret with `webAuth` — see the
[chart README](../charts/prometheus-universal-exporter/README.md#exporter-authentication).

When enabled, Basic Auth is required for `/probe`, `/metrics`, the configured self-metrics endpoint and the landing page at `/`, which lists the collectors. `/health` and `/ready` remain unauthenticated for Kubernetes probes. Exporter-side Basic Auth is mutually exclusive with `request.forward_authorization`; enable one model or the other so the incoming Authorization header cannot be confused with the exporter credential.

This conflict is rejected during startup: the exporter logs `invalid startup configuration; exiting` and terminates with a non-zero exit code. Invalid configurations detected during file reload are rejected while the last valid configuration remains active.

## Credentials from a Kubernetes Secret

To use exporter Basic Auth and a Kubernetes Secret for target credentials at the same time, disable the bridge and configure a mounted credential file. `targetAuth.enabled` is `false` by default; for basic auth:

```yaml
web:
  basic_auth:
    enabled: true
    username: exporter
    password: change-me

collectors:
  - name: protected_status
    request:
      type: http
      path: /status
      basic_auth_file:
        username: /var/run/prometheus-universal-exporter/target-auth/username
        password: /var/run/prometheus-universal-exporter/target-auth/password
```

For the Helm chart, set `targetAuth.enabled: true`, `targetAuth.type: basic`, `targetAuth.secretName`, `usernameKey`, and `passwordKey`. The mounted Secret is read by the exporter and sent as HTTP Basic Auth to the underlying endpoint. For bearer auth, use `type: bearer`, `secretKey`, `fileName`, and `request.bearer_token_file`. The selected monitor can independently use its `auth.type: basic` to authenticate its scrape of the exporter; `request.forward_authorization` must remain `false`.
