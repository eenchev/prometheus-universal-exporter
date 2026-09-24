package config

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// request.type is required and selects how a collector reaches its data. Each
// type owns the request keys, probe parameters and scheduled-target keys it
// accepts, its own validation, and its own fetch.

func typedCollector(requestType string) model.Collector {
	c := testutil.Collector("typed", "text")
	c.Request = model.RequestConfig{Type: requestType}
	return c
}

func TestRequestTypeIsRequired(t *testing.T) {
	err := Validate(&model.Config{Collectors: []model.Collector{typedCollector("")}})
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
	for _, name := range []string{"grpc", "ftpfile", "https", "file"} {
		err := Validate(&model.Config{Collectors: []model.Collector{typedCollector(name)}})
		if err == nil || !strings.Contains(err.Error(), `unsupported request.type "`+name+`"`) || !strings.Contains(err.Error(), "http") {
			t.Errorf("%s: err=%v, want it rejected with the supported types listed", name, err)
		}
	}
}

func TestRequestTypeIsNormalised(t *testing.T) {
	for _, given := range []string{"http", "HTTP", " Http "} {
		cfg := &model.Config{Collectors: []model.Collector{typedCollector(given)}}
		if err := Validate(cfg); err != nil {
			t.Fatalf("%q: %v", given, err)
		}
		if got := cfg.Collectors[0].Request.Type; got != fetch.RequestTypeHTTP {
			t.Fatalf("%q was stored as %q", given, got)
		}
	}
}

// For http nothing but type is required: path may be empty because the target
// URL can carry the whole path, and method defaults to GET.
func TestHTTPRequiresOnlyTheType(t *testing.T) {
	cfg := &model.Config{Collectors: []model.Collector{typedCollector(fetch.RequestTypeHTTP)}}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Collectors[0].Request.Method; got != http.MethodGet {
		t.Fatalf("method defaulted to %q, want GET", got)
	}
}

// Every request key http documents is accepted together. A key that is set but
// not listed for the type would be rejected, so this pins the list.
func TestHTTPAcceptsEveryHTTPKey(t *testing.T) {
	c := typedCollector(fetch.RequestTypeHTTP)
	c.Request = model.RequestConfig{
		Type: fetch.RequestTypeHTTP, Method: "POST", Path: "/api/{{param_tenant:acme}}", Query: map[string]string{"a": "b"},
		Headers: map[string]string{"Accept": "application/json"}, Body: "{}",
		BearerTokenFile: "/run/token", ForwardHeaders: []string{"X-Tenant"},
		TLS: model.TLSConfig{InsecureSkipVerify: true}, Retry: model.RetryConfig{Attempts: 2, Backoff: model.Duration(1)},
		MaxResponseBytes: 1024, FollowRedirects: true, EnableHTTP2: true, AllowedSchemes: []string{"https"},
	}
	if err := Validate(&model.Config{Collectors: []model.Collector{c}}); err != nil {
		t.Fatal(err)
	}
}

// The http rules moved with the type; they still apply.
func TestHTTPRulesStillApply(t *testing.T) {
	tests := []struct {
		name   string
		adjust func(*model.RequestConfig)
		want   string
	}{
		{"unsupported method", func(r *model.RequestConfig) { r.Method = "TRACE" }, "unsupported method"},
		{"two bearer sources", func(r *model.RequestConfig) { r.BearerToken, r.BearerTokenFile = "t", "/f" }, "bearer_token_file"},
		{"two basic sources", func(r *model.RequestConfig) {
			r.BasicAuth, r.BasicAuthFile = &model.BasicAuth{Username: "u"}, &model.BasicAuthFile{Username: "/u", Password: "/p"}
		}, "basic_auth_file"},
		{"basic and bearer", func(r *model.RequestConfig) { r.BasicAuth, r.BearerToken = &model.BasicAuth{Username: "u"}, "t" }, "basic and bearer"},
		{"negative retries", func(r *model.RequestConfig) { r.Retry.Attempts = -1 }, "retry.attempts"},
		{"malformed path parameter", func(r *model.RequestConfig) { r.Path = "/{{tenant}}" }, "not a path parameter"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := typedCollector(fetch.RequestTypeHTTP)
			test.adjust(&c.Request)
			err := Validate(&model.Config{Collectors: []model.Collector{c}})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v, want %q", err, test.want)
			}
		})
	}
}

// fixtureType is a second request type registered only for these tests, so
// the per-type rules can be exercised while http is the only real one. It
// accepts path and nothing else, and its fetch returns a canned body.
func registerFixtureType(t *testing.T) {
	t.Helper()
	fetch.RequestTypes["fixture"] = &fetch.RequestType{
		Name:         "fixture",
		Fields:       []string{"path"},
		Overrides:    []string{"path"},
		TargetFields: []string{"path"},
		Validate:     func(*model.Collector) error { return nil },
		Fetch: func(_ context.Context, target string, c *model.Collector, _ fetch.RequestOverrides, _ http.Header) (*fetch.HTTPResponse, error) {
			return &fetch.HTTPResponse{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": {"text/plain"}}, Body: []byte("value=5\n"), Target: target, Collector: c.Name}, nil
		},
	}
	t.Cleanup(func() { delete(fetch.RequestTypes, "fixture") })
}

func fixtureCollector() model.Collector {
	c := testutil.Collector("fixed", "text")
	c.Request = model.RequestConfig{Type: "fixture", Path: "/data"}
	return c
}

func TestAKeyThatBelongsToAnotherTypeIsRejected(t *testing.T) {
	registerFixtureType(t)
	c := fixtureCollector()
	c.Request.Method = "GET"
	err := Validate(&model.Config{Collectors: []model.Collector{c}})
	if err == nil || !strings.Contains(err.Error(), `request.method, which does not apply to request.type "fixture"`) {
		t.Fatalf("err=%v", err)
	}
	// The same key is fine for http, which owns it.
	if err := Validate(&model.Config{Collectors: []model.Collector{typedCollector(fetch.RequestTypeHTTP)}}); err != nil {
		t.Fatal(err)
	}
}

func TestScheduledTargetKeysFollowTheRequestType(t *testing.T) {
	registerFixtureType(t)
	cfg := &model.Config{Collectors: []model.Collector{fixtureCollector()}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	withMethod := &model.TargetFile{Targets: []model.ScheduledTarget{{Name: "one", Collector: "fixed", Target: "fixture://a", Request: model.TargetRequestConfig{Method: "POST"}}}}
	if err := ValidateTargets(withMethod); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTargetsAgainst(withMethod, cfg); err == nil || !strings.Contains(err.Error(), `request.method, which does not apply to collector "fixed"`) {
		t.Fatalf("err=%v", err)
	}
	withPath := &model.TargetFile{Targets: []model.ScheduledTarget{{Name: "one", Collector: "fixed", Target: "fixture://a", Request: model.TargetRequestConfig{Path: "/b", PathSet: true}}}}
	if err := ValidateTargets(withPath); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTargetsAgainst(withPath, cfg); err != nil {
		t.Fatalf("a key the type accepts should pass: %v", err)
	}
}
