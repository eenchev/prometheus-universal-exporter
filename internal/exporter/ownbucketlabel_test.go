//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A histogram's buckets carry le and a summary's quantiles quantile. A rule
// or a pre-script that gives such a series that label for itself would have
// it written on _sum and _count as well, exposition this exporter's own
// decoder, and Prometheus's, refuses. The probe fails saying so and naming
// the series, rather than answering with it.
func TestAProbeRefusesAHistogramGivenAnLeOfItsOwn(t *testing.T) {
	testutil.CaptureLogs(t)
	const exposition = "# TYPE h histogram\nh_bucket{le=\"1\"} 5\nh_bucket{le=\"+Inf\"} 7\nh_sum 3\nh_count 7\n# TYPE s summary\ns{quantile=\"0.5\"} 2\ns_sum 4\ns_count 2\n# TYPE g gauge\ng 1\n"
	target := utf8Target(t, exposition)

	rule := passthrough("rule", "", "")
	rule.Metrics = []model.MetricRule{{Name: "h", Labels: []model.LabelRule{{Name: "le", Value: "own"}}}}
	quantile := passthrough("quantile", "", "")
	quantile.Metrics = []model.MetricRule{{Name: "s", Labels: []model.LabelRule{{Name: "quantile", Value: "own"}}}}
	// The same labels on a gauge are labels like any other.
	gauge := passthrough("gauge", "", "")
	gauge.Metrics = []model.MetricRule{{Name: "g", Labels: []model.LabelRule{{Name: "le", Value: "1"}, {Name: "quantile", Value: "0.5"}}}}
	script := passthrough("script", "", "")
	script.Transform.PreScript = `for s in data["metrics"]:
    if s["name"] == "h":
        s["labels"]["le"] = "own"
`
	server := verboseServer(t, false, rule, quantile, gauge, script)

	for collector, want := range map[string]string{
		"rule":     `metric "h" is a histogram and has a label le of its own, which its buckets carry; name the label something else`,
		"quantile": `metric "s" is a summary and has a label quantile of its own, which its quantiles carry; name the label something else`,
		"script":   `metric "h" is a histogram and has a label le of its own, which its buckets carry; name the label something else`,
	} {
		t.Run(collector, func(t *testing.T) {
			if collector == "script" {
				requirePython(t)
			}
			response := probeOnce(t, server, "/probe?collector="+collector+"&target="+url.QueryEscape(target.URL), nil)
			if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), want) {
				t.Errorf("%d %s, want 502 saying %s", response.Code, response.Body, want)
			}
		})
	}
	response := probeOnce(t, server, "/probe?collector=gauge&target="+url.QueryEscape(target.URL), nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `g{le="1",quantile="0.5"} 1`) {
		t.Errorf("gauge: %d %s", response.Code, response.Body)
	}
}
