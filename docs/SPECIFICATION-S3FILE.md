# Prometheus Universal Exporter — `s3file` Request Type Specification

This is the `s3file` request type's section of § 5.1 of the
[exporter specification](SPECIFICATION-EXPORTER.md), in a document of its
own so that a change to the type is a change to this document. Everything
the request types share — the registry and the rules every type is held to
(§ 5.1), building without a type, decoding, transforms, error policies,
limits, caching, static targets and exposition — is in the exporter
specification, and a section number here, such as § 42.14, is one of its
sections. What a user reads of the type is [S3FILE.md](S3FILE.md).

## `s3file`

An `s3file` collector reads an object, or every matching object under a
prefix, from a bucket of Amazon S3 or of a store that speaks the S3 API, the
probe's `target` naming the bucket, and MUST present what it read exactly as
`localfile` presents a file: the same path rules, the same reading of one
file and of a directory (§ 5.1a), the same response. Its keys:

| Key | Default | Rule |
| --- | --- | --- |
| `root` | none | **Required.** As `localfile`'s, an absolute directory, read as the key prefix of the objects under it: `/a/b` is the keys that start with `a/b/`. `/`, every key of the bucket, MUST be taken. |
| `path`, `files`, `max_files`, `max_total_bytes`, `max_age`, `max_response_bytes` | as `localfile` | With `localfile`'s rules; an object's key is `root`'s prefix and the file's path under it joined. |
| `region` | the AWS SDK's, else `us-east-1` | Letters, digits, `-` and `_`. |
| `endpoint` | AWS's | An `http://` or `https://` URL of a host, with a port or not, and no user, path, query or fragment. |
| `path_style` | `false` | The bucket in the URL's path rather than its host. |
| `access_key_id_file`, `secret_access_key_file` | none | Both or neither. |
| `session_token_file` | none | Only with the two above. |
| `checksums` | `when_present` | `when_present`, `required` or `off`; any other MUST be refused at load. Unless `off`, the GetObject MUST ask for the object's checksum and a checksum of the whole object the store sends MUST be checked as the body is read, a mismatch failing the read; with `required`, an object sent without one, or with a composite one, MUST fail before its body is read; with `off` none MUST be checked. The SDK's own environment and shared config MUST NOT change it, and the SDK MUST NOT log by itself of an object sent without one. |
| `allowed_targets`, `denied_targets` | none | Bucket names and globs of them; an address or a network MUST be refused at load. |

- The credential files MUST be read at every request, not at load, as every
  credential file is, their surrounding whitespace taken off; a file that is
  missing, unreadable or empty MUST fail the scrape naming the key and the
  file, never its content. A placeholder in one, or one written with blanks
  alone, MUST be refused at load: `""` is the key left out. `--dry-run`
  MUST read them as a request would and list what a scrape would fail on as
  warnings of the `config` check, without failing it; startup and reloads
  MUST NOT read them. Without them, the credentials MUST be the AWS SDK's
  default chain: the environment, a web identity token, the shared files,
  the container's and the instance's role. A client of the S3 API, and the
  credentials its chain found, MUST be shared by the scrapes of collectors
  with the same region, endpoint, path style and credential files, and
  dropped when no collector in force has those settings.
- The target MUST be a bucket name, written as it is or after `s3://`, with
  a `/` after it or not: 3 to 63 lower-case letters, digits, dots and
  hyphens, starting and ending with a letter or a digit, without `..`. It is
  required: a probe without one MUST be answered `400`, a static target
  without one refused at load. A target with a key, a query, a user or
  another scheme, or that is no bucket name, MUST be answered `400` before
  anything is sent. A bucket `denied_targets` matches, or, with
  `allowed_targets` set, that it does not, MUST be refused with `403`
  before anything is sent and counted in
  `http_exporter_targets_refused_total`.
- One object MUST be read with a HeadObject and then a GetObject, and read
  again, once, when the GetObject finds its size or modification time other
  than the HeadObject said, as `localfile` reads a file that changes under
  the read. An object missing to the HeadObject MUST be followed by a
  HeadBucket, so that a missing bucket fails the scrape saying the bucket
  does not exist; an answer of `403` MUST fail it saying access was denied
  and which permissions the credentials need. Every request MUST be sent
  once, without the SDK's retries, within the probe's deadline. A failed
  read MUST be reported in the `s3` stage, naming the key as the file under
  `root` and the bucket as `s3://<bucket>`. What the store answered MUST be
  the operator's alone (§ 19.4): a probe's caller MUST be told, in place of
  the SDK's error, `operation error S3: <operation>, the store answered
  <status> <code>; its message is in the exporter's log`, the store's
  message, which names the principal and the account, withheld; a request
  the SDK found no credentials for MUST be told `..., the AWS SDK found no
  credentials it could use; which it tried is in the exporter's log`. A
  failure of the network MUST be told as it is.
- A collector with `files` MUST list the objects directly under `root`'s
  prefix with ListObjectsV2 and the delimiter `/`, in pages of at most 256
  asked for one at a time as `localfile` lists a directory in batches, and
  read the objects whose name, the key after the prefix, matches, as
  `localfile` reads a directory's files: a key below a further `/` and the
  key of the prefix itself MUST NOT be read. A listing's description of an
  object MUST stand in for its HeadObject until a GetObject finds the object
  changed since. A prefix no key has MUST be read as an empty directory. A
  store that answers 16 pages in a row with no entry while saying more
  follow MUST fail the read saying so.
- An object's response MUST be `localfile`'s, with `ETag`, the object's
  entity tag, among its headers. A debug probe's request section MUST show
  `READ` and the object's `s3://bucket/key` URL, the key escaped as a URL's
  path is, as `localfile`'s file is, or the prefix's ending in
  `/`, and never a credential; the verbose `url` label MUST be that URL,
  path parameters as their placeholders, and `http_method` MUST be `READ`.
- A static target MAY set `path` and `timeout`, the path held to the rules
  of a file's.
- Only the type's files, behind its tag alone, MAY import the AWS SDK.
