# Kubernetes objects

The `kubernetes` request type lists the Kubernetes objects of one resource,
built in or custom, and hands them to the transforms as a JSON document. Use
it for the metrics kube-state-metrics gives, and for those it cannot,
from configuration alone:

- how many replicas each Deployment wants and how many are available;
- a condition of a custom resource, such as whether each cert-manager
  Certificate is Ready, and when it expires;
- how many Pods are in each phase, how many Jobs failed, which nodes are
  cordoned;
- what each Pod uses of its node's ephemeral storage, CPU and memory, from
  the nodes' kubelets ([Kubelet statistics](#kubelet-statistics)), beside
  what its containers request, read with
  [`parse_quantity`](CONFIGURATION.md#value-parsing-helpers).

It needs no client library and no other program: the exporter lists the
objects over the API server's REST API with its pod's service account, as
the [target allocator](ALLOCATOR.md#discovering-targets) lists
EndpointSlices, with the same code.

## A collector

```yaml
collectors:
  - name: deployments
    request:
      type: kubernetes
      group: apps
      version: v1
      resource: deployments
      namespaces: [shop]
      label_selector: app.kubernetes.io/part-of=shop
    transform:
      type: jq
    metrics:
      - name: kube_deployment_spec_replicas
        description: The replicas each Deployment wants
        items: .items[]
        expression: .spec.replicas
        labels:
          - name: namespace
            expression: .metadata.namespace
          - name: deployment
            expression: .metadata.name
      - name: kube_deployment_status_replicas_available
        description: The replicas of each Deployment that are available
        items: .items[]
        expression: .status.availableReplicas // 0
        labels:
          - name: namespace
            expression: .metadata.namespace
          - name: deployment
            expression: .metadata.name
```

```text
/probe?collector=deployments
```

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `kubernetes`. |
| `resource` | — | **Required.** The plural name of the resource listed, as `kubectl api-resources` lists it: `pods`, `deployments`, `certificates`. Lower-case letters, digits and hyphens; a subresource, such as `pods/status`, is refused. The core group's `secrets` is refused but with `metadata_only`: see [Secrets](#secrets). |
| `group` | the core group | The resource's API group: `apps`, `batch`, `cert-manager.io`. Left out for the core group, of pods, services, nodes and namespaces, which has no name; `core` and `v1` are refused. |
| `version` | `v1` | The resource's API version: `v1`, `v1beta1`. |
| `namespaces` | the exporter's own | The namespaces listed, each at its own path, in the order written. Left out, the namespace of the exporter's pod, read from its service account. |
| `all_namespaces` | `false` | List the resource across the cluster: every namespace's objects, or a cluster-scoped resource's, such as `nodes`. Not with `namespaces`. |
| `label_selector` | none | A [label selector](https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#label-selectors) the API server filters by: `app=shop,tier!=cache`, `environment in (prod)`. |
| `field_selector` | none | A [field selector](https://kubernetes.io/docs/concepts/overview/working-with-objects/field-selectors/), of the fields the resource can be selected by: `status.phase=Running`, `spec.nodeName=node-1`. |
| `max_objects` | `10000` | The most objects a listing may hold; one more fails the scrape. |
| `max_response_bytes` | `10MiB` | The most bytes of a page of the listing, and of the objects listed: see [Limits](#limits). |
| `api_server`, `tls`, `bearer_token_file` | the exporter's own cluster | Another cluster's API server: see [Outside the cluster](#outside-the-cluster). |
| `metadata_only` | `false` | List the objects' metadata alone: see [Metadata alone](#metadata-alone). |
| `kubelet` | none | `stats/summary`: ask each node's kubelet for what its pods use, of a listing of `nodes`: see [Kubelet statistics](#kubelet-statistics). |
| `kubelet_via_api_server`, `kubelet_ca_file` | the kubelets directly, verified against the API server's CA | How the kubelets are asked: see [Kubelet statistics](#kubelet-statistics). |

No other request key applies, and setting one — `path`, `headers`,
`allowed_targets` — is a startup error. The selectors are sent as written
and checked by the API server: one it cannot read fails the scrape with its
message. The decoder is `json`, whether `decoder.type` is left out or set to
it; any other decoder, and a transform that does not read JSON (`regex`,
`csv`, `css`, `prometheus`), is refused when the configuration loads. Use
`jq`, `yq` or `python`.

## No target

A `kubernetes` collector lists what its configuration names and takes no
`target`: a probe leaves it out, and a probe with one is answered `400`. So
whoever can reach `/probe` cannot have the exporter list anything its service
account may read beyond what the configuration names; a probe takes only
`timeout` and the `param_<name>` parameters of the collector's
[label values](REQUESTS.md#in-label-values). One collector is one listing,
of the namespaces it names, all of them at once, four at a time.

## The API server

The exporter lists the API server of the cluster it runs in, as any pod
reaches it: at the address in `KUBERNETES_SERVICE_HOST` and
`KUBERNETES_SERVICE_PORT`, with the token of its service account, and
verifying the server's certificate against the cluster's CA, both from
`/var/run/secrets/kubernetes.io/serviceaccount`. The token is read again at
every scrape, so a rotated one is used. Outside a pod, without `api_server`,
every scrape fails saying so.

The listing is the API's own: a `GET` of the resource's path,
`/api/v1/namespaces/<namespace>/pods` for the core group and
`/apis/<group>/<version>/namespaces/<namespace>/<resource>` for the others,
without the namespace for `all_namespaces`, asking for pages of 100 objects
and following each page's `continue` token to the last. A page larger than
the response limit ([Limits](#limits)) is asked for again, with the same
`continue` token, with half as many objects, down to one, and the rest of
the listing keeps the smaller size; the next scrape starts at 100 again.
The namespaces of a collector are listed at once, four at a time, so a
collector of many takes about as long as its slowest few rather than as all
of them added up; the document holds their objects in the order
`namespaces` names them, and the first listing to fail ends the others and
fails the scrape. The whole listing, every page of every namespace and
every page asked for again, and with `kubelet` every kubelet's request,
takes at most the probe's timeout; a listing with no deadline of its own,
as a static target's without one, gives up after 30 seconds.

Two answers of a busy API server are taken in stride:

- A page refused with `429 Too Many Requests`, which the API server's
  [priority and fairness](https://kubernetes.io/docs/concepts/cluster-administration/flow-control/)
  answers when it has no seat for the request, is asked for again after
  the wait its `Retry-After` asks for, a second without one, at most three
  times, and not when the wait would end after the scrape's deadline. Then
  the scrape fails, saying how many times the page was asked for again. A
  debug probe shows each wait after the request it followed.
- A page whose `continue` token the API server no longer has, answered
  `410 Gone` with the reason `Expired` because etcd compacted the version
  the listing began at, goes on with the token the answer carries for the
  rest of the listing as it is now; an answer with none starts the
  listing over, once, each object listed once. A second such answer fails
  the scrape: the objects change faster than they can be listed.

Every request names the exporter in its `User-Agent`,
`prometheus-universal-exporter/<version>`, the version `--version` prints,
so the API server's audit log tells the exporter apart from other clients
of its service account, and a FlowSchema can match it.

With [`otlp.traces.propagate`](OTLP.md#propagation-to-the-targets), each
page's request carries the probe's or the scrape's W3C `traceparent` and
`tracestate`, as an HTTP target's request does, under a span id of its own,
which its span then has: an API server traced with `APIServerTracing` makes
its spans of the listing children of the exporter's. An exported trace
shows each request's DNS lookup, connection, TLS handshake and time to the
first byte as spans under its own
([Traces of slow requests](OTLP.md#traces-of-slow-requests)), each
request's its own though the namespaces are listed at once.

The request goes through the proxy the environment names, as every request
of the exporter does ([Proxies](REQUESTS.md#proxies)). Where the pod has
`HTTPS_PROXY` set, put the API server in `NO_PROXY`: `kubernetes.default.svc`
and the address of `KUBERNETES_SERVICE_HOST`; and for a collector of the
kubelets asked directly, the nodes' addresses, such as their CIDR.

### Permissions

The exporter's service account needs `list` on what each collector lists,
in each namespace it lists, or across the cluster for `all_namespaces`; it
needs nothing else, since a collector only lists, but for a collector of the
kubelets' statistics, which also needs `get` on `nodes/stats`
([Kubelet statistics](#kubelet-statistics)). Without the permission
every scrape fails, saying what the API server refused:

```text
listing deployments (apps/v1) in the namespace shop answered 403 Forbidden:
deployments.apps is forbidden: User "system:serviceaccount:monitoring:exporter"
cannot list resource "deployments" in API group "apps" in the namespace "shop";
the exporter's service account may not list them: grant it list on deployments
in group "apps", with the chart's kubernetesAccess.rules
```

That is the error the exporter's log and a debug probe's report show. What
the API server said names the service account and the namespaces, so a
probe's caller is told the listing, the status and the advice without it:
`listing deployments (apps/v1) in the namespace shop answered 403
Forbidden; what it said is in the exporter's log; the exporter's service
account may not list them: ...`. The API server's URL is withheld from the
caller as well: a request that could not reach it is answered `listing
deployments: the request to the API server failed; why is in the
exporter's log`. See
[What a failed probe's caller is told](CONFIGURATION.md#what-a-failed-probes-caller-is-told).

The Helm chart grants it with `kubernetesAccess`: the rules name each
resource's `apiGroups` (`""` for the core group) and `resources`, the verb
is always `list`, `kubelet` adds `get` on the kubelets' statistics
([Kubelet statistics](#kubelet-statistics)), and the chart mounts the pods'
token, which it does not otherwise ([kubernetesAccess](../charts/prometheus-universal-exporter/README.md#kubernetes-collectors)):

```yaml
kubernetesAccess:
  enabled: true
  namespaces: [shop]
  rules:
    - apiGroups: [apps]
      resources: [deployments]
```

A collector with `all_namespaces`, or of a cluster-scoped resource such as
`nodes`, needs `clusterWide: true`, a ClusterRole in place of a Role in each
namespace.

### Outside the cluster

`api_server` names another API server, or the exporter's own cluster's when
it runs outside it: an `https` URL, with a path when the server is behind
one, such as a proxy's, and no user, query or fragment. Its credentials are
the request's own keys, each file read at every scrape:

```yaml
    request:
      type: kubernetes
      api_server: https://k8s.example.com:6443
      tls:
        ca_file: /etc/exporter/k8s/ca.crt
      bearer_token_file: /etc/exporter/k8s/token
      resource: nodes
      all_namespaces: true
```

- `tls.ca_file` is the CA the server's certificate is verified against,
  beside the system's roots; `tls.server_name` the name verified, when the
  URL's host is not on the certificate.
- `bearer_token_file` is the token sent, or `tls.cert_file` and
  `tls.key_file` a client certificate; one of the two is required.
- `tls.insecure_skip_verify` is refused: the token reads the cluster, and is
  never sent to a server that is not verified.
- A collector with `api_server` has no namespace of its own and lists
  `namespaces`, or sets `all_namespaces`.

Without `api_server`, `tls`, even an empty block, and `bearer_token_file`
are refused: the
exporter's own cluster is reached with its pod's service account alone.
`--dry-run` reads the token and TLS files as a scrape would and warns of one
a scrape would fail on.

## The document

```json
{
  "kind": "Deployment",
  "api_version": "apps/v1",
  "count": 2,
  "items": [
    {
      "metadata": {"name": "cart", "namespace": "shop", "labels": {"app.kubernetes.io/part-of": "shop"}},
      "spec": {"replicas": 3},
      "status": {"replicas": 3, "availableReplicas": 2, "conditions": [{"type": "Available", "status": "True"}]}
    },
    {
      "metadata": {"name": "web", "namespace": "shop", "labels": {"app.kubernetes.io/part-of": "shop"}},
      "spec": {"replicas": 2},
      "status": {"replicas": 2}
    }
  ]
}
```

- `kind` is the objects' kind, the list's kind as the API server named it
  without `List`, and `api_version` their group and version.
- `count` is the number of objects, `items | length`.
- `items` are the objects in the order listed: namespace by namespace, in
  the order of `namespaces`, and within one as the API server orders them,
  by name. Each is as the API server sent it, but for its
  `metadata.managedFields`, which say which client set which field: often
  the largest part of an object, and of no use to a metric. The objects of
  a built-in resource carry no `kind` and `apiVersion` of their own in a
  list; those of a custom resource do.
- A field the API server leaves out is missing, not `0` or `null`: a
  Deployment with no available replica has no `availableReplicas`. Write
  `// 0` where a missing count means none.

## Metadata alone

`metadata_only: true` lists the objects' metadata and nothing else: the API
server answers a `PartialObjectMetadataList`, each object's name,
namespace, labels, annotations, owners and times, without its spec, its
status or its data. The pages are far smaller, which matters for objects
whose bulk is of no use to a metric, as a ConfigMap's data, and it is the
one way to list Secrets ([Secrets](#secrets)).

```yaml
  - name: configmaps
    request:
      type: kubernetes
      resource: configmaps
      all_namespaces: true
      metadata_only: true
    transform:
      type: jq
    metrics:
      - name: kube_configmap_created_timestamp_seconds
        items: .items[]
        expression: .metadata.creationTimestamp | fromdateiso8601
        labels:
          - name: namespace
            expression: .metadata.namespace
          - name: configmap
            expression: .metadata.name
```

- The document's `kind` is `PartialObjectMetadata` and its `api_version`
  `meta.k8s.io/v1`, as the API server names the objects, whether there are
  any or not; each object has `metadata` alone.
- Beside the managed fields, each object loses the annotation
  `kubectl.kubernetes.io/last-applied-configuration`, which `kubectl apply`
  writes with the whole object it applied: a Secret's values among them.
- The pages are asked for with that media type alone. An answer that is not
  a `PartialObjectMetadataList`, from a proxy before the API server that
  answers the objects whole, fails the scrape, and nothing of it reaches the
  transforms; an API server that cannot answer it says `406`.
- It needs the same `list` permission, and cannot be with `kubelet`.

## Secrets

A collector of the core group's `secrets` is refused when the configuration
loads, unless it lists their metadata alone, `metadata_only: true`: their
values would pass through the transforms, where an error, a debug report or
a label could show them, and a metrics exporter has no business reading
them. Of their metadata, a metric of each Secret's existence, labels or
age:

```yaml
  - name: secrets
    request:
      type: kubernetes
      resource: secrets
      namespaces: [shop]
      metadata_only: true
    transform:
      type: jq
    metrics:
      - name: kube_secret_created_timestamp_seconds
        items: .items[]
        expression: .metadata.creationTimestamp | fromdateiso8601
        labels:
          - name: secret
            expression: .metadata.name
```

The chart's `kubernetesAccess` still refuses to grant `secrets`: `list` on
them lets the account's token read their values, whatever the exporter asks
for, so that grant is yours to make, with a Role of your own, knowing what it
allows. A custom resource named `secrets` in another group is listed as any
other.

## Kubelet statistics

What a pod uses of its node — its ephemeral storage, the bytes its
containers' writable layers, logs and `emptyDir` volumes take, its CPU and
memory, its volumes' — is in no object of the API. The kubelet of each node
has it, in its Summary API. `kubelet: stats/summary`, beside
`resource: nodes` and `all_namespaces: true`, lists the nodes and asks each
one's kubelet for its summary:

```yaml
  - name: ephemeral_storage
    request:
      type: kubernetes
      resource: nodes
      all_namespaces: true
      kubelet: stats/summary
    transform:
      type: jq
    metrics:
      - name: kube_pod_ephemeral_storage_used_bytes
        description: The bytes each pod uses of its node's ephemeral storage
        items: .items[].pods[] | select(."ephemeral-storage")
        expression: '."ephemeral-storage".usedBytes'
        labels:
          - name: namespace
            expression: .podRef.namespace
          - name: pod
            expression: .podRef.name
      - name: kube_node_fs_available_bytes
        items: .items[]
        expression: .node.fs.availableBytes
        labels:
          - name: node
            expression: .node.nodeName
      - name: kube_kubelet_summaries_failed
        description: The nodes whose kubelet answered no summary
        expression: .failed | length
```

and what each container requests, from the Pods, its quantity read by
[`parse_quantity`](CONFIGURATION.md#value-parsing-helpers):

```yaml
  - name: ephemeral_storage_requests
    request:
      type: kubernetes
      resource: pods
      all_namespaces: true
    transform:
      type: jq
    metrics:
      - name: kube_pod_container_ephemeral_storage_request_bytes
        items: .items[] | {namespace: .metadata.namespace, pod: .metadata.name} as $pod | .spec.containers[] | select(.resources.requests["ephemeral-storage"]) | $pod + {container: .name, request: .resources.requests["ephemeral-storage"]}
        expression: .request | parse_quantity
        labels:
          - name: namespace
            expression: .namespace
          - name: pod
            expression: .pod
          - name: container
            expression: .container
```

The document is the summaries, and the nodes that gave none:

```json
{
  "kind": "Summary",
  "api_version": "stats/v1alpha1",
  "count": 1,
  "items": [
    {
      "node": {"nodeName": "node-a", "fs": {"availableBytes": 81604378624, "capacityBytes": 105089261568, "usedBytes": 23484882944}},
      "pods": [
        {
          "podRef": {"name": "web-7c9d", "namespace": "shop"},
          "ephemeral-storage": {"availableBytes": 81604378624, "capacityBytes": 105089261568, "usedBytes": 1048576},
          "containers": [{"name": "app", "rootfs": {"usedBytes": 917504}, "logs": {"usedBytes": 131072}}],
          "volume": [{"name": "cache", "usedBytes": 0}]
        }
      ]
    }
  ],
  "failed": [{"node": "node-b", "error": "the kubelet answered 403 Forbidden"}]
}
```

- `items` are the summaries, each as its kubelet answered it, in the order
  the nodes were listed; `label_selector` and `field_selector` choose the
  nodes, `max_objects` bounds them.
- `failed` are the nodes whose kubelet answered none, and why, in words that
  name no address: its status, that the request failed, that its Node has
  no address, that its answer is not JSON, or that its summary is larger
  than the response limit. One node down leaves the others' series: a
  kubelet that does not answer is given up on a tenth of the scrape's time
  left before its deadline, at most a second before, and its node is among
  the failed with `the request to the kubelet failed: context deadline
  exceeded`. When no kubelet answers, the scrape fails as the first one
  did, saying what to do about it.
- Each summary is held to the response limit, and so are all of them
  together: a large cluster's need a `max_response_bytes` above 10 MiB, or
  a collector of each pool, chosen by `label_selector`.

Each kubelet is asked at its Node's address, `InternalIP` first, then
`Hostname` and `ExternalIP`, and the port the Node says its kubelet serves
on, 10250 by default, four at a time, with the token the nodes are listed
with. The service account needs `list` on `nodes` and `get` on
`nodes/stats`, which the chart's `kubernetesAccess.kubelet: stats` grants
beside a rule of `nodes`, with `clusterWide`:

```yaml
kubernetesAccess:
  enabled: true
  clusterWide: true
  kubelet: stats
  rules:
    - apiGroups: [""]
      resources: [nodes]
```

The kubelet's certificate is verified, since the token is sent to it:
against `kubelet_ca_file`, or the CA the API server is verified against
without it, the cluster's in its own cluster. That holds where the kubelets'
serving certificates are signed by the cluster's CA, as with
`serverTLSBootstrap`, or by a CA you give in `kubelet_ca_file`; where each
kubelet signs its own, no CA verifies them and every node fails saying so.
Then `kubelet_via_api_server: true` asks each kubelet through the API
server's proxy of its node, `/api/v1/nodes/<name>/proxy/stats/summary`,
which verifies the kubelets itself. That needs `get` on `nodes/proxy`
(`kubernetesAccess.kubelet: proxy`), which lets its holder do all a kubelet
does, running commands in containers among it: prefer `nodes/stats` where
the certificates allow it. With `networkPolicy.egress` rules, the direct
way needs one for the kubelets' port on the nodes' addresses.

## Limits

- `max_objects`, 10000 by default, bounds the objects of a listing, across
  its namespaces; one more fails the scrape as a limit exceeded. Narrow the
  listing with `namespaces`, `label_selector` or `field_selector` rather
  than raising it.
- The collector's response limit, `request.max_response_bytes` or
  `limits.max_response_bytes`, 10 MiB when neither is set, bounds each page
  as the API server sends it, managed fields included, and the objects of
  the whole listing, without them. A page over it is not taken but asked
  for again with half as many objects, so large objects, such as custom
  resources with large specs, make smaller pages rather than a failed
  scrape; only a single object larger than the limit, or objects that come
  to more than it, fail the scrape as a limit exceeded. A page holds at
  most 100 objects and never more than the limit, so the limit bounds what
  a scrape holds in memory whatever the cluster's size.

## Examples

Every example lists from the exporter's own cluster; give each collector
`kubernetesAccess` rules for its resource.

Each Deployment's replicas, wanted and available, and whether all are:

```yaml
  - name: deployment_replicas
    request:
      type: kubernetes
      group: apps
      resource: deployments
      namespaces: [shop]
    transform:
      type: jq
    metrics:
      - name: kube_deployment_spec_replicas
        items: .items[]
        expression: .spec.replicas
        labels:
          - name: deployment
            expression: .metadata.name
      - name: kube_deployment_status_replicas_available
        items: .items[]
        expression: .status.availableReplicas // 0
        labels:
          - name: deployment
            expression: .metadata.name
      - name: kube_deployment_fully_available
        items: .items[]
        expression: if (.status.availableReplicas // 0) >= .spec.replicas then 1 else 0 end
        labels:
          - name: deployment
            expression: .metadata.name
```

A condition of a custom resource as a gauge: whether each cert-manager
Certificate is Ready, and when it expires.

```yaml
  - name: certificates
    request:
      type: kubernetes
      group: cert-manager.io
      resource: certificates
      all_namespaces: true
    transform:
      type: jq
    metrics:
      - name: certmanager_certificate_ready
        items: .items[]
        expression: if any(.status.conditions[]?; .type == "Ready" and .status == "True") then 1 else 0 end
        labels:
          - name: namespace
            expression: .metadata.namespace
          - name: certificate
            expression: .metadata.name
      - name: certmanager_certificate_expiration_timestamp_seconds
        items: .items[] | select(.status.notAfter)
        expression: .status.notAfter | fromdateiso8601
        labels:
          - name: namespace
            expression: .metadata.namespace
          - name: certificate
            expression: .metadata.name
```

How many Pods are in each phase, every phase a series even when no Pod is
in it:

```yaml
  - name: pod_phases
    request:
      type: kubernetes
      resource: pods
      all_namespaces: true
    transform:
      type: jq
    metrics:
      - name: kube_pods
        items: '[.items[].status.phase] as $phases | ["Pending", "Running", "Succeeded", "Failed", "Unknown"][] | . as $p | {phase: $p, count: ([$phases[] | select(. == $p)] | length)}'
        expression: .count
        labels:
          - name: phase
            expression: .phase
```

## Static targets

A [static target](STATIC-TARGETS.md) of a `kubernetes` collector lists on the
exporter's own timer, and names no `target`; it may set `timeout`:

```yaml
interval: 1m
targets:
  - name: shop_deployments
    collector: deployments
```

## Errors and self-metrics

A failed listing is reported in the `kubernetes` stage and follows
`error_handling.on_fetch_error`. The API server's refusals say what to do:
`401`, a token it does not take; `403`, a permission the service account
lacks ([Permissions](#permissions)); `404`, a resource it does not have, of
a misspelt name, group or version or of a custom resource whose definition
is not installed, or a cluster-scoped resource listed in a namespace, which
`all_namespaces` lists; `429`, how many times the page was asked for again;
`410`, a listing that expired again after it started over; `406`, an API
server that does not answer the metadata alone. Transforms see no status (`$status` is `null`,
Python's `response.status_code` `None`).

The verbose self-metrics label a listing `url="kubernetes:/apis/apps/v1/deployments"`,
the path of the listing across the cluster, or with `api_server` its URL
before the path, and `http_method="LIST"`; which namespaces are listed is the
collector's. A collector has no target to refuse, so no
`http_exporter_targets_refused_total` series.

A [debug probe](CONFIGURATION.md#debug-probes) shows each page's
request, `GET` and its URL with the `continue` token masked and the
`limit` and the selectors shown, the bearer token
masked, and the status it was answered with, a page over the response
limit with its size and the limit after the status and followed by the
same page's request with half the size, a page refused with `429` with the
wait after it, each kubelet's request of a kubelet collector after the
pages of the nodes, and then the document.
