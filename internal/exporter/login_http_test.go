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
	if only := familyTypes("http_exporter_logins_total"); !slices.Equal(only, builtOf("graphite", "http")) {
		t.Errorf("http_exporter_logins_total is of %v, want http and graphite", only)
	}
	if only := familyTypes("http_exporter_login_failures_total"); !slices.Equal(only, builtOf("graphite", "http")) {
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

// A login the identity provider refuses fails the probe with the status the
// provider answered and, for an OAuth 2.0 token endpoint, the error code
// when RFC 6749 registers it; what the provider said, which may name the
// user, the client or the directory it asked, is the operator's: it is in
// the log and the debug report, with what reads as a credential masked as
// in every text the exporter did not write, and not in the probe's answer.
func TestARefusedLoginAnswersTheCallerTheStatusAndLogsTheAnswer(t *testing.T) {
	const said = "user svc-metrics-7f3a is unknown to ldap://10.20.30.40:389"
	for _, tc := range []struct {
		name, contentType, body string
		login                   func() *model.LoginConfig
		answer, notAnswer       string
		logged                  string
	}{
		{
			name: "json", body: `{"message": "` + said + `", "password": "hunter2-echoed"}`,
			login: func() *model.LoginConfig {
				return &model.LoginConfig{Type: "json", URL: "/login", Token: ".access_token", FieldFiles: map[string]string{"password": testutil.WriteFile(t, "password", "the-login-password")}}
			},
			answer: "the login was answered with HTTP status 401; what it said is in the exporter's log",
			logged: said,
		},
		{
			name: "oauth2 registered code", contentType: "application/json", body: `{"error": "invalid_client", "error_description": "` + said + `"}`,
			login: func() *model.LoginConfig {
				return &model.LoginConfig{Type: "oauth2", URL: "/login", ClientID: "client-id-4b1d", ClientSecretFile: testutil.WriteFile(t, "secret", "the-client-secret")}
			},
			answer: "the token endpoint answered HTTP status 401, invalid_client; what it said is in the exporter's log",
			logged: "401, invalid_client: " + said,
		},
		{
			name: "oauth2 code of its own", contentType: "application/json", body: `{"error": "client-id-4b1d_locked_in_ldap", "error_description": "` + said + `"}`,
			login: func() *model.LoginConfig {
				return &model.LoginConfig{Type: "oauth2", URL: "/login", ClientID: "client-id-4b1d", ClientSecretFile: testutil.WriteFile(t, "secret", "the-client-secret")}
			},
			answer:    "the token endpoint answered HTTP status 401 with an error code RFC 6749 does not register; what it said is in the exporter's log",
			notAnswer: "locked_in_ldap",
			logged:    "401, client-id-4b1d_locked_in_ldap: " + said,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fetch.CloseConnections()
			t.Cleanup(fetch.CloseConnections)
			logs := testutil.CaptureLogs(t)
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/login" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if tc.contentType != "" {
					w.Header().Set("Content-Type", tc.contentType)
				}
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(target.Close)
			c := testutil.Collector("login_api", "text")
			c.Request.Login = tc.login()
			server := modeServer(t, c)
			server.SetDebug(DebugFull)
			r := probeOnce(t, server, "/probe?collector=login_api&target="+url.QueryEscape(target.URL+"/data"), nil)
			answer := r.Body.String()
			if r.Code == http.StatusOK || !strings.Contains(answer, tc.answer) {
				t.Errorf("answered %d: %s; want %q", r.Code, answer, tc.answer)
			}
			for _, kept := range []string{"svc-metrics", "10.20.30.40", "ldap", "client-id-4b1d", tc.notAnswer} {
				if kept != "" && strings.Contains(answer, kept) {
					t.Errorf("the probe's answer shows %q of the provider's answer: %s", kept, answer)
				}
			}
			if !strings.Contains(logs.String(), tc.logged) {
				t.Errorf("the log does not hold %q:\n%s", tc.logged, logs)
			}
			report := debugProbeGet(t, server, "collector=login_api&debug=true&target="+url.QueryEscape(target.URL+"/data")).Body.String()
			if !strings.Contains(report, tc.logged) {
				t.Errorf("the debug report does not hold %q:\n%s", tc.logged, report)
			}
			for _, secret := range []string{"hunter2-echoed", "the-login-password", "the-client-secret"} {
				if strings.Contains(logs.String()+answer+report, secret) {
					t.Errorf("%q is in the log, the answer or the report", secret)
				}
			}
		})
	}
}
