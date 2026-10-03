package config

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// request.type is required and selects how a collector reaches its data. Each
// type owns the request keys, probe parameters and static-target keys it
// accepts, its own validation, and its own fetch.

func typedCollector(requestType string) model.Collector {
	c := testutil.Collector("typed", "text")
	c.Request = model.RequestConfig{Type: requestType}
	return c
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

func TestStaticTargetKeysFollowTheRequestType(t *testing.T) {
	registerFixtureType(t)
	cfg := &model.Config{Collectors: []model.Collector{fixtureCollector()}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	withMethod := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "one", Collector: "fixed", Target: "fixture://a", Request: model.TargetRequestConfig{Method: "POST"}}}}
	if err := ValidateStaticTargets(withMethod); err != nil {
		t.Fatal(err)
	}
	if err := ValidateStaticTargetsAgainst(withMethod, cfg); err == nil || !strings.Contains(err.Error(), `request.method, which does not apply to collector "fixed"`) {
		t.Fatalf("err=%v", err)
	}
	withPath := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "one", Collector: "fixed", Target: "fixture://a", Request: model.TargetRequestConfig{Path: "/b", PathSet: true}}}}
	if err := ValidateStaticTargets(withPath); err != nil {
		t.Fatal(err)
	}
	if err := ValidateStaticTargetsAgainst(withPath, cfg); err != nil {
		t.Fatalf("a key the type accepts should pass: %v", err)
	}
}
