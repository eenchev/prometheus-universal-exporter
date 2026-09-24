package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A configuration naming a real type that this build left out is told so, and
// told how to get a build with it; a name that is no type at all is not.
func TestARequestTypeLeftOutOfTheBuildIsNamedAsSuch(t *testing.T) {
	saved := fetch.RequestTypes[fetch.RequestTypeHTTP]
	delete(fetch.RequestTypes, fetch.RequestTypeHTTP)
	registerFixtureType(t)
	t.Cleanup(func() { fetch.RequestTypes[fetch.RequestTypeHTTP] = saved })

	err := Validate(&model.Config{Collectors: []model.Collector{typedCollector(fetch.RequestTypeHTTP)}})
	if err == nil {
		t.Fatal("a type missing from the build must be rejected")
	}
	for _, want := range []string{`request.type "http"`, "does not include", "built with: fixture", "request_type_http"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
	err = Validate(&model.Config{Collectors: []model.Collector{typedCollector("gopher")}})
	if err == nil || !strings.Contains(err.Error(), `unsupported request.type "gopher"`) {
		t.Errorf("err=%v", err)
	}
}
