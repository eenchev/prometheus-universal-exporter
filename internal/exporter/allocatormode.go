package exporter

import (
	"net/http"
	"sort"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// An exporter in allocator mode (--allocator.url) that starts without a
// bundle in its cache directory has no configuration until the allocator
// serves one. Until then its manager holds an empty configuration and
// reports that it is awaiting one (config.NewAwaitingManager):
//   - /health answers, and /ready answers 503;
//   - every other endpoint (/probe, the static targets, the self-metrics,
//     the landing and collectors pages, the debug page and /-/reload)
//     answers 503 with a sentence that says what it waits for.
//
// The bundle carries web.basic_auth, so until one is in force nothing says
// which requests to let in, and none is let in (basicAuthMiddleware). The
// self-metrics would show the allocator's sync errors while waiting, but the
// credential protects them; the exporter's log has those errors. Waiting
// ends with the first bundle put in force, whatever reload did it. A bundle
// in the cache directory that loads at start is in force from the start.

// SetAwaitingReason sets what the endpoints answer while the manager is
// awaiting its first configuration: a plain sentence naming what it waits
// for, such as the allocator's URL. /ready says only that there is no
// configuration yet: it is never authenticated.
func (s *Server) SetAwaitingReason(reason string) { s.awaitingReason = reason }

// awaitingConfiguration answers a request 503 while the manager is
// awaiting its first configuration, and reports whether it did.
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

// AllocatorStatus is how an exporter in allocator mode keeps up with the
// allocator, for the self-metrics. It holds when the exporter last heard
// which bundle is its own, its sync errors by stage (each stage listed from
// zero), and the bundles it refused. The generation in force is not part of
// it. That belongs to the manager (config.Manager.Generation) and is stored
// with the configuration in force. So the self-metrics never name a
// generation other than that of the configuration the probes are answered
// from. The client could, since it learns of it only after the reload has
// returned.
type AllocatorStatus struct {
	LastSync   time.Time
	SyncErrors map[string]uint64
	Refused    uint64
	// URL is the allocator's URL (--allocator.url) and Member is this
	// replica's member name, for the status page's link to the allocator's
	// view of the member. Generation is the bundle the client last saw put
	// in force, and InForceAt is when. The status page shows it beside the
	// manager's generation only when the two are the same bundle.
	URL, Member, Generation string
	InForceAt               time.Time
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
