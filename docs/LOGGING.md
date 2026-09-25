# Logging

Every line the exporter writes is a JSON object, at the level set by
`--log.level` (`debug`, `info`, `warn` or `error`, in any case; any other
value is refused with exit status 2, rather than logging at `info` without a
word):

```json
{"time":"2026-09-18T21:49:52+03:00","level":"INFO","msg":"starting exporter","address":":8080","collectors":1,"static_targets":0,"config_watch":true,"config_watch_interval":"1m30s"}
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

A target that answers with an error status usually says why in the body, so
the line for a `http_status` failure adds `response_body`: the start of the
body, at most 256 bytes, on one line. It is logged only, never put in the
probe's answer. An error page can echo what it was sent, so what reads as a
credential in it is masked as `<redacted>` first: a `Bearer` or `Basic`
credential, a JSON Web Token, and the value of a field or parameter whose name
reads as a credential's, by the rule below for a target's query. The masking
errs towards hiding: an ordinary word after such a name may be masked too.

A request that fails quotes the URL it was sending, as Go reports it:
`Get "https://api.example.com/v1/status?api_key=<redacted>": dial tcp ...`.
Every query value is masked and a password in the URL is shown as
`redacted:redacted`, so a token in `request.query` or in the target's query
reaches neither the log, nor the probe's answer, nor a debug report.

Wherever a line names the target itself, a password in it is shown as
`redacted:redacted`, and the value of a query parameter whose name reads as a
credential as `<redacted>`:
`http://host:9100/metrics?token=<redacted>&tenant=a`. A name reads as a
credential's when it contains, in any case, `auth`, `cookie`, `token`,
`secret`, `password`, `passwd`, `passphrase`, `passcode`, `key`, `session`,
`signature`, `credential` or `jwt`, or when `sig` (an Azure SAS signature),
`pwd`, `pw` or `pass` is a whole word of it — the name itself, or a part
between punctuation or at a change to upper case, as in `db_pwd`, `X-Sig` or
`userPass`, but not `design`, `signal` or `bypass`. Other parameters are shown
as given. A URL's fragment, `#…`, is never shown: it is never sent to the
target, and one copied from a browser can carry a token. The static targets
endpoint's `target` label and the OTLP `target` attribute show a target the
same way, and a header's value is withheld by the same rule for its name.

The same applies to static targets (`static target scrape failed`, then
`static target recovered`; a stage passed over under `error_handling` `log`
is `static target stage failed; continuing`, at warning level, as a probe's
is `probe stage failed; continuing`), to a file of a directory that fails, to a
directory over `max_files` or its listing bound, to probes rejected by
`max_concurrent_probes`, to output repaired for invalid UTF-8, and to probes
answered with the last good result under `cache.stale_if_error` (`probe
failed; answered with the last successful result`, with its `result_age`,
then `probe answered with a fresh result again`). Up to 10,000
failing things are remembered at a time, and one not reported for an hour is
forgotten: the same failure later is logged as new, and its recovery after
the silence is not logged; past that bound, a new failure is simply logged
every time.

A probe is one failing thing with everything that makes it the probe it is:
its target, and its parameters and forwarded headers. Probes of one target
that differ in `path`, a `param_` or a `header_` — tenants of one service,
say — fail and recover apart, and each line carries the probe's `url`, as
the self-metrics label it, so it says which one failed.

