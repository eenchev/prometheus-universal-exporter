package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A request.path may carry {{param_<name>}} placeholders, bound on every probe
// from the param_<name> query parameter, falling back to an optional default
// written after a colon, and failing the probe with 400 when neither exists.

// pathRecorder is a target that remembers the exact request line it received,
// escaped as sent, so a test can see whether a value became one segment or
// several.
type pathRecorder struct {
	mu    sync.Mutex
	paths []string
	hits  atomic.Int64
}

func (p *pathRecorder) serve(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.hits.Add(1)
		p.mu.Lock()
		p.paths = append(p.paths, r.RequestURI)
		p.mu.Unlock()
		_, _ = w.Write([]byte("value=7\n"))
	}))
	t.Cleanup(server.Close)
	return server
}

func (p *pathRecorder) last(t *testing.T) string {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.paths) == 0 {
		t.Fatal("the target was never contacted")
	}
	return p.paths[len(p.paths)-1]
}

const tenantPath = "/api/{{param_tenant}}/v{{param_version:2}}/status"

func pathCollector(path string) Collector {
	c := testCollector("tenants", "text")
	c.Request.Path = path
	return c
}

func pathServer(t *testing.T, collectors ...Collector) *Server {
	t.Helper()
	cfg := &Config{Collectors: collectors}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return NewServer(NewConfigManager(cfg, "", slog.Default()), "python3", slog.Default())
}

func probeWith(t *testing.T, server *Server, target string, params string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/probe?target="+url.QueryEscape(target)+"&collector=tenants"+params, nil)
	server.Handler().ServeHTTP(recorder, request)
	return recorder
}

func TestPathParametersAreBoundFromTheProbe(t *testing.T) {
	tests := []struct {
		name   string
		params string
		want   string
	}{
		{"supplied, with the other defaulted", "&param_tenant=acme", "/api/acme/v2/status"},
		{"both supplied", "&param_tenant=acme&param_version=3", "/api/acme/v3/status"},
		{"an empty value takes the default", "&param_tenant=acme&param_version=", "/api/acme/v2/status"},
		// A value is one segment. A slash in it is escaped rather than
		// becoming a new path segment, and so are the characters that would
		// otherwise start a query or a fragment.
		{"a slash stays inside the segment", "&param_tenant=" + url.QueryEscape("a/b"), "/api/a%2Fb/v2/status"},
		{"query and fragment characters are escaped", "&param_tenant=" + url.QueryEscape("a?b#c"), "/api/a%3Fb%23c/v2/status"},
		{"spaces and non-ASCII are escaped", "&param_tenant=" + url.QueryEscape("é x"), "/api/%C3%A9%20x/v2/status"},
		{"a dot inside a value is harmless", "&param_tenant=a.b", "/api/a.b/v2/status"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var recorder pathRecorder
			target := recorder.serve(t)
			server := pathServer(t, pathCollector(tenantPath))

			response := probeWith(t, server, target.URL, test.params)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if got := recorder.last(t); got != test.want {
				t.Fatalf("target received %q, want %q", got, test.want)
			}
		})
	}
}

// Each of these is the caller's mistake, so it is a 400 that names the
// parameter, and the target is never contacted.
func TestPathParameterMistakesAreRejectedBeforeTheTarget(t *testing.T) {
	tests := []struct {
		name   string
		params string
		want   string
	}{
		{"missing, with no default", "", "param_tenant"},
		{"empty, with no default", "&param_tenant=", "param_tenant"},
		{"given twice", "&param_tenant=a&param_tenant=b", "given 2 times"},
		{"dot-dot", "&param_tenant=..", `must not be ".."`},
		{"dot", "&param_tenant=.", `must not be "."`},
		// A misspelling would otherwise quietly fall back to a default and
		// report one tenant's numbers as another's.
		{"not used by the path", "&param_tenant=acme&param_tenat=acme", "param_tenat"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var recorder pathRecorder
			target := recorder.serve(t)
			server := pathServer(t, pathCollector(tenantPath))

			response := probeWith(t, server, target.URL, test.params)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400; body=%s", response.Code, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), test.want) {
				t.Fatalf("body %q should mention %q", response.Body.String(), test.want)
			}
			if recorder.hits.Load() != 0 {
				t.Fatal("the target was contacted for a probe that should have been rejected")
			}
		})
	}
}

// The path probe parameter replaces request.path wholesale. It is already the
// scrape's own choice, so its text is used as given and path parameters have
// nothing to bind into.
func TestThePathOverrideIsNotTemplated(t *testing.T) {
	var recorder pathRecorder
	target := recorder.serve(t)
	server := pathServer(t, pathCollector(tenantPath))

	response := probeWith(t, server, target.URL, "&path=/fixed")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if got := recorder.last(t); got != "/fixed" {
		t.Fatalf("target received %q, want /fixed", got)
	}

	rejected := probeWith(t, server, target.URL, "&path=/fixed&param_tenant=acme")
	if rejected.Code != http.StatusBadRequest || !strings.Contains(rejected.Body.String(), "replaces request.path") {
		t.Fatalf("a path parameter beside a path override should be rejected as unused: %d %s", rejected.Code, rejected.Body.String())
	}
}

// A collector without placeholders is untouched: no parameter is required, and
// a param_ parameter it cannot use is still a mistake worth reporting.
func TestAPlainPathIsUnaffected(t *testing.T) {
	var recorder pathRecorder
	target := recorder.serve(t)
	server := pathServer(t, pathCollector("/status"))

	if response := probeWith(t, server, target.URL, ""); response.Code != http.StatusOK || recorder.last(t) != "/status" {
		t.Fatalf("status=%d path=%q", response.Code, recorder.last(t))
	}
	if response := probeWith(t, server, target.URL, "&param_tenant=acme"); response.Code != http.StatusBadRequest {
		t.Fatalf("an unused path parameter should be rejected, got %d", response.Code)
	}
}

func TestMalformedPlaceholdersAreRejectedAtStartup(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/api/{{param_tenant", "unclosed placeholder"},
		{"/api/{{tenant}}", "not a path parameter"},
		{"/api/{{param_}}", "not a path parameter"},
		{"/api/{{param_a-b}}", "not a path parameter"},
		{"/api/{{ param_tenant }}", "not a path parameter"},
		{"/api/{{param_tenant:${DEFAULT_TENANT}}}", "--config.export-env"},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			cfg := &Config{Collectors: []Collector{pathCollector(test.path)}}
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("%q should be rejected", test.path)
			}
			if !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), `"tenants"`) {
				t.Fatalf("error %q should mention %q and name the collector", err, test.want)
			}
		})
	}
}

func TestWellFormedPlaceholdersAreAccepted(t *testing.T) {
	for _, path := range []string{
		"/api/{{param_tenant}}",
		"/api/{{param_tenant:acme}}",
		"/api/{{param_suffix:}}",
		"/{{param_a}}/{{param_b:x}}/{{param_a}}",
		"/api/{{param_Region_2}}",
		"/metrics}}", // a stray closing pair is ordinary text
	} {
		cfg := &Config{Collectors: []Collector{pathCollector(path)}}
		if err := cfg.Validate(); err != nil {
			t.Errorf("%q: %v", path, err)
		}
	}
}

// An explicit empty default is a default: the placeholder may be left out and
// binds nothing.
func TestAnEmptyDefaultBindsNothing(t *testing.T) {
	var recorder pathRecorder
	target := recorder.serve(t)
	server := pathServer(t, pathCollector("/items{{param_suffix:}}"))

	if response := probeWith(t, server, target.URL, ""); response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if got := recorder.last(t); got != "/items" {
		t.Fatalf("target received %q, want /items", got)
	}
}

// The environment and the probe fill different things at different times: the
// environment once, as the file is read, and the probe on every scrape. The two
// compose, so a default can come from the environment.
func TestEnvironmentReferencesAndPathParametersCompose(t *testing.T) {
	t.Setenv("DEFAULT_TENANT", "fromenv")
	body := "collectors:\n  - name: tenants\n    request:\n      type: http\n      path: /api/{{param_tenant:${DEFAULT_TENANT}}}/status\n" +
		"    transform:\n      type: regex\n    metrics:\n      - name: demo_value\n        expression: 'value=(\\d+)'\n"
	path := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path, WithEnvExpansion())
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Collectors[0].Request.Path; got != "/api/{{param_tenant:fromenv}}/status" {
		t.Fatalf("path=%q: the environment reference should be expanded and the placeholder kept", got)
	}
	var recorder pathRecorder
	target := recorder.serve(t)
	server := NewServer(NewConfigManager(cfg, "", slog.Default()), "python3", slog.Default())

	if response := probeWith(t, server, target.URL, ""); response.Code != http.StatusOK || recorder.last(t) != "/api/fromenv/status" {
		t.Fatalf("status=%d path=%q, want the environment default", response.Code, recorder.last(t))
	}
	if response := probeWith(t, server, target.URL, "&param_tenant=acme"); response.Code != http.StatusOK || recorder.last(t) != "/api/acme/status" {
		t.Fatalf("status=%d path=%q, want the probe value", response.Code, recorder.last(t))
	}

	// Without expansion the reference is not a default, and the load says so
	// rather than binding "${DEFAULT_TENANT" and leaving a brace behind.
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "--config.export-env") {
		t.Fatalf("err=%v, want a pointer at --config.export-env", err)
	}
}

// The cache key covers every probe parameter, so two tenants never share an
// entry and one tenant's repeat is served from memory.
func TestPathParametersAreDistinctInTheCache(t *testing.T) {
	var recorder pathRecorder
	target := recorder.serve(t)
	c := pathCollector(tenantPath)
	c.Cache = Duration(time.Minute)
	c.Limits.MaxCacheEntries = 10
	server := pathServer(t, c)

	for _, params := range []string{"&param_tenant=acme", "&param_tenant=globex", "&param_tenant=acme"} {
		if response := probeWith(t, server, target.URL, params); response.Code != http.StatusOK {
			t.Fatalf("%s: status=%d", params, response.Code)
		}
	}
	if got := recorder.hits.Load(); got != 2 {
		t.Fatalf("target contacted %d times, want 2 (one per tenant)", got)
	}
}

// The verbose self-metrics label a request by its URL. A path parameter's value
// is a tenant or an account — exactly what labels already leave out of the
// query string — and one series per value would be unbounded, so the label
// keeps the placeholder.
func TestTheRequestLabelKeepsThePlaceholder(t *testing.T) {
	var recorder pathRecorder
	target := recorder.serve(t)
	cfg := &Config{
		Collectors: []Collector{pathCollector(tenantPath)},
		Web:        WebConfig{SelfMetrics: SelfMetricsConfig{Verbose: true}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	server := NewServer(NewConfigManager(cfg, "", slog.Default()), "python3", slog.Default())
	if response := probeWith(t, server, target.URL, "&param_tenant=acme"); response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}

	exposition := selfMetrics(t, server)
	want := `url="` + target.URL + tenantPath + `"`
	if !strings.Contains(exposition, want) {
		t.Fatalf("expected a series labelled %s in:\n%s", want, exposition)
	}
	if strings.Contains(exposition, "acme") {
		t.Fatalf("the tenant reached a label:\n%s", exposition)
	}
}

// A scheduled target has no probe, so it cannot supply a path parameter.
func TestScheduledTargetsAndPathParameters(t *testing.T) {
	t.Run("placeholders in the target's own path are rejected", func(t *testing.T) {
		file := &TargetFile{Targets: []ScheduledTarget{{Name: "one", Collector: "tenants", Target: "http://a.example",
			Request: TargetRequestConfig{Path: "/api/{{param_tenant:acme}}", PathSet: true}}}}
		if err := file.Validate(); err == nil || !strings.Contains(err.Error(), "no probe to supply them") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("a collector placeholder without a default is rejected", func(t *testing.T) {
		cfg := &Config{Collectors: []Collector{pathCollector(tenantPath)}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		file := &TargetFile{Targets: []ScheduledTarget{{Name: "one", Collector: "tenants", Target: "http://a.example"}}}
		if err := file.Validate(); err != nil {
			t.Fatal(err)
		}
		if err := file.ValidateAgainst(cfg); err == nil || !strings.Contains(err.Error(), "without a default") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("a target setting its own path does not need the defaults", func(t *testing.T) {
		cfg := &Config{Collectors: []Collector{pathCollector(tenantPath)}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		file := &TargetFile{Targets: []ScheduledTarget{{Name: "one", Collector: "tenants", Target: "http://a.example",
			Request: TargetRequestConfig{Path: "/api/acme/v2/status", PathSet: true}}}}
		if err := file.Validate(); err != nil {
			t.Fatal(err)
		}
		if err := file.ValidateAgainst(cfg); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("placeholders with defaults take them", func(t *testing.T) {
		var recorder pathRecorder
		target := recorder.serve(t)
		cfg := &Config{Collectors: []Collector{pathCollector("/api/{{param_tenant:acme}}/status")}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
		file := &TargetFile{Targets: []ScheduledTarget{{Name: "one", Collector: "tenants", Target: target.URL}}}
		server := newScheduledServer(t, cfg, file)

		server.scrapeScheduledTargets(context.Background(), 10*time.Second)
		if got := recorder.last(t); got != "/api/acme/status" {
			t.Fatalf("target received %q, want the default", got)
		}
	})
}

// The token that holds a value's place through path.Join must never reach the
// wire, whatever the value.
func TestNoPlaceholderTokenLeaksIntoTheURL(t *testing.T) {
	c := pathCollector("/{{param_a}}/{{param_b:x}}/{{param_a}}/")
	for _, value := range []string{"v", "a/b", "%00", "0", "1"} {
		u, err := resolveRequestURL("http://h.example/base", &c, RequestOverrides{Params: map[string]string{"param_a": value}})
		if err != nil {
			t.Fatal(err)
		}
		rendered := u.String()
		if strings.Contains(u.Path, "\x00") || strings.Contains(rendered, "%00") && value != "%00" {
			t.Fatalf("value %q left a token in %q", value, rendered)
		}
		escaped := url.PathEscape(value)
		want := "http://h.example/base/" + escaped + "/x/" + escaped + "/"
		if rendered != want {
			t.Fatalf("value %q rendered %q, want %q", value, rendered, want)
		}
	}
}
