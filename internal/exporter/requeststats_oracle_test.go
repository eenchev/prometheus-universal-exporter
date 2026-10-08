package exporter

import (
	"fmt"
	"math/rand/v2"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// oracleTracker is requestTracker as it was before expireLocked kept
// idleFloor, which went through every tracked request each time it was
// called: the tests below hold the tracker to doing what it did. Only what
// the tests drive is copied, and setStatic takes its requests in an order
// the test gives, where the old code took them in the map's order.
type oracleTracker struct {
	stats         map[requestKey]*trackedRequest
	static        map[requestKey]bool
	capReached    bool
	latestDropped time.Time
	now           func() time.Time
}

func newOracleTracker(now func() time.Time) *oracleTracker {
	return &oracleTracker{stats: map[requestKey]*trackedRequest{}, static: map[requestKey]bool{}, now: now}
}

func (t *oracleTracker) statsFor(key requestKey) *serverStats {
	return t.adoptLocked(key, newServerStats(t.now()))
}

func (t *oracleTracker) newStats() *serverStats { return newServerStats(t.now()) }

func (t *oracleTracker) existing(key requestKey) *serverStats {
	tracked := t.stats[key]
	if tracked == nil {
		return nil
	}
	tracked.used = t.now()
	return tracked.stats
}

func (t *oracleTracker) adopt(key requestKey, staged *serverStats) {
	if kept := t.adoptLocked(key, staged); kept != nil && kept != staged {
		staged.mu.Lock()
		values := staged.statsValues
		staged.mu.Unlock()
		kept.mu.Lock()
		kept.absorb(values)
		kept.mu.Unlock()
	}
}

func (t *oracleTracker) adoptLocked(key requestKey, created *serverStats) *serverStats {
	now := t.now()
	if tracked := t.stats[key]; tracked != nil {
		tracked.used = now
		return tracked.stats
	}
	if len(t.stats) >= VerboseRequestSeriesLimit {
		t.expireLocked(now)
	}
	if len(t.stats) >= VerboseRequestSeriesLimit {
		t.capReached = true
		return nil
	}
	created.mu.Lock()
	if !created.created.After(t.latestDropped) {
		created.created = now
	}
	created.mu.Unlock()
	t.stats[key] = &trackedRequest{stats: created, used: now}
	return created
}

func (t *oracleTracker) dropLocked(key requestKey) {
	tracked := t.stats[key]
	if tracked == nil {
		return
	}
	if created := tracked.stats.snapshot().created; created.After(t.latestDropped) {
		t.latestDropped = created
	}
	delete(t.stats, key)
}

func (t *oracleTracker) setStatic(keys map[requestKey]bool, order []requestKey) {
	for key := range t.static {
		if !keys[key] {
			t.dropLocked(key)
		}
	}
	t.static = keys
	for _, key := range order {
		if t.stats[key] == nil {
			t.adoptLocked(key, newServerStats(t.now()))
		}
	}
	t.settleCapLocked()
}

func (t *oracleTracker) expire() { t.expireLocked(t.now()) }

func (t *oracleTracker) expireLocked(now time.Time) {
	for key, tracked := range t.stats {
		if !t.static[key] && now.Sub(tracked.used) > VerboseRequestIdleExpiry {
			t.dropLocked(key)
		}
	}
	t.settleCapLocked()
}

func (t *oracleTracker) settleCapLocked() {
	if len(t.stats) < VerboseRequestSeriesLimit {
		t.capReached = false
	}
}

func (t *oracleTracker) forgetCollectors(names map[string]bool) {
	for key := range t.stats {
		if names[key.Collector] {
			t.dropLocked(key)
		}
	}
	t.capReached = len(t.stats) >= VerboseRequestSeriesLimit
}

func (t *oracleTracker) Reset() {
	for key := range t.stats {
		t.dropLocked(key)
	}
	t.stats = map[requestKey]*trackedRequest{}
	t.static = map[requestKey]bool{}
	t.capReached = false
}

// sortedRequestKeys is keys in the order Snapshot gives them.
func sortedRequestKeys(keys []requestKey) []requestKey {
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Collector != keys[j].Collector {
			return keys[i].Collector < keys[j].Collector
		}
		if keys[i].URL != keys[j].URL {
			return keys[i].URL < keys[j].URL
		}
		return keys[i].Method < keys[j].Method
	})
	return keys
}

// staticOrder is the order the old setStatic would have had to take keys in
// to track what the new one tracked: first the requests it started tracking,
// then the rest. Past the limit the old code tracked the first of them in
// the map's order that there was room for, so the new tracker does what the
// old one did exactly when the oracle, given this order, tracks the same.
func staticOrder(keys map[requestKey]bool, before map[requestKey]bool, after map[requestKey]*trackedRequest) []requestKey {
	var started, rest []requestKey
	for key := range keys {
		if after[key] != nil && !before[key] {
			started = append(started, key)
		} else {
			rest = append(rest, key)
		}
	}
	return append(sortedRequestKeys(started), sortedRequestKeys(rest)...)
}

// sameTrackers reports how tracker differs from the oracle: in the requests
// it tracks and their values, whether it is capped, and in what it keeps
// that decides what it shows later, when each request was last used, the
// static requests and the latest creation time dropped. After a read, read
// is set, and what the verbose series are made of, Snapshot, is compared as
// well, in its order.
func sameTrackers(tracker *requestTracker, oracle *oracleTracker, read bool) error {
	switch {
	case tracker.capReached != oracle.capReached:
		return fmt.Errorf("capped=%v, the old tracker %v", tracker.capReached, oracle.capReached)
	case len(tracker.stats) != len(oracle.stats):
		return fmt.Errorf("%d requests tracked, the old tracker %d", len(tracker.stats), len(oracle.stats))
	case !tracker.latestDropped.Equal(oracle.latestDropped):
		return fmt.Errorf("latest dropped creation time %v, the old tracker %v", tracker.latestDropped, oracle.latestDropped)
	case len(tracker.static) != len(oracle.static):
		return fmt.Errorf("%d static requests, the old tracker %d", len(tracker.static), len(oracle.static))
	}
	for key := range tracker.static {
		if !oracle.static[key] {
			return fmt.Errorf("%+v is static, not to the old tracker", key)
		}
	}
	for key, tracked := range tracker.stats {
		was := oracle.stats[key]
		switch {
		case was == nil:
			return fmt.Errorf("%+v is tracked, not by the old tracker", key)
		case tracked.stats.snapshot() != was.stats.snapshot():
			return fmt.Errorf("%+v is %+v, in the old tracker %+v", key, tracked.stats.snapshot(), was.stats.snapshot())
		case !tracked.used.Equal(was.used):
			return fmt.Errorf("%+v was last used at %v, in the old tracker at %v", key, tracked.used, was.used)
		}
	}
	if !read {
		return nil
	}
	samples, capped := tracker.Snapshot()
	keys := make([]requestKey, 0, len(oracle.stats))
	for key := range oracle.stats {
		keys = append(keys, key)
	}
	for i, key := range sortedRequestKeys(keys) {
		if want := (requestSample{Key: key, Values: oracle.stats[key].stats.snapshot()}); samples[i] != want {
			return fmt.Errorf("series %d are of %+v, the old tracker's of %+v", i, samples[i], want)
		}
	}
	if capped != oracle.capReached {
		return fmt.Errorf("the series say capped=%v, the old tracker's %v", capped, oracle.capReached)
	}
	return nil
}

// A tracker that goes through its requests only when one may be idle tracks
// what the old one did, which went through them at every expiry: driven by
// generated sequences of probes of new and tracked requests, scrapes of
// static targets, reads of the verbose self-metrics with the static targets
// as they were or changed by a reload, collectors removed, verbose switched
// off, and time passing by seconds, minutes, about the idle expiry and now
// and then backwards, it keeps the same requests with the same values, is
// capped at the same reads, and keeps the same times, after every step. Past
// the limit the old tracker took the static targets in the map's order, and
// the new one is held to an order the old could have had (staticOrder).
func TestTheTrackerKeepsWhatTheOldTrackerKept(t *testing.T) {
	sequences, steps := alloctest.UnlessRaced(3, 1), alloctest.UnlessRaced(300, 150)
	collectors := []string{"a", "b", "c"}
	keyOf := func(i int) requestKey {
		return requestKey{Collector: collectors[i%len(collectors)], URL: fmt.Sprintf("http://h/%d", i/len(collectors)), Method: "GET"}
	}
	keys := len(collectors) * VerboseRequestSeriesLimit
	key := func(rng *rand.Rand) requestKey { return keyOf(rng.IntN(keys)) }
	for seq := range sequences {
		rng := rand.New(rand.NewPCG(uint64(seq), 49))
		now := time.Unix(1_700_000_000, 0)
		clock := func() time.Time { return now }
		tracker := newRequestTracker()
		tracker.now = clock
		oracle := newOracleTracker(clock)
		static := map[requestKey]bool{}
		for step := range steps {
			var did string
			read := false
			switch r := rng.IntN(100); {
			case r < 40:
				// Probes, a few at once: of a request tracked each counts on
				// it, of a new one it counts aside and is adopted when it ends.
				n := 1 + rng.IntN(60)
				did = fmt.Sprintf("%d probes", n)
				for range n {
					k := key(rng)
					stats, was := tracker.existing(k), oracle.existing(k)
					if (stats == nil) != (was == nil) {
						t.Fatalf("sequence %d step %d: a probe of %+v finds it tracked: %v, by the old tracker: %v", seq, step, k, stats != nil, was != nil)
					}
					if stats != nil {
						stats.probes++
						was.probes++
						continue
					}
					staged, was := tracker.newStats(), oracle.newStats()
					staged.probes, was.probes = 1, 1
					tracker.adopt(k, staged, nil)
					oracle.adopt(k, was)
				}
			case r < 50:
				// A scrape of a static target.
				k := key(rng)
				did = fmt.Sprintf("scrape of %+v", k)
				got, want := tracker.statsFor(k), oracle.statsFor(k)
				if (got == nil) != (want == nil) {
					t.Fatalf("sequence %d step %d: %s: statistics %v, the old tracker's %v", seq, step, did, got != nil, want != nil)
				}
				if got != nil {
					got.lastScrape, want.lastScrape = now, now
				}
			case r < 75:
				// A read of the verbose self-metrics, with the static targets
				// as they are or as a reload left them: a few, nearly as many
				// as the tracker holds, or a few more, which are left over.
				if rng.IntN(3) == 0 {
					n := [3]int{rng.IntN(100), VerboseRequestSeriesLimit - rng.IntN(100), VerboseRequestSeriesLimit + 1 + rng.IntN(100)}[rng.IntN(3)]
					static = map[requestKey]bool{}
					for _, i := range rng.Perm(keys)[:n] {
						static[keyOf(i)] = true
					}
				}
				did = fmt.Sprintf("read with %d static targets", len(static))
				before := map[requestKey]bool{}
				for k := range tracker.stats {
					before[k] = true
				}
				// Each read makes the map anew (staticRequestKeys), which
				// both trackers then keep.
				seeded := make(map[requestKey]bool, len(static))
				for k := range static {
					seeded[k] = true
				}
				tracker.setStatic(seeded)
				oracle.setStatic(seeded, staticOrder(seeded, before, tracker.stats))
				tracker.expire()
				oracle.expire()
				read = true
			case r < 77:
				names := map[string]bool{collectors[rng.IntN(len(collectors))]: true}
				did = fmt.Sprintf("collectors %v removed", names)
				tracker.forgetCollectors(names)
				oracle.forgetCollectors(names)
			case r < 78:
				did = "verbose switched off"
				tracker.Reset()
				oracle.Reset()
			default:
				passes := []time.Duration{0, time.Second, time.Minute, 10 * time.Minute, VerboseRequestIdleExpiry / 2, VerboseRequestIdleExpiry - time.Second, VerboseRequestIdleExpiry, VerboseRequestIdleExpiry + time.Second, -time.Minute, -VerboseRequestIdleExpiry / 2}
				d := passes[rng.IntN(len(passes))]
				did = fmt.Sprintf("%v passed", d)
				now = now.Add(d)
			}
			if err := sameTrackers(tracker, oracle, read); err != nil {
				t.Fatalf("sequence %d step %d, after %s: %v", seq, step, did, err)
			}
		}
	}
}

// A read of the verbose self-metrics with more static targets than the
// tracker holds goes through the tracked requests at most once, however many
// of the static targets are left over: the tracker full of probed requests,
// then of static ones, and an hour later with the probed ones idle. Each
// static target left over was offered to the tracker, and each offer went
// through every request tracked, a thousand times a thousand at each read.
func TestAReadOfTheVerboseSelfMetricsGoesThroughTheTrackedRequestsAtMostOnce(t *testing.T) {
	var scans atomic.Int64
	count := func() { scans.Add(1) }
	requestsScannedHook.Store(&count)
	t.Cleanup(func() { requestsScannedHook.Store(nil) })

	tracker := newRequestTracker()
	now := time.Unix(1_700_000_000, 0)
	tracker.now = func() time.Time { return now }
	for i := range VerboseRequestSeriesLimit {
		tracker.statsFor(requestKey{Collector: "probed", URL: fmt.Sprintf("http://probed/%d", i), Method: "GET"})
	}
	static := map[requestKey]bool{}
	for i := range alloctest.UnlessRaced(2*VerboseRequestSeriesLimit, VerboseRequestSeriesLimit+100) {
		static[requestKey{Collector: "static", URL: fmt.Sprintf("http://static/%d", i), Method: "GET"}] = true
	}
	read := func(at string, wantStatic int) {
		t.Helper()
		scans.Store(0)
		tracker.setStatic(static)
		tracker.expire()
		samples, capped := tracker.Snapshot()
		got := 0
		for _, sample := range samples {
			if sample.Key.Collector == "static" {
				got++
			}
		}
		if got != wantStatic || len(samples) != VerboseRequestSeriesLimit || !capped {
			t.Fatalf("%s: %d requests tracked, %d static, capped=%v; want the tracker full with %d static", at, len(samples), got, capped, wantStatic)
		}
		if n := scans.Load(); n > 1 {
			t.Fatalf("%s: the read went through the tracked requests %d times, want at most once", at, n)
		}
	}
	read("full of probed requests", 0)
	read("full of probed requests, again", 0)
	now = now.Add(VerboseRequestIdleExpiry + time.Second)
	read("with the probed requests idle", VerboseRequestSeriesLimit)
	read("full of static requests", VerboseRequestSeriesLimit)
	now = now.Add(VerboseRequestIdleExpiry + time.Second)
	read("full of static requests an hour later", VerboseRequestSeriesLimit)
}
