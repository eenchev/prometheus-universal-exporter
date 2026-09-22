# Logging

Every line the exporter writes is a JSON object, at the level set by
`--log.level` (`debug`, `info`, `warn` or `error`):

```json
{"time":"2026-09-18T21:49:52+03:00","level":"INFO","msg":"starting exporter","address":":8080","collectors":1,"scheduled_targets":0,"config_watch":true,"config_watch_interval":"1m30s"}
{"time":"2026-09-18T21:49:54+03:00","level":"ERROR","msg":"metric extraction failed","collector":"exchange_rates","metric":"exchange_rate_observation_timestamp_seconds","error_mode":"log","error":"metric \"exchange_rate_observation_timestamp_seconds\" value is missing"}
```

There is no second format. That is worth stating because it is easy to lose: a
metric rule failing under `error_mode: log` reports from inside a transform,
several calls below anything holding a logger, so it goes through Go's default
logger rather than the exporter's. The exporter installs its JSON logger as the
process default at startup so those lines are JSON too, instead of arriving as
`2026/09/18 21:43:35 ERROR metric extraction failed metric=...` in the middle of
a stream your collector is parsing.

A failing rule is reported with its collector as well as its name, because the
same metric name is often declared by several collectors and the rule name alone
would not say which one to go and look at. The line is the same under
`error_mode: log` and `error_mode: fail`, with `error_mode` saying which applied;
under `fail` it is followed by a `probe failed` line with `"stage":"metric"` and
the target, as for any other failed probe. `ignore` writes nothing.

`config_watch_interval` appears only when `--config.watch` is on, since that is
what bounds how stale a running configuration can be; with the watch off there
is no interval to report.
