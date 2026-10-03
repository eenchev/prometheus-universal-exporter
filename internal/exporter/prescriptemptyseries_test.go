//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// A pre-script that misspelled a histogram's keys was answered 200 with a
// "# TYPE made histogram" line and no sample under it, in the text format
// and in OpenMetrics. The probe fails, saying which keys a histogram series
// has, and no line of the family is written.
func TestAProbeFailsOnAPreScriptThatLeavesAHistogramWithNothing(t *testing.T) {
	requirePython(t)
	target := utf8Target(t, "# TYPE up_thing gauge\nup_thing 1\n")
	c := passthrough("pass", "", "")
	c.Limits = scriptLimits()
	c.Transform.PreScript = `data["metrics"].append({"name": "made", "type": "histogram", "bucket": [{"le": 1, "count": 1}], "cnt": 1, "labels": {"a": "b"}})`
	server := verboseServer(t, false, c)
	for _, accept := range []string{"text/plain", prometheus3Accept} {
		r := probeOnce(t, server, "/probe?collector=pass&target="+url.QueryEscape(target.URL), http.Header{"Accept": {accept}})
		answer := r.Body.String()
		if r.Code != http.StatusBadGateway || !strings.Contains(answer, `the histogram made has no buckets, no sum and no count; a histogram series has the keys "buckets", "sum" and "count", and this one has the keys "bucket", "cnt", "labels", "name", "type"`) || strings.Contains(answer, "# TYPE made") {
			t.Errorf("%s: %d %s", accept, r.Code, answer)
		}
	}
}
