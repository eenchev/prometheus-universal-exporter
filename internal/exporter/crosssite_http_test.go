//go:build !select_request_types || request_type_http

package exporter

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// Requests a page of another site makes a browser send (crosssite.go).

// crossSiteRequests are a request to each endpoint behind the guard, the
// probe of target through collector first among them, as a browser would
// send it to an exporter with every endpoint on.
func crossSiteRequests(server *Server, target string) map[string]func() *http.Request {
	get := func(path string) func() *http.Request {
		return func() *http.Request { return httptest.NewRequest(http.MethodGet, path, nil) }
	}
	return map[string]func() *http.Request{
		"landing page":  get("/"),
		"collectors":    get("/collectors"),
		"probe":         get("/probe?collector=first&target=" + target),
		"self-metrics":  get(server.selfMetricsEndpoint()),
		"static target": get(server.staticTargetsEndpoint()),
		"debug page":    get("/debug?collector=first&target=" + target),
		"reload": func() *http.Request {
			return httptest.NewRequest(http.MethodPost, "/-/reload", strings.NewReader("a=b"))
		},
		"debug run": func() *http.Request {
			request := httptest.NewRequest(http.MethodPost, "/debug/run", bytes.NewReader([]byte(`{"configured":"first","target":"`+target+`"}`)))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Debug-Run", "run")
			return request
		},
	}
}

// Every endpoint but /health and /ready refuses with 403 a request the
// browser marks cross-site or same-site, with the exporter's Basic Auth
// credential attached or with Basic Auth off, before the target is
// contacted or the configuration reloaded; same-origin, none and no
// Sec-Fetch-Site at all pass.
func TestEveryEndpointRefusesRequestsAnotherSiteMakes(t *testing.T) {
	var contacted atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		contacted.Add(1)
		_, _ = w.Write([]byte("up 1\n"))
	}))
	defer target.Close()
	for _, auth := range []bool{true, false} {
		web := ""
		if auth {
			web = "web:\n  basic_auth:\n    enabled: true\n    username: exporter\n    password: secret\n"
		}
		conf := web + testutil.CollectorsDocument("first")
		r := newReloadable(t, conf, "")
		r.server.SetLifecycle(true)
		r.server.SetDebug(DebugFull)
		send := func(request *http.Request, site string) *httptest.ResponseRecorder {
			if auth {
				request.SetBasicAuth("exporter", "secret")
			}
			if site != "" {
				request.Header.Set("Sec-Fetch-Site", site)
			}
			recorder := httptest.NewRecorder()
			r.server.Handler().ServeHTTP(recorder, request)
			return recorder
		}
		r.write(r.path, web+testutil.CollectorsDocument("first", "second"))
		for name, request := range crossSiteRequests(r.server, target.URL) {
			for _, site := range []string{"cross-site", "same-site", "Cross-Site"} {
				recorder := send(request(), site)
				if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "another site") {
					t.Errorf("auth %v: %s with Sec-Fetch-Site %s answered %d: %s", auth, name, site, recorder.Code, recorder.Body)
				}
			}
		}
		if got := contacted.Load(); got != 0 {
			t.Errorf("auth %v: the target was contacted %d times by refused requests", auth, got)
		}
		// The configuration file gains a collector before the refused
		// reloads, which must leave the one in force as it was.
		if got := len(r.manager.Get().Collectors); got != 1 {
			t.Errorf("auth %v: %d collectors in force after refused reloads, want 1", auth, got)
		}
		for name, request := range crossSiteRequests(r.server, target.URL) {
			for _, site := range []string{"same-origin", "none", ""} {
				if recorder := send(request(), site); recorder.Code == http.StatusForbidden || recorder.Code == http.StatusUnauthorized {
					t.Errorf("auth %v: %s with Sec-Fetch-Site %q answered %d: %s", auth, name, site, recorder.Code, recorder.Body)
				}
			}
		}
		if contacted.Load() == 0 {
			t.Errorf("auth %v: no probe that passed contacted the target", auth)
		}
		for _, path := range []string{"/health", "/ready"} {
			for _, site := range []string{"cross-site", "same-site"} {
				request := httptest.NewRequest(http.MethodGet, path, nil)
				request.Header.Set("Sec-Fetch-Site", site)
				recorder := httptest.NewRecorder()
				r.server.Handler().ServeHTTP(recorder, request)
				if recorder.Code != http.StatusOK {
					t.Errorf("auth %v: %s with Sec-Fetch-Site %s answered %d", auth, path, site, recorder.Code)
				}
			}
		}
		contacted.Store(0)
	}
}
