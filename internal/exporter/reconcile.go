package exporter

import (
	"reflect"
	"sync/atomic"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The exporter keeps state per collector: its self-metric counters, the
// verbose per-request series and scrape-time histogram, cached results and
// what the failure log remembers. A reload can remove a collector, or change
// its definition, and that state has to follow:
//
//   - A removed collector's self-metric series stop being exposed and exported,
//     so Prometheus marks them stale instead of showing a collector that no
//     longer exists with frozen values; everything kept about it is dropped,
//     and a collector later added again under the name starts from zero.
//   - A changed collector keeps its counters, which describe the collector by
//     name, but its cached results are dropped: their keys carry the old
//     definition, so they could never be served again, and would only hold
//     memory and count in http_exporter_cache_entries until they expired.
//     What the failure log remembers of it is forgotten too: a failure of the
//     old definition is none of the new one's, whose first failure is logged
//     as a first. A probe's failures are told apart by the definition already
//     (probeCacheKey); a static target's are not.
//
// The state follows a reload when the reload is made: the configuration
// manager tells the server when it has put something in force
// (config.Manager's OnInstall, followReload), on the goroutine that reloads
// and never on a probe's. What following a configuration takes long to work
// out, the fingerprints of its collectors, the reload works out before it
// puts the configuration in force (config.Manager's OnPrepare,
// prepareReload), so that nothing that uses the configuration waits for it.
//
// The state does not depend on having been told: the server also compares
// the configuration it last saw with the current one whenever it is about
// to use its per-collector state, which costs a pointer comparison while
// nothing has changed, and follows then what it was not told of. Followed
// only so, two reloads with no use of the state between
// them are followed as one: a collector the first removed and the second
// brought back as it was is then one that stayed.
//
// A probe or a static target scrape reads its collector in one configuration
// and counts some time later, when a reload may have removed the collector,
// and another brought one back under the name. So the configurations followed
// are numbered, and a probe or scrape says which one it read (statsSince): it
// counts in the collector's statistics only while the collector has been in
// every configuration followed since.
//
// What it writes under the collector's name when it ends is held to the same
// test, and to a finer one: its result in the cache, and what the failure log
// remembers of it, are the collector's only while the collector has been in
// every configuration followed since with the definition the probe or scrape
// read (configRead). A result made by a definition that is gone would be
// served to a collector brought back under the name with that definition,
// and one of a definition a reload changed could never be served and would
// only count in the collector's max_cache_entries; a failure of either would
// pass for the first of the collector's own.
//
// A static target's scrape reads its target with its collector, and the
// result it publishes for the static targets endpoint is held to both: it is
// the target's only while the collector stands as above and the target has
// been in every static target file followed since as the scrape read it
// (targetStands). So the static target file in force is followed with the
// configuration: a reload of either makes a generation. What the failure
// log remembers of the scrape is held to both as well (logStands), and the
// remembered failures of a target a reload removed or changed are forgotten
// as those of a changed collector are: the failure log tells a static
// target's failures apart by the target's name, not by its definition.

// followedConfig is a configuration the per-collector state has followed,
// with the static target file in force with it, nil without one, and its
// generation: one more than that of the configuration followed before it,
// from firstGeneration. defined says, for each collector of the
// configuration, the generation it has been there from with the definition
// it has, and targetsDefined the same for each static target of the file;
// neither is changed once the configuration is followed. fingerprints are
// those of the configuration's collectors, by which the next reload tells
// the ones it changed, and the schedule of the static targets the targets
// whose collector changed (fingerprintOf): each is worked out once, when
// this configuration is followed or when the next is, and not again for
// every reload.
type followedConfig struct {
	config         *model.Config
	targets        *model.StaticTargetFile
	generation     uint64
	defined        map[string]uint64
	targetsDefined map[string]uint64
	fingerprints   *fingerprintGeneration
}

// fingerprint is the fingerprint of the configuration's collector at index:
// the one kept for it, and one made afresh when none is kept.
func (f *followedConfig) fingerprint(index int) string {
	if f.fingerprints == nil {
		return collectorFingerprint(&f.config.Collectors[index])
	}
	return f.fingerprints.at(index)
}

// fingerprintOf is the fingerprint of the collector of cfg named name, and
// empty when cfg has none of that name: collectorFingerprint of the first
// collector so named. It is for a caller that was given cfg and f together,
// as the schedule of the static targets is (planFollowed), and is told
// whether a collector changed by the very fingerprints the following of the
// reload was: when cfg is the configuration f followed, the fingerprint is
// the one kept for it, which the reload made before it put cfg in force
// (prepareReload) and which is made here, once, and kept when nothing had
// asked for it, as for the configuration the exporter started with. Only
// the fingerprints of cfg itself are read: for any other configuration, and
// for a caller that follows none, f nil, the definition is encoded afresh,
// as it was for every call. f never changes once followed, so no reload that
// comes meanwhile makes them those of another configuration. It takes no
// lock, and waits only where a probe or a reload is making that same
// fingerprint, for that one (fingerprintGeneration.at).
func (f *followedConfig) fingerprintOf(cfg *model.Config, name string) string {
	for i := range cfg.Collectors {
		if cfg.Collectors[i].Name != name {
			continue
		}
		if f != nil && f.config == cfg && f.fingerprints != nil && f.fingerprints.config == cfg {
			return f.fingerprints.at(i)
		}
		return collectorFingerprint(&cfg.Collectors[i])
	}
	return ""
}

// configRead is the configuration a probe or a static target scrape read its
// collector in: the generation it was followed at, and where the
// configuration followed now is found. target is the static target a scrape
// read with its collector, by name, and nil for a probe, which reads none.
// The zero value is that of a caller that names no configuration, whose
// collector always stands.
type configRead struct {
	followed   *atomic.Pointer[followedConfig]
	generation uint64
	target     *string
}

// readAt is the configuration followed at generation, as a probe that read
// its collector there carries it.
func (s *Server) readAt(generation uint64) configRead {
	return configRead{followed: &s.followed, generation: generation}
}

// readTargetAt is readAt for the scrape of the static target named target,
// read with its collector at generation.
func (s *Server) readTargetAt(generation uint64, target string) configRead {
	return configRead{followed: &s.followed, generation: generation, target: &target}
}

// stands reports whether the collector name is still the one r read: in the
// configuration followed now, and in every one followed since r's, with the
// definition it had there. It is the test the statistics make by generation
// (statsSince), which a changed definition fails as a removed collector
// does. What a trip writes under the collector's name asks it under the lock
// of what it writes to, which followLocked holds while it drops what the
// reload retires and replaces the followed configuration: so nothing is
// written after the drop that the drop was to remove, and it costs a
// writer one atomic load and a map lookup, under a lock it holds anyway.
func (r configRead) stands(name string) bool {
	if r.followed == nil {
		return true
	}
	followed := r.followed.Load()
	if followed == nil {
		return true
	}
	since, defined := followed.defined[name]
	return defined && since <= r.generation
}

// targetStands is stands for a static target's scrape and the result it
// publishes: the collector stands, and the static target name is still the
// one r read, in every static target file followed since r's as it was
// there. A target a reload removed, changed or brought back since is another
// target under the name, whose own scrape publishes its result. The scrape
// asks under the lock of the targets' results, which the followed
// configuration is replaced under (storeFollowed).
func (r configRead) targetStands(collector, name string) bool {
	if r.followed == nil {
		return true
	}
	followed := r.followed.Load()
	if followed == nil {
		return true
	}
	if since, defined := followed.defined[collector]; !defined || since > r.generation {
		return false
	}
	since, defined := followed.targetsDefined[name]
	return defined && since <= r.generation
}

// logStands is the test what a trip tells the failure log is held to: stands
// for a probe, and targetStands for the scrape of a static target, whose
// failures are remembered under the target's name. A scrape that read its
// target before a reload of the static target file changed it, the collector
// as it was, would otherwise have its failure remembered as one of the target
// now under the name, and its success logged as that target's recovery.
func (r configRead) logStands(collector string) bool {
	if r.target != nil {
		return r.targetStands(collector, *r.target)
	}
	return r.stands(collector)
}

// firstGeneration is the generation of the configuration the exporter starts
// with. noGeneration, which is before it, is that of a configuration the
// per-collector state does not follow: no collector has been there since.
const (
	noGeneration    uint64 = 0
	firstGeneration uint64 = 1
)

// reconcile brings the per-collector state in line with the configuration in
// force, and returns that configuration with its generation, for the caller
// to use together: the collectors it reads there are the ones whose
// statistics the generation names.
//
// What the following is to do is worked out before the statistics lock is
// taken (planFollowing), and only done under it (followLocked): telling the
// collectors a reload changed from those it left encodes every one of them,
// which takes as long as the configuration is large, and every probe takes
// that lock to find its collector's statistics. The lock is then taken to
// make a following that is still the one to make: of the configuration and
// file in force then, from the configuration followed then. A reload, or
// another caller's following, may have come between the two; the following
// is then worked out again, from what is followed now, or found made. So
// any number of callers may work out the same following at once, a reload
// and the probes that come while it does: they share the fingerprints, one
// makes the following, and the others find it made. The fingerprints of a
// configuration a reload put in force were made before it was in force
// (prepareReload), so none of them waits for one; only a configuration put
// in force without the manager's reload has them made here.
func (s *Server) reconcile() *followedConfig {
	cfg, targets := s.manager.InForce()
	previous := s.followed.Load()
	for {
		if previous != nil && previous.config == cfg && previous.targets == targets {
			return previous
		}
		plan := planFollowing(previous, cfg, targets, s.fingerprints)
		if hook := followPlannedHook.Load(); hook != nil {
			(*hook)()
		}
		var report func()
		s.statsMu.Lock()
		// Read again under the lock: another reload may have come, and whoever
		// holds the lock follows the latest.
		inForce, file := s.manager.InForce()
		followed := s.followed.Load()
		made := followed == previous && inForce == cfg && file == targets
		if made {
			followed = s.followLocked(plan, &report)
		}
		s.statsMu.Unlock()
		if made {
			if report != nil {
				report()
			}
			return followed
		}
		previous, cfg, targets = followed, inForce, file
	}
}

// followPlannedHook, set by tests, is called when a following has been
// worked out and the statistics lock is not yet taken to make it, so a test
// can put a probe, or another reload, between the two.
var followPlannedHook atomic.Pointer[func()]

// prepareReload works out, for a configuration a reload is about to put in
// force, what following it needs that takes as long as the configuration is
// large: the fingerprint of every collector of cfg, and of those collectors
// of the configuration followed that cfg still names and that nothing has
// asked for yet, which the following compares them with. The configuration
// manager calls it once the reload has read and checked cfg and before it is
// in force (config.Manager's OnPrepare), on the goroutine that reloads: the
// probes of the configuration still in force go on meanwhile, and from when
// cfg is in force whoever follows it first, the reload (followReload) or a
// probe that came before that was done (reconcile), finds every fingerprint
// made, and waits for none.
//
// Only the fingerprints are prepared, which are of a configuration and of
// nothing else: a configuration never changes once loaded, so they cannot be
// out of date whatever is followed when they are used. The plan of the
// following is not: it is from the configuration followed, which is another
// by then when something other than a reload put one in force meanwhile, and
// with the fingerprints made it costs no more than its maps (planFollowing).
// A static target file reloaded alone leaves the configuration the one
// followed, whose fingerprints are kept with it: nothing is prepared.
//
// It takes no lock, and waits only for a probe that is working out one of
// the same fingerprints, for that one.
func (s *Server) prepareReload(cfg *model.Config, _ *model.StaticTargetFile) {
	followed := s.followed.Load()
	if cfg == nil || followed != nil && followed.config == cfg {
		return
	}
	s.fingerprints.prepare(cfg)
	// A configuration followed that a reload prepared has them all: only the
	// one the exporter started with, or one put in force without a reload,
	// may have some left to make.
	if followed == nil || followed.config == nil || followed.fingerprints == nil || followed.fingerprints.whole.Load() {
		return
	}
	named := make(map[string]bool, len(cfg.Collectors))
	for i := range cfg.Collectors {
		named[cfg.Collectors[i].Name] = true
	}
	for i := range followed.config.Collectors {
		if named[followed.config.Collectors[i].Name] {
			followed.fingerprints.at(i)
		}
	}
}

// followReload follows a reload as it is made: the configuration manager
// calls it when it has put a configuration or a static target file in force
// (config.Manager's OnInstall), so the per-collector state is in line with
// what is in force from then on, whether or not anything asks for it, and
// what a probe or scrape of a collector the reload retired writes from then
// on goes nowhere. The reload waits for it. A probe does not, unless it asks
// for the state meanwhile: it then follows the reload itself, as it would
// have without being told, and neither holds a lock the other needs while
// it works out what the following is to do (reconcile), nor waits for a
// fingerprint, all made before the configuration was in force
// (prepareReload).
func (s *Server) followReload() { s.reconcile() }

// generationOf is the generation of cfg when it is the configuration in
// force, and noGeneration when it is not: for a caller that has just read
// cfg and did not read its generation with it.
func (s *Server) generationOf(cfg *model.Config) uint64 {
	if followed := s.reconcile(); followed.config == cfg {
		return followed.generation
	}
	return noGeneration
}

// followedInForce is the manager's InForce as the per-collector state
// follows it: the configuration and the static target file in force, with
// their generation and those of their collectors and targets.
func (s *Server) followedInForce() *followedConfig {
	for {
		cfg, file := s.manager.InForce()
		// A reload between the two readings is followed at the next round.
		if followed := s.reconcile(); followed.config == cfg && followed.targets == file {
			return followed
		}
	}
}

// stay is the generation the stay of the static target named target, of the
// collector named collector, began at: the one the target has been in the
// followed files from as it is, or the one the collector has had its
// definition from, whichever is later. It is the first a scrape of the
// target can have read and still publish (targetStands), so the schedule
// gives a target a first scrape whenever it moves (planFollowed). A caller
// that follows no configuration, f nil, knows of no stay.
func (f *followedConfig) stay(target, collector string) uint64 {
	if f == nil {
		return noGeneration
	}
	return max(f.targetsDefined[target], f.defined[collector])
}

// following is what following a reload is to do, worked out before the
// statistics lock is taken (planFollowing): next is the configuration to
// follow, from previous, the one followed when it was worked out. moved
// names the static targets that left their stay (targetsMoved). collectors
// says that next has another configuration than previous, whose collectors
// are then gone through; removed names those of previous it no longer has,
// and stale those and the ones whose definition changed, neither set when
// previous followed no configuration.
type following struct {
	previous, next *followedConfig
	moved          map[string]bool
	collectors     bool
	removed, stale map[string]bool
}

// planFollowing works out the following of cfg, with the static target file
// targets in force with it, from previous, the configuration followed, which
// is another. It takes no lock and changes nothing the server keeps: it reads
// the two configurations, which never change once loaded, and what previous
// says, which does not either; the fingerprints it has worked out are
// remembered in memo for the probes of cfg, which key their results by them,
// and in next for the reload after this one. So it is here that the time of
// a following goes, and several callers may be at it at once: they share the
// fingerprints, each worked out once, and for a reload before its
// configuration was in force (prepareReload), which leaves the maps to make
// here.
func planFollowing(previous *followedConfig, cfg *model.Config, targets *model.StaticTargetFile, memo *fingerprintMemo) *following {
	next := &followedConfig{config: cfg, targets: targets, generation: firstGeneration}
	if previous != nil {
		next.generation = previous.generation + 1
	}
	plan := &following{previous: previous, next: next}
	next.targetsDefined = targetsDefinedFrom(previous, targets, next.generation)
	plan.moved = targetsMoved(previous, next.targetsDefined)
	if previous != nil && previous.config == cfg {
		// Another static target file with the configuration followed: the
		// collectors are as they were, and nothing kept for them changes.
		next.defined, next.fingerprints = previous.defined, previous.fingerprints
		return plan
	}
	plan.collectors = true
	var collectors []model.Collector
	if cfg != nil {
		collectors = cfg.Collectors
		next.fingerprints = memo.of(cfg)
	}
	next.defined = make(map[string]uint64, len(collectors))
	if previous == nil || previous.config == nil {
		for i := range collectors {
			next.defined[collectors[i].Name] = next.generation
		}
		return plan
	}
	current := make(map[string]string, len(collectors))
	for i := range collectors {
		current[collectors[i].Name] = next.fingerprints.at(i)
	}
	plan.removed, plan.stale = map[string]bool{}, map[string]bool{}
	for i := range previous.config.Collectors {
		c := &previous.config.Collectors[i]
		fingerprint, kept := current[c.Name]
		switch {
		case !kept:
			plan.removed[c.Name] = true
			plan.stale[c.Name] = true
		case fingerprint != previous.fingerprint(i):
			plan.stale[c.Name] = true
		default:
			// Unchanged, it is defined as it has been.
			next.defined[c.Name] = previous.defined[c.Name]
		}
	}
	// The definition of a collector that is new, back or changed has been
	// there from this generation.
	for i := range collectors {
		if _, unchanged := next.defined[collectors[i].Name]; !unchanged {
			next.defined[collectors[i].Name] = next.generation
		}
	}
	return plan
}

// followLocked makes the following plan, under statsMu, which the caller
// took once plan was worked out and found plan.previous still followed and
// plan.next still in force: it makes plan.next the configuration the
// per-collector state follows, and returns it with its generation. All that
// a following changes is changed under the lock, and the followed
// configuration is replaced last: a caller that finds the configuration
// followed finds the state of the collectors a reload removed dropped, and
// one that takes statistics meanwhile waits, for no longer than dropping and
// replacing takes: nothing is compared or encoded here. When there are
// cached results or remembered failures to drop, it is replaced under the
// locks of the cache and of the failure log, with the drop: a trip that
// asks under either lock whether its collector stands (configRead) is
// answered by the configuration followed before the drop, and what it wrote
// is dropped, or by this one, and it writes nothing. So it is when only
// static targets left their stay (targetsMoved): their remembered failures
// are forgotten, and the followed configuration replaced, under the lock of
// the failure log. report, when it is set, is given what there is to log
// once the lock is released.
func (s *Server) followLocked(plan *following, report *func()) *followedConfig {
	next, moved := plan.next, plan.moved
	if !plan.collectors {
		s.storeFollowedForgetting(next, moved)
		return next
	}
	if s.since == nil {
		s.since = map[string]uint64{}
	}
	var collectors []model.Collector
	if next.config != nil {
		collectors = next.config.Collectors
	}
	if plan.previous == nil || plan.previous.config == nil {
		for i := range collectors {
			s.since[collectors[i].Name] = next.generation
		}
		s.storeFollowed(next)
		return next
	}
	removed, stale := plan.removed, plan.stale
	for name := range removed {
		// The histogram goes with the statistics it is part of. A trip
		// still under way holds them and goes on counting in them, where
		// nothing shows. They are retired before the collector's requests
		// are forgotten, so that such a trip cannot start a request of
		// the collector being tracked afterwards (requestTracker.adopt).
		if stats := s.stats[name]; stats != nil {
			stats.retired.Store(true)
		}
		delete(s.stats, name)
		delete(s.since, name)
	}
	// A collector that is new, or back, has been there from this generation.
	for i := range collectors {
		if _, known := s.since[collectors[i].Name]; !known {
			s.since[collectors[i].Name] = next.generation
		}
	}
	if len(stale) == 0 {
		s.storeFollowedForgetting(next, moved)
		return next
	}
	if len(removed) > 0 {
		s.requests.forgetCollectors(removed)
	}
	s.failures.mu.Lock()
	s.cache.mu.Lock()
	// The failures of a changed collector are forgotten with those of a
	// removed one: they are the old definition's.
	s.failures.forgetCollectorsLocked(stale)
	s.failures.forgetStaticTargetsLocked(moved)
	dropped := s.cache.dropCollectorsLocked(stale)
	s.storeFollowed(next)
	s.cache.mu.Unlock()
	s.failures.mu.Unlock()
	if report != nil {
		*report = func() {
			s.logger.Debug("per-collector state follows the reloaded configuration", "removed", model.SortedKeys(removed), "changed", len(stale)-len(removed), "cache_entries_dropped", dropped)
		}
	}
	return next
}

// storeFollowed makes next the followed configuration, under the lock of the
// static targets' results: a scrape that asks under it whether its target
// stands (targetStands) is answered by the configuration followed before, and
// publishes before any scrape that read this one has begun, or by this one.
// statsMu is held, and the locks of the failure log and of the cache when
// there is something of theirs to drop; the lock taken here is the innermost,
// and nothing is locked under it.
func (s *Server) storeFollowed(next *followedConfig) {
	s.staticMu.Lock()
	s.followed.Store(next)
	s.staticMu.Unlock()
}

// storeFollowedForgetting is storeFollowed for a reload that retires no
// collector: when static targets left their stay, moved, their remembered
// failures are forgotten and next is made the followed configuration under
// the lock of the failure log, so that a scrape that asks under it whether
// its target stands (logStands) wrote before the failures were forgotten, or
// writes nothing. statsMu is held; the failure log's lock is taken before
// that of the targets' results, as where there are collectors to drop.
func (s *Server) storeFollowedForgetting(next *followedConfig, moved map[string]bool) {
	if len(moved) == 0 {
		s.storeFollowed(next)
		return
	}
	s.failures.mu.Lock()
	s.failures.forgetStaticTargetsLocked(moved)
	s.storeFollowed(next)
	s.failures.mu.Unlock()
}

// targetsMoved names the static targets of the file followed before,
// previous, that are not in the files followed now as they were there:
// defined says from when each target now in force has been as it is, so a
// target moved when a reload removed it, or changed it. One that is new has
// nothing remembered of it: it is not named. nil when none moved.
func targetsMoved(previous *followedConfig, defined map[string]uint64) map[string]bool {
	if previous == nil {
		return nil
	}
	var moved map[string]bool
	for name, since := range previous.targetsDefined {
		if defined[name] != since {
			if moved == nil {
				moved = map[string]bool{}
			}
			moved[name] = true
		}
	}
	return moved
}

// targetsDefinedFrom says, for each static target of file, the generation it
// has been in the followed files from as it is: that of the file followed
// before, previous, for a target that was there with the same definition,
// and generation for one that is new, changed or back. What makes a target
// another is what starts it again in the schedule (targetSchedule.plan),
// but for its collector's definition, which a scrape asks about apart
// (targetStands).
func targetsDefinedFrom(previous *followedConfig, file *model.StaticTargetFile, generation uint64) map[string]uint64 {
	if previous != nil && previous.targets == file {
		return previous.targetsDefined
	}
	targets := staticTargetsOf(file)
	if len(targets) == 0 {
		return nil
	}
	var former map[string]*model.StaticTarget
	if previous != nil {
		was := staticTargetsOf(previous.targets)
		former = make(map[string]*model.StaticTarget, len(was))
		for i := range was {
			former[was[i].Name] = &was[i]
		}
	}
	defined := make(map[string]uint64, len(targets))
	for i := range targets {
		defined[targets[i].Name] = generation
		if was := former[targets[i].Name]; was != nil && reflect.DeepEqual(*was, targets[i]) {
			defined[targets[i].Name] = previous.targetsDefined[targets[i].Name]
		}
	}
	return defined
}

// dropCollectors removes the cached results of the named collectors, and
// reports how many.
func (c *responseCache) dropCollectors(names map[string]bool) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dropCollectorsLocked(names)
}

// dropCollectorsLocked is dropCollectors under the cache's lock.
func (c *responseCache) dropCollectorsLocked(names map[string]bool) int {
	dropped := 0
	for name := range names {
		dropped += c.dropCollectorLocked(name)
	}
	return dropped
}

// forgetCollectors drops the per-request series of the named collectors.
func (t *requestTracker) forgetCollectors(names map[string]bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for key := range t.stats {
		if names[key.Collector] {
			t.dropLocked(key)
		}
	}
	t.capReached = len(t.stats) >= VerboseRequestSeriesLimit
}
