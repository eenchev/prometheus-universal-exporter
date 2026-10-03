//go:build !select_request_types || (request_type_http && request_type_graphite)

package exporter

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// A probe asks Graphite for the collector's expressions, placeholders
// filled from the probe, and maps the series with jq: the newest value of
// each, a series of nulls and one older than max_age left out.
func TestAGraphiteCollectorProbe(t *testing.T) {
	graphite := &fakeGraphite{}
	upstream := httptest.NewServer(graphite)
	defer upstream.Close()
	server := graphiteExporter(t, graphiteExporterConfig)
	recorder := probeOnce(t, server, "/probe?collector=graphite_app&param_env=staging&target="+url.QueryEscape(upstream.URL), nil)
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK {
		t.Fatalf("%d %s", recorder.Code, body)
	}
	for _, want := range []string{`app_requests{host="web01"} 42`, `app_requests{host="web02"} 7`, `cpu_load{env="staging"} 0.5`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q:\n%s", want, body)
		}
	}
	for _, absent := range []string{"web03", `host="old"`} {
		if strings.Contains(body, absent) {
			t.Errorf("%s is exported:\n%s", absent, body)
		}
	}
	asked := graphite.asked()
	if len(asked) != 1 || strings.Join(asked[0], " | ") != "app.*.requests | seriesByTag('name=cpu.load', 'env=staging')" {
		t.Fatalf("asked %q", asked)
	}
	// The series of nulls and the one older than max_age are counted.
	if metrics := selfMetrics(t, server); !strings.Contains(metrics, `http_exporter_decoder_series_left_out_total{collector="graphite_app"} 2`) {
		t.Fatalf("%s", metrics)
	}
	// A value that could change the expression is refused before Graphite
	// is asked; so is a probe parameter of http's alone.
	for query, want := range map[string]string{
		"param_env=" + url.QueryEscape("x'),sumSeries('y"): "lands inside a Graphite expression",
		"method=POST": `request.type is "graphite"`,
	} {
		recorder = probeOnce(t, server, "/probe?collector=graphite_app&target="+url.QueryEscape(upstream.URL)+"&"+query, nil)
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), want) {
			t.Errorf("%s: %d %s", query, recorder.Code, recorder.Body)
		}
	}
	if len(graphite.asked()) != 1 {
		t.Fatalf("a refused probe reached Graphite: %q", graphite.asked())
	}
}
