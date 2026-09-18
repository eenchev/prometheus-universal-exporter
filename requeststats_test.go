package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func verboseServer(t *testing.T, verbose bool, collectors ...Collector) *Server {
	t.Helper()
	cfg := &Config{Collectors: collectors, Web: WebConfig{SelfMetrics: SelfMetricsConfig{Verbose: verbose}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return NewServer(NewConfigManager(cfg, "", slog.Default()), "python3", slog.Default())
}

func TestVerboseRequestMetricsAreAbsentByDefault(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer target.Close()
	server := verboseServer(t, false, testCollector("quiet", "text"))

	if response := probeOnce(t, server, "/probe?target="+target.URL+"&collector=quiet", nil); response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	exposition := selfMetrics(t, server)
	for _, unwanted := range []string{
		"http_exporter_request_last_status_code",
		"http_exporter_request_last_scrape_timestamp_seconds",
		"http_exporter_request_last_duration_seconds",
		"http_exporter_request_series_capped",
	} {
		if strings.Contains(exposition, unwanted) {
			t.Fatalf("%s must not appear unless verbose self-metrics are configured", unwanted)
		}
	}
	// The ordinary per-collector self-metrics are unaffected.
	if !strings.Contains(exposition, `http_exporter_scrapes_total{collector="quiet"} 1`) {
		t.Fatalf("the standard self-metrics should still be present:\n%s", exposition)
	}
}

func TestVerboseRequestMetricsReportStatusTimestampAndDuration(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer target.Close()
	collector := testCollector("verbose", "text")
	collector.Request.Path = "/v1/status"
	server := verboseServer(t, true, collector)

	before := float64(time.Now().Unix())
	if response := probeOnce(t, server, "/probe?target="+target.URL+"&collector=verbose", nil); response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	exposition := selfMetrics(t, server)
	labels := fmt.Sprintf(`{collector="verbose",method="GET",url="%s/v1/status"}`, target.URL)
	if !strings.Contains(exposition, "http_exporter_request_last_status_code"+labels+" 200") {
		t.Fatalf("status code series missing:\n%s", exposition)
	}
	if !strings.Contains(exposition, "http_exporter_request_series_capped 0") {
		t.Fatalf("the capped indicator should be present and zero:\n%s", exposition)
	}
	timestamp := metricValue(t, exposition, "http_exporter_request_last_scrape_timestamp_seconds"+labels)
	if timestamp < before || timestamp > float64(time.Now().Unix())+1 {
		t.Fatalf("timestamp %v is not around now (%v)", timestamp, before)
	}
	duration := metricValue(t, exposition, "http_exporter_request_last_duration_seconds"+labels)
	if duration <= 0 || duration > 30 {
		t.Fatalf("duration %v is not a plausible scrape duration", duration)
	}
}

func metricValue(t *testing.T, exposition, series string) float64 {
	t.Helper()
	for _, line := range strings.Split(exposition, "\n") {
		if strings.HasPrefix(line, series+" ") {
			var value float64
			if _, err := fmt.Sscanf(strings.TrimPrefix(line, series+" "), "%g", &value); err != nil {
				t.Fatalf("parsing %q: %v", line, err)
			}
			return value
		}
	}
	t.Fatalf("series %q not found in:\n%s", series, exposition)
	return 0
}

// A metric label is persisted by Prometheus and handed to anything federating
// from it, so credentials and query strings must never reach one.
func TestRequestLabelDropsCredentialsAndQuery(t *testing.T) {
	tests := []struct {
		target string
		path   string
		query  map[string]string
		want   string
	}{
		{target: "http://api.example:8080", want: "http://api.example:8080"},
		{target: "http://user:secret@api.example:8080", want: "http://api.example:8080"},
		{target: "https://api.example", path: "/v1/status", want: "https://api.example/v1/status"},
		{target: "http://api.example?token=abc", want: "http://api.example"},
		{target: "http://api.example", query: map[string]string{"token": "abc"}, want: "http://api.example"},
		{target: "http://api.example/base", path: "/v1", query: map[string]string{"t": "1"}, want: "http://api.example/base/v1"},
		{target: "api.example", want: "http://api.example"},
	}
	for _, test := range tests {
		t.Run(test.target+test.path, func(t *testing.T) {
			c := testCollector("labels", "text")
			c.Request.Path = test.path
			c.Request.Query = test.query
			resolved, err := resolveRequestURL(test.target, &c, RequestOverrides{})
			if err != nil {
				t.Fatal(err)
			}
			if got := requestLabelURL(resolved); got != test.want {
				t.Fatalf("label=%q, want %q", got, test.want)
			}
			for _, leaked := range []string{"secret", "token", "abc"} {
				if strings.Contains(requestLabelURL(resolved), leaked) {
					t.Fatalf("label %q leaked %q", requestLabelURL(resolved), leaked)
				}
			}
		})
	}
}

func TestVerboseRequestMetricsSeparateMethodsAndURLs(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer target.Close()
	server := verboseServer(t, true, testCollector("split", "text"))

	for _, query := range []string{
		"&path=/a",
		"&path=/b",
		"&path=/a&method=POST",
		"&path=/a", // a repeat updates the existing series rather than adding one
	} {
		if response := probeOnce(t, server, "/probe?target="+target.URL+"&collector=split"+query, nil); response.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", query, response.Code, response.Body.String())
		}
	}
	exposition := selfMetrics(t, server)
	count := strings.Count(exposition, "http_exporter_request_last_status_code{")
	if count != 3 {
		t.Fatalf("expected one series per URL and method, got %d:\n%s", count, exposition)
	}
	for _, want := range []string{
		fmt.Sprintf(`{collector="split",method="GET",url="%s/a"}`, target.URL),
		fmt.Sprintf(`{collector="split",method="GET",url="%s/b"}`, target.URL),
		fmt.Sprintf(`{collector="split",method="POST",url="%s/a"}`, target.URL),
	} {
		if !strings.Contains(exposition, "http_exporter_request_last_status_code"+want) {
			t.Fatalf("missing series %s:\n%s", want, exposition)
		}
	}
}

func TestVerboseRequestMetricsRecordFailures(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer target.Close()
	server := verboseServer(t, true, testCollector("failing", "text"))

	if response := probeOnce(t, server, "/probe?target="+target.URL+"&collector=failing", nil); response.Code != http.StatusBadGateway {
		t.Fatalf("status=%d", response.Code)
	}
	exposition := selfMetrics(t, server)
	labels := fmt.Sprintf(`{collector="failing",method="GET",url="%s"}`, target.URL)
	if !strings.Contains(exposition, "http_exporter_request_last_status_code"+labels+" 503") {
		t.Fatalf("a failing scrape should record its status code:\n%s", exposition)
	}
}

func TestVerboseRequestMetricsCoverScheduledTargets(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer target.Close()
	cfg := &Config{
		Collectors: []Collector{testCollector("text", "text")},
		OTLP:       otlpConfig("http://collector.invalid/v1/metrics"),
		Web:        WebConfig{SelfMetrics: SelfMetricsConfig{Verbose: true}},
	}
	file := &TargetFile{Targets: []ScheduledTarget{{Name: "one", Collector: "text", Target: target.URL}}}
	server := newScheduledServer(t, cfg, file)
	server.scrapeScheduledTargets(context.Background(), 10*time.Second)

	exposition := selfMetrics(t, server)
	labels := fmt.Sprintf(`{collector="text",method="GET",url="%s"}`, target.URL)
	if !strings.Contains(exposition, "http_exporter_request_last_status_code"+labels+" 200") {
		t.Fatalf("a scheduled scrape should be recorded:\n%s", exposition)
	}
}

func TestVerboseRequestSeriesAreCapped(t *testing.T) {
	tracker := newRequestTracker()
	for i := 0; i < VerboseRequestSeriesLimit; i++ {
		tracker.Record(requestKey{Collector: "c", URL: fmt.Sprintf("http://h/%d", i), Method: "GET"}, requestOutcome{StatusCode: 200})
	}
	samples, capped := tracker.Snapshot()
	if len(samples) != VerboseRequestSeriesLimit || capped {
		t.Fatalf("at the limit: samples=%d capped=%v", len(samples), capped)
	}

	tracker.Record(requestKey{Collector: "c", URL: "http://h/overflow", Method: "GET"}, requestOutcome{StatusCode: 200})
	samples, capped = tracker.Snapshot()
	if len(samples) != VerboseRequestSeriesLimit {
		t.Fatalf("the limit was exceeded: %d series", len(samples))
	}
	if !capped {
		t.Fatal("reaching the limit must be reported, not silent")
	}

	// A request already tracked keeps updating after the limit is reached.
	tracker.Record(requestKey{Collector: "c", URL: "http://h/0", Method: "GET"}, requestOutcome{StatusCode: http.StatusServiceUnavailable})
	samples, _ = tracker.Snapshot()
	for _, sample := range samples {
		if sample.Key.URL == "http://h/0" && sample.Outcome.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("an existing series stopped updating at the limit: %+v", sample.Outcome)
		}
	}
	if VerboseRequestSeriesLimit != 1000 {
		t.Fatalf("VerboseRequestSeriesLimit=%d, want the documented 1000", VerboseRequestSeriesLimit)
	}
}

func TestVerboseCappedIndicatorIsExposed(t *testing.T) {
	server := verboseServer(t, true, testCollector("capped", "text"))
	for i := 0; i <= VerboseRequestSeriesLimit; i++ {
		server.recordRequest("capped", fmt.Sprintf("http://h/%d", i), http.MethodGet, 200, time.Millisecond)
	}
	exposition := selfMetrics(t, server)
	if !strings.Contains(exposition, "http_exporter_request_series_capped 1") {
		t.Fatalf("the capped indicator should read 1 once the limit is reached:\n%s", strings.Join(strings.Split(exposition, "\n")[:5], "\n"))
	}
}

// Verbosity comes from the configuration, so a reload can turn it on and off.
func TestVerbosityFollowsTheConfiguration(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer target.Close()
	quiet := &Config{Collectors: []Collector{testCollector("toggled", "text")}}
	if err := quiet.Validate(); err != nil {
		t.Fatal(err)
	}
	manager := NewConfigManager(quiet, "", slog.Default())
	server := NewServer(manager, "python3", slog.Default())

	probe := "/probe?target=" + target.URL + "&collector=toggled"
	probeOnce(t, server, probe, nil)
	if strings.Contains(selfMetrics(t, server), "http_exporter_request_last_status_code") {
		t.Fatal("verbose series appeared while verbose was off")
	}

	loud := &Config{Collectors: []Collector{testCollector("toggled", "text")}, Web: WebConfig{SelfMetrics: SelfMetricsConfig{Verbose: true}}}
	if err := loud.Validate(); err != nil {
		t.Fatal(err)
	}
	manager.current.Store(loud)
	probeOnce(t, server, probe, nil)
	if !strings.Contains(selfMetrics(t, server), "http_exporter_request_last_status_code") {
		t.Fatal("verbose series did not appear after the configuration enabled them")
	}

	manager.current.Store(quiet)
	exposition := selfMetrics(t, server)
	if strings.Contains(exposition, "http_exporter_request_last_status_code") {
		t.Fatalf("turning verbose off must drop the series rather than leave them stale:\n%s", exposition)
	}
}

func TestSelfMetricsConfigDefaultsToQuiet(t *testing.T) {
	cfg := &Config{Collectors: []Collector{testCollector("default", "text")}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Web.SelfMetrics.Verbose {
		t.Fatal("verbose self-metrics must be opt-in")
	}
}
