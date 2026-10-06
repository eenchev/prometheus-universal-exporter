//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// What following a reload is to do is worked out before the statistics lock
// is taken, and only done under it (reconcile.go): the tests here hold that
// it does what it did when all of it was done under the lock, that the lock
// is not held while a collector's definition is encoded, and what becomes of
// a following another caller, or another reload, overtakes.

// unplannedFollowLocked is followLocked as it was when the whole of a
// following was done under statsMu: the fingerprints of the collectors of
// both configurations worked out there, those of the configuration followed
// afresh at every reload.
func unplannedFollowLocked(s *Server, cfg *model.Config, targets *model.StaticTargetFile, report *func()) *followedConfig {
	previous := s.followed.Load()
	if previous != nil && previous.config == cfg && previous.targets == targets {
		return previous
	}
	next := &followedConfig{config: cfg, targets: targets, generation: firstGeneration}
	if previous != nil {
		next.generation = previous.generation + 1
	}
	next.targetsDefined = targetsDefinedFrom(previous, targets, next.generation)
	moved := targetsMoved(previous, next.targetsDefined)
	if previous != nil && previous.config == cfg {
		next.defined = previous.defined
		s.storeFollowedForgetting(next, moved)
		return next
	}
	if s.since == nil {
		s.since = map[string]uint64{}
	}
	var collectors []model.Collector
	if cfg != nil {
		collectors = cfg.Collectors
	}
	next.defined = make(map[string]uint64, len(collectors))
	if previous == nil || previous.config == nil {
		for i := range collectors {
			s.since[collectors[i].Name] = next.generation
			next.defined[collectors[i].Name] = next.generation
		}
		s.storeFollowed(next)
		return next
	}
	current := make(map[string]string, len(collectors))
	for i := range collectors {
		current[collectors[i].Name] = s.fingerprints.fingerprint(cfg, &collectors[i])
	}
	removed, stale := map[string]bool{}, map[string]bool{}
	for i := range previous.config.Collectors {
		c := &previous.config.Collectors[i]
		fingerprint, kept := current[c.Name]
		switch {
		case !kept:
			removed[c.Name] = true
			stale[c.Name] = true
		case fingerprint != collectorFingerprint(c):
			stale[c.Name] = true
		default:
			next.defined[c.Name] = previous.defined[c.Name]
		}
	}
	for name := range removed {
		if stats := s.stats[name]; stats != nil {
			stats.retired.Store(true)
		}
		delete(s.stats, name)
		delete(s.since, name)
	}
	for i := range collectors {
		if _, known := s.since[collectors[i].Name]; !known {
			s.since[collectors[i].Name] = next.generation
		}
		if _, unchanged := next.defined[collectors[i].Name]; !unchanged {
			next.defined[collectors[i].Name] = next.generation
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

// unplannedReconcile is reconcile as it was with unplannedFollowLocked.
func unplannedReconcile(s *Server) *followedConfig {
	cfg, targets := s.manager.InForce()
	if followed := s.followed.Load(); followed != nil && followed.config == cfg && followed.targets == targets {
		return followed
	}
	var report func()
	s.statsMu.Lock()
	cfg, targets = s.manager.InForce()
	followed := unplannedFollowLocked(s, cfg, targets, &report)
	s.statsMu.Unlock()
	if report != nil {
		report()
	}
	return followed
}

// followTarget is a target that answers value=42, or 500 while failing is
// set, and counts the requests it got. A request that finds a gate waits
// there until the gate is released, having said that it arrived: so a test
// holds a probe at its target.
type followTarget struct {
	*httptest.Server
	requests atomic.Int64
	failing  atomic.Bool
	gate     atomic.Pointer[followGate]
}

type followGate struct{ arrived, release chan struct{} }

func newFollowTarget(t *testing.T) *followTarget {
	t.Helper()
	target := &followTarget{}
	target.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		target.requests.Add(1)
		if gate := target.gate.Swap(nil); gate != nil {
			close(gate.arrived)
			<-gate.release
		}
		if target.failing.Load() {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	t.Cleanup(target.Close)
	return target
}

// followCollectors are the collectors a generated configuration has some of,
// and followStatics the static targets a generated file has some of, the
// first always.
var (
	followCollectors = []string{"a", "b", "c", "d", "e"}
	followStatics    = []string{"s1", "s2", "s3", "s4"}
)

// followStatic is a static target of a generated file: its collector, and
// which of its definitions it has.
type followStatic struct {
	collector string
	variant   int
}

// followCase is a generated configuration, and the static target file with
// it: which of its definitions each collector there has, and each static
// target.
type followCase struct {
	collectors map[string]int
	statics    map[string]followStatic
}

// document is the configuration file of c, with verbose self-metrics or
// without. The collectors a and b keep their results under cache.ttl and c
// under cache.stale_if_error, so a reload has cached results to drop; a
// definition differs from another of the collector in its metric's name.
func (c followCase) document(verbose bool) string {
	var b strings.Builder
	if verbose {
		b.WriteString("web:\n  self_metrics:\n    verbose: true\n")
	}
	b.WriteString("collectors:\n")
	for _, name := range followCollectors {
		variant, there := c.collectors[name]
		if !there {
			continue
		}
		collector := strings.Replace(testutil.CollectorYAML(name), name+"_value\n", fmt.Sprintf("%s_value_%d\n", name, variant), 1)
		switch name {
		case "a", "b":
			collector = strings.Replace(collector, "    transform:\n", "    cache:\n      ttl: 1h\n    transform:\n", 1)
		case "c":
			collector = strings.Replace(collector, "    transform:\n", "    cache:\n      stale_if_error: 1h\n    transform:\n", 1)
		}
		b.WriteString(collector)
	}
	return b.String()
}

// staticsDocument is the static target file of c, each target at the target
// of its collector; a definition differs from another of the target in a
// label.
func (c followCase) staticsDocument(targets map[string]*followTarget) string {
	var b strings.Builder
	b.WriteString("interval: 1m\ntargets:\n")
	for _, name := range followStatics {
		if static, there := c.statics[name]; there {
			fmt.Fprintf(&b, "  - name: %s\n    collector: %s\n    target: %s\n    labels:\n      definition: \"%d\"\n", name, static.collector, targets[static.collector].URL, static.variant)
		}
	}
	return b.String()
}

// next is the case a generated reload leaves: each collector kept as it is,
// removed, added or changed, and each static target likewise, or given
// another collector; a target whose collector went goes with it, but the
// first, which moves to a collector that is there.
func (c followCase) next(r *rand.Rand) followCase {
	out := followCase{collectors: map[string]int{}, statics: map[string]followStatic{}}
	for _, name := range followCollectors {
		variant, there := c.collectors[name]
		switch r.IntN(8) {
		case 0, 1:
			there, variant = !there, r.IntN(3)
		case 2, 3:
			variant = (variant + 1 + r.IntN(2)) % 3
		}
		if there {
			out.collectors[name] = variant
		}
	}
	if len(out.collectors) == 0 {
		out.collectors["a"] = r.IntN(3)
	}
	present := model.SortedKeys(out.collectors)
	for _, name := range followStatics {
		static, there := c.statics[name]
		switch r.IntN(8) {
		case 0, 1:
			there, static = !there, followStatic{collector: present[r.IntN(len(present))], variant: r.IntN(3)}
		case 2:
			static.variant = (static.variant + 1 + r.IntN(2)) % 3
		case 3:
			static.collector = present[r.IntN(len(present))]
		}
		if _, defined := out.collectors[static.collector]; !defined {
			there = false
		}
		if name == followStatics[0] && !there {
			there, static = true, followStatic{collector: present[r.IntN(len(present))], variant: r.IntN(3)}
		}
		if there {
			out.statics[name] = static
		}
	}
	return out
}

// followRig is a server over a configuration file and a static target file
// the test rewrites, that follows its reloads as it does now, or as it did:
// unplanned, by the following that was (unplannedReconcile).
type followRig struct {
	t         *testing.T
	unplanned bool
	verbose   bool
	static    bool
	dir       string
	manager   *config.Manager
	server    *Server
	targets   map[string]*followTarget
	// logs is what the manager and the server logged (lockedBuffer), and
	// logsRead how much of it logged has returned.
	logs     *lockedBuffer
	logsRead int
}

func newFollowRig(t *testing.T, unplanned, verbose, static bool, targets map[string]*followTarget, first followCase) *followRig {
	t.Helper()
	r := &followRig{t: t, unplanned: unplanned, verbose: verbose, static: static, dir: t.TempDir(), targets: targets, logs: &lockedBuffer{}}
	logger := slog.New(slog.NewJSONHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r.write("config.yaml", first.document(verbose))
	cfg, err := config.Load(r.path("config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	r.manager = config.NewManager(cfg, r.path("config.yaml"), logger)
	r.manager.SetPythonPath("python3")
	if static {
		r.manager.SetTargets(r.path("targets.yaml"), r.staticsFile(first))
	}
	if unplanned {
		// Told before the server is, of every reload: the server then finds
		// the reload followed, and follows nothing itself.
		r.manager.OnInstall(func() { unplannedReconcile(r.server) })
	}
	r.server = NewServer(r.manager, "python3", logger)
	return r
}

func (r *followRig) path(name string) string { return filepath.Join(r.dir, name) }

func (r *followRig) write(name, body string) {
	r.t.Helper()
	if err := os.WriteFile(r.path(name), []byte(body), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

// staticsFile writes the static target file of c and reads it as the
// exporter reads the file it starts with.
func (r *followRig) staticsFile(c followCase) *model.StaticTargetFile {
	r.t.Helper()
	r.write("targets.yaml", c.staticsDocument(r.targets))
	file, err := config.LoadStaticTargets(r.path("targets.yaml"))
	if err == nil {
		err = config.ValidateStaticTargets(file)
	}
	if err == nil {
		err = config.ValidateStaticTargetsAgainst(file, r.manager.Get())
	}
	if err != nil {
		r.t.Fatal(err)
	}
	return file
}

// follow follows what the manager holds and did not tell of.
func (r *followRig) follow() {
	if r.unplanned {
		unplannedReconcile(r.server)
		return
	}
	r.server.reconcile()
}

// reload puts c in force. alone, when the collectors of c are those in
// force, puts its static target file in force alone, without the manager
// telling of it, so that it is followed with the configuration it was
// followed with; otherwise both files are read again, as SIGHUP reads them.
func (r *followRig) reload(c followCase, alone bool) {
	r.t.Helper()
	if alone && r.static {
		r.manager.SetTargets(r.path("targets.yaml"), r.staticsFile(c))
		r.follow()
		return
	}
	r.write("config.yaml", c.document(r.verbose))
	if r.static {
		r.write("targets.yaml", c.staticsDocument(r.targets))
	}
	if err := r.manager.Reload(config.ReloadTriggerSignal); err != nil {
		r.t.Fatal(err)
	}
}

// followVolatile are the attributes of a log line that say how long
// something took, or when: they differ from run to run.
var followVolatile = []string{"time", "duration", "failed_for", "result_age", "failing_since", "elapsed"}

// logged is the log lines written since it was last asked, without what
// differs from run to run.
func (r *followRig) logged() string {
	r.t.Helper()
	var out []string
	all := r.logs.String()
	fresh := all[r.logsRead:]
	r.logsRead = len(all)
	for _, line := range strings.Split(strings.TrimSpace(fresh), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			r.t.Fatalf("the log line %s: %v", line, err)
		}
		for _, name := range followVolatile {
			delete(record, name)
		}
		// Where a file is, differs from rig to rig.
		for name, value := range record {
			if text, ok := value.(string); ok {
				record[name] = strings.ReplaceAll(text, r.dir, "<dir>")
			}
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			r.t.Fatal(err)
		}
		out = append(out, string(encoded))
	}
	return strings.Join(out, "\n")
}

// firstDifference is the first line of got that is not the line of want at
// its place, with the line before it.
func firstDifference(got, want string) string {
	gotLines, wantLines := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i, line := range gotLines {
		if i >= len(wantLines) || line != wantLines[i] {
			return strings.Join(gotLines[max(0, i-1):i+1], "\n")
		}
	}
	return "(nothing more)"
}

// answeredWith is how a probe was answered, without the age of a result, which
// differs from run to run.
func answeredWith(outcome probeOutcome) string {
	var lines []string
	for _, line := range strings.Split(outcome.body, "\n") {
		if !strings.HasPrefix(line, "http_exporter_result_age_seconds") {
			lines = append(lines, line)
		}
	}
	return fmt.Sprintf("%d %q", outcome.code, strings.Join(lines, "\n"))
}

// shown is the server's self-metrics without what differs from run to run:
// the series that measure time or memory, and the runtime's.
func (r *followRig) shown() string {
	r.t.Helper()
	var lines []string
	for _, line := range strings.Split(selfMetrics(r.t, r.server), "\n") {
		name, _, _ := strings.Cut(line, "{")
		name, _, _ = strings.Cut(name, " ")
		if strings.HasPrefix(line, "#") || !strings.HasPrefix(name, "http_exporter_") {
			continue
		}
		timed := strings.Contains(name, "duration") || strings.Contains(name, "timestamp") || strings.Contains(name, "_age") || strings.Contains(name, "uptime") || strings.Contains(name, "start_time")
		if timed && !strings.HasSuffix(name, "_count") {
			continue
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// state is all the server keeps per collector and per static target, and
// what it shows of it: the configuration followed, with the generations of
// its collectors and targets; the statistics, with the stay they are of; the
// requests tracked; the cached results; the failures remembered; the static
// targets' results; the self-metrics; and how many requests each target got.
func (r *followRig) state() string {
	r.t.Helper()
	s := r.server
	var b strings.Builder
	followed := s.followed.Load()
	fmt.Fprintf(&b, "followed: generation %d, the configuration in force %v, defined %v, targets defined %v\n", followed.generation, followed.config == r.manager.Get() && followed.targets == r.manager.StaticTargetFile(), followed.defined, followed.targetsDefined)
	s.statsMu.Lock()
	since := fmt.Sprint(s.since)
	kept := map[string]*serverStats{}
	for name, stats := range s.stats {
		kept[name] = stats
	}
	s.statsMu.Unlock()
	fmt.Fprintf(&b, "since: %s\n", since)
	for _, name := range model.SortedKeys(kept) {
		values := kept[name].snapshot()
		values.lastDuration, values.lastScriptDuration, values.lastScrape, values.created = 0, 0, time.Time{}, time.Time{}
		fmt.Fprintf(&b, "statistics of %s: since %d, retired %v, %+v\n", name, kept[name].since, kept[name].retired.Load(), values)
	}
	s.requests.mu.Lock()
	var requests []string
	for key := range s.requests.stats {
		requests = append(requests, fmt.Sprintf("%+v", key))
	}
	s.requests.mu.Unlock()
	sort.Strings(requests)
	fmt.Fprintf(&b, "requests tracked: %v\n", requests)
	s.cache.mu.Lock()
	var cached []string
	for key, entry := range s.cache.entries {
		cached = append(cached, fmt.Sprintf("%s %q", entry.collector, key))
	}
	for name, entries := range s.cache.byCollector {
		cached = append(cached, fmt.Sprintf("%s holds %d", name, entries.Len()))
	}
	s.cache.mu.Unlock()
	sort.Strings(cached)
	fmt.Fprintf(&b, "cached: %s\n", strings.Join(cached, "; "))
	s.failures.mu.Lock()
	var remembered []string
	for key, st := range s.failures.entries {
		remembered = append(remembered, fmt.Sprintf("%q: stage %s, %q, %d failures, %d suppressed", key, st.stage, st.err, st.failures, st.suppressed))
	}
	rules := fmt.Sprint(s.failures.ruleFailures)
	s.failures.mu.Unlock()
	sort.Strings(remembered)
	fmt.Fprintf(&b, "remembered: %s; of rules %s\n", strings.Join(remembered, "; "), rules)
	s.staticMu.Lock()
	published := model.SortedKeys(s.staticResults)
	succeeded := model.SortedKeys(s.staticLastSuccess)
	s.staticMu.Unlock()
	for _, name := range published {
		// But for the age of the result, which differs from run to run.
		series := slices.DeleteFunc(publishedOf(s, name), func(line string) bool { return strings.HasPrefix(line, "http_exporter_result_age_seconds") })
		fmt.Fprintf(&b, "published for %s: %v\n", name, series)
	}
	fmt.Fprintf(&b, "last successes noted: %v\n", succeeded)
	for _, name := range followCollectors {
		fmt.Fprintf(&b, "the target of %s got %d requests\n", name, r.targets[name].requests.Load())
	}
	fmt.Fprintf(&b, "shown:\n%s\n", r.shown())
	return b.String()
}

// play takes the rig through the reloads the seed generates, with probes
// and static target scrapes held across each, and returns what there was to
// see after each step. Before a reload a probe of one collector goes to its
// target and is held there, a probe of another is held where it has read
// the configuration and not yet taken its statistics, and the configuration
// and two static targets are read for scrapes that begin after the reload.
// Then the reload is made; the held probes go on, and the late scrapes are
// made; and every collector in force is probed twice, and every static
// target scraped, some of the targets failing.
func (r *followRig) play(seed uint64, steps int) []string {
	r.t.Helper()
	for _, target := range r.targets {
		target.requests.Store(0)
		target.failing.Store(false)
	}
	random := rand.New(rand.NewPCG(seed, 1))
	current := followCase{collectors: map[string]int{"a": 0, "b": 0, "c": 0}, statics: map[string]followStatic{"s1": {collector: "a"}, "s2": {collector: "c"}}}
	var transcript []string
	for step := range steps {
		var b strings.Builder
		present := model.SortedKeys(current.collectors)
		atTarget, atStatistics := present[random.IntN(len(present))], present[random.IntN(len(present))]
		gate := &followGate{arrived: make(chan struct{}), release: make(chan struct{})}
		r.targets[atTarget].gate.Store(gate)
		// At an address of its own, so that no cached result answers it.
		inFlight := probeAsync(context.Background(), r.server, probePath(atTarget, fmt.Sprintf("%s/held/%d", r.targets[atTarget].URL, step), ""), nil)
		<-gate.arrived
		goOn := heldProbe(r.t, r.server, atStatistics, r.targets[atStatistics].URL)
		readConfig, readFile, readGeneration := r.server.inForce()
		var late []model.StaticTarget
		if readFile != nil {
			late = readFile.Targets[:min(2, len(readFile.Targets))]
		}

		next, alone := current.next(random), false
		switch random.IntN(4) {
		case 0:
			// Read again as it is: every collector and target stays.
			next = current
		case 1:
			// The static target file alone.
			next.collectors, alone = current.collectors, true
			for name, static := range next.statics {
				if _, defined := next.collectors[static.collector]; !defined {
					static.collector = present[0]
					next.statics[name] = static
				}
			}
		}
		r.reload(next, alone)
		current = next
		fmt.Fprintf(&b, "step %d: %v with %v, the static target file alone %v\n", step, current.collectors, current.statics, alone)

		for _, name := range followCollectors {
			r.targets[name].failing.Store(random.IntN(3) == 0)
		}
		close(gate.release)
		fmt.Fprintf(&b, "the probe of %s held at its target: %s\n", atTarget, answeredWith(<-inFlight))
		fmt.Fprintf(&b, "the probe of %s held before its statistics: %s\n", atStatistics, answeredWith(goOn()))
		for _, target := range late {
			r.server.scrapeTargetSince(context.Background(), readConfig, readGeneration, target)
		}
		for _, name := range model.SortedKeys(current.collectors) {
			for range 2 {
				outcome := probeOnce(r.t, r.server, probePath(name, r.targets[name].URL, ""), nil)
				fmt.Fprintf(&b, "a probe of %s: %s\n", name, answeredWith(probeOutcome{outcome.Code, outcome.Body.String()}))
			}
		}
		cfg, file, generation := r.server.inForce()
		if file != nil {
			for _, target := range file.Targets {
				r.server.scrapeTargetSince(context.Background(), cfg, generation, target)
			}
		}
		b.WriteString(r.state())
		fmt.Fprintf(&b, "logged:\n%s\n", r.logged())
		transcript = append(transcript, b.String())
	}
	return transcript
}

// Following a reload does what it did when all of it was done under the
// statistics lock. Over generated sequences of reloads, each keeping,
// removing, adding or changing collectors and static targets, or the static
// target file alone, or reading everything again as it is, with a probe held
// at its target and another held before it takes its statistics across each
// reload, and static target scrapes that begin after it with what they read
// before: the answers, the statistics, the generations, the requests
// tracked, the cached results, the failures remembered, the static targets'
// results, the self-metrics and the log lines are those of a server that
// follows each reload as it was followed before, with verbose self-metrics
// and without, with a static target file and without. It is 16 sequences of
// 8 reloads, and 4 of 6 under the race detector.
func TestFollowingAReloadWorkedOutAheadDoesWhatItDid(t *testing.T) {
	testutil.CaptureLogs(t)
	targets := map[string]*followTarget{}
	for _, name := range followCollectors {
		targets[name] = newFollowTarget(t)
	}
	sequences, steps := alloctest.UnlessRaced(16, 4), alloctest.UnlessRaced(8, 6)
	first := followCase{collectors: map[string]int{"a": 0, "b": 0, "c": 0}, statics: map[string]followStatic{"s1": {collector: "a"}, "s2": {collector: "c"}}}
	for sequence := range sequences {
		verbose, static := sequence%2 == 0, sequence%4 != 3
		var plans atomic.Int64
		counted := func() { plans.Add(1) }
		followPlannedHook.Store(&counted)
		now := newFollowRig(t, false, verbose, static, targets, first).play(uint64(sequence), steps)
		if plans.Load() < int64(steps) {
			t.Fatalf("sequence %d: %d followings were worked out ahead of the lock in %d reloads, want one for each at least", sequence, plans.Load(), steps)
		}
		plans.Store(0)
		was := newFollowRig(t, true, verbose, static, targets, first).play(uint64(sequence), steps)
		followPlannedHook.Store(nil)
		if plans.Load() != 0 {
			t.Fatalf("sequence %d: the server that follows as it did worked out %d followings ahead of the lock, want none: it is no oracle", sequence, plans.Load())
		}
		for step := range steps {
			if now[step] != was[step] {
				t.Fatalf("sequence %d (verbose %v, static targets %v), step %d: following the reloads leaves, where it first differs,\n%s\nand it left\n%s\nafter\n%s", sequence, verbose, static, step, firstDifference(now[step], was[step]), firstDifference(was[step], now[step]), strings.SplitN(now[step], "\n", 2)[0])
			}
		}
	}
}

// countFingerprints counts, until the test ends, the collectors' definitions
// encoded, and those of them encoded while the statistics lock of the server
// was held: by whom, the count does not say, and in these tests it is the
// reload alone that takes it.
func countFingerprints(t *testing.T, server *Server) (encoded, locked *atomic.Int64) {
	t.Helper()
	encoded, locked = &atomic.Int64{}, &atomic.Int64{}
	hook := func() {
		encoded.Add(1)
		if !server.statsMu.TryLock() {
			locked.Add(1)
			return
		}
		server.statsMu.Unlock()
	}
	fingerprintedHook.Store(&hook)
	t.Cleanup(func() { fingerprintedHook.Store(nil) })
	return encoded, locked
}

// Following a reload encodes no collector's definition while it holds the
// statistics lock, which every probe takes to find its collector's
// statistics, and encodes each collector of the configuration it follows
// once and none of the configuration it had followed, whose fingerprints it
// kept: so a reload of 100 collectors (10 under the race detector) that
// leaves them as they were, changes every one, or removes half and adds as
// many, with a static target file read again or without one, encodes 100
// definitions, where it encoded as many again, all of them under the lock.
// The first reload after the start also encodes the collectors it started
// with that no probe had asked for, those the reload kept: before the lock
// is taken as well.
func TestFollowingAReloadEncodesNoCollectorUnderTheStatisticsLock(t *testing.T) {
	n := alloctest.UnlessRaced(100, 10)
	for _, shape := range followBenchShapes {
		for _, static := range []bool{false, true} {
			where := fmt.Sprintf("%s, a static target file %v", shape.name, static)
			var targets strings.Builder
			if static {
				// Of the collectors every configuration here has.
				targets.WriteString("interval: 1m\ntargets:\n")
				for _, name := range followBenchNames(n/2, n/2) {
					fmt.Fprintf(&targets, "  - name: t_%s\n    collector: %s\n    target: http://127.0.0.1:9/%s\n", name, name, name)
				}
			}
			documents := [2]string{shape.first(n), shape.second(n)}
			r := newReloadable(t, documents[0], targets.String())
			encoded, locked := countFingerprints(t, r.server)
			for reload := 1; reload <= 3; reload++ {
				encoded.Store(0)
				before := r.server.followed.Load().generation
				r.reloadTo(documents[reload%2])
				if followed := r.server.followed.Load(); followed.generation != before+1 || followed.config != r.manager.Get() {
					t.Fatalf("%s: reload %d left generation %d followed, the configuration in force %v, want generation %d of it", where, reload, followed.generation, followed.config == r.manager.Get(), before+1)
				}
				if locked.Load() != 0 {
					t.Fatalf("%s: reload %d encoded %d collectors' definitions while the statistics lock was held, want none", where, reload, locked.Load())
				}
				if got := encoded.Load(); reload > 1 && got != int64(n) {
					t.Errorf("%s: reload %d encoded %d collectors' definitions, want %d, each collector of the new configuration once", where, reload, got, n)
				} else if reload == 1 && got > int64(2*n) {
					t.Errorf("%s: the first reload encoded %d collectors' definitions, want at most %d, each collector of both configurations once", where, got, 2*n)
				}
			}
		}
	}
}

// A static target file reloaded alone, the configuration as it was, encodes
// no collector's definition at all: the collectors are those followed.
func TestFollowingAStaticTargetFileAloneEncodesNoCollector(t *testing.T) {
	testutil.CaptureLogs(t)
	targets := map[string]*followTarget{}
	for _, name := range followCollectors {
		targets[name] = newFollowTarget(t)
	}
	first := followCase{collectors: map[string]int{"a": 0, "b": 0}, statics: map[string]followStatic{"s1": {collector: "a"}}}
	r := newFollowRig(t, false, false, true, targets, first)
	encoded, _ := countFingerprints(t, r.server)
	before := r.server.followed.Load()
	first.statics = map[string]followStatic{"s1": {collector: "b", variant: 1}, "s2": {collector: "a"}}
	r.reload(first, true)
	followed := r.server.followed.Load()
	if followed.generation != before.generation+1 || followed.targets != r.manager.StaticTargetFile() || followed.targetsDefined["s1"] != followed.generation || followed.defined["a"] != before.defined["a"] {
		t.Fatalf("the static target file alone left generation %d followed, its file in force %v, targets defined %v and collectors defined %v", followed.generation, followed.targets == r.manager.StaticTargetFile(), followed.targetsDefined, followed.defined)
	}
	if encoded.Load() != 0 {
		t.Errorf("following the static target file alone encoded %d collectors' definitions, want none", encoded.Load())
	}
}

// holdFirstFollowing holds the first following that is worked out from now
// on where it has been worked out and the statistics lock is not yet taken;
// reached is closed when it is there, and resume lets it go on. worked
// counts the followings worked out, that one and the later ones, which are
// not held.
func holdFirstFollowing(t *testing.T) (reached <-chan struct{}, resume func(), worked *atomic.Int64) {
	t.Helper()
	there, held := make(chan struct{}), make(chan struct{})
	worked = &atomic.Int64{}
	var once sync.Once
	hook := func() {
		if worked.Add(1) == 1 {
			close(there)
			<-held
		}
	}
	followPlannedHook.Store(&hook)
	resume = func() { once.Do(func() { close(held) }) }
	t.Cleanup(func() {
		resume()
		followPlannedHook.Store(nil)
	})
	return there, resume, worked
}

// A reload holds no lock a probe needs while it works out what following it
// is to do: a probe that comes then, and a read of the self-metrics, are
// answered before the reload's following ends. They find the configuration
// in force not yet followed, as a probe always could between the reload
// putting it in force and following it, and follow it themselves, with the
// fingerprints the reload has worked out: the probe is answered by the new
// configuration, the cached results of the collector the reload changed are
// dropped, and the reload then finds its following made, one generation and
// not two.
func TestAProbeIsAnsweredWhileAReloadWorksOutItsFollowing(t *testing.T) {
	target, requests, _ := failableTarget(t)
	r := newReloadable(t, cachedDocument("kept", "changed"), "")
	for _, name := range []string{"kept", "changed"} {
		if outcome := probeOnce(t, r.server, probePath(name, target.URL, ""), nil); outcome.Code != http.StatusOK {
			t.Fatalf("the probe of %s before the reload was answered %d: %s", name, outcome.Code, outcome.Body)
		}
	}
	before := r.server.followed.Load()
	reached, resume, worked := holdFirstFollowing(t)
	r.write(r.path, strings.Replace(cachedDocument("kept", "changed"), "changed_value", "renamed_value", 1))
	reloaded := make(chan error, 1)
	go func() { reloaded <- r.manager.Reload(config.ReloadTriggerSignal) }()
	<-reached
	if r.server.followed.Load() != before || r.manager.Get() == before.config {
		t.Fatal("where the reload has worked out its following, the new configuration is not in force, or is followed already")
	}
	if !r.server.statsMu.TryLock() {
		t.Fatal("the reload holds the statistics lock where it has worked out its following and not yet made it")
	}
	r.server.statsMu.Unlock()
	if outcome := probeOnce(t, r.server, probePath("changed", target.URL, ""), nil); outcome.Code != http.StatusOK || !strings.Contains(outcome.Body.String(), "renamed_value 42") || requests.Load() != 3 {
		t.Fatalf("the probe that came while the reload worked out its following was answered %d after %d requests, want by the new definition, from its target, the third request: %s", outcome.Code, requests.Load(), outcome.Body)
	}
	followed := r.server.followed.Load()
	if followed.config != r.manager.Get() || followed.generation != before.generation+1 || followed.defined["kept"] != before.defined["kept"] || followed.defined["changed"] != followed.generation {
		t.Fatalf("the probe left generation %d followed, the configuration in force %v, collectors defined %v, want generation %d of it, the collector kept as it was and the changed one from then", followed.generation, followed.config == r.manager.Get(), followed.defined, before.generation+1)
	}
	if shown := selfMetrics(t, r.server); !strings.Contains(shown, `http_exporter_scrapes_total{collector="changed"} 2`) || !strings.Contains(shown, `http_exporter_cache_entries{collector="changed"} 1`) || !strings.Contains(shown, `http_exporter_cache_entries{collector="kept"} 1`) {
		t.Fatalf("the self-metrics read while the reload worked out its following do not show the changed collector's two probes, its one cached result, of the new definition, and the kept collector's:\n%s", shown)
	}
	select {
	case err := <-reloaded:
		t.Fatalf("the reload returned (%v) before its following was let go on", err)
	default:
	}
	resume()
	if err := <-reloaded; err != nil {
		t.Fatal(err)
	}
	if after := r.server.followed.Load(); after != followed {
		t.Fatalf("the reload followed again what the probe had followed: generation %d, want %d", after.generation, followed.generation)
	}
	// The reload's own, and the probe's; the read of the self-metrics found
	// the configuration followed.
	if worked.Load() != 2 {
		t.Errorf("%d followings were worked out, want 2: the reload's and the probe's", worked.Load())
	}
	if held := cachedOf(r.server, "kept"); held != 1 {
		t.Errorf("the collector the reload kept has %d cached results, want the 1 it had", held)
	}
}

// A following that another reload overtakes, between its being worked out
// and the lock being taken, is not made: it was of a configuration no longer
// in force. It is worked out again, of the configuration in force then and
// from the one still followed, so the two reloads are followed as one, as
// two with no use of the state between them always were: a collector the
// first removed and the second brought back as it was is one that stayed,
// with its cached result, and one the second changed has none.
func TestAFollowingAnotherReloadOvertakesIsWorkedOutAgain(t *testing.T) {
	testutil.CaptureLogs(t)
	stays, back, changes := cachingCollector("stays", time.Hour), cachingCollector("back", time.Hour), cachingCollector("changes", time.Hour)
	changed := cachingCollector("changes", time.Hour)
	changed.Limits.MaxResponseBytes = 2048
	server, _ := newCacheTestServer(t, stays, back, changes)
	set := model.MetricSet{Metrics: []model.Metric{{Name: "demo_value", Type: model.GaugeMetricType, Value: 1}}}
	for _, name := range []string{"stays", "back", "changes"} {
		server.cache.Put(name+"-result", name, set, time.Hour, 0, 0, time.Now())
	}
	before := server.followed.Load()
	reached, resume, worked := holdFirstFollowing(t)
	reloadTo(t, server, stays, changes)
	first := server.manager.Get()
	done := make(chan *followedConfig, 1)
	go func() { done <- server.reconcile() }()
	<-reached
	reloadTo(t, server, stays, back, changed)
	resume()
	followed := <-done
	if followed != server.followed.Load() || followed.config != server.manager.Get() || followed.config == first || followed.generation != before.generation+1 {
		t.Fatalf("the following left generation %d, of the configuration in force %v, of the one overtaken %v, want generation %d of the one in force", followed.generation, followed.config == server.manager.Get(), followed.config == first, before.generation+1)
	}
	if worked.Load() != 2 {
		t.Errorf("%d followings were worked out, want 2: the one overtaken and the one made", worked.Load())
	}
	for name, want := range map[string]uint64{"stays": before.generation, "back": before.generation, "changes": followed.generation} {
		if followed.defined[name] != want {
			t.Errorf("the collector %s is defined from generation %d, want %d", name, followed.defined[name], want)
		}
	}
	for name, want := range map[string]int{"stays": 1, "back": 1, "changes": 0} {
		if held := cachedOf(server, name); held != want {
			t.Errorf("the collector %s has %d cached results, want %d", name, held, want)
		}
	}
}
