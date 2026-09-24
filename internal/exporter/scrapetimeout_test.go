package exporter

import (
	"context"
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
// without the header is unaffected. That a localfile read which has not
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
