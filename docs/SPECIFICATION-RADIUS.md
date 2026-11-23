# Prometheus Universal Exporter — `radius` Request Type Specification

This is the `radius` request type's section of § 5.1 of the
[exporter specification](SPECIFICATION-EXPORTER.md), in a document of its
own so that a change to the type is a change to this document. Everything
the request types share — the registry and the rules every type is held to
(§ 5.1), building without a type, decoding, transforms, error policies,
limits, caching, static targets and exposition — is in the exporter
specification, and a section number here, such as § 42.14, is one of its
sections. What a user reads of the type is [RADIUS.md](RADIUS.md).

## `radius`

TODO(radius): what a `radius` collector reads and hands the transforms, its
keys in a table of `Key`, `Default` and `Rule`, what a target is for it, its
fetch, its limits and how each of its failures reads, each as MUST rules, as
the other types' specifications hold theirs.
