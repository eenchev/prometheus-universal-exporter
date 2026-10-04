//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The state kept per collector follows a reload when the reload is made, and
// a static target's scrape publishes its result only for the target and the
// collector it read (reconcile.go).

// publishedOf is what the static targets endpoint holds for the target: its
// series and whether it is up, each with its value, sorted. The duration and
// the time of the last success, which differ from scrape to scrape, are left
// out.
func publishedOf(server *Server, target string) []string {
	server.staticMu.Lock()
	defer server.staticMu.Unlock()
	var out []string
	for _, m := range server.staticResults[target].Metrics {
		if m.Name == "http_exporter_target_scrape_duration_seconds" || m.Name == "http_exporter_target_last_success_timestamp_seconds" {
			continue
		}
		series := []string{m.Name}
		for _, name := range model.SortedKeys(m.Labels) {
			if name != "collector" && name != "static_target" && name != "target" {
				series = append(series, name+"="+m.Labels[name])
			}
		}
		out = append(out, strings.Join(append(series, strconv.FormatFloat(m.Value, 'g', -1, 64)), " "))
	}
	sort.Strings(out)
	return out
}

// lastSuccessOf is when the static target is noted to have last succeeded.
func lastSuccessOf(server *Server, target string) time.Time {
	server.staticMu.Lock()
	defer server.staticMu.Unlock()
	return server.staticLastSuccess[target]
}

// A reload is followed when it is made, by the reload itself: when POST
// /-/reload, SIGHUP or the watch has put a configuration in force, the cached
// results of the collectors it removed or changed are dropped and their
// failures forgotten, those of a changed collector as those of a removed one,
// and a collector it left as it was keeps both, before anything asks for the
// state.
func TestAReloadIsFollowedWhenItIsMade(t *testing.T) {
	r := newReloadable(t, cachedDocument("kept", "gone", "changed"), "")
	logger := testutil.QuietLogger(t)
	set := model.MetricSet{Metrics: []model.Metric{{Name: "demo_value", Type: model.GaugeMetricType, Value: 1}}}
	for _, name := range []string{"kept", "gone", "changed"} {
		r.server.cache.Put(name+"-result", name, set, time.Hour, 0, 0, time.Now())
		r.server.failures.failed(logger, slog.LevelError, failureKey(name, "static target one", ""), "static target scrape failed", "fetch", context.DeadlineExceeded)
	}
	before := r.server.followed.Load()
	r.reloadTo(strings.Replace(cachedDocument("kept", "changed"), "changed_value", "renamed_value", 1))
	// Nothing has asked for the state since the reload.
	followed := r.server.followed.Load()
	if followed.config != r.manager.Get() || followed.generation != before.generation+1 {
		t.Fatalf("after the reload the state follows generation %d, the configuration in force %v, want generation %d of the configuration in force", followed.generation, followed.config == r.manager.Get(), before.generation+1)
	}
	for name, want := range map[string]int{"kept": 1, "gone": 0, "changed": 0} {
		if held, remembered := cachedOf(r.server, name), rememberedOf(r.server, name); held != want || len(remembered) != want {
			t.Errorf("after the reload the collector %s has %d cached results and the failures %v remembered, want %d of each", name, held, remembered, want)
		}
	}
}

// So the failure of a scrape that read a collector before a reload changed
// it goes nowhere as soon as the reload is made, with nothing having asked
// for the state in between: it is logged at debug level only and not
// remembered, and the first failure of the new definition is logged in full,
// as a first failure.
func TestALateFailureGoesNowhereAsSoonAsTheReloadIsMade(t *testing.T) {
	target, _, failing := failableTarget(t)
	r := newReloadable(t, testutil.CollectorsDocument("x"), staticDocument("one", "x", target.URL))
	logs := testutil.CaptureLogs(t)
	r.server.logger = slog.Default()
	cfg, file, generation := r.server.inForce()
	r.reloadBoth(strings.Replace(testutil.CollectorsDocument("x"), "x_value", "renamed_value", 1), staticDocument("one", "x", target.URL))
	failing.Store(true)
	r.server.scrapeTargetSince(context.Background(), cfg, generation, file.Targets[0])
	if remembered := rememberedOf(r.server, "x"); len(remembered) != 0 || strings.Contains(logs.String(), "static target scrape failed") {
		t.Errorf("the failure of the definition the reload replaced is remembered as %v, or logged above debug level:\n%s", remembered, logs)
	}
	cfg, file, generation = r.server.inForce()
	r.server.scrapeTargetSince(context.Background(), cfg, generation, file.Targets[0])
	if remembered := rememberedOf(r.server, "x"); !slices.Equal(remembered, []string{"http_status x1"}) || strings.Count(logs.String(), `"msg":"static target scrape failed"`) != 1 {
		t.Errorf("the first failure of the new definition is remembered as %v, want once, and logged in full once:\n%s", remembered, logs)
	}
}

// A server that was not told of a reload follows it when the state is next
// used, as it always did. A failure that ends before then is one from before
// the reload: logged and remembered. When the reload is followed, the
// failures of a collector it changed are forgotten with its cached results,
// so the first failure of the new definition is logged in full and remembered
// as a first failure, where it was a repeat of the old definition's, logged
// at debug level only.
func TestFollowingAReloadForgetsTheFailuresOfAChangedCollector(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	target, _, failing := failableTarget(t)
	old, changed := windowCollector("x", "ttl"), windowCollector("x", "ttl")
	changed.Metrics[0].Name = "renamed_value"
	static := model.StaticTarget{Name: "one", Collector: "x", Target: target.URL, Interval: model.Duration(time.Minute)}
	server, manager := newCacheTestServer(t, old)
	manager.SetTargets("", &model.StaticTargetFile{Targets: []model.StaticTarget{static}})
	read := server.reconcile()
	// The manager is replaced, so nothing tells the server of the reload.
	reloadTo(t, server, changed)
	failing.Store(true)
	server.scrapeTargetSince(context.Background(), read.config, read.generation, static)
	if remembered := rememberedOf(server, "x"); !slices.Equal(remembered, []string{"http_status x1"}) {
		t.Fatalf("before the reload is followed the failure is remembered as %v, want once", remembered)
	}
	cfg, _, generation := server.inForce()
	if remembered := rememberedOf(server, "x"); len(remembered) != 0 {
		t.Errorf("once the reload is followed the changed collector has the failures %v remembered, want none", remembered)
	}
	server.scrapeTargetSince(context.Background(), cfg, generation, static)
	if remembered := rememberedOf(server, "x"); !slices.Equal(remembered, []string{"http_status x1"}) || strings.Count(logs.String(), `"msg":"static target scrape failed"`) != 2 {
		t.Errorf("the first failure of the new definition is remembered as %v, want once, and each definition's failure logged in full:\n%s", remembered, logs)
	}
}

// A static target's scrape that ends after reloads removed its target and
// its collector and brought both back, the collector with another rule,
// publishes nothing and is noted nowhere: the endpoint keeps the result of
// the first scrape of the target brought back, which had ended sooner, and
// that target's last success is its own. Published, the old result showed
// the series of the definition that is gone under the target's name until
// the target's next turn, a whole interval later.
func TestALateStaticScrapePublishesNothingUnderATargetBroughtBack(t *testing.T) {
	testutil.CaptureLogs(t)
	target := textTarget(t, "value=42\n")
	other := textTarget(t, "value=1\n")
	r := newReloadable(t, testutil.CollectorsDocument("kept", "gone"), staticDocument("one", "gone", target.URL))
	oldCfg, oldFile, oldGeneration := r.server.inForce()
	r.reloadBoth(testutil.CollectorsDocument("kept"), staticDocument("stays", "kept", other.URL))
	back := strings.Replace(testutil.CollectorsDocument("kept", "gone"), "gone_value", "new_value", 1)
	r.reloadBoth(back, staticDocument("one", "gone", target.URL))
	cfg, file, generation := r.server.inForce()
	r.server.scrapeTargetSince(context.Background(), cfg, generation, file.Targets[0])
	want := []string{"http_exporter_target_up 1", "new_value 42"}
	if got := publishedOf(r.server, "one"); !slices.Equal(got, want) {
		t.Fatalf("the target brought back has %v published, want %v", got, want)
	}
	success := lastSuccessOf(r.server, "one")
	r.server.scrapeTargetSince(context.Background(), oldCfg, oldGeneration, oldFile.Targets[0])
	if got := publishedOf(r.server, "one"); !slices.Equal(got, want) {
		t.Errorf("after the scrape of the target that was removed the target brought back has %v published, want %v, its own result", got, want)
	}
	if got := lastSuccessOf(r.server, "one"); !got.Equal(success) {
		t.Errorf("after the scrape of the target that was removed the target brought back last succeeded at %v, want %v", got, success)
	}
	// A result such a scrape comes to publish all the same, a reload having
	// been followed since its outcome was noted, is not stored.
	late := model.MetricSet{Metrics: []model.Metric{{Name: "gone_value", Type: model.GaugeMetricType, Value: 42}}}
	r.server.publishStaticResult(r.server.readAt(oldGeneration), oldFile.Targets[0], otlpResourceIdentity{}, late, time.Time{}, scrapeTime{})
	if got := publishedOf(r.server, "one"); !slices.Equal(got, want) {
		t.Errorf("after the result of the target that was removed came to be published the target brought back has %v published, want %v", got, want)
	}
}

// labelledDocument is a static target file with the target one of the
// collector kept, labelled site when it is not empty, and the target beside
// when it is asked for.
func labelledDocument(address, site string, beside bool) string {
	document := "interval: 1m\ntargets:\n  - name: one\n    collector: kept\n    target: " + address + "\n"
	if site != "" {
		document += "    labels:\n      site: " + site + "\n"
	}
	if beside {
		document += "  - name: beside\n    collector: kept\n    target: " + address + "\n"
	}
	return document
}

// The target and its collector each count: with the collector left as it
// was, a scrape that read a target before a reload of the static target file
// changed it, or before one removed it and another brought it back as it
// was, publishes nothing, and neither does one whose collector a reload
// changed while the target stayed as it was; the result of the first scrape
// of the target now under the name stands. A scrape of a target that reloads
// left as it was, with its collector — the same files read anew, or another
// target added beside it — publishes its result as it always did.
func TestALateStaticScrapePublishesOnlyForTheTargetItRead(t *testing.T) {
	for _, reloads := range []string{"change the target", "remove the target and bring it back", "change the collector", "read the same files anew", "add a target beside it"} {
		testutil.CaptureLogs(t)
		var value atomic.Int64
		value.Store(1)
		target, _ := countingTarget(func(*http.Request) string { return "value=" + strconv.FormatInt(value.Load(), 10) + "\n" })
		document := testutil.CollectorsDocument("kept")
		r := newReloadable(t, document, labelledDocument(target.URL, "", false))
		oldCfg, oldFile, oldGeneration := r.server.inForce()
		kept := true
		switch reloads {
		case "change the target":
			r.reloadBoth(document, labelledDocument(target.URL, "b", false))
			kept = false
		case "remove the target and bring it back":
			r.reloadBoth(document, strings.Replace(labelledDocument(target.URL, "", false), "name: one", "name: another", 1))
			r.reloadBoth(document, labelledDocument(target.URL, "", false))
			kept = false
		case "change the collector":
			r.reloadBoth(strings.Replace(document, "kept_value", "renamed_value", 1), labelledDocument(target.URL, "", false))
			kept = false
		case "read the same files anew":
			r.reloadBoth(document, labelledDocument(target.URL, "", false))
		default:
			r.reloadBoth(document, labelledDocument(target.URL, "", true))
		}
		if since := r.server.followed.Load().defined["kept"]; (since == firstGeneration) != (reloads != "change the collector") {
			t.Fatalf("reloads that %s: the collector is defined from generation %d, and the exporter started at %d", reloads, since, firstGeneration)
		}
		cfg, file, generation := r.server.inForce()
		r.server.scrapeTargetSince(context.Background(), cfg, generation, file.Targets[0])
		// The scrape that read the target before the reloads ends now, and
		// is answered otherwise.
		value.Store(2)
		r.server.scrapeTargetSince(context.Background(), oldCfg, oldGeneration, oldFile.Targets[0])
		want := []string{"http_exporter_target_up 1", "kept_value 1"}
		switch {
		case reloads == "change the target":
			want = []string{"http_exporter_target_up site=b 1", "kept_value site=b 1"}
		case reloads == "change the collector":
			want = []string{"http_exporter_target_up 1", "renamed_value 1"}
		case kept:
			want = []string{"http_exporter_target_up 1", "kept_value 2"}
		}
		if got := publishedOf(r.server, "one"); !slices.Equal(got, want) {
			t.Errorf("reloads that %s: after the scrape that read the target before them the target has %v published, want %v", reloads, got, want)
		}
		target.Close()
	}
}

// A reload of the static target file alone, as the watch makes when only
// that file changed, puts no other configuration in force, and is followed
// all the same, when it is made: the collectors are as they were, defined
// from when they were, and a scrape that read a target the reload changed
// publishes nothing.
func TestAReloadOfTheStaticTargetFileAloneIsFollowed(t *testing.T) {
	testutil.CaptureLogs(t)
	target := textTarget(t, "value=42\n")
	r := newReloadable(t, testutil.CollectorsDocument("kept"), labelledDocument(target.URL, "", false))
	r.manager.SetWatchInterval(5 * time.Millisecond)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.manager.ReloadLoop(ctx)
	}()
	defer func() {
		stop()
		<-done
	}()
	oldCfg, oldFile, oldGeneration := r.server.inForce()
	r.write(r.targets, labelledDocument(target.URL, "b", false))
	testutil.WaitFor(t, "the reload of the static target file to be followed", func() bool { return r.server.followed.Load().targets != oldFile })
	followed := r.server.followed.Load()
	if cfg, file := r.manager.InForce(); cfg != oldCfg || followed.config != oldCfg || followed.targets != file {
		t.Fatalf("after a reload of the static target file alone another configuration is in force %v, and the state follows the configuration %v and the file in force %v, want the same configuration, followed with the file in force", cfg != oldCfg, followed.config == oldCfg, followed.targets == file)
	}
	if followed.generation != oldGeneration+1 || followed.defined["kept"] != firstGeneration || followed.targetsDefined["one"] != followed.generation {
		t.Fatalf("the state follows generation %d, the collector defined from %d and the target from %d; want generation %d, the collector from %d and the target from this one", followed.generation, followed.defined["kept"], followed.targetsDefined["one"], oldGeneration+1, firstGeneration)
	}
	r.server.scrapeTargetSince(context.Background(), oldCfg, oldGeneration, oldFile.Targets[0])
	if got := publishedOf(r.server, "one"); len(got) != 0 {
		t.Errorf("the scrape that read the target before the reload changed it published %v, want nothing", got)
	}
	r.server.scrapeTargetSince(context.Background(), followed.config, followed.generation, followed.targets.Targets[0])
	if got, want := publishedOf(r.server, "one"), []string{"http_exporter_target_up site=b 1", "kept_value site=b 42"}; !slices.Equal(got, want) {
		t.Errorf("the scrape of the target in force published %v, want %v", got, want)
	}
}

// A static target stands for a scrape exactly when it has been in every
// static target file followed since the scrape read it, as it was then, and
// its collector stands: over every sequence of four reloads of the file among
// files that remove, add and change targets, and after each, to a scrape of
// every earlier generation and every target name. A scrape that names no
// configuration finds every target standing.
func TestAStaticTargetStandsWhileEveryFileSinceHasItAsItWas(t *testing.T) {
	testutil.CaptureLogs(t)
	one := model.StaticTarget{Name: "one", Collector: "kept", Target: "http://one.invalid", Interval: model.Duration(time.Minute)}
	two := model.StaticTarget{Name: "two", Collector: "kept", Target: "http://two.invalid", Interval: model.Duration(time.Minute)}
	changed := one
	changed.Labels = map[string]string{"site": "b"}
	slower := one
	slower.Interval = model.Duration(time.Hour)
	variants := [][]model.StaticTarget{{one}, {one, two}, {changed, two}, {slower}, {two}, nil}
	const steps = 4
	sequences := 1
	for range steps {
		sequences *= len(variants)
	}
	for sequence := range sequences {
		server, manager := newCacheTestServer(t, testutil.Collector("kept", "text"))
		manager.SetTargets("", &model.StaticTargetFile{Targets: []model.StaticTarget{one}})
		// history is the targets of every file followed.
		history := [][]model.StaticTarget{{one}}
		generations := []uint64{server.reconcile().generation}
		for step, rest := 0, sequence; step < steps; step, rest = step+1, rest/len(variants) {
			variant := variants[rest%len(variants)]
			manager.SetTargets("", &model.StaticTargetFile{Targets: slices.Clone(variant)})
			history = append(history, variant)
			generations = append(generations, server.reconcile().generation)
			if got, want := generations[len(generations)-1], generations[len(generations)-2]+1; got != want {
				t.Fatalf("sequence %d, step %d: the file is followed at generation %d, want %d", sequence, step, got, want)
			}
			for age, generation := range generations {
				for _, name := range []string{"one", "two", "never_configured"} {
					was := targetNamed(history[age], name)
					want := was != nil
					for _, later := range history[age:] {
						if now := targetNamed(later, name); now == nil || !reflect.DeepEqual(now, was) {
							want = false
						}
					}
					if got := server.readAt(generation).targetStands("kept", name); got != want {
						t.Fatalf("sequence %d, step %d: to a scrape that read file %d the target %s stands %v, want %v", sequence, step, age, name, got, want)
					}
					if server.readAt(generation).targetStands("never_configured", name) {
						t.Fatalf("sequence %d, step %d: the target %s stands for a collector that is not configured", sequence, step, name)
					}
					if !(configRead{}).targetStands("kept", name) {
						t.Fatalf("sequence %d, step %d: to a scrape that names no configuration the target %s does not stand", sequence, step, name)
					}
				}
			}
		}
	}
}

// targetNamed is the target of that name among targets, nil without one.
func targetNamed(targets []model.StaticTarget, name string) *model.StaticTarget {
	for i := range targets {
		if targets[i].Name == name {
			return &targets[i]
		}
	}
	return nil
}

// formerPublishes is whether publishStaticResult published a scrape's result
// before it asked what the scrape read: whenever a target of the name was in
// force.
func formerPublishes(server *Server, name string) bool { return server.staticTargetInForce(name) }

// A scrape made with the configuration and the static target file in force
// publishes exactly when it did: over every file of the sequences above
// followed after another, a scrape of each of its targets, read as it is in
// force, is published, as it was while a target of its name was in force, and
// one of a target the file lacks is not.
func TestAScrapeOfTheTargetsInForcePublishesAsItDid(t *testing.T) {
	testutil.CaptureLogs(t)
	target := textTarget(t, "value=42\n")
	one := model.StaticTarget{Name: "one", Collector: "kept", Target: target.URL, Interval: model.Duration(time.Minute)}
	two := one
	two.Name = "two"
	changed := one
	changed.Labels = map[string]string{"site": "b"}
	variants := [][]model.StaticTarget{{one}, {one, two}, {changed, two}, {two}, nil}
	for first := range variants {
		for second := range variants {
			server, manager := newCacheTestServer(t, testutil.Collector("kept", "text"))
			for _, variant := range [][]model.StaticTarget{variants[first], variants[second]} {
				manager.SetTargets("", &model.StaticTargetFile{Targets: slices.Clone(variant)})
				cfg, file, generation := server.inForce()
				for _, scraped := range []model.StaticTarget{one, two} {
					if now := targetNamed(staticTargetsOf(file), scraped.Name); now != nil {
						scraped = *now
					}
					server.staticMu.Lock()
					delete(server.staticResults, scraped.Name)
					server.staticMu.Unlock()
					server.scrapeTargetSince(context.Background(), cfg, generation, scraped)
					if got, want := len(publishedOf(server, scraped.Name)) > 0, formerPublishes(server, scraped.Name); got != want {
						t.Fatalf("files %d then %d: the scrape of %s with what is in force is published %v, want %v as before", first, second, scraped.Name, got, want)
					}
				}
			}
		}
	}
}

// windowedDocument is a configuration file of the named collectors, each
// keeping its results for an hour, fresh and as a fallback, and the collector
// flip producing the series named by flipSeries.
func windowedDocument(flipSeries string, collectors ...string) string {
	var b strings.Builder
	b.WriteString("collectors:\n")
	for _, name := range collectors {
		document := strings.Replace(testutil.CollectorYAML(name), "    transform:\n", "    cache:\n      ttl: 1h\n      stale_if_error: 1h\n    transform:\n", 1)
		b.WriteString(strings.Replace(document, "flip_value", flipSeries, 1))
	}
	return b.String()
}

// Probes of three collectors, the scrape loop over a static target of each,
// and reads of the self-metrics and of the static targets endpoint, all
// running while reloads remove one collector and bring it back, change
// another and change it back, and load the same files anew: when the last
// reload has removed the one and changed the other, the collector no reload
// touched is defined from the start and has results cached, the removed one
// has nothing cached and nothing remembered, every result cached under the
// changed one is of its definition in force, and so is the result its static
// target has published once that target's first scrape after the reload has
// ended. Run with the race detector, it also shows that the probes, the
// scrapes and the reloads that follow them write and replace the state
// together.
func TestProbesScrapesAndReloadsTogetherLeaveOnlyWhatIsInForce(t *testing.T) {
	testutil.CaptureLogs(t)
	var asked atomic.Int64
	target, _ := countingTarget(func(*http.Request) string { return "value=42\n" })
	defer target.Close()
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// One answer in five fails, so failures are remembered and recovered
		// from throughout.
		if asked.Add(1)%5 == 0 {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer failing.Close()
	names := soonScraped(t, time.Second, 3)
	targets := func(withGone bool) string {
		document := "interval: 1s\nconcurrency: 2\ntargets:\n"
		document += "  - name: " + names[0] + "\n    collector: kept\n    target: " + failing.URL + "\n"
		document += "  - name: " + names[1] + "\n    collector: flip\n    target: " + target.URL + "\n"
		if withGone {
			document += "  - name: " + names[2] + "\n    collector: gone\n    target: " + failing.URL + "\n"
		}
		return document
	}
	r := newReloadable(t, windowedDocument("flip_value", "kept", "flip", "gone"), targets(true))
	stopLoop := runLoop(t, r.server)
	const probers, rounds = 4, 8
	var wg sync.WaitGroup
	var ended [probers]atomic.Int64
	// probed waits until every prober has ended two probes more.
	probed := func() {
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
	quit := make(chan struct{})
	for i := range probers {
		wg.Go(func() {
			for n := 0; ; n++ {
				select {
				case <-quit:
					return
				default:
				}
				// A few requests of each collector, so some probes share a
				// trip or a cached result and some make their own.
				collector := []string{"kept", "flip", "gone"}[n%3]
				request := httptest.NewRequest(http.MethodGet, probePath(collector, failing.URL, fmt.Sprintf("&timeout=%d.%03ds", 5+i, n%7)), nil)
				r.server.Handler().ServeHTTP(httptest.NewRecorder(), request)
				ended[i].Add(1)
			}
		})
	}
	wg.Go(func() {
		for {
			select {
			case <-quit:
				return
			default:
			}
			selfMetrics(t, r.server)
			r.server.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/static-targets", nil))
		}
	})
	for range rounds {
		r.reloadBoth(windowedDocument("flip_other", "kept", "flip"), targets(false))
		probed()
		r.reloadBoth(windowedDocument("flip_value", "kept", "flip", "gone"), targets(true))
		probed()
		// The same files again: nothing changed.
		r.reloadBoth(windowedDocument("flip_value", "kept", "flip", "gone"), targets(true))
		probed()
	}
	r.reloadBoth(windowedDocument("flip_other", "kept", "flip"), targets(false))
	testutil.WaitFor(t, "the changed collector's target to be scraped as it is in force", func() bool {
		return slices.Contains(publishedOf(r.server, names[1]), "flip_other 42")
	})
	close(quit)
	wg.Wait()
	stopLoop()
	followed := r.server.followed.Load()
	if followed.config != r.manager.Get() || followed.defined["kept"] != firstGeneration {
		t.Errorf("the state follows the configuration in force %v, the collector no reload changed defined from generation %d, want from %d", followed.config == r.manager.Get(), followed.defined["kept"], firstGeneration)
	}
	if held, remembered := cachedOf(r.server, "gone"), rememberedOf(r.server, "gone"); held != 0 || len(remembered) != 0 {
		t.Errorf("the removed collector has %d cached results and the failures %v remembered", held, remembered)
	}
	if held := cachedOf(r.server, "kept"); held <= 0 {
		t.Errorf("the collector no reload changed has %d cached results, want some", held)
	}
	r.server.cache.mu.Lock()
	for _, entry := range r.server.cache.entries {
		for _, m := range entry.set.Metrics {
			if entry.collector == "flip" && m.Name != "flip_other" {
				t.Errorf("the changed collector holds a result of a former definition: the series %s", m.Name)
			}
		}
	}
	r.server.cache.mu.Unlock()
	if got := publishedOf(r.server, names[1]); !slices.Contains(got, "flip_other 42") || slices.Contains(got, "flip_value 42") {
		t.Errorf("the changed collector's target has %v published, want the series of its definition in force and none of the former", got)
	}
	if got := publishedOf(r.server, names[2]); len(got) != 0 && r.server.staticTargetInForce(names[2]) {
		t.Errorf("the removed collector's target is in force with %v published", got)
	}
}
