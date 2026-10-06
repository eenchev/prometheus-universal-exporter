//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// What a trip writes to the failure log goes nowhere once reloads have ended
// the stay of the collector it read (reconcile.go), at every place where a
// trip writes there, each shown here by a trip that reaches it: the places of
// an http collector's trip, those only a probe has, and those of the scrape
// loop.

// A static target's scrape that read its collector before reloads removed it
// and brought it back writes nothing to the failure log at any place where
// the trip of an http collector writes there, and its success ends no failure
// of the collector in force: a stage that failed and was carried on under
// error_handling log, a metric rule under error_mode log, label values that
// were not valid UTF-8, sample lines the prometheus decoder left out, a
// credential file that cannot be read, and no slot of the collector's
// max_concurrent_probes within the scrape's time.
func TestALateScrapeWritesNothingToTheFailureLogWhereverItFails(t *testing.T) {
	target, _, failing := failableTarget(t)
	body := func(text string) *string { return &text }
	static := func(collector, address string) model.StaticTarget {
		return model.StaticTarget{Name: "one", Collector: collector, Target: address, Interval: model.Duration(time.Minute)}
	}

	carriesOn := testutil.Collector("carries_on", "text")
	carriesOn.ErrorHandling.OnFetchError = model.ErrorPolicyLog
	checkLateSite(t, lateSite{
		name: "a stage carried on under error_handling log", collector: carriesOn, static: static("carries_on", target.URL),
		fail: func(*lateScrape) { failing.Store(true) }, mend: func(*lateScrape) { failing.Store(false) },
		stage: "http_status", failure: "static target stage failed; continuing", recovery: "static target recovered",
	})

	document, held := changingTarget(t, "")
	checkLateSite(t, lateSite{
		name: "a metric rule under error_mode log", collector: modeCollector("rules", model.ErrorModeLog), static: static("rules", document.URL),
		fail: func(*lateScrape) { held.Store(body(`{"up":1}`)) }, mend: func(*lateScrape) { held.Store(body(`{"up":1,"latency":0.5}`)) },
		stage: "metric", failure: "metric extraction failed", recovery: "metric extraction recovered",
	})

	text, said := changingTarget(t, "")
	checkLateSite(t, lateSite{
		name: "label values that are not valid UTF-8", collector: regexCollector("repaired"), static: static("repaired", text.URL),
		fail: func(*lateScrape) { said.Store(body("v=1 caf\xe9\n")) }, mend: func(*lateScrape) { said.Store(body("v=1 cafe\n")) },
		stage: "utf8", failure: "label values or help text were not valid UTF-8", recovery: "output is valid UTF-8 again",
	})

	exposition, written := changingTarget(t, "")
	checkLateSite(t, lateSite{
		name: "sample lines the prometheus decoder leaves out", collector: passthrough("left_out", "", ""), static: static("left_out", exposition.URL),
		fail: func(*lateScrape) { written.Store(body(micrometerTimer(true))) }, mend: func(*lateScrape) { written.Store(body(micrometerTimer(false))) },
		stage: "decode", failure: "sample lines left out", recovery: "every sample line is part of its family again",
	})

	failing.Store(false)
	token := filepath.Join(t.TempDir(), "token")
	withToken := static("credential", target.URL)
	withToken.Request.BearerTokenFile = token
	checkLateSite(t, lateSite{
		name: "a credential file that cannot be read", collector: testutil.Collector("credential", "text"), static: withToken,
		fail: func(*lateScrape) { _ = os.Remove(token) },
		mend: func(*lateScrape) {
			if err := os.WriteFile(token, []byte("secret\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		stage: "credentials", failure: "static target scrape failed", recovery: "static target recovered",
	})

	limited := testutil.Collector("limited", "text")
	limited.MaxConcurrentProbes = 1
	checkLateSite(t, lateSite{
		name: "no slot of max_concurrent_probes", collector: limited, static: static("limited", target.URL),
		// The collector's only slot is taken, and the scrape's time is over
		// when it comes to wait for it.
		fail: func(l *lateScrape) {
			if full := l.server.trips.tryAcquire("limited", 1); full != nil {
				t.Fatal(full.message)
			}
		},
		ctx: func() context.Context {
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			t.Cleanup(cancel)
			return ctx
		},
		stage: "concurrency", failure: "static target scrape failed",
	})
}

// A probe is held to the same at the places only a probe writes to the
// failure log from. One that read its collector before reloads removed it
// and brought it back, and goes on after: rejected because the collector's
// max_concurrent_probes are all in progress, it is not remembered as
// rejected nor logged above debug level; failing and answered with the stale
// result of the collector in force, under cache.stale_if_error, it is not
// remembered as answered stale; and succeeding, it ends neither the failure
// nor the stale answers of the collector in force, which its own next
// success ends.
func TestALateProbeWritesNothingToTheFailureLogWhereOnlyAProbeDoes(t *testing.T) {
	// late is a server with a probe of collector c held where it has read
	// the configuration, and reloads, each followed, that removed c and
	// brought it back since.
	late := func(t *testing.T, c model.Collector, address string) (server *Server, goOn func() probeOutcome) {
		t.Helper()
		kept := windowCollector("kept", "ttl")
		server, _ = newCacheTestServer(t, kept, c)
		goOn = heldProbe(t, server, c.Name, address)
		reloadTo(t, server, kept)
		selfMetrics(t, server)
		reloadTo(t, server, kept, c)
		selfMetrics(t, server)
		return server, goOn
	}

	t.Run("rejected for max_concurrent_probes", func(t *testing.T) {
		logs := testutil.CaptureLogs(t)
		target, requests, _ := failableTarget(t)
		limited := testutil.Collector("limited", "text")
		limited.MaxConcurrentProbes = 1
		server, goOn := late(t, limited, target.URL)
		if full := server.trips.tryAcquire("limited", 1); full != nil {
			t.Fatal(full.message)
		}
		if outcome := goOn(); outcome.code != http.StatusServiceUnavailable || requests.Load() != 0 {
			t.Fatalf("the late probe was answered %d after %d requests, want 503 and none: %s", outcome.code, requests.Load(), outcome.body)
		}
		if remembered := rememberedOf(server, "limited"); len(remembered) != 0 || strings.Contains(logs.String(), "probe rejected") {
			t.Errorf("the rejection of a probe of a stay that ended is remembered as %v, or logged above debug level:\n%s", remembered, logs)
		}
		probeOnce(t, server, probePath("limited", target.URL, ""), nil)
		if remembered := rememberedOf(server, "limited"); !slices.Equal(remembered, []string{"concurrency x1"}) || strings.Count(logs.String(), "probe rejected") != 1 {
			t.Errorf("the rejection of a probe of the collector in force is remembered as %v, want once, and logged once:\n%s", remembered, logs)
		}
	})

	t.Run("answered with a stale result", func(t *testing.T) {
		logs := testutil.CaptureLogs(t)
		target, _, failing := failableTarget(t)
		server, goOn := late(t, windowCollector("stale", "stale_if_error"), target.URL)
		// The collector in force has a result to fall back on, under the key
		// the late probe has.
		if first := probeOnce(t, server, probePath("stale", target.URL, ""), nil); first.Code != http.StatusOK {
			t.Fatalf("the probe of the collector in force was answered %d: %s", first.Code, first.Body)
		}
		failing.Store(true)
		if outcome := goOn(); outcome.code != http.StatusOK || !strings.Contains(outcome.body, "http_exporter_result_stale 1") {
			t.Fatalf("the late probe was answered %d, want the stale result: %s", outcome.code, outcome.body)
		}
		if remembered := rememberedOf(server, "stale"); len(remembered) != 0 || strings.Contains(logs.String(), "answered with the last successful result") {
			t.Errorf("the stale answer of a probe of a stay that ended is remembered as %v, or logged above debug level:\n%s", remembered, logs)
		}
		probeOnce(t, server, probePath("stale", target.URL, ""), nil)
		if remembered := rememberedOf(server, "stale"); !slices.Equal(remembered, []string{"http_status x1", "stale x1"}) || strings.Count(logs.String(), "answered with the last successful result") != 1 {
			t.Errorf("the stale answer of a probe of the collector in force is remembered as %v, want with its failure, once each, and logged once:\n%s", remembered, logs)
		}
	})

	t.Run("answered with a fresh result again", func(t *testing.T) {
		logs := testutil.CaptureLogs(t)
		target, _, failing := failableTarget(t)
		server, goOn := late(t, windowCollector("stale", "stale_if_error"), target.URL)
		for _, fails := range []bool{false, true} {
			failing.Store(fails)
			if got := probeOnce(t, server, probePath("stale", target.URL, ""), nil); got.Code != http.StatusOK {
				t.Fatalf("the probe of the collector in force, its target failing %v, was answered %d: %s", fails, got.Code, got.Body)
			}
		}
		failing.Store(false)
		if outcome := goOn(); outcome.code != http.StatusOK || !strings.Contains(outcome.body, "http_exporter_result_stale 0") {
			t.Fatalf("the late probe was answered %d, want a fresh result: %s", outcome.code, outcome.body)
		}
		if remembered := rememberedOf(server, "stale"); !slices.Equal(remembered, []string{"http_status x1", "stale x1"}) || strings.Contains(logs.String(), "fresh result again") || strings.Contains(logs.String(), "probe recovered") {
			t.Errorf("after the success of a probe of a stay that ended the collector in force has %v remembered, want its failure and its stale answer, and no recovery logged:\n%s", remembered, logs)
		}
		probeOnce(t, server, probePath("stale", target.URL, ""), nil)
		if remembered := rememberedOf(server, "stale"); len(remembered) != 0 || strings.Count(logs.String(), "fresh result again") != 1 {
			t.Errorf("after a success of its own the collector in force has %v remembered, want nothing, and the fresh result logged once:\n%s", remembered, logs)
		}
	})
}

// loopLog is a server's log, debug lines too, that can be read while the
// scrape loop writes it.
func loopLog(server *Server) *lockedBuffer {
	logs := &lockedBuffer{}
	server.logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return logs
}

// holdFirstScrapeOf makes the scrape loop's first scrape of the target name
// wait where it is about to take a slot, until resume is called. reached is
// closed when the scrape is there.
func holdFirstScrapeOf(t *testing.T, name string) (reached <-chan struct{}, resume func()) {
	t.Helper()
	there, held := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	hook := func(scraped string) {
		if scraped == name && first.CompareAndSwap(false, true) {
			close(there)
			<-held
		}
	}
	slotWaitHook.Store(&hook)
	resume = sync.OnceFunc(func() { close(held) })
	t.Cleanup(func() {
		resume()
		slotWaitHook.Store(nil)
	})
	return there, resume
}

// runLoop runs the server's scrape loop until stop is called, which returns
// when the loop has, with the scrapes in flight ended.
func runLoop(t *testing.T, server *Server) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.StaticScrapeLoop(ctx)
	}()
	stop = sync.OnceFunc(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("the scrape loop did not return")
		}
	})
	t.Cleanup(func() {
		cancel()
		server.AbortStaticScrapes()
		<-done
	})
	return stop
}

// The scrape loop's own scrape is held to the same. One that came due, and
// waited for its slot while reloads removed its target and brought it back,
// with its collector or in the static target file alone, begins when the
// target brought back has been scraped: it is no end of a run of skipped
// scrapes of that target, whose skipped scrape stays remembered; its result
// is not published, so the endpoint keeps the result of the target brought
// back, and that target's last success is its own; and what it is answered
// is not cached under the collector brought back.
func TestTheScrapeLoopsLateScrapeLeavesNothingUnderATargetBroughtBack(t *testing.T) {
	for _, removed := range []string{"the target and its collector", "the target alone"} {
		t.Run(removed, func(t *testing.T) {
			names := soonScraped(t, time.Minute, 2)
			name, stays := names[0], names[1]
			var value atomic.Int64
			value.Store(42)
			target, hits := countingTarget(func(*http.Request) string { return "value=" + strconv.FormatInt(value.Load(), 10) + "\n" })
			defer target.Close()
			other := textTarget(t, "value=1\n")
			reached, resume := holdFirstScrapeOf(t, name)
			// The collector that is removed caches until then, and is back
			// without a cache: the late scrape goes to the target, and what
			// it would cache is the only result under the name. The one that
			// stays caches nothing throughout.
			first, without, back := cachedDocument("kept", "gone"), cachedDocument("kept"), testutil.CollectorsDocument("kept", "gone")
			if removed == "the target alone" {
				first, without = back, back
			}
			r := newReloadable(t, first, staticDocument(name, "gone", target.URL))
			logs := loopLog(r.server)
			stop := runLoop(t, r.server)
			select {
			case <-reached:
			case <-time.After(15 * time.Second):
				t.Fatal("the target's scrape never came due")
			}
			r.reloadBoth(without, staticDocument(stays, "kept", other.URL))
			// The loop has seen the target gone when it scrapes the one in its place.
			testutil.WaitFor(t, "the loop to scrape the target put in its place", func() bool { return len(publishedOf(r.server, stays)) > 0 })
			r.reloadBoth(back, staticDocument(name, "gone", target.URL))
			testutil.WaitFor(t, "the first scrape of the target brought back to be published", func() bool { return len(publishedOf(r.server, name)) > 0 })
			want := []string{"gone_value 42", "http_exporter_target_up 1"}
			if got := publishedOf(r.server, name); !slices.Equal(got, want) || hits.Load() != 1 {
				t.Fatalf("the target brought back has %v published after %d requests, want %v after one", got, hits.Load(), want)
			}
			success := lastSuccessOf(r.server, name)
			// The target brought back has a skipped scrape remembered, and its
			// target answers otherwise from now on.
			key := staticTargetKey("gone", name).aspect(scheduleAspect)
			r.server.failures.failed(r.server.logger, slog.LevelWarn, key, "static target scrape skipped", "schedule", errStillRunning)
			value.Store(7)
			resume()
			testutil.WaitFor(t, "the late scrape to reach the target", func() bool { return hits.Load() == 2 })
			stop()
			if got := publishedOf(r.server, name); !slices.Equal(got, want) {
				t.Errorf("after the late scrape the target brought back has %v published, want %v, its own result", got, want)
			}
			if got := lastSuccessOf(r.server, name); !got.Equal(success) {
				t.Errorf("after the late scrape the target brought back last succeeded at %v, want %v, when its own scrape did", got, success)
			}
			if remembered := rememberedOf(r.server, "gone"); !slices.Equal(remembered, []string{"schedule x1"}) || strings.Contains(logs.String(), "on schedule again") {
				t.Errorf("after the late scrape began the target brought back has %v remembered, want its skipped scrape, and no recovery logged:\n%s", remembered, logs)
			}
			if held := cachedOf(r.server, "gone"); held != 0 {
				t.Errorf("%d results are cached under the collector brought back, want none", held)
			}
		})
	}
}

// A scrape of the loop that finds no slot within its interval, when a reload
// has removed its target meanwhile, with its collector or from the static
// target file alone, is not remembered as skipped, and is logged at debug
// level only, as superseded. The scrape is held where it is about to wait
// for its slot until the reload is made, so the reload comes first however
// late it is made: a slow machine makes the test slower, and no turn of the
// target that is skipped meanwhile, and forgotten with the reload, changes
// what it shows.
func TestAScrapeThatFindsNoSlotAfterItsCollectorWasRemovedIsNotRemembered(t *testing.T) {
	for _, removed := range []string{"the target and its collector", "the target alone"} {
		t.Run(removed, func(t *testing.T) {
			holder, waiter := soonScraped(t, time.Minute, 1)[0], ""
			for _, name := range soonScraped(t, time.Second, 2) {
				if name != holder {
					waiter = name
				}
			}
			held, heldHits, release := holdingTarget(t, 0)
			queued, queuedHits := countingTarget(func(*http.Request) string { return "value=1\n" })
			defer queued.Close()
			// The scrape that will find no slot waits until the reload is
			// made.
			reloaded, waiting := make(chan struct{}), make(chan struct{})
			var once sync.Once
			hook := func(name string) {
				if name == waiter {
					once.Do(func() { close(waiting) })
					<-reloaded
				}
			}
			slotWaitHook.Store(&hook)
			letGo := sync.OnceFunc(func() { close(reloaded) })
			t.Cleanup(func() {
				letGo()
				slotWaitHook.Store(nil)
			})
			targets := func(withWaiter bool) string {
				document := "interval: 1m\nconcurrency: 1\ntargets:\n  - name: " + holder + "\n    collector: kept\n    target: " + held.URL + "\n"
				if withWaiter {
					document += "  - name: " + waiter + "\n    collector: gone\n    target: " + queued.URL + "\n    interval: 1s\n"
				}
				return document
			}
			r := newReloadable(t, testutil.CollectorsDocument("kept", "gone"), targets(true))
			logs := loopLog(r.server)
			stop := runLoop(t, r.server)
			testutil.WaitFor(t, "the other scrape to take the only slot", func() bool { return heldHits.Load() >= 1 })
			select {
			case <-waiting:
			case <-time.After(15 * time.Second):
				t.Fatal("the second scrape never came to wait for the slot")
			}
			if removed == "the target alone" {
				r.reloadBoth(testutil.CollectorsDocument("kept", "gone"), targets(false))
			} else {
				r.reloadBoth(testutil.CollectorsDocument("kept"), targets(false))
			}
			letGo()
			testutil.WaitFor(t, "the scrape without a slot to give up", func() bool { return strings.Contains(logs.String(), `"superseded":true`) })
			close(release)
			stop()
			text := logs.String()
			if remembered := rememberedOf(r.server, "gone"); len(remembered) != 0 || !strings.Contains(text, `"level":"DEBUG","msg":"static target scrape skipped"`) || queuedHits.Load() != 0 {
				t.Errorf("the scrape that found no slot after its target was removed is remembered as %v after %d requests, want nothing, and logged at debug level as superseded:\n%s", remembered, queuedHits.Load(), text)
			}
		})
	}
}
