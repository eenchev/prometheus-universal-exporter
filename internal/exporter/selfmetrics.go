package exporter

import (
	"net/http"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// statsValues are the exporter's own counters for one collector, or for one
// request when verbose self-metrics are configured. They are separated from the
// mutex so a consistent copy can be taken under the lock and rendered outside
// it.
type statsValues struct {
	probes, success, decodeOK, parseErrors, transformErrors, missing, scriptErrors, limitErrors, emitted, cacheHits, cacheMisses uint64
	// coalesced counts probes answered by sharing another probe's request.
	coalesced uint64
	// rejected counts probes turned away by max_concurrent_probes, and
	// rejectedByExporter those turned away by --probe.max-concurrent.
	rejected, rejectedByExporter uint64
	// refused counts trips request.allowed_targets or denied_targets
	// refused.
	refused uint64
	// staleServed counts failed trips answered with the last good result
	// (cache.stale_if_error).
	staleServed uint64
	// invalidUTF8 counts label values and help texts whose invalid UTF-8
	// was replaced.
	invalidUTF8 uint64
	// seriesLeftOut and linesSkipped count what the graphite decoder left
	// out before the rules saw it (graphitereport.go).
	seriesLeftOut, linesSkipped uint64
	// lastScriptDuration is how long the Python of the last probe that ran any
	// took, in seconds.
	lastScriptDuration float64
	lastStatus         int
	// grpcCode is the gRPC status code of the last call of a grpc
	// collector, -1 when it made none; grpcCalled says there was a scrape
	// at all, before which the code is -1 too.
	grpcCode     int
	grpcCalled   bool
	lastBytes    int64
	lastDuration float64
	// lastScrape is only kept per request; the per-collector series has no
	// timestamp of its own.
	lastScrape time.Time
}

type serverStats struct {
	mu sync.Mutex
	statsValues
	// ruleFailures counts, per metric name, the series the collector's rules
	// could not produce and carried on without, under error_mode log or
	// ignore. Only the per-collector statistics keep it.
	ruleFailures map[string]uint64
}

// ruleFailureCount returns how many series of metric have failed.
func (s *serverStats) ruleFailureCount(metric string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ruleFailures[metric]
}

func (s *serverStats) snapshot() statsValues {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statsValues
}

// selfMetricDescriptors are the exporter's per-collector metric families, in
// the order they are exposed: each one's name, type and help text, and how its
// value is read from a collector's counters. They are the single source of all
// three, for the text exposition and for OTLP alike (selfMetricSet), so a
// family cannot be declared a gauge in one and a counter in the other. A
// shared placeholder help would make the endpoint self-documenting in name
// only: HELP is what a reader sees in Grafana's metric browser or `curl`.
var selfMetricDescriptors = []selfMetricDescriptor{
	{"http_exporter_scrapes_total", model.CounterMetricType, "Probes served for this collector, including those answered from the response cache.", func(v statsValues) float64 { return float64(v.probes) }},
	{"http_exporter_scrape_success_total", model.CounterMetricType, "Probes for this collector that completed without a fatal error.", func(v statsValues) float64 { return float64(v.success) }},
	{"http_exporter_scrape_duration_seconds", model.GaugeMetricType, "Duration of the most recent probe of this collector, in seconds.", func(v statsValues) float64 { return v.lastDuration }},
	{"http_exporter_scrape_http_status_code", model.GaugeMetricType, "HTTP status the target returned on the most recent scrape, or 0 when the request failed before a response arrived.", func(v statsValues) float64 { return float64(v.lastStatus) }},
	{"http_exporter_scrape_grpc_status_code", model.GaugeMetricType, "gRPC status code of the most recent call of this grpc collector: 0 for OK, 14 for UNAVAILABLE; -1 before the first call, or when a scrape made none, as when the message did not encode.", func(v statsValues) float64 {
		if !v.grpcCalled {
			return -1
		}
		return float64(v.grpcCode)
	}},
	{"http_exporter_scrape_response_bytes", model.GaugeMetricType, "Size of the most recent response body for this collector, in bytes.", func(v statsValues) float64 { return float64(v.lastBytes) }},
	{"http_exporter_decode_success_total", model.CounterMetricType, "Responses this collector decoded into its configured format.", func(v statsValues) float64 { return float64(v.decodeOK) }},
	{"http_exporter_parse_errors_total", model.CounterMetricType, "Responses this collector's decoder could not parse.", func(v statsValues) float64 { return float64(v.parseErrors) }},
	{"http_exporter_transform_errors_total", model.CounterMetricType, "Transforms that failed for this collector.", func(v statsValues) float64 { return float64(v.transformErrors) }},
	{"http_exporter_missing_keys_total", model.CounterMetricType, "Values the response did not contain: a failed transform's, and each series a metric rule carried on without.", func(v statsValues) float64 { return float64(v.missing) }},
	{"http_exporter_script_errors_total", model.CounterMetricType, "Python script failures during this collector's transform.", func(v statsValues) float64 { return float64(v.scriptErrors) }},
	{"http_exporter_script_duration_seconds", model.GaugeMetricType, "Duration of the most recent Python script run for this collector, in seconds.", func(v statsValues) float64 { return v.lastScriptDuration }},
	{"http_exporter_metrics_emitted_total", model.CounterMetricType, "Metrics this collector has produced across its scrapes.", func(v statsValues) float64 { return float64(v.emitted) }},
	{"http_exporter_invalid_utf8_total", model.CounterMetricType, "Label values and help texts this collector produced that were not valid UTF-8, whose invalid bytes were replaced with U+FFFD.", func(v statsValues) float64 { return float64(v.invalidUTF8) }},
	{"http_exporter_decoder_series_left_out_total", model.CounterMetricType, "Series the graphite decoder left out before the metric rules saw them: with no point that has a value, with the newest point older than response.graphite.max_age, or answered twice.", func(v statsValues) float64 { return float64(v.seriesLeftOut) }},
	{"http_exporter_decoder_lines_skipped_total", model.CounterMetricType, "Carbon lines the graphite decoder could not read and skipped, under response.graphite.invalid_lines: skip.", func(v statsValues) float64 { return float64(v.linesSkipped) }},
	{"http_exporter_series_limit_exceeded_total", model.CounterMetricType, "Scrapes rejected for exceeding this collector's response size or series limits.", func(v statsValues) float64 { return float64(v.limitErrors) }},
	{"http_exporter_cache_hits_total", model.CounterMetricType, "Probes answered from this collector's response cache.", func(v statsValues) float64 { return float64(v.cacheHits) }},
	{"http_exporter_cache_misses_total", model.CounterMetricType, "Probes that found no usable cache entry and went to the target; a probe that shared another's trip is counted in http_exporter_probes_coalesced_total instead.", func(v statsValues) float64 { return float64(v.cacheMisses) }},
	{"http_exporter_cache_stale_served_total", model.CounterMetricType, "Probes and static target scrapes whose trip to the target failed and that were answered with the last successful result instead, under cache.stale_if_error.", func(v statsValues) float64 { return float64(v.staleServed) }},
	// What a collector's cache holds belongs to the collector, not to any one
	// request, so it has no per-request value.
	{"http_exporter_cache_entries", model.GaugeMetricType, "Entries currently held in this collector's response cache, including stale ones kept for cache.stale_if_error.", nil},
	{"http_exporter_probes_in_flight", model.GaugeMetricType, "Trips to this collector's targets in progress, which max_concurrent_probes bounds.", nil},
	{"http_exporter_probes_rejected_total", model.CounterMetricType, "Probes answered 503, and static target scrapes that found no slot in time, because this collector already had max_concurrent_probes trips to its targets in progress.", func(v statsValues) float64 { return float64(v.rejected) }},
	{"http_exporter_probes_rejected_exporter_limit_total", model.CounterMetricType, "Probes of this collector answered 503, and static target scrapes that found no slot in time, because the exporter already had --probe.max-concurrent trips to targets in progress.", func(v statsValues) float64 { return float64(v.rejectedByExporter) }},
	{"http_exporter_targets_refused_total", model.CounterMetricType, "Probes and static target scrapes whose target, or a redirect's, this collector's request.allowed_targets or denied_targets refused; a probe is answered 403.", func(v statsValues) float64 { return float64(v.refused) }},
	{"http_exporter_probes_coalesced_total", model.CounterMetricType, "Probes answered by sharing an identical probe already in flight instead of going to the target.", func(v statsValues) float64 { return float64(v.coalesced) }},
}

// requestTypeFamilies are the families only the collectors of some request
// types have, by those types: a collector of another type has no series in
// them, so an exporter without such collectors, or built without the types,
// does not show the family at all.
var requestTypeFamilies = map[string][]string{
	"http_exporter_scrape_grpc_status_code": {"grpc"},
	// A localfile collector reads files, and has no target to refuse.
	"http_exporter_targets_refused_total": {"http", "graphite", "grpc"},
}

// selfMetricDescriptor describes one per-collector self-metric family. Value
// is nil for a family that belongs to the collector rather than to a request.
type selfMetricDescriptor struct {
	Name  string
	Type  model.MetricType
	Help  string
	Value func(statsValues) float64
}

// exporterMetricHelp describes the exporter-wide families, which carry no
// collector's counters.
var exporterMetricHelp = map[string]string{
	"http_exporter_build_info":                                 "1, with the exporter's version, revision, Go version and built request types as labels.",
	"http_exporter_collector_config_valid":                     "Whether the collector configuration is valid.",
	"http_exporter_rule_failures_total":                        "Series a metric rule could not produce and the probe carried on without, under error_mode log or ignore.",
	"http_exporter_trips_in_flight":                            "Trips to targets in progress, probes and static target scrapes of every collector together, which --probe.max-concurrent bounds.",
	"http_exporter_trips_max_concurrent":                       "--probe.max-concurrent: how many trips to targets may be in progress at once; 0 for no limit across collectors.",
	"http_exporter_static_targets":                             "Static targets configured in the static target file, served on the static targets endpoint.",
	"http_exporter_static_targets_exported_via_otlp":           "Static targets with export_via_otlp, also delivered over OTLP.",
	"http_exporter_otlp_exports_total":                         "OTLP exports, each a delivery of everything pending with its retries, by result: success or failure.",
	"http_exporter_otlp_export_retries_total":                  "OTLP export attempts repeated after a network error, 429, 502, 503 or 504.",
	"http_exporter_otlp_points_dropped_total":                  "Data points given up on: refused by the OTLP endpoint with a status that is not retried, or the oldest waiting past otlp.max_pending_points.",
	"http_exporter_otlp_export_duration_seconds":               "Duration of the most recent OTLP export, its retries included.",
	"http_exporter_otlp_last_export_success_timestamp_seconds": "Unix time of the last OTLP export that got through; 0 before the first.",
}

// selfMetricHelp indexes the help of every family the self-metrics carry
// outside the verbose and runtime sets.
var selfMetricHelp = func() map[string]string {
	out := make(map[string]string, len(selfMetricDescriptors)+len(exporterMetricHelp)+len(config.ReloadMetricHelp))
	for _, d := range selfMetricDescriptors {
		out[d.Name] = d.Help
	}
	for name, help := range exporterMetricHelp {
		out[name] = help
	}
	for name, help := range config.ReloadMetricHelp {
		out[name] = help
	}
	return out
}()

// selfMetricNames lists the per-collector families in exposition order.
func selfMetricNames() []string {
	names := make([]string, 0, len(selfMetricDescriptors))
	for _, d := range selfMetricDescriptors {
		names = append(names, d.Name)
	}
	return names
}

// metricsHandler serves the self-metrics. The text is rendered from the same
// set OTLP exports, by the same code as collector output, so the two cannot
// disagree about a family's type or help, and every family is one contiguous
// block with one HELP and one TYPE line.
func (s *Server) metricsHandler(w http.ResponseWriter, r *http.Request) {
	set := s.selfMetricSet()
	writeMetricSet(w, r, &set)
}

// collectorStats returns the counters of every configured collector, sorted
// by name. A collector a reload removed is not among them: its series stop,
// and Prometheus marks them stale (reconcile.go).
func (s *Server) collectorStats() (names []string, values map[string]statsValues) {
	s.reconcile()
	s.statsMu.Lock()
	stats := make(map[string]*serverStats)
	for _, c := range s.manager.Get().Collectors {
		if s.stats[c.Name] == nil {
			s.stats[c.Name] = &serverStats{}
		}
		stats[c.Name] = s.stats[c.Name]
		names = append(names, c.Name)
	}
	s.statsMu.Unlock()
	sort.Strings(names)
	values = make(map[string]statsValues, len(names))
	for _, name := range names {
		values[name] = stats[name].snapshot()
	}
	return names, values
}

// selfMetricSet is every self-metric, grouped by family: each per-collector
// family with the collectors' series and then, in verbose mode, the
// per-request ones; then the exporter-wide families; then the verbose-only and
// runtime families.
func (s *Server) selfMetricSet() model.MetricSet {
	names, values := s.collectorStats()
	cacheEntries := s.cache.Stats(time.Now())
	requests, requestFamilies := s.verboseRequests()
	// The families that belong to a collector rather than to a request.
	collectorOnly := map[string]func(string) float64{
		"http_exporter_cache_entries":    func(name string) float64 { return float64(cacheEntries[name]) },
		"http_exporter_probes_in_flight": func(name string) float64 { return float64(s.trips.count(name)) },
	}
	requestTypes := map[string]string{}
	for _, c := range s.manager.Get().Collectors {
		requestTypes[c.Name] = c.Request.Type
	}
	var out []model.Metric
	for _, d := range selfMetricDescriptors {
		only, typed := requestTypeFamilies[d.Name]
		for _, name := range names {
			if typed && !slices.Contains(only, requestTypes[name]) {
				continue
			}
			var value float64
			if d.Value != nil {
				value = d.Value(values[name])
			} else {
				value = collectorOnly[d.Name](name)
			}
			out = append(out, model.Metric{Name: d.Name, Help: d.Help, Type: d.Type, Value: value, Labels: map[string]string{"collector": name}})
		}
		if d.Value == nil {
			continue
		}
		for _, sample := range requests {
			if typed && !slices.Contains(only, requestTypes[sample.Key.Collector]) {
				continue
			}
			out = append(out, model.Metric{Name: d.Name, Help: d.Help, Type: d.Type, Value: d.Value(sample.Values), Labels: requestLabels(sample.Key)})
		}
	}
	out = append(out, buildInfoMetric())
	for _, c := range s.manager.Get().Collectors {
		out = append(out, model.Metric{Name: "http_exporter_collector_config_valid", Help: exporterMetricHelp["http_exporter_collector_config_valid"], Type: model.GaugeMetricType, Value: 1, Labels: map[string]string{"collector": c.Name}})
	}
	out = append(out, s.ruleFailureMetrics()...)
	out = append(out, s.staticTargetCountMetrics()...)
	out = append(out, s.tripTotalMetrics()...)
	out = append(out, s.manager.ReloadMetrics()...)
	out = append(out, s.otlpStatusMetrics()...)
	out = append(out, requestFamilies...)
	out = append(out, s.verboseCollectorMetrics()...)
	out = append(out, s.runtimeMetrics()...)
	return model.MetricSet{Metrics: out}
}

// recordScriptDuration keeps the duration of a probe's Python, when it ran
// any, on the collector and the request.
func recordScriptDuration(rec statsRecorder, timer *transform.ScriptTimer) {
	if seconds, ran := timer.Seconds(); ran {
		rec.update(func(x *serverStats) { x.lastScriptDuration = seconds })
	}
}

// ruleFailureMetrics is http_exporter_rule_failures_total: a series for every
// metric rule of every collector, from zero, so a rate works from the start
// and a rule that never fails still has its series. Two rules exporting one
// metric name share it.
func (s *Server) ruleFailureMetrics() []model.Metric {
	var out []model.Metric
	for _, c := range s.manager.Get().Collectors {
		stats := s.statsFor(c.Name)
		seen := map[string]bool{}
		for _, rule := range c.Metrics {
			if rule.Name == "" || seen[rule.Name] {
				continue
			}
			seen[rule.Name] = true
			out = append(out, model.Metric{
				Name: "http_exporter_rule_failures_total", Help: exporterMetricHelp["http_exporter_rule_failures_total"], Type: model.CounterMetricType,
				Value: float64(stats.ruleFailureCount(rule.Name)), Labels: map[string]string{"collector": c.Name, "metric": rule.Name},
			})
		}
	}
	return out
}
