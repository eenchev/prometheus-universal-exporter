# Prometheus Universal Exporter — Helm Chart Specification

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
  name: prometheus-universal-exporter-target
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
          replacement: prometheus-universal-exporter.monitoring.svc:8080
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

The chart MUST deploy the exporter as a Kubernetes `Deployment`, with the
target allocator too (§ 33.10c).

The chart MUST expose the exporter's process settings as values rather than
hardcoding them in the Pod template. At minimum `server.listenAddress` MUST set
`--web.listen-address` and `server.pythonPath` MUST set `--python.path`. The
container port MUST be derived from the port in `server.listenAddress`, and a
value without a valid TCP port MUST fail rendering with a clear message; its
examples of a valid value MUST NOT include a loopback address. A loopback host
— `127.x.x.x`, `localhost` or `[::1]` — MUST fail rendering too: the exporter
would start, and neither the kubelet's probes nor the Service would reach it.
The
container port MUST keep the name `http` so Service, Ingress, ServiceMonitor,
and PodMonitor references remain valid when the port changes.
`server.pythonPath` MUST default to the interpreter path in the published
container image. Where the chart README names that image's Python base, it
MUST name the release the Dockerfile pins as `PYTHON_VERSION`, and a test MUST
fail when the two differ, so a Dockerfile update cannot leave the README
describing another interpreter.

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
podLabels: {}
podAnnotations: {}
priorityClassName: ""
nodeSelector: {}
tolerations: []
affinity: {}
topologySpreadConstraints: []
```

The deployment MUST run as a non-root user: `podSecurityContext` MUST set
`runAsNonRoot`, and `runAsUser`, `runAsGroup` and `fsGroup` to the image's
numeric user, 65532, since Kubernetes can verify `runAsNonRoot` only for a
numeric user.

The chart MUST configure liveness/readiness probes using the exporter health endpoints: liveness on `/health` and readiness on `/ready`, which reports a shutdown and, when `otlp.unready_after_failures` asks for it, failing OTLP exports, but not a rejected reload (SPECIFICATION-EXPORTER.md § 23).
Their timings MUST come from the `livenessProbe` and `readinessProbe` values,
defaulting to `periodSeconds: 10`, `timeoutSeconds: 3` and a `failureThreshold`
of 5 for liveness and 3 for readiness, so a pod busy with a burst of probes is
not restarted as dead. The check itself is the chart's: a value setting
`httpGet`, `exec`, `tcpSocket` or `grpc` MUST fail rendering.

With `goMemLimit.enabled`, the default, the chart MUST render
`--runtime.memory-limit-ratio` with `goMemLimit.ratio`, `0.8` by default,
from 0.1 to 1, or fail rendering, refused by the values schema and, with it
skipped, by the templates. A ratio above 0 and below 0.1 MUST fail with a
message saying the floor and why: the Go memory limit would leave the heap
almost nothing and the Go runtime would spend its time collecting garbage,
and the exporter refuses such a flag (SPECIFICATION-EXPORTER.md § 30). A
number MUST be written out, without an exponent, however small, so the
templates' message refusing `1e-5`, which is seen with the values schema
skipped, says `0.00001`, never `1e-05`. Its digits MUST be
those Helm's `toJson` gives it, so every number a template printed without
an exponent before renders as it did. A string MUST be checked as written,
and the values schema's pattern for it takes no exponent.

`goGC.percent` is the Go garbage collector's target. Empty, the default, or
null, the chart MUST render nothing for it, so Go's default, 100, stays, and
a release that does not set it MUST render as it did without the value. Set,
it MUST be rendered as the `GOGC` environment variable of the exporter
container, its value a string, before the entries of `env` and with
`envFrom` unchanged; the exporter takes no flag for it, since the Go runtime
reads the variable itself. It MUST be a whole number from 1 to 10000, given
as a number or as a string, or the string `off`; anything else MUST be
refused by the values schema and, with the schema skipped, MUST fail
rendering with a message saying what it may be. A boolean is among what is
refused: YAML reads an unquoted `off` as `false`, which could as well mean
"leave it alone", so `off` MUST be written in quotes in a values file, and
`values.yaml` and the chart README MUST say so.

The variable MUST have one source: a `GOGC` entry in `env` beside a
`goGC.percent` MUST fail rendering with a message naming both, since
Kubernetes accepts a name twice and keeps the last. A `GOGC` entry in `env`
with no `goGC.percent` MUST be rendered as given.

`goGC.percent: off` collects nothing until the Go memory limit is near, so
it MUST fail rendering, with a message naming `goGC.percent` and the value
that leaves the limit out, unless a Go memory limit is in force. One is in
force when `goMemLimit.enabled` renders the ratio and
`resources.limits.memory` is set, which is the limit the exporter takes its
share of; and, since `GOMEMLIMIT` in the environment wins over the ratio in
the exporter (SPECIFICATION-EXPORTER.md § 30), when `env` holds a
`GOMEMLIMIT` entry, the operator's own limit, unless its value is `off`,
which sets no limit and wins over the ratio all the same; an entry with an
empty value is no entry to the exporter, and none here. A number MUST
render with or without a memory limit. Changing `goGC.percent` changes the
pod template and so rolls the pods (§ 33.3); it MUST NOT need an annotation
of its own.

The chart MUST offer an optional PodDisruptionBudget, `podDisruptionBudget`,
disabled by default, selecting the Deployment's pods with `minAvailable` or
`maxUnavailable`, `maxUnavailable: 1` when neither is set; setting both MUST
fail rendering. A value MUST be rendered as set, `maxUnavailable: 0`
included, which Kubernetes accepts though it allows no voluntary eviction;
the notes MUST warn about `0` and `0%`. They MUST warn as well about a
`minAvailable` that leaves no pod to evict: without autoscaling, a count of
`replicaCount` or more, or a percentage that, rounded up as Kubernetes rounds
it, is `replicaCount` or more, while `replicaCount` is above zero; and `100%`
or more whatever the replicas, autoscaling included. With autoscaling the
replica count is not the chart's to know, so no other `minAvailable` is warned
about.

`service.sessionAffinity`, empty by default, MUST be rendered on the Service
when set, `None` or `ClientIP`, and the chart MUST document that each replica
keeps its own response cache, so a probe's cache hits fall as replicas are
added unless one Prometheus's probes reach one pod.

The chart MUST offer an optional `autoscaling/v2` HorizontalPodAutoscaler,
`autoscaling`, disabled by default, scaling the Deployment on CPU utilization
(80% by default), memory utilization, the exporter's own load, its static
targets per pod, further metrics or none of these but not all absent, with
`behavior` passed through; with it the Deployment MUST NOT render `replicas`.
`maxReplicas` below `minReplicas`, or nothing to scale on, MUST fail
rendering. `autoscaling.targetBusy`, null by default, MUST render a `Pods`
metric named `autoscaling.busyMetric` (`http_exporter_busy` by default) with
an `AverageValue` target of it, quoted: a number above 0, rendered as JSON
writes it, or a decimal quantity with an optional `m`; zero, a negative
number or any other text MUST fail rendering. `autoscaling.targetStaticTargetsPerPod`,
null by default, a whole number from 1 to 2147483647, MUST render a `Pods`
metric named `autoscaling.staticTargetsMetric` (`http_exporter_static_targets`
by default) the same way, and MUST fail rendering without `allocator.enabled`,
since without the allocator every pod scrapes every static target and their
number per pod does not fall as pods are added. Either target with its metric
name empty MUST fail rendering. The chart README MUST say that a metrics
adapter serves the two metrics, and link the self-metrics documentation's
prometheus-adapter rules and KEDA example. Every example query of an
allocator metric there, and every prometheus-adapter rule over one, MUST take
the `max` of the series, which every allocator copy reports, so the targets
are not counted once per copy and KEDA gets one series.

Without the allocator every replica scrapes every static target, so the chart MUST document running
static targets with one replica and without autoscaling: each added replica
contacts every target again, and exports every target with `export_via_otlp`
again, as duplicate series. Its notes MUST warn when `staticTargets.enabled`
is rendered with `replicaCount` above 1 or with autoscaling, naming the
targets exported over OTLP.

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
`collector_files` relative to `config.yaml`. A key of `config.data` that
Kubernetes refuses as a ConfigMap key — anything but letters, digits, `-`,
`_` and `.`, longer than 253 characters, or `.` or `..` — MUST fail
rendering, whether or not `config.enabled` is on, naming the key and what a
key may hold and saying that the directory is flat, so a file goes in under a
name without `/` or in a directory mounted through `extraVolumes`; the
values schema MUST refuse the same keys. A change to any key MUST roll or
reload the exporter as a change to `config.yaml` does (§ 33.3). Every file
the chart renders into a ConfigMap — each key of `config.data` and the
static target document — MUST reach the pod byte for byte as it was given,
under a key rendered as a string: the key MUST be quoted, so a name YAML
reads as a number or a boolean stays a key, and the content MUST NOT depend
on the indentation of its first line, so a file starting with an indented
line, which a block scalar without an indentation indicator ends at the
first line indented less, MUST render. A file SHOULD stay readable text in
the rendered ConfigMap; one that YAML text cannot hold unchanged — with
control characters, carriage returns or other line breaks YAML rewrites, a
byte order mark, bytes that are not UTF-8, or anything but one line break or
none after its last visible character — MUST be rendered under `binaryData`
instead, which the pod mounts as the same file. The checksum annotation
(§ 33.3) MUST change with any such file, however it is rendered. The chart
documentation MUST show collector files supplied this way, and a test MUST
load that example as the exporter would.

The static target document (`staticTargets`) MUST NOT be a file of the
configuration directory, where a `collector_files` pattern such as `*.yaml`
would match it and the exporter would refuse to start reading it as a
collector file. The chart MUST render it into a ConfigMap of its own,
`<fullname>-static-targets`, whether or not `config.enabled` is on, mount
that read-only at `/etc/prometheus-universal-exporter-static-targets` and
pass `--static-targets-file` with the file there. No other file the chart
mounts is in the configuration directory either: the credentials and
`known_hosts` are in their own `mountPath`s, which may not be inside it
(§ 33.10a).

The chart MUST NOT depend on the collectors' request types: an `http`,
`localfile`, `graphite` or `grpc` collector is configured in `config.data`
alone, and a monitor's `params` reach any of them as `/probe` parameters. The
chart documentation MUST say how a `graphite` collector is monitored: a
monitor selecting the Graphite Service, whose address becomes the target; and
how a `grpc` collector is: a monitor selecting the gRPC server's Service,
whose `host:port` address becomes the target, with descriptor files, when the
collector reads any, mounted through `extraVolumes` and `extraVolumeMounts`.

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
`service.type` MUST be `ClusterIP`, `NodePort` or `LoadBalancer`, and the values
schema MUST refuse `ExternalName`, a DNS alias with no endpoints through which
nothing would reach the exporter.

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
    namespaceSelector: {}
    port: http
    collector: example
    params: {}
    relabelings: []
    metricRelabelings: []
```

The chart MUST render the endpoint's `params.collector` from the entry's
`collector`, and allow user-provided `relabelings` and `metricRelabelings`, to
route discovered targets through `/probe` and filter or rewrite scraped
samples. An entry's `params` MUST NOT set `collector` or `target`, which the
chart renders itself — the collector from `collector`, checked against
`config.data` (§ 42.7), and the target from each discovered target's address
through the relabeling below: either would render a second key of that name
and bypass the collector check, so rendering MUST fail with a message pointing
to the entry's `collector` (and `targetSelector` for `target`).

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
    replacement: <fullname>.<namespace>.svc:<service.port>
```

Each monitor entry MUST have a unique name and select one collector. Multiple
entries MUST be supported for different target selectors, collectors, or
scrape settings. An entry renders a monitor named `<fullname>-<name>`, so the
name MUST be a DNS-1123 label — lower-case letters, digits and `-`, starting
and ending with a letter or digit, at most 63 characters — which the values
schema MUST enforce; two entries of one name, or an entry
named `self` or `static-targets`, the suffixes of the chart's own monitors,
MUST fail rendering, since the second object would replace the first.

An entry's `port` MUST be rendered as its endpoint's `port`, quoted, and
`http` when the entry has none: the name of the port whose address becomes
the probe's target, a port of the selected Services for `type: service` and a
container port of the selected pods for `type: pod`, so targets that name
their port otherwise, a gRPC server's `grpc` port among them, can be probed.
A `port` is a port's name and never its number, as the Prometheus Operator's
`port` field is, which looks the value up among the ports' names: a number
rendered as a name matches no port and leaves the monitor without targets.
So a `port` MUST be lower-case letters, digits and `-` with at least one
letter and no `-` first or last, of at most 63 characters for `type: service`
and of at most 15 with no `--` for `type: pod`. The lengths and the
characters are Kubernetes' rules for the name of a Service port and of a
container port, and the letter is its rule for a container port only: for
`type: service` the letter is the chart's own rule, made so that a number
written as a name (`"9115"`) is not rendered as a port name that matches
nothing. Kubernetes itself allows a Service port to be named in digits alone
(`name: "8080"`); the chart does not take such a name, since it cannot be told
from a number given by mistake, so a Service whose port is named in digits
alone cannot be monitored through this value until its port has a name with a
letter. The chart documentation and the description of `port` in the values
schema MUST say that the letter is the chart's rule for a Service port and
not Kubernetes', and what follows for such a Service. The values schema MUST
enforce the grammar, and the templates MUST enforce it themselves when the
schema is skipped, failing with a message that names the monitor and, for a
number — written as one or as a string of digits — says to name the port on
the Services or the pods and give that name. The chart MUST NOT render a port
number: a PodMonitor's
`portNumber` is unknown to the CRDs of Prometheus Operator releases before
0.79, and `targetPort` is deprecated on a PodMonitor and on a ServiceMonitor
is a container port of the pods behind the Service, not the Service's own.
The chart documentation MUST say where a port is given its name, on a Service
and on a pod.

An entry's `namespaceSelector` MUST be rendered as the monitor's
`spec.namespaceSelector`, as the Prometheus Operator defines it — `any`, a
boolean, or `matchNames`, a list of namespace names — so targets outside the
monitor's own namespace can be found; without one, none MUST be rendered. The
values schema MUST refuse a `namespaceSelector` with any other key. The
chart documentation MUST name the port in its description of monitoring a
`grpc` collector.

Without `targetSelector` the monitor MUST select targets labelled
`app.kubernetes.io/name: target`; with it, the selector as given. Either way
the rendered monitor MUST parse with `spec.selector` a mapping holding its
keys: a template that trims the newline after `selector:` renders
`selector:matchLabels:`, one key of that name, which `helm template` and
`helm lint` accept and the Prometheus Operator reads as a monitor selecting
everything.

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

`kubernetes` collectors list objects with the exporter's own ServiceAccount, which `kubernetesAccess` grants, off by default. With `kubernetesAccess.enabled`, the exporter's pods and its ServiceAccount MUST set `automountServiceAccountToken: true`, whatever `serviceAccount.automount` says, and the chart MUST render, allowing the verb `list` alone on the `apiGroups` and `resources` of each entry of `kubernetesAccess.rules`, in that order, to the ServiceAccount the pods run as, in the release's namespace: a Role and a RoleBinding `<fullname>` in each namespace of `kubernetesAccess.namespaces`, in the release's namespace when it lists none; or, with `kubernetesAccess.clusterWide`, a ClusterRole and a ClusterRoleBinding `<namespace>-<fullname>` in their place. Rendering MUST fail, by the values schema and, with the schema skipped, by the templates, naming `kubernetesAccess`, when it is enabled with no rules; when a rule's `apiGroups` or `resources` is empty or holds `*`; when a rule whose `apiGroups` holds `""` names `secrets`, which no `kubernetes` collector may list; and when `clusterWide` is set beside `namespaces`. While it is enabled, the service account token directory, `/var/run/secrets/kubernetes.io/serviceaccount`, MUST be among the paths no `extraVolumeMounts` entry may be at or below. Without it nothing of it MUST be rendered.

With `serviceAccount.create`, true by default, the chart MUST render a ServiceAccount named `serviceAccount.name`, `<fullname>` when it is empty, carrying `serviceAccount.annotations`, and the exporter's pods MUST run as it. With `serviceAccount.create: false` the chart MUST NOT render a ServiceAccount, and the pods, and the `kubernetesAccess` bindings, MUST name the existing account `serviceAccount.name` names, `default` included when it is written out. Rendering MUST fail, by the values schema and, with the schema skipped, by the templates, when `serviceAccount.create` is false and `serviceAccount.name` is empty, the template's message naming `serviceAccount.name`, rather than falling back on the namespace's `default` account, which other workloads share and `kubernetesAccess` would grant to; and when `serviceAccount.create` is false and `serviceAccount.annotations` is not empty, the template's message saying that annotations apply only to an account the chart creates and belong on the existing account, since the chart never changes an account it did not create.

### 33.9 Network and security settings

The chart SHOULD support:

- Pod security context
- Container security context
- Read-only root filesystem where practical
- Dropping Linux capabilities
- `allowPrivilegeEscalation: false`
- NetworkPolicy as an optional feature

If a NetworkPolicy is provided, it MUST account for the exporter needing to reach configured target endpoints. `networkPolicy.enabled` alone MUST limit ingress to the `http` port, from anywhere, and MUST NOT limit egress, since the targets probes name are not known to the chart; `ingress` rules MUST replace that ingress default; `egress` rules MUST limit egress to them, and add DNS, port 53 over UDP and TCP, unless `networkPolicy.allowDNS` is false.

Objects MUST be named `<release>-<chart>`, or after the release alone when its name contains the chart's, so two releases in one namespace do not collide; `fullnameOverride` MUST name them outright. The pod MUST set `automountServiceAccountToken` from `serviceAccount.automount`, off by default, whichever account it runs as, but for `kubernetesAccess` (§ 33.8).

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
staticTargets: {}
serviceAccount: {}
securityContext: {}
podSecurityContext: {}
podLabels: {}
podAnnotations: {}
priorityClassName: ""
nodeSelector: {}
tolerations: []
affinity: {}
topologySpreadConstraints: []
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
| `selfMetrics.path` | `--web.self-metrics-path`, a path of plain segments no other endpoint uses; the values schema refuses one the exporter would |
| `server.pythonPath` | `--python.path` |
| `server.logLevel` | `--log.level`, one of `debug`, `info`, `warn`, `error`; default `info` |
| `server.probeTimeoutOffset` | `--probe.timeout-offset`, a Go duration of zero or more |
| `server.probeDefaultTimeout` | `--probe.default-timeout`, a Go duration of zero or more |
| `server.probeMaxConcurrent` | `--probe.max-concurrent`, a whole number from 0 to 2147483647, as a number or as a string of digits with no zero before another digit; empty renders no flag |
| `server.pythonMaxWorkers` | `--python.max-workers`, a whole number from 0 to 2147483647, as a number or as a string of digits with no zero before another digit; empty renders no flag |
| `server.shutdownTimeout` | `--web.shutdown-timeout`, whole hours, minutes and seconds such as `30s` or `1m30s`, positive |
| `server.shutdownDelay` | `--web.shutdown-delay`, whole hours, minutes and seconds such as `5s`, `0s` allowed; default `5s`, and empty renders no flag |
| `server.enableLifecycle` | `--web.enable-lifecycle`, rendered only when `true`; default `false` |
| `server.debug` | `--web.debug=<level>`, rendered only for `edit` or `full`; empty, the default, and `off` render no flag, and any other value MUST fail rendering, by the schema's enum and in the template |
| `server.watchConfig`, `server.watchConfigInterval` | `--config.watch`, `--config.watch-interval` |
| `server.expandEnv` | `--config.expand-env` |
| `staticTargets.expandEnv` | `--static-targets.expand-env`, rendered only when `true` and `staticTargets.enabled` |
| `staticTargets.enabled`, `staticTargets.fileName` | `--static-targets-file=/etc/prometheus-universal-exporter-static-targets/<fileName>`, rendered only when enabled (§ 33.2) |
| `staticTargets.path` | `--web.static-targets-path`, always rendered; a path of plain segments that is neither `selfMetrics.path` nor another endpoint's; default `/static-targets` |
| `goMemLimit.enabled`, `goMemLimit.ratio` | `--runtime.memory-limit-ratio`, rendered only when enabled (§ 33.1) |
| `config` | `--config.file`, the chart's mount path of `config.yaml` (§ 33.2), rendered unless `allocator.enabled` |
| `allocator.enabled` | `--allocator.url`, `--allocator.cache-dir`, `--allocator.member`, `--allocator.advertise`, rendered only when enabled (§ 33.10c) |
| `allocator.tokenSecret.name`, `allocator.tokenSecret.key` | `--allocator.token-file`, rendered whenever `allocator.enabled`, naming the generated Secret's token when the name is empty (§ 33.10c) |
| `allocator.staticTargets.expandEnv` | `--static-targets.expand-env`, rendered only when `true` and `allocator.enabled` (§ 33.10c) |

An invalid value MUST fail rendering and be refused by the values schema.

A whole number of the values MUST be rendered as the whole number it is,
written out in digits, however it was written and however Helm hands it to
the templates. Helm reads a number of a values file, and of `--set-json`, as
floating point, and a template prints a floating-point number of a million or
more with an exponent, so `2000000`, `2e6` and `2000000.0` MUST all render
`2000000`, and never `2e+06`, wherever the chart renders the value: in a
flag, a count, a number of seconds and a port alike. The templates MUST
decide what a whole number is by its value, not by how a template prints it.

The whole numbers the templates print themselves each have a range:
`replicaCount`, `terminationGracePeriodSeconds`, `server.probeMaxConcurrent`,
`server.pythonMaxWorkers`, and a `podDisruptionBudget.minAvailable` or
`maxUnavailable` given as a number, from 0 to 2147483647;
`autoscaling.minReplicas`, `maxReplicas`,
`targetCPUUtilizationPercentage` and `targetMemoryUtilizationPercentage`
from 1 to 2147483647; and `service.port` from 1 to 65535. 2147483647 is what
a Kubernetes count holds, and the most the exporter's count flags take on
every platform; the exporter takes a larger count on a 64-bit platform
(SPECIFICATION-EXPORTER.md § 30), and Kubernetes a longer grace period, and
the chart MUST NOT. A fraction, and a
number outside its range — one too large for a template to hold as a whole
number among them, which MUST NOT be rendered as another number — MUST be
refused by the values schema, which MUST carry each maximum, and, with the
schema skipped, MUST fail rendering with a message that names the value,
gives the range and writes the number out, in digits when it is below
10^21 and as Helm's `toJson` prints it from there. A count flag given as a
string MUST be a string of digits within the same range with no zero before
another digit, since the exporter's flag reads `010` as eight and refuses
`08`, and MUST be rendered as it is written; the schema's pattern for it
MUST be the templates'. A `podDisruptionBudget.minAvailable` or
`maxUnavailable` given as a string is rendered bare, so a string of digits
becomes the number, and MUST be a whole number in the same range with no
zero before another digit, since YAML reads `010` as the octal number eight,
or a percentage from `0%` to `100%` written the same way, since Kubernetes
refuses a budget's percentage past 100%; anything else MUST be refused by
the values schema, whose pattern MUST be the templates', and, with the
schema skipped, MUST fail rendering with a message naming the value and
saying what it may be. A value left out or null MUST render nothing for the
number, which is then Kubernetes' to default.

A message that names a number of the values — a grace period too short for
the shutdown, a monitor's `port` given as its number, a `goGC.percent` out of
range — MUST write it out too, and the notes MUST compare a budget's
`minAvailable` with the replica count as the numbers they are. The whole
numbers inside the Kubernetes shapes the chart passes through (§ 33.10b) — a
security context's `runAsUser`, `runAsGroup` and `fsGroup`, the probes'
timings, `resources`, `strategy.rollingUpdate`, tolerations, relabelings —
MUST stay written out in digits, as `toYaml` writes every whole number of
up to eighteen digits, a user ID such as `1000680000` included, and the
chart MUST hand each such shape to `toYaml` rather than print a number of
it itself. The templates do not bound them. The values schema MUST bound
the probes' timings and the rolling update's counts as Kubernetes does, so
a value Kubernetes would refuse when the Deployment is applied fails at
rendering instead: each timing a whole number up to 2147483647, from 0 for
`initialDelaySeconds` and from 1 for the others, `successThreshold` 1 on
the liveness probe, the one value Kubernetes takes there; `maxSurge` and
`maxUnavailable` a whole number from 0 to 2147483647, or a percentage with
no zero before another digit, of at most 2147483647 for `maxSurge` and at
most `100%` for `maxUnavailable`, as Kubernetes bounds it; and neither a
string of digits alone, which `toYaml` renders as a string and Kubernetes
refuses. Both rendered as 0, as `0` or `0%` in either, MUST fail rendering,
refused by the schema and, with it skipped, by the templates with a message
naming both keys, since Kubernetes refuses a rolling update that could
neither add a pod nor take one away; a count left out is not rendered and
is Kubernetes' default, 25%, and with `Recreate` neither is rendered. The
other shapes are Kubernetes' to bound.

`server.probeTimeoutOffset`, `server.probeDefaultTimeout`, `server.probeMaxConcurrent`, `server.pythonMaxWorkers` and `server.shutdownTimeout` MUST default to empty
and, while empty, MUST NOT render their flags at all, so the exporter's own default applies and an image
older than the flag still starts. A test MUST fail when the exporter has a flag
the chart neither renders nor refuses as one-shot (§ 33.10a). The chart
README's flag table MUST list every flag the chart renders, and its
description of the one-shot flags every flag the chart refuses as one; a test
MUST compare that table, that description and the table above with the flags
the exporter defines and the chart renders, and fail when any is incomplete.

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
  values, so a path is reserved only while the chart actually mounts it: the
  configuration directory always, the static target directory while
  `staticTargets.enabled` is on, the allocator cache directory while
  `allocator.enabled` is on and the allocator token directory while a token
  is set (§ 33.10c), and `targetAuth.mountPath`,
  `webAuth.mountPath`, `sshAuth.mountPath` and `sqlAuth.mountPath` while
  each is enabled.

The chart's own mounts MUST NOT collide either, since a pod cannot mount two
volumes at one path: an enabled `targetAuth.mountPath`, `webAuth.mountPath`,
`sshAuth.mountPath` or `sqlAuth.mountPath` that is the configuration
directory, or two of them
being one path, MUST fail
rendering, naming the value and what it collides with, and so MUST two
`extraVolumeMounts` entries of one `mountPath`. Every one of these
comparisons MUST ignore a trailing slash, so `/etc/prometheus-universal-exporter/`
collides with `/etc/prometheus-universal-exporter`.

The configuration directory, the static target directory while
`staticTargets.enabled` is on, and `targetAuth.mountPath`,
`webAuth.mountPath`, `sshAuth.mountPath` and `sqlAuth.mountPath` while each
is enabled, are
ConfigMap, Secret and projected volumes, which Kubernetes mounts read-only, so no mount point can be created in them
and a mount below one leaves the container unstarted (CreateContainerError).
An `extraVolumeMounts` entry, a `subPath` file among them, or an enabled
credential's `mountPath` below one of them, and an enabled credential's
`mountPath` above the configuration or static target directory, MUST fail
rendering, naming the
value and the directory to mount outside of; the paths MUST be compared
cleaned, so doubled and dotted segments hide nothing. The values schema MUST
refuse such a path below the configuration directory where its pattern can
tell, and only where the templates refuse it too: an `extraVolumeMounts`
entry always, a credential's while it is enabled.

An `extraArgs` entry that does not begin with `--` MUST be rejected as well: a
bare word is read as a positional argument and ignored, so it would fail by
doing nothing. So MUST every one-shot flag, which prints something and exits so
that a pod started with it would restart for ever instead of serving:
`--dry-run` (SPECIFICATION-EXPORTER.md § 30), `--config.schema`,
`--config.collector-file-schema`, `--static-targets-file-schema`, `--version`
and `--help`.

### 33.10b Values schema

The chart MUST ship a `values.schema.json` describing every value in
`values.yaml`. Helm validates values against it on `template`, `install` and
`upgrade`, which is what turns a misspelled value into a failed render instead of
a Deployment that starts and quietly ignores what the operator asked for.

The schema MUST:

- declare a property for every key `values.yaml` sets, and set none the chart
  does not read, so the schema and the defaults describe the same chart. Two
  properties are the exception, since Helm passes them to a chart used as a
  dependency of another and a root schema that refuses unknown keys would
  otherwise refuse every parent chart: `global`, any object, which holds the
  parent's global values and is an empty map when it has none, and `enabled`,
  a boolean, the key a dependency's `condition` conventionally reads. The
  schema MUST allow both, `values.yaml` MUST set neither, and the templates
  MUST NOT read either;
- require nothing at the top level. Every value has a default, so an install
  passing no values MUST succeed;
- set `additionalProperties: false` on the top level and on the objects the
  chart defines itself, since an unknown key is a typo and rejecting it is the
  point of having a schema at all;
- constrain the values whose wrong value fails late rather than loudly: the
  enumerations the templates compare against (`image.pullPolicy`,
  `service.type`, `ingress` path types, `strategy.type`, a monitor's `type` and
  `auth.type`, `targetAuth.type`), a monitor's `auth` that is enabled without
  a `type` or a non-empty `secretName` (§ 42.7), a monitor's `port` and
  `namespaceSelector` (§ 33.5), Go durations, the Prometheus durations of
  the monitors' `interval` and `scrapeTimeout` — a monitor's, and
  `selfMetrics`' and `staticTargets.monitor`'s, which the Prometheus Operator
  takes as whole numbers of `y`, `w`, `d`, `h`, `m`, `s` and `ms`, with no
  fraction, `us` or `ns` — monitor names as DNS-1123 labels, the probes'
  timings (`terminationGracePeriodSeconds` on `livenessProbe` only, since
  Kubernetes refuses it on a readiness probe) and the rolling update's
  counts within what Kubernetes takes (§ 33.10), TCP port ranges, the
  maximum of every whole number the templates print themselves and the
  count flags' and the disruption budget's strings within it (§ 33.10),
  `server.listenAddress` in the same `host:port` shape the render-time check
  enforces, `goGC.percent` as the whole number from 1 to 10000 or the `off`
  the render-time check accepts (§ 33.1), the keys of `config.data` and
  `staticTargets.fileName` as ConfigMap keys (§ 33.2), a mount path inside the
  configuration directory (§ 33.10a), the rules of an enabled `sshAuth`
  and of an enabled `sqlAuth` (§ 42.6), the target allocator's refused
  combinations, probe job names and `settle` (§ 33.10c), and the `--` prefix
  on an `extraArgs` entry; and
- stay open where the chart passes a raw Kubernetes shape straight through —
  `resources`, `affinity`, the security contexts, `tolerations`, `env`,
  `envFrom`, `extraVolumes`, relabelings, network policy rules — beyond the
  fields Kubernetes itself requires. Constraining those would make the chart
  reject a field Kubernetes gained, for no benefit the API server does not
  already provide.

A value the schema rejects MUST fail rendering with the schema's own message.
The schema MUST NOT be used as a reason to change the values API: a value that
is optional today stays optional.

### 33.10c Target allocator

`allocator.enabled`, off by default, MUST deploy the target allocator beside
the exporter and switch the exporter pods to allocator mode. The allocator
MUST run as a `Deployment` of `allocator.replicas` pods, a whole number from 1
to 2147483647, 2 when it is empty, the default, from the exporter's image
unless `allocator.image` names another, running `/bin/pue-allocator`, the
binary beside the exporter's in the image. While a probe job or a `discovered_targets` entry of
`allocator.staticTargets.data` has a discovery source, the Deployment MUST
have one replica and the `Recreate` strategy, and `allocator.replicas` above
1 MUST fail rendering, naming the value, and be refused by the values schema
for a probe job's source: each copy asks its sources on its own timer, so two
would serve the exporter pods and Prometheus different targets, and a
rolling update would run the old pod beside the new one while it terminates.
The allocator MUST be run with `--web.listen-address=:9116`, `--config.file` naming the
configuration's `config.yaml`, `--static-targets-file` while
`allocator.staticTargets.data` is not empty, `--probe-targets-file` while
`allocator.probeJobs` is not empty, `--web.token-file` naming the token's
mounted file, `--log.level`
from `allocator.logLevel`, `--settle` from `allocator.settle` unless it is
empty, and `--members.max` from `allocator.maxMembers`, 1000 by default, a
whole number from 1 to 2147483647. Its `minReadySeconds` MUST be `max(allocator.settle, 10s)`, an empty
settle counting as the allocator's 15s, rounded up to whole seconds, plus 10:
`/ready` answers once the allocator has read its inputs, but its first member
list takes effect that settle after it starts and the exporter pods hear of it
at their next heartbeat, so a rollout counting a new pod available at once
could replace every old allocator before a new one hands out a target. Its
pods MUST be scheduled by `allocator.nodeSelector`, `allocator.tolerations`,
`allocator.affinity`, `allocator.priorityClassName` and
`allocator.topologySpreadConstraints`, never the exporter's values of those
names, and carry `allocator.podLabels` and `allocator.podAnnotations` over
`defaultLabels` and `defaultAnnotations`; a pod label the chart sets MUST
fail rendering and `checksum/config` MUST stay the chart's. A topology spread
constraint without a `labelSelector` MUST be given the allocator's selector
labels; the default MUST be one constraint of `maxSkew: 1` over
`kubernetes.io/hostname` with `whenUnsatisfiable: ScheduleAnyway`, and `[]`
MUST render none. Its pods and objects MUST carry
`app.kubernetes.io/name: <name>-allocator`, so no selector of the exporter's —
its Service, its monitors, its disruption budget and NetworkPolicy — selects
them. The chart MUST render a headless Service `<fullname>-allocator` with
the port `http`, 9116, over the allocator's pods, so the exporter pods reach
every allocator at its name; a `PodDisruptionBudget` of `maxUnavailable: 1`
while `allocator.pdb.enabled`, the default, which lets a node drain evict the
one allocator of a release with discovery; and a NetworkPolicy while
`allocator.networkPolicy.enabled`, the default, admitting ingress to the
allocator's `http` port only from the release's exporter pods and from the
peers of `allocator.networkPolicy.prometheus`, which defaults to pods labelled
`app.kubernetes.io/name: prometheus` in any namespace, as the Prometheus
Operator labels Prometheus.

The allocator MUST always require a bearer token, since the NetworkPolicy's
default peer admits any Prometheus-labelled pod of the cluster, which without
one could heartbeat as a member, be handed targets and be probed with the
exporter's credential. The token MUST come from the Secret
`allocator.tokenSecret.name` names, under `allocator.tokenSecret.key`
(`token` by default), and when the name is empty, the default, from a Secret
`<fullname>-allocator-token` the chart renders with a random token under that
key, which MUST be read back from the release's existing Secret of that name
with `lookup`, so an upgrade keeps it; where `lookup` reads nothing, as with
`helm template`, a new one MUST be generated. With a name the chart MUST
render no Secret. The token's Secret MUST be mounted into the allocator's
and the exporter pods, the allocator run with `--web.token-file`, the exporter
pods with `--allocator.token-file`, and every ScrapeConfig and the
allocator's ServiceMonitor MUST present it as `Bearer` credentials. The
allocator MUST NOT be required to compare a member's advertised address with
the source of its heartbeat: a sidecar or proxy between them would make them
differ for every pod.

The configuration the allocator serves MUST be `config.data`, or the
ConfigMap supplied with `config.enabled: false`, mounted into the allocator at
the configuration directory; `allocator.staticTargets.data` and
`allocator.probeJobs`, the latter rendered under `probe_jobs` as given, MUST be
ConfigMaps of their own, mounted into the allocator only, outside the
configuration directory. The allocator's pods MUST roll when any of the three
changes. A probe job MUST hold only the keys the allocator's probe targets
file takes, `name`, `collector`, `targets`, `params`, `labels`,
`shard_key`, `discovery` and `target_template`, since the allocator decodes
it strictly, and MUST list at least one target or one discovery source; the
values schema MUST declare exactly those keys, and of each discovery source
exactly the allocator's (`name`, `labels`, `max_targets` and one of
`file_sd`, `http_sd`, `kubernetes` and `dns_sd`, each with its own keys).

`allocator.extraVolumes` and `allocator.extraVolumeMounts`, empty by default,
MUST be passed through unchanged into the allocator's pods, appended after the
volumes and mounts the chart gives them, and never into the exporter's; the
exporter's `extraVolumes` and `extraVolumeMounts` MUST NOT reach the
allocator's pods. They are what mounts a `file_sd` source's files, and an
`http_sd` source's token and TLS files, into the allocator. The values schema
MUST take them in the shapes of `extraVolumes` and `extraVolumeMounts`, with
the same pattern refusing a mount path inside the configuration directory. An
`allocator.extraVolumeMounts` entry MUST fail rendering, naming the value and
the directory, at or inside a directory the allocator's pod mounts, every one
a read-only volume: the configuration directory always, the static target
directory while `allocator.staticTargets.data` is not empty, the probe
targets directory while `allocator.probeJobs` is not empty, the token's
directory, and
`/var/run/secrets/kubernetes.io/serviceaccount` while the service account
token is mounted, with a `kubernetes` source or `serviceAccount.automount`;
and so MUST two entries of one `mountPath`. The paths MUST be compared
cleaned, as for the exporter's (§ 33.10a).

While a probe job or a `discovered_targets` entry of
`allocator.staticTargets.data` has a `kubernetes` discovery source, the chart
MUST render a ServiceAccount `<fullname>-allocator` with
`automountServiceAccountToken: true`, and the allocator's pods MUST run as it
with `automountServiceAccountToken: true`; with `allocator.rbac.create`, true
by default, it MUST render, allowing only `list` on
`endpointslices` of the API group `discovery.k8s.io` to that ServiceAccount,
a Role and a RoleBinding `<fullname>-allocator` in each namespace a source
lists, in the release's namespace for a source that lists none, and a
ClusterRole and a ClusterRoleBinding `<namespace>-<fullname>-allocator` for a
source with `all_namespaces`. Without such a source none of them MUST be
rendered, and the allocator's pods MUST run as the exporter's ServiceAccount
with `serviceAccount.automount`.

`allocator.serviceAccount.name`, empty by default, when set MUST run the
allocator's pods as the existing account it names, with or without a
`kubernetes` source, and the chart MUST NOT render `<fullname>-allocator`.
With a `kubernetes` source the pods MUST set
`automountServiceAccountToken: true`, and with `allocator.rbac.create` the
Roles and ClusterRole above MUST be bound to the named account; with
`allocator.rbac.create` false none of them MUST be rendered. Without a
`kubernetes` source the pods MUST set `automountServiceAccountToken` from
`serviceAccount.automount`.

`allocator.staticTargets.expandEnv`, off by default, MUST render
`--static-targets.expand-env` on the exporter pods in allocator mode, which
read the static target file of the allocator's bundle as their own, and
never on the allocator, which serves the document as written.
`staticTargets.expandEnv` beside `allocator.enabled` MUST fail rendering,
naming `allocator.staticTargets.expandEnv`.

`allocator.serviceMonitor.enabled`, off by default, MUST render a
`ServiceMonitor` `<fullname>-allocator` of the allocator's `/metrics` on its
`http` port, selecting the allocator's Service by its selector labels and so
no Service of the exporter's, with `interval`, `scrapeTimeout` and `labels`
from `allocator.serviceMonitor` when set, a timeout longer than the interval
failing rendering, and presenting the allocator's token as `Bearer`
credentials. Being asked for, it MUST NOT wait on the
CRD's presence, and counts as another monitor for the self monitor (§ 33.5).

In allocator mode the exporter pods MUST NOT be given `--config.file`,
`--static-targets-file` or `--config.watch`, and MUST NOT mount the
configuration or a static target ConfigMap; they MUST be given
`--allocator.url` naming the allocator's Service in the release's namespace on
port 9116, `--allocator.cache-dir=/var/cache/prometheus-universal-exporter`,
`--allocator.member` set to the pod's name and `--allocator.advertise` to the
pod's IP and the container port, both from the downward API through the
environment variables `POD_NAME` and `POD_IP`, and `--allocator.token-file`
naming the key of the token's Secret, `allocator.tokenSecret.name`'s or the
generated one, mounted read-only with mode 0400. An `env` entry of either name MUST fail
rendering. The credential Secrets (`targetAuth`, `webAuth`, `sshAuth`,
`sqlAuth`) and `extraVolumes` MUST stay mounted as without the allocator. The
cache directory MUST be an `emptyDir`: it survives a container restart but not
the pod's replacement, and a new pod waits for the allocator. The exporter
MUST stay a `Deployment` behind its Service, scaled by the
HorizontalPodAutoscaler as without the allocator, and the chart MUST NOT
render a second Service over its pods. The exporter pods' checksum annotation
MUST leave the configuration out in allocator mode, since a change reaches
them from the allocator without a restart. With `networkPolicy.egress` rules,
the exporter's NetworkPolicy MUST also admit egress to the allocator's pods on
port 9116.

Each `allocator.probeJobs` entry, while `allocator.scrapeConfig.enabled`, the
default, MUST render a Prometheus Operator `ScrapeConfig`
(`monitoring.coreos.com/v1alpha1`) named `<fullname>-<name>` whose one
`httpSDConfigs` entry reads `http://<fullname>-allocator.<namespace>.svc:9116/v1/sd/<name>`
every `allocator.scrapeConfig.refreshInterval`, 30s by default, presenting the
token as `Bearer` credentials when one is set, with `scrapeInterval` and
`scrapeTimeout` from `allocator.scrapeConfig` when set and its `labels`, and
the `webAuth` Secret's Basic Auth while `webAuth.enabled`. It MUST NOT set
relabelings, a metrics path or parameters: the allocator's answer sets
`__address__`, `__metrics_path__`, the `__param_` labels and `instance`. A
probe job's `name` MUST be a DNS-1123 label, unique among the entries, and
its `collector` MUST be given and, when the chart holds the configuration,
one it defines, as a monitor's must (§ 33.5).

With static targets at the allocator, the static targets monitor
(`staticTargets.monitor`) MUST be rendered without `staticTargets.enabled` and
read every exporter pod's share; `staticTargets.monitor.targets` MUST then fail
rendering, since a pod holds only its share.

These MUST fail rendering with a sentence saying what to change, and be
refused by the values schema where it can express them:
`allocator.enabled` beside `staticTargets.enabled`, since the allocator owns
the static targets; `allocator.enabled` beside an enabled `monitors` entry,
since probe targets then come from `allocator.probeJobs` through the
ScrapeConfigs; a probe job without a target or with a key the allocator does
not take; `staticTargets.expandEnv` beside `allocator.enabled`; and an
`extraArgs` entry naming an `--allocator.*` flag the chart renders.

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
- `helm template` with `strategy.type=Recreate`, with a monitor's `port` and
  `namespaceSelector` on a monitor of each type, with a configuration file
  whose first line is indented, and of a parent chart that has the chart as a
  dependency.
- `helm template` with a monitor's enabled `auth` without a `type`, with a
  monitor's `port` given as a number, with a
  scrape timeout longer than its interval, with the Ingress enabled and the
  Service not, and with a `webAuth.mountPath` and an `extraVolumeMounts` entry
  at the configuration directory written with a trailing slash, each of which
  MUST fail.
- `helm template` with `goGC.percent` set, which MUST render `GOGC` with that
  value, and with the default values, which MUST render no `GOGC`; and with
  `goGC.percent` `off` and `goMemLimit` disabled, `0`, and a number beside a
  `GOGC` entry in `env`, each of which MUST fail.
- `helm template` with `replicaCount`, `server.probeMaxConcurrent` and
  `server.pythonMaxWorkers` of two million given with `--set-json`, which
  hands the templates floating-point numbers as a values file does, one
  written out, one with an exponent and one with a decimal point, which MUST
  render each written out and no number with an exponent; and with a
  `replicaCount` of 2147483648, which MUST fail.
- every rendered manifest starting its own YAML document, with monitors enabled
  and the self-metrics monitor rendering alongside them
- every rendered ServiceMonitor and PodMonitor parsed as YAML, with a
  `spec.selector.matchLabels` mapping and no mapping key holding a colon, for
  probe monitors of both types with and without `targetSelector`, the static
  targets monitor and the self-metrics monitor
- ConfigMap generation
- Deployment generation
- Service generation
- Correct `/probe` path and collector parameter configuration

The validation is written twice, as the `helm-test` recipe of the Makefile and
as the chart steps of the CI workflow, and the two MUST make the same checks.
Every check of either MUST fail its run when what it checks does not hold:

- a render that must succeed, and a line that must be in a render, MUST be a
  command whose failure ends its shell: the last command of its recipe line
  or step, or one under `set -e` — which a workflow step's shell has from
  GitHub unless a `shell:` takes it away, and a recipe line has only by saying
  so — and MUST NOT be followed by `||`, `&&` or `&`. It MAY be followed by
  `|| exit N`, or by `|| { ...; exit N; }` on one line or several, with an
  `N` that is not zero: the failure then ends the shell where it happened;
- a render that must be refused, and a text that must be absent from a render,
  MUST be the whole condition of an `if` whose `then` branch ends the shell
  with a status that is not zero;
- helm MUST NOT be piped into a command that passes on a render of nothing,
  nor into a condition, and a render kept in a variable MUST be kept from the
  one helm command, and read as `echo "$name"` or `printf '%s\n' "$name"`
  piped into `grep -q` and a pattern, `python3 -c` or the manifest check;
- a check MUST be made on every run of its block: it MUST stand at the top
  of the block, in the body of a `for name in value ...; do` loop over one or
  more values written out, or be the condition of an `if` that stands there.
  It MUST NOT stand in a branch of an `if`, whatever the condition — there is
  no guard a check MAY stand behind — nor after `&&` or `||`, nor in any
  other loop; and an `exit`, whatever its status, MUST be the one in the
  `then` branch of a check's `if` or after a check's `||`, since any other
  ends the block before the checks after it;
- a block of checks MUST NOT define a function or an alias, read commands
  from a file, set `PATH`, use `hash`, `return`, `exec`, `break` or
  `continue`, turn `-e` off, set a shell option other than `-e`, `-u`, `-x`,
  `-v` and `pipefail`, or set a `trap` other than a clean-up that only
  removes files, `trap 'rm -rf "$dir"' EXIT`; and
- no recipe line of the checks MAY start with make's `-`; the Makefile MUST
  NOT set `.IGNORE`, `.ONESHELL`, `.SHELLFLAGS`, `MAKEFLAGS`, `PATH` or a
  `SHELL` other than sh or bash, nor give the `helm-test` target a second
  rule; no chart step, or its job, MAY have `continue-on-error`; a chart step
  MUST NOT have an `if` other than the one that runs the chart steps when the
  chart changed, which a step with the id `changes` MUST give for `charts/**`
  as its `chart` filter, and the job MUST NOT have an `if`; and a `shell:` of
  a chart step, of its job's defaults or of the workflow's MUST be bash or sh
  started with `-e`.

A test MUST read both lists as the shell runs them and fail, naming the file,
the line and the recipe or step, on a check that breaks one of these rules.
Where a render is looked at in a way the test does not take for a check — a
here-string, `[[ ]]`, a `grep` of a file or with other options than `-q` —
the finding MUST name that way and say what to write instead, and a check
whose failure does end its shell MUST NOT be reported as one that cannot
fail.

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
- The service account: the one the chart creates, running as an existing one
  named by `serviceAccount.name`, when a token is mounted and which account
  `kubernetesAccess` binds, that annotations apply only to a created account,
  and the allocator's account with `allocator.serviceAccount.name`
- Mounting the TLS files of `tcp`, `redis` and `ldap` collectors from a Secret
  through `extraVolumes` and `extraVolumeMounts`, with an example
- Installation and upgrade from the published OCI repository, including the
  recommendation to pin a version
- Use as a dependency of another chart, with the `condition` that switches it
  and the values under its name; the version the example pins MUST be the
  chart's
- A reference listing every top-level value, its type and its default
- The features the chart deploys, described only where the exporter
  implements them
- What the exporter reads, in its opening: every request type in the tree,
  the opt-in ones as needing a build that asks for them, and every decoder;
  `Chart.yaml`'s `description`, which Artifact Hub shows on the search card,
  MUST name every request type of the image the chart deploys, a default
  build. A test MUST find each `internal/fetch/requesttype_<name>.go` named
  in both, the opt-in ones in the README alone, and each decoder in the
  README's opening, so a new request type or decoder fails it until the
  chart describes it
- The target allocator: what `allocator.enabled` renders, the exporter in
  allocator mode and its `emptyDir` cache, the ScrapeConfigs of the probe
  jobs, and the combinations the chart refuses (§ 33.10c)

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
owners. The repository is registered, so the file MUST carry the repository ID
Artifact Hub generated, a UUID; a placeholder or any other value MUST be
refused, since Artifact Hub would stop verifying ownership and stop indexing
new versions. The release workflow MUST verify that the file parses and names
its owners. How the repository was registered, and what re-registering takes,
MUST be documented, since it is manual work nothing in the repository can do.

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

with at least these values combinations. The Go tests MUST render them with
helm, and check what each renders or the error it fails with, whenever helm is
on the PATH, as CI installs it for any change to the chart; without helm they
are skipped and the text checks of the templates still run:

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
   `server.logLevel`, `server.probeTimeoutOffset` and
   `server.probeDefaultTimeout` set MUST render `--log.level`,
   `--probe.timeout-offset` and `--probe.default-timeout`; an unknown level
   and a negative offset or default timeout MUST fail rendering; the default
   MUST render `--log.level=info` and neither timeout flag.
9a. Static targets enabled, which MUST add the `--static-targets-file`
   argument and render the target document into its own ConfigMap, mounted
   outside the configuration directory (§ 33.2). An
   empty document MUST fail rendering. Static targets MUST render without OTLP
   export in the configuration; when the chart manages the configuration, a
   target setting `export_via_otlp` without `otlp.enabled: true` MUST fail
   rendering with an explicit message naming the target, rather than
   producing a Deployment that cannot start. `staticTargets.fileName` MUST be
   a ConfigMap key — letters, digits, `-`, `_` and `.`, not `.` or `..` —
   otherwise rendering MUST fail, as the values schema MUST refuse it; it MAY
   be a key of `config.data` too, being in another ConfigMap. With
   `config.enabled: false` the static target ConfigMap MUST still render, so
   `staticTargets.data` MUST be required then too. `staticTargets.path` equal to
   `selfMetrics.path` MUST fail rendering. `staticTargets.expandEnv` MUST add
   `--static-targets.expand-env` with static targets enabled, and nothing
   without; `server.expandEnv` MUST NOT add it.
9b. The static targets monitor: with `staticTargets.enabled` and
   `staticTargets.monitor.enabled`, the default, a ServiceMonitor, or a
   PodMonitor with `staticTargets.monitor.type: pod`, named
   `<fullname>-static-targets`, MUST scrape `staticTargets.path` on the `http`
   port with the monitor's interval and scrape timeout and `honorLabels: true`,
   so the series keep their `static_target`, `target` and target labels, and
   MUST present the exporter's credential when `webAuth` is enabled. With
   either off it MUST NOT render; a type other than `service` or `pod` MUST
   fail rendering. `staticTargets.monitor.targets` MUST render as the
   endpoint's `params.targets` list, and nothing when empty; a name that is
   not a target of `staticTargets.data` — a target without a name being
   `<collector>_<index>`, as the exporter names it — MUST fail rendering, since
   the endpoint would answer every scrape `400`.
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
   MUST parse. `.artifacthub-repo.yml` MUST parse, name owners with a name and
   an email address, and carry a UUID repository ID.
16. The documented install: a version pinned in a documented `helm install`
   command MUST equal the version `Chart.yaml` declares, so a chart bump cannot
   leave a reader with a command that installs something else.
17. Resources and availability: the default MUST render
   `--runtime.memory-limit-ratio=0.8`, none with `goMemLimit.enabled: false`,
   and a ratio of 0 or above 1 MUST fail rendering; `server.probeMaxConcurrent` and `server.pythonMaxWorkers` set MUST
   render their flags, and a negative or fractional one MUST fail rendering;
   the probes' timings MUST follow their values, and a probe value setting
   `httpGet` MUST fail rendering; `podDisruptionBudget.enabled` MUST render a
   `policy/v1` PodDisruptionBudget selecting the pods, `maxUnavailable: 1`
   with neither of `minAvailable` and `maxUnavailable`, and both MUST fail
   rendering; `autoscaling.enabled` MUST render a HorizontalPodAutoscaler for
   the Deployment and leave `replicas` out of it, and `maxReplicas` below
   `minReplicas` MUST fail rendering; `autoscaling.targetBusy` of `0.5` and
   `"1500m"` MUST render a `Pods` metric of `http_exporter_busy`, or of the
   `busyMetric` set, with `averageValue` `"0.5"` and `"1500m"`;
   `autoscaling.targetStaticTargetsPerPod: 40` with `allocator.enabled` MUST
   render one of `http_exporter_static_targets` with `averageValue: "40"`, and
   without the allocator MUST fail rendering, as MUST a `targetBusy` of `0m`
   or `lots` and an empty `busyMetric`.
18. Pod scheduling and metadata: `priorityClassName` MUST render on the pod;
   a `topologySpreadConstraints` entry without a `labelSelector` MUST render
   with one matching the Deployment's selector, and one with its own MUST
   keep it; `podLabels` and `podAnnotations` MUST render on the pod only, over
   `defaultLabels` and `defaultAnnotations`; a `podLabels` key the chart sets
   itself MUST fail rendering, and a `podAnnotations` `checksum/config` MUST
   be replaced by the chart's.
19. Debugging collectors: `server.debug: edit` and `server.debug: full`
   MUST render `--web.debug=edit` and `--web.debug=full`; the default and
   `off` MUST render no `--web.debug`; any other value MUST fail rendering,
   naming `server.debug`; and the flag in `extraArgs` MUST fail rendering,
   naming `server.debug`.
20. Names, network policy, monitors and scheduling: releases `blue` and
   `green` MUST get objects of their own names, a release named after the
   chart the chart's name, and `fullnameOverride` its own; the network policy
   alone MUST allow the `http` port in and leave egress open, and with egress
   rules add DNS unless `allowDNS` is false; a probe monitor MUST point at the
   Service's full name in the release's namespace or `namespaceOverride`, and
   MUST fail rendering without the Service, without a collector, or with a
   collector `config.data` does not define; the self monitor MUST render once,
   of `selfMetrics.type`, with another monitor or with the cluster serving the
   kind, and for a static-targets-only release; `watchConfigInterval: 1m30s`
   MUST render and `0s` and `0m0s` fail; `maxUnavailable: 0` MUST render as
   `0` with a warning in the notes; the pod MUST not mount a token on the
   default account.
21. Monitor structure: the rendered ServiceMonitors and PodMonitors MUST be
   parsed as YAML, not searched as text, and each MUST have a
   `spec.selector.matchLabels` mapping, with no mapping key anywhere holding a
   colon, for probe monitors of `type: service` and `type: pod` with and
   without `targetSelector` (the latter selecting
   `app.kubernetes.io/name: target`, the former its own labels), the static
   targets monitor of both types, and the self-metrics monitor of both types.
22. Monitor values: a monitor name that is not a DNS-1123 label (upper case,
   an underscore), two entries of one name, and an entry named `self` or
   `static-targets` MUST fail rendering; so MUST a monitor's `params.collector`,
   with a message pointing to the entry's `collector`, and its `params.target`;
   and a fractional or `us`/`ns` `interval` or `scrapeTimeout` on a probe
   monitor, `selfMetrics` or `staticTargets.monitor`, while `1m30s`, `1500ms`
   and `1d` MUST render.
23. The exporter credential on probe monitors: with `webAuth.enabled`, a probe
   monitor of either type without `auth` MUST render `basicAuth` naming the
   `webAuth` Secret and its `usernameKey` and `passwordKey`; one with its own
   bearer `auth` MUST keep that and render no `basicAuth`; without
   `webAuth` a probe monitor without `auth` MUST render none. CI MUST check
   the probe monitor itself, not any monitor, for the credential.
24. Unreachable pods: a `server.listenAddress` of `127.0.0.1:8080`,
   `localhost:8080` or `[::1]:8080` MUST fail rendering, and `[::]:9115`
   render; no chart message or document MUST offer a loopback address as an
   example; `service.type: ExternalName` and a `readinessProbe`
   `terminationGracePeriodSeconds` MUST fail rendering, while a
   `livenessProbe` one MUST render.
25. The README's Exporter authentication examples: each values block of the
   section MUST render as written, and the `config.yaml` it renders MUST load
   as the exporter loads it, the `webAuth` files standing in a directory of
   the test's own; an example that replaces `config.yaml` MUST therefore keep
   collectors in it, and one whose monitors name a collector MUST define it.
26. A dependency of another chart: a parent chart built in a directory of the
   test's own, naming the chart by its `file://` path with a `condition` of
   `prometheus-universal-exporter.enabled`, MUST render the chart's
   Deployment and ConfigMap with no values, with `global` values and with
   `enabled: true` and a value of the chart's under its name; the condition
   set false MUST render nothing of the chart; a misspelled value under the
   chart's name and an `enabled` that is not a boolean MUST still fail
   rendering. Without helm, the schema MUST still be checked to declare
   `global` as an object and `enabled` as a boolean, and `values.yaml` to set
   neither.
27. The Deployment strategy: the default MUST render `RollingUpdate` with
   `maxSurge: 1` and `maxUnavailable: 0`; `strategy.type: Recreate` alone MUST
   render `type: Recreate` and no `rollingUpdate`; a `rollingUpdate` set with
   `RollingUpdate` MUST be rendered as set.
28. Monitor credentials: an enabled `auth` of `type: bearer` and of
   `type: basic` MUST render complete selectors on monitors of both types,
   with the default keys, a key and `optional` of its own, the endpoint's
   relabelings still in place, and no `webAuth` credential beside it; an
   `auth` that is not enabled MUST render none and need neither `type` nor
   `secretName`; an enabled `auth` without a `type`, without a `secretName`
   or with an empty one MUST fail rendering on monitors of both types, by the
   values schema, and, with the schema skipped and `webAuth` enabled, by the
   templates, with a message naming the monitor and `auth.type` or
   `auth.secretName`. A monitor without a `name` MUST fail rendering, and
   neither monitor template MUST hold a fallback name.
29. Monitor ports and namespaces: a monitor without `port` MUST render the
   port `http` and no `namespaceSelector`, for both types; a `port` MUST be
   rendered as a string on the endpoint, one YAML would read as a boolean
   included; `namespaceSelector.matchNames` and `namespaceSelector.any` MUST
   be rendered under the monitor's `spec`, beside its `selector`; an empty
   `port`, a `port` that is a number, a `namespaceSelector` with another key
   and a `matchNames` entry that is empty MUST fail rendering. For both
   types the ports `http`, `grpc`, `metrics-2`, `a`, `9-a` and one of 15
   characters MUST render as given, as strings, and `9115` as a number and
   as a string, `Http`, `-http`, `http-`, `1-2`, `my_port`, a boolean, an
   empty one and one of 64 characters MUST be refused by the values schema,
   naming `monitors.0.port`; `a--b` and a name of 16 characters MUST be
   refused for `type: pod` and rendered for `type: service`, as one of 63
   MUST be. With the schema skipped the templates MUST refuse the same
   values, naming the monitor: a number, as a number or as a string, with a
   message saying that the port is a number and to name the port in the
   Services' `spec.ports` or the pods' `spec.containers[].ports`, anything
   else with the grammar of the type's port names; `grpc`, `metrics-2` and
   `a` MUST still render, a null `port` MUST render `http`, and the `port`
   of an entry that is not enabled MUST NOT fail rendering. A test MUST
   check that the chart README, the description of `port` in the values
   schema and this specification each say that the letter is the chart's
   own rule for a Service port, that Kubernetes allows such a port a name
   of digits alone and that a Service with one cannot be monitored through
   `port` until the port has a name with a letter; that none presents the
   letter as how a Service port is named; and that the schema's pattern
   still refuses `8080`, `9115` and `80-80` and takes `http`, `8080-tcp`
   and `9-a`.
30. Configuration files as given: files whose first line is indented, blank
   or starts with a tab, without a final line break, holding document
   markers, blank and whitespace-only lines and trailing spaces inside, or
   characters outside ASCII, and files named `123` and `true`, MUST be
   rendered under `data` with string keys and read back as the bytes given;
   files ending in two line breaks, a trailing space, a no-break space or a
   whitespace-only line, holding carriage returns, a control character, a delete character,
   a byte order mark, a next-line, line-separator or paragraph-separator
   character or bytes that are not UTF-8, an empty file and a file of one
   line break MUST be rendered under `binaryData` and decode to the bytes
   given. Each text file MUST also round-trip as the last file of the
   ConfigMap and as the first lines of the static target document. The
   `checksum/config` annotation MUST be the same for the same values and
   differ after a change to `config.yaml`, to a collector file, to its first
   line's indentation or its final line break, to a file under `binaryData`,
   after a file is added, and after the static target document is added or
   changed.
31. Scrape timings, the Ingress and mount paths: a `scrapeTimeout` longer
   than its `interval` MUST fail rendering, naming the monitor and both
   values, on a probe monitor of either type, in seconds against minutes,
   milliseconds, hours, days, weeks and years, on `selfMetrics` and on the
   static targets monitor; one equal to its interval in other units, one
   without an interval, and the timings of a disabled entry, of
   `selfMetrics` when disabled and of a static targets monitor that is not
   rendered MUST render. `ingress.enabled` MUST render an Ingress to the
   Service and, with `service.enabled: false`, fail rendering.
   `webAuth.mountPath` or `targetAuth.mountPath` at the configuration
   directory, with or without a trailing slash, the two at one path, an
   `extraVolumeMounts` entry at either or at the configuration directory
   with a trailing slash on either side, and two entries at one path MUST
   fail rendering, naming what collides; the three at paths of their own,
   one nested in another, MUST render.
32. A budget of every replica: the notes MUST warn when `minAvailable` is 1
   with one replica, 2 or 3 with two, `50%` with one, `67%` with three and
   `100%` with three or with autoscaling, and MUST NOT warn for 1 with two
   replicas, 0, `50%` with two, `66%` with three, 1 with autoscaling, 1 with
   no replicas, the default budget, `maxUnavailable: 1` or a budget that is
   not enabled.
33. Documentation in step with the code: the chart README's flag table and
   § 33.10's MUST name every flag the chart's Deployment renders and no
   other, and the README's one-shot sentence every exporter flag the chart
   does not render; the Python base image the chart README names MUST be
   the Dockerfile's `PYTHON_VERSION`; the version the README's dependency
   example pins MUST be the chart's; and the static target example in
   `values.yaml`'s comments MUST load and validate as the exporter loads a
   static target document, its one target holding its `request`, `labels`,
   `export_via_otlp` and `otlp`, with no commented line left after the
   `monitor` values.
34. The cases of `make helm-test` and CI: the Go tests above are skipped
   without helm, which neither `make helm-test` nor the chart steps of the CI
   workflow are, so each of the two MUST also render
   `strategy.type=Recreate` and find `type: Recreate` and no `rollingUpdate`;
   render a monitor of `type: service` and one of `type: pod` with
   `port: grpc` and `namespaceSelector.matchNames`, and find the port, the
   selector and the namespace; render `goGC.percent=400` and find
   `name: GOGC` and `value: "400"`, and render the default values and find
   no `GOGC`; render `replicaCount`, `server.probeMaxConcurrent` and
   `server.pythonMaxWorkers` of two million given with `--set-json` as
   `2000000`, `2e6` and `2000000.0`, and find `replicas: 2000000`, both
   flags written out and no `e+0`; render a further `config.data` file whose
   first line is indented, `testdata/chart/indented-first-line.yaml`, find
   that line indented in the ConfigMap and pass the render to the manifest
   check; and build the parent chart `testdata/chart/parent` with
   `helm dependency build`, render it with
   `prometheus-universal-exporter.enabled=true`, and find the dependency's
   Deployment and the replica count the parent's values give it. The parent
   MUST be built in a temporary copy of the two charts, so that neither the
   `charts` directory nor the `Chart.lock` helm writes reaches the working
   tree, and the build MUST need no network. Each of the two MUST fail when
   helm accepts a monitor's enabled `auth` without a `type`, a monitor's
   `port` of `"9115"`, a
   `selfMetrics.scrapeTimeout` longer than `selfMetrics.interval`,
   `ingress.enabled` with `service.enabled=false`, a `webAuth.mountPath` at
   the configuration directory written with a trailing slash, an
   `extraVolumeMounts` entry there written with one, `goGC.percent` `off` as
   a string with `goMemLimit.enabled=false`, `goGC.percent=0`,
   `goGC.percent=200` beside a `GOGC` entry in `env`, or a `replicaCount` of
   2147483648. A check of what a render
   holds MUST keep the render before reading it, so that helm failing fails
   the check rather than handing the reader nothing. A test MUST fail when
   either list lacks one of these checks. A test MUST also find no check in
   either list whose failure would fail nothing (§ 33.11), reading at least
   as many checks from each list as it has had, and with it a recipe line as
   a shell started without `-e` at the line it is written on, make's `@`,
   `+` and `-` off it, and a step as a shell started with `-e` unless its
   `shell:`, its job's or the workflow's default is a command without the
   flag, without the script (`{0}`) last, or with a word that GitHub's own
   command for bash and sh does not have, `-n` or `-c` among them. Each of
   these changes, made to a copy of the Makefile's or the
   workflow's text and never to the files, MUST be reported with the file
   and what is wrong: a `then` branch that lost its `exit 1`, that exits
   with 0, with 256, which a shell takes for 0, or that exits only when its
   message fails; a check followed by `|| exit 256` or by
   `|| { ...; exit 256; }`; a check followed by
   `|| true` or `|| :`; a recipe line that lost its `set -eu`, that turns it
   off again with `set +e`, that goes on after a check without it, or that
   starts with `-` or `@-`; `.IGNORE` in the Makefile; `continue-on-error`
   on a chart step or on its job; a chart step with `if: false` or with a
   `shell:` without `-e`; a condition joined to `&& false` or followed by
   another command; a render piped into a condition, piped into `cat`, or
   kept from `helm ... || true`; a render read with `grep -qv` or with an
   empty pattern; an `if` piped into another command; a refusal written
   behind `!`; a rejection replaced by `if false`, a case rendering another
   value and a pattern weakened in one list only; an `exit 0`, a `return` or
   an `exec` before the checks of a block, and a `continue` or a `break`
   before the check of a loop; the checks of a block wrapped in `if false`, in
   an `if` on a variable, in the `else` of `if true`, in a `while`, or in a
   loop over nothing or over a variable; a check moved into the `then` branch
   of a refusal or behind `true ||`; a refusal behind `false &&`, or whose
   `exit 1` is inside an `if false`; a `trap` that exits with 0 on `EXIT` or
   on `ERR`, and the clean-up trap with an `exit 0` added; `helm` or `python3`
   made a function and `grep` an alias; a `PATH` set in a block or in the
   Makefile; `hash -p` naming another program as `helm`; a file of commands
   read in; the checks of a block made the lines of a here-document, which
   the shell runs none of; `set +e` before a step's last check, and `set -n`; a check
   followed by `|| exit 0`, by `|| { ...; }` with no exit or with `exit 0`, or
   by `|| { ...; exit 1; } | cat`; an `if` on the job; a `chart` filter that
   no longer names `charts/**`; a `shell:` without `-e` as the workflow's or
   the job's default, and `bash -n -e {0}` on a step; and in the Makefile a
   `SHELL` that is not a shell, `MAKEFLAGS`, and a second rule, or a variable
   of its own, for `helm-test`. Edits that take nothing from a check MUST be
   made the same way, and each MUST either leave the test with no finding — a
   comment or a line of progress, two refusals in the other order, a refusal
   in a loop over values, a helm command continued onto another line,
   `set -euo pipefail`, `printf '%s\n' "$out"` for `echo "$out"` in one list
   or in both, `"${out}"`, `exit 2`, a check followed by `|| exit 1` or by
   `|| { echo "..." >&2; exit 1; }` on one line or on several, a blank line,
   another message, a render kept without quotes or in a variable of another
   name, the chart's path in a variable, `test -n "$out"`, `&>/dev/null`, and
   an `if` with no check and no `exit` in it — or be refused by a finding that
   names the way it is written and what to write instead: `grep -c`,
   `grep -qF` and `grep -q -e`, `[[ "$out" == *X* ]]`, a here-string, a render
   written to a file and the file read, a kept render printed as `printf`'s
   format, a refusal behind `!`, a variable renamed where the render is kept
   and not where it is read, and `--debug` on a refusal the Go tests make too,
   whose finding MUST show the check that is wanted beside the one that is
   made. No such edit MAY be called a check that can never fail. Both readers
   of the lists MUST take the same ways of writing a check: each of the
   accepted ones MUST be read as the same list of checks as the plain
   script's, with as many checks weighed. The parent chart MUST hold a
   `Chart.yaml` and a `values.yaml` and nothing else, name the chart as its
   one dependency by a relative `file://` path that reaches it, at the
   chart's version and with the condition
   `prometheus-universal-exporter.enabled`, and set `global` values and the
   chart's `replicaCount` but not the condition; the indented file MUST keep
   an indented first line and a later line that is not indented, and MUST be
   rendered and read back as given; tests MUST check both, the renders
   whenever helm is on the PATH.
35. The garbage collector's target: the default, an empty and a null
   `goGC.percent` MUST render no `GOGC` and no `env`, the same text as a
   render without the value, and `goGC.percent=400` MUST differ from it by
   the exporter container's `env` alone. `400`, as a number, from a values
   file and as a string, `1` and `10000` MUST render `GOGC` once in the
   whole render, on the exporter container, as a quoted string; with `env`
   and `envFrom` set it MUST come first in `env`, the entries of `env` after
   it in their order and `envFrom` after `env`. `off` MUST render with the
   default values, quoted in a values file, with another ratio and memory
   limit, and with `goMemLimit` off or no memory limit beside a
   `GOMEMLIMIT` entry in `env` holding a value or a `valueFrom`; a number
   MUST render without a memory limit
   and with `goMemLimit` off; a `GOGC` entry in `env` with no `goGC.percent`
   MUST render as given. `0`, `-1`, `10001`, `1.5` as a string and as a
   number, `fast`, the strings `0`, `10001` and `0400`, `400%`, `true`, an
   unquoted `off` in a values file and another key under `goGC` MUST be
   refused by the values schema, naming `goGC.percent` or the key. A number
   or `off` beside a `GOGC` entry in `env`, with a value or a `valueFrom`,
   MUST fail rendering naming both; `off` with `goMemLimit.enabled: false`,
   without `resources.limits.memory`, `resources.limits` or `resources`,
   with neither, with `GOMEMLIMIT=off` in `env`, and with `goMemLimit` off
   beside an empty `GOMEMLIMIT` entry MUST fail rendering naming
   `goGC.percent` and what leaves the limit out, and with
   `goMemLimit.ratio=0` by the ratio's own rule. With the schema skipped,
   `0`, `-1`, `10001`, `1.5`, `fast`, `0400`, `false` and `true` MUST fail
   rendering with the templates' own message, and `400`, `"400"`, `"off"`,
   an empty string and null MUST render. Without helm, `values.yaml` MUST
   still be checked to default `goGC.percent` to empty, the Deployment to
   take `GOGC` from the helper that checks it, and the schema to take an
   integer from 1 to 10000, a string or null, refuse another key, and hold
   the pattern the helper checks with an alternative for the empty string.
36. Whole numbers (§ 33.10): written `2000000`, `2e6` and `2000000.0` in a
   values file, and as `1000000`, `1000680000` and `2147483647`, a number
   MUST render written out in `replicas`, both count flags,
   `terminationGracePeriodSeconds` and the budget's `minAvailable`, and in
   the HorizontalPodAutoscaler's `minReplicas`, `maxReplicas` and both
   `averageUtilization` targets and the budget's `maxUnavailable`; the same
   renders MUST hold a pod and a container `runAsUser`, an `fsGroup`, a
   probe's `periodSeconds`, a CPU limit and a relabeling's `modulus` of that
   number written out, and no number with an exponent. `service.port`
   written `9.115e3` MUST render `9115` in the Service, the Ingress and the
   monitors' address. The values schema MUST refuse, for its maximum and
   naming the value, a `replicaCount` and a grace period of 2147483648, an
   `autoscaling.maxReplicas` of `3e9` and a count flag of `1e30`, and MUST
   refuse a `replicaCount` of `2000000.5` and a count flag given as the
   strings `"2147483648"`, `"99999999999999999999"` and `"010"`; count flags
   of `"2147483647"` and `"0"` MUST render as written. With the schema
   skipped the templates MUST refuse, naming the value, the number and the
   range: a `replicaCount` of `1.5`, `2000000.5`, `2147483648`, `3e9`,
   `1e30`, `100000000000000000000`, `-1` and a word; a grace period of
   `2000000.5` and `1e30`; a `service.port` of `2000000` and `65536`;
   `autoscaling.minReplicas: 0` and a utilization target of `3000000000`; a
   budget's `minAvailable: 1.5`; and count flags of `3000000000`, `1e30`,
   `1.5`, `"2147483648"` and `"010"`; a monitor's `port` of `2000000` and a
   `goGC.percent` of `2000000` MUST be named written out; and every number
   at 2147483647 MUST render as with the schema. A grace period of 1000000
   seconds too short for the shutdown MUST be refused naming it written out,
   and the notes MUST warn for a `minAvailable` of `2000000` and of `2e6`
   that is every replica. A chart of the helpers alone MUST print every
   whole number of a table below a million — each within a thousand of
   zero, the powers of ten and their neighbours, and generated ones, as a
   values file and as `--set` give them — as a template prints it by itself,
   so that no render of such a number changed. Without helm, each whole
   number's template MUST still be checked to render it through the helper
   with the range the schema holds, the schema's pattern for the count flags
   to be the templates' with an alternative for the empty string and to take
   exactly the strings of digits of 2147483647 or less with no zero before
   another digit, and no integer of the schema to be without a maximum.
37. Bounds of what Kubernetes takes (§ 33.1, § 33.10): the values schema
   MUST refuse, naming the value, each probe timing of `1e30` and of
   2147483648, a liveness `successThreshold` of 2, a fraction, and a rolling
   update's `maxSurge` and `maxUnavailable` of 2147483648, `-1`, `2.5`, the
   strings `"3"` and `"010%"`, and a `maxUnavailable` of `"101%"`; it MUST
   take each timing at 2147483647, a readiness `successThreshold` of 3, a
   `maxSurge` of `"200%"` and a `maxUnavailable` of `"100%"`, rendered as
   written. A budget's `minAvailable` or `maxUnavailable` given as the
   string `"3000000000"`, `"2147483648"`, `"010"`, `"00"`, `"200%"`,
   `"101%"`, `"050%"`, `"-1"`, `"1.5"`, `"1e3"`, `"1 "` or `""` MUST be
   refused by the schema naming the value and, with it skipped, by the
   templates' message naming the key; `"2147483647"`, `"0"`, `"0%"` and
   `"100%"` MUST render bare. The schema's pattern for the budget's strings
   MUST be the templates', and MUST take exactly the strings of digits of
   2147483647 or less with no zero before another digit and the percentages
   from `0%` to `100%` written the same way. `goMemLimit.ratio` written
   `1`, `1.0` and `0.9999999` MUST render as written, and `0`, `1.5` and the
   string `"1e-5"` MUST be refused by the schema and, with it skipped, by
   the templates; a chart of the helpers alone MUST render every ratio from
   0.1 to 1 of a table that a template printed without an exponent as it
   printed it.
38. Floors of what makes sense (§ 33.1, § 33.10): `goMemLimit.ratio` written
   `0.1`, `0.10000001`, the string `".1"` and `1` MUST render as written, and
   `0.1000` as `0.1`; `0.0999999`, `0.0001`, `1e-5`, `0.00001`, `1e-9`,
   `1.5e-7`, `1e-300` and the string `"0.05"` MUST be refused by the schema
   naming `goMemLimit.ratio` and, with it skipped, by the templates' message
   saying the floor, the value written out with no exponent. A rolling update
   whose rendered `maxSurge` and `maxUnavailable` are both 0, as `0` or `0%`
   in either, MUST be refused by the schema at `strategy` and, with it
   skipped, by the templates' message naming both keys; one of them left out,
   which Kubernetes defaults to 25%, one of them 1 or `1%`, and both 0 with
   `strategy.type: Recreate`, which renders no `rollingUpdate`, MUST render.
   The repository's own schema validator MUST give helm's verdict on each.
39. Mounts below a read-only volume (§ 33.10a): an `extraVolumeMounts`
   directory and a `subPath` file inside the configuration directory,
   `webAuth.mountPath` and `targetAuth.mountPath` inside it, each credential's
   inside the other's, `webAuth.mountPath` of `/etc`, above the
   configuration directory, and an `extraVolumeMounts` entry inside either
   credential's directory MUST fail rendering, by the values schema naming
   the value where it is refused there, and with the schema skipped by the
   templates' message naming the value, the directory and where to mount
   instead; a path written with doubled and dotted segments, and one whose
   `..` stays inside, MUST be refused as its cleaned path is. A path beside
   the configuration directory, one that climbs out of it again, the
   credentials beside each other below an `emptyDir` of the values' own, and
   credentials that are not enabled at paths inside the configuration
   directory or with a mount inside theirs MUST render, with the schema and
   without. A chart of the helpers alone MUST run the check as it was and as
   it is over a generated table of thousands of credential and extra mount
   paths — at, inside, above and beside the configuration directory and the
   credentials, with trailing slashes, doubled and dotted segments — and the
   new check MUST refuse with the old one's message where the old one
   refused, refuse exactly the cases Go's `path.Clean` finds below a
   read-only volume where it rendered, and render every other. Over
   generated paths, every path the schema's pattern refuses MUST be, cleaned,
   inside the configuration directory, every plain path inside it MUST be
   refused, the directory itself, with trailing slashes, MUST NOT be, the
   pattern MUST be the same for the five values, and the repository's own
   validator MUST refuse a credential's path inside it only while enabled.
40. Probe monitor timings: a monitor of either type without `interval` and
   `scrapeTimeout` MUST render neither, no `null` anywhere, its `params`
   right after its `path`; one with only `interval` or only `scrapeTimeout`
   MUST render that one alone, a `scrapeTimeout` of `5m` without an interval
   still rendering; and one with both MUST render both as written.
41. SSH credentials (§ 42.6): the default values, and `sshAuth` disabled with
   its keys filled in, MUST render byte for byte what the chart renders
   without the block, with no `ssh-auth` volume and no known hosts
   ConfigMap. Enabled, a private key with its passphrase and inline lines, a
   password with known hosts from the Secret at a `mountPath` with a
   trailing slash, and a key and a password with inline lines lacking a
   final newline MUST each render the `ssh-auth` mount read-only at
   `mountPath` and one projected volume of `defaultMode` 0400 whose Secret
   source names exactly the keys set under their file names, a ConfigMap
   source and the `-ssh-known-hosts` ConfigMap holding the lines as given
   only with inline lines, and the pod's `fsGroup` 65532. No `secretName`,
   no credential, a passphrase without a key, no known hosts, known hosts
   from both, and inline lines of comments, short lines or blanks alone
   MUST each be refused by the values schema at the value it names and,
   with the schema skipped, by the templates' message. A chart of the
   `validateSSHAuth` helper alone MUST run it over every combination of the
   block's keys — empty, set and blanks, inline lines with an entry, of
   comments, short lines, blanks, tabs and carriage returns — reaching all
   six refusals and none, and the repository's schema validator MUST take
   exactly the blocks the helper takes, helm's own validator agreeing on
   one block of each outcome; the schema's entry pattern MUST be the
   helper's. `checksum/config` MUST be the same for the same lines, differ
   for another key, an added line, a removed final newline and a carriage
   return, and with known hosts from the Secret equal the default values'.
   `sshAuth.mountPath` at the configuration directory, at `webAuth.mountPath`
   or at an `extraVolumeMounts` entry's MUST fail rendering naming both; and
   inside the configuration directory, inside `webAuth.mountPath`,
   `targetAuth.mountPath` inside it, `/etc` above the configuration
   directory and an `extraVolumeMounts` `subPath` file inside it MUST fail
   as item 39 says, naming a read-only projected volume, while `sshAuth`
   beside the other credentials, and not enabled inside the configuration
   directory or with a mount inside its path, MUST render. The chart
   README's SSH file collectors section MUST show two `sshAuth` examples,
   each rendering, with its collector configuration, a volume holding
   `private_key` and `known_hosts`, and a collector naming both files.
42. ConfigMap keys of `config.data` (§ 33.2): keys with a `/`, a space, a
   colon, a letter outside ASCII, `.`, `..` and 254 characters MUST each be
   refused by the values schema, naming the key, and, with the schema
   skipped, by the templates' message naming the key, what a key may hold
   and that the configuration directory is flat; one with a `/` MUST be
   refused so with `config.enabled: false` too. Keys of digits only,
   starting with `-` or `_`, of three dots and of 253 characters MUST
   render, with the schema and without.
43. The static target directory (§ 33.2): with `collector_files: ["*.yaml"]`
   in `config.yaml`, a further collector file and static targets enabled,
   the rendered ConfigMaps laid out at the Deployment's mount paths MUST
   hold `--static-targets-file` outside the directory of `--config.file`,
   and MUST load as the exporter loads them at startup: the configuration
   with its own collector and the collector file's, and the static target
   document, checked against it. An `extraVolumeMounts` entry at
   `/etc/prometheus-universal-exporter-static-targets` written with a
   trailing slash, `webAuth.mountPath` at it, an `extraVolumeMounts` `subPath` file inside
   it and `targetAuth.mountPath` inside it MUST fail rendering naming the
   static target directory while static targets are enabled, and render
   while they are not. With `config.enabled: false` the static target
   document MUST be the one file of the ConfigMaps rendered, and static
   targets without `staticTargets.data` MUST fail rendering.
44. SQL credentials (§ 42.6): the default values, and `sqlAuth` disabled with
   its keys filled in, MUST render byte for byte what the chart renders
   without the block, with no `sql-auth` volume. Enabled, a password, a
   private key, a CA bundle at a `mountPath` with a trailing slash, a
   client certificate and key, and
   every key at once MUST each render the `sql-auth` mount read-only at
   `mountPath` and one Secret volume of `defaultMode` 0400 naming exactly
   the keys set under their file names, and the pod's `fsGroup` 65532. No
   `secretName`, no key, a client certificate without its key and a key
   without its certificate MUST each be refused by the values schema at the
   value it names and, with the schema skipped, by the templates' message;
   a `passphraseKey` MUST be refused by the values schema as a key the block
   does not have and, with the schema skipped, by the templates, whether the
   block is enabled or not. A chart of
   the `validateSQLAuth` helper alone MUST run it over every combination of
   the block's keys — empty, set and blanks — reaching all four refusals
   and none, and the repository's schema validator MUST take exactly the
   blocks the helper takes, helm's own validator agreeing on one block of
   each outcome. `sqlAuth.mountPath` at the configuration directory, at the
   static target directory, at `targetAuth.mountPath`, `webAuth.mountPath`
   or `sshAuth.mountPath`, or at an `extraVolumeMounts` entry's MUST fail
   rendering naming both; inside the configuration directory, inside
   `sshAuth.mountPath`, `webAuth.mountPath` inside it, `/etc` above the
   configuration directory and an `extraVolumeMounts` `subPath` file inside
   it MUST fail as item 39 says, naming a read-only Secret volume; and
   `sqlAuth` beside every other mount, and not enabled inside the
   configuration directory or with a mount inside its path, MUST render,
   with the schema and without. The schema's pattern for a `mountPath`
   inside the configuration directory MUST be the other credentials', and
   refuse it only while enabled. The chart README's SQL collectors section
   MUST show two `sqlAuth` examples, a collector whose `sql` request names
   the mounted `password_file` and `tls.ca_file` and which the exporter's
   `config.Load` loads as written, a further database's
   Secret through `extraVolumes` and an egress rule; the password example
   with its collector MUST render a volume holding `password` and `ca.crt`,
   the Snowflake example one holding `private_key` alone, and all
   of them together the further Secret's volume of mode 0400 beside it and
   a NetworkPolicy allowing egress to port 5432.

45. The target allocator (§ 33.10c):

   - The chart with `allocator.enabled` renders the allocator's Deployment of
     `allocator.replicas` pods running `/bin/pue-allocator` from the exporter's
     image with `--web.listen-address=:9116`, `--config.file`,
     `--static-targets-file`, `--probe-targets-file`, `--log.level`,
     `--settle` and `--members.max`, mounting the configuration ConfigMap and its own static target
     and probe target ConfigMaps (the probe jobs under `probe_jobs` as given); a
     headless Service on port `http`, 9116; a PodDisruptionBudget of
     `maxUnavailable: 1`; and a NetworkPolicy admitting the exporter's pods and,
     by default, the Prometheus Operator's Prometheus pods.
   - The exporter pods in allocator mode get `--allocator.url` naming the
     allocator's Service, `--allocator.cache-dir`, `--allocator.member=$(POD_NAME)`
     and `--allocator.advertise=$(POD_IP):<port>` from the downward API, a cache
     `emptyDir`, and neither `--config.file`, `--static-targets-file` nor the
     configuration ConfigMap; the credential Secrets stay mounted.
   - No selector of the exporter's (its Service, the self-metrics and static
     targets monitors) selects the allocator's pods, and the allocator's Service
     is not selected by the self-metrics monitor.
   - Each probe job renders a ScrapeConfig whose `httpSDConfigs` URL is the
     job's `/v1/sd/<job>` on the allocator's Service, refreshed every 30s, with
     the interval, timeout and labels given and no relabelings, metrics path or
     parameters; it presents the token as Bearer credentials, the exporter
     gets `--allocator.token-file` and the allocator `--web.token-file`, from
     the Secret `allocator.tokenSecret` names or, without a name, the
     `<fullname>-allocator-token` the chart generates with a random token,
     a new one at each `helm template`, and none with a name; with `webAuth.enabled` it presents the exporter's Basic
     Auth. No probe job, or `scrapeConfig.enabled: false`, renders none.
   - In allocator mode the exporter is a Deployment, which the
     HorizontalPodAutoscaler scales, with no StatefulSet and no Service beside
     the exporter's and the allocator's; the values schema refuses an
     `allocator.cache` key.
   - In allocator mode a configuration change rolls the allocator's pods and not
     the exporter's; a static target change rolls the allocator's; without the
     allocator a configuration change still rolls the exporter's pods.
   - With static targets at the allocator the static targets monitor is
     rendered, and with `networkPolicy.egress` rules the exporter's NetworkPolicy
     admits egress to the allocator's pods on port 9116.
   - The chart refuses, with the values schema and with the schema skipped, the
     allocator beside `staticTargets.enabled` or an enabled `monitors` entry,
     a probe job of an unknown or no collector, a probe job name that is no
     DNS-1123 label or is used twice, `allocator.replicas` 0,
     `allocator.maxMembers` 0, a `settle` that is no duration, an unknown log level, an `env` entry `POD_IP`, an
     `--allocator.*` flag in `extraArgs`, `staticTargets.monitor.targets`
     beside the allocator, a mount over the cache or token directory, and a
     ScrapeConfig timeout longer than its interval.
   - With a discovery source, of a probe job or of a `discovered_targets`
     entry, the allocator's Deployment has one replica and the `Recreate`
     strategy, an empty or 1 `allocator.replicas` included, and its
     disruption budget stays `maxUnavailable: 1`; `allocator.replicas: 2`
     fails rendering, through the schema for a probe job and through the
     template with the schema skipped or for a static target entry; without
     discovery the allocator keeps 2 replicas, or `allocator.replicas`, and
     the default strategy (`TestChartRunsOneAllocatorWithDiscovery`).
   - The allocator's Role allows only `list` on EndpointSlices
     (`TestChartGrantsTheAllocatorEndpointSlicesForKubernetesDiscovery`).
   - Every example query of an allocator metric in `docs/*.md` and the chart
     README, and every prometheus-adapter rule over one, takes the `max` of
     the allocator copies' series
     (`TestExampleQueriesOfAllocatorMetricsTakeTheirMax`).
   - The chart's renders without the allocator are unchanged: `make helm-test`
     and the CI chart steps also render allocator mode and reject the allocator
     beside static targets and a probe monitor beside the allocator.

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
- `tolerations`, `affinity`, `nodeSelector`, `topologySpreadConstraints`
  (one without a `labelSelector` selecting the release's pods) and
  `priorityClassName`;
- `podLabels` and `podAnnotations` for the pods only, applied after
  `defaultLabels` and `defaultAnnotations`; the chart's identity labels and
  `checksum/config` MUST stay the chart's, a `podLabels` key among them
  failing rendering;
- an optional Ingress resource with class, host, path, TLS, and annotations;
  it routes to the exporter's Service, so `ingress.enabled` with
  `service.enabled: false` MUST fail rendering;
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

The chart MUST also offer `webAuth` (`enabled`, false by default,
`secretName`, `usernameKey` and `passwordKey`, `username` and `password` by
default, and `mountPath`, `/var/run/prometheus-universal-exporter/web-auth`
by default) to mount a Secret's two keys as files named `username` and
`password`, for the exporter's `web.basic_auth.username_file` and
`password_file` (SPECIFICATION-EXPORTER.md § 42.5), so the exporter's own
password need not be in the ConfigMap. `secretName` MUST be required when it
is enabled, and its `mountPath` MUST be a path of its own: an
`extraVolumeMounts` entry at it or below it, or it being the configuration
directory or `targetAuth.mountPath`, or inside or above either, MUST fail
rendering (§ 33.10a). While it is enabled, every monitor the chart renders MUST send its
credential as `basicAuth`, since `web.basic_auth` protects `/probe`, the
self-metrics and the static targets endpoints alike: the self-health and
static targets monitors, and each probe monitor without `auth.enabled` of its
own. Without it, a probe monitor gets a `401` on every scrape.

The chart MUST also offer `sshAuth`, for the files an `sshfile` collector
reads (SPECIFICATION-EXPORTER.md): `enabled`, false by default, `secretName`,
`privateKeyKey`, `passphraseKey`, `passwordKey`, `knownHosts`,
`knownHostsSecretKey`, each empty by default, and `mountPath`,
`/var/run/prometheus-universal-exporter/ssh` by default. Disabled, it MUST
render nothing, and the chart MUST render exactly what it renders without
the block. Enabled, it MUST render one projected volume, mounted read-only at
`mountPath`, with `defaultMode` 0400, holding `private_key` from the Secret's
`privateKeyKey`, `passphrase` from `passphraseKey` and `password` from
`passwordKey`, each only when its key is set, and `known_hosts` from the
Secret's `knownHostsSecretKey` or from `knownHosts`, which the chart MUST
render as the key `known_hosts` of a ConfigMap of its own,
`<fullname>-ssh-known-hosts`, written as the configuration's files are
(§ 33.2). The files are readable by the exporter through the pod's `fsGroup`,
which the default `podSecurityContext` sets to the exporter's group; the
chart README MUST say so. The inline lines MUST be part of the pod
template's `checksum/config`, so changing them rolls the pods (§ 33.3), and
without them that checksum MUST be the configuration's alone. While it is
enabled, rendering MUST fail, by the values schema and by the templates with
the schema skipped, with a message saying what to set, when `secretName` is
empty; when neither `privateKeyKey` nor `passwordKey` is set; when
`passphraseKey` is set without `privateKeyKey`; when neither `knownHosts`
nor `knownHostsSecretKey` is set, or both are, since the one `known_hosts`
file comes from one source; and when `knownHosts` holds no known_hosts
entry, a line that, after blanks, does not begin with `#` and has at least
three blank-separated fields. The schema's pattern for an entry and the
templates' MUST be one. Its `mountPath` MUST be a path of its own, as
`webAuth`'s is (§ 33.10a).

The chart MUST also offer `sqlAuth`, for the files an `sql` collector reads
(SPECIFICATION-EXPORTER.md): `enabled`, false by default, `secretName`,
`passwordKey`, `privateKeyKey`, `caKey`, `certKey`, `keyKey`, each empty
by default, and `mountPath`,
`/var/run/prometheus-universal-exporter/sql` by default. Disabled, it MUST
render nothing, and the chart MUST render exactly what it renders without
the block. Enabled, it MUST render one Secret volume, mounted read-only at
`mountPath`, with `defaultMode` 0400, holding `password` from the Secret's
`passwordKey`, `private_key` from `privateKeyKey`, `ca.crt` from `caKey`,
`client.crt` from `certKey` and `client.key` from `keyKey`, each only when
its key is set; the files are readable by the exporter through the pod's
`fsGroup`, as `sshAuth`'s are. It MUST NOT offer a passphrase: the exporter
reads only an unencrypted Snowflake key and refuses `passphrase_file` on an
`sql` request, and the chart README MUST point to the conversion
docs/SQL.md gives. A `sqlAuth.passphraseKey` MUST be refused, enabled or not,
by the values schema and by the templates with the schema skipped.
While it is enabled, rendering MUST fail, by the values schema and by the
templates with the schema skipped, with a message saying what to set, when
`secretName` is empty; when none of `passwordKey`, `privateKeyKey`,
`caKey`, `certKey` and `keyKey` is set; and when `certKey` is set without `keyKey` or `keyKey`
without `certKey`. Its `mountPath` MUST be a path of its own, as `webAuth`'s
is (§ 33.10a). The block mounts one Secret, not a list of them; the chart
README MUST show how a further database's Secret is mounted through
`extraVolumes` and `extraVolumeMounts`, and an egress rule for the
databases' ports for use with `networkPolicy.egress`.

### 42.6a Shutdown and the grace period

A stopping pod needs `server.shutdownDelay` (none when empty), during which it
answers probes while `/ready` answers `503` so Kubernetes removes it from its
Service before it stops listening, then `server.shutdownTimeout` (the
exporter's 15 seconds when empty), and 10 seconds more for the last OTLP export
and exiting: 30 seconds with the defaults. The chart MUST
offer `terminationGracePeriodSeconds`, empty by default. Empty, it MUST render
none while that need is 30 seconds or less, Kubernetes' default, and the need
itself when it is more. Set, it MUST be rendered, and rendering MUST fail when
it is less than the need, naming both.

## 42.7 Helm monitor arrays and opt-in monitor authentication

The Helm chart MUST expose a `monitors` array. Each entry MUST contain a
unique resource name, which is required (§ 33.5), an `enabled` flag, and a
`type` of `pod` or `service`:

```yaml
monitors:
  - name: application-services
    enabled: true
    type: service # service or pod
```

Each enabled entry MUST render exactly one corresponding PodMonitor or
ServiceMonitor, named `<fullname>-<name>` and nothing else: the values schema
requires the name, so the templates MUST NOT carry a fallback name, such as
one made of the entry's index. The chart MUST support multiple enabled entries
and produce unique resource names. Every enabled entry MUST name a `collector`, and, with
`config.enabled`, one `config.data` defines in `config.yaml` or a collector
file among its keys, unless `config.yaml` lists collector files by absolute
path; otherwise rendering MUST fail. A probe monitor MUST send Prometheus to
the Service's full name, `<fullname>.<namespace>.svc:<port>`, so a Prometheus
in another namespace reaches it, and MUST fail rendering with
`service.enabled: false`.

With `selfMetrics.enabled`, exactly one self-health monitor MUST be rendered,
a ServiceMonitor or, with `selfMetrics.type: pod`, a PodMonitor, when the
chart renders any other monitor (a probe monitor, the static targets
monitor, a probe job's ScrapeConfig or the allocator's ServiceMonitor) or the
cluster serves that kind; a ServiceMonitor with
`service.enabled: false` MUST fail rendering, and so MUST a static targets
monitor of type `service`.

Monitor authentication MUST be explicitly opt-in and disabled by default for
each array entry:

```yaml
auth:
  enabled: false
  type: bearer # bearer or basic
```

When `auth.enabled` is false, the chart MUST NOT render `authorization`, nor
`basicAuth` other than the `webAuth` credential while `webAuth.enabled`
(§ 42.6), regardless of the configured `auth.type`. When enabled, `auth` MUST
say its `type`, `bearer` or `basic`, and its `secretName`, not empty, and the
chart MUST render the corresponding SecretKeySelectors. An enabled `auth`
without a `type`, or without a `secretName`, MUST fail rendering, in the
values schema and again in the ServiceMonitor and PodMonitor templates with a
message naming the monitor and the value to set: such an entry would
otherwise render no credential at all, or the `webAuth` credential in place
of the entry's own, or a selector naming no Secret, and Prometheus would
scrape with the wrong credential or none. The selectors' keys MUST default to
`token`, `username` and `password`, and `optional` to false, so an entry
naming only its `type` and `secretName` renders complete selectors. An
entry's own enabled `auth` MUST win over the `webAuth` credential. Tests MUST
cover the disabled default, both monitor selector types, enabled bearer/basic
authentication rendering, an enabled `auth` missing its `type` or its
`secretName` on monitors of both types, and the `webAuth` credential on probe
monitors of both types without `auth`.

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
require no overlap MAY choose `strategy.type: Recreate`. `rollingUpdate` MUST
be rendered only with `strategy.type: RollingUpdate`, or with no type, which
MUST render `RollingUpdate`: with `Recreate` it MUST be left out, the default
values' `rollingUpdate` included, since Kubernetes refuses a Deployment that
carries it beside `Recreate`, and `strategy.type: Recreate` alone MUST
therefore render a Deployment Kubernetes accepts.

## 42.10 Per-scrape request overrides

The parameters themselves, and what the exporter does with each, are specified in
[SPECIFICATION-EXPORTER.md](SPECIFICATION-EXPORTER.md) § 42.10.

The chart MUST expose these parameters as list-valued `params` entries on
each `monitors` item, and MUST expose `interval` and `scrapeTimeout` on each
item as the Prometheus Operator scrape settings; one an item leaves out MUST
be left out of the endpoint, not rendered as null, so the Operator's default
applies. The monitor scrape timeout
and the exporter target-request timeout override are distinct: the former is
set on the generated ServiceMonitor or PodMonitor, while the latter is passed
to `/probe` as `params.timeout`. Prometheus refuses a scrape timeout longer
than the scrape interval, so a monitor whose `scrapeTimeout` is longer than
its `interval` MUST fail rendering, naming the monitor and both values: an
enabled `monitors` entry, `selfMetrics` while enabled, and
`staticTargets.monitor` while it is rendered. The two MUST be compared as
Prometheus durations in every unit one may hold — `y` of 365 days, `w`, `d`,
`h`, `m`, `s` and `ms` — so `61s` is longer than `1m`; equal values MUST
render; and a monitor that sets only one of the two, or `0`, MUST render,
since Prometheus's own default stands for the other. The TLS override is passed as
`params.insecure_skip_verify`; when absent, the collector's TLS setting MUST be
preserved. `params.retry_attempts` and `params.retry_backoff` override the
collector's retry settings for that scrape; when absent, the collector values
MUST be preserved.

A monitor's `params` MUST also pass `param_<name>` entries through unchanged,
since they fill the `{{param_<name>}}` placeholders of the collector's
`request.path` (SPECIFICATION-EXPORTER.md § 42.10a). The values schema MUST NOT
restrict `params` to a fixed set of keys for the same reason; only `collector`
and `target`, which the chart renders itself, MUST fail rendering (§ 33.5).

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

- publish nothing before CI has passed on the commit the tag names
  ([SPECIFICATION-EXPORTER.md](SPECIFICATION-EXPORTER.md) § 42.12);
- verify that the version implied by the tag matches the `version` field in
  `Chart.yaml`, failing the release when they disagree;
- re-run the chart lint and template scenarios;
- package the chart without overriding `version` or `appVersion`, so the
  published artifact carries exactly what the committed `Chart.yaml` declares;
- before publishing, check that the image the chart deploys by default,
  `ghcr.io/<owner>/prometheus-universal-exporter:<appVersion>`, exists in the
  registry (`docker buildx imagetools inspect`), and fail, saying to release
  the exporter first, when it does not: a chart published ahead of its image
  leaves every default install in `ImagePullBackOff`;
- publish the chart as an OCI artifact, and sign and verify it with cosign by
  the digest `helm push` reports, never by its tag, which could be moved to
  other content between the push and the signature; and
- create a GitHub Release containing the chart archive.

The CI workflow MUST run the Go suite for changes to anything the repository
tests read — Go sources, workflows, the chart, `configs`, `examples`,
`testdata`, the docs and READMEs, the Dockerfile, `.dockerignore`, the
Makefile, `tools` and the lint configuration — and the chart steps for
changes to the chart, the files they render with and `ci.yml` itself, which
defines them: every changed-path filter a step of `ci.yml` runs under MUST
match `ci.yml`, so a change to those steps alone runs them. The image MUST report
the commit it was built from: the release workflow passes it as the
`REVISION` build argument, and `.dockerignore` keeps `.git` out of the build
context.

`Chart.yaml` MUST be the source of truth for the chart version. `appVersion`
is the exporter release a chart version was validated against and MUST be
maintained by hand rather than derived from a release tag. `image.tag` MUST
default to empty, which MUST render `appVersion`, so a chart version deploys
the exporter release it was validated against and never a moving tag such as
`latest`; a non-empty `image.tag` MUST be rendered as given. The
`artifacthub.io/images` annotation MUST name the image at `appVersion`.

## 42.15a Environment variable expansion in configuration

The expansion itself is specified in
[SPECIFICATION-EXPORTER.md](SPECIFICATION-EXPORTER.md) § 42.15a.

The chart MUST expose the flag, and MUST provide `env` and `envFrom` in the
ordinary Kubernetes shapes, because the flag is inert without a way to set
variables in the container: the material an operator wants to keep out of a
committed file generally lives in a Secret.
The same `env` is how a proxy is set: the exporter takes `HTTPS_PROXY`,
`HTTP_PROXY` and `NO_PROXY` from its environment (SPECIFICATION-EXPORTER.md
§ 42.15b), and the chart MUST NOT add a proxy value of its own. The chart
README MUST show setting them.
