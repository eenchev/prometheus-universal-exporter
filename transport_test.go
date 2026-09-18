package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// redirectingTarget serves a redirect at / and the real payload at /final, and
// counts how often the payload was actually reached.
func redirectingTarget() (*httptest.Server, *atomic.Int64) {
	var reached atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/final" {
			reached.Add(1)
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("value=42\n"))
			return
		}
		http.Redirect(w, r, "/final", http.StatusFound)
	}))
	return target, &reached
}

func TestRedirectsAreNotFollowedByDefault(t *testing.T) {
	target, reached := redirectingTarget()
	defer target.Close()
	cfg := &Config{Collectors: []Collector{testCollector("redirects", "text")}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Collectors[0].Request.FollowRedirects {
		t.Fatal("follow_redirects must default to false")
	}
	if cfg.Collectors[0].Request.EnableHTTP2 {
		t.Fatal("enable_http2 must default to false")
	}
	server := NewServer(NewConfigManager(cfg, "", slog.Default()), "python3", slog.Default())

	response := probeOnce(t, server, "/probe?target="+target.URL+"&collector=redirects", nil)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s, want the unfollowed redirect to fail the probe", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "302") {
		t.Fatalf("the probe should report the redirect status: %s", response.Body.String())
	}
	if got := reached.Load(); got != 0 {
		t.Fatalf("the redirect destination was reached %d times, want 0", got)
	}
}

func TestFollowRedirectsConfigurationAndOverride(t *testing.T) {
	tests := []struct {
		name       string
		collector  bool
		override   string
		wantStatus int
		wantFinal  int64
	}{
		{name: "disabled by default", wantStatus: http.StatusBadGateway},
		{name: "enabled on the collector", collector: true, wantStatus: http.StatusOK, wantFinal: 1},
		{name: "enabled by the probe parameter", override: "&follow_redirects=true", wantStatus: http.StatusOK, wantFinal: 1},
		{name: "disabled by the probe parameter", collector: true, override: "&follow_redirects=false", wantStatus: http.StatusBadGateway},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target, reached := redirectingTarget()
			defer target.Close()
			collector := testCollector("redirects", "text")
			collector.Request.FollowRedirects = test.collector
			cfg := &Config{Collectors: []Collector{collector}}
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			server := NewServer(NewConfigManager(cfg, "", slog.Default()), "python3", slog.Default())

			response := probeOnce(t, server, "/probe?target="+target.URL+"&collector=redirects"+test.override, nil)
			if response.Code != test.wantStatus {
				t.Fatalf("status=%d body=%s, want %d", response.Code, response.Body.String(), test.wantStatus)
			}
			if test.wantStatus == http.StatusOK && !strings.Contains(response.Body.String(), "demo_value 42") {
				t.Fatalf("body=%q", response.Body.String())
			}
			if got := reached.Load(); got != test.wantFinal {
				t.Fatalf("redirect destination reached %d times, want %d", got, test.wantFinal)
			}
		})
	}
}

// protocolTarget reports the HTTP version each request arrived on. It serves
// TLS because Go negotiates HTTP/2 through ALPN, not over cleartext.
func protocolTarget(t *testing.T) (*httptest.Server, *atomic.Value) {
	t.Helper()
	var protocol atomic.Value
	protocol.Store("")
	target := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protocol.Store(r.Proto)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	target.EnableHTTP2 = true
	target.StartTLS()
	return target, &protocol
}

func TestEnableHTTP2ConfigurationAndOverride(t *testing.T) {
	tests := []struct {
		name      string
		collector bool
		override  string
		want      string
	}{
		{name: "http 1.1 by default", want: "HTTP/1.1"},
		{name: "enabled on the collector", collector: true, want: "HTTP/2.0"},
		{name: "enabled by the probe parameter", override: "&enable_http2=true", want: "HTTP/2.0"},
		{name: "disabled by the probe parameter", collector: true, override: "&enable_http2=false", want: "HTTP/1.1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target, protocol := protocolTarget(t)
			defer target.Close()
			collector := testCollector("protocol", "text")
			collector.Request.EnableHTTP2 = test.collector
			collector.Request.TLS.InsecureSkipVerify = true // the test server is self-signed
			cfg := &Config{Collectors: []Collector{collector}}
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			server := NewServer(NewConfigManager(cfg, "", slog.Default()), "python3", slog.Default())

			response := probeOnce(t, server, "/probe?target="+target.URL+"&collector=protocol"+test.override, nil)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if got := protocol.Load().(string); got != test.want {
				t.Fatalf("target saw %s, want %s", got, test.want)
			}
		})
	}
}

func TestBooleanOverrideParsing(t *testing.T) {
	for _, name := range []string{"follow_redirects", "enable_http2", "insecure_skip_verify"} {
		t.Run(name, func(t *testing.T) {
			absent, err := parseRequestOverrides(url.Values{})
			if err != nil {
				t.Fatal(err)
			}
			if overrideFor(t, absent, name) != nil {
				t.Fatalf("an absent %s must leave the collector setting in force", name)
			}
			for _, raw := range []string{"true", "false"} {
				parsed, err := parseRequestOverrides(url.Values{name: {raw}})
				if err != nil {
					t.Fatal(err)
				}
				value := overrideFor(t, parsed, name)
				if value == nil || *value != (raw == "true") {
					t.Fatalf("%s=%s parsed as %v", name, raw, value)
				}
			}
			for _, raw := range []string{"yes", "1", "", "TRUE!"} {
				if _, err := parseRequestOverrides(url.Values{name: {raw}}); err == nil {
					t.Fatalf("%s=%q was accepted; want a client error", name, raw)
				}
			}
		})
	}
}

func overrideFor(t *testing.T, overrides RequestOverrides, name string) *bool {
	t.Helper()
	switch name {
	case "follow_redirects":
		return overrides.FollowRedirects
	case "enable_http2":
		return overrides.EnableHTTP2
	case "insecure_skip_verify":
		return overrides.InsecureSkipVerify
	}
	t.Fatalf("unknown override %q", name)
	return nil
}

func TestInvalidBooleanOverrideIsRejectedBeforeTheTarget(t *testing.T) {
	var requests atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer target.Close()
	cfg := &Config{Collectors: []Collector{testCollector("rejects", "text")}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	server := NewServer(NewConfigManager(cfg, "", slog.Default()), "python3", slog.Default())

	for _, query := range []string{"&follow_redirects=maybe", "&enable_http2=on"} {
		response := probeOnce(t, server, "/probe?target="+target.URL+"&collector=rejects"+query, nil)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d, want 400", query, response.Code)
		}
		if !strings.Contains(response.Body.String(), "want true or false") {
			t.Fatalf("%s: body=%q", query, response.Body.String())
		}
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("the target was contacted %d times despite an invalid override", got)
	}
}

func TestRedirectPolicyIsNoLongerAccepted(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	document := "collectors:\n  - name: legacy\n    request:\n      redirect_policy: none\n" +
		"    transform:\n      type: regex\n    metrics:\n      - name: demo_value\n        expression: 'value=(\\d+)'\n"
	if err := os.WriteFile(path, []byte(document), 0600); err != nil {
		t.Fatal(err)
	}
	// follow_redirects replaced it, and the strict decoder makes the removal
	// loud instead of silently changing how a collector behaves.
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "redirect_policy") {
		t.Fatalf("LoadConfig() error=%v, want the removed field to be rejected", err)
	}
}

func TestScheduledTargetCarriesTransportSettings(t *testing.T) {
	path := t.TempDir() + "/targets.yaml"
	document := "targets:\n  - name: legacy_eu\n    collector: text\n    target: http://legacy.example:8080\n" +
		"    request:\n      follow_redirects: true\n      enable_http2: true\n"
	if err := os.WriteFile(path, []byte(document), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := LoadTargetFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Validate(); err != nil {
		t.Fatal(err)
	}
	target := file.Targets[0]
	overrides := target.overrides()
	if overrides.FollowRedirects == nil || !*overrides.FollowRedirects {
		t.Fatalf("follow_redirects=%v", overrides.FollowRedirects)
	}
	if overrides.EnableHTTP2 == nil || !*overrides.EnableHTTP2 {
		t.Fatalf("enable_http2=%v", overrides.EnableHTTP2)
	}
	query := target.cacheQuery()
	if query.Get("follow_redirects") != "true" || query.Get("enable_http2") != "true" {
		t.Fatalf("the settings must reach the cache key: %v", query)
	}
}

func TestScheduledTargetFollowsRedirectsWhenConfigured(t *testing.T) {
	target, reached := redirectingTarget()
	defer target.Close()
	cfg := &Config{Collectors: []Collector{testCollector("text", "text")}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	follow := true
	file := &TargetFile{Targets: []ScheduledTarget{{
		Name: "followed", Collector: "text", Target: target.URL,
		Request: TargetRequestConfig{FollowRedirects: &follow},
	}}}
	server := newScheduledServer(t, cfg, file)

	server.scrapeScheduledTargets(context.Background(), 10*time.Second)
	resources := server.drainOTLP()
	if len(resources) != 1 {
		t.Fatalf("resources=%d", len(resources))
	}
	if value := metricByName(resources[0].Set, "demo_value"); value == nil || value.Value != 42 {
		t.Fatalf("the scheduled scrape did not follow the redirect: %+v", resources[0].Set.Metrics)
	}
	if got := reached.Load(); got != 1 {
		t.Fatalf("redirect destination reached %d times, want 1", got)
	}
}
