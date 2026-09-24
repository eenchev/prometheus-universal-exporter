package exporter

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A probe gives itself Prometheus's scrape timeout less --probe.timeout-offset
// (scrapetimeout.go).

func TestProbeBudget(t *testing.T) {
	for header, want := range map[string]time.Duration{
		"":     0,
		"abc":  0,
		"0":    0,
		"-3":   0,
		"NaN":  0,
		"+Inf": 0,
		"10":   9500 * time.Millisecond,
		"2.5":  2 * time.Second,
		// An offset larger than half the timeout would leave too little.
		"0.6": 300 * time.Millisecond,
		"1":   500 * time.Millisecond,
	} {
		h := http.Header{}
		if header != "" {
			h.Set(scrapeTimeoutHeader, header)
		}
		if got := probeBudget(h, DefaultTimeoutOffset); got != want {
			t.Errorf("%q: budget %s, want %s", header, got, want)
		}
	}
	// Surrounding blanks are ignored.
	if got := probeBudget(http.Header{scrapeTimeoutHeader: {"  2.5  "}}, DefaultTimeoutOffset); got != 2*time.Second {
		t.Errorf("padded header: %s", got)
	}
	h := http.Header{scrapeTimeoutHeader: {"10"}}
	if got := probeBudget(h, 0); got != 10*time.Second {
		t.Errorf("no offset: %s", got)
	}
}

func probeWithScrapeTimeout(t *testing.T, server *Server, query, timeout string) *httptest.ResponseRecorder {
	t.Helper()
	return probeOnce(t, server, "/probe?"+query, http.Header{scrapeTimeoutHeader: {timeout}})
}

// A target slower than Prometheus will wait gets an answer naming the budget,
// in time for Prometheus to read it.
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
	if elapsed < 250*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("answered after %s, want about the 300ms budget", elapsed)
	}
}

// The budget bounds the whole trip whatever the request type, and a probe
// without the header that answers in time is unaffected. That a localfile read which has not
// returned is abandoned at the deadline is pinned in
// fetch/localfile_reads_test.go.
func TestTheBudgetBoundsEveryRequestTypeAndIsOptional(t *testing.T) {
	var stall atomic.Bool
	stall.Store(true)
	fetch.RequestTypes["stalling"] = &fetch.RequestType{
		Name:     "stalling",
		Validate: func(*model.Collector) error { return nil },
		Fetch: func(ctx context.Context, target string, c *model.Collector, _ fetch.RequestOverrides, _ http.Header) (*fetch.HTTPResponse, error) {
			if stall.Load() {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return &fetch.HTTPResponse{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": {"text/plain"}}, Body: []byte("value=5\n"), Target: target, Collector: c.Name}, nil
		},
	}
	t.Cleanup(func() { delete(fetch.RequestTypes, "stalling") })
	c := testutil.Collector("stalled", "text")
	c.Request = model.RequestConfig{Type: "stalling"}
	cfg := &model.Config{Collectors: []model.Collector{c}}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, "", testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
	server.SetTimeoutOffset(0)

	recorder := probeWithScrapeTimeout(t, server, "collector=stalled&target=somewhere", "0.2")
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "200ms budget") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body)
	}

	stall.Store(false)
	if recorder := probeOnce(t, server, "/probe?collector=stalled&target=somewhere", nil); recorder.Code != http.StatusOK {
		t.Fatalf("without the header: status=%d body=%s", recorder.Code, recorder.Body)
	}
	if recorder := probeWithScrapeTimeout(t, server, "collector=stalled&target=somewhere", "10"); recorder.Code != http.StatusOK {
		t.Fatalf("with a generous timeout: status=%d body=%s", recorder.Code, recorder.Body)
	}
}

// A probe that names no deadline gets --probe.default-timeout, so a target
// that never answers cannot hold it for ever, and the error says whose
// deadline it was.
func TestAProbeWithoutADeadlineGetsTheDefaultTimeout(t *testing.T) {
	fetch.RequestTypes["hanging"] = &fetch.RequestType{
		Name:     "hanging",
		Validate: func(*model.Collector) error { return nil },
		Fetch: func(ctx context.Context, _ string, _ *model.Collector, _ fetch.RequestOverrides, _ http.Header) (*fetch.HTTPResponse, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	t.Cleanup(func() { delete(fetch.RequestTypes, "hanging") })
	c := testutil.Collector("hung", "text")
	c.Request = model.RequestConfig{Type: "hanging"}
	cfg := &model.Config{Collectors: []model.Collector{c}}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, "", testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
	server.SetDefaultProbeTimeout(200 * time.Millisecond)

	start := time.Now()
	recorder := probeOnce(t, server, "/probe?collector=hung&target=somewhere", nil)
	elapsed := time.Since(start)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body)
	}
	for _, want := range []string{"ran out of its 200ms budget", "--probe.default-timeout"} {
		if !strings.Contains(recorder.Body.String(), want) {
			t.Fatalf("body %q lacks %q", recorder.Body, want)
		}
	}
	if elapsed > 2*time.Second {
		t.Fatalf("answered after %s, want about the 200ms default", elapsed)
	}
}

// Which deadline a probe gets: Prometheus's when it sent one, else the
// default, unless the probe bounded its request with a timeout parameter or
// the default is 0.
func TestWhichDeadlineAProbeGets(t *testing.T) {
	withHeader := http.Header{scrapeTimeoutHeader: {"10"}}
	for name, tc := range map[string]struct {
		header     http.Header
		overrides  fetch.RequestOverrides
		noDefault  bool
		wantBudget time.Duration
		wantSource string
	}{
		"Prometheus's scrape timeout":             {header: withHeader, wantBudget: 9500 * time.Millisecond, wantSource: budgetFromScrapeTimeout},
		"the header wins over a timeout":          {header: withHeader, overrides: fetch.RequestOverrides{Timeout: time.Second}, wantBudget: 9500 * time.Millisecond, wantSource: budgetFromScrapeTimeout},
		"no deadline named":                       {wantBudget: 30 * time.Second, wantSource: budgetFromDefault},
		"a timeout parameter bounds the request":  {overrides: fetch.RequestOverrides{Timeout: time.Second}},
		"a default of 0 leaves the probe unbound": {noDefault: true},
	} {
		t.Run(name, func(t *testing.T) {
			s := &Server{timeoutOffset: 500 * time.Millisecond, defaultProbeTimeout: 30 * time.Second}
			if tc.noDefault {
				s.defaultProbeTimeout = 0
			}
			budget, source := s.probeDeadline(tc.header, tc.overrides)
			if budget != tc.wantBudget || source != tc.wantSource {
				t.Fatalf("got %s from %q, want %s from %q", budget, source, tc.wantBudget, tc.wantSource)
			}
		})
	}
}

// A probe whose retry wait runs into its deadline reports the target's own
// answer — the http_status stage, the status and the body, in the answer, the
// log and last_status — rather than a bare deadline error.
func TestAProbeWhoseRetryRunsOutOfTimeReportsTheTargetsAnswer(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "maintenance until noon", http.StatusServiceUnavailable)
	}))
	t.Cleanup(target.Close)
	c := testutil.Collector("retrying", "text")
	c.Request.Retry = model.RetryConfig{Attempts: 2, Backoff: model.Duration(5 * time.Second)}
	cfg := &model.Config{Collectors: []model.Collector{c}}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
	server.SetTimeoutOffset(0)

	recorder := probeWithScrapeTimeout(t, server, "collector=retrying&target="+url.QueryEscape(target.URL), "0.3")
	body := recorder.Body.String()
	if recorder.Code != http.StatusBadGateway || !strings.Contains(body, "http_status failed") || !strings.Contains(body, "received HTTP status 503") || !strings.Contains(body, "ran out of its 300ms budget") {
		t.Fatalf("status=%d body=%s", recorder.Code, body)
	}
	if !strings.Contains(logs.String(), "maintenance until noon") {
		t.Fatalf("the target's explanation is not in the log:\n%s", logs)
	}
	if got := seriesValue(t, selfMetrics(t, server), `http_exporter_scrape_http_status_code{collector="retrying"}`); got != 503 {
		t.Fatalf("last status %v, want 503", got)
	}
}
