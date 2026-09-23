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

`--dry-run` follows the same rule: its log lines on stderr are JSON, and even a
command line that cannot be parsed is reported as a JSON line rather than the
flag package's plain-text complaint. Its report is a separate JSON document on
stdout, described in
[Dry run](CONFIGURATION.md#dry-run).

## Repeated failures

A target that is down fails every probe, from every Prometheus replica, every
scrape interval. Logging each of them would bury everything else, and the
[self-metrics](SELF-METRICS.md) already count every one exactly. So a failure is
logged in full the first time, and while the same thing keeps failing the same
way — the same collector, target (and file, for a
[directory](LOCALFILE.md#reading-a-directory)), stage and error — it is
logged again only every five minutes, at its own level, with how many times it
happened since the last line and since when:

```json
{"level":"ERROR","msg":"probe failed","collector":"api","target":"http://api:8080","stage":"http_status","error":"received HTTP status 503"}
{"level":"ERROR","msg":"probe failed","collector":"api","target":"http://api:8080","stage":"http_status","error":"received HTTP status 503","repeated":10,"failing_since":"2026-09-23T10:00:00Z"}
{"level":"INFO","msg":"probe recovered","collector":"api","target":"http://api:8080","stage":"http_status","failed_for":"7m30s","failures":15}
```

A different stage or error is a new failure and is logged at once, and the
first success after a failure is logged at info level with how long it failed
and how many times. The repeats in between are still written at debug level,
marked `"repeat":true`, so `--log.level=debug` shows every one.

The same applies to scheduled targets (`scheduled target scrape failed`, then
`scheduled target recovered`), to a file of a directory that fails, to a
directory over `max_files` or its listing bound, to probes rejected by
`max_concurrent_probes`, and to output repaired for invalid UTF-8. Up to 10,000
failing things are remembered at a time, and one not reported for an hour is
forgotten; past that bound, a new failure is simply logged every time.

