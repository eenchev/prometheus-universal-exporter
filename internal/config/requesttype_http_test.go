//go:build !select_request_types || request_type_http

package config

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

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
	for _, name := range []string{"soap", "ftpfile", "https", "file"} {
		err := Validate(&model.Config{Collectors: []model.Collector{typedCollector(name)}})
		if err == nil || !strings.Contains(err.Error(), `unsupported request.type "`+name+`"`) || !strings.Contains(err.Error(), "http") {
			t.Errorf("%s: err=%v, want it rejected with the supported types listed", name, err)
		}
	}
}

// A request type is taken as the schemas take it, in lower case and
// without blanks around it; written otherwise, it is refused naming the
// type as it is written. The exporter took any case and blanks around it.
func TestRequestTypeIsTakenAsWritten(t *testing.T) {
	cfg := &model.Config{Collectors: []model.Collector{typedCollector("http")}}
	if err := Validate(cfg); err != nil || cfg.Collectors[0].Request.Type != fetch.RequestTypeHTTP {
		t.Fatalf("http: %v, stored as %q", err, cfg.Collectors[0].Request.Type)
	}
	for _, given := range []string{"HTTP", " Http ", "http ", "Http"} {
		err := Validate(&model.Config{Collectors: []model.Collector{typedCollector(given)}})
		if want := fmt.Sprintf(`collector "typed" has unsupported request.type %q; a type is written in lower case and without blanks around it: http`, given); err == nil || err.Error() != want {
			t.Errorf("%q: err=%v, want %q", given, err, want)
		}
	}
	// A type no build has is refused as unknown, whatever its case.
	if err := Validate(&model.Config{Collectors: []model.Collector{typedCollector("SOAP")}}); err == nil || !strings.Contains(err.Error(), `unsupported request.type "SOAP"; the supported types are: `) {
		t.Errorf("SOAP: err=%v", err)
	}
}

// For http nothing but type is required: path may be empty because the target
// URL can carry the whole path, and method defaults to GET.
func TestHTTPRequiresOnlyTheType(t *testing.T) {
	cfg := &model.Config{Collectors: []model.Collector{typedCollector(fetch.RequestTypeHTTP)}}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Collectors[0].Request.Own.(fetch.HTTPConfig).Method; got != http.MethodGet {
		t.Fatalf("method defaulted to %q, want GET", got)
	}
}

// Every request key http documents is accepted together. A key that is set but
// not listed for the type would be rejected, so this pins the list.
func TestHTTPAcceptsEveryHTTPKey(t *testing.T) {
	c := typedCollector(fetch.RequestTypeHTTP)
	c.Request = model.RequestConfig{
		Type: fetch.RequestTypeHTTP, Path: "/api/{{param_tenant:acme}}", Query: map[string]string{"a": "b"},
		Headers:         map[string]string{"Accept": "application/json"},
		BearerTokenFile: "/run/token", ForwardHeaders: []string{"X-Tenant"},
		TLS: model.TLSConfig{InsecureSkipVerify: true}, Retry: model.RetryConfig{Attempts: 2, Backoff: model.Duration(1)},
		MaxResponseBytes: 1024, FollowRedirects: true, EnableHTTP2: true, AllowedSchemes: []string{"https"}, Own: fetch.HTTPConfig{Method: "POST", Body: "{}"},
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
		{"unsupported method", func(r *model.RequestConfig) { httpOwn(r, func(o *fetch.HTTPConfig) { o.Method = "TRACE" }) }, "unsupported method"},
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

func TestAKeyThatBelongsToAnotherTypeIsRejected(t *testing.T) {
	registerFixtureType(t)
	c := fixtureCollector()
	editHTTP(&c, func(r *fetch.HTTPConfig) { r.Method = "GET" })
	err := Validate(&model.Config{Collectors: []model.Collector{c}})
	if err == nil || !strings.Contains(err.Error(), `request.method, which does not apply to request.type "fixture"`) {
		t.Fatalf("err=%v", err)
	}
	// The same key is fine for http, which owns it.
	if err := Validate(&model.Config{Collectors: []model.Collector{typedCollector(fetch.RequestTypeHTTP)}}); err != nil {
		t.Fatal(err)
	}
}

// The localfile keys do not apply to http: an http collector that sets one
// is refused by the key's name, in a build without localfile too.
// TestLocalFileValidation has what a localfile collector's request must be.
func TestLocalFileKeysDoNotApplyToHTTP(t *testing.T) {
	root := t.TempDir()
	for key, change := range map[string]func(*model.Collector){
		"root":    func(c *model.Collector) { c.Request.Root = root },
		"max_age": func(c *model.Collector) { c.Request.MaxAge = model.Duration(time.Minute) },
	} {
		c := testutil.Collector("web", "text")
		change(&c)
		err := Validate(&model.Config{Collectors: []model.Collector{c}})
		if err == nil || !strings.Contains(err.Error(), "request."+key+`, which does not apply to request.type "http"`) {
			t.Errorf("%s on http: err=%v", key, err)
		}
	}
}
