package exporter

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The landing page at / (landing.go).

func landingServer(t *testing.T, cfg *model.Config) *Server {
	t.Helper()
	server := newScheduledServer(t, cfg, nil)
	server.logger = testutil.QuietLogger(t)
	return server
}

func getPath(server *Server, method, path string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(method, path, nil))
	return recorder
}

// The page names the build, links the endpoints, and lists every collector
// with a form that probes through it.
func TestTheLandingPageListsTheEndpointsAndCollectors(t *testing.T) {
	c := testutil.Collector("weather", "text")
	server := landingServer(t, &model.Config{Collectors: []model.Collector{c}})
	server.SetSelfMetricsPath("/self-metrics")

	response := getPath(server, http.MethodGet, "/")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	if got := response.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type=%q", got)
	}
	page := response.Body.String()
	for _, want := range []string{
		"<title>Prometheus Universal Exporter</title>",
		"Version " + BuildVersion().Version,
		`href="/metrics"`, `href="/self-metrics"`, `href="/health"`, `href="/ready"`,
		`<form action="/probe" method="get"><input type="hidden" name="collector" value="weather">`,
		`name="target"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, "/-/reload") {
		t.Error("the page offers /-/reload without --web.enable-lifecycle")
	}
}

// Only / is the page: another unknown path is still not found, and a method
// other than GET or HEAD is refused.
func TestTheLandingPageIsOnlyAtTheRoot(t *testing.T) {
	server := landingServer(t, &model.Config{Collectors: []model.Collector{testutil.Collector("weather", "text")}})
	if code := getPath(server, http.MethodGet, "/nothing-here").Code; code != http.StatusNotFound {
		t.Errorf("an unknown path answered %d, want 404", code)
	}
	if code := getPath(server, http.MethodHead, "/").Code; code != http.StatusOK {
		t.Errorf("HEAD / answered %d, want 200", code)
	}
	if code := getPath(server, http.MethodPost, "/").Code; code != http.StatusMethodNotAllowed {
		t.Errorf("POST / answered %d, want 405", code)
	}
}

// With the exporter's Basic Auth on, the page is protected like /probe: it
// lists the collectors.
func TestTheLandingPageIsProtectedByBasicAuth(t *testing.T) {
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("weather", "text")}, Web: model.WebConfig{BasicAuth: &model.ExporterBasicAuth{Enabled: true, Username: "u", Password: "p"}}}
	server := landingServer(t, cfg)
	if code := getPath(server, http.MethodGet, "/").Code; code != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated / answered %d, want 401", code)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.SetBasicAuth("u", "p")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("an authenticated / answered %d", recorder.Code)
	}
}
