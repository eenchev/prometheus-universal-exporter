//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// End to end: a target's histogram of buckets alone and its summary of
// quantiles alone pass through a probe unchanged in either format, and one
// with a bucket written twice fails the probe naming the series.
func TestIncompleteHistogramsAndSummariesPassThroughAProbe(t *testing.T) {
	bodies := map[string]string{
		"/alone":     "# TYPE h histogram\nh_bucket{le=\"1\"} 5\nh_bucket{le=\"+Inf\"} 7\n# TYPE s summary\ns{quantile=\"0.5\"} 2\n",
		"/duplicate": "# TYPE h histogram\nh_bucket{le=\"1\"} 5\nh_bucket{le=\"1.0\"} 6\nh_bucket{le=\"+Inf\"} 7\nh_sum 3\nh_count 7\n",
	}
	target := utf8Target(t, "")
	target.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(bodies[r.URL.Path]))
	})
	server := verboseServer(t, false, passthrough("pass", "", ""))
	server.logger = testutil.QuietLogger(t)
	probe := func(path, accept string) (int, string) {
		r := probeOnce(t, server, "/probe?collector=pass&target="+url.QueryEscape(target.URL+path), http.Header{"Accept": {accept}})
		return r.Code, r.Body.String()
	}
	if code, body := probe("/alone", "text/plain"); code != http.StatusOK || body != bodies["/alone"] {
		t.Fatalf("text: %d\n%s", code, body)
	}
	wantOM := "# TYPE h histogram\nh_bucket{le=\"1.0\"} 5\nh_bucket{le=\"+Inf\"} 7\n# TYPE s summary\ns{quantile=\"0.5\"} 2\n# EOF\n"
	if code, body := probe("/alone", "application/openmetrics-text"); code != http.StatusOK || body != wantOM {
		t.Fatalf("OpenMetrics: %d\n%s", code, body)
	}
	want := "the histogram h, which starts in line 2, has two buckets with the upper bound 1"
	if code, body := probe("/duplicate", "text/plain"); code != http.StatusBadGateway || !strings.Contains(body, want) {
		t.Errorf("/duplicate: %d %s, want 502 with %q", code, body, want)
	}
}
