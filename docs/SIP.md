# The sip request type

TODO(sip): the page's title, what the sip request type reads and what
for, in a paragraph or two, as the other request types' pages open.

The type is opt-in: the default build and the published image leave it out,
so a build carries it only when asked
([Building with sip](#building-with-sip)).

## Building with sip

`OPT_IN_REQUEST_TYPES` names the opt-in types a build adds to the types it
carries, comma-separated:

```sh
make build OPT_IN_REQUEST_TYPES=sip              # bin/prometheus-universal-exporter
docker build --build-arg OPT_IN_REQUEST_TYPES=sip -t exporter:sip .
go build -tags "$(sh tools/request-type-tags.sh '' '' sip)" .
```

The tag is `request_type_sip`. A configuration with an `sip` collector
is refused by a build without the type, at startup, on a reload and by
`--dry-run`, saying how to build one that has it.

## A collector

TODO(sip): a collector that works, as a `yaml` block, and what each key
does.

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `sip`. |
| TODO(sip) | | |

## The target

TODO(sip): what a probe's target is, how it is checked, and how it is
shown in logs and labels.

## Static targets

A [static target](STATIC-TARGETS.md) of an `sip` collector may set
`timeout` under its `request`. TODO(sip): what a static target of the
type is, and every other key its `request` may set, each named in this
paragraph, as a test of the pages reads the type's TargetFields here.

## Limits

TODO(sip): what bounds a scrape: its deadline, the size of the answer,
and anything of the type's own.

## Errors and self-metrics

TODO(sip): what fails a scrape, how each failure reads, and the
self-metrics of the type's collectors. Until the type is written, every
scrape fails: `the sip request type is not implemented yet`.

## Limitations

TODO(sip): what the type does not do, and why.

## Troubleshooting

TODO(sip): the failures a reader is likely to meet, and what to do about
each.
