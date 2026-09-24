package exporter

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
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
// from the file is dropped with the reload. ?targets= narrows a read to the
// targets it names (requestedStaticTargets).

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
//
// Over OTLP every series carries static_target too, as on the endpoint. Two
// targets of one collector, without labels or an OTLP identity of their own,
// arrive under the same resource with the same series, and without it the
// later target's values would replace the earlier's in the pending export.
func (s *Server) publishStaticTarget(target model.StaticTarget, identity otlpResourceIdentity, set model.MetricSet) {
	s.publishStaticResult(target, identity, set, time.Time{})
}

// publishStaticResult is publishStaticTarget for a result whose data came
// from the target at fetched: its http_exporter_result_age_seconds is worked
// out again at every read of the endpoint, so it says how old the data is
// when Prometheus reads it, not how old it was when the scrape made it.
// fetched is zero for a result without that series.
func (s *Server) publishStaticResult(target model.StaticTarget, identity otlpResourceIdentity, set model.MetricSet, fetched time.Time) {
	// A reload may have removed the target while its scrape was in flight;
	// its result then goes nowhere, over OTLP included.
	if !s.staticTargetInForce(target.Name) {
		return
	}
	// Stored already labelled with its target, as the endpoint and OTLP
	// serve it, so a read of the endpoint only merges and writes, and the
	// labelling is done once per scrape rather than once per read. The
	// stored series are never changed after this: readers share them.
	labelled := withStaticTargetLabel(set, target.Name)
	s.staticMu.Lock()
	if s.staticResults == nil {
		s.staticResults = map[string]model.MetricSet{}
		s.staticFetched = map[string]time.Time{}
	}
	s.staticResults[target.Name] = labelled
	if fetched.IsZero() {
		delete(s.staticFetched, target.Name)
	} else {
		s.staticFetched[target.Name] = fetched
	}
	s.staticMu.Unlock()
	if target.ExportViaOTLP {
		s.queueOTLPResource(labelled, identity)
	}
}

// staticTargetInForce reports whether a target of that name is in force.
func (s *Server) staticTargetInForce(name string) bool {
	for _, target := range s.manager.StaticTargets() {
		if target.Name == name {
			return true
		}
	}
	return false
}

// withStaticTargetLabel is set with every series labelled static_target, the
// target's name, over any label of that name the series had, as the endpoint
// labels it (mergeStaticTargets). set itself is left alone.
func withStaticTargetLabel(set model.MetricSet, name string) model.MetricSet {
	out := model.MetricSet{Metrics: make([]model.Metric, len(set.Metrics))}
	for i, m := range set.Metrics {
		m.Labels = model.CloneLabels(m.Labels)
		if m.Labels == nil {
			m.Labels = map[string]string{}
		}
		m.Labels[config.StaticTargetLabel] = name
		out.Metrics[i] = m
	}
	return out
}

// staticTargetResults returns the latest result of each target in force, by
// name, its data's age as of now, and forgets the results of targets no
// longer in force.
func (s *Server) staticTargetResults() []namedSet {
	targets := s.manager.StaticTargets()
	current := make(map[string]bool, len(targets))
	var out []namedSet
	now := time.Now()
	s.staticMu.Lock()
	for _, target := range targets {
		current[target.Name] = true
		if set, ok := s.staticResults[target.Name]; ok {
			if fetched, aged := s.staticFetched[target.Name]; aged {
				set = withResultAge(set, now.Sub(fetched))
			}
			out = append(out, namedSet{name: target.Name, set: set, labelled: true})
		}
	}
	for name := range s.staticResults {
		if !current[name] {
			delete(s.staticResults, name)
			delete(s.staticFetched, name)
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

// withResultAge is set with its http_exporter_result_age_seconds set to age,
// in a copy of its series, so the stored result is left as it was.
func withResultAge(set model.MetricSet, age time.Duration) model.MetricSet {
	out := model.MetricSet{Metrics: make([]model.Metric, len(set.Metrics))}
	copy(out.Metrics, set.Metrics)
	for i := range out.Metrics {
		if out.Metrics[i].Name == resultAgeMetric {
			out.Metrics[i].Value = max(age.Seconds(), 0)
		}
	}
	return out
}

// namedSet is a target's result; labelled when its series already carry
// static_target, as the stored results do.
type namedSet struct {
	name     string
	set      model.MetricSet
	labelled bool
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
	names, err := s.requestedStaticTargets(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Every target is merged, filtered or not, so which family keeps its type
	// in a clash — and what is logged about it — does not depend on which
	// targets a read asked for: a filtered read serves exactly the named
	// targets' part of what an unfiltered one would.
	merged := s.mergeStaticTargets(s.staticTargetResults())
	if names != nil {
		kept := merged.Metrics[:0]
		for _, m := range merged.Metrics {
			if names[m.Labels[config.StaticTargetLabel]] {
				kept = append(kept, m)
			}
		}
		merged.Metrics = kept
	}
	writeMetricSet(w, &merged)
}

// staticTargetsParam is the query parameter that narrows the endpoint to some
// targets: ?targets=eu,us, or repeated, ?targets=eu&targets=us, which is how
// Prometheus renders a scrape config's params list.
const staticTargetsParam = "targets"

// requestedStaticTargets reads the targets parameter: nil when there is none,
// so every target is served, else the names it gives. A name no target in
// force has is refused rather than served as nothing, so a misspelt or
// removed target fails the scrape where it can be seen; so is a parameter
// that names no target at all.
func (s *Server) requestedStaticTargets(query url.Values) (map[string]bool, error) {
	values, given := query[staticTargetsParam]
	if !given {
		return nil, nil
	}
	names := map[string]bool{}
	for _, value := range values {
		for _, name := range strings.Split(value, ",") {
			if name = strings.TrimSpace(name); name != "" {
				names[name] = true
			}
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("the %s parameter names no static target; give one or more names, separated by commas, or leave it out to read every target", staticTargetsParam)
	}
	known := map[string]bool{}
	for _, target := range s.manager.StaticTargets() {
		known[target.Name] = true
	}
	var unknown []string
	for name := range names {
		if !known[name] {
			unknown = append(unknown, strconv.Quote(name))
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("no static target is named %s", strings.Join(unknown, ", "))
	}
	return names, nil
}

// mergeStaticTargets puts the targets' results into one exposition: every
// series labelled with its target, and each family's series together, as the
// text format requires, in the order the families first appear. A family one
// target exports with another type than an earlier target is left out for that
// target and logged, since one family cannot have two types; the rest of both
// targets is served.
func (s *Server) mergeStaticTargets(results []namedSet) model.MetricSet {
	clashes := map[string]staticClash{}
	type family struct {
		typ     model.MetricType
		metrics []model.Metric
	}
	families := map[string]*family{}
	var order []string
	for _, result := range results {
		set := result.set
		if !result.labelled {
			set = withStaticTargetLabel(set, result.name)
		}
		for _, m := range set.Metrics {
			f := families[m.Name]
			if f == nil {
				f = &family{typ: m.Type}
				families[m.Name] = f
				order = append(order, m.Name)
			}
			if f.typ != m.Type {
				key := failureKey("", "static target "+result.name, "family "+m.Name)
				clashes[key] = staticClash{target: result.name, metric: m.Name}
				s.failures.failed(s.logger, slog.LevelWarn, key,
					"static target metric left out of the static targets endpoint", "exposition", errFamilyTypeClash,
					"target", result.name, "metric", m.Name, "type", string(m.Type), "type_in_use", string(f.typ))
				continue
			}
			f.metrics = append(f.metrics, m)
		}
	}
	s.settleStaticClashes(clashes, results)
	var out model.MetricSet
	for _, name := range order {
		out.Metrics = append(out.Metrics, families[name].metrics...)
	}
	return out
}

// staticClash is a target's metric left out of the endpoint for its type.
type staticClash struct{ target, metric string }

// settleStaticClashes ends the clashes of the previous read that this one,
// with results, no longer has. One whose target is still served is logged as
// back on the endpoint, as a failure that stopped is logged as recovered; one
// whose target is gone is forgotten without a word, since nothing was fixed.
// Without this a clash that went away was never said to have, and one that
// came back within the failure log's memory read as the old one continuing.
func (s *Server) settleStaticClashes(clashes map[string]staticClash, results []namedSet) {
	served := make(map[string]bool, len(results))
	for _, result := range results {
		served[result.name] = true
	}
	s.staticClashMu.Lock()
	previous := s.staticClashes
	s.staticClashes = clashes
	s.staticClashMu.Unlock()
	for key, clash := range previous {
		if _, still := clashes[key]; still {
			continue
		}
		if !served[clash.target] {
			s.failures.forget(key)
			continue
		}
		s.failures.recovered(s.logger, key, "static target metric back on the static targets endpoint", "target", clash.target, "metric", clash.metric)
	}
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
