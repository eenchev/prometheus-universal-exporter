# Endpoints

The exporter serves its endpoints over plain HTTP on `--web.listen-address`
(`:8080` by default). Which of them are served, and at which path, is set by
the [command-line flags](FLAGS.md).

| Path | Purpose |
| --- | --- |
| `/` | A landing page: the build, links to the endpoints, and when the last reload put something in force and [what it changed](CONFIGURATION.md#what-a-reload-changed). |
| `/collectors` | Each collector with a form that probes a target through it, taking its request parameters, forwarded headers and, when it forwards `Authorization`, a target credential; with `--web.debug`, a link into the debug page. See [Probing from the browser](AUTHENTICATION.md#probing-from-the-browser). |
| `/debug` | With `--web.debug`: run a collector, written there or configured, and see each stage of its trip and the series it gives, on a pasted response or, at `full`, a fetched target. See [Debugging collectors](CONFIGURATION.md#debugging-collectors). |
| `/debug/run` | With `--web.debug`: where the debug page posts each run, `POST` with a JSON body and `X-Debug-Run`, answered with the run's stages, series and report as JSON; any other method is answered `405`. See [Debugging collectors](CONFIGURATION.md#debugging-collectors). |
| `/probe` | Scrape a target through a collector. Takes `target` and `collector`, with `GET` or `HEAD`; any other method is answered `405`. With `--web.debug=full`, `&debug=true` answers with a report of the trip instead; see [Debug probes](CONFIGURATION.md#debug-probes). |
| `/self-metrics` | The exporter's own metrics, at `--web.self-metrics-path`, with `GET` or `HEAD`; any other method is answered `405`. See [Self-metrics](SELF-METRICS.md). |
| `/static-targets` | The latest results of the [static targets](STATIC-TARGETS.md), at `--web.static-targets-path`. `?targets=a,b` serves only the targets named; with `--web.debug=full`, `?debug=<name>` [reports one scrape](STATIC-TARGETS.md#debugging-a-static-target). |
| `/-/reload` | `POST` or `PUT` reloads the configuration, with `--web.enable-lifecycle`, as either does Prometheus's own; any other method is answered `405`. See [Reloading on demand](CONFIGURATION.md#reloading-on-demand). |
| `/health`, `/ready` | Kubernetes probes. `/ready` is `503` while OTLP exports keep failing (when `otlp.unready_after_failures` asks for it), the exporter is shutting down, or in allocator mode before the first bundle, not after a rejected reload; see [Readiness](CONFIGURATION.md#readiness). Never authenticated. |

`/probe`, the self-metrics path and the static targets path answer gzip-compressed when the client accepts it, as Prometheus does on every scrape.

Every endpoint but `/health` and `/ready` answers `403` to a request the
browser marks as made by another site's page, `Sec-Fetch-Site: cross-site`
or `same-site`, with Basic Auth on or off: a browser sends the exporter's
credential with such requests too. The exporter's own pages, an address typed
in, and clients that are not browsers, Prometheus among them, pass. See
[Requests from other sites](AUTHENTICATION.md#requests-from-other-sites).
