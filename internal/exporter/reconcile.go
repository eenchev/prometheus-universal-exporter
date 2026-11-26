package exporter

import (
	"sync/atomic"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// The exporter keeps state per collector: its self-metric counters, the
// verbose per-request series and scrape-time histogram, cached results,
// what the failure log remembers, the recent probes on the status page
// (recentprobes.go), the targets that failure_backoff is backing off from,
// and the statistics of its Python workers (pythonstats.go). A reload can
// remove a collector or change its definition, and that state follows:
//
//   - A removed collector's self-metric series stop being exposed and
//     exported, so Prometheus marks them stale instead of showing frozen
//     values. Everything kept about it is dropped, and a collector added again
//     under the name starts from zero.
//   - A changed collector keeps its counters, which describe it by name. Its
//     cached results are dropped: their keys carry the old definition, so they
//     could never be served again. What the failure log remembers of it is
//     forgotten, so the new definition's first failure is logged as a first.
//     The targets it was backing off from are forgotten too. A probe's
//     failures are already told apart by the definition (probeCacheKey); a
//     static target's are not.
//
// The configuration manager tells the server, on the reloading goroutine,
// when it has put something in force (config.Manager's OnInstall,
// followReload). The slow part, the collectors' fingerprints, is computed
// before the configuration is in force (OnPrepare, prepareReload), so
// nothing that uses the configuration waits for it.
//
// The state does not depend on that notification. Before using its
// per-collector state, the server compares the configuration it last saw with
// the current one (a pointer comparison while nothing has changed) and
// follows any change it was not told about. Two reloads followed only this
// way are followed as one: a collector removed by the first and restored
// unchanged by the second is one that stayed.
//
// A probe or a static target scrape reads its collector in one configuration
// and counts some time later. By then a reload may have removed the collector
// and another restored one under the name. So the followed configurations
// are numbered, and a probe or scrape carries the one it read (statsSince).
// It counts in the collector's statistics only while the collector has been
// in every configuration followed since.
//
// What it writes under the collector's name when it ends (its cached result,
// and what the failure log remembers) must pass a stricter test. It belongs
// to the collector only while the collector has been in every configuration
// followed since, with the definition the probe or scrape read (configRead).
// Otherwise a result of a removed definition would be served to a collector
// restored under the name, and one of a changed definition would only count
// in max_cache_entries. A failure of either would pass for the collector's
// own first.
//
// A static target's scrape reads its target with its collector, and the
// result it publishes must pass both tests. The collector must stand as
// above, and the target must have been in every static target file followed
// since, as the scrape read it (targetStands). So the static target file in
// force is followed with the configuration, and a reload of either makes a
// generation. The failure log's memory of the scrape must pass both tests
// too (logStands). The remembered failures of a target that a reload removed
// or changed are forgotten, because the failure log tells a static target's
// failures apart by its name, not its definition.

// followedConfig is a configuration that the per-collector state has
// followed. It holds the static target file in force with it (nil if none)
// and its generation, one more than the previous one's, from
// firstGeneration. defined gives, for each collector, the generation since
// which it has had its definition; targetsDefined gives the same for each
// static target. Neither changes once followed. fingerprints are those of
// the configuration's collectors, computed once. The next reload uses them
// to find the collectors it changed, and the static target schedule to find
// the targets whose collector changed (fingerprintOf). They also hold each
// collector's position by name, which is how a probe and a static target
// scrape find their collector (collectorOf).
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

// fingerprintOf returns the fingerprint of cfg's collector named name, or
// empty when cfg has none: collectorFingerprint of the first collector with
// that name. It is for a caller that has cfg and f together, as the static
// target schedule does (planFollowed). When cfg is the configuration f
// followed, it returns the kept fingerprint, which the reload computed before
// putting cfg in force (prepareReload). A fingerprint nothing had requested
// yet, as for the configuration the exporter started with, is computed here
// once and kept. For any other configuration, and for a caller that follows
// none (f nil), the definition is encoded afresh. f never changes once
// followed, so a concurrent reload cannot swap the fingerprints. It takes no
// lock, and waits only when a probe or a reload is computing the same
// fingerprint (fingerprintGeneration.at).
//
// The collector is looked up by name where the fingerprints are kept
// (fingerprintGeneration.place), instead of by scanning cfg's collectors.
// The schedule would otherwise scan for every collector with a static target
// after every reload. For another configuration, or a caller that follows
// none, the collectors are scanned (placeByScan). The scrape loop is never
// such a caller, and encoding the definition costs far more than the scan.
func (f *followedConfig) fingerprintOf(cfg *model.Config, name string) string {
	if kept := f.keptFor(cfg); kept != nil {
		if index, found := kept.place(name); found {
			return kept.at(index)
		}
		return ""
	}
	if index := placeByScan(cfg, name); index >= 0 {
		return collectorFingerprint(&cfg.Collectors[index])
	}
	return ""
}

// keptFor returns the fingerprints kept for cfg, with the places of its
// collectors, when cfg is the configuration f followed and f keeps them. It
// returns nil for any other configuration, for a caller that follows none
// (f nil), and when none are kept.
func (f *followedConfig) keptFor(cfg *model.Config) *fingerprintGeneration {
	if f != nil && f.config == cfg && f.fingerprints != nil && f.fingerprints.config == cfg {
		return f.fingerprints
	}
	return nil
}

// collectorOf returns cfg's collector named name, or nil when cfg has none:
// model.CollectorByName(cfg, name), the first collector with that name. A
// probe and a static target scrape use it to find their collector. When cfg
// is the configuration f followed, the collector is looked up by name where
// the fingerprints are kept, as in fingerprintOf, so a probe or scrape does
// not scan the whole configuration. For any other configuration, such as one
// a scrape read before a reload that came while it waited for a slot, the
// collectors are scanned.
func (f *followedConfig) collectorOf(cfg *model.Config, name string) *model.Collector {
	if kept := f.keptFor(cfg); kept != nil {
		if index, found := kept.place(name); found {
			return &cfg.Collectors[index]
		}
		return nil
	}
	if index := placeByScan(cfg, name); index >= 0 {
		return &cfg.Collectors[index]
	}
	return nil
}

// When a static target's scrape ends, it asks whether a target with its name
// is in force (staticTargetInForce). A read of the static targets endpoint
// that names targets asks this for each name (requestedStaticTargets).
// Scanning the file's targets for this costs as much as the file is large,
// at every scrape and read: for 10,000 targets, more than the rest of a
// scrape answered from the cache. Following a file already records each
// target's name once, with the generation since which it has been there
// (targetsDefinedFrom), and keeps the names while the file is followed. So a
// name is looked up there, and nothing more is kept or computed.

// targetsScannedHook, set by tests, is called whenever the targets of a
// static target file are scanned, to find a name (staticTargetNamedByScan,
// staticTargetNamesByScan) or to record the names (targetsDefinedFrom): once
// per scan, not per target, so a test can check that it happens once per
// file, not once per scrape or read.
var targetsScannedHook atomic.Pointer[func()]

// targetsScanned tells a test that the targets of a static target file are
// about to be gone through.
func targetsScanned() {
	if hook := targetsScannedHook.Load(); hook != nil {
		(*hook)()
	}
}

// targetNames returns the names of file's static targets as f recorded them
// when it was followed, and whether it recorded them. f recorded them only
// for the file it follows: every name a target of that file has
// (targetsDefined), so a name is present exactly when scanning file finds a
// target with that name. For any other file (such as one a reload has put in
// force that nothing has followed yet) and for a caller that follows none
// (f nil), nothing is recorded and the caller scans the targets. f never
// changes once followed, so the names are read without a lock.
func (f *followedConfig) targetNames(file *model.StaticTargetFile) (names map[string]uint64, noted bool) {
	if f == nil || f.targets != file {
		return nil, false
	}
	return f.targetsDefined, true
}

// configRead is the configuration in which a probe or a static target scrape
// read its collector: the generation it was followed at, and where the
// configuration followed now is found. target is the static target, by
// name, that a scrape read with its collector; it is nil for a probe, which
// reads none. The zero value is that of a caller that names no
// configuration, whose collector always stands.
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
// same definition. It is the test the statistics make by generation
// (statsSince); a changed definition fails it like a removed collector. A
// trip calls it under the lock of what it writes to. followLocked holds that
// lock while it drops what the reload retires and replaces the followed
// configuration, so nothing the drop was meant to remove is written after
// it. It costs a writer one atomic load and a map lookup, under a lock it
// holds anyway.
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
// publishes. It reports whether the collector stands and the static target
// name is still the one r read, unchanged in every static target file
// followed since. A target that a reload removed, changed or restored since
// is another target under the name, and its own scrape publishes its
// result. The scrape calls it under the lock of the targets' results, under
// which the followed configuration is replaced (storeFollowed).
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

// logStands is the test applied to what a trip tells the failure log. It is
// stands for a probe, and targetStands for a static target's scrape, whose
// failures are remembered under the target's name. Without it, a scrape that
// read its target (and the collector as it was) before a reload of the
// static target file changed it would have its failure remembered as one of
// the target now under the name, and its success logged as that target's
// recovery.
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
// force. It returns that configuration with its generation, for the caller
// to use together: the collectors it reads there are the ones whose
// statistics the generation names.
//
// The work is planned before the statistics lock is taken (planFollowing)
// and only carried out under it (followLocked). Finding the collectors a
// reload changed encodes all of them, which takes as long as the
// configuration is large, and every probe takes that lock to find its
// collector's statistics. The lock is then taken only to carry out a plan
// that is still current: for the configuration and file in force, and from
// the configuration followed. If a reload, or another caller's following,
// came in between, the plan is made again from what is followed now, or
// found done. So several callers (a reload, and the probes that arrive
// meanwhile) may plan the same following at once. They share the
// fingerprints, one carries it out, and the others find it done. The
// fingerprints of a configuration a reload put in force were computed
// earlier (prepareReload), so none of them waits. Only a configuration put
// in force without the manager's reload has them computed here.
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
			s.applyFollowed(followed)
			return followed
		}
		previous, cfg, targets = followed, inForce, file
	}
}

// applyFollowed tells the request types what followed holds. Types that keep
// connections across scrapes close the ones no collector in force still
// uses. The entries of rate_limits are followed, keeping the buckets of
// entries a reload left unchanged. It runs after statsMu (which every probe
// takes) is released, so two callers that each made a following may arrive
// in either order. One that followed an older generation and arrived last
// would restore that generation's connections and rate limits until the next
// reload. So only a generation later than the last applied one is applied,
// under a dedicated lock.
func (s *Server) applyFollowed(followed *followedConfig) {
	if hook := followAppliesHook.Load(); hook != nil {
		(*hook)(followed.generation)
	}
	s.appliedMu.Lock()
	defer s.appliedMu.Unlock()
	if followed.generation <= s.appliedGeneration {
		return
	}
	s.appliedGeneration = followed.generation
	var collectors []model.Collector
	var hostRateLimits []model.HostRateLimit
	var secretStores []model.SecretStore
	if followed.config != nil {
		collectors = followed.config.Collectors
		hostRateLimits = followed.config.RateLimits
		secretStores = followed.config.SecretStores
	}
	fetch.FollowConfiguration(collectors, staticTargetsOf(followed.targets))
	fetch.FollowHostRateLimits(hostRateLimits)
	fetch.FollowSecretStores(secretStores)
	transform.FollowPythonStates(collectors)
}

// followPlannedHook, set by tests, is called when a following has been
// worked out and the statistics lock is not yet taken to make it, so a test
// can put a probe, or another reload, between the two.
var followPlannedHook atomic.Pointer[func()]

// followAppliesHook, set by tests, is called with the generation of a
// following made, once statsMu is released and before the request types
// are told of it (applyFollowed), so a test can make a later following
// meanwhile.
var followAppliesHook atomic.Pointer[func(generation uint64)]

// prepareReload computes the slow part of following a configuration that a
// reload is about to put in force, the part that takes as long as the
// configuration is large. That is the fingerprint of every collector of cfg,
// and of the collectors of the followed configuration that cfg still names
// and that nothing has requested yet, which the following compares them
// with. The configuration manager calls it on the reloading goroutine, once
// the reload has read and checked cfg and before cfg is in force
// (config.Manager's OnPrepare). Probes of the configuration still in force
// continue meanwhile. Once cfg is in force, whoever follows it first (the
// reload via followReload, or a probe via reconcile) finds every
// fingerprint computed.
//
// Only the fingerprints are prepared. They belong to a configuration alone,
// which never changes once loaded, so they cannot be stale. The plan is not
// prepared, because it depends on the followed configuration, which may have
// changed if something other than a reload put one in force. With the
// fingerprints computed, the plan costs no more than its maps
// (planFollowing). A static target file reloaded alone leaves the followed
// configuration and its kept fingerprints unchanged, so nothing is prepared.
//
// It takes no lock, and waits only for a probe that is computing one of the
// same fingerprints.
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

// followReload follows a reload as it is made. The configuration manager
// calls it when it has put a configuration or a static target file in force
// (config.Manager's OnInstall), so the per-collector state matches what is in
// force from then on, and anything a probe or scrape of a retired collector
// writes afterwards is discarded. The reload waits for it. A probe does not,
// unless it requests the state meanwhile, in which case it follows the reload
// itself. Neither holds a lock the other needs while planning (reconcile),
// and neither waits for a fingerprint: all were computed before the
// configuration was in force (prepareReload).
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

// stay returns the generation at which the stay of the static target named
// target, of the collector named collector, began. That is the later of the
// generation since which the target has been unchanged in the followed
// files and the one since which the collector has had its definition. It is
// the earliest a scrape of the target can have read and still publish
// (targetStands), so the schedule gives a target a first scrape whenever it
// moves (planFollowed). A caller that follows no configuration (f nil) knows
// of no stay.
func (f *followedConfig) stay(target, collector string) uint64 {
	if f == nil {
		return noGeneration
	}
	return max(f.targetsDefined[target], f.defined[collector])
}

// following is the plan for following a reload, made before the statistics
// lock is taken (planFollowing). next is the configuration to follow, and
// previous is the one followed when the plan was made. moved names the
// static targets that left their stay (targetsMoved). collectors reports
// that next has a different configuration from previous, whose collectors
// are then scanned. removed names the collectors of previous that next no
// longer has, and stale names those plus the ones whose definition changed.
// Neither is set when previous followed no configuration.
type following struct {
	previous, next *followedConfig
	moved          map[string]bool
	collectors     bool
	removed, stale map[string]bool
}

// planFollowing plans the following of cfg, with the static target file
// targets in force with it, from previous, the followed configuration, which
// differs. It takes no lock and changes nothing the server keeps: it only
// reads the two configurations and previous, none of which change. The
// fingerprints it computes are remembered in memo for the probes of cfg,
// which key their results by them, and in next for the next reload. This is
// where a following spends its time, and several callers may be here at
// once. They share the fingerprints, each computed once. For a reload they
// were computed before cfg was in force (prepareReload), which leaves only
// the maps to build.
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

// followLocked carries out plan under statsMu. The caller took the lock after
// making plan and finding plan.previous still followed and plan.next still in
// force. It makes plan.next the configuration the per-collector state
// follows, and returns it with its generation.
//
// Everything a following changes is changed under the lock, and the followed
// configuration is replaced last. So a caller that finds the configuration
// followed also finds the state of removed collectors dropped. A caller that
// takes statistics meanwhile waits only for the drop and the replacement:
// nothing is compared or encoded here.
//
// When there are cached results or remembered failures to drop, the
// configuration is replaced together with the drop, under the locks of the
// cache and of the failure log. A trip that asks under either lock whether
// its collector stands (configRead) is answered in one of two ways. Either
// by the previous configuration, and what it wrote is dropped, or by this
// one, and it writes nothing. The same applies when only static targets left
// their stay (targetsMoved): their remembered failures are forgotten, and the
// followed configuration replaced, under the failure log's lock. report,
// when set, receives what there is to log once the lock is released.
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
	if len(removed) > 0 {
		// The statistics of the collector's Python workers go with its
		// own, under the lock a trip takes both under (pythonstats.go).
		transform.PythonWorkers().Retire(removed)
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
	s.backoffs.mu.Lock()
	// The failures of a changed collector are forgotten with those of a
	// removed one, since they are the old definition's. So are the targets
	// it was backing off from (backoff.go). This happens under the same lock
	// a trip records its outcome under once it has asked whether its
	// collector stands. So what a trip of the old definition records is
	// either in before they are forgotten, or not recorded at all.
	s.failures.forgetCollectorsLocked(stale)
	s.failures.forgetStaticTargetsLocked(moved)
	dropped := s.cache.dropCollectorsLocked(stale)
	s.backoffs.forgetLocked(stale)
	// The last trips the status page draws are the old definitions'.
	s.chains.forget(stale)
	// The recent probes of the status page (recentprobes.go) go with them,
	// under the lock a probe records under once it has asked whether its
	// collector stands.
	s.recent.mu.Lock()
	s.recent.forgetLocked(stale)
	s.storeFollowed(next)
	s.recent.mu.Unlock()
	s.backoffs.mu.Unlock()
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
// static targets' results. A scrape that asks under this lock whether its
// target stands (targetStands) is answered either by the previous
// configuration, and publishes before any scrape that read this one has
// begun, or by this one. statsMu is held, and so are the locks of the failure
// log and the cache when there is something of theirs to drop. The lock taken
// here is the innermost, and nothing is locked under it.
//
// When next follows a different static target file, what is kept of each
// target it no longer has (its result, its last success and its status page
// record) is forgotten under the same lock. Nothing else forgets them except
// a read of the static targets endpoint. That read may never happen for an
// exporter whose targets are exported only over OTLP, or that the allocator
// gives targets that move between replicas. A target restored under its name
// would then show the removed one's last scrape, error and failures. A
// scrape of a removed target that ends after this does not stand
// (targetStands), so it records nothing, and the restored target starts
// with none.
func (s *Server) storeFollowed(next *followedConfig) {
	s.staticMu.Lock()
	if previous := s.followed.Load(); previous == nil || previous.targets != next.targets {
		s.forgetStaticTargetsGoneLocked(next.targetsDefined)
	}
	s.followed.Store(next)
	s.staticMu.Unlock()
}

// forgetStaticTargetsGoneLocked forgets the result, last success and status
// record of every static target not named in defined, the targets of the
// file followed next. staticMu is held.
func (s *Server) forgetStaticTargetsGoneLocked(defined map[string]uint64) {
	for name := range s.staticResults {
		if _, kept := defined[name]; !kept {
			delete(s.staticResults, name)
			delete(s.staticFetched, name)
			// The endpoint's merge holds the result: it is made again.
			s.staticGeneration++
		}
	}
	for name := range s.staticLastSuccess {
		if _, kept := defined[name]; !kept {
			delete(s.staticLastSuccess, name)
		}
	}
	for name := range s.staticStatus {
		if _, kept := defined[name]; !kept {
			delete(s.staticStatus, name)
		}
	}
}

// storeFollowedForgetting is storeFollowed for a reload that retires no
// collector. When static targets left their stay (moved), their remembered
// failures are forgotten and next becomes the followed configuration under
// the failure log's lock. So a scrape that asks under that lock whether its
// target stands (logStands) either wrote before the failures were forgotten
// or writes nothing. statsMu is held. The failure log's lock is taken before
// the lock of the targets' results, as when there are collectors to drop.
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

// targetsMoved names the static targets of previous, the file followed
// before, that are not in the files followed now as they were there. defined
// says since when each target now in force has been as it is, so a target
// has moved when a reload removed or changed it. A new target has nothing
// remembered and is not named. It returns nil when none moved.
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

// targetsDefinedFrom gives, for each static target of file, the generation
// since which it has been unchanged in the followed files: the value in
// previous for a target that was there with the same definition, and
// generation for one that is new, changed or back. What counts as a
// different target is what restarts it in the schedule (targetSchedule.plan),
// except for its collector's definition, which a scrape checks separately
// (targetStands).
// The names recorded here are also how a target is recognised as one of the
// file (targetNames): every target of file is recorded, and nothing else.
func targetsDefinedFrom(previous *followedConfig, file *model.StaticTargetFile, generation uint64) map[string]uint64 {
	if previous != nil && previous.targets == file {
		return previous.targetsDefined
	}
	targets := staticTargetsOf(file)
	if len(targets) == 0 {
		return nil
	}
	targetsScanned()
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
		if was := former[targets[i].Name]; was != nil && model.SameStaticTarget(*was, targets[i]) {
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
