package exporter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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

func assertContains(t *testing.T, body string, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if !strings.Contains(body, fragment) {
			t.Errorf("no %q in the report:\n%s", fragment, body)
		}
	}
}

// A debug probe or debug scrape that panics — in its request type here, as it
// could in a decoder or a transform — gives back its concurrency slot and
// reports the panic: kept, the slot would be gone for good, and after
// max_concurrent_probes such probes every probe of the collector would be
// refused.
func TestAPanickingDebugProbeGivesBackItsSlot(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	fetch.RequestTypes["panics"] = &fetch.RequestType{
		Name:         "panics",
		Fields:       []string{"path"},
		TargetFields: []string{"path"},
		Validate:     func(*model.Collector) error { return nil },
		Fetch: func(context.Context, string, *model.Collector, fetch.RequestOverrides, http.Header) (*fetch.HTTPResponse, error) {
			panic("boom")
		},
	}
	t.Cleanup(func() { delete(fetch.RequestTypes, "panics") })
	broken := testutil.Collector("broken", "text")
	broken.Request = model.RequestConfig{Type: "panics", Path: "/data"}
	broken.MaxConcurrentProbes = 1
	cfg := &model.Config{Collectors: []model.Collector{broken}}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "broken", Collector: "broken", Target: "panics://data"}}}
	server := newStaticServer(t, cfg, file)
	server.SetProbeDebug(true)

	for range 2 {
		r := debugProbeGet(t, server, "collector=broken&debug=true&target=panics://data")
		if r.Code != http.StatusOK {
			t.Fatalf("answered %d: %s", r.Code, r.Body)
		}
		assertContains(t, r.Body.String(), "A probe would have answered 500: probe failed: internal error: boom", "probe panicked")
		if n := server.trips.count("broken"); n != 0 {
			t.Fatalf("%d slots still taken after a debug probe panicked", n)
		}
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/static-targets?debug=broken", nil))
	assertContains(t, recorder.Body.String(), "The scrape would have published target up 0: the scrape failed: internal error: boom")
	if n := server.trips.count("broken"); n != 0 {
		t.Fatalf("%d slots still taken after a debug scrape panicked", n)
	}
	if !strings.Contains(logs.String(), "debug probe panicked") || !strings.Contains(logs.String(), "stack") {
		t.Fatalf("the panic was not logged with its stack:\n%s", logs.String())
	}
}
