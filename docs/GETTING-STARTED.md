# Getting started

This page takes you from nothing to Prometheus scraping your first metric,
in about 15 minutes. It uses one public JSON API, the weather from
[Open-Meteo](https://open-meteo.com), which needs no account or key. When
you are done, [How do I…](HOW-DO-I.md) lists what to read next for the task
you have in mind.

## 1. Run the exporter

Pick one:

```sh
# With Docker. The configuration is the file you write in step 2.
docker run -p 8080:8080 \
  -v "$PWD/config.yaml:/etc/prometheus-universal-exporter/config.yaml:ro" \
  ghcr.io/eenchev/prometheus-universal-exporter:latest

# Or from a checkout, with Go installed.
go run . --config.file=config.yaml
```

The exporter listens on port 8080. It reads its configuration once at
start; restart it after you change the file, or see
[Watching the configuration](CONFIGURATION.md#watching-the-configuration) to make it pick changes up by
itself.

## 2. Describe what to read: a collector

A *collector* says how to ask a source for data and how to turn the answer
into metrics. It never names the source itself: the address comes with each
scrape, so one collector serves every source that answers the same way.

Open-Meteo answers a request for the current weather with JSON like this:

```json
{"latitude": 42.7, "longitude": 23.32, "current": {"time": "2026-11-09T14:00", "temperature_2m": 12.4, "wind_speed_10m": 3.1}}
```

Save this as `config.yaml`:

```yaml
collectors:
  - name: weather
    request:
      type: http                  # ask over HTTP
      path: /v1/forecast          # joined onto the target's address
      query:
        latitude: "42.6977"
        longitude: "23.3219"
        current: temperature_2m,wind_speed_10m
        wind_speed_unit: ms
    decoder:
      type: json                  # the answer is JSON
    transform:
      type: jq                    # read values with jq expressions
    metrics:
      - name: weather_temperature_celsius
        description: Air temperature 2 m above ground.
        expression: .current.temperature_2m
      - name: weather_wind_speed_meters_per_second
        description: Wind speed 10 m above ground.
        expression: .current.wind_speed_10m
```

Each entry under `metrics` is one metric: its name, and a jq expression
that picks its value out of the answer. If the file has a mistake, the
exporter refuses to start and says where: the line of an unknown key, or the
collector and metric of an expression that does not parse.

## 3. Probe it

Ask the exporter to read the API through the collector:

```sh
curl 'http://localhost:8080/probe?target=https://api.open-meteo.com&collector=weather'
```

The answer is what Prometheus scrapes:

```text
# HELP weather_temperature_celsius Air temperature 2 m above ground.
# TYPE weather_temperature_celsius gauge
weather_temperature_celsius 12.4
# HELP weather_wind_speed_meters_per_second Wind speed 10 m above ground.
# TYPE weather_wind_speed_meters_per_second gauge
weather_wind_speed_meters_per_second 3.1
```

`target` is the source's address and `collector` the collector's name. A
probe that fails, such as one whose target cannot be reached, answers with an
HTTP error status and a line saying what failed. A metric whose expression
finds no value is only left out, and the rest are served; to fail the probe
instead, see [When a metric cannot be extracted](CONFIGURATION.md#when-a-metric-cannot-be-extracted).

To try expressions without restarting, start the exporter with
`--web.debug=edit` and open <http://localhost:8080/debug>: paste an answer,
such as the JSON above, and run the collector on it, or one you edit in the
page, with each stage shown. `--web.debug=full` lets the page fetch the
answer from a target as well. See
[Debugging collectors](CONFIGURATION.md#debugging-collectors).

## 4. Point Prometheus at it

Prometheus scrapes the exporter's `/probe` endpoint and passes the source as
`target`. This is the same *probe pattern* blackbox_exporter uses:

```yaml
scrape_configs:
  - job_name: weather
    metrics_path: /probe
    params:
      collector: [weather]
    static_configs:
      - targets:
          - https://api.open-meteo.com
    relabel_configs:
      - source_labels: [__address__]
        target_label: __param_target   # the source becomes the target parameter
      - source_labels: [__param_target]
        target_label: instance         # and the instance label
      - target_label: __address__
        replacement: localhost:8080    # Prometheus itself talks to the exporter
```

On Kubernetes, the Helm chart writes this for you as a `ServiceMonitor` or a
`PodMonitor`: see [Install with Helm](../README.md#install-with-helm) and
[Prometheus Operator](PROMETHEUS-OPERATOR.md).

## 5. Or let the exporter scrape on its own

For a fixed list of sources, you can skip the relabeling: list them as
*static targets*, which the exporter scrapes on its own schedule and serves
together on `/static-targets`. Save this as `static-targets.yaml`:

```yaml
interval: 5m
targets:
  - name: sofia
    collector: weather
    target: https://api.open-meteo.com
    labels:
      city: Sofia
```

Start the exporter with `--static-targets-file=static-targets.yaml` (with
Docker, mount the file as you did `config.yaml`) and
scrape `http://localhost:8080/static-targets` like any other exporter. The
page <http://localhost:8080/static-targets/status> shows each target's last
scrape. See [Static targets](STATIC-TARGETS.md).

## 6. Where to go from here

- One collector for many places: put `{{param_latitude}}` in the query and
  pass `param_latitude` with each probe; see
  [Path parameters](REQUESTS.md#path-parameters). The complete Open-Meteo
  example, with every weather value, caching and labels, is in
  [`examples/open-meteo`](../examples/open-meteo/config.yaml).
- A list in the answer, one series per item: [Metrics per item](CONFIGURATION.md#metrics-per-item).
- A source that is not JSON over HTTP: [Which request type?](CONFIGURATION.md#which-request-type).
- A source that wants a login: [Logging in to a target](AUTHENTICATION.md#logging-in-to-a-target).
- Everything else, by task: [How do I…](HOW-DO-I.md).
