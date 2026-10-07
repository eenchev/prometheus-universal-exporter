# Logging

Every line the exporter writes is a JSON object, at the level set by
`--log.level` (`debug`, `info`, `warn` or `error`, in any case; any other
value is refused with exit status 2, rather than logging at `info` without a
word):

```json
{"time":"2026-09-18T21:49:52+03:00","level":"INFO","msg":"starting exporter","address":":8080","collectors":1,"static_targets":0,"config_watch":true,"config_watch_interval":"1m30s"}
{"time":"2026-09-18T21:49:54+03:00","level":"WARN","msg":"metric extraction failed","collector":"exchange_rates","target":"https://api.frankfurter.dev/v1/latest","url":"https://api.frankfurter.dev/v1/latest","metric":"exchange_rate_observation_timestamp_seconds","error_mode":"log","failures":1,"error":"metric \"exchange_rate_observation_timestamp_seconds\" value is missing"}
```

There is no second format. That is worth stating because it is easy to lose:
code deep inside a transform, several calls below anything holding a logger,
can still reach Go's default logger. The exporter installs its JSON logger as
the process default at startup so such lines are JSON too, instead of arriving
as `2026/09/18 21:43:35 ERROR ...` in the middle of a stream your collector is
parsing.

A rule failing under `error_mode: log` is reported with its collector and
target as well as its name, because the same metric name is often declared by
several collectors and the rule name alone would not say which one to go and
look at. It is a warning, since the scrape was still answered, and it is a
[repeated failure](#repeated-failures) like any other: a rule that fails on
every scrape of a target is logged once, then as a repeat, and
`metric extraction recovered` once it produces its series again. Under
`error_mode: fail` the failure is the probe's: one `probe failed` line with
`"stage":"metric"`, the `metric` and the target, as for any other failed
probe. `ignore` writes nothing.

Each rule is logged, remembered and recovered by itself. A collector may have
several rules of one metric name — one for each column or path the metric's
series come from — and each has its own lines: a rule that starts to fail
while another of its name has been failing for days is logged in full, and
one that works again is logged as recovered while the other goes on as a
repeat. So that they can be told apart, the lines of a name that several
rules of the collector export also carry the rule's `expression`, and its
`items` when it has any, after `metric`:

```json
{"level":"WARN","msg":"metric extraction failed","collector":"storage","target":"http://nas:8080","url":"http://nas:8080","metric":"disk_bytes","expression":"//disk/free","error_mode":"log","failures":1,"error":"metric \"disk_bytes\" node 2: value \"n/a\" is not a number; map text to numbers with value_map"}
{"level":"INFO","msg":"metric extraction recovered","collector":"storage","target":"http://nas:8080","url":"http://nas:8080","metric":"disk_bytes","expression":"//disk/free","stage":"metric","failed_for":"2m0s","failures":2}
```

The lines of a name only one rule exports have neither attribute. Whether a
name is shared is decided by the rules the collector has, not by which of
them failed, so a rule's lines read the same on every scrape. Rules alike in
name, expression and items, whose series only their labels tell apart, are
one rule to the log, as they are to the transform, which counts their
failures together: it is logged if either of them has `error_mode: log`,
whichever comes first. (Rules alike in their labels as well are
[the same rule](CONFIGURATION.md#two-rules-that-are-the-same-rule) written
twice, which the configuration is refused for.) A `prometheus` rule without
a name has an empty `metric`. An empty `expression` on a line is the
`prometheus` rule that has none and matches the sample of its own name, where
another rule of that name has an expression.

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
[directory](LOCALFILE.md#reading-a-directory), and rule, for a rule that
carried on), stage and error — it is
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

The same error is the same failure, not the same text. An error says where in
the response it happened and what it measured — `CSV column "used" is empty
in row 3`, `metric "m" value is missing for node 7`, a decoder's line and
column, `value is 612 bytes, longer than limits.max_label_value_length 500`,
a file `last modified 2h0m0s ago` — and in a response that changes from scrape
to scrape those differ every time. They are left out of what is compared: an
empty cell in row 3 and then in row 7, or a label 612 bytes long and then 640,
is one failure that keeps happening, logged once and then as a repeat, and its
recovery counts every scrape of it. Each line still shows the error in full,
with the row and the size it had on that scrape, and so does the answer to the
scraper. Everything else an error says still tells failures apart: another
column, metric or label, another limit, and another value — `value "n/a" is
not a number` and then `value "N/A" is not a number` are two failures. A YAML
document that cannot be parsed is held to the same: the line its error names,
`yaml: line 12: did not find expected key`, and the two lines a key written
twice names, are left out of what is compared, and a problem the error lists
more than once counts once: a list with the same mistake in every item is one
failure however many items it has. An error on a document's first line names
no line at all (`yaml: did not find expected key`); it is the same failure as
the one that names a line, so a mistake that moves to the first line, or from
it, is a repeat like any other. The YAML library lists a key written several times
as a problem for each two of them, which for a key written 1200 times is
719,400; the error names the first ten problems and counts the others, `...
and 719390 more problems`, in the log and in the answer to the scraper.
A decode error that quotes a part of the body quotes no more than its start
and says how long the whole was — `mapping key "aaaa..."... (100000 bytes)` in
a YAML error, `expected float as value, got "aaaa..."... (1000000 bytes)` in
that of an exposition, the first 64 bytes of a value in the other decoders' —
and keeps what the message says after it, so the line ends with what is wrong
and what to change. An error still over 2,000 bytes, long by many values
rather than by one, is cut there and ends `... (6400 bytes)`. Each such
length, a size like any other, is left out of what is compared: the same
mistake with a longer or a shorter value that starts the same is a repeat.
The 2,000 bytes bound the error of every stage, not of the decode alone:
whatever a probe or a static target's scrape fails with — a fetch, a
refused target, a rule, a script, the validation, a file of a directory, a
credential file that cannot be read — the `error` of its log line, the
answer to the scraper after the collector and the stage, the line a debug
report names it in and the text it is remembered by are each no longer,
whatever the target sent, the scraper asked for or the script raised, and so
is the first failure a rule's line shows (`metric extraction failed`) and
the `panic` of a probe that panicked. A trip that ran out of its budget says
so after the error. Where an error says why after the part that can be long,
that part is shown by its start so that the reason is whole: a URL by its
first 512 bytes (`Get "http://db.internal/aaaa..."... (8019 bytes): dial tcp
10.0.0.7:80: connect: connection refused`), the name of a metric or of a
label by its first 200 (`invalid metric name "aaaa..."... (10485561 bytes):
longer than limits.max_metric_name_length 200`), a script's traceback by its
last frames and the exception's own line ([Python](PYTHON.md#how-scripts-run)),
and the metrics of a directory's file that clash with another file's by the
first of them, the rest counted (`...; and 4983 more: a metric has one type
across the directory's files`).
Of a [runtime error of the XPath engine](CONFIGURATION.md#when-the-xpath-engine-fails-on-an-expression)
what follows `runtime error:` is left out as well, since the numbers there
come from the response; so it is of a runtime error while a YAML document is
decoded, which is told from another by the file and the function it was
raised in, not by its line.
Where several labels of one series are over a limit, or are no label names,
the error names the first of them by name, so it is the same error on every
scrape.

A fetch that fails names the connection it failed on, and that is left out
too, so that a target which resets every connection is one failure and not a
new one on every probe: the address and port the connection was made from, as
in `read tcp 10.0.0.1:53412->10.0.0.2:80: read: connection reset by peer`,
which is another port each time; the stream an HTTP/2 stream error or a
`GOAWAY` names; the time an expired certificate was held against, `current
time … is after …`; the offset at which a compressed answer turned out
corrupt; and, in what a gRPC call failed with, that address, that time and the
size of an answer over the limit, `grpc: received message larger than max
(5007 vs. 1000)`, where the gRPC client itself wrote them — after `read tcp`
or `write tcp`, after `x509: certificate has expired or is not yet valid:
current time`, and in those words of the size. What the server answered a call
with is left as it is: `replica 10.0.0.7:5432->10.0.0.9:5432 is lagging` and
the same of another replica are two failures, unless the server's message
holds those very words of the client's. The address the connection went to
stays, and so does the name
server a lookup failed at: a connection refused after one that was reset, or
the same failure at another address, is a new failure.

A probe or scrape that ends after a
[reload](CONFIGURATION.md#reloading-on-demand) removed its collector, or
changed its definition, failed or succeeded for a collector that is gone: its
failure is written at debug level only, marked `"superseded":true`, and
neither it nor its success changes what is remembered of the collector now
under the name. The reload forgets what was remembered of a collector it
removed or changed when it is made, so the first failure of a changed
collector is logged in full, and its first success after the reload is not
logged as a recovery from a failure of the old definition. A static target
that the reload removed from the static target file, or changed there, is
held to the same, its collector as it was: what was remembered of it is
forgotten, and a scrape that had read it before the reload changes nothing
of what is remembered of the target now under the name. A static target's
scrape whose result is not published, because the reload removed or changed
its target or its collector, says so at debug level, marked
`"superseded":true` too.

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

A query is shown as it was sent, with values masked where they stand: every
pair keeps its place and its spelling, a bare key stays a bare key, and a pair
no parser would take — `x=100%` — is still there. Pairs are told apart at `&`
and at `;`, since servers differ on `;`, and masked by the more cautious
reading of the two: the token of `a=1;token=SECRET` is masked; a name is also
everything from an `&` to the first `=` after it, as a server that splits at
`&` alone reads it, so `token;id=SECRET` is shown as `token;id=<redacted>` and
`pass;word=SECRET` as `pass;word=<redacted>`; and once a
value is masked so is what follows it up to the next `&`, which is the rest of
that value to a server that splits at `&` alone — `token=SE;CRET` is shown as
`token=<redacted>;<redacted>`, and `token=SECRET;x=1` as
`token=<redacted>;x=<redacted>`. A name is read as a server reads it, so
`%74oken=SECRET` and `TOKEN=SECRET` are masked as `token=SECRET` is, each
shown under the name it was written with.

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
`max_concurrent_probes`, to rules failing under `error_mode: log`, to output
repaired for invalid UTF-8, and to probes
answered with the last good result under `cache.stale_if_error` (`probe
failed; answered with the last successful result`, with its `result_age`,
then `probe answered with a fresh result again`), and to a metric name that
two writers of one OTLP resource export as different kinds (`OTLP metric name
written as two kinds under one resource; the data points of the kind written
earlier are left out of the export`, with the `metric`, the `kind` exported,
the `left_out_kind` and its `left_out_points`, and the resource's
`service_name`; it is one failing thing per resource and name whichever kind
is written last, and has no line for its end: see
[Two writers of one series or one name](OTLP.md#two-writers-of-one-series-or-one-name)). Up to 10,000
failing things are remembered at a time, and one not reported for an hour is
forgotten: the same failure later is logged as new, and its recovery after
the silence is not logged; past that bound, a new failure is simply logged
every time.

A probe is one failing thing with everything that makes it the probe it is:
its target, and its parameters and forwarded headers. Probes of one target
that differ in `path`, a `param_` or a `header_` — tenants of one service,
say — fail and recover apart, and each line carries the probe's `url`, as
the self-metrics label it, so it says which one failed.

