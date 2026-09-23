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

// verboseOnlyNames are the series that exist only while verbose self-metrics
// are configured. The self-metric families themselves are always exported; what
// verbose adds is a second, labelled copy of them per request.
var verboseOnlyNames = []string{
	"http_exporter_request_series_capped",
	"http_exporter_request_series_tracked",
	"http_exporter_request_last_scrape_timestamp_seconds",
	"http_exporter_collector_scrape_duration_seconds",
	"http_exporter_python_workers",
	"http_exporter_python_worker_starts_total",
	"http_exporter_python_worker_start_failures_total",
	"http_exporter_python_worker_stops_total",
	"http_exporter_python_runs_total",
	"http_exporter_python_pool_workers",
	"http_exporter_python_pool_worker_starts_total",
	"http_exporter_python_pool_worker_start_failures_total",
	"http_exporter_python_pool_worker_stops_total",
	"http_exporter_python_pool_runs_total",
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
	for _, unwanted := range verboseOnlyNames {
		if strings.Contains(exposition, unwanted) {
			t.Fatalf("%s must not appear unless verbose self-metrics are configured", unwanted)
		}
	}
	if strings.Contains(exposition, "http_method=") || strings.Contains(exposition, "url=") {
		t.Fatalf("no self-metric should carry a request label while verbose is off:\n%s", exposition)
	}
	// The ordinary per-collector self-metrics are unaffected.
	if !strings.Contains(exposition, `http_exporter_scrapes_total{collector="quiet"} 1`) {
		t.Fatalf("the standard self-metrics should still be present:\n%s", exposition)
	}
}

// Verbose does not replace the per-collector series; it publishes the same
// families a second time, broken down by the request that produced them.
func TestVerboseRepublishesEverySelfMetricPerRequest(t *testing.T) {
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
	labels := fmt.Sprintf(`{collector="verbose",http_method="GET",url="%s/v1/status"}`, target.URL)

	// The per-collector series are still there, unlabelled and unchanged.
	if !strings.Contains(exposition, `http_exporter_scrapes_total{collector="verbose"} 1`) {
		t.Fatalf("the per-collector series must survive verbose mode:\n%s", exposition)
	}
	// Every family except the collector-wide cache size is republished.
	for _, name := range verboseRequestSeriesNames() {
		if !strings.Contains(exposition, name+labels) {
			t.Fatalf("%s is not published per request:\n%s", name, exposition)
		}
	}
	if strings.Contains(exposition, "http_exporter_cache_entries"+labels) {
		t.Fatal("cache entries belong to a collector's cache, not to one request")
	}
	if got := metricValue(t, exposition, "http_exporter_scrapes_total"+labels); got != 1 {
		t.Fatalf("per-request scrapes=%v, want 1", got)
	}
	if got := metricValue(t, exposition, "http_exporter_scrape_http_status_code"+labels); got != http.StatusOK {
		t.Fatalf("per-request status=%v, want 200", got)
	}
	if got := metricValue(t, exposition, "http_exporter_scrape_duration_seconds"+labels); got <= 0 || got > 30 {
		t.Fatalf("per-request duration %v is not a plausible scrape duration", got)
	}
	timestamp := metricValue(t, exposition, "http_exporter_request_last_scrape_timestamp_seconds"+labels)
	if timestamp < before || timestamp > float64(time.Now().Unix())+1 {
		t.Fatalf("timestamp %v is not around now (%v)", timestamp, before)
	}
	if !strings.Contains(exposition, "http_exporter_request_series_capped 0") {
		t.Fatalf("the capped indicator should be present and zero:\n%s", exposition)
	}
}

// A metric family may be described once. Publishing the same families twice,
// unlabelled and per request, must not repeat their HELP or TYPE lines: a
// second one makes the whole exposition invalid to a Prometheus parser.
func TestVerboseExpositionDeclaresEachFamilyOnce(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer target.Close()
	server := verboseServer(t, true, testCollector("declared", "text"))
	probeOnce(t, server, "/probe?target="+target.URL+"&collector=declared", nil)
	probeOnce(t, server, "/probe?target="+target.URL+"&collector=declared&path=/other", nil)

	seen := map[string]int{}
	for _, line := range strings.Split(selfMetrics(t, server), "\n") {
		if !strings.HasPrefix(line, "# HELP ") && !strings.HasPrefix(line, "# TYPE ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			t.Fatalf("malformed metadata line %q", line)
		}
		seen[fields[1]+" "+fields[2]]++
	}
	for key, count := range seen {
		if count > 1 {
			t.Errorf("%q is declared %d times; a family may be described once", key, count)
		}
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
	if count := strings.Count(exposition, "http_exporter_scrapes_total{collector=\"split\",http_method="); count != 3 {
		t.Fatalf("expected one series per URL and method, got %d:\n%s", count, exposition)
	}
	for _, want := range []string{
		fmt.Sprintf(`{collector="split",http_method="GET",url="%s/a"}`, target.URL),
		fmt.Sprintf(`{collector="split",http_method="GET",url="%s/b"}`, target.URL),
		fmt.Sprintf(`{collector="split",http_method="POST",url="%s/a"}`, target.URL),
	} {
		if !strings.Contains(exposition, "http_exporter_scrapes_total"+want) {
			t.Fatalf("missing series %s:\n%s", want, exposition)
		}
	}
	// The repeated probe lands on the existing series, and the collector total
	// still counts all four.
	if got := metricValue(t, exposition, fmt.Sprintf(`http_exporter_scrapes_total{collector="split",http_method="GET",url="%s/a"}`, target.URL)); got != 2 {
		t.Fatalf("repeated probe count=%v, want 2", got)
	}
	if got := metricValue(t, exposition, `http_exporter_scrapes_total{collector="split"}`); got != 4 {
		t.Fatalf("collector total=%v, want 4", got)
	}
}

// The two views are raised through the same recorder, so the collector's totals
// must equal the sum of its requests'. A counter raised on one and not the
// other is the failure this guards against.
func TestPerCollectorTotalsMatchTheSumOfItsRequests(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/bad") {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer ok.Close()
	server := verboseServer(t, true, testCollector("summed", "text"))
	for _, path := range []string{"/a", "/a", "/b", "/bad"} {
		probeOnce(t, server, "/probe?target="+ok.URL+"&collector=summed&path="+path, nil)
	}

	exposition := selfMetrics(t, server)
	for _, name := range []string{
		"http_exporter_scrapes_total",
		"http_exporter_scrape_success_total",
		"http_exporter_decode_success_total",
		"http_exporter_metrics_emitted_total",
	} {
		total := metricValue(t, exposition, name+`{collector="summed"}`)
		sum := 0.0
		for _, line := range strings.Split(exposition, "\n") {
			if strings.HasPrefix(line, name+`{collector="summed",http_method=`) {
				sum += metricValue(t, exposition, strings.Fields(line)[0])
			}
		}
		if total != sum {
			t.Errorf("%s: collector total %v but the requests sum to %v", name, total, sum)
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
	labels := fmt.Sprintf(`{collector="failing",http_method="GET",url="%s"}`, target.URL)
	if got := metricValue(t, exposition, "http_exporter_scrape_http_status_code"+labels); got != http.StatusServiceUnavailable {
		t.Fatalf("status=%v, want 503 on the failing request's own series", got)
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
	labels := fmt.Sprintf(`{collector="text",http_method="GET",url="%s"}`, target.URL)
	if got := metricValue(t, exposition, "http_exporter_scrape_http_status_code"+labels); got != http.StatusOK {
		t.Fatalf("a scheduled scrape should be recorded on its own series:\n%s", exposition)
	}
	if got := metricValue(t, exposition, "http_exporter_scrapes_total"+labels); got != 1 {
		t.Fatalf("scheduled scrapes=%v, want 1", got)
	}
}

// A scheduled target's request is fully described by the configuration, so its
// series exist from the first scrape of /self-metrics rather than only after
// the target has been collected once.
func TestScheduledTargetsAreVisibleBeforeTheirFirstCollection(t *testing.T) {
	cfg := &Config{
		Collectors: []Collector{testCollector("text", "text")},
		OTLP:       otlpConfig("http://collector.invalid/v1/metrics"),
		Web:        WebConfig{SelfMetrics: SelfMetricsConfig{Verbose: true}},
	}
	file := &TargetFile{Targets: []ScheduledTarget{{Name: "one", Collector: "text", Target: "http://api.example:8080"}}}
	server := newScheduledServer(t, cfg, file)

	exposition := selfMetrics(t, server)
	labels := `{collector="text",http_method="GET",url="http://api.example:8080"}`
	if got := metricValue(t, exposition, "http_exporter_scrapes_total"+labels); got != 0 {
		t.Fatalf("a configured target should be listed with no scrapes yet, got %v:\n%s", got, exposition)
	}
	// A request that has never been scraped reports no timestamp rather than
	// the zero instant, which would read as a scrape in 1970.
	if got := metricValue(t, exposition, "http_exporter_request_last_scrape_timestamp_seconds"+labels); got != 0 {
		t.Fatalf("timestamp=%v before the first collection, want 0", got)
	}
	if got := metricValue(t, exposition, "http_exporter_request_series_tracked"); got != 1 {
		t.Fatalf("tracked=%v, want the one configured target", got)
	}
}

// A response served from the collector cache is not a scrape: no request
// reaches the target, so the request's last-scrape timestamp and status must
// keep describing the scrape that filled the cache.
func TestACachedResponseDoesNotOverwriteTheLastScrape(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer target.Close()
	collector := testCollector("cached", "text")
	collector.Cache = Duration(time.Hour)
	server := verboseServer(t, true, collector)

	probe := "/probe?target=" + target.URL + "&collector=cached"
	if response := probeOnce(t, server, probe, nil); response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	labels := fmt.Sprintf(`{collector="cached",http_method="GET",url="%s"}`, target.URL)
	scrapedAt := metricValue(t, selfMetrics(t, server), "http_exporter_request_last_scrape_timestamp_seconds"+labels)

	time.Sleep(20 * time.Millisecond)
	if response := probeOnce(t, server, probe, nil); response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	second := selfMetrics(t, server)
	if got := metricValue(t, second, "http_exporter_cache_hits_total"+labels); got != 1 {
		t.Fatalf("the second probe should be a cache hit on the request's own series, got %v:\n%s", got, second)
	}
	if got := metricValue(t, second, "http_exporter_scrape_http_status_code"+labels); got != http.StatusOK {
		t.Fatalf("status=%v after a cache hit, want the 200 of the scrape that filled the cache", got)
	}
	if got := metricValue(t, second, "http_exporter_request_last_scrape_timestamp_seconds"+labels); got != scrapedAt {
		t.Fatalf("timestamp moved to %v on a cache hit, want %v", got, scrapedAt)
	}
}

func TestVerboseRequestSeriesAreCapped(t *testing.T) {
	tracker := newRequestTracker()
	for i := 0; i < VerboseRequestSeriesLimit; i++ {
		stats := tracker.statsFor(requestKey{Collector: "c", URL: fmt.Sprintf("http://h/%d", i), Method: "GET"})
		if stats == nil {
			t.Fatalf("request %d was refused below the limit", i)
		}
		stats.probes++
	}
	samples, capped := tracker.Snapshot()
	if len(samples) != VerboseRequestSeriesLimit || capped {
		t.Fatalf("at the limit: samples=%d capped=%v", len(samples), capped)
	}

	if tracker.statsFor(requestKey{Collector: "c", URL: "http://h/overflow", Method: "GET"}) != nil {
		t.Fatal("a new request past the limit must be refused")
	}
	samples, capped = tracker.Snapshot()
	if len(samples) != VerboseRequestSeriesLimit {
		t.Fatalf("the limit was exceeded: %d series", len(samples))
	}
	if !capped {
		t.Fatal("reaching the limit must be reported, not silent")
	}

	// A request already tracked keeps updating after the limit is reached.
	existing := tracker.statsFor(requestKey{Collector: "c", URL: "http://h/0", Method: "GET"})
	if existing == nil {
		t.Fatal("a tracked request must keep updating past the limit")
	}
	existing.lastStatus = http.StatusServiceUnavailable
	samples, _ = tracker.Snapshot()
	for _, sample := range samples {
		if sample.Key.URL == "http://h/0" && sample.Values.lastStatus != http.StatusServiceUnavailable {
			t.Fatalf("an existing series stopped updating at the limit: %+v", sample.Values)
		}
	}
	if VerboseRequestSeriesLimit != 1000 {
		t.Fatalf("VerboseRequestSeriesLimit=%d, want the documented 1000", VerboseRequestSeriesLimit)
	}
}

func TestVerboseCappedIndicatorIsExposed(t *testing.T) {
	server := verboseServer(t, true, testCollector("capped", "text"))
	for i := 0; i <= VerboseRequestSeriesLimit; i++ {
		server.registerRequest("capped", fmt.Sprintf("http://h/%d", i), http.MethodGet)
	}
	exposition := selfMetrics(t, server)
	if got := metricValue(t, exposition, "http_exporter_request_series_capped"); got != 1 {
		t.Fatalf("the capped indicator should read 1 once the limit is reached, got %v", got)
	}
	if got := metricValue(t, exposition, "http_exporter_request_series_tracked"); got != VerboseRequestSeriesLimit {
		t.Fatalf("tracked=%v, want the limit", got)
	}
}

// The tracked count is what tells an operator that verbose self-metrics are on
// but nothing has been collected yet, rather than leaving the cap indicator
// alone on the page with nothing to explain it.
func TestTrackedSeriesCountIsReported(t *testing.T) {
	server := verboseServer(t, true, testCollector("counted", "text"))
	if got := metricValue(t, selfMetrics(t, server), "http_exporter_request_series_tracked"); got != 0 {
		t.Fatalf("tracked=%v before any request, want 0", got)
	}
	server.registerRequest("counted", "http://a.example", http.MethodGet)
	server.registerRequest("counted", "http://b.example", http.MethodPost)
	server.registerRequest("counted", "http://a.example", http.MethodGet)
	if got := metricValue(t, selfMetrics(t, server), "http_exporter_request_series_tracked"); got != 2 {
		t.Fatalf("tracked=%v, want the two distinct requests", got)
	}
}

// Registering is not recording: it must never invent a scrape or overwrite one.
func TestRegisteringARequestDoesNotClaimAScrape(t *testing.T) {
	tracker := newRequestTracker()
	key := requestKey{Collector: "c", URL: "http://h", Method: "GET"}
	tracker.statsFor(key)
	samples, _ := tracker.Snapshot()
	if len(samples) != 1 || !samples[0].Values.lastScrape.IsZero() || samples[0].Values.probes != 0 {
		t.Fatalf("a registered request should start empty: %+v", samples)
	}

	stats := tracker.statsFor(key)
	stats.probes = 3
	stats.lastScrape = time.Now()
	tracker.statsFor(key)
	samples, _ = tracker.Snapshot()
	if len(samples) != 1 || samples[0].Values.probes != 3 || samples[0].Values.lastScrape.IsZero() {
		t.Fatalf("registering an already scraped request must leave it alone: %+v", samples)
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
	if strings.Contains(selfMetrics(t, server), "http_method=") {
		t.Fatal("verbose series appeared while verbose was off")
	}

	loud := &Config{Collectors: []Collector{testCollector("toggled", "text")}, Web: WebConfig{SelfMetrics: SelfMetricsConfig{Verbose: true}}}
	if err := loud.Validate(); err != nil {
		t.Fatal(err)
	}
	manager.current.Store(loud)
	probeOnce(t, server, probe, nil)
	if !strings.Contains(selfMetrics(t, server), "http_method=") {
		t.Fatal("verbose series did not appear after the configuration enabled them")
	}

	manager.current.Store(quiet)
	exposition := selfMetrics(t, server)
	if strings.Contains(exposition, "http_method=") {
		t.Fatalf("turning verbose off must drop the series rather than leave them stale:\n%s", exposition)
	}
	// The per-collector view is unaffected by either switch.
	if got := metricValue(t, exposition, `http_exporter_scrapes_total{collector="toggled"}`); got != 2 {
		t.Fatalf("collector scrapes=%v, want 2 across both settings", got)
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
