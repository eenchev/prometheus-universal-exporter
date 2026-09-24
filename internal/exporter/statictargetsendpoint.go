package exporter

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The static targets endpoint, --web.static-targets-path, serves the latest
// result of every static target together, for Prometheus to scrape like any
// other exporter's metrics. Each target is scraped on its own interval
// (statictargetschedule.go), and a scrape of the endpoint only reads what the
// last scrape of each target left: the targets are never contacted on
// Prometheus's schedule, and a slow target never slows the endpoint.
//
// A target's result is its metrics, with the target's labels, or the last good
// result marked stale under cache.stale_if_error, and its health metrics,
// http_exporter_target_up, http_exporter_target_scrape_duration_seconds and
// http_exporter_target_last_success_timestamp_seconds.
// Every series carries static_target, the target's name, since the targets
// share one endpoint and the same metric from two targets must stay two
// series. A target that has not been scraped yet is absent, and one removed
// from the file is dropped with the reload.

// DefaultStaticTargetsPath is where the static targets are served unless
// --web.static-targets-path says otherwise.
const DefaultStaticTargetsPath = "/static-targets"

// StaticTargetsPath checks --web.static-targets-path as SelfMetricsPath checks
// its path, and that it is not the self-metrics path, selfMetricsPath.
func StaticTargetsPath(path, selfMetricsPath string) (string, error) {
	path, err := endpointPath("--web.static-targets-path", path, "/static-targets")
	if err != nil {
		return "", err
	}
	if path == selfMetricsPath {
		return "", fmt.Errorf("--web.static-targets-path %q is also --web.self-metrics-path; the two endpoints need paths of their own", path)
	}
	return path, nil
}

// SetStaticTargetsPath serves the static targets at path, which
// StaticTargetsPath has checked.
func (s *Server) SetStaticTargetsPath(path string) { s.staticTargetsPath = path }

// staticTargetsEndpoint is the path the static targets are served at.
func (s *Server) staticTargetsEndpoint() string {
	if s.staticTargetsPath == "" {
		return DefaultStaticTargetsPath
	}
	return s.staticTargetsPath
}

// publishStaticTarget records set as target's latest result, for the endpoint,
// and queues it for OTLP when the target is exported that way.
func (s *Server) publishStaticTarget(target model.StaticTarget, identity otlpResourceIdentity, set model.MetricSet) {
	s.staticMu.Lock()
	if s.staticResults == nil {
		s.staticResults = map[string]model.MetricSet{}
	}
	s.staticResults[target.Name] = set
	s.staticMu.Unlock()
	if target.ExportViaOTLP {
		s.queueOTLPResource(set, identity)
	}
}

// staticTargetResults returns the latest result of each target in force, by
// name, and forgets the results of targets no longer in force.
func (s *Server) staticTargetResults() []namedSet {
	targets := s.manager.StaticTargets()
	current := make(map[string]bool, len(targets))
	var out []namedSet
	s.staticMu.Lock()
	for _, target := range targets {
		current[target.Name] = true
		if set, ok := s.staticResults[target.Name]; ok {
			out = append(out, namedSet{name: target.Name, set: set})
		}
	}
	for name := range s.staticResults {
		if !current[name] {
			delete(s.staticResults, name)
		}
	}
	for name := range s.staticLastSuccess {
		if !current[name] {
			delete(s.staticLastSuccess, name)
		}
	}
	s.staticMu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

type namedSet struct {
	name string
	set  model.MetricSet
}

// errFamilyTypeClash is a target's family whose type another target's family
// of the same name already has otherwise.
var errFamilyTypeClash = errors.New("another static target exports this metric with a different type")

func (s *Server) staticTargetsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "use GET or HEAD to read the static targets", http.StatusMethodNotAllowed)
		return
	}
	merged := s.mergeStaticTargets(s.staticTargetResults())
	writeMetricSet(w, &merged)
}

// mergeStaticTargets puts the targets' results into one exposition: every
// series labelled with its target, and each family's series together, as the
// text format requires, in the order the families first appear. A family one
// target exports with another type than an earlier target is left out for that
// target and logged, since one family cannot have two types; the rest of both
// targets is served.
func (s *Server) mergeStaticTargets(results []namedSet) model.MetricSet {
	type family struct {
		typ     model.MetricType
		metrics []model.Metric
	}
	families := map[string]*family{}
	var order []string
	for _, result := range results {
		for _, m := range result.set.Metrics {
			m.Labels = model.CloneLabels(m.Labels)
			if m.Labels == nil {
				m.Labels = map[string]string{}
			}
			m.Labels[config.StaticTargetLabel] = result.name
			f := families[m.Name]
			if f == nil {
				f = &family{typ: m.Type}
				families[m.Name] = f
				order = append(order, m.Name)
			}
			if f.typ != m.Type {
				s.failures.failed(s.logger, slog.LevelWarn, failureKey("", "static target "+result.name, "family "+m.Name),
					"static target metric left out of the static targets endpoint", "exposition", errFamilyTypeClash,
					"target", result.name, "metric", m.Name, "type", string(m.Type), "type_in_use", string(f.typ))
				continue
			}
			f.metrics = append(f.metrics, m)
		}
	}
	var out model.MetricSet
	for _, name := range order {
		out.Metrics = append(out.Metrics, families[name].metrics...)
	}
	return out
}

// staticTargetCountMetrics are the self-metrics counting the static targets:
// all of them, and those also exported over OTLP.
func (s *Server) staticTargetCountMetrics() []model.Metric {
	targets := s.manager.StaticTargets()
	viaOTLP := 0
	for _, target := range targets {
		if target.ExportViaOTLP {
			viaOTLP++
		}
	}
	return []model.Metric{
		{Name: "http_exporter_static_targets", Help: exporterMetricHelp["http_exporter_static_targets"], Type: model.GaugeMetricType, Value: float64(len(targets))},
		{Name: "http_exporter_static_targets_exported_via_otlp", Help: exporterMetricHelp["http_exporter_static_targets_exported_via_otlp"], Type: model.GaugeMetricType, Value: float64(viaOTLP)},
	}
}

// recordStaticTargetOutcome notes a scrape of the target named name, at now,
// and returns when it last succeeded: now for a success, else the earlier
// success, zero if there was none.
func (s *Server) recordStaticTargetOutcome(name string, ok bool, now time.Time) time.Time {
	s.staticMu.Lock()
	defer s.staticMu.Unlock()
	if s.staticLastSuccess == nil {
		s.staticLastSuccess = map[string]time.Time{}
	}
	if ok {
		s.staticLastSuccess[name] = now
	}
	return s.staticLastSuccess[name]
}

// unixSeconds is t as Unix seconds, 0 for the zero time, which would otherwise
// read as a moment in year 1.
func unixSeconds(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	return float64(t.UnixNano()) / 1e9
}
