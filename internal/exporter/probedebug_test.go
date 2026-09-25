package exporter

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// Debug probes (probedebug.go).

func debugProbeGet(t *testing.T, server *Server, query string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/probe?"+query, nil))
	return recorder
}

// debugCollector reads {"up":1,"temperature":21.5} with jq, sending a
// bearer token, an API key header and a token in the query, and has
// demo_missing, which the target never provides, carry on under log.
func debugCollector(name string) model.Collector {
	c := modeCollector(name, model.ErrorModeLog)
	c.Metrics[1].Name = "demo_missing"
	c.Request.BearerToken = "s3cret-bearer"
	c.Request.Headers = map[string]string{"X-Api-Key": "s3cret-key", "Accept": "application/json"}
	c.Request.Query = map[string]string{"token": "s3cret-query", "view": "full"}
	return c
}

func assertContains(t *testing.T, body string, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if !strings.Contains(body, fragment) {
			t.Errorf("no %q in the report:\n%s", fragment, body)
		}
	}
}

// Without --web.enable-probe-debug a debug probe is refused 403, naming the
// flag, before the target is contacted; a debug value that is not a boolean
// is refused 400; debug=false is a normal probe.
func TestDebugProbesAreOffByDefault(t *testing.T) {
	testutil.CaptureLogs(t)
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"up":1}`))
	}))
	t.Cleanup(target.Close)
	server := modeServer(t, debugCollector("dbg"))
	query := "collector=dbg&target=" + url.QueryEscape(target.URL)
	if r := debugProbeGet(t, server, query+"&debug=true"); r.Code != http.StatusForbidden || !strings.Contains(r.Body.String(), "--web.enable-probe-debug") {
		t.Fatalf("answered %d: %s", r.Code, r.Body)
	}
	if hits.Load() != 0 {
		t.Fatal("a refused debug probe reached the target")
	}
	server.SetProbeDebug(true)
	if r := debugProbeGet(t, server, query+"&debug=maybe"); r.Code != http.StatusBadRequest {
		t.Fatalf("debug=maybe answered %d", r.Code)
	}
	if r := debugProbeGet(t, server, query+"&debug=false"); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "demo_up 1") {
		t.Fatalf("debug=false answered %d: %s", r.Code, r.Body)
	}
}

// A debug probe's report shows the request with its credentials redacted,
// the response, each stage, the series by metric, the rule that carried on
// with its first error, the logs and the metrics a probe would have served.
func TestADebugProbeReportsTheTrip(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	var authorization atomic.Value
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "session=s3cret-cookie")
		_, _ = w.Write([]byte(`{"up":1,"temperature":21.5}`))
	}))
	t.Cleanup(target.Close)
	server := modeServer(t, debugCollector("dbg"))
	server.SetProbeDebug(true)
	r := debugProbeGet(t, server, "collector=dbg&debug=true&target="+url.QueryEscape(target.URL))
	body := r.Body.String()
	if r.Code != http.StatusOK || !strings.HasPrefix(r.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("answered %d %s:\n%s", r.Code, r.Header().Get("Content-Type"), body)
	}
	if authorization.Load() != "Bearer s3cret-bearer" {
		t.Fatalf("the target got Authorization %v", authorization.Load())
	}
	if strings.Contains(body, "s3cret") {
		t.Fatalf("a credential is in the report:\n%s", body)
	}
	assertContains(t, body,
		`Debug probe of collector "dbg"`,
		"A probe would have answered 200 with 1 series",
		"1. GET "+target.URL+"?token=<redacted>&view=<redacted> -> 200 OK",
		"Authorization: <redacted>", "X-Api-Key: <redacted>", "Accept: application/json",
		"Rules that carried on without some series",
		"Status 200 OK", "Content-Type: application/json", "Set-Cookie: <redacted>",
		"Body: 27 bytes", `{"up":1,"temperature":21.5}`,
		"http         ok", "decode       ok", "json", "transform    ok", "validation   ok",
		"Series by metric", "demo_up: 1",
		"Rules that gave no series: demo_missing",
		"demo_missing: 1 failed, 1 of them missing values; first:",
		"metric extraction failed",
		"Metrics a probe would have served", "demo_up 1",
	)
	// The rule's failure went to the report, not the exporter's log.
	if strings.Contains(logs.String(), "metric extraction failed") {
		t.Errorf("the debug probe's rule failure was logged:\n%s", logs)
	}
}

// A debug probe leaves nothing behind: no cache entry, no self-metric, no
// OTLP point, no failure in the failure log; and it goes to the target
// even when a probe would have been answered from the cache.
func TestADebugProbeLeavesNothingBehind(t *testing.T) {
	testutil.CaptureLogs(t)
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"up":1,"temperature":21.5}`))
	}))
	t.Cleanup(target.Close)
	c := debugCollector("dbg")
	c.Cache = model.CacheConfig{TTL: model.Duration(time.Minute)}
	cfg := &model.Config{Collectors: []model.Collector{c}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	server := newStaticServer(t, cfg, nil)
	server.SetProbeDebug(true)
	query := "collector=dbg&target=" + url.QueryEscape(target.URL)

	debugProbeGet(t, server, query+"&debug=true")
	debugProbeGet(t, server, query+"&debug=true")
	if hits.Load() != 2 {
		t.Fatalf("two debug probes made %d trips", hits.Load())
	}
	stats := server.statsFor("dbg")
	stats.mu.Lock()
	probes, hitsCounted, misses := stats.probes, stats.cacheHits, stats.cacheMisses
	stats.mu.Unlock()
	if probes != 0 || hitsCounted != 0 || misses != 0 {
		t.Fatalf("debug probes were counted: probes %d, cache hits %d, misses %d", probes, hitsCounted, misses)
	}
	if resources := server.drainOTLP(); len(resources) != 0 {
		t.Fatalf("a debug probe was queued for OTLP: %+v", resources)
	}
	// The cache is empty: a normal probe goes to the target, and then a
	// debug probe goes again rather than reading what it cached.
	debugProbeGet(t, server, query)
	debugProbeGet(t, server, query+"&debug=true")
	if hits.Load() != 4 {
		t.Fatalf("%d trips, want 4: a debug probe filled or read the cache", hits.Load())
	}
}

// A failing trip's report says where it failed and what a probe would have
// answered, shows the target's body, and leaves the failure log alone.
func TestADebugProbeReportsAFailure(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "the database is down", http.StatusServiceUnavailable)
	}))
	t.Cleanup(target.Close)
	server := modeServer(t, debugCollector("dbg"))
	server.SetProbeDebug(true)
	body := debugProbeGet(t, server, "collector=dbg&debug=true&target="+url.QueryEscape(target.URL)).Body.String()
	assertContains(t, body,
		"A probe would have answered 502: collector dbg http_status failed: received HTTP status 503",
		"Status 503 Service Unavailable", "the database is down",
		"http_status  failed", `msg="probe failed"`,
		"Metrics a probe would have served\n  none",
	)
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "probe failed") {
			t.Fatalf("the debug probe's failure was logged: %s", line)
		}
	}
	// A normal probe's failure is still logged as the first of its kind.
	probe(t, server, url.QueryEscape(target.URL), "dbg")
	if !strings.Contains(logs.String(), "probe failed") {
		t.Fatalf("the probe after the debug probe was not logged as failing:\n%s", logs)
	}
}

// The target policy applies to a debug probe: a refused target is reported,
// never contacted.
func TestADebugProbeKeepsTheTargetPolicy(t *testing.T) {
	testutil.CaptureLogs(t)
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(target.Close)
	c := debugCollector("dbg")
	c.Request.DeniedTargets = []string{"127.0.0.0/8", "::1"}
	server := modeServer(t, c)
	server.SetProbeDebug(true)
	body := debugProbeGet(t, server, "collector=dbg&debug=true&target="+url.QueryEscape(target.URL)).Body.String()
	assertContains(t, body, "A probe would have answered 403: collector dbg refused the target", "target_policy refused")
	if hits.Load() != 0 {
		t.Fatal("a refused target was contacted")
	}
}

// Every request is listed, redirects included, and a stale result a probe
// would have served instead of the failure is said to be.
func TestADebugProbeListsRedirectsAndTheStaleAnswer(t *testing.T) {
	testutil.CaptureLogs(t)
	var fail atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/old", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/new", http.StatusFound) })
	mux.HandleFunc("/new", func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"up":1}`))
	})
	target := httptest.NewServer(mux)
	t.Cleanup(target.Close)
	c := debugCollector("dbg")
	c.Request.Query = nil
	c.Request.FollowRedirects = true
	c.Cache = model.CacheConfig{TTL: model.Duration(time.Millisecond), StaleIfError: model.Duration(time.Hour)}
	server := modeServer(t, c)
	server.SetProbeDebug(true)
	query := "collector=dbg&target=" + url.QueryEscape(target.URL+"/old")

	body := debugProbeGet(t, server, query+"&debug=true").Body.String()
	assertContains(t, body, "1. GET "+target.URL+"/old -> 302 Found", "2. GET "+target.URL+"/new (redirect) -> 200 OK")

	probe(t, server, url.QueryEscape(target.URL+"/old"), "dbg")
	time.Sleep(5 * time.Millisecond)
	fail.Store(true)
	body = debugProbeGet(t, server, query+"&debug=true").Body.String()
	assertContains(t, body, "A probe would have answered 200 with the last good result", "marked stale", "instead of 502")
}

func TestRedactURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://user:pass@host/p?a=1&b=2&a=3": "https://<redacted>@host/p?a=<redacted>&a=<redacted>&b=<redacted>",
		"https://host/p":                       "https://host/p",
		"grpc://host:443/pkg.Svc/Method":       "grpc://host:443/pkg.Svc/Method",
	} {
		if got := redactURL(raw); got != want {
			t.Errorf("redactURL(%q) = %q, want %q", raw, got, want)
		}
	}
	for name, want := range map[string]bool{"Authorization": true, "X-Api-Key": true, "Cookie": true, "X-Auth-Token": true, "Accept": false, "Content-Type": false} {
		if got := redactHeader(name, "v") == redacted; got != want {
			t.Errorf("header %s redacted: %v", name, got)
		}
	}
}

// The requests a trip sent are recorded only for a trip with a trace.
func TestRequestTraceIsOnlyKeptWhenAsked(t *testing.T) {
	target := textTarget(t, "value=1\n")
	c := testutil.Collector("text", "text")
	if _, err := fetch.FetchCollector(t.Context(), target.URL, &c, fetch.RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	ctx, trace := fetch.WithRequestTrace(t.Context())
	if _, err := fetch.FetchCollector(ctx, target.URL, &c, fetch.RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	if requests := trace.Requests(); len(requests) != 1 || requests[0].Outcome != "200 OK" {
		t.Fatalf("recorded %+v", requests)
	}
}
