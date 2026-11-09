# Prometheus Universal Exporter — `kubernetes` Request Type Specification

This is the `kubernetes` request type's section of § 5.1 of the
[exporter specification](SPECIFICATION-EXPORTER.md), in a document of its
own so that a change to the type is a change to this document. Everything
the request types share — the registry and the rules every type is held to
(§ 5.1), building without a type, decoding, transforms, error policies,
limits, caching, static targets and exposition — is in the exporter
specification, and a section number here, such as § 42.14, is one of its
sections. What a user reads of the type is [KUBERNETES.md](KUBERNETES.md).

## `kubernetes`

A `kubernetes` collector lists the objects of one resource of a Kubernetes
API server, built in or custom, or, with `kubelet`, asks the kubelet of
each node it lists for its Summary API, and hands them to the transforms as
a JSON document. The listing MUST be made over the API's REST interface with the
standard library alone, through `internal/kubeapi`, the code the target
allocator's Kubernetes discovery (§ 42.14d) lists EndpointSlices with. Its
keys:

| Key | Default | Rule |
| --- | --- | --- |
| `resource` | none | **Required.** A resource's plural name: lower-case letters, digits and `-`, at most 63 characters; one with `/`, a subresource, MUST be refused at load. |
| `group` | the core group | An API group's name, a lower-case DNS subdomain of at most 253 characters; `core` and `v1` MUST be refused at load, the core group being left out. |
| `version` | `v1` | Lower-case letters, digits and `-`, at most 63 characters. |
| `namespaces` | the exporter's own | Each a namespace's name; one listed twice MUST be refused at load. |
| `all_namespaces` | `false` | The listing across the cluster; with `namespaces` MUST be refused at load. |
| `label_selector`, `field_selector` | none | Sent as written; blanks alone MUST be refused at load. |
| `max_objects` | 10000 | Zero or more, `0` the default; negative MUST be refused at load. |
| `max_response_bytes` | 10 MiB | As every type's response limit (§ 20). |
| `api_server` | the exporter's own cluster's | An `https` URL with a host, a path or not, and no user, query or fragment; anything else MUST be refused at load. |
| `tls`, `bearer_token_file` | none | Only with `api_server`, MUST be refused at load without it, a written `tls` block even when empty (`tls: {}`). With it, `bearer_token_file` or `tls.cert_file` and `tls.key_file` (both or neither) MUST be set, and `tls.insecure_skip_verify` true MUST be refused. |
| `metadata_only` | `false` | The objects' metadata alone; with `kubelet` MUST be refused at load. |
| `kubelet` | none | `stats/summary`, anything else MUST be refused at load; only with `resource: nodes` of the core group and `all_namespaces: true`, MUST be refused at load otherwise. |
| `kubelet_via_api_server` | `false` | Only with `kubelet`; with `kubelet_ca_file` MUST be refused at load. |
| `kubelet_ca_file` | the API server's CA | Only with `kubelet`, MUST be refused at load without it; read at every scrape, and by `--dry-run`, which warns of one a scrape would fail on. |

- The core group's `secrets` MUST be refused at load without
  `metadata_only: true`, by the exporter and by the schema (§ 24.3): their
  values would pass through the transforms.
- With `metadata_only`, every page MUST be asked for with `Accept:
  application/json;as=PartialObjectMetadataList;g=meta.k8s.io;v=v1` and no
  other media type, and a page whose `kind` is not
  `PartialObjectMetadataList` MUST fail the scrape without anything of it
  handed on, naming the kind it is; each object MUST keep its `kind`,
  `apiVersion` and `metadata` alone, whatever else the answer carried, and
  MUST lose its `metadata.managedFields` and the annotation
  `kubectl.kubernetes.io/last-applied-configuration`, its other annotations
  kept; the document's `kind` MUST be `PartialObjectMetadata` and its
  `api_version` `meta.k8s.io/v1`, whether it has objects or not.
- A collector MUST take no target: a probe with a `target` MUST be
  answered `400` before anything is listed, and a static target that names
  one MUST be refused at load; a probe without one is complete. A probe
  MUST take `timeout` and `param_<name>` parameters, and a static target's
  request block `timeout`; `allowed_targets` and `denied_targets` MUST be
  refused at load, there being no target to hold to them.
- Without `api_server`, the API server MUST be the one of the cluster the
  exporter runs in, at `https://` and the address in
  `KUBERNETES_SERVICE_HOST` and `KUBERNETES_SERVICE_PORT`, its certificate
  verified against `ca.crt` of the service account directory,
  `/var/run/secrets/kubernetes.io/serviceaccount`, and the directory's
  `token` sent as a bearer token; without those variables a scrape MUST
  fail saying the exporter does not run in a pod. With `api_server`, the
  server MUST be verified against `tls.ca_file` and the system's roots, and
  `bearer_token_file`'s token or the client certificate sent. The token
  and the TLS files MUST be read at every scrape, so a rotated one is used;
  `--dry-run` MUST read those of a collector with `api_server` as a scrape
  would and warn of one a scrape would fail on, without failing the check.
- Without `namespaces` and `all_namespaces`, the namespace listed MUST be
  the exporter's own, read from the service account directory's
  `namespace` at every scrape; with `api_server` the scrape MUST fail
  saying to list `namespaces`. The path listed MUST be
  `/api/<version>/namespaces/<namespace>/<resource>` for the core group and
  `/apis/<group>/<version>/namespaces/<namespace>/<resource>` for the
  others, without `/namespaces/<namespace>` for `all_namespaces`, one
  listing a namespace. The listings of a collector's namespaces MUST be
  made at once, at most four at a time, and the document MUST hold the
  objects in the order of `namespaces`; the first listing to fail MUST end
  the others and fail the scrape.
- Every request MUST carry the `User-Agent` `prometheus-universal-exporter/
  <version>`, the allocator's `pue-allocator/<version>`, the version as
  `--version` reports it.
- A page answered `429 Too Many Requests` MUST be asked for again after the
  wait its `Retry-After` asks for, in seconds or as an HTTP date, a second
  without one the exporter can read, at most three times, and not when the
  wait would end at or after the scrape's deadline; a debug report MUST
  show each wait on the request it followed. A page asked for with a
  `continue` token answered `410` with a Status of reason `Expired` MUST be
  asked for again with the Status's `metadata.continue` when it has one;
  without one the listing MUST start again from its first page once, the
  objects it had listed put away, so each is in the document once, and a
  second such answer MUST fail the scrape.
- A listing MUST ask for pages with `limit=100` and follow each page's
  `metadata.continue` to the last, sending `labelSelector` and
  `fieldSelector` as written when they are set. A page larger than the
  response limit MUST be read no further than one byte past it, MUST NOT
  be taken, counted towards `max_objects` or the objects' bytes, and MUST
  be asked for again with the same `continue` and half the limit, rounded
  down, down to `limit=1`; the rest of the listing MUST keep the smaller
  limit, and the next scrape MUST start at `limit=100` again. The whole
  listing, the pages asked for again included, MUST take at most the
  scrape's deadline, 30 seconds without one.
- The answer MUST be a JSON document of `kind`, the list's kind without
  its `List` suffix, `api_version`, `count` and `items`, the objects in the
  order listed, each as the API server sent it without its
  `metadata.managedFields`; a listing of no objects has `items` empty and,
  when the API server named no kind, the collector's group and version as
  `api_version`.
- More objects than `max_objects` MUST fail the scrape as a limit exceeded.
  A page of one object larger than the response limit MUST fail the scrape
  as a limit exceeded, saying a single object is larger than the limit and
  naming it in bytes; so MUST objects that come to more than it without
  their managed fields, and a document larger than it.
- A failed listing MUST be reported in the `kubernetes` stage. An API
  server's answer other than `200` MUST fail the scrape naming the
  resource, its group and version, the namespace or the cluster, the
  status and the Status object's message, and for `401` that the token is
  not taken, for `403` that the service account needs `list` on the
  resource and the chart's `kubernetesAccess.rules` that grant it, for
  `404` how to name a resource and that a cluster-scoped one is listed with
  `all_namespaces`, for `429` how many times the page was asked for again,
  for `410` that the listing expired again, and for `406` that the API
  server does not answer the metadata alone. The token MUST NOT appear in
  an error. The Status
  object's message, or the body that is none, MUST be the operator's alone
  (§ 19.4): a probe's caller MUST be answered the listing and the status,
  `; what it said is in the exporter's log`, and the advice. A page that is
  not a list MUST be answered to the caller without the API server's URL,
  and a request that failed, unanswered, as `listing <resource>: the
  request to the API server failed`, with `: context deadline exceeded` or
  `: context canceled` when the scrape's end ended it, and `; why is in the
  exporter's log`; the error MUST keep the URL and the network's words.
- The decoder MUST be `json`, as for `grpc`; another decoder, and a
  transform other than `jq`, `yq` and `python`, MUST be refused at load.
- A debug probe's request section MUST show each page's `GET` and URL, its
  query's values masked, its `Authorization` masked, and its status, a
  page asked for again included; a page larger than the response limit
  MUST show after its status its limit and the response limit. The
  verbose `url` label MUST be `kubernetes:` and the path of the listing
  across the cluster, or with `api_server` its URL before that path, and
  `http_method` MUST be `LIST`.
- With `kubelet`, after the nodes are listed, the kubelet of each MUST be
  asked, at most four at a time, with `GET` of `/stats/summary` and the
  listing's token: at `https://` and the Node's first address of type
  `InternalIP`, `Hostname` or `ExternalIP`, in that order, and
  `status.daemonEndpoints.kubeletEndpoint.Port`, 10250 when it names none,
  the kubelet's certificate verified against `kubelet_ca_file`, or the CA
  the API server is verified against without it, with the listing's
  client certificate if any; or, with `kubelet_via_api_server`, at the API
  server's `/api/v1/nodes/<name>/proxy/stats/summary` with the listing's
  client. Each answer MUST be at most the response limit and JSON, and all
  of them together at most the response limit, which past it MUST fail
  the scrape as a limit exceeded. The document MUST be of `kind`
  `Summary`, `api_version` `stats/v1alpha1`, `count`, `items`, each
  summary as its kubelet answered it in the order the nodes were listed,
  and `failed`, a `node` and an `error` for each node whose kubelet
  answered none, the error naming no address: `the kubelet answered
  <status>`, that the Node has no address, that the summary is larger than
  the response limit, that the answer is not JSON, or `the request to the
  kubelet failed`, with `: context deadline exceeded` when its time ran
  out. The kubelets' requests MUST end a tenth of the scrape's time left
  when they begin before its deadline, at most a second before, so that a
  kubelet that does not answer is among the failed and the others'
  summaries are answered; the scrape's own end before then MUST fail it,
  saying it ended while the kubelets were asked. A collector of nodes none
  of whose kubelets answered MUST
  fail the scrape with the first's failure, for `403` naming `nodes/stats`,
  or `nodes/proxy` with `kubelet_via_api_server`, and the chart's
  `kubernetesAccess.kubelet`, and for a certificate no CA signs
  `kubelet_ca_file` and `kubelet_via_api_server`; a probe's caller MUST be
  answered it without the kubelet's address. No node is a document with
  `items` and `failed` empty.
