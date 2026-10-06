//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// holdFirstProbeOf makes the first probe of collector wait where it has read
// the configuration and has not yet taken the collector's statistics, until
// resume is called. reached is closed when the probe is there.
func holdFirstProbeOf(t *testing.T, collector string) (reached <-chan struct{}, resume func()) {
	t.Helper()
	there, held := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	var once sync.Once
	hook := func(name string) {
		if name == collector && first.CompareAndSwap(false, true) {
			close(there)
			<-held
		}
	}
	probeConfigReadHook.Store(&hook)
	resume = func() { once.Do(func() { close(held) }) }
	t.Cleanup(func() {
		resume()
		probeConfigReadHook.Store(nil)
	})
	return there, resume
}

// bodyHeldTarget is a target that answers value=42 at once, except to the
// first request it gets: that one is sent the answer's headers and the
// beginning of its body, and the rest when release is called, so the probe
// that made it is held while it reads the answer. arrived has a value for
// every request that reached the target.
func bodyHeldTarget(t *testing.T) (target *httptest.Server, arrived <-chan struct{}, release func()) {
	t.Helper()
	var first atomic.Bool
	var once sync.Once
	held, reached := make(chan struct{}), make(chan struct{}, 16)
	release = func() { once.Do(func() { close(held) }) }
	target = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if first.CompareAndSwap(false, true) {
			_, _ = w.Write([]byte("value="))
			w.(http.Flusher).Flush()
			reached <- struct{}{}
			select {
			case <-held:
			case <-r.Context().Done():
				return
			}
			_, _ = w.Write([]byte("42\n"))
			return
		}
		reached <- struct{}{}
		_, _ = w.Write([]byte("value=42\n"))
	}))
	t.Cleanup(func() {
		release()
		target.Close()
	})
	return target, reached, release
}

// shownOf is the lines of a self-metrics answer that carry the collector.
func shownOf(answer, collector string) []string {
	var lines []string
	for line := range strings.SplitSeq(answer, "\n") {
		if strings.Contains(line, `collector="`+collector+`"`) {
			lines = append(lines, line)
		}
	}
	return lines
}

// startsFromZero fails the test unless the collector gone of the answer body
// has counted probes probes and nothing else, in its counters, its scrape-time
// histogram and its per-request series when verbose, and every creation time
// of its series is back or later.
func startsFromZero(t *testing.T, where, body string, verbose bool, probes float64, back int64) {
	t.Helper()
	series := []string{
		`http_exporter_scrapes_total{collector="gone"}`,
		`http_exporter_scrape_success_total{collector="gone"}`,
		`http_exporter_decode_success_total{collector="gone"}`,
		`http_exporter_metrics_emitted_total{collector="gone"}`,
	}
	if verbose {
		series = append(series, `http_exporter_collector_scrape_duration_seconds_count{collector="gone"}`)
	}
	for _, name := range series {
		if got := seriesValue(t, body, name); got != probes {
			t.Errorf("%s: %s is %v, want %v", where, name, got, probes)
		}
	}
	var early []string
	created := 0
	for _, line := range shownOf(body, "gone") {
		name, value, _ := strings.Cut(line, " ")
		switch {
		case strings.Contains(name, "_created{"):
			created++
			if at := createdTimes(t, line)[name]; at < back {
				early = append(early, fmt.Sprintf("%s at %d", name, at))
			}
		case strings.Contains(name, "_total{") && strings.Contains(name, "url="):
			if got := seriesValue(t, body, name); got != 0 && got != probes {
				t.Errorf("%s: the request series %s is at %s, want 0 or %v", where, name, value, probes)
			}
		}
	}
	if created == 0 {
		t.Errorf("%s: the collector brought back has no _created sample:\n%s", where, body)
	}
	if len(early) > 0 {
		t.Errorf("%s: %d of the %d series created are created earlier than %d, when the collector came back, the first: %s", where, len(early), created, back, early[0])
	}
}

// A probe that read the configuration before a reload removed its collector,
// and takes the collector's statistics only after they were dropped, counts
// nowhere that is shown, as one does that was further on at the reload: the
// removed collector has no series, not a per-request one either, and the
// collector brought back under the name starts from zero at a time no earlier
// than its return. That holds without verbose mode and with it, whether the
// probe goes on before the collector is back or only after it was probed
// again.
func TestAProbeThatReadTheConfigurationBeforeItsCollectorWasRemovedIsCountedNowhereShown(t *testing.T) {
	for _, verbose := range []bool{false, true} {
		for _, goesOnLate := range []bool{false, true} {
			where := fmt.Sprintf("verbose %v, the probe goes on after the collector is back %v", verbose, goesOnLate)
			target := textTarget(t, "value=42\n")
			r := newReloadable(t, createdDocument(verbose, "kept", "gone"), "")
			read := func() string {
				_, body := answerOf(t, r.server, "/self-metrics", prometheus2Accept)
				return body
			}
			reached, resume := holdFirstProbeOf(t, "gone")
			old := probeAsync(context.Background(), r.server, probePath("gone", target.URL, ""), nil)
			<-reached
			r.reloadTo(createdDocument(verbose, "kept"))
			answers := []string{read()}
			goOn := func() {
				resume()
				if outcome := <-old; outcome.code != http.StatusOK {
					t.Fatalf("%s: the probe that had read the configuration was answered %d: %s", where, outcome.code, outcome.body)
				}
			}
			probes := 0.0
			if !goesOnLate {
				goOn()
				answers = append(answers, read())
			}
			for _, answer := range answers {
				if shown := shownOf(answer, "gone"); len(shown) > 0 {
					t.Errorf("%s: the removed collector is shown:\n%s", where, strings.Join(shown, "\n"))
				}
			}
			time.Sleep(2 * time.Millisecond)
			back := time.Now().UnixMilli()
			r.reloadTo(createdDocument(verbose, "kept", "gone"))
			if goesOnLate {
				if code := probeOnce(t, r.server, probePath("gone", target.URL, ""), nil).Code; code != http.StatusOK {
					t.Fatalf("%s: the probe of the collector brought back was answered %d", where, code)
				}
				goOn()
				probes = 1
			}
			body := read()
			startsFromZero(t, where, body, verbose, probes, back)
			if verbose {
				if got := seriesValue(t, body, "http_exporter_request_series_tracked"); got != probes {
					t.Errorf("%s: %v requests are tracked, want %v", where, got, probes)
				}
			}
			strictlyRead(t, append(answers, body)...)
			probeConfigReadHook.Store(nil)
		}
	}
}

// Wherever a probe is when a reload removes its collector, the collector
// brought back starts from zero: a probe that has not read the configuration
// yet reads the new one, and is refused while the collector is gone; one that
// has read it, one that waits for its target, and one that is reading the
// target's answer go on with the collector they had, and are counted nowhere
// that is shown.
func TestACollectorBroughtBackStartsFromZeroWhereverAProbeOfItWas(t *testing.T) {
	for _, held := range []string{"before it read the configuration", "when it had read the configuration", "waiting for its target", "reading its target's answer"} {
		for _, verbose := range []bool{false, true} {
			where := fmt.Sprintf("a probe held %s, verbose %v", held, verbose)
			r := newReloadable(t, createdDocument(verbose, "kept", "gone"), "")
			read := func() string {
				_, body := answerOf(t, r.server, "/self-metrics", prometheus2Accept)
				return body
			}
			var old <-chan probeOutcome
			goOn := func() {}
			wantCode := http.StatusOK
			switch held {
			case "before it read the configuration":
				// It is sent once the collector is gone: nothing of it has
				// happened before then.
				wantCode = http.StatusBadRequest
			case "when it had read the configuration":
				target := textTarget(t, "value=42\n")
				reached, resume := holdFirstProbeOf(t, "gone")
				old, goOn = probeAsync(context.Background(), r.server, probePath("gone", target.URL, ""), nil), resume
				<-reached
			case "waiting for its target":
				target, arrived, release := slowFirstTarget(t)
				old, goOn = probeAsync(context.Background(), r.server, probePath("gone", target.URL, ""), nil), release
				<-arrived
			case "reading its target's answer":
				target, arrived, release := bodyHeldTarget(t)
				old, goOn = probeAsync(context.Background(), r.server, probePath("gone", target.URL, ""), nil), release
				<-arrived
			}
			r.reloadTo(createdDocument(verbose, "kept"))
			removed := read()
			if old == nil {
				old = probeAsync(context.Background(), r.server, probePath("gone", "http://127.0.0.1:1", ""), nil)
			}
			goOn()
			if outcome := <-old; outcome.code != wantCode {
				t.Fatalf("%s: the probe was answered %d, want %d: %s", where, outcome.code, wantCode, outcome.body)
			}
			for _, answer := range []string{removed, read()} {
				if shown := shownOf(answer, "gone"); len(shown) > 0 {
					t.Errorf("%s: the removed collector is shown:\n%s", where, strings.Join(shown, "\n"))
				}
			}
			time.Sleep(2 * time.Millisecond)
			back := time.Now().UnixMilli()
			r.reloadTo(createdDocument(verbose, "kept", "gone"))
			body := read()
			startsFromZero(t, where, body, verbose, 0, back)
			if verbose {
				if got := seriesValue(t, body, "http_exporter_request_series_tracked"); got != 0 {
					t.Errorf("%s: %v requests are tracked, want none", where, got)
				}
			}
			probeConfigReadHook.Store(nil)
		}
	}
}

// staticDocument is a static target file with one target of a collector.
func staticDocument(name, collector, target string) string {
	return "interval: 1m\ntargets:\n  - name: " + name + "\n    collector: " + collector + "\n    target: " + target + "\n"
}

// reloadBoth reloads the server with a configuration file and a static
// target file.
func (r *reloadable) reloadBoth(document, targets string) {
	r.t.Helper()
	r.write(r.path, document)
	r.write(r.targets, targets)
	if err := r.manager.Reload(config.ReloadTriggerSignal); err != nil {
		r.t.Fatal(err)
	}
}

// A static target's scrape uses the configuration its target was read with,
// which may be long before it begins: it waits for a slot. One whose
// collector a reload removed meanwhile, with its target, is counted nowhere
// that is shown, and the collector and target brought back start from zero,
// the target's per-request series too.
func TestAStaticScrapeBegunAfterItsCollectorWasRemovedIsCountedNowhereShown(t *testing.T) {
	name := soonScraped(t, time.Minute, 1)[0]
	target, hits := countingTarget(func(*http.Request) string { return "value=42\n" })
	defer target.Close()
	other := textTarget(t, "value=1\n")
	waiting, resume := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	var once sync.Once
	hook := func(scraped string) {
		if scraped == name && first.CompareAndSwap(false, true) {
			close(waiting)
			<-resume
		}
	}
	slotWaitHook.Store(&hook)
	letGo := func() { once.Do(func() { close(resume) }) }
	t.Cleanup(func() {
		letGo()
		slotWaitHook.Store(nil)
	})
	r := newReloadable(t, createdDocument(true, "kept", "gone"), staticDocument(name, "gone", target.URL))
	read := func() string {
		_, body := answerOf(t, r.server, "/self-metrics", prometheus2Accept)
		return body
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.server.StaticScrapeLoop(ctx)
	}()
	t.Cleanup(func() {
		stop()
		r.server.AbortStaticScrapes()
		<-done
	})
	select {
	case <-waiting:
	case <-time.After(30 * time.Second):
		t.Fatal("the target's scrape never came due")
	}
	r.reloadBoth(createdDocument(true, "kept"), staticDocument("stays", "kept", other.URL))
	removed := read()
	letGo()
	testutil.WaitFor(t, "the scrape begun after the reload to reach its target", func() bool { return hits.Load() >= 1 })
	// The loop returns when the scrapes in flight have ended.
	stop()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the loop did not return")
	}
	for _, answer := range []string{removed, read()} {
		if shown := shownOf(answer, "gone"); len(shown) > 0 {
			t.Errorf("the removed collector is shown:\n%s", strings.Join(shown, "\n"))
		}
	}
	time.Sleep(2 * time.Millisecond)
	back := time.Now().UnixMilli()
	r.reloadBoth(createdDocument(true, "kept", "gone"), staticDocument(name, "gone", target.URL))
	body := read()
	startsFromZero(t, "after the scrape", body, true, 0, back)
	request := `{collector="gone",http_method="GET",url="` + target.URL + `/status"}`
	if got := seriesValue(t, body, "http_exporter_scrapes_total"+request); got != 0 {
		t.Errorf("the request of the target brought back is at %v, want 0", got)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("the target was asked %d times, want once, by the scrape begun after the reload", got)
	}
}

// A static target scraped with a configuration that is no longer the one in
// force, by a caller that does not say when it read it, is counted for its
// collector only when that configuration is the one the statistics follow.
func TestAStaticScrapeWithAConfigurationNoLongerInForceIsCountedNowhereShown(t *testing.T) {
	target := textTarget(t, "value=42\n")
	r := newReloadable(t, createdDocument(true, "kept", "gone"), staticDocument("one", "gone", target.URL))
	read := func() string {
		_, body := answerOf(t, r.server, "/self-metrics", prometheus2Accept)
		return body
	}
	cfg, file := r.manager.InForce()
	r.reloadBoth(createdDocument(true, "kept"), staticDocument("stays", "kept", target.URL))
	removed := read()
	r.server.scrapeTarget(context.Background(), cfg, file.Targets[0])
	for _, answer := range []string{removed, read()} {
		if shown := shownOf(answer, "gone"); len(shown) > 0 {
			t.Errorf("the removed collector is shown:\n%s", strings.Join(shown, "\n"))
		}
	}
	time.Sleep(2 * time.Millisecond)
	back := time.Now().UnixMilli()
	r.reloadBoth(createdDocument(true, "kept", "gone"), staticDocument("one", "gone", target.URL))
	body := read()
	startsFromZero(t, "after the scrape", body, true, 0, back)
	// With the configuration in force the scrape counts as ever.
	cfg, file = r.manager.InForce()
	r.server.scrapeTarget(context.Background(), cfg, file.Targets[0])
	if got := seriesValue(t, read(), `http_exporter_scrapes_total{collector="gone"}`); got != 1 {
		t.Errorf("a scrape with the configuration in force is counted %v times, want once", got)
	}
}

// oldStatsFor is statsFor as it was before a caller said which configuration
// it read: the statistics kept under the name, made when there are none.
func oldStatsFor(s *Server, name string) *serverStats {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	if x := s.stats[name]; x != nil {
		return x
	}
	x := newServerStats(time.Now())
	s.stats[name] = x
	return x
}

// A collector no reload removes has the statistics it had, whoever asks and
// whenever they read the configuration: over every sequence of four reloads
// that keep it, among configurations that remove, add and change the
// collectors beside it and change its own definition, a caller of every
// earlier configuration, the reader of the self-metrics and a caller by name
// are given the very statistics the lookup by name gave. Under the race
// detector it is every thirteenth sequence, 20 of the 256, which have every
// configuration at every step, and every configuration after every other,
// with verbose self-metrics and without.
func TestACollectorThatStaysHasTheStatisticsItHadThroughEveryReload(t *testing.T) {
	kept, gone, added := testutil.Collector("kept", "text"), testutil.Collector("gone", "text"), testutil.Collector("added", "text")
	changed := testutil.Collector("kept", "text")
	changed.Limits.MaxResponseBytes = 2048
	variants := [][]model.Collector{{kept, gone}, {kept}, {kept, gone, added}, {changed, added}}
	const steps = 4
	sequences := 1
	for range steps {
		sequences *= len(variants)
	}
	for sequence := range sequences {
		if alloctest.RaceDetector && sequence%13 != 0 {
			continue
		}
		server := verboseServer(t, sequence%2 == 0, kept, gone)
		server.logger = testutil.QuietLogger(t)
		first := server.statsFor("kept")
		generations := []uint64{server.reconcile().generation}
		for step, rest := 0, sequence; step < steps; step, rest = step+1, rest/len(variants) {
			reloadTo(t, server, variants[rest%len(variants)]...)
			if step%2 == 0 {
				// The reader of the self-metrics is the first to follow the
				// reload at one step, a caller's lookup at the next.
				_, _, stats := server.collectorStats()
				if stats["kept"] != first {
					t.Fatalf("sequence %d, step %d: the self-metrics read other statistics of the collector that stayed", sequence, step)
				}
			}
			generations = append(generations, server.reconcile().generation)
			for age, generation := range generations {
				got := server.statsSince(generation, "kept")
				if want := oldStatsFor(server, "kept"); got != want || got != first || got.retired.Load() {
					t.Fatalf("sequence %d, step %d: a caller that read configuration %d is given other statistics than the lookup by name gave", sequence, step, age)
				}
			}
			if got := server.statsFor("kept"); got != first {
				t.Fatalf("sequence %d, step %d: a caller by name is given other statistics", sequence, step)
			}
		}
	}
}

// The statistics a caller counts in are those of the stay of its collector
// that it read: one that read the collector before a reload removed it is
// given statistics of its own, retired and kept nowhere, when it asks after
// they were dropped, and also when the collector is back by then, whether or
// not the collector brought back has been used; one that read the collector
// brought back is given that collector's, made at its first use; and a
// collector a reload adds has its statistics made by the first to ask.
func TestACallerCountsInTheStatisticsOfTheCollectorItRead(t *testing.T) {
	kept, gone, added := testutil.Collector("kept", "text"), testutil.Collector("gone", "text"), testutil.Collector("added", "text")
	server := verboseServer(t, false, kept, gone)
	server.logger = testutil.QuietLogger(t)
	kept1 := func(name string) *serverStats {
		server.statsMu.Lock()
		defer server.statsMu.Unlock()
		return server.stats[name]
	}
	before := server.reconcile()
	original := server.statsSince(before.generation, "gone")
	if original != kept1("gone") || original.retired.Load() {
		t.Fatal("a caller of the configuration in force is not given the collector's statistics")
	}
	reloadTo(t, server, kept)
	without := server.reconcile()
	if without.generation <= before.generation || !original.retired.Load() {
		t.Fatalf("the reload was followed at generation %d after %d, the removed collector's statistics retired: %v", without.generation, before.generation, original.retired.Load())
	}
	detached := server.statsSince(before.generation, "gone")
	if detached == original || !detached.retired.Load() || kept1("gone") != nil {
		t.Fatalf("a caller that read the removed collector is given statistics that are kept, or not retired: kept %v", kept1("gone"))
	}
	// The collector is back, and nobody has used it yet.
	reloadTo(t, server, kept, gone, added)
	back := server.reconcile()
	for _, generation := range []uint64{before.generation, noGeneration} {
		if early := server.statsSince(generation, "gone"); !early.retired.Load() || kept1("gone") != nil {
			t.Fatalf("a caller of generation %d makes the statistics of the collector brought back at %d", generation, back.generation)
		}
	}
	// It is used by a caller that read it brought back.
	returned := server.statsSince(back.generation, "gone")
	if returned != kept1("gone") || returned == original || returned.retired.Load() || returned.since != back.generation {
		t.Fatal("a caller that read the collector brought back is not given statistics made for it")
	}
	if early := server.statsSince(before.generation, "gone"); early == returned || !early.retired.Load() {
		t.Fatal("a caller that read the removed collector is given the statistics of the one brought back")
	}
	// A later reload that keeps it leaves a caller of the return its statistics.
	reloadTo(t, server, kept, gone)
	if later := server.reconcile(); server.statsSince(back.generation, "gone") != returned || server.statsSince(later.generation, "gone") != returned {
		t.Fatal("the collector brought back lost its statistics at a reload that kept it")
	}
	// The collector the reload added, and removed again, is nobody's now.
	if stale := server.statsSince(back.generation, "added"); !stale.retired.Load() || kept1("added") != nil {
		t.Fatal("a caller that read a collector since removed makes statistics for it")
	}
	if unknown := server.statsFor("never_configured"); !unknown.retired.Load() || kept1("never_configured") != nil {
		t.Fatal("statistics are kept for a name the configuration does not have")
	}
}

// keptCounters is the counter series of the collector kept in a self-metrics
// answer, per-request ones included, without their creation times.
func keptCounters(t *testing.T, server *Server) string {
	t.Helper()
	var lines []string
	for _, line := range shownOf(selfMetrics(t, server), "kept") {
		if name, _, _ := strings.Cut(line, "{"); strings.HasSuffix(name, "_total") || strings.HasSuffix(name, "_count") {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// A collector that is not removed counts as it did, whatever reloads come
// between its probes and while one of them is between reading the
// configuration and taking the statistics: over every sequence of three
// steps, each a probe, a reload that removes the collector beside it, one
// that brings that back and adds another, or a probe held across such a
// reload, its counters, its histogram's count and its request's series are
// those of a server that made the same probes and was never reloaded. Under
// the race detector it is every third sequence, 22 of the 64, which have
// every step first, second and third, and every step after every other,
// with verbose self-metrics and without.
func TestACollectorThatStaysCountsAsItDidThroughReloads(t *testing.T) {
	target := textTarget(t, "value=42\n")
	kept, gone, added := testutil.Collector("kept", "text"), testutil.Collector("gone", "text"), testutil.Collector("added", "text")
	steps := []string{"probe", "remove", "add", "held"}
	const length = 3
	sequences := 1
	for range length {
		sequences *= len(steps)
	}
	for sequence := range sequences {
		if alloctest.RaceDetector && sequence%3 != 0 {
			continue
		}
		verbose := sequence%2 == 0
		reloaded, never := verboseServer(t, verbose, kept, gone), verboseServer(t, verbose, kept, gone)
		reloaded.logger, never.logger = testutil.QuietLogger(t), testutil.QuietLogger(t)
		var taken []string
		for rest := sequence; len(taken) < length; rest /= len(steps) {
			step := steps[rest%len(steps)]
			taken = append(taken, step)
			switch step {
			case "probe":
				probeOnce(t, reloaded, probePath("kept", target.URL, ""), nil)
				probeOnce(t, never, probePath("kept", target.URL, ""), nil)
			case "remove":
				reloadTo(t, reloaded, kept)
			case "add":
				reloadTo(t, reloaded, kept, gone, added)
			case "held":
				reached, resume := holdFirstProbeOf(t, "kept")
				held := probeAsync(context.Background(), reloaded, probePath("kept", target.URL, ""), nil)
				<-reached
				if len(reloaded.manager.Get().Collectors) > 1 {
					reloadTo(t, reloaded, kept)
				} else {
					reloadTo(t, reloaded, kept, gone, added)
				}
				// The reload is followed while the probe is held.
				selfMetrics(t, reloaded)
				resume()
				if outcome := <-held; outcome.code != http.StatusOK {
					t.Fatalf("%v: the held probe was answered %d: %s", taken, outcome.code, outcome.body)
				}
				probeConfigReadHook.Store(nil)
				probeOnce(t, never, probePath("kept", target.URL, ""), nil)
			}
			if got, want := keptCounters(t, reloaded), keptCounters(t, never); got != want {
				t.Fatalf("after %v, verbose %v, the collector that stayed shows\n%s\nand without the reloads\n%s", taken, verbose, got, want)
			}
		}
	}
}

// Probes of a collector that reloads remove and bring back, and of one that
// stays, running while the reloads come: every probe of the one that stays is
// counted, and when the probes have ended and the other is removed and
// brought back once more it starts from zero. Run with the race detector, it
// also shows the statistics and the configuration they follow are read and
// replaced together.
func TestProbesDuringReloadsAreCountedForTheCollectorThatStaysAndNotForOneBroughtBack(t *testing.T) {
	target := textTarget(t, "value=42\n")
	r := newReloadable(t, createdDocument(true, "kept", "gone"), "")
	const probers, rounds = 4, 12
	var wg sync.WaitGroup
	var keptProbes atomic.Int64
	stop := make(chan struct{})
	for i := range probers {
		wg.Go(func() {
			name := []string{"kept", "gone"}[i%2]
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				// Each probe a request of its own, so none shares another's trip:
				// by a timeout of its own, of an hour and more, which none of
				// them is held to.
				request := httptest.NewRequest(http.MethodGet, probePath(name, target.URL, fmt.Sprintf("&timeout=%d.%03ds", 3600+i, n%1000)), nil)
				recorder := httptest.NewRecorder()
				r.server.Handler().ServeHTTP(recorder, request)
				if name == "kept" {
					if recorder.Code != http.StatusOK {
						t.Errorf("a probe of the collector that stays was answered %d: %s", recorder.Code, recorder.Body)
						return
					}
					keptProbes.Add(1)
				}
			}
		})
	}
	for round := range rounds {
		r.reloadTo(createdDocument(true, "kept"))
		if round%2 == 0 {
			selfMetrics(t, r.server)
		}
		r.reloadTo(createdDocument(true, "kept", "gone"))
		if round%3 == 0 {
			selfMetrics(t, r.server)
		}
	}
	close(stop)
	wg.Wait()
	_, body := answerOf(t, r.server, "/self-metrics", prometheus2Accept)
	if got := seriesValue(t, body, `http_exporter_scrapes_total{collector="kept"}`); got != float64(keptProbes.Load()) {
		t.Errorf("%v probes of the collector that stayed are counted, want %d", got, keptProbes.Load())
	}
	r.reloadTo(createdDocument(true, "kept"))
	if _, removed := answerOf(t, r.server, "/self-metrics", prometheus2Accept); len(shownOf(removed, "gone")) > 0 {
		t.Errorf("the removed collector is shown:\n%s", strings.Join(shownOf(removed, "gone"), "\n"))
	}
	time.Sleep(2 * time.Millisecond)
	back := time.Now().UnixMilli()
	r.reloadTo(createdDocument(true, "kept", "gone"))
	_, body = answerOf(t, r.server, "/self-metrics", prometheus2Accept)
	startsFromZero(t, "after the probes", body, true, 0, back)
}
