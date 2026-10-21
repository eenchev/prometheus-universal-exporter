package exporter

import (
	"net/http"
	"sort"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// An exporter in allocator mode (--allocator.url) that starts without a
// bundle in its cache directory has no configuration until the allocator
// serves it one. Its manager then holds an empty one and says it is awaiting
// (config.NewAwaitingManager): the endpoints are served, /ready answers 503
// and /probe 503 with a sentence that says what it waits for, the static
// targets endpoint has no targets and the self-metrics are served. The first
// bundle put in force ends it, whatever reload put it in force.

// SetAwaitingReason sets what /probe answers while the manager is awaiting
// its first configuration: a plain sentence naming what it waits for, such
// as the allocator's URL. /ready says only that there is no configuration
// yet: it is never authenticated.
func (s *Server) SetAwaitingReason(reason string) { s.awaitingReason = reason }

// awaitingConfiguration answers a probe 503 while the manager is awaiting
// its first configuration, and reports whether it did.
func (s *Server) awaitingConfiguration(w http.ResponseWriter) bool {
	if !s.manager.Awaiting() {
		return false
	}
	reason := s.awaitingReason
	if reason == "" {
		reason = awaitingReadyReason
	}
	http.Error(w, reason, http.StatusServiceUnavailable)
	return true
}

// awaitingReadyReason is what /ready says while the manager is awaiting its
// first configuration.
const awaitingReadyReason = "no configuration yet: waiting for the allocator"

// AllocatorStatus is how an exporter in allocator mode is keeping up with
// the allocator, for the self-metrics: when it last heard which bundle is
// its own; its sync errors by stage, each stage listed from zero; and the
// bundles it refused. The generation in force is not part of it: it is the
// manager's (config.Manager.Generation), stored with the configuration in
// force, so the self-metrics never name a generation other than that of the
// configuration the probes are answered from, as the client, which learns
// of it once the reload has returned, could.
type AllocatorStatus struct {
	LastSync   time.Time
	SyncErrors map[string]uint64
	Refused    uint64
}

// SetAllocatorStatus makes status the source of the allocator self-metrics,
// which an exporter outside allocator mode does not have.
func (s *Server) SetAllocatorStatus(status func() AllocatorStatus) { s.allocatorStatus = status }

// allocatorStatusMetrics renders the allocator self-metrics, none outside
// allocator mode.
func (s *Server) allocatorStatusMetrics() []model.Metric {
	if s.allocatorStatus == nil {
		return nil
	}
	st := s.allocatorStatus()
	generation := s.manager.Generation()
	metric := func(name string, typ model.MetricType, value float64, labels map[string]string) model.Metric {
		return model.Metric{Name: name, Help: exporterMetricHelp[name], Type: typ, Value: value, Labels: labels}
	}
	var out []model.Metric
	if generation != "" {
		out = append(out, metric("http_exporter_allocator_generation_info", model.GaugeMetricType, 1, map[string]string{"generation": generation}))
	}
	out = append(out, metric("http_exporter_allocator_last_sync_timestamp_seconds", model.GaugeMetricType, model.ScrapeTimestamp(st.LastSync), nil))
	stages := make([]string, 0, len(st.SyncErrors))
	for stage := range st.SyncErrors {
		stages = append(stages, stage)
	}
	sort.Strings(stages)
	for _, stage := range stages {
		out = append(out, metric("http_exporter_allocator_sync_errors_total", model.CounterMetricType, float64(st.SyncErrors[stage]), map[string]string{"stage": stage}))
	}
	return append(out, metric("http_exporter_allocator_bundle_refused_total", model.CounterMetricType, float64(st.Refused), nil))
}
