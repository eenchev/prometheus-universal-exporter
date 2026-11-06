package allocator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
	// refreshed is when the source was last asked here, or its targets
	// last taken from the leader's snapshot, whatever came of it, and
	// fromSnapshot says what the source holds came from the snapshot.
	refreshed    time.Time
	fromSnapshot bool
	// leaderRefresh is how often the leader asks the source, as its last
	// snapshot taken said, 0 when it said none.
	leaderRefresh time.Duration
}

// interval is how often what the source holds is renewed: the leader's
// interval of it when it came from the leader's snapshot and the snapshot
// said one, as the leader is who asks it then, whatever this instance's
// own settings of it say, and the source's own otherwise.
func (s *sourceState) interval() time.Duration {
	if s.fromSnapshot && s.leaderRefresh > 0 {
		return s.leaderRefresh
	}
	return s.spec.cfg.refresh()
}

// fetch asks the source once; logger takes what is logged on the way.
func (s *sourceState) fetch(ctx context.Context, logger *slog.Logger) ([]discovered, error) {
	d := &s.spec.cfg
	switch {
	case d.FileSD != nil:
		groups, err := refreshFileSD(d.FileSD)
		if err != nil {
			return nil, err
		}
		return flatten(groups, d.Labels, s.limit)
	case d.HTTPSD != nil:
		groups, err := refreshHTTPSD(ctx, d.HTTPSD, seconds(d.refresh().Seconds()), d.timeout())
		if err != nil {
			return nil, err
		}
		return flatten(groups, d.Labels, s.limit)
	case d.DNSSD != nil:
		return refreshDNSSD(ctx, d.DNSSD, d.Labels, s.limit, d.timeout())
	default:
		return refreshKubernetes(ctx, d.Kubernetes, d.Labels, s.limit, d.timeout(), logger)
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
			s.refreshed, s.fromSnapshot, s.leaderRefresh = old.refreshed, old.fromSnapshot, old.leaderRefresh
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
// every source a reload adds later, each asking its source at once and then
// every refresh_interval. BeginShutdown stops them. An instance among peers
// (UsePeers) does not call it: RunPeers starts and stops the loops as the
// instance comes to lead and stops leading.
func (a *Allocator) StartDiscovery() { a.setDiscovering(true) }

// setDiscovering starts the refresh loops, or stops them, keeping what the
// sources found; stopping them stops those a follower runs of sources the
// leader lacks too, which its next snapshot starts again. After
// BeginShutdown it does nothing.
func (a *Allocator) setDiscovering(on bool) {
	a.dmu.Lock()
	defer a.dmu.Unlock()
	if a.discoveryStopped || a.discovering == on {
		return
	}
	a.discovering = on
	for _, s := range a.sources {
		switch {
		case on:
			// A follower may already ask a source the leader lacks
			// (takeSnapshot).
			if s.cancel == nil {
				a.startSource(s)
			}
		case s.cancel != nil:
			s.cancel()
			s.cancel = nil
		}
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
			found, err := a.askSource(ctx, s)
			// The answer is taken and put in force under reloadMu, as a
			// snapshot is served (snapshotOf), so no snapshot holds what a
			// source found without the targets it moved draining.
			a.reloadMu.Lock()
			if a.takeAnswer(ctx, s, found, err) {
				a.applyDiscoveryLocked()
			}
			a.reloadMu.Unlock()
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
	found, err := a.askSource(ctx, s)
	return a.takeAnswer(ctx, s, found, err)
}

// askSource asks s once.
func (a *Allocator) askSource(ctx context.Context, s *sourceState) ([]discovered, error) {
	attrs := []any{"kind", s.spec.kind, "owner", s.spec.owner, "source", s.spec.cfg.Name}
	return s.fetch(ctx, a.logger.With(attrs...))
}

// takeAnswer takes s's answer, found or err, and reports whether what it
// found changed.
func (a *Allocator) takeAnswer(ctx context.Context, s *sourceState, found []discovered, err error) bool {
	if ctx.Err() != nil {
		// Stopped meanwhile: the source is no longer in force.
		return false
	}
	a.dmu.Lock()
	defer a.dmu.Unlock()
	s.refreshed = a.clock.Now()
	if err != nil {
		a.sourceFailed(s, err)
		return false
	}
	a.sourceAnswered(s)
	s.successes++
	s.lastGood = s.refreshed
	s.fromSnapshot = false
	if s.ok {
		found = holdWeights(s.found, found, s.spec.weightLabel)
	}
	if s.ok && sameDiscovered(s.found, found) {
		return false
	}
	s.found, s.ok = found, true
	return true
}

// sourceFailed logs and counts a failure of s, an answer of its own or
// the leader's snapshot of it refused, once while its text stays the same.
// Called under dmu.
func (a *Allocator) sourceFailed(s *sourceState, err error) {
	text := model.SameFailureText(err)
	if text == s.lastErr {
		return
	}
	s.lastErr = text
	s.failures++
	attrs := []any{"kind", s.spec.kind, "owner", s.spec.owner, "source", s.spec.cfg.Name, "error", err}
	if s.ok {
		a.logger.Warn("a discovery source failed; the targets of its last good answer stay in force", attrs...)
	} else {
		a.logger.Warn("a discovery source failed and has never answered; it adds no targets until it does", attrs...)
	}
}

// sourceAnswered notes an answer of s taken, logging the end of a
// failure. Called under dmu.
func (a *Allocator) sourceAnswered(s *sourceState) {
	if s.lastErr != "" {
		a.logger.Info("a discovery source answers again", "kind", s.spec.kind, "owner", s.spec.owner, "source", s.spec.cfg.Name)
	}
	s.lastErr = ""
}

func sameDiscovered(a, b []discovered) bool {
	x, _ := json.Marshal(groupsOf(a))
	y, _ := json.Marshal(groupsOf(b))
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

// applyDiscoveryLocked puts in force the input files in force with what
// the sources found now. Called under reloadMu.
func (a *Allocator) applyDiscoveryLocked() {
	if a.putInForce(a.base, true, nil) {
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
// over, and one no longer there leaves every share at once. discovered says
// the change is one of the discovered targets. leader, when not nil, are
// the drains of the leader's snapshot the discovered targets come from,
// which are put in force in place of those the change would make here
// (takeDrains), even when the targets stay the same. It reports whether the
// inputs in force changed. Called under reloadMu.
func (a *Allocator) putInForce(base *Inputs, discovered bool, leader *peerSnapshot) bool {
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
	counts := a.noteFallbacks(in.fallbacks)
	a.mu.Lock()
	defer a.mu.Unlock()
	for key, n := range counts {
		a.fallbacksTotal[key] += n
	}
	same := a.inputs != nil && a.inputs.digest == in.digest
	if same && leader == nil {
		return false
	}
	old, now := a.inputs, a.clock.Now()
	if !same {
		a.inputs = in
	}
	if leader != nil {
		if !a.takeDrains(leader.Draining, now) && same {
			a.schedule()
			return false
		}
	} else {
		a.handOver(old, now, discovered)
	}
	a.rebuild()
	a.schedule()
	return !same
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
