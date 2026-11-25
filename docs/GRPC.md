# gRPC

The `grpc` request type calls one unary or server streaming gRPC method on
the target and hands the answer to the transforms as JSON. jq, yq or a
Python script then map it to Prometheus metrics, with the same rules,
limits, caching and error policies as any other collector.

The call is described in the configuration alone: the method, the request
message as JSON, and metadata. The message types come from the server's
reflection service, from a descriptor set file, or from `.proto` files
compiled when the configuration loads. No service needs code generated for
it, and a new service needs no new build of the exporter.

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
| `tls` | none | `ca_file`, `cert_file`, `key_file`, `insecure_skip_verify`, `server_name`, as for `http`. The block written, even empty, makes the call use TLS. See [TLS](#tls). |
| `bearer_token`, `bearer_token_file`, `basic_auth`, `basic_auth_file` | — | Sent as the `authorization` metadata. See [Authentication](#authentication). |
| `forward_authorization`, `forward_headers` | off | The probe's `Authorization` and listed headers, forwarded as metadata. |
| `retry` | none | `attempts`, `backoff`, and `codes`, the status codes retried: `[UNAVAILABLE]` when left out. See [Errors and retries](#errors-and-retries). |
| `allowed_targets`, `denied_targets` | none | Hosts, globs, addresses and networks the calls may and may not reach, checked for the server and each connection; see [Restricting targets](REQUESTS.md#restricting-targets). |
| `max_response_bytes` | the collector's limit | The largest answer accepted: the message as it arrives, and the JSON it becomes, which writing every zero value can make many times larger (see [The answer](#the-answer)). With `reflection`, also the most bytes of files a reflection question takes, never less than 10 MiB. |

`path`, `query`, `headers`, `body`, `method`, `follow_redirects`,
`redirect_trusted_hosts`, `enable_http2`, `new_connection`, `ip_family` and `allowed_schemes` are `http`'s
and refused, as any key of
another request type is. The method is `rpc`, not `method`, because `method`
is already the HTTP verb, and a key that meant two things by type would read
the same and check differently.

`decoder.type` left out is `json`, which is what a call is answered with;
another decoder is refused. The transform is `jq`, `yq` or `python`, which
read JSON.

## Targets

A target is `host:port`, `dns:///host:port`, `grpc://host:port` or
`grpcs://host:port`, always with the port.

- A bare target is plaintext, as a bare `http` target is `http://`.
  `grpcs://`, or a `request.tls` block (even an empty one), makes the call
  use TLS. `grpc://` with a `tls` block is a contradiction and is refused.
- A target with a path, a query or a user in it is refused: the method is
  `rpc`.
- The host is written as an `http` target's is: an address, or a name in
  ASCII, with an internationalised name in its `xn--` form. A target with
  any other character is refused with `403` before a connection is made
  ([Restricting targets](REQUESTS.md#restricting-targets)).
- The host is always a server's name or address, looked up over DNS, never
  one of grpc-go's own target schemes: `unix-abstract:8443` and
  `passthrough:8443` are the hosts `unix-abstract` and `passthrough`, both
  directly and behind a proxy.
- The host `unix` is refused, in any case and any form of target, because
  grpc-go dials `unix:8443` as the Unix socket `8443` on the exporter's
  machine, past the proxy and `allowed_targets`. A Unix socket cannot be a
  target.

A static target's target is checked when the file loads. A probe's target
is checked before anything is called, and answered `400` when it is
malformed.

Calls to one target with one set of TLS settings share one connection.

- **Kept for 5 minutes unused.** The connection is kept between probes and
  closed 5 minutes after the end of the last call, or reflection question,
  on it, whether or not another probe comes. A connection whose server has
  gone keeps trying to reconnect, so one left open would go on dialing a
  target that nobody asks about. It is made again with a new certificate
  when a TLS file changes on disk.
- **At most 1000 connections**, across collectors, because a probe chooses
  its target. A new connection past that closes the one used longest ago
  that no call is using. A connection in use is never closed for the bound:
  when more than 1000 are in use at once, all are kept, and the cache
  shrinks back to the bound as their calls end.
- **20 seconds to connect.** An attempt to connect (the TCP connection, the
  TLS handshake and the server's first HTTP/2 frames) is given up after 20
  seconds, and a probe still waiting for it then fails as `UNAVAILABLE`.
- **Reconnecting.** When the server goes away, the connection tries to
  reconnect at most every 5 seconds, and a probe that finds it failed tries
  at once. A server that comes back is therefore answered at the next probe
  instead of after a growing wait. The probe waits up to a second for the
  connection, and tries again every quarter of a second for as long as
  there is none, so an attempt that had begun before the server was back
  does not make the probe fail with it.

A connection can also die silently: a node that lost power, or a firewall
or NAT that forgot the connection, sends nothing to close it, and it goes on
looking established.

- The probe that finds it so waits out its deadline and fails with
  `DEADLINE_EXCEEDED`. It also drops the connection, so the next probe
  connects anew and is answered. Otherwise every probe would wait out its
  deadline until the operating system gives the connection up, which takes
  minutes.
- Only the probe's own deadline, on a connection that was established, does
  this. A `DEADLINE_EXCEEDED` that the server answered with leaves the
  connection alone, and so does a deadline that ends while the connection
  is still being made.
- Other probes whose calls are on the dropped connection are not cut off;
  it is closed when the last of them has ended.
- A server that is merely slower than the probe's deadline therefore gets a
  new connection at each probe, which is what a timed-out `http` request
  costs too.

The exporter sends no keepalive pings to find dead connections: a gRPC
server that has not been configured to allow them closes the connection of
a client that pings more often than every five minutes.

The connection is made through the proxy
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
so a scrape costs one call instead of two, and probes that find no answer at
the same time share one question to the server.

- The answer is forgotten when the 10 minutes end, whether or not another
  probe comes.
- At most 1000 answers are kept, across collectors, holding at most 128 MiB
  of files in all, as the servers sent them. A new answer past either bound
  forgets the answers used longest ago. An answer larger than 128 MiB on its
  own serves the probes that asked for it and is not kept, so each probe
  asks again.
- A call that fails with `UNIMPLEMENTED`, or whose answer does not decode,
  asks again and calls again, once, which covers a server upgraded to a new
  schema. This is not one of the retries.
- A question that fails with a status is reported, and retried, as a call
  with that status. A server that refuses the connection when asked is
  `UNAVAILABLE`, and is asked again under `retry`, the same attempts
  covering the question and the call.
- A target without reflection fails, saying so and saying to register it on
  the server or to use `protoset` or `proto`.
- The reflection question carries the call's `metadata` and credentials, so
  a server that authenticates every RPC, reflection included, answers it.

The server decides how many files a reflection answer holds and how large
they are. A question therefore takes at most 4096 files, and at most the
collector's response limit of files (or 10 MiB when the limit is smaller),
counted as they arrive, across all its answers together.

- The 10 MiB floor is there because the limit is sized for the answers: a
  collector that holds them to a kilobyte still needs its service's
  descriptors, which can be larger.
- A service's file and its imports are tens of files, a few hundred in the
  largest APIs, so a server that passes either bound is broken or hostile.
  The probe fails, saying which bound was passed, and is counted in
  `http_exporter_series_limit_exceeded_total`. Past the bytes bound, raise
  `max_response_bytes` if the files really are that large. Either way,
  `descriptors: protoset` or `proto` takes the types from files that the
  exporter reads itself, and asks the server nothing.
- Probes that share a question share the limit of the collector whose probe
  asked it.
- A failed question is not kept, so the next probe asks again.
- The question goes on when the probe that asked it gives up, until it is
  answered or its own 30-second timeout ends, so that a slow server's answer
  still serves the probes that follow. The bounds hold it meanwhile.

Unary methods are called, and server streaming ones with
[`stream_messages`](#server-streaming). The following are refused, saying
which kind of method it is: a client or bidirectional streaming method, a
server streaming one without `stream_messages`, and a unary one with it.
They are refused when the configuration loads with `protoset` and `proto`,
and at the first call with `reflection`.

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

An import that the set does not carry is taken from the types built into the
exporter when it has them, as it has the well-known types
(`google/protobuf/*.proto`). Any other missing import fails, naming it. The
file is read again when it changes on disk, so a new set is picked up at the
next call without a reload. With `--config.watch`, the configuration is
checked against the new set as well, [as below](#proto-sources).

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

`proto_import_paths` are the directories that an `import` is resolved in,
as `protoc -I` takes them, and each file of `proto_files` must be under one
of them. Left out, each file's own directory is one, which suits files that
import each other by their bare names. The well-known types are built in,
and imports are resolved only on the local filesystem, not from a registry
such as Buf's. A file that does not compile fails the configuration with
the compiler's message, naming the file and line.

The files are compiled again, at the next call and without a reload, when:

- one of them changes on disk;
- a file that they import changes, through however many files;
- a file appears earlier in the import paths than the one that was
  compiled. It is then the one read, as with `protoc`. A file of the same
  name later in the import paths is never read.

A call sees a file as changed when the watch of the configuration would
(see [Watching the configuration](CONFIGURATION.md#descriptor-files)): its
modification time, size or permissions changed, it appeared or disappeared,
or its path leads to another file, by a symbolic link or by another file
renamed over it, which its device and inode show.

- A `.proto` file or a descriptor set mounted from a Kubernetes ConfigMap is
  therefore compiled again at the first call after Kubernetes swaps its
  `..data` link to a new version, even when the new file has exactly the
  time and size of the old one. So is one that a file of the same time, size
  and permissions was renamed over. The watch reloads for it too.
- A file made unreadable by a `chmod` fails that call with
  `permission denied`, and so does an unreadable file in an earlier import
  path; the next import path's file of that name is not read instead, as it
  is when the file is missing. Run as root, which reads a file whatever its
  permissions, the call compiles the files once more and succeeds.
- A filesystem that gives an unchanged file another inode number (FUSE
  without `use_ino`, once the kernel has dropped the file from its cache, or
  the files mounted again) makes the next call compile the files again and
  the watch reload, once each time the number changes (see
  [Watching the configuration](CONFIGURATION.md#descriptor-files)).

A call finds out only when it fails that a changed file no longer fits: the
method is gone, the request type no longer takes the `message`, or the files
do not compile. With [`--config.watch`](CONFIGURATION.md#descriptor-files):

- The watch follows the descriptor files of the collectors in force: a
  `protoset_file`, the `proto_files` and the files they import.
- The tick after one of them changes reads the configuration again and
  checks it against them, with the static target file when one of its
  targets sets a `request.message` for a collector that reads such files.
- Files that still fit are logged as a reload, and nothing kept about the
  collector is dropped.
- Files that do not fit are a rejected reload, logged with the reason and
  shown by `http_exporter_config_last_reload_successful`, and the calls fail
  until the files are fixed.
- A reload rejected because one of these files did not compile, was missing,
  or lacked what the configuration or a static target's message uses is
  tried again when that file changes or appears. This holds for an imported
  file as well as one that the configuration names.

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
about one service.

- A service that the server's health service does not know is answered
  `NOT_FOUND`. That fails the probe at the `grpc` stage like any other
  status, with `http_exporter_scrape_grpc_status_code` reading `5`. Check
  the name against the server's registrations, or set
  `error_handling.on_fetch_error: log` for a health check that should carry
  on without the series.
- `Watch` streams, and is called with `stream_messages: 1`: it sends the
  status at once, and the call reads it and then ends the stream.
- With `descriptors` set, the health service's types come from that source
  like any other service's.

`List` asks for every service the server's health service knows in one
call, `{"statuses": {"<service>": {"status": "SERVING"}, …}}`, the server as
a whole under `""`, so one collector gives a series per service without a
probe each:

```yaml
collectors:
  - name: grpc_health_all
    request:
      type: grpc
      rpc: grpc.health.v1.Health/List
    transform:
      type: jq
    metrics:
      - name: grpc_service_serving
        description: 1 when the service answers SERVING
        items: '.statuses | to_entries[] | select(.key != "")'
        expression: 'if .value.status == "SERVING" then 1 else 0 end'
        labels:
          - name: service
            expression: .key
```

`List` is newer than `Check` and is served by recent releases of grpc-go and
the other libraries. A server that does not serve it answers
`UNIMPLEMENTED`, which fails the probe: ask such a server with `Check`, one
probe per service. A server with more services than it lists at once (100
for grpc-go) answers `RESOURCE_EXHAUSTED`.

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
- A `google.protobuf.Any` is written as the message it carries, with its
  `@type`: `{"@type": "type.googleapis.com/acme.queue.v1.Detail", "reason":
  "slow consumer"}`. Its type is found among the service's own types, from
  whichever descriptors described the method, or among the well-known types
  built into the exporter; one in neither fails the answer, naming it.

`max_response_bytes` bounds the JSON as well as the message. Writing every
zero value can make the JSON far larger than the message: an empty shard is
two bytes on the wire and `{"id":"","depth":"0"}` as JSON, so 8 MB of empty
shards would be 88 MB of JSON.

- The exporter therefore counts the JSON's size from the message as it
  arrives, before decoding it, and refuses an answer whose count is over the
  limit at that point: `the answer is at least 10485761 bytes as JSON, over
  the response limit of 10485760`. Neither decoding nor rendering a refused
  answer costs more than reading it did.
- The count is exact except for floating-point numbers, which count as one
  digit, and the well-known types with a form of their own, such as
  `Timestamp`, which count at their shortest. An answer that the count lets
  through is therefore rendered, and still refused when its JSON is over the
  limit after all (`the answer is 10485770 bytes as JSON, ...`).
- A message that carries a single-valued field or a map key twice is counted
  with each, although no encoder writes that.

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

## Server streaming

With `stream_messages`, the method is a server streaming one. It is called
with the message, and its stream is read until the server ends it or that
many messages have come, when the call ends the stream itself. The answer
holds each message, rendered as a unary answer is, in `messages`, and
`ended`: `true` when the server ended the stream, `false` when it was cut at
`stream_messages`:

```yaml
collectors:
  - name: queue_watch
    request:
      type: grpc
      rpc: acme.queue.v1.QueueService/Watch
      descriptors: reflection
      message: '{"queue": {{param_queue|json}}}'
      stream_messages: 10
    transform:
      type: jq
    metrics:
      - name: queue_watch_updates
        expression: .messages | length
      - name: queue_watch_last_total
        expression: .messages[-1].total
```

```json
{"messages": [{"shards": [], "total": "1", "state": "STATE_OK", "...": "..."},
              {"shards": [], "total": "2", "state": "STATE_OK", "...": "..."}],
 "ended": true}
```

`stream_messages` is from 1 to 1000.

- A stream that the server ends with a status other than `OK` fails the call
  with that status, whatever messages came before it. The status is retried
  and accepted as a unary call's is.
- A stream that stays open with fewer messages than `stream_messages` is
  read until the probe's deadline, which then fails it with
  `DEADLINE_EXCEEDED`. Set `stream_messages` to what the method sends at
  once, such as `1` for the health service's `Watch`, which sends the status
  and afterwards only its changes.
- The messages are held to the response limit together, as JSON. Past it
  the call fails as an exceeded limit, saying at which message.

A debug probe's trace shows the call's outcome as `grpc OK, 2 messages`.

## Placeholders

The message may hold
[`{{param_<name>}}` placeholders](REQUESTS.md#in-the-body-headers-and-query),
filled from the probe's `param_<name>` or from the default after the colon.
The body's filters apply to the JSON they land in: `|json` for a string,
which quotes and escapes it, `|number` for a number, checked to be one, and
`|raw` for a value written as it is. `|form` and `|xml` do not write JSON
and are refused.

- Metadata values may hold placeholders too. Their values may not hold a
  control character, as a header's may not.
- `rpc` takes none: one collector calls one method.
- The collector's fixed label values, `transform.labels` and a rule's
  static `value` take placeholders as for any request type, written as given
  ([In label values](REQUESTS.md#in-label-values)). The `message` probe
  parameter, which replaces the message, leaves a parameter that a label
  names in use.

When the configuration loads, every placeholder takes its default, or a
stand-in of its filter's kind when it has none, and the message must be JSON.
A default its filter refuses, `{{param_limit:ten|number}}`, or a metadata
default with a control character, is refused then, naming the parameter.
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
| `UNAVAILABLE` | yes | The server is down, refused the connection, or the TLS handshake failed. Each retry connects again. |
| `DEADLINE_EXCEEDED` | no | The probe ran out of its budget; the error says where the budget came from, as it does for `http`. |
| `UNIMPLEMENTED` | once, with fresh descriptors | The server does not know the service or the method. |
| `UNAUTHENTICATED`, `PERMISSION_DENIED` | no | Logged with the server's message, never the credentials. |
| `RESOURCE_EXHAUSTED` | no | Includes an answer over `max_response_bytes`, which counts in `http_exporter_series_limit_exceeded_total`. An answer that arrives within the limit but is over it as JSON fails the same way, counted the same way, with the gauge at `0`, since the call itself was `OK`. |
| `INTERNAL` | no | Includes an answer whose headers, or trailers, are over 1 MiB, the bound an `http` answer's headers have ([Target requests](REQUESTS.md#compression-and-the-response-size)): `the answer's headers or trailers are larger than 1048576 bytes, the most the exporter reads of a response's headers`, counted in `http_exporter_series_limit_exceeded_total` and never retried, whatever `retry.codes` and `accept_codes` say. The exporter tells the server the bound; a server that keeps to it, as grpc-go's does, ends the call itself instead, which reads `stream terminated by RST_STREAM with error code: INTERNAL_ERROR` and is not counted as a limit. |
| anything else | no | The status and the server's message. |

The Retried column is the default, `retry.codes: [UNAVAILABLE]`: the server
did not start the call, so making it again cannot repeat what it did.

A retry is a new attempt at the server, not a repeat of the answer that the
connection already had. After `backoff`, a connection that could not be
made is made again at once, and the retry waits up to a second for it before
calling, so a server that comes back between two attempts is reached by the
next one. While the server stays down, each retry therefore takes up to a
second longer than `backoff` (within the probe's budget, as always) and
tries to connect up to four times in that second.

A collector whose method is safe to repeat can retry more:

```yaml
retry:
  attempts: 2
  backoff: 500ms
  codes: [UNAVAILABLE, RESOURCE_EXHAUSTED, ABORTED]
```

Codes are named as gRPC names them, in any case; an unknown name, and `OK`,
are refused when the configuration loads, and by the
[schema](CONFIGURATION.md#editor-support) in an editor. `backoff`, and the bounds a probe's
budget and a static target's interval put on retries, work as for `http`.
`retry.non_idempotent`, `retry.statuses`, the HTTP statuses retried, and
`retry.retry_after`, which reads an HTTP answer's `Retry-After`, are
`http`'s, and refused here; `retry.codes` is refused for every other type.

A status that says something rather than that the call failed — `NOT_FOUND`
for a queue not created yet — can be made an answer with `accept_codes`:

```yaml
request:
  type: grpc
  rpc: acme.queue.v1.QueueService/GetStats
  accept_codes: [NOT_FOUND]
metrics:
  - name: queue_exists
    expression: 'if $status == 0 then 1 else 0 end'
  - name: queue_depth
    expression: .depth
    required: false
```

Such a call is not retried, and its rules see an empty object, the status
code as `$status` — `5` here, `0` for `OK` — and the status message as
`$headers["grpc-message"]`; `http_exporter_scrape_grpc_status_code` reads the
code. Codes are named as for `retry.codes`, and `OK` is refused. A status not
listed still fails the call.

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
same name. A name gRPC reserves cannot be listed. A call that carries an
`authorization` of its own, forwarded or a static target's, does not read
the collector's credential files at all, so a missing one does not fail it.

A token sent to a plaintext target travels in the clear, as it does to an
`http://` one: use TLS, or a network, such as a service mesh, that encrypts
between the pods.

## TLS

With `grpcs://` or a `tls` block, the call uses TLS. A `tls` block counts
even when empty: `tls: {}` asks for TLS verified against the system's roots,
and the label is `grpcs://`. The keys say how:

- `ca_file` to verify the server;
- `server_name` when the server's certificate is for another name than the
  target's host;
- `cert_file` and `key_file` for a client certificate, which together make
  mutual TLS;
- `insecure_skip_verify`, which a probe may override, as for `http`.

The files are read when a connection is made, again when one of them
changes, and by [`--dry-run`](CONFIGURATION.md#dry-run), which warns about
one that a connection would fail on. A [static target](#static-targets) may
write a block of its own in place of the collector's.

## Probe parameters

`timeout`, `insecure_skip_verify`, `retry_attempts`, `retry_backoff`,
`param_<name>` and `header_<name>` work as for `http`, and `message`, which
replaces `request.message` for that probe as `body` replaces an `http` body,
placeholders and all. `method`, `path`, `body`, `follow_redirects`,
`enable_http2`, `new_connection`, `ip_family`, `from` and `until` are answered `400`.

## Static targets

A [static target](STATIC-TARGETS.md) of a `grpc` collector is scraped by the
exporter on its interval. Its `target` is the server. Under `request` it may
set:

- `message`, which replaces the collector's;
- `metadata`, sent beside the collector's, with its own value for a key that
  both set;
- `timeout` and `insecure_skip_verify`;
- `retry`, whose `codes` replaces the collector's, and
  [`accept_codes`](#errors-and-retries), which replaces the collector's;
- the credential keys (`basic_auth`, `basic_auth_file`, `bearer_token`,
  `bearer_token_file`);
- its own `tls` block.

For example:

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

A target's `tls` replaces the collector's whole block for that target: a key
that it leaves out is not taken from the collector's. A target can therefore
present a client certificate of its own, or trust its own CA.

- Written, even as `tls: {}`, it asks for TLS. A `grpc://` target asks for
  plaintext, so the two together are refused when the file loads. A target
  cannot turn off the TLS that its collector asks for.
- Its values hold no placeholder, and no probe parameter sets it.
- The block is part of the target's cache key, and a connection is kept by
  its TLS settings, so targets with different blocks share neither an answer
  nor a connection.
- [`--dry-run`](CONFIGURATION.md#dry-run) reads the files of a target's own
  block under the target's name.

## Self-metrics

`http_exporter_scrape_grpc_status_code{collector}` is the status code of the
collector's most recent call: `0` for `OK`, `14` for `UNAVAILABLE`, and `-1`
before the first call or when a scrape made none, as when the message did not
fit. Only `grpc` collectors have it, so an exporter without them does not
show the family.

`http_exporter_scrape_http_status_code` reads `200` after an `OK` call or one
ending in a code of [`accept_codes`](#errors-and-retries), and `0` after any
other. With verbose self-metrics, a call's `url` label is
`grpc://host:port/package.Service/Method` (`grpcs://` over TLS), and its
`http_method` is `POST`, which is what gRPC sends over HTTP/2. See
[Self-metrics](SELF-METRICS.md).

## gRPC-Web and Connect

A server that also speaks gRPC-Web or Connect with JSON can be read with the
`http` type instead: both are plain HTTP `POST`s of the JSON message to
`/package.Service/Method`. Most gRPC servers speak neither, and this type is
for them.

## Building without it

`grpc` is a request type like the others, carried by every build and the
published image. Its code, and the gRPC and protobuf libraries it needs, are
behind its build tag. A build with
[`REQUEST_TYPES`](CONFIGURATION.md#choosing-request-types-at-build-time) that
leaves it out links none of them. It cannot load a `grpc` collector, nor a
collector with the [`protobuf` decoder](CONFIGURATION.md#protobuf-responses),
which reads an HTTP answer that is one protobuf message, with the same
descriptor files:

```text
make build REQUEST_TYPES=http,localfile,graphite
docker build --build-arg REQUEST_TYPES=http,localfile,graphite .
```

`make build` writes the binary to `bin/prometheus-universal-exporter`.

The libraries add about 6 MB to a stripped binary. See
[Dependencies](DEPENDENCIES.md).
