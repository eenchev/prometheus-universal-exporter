# Generic HTTP Prometheus Exporter — Helm Chart Specification

This is the specification for the Helm chart: what it MUST render, expose,
validate and document. The exporter it deploys has its own specification in
[SPECIFICATION-EXPORTER.md](SPECIFICATION-EXPORTER.md), which is also where
repository-wide requirements live — purpose, implementation order, acceptance
criteria, CI gates and documentation requirements.

Section numbers are the ones this specification has always used, and they did not
change when it was split. This document therefore starts at § 29 and skips every
number belonging to the exporter, so a reference to § 33.13 or § 42.8 still points
at the same requirement it always did. A section that carried requirements for
both is present in both documents under the same number, each holding only its
own half and linking the other.

---

# 29. Prometheus Operator manifests

Provide complete examples for:

- Service deployment
- Exporter Service
- ServiceMonitor
- PodMonitor

Example ServiceMonitor pattern:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: generic-http-exporter-target
spec:
  selector:
    matchLabels:
      app: target
  endpoints:
    - port: http
      path: /probe
      params:
        collector:
          - legacy_text
      relabelings:
        - sourceLabels: [__address__]
          targetLabel: __param_target
        - sourceLabels: [__param_target]
          targetLabel: instance
        - targetLabel: __address__
          replacement: generic-http-exporter:8080
```

The example must be tested against Prometheus Operator semantics.

---

# 33. Helm chart

The repository MUST include a production-ready Helm chart for deploying the exporter to Kubernetes.

The Helm chart MUST be maintained in the same repository as the application source code and MUST be usable without manually creating Kubernetes manifests for the core deployment.

Recommended repository location:

```text
charts/prometheus-universal-exporter/
```

The chart SHOULD follow Helm conventions and include at minimum:

```text
charts/prometheus-universal-exporter/
├── Chart.yaml
├── values.yaml
├── README.md
├── templates/
│   ├── _helpers.tpl
│   ├── deployment.yaml
│   ├── service.yaml
│   ├── configmap.yaml
│   ├── serviceaccount.yaml
│   ├── servicemonitor.yaml
│   ├── podmonitor.yaml
│   └── NOTES.txt
```

Not every listed template must be rendered by default, but the chart structure MUST cleanly support the corresponding features.

### 33.1 Deployment

The chart MUST deploy the exporter as a Kubernetes `Deployment`.

The chart MUST expose the exporter's process settings as values rather than
hardcoding them in the Pod template. At minimum `server.listenAddress` MUST set
`--web.listen-address` and `server.pythonPath` MUST set `--python.path`. The
container port MUST be derived from the port in `server.listenAddress`, and a
value without a valid TCP port MUST fail rendering with a clear message. The
container port MUST keep the name `http` so Service, Ingress, ServiceMonitor,
and PodMonitor references remain valid when the port changes.
`server.pythonPath` MUST default to the interpreter path in the published
container image.

Arguments rendered into the Pod template MUST be quoted so the argument value
reaches the process exactly as configured, without literal quote characters.

Configurable values SHOULD include at least:

```yaml
image:
  repository: ...
  tag: ...
  pullPolicy: IfNotPresent

replicaCount: 1

resources:
  requests: {}
  limits: {}

service:
  enabled: true
  type: ClusterIP
  port: 8080

podSecurityContext: {}
securityContext: {}
nodeSelector: {}
tolerations: []
affinity: {}
```

The deployment SHOULD run as a non-root user where practical.

The chart MUST configure liveness/readiness probes using the exporter health endpoints.

### 33.2 Exporter configuration

Collector configuration MUST be supplied through a Helm-managed ConfigMap by default.

Example:

```yaml
config:
  enabled: true
  data:
    config.yaml: |
      collectors:
        - name: example
          request:
            type: http
          ...
```

The chart MUST mount the configuration into the exporter container using a stable path such as:

```text
/etc/prometheus-universal-exporter/config.yaml
```

The container arguments MUST reference that path.

Every key of `config.data` MUST be rendered into the ConfigMap and the whole
ConfigMap mounted as that directory, so collector files (§ 5.0 of the exporter
specification) can be supplied as further keys and listed under
`collector_files` relative to `config.yaml`. A change to any key MUST roll or
reload the exporter as a change to `config.yaml` does (§ 33.3). The chart
documentation MUST show collector files supplied this way, with a key naming
pattern that cannot match the scheduled target file rendered into the same
directory, and a test MUST load that example as the exporter would.

### 33.3 Configuration reload / rollout

The chart MUST ensure that changes to the ConfigMap eventually cause the exporter to use the new configuration.

Preferred behavior:

- If the exporter supports live configuration reload, mount the ConfigMap and configure the exporter to reload it.
- Otherwise, use a checksum annotation on the Deployment pod template so a ConfigMap change triggers a rollout.

The implementation MUST document which behavior is used.

### 33.4 Service

The chart MUST expose a `service.enabled` value that defaults to `true`. When
enabled, the chart MUST create a Kubernetes `Service` exposing the exporter
HTTP port. When disabled, the Service resource MUST NOT be rendered.

The Service MUST be usable as the target of Prometheus Operator
`ServiceMonitor` resources. The documentation MUST warn that the chart's
generated ServiceMonitor and PodMonitor routing requires this Service.

### 33.5 ServiceMonitor support

The chart MUST provide optional `ServiceMonitor` resources, selected through
entries in the shared `monitors` values array.

Example values:

```yaml
monitors:
  - name: application-services
    enabled: true
    type: service
    interval: 30s
    scrapeTimeout: 10s
    labels: {}
    annotations: {}
    targetSelector: {}
    collector: example
    params: {}
    relabelings: []
    metricRelabelings: []
```

The chart MUST allow configuring `params.collector`, user-provided
`relabelings`, and `metricRelabelings` to route discovered targets through
`/probe` and filter or rewrite scraped samples.

Because collector selection is target-specific, the chart MUST support a documented configuration pattern where a ServiceMonitor endpoint passes:

```yaml
params:
  collector:
    - example
```

and rewrites the target using:

```yaml
relabelings:
  - sourceLabels: [__address__]
    targetLabel: __param_target
  - sourceLabels: [__param_target]
    targetLabel: instance
  - targetLabel: __address__
    replacement: <exporter-service>:<port>
```

Each monitor entry MUST have a unique name and select one collector. Multiple
entries MUST be supported for different target selectors, collectors, or
scrape settings.

### 33.6 PodMonitor support

When a `monitors` entry has `type: pod`, the chart MUST provide the equivalent
optional `PodMonitor` resource using that entry's settings.

It MUST implement the equivalent target relabeling and collector parameter behavior described for `ServiceMonitor`.

### 33.7 CRD availability

The exporter Helm chart MUST NOT install Prometheus Operator CRDs itself.

If `ServiceMonitor` or `PodMonitor` is enabled and the required CRDs are absent, the chart SHOULD fail clearly or document the dependency on Prometheus Operator.

### 33.8 RBAC

The exporter does not inherently need Kubernetes API access for target discovery because target discovery is performed by Prometheus Operator.

Therefore the chart SHOULD default to no additional Kubernetes API permissions.

A dedicated ServiceAccount MAY be created for standard Kubernetes deployment conventions, but no broad `ClusterRole`/`ClusterRoleBinding` should be installed unless a future feature explicitly requires it.

### 33.9 Network and security settings

The chart SHOULD support:

- Pod security context
- Container security context
- Read-only root filesystem where practical
- Dropping Linux capabilities
- `allowPrivilegeEscalation: false`
- NetworkPolicy as an optional feature

If a NetworkPolicy is provided, it MUST account for the exporter needing to reach configured target endpoints.

### 33.10 Helm values

`values.yaml` MUST document all user-configurable settings. At minimum:

```yaml
image: {}
replicaCount: 1
nameOverride: ""
fullnameOverride: ""
resources: {}
server: {}
service: {}
config: {}
otlpTargets: {}
serviceAccount: {}
securityContext: {}
podSecurityContext: {}
nodeSelector: {}
tolerations: []
affinity: {}
monitors: []
networkPolicy: {}
```

The chart MUST provide sane production defaults and avoid hardcoding environment-specific values.

Every flag a serving exporter takes MUST be rendered from a named value rather
than left to `extraArgs`, so it is documented, validated and covered by the
values schema:

| Value | Flag |
| --- | --- |
| `server.listenAddress` | `--web.listen-address` |
| `selfMetrics.path` | `--web.self-metrics-path` |
| `server.pythonPath` | `--python.path` |
| `server.logLevel` | `--log.level`, one of `debug`, `info`, `warn`, `error`; default `info` |
| `server.probeTimeoutOffset` | `--probe.timeout-offset`, a Go duration of zero or more |
| `server.watchConfig`, `server.watchConfigInterval` | `--config.watch`, `--config.watch-interval` |
| `server.expandEnv` | `--config.export-env` |
| `otlpTargets.enabled` | `--otlp.targets-file` |

An invalid value MUST fail rendering and be refused by the values schema.
`server.probeTimeoutOffset` MUST default to empty and, while empty, MUST NOT
render its flag at all, so the exporter's own default applies and an image
older than the flag still starts. A test MUST fail when the exporter has a flag
the chart neither renders nor refuses as one-shot (§ 33.10a).

### 33.10a Extra volumes and command-line arguments

The chart mounts the ConfigMap it renders and passes the flags it derives from
its own values. Everything beyond that MUST be reachable without forking the
chart, through three values taking the ordinary Kubernetes and command-line
shapes:

- `extraVolumes` and `extraVolumeMounts`, passed through unchanged and appended
  after the volumes and mounts the chart creates. This is what mounts a
  ConfigMap or Secret the chart does not manage — an additional collector
  document, a CA bundle, a credential file — and, being the raw shapes, it
  covers projected volumes and emptyDirs as well without a value per kind.
- `extraArgs`, appended after the flags the chart renders, each entry a whole
  argument.

All three MUST default to empty, so a chart that is not asked for them renders
exactly as before.

Two collisions MUST fail rendering with a message naming the value to use
instead, because both otherwise fail at run time in a way that points somewhere
other than the values file:

- An `extraArgs` entry setting a flag the chart already renders. Go's flag
  package keeps the last occurrence, so the entry would win silently; for
  `--web.listen-address` the container port and the probes would still follow
  `server.listenAddress`, producing a pod that listens on one port while
  Kubernetes checks another. The rejected set MUST cover every flag the chart
  renders, and the message MUST name the value that controls it.
- An `extraVolumeMounts` entry whose `mountPath` is one the chart already
  mounts. A mount over the configuration directory replaces it, so the exporter
  starts with no `config.yaml` and crash-loops with an error about the missing
  file rather than about the mount that hid it. The reserved set MUST follow the
  values, so a path is reserved only while the chart actually mounts it.

An `extraArgs` entry that does not begin with `--` MUST be rejected as well: a
bare word is read as a positional argument and ignored, so it would fail by
doing nothing. So MUST every one-shot flag, which prints something and exits so
that a pod started with it would restart for ever instead of serving:
`--dry-run` (SPECIFICATION-EXPORTER.md § 30), `--config.schema`,
`--config.collector-file-schema` and `--help`.

### 33.10b Values schema

The chart MUST ship a `values.schema.json` describing every value in
`values.yaml`. Helm validates values against it on `template`, `install` and
`upgrade`, which is what turns a misspelled value into a failed render instead of
a Deployment that starts and quietly ignores what the operator asked for.

The schema MUST:

- declare a property for every key `values.yaml` sets, and set none the chart
  does not read, so the schema and the defaults describe the same chart;
- require nothing at the top level. Every value has a default, so an install
  passing no values MUST succeed;
- set `additionalProperties: false` on the top level and on the objects the
  chart defines itself, since an unknown key is a typo and rejecting it is the
  point of having a schema at all;
- constrain the values whose wrong value fails late rather than loudly: the
  enumerations the templates compare against (`image.pullPolicy`,
  `service.type`, `ingress` path types, `strategy.type`, a monitor's `type` and
  `auth.type`, `targetAuth.type`), Go durations, TCP port ranges,
  `server.listenAddress` in the same `host:port` shape the render-time check
  enforces, and the `--` prefix on an `extraArgs` entry; and
- stay open where the chart passes a raw Kubernetes shape straight through —
  `resources`, `affinity`, the security contexts, `tolerations`, `env`,
  `envFrom`, `extraVolumes`, relabelings, network policy rules — beyond the
  fields Kubernetes itself requires. Constraining those would make the chart
  reject a field Kubernetes gained, for no benefit the API server does not
  already provide.

A value the schema rejects MUST fail rendering with the schema's own message.
The schema MUST NOT be used as a reason to change the values API: a value that
is optional today stays optional.

### 33.11 Helm validation

The repository MUST include automated Helm validation covering at least:

- `helm lint`
- `helm template` with default values
- `helm template` with one enabled `monitors` entry of `type: service`;
- `helm template` with one enabled `monitors` entry of `type: pod`;
- `helm template` with multiple enabled `monitors` entries;
- `helm template` with `monitors: []`.
- `helm package`, followed by rendering the packaged chart and a client-side
  `helm install --dry-run` of it, so a chart that cannot be packaged or
  installed fails before a release attempts it rather than during one.
- `helm template` with a value the schema rejects — a wrong type, a value
  outside an enumeration, and an unknown top-level key — each of which MUST
  fail.
- `helm template` with `extraArgs`, `extraVolumes` and `extraVolumeMounts` set,
  and one rendering each for an `extraArgs` entry naming a chart-managed flag,
  an `extraArgs` entry that is not a flag, and an `extraVolumeMounts` entry at a
  mount path the chart already uses, each of which MUST fail.
- every rendered manifest starting its own YAML document, with monitors enabled
  and the self-metrics monitor rendering alongside them
- ConfigMap generation
- Deployment generation
- Service generation
- Correct `/probe` path and collector parameter configuration

If feasible, use a Kubernetes schema/testing tool such as `kubeconform` or an equivalent to validate rendered manifests.

### 33.12 Helm examples

The repository MUST include examples showing:

1. Basic exporter installation.
2. Exporter with a JSON collector.
3. ServiceMonitor targeting a Service and selecting a collector.
4. PodMonitor targeting Pods and selecting a collector.
5. Configuration containing multiple collectors.

Examples MUST not contain real credentials.

### 33.13 Chart documentation

The Helm chart README MUST document:

- Installation
- Upgrade
- Uninstallation
- Configuration values
- How to supply collectors
- ServiceMonitor usage
- PodMonitor usage
- Prometheus Operator prerequisite
- Resource/security configuration
- Configuration reload behavior
- Extra volumes, volume mounts and command-line arguments, and which collisions
  the chart rejects
- Installation and upgrade from the published OCI repository, including the
  recommendation to pin a version
- A reference listing every top-level value, its type and its default
- The features the chart deploys, described only where the exporter
  implements them

---

### 33.14 Publication and discoverability

The chart MUST be published as an OCI artifact to the registry belonging to the
repository that builds it, and MUST be installable without authenticating: a
public chart behind a login is indistinguishable, to the person running
`helm install`, from a chart that does not exist. Making the published package
public is a one-time setting on the registry that no workflow token can change,
so it MUST be documented as a manual step rather than assumed.

A traditional `index.yaml` repository MUST NOT be maintained alongside it. Two
distribution paths for one chart means two things to keep in step and one of
them silently going stale.

The chart MUST carry the metadata a package index displays and searches:

- `home` and `sources` pointing at the project;
- at least one maintainer, with a name and an email address;
- keywords, each naming a capability the exporter actually implements. A keyword
  for something it does not do is worse than a missing one, because it brings a
  reader who then leaves; and
- the Artifact Hub annotations that apply, including a category. Annotations
  whose value is itself YAML MUST parse, since nothing in Helm validates them
  and an index is left to fail on them out of sight. A security-update
  annotation MUST NOT be set unless the release actually carries one.

Ownership of the published repository is claimed through an
`.artifacthub-repo.yml` at the repository root, naming the repository ID and the
owners. The ID does not exist until the repository is registered, and a chart
must be published before it can be registered, so the file MUST be allowed to
carry a clearly marked placeholder: a release MUST NOT be blocked by it. The
release workflow MUST nevertheless verify that the file parses and names its
owners, and MUST warn when the placeholder is still in place, so a release does
not quietly publish a chart that nothing will index. The registration steps and
the replacement of the placeholder MUST be documented as manual work.

The documentation MUST NOT state that the chart is available on a package index
before it has actually been registered and indexed there.

A packaged chart MUST NOT be committed. It is build output, and a committed
`.tgz` is a second, stale definition of a released version sitting beside the
source it was built from.

---

# 34. Testing requirements

The chart's share of the test requirements. The test layers, tooling and CI
gates they run under, and every other subsection of § 34, are in
[SPECIFICATION-EXPORTER.md](SPECIFICATION-EXPORTER.md) § 34.

## 34.28 ServiceMonitor and PodMonitor tests

The repository MUST contain fixture manifests for:

- ServiceMonitor with one collector.
- ServiceMonitor with multiple collector configurations.
- PodMonitor with one collector.
- PodMonitor with multiple collector configurations.

Rendered manifests MUST be tested for:

- Correct scrape path `/probe`.
- Correct `collector` parameter.
- Correct `__param_target` relabeling.
- Correct `instance` label relabeling.
- Correct exporter `__address__` rewrite.
- Correct port/service references.
- Selector behavior.

Where practical, run an integration test against a Kubernetes test cluster and Prometheus Operator CRDs.

## 34.29 Helm tests

The Helm chart MUST be tested using:

```text
helm lint
helm template
```

with at least these values combinations:

1. Default configuration.
2. Monitor disabled.
3. Monitor enabled with `type: service`.
4. Monitor enabled with `type: pod`.
5. Custom target and metric relabelings.
6. Custom image/repository/tag.
7. Custom resources.
8. Custom securityContext.
9. Custom exporter arguments/configuration, including a custom
   `server.listenAddress` and `server.pythonPath`, and an invalid
   `server.listenAddress` that MUST fail rendering. The chart MUST require
   `host:port`: a value carrying no port, such as a bare port number or a bare
   host, renders as valid YAML and then makes the container exit immediately
   with "missing port in address", so it MUST be rejected while rendering rather
   than at run time. A port outside 1-65535 MUST be rejected too.
   `server.logLevel` and `server.probeTimeoutOffset` set MUST render
   `--log.level` and `--probe.timeout-offset`; an unknown level and a negative
   offset MUST fail rendering; the default MUST render `--log.level=info` and
   no `--probe.timeout-offset`.
9a. Scheduled targets enabled, which MUST add the `--otlp.targets-file`
   argument and render the target document into the exporter ConfigMap. When
   the chart manages the configuration, enabling scheduled targets without
   `otlp.enabled: true`, or with an empty document, MUST fail rendering with an
   explicit message rather than producing a Deployment that cannot start.
10. Multiple collectors in ConfigMap content, including collectors supplied as
    collector files in further `config.data` keys: the ConfigMap template MUST
    render every key, the configuration volume MUST mount the whole ConfigMap,
    and the documented example MUST load as the exporter would load it.
11. Existing Secret references for credentials where supported.
13. Packaging: `helm package` MUST succeed, the resulting archive MUST render
   and MUST pass a client-side `helm install --dry-run`, and no packaged chart
   MUST be present in the repository.
14. The values schema: it MUST declare exactly the keys `values.yaml` sets,
   require nothing, and reject an unknown top-level key. A wrong type and a
   value outside an enumeration MUST each fail rendering.
15. The index metadata: `Chart.yaml` MUST carry a SemVer `version` and
   `appVersion`, a description, `home`, `sources`, a maintainer with a name and
   an email address, and every advertised keyword; annotations carrying YAML
   MUST parse. `.artifacthub-repo.yml` MUST parse and name owners, accepting
   either the placeholder or a real repository ID, since it has to pass both
   before and after registration.
16. The documented install: a version pinned in a documented `helm install`
   command MUST equal the version `Chart.yaml` declares, so a chart bump cannot
   leave a reader with a command that installs something else.

12. `extraArgs`, `extraVolumes` and `extraVolumeMounts` set together, which MUST
   append the argument, the volume and the mount to the ones the chart renders
   and leave those unchanged. Further renderings MUST fail: an `extraArgs`
   entry naming a flag the chart manages, an `extraArgs` entry of `--dry-run`
   or of another one-shot flag such as `--config.schema`, an
   `extraArgs` entry that does not begin with `--`, and an `extraVolumeMounts`
   entry whose `mountPath` is one the chart already mounts. A rendering that succeeds for any of these is a
   test failure, since each produces a pod that starts and then behaves as
   though the values file said something it did not.

Every manifest a template renders MUST begin its own YAML document. A template
that renders more than one manifest, whether because it declares several or
because it wraps one in a range, MUST emit a `---` before each. `helm template`
and `helm lint` do not detect a missing separator — helm prints whatever the
template produced — so two manifests silently merge into a single document and
the chart only fails when it is applied.

Two checks MUST cover this, because neither alone is enough. A test in the Go
suite MUST verify statically that every manifest a template renders is preceded
by a separator, and the CI path filters MUST run that suite for changes under
`charts/`, since a chart-only change is exactly the change that would break it.
A CI step MUST additionally render the chart with monitors enabled and fail when
the number of manifests rendered exceeds the number of YAML documents the output
parses into.

Rendered manifests SHOULD be validated with `kubeconform`, `kubeval`, or equivalent.

Helm tests MUST verify that invalid combinations either fail rendering clearly or are rejected by chart validation.

# 42. Dedicated self-health endpoint, OTLP, and deployment controls

The dedicated self-health endpoint these values expose is specified in
[SPECIFICATION-EXPORTER.md](SPECIFICATION-EXPORTER.md) § 42.

The Helm chart MUST expose values for the self-health path and MUST provide an
optional ServiceMonitor and PodMonitor that select the exporter Service/Pods
and scrape the dedicated endpoint. The self-health monitor MUST be separate
from the target-probing monitor: a monitor that selects discovered target Pods
MUST NOT accidentally scrape the exporter health endpoint on those target
Pods.

The Helm chart MUST support:

- `namespaceOverride` for namespaced resources;
- configurable Deployment strategy, including RollingUpdate settings;
- CPU and memory requests and limits through `resources.requests` and
  `resources.limits`;
- `tolerations` and `affinity`;
- an optional Ingress resource with class, host, path, TLS, and annotations;
- optional NEG integration, implemented by a configurable Service annotation
  such as the GKE `cloud.google.com/neg` annotation.

## 42.3 Additional tests and documentation

These are the chart's share of the tests; the exporter's share is in
[SPECIFICATION-EXPORTER.md](SPECIFICATION-EXPORTER.md) § 42.3.

Tests MUST cover the configurable self-health path, separate Operator monitor
resources, namespace override, Deployment strategy, resources, scheduling
constraints, and Ingress/NEG rendering.

## 42.4 PodMonitor/ServiceMonitor header and authentication propagation

The chart MUST expose a monitor `headers` map for non-secret target headers.
Each entry MUST be encoded as a `header_<Header-Name>` endpoint parameter, which
the exporter forwards only when the selected collector lists the header in
`request.forward_headers` — see
[SPECIFICATION-EXPORTER.md](SPECIFICATION-EXPORTER.md) § 42.4 for the exporter
side and for the header names that MUST never be forwarded.

The PodMonitor API provides no arbitrary header map equivalent to Prometheus
`http_config.http_headers`, so the `header_<Header-Name>` parameter convention is
the chart-compatible bridge. Monitor header parameters MUST be documented as
non-secret: basic and bearer credentials MUST be supplied through Kubernetes
Secret references, not URL parameters.

The test suite MUST verify bearer and basic monitor rendering.

## 42.6 Independent exporter authentication and target credentials

The credential files this Secret volume produces are read by the exporter as
specified in [SPECIFICATION-EXPORTER.md](SPECIFICATION-EXPORTER.md) § 42.6.

The chart MUST default `targetAuth.enabled` to false and mount no target
credential Secret by default. It SHOULD provide an optional Secret volume
controlled by `targetAuth.enabled`, `targetAuth.type`, `targetAuth.secretName`,
and the corresponding bearer or basic-auth key/file settings, so the file path
can be declared in exporter configuration without placing credentials in a
ConfigMap.

## 42.7 Helm monitor arrays and opt-in monitor authentication

The Helm chart MUST expose a `monitors` array. Each entry MUST contain a
unique optional resource name, an `enabled` flag, and a `type` of `pod` or
`service`:

```yaml
monitors:
  - name: application-services
    enabled: true
    type: service # service or pod
```

Each enabled entry MUST render exactly one corresponding PodMonitor or
ServiceMonitor. The chart MUST support multiple enabled entries and produce
unique resource names. A self-health monitor MUST be rendered once per
monitor type used by the array when self-health monitoring is enabled.

Monitor authentication MUST be explicitly opt-in and disabled by default for
each array entry:

```yaml
auth:
  enabled: false
  type: bearer # bearer or basic
```

When `auth.enabled` is false, the chart MUST NOT render `authorization` or
`basicAuth`, regardless of the configured `auth.type`. When enabled, `auth.type`
MUST select bearer or basic authentication and the chart MUST render the
corresponding SecretKeySelectors. Tests MUST cover the disabled default, both
monitor selector types, and enabled bearer/basic authentication rendering.

## 42.8 Monitor relabeling and Deployment rollout behavior

Each Helm `monitors` entry MUST expose native Prometheus Operator
`relabelings` and `metricRelabelings` lists. The chart MUST preserve its
mandatory target-routing relabelings and append user-provided entry
`relabelings` after them. Entry `metricRelabelings` MUST be rendered on the
selected ServiceMonitor endpoint or PodMonitor pod metrics endpoint.
These lists MUST support the standard fields, including `sourceLabels`,
`targetLabel`, `regex`, `replacement`, `action`, and `modulus`, as applicable.

The self-health monitor MUST independently support
`selfMetrics.relabelings` and `selfMetrics.metricRelabelings`.

The default Deployment strategy MUST work with `replicaCount: 1`. The default
`RollingUpdate` settings MUST use explicit `maxUnavailable: 0` and
`maxSurge: 1`, keeping the old ready Pod until the replacement is ready and
temporarily allowing two Pods. The chart MAY accept percentage values as
supported by Kubernetes, but MUST document their rounding behavior. Users that
require no overlap MAY choose `strategy.type: Recreate`.

## 42.10 Per-scrape request overrides

The parameters themselves, and what the exporter does with each, are specified in
[SPECIFICATION-EXPORTER.md](SPECIFICATION-EXPORTER.md) § 42.10.

The chart MUST expose these parameters as list-valued `params` entries on
each `monitors` item, and MUST expose `interval` and `scrapeTimeout` on each
item as the Prometheus Operator scrape settings. The monitor scrape timeout
and the exporter target-request timeout override are distinct: the former is
set on the generated ServiceMonitor or PodMonitor, while the latter is passed
to `/probe` as `params.timeout`. The TLS override is passed as
`params.insecure_skip_verify`; when absent, the collector's TLS setting MUST be
preserved. `params.retry_attempts` and `params.retry_backoff` override the
collector's retry settings for that scrape; when absent, the collector values
MUST be preserved.

A monitor's `params` MUST also pass `param_<name>` entries through unchanged,
since they fill the `{{param_<name>}}` placeholders of the collector's
`request.path` (SPECIFICATION-EXPORTER.md § 42.10a). The values schema MUST NOT
restrict `params` to a fixed set of keys for the same reason.

## 42.11 Helm-wide default metadata

The Helm chart MUST expose `defaultLabels` and `defaultAnnotations` maps. The
chart MUST apply them to the metadata of every Kubernetes object it creates,
including the Deployment Pod template and conditionally rendered ConfigMap,
ServiceAccount, Service, Ingress, NetworkPolicy, ServiceMonitor, and
PodMonitor resources. Resource-specific metadata maps MUST be applied after
the defaults and therefore MUST override a same-named default. The chart's
generated identity labels and required operational annotations MUST remain
valid and authoritative where they conflict with user defaults.

## 42.12 CI, container, and chart releases

The release tag contract both workflows share, and the exporter's own release
workflow, are specified in
[SPECIFICATION-EXPORTER.md](SPECIFICATION-EXPORTER.md) § 42.12.

The chart release workflow MUST be triggered by tags under `chart` and MUST:

- verify that the version implied by the tag matches the `version` field in
  `Chart.yaml`, failing the release when they disagree;
- re-run the chart lint and template scenarios;
- package the chart without overriding `version` or `appVersion`, so the
  published artifact carries exactly what the committed `Chart.yaml` declares;
- publish the chart as an OCI artifact; and
- create a GitHub Release containing the chart archive.

`Chart.yaml` MUST be the source of truth for the chart version. `appVersion`
records the exporter release a chart version was validated against and MUST be
maintained by hand rather than derived from a release tag.

## 42.15a Environment variable expansion in configuration

The expansion itself is specified in
[SPECIFICATION-EXPORTER.md](SPECIFICATION-EXPORTER.md) § 42.15a.

The chart MUST expose the flag, and MUST provide `env` and `envFrom` in the
ordinary Kubernetes shapes, because the flag is inert without a way to set
variables in the container: the material an operator wants to keep out of a
committed file generally lives in a Secret.
