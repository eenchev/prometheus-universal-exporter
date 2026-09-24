# gRPC

The `grpc` request type calls one unary gRPC method on the target and hands
the answer to the transforms as JSON. jq, yq or a Python script then map it to
Prometheus metrics, with the same rules, limits, caching and error policies as
any other collector.

The call is described in configuration alone: the method, the request message
as JSON, and metadata. The message types come from the server's reflection
service, a descriptor set file, or `.proto` files compiled when the
configuration loads, so no service needs code generated for it, and no new
service needs a new build of the exporter.

## A collector

```yaml
collectors:
  - name: queue_stats
    request:
      type: grpc
      rpc: acme.queue.v1.QueueService/GetStats
      message: '{"queue": {{param_queue|json}}, "include_shards": true}'
      metadata:
        x-tenant: "{{param_tenant:default}}"
      descriptors: reflection
    transform:
      type: jq
    metrics:
      - name: queue_depth
        description: Messages waiting in each shard
        items: .shards[]
        expression: .depth
        labels:
          - name: shard
            expression: .id
      - name: queue_total
        description: Messages waiting in the queue
        expression: .total
```

The probe's `target` is the server, `host:port`:

```text
/probe?target=queue.internal:9090&collector=queue_stats&param_queue=orders
  -> acme.queue.v1.QueueService/GetStats on queue.internal:9090
     {"queue": "orders", "include_shards": true}   x-tenant: default
```

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `grpc`. |
| `rpc` | — | **Required.** The method, `package.Service/Method`. Checked for its shape when the configuration loads, and against the descriptors then too with `protoset` and `proto`, or at the first call with `reflection`. |
| `message` | `{}` | The request message in the [protobuf JSON mapping](https://protobuf.dev/programming-guides/json/). May use [placeholders](#placeholders). Must be JSON once its placeholders take their defaults. |
| `metadata` | — | Request metadata. Keys are lower-case letters, digits and `- _ .`; values may use placeholders. Binary `-bin` keys, `grpc-` keys and the ones gRPC and HTTP/2 set themselves (`content-type`, `te`, `host`, `user-agent`, `:`…) are refused. |
| `descriptors` | — | **Required**, but for the [health service](#health-checks). Where the message types come from: `reflection`, `protoset` or `proto`. See [Descriptors](#descriptors). |
| `protoset_file` | — | With `descriptors: protoset`: a `FileDescriptorSet`. |
| `proto_files` | — | With `descriptors: proto`: the `.proto` files that define the service. |
| `proto_import_paths` | the directory of each file | With `descriptors: proto`: the directories `import` statements are resolved in. |
| `tls` | none | `ca_file`, `cert_file`, `key_file`, `insecure_skip_verify`, `server_name`, as for `http`. Any of them makes the call use TLS. See [TLS](#tls). |
| `bearer_token`, `bearer_token_file`, `basic_auth`, `basic_auth_file` | — | Sent as the `authorization` metadata. See [Authentication](#authentication). |
| `forward_authorization`, `forward_headers` | off | The probe's `Authorization` and listed headers, forwarded as metadata. |
| `retry` | none | `attempts`, `backoff`, and `codes`, the status codes retried: `[UNAVAILABLE]` when left out. See [Errors and retries](#errors-and-retries). |
| `max_response_bytes` | the collector's limit | The largest answer accepted: the message as it arrives, and the JSON it becomes, which writing every zero value can make several times larger. |

`path`, `query`, `headers`, `body`, `method`, `follow_redirects`,
`enable_http2` and `allowed_schemes` are `http`'s and refused, as any key of
another request type is. The method is `rpc`, not `method`, because `method`
is already the HTTP verb, and a key that meant two things by type would read
the same and check differently.

`decoder.type` left out is `json`, which is what a call is answered with;
another decoder is refused. The transform is `jq`, `yq` or `python`, which
read JSON.

## Targets

A target is `host:port`, `dns:///host:port`, `grpc://host:port` or
`grpcs://host:port`, with the port always given. A bare target is
plaintext, as a bare `http` target is `http://`. `grpcs://`, or any key of
`request.tls`, makes the call use TLS; `grpc://` with a `tls` block
contradicts it and is refused. A target with a path, a query or a user in it
is refused: the method is `rpc`. A static target's target is checked when the
file loads, and a probe's before anything is called, answered `400` when it is
malformed.

Calls to one target, with one set of TLS settings, share one connection,
kept between probes and closed after 5 minutes unused, and made again with a
new certificate when a TLS file changes on disk. It is made through the proxy
the environment names, `HTTPS_PROXY` and `NO_PROXY`, as the other types'
requests are. A name that resolves to several addresses is called on the
first that answers; a probe asks one target, and balancing across the
addresses is Prometheus's service discovery's to do.

## Descriptors

To encode the JSON message and decode the answer, the exporter needs the
method's input and output types. It builds them at run time from
descriptors, from the source `descriptors` names. There is no default: which
one suits depends on the server, and a source chosen by default would be found
wrong only at the first scrape.

| Source | How | Checked | Suits |
| --- | --- | --- | --- |
| `reflection` | Asks the target's `grpc.reflection.v1` service, or `v1alpha` for a server without v1, for the file that defines the service and the files it imports. | At the first call to each target. | Servers that register reflection, as most internal services do. |
| `protoset` | Reads a `FileDescriptorSet` from `protoset_file`. | When the configuration loads, and by `--dry-run`: the service, the method, that it is unary, and that `message` fits it. | Servers without reflection, when a build already writes descriptor sets. |
| `proto` | Compiles `proto_files`, resolving imports in `proto_import_paths`, the well-known types built in. | As `protoset`, plus compile errors with the file and line. | Servers without reflection, when their `.proto` files are at hand: mounted from the service's repository, or from a ConfigMap, with no build step. |

A reflection answer is kept for each connection and service for 10 minutes,
so a scrape costs one call rather than two, and probes that find no answer at
the same time share one question to the server. A call that fails with
`UNIMPLEMENTED`, or whose answer does not decode, asks again and calls again,
once, which covers a server upgraded to a new schema; this is not one of the
retries. A target without reflection fails saying so, and to register it on
the server or use `protoset` or `proto`.

Only unary methods are called. A streaming method is refused when the
configuration loads with `protoset` and `proto`, and at the first call with
`reflection`, saying which kind it is.

### A descriptor set

```yaml
collectors:
  - name: queue_stats_protoset
    request:
      type: grpc
      rpc: acme.queue.v1.QueueService/GetStats
      message: '{"queue": "orders"}'
      descriptors: protoset
      protoset_file: /etc/exporter/protos/queue.pb
    transform:
      type: jq
    metrics:
      - name: queue_total
        expression: .total
```

Write the set with its imports, so it is whole:

```text
protoc --descriptor_set_out=queue.pb --include_imports acme/queue/v1/queue.proto
buf build -o queue.pb
```

An import the set does not carry is taken from the types built into the
exporter when it has them, as it has the well-known types
(`google/protobuf/*.proto`); any other missing import fails, naming it. The
file is read again when it changes on disk, so a new set is picked up at the
next call without a reload.

### .proto sources

```yaml
collectors:
  - name: queue_stats_proto
    request:
      type: grpc
      rpc: acme.queue.v1.QueueService/GetStats
      message: '{"queue": "orders"}'
      descriptors: proto
      proto_files: [/etc/exporter/protos/acme/queue/v1/queue.proto]
      proto_import_paths: [/etc/exporter/protos]
    transform:
      type: jq
    metrics:
      - name: queue_total
        expression: .total
```

`proto_import_paths` are the directories an `import` is resolved in, as
`protoc -I` takes them, and each file of `proto_files` must be under one of
them. Left out, each file's own directory is one, which suits files that
import each other by their bare names. The well-known types are built in,
and imports are resolved only on the local filesystem, not from a registry
such as Buf's. A file that does not compile fails the configuration with the
compiler's message, naming the file and line; the files are compiled again
when one of them, or a file they import, changes on disk.

### Health checks

The health service's types, `grpc.health.v1`, are built into the exporter, so
a collector calling it needs no `descriptors`:

```yaml
collectors:
  - name: grpc_health
    request:
      type: grpc
      rpc: grpc.health.v1.Health/Check
      message: '{"service": {{param_service:|json}}}'
    transform:
      type: jq
    metrics:
      - name: grpc_serving
        description: 1 when the service answers SERVING
        expression: 'if .status == "SERVING" then 1 else 0 end'
```

An empty `service` asks about the server as a whole; `param_service` asks
about one service. A service the server's health service does not know is
answered `NOT_FOUND`, which fails the probe at the `grpc` stage like any other
status, `http_exporter_scrape_grpc_status_code` reading `5`: check the name
against the server's registrations, or set `error_handling.on_fetch_error: log`
for a health check that should carry on without the series. `Watch` streams and is refused. With `descriptors` set, the health service's
types come from that source like any other service's.

## The answer

A call answered `OK` becomes an ordinary fetch result: the answer rendered as
JSON, `Content-Type: application/json`, and the answer's headers and trailers
as its headers. The JSON is the protobuf JSON mapping, with three choices that
matter for metrics:

- Fields at their zero value are written. proto3 leaves out a field at its
  zero value, so a shard with depth 0 would have no `depth` at all, and its
  rule would report a missing value; here it is `"depth": "0"`.
- Field names are as the `.proto` file writes them, `include_shards` rather
  than `includeShards`, as the service's own documentation shows them.
- 64-bit integers are strings, as the mapping has it: `"total": "1234567890123"`.
  Metric values read numeric strings as numbers, and labels take them as
  written. Beyond 2⁵³ a value loses precision as a float64 sample, as any
  large number does. Enums are their names, which suits labels;
  `google.protobuf.Timestamp` and `Duration` are RFC 3339 and `"1.5s"`
  strings.

A Python transform reads the same JSON from `data`:

```yaml
collectors:
  - name: queue_py
    request:
      type: grpc
      rpc: acme.queue.v1.QueueService/GetStats
      message: '{"queue": "orders", "include_shards": true}'
      descriptors: reflection
    transform:
      type: python
      script: |
        for shard in data["shards"]:
            metric(name="queue_depth", value=int(shard["depth"]), labels={"shard": shard["id"]})
```

## Placeholders

The message may hold [`{{param_<name>}}` placeholders](REQUESTS.md#in-the-body-headers-and-query),
filled from the probe's `param_<name>`, or the default after the colon, with
the body's filters for the JSON they land in: `|json` for a string, which
quotes and escapes it, `|number` for a number, checked to be one, and `|raw`
for a value written as it is. `|form` and `|xml` do not write JSON and are
refused. Metadata values may hold placeholders too, whose values may not hold
a control character, as a header's may not. `rpc` takes none: one collector
calls one method.

When the configuration loads, every placeholder takes its default, or a
stand-in of its filter's kind when it has none, and the message must be JSON.
With `protoset` and `proto`, a message whose placeholders all have defaults
is also checked against the method's input type then; one with a placeholder
without a default is checked when a probe fills it in.

A message that does not fit the input type — an unknown field, a string for a
number — fails the probe at the `message` stage, naming the field, before the
target is called.

## Errors and retries

A call that ends with a status other than `OK` fails the fetch at the `grpc`
stage, under `error_handling.on_fetch_error` like any fetch error:

| Status | Retried | Notes |
| --- | --- | --- |
| `UNAVAILABLE` | yes | The server is down, refused the connection, or the TLS handshake failed. |
| `DEADLINE_EXCEEDED` | no | The probe ran out of its budget; the error says where the budget came from, as it does for `http`. |
| `UNIMPLEMENTED` | once, with fresh descriptors | The server does not know the service or the method. |
| `UNAUTHENTICATED`, `PERMISSION_DENIED` | no | Logged with the server's message, never the credentials. |
| `RESOURCE_EXHAUSTED` | no | Includes an answer over `max_response_bytes`, which counts in `http_exporter_series_limit_exceeded_total`. An answer that arrives within the limit but is over it as JSON fails the same way, counted the same way, with the gauge at `0`, since the call itself was `OK`. |
| anything else | no | The status and the server's message. |

The Retried column is the default, `retry.codes: [UNAVAILABLE]`: the server
did not start the call, so making it again cannot repeat what it did. A
collector whose method is safe to repeat can retry more:

```yaml
retry:
  attempts: 2
  backoff: 500ms
  codes: [UNAVAILABLE, RESOURCE_EXHAUSTED, ABORTED]
```

Codes are named as gRPC names them, in any case; an unknown name, and `OK`,
are refused when the configuration loads. `backoff`, and the bounds a probe's
budget and a static target's interval put on retries, work as for `http`.
`retry.non_idempotent` is `http`'s, and refused here; `retry.codes` is
refused for every other type.

The probe's error answer and the log line carry the status and the server's
message, and the log line has a `grpc_code` attribute. The probe's budget, or
its `timeout`, is the call's deadline, which gRPC sends to the server, so the
server can stop work the exporter no longer waits for.

## Authentication

The credential keys work as for `http`, and are sent as the `authorization`
metadata: `bearer_token` and `bearer_token_file` as `Bearer <token>`,
`basic_auth` and `basic_auth_file` as `Basic <base64>`. A file is read at every
call, so a rotated token, such as a Kubernetes service account token, is used
from the next scrape. Setting `authorization` in `metadata` as well is refused.

`forward_authorization` passes on the probe's own `Authorization` header, and
`forward_headers` the headers it lists, as metadata, lower-cased; a
`header_<name>` probe parameter fills a listed header, as for `http`. A
forwarded header replaces the collector's metadata and credentials of the
same name. A name gRPC reserves cannot be listed.

A token sent to a plaintext target travels in the clear, as it does to an
`http://` one: use TLS, or a network, such as a service mesh, that encrypts
between the pods.

## TLS

With `grpcs://` or any `tls` key, the call uses TLS: `ca_file` to verify the
server, `server_name` when the server's certificate is for another name than
the target's host, `cert_file` and `key_file` for a client certificate, which
together are mutual TLS, and `insecure_skip_verify`, which a probe may
override, as for `http`.

## Probe parameters

`timeout`, `insecure_skip_verify`, `retry_attempts`, `retry_backoff`,
`param_<name>` and `header_<name>` work as for `http`, and `message`, which
replaces `request.message` for that probe as `body` replaces an `http` body,
placeholders and all. `method`, `path`, `body`, `follow_redirects`,
`enable_http2`, `from` and `until` are answered `400`.

## Static targets

A [static target](STATIC-TARGETS.md) of a `grpc` collector is scraped by the
exporter on its interval. Its `target` is the server, and under `request` it
may set `message`, which replaces the collector's, `metadata`, sent besides
the collector's with its own value for a key of both, `timeout`,
`insecure_skip_verify`, `retry`, whose `codes` replaces the collector's, and
the credential keys:

```yaml
interval: 1m
targets:
  - name: orders
    collector: queue_stats
    target: queue.internal:9090
    params:
      param_queue: orders
  - name: billing_eu
    collector: queue_stats
    target: grpcs://queue.eu.internal:9090
    request:
      message: '{"queue": "billing", "include_shards": true}'
      metadata:
        x-region: eu
```

A target's own message and metadata are sent as written, so they cannot hold
placeholders; the collector's are filled from the target's `params`. With
[caching](CONFIGURATION.md#response-caching), each target's own message,
metadata and retry codes are part of its cache key, so targets asking one
server different questions never share a result.

## Self-metrics

`http_exporter_scrape_grpc_status_code{collector}` is the status code of the
collector's most recent call: `0` for `OK`, `14` for `UNAVAILABLE`, and `-1`
before the first call, or when a scrape made none, as when the message did
not fit. Only `grpc` collectors have it, so an exporter without them does not
show the family. `http_exporter_scrape_http_status_code` reads `200` after an
`OK` call and `0` after any other. With verbose self-metrics, a call's `url`
label is `grpc://host:port/package.Service/Method`, `grpcs://` over TLS, and
its `http_method` is `POST`, which is what gRPC sends over HTTP/2. See
[Self-metrics](SELF-METRICS.md).

## gRPC-Web and Connect

A server that also speaks gRPC-Web or Connect with JSON can be read with the
`http` type instead: both are plain HTTP `POST`s of the JSON message to
`/package.Service/Method`. Most gRPC servers speak neither, and this type is
for them.

## Building without it

`grpc` is a request type like the others, carried by every build and the
published image. Its code, and the gRPC and protobuf libraries it needs, are
behind its build tag, so a build with
[`REQUEST_TYPES`](CONFIGURATION.md#choosing-request-types-at-build-time) that
leaves it out links none of them and cannot load a `grpc` collector:

```text
make build REQUEST_TYPES=http,localfile,graphite
docker build --build-arg REQUEST_TYPES=http,localfile,graphite .
```

The libraries add about 6 MB to a stripped binary. See
[Dependencies](DEPENDENCIES.md).
