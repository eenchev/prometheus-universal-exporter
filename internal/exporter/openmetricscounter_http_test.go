//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"net/url"
	"slices"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// End to end: a target's counters with UTF-8 names, escaped by name_escaping
// values, are counters in OpenMetrics, each sample its family and _total,
// beside a histogram of such a name; the exporter's own reader finds every
// counter in the answer; and the text format has the names as they were
// escaped.
func TestEscapedCountersThroughAProbeInOpenMetrics(t *testing.T) {
	target := utf8Target(t, "# TYPE \"my.requests_total\" counter\n{\"my.requests_total\"} 3\n# TYPE \"my.errors\" counter\n{\"my.errors\"} 1\n# TYPE \"my.total\" counter\n{\"my.total\"} 2\n# TYPE plain_total counter\nplain_total 4\n"+
		"# TYPE \"my.hist\" histogram\n{\"my.hist_bucket\",le=\"+Inf\"} 2\n{\"my.hist_sum\"} 1\n{\"my.hist_count\"} 2\n")
	server := verboseServer(t, false, passthrough("values", transform.NameEscapingValues, ""))
	server.logger = testutil.QuietLogger(t)
	probe := func(accept string) string {
		r := probeOnce(t, server, "/probe?collector=values&target="+url.QueryEscape(target.URL), http.Header{"Accept": {accept}})
		if r.Code != http.StatusOK {
			t.Fatalf("%d %s", r.Code, r.Body.String())
		}
		return r.Body.String()
	}
	want := "# TYPE U__my_2e_requests_ counter\nU__my_2e_requests__total 3\n# TYPE U__my_2e_errors counter\nU__my_2e_errors_total 1\n# TYPE U__my_2e counter\nU__my_2e_total 2\n# TYPE plain counter\nplain_total 4\n" +
		"# TYPE U__my_2e_hist histogram\nU__my_2e_hist_bucket{le=\"+Inf\"} 2\nU__my_2e_hist_sum 1\nU__my_2e_hist_count 2\n# EOF\n"
	answer := probe("application/openmetrics-text")
	if answer != want {
		t.Fatalf("got:\n%s\nwant:\n%s", answer, want)
	}
	if err := strictOpenMetricsError(answer); err != nil {
		t.Fatalf("a strict parser refuses the answer: %v", err)
	}
	var read []string
	for _, m := range decodedAnswer(t, answer, true) {
		read = append(read, string(m.Type)+" "+m.Name)
	}
	if want := []string{"counter U__my_2e_requests__total", "counter U__my_2e_errors_total", "counter U__my_2e_total", "counter plain_total", "histogram U__my_2e_hist"}; !slices.Equal(read, want) {
		t.Fatalf("the exporter's own reader found %q, want %q", read, want)
	}
	wantText := "# TYPE U__my_2e_requests__total counter\nU__my_2e_requests__total 3\n# TYPE U__my_2e_errors counter\nU__my_2e_errors 1\n# TYPE U__my_2e_total counter\nU__my_2e_total 2\n# TYPE plain_total counter\nplain_total 4\n" +
		"# TYPE U__my_2e_hist histogram\nU__my_2e_hist_bucket{le=\"+Inf\"} 2\nU__my_2e_hist_sum 1\nU__my_2e_hist_count 2\n"
	if got := probe("text/plain"); got != wantText {
		t.Fatalf("the text format answers\n%s\nwant\n%s", got, wantText)
	}
}
