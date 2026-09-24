// Package exporter serves the exporter's HTTP endpoints — /probe, the
// self-metrics, /health, /ready and /-/reload — and runs the probe pipeline
// behind them: fetch, decode, transform and validate, with the response cache,
// shared in-flight probes and concurrency limits. It also scrapes the static
// targets and exports to OTLP.
package exporter
