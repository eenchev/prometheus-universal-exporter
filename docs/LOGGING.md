# Logging

Every line that the exporter writes is a JSON object, at the level set by
`--log.level`: `debug`, `info`, `warn` or `error`, in any case. Any other
value is refused with exit status 2, instead of silently logging at `info`:

```json
{"time":"2026-09-18T21:49:52+03:00","level":"INFO","msg":"starting exporter","address":":8080","collectors":1,"static_targets":0,"config_watch":true,"config_watch_interval":"1m30s"}
{"time":"2026-09-18T21:49:54+03:00","level":"WARN","msg":"metric extraction failed","collector":"exchange_rates","target":"https://api.frankfurter.dev/v1/latest","url":"https://api.frankfurter.dev/v1/latest","metric":"exchange_rate_observation_timestamp_seconds","error_mode":"log","failures":1,"error":"metric \"exchange_rate_observation_timestamp_seconds\" value is missing"}
```

With [`otlp.logs`](OTLP.md#the-exporters-own-log) the same lines are also
sent over OTLP, as log records, from `otlp.logs.level` up; stderr is written
as it is without it, and the export's own lines are written to stderr alone.

There is no second format. That is worth stating because it is easy to lose:
code deep inside a transform, several calls below anything holding a logger,
can still reach Go's default logger. The exporter installs its JSON logger as
the process default at startup so such lines are JSON too, instead of arriving
as `2026/09/18 21:43:35 ERROR ...` in the middle of a stream your collector is
parsing.

A rule failing under `error_mode: log` is reported with its collector and
target as well as its name. The same metric name is often declared by
several collectors, and the rule name alone would not say which one to look
at.

- It is a warning, because the scrape was still answered.
- It is a [repeated failure](#repeated-failures) like any other: a rule that
  fails on every scrape of a target is logged once, then as a repeat, and
  as `metric extraction recovered` once it produces its series again.
- Under `error_mode: fail`, the failure belongs to the probe: one
  `probe failed` line with `"stage":"metric"`, the `metric` and the target,
  as for any other failed probe.
- `ignore` writes nothing.

Each rule is logged, remembered and recovered separately. A collector may
have several rules of one metric name, one for each column or path that the
metric's series come from, and each has its own lines. A rule that starts to
fail while another of its name has been failing for days is logged in full,
and one that works again is logged as recovered while the other goes on as a
repeat. So that they can be told apart, the lines of a name that several
rules of the collector export also carry the rule's `expression`, and its
`items` when it has any, after `metric`:

```json
{"level":"WARN","msg":"metric extraction failed","collector":"storage","target":"http://nas:8080","url":"http://nas:8080","metric":"disk_bytes","expression":"//disk/free","error_mode":"log","failures":1,"error":"metric \"disk_bytes\" node 2: value \"n/a\" is not a number; map text to numbers with value_map"}
{"level":"INFO","msg":"metric extraction recovered","collector":"storage","target":"http://nas:8080","url":"http://nas:8080","metric":"disk_bytes","expression":"//disk/free","stage":"metric","failed_for":"2m0s","failures":2}
```

The lines of a name that only one rule exports have neither attribute.

- Whether a name is shared is decided by the rules that the collector has,
  not by which of them failed, so a rule's lines read the same on every
  scrape.
- Rules alike in name, expression and items, whose series only their labels
  tell apart, are one rule to the log, as they are to the transform, which
  counts their failures together. The failure is logged if either of them
  has `error_mode: log`, whichever comes first. (Rules alike in their labels
  as well are [the same rule](CONFIGURATION.md#two-rules-that-are-the-same-rule)
  written twice, and the configuration is refused for that.)
- A `prometheus` rule without a name has an empty `metric`. An empty
  `expression` on a line is the `prometheus` rule that has none and matches
  the sample of its own name, where another rule of that name has an
  expression.

`config_watch_interval` appears only when `--config.watch` is on, since that is
what bounds how stale a running configuration can be; with the watch off there
is no interval to report.

A reload that puts a file in force logs it at `info`, as
`configuration reloaded` or `static targets reloaded`, with what triggered
it. Under `changes` it lists the collectors or the static targets that it
added, removed and changed, or says `no changes`. Each list names ten and
counts the rest, so the line stays short however much the reload replaced;
see [What a reload changed](CONFIGURATION.md#what-a-reload-changed).

`--dry-run` follows the same rule: its log lines on stderr are JSON, and even a
command line that cannot be parsed is reported as a JSON line rather than the
flag package's plain-text complaint. Its report is a separate JSON document on
stdout, described in
[Dry run](CONFIGURATION.md#dry-run).

## Repeated failures

A target that is down fails every probe, from every Prometheus replica, at
every scrape interval. Logging each failure would bury everything else, and
the [self-metrics](SELF-METRICS.md) already count every one exactly. A
failure is therefore logged in full the first time. While the same thing
keeps failing the same way (the same collector, target, stage and error,
plus the file for a [directory](LOCALFILE.md#reading-a-directory) and the
rule for a rule that carried on), it is logged again only every five
minutes, at its own level, with how many times it happened since the last
line and since when:

```json
{"level":"ERROR","msg":"probe failed","collector":"api","target":"http://api:8080","stage":"http_status","error":"received HTTP status 503"}
{"level":"ERROR","msg":"probe failed","collector":"api","target":"http://api:8080","stage":"http_status","error":"received HTTP status 503","repeated":10,"failing_since":"2026-09-23T10:00:00Z"}
{"level":"INFO","msg":"probe recovered","collector":"api","target":"http://api:8080","stage":"http_status","failed_for":"7m30s","failures":15}
```

A different stage or error is a new failure and is logged at once, and the
first success after a failure is logged at info level with how long it failed
and how many times. The repeats in between are still written at debug level,
marked `"repeat":true`, so `--log.level=debug` shows every one.

A `web.basic_auth` file of the exporter's own that can no longer be read is
one such failure. Every request is refused with `500`, but it is logged once
as an error, `the exporter's Basic Auth credential cannot be read; refusing
every request`, and the first request that reads the file again logs `the
exporter's Basic Auth credential can be read again`
([Authentication](AUTHENTICATION.md)).

The same error is the same failure, not the same text.

**What is left out of the comparison.** An error says where in the response
it happened and what it measured: `CSV column "used" is empty in row 3`,
`metric "m" value is missing for node 7`, a decoder's line and column,
`value is 612 bytes, longer than limits.max_label_value_length 500`, a file
`last modified 2h0m0s ago`. In a response that changes from scrape to
scrape, those differ every time, so they are left out of what is compared.

- An empty cell in row 3 and then in row 7, or a label 612 bytes long and
  then 640, is one failure that keeps happening, logged once and then as a
  repeat, and its recovery counts every scrape of it.
- Each line still shows the error in full, with the row and the size that it
  had on that scrape, and so does the answer to the scraper.
- Everything else that an error says still tells failures apart: another
  column, metric or label, another limit, and another value.
  `value "n/a" is not a number` and then `value "N/A" is not a number` are
  two failures.

**YAML errors.** A YAML document that cannot be parsed is treated the same
way.

- The line that its error names, `yaml: line 12: did not find expected key`,
  and the two lines that a key written twice names, are left out of what is
  compared.
- A problem that the error lists more than once counts once: a list with the
  same mistake in every item is one failure, however many items it has.
- An error on a document's first line names no line at all
  (`yaml: did not find expected key`). It is the same failure as the one
  that names a line, so a mistake that moves to or from the first line is a
  repeat like any other.
- The YAML library lists a key written several times as a problem for each
  pair of them, which for a key written 1200 times is 719,400 problems. The
  error names the first ten problems and counts the others,
  `... and 719390 more problems`, in the log and in the answer to the
  scraper.

**Long errors.** A decode error that quotes a part of the body quotes no more
than its start, and says how long the whole was:
`mapping key "aaaa..."... (100000 bytes)` in a YAML error,
`expected float as value, got "aaaa..."... (1000000 bytes)` in that of an
exposition, and the first 64 bytes of a value in the other decoders. It
keeps what the message says after the quote, so the line ends with what is
wrong and what to change.

- An error still over 2,000 bytes, long because of many values rather than
  one, is cut there and ends `... (6400 bytes)`.
- Each such length, a size like any other, is left out of what is compared:
  the same mistake with a longer or a shorter value that starts the same is
  a repeat.
- The 2,000 bytes bound the error of every stage, not of the decode alone.
  Whatever a probe or a static target's scrape fails with (a fetch, a
  refused target, a rule, a script, the validation, a file of a directory, a
  credential file that cannot be read), each of these is no longer, whatever
  the target sent, the scraper asked for or the script raised: the `error`
  of its log line, the answer to the scraper after the collector and the
  stage, the line that a debug report names it in, and the text that it is
  remembered by. So is the first failure that a rule's line shows
  (`metric extraction failed`) and the `panic` of a probe that panicked.
- A panic raised where a fetch does part of its work at the same time, as
  when it pings several addresses or reads a directory's files, is logged
  twice: first as `a goroutine of a scrape panicked`, with the stack that it
  was raised on, then as the probe's or the static target's failure.
- A trip that ran out of its budget says so after the error.

Where an error says why after the part that can be long, that part is shown
by its start, so that the reason is whole:

- a URL by its first 512 bytes (`Get "http://db.internal/aaaa..."...
  (8019 bytes): dial tcp 10.0.0.7:80: connect: connection refused`);
- the name of a metric or of a label by its first 200 bytes
  (`invalid metric name "aaaa..."... (10485561 bytes): longer than
  limits.max_metric_name_length 200`);
- a script's traceback by its last frames and the exception's own line
  ([Python](PYTHON.md#how-scripts-run));
- the metrics of a directory's file that clash with another file's by the
  first of them, with the rest counted (`...; and 4983 more: a metric has
  one type across the directory's files`).

**Long metric names in attributes.** A metric's name is shown by the same
rule in a line's attributes too, without the quotes that an error puts
around it: a name of 200 bytes or fewer as it is, and a longer one by its
first 200 bytes, cut between two characters, and its length:
`"first_metric":"queue_depth_aaaa... (1048576 bytes)"`. This applies to:

- the `first_metric` of the warning about output repaired for invalid UTF-8
  (`label values or help text were not valid UTF-8; ...`, with the
  `collector`, the `target`, which for a file of a directory is the file's
  name, and the count of `values` repaired). It is logged before the names
  are validated, when a name is as long as the target or a script made it;
- the `metric` and `clashes_with` of `static target metric left out of the
  static targets endpoint`, and the `metric` of `static target metric back
  on the static targets endpoint`;
- the `metric` of `OTLP metric name written as two kinds under one
  resource`, and the `metric`, `prometheus_name` and
  `left_out_prometheus_name` of `OTLP metric name written for two Prometheus
  metric names under one resource`, whose names can be as long as a raised
  `limits.max_metric_name_length` allows.

The cut changes nothing about what counts as one failure. The warning about
repaired UTF-8 is one failing thing per collector, target and file, whatever
its first metric, and the others are remembered under the whole name, so two
long names that start alike are logged separately.

**Other attributes from outside the exporter** are bounded where they come
in:

- a probe's `target` by the 8 KiB that a probe parameter may be, and its
  `url` by that and the 8 KiB of a `path` parameter, each three times as
  long where every byte is percent-encoded;
- `response_body` by its 256 bytes;
- a directory's `file` by what the filesystem allows a name to be;
- what a Python script printed, at debug level, by its first 4,096
  characters.

**Other parts left out of the comparison.**

- Of a [runtime error of the XPath engine](CONFIGURATION.md#when-the-xpath-engine-fails-on-an-expression),
  what follows `runtime error:` is left out as well, because the numbers
  there come from the response. The same goes for a runtime error while a
  YAML document is decoded, which is told apart from another by the file and
  the function it was raised in, not by its line.
- Where several labels of one series are over a limit, or are not valid
  label names, the error names the first of them by name, so it is the same
  error on every scrape.

A fetch that fails names the connection that it failed on, and that is left
out too, so that a target which resets every connection is one failure, not
a new one on every probe. Left out are:

- the address and port that the connection was made from, as in
  `read tcp 10.0.0.1:53412->10.0.0.2:80: read: connection reset by peer`,
  which is a different port each time;
- the stream that an HTTP/2 stream error or a `GOAWAY` names;
- the time that an expired certificate was checked against,
  `current time … is after …`;
- the offset at which a compressed answer turned out to be corrupt;
- in what a gRPC call failed with, that address, that time and the size of
  an answer over the limit, `grpc: received message larger than max
  (5007 vs. 1000)`, where the gRPC client itself wrote them: after
  `read tcp` or `write tcp`, after `x509: certificate has expired or is not
  yet valid: current time`, and in those words for the size.

What the server answered a call with is left as it is:
`replica 10.0.0.7:5432->10.0.0.9:5432 is lagging` and the same of another
replica are two failures, unless the server's message holds exactly those
words of the client's. The address that the connection went to stays, and so
does the name server that a lookup failed at: a connection refused after one
that was reset, or the same failure at another address, is a new failure.

A probe or scrape that ends after a
[reload](CONFIGURATION.md#reloading-on-demand) removed or changed its
collector failed or succeeded for a collector that is gone.

- Its failure is written at debug level only, marked `"superseded":true`,
  and neither its failure nor its success changes what is remembered about
  the collector now under that name.
- The reload forgets what was remembered about a collector that it removed
  or changed, when it is made. The first failure of a changed collector is
  therefore logged in full, and its first success after the reload is not
  logged as a recovery from a failure of the old definition.
- A static target that the reload removed from the static target file, or
  changed there, is treated the same way, with its collector as it was: what
  was remembered about it is forgotten, and a scrape that had read it before
  the reload changes nothing of what is remembered about the target now
  under that name.
- A static target's scrape whose result is not published, because the
  reload removed or changed its target or its collector, says so at debug
  level, also marked `"superseded":true`.

A target that answers with an error status usually says why in the body, so
the line for an `http_status` failure adds `response_body`: the start of the
body, at most 256 bytes, on one line. It is only logged, never put in the
probe's answer. An error page can echo what it was sent, so anything in it
that reads as a credential is first masked as `<redacted>`:

- a `Bearer` or `Basic` credential;
- a JSON Web Token;
- the value of a field or parameter whose name reads as a credential's, by
  the rule below for a target's query, including a JSON field written as a
  string in JSON (`\"password\":\"…\"`) and an XML element's text or CDATA
  (`<password>…</password>`).

The masking errs towards hiding: an ordinary word after such a name may be
masked too.

A request that fails quotes the URL it was sending, as Go reports it:
`Get "https://api.example.com/v1/status?api_key=<redacted>": dial tcp ...`.
Every query value is masked and a password in the URL is shown as
`redacted:redacted`, so a token in `request.query` or in the target's query
reaches neither the log, nor the probe's answer, nor a debug report.

A query is shown as it was sent, with values masked where they stand: every
pair keeps its place and its spelling, a bare key stays a bare key, and a
pair that no parser would accept, such as `x=100%`, is still there.

- Pairs are told apart at `&` and at `;`, because servers differ on `;`, and
  are masked by the more cautious reading of the two: the token of
  `a=1;token=SECRET` is masked.
- A name is also everything from an `&` to the first `=` after it, as a
  server that splits at `&` alone reads it, so `token;id=SECRET` is shown as
  `token;id=<redacted>` and `pass;word=SECRET` as `pass;word=<redacted>`.
- Once a value is masked, so is what follows it up to the next `&`, which is
  the rest of that value to a server that splits at `&` alone:
  `token=SE;CRET` is shown as `token=<redacted>;<redacted>`, and
  `token=SECRET;x=1` as `token=<redacted>;x=<redacted>`.
- A name is read as a server reads it, so `%74oken=SECRET` and
  `TOKEN=SECRET` are masked as `token=SECRET` is, each shown under the name
  it was written with.

Wherever a line names the target itself, a password in it is shown as
`redacted:redacted`, and the value of a query parameter whose name reads as
a credential's as `<redacted>`:
`http://host:9100/metrics?token=<redacted>&tenant=a`.

A name is read word by word, its words split at punctuation and at a change
to upper case (`X-Auth-Token`, `client_secret`, `apiKey`). It reads as a
credential's when:
- in any case, `auth`, `cookie`, `token`, `secret`, `password`, `passwd`,
  `passphrase`, `passcode`, `key`, `session`, `signature`, `credential` or
  `jwt` is anywhere in it, once its ordinary words that only contain one of
  these are taken out (`author`, `authors`, `authority`, `monkey`,
  `keyboard`, `keyword`, `tokenizer` and a few like them). So `apikey`,
  `accesstoken`, `authToken` and `author_token` read as a credential's, and
  `author`, `cert-authority` and `keyboard-interactive` do not;
- or `sig` (an Azure SAS signature), `pwd`, `pw`, `pass`, `sid`, `hmac` or
  `otp` is a whole word of it, as in `db_pwd`, `X-Sig`, `userPass` or
  `X-Hmac`, but not in `design`, `signal`, `bypass` or `side`;
- or, for a query parameter's name, `code` (OAuth's authorization code) or
  `ticket` (a CAS service ticket), both of which a redirect back from a
  login carries, is a whole word of it: `?code=<redacted>&state=x`. As
  whole words they also mask some parameters that hold no secret,
  `country_code`, `zip_code` or `promo_code`, but not `barcode` or `codec`.
  In a header's name, or a field's in a logged body, they are left alone,
  since an error body's `<code>401</code>` is no secret. A URL in a logged
  body does have them masked as a query's or a path's parameter, right after
  a `?`, a `&` or a `;` and before a `=`: `href="/cb?code=<redacted>&state=x"`.

A credential word run together with another, as in `apikey`, is still found,
so a name errs towards being masked.

A path parameter, what follows a `;` in a segment of the path, written as it
is or escaped as `%3B`, is masked by the same rule for its name, because a
servlet's session rides in a URL's path when cookies are off:
`/app;jsessionid=<redacted>`. A segment of the path that is a secret as a
whole, such as a bot's token in `/bot123456:AAE…/getMe`, has no name to tell
it by, and is shown. Other parameters are shown as given.

A URL's fragment, `#…`, is never shown: it is never sent to the target, and
one copied from a browser can carry a token. The static targets endpoint's
`target` label and the OTLP `target` attribute show a target the same way,
and a header's value is withheld by the same rule for its name.

The same repeat rules apply to:

- static targets (`static target scrape failed`, then
  `static target recovered`). A stage passed over under `error_handling`
  `log` is `static target stage failed; continuing`, at warning level, as a
  probe's is `probe stage failed; continuing`;
- a file of a directory that fails, and a directory over `max_files` or its
  listing bound;
- probes rejected by `max_concurrent_probes`;
- rules failing under `error_mode: log`;
- output repaired for invalid UTF-8;
- probes answered with the last good result under `cache.stale_if_error`
  (`probe failed; answered with the last successful result`, with its
  `result_age`, then `probe answered with a fresh result again`);
- a metric name that two writers of one OTLP resource export as different
  kinds (`OTLP metric name written as two kinds under one resource; the data
  points of the kind written earlier are left out of the export`, with the
  `metric`, the `kind` exported, the `left_out_kind` and its
  `left_out_points`, and the resource's `service_name`). It is one failing
  thing per resource and name, whichever kind is written last, and has no
  line for its end;
- a name that `otlp.metric_names: otel` exports for two Prometheus names
  (`OTLP metric name written for two Prometheus metric names under one
  resource; the data points of the name written earlier are left out of the
  export`, with the `metric`, the `prometheus_name` exported, the
  `left_out_prometheus_name` and its `left_out_points`, and the
  `service_name`). It is one failing thing per resource and name, separate
  from the one for kinds: see
  [Two writers of one series or one name](OTLP.md#two-writers-of-one-series-or-one-name).

Up to 10,000 failing things are remembered at a time, and one not reported
for an hour is forgotten: the same failure later is logged as new, and its
recovery after the silence is not logged. Past that bound, a new failure is
simply logged every time.

A probe or static target scrape that
[`failure_backoff`](CONFIGURATION.md#backing-off-from-failing-targets) keeps
from its target fails as its fetch would have, in the fetch stage. Its
repeats are one failure, even though the count of failures and the time of
the next try in its text change. The failure that starts the backoff is also
logged once, at warning level: `target is backing off after consecutive
failed scrapes; its probes and scrapes fail without contacting it until the
wait has passed (failure_backoff)`, with its `failures` and `next_try`.

A probe is one failing thing together with everything that makes it the
probe it is: its target, and its parameters and forwarded headers. Probes of
one target that differ in `path`, a `param_` or a `header_` (tenants of one
service, for example) fail and recover separately, and each line carries the
probe's `url`, as the self-metrics label it, so it says which one failed.

