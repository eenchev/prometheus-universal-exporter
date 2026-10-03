//go:build !select_request_types || request_type_http || request_type_localfile

package exporter

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

func regexCollector(name string) model.Collector {
	return model.Collector{
		Name:      name,
		Request:   model.RequestConfig{Type: fetch.RequestTypeHTTP},
		Transform: model.TransformConfig{Type: "regex"},
		Metrics:   []model.MetricRule{{Name: "v", Type: model.GaugeMetricType, Expression: `v=(\d+) (?P<who>\S+)`, Labels: []model.LabelRule{{Name: "who", Expression: "who"}}}},
	}
}

func landingServer(t *testing.T, cfg *model.Config) *Server {
	t.Helper()
	server := newStaticServer(t, cfg, nil)
	server.logger = testutil.QuietLogger(t)
	return server
}

func getPath(server *Server, method, path string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(method, path, nil))
	return recorder
}

// getPage fetches a page, which must be HTML that is not cached.
func getPage(t *testing.T, server *Server, path string) string {
	t.Helper()
	response := getPath(server, http.MethodGet, path)
	if response.Code != http.StatusOK {
		t.Fatalf("%s answered %d", path, response.Code)
	}
	if got := response.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("%s Content-Type=%q", path, got)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("%s Cache-Control=%q", path, got)
	}
	// No other site may frame the pages: the collectors page takes target
	// credentials.
	if got := response.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Fatalf("%s X-Frame-Options=%q", path, got)
	}
	if got := response.Header().Get("Content-Security-Policy"); !strings.Contains(got, "frame-ancestors 'none'") || !strings.Contains(got, "connect-src 'self'") {
		t.Fatalf("%s Content-Security-Policy=%q", path, got)
	}
	return response.Body.String()
}

func requireContains(t *testing.T, page string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q:\n%s", want, page)
		}
	}
}

func seriesValue(t *testing.T, exposition, series string) float64 {
	t.Helper()
	for _, line := range strings.Split(exposition, "\n") {
		if value, found := strings.CutPrefix(line, series+" "); found {
			v, err := strconv.ParseFloat(value, 64)
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
	}
	t.Fatalf("no series %s", series)
	return 0
}
