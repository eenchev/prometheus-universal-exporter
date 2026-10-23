# Kubernetes objects

The `kubernetes` request type lists the Kubernetes objects of one resource,
built in or custom, and hands them to the transforms as a JSON document. Use
it for the metrics kube-state-metrics gives, and for those it cannot,
from configuration alone:

- how many replicas each Deployment wants and how many are available;
- a condition of a custom resource, such as whether each cert-manager
  Certificate is Ready, and when it expires;
- how many Pods are in each phase, how many Jobs failed, which nodes are
  cordoned.

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
| `resource` | — | **Required.** The plural name of the resource listed, as `kubectl api-resources` lists it: `pods`, `deployments`, `certificates`. Lower-case letters, digits and hyphens; a subresource, such as `pods/status`, is refused. The core group's `secrets` is refused: see [Secrets](#secrets). |
| `group` | the core group | The resource's API group: `apps`, `batch`, `cert-manager.io`. Left out for the core group, of pods, services, nodes and namespaces, which has no name; `core` and `v1` are refused. |
| `version` | `v1` | The resource's API version: `v1`, `v1beta1`. |
| `namespaces` | the exporter's own | The namespaces listed, each at its own path, in the order written. Left out, the namespace of the exporter's pod, read from its service account. |
| `all_namespaces` | `false` | List the resource across the cluster: every namespace's objects, or a cluster-scoped resource's, such as `nodes`. Not with `namespaces`. |
| `label_selector` | none | A [label selector](https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#label-selectors) the API server filters by: `app=shop,tier!=cache`, `environment in (prod)`. |
| `field_selector` | none | A [field selector](https://kubernetes.io/docs/concepts/overview/working-with-objects/field-selectors/), of the fields the resource can be selected by: `status.phase=Running`, `spec.nodeName=node-1`. |
| `max_objects` | `10000` | The most objects a listing may hold; one more fails the scrape. |
| `max_response_bytes` | `10MiB` | The most bytes of a page of the listing, and of the objects listed: see [Limits](#limits). |
| `api_server`, `tls`, `bearer_token_file` | the exporter's own cluster | Another cluster's API server: see [Outside the cluster](#outside-the-cluster). |

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
of the namespaces it names.

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
The whole listing, every page of every namespace and every page asked for
again, takes at most the probe's timeout; a listing
with no deadline of its own, as a static target's without one, gives up
after 30 seconds.

The request goes through the proxy the environment names, as every request
of the exporter does ([Proxies](REQUESTS.md#proxies)). Where the pod has
`HTTPS_PROXY` set, put the API server in `NO_PROXY`: `kubernetes.default.svc`
and the address of `KUBERNETES_SERVICE_HOST`.

### Permissions

The exporter's service account needs `list` on what each collector lists,
in each namespace it lists, or across the cluster for `all_namespaces`; it
needs nothing else, since a collector only lists. Without the permission
every scrape fails, saying what the API server refused:

```text
listing deployments (apps/v1) in the namespace shop answered 403 Forbidden:
deployments.apps is forbidden: User "system:serviceaccount:monitoring:exporter"
cannot list resource "deployments" in API group "apps" in the namespace "shop";
the exporter's service account may not list them: grant it list on deployments
in group "apps", with the chart's kubernetesAccess.rules
```

The Helm chart grants it with `kubernetesAccess`: the rules name each
resource's `apiGroups` (`""` for the core group) and `resources`, the verb
is always `list`, and the chart mounts the pods' token, which it does not
otherwise ([kubernetesAccess](../charts/prometheus-universal-exporter/README.md#kubernetes-collectors)):

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

## Secrets

A collector of the core group's `secrets` is refused when the configuration
loads, and the chart refuses to grant them: their values would pass through
the transforms, where an error, a debug report or a label could show them,
and a metrics exporter has no business reading them. A Secret's existence
or age is better watched from what uses it. A custom resource named
`secrets` in another group is listed as any other.

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
`all_namespaces` lists. Transforms see no status (`$status` is `null`,
Python's `response.status_code` `None`).

The verbose self-metrics label a listing `url="kubernetes:/apis/apps/v1/deployments"`,
the path of the listing across the cluster, or with `api_server` its URL
before the path, and `http_method="LIST"`; which namespaces are listed is the
collector's. A collector has no target to refuse, so no
`http_exporter_targets_refused_total` series.

A [debug probe](CONFIGURATION.md#debug-probes) shows each page's
request, `GET` and its URL with the query's values masked, the token
masked, and the status it was answered with, a page over the response
limit with its size and the limit after the status and followed by the
same page's request with half the size, and then the document.
