//go:build !select_request_types || request_type_http

package exporter

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

func TestProbeTextCollector(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer monitor-token" {
			t.Errorf("authorization=%q", got)
		}
		if got := r.Header.Get("X-Tenant"); got != "team-a" {
			t.Errorf("X-Tenant=%q", got)
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer target.Close()
	c := testutil.Collector("text", "text")
	c.Request.ForwardAuthorization = true
	c.Request.ForwardHeaders = []string{"X-Tenant"}
	cfg := &model.Config{Collectors: []model.Collector{c}}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	m := config.NewManager(cfg, "", slog.Default())
	s := NewServer(m, "python3", slog.Default())
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/probe?target="+target.URL+"&collector=text&header_X-Tenant=team-a", nil)
	req.Header.Set("Authorization", "Bearer monitor-token")
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "demo_value 42") {
		t.Fatalf("body=%s", rr.Body.String())
	}
}

func TestProbeRequestOverrides(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.URL.Path != "/api/status" || string(body) != "raw payload" {
			t.Errorf("request=%s %s %q", r.Method, r.URL.Path, string(body))
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer target.Close()
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("override", "text")}}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
	request := httptest.NewRequest(http.MethodGet, "/probe?target="+target.URL+"&collector=override&method=POST&path=%2Fapi%2Fstatus&timeout=2s&body=raw+payload", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestExporterBasicAuthProtection(t *testing.T) {
	cfg := &model.Config{
		Collectors: []model.Collector{testutil.Collector("text", "text")},
		Web:        model.WebConfig{BasicAuth: &model.ExporterBasicAuth{Enabled: true, Username: "exporter", Password: "secret"}},
	}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
	unauthorized := httptest.NewRecorder()
	server.Handler().ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/self-metrics", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}
	if got := unauthorized.Header().Get("WWW-Authenticate"); got == "" {
		t.Fatal("missing WWW-Authenticate challenge")
	}
	authorized := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/self-metrics", nil)
	request.SetBasicAuth("exporter", "secret")
	server.Handler().ServeHTTP(authorized, request)
	if authorized.Code != http.StatusOK {
		t.Fatalf("authorized status=%d body=%s", authorized.Code, authorized.Body.String())
	}
	health := httptest.NewRecorder()
	server.Handler().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status=%d", health.Code)
	}
}

func TestBearerTokenFileIsUsedIndependentlyOfExporterAuth(t *testing.T) {
	tokenFile := t.TempDir() + "/token"
	if err := os.WriteFile(tokenFile, []byte(" target-token \n"), 0600); err != nil {
		t.Fatal(err)
	}
	var received string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer target.Close()
	c := testutil.Collector("file_token", "text")
	c.Request.BearerTokenFile = tokenFile
	cfg := &model.Config{Collectors: []model.Collector{c}, Web: model.WebConfig{BasicAuth: &model.ExporterBasicAuth{Enabled: true, Username: "exporter", Password: "secret"}}}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
	request := httptest.NewRequest(http.MethodGet, "/probe?target="+target.URL+"&collector=file_token", nil)
	request.SetBasicAuth("exporter", "secret")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if received != "Bearer target-token" {
		t.Fatalf("target authorization=%q", received)
	}
}

func TestBasicAuthFileIsUsedForTarget(t *testing.T) {
	dir := t.TempDir()
	usernameFile := dir + "/username"
	passwordFile := dir + "/password"
	if err := os.WriteFile(usernameFile, []byte(" target-user \n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(passwordFile, []byte(" target-password \n"), 0600); err != nil {
		t.Fatal(err)
	}
	var receivedUser, receivedPassword string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedUser, receivedPassword, _ = r.BasicAuth()
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer target.Close()
	c := testutil.Collector("file_basic", "text")
	c.Request.BasicAuthFile = &model.BasicAuthFile{Username: usernameFile, Password: passwordFile}
	cfg := &model.Config{Collectors: []model.Collector{c}}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
	request := httptest.NewRequest(http.MethodGet, "/probe?target="+target.URL+"&collector=file_basic", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if receivedUser != "target-user" || receivedPassword != "target-password" {
		t.Fatalf("target basic auth=%q/%q", receivedUser, receivedPassword)
	}
}

// The exporter's own metrics are served at one path: /self-metrics unless
// --web.self-metrics-path names another, and nowhere else.
func TestSelfMetricsAreServedAtOnePath(t *testing.T) {
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("health", "text")}}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		set       string
		served    string
		notServed []string
	}{
		{set: "", served: "/self-metrics", notServed: []string{"/metrics"}},
		{set: "/metrics", served: "/metrics", notServed: []string{"/self-metrics"}},
		{set: "/internal/stats", served: "/internal/stats", notServed: []string{"/metrics", "/self-metrics"}},
	} {
		t.Run(tc.served, func(t *testing.T) {
			server := NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
			if tc.set != "" {
				server.SetSelfMetricsPath(tc.set)
			}
			handler := server.Handler()
			for path, want := range map[string]string{"/health": "ok\n", "/ready": "ready\n", tc.served: "http_exporter_collector_config_valid"} {
				rr := httptest.NewRecorder()
				handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
				if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), want) {
					t.Fatalf("%s: status=%d body=%q", path, rr.Code, rr.Body.String())
				}
			}
			for _, path := range tc.notServed {
				rr := httptest.NewRecorder()
				handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
				if rr.Code != http.StatusNotFound {
					t.Fatalf("%s answered %d, want 404: self-metrics are served at %s only", path, rr.Code, tc.served)
				}
			}
		})
	}
}
