//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The loop scrapes each target on its own interval and queues the results,
// with no export running: the export does not decide when targets are scraped.
func TestStaticTargetsAreScrapedOnTheirOwnIntervals(t *testing.T) {
	var fast, slow atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fast" {
			fast.Add(1)
		} else {
			slow.Add(1)
		}
		_, _ = w.Write([]byte("value=1\n"))
	}))
	t.Cleanup(target.Close)
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	// Built directly: validation holds intervals to at least a second, which
	// would make this test slow.
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{Name: "fast", Collector: "text", Target: target.URL + "/fast", Interval: model.Duration(40 * time.Millisecond)},
		{Name: "slow", Collector: "text", Target: target.URL + "/slow", Interval: model.Duration(time.Hour)},
	}}
	manager := config.NewManager(cfg, "", testutil.QuietLogger(t))
	manager.SetTargets("", file)
	server := NewServer(manager, "python3", slog.Default())
	server.logger = testutil.QuietLogger(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.StaticScrapeLoop(ctx)
	}()
	testutil.WaitFor(t, "the fast target to be scraped several times", func() bool { return fast.Load() >= 4 })
	cancel()
	<-done
	if got := slow.Load(); got > 1 {
		t.Errorf("the hourly target was scraped %d times", got)
	}
	if results := server.staticTargetResults(); len(results) == 0 || results[0].name != "fast" {
		t.Errorf("the scrapes published %+v, want the fast target's result for the static targets endpoint", results)
	}
	// Neither target sets export_via_otlp, so nothing waits for an export.
	if _, ok := pendingValue(server, "http_exporter_target_up"); ok {
		t.Error("a target without export_via_otlp was queued for OTLP")
	}
}

// The export loop only delivers: it scrapes no target.
func TestTheExportLoopDoesNotScrape(t *testing.T) {
	var requests atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte("value=1\n"))
	}))
	t.Cleanup(target.Close)
	var exports atomic.Int64
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		readOTLPBody(t, r)
		exports.Add(1)
	}))
	t.Cleanup(endpoint.Close)
	server := newStaticServer(t, &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlpConfig(endpoint.URL)},
		&model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "t", Collector: "text", Target: target.URL}}})
	// Shorter than a configuration may set, which is 1s at the least, so a
	// few exports do not take the test seconds; set before the loop starts.
	server.manager.Get().OTLP.Interval = model.Duration(20 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.OTLPExportLoop(ctx)
	}()
	testutil.WaitFor(t, "a few exports", func() bool { return exports.Load() >= 3 })
	cancel()
	<-done
	if got := requests.Load(); got != 0 {
		t.Fatalf("the export loop scraped the target %d times", got)
	}
}

// The scrape loop scrapes at most the file's concurrency at once.
func TestTheScrapeLoopKeepsToTheFilesConcurrency(t *testing.T) {
	var inFlight, most, scraped atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		now := inFlight.Add(1)
		for {
			seen := most.Load()
			if now <= seen || most.CompareAndSwap(seen, now) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		inFlight.Add(-1)
		scraped.Add(1)
		_, _ = w.Write([]byte("value=1\n"))
	}))
	t.Cleanup(target.Close)
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	// Built directly, with intervals under the least validation allows, so
	// every target is first due within 300ms; each scrape outlasts the
	// handler's 50ms, so a scrape in the handler is one the loop is running.
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Concurrency: 2}
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		file.Targets = append(file.Targets, model.StaticTarget{Name: name, Collector: "text", Target: target.URL, Interval: model.Duration(300 * time.Millisecond)})
	}
	manager := config.NewManager(cfg, "", testutil.QuietLogger(t))
	manager.SetTargets("", file)
	server := NewServer(manager, "python3", testutil.QuietLogger(t))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.StaticScrapeLoop(ctx)
	}()
	testutil.WaitFor(t, "every target to be scraped", func() bool { return scraped.Load() >= 5 })
	cancel()
	<-done
	if got := most.Load(); got != 2 {
		t.Fatalf("at most %d scrapes ran at once, want the file's concurrency, 2", got)
	}
}
