# Changelog

What each release changes for the people who run it. The exporter and the Helm
chart are released separately ([Releases](docs/RELEASING.md)), so each has
sections of its own: `## exporter X.Y.Z` and `## chart X.Y.Z`, newest first.
A section's text is its GitHub Release's description.

Every pull request adds its lines to `## Unreleased`; a release moves the
lines it ships into its own section. Each section begins with
`### What changed for you`: what someone upgrading has to do or will notice,
such as a renamed configuration key, a changed default, something removed or a
changed metric, or "Nothing; you can upgrade without changes." when there is
nothing. `### New` and `### Fixed` follow when they have lines.

## Unreleased

### What changed for you

- A label built from `$url`, the static targets endpoint's `target` label
  and the OTLP `target` attribute show `<redacted>` where they showed the
  value of a query parameter with `code`, `ticket`, `sid`, `otp` or `hmac`
  as a word of its name (`code`, `country_code`, `promo_code`, `user_sid`),
  and of a path parameter whose name reads as a credential's
  (`;jsessionid=`, `;sid=`, after `;` or `%3B`). A target or a URL with one
  makes a new series, and the old one goes stale: adjust the queries,
  recording rules and alerts that matched the old values. Where the query
  does not matter, keep a label to the path with
  `$url | split("?")[0] | split(";")[0]`
  ([Logging](docs/LOGGING.md#repeated-failures)).
- The status page writes a duration as Go does with every unit that is zero
  left out: an interval of `1m`, an age of `35m` and a threshold of `2h`
  where it wrote `1m0s`, `35m0s` and `2h0m0s`. The JSON keeps Go's form
  and its seconds ([Status page](docs/ENDPOINTS.md#status-page)).
- `pue-allocator --settle` takes at most `1h`, and the chart's
  `allocator.settle` fails rendering beyond it, where any duration was
  taken: the targets of a replica gone without a word stay with it,
  unscraped, for the settle time after its member TTL. An allocator started
  with a longer settle time exits 2; set `1h` or less
  ([Members and allocation](docs/ALLOCATOR.md#members-and-allocation)).
- The target allocator bounds each member's load at
  `--allocation.balance-factor`, 1.25 by default, times its fair share, and
  places static and probe targets in one pass: with a few targets a member,
  some that rendezvous hashing put on a member already over its bound go to
  another, and the targets of one shard key, static or probe, stay on one
  member. With many targets a member the bound seldom binds and nothing
  moves: a thousand over five members are placed as before, every one. Give
  `--allocation.balance-factor=10` for hashing alone, for up to 10 members
  ([Weights and capacity](docs/ALLOCATOR.md#weights-and-capacity)).
- A `collector_files` entry of the allocator's configuration that would
  match `allocation.yaml`, such as `a*.yaml`, is refused at load, as one
  matching `static-targets.yaml` is: the bundle may carry a file of that
  name. Narrow the entry, for example to a directory of collector files.

### New

- The exporter's and the allocator's pages show an outline on what the
  keyboard focuses, give search boxes and selects a border that can be seen
  (3:1), tell a screen reader each ✓ ✗ strip as its counts and its latest
  result, and announce a filter's count of the rows it keeps
  ([Endpoints](docs/ENDPOINTS.md)).
- The exporter's status page and the allocator's pages show each moment in
  the browser's time, with UTC and how long ago in its tooltip; without
  JavaScript they show UTC as before ([Endpoints](docs/ENDPOINTS.md)).
- An exporter in allocator mode says its `--web.static-targets-path` in its
  heartbeats, and a member's view on the allocator's page links the
  member's own status page and, of each static target it owns or keeps
  draining, the series it serves of it
  ([A member's targets](docs/ALLOCATOR.md#a-members-targets)).
- The allocator's page has a view of each `discovered_targets` entry,
  `/?entry=<name>`, as a probe job has: its sources, how its targets are
  spread over the members and which member owns each, linked from the static
  targets table and the discovery sources' Feeds column; `/v1/status?entry=`
  serves it as JSON
  ([A discovered_targets entry's targets](docs/ALLOCATOR.md#a-discovered_targets-entrys-targets)).
- With `--web.debug=full`, a recent probe on the status page links its
  debug report, the probe sent again with `&debug=true`, when the page shows
  it as it was sent ([Recent probes](docs/ENDPOINTS.md#recent-probes)).
- On the status page each static target's collector links its section of
  the collectors page, and in allocator mode the page names the member,
  linked to the allocator's view of it, the bundle in force and since when,
  and when the allocator was last heard from; the JSON has them as
  `allocator` ([Status page](docs/STATIC-TARGETS.md#status-page)).
- The status page's static targets table sorts by a column's heading and
  has a filter of name, collector, target, labels or result, kept through a
  refresh ([Status page](docs/STATIC-TARGETS.md#status-page)).
- Each static target's row on the status page has a strip of its last 10
  scrapes, ✓ ✗ ~ …, as the recent probes have, and the JSON has them as
  `recent` ([Status page](docs/STATIC-TARGETS.md#status-page)).
- The allocator's status page tells static targets from probe targets: it
  says what each kind is and how many of each the files write and the
  sources found, groups the members table's columns under the two kinds in
  the bars' colours, adds a static targets table, of the written targets
  and each `discovered_targets` entry, beside the probe jobs table, and
  says which kind each discovery source feeds
  ([Status page](docs/ALLOCATOR.md#status-page)).
- On the status page a scraped static target's name links to its own
  series, `/static-targets?targets=<name>`, and with `--web.debug=full` a
  *debug* link beside it scrapes it once now and reports each stage
  ([Status page](docs/STATIC-TARGETS.md#status-page)).
- The opt-in `tcp` type speaks UDP (`udp: true`: one datagram sent, the
  first back is the answer) and over a Unix socket (`unix_socket`, as an
  `http` collector's), and binary protocols: `send_hex` writes the request's
  bytes, and `answer_bytes` ends an answer of a known length, as Modbus
  TCP's ([Text protocols over TCP](docs/TCP.md#over-udp)).
- An `http` collector speaks FastCGI with `request.fastcgi`, to a port or
  a `unix_socket`, so PHP-FPM's status and ping pages, which PHP-FPM serves
  over FastCGI alone, are read as any HTTP answer
  ([FastCGI](docs/REQUESTS.md#fastcgi)).
- The opt-in `icmp` request type pings a host on an unprivileged ICMP
  socket, as `ping` does, and answers how many echoes came back, the loss
  and the round trips as a JSON document; a host that answers nothing is a
  loss of 1, not a failed scrape ([Ping over ICMP](docs/ICMP.md)).
- `decoder.type: binary` reads a binary answer by the layout of
  `response.binary`, integers, floats, strings and lists of them, into a
  document jq, yq and Python read by the fields' names
  ([Binary answers](docs/CONFIGURATION.md#binary-answers)).
- Weighted allocation: a target's `weight`, a static target file's or a
  probe targets file's `default_weight`, a `discovered_targets` entry's or
  a probe job's `weight`, and `weight_label`, a discovery label whose value
  is a discovered target's own weight, say what each target takes of a
  replica, and the exporter's `--allocator.capacity`, from 0.1 to 100, what
  a replica takes against the others; the allocator places the targets by
  weighted rendezvous hashing with bounded loads, so each member's weight
  follows its capacity. A discovered weight that is no weight falls back on
  its owner's, logged once and counted in
  `pue_allocator_weight_label_fallbacks_total`, and one that moves less
  than 20% is ignored. Every weight is 1 and every capacity 1 unless set
  ([Weights and capacity](docs/ALLOCATOR.md#weights-and-capacity)). The
  exporter refuses a static target file with the weights, naming the
  allocator, as it does `discovered_targets`.
- `pue-allocator --allocation.balance-factor`, from 1 to 10, 1.25 by
  default: how far above its fair share of the weight a member's load may
  go. A factor other than the default travels in every bundle, so instances
  of different factors serve different generations
  ([Weights and capacity](docs/ALLOCATOR.md#weights-and-capacity)).
- The allocator's `/metrics` has `pue_allocator_assigned_weight`,
  `pue_allocator_member_capacity`, `pue_allocator_member_load_ratio` and
  `pue_allocator_overloaded_members`, the members a unit too heavy for
  any member's room puts over their bound, not those a whole unit puts a
  little over it, and `/v1/status` each member's
  capacity, weight, fair share and load ratio, each job's and target's
  weight, and the units heavier than a fair share, which are also logged
  once ([Metrics](docs/ALLOCATOR.md#metrics)).
- The allocator's status page shows each member's capacity, and in the
  Targets table its weight, the weight it keeps draining, its fair share
  and a bar of its weight with its fair share marked, the overloaded
  members marked and the units heavier than a fair share named in a
  warning; the probe jobs table, a member's view and a job's view give each
  job's and each target's weight, and the discovery sources table how many
  weights fell back ([Status page](docs/ALLOCATOR.md#status-page)).
- The chart's `allocator.balanceFactor` sets the allocator's
  `--allocation.balance-factor`, from 1 to 10 with at most three decimals;
  empty keeps the allocator's 1.25
  ([chart README](charts/prometheus-universal-exporter/README.md)).

- `request.ip_family` on an `http` or `graphite` collector chooses the
  address family its requests connect over, as blackbox_exporter's
  `preferred_ip_protocol` and `ip_protocol_fallback` do: `ip4` or `ip6`
  alone, failing when the target has no address of it, or `prefer_ip4` or
  `prefer_ip6` first and the other after, the first given half of the
  time left so that one that never answers leaves the other its half of
  the same probe. Left out, either family, as
  before. A probe parameter and a static target set it too; it is refused
  beside a `unix_socket` or a proxy, which chooses the family itself
  ([Address families](docs/REQUESTS.md#address-families)).
- jq and yq rules of `http` and `graphite` collectors read `$http_version`,
  the version of HTTP the answer came in, `"1.0"`, `"1.1"` or `"2"`, and
  `$remote_ip`, the IP address the request's connection was made to, `null`
  through a proxy or over a Unix socket; a Python script has them as
  `response.http_version` and `response.remote_ip`, and a debug report
  shows them after `$redirects`. The uptime example and recipe answer
  `site_http_version` and `site_ip_protocol`, blackbox_exporter's
  `probe_http_version` and `probe_ip_protocol`
  ([Timings, TLS and the final URL](docs/REQUESTS.md#timings-tls-and-the-final-url)).
- [WebSocket endpoints](docs/REQUESTS.md#websocket-endpoints): the uptime
  recipe checks a WebSocket endpoint's handshake with an `http` collector,
  its request carrying the upgrade's headers and a rule comparing the
  `Sec-WebSocket-Accept` it answers with.
- Allocator instances with a discovery source run side by side as peers:
  `pue-allocator --peers` names every instance, itself included, a name
  resolving to all of them or a list, and `--peers.self` the instance's own
  address. The live instance of the lowest address leads and alone asks the
  discovery sources; the others fetch what it found every 2 seconds, beside
  their checks of the leader so a leader that hangs while it answers does
  not hold them up, and serve the same bundles and targets, and when it
  goes, telling the others for 3 seconds before it stops, the next instance
  leads at once from the targets it took last, and after three failed
  checks when it stops answering. `--peers.check-interval` (2s, from 500ms
  to 1m), `--peers.check-timeout` (1s, below the interval),
  `--peers.failure-threshold` (3, from 1 to 20) and
  `--peers.snapshot-timeout` (30s, up to 5m) set those timings, which the
  status page and `/v1/status` show; the chart sets them from
  `allocator.peerCheckInterval`, `allocator.peerCheckTimeout`,
  `allocator.peerFailureThreshold` and `allocator.peerSnapshotTimeout`,
  refused without a discovery source, and gives the allocator's pods a
  grace period that covers the wait at their stop. New metrics say which
  instance leads, which instances are up (`pue_allocator_peer_up{peer}`) and
  how long a follower has not held the leader's targets ([Instances with
  discovery](docs/ALLOCATOR.md#instances-with-discovery)).
- `pue-allocator` serves a status page at `/`: the instance, its peers and the
  leader, the members and their bundles, how the targets are spread over
  the members, with the average and the imbalance, a table of the probe
  jobs, each with its targets, the members it is routed among, the fewest,
  average and most a member has, the imbalance, marked where the most is
  further above the average than hashing puts targets by chance, and its
  discovery sources, sortable by any column and with a filter box, and
  the discovery sources, refreshing itself as the exporter's status page
  does and writing a duration as it does, `1m` and `1h30s`; and the same
  as JSON at `/v1/status`. Each member links to its own
  view, `/?member=<id>` and `/v1/status?member=<id>`: the static targets it
  owns and drains, with where they moved, and the probe targets routed to
  it by job, credentials withheld, with a filter box and at most 1000
  targets a list ([A member's targets](docs/ALLOCATOR.md#a-members-targets)).
  Each probe job links to its own view, `/?job=<name>` and
  `/v1/status?job=<name>`: its collector, labels, params (credentials
  withheld) and sources, the targets each member has with a bar, and each
  target with the member it is routed to and its labels, its source
  labels (`__meta_…`) left out unless *Show source labels* (`?meta=1`) asks
  for them, filtered and bounded as a member's ([A probe job's
  targets](docs/ALLOCATOR.md#a-probe-jobs-targets)). A
  browser gives `--web.token-file`'s token as the password of Basic auth,
  which the page, `/v1/status` and `/metrics` take beside the bearer token
  ([Status page](docs/ALLOCATOR.md#status-page)).
- `pue-allocator --discovery.refresh-interval`: how often a discovery source
  that sets no `refresh_interval` is asked, whatever its kind, from 1s to
  24h; left out, each kind keeps its own default. The chart sets it from
  `allocator.discoveryRefreshInterval`, and the allocator's
  `--config.watch-interval`, now at most 24h too, from the new
  `allocator.configWatchInterval`, refusing at rendering a value over 24h.
  The status page shows how often the input files are read, the settle
  time and each source's interval, on a follower the leader's, who asks it,
  and the allocator docs list every delay
  between a change and the exporters with the setting of each ([How fast
  changes reach the
  exporters](docs/ALLOCATOR.md#how-fast-changes-reach-the-exporters)).
- `pue-allocator --peers.leader-only`: only the leader serves the whole of
  `/metrics` and of the status page, and the other instances the peers'
  metrics and their own peer state alone, so a Prometheus scraping every
  instance has one instance's series of the allocation; the chart's
  `allocator.leaderOnly` sets it with a discovery source
  ([Leader-only metrics](docs/ALLOCATOR.md#leader-only-metrics)).
- The chart runs `allocator.replicas` allocator pods, 2 by default, with a
  discovery source too, as peers of the allocator's headless Service, rolled
  out one at a time, where it ran one pod and refused more.
- `pue-allocator --heartbeat.interval`, 10s by default, from 1s to 2m: how
  often the replicas heartbeat, which the allocator says in every heartbeat
  answer (`heartbeat_interval_seconds`) and the exporter heartbeats at from
  its next heartbeat on, the shortest when instances of the allocator
  differ; a member is dropped after three intervals, where the interval was
  fixed at 10s and the member TTL at 30s. The exporter heartbeats each
  instance on a schedule of its own, giving up on a heartbeat after 5s or
  the interval when that is shorter, so an instance that hangs makes the
  replica late to no other, and looks up the instances' addresses at the
  interval in force, a new one taking effect at once. Each heartbeat says
  the longest interval the instances the replica finds said
  (`interval_seconds`), once one has said one, each instance told apart by
  the name its answer carries (`instance`), so instances answering one
  address in turn keep their own say; an instance's first member list waits
  the longest any says after the first heartbeat it hears, so an instance
  started with a shorter interval than the instances before it hears the
  replicas on the old interval first, also when the lookups find it late
  and when the first it hears started after it. The
  status page shows both and marks a member whose heartbeat is overdue
  ([Members and allocation](docs/ALLOCATOR.md#members-and-allocation)).
- The chart's `allocator.heartbeatInterval` sets the allocator's
  `--heartbeat.interval`, and the allocator Deployment's `minReadySeconds`
  is `max(allocator.settle, 2m)` plus it, so a new allocator pod counts as
  available only once its first member list has formed and reached the
  exporter pods, also during a rollout that shortens the interval, whose
  old value the chart cannot know.

### Fixed

- An exporter in allocator mode whose lookup of the allocator's addresses
  failed sent no heartbeat until one succeeded, and fell out of the member
  list after the member TTL though every instance was up. It now heartbeats the
  addresses it resolved to last
  ([Members and allocation](docs/ALLOCATOR.md#members-and-allocation)).
- `pue-allocator` logged an `http_sd` source's failure, and refused a
  `url` that is not an `http` or `https` URL, with the URL as written, a
  token in its query, `?access_token=…`, shown in clear. The values of query
  parameters whose names read as credentials' are now masked there, as a
  target is in a log line, and on the new status page; a `url` that is no
  URL at all is no longer shown ([Sources](docs/ALLOCATOR.md#sources)).
- `$url`, a target in a log line, the static targets endpoint's `target`
  label, the OTLP `target` attribute, a debug report and an error showed a
  session id in the URL's path, `/app;jsessionid=…`, and OAuth's `code` and
  CAS's `ticket` in its query, which the redirect back from a login carries.
  The value of a path parameter whose name reads as a credential's is now
  masked, `/app;jsessionid=<redacted>`, after an escaped `;`, `%3B`, too,
  and `code`, `ticket`, `sid`, `hmac` and `otp` are credentials' names, each
  as a whole word: `code` and `ticket` of a query or path parameter alone, a
  URL's in a logged body among them, the others of a header and a field in a
  logged body too. A query parameter of a country's or a promotion's code,
  `country_code`, is masked as well
  ([Logging](docs/LOGGING.md#repeated-failures)). A debug report and the
  debug page showed a redirect's `Location`, and `Content-Location`,
  `Refresh` and `Link`, as they came; their URL is now masked as a request
  line's is ([Debug probes](docs/CONFIGURATION.md#debug-probes)).
- An answer `101 Switching Protocols`, as a WebSocket endpoint gives an
  `http` collector that asks for the upgrade, had what followed it on the
  connection, the WebSocket's frames, read as its body until the probe's
  deadline, which failed the probe. It is now the status and the headers
  alone, and its connection is closed at once.

## exporter 2.0.0

### What changed for you

- The self-metrics have a histogram more of each collector, always:
  `http_exporter_probe_duration_seconds{collector,kind}`, 16 more lines of
  the text format a collector, and 16 more for one a static target names.
- A Prometheus that asks for its protobuf format first, as it does when it
  scrapes native histograms, gets the self-metrics in protobuf, with
  `http_exporter_collector_scrape_duration_seconds` carrying native buckets
  beside its classic ones; it got OpenMetrics before. Every other scraper
  gets what it got. `web.self_metrics.native_histograms: false` answers it in
  OpenMetrics again.
- Fewer Python workers stay idle, for less long: after a burst of scrapes one
  worker of each collector's script stays idle, rather than four, and an idle
  worker stops after two minutes unused, rather than five. Each is a CPython
  process of about 11 MB, 17 MB with lxml, so ten Python collectors keep
  about 110 to 170 MB in idle workers rather than 440 to 680 MB. Two minutes
  is twice a scrape interval of a minute, so a collector scraped every minute
  or more often still keeps its worker warm. `--python.max-idle-workers` and
  `--python.idle-timeout` bring the old behaviour back (`4` and `5m`).
- The `go_` and `process_` resource metrics are published on the
  self-metrics endpoint by default: reading them no longer stops the world,
  because the memory statistics now come from `runtime/metrics` and the
  garbage collection pauses from `runtime/debug.ReadGCStats` instead of
  `runtime.ReadMemStats`. Set `web.self_metrics.resource_metrics_enabled:
  false` to leave them out. `go_gc_duration_seconds` now carries the
  quantiles 0, 0.25, 0.5, 0.75 and 1 beside its `_sum` and `_count`, as in
  client_golang. The series names, types and help texts are as before.
- The JSON schemas refuse more of what the exporter refuses, so an editor now
  marks it: durations and sizes under the exporter's least (`script_timeout`
  under 1ms, `max_script_memory` under 32MiB, intervals under 1s, a zero
  `failure_backoff` duration), a client certificate without its key or a key
  without its certificate, two credentials in one request, a pagination key of
  another paging type, `grpc` descriptor files beside the wrong source, and a
  `python` transform without a `script`.
- A key that takes one of a few words takes them only as the schemas write
  them: `request.type`, `transform.type`, `decoder.type`, the
  `error_handling` policies and a rule's `error_mode`,
  `request.pagination.type`, `request.descriptors` and `response.graphite`'s
  `value` and `invalid_lines` in lower case and without blanks around them
  (`log`, not `LOG`; `type: http`, not `HTTP`), and `request.method` in upper
  case or lower case (`GET` or `get`, not `Get`). A configuration that writes
  one otherwise is refused, naming the words taken: write them in lower case.
  The static target file's schema now takes a `method` in lower case, as the
  exporter does.
- An entry of `accept_status` or `retry.statuses` written in quotes is taken
  only as the schemas take it: a status's three digits (`"503"`) or a class
  (`"5xx"`). Text with a sign, leading zeros or blanks, such as `"+503"`,
  `"0503"` or `" 503 "`, is refused when the configuration loads; write
  `503`. An unquoted number is read as before.
- The schemas refuse an entry of a `grpc` collector's or target's
  `retry.codes` and `accept_codes` that names no gRPC status code, or `OK`,
  as the exporter does, so an editor now marks it.
- `starttls` on an `ldap` collector is refused as a key `ldap` does not take,
  pointing to `start_tls: true`; `start_tls` on a `tcp` or `tlscert` collector
  points to `starttls` and its protocols.
- The image declares a `HEALTHCHECK`, so Docker shows the container
  `healthy` while the exporter is ready. A container started with another
  `--web.listen-address` needs `--health-cmd` with it, and the target
  allocator run from the image needs `--no-healthcheck`.
- The debug page calls the collector written on it the playground, and runs
  only when *Run* is pressed: *Run as you type* is gone. *Probe a target*
  probes through the configured collector chosen, or, with the playground,
  through the one it was copied from with *Edit a copy*; the *Fetch through*
  list is gone, and a playground copied from none cannot fetch. *Scrape a
  static target* is a list of the static targets of that same collector,
  each shown with its address, credentials withheld, in place of a name to
  type, and follows reloads; `/debug?static_target=<name>` chooses the
  target and its collector. The pasted body starts empty with a configured
  collector, the example's answer only with the example.
- The exporter's pages show the project's logo as their browser-tab icon.
- The status page shows a long error — and a long target, labels or probe
  parameters — as its first 160 characters or three lines with *Show all*,
  which opens the whole text in a box over the page; the page does not
  refresh while the box is open. On the debug page *Run* stands level with
  the timeout box beside it, and the status page's Refresh button and
  auto-refresh selector are one height, centred on one line.
- A `SIGTERM` or `SIGINT` no longer waits for the static target scrapes in
  flight: they are cut short at once, logging nothing above `debug`, so a
  target that does not answer no longer holds Ctrl-C up for all of
  `--web.shutdown-timeout`. The last OTLP export goes without what those
  scrapes would have queued. Probes in progress are still waited for, and the
  log now says how many, for how long at most, what is still awaited every 5
  seconds, and what it gave up on when the timeout runs out. A second signal
  logs `second signal: exiting now, …` before the process ends by it, as
  before. The line the shutdown begins with now reads
  `shutting down: waiting for 1 probe in progress, at most 15s; …`, or
  `shutting down: no probes in progress; exiting`.
- The debug page no longer keeps the playground, its body or the collector
  it was copied from in the browser between visits. A collector run on what
  another collector fetches, through `request_from` or a static target,
  must now be of that collector's request type (`400` otherwise), where it
  had to be an `http` collector before.
- The debug page's *Test the metrics endpoint*, *Test the logs endpoint* and
  *Test the traces endpoint* are in a panel of their own below *Output*,
  *OTLP endpoints*, there before anything has run, rather than at the
  bottom of the *OTLP* tab.
- The OTLP export writes `aggregationTemporality` as the number `2` where it
  wrote the name `AGGREGATION_TEMPORALITY_CUMULATIVE`: the OTLP/JSON encoding
  requires an enum's number, which every receiver accepts. A receiver or a
  test that matched the request body's text sees the number now.

### New

- jq and yq rules of `http` and `graphite` collectors read `$timings`, how
  long the request's phases took in seconds — `dns`, `connect`, `tls`,
  `first_byte` and `total` — measured on the request that was answered,
  with `null` for a phase it did not have, such as the handshake over plain
  HTTP or the connection a kept one spared; a Python script has the same as
  `response.timings`, and a debug probe's report shows it under the status.
  Every jq and yq program has the variable, `null` for other request types
  ([Timings, TLS and the final URL](docs/REQUESTS.md#timings-tls-and-the-final-url)).
- jq and yq rules of `http` and `graphite` collectors read `$tls`, the TLS
  connection the answer came on — `tls_version`, `cipher_suite`, `server_name`,
  `verified`, and the target certificate's `not_before`, `not_after`,
  `subject_cn` and `issuer_cn` — so an uptime check alerts on the
  certificate's expiry without a `tlscert` collector beside it; `null` over
  plain HTTP. A Python script has it as `response.tls`
  ([Timings, TLS and the final URL](docs/REQUESTS.md#timings-tls-and-the-final-url)).
- [Uptime probes](docs/REQUESTS.md#uptime-probes) puts an uptime check of
  HTTP and HTTPS sites together, as blackbox_exporter's `http` prober makes
  it: one collector, shipped as `configs/config.uptime.example.yaml`, the
  Prometheus job, what each series means, and a table of blackbox_exporter's
  series with what answers each here. The collector connects anew on every
  probe (`new_connection`), so a site's new address or a certificate that
  no longer verifies is seen at once, as blackbox_exporter sees it.
- `request.new_connection: true` on an `http` or `graphite` collector sends
  every request — each retry and each followed redirect too — on a
  connection of its own, closed after its answer, so each looks the name up,
  connects and verifies the certificate the target serves now, and
  `$timings` has its lookup, connection and handshake; a probe parameter and
  a static target set it too
  ([Connections](docs/REQUESTS.md#connections)).
- jq and yq rules of `http` and `graphite` collectors read `$url`, where the
  request went after its redirects, without its userinfo and with credential
  query values `<redacted>` as a target in a log line, and `$redirects`, how
  many redirects were followed; a Python script has them as `response.url`
  and `response.redirects`
  ([Timings, TLS and the final URL](docs/REQUESTS.md#timings-tls-and-the-final-url)).
- `report_failures: true` on a collector answers every probe of it `200`
  with an exposition, never `502`: `http_exporter_probe_success` 1 or 0,
  `http_exporter_probe_elapsed_seconds`, and on a failure
  `http_exporter_probe_failed_stage{stage}` naming the stage, `panic` for a
  probe that panicked, with nothing of the error. With `method: HEAD`, `accept_status: [any]` and
  `decoder.type: none` it is an uptime check that alerts on a value, as
  blackbox_exporter's `probe_success`. Failures are logged and counted in
  the self-metrics as before, and a probe `error_handling` carried on past
  a failed stage is one: counted no success, shown failed on the status
  page, answered by `cache.stale_if_error`'s last good result and traced
  as failed, where without the key it is a success; a failed probe's trace
  has the stage as its `error.type`, not a status; static targets, which have
  `http_exporter_target_up`, are not changed
  ([Reporting failed probes as metrics](docs/CONFIGURATION.md#reporting-failed-probes-as-metrics)).
- `decoder.type: none` reads no body: jq and yq rules read `null` with
  `$status` and `$headers`, and a Python script's `data` is `None`, so an
  uptime check with `method: HEAD` no longer reads an empty or HTML body as
  YAML to get at the status, and a body that is no document fails nothing.
  A `regex`, `csv`, `css`, `xpath` or `prometheus` transform, and
  `response.charset`, are refused beside it
  ([No decoder](docs/CONFIGURATION.md#no-decoder)).
- `request.accept_status: [any]` accepts every status from 100 to 599, so
  an uptime check decodes whatever the target answers and its rules compare
  `$status`, instead of listing `["1xx", "2xx", "3xx", "4xx", "5xx"]`; it is
  written alone, and `retry.statuses` with entries is refused beside it,
  since an accepted status is not retried
  ([Accepting other statuses](docs/REQUESTS.md#accepting-other-statuses)).
- With `otlp.traces`, the request spans of dns, sql, grpc, ntp, ldap,
  tlscert, redis, sshfile and s3file collectors tell how the request ended
  in attributes the exporter works out of the answer, never the target's
  text: the record type and the records' count, the rows and columns
  returned (`db.response.returned_rows`) and whether the connection was
  kept, `rpc.grpc.status_code`, the stratum, offset and delay, the LDAP
  result code and a page's entries, the TLS version, cipher and leaf's
  expiry (`tls.protocol.version`, `tls.cipher`, `tls.server.not_after`)
  with the certificates' count and whether they verified, a Redis reply's
  type and size, and the files or objects read and their bytes
  ([Traces of slow requests](docs/OTLP.md#traces-of-slow-requests)).
- `otlp.traces.propagate: true` sends a kubernetes collector's requests to
  the API server the `traceparent` and `tracestate` an HTTP target's
  request carries, so an API server traced with `APIServerTracing` makes
  its spans of the listing children of the exporter's.
- `http_exporter_probe_duration_seconds`, a histogram of every collector's
  probe durations (`kind="probe"`) and static target scrape durations
  (`kind="static"`, for a collector a static target names), as
  `http_exporter_scrape_duration_seconds` measures them, cache hits and
  shared probes included, published with or without verbose self-metrics,
  never by target ([Probe durations](docs/SELF-METRICS.md#probe-durations)).
  It has the buckets of the verbose trip histogram, which stays, its native
  and exponential buckets, and, with `otlp.traces`, exemplars of the
  exported traces of probes and scrapes.
- `metric_name_validation_scheme: utf8` on a collector, Prometheus 3's
  setting: the collector keeps UTF-8 metric and label names such as
  `http.server.duration` as they are, and each answer writes them as its
  scraper's `Accept` asks — quoted (`{"http.server.duration"} 1`) for
  `escaping=allow-utf-8`, escaped by `underscores` (the default), `dots` or
  `values` otherwise — with the escaping in its `Content-Type`. The default,
  `legacy`, is the classic names and `name_escaping` as before.
- Native and exponential histograms of the exporter's own scrape durations:
  `http_exporter_collector_scrape_duration_seconds` keeps its durations in
  exponential buckets too, which follow the durations observed (scale 20
  down to whatever fits 160 buckets, as OpenTelemetry's SDKs do). The
  self-metrics write them as a native histogram, schema 3 at most, in
  Prometheus's protobuf format (`web.self_metrics.native_histograms`, on by
  default), and the OTLP export sends them as an exponential histogram with
  `otlp.exponential_histograms: true`.
- Metrics, logs and traces linked, with `otlp.traces` on
  ([Linking metrics, logs and traces](docs/OTLP.md#linking-metrics-logs-and-traces)).
  With `otlp.logs` too, a log record of a line a probe or a static target
  scrape logged, such as `probe failed`, carries its trace's `traceId`, the
  `spanId` of the stage it was logged in and `flags` 1, when that trace was
  exported by the time the record is sent; stderr's lines are as before.
  With verbose self-metrics, each bucket of
  `http_exporter_collector_scrape_duration_seconds` has as its exemplar the
  latest trip of an exported trace, in OpenMetrics, protobuf (classic and
  native) and OTLP. `otlp.traces.propagate: true` sends each HTTP, graphite
  and gRPC request to a target a W3C `traceparent` under its span's id, the
  sampled flag set where `parent_based` or `sample_ratio` has already kept
  the trace (never for a debug page run, whose trace is not exported), and
  with the trace export off passes a probe's caller's `traceparent` and
  `tracestate` on as they came; a `traceparent` in `request.headers`,
  `request.metadata` or forwarded is sent as it is, a `tracestate` set there
  without one is dropped, and a login carries none.
- Sampling rules for `otlp.traces`, tried after `threshold` and `errors`:
  `parent_based: true` exports the trace of a probe whose caller's
  `traceparent` is sampled, and `sample_ratio` a share, 0 to 1, of the rest,
  decided from the trace id as OpenTelemetry's consistent probability
  sampling decides it, the kept traces' spans carrying `ot=th:<threshold>`
  in their `traceState` before the caller's `tracestate`.
  `max_per_second` caps every trace kept, a token bucket in which traces
  sampled by the caller or the ratio leave half the tokens to slow and
  failed ones. The probe's span says why in `sampling.reason`; the traces
  kept and dropped are counted by reason in
  `http_exporter_otlp_traces_kept_total` and
  `http_exporter_otlp_traces_dropped_total` and on the status page, and the
  debug page's *OTLP* tab says which rule would keep a run's trace, or why
  none would. A caller's `tracestate` is now carried on the spans of its
  probe's trace. Without the new keys the export is as it was.
- Traces of slow requests over OTLP: with `otlp.traces` switched on, each
  probe and static target scrape that took `otlp.traces.threshold` (1s by
  default) or longer, and with `otlp.traces.errors: true` each that failed,
  is exported as a trace to the collector's `/v1/traces`: a span for the
  probe or the scrape, and under it one for each stage of its trip — the
  cache, the wait for a slot, the fetch, the decode, the Python scripts, the
  transform, the validation — and one for each request it sent, every retry,
  redirect, page and login, by OpenTelemetry's conventions, credentials and
  query values withheld. A caller's `traceparent` is continued. The traces
  go under the probes' and the static targets' resources with
  `otlp.exporter_instance` and `otlp.scope`, wait in a queue bounded by
  `otlp.traces.max_pending_spans`, cost a probe nothing while the block is
  off, and are counted in `http_exporter_otlp_trace_exports_total`,
  `http_exporter_otlp_spans_sent_total` and
  `http_exporter_otlp_spans_dropped_total` and on the status page's *Trace
  export* part. The debug page's *OTLP* tab shows a run's spans as a tree
  with their times and whether the export would send them, and tests the
  traces endpoint; a run is never exported. See
  [Traces of slow requests](docs/OTLP.md#traces-of-slow-requests).

- `otlp.exporter_instance` names the exporter replica that sends the OTLP
  export, as one more attribute of every resource it sends: the probes',
  the exporter's own metrics', every static target's and the log records'.
  The attribute is `pue.exporter.instance` unless `attribute` names another,
  and the value is `value`, or, left out, the `--allocator.member` name in
  allocator mode and the host name, a pod's name, otherwise. An attribute
  the resource has already — `service.name`, a key of
  `otlp.resource_attributes`, a static target's own attribute, a label it
  moves to the resource, or `service.instance.id` beside its `instance_id` —
  is refused. The status page's OTLP section, the debug page's OTLP tab,
  `--dry-run` and the start's line show it; off, the default, the export is
  as before. See
  [The exporter instance](docs/OTLP.md#the-exporter-instance).
- The debug page's collector editor colours the YAML as it is typed — keys,
  strings, numbers, booleans, comments, a list's dashes, block scalars,
  anchors, tags, `{{param_<name>}}` placeholders and `${NAME}` references,
  and a withheld value muted — in either theme, the text staying the
  browser's own, with undo, Tab and paste. A block folds behind a button in
  the gutter, which the keyboard reaches too, its first line saying how many
  lines it hides and the gutter keeping each line's number, with *Expand
  all* and *Collapse all*; the playground is read-only while a block of it
  is folded, and a click on its text edits it there.
- The debug page has an *OTLP* tab beside *Table* and *Text*, with
  `otlp.enabled`: what the next export would send of a run's series, made by
  the export's own code — the resource, the scope, each data point with its
  OTLP name, kind, unit, attributes, value and what was changed of it, the
  points left out and why, and the request's JSON as a tree that folds,
  coloured, with *Expand all* and *Collapse all*, a folded attribute
  reading `service.name = "…"` — and with `otlp.logs` the
  run's log lines as log records. Nothing of a run is exported. At
  `--web.debug=full` it tests the endpoint with one empty export, sent once
  with the export's headers, TLS, compression and timeout
  (`POST /debug/otlp-test`).
- `--python.max-idle-workers` (the chart's `server.pythonMaxIdleWorkers`),
  `1` by default, says how many workers of each collector's script stay idle
  after a burst of concurrent scrapes; `0` keeps none, each worker stopping
  when its run ends. `--python.idle-timeout` (`server.pythonIdleTimeout`),
  `2m` by default and at least `1s`, says how long an idle worker is kept
  unused. Idle workers are checked every minute, or every half the timeout
  when that is shorter. See
  [Idle workers and memory](docs/PYTHON.md#how-scripts-run).
- `--web.debug.dangerous` (the chart's `server.debugDangerous`), with
  `--web.debug=full` only, lets the debug page's playground run Python, name
  the server's files and probe a target with its own request, for writing a
  collector on a machine of one's own; never on a shared deployment.
- The pages' bar has a *Theme* picker: *Auto*, the default, follows the
  browser's light or dark preference, and *Light* or *Dark* holds whatever it
  prefers, kept in the browser for every page.
- `--healthcheck` asks a running exporter whether it is ready (`/ready`), or
  with `--healthcheck=health` alive (`/health`), and exits 0 or 1, for a
  container health check in an image without `curl`.
- `request.unix_socket` sends an `http` collector's requests over a Unix
  domain socket, such as Docker's `/var/run/docker.sock` or a php-fpm status
  socket, as `curl --unix-socket` does; the URL's host is still the `Host`
  header. A path longer than the system allows (107 bytes on Linux, 103 on
  macOS) is refused when the configuration loads.
- `--dry-run` lists how each `http` and `graphite` collector connects under
  `request_policies`: its `unix_socket`, or the `proxy` in force — its
  `proxy_url`, from its own request or its file's `defaults`, `direct`, or
  `environment` when it follows `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY` —
  with the paths of `proxy_basic_auth_file`, never the credentials.
- `otlp.logs` sends the exporter's own log lines over OTLP as well, to the
  collector its metrics go to (`/v1/logs`), from `otlp.logs.level` up, with
  the `otlp` block's headers, TLS and retries; stderr is written as before.
  Logging never waits for it: a line that finds `max_pending_records` waiting
  is dropped and counted (`http_exporter_otlp_log_*`), and the status page
  shows the log export. It never makes the exporter unready.
- The status page has a Refresh button and an auto-refresh selector, as a
  Grafana dashboard has: `Off`, or every `5s` to `1h`, kept in the URL as
  `?refresh=30s`, refreshing the page's content in place, pausing while the
  tab is hidden, and saying once, beside the controls and in the browser's
  time, the time the data shown is of, or that a refresh failed.
  `--web.status-refresh`, and the chart's `server.statusRefresh`, set the
  interval a URL without one gets; off by default.
- `otlp.metric_names: otel` names the OTLP export's metrics as OpenTelemetry
  does: a counter without its `_total`, and a unit suffix such as `_seconds`
  or `_bytes_per_second` moved into the metric's unit (`s`, `By/s`). The
  default, `prometheus`, keeps the names as they are.
- `otlp.scope` sets the instrumentation scope's name and version of the
  exported metrics and log records; left out, it is
  `prometheus-universal-exporter` without a version, as before.
- A static target's `otlp.labels_as_resource` moves labels of its own from
  every exported data point to its OTLP resource, and `otlp.instance_id: true`
  gives the resource `service.instance.id`, the target's name.
- The debug page shows a run's series as a table, beside the Prometheus
  text: a row for each sample, a column for each label, the value and the
  type, sorted by a column's header, filtered by text and by metric, and the
  metric lint's advice on the rows of the metric it is about. The text view
  is the whole answer, as before, and `/debug/run` answers the series one
  sample at a time as `samples`. The response radio that probes a target is
  now called *Probe a target*. A pasted response's headers and the probe
  parameters are rows of a name and a value, added with *+ Add header* or
  *+ Add parameter* and removed with *×*, in place of a text box of lines:
  a row is checked as the server would check it and marked with why, the
  collector's own `{{param_<name>}}` parameters are rows to start with,
  their defaults shown, and the others it takes are suggested as names. A
  run's `headers` and `params` take a list of values for a repeated name.
  *Copy as* gives a probe of the form as its `/probe` URL, a `curl` command,
  a Prometheus `scrape_config` for the collector and the target, and the
  debug probe's URL, and a static target as its `/static-targets` URL and
  `curl` command; no credential is put in them.
- The debug page's editor numbers its lines in a gutter beside the text, and
  marks the lines a refusal of the playground names, in the gutter and with
  a band behind each; `line 7` in the error is a link that selects the line.
  In the playground Tab indents by two spaces and Shift+Tab outdents (Esc,
  then Tab, leaves the editor), and Ctrl+Enter or ⌘+Enter runs from
  anywhere on the page.

### Fixed

- A field, header or query parameter named `author`, `authority`,
  `keyboard` and the like is no longer masked as a credential: a name is now
  read by its words, split at punctuation and at a change to upper case,
  and an ordinary word that only holds `auth`, `key` or `token` (`author`,
  `authors`, `authority`, `monkey`, `keyboard`, `keyword`, `tokenizer`, …)
  no longer counts, so `author: Bob` and `<author>Bob</author>` are shown.
  `Authorization`, `X-Auth-Token`, `apikey`, `authToken` and the rest are
  masked as before.
- The debug page's JSON viewer opens an array of any size: one of more
  than some 100000 items, such as a body of 200000 points, threw
  `RangeError` and stayed empty for good, and near the top of the body left
  the last run's tree in the box. Items are now made a couple of thousand a
  frame, and a node or a body that cannot be shown says so.
- A trip shared by identical probes is in the trace of every probe that
  used it: when the probe that started it was left by its caller, the
  trip's spans were in no trace, the probes that joined it had only their
  `shared_trip` span, and the one that left had a request span without a
  status and the message that it waited for an identical probe's trip,
  which it had started. Each probe now has a copy of the trip's spans; one
  whose caller left has what the trip had done by then, a request still
  unanswered marked `request.unfinished`, and a message that says whose
  trip it was.
- A probe or a scrape that ended after a reload switched `otlp.traces` off
  is no longer queued, to be sent if a later reload switches the export on
  again before the next export.

- A trace's spans no longer carry text the target sent that the log never
  shows: the CLIENT span of a tcp or sshexec request that went through had
  the server's greeting, or the command's whole stderr, as `request.outcome`,
  and a failed request the reason of its HTTP status line. A span now has,
  of a request that went through, only numbers its request type works out
  (`tcp.greeting.bytes`, `tcp.greeting.matched`, `tcp.answer.bytes`,
  `process.exit.code`, `sshexec.stdout.bytes`, `sshexec.stderr.bytes`), and
  of a failure what the log line of that failure says.
- A failed trace export, or a reload lowering `otlp.traces.max_pending_spans`,
  no longer cuts the oldest trace waiting span by span, which exported the
  rest of it without its root or its stages: the oldest traces are dropped
  whole.
- A probe whose caller read its answer slowly is no longer taken for a slow
  probe by `otlp.traces.threshold`: it is timed, and its span ends, when its
  answer is ready, as its duration in the self-metrics is.
- A new `otlp.interval`, `otlp.logs.interval` or `otlp.traces.interval` is
  followed at once: a reload that shortened one waited out the old interval
  first, turning the export on waited up to 30 seconds, and an exporter in
  allocator mode made its first export about 30 seconds after its first
  bundle, whatever its interval said. The next export is now an interval of
  the new configuration after the last one.
- The debug page's OTLP endpoint test shows every `otlp.headers` value as
  `<redacted>`, by its name alone: a vendor's API key under a name that
  does not read as a credential's, `x-honeycomb-team` or `uptrace-dsn`, was
  shown as sent.
- The debug page's configured view and *Edit a copy* withhold a request
  body that writes a credential as an XML element, `<password>…</password>`,
  its text or its CDATA, or as JSON written as a string in JSON
  (`\"password\":\"…\"`); the log masks both in a target's error body too.
- The debug page withholds a `tcp` send that logs in with IMAP's
  `a1 LOGIN user …`, Redis's `HELLO 3 AUTH user …`, `AUTH` framed as RESP,
  or `AUTH …` after a `;`.
- The debug page's *OTLP* tab writes a run's points at whole milliseconds,
  as the export does, not in nanoseconds.
- The line that a debug probe or a debug scrape was served is written to
  stderr alone and no longer reaches the OTLP log export.
- The status page no longer shows the last error of a static target whose
  last scrape succeeded as if it still failed beside `success` and 0
  failures: the error stays, muted, under *Earlier failure, recovered since*,
  with when it was and how long ago. The recent probes and the OTLP, log and
  trace exports' last error follow the same rule, and the JSON says it with
  `last_error_recovered`.
- The debug page no longer shows a credential written in free text of a
  configured collector's request, in its view or in *Edit a copy*: an `http`
  body, a `grpc` message, a `tcp` send (`AUTH …`, `PASS …`), a `redis`
  command's argument, a `sql` statement or argument, a query parameter's
  value, a folded block among them, is shown `<redacted>` whole when
  something in it reads as a credential.
- *Edit a copy* of a collector whose `tls` or other block is written on one
  line (`tls: {ca_file: /etc/ca.pem, server_name: api}`) loads: the comment
  for the dropped file stands on a line of its own, where a stand-in key was
  left that the playground refused.
- *Edit a copy* of a `grpc` collector with `descriptors: proto` or
  `protoset` loads: the playground, which makes no request, no longer asks
  for the `proto_files` or `protoset_file` the copy leaves out.
- *Edit a copy* is disabled until the collector's YAML is read, so it no
  longer copies the page's `Reading the configuration…` line or an error.
- *Copy as* *scrape_config* writes `metrics_path` as the path reads
  (`/my pfx/probe`), which Prometheus escapes once, not escaped twice.
- The status page no longer swaps its content under the long-text box when
  a refresh already on its way answers while the box is open; the answer is
  shown when it closes, and focus goes back to *Show all*.
- The collectors page's *Debug* link no longer keeps a target cleared from
  the form since it was last followed.
- The debug page's `Ctrl+Enter runs` hint is level with *Run* and the
  timeout's box.
- The debug page asks for what the chosen collector's request type takes,
  not for an HTTP target: the target as the type names it, with an example
  of its forms (`SSH host`, `host or host:22`; `Name to resolve`,
  `example.com`), no target field for a collector that names its own
  (`kubernetes`), the collector's own `{{param_...}}` parameters with their
  defaults, or a note that it takes none, and, for a pasted response, what
  the type's answer is, with a status and headers only for `http` and
  `graphite`. A response pasted for a `sql` collector is read as its rows,
  a JSON list of objects, where it gave no series before. The collectors
  page labels and hints each target the same way.
- The problems of a one-line block (`defaults: {...}`, a collector's
  `request`, `otlp`, `web.basic_auth`) are said in the order of the keys on
  the line.
- An answer sent uncompressed no longer allocates for the answer-size
  counters.
- A `cache`, `failure_backoff` or `request.tls` block, and a static target's
  `request`, say every unknown key and value of the wrong kind they hold, not
  only their first unknown key; a mistake in a `cache` or `failure_backoff`
  no longer hides the problems in the rest of the file.
- The schemas take `request.pagination.size: 0`, which the exporter reads
  as `size` left out: the offset moves on by the items each page held.
- Reading a scraper's `Accept-Encoding` header no longer allocates on every
  scrape.
- GitHub Releases are described from `CHANGELOG.md` instead of a list of pull
  requests.
- The status page has the bar the other pages have at their top: back to the
  landing page, the other pages and the documentation. Every HTML page now
  carries the same bar, the page shown marked.
- The debug page shows the configured collector chosen as YAML, as its file
  writes it, read-only, with its credentials and file paths withheld and the
  keys it takes from its file's defaults marked, instead of the example,
  which it does not run; *Edit a copy* writes it on the page. A link from
  `/collectors` opens the page with it shown.
- The debug page's playground may be of any request type the build has, so
  *Edit a copy* of a `tlscert`, `sql`, `dns` or other collector now runs: on
  a pasted answer of that type, or on what the collector it was copied from
  answers for a target or a static target. Before, every run of such a copy
  was refused with `422: ... a collector written on the debug page is an
  http collector`. The playground's request block still makes no request,
  and a playground with a `*_file` key, a `login` or Python is still
  refused.
- The debug page opens with the example playground every time, not with
  what was written or copied on it at an earlier visit; *Back to the
  example* puts it back in place of a copy.
- A pasted response asks for a `Content-Type`, a status and headers only
  when the playground's `request.type`, as the editor writes it, is `http`
  or `graphite`, and its placeholder says what that type's answer is; the
  fields follow the type as it is typed.
- *Scrape a static target* says whose static targets it lists: the
  collector chosen, or the one the playground was copied from.
- A `localfile` read's `READ` line in a debug report, and its verbose `url`
  label, show the file's `file://` URL escaped as a URL is: a space in the
  path is `%20`, where it was shown as a literal space, and a `?` or a `#`
  in a file's name no longer reads as a query or a fragment. The `sftp://`
  URL of an `sshfile` read, the `s3://` URL of an `s3file` object and the
  database of a `sql` statement's URL are escaped the same way.
- The debug page shows a configured collector's folded block scalar, such
  as a `statement: >-` written over several lines, over the lines its file
  writes it on, where it was folded into one long line that ran off the
  right of the editor.
- A debug report's request lines, on `/probe?debug=true` and the debug page,
  show the values of the query parameters the collector's configuration
  sets as its file writes them, where every value was `<redacted>`: a
  `graphite` collector's `target`, `from`, `until` and `format=json`, an
  `http` collector's `request.query` values without placeholders and a
  `kubernetes` listing's `limit` and selectors. A value a probe parameter,
  a placeholder or a `${NAME}` reference gives, the target's own query and
  any parameter whose name reads as a credential's stay masked, as every
  query value of an error, which a probe's caller is told, does.
- *Edit a copy* of a configured collector that names files (`tls.ca_file`,
  `password_file`, `bearer_token_file`, `known_hosts_file`, …) or logs in
  gives a playground that loads: those lines are left out, each replaced by
  a comment saying why, with a block they leave empty, and a note above the
  editor lists them. It was refused until they were deleted by hand.
- `--healthcheck` with `--web.listen-address=[::]:PORT` asks `127.0.0.1`,
  which the exporter's `[::]` listener answers too, where it asked `[::1]`
  and failed on a host without IPv6, so `healthProbes.mode: exec` restarted
  the pod for ever. A named port (`:http`) and an IPv6 zone
  (`[fe80::1%eth0]:8080`) give a URL the check can ask, where it could not.
- With `otlp.metric_names: otel` two counters that are one name there, such
  as `x_seconds` and `x_seconds_total`, or `requests` and `requests_total`,
  are no longer merged into one metric that kept one point of the two where
  their labels were alike: the one written last is exported, and the other
  is left out and logged as a warning, `OTLP metric name written for two
  Prometheus metric names under one resource`, as a name of two kinds is.
- The OTLP log export sends each attribute key of a record once, the value
  given last, where `logger.With("a", 1)` and a line's own `a` gave two
  attributes `a`; and a value whose `Error` or `String` panics, as a nil
  error's may, is `<nil>` or `!PANIC: …`, as on stderr, where it took the
  exporter down.

## chart 2.0.0

### What changed for you

- Deploys exporter 2.0.0, a major release: read its
  `### What changed for you` before upgrading, since configuration that
  1.8.0 took may now be refused when it loads, such as an upper-case
  `request.type` or a quoted `accept_status` entry like `"+503"`.

### New

- The chart's `healthProbes.mode: exec` runs the liveness and readiness
  probes as the exporter's `--healthcheck` in the container instead of HTTP
  GETs from the kubelet.
- The chart's `probes` renders a Prometheus Operator `Probe` for each entry,
  as blackbox_exporter's uptime checks are declared: its prober the
  exporter's Service at `/probe`, the entry's `collector` sent as the
  `collector` parameter, and its `targets`, a list of URLs, or the Ingresses
  its `ingress` selects, as the targets. Empty by default, so nothing is
  rendered and the Probe CRD is needed only with an entry.
- `server.pythonMaxIdleWorkers` and `server.pythonIdleTimeout` set the
  exporter's `--python.max-idle-workers` and `--python.idle-timeout`,
  `server.debugDangerous` its `--web.debug.dangerous`, and
  `server.statusRefresh` its `--web.status-refresh`; each is left out when
  empty or false, so the exporter's default applies.

## exporter 1.8.0

### What changed for you

- Answers of `/probe`, the self-metrics and the static targets are gzipped at
  level 1 instead of 6: a few percent bigger and about a third faster to make.
  `--web.gzip-level` sets the level, 0 to answer uncompressed.
- A failed probe's answer no longer quotes what the target answered: a value
  a decoder could not read, a database's message (the caller is told its error
  code), an identity provider's refusal and an `sshexec` command's stderr are
  replaced by `[withheld; in the exporter's log]`. The log and the debug report
  keep the whole text. A probe that panicked is answered a fixed text.
- The status page shows a probe's or a static target's error as the probe's
  caller is told it; the whole text only with `--web.debug=full`.
- A `vertica` statement the driver would not prepare is refused, and the SQL
  page says a Vertica server can read the exporter's files through its driver:
  hold `vertica` collectors to trusted servers with `allowed_targets`.

### New

- A top-level `defaults` block, in the configuration file and in each collector
  file, gives that file's collectors every key they do not write themselves;
  `failure_backoff: {enabled: false}` turns off a backoff the defaults give.
- `rate_limits`: limits per host, shared by every `http` and `graphite`
  collector.
- `request.proxy_url` and `request.proxy_basic_auth_file`: a proxy of the
  collector's own for `http` and `graphite`, or `direct`.
- Static targets may carry their own credentials and `tls` block (`redis`,
  `ldap`, `tcp`, `sql`, `grpc`, `http`, `graphite`).
- A status page for the static targets at `/static-targets/status`
  (`?format=json` for scripts), with the recent probes and the OTLP export.
- `derived`: sum, difference and ratio series computed from a collector's own
  series, under every transform.
- Metric lint: advice about the series a collector makes, in `--dry-run` and
  every debug run.
- The `ntp` request type, in the default build, and the opt-in `sshexec`
  request type, which runs a fixed command on a host over SSH.
- `tcp`: `request.greeting` for services that speak first, `request.starttls`
  (smtp, imap, pop3, ftp), `request.document` for a JSON document of the
  exchange, and `X-Tcp-*` timing headers on every answer.
- `tlscert`: `request.starttls` reads a mail or FTP server's certificate.
- Self-metrics `http_exporter_answer_bytes_total` and
  `http_exporter_answer_sent_bytes_total`, by endpoint, before and after
  compression.

### Fixed

- A static target removed at a reload is forgotten then, so one brought back
  starts as not yet scraped; a scrape skipped by `failure_backoff` no longer
  shows as a failed one on the status page.
- `tcp`: a race between the TLS handshake and the deadline, and a `send` its
  placeholders leave empty, now refused with 400.

## chart 1.8.0

### What changed for you

- Deploys exporter 1.8.0.
- Rendering fails when `ingress.enabled`, or a `service.type` of `NodePort` or
  `LoadBalancer`, exposes the exporter without `webAuth`, or with a rendered
  configuration whose `web.basic_auth` does not read the mounted password.
  Enable `webAuth`, or set `exposure.allowWithoutWebAuth: true` when callers
  authenticate elsewhere.

### New

- `server.gzipLevel` sets `--web.gzip-level`.
