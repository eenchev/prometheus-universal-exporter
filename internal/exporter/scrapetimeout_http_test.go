//go:build !select_request_types || request_type_http

package exporter

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A target slower than Prometheus will wait gets an answer naming the budget,
// in time for Prometheus to read it.
//
// The target never answers while the test runs, so that the probe is
// answered at all, with the budget it names, is the budget's doing, and it
// is not answered before the budget is over. How soon after is the
// machine's to say and not measured.
func TestProbeAnswersBeforePrometheusGivesUp(t *testing.T) {
	release := make(chan struct{})
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		_, _ = w.Write([]byte("value=1\n"))
	}))
	t.Cleanup(func() { close(release); target.Close() })
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("slow", "text")}}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, "", testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
	server.SetTimeoutOffset(100 * time.Millisecond)

	start := time.Now()
	recorder := probeWithScrapeTimeout(t, server, "collector=slow&target="+url.QueryEscape(target.URL), "0.4")
	elapsed := time.Since(start)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body)
	}
	for _, want := range []string{"context deadline exceeded", "ran out of its 300ms budget", "--probe.timeout-offset"} {
		if !strings.Contains(recorder.Body.String(), want) {
			t.Fatalf("body %q lacks %q", recorder.Body, want)
		}
	}
	if elapsed < 300*time.Millisecond {
		t.Fatalf("answered after %s, before the 300ms budget was over", elapsed)
	}
}

// A probe whose retry wait runs into its deadline reports the target's own
// answer — the http_status stage, the status and the body, in the answer, the
// log and last_status — rather than a bare deadline error.
//
// The wait is an hour, so the budget is what ends the probe, and it ends it
// in the wait once the target's answer has been read within the budget. How
// long that takes is the machine's to say: the budget starts at 100ms, and a
// round in which it ran out before the exporter had the whole answer, which
// the trace of the probe's requests says, is made again with twice the
// budget and a new exporter, up to half a minute. A slow machine makes the
// test slower, where one budget, two seconds, fails it when the target takes
// longer.
func TestAProbeWhoseRetryRunsOutOfTimeReportsTheTargetsAnswer(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "maintenance until noon", http.StatusServiceUnavailable)
	}))
	t.Cleanup(target.Close)
	c := testutil.Collector("retrying", "text")
	c.Request.Retry = model.RetryConfig{Attempts: 2, Backoff: model.Duration(time.Hour)}
	// round probes with the budget, and reports whether the exporter had read
	// the target's answer when the budget ended: only then did the deadline
	// come in the wait before the retry.
	round := func(budget time.Duration) (inTime bool) {
		logs.Reset()
		cfg := &model.Config{Collectors: []model.Collector{c}}
		if err := config.Validate(cfg); err != nil {
			t.Fatal(err)
		}
		server := NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
		server.SetTimeoutOffset(0)
		scrapeTimeout := strconv.FormatFloat(budget.Seconds(), 'f', -1, 64)
		ranOut := "ran out of its " + probeBudget(http.Header{scrapeTimeoutHeader: {scrapeTimeout}}, 0).String() + " budget"
		ctx, trace := fetch.WithRequestTrace(t.Context())
		request := httptest.NewRequest(http.MethodGet, "/probe?collector=retrying&target="+url.QueryEscape(target.URL), nil).WithContext(ctx)
		request.Header.Set(scrapeTimeoutHeader, scrapeTimeout)
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		if sent := trace.Requests(); len(sent) != 1 || sent[0].Outcome != "503 Service Unavailable" {
			return false
		}
		body := recorder.Body.String()
		if recorder.Code != http.StatusBadGateway || !strings.Contains(body, "http_status failed") || !strings.Contains(body, "received HTTP status 503") || !strings.Contains(body, ranOut) {
			t.Fatalf("status=%d body=%s", recorder.Code, body)
		}
		if !strings.Contains(logs.String(), "maintenance until noon") {
			t.Fatalf("the target's explanation is not in the log:\n%s", logs)
		}
		if got := seriesValue(t, selfMetrics(t, server), `http_exporter_scrape_http_status_code{collector="retrying"}`); got != 503 {
			t.Fatalf("last status %v, want 503", got)
		}
		return true
	}
	for budget := 100 * time.Millisecond; !round(budget); budget *= 2 {
		if 2*budget > 30*time.Second {
			t.Fatalf("the exporter did not read the target's answer within a budget of %s", budget)
		}
	}
}

// A static target scrape that runs out of its interval says so, rather than
// only that a context deadline was exceeded.
func TestAStaticTargetScrapeSaysItsIntervalRanOut(t *testing.T) {
	release := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer hang.Close()
	defer close(release)
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}}
	target := model.StaticTarget{Name: "slow", Collector: "text", Target: hang.URL, Interval: model.Duration(time.Second)}
	server := newStaticServer(t, cfg, &model.StaticTargetFile{Interval: model.Duration(time.Second), Targets: []model.StaticTarget{target}})
	var logs bytes.Buffer
	server.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	server.scrapeTarget(ctx, server.manager.Get(), server.manager.StaticTargets()[0])
	if !strings.Contains(logs.String(), "the scrape ran out of its 1s budget: the target's interval, which a scrape must end within") {
		t.Fatalf("%s", logs.String())
	}
}
