package config

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/discovery"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The exporter asks the dns_sd sources of its static target file's
// discovered_targets itself (staticdiscovery.go). Each source is asked every
// refresh interval, from the moment the file that has it is put in force.
// An answer that differs from the source's last puts the file's targets in
// force again, with those the entries make of every source's last answer,
// as a reload of the static target file would, under the lock every reload
// takes. A failure keeps the source's last answer, as the allocator keeps
// it. A source that has not answered yet gives no targets.

// discoverySourceState is a source being asked.
type discoverySourceState struct {
	key, entry string
	source     model.DiscoverySource
	// next is when it is asked again, and running says it is being asked.
	next    time.Time
	running bool
	// found is its last answer, answered whether it gave one.
	found    []discovery.Target
	answered bool
	// lastErr is the text of its last failure, "" after an answer, so a
	// failure that stays the same is logged once.
	lastErr             string
	successes, failures uint64
	lastSuccess         time.Time
}

// staticDiscovery is the manager's sources, by key, and what the last
// expansion checked and left out.
type staticDiscovery struct {
	mu      sync.Mutex
	sources map[string]*discoverySourceState
	wake    chan struct{}
	// checks and skipped are the expansion's, read and written under the
	// manager's reloadMu.
	checks  discoveryChecks
	skipped string
}

// discoveryNow is the clock of the sources' schedule; a test sets it.
var discoveryNow = time.Now

// wakeDiscovery tells DiscoveryLoop the sources changed.
func (m *Manager) wakeDiscovery() {
	m.discovery.mu.Lock()
	if m.discovery.wake == nil {
		m.discovery.wake = make(chan struct{}, 1)
	}
	wake := m.discovery.wake
	m.discovery.mu.Unlock()
	select {
	case wake <- struct{}{}:
	default:
	}
}

// syncDiscovery makes the sources asked those of f's entries: a source
// whose key f keeps goes on with what it found, a new one is asked at once,
// and one f no longer has is dropped. reloadMu is held.
func (m *Manager) syncDiscovery(f *model.StaticTargetFile) {
	m.discovery.mu.Lock()
	kept := map[string]*discoverySourceState{}
	for i := range f.Discovered {
		e := &f.Discovered[i]
		for j := range e.Sources {
			key := discoverySourceKey(e, &e.Sources[j])
			s := m.discovery.sources[key]
			if s == nil {
				s = &discoverySourceState{key: key, entry: e.Name, source: e.Sources[j], next: discoveryNow()}
			}
			kept[key] = s
		}
	}
	changed := len(kept) != len(m.discovery.sources)
	for key := range kept {
		if m.discovery.sources[key] == nil {
			changed = true
		}
	}
	m.discovery.sources = kept
	m.discovery.mu.Unlock()
	if changed {
		m.wakeDiscovery()
	}
}

// discovered is f with the targets its entries make of every source's last
// answer, checked against c, logging what is left out once while it stays
// the same. reloadMu is held.
func (m *Manager) discovered(f *model.StaticTargetFile, c *model.Config) *model.StaticTargetFile {
	if f == nil || len(f.Discovered) == 0 {
		return f
	}
	found := map[string][]discovery.Target{}
	m.discovery.mu.Lock()
	for key, s := range m.discovery.sources {
		if s.answered {
			found[key] = s.found
		}
	}
	m.discovery.mu.Unlock()
	out, skipped, checks := expandDiscovered(f, found, c, m.discovery.checks)
	m.discovery.checks = checks
	if text := strings.Join(skipped, "; "); text != m.discovery.skipped {
		m.discovery.skipped = text
		for _, s := range skipped {
			m.logger.Warn("discovered target left out", "reason", s)
		}
	}
	return out
}

// DiscoveryLoop asks the sources of the static target file in force until
// ctx ends, each on its own refresh interval and each ask within its
// timeout, and puts the targets in force again when an answer changes.
func (m *Manager) DiscoveryLoop(ctx context.Context) {
	m.wakeDiscovery()
	m.discovery.mu.Lock()
	wake := m.discovery.wake
	m.discovery.mu.Unlock()
	var running sync.WaitGroup
	defer running.Wait()
	for {
		now := discoveryNow()
		next := now.Add(time.Hour)
		var due []*discoverySourceState
		m.discovery.mu.Lock()
		for _, s := range m.discovery.sources {
			switch {
			case s.running:
			case !now.Before(s.next):
				s.running = true
				due = append(due, s)
			case s.next.Before(next):
				next = s.next
			}
		}
		m.discovery.mu.Unlock()
		for _, s := range due {
			running.Go(func() { m.askSource(ctx, s) })
		}
		timer := time.NewTimer(next.Sub(now))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-wake:
			timer.Stop()
		}
	}
}

// askSource asks s once, keeps its answer, and puts the targets in force
// again when the answer is a change.
func (m *Manager) askSource(ctx context.Context, s *discoverySourceState) {
	limit := s.source.MaxTargets
	if limit == 0 {
		limit = DefaultMaxDiscoveredTargets
	}
	found, err := discovery.RefreshDNSSD(ctx, s.source.DNSSD, s.source.Labels, limit, discoveryTimeout(&s.source))
	if ctx.Err() != nil {
		return
	}
	refresh, _ := discoveryRefresh(&s.source)
	m.discovery.mu.Lock()
	s.running = false
	s.next = discoveryNow().Add(refresh)
	current := m.discovery.sources[s.key] == s
	changed := false
	switch {
	case err != nil:
		s.failures++
		if text := err.Error(); text != s.lastErr {
			s.lastErr = text
			m.logger.Warn("discovery source failed; its last answer stays", "entry", s.entry, "source", s.source.Name, "error", err)
		}
	default:
		s.successes++
		s.lastSuccess, s.lastErr = discoveryNow(), ""
		changed = !s.answered || !slices.EqualFunc(s.found, found, func(a, b discovery.Target) bool {
			return a.Address == b.Address && mapsEqual(a.Labels, b.Labels)
		})
		s.found, s.answered = found, true
	}
	m.discovery.mu.Unlock()
	m.wakeDiscovery()
	if current && changed {
		m.rediscover()
	}
}

// rediscover puts the static target file in force again with what the
// sources last found, when that changes its targets.
func (m *Manager) rediscover() {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	f := m.StaticTargetFile()
	if f == nil || len(f.Discovered) == 0 {
		return
	}
	out := m.discovered(f, m.Get())
	if slices.EqualFunc(out.Targets, f.Targets, func(a, b model.StaticTarget) bool {
		return a.Name == b.Name && a.Target == b.Target && mapsEqual(a.Labels, b.Labels)
	}) {
		return
	}
	m.install(discoveryTrigger, nil, out)
}

// discoveryTrigger is what a change of the discovered targets is logged as,
// as a reload's trigger is. It is no reload: the reload status and its
// self-metrics leave it out.
const discoveryTrigger = "discovery"

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// DiscoveryHelp is the help of the discovery families.
var DiscoveryHelp = map[string]string{
	"http_exporter_discovery_targets":                        "Targets each dns_sd source of the static target file's discovered_targets last found.",
	"http_exporter_discovery_refreshes_total":                "Asks of each dns_sd source of the static target file's discovered_targets, by result: success or failure.",
	"http_exporter_discovery_last_success_timestamp_seconds": "Unix time each dns_sd source of the static target file's discovered_targets last answered; 0 for one that never has.",
}

// DiscoveryMetrics renders the sources' state, one family at a time, by
// entry and source.
func (m *Manager) DiscoveryMetrics() []model.Metric {
	m.discovery.mu.Lock()
	defer m.discovery.mu.Unlock()
	keys := make([]string, 0, len(m.discovery.sources))
	for key := range m.discovery.sources {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b string) int {
		x, y := m.discovery.sources[a], m.discovery.sources[b]
		return strings.Compare(x.entry+"\x00"+x.source.Name, y.entry+"\x00"+y.source.Name)
	})
	var targets, refreshes, last []model.Metric
	for _, key := range keys {
		s := m.discovery.sources[key]
		labels := func(extra ...string) map[string]string {
			l := map[string]string{"entry": s.entry, "source": s.source.Name}
			for i := 0; i+1 < len(extra); i += 2 {
				l[extra[i]] = extra[i+1]
			}
			return l
		}
		targets = append(targets, model.Metric{Name: "http_exporter_discovery_targets", Help: DiscoveryHelp["http_exporter_discovery_targets"], Type: model.GaugeMetricType, Value: float64(len(s.found)), Labels: labels()})
		refreshes = append(refreshes,
			model.Metric{Name: "http_exporter_discovery_refreshes_total", Help: DiscoveryHelp["http_exporter_discovery_refreshes_total"], Type: model.CounterMetricType, Value: float64(s.failures), Labels: labels("result", "failure")},
			model.Metric{Name: "http_exporter_discovery_refreshes_total", Help: DiscoveryHelp["http_exporter_discovery_refreshes_total"], Type: model.CounterMetricType, Value: float64(s.successes), Labels: labels("result", "success")})
		at := 0.0
		if !s.lastSuccess.IsZero() {
			at = model.ScrapeTimestamp(s.lastSuccess)
		}
		last = append(last, model.Metric{Name: "http_exporter_discovery_last_success_timestamp_seconds", Help: DiscoveryHelp["http_exporter_discovery_last_success_timestamp_seconds"], Type: model.GaugeMetricType, Value: at, Labels: labels()})
	}
	return append(append(targets, refreshes...), last...)
}
