package exporter

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

func verboseServer(t *testing.T, verbose bool, collectors ...model.Collector) *Server {
	t.Helper()
	cfg := &model.Config{Collectors: collectors, Web: model.WebConfig{SelfMetrics: model.SelfMetricsConfig{Verbose: verbose}}}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	return NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
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
	"http_exporter_python_pool_runs_waiting",
	"http_exporter_trips_waiting",
}

func TestVerboseRequestMetricsAreAbsentByDefault(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer target.Close()
	server := verboseServer(t, false, testutil.Collector("quiet", "text"))

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
	collector := testutil.Collector("verbose", "text")
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
		if _, typed := requestTypeFamilies[name]; typed {
			continue
		}
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
	server := verboseServer(t, true, testutil.Collector("declared", "text"))
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

func TestVerboseRequestMetricsSeparateMethodsAndURLs(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer target.Close()
	server := verboseServer(t, true, testutil.Collector("split", "text"))

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
	server := verboseServer(t, true, testutil.Collector("summed", "text"))
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
	server := verboseServer(t, true, testutil.Collector("failing", "text"))

	if response := probeOnce(t, server, "/probe?target="+target.URL+"&collector=failing", nil); response.Code != http.StatusBadGateway {
		t.Fatalf("status=%d", response.Code)
	}
	exposition := selfMetrics(t, server)
	labels := fmt.Sprintf(`{collector="failing",http_method="GET",url="%s"}`, target.URL)
	if got := metricValue(t, exposition, "http_exporter_scrape_http_status_code"+labels); got != http.StatusServiceUnavailable {
		t.Fatalf("status=%v, want 503 on the failing request's own series", got)
	}
}

func TestVerboseRequestMetricsCoverStaticTargets(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer target.Close()
	cfg := &model.Config{
		Collectors: []model.Collector{testutil.Collector("text", "text")},
		OTLP:       otlpConfig("http://collector.invalid/v1/metrics"),
		Web:        model.WebConfig{SelfMetrics: model.SelfMetricsConfig{Verbose: true}},
	}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "one", Collector: "text", Target: target.URL}}}
	server := newStaticServer(t, cfg, file)
	server.scrapeStaticTargets(context.Background(), 10*time.Second)

	exposition := selfMetrics(t, server)
	labels := fmt.Sprintf(`{collector="text",http_method="GET",url="%s"}`, target.URL)
	if got := metricValue(t, exposition, "http_exporter_scrape_http_status_code"+labels); got != http.StatusOK {
		t.Fatalf("a static target scrape should be recorded on its own series:\n%s", exposition)
	}
	if got := metricValue(t, exposition, "http_exporter_scrapes_total"+labels); got != 1 {
		t.Fatalf("static target scrapes=%v, want 1", got)
	}
}

// A static target's request is fully described by the configuration, so its
// series exist from the first scrape of /self-metrics rather than only after
// the target has been collected once.
func TestStaticTargetsAreVisibleBeforeTheirFirstCollection(t *testing.T) {
	cfg := &model.Config{
		Collectors: []model.Collector{testutil.Collector("text", "text")},
		OTLP:       otlpConfig("http://collector.invalid/v1/metrics"),
		Web:        model.WebConfig{SelfMetrics: model.SelfMetricsConfig{Verbose: true}},
	}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "one", Collector: "text", Target: "http://api.example:8080"}}}
	server := newStaticServer(t, cfg, file)

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
	collector := testutil.Collector("cached", "text")
	collector.Cache.TTL = model.Duration(time.Hour)
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
	server := verboseServer(t, true, testutil.Collector("capped", "text"))
	for i := 0; i <= VerboseRequestSeriesLimit; i++ {
		server.requests.statsFor(requestKey{Collector: "capped", URL: fmt.Sprintf("http://h/%d", i), Method: http.MethodGet})
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
	server := verboseServer(t, true, testutil.Collector("counted", "text"))
	if got := metricValue(t, selfMetrics(t, server), "http_exporter_request_series_tracked"); got != 0 {
		t.Fatalf("tracked=%v before any request, want 0", got)
	}
	server.requests.statsFor(requestKey{Collector: "counted", URL: "http://a.example", Method: http.MethodGet})
	server.requests.statsFor(requestKey{Collector: "counted", URL: "http://b.example", Method: http.MethodPost})
	server.requests.statsFor(requestKey{Collector: "counted", URL: "http://a.example", Method: http.MethodGet})
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
	quiet := &model.Config{Collectors: []model.Collector{testutil.Collector("toggled", "text")}}
	if err := config.Validate(quiet); err != nil {
		t.Fatal(err)
	}
	manager := config.NewManager(quiet, "", slog.Default())
	server := NewServer(manager, "python3", slog.Default())

	probe := "/probe?target=" + target.URL + "&collector=toggled"
	probeOnce(t, server, probe, nil)
	if strings.Contains(selfMetrics(t, server), "http_method=") {
		t.Fatal("verbose series appeared while verbose was off")
	}

	loud := &model.Config{Collectors: []model.Collector{testutil.Collector("toggled", "text")}, Web: model.WebConfig{SelfMetrics: model.SelfMetricsConfig{Verbose: true}}}
	if err := config.Validate(loud); err != nil {
		t.Fatal(err)
	}
	installConfig(server, loud)
	probeOnce(t, server, probe, nil)
	if !strings.Contains(selfMetrics(t, server), "http_method=") {
		t.Fatal("verbose series did not appear after the configuration enabled them")
	}

	installConfig(server, quiet)
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
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("default", "text")}}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Web.SelfMetrics.Verbose {
		t.Fatal("verbose self-metrics must be opt-in")
	}
}

// A probe whose target the collector's denied_targets refuses must not take
// one of the limited request slots: a caller probing refused targets would
// otherwise fill them, and a legitimate target probed afterwards would never be
// tracked.
func TestRefusedProbesDoNotTakeRequestSlots(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer target.Close()
	c := testutil.Collector("guarded", "text")
	c.Request.DeniedTargets = []string{"denied.example"}
	server := verboseServer(t, true, c)
	// All slots but one are taken, so a single refused probe taking one
	// would leave none for the legitimate target.
	for i := 0; i < VerboseRequestSeriesLimit-1; i++ {
		server.requests.statsFor(requestKey{Collector: "other", URL: fmt.Sprintf("http://h/%d", i), Method: http.MethodGet})
	}
	for i := 0; i < 5; i++ {
		response := probeOnce(t, server, fmt.Sprintf("/probe?collector=guarded&target=http://denied.example/%d", i), nil)
		if response.Code != http.StatusForbidden {
			t.Fatalf("status=%d, want the refusal's 403", response.Code)
		}
	}
	exposition := selfMetrics(t, server)
	if strings.Contains(exposition, "denied.example") {
		t.Fatalf("a refused probe's request is tracked:\n%s", exposition)
	}
	if got := metricValue(t, exposition, "http_exporter_request_series_capped"); got != 0 {
		t.Fatalf("capped=%v after refused probes only", got)
	}
	// The refusals are still counted on the collector.
	if got := metricValue(t, exposition, `http_exporter_targets_refused_total{collector="guarded"}`); got != 5 {
		t.Fatalf("collector refused=%v, want 5", got)
	}

	if response := probeOnce(t, server, "/probe?collector=guarded&target="+target.URL, nil); response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	exposition = selfMetrics(t, server)
	labels := fmt.Sprintf(`{collector="guarded",http_method="GET",url="%s"}`, target.URL)
	if got := metricValue(t, exposition, "http_exporter_scrapes_total"+labels); got != 1 {
		t.Fatalf("the legitimate target's scrapes=%v, want 1 on its own series", got)
	}
	if got := metricValue(t, exposition, "http_exporter_scrape_success_total"+labels); got != 1 {
		t.Fatalf("the legitimate target's successes=%v, want 1", got)
	}
}

// A probe of a request already tracked keeps updating it even when refused,
// as a reload that denies a target once allowed should show on its series.
func TestARefusedProbeOfATrackedRequestIsCounted(t *testing.T) {
	c := testutil.Collector("guarded", "text")
	c.Request.DeniedTargets = []string{"denied.example"}
	server := verboseServer(t, true, c)
	server.requests.statsFor(requestKey{Collector: "guarded", URL: "http://denied.example", Method: http.MethodGet})
	probeOnce(t, server, "/probe?collector=guarded&target=http://denied.example", nil)
	labels := `{collector="guarded",http_method="GET",url="http://denied.example"}`
	if got := metricValue(t, selfMetrics(t, server), "http_exporter_targets_refused_total"+labels); got != 1 {
		t.Fatalf("refused=%v on the tracked request, want 1", got)
	}
}

// Requests nobody asks for any more expire, so their slots come back and their
// frozen timestamps leave the endpoint; the configured static targets' do not.
func TestIdleRequestsExpire(t *testing.T) {
	tracker := newRequestTracker()
	now := time.Unix(1_700_000_000, 0)
	tracker.now = func() time.Time { return now }
	idle := requestKey{Collector: "c", URL: "http://idle", Method: "GET"}
	busy := requestKey{Collector: "c", URL: "http://busy", Method: "GET"}
	static := requestKey{Collector: "c", URL: "http://static", Method: "GET"}
	tracker.statsFor(idle)
	tracker.statsFor(busy)
	tracker.setStatic(map[requestKey]bool{static: true})

	now = now.Add(VerboseRequestIdleExpiry / 2)
	tracker.existing(busy)
	now = now.Add(VerboseRequestIdleExpiry/2 + time.Second)
	tracker.expire()
	samples, _ := tracker.Snapshot()
	var urls []string
	for _, sample := range samples {
		urls = append(urls, sample.Key.URL)
	}
	if got := strings.Join(urls, " "); got != "http://busy http://static" {
		t.Fatalf("tracked after expiry: %s, want the used and the static request", got)
	}
	if VerboseRequestIdleExpiry != time.Hour {
		t.Fatalf("VerboseRequestIdleExpiry=%v, want the documented hour", VerboseRequestIdleExpiry)
	}
}

// At the limit, an idle request makes room for a new one at once rather than
// at the next scrape of the self-metrics.
func TestAnIdleRequestMakesRoomAtTheLimit(t *testing.T) {
	tracker := newRequestTracker()
	now := time.Unix(1_700_000_000, 0)
	tracker.now = func() time.Time { return now }
	for i := 0; i < VerboseRequestSeriesLimit; i++ {
		tracker.statsFor(requestKey{Collector: "c", URL: fmt.Sprintf("http://h/%d", i), Method: "GET"})
	}
	if tracker.statsFor(requestKey{Collector: "c", URL: "http://new", Method: "GET"}) != nil {
		t.Fatal("a new request was tracked past the limit")
	}
	now = now.Add(VerboseRequestIdleExpiry + time.Second)
	if tracker.statsFor(requestKey{Collector: "c", URL: "http://new", Method: "GET"}) == nil {
		t.Fatal("idle requests did not make room for a new one")
	}
	if samples, capped := tracker.Snapshot(); len(samples) != 1 || capped {
		t.Fatalf("samples=%d capped=%v, want only the new request and the limit clear", len(samples), capped)
	}
}

// A reload that points a static target elsewhere drops the old URL's series,
// which would otherwise stay with a timestamp that never moves again.
func TestAReloadDropsTheRequestsOfRemovedStaticTargets(t *testing.T) {
	cfg := &model.Config{
		Collectors: []model.Collector{testutil.Collector("text", "text")},
		OTLP:       otlpConfig("http://collector.invalid/v1/metrics"),
		Web:        model.WebConfig{SelfMetrics: model.SelfMetricsConfig{Verbose: true}},
	}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "one", Collector: "text", Target: "http://old.example"}}}
	server := newStaticServer(t, cfg, file)
	if !strings.Contains(selfMetrics(t, server), `url="http://old.example"`) {
		t.Fatal("the static target's request is not listed")
	}
	server.manager.SetTargets("", &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "one", Collector: "text", Target: "http://new.example"}}})
	exposition := selfMetrics(t, server)
	if strings.Contains(exposition, "old.example") || !strings.Contains(exposition, `url="http://new.example"`) {
		t.Fatalf("after the reload, want only the new URL listed:\n%s", exposition)
	}
	if got := metricValue(t, exposition, "http_exporter_request_series_tracked"); got != 1 {
		t.Fatalf("tracked=%v, want 1", got)
	}
}

// Two probes of a request not tracked yet that finish at once both count.
func TestAbsorbAddsEveryCounter(t *testing.T) {
	var a, b statsValues
	for _, v := range []*statsValues{&a, &b} {
		rv := reflect.ValueOf(v).Elem()
		for i := 0; i < rv.NumField(); i++ {
			if f := rv.Field(i); f.Kind() == reflect.Uint64 {
				// The fields are unexported; the test sets them all
				// so a counter added later cannot be left out.
				reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().SetUint(1)
			}
		}
	}
	b.lastScrape = time.Unix(1, 0)
	b.lastStatus = http.StatusTeapot
	a.absorb(b)
	rv := reflect.ValueOf(a)
	for i := 0; i < rv.NumField(); i++ {
		if rv.Field(i).Kind() == reflect.Uint64 && rv.Field(i).Uint() != 2 {
			t.Errorf("%s=%d after absorb, want 2", rv.Type().Field(i).Name, rv.Field(i).Uint())
		}
	}
	if a.lastStatus != http.StatusTeapot || !a.lastScrape.Equal(b.lastScrape) {
		t.Fatalf("the later trip's values were not taken: %+v", a)
	}
}
