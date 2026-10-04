//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
)

// What a probe or a static target scrape writes under its collector's name
// when it ends — its result in the cache, its failure or recovery in the
// failure log — goes nowhere once a reload has removed the collector it read,
// or changed its definition (reconcile.go).

// failableTarget is a target that answers value=42, or 500 while failing is
// set, and counts the requests it got.
func failableTarget(t *testing.T) (target *httptest.Server, requests *atomic.Int64, failing *atomic.Bool) {
	t.Helper()
	requests, failing = &atomic.Int64{}, &atomic.Bool{}
	target = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		if failing.Load() {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	t.Cleanup(target.Close)
	return target, requests, failing
}

// windowCollector is a collector that keeps its results for an hour: fresh,
// under cache.ttl, or as a fallback only, under cache.stale_if_error.
func windowCollector(name, window string) model.Collector {
	c := testutil.Collector(name, "text")
	if window == "stale_if_error" {
		c.Cache.StaleIfError = model.Duration(time.Hour)
	} else {
		c.Cache.TTL = model.Duration(time.Hour)
	}
	return c
}

// cachedOf is how many results the cache holds under the collector's name.
func cachedOf(server *Server, collector string) int {
	server.cache.mu.Lock()
	defer server.cache.mu.Unlock()
	held := 0
	for _, entry := range server.cache.entries {
		if entry.collector == collector {
			held++
		}
	}
	if entries := server.cache.byCollector[collector]; entries != nil && entries.Len() != held {
		return -1
	}
	return held
}

// heldProbe starts a probe of collector and holds it where it has read the
// configuration and has not gone to its target; goOn lets it go and returns
// how it was answered.
func heldProbe(t *testing.T, server *Server, collector, target string) (goOn func() probeOutcome) {
	t.Helper()
	reached, resume := holdFirstProbeOf(t, collector)
	held := probeAsync(context.Background(), server, probePath(collector, target, ""), nil)
	<-reached
	return func() probeOutcome {
		resume()
		outcome := <-held
		probeConfigReadHook.Store(nil)
		return outcome
	}
}

// A probe that read the configuration before a reload removed its collector
// goes to its target and is answered, and leaves nothing under the
// collector's name: no cached result, and no failure remembered or logged
// above debug level. So the collector brought back under the name with the
// same definition is not answered with the result of the one that was
// removed, within cache.ttl or, when its own trip fails, within
// cache.stale_if_error, and its first failure is logged as a first failure.
// That holds whether the reload was followed before the probe went on or only
// after it ended, when the reload itself drops what the probe wrote.
func TestAProbeThatEndsAfterItsCollectorWasRemovedLeavesNothingUnderItsName(t *testing.T) {
	for _, window := range []string{"ttl", "stale_if_error"} {
		for _, fails := range []bool{false, true} {
			for _, followedFirst := range []bool{true, false} {
				where := fmt.Sprintf("cache.%s, the late probe fails %v, the reload followed before it goes on %v", window, fails, followedFirst)
				logs := testutil.CaptureLogs(t)
				target, requests, failing := failableTarget(t)
				kept, gone := windowCollector("kept", window), windowCollector("gone", window)
				server, _ := newCacheTestServer(t, kept, gone)
				goOn := heldProbe(t, server, "gone", target.URL)
				reloadTo(t, server, kept)
				if followedFirst {
					selfMetrics(t, server)
				}
				failing.Store(fails)
				wantCode := http.StatusOK
				if fails {
					wantCode = http.StatusBadGateway
				}
				if outcome := goOn(); outcome.code != wantCode || requests.Load() != 1 {
					t.Fatalf("%s: the probe that had read the configuration was answered %d after %d requests, want %d after 1: %s", where, outcome.code, requests.Load(), wantCode, outcome.body)
				}
				selfMetrics(t, server)
				if held, remembered := cachedOf(server, "gone"), rememberedOf(server, "gone"); held != 0 || len(remembered) != 0 {
					t.Errorf("%s: the removed collector has %d cached results and the failures %v remembered", where, held, remembered)
				}
				if followedFirst && strings.Contains(logs.String(), "probe failed") {
					t.Errorf("%s: the failure of a collector that is gone is logged above debug level:\n%s", where, logs)
				}
				// The collector is back as it was: its key in the cache and
				// in the failure log is the one the late probe had.
				reloadTo(t, server, kept, gone)
				failing.Store(window == "stale_if_error" || fails)
				first := probeOnce(t, server, probePath("gone", target.URL, ""), nil)
				wantCode = http.StatusOK
				if failing.Load() {
					wantCode = http.StatusBadGateway
				}
				if first.Code != wantCode || requests.Load() != 2 {
					t.Errorf("%s: the first probe of the collector brought back was answered %d after %d requests, want %d from its own trip, the second request: %s", where, first.Code, requests.Load(), wantCode, first.Body)
				}
				if failing.Load() {
					if remembered := rememberedOf(server, "gone"); !slices.Equal(remembered, []string{"http_status x1"}) {
						t.Errorf("%s: the first failure of the collector brought back is remembered as %v, want as a first failure", where, remembered)
					}
				}
			}
		}
	}
}

// The same holds when the reload put a different collector under the name: a
// probe that read the old definition leaves no cached result, which the new
// collector could never serve and which would count in its
// max_cache_entries, so the new collector's first probe goes to the target
// and its second is answered from its own entry; and the old probe's failure
// is remembered nowhere, and logged at debug level only.
func TestAProbeThatEndsAfterItsCollectorWasReplacedLeavesNothingUnderItsName(t *testing.T) {
	// The old definition keeps a result for a day, the new one for a minute,
	// and either holds one entry: kept, the old result would be the last to
	// be evicted, and the new collector's own would be evicted in its place.
	old := cachingCollector("swapped", 24*time.Hour)
	old.Limits.MaxCacheEntries = 1
	replaced := cachingCollector("swapped", time.Minute)
	replaced.Limits.MaxCacheEntries = 1
	replaced.Metrics[0].Name = "renamed_value"

	t.Run("its result is not cached", func(t *testing.T) {
		testutil.CaptureLogs(t)
		target, requests, _ := failableTarget(t)
		server, _ := newCacheTestServer(t, old)
		goOn := heldProbe(t, server, "swapped", target.URL)
		reloadTo(t, server, replaced)
		selfMetrics(t, server)
		if outcome := goOn(); outcome.code != http.StatusOK || !strings.Contains(outcome.body, "demo_value 42") {
			t.Fatalf("the probe that had read the old definition was answered %d: %s", outcome.code, outcome.body)
		}
		if held := cachedOf(server, "swapped"); held != 0 {
			t.Errorf("the result of the definition that is gone is cached under the collector's name: %d entries", held)
		}
		first := probeOnce(t, server, probePath("swapped", target.URL, ""), nil)
		if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), "renamed_value 42") || requests.Load() != 2 {
			t.Fatalf("the new collector's first probe was answered %d after %d requests, want from the target, the second request: %s", first.Code, requests.Load(), first.Body)
		}
		second := probeOnce(t, server, probePath("swapped", target.URL, ""), nil)
		if second.Code != http.StatusOK || requests.Load() != 2 || cachedOf(server, "swapped") != 1 {
			t.Errorf("the new collector's second probe was answered %d after %d requests with %d entries cached, want from its own entry, after 2 requests", second.Code, requests.Load(), cachedOf(server, "swapped"))
		}
	})

	t.Run("its failure is not remembered", func(t *testing.T) {
		logs := testutil.CaptureLogs(t)
		target, _, failing := failableTarget(t)
		server, _ := newCacheTestServer(t, old)
		goOn := heldProbe(t, server, "swapped", target.URL)
		reloadTo(t, server, replaced)
		selfMetrics(t, server)
		failing.Store(true)
		if outcome := goOn(); outcome.code != http.StatusBadGateway {
			t.Fatalf("the probe that had read the old definition was answered %d: %s", outcome.code, outcome.body)
		}
		if remembered := rememberedOf(server, "swapped"); len(remembered) != 0 || strings.Contains(logs.String(), "probe failed") {
			t.Errorf("the failure of the definition that is gone is remembered as %v, or logged above debug level:\n%s", remembered, logs)
		}
		probeOnce(t, server, probePath("swapped", target.URL, ""), nil)
		if remembered := rememberedOf(server, "swapped"); !slices.Equal(remembered, []string{"http_status x1"}) || strings.Count(logs.String(), "probe failed") != 1 {
			t.Errorf("the new collector's first failure is remembered as %v, want alone and logged once in full:\n%s", remembered, logs)
		}
	})
}

// A probe that read the configuration before a reload removed its collector,
// and ends only when the collector is back under the name as it was and has
// failed, is no recovery of the collector brought back, whose failure stays
// remembered, and its result does not answer that collector's next probe.
func TestAProbeThatEndsAfterItsCollectorCameBackIsNotThatCollectorsRecovery(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	target, requests, failing := failableTarget(t)
	kept, gone := windowCollector("kept", "ttl"), windowCollector("gone", "ttl")
	server, _ := newCacheTestServer(t, kept, gone)
	goOn := heldProbe(t, server, "gone", target.URL)
	reloadTo(t, server, kept)
	selfMetrics(t, server)
	reloadTo(t, server, kept, gone)
	failing.Store(true)
	if first := probeOnce(t, server, probePath("gone", target.URL, ""), nil); first.Code != http.StatusBadGateway {
		t.Fatalf("the probe of the collector brought back was answered %d: %s", first.Code, first.Body)
	}
	failing.Store(false)
	if outcome := goOn(); outcome.code != http.StatusOK || requests.Load() != 2 {
		t.Fatalf("the probe that had read the configuration was answered %d after %d requests: %s", outcome.code, requests.Load(), outcome.body)
	}
	if held, remembered := cachedOf(server, "gone"), rememberedOf(server, "gone"); held != 0 || !slices.Equal(remembered, []string{"http_status x1"}) || strings.Contains(logs.String(), "probe recovered") {
		t.Errorf("after the success of the collector that was removed, the one brought back has %d cached results and the failures %v remembered, want none cached, its failure kept and no recovery logged:\n%s", held, remembered, logs)
	}
	if next := probeOnce(t, server, probePath("gone", target.URL, ""), nil); next.Code != http.StatusOK || requests.Load() != 3 {
		t.Errorf("the next probe of the collector brought back was answered %d after %d requests, want from its own trip, the third request", next.Code, requests.Load())
	}
	if remembered := rememberedOf(server, "gone"); len(remembered) != 0 || strings.Count(logs.String(), "probe recovered") != 1 {
		t.Errorf("after its own success the collector brought back has the failures %v remembered, want recovered and logged once:\n%s", remembered, logs)
	}
}

// A static target's scrape is held to the same: one made with a configuration
// read before a reload removed its collector, or changed it, caches nothing
// and has no failure remembered, whether it says when it read the
// configuration or not; made with the configuration in force it caches its
// result and has its failure remembered as ever.
func TestAStaticScrapeOfACollectorNoLongerInForceLeavesNothingUnderItsName(t *testing.T) {
	testutil.CaptureLogs(t)
	target, requests, failing := failableTarget(t)
	kept, gone := windowCollector("kept", "ttl"), windowCollector("gone", "ttl")
	changed := windowCollector("gone", "ttl")
	changed.Metrics[0].Name = "renamed_value"
	static := model.StaticTarget{Name: "one", Collector: "gone", Target: target.URL, Interval: model.Duration(time.Minute)}
	for _, reload := range []string{"removes", "changes"} {
		for _, fails := range []bool{false, true} {
			for _, saysWhen := range []bool{true, false} {
				where := fmt.Sprintf("a reload that %s the collector, the scrape fails %v, it says when it read the configuration %v", reload, fails, saysWhen)
				server, _ := newCacheTestServer(t, kept, gone)
				read := server.reconcile()
				if reload == "removes" {
					reloadTo(t, server, kept)
				} else {
					reloadTo(t, server, kept, changed)
				}
				server.reconcile()
				before := requests.Load()
				failing.Store(fails)
				if saysWhen {
					server.scrapeTargetSince(context.Background(), read.config, read.generation, static)
				} else {
					server.scrapeTarget(context.Background(), read.config, static)
				}
				if held, remembered := cachedOf(server, "gone"), rememberedOf(server, "gone"); requests.Load() != before+1 || held != 0 || len(remembered) != 0 {
					t.Errorf("%s: after %d requests the collector has %d cached results and the failures %v remembered, want nothing after one request", where, requests.Load()-before, held, remembered)
				}
			}
		}
	}
	for _, fails := range []bool{false, true} {
		server, manager := newCacheTestServer(t, kept, gone)
		manager.SetTargets("", &model.StaticTargetFile{Targets: []model.StaticTarget{static}})
		reloadTo(t, server, gone)
		failing.Store(fails)
		cfg, _, generation := server.inForce()
		server.scrapeTargetSince(context.Background(), cfg, generation, static)
		wantHeld, wantRemembered := 1, 0
		if fails {
			wantHeld, wantRemembered = 0, 1
		}
		if held, remembered := cachedOf(server, "gone"), rememberedOf(server, "gone"); held != wantHeld || len(remembered) != wantRemembered {
			t.Errorf("a scrape with the configuration in force, failing %v, leaves %d cached results and the failures %v, want %d and %d", fails, held, remembered, wantHeld, wantRemembered)
		}
	}
}

// cachedDocument is a configuration file of the named collectors, each
// keeping its results for an hour.
func cachedDocument(collectors ...string) string {
	var b strings.Builder
	b.WriteString("collectors:\n")
	for _, name := range collectors {
		b.WriteString(strings.Replace(testutil.CollectorYAML(name), "    transform:\n", "    cache:\n      ttl: 1h\n    transform:\n", 1))
	}
	return b.String()
}

// Probes of a caching collector that reloads remove and bring back, running
// while the reloads come, each a request of its own that would be cached:
// once a reload that removed the collector is followed nothing is cached
// under its name, then or after the probes in flight have ended, and nothing
// is remembered of it. Run with the race detector, it also shows that the
// cache, the failure log and the configuration they follow are written and
// replaced together.
func TestProbesDuringReloadsCacheNothingForACollectorThatIsRemoved(t *testing.T) {
	testutil.CaptureLogs(t)
	target := textTarget(t, "value=42\n")
	r := newReloadable(t, cachedDocument("kept", "gone"), "")
	const probers, rounds = 4, 12
	var wg sync.WaitGroup
	var ended [probers]atomic.Int64
	// inFlightEnded waits until every prober has ended the probe it had in
	// flight when it is called, and one more.
	inFlightEnded := func() {
		var after [probers]int64
		for i := range ended {
			after[i] = ended[i].Load() + 2
		}
		testutil.WaitFor(t, "the probes in flight to end", func() bool {
			for i := range ended {
				if ended[i].Load() < after[i] {
					return false
				}
			}
			return true
		})
	}
	stop := make(chan struct{})
	for i := range probers {
		wg.Go(func() {
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				// Each probe a request of its own, so each makes a trip and
				// has a result to cache.
				request := httptest.NewRequest(http.MethodGet, probePath("gone", target.URL, fmt.Sprintf("&timeout=%d.%03ds", 5+i, n%1000)), nil)
				r.server.Handler().ServeHTTP(httptest.NewRecorder(), request)
				ended[i].Add(1)
			}
		})
	}
	for round := range rounds {
		r.reloadTo(cachedDocument("kept"))
		selfMetrics(t, r.server)
		if held := cachedOf(r.server, "gone"); held != 0 {
			t.Errorf("round %d: %d results are cached for the removed collector when its removal is followed", round, held)
		}
		inFlightEnded()
		if held, remembered := cachedOf(r.server, "gone"), rememberedOf(r.server, "gone"); held != 0 || len(remembered) != 0 {
			t.Errorf("round %d: the removed collector has %d cached results and the failures %v remembered once the probes in flight have ended", round, held, remembered)
		}
		r.reloadTo(cachedDocument("kept", "gone"))
		inFlightEnded()
	}
	close(stop)
	wg.Wait()
	if held := cachedOf(r.server, "gone"); held == 0 {
		t.Error("the collector brought back has nothing cached; its probes cached nothing")
	}
	r.reloadTo(cachedDocument("kept"))
	selfMetrics(t, r.server)
	if held, remembered := cachedOf(r.server, "gone"), rememberedOf(r.server, "gone"); held != 0 || len(remembered) != 0 {
		t.Errorf("after the probes the removed collector has %d cached results and the failures %v remembered", held, remembered)
	}
}

// writesOf is what a server keeps under the collector kept: the keys of its
// cached results, and what the failure log remembers of it.
func writesOf(server *Server) string {
	server.cache.mu.Lock()
	var keys []string
	for key, entry := range server.cache.entries {
		if entry.collector == "kept" {
			keys = append(keys, key)
		}
	}
	server.cache.mu.Unlock()
	sort.Strings(keys)
	return fmt.Sprintf("cached %v, remembered %v", keys, rememberedOf(server, "kept"))
}

// A collector that reloads leave unchanged caches and logs as it did: over
// every sequence of three steps — a probe that succeeds, one that fails, a
// reload that removes the collector beside it, one that brings that back and
// adds another, and a probe, succeeding or failing, held across such a reload
// after it read the configuration — its cached results, what the failure log
// remembers of it and its counters, cache hits and misses among them, are
// those of a server that made the same probes and was never reloaded.
func TestACollectorThatStaysCachesAndLogsAsItDidThroughReloads(t *testing.T) {
	testutil.CaptureLogs(t)
	target, _, failing := failableTarget(t)
	kept, gone, added := windowCollector("kept", "ttl"), windowCollector("gone", "ttl"), windowCollector("added", "ttl")
	steps := []string{"probe", "failed probe", "remove", "add", "held", "held failing"}
	const length = 3
	sequences := 1
	for range length {
		sequences *= len(steps)
	}
	for sequence := range sequences {
		reloaded, never := verboseServer(t, false, kept, gone), verboseServer(t, false, kept, gone)
		var taken []string
		for rest := sequence; len(taken) < length; rest /= len(steps) {
			step := steps[rest%len(steps)]
			taken = append(taken, step)
			failing.Store(strings.Contains(step, "fail"))
			switch step {
			case "probe", "failed probe":
				probeOnce(t, reloaded, probePath("kept", target.URL, ""), nil)
				probeOnce(t, never, probePath("kept", target.URL, ""), nil)
			case "remove":
				reloadTo(t, reloaded, kept)
			case "add":
				reloadTo(t, reloaded, kept, gone, added)
			default:
				goOn := heldProbe(t, reloaded, "kept", target.URL)
				if len(reloaded.manager.Get().Collectors) > 1 {
					reloadTo(t, reloaded, kept)
				} else {
					reloadTo(t, reloaded, kept, gone, added)
				}
				// The reload is followed while the probe is held.
				selfMetrics(t, reloaded)
				held := goOn()
				if outcome := probeOnce(t, never, probePath("kept", target.URL, ""), nil); outcome.Code != held.code {
					t.Fatalf("%v: the held probe was answered %d, and %d without the reload", taken, held.code, outcome.Code)
				}
			}
			if got, want := writesOf(reloaded), writesOf(never); got != want {
				t.Fatalf("after %v the collector that stayed has\n%s\nand without the reloads\n%s", taken, got, want)
			}
			if got, want := keptCounters(t, reloaded), keptCounters(t, never); got != want {
				t.Fatalf("after %v the collector that stayed shows\n%s\nand without the reloads\n%s", taken, got, want)
			}
		}
	}
}

// formerFollowLocked is followLocked as it was before the followed
// configuration said from when each collector has had its definition, and
// was replaced under the locks of the cache and the failure log, but for one
// thing it does as followLocked now does: it forgets the failures of a
// changed collector with those of a removed one, where it kept them.
func formerFollowLocked(s *Server, cfg *model.Config) *followedConfig {
	previous := s.followed.Load()
	if previous != nil && previous.config == cfg {
		return previous
	}
	next := &followedConfig{config: cfg, generation: firstGeneration}
	if previous != nil {
		next.generation = previous.generation + 1
	}
	defer s.followed.Store(next)
	if s.since == nil {
		s.since = map[string]uint64{}
	}
	var collectors []model.Collector
	if cfg != nil {
		collectors = cfg.Collectors
	}
	if previous == nil || previous.config == nil {
		for i := range collectors {
			s.since[collectors[i].Name] = next.generation
		}
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
	}
	if len(stale) == 0 {
		return next
	}
	if len(removed) > 0 {
		s.requests.forgetCollectors(removed)
	}
	s.failures.forgetCollectors(stale)
	s.cache.dropCollectors(stale)
	return next
}

// keptState is what a server keeps per collector: the generation followed,
// each collector's cached results and remembered failures, the generation it
// has been there from, and whether statistics are kept for it.
func keptState(server *Server) string {
	server.statsMu.Lock()
	defer server.statsMu.Unlock()
	names := map[string]bool{}
	for name := range server.since {
		names[name] = true
	}
	for name := range server.stats {
		names[name] = true
	}
	server.cache.mu.Lock()
	for _, entry := range server.cache.entries {
		names[entry.collector] = true
	}
	server.cache.mu.Unlock()
	server.failures.mu.Lock()
	for key := range server.failures.entries {
		names[keyCollector(key)] = true
	}
	server.failures.mu.Unlock()
	var b strings.Builder
	fmt.Fprintf(&b, "generation %d\n", server.followed.Load().generation)
	for _, name := range model.SortedKeys(names) {
		since, named := server.since[name]
		fmt.Fprintf(&b, "%s: %d cached, remembered %v, since %d (%v), statistics %v\n", name, cachedOf(server, name), rememberedOf(server, name), since, named, server.stats[name] != nil)
	}
	return b.String()
}

// Following a reload drops and keeps what it did: over every sequence of four
// reloads among configurations that remove, add and change collectors, with a
// cached result, a remembered failure and statistics under every collector's
// name before each, the cached results, the remembered failures, the
// statistics kept and the generations are those the reload left before the
// followed configuration said from when each definition has been there, but
// for the failures of a changed collector, now forgotten. And
// after each reload a caller of every earlier generation finds a collector
// standing exactly when the collector has been in every configuration since
// with the definition it had; a caller that names no configuration always.
func TestFollowingAReloadDropsAndKeepsWhatItDid(t *testing.T) {
	logger := testutil.QuietLogger(t)
	kept, gone, added := testutil.Collector("kept", "text"), testutil.Collector("gone", "text"), testutil.Collector("added", "text")
	changed := testutil.Collector("kept", "text")
	changed.Limits.MaxResponseBytes = 2048
	variants := [][]model.Collector{{kept, gone}, {kept}, {kept, gone, added}, {changed, added}}
	set := model.MetricSet{Metrics: []model.Metric{{Name: "demo_value", Type: model.GaugeMetricType, Value: 1}}}
	const steps = 4
	sequences := 1
	for range steps {
		sequences *= len(variants)
	}
	for sequence := range sequences {
		now, former := verboseServer(t, false, kept, gone), verboseServer(t, false, kept, gone)
		now.logger, former.logger = logger, logger
		// history is the definitions of every configuration followed.
		history := []map[string]string{{}}
		for _, c := range now.manager.Get().Collectors {
			history[0][c.Name] = collectorFingerprint(&c)
		}
		generations := []uint64{now.reconcile().generation}
		for step, rest := 0, sequence; step < steps; step, rest = step+1, rest/len(variants) {
			variant := variants[rest%len(variants)]
			cfg := &model.Config{Collectors: variant}
			if err := config.Validate(cfg); err != nil {
				t.Fatal(err)
			}
			for _, server := range []*Server{now, former} {
				for _, c := range server.manager.Get().Collectors {
					server.cache.Put(fmt.Sprintf("%s-%d", c.Name, step), c.Name, set, time.Hour, 0, 0, time.Now())
					server.failures.failed(logger, slog.LevelError, failureKey(c.Name, "http://target", ""), "probe failed", "fetch", context.DeadlineExceeded)
					server.statsFor(c.Name)
				}
				installConfig(server, cfg)
			}
			now.reconcile()
			former.statsMu.Lock()
			formerFollowLocked(former, cfg)
			former.statsMu.Unlock()
			if got, want := keptState(now), keptState(former); got != want {
				t.Fatalf("sequence %d, step %d: following the reload leaves\n%s\nand it left\n%s", sequence, step, got, want)
			}
			definitions := map[string]string{}
			for i := range cfg.Collectors {
				definitions[cfg.Collectors[i].Name] = collectorFingerprint(&cfg.Collectors[i])
			}
			history = append(history, definitions)
			// The same variant again is the same configuration to the
			// collectors, and another generation.
			generations = append(generations, now.reconcile().generation)
			for age, generation := range generations {
				for _, name := range []string{"kept", "gone", "added", "never_configured"} {
					want := true
					for _, later := range history[age:] {
						if definition, there := later[name]; !there || definition != history[age][name] {
							want = false
						}
					}
					if got := now.readAt(generation).stands(name); got != want {
						t.Fatalf("sequence %d, step %d: to a caller that read configuration %d the collector %s stands %v, want %v", sequence, step, age, name, got, want)
					}
					if !(configRead{}).stands(name) {
						t.Fatalf("sequence %d, step %d: to a caller that names no configuration the collector %s does not stand", sequence, step, name)
					}
				}
			}
			for _, name := range []string{"kept", "gone", "added"} {
				if now.readAt(noGeneration).stands(name) {
					t.Fatalf("sequence %d, step %d: to a caller of no generation the collector %s stands", sequence, step, name)
				}
			}
		}
	}
}
