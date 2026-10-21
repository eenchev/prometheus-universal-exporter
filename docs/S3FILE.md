# Objects in S3

The `s3file` request type is [`localfile`](LOCALFILE.md) for an object store:
it reads an object, or every matching object under a prefix, from a bucket of
Amazon S3 or of a store that speaks its API — MinIO, Ceph's RADOS Gateway,
Cloudflare R2, Google Cloud Storage — and decodes, transforms and checks it
exactly as `localfile` does a file of the exporter's own. Use it where what
writes the metrics, or a status, leaves them in a bucket:

- the `.prom` files a batch job, a Lambda function or a CI pipeline uploads
  in place of node_exporter's textfile collector directory;
- the JSON report a backup tool or a data pipeline writes after each run;
- an export a SaaS product drops into a bucket you own.

The type is opt-in: the default build and the published image leave it out,
with the AWS SDK it alone needs, so a build carries it only when asked
([Building with s3file](#building-with-s3file)).

## Building with s3file

`OPT_IN_REQUEST_TYPES` names the opt-in types a build adds to the types it
carries, comma-separated:

```sh
make build OPT_IN_REQUEST_TYPES=s3file           # bin/prometheus-universal-exporter
docker build --build-arg OPT_IN_REQUEST_TYPES=s3file -t exporter:s3file .
go build -tags "$(sh tools/request-type-tags.sh '' '' s3file)" .
```

The tag is `request_type_s3file`, which is all `go build -tags
request_type_s3file .` needs by hand: every type of the default build, and
`s3file`. `OPT_IN_REQUEST_TYPES` goes with `REQUEST_TYPES` (see [Choosing
request types at build time](CONFIGURATION.md#choosing-request-types-at-build-time)):
`REQUEST_TYPES=http OPT_IN_REQUEST_TYPES=s3file` builds the two, and
`REQUEST_TYPES` may name `s3file` itself, `REQUEST_TYPES=s3file` building it
alone. A name that is no opt-in type, or a type of every build, fails the
build. The AWS SDK adds about 6.7 MB to the binary.

A configuration with an `s3file` collector is refused by a build without the
type, at startup, on a reload and by `--dry-run`, saying how to build one
that has it:

```text
collector "backups" uses request.type "s3file", which this build of the exporter does not include; it was built with: dns, graphite, grpc, http, localfile, sql, sshfile, tlscert. s3file is an opt-in type, which the default build and the published image leave out: build with OPT_IN_REQUEST_TYPES=s3file (make build, or the Dockerfile's build argument), which adds the tag request_type_s3file
```

## A collector

```yaml
collectors:
  - name: backups
    request:
      type: s3file
      root: /reports/backup
      path: last-run.json
      region: eu-west-1
      max_age: 26h
    transform:
      type: jq
    metrics:
      - name: backup_last_success_timestamp_seconds
        type: gauge
        expression: .finished_at
      - name: backup_bytes
        type: gauge
        expression: .bytes
```

```text
/probe?collector=backups&target=acme-backups
```

The object read is `reports/backup/last-run.json` in the bucket
`acme-backups`.

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `s3file`. |
| `root` | — | **Required.** The key prefix the collector may read under, written as an absolute directory: `/reports/backup` for the keys that start with `reports/backup/`, `/` for every key of the bucket. Nothing outside it can be read. |
| `path` | — | The object, relative to `root`: its key is `root`'s prefix and `path` joined. May use [path parameters](REQUESTS.md#path-parameters). |
| `files` | — | Read every matching object under `root` instead of one: the [name patterns](#reading-a-prefix) to read. Not with `path`. |
| `max_files` | `100` | With `files`: the most objects one scrape reads. |
| `max_total_bytes` | `64MiB` | With `files`: the most one scrape reads across every object. |
| `max_age` | off | Refuse an object last modified longer ago than this. |
| `max_response_bytes` | limit | The most that is read of one object; a larger one fails the scrape. |
| `region` | the SDK's | The region requests are signed for, as the store names it: `eu-west-1`, `auto` for R2. Left out, the AWS SDK's (`AWS_REGION`, the shared config), else `us-east-1`. |
| `endpoint` | AWS | The scheme, host and port of a store other than AWS, such as `https://minio.example.com:9000`; nothing after them. See [Other stores](#other-stores). |
| `path_style` | `false` | Name the bucket in the URL's path, `endpoint/bucket/key`, rather than in its host, `bucket.endpoint/key`. A bucket whose name has a dot needs it over HTTPS, since the store's certificate does not cover the dotted host. |
| `access_key_id_file`, `secret_access_key_file` | — | The files of a static access key, both or neither. Left out, the SDK's default chain finds the credentials. See [Credentials](#credentials). |
| `session_token_file` | — | The file of a temporary access key's session token; only with the two above. |
| `allowed_targets`, `denied_targets` | — | The buckets the collector may and may not read, by name or glob, such as `metrics-*`. See [Which object is read](#which-object-is-read). |

No other request key applies, and setting one — `method`, `headers`, a
`bearer_token`, `retry` — is a startup error. The credential files name
files on the exporter's own filesystem, such as a mounted Secret, and take
no placeholders; written with blanks alone, each is refused (`""` is the key
left out). They are read at every request, not when the configuration
loads: a file that is missing, unreadable or empty fails the scrape, naming
it, and a key rotated on disk is used by the next scrape.
[`--dry-run`](CONFIGURATION.md#dry-run) reads them, though, as a request would,
and warns — without failing the check — of each a scrape would fail on now.

## Which object is read

The probe's `target` is the bucket, `acme-backups` or `s3://acme-backups`.
It is required, and a probe without one is answered `400`; so is a target
that is no bucket name — 3 to 63 lower-case letters, digits, dots and
hyphens, starting and ending with a letter or a digit — or that holds a key,
a query or a user (`s3://acme-backups/reports/x.json`): the object is `root`
and `path`, which a probe cannot move outside `root`.

| Probe | Object read |
| --- | --- |
| `/probe?collector=backups&target=acme-backups` | `reports/backup/last-run.json` |
| `/probe?collector=backups&target=acme-backups&path=previous.json` | `reports/backup/previous.json` |

The `/probe` parameters that apply are `path`, `timeout` and
`param_<name>`, held to `localfile`'s rules: a `path` relative to `root`,
without `..` above it, and a path parameter a single name, without `/`:

```yaml
  - name: tenant_usage
    request:
      type: s3file
      root: /usage
      path: "{{param_tenant}}.json"
      region: eu-west-1
    transform:
      type: jq
    metrics:
      - name: tenant_storage_bytes
        type: gauge
        expression: .storage_bytes
```

```text
/probe?collector=tenant_usage&target=acme-billing&param_tenant=initech
```

The bucket comes from whoever can reach `/probe`, while the store the
request is sent to — AWS's endpoint of the region, or `endpoint` — is the
configuration's. `allowed_targets` and `denied_targets` therefore name
buckets, as for [any type](REQUESTS.md#restricting-targets) they name what
a target is: a bucket matching `denied_targets` is refused with `403` before
anything is sent, and with `allowed_targets` set, one matching none of it.
An address or a network names no bucket, and is refused when the
configuration loads. What a probe can read is still what the credentials
can: give them `s3:GetObject` on the prefixes the collectors read, and
`s3:ListBucket` where they read a prefix, and no more.

## Credentials

Without credential files, the AWS SDK's default chain finds the credentials,
in this order:

1. the environment: `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` and
   `AWS_SESSION_TOKEN`;
2. a web identity token, `AWS_WEB_IDENTITY_TOKEN_FILE` with `AWS_ROLE_ARN`,
   which EKS sets for a pod whose service account names an IAM role (IRSA),
   exchanged with STS for the role's credentials;
3. the shared files, `~/.aws/credentials` and `~/.aws/config`, or those
   `AWS_SHARED_CREDENTIALS_FILE` and `AWS_CONFIG_FILE` name, under
   `AWS_PROFILE`;
4. the container's role, by ECS's or EKS Pod Identity's endpoint;
5. the instance's role, by the EC2 instance metadata service.

The credentials it finds are kept, and refreshed before they expire, by a
client shared by every scrape of collectors with the same `region`,
`endpoint`, `path_style` and credential files.

**On Kubernetes,** the role of the pod's service account is the way to give
an exporter on EKS its credentials, with nothing to rotate: annotate the
service account with the role, `eks.amazonaws.com/role-arn`, through the
chart's `serviceAccount.annotations` (see the [chart
README](../charts/prometheus-universal-exporter/README.md#s3-object-collectors)).
Elsewhere, put a key's two halves in a Secret and hand them in as the
environment variables above, through `env` or `envFrom`, or mount the Secret
and name its files with the credential keys.

**Static credentials** are `access_key_id_file` and
`secret_access_key_file`, with `session_token_file` for a temporary key. Each
file holds the value alone; the whitespace around it, a trailing newline
among it, is left out, as for the other types' credential files. They take
the place of the default chain for the collector, and are read at every
request, so a rotated key is used at once.

## Other stores

A store that speaks the S3 API is reached with `endpoint`, the scheme, host
and port alone, and most need `path_style: true`, since a bucket's name is
not a host name of theirs:

| Store | `endpoint` | `path_style` | `region` | Credentials |
| --- | --- | --- | --- | --- |
| MinIO | `https://minio.example.com:9000` | `true` | `us-east-1`, or the server's `MINIO_SITE_REGION` | An access key of the server's |
| Ceph RADOS Gateway | `https://rgw.example.com` | `true` | the zone group's name, often `default` | An S3 key of a gateway user |
| Cloudflare R2 | `https://<account>.r2.cloudflarestorage.com` | `false` | `auto` | An R2 API token's access key |
| Google Cloud Storage | `https://storage.googleapis.com` | `false` | `auto` | An HMAC key of a service account |

```yaml
  - name: minio_textfiles
    request:
      type: s3file
      root: /textfiles
      files: ["*.prom"]
      endpoint: https://minio.example.com:9000
      path_style: true
      region: us-east-1
      access_key_id_file: /etc/exporter/minio/access_key_id
      secret_access_key_file: /etc/exporter/minio/secret_access_key
    transform:
      type: prometheus
```

A store whose certificate is signed by a private authority is trusted with
the SDK's own `AWS_CA_BUNDLE`, a file of the authority's certificate in PEM,
and a proxy is taken from `HTTPS_PROXY`, `HTTP_PROXY` and `NO_PROXY`, as
for every request the exporter makes.

## Reading a prefix

With `files`, the collector reads every object directly under `root` whose
name — its key after `root`'s prefix — matches one of the patterns, as a
[`localfile` collector reads a directory](LOCALFILE.md#reading-a-directory),
each object decoded and checked on its own and labelled `file`, with
`localfile_mtime_seconds`, `localfile_scrape_error` and
`localfile_files_skipped` beside them, under the same names as `localfile`'s
so one alert covers both:

```yaml
  - name: textfiles
    request:
      type: s3file
      root: /textfiles/prod
      files: ["*.prom"]
      max_age: 1h
      region: eu-west-1
    transform:
      type: prometheus
```

```text
/probe?collector=textfiles&target=acme-metrics
```

A bucket has no directories, only keys, so the prefix is read as one: a key
below a further `/`, such as `textfiles/prod/old/a.prom`, is in a
subdirectory and is not read, and the key of the prefix itself,
`textfiles/prod/`, the marker a console leaves for a folder, is no file. A
prefix nothing is under is an empty directory, whose read answers no object
rather than failing. The limits are `localfile`'s — `max_files`, the response
limit for each object, `max_total_bytes`, objects read four at a time, what
was read when the deadline came answered, and the listing stopped after ten
times `max_files` entries, at least 1000. The objects are listed with
ListObjectsV2, 256 at a time, the next page asked for only when the one
before has been looked at, so a prefix of a million keys costs a scrape what
its first thousands do. A listing describes each object it lists, its size
and when it was last modified, so the objects are not asked again before
they are read. The `path` and `param_<name>` probe parameters are refused
by such a collector, having no object to name.

## What a read asks of the store

One object is read with a HeadObject, which says its size and when it was
last modified, and a GetObject; an object whose size or modification time
the GetObject finds changed since — replaced between the two — is read once
more, and refused, saying it changed while it was read, if it changes again.
A missing object is answered `404` to the HeadObject, and the bucket is then
asked with a HeadBucket, so that a missing bucket is told apart. Every
request is signed with Signature Version 4, and sent once: the SDK's retries
are switched off, as a file read retries nothing, and the next scrape tries
again. Every request is made within the probe's deadline, its `timeout`
parameter or Prometheus's scrape timeout (see [Probe
deadlines](CONFIGURATION.md#probe-deadlines)), and a store that stops
answering is given up at it.

Transforms see an object as `localfile`'s file: no status (`$status` is
`null`, Python's `response.status_code` `None`), and `Content-Type` from the
name's extension, `Content-Length` and `Last-Modified`, with `ETag`, the
object's entity tag, besides.

## Static targets

A [static target](STATIC-TARGETS.md) of an `s3file` collector names the
bucket as its `target`, which it requires, and may set under its `request`
`path` and `timeout`. `root`, the region, the endpoint and the credentials
are the collector's, and `params` fills its placeholders:

```yaml
interval: 5m
targets:
  - name: backups_prod
    collector: backups
    target: acme-backups
  - name: backups_staging
    collector: backups
    target: s3://acme-backups-staging
    request:
      path: staging-run.json
```

## Errors and self-metrics

A failed read is reported in the `s3` stage and follows
`error_handling.on_fetch_error`, as a failed `localfile` read does, naming
the key as a file under `root` and the bucket: `file
/reports/backup/last-run.json does not exist (in s3://acme-backups)`. An
object over the size limit counts in
`http_exporter_series_limit_exceeded_total`, a bucket the target lists
refuse in `http_exporter_targets_refused_total`, answered `403`.

A [debug probe](CONFIGURATION.md#debugging-a-probe) shows the read in its
request section by the object's `s3://` URL, and never a credential:

```text
Requests
  1. READ s3://acme-backups/reports/backup/last-run.json -> read in 38ms
```

With [verbose self-metrics](SELF-METRICS.md#verbose-per-request-self-metrics)
a read is labelled with that `s3://` URL, path parameters kept as their
placeholders, or the prefix's ending in `/`, and `http_method="READ"`.

## Limitations

- **No limit on connections of its own.** The SDK's client keeps its
  connections to the store, shared by the collectors with the same settings;
  how many scrapes of a collector read at once is its
  [`max_concurrent_probes`](CONFIGURATION.md#limiting-concurrent-probes), and
  a prefix's objects are read four at a time.
- **No anonymous access.** A public bucket is read with credentials too, any
  the store takes.
- **Checksums are not checked.** An object is read as the store sends it;
  the transport's TLS is what guards it on the way.
- **One object store per collector.** `endpoint` and `region` are the
  collector's; a probe chooses the bucket alone.

## Troubleshooting

| The scrape says | Do |
| --- | --- |
| `... uses request.type "s3file", which this build of the exporter does not include` | Build with `OPT_IN_REQUEST_TYPES=s3file` (see [Building with s3file](#building-with-s3file)); the published image does not carry it. |
| `file ... does not exist (in s3://...)` | The key: `root`'s prefix and `path` joined, without the leading `/`. A store that answers `403` for a missing object to credentials without `s3:ListBucket` says `access denied` instead. |
| `the bucket does not exist` | The target's bucket, or the `endpoint` and `region` it is in. |
| `access denied (403): the credentials need s3:GetObject ...` | The policy of the credentials, or of the bucket; for a prefix, `s3:ListBucket` too. |
| `... get credentials: failed to refresh cached credentials, no EC2 IMDS role found ...` | The default chain found no credentials: set the environment, the role of the pod's service account, or the credential files. |
| `... PermanentRedirect ...` or `... AuthorizationHeaderMalformed ... the region '...' is wrong` | `region` is not the bucket's. |
| `reading request.access_key_id_file: ...`, `request.access_key_id_file ... is empty` | The file, or the Secret it is mounted from. |
| `... was last modified ... ago, longer than request.max_age ...` | Whatever uploads the object has stopped. |
| `... changed while it was read, 2 times ...` | The object is replaced faster than it is read. |
