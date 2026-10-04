//go:build !select_request_types || (request_type_http && request_type_graphite)

package fetch

import (
	"net/url"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A probe parameter of http's alone is refused for a graphite collector,
// where the build has http: in a build without it method is a parameter no
// request type knows. TestGraphiteRequestValidation has the parameters a
// graphite collector takes.
func TestAGraphiteCollectorRefusesAProbeParameterOfHTTPs(t *testing.T) {
	c := graphiteCollector(`aliasSub(sumSeries(app.{web,api}[0-9].x), '(\w+)', "\1")`, "seriesByTag('name=a', 'env=~(prod|staging)')")
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	if err := CheckOverrideParams(&c, url.Values{"method": {"POST"}}); err == nil || !strings.Contains(err.Error(), `request.type is "graphite"`) {
		t.Fatalf("method: err=%v", err)
	}
}

// from and until are refused for a collector of another type, where the
// build has graphite, whose parameters they are.
// TestGraphiteWindowProbeParameters has a graphite collector, which takes
// them.
func TestGraphiteWindowProbeParametersAreRefusedForAnHTTPCollector(t *testing.T) {
	h := model.Collector{Name: "web", Request: model.RequestConfig{Type: RequestTypeHTTP}}
	if err := ValidateRequest(&h); err != nil {
		t.Fatal(err)
	}
	if err := CheckOverrideParams(&h, url.Values{"from": {"-1h"}}); err == nil || !strings.Contains(err.Error(), `probe parameters from do not apply to collector "web"`) {
		t.Fatalf("err=%v", err)
	}
}
