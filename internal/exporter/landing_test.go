package exporter

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The HTML pages: the landing page at / (landing.go) and the collectors page
// at /collectors (collectorspage.go).

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

func weatherConfig() *model.Config {
	return &model.Config{Collectors: []model.Collector{testutil.Collector("weather", "text")}}
}

// The landing page names the build and links the endpoints and the
// collectors page; the probe forms are on that page, not this one.
func TestTheLandingPageLinksTheEndpointsAndTheCollectorsPage(t *testing.T) {
	server := landingServer(t, weatherConfig())
	server.SetSelfMetricsPath("/self-metrics")
	page := getPage(t, server, "/")
	requireContains(t, page,
		"<title>Prometheus Universal Exporter</title>",
		"Version "+BuildVersion().Version,
		"1 collector loaded.",
		`href="/collectors"`, `href="/self-metrics"`, `href="/health"`, `href="/ready"`,
	)
	if strings.Contains(page, "<form") {
		t.Error("the landing page carries a probe form; they belong on /collectors")
	}
	if strings.Contains(page, `href="/metrics"`) {
		t.Error("the page links /metrics, which serves nothing unless it is the self-metrics path")
	}
	if strings.Contains(page, "/-/reload") {
		t.Error("the page offers /-/reload without --web.enable-lifecycle")
	}
}

// Only / is the landing page: another unknown path is still not found, and a
// method other than GET or HEAD is refused.
func TestTheLandingPageIsOnlyAtTheRoot(t *testing.T) {
	server := landingServer(t, weatherConfig())
	if code := getPath(server, http.MethodGet, "/nothing-here").Code; code != http.StatusNotFound {
		t.Errorf("an unknown path answered %d, want 404", code)
	}
	for _, path := range []string{"/", "/collectors"} {
		if code := getPath(server, http.MethodHead, path).Code; code != http.StatusOK {
			t.Errorf("HEAD %s answered %d, want 200", path, code)
		}
		if code := getPath(server, http.MethodPost, path).Code; code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s answered %d, want 405", path, code)
		}
	}
}

// With the exporter's Basic Auth on, both pages are protected like /probe:
// they list the collectors.
func TestThePagesAreProtectedByBasicAuth(t *testing.T) {
	cfg := weatherConfig()
	cfg.Web = model.WebConfig{BasicAuth: &model.ExporterBasicAuth{Enabled: true, Username: "u", Password: "p"}}
	server := landingServer(t, cfg)
	for _, path := range []string{"/", "/collectors"} {
		if code := getPath(server, http.MethodGet, path).Code; code != http.StatusUnauthorized {
			t.Fatalf("an unauthenticated %s answered %d, want 401", path, code)
		}
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.SetBasicAuth("u", "p")
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("an authenticated %s answered %d", path, recorder.Code)
		}
	}
}

// Each collector has a form probing through it, with the target it needs.
func TestTheCollectorsPageHasAProbeFormPerCollector(t *testing.T) {
	server := landingServer(t, weatherConfig())
	page := getPage(t, server, "/collectors")
	requireContains(t, page,
		`href="/"`,
		`<form class="probe" action="/probe" method="get" autocomplete="off">`,
		`<input type="hidden" name="collector" value="weather">`,
		`name="target" placeholder="http://host:port" required>`,
	)
	for _, absent := range []string{"Request parameters", "Forwarded headers", "Target credential", "The exporter sends the target"} {
		if strings.Contains(page, absent) {
			t.Errorf("a collector taking nothing but a target shows %q", absent)
		}
	}
}

// A collector's request parameters are fields named as the probe takes
// them, required unless the placeholder has a default, which is shown.
func TestTheCollectorsPageAsksForRequestParameters(t *testing.T) {
	c := testutil.Collector("tenants", "text")
	c.Request.Path = "/api/{{param_tenant}}/v{{param_version:2}}/status"
	c.Request.Query = map[string]string{"region": "{{param_region:eu}}"}
	server := landingServer(t, &model.Config{Collectors: []model.Collector{c}})
	page := getPage(t, server, "/collectors")
	requireContains(t, page,
		"Request parameters",
		`name="param_tenant" required>`,
		`name="param_version" placeholder="default: 2">`,
		`name="param_region" placeholder="default: eu">`,
	)
}

// Forwarded headers are fields sent as header_<name> probe parameters, the
// ones never forwarded left out; a forwarded Authorization is a credential
// the script sends as a header, so its fields have no name and never reach
// the URL.
func TestTheCollectorsPageAsksForWhatTheTargetNeeds(t *testing.T) {
	c := testutil.Collector("tenant_status", "text")
	c.Request.ForwardAuthorization = true
	c.Request.ForwardHeaders = []string{"x-tenant", "Host", "Authorization", "X-Tenant"}
	server := landingServer(t, &model.Config{Collectors: []model.Collector{c}})
	page := getPage(t, server, "/collectors")
	requireContains(t, page,
		"Forwarded headers", `name="header_X-Tenant"`,
		"Target credential", `data-auth="scheme"`, `data-auth="token"`, `data-auth="username"`, `data-auth="password"`,
		`headers.Authorization = "Bearer "`,
	)
	if strings.Count(page, `name="header_`) != 1 {
		t.Errorf("want one forwarded header field, X-Tenant, once:\n%s", page)
	}
	fields := regexp.MustCompile(`<(?:input|select)[^>]*data-auth="[a-z]+"[^>]*>`).FindAllString(page, -1)
	if len(fields) != 4 {
		t.Fatalf("want the scheme, token, username and password fields, got %q", fields)
	}
	for _, field := range fields {
		if strings.Contains(field, "name=") {
			t.Errorf("a credential field has a name, so a plain form submission would put it in the URL: %s", field)
		}
	}
}

// A credential the configuration holds is the exporter's to send: the page
// says it is there, never what it is.
func TestTheCollectorsPageNeverShowsAConfiguredCredential(t *testing.T) {
	basic := testutil.Collector("basic", "text")
	basic.Request.BasicAuth = &model.BasicAuth{Username: "svc-user", Password: "s3cret-password"}
	bearer := testutil.Collector("bearer", "text")
	bearer.Request.BearerToken = "s3cret-token"
	server := landingServer(t, &model.Config{Collectors: []model.Collector{basic, bearer}})
	page := getPage(t, server, "/collectors")
	requireContains(t, page,
		"The exporter sends the target basic auth from the configuration; nothing to enter here.",
		"The exporter sends the target a bearer token from the configuration; nothing to enter here.")
	for _, secret := range []string{"svc-user", "s3cret-password", "s3cret-token"} {
		if strings.Contains(page, secret) {
			t.Errorf("the page shows %q", secret)
		}
	}
	if strings.Contains(page, "Target credential") {
		t.Error("a collector that does not forward Authorization asks for a credential")
	}
}

// Each form starts with a timeout the page's script sends as Prometheus's
// scrape timeout header, so a probe from the page cannot hang; the field has
// no name, since it is a header, not a probe parameter.
func TestTheCollectorsPageSendsATimeout(t *testing.T) {
	page := getPage(t, landingServer(t, weatherConfig()), "/collectors")
	requireContains(t, page,
		`type="number" data-timeout min="1" max="600" step="any" value="10" required>`,
		`"X-Prometheus-Scrape-Timeout-Seconds": String(timeout)`,
		`AbortSignal.timeout(`,
	)
	field := regexp.MustCompile(`<input[^>]*data-timeout[^>]*>`).FindString(page)
	if field == "" || strings.Contains(field, "name=") {
		t.Fatalf("the timeout field is missing or named: %q", field)
	}
}

// Basic auth with both fields blank sends no credential, as a blank bearer
// token does.
func TestABlankTargetCredentialIsNotSent(t *testing.T) {
	c := testutil.Collector("tenant_status", "text")
	c.Request.ForwardAuthorization = true
	page := getPage(t, landingServer(t, &model.Config{Collectors: []model.Collector{c}}), "/collectors")
	requireContains(t, page,
		`scheme.value === "bearer" && field("token").value !== ""`,
		`scheme.value === "basic" && (field("username").value !== "" || field("password").value !== "")`,
	)
}

// A localfile target names a file or a directory under the collector's root.
func TestTheCollectorsPageHintsALocalfileTarget(t *testing.T) {
	c := testutil.Collector("textfile", "text")
	c.Request = model.RequestConfig{Type: "localfile", Root: t.TempDir(), Path: "batch.prom"}
	page := getPage(t, landingServer(t, &model.Config{Collectors: []model.Collector{c}}), "/collectors")
	requireContains(t, page, `placeholder="a file or directory under its root (optional)"`)
}
