# Command-line flags

These are the exporter's flags; `--help` prints the same list with their
defaults. The target allocator, `pue-allocator`, has flags of its own, listed
in [Running it](ALLOCATOR.md#running-it) on its page.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--config.file` | `/etc/prometheus-universal-exporter/config.yaml` | The configuration document. |
| `--web.listen-address` | `:8080` | Address the HTTP endpoints listen on. |
| `--web.self-metrics-path` | `/self-metrics` | Path of the exporter's own metrics, served there and nowhere else; `/metrics` for the conventional path. A path another endpoint uses is refused. |
| `--web.static-targets-path` | `/static-targets` | Path the static targets' latest results are served at, for Prometheus to scrape; their [status page](STATIC-TARGETS.md#status-page) is served at this path followed by `/status`. A path another endpoint uses is refused, and so is a `--web.self-metrics-path` on the status page's. |
| `--python.path` | `python3` | Interpreter used by the `python` transform. |
| `--log.level` | `info` | `debug`, `info`, `warn` or `error`, in any case; anything else is refused. |
| `--config.watch` | off | Re-read the configuration when it changes on disk, or a [descriptor file](CONFIGURATION.md#descriptor-files) of a grpc collector does. |
| `--config.watch-interval` | `60s` | How often to check, with `--config.watch`. |
| `--config.expand-env` | off | Expand `${NAME}` references in the configuration and its collector files. |
| `--static-targets.expand-env` | off | Expand `${NAME}` references in the static target file. See [Static targets](STATIC-TARGETS.md#environment-variables). |
| `--static-targets-file` | none | Static targets the exporter scrapes itself, on their own intervals. See [Static targets](STATIC-TARGETS.md). |
| `--config.schema` | off | Print the JSON Schema of the configuration file, for editors, and exit. See [Editor support](CONFIGURATION.md#editor-support). |
| `--config.collector-file-schema` | off | Print the JSON Schema of a collector file, for editors, and exit. See [Collector files](CONFIGURATION.md#collector-files). |
| `--static-targets-file-schema` | off | Print the JSON Schema of the static target file, for editors, and exit. See [Static targets](STATIC-TARGETS.md). |
| `--probe.timeout-offset` | `500ms` | How much of Prometheus's scrape timeout a probe leaves unused, so it answers with its own error first. See [Probe deadlines](CONFIGURATION.md#probe-deadlines). |
| `--probe.default-timeout` | `30s` | How long a probe may take without an `X-Prometheus-Scrape-Timeout-Seconds` header, as from curl or a script; a `timeout` parameter bounds the request within it and cannot lift it. `0` leaves such a probe unbounded. See [Probe deadlines](CONFIGURATION.md#probe-deadlines). |
| `--probe.max-concurrent` | `0` | How many trips to targets, probes and static target scrapes of every collector together, may be in progress at once, on top of each collector's `max_concurrent_probes`. `0` leaves only the per-collector limits. See [Limiting concurrent probes](CONFIGURATION.md#limiting-concurrent-probes). |
| `--python.max-workers` | `0` | How many Python workers, of every collector together, may be alive at once. `0` leaves them bounded only per script. See [How scripts run](PYTHON.md#how-scripts-run). |
| `--runtime.memory-limit-ratio` | `0` | Set the Go memory limit to this share of the container's memory limit, read from its cgroup at startup, leaving the rest to the Python workers; 0, off, or from 0.1 to 1, since below 0.1 the Go runtime would spend its time collecting garbage. `GOMEMLIMIT` in the environment wins. The Helm chart sets `0.8`. |
| `--web.shutdown-delay` | `0s` | How long a shutdown keeps serving, with `/ready` answering `503`, before it begins, so a load balancer stops sending probes first. The Helm chart sets `5s`. See [Shutting down](CONFIGURATION.md#shutting-down). |
| `--web.shutdown-timeout` | `15s` | How long a shutdown waits for the probes in progress. Keep it at least as long as Prometheus's scrape timeout. See [Shutting down](CONFIGURATION.md#shutting-down). |
| `--web.enable-lifecycle` | off | Enable `/-/reload`, which, on `POST` or `PUT`, reloads the configuration and reports whether it was accepted. `SIGHUP` reloads either way. See [Reloading on demand](CONFIGURATION.md#reloading-on-demand). |
| `--web.debug` | `off` | Debugging collectors: `edit` serves `/debug`, where a collector is run on a pasted response, nothing fetched; `full` adds fetching a target there, `/probe?...&debug=true` and `/static-targets?debug=<name>`, which show what the target answered. See [Debugging collectors](CONFIGURATION.md#debugging-collectors). |
| `--version` | off | Print the version, git revision, Go version, request types and sql drivers of the build (no sql drivers of a build without the `sql` type), and exit. The same is in the `http_exporter_build_info` self-metric. |
| `--allocator.url` | none | Take the configuration and this exporter's share of the static targets from the target allocator at this `http://` URL. Cannot be given with `--config.file` or `--static-targets-file`. See [Configuration from the target allocator](CONFIGURATION.md#configuration-from-the-target-allocator). |
| `--allocator.cache-dir` | none | Directory the bundle from the allocator is kept in and loaded from, and a restart resumes from; required with `--allocator.url`. |
| `--allocator.member` | the host name | This exporter's name in the allocator's member list. |
| `--allocator.advertise` | first address, listen port | `host:port` Prometheus is sent to for this exporter's probes: the listen address's host, or else the host's first address that is not a loopback one, with the listen port. |
| `--allocator.token-file` | none | File holding the bearer token sent to the allocator, read again for every request. |
| `--dry-run` | off | Validate the files and flags above, print a JSON report and exit `0` or `1`, without starting. See [Dry run](CONFIGURATION.md#dry-run). |

A flag takes its value as `--name=value`. An argument that is not a flag is refused with exit status `2`, at a start and with `--dry-run` alike, naming the argument: after `--config.watch true`, every flag that follows `true` would otherwise be left unread without a word.
