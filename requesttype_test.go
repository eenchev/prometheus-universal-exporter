package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// request.type is required and selects how a collector reaches its data. Each
// type owns the request keys, probe parameters and scheduled-target keys it
// accepts, its own validation, and its own fetch.

func typedCollector(requestType string) Collector {
	c := testCollector("typed", "text")
	c.Request = RequestConfig{Type: requestType}
	return c
}

func TestRequestTypeIsRequired(t *testing.T) {
	err := (&Config{Collectors: []Collector{typedCollector("")}}).Validate()
	if err == nil {
		t.Fatal("a collector without request.type must be rejected")
	}
	for _, want := range []string{`"typed"`, "request.type", "required", "type: http"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestAnUnknownRequestTypeIsRejected(t *testing.T) {
	for _, name := range []string{"grpc", "localfile", "ftpfile", "https"} {
		err := (&Config{Collectors: []Collector{typedCollector(name)}}).Validate()
		if err == nil || !strings.Contains(err.Error(), `unsupported request.type "`+name+`"`) || !strings.Contains(err.Error(), "http") {
			t.Errorf("%s: err=%v, want it rejected with the supported types listed", name, err)
		}
	}
}

func TestRequestTypeIsNormalised(t *testing.T) {
	for _, given := range []string{"http", "HTTP", " Http "} {
		cfg := &Config{Collectors: []Collector{typedCollector(given)}}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("%q: %v", given, err)
		}
		if got := cfg.Collectors[0].Request.Type; got != RequestTypeHTTP {
			t.Fatalf("%q was stored as %q", given, got)
		}
	}
}

// For http nothing but type is required: path may be empty because the target
// URL can carry the whole path, and method defaults to GET.
func TestHTTPRequiresOnlyTheType(t *testing.T) {
	cfg := &Config{Collectors: []Collector{typedCollector(RequestTypeHTTP)}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Collectors[0].Request.Method; got != http.MethodGet {
		t.Fatalf("method defaulted to %q, want GET", got)
	}
}

// Every request key http documents is accepted together. A key that is set but
// not listed for the type would be rejected, so this pins the list.
func TestHTTPAcceptsEveryHTTPKey(t *testing.T) {
	c := typedCollector(RequestTypeHTTP)
	c.Request = RequestConfig{
		Type: RequestTypeHTTP, Method: "POST", Path: "/api/{{param_tenant:acme}}", Query: map[string]string{"a": "b"},
		Headers: map[string]string{"Accept": "application/json"}, Body: "{}",
		BearerTokenFile: "/run/token", ForwardHeaders: []string{"X-Tenant"},
		TLS: TLSConfig{InsecureSkipVerify: true}, Retry: RetryConfig{Attempts: 2, Backoff: Duration(1)},
		MaxResponseBytes: 1024, FollowRedirects: true, EnableHTTP2: true, AllowedSchemes: []string{"https"},
	}
	if err := (&Config{Collectors: []Collector{c}}).Validate(); err != nil {
		t.Fatal(err)
	}
}

// The http rules moved with the type; they still apply.
func TestHTTPRulesStillApply(t *testing.T) {
	tests := []struct {
		name   string
		adjust func(*RequestConfig)
		want   string
	}{
		{"unsupported method", func(r *RequestConfig) { r.Method = "TRACE" }, "unsupported method"},
		{"two bearer sources", func(r *RequestConfig) { r.BearerToken, r.BearerTokenFile = "t", "/f" }, "bearer_token_file"},
		{"two basic sources", func(r *RequestConfig) {
			r.BasicAuth, r.BasicAuthFile = &BasicAuth{Username: "u"}, &BasicAuthFile{Username: "/u", Password: "/p"}
		}, "basic_auth_file"},
		{"basic and bearer", func(r *RequestConfig) { r.BasicAuth, r.BearerToken = &BasicAuth{Username: "u"}, "t" }, "basic and bearer"},
		{"negative retries", func(r *RequestConfig) { r.Retry.Attempts = -1 }, "retry.attempts"},
		{"malformed path parameter", func(r *RequestConfig) { r.Path = "/{{tenant}}" }, "not a path parameter"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := typedCollector(RequestTypeHTTP)
			test.adjust(&c.Request)
			err := (&Config{Collectors: []Collector{c}}).Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v, want %q", err, test.want)
			}
		})
	}
}

// Every request key must belong to at least one type. A field added to
// RequestConfig without deciding which types accept it would be rejected for
// all of them; this makes that decision impossible to forget.
func TestEveryRequestKeyBelongsToAType(t *testing.T) {
	claimed := map[string]bool{"type": true}
	targetClaimed := map[string]bool{}
	for _, rt := range requestTypes {
		for _, key := range rt.Fields {
			claimed[key] = true
		}
		for _, key := range rt.TargetFields {
			targetClaimed[key] = true
		}
		if rt.Validate == nil || rt.Fetch == nil {
			t.Errorf("request type %q is missing Validate or Fetch", rt.Name)
		}
	}
	for _, key := range yamlKeys(reflect.TypeOf(RequestConfig{})) {
		if !claimed[key] {
			t.Errorf("request.%s is accepted by no request type", key)
		}
	}
	for _, key := range yamlKeys(reflect.TypeOf(TargetRequestConfig{})) {
		if !targetClaimed[key] {
			t.Errorf("a scheduled target's request.%s is accepted by no request type", key)
		}
	}
}

func yamlKeys(t reflect.Type) []string {
	var keys []string
	for i := 0; i < t.NumField(); i++ {
		key, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		if key != "" && key != "-" {
			keys = append(keys, key)
		}
	}
	return keys
}

// fixtureType is a second request type registered only for these tests, so
// the per-type rules can be exercised while http is the only real one. It
// accepts path and nothing else, and its fetch returns a canned body.
func registerFixtureType(t *testing.T) {
	t.Helper()
	requestTypes["fixture"] = &requestType{
		Name:         "fixture",
		Fields:       []string{"path"},
		Overrides:    []string{"path"},
		TargetFields: []string{"path"},
		Validate:     func(*Collector) error { return nil },
		Fetch: func(_ context.Context, target string, c *Collector, _ RequestOverrides, _ http.Header) (*HTTPResponse, error) {
			return &HTTPResponse{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": {"text/plain"}}, Body: []byte("value=5\n"), Target: target, Collector: c.Name}, nil
		},
	}
	t.Cleanup(func() { delete(requestTypes, "fixture") })
}

func fixtureCollector() Collector {
	c := testCollector("fixed", "text")
	c.Request = RequestConfig{Type: "fixture", Path: "/data"}
	return c
}

func TestAKeyThatBelongsToAnotherTypeIsRejected(t *testing.T) {
	registerFixtureType(t)
	c := fixtureCollector()
	c.Request.Method = "GET"
	err := (&Config{Collectors: []Collector{c}}).Validate()
	if err == nil || !strings.Contains(err.Error(), `request.method, which does not apply to request.type "fixture"`) {
		t.Fatalf("err=%v", err)
	}
	// The same key is fine for http, which owns it.
	if err := (&Config{Collectors: []Collector{typedCollector(RequestTypeHTTP)}}).Validate(); err != nil {
		t.Fatal(err)
	}
}

// Decoding, transforms and exposition are shared by every type: a collector
// whose type fetches its bytes some other way is served like any other.
func TestAProbeFetchesThroughTheCollectorsType(t *testing.T) {
	registerFixtureType(t)
	server := pathServer(t, fixtureCollector())
	response := probeQueryString(t, server, "target=fixture://data&collector=fixed")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "demo_value 5") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestProbeParametersFollowTheRequestType(t *testing.T) {
	registerFixtureType(t)
	server := pathServer(t, fixtureCollector())
	base := "target=fixture://data&collector=fixed"

	for _, param := range []string{"method=POST", "timeout=5s", "header_X-Tenant=a", "param_tenant=a", "retry_attempts=2"} {
		response := probeQueryString(t, server, base+"&"+param)
		name, _, _ := strings.Cut(param, "=")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), name) || !strings.Contains(response.Body.String(), `"fixture"`) {
			t.Errorf("%s: status=%d body=%s, want a 400 naming the parameter and the type", param, response.Code, response.Body.String())
		}
	}
	// A parameter the type accepts is fine, and one no type knows is ignored
	// as it always was, since a monitor may carry parameters of its own.
	for _, param := range []string{"path=/other", "unrelated=1"} {
		if response := probeQueryString(t, server, base+"&"+param); response.Code != http.StatusOK {
			t.Errorf("%s: status=%d body=%s", param, response.Code, response.Body.String())
		}
	}
}

// Every override http documents is accepted for an http collector.
func TestHTTPAcceptsEveryHTTPOverride(t *testing.T) {
	var recorder pathRecorder
	target := recorder.serve(t)
	c := pathCollector("/api/{{param_tenant}}")
	c.Request.ForwardHeaders = []string{"X-Tenant"}
	server := pathServer(t, c)
	response := probeWith(t, server, target.URL, "&param_tenant=a&method=GET&timeout=5s&body=x&insecure_skip_verify=false"+
		"&follow_redirects=false&enable_http2=false&retry_attempts=0&retry_backoff=0s&header_X-Tenant=a")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestScheduledTargetKeysFollowTheRequestType(t *testing.T) {
	registerFixtureType(t)
	cfg := &Config{Collectors: []Collector{fixtureCollector()}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	withMethod := &TargetFile{Targets: []ScheduledTarget{{Name: "one", Collector: "fixed", Target: "fixture://a", Request: TargetRequestConfig{Method: "POST"}}}}
	if err := withMethod.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := withMethod.ValidateAgainst(cfg); err == nil || !strings.Contains(err.Error(), `request.method, which does not apply to collector "fixed"`) {
		t.Fatalf("err=%v", err)
	}
	withPath := &TargetFile{Targets: []ScheduledTarget{{Name: "one", Collector: "fixed", Target: "fixture://a", Request: TargetRequestConfig{Path: "/b", PathSet: true}}}}
	if err := withPath.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := withPath.ValidateAgainst(cfg); err != nil {
		t.Fatalf("a key the type accepts should pass: %v", err)
	}
}

// The configuration the chart ships by default has to start, so it declares
// its request type like any other.
func TestTheChartsDefaultConfigurationIsValid(t *testing.T) {
	var values struct {
		Config struct {
			Data map[string]string `yaml:"data"`
		} `yaml:"config"`
	}
	raw, err := os.ReadFile("charts/prometheus-universal-exporter/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	path := writeFile(t, "config.yaml", values.Config.Data["config.yaml"])
	if _, err := LoadConfig(path); err != nil {
		t.Fatalf("the chart's default configuration does not load: %v", err)
	}
}

// --dry-run reports a missing type like any other invalid configuration.
func TestDryRunReportsAMissingRequestType(t *testing.T) {
	untyped := strings.Replace(minimalConfig, "      type: http\n", "", 1)
	out := runCheckCLI(t, "--config.file="+writeFile(t, "config.yaml", untyped))
	if out.code != 1 || !strings.Contains(out.result(t, "config").Errors[0], "request.type") {
		t.Fatalf("exit=%d\n%s", out.code, out.stdout)
	}
}

func probeQueryString(t *testing.T, server *Server, query string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/probe?"+query, nil))
	return recorder
}
