# The radius request type

TODO(radius): the page's title, what the radius request type reads and what
for, in a paragraph or two, as the other request types' pages open.

The type is opt-in: the default build and the published image leave it out,
so a build carries it only when asked
([Building with radius](#building-with-radius)).

## Building with radius

`OPT_IN_REQUEST_TYPES` names the opt-in types a build adds to the types it
carries, comma-separated:

```sh
make build OPT_IN_REQUEST_TYPES=radius              # bin/prometheus-universal-exporter
docker build --build-arg OPT_IN_REQUEST_TYPES=radius -t exporter:radius .
go build -tags "$(sh tools/request-type-tags.sh '' '' radius)" .
```

The tag is `request_type_radius`. A configuration with a `radius` collector
is refused by a build without the type, at startup, on a reload and by
`--dry-run`, saying how to build one that has it.

## A collector

TODO(radius): a collector that works, as a `yaml` block, and what each key
does.

| Key | Default | Notes |
| --- | --- | --- |
| `type` | — | **Required.** `radius`. |
| TODO(radius) | | |

## The target

TODO(radius): what a probe's target is, how it is checked, and how it is
shown in logs and labels.

## Static targets

A [static target](STATIC-TARGETS.md) of a `radius` collector may set
`timeout` under its `request`. TODO(radius): what a static target of the
type is, and every other key its `request` may set, each named in this
paragraph, as a test of the pages reads the type's TargetFields here.

## Limits

TODO(radius): what bounds a scrape: its deadline, the size of the answer,
and anything of the type's own.

## Errors and self-metrics

TODO(radius): what fails a scrape, how each failure reads, and the
self-metrics of the type's collectors. Until the type is written, every
scrape fails: `the radius request type is not implemented yet`.

## Limitations

TODO(radius): what the type does not do, and why.

## Troubleshooting

TODO(radius): the failures a reader is likely to meet, and what to do about
each.
