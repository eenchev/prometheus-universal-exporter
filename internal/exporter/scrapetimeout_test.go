package exporter

import (
	"context"
	"net/http"
	"net/http/httptest"
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
		"":    0,
		"abc": 0,
		"0":   0,
		"-3":  0,
		"NaN": 0,
		"10":  9500 * time.Millisecond,
		// A timeout past an hour counts as an hour, and never overflows.
		"3600":  time.Hour - 500*time.Millisecond,
		"1e9":   time.Hour - 500*time.Millisecond,
		"1e300": time.Hour - 500*time.Millisecond,
		// Infinity is no timeout at all, however it is written, and nor is
		// a number too large to be one.
		"+Inf":     0,
		"Inf":      0,
		"infinity": 0,
		"-Inf":     0,
		"1e400":    0,
		"2.5":      2 * time.Second,
		// A positive timeout is a budget, however small.
		"1e-10": time.Nanosecond,
		"1e-9":  time.Nanosecond,
		"4e-9":  2 * time.Nanosecond,
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

// A probe without a scrape timeout gets --probe.default-timeout, so a target
// that never answers cannot hold it for ever, and the error says whose
// deadline it was.
func TestAProbeWithoutADeadlineGetsTheDefaultTimeout(t *testing.T) {
	fetch.RequestTypes["hanging"] = &fetch.RequestType{
		Name:      "hanging",
		Overrides: []string{"timeout"},
		Validate:  func(*model.Collector) error { return nil },
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

	// The target never answers, so a probe that is answered was ended by
	// the budget its answer names; how long that took is not measured.
	recorder := probeOnce(t, server, "/probe?collector=hung&target=somewhere", nil)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body)
	}
	for _, want := range []string{"ran out of its 200ms budget", "--probe.default-timeout"} {
		if !strings.Contains(recorder.Body.String(), want) {
			t.Fatalf("body %q lacks %q", recorder.Body, want)
		}
	}
	// A timeout parameter cannot lift the default.
	recorder = probeOnce(t, server, "/probe?collector=hung&target=somewhere&timeout=1h", nil)
	if !strings.Contains(recorder.Body.String(), "ran out of its 200ms budget") || !strings.Contains(recorder.Body.String(), "--probe.default-timeout") || !strings.Contains(recorder.Body.String(), "its timeout parameter, 1h0m0s, is capped to it") {
		t.Fatalf("timeout=1h: %d %s", recorder.Code, recorder.Body)
	}
}

// A scrape timeout that is no length of time, Inf, leaves the probe unbounded
// by the header: it gets --probe.default-timeout, as a probe without the
// header does, not the hour the longest timeout counts as. A positive
// timeout is a budget however small: one that rounds to no time at all ends
// the probe at once, rather than counting as no header.
func TestANonFiniteScrapeTimeoutIsNoneAndATinyOneIsABudget(t *testing.T) {
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
	// The caller gives up after five seconds, so a probe given a longer
	// budget than it should have fails the test rather than holding it.
	probe := func(header string) *httptest.ResponseRecorder {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		request := httptest.NewRequest(http.MethodGet, "/probe?collector=hung&target=somewhere", nil).WithContext(ctx)
		request.Header.Set(scrapeTimeoutHeader, header)
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		return recorder
	}
	for _, header := range []string{"Inf", "+Inf", "infinity"} {
		if recorder := probe(header); recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "ran out of its 200ms budget: --probe.default-timeout") {
			t.Errorf("a scrape timeout of %s: answered %d: %s", header, recorder.Code, recorder.Body)
		}
	}
	// With the default a minute, only the header's own budget ends the probe
	// at once.
	server.SetDefaultProbeTimeout(time.Minute)
	if recorder := probe("1e-10"); recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "ran out of its 1ns budget: Prometheus's scrape timeout less --probe.timeout-offset") {
		t.Errorf("a scrape timeout of 1e-10: answered %d: %s", recorder.Code, recorder.Body)
	}
}

// Which deadline a probe gets: Prometheus's when it sent one, else the
// default, which a timeout parameter cannot lift, unless the default is 0.
func TestWhichDeadlineAProbeGets(t *testing.T) {
	withHeader := http.Header{scrapeTimeoutHeader: {"10"}}
	for name, tc := range map[string]struct {
		header     http.Header
		overrides  fetch.RequestOverrides
		noDefault  bool
		wantBudget time.Duration
		wantSource string
	}{
		"Prometheus's scrape timeout":              {header: withHeader, wantBudget: 9500 * time.Millisecond, wantSource: budgetFromScrapeTimeout},
		"the header wins over a timeout":           {header: withHeader, overrides: fetch.RequestOverrides{Timeout: time.Second}, wantBudget: 9500 * time.Millisecond, wantSource: budgetFromScrapeTimeout},
		"no deadline named":                        {wantBudget: 30 * time.Second, wantSource: budgetFromDefault},
		"a short timeout keeps the default budget": {overrides: fetch.RequestOverrides{Timeout: time.Second}, wantBudget: 30 * time.Second, wantSource: budgetFromDefault},
		"a long timeout cannot lift the default":   {overrides: fetch.RequestOverrides{Timeout: 24 * time.Hour}, wantBudget: 30 * time.Second, wantSource: budgetFromDefault + "; its timeout parameter, 24h0m0s, is capped to it"},
		"a default of 0 leaves the probe unbound":  {noDefault: true},
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
