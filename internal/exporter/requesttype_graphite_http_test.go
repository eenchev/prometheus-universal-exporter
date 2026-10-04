//go:build !select_request_types || (request_type_http && request_type_graphite)

package exporter

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// A probe parameter of http's alone is refused before Graphite is asked,
// where the build has http: in a build without it method is a parameter no
// request type knows. TestAGraphiteCollectorProbe has the probe itself and
// the value it refuses.
func TestAGraphiteProbeRefusesAProbeParameterOfHTTPs(t *testing.T) {
	graphite := &fakeGraphite{}
	upstream := httptest.NewServer(graphite)
	defer upstream.Close()
	server := graphiteExporter(t, graphiteExporterConfig)
	recorder := probeOnce(t, server, "/probe?collector=graphite_app&target="+url.QueryEscape(upstream.URL)+"&method=POST", nil)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `request.type is "graphite"`) {
		t.Errorf("method=POST: %d %s", recorder.Code, recorder.Body)
	}
	if len(graphite.asked()) != 0 {
		t.Fatalf("a refused probe reached Graphite: %q", graphite.asked())
	}
}
