# The sshexec request type

TODO(sshexec): the page's title, what the sshexec request type reads and what
for, in a paragraph or two, as the other request types' pages open.

The type is opt-in: the default build and the published image leave it out,
so a build carries it only when asked
([Building with sshexec](#building-with-sshexec)).

## Building with sshexec

`OPT_IN_REQUEST_TYPES` names the opt-in types a build adds to the types it
carries, comma-separated:

```sh
make build OPT_IN_REQUEST_TYPES=sshexec              # bin/prometheus-universal-exporter
docker build --build-arg OPT_IN_REQUEST_TYPES=sshexec -t exporter:sshexec .
go build -tags "$(sh tools/request-type-tags.sh '' '' sshexec)" .
```

The tag is `request_type_sshexec`. A configuration with a `sshexec` collector
is refused by a build without the type, at startup, on a reload and by
`--dry-run`, saying how to build one that has it.

## A collector

TODO(sshexec): a collector that works, as a `yaml` block, and what each key
does.

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `sshexec`. |
| TODO(sshexec) | | |

## The target

TODO(sshexec): what a probe's target is, how it is checked, and how it is
shown in logs and labels.

## Static targets

TODO(sshexec): what a static target of the type is, and the keys its
`request` may set.

## Limits

TODO(sshexec): what bounds a scrape: its deadline, the size of the answer,
and anything of the type's own.

## Errors and self-metrics

Until the type is written, every scrape of a `sshexec` collector fails: `the
sshexec request type is not implemented yet`. TODO(sshexec): what fails a scrape, how each failure reads,
and the self-metrics of the type's collectors.

## Limitations

TODO(sshexec): what the type does not do, and why.

## Troubleshooting

TODO(sshexec): the failures a reader is likely to meet, and what to do about
each.
