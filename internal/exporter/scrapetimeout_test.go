package exporter

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
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
