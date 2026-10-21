package allocator

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// sourceState is a running discovery source and what it last found.
type sourceState struct {
	spec  sourceSpec
	limit int
	// cancel stops its refresh loop; nil while none runs.
	cancel context.CancelFunc
	// The rest is guarded by Allocator.dmu. found is the last good answer
	// and ok says there is one: a source that never answered has no
	// targets, and one that fails keeps those of its last good answer.
	found     []discovered
	ok        bool
	lastErr   string
	successes uint64
	failures  uint64
	lastGood  time.Time
}

// fetch asks the source once.
func (s *sourceState) fetch(ctx context.Context) ([]discovered, error) {
	d := &s.spec.cfg
	switch {
	case d.FileSD != nil:
		groups, err := refreshFileSD(d.FileSD)
		if err != nil {
			return nil, err
		}
		return flatten(groups, d.Labels, s.limit)
	case d.HTTPSD != nil:
		groups, err := refreshHTTPSD(ctx, d.HTTPSD, seconds(d.refresh().Seconds()))
		if err != nil {
			return nil, err
		}
		return flatten(groups, d.Labels, s.limit)
	default:
		return refreshKubernetes(ctx, d.Kubernetes, d.Labels, s.limit)
	}
}

// syncSources makes the sources run those of in: a source whose owner and
// configuration stay keeps running, with what it found; one in no longer
// stops, and a new one starts. A new source that replaces one of the same
// kinds, owner and name, a source whose settings a reload changed, starts
// with what the old one found, and with its counts, so its targets stay in
// force until it answers itself rather than leaving every share and coming
// back a refresh later; any other new source starts with nothing found.
// Called under reloadMu.
func (a *Allocator) syncSources(in *Inputs) {
	a.dmu.Lock()
	defer a.dmu.Unlock()
	want := map[string]bool{}
	for _, spec := range in.sources {
		want[spec.key] = true
	}
	replaced := map[string]*sourceState{}
	for key, s := range a.sources {
		if !want[key] {
			if s.cancel != nil {
				s.cancel()
			}
			delete(a.sources, key)
			replaced[s.spec.identity()] = s
		}
	}
	for _, spec := range in.sources {
		if a.sources[spec.key] != nil {
			continue
		}
		limit := spec.cfg.MaxTargets
		if limit == 0 {
			limit = a.maxDiscovered
		}
		s := &sourceState{spec: spec, limit: limit}
		if old := replaced[spec.identity()]; old != nil {
			// The failure text is left behind: the new settings' first
			// failure is logged and counted, even one of the same text.
			s.found, s.ok = old.found, old.ok
			s.successes, s.failures, s.lastGood = old.successes, old.failures, old.lastGood
		}
		a.sources[spec.key] = s
		if a.discovering {
			a.startSource(s)
		}
	}
}

// identity is what stays of a source whose settings change: the kind of
// its owner, the owner, its own kind and its name.
func (s sourceSpec) identity() string {
	return s.kind + "/" + s.owner + "/" + s.cfg.kind() + "/" + s.cfg.Name
}

// StartDiscovery starts a refresh loop for every discovery source, and for
// every source a reload adds later, each asking its source at once and
// then every refresh_interval. BeginShutdown stops them.
func (a *Allocator) StartDiscovery() {
	a.dmu.Lock()
	defer a.dmu.Unlock()
	if a.discovering || a.discoveryStopped {
		return
	}
	a.discovering = true
	for _, s := range a.sources {
		a.startSource(s)
	}
}

// stopDiscovery stops every refresh loop.
func (a *Allocator) stopDiscovery() {
	a.dmu.Lock()
	defer a.dmu.Unlock()
	a.discovering, a.discoveryStopped = false, true
	for _, s := range a.sources {
		if s.cancel != nil {
			s.cancel()
			s.cancel = nil
		}
	}
}

// startSource starts s's refresh loop. Called under dmu.
func (a *Allocator) startSource(s *sourceState) {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	go func() {
		for {
			if a.refreshSource(ctx, s) {
				a.applyDiscovery()
			}
			timer := time.NewTimer(s.spec.cfg.refresh())
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}

// refreshSource asks s once and reports whether what it found changed. A
// failure keeps what s found last, and is logged and counted once while
// its text stays the same, as a refused reload is.
func (a *Allocator) refreshSource(ctx context.Context, s *sourceState) bool {
	found, err := s.fetch(ctx)
	if ctx.Err() != nil {
		// Stopped meanwhile: the source is no longer in force.
		return false
	}
	a.dmu.Lock()
	defer a.dmu.Unlock()
	attrs := []any{"kind", s.spec.kind, "owner", s.spec.owner, "source", s.spec.cfg.Name}
	if err != nil {
		if text := model.SameFailureText(err); text != s.lastErr {
			s.lastErr = text
			s.failures++
			if s.ok {
				a.logger.Warn("a discovery source failed; the targets of its last good answer stay in force", append(attrs, "error", err)...)
			} else {
				a.logger.Warn("a discovery source failed and has never answered; it adds no targets until it does", append(attrs, "error", err)...)
			}
		}
		return false
	}
	if s.lastErr != "" {
		a.logger.Info("a discovery source answers again", attrs...)
	}
	s.lastErr = ""
	s.successes++
	s.lastGood = a.clock.Now()
	if s.ok && sameDiscovered(s.found, found) {
		return false
	}
	s.found, s.ok = found, true
	return true
}

func sameDiscovered(a, b []discovered) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// foundBySource is what every source that has answered found, by key.
func (a *Allocator) foundBySource() map[string][]discovered {
	a.dmu.Lock()
	defer a.dmu.Unlock()
	out := make(map[string][]discovered, len(a.sources))
	for key, s := range a.sources {
		if s.ok {
			out[key] = s.found
		}
	}
	return out
}

// applyDiscovery puts in force the input files in force with what the
// sources found now.
func (a *Allocator) applyDiscovery() {
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()
	if a.putInForce(a.base) {
		a.logger.Info("discovered targets changed", a.currentInputs().logAttrs()...)
	}
}

func (a *Allocator) currentInputs() *Inputs {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inputs
}

// putInForce puts base, with what the sources found added, in force, as a
// reload does: a static target that moves to another member is handed
// over, and one no longer there leaves every share at once. It reports
// whether the inputs in force changed. Called under reloadMu.
func (a *Allocator) putInForce(base *Inputs) bool {
	in, skipped, checked := base.expand(a.foundBySource(), a.checked)
	a.checked = checked
	if text := strings.Join(skipped, "\n"); text != a.lastSkipped {
		a.lastSkipped = text
		if len(skipped) > 0 {
			shown := skipped
			if len(shown) > 10 {
				shown = shown[:10]
			}
			a.logger.Warn(fmt.Sprintf("%d discovered targets are left out", len(skipped)), "targets", shown)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.inputs != nil && a.inputs.digest == in.digest {
		return false
	}
	old := a.inputs
	a.inputs = in
	a.handOver(old, a.clock.Now())
	a.rebuild()
	a.schedule()
	return true
}

// sourceMetrics are what /metrics shows of each source, in order of kind,
// owner and name.
type sourceMetric struct {
	kind, owner, name   string
	found               int
	successes, failures uint64
	lastGood            time.Time
}

func (a *Allocator) sourceMetrics() []sourceMetric {
	a.dmu.Lock()
	defer a.dmu.Unlock()
	out := make([]sourceMetric, 0, len(a.sources))
	for _, s := range a.sources {
		found := 0
		if s.ok {
			found = len(s.found)
		}
		out = append(out, sourceMetric{kind: s.spec.kind, owner: s.spec.owner, name: s.spec.cfg.Name, found: found, successes: s.successes, failures: s.failures, lastGood: s.lastGood})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].kind != out[j].kind {
			return out[i].kind < out[j].kind
		}
		if out[i].owner != out[j].owner {
			return out[i].owner < out[j].owner
		}
		return out[i].name < out[j].name
	})
	return out
}
