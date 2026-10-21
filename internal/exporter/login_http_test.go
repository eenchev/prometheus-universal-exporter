//go:build !select_request_types || request_type_http

package exporter

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// loginTarget answers a JSON login at /login with the token login-token,
// or 403 while refusing is set, and value=1 at /data to a request with
// that token.
func loginTarget(t *testing.T, refusing *atomic.Bool) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			if refusing.Load() {
				http.Error(w, `{"error": "account locked"}`, http.StatusForbidden)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "login-token-value", "expires_in": 3600})
			return
		}
		if r.Header.Get("Authorization") != "Bearer login-token-value" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "value=1")
	}))
	t.Cleanup(server.Close)
	return server
}

// loginCollector is a regex collector logging in at /login of its target
// with the password in the file password.
func loginCollector(password string) model.Collector {
	c := testutil.Collector("login_api", "text")
	c.Request.Login = &model.LoginConfig{Type: "json", URL: "/login", Token: ".access_token", ExpiresIn: ".expires_in", FieldFiles: map[string]string{"password": password}}
	return c
}

// A probe of a collector that logs in is answered with what the target
// sends to its token, and the logins it made are counted; a login that
// fails fails the probe in the login stage, logged as such, and is counted
// as failed. The secret the login sends is never in the answer or the log.
func TestAProbeLogsInAndCountsItsLogins(t *testing.T) {
	fetch.CloseConnections()
	t.Cleanup(fetch.CloseConnections)
	logs := testutil.CaptureLogs(t)
	var refusing atomic.Bool
	target := loginTarget(t, &refusing)
	server := modeServer(t, loginCollector(testutil.WriteFile(t, "password", "the-login-password")))
	probeURL := "/probe?collector=login_api&target=" + url.QueryEscape(target.URL+"/data")
	for range 2 {
		if r := probeOnce(t, server, probeURL, nil); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "demo_value 1") {
			t.Fatalf("answered %d: %s", r.Code, r.Body)
		}
	}
	metrics := selfMetrics(t, server)
	for _, want := range []string{`http_exporter_logins_total{collector="login_api"} 1`, `http_exporter_login_failures_total{collector="login_api"} 0`} {
		if !strings.Contains(metrics, want+"\n") {
			t.Errorf("no %s in the self-metrics:\n%s", want, metrics)
		}
	}

	refusing.Store(true)
	refused := modeServer(t, loginCollector(testutil.WriteFile(t, "other", "the-login-password")))
	r := probeOnce(t, refused, "/probe?collector=login_api&target="+url.QueryEscape(target.URL+"/other"), nil)
	if r.Code == http.StatusOK || !strings.Contains(r.Body.String(), "HTTP status 403") || !strings.Contains(r.Body.String(), "login") {
		t.Fatalf("answered %d: %s; want the login's failure", r.Code, r.Body)
	}
	if !strings.Contains(logs.String(), `"stage":"login"`) {
		t.Errorf("the failure was not logged in the login stage:\n%s", logs)
	}
	if strings.Contains(logs.String()+r.Body.String(), "the-login-password") {
		t.Error("the login's secret is in the log or the answer")
	}
	if metrics := selfMetrics(t, refused); !strings.Contains(metrics, `http_exporter_login_failures_total{collector="login_api"} 1`+"\n") {
		t.Errorf("the failed login is not counted:\n%s", metrics)
	}
}

// A collector without request.login has the login families at 0, and a
// collector of a type that does not log in has none.
func TestTheLoginFamiliesAreThoseOfTheTypesThatLogIn(t *testing.T) {
	if only := requestTypeFamilies["http_exporter_logins_total"]; !slices.Equal(only, []string{"http", "graphite"}) {
		t.Errorf("http_exporter_logins_total is of %v, want http and graphite", only)
	}
	if only := requestTypeFamilies["http_exporter_login_failures_total"]; !slices.Equal(only, []string{"http", "graphite"}) {
		t.Errorf("http_exporter_login_failures_total is of %v, want http and graphite", only)
	}
	server := modeServer(t, testutil.Collector("plain", "text"))
	if metrics := selfMetrics(t, server); !strings.Contains(metrics, `http_exporter_logins_total{collector="plain"} 0`+"\n") {
		t.Errorf("no login series at 0 for an http collector without a login:\n%s", metrics)
	}
}

// A debug probe's report shows the login request, marked as one, before the
// request it was made for, and shows neither the secret the login sent nor
// the token it got.
func TestADebugProbeShowsTheLoginWithoutItsSecrets(t *testing.T) {
	fetch.CloseConnections()
	t.Cleanup(fetch.CloseConnections)
	testutil.CaptureLogs(t)
	var refusing atomic.Bool
	target := loginTarget(t, &refusing)
	server := modeServer(t, loginCollector(testutil.WriteFile(t, "password", "the-login-password")))
	server.SetDebug(DebugFull)
	r := debugProbeGet(t, server, "collector=login_api&debug=true&target="+url.QueryEscape(target.URL+"/data"))
	report := r.Body.String()
	assertContains(t, report,
		"1. POST "+target.URL+"/login (login) -> 200 OK",
		"2. GET "+target.URL+"/data -> 200 OK",
		"Authorization: <redacted>",
	)
	for _, secret := range []string{"the-login-password", "login-token-value"} {
		if strings.Contains(report, secret) {
			t.Errorf("the report shows %q:\n%s", secret, report)
		}
	}
}

// A static target's login is its own: targets that log in differently
// share no cached answer.
func TestAStaticTargetsLoginIsPartOfItsCacheKey(t *testing.T) {
	login := func(url string) *model.LoginConfig {
		return &model.LoginConfig{Type: "json", URL: url, Token: ".t", FieldFiles: map[string]string{"password": "/p"}}
	}
	a := targetOwnRequest(&model.StaticTarget{Request: model.TargetRequestConfig{Login: login("/a")}})
	b := targetOwnRequest(&model.StaticTarget{Request: model.TargetRequestConfig{Login: login("/b")}})
	if len(a) == 0 || slices.Equal(a, b) {
		t.Errorf("targets logging in at /a and /b have the cache keys %q and %q", a, b)
	}
	if own := targetOwnRequest(&model.StaticTarget{}); own != nil {
		t.Errorf("a target without a login has %q", own)
	}
}
